package main


import (
	"fmt"
	"strings"
	"time"
)

type manualOutputLine struct {
	stderr bool
	text   string
}

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

func (r *manualCoreResult) uiMessage() string {
	var out []string
	for _, l := range r.lines {
		if !l.stderr {
			out = append(out, l.text)
		}
	}
	return strings.Join(out, "\n")
}

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

func manualCoreExitCode(err error) int {
	if ce, ok := err.(*manualCoreError); ok && ce.usage {
		return 2
	}
	return 1
}

type manualStateView struct {
	KillSwitch     bool
	HasStrategy    bool
	PendingCBClose bool
	DailyLossHold  bool
	DailyLossNote  string
	NotionalHold   bool
	NotionalNote   string
	ExposureCap      ExposureCapStatus
	ExposureCapAsset string
	Pos              *Position
}

func manualStateViewFromState(cfg *Config, state *AppState, strategyID, symbol string) manualStateView {
	v := manualStateView{KillSwitch: state.PortfolioRisk.KillSwitchActive}
	if cfg != nil {
		if st := evaluateDailyLossLimit(cfg.PortfolioRisk, state.Strategies, cfg.Strategies, time.Now().UTC()); st.Tripped {
			v.DailyLossHold = true
			v.DailyLossNote = dailyLossHoldDetail(st)
		}
		if held, detail := evaluateNotionalCapHold(cfg.PortfolioRisk, state.Strategies, nil); held {
			v.NotionalHold = true
			v.NotionalNote = detail
		}
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

type manualCoreDeps struct {
	cfg      *Config
	stateDB  *StateDB
	notifier *MultiNotifier

	loadState func(strategyID, symbol string) (manualStateView, error)

	execute     func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
	updateSL    func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error)
	cancelOrder func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)
	fetchMids   manualMarkFetcher
	closer      HyperliquidLiveCloser

	lockManualActions func() (release func(), err error)
}

func (d manualCoreDeps) acquireManualActionLock() (func(), error) {
	if d.lockManualActions == nil {
		return func() {}, nil
	}
	return d.lockManualActions()
}

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


