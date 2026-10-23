package main

import (
	"strings"
	"testing"
	"time"
)

func TestQuoteLifecycleSettlesOpposingFillsRoundTripsAndReversalsOnce(t *testing.T) {
	e := newQuoteTestEnv(t, quoteTestOptions{})
	pair := e.pair("99", "101", "0.2")

	r1 := e.mustRun(1, 100, pair, quoteExitRequest{})
	if r1.Generation != 1 || len(r1.HoldReasons) != 0 {
		t.Fatalf("first deadline %+v", r1)
	}
	g1 := e.sides(1)
	if g1[quoteRoleBid] == nil || g1[quoteRoleBid].SubmitState != quoteSubmitResting || g1[quoteRoleAsk].SubmitState != quoteSubmitResting {
		t.Fatalf("both sides must rest after the first deadline: %+v", g1)
	}
	if c := e.contract(); c == nil || c.Revision < 1 || c.Book.Qty != "0" {
		t.Fatalf("contract %+v", c)
	}

	bidCID := g1[quoteRoleBid].ClientOrderID
	askCID := g1[quoteRoleAsk].ClientOrderID
	e.venue.fill(t, bidCID, "0.1", "99", "0.01", e.deadlineMs(1)+10_000)
	r2 := e.mustRun(2, 100, pair, quoteExitRequest{})
	if r2.SettlementKind != quoteSettleFills && r2.SettlementKind != quoteSettleBoth {
		t.Fatalf("second deadline must settle the bid fill: %+v", r2)
	}
	c := e.contract()
	ratEq(t, "inventory", c.Book.Qty, "0.1")
	ratEq(t, "cost", c.Book.Cost, "9.9")
	ratEq(t, "cash", c.Book.Cash, "999.99")
	ratEq(t, "risk anchor", c.Book.RiskAnchorPx, "99")
	s := e.owner()
	if pos := s.Positions["BTC"]; pos == nil || pos.Side != "long" || pos.Quantity != 0.1 || pos.AvgCost != 99 {
		t.Fatalf("projected position %+v", pos)
	}
	stops := e.stops()
	if len(stops) != 1 || stops[0].SubmitState != quoteSubmitResting || stops[0].Request.Side != "sell" || stops[0].Request.Size != "0.1" {
		t.Fatalf("a resting sell stop must cover the settled long: %+v", stops)
	}
	ratEq(t, "stop trigger", stops[0].Request.TriggerPx, "94.05")
	if e.sides(1)[quoteRoleBid].RowID != g1[quoteRoleBid].RowID || r2.Generation != 1 {
		t.Fatalf("an identical desired side must keep its identity: %+v", r2)
	}

	e.venue.fill(t, askCID, "0.2", "101", "0.02", e.deadlineMs(2)+5_000)
	e.mustRun(3, 100, pair, quoteExitRequest{})
	c = e.contract()
	ratEq(t, "reversed inventory", c.Book.Qty, "-0.1")
	ratEq(t, "realized gross", c.Book.RealizedGross, "0.2")
	ratEq(t, "fees", c.Book.Fees, "0.03")
	ratEq(t, "cash after reversal", c.Book.Cash, "1000.17")
	ratEq(t, "residual cost", c.Book.Cost, "10.1")
	ratEq(t, "residual anchor", c.Book.RiskAnchorPx, "101")
	if c.Book.FirstOpenMs != e.deadlineMs(2)+5_000 {
		t.Fatalf("the residual open time must be the reversal fill time: %d", c.Book.FirstOpenMs)
	}
	stops = e.stops()
	var resting, cancelled int
	for _, st := range stops {
		switch {
		case st.SubmitState == quoteSubmitResting && st.Request.Side == "buy" && st.Request.Size == "0.1":
			resting++
			ratEq(t, "residual stop trigger", st.Request.TriggerPx, "106.05")
		case st.SubmitState == quoteSubmitCancelled && st.Request.Side == "sell":
			cancelled++
		}
	}
	if resting != 1 || cancelled != 1 || len(stops) != 2 {
		t.Fatalf("the reversal must cancel the long stop and place one residual buy stop: %+v", stops)
	}
	closes := e.count(`SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ?`, quoteTestOwner)
	if closes != 1 {
		t.Fatalf("closed records %d, want 1", closes)
	}
	opens := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0 AND trade_type = 'perps'`, quoteTestOwner)
	reduces := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 1`, quoteTestOwner)
	if opens != 2 || reduces != 1 {
		t.Fatalf("trades opens=%d reduces=%d", opens, reduces)
	}
	if n := e.count(`SELECT COUNT(*) FROM quote_fills WHERE settlement_id = 0`); n != 0 {
		t.Fatalf("%d fill(s) left unapplied", n)
	}
	var fees float64
	if err := e.db().db.QueryRow(`SELECT SUM(exchange_fee) FROM trades WHERE strategy_id = ?`, quoteTestOwner).Scan(&fees); err != nil {
		t.Fatal(err)
	}
	if fees < 0.0299999 || fees > 0.0300001 {
		t.Fatalf("trade fees %v, want 0.03 allocated exactly once", fees)
	}

	before := e.contract()
	e.reopen()
	if got := e.contract(); got.Revision != before.Revision || got.Book != before.Book {
		t.Fatalf("reload changed the durable book: %+v vs %+v", got, before)
	}
	s = e.owner()
	if pos := s.Positions["BTC"]; pos == nil || pos.Side != "short" || pos.Quantity != 0.1 || s.Quote == nil || s.Quote.Revision != before.Revision {
		t.Fatalf("reloaded owner %+v %+v", pos, s.Quote)
	}
	e.clock.Advance(time.Minute)
	e.mustRun(4, 100, pair, quoteExitRequest{})
	if n := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, quoteTestOwner); n != 3 {
		t.Fatalf("a repeated recovery after restart must book nothing new: %d trades", n)
	}
}

