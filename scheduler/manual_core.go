package main

// #1257 (Phase 4 of #1229): shared manual-action cores.
//
// The manual CLI commands (manual-open/add/close, force-close,
// manual-update-sl/cancel-sl) and the dashboard trade-action endpoints
// (ui_trade_actions.go) execute the SAME code paths — the cores below. The
// CLI wrappers in manual.go / manual_sl.go keep flag parsing and printing;
// the UI handlers keep HTTP plumbing and the confirm-nonce check. Every
// fail-closed guard (kill switch, pending CB close, ownership,
// manualSLAutoManaged, pendingSLActionExists, force-close scope) lives here
// exactly once so neither caller can bypass it.
//
// Cores never print: they collect ordered operator-facing lines in the result
// (the CLI wrapper replays them to stdout/stderr; the UI joins them into the
// HTTP response) and return a *manualCoreError whose message matches the old
// CLI stderr text. warnNotifier calls stay inline because the notification
// must fire at event time regardless of caller.
//
// On-chain side effects go only through the injected exec funcs, which
// default to the existing RunHyperliquid* helpers (runPythonSideEffect lane)
// — no new Python spawning path. Injection exists so Go tests never spawn
// Python.

import (
	"fmt"
	"strings"
	"time"
)

// manualOutputLine is one operator-facing line emitted by a core, tagged with
// the stream the CLI wrapper should replay it on.
type manualOutputLine struct {
	stderr bool
	text   string
}

// manualCoreResult accumulates ordered output plus the queued outcome. It is
// always returned (even alongside an error) so lines emitted before a failure
// — e.g. a "Filled:" line before a queue-insert error — are never lost.
type manualCoreResult struct {
	lines  []manualOutputLine
	queued bool
}

func (r *manualCoreResult) outf(format string, args ...interface{}) {
	r.lines = append(r.lines, manualOutputLine{text: fmt.Sprintf(format, args...)})
}

func (r *manualCoreResult) errf(format string, args ...interface{}) {
	r.lines = append(r.lines, manualOutputLine{stderr: true, text: fmt.Sprintf(format, args...)})
}

// uiMessage joins the stdout lines for the HTTP response body.
func (r *manualCoreResult) uiMessage() string {
	var out []string
	for _, l := range r.lines {
		if !l.stderr {
			out = append(out, l.text)
		}
	}
	return strings.Join(out, "\n")
}

// manualCoreError preserves the CLI error text and exit-code class
// (usage=2, failure=1).
type manualCoreError struct {
	usage bool
	msg   string
}

func (e *manualCoreError) Error() string { return e.msg }

func manualUsagef(format string, args ...interface{}) error {
	return &manualCoreError{usage: true, msg: fmt.Sprintf(format, args...)}
}

func manualFailf(format string, args ...interface{}) error {
	return &manualCoreError{msg: fmt.Sprintf(format, args...)}
}

// manualCoreExitCode maps a core error back to the CLI exit code.
func manualCoreExitCode(err error) int {
	if ce, ok := err.(*manualCoreError); ok && ce.usage {
		return 2
	}
	return 1
}

// manualStateView is the read-only state snapshot a core needs. The CLI
// builds it from a fresh LoadStateWithDB read; the daemon builds it from the
// live in-memory AppState under ss.mu.RLock (released before the core runs
// any subprocess, per the 6-phase lock pattern).
type manualStateView struct {
	KillSwitch     bool
	HasStrategy    bool
	PendingCBClose bool
	DailyLossHold  bool   // #1269: portfolio daily loss limit tripped — position-increasing manual actions refuse
	DailyLossNote  string // #1269: one-line detail for the refusal message (set iff DailyLossHold)
	// #1270: same-direction exposure cap — both arms. The bucket arm sums at
	// AvgCost (no live feed on this path); the concentration arm evaluates
	// against the displayStrategyValue basis (see manualExposureCapStatus), so
	// a concentration-only config still protects manual entries. Guard sites
	// call exposureCapManualEntryBlock(ExposureCap, ExposureCapAsset, side).
	ExposureCap      ExposureCapStatus
	ExposureCapAsset string    // computeAssetDeltas key for this strategy (extractAsset)
	Pos              *Position // copy; nil when no open position for the symbol
}

func manualStateViewFromState(cfg *Config, state *AppState, strategyID, symbol string) manualStateView {
	v := manualStateView{KillSwitch: state.PortfolioRisk.KillSwitchActive}
	// #1269: manual opens/adds are CLI/dashboard-driven, not dispatch-loop
	// signals, so the daily loss limit is enforced here — next to the
	// kill-switch and pending-CB guards — instead of at the six
	// pausedBlocksSignal sites (which never see type=manual entries).
	if cfg != nil {
		if st := evaluateDailyLossLimit(cfg.PortfolioRisk, state.Strategies, time.Now().UTC()); st.Tripped {
			v.DailyLossHold = true
			v.DailyLossNote = dailyLossHoldDetail(st)
		}
		// #1270: same-direction exposure cap, both arms. nil prices →
		// positions value at AvgCost (this path has no live price feed); the
		// concentration basis comes from manualExposureCapStatus so a
		// concentration-only config is enforced here too, not silently inert.
		v.ExposureCap = manualExposureCapStatus(cfg, state)
		for _, sc := range cfg.Strategies {
			if sc.ID == strategyID {
				v.ExposureCapAsset = extractAsset(sc)
				break
			}
		}
	}
	ss := state.Strategies[strategyID]
	if ss == nil {
		return v
	}
	v.HasStrategy = true
	v.PendingCBClose = ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil
	if pos := ss.Positions[symbol]; pos != nil {
		cp := *pos
		cp.TPOIDs = cloneInt64s(pos.TPOIDs)
		cp.TPArmedTiers = append([]bool(nil), pos.TPArmedTiers...)
		v.Pos = &cp
	}
	return v
}

// manualCoreDeps carries the explicit dependencies both callers provide, plus
// injectable on-chain exec seams (defaults = the real RunHyperliquid*
// helpers) so tests never spawn Python.
type manualCoreDeps struct {
	cfg      *Config
	stateDB  *StateDB
	notifier *MultiNotifier

	// loadState returns the current state view for strategyID+symbol.
	loadState func(strategyID, symbol string) (manualStateView, error)

	execute     func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
	updateSL    func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error)
	cancelOrder func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)
	fetchMids   manualMarkFetcher
	closer      HyperliquidLiveCloser // force-close only

	// lockManualActions takes the cross-process manual-action lock, returning a
	// release closure to defer. It makes a core's guard-check → on-chain submit
	// → pending-row insert atomic against every OTHER process/caller sharing the
	// state DB (a CLI racing the dashboard, or two concurrent CLIs) — see
	// acquireManualActionFileLock. Production constructors always set it; it is
	// nil only in bare-struct test deps (single-goroutine, no cross-process
	// race), where acquireManualActionLock degrades to a no-op.
	lockManualActions func() (release func(), err error)
}

// acquireManualActionLock takes the cross-process manual-action lock via the
// injected dep, or a no-op when unset (bare test deps). The returned closure
// releases the lock and must be deferred by the caller for the whole span
// through the pending-row insert.
func (d manualCoreDeps) acquireManualActionLock() (func(), error) {
	if d.lockManualActions == nil {
		return func() {}, nil
	}
	return d.lockManualActions()
}

// newCLIManualCoreDeps builds deps for the standalone CLI process: state is
// read from the shared SQLite DB.
func newCLIManualCoreDeps(cfg *Config, stateDB *StateDB, notifier *MultiNotifier) manualCoreDeps {
	d := newManualCoreDeps(cfg, stateDB, notifier)
	d.loadState = func(strategyID, symbol string) (manualStateView, error) {
		state, err := LoadStateWithDB(cfg, stateDB)
		if err != nil {
			return manualStateView{}, err
		}
		return manualStateViewFromState(cfg, state, strategyID, symbol), nil
	}
	return d
}

func newManualCoreDeps(cfg *Config, stateDB *StateDB, notifier *MultiNotifier) manualCoreDeps {
	return manualCoreDeps{
		cfg:         cfg,
		stateDB:     stateDB,
		notifier:    notifier,
		execute:     RunHyperliquidExecute,
		updateSL:    RunHyperliquidUpdateStopLoss,
		cancelOrder: RunHyperliquidCancelOrder,
		fetchMids:   fetchHyperliquidMids,
		closer:      defaultHyperliquidForceCloseCloser,
		lockManualActions: func() (func(), error) {
			return acquireManualActionFileLock(cfg.DBFile)
		},
	}
}

// lookupManualStrategy is the silent core behind findManualStrategy.
func lookupManualStrategy(cfg *Config, id string) (StrategyConfig, error) {
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			if sc.Type != "manual" {
				return StrategyConfig{}, manualFailf("error: strategy %q has type=%q; manual-open/close only works with type=manual strategies", id, sc.Type)
			}
			return sc, nil
		}
	}
	return StrategyConfig{}, manualFailf("error: strategy %q not found in config", id)
}

