package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func pairWakeAt(t *testing.T, e *pairTestEnv, at time.Time) time.Duration {
	t.Helper()
	d := e.bc.pairWakeDelay(at)
	if all := e.bc.wakeDelay(at); all != d {
		t.Fatalf("a pair-only coordinator must wake at its pair delay %s, got %s", d, all)
	}
	return d
}

func TestBasketPairWakeFollowsCadenceAndActionBoundaries(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	t0 := pairTestStart()
	if d := pairWakeAt(t, e, t0); d != 0 {
		t.Fatalf("a pair that has not stepped in the current cadence slot must wake now, got %s", d)
	}
	e.slot(t0, "in-0")
	e.step(t0.Add(time.Second))
	a := e.actionsByPurpose(pairPurposeEntry)
	if len(a) != 1 || a[0].Status != basketActionUnresolved {
		t.Fatalf("want one unresolved first leg, got %+v", a)
	}
	deadline := a[0].AcceptDeadlineMs
	if deadline != t0.Add(31*time.Second).UnixMilli() {
		t.Fatalf("the acceptance deadline must come from the fresh dispatch time, got %d", deadline)
	}
	if d := pairWakeAt(t, e, t0.Add(time.Second)); d != 30*time.Second {
		t.Fatalf("the acceptance deadline is the nearest boundary, want 30s, got %s", d)
	}
	if d := pairWakeAt(t, e, time.UnixMilli(deadline).UTC()); d != 0 {
		t.Fatalf("a boundary that passed after the last step must wake now, got %s", d)
	}
	e.step(time.UnixMilli(deadline).UTC())
	if d := pairWakeAt(t, e, time.UnixMilli(deadline).UTC()); d != t0.Add(time.Minute).Sub(time.UnixMilli(deadline)) {
		t.Fatalf("after the step at the deadline the next cadence slot is the nearest boundary, got %s", d)
	}
	limit := deadline + entryRecoveryGrace.Milliseconds()
	absent := basketVenueOutcome{Outcome: pairVenueAbsent, CoverageComplete: true, CoverageThroughMs: limit}
	pairQueryVenue(e, a[0], absent, absent)
	e.step(time.UnixMilli(limit).UTC())
	if a[0].Status != basketActionUnresolved {
		t.Fatalf("absence at exactly the deadline plus grace must not release, got %s", a[0].Status)
	}
	if d := pairWakeAt(t, e, time.UnixMilli(limit).UTC()); d != time.Millisecond {
		t.Fatalf("the strict absence boundary is one millisecond after the deadline plus grace, got %s", d)
	}
	e.step(time.UnixMilli(limit + 1).UTC())
	if a[0].Status != basketActionNoFill || !a[0].Terminal {
		t.Fatalf("absence one millisecond after the boundary must release, got %s", a[0].Status)
	}
	next := time.UnixMilli(limit + 1).UTC()
	slot := next.UnixMilli() - next.UnixMilli()%60000 + 60000
	if d := pairWakeAt(t, e, next); d != time.UnixMilli(slot).Sub(next) {
		t.Fatalf("a terminal action leaves only the cadence boundary, got %s", d)
	}
}

func TestBasketPairWakeFollowsLifecycleBoundaries(t *testing.T) {
	t.Run("hold age", func(t *testing.T) {
		e := newPairTestEnv(t, pairAll(pairSetLimit("decision_cadence_ms", 600000), pairSetLimit("max_hold_age_ms", 150000)))
		pairEnterHoldRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		if e.lifecycle() != pairLifecycleMatched {
			t.Fatalf("want a matched pair, got %s", e.lifecycle())
		}
		at := pairSlotAt(pairTestStart(), 1).Add(time.Second)
		if d := pairWakeAt(t, e, at); d != 90*time.Second {
			t.Fatalf("the hold age 150s after the first fill is the nearest boundary, want 90s, got %s", d)
		}
		end := time.UnixMilli(e.rt().Pair.Progress.FirstFillMs + 150000).UTC()
		e.step(end)
		if e.lifecycle() == pairLifecycleMatched || e.lifecycle() == pairLifecycleOpening {
			t.Fatalf("the hold age must start the exit at equality, got %s", e.lifecycle())
		}
		if d := pairWakeAt(t, e, end); d != pairTestStart().Add(10*time.Minute).Sub(end) {
			t.Fatalf("after the exit the hold-age boundary must leave the schedule, got %s", d)
		}
	})
	t.Run("protective retry", func(t *testing.T) {
		e := newPairTestEnv(t, pairSetLimit("decision_cadence_ms", 600000))
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.externalBookChange(pairTestHL, 0.01)
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "venue busy"})
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueFilled})
		at := pairSlotAt(pairTestStart(), 1)
		e.step(at)
		r := e.rt().Pair.Progress.Retries[pairTestOKX]
		if e.lifecycle() != pairLifecycleRecovery || r.RetryAtMs != at.UnixMilli()+5000 {
			t.Fatalf("want recovery with a retry at +5s, got %s %+v", e.lifecycle(), r)
		}
		if d := pairWakeAt(t, e, at); d != 5*time.Second {
			t.Fatalf("an eligible protective retry is the nearest boundary, want 5s, got %s", d)
		}
		retry := time.UnixMilli(r.RetryAtMs).UTC()
		e.step(retry)
		if d := pairWakeAt(t, e, retry); d != pairTestStart().Add(10*time.Minute).Sub(retry) {
			t.Fatalf("a filled retry leaves no retry boundary, got %s", d)
		}
	})
	t.Run("recovery age", func(t *testing.T) {
		e := newPairTestEnv(t, pairAll(pairSetLimit("decision_cadence_ms", 600000), pairSetLimit("recovery_age_ms", 150000)))
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.externalBookChange(pairTestHL, 0.01)
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "timeout"})
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "timeout"})
		at := pairSlotAt(pairTestStart(), 1)
		e.step(at)
		if e.lifecycle() != pairLifecycleRecovery || len(e.actionsByPurpose(pairPurposeProtective)) != 2 {
			t.Fatalf("want recovery with two unresolved protective reductions, got %s", e.lifecycle())
		}
		if d := pairWakeAt(t, e, at); d != 30*time.Second {
			t.Fatalf("the protective acceptance deadline is the nearest boundary, got %s", d)
		}
		accept := at.Add(30 * time.Second)
		e.step(accept)
		if d := pairWakeAt(t, e, accept); d != 120*time.Second {
			t.Fatalf("the recovery age 150s after its start is the nearest boundary, want 120s, got %s", d)
		}
		end := at.Add(150 * time.Second)
		e.step(end)
		if e.lifecycle() != pairLifecycleOperatorHeld {
			t.Fatalf("the recovery age must hold the pair for an operator at equality, got %s", e.lifecycle())
		}
		if d := pairWakeAt(t, e, end); d != time.Millisecond {
			t.Fatalf("after the recovery age the strict absence boundary remains, want 1ms, got %s", d)
		}
	})
}