func quoteRestingBid(t *testing.T, e *quoteTestEnv) *quoteOrderRow {
	t.Helper()
	e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	bid := e.sides(1)[quoteRoleBid]
	if bid == nil || bid.SubmitState != quoteSubmitResting {
		t.Fatalf("bid %+v", bid)
	}
	return bid
}

func TestQuoteRecoveryHoldsEveryUnprovenPrefix(t *testing.T) {
	pair := func(e *quoteTestEnv) quoteDesiredPair { return e.pair("99", "101", "0.2") }
	t.Run("an exact repeat applies once", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		f := e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Fills: []quoteRawFill{f, f}, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		e.mustRun(2, 100, pair(e), quoteExitRequest{})
		e.mustRun(3, 100, pair(e), quoteExitRequest{})
		ratEq(t, "inventory", e.contract().Book.Qty, "0.1")
		if n := e.count(`SELECT COUNT(*) FROM quote_fills`); n != 1 {
			t.Fatalf("%d evidence rows, want 1", n)
		}
	})
	t.Run("a conflicting copy holds and keeps the stored evidence", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		f := e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.mustRun(2, 100, pair(e), quoteExitRequest{})
		rev := e.contract().Revision
		changed := f
		changed.Fee = "0.02"
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Fills: []quoteRawFill{changed}, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		res := e.mustRun(3, 100, pair(e), quoteExitRequest{})
		if len(res.SettlementHold) == 0 || e.contract().Revision != rev {
			t.Fatalf("a conflicting copy must hold the prefix: %+v rev %d -> %d", res, rev, e.contract().Revision)
		}
		ratEq(t, "inventory", e.contract().Book.Qty, "0.1")
		if n := e.count(`SELECT COUNT(*) FROM quote_fills WHERE fee = '0.01'`); n != 1 {
			t.Fatalf("the stored copy must stay unchanged")
		}
	})
	t.Run("incomplete coverage never settles a zero and holds increases", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		quoteRestingBid(t, e)
		rev := e.contract().Revision
		boundary := e.contract().AppliedBoundaryMs
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Coverage: quoteFillCoverage{Complete: false, FromMs: since, Reason: "retention limit reached"}}, nil, true
		}
		res := e.mustRun(2, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		if len(res.SettlementHold) == 0 || e.contract().Revision != rev || e.contract().AppliedBoundaryMs != boundary {
			t.Fatalf("incomplete history must hold: %+v", res)
		}
		if g := e.sides(2); len(g) != 0 {
			t.Fatalf("no replacement generation may start while the prefix holds: %+v", g)
		}
		if s := e.sides(1); s[quoteRoleBid].CancelState == quoteCancelNone || s[quoteRoleAsk].CancelState == quoteCancelNone {
			t.Fatalf("held settlement must cancel risk-increasing quotes: %+v", s)
		}
	})
	t.Run("a unique tied chain applies and an ambiguous tie holds", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		s := e.sides(1)
		at := e.deadlineMs(1) + 2000
		e.venue.fill(t, s[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", at)
		e.venue.fill(t, s[quoteRoleAsk].ClientOrderID, "0.05", "101", "0.005", at)
		e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ratEq(t, "tied chain inventory", e.contract().Book.Qty, "0.05")
	})
	t.Run("two distinct tied chains hold", func(t *testing.T) {
		e2 := newQuoteTestEnv(t, quoteTestOptions{})
		e2.mustRun(1, 100, e2.pair("99", "101", "0.2"), quoteExitRequest{})
		s2 := e2.sides(1)
		at2 := e2.deadlineMs(1) + 2000
		a := e2.venue.fill(t, s2[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", at2)
		b := e2.venue.fill(t, s2[quoteRoleAsk].ClientOrderID, "0.1", "101", "0.01", at2)
		c := a
		c.TradeID, c.Px, c.StartPosition = "77777", "98", "0"
		c.Raw = `{"tid":"77777"}`
		e2.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Fills: []quoteRawFill{a, b, c}, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		res := e2.mustRun(2, 100, e2.pair("99", "101", "0.2"), quoteExitRequest{})
		if len(res.SettlementHold) == 0 {
			t.Fatalf("two distinct chains at one event time must hold: %+v", res)
		}
		ratEq(t, "ambiguous inventory", e2.contract().Book.Qty, "0")
	})
	t.Run("an unrelated fill holds the prefix", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		f := e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		alien := f
		alien.OrderID, alien.ClientOrderID, alien.TradeID, alien.Raw = "424242", "0xforeign", "999", `{"alien":true}`
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Fills: []quoteRawFill{f, alien}, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		res := e.mustRun(2, 100, pair(e), quoteExitRequest{})
		if len(res.SettlementHold) == 0 {
			t.Fatalf("an unattributable fill must hold: %+v", res)
		}
		ratEq(t, "inventory", e.contract().Book.Qty, "0")
	})
	t.Run("fills that keep arriving across the position read hold after bounded retries", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		n := 0
		e.venue.afterPosition = func() {
			n++
			e.venue.fill(t, bid.ClientOrderID, "0.01", "99", "0.001", e.clock.Now().UnixMilli())
		}
		res := e.mustRun(2, 100, pair(e), quoteExitRequest{})
		e.venue.afterPosition = nil
		if len(res.SettlementHold) == 0 || n < quoteRecoveryMaxRetries {
			t.Fatalf("an unstable prefix must hold after %d attempts: %+v (attempts %d)", quoteRecoveryMaxRetries, res, n)
		}
		ratEq(t, "inventory", e.contract().Book.Qty, "0")
	})
	t.Run("a fresh position mismatch holds", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.venue.positionHook = func() (quotePositionRead, error, bool) {
			return quotePositionRead{Account: e.venue.account, Coin: "BTC", Size: "0.3", ReadAtMs: e.clock.Now().UnixMilli()}, nil, true
		}
		res := e.mustRun(2, 100, pair(e), quoteExitRequest{})
		if len(res.SettlementHold) == 0 {
			t.Fatalf("an exposure mismatch must hold: %+v", res)
		}
		ratEq(t, "inventory", e.contract().Book.Qty, "0")
	})
	t.Run("a late fill before the applied boundary is a contradiction", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		e.mustRun(2, 100, pair(e), quoteExitRequest{})
		boundary := e.contract().AppliedBoundaryMs
		e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", boundary-1)
		res := e.mustRun(3, 100, pair(e), quoteExitRequest{})
		if len(res.SettlementHold) == 0 {
			t.Fatalf("a fill earlier than the applied boundary must hold: %+v", res)
		}
		if n := e.count(`SELECT COUNT(*) FROM quote_fills WHERE settlement_id = 0`); n != 1 {
			t.Fatalf("the contradicting fill must be kept as unapplied evidence, got %d", n)
		}
	})
	t.Run("a fee correction applies once with no quantity replay", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		f := e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.mustRun(2, 100, pair(e), quoteExitRequest{})
		corr := f
		corr.Kind, corr.CorrectsTrade, corr.TradeID, corr.Fee, corr.TimeMs, corr.Raw = "fee_correction", f.TradeID, "corr-1", "0.004", e.deadlineMs(2)+1000, `{"correction":1}`
		base := e.venue.fills
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			fills := append(append([]quoteRawFill(nil), base...), corr)
			return quoteFillRecovery{Fills: fills, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		e.mustRun(3, 100, pair(e), quoteExitRequest{})
		e.mustRun(4, 100, pair(e), quoteExitRequest{})
		c := e.contract()
		ratEq(t, "inventory after correction", c.Book.Qty, "0.1")
		ratEq(t, "fees after correction", c.Book.Fees, "0.004")
		ratEq(t, "cash after correction", c.Book.Cash, "999.996")
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = ?`, quoteFeeAdjustmentType); n != 1 {
			t.Fatalf("fee adjustment rows %d, want 1", n)
		}
	})
	t.Run("an unsupported fee asset holds", func(t *testing.T) {
		e2 := newQuoteTestEnv(t, quoteTestOptions{})
		bid2 := quoteRestingBid(t, e2)
		odd := e2.venue.fill(t, bid2.ClientOrderID, "0.1", "99", "0.01", e2.deadlineMs(1)+1000)
		odd.FeeAsset = "HYPE"
		e2.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Fills: []quoteRawFill{odd}, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		res := e2.mustRun(2, 100, e2.pair("99", "101", "0.2"), quoteExitRequest{})
		if len(res.SettlementHold) == 0 {
			t.Fatalf("an unsupported fee asset must hold: %+v", res)
		}
		ratEq(t, "inventory", e2.contract().Book.Qty, "0")
	})
}

func TestQuoteRacesRoundTripsAndReconnectsSettleThroughOneSink(t *testing.T) {
	t.Run("a flat round trip keeps cash, fees and trades and cancels the stop", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		pair := e.pair("99", "101", "0.2")
		e.mustRun(1, 100, pair, quoteExitRequest{})
		g := e.sides(1)
		e.venue.fill(t, g[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.venue.fill(t, g[quoteRoleAsk].ClientOrderID, "0.1", "101", "0.02", e.deadlineMs(1)+2000)
		e.mustRun(2, 100, pair, quoteExitRequest{})
		c := e.contract()
		ratEq(t, "flat inventory", c.Book.Qty, "0")
		ratEq(t, "round-trip cash", c.Book.Cash, "1000.17")
		ratEq(t, "round-trip fees", c.Book.Fees, "0.03")
		if c.Book.PositionID != "" || c.Book.FirstOpenMs != 0 {
			t.Fatalf("a flat book clears its position identity and open time: %+v", c.Book)
		}
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, quoteTestOwner); n != 2 {
			t.Fatalf("the round trip keeps both trades: %d", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ?`, quoteTestOwner); n != 1 {
			t.Fatalf("closed records %d", n)
		}
		for _, st := range e.stops() {
			if st.mayRest() {
				t.Fatalf("a flat book must not keep a resting stop: %+v", st)
			}
		}
	})
	t.Run("a fill that races a requested cancel settles before the replacement", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ask := e.sides(1)[quoteRoleAsk]
		e.venue.cancelHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if req.ClientOrderID != ask.ClientOrderID {
				return quoteOrderOutcome{}, false
			}
			o := e.venue.orders[req.ClientOrderID]
			e.venue.fillLocked(o, "0.05", "101", "0.005", e.venue.nowMs())
			o.status = quoteOutcomeCancelled
			return e.venue.outcomeOf(o), true
		}
		res := e.mustRun(2, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		if len(e.sides(2)) != 0 || len(res.HoldReasons) == 0 {
			t.Fatalf("the replacement must wait for the raced fill to settle: %+v", res)
		}
		e.venue.cancelHook = nil
		e.mustRun(3, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		ratEq(t, "raced fill inventory", e.contract().Book.Qty, "-0.05")
		e.mustRun(4, 100, e.pair("98", "102", "0.2"), quoteExitRequest{})
		if g := e.sides(2); len(g) != 2 {
			t.Fatalf("after settlement the replacement starts: %+v", g)
		}
	})
	t.Run("a disconnect holds and the reconnect settles once", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{}, errQuoteInjected, true
		}
		if res := e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{}); len(res.SettlementHold) == 0 {
			t.Fatalf("a disconnected history read must hold: %+v", res)
		}
		e.venue.recoverHook = nil
		e.venue.positionHook = func() (quotePositionRead, error, bool) { return quotePositionRead{}, errQuoteInjected, true }
		if res := e.mustRun(3, 100, e.pair("99", "101", "0.2"), quoteExitRequest{}); len(res.SettlementHold) == 0 {
			t.Fatalf("a disconnected position read must hold: %+v", res)
		}
		e.venue.positionHook = nil
		e.mustRun(4, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.mustRun(5, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ratEq(t, "inventory after reconnect", e.contract().Book.Qty, "0.1")
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, quoteTestOwner); n != 1 {
			t.Fatalf("the reconnect must book once: %d", n)
		}
	})
	for _, stopFirst := range []bool{true, false} {
		stopFirst := stopFirst
		name := "a quote fill before a stop fill"
		if stopFirst {
			name = "a stop fill before a quote fill"
		}
		t.Run(name+" settles in event order through the one sink", func(t *testing.T) {
			e := newQuoteTestEnv(t, quoteTestOptions{})
			pair := e.pair("99", "101", "0.2")
			e.mustRun(1, 100, pair, quoteExitRequest{})
			g := e.sides(1)
			e.venue.fill(t, g[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
			e.mustRun(2, 100, pair, quoteExitRequest{})
			stop := e.stops()[0]
			at := e.deadlineMs(2) + 1000
			if stopFirst {
				e.venue.fill(t, stop.ClientOrderID, "0.1", "94.05", "0.01", at)
				e.venue.fill(t, g[quoteRoleBid].ClientOrderID, "0.1", "94", "0.01", at+1)
			} else {
				e.venue.fill(t, g[quoteRoleBid].ClientOrderID, "0.1", "94", "0.01", at)
				e.venue.fill(t, stop.ClientOrderID, "0.1", "94.05", "0.01", at+1)
			}
			e.mustRun(3, 100, quoteDesiredPair{}, quoteExitRequest{})
			c := e.contract()
			ratEq(t, "inventory after the race", c.Book.Qty, "0.1")
			if n := e.count(`SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND exchange_order_id LIKE 'quote:%'`, quoteTestOwner); n != 3 {
				t.Fatalf("every fill books once through the quote sink: %d", n)
			}
			if n := e.count(`SELECT COUNT(*) FROM quote_fills WHERE role = 'stop' AND settlement_id != 0`); n != 1 {
				t.Fatalf("the stop fill must settle through the sink: %d", n)
			}
			if n := e.count(`SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ? AND close_reason = ?`, quoteTestOwner, quoteCloseReasonStop); stopFirst && n != 1 {
				t.Fatalf("a stop that flattens records a stop close: %d", n)
			}
		})
	}
}

func TestQuoteFeeCorrectionsOfOneFillChainFromThePreviousCorrection(t *testing.T) {
	setup := func(t *testing.T) (*quoteTestEnv, quoteRawFill, func(...quoteRawFill)) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		bid := quoteRestingBid(t, e)
		f := e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "1.0", e.deadlineMs(1)+1000)
		e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ratEq(t, "cash after the fill", e.contract().Book.Cash, "999")
		base := append([]quoteRawFill(nil), e.venue.fills...)
		var extra []quoteRawFill
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			fills := append(append([]quoteRawFill(nil), base...), extra...)
			return quoteFillRecovery{Fills: fills, Coverage: quoteFillCoverage{Complete: true, FromMs: since, ThroughMs: until}}, nil, true
		}
		add := func(c ...quoteRawFill) { extra = append(extra, c...) }
		return e, f, add
	}
	corr := func(f quoteRawFill, id, fee string, tMs int64) quoteRawFill {
		c := f
		c.Kind, c.CorrectsTrade, c.TradeID, c.Fee, c.TimeMs, c.Raw = "fee_correction", f.TradeID, id, fee, tMs, `{"correction":"`+id+`"}`
		return c
	}
	adjustments := func(e *quoteTestEnv) int {
		return e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = ?`, quoteFeeAdjustmentType)
	}
	t.Run("two corrections in one settlement book only the newest fee", func(t *testing.T) {
		e, f, add := setup(t)
		add(corr(f, "corr-a", "1.2", e.deadlineMs(2)+1000), corr(f, "corr-b", "1.5", e.deadlineMs(2)+2000))
		e.mustRun(3, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.mustRun(4, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		if n := e.count(`SELECT COUNT(*) FROM quote_fills WHERE kind = 'fee_correction' AND settlement_id != 0`); n != 2 {
			t.Fatalf("both corrections must settle in one settlement, applied %d", n)
		}
		if n := e.count(`SELECT COUNT(DISTINCT settlement_id) FROM quote_fills WHERE kind = 'fee_correction'`); n != 1 {
			t.Fatalf("the corrections must share one settlement, got %d", n)
		}
		c := e.contract()
		ratEq(t, "cash after two corrections", c.Book.Cash, "998.5")
		ratEq(t, "fees after two corrections", c.Book.Fees, "1.5")
		if n := adjustments(e); n != 2 {
			t.Fatalf("fee adjustment rows %d, want 2", n)
		}
	})
	t.Run("corrections in two settlements chain from the applied one", func(t *testing.T) {
		e, f, add := setup(t)
		add(corr(f, "corr-a", "1.2", e.deadlineMs(2)+1000))
		e.mustRun(3, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.mustRun(4, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		ratEq(t, "cash after the first correction", e.contract().Book.Cash, "998.8")
		add(corr(f, "corr-b", "1.5", e.deadlineMs(4)+1000))
		e.mustRun(5, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.mustRun(6, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		c := e.contract()
		ratEq(t, "cash after the second correction", c.Book.Cash, "998.5")
		ratEq(t, "fees after the second correction", c.Book.Fees, "1.5")
		var second float64
		if err := e.db().db.QueryRow(`SELECT exchange_fee FROM trades WHERE trade_type = ? ORDER BY rowid DESC LIMIT 1`, quoteFeeAdjustmentType).Scan(&second); err != nil {
			t.Fatal(err)
		}
		if second < 0.3-1e-12 || second > 0.3+1e-12 {
			t.Fatalf("the second correction delta is %v, want 0.3", second)
		}
	})
	t.Run("corrections in reverse row id order hold", func(t *testing.T) {
		e, f, add := setup(t)
		add(corr(f, "corr-b", "1.5", e.deadlineMs(2)+2000), corr(f, "corr-a", "1.2", e.deadlineMs(2)+1000))
		var held bool
		for n := 3; n <= 4; n++ {
			res := e.mustRun(n, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
			if len(res.SettlementHold) > 0 {
				held = true
			}
		}
		if !held {
			t.Fatal("corrections whose row ids run against their order must hold the settlement")
		}
		c := e.contract()
		ratEq(t, "cash with held corrections", c.Book.Cash, "999")
		ratEq(t, "fees with held corrections", c.Book.Fees, "1")
		if n := adjustments(e); n != 0 {
			t.Fatalf("a held settlement books no fee adjustment, got %d", n)
		}
	})
}

func TestQuotePartlyFilledCancelledOrderResolvesOnceItsFillsSettle(t *testing.T) {
	cases := []struct {
		name, size, filled string
		fills              []string
		restart            bool
	}{
		{name: "origSz 1.1 and sz 0.3 report the exact filled text", size: "1.1", filled: "0.8", fills: []string{"0.5", "0.3"}},
		{name: "origSz 0.3 and sz 0.1 report the exact filled text", size: "0.3", filled: "0.2", fills: []string{"0.2"}},
		{name: "a float-noisy low filled text is raised to the applied quantity", size: "0.3", filled: "0.19999999999999998", fills: []string{"0.2"}},
		{name: "a cancel outcome with no filled quantity stays resolved after a restart", size: "0.3", filled: "", fills: []string{"0.2"}, restart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newQuoteTestEnv(t, quoteTestOptions{quote: map[string]any{"max_order_qty": 2}})
			pair := e.pair("99", "", tc.size)
			e.mustRun(1, 100, pair, quoteExitRequest{})
			bid := e.sides(1)[quoteRoleBid]
			if bid == nil || bid.SubmitState != quoteSubmitResting {
				t.Fatalf("bid %+v", bid)
			}
			for i, sz := range tc.fills {
				e.venue.fill(t, bid.ClientOrderID, sz, "99", "0", e.deadlineMs(1)+int64(1000*(i+1)))
			}
			e.venue.statusHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
				if !strings.EqualFold(req.ClientOrderID, bid.ClientOrderID) {
					return quoteOrderOutcome{}, false
				}
				return quoteOrderOutcome{Status: quoteOutcomeCancelled, VenueOrderID: bid.VenueOrderID, VenueTimeMs: e.deadlineMs(1) + 5000, FilledQty: tc.filled, Detail: "canceled"}, true
			}
			e.mustRun(2, 100, e.pair("", "", ""), quoteExitRequest{})
			e.mustRun(3, 100, e.pair("", "", ""), quoteExitRequest{})
			if tc.restart {
				e.reopen()
			}
			got := e.sides(1)[quoteRoleBid]
			if got.SubmitState != quoteSubmitCancelled || !got.resolved() {
				t.Fatalf("the cancelled bid must be resolved once its fills settle: state %s confirmed %s applied %s", got.SubmitState, got.ConfirmedQty, got.AppliedQty)
			}
			want := "0"
			for _, sz := range tc.fills {
				want = quoteRatString(quoteRatAdd(mustQuoteRat(want), mustQuoteRat(sz)))
			}
			ratEq(t, "settled inventory", e.contract().Book.Qty, want)
		})
	}
}