// lookupForceCloseStrategy is the silent core behind findForceCloseStrategy:
// force-close stays live-HL-perps-only (#1140).
func lookupForceCloseStrategy(cfg *Config, id string) (StrategyConfig, string, error) {
	for _, sc := range cfg.Strategies {
		if sc.ID != id {
			continue
		}
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			return StrategyConfig{}, "", manualFailf("error: strategy %q has platform=%q type=%q; force-close only works with live Hyperliquid perps strategies", id, sc.Platform, sc.Type)
		}
		if !hyperliquidIsLive(sc.Args) {
			return StrategyConfig{}, "", manualFailf("error: strategy %q is not live mode; force-close only works with live Hyperliquid perps strategies", id)
		}
		sym := hyperliquidSymbol(sc.Args)
		if strings.TrimSpace(sym) == "" {
			return StrategyConfig{}, "", manualFailf("error: strategy %q has no Hyperliquid symbol in args", id)
		}
		return sc, sym, nil
	}
	return StrategyConfig{}, "", manualFailf("error: strategy %q not found in config", id)
}

func refuseIfPendingManualPositionAction(stateDB *StateDB, cmdName, strategyID, symbol string) error {
	pending, err := pendingManualActionExists(stateDB, strategyID, symbol, "open", "add", "close")
	if err != nil {
		return manualFailf("error: could not check for queued position actions (%v) — refusing %s to avoid double-firing an on-chain order; retry once the scheduler is reachable", err, cmdName)
	}
	if pending {
		return manualFailf("error: a position-changing action (open/add/close) for %s/%s is already submitted and awaiting the scheduler's next cycle — wait for it to apply before running %s again", strategyID, symbol, cmdName)
	}
	return nil
}

func refuseIfRestingLimitOrderQueued(stateDB *StateDB, cmdName, strategyID, symbol string) error {
	existing, err := stateDB.CountPendingLimitOrders(strategyID, symbol)
	if err != nil {
		return manualFailf("error: could not check for resting limit orders (%v) — refusing %s to avoid double-firing an on-chain order; retry once the scheduler is reachable", err, cmdName)
	}
	if existing > 0 {
		return manualFailf("error: %s already has a resting limit order for %s — cancel it first (go-trader manual-cancel %s) before running %s", strategyID, symbol, strategyID, cmdName)
	}
	return nil
}

// refuseIfPositionActionQueued fails closed when any position-changing action
// (open/add/close — force-close queues its fill as a "close" row) for
// strategyID+symbol is still un-drained in pending_manual_actions, OR when a
// resting manual limit-open for the same strategy+symbol exists in
// pending_limit_orders (#1261). It is the core-level twin of the UI handler's
// double-fire guard (ui_trade_actions.go): between an action submitting
// on-chain and the scheduler draining/adopting its row, a second submit from ANY
// caller (a rapid CLI re-run, a future core caller) fires a real order the
// in-memory accounting cannot see yet — doubling/flipping the position (a sized
// manual close is a regular non-reduce-only order) and orphaning it on drain
// (#1009 corrupt close).
// Symmetric with resolveManualSLTargetCore's refusal (#1260 review 5) and
// runManualLimitOpen's resting-order placement guard (#1261) so no queued row
// references a position another queued row will delete, reshape, or create
// first. Callers skip it on --record-only / --dry-run (those place no new
// on-chain order; --record-only/re-register must stay usable) and key it on the
// SAME symbol the core writes into the queued row (forceCloseCore uses the
// args-derived sym, not the empty perps sc.Symbol). Fail closed on a check
// error. manual-add/manual-close call
// clearRestingLimitRemainderForPositionAction first, so a partial limit fill can
// still be averaged/flattened after the unfilled remainder is proven off-book.
func refuseIfPositionActionQueued(d manualCoreDeps, cmdName, strategyID, symbol string) error {
	if err := refuseIfPendingManualPositionAction(d.stateDB, cmdName, strategyID, symbol); err != nil {
		return err
	}
	if err := refuseIfRestingLimitOrderQueued(d.stateDB, cmdName, strategyID, symbol); err != nil {
		return err
	}
	return nil
}

func pendingLimitOrdersForStrategySymbol(stateDB *StateDB, strategyID, symbol string) ([]PendingLimitOrder, error) {
	orders, err := stateDB.LoadPendingLimitOrders()
	if err != nil {
		return nil, err
	}
	matching := make([]PendingLimitOrder, 0, len(orders))
	for _, o := range orders {
		if o.StrategyID == strategyID && o.Symbol == symbol {
			matching = append(matching, o)
		}
	}
	return matching, nil
}

func limitStatusForOID(res *HyperliquidLimitStatusResult, oid int64) (HyperliquidLimitOrderStatus, bool) {
	if res == nil {
		return HyperliquidLimitOrderStatus{}, false
	}
	for _, st := range res.Orders {
		if st.OID == oid {
			return st, true
		}
	}
	return HyperliquidLimitOrderStatus{}, false
}

// clearRestingLimitRemainderForPositionAction cancels a resting manual limit
// remainder so an owned partial-fill position can be flattened/averaged in one
// step. It returns the confirmed cumulative filled size and volume-weighted
// average price of every order it cleared — the authoritative, currently-adopted
// position size a caller must size its flatten against. That number matters
// because the caller's own state snapshot can lag: reconcilePendingLimitOrders
// persists a newly-adopted fill's watermark (UpdatePendingLimitOrderFill) BEFORE
// the grown in-memory position is flushed to the DB (end-of-cycle SaveState) and
// does NOT hold the manual-action lock, so a snapshot taken mid-fill undercounts
// the position. Because the unadopted-fill guard below proves the exchange fill
// equals the tracked watermark (fully adopted) and the placement guard proves
// the position is composed solely of this order's fills, the cleared cumulative
// fill IS the true position size in this window. Returns (0, 0, nil) when no
// remainder rests.
func clearRestingLimitRemainderForPositionAction(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, cmdName, strategyID, symbol string) (float64, float64, error) {
	orders, err := pendingLimitOrdersForStrategySymbol(d.stateDB, strategyID, symbol)
	if err != nil {
		return 0, 0, manualFailf("error: could not check for resting limit orders (%v) — refusing %s to avoid double-firing an on-chain order; retry once the scheduler is reachable", err, cmdName)
	}
	if len(orders) == 0 {
		return 0, 0, nil
	}

	if _, err := d.stateDB.MarkPendingLimitOrderCancelRequested(strategyID, symbol); err != nil {
		return 0, 0, manualFailf("error: could not mark resting limit order cancel_requested (%v) — refusing %s to avoid racing the scheduler's fill adoption", err, cmdName)
	}

	var clearedQty, clearedNotional float64
	for _, o := range orders {
		cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(sc.Script, o.Symbol, o.OrderOID)
		if cstderr != "" {
			res.errf("[limit-cancel] %s stderr: %s", strategyID, cstderr)
		}
		if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
			msg := ""
			if cancelRes != nil {
				msg = cancelRes.Error
			}
			return 0, 0, manualFailf("error: could not cancel resting limit order for %s/%s (oid=%d): %v %s — cancellation is queued for the scheduler; wait for the next cycle before running %s", strategyID, o.Symbol, o.OrderOID, cerr, msg, cmdName)
		}

		statusRes, sstderr, serr := runHyperliquidLimitStatusFn(sc.Script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
		if sstderr != "" {
			res.errf("[limit-status] %s stderr: %s", strategyID, sstderr)
		}
		if serr != nil || statusRes == nil || statusRes.Error != "" {
			msg := ""
			if statusRes != nil {
				msg = statusRes.Error
			}
			return 0, 0, manualFailf("error: could not verify cancelled limit order for %s/%s (oid=%d): %v %s — cancellation is queued for the scheduler; wait for it to adopt any final fill before running %s", strategyID, o.Symbol, o.OrderOID, serr, msg, cmdName)
		}
		if statusRes.OpenOrdersError != "" {
			return 0, 0, manualFailf("error: could not verify cancelled limit order for %s/%s (oid=%d): open-orders state unknown (%s) — cancellation is queued for the scheduler; wait for the next cycle before running %s", strategyID, o.Symbol, o.OrderOID, statusRes.OpenOrdersError, cmdName)
		}
		st, ok := limitStatusForOID(statusRes, o.OrderOID)
		if !ok {
			return 0, 0, manualFailf("error: could not verify cancelled limit order for %s/%s (oid=%d): status response did not include the order — cancellation is queued for the scheduler; wait for the next cycle before running %s", strategyID, o.Symbol, o.OrderOID, cmdName)
		}
		if st.FillsError != "" {
			return 0, 0, manualFailf("error: could not verify cancelled limit order fills for %s/%s (oid=%d): %s — cancellation is queued for the scheduler; wait for it to adopt any final fill before running %s", strategyID, o.Symbol, o.OrderOID, st.FillsError, cmdName)
		}
		if st.Resting == nil || *st.Resting {
			return 0, 0, manualFailf("error: resting limit order for %s/%s (oid=%d) is not yet confirmed off-book — cancellation is queued for the scheduler; wait for the next cycle before running %s", strategyID, o.Symbol, o.OrderOID, cmdName)
		}
		if st.FilledSize > o.FilledSize+limitFillEpsilon {
			return 0, 0, manualFailf("error: resting limit order for %s/%s (oid=%d) has an unadopted fill (tracked %.6f, exchange %.6f) — cancellation is queued; run/wait for the scheduler to adopt the fill before running %s", strategyID, o.Symbol, o.OrderOID, o.FilledSize, st.FilledSize, cmdName)
		}
		if err := d.stateDB.DeletePendingLimitOrder(o.ID); err != nil {
			return 0, 0, manualFailf("error: cancelled limit order for %s/%s (oid=%d) is off-book but the queue row could not be cleared (%v) — refusing %s so the scheduler can finalize it safely", strategyID, o.Symbol, o.OrderOID, err, cmdName)
		}
		// Off-book with its full cumulative fill adopted (the guard above proved
		// st.FilledSize <= watermark). Accumulate it as this order's authoritative
		// contribution to the tracked position; mirror reconcile's avg-price
		// fallback to the limit price when the fills poll returns no VWAP.
		fillPx := st.AvgPx
		if fillPx <= 0 {
			fillPx = o.LimitPrice
		}
		clearedQty += st.FilledSize
		clearedNotional += st.FilledSize * fillPx
		res.outf("Cancelled resting limit remainder: %s %s oid=%d before %s", strategyID, o.Symbol, o.OrderOID, cmdName)
	}
	clearedAvgPx := 0.0
	if clearedQty > 0 {
		clearedAvgPx = clearedNotional / clearedQty
	}
	return clearedQty, clearedAvgPx, nil
}

