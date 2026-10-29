package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func quoteFailCommitsAfter(t *testing.T, pass int) func() int {
	t.Helper()
	prev := storeCommitHook
	calls := 0
	failed := 0
	storeCommitHook = func(role storageRole) error {
		calls++
		if calls > pass {
			failed++
			return errQuoteInjected
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	return func() int { return failed }
}

func quoteClearCommitHook(t *testing.T) {
	storeCommitHook = nil
}

func TestQuoteFailureInjectionAppliesEachConfirmedFillOnce(t *testing.T) {
	t.Run("a failed settlement commit reloads and the repeat applies once", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		rev := e.contract().Revision
		d := e.deadlineMs(2)
		e.clock.Set(time.UnixMilli(d))
		_, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100), Verdict: e.verdict(d),
			Decide: func(in quoteDecisionInput) (quoteDecision, error) {
				if in.Snapshot.Inventory != "1/10" && in.Snapshot.Inventory != "0.1" {
					t.Errorf("the callback must see the staged settlement: %q", in.Snapshot.Inventory)
				}
				quoteFailCommitsAfter(t, 0)
				return quoteDecision{Desired: e.pair("99", "101", "0.2"), Progress: &quoteTestProgress{env: e}}, nil
			}})
		quoteClearCommitHook(t)
		if !isQuoteReloadError(err) {
			t.Fatalf("a failed commit must return a typed reload error: %v", err)
		}
		if c := e.contract(); c.Revision != rev || c.Book.Qty != "0" {
			t.Fatalf("the failed commit must leave the durable book unchanged: %+v", c)
		}
		if s := e.owner(); len(s.Positions) != 0 || s.Quote.Revision != rev {
			t.Fatalf("the durable reload must restore memory: %+v rev %d", s.Positions, s.Quote.Revision)
		}
		e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ratEq(t, "inventory after the repeat", e.contract().Book.Qty, "0.1")
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, quoteTestOwner); n != 1 {
			t.Fatalf("the repeat must book the fill once, got %d trades", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM quote_test_progress WHERE deadline_ms = ?`, d); n != 1 {
			t.Fatalf("the progress record must commit with the settlement exactly once: %d", n)
		}
	})
	t.Run("a progress record error aborts the whole commit", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		rev := e.contract().Revision
		d := e.deadlineMs(2)
		e.clock.Set(time.UnixMilli(d))
		_, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100), Verdict: e.verdict(d),
			Decide: func(in quoteDecisionInput) (quoteDecision, error) {
				return quoteDecision{Progress: &quoteTestProgress{env: e, fail: errQuoteInjected}}, nil
			}})
		if !isQuoteReloadError(err) || !errors.Is(err, errQuoteInjected) {
			t.Fatalf("a progress error must abort with a reload error: %v", err)
		}
		if c := e.contract(); c.Revision != rev || c.Book.Qty != "0" {
			t.Fatalf("the aborted commit must not settle: %+v", c)
		}
	})
	t.Run("a failed reservation commit sends nothing", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		d := e.deadlineMs(1)
		e.clock.Set(time.UnixMilli(d))
		res, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100), Verdict: e.verdict(d),
			Decide: func(in quoteDecisionInput) (quoteDecision, error) {
				quoteFailCommitsAfter(t, 1)
				return quoteDecision{Desired: e.pair("99", "101", "0.2"), Progress: &quoteTestProgress{env: e}}, nil
			}})
		quoteClearCommitHook(t)
		if err != nil || res.Generation != 0 || len(res.HoldReasons) == 0 {
			t.Fatalf("a failed reservation must hold with no generation: %+v %v", res, err)
		}
		if n := e.venue.sendCount(); n != 0 {
			t.Fatalf("%d order(s) sent after a failed reservation", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'quote'`); n != 0 {
			t.Fatalf("a rolled-back reservation left %d rows", n)
		}
	})
	t.Run("an accepted order whose response was not saved is recovered and never resent", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		armed := false
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if !armed {
				armed = true
				quoteFailCommitsAfter(t, 0)
			}
			return quoteOrderOutcome{}, false
		}
		e.mustRun(1, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
		quoteClearCommitHook(t)
		e.venue.placeHook = nil
		bid := e.sides(1)[quoteRoleBid]
		if bid.SubmitState != quoteSubmitSubmitting {
			t.Fatalf("the unsaved response must leave the row submitting: %+v", bid)
		}
		sends := e.venue.sendCount()
		e.mustRun(2, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
		bid = e.sides(1)[quoteRoleBid]
		if bid.SubmitState != quoteSubmitResting || bid.VenueOrderID == "" || e.venue.sendCount() != sends {
			t.Fatalf("recovery must find the resting order by client id with no resend: %+v sends %d -> %d", bid, sends, e.venue.sendCount())
		}
	})
	t.Run("a lost order id recovers through client lookup", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if req.Role == quoteRoleBid {
				e.venue.acceptLocked(req, quoteOutcomeResting)
				return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "connection reset after send"}, true
			}
			return quoteOrderOutcome{}, false
		}
		res := e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.placeHook = nil
		g := e.sides(1)
		if g[quoteRoleBid].SubmitState != quoteSubmitUnknown {
			t.Fatalf("the uncertain bid must stay unknown: %+v %+v", g[quoteRoleBid], res)
		}
		sends := e.venue.sendCount()
		e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		g = e.sides(1)
		if g[quoteRoleBid].SubmitState != quoteSubmitResting || g[quoteRoleBid].VenueOrderID == "" || e.venue.sendCount() != sends {
			t.Fatalf("client lookup must recover the lost order id without a resend: %+v", g[quoteRoleBid])
		}
	})
	t.Run("an uncertain absence stays unknown until expiry plus grace and coverage pass", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "timeout"}, true
		}
		e.mustRun(1, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
		e.venue.placeHook = nil
		bid := e.sides(1)[quoteRoleBid]
		grace := entryRecoveryGrace.Milliseconds()
		e.clock.Set(time.UnixMilli(bid.RequestExpiryMs + grace))
		e.adm.resolveUncertain(context.Background())
		if got := e.sides(1)[quoteRoleBid]; got.SubmitState != quoteSubmitUnknown {
			t.Fatalf("at expiry plus grace the absence is still unresolved: %+v", got)
		}
		e.mustRun(4, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
		e.adm.resolveUncertain(context.Background())
		if got := e.sides(1)[quoteRoleBid]; got.SubmitState != quoteSubmitNeverSent {
			t.Fatalf("after expiry plus grace with coverage the absence proves never accepted: %+v", got)
		}
	})
	t.Run("a request at its expiry is never sent", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{quote: map[string]any{"request_expiry_ms": 1}})
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			e.clock.Set(time.UnixMilli(req.ExpiryMs))
			return quoteOrderOutcome{}, false
		}
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		g := e.sides(1)
		rejected, neverSent := 0, 0
		for _, o := range g {
			switch o.SubmitState {
			case quoteSubmitRejected:
				rejected++
			case quoteSubmitNeverSent:
				neverSent++
			}
		}
		if rejected != 1 || neverSent != 1 {
			t.Fatalf("at equality the venue refuses acceptance and the pair stops: %+v", g)
		}
	})
	t.Run("an unknown cancel blocks replacement and holds increases", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.cancelHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "network"}, true
		}
		e.venue.statusHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "network"}, true
		}
		res := e.mustRun(2, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		if len(e.sides(2)) != 0 || len(res.HoldReasons) == 0 {
			t.Fatalf("an unknown cancel must block the replacement: %+v", res)
		}
		e.venue.cancelHook, e.venue.statusHook = nil, nil
		e.mustRun(3, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		e.mustRun(4, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		if g := e.sides(2); len(g) != 2 {
			t.Fatalf("after the cancel resolves the replacement starts: %+v", g)
		}
	})
}

