package main

import (
	"reflect"
	"testing"
)

func TestEffectiveOpenCloseStrategies(t *testing.T) {
	tests := []struct {
		name      string
		sc        StrategyConfig
		wantOpen  string
		wantClose []string
	}{
		{
			name:      "legacy defaults to positional strategy",
			sc:        StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			wantOpen:  "sma_crossover",
			wantClose: []string{"sma_crossover"},
		},
		{
			name:      "open override defaults implicit close to open strategy",
			sc:        StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}, OpenStrategy: "momentum"},
			wantOpen:  "momentum",
			wantClose: []string{"momentum"},
		},
		{
			name:      "explicit close list wins",
			sc:        StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}, OpenStrategy: "momentum", CloseStrategies: []string{"rsi", "macd"}},
			wantOpen:  "momentum",
			wantClose: []string{"rsi", "macd"},
		},
		{
			name:      "disable implicit close leaves no default close strategies",
			sc:        StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}, DisableImplicitClose: true},
			wantOpen:  "sma_crossover",
			wantClose: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveOpenStrategy(tt.sc); got != tt.wantOpen {
				t.Fatalf("effectiveOpenStrategy() = %q, want %q", got, tt.wantOpen)
			}
			if got := effectiveCloseStrategies(tt.sc); !reflect.DeepEqual(got, tt.wantClose) {
				t.Fatalf("effectiveCloseStrategies() = %#v, want %#v", got, tt.wantClose)
			}
		})
	}
}

func TestAppendOpenCloseArgsOnlyWhenOptedIn(t *testing.T) {
	legacy := StrategyConfig{Args: []string{"sma_crossover", "BTC/USDT", "1h"}}
	if got := appendOpenCloseArgs(legacy.Args, legacy, "long"); !reflect.DeepEqual(got, legacy.Args) {
		t.Fatalf("legacy args mutated: %#v", got)
	}

	openOnly := StrategyConfig{
		Args:         []string{"sma_crossover", "BTC/USDT", "1h"},
		OpenStrategy: "momentum",
	}
	gotOpenOnly := appendOpenCloseArgs(openOnly.Args, openOnly, "")
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
	got := appendOpenCloseArgs(sc.Args, sc, "long")
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
