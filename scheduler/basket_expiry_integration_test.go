package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBasketHorizonContractCompatibilityAndRefusals(t *testing.T) {
	plain := loadBasketTestConfig(t, nil)
	c, err := buildBasketContract(plain, plain.Baskets[0])
	if err != nil {
		t.Fatal(err)
	}
	if c.JSON() != basketLegacyContractJSON || c.Fingerprint() != basketLegacyContractFP {
		t.Fatalf("a basket without holding_horizon_seconds changed its contract bytes:\n got %s\nwant %s", c.JSON(), basketLegacyContractJSON)
	}
	zero := loadBasketTestConfig(t, func(root map[string]interface{}) { basketTestBasket(root)["holding_horizon_seconds"] = 0 })
	if zc, _ := buildBasketContract(zero, zero.Baskets[0]); zc.Fingerprint() != basketLegacyContractFP {
		t.Fatalf("holding_horizon_seconds: 0 must keep the legacy fingerprint, got %s", zc.JSON())
	}
	set := loadBasketTestConfig(t, func(root map[string]interface{}) { basketTestBasket(root)["holding_horizon_seconds"] = 3600 })
	sc, _ := buildBasketContract(set, set.Baskets[0])
	if sc.Version != basketContractVersion || !strings.Contains(sc.JSON(), `"holding_horizon_seconds":3600`) || sc.Fingerprint() == basketLegacyContractFP {
		t.Fatalf("a set horizon must be part of the version-1 contract: %s", sc.JSON())
	}
	if diff := basketContractDiff(c, sc); len(diff) != 1 || diff[0] != "holding_horizon_seconds" {
		t.Fatalf("contract diff = %v, want only holding_horizon_seconds", diff)
	}

	for name, raw := range map[string]interface{}{
		"negative": -1, "fraction": 1.5, "boolean": true, "null": nil, "string": "60",
		"above bound": int64(basketMaxHoldingHorizonSeconds) + 1, "exponent": json.Number("1e3"),
	} {
		t.Run(name, func(t *testing.T) {
			path := basketTestConfigJSON(t, t.TempDir(), func(root map[string]interface{}) {
				basketTestBasket(root)["holding_horizon_seconds"] = raw
			})
			_, err := LoadConfigReadOnly(path)
			if err == nil || !strings.Contains(err.Error(), "holding_horizon_seconds") {
				t.Fatalf("holding_horizon_seconds %v must be refused at load, got %v", raw, err)
			}
		})
	}
	maxCfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["holding_horizon_seconds"] = basketMaxHoldingHorizonSeconds
	})
	if maxCfg.Baskets[0].HoldingHorizonSeconds != basketMaxHoldingHorizonSeconds {
		t.Fatalf("the frozen upper bound must load: %d", maxCfg.Baskets[0].HoldingHorizonSeconds)
	}
	set.Baskets[0].HoldingHorizonSeconds = -5
	errs := basketConfigErrors(set)
	set.Baskets[0].HoldingHorizonSeconds = 3600
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "holding_horizon_seconds") {
		t.Fatalf("basket validation must refuse a negative horizon: %v", errs)
	}

	next := loadBasketTestConfig(t, func(root map[string]interface{}) { basketTestBasket(root)["holding_horizon_seconds"] = 7200 })
	next.DBFile, next.LogDir = set.DBFile, set.LogDir
	if err := validateHotReloadCompatible(set, next); err == nil || !strings.Contains(err.Error(), "holding_horizon_seconds") {
		t.Fatalf("reload must refuse a horizon change, got %v", err)
	}
}

func TestBasketHorizonStartupRefusesAChangeWhileOpenAndKeepsTheRankerPayload(t *testing.T) {
	cfg := basketHorizonConfig(t, 0, nil)
	var calls int
	var mu sync.Mutex
	var stdin []string
	env := newBasketTestEnv(t, cfg, basketCountingRanker(&calls, &mu, &stdin, basketTestRanker(basketTestWeights, "")), nil)
	env.openMembers(basketTestDeadline())
	for _, in := range stdin {
		if strings.Contains(in, "holding_horizon") {
			t.Fatalf("the ranker request carries the coordinator lifecycle field: %s", in)
		}
	}
	for _, m := range basketTestMembers {
		if l, _, _ := env.lifecycleRow(m.ID); l.active() {
			t.Fatalf("a basket without a horizon must not track a lifecycle: %+v", l)
		}
	}
	withHorizon := basketHorizonConfig(t, 3600, nil)
	withHorizon.DBFile, withHorizon.LogDir = cfg.DBFile, cfg.LogDir
	env.store.Close()
	env.store = openSourceStore(t, withHorizon)
	loaded, _, err := LoadStateWithStore(withHorizon, env.store)
	if err != nil {
		t.Fatal(err)
	}
	runtimes, err := env.store.LoadBaskets(loaded, withHorizon)
	if err != nil {
		t.Fatal(err)
	}
	bc := newBasketCoordinator(withHorizon, loaded, env.store, env.mu, nil, nil, runtimes)
	if err := bc.startupCheck(); err == nil || !strings.Contains(err.Error(), "holding_horizon_seconds") {
		t.Fatalf("adding a horizon while members are open must be refused at startup, got %v", err)
	}
}

