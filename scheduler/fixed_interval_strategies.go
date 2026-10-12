package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const fixedIntervalParam = "expected_interval_ms"

var fixedIntervalStrategyDefaults = map[string]int64{
	"camarilla_pivot_reversal": 3_600_000,
	"chande_momentum":          3_600_000,
	"dual_thrust":              3_600_000,
}

type fixedIntervalUse struct {
	Role   string
	Name   string
	Params map[string]interface{}
}

func fixedIntervalUses(sc StrategyConfig) []fixedIntervalUse {
	return namedStrategyUses(sc, func(name string) bool {
		_, ok := fixedIntervalStrategyDefaults[name]
		return ok
	})
}

func namedStrategyUses(sc StrategyConfig, match func(string) bool) []fixedIntervalUse {
	if sc.Type == "options" {
		return nil
	}
	var uses []fixedIntervalUse
	openName := strings.TrimSpace(effectiveOpenStrategy(sc))
	if match(openName) {
		uses = append(uses, fixedIntervalUse{Role: edgeRefRoleOpen, Name: openName, Params: sc.OpenStrategy.Params})
	}
	for _, ref := range sc.closeRefs() {
		name := canonicalCloseStrategyName(ref.Name)
		if !match(name) {
			continue
		}
		params := ref.Params
		if len(params) == 0 {
			params = nil
			if name == openName {
				params = sc.OpenStrategy.Params
			}
		}
		uses = append(uses, fixedIntervalUse{Role: edgeRefRoleClose, Name: name, Params: params})
	}
	return uses
}

func fixedIntervalParamValue(params map[string]interface{}) (int64, bool, error) {
	raw, ok := params[fixedIntervalParam]
	if !ok {
		return 0, false, nil
	}
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, true, fmt.Errorf("must be a positive integer, got %v", raw)
		}
		f = parsed
	default:
		return 0, true, fmt.Errorf("must be a positive integer, got %#v", raw)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 1 || f != math.Trunc(f) || f > math.MaxInt64/2 {
		return 0, true, fmt.Errorf("must be a positive integer, got %v", raw)
	}
	return int64(f), true, nil
}

func fixedIntervalContinuousCandles(sc StrategyConfig) bool {
	return sc.Type == "perps" || (sc.Type == "manual" && sc.Platform == "hyperliquid")
}

func fixedIntervalStrategyErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, use := range fixedIntervalUses(sc) {
			label := fmt.Sprintf("strategy %s: %s strategy %q", sc.ID, use.Role, use.Name)
			if !fixedIntervalContinuousCandles(sc) {
				errs = append(errs, fmt.Sprintf("%s holds every signal unless its candles are spaced exactly %s apart; type=%s platform=%s has trading-session breaks or no continuous candle contract, so it runs only on perps (hyperliquid, okx) and Hyperliquid manual strategies", label, fixedIntervalParam, sc.Type, sc.Platform))
				continue
			}
			_, timeframe := strategyArgSymbolTimeframe(sc.Args)
			if timeframe == "" && sc.Type == "manual" {
				timeframe = strings.TrimSpace(sc.Timeframe)
			}
			want, ok := closedBarDecisionIntervalsMs[timeframe]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s needs a fixed-duration timeframe in args to check %s, got %q (supported: %s)", label, fixedIntervalParam, timeframe, closedBarSupportedIntervals()))
				continue
			}
			got, explicit, err := fixedIntervalParamValue(use.Params)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: params.%s %v", label, fixedIntervalParam, err))
				continue
			}
			source := "explicit"
			if !explicit {
				got = fixedIntervalStrategyDefaults[use.Name]
				source = "registry default"
			}
			if got != want {
				errs = append(errs, fmt.Sprintf("%s: %s %s=%d does not match timeframe %s (%d ms), so every signal would hold on a cadence gap; set \"%s\": %d in its params", label, source, fixedIntervalParam, got, timeframe, want, fixedIntervalParam, want))
			}
		}
	}
	return errs
}