func TestBasketPairWakeNeverSpinsOnHeldBlockedOrTerminalWork(t *testing.T) {
	t.Run("held slots", func(t *testing.T) {
		e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
		e.quotes.mu.Lock()
		e.quotes.err = errors.New("book outage")
		e.quotes.mu.Unlock()
		h := newPairTestHost(e, time.Minute)
		h.runUntil(pairTestStart().Add(5 * time.Second))
		if len(h.runs) < 10 || len(h.runs) > 12 {
			t.Fatalf("held slots must wake once per cadence slot, got %d runs", len(h.runs))
		}
		for _, r := range h.runs {
			if r.Next <= 0 {
				t.Fatalf("a held slot scheduled a zero-delay wake at %s", r.At.Format(time.RFC3339Nano))
			}
		}
		for i := 0; i < 3; i++ {
			at := pairTestStart().Add(5 * time.Second)
			e.step(at)
			if d := e.bc.wakeDelay(at); d <= 0 {
				t.Fatalf("repeated steps at an unchanged clock must settle on a future wake, got %s", d)
			}
		}
	})
	t.Run("failed saves and save-blocked partitions", func(t *testing.T) {
		e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
		e.quotes.mu.Lock()
		e.quotes.err = errors.New("book outage")
		e.quotes.mu.Unlock()
		h := newPairTestHost(e, time.Minute)
		f := &pairCommitFault{from: 1}
		f.install(t)
		h.runUntil(pairTestStart().Add(3 * time.Second))
		blocked := 0
		for _, p := range h.order {
			if p == "save_blocked" {
				blocked++
			}
		}
		if blocked == 0 {
			t.Fatal("three failed saves must send the host through its save-blocked branch")
		}
		for _, r := range h.runs[1:] {
			if r.Next <= 0 {
				t.Fatalf("durability-pending work scheduled a zero-delay wake at %s", r.At.Format(time.RFC3339Nano))
			}
		}
	})
}

func TestBasketPairWakeIsRebuiltFromSavedRecordsAfterRestart(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	t0 := pairTestStart()
	e.slot(t0, "in-0")
	e.step(t0.Add(time.Second))
	at := t0.Add(10 * time.Second)
	e.step(at)
	before := pairWakeAt(t, e, at)
	if before != 21*time.Second {
		t.Fatalf("want the acceptance deadline 21s away, got %s", before)
	}
	e.restart()
	if d := pairWakeAt(t, e, at); d != 0 {
		t.Fatalf("a restarted process must step once before it trusts its schedule, got %s", d)
	}
	e.step(at)
	if d := pairWakeAt(t, e, at); d != before {
		t.Fatalf("the restarted schedule must match the saved action boundaries, got %s, want %s", d, before)
	}
}

func TestBasketPairWakeKeepsOrdinaryBasketTiming(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	env := newBasketTestEnv(t, cfg, nil, nil)
	next := basketTestDeadline().Add(24 * time.Hour)
	for _, tc := range []struct {
		name string
		now  time.Time
		mut  pairMutation
	}{
		{"pair boundary at the basket deadline", next.Add(-1500 * time.Millisecond), nil},
		{"pair boundary before the basket deadline", basketTestDeadline().Add(2*time.Hour + 30*time.Second), nil},
		{"long pair cadence", basketTestDeadline().Add(2 * time.Hour), pairSetLimit("decision_cadence_ms", 2147483647)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := env.bc.wakeDelay(tc.now)
			if base <= 0 {
				t.Fatalf("the basket fixture must have a future wake at %s, got %s", tc.now, base)
			}
			pe := newPairTestEnv(t, tc.mut)
			pe.step(tc.now)
			p := pe.bc.pairWakeDelay(tc.now)
			if p <= 0 {
				t.Fatalf("a stepped pair must have a future wake, got %s", p)
			}
			env.bc.runtimes[pairTestID] = pe.rt()
			got := env.bc.wakeDelay(tc.now)
			delete(env.bc.runtimes, pairTestID)
			want := base
			if p < base {
				want = p
			}
			if got != want {
				t.Fatalf("the mixed wake must be the earlier of the basket wake %s and the pair wake %s, got %s", base, p, got)
			}
			if p >= base && got != base {
				t.Fatalf("a pair wake at or after the basket wake must leave the ordinary timing unchanged, got %s want %s", got, base)
			}
			if after := env.bc.wakeDelay(tc.now); after != base {
				t.Fatalf("the ordinary basket wake changed to %s", after)
			}
		})
	}
}

