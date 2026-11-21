package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

func limitFiniteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// limitStatusEvidence validates one --limit-status answer for one queue row.
// Every use of a venue fill, and every deletion of a queue row, reads the
// order through this check. A non-empty reason means the evidence is missing,
// regressed, incomplete or conflicting, and the row stays unresolved.
func limitStatusEvidence(res *HyperliquidLimitStatusResult, o PendingLimitOrder) (HyperliquidLimitOrderStatus, string) {
	st, reason := limitOrderStateEvidence(res, o)
	if reason != "" {
		return st, reason
	}
	if st.FillsError != "" {
		return st, fmt.Sprintf("fills unreadable (%s)", st.FillsError)
	}
	for _, v := range []float64{st.FilledSize, st.AvgPx, st.Fee, st.ExecutedSize} {
		if !limitFiniteNonNegative(v) {
			return st, fmt.Sprintf("the status carries an unusable number (%v)", v)
		}
	}
	if !st.FillsComplete {
		return st, fmt.Sprintf("the fill history (%.10g) does not yet account for the executed size %.10g", st.FilledSize, st.ExecutedSize)
	}
	if st.FilledSize < o.FilledSize-limitFillEpsilon {
		return st, fmt.Sprintf("the venue fill %.10g is below the recorded %.10g", st.FilledSize, o.FilledSize)
	}
	if o.OrderSize > 0 && st.FilledSize > o.OrderSize*(1+1e-6)+limitFillEpsilon {
		return st, fmt.Sprintf("the venue fill %.10g exceeds the order size %.10g", st.FilledSize, o.OrderSize)
	}
	if st.FilledSize > limitFillEpsilon && !(st.AvgPx > 0) {
		return st, "the venue fill carries no price"
	}
	return st, ""
}

func limitOrderStateEvidence(res *HyperliquidLimitStatusResult, o PendingLimitOrder) (HyperliquidLimitOrderStatus, string) {
	if res == nil {
		return HyperliquidLimitOrderStatus{}, "the status read returned nothing"
	}
	if res.Error != "" {
		return HyperliquidLimitOrderStatus{}, fmt.Sprintf("order state unreadable (%s)", res.Error)
	}
	if res.OpenOrdersError != "" {
		return HyperliquidLimitOrderStatus{}, fmt.Sprintf("open-orders state unknown (%s)", res.OpenOrdersError)
	}
	st, ok := limitStatusForOID(res, o.OrderOID)
	if !ok {
		return HyperliquidLimitOrderStatus{}, "status response did not include the order"
	}
	if st.StatusError != "" {
		return st, fmt.Sprintf("order status unreadable (%s)", st.StatusError)
	}
	if st.Coin != o.Symbol {
		return st, fmt.Sprintf("the exchange reports the order on %q, not %q", st.Coin, o.Symbol)
	}
	if st.Resting == nil {
		return st, "the exchange did not report whether the order is still resting"
	}
	if st.Terminal && *st.Resting {
		return st, fmt.Sprintf("the order reports terminal state %q while it is listed as resting", st.OrderStatus)
	}
	if !st.Terminal && !*st.Resting {
		return st, fmt.Sprintf("the order reports live state %q but is not listed as resting", st.OrderStatus)
	}
	return st, ""
}

func limitStatusRestingForCancel(st HyperliquidLimitOrderStatus, o PendingLimitOrder) bool {
	if st.OID != o.OrderOID || st.Resting == nil || !*st.Resting {
		return false
	}
	return st.Coin == "" || st.Coin == o.Symbol
}

func limitStatusOffBook(st HyperliquidLimitOrderStatus) bool {
	return st.Terminal && st.Resting != nil && !*st.Resting
}

