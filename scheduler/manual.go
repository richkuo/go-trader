package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const defaultManualMarginUSD = 50.0

const defaultManualStopLossATRMult = 2.0

func runManualOpen(args []string) int {
	fs := flag.NewFlagSet("manual-open", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	side := fs.String("side", "", "Position side: long or short (default: \"long\", override via user_defaults.manual.side in config)")
	size := fs.Float64("size", 0, "Size in base units (coin qty)")
	notional := fs.Float64("notional", 0, "Size as USD notional (size = notional / price)")
	margin := fs.Float64("margin", 0, "Size as USD margin (size = margin * leverage / price)")
	atr := fs.Float64("atr", 0, "ATR value to stamp on the position (required for ATR-based stops when not auto-fetched)")
	slATRMult := fs.Float64("stop-loss-atr-mult", 0, "Override stop_loss_atr_mult for this position (0 = use strategy default)")
	slPct := fs.Float64("stop-loss-pct", 0, "Override stop_loss_pct for this position (0 = use strategy default)")
	fillPrice := fs.Float64("fill-price", 0, "Fill price for --record-only (required when --record-only is set)")
	limitPrice := fs.Float64("limit-price", 0, "Place a resting limit order at this price instead of a market order (#883). The scheduler tracks fills and arms protection post-fill.")
	tif := fs.String("tif", "Alo", "Time-in-force for --limit-price: Alo=post-only maker (default, rejects a crossed price) or Gtc=allow immediate marketable fill")
	expireAfter := fs.Duration("expire-after", 0, "Auto-cancel a resting --limit-price order after this duration (e.g. 2h, 30m); 0 = GTC, no expiry")
	recordOnly := fs.Bool("record-only", false, "Register an existing fill without placing a new on-chain order")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

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

	if *limitPrice > 0 {
		resolvedSide, openSide, sideErr := resolveManualOpenSide(cfg, sc, *side)
		if sideErr != nil {
			fmt.Fprintln(os.Stderr, sideErr.Error())
			return manualCoreExitCode(sideErr)
		}
		if *recordOnly {
			fmt.Fprintln(os.Stderr, "error: --limit-price cannot be combined with --record-only (a resting order has no fill to record yet)")
			return 2
		}
		resolvedMargin, marginDefaulted, sizeErr := validateManualSizing(cfg, *size, *notional, *margin, false)
		if sizeErr != nil {
			fmt.Fprintln(os.Stderr, sizeErr.Error())
			return manualCoreExitCode(sizeErr)
		}
		if marginDefaulted {
			fmt.Fprintf(os.Stderr, "[manual-open] no sizing flag provided; defaulting to --margin %g\n", resolvedMargin)
		}
		if *tif != "Alo" && *tif != "Gtc" {
			fmt.Fprintf(os.Stderr, "error: --tif must be Alo or Gtc, got %q\n", *tif)
			return 2
		}
		if *expireAfter < 0 {
			fmt.Fprintln(os.Stderr, "error: --expire-after must be non-negative")
			return 2
		}

		stateDB, err := OpenStateDB(cfg.DBFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
			return 1
		}
		defer stateDB.Close()

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
				if st := evaluateDailyLossLimit(cfg.PortfolioRisk, state.Strategies, cfg.Strategies, time.Now().UTC()); st.Tripped {
					fmt.Fprintf(os.Stderr, "error: %s — manual-open blocked until UTC rollover (closes and SL edits are unaffected)\n", dailyLossHoldDetail(st))
					return 1
				}
				if held, detail := evaluateNotionalCapHold(cfg.PortfolioRisk, state.Strategies, nil); held {
					fmt.Fprintf(os.Stderr, "error: %s — manual-open blocked (closes and SL edits are unaffected)\n", detail)
					return 1
				}
				capSt := manualExposureCapStatus(cfg, state)
				if blocked, why := exposureCapManualEntryBlock(capSt, extractAsset(sc), resolvedSide); blocked {
					fmt.Fprintf(os.Stderr, "error: %s — manual limit-open (%s) blocked (closes and SL edits are unaffected)\n", why, resolvedSide)
					return 1
				}
				if capSt.PVBasisMiss {
					fmt.Fprintf(os.Stderr, "warning: %s\n", exposureCapPVBasisMissWarning)
				}
			}
		}

		return runManualLimitOpen(cfg, sc, stateDB, manualLimitOpenInputs{
			strategyID:  strategyID,
			side:        resolvedSide,
			openSide:    openSide,
			size:        *size,
			notional:    *notional,
			margin:      resolvedMargin,
			limitPrice:  *limitPrice,
			tif:         *tif,
			atr:         *atr,
			slATRMult:   *slATRMult,
			slPct:       *slPct,
			expireAfter: *expireAfter,
			dryRun:      *dryRun,
		})
	}

	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	notifier, closeNotifier := buildNotifierFromConfig(cfg)
	defer closeNotifier()

	res, coreErr := manualOpenCore(newCLIManualCoreDeps(cfg, stateDB, notifier), sc, manualOpenInputs{
		StrategyID: strategyID,
		Side:       *side,
		Size:       *size,
		Notional:   *notional,
		Margin:     *margin,
		ATR:        *atr,
		SLATRMult:  *slATRMult,
		SLPct:      *slPct,
		RecordOnly: *recordOnly,
		FillPrice:  *fillPrice,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

func runManualAdd(args []string) int {
	fs := flag.NewFlagSet("manual-add", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	size := fs.Float64("size", 0, "Add size in base units (coin qty)")
	notional := fs.Float64("notional", 0, "Add size as USD notional (size = notional / price)")
	margin := fs.Float64("margin", 0, "Add size as USD margin (size = margin * leverage / price)")
	fillPrice := fs.Float64("fill-price", 0, "Fill price for --record-only (required when --record-only is set)")
	recordOnly := fs.Bool("record-only", false, "Register an existing same-side add fill without placing a new on-chain order")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-add <strategy-id> [--size N | --notional N | --margin N] [--record-only --size N --fill-price P] [flags]")
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

	res, coreErr := manualAddCore(newCLIManualCoreDeps(cfg, stateDB, nil), sc, manualAddInputs{
		StrategyID: strategyID,
		Size:       *size,
		Notional:   *notional,
		Margin:     *margin,
		RecordOnly: *recordOnly,
		FillPrice:  *fillPrice,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

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

	res, coreErr := manualCloseCore(newCLIManualCoreDeps(cfg, stateDB, nil), sc, manualCloseInputs{
		StrategyID: strategyID,
		Qty:        *qty,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

func runForceClose(args []string) int {
	return runForceCloseWithCloser(args, defaultHyperliquidForceCloseCloser)
}

func runForceCloseWithCloser(args []string, closer HyperliquidLiveCloser) int {
	fs := flag.NewFlagSet("force-close", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	qty := fs.Float64("qty", 0, "Quantity to close in base units (0 = full strategy position)")
	dryRun := fs.Bool("dry-run", false, "Print planned action without placing order or mutating state")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader force-close <strategy-id> [--qty N] [--dry-run]")
		return 2
	}
	strategyID := fs.Arg(0)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}

	sc, sym, ok := findForceCloseStrategy(cfg, strategyID)
	if !ok {
		return 1
	}

	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	deps := newCLIManualCoreDeps(cfg, stateDB, nil)
	deps.closer = closer
	res, coreErr := forceCloseCore(deps, sc, sym, forceCloseInputs{
		StrategyID: strategyID,
		Qty:        *qty,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

func printManualCoreOutcome(res *manualCoreResult, err error) int {
	if res != nil {
		for _, l := range res.lines {
			if l.stderr {
				fmt.Fprintln(os.Stderr, l.text)
			} else {
				fmt.Println(l.text)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return manualCoreExitCode(err)
	}
	return 0
}

type manualAlert struct {
	sc     StrategyConfig
	ss     *StrategyState
	trades int
}

func drainPendingManualActions(state *AppState, cfg *Config, stateDB *StateDB) []manualAlert {
	if stateDB == nil {
		return nil
	}
	actions, err := stateDB.LoadPendingManualActions()
	if err != nil {
		fmt.Printf("[manual] failed to load pending actions: %v\n", err)
		return nil
	}
	if len(actions) == 0 {
		return nil
	}

	scByID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		scByID[sc.ID] = sc
	}

	var maxDrained int64
	applied := make(map[string]*manualAlert)
	var order []string
	for _, a := range actions {
		if err := applyManualAction(state, cfg, scByID, a); err != nil {
			fmt.Printf("[manual] failed to apply action %d (%s %s): %v\n", a.ID, a.Action, a.StrategyID, err)
			continue
		}
		if a.ID > maxDrained {
			maxDrained = a.ID
		}
		if !manualActionRecordsTrade(a.Action) {
			continue
		}
		ma := applied[a.StrategyID]
		if ma == nil {
			ma = &manualAlert{sc: scByID[a.StrategyID], ss: state.Strategies[a.StrategyID]}
			applied[a.StrategyID] = ma
			order = append(order, a.StrategyID)
		}
		ma.trades++
	}

	if maxDrained > 0 {
		if err := stateDB.DeletePendingManualActionsThrough(maxDrained); err != nil {
			fmt.Printf("[manual] failed to delete drained actions: %v\n", err)
		}
	}

	alerts := make([]manualAlert, 0, len(order))
	for _, id := range order {
		alerts = append(alerts, *applied[id])
	}
	return alerts
}

func applyManualAction(state *AppState, cfg *Config, scByID map[string]StrategyConfig, a PendingManualAction) error {
	sc, hasSC := scByID[a.StrategyID]
	if !hasSC {
		return fmt.Errorf("strategy %q not found in config", a.StrategyID)
	}
	if err := validatePendingManualActionStrategy(sc, a); err != nil {
		return err
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
			Symbol:                          a.Symbol,
			Quantity:                        a.Quantity,
			InitialQuantity:                 a.Quantity,
			AvgCost:                         a.FillPrice,
			EntryATR:                        a.EntryATR,
			Side:                            a.Side,
			Multiplier:                      1,
			Leverage:                        sc.Leverage,
			OwnerStrategyID:                 a.StrategyID,
			OpenedAt:                        now,
			StopLossOID:                     a.StopLossOID,
			StopLossTriggerPx:               a.StopLossTriggerPx,
			TPOIDs:                          a.TPOIDs,
			RatchetFallbackNormalizePending: a.RatchetFallbackNormalizePending,
		}
		pos.ATRMethodAtOpen = normalizeATRMethod(a.ATRMethod)
		if pos.ATRMethodAtOpen == "" {
			pos.ATRMethodAtOpen = resolveATRMethod(sc, cfg)
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
			FeeSource:         FeeSourceUserFills,
			PnLGross:          true,
			EntryATR:          a.EntryATR,
			StopLossOID:       a.StopLossOID,
			StopLossTriggerPx: a.StopLossTriggerPx,
			TPOIDs:            cloneInt64s(a.TPOIDs),
			Manual:            true,
		}
		recordPositionOpen(ss, sc, &trade, pos)
		ss.Cash -= a.FillFee
		fmt.Printf("[manual] applied open: %s %s %.6f %s @ $%.4f\n",
			a.StrategyID, a.Side, a.Quantity, a.Symbol, a.FillPrice)

	case "close":
		if a.ExchangeOrderID != "" && strategyHasCloseTradeForOID(ss, a.ExchangeOrderID) {
			if sc.Type != "manual" {
				if pos, ok := ss.Positions[a.Symbol]; ok && pos != nil {
					clearForceCloseCanceledProtectionOIDs(pos, a.StopLossOID, a.TPOIDs)
				}
			}
			fmt.Printf("[manual] skipped duplicate close: %s %s oid=%s already booked\n",
				a.StrategyID, a.Symbol, a.ExchangeOrderID)
			return nil
		}
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s", a.StrategyID, a.Symbol)
		}
		if !manualPositionOwnedByStrategy(pos, a.StrategyID) {
			return fmt.Errorf("position %s/%s is owned by %q, not %q", a.StrategyID, a.Symbol, pos.OwnerStrategyID, a.StrategyID)
		}
		closedFull := a.IsFullClose
		side := closeTradeSide(pos.Side)
		closeLabel := operatorCloseLabel(sc)

		trade := Trade{
			Timestamp:       now,
			StrategyID:      a.StrategyID,
			Symbol:          a.Symbol,
			Side:            side,
			Quantity:        a.Quantity,
			Price:           a.FillPrice,
			Value:           a.Quantity * a.FillPrice,
			TradeType:       manualCloseTradeType(pos),
			Details:         fmt.Sprintf("%s %s @ $%.4f | PnL=$%.2f", closeLabel, a.Symbol, a.FillPrice, a.RealizedPnL),
			PositionID:      ensurePositionTradeID(a.StrategyID, a.Symbol, pos),
			ExchangeOrderID: a.ExchangeOrderID,
			ExchangeFee:     a.FillFee,
			FeeSource:       FeeSourceUserFills,
			IsClose:         true,
			RealizedPnL:     a.RealizedPnL + a.FillFee,
			PnLGross:        true,
			Manual:          sc.Type == "manual",
		}
		RecordTrade(ss, trade)
		if sc.Type != "manual" {
			recordPositionTradeResult(ss, pos, a.RealizedPnL)
		}
		ss.Cash += a.RealizedPnL

		if closedFull {
			recordClosedPosition(ss, pos, a.FillPrice, a.RealizedPnL, operatorCloseReason(sc), now)
			delete(ss.Positions, a.Symbol)
			clearHLPerpsPositionAlertThrottles(ss, a.Symbol)
		} else {
			preReduceQty := pos.Quantity
			preReduceBasis := pos.HedgePrimaryQtyBasis
			pos.Quantity -= a.Quantity
			if sc.Type != "manual" {
				clearForceCloseCanceledProtectionOIDs(pos, a.StopLossOID, a.TPOIDs)
			}
			if pos.isHedgeLeg() {
				pos.HedgePrimaryQtyBasis = hedgeBasisAfterPartialReduce(preReduceBasis, preReduceQty, pos.Quantity)
			}
		}
		fmt.Printf("[manual] applied %s: %s %.6f %s @ $%.4f | PnL=$%.2f\n",
			closeLabel, a.StrategyID, a.Quantity, a.Symbol, a.FillPrice, a.RealizedPnL)

	case "add":
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s; open one first", a.StrategyID, a.Symbol)
		}
		if !manualPositionOwnedByStrategy(pos, a.StrategyID) {
			return fmt.Errorf("position %s/%s is owned by %q, not %q", a.StrategyID, a.Symbol, pos.OwnerStrategyID, a.StrategyID)
		}
		if a.Side != "" && a.Side != pos.Side {
			return fmt.Errorf("scale-in side %q does not match open position side %q for %s/%s", a.Side, pos.Side, a.StrategyID, a.Symbol)
		}
		applyScaleIn(pos, a.Quantity, a.FillPrice)
		trade := Trade{
			Timestamp:       now,
			StrategyID:      a.StrategyID,
			Symbol:          a.Symbol,
			Side:            openTradeSide(pos.Side),
			Quantity:        a.Quantity,
			Price:           a.FillPrice,
			Value:           a.Quantity * a.FillPrice,
			TradeType:       scaleInTradeType,
			Details:         fmt.Sprintf("manual scale-in %s %s @ $%.4f (add #%d, new qty %.6f, avg $%.4f)", pos.Side, a.Symbol, a.FillPrice, pos.ScaleInCount, pos.Quantity, pos.AvgCost),
			PositionID:      ensurePositionTradeID(a.StrategyID, a.Symbol, pos),
			ExchangeOrderID: a.ExchangeOrderID,
			ExchangeFee:     a.FillFee,
			FeeSource:       FeeSourceUserFills,
			PnLGross:        true,
			IsClose:         false,
			Manual:          true,
		}
		trade.Regime = pos.Regime
		trade.EntryATR = pos.EntryATR
		RecordTrade(ss, trade)
		ss.Cash -= a.FillFee
		fmt.Printf("[manual] applied scale-in: %s +%.6f %s @ $%.4f (new qty %.6f, avg $%.4f)\n",
			a.StrategyID, a.Quantity, a.Symbol, a.FillPrice, pos.Quantity, pos.AvgCost)

	case "update-sl":
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s", a.StrategyID, a.Symbol)
		}
		if !manualPositionOwnedByStrategy(pos, a.StrategyID) {
			return fmt.Errorf("position %s/%s is owned by %q, not %q", a.StrategyID, a.Symbol, pos.OwnerStrategyID, a.StrategyID)
		}
		pos.StopLossOID = a.StopLossOID
		pos.StopLossTriggerPx = a.StopLossTriggerPx
		fmt.Printf("[manual] applied update-sl: %s %s stop-loss -> $%.4f (OID=%d)\n",
			a.StrategyID, a.Symbol, a.StopLossTriggerPx, a.StopLossOID)

	case "cancel-sl":
		pos, exists := ss.Positions[a.Symbol]
		if !exists || pos == nil {
			return fmt.Errorf("no open position for %s/%s", a.StrategyID, a.Symbol)
		}
		if !manualPositionOwnedByStrategy(pos, a.StrategyID) {
			return fmt.Errorf("position %s/%s is owned by %q, not %q", a.StrategyID, a.Symbol, pos.OwnerStrategyID, a.StrategyID)
		}
		pos.StopLossOID = 0
		pos.StopLossTriggerPx = 0
		fmt.Printf("[manual] applied cancel-sl: %s %s (stop-loss removed)\n",
			a.StrategyID, a.Symbol)

	default:
		return fmt.Errorf("unknown action %q", a.Action)
	}
	return nil
}

func findManualStrategy(cfg *Config, id string) (StrategyConfig, bool) {
	sc, err := lookupManualStrategy(cfg, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return StrategyConfig{}, false
	}
	return sc, true
}

func findForceCloseStrategy(cfg *Config, id string) (StrategyConfig, string, bool) {
	sc, sym, err := lookupForceCloseStrategy(cfg, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return StrategyConfig{}, "", false
	}
	return sc, sym, true
}

func hyperliquidSucceededCancelOIDs(result *HyperliquidCloseResult, requested []int64) []int64 {
	if result == nil || len(requested) == 0 {
		return nil
	}
	if len(result.CancelStopLossSucceededOIDs) > 0 {
		requestedSet := make(map[int64]struct{}, len(requested))
		for _, oid := range requested {
			if oid > 0 {
				requestedSet[oid] = struct{}{}
			}
		}
		var out []int64
		seen := make(map[int64]struct{}, len(result.CancelStopLossSucceededOIDs))
		for _, oid := range result.CancelStopLossSucceededOIDs {
			if oid <= 0 {
				continue
			}
			if _, ok := requestedSet[oid]; !ok {
				continue
			}
			if _, dup := seen[oid]; dup {
				continue
			}
			out = append(out, oid)
			seen[oid] = struct{}{}
		}
		return out
	}
	if result.CancelStopLossSucceeded && result.CancelStopLossError == "" {
		return cloneInt64s(requested)
	}
	return nil
}

func forceCloseCanceledProtectionSnapshot(pos *Position, canceledOIDs []int64) (int64, []int64) {
	if pos == nil || len(canceledOIDs) == 0 {
		return 0, nil
	}
	canceled := make(map[int64]struct{}, len(canceledOIDs))
	for _, oid := range canceledOIDs {
		if oid > 0 {
			canceled[oid] = struct{}{}
		}
	}
	var slOID int64
	if pos.StopLossOID > 0 {
		if _, ok := canceled[pos.StopLossOID]; ok {
			slOID = pos.StopLossOID
		}
	}
	var tpOIDs []int64
	for idx, oid := range pos.TPOIDs {
		if oid <= 0 {
			continue
		}
		if _, ok := canceled[oid]; !ok {
			continue
		}
		if tpOIDs == nil {
			tpOIDs = make([]int64, len(pos.TPOIDs))
		}
		tpOIDs[idx] = oid
	}
	return slOID, tpOIDs
}

func clearForceCloseCanceledProtectionOIDs(pos *Position, canceledSLOID int64, canceledTPOIDs []int64) {
	if pos == nil {
		return
	}
	if canceledSLOID > 0 && pos.StopLossOID == canceledSLOID {
		pos.StopLossOID = 0
		pos.StopLossTriggerPx = 0
	}
	for idx, canceledOID := range canceledTPOIDs {
		if canceledOID <= 0 {
			continue
		}
		if idx >= len(pos.TPOIDs) || pos.TPOIDs[idx] != canceledOID {
			continue
		}
		pos.TPOIDs[idx] = 0
		if idx < len(pos.TPArmedTiers) {
			pos.TPArmedTiers[idx] = false
		}
	}
}

func validatePendingManualActionStrategy(sc StrategyConfig, a PendingManualAction) error {
	if sc.Type == "manual" {
		return nil
	}
	if a.Action == "close" && sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
		return nil
	}
	if a.Action == "close" {
		return fmt.Errorf("strategy %q close action requires type=manual or live Hyperliquid perps (got platform=%q type=%q)", a.StrategyID, sc.Platform, sc.Type)
	}
	return fmt.Errorf("strategy %q is not type=manual", a.StrategyID)
}

func operatorCloseLabel(sc StrategyConfig) string {
	if sc.Type == "perps" {
		return "force close"
	}
	return "manual close"
}

func operatorCloseReason(sc StrategyConfig) string {
	if sc.Type == "perps" {
		return "force_close"
	}
	return "manual_close"
}

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

type manualMarkFetcher func(coins []string) (map[string]float64, error)

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

func openTradeSide(posSide string) string {
	if posSide == "short" {
		return "sell"
	}
	return "buy"
}

func resolveManualRatchetRegimeLabel(sc StrategyConfig, cfg *Config, notifier *MultiNotifier) string {
	if cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
		return ""
	}
	if !strategyUsesTrailingTPRatchetClose(sc) || sc.TrailingStopATRMultRegime == nil || !sc.TrailingStopATRMultRegime.IsConfigured() {
		return ""
	}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
	posCtx := positionCtxFromPosition(nil)
	result, _, _, ok := runHyperliquidCheck(&sc, nil, posCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger, nil)
	if !ok || result == nil {
		return ""
	}
	payload := regimePayloadValue(result.Regime)
	return strings.TrimSpace(payload.Label(resolveStrategyRegimeWindow(sc, "atr", cfg.Regime), cfg.Regime))
}

func manualRatchetOpeningTrailOrFallback(block *RegimeATRBlock, label string, fallbackMult float64) (float64, bool) {
	if block != nil && strings.TrimSpace(label) != "" {
		if mult, ok := resolveRegimeATR(*block, label); ok && mult > 0 {
			return mult, false
		}
	}
	if fallbackMult > 0 {
		return fallbackMult, true
	}
	return defaultManualStopLossATRMult, true
}

func runManualCloseEval(sc StrategyConfig, ss *StrategyState, cfg *Config, notifier *MultiNotifier, logger *StrategyLogger) (float64, float64, bool) {
	pos := ss.Positions[sc.Symbol]
	if pos == nil {
		return 0, 0, true
	}

	posCtx := positionCtxFromPosition(pos)
	result, _, price, ok := runHyperliquidCheck(&sc, nil, posCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger, nil)
	if !ok {
		return 0, 0, false
	}
	return result.CloseFraction, price, true
}
