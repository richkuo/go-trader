package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	nativeCloseEvidenceVersion = 1

	pairObligationNativeUnknown     = "native_fill_unknown"
	pairObligationNativeUnconfirmed = "native_fill_unconfirmed"
	pairObligationNativeReceipt     = "native_receipt_unattached"
	pairObligationNativeConflict    = "native_receipt_conflict"
	pairObligationNativeAmbiguous   = "native_receipt_ambiguous"
	pairObligationNativeLegacy      = "native_legacy_unverified"
	pairObligationEmergencyClose    = "emergency_close_pending"

	pairNativeTriggerKillSwitch = "kill_switch"
	pairNativeTriggerCircuit    = "circuit_breaker"

	PlatformPendingClosePair = "pair"

	pairPaperCloseStaged    = "staged"
	pairPaperCloseCommitted = "committed"
	pairPaperCloseSource    = "pair_view"
	pairPaperCloseKeep      = 32
)

var errPairNativeDuplicate = errors.New("the native receipt fills are already attached to a saved native action")

func pairNativePurpose(p string) bool {
	return p == pairPurposeKillSwitch || p == pairPurposeOperator
}

func (a *basketAction) pairNative() bool {
	return a != nil && a.pair() && pairNativePurpose(a.Purpose)
}

func pairNativeObligationKind(kind string) bool {
	switch kind {
	case pairObligationNativeUnknown, pairObligationNativeUnconfirmed, pairObligationNativeReceipt, pairObligationNativeConflict,
		pairObligationNativeAmbiguous, pairObligationNativeLegacy, pairObligationEmergencyClose:
		return true
	}
	return false
}

type nativeCloseEvidence struct {
	V                 int              `json:"v"`
	VenueOrderIDs     []string         `json:"venue_order_ids"`
	AcceptedAtMs      int64            `json:"accepted_at_ms"`
	ExecutedNativeQty string           `json:"executed_native_qty"`
	Fills             []pairScriptFill `json:"fills"`
	Complete          bool             `json:"complete"`
	Detail            string           `json:"detail"`
}

type pairNativeResult struct {
	Out         basketVenueOutcome
	ExecutedQty float64
	Unconfirmed string
}

func (r pairNativeResult) confirmed() bool {
	return r.Out.CumQty > 0
}

func pairNativeUnknown(detail string, nowMs int64) pairNativeResult {
	return pairNativeResult{Out: basketVenueOutcome{Outcome: basketVenueUnknown, Detail: detail, ObservedAtMs: nowMs}, Unconfirmed: detail}
}

func pairNativeEvidenceResult(m pairContractMember, ev *nativeCloseEvidence, reportedFill, alreadyFlat bool, closeErr error, nowMs int64) pairNativeResult {
	errText := ""
	if closeErr != nil {
		errText = closeErr.Error()
	}
	if ev == nil {
		switch {
		case alreadyFlat && closeErr == nil:
			return pairNativeResult{Out: basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "the venue reports the position already flat", ObservedAtMs: nowMs}}
		case errText != "":
			return pairNativeUnknown("the native close returned no fill records: "+errText, nowMs)
		case reportedFill:
			return pairNativeUnknown("the native close reported a fill without venue fill records, so a requested size or order price cannot be booked", nowMs)
		}
		return pairNativeUnknown("the native close returned no fill evidence", nowMs)
	}
	if ev.V != nativeCloseEvidenceVersion {
		return pairNativeUnknown(fmt.Sprintf("the native close evidence version %d is not %d", ev.V, nativeCloseEvidenceVersion), nowMs)
	}
	csDec, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil || m.Meta.ContractSize <= 0 {
		return pairNativeUnknown(fmt.Sprintf("member %s has no admitted contract size", m.MemberID), nowMs)
	}
	cs, _ := new(big.Rat).SetString(string(csDec))
	orders := map[string]bool{}
	for _, id := range ev.VenueOrderIDs {
		if !pairPrintableASCII(id, pairEvidenceMaxIDLen) {
			return pairNativeUnknown(fmt.Sprintf("the native close names a malformed venue order id %q", id), nowMs)
		}
		orders[id] = true
	}
	executed := -1.0
	if strings.TrimSpace(ev.ExecutedNativeQty) != "" {
		q, _, err := pairScaleDecimal(ev.ExecutedNativeQty, cs, "executed native quantity", false)
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		executed = pairRatFloat(q)
	}
	sumQty, sumNotional := new(big.Rat), new(big.Rat)
	out := basketVenueOutcome{Detail: "native close fill records", Terminal: true}
	seen := map[string]bool{}
	allFees := len(ev.Fills) > 0
	var firstMs, lastMs int64
	for _, f := range ev.Fills {
		if seen[f.FillID] {
			return pairNativeUnknown(fmt.Sprintf("native fill %s appears twice", f.FillID), nowMs)
		}
		seen[f.FillID] = true
		if len(orders) > 0 && !orders[f.OrderID] {
			return pairNativeUnknown(fmt.Sprintf("native fill %s names order %q, which the close did not send", f.FillID, f.OrderID), nowMs)
		}
		q, qs, err := pairScaleDecimal(f.NativeQty, cs, "fill native quantity", true)
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		n, ns, err := pairScaleDecimal(f.NativeNotional, cs, "fill native notional", true)
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		px, err := pairCoverageRat("fill native price", f.NativePx, true)
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		rawQty, _ := pairCoverageRat("fill native quantity", f.NativeQty, true)
		if new(big.Rat).Mul(rawQty, px).Cmp(mustRat(f.NativeNotional)) != 0 {
			return pairNativeUnknown(fmt.Sprintf("native fill %s notional %s is not its size times its price", f.FillID, f.NativeNotional), nowMs)
		}
		if f.VenueTimeMs <= 0 {
			return pairNativeUnknown(fmt.Sprintf("native fill %s has no venue time", f.FillID), nowMs)
		}
		rec, err := pairConfirmedFill{FillID: f.FillID, VenueTimeMs: f.VenueTimeMs, BaseQty: qs, NativeNotional: ns}.sealed()
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		if err := pairCheckConfirmedFill(rec); err != nil {
			return pairNativeUnknown("native fill: "+err.Error(), nowMs)
		}
		sumQty.Add(sumQty, q)
		sumNotional.Add(sumNotional, n)
		out.FillCoverage = append(out.FillCoverage, rec)
		if firstMs == 0 || f.VenueTimeMs < firstMs {
			firstMs = f.VenueTimeMs
		}
		if f.VenueTimeMs > lastMs {
			lastMs = f.VenueTimeMs
		}
		out.Fills = append(out.Fills, pairVenueFill{FillID: f.FillID, BaseQty: pairRatFloat(q), EventMs: f.VenueTimeMs})
		if !f.FeeKnown {
			allFees = false
			continue
		}
		amount, err := pairNormalizeDecimal(f.FeeAmount)
		if err != nil {
			return pairNativeUnknown(fmt.Sprintf("native fill %s fee %q: %v", f.FillID, f.FeeAmount, err), nowMs)
		}
		fee, err := pairFeeFill{FillID: f.FillID, VenueTimeMs: f.VenueTimeMs, BaseQty: qs, NativeNotional: ns, FeeCurrency: strings.ToUpper(strings.TrimSpace(f.FeeCurrency)), FeeAmount: amount}.sealed()
		if err != nil {
			return pairNativeUnknown(err.Error(), nowMs)
		}
		if err := pairCheckFeeFill(fee, m); err != nil {
			return pairNativeUnknown("native fee: "+err.Error(), nowMs)
		}
		out.FeeCoverage = append(out.FeeCoverage, fee)
	}
	if len(out.FillCoverage) == 0 {
		if executed == 0 && errText == "" && ev.Complete {
			return pairNativeResult{Out: basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "the native close order executed nothing", ObservedAtMs: nowMs}, ExecutedQty: 0}
		}
		why := strings.TrimSpace(ev.Detail)
		if why == "" {
			why = "the venue returned no fill record for the close"
		}
		if errText != "" {
			why += "; " + errText
		}
		r := pairNativeUnknown("the native close has no confirmed fill record: "+why, nowMs)
		r.ExecutedQty = math.Max(executed, 0)
		return r
	}
	out.CumQty, out.CumNotional = pairRatFloat(sumQty), pairRatFloat(sumNotional)
	out.Outcome = basketVenuePartial
	out.ObservedAtMs = nowMs
	if lastMs > out.ObservedAtMs {
		out.ObservedAtMs = lastMs
	}
	for i := range out.Fills {
		out.Fills[i].VisibleMs = out.ObservedAtMs
	}
	out.AcceptedAtMs = ev.AcceptedAtMs
	if out.AcceptedAtMs <= 0 || out.AcceptedAtMs > firstMs {
		out.AcceptedAtMs = firstMs
	}
	if len(ev.VenueOrderIDs) == 1 {
		out.VenueOrderID = ev.VenueOrderIDs[0]
	}
	out.CoverageComplete = ev.Complete
	if len(out.FeeCoverage) > 0 {
		out.FeePresent = true
		out.FeeEvidenceVersion = int64(len(out.FeeCoverage))
	}
	out.FeeFinal = ev.Complete && allFees
	if errText != "" {
		out.Detail = "native close fill records returned with a transport error: " + errText
	}
	res := pairNativeResult{Out: out, ExecutedQty: out.CumQty}
	switch {
	case executed > out.CumQty && !basketQtyEqual(executed, out.CumQty):
		res.ExecutedQty = executed
		res.Unconfirmed = fmt.Sprintf("the venue reports %.10g executed, but fill records confirm %.10g; the remainder has no fill evidence and books nothing", executed, out.CumQty)
	case executed >= 0 && executed < out.CumQty && !basketQtyEqual(executed, out.CumQty):
		return pairNativeUnknown(fmt.Sprintf("the fill records confirm %.10g, above the %.10g the venue reports executed", out.CumQty, executed), nowMs)
	case !ev.Complete:
		why := strings.TrimSpace(ev.Detail)
		if why == "" {
			why = "the venue fill history is incomplete"
		}
		res.Unconfirmed = "the close fill evidence is incomplete: " + why
	}
	return res
}

func mustRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return new(big.Rat)
	}
	return r
}

type pairNativeReceipt struct {
	ID           string             `json:"id"`
	Key          string             `json:"key"`
	MemberID     string             `json:"member_id"`
	Purpose      string             `json:"purpose"`
	Trigger      string             `json:"trigger"`
	OrderSide    string             `json:"order_side"`
	PositionSide string             `json:"position_side"`
	ExecutedQty  float64            `json:"executed_qty,omitempty"`
	Unconfirmed  string             `json:"unconfirmed,omitempty"`
	Outcome      basketVenueOutcome `json:"outcome"`
	CreatedAtMs  int64              `json:"created_at_ms"`
	Hash         string             `json:"hash"`
}

func (r pairNativeReceipt) computeHash() string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	return pairSHA256(b)
}

func pairNativeReceiptID(key, purpose, trigger string, out basketVenueOutcome) string {
	ids := make([]string, 0, len(out.FillCoverage))
	for _, f := range out.FillCoverage {
		ids = append(ids, f.FillID+":"+f.EvidenceHash)
	}
	sort.Strings(ids)
	return "pnr-" + pairSHA256([]byte(strings.Join(append([]string{key, purpose, trigger, out.VenueOrderID}, ids...), "|")))[:32]
}

type pairPaperClose struct {
	MemberID      string                  `json:"member_id"`
	Key           string                  `json:"key"`
	Symbol        string                  `json:"symbol"`
	PositionID    string                  `json:"position_id"`
	Side          string                  `json:"side"`
	Qty           float64                 `json:"qty"`
	Trigger       string                  `json:"trigger"`
	CloseAtMs     int64                   `json:"close_at_ms"`
	PriceUSD      float64                 `json:"price_usd"`
	NativeMark    float64                 `json:"native_mark"`
	PriceCurrency string                  `json:"price_currency"`
	Source        string                  `json:"source"`
	Generation    uint64                  `json:"generation"`
	EvidenceHash  string                  `json:"evidence_hash"`
	Conversion    *pairConversionEvidence `json:"conversion,omitempty"`
	Status        string                  `json:"status"`
	CommittedAtMs int64                   `json:"committed_at_ms,omitempty"`
}

func (p *pairProgress) stagedPaperClose(memberID string) *pairPaperClose {
	for i := range p.PaperCloses {
		c := &p.PaperCloses[i]
		if c.MemberID == memberID && c.Status == pairPaperCloseStaged {
			return c
		}
	}
	return nil
}

func (p *pairProgress) trimPaperCloses() {
	var staged, committed []pairPaperClose
	for _, c := range p.PaperCloses {
		if c.Status == pairPaperCloseStaged {
			staged = append(staged, c)
		} else {
			committed = append(committed, c)
		}
	}
	if len(committed) > pairPaperCloseKeep {
		committed = committed[len(committed)-pairPaperCloseKeep:]
	}
	p.PaperCloses = append(committed, staged...)
}

type pairNativeCloseRouter interface {
	pairCircuitHLCloser(stratID string, inner HyperliquidLiveCloser) HyperliquidLiveCloser
	pairCircuitOKXCloser(stratID string, inner OKXLiveCloser) OKXLiveCloser
}

func pairCircuitHLCloserFor(state *AppState, stratID string, inner HyperliquidLiveCloser) HyperliquidLiveCloser {
	if state != nil {
		if r, ok := state.pairViews.(pairNativeCloseRouter); ok {
			return r.pairCircuitHLCloser(stratID, inner)
		}
	}
	return inner
}

func pairCircuitOKXCloserFor(state *AppState, stratID string, inner OKXLiveCloser) OKXLiveCloser {
	if state != nil {
		if r, ok := state.pairViews.(pairNativeCloseRouter); ok {
			return r.pairCircuitOKXCloser(stratID, inner)
		}
	}
	return inner
}

type pairNativeSend struct {
	rt           *basketRuntime
	m            pairContractMember
	a            *basketAction
	recordErr    error
	purpose      string
	trigger      string
	orderSide    string
	positionSide string
	ambiguous    string
	sentMs       int64
}

func pairNativeStartMs(a *basketAction) int64 {
	if a.SubmittedAtMs > 0 {
		return a.SubmittedAtMs
	}
	return a.CreatedAtMs
}

