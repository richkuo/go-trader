package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
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

// manualSLTarget holds the resolved, guard-checked context shared by the
// manual-update-sl and manual-cancel-sl handlers. The caller owns stateDB and
// must Close it.
type manualSLTarget struct {
	cfg     *Config
	sc      StrategyConfig
	stateDB *StateDB
	pos     *Position
	symbol  string
}

// resolveManualSLTarget loads config + state, locates the owned open position,
// and runs the shared safety guards (kill switch, pending CB close, ownership,
// auto-managed-SL). On any failure it prints a clear error, closes the DB it
// opened, and returns a non-zero exit code in rc. On success rc is 0 and the
// caller must Close target.stateDB.
func resolveManualSLTarget(cmdName, configPath, strategyID, symbolFlag string) (target manualSLTarget, rc int) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return manualSLTarget{}, 1
	}
	sc, ok := findManualStrategy(cfg, strategyID)
	if !ok {
		return manualSLTarget{}, 1
	}

	symbol := strings.ToUpper(strings.TrimSpace(symbolFlag))
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimSpace(sc.Symbol))
	}
	if symbol == "" {
		fmt.Fprintf(os.Stderr, "error: no --symbol provided and strategy %q has no configured symbol\n", strategyID)
		return manualSLTarget{}, 2
	}

	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return manualSLTarget{}, 1
	}

	state, err := LoadStateWithDB(cfg, stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		stateDB.Close()
		return manualSLTarget{}, 1
	}

	// Removing/moving protection during a portfolio flatten or a pending
	// circuit-breaker close is unsafe and pointless — the position is being
	// closed anyway. Mirror manual-open's kill-switch/CB guards.
	if state.PortfolioRisk.KillSwitchActive {
		fmt.Fprintf(os.Stderr, "error: portfolio kill switch is active — %s blocked\n", cmdName)
		stateDB.Close()
		return manualSLTarget{}, 1
	}
	ss := state.Strategies[strategyID]
	if ss != nil && ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		fmt.Fprintf(os.Stderr, "error: strategy has a pending circuit-breaker close — %s blocked\n", cmdName)
		stateDB.Close()
		return manualSLTarget{}, 1
	}

	var pos *Position
	if ss != nil {
		pos = ss.Positions[symbol]
	}
	if pos == nil {
		fmt.Fprintf(os.Stderr, "error: no open position for %s/%s\n", strategyID, symbol)
		stateDB.Close()
		return manualSLTarget{}, 1
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) {
		fmt.Fprintf(os.Stderr, "error: position %s/%s is owned by %q, not %q\n", strategyID, symbol, pos.OwnerStrategyID, strategyID)
		stateDB.Close()
		return manualSLTarget{}, 1
	}

	// Block when the strategy's automated protection would revert the edit on
	// the next cycle — otherwise the operator's change silently bounces back.
	if managed, reason := manualSLAutoManaged(sc, pos); managed {
		fmt.Fprintf(os.Stderr, "error: %s for %s/%s — a manual stop-loss edit would be reverted on the next scheduler cycle.\n", reason, strategyID, symbol)
		fmt.Fprintln(os.Stderr, "       To manage the stop-loss manually, opt the strategy out of auto-protection (set stop_loss_atr_mult: 0 and remove any trailing close).")
		stateDB.Close()
		return manualSLTarget{}, 1
	}

	return manualSLTarget{cfg: cfg, sc: sc, stateDB: stateDB, pos: pos, symbol: symbol}, 0
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
	if *trigger <= 0 {
		fmt.Fprintln(os.Stderr, "error: --trigger must be > 0")
		return 2
	}

	target, rc := resolveManualSLTarget("manual-update-sl", *configPath, strategyID, *symbol)
	if rc != 0 {
		return rc
	}
	defer target.stateDB.Close()
	pos := target.pos

	// Best-effort immediate-fill guard: a trigger on the wrong side of the mark
	// fires the moment it is placed. A failed mark fetch does not block.
	mark := 0.0
	if mids, err := fetchHyperliquidMids([]string{target.symbol}); err == nil {
		mark = mids[target.symbol]
	} else {
		fmt.Fprintf(os.Stderr, "warning: could not fetch mark for immediate-fill check: %v\n", err)
	}
	if slTriggerWouldFillImmediately(pos.Side, *trigger, mark) {
		fmt.Fprintf(os.Stderr, "error: trigger $%.4f would fill immediately against mark $%.4f for a %s position\n", *trigger, mark, pos.Side)
		return 1
	}

	if *dryRun {
		fmt.Printf("[dry-run] manual-update-sl %s: %s stop-loss $%.4f -> $%.4f (qty %.6f, cancel OID=%d)\n",
			strategyID, target.symbol, pos.StopLossTriggerPx, *trigger, pos.Quantity, pos.StopLossOID)
		return 0
	}

	slResult, slStderr, slErr := RunHyperliquidUpdateStopLoss(target.sc.Script, target.symbol, pos.Side, pos.Quantity, *trigger, pos.StopLossOID)
	if slStderr != "" {
		fmt.Fprintf(os.Stderr, "SL update stderr: %s\n", slStderr)
	}
	if slErr != nil {
		fmt.Fprintf(os.Stderr, "error updating stop-loss: %v\n", slErr)
		return 1
	}
	if slResult.Error != "" {
		fmt.Fprintf(os.Stderr, "error from HL: %s\n", slResult.Error)
		return 1
	}
	if slResult.StopLossFilledImmediately {
		// The new trigger fired on placement; the position closed on-chain. Do
		// NOT queue an update-sl (there is no resting order to adopt) — the next
		// reconcile cycle books the close.
		fmt.Fprintf(os.Stderr, "error: stop-loss filled immediately on placement — position closed on-chain; reconcile will adopt the close. Do not retry.\n")
		return 1
	}
	if slResult.StopLossOID == 0 {
		fmt.Fprintln(os.Stderr, "error: HL returned no stop-loss OID after update; on-chain state may be inconsistent — verify on the HL UI")
		return 1
	}

	newTrigger := slResult.StopLossTriggerPx
	if newTrigger == 0 {
		newTrigger = *trigger
	}
	fmt.Printf("Stop-loss updated: %s %s -> $%.4f (OID=%d)\n", strategyID, target.symbol, newTrigger, slResult.StopLossOID)

	action := PendingManualAction{
		StrategyID:        strategyID,
		Action:            "update-sl",
		Symbol:            target.symbol,
		Side:              pos.Side,
		Quantity:          pos.Quantity,
		StopLossOID:       slResult.StopLossOID,
		StopLossTriggerPx: newTrigger,
		CreatedAt:         time.Now().UTC(),
	}
	if err := target.stateDB.InsertPendingManualAction(action); err != nil {
		// On-chain SL already moved, but the scheduler will keep tracking the
		// old (now-cancelled) OID until it reconciles. The position is still
		// protected on-chain at the new trigger; flag for operator awareness.
		fmt.Fprintf(os.Stderr, "CRITICAL: stop-loss moved on-chain to $%.4f (OID=%d) but queue insert failed (%v); the scheduler still tracks the old OID until reconcile. Restart to resync.\n",
			newTrigger, slResult.StopLossOID, err)
		return 1
	}

	fmt.Printf("Queued: %s stop-loss update will sync to the dashboard after the next scheduler cycle.\n", strategyID)
	return 0
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

	target, rc := resolveManualSLTarget("manual-cancel-sl", *configPath, strategyID, *symbol)
	if rc != 0 {
		return rc
	}
	defer target.stateDB.Close()
	pos := target.pos

	if pos.StopLossOID == 0 {
		fmt.Fprintf(os.Stderr, "error: no resting stop-loss to cancel for %s/%s\n", strategyID, target.symbol)
		return 1
	}

	if *dryRun {
		fmt.Printf("[dry-run] manual-cancel-sl %s: cancel %s stop-loss $%.4f (OID=%d)\n",
			strategyID, target.symbol, pos.StopLossTriggerPx, pos.StopLossOID)
		return 0
	}

	cancelResult, cancelStderr, cancelErr := RunHyperliquidCancelOrder(target.sc.Script, target.symbol, pos.StopLossOID)
	if cancelStderr != "" {
		fmt.Fprintf(os.Stderr, "SL cancel stderr: %s\n", cancelStderr)
	}
	if cancelErr != nil {
		fmt.Fprintf(os.Stderr, "error cancelling stop-loss: %v\n", cancelErr)
		return 1
	}
	if cancelResult.Error != "" {
		fmt.Fprintf(os.Stderr, "error from HL: %s\n", cancelResult.Error)
		return 1
	}
	if !cancelResult.Cancelled {
		fmt.Fprintf(os.Stderr, "error: HL did not confirm cancel of OID %d: %s\n", pos.StopLossOID, cancelResult.CancelError)
		return 1
	}

	fmt.Printf("Stop-loss cancelled: %s %s (was OID=%d @ $%.4f)\n", strategyID, target.symbol, pos.StopLossOID, pos.StopLossTriggerPx)

	action := PendingManualAction{
		StrategyID: strategyID,
		Action:     "cancel-sl",
		Symbol:     target.symbol,
		Side:       pos.Side,
		CreatedAt:  time.Now().UTC(),
	}
	if err := target.stateDB.InsertPendingManualAction(action); err != nil {
		// The on-chain SL is gone but the scheduler still believes the position
		// is protected until it reconciles — the position is NAKED on-chain.
		fmt.Fprintf(os.Stderr, "CRITICAL: stop-loss cancelled on-chain but queue insert failed (%v); the position is now UNPROTECTED and the scheduler still tracks the old OID. Re-arm protection or restart immediately.\n", err)
		return 1
	}

	fmt.Printf("Queued: %s stop-loss removal will sync to the dashboard after the next scheduler cycle.\n", strategyID)
	return 0
}
