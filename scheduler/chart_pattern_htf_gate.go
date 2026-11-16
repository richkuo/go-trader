package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	chartPatternStrategyName               = "chart_pattern"
	chartPatternHTFGateFactorParam         = "htf_gate_factor"
	chartPatternHTFGateEMASlowParam        = "htf_gate_ema_slow"
	chartPatternHTFGateModeParam           = "htf_gate_mode"
	chartPatternDefaultHTFGateEMASlow      = 40
	chartPatternDefaultHTFGateMode         = "veto"
	chartPatternHTFGateParamMax            = 1_000_000
	chartPatternHTFGateWarmIndicator       = "htf_gate_warm"
	chartPatternHTFGateBucketsIndicator    = "htf_gate_buckets"
	chartPatternHTFGateCandlesIndicator    = "htf_gate_candles"
	chartPatternHTFGateMinCandlesIndicator = "htf_gate_min_candles"
	firstValueOhlcvLimitCheckScript        = "check_strategy.py"
)

type chartPatternHTFGate struct {
	Factor        int64
	EMASlow       int64
	EMASlowSource string
	Mode          string
}

func (g chartPatternHTFGate) minCandles() int64 {
	return g.Factor * (g.EMASlow + 1)
}

func chartPatternHTFGateFromParams(params map[string]interface{}) (chartPatternHTFGate, bool, error) {
	factorParams := params
	if raw, ok := params[chartPatternHTFGateFactorParam]; ok && raw == nil {
		factorParams = nil
	}
	factor, err := dualThrustIntegerParam(factorParams, chartPatternHTFGateFactorParam, 0, -chartPatternHTFGateParamMax, chartPatternHTFGateParamMax)
	if err != nil {
		return chartPatternHTFGate{}, false, err
	}
	if factor <= 1 {
		return chartPatternHTFGate{}, false, nil
	}
	gate := chartPatternHTFGate{Factor: factor, EMASlowSource: "explicit", Mode: chartPatternDefaultHTFGateMode}
	if _, explicit := params[chartPatternHTFGateEMASlowParam]; !explicit {
		gate.EMASlowSource = "registry default"
	}
	gate.EMASlow, err = dualThrustIntegerParam(params, chartPatternHTFGateEMASlowParam, chartPatternDefaultHTFGateEMASlow, 1, chartPatternHTFGateParamMax)
	if err != nil {
		return chartPatternHTFGate{}, false, err
	}
	if mode, ok := params[chartPatternHTFGateModeParam].(string); ok {
		gate.Mode = mode
	}
	return gate, true, nil
}

func chartPatternOpenHTFGate(sc StrategyConfig) (chartPatternHTFGate, bool) {
	for _, use := range namedStrategyUses(sc, func(name string) bool { return name == chartPatternStrategyName }) {
		if use.Role != edgeRefRoleOpen {
			continue
		}
		gate, active, err := chartPatternHTFGateFromParams(use.Params)
		if err != nil || !active {
			return chartPatternHTFGate{}, false
		}
		return gate, true
	}
	return chartPatternHTFGate{}, false
}

func checkScriptTakesFirstOhlcvLimit(sc StrategyConfig) bool {
	return filepath.Base(strings.TrimSpace(sc.Script)) == firstValueOhlcvLimitCheckScript
}

func parseCheckOhlcvLimitValue(name, value string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("args %s=%q is not a positive integer", name, value)
	}
	return n, nil
}

func firstCheckArgsOhlcvLimit(args []string) (int64, bool, error) {
	prefix := hyperliquidCheckOhlcvLimitFlag + "="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			n, err := parseCheckOhlcvLimitValue(hyperliquidCheckOhlcvLimitFlag, strings.TrimPrefix(arg, prefix))
			return n, true, err
		}
	}
	for i, arg := range args {
		if arg != hyperliquidCheckOhlcvLimitFlag {
			continue
		}
		if i+1 >= len(args) {
			return 0, true, fmt.Errorf("args %s has no value", arg)
		}
		n, err := parseCheckOhlcvLimitValue(arg, args[i+1])
		return n, true, err
	}
	return 0, false, nil
}

func lastCheckArgsOhlcvLimit(args []string) (int64, bool, error) {
	var limit int64
	found := false
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(strings.TrimSpace(args[i]), "=")
		if len(name) < 4 || !strings.HasPrefix(hyperliquidCheckOhlcvLimitFlag, name) {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return 0, true, fmt.Errorf("args %s has no value", name)
			}
			i++
			value = args[i]
		}
		n, err := parseCheckOhlcvLimitValue(name, value)
		if err != nil {
			return 0, true, err
		}
		limit, found = n, true
	}
	return limit, found, nil
}

func feedConsumerCoversStrategy(cfg *Config, sc StrategyConfig) bool {
	return cfg != nil && cfg.marketFeedDeadlineScheduled() && feedScopedStrategy(sc)
}

