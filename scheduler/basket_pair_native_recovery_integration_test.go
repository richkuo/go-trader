package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type pairNativeAnswer struct {
	res pairNativeHistoryResult
	err error
}

type pairNativeRig struct {
	t       *testing.T
	f       *pairKillSwitchFixture
	e       *pairTestEnv
	mu      sync.Mutex
	answers map[string][]pairNativeAnswer
	reads   []pairNativeHistoryRequest
	during  func(req pairNativeHistoryRequest)
	mock    *mockNotifier
}

func newPairNativeRig(t *testing.T) *pairNativeRig {
	t.Helper()
	f := newPairKillSwitchFixture(t)
	r := &pairNativeRig{t: t, f: f, e: f.env, answers: map[string][]pairNativeAnswer{}}
	r.install()
	return r
}

func (r *pairNativeRig) install() {
	r.mock = &mockNotifier{}
	r.e.bc.notifier = NewMultiNotifier(notifierBackend{notifier: r.mock, ownerID: "owner"})
	r.e.bc.pairNativeHistory = r.read
}

func (r *pairNativeRig) restart() {
	r.e.restart()
	r.install()
}

func (r *pairNativeRig) answer(member string, a pairNativeAnswer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[member] = append(r.answers[member], a)
}

func (r *pairNativeRig) read(ctx context.Context, m pairContractMember, req pairNativeHistoryRequest) (pairNativeHistoryResult, error) {
	r.mu.Lock()
	r.reads = append(r.reads, req)
	q := r.answers[m.MemberID]
	during := r.during
	var a pairNativeAnswer
	ok := len(q) > 0
	if ok {
		a, r.answers[m.MemberID] = q[0], q[1:]
	}
	r.mu.Unlock()
	if during != nil {
		during(req)
	}
	if !ok {
		return pairNativeHistoryResult{}, fmt.Errorf("the fixture has no scripted venue history for %s", m.MemberID)
	}
	return a.res, a.err
}

func (r *pairNativeRig) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reads)
}

func pairNativeHistoryOf(ev *nativeCloseEvidence, side, instrument string) pairNativeHistoryResult {
	res := pairNativeHistoryResult{Complete: true, Evidence: *ev}
	for _, id := range ev.VenueOrderIDs {
		res.Orders = append(res.Orders, pairNativeHistoryOrder{OrderID: id, State: "filled", Terminal: true, Side: side, Instrument: instrument, ExecutedNativeQty: ev.ExecutedNativeQty})
	}
	return res
}

func pairNativeZeroHistory(oid, side, instrument string, atMs int64) pairNativeHistoryResult {
	ev := nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{oid}, AcceptedAtMs: atMs, ExecutedNativeQty: "0", Complete: true}
	return pairNativeHistoryResult{Complete: true, Evidence: ev, Orders: []pairNativeHistoryOrder{{OrderID: oid, State: "canceled", Terminal: true, Side: side, Instrument: instrument, ExecutedNativeQty: "0", AcceptedAtMs: atMs}}}
}

func pairNativeLostFills(oid, executed, detail string, closeErr bool) *nativeCloseEvidence {
	complete := true
	ev := &nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{oid}, ExecutedNativeQty: executed, Detail: detail, OrdersComplete: &complete}
	if closeErr {
		ev.ExecutedNativeQty = ""
	}
	return ev
}

func (r *pairNativeRig) fireHL(ev *nativeCloseEvidence, err error) {
	r.t.Helper()
	r.f.hlOut = func() (*HyperliquidCloseResult, error) { return pairHLNativeResult(ev, 59900), err }
	if _, cerr := r.e.bc.pairKillSwitchHLCloser(r.f.closers.hl)("BTC", nil, nil); (cerr != nil) != (err != nil) {
		r.t.Fatalf("the native closer must return its own error unchanged: %v", cerr)
	}
}

func (r *pairNativeRig) fireOKX(ev *nativeCloseEvidence, err error) {
	r.t.Helper()
	r.f.okxOut = func() (*OKXCloseResult, error) { return pairOKXNativeResult(ev, 60050), err }
	if _, cerr := r.e.bc.pairKillSwitchOKXCloser(r.f.closers.okx)("BTC", nil); (cerr != nil) != (err != nil) {
		r.t.Fatalf("the native closer must return its own error unchanged: %v", cerr)
	}
}

func (r *pairNativeRig) obligation(kind, member string) *pairObligation {
	r.e.mu.RLock()
	defer r.e.mu.RUnlock()
	for _, o := range r.e.rt().Pair.Progress.Obligations {
		if o.Kind == kind && o.MemberID == member {
			cp := o
			return &cp
		}
	}
	return nil
}

func (r *pairNativeRig) attempts(member string) []pairNativeAttempt {
	r.e.mu.RLock()
	defer r.e.mu.RUnlock()
	var out []pairNativeAttempt
	for _, a := range r.e.rt().Pair.Progress.NativeAttempts {
		if a.MemberID == member {
			out = append(out, a)
		}
	}
	return out
}

func (r *pairNativeRig) savedResolutions(member string) []pairNativeResolution {
	r.t.Helper()
	rows, err := r.e.db().db.Query(`SELECT record_json, alert_status FROM pair_native_resolutions WHERE pair_id = ? AND strategy_id = ? ORDER BY committed_at_ms, resolution_id`, pairTestID, member)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []pairNativeResolution
	for rows.Next() {
		var raw, alert string
		if err := rows.Scan(&raw, &alert); err != nil {
			r.t.Fatal(err)
		}
		var res pairNativeResolution
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			r.t.Fatal(err)
		}
		res.AlertStatus = alert
		out = append(out, res)
	}
	return out
}

func (r *pairNativeRig) savedProgressValue() pairProgress {
	r.t.Helper()
	var p pairProgress
	if err := json.Unmarshal([]byte(r.e.savedProgress()), &p); err != nil {
		r.t.Fatalf("saved pair progress is unreadable: %v", err)
	}
	return p
}

const pairNativeTestOwner = "owner-1927"

func (r *pairNativeRig) closeQty(member string) (float64, float64, int) {
	qty, fee, n := 0.0, 0.0, 0
	for _, tr := range r.e.savedTrades(member) {
		if tr.IsClose {
			qty += tr.Qty
			fee += tr.Fee
			n++
		}
	}
	return qty, fee, n
}

func (r *pairNativeRig) nativeAction(member string) *basketAction {
	var last *basketAction
	for _, p := range []string{pairPurposeKillSwitch, pairPurposeOperator} {
		for _, a := range r.e.actionsByPurpose(p) {
			if a.StrategyID == member {
				last = a
			}
		}
	}
	return last
}

func (r *pairNativeRig) decisionDMs() int {
	return countDMs(r.mock, "PAIR NATIVE DECISION")
}

func (r *pairNativeRig) step(d time.Duration) {
	r.e.step(r.e.now.Add(d))
}

