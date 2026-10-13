package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const entryOperatorCommandName = "resolve-entry"

var entryIntentIDPattern = regexp.MustCompile(`^ei-[0-9a-f]{24}$`)

var entryAttemptLocks sync.Map

func tryLockEntryAttempt(strategyID string) (func(), bool) {
	v, _ := entryAttemptLocks.LoadOrStore(strategyID, &sync.Mutex{})
	m := v.(*sync.Mutex)
	if !m.TryLock() {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(m.Unlock) }, true
}

var (
	heldLiveEntryAttemptsMu sync.Mutex
	heldLiveEntryAttempts   = map[*entryIntent]func(){}
)

func holdLiveEntryAttempt(in *entryIntent, release func()) {
	heldLiveEntryAttemptsMu.Lock()
	heldLiveEntryAttempts[in] = release
	heldLiveEntryAttemptsMu.Unlock()
}

func releaseLiveEntryAttempt(in *entryIntent) {
	if in == nil {
		return
	}
	heldLiveEntryAttemptsMu.Lock()
	release := heldLiveEntryAttempts[in]
	delete(heldLiveEntryAttempts, in)
	heldLiveEntryAttemptsMu.Unlock()
	if release != nil {
		release()
	}
}

func releaseHeldLiveEntryAttempts() {
	heldLiveEntryAttemptsMu.Lock()
	releases := make([]func(), 0, len(heldLiveEntryAttempts))
	for in, release := range heldLiveEntryAttempts {
		releases = append(releases, release)
		delete(heldLiveEntryAttempts, in)
	}
	heldLiveEntryAttemptsMu.Unlock()
	for _, release := range releases {
		release()
	}
}

func entryIntentSameEvidence(a, b *entryIntent) bool {
	return a.IntentID == b.IntentID &&
		a.Partition == b.Partition &&
		a.StrategyID == b.StrategyID &&
		a.Status == b.Status &&
		a.Provider == b.Provider &&
		a.AccountKey == b.AccountKey &&
		a.InstrumentType == b.InstrumentType &&
		a.ClientOrderID == b.ClientOrderID &&
		a.InitialStopClientID == b.InitialStopClientID &&
		a.VenueOrderID == b.VenueOrderID &&
		a.RequestJSON == b.RequestJSON &&
		a.EvidenceJSON == b.EvidenceJSON &&
		a.ConfirmedQty == b.ConfirmedQty &&
		a.ConfirmedNotional == b.ConfirmedNotional &&
		a.ConfirmedFee == b.ConfirmedFee &&
		a.AppliedQty == b.AppliedQty &&
		a.AppliedNotional == b.AppliedNotional &&
		a.AppliedFee == b.AppliedFee &&
		a.SettlementSeq == b.SettlementSeq &&
		a.ReplayPayloadJSON == b.ReplayPayloadJSON &&
		a.ReplayDeliveredSeq == b.ReplayDeliveredSeq
}

func entryIntentStaleLocked(app *AppState, s *StrategyState, in, snap *entryIntent) string {
	switch {
	case s == nil || in == nil || snap == nil:
		return "the attempt is no longer available"
	case app != nil && app.Strategies[s.ID] != s:
		return "the strategy state was replaced"
	case s.EntryGuard == nil || s.EntryGuard.Intents[in.IntentID] != in:
		return "the attempt is no longer held"
	case !entryIntentSameEvidence(in, snap):
		return fmt.Sprintf("the attempt changed from %s to %s or its evidence changed", snap.Status, in.Status)
	}
	return ""
}

func entryStoredExecutionProof(in *entryIntent) string {
	switch {
	case in.ConfirmedQty != 0 || in.ConfirmedNotional != 0 || in.ConfirmedFee != 0:
		return fmt.Sprintf("the saved confirmed fill of %.8f", in.ConfirmedQty)
	case in.AppliedQty != 0 || in.AppliedNotional != 0 || in.AppliedFee != 0:
		return fmt.Sprintf("the saved booked fill of %.8f", in.AppliedQty)
	case in.SettlementSeq != 0:
		return "the saved settlement"
	case in.VenueOrderID != "":
		return fmt.Sprintf("the saved venue order %s", in.VenueOrderID)
	}
	if strings.TrimSpace(in.EvidenceJSON) == "" {
		return ""
	}
	var ev entryIntentEvidence
	if err := json.Unmarshal([]byte(in.EvidenceJSON), &ev); err != nil {
		return "the unreadable saved evidence"
	}
	switch {
	case ev.FilledQty != 0:
		return fmt.Sprintf("the saved evidence of a %.8f fill", ev.FilledQty)
	case ev.VenueOrderID != "":
		return fmt.Sprintf("the saved evidence of venue order %s", ev.VenueOrderID)
	case ev.StopOrderID != "" || ev.StopStatus == "filled" || ev.StopStatus == "open" || ev.StopStatus == "triggered":
		return "the saved evidence of an initial stop order"
	}
	return ""
}

