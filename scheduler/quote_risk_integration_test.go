package main

import (
	"context"
	"testing"
	"time"
)

func quoteLongInventoryEnv(t *testing.T, o quoteTestOptions) *quoteTestEnv {
	t.Helper()
	e := newQuoteTestEnv(t, o)
	e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
	e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	ratEq(t, "long inventory", e.contract().Book.Qty, "0.1")
	return e
}

func quoteAssertIncreasesHeld(t *testing.T, e *quoteTestEnv, res quoteDeadlineResult, label string) {
	t.Helper()
	if len(res.HoldReasons) == 0 {
		t.Fatalf("%s: increases must be held: %+v", label, res)
	}
	if g := e.sides(2); len(g) != 0 {
		t.Fatalf("%s: no new generation may start while held: %+v", label, g)
	}
	for role, o := range e.sides(1) {
		if o.increasesRisk() && o.mayRest() && o.CancelState == quoteCancelNone {
			t.Fatalf("%s: the risk-increasing %s must be cancelled: %+v", label, role, o)
		}
	}
}

func TestQuoteHoldsCancelIncreasesWhileReductionsAndProtectionContinue(t *testing.T) {
	type holdCase struct {
		name  string
		arm   func(e *quoteTestEnv)
		patch func(e *quoteTestEnv, req *quoteDeadlineRequest)
	}
	cases := []holdCase{
		{name: "pause", arm: func(e *quoteTestEnv) { e.adm.sc.Paused = true }},
		{name: "circuit breaker", arm: func(e *quoteTestEnv) {
			e.adm.mu.Lock()
			s := e.adm.state.Strategies[quoteTestOwner]
			s.RiskState.CircuitBreaker, s.RiskState.CircuitBreakerUntil = true, e.clock.Now().Add(time.Hour)
			e.adm.mu.Unlock()
		}},
		{name: "daily loss", arm: func(e *quoteTestEnv) {
			e.adm.cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25, DailyMaxLossUSD: 1}
			e.adm.mu.Lock()
			s := e.adm.state.Strategies[quoteTestOwner]
			s.RiskState.DailyPnL, s.RiskState.DailyPnLDate = -2, e.clock.Now().UTC().Format("2006-01-02")
			e.adm.mu.Unlock()
		}},
		{name: "kill switch", arm: func(e *quoteTestEnv) {
			e.adm.mu.Lock()
			e.adm.state.partitionRisk(e.part).KillSwitchActive = true
			e.adm.mu.Unlock()
		}},
		{name: "persistence hold", arm: func(e *quoteTestEnv) { e.store.recordSaveOutcome(e.part, errQuoteInjected) }},
		{name: "operator hold", arm: func(e *quoteTestEnv) {
			if err := e.adm.OperatorHold(true, "maintenance"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing verdict", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) { req.Verdict = nil }},
		{name: "unhealthy verdict", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) {
			req.Verdict.Status, req.Verdict.Reasons = quoteVerdictUnhealthy, []string{"feed disconnected"}
		}},
		{name: "out-of-order verdict", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) { req.Verdict.DeadlineMs-- }},
		{name: "incompatible verdict", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) { req.Verdict.PolicyFingerprint = "other" }},
		{name: "stale mark", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) {
			req.Marks[0].EvidenceMs = req.DeadlineMs - req.Marks[0].MaxAgeMs - 1
		}},
		{name: "mark received after the deadline", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) { req.Marks[0].EvidenceMs = req.DeadlineMs + 1 }},
		{name: "failed evaluation", patch: func(e *quoteTestEnv, req *quoteDeadlineRequest) {
			req.Decide = func(quoteDecisionInput) (quoteDecision, error) { return quoteDecision{}, errQuoteInjected }
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := quoteLongInventoryEnv(t, quoteTestOptions{})
			if c.arm != nil {
				c.arm(e)
			}
			d := e.deadlineMs(3)
			e.clock.Set(time.UnixMilli(d))
			reducing := quoteExitRequest{Kind: quoteExitKindReducing, ReducingAsk: &quoteSideRequest{Px: "102", Size: "0.1"}}
			req := quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100), Verdict: e.verdict(d), Decide: func(quoteDecisionInput) (quoteDecision, error) {
				return quoteDecision{Desired: e.pair("98", "102", "0.2"), Exit: reducing, Progress: &quoteTestProgress{env: e}}, nil
			}}
			if c.patch != nil {
				c.patch(e, &req)
			}
			res, err := RunDeadline(context.Background(), e.adm, req)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.HoldReasons) == 0 {
				t.Fatalf("increases must be held: %+v", res)
			}
			for role, o := range e.sides(1) {
				if o.increasesRisk() && o.mayRest() && o.CancelState == quoteCancelNone {
					t.Fatalf("the risk-increasing %s must be cancelled: %+v", role, o)
				}
			}
			for _, o := range e.sides(2) {
				if !o.Request.ReduceOnly && o.SubmitState != quoteSubmitNeverSent {
					t.Fatalf("a held deadline may only send reduce-only quotes: %+v", o)
				}
			}
			stops := e.stops()
			if len(stops) != 1 || stops[0].SubmitState != quoteSubmitResting || stops[0].CancelState != quoteCancelNone {
				t.Fatalf("verified protection must continue: %+v", stops)
			}
			ratEq(t, "settled inventory", e.contract().Book.Qty, "0.1")
		})
	}
	t.Run("a held deadline still places a verified reducing ask", func(t *testing.T) {
		e := quoteLongInventoryEnv(t, quoteTestOptions{})
		e.adm.sc.Paused = true
		reducing := quoteExitRequest{Kind: quoteExitKindReducing, ReducingAsk: &quoteSideRequest{Px: "102", Size: "0.1"}}
		e.mustRun(3, 100, quoteDesiredPair{}, quoteExitRequest{})
		e.mustRun(4, 100, quoteDesiredPair{}, quoteExitRequest{})
		res := e.mustRun(5, 100, e.pair("98", "102", "0.2"), reducing)
		var reduceOnly *quoteOrderRow
		for _, o := range e.sides(res.Generation) {
			if o.Role == quoteRoleAsk {
				reduceOnly = o
			}
		}
		if reduceOnly == nil || !reduceOnly.Request.ReduceOnly || reduceOnly.SubmitState != quoteSubmitResting {
			t.Fatalf("a verified reduction must stay available while increases are held: %+v (gen %d)", reduceOnly, res.Generation)
		}
	})
}

