package main


import "fmt"

type PerpsSizing struct {
	SizingLeverage    float64
	ExchangeLeverage  float64
	MarginPerTradeUSD float64
	SharedWalletPool    bool
	ReleasableMarginUSD float64
	RiskPerTradePct float64
	RiskStopDistance float64
	RiskStopUnresolved string
	EntrySizeMult float64
}

func (s PerpsSizing) entrySizeMult() float64 {
	if s.EntrySizeMult <= 0 || s.EntrySizeMult > 1.0 {
		return 1.0
	}
	return s.EntrySizeMult
}

func withEntrySizeMult(sizing PerpsSizing, mult float64) PerpsSizing {
	sizing.EntrySizeMult = mult
	return sizing
}

func withSharedWalletPoolSizing(sc StrategyConfig, sizing PerpsSizing, posQty, price, avgCost, posLeverage float64, balanceKnown bool) PerpsSizing {
	if !usesSharedWalletPoolBudget(sc) {
		return sizing
	}
	sizing.SharedWalletPool = true
	marginPrice := sharedWalletPoolMarginBasisPrice(price, avgCost)
	if balanceKnown && posQty > 0 && marginPrice > 0 {
		leverage := sharedWalletPoolMarginLeverage(posLeverage, sizing.ExchangeLeverage)
		sizing.ReleasableMarginUSD = posQty * marginPrice / leverage
	}
	return sizing
}

func (s PerpsSizing) riskUnresolvedLabel() string {
	if s.RiskStopUnresolved != "" {
		return s.RiskStopUnresolved
	}
	return "stop distance unresolved"
}

func PerpsSizingFor(sc StrategyConfig, price, atr float64) PerpsSizing {
	s := PerpsSizing{
		SizingLeverage:    EffectiveSizingLeverage(sc),
		ExchangeLeverage:  EffectiveExchangeLeverage(sc),
		MarginPerTradeUSD: EffectiveMarginPerTradeUSD(sc),
	}
	pct := EffectiveRiskPerTradePct(sc)
	if pct <= 0 {
		return s
	}
	s.RiskPerTradePct = pct
	dist, ok, reason := PerpsRiskStopDistance(sc, price, atr)
	if ok {
		s.RiskStopDistance = dist
	} else {
		s.RiskStopUnresolved = reason
	}
	return s
}

func EffectiveRiskPerTradePct(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		return 0
	}
	if sc.RiskPerTradePct == nil || *sc.RiskPerTradePct <= 0 {
		return 0
	}
	return *sc.RiskPerTradePct
}

func PerpsRiskBasedNotional(cash, price, riskPct, stopDistance, exchangeLeverage float64) float64 {
	if cash <= 0 || price <= 0 || riskPct <= 0 || stopDistance <= 0 {
		return 0
	}
	riskDollars := cash * riskPct / 100
	notional := riskDollars / stopDistance * price
	if exchangeLeverage <= 0 {
		exchangeLeverage = 1
	}
	if maxNotional := cash * exchangeLeverage; notional > maxNotional {
		notional = maxNotional
	}
	return notional
}

func PerpsOpenNotionalSized(cash, price float64, sizing PerpsSizing) float64 {
	if sizing.RiskPerTradePct > 0 {
		return PerpsRiskBasedNotional(cash, price, sizing.RiskPerTradePct, sizing.RiskStopDistance, sizing.ExchangeLeverage) * sizing.entrySizeMult()
	}
	return PerpsOpenNotional(cash, sizing.SizingLeverage, sizing.ExchangeLeverage, sizing.MarginPerTradeUSD) * sizing.entrySizeMult()
}

type riskStopOwner int

const (
	riskStopOwnerNone riskStopOwner = iota
	riskStopOwnerTrailingATR
	riskStopOwnerFixedATR
	riskStopOwnerTrailingPct
	riskStopOwnerFixedPct
	riskStopOwnerMarginPct
)