func TestBasketPairSealedReadUsesTheRequiredCallback(t *testing.T) {
	t.Run("one callback per collection without the coordinator lock", func(t *testing.T) {
		e := pairCollectEnv(t, nil)
		at := pairTestStart().Add(1500 * time.Millisecond)
		var free []bool
		var needOK []bool
		in := e.inputs(at)
		cycle := e.quotes.snapshot(e.rt().Contract.pairMembersSorted(), e.rt().Contract.Pair.BookNeed, at.UnixMilli()-50, "cycle-snapshot")
		in.Snapshot = cycle
		in.SnapshotFor = func(*basketRuntime, time.Time) *marketSnapshot { return cycle }
		in.PairSnapshotFor = func(rt *basketRuntime, readAt time.Time) *marketSnapshot {
			ok := e.mu.TryLock()
			if ok {
				e.mu.Unlock()
			}
			free = append(free, ok)
			s := e.sealFor(rt, readAt)
			key := l2BookKeyFor("BTC")
			needOK = append(needOK, s.books[key] != nil && s.books[key].Need == pairFeedBookNeeds(rt)[key])
			return s
		}
		e.now = at
		e.bc.beginCycle()
		e.observe("")
		e.bc.step(in)
		if len(free) != 1 || !free[0] || !needOK[0] {
			t.Fatalf("the callback must run once, outside the coordinator lock, with the full pair need: free=%v need=%v", free, needOK)
		}
		if len(e.sealCalls) != 1 || e.sealCalls[0].AtMs != at.UnixMilli() {
			t.Fatalf("the callback must be called at the collection read time %d, got %+v", at.UnixMilli(), e.sealCalls)
		}
		status, _, _, _ := e.slotRow(pairTestStart().UnixMilli())
		rb := e.rt().Rebalances[pairTestStart().UnixMilli()]
		if status != basketRebalanceHeld && status != basketRebalanceFrozen {
			t.Fatalf("the sealed read must freeze an envelope, got %s", status)
		}
		if rb.SnapshotID != e.sealCalls[0].ID || rb.SnapshotID == cycle.EvaluationID {
			t.Fatalf("the envelope must bind the callback seal %q, never the cycle snapshot, got %q", e.sealCalls[0].ID, rb.SnapshotID)
		}
	})
	t.Run("a newer host seal during collection keeps the captured seal", func(t *testing.T) {
		e := pairCollectEnv(t, nil)
		at := pairTestStart().Add(time.Second)
		latest := "feed-first"
		var mu sync.Mutex
		e.quotes.onRead = func(pairMemberRead) {
			mu.Lock()
			latest = "feed-newer"
			mu.Unlock()
		}
		in := e.inputs(at)
		calls := 0
		in.PairSnapshotFor = func(rt *basketRuntime, readAt time.Time) *marketSnapshot {
			mu.Lock()
			id := latest
			mu.Unlock()
			calls++
			return e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, readAt.UnixMilli()-50, id)
		}
		e.now = at
		e.bc.beginCycle()
		e.observe("")
		e.bc.step(in)
		rb := e.rt().Rebalances[pairTestStart().UnixMilli()]
		if rb == nil || rb.EvidenceJSON == "" || rb.SnapshotID != "feed-first" || calls != 1 {
			t.Fatalf("a valid captured seal must stay usable after the host seals a newer one: %+v calls=%d", rb, calls)
		}
	})
	t.Run("public and archive members need no callback", func(t *testing.T) {
		for _, mut := range []pairMutation{
			pairSetMember(0, "source", "public_snapshot"),
			pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 250)),
		} {
			e := pairCollectEnv(t, mut)
			e.collect = false
			e.step(pairTestStart().Add(time.Second))
			status, hold, _, evidence := e.slotRow(pairTestStart().UnixMilli())
			if evidence == "" || len(e.sealCalls) != 0 {
				t.Fatalf("a member that needs no seal must collect without the callback, got %s %q", status, hold)
			}
		}
	})
}

func TestBasketPairSealedReadHoldsWithoutAValidSeal(t *testing.T) {
	cases := []struct {
		name string
		edit func(e *pairTestEnv, in *basketCycleInputs)
		want string
	}{
		{"no callback with a cycle snapshot present", func(e *pairTestEnv, in *basketCycleInputs) {
			snap := e.quotes.snapshot(e.rt().Contract.pairMembersSorted(), e.rt().Contract.Pair.BookNeed, in.Now.UnixMilli()-50, "cycle")
			in.PairSnapshotFor, in.Snapshot = nil, snap
			in.SnapshotFor = func(*basketRuntime, time.Time) *marketSnapshot { return snap }
		}, "no sealed market snapshot"},
		{"no generation callback", func(e *pairTestEnv, in *basketCycleInputs) { in.PairGeneration = nil }, "PairGeneration"},
		{"nil seal", func(e *pairTestEnv, in *basketCycleInputs) {
			in.PairSnapshotFor = func(*basketRuntime, time.Time) *marketSnapshot { return nil }
		}, "no sealed market snapshot was returned"},
		{"seal after the read time", func(e *pairTestEnv, in *basketCycleInputs) {
			in.PairSnapshotFor = func(rt *basketRuntime, at time.Time) *marketSnapshot {
				return e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, at.UnixMilli()+1, "future")
			}
		}, "after the read time"},
		{"stale seal", func(e *pairTestEnv, in *basketCycleInputs) {
			in.PairSnapshotFor = func(rt *basketRuntime, at time.Time) *marketSnapshot {
				return e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, at.UnixMilli()-6000, "stale")
			}
		}, "older than quote_age_ms"},
		{"no identity", func(e *pairTestEnv, in *basketCycleInputs) {
			in.PairSnapshotFor = func(rt *basketRuntime, at time.Time) *marketSnapshot {
				return e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, at.UnixMilli()-50, "")
			}
		}, "no printable identity"},
		{"seal generation behind the host", func(e *pairTestEnv, in *basketCycleInputs) {
			in.PairGeneration = func() uint64 { return 2 }
		}, "current feed generation 2"},
		{"generation moves during the read", func(e *pairTestEnv, in *basketCycleInputs) {
			e.quotes.onRead = func(pairMemberRead) {
				e.sealMu.Lock()
				e.generation = 2
				e.sealMu.Unlock()
			}
		}, "moved from 1 to 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := pairCollectEnv(t, nil)
			at := pairTestStart().Add(time.Second)
			in := e.inputs(at)
			tc.edit(e, &in)
			e.now = at
			e.bc.beginCycle()
			e.observe("")
			e.bc.step(in)
			status, hold, input, evidence := e.slotRow(pairTestStart().UnixMilli())
			if status != basketRebalanceHeld || input != "" || evidence != "" || e.runner.calls != 0 {
				t.Fatalf("a missing or invalid seal must hold durably before any policy run, got %s %q (%d policy calls)", status, hold, e.runner.calls)
			}
			if !strings.Contains(hold, tc.want) {
				t.Fatalf("the hold must name the seal failure %q, got %q", tc.want, hold)
			}
		})
	}
}