// ---------------------------------------------------------------------------
// manual-open

type manualOpenInputs struct {
	StrategyID string
	Side       string // "" → user_defaults.manual.side / "long"
	Size       float64
	Notional   float64
	Margin     float64
	ATR        float64
	SLATRMult  float64
	SLPct      float64
	RecordOnly bool
	FillPrice  float64
	DryRun     bool
}

// resolveManualOpenSide applies the config-default side and validates it
// against the strategy's direction enum. Shared by the market core and the
// #883 resting-limit CLI path.
func resolveManualOpenSide(cfg *Config, sc StrategyConfig, side string) (string, string, error) {
	side = strings.ToLower(strings.TrimSpace(side))
	if side == "" {
		side = cfg.resolveManualSide()
	}
	if side != "long" && side != "short" {
		return "", "", manualUsagef("error: --side must be \"long\" or \"short\", got %q", side)
	}
	if side == "short" && !PerpsAllowsShort(sc) {
		return "", "", manualFailf("error: strategy %q direction=%q does not allow shorts (set direction to %q or %q)", sc.ID, EffectiveDirection(sc), DirectionShort, DirectionBoth)
	}
	if side == "long" && !PerpsAllowsLong(sc) {
		return "", "", manualFailf("error: strategy %q direction=%q does not allow longs (set direction to %q or %q)", sc.ID, EffectiveDirection(sc), DirectionLong, DirectionBoth)
	}
	openSide := "buy"
	if side == "short" {
		openSide = "sell"
	}
	return side, openSide, nil
}

// validateManualSizing enforces the one-of-{size,notional,margin} rule,
// defaulting to the configured margin when nothing was passed (market and
// add paths). Returns the (possibly defaulted) margin plus whether the
// default kicked in, so the caller can emit the CLI note.
func validateManualSizing(cfg *Config, size, notional, margin float64, recordOnly bool) (float64, bool, error) {
	sizingInputs := countSizingFlags(size, notional, margin)
	defaulted := false
	if sizingInputs == 0 && !recordOnly {
		margin = cfg.resolveManualMarginUSD()
		sizingInputs = 1
		defaulted = true
	}
	if sizingInputs == 0 {
		return margin, false, manualUsagef("error: one of --size, --notional, or --margin is required")
	}
	if sizingInputs > 1 {
		return margin, false, manualUsagef("error: only one of --size, --notional, or --margin may be specified")
	}
	return margin, defaulted, nil
}

