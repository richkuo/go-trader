package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	entryRecoveryGrace         = 2 * time.Minute
	entryRecoveryFillLookback  = 2 * time.Minute
	entryRecoveryQueryInterval = time.Minute
	entryRecoveryQueryTimeout  = 45 * time.Second
)

type entryFillCoverage struct {
	Complete     bool    `json:"complete"`
	Reason       string  `json:"reason,omitempty"`
	FromMs       int64   `json:"from_ms"`
	ThroughMs    int64   `json:"through_ms"`
	Pages        int     `json:"pages"`
	Rows         int     `json:"rows"`
	MatchedCount int     `json:"matched_count"`
	MatchedQty   float64 `json:"matched_qty"`
	Source       string  `json:"source,omitempty"`
}

type entryRecoveryOrder struct {
	Found         bool    `json:"found"`
	Status        string  `json:"status,omitempty"`
	OrderID       string  `json:"order_id,omitempty"`
	OrigSz        float64 `json:"orig_sz,omitempty"`
	RemainingSz   float64 `json:"remaining_sz,omitempty"`
	ExecutedSz    float64 `json:"executed_sz"`
	Terminal      bool    `json:"terminal"`
	TriggerPx     float64 `json:"trigger_px,omitempty"`
	FillQty       float64 `json:"fill_qty"`
	FillNotional  float64 `json:"fill_notional"`
	FillFee       float64 `json:"fill_fee"`
	FillCount     int     `json:"fill_count"`
	FillAvgPx     float64 `json:"fill_avg_px"`
	FillsComplete bool    `json:"fills_complete"`

	Coverage *entryFillCoverage `json:"coverage,omitempty"`
}

type entryRecoveryResult struct {
	Platform    string              `json:"platform"`
	Account     string              `json:"account,omitempty"`
	Order       *entryRecoveryOrder `json:"order"`
	Stop        *entryRecoveryOrder `json:"stop,omitempty"`
	CheckedAtMs int64               `json:"checked_at_ms"`
	Error       string              `json:"error,omitempty"`
}

func parseEntryRecoveryOutput(stdout []byte, stderr string, runErr error) (*entryRecoveryResult, string, error) {
	var res entryRecoveryResult
	if err := json.Unmarshal(stdout, &res); err != nil {
		if runErr != nil {
			return nil, stderr, fmt.Errorf("recovery query error: %w (stderr: %s)", runErr, stderr)
		}
		return nil, stderr, fmt.Errorf("parse recovery output: %w (stdout: %s)", err, string(stdout))
	}
	if runErr != nil {
		why := res.Error
		if why == "" {
			why = stderr
		}
		return &res, stderr, fmt.Errorf("recovery query error: %w (%s)", runErr, why)
	}
	if res.Error != "" {
		return &res, stderr, fmt.Errorf("recovery query error: %s", res.Error)
	}
	return &res, stderr, nil
}

func buildHyperliquidRecoverEntryArgs(symbol, cloid, stopCloid string, sinceMs int64) []string {
	args := []string{
		"--recover-entry",
		"--symbol=" + symbol,
		"--cloid=" + cloid,
		fmt.Sprintf("--since-ms=%d", sinceMs),
		"--mode=live",
	}
	if stopCloid != "" {
		args = append(args, "--stop-cloid="+stopCloid)
	}
	return args
}

func RunHyperliquidRecoverEntry(script, symbol, cloid, stopCloid string, sinceMs int64) (*entryRecoveryResult, string, error) {
	stdout, stderr, err := runPythonWithTimeout(shutdownReadOnlyCtx, script, buildHyperliquidRecoverEntryArgs(symbol, cloid, stopCloid, sinceMs), nil, entryRecoveryQueryTimeout)
	return parseEntryRecoveryOutput(stdout, string(stderr), err)
}

func buildOKXRecoverEntryArgs(symbol, instType, clientOrderID string, sinceMs int64) []string {
	return []string{
		"--recover-entry",
		"--symbol=" + symbol,
		"--inst-type=" + instType,
		"--client-order-id=" + clientOrderID,
		fmt.Sprintf("--since-ms=%d", sinceMs),
		"--mode=live",
	}
}

