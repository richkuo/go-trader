package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	pairNativeModeVenueHistory = "venue_history"
	pairNativeModeZeroFill     = "zero_fill"
	pairNativeModeOperatorRead = "operator_venue_read"
	pairNativeModeWriteOff     = "write_off"
	pairNativeModeAcknowledge  = "acknowledge"

	pairNativeDecidedByCoordinator = "coordinator"
	pairNativeDecidedByOperator    = "operator"

	pairNativeDecisionShow     = "show"
	pairNativeDecisionRead     = "venue-read"
	pairNativeDecisionWriteOff = "write-off"
	pairNativeDecisionAck      = "acknowledge"

	pairNativeProvenanceEvidence = "close_evidence"
	pairNativeProvenanceError    = "close_error"
	pairNativeProvenanceNone     = "close_no_evidence"
	pairNativeProvenanceOperator = "operator_orders"

	pairObligationNativeFundingUncertain = "native_funding_uncertain"

	pairNativeReasonMax = 500
	pairNativeTextMax   = 500
)

type pairNativeAttempt struct {
	ID             string                `json:"id"`
	MemberID       string                `json:"member_id"`
	Key            string                `json:"key"`
	Provider       string                `json:"provider"`
	AccountKey     string                `json:"account_key"`
	Instrument     string                `json:"instrument"`
	ContractFP     string                `json:"contract_fingerprint"`
	Purpose        string                `json:"purpose"`
	Trigger        string                `json:"trigger"`
	OrderSide      string                `json:"order_side,omitempty"`
	PositionSide   string                `json:"position_side,omitempty"`
	ActionID       string                `json:"action_id,omitempty"`
	ReceiptID      string                `json:"receipt_id,omitempty"`
	Ambiguous      string                `json:"ambiguous,omitempty"`
	SentAtMs       int64                 `json:"sent_at_ms"`
	EndedAtMs      int64                 `json:"ended_at_ms"`
	DeadlineMs     int64                 `json:"deadline_ms"`
	VenueOrderIDs  []string              `json:"venue_order_ids,omitempty"`
	OrdersComplete bool                  `json:"orders_complete"`
	Outcome        string                `json:"outcome"`
	Evidence       json.RawMessage       `json:"evidence,omitempty"`
	CloseError     string                `json:"close_error,omitempty"`
	Provenance     string                `json:"provenance"`
	Rev            int64                 `json:"rev"`
	RetryCount     int                   `json:"retry_count,omitempty"`
	RetryAtMs      int64                 `json:"retry_at_ms,omitempty"`
	LastError      string                `json:"last_error,omitempty"`
	LastReadAtMs   int64                 `json:"last_read_at_ms,omitempty"`
	Pending        *pairNativeResolution `json:"pending,omitempty"`
}

