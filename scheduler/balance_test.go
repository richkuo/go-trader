package main

import (
	"testing"
)

func TestResolveCapitalPct(t *testing.T) {
	cases := []struct {
		name string
		in   StrategyConfig
		want float64
	}{
		{"no capital pct keeps capital", StrategyConfig{ID: "test-1", Capital: 1000, Platform: "binanceus"}, 1000},
		{"no capital pct keeps capital hl", StrategyConfig{ID: "test-2", Capital: 2000, Platform: "hyperliquid"}, 2000},
		{"capital pct falls back to capital", StrategyConfig{ID: "test-pct", Capital: 500, CapitalPct: 0.5, Platform: "binanceus"}, 500},
		{"capital pct with no capital stays zero", StrategyConfig{ID: "test-no-fallback", Capital: 0, CapitalPct: 0.5, Platform: "binanceus"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strategies := []StrategyConfig{tc.in}
			resolveCapitalPct(strategies)
			if strategies[0].Capital != tc.want {
				t.Errorf("expected capital=%g, got %g", tc.want, strategies[0].Capital)
			}
		})
	}
}
