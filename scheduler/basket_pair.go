package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const pairActionRequestVersion = 2

const pairMaxActionsPerStep = 16

type pairActionRequest struct {
	V                int     `json:"v"`
	PairID           string  `json:"pair_id"`
	Partition        string  `json:"partition"`
	MemberID         string  `json:"member_id"`
	Provider         string  `json:"provider"`
	AccountKey       string  `json:"account_key"`
	Instrument       string  `json:"instrument"`
	ClientOrderID    string  `json:"client_order_id"`
	Purpose          string  `json:"purpose"`
	OrderSide        string  `json:"order_side"`
	PositionSide     string  `json:"position_side"`
	BaseQty          float64 `json:"base_qty"`
	NativeQty        float64 `json:"native_qty"`
	ContractSize     float64 `json:"contract_size"`
	PriceBound       float64 `json:"price_bound"`
	AcceptDeadlineMs int64   `json:"accept_deadline_ms"`
	QuoteEventID     string  `json:"quote_event_id,omitempty"`
	OriginSlotMs     int64   `json:"origin_slot_ms"`
	ReduceOnly       bool    `json:"reduce_only"`

	ContractFingerprint string                   `json:"contract_fingerprint"`
	Conversions         []pairConversionEvidence `json:"conversions,omitempty"`
	CapabilityHash      string                   `json:"capability_hash,omitempty"`
	EvidenceHash        string                   `json:"evidence_hash,omitempty"`
	Reference           *pairObservation         `json:"reference,omitempty"`
	ReferenceMaxAgeMs   int64                    `json:"reference_max_age_ms,omitempty"`
	ReferenceReceiptMs  int64                    `json:"reference_receipt_ms,omitempty"`
	ReferencePx         string                   `json:"reference_px,omitempty"`
	NativeBooked        bool                     `json:"native_booked,omitempty"`
}

