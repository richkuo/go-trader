package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// defaultManualMarginUSD is the implicit --margin value used by manual-open
// when the operator omits all sizing flags (#691). --record-only still requires
// an explicit --size since the operator placed the on-chain order themselves.
const defaultManualMarginUSD = 50.0

// defaultManualStopLossATRMult is the implicit stop_loss_atr_mult applied to
// HL type=manual strategies that omit all stop fields (#691). Kept separate
// from DefaultStopLossATRMult (1.0) so non-manual perps keep their own default.
const defaultManualStopLossATRMult = 1.5

// runManualOpen implements `go-trader manual-open <strategy-id>`.
// It places an on-chain HL order (or records an existing fill with --record-only),
// then enqueues the fill in pending_manual_actions for the scheduler to drain.
func runManualOpen(args []string) int {
	fs := flag.NewFlagSet("manual-open", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	side := fs.String("side", "", "Position side: long or short (default: \"long\", override via manual_defaults.side in config)")
	size := fs.Float64("size", 0, "Size in base units (coin qty)")
	notional := fs.Float64("notional", 0, "Size as USD notional (size = notional / price)")
	margin := fs.Float64("margin", 0, "Size as USD margin (size = margin * leverage / price)")
	atr := fs.Float64("atr", 0, "ATR value to stamp on the position (required for ATR-based stops when not auto-fetched)")
	slATRMult := fs.Float64("stop-loss-atr-mult", 0, "Override stop_loss_atr_mult for this position (0 = use strategy default)")
	slPct := fs.Float64("stop-loss-pct", 0, "Override stop_loss_pct for this position (0 = use strategy default)")
	fillPrice := fs.Float64("fill-price", 0, "Fill price for --record-only (required when --record-only is set)")
	recordOnly := fs.Bool("record-only", false, "Register an existing fill without placing a new on-chain order")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

	// #711: stdlib flag.Parse stops at the first positional arg, so the
	// documented `manual-open <strategy-id> --flag value` form fails to parse
	// the trailing flags. Reorder to put the positional last so both
	// orderings work.
	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-open <strategy-id> [--side long|short] [--size N | --notional N | --margin N] [flags]")
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

	// #696: resolve --side default after config load so manual_defaults.side
	// can override the "long" fallback when the operator omits the flag.
	*side = strings.ToLower(strings.TrimSpace(*side))
	if *side == "" {
		*side = cfg.resolveManualSide()
	}
	if *side != "long" && *side != "short" {
		fmt.Fprintf(os.Stderr, "error: --side must be \"long\" or \"short\", got %q\n", *side)
		return 2
	}
	// #656: direction enum gates manual-open sides. direction="long" rejects
	// --side short (legacy allow_shorts=false behavior); direction="short"
	// rejects --side long; direction="both" allows either.
	if *side == "short" && !PerpsAllowsShort(sc) {
		fmt.Fprintf(os.Stderr, "error: strategy %q direction=%q does not allow shorts (set direction to %q or %q)\n", strategyID, EffectiveDirection(sc), DirectionShort, DirectionBoth)
		return 1
	}
	if *side == "long" && !PerpsAllowsLong(sc) {
		fmt.Fprintf(os.Stderr, "error: strategy %q direction=%q does not allow longs (set direction to %q or %q)\n", strategyID, EffectiveDirection(sc), DirectionLong, DirectionBoth)
		return 1
	}

	sizingInputs := countSizingFlags(*size, *notional, *margin)
	if sizingInputs == 0 && !*recordOnly {
		*margin = cfg.resolveManualMarginUSD()
		sizingInputs = 1
		fmt.Fprintf(os.Stderr, "[manual-open] no sizing flag provided; defaulting to --margin %g\n", *margin)
	}
	if sizingInputs == 0 {
		fmt.Fprintln(os.Stderr, "error: one of --size, --notional, or --margin is required")
		return 2
	}
	if sizingInputs > 1 {
		fmt.Fprintln(os.Stderr, "error: only one of --size, --notional, or --margin may be specified")
		return 2
	}

	if *recordOnly {
		if *size <= 0 {
			fmt.Fprintln(os.Stderr, "error: --record-only requires --size (coin qty of the fill you placed)")
			return 2
		}
		if *fillPrice <= 0 {
			fmt.Fprintln(os.Stderr, "error: --record-only requires --fill-price (the price at which your fill executed)")
			return 2
		}
	}

	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	// Fix #4: guard against placing into a kill-switched or CB-pending account.
	if !*dryRun {
		state, loadErr := LoadStateWithDB(cfg, stateDB)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not load state for safety check: %v\n", loadErr)
		} else {
			if state.PortfolioRisk.KillSwitchActive {
				fmt.Fprintln(os.Stderr, "error: portfolio kill switch is active — manual-open blocked (use manual-close to flatten)")
				return 1
			}
			if ss := state.Strategies[strategyID]; ss != nil {
				if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
					fmt.Fprintln(os.Stderr, "error: strategy has a pending circuit-breaker close — manual-open blocked")
					return 1
				}
			}
		}
	}

	// ATR plausibility guard: mirror stampEntryATRIfOpened's 50%-of-AvgCost check.
	// We don't have fillPrice yet for live orders so defer to post-fill; for
	// --record-only we can check immediately.
	entryATR := *atr
	if *recordOnly && entryATR > 0 && *fillPrice > 0 && entryATR > 0.5**fillPrice {
		fmt.Fprintf(os.Stderr, "error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)\n", entryATR, *fillPrice)
		return 1
	}

	openSide := "buy"
	if *side == "short" {
		openSide = "sell"
	}

	effectiveSLPct := 0.0
	if *slPct > 0 {
		effectiveSLPct = *slPct
	}

	script := sc.Script

	// #711: --margin/--notional need a price to resolve to coin qty; passing
	// price=0 to resolveManualSize returns 0 and HL rejects the order with
	// "--size must be > 0". Fetch the current HL mid as the price reference
	// (market orders fill at ~mid). --size and --record-only paths skip the
	// fetch since size is explicit.
	var resolvedOrderSize, sizingMark float64
	var sizingFailed bool
	if !*recordOnly {
		qty, mark, err := resolveManualOpenOrderSize(sc, *size, *notional, *margin, fetchHyperliquidMids)
		if err != nil {
			if *dryRun {
				fmt.Fprintf(os.Stderr, "warning: dry-run sizing best-effort failed: %v\n", err)
				sizingFailed = true
			} else {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return 1
			}
		}
		resolvedOrderSize = qty
		sizingMark = mark
	}

	var resolvedFillPrice, fillQty, fillFee float64
	var exchangeOID string

	if *dryRun {
		prefix := "[dry-run]"
		if sizingFailed {
			prefix = "[dry-run] [sizing failed]"
		}
		fmt.Printf("%s manual-open %s: %s %.6f %s (script=%s, sl_pct=%.2f, mark=$%.4f)\n",
			prefix, strategyID, *side, resolvedOrderSize, sc.Symbol, script, effectiveSLPct, sizingMark)
		return 0
	}

	if *recordOnly {
		// Operator already placed the fill on the exchange UI.
		fillQty = *size
		resolvedFillPrice = *fillPrice
		// ATR post-fill plausibility (same guard as above, unified path)
		if entryATR > 0 && entryATR > 0.5*resolvedFillPrice {
			fmt.Fprintf(os.Stderr, "error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)\n", entryATR, resolvedFillPrice)
			return 1
		}
		// --record-only does not auto-arm the SL trigger (the operator placed
		// the fill on the UI, so they're responsible for its protection).
		// Warn if the operator passed SL-related flags that won't take effect.
		if *slATRMult > 0 || *slPct > 0 || (sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0) {
			fmt.Fprintln(os.Stderr, "warning: --record-only does not arm a stop-loss trigger automatically — place the SL manually on the HL UI")
		}
	} else {
		execResult, execStderr, execErr := RunHyperliquidExecute(
			script, sc.Symbol, openSide,
			resolvedOrderSize,
			effectiveSLPct, 0, 0, sc.MarginMode, sc.Leverage, false,
			hlExecuteSnapshot{},
		)
		if execStderr != "" {
			fmt.Fprintf(os.Stderr, "HL execute stderr: %s\n", execStderr)
		}
		if execErr != nil {
			fmt.Fprintf(os.Stderr, "error placing order: %v\n", execErr)
			return 1
		}
		if execResult.Error != "" {
			fmt.Fprintf(os.Stderr, "error from HL: %s\n", execResult.Error)
			return 1
		}

		fill := execResult.Execution
		if fill == nil || fill.Fill == nil {
			fmt.Fprintln(os.Stderr, "error: no fill returned from execute")
			return 1
		}
		resolvedFillPrice = fill.Fill.AvgPx
		fillQty = fill.Fill.TotalSz
		fillFee = fill.Fill.Fee
		if fill.Fill.OID != 0 {
			exchangeOID = fmt.Sprintf("%d", fill.Fill.OID)
		}
		if fillQty <= 0 {
			fillQty = resolveManualSize(*size, *notional, *margin, resolvedFillPrice, sc.Leverage)
		}

		// Post-fill ATR plausibility guard.
		if entryATR > 0 && resolvedFillPrice > 0 && entryATR > 0.5*resolvedFillPrice {
			fmt.Fprintf(os.Stderr, "warning: --atr %.4f exceeds 50%% of fill price %.4f — EntryATR will not be stamped\n", entryATR, resolvedFillPrice)
			entryATR = 0
		}
	}

	fmt.Printf("Filled: %s %.6f %s @ $%.4f (fee=$%.4f)\n", *side, fillQty, sc.Symbol, resolvedFillPrice, fillFee)

	// Build notifier for warning paths (no-op when Discord/Telegram not configured).
	notifier, closeNotifier := buildNotifierFromConfig(cfg)
	defer closeNotifier()

	effectiveATRMult := *slATRMult
	if effectiveATRMult == 0 && sc.StopLossATRMult != nil {
		effectiveATRMult = *sc.StopLossATRMult
	}

	// When --atr is omitted, fetch ATR from the same OHLCV/period strategy opens
	// see via stampEntryATRIfOpened (#689). On fetch failure, fall back to the
	// leverage-aware heuristic (0.1*fillPrice/leverage = ~10% margin risk at 1× ATR).
	// Collapses fetch-failure + fallback into a single notifier message so one
	// event = one Discord/Telegram alert.
	if !*recordOnly && entryATR == 0 {
		needsATRProtection := effectiveATRMult > 0 || strategyUsesTieredTPATRClose(sc)
		if needsATRProtection {
			fetched, fetchErr, fetchedOK := fetchManualEntryATR(sc)
			if fetchedOK {
				// Mirror stampEntryATRIfOpened's 50%-of-AvgCost plausibility guard.
				if resolvedFillPrice > 0 && fetched > 0.5*resolvedFillPrice {
					fetchErr = fmt.Sprintf("fetched ATR=%.6f exceeds 50%% of fill price %.4f", fetched, resolvedFillPrice)
					fetchedOK = false
				} else {
					entryATR = fetched
					fmt.Fprintf(os.Stderr, "[manual-open] %s %s: --atr omitted; auto-fetched ATR=%.6f (period=14, %s)\n",
						strategyID, sc.Symbol, fetched, sc.Timeframe)
				}
			}
			if !fetchedOK {
				if fb, ok := computeFallbackATR(resolvedFillPrice, sc.Leverage); ok {
					entryATR = fb
					warnNotifier(notifier, fmt.Sprintf(
						"[manual-open] %s %s: ATR auto-fetch failed (%s); using fallback ATR=%.6f (0.1*%.4f/%.2f lev) — pass --atr explicitly for accuracy",
						strategyID, sc.Symbol, fetchErr, fb, resolvedFillPrice, sc.Leverage))
				} else {
					warnNotifier(notifier, fmt.Sprintf(
						"[manual-open] %s %s: ATR auto-fetch failed (%s) and leverage<=0 — cannot compute fallback; position is NAKED (no ATR-based SL/TP)",
						strategyID, sc.Symbol, fetchErr))
				}
			}
		}
	}

	// Arm ATR-based stop-loss after fill (separate from the execute call so we
	// control trigger placement independently of the pct-based SL path).
	var stopLossOID int64
	var stopLossTriggerPx float64

	if effectiveATRMult > 0 && entryATR > 0 && !*recordOnly {
		if *side == "long" {
			stopLossTriggerPx = resolvedFillPrice - effectiveATRMult*entryATR
		} else {
			stopLossTriggerPx = resolvedFillPrice + effectiveATRMult*entryATR
		}
		if stopLossTriggerPx > 0 {
			slResult, slStderr, slErr := RunHyperliquidUpdateStopLoss(script, sc.Symbol, *side, fillQty, stopLossTriggerPx, 0)
			if slStderr != "" {
				fmt.Fprintf(os.Stderr, "SL arm stderr: %s\n", slStderr)
			}
			if slErr != nil {
				fmt.Fprintf(os.Stderr, "warning: SL placement failed: %v (position is open but unprotected)\n", slErr)
			} else if slResult.Error != "" {
				fmt.Fprintf(os.Stderr, "warning: SL arm error: %s\n", slResult.Error)
			} else {
				stopLossOID = slResult.StopLossOID
				stopLossTriggerPx = slResult.StopLossTriggerPx
				fmt.Printf("Stop-loss armed at $%.4f (OID=%d)\n", stopLossTriggerPx, stopLossOID)
			}
		}
	}

	// Place TP[n] reduce-only orders inline immediately after the fill so the
	// position is fully protected before the next scheduler cycle.
	// Note: if the strategy has no tiered close AND no ATR-based SL configured,
	// no warning fires here — that is intentional (no ATR protection requested).
	var tpOIDs []int64
	if !*recordOnly && strategyUsesTieredTPATRClose(sc) && entryATR > 0 {
		oids, warn, err := placeManualProtectionInline(sc, *side, fillQty, resolvedFillPrice, entryATR, effectiveATRMult, stopLossOID)
		if err != nil || warn != "" {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: TP placement issue (position open with SL only): err=%v warn=%s",
				strategyID, sc.Symbol, err, warn))
		}
		tpOIDs = oids
		if len(oids) > 0 {
			fmt.Printf("Take-profits armed: OIDs=%v\n", oids)
		}
	}

	action := PendingManualAction{
		StrategyID:        strategyID,
		Action:            "open",
		Symbol:            sc.Symbol,
		Side:              *side,
		Quantity:          fillQty,
		FillPrice:         resolvedFillPrice,
		FillFee:           fillFee,
		ExchangeOrderID:   exchangeOID,
		StopLossOID:       stopLossOID,
		StopLossTriggerPx: stopLossTriggerPx,
		EntryATR:          entryATR,
		TPOIDs:            tpOIDs,
		CreatedAt:         time.Now().UTC(),
	}
	if err := stateDB.InsertPendingManualAction(action); err != nil {
		// On-chain fill (and SL/TPs) succeeded but the queue insert failed —
		// the scheduler will never adopt this position, so reconcile would see
		// an unowned on-chain position with orphaned reduce-only triggers.
		// Skip cleanup in --record-only because the operator's pre-existing
		// fill is theirs to manage; we never placed those on-chain orders.
		if *recordOnly {
			fmt.Fprintf(os.Stderr, "error queuing action: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "CRITICAL: queue insert failed (%v); on-chain position is open but the scheduler cannot adopt it. Attempting cleanup...\n", err)
		cleanedUp, cleanupMsg := attemptManualOpenCleanup(sc.Symbol, fillQty, stopLossOID, tpOIDs)
		if cleanedUp {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: queue insert failed (%v); position auto-flattened: %s",
				strategyID, sc.Symbol, err, cleanupMsg))
		} else {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: queue insert failed (%v) AND auto-flatten failed: %s — MANUAL INTERVENTION REQUIRED on HL UI (side=%s qty=%.6f sl_oid=%d tp_oids=%v)",
				strategyID, sc.Symbol, err, cleanupMsg, *side, fillQty, stopLossOID, tpOIDs))
		}
		return 1
	}

	fmt.Printf("Queued: %s position will appear in the dashboard after the next scheduler cycle.\n", strategyID)
	return 0
}