func TestQuoteRiskEnvelopeAppliesBoundaryRulesAtEquality(t *testing.T) {
	t.Run("marked inventory cap passes at equality", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{quote: map[string]any{"max_inventory_usd": 20, "margin_price_buffer_pct": 0}})
		res := e.mustRun(1, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
		if res.Generation != 1 {
			t.Fatalf("a reading equal to the cap must pass: %+v", res)
		}
	})
	t.Run("marked inventory cap fails one lot above", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{quote: map[string]any{"max_inventory_usd": 20, "margin_price_buffer_pct": 0}})
		res := e.mustRun(1, 100, e.pair("99", "", "0.201"), quoteExitRequest{})
		if res.Generation != 0 || len(res.HoldReasons) == 0 || e.venue.sendCount() != 0 {
			t.Fatalf("a reading above the cap must refuse the reservation: %+v", res)
		}
	})
	t.Run("free margin passes at equality", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{strategy: map[string]any{"capital": 4}, quote: map[string]any{"fee_reserve_rate": 0, "margin_price_buffer_pct": 0}})
		if res := e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{}); res.Generation != 1 {
			t.Fatalf("available margin equal to the requirement must pass: %+v", res)
		}
	})
	t.Run("free margin fails one cent below", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{strategy: map[string]any{"capital": 3.99}, quote: map[string]any{"fee_reserve_rate": 0, "margin_price_buffer_pct": 0}})
		res := e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		if res.Generation != 0 || e.venue.sendCount() != 0 {
			t.Fatalf("insufficient margin must refuse the pair: %+v", res)
		}
	})
	t.Run("a mark at exactly the maximum age is fresh", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		d := e.deadlineMs(1)
		e.clock.Set(time.UnixMilli(d))
		marks := e.marks(d, 100)
		marks[0].EvidenceMs = d - marks[0].MaxAgeMs
		res, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: marks, Verdict: e.verdict(d), Decide: func(quoteDecisionInput) (quoteDecision, error) {
			return quoteDecision{Desired: e.pair("99", "101", "0.2")}, nil
		}})
		if err != nil || res.Generation != 1 {
			t.Fatalf("a mark at the age limit must pass: %+v %v", res, err)
		}
	})
	t.Run("an earlier deadline refuses and an equal deadline repeats with the same generation", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		first := e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		sends := e.venue.sendCount()
		again := e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		if again.Generation != first.Generation || e.venue.sendCount() != sends {
			t.Fatalf("a repeated deadline must reuse its generation and send nothing: %+v %+v", first, again)
		}
		if _, err := e.run(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{}); err == nil {
			t.Fatal("an earlier deadline must be refused")
		}
		d := e.deadlineMs(2) + 30_000
		if _, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100), Verdict: e.verdict(d)}); err == nil {
			t.Fatal("a deadline inside the effective cadence is not due")
		}
	})
}