func TestBasketHorizonAnchorsOnTheHistoricalCycleClockAndClosesBetweenDeadlines(t *testing.T) {
	const horizon = int64(6*3600 + 90)
	cfg := basketHorizonConfig(t, horizon, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	wantExpiry := opened.UnixMilli() + horizon*1000
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	frozen := rb.TargetsJSON
	pids := map[string]string{}
	for _, m := range basketTestMembers {
		s := env.state.Strategies[m.ID]
		pos := s.Positions[m.Coin]
		if !pos.OpenedAt.Equal(opened) {
			t.Fatalf("%s opened at %s, want the cycle time %s", m.ID, pos.OpenedAt, opened)
		}
		for _, tr := range s.TradeHistory {
			if !tr.IsClose && !tr.Timestamp.Equal(opened) {
				t.Fatalf("%s opening trade stamped %s, want the cycle time %s", m.ID, tr.Timestamp, opened)
			}
		}
		l, _, ok := env.lifecycleRow(m.ID)
		if !ok || l.PositionID != pos.TradePositionID || l.EntryMs != opened.UnixMilli() || l.ExpiryMs != wantExpiry {
			t.Fatalf("%s lifecycle row %+v, want position %s entered %d expiring %d", m.ID, l, pos.TradePositionID, opened.UnixMilli(), wantExpiry)
		}
		pids[m.ID] = pos.TradePositionID
	}
	expiry := time.UnixMilli(wantExpiry).UTC()

	mid := deadline.Add(3 * time.Hour)
	if d := env.bc.wakeDelay(mid); d != expiry.Sub(mid) {
		t.Fatalf("wake delay between deadlines = %s, want %s to the expiry", d, expiry.Sub(mid))
	}
	env.stepAt(mid, basketTestSnapshot(mid, 60, basketTestMarks(), 1), true, time.Time{})
	env.stepAt(expiry.Add(-time.Second), basketTestSnapshot(mid, 60, basketTestMarks(), 1), true, time.Time{})
	if n := len(env.expirySubmits(venue)); n != 0 {
		t.Fatalf("%d expiry orders before the horizon", n)
	}
	if env.bc.expiryWork(expiry.Add(-time.Second)) || !env.bc.expiryWork(expiry) {
		t.Fatal("the main-loop basket-work check must turn on exactly at the expiry")
	}

	closeAt := expiry.Add(5 * time.Second)
	env.stepAt(closeAt, basketTestSnapshot(mid, 60, basketTestMarks(), 1), true, time.Time{})
	if n := len(env.expirySubmits(venue)); n != 4 {
		t.Fatalf("expiry orders %d, want one per member", n)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s still holds %.10g after the horizon", m.ID, q)
		}
		s := env.state.Strategies[m.ID]
		last := s.TradeHistory[len(s.TradeHistory)-1]
		if !last.IsClose || !last.Timestamp.Equal(closeAt) {
			t.Fatalf("%s close trade %+v, want a close stamped %s", m.ID, last, closeAt)
		}
		l, _, _ := env.lifecycleRow(m.ID)
		if l.active() || l.SuppressThroughMs != deadline.UnixMilli() {
			t.Fatalf("%s lifecycle after the flat commit %+v, want cleared with suppression through %d", m.ID, l, deadline.UnixMilli())
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry' AND status = 'committed' AND deadline_ms = ? AND lifecycle_attempt = 1 AND created_at_ms = ? AND updated_at_ms = ?`,
		deadline.UnixMilli(), closeAt.UnixMilli(), closeAt.UnixMilli()); n != 4 {
		t.Fatalf("committed expiry rows on the cycle clock = %d, want 4", n)
	}
	rows, err := env.store.primary().db.Query(`SELECT evidence_json, lifecycle_position_id, strategy_id FROM basket_member_actions WHERE purpose = 'expiry'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ev, pid, sid string
		if err := rows.Scan(&ev, &pid, &sid); err != nil {
			t.Fatal(err)
		}
		var out basketVenueOutcome
		if err := json.Unmarshal([]byte(ev), &out); err != nil || out.ObservedAtMs != closeAt.UnixMilli() {
			t.Fatalf("expiry evidence %s, want it observed at the cycle time %d", ev, closeAt.UnixMilli())
		}
		if pid != pids[sid] {
			t.Fatalf("expiry action for %s names position %s, want %s", sid, pid, pids[sid])
		}
	}
	rows.Close()
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances WHERE deadline_ms = ? AND status = 'completed' AND targets_json = ?`, deadline.UnixMilli(), frozen); n != 1 {
		t.Fatal("expiry must leave the terminal rebalance status and its frozen targets unchanged")
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 4 {
		t.Fatalf("persisted closes %d, want one per confirmed fill", n)
	}

	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	for _, m := range basketTestMembers {
		if l := env.rt().Lifecycle[m.ID]; l.active() || l.SuppressThroughMs != deadline.UnixMilli() {
			t.Fatalf("%s suppression did not survive the restart: %+v", m.ID, l)
		}
	}
	next := deadline.Add(24 * time.Hour)
	reopen := next.Add(time.Minute)
	env.cycle(reopen, next, basketTestSnapshot(next, 60, basketTestMarks(), 2), true)
	for _, m := range basketTestMembers {
		pos := env.state.Strategies[m.ID].Positions[m.Coin]
		if pos == nil || pos.Quantity <= 0 {
			t.Fatalf("%s did not reopen at the later eligible deadline", m.ID)
		}
		l, _, _ := env.lifecycleRow(m.ID)
		if l.PositionID == pids[m.ID] || l.PositionID != pos.TradePositionID || l.EntryMs != reopen.UnixMilli() || l.ExpiryMs != reopen.UnixMilli()+horizon*1000 {
			t.Fatalf("%s new lifecycle %+v, want a new anchor at %d", m.ID, l, reopen.UnixMilli())
		}
	}
}

func TestBasketOrdinaryPerpsBookingKeepsTheSystemClock(t *testing.T) {
	s := &StrategyState{ID: "plain", Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	before := time.Now().UTC()
	if _, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, basketTestLogger("plain")); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	pos := s.Positions["BTC"]
	if pos == nil || pos.OpenedAt.Before(before) || pos.OpenedAt.After(after) {
		t.Fatalf("ordinary booking must keep the system clock: %+v (window %s..%s)", pos, before, after)
	}
	historical := basketHistoricalDeadline()
	withStrategyBookClock(s, historical, func() {
		bookPerpsClose(s, "BTC", 60100, "signal", "Close", "closed", basketTestLogger("plain"))
	})
	if last := s.TradeHistory[len(s.TradeHistory)-1]; !last.Timestamp.Equal(historical) || !s.bookClock.IsZero() {
		t.Fatalf("the scoped clock must stamp only the wrapped booking and then reset: %+v, clock %s", last, s.bookClock)
	}
	if _, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, basketTestLogger("plain")); err != nil {
		t.Fatal(err)
	}
	if p := s.Positions["BTC"]; p == nil || p.OpenedAt.Before(after) {
		t.Fatalf("a later ordinary booking must use the system clock again: %+v", p)
	}
}

func TestBasketExpiryOverdueAfterRestartReconcilesFirstAndHoldsIncreases(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	venue := newScriptedBasketVenue(false)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueFilled}, {Outcome: basketVenueFilled}, {Outcome: basketVenueFilled}, {Outcome: basketVenueFilled},
		{Outcome: basketVenueUnknown, Detail: "timeout"}}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	expiry := opened.Add(time.Hour)
	env.stepAt(expiry.Add(time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	pending := env.rt().pendingAction()
	if pending == nil || !pending.expiry() || pending.Status != basketActionUnresolved {
		t.Fatalf("want one unresolved expiry attempt, got %+v", pending)
	}
	if n := len(env.expirySubmits(venue)); n != 1 {
		t.Fatalf("an unknown expiry outcome must hold every other order; expiry submits %d", n)
	}
	env.stepAt(expiry.Add(2*time.Minute), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	if n := len(env.expirySubmits(venue)); n != 1 {
		t.Fatalf("an unresolved attempt must be queried, never resent; expiry submits %d", n)
	}

	next := deadline.Add(24 * time.Hour)
	overdue := next.Add(time.Minute)
	venue.queries[pending.ClientOrderID] = []basketVenueOutcome{{Outcome: basketVenueFilled, FilledQty: pending.RequestedQty}}
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	if d := env.bc.wakeDelay(overdue); d != 0 {
		t.Fatalf("an overdue expiry after restart must wake immediately, got %s", d)
	}
	if !env.bc.expiryWork(overdue) {
		t.Fatal("the main-loop basket-work check must see the overdue expiry after restart")
	}
	submitsBefore := len(venue.submits)
	env.cycle(overdue, next, basketTestSnapshot(next, 60, basketTestMarks(), 2), true)
	after := venue.submits[submitsBefore:]
	if len(after) != 3 {
		t.Fatalf("after restart the remaining three members close and nothing else is sent; got %d submissions", len(after))
	}
	for _, a := range after {
		if !a.expiry() || a.ClientOrderID == pending.ClientOrderID {
			t.Fatalf("submission %+v after restart: want only new expiry attempts", a)
		}
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s still holds %.10g after the overdue close", m.ID, q)
		}
		if n := env.closeTrades(m.ID, m.Coin); n != 1 {
			t.Fatalf("%s booked %d closes, want exactly one per confirmed fill", m.ID, n)
		}
	}
	rb := env.rt().Rebalances[next.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceFrozen {
		t.Fatalf("the same-cycle rank must freeze its targets without consuming them, got %+v", rb)
	}
	env.cycle(overdue.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 2), true)
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s reopened from the deadline its expiry covers: %.10g", m.ID, q)
		}
	}
	rb = env.rt().Rebalances[next.UnixMilli()]
	if rb.Status != basketRebalanceCompleted || !strings.Contains(rb.outcomeJSON(), "expiry_suppressed") {
		t.Fatalf("the covered rebalance must complete with every expired member at an effective zero target: %+v", rb)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("persisted opens %d, want the original four only", n)
	}
}