// runManualClose implements `go-trader manual-close <strategy-id>`.
func runManualClose(args []string) int {
	fs := flag.NewFlagSet("manual-close", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	qty := fs.Float64("qty", 0, "Quantity to close in base units (0 = full position)")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-close <strategy-id> [--qty N] [--dry-run]")
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

	state, err := LoadStateWithDB(cfg, stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		return 1
	}
	ss := state.Strategies[strategyID]
	pos := ss.Positions[sc.Symbol]
	if pos == nil {
		fmt.Fprintf(os.Stderr, "error: no open position found for %s/%s\n", strategyID, sc.Symbol)
		return 1
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) {
		fmt.Fprintf(os.Stderr, "error: position %s/%s is owned by %q, not %q\n", strategyID, sc.Symbol, pos.OwnerStrategyID, strategyID)
		return 1
	}

	// Operator intent: --qty omitted (or equal to the full position) is a full
	// close; any smaller value is a partial close. We track this explicitly
	// rather than inferring from the eventual fill quantity, since lot-size
	// rounding can otherwise collapse a deliberate ~99% partial into a full.
	closeQty := pos.Quantity
	intentFullClose := true
	if *qty > 0 {
		if *qty > pos.Quantity {
			fmt.Fprintf(os.Stderr, "error: --qty %.6f exceeds open position %.6f\n", *qty, pos.Quantity)
			return 1
		}
		closeQty = *qty
		// Within 0.0001 (typical HL lot size) is treated as full close.
		if pos.Quantity-*qty > 0.0001 {
			intentFullClose = false
		}
	}

	closeSide := "sell"
	if pos.Side == "short" {
		closeSide = "buy"
	}

	if *dryRun {
		fmt.Printf("[dry-run] manual-close %s: %s %.6f %s (current pos=%.6f, avg_cost=$%.4f)\n",
			strategyID, closeSide, closeQty, sc.Symbol, pos.Quantity, pos.AvgCost)
		return 0
	}

	// Fix #2: only cancel the SL on a full close; leave it resting on partial close.
	cancelOID := int64(0)
	if intentFullClose {
		cancelOID = pos.StopLossOID
	}
	closeFullPosition := shouldCloseFullPosition(
		manualCloseIntentFraction(intentFullClose, closeQty, pos.Quantity),
		sc.Symbol,
		hyperliquidCloseScopeStrategies(cfg.Strategies),
	)
	var extraCancelOIDs []int64
	if intentFullClose {
		extraCancelOIDs = cloneInt64s(pos.TPOIDs)
	}

	execResult, stderr, execErr := RunHyperliquidExecute(
		sc.Script, sc.Symbol, closeSide, closeQty,
		0, cancelOID, 0, "", 0, closeFullPosition, hlExecuteSnapshot{}, extraCancelOIDs...,
	)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "HL close stderr: %s\n", stderr)
	}
	if execErr != nil {
		fmt.Fprintf(os.Stderr, "error placing close order: %v\n", execErr)
		return 1
	}
	if execResult.Error != "" {
		fmt.Fprintf(os.Stderr, "error from HL: %s\n", execResult.Error)
		return 1
	}
	// Cancel failures are non-fatal but leave reduce-only OIDs resting
	// on-chain after the strategy is virtually flat — surface them so the
	// operator can verify TP/SL state on HL.
	if execResult.CancelStopLossError != "" {
		fmt.Fprintf(os.Stderr,
			"warning: manual close cancel failed (non-fatal) for %s/%s: %s (sl_oid=%d tp_oids=%v) — verify HL on-chain triggers\n",
			strategyID, sc.Symbol, execResult.CancelStopLossError, cancelOID, extraCancelOIDs)
	}

	fill := execResult.Execution
	if fill == nil || fill.Fill == nil {
		fmt.Fprintln(os.Stderr, "error: no fill returned from close execute")
		return 1
	}

	fillAvgPx := fill.Fill.AvgPx
	fillFee := fill.Fill.Fee
	var exchangeOID string
	if fill.Fill.OID != 0 {
		exchangeOID = fmt.Sprintf("%d", fill.Fill.OID)
	}

	var realizedPnL float64
	if pos.Side == "long" {
		realizedPnL = closeQty * (fillAvgPx - pos.AvgCost)
	} else {
		realizedPnL = closeQty * (pos.AvgCost - fillAvgPx)
	}
	realizedPnL -= fillFee

	fmt.Printf("Closed: %.6f %s @ $%.4f | PnL=$%.2f (fee=$%.4f)\n",
		closeQty, sc.Symbol, fillAvgPx, realizedPnL, fillFee)

	action := PendingManualAction{
		StrategyID:      strategyID,
		Action:          "close",
		Symbol:          sc.Symbol,
		Side:            closeSide,
		Quantity:        closeQty,
		FillPrice:       fillAvgPx,
		FillFee:         fillFee,
		ExchangeOrderID: exchangeOID,
		RealizedPnL:     realizedPnL,
		IsFullClose:     intentFullClose,
		CreatedAt:       time.Now().UTC(),
	}
	if err := stateDB.InsertPendingManualAction(action); err != nil {
		fmt.Fprintf(os.Stderr, "error queuing close action: %v\n", err)
		return 1
	}

	fmt.Printf("Queued: close will be reflected in the dashboard after the next scheduler cycle.\n")
	return 0
}