func (bc *basketCoordinator) pairNativeMember(provider, instrumentOrSymbol, stratID string) (*basketRuntime, pairContractMember, bool) {
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		if rt == nil || !rt.Partition.IsLive() {
			continue
		}
		for _, m := range rt.Contract.pairMembersSorted() {
			if m.Provider != provider || (stratID != "" && m.MemberID != stratID) {
				continue
			}
			if strings.EqualFold(m.Instrument, instrumentOrSymbol) || strings.EqualFold(m.Symbol, instrumentOrSymbol) {
				return rt, m, true
			}
		}
	}
	return nil, pairContractMember{}, false
}

func (bc *basketCoordinator) pairNativePeerHolders(rt *basketRuntime, m pairContractMember) []string {
	var out []string
	sc, ok := bc.members[m.MemberID]
	if !ok || bc.cfg == nil {
		return nil
	}
	for _, peer := range bc.cfg.Strategies {
		if peer.ID == m.MemberID || peer.Platform != sc.Platform || partitionFor(peer) != rt.Partition {
			continue
		}
		if basketPositionSigned(bc.state.Strategies[peer.ID], m.Symbol) != 0 {
			out = append(out, peer.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (bc *basketCoordinator) beginPairNative(provider, coin, stratID, purpose, trigger string, partialNative *float64, now time.Time) *pairNativeSend {
	if bc == nil || !bc.active() {
		return nil
	}
	rt, m, ok := bc.pairNativeMember(provider, coin, stratID)
	if !ok {
		return nil
	}
	send := &pairNativeSend{rt: rt, m: m, purpose: purpose, trigger: trigger, sentMs: now.UnixMilli()}
	release, locked := tryLockEntryAttempt(m.MemberID)
	if !locked {
		send.recordErr = fmt.Errorf("pair member %s has an entry resolution in progress, so the native %s close runs without a saved pair action", m.MemberID, trigger)
	} else {
		defer release()
	}
	if provider == entryProviderHyperliquid {
		unlock := lockHyperliquidProtectionSync(m.Symbol)
		defer unlock()
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	rt.clock = now
	if peers := bc.pairNativePeerHolders(rt, m); len(peers) > 0 {
		send.ambiguous = fmt.Sprintf("strategies %s also hold %s in the %s partition, so a whole-coin native fill cannot be attributed to pair member %s", strings.Join(peers, ", "), m.Symbol, partitionLabel(rt.Partition), m.MemberID)
	}
	superseded := false
	for _, prev := range rt.pairUnresolvedActions() {
		if prev.StrategyID == m.MemberID && prev.pairNative() && prev.Status != basketActionSettling {
			bc.endPairNativeAction(rt, prev, fmt.Sprintf("a later native %s close for this member supersedes it before its outcome was confirmed", trigger), now)
			superseded = true
		}
	}
	if superseded {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
	s := bc.state.Strategies[m.MemberID]
	e := basketPositionSigned(s, m.Symbol)
	if e != 0 {
		send.orderSide, send.positionSide = pairSideFor(basketDirectionReduce, 0, e)
	}
	if send.recordErr != nil || send.ambiguous != "" || e == 0 {
		return send
	}
	sc, ok := bc.members[m.MemberID]
	if !ok {
		send.recordErr = fmt.Errorf("pair member %s has no configuration", m.MemberID)
		return send
	}
	v, err := bc.pairVenueFor(sc)
	if err != nil {
		send.recordErr = err
		return send
	}
	cloid, err := v.NewClientOrderID()
	if err != nil {
		send.recordErr = err
		return send
	}
	qty := math.Abs(e)
	if partialNative != nil && *partialNative > 0 {
		if pq := *partialNative * m.Meta.ContractSize; pq < qty {
			qty = pq
		}
	}
	nowMs := now.UnixMilli()
	deadline, okAdd := pairAddMs(nowMs, rt.Pair.Limits.AcceptanceDeadlineMs)
	if !okAdd {
		send.recordErr = fmt.Errorf("pair %s acceptance deadline overflows", rt.Contract.BasketID)
		return send
	}
	a := bc.newPairNativeAction(rt, m, purpose, trigger, send.orderSide, send.positionSide, qty, e, cloid, deadline, nowMs, basketActionReserved)
	if err := bc.store.ReservePairAction(rt, a, nil, nil); err != nil {
		send.recordErr = fmt.Errorf("pair %s %s reduction for %s could not be reserved before transport: %w", rt.Contract.BasketID, trigger, m.MemberID, err)
		return send
	}
	rt.Actions[a.ActionID] = a
	if err := bc.store.MarkPairActionSubmitting(rt, a, nil); err != nil {
		a.Status, a.Terminal = basketActionHeld, true
		a.HoldReason = "the submission could not be recorded before the native close; the close still runs and its fill receipt is accounted separately"
		a.UpdatedAtMs = nowMs
		rt.touchPairProgress()
		bc.persistPair(rt)
		send.recordErr = fmt.Errorf("pair %s %s reduction for %s could not be marked submitting: %w", rt.Contract.BasketID, trigger, m.MemberID, err)
		return send
	}
	send.a = a
	return send
}

func (bc *basketCoordinator) newPairNativeAction(rt *basketRuntime, m pairContractMember, purpose, trigger, orderSide, positionSide string, qty, pre float64, cloid string, deadline, nowMs int64, status string) *basketAction {
	slotMs := nowMs
	if rb := rt.latest(); rb != nil {
		slotMs = rb.DeadlineMs
	}
	a := &basketAction{
		GroupMode: pairGroupMode, Partition: rt.Partition, BasketID: rt.Contract.BasketID, DeadlineMs: slotMs, StrategyID: m.MemberID,
		Symbol: m.Symbol, Direction: basketDirectionReduce, OrderSide: orderSide, PositionSide: positionSide, Status: status,
		RequestedQty: qty, PreQty: pre, Provider: m.Provider, AccountKey: m.AccountKey, ClientOrderID: cloid, Purpose: purpose,
		Instrument: m.Instrument, NativeQty: pairNativeQty(m, qty), ContractSize: m.Meta.ContractSize, AcceptDeadlineMs: deadline,
		CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
		RecoveryJSON: marshalEntryJSON(pairRecoveryNote{NativeBooked: true, Trigger: trigger, Reason: trigger + " flattening keeps native emergency authority; the coordinator books it only from confirmed fill records"}),
	}
	a.RequestJSON = marshalEntryJSON(pairActionRequest{V: pairActionRequestVersion, PairID: a.BasketID, Partition: rt.Partition.String(), MemberID: a.StrategyID, Provider: a.Provider,
		AccountKey: a.AccountKey, Instrument: a.Instrument, ClientOrderID: cloid, Purpose: purpose, OrderSide: orderSide, PositionSide: positionSide, BaseQty: qty,
		NativeQty: a.NativeQty, ContractSize: a.ContractSize, AcceptDeadlineMs: deadline, OriginSlotMs: slotMs, ReduceOnly: true,
		ContractFingerprint: rt.Contract.Fingerprint(), NativeBooked: true})
	return a
}

func (bc *basketCoordinator) endPairNativeAction(rt *basketRuntime, a *basketAction, reason string, now time.Time) {
	a.Status = basketActionHeld
	a.Terminal = true
	a.HoldReason = reason + "; its fill is unknown, so the member accounting stays open and nothing is booked from it"
	a.UpdatedAtMs = now.UnixMilli()
	if rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: a.HoldReason, AtMs: now.UnixMilli(), FromMs: pairNativeStartMs(a)}) {
		bc.pairAlert(rt, "native-unknown|"+a.ActionID, fmt.Sprintf("%s native close %s ended without a confirmed outcome (%s); the result stays incomplete until an operator reconciles the venue account", a.StrategyID, a.ActionID, reason), true)
	}
}

func (bc *basketCoordinator) resolvePairNativeAction(rt *basketRuntime, a *basketAction, now time.Time) bool {
	nowMs := now.UnixMilli()
	reason := ""
	switch {
	case basketPositionSigned(bc.state.Strategies[a.StrategyID], a.Symbol) == 0:
		reason = "the native close outcome was not confirmed and the member book is now flat"
	case pairReached(nowMs, a.AcceptDeadlineMs, entryRecoveryGrace.Milliseconds()):
		reason = "the native close outcome was not confirmed by the acceptance deadline plus grace"
	default:
		return false
	}
	bc.endPairNativeAction(rt, a, reason, now)
	rt.touchPairProgress()
	bc.pairLogf(rt, "native action %s for %s ends: %s", a.ActionID, a.StrategyID, a.HoldReason)
	return bc.persistPair(rt)
}

func (bc *basketCoordinator) finishPairNative(send *pairNativeSend, ev *nativeCloseEvidence, reportedFill, alreadyFlat bool, closeErr error, now time.Time) float64 {
	if send == nil || send.rt == nil {
		return 0
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	rt, m := send.rt, send.m
	rt.clock = now
	if send.recordErr != nil {
		msg := fmt.Sprintf("a native close for a pair member ran without a complete pair record: %v", send.recordErr)
		bc.pairOpenRecovery(rt, msg, now)
		bc.persistPair(rt)
		bc.pairAlert(rt, "native-unrecorded|"+send.recordErr.Error(), msg+"; the close was still sent, its fill receipt is kept for accounting, increases are frozen and recovery reduces the other leg", true)
	}
	res := pairNativeEvidenceResult(m, ev, reportedFill, alreadyFlat, closeErr, now.UnixMilli())
	if res.confirmed() {
		bc.pairOpenRecovery(rt, fmt.Sprintf("a native %s close for %s confirmed %.10g of the member book", send.trigger, m.MemberID, res.Out.CumQty), now)
		if send.orderSide == "" {
			send.orderSide, send.positionSide = pairNativeSideFromFills(ev)
		}
	}
	key := rt.Contract.pairMemberKey(m).String()
	if send.ambiguous != "" {
		id := pairNativeReceiptID(key, send.purpose, send.trigger, res.Out)
		if res.confirmed() || res.Out.Outcome == basketVenueUnknown {
			rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeAmbiguous, MemberID: m.MemberID, ActionID: id, Detail: send.ambiguous, AtMs: now.UnixMilli(), FromMs: send.sentMs})
			rt.touchPairProgress()
			bc.persistPair(rt)
			bc.pairAlert(rt, "native-ambiguous|"+id, fmt.Sprintf("%s: %s; nothing is booked for the pair member and the result stays incomplete", rt.Contract.BasketID, send.ambiguous), true)
		}
		return res.Out.CumQty
	}
	a := send.a
	if a != nil && rt.Actions[a.ActionID] == a && basketActionPending(a.Status) {
		bc.applyPairNativeResult(rt, a, res, now)
	} else {
		bc.retainPairNativeReceipt(rt, send, res, now)
	}
	return res.Out.CumQty
}

func pairNativeSideFromFills(ev *nativeCloseEvidence) (string, string) {
	if ev == nil {
		return "", ""
	}
	for _, f := range ev.Fills {
		switch f.Side {
		case "sell":
			return "sell", entrySideLong
		case "buy":
			return "buy", entrySideShort
		}
	}
	return "", ""
}

func (bc *basketCoordinator) applyPairNativeResult(rt *basketRuntime, a *basketAction, res pairNativeResult, now time.Time) {
	out := res.Out
	nowMs := now.UnixMilli()
	p := &rt.Pair.Progress
	switch {
	case out.Outcome == basketVenueUnknown:
		a.Status = basketActionUnresolved
		a.HoldReason = "the native close outcome is unknown: " + out.Detail
		a.EvidenceJSON = marshalEntryJSON(out)
		a.UpdatedAtMs = nowMs
		if p.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: a.HoldReason, AtMs: nowMs, FromMs: pairNativeStartMs(a)}) {
			bc.pairAlert(rt, "native-unknown|"+a.ActionID, fmt.Sprintf("%s native close %s has no confirmed fill evidence (%s); nothing is booked and the result stays incomplete", a.StrategyID, a.ActionID, out.Detail), true)
		}
		rt.touchPairProgress()
		bc.persistPair(rt)
		return
	case out.CumQty == 0:
		a.Status, a.Terminal, a.HoldReason = basketActionNoFill, true, out.Detail
		a.EvidenceJSON = marshalEntryJSON(out)
		a.UpdatedAtMs = nowMs
		rt.touchPairProgress()
		bc.persistPair(rt)
		return
	}
	if basketQtyEqual(out.CumQty, a.RequestedQty) {
		out.Outcome = basketVenueFilled
		out.CumQty = a.RequestedQty
	}
	if out.FeePresent {
		out.FeeEvidenceID = pairFeeEvidenceID(a)
	}
	if why := pairOutcomeError(a, out); why != "" {
		bc.holdPairAction(rt, a, "native close evidence is inconsistent: "+why, true, now)
		p.addObligation(pairObligation{Kind: pairObligationNativeUnconfirmed, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: "native close evidence is inconsistent: " + why, AtMs: nowMs, FromMs: pairNativeStartMs(a)})
		rt.touchPairProgress()
		bc.persistPair(rt)
		return
	}
	if why := bc.pairNativeFillConflict(rt, a, out.FillCoverage); why != "" {
		bc.holdPairAction(rt, a, why, true, now)
		p.addObligation(pairObligation{Kind: pairObligationNativeConflict, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: why, AtMs: nowMs})
		rt.touchPairProgress()
		bc.persistPair(rt)
		return
	}
	if !bc.acceptPairLiveEvidence(rt, a, out, true, now) {
		return
	}
	a.ConfirmedQty, a.ConfirmedNotional = out.CumQty, out.CumNotional
	a.ConfirmedAvgPx = a.ConfirmedNotional / a.ConfirmedQty
	a.Terminal = true
	if out.VenueOrderID != "" {
		a.VenueOrderID = out.VenueOrderID
	}
	a.EvidenceJSON = marshalEntryJSON(out)
	a.HoldReason = ""
	a.UpdatedAtMs = nowMs
	a.Status = basketActionSettling
	if res.Unconfirmed != "" {
		p.addObligation(pairObligation{Kind: pairObligationNativeUnconfirmed, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: res.Unconfirmed, AtMs: nowMs, FromMs: pairNativeStartMs(a)})
		bc.pairAlert(rt, "native-unconfirmed|"+a.ActionID, fmt.Sprintf("%s native close %s: %s; the confirmed part books once and the result stays incomplete", a.StrategyID, a.ActionID, res.Unconfirmed), true)
	}
	rt.touchPairProgress()
	bc.persistPair(rt)
}

func (bc *basketCoordinator) pairNativeFillConflict(rt *basketRuntime, a *basketAction, fills []pairConfirmedFill) string {
	if len(fills) == 0 {
		return ""
	}
	want := map[string]string{}
	for _, f := range fills {
		want[f.FillID] = f.EvidenceHash
	}
	for _, id := range rt.sortedActionIDs() {
		o := rt.Actions[id]
		if o == a || o.StrategyID != a.StrategyID || !o.pairNative() {
			continue
		}
		prev, err := pairParseFills(o.FillCoverageJSON)
		if err != nil {
			return fmt.Sprintf("native action %s holds unreadable fill records: %v", o.ActionID, err)
		}
		for _, f := range prev {
			if _, ok := want[f.FillID]; ok {
				return fmt.Sprintf("native fill %s is already attached to native action %s; a fill is accounted once", f.FillID, o.ActionID)
			}
		}
	}
	return ""
}

func (bc *basketCoordinator) retainPairNativeReceipt(rt *basketRuntime, send *pairNativeSend, res pairNativeResult, now time.Time) {
	nowMs := now.UnixMilli()
	p := &rt.Pair.Progress
	m := send.m
	key := rt.Contract.pairMemberKey(m).String()
	id := pairNativeReceiptID(key, send.purpose, send.trigger, res.Out)
	switch {
	case res.Out.Outcome == basketVenueUnknown:
		if p.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: m.MemberID, ActionID: id, Detail: "a native close ran without a saved action and returned no confirmed fill: " + res.Out.Detail, AtMs: nowMs, FromMs: send.sentMs}) {
			rt.touchPairProgress()
			bc.pairAlert(rt, "native-unknown|"+id, fmt.Sprintf("%s native %s close ran without a saved action and its fill is unknown (%s); the result stays incomplete", m.MemberID, send.trigger, res.Out.Detail), true)
		}
		bc.persistPair(rt)
		return
	case !res.confirmed():
		bc.pairLogf(rt, "native %s close for %s ran without a saved action and confirmed no fill (%s)", send.trigger, m.MemberID, res.Out.Detail)
		return
	}
	for _, r := range p.NativeReceipts {
		if r.ID == id {
			return
		}
	}
	r := pairNativeReceipt{ID: id, Key: key, MemberID: m.MemberID, Purpose: send.purpose, Trigger: send.trigger, OrderSide: send.orderSide, PositionSide: send.positionSide,
		ExecutedQty: res.ExecutedQty, Unconfirmed: res.Unconfirmed, Outcome: res.Out, CreatedAtMs: nowMs}
	r.Hash = r.computeHash()
	p.NativeReceipts = append(p.NativeReceipts, r)
	p.addObligation(pairObligation{Kind: pairObligationNativeReceipt, MemberID: m.MemberID, ActionID: id,
		Detail: fmt.Sprintf("a confirmed native %s fill of %.10g has no saved action yet; the coordinator attaches and books it without another order", send.trigger, res.Out.CumQty), AtMs: nowMs})
	rt.touchPairProgress()
	if !bc.persistPair(rt) {
		bc.pairAlert(rt, "native-receipt-memory|"+id, fmt.Sprintf("%s: the confirmed native fill receipt %s for %s is held in process memory only; it is not durable, the coordinator retries its save every step and a process loss leaves the result incomplete", rt.Contract.BasketID, id, m.MemberID), true)
	}
}