func TestBasketPairReferenceReadContract(t *testing.T) {
	e := pairCollectEnv(t, nil)
	rt := e.rt()
	okx, _ := rt.Contract.pairMember(pairTestOKX)
	hl, _ := rt.Contract.pairMember(pairTestHL)
	at := pairTestStart()
	e.now = at
	bare := basketCycleInputs{Now: at}
	obs, err := e.bc.readPairReference(context.Background(), rt, bare, okx, "buy")
	if err != nil || len(obs.NativeAsks) != 5 || obs.NormalizationID != "" || obs.SnapshotID != "" {
		t.Fatalf("a public reference needs no callback and returns native levels: %v %+v", err, obs)
	}
	if _, err := e.bc.readPairReference(context.Background(), rt, bare, hl, "sell"); err == nil {
		t.Fatal("a sealed reference without the callback must fail with no substitute observation")
	}
	in := e.inputs(at)
	var free bool
	base := in.PairSnapshotFor
	in.PairSnapshotFor = func(r *basketRuntime, readAt time.Time) *marketSnapshot {
		if free = e.mu.TryLock(); free {
			e.mu.Unlock()
		}
		return base(r, readAt)
	}
	sealed, err := e.bc.readPairReference(context.Background(), rt, in, hl, "sell")
	if err != nil || !free || len(e.sealCalls) != 1 || e.sealCalls[0].AtMs != at.UnixMilli() || sealed.SnapshotID != e.sealCalls[0].ID {
		t.Fatalf("a sealed reference must read its own seal at its read time outside the lock: %v free=%v calls=%+v obs=%q", err, free, e.sealCalls, sealed.SnapshotID)
	}
	e.mu.Lock()
	_, err = e.bc.readPairReferenceLocked(context.Background(), rt, in, okx, "sell")
	e.mu.Unlock()
	if err != nil {
		t.Fatalf("a caller under the coordinator lock must use the locked entry point: %v", err)
	}
	wrong := okx
	wrong.AccountKey = "synthetic:other"
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	gens := 0
	moving := e.inputs(at)
	moving.PairGeneration = func() uint64 {
		gens++
		return uint64(gens)
	}
	moving.PairSnapshotFor = func(r *basketRuntime, readAt time.Time) *marketSnapshot {
		s := e.sealFor(r, readAt)
		s.ConfigGeneration = 1
		return s
	}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		in     basketCycleInputs
		member pairContractMember
		side   string
		setup  func()
	}{
		{"wrong member identity", context.Background(), bare, wrong, "buy", nil},
		{"no order side", context.Background(), bare, okx, "hold", nil},
		{"cancelled caller", cancelled, bare, okx, "buy", nil},
		{"pair records change during the read", context.Background(), bare, okx, "buy", func() {
			e.quotes.onRead = func(pairMemberRead) {
				e.mu.Lock()
				e.rt().touchPairProgress()
				e.mu.Unlock()
			}
		}},
		{"completion at the logical deadline", context.Background(), bare, okx, "buy", func() {
			e.quotes.onRead = func(pairMemberRead) { e.now = e.now.Add(2 * time.Second) }
		}},
		{"generation moves during the read", context.Background(), moving, hl, "sell", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.quotes.mu.Lock()
			e.quotes.onRead = nil
			e.quotes.mu.Unlock()
			e.now = at
			if tc.setup != nil {
				tc.setup()
			}
			got, err := e.bc.readPairReference(tc.ctx, rt, tc.in, tc.member, tc.side)
			if err == nil || got.Payload != nil {
				t.Fatalf("the reference must fail with no substitute observation, got %v %+v", err, got)
			}
		})
	}
}

func TestBasketPairPolicyBudgetBounds(t *testing.T) {
	at := pairTestStart()
	e := newPairTestEnv(t, nil)
	rb := e.slot(at, "in-0")
	rt := e.rt()
	env, why := verifyPairEnvelope(rt, rb)
	if why != "" {
		t.Fatal(why)
	}
	oldest := env.CutoffMs
	for _, m := range env.Members {
		if m.Observation.FirstReceiptMs < oldest {
			oldest = m.Observation.FirstReceiptMs
		}
	}
	if oldest >= env.CutoffMs || oldest != at.UnixMilli()-200 {
		t.Fatalf("the fixture's earliest member receipt must precede the cutoff, got %d (cutoff %d)", oldest, env.CutoffMs)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		want time.Duration
		ok   bool
	}{
		{"earliest member receipt bounds before the cutoff", at.Add(time.Second), 3800 * time.Millisecond, true},
		{"one millisecond before the receipt bound", at.Add(4799 * time.Millisecond), time.Millisecond, true},
		{"quote age equality leaves no launch budget", at.Add(4800 * time.Millisecond), 0, false},
	} {
		d, ok := e.bc.pairPolicyBudget(rt, rb, tc.now)
		if d != tc.want || ok != tc.ok {
			t.Fatalf("%s: budget %s ok=%v, want %s ok=%v", tc.name, d, ok, tc.want, tc.ok)
		}
	}
	short := newPairTestEnv(t, pairSetLimit("decision_cadence_ms", 2000))
	srb := short.slot(at, "in-0")
	if d, ok := short.bc.pairPolicyBudget(short.rt(), srb, at.Add(time.Second)); d != time.Second || !ok {
		t.Fatalf("the next cadence slot must bound the budget, got %s ok=%v", d, ok)
	}
	if d, ok := short.bc.pairPolicyBudget(short.rt(), srb, at.Add(2*time.Second)); ok || d != 0 {
		t.Fatalf("the next cadence slot is strict, got %s ok=%v", d, ok)
	}
	long := newPairTestEnv(t, pairSetLimit("quote_age_ms", 60000))
	lrb := long.slot(at, "in-0")
	if d, ok := long.bc.pairPolicyBudget(long.rt(), lrb, at.Add(time.Second)); d != pairPolicyMaxTimeout || !ok {
		t.Fatalf("pairPolicyMaxTimeout must cap the budget, got %s ok=%v", d, ok)
	}
	over := *rb
	over.DeadlineMs = math.MaxInt64 - 1
	if _, why := pairPolicyWindowFor(rt, &over, env); why == "" {
		t.Fatal("an overflowing next slot must refuse the budget")
	}
	bad := *rb
	bad.EvidenceJSON = strings.Replace(bad.EvidenceJSON, `"v":2}`, `"v":3}`, 1)
	if _, ok := e.bc.pairPolicyBudget(rt, &bad, at.Add(time.Second)); ok {
		t.Fatal("an envelope that fails verification must refuse the budget")
	}
	t.Run("no policy call without budget", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.slot(at, "in-0")
		e.step(at.Add(4800 * time.Millisecond))
		rb := e.rt().Rebalances[at.UnixMilli()]
		if e.runner.calls != 0 || rb.Status != basketRebalanceHeld {
			t.Fatalf("an expired budget must hold without a policy call, got %d calls and %s", e.runner.calls, rb.Status)
		}
		if _, marked := pairIncompleteEvidence(e.rt()); marked {
			t.Fatal("a missing launch budget is not an infrastructure timeout")
		}
	})
	t.Run("strict cadence at the decision", func(t *testing.T) {
		mut := pairAll(pairSetLimit("decision_cadence_ms", 2000), pairSetLimit("quote_age_ms", 60000))
		e := newPairTestEnv(t, mut)
		e.slot(at, "in-0")
		e.step(at.Add(2 * time.Second))
		if e.runner.calls != 0 {
			t.Fatalf("a decision at the next cadence slot must not run the policy, got %d calls", e.runner.calls)
		}
		e2 := newPairTestEnv(t, mut)
		e2.slot(at, "in-0")
		e2.step(at.Add(1900 * time.Millisecond))
		if e2.runner.calls != 1 {
			t.Fatalf("a decision before the next cadence slot must run the policy, got %d calls", e2.runner.calls)
		}
	})
}