// drainPendingManualActions reads all rows from pending_manual_actions and
// applies them to the in-memory AppState, then deletes the drained rows.
// Called at the top of each scheduler cycle before dueStrategies is built.
func drainPendingManualActions(state *AppState, cfg *Config, stateDB *StateDB) {
	if stateDB == nil {
		return
	}
	actions, err := stateDB.LoadPendingManualActions()
	if err != nil {
		fmt.Printf("[manual] failed to load pending actions: %v\n", err)
		return
	}
	if len(actions) == 0 {
		return
	}

	scByID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		scByID[sc.ID] = sc
	}

	var maxDrained int64
	for _, a := range actions {
		if err := applyManualAction(state, scByID, a); err != nil {
			fmt.Printf("[manual] failed to apply action %d (%s %s): %v\n", a.ID, a.Action, a.StrategyID, err)
			continue
		}
		if a.ID > maxDrained {
			maxDrained = a.ID
		}
	}

	if maxDrained > 0 {
		if err := stateDB.DeletePendingManualActionsThrough(maxDrained); err != nil {
			fmt.Printf("[manual] failed to delete drained actions: %v\n", err)
		}
	}
}

// applyManualAction materialises one pending_manual_actions row into AppState.
func applyManualAction(state *AppState, scByID map[string]StrategyConfig, a PendingManualAction) error {
	sc, hasSC := scByID[a.StrategyID]
	if !hasSC {
		return fmt.Errorf("strategy %q not found in config", a.StrategyID)
	}
	if sc.Type != "manual" {
		return fmt.Errorf("strategy %q is not type=manual", a.StrategyID)
	}

	ss := state.Strategies[a.StrategyID]
	if ss == nil {
		return fmt.Errorf("strategy state for %q not found", a.StrategyID)
	}

	now := a.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	switch a.Action {
	case "open":
		if _, exists := ss.Positions[a.Symbol]; exists {
			return fmt.Errorf("position already open for %s/%s; close it first", a.StrategyID, a.Symbol)
		}
		pos := &Position{
			Symbol:            a.Symbol,
			Quantity:          a.Quantity,
			InitialQuantity:   a.Quantity,
			AvgCost:           a.FillPrice,
			EntryATR:          a.EntryATR,
			Side:              a.Side,
			Multiplier:        1, // perps
			Leverage:          sc.Leverage,
			OwnerStrategyID:   a.StrategyID,
			OpenedAt:          now,
			StopLossOID:       a.StopLossOID,
			StopLossTriggerPx: a.StopLossTriggerPx,
			TPOIDs:            a.TPOIDs,
		}
		pos.TradePositionID = newTradePositionID(a.StrategyID, a.Symbol, now)
		ss.Positions[a.Symbol] = pos

		trade := Trade{
			Timestamp:         now,
			StrategyID:        a.StrategyID,
			Symbol:            a.Symbol,
			Side:              openTradeSide(a.Side),
			Quantity:          a.Quantity,
			Price:             a.FillPrice,
			Value:             a.Quantity * a.FillPrice,
			TradeType:         "perps",
			Details:           fmt.Sprintf("manual open %s %s @ $%.4f", a.Side, a.Symbol, a.FillPrice),
			PositionID:        pos.TradePositionID,
			ExchangeOrderID:   a.ExchangeOrderID,
			ExchangeFee:       a.FillFee,
			EntryATR:          a.EntryATR,
			StopLossOID:       a.StopLossOID,
			StopLossTriggerPx: a.StopLossTriggerPx,
			TPOIDs:            cloneInt64s(a.TPOIDs),
			Manual:            true,
		}
		recordPositionOpen(ss, sc, &trade, pos)
		// Fix #1: perps open deducts only the fee; notional stays virtual.
		ss.Cash -= a.FillFee
		fmt.Printf("[manual] applied open: %s %s %.6f %s @ $%.4f\n",
			a.StrategyID, a.Side, a.Quantity, a.Symbol, a.FillPrice)

	case "close":
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s", a.StrategyID, a.Symbol)
		}
		if !manualPositionOwnedByStrategy(pos, a.StrategyID) {
			return fmt.Errorf("position %s/%s is owned by %q, not %q", a.StrategyID, a.Symbol, pos.OwnerStrategyID, a.StrategyID)
		}
		// Use the explicit IsFullClose intent flag rather than a tolerance
		// heuristic, so a deliberate 99% partial close isn't silently
		// collapsed into a full close.
		closedFull := a.IsFullClose
		side := closeTradeSide(pos.Side)

		trade := Trade{
			Timestamp:       now,
			StrategyID:      a.StrategyID,
			Symbol:          a.Symbol,
			Side:            side,
			Quantity:        a.Quantity,
			Price:           a.FillPrice,
			Value:           a.Quantity * a.FillPrice,
			TradeType:       "perps",
			Details:         fmt.Sprintf("manual close %s @ $%.4f | PnL=$%.2f", a.Symbol, a.FillPrice, a.RealizedPnL),
			PositionID:      ensurePositionTradeID(a.StrategyID, a.Symbol, pos),
			ExchangeOrderID: a.ExchangeOrderID,
			ExchangeFee:     a.FillFee,
			IsClose:         true,
			RealizedPnL:     a.RealizedPnL,
			Manual:          true,
		}
		RecordTrade(ss, trade)
		// Fix #1: perps close credits only the realized PnL; notional was never debited.
		ss.Cash += a.RealizedPnL

		if closedFull {
			recordClosedPosition(ss, pos, a.FillPrice, a.RealizedPnL, "manual_close", now)
			delete(ss.Positions, a.Symbol)
		} else {
			pos.Quantity -= a.Quantity
		}
		fmt.Printf("[manual] applied close: %s %.6f %s @ $%.4f | PnL=$%.2f\n",
			a.StrategyID, a.Quantity, a.Symbol, a.FillPrice, a.RealizedPnL)

	default:
		return fmt.Errorf("unknown action %q", a.Action)
	}
	return nil
}