func TestBasketPairNativeRecoveryRateLimitThenCompleteReadBooksOnce(t *testing.T) {
	for _, tc := range []struct {
		name, member, side, instrument, oid, px, fee, ccy string
		sizes                                             []string
		fire                                              func(r *pairNativeRig, ev *nativeCloseEvidence)
		wantBook                                          float64
	}{
		{name: "hyperliquid", member: pairTestHL, side: "sell", instrument: "BTC", oid: "951", px: "59900", fee: "0.9", ccy: "USDC", sizes: []string{"0.02", "0.04"},
			fire: func(r *pairNativeRig, ev *nativeCloseEvidence) { r.fireHL(ev, nil) }},
		{name: "okx", member: pairTestOKX, side: "buy", instrument: "BTC-USDT-SWAP", oid: "851", px: "60050", fee: "0.3", ccy: "USDT", sizes: []string{"2", "4"},
			fire: func(r *pairNativeRig, ev *nativeCloseEvidence) { r.fireOKX(ev, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairNativeRig(t)
			e := r.e
			sentAt := e.nativeAt()
			full := pairNativeEvidence(tc.oid, tc.side, sentAt, tc.px, tc.fee, tc.ccy, tc.sizes...)
			lost := pairNativeLostFills(tc.oid, full.ExecutedNativeQty, "the user fill lookup was rate limited (429)", false)
			before := r.f.hlCalls + r.f.okxCalls
			tc.fire(r, lost)
			ob := r.obligation(pairObligationNativeUnknown, tc.member)
			atts := r.attempts(tc.member)
			if ob == nil || len(atts) != 1 || !atts[0].OrdersComplete || len(atts[0].VenueOrderIDs) != 1 || atts[0].VenueOrderIDs[0] != tc.oid || atts[0].ActionID != ob.ActionID {
				t.Fatalf("a rate-limited fill lookup must keep the unknown obligation and its order identity: %+v %+v", ob, atts)
			}
			r.restart()
			if atts := r.attempts(tc.member); len(atts) != 1 || atts[0].VenueOrderIDs[0] != tc.oid {
				t.Fatalf("the order identity must survive a restart: %+v", atts)
			}
			r.answer(tc.member, pairNativeAnswer{err: errors.New("429 Too Many Requests")})
			r.step(6 * time.Second)
			if r.readCount() != 1 || r.obligation(pairObligationNativeUnknown, tc.member) == nil || len(r.savedResolutions(tc.member)) != 0 {
				t.Fatalf("a rate-limited history read must clear nothing: reads %d %v", r.readCount(), e.obligationKinds(tc.member))
			}
			if a := r.attempts(tc.member); len(a) != 1 || a[0].RetryCount != 1 || !strings.Contains(a[0].LastError, "429") || a[0].RetryAtMs <= e.now.UnixMilli() {
				t.Fatalf("the failed read must save its diagnostic and a later retry: %+v", a)
			}
			bookBefore := e.signed(tc.member)
			r.step(time.Second)
			if r.readCount() != 1 {
				t.Fatalf("no read may run before the retry is due, got %d", r.readCount())
			}
			r.answer(tc.member, pairNativeAnswer{res: pairNativeHistoryOf(full, tc.side, tc.instrument)})
			r.step(15 * time.Second)
			if r.readCount() != 2 {
				t.Fatalf("the due retry must read once, got %d", r.readCount())
			}
			if got := e.signed(tc.member); got != 0 || bookBefore == 0 {
				t.Fatalf("the recovered fills must flatten the member book once: before %v after %v", bookBefore, got)
			}
			res := r.savedResolutions(tc.member)
			if len(res) != 1 || res[0].Mode != pairNativeModeVenueHistory || res[0].Kind != pairObligationNativeUnknown || res[0].DecidedBy != pairNativeDecidedByCoordinator || len(res[0].FillIDs) != len(tc.sizes) {
				t.Fatalf("the resolution audit must commit once with the booked fills: %+v", res)
			}
			if r.obligation(pairObligationNativeUnknown, tc.member) != nil || len(r.attempts(tc.member)) != 0 || strings.Contains(e.savedProgress(), pairObligationNativeUnknown) {
				t.Fatalf("the unknown obligation and its attempt must end with the book commit: %v", e.obligationKinds(tc.member))
			}
			a := r.nativeAction(tc.member)
			ledger, err := pairParseLedger(a.SettlementJSON)
			if err != nil || a.Status != basketActionCommitted || len(ledger.Fills) != len(tc.sizes) || len(ledger.Fees) != len(tc.sizes) || !basketQtyEqual(a.AppliedQty, a.ConfirmedQty) {
				t.Fatalf("the native action must settle once through the ledger: %+v %v", a, err)
			}
			rows := e.savedCloseRows(tc.member)
			for i := 0; i < 3; i++ {
				r.answer(tc.member, pairNativeAnswer{res: pairNativeHistoryOf(full, tc.side, tc.instrument)})
				r.step(time.Minute)
			}
			r.restart()
			r.step(time.Minute)
			if r.readCount() != 2 || e.savedCloseRows(tc.member) != rows || rows != 1 || len(r.savedResolutions(tc.member)) != 1 || r.obligation(pairObligationNativeUnknown, tc.member) != nil {
				t.Fatalf("repeated reads and a restart must add nothing: reads %d rows %d->%d", r.readCount(), rows, e.savedCloseRows(tc.member))
			}
			if after := r.f.hlCalls + r.f.okxCalls; after != before+1 {
				t.Fatalf("recovery must send no order: %d -> %d", before, after)
			}
			if got := r.savedResolutions(tc.member)[0].AlertStatus; got != pairNativeAlertDispatched {
				t.Fatalf("the resolution alert must be handed to the notifier once, status %q", got)
			}
		})
	}
}

func TestBasketPairNativeRecoveryConfirmedPartialThenAdditionalFillsBookOnlyTheDelta(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	sentAt := e.nativeAt()
	full := pairNativeEvidence("961", "sell", sentAt, "59900", "0.4", "USDC", "0.03", "0.03")
	partial := pairNativeEvidence("961", "sell", sentAt, "59900", "0.4", "USDC", "0.03")
	partial.ExecutedNativeQty, partial.Complete, partial.Detail = "0.06", false, "one fill is not visible yet"
	r.fireHL(partial, nil)
	r.step(time.Second)
	a := r.nativeAction(pairTestHL)
	if a == nil || a.Status != basketActionHeld || !basketQtyEqual(a.AppliedQty, 0.03) || r.obligation(pairObligationNativeUnconfirmed, pairTestHL) == nil {
		t.Fatalf("setup: the confirmed part must book and the remainder stay unconfirmed: %+v %v", a, e.obligationKinds(pairTestHL))
	}
	var prev basketVenueOutcome
	_ = json.Unmarshal([]byte(a.EvidenceJSON), &prev)
	_, fee1, rows1 := r.closeQty(pairTestHL)
	r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(full, "sell", "BTC")})
	r.step(10 * time.Second)
	a = r.nativeAction(pairTestHL)
	if a.Status != basketActionCommitted || !basketQtyEqual(a.AppliedQty, 0.06) || e.signed(pairTestHL) != 0 {
		t.Fatalf("the later complete read must book only the new fill: %+v book %v", a, e.signed(pairTestHL))
	}
	var next basketVenueOutcome
	_ = json.Unmarshal([]byte(a.EvidenceJSON), &next)
	if len(prev.Fills) != 1 || len(next.Fills) != 2 || next.Fills[0] != prev.Fills[0] && next.Fills[1] != prev.Fills[0] {
		t.Fatalf("the accepted fill must keep its saved visibility time: %+v then %+v", prev.Fills, next.Fills)
	}
	qty, fee, rows := r.closeQty(pairTestHL)
	if !basketQtyEqual(qty, 0.06) || !pairCostNear(fee, 0.8) || !pairCostNear(fee-fee1, 0.4) || rows != rows1+1 {
		t.Fatalf("each fill and fee must book once: qty %v fee %v (first %v) rows %d->%d", qty, fee, fee1, rows1, rows)
	}
	ledger, _ := pairParseLedger(a.SettlementJSON)
	if len(ledger.Fills) != 2 || len(ledger.Fees) != 2 {
		t.Fatalf("the ledger must hold each fill once: %+v", ledger)
	}
	if r.obligation(pairObligationNativeUnconfirmed, pairTestHL) != nil || len(r.savedResolutions(pairTestHL)) != 1 {
		t.Fatalf("the unconfirmed obligation must end with one resolution: %v", e.obligationKinds(pairTestHL))
	}
}