func TestBasketExpiryInFlightRankCannotReopenAndALaterDeadlineCan(t *testing.T) {
	deadline := basketTestDeadline()
	next := deadline.Add(24 * time.Hour)
	cfg := basketHorizonConfig(t, int64((24*time.Hour+4*time.Minute)/time.Second), nil)
	release := make(chan struct{})
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	opened := env.openMembers(deadline)
	expiry := opened.Add(24*time.Hour + 4*time.Minute)
	env.bc.rankRun = basketBlockingRanker(release, basketTestWeights)
	snap := basketTestSnapshot(next, 60, basketTestMarks(), 3)
	env.stepAt(next.Add(time.Minute), snap, false, time.Time{})
	if env.rt().rank == nil || env.rt().rank.DeadlineMs != next.UnixMilli() {
		t.Fatalf("the rank for %s must be in flight", next)
	}
	env.stepAt(expiry.Add(time.Second), snap, false, time.Time{})
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s did not close while the rank was in flight: %.10g", m.ID, q)
		}
		if l, _, _ := env.lifecycleRow(m.ID); l.SuppressThroughMs != next.UnixMilli() {
			t.Fatalf("%s suppression %d must cover the in-flight rank deadline %d", m.ID, l.SuppressThroughMs, next.UnixMilli())
		}
	}
	close(release)
	job := env.rt().rank
	select {
	case <-job.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the released rank did not finish")
	}
	for i := 0; i < 3; i++ {
		env.cycle(expiry.Add(time.Duration(2+i)*time.Minute), next, snap, true)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s reopened from the in-flight rank its expiry covers: %.10g", m.ID, q)
		}
	}
	if rb := env.rt().Rebalances[next.UnixMilli()]; rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the covered rebalance must finish with zero effective targets: %+v", rb)
	}
	later := next.Add(24 * time.Hour)
	env.cycle(later.Add(time.Minute), later, basketTestSnapshot(later, 60, basketTestMarks(), 4), true)
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q == 0 {
			t.Fatalf("%s did not reopen at the later eligible deadline", m.ID)
		}
		if l, _, _ := env.lifecycleRow(m.ID); !l.active() || l.EntryMs != later.Add(time.Minute).UnixMilli() {
			t.Fatalf("%s new lifecycle %+v, want a new anchor", m.ID, l)
		}
	}
}