// findManualStrategy locates a type=manual strategy by ID in the config,
// printing a clear error if not found or wrong type.
func findManualStrategy(cfg *Config, id string) (StrategyConfig, bool) {
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			if sc.Type != "manual" {
				fmt.Fprintf(os.Stderr, "error: strategy %q has type=%q; manual-open/close only works with type=manual strategies\n", id, sc.Type)
				return StrategyConfig{}, false
			}
			return sc, true
		}
	}
	fmt.Fprintf(os.Stderr, "error: strategy %q not found in config\n", id)
	return StrategyConfig{}, false
}

// collectBoolFlagNames returns the names of bool flags registered on fs.
// reorderArgsForPositional uses this to avoid consuming the strategy-id
// positional as the value of a preceding bool flag. Derived from the FlagSet
// (rather than a hardcoded map) so new bool flags self-register.
func collectBoolFlagNames(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		type boolFlag interface{ IsBoolFlag() bool }
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			out[f.Name] = true
		}
	})
	return out
}

// reorderArgsForPositional moves positional (non-flag) arguments to the end
// so Go's stdlib flag.Parse — which stops at the first non-flag — can still
// parse flags placed after a positional. This makes both invocation styles
// work for `manual-open` / `manual-close` (#711):
//
//	manual-open <strategy-id> --flag value
//	manual-open --flag value <strategy-id>
//
// boolFlags lists flags that take no value (so we don't consume the next arg
// as their value when it is actually the positional).
func reorderArgsForPositional(args []string, boolFlags map[string]bool) []string {
	var flagArgs, positional []string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if strings.Contains(a, "=") {
				i++
				continue
			}
			name := strings.TrimLeft(a, "-")
			if !boolFlags[name] && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i += 2
				continue
			}
			i++
			continue
		}
		positional = append(positional, a)
		i++
	}
	return append(flagArgs, positional...)
}

