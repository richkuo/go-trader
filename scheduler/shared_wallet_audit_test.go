package main

import (
	"math"
	"testing"
)

func TestHyperliquidKillSwitchFillShare(t *testing.T) {
	t.Run("ProportionalSplit", func(t *testing.T) {
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
	})
	t.Run("FailsClosedWhenNotAPeer", func(t *testing.T) {
		hlLive := []StrategyConfig{
			{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
				Args: []string{"sma", "ETH", "1h", "--mode=live"}},
			{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5,
				Args: []string{"ema", "ETH", "1h", "--mode=live"}},
		}
		virtualQty := hlVirtualQuantitySnapshot{
			"ETH": {"hl-a": 1.5, "hl-b": 0.5},
		}

		t.Run("non-peer strategy gets zero share", func(t *testing.T) {
			outsider := StrategyConfig{ID: "hl-c", Platform: "hyperliquid", Type: "perps",
				Args: []string{"rsi", "ETH", "1h", "--mode=live"}}
			sz, fee := hyperliquidKillSwitchFillShare(outsider, "ETH", 2.0, 4.0, hlLive, virtualQty)
			if sz != 0 || fee != 0 {
				t.Errorf("fill share for non-peer = (%v, %v); want (0, 0) fail-closed", sz, fee)
			}
		})

		t.Run("peer with zero virtual quantity gets zero share", func(t *testing.T) {
			zeroQty := hlVirtualQuantitySnapshot{
				"ETH": {"hl-a": 0, "hl-b": 0.5},
			}
			sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, zeroQty)
			if sz != 0 || fee != 0 {
				t.Errorf("fill share for zero-qty peer = (%v, %v); want (0, 0) fail-closed", sz, fee)
			}
		})

		t.Run("all peers zero quantity gets zero share", func(t *testing.T) {
			allZero := hlVirtualQuantitySnapshot{
				"ETH": {"hl-a": 0, "hl-b": 0},
			}
			sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, allZero)
			if sz != 0 || fee != 0 {
				t.Errorf("fill share with all-zero peers = (%v, %v); want (0, 0) fail-closed", sz, fee)
			}
		})

		t.Run("sole peer receives full fill", func(t *testing.T) {
			solo := hlLive[:1]
			sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, solo, virtualQty)
			if sz != 2.0 || fee != 4.0 {
				t.Errorf("sole-peer fill share = (%v, %v); want full (2.0, 4.0)", sz, fee)
			}
		})

		t.Run("peer shares sum to the reported fill", func(t *testing.T) {
			szA, feeA := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, virtualQty)
			szB, feeB := hyperliquidKillSwitchFillShare(hlLive[1], "ETH", 2.0, 4.0, hlLive, virtualQty)
			if math.Abs(szA+szB-2.0) > 1e-9 || math.Abs(feeA+feeB-4.0) > 1e-9 {
				t.Errorf("peer shares sum to (%v, %v); want (2.0, 4.0)", szA+szB, feeA+feeB)
			}
		})
	})
	t.Run("PerpsAndManualPeers", func(t *testing.T) {
		roster := []StrategyConfig{
			{ID: "hl-perps-eth-live", Platform: "hyperliquid", Type: "perps",
				Args: []string{"sma", "ETH", "1h", "--mode=live"}},
			{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
				Args: []string{"hold", "ETH", "1h", "--mode=live"}},
		}
		states := map[string]*StrategyState{
			"hl-perps-eth-live": {ID: "hl-perps-eth-live", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1.5, Side: "long"},
			}},
			"hl-manual-eth-live": {ID: "hl-manual-eth-live", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.5, Side: "long"},
			}},
		}
		snap := snapshotHyperliquidVirtualQuantities(states, roster)
		if snap == nil || snap["ETH"]["hl-perps-eth-live"] != 1.5 || snap["ETH"]["hl-manual-eth-live"] != 0.5 {
			t.Fatalf("virtual snapshot must include BOTH peers' quantities, got %+v", snap)
		}

		szPerps, feePerps := hyperliquidKillSwitchFillShare(roster[0], "ETH", 2.0, 10.0, roster, snap)
		if math.Abs(szPerps-1.5) > 1e-9 || math.Abs(feePerps-7.5) > 1e-9 {
			t.Errorf("perps share = (%.6f, %.6f); want (1.5, 7.5)", szPerps, feePerps)
		}
		szMann, feeMan := hyperliquidKillSwitchFillShare(roster[1], "ETH", 2.0, 10.0, roster, snap)
		if math.Abs(szMann-0.5) > 1e-9 || math.Abs(feeMan-2.5) > 1e-9 {
			t.Errorf("manual share = (%.6f, %.6f); want (0.5, 2.5)", szMann, feeMan)
		}
	})
}