func TestBasketPairNativeRecoveryRefusedAnswersClearNothing(t *testing.T) {
	cases := []struct {
		name   string
		answer func(r *pairNativeRig, sentAt int64) pairNativeAnswer
		during func(r *pairNativeRig) func(req pairNativeHistoryRequest)
	}{
		{name: "timeout", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			return pairNativeAnswer{err: context.DeadlineExceeded}
		}},
		{name: "incomplete history", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			return pairNativeAnswer{res: pairNativeHistoryResult{Detail: "the order is live"}}
		}},
		{name: "clipped history", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			ev := pairNativeEvidence("971", "sell", sentAt, "59900", "0.9", "USDC", "0.02")
			ev.ExecutedNativeQty, ev.Complete, ev.Detail = "0.06", false, "the fill history needs more pages"
			return pairNativeAnswer{res: pairNativeHistoryResult{Complete: true, Evidence: *ev}}
		}},
		{name: "fills of another order", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			ev := pairNativeEvidence("999", "sell", sentAt, "59900", "0.9", "USDC", "0.06")
			ev.VenueOrderIDs = []string{"971"}
			return pairNativeAnswer{res: pairNativeHistoryOf(ev, "sell", "BTC")}
		}},
		{name: "overfill", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			return pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("971", "sell", sentAt, "59900", "0.9", "USDC", "0.09"), "sell", "BTC")}
		}},
		{name: "empty history", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			ev := nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{"971"}, Complete: true}
			return pairNativeAnswer{res: pairNativeHistoryResult{Complete: true, Evidence: ev}}
		}},
		{name: "native send during the read", answer: func(r *pairNativeRig, sentAt int64) pairNativeAnswer {
			return pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("971", "sell", sentAt, "59900", "0.9", "USDC", "0.06"), "sell", "BTC")}
		}, during: func(r *pairNativeRig) func(req pairNativeHistoryRequest) {
			return func(req pairNativeHistoryRequest) {
				r.f.hlOut = func() (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("the venue is unreachable") }
				r.e.bc.pairKillSwitchHLCloser(r.f.closers.hl)("BTC", nil, nil)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairNativeRig(t)
			e := r.e
			sentAt := e.nativeAt()
			r.fireHL(pairNativeLostFills("971", "0.06", "the fill lookup failed", false), nil)
			calls := r.f.hlCalls
			book := e.signed(pairTestHL)
			r.answer(pairTestHL, tc.answer(r, sentAt))
			if tc.during != nil {
				r.during = tc.during(r)
			}
			r.step(6 * time.Second)
			r.during = nil
			if r.readCount() != 1 || e.signed(pairTestHL) != book || len(r.savedResolutions(pairTestHL)) != 0 || r.obligation(pairObligationNativeUnknown, pairTestHL) == nil {
				t.Fatalf("a refused answer must change no book and clear nothing: reads %d book %v %v", r.readCount(), e.signed(pairTestHL), e.obligationKinds(pairTestHL))
			}
			wantCalls := calls
			if tc.during != nil {
				wantCalls++
			}
			if r.f.hlCalls != wantCalls {
				t.Fatalf("recovery must send no order (an emergency send keeps its authority): calls %d want %d", r.f.hlCalls, wantCalls)
			}
			if a := r.nativeAction(pairTestHL); a != nil && (a.ConfirmedQty > 0 || a.AppliedQty > 0) {
				t.Fatalf("a refused answer must leave the saved action unconfirmed: %+v", a)
			}
			if p := r.savedProgressValue(); len(p.NativeAttempts) == 0 {
				t.Fatalf("the attempt and its diagnostic must stay saved: %+v", p.NativeAttempts)
			}
		})
	}
}