func perpsRiskStopOwner(sc StrategyConfig) (riskStopOwner, string) {
	if strategyUsesUnifiedRegimeClose(sc) {
		return riskStopOwnerNone, "the unified per-regime close owns the SL and resolves it after open (#841)"
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return riskStopOwnerTrailingATR, ""
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return riskStopOwnerFixedATR, ""
	}
	if sc.StopLossATRRegime.IsConfigured() {
		return riskStopOwnerNone, "stop_loss_atr_regime resolves the SL from the regime stamped after open (#733)"
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		return riskStopOwnerNone, "trailing_stop_atr_regime resolves the SL from the regime stamped after open (#733)"
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return riskStopOwnerTrailingPct, ""
		}
		return riskStopOwnerNone, "trailing_stop_pct=0 explicitly disables the stop — no distance to size risk against"
	}
	if sc.StopLossPct != nil {
		if *sc.StopLossPct > 0 {
			return riskStopOwnerFixedPct, ""
		}
		return riskStopOwnerNone, "stop_loss_pct=0 explicitly disables the stop — no distance to size risk against"
	}
	if sc.StopLossMarginPct != nil {
		if *sc.StopLossMarginPct > 0 && sc.Leverage > 0 {
			return riskStopOwnerMarginPct, ""
		}
		if *sc.StopLossMarginPct > 0 {
			return riskStopOwnerNone, "stop_loss_margin_pct requires leverage > 0 to derive a price distance"
		}
		return riskStopOwnerNone, "stop_loss_margin_pct=0 explicitly disables the stop — no distance to size risk against"
	}
	return riskStopOwnerNone, "no explicit stop owner (the max_drawdown_pct fallback is an account backstop, not a per-trade stop)"
}

func PerpsRiskStopDistance(sc StrategyConfig, price, atr float64) (float64, bool, string) {
	owner, reason := perpsRiskStopOwner(sc)
	if reason != "" {
		return 0, false, reason
	}
	if price <= 0 {
		return 0, false, fmt.Sprintf("no positive price (got %g)", price)
	}
	switch owner {
	case riskStopOwnerTrailingATR, riskStopOwnerFixedATR:
		mult := 0.0
		field := "stop_loss_atr_mult"
		if owner == riskStopOwnerTrailingATR {
			mult = *sc.TrailingStopATRMult
			field = "trailing_stop_atr_mult"
		} else {
			mult = *sc.StopLossATRMult
		}
		if atr != atr {
			return 0, false, fmt.Sprintf("%s owner: ATR in check payload is NaN", field)
		}
		if atr <= 0 {
			return 0, false, fmt.Sprintf("%s owner: no positive ATR in check payload (got %g)", field, atr)
		}
		if atr > price*0.5 {
			return 0, false, fmt.Sprintf("%s owner: ATR %g implausible (> 50%% of price %g — unit mismatch?)", field, atr, price)
		}
		return mult * atr, true, ""
	case riskStopOwnerTrailingPct:
		return price * *sc.TrailingStopPct / 100, true, ""
	case riskStopOwnerFixedPct:
		return price * *sc.StopLossPct / 100, true, ""
	case riskStopOwnerMarginPct:
		return price * (*sc.StopLossMarginPct / sc.Leverage) / 100, true, ""
	}
	return 0, false, "no sizing-grade stop owner"
}

func validateRiskPerTradePct(sc StrategyConfig, prefix string) []string {
	if sc.RiskPerTradePct == nil {
		return nil
	}
	var errs []string
	v := *sc.RiskPerTradePct
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct is only supported for HL perps strategies (got platform=%q type=%q) — the stop owners it sizes from are HL-perps-only fields", prefix, sc.Platform, sc.Type))
	}
	if v <= 0 || v > 10 {
		errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct must be in (0, 10], got %g", prefix, v))
	}
	if sc.SizingLeverage != 0 {
		errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct and sizing_leverage are mutually exclusive — pick one sizing mode", prefix))
	}
	if sc.MarginPerTradeUSD != nil {
		errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct and margin_per_trade_usd are mutually exclusive — pick one sizing mode", prefix))
	}
	if sc.AllowScaleIn {
		errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct is incompatible with allow_scale_in — add legs re-size off frozen SL geometry, so per-trade dollar risk would not stay constant", prefix))
	}
	if sc.Type == "perps" && sc.Platform == "hyperliquid" {
		if _, reason := perpsRiskStopOwner(sc); reason != "" {
			errs = append(errs, fmt.Sprintf("%s: risk_per_trade_pct requires a stop owner whose distance is resolvable at sizing time — %s", prefix, reason))
		}
	}
	return errs
}