func TestQuoteShutdownKeepsUnknownOutcomesAndStartupRecoversThem(t *testing.T) {
	e := newQuoteTestEnv(t, quoteTestOptions{})
	e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
	e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	e.venue.cancelHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "drain timeout"}, true
	}
	lines := e.adm.ShutdownCleanup(30 * time.Second)
	if len(lines) == 0 {
		t.Fatal("shutdown cleanup must report its work")
	}
	g := e.sides(1)
	if g[quoteRoleAsk].CancelState != quoteCancelUnknown {
		t.Fatalf("an unresolved shutdown cancel must stay unknown: %+v", g[quoteRoleAsk])
	}
	stops := e.stops()
	if len(stops) != 1 || stops[0].SubmitState != quoteSubmitResting || stops[0].CancelState != quoteCancelNone {
		t.Fatalf("the confirmed stop must stay resting on nonzero inventory: %+v", stops)
	}
	sends := e.venue.sendCount()
	if res, err := e.run(3, 100, e.pair("98", "102", "0.2"), quoteExitRequest{}); err != nil || e.venue.sendCount() != sends || len(res.HoldReasons) == 0 {
		t.Fatalf("after shutdown no new intent may be sent: %+v %v", res, err)
	}
	e.venue.cancelHook = nil
	e.reopen()
	if _, err := RecoverOnStart(context.Background(), e.adm); err != nil {
		t.Fatal(err)
	}
	if g := e.sides(1); g[quoteRoleAsk].SubmitState != quoteSubmitCancelled || g[quoteRoleAsk].CancelState != quoteCancelConfirmed {
		t.Fatalf("startup recovery must resolve the requested cancel: %+v", g[quoteRoleAsk])
	}
}