func (bc *basketCoordinator) attachPairNativeReceipts(rt *basketRuntime, in basketCycleInputs) {
	if in.SaveBlocked || len(rt.Pair.Progress.NativeReceipts) == 0 {
		return
	}
	now := in.Now
	nowMs := now.UnixMilli()
	receipts := append([]pairNativeReceipt(nil), rt.Pair.Progress.NativeReceipts...)
	for _, r := range receipts {
		p := &rt.Pair.Progress
		m, ok := rt.Contract.pairMember(r.MemberID)
		if !ok || r.Hash != r.computeHash() || r.Key != rt.Contract.pairMemberKey(m).String() || !pairNativePurpose(r.Purpose) || r.OrderSide == "" {
			if p.addObligation(pairObligation{Kind: pairObligationNativeConflict, MemberID: r.MemberID, ActionID: r.ID, Detail: "the saved native receipt does not match its seal, member key, purpose or side; it is never booked", AtMs: nowMs}) {
				rt.touchPairProgress()
				bc.persistPair(rt)
			}
			continue
		}
		if p.obligation(pairObligationNativeConflict, r.MemberID, r.ID) != nil {
			continue
		}
		sc, ok := bc.members[r.MemberID]
		if !ok {
			continue
		}
		v, err := bc.pairVenueFor(sc)
		if err != nil {
			continue
		}
		cloid, err := v.NewClientOrderID()
		if err != nil {
			continue
		}
		qty := r.Outcome.CumQty
		if r.ExecutedQty > qty {
			qty = r.ExecutedQty
		}
		deadline, okAdd := pairAddMs(r.CreatedAtMs, rt.Pair.Limits.AcceptanceDeadlineMs)
		if !okAdd {
			continue
		}
		a := bc.newPairNativeAction(rt, m, r.Purpose, r.Trigger, r.OrderSide, r.PositionSide, qty, 0, cloid, deadline, nowMs, basketActionSettling)
		a.SubmittedAtMs = r.Outcome.AcceptedAtMs
		out := r.Outcome
		if basketQtyEqual(out.CumQty, a.RequestedQty) {
			out.Outcome = basketVenueFilled
			out.CumQty = a.RequestedQty
		}
		if out.FeePresent {
			out.FeeEvidenceID = pairFeeEvidenceID(a)
		}
		if why := pairOutcomeError(a, out); why != "" {
			p.addObligation(pairObligation{Kind: pairObligationNativeConflict, MemberID: r.MemberID, ActionID: r.ID, Detail: "the saved native receipt is inconsistent: " + why, AtMs: nowMs})
			rt.touchPairProgress()
			bc.persistPair(rt)
			continue
		}
		if why := bc.pairNativeFillConflict(rt, a, out.FillCoverage); why != "" {
			bc.dropPairNativeReceipt(rt, r, why, nowMs)
			continue
		}
		ev, why := pairEvaluateExecution(a, out)
		if why != "" {
			p.addObligation(pairObligation{Kind: pairObligationNativeConflict, MemberID: r.MemberID, ActionID: r.ID, Detail: "the saved native receipt has malformed execution records: " + why, AtMs: nowMs})
			rt.touchPairProgress()
			bc.persistPair(rt)
			continue
		}
		a.FillCoverageJSON = ev.raw
		a.AcceptedAtMs = out.AcceptedAtMs
		if len(out.FeeCoverage) > 0 {
			fees := append([]pairFeeFill(nil), out.FeeCoverage...)
			sort.Slice(fees, func(i, j int) bool { return pairScopedFillID(a, fees[i].FillID) < pairScopedFillID(a, fees[j].FillID) })
			cur, amt := pairFeeTotals(fees)
			a.FeeCoverageJSON = pairMarshalRecords(fees, len(fees))
			a.FeePresent, a.FeeCurrency, a.FeeAmount, a.ConfirmedFee = true, cur, amt, amt
			a.FeeEvidenceID, a.FeeEvidenceVersion = out.FeeEvidenceID, int64(len(fees))
		}
		if out.FeeFinal && pairExecutionComplete(ev.fills, out.CumQty, out.CumNotional) && pairFeeCoversAll(out.FeeCoverage, ev.fills) {
			a.FeeFinal = true
		}
		a.ConfirmedQty, a.ConfirmedNotional = out.CumQty, out.CumNotional
		a.ConfirmedAvgPx = a.ConfirmedNotional / a.ConfirmedQty
		a.Terminal = true
		a.VenueOrderID = out.VenueOrderID
		a.EvidenceJSON = marshalEntryJSON(out)
		err = bc.store.AttachPairNativeReceipt(rt, a, r.ID)
		switch {
		case errors.Is(err, errPairNativeDuplicate):
			bc.dropPairNativeReceipt(rt, r, err.Error(), nowMs)
			continue
		case err != nil:
			bc.pairLogf(rt, "native receipt %s for %s is not attached yet: %v; it stays in the pair record and no order is sent", r.ID, r.MemberID, err)
			continue
		}
		if r.Unconfirmed != "" {
			rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnconfirmed, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: r.Unconfirmed, AtMs: nowMs, FromMs: a.SubmittedAtMs})
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		bc.pairLogf(rt, "native receipt %s for %s attached as accounting action %s (%.10g confirmed); no order was sent", r.ID, r.MemberID, a.ActionID, a.ConfirmedQty)
	}
}