func TestBasketPairQuoteAgeEqualityAuthorizesTheHedge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
		sent  bool
	}{{"receipt exactly at quote_age_ms", 4850 * time.Millisecond, true}, {"one millisecond past quote_age_ms", 4851 * time.Millisecond, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterHoldRunner(e, 0.06)
			e.slotStep(0)
			slot1 := pairSlotAt(pairTestStart(), 1)
			rb := e.slot(slot1, "in-1")
			env, why := verifyPairEnvelope(e.rt(), rb)
			if why != "" {
				t.Fatal(why)
			}
			m, _ := env.member(pairTestHL)
			if m.Observation.FirstReceiptMs != slot1.UnixMilli()-150 {
				t.Fatalf("the fixture hedge receipt moved to %d", m.Observation.FirstReceiptMs)
			}
			e.step(slot1.Add(tc.after))
			hedged := len(e.venues.hl.submits) == 1
			if hedged != tc.sent {
				t.Fatalf("the hedge sent=%v, want %v (quote age is inclusive at equality)", hedged, tc.sent)
			}
		})
	}
}

func TestBasketPairPolicyTimeoutMarksTheRunIncomplete(t *testing.T) {
	at := pairTestStart()
	timeoutStep := at.Add(4700 * time.Millisecond)
	t.Run("timeout holds, marks, survives success, lifecycle reset and restart", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.block = make(chan struct{})
		e.slot(at, "in-0")
		start := time.Now()
		e.step(timeoutStep)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("the quote-age budget must end the run near 100ms, took %s", took)
		}
		if e.runner.calls != 1 {
			t.Fatalf("the policy must start inside its quote-age budget and then time out, got %d calls", e.runner.calls)
		}
		e.runner.block = nil
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceHeld {
			t.Fatalf("a policy timeout must hold the slot, got %s", rb.Status)
		}
		o, ok := pairIncompleteEvidence(e.rt())
		if !ok || !strings.Contains(o.Detail, formatClosedBarMs(at.UnixMilli())) || !strings.Contains(o.Detail, "deadline") {
			t.Fatalf("the timeout must leave an infrastructure marker with its slot and cause: %+v", o)
		}
		h := newPairTestHost(e, time.Minute)
		if !h.savedIncomplete() {
			t.Fatal("the marker must be saved with the hold")
		}
		status, _, _, _ := e.slotRow(at.UnixMilli())
		if status != basketRebalanceHeld {
			t.Fatalf("the saved slot must hold, got %s", status)
		}
		pairEnterExitRunner(e, 0.06)
		for i := 1; i <= 4; i++ {
			e.slotStep(i)
		}
		if e.lifecycle() != pairLifecycleFlat && e.lifecycle() != pairLifecycleOpening {
			t.Fatalf("the round trip must return the pair to flat, got %s", e.lifecycle())
		}
		after, ok := pairIncompleteEvidence(e.rt())
		if !ok || after != o || !h.savedIncomplete() {
			t.Fatalf("later success and the lifecycle reset must keep the first marker: %+v", after)
		}
		e.restart()
		again, ok := pairIncompleteEvidence(e.rt())
		if !ok || again != o {
			t.Fatalf("a restart must keep the first marker, got %+v", again)
		}
		if errs := e.bc.pairStartupErrors(e.rt()); len(errs) != 0 {
			t.Fatalf("the marker is not recovery evidence: %v", errs)
		}
	})
	t.Run("explicit economic hold has no marker", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.slot(at, "in-0")
		e.step(timeoutStep)
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceHeld || e.runner.calls != 1 {
			t.Fatalf("the policy hold must freeze a held slot, got %s", rb.Status)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); ok {
			t.Fatal("an explicit policy hold is economic and never marks the run incomplete")
		}
	})
	t.Run("failed save keeps the hold and marker and saves them together", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.block = make(chan struct{})
		e.slot(at, "in-0")
		f := &pairCommitFault{from: 1}
		f.install(t)
		e.step(timeoutStep)
		e.runner.block = nil
		storeCommitHook = nil
		rb := e.rt().Rebalances[at.UnixMilli()]
		if !e.rt().needsDurable || rb.Status != basketRebalanceHeld {
			t.Fatalf("an unsaved timeout hold must stay in memory behind the durable gate, got %s needsDurable=%v", rb.Status, e.rt().needsDurable)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); !ok {
			t.Fatal("the pending marker must report the run incomplete before it is saved")
		}
		h := newPairTestHost(e, time.Minute)
		if status, _, _, _ := e.slotRow(at.UnixMilli()); status != basketRebalanceFrozen || h.savedIncomplete() {
			t.Fatalf("nothing is saved while the commit fails, got %s", status)
		}
		calls := e.runner.calls
		e.step(at.Add(10 * time.Second))
		if status, _, _, _ := e.slotRow(at.UnixMilli()); status != basketRebalanceHeld || !h.savedIncomplete() || e.rt().needsDurable {
			t.Fatalf("the retried save must write the hold and the marker together, got %s", status)
		}
		if e.runner.calls != calls {
			t.Fatalf("a retried save must not decide the slot again, got %d calls", e.runner.calls-calls)
		}
	})
	t.Run("changed state during a timed-out run keeps the marker and discards the decision", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			time.Sleep(300 * time.Millisecond)
			e.mu.Lock()
			e.rt().touchPairProgress()
			e.mu.Unlock()
			return pairEnterResult(r, 0.06), nil
		}
		e.slot(at, "in-0")
		e.step(timeoutStep)
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.TargetsJSON != "" || rb.Status != basketRebalanceFrozen {
			t.Fatalf("a run whose state changed must be discarded, got %s %q", rb.Status, rb.TargetsJSON)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); !ok || !newPairTestHost(e, time.Minute).savedIncomplete() {
			t.Fatal("the timeout evidence must be saved through the current progress owner")
		}
	})
}