type entryAbsenceAudit struct {
	Account             string             `json:"account"`
	ClientOrderID       string             `json:"client_order_id"`
	InitialStopClientID string             `json:"initial_stop_client_id,omitempty"`
	EntryDeadlineMs     int64              `json:"entry_deadline_ms"`
	StopDeadlineMs      int64              `json:"stop_deadline_ms,omitempty"`
	ReleasableAfterMs   int64              `json:"releasable_after_ms"`
	WindowStartMs       int64              `json:"window_start_ms"`
	OrderCoverage       entryFillCoverage  `json:"order_coverage"`
	StopCoverage        *entryFillCoverage `json:"stop_coverage,omitempty"`
	CheckedAtMs         int64              `json:"checked_at_ms"`
}

type entryOperatorAudit struct {
	OperatorID      string          `json:"operator_id"`
	Command         string          `json:"command"`
	Provider        string          `json:"provider"`
	Symbol          string          `json:"symbol"`
	InstrumentType  string          `json:"instrument_type"`
	PriorStatus     string          `json:"prior_status"`
	PriorHoldReason string          `json:"prior_hold_reason,omitempty"`
	PriorEvidence   json.RawMessage `json:"prior_evidence,omitempty"`
	ReleasedAtMs    int64           `json:"released_at_ms"`
}

func entryReleasableAfterMs(in *entryIntent, req entryIntentRequest) (int64, string) {
	if req.ExpiresAfterMs <= 0 {
		return 0, "the saved request has no venue deadline"
	}
	if req.SubmittedAtMs <= 0 || req.SubmittedAtMs > req.ExpiresAfterMs {
		return 0, "the saved submission time is missing or after the venue deadline"
	}
	deadline := req.ExpiresAfterMs
	if in.InitialStopClientID != "" {
		if req.StopExpiresAfterMs <= 0 {
			return 0, "the saved request carries an initial-stop client id but no initial-stop deadline"
		}
		if req.StopExpiresAfterMs > deadline {
			deadline = req.StopExpiresAfterMs
		}
	}
	return deadline + entryRecoveryGrace.Milliseconds(), ""
}

func entryOrderAbsent(o *entryRecoveryOrder, label string, windowStart, releasable int64) string {
	switch {
	case o == nil:
		return fmt.Sprintf("the %s lookup returned no evidence", label)
	case o.Found:
		return fmt.Sprintf("the %s exists at the venue (order %s, %s)", label, o.OrderID, o.Status)
	case o.FillQty != 0 || o.FillCount != 0 || o.ExecutedSz != 0:
		return fmt.Sprintf("the venue reports %d fill(s) for the %s client id", o.FillCount, label)
	case o.Coverage == nil:
		return fmt.Sprintf("the %s lookup returned no fill-history coverage", label)
	case !o.Coverage.Complete:
		why := o.Coverage.Reason
		if why == "" {
			why = "no reason given"
		}
		return fmt.Sprintf("the fill history for the %s is incomplete: %s", label, why)
	case o.Coverage.FromMs > windowStart:
		return fmt.Sprintf("the fill history for the %s starts at %s, after the attempt window start %s", label, formatClosedBarMs(o.Coverage.FromMs), formatClosedBarMs(windowStart))
	case o.Coverage.ThroughMs < releasable:
		return fmt.Sprintf("the fill history for the %s ends at %s, before the release deadline %s", label, formatClosedBarMs(o.Coverage.ThroughMs), formatClosedBarMs(releasable))
	case o.Coverage.MatchedCount != 0 || o.Coverage.MatchedQty != 0:
		return fmt.Sprintf("the fill history holds %d fill(s) for the %s client id", o.Coverage.MatchedCount, label)
	}
	return ""
}