func (bc *basketCoordinator) dropPairNativeReceipt(rt *basketRuntime, r pairNativeReceipt, why string, nowMs int64) {
	p := &rt.Pair.Progress
	kept := p.NativeReceipts[:0]
	for _, x := range p.NativeReceipts {
		if x.ID != r.ID {
			kept = append(kept, x)
		}
	}
	p.NativeReceipts = kept
	p.clearObligation(pairObligationNativeReceipt, r.MemberID, r.ID)
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairLogf(rt, "native receipt %s for %s is a repeat of saved evidence and changes nothing: %s", r.ID, r.MemberID, why)
}

func (bc *basketCoordinator) pairKillSwitchHLCloser(inner HyperliquidLiveCloser) HyperliquidLiveCloser {
	return bc.pairNativeHLCloser("", pairPurposeKillSwitch, pairNativeTriggerKillSwitch, inner)
}

func (bc *basketCoordinator) pairKillSwitchOKXCloser(inner OKXLiveCloser) OKXLiveCloser {
	return bc.pairNativeOKXCloser("", pairPurposeKillSwitch, pairNativeTriggerKillSwitch, inner)
}

func (bc *basketCoordinator) pairCircuitHLCloser(stratID string, inner HyperliquidLiveCloser) HyperliquidLiveCloser {
	return bc.pairNativeHLCloser(stratID, pairPurposeOperator, pairNativeTriggerCircuit, inner)
}