// manualOpenCore is the shared market-order open path behind
// `go-trader manual-open` and POST /api/strategies/{id}/open. The #883
// resting-limit path stays in the CLI wrapper (fire-and-exit, own function).
func manualOpenCore(d manualCoreDeps, sc StrategyConfig, in manualOpenInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID
	cfg := d.cfg

	side, openSide, err := resolveManualOpenSide(cfg, sc, in.Side)
	if err != nil {
		return res, err
	}

	margin, marginDefaulted, err := validateManualSizing(cfg, in.Size, in.Notional, in.Margin, in.RecordOnly)
	if err != nil {
		return res, err
	}
	if marginDefaulted {
		res.errf("[manual-open] no sizing flag provided; defaulting to --margin %g", margin)
	}
	in.Margin = margin

	if in.RecordOnly {
		if in.Size <= 0 {
			return res, manualUsagef("error: --record-only requires --size (coin qty of the fill you placed)")
		}
		if in.FillPrice <= 0 {
			return res, manualUsagef("error: --record-only requires --fill-price (the price at which your fill executed)")
		}
	}

	// Fix #4: guard against placing into a kill-switched or CB-pending account.
	if !in.DryRun {
		view, loadErr := d.loadState(strategyID, sc.Symbol)
		if loadErr != nil {
			res.errf("warning: could not load state for safety check: %v", loadErr)
		} else {
			if view.KillSwitch {
				return res, manualFailf("error: portfolio kill switch is active — manual-open blocked (use manual-close to flatten)")
			}
			if view.PendingCBClose {
				return res, manualFailf("error: strategy has a pending circuit-breaker close — manual-open blocked")
			}
			if view.DailyLossHold {
				return res, manualFailf("error: %s — manual-open blocked until UTC rollover (closes and SL edits are unaffected)", view.DailyLossNote)
			}
			// #1270: a manual open increases exposure in `side`'s direction —
			// refuse while that direction's bucket is capped or this asset is
			// over-concentrated in that direction (the other direction,
			// closes, and SL edits are unaffected).
			if blocked, why := exposureCapManualEntryBlock(view.ExposureCap, view.ExposureCapAsset, side); blocked {
				return res, manualFailf("error: %s — manual-open (%s) blocked (closes and SL edits are unaffected)", why, side)
			}
			if view.ExposureCap.PVBasisMiss {
				res.errf("%s", exposureCapPVBasisMissWarning)
			}
		}
	}

	// #1260 review: refuse a second open while a position-changing action is
	// already queued for this strategy+symbol (a re-fired open doubles the
	// position). The UI handler guards this too under tradeActionMu; this covers
	// the CLI + any future core caller. Skip --record-only / --dry-run (no new
	// on-chain order). Runs before the sizing mid-fetch so a refusal is cheap.
	if !in.RecordOnly && !in.DryRun {
		// Hold the cross-process lock from before the guard READ through the
		// pending-row INSERT (function-scoped defer), so a CLI racing the
		// dashboard (or two CLIs) can't both observe no-pending and both fire.
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
		if err := refuseIfPositionActionQueued(d, "manual-open", strategyID, sc.Symbol); err != nil {
			return res, err
		}
	}

	// ATR plausibility guard: mirror stampEntryATRIfOpened's 50%-of-AvgCost check.
	entryATR := in.ATR
	if in.RecordOnly && entryATR > 0 && in.FillPrice > 0 && entryATR > 0.5*in.FillPrice {
		return res, manualFailf("error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)", entryATR, in.FillPrice)
	}

	effectiveSLPct := 0.0
	if in.SLPct > 0 {
		effectiveSLPct = in.SLPct
	}

	script := sc.Script

	// #711: --margin/--notional need a price reference (HL mid) to resolve qty.
	var resolvedOrderSize, sizingMark float64
	var sizingFailed bool
	if !in.RecordOnly {
		qty, mark, err := resolveManualOpenOrderSize(sc, in.Size, in.Notional, in.Margin, d.fetchMids)
		if err != nil {
			if in.DryRun {
				res.errf("warning: dry-run sizing best-effort failed: %v", err)
				sizingFailed = true
			} else {
				return res, manualFailf("error: %v", err)
			}
		}
		resolvedOrderSize = qty
		sizingMark = mark
	}

	var resolvedFillPrice, fillQty, fillFee float64
	var exchangeOID string

	if in.DryRun {
		prefix := "[dry-run]"
		if sizingFailed {
			prefix = "[dry-run] [sizing failed]"
		}
		res.outf("%s manual-open %s: %s %.6f %s (script=%s, sl_pct=%.2f, mark=$%.4f)",
			prefix, strategyID, side, resolvedOrderSize, sc.Symbol, script, effectiveSLPct, sizingMark)
		return res, nil
	}

	if in.RecordOnly {
		// Operator already placed the fill on the exchange UI.
		fillQty = in.Size
		resolvedFillPrice = in.FillPrice
		if entryATR > 0 && entryATR > 0.5*resolvedFillPrice {
			return res, manualFailf("error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)", entryATR, resolvedFillPrice)
		}
		// --record-only does not auto-arm the SL trigger.
		if in.SLATRMult > 0 || in.SLPct > 0 || (sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0) {
			res.errf("warning: --record-only does not arm a stop-loss trigger automatically — place the SL manually on the HL UI")
		}
	} else {
		execResult, execStderr, execErr := d.execute(
			script, sc.Symbol, openSide,
			resolvedOrderSize,
			effectiveSLPct, 0, 0, sc.MarginMode, sc.Leverage, false,
			hlExecuteSnapshot{},
		)
		if execStderr != "" {
			res.errf("HL execute stderr: %s", execStderr)
		}
		if execErr != nil {
			return res, manualFailf("error placing order: %v", execErr)
		}
		if execResult.Error != "" {
			return res, manualFailf("error from HL: %s", execResult.Error)
		}

		fill := execResult.Execution
		if fill == nil || fill.Fill == nil {
			return res, manualFailf("error: no fill returned from execute")
		}
		resolvedFillPrice = fill.Fill.AvgPx
		fillQty = fill.Fill.TotalSz
		fillFee = fill.Fill.Fee
		if fill.Fill.OID != 0 {
			exchangeOID = fmt.Sprintf("%d", fill.Fill.OID)
		}
		if fillQty <= 0 {
			fillQty = resolveManualSize(in.Size, in.Notional, in.Margin, resolvedFillPrice, sc.Leverage)
		}

		// Post-fill ATR plausibility guard.
		if entryATR > 0 && resolvedFillPrice > 0 && entryATR > 0.5*resolvedFillPrice {
			res.errf("warning: --atr %.4f exceeds 50%% of fill price %.4f — EntryATR will not be stamped", entryATR, resolvedFillPrice)
			entryATR = 0
		}
	}

	res.outf("Filled: %s %.6f %s @ $%.4f (fee=$%.4f)", side, fillQty, sc.Symbol, resolvedFillPrice, fillFee)

	notifier := d.notifier

	effectiveATRMult := in.SLATRMult
	if effectiveATRMult == 0 && sc.StopLossATRMult != nil {
		effectiveATRMult = *sc.StopLossATRMult
	}

	// #1115: resolve the per-regime opening trail for a ratchet-regime manual so
	// the position never opens NAKED until the daemon's trailing walker runs.
	ratchetFallbackNormalizePending := false
	if effectiveATRMult == 0 && !in.RecordOnly && strategyUsesTrailingTPRatchetClose(sc) &&
		sc.TrailingStopATRRegime != nil && sc.TrailingStopATRRegime.IsConfigured() {
		// Impure step: read the current regime label (spawns the regime subprocess).
		label := resolveManualRatchetRegimeLabel(sc, cfg, notifier)
		mult, fellBack := manualRatchetOpeningTrailOrFallback(sc.TrailingStopATRRegime, label, cfg.resolveManualRatchetFallbackATRMult())
		effectiveATRMult = mult
		ratchetFallbackNormalizePending = fellBack
		if fellBack {
			warnNotifier(notifier, fmt.Sprintf("[manual-open] %s %s: could not resolve the live regime trail (label=%q); arming a fallback SL at %.4g×ATR (daemon will normalize once when the configured regime trail is available)", strategyID, sc.Symbol, label, effectiveATRMult))
		} else {
			res.errf("[manual-open] %s %s: regime=%s → initial trailing SL at %.4g×ATR", strategyID, sc.Symbol, label, mult)
		}
	}

	// When --atr is omitted, fetch ATR like stampEntryATRIfOpened (#689); on
	// fetch failure fall back to the leverage-aware heuristic.
	if !in.RecordOnly && entryATR == 0 {
		needsATRProtection := effectiveATRMult > 0 || strategyUsesTieredTPATRClose(sc)
		if needsATRProtection {
			fetched, fetchErr, fetchedOK := fetchManualEntryATR(sc, cfg)
			if fetchedOK {
				if resolvedFillPrice > 0 && fetched > 0.5*resolvedFillPrice {
					fetchErr = fmt.Sprintf("fetched ATR=%.6f exceeds 50%% of fill price %.4f", fetched, resolvedFillPrice)
					fetchedOK = false
				} else {
					entryATR = fetched
					res.errf("[manual-open] %s %s: --atr omitted; auto-fetched ATR=%.6f (period=14, %s)",
						strategyID, sc.Symbol, fetched, resolveManualATRTimeframe(sc))
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

	// Arm ATR-based stop-loss after fill.
	var stopLossOID int64
	var stopLossTriggerPx float64

	if effectiveATRMult > 0 && entryATR > 0 && !in.RecordOnly {
		if side == "long" {
			stopLossTriggerPx = resolvedFillPrice - effectiveATRMult*entryATR
		} else {
			stopLossTriggerPx = resolvedFillPrice + effectiveATRMult*entryATR
		}
		if stopLossTriggerPx > 0 {
			slResult, slStderr, slErr := d.updateSL(script, sc.Symbol, side, fillQty, stopLossTriggerPx, 0)
			if slStderr != "" {
				res.errf("SL arm stderr: %s", slStderr)
			}
			if slErr != nil {
				res.errf("warning: SL placement failed: %v (position is open but unprotected)", slErr)
			} else if slResult.Error != "" {
				res.errf("warning: SL arm error: %s", slResult.Error)
			} else {
				stopLossOID = slResult.StopLossOID
				stopLossTriggerPx = slResult.StopLossTriggerPx
				res.outf("Stop-loss armed at $%.4f (OID=%d)", stopLossTriggerPx, stopLossOID)
			}
		}
	}

	// Place TP[n] reduce-only orders inline immediately after the fill.
	var tpOIDs []int64
	if !in.RecordOnly && strategyUsesTieredTPATRClose(sc) && entryATR > 0 {
		oids, warn, err := placeManualProtectionInline(sc, side, fillQty, resolvedFillPrice, entryATR, effectiveATRMult, stopLossOID)
		if err != nil || warn != "" {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: TP placement issue (position open with SL only): err=%v warn=%s",
				strategyID, sc.Symbol, err, warn))
		}
		tpOIDs = oids
		if len(oids) > 0 {
			res.outf("Take-profits armed: OIDs=%v", oids)
		}
	}

	action := PendingManualAction{
		StrategyID:                      strategyID,
		Action:                          "open",
		Symbol:                          sc.Symbol,
		Side:                            side,
		Quantity:                        fillQty,
		FillPrice:                       resolvedFillPrice,
		FillFee:                         fillFee,
		ExchangeOrderID:                 exchangeOID,
		StopLossOID:                     stopLossOID,
		StopLossTriggerPx:               stopLossTriggerPx,
		EntryATR:                        entryATR,
		ATRMethod:                       resolveATRMethod(sc, cfg),
		TPOIDs:                          tpOIDs,
		RatchetFallbackNormalizePending: ratchetFallbackNormalizePending && stopLossOID > 0 && stopLossTriggerPx > 0,
		CreatedAt:                       time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		// On-chain fill (and SL/TPs) succeeded but the queue insert failed.
		// Skip cleanup in --record-only: the operator's pre-existing fill is
		// theirs to manage; we never placed those on-chain orders.
		if in.RecordOnly {
			return res, manualFailf("error queuing action: %v", err)
		}
		res.errf("CRITICAL: queue insert failed (%v); on-chain position is open but the scheduler cannot adopt it. Attempting cleanup...", err)
		cleanedUp, cleanupMsg := attemptManualOpenCleanup(sc.Symbol, fillQty, stopLossOID, tpOIDs)
		if cleanedUp {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: queue insert failed (%v); position auto-flattened: %s",
				strategyID, sc.Symbol, err, cleanupMsg))
		} else {
			warnNotifier(notifier, fmt.Sprintf(
				"[manual-open] %s %s: queue insert failed (%v) AND auto-flatten failed: %s — MANUAL INTERVENTION REQUIRED on HL UI (side=%s qty=%.6f sl_oid=%d tp_oids=%v)",
				strategyID, sc.Symbol, err, cleanupMsg, side, fillQty, stopLossOID, tpOIDs))
		}
		return res, manualFailf("error queuing action: %v", err)
	}

	res.queued = true
	res.outf("Queued: %s position will appear in the dashboard after the next scheduler cycle.", strategyID)
	return res, nil
}

