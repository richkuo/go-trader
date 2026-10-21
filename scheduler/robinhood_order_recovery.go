package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const robinhoodSubmitWindowMargin = 15 * time.Second

const robinhoodSettleWindow = 10 * time.Minute

const robinhoodAttestReleaseChoice = "attest-no-order"

type robinhoodResolveVerdict struct {
	Unresolved bool
	Reason     string
}

func robinhoodAttemptByID(state *AppState, strategyID, attemptID string) (*StrategyState, *robinhoodOrderAttempt) {
	if state == nil {
		return nil, nil
	}
	s := state.Strategies[strategyID]
	if s == nil || s.RobinhoodOrders == nil {
		return s, nil
	}
	return s, s.RobinhoodOrders.Attempts[attemptID]
}

func robinhoodResolveRequestFor(a *robinhoodOrderAttempt, orderID string, cancelOpen bool) RobinhoodResolveRequest {
	budget := robinhoodOrderBudgetMs()
	if req, err := parseRobinhoodOrderRequest(a); err == nil && req.BudgetMs > 0 {
		budget = req.BudgetMs
	}
	if orderID == "" {
		orderID = a.VenueOrderID
	}
	return RobinhoodResolveRequest{
		Symbol:            a.Symbol,
		Side:              a.Side,
		RefID:             a.RefID,
		OrderID:           orderID,
		RequestedQuantity: robinhoodRequestedQty(a, nil),
		SubmittedAfterMs:  a.CreatedAtMs,
		SubmittedBeforeMs: a.CreatedAtMs + budget + robinhoodSubmitWindowMargin.Milliseconds(),
		ExpectedAccountID: a.VenueAccountID,
		CancelOpen:        cancelOpen,
	}
}

func resolveOneRobinhoodAttempt(d robinhoodEmergencyDeps, strategyID, attemptID, orderID string, cancelOpen bool, now time.Time) robinhoodResolveVerdict {
	d.mu.RLock()
	s, live := robinhoodAttemptByID(d.state, strategyID, attemptID)
	var snap robinhoodOrderAttempt
	if live != nil {
		snap = *live
	}
	d.mu.RUnlock()
	if s == nil || live == nil {
		return robinhoodResolveVerdict{}
	}
	if !snap.unresolved() {
		return robinhoodResolveVerdict{}
	}
	if snap.Status == robinhoodAttemptSettling {
		return robinhoodResolveVerdict{Unresolved: true, Reason: "a confirmed fill waits for settlement"}
	}
	accountKey := robinhoodAccountKey()
	if accountKey == "" || accountKey != snap.AccountKey {
		reason := "the configured Robinhood account differs from the account that sent this order; no venue call is made"
		notifyRobinhoodOrderHold(d.notifier, strategyID, &snap, reason, now)
		return robinhoodResolveVerdict{Unresolved: true, Reason: reason}
	}
	release, ok := tryLockRobinhoodCoin(snap.AccountKey, snap.Symbol)
	if !ok {
		return robinhoodResolveVerdict{Unresolved: true, Reason: "another resolution of this coin is in progress"}
	}
	defer release()
	req := robinhoodResolveRequestFor(&snap, orderID, cancelOpen)
	req.VerifyFields = orderID != "" && orderID != snap.VenueOrderID
	res, stderr, err := robinhoodResolveFn(robinhoodResolveScript, req)
	if stderr != "" {
		fmt.Printf("[rh-resolve] %s %s stderr: %s\n", snap.AttemptID, snap.Symbol, stderr)
	}
	class, detail := classifyRobinhoodOrderOutcome(res, err, &snap, req.RequestedQuantity)
	d.mu.Lock()
	_, current := robinhoodAttemptByID(d.state, strategyID, attemptID)
	if current == nil || current.Status != snap.Status || current.UpdatedAtMs != snap.UpdatedAtMs {
		d.mu.Unlock()
		return robinhoodResolveVerdict{Unresolved: current.unresolved(), Reason: "the attempt changed during resolution"}
	}
	recordErr := recordRobinhoodOrderOutcomeLocked(d.store, s, current, res, class, detail, time.Now())
	var after robinhoodOrderAttempt
	after = *current
	d.mu.Unlock()
	if recordErr != nil {
		reason := fmt.Sprintf("%s; saving the resolution failed: %v", detail, recordErr)
		notifyRobinhoodOrderHold(d.notifier, strategyID, &after, reason, now)
		return robinhoodResolveVerdict{Unresolved: true, Reason: reason}
	}
	switch class {
	case robinhoodOrderConfirmed:
		fmt.Printf("[CRITICAL] rh-resolve: attempt %s %s %s filled %.10g at $%.6f (order %s); settling\n", snap.AttemptID, snap.Side, snap.Symbol, after.ConfirmedQty, after.ConfirmedAvgPx, after.VenueOrderID)
		return robinhoodResolveVerdict{Unresolved: true, Reason: "a confirmed fill waits for settlement"}
	case robinhoodOrderNoFill:
		fmt.Printf("[INFO] rh-resolve: attempt %s %s %s released with no fill: %s\n", snap.AttemptID, snap.Side, snap.Symbol, detail)
		return robinhoodResolveVerdict{}
	}
	notifyRobinhoodOrderHold(d.notifier, strategyID, &after, detail, now)
	return robinhoodResolveVerdict{Unresolved: true, Reason: detail}
}