func TestBasketPairSimulatedClockPolicyBudget(t *testing.T) {
	at := pairTestStart()
	launch := at.Add(4700 * time.Millisecond)
	slow := func(e *pairTestEnv, during func()) {
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			time.Sleep(300 * time.Millisecond)
			if during != nil {
				during()
			}
			return pairEnterResult(r, 0.06), nil
		}
	}
	t.Run("real mode bounds the wall run by quote age", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		slow(e, nil)
		e.slot(at, "in-0")
		e.step(launch)
		if _, ok := pairIncompleteEvidence(e.rt()); !ok || e.rt().Rebalances[at.UnixMilli()].Status != basketRebalanceHeld {
			t.Fatal("a 300ms run against a 100ms real budget must time out even when the runner ignores its context")
		}
	})
	bind := func(t *testing.T, e *pairTestEnv, now time.Time) *simulatedClock {
		t.Helper()
		sim := &simulatedClock{}
		if err := sim.Set(now); err != nil {
			t.Fatal(err)
		}
		if err := bindTradingClock("pair-cadence-test", sim.Now); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unbindTradingClock("pair-cadence-test") })
		e.bc.pairClock = sim.Now
		return sim
	}
	t.Run("simulated mode uses the fixed wall budget", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		sim := bind(t, e, launch)
		slow(e, nil)
		e.slot(at, "in-0")
		e.step(launch)
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceExecuting || e.runner.calls != 1 {
			t.Fatalf("a simulated run must use the fixed wall budget and dispatch, got %s (%q)", rb.Status, rb.HoldReason)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); ok {
			t.Fatal("a completed simulated run must not mark the run incomplete")
		}
		if !sim.Now().Equal(launch) {
			t.Fatalf("wall time must never advance the simulated clock, got %s", sim.Now())
		}
	})
	t.Run("simulated eligibility is rechecked after the run", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		sim := bind(t, e, launch)
		slow(e, func() { _ = sim.Set(at.Add(4801 * time.Millisecond)) })
		e.slot(at, "in-0")
		e.step(launch)
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceHeld || len(e.rt().Actions) != 0 || !strings.Contains(rb.HoldReason, "after its useful decision window") {
			t.Fatalf("a result past its simulated window must hold without an action, got %s %q", rb.Status, rb.HoldReason)
		}
	})
	t.Run("simulated eligibility is checked before launch", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		late := at.Add(4801 * time.Millisecond)
		bind(t, e, late)
		e.slot(at, "in-0")
		e.step(late)
		if e.runner.calls != 0 || e.rt().Rebalances[at.UnixMilli()].Status != basketRebalanceHeld {
			t.Fatalf("an ineligible simulated slot must hold without a policy call, got %d calls", e.runner.calls)
		}
	})
}

type pairClockVenue struct {
	*pairScriptedVenue
	onValuation func()
}

func (v pairClockVenue) Valuation(m pairContractMember, nowMs int64) (pairMemberValuation, error) {
	if v.onValuation != nil {
		v.onValuation()
	}
	return v.pairScriptedVenue.Valuation(m, nowMs)
}

