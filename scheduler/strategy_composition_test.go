package main

import (
	"reflect"
	"testing"
)

func TestEffectiveOpenStrategy(t *testing.T) {
	tests := []struct {
		name     string
		sc       StrategyConfig
		wantOpen string
	}{
		{
			name:     "legacy defaults to positional strategy",
			sc:       StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			wantOpen: "sma_crossover",
		},
		{
			name:     "open override wins over positional",
			sc:       StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}, OpenStrategy: "momentum"},
			wantOpen: "momentum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveOpenStrategy(tt.sc); got != tt.wantOpen {
				t.Fatalf("effectiveOpenStrategy() = %q, want %q", got, tt.wantOpen)
			}
		})
	}
}

func TestAppendOpenCloseArgsOnlyWhenOptedIn(t *testing.T) {
	legacy := StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}}
	if got := appendOpenCloseArgs(legacy.Args, legacy, PositionCtx{Side: "long"}); !reflect.DeepEqual(got, legacy.Args) {
		t.Fatalf("legacy args mutated: %#v", got)
	}

	openOnly := StrategyConfig{
		Args:         []string{"sma_crossover", "BTC/USDT", "1h"},
		OpenStrategy: "momentum",
	}
	gotOpenOnly := appendOpenCloseArgs(openOnly.Args, openOnly, PositionCtx{})
	wantOpenOnly := []string{
		"sma_crossover", "BTC/USDT", "1h",
		"--open-strategy", "momentum",
	}
	if !reflect.DeepEqual(gotOpenOnly, wantOpenOnly) {
		t.Fatalf("appendOpenCloseArgs(open only) = %#v, want %#v", gotOpenOnly, wantOpenOnly)
	}

	sc := StrategyConfig{
		Args:                 []string{"sma_crossover", "BTC/USDT", "1h"},
		OpenStrategy:         "momentum",
		CloseStrategies:      []string{"rsi", "macd"},
		DisableImplicitClose: true,
	}
	got := appendOpenCloseArgs(sc.Args, sc, PositionCtx{Side: "long"})
	want := []string{
		"sma_crossover", "BTC/USDT", "1h",
		"--open-strategy", "momentum",
		"--close-strategies", "rsi,macd",
		"--disable-implicit-close",
		"--position-side", "long",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendOpenCloseArgs() = %#v, want %#v", got, want)
	}
}

func TestAppendOpenCloseArgsPositionCtx(t *testing.T) {
	sc := StrategyConfig{
		Args:            []string{"triple_ema", "ETH", "1h"},
		CloseStrategies: []string{"tp_at_pct"},
	}
	tests := []struct {
		name string
		pos  PositionCtx
		want []string
	}{
		{
			name: "flat omits position context",
			pos:  PositionCtx{},
			want: []string{"triple_ema", "ETH", "1h", "--close-strategies", "tp_at_pct"},
		},
		{
			name: "partial position includes current and initial qty",
			pos: PositionCtx{
				Side:            "long",
				AvgCost:         3000,
				Quantity:        0.25,
				InitialQuantity: 1,
			},
			want: []string{
				"triple_ema", "ETH", "1h",
				"--close-strategies", "tp_at_pct",
				"--position-side", "long",
				"--position-avg-cost=3000",
				"--position-qty=0.25",
				"--position-initial-qty=1",
			},
		},
		{
			name: "full position with atr",
			pos: PositionCtx{
				Side:            "short",
				AvgCost:         51000.5,
				Quantity:        0.4,
				InitialQuantity: 0.4,
				EntryATR:        750.25,
			},
			want: []string{
				"triple_ema", "ETH", "1h",
				"--close-strategies", "tp_at_pct",
				"--position-side", "short",
				"--position-avg-cost=51000.5",
				"--position-qty=0.4",
				"--position-initial-qty=0.4",
				"--position-entry-atr=750.25",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendOpenCloseArgs(sc.Args, sc, tt.pos); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("appendOpenCloseArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPositionCtxForSymbol(t *testing.T) {
	s := &StrategyState{Positions: map[string]*Position{
		"ETH": {
			Symbol:          "ETH",
			Side:            "long",
			Quantity:        0.5,
			InitialQuantity: 1,
			AvgCost:         3000,
			EntryATR:        125,
		},
	}}
	got := positionCtxForSymbol(s, "ETH")
	want := PositionCtx{Side: "long", AvgCost: 3000, Quantity: 0.5, InitialQuantity: 1, EntryATR: 125}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("positionCtxForSymbol() = %#v, want %#v", got, want)
	}
	if got := positionCtxForSymbol(s, "BTC"); !reflect.DeepEqual(got, PositionCtx{}) {
		t.Fatalf("missing symbol ctx = %#v, want zero", got)
	}
}

func TestMaxCloseFraction(t *testing.T) {
	got := maxCloseFraction([]float64{0.25, 0.8, 1.2, -1})
	if got != 1 {
		t.Fatalf("maxCloseFraction() = %g, want 1", got)
	}
}

func TestComposeOpenCloseSignal(t *testing.T) {
	tests := []struct {
		name          string
		openAction    string
		closeFraction float64
		positionSide  string
		want          int
	}{
		{"flat opens long", "long", 0, "", 1},
		{"flat opens short", "short", 0, "", -1},
		{"long close wins before open", "short", 1, "long", -1},
		{"short close wins before open", "long", 1, "short", 1},
		{"flat close suppresses open", "long", 1, "", 0},
		{"long ignores opposite open without close", "short", 0, "long", 0},
		{"short ignores same-side hold without close", "short", 0, "short", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeOpenCloseSignal(tt.openAction, tt.closeFraction, tt.positionSide); got != tt.want {
				t.Fatalf("composeOpenCloseSignal() = %d, want %d", got, tt.want)
			}
		})
	}
}
