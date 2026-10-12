package main

import (
	"fmt"
	"sort"
	"strings"
)

const camarillaPivotReversalStrategyName = "camarilla_pivot_reversal"

var camarillaPivotReversalIntervals = map[string]int64{
	"15m": 900_000,
	"30m": 1_800_000,
	"1h":  3_600_000,
	"2h":  7_200_000,
	"4h":  14_400_000,
	"8h":  28_800_000,
	"12h": 43_200_000,
}

func camarillaPivotReversalSupportedIntervals() string {
	out := make([]string, 0, len(camarillaPivotReversalIntervals))
	for tf := range camarillaPivotReversalIntervals {
		out = append(out, tf)
	}
	sort.Slice(out, func(i, j int) bool {
		return camarillaPivotReversalIntervals[out[i]] < camarillaPivotReversalIntervals[out[j]]
	})
	return strings.Join(out, ", ")
}

func camarillaPivotReversalUseError(sc StrategyConfig, use fixedIntervalUse) error {
	_, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if _, ok := camarillaPivotReversalIntervals[timeframe]; !ok {
		return fmt.Errorf("timeframe %q is not supported (supported: %s); a complete prior UTC day and the current session must fit the 200-bar check window", timeframe, camarillaPivotReversalSupportedIntervals())
	}
	got, explicit, err := fixedIntervalParamValue(use.Params)
	if err != nil {
		return fmt.Errorf("params.%s %v", fixedIntervalParam, err)
	}
	if !explicit {
		got = fixedIntervalStrategyDefaults[camarillaPivotReversalStrategyName]
	}
	for _, ms := range camarillaPivotReversalIntervals {
		if ms == got {
			return nil
		}
	}
	return fmt.Errorf("params.%s=%d is not a supported interval (supported: %s)", fixedIntervalParam, got, camarillaPivotReversalSupportedIntervals())
}

func camarillaPivotReversalParamErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, use := range namedStrategyUses(sc, func(name string) bool { return name == camarillaPivotReversalStrategyName }) {
			if err := camarillaPivotReversalUseError(sc, use); err != nil {
				errs = append(errs, fmt.Sprintf("strategy %s: %s strategy %s %v", sc.ID, use.Role, camarillaPivotReversalStrategyName, err))
			}
		}
	}
	return errs
}