// manualMarkFetcher matches fetchHyperliquidMids for dependency injection in
// tests of resolveManualOpenOrderSize.
type manualMarkFetcher func(coins []string) (map[string]float64, error)

// resolveManualOpenOrderSize converts --size/--margin/--notional inputs into a
// concrete coin qty for the HL execute call. --size is explicit; --margin and
// --notional need a price reference (HL mid) to compute the qty. Returns
// (qty, mark, err); on --size path mark is 0. (#711)
func resolveManualOpenOrderSize(sc StrategyConfig, size, notional, margin float64, fetch manualMarkFetcher) (float64, float64, error) {
	if size > 0 {
		return size, 0, nil
	}
	coin := hyperliquidConfiguredCoin(sc)
	if coin == "" {
		return 0, 0, fmt.Errorf("cannot determine HL coin for strategy %q (symbol=%q)", sc.ID, sc.Symbol)
	}
	marks, err := fetch([]string{coin})
	if err != nil {
		return 0, 0, fmt.Errorf("fetch HL mark for %s: %w", coin, err)
	}
	mark := marks[coin]
	if mark <= 0 {
		return 0, 0, fmt.Errorf("HL mark for %s missing or non-positive — cannot resolve --margin/--notional sizing", coin)
	}
	qty := resolveManualSize(size, notional, margin, mark, sc.Leverage)
	if qty <= 0 {
		return 0, mark, fmt.Errorf("resolved size is zero (size=%g notional=%g margin=%g mark=%g leverage=%g) — check --margin/--notional and strategy leverage", size, notional, margin, mark, sc.Leverage)
	}
	return qty, mark, nil
}