type pairNativeResolution struct {
	ID                string             `json:"id"`
	Partition         string             `json:"partition"`
	PairID            string             `json:"pair_id"`
	MemberID          string             `json:"member_id"`
	Kind              string             `json:"kind"`
	ObligationID      string             `json:"obligation_id"`
	AttemptID         string             `json:"attempt_id,omitempty"`
	ActionID          string             `json:"action_id,omitempty"`
	ReceiptID         string             `json:"receipt_id,omitempty"`
	Mode              string             `json:"mode"`
	DecidedBy         string             `json:"decided_by"`
	Operator          string             `json:"operator,omitempty"`
	RequestID         string             `json:"request_id,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	Revision          string             `json:"revision"`
	DecidedAtMs       int64              `json:"decided_at_ms"`
	CommittedAtMs     int64              `json:"committed_at_ms"`
	Prior             pairObligation     `json:"prior"`
	Attempt           *pairNativeAttempt `json:"attempt,omitempty"`
	Evidence          json.RawMessage    `json:"evidence,omitempty"`
	FillIDs           []string           `json:"fill_ids,omitempty"`
	BookedQty         float64            `json:"booked_qty,omitempty"`
	FundingHoldFromMs int64              `json:"funding_hold_from_ms,omitempty"`
	AlertStatus       string             `json:"alert_status,omitempty"`
}

type pairNativeFundingHold struct {
	MemberID     string `json:"member_id"`
	Kind         string `json:"kind"`
	ObligationID string `json:"obligation_id"`
	ResolutionID string `json:"resolution_id"`
	FromMs       int64  `json:"from_ms"`
	AtMs         int64  `json:"at_ms"`
	Detail       string `json:"detail"`
}

type pairNativeOperatorRequest struct {
	ID           string   `json:"id"`
	MemberID     string   `json:"member_id"`
	Kind         string   `json:"kind"`
	ObligationID string   `json:"obligation_id"`
	Revision     string   `json:"revision"`
	Decision     string   `json:"decision"`
	Operator     string   `json:"operator"`
	Reason       string   `json:"reason"`
	Orders       []string `json:"orders,omitempty"`
	StagedAtMs   int64    `json:"staged_at_ms"`
}

type pairNativeReadToken struct {
	attemptID string
	revision  string
	gen       uint64
}

type pairNativeTarget struct {
	ob        pairObligation
	attID     string
	action    *basketAction
	orders    []string
	side      string
	fromMs    int64
	throughMs int64
}

type pairNativeOperatorCommand struct {
	Invoker      string
	Partition    string
	PairID       string
	MemberID     string
	Kind         string
	ObligationID string
	Decision     string
	Revision     string
	Reason       string
	Orders       []string
}

type pairNativeOperatorRouter interface {
	pairNativeOperator(cmd pairNativeOperatorCommand, stage bool) (string, bool)
}

func pairNativeTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (p pairProgress) hasNativeRecovery() bool {
	return len(p.NativeAttempts) > 0 || p.NativeAttemptSeq > 0 || len(p.NativeFundingHolds) > 0 || len(p.NativeRequests) > 0
}

func (p *pairProgress) nativeAttempt(id string) *pairNativeAttempt {
	for i := range p.NativeAttempts {
		if p.NativeAttempts[i].ID == id {
			return &p.NativeAttempts[i]
		}
	}
	return nil
}

func (p *pairProgress) nativeAttemptFor(ob pairObligation) *pairNativeAttempt {
	if ob.ActionID == "" {
		return nil
	}
	for i := range p.NativeAttempts {
		a := &p.NativeAttempts[i]
		if a.MemberID == ob.MemberID && (a.ID == ob.ActionID || a.ActionID == ob.ActionID || a.ReceiptID == ob.ActionID) {
			return a
		}
	}
	return nil
}

func (p *pairProgress) nativePendingFor(actionID string) bool {
	for _, a := range p.NativeAttempts {
		if a.Pending != nil && a.Pending.ActionID == actionID {
			return true
		}
	}
	return false
}

func (p *pairProgress) nativePendingResolution(kind, memberID, obligationID string) *pairNativeAttempt {
	for i := range p.NativeAttempts {
		a := &p.NativeAttempts[i]
		if a.Pending != nil && a.Pending.Kind == kind && a.Pending.MemberID == memberID && a.Pending.ObligationID == obligationID {
			return a
		}
	}
	return nil
}

func (p *pairProgress) nativeRequestFor(kind, memberID, obligationID string) *pairNativeOperatorRequest {
	for i := range p.NativeRequests {
		r := &p.NativeRequests[i]
		if r.Kind == kind && r.MemberID == memberID && r.ObligationID == obligationID {
			return r
		}
	}
	return nil
}

func (s *pairRuntimeState) noteNativeRequestsSeen() {
	seen := map[string]bool{}
	for _, r := range s.Progress.NativeRequests {
		seen[r.ID] = true
	}
	s.nativeSeen = seen
}

func (p *pairProgress) removeNativeRequest(id string) bool {
	kept := p.NativeRequests[:0:0]
	removed := false
	for _, r := range p.NativeRequests {
		if r.ID == id {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	p.NativeRequests = kept
	return removed
}

func (p *pairProgress) pruneNativeAttempts() {
	refs := map[string]bool{}
	for _, o := range p.Obligations {
		if pairNativeObligationKind(o.Kind) && o.Kind != pairObligationEmergencyClose && o.ActionID != "" {
			refs[o.MemberID+"|"+o.ActionID] = true
		}
	}
	for _, r := range p.NativeReceipts {
		refs[r.MemberID+"|"+r.ID] = true
	}
	kept := p.NativeAttempts[:0:0]
	for _, a := range p.NativeAttempts {
		if refs[a.MemberID+"|"+a.ID] || (a.ActionID != "" && refs[a.MemberID+"|"+a.ActionID]) || (a.ReceiptID != "" && refs[a.MemberID+"|"+a.ReceiptID]) {
			if a.Pending != nil && p.obligation(a.Pending.Kind, a.Pending.MemberID, a.Pending.ObligationID) == nil {
				a.Pending = nil
			}
			kept = append(kept, a)
		}
	}
	p.NativeAttempts = kept
}

func (p pairProgress) nativeRecoveryPending() []string {
	var out []string
	for _, h := range p.NativeFundingHolds {
		out = append(out, fmt.Sprintf("native funding hold for %s from %d after resolution %s", h.MemberID, h.FromMs, h.ResolutionID))
	}
	return append(out, p.nativeRecoveryStaged()...)
}

func (p pairProgress) nativeRecoveryStaged() []string {
	var out []string
	for _, r := range p.NativeRequests {
		out = append(out, fmt.Sprintf("operator native decision %s for %s %s is staged", r.ID, r.Kind, r.MemberID))
	}
	for _, a := range p.NativeAttempts {
		if a.Pending != nil {
			out = append(out, fmt.Sprintf("native recovery %s for %s waits for its book to commit", a.ID, a.MemberID))
		}
	}
	return out
}

func pairCloneProgress(p pairProgress) pairProgress {
	b, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var out pairProgress
	if err := json.Unmarshal(b, &out); err != nil {
		return p
	}
	return out
}

func pairNativeResolutionID(rt *basketRuntime, kind, memberID, obligationID string) string {
	return "pnz-" + pairSHA256([]byte(strings.Join([]string{rt.Partition.String(), rt.Contract.BasketID, kind, memberID, obligationID}, "|")))[:32]
}

func (rt *basketRuntime) pairNativeResolution(kind, memberID, obligationID string) *pairNativeResolution {
	if rt == nil || rt.Pair == nil {
		return nil
	}
	for i := range rt.Pair.resolutions {
		r := &rt.Pair.resolutions[i]
		if r.Kind == kind && r.MemberID == memberID && r.ObligationID == obligationID {
			return r
		}
	}
	return nil
}

func (rt *basketRuntime) pairNativeResolvedFillOverlap(memberID string, fills []pairConfirmedFill) string {
	if rt == nil || rt.Pair == nil || len(fills) == 0 {
		return ""
	}
	want := map[string]bool{}
	for _, f := range fills {
		want[f.FillID] = true
	}
	for _, r := range rt.Pair.resolutions {
		if r.MemberID != memberID || (r.Mode != pairNativeModeWriteOff && r.Mode != pairNativeModeAcknowledge) {
			continue
		}
		for _, id := range r.FillIDs {
			if want[id] {
				return fmt.Sprintf("native fill %s belongs to the %s decision %s; a decided fill never attaches or books later", id, r.Mode, r.ID)
			}
		}
	}
	return ""
}

func pairNativeHorizonKind(kind string) bool {
	return kind == pairObligationNativeUnknown || kind == pairObligationNativeUnconfirmed || kind == pairObligationNativeAmbiguous
}

func pairNativeResolvableKind(kind string) bool {
	switch kind {
	case pairObligationNativeUnknown, pairObligationNativeUnconfirmed, pairObligationNativeConflict, pairObligationNativeAmbiguous, pairObligationNativeLegacy:
		return true
	}
	return false
}

func pairNativeDecisionAllowed(kind, decision string) bool {
	switch kind {
	case pairObligationNativeUnknown, pairObligationNativeUnconfirmed:
		return decision == pairNativeDecisionRead || decision == pairNativeDecisionWriteOff
	case pairObligationNativeConflict, pairObligationNativeAmbiguous, pairObligationNativeLegacy:
		return decision == pairNativeDecisionWriteOff || decision == pairNativeDecisionAck
	}
	return false
}

func pairNativeDecisionsFor(kind string) []string {
	var out []string
	for _, d := range []string{pairNativeDecisionRead, pairNativeDecisionWriteOff, pairNativeDecisionAck} {
		if pairNativeDecisionAllowed(kind, d) {
			out = append(out, d)
		}
	}
	return out
}

func pairNativeObligationRevision(rt *basketRuntime, ob pairObligation) string {
	type actionRev struct {
		ID      string  `json:"id"`
		Status  string  `json:"status"`
		CQ      float64 `json:"cq"`
		CN      float64 `json:"cn"`
		AQ      float64 `json:"aq"`
		Fills   string  `json:"fills"`
		Fees    string  `json:"fees"`
		Ledger  string  `json:"ledger"`
		Request string  `json:"request"`
	}
	type attemptRev struct {
		ID       string   `json:"id"`
		Rev      int64    `json:"rev"`
		Orders   []string `json:"orders"`
		Complete bool     `json:"complete"`
		Pending  bool     `json:"pending"`
	}
	doc := struct {
		Kind    string      `json:"kind"`
		Member  string      `json:"member"`
		ID      string      `json:"id"`
		FromMs  int64       `json:"from_ms"`
		Action  *actionRev  `json:"action,omitempty"`
		Attempt *attemptRev `json:"attempt,omitempty"`
		Receipt string      `json:"receipt,omitempty"`
	}{Kind: ob.Kind, Member: ob.MemberID, ID: ob.ActionID, FromMs: ob.FromMs}
	if a := rt.Actions[ob.ActionID]; a != nil && a.StrategyID == ob.MemberID {
		doc.Action = &actionRev{ID: a.ActionID, Status: a.Status, CQ: a.ConfirmedQty, CN: a.ConfirmedNotional, AQ: a.AppliedQty, Fills: a.FillCoverageJSON,
			Fees: a.FeeCoverageJSON, Ledger: a.SettlementJSON, Request: pairSHA256([]byte(a.RequestJSON))}
	}
	if att := rt.Pair.Progress.nativeAttemptFor(ob); att != nil {
		doc.Attempt = &attemptRev{ID: att.ID, Rev: att.Rev, Orders: att.VenueOrderIDs, Complete: att.OrdersComplete, Pending: att.Pending != nil}
	}
	for _, r := range rt.Pair.Progress.NativeReceipts {
		if r.ID == ob.ActionID && r.MemberID == ob.MemberID {
			doc.Receipt = r.Hash
		}
	}
	b, _ := json.Marshal(doc)
	return pairSHA256(b)[:16]
}

func (bc *basketCoordinator) pairNativeGenOf(memberID string) uint64 {
	if bc.pairNativeGen == nil {
		return 0
	}
	return bc.pairNativeGen[memberID]
}

func (bc *basketCoordinator) pairNativeSendBegan(memberID string) {
	if bc.pairNativeGen == nil {
		bc.pairNativeGen = map[string]uint64{}
	}
	if bc.pairNativeSending == nil {
		bc.pairNativeSending = map[string]int{}
	}
	bc.pairNativeGen[memberID]++
	bc.pairNativeSending[memberID]++
}

func (bc *basketCoordinator) pairNativeSendEnded(memberID string) {
	if bc.pairNativeGen == nil {
		bc.pairNativeGen = map[string]uint64{}
	}
	bc.pairNativeGen[memberID]++
	if bc.pairNativeSending[memberID] > 0 {
		bc.pairNativeSending[memberID]--
	}
}

func (bc *basketCoordinator) pairNativeBusy(rt *basketRuntime, memberID string) string {
	if bc.pairNativeSending[memberID] > 0 {
		return fmt.Sprintf("a native close for %s is in flight", memberID)
	}
	if rt.Pair.nativeReads[memberID] != nil {
		return fmt.Sprintf("a venue history read for %s is in flight", memberID)
	}
	return ""
}

func (bc *basketCoordinator) pairNativeAttemptFor(rt *basketRuntime, send *pairNativeSend, ev *nativeCloseEvidence, closeErr error, res pairNativeResult, now time.Time) pairNativeAttempt {
	p := &rt.Pair.Progress
	p.NativeAttemptSeq++
	m := send.m
	nowMs := now.UnixMilli()
	att := pairNativeAttempt{MemberID: m.MemberID, Key: rt.Contract.pairMemberKey(m).String(), Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument,
		ContractFP: rt.Contract.Fingerprint(), Purpose: send.purpose, Trigger: send.trigger, OrderSide: send.orderSide, PositionSide: send.positionSide,
		Ambiguous: send.ambiguous, SentAtMs: send.sentMs, EndedAtMs: nowMs, Outcome: res.Out.Outcome, Rev: 1, Provenance: pairNativeProvenanceNone}
	att.ID = "pna-" + pairSHA256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d|%d", rt.Contract.BasketID, att.Key, att.Purpose, att.Trigger, att.SentAtMs, p.NativeAttemptSeq)))[:32]
	if d, ok := pairAddMs(send.sentMs, rt.Pair.Limits.AcceptanceDeadlineMs); ok {
		att.DeadlineMs = d
	}
	if closeErr != nil {
		att.CloseError = pairNativeTruncate(closeErr.Error(), pairNativeTextMax)
		att.Provenance = pairNativeProvenanceError
	}
	if ev != nil {
		att.Provenance = pairNativeProvenanceEvidence
		if b, err := json.Marshal(ev); err == nil {
			att.Evidence = b
		}
		valid := len(ev.VenueOrderIDs) > 0 && len(ev.VenueOrderIDs) <= pairNativeHistoryMaxOrders
		seen := map[string]bool{}
		var ids []string
		for _, id := range ev.VenueOrderIDs {
			if !pairPrintableASCII(id, pairEvidenceMaxIDLen) {
				valid = false
				break
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if valid {
			sort.Strings(ids)
			att.VenueOrderIDs = ids
			if ev.OrdersComplete != nil {
				att.OrdersComplete = *ev.OrdersComplete
			} else {
				att.OrdersComplete = closeErr == nil
			}
		}
	}
	if att.OrderSide == "" {
		att.OrderSide, att.PositionSide = pairNativeSideFromFills(ev)
	}
	if next, ok := pairAddMs(nowMs, pairNativeRetryDelay(rt, 0)); ok {
		att.RetryAtMs = next
	}
	return att
}

func pairNativeRetryDelay(rt *basketRuntime, count int) int64 {
	base := rt.Pair.Limits.RetryInitialMs
	if base <= 0 {
		base = 1000
	}
	max := rt.Pair.Limits.RetryMaxMs
	if max < base {
		max = base
	}
	d := base
	for i := 0; i < count && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

func (bc *basketCoordinator) pairNativeHistoryReaderFor(m pairContractMember) pairNativeHistoryReader {
	if bc.pairNativeHistory != nil {
		return bc.pairNativeHistory
	}
	sc, ok := bc.members[m.MemberID]
	if !ok {
		return nil
	}
	v, err := bc.pairVenueFor(sc)
	if err != nil || !v.LiveShaped() || v.Virtual() {
		return nil
	}
	if src, ok := v.(pairNativeHistorySource); ok {
		return src.NativeHistory
	}
	return nil
}

func (bc *basketCoordinator) pairNativeTargetFor(rt *basketRuntime, ob pairObligation, orders []string) (pairNativeTarget, string) {
	t := pairNativeTarget{ob: ob}
	if ob.Kind != pairObligationNativeUnknown && ob.Kind != pairObligationNativeUnconfirmed {
		return t, fmt.Sprintf("%s is never resolved from a venue read; only native_fill_unknown and native_fill_unconfirmed are", ob.Kind)
	}
	p := &rt.Pair.Progress
	complete := false
	att := p.nativeAttemptFor(ob)
	if a := rt.Actions[ob.ActionID]; a != nil && a.StrategyID == ob.MemberID && a.pairNative() {
		t.action = a
	}
	if att == nil && t.action == nil {
		return t, "the obligation names no saved native attempt or action; an obligation saved before attempts were kept may cover several closes, so only a write-off can end it"
	}
	if att != nil {
		if att.Ambiguous != "" {
			return t, "the close was ambiguous when it ran: " + att.Ambiguous
		}
		t.attID = att.ID
		t.orders, complete = att.VenueOrderIDs, att.OrdersComplete
		t.side = att.OrderSide
		t.fromMs, t.throughMs = att.SentAtMs-pairNativeAcceptSkewMs, att.DeadlineMs+pairNativeAcceptSkewMs
	}
	if a := t.action; a != nil {
		if !pairNativeRereadable(rt, a) {
			return t, fmt.Sprintf("native action %s is %s; a venue read resolves only a held or unresolved native action, or a recovered one whose booking still waits for evidence", a.ActionID, a.Status)
		}
		if t.side == "" {
			t.side = a.OrderSide
		}
		if att == nil {
			t.fromMs, t.throughMs = pairNativeStartMs(a)-pairNativeAcceptSkewMs, a.AcceptDeadlineMs+pairNativeAcceptSkewMs
		}
	}
	if len(orders) > 0 {
		saved, why := pairNativeSavedOrderIDs(att, t.action)
		if why != "" {
			return t, why
		}
		seen := map[string]bool{}
		var merged []string
		for _, list := range [][]string{orders, saved} {
			for _, o := range list {
				if !seen[o] {
					seen[o] = true
					merged = append(merged, o)
				}
			}
		}
		for _, other := range p.NativeAttempts {
			if other.ID == t.attID || other.MemberID != ob.MemberID {
				continue
			}
			for _, id := range other.VenueOrderIDs {
				if seen[id] {
					return t, fmt.Sprintf("venue order %s belongs to native attempt %s; one order is attributed to one attempt", id, other.ID)
				}
			}
		}
		for _, aid := range rt.sortedActionIDs() {
			o := rt.Actions[aid]
			if o == t.action || o.StrategyID != ob.MemberID || !o.pair() || o.VenueOrderID == "" {
				continue
			}
			if seen[o.VenueOrderID] {
				return t, fmt.Sprintf("venue order %s belongs to pair action %s (%s); one order is attributed to one action", o.VenueOrderID, o.ActionID, o.Purpose)
			}
		}
		sort.Strings(merged)
		t.orders, complete = merged, true
	}
	switch {
	case len(t.orders) == 0:
		return t, "no venue order id is saved for this close; an operator venue-read with the order ids can check it"
	case !complete:
		return t, "the saved order list may be incomplete because a send had an unknown result; an operator venue-read with the order ids can check it"
	case len(t.orders) > pairNativeHistoryMaxOrders:
		return t, fmt.Sprintf("the close names %d orders, above the %d a venue read checks", len(t.orders), pairNativeHistoryMaxOrders)
	case t.side != "buy" && t.side != "sell":
		return t, "the close side is unknown"
	case t.fromMs <= 0 || t.throughMs < t.fromMs:
		return t, "the attempt has no acceptance window"
	}
	return t, ""
}

func pairNativeFillOwnedElsewhere(rt *basketRuntime, a *basketAction, fills []pairConfirmedFill) string {
	if len(fills) == 0 {
		return ""
	}
	want := map[string]bool{}
	for _, f := range fills {
		want[f.FillID] = true
	}
	for _, id := range rt.sortedActionIDs() {
		o := rt.Actions[id]
		if o == a || o.StrategyID != a.StrategyID || !o.pair() || o.pairNative() || strings.TrimSpace(o.FillCoverageJSON) == "" {
			continue
		}
		prev, err := pairParseFills(o.FillCoverageJSON)
		if err != nil {
			return fmt.Sprintf("pair action %s holds unreadable fill records: %v", o.ActionID, err)
		}
		for _, f := range prev {
			if want[f.FillID] {
				return fmt.Sprintf("fill %s is already accounted on pair action %s (%s); a venue read never books it again as a native close fill", f.FillID, o.ActionID, o.Purpose)
			}
		}
	}
	return ""
}

func pairNativeSavedOrderIDs(att *pairNativeAttempt, a *basketAction) ([]string, string) {
	var out []string
	if att != nil {
		out = append(out, att.VenueOrderIDs...)
		if len(att.Evidence) > 0 {
			var ev nativeCloseEvidence
			if err := json.Unmarshal(att.Evidence, &ev); err != nil {
				return nil, "the saved close evidence of the attempt is unreadable, so its order ids are unknown; only a write-off can end it"
			}
			out = append(out, ev.VenueOrderIDs...)
		}
	}
	if a != nil && a.VenueOrderID != "" {
		out = append(out, a.VenueOrderID)
	}
	for _, id := range out {
		if !pairPrintableASCII(id, pairEvidenceMaxIDLen) {
			return nil, fmt.Sprintf("a saved venue order id %q of this close is not a readable order id, so a venue read cannot cover every order the close sent", id)
		}
	}
	return out, ""
}

func (bc *basketCoordinator) stepPairNativeRecovery(rt *basketRuntime, in basketCycleInputs) {
	bc.flushPairNativeAlerts(rt)
	if in.SaveBlocked || rt.needsDurable {
		return
	}
	if rt.Pair.nativeReads == nil {
		rt.Pair.nativeReads = map[string]*pairNativeReadToken{}
	}
	if bc.applyPairNativeRequests(rt, in) {
		bc.flushPairNativeAlerts(rt)
		return
	}
	nowMs := in.Now.UnixMilli()
	p := &rt.Pair.Progress
	staged := map[string]bool{}
	for _, r := range p.NativeRequests {
		staged[r.MemberID] = true
	}
	ids := make([]string, 0, len(p.NativeAttempts))
	for _, a := range p.NativeAttempts {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		att := p.nativeAttempt(id)
		if att == nil || att.RetryAtMs <= 0 || att.RetryAtMs > nowMs || staged[att.MemberID] || !att.OrdersComplete || att.Ambiguous != "" {
			continue
		}
		var ob *pairObligation
		for _, kind := range []string{pairObligationNativeUnknown, pairObligationNativeUnconfirmed} {
			for _, key := range []string{att.ID, att.ActionID} {
				if key != "" && ob == nil {
					ob = p.obligation(kind, att.MemberID, key)
				}
			}
		}
		if ob == nil || bc.pairNativeBusy(rt, att.MemberID) != "" || rt.pairNativeResolution(ob.Kind, ob.MemberID, ob.ActionID) != nil {
			continue
		}
		if a := rt.Actions[att.ActionID]; att.ActionID != "" && a != nil && !pairNativeRereadable(rt, a) {
			continue
		}
		t, why := bc.pairNativeTargetFor(rt, *ob, nil)
		if why == "" {
			why = bc.readPairNative(rt, t, nil, in)
		}
		if why != "" {
			bc.notePairNativeReadFailure(rt, id, why, in.Now)
		}
		bc.flushPairNativeAlerts(rt)
		return
	}
}

func (bc *basketCoordinator) notePairNativeReadFailure(rt *basketRuntime, attemptID, why string, now time.Time) {
	att := rt.Pair.Progress.nativeAttempt(attemptID)
	if att == nil {
		return
	}
	nowMs := now.UnixMilli()
	att.RetryCount++
	att.LastError = pairNativeTruncate(why, pairNativeTextMax)
	att.LastReadAtMs = nowMs
	att.RetryAtMs = 0
	if bc.pairNativeHistoryReaderFor(pairContractMember{MemberID: att.MemberID}) != nil {
		if next, ok := pairAddMs(nowMs, pairNativeRetryDelay(rt, att.RetryCount)); ok {
			att.RetryAtMs = next
		}
	}
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairLogf(rt, "native attempt %s for %s: the venue read did not resolve it (%s); nothing is booked or cleared and no order is sent", attemptID, att.MemberID, why)
	bc.pairAlert(rt, "native-recovery|"+attemptID, fmt.Sprintf("%s native close attempt %s is not resolved by its venue read yet (%s); the obligation stays, nothing is booked and the read retries", att.MemberID, attemptID, why), false)
}

func (bc *basketCoordinator) readPairNative(rt *basketRuntime, t pairNativeTarget, op *pairNativeOperatorRequest, in basketCycleInputs) string {
	m, ok := rt.Contract.pairMember(t.ob.MemberID)
	if !ok {
		return "the obligation member is not in the pair contract"
	}
	reader := bc.pairNativeHistoryReaderFor(m)
	if reader == nil {
		return "this host has no private venue history reader for the member (paper, replay and virtual hosts never read venue history)"
	}
	if why := bc.pairNativeBusy(rt, m.MemberID); why != "" {
		return why
	}
	rev := pairNativeObligationRevision(rt, t.ob)
	tok := &pairNativeReadToken{attemptID: t.attID, revision: rev, gen: bc.pairNativeGenOf(m.MemberID)}
	rt.Pair.nativeReads[m.MemberID] = tok
	req := pairNativeHistoryRequest{V: pairNativeHistoryVersion, PairID: rt.Contract.BasketID, Partition: rt.Partition.String(), MemberID: m.MemberID, Provider: m.Provider,
		AccountKey: m.AccountKey, Instrument: m.Instrument, AttemptID: t.attID, OrderSide: t.side, VenueOrderIDs: t.orders, FromMs: t.fromMs, ThroughMs: t.throughMs, Revision: rev}
	if req.AttemptID == "" {
		req.AttemptID = "pno-" + pairSHA256([]byte(t.ob.Kind + "|" + t.ob.MemberID + "|" + t.ob.ActionID))[:32]
	}
	parent := bc.parent
	if parent == nil {
		parent = context.Background()
	}
	var res pairNativeHistoryResult
	var err error
	bc.unlocked(func() {
		ctx, cancel := context.WithTimeout(parent, pairNativeHistoryTimeout)
		defer cancel()
		res, err = reader(ctx, m, req)
	})
	if rt.Pair.nativeReads[m.MemberID] != tok {
		return "the read token was replaced while the venue history was read; the stale answer is discarded"
	}
	delete(rt.Pair.nativeReads, m.MemberID)
	cur := rt.Pair.Progress.obligation(t.ob.Kind, t.ob.MemberID, t.ob.ActionID)
	switch {
	case bc.pairNativeGenOf(m.MemberID) != tok.gen:
		return "a native close for the member ran while the venue history was read; the stale answer is discarded"
	case cur == nil:
		return "the obligation ended while the venue history was read; the stale answer is discarded"
	case pairNativeObligationRevision(rt, *cur) != rev:
		return "the obligation evidence changed while the venue history was read; the stale answer is discarded"
	case t.action != nil && rt.Actions[t.action.ActionID] != t.action:
		return "the native action changed while the venue history was read; the stale answer is discarded"
	case err != nil:
		return "the venue history read failed: " + err.Error()
	case !res.Complete:
		return "the venue history is not complete: " + res.Detail
	}
	t.ob = *cur
	if req.AttemptID != t.attID {
		t.attID = ""
	}
	return bc.applyPairNativeHistory(rt, m, t, req, res, op, in.Now)
}

func pairNativeKeepVisible(prevEvidence string, out *basketVenueOutcome) {
	if strings.TrimSpace(prevEvidence) == "" || len(out.Fills) == 0 {
		return
	}
	var prev basketVenueOutcome
	if json.Unmarshal([]byte(prevEvidence), &prev) != nil {
		return
	}
	saved := map[string]pairVenueFill{}
	for _, f := range prev.Fills {
		saved[f.FillID] = f
	}
	for i, f := range out.Fills {
		if s, ok := saved[f.FillID]; ok && s.BaseQty == f.BaseQty && s.EventMs == f.EventMs {
			out.Fills[i] = s
		}
	}
}

func (bc *basketCoordinator) newPairNativeResolution(rt *basketRuntime, ob pairObligation, attID, mode string, op *pairNativeOperatorRequest, now time.Time) pairNativeResolution {
	nowMs := now.UnixMilli()
	r := pairNativeResolution{ID: pairNativeResolutionID(rt, ob.Kind, ob.MemberID, ob.ActionID), Partition: rt.Partition.String(), PairID: rt.Contract.BasketID, MemberID: ob.MemberID,
		Kind: ob.Kind, ObligationID: ob.ActionID, AttemptID: attID, Mode: mode, DecidedBy: pairNativeDecidedByCoordinator, Revision: pairNativeObligationRevision(rt, ob),
		DecidedAtMs: nowMs, CommittedAtMs: nowMs, Prior: ob, AlertStatus: pairNativeAlertQueued}
	if op != nil {
		r.DecidedBy, r.Operator, r.Reason, r.RequestID, r.DecidedAtMs = pairNativeDecidedByOperator, op.Operator, op.Reason, op.ID, op.StagedAtMs
		if mode == pairNativeModeVenueHistory {
			r.Mode = pairNativeModeOperatorRead
		}
	}
	if att := rt.Pair.Progress.nativeAttempt(attID); att != nil {
		cp := *att
		cp.Pending = nil
		r.Attempt = &cp
	}
	return r
}

func (bc *basketCoordinator) commitPairNativeMutation(rt *basketRuntime, rows []pairNativeResolution, updates []pairNativeRowUpdate, actions []*basketAction, mutate func()) error {
	snap := pairCloneProgress(rt.Pair.Progress)
	saved := make([]basketAction, len(actions))
	for i, a := range actions {
		saved[i] = *a
	}
	mutate()
	rt.Pair.Progress.pruneNativeAttempts()
	rt.touchPairProgress()
	if err := bc.store.CommitPairNative(rt, rows, updates); err != nil {
		rt.Pair.Progress = snap
		for i, a := range actions {
			*a = saved[i]
		}
		return err
	}
	rt.Pair.resolutions = append(rt.Pair.resolutions, rows...)
	for _, r := range rows {
		if r.AlertStatus == pairNativeAlertQueued {
			rt.Pair.nativeAlerts = append(rt.Pair.nativeAlerts, r)
		}
	}
	return nil
}

func (bc *basketCoordinator) applyPairNativeHistory(rt *basketRuntime, m pairContractMember, t pairNativeTarget, req pairNativeHistoryRequest, res pairNativeHistoryResult, op *pairNativeOperatorRequest, now time.Time) string {
	ev := res.Evidence
	pres := pairNativeEvidenceResult(m, &ev, false, false, nil, now.UnixMilli())
	switch {
	case pres.Out.Outcome == basketVenueUnknown:
		return "the venue history evidence is not usable: " + pres.Out.Detail
	case pres.Unconfirmed != "":
		return "the venue history evidence is incomplete: " + pres.Unconfirmed
	case pres.Out.Outcome == basketVenueRejected:
		for _, o := range res.Orders {
			if q, err := pairCoverageRat("executed", o.ExecutedNativeQty, false); err != nil || q.Sign() != 0 || !o.Terminal {
				return fmt.Sprintf("order %s does not prove a terminal zero execution", o.OrderID)
			}
		}
		return bc.commitPairNativeZero(rt, t, ev, op, now)
	}
	if t.action == nil {
		return bc.convertPairNativeReceipt(rt, m, t, pres, ev, op, now)
	}
	return bc.recoverPairNativeAction(rt, m, t, req, pres, ev, op, now)
}

func (bc *basketCoordinator) commitPairNativeZero(rt *basketRuntime, t pairNativeTarget, ev nativeCloseEvidence, op *pairNativeOperatorRequest, now time.Time) string {
	if t.ob.Kind != pairObligationNativeUnknown {
		return "a zero-fill answer contradicts the confirmed fills of a native_fill_unconfirmed obligation"
	}
	a := t.action
	var actions []*basketAction
	var updates []pairNativeRowUpdate
	if a != nil {
		if a.ConfirmedQty > 1e-12 || a.AppliedQty > 1e-12 || strings.TrimSpace(a.FillCoverageJSON) != "" || strings.TrimSpace(a.SettlementJSON) != "" {
			return fmt.Sprintf("native action %s already holds confirmed or booked fills; a zero-fill answer never erases them", a.ActionID)
		}
		actions = append(actions, a)
		updates = append(updates, pairNativeRowUpdate{action: a, from: []string{basketActionHeld, basketActionUnresolved}, to: basketActionNoFill, zero: true, savedRaw: a.durableFills})
	}
	r := bc.newPairNativeResolution(rt, t.ob, t.attID, pairNativeModeZeroFill, op, now)
	if b, err := json.Marshal(ev); err == nil {
		r.Evidence = b
	}
	nowMs := now.UnixMilli()
	err := bc.commitPairNativeMutation(rt, []pairNativeResolution{r}, updates, actions, func() {
		p := &rt.Pair.Progress
		p.clearObligation(t.ob.Kind, t.ob.MemberID, t.ob.ActionID)
		if op != nil {
			p.removeNativeRequest(op.ID)
		}
		if a != nil {
			a.Status, a.Terminal = basketActionNoFill, true
			a.HoldReason = "a complete venue read proves every close order ended with no execution (resolution " + r.ID + ")"
			a.EvidenceJSON = marshalEntryJSON(basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: a.HoldReason, ObservedAtMs: nowMs})
			a.UpdatedAtMs = nowMs
		}
	})
	if err != nil {
		return "the zero-fill resolution did not save: " + err.Error()
	}
	bc.pairLogf(rt, "native %s for %s (%s) resolved as a proven zero fill by %s; no book changed", t.ob.Kind, t.ob.MemberID, t.ob.ActionID, r.DecidedBy)
	return ""
}

func (bc *basketCoordinator) convertPairNativeReceipt(rt *basketRuntime, m pairContractMember, t pairNativeTarget, pres pairNativeResult, ev nativeCloseEvidence, op *pairNativeOperatorRequest, now time.Time) string {
	if t.ob.Kind != pairObligationNativeUnknown || t.attID == "" {
		return "only an action-less native_fill_unknown attempt converts to a fill receipt"
	}
	p := &rt.Pair.Progress
	att := p.nativeAttempt(t.attID)
	if att == nil {
		return "the native attempt is no longer saved"
	}
	out := pres.Out
	if why := bc.pairNativeFillConflict(rt, &basketAction{StrategyID: m.MemberID}, out.FillCoverage); why != "" {
		return why
	}
	if why := pairNativeFillOwnedElsewhere(rt, &basketAction{StrategyID: m.MemberID}, out.FillCoverage); why != "" {
		return why
	}
	if why := rt.pairNativeResolvedFillOverlap(m.MemberID, out.FillCoverage); why != "" {
		return why
	}
	side, pos := att.OrderSide, att.PositionSide
	if side == "" {
		side, pos = pairNativeSideFromFills(&ev)
	}
	if side == "" {
		return "the close side is unknown"
	}
	if pos == "" {
		pos = entrySideLong
		if side == "buy" {
			pos = entrySideShort
		}
	}
	key := rt.Contract.pairMemberKey(m).String()
	id := pairNativeReceiptID(key, att.Purpose, att.Trigger, out)
	for _, r := range p.NativeReceipts {
		if r.ID == id {
			return "the fill receipt is already retained"
		}
	}
	nowMs := now.UnixMilli()
	rec := pairNativeReceipt{ID: id, Key: key, MemberID: m.MemberID, Purpose: att.Purpose, Trigger: att.Trigger, OrderSide: side, PositionSide: pos,
		ExecutedQty: pres.ExecutedQty, Outcome: out, CreatedAtMs: nowMs}
	rec.Hash = rec.computeHash()
	r := bc.newPairNativeResolution(rt, t.ob, t.attID, pairNativeModeVenueHistory, op, now)
	r.ReceiptID = id
	if b, err := json.Marshal(ev); err == nil {
		r.Evidence = b
	}
	for _, f := range out.FillCoverage {
		r.FillIDs = append(r.FillIDs, f.FillID)
	}
	err := bc.commitPairNativeMutation(rt, []pairNativeResolution{r}, nil, nil, func() {
		p := &rt.Pair.Progress
		p.NativeReceipts = append(p.NativeReceipts, rec)
		p.addObligation(pairObligation{Kind: pairObligationNativeReceipt, MemberID: m.MemberID, ActionID: id,
			Detail: fmt.Sprintf("a venue read confirmed native fills of %.10g for attempt %s; the coordinator attaches and books them without another order", out.CumQty, t.attID), AtMs: nowMs})
		p.clearObligation(t.ob.Kind, t.ob.MemberID, t.ob.ActionID)
		if a := p.nativeAttempt(t.attID); a != nil {
			a.ReceiptID = id
			a.Rev++
			a.LastError = ""
		}
		if op != nil {
			p.removeNativeRequest(op.ID)
		}
	})
	if err != nil {
		return "the fill receipt from the venue read did not save: " + err.Error()
	}
	bc.pairLogf(rt, "native attempt %s for %s: a complete venue read confirmed %.10g; receipt %s books once through the receipt owner", t.attID, m.MemberID, out.CumQty, id)
	return ""
}

func pairNativeRecoveredAction(m pairContractMember, a *basketAction, out basketVenueOutcome) (basketAction, string) {
	next := *a
	if why := pairLiveLegacyReason(a); why != "" {
		return next, "the native action is a legacy action: " + why
	}
	ev, why := pairEvaluateExecution(a, out)
	if why != "" {
		return next, "the venue execution records conflict with the saved ones: " + why
	}
	verdict, fees, reason := pairEvaluateFees(a, m, out, ev.fills)
	if a.FeeFinal && ev.grew {
		return next, "new execution records arrived after the fee set was final"
	}
	switch verdict {
	case pairFeeConflict, pairFeeInvalid:
		return next, "the venue fee records are refused: " + reason
	}
	next.FillCoverageJSON = ev.raw
	if next.AcceptedAtMs == 0 && out.AcceptedAtMs > 0 {
		next.AcceptedAtMs = out.AcceptedAtMs
	}
	if verdict == pairFeeAccept {
		cur, amt := pairFeeTotals(fees)
		next.FeeCoverageJSON = pairMarshalRecords(fees, len(fees))
		next.FeePresent, next.FeeCurrency, next.FeeAmount, next.ConfirmedFee = true, cur, amt, amt
		next.FeeEvidenceID, next.FeeEvidenceVersion = out.FeeEvidenceID, int64(len(fees))
	}
	if out.FeeFinal && !next.FeeFinal && (verdict == pairFeeAccept || verdict == pairFeeSame) {
		accepted, _ := pairParseFees(next.FeeCoverageJSON)
		if pairExecutionComplete(ev.fills, out.CumQty, out.CumNotional) && pairFeeCoversAll(accepted, ev.fills) {
			next.FeeFinal = true
		}
	}
	next.ConfirmedQty, next.ConfirmedNotional = out.CumQty, out.CumNotional
	if next.ConfirmedQty > 0 {
		next.ConfirmedAvgPx = next.ConfirmedNotional / next.ConfirmedQty
	}
	next.Terminal = true
	if next.VenueOrderID == "" && out.VenueOrderID != "" {
		next.VenueOrderID = out.VenueOrderID
	}
	next.EvidenceJSON = marshalEntryJSON(out)
	next.HoldReason = ""
	next.Status = basketActionSettling
	return next, ""
}

func (bc *basketCoordinator) recoverPairNativeAction(rt *basketRuntime, m pairContractMember, t pairNativeTarget, req pairNativeHistoryRequest, pres pairNativeResult, ev nativeCloseEvidence, op *pairNativeOperatorRequest, now time.Time) string {
	a := t.action
	if !pairNativeRereadable(rt, a) {
		return fmt.Sprintf("native action %s is %s; recovery settles only a held or unresolved native action, or a recovered one whose booking still waits for evidence", a.ActionID, a.Status)
	}
	prior := a.Status
	out := pres.Out
	pairNativeKeepVisible(a.EvidenceJSON, &out)
	if basketQtyEqual(out.CumQty, a.RequestedQty) {
		out.Outcome = basketVenueFilled
		out.CumQty = a.RequestedQty
	}
	if out.FeePresent {
		out.FeeEvidenceID = pairFeeEvidenceID(a)
	}
	if why := pairOutcomeError(a, out); why != "" {
		return "the venue answer is inconsistent with the saved native action: " + why
	}
	if why := bc.pairNativeFillConflict(rt, a, out.FillCoverage); why != "" {
		return why
	}
	if why := pairNativeFillOwnedElsewhere(rt, a, out.FillCoverage); why != "" {
		return why
	}
	if why := rt.pairNativeResolvedFillOverlap(a.StrategyID, out.FillCoverage); why != "" {
		return why
	}
	next, why := pairNativeRecoveredAction(m, a, out)
	if why != "" {
		return why
	}
	if next.FillCoverageJSON == a.FillCoverageJSON && next.FeeCoverageJSON == a.FeeCoverageJSON && next.FeeFinal == a.FeeFinal && basketQtyEqual(next.ConfirmedQty, a.ConfirmedQty) {
		return "the venue read adds no fill, fee or finality evidence to the saved action"
	}
	nowMs := now.UnixMilli()
	next.UpdatedAtMs = nowMs
	fills, _ := pairParseFills(next.FillCoverageJSON)
	r := bc.newPairNativeResolution(rt, t.ob, t.attID, pairNativeModeVenueHistory, op, now)
	r.ActionID = a.ActionID
	if b, err := json.Marshal(ev); err == nil {
		r.Evidence = b
	}
	attID := t.attID
	if attID == "" {
		attID = req.AttemptID
	}
	r.AttemptID = attID
	savedFills := a.durableFills
	err := bc.commitPairNativeMutation(rt, nil, []pairNativeRowUpdate{{action: a, from: []string{prior}, to: basketActionSettling, fills: fills, savedRaw: savedFills}},
		[]*basketAction{a}, func() {
			p := &rt.Pair.Progress
			*a = next
			att := p.nativeAttempt(attID)
			if att == nil {
				p.NativeAttempts = append(p.NativeAttempts, pairNativeAttempt{ID: attID, MemberID: m.MemberID, Key: rt.Contract.pairMemberKey(m).String(), Provider: m.Provider,
					AccountKey: m.AccountKey, Instrument: m.Instrument, ContractFP: rt.Contract.Fingerprint(), Purpose: a.Purpose, OrderSide: t.side, PositionSide: a.PositionSide,
					ActionID: a.ActionID, SentAtMs: pairNativeStartMs(a), EndedAtMs: pairNativeStartMs(a), DeadlineMs: a.AcceptDeadlineMs, VenueOrderIDs: t.orders, OrdersComplete: true,
					Outcome: out.Outcome, Provenance: pairNativeProvenanceOperator, Rev: 1})
				att = &p.NativeAttempts[len(p.NativeAttempts)-1]
			}
			att.Rev++
			att.LastError = ""
			att.RetryAtMs = 0
			if next, ok := pairAddMs(nowMs, pairNativeRetryDelay(rt, 0)); ok {
				att.RetryAtMs = next
			}
			pending := r
			pending.AlertStatus = pairNativeAlertQueued
			att.Pending = &pending
			if op != nil {
				p.removeNativeRequest(op.ID)
			}
		})
	if err != nil {
		return "the native recovery transition did not save: " + err.Error()
	}
	bc.pairLogf(rt, "native action %s for %s: a complete venue read confirmed %.10g (%d fill records); it settles once through the settlement ledger and the %s obligation ends when the book commits",
		a.ActionID, a.StrategyID, a.ConfirmedQty, len(fills), t.ob.Kind)
	return ""
}

func pairNativeRereadable(rt *basketRuntime, a *basketAction) bool {
	switch a.Status {
	case basketActionHeld, basketActionUnresolved:
		return true
	case basketActionSettling:
		return rt.Pair.Progress.nativePendingFor(a.ActionID) && !pairLiveCompleteFor(rt, a)
	}
	return false
}

func pairLiveCompleteFor(rt *basketRuntime, a *basketAction) bool {
	if !a.FeeFinal || pairLiveLegacyReason(a) != "" {
		return false
	}
	if rt != nil && rt.isPair() && rt.Pair.Progress.obligation(pairObligationFeeConflict, a.StrategyID, a.ActionID) != nil {
		return false
	}
	pl, err := pairBuildSettlePlan(a)
	if err != nil || !pairExecutionComplete(pl.exec, a.ConfirmedQty, a.ConfirmedNotional) || !pairFeeCoversAll(pl.fees, pl.exec) {
		return false
	}
	applied := map[string]bool{}
	for _, f := range pl.ledger.Fees {
		applied[f.FillID] = true
	}
	for _, e := range pl.exec {
		if _, ok := pl.booked[e.FillID]; !ok || !applied[e.FillID] {
			return false
		}
	}
	return true
}

func pairNativeFinalizable(rt *basketRuntime, books map[string]*StrategyState) []pairNativeResolution {
	if rt == nil || rt.Pair == nil || len(books) == 0 {
		return nil
	}
	p := &rt.Pair.Progress
	var out []pairNativeResolution
	for _, att := range p.NativeAttempts {
		if att.Pending == nil || books[att.MemberID] == nil {
			continue
		}
		r := *att.Pending
		a := rt.Actions[r.ActionID]
		if a == nil || a.StrategyID != att.MemberID || a.Status != basketActionCommitted || a.ConfirmedQty-a.AppliedQty > 1e-12 || !pairLiveCompleteFor(rt, a) {
			continue
		}
		if p.obligation(r.Kind, r.MemberID, r.ObligationID) == nil || rt.pairNativeResolution(r.Kind, r.MemberID, r.ObligationID) != nil {
			continue
		}
		r.CommittedAtMs = rt.auditNowMs()
		r.BookedQty = a.AppliedQty
		r.FillIDs = nil
		if l, err := pairParseLedger(a.SettlementJSON); err == nil {
			for _, f := range l.Fills {
				r.FillIDs = append(r.FillIDs, f.FillID)
			}
		}
		out = append(out, r)
	}
	return out
}

func pairNativeProgressAfter(p pairProgress, fin []pairNativeResolution) pairProgress {
	q := p
	q.Obligations = append([]pairObligation(nil), p.Obligations...)
	q.NativeAttempts = append([]pairNativeAttempt(nil), p.NativeAttempts...)
	for _, r := range fin {
		q.clearObligation(r.Kind, r.MemberID, r.ObligationID)
	}
	q.pruneNativeAttempts()
	return q
}

func applyPairNativeFinalized(rt *basketRuntime) {
	if rt == nil || rt.Pair == nil || len(rt.Pair.finalizing) == 0 {
		return
	}
	fin := rt.Pair.finalizing
	rt.Pair.finalizing = nil
	rt.Pair.Progress = pairNativeProgressAfter(rt.Pair.Progress, fin)
	rt.Pair.resolutions = append(rt.Pair.resolutions, fin...)
	rt.Pair.nativeAlerts = append(rt.Pair.nativeAlerts, fin...)
	rt.touchPairProgress()
}

func (bc *basketCoordinator) flushPairNativeAlerts(rt *basketRuntime) {
	if len(rt.Pair.nativeAlerts) == 0 {
		return
	}
	alerts := rt.Pair.nativeAlerts
	rt.Pair.nativeAlerts = nil
	var ids []string
	for _, r := range alerts {
		critical := r.DecidedBy == pairNativeDecidedByOperator
		label := "the coordinator"
		if critical {
			label = "operator " + r.Operator
		}
		msg := fmt.Sprintf("native %s for %s (%s) is resolved by %s as %s (resolution %s, revision %s)", r.Kind, r.MemberID, r.ObligationID, label, r.Mode, r.ID, r.Revision)
		if r.Reason != "" {
			msg += "; reason: " + r.Reason
		}
		if r.BookedQty > 0 {
			msg += fmt.Sprintf("; %.10g booked once through the settlement ledger", r.BookedQty)
		}
		if r.FundingHoldFromMs > 0 {
			msg += fmt.Sprintf("; funding stays incomplete for %s from %d", r.MemberID, r.FundingHoldFromMs)
		}
		if r.Mode == pairNativeModeWriteOff || r.Mode == pairNativeModeAcknowledge {
			msg += "; no book changed, venue flatness is not asserted and the kill switch is unchanged"
		}
		if critical {
			msg = "**PAIR NATIVE DECISION** " + msg
		}
		bc.emit(rt, msg, critical)
		ids = append(ids, r.ID)
	}
	if err := bc.store.MarkPairNativeAlerts(rt, ids, pairNativeAlertDispatched); err != nil {
		bc.pairLogf(rt, "native resolution alerts were handed to the notifier, but their dispatch state did not save (%v); a restart never resends them", err)
	}
}

func (bc *basketCoordinator) pairNativeRequestStale(rt *basketRuntime, req pairNativeOperatorRequest) string {
	p := &rt.Pair.Progress
	ob := p.obligation(req.Kind, req.MemberID, req.ObligationID)
	switch {
	case rt.pairNativeResolution(req.Kind, req.MemberID, req.ObligationID) != nil:
		return "the obligation already has a committed resolution"
	case ob == nil:
		return "the obligation is no longer open"
	case pairNativeObligationRevision(rt, *ob) != req.Revision:
		return "the obligation evidence changed after the decision was staged (stale revision)"
	case req.Decision != pairNativeDecisionRead && p.nativePendingResolution(req.Kind, req.MemberID, req.ObligationID) != nil:
		return "a recovered booking for the obligation waits for its book to commit; it ends the obligation itself"
	}
	if bc.pairNativeReqGen == nil {
		bc.pairNativeReqGen = map[string]uint64{}
	}
	gen, ok := bc.pairNativeReqGen[req.ID]
	if !ok {
		bc.pairNativeReqGen[req.ID] = bc.pairNativeGenOf(req.MemberID)
	} else if gen != bc.pairNativeGenOf(req.MemberID) {
		return "a native close for the member ran after the decision was staged; the close keeps its authority and the decision is void"
	}
	return ""
}

func (bc *basketCoordinator) dropPairNativeRequest(rt *basketRuntime, req pairNativeOperatorRequest, why string, now time.Time) {
	if !rt.Pair.Progress.removeNativeRequest(req.ID) {
		return
	}
	delete(bc.pairNativeReqGen, req.ID)
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairLogf(rt, "operator native decision %s (%s %s %s) is not applied: %s", req.ID, req.Decision, req.Kind, req.ObligationID, why)
	bc.pairAlert(rt, "native-decision-void|"+req.ID, fmt.Sprintf("operator native decision %s (%s on %s %s for %s) is not applied: %s; nothing changed", req.ID, req.Decision, req.Kind, req.ObligationID, req.MemberID, why), false)
}

func (bc *basketCoordinator) applyPairNativeRequests(rt *basketRuntime, in basketCycleInputs) bool {
	p := &rt.Pair.Progress
	if len(p.NativeRequests) == 0 {
		return false
	}
	read := false
	for _, req := range append([]pairNativeOperatorRequest(nil), p.NativeRequests...) {
		if why := bc.pairNativeRequestStale(rt, req); why != "" {
			bc.dropPairNativeRequest(rt, req, why, in.Now)
			continue
		}
		if bc.pairNativeBusy(rt, req.MemberID) != "" {
			continue
		}
		switch req.Decision {
		case pairNativeDecisionWriteOff, pairNativeDecisionAck:
			if why := bc.commitPairNativeDecision(rt, req, in.Now); why != "" {
				bc.pairLogf(rt, "operator native decision %s did not commit: %s; it stays staged and retries", req.ID, why)
			}
		case pairNativeDecisionRead:
			if read {
				continue
			}
			read = true
			ob := rt.Pair.Progress.obligation(req.Kind, req.MemberID, req.ObligationID)
			if ob == nil {
				continue
			}
			op := req
			t, why := bc.pairNativeTargetFor(rt, *ob, req.Orders)
			if why == "" {
				why = bc.readPairNative(rt, t, &op, in)
			}
			if why != "" {
				bc.dropPairNativeRequest(rt, req, "the venue read did not resolve it: "+why, in.Now)
			} else {
				delete(bc.pairNativeReqGen, req.ID)
			}
		default:
			bc.dropPairNativeRequest(rt, req, "unknown decision", in.Now)
		}
	}
	return read
}

func (bc *basketCoordinator) commitPairNativeDecision(rt *basketRuntime, req pairNativeOperatorRequest, now time.Time) string {
	p := &rt.Pair.Progress
	obp := p.obligation(req.Kind, req.MemberID, req.ObligationID)
	if obp == nil {
		return "the obligation is no longer open"
	}
	ob := *obp
	mode := pairNativeModeWriteOff
	if req.Decision == pairNativeDecisionAck {
		mode = pairNativeModeAcknowledge
	}
	attID := ""
	if att := p.nativeAttemptFor(ob); att != nil {
		attID = att.ID
	}
	op := req
	r := bc.newPairNativeResolution(rt, ob, attID, mode, &op, now)
	if att := p.nativeAttempt(attID); att != nil {
		r.Evidence = att.Evidence
		var ev nativeCloseEvidence
		if json.Unmarshal(att.Evidence, &ev) == nil {
			for _, f := range ev.Fills {
				r.FillIDs = append(r.FillIDs, f.FillID)
			}
		}
	}
	for _, rc := range p.NativeReceipts {
		if rc.ID == ob.ActionID && rc.MemberID == ob.MemberID {
			for _, f := range rc.Outcome.FillCoverage {
				r.FillIDs = append(r.FillIDs, f.FillID)
			}
		}
	}
	sort.Strings(r.FillIDs)
	r.FillIDs = dedupeSortedStrings(r.FillIDs)
	if pairNativeHorizonKind(ob.Kind) && ob.FromMs > 0 {
		r.FundingHoldFromMs = ob.FromMs
	}
	nowMs := now.UnixMilli()
	var actions []*basketAction
	a := rt.Actions[ob.ActionID]
	if a != nil && (a.StrategyID != ob.MemberID || !a.pairNative()) {
		a = nil
	}
	if a != nil {
		actions = append(actions, a)
	}
	err := bc.commitPairNativeMutation(rt, []pairNativeResolution{r}, nil, actions, func() {
		p := &rt.Pair.Progress
		p.clearObligation(ob.Kind, ob.MemberID, ob.ActionID)
		p.removeNativeRequest(req.ID)
		if a != nil && a.Status == basketActionUnresolved {
			a.Status, a.Terminal = basketActionHeld, true
			a.HoldReason = fmt.Sprintf("operator %s recorded a %s for %s (resolution %s): %s; nothing is booked from this action", req.Operator, mode, ob.Kind, r.ID, req.Reason)
			a.UpdatedAtMs = nowMs
		}
		if ob.Kind == pairObligationNativeConflict {
			kept := p.NativeReceipts[:0:0]
			for _, rc := range p.NativeReceipts {
				if rc.ID == ob.ActionID && rc.MemberID == ob.MemberID {
					p.clearObligation(pairObligationNativeReceipt, rc.MemberID, rc.ID)
					continue
				}
				kept = append(kept, rc)
			}
			p.NativeReceipts = kept
		}
		if r.FundingHoldFromMs > 0 {
			p.NativeFundingHolds = append(p.NativeFundingHolds, pairNativeFundingHold{MemberID: ob.MemberID, Kind: ob.Kind, ObligationID: ob.ActionID, ResolutionID: r.ID,
				FromMs: ob.FromMs, AtMs: nowMs, Detail: fmt.Sprintf("the operator %s left the inventory from %d unproven; funding after it stays incomplete", mode, ob.FromMs)})
		}
	})
	if err != nil {
		if errors.Is(err, errPairNativeResolved) {
			bc.dropPairNativeRequest(rt, req, "the obligation already has a committed resolution", now)
			return ""
		}
		return err.Error()
	}
	delete(bc.pairNativeReqGen, req.ID)
	bc.pairLogf(rt, "operator %s committed a %s for native %s %s (%s); resolution %s; no book changed", req.Operator, mode, ob.Kind, ob.MemberID, ob.ActionID, r.ID)
	return ""
}

func pairNativeOperatorRequestID(rt *basketRuntime, cmd pairNativeOperatorCommand) string {
	orders := append([]string(nil), cmd.Orders...)
	sort.Strings(orders)
	return "pnq-" + pairSHA256([]byte(strings.Join([]string{rt.Partition.String(), rt.Contract.BasketID, cmd.Kind, cmd.MemberID, cmd.ObligationID, cmd.Revision,
		cmd.Decision, cmd.Invoker, cmd.Reason, strings.Join(orders, ",")}, "|")))[:32]
}

func (bc *basketCoordinator) pairNativeOperator(cmd pairNativeOperatorCommand, stage bool) (string, bool) {
	if bc == nil {
		return "no pair coordinator is running", false
	}
	if stage {
		bc.mu.Lock()
		defer bc.mu.Unlock()
	} else {
		bc.mu.RLock()
		defer bc.mu.RUnlock()
	}
	rt := bc.runtimes[cmd.PairID]
	if rt == nil || !rt.isPair() {
		return fmt.Sprintf("refused: pair %q is not a running pair", cmd.PairID), false
	}
	if cmd.Partition != rt.Partition.String() {
		return fmt.Sprintf("refused: pair %s belongs to partition %s, not %q", cmd.PairID, rt.Partition.String(), cmd.Partition), false
	}
	if _, ok := rt.Contract.pairMember(cmd.MemberID); !ok {
		return fmt.Sprintf("refused: %q is not a member of pair %s", cmd.MemberID, cmd.PairID), false
	}
	if cmd.Decision == pairNativeDecisionShow {
		return bc.describePairNative(rt, cmd), true
	}
	if why := bc.pairLiveRefusal(rt); why != "" {
		return "refused: " + why + "; no coordinator step applies a decision on this host", false
	}
	if !pairNativeResolvableKind(cmd.Kind) {
		return fmt.Sprintf("refused: %q is not a native obligation an operator resolves (allowed: %s, %s, %s, %s, %s)", cmd.Kind, pairObligationNativeUnknown,
			pairObligationNativeUnconfirmed, pairObligationNativeConflict, pairObligationNativeAmbiguous, pairObligationNativeLegacy), false
	}
	p := &rt.Pair.Progress
	if r := rt.pairNativeResolution(cmd.Kind, cmd.MemberID, cmd.ObligationID); r != nil {
		return fmt.Sprintf("refused: this obligation is already resolved by %s (%s by %s at %s); no second decision is applied", r.ID, r.Mode, r.DecidedBy, formatClosedBarMs(r.CommittedAtMs)), false
	}
	obp := p.obligation(cmd.Kind, cmd.MemberID, cmd.ObligationID)
	if obp == nil {
		return fmt.Sprintf("refused: pair %s has no open %s obligation %q for %s", cmd.PairID, cmd.Kind, cmd.ObligationID, cmd.MemberID), false
	}
	ob := *obp
	if !pairNativeDecisionAllowed(cmd.Kind, cmd.Decision) {
		return fmt.Sprintf("refused: %s permits only %s", cmd.Kind, strings.Join(pairNativeDecisionsFor(cmd.Kind), " or ")), false
	}
	if rev := pairNativeObligationRevision(rt, ob); cmd.Revision != rev {
		return fmt.Sprintf("refused: revision %q is stale; the current revision is %s (run the command with decision show to review it)", cmd.Revision, rev), false
	}
	if strings.TrimSpace(cmd.Reason) == "" || len(cmd.Reason) > pairNativeReasonMax || !pairPrintableReason(cmd.Reason) {
		return fmt.Sprintf("refused: a decision needs a printable reason of 1..%d characters", pairNativeReasonMax), false
	}
	if len(cmd.Orders) > 0 && cmd.Decision != pairNativeDecisionRead {
		return "refused: order ids apply only to a venue-read decision", false
	}
	if why := bc.pairNativeBusy(rt, cmd.MemberID); why != "" {
		return "refused: " + why + "; retry after it ends", false
	}
	if att := p.nativePendingResolution(cmd.Kind, cmd.MemberID, cmd.ObligationID); att != nil && cmd.Decision != pairNativeDecisionRead {
		return fmt.Sprintf("refused: native attempt %s already holds a recovered booking for this obligation that waits for its book to commit; it ends the obligation through the settlement ledger, so no write-off or acknowledgment is applied (a venue-read can add the missing fee evidence)", att.ID), false
	}
	id := pairNativeOperatorRequestID(rt, cmd)
	if staged := p.nativeRequestFor(cmd.Kind, cmd.MemberID, cmd.ObligationID); staged != nil {
		if staged.ID == id {
			return fmt.Sprintf("already staged as %s; it applies at the next coordinator step and is never applied twice", id), false
		}
		return fmt.Sprintf("refused: decision %s (%s) is already staged for this obligation", staged.ID, staged.Decision), false
	}
	if cmd.Decision == pairNativeDecisionRead {
		if _, why := bc.pairNativeTargetFor(rt, ob, cmd.Orders); why != "" {
			return "refused: " + why, false
		}
		m, _ := rt.Contract.pairMember(cmd.MemberID)
		if bc.pairNativeHistoryReaderFor(m) == nil {
			return "refused: this host has no private venue history reader for the member", false
		}
	}
	summary := fmt.Sprintf("%s on pair %s (%s) member %s: %s %s, revision %s. Reason: %s.", cmd.Decision, cmd.PairID, cmd.Partition, cmd.MemberID, cmd.Kind, cmd.ObligationID, cmd.Revision, cmd.Reason)
	switch cmd.Decision {
	case pairNativeDecisionWriteOff:
		summary += " A write-off books nothing, does not assert the venue is flat, keeps the kill switch and keeps funding incomplete for any unproven interval."
	case pairNativeDecisionAck:
		summary += " An acknowledgment books nothing and never books a historical quantity again."
	case pairNativeDecisionRead:
		summary += " The coordinator reads the venue history once and books only proven fills through the settlement ledger."
	}
	if cmd.Decision != pairNativeDecisionRead && pairNativeHorizonKind(ob.Kind) && ob.FromMs > 0 {
		summary += fmt.Sprintf(" It adds a funding hold for %s from %d: later funding of that member is never booked, so pair increases hold while the hold exists (the pair can still finish its flat reset, exit and protect).", cmd.MemberID, ob.FromMs)
	}
	if !stage {
		return summary, true
	}
	req := pairNativeOperatorRequest{ID: id, MemberID: cmd.MemberID, Kind: cmd.Kind, ObligationID: cmd.ObligationID, Revision: cmd.Revision, Decision: cmd.Decision,
		Operator: cmd.Invoker, Reason: cmd.Reason, Orders: append([]string(nil), cmd.Orders...), StagedAtMs: bc.pairWallNow().UnixMilli()}
	sort.Strings(req.Orders)
	p.NativeRequests = append(p.NativeRequests, req)
	rt.touchPairProgress()
	if !bc.persistPair(rt) {
		p.removeNativeRequest(id)
		rt.touchPairProgress()
		return "refused: the decision could not be saved, so nothing is staged; retry later", false
	}
	if bc.pairNativeReqGen == nil {
		bc.pairNativeReqGen = map[string]uint64{}
	}
	bc.pairNativeReqGen[id] = bc.pairNativeGenOf(cmd.MemberID)
	return fmt.Sprintf("Staged decision %s: %s It applies at the next coordinator step; a CRITICAL alert reports the committed result, and a newer native close or changed evidence voids it.", id, summary), true
}

func pairPrintableReason(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (bc *basketCoordinator) describePairNative(rt *basketRuntime, cmd pairNativeOperatorCommand) string {
	p := &rt.Pair.Progress
	var lines []string
	for _, o := range p.Obligations {
		if o.MemberID != cmd.MemberID || !pairNativeResolvableKind(o.Kind) {
			continue
		}
		if (cmd.Kind != "" && o.Kind != cmd.Kind) || (cmd.ObligationID != "" && o.ActionID != cmd.ObligationID) {
			continue
		}
		line := fmt.Sprintf("- %s %s revision %s (decisions: %s): %s", o.Kind, o.ActionID, pairNativeObligationRevision(rt, o), strings.Join(pairNativeDecisionsFor(o.Kind), ", "), o.Detail)
		if o.FromMs > 0 {
			line += fmt.Sprintf("; unknown from %d", o.FromMs)
		}
		if att := p.nativeAttemptFor(o); att != nil {
			line += fmt.Sprintf("; attempt %s orders %v complete=%v side %s, %d reads, last: %s", att.ID, att.VenueOrderIDs, att.OrdersComplete, att.OrderSide, att.RetryCount, att.LastError)
		}
		if a := p.nativePendingResolution(o.Kind, o.MemberID, o.ActionID); a != nil {
			line += fmt.Sprintf("; recovered booking %s for action %s waits for its book to commit", a.Pending.ID, a.Pending.ActionID)
		}
		if r := p.nativeRequestFor(o.Kind, o.MemberID, o.ActionID); r != nil {
			line += fmt.Sprintf("; staged decision %s (%s)", r.ID, r.Decision)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	for _, r := range rt.Pair.resolutions {
		if r.MemberID == cmd.MemberID && (cmd.Kind == "" || r.Kind == cmd.Kind) && (cmd.ObligationID == "" || r.ObligationID == cmd.ObligationID) {
			lines = append(lines, fmt.Sprintf("- resolved %s %s by %s as %s (%s)", r.Kind, r.ObligationID, r.DecidedBy, r.Mode, r.ID))
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Pair %s (%s) member %s has no open native obligation that matches.", rt.Contract.BasketID, rt.Partition.String(), cmd.MemberID)
	}
	return fmt.Sprintf("Pair %s (%s) member %s native obligations:\n%s", rt.Contract.BasketID, rt.Partition.String(), cmd.MemberID, strings.Join(lines, "\n"))
}

func pairNativeFundingHoldFromMs(rt *basketRuntime, memberID string) int64 {
	var from int64
	for _, h := range rt.Pair.Progress.NativeFundingHolds {
		if h.MemberID == memberID && h.FromMs > 0 && (from == 0 || h.FromMs < from) {
			from = h.FromMs
		}
	}
	return from
}

func pairNativeRecoveryAccounted(p *pairProgress) bool {
	if len(p.NativeRequests) > 0 {
		return false
	}
	for _, a := range p.NativeAttempts {
		if a.Pending != nil {
			return false
		}
	}
	return true
}
