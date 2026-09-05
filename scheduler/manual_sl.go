package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func manualActionRecordsTrade(action string) bool {
	switch action {
	case "open", "close", "add":
		return true
	default:
		return false
	}
}

func manualSLAutoManaged(sc StrategyConfig, pos *Position) (bool, string) {
	if plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0); ok && plan.StopLossATRMult > 0 {
		return true, fmt.Sprintf("an ATR stop-loss is armed (effective stop_loss_atr_mult=%g)", plan.StopLossATRMult)
	}
	if effectiveTrailingStopPct(sc, pos) > 0 {
		return true, "a trailing stop manages the stop-loss"
	}
	if sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero() {
		return true, "a regime-aware stop-loss (stop_loss_atr_mult_regime) manages the stop-loss"
	}
	if sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero() {
		return true, "a regime-aware trailing stop (trailing_stop_atr_mult_regime) manages the stop-loss"
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		return true, "a unified regime close owns the stop-loss"
	}
	return false, ""
}

func slTriggerWouldFillImmediately(side string, triggerPx, mark float64) bool {
	if mark <= 0 || triggerPx <= 0 {
		return false
	}
	switch side {
	case "long":
		return triggerPx >= mark
	case "short":
		return triggerPx <= mark
	}
	return false
}

func slPlacementFailureLeftNaked(cancelSucceeded bool, oldOID int64) bool {
	return cancelSucceeded || oldOID == 0
}

func pendingSLActionExists(stateDB *StateStore, strategyID, symbol string) (bool, error) {
	actions, err := stateDB.LoadPendingManualActions()
	if err != nil {
		return false, err
	}
	for _, a := range actions {
		if a.StrategyID != strategyID || !strings.EqualFold(a.Symbol, symbol) {
			continue
		}
		if a.Action == "update-sl" || a.Action == "cancel-sl" {
			return true, nil
		}
	}
	return false, nil
}

func runManualUpdateSL(args []string) int {
	fs := flag.NewFlagSet("manual-update-sl", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	symbol := fs.String("symbol", "", "Coin symbol (defaults to the strategy's configured symbol)")
	trigger := fs.Float64("trigger", 0, "New stop-loss trigger price (required)")
	dryRun := fs.Bool("dry-run", false, "Print the planned action without placing the order or mutating state")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-update-sl <strategy-id> --trigger N [--symbol Y] [--dry-run]")
		return 2
	}
	strategyID := fs.Arg(0)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	sc, ok := findManualStrategy(cfg, strategyID)
	if !ok {
		return 1
	}
	stateDB, err := openToolStateStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	res, coreErr := manualUpdateSLCore(newCLIManualCoreDeps(cfg, stateDB, nil), sc, manualSLInputs{
		StrategyID: strategyID,
		Symbol:     *symbol,
		Trigger:    *trigger,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

func runManualCancelSL(args []string) int {
	fs := flag.NewFlagSet("manual-cancel-sl", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	symbol := fs.String("symbol", "", "Coin symbol (defaults to the strategy's configured symbol)")
	dryRun := fs.Bool("dry-run", false, "Print the planned action without cancelling the order or mutating state")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-cancel-sl <strategy-id> [--symbol Y] [--dry-run]")
		return 2
	}
	strategyID := fs.Arg(0)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	sc, ok := findManualStrategy(cfg, strategyID)
	if !ok {
		return 1
	}
	stateDB, err := openToolStateStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	res, coreErr := manualCancelSLCore(newCLIManualCoreDeps(cfg, stateDB, nil), sc, manualSLInputs{
		StrategyID: strategyID,
		Symbol:     *symbol,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}