// resolveManualSize converts the sizing inputs to a coin qty.
// price=0 is acceptable for --size (qty is already explicit).
func resolveManualSize(size, notional, margin, price, leverage float64) float64 {
	if size > 0 {
		return size
	}
	if price <= 0 {
		return 0
	}
	if notional > 0 {
		return notional / price
	}
	if margin > 0 && leverage > 0 {
		return (margin * leverage) / price
	}
	return 0
}

func countSizingFlags(size, notional, margin float64) int {
	n := 0
	if size > 0 {
		n++
	}
	if notional > 0 {
		n++
	}
	if margin > 0 {
		n++
	}
	return n
}

// manualPositionOwnedByStrategy gates manual close paths on owner identity to
// prevent one manual strategy from flattening a peer's wallet exposure on a
// shared coin (#620). An empty OwnerStrategyID is treated as owned for
// backward-compat with positions opened before #569 stamped owners and with
// reconciler-discovered positions that have no recorded owner; tightening that
// further would silently strand pre-existing positions and break reconciler
// adoption. New manual paths must always stamp OwnerStrategyID.
func manualPositionOwnedByStrategy(pos *Position, strategyID string) bool {
	return pos == nil || pos.OwnerStrategyID == "" || pos.OwnerStrategyID == strategyID
}