// ---------------------------------------------------------------------------
// manual-add

type manualAddInputs struct {
	StrategyID string
	Size       float64
	Notional   float64
	Margin     float64
	RecordOnly bool
	FillPrice  float64
	DryRun     bool
}

// manualAddCore is the shared scale-in path behind `go-trader manual-add`
// (#873) and POST /api/strategies/{id}/add.
func manualAddCore(d manualCoreDeps, sc StrategyConfig, in manualAddInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID

	margin, marginDefaulted, err := validateManualSizing(d.cfg, in.Size, in.Notional, in.Margin, in.RecordOnly)
	if err != nil {
		return res, err
	}
	if marginDefaulted {
		res.errf("[manual-add] no sizing flag provided; defaulting to --margin %g", margin)
	}
	in.Margin = margin
	if in.RecordOnly {
		if in.Size <= 0 {
			return res, manualUsagef("error: --record-only requires --size (coin qty of the fill you placed)")
		}
		if in.FillPrice <= 0 {
			return res, manualUsagef("error: --record-only requires --fill-price (the price at which your fill executed)")
		}
	}

	// An add requires the position to already exist — same kill-switch /
	// CB-pending guards as manual-open.
	view, loadErr := d.loadState(strategyID, sc.Symbol)
	if loadErr != nil {
		return res, manualFailf("error: could not load state to locate the open position: %v", loadErr)
	}
	if view.KillSwitch {
		return res, manualFailf("error: portfolio kill switch is active — manual-add blocked (use manual-close to flatten)")
	}
	if !view.HasStrategy {
		return res, manualFailf("error: no state for strategy %q", strategyID)
	}
	if view.PendingCBClose {
		return res, manualFailf("error: strategy has a pending circuit-breaker close — manual-add blocked")
	}
	if view.DailyLossHold {
		return res, manualFailf("error: %s — manual-add blocked until UTC rollover (closes and SL edits are unaffected)", view.DailyLossNote)
	}
	pos := view.Pos
	if pos == nil {
		return res, manualFailf("error: no open position for %s/%s; open one first with manual-open", strategyID, sc.Symbol)
	}
	// #1270: an add grows exposure in the position's direction — refuse while
	// that direction's bucket is capped or this asset is over-concentrated in
	// that direction (closes and SL edits are unaffected).
	addDir := "long"
	if pos.Side == "short" {
		addDir = "short"
	}
	if blocked, why := exposureCapManualEntryBlock(view.ExposureCap, view.ExposureCapAsset, addDir); blocked {
		return res, manualFailf("error: %s — manual-add (%s) blocked (closes and SL edits are unaffected)", why, addDir)
	}
	if view.ExposureCap.PVBasisMiss {
		res.errf("%s", exposureCapPVBasisMissWarning)
	}

	// #1260 review: refuse a scale-in while a position-changing action is queued
	// for this strategy+symbol — an add fired while a close is queued fires a
	// real buy that the drain orphans (the close applies first, deletes the
	// position, then the add row fails every cycle). Skip --record-only /
	// --dry-run. Runs before the sizing mid-fetch so a refusal is cheap.
	if !in.RecordOnly && !in.DryRun {
		// Cross-process lock held from before the guard READ through the
		// pending-row INSERT (see manualOpenCore).
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
		// The add's on-chain order size comes from the sizing flags, not the
		// position snapshot, and the daemon blends it into the live in-memory
		// position — so an add is never mis-sized by a stale snapshot and the
		// confirmed cleared fill is not needed here (unlike manual-close below).
		if _, _, err := clearRestingLimitRemainderForPositionAction(d, res, sc, "manual-add", strategyID, sc.Symbol); err != nil {
			return res, err
		}
		if err := refuseIfPositionActionQueued(d, "manual-add", strategyID, sc.Symbol); err != nil {
			return res, err
		}
	}

	side := pos.Side
	addSide := "buy"
	if side == "short" {
		addSide = "sell"
	}

	var resolvedOrderSize, sizingMark float64
	if !in.RecordOnly {
		qty, mark, err := resolveManualOpenOrderSize(sc, in.Size, in.Notional, in.Margin, d.fetchMids)
		if err != nil {
			if in.DryRun {
				res.errf("warning: dry-run sizing best-effort failed: %v", err)
			} else {
				return res, manualFailf("error: %v", err)
			}
		}
		resolvedOrderSize = qty
		sizingMark = mark
	}

	if in.DryRun {
		res.outf("[dry-run] manual-add %s: %s +%.6f %s (script=%s, mark=$%.4f, current qty=%.6f avg=$%.4f)",
			strategyID, side, resolvedOrderSize, sc.Symbol, sc.Script, sizingMark, pos.Quantity, pos.AvgCost)
		return res, nil
	}

	var resolvedFillPrice, fillQty, fillFee float64
	var exchangeOID string

	if in.RecordOnly {
		fillQty = in.Size
		resolvedFillPrice = in.FillPrice
	} else {
		// Add order: same-side market order. No SL pct, no cancel OID, and NO
		// margin-mode/leverage (HL rejects update_leverage on an open position);
		// the post-add protection sync re-sizes SL + un-cleared TPs.
		execResult, execStderr, execErr := d.execute(
			sc.Script, sc.Symbol, addSide,
			resolvedOrderSize,
			0, 0, 0, "", 0, false,
			hlExecuteSnapshot{},
		)
		if execStderr != "" {
			res.errf("HL execute stderr: %s", execStderr)
		}
		if execErr != nil {
			return res, manualFailf("error placing order: %v", execErr)
		}
		if execResult.Error != "" {
			return res, manualFailf("error from HL: %s", execResult.Error)
		}
		fill := execResult.Execution
		if fill == nil || fill.Fill == nil {
			return res, manualFailf("error: no fill returned from execute")
		}
		resolvedFillPrice = fill.Fill.AvgPx
		fillQty = fill.Fill.TotalSz
		fillFee = fill.Fill.Fee
		if fill.Fill.OID != 0 {
			exchangeOID = fmt.Sprintf("%d", fill.Fill.OID)
		}
		if fillQty <= 0 {
			fillQty = resolveManualSize(in.Size, in.Notional, in.Margin, resolvedFillPrice, sc.Leverage)
		}
	}

	res.outf("Filled scale-in: %s +%.6f %s @ $%.4f (fee=$%.4f)", side, fillQty, sc.Symbol, resolvedFillPrice, fillFee)

	action := PendingManualAction{
		StrategyID:      strategyID,
		Action:          "add",
		Symbol:          sc.Symbol,
		Side:            side,
		Quantity:        fillQty,
		FillPrice:       resolvedFillPrice,
		FillFee:         fillFee,
		ExchangeOrderID: exchangeOID,
		CreatedAt:       time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		return res, manualFailf("error queuing action: %v", err)
	}
	res.queued = true
	res.outf("Queued: scale-in for %s will blend into the position after the next scheduler cycle.", strategyID)
	return res, nil
}

// ---------------------------------------------------------------------------
// manual-close

type manualCloseInputs struct {
	StrategyID string
	Qty        float64 // 0 = full position
	DryRun     bool
}

