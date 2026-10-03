package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEffectiveMarginPerTradeUSDOmittedReturnsZero(t *testing.T) {
	sc := StrategyConfig{Type: "perps", Leverage: 20, SizingLeverage: 1}
	if got := EffectiveMarginPerTradeUSD(sc); got != 0 {
		t.Errorf("EffectiveMarginPerTradeUSD(omitted) = %g, want 0", got)
	}
	if got := ComputePerpsOpenNotional(sc, 1000); got != 1000 {
		t.Errorf("ComputePerpsOpenNotional with omitted margin_per_trade_usd = %g, want 1000 (cash × sizing_leverage)", got)
	}
}