func proveEntryAbsence(in *entryIntent, req entryIntentRequest, res *entryRecoveryResult, now time.Time) (*entryAbsenceAudit, string) {
	if !entryAccountBound(in) {
		return nil, "the attempt has no saved venue account binding, so a query cannot be tied to the account that sent it"
	}
	if cur := currentEntryAccountKey(in); cur != in.AccountKey {
		return nil, "the configured venue account differs from the account that sent the attempt"
	}
	if res == nil {
		return nil, "the venue query returned no evidence"
	}
	if res.Error != "" {
		return nil, res.Error
	}
	if res.Account != in.AccountKey {
		return nil, fmt.Sprintf("the venue query ran against account %q, not the attempt's account %q", res.Account, in.AccountKey)
	}
	releasable, why := entryReleasableAfterMs(in, req)
	if why != "" {
		return nil, why
	}
	if now.UnixMilli() <= releasable {
		return nil, fmt.Sprintf("the venue deadline plus grace has not passed (release after %s)", formatClosedBarMs(releasable))
	}
	windowStart := min(entryRecoveryWindowStart(in), req.SubmittedAtMs)
	if why := entryOrderAbsent(res.Order, "entry order", windowStart, releasable); why != "" {
		return nil, why
	}
	audit := &entryAbsenceAudit{
		Account:           in.AccountKey,
		ClientOrderID:     in.ClientOrderID,
		EntryDeadlineMs:   req.ExpiresAfterMs,
		ReleasableAfterMs: releasable,
		WindowStartMs:     windowStart,
		OrderCoverage:     *res.Order.Coverage,
		CheckedAtMs:       res.CheckedAtMs,
	}
	if in.InitialStopClientID != "" {
		if why := entryOrderAbsent(res.Stop, "initial stop", windowStart, releasable); why != "" {
			return nil, why
		}
		stopCoverage := *res.Stop.Coverage
		audit.InitialStopClientID = in.InitialStopClientID
		audit.StopDeadlineMs = req.StopExpiresAfterMs
		audit.StopCoverage = &stopCoverage
	}
	return audit, ""
}

func entryOperatorRowMismatch(mem, disk *entryIntent) string {
	switch {
	case disk.Status != mem.Status:
		return fmt.Sprintf("status %s in memory, %s in the state file", mem.Status, disk.Status)
	case disk.Partition != mem.Partition:
		return "the owning partition differs"
	case disk.Provider != mem.Provider || disk.AccountKey != mem.AccountKey || disk.InstrumentType != mem.InstrumentType || disk.Symbol != mem.Symbol:
		return "the venue identity differs"
	case disk.ClientOrderID != mem.ClientOrderID || disk.InitialStopClientID != mem.InitialStopClientID:
		return "the client order ids differ"
	case disk.RequestJSON != mem.RequestJSON:
		return "the saved request differs"
	case disk.EvidenceJSON != mem.EvidenceJSON || disk.VenueOrderID != mem.VenueOrderID:
		return "the saved evidence differs"
	case disk.ConfirmedQty != mem.ConfirmedQty || disk.AppliedQty != mem.AppliedQty || disk.SettlementSeq != mem.SettlementSeq:
		return "the saved fill quantities differ"
	}
	return ""
}

type entryOperatorDeps struct {
	state    *AppState
	mu       *sync.RWMutex
	store    *StateStore
	strategy func(id string) (StrategyConfig, bool)
	logMgr   *LogManager
}

type entryOperatorOutcome struct {
	Released bool
	Message  string
}

func entryOperatorProvider(sc StrategyConfig) string {
	switch {
	case sc.Platform == "hyperliquid" && sc.Type == "perps":
		return entryProviderHyperliquid
	case sc.Platform == "okx" && (sc.Type == "spot" || sc.Type == "perps"):
		return entryProviderOKX
	}
	return ""
}