// manualCloseCore is the shared close path behind `go-trader manual-close`
// and POST /api/strategies/{id}/close.
func manualCloseCore(d manualCoreDeps, sc StrategyConfig, in manualCloseInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID

	view, loadErr := d.loadState(strategyID, sc.Symbol)
	if loadErr != nil {
		return res, manualFailf("Failed to load state: %v", loadErr)
	}
	pos := view.Pos
	if pos == nil {
		return res, manualFailf("error: no open position found for %s/%s", strategyID, sc.Symbol)
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) {
		return res, manualFailf("error: position %s/%s is owned by %q, not %q", strategyID, sc.Symbol, pos.OwnerStrategyID, strategyID)
	}

	// Dry-run is advisory: it reports against the current snapshot and neither
	// takes the manual-action lock nor cancels/reconciles any resting limit
	// remainder (cancelling would be a real on-chain side effect). The --qty
	// bounds are checked against the snapshot here; the live path below resolves
	// the true size under the lock and re-checks against it.
	if in.DryRun {
		dryCloseSide := "sell"
		if pos.Side == "short" {
			dryCloseSide = "buy"
		}
		dryCloseQty := pos.Quantity
		if in.Qty > 0 {
			if in.Qty > pos.Quantity {
				return res, manualFailf("error: --qty %.6f exceeds open position %.6f", in.Qty, pos.Quantity)
			}
			dryCloseQty = in.Qty
		}
		res.outf("[dry-run] manual-close %s: %s %.6f %s (current pos=%.6f, avg_cost=$%.4f)",
			strategyID, dryCloseSide, dryCloseQty, sc.Symbol, pos.Quantity, pos.AvgCost)
		return res, nil
	}

	// Cross-process lock: held (function-scoped defer, past the dry-run return
	// above) from before the guard READS below through the pending-row INSERT,
	// so a CLI racing the dashboard (or two CLIs) can't both pass and both fire.
	unlock, lockErr := d.acquireManualActionLock()
	if lockErr != nil {
		return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
	}
	defer unlock()

	// #1260 review: refuse a second close while a position-changing action is
	// queued for this strategy+symbol. A re-fired sized manual close is a regular
	// non-reduce-only order (it can flip into an opposite position), and the
	// queued close row would double-decrement the position on drain (#1009
	// corrupt close). Applies to full AND partial close. The UI handler guards
	// this too; this covers the CLI + any future core caller.
	clearedQty, clearedAvgPx, err := clearRestingLimitRemainderForPositionAction(d, res, sc, "manual-close", strategyID, sc.Symbol)
	if err != nil {
		return res, err
	}
	// Resolve the true, currently-adopted position size UNDER THE LOCK. `pos`
	// above was read by loadState BEFORE the lock, and the daemon's
	// reconcilePendingLimitOrders adopts limit fills and flushes+deletes their
	// rows WITHOUT holding the manual-action lock — so between our loadState and
	// here it can supersede that snapshot. The global lock can be held for seconds
	// by a concurrent manual/dashboard op, widening this window enough for a
	// same-strategy+symbol resting limit to fully fill and drain.
	if clearedQty > 0 {
		// A resting remainder was present: clearResting cancelled it and read the
		// authoritative cumulative fill straight from the exchange (the placement
		// guard proves the position is solely this order's fills), so clearedQty IS
		// the true size. state.db is NOT authoritative here — the daemon has not
		// yet processed the cancel we just issued — so grow the snapshot up to
		// clearedQty (and its cumulative VWAP) rather than re-reading. Critical on
		// a shared coin, where closeFullPosition is false and a sized close of the
		// stale (smaller) qty would leave an untracked residual after the daemon
		// books flat, and so the queued close quantity + realized PnL match the
		// true size and cost.
		if clearedQty > pos.Quantity+limitFillEpsilon {
			staleQty := pos.Quantity
			pos.Quantity = clearedQty
			if clearedAvgPx > 0 {
				pos.AvgCost = clearedAvgPx
			}
			res.errf("[manual-close] %s %s: reconciled stale position snapshot %.6f → %.6f (scheduler adopted a limit fill before flushing state); closing the true size",
				strategyID, sc.Symbol, staleQty, pos.Quantity)
		}
	} else {
		// No resting remainder for this strategy+symbol under the lock. The
		// pre-lock snapshot may still be stale: the daemon can adopt a terminal
		// limit fill and flush+delete its row (without the manual-action lock)
		// between our loadState and this point, so clearResting finds nothing yet
		// the on-chain position already grew. But flush-before-delete
		// (reconcilePendingLimitOrders) guarantees "terminal row absent ⇒ state.db
		// reflects the adopted fill," and no NEW resting row can appear while we
		// hold the lock (manual limit-open takes it too). So a fresh re-read under
		// the lock IS the true, currently-adopted size — re-read before sizing so a
		// shared-coin close never flattens a stale (smaller) snapshot and leaks an
		// untracked residual, and so the --qty bound below validates against the
		// true size (#1263 review-4).
		refreshed, rerr := d.loadState(strategyID, sc.Symbol)
		if rerr != nil {
			return res, manualFailf("Failed to re-load state: %v", rerr)
		}
		if refreshed.Pos == nil {
			return res, manualFailf("error: no open position found for %s/%s", strategyID, sc.Symbol)
		}
		if !manualPositionOwnedByStrategy(refreshed.Pos, strategyID) {
			return res, manualFailf("error: position %s/%s is owned by %q, not %q", strategyID, sc.Symbol, refreshed.Pos.OwnerStrategyID, strategyID)
		}
		pos = refreshed.Pos
	}

	// Close side, keyed off the RESOLVED position (a limit fill cannot flip side
	// and a flip needs the lock we hold, so this is stable — but recompute it here
	// so no field survives from the possibly-replaced pre-lock snapshot).
	closeSide := "sell"
	if pos.Side == "short" {
		closeSide = "buy"
	}

	// Operator intent, evaluated against the RESOLVED position size (not the
	// pre-lock snapshot): --qty omitted (or equal to the full position) is a full
	// close; any smaller value is a partial close. Checking after the resolution
	// above means an explicit --qty matching the true, already-adopted size is
	// accepted instead of being wrongly refused against a stale smaller snapshot
	// (#1263 review-3/4), and the bounds error reports the true size. An explicit
	// --qty is never scaled up to the resolved size — only an omitted (or
	// within-lot-of-full) --qty flattens the resolved position — so a partial
	// close never removes more than the operator asked for.
	closeQty := pos.Quantity
	intentFullClose := true
	if in.Qty > 0 {
		if in.Qty > pos.Quantity {
			return res, manualFailf("error: --qty %.6f exceeds open position %.6f", in.Qty, pos.Quantity)
		}
		closeQty = in.Qty
		// Within 0.0001 (typical HL lot size) is treated as full close.
		if pos.Quantity-in.Qty > 0.0001 {
			intentFullClose = false
		}
	}
	if err := refuseIfPositionActionQueued(d, "manual-close", strategyID, sc.Symbol); err != nil {
		return res, err
	}

	// #1052 review: refuse a full close while an SL edit for this position is
	// still un-drained (the new SL OID lives only in the queued action; a full
	// close would cancel the stale OID and orphan the new stop-loss on-chain).
	// Fail closed on a check error. Partial close leaves the SL resting.
	if intentFullClose {
		if pending, perr := pendingSLActionExists(d.stateDB, strategyID, sc.Symbol); perr != nil {
			return res, manualFailf("error: could not check for queued stop-loss edits (%v) — refusing the full close to avoid orphaning an on-chain order; retry once the scheduler is reachable", perr)
		} else if pending {
			return res, manualFailf("error: a stop-loss edit for %s/%s is queued and not yet applied — run the scheduler (`--once`) or wait for the next cycle before a full close (closing now would orphan the new stop-loss on-chain)", strategyID, sc.Symbol)
		}
	}

	// Fix #2: only cancel the SL on a full close; leave it resting on partial close.
	cancelOID := int64(0)
	if intentFullClose {
		cancelOID = pos.StopLossOID
	}
	closeFullPosition := shouldCloseFullPosition(
		manualCloseIntentFraction(intentFullClose, closeQty, pos.Quantity),
		sc.Symbol,
		hyperliquidCloseScopeStrategies(d.cfg.Strategies),
	)
	var extraCancelOIDs []int64
	if intentFullClose {
		extraCancelOIDs = cloneInt64s(pos.TPOIDs)
	}

	execResult, stderr, execErr := d.execute(
		sc.Script, sc.Symbol, closeSide, closeQty,
		0, cancelOID, 0, "", 0, closeFullPosition, hlExecuteSnapshot{}, extraCancelOIDs...,
	)
	if stderr != "" {
		res.errf("HL close stderr: %s", stderr)
	}
	if execErr != nil {
		return res, manualFailf("error placing close order: %v", execErr)
	}
	if execResult.Error != "" {
		return res, manualFailf("error from HL: %s", execResult.Error)
	}
	// Cancel failures are non-fatal but leave reduce-only OIDs resting on-chain.
	if execResult.CancelStopLossError != "" {
		res.errf("warning: manual close cancel failed (non-fatal) for %s/%s: %s (sl_oid=%d tp_oids=%v) — verify HL on-chain triggers",
			strategyID, sc.Symbol, execResult.CancelStopLossError, cancelOID, extraCancelOIDs)
	}

	fill := execResult.Execution
	if fill == nil || fill.Fill == nil {
		return res, manualFailf("error: no fill returned from close execute")
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

	res.outf("Closed: %.6f %s @ $%.4f | PnL=$%.2f (fee=$%.4f)",
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
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		return res, manualFailf("error queuing close action: %v", err)
	}

	res.queued = true
	res.outf("Queued: close will be reflected in the dashboard after the next scheduler cycle.")
	return res, nil
}

// ---------------------------------------------------------------------------
// force-close

type forceCloseInputs struct {
	StrategyID string
	Qty        float64 // 0 = full strategy position
	DryRun     bool
}

// forceCloseCore is the shared live-HL-perps close path behind
// `go-trader force-close` (#1140) and POST /api/strategies/{id}/force-close.
// sym comes from lookupForceCloseStrategy.
func forceCloseCore(d manualCoreDeps, sc StrategyConfig, sym string, in forceCloseInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID
	if in.Qty < 0 {
		return res, manualUsagef("error: --qty must be non-negative, got %.6f", in.Qty)
	}
	if d.closer == nil {
		return res, manualFailf("error: hyperliquid closer unavailable")
	}

	view, loadErr := d.loadState(strategyID, sym)
	if loadErr != nil {
		return res, manualFailf("Failed to load state: %v", loadErr)
	}
	if !view.HasStrategy {
		return res, manualFailf("error: strategy state for %q not found", strategyID)
	}
	pos := view.Pos
	if pos == nil {
		return res, manualFailf("error: no open position found for %s/%s", strategyID, sym)
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) {
		return res, manualFailf("error: position %s/%s is owned by %q, not %q", strategyID, sym, pos.OwnerStrategyID, strategyID)
	}

	closeQty := pos.Quantity
	intentFullClose := true
	if in.Qty > 0 {
		if in.Qty > pos.Quantity {
			return res, manualFailf("error: --qty %.6f exceeds open position %.6f", in.Qty, pos.Quantity)
		}
		closeQty = in.Qty
		if pos.Quantity-in.Qty > 0.0001 {
			intentFullClose = false
		}
	}

	closeSide := "sell"
	if pos.Side == "short" {
		closeSide = "buy"
	}

	// Cross-process lock held (function-scoped defer) from before the guard
	// READS — the full-close SL check just below AND refuseIfPositionActionQueued
	// further down — through the pending-row INSERT, so a CLI racing the
	// dashboard, or a concurrent SL edit, can't interleave the check and submit.
	// Placed before the SL read (which precedes the dry-run return); a dry-run
	// neither submits nor inserts, so it needs no lock.
	if !in.DryRun {
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
	}

	closeFullPosition := false
	if intentFullClose {
		if pending, perr := pendingSLActionExists(d.stateDB, strategyID, sym); perr != nil {
			return res, manualFailf("error: could not check for queued stop-loss edits (%v) - refusing the full close to avoid orphaning an on-chain order; retry once the scheduler is reachable", perr)
		} else if pending {
			return res, manualFailf("error: a stop-loss edit for %s/%s is queued and not yet applied - run the scheduler (`--once`) or wait for the next cycle before a full close", strategyID, sym)
		}
		closeFullPosition = shouldCloseFullPosition(
			manualCloseIntentFraction(true, closeQty, pos.Quantity),
			sym,
			hyperliquidCloseScopeStrategies(d.cfg.Strategies),
		)
	}

	var cancelOIDs []int64
	if intentFullClose {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	var partialSz *float64
	if !closeFullPosition {
		partial := closeQty
		partialSz = &partial
	}

	if in.DryRun {
		mode := fmt.Sprintf("sized %.6f", closeQty)
		if closeFullPosition {
			mode = "full market_close"
		}
		res.outf("[dry-run] force-close %s: %s %.6f %s (current pos=%.6f, avg_cost=$%.4f, %s)",
			strategyID, closeSide, closeQty, sym, pos.Quantity, pos.AvgCost, mode)
		return res, nil
	}

	// #1260 review: refuse a second force-close while a position-changing action
	// (open/add/close) is queued for this strategy+symbol. force-close is
	// reduce-only + fill-based-qty so a double-fire is bounded, but keep the
	// guard symmetric with the other cores and block an add/close queued behind
	// it. Key on the args-derived sym the queued row uses (perps sc.Symbol is
	// empty).
	if err := refuseIfPositionActionQueued(d, "force-close", strategyID, sym); err != nil {
		return res, err
	}

	result, execErr := d.closer(sym, partialSz, cancelOIDs)
	if execErr != nil {
		return res, manualFailf("error placing force-close order: %v", execErr)
	}
	if result == nil || result.Close == nil {
		return res, manualFailf("error: no close result returned from HL")
	}
	if result.Error != "" {
		return res, manualFailf("error from HL: %s", result.Error)
	}
	if result.CancelStopLossError != "" {
		res.errf("warning: force-close cancel failed (non-fatal) for %s/%s: %s (oids=%v) - verify HL on-chain triggers",
			strategyID, sym, result.CancelStopLossError, cancelOIDs)
	}
	if result.Close.AlreadyFlat {
		return res, manualFailf("error: HL reports %s already flat; run the scheduler once to reconcile state", sym)
	}
	fill := result.Close.Fill
	if fill == nil {
		return res, manualFailf("error: no fill returned from force-close")
	}

	fillAvgPx := fill.AvgPx
	if fillAvgPx <= 0 {
		return res, manualFailf("error: invalid force-close fill price %.6f", fillAvgPx)
	}
	filledQty := fill.TotalSz
	if filledQty <= 0 {
		filledQty = closeQty
	}
	if filledQty <= 0 {
		return res, manualFailf("error: force-close fill quantity is zero")
	}
	fillFee := fill.Fee
	if filledQty > pos.Quantity+1e-9 {
		if fill.TotalSz > 0 {
			fillFee *= pos.Quantity / fill.TotalSz
		}
		res.errf("warning: force-close fill size %.6f exceeds virtual position %.6f for %s/%s; attributing only the virtual quantity",
			filledQty, pos.Quantity, strategyID, sym)
		filledQty = pos.Quantity
	} else if filledQty > pos.Quantity {
		filledQty = pos.Quantity
	}
	actualFullClose := intentFullClose && pos.Quantity-filledQty <= 0.0001
	var canceledSLOID int64
	var canceledTPOIDs []int64
	if !actualFullClose {
		canceledSLOID, canceledTPOIDs = forceCloseCanceledProtectionSnapshot(pos, hyperliquidSucceededCancelOIDs(result, cancelOIDs))
	}
	var exchangeOID string
	if fill.OID != 0 {
		exchangeOID = fmt.Sprintf("%d", fill.OID)
	}

	var realizedPnL float64
	if pos.Side == "long" {
		realizedPnL = filledQty * (fillAvgPx - pos.AvgCost)
	} else {
		realizedPnL = filledQty * (pos.AvgCost - fillAvgPx)
	}
	realizedPnL -= fillFee

	res.outf("Force-closed: %.6f %s @ $%.4f | PnL=$%.2f (fee=$%.4f)",
		filledQty, sym, fillAvgPx, realizedPnL, fillFee)
	// #1159: the scheduler owns the hedge leg exclusively. A manual force-close
	// of the primary leaves the hedge coherence to the next cycle's state-derived
	// hedge sync (primary flat/reduced → hedge closed/reduced), which is
	// restart-safe. No CLI-side hedge order.
	if sc.HedgeEnabled() {
		if hc := hedgeCoin(sc); hc != "" {
			res.outf("note: hedge leg %s will be reduced/closed by the scheduler on the next cycle (#1159)", hc)
		}
	}

	action := PendingManualAction{
		StrategyID:      strategyID,
		Action:          "close",
		Symbol:          sym,
		Side:            closeSide,
		Quantity:        filledQty,
		FillPrice:       fillAvgPx,
		FillFee:         fillFee,
		ExchangeOrderID: exchangeOID,
		RealizedPnL:     realizedPnL,
		IsFullClose:     actualFullClose,
		StopLossOID:     canceledSLOID,
		TPOIDs:          canceledTPOIDs,
		CreatedAt:       time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		return res, manualFailf("error queuing force-close action: %v", err)
	}

	res.queued = true
	res.outf("Queued: force-close will be reflected in the dashboard after the next scheduler cycle.")
	return res, nil
}

// ---------------------------------------------------------------------------
// manual-update-sl / manual-cancel-sl (#1050)

type manualSLInputs struct {
	StrategyID string
	Symbol     string  // "" → strategy's configured symbol
	Trigger    float64 // update-sl only
	DryRun     bool
}

// resolveManualSLTargetCore runs the shared SL-edit guards (kill switch,
// pending CB close, ownership, manualSLAutoManaged, pendingSLActionExists —
// all fail-closed) and returns the position snapshot + resolved symbol.
func resolveManualSLTargetCore(d manualCoreDeps, sc StrategyConfig, cmdName, strategyID, symbolFlag string) (*Position, string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(symbolFlag))
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimSpace(sc.Symbol))
	}
	if symbol == "" {
		return nil, "", manualUsagef("error: no --symbol provided and strategy %q has no configured symbol", strategyID)
	}

	view, err := d.loadState(strategyID, symbol)
	if err != nil {
		return nil, "", manualFailf("Failed to load state: %v", err)
	}

	// Removing/moving protection during a portfolio flatten or a pending
	// circuit-breaker close is unsafe and pointless — mirror manual-open's guards.
	if view.KillSwitch {
		return nil, "", manualFailf("error: portfolio kill switch is active — %s blocked", cmdName)
	}
	if view.HasStrategy && view.PendingCBClose {
		return nil, "", manualFailf("error: strategy has a pending circuit-breaker close — %s blocked", cmdName)
	}

	pos := view.Pos
	if pos == nil {
		return nil, "", manualFailf("error: no open position for %s/%s", strategyID, symbol)
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) {
		return nil, "", manualFailf("error: position %s/%s is owned by %q, not %q", strategyID, symbol, pos.OwnerStrategyID, strategyID)
	}

	// Block when the strategy's automated protection would revert the edit on
	// the next cycle.
	if managed, reason := manualSLAutoManaged(sc, pos); managed {
		return nil, "", manualFailf("error: %s for %s/%s — a manual stop-loss edit would be reverted on the next scheduler cycle.\n       To manage the stop-loss manually, opt the strategy out of auto-protection (set stop_loss_atr_mult: 0 and remove any trailing close).", reason, strategyID, symbol)
	}

	// Refuse a second SL edit while a prior one is still un-drained (#1052
	// review) — fail closed: a check error blocks the edit.
	if pending, err := pendingSLActionExists(d.stateDB, strategyID, symbol); err != nil {
		return nil, "", manualFailf("error: could not check for queued stop-loss edits (%v) — refusing to avoid orphaning an on-chain order; retry once the scheduler is reachable", err)
	} else if pending {
		return nil, "", manualFailf("error: a stop-loss edit for %s/%s is already queued and not yet applied — run the scheduler (`--once`) or wait for the next cycle before editing again (a second edit now would orphan the first stop-loss on-chain)", strategyID, symbol)
	}

	// Symmetric with the close cores' pendingSLActionExists refusal (#1260
	// review 5): a queued close/open/add means the position this edit targets
	// may be deleted (or reshaped) before the edit's row drains — the edit
	// would fire a redundant on-chain order against a flat position and leave
	// a permanently-stuck pending row. Fail closed on a check error.
	if pending, err := pendingManualActionExists(d.stateDB, strategyID, symbol, "open", "add", "close"); err != nil {
		return nil, "", manualFailf("error: could not check for queued position actions (%v) — refusing to avoid orphaning an on-chain order; retry once the scheduler is reachable", err)
	} else if pending {
		return nil, "", manualFailf("error: a position-changing action for %s/%s is already queued and not yet applied — wait for the next scheduler cycle before editing the stop-loss", strategyID, symbol)
	}

	return pos, symbol, nil
}