// limitRowOwnerEligible reports a configured Hyperliquid-live type=manual
// owner for the row's coin. Only such an owner proves the book that a positive
// fill watermark was saved with.
func limitRowOwnerEligible(strategies []StrategyConfig, o PendingLimitOrder) bool {
	for _, sc := range strategies {
		if sc.ID != o.StrategyID {
			continue
		}
		if killSwitchLimitOrderAdoptionBlock(sc, true) != "" {
			return false
		}
		return sc.Symbol == o.Symbol || hyperliquidSymbol(sc.Args) == o.Symbol
	}
	return false
}

// limitRowNeedsReview reports a positive watermark that no automatic path may
// adopt from or delete: one that is not sealed with the book, or one with no
// eligible owner.
func limitRowNeedsReview(strategies []StrategyConfig, o PendingLimitOrder) string {
	if o.FilledSize == 0 && o.BookFilledSize == 0 {
		return ""
	}
	if !o.fillSealed() {
		return fmt.Sprintf("its recorded fill %.6f is not sealed with the book (seal %.6f), so it may have been written by an older build without the matching position", o.FilledSize, o.BookFilledSize)
	}
	if !limitRowOwnerEligible(strategies, o) {
		return fmt.Sprintf("its recorded fill %.6f has no configured Hyperliquid-live type=manual owner for %s that proves the book", o.FilledSize, o.Symbol)
	}
	return ""
}

// ------------------------------------------------------------- staging

type limitSettlementBatch struct {
	live     *AppState
	view     *AppState
	staged   map[string]*StrategyState
	order    []string
	captured map[string]*[]ReplayDecision
}

func newLimitSettlementBatch(state *AppState) *limitSettlementBatch {
	view := *state
	view.Strategies = make(map[string]*StrategyState, len(state.Strategies))
	for id, s := range state.Strategies {
		view.Strategies[id] = s
	}
	return &limitSettlementBatch{
		live:     state,
		view:     &view,
		staged:   make(map[string]*StrategyState),
		captured: make(map[string]*[]ReplayDecision),
	}
}

func (b *limitSettlementBatch) stage(id string) *StrategyState {
	if s := b.staged[id]; s != nil {
		return s
	}
	live := b.live.Strategies[id]
	if live == nil {
		return nil
	}
	c := cloneQuoteStrategy(live)
	c.deferTradePersist = true
	captured := []ReplayDecision{}
	c.replayCapture = &captured
	b.staged[id] = c
	b.captured[id] = &captured
	b.order = append(b.order, id)
	b.view.Strategies[id] = c
	return c
}

