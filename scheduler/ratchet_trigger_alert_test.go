package main

import (
	"testing"
)

func ratchetAlertSC(initialTrail float64, tiers ...map[string]interface{}) StrategyConfig {
	items := make([]interface{}, len(tiers))
	for i, t := range tiers {
		items[i] = t
	}
	return StrategyConfig{
		ID:       "hl-rmc-eth-live",
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategy: &StrategyRef{
			Name:   "trailing_tp_ratchet",
			Params: map[string]interface{}{"tp_tiers": items},
		},
		TrailingStopATRMult: &initialTrail,
	}
}

func tier(mult, frac, after float64) map[string]interface{} {
	return map[string]interface{}{"atr_multiple": mult, "close_fraction": frac, "trailing_mult_after": after}
}

func TestApplyTrailingTPRatchetToPosition_AlertLongMath(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.0))
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging",
	}
	tightened, a := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	if !tightened || a == nil {
		t.Fatalf("expected tighten+alert, got tightened=%v alert=%v", tightened, a)
	}
	if a.TierIdx != 0 || a.TotalTiers != 2 {
		t.Fatalf("tier=%d/%d want 0/2", a.TierIdx, a.TotalTiers)
	}
	if a.TierATRMultiple != 1.0 || a.TierTriggerPx != 110 {
		t.Fatalf("tierATR=%g triggerPx=%g want 1.0,110", a.TierATRMultiple, a.TierTriggerPx)
	}
	if a.MarkPrice != 115 || a.AnchorPrice != 100 || a.EntryATR != 10 {
		t.Fatalf("mark=%g anchor=%g atr=%g", a.MarkPrice, a.AnchorPrice, a.EntryATR)
	}
	if a.ProfitATR != 1.5 || a.ProfitUSD != 30 {
		t.Fatalf("profitATR=%g profitUSD=%g want 1.5,30", a.ProfitATR, a.ProfitUSD)
	}
	if a.OldTrailMult != 3.0 || a.NewTrailMult != 2.0 {
		t.Fatalf("trail %g->%g want 3->2", a.OldTrailMult, a.NewTrailMult)
	}
	if a.HighWaterMark != 115 || a.IntendedSLTriggerPx != 95 {
		t.Fatalf("hwm=%g intendedSL=%g want 115,95", a.HighWaterMark, a.IntendedSLTriggerPx)
	}
	if !a.HasNextTier || a.NextTierATRMultiple != 2.0 || a.NextTierTrailAfter != 1.0 || a.NextTierTriggerPx != 120 {
		t.Fatalf("next tier mismatch: %+v", a)
	}
}

func TestApplyTrailingTPRatchetToPosition_AlertShortMath(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.0))
	pos := &Position{
		Symbol: "ETH", Side: "short", Quantity: 1, InitialQuantity: 1,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging",
	}
	tightened, a := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 85, nil)
	if !tightened || a == nil {
		t.Fatalf("expected tighten+alert, got tightened=%v alert=%v", tightened, a)
	}
	if a.TierTriggerPx != 90 {
		t.Fatalf("triggerPx=%g want 90", a.TierTriggerPx)
	}
	if a.ProfitATR != 1.5 || a.ProfitUSD != 15 {
		t.Fatalf("profitATR=%g profitUSD=%g want 1.5,15", a.ProfitATR, a.ProfitUSD)
	}
	if a.HighWaterMark != 85 || a.IntendedSLTriggerPx != 105 {
		t.Fatalf("hwm=%g intendedSL=%g want 85,105", a.HighWaterMark, a.IntendedSLTriggerPx)
	}
	if a.NextTierTriggerPx != 80 {
		t.Fatalf("nextTriggerPx=%g want 80", a.NextTierTriggerPx)
	}
}