// manualUpdateSLCore is the shared cancel-then-queue SL move behind
// `go-trader manual-update-sl` and POST /api/strategies/{id}/update-sl.
func manualUpdateSLCore(d manualCoreDeps, sc StrategyConfig, in manualSLInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID
	if in.Trigger <= 0 {
		return res, manualUsagef("error: --trigger must be > 0")
	}

	// Cross-process lock held (function-scoped defer) from before the guard
	// READS inside resolveManualSLTargetCore (pending SL + pending position
	// checks) through the pending-row INSERT, so an SL edit racing a
	// close/open/add across processes can't interleave. Dry-run neither submits
	// nor inserts, so it needs no lock.
	if !in.DryRun {
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
	}

	pos, symbol, err := resolveManualSLTargetCore(d, sc, "manual-update-sl", strategyID, in.Symbol)
	if err != nil {
		return res, err
	}

	// Best-effort immediate-fill guard: a trigger on the wrong side of the mark
	// fires the moment it is placed. A failed mark fetch does not block.
	mark := 0.0
	if mids, err := d.fetchMids([]string{symbol}); err == nil {
		mark = mids[symbol]
	} else {
		res.errf("warning: could not fetch mark for immediate-fill check: %v", err)
	}
	if slTriggerWouldFillImmediately(pos.Side, in.Trigger, mark) {
		return res, manualFailf("error: trigger $%.4f would fill immediately against mark $%.4f for a %s position", in.Trigger, mark, pos.Side)
	}

	if in.DryRun {
		res.outf("[dry-run] manual-update-sl %s: %s stop-loss $%.4f -> $%.4f (qty %.6f, cancel OID=%d)",
			strategyID, symbol, pos.StopLossTriggerPx, in.Trigger, pos.Quantity, pos.StopLossOID)
		return res, nil
	}

	slResult, slStderr, slErr := d.updateSL(sc.Script, symbol, pos.Side, pos.Quantity, in.Trigger, pos.StopLossOID)
	if slStderr != "" {
		res.errf("SL update stderr: %s", slStderr)
	}
	if slErr != nil {
		// Subprocess failure — the cancel may have run before the failure; the
		// operator must verify on-chain.
		return res, manualFailf("error updating stop-loss: %v — the old stop-loss may have been cancelled without a replacement; verify protection on the HL UI before retrying.", slErr)
	}
	if slResult.Error != "" {
		return res, manualFailf("error from HL: %s", slResult.Error)
	}
	if slResult.StopLossFilledImmediately {
		// The new trigger fired on placement; the position closed on-chain. Do
		// NOT queue an update-sl — the next reconcile cycle books the close.
		return res, manualFailf("error: stop-loss filled immediately on placement — position closed on-chain; reconcile will adopt the close. Do not retry.")
	}
	if slResult.StopLossOID == 0 {
		// Placement returned no OID without raising. Distinguish naked from safe.
		if slPlacementFailureLeftNaked(slResult.CancelStopLossSucceeded, pos.StopLossOID) {
			return res, manualFailf("CRITICAL: stop-loss placement failed after the old order was removed (%s) — the position is now UNPROTECTED on-chain. Re-arm immediately (manual-update-sl) or close the position.", slResult.StopLossError)
		}
		return res, manualFailf("error: stop-loss replacement failed (%s); the previous stop-loss (OID=%d) is still resting on-chain — verify on the HL UI.", slResult.StopLossError, pos.StopLossOID)
	}

	newTrigger := slResult.StopLossTriggerPx
	if newTrigger == 0 {
		newTrigger = in.Trigger
	}
	res.outf("Stop-loss updated: %s %s -> $%.4f (OID=%d)", strategyID, symbol, newTrigger, slResult.StopLossOID)

	action := PendingManualAction{
		StrategyID:        strategyID,
		Action:            "update-sl",
		Symbol:            symbol,
		Side:              pos.Side,
		Quantity:          pos.Quantity,
		StopLossOID:       slResult.StopLossOID,
		StopLossTriggerPx: newTrigger,
		CreatedAt:         time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		return res, manualFailf("CRITICAL: stop-loss moved on-chain to $%.4f (OID=%d) but queue insert failed (%v); the scheduler still tracks the old OID until reconcile. Restart to resync.",
			newTrigger, slResult.StopLossOID, err)
	}

	res.queued = true
	res.outf("Queued: %s stop-loss update will sync to the dashboard after the next scheduler cycle.", strategyID)
	return res, nil
}

