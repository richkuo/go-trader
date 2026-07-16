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
const defaultManualStopLossATRMult = 2.0

// runManualOpen implements `go-trader manual-open <strategy-id>`.
// It places an on-chain HL order (or records an existing fill with --record-only),
// then enqueues the fill in pending_manual_actions for the scheduler to drain.
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

	// #883: resting-limit-order placement is a self-contained fire-and-exit path
	// — it places the maker order, persists its OID, and returns. The scheduler
	// owns fill detection + protection arming (there is no synchronous fill
	// here), so it stays in the CLI instead of the #1257 shared core; it reuses
	// the core's side/sizing validation helpers.
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
		// Ioc is intentionally NOT accepted here: an immediate-or-cancel order
		// never rests, so it doesn't fit a feature about resting limit orders.
		// (adapter.limit_open still supports Ioc for any future internal use.)
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
				// #1269: a resting limit open is still a position-increasing
				// entry — refuse while the daily loss limit is tripped, same
				// as the market-order core path.
				if st := evaluateDailyLossLimit(cfg.PortfolioRisk, state.Strategies, time.Now().UTC()); st.Tripped {
					fmt.Fprintf(os.Stderr, "error: %s — manual-open blocked until UTC rollover (closes and SL edits are unaffected)\n", dailyLossHoldDetail(st))
					return 1
				}
				// #1270: a resting limit open increases exposure in resolvedSide's
				// direction once it fills — refuse while that direction's bucket
				// is capped or this asset is over-concentrated in that direction.
				// nil prices → AvgCost valuation; the concentration basis comes
				// from manualExposureCapStatus, same as the market-order core
				// path (manualStateViewFromState).
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

	// Build notifier for warning paths (no-op when Discord/Telegram not configured).
	notifier, closeNotifier := buildNotifierFromConfig(cfg)
	defer closeNotifier()

	// #1257: the market-order path runs in the shared core (same code as the
	// dashboard endpoint); this wrapper only parses flags and replays output.
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

// runManualAdd implements `go-trader manual-add <strategy-id>` (#873). It scales
// into an EXISTING manual position: places a same-side on-chain HL order (or
// records an existing fill with --record-only), then enqueues an "add" action
// for the scheduler to blend in. The side is inferred from the open position;
// EntryATR, the regime label, and the TP tier geometry stay frozen — only the
// on-chain protection SIZING is re-based on the next scheduler cycle.
func runManualAdd(args []string) int {
	// #873: manual-add is the operator-intent path and deliberately bypasses the
	// allow_scale_in flag and the scaleInLiveProtectionResizable load-time guard
	// (those gate the strategy-flag perps path only). That is safe because a
	// type=manual strategy always auto-configures an ATR stop-loss, which the
	// protection-sync resize path can grow after an add.
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

	res, coreErr := manualCloseCore(newCLIManualCoreDeps(cfg, stateDB, nil), sc, manualCloseInputs{
		StrategyID: strategyID,
		Qty:        *qty,
		DryRun:     *dryRun,
	})
	return printManualCoreOutcome(res, coreErr)
}

// runForceClose implements `go-trader force-close <strategy-id>` for live HL
// perps strategies. It submits the venue close immediately, then enqueues the
// confirmed fill for the scheduler drain so state/trade mutation stays in the
// daemon-owned path.
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

// printManualCoreOutcome replays a core's ordered output lines on the streams
// the pre-#1257 monolith used, prints the failure (if any) to stderr, and
// maps it to the CLI exit code.
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

// manualAlert captures one strategy's successfully drained manual actions so the
// caller can emit trade alerts AFTER releasing mu. drainPendingManualActions runs
// under mu.Lock and sendTradeAlerts re-acquires mu.RLock; since sync.RWMutex is
// not reentrant, alerting inside the drain would self-deadlock (#880).
type manualAlert struct {
	sc     StrategyConfig
	ss     *StrategyState
	trades int // count of trades appended this drain for this strategy
}