func RunOKXRecoverEntry(script, symbol, instType, clientOrderID string, sinceMs int64) (*entryRecoveryResult, string, error) {
	stdout, stderr, err := runPythonWithTimeout(shutdownReadOnlyCtx, script, buildOKXRecoverEntryArgs(symbol, instType, clientOrderID, sinceMs), nil, entryRecoveryQueryTimeout)
	return parseEntryRecoveryOutput(stdout, string(stderr), err)
}

var runHyperliquidRecoverEntryFn = RunHyperliquidRecoverEntry
var runOKXRecoverEntryFn = RunOKXRecoverEntry

type entryRecoveryVerdictKind int

const (
	entryRecoveryHold entryRecoveryVerdictKind = iota
	entryRecoveryNoFill
	entryRecoverySettling
	entryRecoveryFilled
)

type entryRecoveryVerdict struct {
	Kind     entryRecoveryVerdictKind
	Reason   string
	Evidence entryIntentEvidence
	Wait     bool
}

func classifyEntryRecovery(in *entryIntent, req entryIntentRequest, res *entryRecoveryResult, now time.Time) entryRecoveryVerdict {
	ev := entryIntentEvidence{ObservedAtMs: now.UnixMilli(), RecoveredAfter: true}
	if res == nil || res.Error != "" || res.Order == nil {
		why := "the venue query returned no order evidence"
		if res != nil && res.Error != "" {
			why = res.Error
		}
		return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: why}
	}
	stored := entryStoredExecutionProof(in)
	o := res.Order
	ev.VenueOrderID = o.OrderID
	ev.OrderStatus = o.Status
	if !o.Found {
		if o.FillQty != 0 || o.FillCount != 0 || (o.Coverage != nil && (o.Coverage.MatchedCount != 0 || o.Coverage.MatchedQty != 0)) {
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the venue reports fills for the client order id but no order"}
		}
		if stored != "" {
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the venue has no order for the client order id, but " + stored + " proves execution; an empty venue result cannot release it"}
		}
		audit, why := proveEntryAbsence(in, req, res, now)
		if why != "" {
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the venue has no order for the client order id, but that does not prove the order was never accepted: " + why}
		}
		ev.Outcome = "expired_unaccepted"
		ev.Detail = fmt.Sprintf("no order or fill carries the client order id after its venue deadline %s; the fill history from %s through %s is complete", formatClosedBarMs(req.ExpiresAfterMs), formatClosedBarMs(audit.WindowStartMs), formatClosedBarMs(audit.OrderCoverage.ThroughMs))
		ev.Absence = audit
		return entryRecoveryVerdict{Kind: entryRecoveryNoFill, Evidence: ev}
	}
	if in.ConfirmedQty > 0 && o.FillQty < in.ConfirmedQty-math.Max(1e-9, in.ConfirmedQty*1e-6) {
		return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: fmt.Sprintf("order %s now shows %.8f filled, less than the saved proven fill %.8f", o.OrderID, o.FillQty, in.ConfirmedQty)}
	}
	if !o.Terminal {
		if o.FillQty > 0 && o.FillsComplete && finitePositive(o.FillAvgPx) {
			ev.Outcome = "partial_open_order"
			ev.FilledQty = o.FillQty
			ev.AvgPx = o.FillAvgPx
			ev.Fee = o.FillFee
			return entryRecoveryVerdict{Kind: entryRecoverySettling, Evidence: ev, Reason: fmt.Sprintf("order %s is still %s with %.8f filled", o.OrderID, o.Status, o.FillQty)}
		}
		return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: fmt.Sprintf("order %s is still %s", o.OrderID, o.Status)}
	}
	if o.ExecutedSz <= 0 && o.FillQty <= 0 {
		if stored != "" {
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: fmt.Sprintf("order %s ended %s with no execution, but %s contradicts it", o.OrderID, o.Status, stored)}
		}
		if o.Coverage != nil && (o.Coverage.MatchedCount != 0 || o.Coverage.MatchedQty != 0) {
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: fmt.Sprintf("order %s ended %s with no execution, but the fill history holds %d fill(s) for the client order id", o.OrderID, o.Status, o.Coverage.MatchedCount)}
		}
		ev.Outcome = "terminal_zero_fill"
		ev.Detail = fmt.Sprintf("order %s ended %s with no execution", o.OrderID, o.Status)
		return entryRecoveryVerdict{Kind: entryRecoveryNoFill, Evidence: ev}
	}
	if !o.FillsComplete || !finitePositive(o.FillQty) || !finitePositive(o.FillAvgPx) || math.IsNaN(o.FillFee) || math.IsInf(o.FillFee, 0) {
		return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: fmt.Sprintf("order %s executed %.8f but the matched fill history (%.8f over %d fill(s)) does not cover it", o.OrderID, o.ExecutedSz, o.FillQty, o.FillCount)}
	}
	ev.Outcome = "filled"
	ev.FilledQty = o.FillQty
	ev.AvgPx = o.FillAvgPx
	ev.Fee = o.FillFee
	if in.Provider == entryProviderHyperliquid && req.StopLossPct > 0 && in.InitialStopClientID != "" {
		s := res.Stop
		switch {
		case s == nil:
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the initial stop lookup returned no evidence"}
		case s.Found && !s.Terminal:
			ev.StopOrderID = s.OrderID
			ev.StopStatus = s.Status
			ev.StopTriggerPx = s.TriggerPx
		case s.Found && s.Status == "filled":
			if !s.FillsComplete || !finitePositive(s.TriggerPx) {
				return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the initial stop filled but its fill history is incomplete"}
			}
			ev.StopOrderID = s.OrderID
			ev.StopStatus = "filled"
			ev.StopTriggerPx = s.TriggerPx
		case s.Found:
			ev.StopStatus = s.Status
		case s.FillQty > 0:
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the venue reports fills for the initial stop client id but no order"}
		case req.StopExpiresAfterMs > 0 && now.UnixMilli() > req.StopExpiresAfterMs+entryRecoveryGrace.Milliseconds():
			ev.StopStatus = "not_placed"
		default:
			return entryRecoveryVerdict{Kind: entryRecoveryHold, Reason: "the initial stop deadline has not passed"}
		}
	}
	return entryRecoveryVerdict{Kind: entryRecoveryFilled, Evidence: ev}
}

