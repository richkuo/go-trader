package main

import (
	"fmt"
	"strings"
)

// RatchetTriggerAlert is the immutable snapshot of a trailing_tp_ratchet* tier
// clearing and tightening the trail (#1110). It is captured at the instant the
// ratchet state changes — inside the state lock — so the formatted owner DM can
// be delivered AFTER the lock is released. Discord/Telegram HTTP must never run
// while holding mu (see applyTrailingTPRatchet / executeHyperliquidResultDeferredOpen
// callers). Mirrors the SLAdjustmentAlert / ProtectionFillAlert snapshot pattern.
type RatchetTriggerAlert struct {
	StrategyID string
	Symbol     string
	Side       string // "long" or "short" — the position side

	TierIdx         int     // 0-based index of the highest tier cleared this event
	TotalTiers      int     // total rungs in the resolved ladder
	TierATRMultiple float64 // ATR multiple of the cleared tier (the trigger threshold)
	TierTriggerPx   float64 // price level at the cleared tier (anchor ± mult×ATR)

	MarkPrice   float64 // mark that cleared the tier
	AnchorPrice float64 // frozen entry / risk-anchor the tier offsets measure from (#873)
	EntryATR    float64

	ProfitATR float64 // profit distance in ATR multiples at trigger
	ProfitUSD float64 // profit in USD at trigger (qty × distance × multiplier)

	OldTrailMult float64 // trail ATR mult before the tighten
	NewTrailMult float64 // trail ATR mult after the tighten

	HighWaterMark       float64 // HWM used for the intended-SL computation
	IntendedSLTriggerPx float64 // computed SL trigger from HWM + new trail mult; "intended" until the trailing walker confirms the on-chain replacement. 0 when not computable.

	HasNextTier         bool
	NextTierATRMultiple float64
	NextTierTrailAfter  float64
	NextTierTriggerPx   float64

	RegimeLabel          string // label used to resolve trailing_tp_ratchet_regime tiers
	PositionRegimeAtOpen string // pos.Regime stamped at open; shown only when distinct from RegimeLabel
}

// formatRatchetTriggerAlert renders the owner DM body. Pure function so it's
// testable without spinning a notifier. Prices use the codebase $%.4f / signed
// USD conventions (see formatSLAdjustmentAlert / formatSignedUSD).
func formatRatchetTriggerAlert(a RatchetTriggerAlert) string {
	side := "long"
	if strings.ToLower(strings.TrimSpace(a.Side)) == "short" {
		side = "short"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s %s — Ratchet Tier %d/%d cleared\n",
		a.StrategyID, a.Symbol, side, a.TierIdx+1, a.TotalTiers)
	fmt.Fprintf(&b, "  Triggered at: %g×ATR ($%.4f) | Mark: $%.4f\n",
		a.TierATRMultiple, a.TierTriggerPx, a.MarkPrice)
	fmt.Fprintf(&b, "  Entry: $%.4f | ATR: $%.4f | Profit: %.2f×ATR (%s)\n",
		a.AnchorPrice, a.EntryATR, a.ProfitATR, formatSignedUSD(a.ProfitUSD))
	fmt.Fprintf(&b, "  Trail tightened: %g×ATR → %g×ATR\n", a.OldTrailMult, a.NewTrailMult)
	if a.IntendedSLTriggerPx > 0 {
		fmt.Fprintf(&b, "  Intended SL trigger: ~$%.4f (HWM $%.4f %s %g×$%.4f)\n",
			a.IntendedSLTriggerPx, a.HighWaterMark, hwmTrailSign(side), a.NewTrailMult, a.EntryATR)
	}
	if a.HasNextTier {
		fmt.Fprintf(&b, "  Next tier: %g×ATR ($%.4f) → trail tightens to %g×ATR\n",
			a.NextTierATRMultiple, a.NextTierTriggerPx, a.NextTierTrailAfter)
	}
	if regime := strings.TrimSpace(a.RegimeLabel); regime != "" {
		line := fmt.Sprintf("  Regime: %s", regime)
		if posReg := strings.TrimSpace(a.PositionRegimeAtOpen); posReg != "" && posReg != regime {
			line += fmt.Sprintf(" (stamped at open: %s)", posReg)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// hwmTrailSign returns the sign used in the intended-SL formula display: a long
// trails BELOW the high-water mark (HWM − trail×ATR), a short trails ABOVE it.
func hwmTrailSign(side string) string {
	if side == "short" {
		return "+"
	}
	return "-"
}

// notifyRatchetTrigger emits an owner DM for a ratchet tier-clearing event.
// No-ops when the feature is disabled, the alert is nil (no tier tightened the
// trail this cycle), the sender is a nil interface, or the underlying pointer is
// nil. Mirrors notifyProtectionFill / notifySLAdjustment so the default route is
// owner DM only (consistent with the other auto-protective-stop alerts).
func notifyRatchetTrigger(sender ownerDMSender, enabled bool, alert *RatchetTriggerAlert) {
	if !enabled || alert == nil || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatRatchetTriggerAlert(*alert))
}