func manualCloseIntentFraction(intentFullClose bool, closeQty, posQty float64) float64 {
	if intentFullClose {
		return 1.0
	}
	if posQty <= 0 {
		return 0
	}
	return closeQty / posQty
}

func hyperliquidCloseScopeStrategies(strategies []StrategyConfig) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(strategies))
	for _, sc := range strategies {
		if isHLLiveReconcilable(sc) {
			out = append(out, sc)
		}
	}
	return out
}

// openTradeSide converts a position side ("long"/"short") to the trade buy/sell side for an open.
func openTradeSide(posSide string) string {
	if posSide == "short" {
		return "sell"
	}
	return "buy"
}

// runManualCloseEval runs the close-evaluator loop for a single type=manual
// strategy that has an open position. Called from the main scheduler loop.
// Returns (closeFraction, closePrice, ok).
func runManualCloseEval(sc StrategyConfig, ss *StrategyState, cfg *Config, notifier *MultiNotifier, logger *StrategyLogger) (float64, float64, *float64, bool) {
	pos := ss.Positions[sc.Symbol]
	if pos == nil {
		return 0, 0, nil, true // flat — nothing to do
	}

	posCtx := positionCtxFromPosition(pos)
	result, _, price, ok := runHyperliquidCheck(&sc, nil, posCtx, cfg.Regime, notifier, logger)
	if !ok {
		return 0, 0, nil, false
	}
	// PostTPTrailingATRMult is set only by the trailing_tp_ratchet close family (#844).
	return result.CloseFraction, price, result.PostTPTrailingATRMult, true
}