func resolveEntryAttemptByOperator(deps entryOperatorDeps, operatorID, strategyID, intentID string) entryOperatorOutcome {
	refuse := func(format string, a ...any) entryOperatorOutcome {
		return entryOperatorOutcome{Message: "Refused: " + fmt.Sprintf(format, a...) + " The attempt, the book and the candle boundaries are unchanged."}
	}
	strategyID = strings.TrimSpace(strategyID)
	intentID = strings.TrimSpace(intentID)
	if strategyID == "" || intentID == "" {
		return entryOperatorOutcome{Message: fmt.Sprintf("usage: /%s%s <strategy> <attempt>", commandPrefix, entryOperatorCommandName)}
	}
	if !entryIntentIDPattern.MatchString(intentID) {
		return refuse("%q is not a live entry attempt id (ei- followed by 24 hex digits).", intentID)
	}
	if deps.state == nil || deps.mu == nil || deps.store == nil || deps.strategy == nil {
		return refuse("the state store is unavailable.")
	}
	sc, ok := deps.strategy(strategyID)
	if !ok {
		return refuse("strategy %s is not configured in this instance.", strategyID)
	}
	if isBasketMember(sc) {
		return refuse("strategy %s is a member of basket %s; the basket coordinator reconciles its entries by client order id.", sc.ID, sc.BasketID)
	}
	part := partitionFor(sc)
	if part.Scope != ScopeLive || !isLiveArgs(sc.Args) {
		return refuse("strategy %s is not live; paper strategies send no live entries.", sc.ID)
	}
	provider := entryOperatorProvider(sc)
	if provider == "" {
		return refuse("strategy %s runs on %s %s, which has no live entry attempts.", sc.ID, sc.Platform, sc.Type)
	}
	if deps.store.readOnly() {
		return refuse("the state files are open read-only.")
	}
	if _, err := deps.store.dbForStrategy(sc.ID); err != nil {
		return refuse("the owning state file is unavailable: %v.", err)
	}
	release, ok := tryLockEntryAttempt(sc.ID)
	if !ok {
		return refuse("a live entry or a recovery check for strategy %s is in progress; retry after the next cycle.", sc.ID)
	}
	defer release()

	deps.mu.RLock()
	s := deps.state.Strategies[sc.ID]
	var in *entryIntent
	var snap entryIntent
	guardErr := ""
	switch {
	case s == nil:
		guardErr = fmt.Sprintf("strategy %s has no loaded state.", sc.ID)
	case s.EntryGuard == nil:
		guardErr = fmt.Sprintf("strategy %s has no durable entry record.", sc.ID)
	case s.EntryGuard.Partition != part:
		guardErr = fmt.Sprintf("the durable entry record of %s belongs to the %s partition, not %s.", sc.ID, partitionLabel(s.EntryGuard.Partition), partitionLabel(part))
	default:
		if in = s.EntryGuard.Intents[intentID]; in != nil {
			snap = *in
		}
	}
	deps.mu.RUnlock()
	if guardErr != "" {
		return refuse("%s", guardErr)
	}
	disk, rerr := deps.store.ReadEntryIntent(sc.ID, part, intentID)
	if in == nil {
		switch {
		case errors.Is(rerr, sql.ErrNoRows):
			return refuse("attempt %s does not exist for strategy %s.", intentID, sc.ID)
		case rerr != nil:
			return refuse("attempt %s cannot be read for strategy %s: %v.", intentID, sc.ID, rerr)
		case disk.Status == entryIntentNoFill || disk.Status == entryIntentCommitted:
			return entryOperatorOutcome{Message: fmt.Sprintf("Attempt %s of %s is already resolved (%s); no change was made.", intentID, sc.ID, disk.Status)}
		}
		return refuse("attempt %s is %s in the state file but is not held in memory; a restart reloads it.", intentID, disk.Status)
	}
	if rerr != nil {
		return refuse("attempt %s cannot be read from the owning state file: %v.", intentID, rerr)
	}
	if why := entryOperatorRowMismatch(&snap, disk); why != "" {
		return refuse("the held attempt and the state file disagree (%s).", why)
	}
	if snap.StrategyID != sc.ID || snap.Partition != part || snap.Provider != provider {
		return refuse("attempt %s does not belong to live %s strategy %s.", intentID, provider, sc.ID)
	}
	switch snap.Status {
	case entryIntentSubmitting:
	case entryIntentReserved:
		return refuse("attempt %s was reserved but never submitted; automatic recovery releases it on the next cycle.", intentID)
	case entryIntentSettling:
		return refuse("attempt %s is settling because a fill was proven; this command never relabels a fill as no fill and never repairs its booking.", intentID)
	default:
		return refuse("attempt %s is %s.", intentID, snap.Status)
	}
	if proof := entryStoredExecutionProof(&snap); proof != "" {
		return refuse("%s proves execution; this command never overrides saved execution proof.", proof)
	}
	req, err := parseEntryIntentRequest(&snap)
	if err != nil {
		return refuse("the saved request is unusable: %v.", err)
	}
	releasable, why := entryReleasableAfterMs(&snap, req)
	if why != "" {
		return refuse("%s.", why)
	}
	if time.Now().UnixMilli() <= releasable {
		return refuse("the venue deadline plus grace has not passed; retry after %s.", formatClosedBarMs(releasable))
	}
	if !entryAccountBound(&snap) {
		return refuse("the attempt has no saved venue account binding (it was saved before the binding existed), so no query can prove absence for its account.")
	}
	if cur := currentEntryAccountKey(&snap); cur != snap.AccountKey {
		return refuse("the configured venue account differs from the account that sent the attempt.")
	}
	script := req.Script
	if strings.TrimSpace(script) == "" {
		script = sc.Script
	}
	res, qerr := queryEntryRecovery(&snap, req, script)
	if qerr != nil {
		return refuse("the venue query failed: %v.", qerr)
	}
	checked := time.Now()
	audit, why := proveEntryAbsence(&snap, req, res, checked)
	if why != "" {
		return refuse("%s.", why)
	}
	var prior json.RawMessage
	if strings.TrimSpace(snap.EvidenceJSON) != "" {
		prior = json.RawMessage(snap.EvidenceJSON)
	}
	ev := entryIntentEvidence{
		Outcome:      "operator_verified_absent",
		Detail:       fmt.Sprintf("operator %s requested a fresh check: no order and no fill for the client ids in complete fill history from %s through %s", operatorID, formatClosedBarMs(audit.WindowStartMs), formatClosedBarMs(audit.OrderCoverage.ThroughMs)),
		ObservedAtMs: checked.UnixMilli(),
		Absence:      audit,
		Operator: &entryOperatorAudit{
			OperatorID:      operatorID,
			Command:         entryOperatorCommandName,
			Provider:        snap.Provider,
			Symbol:          snap.Symbol,
			InstrumentType:  snap.InstrumentType,
			PriorStatus:     snap.Status,
			PriorHoldReason: snap.HoldReason,
			PriorEvidence:   prior,
			ReleasedAtMs:    checked.UnixMilli(),
		},
	}
	evJSON := marshalEntryJSON(ev)
	if evJSON == "" {
		return refuse("the release evidence could not be encoded.")
	}
	logger := entryRecoveryLogger(deps.logMgr, sc.ID)
	defer logger.Close()
	deps.mu.Lock()
	if why := entryIntentStaleLocked(deps.state, s, in, &snap); why != "" {
		deps.mu.Unlock()
		return refuse("the attempt changed during the venue query (%s).", why)
	}
	if err := deps.store.ReleaseEntryIntentByOperator(sc.ID, &snap, evJSON, checked.UnixMilli()); err != nil {
		deps.mu.Unlock()
		logger.Error("Operator release of live entry %s failed to save: %v — the attempt stays held (#1771)", intentID, err)
		return refuse("saving the release failed: %v. The hold stays in place and a retry is safe.", err)
	}
	in.Status = entryIntentNoFill
	in.durableStatus = entryIntentNoFill
	in.EvidenceJSON = evJSON
	in.HoldReason = ""
	in.UpdatedAtMs = checked.UnixMilli()
	delete(s.EntryGuard.Intents, in.IntentID)
	deps.mu.Unlock()
	entryRecoveryAlerted.Delete(intentID)
	entryRecoveryLastQuery.Delete(intentID)
	logger.Warn("Live entry %s (%s %s, client order %s) released by operator %s: complete venue evidence shows no order and no fill (#1771)", intentID, snap.Symbol, snap.TargetSide, snap.ClientOrderID, operatorID)
	stopNote := ""
	if audit.StopCoverage != nil {
		stopNote = " and its initial-stop client id"
	}
	return entryOperatorOutcome{Released: true, Message: fmt.Sprintf(
		"Released attempt %s of %s (%s %s, candle %s) as no_fill. The venue (%s) has no order and no fill for client order %s%s in complete fill history from %s through %s (%d page(s), %d row(s)). The audit is saved with the attempt. The next valid closed-bar entry still passes every entry gate and candle boundary.",
		intentID, sc.ID, snap.Symbol, snap.TargetSide, formatClosedBarMs(snap.BarOpenMs), audit.Account, snap.ClientOrderID, stopNote,
		formatClosedBarMs(audit.WindowStartMs), formatClosedBarMs(audit.OrderCoverage.ThroughMs), audit.OrderCoverage.Pages, audit.OrderCoverage.Rows)}
}

var errEntryOperatorReleaseStale = errors.New("the attempt row no longer matches the checked attempt")