type pairRecoveryNote struct {
	NativeBooked bool   `json:"native_booked,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (a *basketAction) recoveryNote() pairRecoveryNote {
	var n pairRecoveryNote
	if strings.TrimSpace(a.RecoveryJSON) != "" {
		_ = json.Unmarshal([]byte(a.RecoveryJSON), &n)
	}
	n.NativeBooked = a.pairNative()
	return n
}

type pairEnvelope struct {
	CutoffMs     int64
	InputFP      string
	EvidenceJSON string
	SnapshotID   string
	Generation   uint64
}

type pairStepInputs struct {
	views pairViewSet
	nowMs int64
}

func (in pairStepInputs) view(id string) (pairMemberValuation, bool) {
	v, ok := in.views[id]
	return v, ok
}

func (in pairStepInputs) at(now time.Time) pairStepInputs {
	in.nowMs = now.UnixMilli()
	return in
}

type pairStepSnapshot struct {
	slots     map[int64]string
	actions   map[string]string
	confirmed map[string]float64
	progress  map[string]string
	books     map[string]string
	lifecycle string
	seq       int64
}

func (bc *basketCoordinator) pairSnapshot(rt *basketRuntime) pairStepSnapshot {
	s := pairStepSnapshot{slots: map[int64]string{}, actions: map[string]string{}, confirmed: map[string]float64{}, progress: map[string]string{}, books: map[string]string{}}
	for d, rb := range rt.Rebalances {
		s.slots[d] = rb.Status + "|" + rb.TargetsJSON
	}
	for id, a := range rt.Actions {
		s.actions[id] = a.Status
		s.confirmed[id] = a.ConfirmedQty
		s.progress[id] = fmt.Sprintf("%v|%v|%v|%d|%v", a.AppliedQty, a.AppliedNotional, a.AppliedFee, a.AppliedFeeVersion, a.ConfirmedNotional)
	}
	for _, m := range rt.Contract.Members {
		st := bc.state.Strategies[m.StrategyID]
		book := fmt.Sprintf("%v", basketPositionSigned(st, m.Coin))
		if st != nil {
			book += fmt.Sprintf("|%v", st.Cash)
			if pos := st.Positions[m.Coin]; pos != nil {
				book += fmt.Sprintf("|%v", pos.AvgCost)
			}
		}
		s.books[m.StrategyID] = book
	}
	s.lifecycle = rt.Pair.Progress.Lifecycle
	s.seq = rt.Pair.Progress.Seq
	return s
}

func (a pairStepSnapshot) same(b pairStepSnapshot) bool {
	if a.lifecycle != b.lifecycle || a.seq != b.seq || len(a.slots) != len(b.slots) || len(a.actions) != len(b.actions) {
		return false
	}
	for k, v := range a.slots {
		if b.slots[k] != v {
			return false
		}
	}
	for k, v := range a.actions {
		if b.actions[k] != v || b.confirmed[k] != a.confirmed[k] || b.progress[k] != a.progress[k] {
			return false
		}
	}
	for k, v := range a.books {
		if b.books[k] != v {
			return false
		}
	}
	return true
}

func pairStartupLine(rt *basketRuntime) string {
	line := fmt.Sprintf("[pair] %s (%s): research pair %s on %s, policy %s, lifecycle %s", rt.Contract.BasketID, partitionLabel(rt.Partition), rt.Contract.Pair.Underlying,
		strings.Join(pairMemberLabels(rt), " + "), rt.Contract.Pair.Policy, rt.Pair.Progress.Lifecycle)
	for _, a := range rt.pairUnresolvedActions() {
		line += fmt.Sprintf("; PENDING %s action %s (%s %s %s, client %s)", a.Purpose, a.ActionID, a.StrategyID, a.Direction, a.Status, a.ClientOrderID)
	}
	if pm := rt.Pair.Progress.PositionMatch; pm != nil && pm.Status != pairPositionMatched {
		line += fmt.Sprintf("; POSITION MATCH %s at %s: %s (a new venue read is required before any increase)", pm.Status, formatClosedBarMs(pm.CheckedAtMs), pm.Reason)
	}
	return line
}

func pairMemberLabels(rt *basketRuntime) []string {
	var out []string
	for _, m := range rt.Contract.pairMembersSorted() {
		out = append(out, m.Provider+":"+m.Instrument)
	}
	return out
}

func (bc *basketCoordinator) pairLogf(rt *basketRuntime, format string, a ...interface{}) {
	fmt.Printf("[pair %s] %s\n", rt.Contract.BasketID, fmt.Sprintf(format, a...))
}

func (bc *basketCoordinator) pairAlert(rt *basketRuntime, key, msg string, critical bool) {
	if rt.Pair.Progress.alerted(key) {
		return
	}
	rt.Pair.Progress.markAlerted(key)
	rt.touchPairProgress()
	bc.emit(rt, msg, critical)
}

func (bc *basketCoordinator) persistPair(rt *basketRuntime) bool {
	bc.clearPairEndedObligations(rt)
	if err := bc.store.PersistBasketProgress(rt); err != nil {
		bc.pairLogf(rt, "could not save pair progress: %v; elective work holds until it saves, and queries and protection continue", err)
		return false
	}
	return true
}

func (bc *basketCoordinator) pairVenueFor(sc StrategyConfig) (pairVenue, error) {
	v, err := bc.venueFor(sc)
	if err != nil {
		return nil, err
	}
	pv, ok := v.(pairVenue)
	if !ok {
		return nil, fmt.Errorf("strategy %s has no pair venue seam", sc.ID)
	}
	if vv, isVirtual := pv.(*pairVirtualVenue); isVirtual && !vv.isAttached() {
		return nil, fmt.Errorf("strategy %s virtual venue is not attached to its state store; no virtual order or query runs", sc.ID)
	}
	rt := bc.byMember[sc.ID]
	if rt == nil || !rt.isPair() {
		return nil, fmt.Errorf("strategy %s has no pair runtime, so its transport identity cannot be checked", sc.ID)
	}
	if err := rt.Pair.admission.checkTransport(sc, pv); err != nil {
		return nil, err
	}
	return pv, nil
}

func (bc *basketCoordinator) pairLiveRefusal(rt *basketRuntime) string {
	adm := rt.Pair.admission
	if !adm.valid() {
		return "the pair has no internal research admission; production pair dispatch is refused"
	}
	if rt.Partition.IsLive() && adm.kind != pairAdmissionLiveShapedFixture {
		return "live pair dispatch is refused; only the controlled live-shaped fixture opens a live-partition pair"
	}
	for _, id := range rt.memberIDs() {
		if _, ok := bc.members[id]; !ok {
			return fmt.Sprintf("member %s has no configuration", id)
		}
	}
	return ""
}

func (bc *basketCoordinator) recoverInterruptedPair(rt *basketRuntime, now time.Time) {
	changed := false
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		if rb.Status == basketRebalanceReserved && rt.pairResumeSlots[d] {
			continue
		}
		if rb.Status == basketRebalanceReserved || rb.Status == basketRebalanceRanking {
			rb.Status = basketRebalanceHeld
			rb.HoldReason = "the scheduler restarted before the quote envelope froze; a cadence slot is never collected twice"
			rb.Outcome.Result = "held"
			rb.UpdatedAtMs = now.UnixMilli()
			changed = true
			bc.pairLogf(rt, "slot %s: %s", formatClosedBarMs(d), rb.HoldReason)
		}
	}
	if bc.recoverPairNativeLegacy(rt, now) {
		changed = true
	}
	if changed {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
}

func (bc *basketCoordinator) pairStartupErrors(rt *basketRuntime) []string {
	var errs []string
	id := rt.Contract.BasketID
	open := false
	for _, m := range rt.Contract.Members {
		s := bc.state.Strategies[m.StrategyID]
		if s == nil {
			continue
		}
		for sym, pos := range s.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			if sym != m.Coin {
				errs = append(errs, fmt.Sprintf("pair %s member %s holds a %s position outside its instrument %s", id, m.StrategyID, sym, m.Coin))
				continue
			}
			open = true
		}
		if len(s.OptionPositions) > 0 {
			errs = append(errs, fmt.Sprintf("pair %s member %s holds option positions", id, m.StrategyID))
		}
	}
	expected := false
	for _, q := range rt.Expected {
		if q != 0 {
			expected = true
		}
	}
	for _, m := range rt.Contract.pairMembersSorted() {
		if sc, ok := bc.members[m.MemberID]; ok {
			if v, err := bc.venueFor(sc); err == nil {
				if vv, virtual := v.(*pairVirtualVenue); virtual && !vv.isAttached() {
					errs = append(errs, fmt.Sprintf("pair %s member %s virtual venue is not attached to its state store; attach it after the runtime load and before startup", id, m.MemberID))
				}
			}
		}
	}
	pending := len(rt.pairUnresolvedActions()) > 0
	for _, u := range rt.Pair.Uses {
		if u.Status == pairUseReserved {
			pending = true
		}
	}
	for _, rb := range rt.Rebalances {
		if !basketRebalanceTerminal(rb.Status) {
			pending = true
		}
	}
	for _, o := range rt.Pair.Progress.Obligations {
		if pairRecoveryObligation(o) {
			pending = true
		}
	}
	if len(rt.Pair.Progress.NativeReceipts) > 0 || rt.Pair.Progress.hasStagedPaperClose() || len(rt.Pair.Progress.nativeRecoveryStaged()) > 0 {
		pending = true
	}
	if rt.Pair.Progress.Lifecycle != "" && rt.Pair.Progress.Lifecycle != pairLifecycleFlat {
		pending = true
	}
	if rt.SavedContract != nil && rt.SavedFP != rt.Contract.Fingerprint() && (pending || open || expected) {
		errs = append(errs, fmt.Sprintf("pair %s contract changed (%s) while a position, an unapplied fill, a funding gap or recovery evidence remains; restore the saved contract or flatten and resolve the pair first", id, strings.Join(basketContractDiff(*rt.SavedContract, rt.Contract), ", ")))
	}
	if rt.SavedContract == nil && open {
		errs = append(errs, fmt.Sprintf("pair %s has no saved contract, but a member already holds a position; a research run starts from fresh-owned state", id))
	}
	for _, sc := range bc.cfg.Strategies {
		if isPairMember(sc) && sc.PairID == id {
			continue
		}
		if partitionFor(sc) != rt.Partition {
			continue
		}
		s := bc.state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		for sym, pos := range s.Positions {
			if pos != nil && pos.Quantity > 0 && strings.EqualFold(strings.TrimSuffix(strings.ToUpper(sym), "/USDT"), rt.Contract.Pair.Underlying) {
				errs = append(errs, fmt.Sprintf("strategy %s holds %s in the %s partition, which pair %s owns exclusively", sc.ID, sym, partitionLabel(rt.Partition), id))
			}
		}
	}
	return errs
}

func (bc *basketCoordinator) observePair(sc StrategyConfig, fields StrategyDecisionFields, indicators map[string]interface{}, price float64, echoErr string) {
	if bc == nil {
		return
	}
	rt := bc.byMember[sc.ID]
	if rt == nil || !rt.isPair() {
		return
	}
	if echoErr != "" {
		delete(rt.observations, sc.ID)
		if rt.pairEchoFailures == nil {
			rt.pairEchoFailures = map[string]string{}
		}
		rt.pairEchoFailures[sc.ID] = echoErr
		return
	}
	delete(rt.pairEchoFailures, sc.ID)
	clock := bc.pairClock
	if clock == nil {
		clock = tradingNow
	}
	rt.observations[sc.ID] = basketMemberObservation{Cycle: bc.cycle, AtMs: clock().UnixMilli(), Fields: fields, Indicators: indicators, Price: price}
}

func (bc *basketCoordinator) reservePairSlot(rt *basketRuntime, slotMs int64, now time.Time) (*basketRebalance, error) {
	if !rt.isPair() {
		return nil, fmt.Errorf("slot reservation needs a pair runtime")
	}
	if slotMs <= 0 {
		return nil, fmt.Errorf("pair slot time must be positive")
	}
	if _, ok := rt.Rebalances[slotMs]; ok {
		return nil, errBasketDeadlineTaken
	}
	rb := &basketRebalance{
		Partition:   rt.Partition,
		BasketID:    rt.Contract.BasketID,
		DeadlineMs:  slotMs,
		Status:      basketRebalanceReserved,
		ContractFP:  rt.Contract.Fingerprint(),
		CreatedAtMs: now.UnixMilli(),
		UpdatedAtMs: now.UnixMilli(),
	}
	if err := bc.store.ReserveBasketRebalance(rt, rb); err != nil {
		return nil, err
	}
	rt.Rebalances[slotMs] = rb
	return rb, nil
}

func (bc *basketCoordinator) recheckPairSlot(rt *basketRuntime, slotMs int64, contractFP string) error {
	return bc.store.RecheckPairSlot(rt, slotMs, contractFP)
}

func (bc *basketCoordinator) freezePairEnvelope(rt *basketRuntime, slotMs int64, env pairEnvelope, now time.Time) error {
	rb := rt.Rebalances[slotMs]
	if rb == nil || rb.Status != basketRebalanceReserved {
		return fmt.Errorf("pair %s slot %d is not reserved", rt.Contract.BasketID, slotMs)
	}
	next := *rb
	next.CutoffMs, next.InputFP, next.EvidenceJSON, next.SnapshotID, next.Generation = env.CutoffMs, env.InputFP, env.EvidenceJSON, env.SnapshotID, env.Generation
	next.UpdatedAtMs = now.UnixMilli()
	if err := bc.store.FreezePairEnvelope(rt, &next); err != nil {
		return err
	}
	rb.CutoffMs, rb.InputFP, rb.EvidenceJSON, rb.SnapshotID, rb.Generation = next.CutoffMs, next.InputFP, next.EvidenceJSON, next.SnapshotID, next.Generation
	rb.Status, rb.durableStatus, rb.UpdatedAtMs = basketRebalanceFrozen, basketRebalanceFrozen, next.UpdatedAtMs
	return nil
}

func (bc *basketCoordinator) stepPair(rt *basketRuntime, in basketCycleInputs) {
	rt.clock = in.Now
	bc.notePairStep(rt, in)
	rt.Pair.noteNativeRequestsSeen()
	if msg := bc.pairLiveRefusal(rt); msg != "" {
		bc.pairAlert(rt, "refused|"+msg, msg+"; no pair action is dispatched", true)
		return
	}
	if !bc.ensurePairCostEvidence(rt) {
		return
	}
	bc.drainPairCostFaults(rt, in.Now)
	if rt.needsDurable {
		err := bc.store.SavePartition(bc.state, rt.Partition)
		if err != nil && bc.rollbackPairFeeFixes(rt, err, in.Now) {
			err = bc.store.SavePartition(bc.state, rt.Partition)
		}
		if err != nil {
			bc.pairLogf(rt, "the last pair settlement is not durable yet (%v); queries, protection and recovery continue, and no elective order is sent", err)
		} else {
			rt.needsDurable = false
		}
	}
	in = bc.refreshPairInputs(rt, in)
	defer bc.flushPairNativeAlerts(rt)
	bc.stepPairNativeRecovery(rt, in)
	in = bc.refreshPairInputs(rt, in)
	inputs := bc.pairValuationsLocked(rt)
	bc.attachPairNativeReceipts(rt, in)
	bc.stepPairEmergency(rt, in, inputs)
	electiveBlocked := bc.reconcilePairActions(rt, in, inputs)
	in = bc.refreshPairInputs(rt, in)
	bc.detectPairExceptions(rt, in)
	bc.checkPairVirtualConservation(rt)
	bc.stepPairFunding(rt, in)
	bc.pairLifecycleChecks(rt, in)
	if !in.SaveBlocked {
		in = bc.stepPairPositions(rt, in)
	}
	bc.executePairProtective(rt, in, inputs)
	in = bc.refreshPairInputs(rt, in)
	if !in.SaveBlocked && !rt.needsDurable {
		in = bc.collectPairSlot(rt, in)
		if !rt.needsDurable {
			bc.decidePendingPairSlots(rt, in)
		}
		in = bc.refreshPairInputs(rt, in)
	}
	if electiveBlocked || in.SaveBlocked || rt.needsDurable {
		return
	}
	bc.executePair(rt, in, inputs)
}

func (bc *basketCoordinator) reconcilePairActions(rt *basketRuntime, in basketCycleInputs, inputs pairStepInputs) bool {
	blocked := false
	for _, a := range rt.pairUnresolvedActions() {
		if !basketActionPending(a.Status) {
			continue
		}
		ok := bc.reconcilePairAction(rt, a, in, inputs)
		if !ok && pairElectivePurpose(a.Purpose) {
			blocked = true
		}
	}
	if rt.pairPendingElective() != nil {
		blocked = true
	}
	return blocked
}

func (bc *basketCoordinator) releasePairEntry(rt *basketRuntime, a *basketAction, reason string, now time.Time) {
	s := bc.state.Strategies[a.StrategyID]
	if in := bc.intentFor(s, a); in != nil && in.unresolved() {
		in.Status = entryIntentNoFill
		in.HoldReason = ""
		in.EvidenceJSON = marshalEntryJSON(entryIntentEvidence{Outcome: "no_fill", Detail: reason, ObservedAtMs: now.UnixMilli()})
		in.UpdatedAtMs = now.UnixMilli()
	}
	for _, u := range rt.Pair.Uses {
		if u.ActionID == a.ActionID && u.Status == pairUseReserved {
			u.Status = pairUseReleased
			u.UpdatedAtMs = now.UnixMilli()
		}
	}
}

func (bc *basketCoordinator) reconcilePairAction(rt *basketRuntime, a *basketAction, in basketCycleInputs, inputs pairStepInputs) bool {
	sc, ok := bc.members[a.StrategyID]
	if !ok {
		bc.pairAlert(rt, "orphan|"+a.ActionID, fmt.Sprintf("pair action %s names %s, which has no configuration; it stays unresolved", a.ActionID, a.StrategyID), true)
		return false
	}
	now := in.Now
	if a.pairNative() && a.Status != basketActionSettling {
		bc.resolvePairNativeAction(rt, a, now)
		return !basketActionPending(a.Status)
	}
	switch a.Status {
	case basketActionReserved:
		a.Status = basketActionNoFill
		a.Terminal = true
		a.HoldReason = "the action was reserved but never marked submitting, so no order was sent"
		a.UpdatedAtMs = now.UnixMilli()
		bc.releasePairEntry(rt, a, a.HoldReason, now)
		rt.touchPairProgress()
		bc.pairLogf(rt, "action %s: %s", a.ActionID, a.HoldReason)
		return bc.persistPair(rt)
	case basketActionSettling:
		if bc.settlePairAction(rt, a, in, inputs) {
			return !basketActionPending(a.Status)
		}
		if a.pairNative() || !bc.pairHasSettlementObligation(rt, a) {
			return false
		}
	}
	v, err := bc.pairVenueFor(sc)
	if err != nil {
		bc.pairAlert(rt, "venue|"+a.ActionID, err.Error(), true)
		return false
	}
	ptr := a
	snap := *a
	queried := *a
	var out basketVenueOutcome
	var qerr error
	bc.notePairVirtualInventory(rt, v, a.StrategyID)
	bc.unlocked(func() { out, qerr = v.Query(&queried) })
	cur := rt.Actions[snap.ActionID]
	if cur != ptr || cur.Status != snap.Status || cur.ConfirmedQty != snap.ConfirmedQty || cur.FeeEvidenceVersion != snap.FeeEvidenceVersion || cur.AppliedQty != snap.AppliedQty ||
		cur.FillCoverageJSON != snap.FillCoverageJSON || cur.FeeCoverageJSON != snap.FeeCoverageJSON || cur.SettlementJSON != snap.SettlementJSON || cur.FeeFinal != snap.FeeFinal {
		bc.pairLogf(rt, "action %s changed while its venue query ran; the stale answer is discarded and the next cycle queries again", snap.ActionID)
		return false
	}
	in = bc.refreshPairInputs(rt, in)
	if qerr != nil {
		bc.pairLogf(rt, "action %s (client %s) is still unresolved: the venue query failed: %v; the saved identity is kept and new risk holds", a.ActionID, a.ClientOrderID, qerr)
		return false
	}
	if why := pairVirtualOutcomeFuture(v, out, in.Now.UnixMilli()); why != "" {
		bc.holdPairAction(rt, a, "malformed or inconsistent venue evidence: "+why, true, in.Now)
		return false
	}
	return bc.applyPairOutcome(rt, a, out, in, inputs)
}

func pairOutcomeError(a *basketAction, out basketVenueOutcome) string {
	if out.ClientOrderID != "" && out.ClientOrderID != a.ClientOrderID {
		return fmt.Sprintf("the venue answered for client order %s, not %s", out.ClientOrderID, a.ClientOrderID)
	}
	if out.AccountKey != "" && out.AccountKey != a.AccountKey {
		return fmt.Sprintf("account mismatch: the venue answered for %s, the action is bound to %s", out.AccountKey, a.AccountKey)
	}
	if out.Instrument != "" && out.Instrument != a.Instrument {
		return fmt.Sprintf("the venue answered for instrument %s, the action names %s", out.Instrument, a.Instrument)
	}
	if out.Side != "" && out.Side != a.OrderSide {
		return fmt.Sprintf("the venue answered for side %s, the action is a %s", out.Side, a.OrderSide)
	}
	if out.VenueOrderID != "" && a.VenueOrderID != "" && out.VenueOrderID != a.VenueOrderID {
		return fmt.Sprintf("the venue answered for venue order %s, the action is bound to venue order %s", out.VenueOrderID, a.VenueOrderID)
	}
	switch out.Outcome {
	case basketVenueUnknown, pairVenueNeverSent, pairVenueAbsent:
		if out.CumQty != 0 || out.CumNotional != 0 {
			return fmt.Sprintf("an %s answer carries fill evidence (%v, %v)", out.Outcome, out.CumQty, out.CumNotional)
		}
		if out.Outcome == pairVenueNeverSent && a.ConfirmedQty > 0 {
			return "a never-sent answer contradicts saved fill evidence"
		}
		return ""
	case basketVenueFilled, basketVenuePartial, pairVenueOpen, basketVenueRejected:
	default:
		return fmt.Sprintf("unknown venue outcome %q", out.Outcome)
	}
	if !pairFinite(out.CumQty) || !pairFinite(out.CumNotional) || out.CumQty < 0 || out.CumNotional < 0 {
		return fmt.Sprintf("the cumulative fill evidence (%v, %v) is not finite and non-negative", out.CumQty, out.CumNotional)
	}
	if out.CumQty > a.RequestedQty*(1+1e-9)+1e-12 {
		return fmt.Sprintf("overfill: %v confirmed of %v requested", out.CumQty, a.RequestedQty)
	}
	if out.CumQty+1e-12 < a.ConfirmedQty || out.CumNotional < a.ConfirmedNotional*(1-1e-12)-1e-12 {
		return fmt.Sprintf("the cumulative fill evidence decreased (%v/%v after %v/%v)", out.CumQty, out.CumNotional, a.ConfirmedQty, a.ConfirmedNotional)
	}
	if out.CumQty > 0 && !(out.CumNotional > 0) {
		return "a confirmed quantity has no confirmed notional"
	}
	if out.Outcome == basketVenueFilled && !basketQtyEqual(out.CumQty, a.RequestedQty) {
		return fmt.Sprintf("a filled answer confirms %v of %v requested", out.CumQty, a.RequestedQty)
	}
	if out.CumQty > a.ConfirmedQty && a.PriceBound > 0 {
		dq := out.CumQty - a.ConfirmedQty
		dn := out.CumNotional - a.ConfirmedNotional
		px := dn / dq
		if a.OrderSide == "buy" && px > a.PriceBound*(1+1e-9) {
			return fmt.Sprintf("the incremental fill price %v is above the saved buy bound %v", px, a.PriceBound)
		}
		if a.OrderSide == "sell" && px < a.PriceBound*(1-1e-9) {
			return fmt.Sprintf("the incremental fill price %v is below the saved sell bound %v", px, a.PriceBound)
		}
	}
	if (out.CumQty > 0 || out.Outcome == pairVenueOpen) && a.AcceptDeadlineMs > 0 {
		if out.AcceptedAtMs <= 0 || !pairBeforeDeadline(out.AcceptedAtMs, a.AcceptDeadlineMs) {
			return fmt.Sprintf("the venue acceptance time %d is not before the saved deadline %d", out.AcceptedAtMs, a.AcceptDeadlineMs)
		}
	}
	if out.FeePresent || out.FeeEvidenceVersion > 0 {
		if out.FeeEvidenceID == "" || out.FeeEvidenceVersion < 1 || !pairFinite(out.FeeAmount) {
			return "fee evidence needs an identity, a version of at least 1 and a finite signed amount"
		}
	}
	if why := pairValidateVenueFills(a, out); why != "" {
		return why
	}
	return pairFillsImmutable(a.EvidenceJSON, out)
}

func (bc *basketCoordinator) holdPairAction(rt *basketRuntime, a *basketAction, reason string, freeze bool, now time.Time) {
	if a.Status == basketActionSubmitting || a.Status == basketActionReserved {
		a.Status = basketActionUnresolved
	}
	a.HoldReason = reason
	a.UpdatedAtMs = now.UnixMilli()
	if freeze {
		p := &rt.Pair.Progress
		p.IncreasesFrozen = true
		if p.FreezeReason == "" {
			p.FreezeReason = fmt.Sprintf("%s action %s: %s", a.StrategyID, a.ActionID, reason)
		}
	}
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairAlert(rt, "hold|"+a.ActionID+"|"+reason, fmt.Sprintf("%s pair action %s (client %s): %s; it stays queryable by its saved identity and new risk holds", a.StrategyID, a.ActionID, a.ClientOrderID, reason), true)
}

func (bc *basketCoordinator) applyPairOutcome(rt *basketRuntime, a *basketAction, out basketVenueOutcome, in basketCycleInputs, inputs pairStepInputs) bool {
	now := in.Now
	if out.ObservedAtMs == 0 {
		out.ObservedAtMs = now.UnixMilli()
	}
	if why := pairOutcomeError(a, out); why != "" {
		bc.holdPairAction(rt, a, "malformed or inconsistent venue evidence: "+why, true, now)
		return false
	}
	evidence := marshalEntryJSON(out)
	if out.VenueOrderID != "" {
		a.VenueOrderID = out.VenueOrderID
	}
	if out.CoverageThroughMs > a.CoverageThroughMs {
		a.CoverageThroughMs = out.CoverageThroughMs
	}
	live := bc.pairLiveShaped(a)
	if !live && (out.FeePresent || out.FeeEvidenceVersion > 0) {
		switch {
		case out.FeeEvidenceVersion > a.FeeEvidenceVersion:
			a.FeePresent, a.FeeCurrency, a.FeeAmount = out.FeePresent, strings.ToUpper(strings.TrimSpace(out.FeeCurrency)), out.FeeAmount
			a.FeeEvidenceID, a.FeeEvidenceVersion = out.FeeEvidenceID, out.FeeEvidenceVersion
			if out.FeePresent {
				a.ConfirmedFee = out.FeeAmount
			}
		case out.FeeEvidenceVersion == a.FeeEvidenceVersion:
			if out.FeeEvidenceID != a.FeeEvidenceID || out.FeeAmount != a.FeeAmount || strings.ToUpper(strings.TrimSpace(out.FeeCurrency)) != a.FeeCurrency || out.FeePresent != a.FeePresent {
				bc.holdPairAction(rt, a, fmt.Sprintf("conflicting fee evidence at identity %s version %d", out.FeeEvidenceID, out.FeeEvidenceVersion), true, now)
				return false
			}
		}
	}
	switch out.Outcome {
	case basketVenueUnknown:
		a.HoldReason = "the venue outcome is unknown: " + out.Detail
		if a.Status == basketActionSubmitting {
			a.Status = basketActionUnresolved
			a.EvidenceJSON = evidence
		}
		a.UpdatedAtMs = now.UnixMilli()
		rt.touchPairProgress()
		bc.persistPair(rt)
		return false
	case pairVenueNeverSent:
		return bc.finishPairNoFill(rt, a, "proven never sent: "+out.Detail, evidence, in)
	case pairVenueAbsent:
		if a.ConfirmedQty == 0 && out.CoverageComplete && !out.ContraryEvidence && pairAbsenceReleasable(now.UnixMilli(), a.AcceptDeadlineMs, out.CoverageThroughMs) {
			return bc.finishPairNoFill(rt, a, "absent after the acceptance deadline plus grace with complete matching history", evidence, in)
		}
		a.HoldReason = "the order is not visible yet, but absence cannot release it before the deadline plus grace with complete history and no contrary evidence"
		if a.Status == basketActionSubmitting {
			a.Status = basketActionUnresolved
		}
		a.UpdatedAtMs = now.UnixMilli()
		rt.touchPairProgress()
		bc.persistPair(rt)
		return false
	case basketVenueRejected:
		if out.CumQty == 0 && a.ConfirmedQty == 0 {
			return bc.finishPairNoFill(rt, a, "proven terminal zero-fill rejection: "+out.Detail, evidence, in)
		}
		out.Terminal = true
	}
	terminal := out.Terminal || out.Outcome == basketVenueFilled || out.Outcome == basketVenueRejected
	if live && !bc.acceptPairLiveEvidence(rt, a, out, terminal || a.Terminal, now) {
		return false
	}
	a.ConfirmedQty = out.CumQty
	a.ConfirmedNotional = out.CumNotional
	if a.ConfirmedQty > 0 {
		a.ConfirmedAvgPx = a.ConfirmedNotional / a.ConfirmedQty
	}
	if terminal {
		a.Terminal = true
	}
	a.EvidenceJSON = evidence
	a.HoldReason = ""
	a.UpdatedAtMs = now.UnixMilli()
	if a.ConfirmedQty > a.AppliedQty+1e-12 || bc.pairFeeDeltaDue(a) {
		a.Status = basketActionSettling
	} else if bc.pairActionComplete(a) {
		a.Status = basketActionCommitted
	} else {
		a.Status = basketActionUnresolved
	}
	rt.touchPairProgress()
	if !bc.persistPair(rt) {
		return false
	}
	if a.Status == basketActionSettling {
		if !bc.settlePairAction(rt, a, in, inputs) {
			return false
		}
	}
	if out.Outcome == basketVenueRejected && pairElectivePurpose(a.Purpose) && a.Direction == basketDirectionIncrease {
		bc.pairOpenRecovery(rt, fmt.Sprintf("the %s leg for %s was rejected after a partial fill (%s)", a.Purpose, a.StrategyID, out.Detail), now)
	}
	return !basketActionPending(a.Status)
}

func (bc *basketCoordinator) pairActionComplete(a *basketAction) bool {
	if !a.Terminal || a.ConfirmedQty-a.AppliedQty > 1e-12 {
		return false
	}
	if bc.pairLiveShaped(a) && a.ConfirmedQty > 0 {
		return bc.pairLiveComplete(a)
	}
	return true
}

func (bc *basketCoordinator) pairFeeDeltaDue(a *basketAction) bool {
	return bc.pairLiveShaped(a) && bc.pairLiveFeeDue(a)
}

func (bc *basketCoordinator) pairHasSettlementObligation(rt *basketRuntime, a *basketAction) bool {
	for _, o := range rt.Pair.Progress.Obligations {
		if o.MemberID == a.StrategyID && o.ActionID == a.ActionID && pairSettlementObligationKind(o.Kind) {
			return true
		}
	}
	return false
}

func (bc *basketCoordinator) pairLiveShaped(a *basketAction) bool {
	sc, ok := bc.members[a.StrategyID]
	if !ok {
		return false
	}
	v, err := bc.venueFor(sc)
	return err == nil && v.LiveShaped()
}

func (bc *basketCoordinator) finishPairNoFill(rt *basketRuntime, a *basketAction, reason, evidence string, in basketCycleInputs) bool {
	now := in.Now
	a.Status = basketActionNoFill
	a.Terminal = true
	a.HoldReason = reason
	if evidence != "" {
		a.EvidenceJSON = evidence
	}
	a.UpdatedAtMs = now.UnixMilli()
	bc.releasePairEntry(rt, a, reason, now)
	p := &rt.Pair.Progress
	switch {
	case pairProtectivePurpose(a.Purpose):
		r := p.Retries[a.StrategyID]
		backoff := rt.Pair.Limits.RetryInitialMs
		for i := 0; i < r.Count && backoff < rt.Pair.Limits.RetryMaxMs; i++ {
			backoff *= 2
		}
		if backoff > rt.Pair.Limits.RetryMaxMs {
			backoff = rt.Pair.Limits.RetryMaxMs
		}
		at, ok := pairAddMs(now.UnixMilli(), backoff)
		if !ok {
			p.addObligation(pairObligation{Kind: "retry_overflow", MemberID: a.StrategyID, Detail: "the retry time overflowed; the reduction waits for an operator", AtMs: now.UnixMilli()})
			at = math.MaxInt64
		}
		if p.Retries == nil {
			p.Retries = map[string]pairRetry{}
		}
		p.Retries[a.StrategyID] = pairRetry{Count: r.Count + 1, RetryAtMs: at}
		bc.pairLogf(rt, "protective reduction %s for %s ended without a fill (%s); a new identity may retry at %s", a.ActionID, a.StrategyID, reason, formatClosedBarMs(at))
	case a.Purpose == pairPurposeEntry:
		if bc.pairExposureTotal(rt) == 0 && len(rt.pairUnresolvedActions()) == 0 {
			if rb := rt.Rebalances[a.DeadlineMs]; rb != nil && !basketRebalanceTerminal(rb.Status) {
				rb.Status = basketRebalanceCompleted
				rb.Outcome.Result = "completed_with_deviations"
				rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{StrategyID: a.StrategyID, Kind: "first_leg_rejected", Detail: reason, AtMs: now.UnixMilli()})
				rb.Outcome.CompletedAtMs = now.UnixMilli()
				rb.UpdatedAtMs = now.UnixMilli()
			}
			p.Lifecycle = pairLifecycleFlat
			p.OriginSlotMs = 0
			bc.pairLogf(rt, "the first leg %s ended with no fill (%s); the entry intent ends and the pair stays flat", a.ActionID, reason)
		} else {
			bc.pairOpenRecovery(rt, fmt.Sprintf("the hedge leg %s for %s ended with no fill (%s)", a.ActionID, a.StrategyID, reason), now)
		}
	case a.Purpose == pairPurposeExit:
		bc.pairLogf(rt, "exit tranche %s for %s ended with no fill (%s); a later step plans the next legal exit tranche", a.ActionID, a.StrategyID, reason)
	}
	rt.touchPairProgress()
	return bc.persistPair(rt)
}

func (bc *basketCoordinator) pairOpenRecovery(rt *basketRuntime, reason string, now time.Time) {
	p := &rt.Pair.Progress
	p.IncreasesFrozen = true
	if p.FreezeReason == "" {
		p.FreezeReason = reason
	}
	if p.Lifecycle != pairLifecycleOperatorHeld {
		p.Lifecycle = pairLifecycleRecovery
	}
	if p.RecoveryStartMs == 0 {
		p.RecoveryStartMs = now.UnixMilli()
	}
	if p.RecoveryReason == "" {
		p.RecoveryReason = reason
	}
	rt.touchPairProgress()
	bc.pairAlert(rt, fmt.Sprintf("recovery|%d", p.RecoveryStartMs), fmt.Sprintf("pair %s recovery: %s; increases are frozen and the surviving exposure reduces toward zero under reduce-only recovery", rt.Contract.BasketID, reason), true)
}

func (bc *basketCoordinator) pairExposure(rt *basketRuntime, id string) float64 {
	m, ok := rt.Contract.member(id)
	if !ok {
		return 0
	}
	e := basketPositionSigned(bc.state.Strategies[id], m.Coin)
	for _, a := range rt.Actions {
		if a.StrategyID != id {
			continue
		}
		if d := a.ConfirmedQty - a.AppliedQty; d > 1e-12 {
			e += a.signedDelta() * d
		}
	}
	return e
}

func (bc *basketCoordinator) pairExposureTotal(rt *basketRuntime) float64 {
	var total float64
	for _, id := range rt.memberIDs() {
		total += math.Abs(bc.pairExposure(rt, id))
	}
	return total
}

func (bc *basketCoordinator) pairObservation(rt *basketRuntime, id string, a *basketAction) basketMemberObservation {
	if obs, ok := rt.observations[id]; ok {
		return obs
	}
	return basketActionObservation(a)
}

func (bc *basketCoordinator) settlePairAction(rt *basketRuntime, a *basketAction, in basketCycleInputs, inputs pairStepInputs) bool {
	now := in.Now
	sc := bc.members[a.StrategyID]
	s := bc.state.Strategies[a.StrategyID]
	m, _ := rt.Contract.pairMember(a.StrategyID)
	if s == nil {
		return false
	}
	if rt.needsDurable {
		bc.pairLogf(rt, "action %s settlement waits: the previous pair settlement is not durable yet", a.ActionID)
		return false
	}
	p := &rt.Pair.Progress
	remaining := a.ConfirmedQty - a.AppliedQty
	if bc.pairLiveShaped(a) {
		return bc.settlePairActionLive(rt, a, in)
	}
	if remaining <= 1e-12 {
		if bc.pairActionComplete(a) {
			a.Status = basketActionCommitted
		} else {
			a.Status = basketActionUnresolved
		}
		rt.touchPairProgress()
		return bc.persistPair(rt)
	}
	val, valOK := inputs.view(a.StrategyID)
	settleConv := 1.0
	why := "the coordinator has published no valuation view for this member, so no " + m.Meta.PriceCurrency + " conversion is checked"
	if valOK {
		c, cwhy := val.conversionAt(m.Meta.PriceCurrency, now.UnixMilli())
		if cwhy == "" {
			settleConv = c
		} else {
			valOK, why = false, cwhy
		}
	}
	if !valOK {
		if p.addObligation(pairObligation{Kind: pairObligationSettlementConversion, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: "settlement holds: " + why, AtMs: now.UnixMilli()}) {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		return false
	}
	p.clearObligation(pairObligationSettlementConversion, a.StrategyID, a.ActionID)
	logger := bc.memberLogger(a.StrategyID)
	defer logger.Close()
	dn := a.ConfirmedNotional - a.AppliedNotional
	px := dn / remaining * settleConv
	if !basketFinitePositive(px) {
		bc.holdPairAction(rt, a, fmt.Sprintf("the incremental fill price %v is not finite and positive", px), true, now)
		return false
	}
	obs := bc.pairObservation(rt, a.StrategyID, a)
	from := len(s.TradeHistory)
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	var booked basketBooking
	fillCtx := bc.pairFillContextFor(rt, a, remaining, now.UnixMilli())
	withStrategyBookClock(s, now, func() {
		withPairFillContext(s, fillCtx, func() {
			booked = bookPairFill(sc, s, bc.cfg, a, remaining, px, pairConfirmedFee{}, obs, logger)
		})
	})
	s.deferTradePersist = prevDefer
	if !(booked.Qty > 0) {
		reason := booked.Hold
		if reason == "" {
			reason = "the booking owner recorded no fill"
		}
		bc.holdPairAction(rt, a, "a confirmed fill could not be booked: "+reason, true, now)
		return false
	}
	a.AppliedQty += booked.Qty
	a.AppliedNotional += dn * booked.Qty / remaining
	a.AppliedFee += booked.Fee
	trades := append([]Trade(nil), s.TradeHistory[from:]...)
	bookedRows := a.bookedTrades()
	for _, t := range trades {
		if t.Symbol != a.Symbol || t.TradeType == TradeTypeFunding {
			continue
		}
		row := pairBookedTrade{TradeTsMs: t.Timestamp.UTC().UnixMilli(), TradeTs: formatTime(t.Timestamp), Qty: t.Quantity, Px: t.Price, FeeUSD: t.ExchangeFee, OrderID: t.ExchangeOrderID, IsClose: t.IsClose}
		if t.IsClose && s.Positions[a.Symbol] == nil {
			row.ClosedPositionID = t.PositionID
		}
		bookedRows = append(bookedRows, row)
	}
	if b, err := json.Marshal(bookedRows); err == nil {
		a.BookedJSON = string(b)
	}
	rt.Expected[a.StrategyID] += a.signedDelta() * booked.Qty
	if math.Abs(rt.Expected[a.StrategyID]) < 1e-12 {
		rt.Expected[a.StrategyID] = 0
	}
	if a.Direction == basketDirectionIncrease && p.FirstFillMs == 0 {
		p.FirstFillMs = now.UnixMilli()
	}
	for _, u := range rt.Pair.Uses {
		if u.ActionID == a.ActionID && u.Status == pairUseReserved {
			u.Status = pairUseConsumed
			u.UpdatedAtMs = now.UnixMilli()
		}
	}
	if in := bc.intentFor(s, a); in != nil {
		in.ConfirmedQty, in.ConfirmedNotional, in.ConfirmedFee = a.ConfirmedQty, a.ConfirmedNotional, a.ConfirmedFee
		in.AppliedQty, in.AppliedNotional, in.AppliedFee = a.AppliedQty, a.AppliedNotional, a.AppliedFee
		in.SettlementSeq++
		in.VenueOrderID = a.VenueOrderID
		in.EvidenceJSON = a.EvidenceJSON
		in.HoldReason = ""
		in.UpdatedAtMs = now.UnixMilli()
		if a.Terminal && basketQtyEqual(a.AppliedQty, a.ConfirmedQty) {
			in.Status = entryIntentCommitted
		} else {
			in.Status = entryIntentSettling
		}
	}
	bc.pairLogf(rt, "action %s booked %s %.10g %s @ $%.6f (fee $%.6f)", a.ActionID, a.OrderSide, booked.Qty, a.Symbol, booked.Px, booked.Fee)
	if bc.notifier != nil && len(trades) > 0 {
		notifier, regime := bc.notifier, bc.cfg.Regime
		bc.outbox = append(bc.outbox, func() { sendTradeAlertRows(sc, trades, notifier, regime) })
	}
	if bc.pairActionComplete(a) {
		a.Status = basketActionCommitted
	} else {
		a.Status = basketActionUnresolved
	}
	a.UpdatedAtMs = now.UnixMilli()
	rt.touchPairProgress()
	bc.clearPairEndedObligations(rt)
	if err := bc.store.SavePartition(bc.state, rt.Partition); err != nil {
		rt.needsDurable = true
		bc.pairLogf(rt, "action %s booked in memory, but the %s state file did not save: %v; no elective order is sent until it does, and a restart books the confirmed delta once from the saved evidence", a.ActionID, partitionLabel(rt.Partition), err)
		return false
	}
	return true
}

func (bc *basketCoordinator) detectPairExceptions(rt *basketRuntime, in basketCycleInputs) {
	now := in.Now
	changed := false
	for _, m := range rt.Contract.Members {
		actual := basketPositionSigned(bc.state.Strategies[m.StrategyID], m.Coin)
		exp := rt.Expected[m.StrategyID]
		if basketQtyEqual(actual, exp) {
			continue
		}
		cause := ""
		switch {
		case actual == 0:
			cause = "the position closed outside the pair coordinator (stop, take-profit, kill switch, liquidation or operator close)"
		case (actual > 0) == (exp > 0) && math.Abs(actual) < math.Abs(exp):
			cause = "the position was reduced outside the pair coordinator (stop, take-profit, kill switch, liquidation or operator close)"
		default:
			cause = "the position grew or flipped outside the pair coordinator"
		}
		rt.Expected[m.StrategyID] = actual
		p := &rt.Pair.Progress
		p.Exceptions = append(p.Exceptions, basketException{StrategyID: m.StrategyID, ExpectedQty: exp, ActualQty: actual, Cause: cause, AtMs: now.UnixMilli()})
		if len(p.Exceptions) > 32 {
			p.Exceptions = p.Exceptions[len(p.Exceptions)-32:]
		}
		msg := fmt.Sprintf("%s %s: expected %.10g, book holds %.10g — %s", m.StrategyID, m.Coin, exp, actual, cause)
		bc.pairOpenRecovery(rt, "unilateral change: "+msg, now)
		bc.pairAlert(rt, fmt.Sprintf("exception|%s|%.10g|%.10g", m.StrategyID, exp, actual), msg+"; increases are frozen, the surviving leg keeps its protection and reduces toward zero, and the closed side is never reopened", true)
		changed = true
	}
	if changed {
		bc.persistPair(rt)
	}
}

func (bc *basketCoordinator) pairLifecycleChecks(rt *basketRuntime, in basketCycleInputs) {
	now := in.Now
	nowMs := now.UnixMilli()
	p := &rt.Pair.Progress
	flat := bc.pairExposureTotal(rt) == 0
	resolved := len(rt.pairUnresolvedActions()) == 0
	accounted := true
	for _, a := range rt.Actions {
		if a.ConfirmedQty-a.AppliedQty > 1e-12 {
			accounted = false
		}
	}
	for _, o := range p.Obligations {
		if pairSettlementObligationKind(o.Kind) || pairFundingHoldsAccounting(o.Kind) || pairNativeObligationKind(o.Kind) {
			accounted = false
		}
	}
	if len(p.NativeReceipts) > 0 || p.hasStagedPaperClose() || !pairNativeRecoveryAccounted(p) {
		accounted = false
	}
	if rt.Pair.funding.hasPending() {
		accounted = false
	}
	changed := false
	if p.Lifecycle == "" {
		p.Lifecycle = pairLifecycleFlat
		changed = true
	}
	if flat && p.clearObligation(pairObligationReductionMinimum, "", "") {
		changed = true
	}
	for _, id := range rt.memberIDs() {
		if bc.pairExposure(rt, id) == 0 && bc.clearPairReductionHold(rt, id) {
			changed = true
		}
	}
	originOpen := false
	if rb := rt.Rebalances[p.OriginSlotMs]; rb != nil && !basketRebalanceTerminal(rb.Status) {
		originOpen = true
	}
	switch p.Lifecycle {
	case pairLifecycleRecovery, pairLifecycleOperatorHeld, pairLifecycleExiting:
		if pm := p.PositionMatch; pm != nil && pm.Status == pairPositionMismatch {
			accounted = false
		}
		if flat && resolved && accounted {
			bc.pairLogf(rt, "the pair is flat with terminal actions and complete accounting; the %s lifecycle ends", p.Lifecycle)
			if rb := rt.Rebalances[p.OriginSlotMs]; rb != nil && !basketRebalanceTerminal(rb.Status) {
				rb.Status = basketRebalanceCompleted
				rb.Outcome.Result = "completed"
				if p.Lifecycle != pairLifecycleExiting {
					rb.Outcome.Result = "protective_exception"
				}
				rb.Outcome.CompletedAtMs = nowMs
				rb.UpdatedAtMs = nowMs
			}
			*p = pairProgress{V: pairProgressVersion, Seq: p.Seq, Lifecycle: pairLifecycleFlat, Alerts: p.Alerts, Exceptions: p.Exceptions, Retries: nil, Obligations: p.Obligations, LastBound: p.LastBound, CostEvidence: p.CostEvidence, FundingBinding: p.FundingBinding, PositionMatch: p.PositionMatch,
				NativeReceipts: p.NativeReceipts, PaperCloses: p.PaperCloses, NativeAttempts: p.NativeAttempts, NativeAttemptSeq: p.NativeAttemptSeq,
				NativeFundingHolds: p.NativeFundingHolds, NativeRequests: p.NativeRequests, RecordingBlocked: p.RecordingBlocked}
			changed = true
		}
	case pairLifecycleOpening:
		if flat && resolved && !originOpen {
			p.Lifecycle, p.OriginSlotMs, p.FirstFillMs = pairLifecycleFlat, 0, 0
			changed = true
		}
	}
	if (p.Lifecycle == pairLifecycleOpening || p.Lifecycle == pairLifecycleMatched) && p.FirstFillMs > 0 && pairReached(nowMs, p.FirstFillMs, rt.Pair.Limits.MaxHoldAgeMs) {
		p.Lifecycle = pairLifecycleExiting
		p.ExitReason = fmt.Sprintf("the maximum hold age of %d ms since the first confirmed fill at %s is reached", rt.Pair.Limits.MaxHoldAgeMs, formatClosedBarMs(p.FirstFillMs))
		if rb := rt.Rebalances[p.OriginSlotMs]; rb != nil && !basketRebalanceTerminal(rb.Status) && len(rt.pairUnresolvedActions()) == 0 {
			rb.Status = basketRebalanceCompleted
			rb.Outcome.Result = "completed_with_deviations"
			rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{Kind: "hold_age_exit", Detail: p.ExitReason, AtMs: nowMs})
			rb.Outcome.CompletedAtMs = nowMs
			rb.UpdatedAtMs = nowMs
		}
		p.OriginSlotMs = 0
		bc.pairLogf(rt, "coordinator exit: %s; the exit obligation is saved before any exit order", p.ExitReason)
		changed = true
	}
	if p.Lifecycle == pairLifecycleRecovery && p.RecoveryStartMs > 0 && pairReached(nowMs, p.RecoveryStartMs, rt.Pair.Limits.RecoveryAgeMs) {
		p.Lifecycle = pairLifecycleOperatorHeld
		p.OperatorHeldReason = fmt.Sprintf("recovery opened at %s did not finish within %d ms", formatClosedBarMs(p.RecoveryStartMs), rt.Pair.Limits.RecoveryAgeMs)
		changed = true
		rt.touchPairProgress()
		bc.persistPair(rt)
		bc.pairAlert(rt, fmt.Sprintf("operator-held|%d", p.RecoveryStartMs), fmt.Sprintf("**PAIR OPERATOR HELD** %s: %s; queries and protective reductions continue, and an operator must review the venue accounts", rt.Contract.BasketID, p.OperatorHeldReason), true)
	}
	if changed {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
}

func (bc *basketCoordinator) pairPolicyMembers(rt *basketRuntime) []pairPolicyMember {
	var out []pairPolicyMember
	for _, m := range rt.Contract.pairMembersSorted() {
		pm := pairPolicyMember{MemberID: m.MemberID, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, Meta: m.Meta,
			CostPolicy: m.CostPolicy.rates(), Source: m.Source, SourceContract: pairSourceContractFor(m.Provider, m.Source), ModeledDeliveryMs: m.ModeledDeliveryMs, Leverage: m.Leverage}
		if s := bc.state.Strategies[m.MemberID]; s != nil {
			pm.BookedBaseQty = basketPositionSigned(s, m.Symbol)
			if pos := s.Positions[m.Symbol]; pos != nil {
				pm.BookedAvgPx = pos.AvgCost
			}
		}
		out = append(out, pm)
	}
	return out
}

func (bc *basketCoordinator) decidePendingPairSlots(rt *basketRuntime, in basketCycleInputs) {
	var undecided []*basketRebalance
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		if rb.Status == basketRebalanceFrozen && rb.TargetsJSON == "" {
			undecided = append(undecided, rb)
		}
	}
	if len(undecided) == 0 {
		return
	}
	newest := undecided[len(undecided)-1]
	why, final := bc.pairDecideHold(rt, bc.pairNow(in).UnixMilli())
	if final {
		now := bc.pairNow(in)
		for _, rb := range undecided {
			t := &pairTargets{Kind: pairPolicyStatusHold, CoordinatorReason: why}
			_, saved, err := bc.freezePairTargets(rt, rb, t, why, now)
			if err != nil {
				bc.pairLogf(rt, "slot %s end hold could not be saved: %v; a later step holds it again", formatClosedBarMs(rb.DeadlineMs), err)
				continue
			}
			bc.pairLogf(rt, "slot %s held: %s", formatClosedBarMs(rb.DeadlineMs), saved)
		}
		return
	}
	if why != "" {
		if rt.pairDecideNote != why {
			rt.pairDecideNote = why
			bc.pairLogf(rt, "slot decisions hold: %s; frozen slots stay undecided, and queries, settlement and protection continue", why)
		}
		return
	}
	rt.pairDecideNote = ""
	changed := false
	for _, rb := range undecided[:len(undecided)-1] {
		rb.Status = basketRebalanceSuperseded
		rb.Outcome.Result = "superseded"
		rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{Kind: "superseded", Detail: fmt.Sprintf("the newer slot %s replaced this undecided slot", formatClosedBarMs(newest.DeadlineMs)), AtMs: in.Now.UnixMilli()})
		rb.UpdatedAtMs = in.Now.UnixMilli()
		changed = true
	}
	if changed {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
	bc.decidePairSlot(rt, newest, in)
}

func (bc *basketCoordinator) decidePairSlot(rt *basketRuntime, rb *basketRebalance, in basketCycleInputs) bool {
	now := bc.pairNow(in)
	rt.clock = now
	p := &rt.Pair.Progress
	if rb.CutoffMs <= 0 || rb.InputFP == "" || rb.EvidenceJSON == "" {
		return false
	}
	if p.Lifecycle != pairLifecycleFlat && p.Lifecycle != pairLifecycleMatched {
		rb.Status = basketRebalanceCompleted
		rb.Outcome.Result = "evidence_only"
		rb.Outcome.CompletedAtMs = now.UnixMilli()
		rb.UpdatedAtMs = now.UnixMilli()
		rt.touchPairProgress()
		bc.persistPair(rt)
		return true
	}
	hold := func(reason string, policyHold bool, t *pairTargets) bool {
		if t == nil {
			t = &pairTargets{Kind: pairPolicyStatusHold}
		}
		t.Kind = pairPolicyStatusHold
		if policyHold {
			t.PolicyReason = reason
		} else {
			t.CoordinatorReason = reason
		}
		_, saved, err := bc.freezePairTargets(rt, rb, t, reason, now)
		if err != nil {
			bc.pairLogf(rt, "slot %s hold could not be saved: %v; a later step decides it again", formatClosedBarMs(rb.DeadlineMs), err)
			return false
		}
		bc.pairLogf(rt, "slot %s held: %s", formatClosedBarMs(rb.DeadlineMs), saved)
		return true
	}
	env, why := verifyPairEnvelope(rt, rb)
	if why != "" {
		return hold("the frozen quote envelope fails verification ("+why+"); no policy runs on unverified evidence and no input is refetched", false, nil)
	}
	if bc.pairPolicyRun == nil {
		return hold("no pair policy runner is wired; the slot holds and existing positions, stops and recovery continue", false, nil)
	}
	window, why := pairPolicyWindowFor(rt, rb, env)
	if why != "" {
		return hold("the useful decision window cannot be computed: "+why, false, nil)
	}
	timeout, why := bc.pairPolicyBudgetFor(rt, rb, env, now)
	if why != "" {
		return hold("no policy runs: "+why, false, nil)
	}
	req := pairPolicyRequest{
		V:                   pairPolicyRequestVersion,
		Partition:           rt.Partition.String(),
		PairID:              rt.Contract.BasketID,
		ContractFingerprint: rt.Contract.Fingerprint(),
		SlotMs:              rb.DeadlineMs,
		CutoffMs:            rb.CutoffMs,
		InputFingerprint:    rb.InputFP,
		Policy:              rt.Contract.Pair.Policy,
		Params:              rt.Contract.Pair.Params,
		Limits:              rt.Pair.Limits,
		BookNeed:            rt.Contract.Pair.BookNeed,
		Lifecycle:           p.Lifecycle,
		Members:             bc.pairPolicyMembers(rt),
		Envelope:            json.RawMessage(rb.EvidenceJSON),
	}
	if req.Params == nil {
		req.Params = map[string]interface{}{}
	}
	stdin, err := json.Marshal(req)
	if err != nil {
		return hold("the policy request could not be encoded: "+err.Error(), false, nil)
	}
	before := bc.pairSnapshot(rt)
	runner := bc.pairPolicyRun
	parent := bc.parent
	var stdout, stderr []byte
	var runErr, ctxErr error
	bc.unlocked(func() {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		wallStart := bc.pairBudgetStart()
		stdout, stderr, runErr = runner(ctx, stdin, timeout)
		_, ctxErr = bc.pairWallBudget("policy", ctx, wallStart)
	})
	now = bc.pairNow(in)
	rt.clock = now
	if !before.same(bc.pairSnapshot(rt)) || rt.Rebalances[rb.DeadlineMs] != rb || rb.Status != basketRebalanceFrozen || rb.TargetsJSON != "" || rb.ContractFP != rt.Contract.Fingerprint() {
		bc.pairLogf(rt, "slot %s: the pair state changed while the policy ran; the result is discarded and a later step decides the slot again", formatClosedBarMs(rb.DeadlineMs))
		if ctxErr != nil {
			bc.notePairPolicyTimeout(rt, rb, ctxErr, timeout, now)
			if !bc.persistPair(rt) {
				rt.needsDurable = true
			}
		}
		return false
	}
	if len(stderr) > 0 {
		bc.pairLogf(rt, "policy stderr: %s", strings.TrimSpace(string(stderr)))
	}
	if ctxErr != nil {
		return bc.holdPairPolicyTimeout(rt, rb, ctxErr, timeout, now)
	}
	if runErr != nil {
		return hold("the policy run failed: "+runErr.Error(), false, nil)
	}
	res, derr := pairDecodePolicyResult(stdout)
	if derr != nil {
		return hold("invalid policy output: unreadable result: "+derr.Error(), false, nil)
	}
	reason, verr := validatePairPolicyResult(rt, req, res)
	if verr != nil {
		return hold("invalid policy output: "+verr.Error(), false, &pairTargets{Decision: res.Decision})
	}
	if res.Status == pairPolicyStatusHold {
		return hold(reason, true, &pairTargets{Decision: res.Decision})
	}
	if why := window.eligible(now.UnixMilli(), rt.Pair.Limits.QuoteAgeMs); why != "" {
		return hold(fmt.Sprintf("the %s result returned after its useful decision window (%s); a late result never authorizes an action", res.Status, why), false, &pairTargets{Decision: res.Decision})
	}
	rounded, evidence, rerr := bc.pairRoundTargets(rt, env, res)
	if rerr != nil {
		return hold("invalid policy output: "+rerr.Error(), false, &pairTargets{Decision: res.Decision})
	}
	t := &pairTargets{Kind: res.Status, Orientation: res.Orientation, Members: rounded, Decision: res.Decision, BoundEvidence: evidence}
	if res.Status == pairPolicyStatusEnter {
		t.FirstLeg = pairChooseFirstLeg(rt)
	}
	if why := bc.pairRoundedEconomics(rt, env, res.Status, rounded, evidence); why != "" {
		return hold("the rounded decision fails its post-rounding checks: "+why, false, &pairTargets{Decision: res.Decision})
	}
	kind, saved, err := bc.freezePairTargets(rt, rb, t, "", now)
	if err != nil {
		bc.pairLogf(rt, "slot %s decision could not be saved: %v; a later step decides it again", formatClosedBarMs(rb.DeadlineMs), err)
		return false
	}
	if kind == pairPolicyStatusHold {
		bc.pairLogf(rt, "slot %s held: %s", formatClosedBarMs(rb.DeadlineMs), saved)
		return true
	}
	bc.pairLogf(rt, "slot %s: %s decision frozen (%s, input %s)", formatClosedBarMs(rb.DeadlineMs), kind, t.Orientation, shortFingerprint(rb.InputFP))
	return true
}

func (bc *basketCoordinator) pairFullFillCapCheck(rt *basketRuntime, env pairQuoteEnvelope, targets map[string]pairPolicyMemberTarget) string {
	var gross, unmatched, maxPx float64
	venue := map[string]float64{}
	var bad []string
	for _, m := range rt.Contract.pairMembersSorted() {
		t := targets[m.MemberID]
		em, ok := env.member(m.MemberID)
		if !ok {
			return fmt.Sprintf("the verified envelope has no member %s", m.MemberID)
		}
		rates, err := pairConversionRates(env, m.MemberID)
		if err != nil {
			return err.Error()
		}
		conv, ok := rates[m.Meta.PriceCurrency]
		if !ok {
			return fmt.Sprintf("member %s has no saved %s conversion for its price bound", m.MemberID, m.Meta.PriceCurrency)
		}
		px := t.PriceBound * conv
		n := math.Abs(t.TargetBaseQty) * px
		gross += n
		venue[m.Provider] += n
		unmatched += t.TargetBaseQty
		if px > maxPx {
			maxPx = px
		}
		if why := bc.pairFullFillCollateral(rt, m, em, rates, t.TargetBaseQty, px); why != "" {
			bad = append(bad, why)
		}
	}
	l := rt.Pair.Limits
	if !basketWithin(gross, l.GrossBudgetUSD) {
		bad = append(bad, fmt.Sprintf("gross $%.2f > budget $%.2f", gross, l.GrossBudgetUSD))
	}
	for p, v := range venue {
		if !basketWithin(v, l.venueCapUSD(p)) {
			bad = append(bad, fmt.Sprintf("%s $%.2f > venue cap $%.2f", p, v, l.venueCapUSD(p)))
		}
	}
	if u := math.Abs(unmatched) * maxPx; !basketWithin(u, l.UnmatchedCapUSD) {
		bad = append(bad, fmt.Sprintf("unmatched $%.2f > cap $%.2f", u, l.UnmatchedCapUSD))
	}
	sort.Strings(bad)
	return strings.Join(bad, "; ")
}

func (bc *basketCoordinator) pairFullFillCollateral(rt *basketRuntime, m pairContractMember, em pairEnvelopeMember, rates map[string]float64, target, pxUSD float64) string {
	s := bc.state.Strategies[m.MemberID]
	if s == nil {
		return m.MemberID + " has no book for the collateral check"
	}
	collConv, ok := rates[em.Account.CollateralCurrency]
	if !ok {
		return fmt.Sprintf("%s has no saved %s conversion for its account collateral", m.MemberID, em.Account.CollateralCurrency)
	}
	eq, ok1 := pairDecimalToFloat(em.Account.Equity)
	free, ok2 := pairDecimalToFloat(em.Account.FreeCollateral)
	if !ok1 || !ok2 {
		return m.MemberID + " saved account evidence has an unreadable amount"
	}
	leg := pairLeg{Member: m, Leverage: EffectiveExchangeLeverage(bc.members[m.MemberID]), Val: pairMemberValuation{FeeRate: m.CostPolicy.TakerFeePct}}
	posted, bookedCosts, upnl := 0.0, 0.0, 0.0
	if pos := s.Positions[m.Symbol]; pos != nil && pos.Quantity > 0 {
		posted = pos.Quantity * pos.AvgCost * leg.imr()
	}
	if s.InitialCapital > s.Cash {
		bookedCosts = s.InitialCapital - s.Cash
	}
	priceConv := rates[m.Meta.PriceCurrency]
	bid, okb := pairTopPx(em.Observation.NativeBids)
	ask, oka := pairTopPx(em.Observation.NativeAsks)
	if !okb || !oka {
		return m.MemberID + " saved observation has no best bid and ask for the adverse valuation"
	}
	if target > 0 {
		upnl = target * (bid*priceConv - pxUSD)
	} else {
		upnl = -target * (pxUSD - ask*priceConv)
	}
	n := math.Abs(target) * pxUSD
	fee := n * leg.feeRate()
	required := n*leg.imr() + fee
	effFree := math.Min(s.Cash-posted, free*collConv-posted-bookedCosts)
	if effFree < required {
		return fmt.Sprintf("%s free collateral $%.2f (smaller of book and saved account evidence) is below the $%.2f margin and fee reserve of the full target", m.MemberID, effFree, required)
	}
	effEquity := math.Min(s.Cash, eq*collConv-bookedCosts) + upnl - fee
	if maint := n * m.Meta.MaintenanceMarginRate; effEquity < maint {
		return fmt.Sprintf("%s equity $%.2f is below the $%.2f maintenance margin of the full target", m.MemberID, effEquity, maint)
	}
	return ""
}

func (bc *basketCoordinator) freezePairTargets(rt *basketRuntime, rb *basketRebalance, t *pairTargets, holdReason string, now time.Time) (string, string, error) {
	t.Version = pairTargetsVersion
	t.SlotMs = rb.DeadlineMs
	t.InputFingerprint = rb.InputFP
	raw := t.JSON()
	if raw == "" {
		return "", "", fmt.Errorf("the decision record could not be encoded")
	}
	if len(raw) > pairTargetsMaxBytes {
		holdReason = fmt.Sprintf("the decision record is %d bytes, above the %d-byte limit; the slot holds without it", len(raw), pairTargetsMaxBytes)
		t = &pairTargets{Version: pairTargetsVersion, Kind: pairPolicyStatusHold, SlotMs: rb.DeadlineMs, InputFingerprint: rb.InputFP,
			CoordinatorReason: holdReason, Decision: json.RawMessage(fmt.Sprintf(`{"oversize_bytes":%d}`, len(raw)))}
		raw = t.JSON()
	}
	status := basketRebalanceFrozen
	next := *rb
	next.UpdatedAtMs = now.UnixMilli()
	if t.Kind == pairPolicyStatusHold {
		status = basketRebalanceHeld
		next.HoldReason = holdReason
		next.Outcome.Result = "held"
	}
	if err := bc.store.WritePairDecision(rt, &next, raw, status); err != nil {
		return "", "", err
	}
	rb.TargetsJSON, rb.Status, rb.durableStatus = raw, status, status
	rb.HoldReason, rb.Outcome, rb.UpdatedAtMs = next.HoldReason, next.Outcome, next.UpdatedAtMs
	if t.Kind == pairPolicyStatusEnter || t.Kind == pairPolicyStatusExit {
		changed := false
		for d, other := range rt.Rebalances {
			if d >= rb.DeadlineMs || other.Status != basketRebalanceFrozen || other.TargetsJSON == "" || len(rt.actionsFor(d)) > 0 {
				continue
			}
			other.Status = basketRebalanceSuperseded
			other.Outcome.Result = "superseded"
			other.Outcome.Deviations = append(other.Outcome.Deviations, basketDeviation{Kind: "superseded", Detail: fmt.Sprintf("the newer decision at %s replaced this decision before any action", formatClosedBarMs(rb.DeadlineMs)), AtMs: now.UnixMilli()})
			other.UpdatedAtMs = now.UnixMilli()
			changed = true
		}
		if changed {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
	}
	return t.Kind, holdReason, nil
}

func (bc *basketCoordinator) pairIncreaseHold(rt *basketRuntime, in basketCycleInputs) string {
	if why := bc.pairHostIncreaseHold(rt); why != "" {
		return why
	}
	p := rt.Pair.Progress
	if p.IncreasesFrozen {
		if p.FreezeReason != "" {
			return p.FreezeReason
		}
		return "increases are frozen for this pair"
	}
	if bc.state.partitionLatched(rt.Partition) {
		return "the " + partitionLabel(rt.Partition) + " kill switch is latched"
	}
	if bc.store.persistenceHoldsPartition(rt.Partition) {
		return "the " + partitionLabel(rt.Partition) + " scope has an unacknowledged state save failure"
	}
	if sr := in.ScopeRisk[rt.Partition]; sr != nil {
		if sr.KillSwitchFired {
			return "the " + partitionLabel(rt.Partition) + " kill switch fired this cycle"
		}
		if sr.DailyLossEntriesHeld {
			return "the daily loss limit holds entries until UTC rollover"
		}
		if sr.NotionalBlocked {
			return "the partition notional cap holds new opens"
		}
	}
	for _, id := range rt.memberIDs() {
		if from := pairNativeFundingHoldFromMs(rt, id); from > 0 {
			return fmt.Sprintf("%s has an open native funding hold from %d after an operator native decision; increases hold until an exact venue position read bounds the hold, and exits, queries and protection continue", id, from)
		}
	}
	for _, id := range rt.memberIDs() {
		sc := bc.members[id]
		if sc.Paused {
			return id + " is paused; increases hold and exits, queries and protection continue"
		}
		if s := bc.state.Strategies[id]; s != nil && s.RiskState.CircuitBreaker {
			return id + " has an active circuit breaker"
		}
		if why := rt.pairEchoFailures[id]; why != "" {
			return id + " management check failed its echo: " + why
		}
		obs, ok := rt.observations[id]
		if !ok {
			return id + " has no validated management check output yet"
		}
		if obs.Cycle < bc.cycle-1 {
			return fmt.Sprintf("%s management output is from cycle %d, older than the previous cycle %d", id, obs.Cycle, bc.cycle-1)
		}
		maxAge := pairManagementMaxAge(bc.cfg)
		if maxAge <= 0 || obs.AtMs <= 0 || in.Now.UnixMilli()-obs.AtMs > maxAge.Milliseconds() {
			return fmt.Sprintf("%s management output observed at %d is older than the maximum management age %s at %d", id, obs.AtMs, maxAge, in.Now.UnixMilli())
		}
		if indicatorsATRValue(obs.Indicators) <= 0 {
			return id + " management output carries no ATR for the native stop"
		}
	}
	if pm := rt.Pair.Progress.PositionMatch; pm != nil && pm.Status == pairPositionMismatch {
		return "the venue position does not match the pair records: " + pm.Reason
	}
	if why := bc.pairVirtualConservation(rt); why != "" {
		return "virtual inventory conservation: " + why
	}
	if ms, _, err := bc.pairLiveMembers(rt); err != nil {
		return err.Error()
	} else if len(ms) > 0 {
		if st := rt.pairPositions; st == nil || st.auth == nil {
			why := "no venue position read has matched the pair records since startup"
			if pm := rt.Pair.Progress.PositionMatch; pm != nil && pm.Reason != "" {
				why = "the last venue position read did not authorize increases: " + pm.Reason
			}
			return why
		}
	}
	return ""
}

func (bc *basketCoordinator) pairPlanFor(rt *basketRuntime, inputs pairStepInputs) *pairPlan {
	plan := &pairPlan{Limits: rt.Pair.Limits, Legs: map[string]*pairLeg{}, NowMs: inputs.nowMs, Views: inputs.views}
	for _, m := range rt.Contract.pairMembersSorted() {
		sc := bc.members[m.MemberID]
		s := bc.state.Strategies[m.MemberID]
		leg := &pairLeg{Member: m, Leverage: EffectiveExchangeLeverage(sc)}
		if s != nil {
			leg.Booked = basketPositionSigned(s, m.Symbol)
			leg.Cash = s.Cash
			if pos := s.Positions[m.Symbol]; pos != nil && pos.Quantity > 0 {
				leg.AvgCostUSD = pos.AvgCost
				leg.Posted = pos.Quantity * pos.AvgCost * leg.imr()
			}
			if s.InitialCapital > s.Cash {
				leg.BookedCosts = s.InitialCapital - s.Cash
			}
		}
		v, ok := inputs.view(m.MemberID)
		switch {
		case !ok:
			leg.ValWhy = "the coordinator has published no valuation view for this member"
			leg.AccountWhy = leg.ValWhy
		default:
			leg.Val = v
			if rate, why := v.conversionAt(m.Meta.PriceCurrency, inputs.nowMs); why == "" {
				leg.PriceRate = rate
			}
			if lo, hi, okb, why := v.usdBoundsAt(inputs.nowMs); okb && basketFinitePositive(leg.PriceRate) {
				leg.ValOK = true
				leg.LowerUSD, leg.UpperUSD = lo, hi
				leg.MarkUSD, _, _ = v.usdMarkAt(inputs.nowMs)
			} else {
				leg.ValWhy = why
			}
			if acct, why := v.accountAt(inputs.nowMs); why == "" {
				leg.Account = acct
			} else {
				leg.AccountWhy = why
			}
		}
		plan.Legs[m.MemberID] = leg
		plan.Order = append(plan.Order, m.MemberID)
	}
	for _, a := range rt.pairUnresolvedActions() {
		leg := plan.Legs[a.StrategyID]
		if leg == nil {
			continue
		}
		if d := a.ConfirmedQty - a.AppliedQty; d > 1e-12 {
			leg.Unapplied += a.signedDelta() * d
			px := a.ConfirmedAvgPx
			if dn := a.ConfirmedNotional - a.AppliedNotional; dn > 0 {
				px = dn / d
			}
			leg.UnappliedFills = append(leg.UnappliedFills, pairPendingFill{ActionID: a.ActionID, MemberID: a.StrategyID, Signed: a.signedDelta() * d, PriceBound: px, Elective: a.Direction == basketDirectionIncrease})
		}
		if rem := a.RequestedQty - a.ConfirmedQty; rem > 1e-12 {
			plan.Pending = append(plan.Pending, pairPendingFill{ActionID: a.ActionID, MemberID: a.StrategyID, Signed: a.signedDelta() * rem, PriceBound: bc.pairRiskBound(rt, a), Elective: pairElectivePurpose(a.Purpose)})
		}
	}
	return plan
}

func (bc *basketCoordinator) recordPairBound(rt *basketRuntime, proof pairEndpointProof, assumptions []string, now time.Time) {
	readings := map[string]float64{"gross_usd": proof.Worst.Gross, "unmatched_usd": proof.Worst.Unmatched}
	for k, v := range proof.Worst.Venue {
		readings[k+"_usd"] = v
	}
	rt.Pair.Progress.LastBound = &pairBoundRecord{AtMs: now.UnixMilli(), Assumptions: assumptions, Readings: readings, Breaches: proof.Breaches}
	rt.touchPairProgress()
}

type pairOrigin struct {
	slot    *basketRebalance
	targets *pairTargets
	legacy  bool
}

func (bc *basketCoordinator) pairOriginSlot(rt *basketRuntime) (pairOrigin, error) {
	ds := rt.sortedDeadlines()
	for i := len(ds) - 1; i >= 0; i-- {
		rb := rt.Rebalances[ds[i]]
		if rb.Status != basketRebalanceFrozen && rb.Status != basketRebalanceExecuting {
			continue
		}
		t, err := parsePairTargets(rb.TargetsJSON)
		if err != nil {
			return pairOrigin{slot: rb}, err
		}
		if !t.actionable() {
			continue
		}
		if t.legacy() {
			return pairOrigin{slot: rb, targets: t, legacy: true}, nil
		}
		if err := pairCheckSavedTargets(rt, t); err != nil {
			return pairOrigin{slot: rb}, err
		}
		return pairOrigin{slot: rb, targets: t}, nil
	}
	return pairOrigin{}, nil
}

func (bc *basketCoordinator) pairFreshEvent(rt *basketRuntime, memberID string, origin *basketRebalance, allowOrigin bool) (pairQuoteEvent, bool) {
	used := map[string]bool{}
	for _, u := range rt.Pair.Uses {
		if u.StrategyID == memberID {
			used[u.EventID] = true
		}
	}
	ds := rt.sortedDeadlines()
	for i := len(ds) - 1; i >= 0; i-- {
		rb := rt.Rebalances[ds[i]]
		if origin != nil && (rb.DeadlineMs < origin.DeadlineMs || (rb.DeadlineMs == origin.DeadlineMs && !allowOrigin)) {
			continue
		}
		ev, ok := pairQuoteEventFromSlot(rb)
		if !ok || ev.ContractFingerprint != rt.Contract.Fingerprint() || used[ev.ID()] {
			continue
		}
		return ev, true
	}
	return pairQuoteEvent{}, false
}

func (bc *basketCoordinator) pairDispatchFresh(rt *basketRuntime, inputs pairStepInputs, now time.Time, increase bool) string {
	nowMs := now.UnixMilli()
	for _, id := range rt.memberIDs() {
		v, ok := inputs.view(id)
		if !ok {
			return id + " valuation is unavailable: the coordinator has published no valuation view for this member"
		}
		why := ""
		if increase {
			why = v.increaseReasonAt(nowMs)
		} else if _, valid, w := v.usdMarkAt(nowMs); !valid {
			why = w
		}
		if why != "" {
			return id + " valuation is unavailable: " + why
		}
	}
	return ""
}

func (bc *basketCoordinator) noteUnavailableBounds(rt *basketRuntime, why string, now time.Time) {
	if rt.Pair.Progress.addObligation(pairObligation{Kind: "bounds_unavailable", Detail: why, AtMs: now.UnixMilli()}) {
		rt.touchPairProgress()
		bc.persistPair(rt)
		bc.pairLogf(rt, "entries hold: %s; every exit, query, protection and recovery obligation continues", why)
	}
}

func (bc *basketCoordinator) executePair(rt *basketRuntime, in basketCycleInputs, inputs pairStepInputs) {
	if bc.state.partitionLatched(rt.Partition) {
		return
	}
	p := &rt.Pair.Progress
	if p.Lifecycle == pairLifecycleRecovery || p.Lifecycle == pairLifecycleOperatorHeld {
		return
	}
	entryNow := in.Now
	for n := 0; n < pairMaxActionsPerStep; n++ {
		if rt.pairPendingElective() != nil || rt.needsDurable {
			return
		}
		in = bc.refreshPairInputs(rt, in)
		now := in.Now
		origin, err := bc.pairOriginSlot(rt)
		if err != nil {
			bc.pairAlert(rt, "targets-unreadable|"+fmt.Sprint(origin.slot.DeadlineMs), fmt.Sprintf("slot %s decision is unreadable (%v); no elective order runs", formatClosedBarMs(origin.slot.DeadlineMs), err), true)
			return
		}
		if origin.legacy {
			bc.holdPairLegacyOrigin(rt, origin.slot, now)
			return
		}
		exiting := p.Lifecycle == pairLifecycleExiting
		if origin.slot == nil && !exiting {
			return
		}
		if origin.slot != nil && origin.targets.Kind == pairPolicyStatusExit && p.Lifecycle != pairLifecycleMatched && p.Lifecycle != pairLifecycleExiting {
			bc.endPairOrigin(rt, origin.slot, "held", "an exit decision needs a matched pair", now)
			return
		}
		if origin.slot != nil && origin.targets.Kind == pairPolicyStatusEnter && p.Lifecycle != pairLifecycleFlat && p.Lifecycle != pairLifecycleOpening {
			bc.endPairOrigin(rt, origin.slot, "held", "an entry decision needs a flat or opening pair, the lifecycle is "+p.Lifecycle, now)
			return
		}
		freshAt := now
		if exiting || origin.targets.Kind == pairPolicyStatusExit {
			freshAt = entryNow
		}
		if why := bc.pairDispatchFresh(rt, inputs, freshAt, !(exiting || origin.targets.Kind == pairPolicyStatusExit)); why != "" {
			if origin.slot != nil && len(rt.actionsFor(origin.slot.DeadlineMs)) == 0 {
				bc.endPairOrigin(rt, origin.slot, "held", "the frozen decision's quotes failed the freshness check at its first dispatch: "+why, now)
				return
			}
			bc.noteUnavailableBounds(rt, why, now)
			return
		}
		if rt.Pair.Progress.clearObligation("bounds_unavailable", "", "") {
			rt.touchPairProgress()
		}
		plan := bc.pairPlanFor(rt, inputs.at(freshAt))
		var acted bool
		if exiting || origin.targets.Kind == pairPolicyStatusExit {
			acted = bc.executePairExit(rt, origin, plan, in, inputs)
		} else {
			acted = bc.executePairEntry(rt, origin, plan, in, inputs)
		}
		if !acted {
			return
		}
	}
}

func (bc *basketCoordinator) holdPairLegacyOrigin(rt *basketRuntime, rb *basketRebalance, now time.Time) {
	reason := fmt.Sprintf("slot %s holds a version-1 decision record without saved bound evidence; it authorizes no further order, and saved actions keep their queries and settlement", formatClosedBarMs(rb.DeadlineMs))
	if len(rt.pairUnresolvedActions()) == 0 && bc.pairExposureTotal(rt) == 0 {
		bc.endPairOrigin(rt, rb, "held", reason, now)
		return
	}
	if rt.Pair.Progress.Lifecycle == pairLifecycleRecovery || rt.Pair.Progress.Lifecycle == pairLifecycleOperatorHeld {
		return
	}
	bc.pairOpenRecovery(rt, reason, now)
	bc.persistPair(rt)
}

func (bc *basketCoordinator) endPairOrigin(rt *basketRuntime, rb *basketRebalance, result, reason string, now time.Time) {
	if result == "held" {
		rb.Status = basketRebalanceHeld
		rb.HoldReason = reason
	} else {
		rb.Status = basketRebalanceCompleted
		rb.Outcome.CompletedAtMs = now.UnixMilli()
	}
	rb.Outcome.Result = result
	if reason != "" && result != "held" {
		rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{Kind: result, Detail: reason, AtMs: now.UnixMilli()})
	}
	rb.UpdatedAtMs = now.UnixMilli()
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairLogf(rt, "slot %s %s: %s", formatClosedBarMs(rb.DeadlineMs), rb.Status, reason)
}

func pairSideFor(direction string, signedTarget float64, current float64) (orderSide, positionSide string) {
	if direction == basketDirectionIncrease {
		if signedTarget > 0 {
			return "buy", entrySideLong
		}
		return "sell", entrySideShort
	}
	if current > 0 {
		return "sell", entrySideLong
	}
	return "buy", entrySideShort
}

func (bc *basketCoordinator) executePairEntry(rt *basketRuntime, origin pairOrigin, plan *pairPlan, in basketCycleInputs, inputs pairStepInputs) bool {
	now := in.Now
	t := origin.targets
	first := t.FirstLeg
	if first == "" {
		first = pairChooseFirstLeg(rt)
	}
	other := ""
	for _, id := range plan.Order {
		if id != first {
			other = id
		}
	}
	fLeg, oLeg := plan.Legs[first], plan.Legs[other]
	if fLeg == nil || oLeg == nil {
		return false
	}
	ef, eo := fLeg.exposure(), oLeg.exposure()
	tf, to := t.Members[first].TargetBaseQty, t.Members[other].TargetBaseQty
	lotF, lotO := pairLotBase(fLeg.Member), pairLotBase(oLeg.Member)
	u := ef + eo
	remF, remO := tf-ef, to-eo
	var c *pairCandidate
	switch {
	case math.Abs(u) >= lotO*(1-1e-9) && math.Abs(remO) >= lotO*(1-1e-9) && (u > 0) != (to > 0):
		qty := math.Min(math.Abs(u), math.Abs(remO))
		os, ps := pairSideFor(basketDirectionIncrease, to, eo)
		c = &pairCandidate{MemberID: other, Member: oLeg.Member, Purpose: pairPurposeEntry, Direction: basketDirectionIncrease, OrderSide: os, PositionSide: ps, FullQty: qty, PriceBound: t.Members[other].PriceBound, Current: eo, Target: to, ProposedBound: t.BoundEvidence[other].ProposedBound}
	case math.Abs(remF) >= lotF*(1-1e-9) && (remF > 0) == (tf > 0):
		os, ps := pairSideFor(basketDirectionIncrease, tf, ef)
		c = &pairCandidate{MemberID: first, Member: fLeg.Member, Purpose: pairPurposeEntry, Direction: basketDirectionIncrease, OrderSide: os, PositionSide: ps, FullQty: math.Abs(remF), PriceBound: t.Members[first].PriceBound, Current: ef, Target: tf, ProposedBound: t.BoundEvidence[first].ProposedBound}
	default:
		if len(rt.pairUnresolvedActions()) > 0 {
			return false
		}
		var devs []string
		if math.Abs(remF) > 1e-12 {
			devs = append(devs, fmt.Sprintf("%s holds %.10g of %.10g (below one lot)", first, ef, tf))
		}
		if math.Abs(remO) > 1e-12 {
			devs = append(devs, fmt.Sprintf("%s holds %.10g of %.10g (below one lot)", other, eo, to))
		}
		result := "completed"
		if len(devs) > 0 {
			result = "completed_with_deviations"
		}
		if math.Abs(ef) == 0 && math.Abs(eo) == 0 {
			bc.endPairOrigin(rt, origin.slot, "completed_with_deviations", "no legal lot reaches either target", now)
			rt.Pair.Progress.Lifecycle = pairLifecycleFlat
			rt.Pair.Progress.OriginSlotMs = 0
			rt.touchPairProgress()
			bc.persistPair(rt)
			return false
		}
		bc.endPairOrigin(rt, origin.slot, result, strings.Join(devs, "; "), now)
		rt.Pair.Progress.Lifecycle = pairLifecycleMatched
		rt.touchPairProgress()
		bc.persistPair(rt)
		bc.pairLogf(rt, "entry reached its frozen targets; the pair is matched")
		return false
	}
	if why := bc.pairIncreaseHold(rt, in); why != "" {
		bc.pairLogf(rt, "increases hold: %s", why)
		return false
	}
	allowOrigin := c.MemberID == first && math.Abs(fLeg.exposure()) == 0
	ev, ok := bc.pairFreshEvent(rt, c.MemberID, origin.slot, allowOrigin)
	if !ok {
		bc.pairLogf(rt, "%s increase waits for a fresh quote event the member has not consumed", c.MemberID)
		return false
	}
	bind, why := bc.pairBindIncrease(rt, c.MemberID, ev, now)
	if why != "" {
		if len(rt.actionsFor(origin.slot.DeadlineMs)) == 0 {
			bc.endPairOrigin(rt, origin.slot, "held", "the selected quote event cannot authorize the first dispatch: "+why, now)
			return false
		}
		bc.pairLogf(rt, "%s increase holds: %s", c.MemberID, why)
		return false
	}
	if c.MemberID == other {
		confirmed := math.Abs(fLeg.exposure())
		if c.FullQty > confirmed+1e-12 {
			c.FullQty = confirmed
		}
	}
	qty, proof, why := plan.legalTranche(*c)
	if why != "" {
		bc.pairLogf(rt, "no legal entry tranche: %s", why)
		bc.noteDeviationPair(rt, origin.slot, "no_legal_tranche", why, now)
		return false
	}
	if gate := bc.pairPartitionGate(rt, *c, qty, plan); gate != "" {
		bc.pairLogf(rt, "%s increase holds: %s", c.MemberID, gate)
		return false
	}
	exact, err := pairExactOrderQty(c.Member, qty)
	why = ""
	if err != nil {
		why = "the tranche has no exact venue quantity: " + err.Error()
	} else {
		why = bc.pairDispatchBookCheck(rt, c.Member, ev, c.OrderSide, c.PriceBound, exact.Base)
	}
	if why != "" {
		switch {
		case len(rt.actionsFor(origin.slot.DeadlineMs)) == 0:
			bc.endPairOrigin(rt, origin.slot, "held", fmt.Sprintf("the first leg %s cannot dispatch: %s", c.MemberID, why), now)
		case c.MemberID == other:
			bc.pairOpenRecovery(rt, fmt.Sprintf("the %s hedge cannot dispatch after the first-leg fill: %s", c.MemberID, why), now)
			bc.persistPair(rt)
		default:
			bc.pairLogf(rt, "%s increase holds: %s", c.MemberID, why)
		}
		return false
	}
	bc.recordPairBound(rt, proof, []string{"independent no-fill and full-fill endpoints for every unresolved request", "each member's own best ask from its checked reference, converted with its price currency", "saved request price bounds widen the upper valuation", "free collateral is the smaller of book and account evidence, posted margin counted once"}, now)
	return bc.runPairAction(rt, origin.slot, *c, qty, ev, bind, in)
}

func (bc *basketCoordinator) pairPartitionGate(rt *basketRuntime, c pairCandidate, qty float64, plan *pairPlan) string {
	pr := partitionRiskConfig(bc.cfg, rt.Partition)
	if pr == nil {
		return ""
	}
	leg := plan.Legs[c.MemberID]
	if leg == nil || !leg.ValOK || !basketFinitePositive(leg.MarkUSD) {
		return "no own valuation for the partition gates"
	}
	mark := leg.MarkUSD
	pm := newPairMarks(plan.NowMs, plan.Views)
	states := filterStatesByPartition(bc.state.Strategies, bc.cfg.Strategies, rt.Partition)
	cfgs := strategiesInPartition(bc.cfg.Strategies, rt.Partition)
	if pr.MaxNotionalUSD > 0 {
		notional, st := portfolioNotionalMarked(states, nil, pm)
		if !st.complete() {
			return "the partition notional is incomplete (" + st.label() + ")"
		}
		if total := notional + qty*mark; total > pr.MaxNotionalUSD {
			return fmt.Sprintf("the partition notional would reach $%.2f, above the $%.2f cap", total, pr.MaxNotionalUSD)
		}
	}
	if !exposureCapConfigured(pr) {
		return ""
	}
	var pv float64
	var pvStatus pairValueStatus
	for _, st := range states {
		v, vs := displayStrategyValueMarked(st, nil, pm)
		pv += v
		pvStatus.merge(vs)
	}
	if !pvStatus.complete() {
		return "the partition value is incomplete (" + pvStatus.label() + ")"
	}
	s := bc.state.Strategies[c.MemberID]
	if s == nil {
		return "no member state for the partition gates"
	}
	hyp := make(map[string]*StrategyState, len(states))
	for id, st := range states {
		hyp[id] = st
	}
	clone := *s
	clone.Positions = make(map[string]*Position, len(s.Positions))
	for k, p := range s.Positions {
		clone.Positions[k] = p
	}
	if p := s.Positions[c.Member.Symbol]; p != nil && p.Quantity > 0 {
		cp := *p
		cp.Quantity += qty
		clone.Positions[c.Member.Symbol] = &cp
	} else {
		clone.Positions[c.Member.Symbol] = &Position{Symbol: c.Member.Symbol, Quantity: qty, AvgCost: mark, Side: c.PositionSide, Multiplier: 1}
	}
	hyp[c.MemberID] = &clone
	before := evaluateExposureCapMarked(pr, states, cfgs, nil, pv, pm)
	after := evaluateExposureCapMarked(pr, hyp, cfgs, nil, pv, pm)
	if after.ValuationIncomplete != "" {
		return "the partition exposure is incomplete (" + after.ValuationIncomplete + ")"
	}
	if c.PositionSide == entrySideLong && after.LongBlocked && after.LongUSD > before.LongUSD {
		return fmt.Sprintf("the tranche would raise same-direction long exposure to $%.2f above the $%.2f cap", after.LongUSD, after.CapUSD)
	}
	if c.PositionSide == entrySideShort && after.ShortBlocked && after.ShortUSD > before.ShortUSD {
		return fmt.Sprintf("the tranche would raise same-direction short exposure to $%.2f above the $%.2f cap", after.ShortUSD, after.CapUSD)
	}
	if stat, ok := after.OverConcentrated[c.Member.Symbol]; ok && stat.Direction == c.PositionSide {
		if prev, was := before.OverConcentrated[c.Member.Symbol]; !was || prev.Direction != stat.Direction || stat.Pct > prev.Pct {
			return fmt.Sprintf("the tranche would leave %s at %.1f%% of portfolio value (cap %.1f%%)", c.Member.Symbol, stat.Pct, after.ConcentrationPct)
		}
	}
	return ""
}

func (bc *basketCoordinator) noteDeviationPair(rt *basketRuntime, rb *basketRebalance, kind, detail string, now time.Time) {
	if rb == nil {
		return
	}
	for _, d := range rb.Outcome.Deviations {
		if d.Kind == kind && d.Detail == detail {
			return
		}
	}
	rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{Kind: kind, Detail: detail, AtMs: now.UnixMilli()})
	rb.UpdatedAtMs = now.UnixMilli()
	rt.touchPairProgress()
	bc.persistPair(rt)
}

func (bc *basketCoordinator) executePairExit(rt *basketRuntime, origin pairOrigin, plan *pairPlan, in basketCycleInputs, inputs pairStepInputs) bool {
	now := in.Now
	nowMs := now.UnixMilli()
	slot := origin.slot
	if slot == nil {
		slot = rt.latest()
	}
	var slotMs int64
	if slot != nil {
		slotMs = slot.DeadlineMs
	}
	var cands []pairCandidate
	for _, id := range plan.Order {
		leg := plan.Legs[id]
		e := leg.exposure()
		if e == 0 {
			continue
		}
		os, ps := pairSideFor(basketDirectionReduce, 0, e)
		c := pairCandidate{MemberID: id, Member: leg.Member, Purpose: pairPurposeExit, Direction: basketDirectionReduce, OrderSide: os, PositionSide: ps, FullQty: math.Abs(e), Current: e, Plan: plan}
		ref, why := bc.pairReductionReference(rt, leg.Member, os, nowMs)
		if why != "" {
			bc.holdPairReduction(rt, c, why, now)
			return false
		}
		c.Ref = ref
		if ev, ok, policy := pairPolicyExitEvidence(rt, slotMs, id); policy {
			if !ok || ev.Side != os {
				bc.noteUnavailableBounds(rt, fmt.Sprintf("%s exit bound evidence (%s, present %v) is not a saved %s bound", id, ev.Side, ok, os), now)
				return false
			}
			c.PriceBound, c.ProposedBound = ev.RoundedBound, ev.ProposedBound
			if _, px, err := pairReferencePx(ref.Ref, os); err != nil || !pairBoundCrossesPx(os, c.PriceBound, px) {
				bc.holdPairReduction(rt, c, fmt.Sprintf("the saved policy %s bound %v does not cross the current reference level %s", os, c.PriceBound, ref.Px), now)
				return false
			}
		} else {
			bound, why := pairReductionBound(leg.Member, os, ref.Ref, nowMs, rt.Pair.Limits)
			if why != "" {
				bc.holdPairReduction(rt, c, why, now)
				return false
			}
			c.PriceBound, c.ProposedBound = bound, bound
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		if rt.Pair.Progress.clearObligation(pairObligationReductionMinimum, "", "") {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		if len(rt.pairUnresolvedActions()) == 0 && origin.slot != nil && !basketRebalanceTerminal(origin.slot.Status) {
			bc.endPairOrigin(rt, origin.slot, "completed", "", now)
		}
		return false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ni := cands[i].FullQty * plan.Legs[cands[i].MemberID].MarkUSD
		nj := cands[j].FullQty * plan.Legs[cands[j].MemberID].MarkUSD
		if ni != nj {
			return ni > nj
		}
		return cands[i].MemberID < cands[j].MemberID
	})
	var reasons []string
	minimum := false
	for _, c := range cands {
		qty, proof, why := plan.legalTranche(c)
		if why != "" {
			reasons = append(reasons, why)
			minimum = minimum || strings.Contains(why, "below the venue minimum")
			continue
		}
		bc.recordPairBound(rt, proof, []string{"elective exit tranches stay within each cap or do not worsen an existing breach at every reachable endpoint"}, now)
		rt.Pair.Progress.clearObligation(pairObligationReductionMinimum, "", "")
		if origin.slot != nil && rt.Pair.Progress.Lifecycle == pairLifecycleMatched {
			rt.Pair.Progress.Lifecycle = pairLifecycleExiting
			rt.Pair.Progress.OriginSlotMs = origin.slot.DeadlineMs
			rt.touchPairProgress()
			if !bc.persistPair(rt) {
				return false
			}
		}
		return bc.runPairAction(rt, slot, c, qty, pairQuoteEvent{}, nil, in)
	}
	bc.pairLogf(rt, "no legal exit tranche this step: %s; protection stays active", strings.Join(reasons, "; "))
	bc.noteDeviationPair(rt, origin.slot, "no_legal_reduction", strings.Join(reasons, "; "), now)
	if minimum {
		detail := "a nonzero exit remainder is below the venue minimum after rounding and stays unreduced by the coordinator: " + strings.Join(reasons, "; ")
		if rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationReductionMinimum, Detail: detail, AtMs: now.UnixMilli()}) {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		bc.pairAlert(rt, "reduction_minimum|"+strings.Join(reasons, "|"), fmt.Sprintf("%s: %s; native protection stays active and an operator must review the remainder", rt.Contract.BasketID, detail), true)
	}
	return false
}

func (bc *basketCoordinator) executePairProtective(rt *basketRuntime, in basketCycleInputs, inputs pairStepInputs) {
	p := &rt.Pair.Progress
	if p.Lifecycle != pairLifecycleRecovery && p.Lifecycle != pairLifecycleOperatorHeld {
		return
	}
	if bc.state.partitionLatched(rt.Partition) {
		return
	}
	for _, id := range rt.memberIDs() {
		in = bc.refreshPairInputs(rt, in)
		now := in.Now
		e := bc.pairExposure(rt, id)
		if e == 0 || rt.pairPendingProtective(id) != nil {
			continue
		}
		if pending := rt.pairPendingElective(); pending != nil && pending.StrategyID == id && pending.Direction == basketDirectionReduce {
			continue
		}
		if r, ok := p.Retries[id]; ok && now.UnixMilli() < r.RetryAtMs {
			continue
		}
		m, _ := rt.Contract.pairMember(id)
		os, ps := pairSideFor(basketDirectionReduce, 0, e)
		c := pairCandidate{MemberID: id, Member: m, Purpose: pairPurposeProtective, Direction: basketDirectionReduce, OrderSide: os, PositionSide: ps, FullQty: math.Abs(e), Current: e}
		ref, why := bc.pairReductionReference(rt, m, os, now.UnixMilli())
		if why != "" {
			bc.holdPairReduction(rt, c, why, now)
			continue
		}
		c.Ref = ref
		bound, why := pairReductionBound(m, os, ref.Ref, now.UnixMilli(), rt.Pair.Limits)
		if why != "" {
			bc.holdPairReduction(rt, c, why, now)
			continue
		}
		c.PriceBound, c.ProposedBound = bound, bound
		slot := rt.Rebalances[p.OriginSlotMs]
		if slot == nil {
			slot = rt.latest()
		}
		bc.runPairAction(rt, slot, c, math.Abs(e), pairQuoteEvent{}, nil, in)
	}
}

func (bc *basketCoordinator) runPairAction(rt *basketRuntime, origin *basketRebalance, c pairCandidate, qty float64, ev pairQuoteEvent, bind *pairIncreaseBinding, in basketCycleInputs) bool {
	in = bc.refreshPairInputs(rt, in)
	now := in.Now
	nowMs := now.UnixMilli()
	sc, ok := bc.members[c.MemberID]
	if !ok {
		return false
	}
	v, err := bc.pairVenueFor(sc)
	if err != nil {
		bc.pairAlert(rt, "venue|"+err.Error(), err.Error(), true)
		return false
	}
	if v.AccountKey(rt.Partition) != c.Member.AccountKey {
		bc.pairAlert(rt, "account|"+c.MemberID, fmt.Sprintf("%s venue binds account %s, the contract names %s; nothing is sent", c.MemberID, v.AccountKey(rt.Partition), c.Member.AccountKey), true)
		return false
	}
	if c.Direction == basketDirectionIncrease && (!ev.valid() || bind == nil) {
		return false
	}
	live := v.LiveShaped()
	reduction := c.Direction == basketDirectionReduce
	if reduction {
		if why := bc.pairReductionStillValid(rt, c, nowMs); why != "" {
			bc.holdPairReduction(rt, c, why, now)
			return false
		}
	}
	var releaseSymbol func()
	defer func() {
		if releaseSymbol != nil {
			releaseSymbol()
		}
	}()
	if reduction && live && c.Member.Provider == entryProviderHyperliquid {
		sized, release, why := bc.pairSizeHLReduction(rt, c, qty, v, &in)
		now, nowMs = in.Now, in.Now.UnixMilli()
		if why != "" {
			bc.holdPairReduction(rt, c, why, now)
			return false
		}
		releaseSymbol, qty = release, sized
		if why := bc.pairReductionStillValid(rt, c, nowMs); why != "" {
			bc.holdPairReduction(rt, c, why, now)
			return false
		}
	}
	exact, err := pairExactOrderQty(c.Member, qty)
	if err != nil {
		if live || c.Direction == basketDirectionIncrease {
			bc.pairLogf(rt, "%s %s %.10g held before reservation: %v", c.MemberID, c.Purpose, qty, err)
			return false
		}
		exact = pairOrderQty{Native: pairNativeQty(c.Member, qty), Base: qty}
	}
	qty = exact.Base
	if live {
		if err := pairBoundOnGrid(c.Member, c.OrderSide, c.PriceBound); err != nil {
			bc.noteUnavailableBounds(rt, fmt.Sprintf("%s %s holds before reservation: the live-shaped transport needs a finite positive bound on the venue grid: %v", c.MemberID, c.Purpose, err), now)
			return false
		}
	}
	deadline, ok := pairAddMs(nowMs, rt.Pair.Limits.AcceptanceDeadlineMs)
	if !ok {
		rt.Pair.Progress.addObligation(pairObligation{Kind: "deadline_overflow", MemberID: c.MemberID, Detail: "the acceptance deadline overflows; nothing is sent", AtMs: nowMs})
		rt.touchPairProgress()
		bc.persistPair(rt)
		return false
	}
	cloid, err := v.NewClientOrderID()
	if err != nil {
		bc.pairLogf(rt, "could not generate a client order id: %v", err)
		return false
	}
	s := bc.state.Strategies[c.MemberID]
	if s == nil {
		return false
	}
	slotMs := nowMs
	if origin != nil {
		slotMs = origin.DeadlineMs
	}
	val := 0.0
	pre := basketPositionSigned(s, c.Member.Symbol)
	native := exact.Native
	a := &basketAction{
		GroupMode:        pairGroupMode,
		Partition:        rt.Partition,
		BasketID:         rt.Contract.BasketID,
		DeadlineMs:       slotMs,
		StrategyID:       c.MemberID,
		Symbol:           c.Member.Symbol,
		Direction:        c.Direction,
		OrderSide:        c.OrderSide,
		PositionSide:     c.PositionSide,
		Status:           basketActionReserved,
		RequestedQty:     qty,
		PreQty:           pre,
		MarkPx:           val,
		Provider:         v.Provider(),
		AccountKey:       c.Member.AccountKey,
		ClientOrderID:    cloid,
		Purpose:          c.Purpose,
		Instrument:       c.Member.Instrument,
		NativeQty:        native,
		ContractSize:     c.Member.Meta.ContractSize,
		PriceBound:       c.PriceBound,
		AcceptDeadlineMs: deadline,
		CreatedAtMs:      nowMs,
		UpdatedAtMs:      nowMs,
	}
	if c.Direction == basketDirectionIncrease {
		a.QuoteEventID = ev.ID()
	}
	req := pairActionRequest{V: pairActionRequestVersion, PairID: a.BasketID, Partition: rt.Partition.String(), MemberID: a.StrategyID, Provider: a.Provider,
		AccountKey: a.AccountKey, Instrument: a.Instrument, ClientOrderID: cloid, Purpose: a.Purpose, OrderSide: a.OrderSide, PositionSide: a.PositionSide,
		BaseQty: qty, NativeQty: native, ContractSize: a.ContractSize, PriceBound: a.PriceBound, AcceptDeadlineMs: deadline, QuoteEventID: a.QuoteEventID,
		OriginSlotMs: slotMs, ReduceOnly: c.Direction == basketDirectionReduce, ContractFingerprint: rt.Contract.Fingerprint()}
	if c.Direction == basketDirectionIncrease {
		req.Conversions, req.CapabilityHash, req.EvidenceHash = bind.Conversions, bind.CapabilityHash, bind.EvidenceHash
	}
	if reduction {
		ref := pairCloneObservation(c.Ref.Ref)
		req.Reference, req.ReferenceMaxAgeMs, req.ReferenceReceiptMs, req.ReferencePx = &ref, rt.Pair.Limits.QuoteAgeMs, ref.FirstReceiptMs, c.Ref.Px
	}
	a.RequestJSON = marshalEntryJSON(req)
	var intent *entryIntent
	var use *pairQuoteEventUse
	if c.Direction == basketDirectionIncrease {
		g := s.EntryGuard
		if g == nil {
			bc.pairLogf(rt, "%s increase held: the durable entry record is unavailable", c.MemberID)
			return false
		}
		if prev := g.unresolvedIntent(); prev != nil {
			bc.pairLogf(rt, "%s increase held: entry %s is still unresolved", c.MemberID, prev.IntentID)
			return false
		}
		id, err := newEntryIntentID()
		if err != nil {
			return false
		}
		signal := 1
		if c.PositionSide == entrySideShort {
			signal = -1
		}
		intent = &entryIntent{
			IntentID:           id,
			Partition:          rt.Partition,
			StrategyID:         c.MemberID,
			Symbol:             c.Member.Symbol,
			TargetSide:         c.PositionSide,
			DecisionBoundaryMs: ev.SlotMs,
			CutoffMs:           ev.CutoffMs,
			Provider:           a.Provider,
			AccountKey:         a.AccountKey,
			InstrumentType:     sc.Type,
			ClientOrderID:      cloid,
			Status:             entryIntentReserved,
			CreatedAtMs:        nowMs,
			UpdatedAtMs:        nowMs,
			DecisionKind:       entryDecisionKindQuote,
			QuoteEventID:       ev.ID(),
			OwnerMode:          pairGroupMode,
			OwnerGroupID:       rt.Contract.BasketID,
		}
		intent.RequestJSON = marshalEntryJSON(entryIntentRequest{Version: entryIntentRequestVersion, Kind: "pair:" + rt.Contract.BasketID + ":" + ev.ID(), Signal: signal, PrevPosQty: math.Abs(pre), Price: c.PriceBound, Size: qty, Script: sc.Script})
		use = &pairQuoteEventUse{Partition: rt.Partition, PairID: rt.Contract.BasketID, StrategyID: c.MemberID, Event: ev, EventID: ev.ID(), Status: pairUseReserved, CreatedAtMs: nowMs, UpdatedAtMs: nowMs}
		a.EntryIntentID = intent.IntentID
	}
	staleIncrease := func() bool {
		in = bc.refreshPairInputs(rt, in)
		if c.Direction != basketDirectionIncrease {
			return false
		}
		why := bc.pairIncreaseStillFresh(rt, c.MemberID, ev, bind, in.Now)
		if why == "" {
			return false
		}
		if origin != nil && len(rt.actionsFor(origin.DeadlineMs)) == 0 {
			bc.endPairOrigin(rt, origin, "held", "the selected quote evidence expired before reservation: "+why, in.Now)
			return true
		}
		bc.pairLogf(rt, "%s increase holds before reservation: %s", c.MemberID, why)
		return true
	}
	if staleIncrease() {
		return false
	}
	p := &rt.Pair.Progress
	if c.Purpose == pairPurposeEntry && p.Lifecycle == pairLifecycleFlat {
		p.Lifecycle = pairLifecycleOpening
		p.OriginSlotMs = slotMs
		rt.touchPairProgress()
		if !bc.persistPair(rt) {
			return false
		}
	}
	if staleIncrease() {
		return false
	}
	if live && c.Direction == basketDirectionIncrease {
		if why := bc.pairPositionAuthorized(rt, "", in.Now.UnixMilli()); why != "" {
			var rwhy string
			in, rwhy = bc.pairReadPositions(rt, in, "", false)
			if rwhy == "" {
				why = bc.pairPositionAuthorized(rt, "", in.Now.UnixMilli())
			} else {
				why = rwhy
			}
			if why == "" && staleIncrease() {
				return false
			}
		}
		if why := bc.pairPositionAuthorized(rt, "", in.Now.UnixMilli()); why != "" {
			bc.pairLogf(rt, "%s increase holds before reservation: the venue position barrier: %s", c.MemberID, why)
			return false
		}
	}
	if reduction {
		in = bc.refreshPairInputs(rt, in)
		if why := bc.pairReductionStillValid(rt, c, in.Now.UnixMilli()); why != "" {
			bc.holdPairReduction(rt, c, "before reservation: "+why, in.Now)
			return false
		}
	}
	if err := bc.store.ReservePairAction(rt, a, intent, use); err != nil {
		bc.pairLogf(rt, "%s %s %.10g held: the action could not be reserved before sending: %v", c.MemberID, c.Purpose, qty, err)
		return false
	}
	rt.Actions[a.ActionID] = a
	rt.bumpPairMutationEpoch()
	if reduction {
		bc.clearPairReductionHold(rt, c.MemberID)
	}
	if intent != nil {
		s.EntryGuard.Intents[intent.IntentID] = intent
		rt.Pair.Intents[intent.IntentID] = intent
	}
	if use != nil {
		rt.Pair.Uses[use.key()] = use
	}
	in = bc.refreshPairInputs(rt, in)
	if err := bc.store.MarkPairActionSubmitting(rt, a, intent); err != nil {
		bc.finishPairNoFill(rt, a, "the submission could not be recorded, so nothing was sent: "+err.Error(), "", in)
		return false
	}
	bc.pairLogf(rt, "%s %s %s %s %.10g base (%.10g native) bound $%.6f (action %s, client %s, deadline %s)", c.MemberID, c.Purpose, c.OrderSide, c.Member.Instrument, qty, native, c.PriceBound, a.ActionID, cloid, formatClosedBarMs(deadline))
	in = bc.refreshPairInputs(rt, in)
	if !pairBeforeDeadline(in.Now.UnixMilli(), a.AcceptDeadlineMs) {
		return bc.finishPairNoFill(rt, a, "the acceptance deadline passed before transport, so nothing was sent", "", in)
	}
	if reduction {
		if why := bc.pairReductionStillValid(rt, c, in.Now.UnixMilli()); why != "" {
			return bc.finishPairNoFill(rt, a, "the saved reduction reference failed its check after reservation, so nothing was sent: "+why, "", in)
		}
	}
	if c.Direction == basketDirectionIncrease {
		if why := bc.pairIncreaseStillFresh(rt, c.MemberID, ev, bind, in.Now); why != "" {
			return bc.finishPairNoFill(rt, a, "the selected quote evidence expired after reservation, so nothing was sent: "+why, "", in)
		}
		if live {
			if why := bc.pairPositionAuthorized(rt, a.ActionID, in.Now.UnixMilli()); why != "" {
				return bc.finishPairNoFill(rt, a, "the venue position barrier no longer authorizes the increase after reservation, so nothing was sent: "+why, "", in)
			}
		}
	}
	proof := bc.pairTransportProofFor(rt, a, bind, c.Direction == basketDirectionIncrease)
	if c.Direction == basketDirectionIncrease {
		proof.book = bc.pairDispatchBookCheck(rt, c.Member, ev, c.OrderSide, a.PriceBound, a.RequestedQty)
		if live && rt.pairPositions != nil && rt.pairPositions.auth != nil {
			proof.positionLimitMs = rt.pairPositions.auth.LimitMs
		} else if live {
			proof.positionLimitMs = -1
		}
	}
	clock, floor := bc.pairClock, in.Now
	if clock == nil {
		clock = tradingNow
	}
	ptr := a
	submitted := *a
	var out basketVenueOutcome
	var serr error
	skipped := ""
	bc.notePairVirtualInventory(rt, v, c.MemberID)
	if releaseSymbol != nil {
		releaseSymbol()
		releaseSymbol = nil
	}
	bc.unlocked(func() {
		t := clock()
		if t.Before(floor) {
			t = floor
		}
		if skipped = proof.refusal(t.UnixMilli()); skipped != "" {
			return
		}
		if c.Direction == basketDirectionIncrease {
			if skipped = bc.pairHostIncreaseHoldUnlocked(rt); skipped != "" {
				return
			}
		}
		if h := bc.pairReplay; h != nil && h.beforeSubmit != nil {
			if serr = h.beforeSubmit(submitted); serr != nil {
				return
			}
		}
		out, serr = v.Submit(&submitted)
		if h := bc.pairReplay; h != nil && h.submitted != nil {
			h.submitted(submitted, out, serr)
		}
	})
	if rt.Actions[a.ActionID] != ptr || ptr.Status != basketActionSubmitting {
		bc.pairLogf(rt, "action %s changed while it was submitted; the submit answer is discarded and the next cycle queries it by its saved identity", a.ActionID)
		return false
	}
	in = bc.refreshPairInputs(rt, in)
	if skipped != "" {
		return bc.finishPairNoFill(rt, a, "the transport boundary refused to send, so nothing was sent: "+skipped, "", in)
	}
	if serr != nil {
		out = basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "transport failure: " + serr.Error()}
	}
	if why := pairVirtualOutcomeFuture(v, out, in.Now.UnixMilli()); why != "" {
		bc.holdPairAction(rt, a, "malformed or inconsistent venue evidence: "+why, true, in.Now)
		return false
	}
	inputs := bc.pairValuationsLocked(rt)
	return bc.applyPairOutcome(rt, a, out, in, inputs)
}

func (bc *basketCoordinator) pairIncreaseStillFresh(rt *basketRuntime, memberID string, ev pairQuoteEvent, bind *pairIncreaseBinding, now time.Time) string {
	if bind == nil || !ev.valid() {
		return "the increase has no selected quote event binding"
	}
	again, why := bc.pairBindIncrease(rt, memberID, ev, now)
	if why != "" {
		return why
	}
	if again.EvidenceHash != bind.EvidenceHash || again.CapabilityHash != bind.CapabilityHash || again.ReceiptMs != bind.ReceiptMs {
		return "the selected quote event no longer binds the same saved envelope"
	}
	return bc.pairDispatchFresh(rt, bc.pairValuationsLocked(rt), now, true)
}

type pairTransportProof struct {
	deadlineMs int64
	increase   bool
	receiptMs  int64
	quoteAgeMs int64
	valuations []pairValuationProof

	requestDeadlineMs int64
	requestReadable   bool
	book              string
	positionLimitMs   int64

	reduction   bool
	requestJSON string
}

type pairValuationProof struct {
	memberID string
	limitMs  int64
}

func (bc *basketCoordinator) pairTransportProofFor(rt *basketRuntime, a *basketAction, bind *pairIncreaseBinding, increase bool) pairTransportProof {
	pr := pairTransportProof{deadlineMs: a.AcceptDeadlineMs, increase: increase, quoteAgeMs: rt.Pair.Limits.QuoteAgeMs}
	var req pairActionRequest
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err == nil {
		pr.requestDeadlineMs, pr.requestReadable = req.AcceptDeadlineMs, true
	}
	if !increase {
		pr.reduction = a.Direction == basketDirectionReduce
		pr.requestJSON = a.RequestJSON
		return pr
	}
	if bind != nil {
		pr.receiptMs = bind.ReceiptMs
	}
	inputs := bc.pairValuationsLocked(rt)
	for _, id := range rt.memberIDs() {
		v, ok := inputs.view(id)
		limit := int64(0)
		if ok && v.CapabilityValid && v.CollateralValid && v.ConversionValid {
			limit = v.evidenceLimitMs()
		}
		pr.valuations = append(pr.valuations, pairValuationProof{memberID: id, limitMs: limit})
	}
	return pr
}

func (pr pairTransportProof) refusal(nowMs int64) string {
	if !pairBeforeDeadline(nowMs, pr.deadlineMs) {
		return fmt.Sprintf("the transport time %d is at or after the acceptance deadline %d", nowMs, pr.deadlineMs)
	}
	if !pr.requestReadable || pr.requestDeadlineMs != pr.deadlineMs {
		return fmt.Sprintf("the saved request deadline %d the child checks is not the action deadline %d", pr.requestDeadlineMs, pr.deadlineMs)
	}
	if !pr.increase {
		if pr.reduction {
			return pairReductionAgeRefusal(pr.requestJSON, nowMs)
		}
		return ""
	}
	if pr.book != "" {
		return pr.book
	}
	if pr.positionLimitMs < 0 || (pr.positionLimitMs > 0 && nowMs > pr.positionLimitMs) {
		return fmt.Sprintf("the matching venue position read is valid through %d, before the transport time %d", pr.positionLimitMs, nowMs)
	}
	if pr.receiptMs <= 0 || !pairAgeWithin(nowMs, pr.receiptMs, pr.quoteAgeMs) {
		return fmt.Sprintf("the selected quote received at %d is outside quote_age_ms %d of the transport time %d", pr.receiptMs, pr.quoteAgeMs, nowMs)
	}
	for _, v := range pr.valuations {
		if v.limitMs <= 0 || nowMs > v.limitMs {
			return fmt.Sprintf("%s valuation evidence (mark, conversion, capability and account) is valid only through %d, before the transport time %d", v.memberID, v.limitMs, nowMs)
		}
	}
	return ""
}

func (bc *basketCoordinator) pairValuationsLocked(rt *basketRuntime) pairStepInputs {
	return pairStepInputs{views: rt.pairView.clone(), nowMs: rt.clock.UnixMilli()}
}
