package main

import (
	"strings"
	"sync"
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

func TestApplyTrailingTPRatchetToPosition_MultiTierJump(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.5), tier(3.0, 0, 1.0))
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging",
	}
	tightened, a := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 125, nil)
	if !tightened || a == nil {
		t.Fatal("expected tighten+alert on multi-tier jump")
	}
	if a.TierIdx != 1 || a.NewTrailMult != 1.5 {
		t.Fatalf("cleared tier=%d trail=%g want 1,1.5", a.TierIdx, a.NewTrailMult)
	}
	if pos.SLAdjustedTiersProcessed != 2 {
		t.Fatalf("watermark=%d want 2 (jumped past tier0)", pos.SLAdjustedTiersProcessed)
	}
	if !a.HasNextTier || a.NextTierATRMultiple != 3.0 || a.NextTierTrailAfter != 1.0 {
		t.Fatalf("next tier should be the 3.0 rung, got %+v", a)
	}
}

func TestApplyTrailingTPRatchetToPosition_NoAlertCases(t *testing.T) {

	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.0))
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging",
	}
	if tightened, _ := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil); !tightened {
		t.Fatal("setup: first clear should tighten")
	}
	if tightened, a := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil); tightened || a != nil {
		t.Fatalf("re-run on processed tier should not alert, got tightened=%v alert=%v", tightened, a)
	}

	scEqual := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 2.0))
	pos2 := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging",
	}
	if tightened, _ := applyTrailingTPRatchetToPosition(scEqual, pos2, "ETH", 110, nil); !tightened {
		t.Fatal("setup: tier0 should tighten 3.0->2.0")
	}
	tightened, a := applyTrailingTPRatchetToPosition(scEqual, pos2, "ETH", 120, nil)
	if tightened || a != nil {
		t.Fatalf("equal-trail tier should advance watermark without alert, got tightened=%v alert=%v", tightened, a)
	}
	if pos2.SLAdjustedTiersProcessed != 2 {
		t.Fatalf("watermark=%d want 2 (advanced even without tighten)", pos2.SLAdjustedTiersProcessed)
	}
}

func TestNotifyRatchetTrigger_Gating(t *testing.T) {
	alert := &RatchetTriggerAlert{StrategyID: "x", Symbol: "ETH", Side: "long", TotalTiers: 1}

	notifyRatchetTrigger(nil, true, alert)
	var mn *MultiNotifier
	notifyRatchetTrigger(mn, true, alert)

	c := &countingDMSender{}
	notifyRatchetTrigger(c, false, alert)
	if c.count != 0 {
		t.Fatalf("disabled flag should suppress, count=%d", c.count)
	}

	notifyRatchetTrigger(c, true, nil)
	if c.count != 0 {
		t.Fatalf("nil alert should suppress, count=%d", c.count)
	}

	notifyRatchetTrigger(c, true, alert)
	if c.count != 1 {
		t.Fatalf("enabled+alert should send once, count=%d", c.count)
	}
}

func TestFormatRatchetTriggerAlert_Rendering(t *testing.T) {
	a := RatchetTriggerAlert{
		StrategyID: "hl-rmc-eth-live", Symbol: "ETH", Side: "long",
		TierIdx: 0, TotalTiers: 3, TierATRMultiple: 1.5, TierTriggerPx: 1611.98,
		MarkPrice: 1619.25, AnchorPrice: 1579.50, EntryATR: 21.65,
		ProfitATR: 1.83, ProfitUSD: 34.85,
		OldTrailMult: 2.0, NewTrailMult: 1.5,
		HighWaterMark: 1623.15, IntendedSLTriggerPx: 1590.74,
		HasNextTier: true, NextTierATRMultiple: 2.0, NextTierTrailAfter: 1.25, NextTierTriggerPx: 1622.80,
		RegimeLabel: "trending_down_choppy", PositionRegimeAtOpen: "trending_down_choppy",
	}
	out := formatRatchetTriggerAlert(a)
	for _, want := range []string{
		"[hl-rmc-eth-live] ETH long — Ratchet Tier 1/3 cleared",
		"Triggered at: 1.5×ATR",
		"Trail tightened: 2×ATR → 1.5×ATR",
		"Intended SL trigger:",
		"Next tier: 2×ATR",
		"Regime: trending_down_choppy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	if strings.Contains(out, "stamped at open") {
		t.Errorf("identical regime should not render the stamped-at-open suffix:\n%s", out)
	}
}

func TestFormatRatchetTriggerAlert_DistinctRegimeShowsStamp(t *testing.T) {
	a := RatchetTriggerAlert{
		StrategyID: "s", Symbol: "ETH", Side: "short", TotalTiers: 2,
		RegimeLabel: "ranging_quiet", PositionRegimeAtOpen: "trending_up_clean",
	}
	out := formatRatchetTriggerAlert(a)
	if !strings.Contains(out, "Regime: ranging_quiet (stamped at open: trending_up_clean)") {
		t.Errorf("distinct open regime should be shown:\n%s", out)
	}
}

func TestApplyTrailingTPRatchet_ReturnsSnapshotForDeferredSend(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.0))
	state := &StrategyState{Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 100, EntryATR: 10, Multiplier: 1, Regime: "ranging"},
	}}
	var mu sync.RWMutex
	a := applyTrailingTPRatchet(sc, state, "ETH", 115, &mu, nil)
	if a == nil {
		t.Fatal("wrapper should surface the alert snapshot")
	}
	if pos := state.Positions["ETH"]; pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 2.0 {
		t.Fatalf("position must already be mutated before delivery, got %v", pos.PostTPTrailingATRMult)
	}
}