type robinhoodRecoveredTrades struct {
	sc     StrategyConfig
	state  *StrategyState
	trades int
}

func settleRobinhoodAttempts(d robinhoodEmergencyDeps, cfg *Config, logMgr *LogManager) []robinhoodRecoveredTrades {
	if d.store == nil || d.state == nil {
		return nil
	}
	checks := robinhoodDuplicateOrderIDs(d.store, d.state, d.mu, d.cfgs)
	byID := make(map[string]StrategyConfig, len(d.cfgs))
	for _, sc := range d.cfgs {
		byID[sc.ID] = sc
	}
	type holdAlert struct {
		strategyID string
		attempt    robinhoodOrderAttempt
		reason     string
	}
	var holds []holdAlert
	var out []robinhoodRecoveredTrades
	d.mu.Lock()
	for _, a := range robinhoodUnresolvedAttemptsLocked(d.state) {
		if a.Origin != robinhoodOriginSignal || a.Status != robinhoodAttemptSettling || cfg == nil {
			continue
		}
		sc, ok := byID[a.StrategyID]
		s := d.state.Strategies[a.StrategyID]
		if !ok || s == nil {
			continue
		}
		db, err := d.store.dbForStrategy(sc.ID)
		if err != nil {
			fmt.Printf("[CRITICAL] rh-settle: attempt %s: %v\n", a.AttemptID, err)
			continue
		}
		if err := checks.Unreadable[a.VenueOrderID]; err != nil {
			continue
		}
		logger := entryRecoveryLogger(logMgr, sc.ID)
		res := settleRobinhoodSignalAttemptLocked(d.store, sc, s, db, a, checks.Booked[a.VenueOrderID], cfg, logger)
		logger.Close()
		if res.Hold != "" {
			holds = append(holds, holdAlert{strategyID: sc.ID, attempt: *a, reason: res.Hold})
		}
		if res.Settled && res.Trades > 0 {
			out = append(out, robinhoodRecoveredTrades{sc: sc, state: s, trades: res.Trades})
		}
	}
	if settled := settleRobinhoodEmergencyAttemptsLocked(d.state, d.cfgs, checks, nil); len(settled) > 0 {
		if err := d.store.SavePartition(d.state, livePartition); err != nil {
			fmt.Printf("[CRITICAL] rh-settle: emergency settlement of %v booked in memory but the live save failed: %v — it retries with the next full save\n", settled, err)
		}
	}
	for _, a := range robinhoodUnresolvedAttemptsLocked(d.state) {
		if a.Status == robinhoodAttemptSettling && a.HoldReason != "" && a.Origin != robinhoodOriginSignal {
			holds = append(holds, holdAlert{strategyID: a.StrategyID, attempt: *a, reason: a.HoldReason})
		}
	}
	d.mu.Unlock()
	for _, h := range holds {
		a := h.attempt
		notifyRobinhoodOrderHold(d.notifier, h.strategyID, &a, h.reason, time.Now())
	}
	return out
}