// manualCancelSLCore is the shared SL removal behind
// `go-trader manual-cancel-sl` and POST /api/strategies/{id}/cancel-sl.
func manualCancelSLCore(d manualCoreDeps, sc StrategyConfig, in manualSLInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID

	// Cross-process lock held (function-scoped defer) from before the guard
	// READS inside resolveManualSLTargetCore through the pending-row INSERT (see
	// manualUpdateSLCore). Dry-run neither submits nor inserts.
	if !in.DryRun {
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
	}

	pos, symbol, err := resolveManualSLTargetCore(d, sc, "manual-cancel-sl", strategyID, in.Symbol)
	if err != nil {
		return res, err
	}

	if pos.StopLossOID == 0 {
		return res, manualFailf("error: no resting stop-loss to cancel for %s/%s", strategyID, symbol)
	}

	if in.DryRun {
		res.outf("[dry-run] manual-cancel-sl %s: cancel %s stop-loss $%.4f (OID=%d)",
			strategyID, symbol, pos.StopLossTriggerPx, pos.StopLossOID)
		return res, nil
	}

	cancelResult, cancelStderr, cancelErr := d.cancelOrder(sc.Script, symbol, pos.StopLossOID)
	if cancelStderr != "" {
		res.errf("SL cancel stderr: %s", cancelStderr)
	}
	if cancelErr != nil {
		return res, manualFailf("error cancelling stop-loss: %v", cancelErr)
	}
	if cancelResult.Error != "" {
		return res, manualFailf("error from HL: %s", cancelResult.Error)
	}
	if !cancelResult.Cancelled {
		return res, manualFailf("error: HL did not confirm cancel of OID %d: %s", pos.StopLossOID, cancelResult.CancelError)
	}

	res.outf("Stop-loss cancelled: %s %s (was OID=%d @ $%.4f)", strategyID, symbol, pos.StopLossOID, pos.StopLossTriggerPx)

	action := PendingManualAction{
		StrategyID: strategyID,
		Action:     "cancel-sl",
		Symbol:     symbol,
		Side:       pos.Side,
		CreatedAt:  time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		return res, manualFailf("CRITICAL: stop-loss cancelled on-chain but queue insert failed (%v); the position is now UNPROTECTED and the scheduler still tracks the old OID. Re-arm protection or restart immediately.", err)
	}

	res.queued = true
	res.outf("Queued: %s stop-loss removal will sync to the dashboard after the next scheduler cycle.", strategyID)
	return res, nil
}
