package main

import (
	"fmt"
	"math"
)

const (
	dualThrustStrategyName   = "dual_thrust"
	dualThrustDayMs          = int64(86_400_000)
	dualThrustHourMs         = int64(3_600_000)
	dualThrustLookbackMin    = 1
	dualThrustLookbackMax    = 199
	dualThrustCoefficientMax = 2.0
	dualThrustMaxWindowBars  = int64(200)
)

var closedBarRequiredStrategies = map[string]string{
	dualThrustStrategyName: "it marks each side's session event as used from the closes it reads, so a forming candle can lose the session's entry or fire it twice",
}

func closedBarRequiredStrategyErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, use := range namedStrategyUses(sc, func(name string) bool {
			_, ok := closedBarRequiredStrategies[name]
			return ok
		}) {
			label := fmt.Sprintf("strategy %s: %s strategy %q", sc.ID, use.Role, use.Name)
			reason := closedBarRequiredStrategies[use.Name]
			if _, why := closedBarDecisionProvider(sc); why != "" {
				errs = append(errs, fmt.Sprintf("%s decides only on closed candles (%s), and closed_bar_decisions is not supported for type=%s platform=%s: %s", label, reason, sc.Type, sc.Platform, why))
				continue
			}
			if !sc.ClosedBarDecisions {
				errs = append(errs, fmt.Sprintf("%s decides only on closed candles (%s); set \"closed_bar_decisions\": true on the strategy", label, reason))
			}
		}
	}
	return errs
}

func dualThrustIntegerParam(params map[string]interface{}, key string, def, low, high int64) (int64, error) {
	v, ok, err := aroonTrendParamNumber(params, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return def, nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
		return 0, fmt.Errorf("%s must be an integer, got %v", key, v)
	}
	if v < float64(low) || v > float64(high) {
		return 0, fmt.Errorf("%s must be in [%d, %d], got %v", key, low, high, v)
	}
	return int64(v), nil
}

func dualThrustCoefficientParam(params map[string]interface{}, key string) error {
	v, ok, err := aroonTrendParamNumber(params, key)
	if err != nil || !ok {
		return err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > dualThrustCoefficientMax {
		return fmt.Errorf("%s must be finite, above 0 and at most %g, got %v", key, dualThrustCoefficientMax, v)
	}
	return nil
}

func dualThrustParamsError(params map[string]interface{}) error {
	lookback, err := dualThrustIntegerParam(params, "lookback_sessions", 4, dualThrustLookbackMin, dualThrustLookbackMax)
	if err != nil {
		return err
	}
	for _, key := range []string{"k_long", "k_short"} {
		if err := dualThrustCoefficientParam(params, key); err != nil {
			return err
		}
	}
	hour, err := dualThrustIntegerParam(params, "session_start_hour", 0, 0, 23)
	if err != nil {
		return err
	}
	interval, err := dualThrustIntegerParam(params, fixedIntervalParam, fixedIntervalStrategyDefaults[dualThrustStrategyName], 1, dualThrustDayMs)
	if err != nil {
		return err
	}
	if dualThrustDayMs%interval != 0 {
		return fmt.Errorf("%s=%d must divide a 24-hour UTC day (%d ms) exactly", fixedIntervalParam, interval, dualThrustDayMs)
	}
	if (hour*dualThrustHourMs)%interval != 0 {
		return fmt.Errorf("session_start_hour=%d is not on the %d ms candle grid; a candle would cross the session boundary", hour, interval)
	}
	perSession := dualThrustDayMs / interval
	if span := (lookback + 1) * perSession; span > dualThrustMaxWindowBars {
		return fmt.Errorf("(lookback_sessions + 1) x bars per session = (%d + 1) x %d = %d exceeds the %d-bar check window", lookback, perSession, span, dualThrustMaxWindowBars)
	}
	return nil
}

func dualThrustParamErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, use := range namedStrategyUses(sc, func(name string) bool { return name == dualThrustStrategyName }) {
			if err := dualThrustParamsError(use.Params); err != nil {
				errs = append(errs, fmt.Sprintf("strategy %s: %s strategy %s params: %v", sc.ID, use.Role, dualThrustStrategyName, err))
			}
		}
	}
	return errs
}