func TestQuoteStopExitAndKillSwitchStayTheSoleInventoryAuthorities(t *testing.T) {
	t.Run("the liquidation clamp tightens at equality", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.venue.liqPx = 94.05
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		stops := e.stops()
		if len(stops) != 1 {
			t.Fatalf("stops %+v", stops)
		}
		ratEq(t, "clamped trigger", stops[0].Request.TriggerPx, "94.521")
	})
	t.Run("an unknown stop placement alerts and holds every increase", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if req.Role == quoteRoleStop {
				return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "timeout"}, true
			}
			return quoteOrderOutcome{}, false
		}
		e.venue.statusHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if req.Role == quoteRoleStop {
				return quoteOrderOutcome{Status: quoteOutcomeUnknown}, true
			}
			return quoteOrderOutcome{}, false
		}
		res := e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		if len(res.HoldReasons) == 0 {
			t.Fatalf("no confirmed stop on nonzero inventory must hold increases: %+v", res)
		}
		stops := e.stops()
		if len(stops) != 1 || stops[0].SubmitState != quoteSubmitUnknown {
			t.Fatalf("the uncertain stop must stay unresolved and never be resent: %+v", stops)
		}
		e.mustRun(3, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
		if len(e.stops()) != 1 {
			t.Fatal("an unknown stop placement must block a second placement")
		}
	})
	t.Run("the inventory exit freezes increases, cancels quotes and closes through the close role", func(t *testing.T) {
		e := quoteLongInventoryEnv(t, quoteTestOptions{})
		exit := quoteExitRequest{Kind: quoteExitKindExit, ExitReason: quoteExitReasonAge}
		e.mustRun(3, 100, e.pair("98", "102", "0.2"), exit)
		for _, o := range e.sides(1) {
			if o.mayRest() {
				t.Fatalf("the exit must cancel every quote first: %+v", o)
			}
		}
		e.mustRun(4, 100, e.pair("98", "102", "0.2"), exit)
		e.mustRun(5, 100, quoteDesiredPair{}, quoteExitRequest{})
		c := e.contract()
		ratEq(t, "inventory after exit", c.Book.Qty, "0")
		if n := e.count(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'quote' AND quote_role = 'close' AND submit_state = 'filled'`); n != 1 {
			t.Fatalf("one close order must fill: %d", n)
		}
		for _, st := range e.stops() {
			if st.mayRest() {
				t.Fatalf("a flat book must cancel its stop: %+v", st)
			}
		}
		if s := e.owner(); len(s.Positions) != 0 {
			t.Fatalf("positions %+v", s.Positions)
		}
	})
	t.Run("a partial exit resizes the remainder stop", func(t *testing.T) {
		e := quoteLongInventoryEnv(t, quoteTestOptions{})
		exit := quoteExitRequest{Kind: quoteExitKindExit, ExitReason: quoteExitReasonCap}
		e.venue.placeHook = func(req quoteOrderRequest) (quoteOrderOutcome, bool) {
			if req.Role != quoteRoleClose {
				return quoteOrderOutcome{}, false
			}
			o := e.venue.acceptLocked(req, quoteOutcomeResting)
			e.venue.fillLocked(o, "0.05", req.Px, "0", e.venue.nowMs())
			o.status = quoteOutcomeCancelled
			return e.venue.outcomeOf(o), true
		}
		e.mustRun(3, 100, quoteDesiredPair{}, exit)
		var resting []*quoteOrderRow
		for _, st := range e.stops() {
			if st.SubmitState == quoteSubmitResting {
				resting = append(resting, st)
			}
		}
		if len(resting) != 1 || resting[0].Request.Size != "0.05" {
			t.Fatalf("the remainder stop must cover the unfilled 0.05: %+v", resting)
		}
	})
	t.Run("the kill-switch lane cancels quotes and refuses a confirmed flat while inventory remains", func(t *testing.T) {
		e := quoteLongInventoryEnv(t, quoteTestOptions{})
		rep := killSwitchQuoteLane(e.store, time.Second)
		if rep.Clear {
			t.Fatalf("nonzero quote inventory is never confirmed flat: %+v", rep)
		}
		for _, o := range e.sides(1) {
			if o.mayRest() {
				t.Fatalf("the cancel-before-flatten lane must cancel every quote: %+v", o)
			}
		}
		stops := e.stops()
		if len(stops) != 1 || stops[0].SubmitState != quoteSubmitResting {
			t.Fatalf("the lane keeps the protective stop: %+v", stops)
		}
		plan := planKillSwitchClose(KillSwitchCloseInputs{QuoteLane: func(b time.Duration) killSwitchQuoteReport { return killSwitchQuoteLane(e.store, b) }})
		if plan.OnChainConfirmedFlat {
			t.Fatal("unresolved quote inventory must prevent a confirmed-flat kill-switch result")
		}
		e.adm.mu.Lock()
		e.adm.state.partitionRisk(e.part).KillSwitchActive = true
		e.adm.mu.Unlock()
		e.mustRun(3, 100, quoteDesiredPair{}, quoteExitRequest{})
		e.mustRun(4, 100, quoteDesiredPair{}, quoteExitRequest{})
		e.mustRun(5, 100, quoteDesiredPair{}, quoteExitRequest{})
		ratEq(t, "inventory after kill-switch flattening", e.contract().Book.Qty, "0")
		if rep := killSwitchQuoteLane(e.store, time.Second); !rep.Clear {
			t.Fatalf("a flat, resolved owner clears the lane: %+v", rep)
		}
	})
	t.Run("ordinary protection, reconciliation and force-close never touch a quote owner", func(t *testing.T) {
		e := quoteLongInventoryEnv(t, quoteTestOptions{})
		live := withLiveArgs(e.adm.sc)
		s := e.owner()
		if synced, px := runHyperliquidProtectionSync(live, s, e.db(), "BTC", e.adm.mu, nil, newTestLogger(t), "test", nil, nil, nil, hlProtectionGuardFull, nil); synced || px != 0 {
			t.Fatal("ordinary protection sync must skip a quote owner")
		}
		if n, _, _ := rearmScalarStopAfterFailedClose(live, s, "BTC", 0, 0, nil, nil, hlCloseRemainderStop{}, e.adm.mu, nil, newTestLogger(t)); n != 0 {
			t.Fatal("the scalar rearm owner must skip a quote owner")
		}
		if breach, _, _ := armPaperStopLossAtOpen(e.adm.sc, s, "BTC", 50, newTestLogger(t)); breach {
			t.Fatal("the paper stop owner must skip a quote owner")
		}
		before := e.contract()
		applyHyperliquidCircuitCloseFill(s, "BTC", 0.1, 90, 0.01, 0, 1, "circuit_breaker")
		if got := e.owner().Positions["BTC"]; got == nil || got.Quantity != 0.1 {
			t.Fatalf("a generic circuit close fill must not rewrite quote inventory: %+v", got)
		}
		if err := e.db().UpdateTradeStampedFields(quoteTestOwner, e.clock.Now(), 1, 1, 1, nil, nil, ""); err == nil {
			t.Fatal("trade stamping must refuse a quote owner")
		}
		if err := e.db().ReconcileModelOnlyClose(modelOnlyCloseCorrection{StrategyID: quoteTestOwner, Symbol: "BTC"}); err == nil {
			t.Fatal("model-only reconciliation must refuse a quote owner")
		}
		if got := e.contract(); got.Revision != before.Revision || got.Book != before.Book {
			t.Fatal("a guarded writer changed the quote book")
		}
		if kept := withoutQuoteOwned([]StrategyConfig{live}, e.adm.state); len(kept) != 0 {
			t.Fatal("generic reconciliation rosters must exclude quote owners")
		}
	})
}