func chartPatternCheckFrame(sc StrategyConfig, cfg *Config) (int64, string, error) {
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	regimeOn := rc != nil && rc.Enabled
	frame := int64(hlBatchPythonDefaultOhlcvLimit)
	source := "the default check frame"
	if regimeOn {
		frame = int64(regimeRequiredOhlcvLimit(rc))
		source = "the regime-enabled check frame (the scheduler appends its " + hyperliquidCheckOhlcvLimitFlag + " after args)"
	}
	firstWins := checkScriptTakesFirstOhlcvLimit(sc)
	var argsLimit int64
	var found bool
	var err error
	if firstWins {
		argsLimit, found, err = firstCheckArgsOhlcvLimit(sc.Args)
	} else {
		argsLimit, found, err = lastCheckArgsOhlcvLimit(sc.Args)
	}
	if err != nil {
		return 0, "", err
	}
	if found && (firstWins || !regimeOn) {
		frame = argsLimit
		source = "the args " + hyperliquidCheckOhlcvLimitFlag + " check frame"
	}
	if feedConsumerCoversStrategy(cfg, sc) {
		if lookback := int64(feedSignalLookback(rc)); lookback < frame {
			frame = lookback
			source = fmt.Sprintf("the %s market feed signal frame (the sealed snapshot holds %d candles for this slot)", cfg.marketFeedMode(), lookback)
		}
	}
	return frame, source, nil
}

func chartPatternHTFGateUseError(sc StrategyConfig, use fixedIntervalUse, cfg *Config) error {
	gate, active, err := chartPatternHTFGateFromParams(use.Params)
	if err != nil {
		return fmt.Errorf("params: %v", err)
	}
	if !active {
		return nil
	}
	frame, source, err := chartPatternCheckFrame(sc, cfg)
	if err != nil {
		return err
	}
	visible := frame / gate.Factor
	if visible > gate.EMASlow {
		return nil
	}
	return fmt.Errorf("%s=%d (%s=%q, %s %s=%d) cannot warm in %s of %d candles: the worst bucket alignment leaves floor(%d / %d) = %d visible higher-timeframe buckets and the trend needs %d, so veto mode would never block a signal and align mode would block every signal; the frame must hold at least %d candles (%s x (%s + 1)); lower %s or %s so that product is at most %d",
		chartPatternHTFGateFactorParam, gate.Factor, chartPatternHTFGateModeParam, gate.Mode, gate.EMASlowSource, chartPatternHTFGateEMASlowParam, gate.EMASlow,
		source, frame, frame, gate.Factor, visible, gate.EMASlow+1, gate.minCandles(),
		chartPatternHTFGateFactorParam, chartPatternHTFGateEMASlowParam, chartPatternHTFGateFactorParam, chartPatternHTFGateEMASlowParam, frame)
}

func chartPatternHTFGateConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		for _, use := range namedStrategyUses(sc, func(name string) bool { return name == chartPatternStrategyName }) {
			if err := chartPatternHTFGateUseError(sc, use, cfg); err != nil {
				errs = append(errs, fmt.Sprintf("strategy %s: %s strategy %s %v", sc.ID, use.Role, chartPatternStrategyName, err))
			}
		}
	}
	return errs
}

func chartPatternIndicatorInt(indicators map[string]interface{}, key string) (int64, bool) {
	raw, ok := indicators[key]
	if !ok {
		return 0, false
	}
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

func applyChartPatternHTFGateHold(sc StrategyConfig, indicators map[string]interface{}, signal *int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool, signalStr string, logger *StrategyLogger) {
	gate, active := chartPatternOpenHTFGate(sc)
	if !active || signal == nil {
		return
	}
	blocks := pausedBlocksSignal(*signal, closeFraction, posQty, posSide, allowsLong, allowsShort)
	warm, ok := chartPatternIndicatorInt(indicators, chartPatternHTFGateWarmIndicator)
	if !ok || (warm != 0 && warm != 1) {
		if blocks {
			if logger != nil {
				logger.Warn("Chart pattern HTF gate: %s signal suppressed — %s=%d is active but the check returned no valid %s indicator, so it cannot show the higher-timeframe trend was warm; entries and additions are held, and closes, stops, ratchet and protection continue (redeploy with scripts/update.sh so Go and Python match) (#2032)",
					signalStr, chartPatternHTFGateFactorParam, gate.Factor, chartPatternHTFGateWarmIndicator)
			}
			*signal = 0
		}
		return
	}
	if warm == 1 {
		return
	}
	candles, _ := chartPatternIndicatorInt(indicators, chartPatternHTFGateCandlesIndicator)
	buckets, _ := chartPatternIndicatorInt(indicators, chartPatternHTFGateBucketsIndicator)
	needed, ok := chartPatternIndicatorInt(indicators, chartPatternHTFGateMinCandlesIndicator)
	if !ok {
		needed = gate.minCandles()
	}
	if logger != nil {
		logger.Warn("Chart pattern HTF gate cold: %s=%d (%s=%d, %s=%q) got %d candles (%d visible higher-timeframe buckets) and needs %d candles (%d buckets), so its trend is not warm this cycle; entries and additions are held, and closes, stops, ratchet and protection continue (#2032)",
			chartPatternHTFGateFactorParam, gate.Factor, chartPatternHTFGateEMASlowParam, gate.EMASlow, chartPatternHTFGateModeParam, gate.Mode, candles, buckets, needed, gate.EMASlow+1)
	}
	if blocks {
		if logger != nil {
			logger.Warn("Chart pattern HTF gate: %s signal suppressed — the higher-timeframe trend is cold this cycle (#2032)", signalStr)
		}
		*signal = 0
	}
}