func (b *limitSettlementBatch) unstage(id string) {
	if b.staged[id] == nil {
		return
	}
	delete(b.staged, id)
	delete(b.captured, id)
	for i, staged := range b.order {
		if staged == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	if live := b.live.Strategies[id]; live != nil {
		b.view.Strategies[id] = live
	}
}

// publish moves the committed books into the published state. It runs only
// after the settlement transaction commits.
func (b *limitSettlementBatch) publish() {
	for _, id := range b.order {
		live := b.live.Strategies[id]
		c := b.staged[id]
		if live == nil || c == nil {
			continue
		}
		c.deferTradePersist = live.deferTradePersist
		c.replayCapture = live.replayCapture
		*live = *c
		if captured := b.captured[id]; captured != nil {
			for _, d := range *captured {
				recordReplayDecision(live, d.DecisionType, d.Symbol, d.Side, d.Quantity, d.ReferencePrice, d.CloseReason, d.DecidedAt, d.EntryATR, d.Regime)
			}
		}
	}
}

type limitCoinSettlement struct {
	decision      limitFillExposureDecision
	deferred      string
	commitErr     error
	applyErrs     []error
	stagedFailure bool
	committed     bool
	logLines      []string
}

var limitSettlementLockWait = time.Duration(0)

// settleCoinLimitFills books the pending fills of one coin in one transaction.
// A row whose fill cannot apply to its book is left out and reported in
// applyErrs; the remaining rows commit together or not at all.
// The manual-action lock of the primary live file serializes it with an
// operator clear; the state lock covers the exposure recheck, the staging, the
// commit and the publication.
func settleCoinLimitFills(state *AppState, cfg *Config, store *StateStore, mu *sync.RWMutex, coin string, rows []*limitFillCandidate, totalDelta, onChainNet float64, readErr error, owners int, now time.Time) limitCoinSettlement {
	var out limitCoinSettlement
	db, err := store.liveFile()
	if err != nil {
		out.commitErr = err
		return out
	}
	unlock, lockErr := acquireManualActionFileLockWithWait(db.path, limitSettlementLockWait)
	if lockErr != nil {
		out.deferred = fmt.Sprintf("the live file's manual-action lock is busy (%v)", lockErr)
		return out
	}
	defer unlock()

	mu.Lock()
	defer mu.Unlock()
	out.decision = classifyLimitFillLiveExposure(coin,
		hyperliquidBookedSignedNetForCoin(cfg.Strategies, state, coin),
		totalDelta, onChainNet, readErr, owners)
	out.decision.Legs = len(rows)
	if !out.decision.adopts() {
		return out
	}

	batch := newLimitSettlementBatch(state)
	out.applyErrs = make([]error, len(rows))
	seals := make([]limitFillSeal, 0, len(rows))
	applied := make(map[string]int, len(rows))
	for i, r := range rows {
		id := r.order.StrategyID
		if batch.stage(id) == nil {
			out.applyErrs[i] = fmt.Errorf("strategy state for %q not found", id)
			continue
		}
		_, line, err := applyLimitFillProgressLogged(batch.view, r.sc, r.order, r.status.FilledSize,
			r.avgPx, r.status.Fee, r.entryATR, resolveATRMethod(r.sc, cfg), now)
		if err != nil {
			out.applyErrs[i] = err
			if applied[id] > 0 {
				out.stagedFailure = true
			} else {
				batch.unstage(id)
			}
			continue
		}
		applied[id]++
		out.logLines = append(out.logLines, line)
		seals = append(seals, limitFillSeal{Prev: r.order, FilledSize: r.status.FilledSize, AvgFillPrice: r.avgPx, FillFee: r.status.Fee})
	}
	if out.stagedFailure || len(seals) == 0 {
		out.logLines = nil
		return out
	}
	if err := store.SettleManualLimitFills(batch.view, batch.order, seals); err != nil {
		out.commitErr = err
		out.logLines = nil
		return out
	}
	batch.publish()
	for i, r := range rows {
		if out.applyErrs[i] != nil {
			continue
		}
		cum := r.status.FilledSize
		r.order.FilledSize = cum
		r.order.BookFilledSize = cum
		r.order.AvgFillPrice = r.avgPx
		r.order.FillFee = r.status.Fee
	}
	out.committed = true
	return out
}

func limitCandidateIDs(rows []*limitFillCandidate) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.order.ID)
	}
	return ids
}

var limitSettlementAlerts = &limitFillExposureTracker{}

func reportLimitSettlementFailure(notifier *MultiNotifier, rows []*limitFillCandidate, coin string, err error, now time.Time) {
	labels := make([]string, 0, len(rows))
	for _, r := range rows {
		labels = append(labels, killSwitchLimitOrderLabel(r.order))
	}
	fmt.Printf("[CRITICAL] limit: %s fill settlement for %s did not commit (%v) — nothing was booked, the queue rows keep their last saved fill, live entries stay held, and the next pass retries from the saved state\n",
		coin, strings.Join(labels, ", "), err)
	fire := false
	for _, r := range rows {
		if limitSettlementAlerts.Record(limitFillExposureKeyFor(r.order), limitFillExposureUnreadable, now) {
			fire = true
		}
	}
	if !fire || notifier == nil || !notifier.HasBackends() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Limit fill not saved: %s**\n", strings.Join(labels, ", "))
	fmt.Fprintf(&b, "The exchange confirms the %s fill, but the save that books it together with its queue row failed: %v.\n", coin, err)
	b.WriteString("• The scheduler booked NOTHING in memory, so the book and the queue row still match the last saved state\n")
	b.WriteString("• New live entries are held until the fill is saved; closes, protection and the kill switch keep running\n")
	b.WriteString("• The reconciler retries from the saved fill every pass and books the fill and its fee once when the save succeeds")
	notifier.SendOwnerDM(b.String())
}

