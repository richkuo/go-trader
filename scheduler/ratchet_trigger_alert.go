package main

import (
	"fmt"
	"strings"
)

type RatchetTriggerAlert struct {
	StrategyID string
	Symbol     string
	Side       string

	TierIdx         int
	TotalTiers      int
	TierATRMultiple float64
	TierTriggerPx   float64

	MarkPrice   float64
	AnchorPrice float64
	EntryATR    float64

	ProfitATR float64
	ProfitUSD float64

	OldTrailMult float64
	NewTrailMult float64

	HighWaterMark       float64
	IntendedSLTriggerPx float64

	HasNextTier         bool
	NextTierATRMultiple float64
	NextTierTrailAfter  float64
	NextTierTriggerPx   float64

	RegimeLabel          string
	PositionRegimeAtOpen string
}

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

func hwmTrailSign(side string) string {
	if side == "short" {
		return "+"
	}
	return "-"
}

func notifyRatchetTrigger(sender ownerDMSender, enabled bool, alert *RatchetTriggerAlert) {
	if !enabled || alert == nil || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatRatchetTriggerAlert(*alert))
}
