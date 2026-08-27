package main

import "fmt"

type ProtectionFillAlert struct {
	StrategyID      string
	Symbol          string
	Side            string
	FillType        string
	IsPartial       bool
	FillPrice       float64
	CloseQty        float64
	RemainingQty    float64
	RealizedPnL     float64
	HasPnL          bool
	ExchangeOrderID string
}

func formatProtectionFillAlert(a ProtectionFillAlert) string {
	headline := fmt.Sprintf("%s filled", a.FillType)
	if a.ExchangeOrderID != "" {
		headline += fmt.Sprintf(" (oid=%s)", a.ExchangeOrderID)
	}
	headline += fmt.Sprintf(" — %s", a.StrategyID)
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

func formatSignedUSD(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

type ownerDMSender interface {
	SendOwnerDM(content string)
}

func notifyProtectionFill(sender ownerDMSender, enabled bool, alert ProtectionFillAlert) {
	if !enabled || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatProtectionFillAlert(alert))
}

func isNilSender(s ownerDMSender) bool {
	if mn, ok := s.(*MultiNotifier); ok {
		return mn == nil
	}
	return false
}

func tpTierLabel(tierIdx int) string {
	return fmt.Sprintf("TP%d", tierIdx+1)
}

func lastBookedTradePnL(s *StrategyState) float64 {
	if s == nil || len(s.TradeHistory) == 0 {
		return 0
	}

	return tradeNetPnL(s.TradeHistory[len(s.TradeHistory)-1])
}
