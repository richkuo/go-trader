package main

import (
	"strings"
	"testing"
)

func TestHyperliquidHedgeConfigErrors_CollisionMatrix(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"}, Direction: "long",
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1}},
		{ID: "b", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "BTC", "1h"}, Direction: "long"},
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if len(errs) == 0 {
		t.Fatal("expected collision with configured BTC")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "BTC") {
		t.Fatalf("errs=%v", errs)
	}
}

func TestHyperliquidHedgeConfigErrors_RejectBothDirection(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"}, Direction: DirectionBoth,
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if len(errs) == 0 {
		t.Fatal("expected direction=both reject")
	}
}

func TestHyperliquidHedgeConfigErrors_HedgeHedgeCollision(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"}, Direction: "long",
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
		{ID: "b", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "SOL", "1h"}, Direction: "long",
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if len(errs) == 0 {
		t.Fatal("expected hedge-hedge collision")
	}
}

func TestHedgeCoinNormalizesCCXT(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Symbol: "btc/USDC:USDC"}}
	if got := hedgeCoin(sc); got != "BTC" {
		t.Fatalf("got %q", got)
	}
}

func TestHedgeUnknownKeyRejected(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"a","hedge":{"enabled":true,"symbol":"BTC","ration":1}}]}`)
	errs := validateStrategyJSONKeys(raw)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "ration") {
		t.Fatalf("expected unknown ration, got %v", errs)
	}
}
