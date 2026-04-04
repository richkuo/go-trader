package main

import (
	"testing"
)

func TestSyncHyperliquidLiveCapitalSkipsNonHL(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "spot-btc",
		Platform: "binanceus",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change for non-hyperliquid, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalSkipsPaper(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change for paper mode, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalSkipsNoMode(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change without --mode=live, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalNoAddress(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	// Should fall back to config capital when no address
	if sc.Capital != original {
		t.Errorf("capital should not change without account address, got %g", sc.Capital)
	}
}