func entryRecoveryNotBefore(in *entryIntent, req entryIntentRequest) int64 {
	if in.Provider != entryProviderHyperliquid {
		return 0
	}
	deadline := req.ExpiresAfterMs
	if req.StopExpiresAfterMs > deadline {
		deadline = req.StopExpiresAfterMs
	}
	if deadline <= 0 {
		return 0
	}
	return deadline + entryRecoveryGrace.Milliseconds()
}

func entryRecoveryWindowStart(in *entryIntent) int64 {
	return in.CreatedAtMs - entryRecoveryFillLookback.Milliseconds()
}

func queryEntryRecovery(in *entryIntent, req entryIntentRequest, script string) (*entryRecoveryResult, error) {
	since := entryRecoveryWindowStart(in)
	switch in.Provider {
	case entryProviderHyperliquid:
		res, _, err := runHyperliquidRecoverEntryFn(script, in.Symbol, in.ClientOrderID, in.InitialStopClientID, since)
		return res, err
	case entryProviderOKX:
		res, _, err := runOKXRecoverEntryFn(script, in.Symbol, in.InstrumentType, in.ClientOrderID, since)
		return res, err
	}
	return nil, fmt.Errorf("provider %q has no recovery query", in.Provider)
}

type entryRecoveryJob struct {
	app    *AppState
	sc     StrategyConfig
	state  *StrategyState
	intent *entryIntent
	snap   entryIntent
}

func (job *entryRecoveryJob) staleLocked() string {
	return entryIntentStaleLocked(job.app, job.state, job.intent, &job.snap)
}