// drainPendingManualActions reads all rows from pending_manual_actions, applies
// them to the in-memory AppState, then deletes the drained rows. It returns one
// manualAlert per strategy that had >=1 action successfully applied (with the
// aggregated trade count) so the caller can fire sendTradeAlerts outside the
// state write lock (#880). Called at the top of each scheduler cycle before
// dueStrategies is built.
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
	var order []string // preserves id-sorted insertion order for deterministic alert emission
	for _, a := range actions {
		if err := applyManualAction(state, cfg, scByID, a); err != nil {
			fmt.Printf("[manual] failed to apply action %d (%s %s): %v\n", a.ID, a.Action, a.StrategyID, err)
			continue
		}
		if a.ID > maxDrained {
			maxDrained = a.ID
		}
		// Trade-recording actions (open via recordPositionOpen, close/add via
		// RecordTrade) append exactly one trade per successful apply; aggregate
		// per strategy so sendTradeAlerts alerts the correct tail slice of
		// TradeHistory. SL-only actions (#1050 update-sl/cancel-sl) record no
		// trade — skip alert bookkeeping so the tail slice isn't misaligned.
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

// applyManualAction materialises one pending_manual_actions row into AppState.
// cfg is needed only to fall back to drain-time atr_method resolution for
// "open" rows queued before the atr_method column existed (#1277).
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
			Multiplier:                      1, // perps
			Leverage:                        sc.Leverage,
			OwnerStrategyID:                 a.StrategyID,
			OpenedAt:                        now,
			StopLossOID:                     a.StopLossOID,
			StopLossTriggerPx:               a.StopLossTriggerPx,
			TPOIDs:                          a.TPOIDs,
			RatchetFallbackNormalizePending: a.RatchetFallbackNormalizePending,
		}
		// #1277: freeze the atr_method the EntryATR was computed under, so
		// checkATRMethodDriftAtStartup sees manual positions too. Prefer the
		// queue-time value carried on the row (resolved next to the EntryATR
		// fetch in manualOpenCore); fall back to drain-time resolution only
		// for rows queued before the column existed — leaving it "" would
		// permanently hide this position from the drift check.
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
		// Fix #1: perps open deducts only the fee; notional stays virtual.
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
		// Use the explicit IsFullClose intent flag rather than a tolerance
		// heuristic, so a deliberate 99% partial close isn't silently
		// collapsed into a full close.
		closedFull := a.IsFullClose
		side := closeTradeSide(pos.Side)
		closeLabel := operatorCloseLabel(sc)
		// #1159: a hedge leg's forced close is tagged distinctly and its PnL
		// routes through RecordHedgeTradeResult (DailyPnL only) instead of
		// RecordTradeResult, mirroring every other close-booking path.
		tradeType := "perps"
		if pos.HedgeFor != "" {
			tradeType = hedgeTradeType
		}

		trade := Trade{
			Timestamp:       now,
			StrategyID:      a.StrategyID,
			Symbol:          a.Symbol,
			Side:            side,
			Quantity:        a.Quantity,
			Price:           a.FillPrice,
			Value:           a.Quantity * a.FillPrice,
			TradeType:       tradeType,
			Details:         fmt.Sprintf("%s %s @ $%.4f | PnL=$%.2f", closeLabel, a.Symbol, a.FillPrice, a.RealizedPnL),
			PositionID:      ensurePositionTradeID(a.StrategyID, a.Symbol, pos),
			ExchangeOrderID: a.ExchangeOrderID,
			ExchangeFee:     a.FillFee,
			FeeSource:       FeeSourceUserFills,
			IsClose:         true,
			RealizedPnL:     a.RealizedPnL + a.FillFee, // action PnL is net; gross row adds the fee back
			PnLGross:        true,
			Manual:          sc.Type == "manual",
		}
		RecordTrade(ss, trade)
		if sc.Type != "manual" {
			if pos.HedgeFor != "" {
				RecordHedgeTradeResult(&ss.RiskState, a.RealizedPnL)
			} else {
				RecordTradeResult(&ss.RiskState, a.RealizedPnL)
			}
		}
		// Fix #1: perps close credits only the realized PnL; notional was never debited.
		ss.Cash += a.RealizedPnL

		if closedFull {
			recordClosedPosition(ss, pos, a.FillPrice, a.RealizedPnL, operatorCloseReason(sc), now)
			delete(ss.Positions, a.Symbol)
		} else {
			pos.Quantity -= a.Quantity
			if sc.Type != "manual" {
				clearForceCloseCanceledProtectionOIDs(pos, a.StopLossOID, a.TPOIDs)
			}
			// #1159 review: a partial force-close on a hedge leg (queued by
			// forceCloseCore's best-effort mirror) reduces pos.Quantity here but
			// must also advance the basis watermark to the primary's CURRENT
			// qty — otherwise the next runHedgeSync cycle still sees the old
			// (pre-close) basis, computes a fresh delta against it, and reduces
			// the hedge a SECOND time on top of this mirror's reduce. The
			// primary's own "close" drain always applies before this one (both
			// were queued in the same forceCloseCore call, primary inserted
			// first, and LoadPendingManualActions replays by ascending id), so
			// ss.Positions[pos.HedgeFor] already reflects the post-close qty.
			if pos.HedgeFor != "" {
				if primaryPos, ok := ss.Positions[pos.HedgeFor]; ok && primaryPos != nil {
					pos.HedgePrimaryQtyBasis = primaryPos.Quantity
				} else {
					pos.HedgePrimaryQtyBasis = 0
				}
			}
		}
		fmt.Printf("[manual] applied %s: %s %.6f %s @ $%.4f | PnL=$%.2f\n",
			closeLabel, a.StrategyID, a.Quantity, a.Symbol, a.FillPrice, a.RealizedPnL)

	case "add":
		// #873 manual scale-in: blend an add leg into the open position. Side is
		// inferred from the position at CLI time; freezes EntryATR/regime/TP
		// geometry (applyScaleIn) and grows InitialQuantity. The next manual
		// protection sync re-sizes the on-chain SL/TP via ScaleInResizePending.
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
		// Perps add deducts only the fee; notional stays virtual (mirrors open).
		ss.Cash -= a.FillFee
		fmt.Printf("[manual] applied scale-in: %s +%.6f %s @ $%.4f (new qty %.6f, avg $%.4f)\n",
			a.StrategyID, a.Quantity, a.Symbol, a.FillPrice, pos.Quantity, pos.AvgCost)

	case "update-sl":
		// #1050: adopt a manually-moved stop-loss. The CLI already cancelled the
		// old OID and placed the new trigger on-chain; this only syncs the
		// in-memory OID + trigger so the daemon tracks the live order. No trade
		// is recorded (an SL move is not a fill).
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
		// #1050: adopt a manually-cancelled stop-loss. The CLI already cancelled
		// the OID on-chain; clear the in-memory trigger so the daemon no longer
		// believes the position is protected. No trade is recorded.
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