func resolveRobinhoodUnresolved(d robinhoodEmergencyDeps, cancelOpen func(a *robinhoodOrderAttempt, now time.Time) bool, now time.Time) map[string]error {
	type job struct {
		strategyID string
		attemptID  string
		cancelOpen bool
	}
	var jobs []job
	d.mu.RLock()
	for _, a := range robinhoodUnresolvedAttemptsLocked(d.state) {
		if a.Status == robinhoodAttemptSettling {
			continue
		}
		jobs = append(jobs, job{strategyID: a.StrategyID, attemptID: a.AttemptID, cancelOpen: cancelOpen(a, now)})
	}
	d.mu.RUnlock()
	for _, j := range jobs {
		resolveOneRobinhoodAttempt(d, j.strategyID, j.attemptID, "", j.cancelOpen, now)
	}
	out := map[string]error{}
	d.mu.RLock()
	for _, a := range robinhoodUnresolvedAttemptsLocked(d.state) {
		reason := a.HoldReason
		if reason == "" {
			reason = "status " + a.Status
		}
		out[a.Symbol] = fmt.Errorf("attempt %s (%s %s): %s", a.AttemptID, a.Origin, a.Side, reason)
	}
	d.mu.RUnlock()
	return out
}

func robinhoodCancelOpenAfterBudget(a *robinhoodOrderAttempt, now time.Time) bool {
	budget := robinhoodOrderBudgetMs()
	if req, err := parseRobinhoodOrderRequest(a); err == nil && req.BudgetMs > 0 {
		budget = req.BudgetMs
	}
	return now.UnixMilli() > a.CreatedAtMs+budget+robinhoodSubmitWindowMargin.Milliseconds()
}

func robinhoodCancelOpenAlways(*robinhoodOrderAttempt, time.Time) bool { return true }

func robinhoodEmergencyResolver(d robinhoodEmergencyDeps, cfg *Config, logMgr *LogManager) func() map[string]error {
	return func() map[string]error {
		now := time.Now()
		resolveRobinhoodUnresolved(d, robinhoodCancelOpenAlways, now)
		recovered := settleRobinhoodAttempts(d, cfg, logMgr)
		sendRobinhoodRecoveredTradeAlerts(d, cfg, recovered)
		out := map[string]error{}
		d.mu.RLock()
		for _, a := range robinhoodUnresolvedAttemptsLocked(d.state) {
			reason := a.HoldReason
			if reason == "" {
				reason = "status " + a.Status
			}
			out[a.Symbol] = fmt.Errorf("attempt %s (%s %s): %s", a.AttemptID, a.Origin, a.Side, reason)
		}
		d.mu.RUnlock()
		return out
	}
}

func sendRobinhoodRecoveredTradeAlerts(d robinhoodEmergencyDeps, cfg *Config, recovered []robinhoodRecoveredTrades) {
	if d.notifier == nil || cfg == nil {
		return
	}
	for _, r := range recovered {
		sendTradeAlerts(r.sc, r.state, r.trades, d.mu, d.notifier, cfg.Regime)
	}
}

func recoverRobinhoodOrders(cfg *Config, state *AppState, store *StateStore, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager, now time.Time) {
	if cfg == nil || state == nil || store == nil {
		return
	}
	d := robinhoodEmergencyDeps{store: store, state: state, mu: mu, cfgs: cfg.Strategies, notifier: notifier}
	mu.RLock()
	pending := len(robinhoodUnresolvedAttemptsLocked(state))
	mu.RUnlock()
	if pending == 0 {
		return
	}
	resolveRobinhoodUnresolved(d, robinhoodCancelOpenAfterBudget, now)
	recovered := settleRobinhoodAttempts(d, cfg, logMgr)
	sendRobinhoodRecoveredTradeAlerts(d, cfg, recovered)
}

func robinhoodOrderStartupLines(state *AppState) []string {
	if state == nil {
		return nil
	}
	var lines []string
	for _, a := range robinhoodUnresolvedAttemptsLocked(state) {
		lines = append(lines, fmt.Sprintf("[CRITICAL] unresolved Robinhood order attempt held: %s: %s", a.StrategyID, robinhoodAttemptSummary(a)))
	}
	sort.Strings(lines)
	return lines
}

const robinhoodOperatorCommandName = "resolve-robinhood-order"

var robinhoodAttemptIDPattern = regexp.MustCompile(`^rh-[0-9a-f]{24}$`)

var robinhoodVenueOrderIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func resolveRobinhoodAttemptByOperator(d robinhoodEmergencyDeps, lookup func(id string) (StrategyConfig, bool), strategyID, attemptID, orderID string, now time.Time) string {
	if lookup == nil || d.store == nil || d.state == nil || d.mu == nil {
		return "status server not ready"
	}
	if !robinhoodAttemptIDPattern.MatchString(attemptID) {
		return fmt.Sprintf("refused: %q is not a Robinhood attempt id (rh- followed by 24 hex characters)", attemptID)
	}
	if orderID != "" && !robinhoodVenueOrderIDPattern.MatchString(orderID) {
		return fmt.Sprintf("refused: %q is not a Robinhood order id", orderID)
	}
	snap, refusal := robinhoodOperatorHeldAttempt(d, lookup, strategyID, attemptID)
	if refusal != "" {
		return refusal
	}
	if orderID != "" && snap.VenueOrderID != "" && orderID != snap.VenueOrderID {
		return fmt.Sprintf("refused: attempt %s is bound to venue order %s, not %s", attemptID, snap.VenueOrderID, orderID)
	}
	if release, ok := tryLockRobinhoodCoin(snap.AccountKey, snap.Symbol); !ok {
		return fmt.Sprintf("refused: another resolution of %s is in progress; try again shortly", snap.Symbol)
	} else {
		release()
	}
	verdict := resolveOneRobinhoodAttempt(d, strategyID, attemptID, orderID, false, now)
	d.mu.RLock()
	_, after := robinhoodAttemptByID(d.state, strategyID, attemptID)
	var final robinhoodOrderAttempt
	if after != nil {
		final = *after
	}
	d.mu.RUnlock()
	switch {
	case after == nil:
		return fmt.Sprintf("attempt %s released: the venue shows no fill (%s). New Robinhood orders for %s may be sent again.", attemptID, snap.Symbol, snap.Symbol)
	case final.Status == robinhoodAttemptSettling:
		return fmt.Sprintf("attempt %s confirmed: %.10g %s filled at $%.6f (order %s). It settles to the book with the next cycle.", attemptID, final.ConfirmedQty, final.Symbol, final.ConfirmedAvgPx, final.VenueOrderID)
	}
	return fmt.Sprintf("attempt %s still held (%s): %s", attemptID, final.Status, verdict.Reason)
}

func robinhoodOperatorHeldAttempt(d robinhoodEmergencyDeps, lookup func(id string) (StrategyConfig, bool), strategyID, attemptID string) (robinhoodOrderAttempt, string) {
	var snap robinhoodOrderAttempt
	sc, ok := lookup(strategyID)
	if !ok {
		return snap, fmt.Sprintf("refused: strategy %q is not configured", strategyID)
	}
	if !isRobinhoodLiveSpot(sc) {
		return snap, fmt.Sprintf("refused: strategy %q is not a live Robinhood spot strategy", strategyID)
	}
	d.mu.RLock()
	_, a := robinhoodAttemptByID(d.state, strategyID, attemptID)
	if a != nil {
		snap = *a
	}
	d.mu.RUnlock()
	if a == nil {
		return snap, fmt.Sprintf("refused: strategy %s holds no unresolved Robinhood attempt %s", strategyID, attemptID)
	}
	if !snap.unresolved() {
		return snap, fmt.Sprintf("refused: attempt %s is already %s", attemptID, snap.Status)
	}
	if snap.Status == robinhoodAttemptSettling {
		return snap, fmt.Sprintf("attempt %s already holds a confirmed fill of %.10g %s at $%.6f; it settles with the next cycle", attemptID, snap.ConfirmedQty, snap.Symbol, snap.ConfirmedAvgPx)
	}
	if accountKey := robinhoodAccountKey(); accountKey == "" || accountKey != snap.AccountKey {
		return snap, fmt.Sprintf("refused: attempt %s was sent from another Robinhood account; no venue call was made", attemptID)
	}
	return snap, ""
}

func robinhoodAttestableStatus(a *robinhoodOrderAttempt) bool {
	return a != nil && a.VenueOrderID == "" && (a.Status == robinhoodAttemptSubmitting || a.Status == robinhoodAttemptUnknown)
}

func robinhoodAttestationEarliest(a *robinhoodOrderAttempt) time.Time {
	req := robinhoodResolveRequestFor(a, "", false)
	return time.UnixMilli(req.SubmittedBeforeMs).Add(robinhoodSettleWindow)
}