func (bc *basketCoordinator) pairCircuitOKXCloser(stratID string, inner OKXLiveCloser) OKXLiveCloser {
	return bc.pairNativeOKXCloser(stratID, pairPurposeOperator, pairNativeTriggerCircuit, inner)
}

func (bc *basketCoordinator) pairNativeHLCloser(stratID, purpose, trigger string, inner HyperliquidLiveCloser) HyperliquidLiveCloser {
	if bc == nil || len(bc.sortedPairIDs()) == 0 {
		return inner
	}
	return func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		if stratID != "" {
			if _, _, ok := bc.pairNativeMemberLocked(entryProviderHyperliquid, symbol, stratID); !ok {
				return inner(symbol, partialSz, cancelStopLossOIDs)
			}
		}
		send := bc.beginPairNative(entryProviderHyperliquid, symbol, stratID, purpose, trigger, partialSz, bc.pairWallNow())
		res, cerr := inner(symbol, partialSz, cancelStopLossOIDs)
		var ev *nativeCloseEvidence
		flat, reported := false, false
		if res != nil && res.Close != nil {
			ev, flat = res.Close.Evidence, res.Close.AlreadyFlat
			reported = res.Close.Fill != nil && res.Close.Fill.TotalSz > 0
		}
		bc.finishPairNative(send, ev, reported, flat, cerr, bc.pairWallNow())
		return res, cerr
	}
}

func (bc *basketCoordinator) pairNativeOKXCloser(stratID, purpose, trigger string, inner OKXLiveCloser) OKXLiveCloser {
	if bc == nil || len(bc.sortedPairIDs()) == 0 {
		return inner
	}
	return func(symbol string, partialSz *float64) (*OKXCloseResult, error) {
		if stratID != "" {
			if _, _, ok := bc.pairNativeMemberLocked(entryProviderOKX, symbol, stratID); !ok {
				return inner(symbol, partialSz)
			}
		}
		send := bc.beginPairNative(entryProviderOKX, symbol, stratID, purpose, trigger, partialSz, bc.pairWallNow())
		res, cerr := inner(symbol, partialSz)
		var ev *nativeCloseEvidence
		flat, reported := false, false
		if res != nil && res.Close != nil {
			ev, flat = res.Close.Evidence, res.Close.AlreadyFlat
			reported = res.Close.Fill != nil && res.Close.Fill.TotalSz > 0
		}
		base := bc.finishPairNative(send, ev, reported, flat, cerr, bc.pairWallNow())
		if send != nil && res != nil && res.Close != nil && res.Close.Fill != nil {
			res.Close.Fill.BaseSz = base
		}
		return res, cerr
	}
}

func (bc *basketCoordinator) pairNativeMemberLocked(provider, coin, stratID string) (*basketRuntime, pairContractMember, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pairNativeMember(provider, coin, stratID)
}

func (bc *basketCoordinator) recoverPairNativeLegacy(rt *basketRuntime, now time.Time) bool {
	changed := false
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if !a.pairNative() || a.Status != basketActionCommitted || a.ConfirmedQty <= 1e-12 || strings.TrimSpace(a.SettlementJSON) != "" {
			continue
		}
		if rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeLegacy, MemberID: a.StrategyID, ActionID: a.ActionID,
			Detail: fmt.Sprintf("native action %s was saved committed with %.10g confirmed before native accounting kept a settlement ledger; its booking is unverified and is never booked again", a.ActionID, a.ConfirmedQty), AtMs: now.UnixMilli()}) {
			changed = true
		}
	}
	return changed
}

func pairNativeAccountingPendingFor(rt *basketRuntime, memberID string) string {
	p := &rt.Pair.Progress
	for _, r := range p.NativeReceipts {
		if r.MemberID == memberID {
			return "native receipt " + r.ID + " is not attached"
		}
	}
	for _, o := range p.Obligations {
		if o.MemberID == memberID && (pairNativeObligationKind(o.Kind) && o.Kind != pairObligationEmergencyClose || pairSettlementObligationKind(o.Kind)) {
			return o.Kind + ": " + o.Detail
		}
	}
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if a.StrategyID == memberID && a.pairNative() && basketActionPending(a.Status) {
			return fmt.Sprintf("native action %s is %s", a.ActionID, a.Status)
		}
	}
	return ""
}

func (bc *basketCoordinator) pairMemberVirtual(memberID string) bool {
	sc, ok := bc.members[memberID]
	if !ok {
		return false
	}
	v, err := bc.venueFor(sc)
	return err == nil && !v.LiveShaped()
}