type manualOpenInputs struct {
	StrategyID string
	Side       string
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
			if view.NotionalHold {
				return res, manualFailf("error: %s — manual-open blocked (closes and SL edits are unaffected)", view.NotionalNote)
			}
			if blocked, why := exposureCapManualEntryBlock(view.ExposureCap, view.ExposureCapAsset, side); blocked {
				return res, manualFailf("error: %s — manual-open (%s) blocked (closes and SL edits are unaffected)", why, side)
			}
			if view.ExposureCap.PVBasisMiss {
				res.errf("%s", exposureCapPVBasisMissWarning)
			}
		}
	}

	if !in.RecordOnly && !in.DryRun {
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
		if err := refuseIfPositionActionQueued(d, "manual-open", strategyID, sc.Symbol); err != nil {
			return res, err
		}
	}

	entryATR := in.ATR
	if in.RecordOnly && entryATR > 0 && in.FillPrice > 0 && entryATR > 0.5*in.FillPrice {
		return res, manualFailf("error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)", entryATR, in.FillPrice)
	}

	effectiveSLPct := 0.0
	if in.SLPct > 0 {
		effectiveSLPct = in.SLPct
	}

	script := sc.Script

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
		fillQty = in.Size
		resolvedFillPrice = in.FillPrice
		if entryATR > 0 && entryATR > 0.5*resolvedFillPrice {
			return res, manualFailf("error: --atr %.4f exceeds 50%% of fill price %.4f (plausibility guard)", entryATR, resolvedFillPrice)
		}
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

	ratchetFallbackNormalizePending := false
	if effectiveATRMult == 0 && !in.RecordOnly && strategyUsesTrailingTPRatchetClose(sc) &&
		sc.TrailingStopATRRegime != nil && sc.TrailingStopATRRegime.IsConfigured() {
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


type manualAddInputs struct {
	StrategyID string
	Size       float64
	Notional   float64
	Margin     float64
	RecordOnly bool
	FillPrice  float64
	DryRun     bool
}

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
	if view.NotionalHold {
		return res, manualFailf("error: %s — manual-add blocked (closes and SL edits are unaffected)", view.NotionalNote)
	}
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

	if !in.RecordOnly && !in.DryRun {
		unlock, lockErr := d.acquireManualActionLock()
		if lockErr != nil {
			return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
		}
		defer unlock()
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


type manualCloseInputs struct {
	StrategyID string
	Qty        float64
	DryRun     bool
}

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

	unlock, lockErr := d.acquireManualActionLock()
	if lockErr != nil {
		return res, manualFailf("error: %v — refusing to avoid double-firing an on-chain order", lockErr)
	}
	defer unlock()

	clearedQty, clearedAvgPx, err := clearRestingLimitRemainderForPositionAction(d, res, sc, "manual-close", strategyID, sc.Symbol)
	if err != nil {
		return res, err
	}
	if clearedQty > 0 {
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

	closeSide := "sell"
	if pos.Side == "short" {
		closeSide = "buy"
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
	if err := refuseIfPositionActionQueued(d, "manual-close", strategyID, sc.Symbol); err != nil {
		return res, err
	}

	if intentFullClose {
		if pending, perr := pendingSLActionExists(d.stateDB, strategyID, sc.Symbol); perr != nil {
			return res, manualFailf("error: could not check for queued stop-loss edits (%v) — refusing the full close to avoid orphaning an on-chain order; retry once the scheduler is reachable", perr)
		} else if pending {
			return res, manualFailf("error: a stop-loss edit for %s/%s is queued and not yet applied — run the scheduler (`--once`) or wait for the next cycle before a full close (closing now would orphan the new stop-loss on-chain)", strategyID, sc.Symbol)
		}
	}

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


type forceCloseInputs struct {
	StrategyID string
	Qty        float64
	DryRun     bool
}

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

	forceCloseCoupledHedgeLeg(d, sc, res, strategyID, sym, filledQty, pos.Quantity, actualFullClose)
	return res, nil
}

func forceCloseCoupledHedgeLeg(d manualCoreDeps, sc StrategyConfig, res *manualCoreResult, strategyID, primarySym string, primaryClosedQty, primaryQtyBefore float64, primaryFullClose bool) {
	if !HedgeEnabled(sc) || d.closer == nil {
		return
	}
	hCoin := hedgeCoin(sc)
	if hCoin == "" {
		return
	}
	hview, err := d.loadState(strategyID, hCoin)
	if err != nil || hview.Pos == nil || !hview.Pos.isHedgeLeg() || hview.Pos.Quantity <= 0 {
		return
	}
	hPos := hview.Pos
	if hPos.HedgeFor != primarySym {
		res.errf("warning: hedge leg on %s is stamped for primary %q, not %q — leaving it alone; reconcile manually",
			hCoin, hPos.HedgeFor, primarySym)
		return
	}

	closeQty := hPos.Quantity
	fullClose := true
	if !primaryFullClose && primaryQtyBefore > 0 {
		fraction := primaryClosedQty / primaryQtyBefore
		if fraction > 1 {
			fraction = 1
		}
		closeQty = hPos.Quantity * fraction
		fullClose = hPos.Quantity-closeQty <= 1e-9
		if closeQty <= 1e-9 {
			return
		}
	}
	var partialSz *float64
	if !fullClose {
		q := closeQty
		partialSz = &q
	} else {
		q := hPos.Quantity
		partialSz = &q
		closeQty = hPos.Quantity
	}

	result, execErr := d.closer(hCoin, partialSz, nil)
	if execErr != nil {
		res.errf("CRITICAL: force-close of the coupled hedge leg on %s failed: %v — the hedge is now OVERSIZED against %s. The scheduler will reconcile it next cycle; verify on-chain.", hCoin, execErr, primarySym)
		return
	}
	if result == nil || result.Close == nil || result.Error != "" {
		msg := "no close result"
		if result != nil && result.Error != "" {
			msg = result.Error
		}
		res.errf("CRITICAL: force-close of the coupled hedge leg on %s failed: %s — the hedge is now OVERSIZED against %s. The scheduler will reconcile it next cycle; verify on-chain.", hCoin, msg, primarySym)
		return
	}
	if result.Close.AlreadyFlat {
		res.errf("warning: HL reports the %s hedge leg already flat; the scheduler will clear the virtual leg next cycle.", hCoin)
		return
	}
	fill := result.Close.Fill
	if fill == nil || fill.AvgPx <= 0 {
		res.errf("CRITICAL: the %s hedge close returned no usable fill — the hedge may be OVERSIZED against %s. Verify on-chain.", hCoin, primarySym)
		return
	}
	filled := fill.TotalSz
	if filled <= 0 {
		filled = closeQty
	}
	if filled > hPos.Quantity {
		filled = hPos.Quantity
	}
	var pnl float64
	if hPos.Side == "long" {
		pnl = filled * (fill.AvgPx - hPos.AvgCost)
	} else {
		pnl = filled * (hPos.AvgCost - fill.AvgPx)
	}
	pnl -= fill.Fee

	var oid string
	if fill.OID != 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	action := PendingManualAction{
		StrategyID:      strategyID,
		Action:          "close",
		Symbol:          hCoin,
		Side:            closeTradeSide(hPos.Side),
		Quantity:        filled,
		FillPrice:       fill.AvgPx,
		FillFee:         fill.Fee,
		ExchangeOrderID: oid,
		RealizedPnL:     pnl,
		IsFullClose: hPos.Quantity-filled <= 1e-9,
		CreatedAt:   time.Now().UTC(),
	}
	if err := d.stateDB.InsertPendingManualAction(action); err != nil {
		res.errf("CRITICAL: the %s hedge leg was closed ON-CHAIN but queuing its bookkeeping row failed: %v — virtual state now overstates the hedge. Run the scheduler to reconcile.", hCoin, err)
		return
	}
	res.outf("Force-closed coupled hedge: %.6f %s @ $%.4f | PnL=$%.2f (fee=$%.4f)", filled, hCoin, fill.AvgPx, pnl, fill.Fee)
}


type manualSLInputs struct {
	StrategyID string
	Symbol     string
	Trigger    float64
	DryRun     bool
}

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

	if managed, reason := manualSLAutoManaged(sc, pos); managed {
		return nil, "", manualFailf("error: %s for %s/%s — a manual stop-loss edit would be reverted on the next scheduler cycle.\n       To manage the stop-loss manually, opt the strategy out of auto-protection (set stop_loss_atr_mult: 0 and remove any trailing close).", reason, strategyID, symbol)
	}

	if pending, err := pendingSLActionExists(d.stateDB, strategyID, symbol); err != nil {
		return nil, "", manualFailf("error: could not check for queued stop-loss edits (%v) — refusing to avoid orphaning an on-chain order; retry once the scheduler is reachable", err)
	} else if pending {
		return nil, "", manualFailf("error: a stop-loss edit for %s/%s is already queued and not yet applied — run the scheduler (`--once`) or wait for the next cycle before editing again (a second edit now would orphan the first stop-loss on-chain)", strategyID, symbol)
	}

	if pending, err := pendingManualActionExists(d.stateDB, strategyID, symbol, "open", "add", "close"); err != nil {
		return nil, "", manualFailf("error: could not check for queued position actions (%v) — refusing to avoid orphaning an on-chain order; retry once the scheduler is reachable", err)
	} else if pending {
		return nil, "", manualFailf("error: a position-changing action for %s/%s is already queued and not yet applied — wait for the next scheduler cycle before editing the stop-loss", strategyID, symbol)
	}

	return pos, symbol, nil
}

func manualUpdateSLCore(d manualCoreDeps, sc StrategyConfig, in manualSLInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID
	if in.Trigger <= 0 {
		return res, manualUsagef("error: --trigger must be > 0")
	}

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
		return res, manualFailf("error updating stop-loss: %v — the old stop-loss may have been cancelled without a replacement; verify protection on the HL UI before retrying.", slErr)
	}
	if slResult.Error != "" {
		return res, manualFailf("error from HL: %s", slResult.Error)
	}
	if slResult.StopLossFilledImmediately {
		return res, manualFailf("error: stop-loss filled immediately on placement — position closed on-chain; reconcile will adopt the close. Do not retry.")
	}
	if slResult.StopLossOID == 0 {
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

func manualCancelSLCore(d manualCoreDeps, sc StrategyConfig, in manualSLInputs) (*manualCoreResult, error) {
	res := &manualCoreResult{}
	strategyID := in.StrategyID

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