func (job *entryRecoveryJob) adoptLocked() {
	job.snap = *job.intent
}

var entryRecoveryAlerted sync.Map

var entryRecoveryLastQuery sync.Map

func recoverEntryIntents(cfg *Config, state *AppState, store *StateStore, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager, now time.Time) {
	if cfg == nil || state == nil || store == nil {
		return
	}
	releaseHeldLiveEntryAttempts()
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	type pendingRecovery struct {
		sc       StrategyConfig
		state    *StrategyState
		intentID string
	}
	var pending []pendingRecovery
	mu.RLock()
	for _, id := range sortedEntryGuardStrategyIDs(state) {
		sc, ok := byID[id]
		if !ok {
			continue
		}
		s := state.Strategies[id]
		for _, intentID := range s.EntryGuard.sortedIntentIDs() {
			pending = append(pending, pendingRecovery{sc: sc, state: s, intentID: intentID})
		}
	}
	mu.RUnlock()
	for _, p := range pending {
		logger := entryRecoveryLogger(logMgr, p.sc.ID)
		release, ok := tryLockEntryAttempt(p.sc.ID)
		if !ok {
			logger.Info("Live entry %s: another resolution of this strategy's entry attempt is in progress; recovery retries next cycle (#1771)", p.intentID)
			logger.Close()
			continue
		}
		var job *entryRecoveryJob
		mu.RLock()
		if state.Strategies[p.sc.ID] == p.state && p.state.EntryGuard != nil {
			if in := p.state.EntryGuard.Intents[p.intentID]; in != nil {
				job = &entryRecoveryJob{app: state, sc: p.sc, state: p.state, intent: in, snap: *in}
			}
		}
		mu.RUnlock()
		if job != nil {
			recoverOneEntryIntent(cfg, store, mu, job, notifier, logger, now)
		}
		release()
		logger.Close()
	}
}

func entryRecoveryLogger(logMgr *LogManager, id string) *StrategyLogger {
	if logMgr != nil {
		if l, err := logMgr.GetStrategyLogger(id); err == nil && l != nil {
			return l
		}
	}
	return &StrategyLogger{stratID: id, writer: os.Stdout}
}

func logStaleEntryRecovery(logger *StrategyLogger, in *entryIntent, why string) {
	if logger != nil {
		logger.Warn("Live entry %s: a stale recovery result was discarded (%s); nothing was booked or changed (#1771)", in.IntentID, why)
	}
}