func prepareRobinhoodAttestedRelease(d robinhoodEmergencyDeps, lookup func(id string) (StrategyConfig, bool), strategyID, attemptID string, now time.Time) (*robinhoodOrderAttempt, string) {
	if lookup == nil || d.store == nil || d.state == nil || d.mu == nil {
		return nil, "status server not ready"
	}
	if !robinhoodAttemptIDPattern.MatchString(attemptID) {
		return nil, fmt.Sprintf("refused: %q is not a Robinhood attempt id (rh- followed by 24 hex characters)", attemptID)
	}
	snap, refusal := robinhoodOperatorHeldAttempt(d, lookup, strategyID, attemptID)
	if refusal != "" {
		return nil, refusal
	}
	if !robinhoodAttestableStatus(&snap) {
		return nil, fmt.Sprintf("refused: attempt %s is %s and bound to venue order %q; an attested release applies only to an attempt with no venue order id. Run the command with the order id instead.", attemptID, snap.Status, snap.VenueOrderID)
	}
	if earliest := robinhoodAttestationEarliest(&snap); now.Before(earliest) {
		return nil, fmt.Sprintf("refused: the settle window of attempt %s has not passed; an attested release is possible after %s", attemptID, earliest.UTC().Format(time.RFC3339))
	}
	if release, ok := tryLockRobinhoodCoin(snap.AccountKey, snap.Symbol); !ok {
		return nil, fmt.Sprintf("refused: another resolution of %s is in progress; try again shortly", snap.Symbol)
	} else {
		release()
	}
	verdict := resolveOneRobinhoodAttempt(d, strategyID, attemptID, "", false, now)
	d.mu.RLock()
	_, after := robinhoodAttemptByID(d.state, strategyID, attemptID)
	var checked robinhoodOrderAttempt
	if after != nil {
		checked = *after
	}
	d.mu.RUnlock()
	switch {
	case after == nil || !checked.unresolved():
		return nil, fmt.Sprintf("attempt %s released on venue evidence: the venue shows no fill (%s). No attestation is needed.", attemptID, snap.Symbol)
	case checked.Status == robinhoodAttemptSettling:
		return nil, fmt.Sprintf("attempt %s confirmed on venue evidence: %.10g %s filled at $%.6f (order %s). It settles to the book with the next cycle; no attestation is needed.", attemptID, checked.ConfirmedQty, checked.Symbol, checked.ConfirmedAvgPx, checked.VenueOrderID)
	case !robinhoodAttestableStatus(&checked):
		return nil, fmt.Sprintf("refused: the venue check bound attempt %s to order %q (%s): %s. Run the command with that order id instead.", attemptID, checked.VenueOrderID, checked.Status, verdict.Reason)
	}
	req := robinhoodResolveRequestFor(&checked, "", false)
	size := "an unknown quantity"
	if q := robinhoodRequestedQty(&checked, nil); q > 0 {
		size = fmt.Sprintf("%.10g %s", q, checked.Symbol)
	} else if fr, err := parseRobinhoodOrderRequest(&checked); err == nil && fr.AmountUSD > 0 {
		size = fmt.Sprintf("$%.2f of %s", fr.AmountUSD, checked.Symbol)
	}
	prompt := fmt.Sprintf("Release Robinhood attempt %s of %s as NO FILL on your word?\n"+
		"Order: %s %s (%s origin), ref_id %s, no venue order id.\n"+
		"Submit window: %s to %s.\n"+
		"Venue check now: %s.\n"+
		"Confirm only after the Robinhood order history shows no %s %s order in that window and the %s holdings show no change from it. "+
		"The program cannot prove this itself. If an order for this attempt exists, the book will not include it, and a new %s order may be sent.",
		attemptID, strategyID, checked.Side, size, checked.Origin, checked.RefID,
		time.UnixMilli(req.SubmittedAfterMs).UTC().Format(time.RFC3339), time.UnixMilli(req.SubmittedBeforeMs).UTC().Format(time.RFC3339),
		verdict.Reason, checked.Side, checked.Symbol, checked.Symbol, checked.Symbol)
	return &checked, prompt
}

