//go:build pyintegration && parkedresearch

package main

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func reversalExpiryMemberConfig() map[string]interface{} {
	return map[string]interface{}{
		"capital": 2000, "leverage": 1, "max_drawdown_pct": 90, "stop_loss_pct": 30,
		"close_strategy": map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 5.0, "close_fraction": 1.0}}}},
	}
}

type reversalExpiryCase struct {
	name    string
	horizon int64
	steps   int
	extra   map[int]basketReplayStep
	skip    []int
	dup     []int
	restart int
	check   func(t *testing.T, out basketReplayOutput, calls []replayCallView)
}

func replayStepAtMs(t *testing.T, st basketReplayStepRecord) int64 {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, st.At)
	if err != nil {
		t.Fatal(err)
	}
	return at.UnixMilli()
}

func replayMember(st basketReplayStepRecord, id string) basketReplayMemberRecord {
	for _, m := range st.Members {
		if m.StrategyID == id {
			return m
		}
	}
	return basketReplayMemberRecord{}
}

func requireExpiryAnchors(t *testing.T, out basketReplayOutput, horizon int64) int {
	t.Helper()
	n := 0
	seen := map[string]bool{}
	for k, st := range out.Steps {
		for _, a := range st.Actions {
			if a.Purpose != basketPurposeExpiry || seen[a.ActionID] {
				continue
			}
			seen[a.ActionID] = true
			n++
			prev := basketReplayMemberRecord{}
			if k > 0 {
				prev = replayMember(out.Steps[k-1], a.StrategyID)
			}
			if a.Attempt < 1 || a.ExpiryMs != prev.LifecycleExpiryMs || a.ExpiryMs != prev.LifecycleEntryMs+horizon*1000 || a.ExpiryMs > replayStepAtMs(t, st) {
				t.Fatalf("expiry action %s attempt %d at %s: expiry %d, previous lifecycle entry %d expiry %d, horizon %ds; the anchor must stay the first confirmed entry plus the horizon", a.ActionID, a.Attempt, st.At, a.ExpiryMs, prev.LifecycleEntryMs, prev.LifecycleExpiryMs, horizon)
			}
		}
	}
	return n
}