func (bc *basketCoordinator) stepPairEmergency(rt *basketRuntime, in basketCycleInputs, inputs pairStepInputs) {
	now := in.Now
	nowMs := now.UnixMilli()
	p := &rt.Pair.Progress
	var commits []pairPaperCommit
	for _, m := range rt.Contract.pairMembersSorted() {
		s := bc.state.Strategies[m.MemberID]
		if s == nil {
			continue
		}
		pending := s.RiskState.getPendingCircuitClose(PlatformPendingClosePair)
		staged := p.stagedPaperClose(m.MemberID)
		if pending == nil && staged == nil {
			continue
		}
		book := basketPositionSigned(s, m.Symbol)
		if book == 0 {
			if rt.needsDurable {
				continue
			}
			c := pairPaperCommit{memberID: m.MemberID}
			if staged != nil {
				c.staged, c.closeAtMs = true, staged.CloseAtMs
			}
			if why := pairNativeAccountingPendingFor(rt, m.MemberID); why != "" {
				if c.staged {
					commits = append(commits, c)
				}
				if p.addObligation(pairObligation{Kind: pairObligationEmergencyClose, MemberID: m.MemberID, Detail: "the member book is flat, but its emergency close accounting is not committed: " + why, AtMs: nowMs}) {
					rt.touchPairProgress()
					bc.persistPair(rt)
				}
				continue
			}
			c.clearPending = pending != nil || p.obligation(pairObligationEmergencyClose, m.MemberID, "") != nil
			if c.staged || c.clearPending {
				commits = append(commits, c)
			}
			continue
		}
		if !bc.pairMemberVirtual(m.MemberID) {
			if p.addObligation(pairObligation{Kind: pairObligationEmergencyClose, MemberID: m.MemberID, Detail: "the emergency close waits for confirmed native fill evidence; a shared, stale, substituted or average-cost price never books a live member", AtMs: nowMs}) {
				rt.touchPairProgress()
				bc.persistPair(rt)
			}
			continue
		}
		if in.SaveBlocked {
			continue
		}
		rec := staged
		if rec == nil {
			r, why := bc.pairSelectPaperClose(rt, m, s, pending, inputs, nowMs)
			if why != "" {
				if p.addObligation(pairObligation{Kind: pairObligationEmergencyClose, MemberID: m.MemberID, Detail: "the emergency close holds the member book: " + why, AtMs: nowMs}) {
					rt.touchPairProgress()
					bc.persistPair(rt)
					bc.pairAlert(rt, "emergency-close|"+m.MemberID, fmt.Sprintf("%s: the emergency close for %s holds its book and latch: %s; it books only at the member's own checked mark", rt.Contract.BasketID, m.MemberID, why), true)
				}
				continue
			}
			p.PaperCloses = append(p.PaperCloses, r)
			rt.touchPairProgress()
			if !bc.persistPair(rt) {
				p.PaperCloses = p.PaperCloses[:len(p.PaperCloses)-1]
				continue
			}
			rec = p.stagedPaperClose(m.MemberID)
		}
		if why := bc.bookPairPaperClose(rt, m, s, rec); why != "" {
			if p.addObligation(pairObligation{Kind: pairObligationEmergencyClose, MemberID: m.MemberID, Detail: "the staged emergency close could not be booked: " + why, AtMs: nowMs}) {
				rt.touchPairProgress()
				bc.persistPair(rt)
			}
			continue
		}
		commits = append(commits, pairPaperCommit{memberID: m.MemberID, staged: true, closeAtMs: rec.CloseAtMs, clearPending: true})
		bc.pairLogf(rt, "emergency close (%s) booked %s %.10g %s at its own checked mark $%.6f (view generation %d); it commits when the book saves", rec.Trigger, m.MemberID, rec.Qty, m.Symbol, rec.PriceUSD, rec.Generation)
	}
	if len(commits) == 0 {
		return
	}
	undo := make([]pairPaperCommitUndo, 0, len(commits))
	for _, c := range commits {
		s := bc.state.Strategies[c.memberID]
		undo = append(undo, newPairPaperCommitUndo(p, s, c))
		c.apply(p, s, nowMs)
	}
	rt.touchPairProgress()
	bc.clearPairEndedObligations(rt)
	if err := bc.store.SavePartition(bc.state, rt.Partition); err != nil {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i].restore(p)
		}
		rt.touchPairProgress()
		rt.needsDurable = true
		bc.pairLogf(rt, "the emergency close book change is in memory, but the %s state file did not save: %v; the close stays staged at its saved price and time, commits only after the book saves, and nothing is repriced", partitionLabel(rt.Partition), err)
		return
	}
	p.trimPaperCloses()
}

type pairPaperCommit struct {
	memberID     string
	staged       bool
	closeAtMs    int64
	clearPending bool
}

func (c pairPaperCommit) apply(p *pairProgress, s *StrategyState, nowMs int64) {
	if c.staged {
		for i := range p.PaperCloses {
			r := &p.PaperCloses[i]
			if r.MemberID == c.memberID && r.CloseAtMs == c.closeAtMs && r.Status == pairPaperCloseStaged {
				r.Status, r.CommittedAtMs = pairPaperCloseCommitted, nowMs
			}
		}
	}
	if c.clearPending {
		s.RiskState.clearPendingCircuitClose(PlatformPendingClosePair)
		p.clearObligation(pairObligationEmergencyClose, c.memberID, "")
	}
}

type pairPaperCommitUndo struct {
	state      *StrategyState
	memberID   string
	staged     bool
	closeAtMs  int64
	pending    *PendingCircuitClose
	obligation *pairObligation
}

func newPairPaperCommitUndo(p *pairProgress, s *StrategyState, c pairPaperCommit) pairPaperCommitUndo {
	u := pairPaperCommitUndo{state: s, memberID: c.memberID, staged: c.staged, closeAtMs: c.closeAtMs, pending: s.RiskState.getPendingCircuitClose(PlatformPendingClosePair)}
	if o := p.obligation(pairObligationEmergencyClose, c.memberID, ""); o != nil {
		cp := *o
		u.obligation = &cp
	}
	return u
}

func (u pairPaperCommitUndo) restore(p *pairProgress) {
	if u.staged {
		for i := range p.PaperCloses {
			c := &p.PaperCloses[i]
			if c.MemberID == u.memberID && c.CloseAtMs == u.closeAtMs {
				c.Status, c.CommittedAtMs = pairPaperCloseStaged, 0
			}
		}
	}
	if u.pending != nil {
		u.state.RiskState.setPendingCircuitClose(PlatformPendingClosePair, u.pending)
	}
	if u.obligation != nil {
		p.addObligation(*u.obligation)
	}
}