func TestBasketExpiryTerminalPartialTakesANewAttempt(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenuePartial, FilledQty: -0.5}}
	env.stepAt(opened.Add(time.Hour+time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	subs := env.expirySubmits(venue)
	partial := subs[0]
	var attempts []basketAction
	for _, a := range subs {
		if a.StrategyID == partial.StrategyID {
			attempts = append(attempts, a)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("%s expiry attempts %d, want the partial and its remainder", partial.StrategyID, len(attempts))
	}
	first, second := attempts[0], attempts[1]
	if first.ActionID == second.ActionID || first.ClientOrderID == second.ClientOrderID || first.LifecycleAttempt != 1 || second.LifecycleAttempt != 2 {
		t.Fatalf("a terminal partial must lead to a new attempt with its own identity: %+v / %+v", first, second)
	}
	if !basketQtyEqual(first.RequestedQty-hlFloorLotSize(first.RequestedQty*0.5, basketTestLots[first.Symbol]), second.RequestedQty) {
		t.Fatalf("the remainder attempt requests %.10g, want what the partial left", second.RequestedQty)
	}
	if n := env.closeTrades(partial.StrategyID, partial.Symbol); n != 2 {
		t.Fatalf("%s booked %d closes, want one per confirmed fill", partial.StrategyID, n)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s still holds %.10g", m.ID, q)
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry' AND strategy_id = ? AND status = 'committed'`, partial.StrategyID); n != 2 {
		t.Fatalf("durable committed attempts %d, want 2", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 5 {
		t.Fatalf("persisted closes %d, want five", n)
	}
}

func TestBasketExpiryRejectedRetriesUnknownReconcilesAndDuplicatesBookOnce(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	mock := env.attachNotifier()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueRejected, Detail: "post-only cross"}}
	at := opened.Add(time.Hour + time.Second)
	env.stepAt(at, snap, true, time.Time{})
	subs := env.expirySubmits(venue)
	if len(subs) != 4 {
		t.Fatalf("expiry submits %d, want the rejection and the three other closes", len(subs))
	}
	rid, coin := subs[0].StrategyID, subs[0].Symbol
	if env.signedQty(rid, coin) == 0 {
		t.Fatalf("the rejected member %s must still be open", rid)
	}
	l := env.rt().Lifecycle[rid]
	if l.Failures != 1 || l.RetryAtMs != at.Add(basketExpiryRetryFloor).UnixMilli() || !l.alerted("expiry-failed|"+l.PositionID) {
		t.Fatalf("a rejected expiry must schedule a bounded retry and keep its alert key: %+v", l)
	}
	if stored, _, _ := env.lifecycleRow(rid); stored.Failures != 1 || stored.RetryAtMs != l.RetryAtMs || !stored.alerted("expiry-failed|"+l.PositionID) {
		t.Fatalf("the retry state must be durable: %+v", stored)
	}
	if n := countDMs(mock, "did not fill"); n != 1 {
		t.Fatalf("rejection alerts %d, want 1", n)
	}
	if env.rt().expiryHold == "" {
		t.Fatal("an open expiry obligation must hold increases")
	}
	submits := len(venue.submits)
	env.stepAt(at.Add(30*time.Second), snap, true, time.Time{})
	if len(venue.submits) != submits {
		t.Fatal("a rejected expiry must wait for its retry time")
	}
	if d := env.bc.wakeDelay(at.Add(30 * time.Second)); d != 30*time.Second {
		t.Fatalf("wake delay %s, want the 30s left of the retry delay", d)
	}
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueRejected, Detail: "still crossing"}}
	second := at.Add(61 * time.Second)
	env.stepAt(second, snap, true, time.Time{})
	l = env.rt().Lifecycle[rid]
	if l.Failures != 2 || l.RetryAtMs != second.Add(2*basketExpiryRetryFloor).UnixMilli() || l.Attempt != 2 {
		t.Fatalf("the second rejection must double the bounded retry: %+v", l)
	}
	if n := countDMs(mock, "did not fill"); n != 1 {
		t.Fatalf("repeated rejection alerts %d, want the retained key to deduplicate", n)
	}
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueUnknown, Detail: "timeout"}}
	retryAt := second.Add(2*basketExpiryRetryFloor + time.Second)
	env.stepAt(retryAt, snap, true, time.Time{})
	pending := env.rt().pendingAction()
	if pending == nil || pending.StrategyID != rid || pending.LifecycleAttempt != 3 {
		t.Fatalf("want the third attempt unresolved, got %+v", pending)
	}
	submits = len(venue.submits)
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	env.stepAt(retryAt.Add(time.Minute), snap, true, time.Time{})
	if len(venue.submits) != submits {
		t.Fatal("an unresolved attempt must be queried by its saved client order id, never resent")
	}
	venue.queries[pending.ClientOrderID] = []basketVenueOutcome{{Outcome: basketVenueFilled, FilledQty: pending.RequestedQty, VenueOrderID: "v-9"}}
	env.stepAt(retryAt.Add(3*time.Minute), snap, true, time.Time{})
	if q := env.signedQty(rid, coin); q != 0 {
		t.Fatalf("%s did not close after the unknown attempt reconciled: %.10g", rid, q)
	}
	if len(venue.submits) != submits {
		t.Fatal("reconciling the unknown attempt must not send another order")
	}
	committed := env.rt().Actions[pending.ActionID]
	if committed == nil || committed.Status != basketActionCommitted || committed.VenueOrderID != "v-9" {
		t.Fatalf("the reconciled attempt must commit: %+v", committed)
	}
	trades := len(env.state.Strategies[rid].TradeHistory)
	env.mu.Lock()
	env.bc.applyOutcome(env.rt(), nil, committed, basketVenueOutcome{Outcome: basketVenueFilled, FilledQty: committed.RequestedQty, VenueOrderID: "v-9"}, basketMemberObservation{}, basketCycleInputs{Now: retryAt.Add(4 * time.Minute)}, true)
	env.mu.Unlock()
	if got := len(env.state.Strategies[rid].TradeHistory); got != trades {
		t.Fatalf("a duplicate venue event booked %d more trades", got-trades)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s still holds %.10g", m.ID, q)
		}
		if l, _, _ := env.lifecycleRow(m.ID); l.active() {
			t.Fatalf("%s lifecycle still active after the flat commit: %+v", m.ID, l)
		}
	}
	for _, sub := range venue.submits {
		if sub.Direction == basketDirectionIncrease && sub.CreatedAtMs > opened.UnixMilli() {
			t.Fatalf("an increase was sent while the expiry obligation was open: %+v", sub)
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 4 {
		t.Fatalf("persisted closes %d, want one per member", n)
	}
}

func TestBasketExpirySettlementSaveFailureAndCrashBookOnce(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	prev := storeCommitHook
	fail := true
	storeCommitHook = func(role storageRole) error {
		if fail && env.rt().pendingAction() == nil && len(env.rt().Actions) > 4 {
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	at := opened.Add(time.Hour + time.Second)
	env.stepAt(at, snap, true, time.Time{})
	rt := env.rt()
	if !rt.needsDurable {
		t.Fatal("a failed expiry settlement save must leave the settlement non-durable")
	}
	closedInMemory := ""
	for _, m := range basketTestMembers {
		if env.signedQty(m.ID, m.Coin) == 0 {
			closedInMemory = m.ID
		}
	}
	if closedInMemory == "" {
		t.Fatal("one member should be flat in memory after the first expiry fill")
	}
	if stored, _, _ := env.lifecycleRow(closedInMemory); !stored.active() {
		t.Fatalf("the lifecycle must not clear before its flat book is durable: %+v", stored)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 0 {
		t.Fatalf("a close trade became durable without its book: %d", n)
	}
	env.stepAt(at.Add(time.Minute), snap, true, time.Time{})
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry'`); n != 1 {
		t.Fatalf("no further expiry order may run before the settlement saves; expiry rows %d", n)
	}
	fail = false
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), nil)
	if q := env.signedQty(closedInMemory, basketMemberCoinByID(closedInMemory)); q == 0 {
		t.Fatal("the crash lost the in-memory booking, so the member must be open again on disk")
	}
	env.stepAt(at.Add(2*time.Minute), snap, true, time.Time{})
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s still holds %.10g after recovery", m.ID, q)
		}
		if l, _, _ := env.lifecycleRow(m.ID); l.active() {
			t.Fatalf("%s lifecycle still active: %+v", m.ID, l)
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 4 {
		t.Fatalf("persisted closes %d, want exactly one per member", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry' AND status = 'no_fill'`); n != 1 {
		t.Fatalf("the uncommitted paper attempt must resolve as no fill, got %d", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry' AND strategy_id = ? AND lifecycle_attempt = 2 AND status = 'committed'`, closedInMemory); n != 1 {
		t.Fatalf("the recovered member must close with a second durable attempt, got %d", n)
	}
}

func basketMemberCoinByID(id string) string {
	for _, m := range basketTestMembers {
		if m.ID == id {
			return m.Coin
		}
	}
	return ""
}

func TestBasketExpiryPauseKillSwitchRankerFailureAndMissingMarks(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	mock := env.attachNotifier()
	expiry := opened.Add(time.Hour)
	at := expiry.Add(time.Second)

	env.state.partitionRisk(defaultPaperPartition).KillSwitchActive = true
	env.stepAt(at, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	if n := len(env.expirySubmits(venue)); n != 0 {
		t.Fatalf("a latched kill switch owns flattening; expiry sent %d orders", n)
	}
	for _, m := range basketTestMembers {
		l, _, _ := env.lifecycleRow(m.ID)
		if !l.active() || l.State != basketExpiryDue || l.SuppressThroughMs != deadline.UnixMilli() || l.RetryAtMs != at.Add(basketExpiryRetryFloor).UnixMilli() {
			t.Fatalf("%s must keep its due obligation and suppression while the kill switch holds: %+v", m.ID, l)
		}
	}
	env.mu.Lock()
	sol := env.state.Strategies["hl-bsk-sol"]
	if !bookPerpsClose(sol, "SOL", sol.Positions["SOL"].AvgCost, "kill_switch", "Kill switch close", "kill switch closed", basketTestLogger(sol.ID)) {
		t.Fatal("the kill-switch flatten did not book")
	}
	if err := env.store.SavePartition(env.state, defaultPaperPartition); err != nil {
		t.Fatal(err)
	}
	env.mu.Unlock()
	if l, _, _ := env.lifecycleRow("hl-bsk-sol"); l.active() || l.SuppressThroughMs != deadline.UnixMilli() {
		t.Fatalf("a kill-switch flatten must clear the lifecycle in its book save and keep suppression: %+v", l)
	}
	env.state.partitionRisk(defaultPaperPartition).KillSwitchActive = false

	without := basketMarksWithout("AVAX")
	marksRetry := at.Add(10 * time.Minute)
	env.stepAt(at.Add(2*time.Minute), basketTestSnapshot(deadline, 60, without, 1), true, marksRetry)
	if n := len(env.expirySubmits(venue)); n != 0 {
		t.Fatalf("a missing held-member mark must hold every expiry order; sent %d", n)
	}
	for _, m := range basketTestMembers {
		if m.ID == "hl-bsk-sol" {
			continue
		}
		if l := env.rt().Lifecycle[m.ID]; l.RetryAtMs != marksRetry.UnixMilli() {
			t.Fatalf("%s retry %d, want the next usable feed key %d", m.ID, l.RetryAtMs, marksRetry.UnixMilli())
		}
	}
	if n := countDMs(mock, "holds: no valid sealed mark for AVAX"); n != 1 {
		t.Fatalf("missing-mark alerts %d, want 1", n)
	}
	if d := env.bc.wakeDelay(at.Add(2 * time.Minute)); d != 8*time.Minute {
		t.Fatalf("wake delay %s, want the 8m to the next usable key", d)
	}
	env.stepAt(marksRetry.Add(time.Second), basketTestSnapshot(deadline, 60, without, 1), true, marksRetry.Add(10*time.Minute))
	if n := countDMs(mock, "holds: no valid sealed mark for AVAX"); n != 1 {
		t.Fatalf("repeated missing-mark alerts %d, want the retained key to deduplicate", n)
	}

	for id, sc := range env.bc.members {
		sc.Paused = true
		env.bc.members[id] = sc
	}
	next := deadline.Add(24 * time.Hour)
	env.bc.rankRun = basketTestRanker(nil, "the evaluator holds")
	env.cycle(next.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 2), true)
	if rb := env.rt().Rebalances[next.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld {
		t.Fatalf("the failing ranker must hold its deadline, got %+v", rb)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("pause and a ranker failure must not stop the close: %s holds %.10g", m.ID, q)
		}
		l, _, _ := env.lifecycleRow(m.ID)
		if m.ID == "hl-bsk-sol" {
			if l.active() || l.SuppressThroughMs != deadline.UnixMilli() {
				t.Fatalf("the flattened member keeps its earlier suppression: %+v", l)
			}
			continue
		}
		if l.active() || l.SuppressThroughMs != next.UnixMilli() {
			t.Fatalf("%s lifecycle %+v, want cleared with suppression through the held deadline", m.ID, l)
		}
	}
}

func TestBasketExpiryBlockedByABoundHoldsAlertsOnceAndLaterTranches(t *testing.T) {
	weights := map[string]float64{"BTC": 0.25, "ETH": -0.25, "SOL": 0, "AVAX": 0}
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["holding_horizon_seconds"] = 8 * 3600
		b["params"] = map[string]interface{}{"weights": map[string]interface{}{"BTC": 0.25, "ETH": -0.25, "SOL": 0, "AVAX": 0}}
	})
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(weights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	var firstEntry time.Time
	for h := 0; h < 6; h++ {
		candle := deadline.Add(time.Duration(h) * time.Hour)
		now := candle.Add(time.Minute)
		env.cycle(now, candle, basketTestSnapshot(candle, 60, basketTestMarks(), 1), true)
		if firstEntry.IsZero() && env.signedQty("hl-bsk-btc", "BTC") != 0 {
			firstEntry = now
		}
	}
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the two-leg rebalance did not complete under the $400 net bound: %+v", rb)
	}
	adds := 0
	for _, a := range venue.submits {
		if a.StrategyID == "hl-bsk-btc" && a.Direction == basketDirectionIncrease {
			adds++
		}
	}
	if adds < 2 {
		t.Fatalf("the net bound should split the BTC leg into same-side adds, saw %d", adds)
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		l, _, _ := env.lifecycleRow(id)
		if !l.active() || l.EntryMs != firstEntry.UnixMilli() {
			t.Fatalf("%s lifecycle %+v: same-side adds must keep the first-entry anchor %d", id, l, firstEntry.UnixMilli())
		}
	}
	mock := env.attachNotifier()
	env.bc.lots = useHLLotMetadataForTest(t, map[string]int{"AVAX": 0, "BTC": 0, "ETH": 0, "SOL": 0})
	at := firstEntry.Add(8*time.Hour + time.Second)
	env.stepAt(at, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	if n := len(env.expirySubmits(venue)); n != 0 {
		t.Fatalf("no legal tranche exists, yet %d expiry orders were sent", n)
	}
	if n := countDMs(mock, "blocked by the basket bounds"); n != 1 {
		t.Fatalf("blocked-bound alerts %d, want exactly one", n)
	}
	if !strings.Contains(mock.dms[0].content, "net") {
		t.Fatalf("the blocked alert must name the bound: %s", mock.dms[0].content)
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		if l, _, _ := env.lifecycleRow(id); !l.active() || !l.alerted("expiry-blocked") || l.RetryAtMs != at.Add(basketExpiryRetryFloor).UnixMilli() {
			t.Fatalf("%s must keep the obligation, the alert key and a retry: %+v", id, l)
		}
	}
	env.stepAt(at.Add(61*time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	env.restartFromDisk(basketTestRanker(weights, ""), venue.factory())
	mock = env.attachNotifier()
	env.bc.lots = hlLotMetadata
	env.stepAt(at.Add(3*time.Minute), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	if n := countDMs(mock, "blocked by the basket bounds"); n != 0 {
		t.Fatalf("the retained alert key must suppress a repeat after restart, got %d", n)
	}
	start := basketExposure{Qty: map[string]float64{}, Mark: basketTestMarks()}
	for _, m := range basketTestMembers {
		start.Qty[m.Coin] = env.signedQty(m.ID, m.Coin)
	}
	env.bc.lots = useHLLotMetadataForTest(t, basketTestLots)
	env.stepAt(at.Add(5*time.Minute), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		if q := env.signedQty(id, basketMemberCoinByID(id)); q != 0 {
			t.Fatalf("%s still holds %.10g once legal tranches exist", id, q)
		}
	}
	bounds := basketBoundsFor(env.rt().Contract)
	ids := map[string]bool{}
	tranches := 0
	for _, a := range env.expirySubmits(venue) {
		if ids[a.ActionID] {
			t.Fatalf("expiry action id %s reused", a.ActionID)
		}
		ids[a.ActionID] = true
		tranches++
	}
	if tranches < 3 {
		t.Fatalf("the $400 net bound should split the closes into tranches, saw %d", tranches)
	}
	running := start.clone()
	for _, a := range env.expirySubmits(venue) {
		basketReachableWithin(t, "expiry tranche", running, a, bounds)
		if a.OrderSide == "buy" {
			running.Qty[a.Symbol] += a.RequestedQty
		} else {
			running.Qty[a.Symbol] -= a.RequestedQty
		}
	}
	for coin, q := range running.Qty {
		if math.Abs(q) > 1e-9 {
			t.Fatalf("%s left %.10g after the tranches", coin, q)
		}
	}
}

func TestBasketExpirySettlesUnderAHeldRankAfterTheOldRebalanceIsPruned(t *testing.T) {
	cfg := basketHorizonConfig(t, 25*3600, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	frozen := env.rt().Rebalances[deadline.UnixMilli()].TargetsJSON
	next := deadline.Add(24 * time.Hour)
	env.bc.rankRun = basketTestRanker(nil, "the evaluator holds")
	env.cycle(next.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 2), true)
	rt := env.rt()
	if rb := rt.Rebalances[next.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld {
		t.Fatalf("the later deadline must hold: %+v", rb)
	}
	env.mu.Lock()
	err := env.store.SavePartition(env.state, defaultPaperPartition)
	env.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.Rebalances[deadline.UnixMilli()]; ok {
		t.Fatal("the completed old rebalance should be pruned from memory once a newer deadline is durable")
	}
	at := opened.Add(25*time.Hour + time.Second)
	env.stepAt(at, basketTestSnapshot(next, 60, basketTestMarks(), 2), true, time.Time{})
	if !env.allFlat() {
		t.Fatal("expiry must settle while the latest rank is held and the old rebalance is pruned")
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'expiry' AND status = 'committed' AND deadline_ms = ?`, next.UnixMilli()); n != 4 {
		t.Fatalf("expiry rows on the held deadline %d, want 4", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances WHERE deadline_ms = ? AND status = 'held'`, next.UnixMilli()); n != 1 {
		t.Fatal("expiry settlement must not change the held rebalance")
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances WHERE deadline_ms = ? AND status = 'completed' AND targets_json = ?`, deadline.UnixMilli(), frozen); n != 1 {
		t.Fatal("the pruned rebalance row and its frozen targets must stay unchanged on disk")
	}
	if rb := rt.Rebalances[next.UnixMilli()]; rb.Status != basketRebalanceHeld || rt.Rebalances[deadline.UnixMilli()] != nil {
		t.Fatalf("expiry must not create or change a rank record in memory: %+v", rt.Rebalances)
	}
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	for _, m := range basketTestMembers {
		if l := env.rt().Lifecycle[m.ID]; l.active() || l.SuppressThroughMs != next.UnixMilli() {
			t.Fatalf("%s suppression after pruning and restart %+v, want %d", m.ID, l, next.UnixMilli())
		}
	}
}
