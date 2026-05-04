package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// runManualOpen implements `go-trader manual-open <strategy-id>`.
// It places an on-chain HL order (or records an existing fill with --record-only),
// then enqueues the fill in pending_manual_actions for the scheduler to drain.
func runManualOpen(args []string) int {
	fs := flag.NewFlagSet("manual-open", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	side := fs.String("side", "", "Position side: long or short")
	size := fs.Float64("size", 0, "Size in base units (coin qty)")
	notional := fs.Float64("notional", 0, "Size as USD notional (size = notional / price)")
	margin := fs.Float64("margin", 0, "Size as USD margin (size = margin * leverage / price)")
	atr := fs.Float64("atr", 0, "ATR value to stamp on the position (required for ATR-based stops when not auto-fetched)")
	slATRMult := fs.Float64("stop-loss-atr-mult", 0, "Override stop_loss_atr_mult for this position (0 = use strategy default)")
	slPct := fs.Float64("stop-loss-pct", 0, "Override stop_loss_pct for this position (0 = use strategy default)")
	fillPrice := fs.Float64("fill-price", 0, "Fill price for --record-only (required when --record-only is set)")
	recordOnly := fs.Bool("record-only", false, "Register an existing fill without placing a new on-chain order")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-open <strategy-id> --side long|short (--size N | --notional N | --margin N) [flags]")
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

	if strings.TrimSpace(*side) == "" {
		fmt.Fprintln(os.Stderr, "error: --side is required (long or short)")
		return 2
	}
	*side = strings.ToLower(strings.TrimSpace(*side))
	if *side != "long" && *side != "short" {
		fmt.Fprintf(os.Stderr, "error: --side must be \"long\" or \"short\", got %q\n", *side)
		return 2
	}
	if !sc.AllowShorts && *side == "short" {
		fmt.Fprintf(os.Stderr, "error: strategy %q does not allow shorts (set allow_shorts: true in config)\n", strategyID)
		return 1
	}

	sizingInputs := countSizingFlags(*size, *notional, *margin)
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

	var resolvedFillPrice, fillQty, fillFee float64
	var exchangeOID string

	if *dryRun {
		displayQty := resolveManualSize(*size, *notional, *margin, 0, sc.Leverage)
		fmt.Printf("[dry-run] manual-open %s: %s %.6f %s (script=%s, sl_pct=%.2f)\n",
			strategyID, *side, displayQty, sc.Symbol, script, effectiveSLPct)
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
	} else {
		execResult, execStderr, execErr := RunHyperliquidExecute(
			script, sc.Symbol, openSide,
			resolveManualSize(*size, *notional, *margin, 0, sc.Leverage),
			effectiveSLPct, 0, 0, sc.MarginMode, sc.Leverage,
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

	// Arm ATR-based stop-loss after fill (separate from the execute call so we
	// control trigger placement independently of the pct-based SL path).
	var stopLossOID int64
	var stopLossTriggerPx float64

	effectiveATRMult := *slATRMult
	if effectiveATRMult == 0 && sc.StopLossATRMult != nil {
		effectiveATRMult = *sc.StopLossATRMult
	}

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
	} else if effectiveATRMult > 0 && entryATR == 0 {
		fmt.Fprintln(os.Stderr, "warning: stop_loss_atr_mult is set but --atr was not provided; SL not armed")
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
		CreatedAt:         time.Now().UTC(),
	}
	if err := stateDB.InsertPendingManualAction(action); err != nil {
		fmt.Fprintf(os.Stderr, "error queuing action: %v\n", err)
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

	closeQty := pos.Quantity
	if *qty > 0 {
		if *qty > pos.Quantity {
			fmt.Fprintf(os.Stderr, "error: --qty %.6f exceeds open position %.6f\n", *qty, pos.Quantity)
			return 1
		}
		closeQty = *qty
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
	isFullClose := closeQty >= pos.Quantity*0.99
	cancelOID := int64(0)
	if isFullClose {
		cancelOID = pos.StopLossOID
	}

	execResult, stderr, execErr := RunHyperliquidExecute(
		sc.Script, sc.Symbol, closeSide, closeQty,
		0, cancelOID, 0, "", 0,
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
			StopLossTriggerPx: a.StopLossTriggerPx,
			Manual:            true,
		}
		RecordTrade(ss, trade)
		// Fix #1: perps open deducts only the fee; notional stays virtual.
		ss.Cash -= a.FillFee
		fmt.Printf("[manual] applied open: %s %s %.6f %s @ $%.4f\n",
			a.StrategyID, a.Side, a.Quantity, a.Symbol, a.FillPrice)

	case "close":
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s", a.StrategyID, a.Symbol)
		}
		// Fix #5: use 0.99 relative tolerance matching HL lot-size rounding semantics.
		closedFull := a.Quantity >= pos.Quantity*0.99
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
func runManualCloseEval(sc StrategyConfig, ss *StrategyState, cfg *Config, logger *StrategyLogger) (float64, float64, bool) {
	pos := ss.Positions[sc.Symbol]
	if pos == nil {
		return 0, 0, true // flat — nothing to do
	}

	posCtx := positionCtxFromPosition(pos)
	result, _, price, ok := runHyperliquidCheck(sc, nil, posCtx, cfg.Regime, logger)
	if !ok {
		return 0, 0, false
	}
	return result.CloseFraction, price, true
}
