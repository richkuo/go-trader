package main

import (
	"testing"
)

func TestResolveCapitalPct_NoCapitalPct(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "test-1", Capital: 1000, Platform: "binanceus"},
		{ID: "test-2", Capital: 2000, Platform: "hyperliquid"},
	}
	resolveCapitalPct(strategies)
	if strategies[0].Capital != 1000 {
		t.Errorf("expected capital=1000, got %g", strategies[0].Capital)
	}
	if strategies[1].Capital != 2000 {
		t.Errorf("expected capital=2000, got %g", strategies[1].Capital)
	}
}

func TestResolveCapitalPct_FallbackCapital(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "test-pct", Capital: 500, CapitalPct: 0.5, Platform: "binanceus"},
	}
	resolveCapitalPct(strategies)
	if strategies[0].Capital != 500 {
		t.Errorf("expected fallback capital=500, got %g", strategies[0].Capital)
	}
}

func TestResolveCapitalPct_NoFallbackCapital(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "test-no-fallback", Capital: 0, CapitalPct: 0.5, Platform: "binanceus"},
	}
	resolveCapitalPct(strategies)
	if strategies[0].Capital != 0 {
		t.Errorf("expected capital=0 (no fallback), got %g", strategies[0].Capital)
	}
}