func releaseRobinhoodAttemptByAttestation(d robinhoodEmergencyDeps, operatorID, strategyID, attemptID string, checked robinhoodOrderAttempt, now time.Time) string {
	if d.store == nil || d.state == nil || d.mu == nil {
		return "status server not ready"
	}
	if strings.TrimSpace(operatorID) == "" {
		return "refused: the operator is unknown; the attempt stays held"
	}
	if accountKey := robinhoodAccountKey(); accountKey == "" || accountKey != checked.AccountKey {
		return fmt.Sprintf("refused: attempt %s was sent from another Robinhood account; the attempt stays held", attemptID)
	}
	release, ok := tryLockRobinhoodCoin(checked.AccountKey, checked.Symbol)
	if !ok {
		return fmt.Sprintf("refused: another resolution of %s is in progress; the attempt stays held. Try again shortly.", checked.Symbol)
	}
	defer release()
	var prior json.RawMessage
	if strings.TrimSpace(checked.EvidenceJSON) != "" && json.Valid([]byte(checked.EvidenceJSON)) {
		prior = json.RawMessage(checked.EvidenceJSON)
	}
	req := robinhoodResolveRequestFor(&checked, "", false)
	d.mu.Lock()
	s, current := robinhoodAttemptByID(d.state, strategyID, attemptID)
	if s == nil || current == nil || !robinhoodAttestableStatus(current) || current.RefID != checked.RefID ||
		current.AccountKey != checked.AccountKey || current.Symbol != checked.Symbol || current.Side != checked.Side {
		d.mu.Unlock()
		return fmt.Sprintf("refused: attempt %s changed while you were confirming; nothing was released. Run the command again.", attemptID)
	}
	ev := robinhoodOrderEvidenceRecord{
		Outcome:           robinhoodOutcomeOperatorAttested,
		Detail:            fmt.Sprintf("operator %s attested that the Robinhood order history shows no %s %s order for ref_id %s in the submit window %s to %s", operatorID, current.Side, current.Symbol, current.RefID, time.UnixMilli(req.SubmittedAfterMs).UTC().Format(time.RFC3339), time.UnixMilli(req.SubmittedBeforeMs).UTC().Format(time.RFC3339)),
		RequestedQuantity: robinhoodRequestedQty(current, nil),
		ObservedAtMs:      now.UnixMilli(),
		Operator: &robinhoodOperatorAttestation{
			OperatorID:      operatorID,
			Command:         robinhoodOperatorCommandName,
			PriorStatus:     current.Status,
			PriorHoldReason: current.HoldReason,
			PriorEvidence:   prior,
		},
	}
	evJSON := marshalRobinhoodJSON(ev)
	if evJSON == "" {
		d.mu.Unlock()
		return "refused: the release evidence could not be encoded; the attempt stays held"
	}
	db, err := d.store.dbForStrategy(strategyID)
	if err != nil {
		d.mu.Unlock()
		return fmt.Sprintf("refused: the owning state file is unavailable: %v. The attempt stays held.", err)
	}
	next := *current
	next.EvidenceJSON = evJSON
	next.HoldReason = ""
	if err := db.transitionRobinhoodAttempt(&next, []string{robinhoodAttemptSubmitting, robinhoodAttemptUnknown}, robinhoodAttemptNoFill); err != nil {
		d.mu.Unlock()
		return fmt.Sprintf("refused: saving the release failed: %v. The attempt stays held and a retry is safe.", err)
	}
	next.Status = robinhoodAttemptNoFill
	next.durableStatus = robinhoodAttemptNoFill
	next.UpdatedAtMs = now.UnixMilli()
	*current = next
	delete(s.RobinhoodOrders.Attempts, attemptID)
	d.mu.Unlock()
	robinhoodHoldAlerts.Delete(attemptID)
	fmt.Printf("[CRITICAL] rh-resolve: attempt %s %s %s (ref_id %s) released as no_fill on the attestation of operator %s; no venue evidence proves it\n", attemptID, checked.Side, checked.Symbol, checked.RefID, operatorID)
	return fmt.Sprintf("attempt %s released as no_fill on your attestation. The attestation is saved with the attempt. New Robinhood orders for %s may be sent again. If an order for this attempt appears later, the book does not include it: reconcile %s by hand.", attemptID, checked.Symbol, checked.Symbol)
}

func robinhoodFreshLongsReader(d robinhoodEmergencyDeps) func() map[string]float64 {
	return func() map[string]float64 {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return robinhoodFreshLongsLocked(d.state, d.cfgs, time.Now())
	}
}
