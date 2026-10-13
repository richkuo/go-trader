package main

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	calendarConditionedEntryStrategyName        = "calendar_conditioned_entry"
	calendarConditionedEntryTrainingParam       = "training_bars"
	calendarConditionedEntryDefaultTrainingBars = 4368
	hyperliquidCheckOhlcvLimitFlag              = "--ohlcv-limit"
)

func hyperliquidCheckArgsOhlcvLimit(args []string) (int64, bool, error) {
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
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 1 {
			return 0, true, fmt.Errorf("args %s=%q is not a positive integer", name, value)
		}
		found = true
		if n > limit {
			limit = n
		}
	}
	return limit, found, nil
}

func calendarConditionedEntryCheckFrameLimit(sc StrategyConfig, rc *RegimeConfig) (int64, string, error) {
	limit := int64(hlBatchPythonDefaultOhlcvLimit)
	source := "the default check frame"
	if rc != nil && rc.Enabled {
		if regimeLimit := int64(regimeRequiredOhlcvLimit(rc)); regimeLimit > limit {
			limit = regimeLimit
			source = "the regime-enabled check frame"
		}
	}
	argsLimit, found, err := hyperliquidCheckArgsOhlcvLimit(sc.Args)
	if err != nil {
		return 0, "", err
	}
	if found && argsLimit > limit {
		limit = argsLimit
		source = "the args " + hyperliquidCheckOhlcvLimitFlag + " check frame"
	}
	return limit, source, nil
}

func calendarConditionedEntryUseError(sc StrategyConfig, use fixedIntervalUse, rc *RegimeConfig) error {
	training, explicit, err := positiveIntegerParamValue(use.Params, calendarConditionedEntryTrainingParam)
	if err != nil {
		return fmt.Errorf("params.%s %v", calendarConditionedEntryTrainingParam, err)
	}
	source := "explicit"
	if !explicit {
		training = calendarConditionedEntryDefaultTrainingBars
		source = "registry default"
	}
	limit, frame, err := calendarConditionedEntryCheckFrameLimit(sc, rc)
	if err != nil {
		return err
	}
	if training > limit {
		return nil
	}
	return fmt.Errorf("%s %s=%d fits %s of %d candles, so entries can fire, but live and paper checks never supply bars_held, so its time_stop close never fires and only the stop would close the position; keep %s above %d", source, calendarConditionedEntryTrainingParam, training, frame, limit, calendarConditionedEntryTrainingParam, limit)
}

func calendarConditionedEntryHistoryErrors(strategies []StrategyConfig, rc *RegimeConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, use := range namedStrategyUses(sc, func(name string) bool { return name == calendarConditionedEntryStrategyName }) {
			if err := calendarConditionedEntryUseError(sc, use, rc); err != nil {
				errs = append(errs, fmt.Sprintf("strategy %s: %s strategy %s %v", sc.ID, use.Role, calendarConditionedEntryStrategyName, err))
			}
		}
	}
	return errs
}