func TestBasketPairNativeRecoveryTerminalZeroFillChangesNoBook(t *testing.T) {
	t.Run("proven zero fill", func(t *testing.T) {
		r := newPairNativeRig(t)
		e := r.e
		sentAt := e.nativeAt()
		r.fireOKX(pairNativeLostFills("861", "0", "the order read is not terminal yet", false), nil)
		book := e.signed(pairTestOKX)
		r.answer(pairTestOKX, pairNativeAnswer{res: pairNativeZeroHistory("861", "buy", "BTC-USDT-SWAP", sentAt)})
		r.step(6 * time.Second)
		a := r.nativeAction(pairTestOKX)
		if a == nil || a.Status != basketActionNoFill || e.signed(pairTestOKX) != book || r.obligation(pairObligationNativeUnknown, pairTestOKX) != nil {
			t.Fatalf("a proven terminal zero fill must end the action no_fill with no book change: %+v %v", a, e.obligationKinds(pairTestOKX))
		}
		res := r.savedResolutions(pairTestOKX)
		if len(res) != 1 || res[0].Mode != pairNativeModeZeroFill || res[0].BookedQty != 0 {
			t.Fatalf("the zero-fill resolution must be saved: %+v", res)
		}
		var status string
		if err := e.db().db.QueryRow(`SELECT status FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&status); err != nil || status != basketActionNoFill {
			t.Fatalf("the saved row must be no_fill: %q %v", status, err)
		}
	})
	t.Run("prior confirmed fills are never erased", func(t *testing.T) {
		r := newPairNativeRig(t)
		e := r.e
		sentAt := e.nativeAt()
		partial := pairNativeEvidence("862", "sell", sentAt, "59900", "0.4", "USDC", "0.03")
		partial.ExecutedNativeQty, partial.Complete = "0.06", false
		r.fireHL(partial, nil)
		r.step(time.Second)
		booked := r.nativeAction(pairTestHL).AppliedQty
		r.answer(pairTestHL, pairNativeAnswer{res: pairNativeZeroHistory("862", "sell", "BTC", sentAt)})
		r.step(10 * time.Second)
		a := r.nativeAction(pairTestHL)
		if !basketQtyEqual(a.AppliedQty, booked) || a.Status == basketActionNoFill || r.obligation(pairObligationNativeUnconfirmed, pairTestHL) == nil || len(r.savedResolutions(pairTestHL)) != 0 {
			t.Fatalf("a zero-fill answer must not erase a confirmed booking: %+v %v", a, e.obligationKinds(pairTestHL))
		}
	})
}

func TestBasketPairNativeRecoveryKeepsEveryOrderIdentityAcrossRestart(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.fireHL(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{"981"}, Detail: "the fill lookup raised"}, errors.New("close subprocess exit 1"))
	release, ok := tryLockEntryAttempt(pairTestOKX)
	if !ok {
		t.Fatal("setup: the entry lock is busy")
	}
	notAll := false
	r.fireOKX(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{"871", "872"}, Detail: "close order 3 failed after 2 close orders were sent", OrdersComplete: &notAll}, errors.New("close failed"))
	r.fireOKX(pairNativeLostFills("873", "6", "the trade read failed", false), nil)
	release()
	r.restart()
	hl := r.attempts(pairTestHL)
	if len(hl) != 1 || hl[0].ActionID == "" || hl[0].VenueOrderIDs[0] != "981" || hl[0].OrdersComplete || hl[0].Provenance != pairNativeProvenanceEvidence || hl[0].CloseError == "" {
		t.Fatalf("a failed initial read must keep its order id, error and incomplete membership: %+v", hl)
	}
	okx := r.attempts(pairTestOKX)
	if len(okx) != 2 {
		t.Fatalf("each action-less okx close must keep its own attempt: %+v", okx)
	}
	var multi, single *pairNativeAttempt
	for i := range okx {
		if len(okx[i].VenueOrderIDs) == 2 {
			multi = &okx[i]
		} else {
			single = &okx[i]
		}
	}
	if multi == nil || single == nil || multi.OrdersComplete || multi.ActionID != "" || !single.OrdersComplete || multi.ID == single.ID {
		t.Fatalf("a multi-order close with a later submission error keeps both ids and incomplete membership: %+v", okx)
	}
	unknown := 0
	for _, o := range e.rt().Pair.Progress.Obligations {
		if o.Kind == pairObligationNativeUnknown && o.MemberID == pairTestOKX && (o.ActionID == multi.ID || o.ActionID == single.ID) {
			unknown++
		}
	}
	if unknown != 2 {
		t.Fatalf("two action-less unknown closes must keep two obligations keyed by their attempts: %d %v", unknown, e.rt().Pair.Progress.Obligations)
	}
	r.step(6 * time.Second)
	r.step(6 * time.Second)
	for _, req := range r.reads {
		if req.AttemptID == multi.ID || req.AttemptID == hl[0].ID {
			t.Fatalf("an attempt with incomplete order membership must never be read automatically: %+v", req)
		}
	}
	e.mu.Lock()
	e.rt().Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: pairTestHL, ActionID: "pnr-0123456789abcdef0123456789abcdef", Detail: "saved before attempts", AtMs: e.now.UnixMilli(), FromMs: e.now.UnixMilli() - 1000})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	r.restart()
	cmd := pairNativeOperatorCommand{Invoker: pairNativeTestOwner, Partition: "live", PairID: pairTestID, MemberID: pairTestHL, Kind: pairObligationNativeUnknown,
		ObligationID: "pnr-0123456789abcdef0123456789abcdef", Decision: pairNativeDecisionRead, Reason: "check it", Orders: []string{"999"}}
	cmd.Revision = r.revision(cmd.Kind, cmd.MemberID, cmd.ObligationID)
	if msg, ok := e.bc.pairNativeOperator(cmd, false); ok || !strings.Contains(msg, "only a write-off") {
		t.Fatalf("an obligation without provable identity must refuse a venue read: %q", msg)
	}
	if r.obligation(pairObligationNativeUnknown, pairTestHL) == nil {
		t.Fatal("the legacy-style obligation must stay held")
	}
}

func (r *pairNativeRig) revision(kind, member, id string) string {
	r.e.mu.RLock()
	defer r.e.mu.RUnlock()
	ob := r.e.rt().Pair.Progress.obligation(kind, member, id)
	if ob == nil {
		return ""
	}
	return pairNativeObligationRevision(r.e.rt(), *ob)
}

func TestBasketPairNativeRecoveryActionlessFillsBookThroughTheReceiptOwner(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	sentAt := e.nativeAt()
	release, ok := tryLockEntryAttempt(pairTestHL)
	if !ok {
		t.Fatal("setup: the entry lock is busy")
	}
	r.fireHL(pairNativeLostFills("991", "0.06", "the fill lookup was rate limited", false), nil)
	release()
	atts := r.attempts(pairTestHL)
	if len(atts) != 1 || atts[0].ActionID != "" || r.obligation(pairObligationNativeUnknown, pairTestHL) == nil || r.obligation(pairObligationNativeUnknown, pairTestHL).ActionID != atts[0].ID {
		t.Fatalf("setup: an action-less unknown close must be keyed by its attempt: %+v %v", atts, e.obligationKinds(pairTestHL))
	}
	r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("991", "sell", sentAt, "59900", "0.9", "USDC", "0.06"), "sell", "BTC")})
	r.step(6 * time.Second)
	r.step(time.Second)
	if e.signed(pairTestHL) != 0 || e.savedCloseRows(pairTestHL) != 1 || len(e.rt().Pair.Progress.NativeReceipts) != 0 {
		t.Fatalf("the recovered fills must book once through the receipt owner: book %v rows %d", e.signed(pairTestHL), e.savedCloseRows(pairTestHL))
	}
	res := r.savedResolutions(pairTestHL)
	if len(res) != 1 || res[0].ReceiptID == "" || res[0].ObligationID != atts[0].ID {
		t.Fatalf("the action-less resolution must name its receipt and attempt: %+v", res)
	}
	if len(r.attempts(pairTestHL)) != 0 || r.obligation(pairObligationNativeUnknown, pairTestHL) != nil {
		t.Fatalf("the attempt must end after the receipt books: %+v", r.attempts(pairTestHL))
	}
}

func TestBasketPairNativeRecoveryFailedBookSaveKeepsTheDurableObligation(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	sentAt := e.nativeAt()
	r.fireHL(pairNativeLostFills("1001", "0.06", "the fill lookup failed", false), nil)
	restore := pairFailBookSaves(false)
	r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("1001", "sell", sentAt, "59900", "0.9", "USDC", "0.06"), "sell", "BTC")})
	r.step(6 * time.Second)
	e.mu.Lock()
	e.rt().touchPairProgress()
	saved := e.bc.persistPair(e.rt())
	e.mu.Unlock()
	if !saved || !e.rt().needsDurable {
		t.Fatalf("setup: the book save must fail and a progress-only save pass: %v %v", saved, e.rt().needsDurable)
	}
	p := r.savedProgressValue()
	found := false
	for _, o := range p.Obligations {
		if o.Kind == pairObligationNativeUnknown && o.MemberID == pairTestHL {
			found = true
		}
	}
	if !found || len(r.savedResolutions(pairTestHL)) != 0 {
		t.Fatalf("a progress-only save must keep the durable obligation and publish no resolution: %+v", p.Obligations)
	}
	e.mu.RLock()
	pending := pairEmergencyPendingFor(e.state, livePartition)
	e.mu.RUnlock()
	if len(pending) == 0 {
		t.Fatal("the completion gate must stay closed while the accounting is not durable")
	}
	restore()
	r.restart()
	if r.obligation(pairObligationNativeUnknown, pairTestHL) == nil {
		t.Fatal("after a restart the obligation must still be open")
	}
	r.step(time.Second)
	r.step(time.Second)
	if e.signed(pairTestHL) != 0 || e.savedCloseRows(pairTestHL) != 1 || len(r.savedResolutions(pairTestHL)) != 1 || r.obligation(pairObligationNativeUnknown, pairTestHL) != nil {
		t.Fatalf("the next complete save must apply the recovered fill once: book %v rows %d", e.signed(pairTestHL), e.savedCloseRows(pairTestHL))
	}
}

type pairNativeTransport struct {
	mu        sync.Mutex
	prompts   []string
	responds  []string
	followups []string
}

func (tr *pairNativeTransport) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.prompts = append(tr.prompts, content)
	return &discordgo.Message{ID: "m", ChannelID: channelID, Content: content}, nil
}

func (tr *pairNativeTransport) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

func (tr *pairNativeTransport) InteractionRespond(_ *discordgo.Interaction, resp *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if resp != nil && resp.Data != nil {
		tr.responds = append(tr.responds, resp.Data.Content)
	}
	return nil
}

func (tr *pairNativeTransport) FollowupMessageCreate(_ *discordgo.Interaction, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.followups = append(tr.followups, data.Content)
	return &discordgo.Message{}, nil
}

func (tr *pairNativeTransport) ApplicationCommandBulkOverwrite(string, string, []*discordgo.ApplicationCommand, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	return nil, nil
}

func (r *pairNativeRig) discord() (*DiscordNotifier, *pairNativeTransport) {
	st := discordgo.NewState()
	st.User = &discordgo.User{ID: "bot"}
	tr := &pairNativeTransport{}
	d := &DiscordNotifier{session: &discordgo.Session{State: st}, transport: tr, ownerID: pairNativeTestOwner,
		ss: &StatusServer{state: r.e.state, mu: r.e.mu, stateDB: r.e.store, strategies: r.e.cfg.Strategies}}
	return d, tr
}

func pairNativeOpts(kv ...string) []*discordgo.ApplicationCommandInteractionDataOption {
	var out []*discordgo.ApplicationCommandInteractionDataOption
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, &discordgo.ApplicationCommandInteractionDataOption{Name: kv[i], Type: discordgo.ApplicationCommandOptionString, Value: kv[i+1]})
	}
	return out
}

func (r *pairNativeRig) command(d *DiscordNotifier, user, guild, reply string, opts []*discordgo.ApplicationCommandInteractionDataOption) string {
	r.t.Helper()
	ic := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, GuildID: guild,
		Data: discordgo.ApplicationCommandInteractionData{Name: commandPrefix + pairNativeCommandName, Options: opts}}}
	if guild != "" {
		ic.Member = &discordgo.Member{User: &discordgo.User{ID: user}}
	} else {
		ic.User = &discordgo.User{ID: user}
	}
	tr := d.transport.(*pairNativeTransport)
	tr.mu.Lock()
	f0, r0 := len(tr.followups), len(tr.responds)
	tr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if reply == "" {
			return
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			d.mu.Lock()
			n := len(d.dmHandlers)
			d.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		d.handleDirectMessage(&discordgo.MessageCreate{Message: &discordgo.Message{Author: &discordgo.User{ID: user}, Content: reply}})
	}()
	d.interactionCreate(tr, ic)
	<-done
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.followups) > f0 {
		return tr.followups[len(tr.followups)-1]
	}
	if len(tr.responds) > r0 {
		return tr.responds[len(tr.responds)-1]
	}
	return ""
}

func (r *pairNativeRig) decide(d *DiscordNotifier, member, kind, id, decision, reason string, extra ...string) string {
	opts := append([]string{"partition", "live", "pair", pairTestID, "member", member, "decision", decision, "kind", kind, "id", id, "revision", r.revision(kind, member, id), "reason", reason}, extra...)
	return r.command(d, pairNativeTestOwner, "", "confirm", pairNativeOpts(opts...))
}

func TestBasketPairNativeOperatorWriteOffKeepsFundingIncompleteAndAppliesOnce(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.fireHL(pairNativeLostFills("1101", "0.06", "the fill lookup failed", false), nil)
	ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
	d, _ := r.discord()
	show := r.command(d, pairNativeTestOwner, "", "", pairNativeOpts("partition", "live", "pair", pairTestID, "member", pairTestHL, "decision", "show"))
	if !strings.Contains(show, ob.ActionID) || !strings.Contains(show, r.revision(ob.Kind, ob.MemberID, ob.ActionID)) {
		t.Fatalf("show must list the obligation and its revision: %q", show)
	}
	killSwitch, _ := json.Marshal(e.state.PortfolioRisk)
	book := e.signed(pairTestHL)
	e.mu.Lock()
	requestPairEmergencyClose(e.state.Strategies[pairTestHL], "BTC", book, "kill_switch")
	e.mu.Unlock()
	msg := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "venue history purged; the owner accepts the gap")
	if !strings.Contains(msg, "Staged decision pnq-") {
		t.Fatalf("the confirmed write-off must be staged: %q", msg)
	}
	if r.obligation(pairObligationNativeUnknown, pairTestHL) == nil || len(r.savedResolutions(pairTestHL)) != 0 {
		t.Fatal("staging must change no obligation before the coordinator applies it")
	}
	again := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "venue history purged; the owner accepts the gap")
	if !strings.Contains(again, "already staged") {
		t.Fatalf("a repeated request must not stage twice: %q", again)
	}
	r.step(time.Second)
	if r.obligation(pairObligationNativeUnknown, pairTestHL) != nil || e.signed(pairTestHL) != book {
		t.Fatalf("the write-off must remove only the named obligation and change no book: %v %v", e.obligationKinds(pairTestHL), e.signed(pairTestHL))
	}
	res := r.savedResolutions(pairTestHL)
	if len(res) != 1 || res[0].Mode != pairNativeModeWriteOff || res[0].Operator != pairNativeTestOwner || res[0].Reason == "" || res[0].FundingHoldFromMs != ob.FromMs || res[0].Prior.ActionID != ob.ActionID {
		t.Fatalf("the audit must save the operator, reason, prior obligation and funding hold: %+v", res)
	}
	if got := pairNativeUnknownFromMs(e.rt(), pairTestHL); got != ob.FromMs {
		t.Fatalf("funding must stay incomplete from the unknown start %d, horizon %d", ob.FromMs, got)
	}
	if o, ok := pairIncompleteEvidence(e.rt()); !ok || o.Kind != pairObligationNativeFundingUncertain && o.Kind != pairObligationEmergencyClose {
		t.Fatalf("the result must stay incomplete after a write-off: %+v %v", o, ok)
	}
	if after, _ := json.Marshal(e.state.PortfolioRisk); string(after) != string(killSwitch) {
		t.Fatal("a write-off must not change the kill switch")
	}
	if r.decisionDMs() != 1 {
		t.Fatalf("one CRITICAL alert per committed decision, got %d", r.decisionDMs())
	}
	if a := r.nativeAction(pairTestHL); a.Status != basketActionHeld {
		t.Fatalf("the written-off action must stay held, got %s", a.Status)
	}
	dup := r.command(d, pairNativeTestOwner, "", "confirm", pairNativeOpts("partition", "live", "pair", pairTestID, "member", pairTestHL, "decision", pairNativeDecisionWriteOff,
		"kind", ob.Kind, "id", ob.ActionID, "revision", "0000000000000000", "reason", "again"))
	if !strings.Contains(dup, "already resolved by") {
		t.Fatalf("a repeated decision after the commit must return the committed audit: %q", dup)
	}
	r.restart()
	r.step(time.Second)
	r.step(time.Second)
	if r.obligation(pairObligationNativeUnknown, pairTestHL) != nil || len(r.savedResolutions(pairTestHL)) != 1 || r.decisionDMs() != 0 || pairNativeUnknownFromMs(e.rt(), pairTestHL) != ob.FromMs {
		t.Fatalf("a restart must keep the decision and its hold and never resend the alert: %v %d", e.obligationKinds(pairTestHL), r.decisionDMs())
	}
	if !e.hasObligation(pairObligationEmergencyClose, pairTestHL) || e.signed(pairTestHL) != book {
		t.Fatalf("a live member whose book is still open must keep its emergency close obligation after a write-off: %v", e.obligationKinds(pairTestHL))
	}
}

func TestBasketPairNativeOperatorRefusalsChangeNothing(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.fireHL(pairNativeLostFills("1201", "0.06", "the fill lookup failed", false), nil)
	ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
	rev := r.revision(ob.Kind, ob.MemberID, ob.ActionID)
	d, _ := r.discord()
	before := e.savedProgress()
	base := []string{"pair", pairTestID, "member", pairTestHL, "kind", ob.Kind, "id", ob.ActionID, "reason", "r"}
	for _, tc := range []struct {
		name, user, guild string
		opts              []string
		want              string
	}{
		{"not the owner", "intruder", "", append([]string{"partition", "live", "decision", "write-off", "revision", rev}, base...), "owner-only"},
		{"guild channel", pairNativeTestOwner, "guild-1", append([]string{"partition", "live", "decision", "write-off", "revision", rev}, base...), "only available in a DM"},
		{"wrong partition", pairNativeTestOwner, "", append([]string{"partition", "paper", "decision", "write-off", "revision", rev}, base...), "belongs to partition live"},
		{"stale revision", pairNativeTestOwner, "", append([]string{"partition", "live", "decision", "write-off", "revision", "ffffffffffffffff"}, base...), "is stale"},
		{"unknown obligation", pairNativeTestOwner, "", []string{"partition", "live", "decision", "write-off", "revision", rev, "pair", pairTestID, "member", pairTestHL, "kind", ob.Kind, "id", "pna-ffff", "reason", "r"}, "has no open"},
		{"disallowed decision", pairNativeTestOwner, "", append([]string{"partition", "live", "decision", "acknowledge", "revision", rev}, base...), "permits only"},
		{"receipt kind", pairNativeTestOwner, "", []string{"partition", "live", "decision", "write-off", "revision", rev, "pair", pairTestID, "member", pairTestHL, "kind", pairObligationNativeReceipt, "id", ob.ActionID, "reason", "r"}, "is not a native obligation"},
		{"emergency kind", pairNativeTestOwner, "", []string{"partition", "live", "decision", "write-off", "revision", rev, "pair", pairTestID, "member", pairTestHL, "kind", pairObligationEmergencyClose, "id", ob.ActionID, "reason", "r"}, "is not a native obligation"},
		{"no reason", pairNativeTestOwner, "", []string{"partition", "live", "decision", "write-off", "revision", rev, "pair", pairTestID, "member", pairTestHL, "kind", ob.Kind, "id", ob.ActionID}, "printable reason"},
		{"no confirmation", pairNativeTestOwner, "", append([]string{"partition", "live", "decision", "write-off", "revision", rev}, base...), "Cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := ""
			if tc.name == "no confirmation" {
				reply = "no"
			}
			msg := r.command(d, tc.user, tc.guild, reply, pairNativeOpts(tc.opts...))
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("reply %q, want %q", msg, tc.want)
			}
			if e.savedProgress() != before || len(e.rt().Pair.Progress.NativeRequests) != 0 {
				t.Fatalf("a refused request must stage nothing")
			}
		})
	}
	e.mu.Lock()
	e.bc.pairNativeSendBegan(pairTestHL)
	e.mu.Unlock()
	busy := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "busy member")
	e.mu.Lock()
	e.bc.pairNativeSendEnded(pairTestHL)
	e.mu.Unlock()
	if !strings.Contains(busy, "in flight") || len(e.rt().Pair.Progress.NativeRequests) != 0 {
		t.Fatalf("a native send in flight must refuse the decision: %q", busy)
	}
	staged := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "staged before a new close")
	if !strings.Contains(staged, "Staged") {
		t.Fatalf("setup: %q", staged)
	}
	r.f.hlOut = func() (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("venue unreachable") }
	r.e.bc.pairKillSwitchHLCloser(r.f.closers.hl)("BTC", nil, nil)
	r.step(time.Second)
	if r.obligation(pairObligationNativeUnknown, pairTestHL) == nil || len(r.savedResolutions(pairTestHL)) != 0 || len(e.rt().Pair.Progress.NativeRequests) != 0 {
		t.Fatalf("a newer native send must void the staged decision and keep the obligation: %v", e.obligationKinds(pairTestHL))
	}
}

func TestBasketPairNativeOperatorVenueReadBooksAPreUpgradeAction(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	sentAt := e.nativeAt()
	r.fireOKX(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, Detail: "no order id"}, errors.New("close failed"))
	ob := r.obligation(pairObligationNativeUnknown, pairTestOKX)
	e.mu.Lock()
	e.rt().Pair.Progress.NativeAttempts = nil
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	d, _ := r.discord()
	if msg := r.decide(d, pairTestOKX, ob.Kind, ob.ActionID, pairNativeDecisionRead, "no ids"); !strings.Contains(msg, "no venue order id is saved") {
		t.Fatalf("a venue read without order ids must refuse: %q", msg)
	}
	other := r.decide(d, pairTestOKX, ob.Kind, ob.ActionID, pairNativeDecisionRead, "found it in the order history", "orders", "1301")
	if !strings.Contains(other, "Staged") {
		t.Fatalf("the operator venue read must stage: %q", other)
	}
	r.answer(pairTestOKX, pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("1301", "buy", sentAt, "60050", "0.3", "USDT", "6"), "buy", "BTC-USDT-SWAP")})
	r.step(time.Second)
	if e.signed(pairTestOKX) != 0 || e.savedCloseRows(pairTestOKX) != 1 {
		t.Fatalf("the checked operator evidence must book once: %v rows %d", e.signed(pairTestOKX), e.savedCloseRows(pairTestOKX))
	}
	res := r.savedResolutions(pairTestOKX)
	if len(res) != 1 || res[0].Mode != pairNativeModeOperatorRead || res[0].Operator != pairNativeTestOwner || r.decisionDMs() != 1 {
		t.Fatalf("the operator read must commit one audited decision and one alert: %+v %d", res, r.decisionDMs())
	}
	if len(r.reads) != 1 || r.reads[0].VenueOrderIDs[0] != "1301" || r.reads[0].OrderSide != "buy" {
		t.Fatalf("the read must check the operator order on the close side: %+v", r.reads)
	}
}

func TestBasketPairNativeOperatorLegacyAcknowledgeSurvivesTheFlatResetAndRestart(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.f.fire()
	r.step(time.Second)
	e.venues.of(pairTestHL).setVenueBase(0)
	e.venues.of(pairTestOKX).setVenueBase(0)
	if _, err := e.db().db.Exec(`UPDATE basket_member_actions SET settlement_json = '' WHERE purpose = 'pair_kill_switch'`); err != nil {
		t.Fatal(err)
	}
	trades := e.savedCloseRows(pairTestHL) + e.savedCloseRows(pairTestOKX)
	r.restart()
	d, _ := r.discord()
	for _, id := range []string{pairTestHL, pairTestOKX} {
		ob := r.obligation(pairObligationNativeLegacy, id)
		if ob == nil {
			t.Fatalf("setup: %s needs a legacy obligation", id)
		}
		if msg := r.decide(d, id, ob.Kind, ob.ActionID, pairNativeDecisionRead, "x"); !strings.Contains(msg, "permits only") {
			t.Fatalf("a legacy obligation never takes a venue read: %q", msg)
		}
		if msg := r.decide(d, id, ob.Kind, ob.ActionID, pairNativeDecisionAck, "booked before ledgers; checked against the venue statement"); !strings.Contains(msg, "Staged") {
			t.Fatalf("the acknowledgment must stage: %q", msg)
		}
		r.step(time.Second)
	}
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	if l := e.rt().Pair.Progress.Lifecycle; l != pairLifecycleFlat {
		t.Fatalf("the pair must finish its flat reset after the acknowledgments, lifecycle %s %v", l, e.obligationKinds(""))
	}
	r.restart()
	r.step(time.Second)
	if e.hasObligation(pairObligationNativeLegacy, pairTestHL) || e.hasObligation(pairObligationNativeLegacy, pairTestOKX) {
		t.Fatalf("a resolved legacy item must never return after the flat reset and a restart: %v", e.obligationKinds(""))
	}
	if got := e.savedCloseRows(pairTestHL) + e.savedCloseRows(pairTestOKX); got != trades {
		t.Fatalf("an acknowledgment never books a historical quantity again: %d -> %d", trades, got)
	}
	if len(r.savedResolutions(pairTestHL)) != 1 || len(r.savedResolutions(pairTestOKX)) != 1 {
		t.Fatal("each acknowledgment must keep its audit after the reset")
	}
	if e.rt().Pair.Progress.NativeFundingHolds != nil {
		t.Fatalf("a legacy acknowledgment imposes no funding hold: %+v", e.rt().Pair.Progress.NativeFundingHolds)
	}
}

func TestBasketPairNativeOperatorStagingRacesAConcurrentStep(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.fireHL(pairNativeLostFills("1401", "0.06", "the fill lookup failed", false), nil)
	ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
	rev := r.revision(ob.Kind, ob.MemberID, ob.ActionID)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.mu.Lock()
			e.now = e.now.Add(100 * time.Millisecond)
			at := e.now
			e.mu.Unlock()
			e.bc.step(basketCycleInputs{Now: at, Once: true})
		}
	}()
	msgs := make([]string, 5)
	for i := range msgs {
		cmd := pairNativeOperatorCommand{Invoker: pairNativeTestOwner, Partition: "live", PairID: pairTestID, MemberID: pairTestHL, Kind: ob.Kind, ObligationID: ob.ActionID,
			Decision: pairNativeDecisionWriteOff, Revision: rev, Reason: "race"}
		msgs[i], _ = e.bc.pairNativeOperator(cmd, true)
	}
	close(stop)
	wg.Wait()
	r.step(time.Second)
	staged := 0
	for _, m := range msgs {
		if strings.Contains(m, "Staged decision") {
			staged++
		}
	}
	if staged != 1 || len(r.savedResolutions(pairTestHL)) != 1 || r.obligation(pairObligationNativeUnknown, pairTestHL) != nil {
		t.Fatalf("concurrent staging and steps must apply one decision once: staged %d %v %v", staged, msgs, e.obligationKinds(pairTestHL))
	}
}

func (r *pairNativeRig) stageRead(member string, ob *pairObligation, orders ...string) string {
	r.t.Helper()
	cmd := pairNativeOperatorCommand{Invoker: pairNativeTestOwner, Partition: "live", PairID: pairTestID, MemberID: member, Kind: ob.Kind, ObligationID: ob.ActionID,
		Decision: pairNativeDecisionRead, Reason: "found the orders in the venue order history", Orders: orders}
	cmd.Revision = r.revision(cmd.Kind, cmd.MemberID, cmd.ObligationID)
	msg, _ := r.e.bc.pairNativeOperator(cmd, true)
	return msg
}

func TestBasketPairNativeOperatorVenueReadCoversEverySavedOrder(t *testing.T) {
	setup := func(t *testing.T) (*pairNativeRig, *pairObligation, int64) {
		r := newPairNativeRig(t)
		sentAt := r.e.nativeAt()
		release, ok := tryLockEntryAttempt(pairTestOKX)
		if !ok {
			t.Fatal("setup: the entry lock is busy")
		}
		notAll := false
		r.fireOKX(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{"871", "872"}, Detail: "close order 3 failed after 2 close orders were sent", OrdersComplete: &notAll}, errors.New("close failed"))
		release()
		atts := r.attempts(pairTestOKX)
		ob := r.obligation(pairObligationNativeUnknown, pairTestOKX)
		if len(atts) != 1 || atts[0].OrdersComplete || len(atts[0].VenueOrderIDs) != 2 || ob == nil || ob.ActionID != atts[0].ID {
			t.Fatalf("setup: a multi-order close with a later submission error keeps both ids as incomplete membership: %+v %+v", atts, ob)
		}
		return r, ob, sentAt
	}
	order := func(id, executed, state string, at int64) pairNativeHistoryOrder {
		return pairNativeHistoryOrder{OrderID: id, State: state, Terminal: true, Side: "buy", Instrument: "BTC-USDT-SWAP", ExecutedNativeQty: executed, AcceptedAtMs: at}
	}
	t.Run("a subset of the saved orders is widened to every saved order", func(t *testing.T) {
		r, ob, sentAt := setup(t)
		e := r.e
		if msg := r.stageRead(pairTestOKX, ob, "871"); !strings.Contains(msg, "Staged") {
			t.Fatalf("the operator venue read must stage: %q", msg)
		}
		ev := pairNativeEvidence("872", "buy", sentAt, "60050", "0.3", "USDT", "6")
		ev.VenueOrderIDs = []string{"871", "872"}
		r.answer(pairTestOKX, pairNativeAnswer{res: pairNativeHistoryResult{Complete: true, Evidence: *ev, Orders: []pairNativeHistoryOrder{order("871", "0", "canceled", sentAt), order("872", "6", "filled", sentAt)}}})
		r.step(time.Second)
		r.step(time.Second)
		if len(r.reads) != 1 || strings.Join(r.reads[0].VenueOrderIDs, ",") != "871,872" {
			t.Fatalf("the read must cover every order the attempt saved, not only the operator subset: %+v", r.reads)
		}
		res := r.savedResolutions(pairTestOKX)
		if len(res) != 1 || res[0].Mode != pairNativeModeOperatorRead || res[0].ReceiptID == "" || strings.Join(res[0].FillIDs, ",") != "872-f1" {
			t.Fatalf("the read must book the fill of the order the operator left out, never a zero fill: %+v", res)
		}
		if e.signed(pairTestOKX) != 0 || e.savedCloseRows(pairTestOKX) != 1 || r.obligation(pairObligationNativeUnknown, pairTestOKX) != nil {
			t.Fatalf("the left-out fill must book once: book %v rows %d %v", e.signed(pairTestOKX), e.savedCloseRows(pairTestOKX), e.obligationKinds(pairTestOKX))
		}
	})
	t.Run("an order the attempt did not save is added to the saved orders", func(t *testing.T) {
		r, ob, sentAt := setup(t)
		e := r.e
		if msg := r.stageRead(pairTestOKX, ob, "871", "872", "873"); !strings.Contains(msg, "Staged") {
			t.Fatalf("the operator venue read must stage: %q", msg)
		}
		ev := &nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{"871", "872", "873"}, Complete: true, ExecutedNativeQty: "6"}
		var orders []pairNativeHistoryOrder
		for i, id := range []string{"871", "872", "873"} {
			one := pairNativeEvidence(id, "buy", sentAt+int64(i), "60050", "0.1", "USDT", "2")
			ev.Fills = append(ev.Fills, one.Fills...)
			orders = append(orders, order(id, "2", "filled", sentAt))
		}
		r.answer(pairTestOKX, pairNativeAnswer{res: pairNativeHistoryResult{Complete: true, Evidence: *ev, Orders: orders}})
		r.step(time.Second)
		r.step(time.Second)
		if len(r.reads) != 1 || strings.Join(r.reads[0].VenueOrderIDs, ",") != "871,872,873" {
			t.Fatalf("the read must cover the saved orders and the operator order: %+v", r.reads)
		}
		res := r.savedResolutions(pairTestOKX)
		if len(res) != 1 || strings.Join(res[0].FillIDs, ",") != "871-f1,872-f1,873-f1" {
			t.Fatalf("each order's fill must be recorded once: %+v", res)
		}
		if e.signed(pairTestOKX) != 0 || e.savedCloseRows(pairTestOKX) != 1 {
			t.Fatalf("the three fills must book once: book %v rows %d", e.signed(pairTestOKX), e.savedCloseRows(pairTestOKX))
		}
		r.step(time.Minute)
		r.restart()
		r.step(time.Minute)
		if e.savedCloseRows(pairTestOKX) != 1 || len(r.savedResolutions(pairTestOKX)) != 1 {
			t.Fatal("a restart must book nothing again")
		}
	})
}

func TestBasketPairNativeOperatorWriteOffOfAFlatMemberEndsTheLifecycle(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	r.f.fire()
	from := e.now.UnixMilli() - 1000
	id := "pnr-00112233445566778899aabbccddeeff"
	e.mu.Lock()
	e.rt().Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: pairTestHL, ActionID: id, Detail: "saved before attempts", AtMs: e.now.UnixMilli(), FromMs: from})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	r.step(time.Second)
	e.venues.of(pairTestHL).setVenueBase(0)
	e.venues.of(pairTestOKX).setVenueBase(0)
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	if e.signed(pairTestHL) != 0 || e.signed(pairTestOKX) != 0 || e.lifecycle() == pairLifecycleFlat {
		t.Fatalf("setup: both books are flat and the open unknown obligation holds the lifecycle: %v %v %s", e.signed(pairTestHL), e.signed(pairTestOKX), e.lifecycle())
	}
	d, _ := r.discord()
	if msg := r.decide(d, pairTestHL, pairObligationNativeUnknown, id, pairNativeDecisionWriteOff, "venue history purged; the owner accepts the gap"); !strings.Contains(msg, "Staged") || !strings.Contains(msg, "pair increases hold while the hold exists") {
		t.Fatalf("the write-off must stage and its confirmation must state the increase hold: %q", msg)
	}
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a write-off of a flat member must let the pair finish its flat reset, lifecycle %s %v", e.lifecycle(), e.obligationKinds(""))
	}
	if got := pairNativeUnknownFromMs(e.rt(), pairTestHL); got != from {
		t.Fatalf("the funding hold must survive the flat reset: %d want %d", got, from)
	}
	if o, ok := pairIncompleteEvidence(e.rt()); !ok || o.Kind != pairObligationNativeFundingUncertain {
		t.Fatalf("the result must stay incomplete through the hold: %+v %v", o, ok)
	}
	if staged := e.rt().Pair.Progress.nativeRecoveryStaged(); len(staged) != 0 {
		t.Fatalf("a funding hold alone is no staged recovery for the contract-change check: %v", staged)
	}
	r.restart()
	r.step(time.Minute)
	if e.lifecycle() != pairLifecycleFlat || pairNativeUnknownFromMs(e.rt(), pairTestHL) != from || len(r.savedResolutions(pairTestHL)) != 1 {
		t.Fatalf("a restart keeps the flat lifecycle, the hold and the one decision: %s", e.lifecycle())
	}
	e.mu.Lock()
	hold := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
	e.mu.Unlock()
	if !strings.Contains(hold, "native funding hold") {
		t.Fatalf("a member with a funding hold must hold pair increases: %q", hold)
	}
	entries := len(e.actionsByPurpose(pairPurposeEntry))
	calls := r.f.hlCalls + r.f.okxCalls
	at := e.now.Truncate(time.Minute).Add(time.Minute)
	e.slot(at, "after-write-off")
	e.step(at.Add(time.Second))
	e.step(at.Add(30 * time.Second))
	if got := len(e.actionsByPurpose(pairPurposeEntry)); got != entries || r.f.hlCalls+r.f.okxCalls != calls || e.signed(pairTestHL) != 0 || e.signed(pairTestOKX) != 0 {
		t.Fatalf("an entry signal after the write-off must send no order: entries %d -> %d, books %v %v", entries, got, e.signed(pairTestHL), e.signed(pairTestOKX))
	}
}

func TestBasketPairNativeOperatorVenueReadRefusesAnOrderOrFillOfAnotherPairAction(t *testing.T) {
	entry := func(r *pairNativeRig) (*basketAction, pairConfirmedFill) {
		a := r.e.memberAction(pairTestHL, pairPurposeEntry)
		fills, err := pairParseFills(a.FillCoverageJSON)
		r.e.mu.Lock()
		if a.VenueOrderID == "" {
			a.VenueOrderID = "2001"
		}
		r.e.mu.Unlock()
		if err != nil || len(fills) != 1 {
			r.t.Fatalf("setup: the booked entry needs its order id and one fill record: %+v %v", a, err)
		}
		return a, fills[0]
	}
	for _, actionless := range []bool{false, true} {
		t.Run(fmt.Sprintf("actionless_%v", actionless), func(t *testing.T) {
			r := newPairNativeRig(t)
			e := r.e
			sentAt := e.nativeAt()
			a, fill := entry(r)
			if actionless {
				release, ok := tryLockEntryAttempt(pairTestHL)
				if !ok {
					t.Fatal("setup: the entry lock is busy")
				}
				r.fireHL(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, Detail: "no order id"}, errors.New("close failed"))
				release()
			} else {
				r.fireHL(&nativeCloseEvidence{V: nativeCloseEvidenceVersion, Detail: "no order id"}, errors.New("close failed"))
			}
			ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
			if ob == nil {
				t.Fatal("setup: the unknown close needs its obligation")
			}
			book, rows := e.signed(pairTestHL), e.savedCloseRows(pairTestHL)
			if msg := r.stageRead(pairTestHL, ob, a.VenueOrderID); !strings.Contains(msg, "belongs to pair action "+a.ActionID) || len(e.rt().Pair.Progress.NativeRequests) != 0 {
				t.Fatalf("an operator order id of another pair action must be refused at staging: %q", msg)
			}
			if msg := r.stageRead(pairTestHL, ob, "1702"); !strings.Contains(msg, "Staged") {
				t.Fatalf("setup: a fresh order id must stage: %q", msg)
			}
			ev := pairNativeEvidence("1702", "sell", sentAt, "59900", "0.9", "USDC", "0.06")
			ev.Fills[0].FillID = fill.FillID
			r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(ev, "sell", "BTC")})
			r.step(time.Second)
			r.step(time.Second)
			if r.readCount() != 1 || len(r.savedResolutions(pairTestHL)) != 0 || e.signed(pairTestHL) != book || e.savedCloseRows(pairTestHL) != rows || r.obligation(pairObligationNativeUnknown, pairTestHL) == nil {
				t.Fatalf("a read that returns a fill another pair action already accounts must book nothing and clear nothing: reads %d book %v rows %d %v", r.readCount(), e.signed(pairTestHL), e.savedCloseRows(pairTestHL), e.obligationKinds(pairTestHL))
			}
			if len(e.rt().Pair.Progress.NativeReceipts) != 0 {
				t.Fatalf("no receipt may carry the other action's fill: %+v", e.rt().Pair.Progress.NativeReceipts)
			}
		})
	}
}

func TestBasketPairFundingNativeDecisionHoldSavesLaterEventsUnresolvable(t *testing.T) {
	t0 := pairTestStart()
	from := pairSlotAt(t0, 2).UnixMilli()
	event := pairSlotAt(t0, 2).Add(30 * time.Second).UnixMilli()
	eventID := pairFundingEventID("hyperliquid", "BTC", event)
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, event, "0.0001")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, event, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	e.openMatched(0.05)
	e.mu.Lock()
	e.rt().Pair.Progress.NativeFundingHolds = append(e.rt().Pair.Progress.NativeFundingHolds, pairNativeFundingHold{MemberID: pairTestHL, Kind: pairObligationNativeUnknown,
		ObligationID: "pna-hold", ResolutionID: "pnz-hold", FromMs: from, AtMs: from, Detail: "the operator write_off left the inventory unproven"})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	pairEnterExitRunner(e, 0.05)
	e.slotStep(5)
	e.assertBooks("exited", 0, 0)
	e.step(pairSlotAt(t0, 6))
	status, reason, _ := e.fundingRow(pairTestHL, eventID)
	if status != pairFundingStatusUnresolvable || reason != pairFundingReasonNativeDecision {
		t.Fatalf("an event that needs inventory the hold left unproven is saved unresolvable, got %q %q", status, reason)
	}
	if len(e.fundingTrades(pairTestHL)) != 0 || e.hasObligation(pairObligationFundingIncomplete, pairTestHL) {
		t.Fatal("the held event books no charge and keeps no funding obligation")
	}
	if !e.alertedPrefix("funding-native-hold|") || e.alertedPrefix("funding-unresolvable|") {
		t.Fatalf("the hold raises one alert per hold, never a critical alert per event: %v", e.rt().Pair.Progress.Alerts)
	}
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("the exit filled flat; a funding hold must not trap the lifecycle, got %s", e.lifecycle())
	}
	if o, ok := pairIncompleteEvidence(e.rt()); !ok || o.Kind != pairObligationNativeFundingUncertain {
		t.Fatalf("the result stays incomplete through the hold: %+v %v", o, ok)
	}
	e.restart()
	e.step(pairSlotAt(t0, 7))
	if status, _, _ := e.fundingRow(pairTestHL, eventID); status != pairFundingStatusUnresolvable || e.lifecycle() != pairLifecycleFlat || e.hasObligation(pairObligationFundingIncomplete, pairTestHL) {
		t.Fatalf("a restart keeps the event unresolvable and the pair flat, got %q %s", status, e.lifecycle())
	}
}

func TestBasketPairNativeOperatorRefusesADecisionWhileARecoveredBookingWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial func(ev *nativeCloseEvidence)
	}{
		{name: "no fee record yet", partial: func(ev *nativeCloseEvidence) {
			for i := range ev.Fills {
				ev.Fills[i].FeeKnown, ev.Fills[i].FeeAmount, ev.Fills[i].FeeCurrency = false, "", ""
			}
		}},
		{name: "one of two fee records missing", partial: func(ev *nativeCloseEvidence) {
			ev.Fills[1].FeeKnown, ev.Fills[1].FeeAmount, ev.Fills[1].FeeCurrency = false, "", ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairNativeRig(t)
			e := r.e
			sentAt := e.nativeAt()
			r.fireHL(pairNativeLostFills("1501", "0.06", "the fill lookup failed", false), nil)
			ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
			first := pairNativeEvidence("1501", "sell", sentAt, "59900", "0.9", "USDC", "0.03", "0.03")
			tc.partial(first)
			r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(first, "sell", "BTC")})
			r.step(6 * time.Second)
			e.mu.RLock()
			pending := e.rt().Pair.Progress.nativePendingResolution(ob.Kind, ob.MemberID, ob.ActionID)
			e.mu.RUnlock()
			if pending == nil || r.obligation(pairObligationNativeUnknown, pairTestHL) == nil || len(r.savedResolutions(pairTestHL)) != 0 {
				t.Fatalf("setup: recovered fills with incomplete fees stage a resolution that waits for the book: %+v %v", pending, e.obligationKinds(pairTestHL))
			}
			d, _ := r.discord()
			show := r.command(d, pairNativeTestOwner, "", "", pairNativeOpts("partition", "live", "pair", pairTestID, "member", pairTestHL, "decision", "show"))
			if !strings.Contains(show, "recovered booking") {
				t.Fatalf("show must report the staged recovery: %q", show)
			}
			msg := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "the owner accepts the gap")
			if !strings.Contains(msg, "recovered booking") || len(e.rt().Pair.Progress.NativeRequests) != 0 {
				t.Fatalf("a write-off must be refused while a recovered booking waits: %q", msg)
			}
			r.answer(pairTestHL, pairNativeAnswer{res: pairNativeHistoryOf(pairNativeEvidence("1501", "sell", sentAt, "59900", "0.9", "USDC", "0.03", "0.03"), "sell", "BTC")})
			for i := 0; i < 3; i++ {
				r.step(time.Minute)
			}
			res := r.savedResolutions(pairTestHL)
			if len(res) != 1 || res[0].Mode != pairNativeModeVenueHistory || res[0].FundingHoldFromMs != 0 || len(res[0].FillIDs) != 2 || r.obligation(pairObligationNativeUnknown, pairTestHL) != nil {
				t.Fatalf("the later final fees must commit the venue-history resolution once with no funding hold: %+v %v", res, e.obligationKinds(pairTestHL))
			}
			qty, fee, _ := r.closeQty(pairTestHL)
			if len(e.rt().Pair.Progress.NativeFundingHolds) != 0 || e.signed(pairTestHL) != 0 || !basketQtyEqual(qty, 0.06) || !pairCostNear(fee, 1.8) {
				t.Fatalf("each fill and fee must book once with no hold: holds %+v book %v qty %v fee %v", e.rt().Pair.Progress.NativeFundingHolds, e.signed(pairTestHL), qty, fee)
			}
			a := r.nativeAction(pairTestHL)
			if a.Status != basketActionCommitted || !a.FeeFinal || !basketQtyEqual(a.AppliedQty, 0.06) {
				t.Fatalf("the action must end committed with final fees: %+v", a)
			}
		})
	}
}

func pairFailNativeCommits() func() {
	prev := storeCommitHook
	storeCommitHook = func(storageRole) error {
		pc := make([]uintptr, 64)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for {
			fr, more := frames.Next()
			if strings.HasSuffix(fr.Function, ".commitPairNativeTx") {
				return fmt.Errorf("disk full")
			}
			if !more {
				return nil
			}
		}
	}
	return func() { storeCommitHook = prev }
}

func TestBasketPairNativeOperatorStagedDecisionThatCannotApplyKeepsTheCadence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(r *pairNativeRig) func()
	}{
		{name: "settlement not durable", block: func(r *pairNativeRig) func() {
			restore := pairFailBookSaves(false)
			r.e.mu.Lock()
			r.e.rt().needsDurable = true
			r.e.mu.Unlock()
			return restore
		}},
		{name: "the decision commit fails", block: func(r *pairNativeRig) func() { return pairFailNativeCommits() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairNativeRig(t)
			e := r.e
			r.fireHL(pairNativeLostFills("1601", "0.06", "the fill lookup failed", false), nil)
			ob := r.obligation(pairObligationNativeUnknown, pairTestHL)
			r.step(time.Second)
			d, _ := r.discord()
			if msg := r.decide(d, pairTestHL, ob.Kind, ob.ActionID, pairNativeDecisionWriteOff, "venue history purged"); !strings.Contains(msg, "Staged") {
				t.Fatalf("setup: %q", msg)
			}
			if w := e.bc.wakeDelay(e.now.Add(time.Millisecond)); w != 0 {
				t.Fatalf("a newly staged decision wakes the coordinator once, got %v", w)
			}
			restore := tc.block(r)
			r.step(time.Second)
			if len(e.rt().Pair.Progress.NativeRequests) != 1 || len(r.savedResolutions(pairTestHL)) != 0 {
				t.Fatalf("setup: the decision must stay staged while it cannot apply: %+v", e.rt().Pair.Progress.NativeRequests)
			}
			if w := e.bc.wakeDelay(e.now.Add(time.Millisecond)); w <= 0 {
				t.Fatalf("a staged decision that cannot apply must not wake the coordinator at once on each cycle, got %v", w)
			}
			restore()
			r.step(time.Minute)
			r.step(time.Minute)
			if len(r.savedResolutions(pairTestHL)) != 1 || r.decisionDMs() != 1 || len(e.rt().Pair.Progress.NativeRequests) != 0 || r.obligation(pairObligationNativeUnknown, pairTestHL) != nil {
				t.Fatalf("a later step must apply the staged decision once: %d resolutions %d alerts", len(r.savedResolutions(pairTestHL)), r.decisionDMs())
			}
		})
	}
}