func recoverOneEntryIntent(cfg *Config, store *StateStore, mu *sync.RWMutex, job *entryRecoveryJob, notifier *MultiNotifier, logger *StrategyLogger, now time.Time) {
	in := job.intent
	s := job.state
	sc := job.sc
	switch {
	case job.snap.replayPending():
		mu.Lock()
		if why := job.staleLocked(); why != "" {
			mu.Unlock()
			logStaleEntryRecovery(logger, in, why)
			return
		}
		publishEntryReplayLocked(store, s, in, logger)
		mu.Unlock()
		return
	case job.snap.Status == entryIntentReserved:
		resolveEntryIntentNoFill(store, s, mu, in, entryIntentEvidence{Outcome: "never_submitted", Detail: "the attempt was reserved but its submission was never recorded, so no order was sent"}, logger, job.staleLocked)
		return
	case !job.snap.unresolved():
		return
	}
	req, err := parseEntryIntentRequest(&job.snap)
	if err != nil {
		job.noteHold(store, mu, err.Error(), notifier, logger)
		return
	}
	if notBefore := entryRecoveryNotBefore(&job.snap, req); notBefore > 0 && now.UnixMilli() <= notBefore {
		job.noteHold(store, mu, fmt.Sprintf("waiting for the venue order deadline (recovery after %s)", formatClosedBarMs(notBefore)), notifier, logger)
		return
	}
	if last, ok := entryRecoveryLastQuery.Load(in.IntentID); ok && now.Sub(last.(time.Time)) < entryRecoveryQueryInterval {
		return
	}
	entryRecoveryLastQuery.Store(in.IntentID, now)
	script := req.Script
	if strings.TrimSpace(script) == "" {
		script = sc.Script
	}
	res, qerr := queryEntryRecovery(&job.snap, req, script)
	if qerr != nil {
		job.noteHold(store, mu, "the venue recovery query failed: "+qerr.Error(), notifier, logger)
		return
	}
	verdict := classifyEntryRecovery(&job.snap, req, res, time.Now())
	switch verdict.Kind {
	case entryRecoveryNoFill:
		resolveEntryIntentNoFill(store, s, mu, in, verdict.Evidence, logger, job.staleLocked)
	case entryRecoverySettling:
		mu.Lock()
		if why := job.staleLocked(); why != "" {
			mu.Unlock()
			logStaleEntryRecovery(logger, in, why)
			return
		}
		markEntryConsumedLocked(s.EntryGuard, in.key(), in.BarOpenMs, now)
		in.Status = entryIntentSettling
		in.EvidenceJSON = marshalEntryJSON(verdict.Evidence)
		if verdict.Evidence.VenueOrderID != "" {
			in.VenueOrderID = verdict.Evidence.VenueOrderID
		}
		in.ConfirmedQty = verdict.Evidence.FilledQty
		in.ConfirmedNotional = verdict.Evidence.FilledQty * verdict.Evidence.AvgPx
		in.ConfirmedFee = verdict.Evidence.Fee
		in.UpdatedAtMs = now.UnixMilli()
		perr := store.PersistEntryGuardProgress(s)
		job.adoptLocked()
		mu.Unlock()
		if perr != nil && logger != nil {
			logger.Error("CRITICAL: saving the partial fill of entry %s failed: %v", in.IntentID, perr)
		}
		job.noteHold(store, mu, verdict.Reason, notifier, logger)
	case entryRecoveryFilled:
		trades := bookRecoveredEntry(cfg, store, mu, job, req, verdict.Evidence, notifier, logger)
		if trades > 0 {
			sendTradeAlerts(sc, s, trades, mu, notifier, cfg.Regime)
		}
	default:
		job.noteHold(store, mu, verdict.Reason, notifier, logger)
	}
}

func (job *entryRecoveryJob) noteHold(store *StateStore, mu *sync.RWMutex, reason string, notifier *MultiNotifier, logger *StrategyLogger) {
	in := job.intent
	sc := job.sc
	mu.Lock()
	if why := job.staleLocked(); why != "" {
		mu.Unlock()
		logStaleEntryRecovery(logger, in, why)
		return
	}
	changed := in.HoldReason != reason
	in.HoldReason = reason
	var perr error
	if changed {
		perr = store.PersistEntryGuardProgress(job.state)
	}
	mu.Unlock()
	if perr != nil && logger != nil {
		logger.Error("Saving the hold reason of entry %s failed: %v", in.IntentID, perr)
	}
	if logger != nil && changed {
		logger.Warn("Live entry %s (%s %s, client order %s) stays held: %s — no new entry is sent for this strategy; exits, stops and protection continue (#1769)", in.IntentID, in.Symbol, in.TargetSide, in.ClientOrderID, reason)
	}
	if _, loaded := entryRecoveryAlerted.LoadOrStore(in.IntentID, true); !loaded && notifier != nil && notifier.HasOwner() {
		notifier.SendOwnerDM(fmt.Sprintf("**LIVE ENTRY UNRESOLVED** [%s] %s %s entry %s (client order %s, candle %s): %s. New entries for this strategy stay held until venue evidence resolves it; exits, stops and protection continue. The owner can request a fresh check with /%s%s.",
			sc.ID, in.Symbol, in.TargetSide, in.IntentID, in.ClientOrderID, formatClosedBarMs(in.BarOpenMs), reason, commandPrefix, entryOperatorCommandName))
	}
}