// -------------------------------------------------------- legacy rows

var limitRowReviewAlerts = &limitFillExposureTracker{}

func reportLimitRowReview(notifier *MultiNotifier, o PendingLimitOrder, reason string, ownerEligible bool, now time.Time) {
	fmt.Printf("[CRITICAL] limit: %s needs an operator: %s — the scheduler will not adopt, resize or delete it\n",
		killSwitchLimitOrderLabel(o), reason)
	if !limitRowReviewAlerts.Record(limitFillExposureKeyFor(o), limitFillExposureUnbacked, now) {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Limit order row needs review: %s**\n", killSwitchLimitOrderLabel(o))
	fmt.Fprintf(&b, "The queue row for %s %.6f %s @ $%.4f needs an operator because %s.\n", o.Side, o.OrderSize, o.Symbol, o.LimitPrice, reason)
	b.WriteString("• The scheduler cannot prove that this fill was saved with the position, so it books nothing from it, does not resize the book from the venue size, and never deletes the row on its own\n")
	fmt.Fprintf(&b, "• New limit fills on %s are deferred while this row is unresolved, because the account's %s size could belong to it\n", o.Symbol, o.Symbol)
	if ownerEligible {
		fmt.Fprintf(&b, "• Cancellation still works: `go-trader manual-cancel %s`\n", o.StrategyID)
	} else {
		fmt.Fprintf(&b, "• `manual-cancel` cannot reach this row, because %q is not a configured Hyperliquid-live type=manual strategy for %s. Cancel order %d on the Hyperliquid UI, or restore the strategy in the config\n", o.StrategyID, o.Symbol, o.OrderOID)
	}
	fmt.Fprintf(&b, "• After the order is off-book and you have confirmed on the Hyperliquid UI that this position is flat, run `go-trader manual-clear-limit-row %d --flattened`. It discards the recovery record only and does not change the book. This alert repeats every %s until then",
		o.OrderOID, effectiveAlertThrottleInterval())
	notifier.SendOwnerDM(b.String())
}

var limitApplyFailureAlerts = &limitFillExposureTracker{}

func reportLimitFillApplyFailure(notifier *MultiNotifier, o PendingLimitOrder, exchangeFill float64, applyErr error, peersBooked bool, now time.Time) {
	fmt.Printf("[CRITICAL] limit: %s fill cannot be booked: exchange fill %.6f, row %.6f — %v — the queue row is kept for an operator\n",
		killSwitchLimitOrderLabel(o), exchangeFill, o.FilledSize, applyErr)
	if !limitApplyFailureAlerts.Record(limitFillExposureKeyFor(o), limitFillExposureUnbacked, now) {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Limit fill cannot be booked: %s**\n", killSwitchLimitOrderLabel(o))
	fmt.Fprintf(&b, "The order reports a filled size of %.6f %s and its queue row records %.6f, but the book cannot take the difference: %v.\n",
		exchangeFill, o.Symbol, o.FilledSize, applyErr)
	fmt.Fprintf(&b, "• The scheduler booked NOTHING for this order, so %.6f %s from it is NOT tracked and has NO protection from the scheduler. Check the position on the Hyperliquid UI\n",
		exchangeFill-o.FilledSize, o.Symbol)
	if peersBooked {
		fmt.Fprintf(&b, "• The other confirmed limit fills on %s in this pass were booked as usual\n", o.Symbol)
	}
	b.WriteString("• The queue row is kept as the recovery record and is marked as needing an operator. The scheduler retries every pass and books the fill on its own if the book can take it later\n")
	fmt.Fprintf(&b, "• To stop further fills, cancel the order: `go-trader manual-cancel %s`\n", o.StrategyID)
	fmt.Fprintf(&b, "• After the order is off-book and you have handled the position on the Hyperliquid UI, run `go-trader manual-clear-limit-row %d --flattened`. It discards the recovery record only and does not change any book. This alert repeats every %s until then",
		o.OrderOID, effectiveAlertThrottleInterval())
	notifier.SendOwnerDM(b.String())
}

var limitUnresolvedPassThreshold = 5

var limitUnresolvedPasses = struct {
	sync.Mutex
	n map[int64]int
}{n: make(map[int64]int)}

func noteLimitRowUnresolved(id int64) int {
	limitUnresolvedPasses.Lock()
	defer limitUnresolvedPasses.Unlock()
	limitUnresolvedPasses.n[id]++
	return limitUnresolvedPasses.n[id]
}

func clearLimitRowUnresolved(id int64) {
	limitUnresolvedPasses.Lock()
	defer limitUnresolvedPasses.Unlock()
	delete(limitUnresolvedPasses.n, id)
}

func retainLimitRowUnresolved(orders []PendingLimitOrder) {
	live := make(map[int64]bool, len(orders))
	for _, o := range orders {
		live[o.ID] = true
	}
	limitUnresolvedPasses.Lock()
	defer limitUnresolvedPasses.Unlock()
	for id := range limitUnresolvedPasses.n {
		if !live[id] {
			delete(limitUnresolvedPasses.n, id)
		}
	}
}

var limitRowUnresolvedAlerts = &limitFillExposureTracker{}

func reportLimitRowUnresolved(notifier *MultiNotifier, o PendingLimitOrder, reason string, passes int, now time.Time) {
	fmt.Printf("[CRITICAL] limit: %s has been unresolved for %d passes in a row: %s — the queue row is kept for an operator\n",
		killSwitchLimitOrderLabel(o), passes, reason)
	if !limitRowUnresolvedAlerts.Record(limitFillExposureKeyFor(o), limitFillExposureUnbacked, now) {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Limit order cannot be verified: %s**\n", killSwitchLimitOrderLabel(o))
	fmt.Fprintf(&b, "The scheduler could not verify %s %.6f %s @ $%.4f for %d passes in a row: %s.\n", o.Side, o.OrderSize, o.Symbol, o.LimitPrice, passes, reason)
	b.WriteString("• While the evidence is incomplete the scheduler books nothing from this order and never deletes its queue row. A fill on it is NOT tracked and has NO protection from the scheduler until the evidence reads complete\n")
	b.WriteString("• A queued cancel or an expiry is still sent to the exchange while the order is listed as resting\n")
	b.WriteString("• The scheduler retries every pass and resumes on its own when the status and the fill history read complete\n")
	fmt.Fprintf(&b, "• If it does not recover: cancel the order with `go-trader manual-cancel %s`, check the %s position on the Hyperliquid UI, and after the order is off-book run `go-trader manual-clear-limit-row %d --flattened`. It discards the recovery record only and does not change any book. This alert repeats every %s until then",
		o.StrategyID, o.Symbol, o.OrderOID, effectiveAlertThrottleInterval())
	notifier.SendOwnerDM(b.String())
}

func issueLimitRowCancel(script string, o PendingLimitOrder, why string) bool {
	cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(script, o.Symbol, o.OrderOID)
	if cstderr != "" {
		fmt.Fprintf(os.Stderr, "[limit] %s cancel stderr: %s\n", o.StrategyID, cstderr)
	}
	if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
		fmt.Printf("[limit] cancel (%s) failed for %s oid=%d: %v — will retry\n", why, o.StrategyID, o.OrderOID, cerr)
		return false
	}
	fmt.Printf("[limit] %s oid=%d cancel issued (%s) while its fill evidence is unresolved; the row stays until the evidence is complete\n", o.StrategyID, o.OrderOID, why)
	return true
}

func limitRowCancelDue(o PendingLimitOrder, now time.Time) (bool, string) {
	if !o.ExpiresAt.IsZero() && now.After(o.ExpiresAt) {
		return true, "TTL expiry"
	}
	if o.CancelRequested {
		return true, "operator cancel"
	}
	return false, ""
}

func markLimitRowOperatorRequired(store *StateStore, o *PendingLimitOrder, now time.Time) {
	if store == nil || o == nil {
		return
	}
	since, err := store.MarkPendingLimitOrderOperatorRequiredOnce(o.ID, now)
	if err != nil {
		fmt.Printf("[limit] failed to persist the operator-required marker on row %d: %v\n", o.ID, err)
		return
	}
	o.OperatorRequiredSince = since
}

// ---------------------------------------------- generic reconcile guard

type hlLimitGrowthHoldSet struct {
	mu         sync.Mutex
	active     bool
	unreadable string
	rows       map[string]bool
}

var (
	hlLimitOwnershipLoader func() ([]PendingLimitOrder, error)
	hlLimitGrowthHolds     = &hlLimitGrowthHoldSet{}
)

func hlLimitGrowthKey(strategyID, coin string) string {
	return strategyID + "\x00" + coin
}

func (h *hlLimitGrowthHoldSet) refresh(loader func() ([]PendingLimitOrder, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = loader != nil
	h.unreadable = ""
	h.rows = make(map[string]bool)
	if loader == nil {
		return
	}
	orders, err := loader()
	if err != nil {
		h.unreadable = err.Error()
		return
	}
	for _, o := range orders {
		h.rows[hlLimitGrowthKey(o.StrategyID, o.Symbol)] = true
	}
}

func (h *hlLimitGrowthHoldSet) clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = false
	h.unreadable = ""
	h.rows = nil
}

// reason reports why venue size alone may not grow or flip a strategy's book:
// a manual limit order still has unresolved fill ownership on the coin, or the
// queue that would say so cannot be read.
func (h *hlLimitGrowthHoldSet) reason(sc StrategyConfig, coin string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		return ""
	}
	if h.unreadable != "" {
		if sc.Type == "manual" {
			return fmt.Sprintf("the limit order queue is unreadable (%s)", h.unreadable)
		}
		return ""
	}
	if h.rows[hlLimitGrowthKey(sc.ID, coin)] {
		return "a manual limit order on this coin still owns its fill"
	}
	return ""
}

// ------------------------------------------------- standalone admission

// standaloneLimitQueueRefusal checks the live queue for a command-line entry
// that runs with no scheduler witness. The queue must be readable, and no row
// may carry a fill that is not sealed with the book or that has no eligible
// owner in the config.
func standaloneLimitQueueRefusal(layout storageLayout, strategies []StrategyConfig) error {
	spec, ok := layout.spec(storageRolePrimary)
	if !ok || spec.InMemory {
		return nil
	}
	if _, err := os.Stat(spec.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := openStateDBReadOnly(spec.Path)
	if err != nil {
		return manualEntryRefused("the resting limit order queue cannot be verified: %v", err)
	}
	defer db.Close()
	db.role = storageRolePrimary
	orders, err := db.LoadPendingLimitOrders()
	if err != nil {
		return manualEntryRefused("the resting limit order queue cannot be verified: %v", err)
	}
	for _, o := range orders {
		if !o.fillSealed() {
			return manualEntryRefused("queue row %d (oid=%d) records a fill of %.6f that is not sealed with the book; resolve it before a live entry", o.ID, o.OrderOID, o.FilledSize)
		}
		if review := limitRowNeedsReview(strategies, o); review != "" {
			return manualEntryRefused("queue row %d (oid=%d) needs an operator because %s; resolve it before a live entry", o.ID, o.OrderOID, review)
		}
	}
	return nil
}
