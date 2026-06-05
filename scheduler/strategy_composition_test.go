package main

import (
	"encoding/json"
	"reflect"
	"strings"
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
			sc:       StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}, OpenStrategy: StrategyRef{Name: "momentum"}},
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
	// Legacy strategies (no open/close opt-in) get no extra args.
	legacy := StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}}
	if got := appendOpenCloseArgs(legacy.Args, legacy, PositionCtx{Side: "long"}); !reflect.DeepEqual(got, legacy.Args) {
		t.Fatalf("legacy args mutated: %#v", got)
	}

	// #640: open/close names + per-ref params are sent via --strategy-refs JSON
	// (see buildStrategyRefsArg), not as separate --open-strategy / --close-strategies
	// flags. appendOpenCloseArgs only adds position-context flags when they're set.
	sc := StrategyConfig{
		Args:          []string{"sma_crossover", "BTC/USDT", "1h"},
		OpenStrategy:  StrategyRef{Name: "momentum"},
		CloseStrategy: &StrategyRef{Name: "rsi"},
	}
	got := appendOpenCloseArgs(sc.Args, sc, PositionCtx{Side: "long"})
	want := []string{
		"sma_crossover", "BTC/USDT", "1h",
		"--position-side", "long",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendOpenCloseArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildStrategyRefsArg(t *testing.T) {
	// Legacy: no opt-in → no flag emitted.
	legacy := StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}}
	got, err := buildStrategyRefsArg(legacy)
	if err != nil {
		t.Fatalf("legacy: unexpected err: %v", err)
	}
	if got != nil {
		// effectiveOpenStrategy falls back to args[0]; we still emit the open ref.
		// Verify the JSON shape carries only the open name with no params and no closes.
		if len(got) != 2 || got[0] != "--strategy-refs" {
			t.Fatalf("legacy: got %#v, want a single --strategy-refs flag", got)
		}
		if !strings.Contains(got[1], `"open":{"name":"sma_crossover"}`) {
			t.Fatalf("legacy: payload missing open ref: %s", got[1])
		}
	}

	sc := StrategyConfig{
		Args:         []string{"sma_crossover", "BTC/USDT", "1h"},
		OpenStrategy: StrategyRef{Name: "momentum", Params: map[string]interface{}{"rsi_period": 14}},
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
		}}},
	}
	got, err = buildStrategyRefsArg(sc)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 || got[0] != "--strategy-refs" {
		t.Fatalf("got %#v, want --strategy-refs JSON", got)
	}
	// Round-trip the JSON to validate structure.
	var payload struct {
		Open   StrategyRef   `json:"open"`
		Closes []StrategyRef `json:"closes"`
	}
	if err := json.Unmarshal([]byte(got[1]), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v\n%s", err, got[1])
	}
	if payload.Open.Name != "momentum" {
		t.Errorf("open.name = %q, want momentum", payload.Open.Name)
	}
	if got, want := payload.Open.Params["rsi_period"], 14.0; got != want {
		t.Errorf("open.params[rsi_period] = %v, want %v", got, want)
	}
	// #842: the wire carries the single close as a length-1 "closes" list.
	if len(payload.Closes) != 1 {
		t.Fatalf("closes length = %d, want 1 (single close)", len(payload.Closes))
	}
	if payload.Closes[0].Name != "tiered_tp_atr" {
		t.Errorf("closes[0].name = %q, want tiered_tp_atr", payload.Closes[0].Name)
	}
	tiers, ok := payload.Closes[0].Params["tp_tiers"].([]interface{})
	if !ok || len(tiers) != 1 {
		t.Errorf("closes[0].params[tp_tiers] = %v, want length 1", payload.Closes[0].Params["tp_tiers"])
	}
}

func TestAppendOpenCloseArgsPositionCtx(t *testing.T) {
	sc := StrategyConfig{
		Args:          []string{"triple_ema", "ETH", "1h"},
		CloseStrategy: &StrategyRef{Name: "tp_at_pct"},
	}
	tests := []struct {
		name string
		pos  PositionCtx
		want []string
	}{
		{
			name: "flat omits position context",
			pos:  PositionCtx{},
			want: []string{"triple_ema", "ETH", "1h"},
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
	got := positionCtxForSymbol(s, "ETH", StrategyConfig{}, nil)
	want := PositionCtx{Side: "long", AvgCost: 3000, Quantity: 0.5, InitialQuantity: 1, EntryATR: 125}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("positionCtxForSymbol() = %#v, want %#v", got, want)
	}
	if got := positionCtxForSymbol(s, "BTC", StrategyConfig{}, nil); !reflect.DeepEqual(got, PositionCtx{}) {
		t.Fatalf("missing symbol ctx = %#v, want zero", got)
	}
}

func TestAppendRegimeArgs(t *testing.T) {
	base := []string{"sma_crossover", "BTC/USDT", "1h"}

	t.Run("nil regime returns args unchanged", func(t *testing.T) {
		got := appendRegimeArgs(base, nil)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("appendRegimeArgs(nil) = %#v, want %#v", got, base)
		}
	})

	t.Run("disabled regime returns args unchanged", func(t *testing.T) {
		got := appendRegimeArgs(base, &RegimeConfig{Enabled: false})
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("appendRegimeArgs(disabled) = %#v, want %#v", got, base)
		}
	})

	t.Run("enabled regime appends spec json and ohlcv limit", func(t *testing.T) {
		regime := &RegimeConfig{Enabled: true, Period: 28, ADXThreshold: 25.5}
		got := appendRegimeArgs(base, regime)
		if len(got) < 4 || got[len(got)-4] != "--regime-windows-spec-json" {
			t.Fatalf("appendRegimeArgs(enabled) missing spec json: %#v", got)
		}
		specJSON := got[len(got)-3]
		if !strings.Contains(specJSON, `"period":28`) || !strings.Contains(specJSON, `"adx_threshold":25.5`) {
			t.Fatalf("spec json = %s", specJSON)
		}
		if got[len(got)-2] != "--ohlcv-limit" || got[len(got)-1] != "200" {
			t.Fatalf("ohlcv tail = %v", got[len(got)-2:])
		}
	})

	t.Run("enabled with defaults appends default window spec", func(t *testing.T) {
		regime := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
		got := appendRegimeArgs(base, regime)
		specIdx := -1
		for i, a := range got {
			if a == "--regime-windows-spec-json" {
				specIdx = i + 1
				break
			}
		}
		if specIdx < 1 {
			t.Fatalf("missing --regime-windows-spec-json in %#v", got)
		}
		if !strings.Contains(got[specIdx], `"period":14`) {
			t.Fatalf("spec json = %s", got[specIdx])
		}
	})
}

func TestAppendRegimePayloadArg(t *testing.T) {
	base := []string{"sma_crossover", "BTC/USDT", "1h"}
	if got := appendRegimePayloadArg(base, RegimePayload{}); !reflect.DeepEqual(got, base) {
		t.Fatalf("empty payload args = %#v, want unchanged", got)
	}

	payload := RegimePayload{
		MultiMode: true,
		Windows: map[string]RegimeSnapshot{
			"default": {Regime: "trending_up", Score: 0.7},
		},
	}
	got := appendRegimePayloadArg(base, payload)
	if len(got) != len(base)+2 || got[len(base)] != "--regime-payload-json" {
		t.Fatalf("payload args = %#v", got)
	}
	var roundTrip RegimePayload
	if err := json.Unmarshal([]byte(got[len(base)+1]), &roundTrip); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if label := roundTrip.Label("default", &RegimeConfig{Enabled: true}); label != "trending_up" {
		t.Fatalf("round-trip label = %q", label)
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