// findManualStrategy locates a type=manual strategy by ID in the config,
// printing a clear error if not found or wrong type.
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

// resolveManualRatchetRegimeLabel runs the regime check at manual-open CLI time
// and returns the current ATR-window regime label for a type=manual strategy
// whose close evaluator is trailing_tp_ratchet_regime (#1115). Impure — it spawns
// the regime subprocess (runHyperliquidCheck) with a flat posCtx (the position
// isn't open yet, so this reads the current/entry regime). Returns "" when the
// strategy isn't a regime ratchet, regime is disabled, or the check fails; the
// pure manualRatchetOpeningTrailOrFallback below turns that into a protective
// fallback so the open is never naked.
func resolveManualRatchetRegimeLabel(sc StrategyConfig, cfg *Config, notifier *MultiNotifier) string {
	if cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
		return ""
	}
	if !strategyUsesTrailingTPRatchetClose(sc) || sc.TrailingStopATRRegime == nil || !sc.TrailingStopATRRegime.IsConfigured() {
		return ""
	}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
	posCtx := positionCtxFromPosition(nil) // flat at open: read the current (entry) regime
	result, _, _, ok := runHyperliquidCheck(&sc, nil, posCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger)
	if !ok || result == nil {
		return ""
	}
	payload := regimePayloadValue(result.Regime)
	return strings.TrimSpace(payload.Label(resolveStrategyRegimeWindow(sc, "atr", cfg.Regime), cfg.Regime))
}

// manualRatchetOpeningTrailOrFallback resolves the inline opening-trail multiple
// armed at manual-open for a trailing_tp_ratchet_regime manual (#1115). It NEVER
// returns <= 0: the per-regime opening trail (fellBack=false) when the resolved
// regime label indexes a positive distance in the block, otherwise the protective
// defaultManualStopLossATRMult fallback (fellBack=true) so the position is never
// armed naked. Pure (no subprocess) so the safety-critical resolve-vs-fallback
// branch is unit-tested directly — the regime label is resolved upstream by the
// impure resolveManualRatchetRegimeLabel. Covers: empty label (regime read
// failed) → fallback; label with no/zero configured trail → fallback; good label
// → per-regime trail.
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

// runManualCloseEval runs the close-evaluator loop for a single type=manual
// strategy that has an open position. Called from the main scheduler loop.
// Returns (closeFraction, closePrice, ok). The live regime no longer rides on
// the check output (#879): the caller reads the global regime store for this
// strategy's signature directly, which also covers the flat case — this eval
// doesn't even spawn a subprocess then — so status/dashboard show a regime
// for manual strategies without a position.
func runManualCloseEval(sc StrategyConfig, ss *StrategyState, cfg *Config, notifier *MultiNotifier, logger *StrategyLogger) (float64, float64, bool) {
	pos := ss.Positions[sc.Symbol]
	if pos == nil {
		return 0, 0, true // flat — nothing to do
	}

	posCtx := positionCtxFromPosition(pos)
	result, _, price, ok := runHyperliquidCheck(&sc, nil, posCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger)
	if !ok {
		return 0, 0, false
	}
	return result.CloseFraction, price, true
}
