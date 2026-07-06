package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// manualActionRecordsTrade reports whether a drained PendingManualAction appends
// exactly one trade to TradeHistory (so drainPendingManualActions can align the
// sendTradeAlerts tail slice). Position-changing fills do; SL-only edits
// (#1050 update-sl/cancel-sl) do not.
func manualActionRecordsTrade(action string) bool {
	switch action {
	case "open", "close", "add":
		return true
	default:
		return false
	}
}

// manualSLAutoManaged reports whether sc's automated protection would re-pin or
// re-arm a manually-edited stop-loss on the next scheduler cycle, making a
// manual SL edit ineffective (#1050). Returns a human-readable reason when true.
//
// A manual SL edit is only coherent on a strategy that has opted out of
// auto-protection (e.g. stop_loss_atr_mult: 0, no trailing/regime close) —
// otherwise the per-cycle protection sync overwrites the operator's trigger.
//
// The check has two layers. First a resolved-value pass catches the scalar
// stop_loss_atr_mult and any regime SL/trailing that already resolves to a
// positive multiplier at command time. Second a configuration-presence pass
// catches regime-resolved SL owners (#733 stop_loss_atr_regime /
// trailing_stop_atr_regime, #841 unified regime close) whose label is
// transiently the #879 fail-open "-" at command time: those resolve to a zero
// multiplier in the first pass and would otherwise slip through, yet a later
// regime resolution plus a force-SL-replace cycle re-pins the trigger. Because
// this is an auto-protective path, judge those by configuration presence, not
// by the multiplier the label happens to resolve to at the instant the command
// runs.
func manualSLAutoManaged(sc StrategyConfig, pos *Position) (bool, string) {
	if plan, ok := buildHyperliquidProtectionPlan(sc, pos); ok && plan.StopLossATRMult > 0 {
		return true, fmt.Sprintf("an ATR stop-loss is armed (effective stop_loss_atr_mult=%g)", plan.StopLossATRMult)
	}
	if effectiveTrailingStopPct(sc, pos) > 0 {
		return true, "a trailing stop manages the stop-loss"
	}
	if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		return true, "a regime-aware stop-loss (stop_loss_atr_regime) manages the stop-loss"
	}
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
		return true, "a regime-aware trailing stop (trailing_stop_atr_regime) manages the stop-loss"
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		return true, "a unified regime close owns the stop-loss"
	}
	return false, ""
}

// slTriggerWouldFillImmediately reports whether a stop-loss placed at triggerPx
// for the given side would fire instantly against the current mark: a long SL
// must sit strictly below the mark, a short SL strictly above. A non-positive
// mark (failed fetch) or trigger returns false so a transient mark-fetch
// failure never blocks a legitimate ratchet — the Python side is the final
// arbiter via StopLossFilledImmediately.
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

// slPlacementFailureLeftNaked reports whether a no-OID stop-loss placement
// failure left the position unprotected on-chain (#1052 review): true when the
// old order was cancelled (cancelSucceeded) or there was none to begin with
// (oldOID == 0); false when the cancel did not run, so the previous stop-loss
// is still resting and the position remains protected.
func slPlacementFailureLeftNaked(cancelSucceeded bool, oldOID int64) bool {
	return cancelSucceeded || oldOID == 0
}

// pendingSLActionExists reports whether an un-drained update-sl/cancel-sl action
// is already queued for strategy+symbol (#1052 review). A second SL edit before
// the daemon drains the first would read the stale pre-edit OID from state.db
// and orphan the first edit's resting order on-chain, so the caller must refuse
// until the queue drains.
func pendingSLActionExists(stateDB *StateDB, strategyID, symbol string) (bool, error) {
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

// runManualUpdateSL implements `go-trader manual-update-sl <strategy-id>
// --trigger N [--symbol Y]` (#1050). It cancel-then-places the on-chain
// stop-loss at the new trigger and queues an update-sl action the scheduler
// adopts on its next cycle (no direct state.db write).
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
	stateDB, err := OpenStateDB(cfg.DBFile)
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

// runManualCancelSL implements `go-trader manual-cancel-sl <strategy-id>
// [--symbol Y]` (#1050). It cancels the on-chain stop-loss OID and queues a
// cancel-sl action the scheduler adopts on its next cycle.
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
	stateDB, err := OpenStateDB(cfg.DBFile)
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
