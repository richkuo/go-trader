package main

// risk_sizing.go — opt-in risk-per-trade position sizing for HL perps (#1268).
//
// The legacy sizing formulas are notional-based: cash × sizing_leverage, or
// min(margin_per_trade_usd, cash) × exchange_leverage (#497/#518). Neither
// consults the distance to the strategy's stop, so two strategies with equal
// notional but different ATR stops carry different dollar risk per trade.
//
// risk_per_trade_pct switches a strategy to fixed-fractional sizing:
//
//	riskDollars = cash × risk_per_trade_pct / 100
//	qty         = riskDollars / stopDistance
//	notional    = qty × price, capped at cash × exchange_leverage
//
// where stopDistance is derived from the strategy's resolved stop owner
// (PerpsRiskStopDistance). The mode is opt-in and mutually exclusive with
// sizing_leverage / margin_per_trade_usd; configs without the field keep the
// legacy formulas byte-identically.
//
// Fail-closed contract: when the stop distance cannot be resolved at sizing
// time (no usable ATR in the check payload for an ATR-mult owner), a fresh
// open is REFUSED with a skip reason — never silently sized from the notional
// formulas. A bidirectional flip degrades to close-only in that case (the
// close leg must still fire; matching the insufficient-cash flip behavior in
// perpsLiveOrderSize). Stop owners that only resolve after the position is
// open (stop_loss_atr_regime / trailing_stop_atr_regime / the #841 unified
// regime close) are rejected at config load instead of failing every cycle.

import "fmt"

// PerpsSizing bundles the per-open sizing inputs resolved from a
// StrategyConfig and threaded into the perps sizers (#497/#518/#1268).
// Exactly one sizing mode applies per open: risk-per-trade when
// RiskPerTradePct > 0 (validation guarantees the notional fields are unset),
// else margin-space when MarginPerTradeUSD > 0, else cash × SizingLeverage.
type PerpsSizing struct {
	SizingLeverage    float64
	ExchangeLeverage  float64
	MarginPerTradeUSD float64
	// RiskPerTradePct opts into risk-per-trade sizing (#1268): the percent of
	// strategy cash to lose if the stop is hit. 0 = notional mode.
	RiskPerTradePct float64
	// RiskStopDistance is the resolved per-unit stop distance in USD (price
	// units) for the risk formula. 0 with RiskPerTradePct > 0 means the
	// distance could not be resolved at sizing time — fresh opens fail closed.
	RiskStopDistance float64
	// RiskStopUnresolved carries the resolver's reason when RiskStopDistance
	// is 0 in risk mode, for skip-reason logging.
	RiskStopUnresolved string
}

// riskUnresolvedLabel returns the resolver failure reason, defaulting to a
// generic label so log lines never render an empty cause.
func (s PerpsSizing) riskUnresolvedLabel() string {
	if s.RiskStopUnresolved != "" {
		return s.RiskStopUnresolved
	}
	return "stop distance unresolved"
}

// PerpsSizingFor resolves the full sizing bundle for a strategy at a given
// mark price and check-payload ATR (the same `indicators.atr` value that
// stampEntryATRIfOpened later freezes onto the position, so sizing and SL
// geometry agree). atr may be 0 when the payload carried none — pct-based
// stop owners don't need it; ATR-mult owners then fail closed.
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

// EffectiveRiskPerTradePct returns the configured risk-per-trade percent, or
// 0 when the strategy is not opted in. HL perps only — validation rejects the
// field elsewhere, and the stop owners the distance derives from are
// HL-perps-only fields.
func EffectiveRiskPerTradePct(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		return 0
	}
	if sc.RiskPerTradePct == nil || *sc.RiskPerTradePct <= 0 {
		return 0
	}
	return *sc.RiskPerTradePct
}

// PerpsRiskBasedNotional is the risk-mode primitive: the USD notional whose
// quantity loses (cash × riskPct/100) if price moves stopDistance against the
// entry. Capped at cash × exchangeLeverage so a tight stop can never size
// past what the exchange will margin (#1268). Returns 0 on any non-positive
// input; callers treat 0 as "cannot size".
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

// PerpsOpenNotionalSized dispatches between the risk formula (#1268) and the
// legacy notional formulas (#497/#518) from a resolved PerpsSizing bundle.
// In risk mode with an unresolved stop distance it returns 0 — the sizers
// turn that into a fail-closed refusal (fresh open) or a close-only degrade
// (flip), never a silent notional fallback.
func PerpsOpenNotionalSized(cash, price float64, sizing PerpsSizing) float64 {
	if sizing.RiskPerTradePct > 0 {
		return PerpsRiskBasedNotional(cash, price, sizing.RiskPerTradePct, sizing.RiskStopDistance, sizing.ExchangeLeverage)
	}
	return PerpsOpenNotional(cash, sizing.SizingLeverage, sizing.ExchangeLeverage, sizing.MarginPerTradeUSD)
}

// riskStopOwner enumerates the stop owners risk-per-trade sizing can derive a
// pre-open stop distance from.
type riskStopOwner int

const (
	riskStopOwnerNone riskStopOwner = iota
	riskStopOwnerTrailingATR
	riskStopOwnerFixedATR
	riskStopOwnerTrailingPct
	riskStopOwnerFixedPct
	riskStopOwnerMarginPct
)

// perpsRiskStopOwner resolves WHICH stop owner would govern the strategy's
// stop distance, mirroring EffectiveStopLossPct's priority order exactly
// (trailing ATR > fixed ATR > regime blocks > trailing pct > fixed pct >
// margin pct). Returns a non-empty reason when no sizing-grade owner exists:
//   - the #841 unified regime close and the *_atr_regime blocks resolve their
//     SL per-regime after the position opens, so the distance is unknowable at
//     sizing time;
//   - explicitly disabled stops (explicit 0 on the selected pct owner) leave
//     the position stop-less — nothing to normalize risk against;
//   - the MaxDrawdownPct fallback is a capped account backstop, not a
//     per-trade stop, and the backtester cannot model it — excluded so live
//     and backtest reject identically.
//
// validateConfig calls this at load so statically unresolvable owners fail
// there; PerpsRiskStopDistance calls it per-open as defense in depth.
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

// PerpsRiskStopDistance derives the per-unit stop distance in USD for
// risk-per-trade sizing from the strategy's resolved stop owner (#1268).
//
// price is the mark the open would fill near (the same price the sizers turn
// notional into quantity with); atr is the check payload's `indicators.atr`.
// ATR-mult owners require a usable ATR — positive, non-NaN, and ≤ 50% of
// price (the stampEntryATRIfOpened plausibility bound; an ATR beyond it is
// almost certainly a unit mismatch, and sizing from it would deflate risk by
// the same error factor). Pct owners derive distance from price alone.
//
// Returns (distance, true, "") or (0, false, reason). Callers fail closed on
// !ok — a fresh open is refused, never notionally sized.
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
		if atr != atr { // NaN
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

// validateRiskPerTradePct returns the validation errors for a strategy's
// risk_per_trade_pct field (#1268); empty when the field is unset or valid.
// Called from validateConfig, AFTER LoadConfig's default_stop_loss_atr_mult
// pass has materialized the implicit stop owner on all-omitted strategies —
// so the "no explicit stop owner" reject only fires for genuine opt-outs.
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
	// Scale-in adds re-size off the frozen #873 RiskAnchorPrice geometry, so
	// per-add dollar risk would not be the constant the mode promises. Reject
	// the combination rather than ship a mode that silently breaks its own
	// invariant on the add leg.
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
