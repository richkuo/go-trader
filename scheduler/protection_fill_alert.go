package main

import "fmt"

// ProtectionFillAlert describes a TP or SL fill detected by the HL reconciler.
// Emission sites build this struct from in-scope state and pass it to
// notifyProtectionFill, which formats and DMs the owner.
type ProtectionFillAlert struct {
	StrategyID   string
	Symbol       string
	Side         string  // "long" or "short" — the position side, not the trade direction
	FillType     string  // "TP1", "TP2", …, or "SL"
	IsPartial    bool    // partial close (TP tier or partial SL) vs. full close
	FillPrice    float64 // actual fill price; 0 if unknown
	CloseQty     float64 // qty closed by this fill
	RemainingQty float64 // remaining position qty after this fill
	RealizedPnL  float64 // 0 when not computable
	HasPnL       bool    // distinguishes "PnL=0" from "PnL unknown"
}

// formatProtectionFillAlert produces the DM body for a TP/SL fill notification.
// Pure function so it's testable without spinning a notifier.
func formatProtectionFillAlert(a ProtectionFillAlert) string {
	headline := fmt.Sprintf("%s filled — %s", a.FillType, a.StrategyID)
	if a.IsPartial {
		headline += " (partial)"
	}
	side := "LONG"
	if a.Side == "short" {
		side = "SHORT"
	}
	priceLine := ""
	if a.FillPrice > 0 {
		priceLine = fmt.Sprintf("%s %s — %.6f @ $%.4f", a.Symbol, side, a.CloseQty, a.FillPrice)
	} else {
		priceLine = fmt.Sprintf("%s %s — %.6f (fill price unknown)", a.Symbol, side, a.CloseQty)
	}
	remaining := fmt.Sprintf("Remaining: %.6f %s", a.RemainingQty, a.Symbol)
	if a.HasPnL {
		return fmt.Sprintf("%s\n%s\n%s | PnL=%s", headline, priceLine, remaining, formatSignedUSD(a.RealizedPnL))
	}
	return fmt.Sprintf("%s\n%s\n%s", headline, priceLine, remaining)
}

// formatSignedUSD formats a USD amount with the sign before the currency symbol
// so negative values read as "-$42.10" instead of "$-42.10".
func formatSignedUSD(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// ownerDMSender is the minimal interface notifyProtectionFill needs from a
// notifier. *MultiNotifier satisfies it; tests can stub with a counting fake
// to assert the disabled-flag actually suppresses emission.
type ownerDMSender interface {
	SendOwnerDM(content string)
}

// notifyProtectionFill emits an owner DM for a protection-order fill detected
// by the reconciler. No-ops when sender is a nil interface, when the underlying
// pointer is nil, or when the feature is disabled.
func notifyProtectionFill(sender ownerDMSender, enabled bool, alert ProtectionFillAlert) {
	if !enabled || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatProtectionFillAlert(alert))
}

// isNilSender reports whether a non-nil interface value carries a nil
// underlying pointer (e.g. (*MultiNotifier)(nil)). Without this check the
// interface compares != nil even though invoking SendOwnerDM would panic.
func isNilSender(s ownerDMSender) bool {
	if mn, ok := s.(*MultiNotifier); ok {
		return mn == nil
	}
	return false
}

// tpTierLabel formats a 0-based tier index as "TP1", "TP2", … for DM display.
func tpTierLabel(tierIdx int) string {
	return fmt.Sprintf("TP%d", tierIdx+1)
}

// lastBookedTradePnL returns the realized PnL of the most recently recorded
// trade on s, or 0 when TradeHistory is empty. Used by the reconciler hook
// sites to surface the per-fill PnL in the DM alert without changing the
// signature of the bookPerps* helpers.
//
// IMPORTANT: this assumes the booker just appended via RecordTrade and no
// other RecordTrade has run between the booker and this read. Each call site
// MUST live immediately after a successful record* call, with no intervening
// trade-recording in between — otherwise the DM will silently mis-report PnL.
func lastBookedTradePnL(s *StrategyState) float64 {
	if s == nil || len(s.TradeHistory) == 0 {
		return 0
	}
	return s.TradeHistory[len(s.TradeHistory)-1].RealizedPnL
}