func TestBasketReplayReversalExpiryScenariosMatchThePaperCoordinator(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	const hourMs = int64(3_600_000)
	const rebalanceMs = 4 * hourMs
	const firstMs = replayFixtureBaseMs + 264*hourMs
	cases := []reversalExpiryCase{
		{name: "between-rank-deadlines", horizon: 7200, steps: 8, check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
			reopened := false
			for _, st := range out.Steps {
				at := replayStepAtMs(t, st)
				for _, a := range st.Actions {
					if a.Purpose == basketPurposeExpiry && a.Status == basketActionCommitted && at%rebalanceMs == 0 {
						t.Fatalf("expiry %s fired at the rank deadline %s; the case needs closes between deadlines", a.ActionID, st.At)
					}
					if a.Purpose == basketPurposeRebalance && a.Direction == basketDirectionIncrease && a.Status == basketActionCommitted && a.DeadlineMs == firstMs+rebalanceMs {
						if prev := replayMember(out.Steps[3], a.StrategyID); prev.SuppressThroughMs == firstMs {
							reopened = true
						}
					}
				}
			}
			if !reopened {
				t.Fatal("a member whose suppression covered the first deadline must reopen at the next eligible deadline")
			}
		}},
		{name: "overdue-after-restart", horizon: 7200, steps: 8, skip: []int{2}, restart: 2, check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
			if out.Run.ResumedAt != 2 {
				t.Fatalf("the run must resume at step 2, got %d", out.Run.ResumedAt)
			}
			st := out.Steps[2]
			at := replayStepAtMs(t, st)
			overdue := 0
			for _, a := range st.Actions {
				if a.Purpose == basketPurposeExpiry && a.Status == basketActionCommitted && a.ExpiryMs < at {
					overdue++
				}
			}
			if at != firstMs+3*hourMs || overdue == 0 {
				t.Fatalf("the first resumed step %s must close the holding horizon that fell due at 02:00 while the run was stopped, got %+v", st.At, st.Actions)
			}
		}},
		{name: "during-an-executing-rebalance", horizon: 18000, steps: 10, restart: 5,
			extra: map[int]basketReplayStep{4: {Venue: []basketReplayScriptedOutcome{{Outcome: "unknown", Detail: "no status"}}}, 6: {Queries: []basketReplayScriptedOutcome{{Outcome: "filled"}}}},
			check: func(t *testing.T, out basketReplayOutput, calls []replayCallView) {
				if out.Steps[5].PendingAction == "" {
					t.Fatal("the 04:00 rebalance must still be executing with an unresolved order at 05:00")
				}
				due := 0
				for _, m := range out.Steps[5].Members {
					if m.LifecycleState == basketExpiryDue && m.Qty != 0 && m.SuppressThroughMs == firstMs+rebalanceMs {
						due++
					}
				}
				if due == 0 {
					t.Fatalf("the holding horizon must fall due at 05:00 while the rebalance executes, with suppression through its recorded deadline: %+v", out.Steps[5].Members)
				}
				for _, a := range out.Steps[5].Actions {
					if a.Purpose == basketPurposeExpiry {
						t.Fatalf("no expiry close may be sent while the rebalance order is unresolved: %+v", a)
					}
				}
				var suppressed string
				for _, a := range out.Steps[6].Actions {
					if a.Purpose == basketPurposeExpiry && a.Status == basketActionCommitted {
						suppressed = a.StrategyID
					}
				}
				if suppressed == "" {
					t.Fatal("the expiry close must run in the step that resolves the pending order")
				}
				later := false
				for _, st := range out.Steps[6:] {
					for _, a := range st.Actions {
						if a.StrategyID != suppressed || a.Direction != basketDirectionIncrease {
							continue
						}
						if a.DeadlineMs <= firstMs+rebalanceMs {
							t.Fatalf("%s reopened for the suppressed deadline %d: %+v", suppressed, a.DeadlineMs, a)
						}
						later = later || a.Status == basketActionCommitted
					}
				}
				if !later {
					t.Fatalf("%s must reopen at a later eligible deadline", suppressed)
				}
				observed := false
				for _, ev := range out.VenueEvents {
					if ev.Kind == "query" && !ev.Scripted && ev.StepIndex == 5 && ev.Outcome.Outcome == basketVenueUnknown {
						observed = true
					}
				}
				if !observed {
					t.Fatal("the resumed run must query the unresolved order before any new submission")
				}
			}},
		{name: "bound-blocked-tranche", horizon: 7200, steps: 6,
			extra: map[int]basketReplayStep{0: {Venue: []basketReplayScriptedOutcome{{Outcome: "filled"}, {Outcome: "unknown", Detail: "no status"}}}, 1: {Queries: []basketReplayScriptedOutcome{{Outcome: "filled"}}}},
			check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
				var blocked string
				for _, m := range out.Steps[2].Members {
					if m.Qty != 0 && containsString(m.LifecycleAlerts, "expiry-blocked") {
						blocked = m.StrategyID
					}
				}
				if blocked == "" {
					t.Fatalf("the 02:00 expiry close must stop at the net bound with a blocked-tranche alert: %+v", out.Steps[2].Members)
				}
				if m := replayMember(out.Steps[3], blocked); m.Qty != 0 {
					t.Fatalf("%s must close once the other leg falls due, still holds %.10g", blocked, m.Qty)
				}
				for _, a := range out.Steps[2].Actions {
					if a.Bounds == nil || !a.Bounds.Legal {
						t.Fatalf("every expiry tranche must keep a legal endpoint: %+v", a)
					}
				}
			}},
		{name: "partial-expiry-fill", horizon: 7200, steps: 5,
			extra: map[int]basketReplayStep{2: {Venue: []basketReplayScriptedOutcome{{Outcome: "partial", FillFraction: 0.5}}}},
			check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
				var member string
				var expiry int64
				for _, a := range out.Steps[2].Actions {
					if a.Purpose == basketPurposeExpiry && a.Attempt == 1 && a.ConfirmedQty > 0 && a.ConfirmedQty < a.RequestedQty && a.Status == basketActionCommitted {
						member, expiry = a.StrategyID, a.ExpiryMs
					}
				}
				if member == "" {
					t.Fatalf("the first expiry attempt must confirm a partial fill: %+v", out.Steps[2].Actions)
				}
				remainder := false
				for _, a := range out.Steps[2].Actions {
					if a.StrategyID == member && a.Purpose == basketPurposeExpiry && a.Attempt == 2 && a.ExpiryMs == expiry && a.Status == basketActionCommitted {
						remainder = true
					}
				}
				if !remainder || replayMember(out.Steps[2], member).Qty != 0 {
					t.Fatalf("the remainder must close as attempt 2 with the same expiry anchor: %+v", out.Steps[2].Actions)
				}
			}},
		{name: "unknown-expiry-fill", horizon: 3600, steps: 6, dup: []int{2},
			extra: map[int]basketReplayStep{1: {Venue: []basketReplayScriptedOutcome{{Outcome: "unknown", Detail: "no status"}}}, 3: {Queries: []basketReplayScriptedOutcome{{Outcome: "filled"}}}},
			check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
				var a0 basketReplayActionRecord
				for _, a := range out.Steps[1].Actions {
					if a.Purpose == basketPurposeExpiry && a.Status == basketActionUnresolved {
						a0 = a
					}
				}
				if a0.ActionID == "" {
					t.Fatalf("the first expiry close must be unresolved: %+v", out.Steps[1].Actions)
				}
				held := replayMember(out.Steps[0], a0.StrategyID).Qty
				for k := 1; k <= 3; k++ {
					if q := replayMember(out.Steps[k], a0.StrategyID).Qty; q != held {
						t.Fatalf("an unknown fill must book nothing before confirmation: step %d holds %.10g, was %.10g", k, q, held)
					}
				}
				polls, confirmed := 0, false
				for _, ev := range out.VenueEvents {
					if ev.ActionID != a0.ActionID || ev.Kind != "query" {
						continue
					}
					if !ev.Scripted && ev.Outcome.Outcome == basketVenueUnknown {
						polls++
					}
					if ev.Scripted && ev.StepIndex == 4 && ev.Outcome.Outcome == basketVenueFilled {
						confirmed = true
					}
				}
				if polls != 2 || !confirmed {
					t.Fatalf("want two unresolved polls (the step and its duplicate) and a confirming scripted query: %+v", out.VenueEvents)
				}
				final := replayActionsByID(out)[a0.ActionID]
				if final.Status != basketActionCommitted || final.Attempt != 1 || final.ExpiryMs != a0.ExpiryMs || final.DeadlineMs != a0.DeadlineMs {
					t.Fatalf("the confirmed close must keep its attempt, expiry anchor and deadline: first %+v final %+v", a0, final)
				}
				for _, tr := range out.Trades {
					ts, _ := time.Parse(time.RFC3339Nano, tr.Timestamp)
					if tr.StrategyID == a0.StrategyID && tr.IsClose && ts.UnixMilli() < replayStepAtMs(t, out.Steps[4]) {
						t.Fatalf("a close was booked before the unknown expiry fill was confirmed: %+v", tr)
					}
				}
			}},
		{name: "save-failure-during-settlement", horizon: 3600, steps: 5,
			extra: map[int]basketReplayStep{1: {Venue: []basketReplayScriptedOutcome{{Outcome: "unknown", Detail: "no status"}}}, 2: {Queries: []basketReplayScriptedOutcome{{Outcome: "filled"}}, SaveFailure: true}},
			check: func(t *testing.T, out basketReplayOutput, _ []replayCallView) {
				s2, s3 := out.Steps[2], out.Steps[3]
				if len(s2.SaveErrors) == 0 || !s2.NeedsDurable {
					t.Fatalf("the confirmed expiry fill must settle into a failed save that leaves the settlement not durable: errors %v durable-pending %v", s2.SaveErrors, s2.NeedsDurable)
				}
				committed := false
				for _, a := range s2.Actions {
					committed = committed || (a.Purpose == basketPurposeExpiry && a.Status == basketActionCommitted)
				}
				if !committed || len(s3.SaveErrors) != 0 || s3.NeedsDurable {
					t.Fatalf("the next durable save must recover the settlement: step 2 committed %v, step 3 errors %v pending %v", committed, s3.SaveErrors, s3.NeedsDurable)
				}
			}},
	}
	pyintegrationBinary(t)
	t.Chdir(root)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := replayScenario{RunID: "xsrev-" + tc.name, FirstBar: 264, Steps: tc.steps, Horizon: tc.horizon, Ranker: "cross_sectional_reversal", CoinCap: 1500,
				Params:       map[string]interface{}{"return_lookback_bars": 12, "volatility_lookback_bars": 24, "quantile": 0.25},
				MemberConfig: reversalExpiryMemberConfig(), Extra: tc.extra, SkipSteps: tc.skip, DupSteps: tc.dup}
			dir := filepath.Join(realTempDir(t), "run")
			var out basketReplayOutput
			if tc.restart > 0 {
				a := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeNew), "--max-steps", strconv.Itoa(tc.restart))
				if a.Code != 0 || a.Out.Status != basketReplayStatusStopped {
					t.Fatalf("bounded run: exit %d status %s %s", a.Code, a.Out.Status, tail(a.Stderr, 4000))
				}
				b := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeResume))
				if b.Code != 0 {
					t.Fatalf("resume: exit %d %s", b.Code, tail(b.Stderr, 4000))
				}
				out = b.Out
			} else {
				p := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeNew))
				if p.Code != 0 {
					t.Fatalf("exit %d %s", p.Code, tail(p.Stderr, 4000))
				}
				out = p.Out
			}
			if out.Status != basketReplayStatusComplete || len(out.Steps) != len(sc.spec(t, fx, dir, basketReplayModeNew).Steps) {
				t.Fatalf("status %s with %d steps, coverage %+v", out.Status, len(out.Steps), out.Coverage)
			}
			ref := newReplayReference(t, sc.spec(t, fx, filepath.Join(realTempDir(t), "ref"), basketReplayModeNew), out.Cost.Applied)
			for i := range ref.spec.Steps {
				if tc.restart > 0 && i == tc.restart {
					ref.restart()
				}
				ref.step(i)
			}
			requireReplayParity(t, tc.name, ref.records, out.Steps, ref.trades(), out.Trades)
			calls := requireReplayCallParity(t, tc.name, ref, out)
			requireOneBookingPerFill(t, out)
			if requireExpiryAnchors(t, out, tc.horizon) == 0 {
				t.Fatal("the scenario sent no holding-horizon close")
			}
			tc.check(t, out, calls)
		})
	}
}