func (bc *basketCoordinator) pairSelectPaperClose(rt *basketRuntime, m pairContractMember, s *StrategyState, pending *PendingCircuitClose, inputs pairStepInputs, nowMs int64) (pairPaperClose, string) {
	pos := s.Positions[m.Symbol]
	if pos == nil || pos.Quantity <= 0 {
		return pairPaperClose{}, "the member has no open position"
	}
	v, ok := inputs.view(m.MemberID)
	if !ok {
		return pairPaperClose{}, "the coordinator has published no valuation view for this member since the last start"
	}
	if v.Substituted {
		return pairPaperClose{}, "the member valuation is substituted from its last checked mark and grants no close authority"
	}
	px, valid, why := v.usdMarkAt(nowMs)
	if !valid {
		return pairPaperClose{}, why
	}
	trigger := pairNativeTriggerCircuit
	if pending != nil && pending.Episode != "" {
		trigger = pending.Episode
	}
	rec := pairPaperClose{MemberID: m.MemberID, Key: rt.Contract.pairMemberKey(m).String(), Symbol: m.Symbol, PositionID: ensurePositionTradeID(s.ID, m.Symbol, pos),
		Side: pos.Side, Qty: pos.Quantity, Trigger: trigger, CloseAtMs: nowMs, PriceUSD: px, NativeMark: v.Mark, PriceCurrency: v.PriceCurrency,
		Source: pairPaperCloseSource, Generation: v.Generation, EvidenceHash: v.EvidenceHash, Status: pairPaperCloseStaged}
	cur := strings.ToUpper(strings.TrimSpace(v.PriceCurrency))
	if cur != pairUSD {
		for _, c := range v.ConversionEvidence {
			if c.Currency == cur {
				cc := c
				rec.Conversion = &cc
				break
			}
		}
		if rec.Conversion == nil {
			return pairPaperClose{}, fmt.Sprintf("the member view has no %s conversion record for its mark", cur)
		}
	}
	return rec, ""
}

func (bc *basketCoordinator) bookPairPaperClose(rt *basketRuntime, m pairContractMember, s *StrategyState, rec *pairPaperClose) string {
	pos := s.Positions[m.Symbol]
	if pos == nil || pos.Quantity <= 0 {
		return "the member has no open position"
	}
	if pos.Side != rec.Side || !basketQtyEqual(pos.Quantity, rec.Qty) {
		return fmt.Sprintf("the member book (%s %.10g) changed after the close was staged (%s %.10g)", pos.Side, pos.Quantity, rec.Side, rec.Qty)
	}
	sc, ok := bc.members[m.MemberID]
	if !ok {
		return "the member has no configuration"
	}
	logger := bc.memberLogger(m.MemberID)
	defer logger.Close()
	details := fmt.Sprintf("Pair emergency close (%s) at the member's own checked mark $%.6f (%s view generation %d, evidence %s)", rec.Trigger, rec.PriceUSD, rec.Source, rec.Generation, shortFingerprint(rec.EvidenceHash))
	var refused error
	withStrategyBookClock(s, time.UnixMilli(rec.CloseAtMs).UTC(), func() {
		refused = withVirtualCloseCostScope(s, &sc, func() {
			mutatePaperPerpsBook(s, m.Symbol, func() {
				bookVirtualClose(s, &sc, m.Symbol, pos, rec.PriceUSD, "pair_emergency_"+rec.Trigger, details, false, logger)
			})
		})
	})
	if refused != nil {
		return refused.Error()
	}
	if s.Positions[m.Symbol] != nil {
		return "the booking owner left the position open"
	}
	return ""
}

func (bc *basketCoordinator) pairEmergencyPendingLocked(part RiskPartition) []string {
	if bc == nil {
		return nil
	}
	var out []string
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		if rt == nil || rt.Partition != part {
			continue
		}
		for _, m := range rt.Contract.pairMembersSorted() {
			s := bc.state.Strategies[m.MemberID]
			why := ""
			switch {
			case s != nil && s.RiskState.getPendingCircuitClose(PlatformPendingClosePair) != nil:
				why = "emergency close pending"
			case rt.Pair.Progress.stagedPaperClose(m.MemberID) != nil:
				why = "staged emergency close not committed"
			case rt.Pair.Progress.obligation(pairObligationEmergencyClose, m.MemberID, "") != nil:
				why = "emergency close obligation open"
			case rt.needsDurable:
				why = "pair settlement not durable"
			}
			if why == "" {
				why = pairNativeAccountingPendingFor(rt, m.MemberID)
			}
			if why != "" {
				out = append(out, fmt.Sprintf("%s (%s)", m.MemberID, why))
			}
		}
	}
	sort.Strings(out)
	return out
}

func pairEmergencyPendingFor(state *AppState, part RiskPartition) []string {
	if state == nil || state.pairViews == nil {
		return nil
	}
	return state.pairViews.pairEmergencyPendingLocked(part)
}

func pairPartitionHasPair(state *AppState, cfg *Config, part RiskPartition) bool {
	if state == nil || state.pairViews == nil || cfg == nil {
		return false
	}
	for _, sc := range strategiesInPartition(cfg.Strategies, part) {
		if isPairMember(sc) {
			return true
		}
	}
	return false
}

func requestPairEmergencyClose(s *StrategyState, symbol string, qty float64, reason string) {
	if s == nil {
		return
	}
	if p := s.RiskState.getPendingCircuitClose(PlatformPendingClosePair); p != nil {
		for _, c := range p.Symbols {
			if c.Symbol == symbol {
				return
			}
		}
	}
	trigger := pairNativeTriggerCircuit
	if strings.Contains(reason, "kill_switch") {
		trigger = pairNativeTriggerKillSwitch
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingClosePair, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: symbol, Size: qty}},
		Episode: trigger,
		FiredAt: tradingNow().UTC(),
	})
}

func pairStrategyMember(s *StrategyState, sc *StrategyConfig) bool {
	return (sc != nil && isPairMember(*sc)) || strategyPairCostRequired(s)
}

func (p *pairProgress) hasStagedPaperClose() bool {
	for _, c := range p.PaperCloses {
		if c.Status == pairPaperCloseStaged {
			return true
		}
	}
	return false
}

func pairActionFillPossible(rt *basketRuntime, a *basketAction) bool {
	return basketActionPending(a.Status) && a.Status != basketActionReserved
}

func pairNativeUnknownFromMs(rt *basketRuntime, memberID string) int64 {
	if rt == nil || rt.Pair == nil {
		return 0
	}
	var from int64
	for _, o := range rt.Pair.Progress.Obligations {
		if o.MemberID != memberID || o.FromMs <= 0 || (o.Kind != pairObligationNativeUnknown && o.Kind != pairObligationNativeUnconfirmed && o.Kind != pairObligationNativeAmbiguous) {
			continue
		}
		if from == 0 || o.FromMs < from {
			from = o.FromMs
		}
	}
	for _, r := range rt.Pair.Progress.NativeReceipts {
		if r.MemberID == memberID && r.Outcome.AcceptedAtMs > 0 && (from == 0 || r.Outcome.AcceptedAtMs < from) {
			from = r.Outcome.AcceptedAtMs
		}
	}
	return from
}

func withoutPairMembers(in []StrategyConfig) []StrategyConfig {
	out := in[:0:0]
	for _, sc := range in {
		if !isPairMember(sc) {
			out = append(out, sc)
		}
	}
	return out
}

func pairKillSwitchAccountingNote(msg string, pending []string) string {
	note := fmt.Sprintf("Pair member accounting is not committed yet: %s. The latch stays and no auto-reset runs until each pair member books from confirmed fill evidence.", strings.Join(pending, "; "))
	if strings.TrimSpace(msg) == "" {
		return note
	}
	return msg + "\n" + note
}

func (a *basketAction) savedNoteNative() bool {
	var n pairRecoveryNote
	dec := json.NewDecoder(strings.NewReader(a.RecoveryJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return false
	}
	return n.NativeBooked
}

func (bc *basketCoordinator) pairWallNow() time.Time {
	if bc.pairClock != nil {
		return bc.pairClock()
	}
	return tradingNow()
}