func TestBasketPairClockAdvanceCannotAuthorizeStaleActions(t *testing.T) {
	at := pairTestStart()
	t.Run("valuation", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		once := false
		base := e.venues.factory()
		e.bc.venueFor = func(sc StrategyConfig) (basketVenue, error) {
			v, err := base(sc)
			if err != nil {
				return nil, err
			}
			return pairClockVenue{pairScriptedVenue: v.(*pairScriptedVenue), onValuation: func() {
				if !once {
					once = true
					e.now = e.now.Add(4 * time.Second)
				}
			}}, nil
		}
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		if e.runner.calls != 0 || len(e.rt().Actions) != 0 {
			t.Fatalf("time spent in the valuations must count against the policy budget, got %d calls and %d actions", e.runner.calls, len(e.rt().Actions))
		}
	})
	t.Run("policy", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			e.now = at.Add(4801 * time.Millisecond)
			return pairEnterResult(r, 0.06), nil
		}
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		if len(e.rt().Actions) != 0 || e.rt().Rebalances[at.UnixMilli()].Status != basketRebalanceHeld {
			t.Fatal("a result whose evidence expired during the run must not dispatch")
		}
	})
	t.Run("query", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800})
		e.slotStep(0)
		first := e.actionsByPurpose(pairPurposeEntry)[0]
		pairQueryVenue(e, first, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true})
		slot1 := pairSlotAt(pairTestStart(), 1)
		e.slot(slot1, "in-1")
		advanced := false
		e.venues.okx.mu.Lock()
		e.venues.okx.onQuery = func(*basketAction) {
			if !advanced {
				advanced = true
				e.now = slot1.Add(5900 * time.Millisecond)
			}
		}
		e.venues.okx.mu.Unlock()
		e.step(slot1.Add(time.Second))
		e.assertBooks("filled leg, no stale hedge", 0, -0.06)
		if len(e.venues.hl.submits) != 0 {
			t.Fatal("a hedge whose quote expired while the query ran must not be sent")
		}
	})
	t.Run("post-reservation expiry sends nothing and releases", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		e.slot(at, "in-0")
		var reserved string
		prev := storeCommitHook
		storeCommitHook = func(storageRole) error {
			for _, a := range e.rt().Actions {
				if a.Status == basketActionReserved && reserved == "" {
					reserved = a.RequestJSON
					e.now = e.now.Add(5 * time.Second)
				}
			}
			return nil
		}
		t.Cleanup(func() { storeCommitHook = prev })
		e.step(at.Add(time.Second))
		storeCommitHook = prev
		acts := e.actionsByPurpose(pairPurposeEntry)
		if reserved == "" || len(acts) != 1 || len(e.venues.okx.submits) != 0 {
			t.Fatalf("want one reserved first leg and no transport, got %d actions and %d sends", len(acts), len(e.venues.okx.submits))
		}
		a := acts[0]
		var status, req, intent, use string
		if err := e.db().db.QueryRow(`SELECT status, request_json FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&status, &req); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM entry_order_intents WHERE intent_id = ?`, a.EntryIntentID).Scan(&intent); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM pair_quote_event_uses WHERE action_id = ?`, a.ActionID).Scan(&use); err != nil {
			t.Fatal(err)
		}
		if status != basketActionNoFill || req != reserved || intent != entryIntentNoFill || use != pairUseReleased {
			t.Fatalf("a post-reservation expiry must end through the saved no-fill path with the request unchanged: %s %s %s (request changed=%v)", status, intent, use, req != reserved)
		}
		if n := e.count(`SELECT COUNT(DISTINCT client_order_id) FROM basket_member_actions`); n != 1 {
			t.Fatalf("no second send identity may be created, got %d", n)
		}
	})
	t.Run("transport boundary reads fresh time", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		e.slot(at, "in-0")
		armed := true
		e.bc.pairClock = func() time.Time {
			if armed && e.mu.TryLock() {
				e.mu.Unlock()
				return e.now.Add(6 * time.Second)
			}
			return e.now
		}
		e.step(at.Add(time.Second))
		armed = false
		acts := e.actionsByPurpose(pairPurposeEntry)
		if len(acts) != 1 || acts[0].Status != basketActionNoFill || len(e.venues.okx.submits) != 0 || !strings.Contains(acts[0].HoldReason, "transport boundary") {
			t.Fatalf("an expiry seen at the unlocked transport boundary must send nothing: %+v sends=%d", acts, len(e.venues.okx.submits))
		}
	})
	t.Run("submit", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		e.slot(at, "in-0")
		e.venues.okx.mu.Lock()
		e.venues.okx.onSubmit = func(*basketAction) { e.now = at.Add(8 * time.Second) }
		e.venues.okx.mu.Unlock()
		e.step(at.Add(time.Second))
		e.assertBooks("a confirmed fill is booked after the evidence aged", 0, -0.06)
		var ts time.Time
		for _, tr := range e.state.Strategies[pairTestOKX].TradeHistory {
			if tr.Symbol == "BTC" {
				ts = tr.Timestamp
			}
		}
		if !ts.Equal(at.Add(8 * time.Second)) {
			t.Fatalf("settlement after a submit must use fresh time, booked at %s", ts.Format(time.RFC3339Nano))
		}
	})
}

func TestBasketPairExitLegsKeepTheStepStartValuationClock(t *testing.T) {
	t.Run("a slow first exit leg still sends the second leg", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("matched", 0.06, -0.06)
		slot2 := pairSlotAt(pairTestStart(), 2)
		stepAt := slot2.Add(time.Second)
		advanced := false
		slow := func(a *basketAction) {
			if a.Purpose == pairPurposeExit && !advanced {
				advanced = true
				e.now = stepAt.Add(5001 * time.Millisecond)
			}
		}
		for _, v := range []*pairScriptedVenue{e.venues.hl, e.venues.okx} {
			v.mu.Lock()
			v.onSubmit = slow
			v.mu.Unlock()
		}
		e.slot(slot2, "in-2")
		e.step(stepAt)
		if !advanced {
			t.Fatal("the first exit leg must reach the venue")
		}
		if exits := e.actionsByPurpose(pairPurposeExit); len(exits) != 2 {
			t.Fatalf("both exit legs must be sent in the same step after the clock passed the valuation age, got %d", len(exits))
		}
		if e.rt().Pair.Progress.obligation("bounds_unavailable", "", "") != nil {
			t.Fatal("a reduction must not save a bounds_unavailable obligation from a clock refresh inside its step")
		}
		e.assertBooks("exited", 0, 0)
	})
	t.Run("an exit with a valuation unavailable at step start holds", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("matched", 0.06, -0.06)
		e.venues.okx.mu.Lock()
		e.venues.okx.valErr = errors.New("quote outage")
		e.venues.okx.mu.Unlock()
		slot2 := e.slotStep(2)
		if exits := e.actionsByPurpose(pairPurposeExit); len(exits) != 0 {
			t.Fatalf("an exit without a valuation at step start must hold, got %d exit actions", len(exits))
		}
		if rb := e.rt().Rebalances[slot2.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "valuation is unavailable") {
			t.Fatalf("the exit decision must end held on the unavailable valuation, got %+v", rb)
		}
		e.assertBooks("still matched", 0.06, -0.06)
	})
}

func pairHostRunner(e *pairTestEnv, holds int) {
	n := 0
	e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep the spread"), nil
		}
		n++
		if n <= holds {
			return pairHoldResult(r, "no edge yet"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
}

func TestBasketPairHostOrderDrivesWakeOnlySteps(t *testing.T) {
	t0 := pairTestStart()
	e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
	pairHostRunner(e, 2)
	h := newPairTestHost(e, time.Minute)
	h.fund = func(time.Time) {}
	h.prepare = func(basketCycleInputs) {}
	if h.interval <= time.Duration(e.rt().Pair.Limits.QuoteAgeMs)*time.Millisecond {
		t.Fatal("the host cycle gap must exceed quote_age_ms for this proof")
	}
	h.runUntil(t0.Add(3 * time.Second))
	if len(h.runs) < 6 || !h.runs[0].Managed {
		t.Fatalf("the host must manage once and then wake on the pair cadence, got %+v", h.runs)
	}
	for i, r := range h.runs[1:] {
		if r.Managed {
			t.Fatalf("run %d managed members inside one host interval", i+1)
		}
		if gap := r.At.Sub(h.runs[i].At); gap != 500*time.Millisecond {
			t.Fatalf("run %d came %s after the previous run, want the 500ms pair cadence", i+1, gap)
		}
	}
	first := strings.Join(h.order[:7], ",")
	if first != "evidence,funding,prepare_risk,risk,manage,step,save" {
		t.Fatalf("the management run must follow the documented host order, got %s", first)
	}
	if wake := strings.Join(h.order[7:13], ","); wake != "evidence,funding,prepare_risk,risk,step,save" {
		t.Fatalf("a wake-only run skips only member management, got %s", wake)
	}
	legs := e.actionsByPurpose(pairPurposeEntry)
	if len(legs) != 2 {
		t.Fatalf("want a first leg and a hedge, got %d entry actions", len(legs))
	}
	for _, a := range legs {
		dispatchedIn := -1
		for i, r := range h.runs {
			if r.At.UnixMilli() == a.CreatedAtMs {
				dispatchedIn = i
			}
		}
		if dispatchedIn <= 0 || h.runs[dispatchedIn].Managed {
			t.Fatalf("action %s must be dispatched in a wake-only run, got run %d", a.ActionID, dispatchedIn)
		}
		var evidence string
		if err := e.db().db.QueryRow(`SELECT r.evidence_json FROM pair_quote_event_uses u JOIN basket_rebalances r ON r.basket_id = u.pair_id AND r.deadline_ms = u.slot_ms WHERE u.action_id = ?`, a.ActionID).Scan(&evidence); err != nil {
			t.Fatal(err)
		}
		env, err := pairDecodeQuoteEnvelope([]byte(evidence), 0)
		if err != nil || env.StartMs != a.CreatedAtMs {
			t.Fatalf("action %s must use the quote event collected in its own step (envelope start %d, created %d, %v)", a.ActionID, env.StartMs, a.CreatedAtMs, err)
		}
	}
	e.assertBooks("matched from wake-only steps", 0.06, -0.06)
	for i, r := range h.runs {
		if i >= len(e.sealCalls) || e.sealCalls[i].AtMs != r.At.UnixMilli() {
			t.Fatalf("each run must read its own seal at its collection time, got %+v", e.sealCalls)
		}
	}
	if len(h.saveErrs) != 0 {
		t.Fatalf("the host saves failed: %v", h.saveErrs)
	}
}

func TestBasketPairHostCycleAdvancesOnlyOnManagementRuns(t *testing.T) {
	t0 := pairTestStart()
	e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
	e.quotes.mu.Lock()
	e.quotes.err = errors.New("book outage")
	e.quotes.mu.Unlock()
	failing := true
	prev := storeCommitHook
	storeCommitHook = func(storageRole) error {
		if failing {
			return errors.New("injected commit fault")
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	for i := 0; i < 3; i++ {
		e.mu.Lock()
		e.store.SaveAll(e.state)
		e.mu.Unlock()
	}
	if !allPartitionsSaveBlocked(e.store, e.cfg) {
		t.Fatal("three failed saves must block every partition before the host runs")
	}
	h := newPairTestHost(e, time.Minute)
	var seen []int
	h.prepare = func(basketCycleInputs) { seen = append(seen, e.bc.cycle) }
	start := e.bc.cycle
	for i := 0; i < 3; i++ {
		h.run(t0.Add(time.Duration(i) * 500 * time.Millisecond))
	}
	if e.bc.cycle != start {
		t.Fatalf("save-blocked runs with management due must not advance the cycle, got %d from %d", e.bc.cycle, start)
	}
	blocked := strings.Join(h.order, ",")
	want := strings.Repeat("evidence,prepare_risk,save_blocked,step,save,", 3)
	if blocked+"," != want {
		t.Fatalf("save-blocked runs must skip risk and management, got %s", blocked)
	}
	failing = false
	e.mu.Lock()
	e.store.SaveAll(e.state)
	e.mu.Unlock()
	h.run(t0.Add(1500 * time.Millisecond))
	if e.bc.cycle != start+1 {
		t.Fatalf("one unblocked management run must advance the cycle by exactly one, got %d from %d", e.bc.cycle, start)
	}
	if got := strings.Join(h.order[15:], ","); got != "evidence,prepare_risk,risk,manage,step,save" {
		t.Fatalf("the unblocked run must manage after risk, got %s", got)
	}
	for i, r := range h.runs {
		if r.Managed != (i == 3) {
			t.Fatalf("run %d managed=%v; only the unblocked run manages", i, r.Managed)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("the prepare hook must run on every host cycle, got %d calls", len(seen))
	}
	for i, c := range seen {
		if c != start {
			t.Fatalf("prepare call %d saw cycle %d; risk preparation must see the cycle from before the management increment %d", i, c, start)
		}
	}
}

func TestBasketPairHostManagementFreshnessGatesIncreases(t *testing.T) {
	t0 := pairTestStart()
	t.Run("no management before the step holds every increase", func(t *testing.T) {
		e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
		pairHostRunner(e, 0)
		h := newPairTestHost(e, time.Minute)
		h.manage = func(time.Time, map[RiskPartition]*scopeCycleRisk) {}
		h.runUntil(t0.Add(time.Second))
		if n := len(e.rt().Actions); n != 0 {
			t.Fatalf("a pair step without member management must not dispatch, got %d actions", n)
		}
	})
	t.Run("management older than two host intervals holds increases", func(t *testing.T) {
		e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 30000))
		pairHostRunner(e, 1000)
		h := newPairTestHost(e, 10*time.Minute)
		h.runUntil(t0.Add(120 * time.Second))
		pairHostRunner(e, 0)
		h.runUntil(t0.Add(150 * time.Second))
		if n := len(e.rt().Actions); n != 0 {
			t.Fatalf("an increase on management output older than 2 x interval_seconds must hold, got %d actions", n)
		}
		e.mu.Lock()
		why := e.bc.pairIncreaseHold(e.rt(), basketCycleInputs{Now: t0.Add(150 * time.Second)})
		e.mu.Unlock()
		if why == "" || !strings.Contains(why, "maximum management age") {
			t.Fatalf("the hold must name the management age, got %q", why)
		}
		fresh := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 30000))
		pairHostRunner(fresh, 1000)
		fh := newPairTestHost(fresh, time.Minute)
		fh.runUntil(t0.Add(120 * time.Second))
		pairHostRunner(fresh, 0)
		fh.runUntil(t0.Add(150 * time.Second))
		last := fh.runs[len(fh.runs)-1]
		if n := len(fresh.actionsByPurpose(pairPurposeEntry)); n == 0 || last.Managed || !last.At.Equal(t0.Add(150*time.Second)) {
			t.Fatalf("a host that manages every interval must dispatch from the wake-only step at +150s, got %d actions (last run %+v)", n, last)
		}
	})
}

func TestBasketPairHostIncompleteRunSurvivesRestart(t *testing.T) {
	t0 := pairTestStart()
	e := pairCollectEnv(t, pairSetLimit("decision_cadence_ms", 500))
	calls := 0
	e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		calls++
		if calls == 2 {
			time.Sleep(time.Second)
		}
		if calls <= 2 || r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	h := newPairTestHost(e, time.Minute)
	h.runUntil(t0)
	if _, ok := h.incomplete(); ok {
		t.Fatal("the first slot completed in time")
	}
	h.runUntil(t0.Add(500 * time.Millisecond))
	o, ok := h.incomplete()
	if !ok || !h.savedIncomplete() || h.runs[len(h.runs)-1].Managed {
		t.Fatalf("a timeout in a wake-only step must mark and save the incomplete run: %+v", o)
	}
	h.runUntil(t0.Add(2 * time.Second))
	if after, ok := h.incomplete(); !ok || after != o {
		t.Fatalf("later successful slots must keep the first marker, got %+v", after)
	}
	if n := len(e.actionsByPurpose(pairPurposeEntry)); n == 0 {
		t.Fatal("slots after the timeout must still decide and dispatch")
	}
	e.restart()
	if after, ok := h.incomplete(); !ok || after != o || !h.savedIncomplete() {
		t.Fatalf("a restart must keep the incomplete run marker, got %+v", after)
	}
}
