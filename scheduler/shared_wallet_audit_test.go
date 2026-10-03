package main

import (
	"math"
	"testing"
	"time"
)

func TestPlanTradeLedgerForStrategy_MigrationOnlyPreservesNetSum(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "sell", Quantity: 0.1,
			Price: 61000, Value: 6100, IsClose: true, RealizedPnL: 95,
			ExchangeOrderID: "200"},
	}

	plan := planTradeLedgerForStrategyWithOIDTotals("hl-x", trades, map[string]HLFillSummary{}, 1000, 0, nil)

	if plan.MigratedCount != 1 || plan.MatchedCount != 0 {
		t.Fatalf("migrated=%d matched=%d, want 1/0 (no fills to true-up)", plan.MigratedCount, plan.MatchedCount)
	}
	if plan.UnmatchedOIDCount != 1 {
		t.Fatalf("UnmatchedOIDCount = %d, want 1", plan.UnmatchedOIDCount)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("want exactly 1 change, got %d", len(plan.Changes))
	}
	c := plan.Changes[0]
	modeledFee := 6100 * HyperliquidTakerFeePct
	if c.WasGross {
		t.Fatalf("WasGross = %v, want false", c.WasGross)
	}
	if math.Abs(c.NewFee-modeledFee) > 1e-9 {
		t.Errorf("NewFee = %v, want modeled %v", c.NewFee, modeledFee)
	}
	if math.Abs((c.NewPnL-c.NewFee)-95) > 1e-9 {
		t.Errorf("net effect NewPnL-NewFee = %v, want 95 (migration must not move money)", c.NewPnL-c.NewFee)
	}
	if math.Abs(plan.NewCash-1095) > 1e-9 {
		t.Errorf("NewCash = %v, want 1095 (initial 1000 + net 95, identical to legacy replay)", plan.NewCash)
	}
	if math.Abs(plan.ReplayedCash-plan.NewCash) > 1e-9 {
		t.Errorf("pre-migration replay %v != post-migration cash %v; migration moved money", plan.ReplayedCash, plan.NewCash)
	}
}

func TestHyperliquidKillSwitchFillShare(t *testing.T) {
	t.Run("non-peer and zero-virtual-qty peer fail closed", func(t *testing.T) {
		peers := []StrategyConfig{
			{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"chk", "BTC", "--mode=live"}},
			{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"chk", "BTC", "--mode=live"}},
		}
		vq := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 1.0, "hl-b": 1.0}}

		outsider := StrategyConfig{ID: "hl-c", Platform: "hyperliquid", Type: "perps",
			Args: []string{"chk", "ETH", "--mode=live"}}
		if sz, fee := hyperliquidKillSwitchFillShare(outsider, "BTC", 2.0, 0.2, peers, vq); sz != 0 || fee != 0 {
			t.Fatalf("non-peer sc must fail closed to (0,0), got (%v,%v)", sz, fee)
		}

		vqZero := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 0.0, "hl-b": 1.0}}
		if sz, fee := hyperliquidKillSwitchFillShare(peers[0], "BTC", 2.0, 0.2, peers, vqZero); sz != 0 || fee != 0 {
			t.Fatalf("zero-virtual-qty peer must fail closed to (0,0), got (%v,%v)", sz, fee)
		}
	})
	t.Run("single peer gets the full fill", func(t *testing.T) {
		sc := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"chk", "BTC", "--mode=live"}}
		sz, fee := hyperliquidKillSwitchFillShare(sc, "BTC", 1.5, 0.3,
			[]StrategyConfig{sc}, hlVirtualQuantitySnapshot{})
		if sz != 1.5 || fee != 0.3 {
			t.Fatalf("single peer must receive the full fill unchanged, got (%v,%v)", sz, fee)
		}
	})
	t.Run("uneven split sums to the fill totals", func(t *testing.T) {
		a := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"chk", "BTC", "--mode=live"}}
		b := StrategyConfig{ID: "hl-b", Platform: "hyperliquid", Type: "perps",
			Args: []string{"chk", "BTC", "--mode=live"}}
		peers := []StrategyConfig{a, b}
		vq := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 1.0, "hl-b": 2.0}}

		szA, feeA := hyperliquidKillSwitchFillShare(a, "BTC", 1.0, 0.1, peers, vq)
		szB, feeB := hyperliquidKillSwitchFillShare(b, "BTC", 1.0, 0.1, peers, vq)

		if math.Abs(szA-1.0/3.0) > 1e-9 || math.Abs(szB-2.0/3.0) > 1e-9 {
			t.Errorf("size shares = %v/%v, want 1/3 and 2/3", szA, szB)
		}
		if math.Abs(feeA-0.1/3.0) > 1e-9 || math.Abs(feeB-0.2/3.0) > 1e-9 {
			t.Errorf("fee shares = %v/%v, want 0.1/3 and 0.2/3", feeA, feeB)
		}
		if math.Abs((szA+szB)-1.0) > 1e-9 || math.Abs((feeA+feeB)-0.1) > 1e-9 {
			t.Errorf("shares must sum to the fill totals: sz %v fee %v", szA+szB, feeA+feeB)
		}
	})
}
