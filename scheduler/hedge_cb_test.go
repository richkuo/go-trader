package main

import "testing"

// hedgeCBStrategy builds a live HL perps strategy on `primaryCoin` with a hedge
// on `hedgeSym`.
func hedgeCBStrategy(id, primaryCoin, hedgeSym string) StrategyConfig {
	return StrategyConfig{
		ID: id, Platform: "hyperliquid", Type: "perps", Direction: "long",
		Leverage: 5, MarginMode: "isolated",
		Args:  []string{"triple_ema", primaryCoin, "1h", "--mode=live"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: hedgeSym, Ratio: 1.0},
	}
}

func hedgeCBState(id, primaryCoin, hedgeSym string) *StrategyState {
	return &StrategyState{
		ID: id, Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: newRiskState(todayUTC(), 0),
		Positions: map[string]*Position{
			primaryCoin: {Symbol: primaryCoin, Quantity: 0.2, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 5},
			hedgeSym:    {Symbol: hedgeSym, Quantity: 0.01, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: primaryCoin, HedgePrimaryQtyBasis: 0.2},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
}

// Finding #1: a latched CB on a SOLE-OWNED-primary hedger enqueues BOTH the
// primary and the hedge, so both flatten and stay flat.
func TestCBPendingSoleOwnedPrimaryEnqueuesHedge(t *testing.T) {
	sc := hedgeCBStrategy("hl-eth", "ETH", "BTC")
	s := hedgeCBState("hl-eth", "ETH", "BTC")
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 3000}, {Coin: "BTC", Size: -0.01, EntryPrice: 60000}},
		HLLiveAll:   []StrategyConfig{sc}, // sole owner of ETH
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("expected a pending circuit close for a sole-owned-primary hedger")
	}
	coins := map[string]bool{}
	for _, c := range p.Symbols {
		coins[c.Symbol] = true
	}
	if !coins["ETH"] || !coins["BTC"] {
		t.Errorf("expected both ETH (primary) and BTC (hedge) in pending; got %+v", p.Symbols)
	}
}

// Finding #1: a latched CB on a SHARED-primary hedger enqueues NOTHING — the
// hedge must NOT be flattened while the primary it offsets stays open (else the
// latched-CB manage-only runHedgeSync re-opens it, churning fees and leaving a
// drawdown-time unhedged window).
func TestCBPendingSharedPrimaryDoesNotEnqueueHedge(t *testing.T) {
	sc := hedgeCBStrategy("hl-eth", "ETH", "BTC")
	peer := StrategyConfig{ID: "hl-eth2", Platform: "hyperliquid", Type: "perps",
		Args: []string{"rsi_macd", "ETH", "1h", "--mode=live"}}
	s := hedgeCBState("hl-eth", "ETH", "BTC")
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 0.4, EntryPrice: 3000}, {Coin: "BTC", Size: -0.01, EntryPrice: 60000}},
		HLLiveAll:   []StrategyConfig{sc, peer}, // ETH shared with a peer
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	if p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
		t.Fatalf("shared-primary hedger must enqueue no CB close (hedge stays to mirror the open primary); got %+v", p.Symbols)
	}
}