func bookRecoveredEntry(cfg *Config, store *StateStore, mu *sync.RWMutex, job *entryRecoveryJob, req entryIntentRequest, ev entryIntentEvidence, notifier *MultiNotifier, logger *StrategyLogger) int {
	in := job.intent
	s := job.state
	sc := job.sc
	mu.Lock()
	if why := job.staleLocked(); why != "" {
		mu.Unlock()
		logStaleEntryRecovery(logger, in, why)
		return 0
	}
	pos := s.Positions[in.Symbol]
	conflict := ""
	absorbedQty, absorbedAvg := 0.0, 0.0
	switch req.Kind {
	case entryIncreaseOpen.String():
		if pos != nil && pos.Quantity > 0 {
			conflict = fmt.Sprintf("the book already holds %.8f %s %s", pos.Quantity, pos.Side, in.Symbol)
		}
	case entryIncreaseAdd.String():
		switch {
		case pos == nil || pos.Side != in.TargetSide:
			conflict = "the position changed since the scale-in was sent"
		case entryRecoveryQtyMatches(pos.Quantity, req.PrevPosQty):
		case req.PrevPosQty > 0 && finitePositive(req.PrevAvgCost) && entryRecoveryQtyMatches(pos.Quantity, req.PrevPosQty+ev.FilledQty):
			absorbedQty, absorbedAvg = pos.Quantity, pos.AvgCost
			pos.Quantity = req.PrevPosQty
			pos.AvgCost = req.PrevAvgCost
			if logger != nil {
				logger.Info("Live scale-in %s: the account sync already set the book to the on-chain %.8f %s (before the add %.8f, fill %.8f); the add is booked once from the pre-add book (#1769)", in.IntentID, absorbedQty, in.Symbol, req.PrevPosQty, ev.FilledQty)
			}
		default:
			conflict = fmt.Sprintf("the position changed since the scale-in was sent (book %.8f, before the add %.8f, fill %.8f)", pos.Quantity, req.PrevPosQty, ev.FilledQty)
		}
	default:
		conflict = fmt.Sprintf("entry kind %q has no recovery booking", req.Kind)
	}
	if conflict != "" {
		perr := persistUnbookedEntryProofLocked(store, s, in, ev, conflict)
		job.adoptLocked()
		mu.Unlock()
		if perr != nil && logger != nil {
			logger.Error("CRITICAL: saving the proven fill of entry %s failed: %v — the attempt stays held in memory (#1771)", in.IntentID, perr)
		}
		job.noteHold(store, mu, "a fill was proven but "+conflict, notifier, logger)
		return 0
	}
	restoreAbsorbed := func() {
		if absorbedQty > 0 && pos.Quantity == req.PrevPosQty {
			pos.Quantity = absorbedQty
			pos.AvgCost = absorbedAvg
		}
	}
	fromIdx := len(s.TradeHistory)
	endCapture := beginLiveEntrySettlement(s)
	signalStr := "BUY"
	if req.Signal == -1 {
		signalStr = "SELL"
	}
	indicators := map[string]interface{}{}
	if req.EntryATR > 0 {
		indicators["atr"] = req.EntryATR
	}
	fields := StrategyDecisionFields{ClosedBar: &ClosedBarDecision{Enabled: true, CutoffMs: in.CutoffMs, DecisionBoundaryMs: in.DecisionBoundaryMs, BarOpenMs: in.BarOpenMs, IntervalMs: in.IntervalMs}}
	trades := 0
	switch in.Provider {
	case entryProviderHyperliquid:
		fill := &HyperliquidFill{AvgPx: ev.AvgPx, TotalSz: ev.FilledQty, Fee: ev.Fee}
		if oid, err := parseInt64(ev.VenueOrderID); err == nil {
			fill.OID = oid
		}
		er := &HyperliquidExecuteResult{OrderOutcome: "filled", Execution: &HyperliquidExecution{Action: strings.ToLower(signalStr), Symbol: in.Symbol, Size: req.Size, Fill: fill}}
		switch ev.StopStatus {
		case "filled":
			fill.StopLossTriggerPx = ev.StopTriggerPx
			er.StopLossFilledImmediately = true
		case "open", "triggered":
			if oid, err := parseInt64(ev.StopOrderID); err == nil && oid > 0 {
				fill.StopLossOID = oid
				fill.StopLossTriggerPx = ev.StopTriggerPx
			}
		}
		result := &HyperliquidResult{StrategyDecisionFields: fields, Symbol: in.Symbol, Signal: req.Signal, Price: ev.AvgPx, Indicators: indicators}
		var openTrade *Trade
		if req.Kind == entryIncreaseAdd.String() {
			trades, _, openTrade, _ = executeHyperliquidScaleInDeferredOpen(sc, s, result, er, signalStr, ev.AvgPx, ev.FilledQty, logger)
		} else {
			trades, _, openTrade, _ = executeHyperliquidResultDeferredOpen(sc, s, result, er, signalStr, ev.AvgPx, cfg.Regime, cfg, HurstGateDecision{}, logger)
		}
		if openTrade != nil {
			recordPositionOpen(s, sc, openTrade, s.Positions[in.Symbol])
		}
	case entryProviderOKX:
		db, err := store.dbForStrategy(sc.ID)
		if err != nil {
			endCapture()
			restoreAbsorbed()
			mu.Unlock()
			job.noteHold(store, mu, "the owning state file is unavailable: "+err.Error(), notifier, logger)
			return 0
		}
		result := &OKXResult{StrategyDecisionFields: fields, Symbol: in.Symbol, Signal: req.Signal, Price: ev.AvgPx, Indicators: indicators}
		er := &OKXExecuteResult{OrderOutcome: "filled", Execution: &OKXExecution{Action: strings.ToLower(signalStr), Symbol: in.Symbol, Size: req.Size, Fill: &OKXFill{AvgPx: ev.AvgPx, TotalSz: ev.FilledQty, Fee: ev.Fee, OID: ev.VenueOrderID}}}
		trades, _, _ = executeOKXResult(sc, s, db, result, er, signalStr, ev.AvgPx, cfg.Regime, cfg, HurstGateDecision{}, logger)
	}
	captured := endCapture()
	if trades == 0 {
		restoreAbsorbed()
	}
	committed := commitLiveEntrySettlementLocked(store, s, in, fromIdx, captured, ev, logger)
	mu.Unlock()
	if committed && logger != nil {
		logger.Warn("Live entry %s recovered from venue evidence after the original outcome was lost: %.8f %s @ $%.4f booked once (#1769)", in.IntentID, ev.FilledQty, in.TargetSide, ev.AvgPx)
	}
	if committed && notifier != nil && notifier.HasOwner() {
		notifier.SendOwnerDM(fmt.Sprintf("**LIVE ENTRY RECOVERED** [%s] %s %s entry %s booked from venue evidence: %.8f @ $%.4f (order %s). Check protection on the next cycle.",
			sc.ID, in.Symbol, in.TargetSide, in.IntentID, ev.FilledQty, ev.AvgPx, ev.VenueOrderID))
	}
	return trades
}

func persistUnbookedEntryProofLocked(store *StateStore, s *StrategyState, in *entryIntent, ev entryIntentEvidence, conflict string) error {
	proof := ev
	proof.Outcome = "filled_unbooked"
	proof.Detail = "the fill was proven but " + conflict
	proof.ObservedAtMs = time.Now().UnixMilli()
	in.EvidenceJSON = marshalEntryJSON(proof)
	if ev.VenueOrderID != "" {
		in.VenueOrderID = ev.VenueOrderID
	}
	in.ConfirmedQty = ev.FilledQty
	in.ConfirmedNotional = ev.FilledQty * ev.AvgPx
	in.ConfirmedFee = ev.Fee
	in.UpdatedAtMs = proof.ObservedAtMs
	return store.PersistEntryGuardProgress(s)
}

func entryRecoveryQtyMatches(book, want float64) bool {
	return math.Abs(book-want) <= 1e-9*math.Max(1, want)
}

func parseInt64(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v, err
}
