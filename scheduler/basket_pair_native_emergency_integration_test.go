package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"strings"
	"testing"
	"time"
)

func pairNativeEvidence(oid, side string, atMs int64, px, fee, feeCcy string, sizes ...string) *nativeCloseEvidence {
	ev := &nativeCloseEvidence{V: nativeCloseEvidenceVersion, VenueOrderIDs: []string{oid}, Complete: true}
	total := new(big.Rat)
	for i, sz := range sizes {
		n, err := pairRatDecimal(new(big.Rat).Mul(mustRat(sz), mustRat(px)))
		if err != nil {
			panic(err)
		}
		f := pairScriptFill{FillID: fmt.Sprintf("%s-f%d", oid, i+1), VenueTimeMs: atMs + int64(i), NativeQty: sz, NativePx: px, NativeNotional: n, OrderID: oid, Side: side}
		if fee != "" {
			f.FeeKnown, f.FeeAmount, f.FeeCurrency = true, fee, feeCcy
		}
		ev.Fills = append(ev.Fills, f)
		total.Add(total, mustRat(sz))
	}
	ev.ExecutedNativeQty, _ = pairRatDecimal(total)
	return ev
}

func pairNativeEvidenceTotal(ev *nativeCloseEvidence) float64 {
	return pairRatFloat(mustRat(ev.ExecutedNativeQty))
}

func pairHLNativeResult(ev *nativeCloseEvidence, px float64) *HyperliquidCloseResult {
	fill := &HyperliquidCloseFill{AvgPx: px}
	if ev != nil {
		fill.TotalSz = pairNativeEvidenceTotal(ev)
	}
	return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "BTC", Fill: fill, Evidence: ev}}
}

func pairOKXNativeResult(ev *nativeCloseEvidence, px float64) *OKXCloseResult {
	fill := &OKXCloseFill{AvgPx: px}
	if ev != nil {
		fill.TotalSz = pairNativeEvidenceTotal(ev)
	}
	return &OKXCloseResult{Close: &OKXClose{Symbol: "BTC", Fill: fill, Evidence: ev}}
}

func (e *pairTestEnv) nativeAt() int64 {
	return e.now.UnixMilli()
}

func (e *pairTestEnv) obligationKinds(member string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []string
	for _, o := range e.rt().Pair.Progress.Obligations {
		if member == "" || o.MemberID == member {
			out = append(out, o.Kind)
		}
	}
	return out
}

func (e *pairTestEnv) savedProgressHas(fragment string) bool {
	return strings.Contains(e.savedProgress(), fragment)
}

func (e *pairTestEnv) closeTrade(id string) (Trade, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.state.Strategies[id].TradeHistory
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].IsClose && h[i].Symbol == "BTC" {
			return h[i], true
		}
	}
	return Trade{}, false
}

func (e *pairTestEnv) savedCloseRows(id string) int {
	n := 0
	for _, r := range e.savedTrades(id) {
		if r.IsClose {
			n++
		}
	}
	return n
}

func pairTestStepUntil(e *pairTestEnv, d time.Duration) {
	e.step(e.now.Add(d))
}

func (v *pairScriptedVenue) queryCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, c := range v.queryCnt {
		n += c
	}
	return n
}

func newPairPaperEmergencyEnv(t *testing.T) *pairTestEnv {
	t.Helper()
	e := newPairTestEnv(t, nil)
	clock := "b1914-paper-" + t.Name()
	if err := bindTradingClock(clock, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock(clock) })
	pairViewMatched(t, e)
	return e
}

func (e *pairTestEnv) quoteOutage(on bool) {
	e.quotes.mu.Lock()
	defer e.quotes.mu.Unlock()
	if on {
		e.quotes.refErr = errors.New("quote outage")
	} else {
		e.quotes.refErr = nil
	}
}

func (e *pairTestEnv) tripLossStreak(id string) string {
	e.t.Helper()
	e.prepare(e.now)
	e.mu.Lock()
	defer e.mu.Unlock()
	sc := e.bc.members[id]
	s := e.state.Strategies[id]
	s.RiskState.ConsecutiveLosses = sc.CircuitBreakerLossStreakThreshold() + 1
	_, why := checkStrategyRiskWithView(&sc, s, 0, nil, e.bc.pairMarksLocked(), nil, nil)
	return why
}

func (e *pairTestEnv) pairPending(id string) *PendingCircuitClose {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state.Strategies[id].RiskState.getPendingCircuitClose(PlatformPendingClosePair)
}

func (e *pairTestEnv) ownMark(id string) (float64, bool, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rt().pairView[id].usdMarkAt(e.now.UnixMilli())
}

func TestBasketPairPaperEmergencyBreakerRetriesWhileActiveAndAfterRestart(t *testing.T) {
	e := newPairPaperEmergencyEnv(t)
	e.quoteOutage(true)
	why := e.tripLossStreak(pairTestOKX)
	if !strings.HasPrefix(why, RiskReasonConsecutiveLosses) || e.signed(pairTestOKX) != -0.06 || e.pairPending(pairTestOKX) == nil {
		t.Fatalf("the paper breaker trip must request the pair close and keep the book: %q %v %+v", why, e.signed(pairTestOKX), e.pairPending(pairTestOKX))
	}
	for i := 0; i < 3; i++ {
		e.step(e.now.Add(time.Minute))
		e.mu.Lock()
		sc := e.bc.members[pairTestOKX]
		_, active := checkStrategyRiskWithView(&sc, e.state.Strategies[pairTestOKX], 0, nil, e.bc.pairMarksLocked(), nil, nil)
		e.mu.Unlock()
		if active != RiskReasonCircuitBreakerActive {
			t.Fatalf("cycle %d: the breaker must stay active, got %q", i, active)
		}
	}
	if e.signed(pairTestOKX) != -0.06 || !e.hasObligation(pairObligationEmergencyClose, pairTestOKX) {
		t.Fatalf("an active breaker without an own mark keeps the book and the obligation: %v %v", e.signed(pairTestOKX), e.obligationKinds(pairTestOKX))
	}
	sc := e.bc.members[pairTestOKX]
	e.now = e.now.Add(sc.CircuitBreakerLossStreakCooldown() + time.Minute)
	e.mu.Lock()
	e.state.Strategies[pairTestOKX].RiskState.ConsecutiveLosses = 0
	checkStrategyRiskWithView(&sc, e.state.Strategies[pairTestOKX], 0, nil, e.bc.pairMarksLocked(), nil, nil)
	e.mu.Unlock()
	if e.pairPending(pairTestOKX) == nil || e.signed(pairTestOKX) != -0.06 {
		t.Fatalf("cooldown expiry must keep the pending close and the book: pending %+v book %v", e.pairPending(pairTestOKX), e.signed(pairTestOKX))
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if e.pairPending(pairTestOKX) == nil || !e.hasObligation(pairObligationEmergencyClose, pairTestOKX) {
		t.Fatalf("the pending close and its obligation must survive a restart: %v", e.obligationKinds(pairTestOKX))
	}
	e.step(e.now.Add(time.Second))
	if e.signed(pairTestOKX) != -0.06 {
		t.Fatal("after a restart the close waits for a checked mark")
	}
	e.quoteOutage(false)
	e.step(e.now.Add(time.Second))
	own, valid, why2 := e.ownMark(pairTestOKX)
	tr, ok := e.closeTrade(pairTestOKX)
	if e.signed(pairTestOKX) != 0 || !valid || !ok || !pairCostNear(tr.Price, own) || e.pairPending(pairTestOKX) != nil || e.hasObligation(pairObligationEmergencyClose, pairTestOKX) {
		t.Fatalf("a checked mark after restart must book the close once at that own mark %v (%s): book %v trade %+v", own, why2, e.signed(pairTestOKX), tr)
	}
	if e.savedCloseRows(pairTestOKX) != 1 {
		t.Fatalf("the paper breaker close must be saved once, got %d", e.savedCloseRows(pairTestOKX))
	}
	if e.lifecycle() != pairLifecycleRecovery {
		t.Fatalf("the one-member close must open recovery for the surviving leg, got %s", e.lifecycle())
	}
}

func TestBasketPairPaperEmergencyFailedSaveKeepsTheStagedPrice(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			e := newPairPaperEmergencyEnv(t)
			e.tripLossStreak(pairTestHL)
			e.now = e.now.Add(time.Second)
			own, valid, _ := e.ownMark(pairTestHL)
			prev := storeCommitHook
			commits := 0
			storeCommitHook = func(role storageRole) error {
				commits++
				if commits >= 2 {
					return fmt.Errorf("disk full")
				}
				return nil
			}
			e.step(e.now)
			storeCommitHook = prev
			staged := e.rt().Pair.Progress.PaperCloses
			if !valid || len(staged) != 1 || !pairCostNear(staged[0].PriceUSD, own) {
				t.Fatalf("the close must stage at the own mark %v: %+v", own, staged)
			}
			if !e.savedProgressHas(pairPaperCloseStaged) || e.savedCloseRows(pairTestHL) != 0 {
				t.Fatalf("the staged selection must be saved before the book, and the failed book save must leave no close row")
			}
			if restart {
				e.restart()
				if p := e.rt().Pair.Progress.stagedPaperClose(pairTestHL); p == nil || e.signed(pairTestHL) != 0.06 {
					t.Fatalf("a restart must reload the staged close and the open book: %+v %v", p, e.signed(pairTestHL))
				}
			}
			pairViewSetHL(e, 52000)
			e.step(e.now.Add(time.Second))
			tr, ok := e.closeTrade(pairTestHL)
			if !ok || !pairCostNear(tr.Price, staged[0].PriceUSD) || e.signed(pairTestHL) != 0 || e.savedCloseRows(pairTestHL) != 1 {
				t.Fatalf("the retried save must book the same staged price %v once, got %+v rows %d", staged[0].PriceUSD, tr, e.savedCloseRows(pairTestHL))
			}
			if c := e.rt().Pair.Progress.PaperCloses; len(c) != 1 || c[0].Status != pairPaperCloseCommitted {
				t.Fatalf("the staged close must commit once: %+v", c)
			}
		})
	}
}

func pairFailBookSavesOnly() func() {
	prev := storeCommitHook
	storeCommitHook = func(storageRole) error {
		pc := make([]uintptr, 64)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for {
			fr, more := frames.Next()
			if strings.HasSuffix(fr.Function, ".saveStateSubset") {
				return fmt.Errorf("disk full")
			}
			if !more {
				return nil
			}
		}
	}
	return func() { storeCommitHook = prev }
}

func (e *pairTestEnv) savedPaperCloses(id string) []pairPaperClose {
	e.t.Helper()
	var p pairProgress
	if err := json.Unmarshal([]byte(e.savedProgress()), &p); err != nil {
		e.t.Fatalf("saved pair progress is unreadable: %v", err)
	}
	var out []pairPaperClose
	for _, c := range p.PaperCloses {
		if c.MemberID == id {
			out = append(out, c)
		}
	}
	return out
}

func TestBasketPairPaperEmergencyBookSaveFailureKeepsTheCloseStaged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failSteps int
		restart   bool
	}{
		{name: "a progress save after the failed book save, then a restart", failSteps: 1, restart: true},
		{name: "the next step saves the book with no restart", failSteps: 1, restart: false},
		{name: "three failed book saves, then a restart at another mark", failSteps: 3, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairPaperEmergencyEnv(t)
			e.tripLossStreak(pairTestHL)
			e.now = e.now.Add(time.Second)
			own, valid, why := e.ownMark(pairTestHL)
			if !valid {
				t.Fatalf("setup: the member needs an own mark: %s", why)
			}
			restore := pairFailBookSavesOnly()
			for i := 0; i < tc.failSteps; i++ {
				at := e.now
				if i > 0 {
					pairViewSetHL(e, 52000+float64(i)*100)
					at = e.now.Add(time.Second)
				}
				e.step(at)
				e.mu.Lock()
				ok := e.bc.persistPair(e.rt())
				e.mu.Unlock()
				if !ok {
					restore()
					t.Fatalf("step %d: a progress save must succeed while book saves fail", i)
				}
				saved := e.savedPaperCloses(pairTestHL)
				if len(saved) != 1 || saved[0].Status != pairPaperCloseStaged || e.savedCloseRows(pairTestHL) != 0 {
					restore()
					t.Fatalf("step %d: a progress save after a failed book save must keep the close staged with no saved close row: %+v rows %d", i, saved, e.savedCloseRows(pairTestHL))
				}
			}
			restore()
			staged := e.savedPaperCloses(pairTestHL)[0]
			if !pairCostNear(staged.PriceUSD, own) {
				t.Fatalf("the close must stage at the own mark %v, got %+v", own, staged)
			}
			if tc.restart {
				e.restart()
				if p := e.rt().Pair.Progress.stagedPaperClose(pairTestHL); p == nil || p.CloseAtMs != staged.CloseAtMs || e.signed(pairTestHL) != 0.06 {
					t.Fatalf("a restart must reload the staged close and the open book: %+v %v", p, e.signed(pairTestHL))
				}
				pairViewSetHL(e, 53000)
			}
			e.step(e.now.Add(time.Second))
			tr, ok := e.closeTrade(pairTestHL)
			if !ok || !pairCostNear(tr.Price, staged.PriceUSD) || tr.Timestamp.UnixMilli() != staged.CloseAtMs || e.signed(pairTestHL) != 0 || e.savedCloseRows(pairTestHL) != 1 {
				t.Fatalf("the close must book once at the staged price %v and time %d, got %+v rows %d", staged.PriceUSD, staged.CloseAtMs, tr, e.savedCloseRows(pairTestHL))
			}
			saved := e.savedPaperCloses(pairTestHL)
			if len(saved) != 1 || saved[0].Status != pairPaperCloseCommitted || e.pairPending(pairTestHL) != nil || e.hasObligation(pairObligationEmergencyClose, pairTestHL) {
				t.Fatalf("the close must commit once after the book saves and clear the pending close: %+v pending %+v", saved, e.pairPending(pairTestHL))
			}
			e.restart()
			if e.signed(pairTestHL) != 0 || e.pairPending(pairTestHL) != nil || e.savedCloseRows(pairTestHL) != 1 || e.rt().Pair.Progress.stagedPaperClose(pairTestHL) != nil {
				t.Fatalf("the committed close must reload flat with no pending close: book %v pending %+v", e.signed(pairTestHL), e.pairPending(pairTestHL))
			}
		})
	}
}

func TestBasketPairPaperEmergencyTriggerNamesTheRequestingMechanism(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		fire func(e *pairTestEnv)
	}{
		{name: "paper portfolio kill switch", want: pairNativeTriggerKillSwitch, fire: func(e *pairTestEnv) {
			e.prepare(e.now)
			e.mu.Lock()
			defer e.mu.Unlock()
			applyPaperKillSwitchCycle(e.state, e.cfg, map[string]float64{"BTC": 50000}, &scopeCycleRisk{Partition: e.rt().Partition, KillSwitchFired: true, Reason: "test drawdown"}, true)
		}},
		{name: "per-strategy loss streak", want: pairNativeTriggerCircuit, fire: func(e *pairTestEnv) {
			e.tripLossStreak(pairTestHL)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairPaperEmergencyEnv(t)
			tc.fire(e)
			if p := e.pairPending(pairTestHL); p == nil || p.Episode != tc.want {
				t.Fatalf("the pending pair close must name %s: %+v", tc.want, p)
			}
			e.step(e.now.Add(time.Second))
			var rec *pairPaperClose
			for i, c := range e.rt().Pair.Progress.PaperCloses {
				if c.MemberID == pairTestHL {
					rec = &e.rt().Pair.Progress.PaperCloses[i]
				}
			}
			tr, ok := e.closeTrade(pairTestHL)
			var reasons []string
			rows, err := e.db().db.Query(`SELECT close_reason FROM closed_positions WHERE close_reason LIKE 'pair_emergency_%' ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var r string
				if err := rows.Scan(&r); err != nil {
					t.Fatal(err)
				}
				reasons = append(reasons, r)
			}
			rows.Close()
			if rec == nil || rec.Trigger != tc.want || !ok || !strings.Contains(tr.Details, "("+tc.want+")") || len(reasons) == 0 {
				t.Fatalf("the staged close and the trade must name %s: %+v %+v %v", tc.want, rec, tr, reasons)
			}
			for _, r := range reasons {
				if r != "pair_emergency_"+tc.want {
					t.Fatalf("every saved pair emergency close reason must be pair_emergency_%s, got %v", tc.want, reasons)
				}
			}
		})
	}
}

func (f *pairKillSwitchFixture) member(id string) []*basketAction {
	var out []*basketAction
	for _, purpose := range []string{pairPurposeKillSwitch, pairPurposeOperator} {
		for _, a := range f.env.actionsByPurpose(purpose) {
			if a.StrategyID == id {
				out = append(out, a)
			}
		}
	}
	return out
}

func TestBasketPairNativeEmergencyCircuitCloseOpensRecoveryAndAttachesBesideAPendingReduction(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	e.mu.Lock()
	e.state.Strategies[pairTestOKX].RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 6}}})
	e.mu.Unlock()
	runPendingOKXCircuitCloses(t.Context(), e.state, e.cfg.Strategies, true, []OKXPosition{{Coin: "BTC", Size: -6, Side: "short"}}, true, nil, f.closers.okx, 5*time.Second, e.mu, nil)
	acts := f.member(pairTestOKX)
	if f.okxCalls != 1 || len(acts) != 1 || acts[0].Purpose != pairPurposeOperator || !strings.Contains(acts[0].RecoveryJSON, `"trigger":"circuit_breaker"`) || acts[0].Status != basketActionSettling {
		t.Fatalf("the okx circuit close must route through the pair-aware closer as a circuit-breaker native action: calls %d %+v", f.okxCalls, acts)
	}
	if e.lifecycle() != pairLifecycleRecovery || !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("a confirmed one-member native close must open recovery before booking, got %s", e.lifecycle())
	}
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "held open for the attachment check"})
	e.step(e.now.Add(time.Second))
	if e.signed(pairTestOKX) != 0 || acts[0].Status != basketActionCommitted {
		t.Fatalf("the okx circuit fill must book once: %v %+v", e.signed(pairTestOKX), acts[0])
	}
	if e.lifecycle() != pairLifecycleRecovery {
		t.Fatalf("booking the native close must keep recovery open, got %s", e.lifecycle())
	}
	var prot *basketAction
	for _, a := range e.actionsByPurpose(pairPurposeProtective) {
		if a.StrategyID == pairTestHL && basketActionPending(a.Status) {
			prot = a
		}
	}
	if prot == nil {
		t.Fatalf("recovery must reduce the surviving hyperliquid leg with a protective reduction: %+v", e.actionsByPurpose(pairPurposeProtective))
	}
	if _, err := e.bc.pairKillSwitchHLCloser(f.closers.hl)("BTC", nil, nil); err != nil {
		t.Fatal(err)
	}
	if f.hlCalls != 1 || len(f.member(pairTestHL)) != 0 || len(e.rt().Pair.Progress.NativeReceipts) != 1 {
		t.Fatalf("the kill switch must still send while the protective reduction is pending and keep the fill as a receipt: calls %d actions %+v receipts %+v", f.hlCalls, f.member(pairTestHL), e.rt().Pair.Progress.NativeReceipts)
	}
	e.step(e.now.Add(time.Second))
	hl := f.member(pairTestHL)
	if len(hl) != 1 || hl[0].Status != basketActionCommitted || e.signed(pairTestHL) != 0 || !basketActionPending(prot.Status) {
		t.Fatalf("the receipt must attach and book while the protective reduction is still pending: %+v book %v protective %s", hl, e.signed(pairTestHL), prot.Status)
	}
	if f.hlCalls != 1 {
		t.Fatalf("attaching the receipt must send nothing, calls %d", f.hlCalls)
	}
}

func TestBasketPairNativeEmergencyHyperliquidCircuitCloseBooksOnlyThroughThePair(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	e.mu.Lock()
	e.state.Strategies[pairTestHL].RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.06}}})
	e.mu.Unlock()
	runPendingHyperliquidCircuitCloses(t.Context(), e.state, e.cfg.Strategies, "0xabc", []HLPosition{{Coin: "BTC", Size: 0.06}}, true, nil, f.closers.hl, 5*time.Second, e.mu, nil)
	before := e.bookSnapshot(pairTestHL)
	if f.hlCalls != 1 || before.qty != 0.06 {
		t.Fatalf("the hyperliquid circuit close must send once and leave the book to the pair coordinator: calls %d %+v", f.hlCalls, before)
	}
	e.step(e.now.Add(time.Second))
	after := e.bookSnapshot(pairTestHL)
	if after.qty != 0 || after.trades != before.trades+1 || e.savedCloseRows(pairTestHL) != 1 {
		t.Fatalf("the coordinator must book the circuit close once: %+v -> %+v rows %d", before, after, e.savedCloseRows(pairTestHL))
	}
}

func TestBasketPairNativeEmergencyLiveClosersNeverTouchAResearchPair(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairViewMatched(t, e)
	calls := 0
	inner := func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		calls++
		return pairHLNativeResult(pairNativeEvidence("990", "sell", e.nativeAt(), "60000", "1", "USDC", "0.06"), 60000), nil
	}
	if _, err := e.bc.pairKillSwitchHLCloser(inner)("BTC", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pairCircuitHLCloserFor(e.state, pairTestHL, inner)("BTC", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(e.actionsByPurpose(pairPurposeKillSwitch))+len(e.actionsByPurpose(pairPurposeOperator)) != 0 || len(e.rt().Pair.Progress.NativeReceipts) != 0 || e.signed(pairTestHL) != 0.06 {
		t.Fatalf("a live closer fill must never be attributed to a paper research pair: calls %d", calls)
	}
}

func TestBasketPairNativeEmergencyPeerHolderMakesTheFillAmbiguous(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	peer := e.bc.members[pairTestHL]
	peer.ID, peer.PairID = "peer-hl-btc", ""
	e.mu.Lock()
	e.cfg.Strategies = append(e.cfg.Strategies, peer)
	ps := NewStrategyState(peer)
	ps.Positions["BTC"] = &Position{Symbol: "BTC", Side: entrySideLong, Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
	e.state.Strategies[peer.ID] = ps
	e.mu.Unlock()
	if _, err := e.bc.pairKillSwitchHLCloser(f.closers.hl)("BTC", nil, nil); err != nil {
		t.Fatal(err)
	}
	e.step(e.now.Add(time.Second))
	if f.hlCalls != 1 || len(f.member(pairTestHL)) != 0 || e.signed(pairTestHL) != 0.06 || !e.hasObligation(pairObligationNativeAmbiguous, pairTestHL) {
		t.Fatalf("a whole-coin fill shared with a peer must book nothing for the member and keep an ambiguity obligation: %v %v", e.signed(pairTestHL), e.obligationKinds(pairTestHL))
	}
}

func TestBasketPairNativeEmergencyFeesAndPartialEvidence(t *testing.T) {
	cases := []struct {
		name       string
		ev         func(e *pairTestEnv) *nativeCloseEvidence
		err        error
		wantBooked float64
		wantFee    float64
		wantStatus string
		wantOblig  string
		wantLeft   float64
	}{
		{name: "explicit zero fee", ev: func(e *pairTestEnv) *nativeCloseEvidence {
			return pairNativeEvidence("911", "sell", e.nativeAt(), "59900", "0", "USDC", "0.06")
		}, wantBooked: 0.06, wantFee: 0, wantStatus: basketActionCommitted, wantLeft: 0},
		{name: "rebate", ev: func(e *pairTestEnv) *nativeCloseEvidence {
			return pairNativeEvidence("912", "sell", e.nativeAt(), "59900", "-0.5", "USDC", "0.03", "0.03")
		}, wantBooked: 0.06, wantFee: -1, wantStatus: basketActionCommitted, wantLeft: 0},
		{name: "fill returned with a transport error", ev: func(e *pairTestEnv) *nativeCloseEvidence {
			return pairNativeEvidence("913", "sell", e.nativeAt(), "59900", "0.9", "USDC", "0.06")
		}, err: errors.New("close subprocess exit 1 with no error field"), wantBooked: 0.06, wantFee: 0.9, wantStatus: basketActionCommitted, wantLeft: 0},
		{name: "confirmed part of a larger execution", ev: func(e *pairTestEnv) *nativeCloseEvidence {
			ev := pairNativeEvidence("914", "sell", e.nativeAt(), "59900", "0.4", "USDC", "0.03")
			ev.ExecutedNativeQty, ev.Complete, ev.Detail = "0.06", false, "one fill is not visible yet"
			return ev
		}, wantBooked: 0.03, wantFee: 0.4, wantStatus: basketActionHeld, wantOblig: pairObligationNativeUnconfirmed, wantLeft: 0.03},
		{name: "fill before any conversion evidence", ev: func(e *pairTestEnv) *nativeCloseEvidence {
			return pairNativeEvidence("915", "sell", 1_600_000_000_000, "59900", "0.9", "USDC", "0.06")
		}, wantBooked: 0, wantStatus: basketActionSettling, wantOblig: pairObligationSettlementConversion, wantLeft: 0.06},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPairKillSwitchFixture(t)
			e := f.env
			ev := tc.ev(e)
			f.hlOut = func() (*HyperliquidCloseResult, error) { return pairHLNativeResult(ev, 59900), tc.err }
			if _, err := e.bc.pairKillSwitchHLCloser(f.closers.hl)("BTC", nil, nil); (err != nil) != (tc.err != nil) {
				t.Fatalf("the wrapper must return the native error unchanged: %v", err)
			}
			e.step(e.now.Add(time.Second))
			e.step(e.now.Add(time.Second))
			acts := f.member(pairTestHL)
			if len(acts) != 1 || acts[0].Status != tc.wantStatus || !basketQtyEqual(acts[0].AppliedQty, tc.wantBooked) {
				t.Fatalf("want one %s native action with %v booked: %+v", tc.wantStatus, tc.wantBooked, acts)
			}
			if got := e.signed(pairTestHL); !basketQtyEqual(got, tc.wantLeft) {
				t.Fatalf("the member book must keep %v, holds %v", tc.wantLeft, got)
			}
			if tc.wantBooked > 0 {
				tr, _ := e.closeTrade(pairTestHL)
				if !pairCostNear(tr.ExchangeFee, tc.wantFee) || !basketQtyEqual(tr.Quantity, tc.wantBooked) {
					t.Fatalf("the close must carry the confirmed signed fee %v once: %+v", tc.wantFee, tr)
				}
			}
			if tc.wantOblig != "" && !e.hasObligation(tc.wantOblig, pairTestHL) {
				t.Fatalf("want obligation %s, got %v", tc.wantOblig, e.obligationKinds(pairTestHL))
			}
			if tc.wantOblig != "" {
				if got, ok := pairIncompleteEvidence(e.rt()); !ok || got.MemberID != pairTestHL {
					t.Fatalf("the incomplete-result reader must report the member, got %+v %v", got, ok)
				}
			}
			if f.hlCalls != 1 {
				t.Fatalf("accounting retries must send nothing, calls %d", f.hlCalls)
			}
		})
	}
}

func TestBasketPairNativeEmergencyRepeatedReceiptsApplyOnce(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	ev := pairNativeEvidence("921", "sell", e.nativeAt(), "59900", "0.9", "USDC", "0.02", "0.04")
	f.hlOut = func() (*HyperliquidCloseResult, error) { return pairHLNativeResult(ev, 59900), nil }
	release, ok := tryLockEntryAttempt(pairTestHL)
	if !ok {
		t.Fatal("setup: the entry lock is busy")
	}
	closer := e.bc.pairKillSwitchHLCloser(f.closers.hl)
	closer("BTC", nil, nil)
	closer("BTC", nil, nil)
	release()
	if n := len(e.rt().Pair.Progress.NativeReceipts); n != 1 {
		t.Fatalf("a repeated receipt for the same fills must be kept once, got %d", n)
	}
	e.step(e.now.Add(time.Second))
	if e.signed(pairTestHL) != 0 || len(f.member(pairTestHL)) != 1 {
		t.Fatalf("the receipt must book once: %v %+v", e.signed(pairTestHL), f.member(pairTestHL))
	}
	e.externalBookChange(pairTestHL, 0.06)
	closer("BTC", nil, nil)
	e.step(e.now.Add(time.Second))
	if e.signed(pairTestHL) != 0.06 || !e.hasObligation(pairObligationNativeConflict, pairTestHL) {
		t.Fatalf("the same fills reported on a later action must hold as a conflict and book nothing: %v %v", e.signed(pairTestHL), e.obligationKinds(pairTestHL))
	}
	if n := e.savedCloseRows(pairTestHL); n != 1 {
		t.Fatalf("multiple fills of one order must apply once, got %d close rows", n)
	}
}

func TestBasketPairNativeEmergencyLegacyCommittedRowsStayHeld(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	f.fire()
	e.step(e.now.Add(time.Second))
	if e.signed(pairTestHL) != 0 {
		t.Fatal("setup: the native close must book")
	}
	rows := e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'pair_kill_switch' AND status = 'committed'`)
	if _, err := e.db().db.Exec(`UPDATE basket_member_actions SET settlement_json = '' WHERE purpose = 'pair_kill_switch'`); err != nil {
		t.Fatal(err)
	}
	trades := e.savedCloseRows(pairTestHL)
	e.restart()
	if rows != 2 || !e.hasObligation(pairObligationNativeLegacy, pairTestHL) || !e.hasObligation(pairObligationNativeLegacy, pairTestOKX) {
		t.Fatalf("committed native rows without a ledger must load as unverified accounting: rows %d %v", rows, e.obligationKinds(""))
	}
	e.step(e.now.Add(time.Second))
	if e.savedCloseRows(pairTestHL) != trades {
		t.Fatal("a legacy native row must never be booked again")
	}
	if got, ok := pairIncompleteEvidence(e.rt()); !ok || got.Kind != pairObligationNativeLegacy {
		t.Fatalf("legacy native rows keep the result incomplete, got %+v %v", got, ok)
	}
}

func TestBasketPairNativeEmergencyOlderFileGainsTheHeldStatus(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	sdb := e.db()
	var ddl string
	if err := sdb.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'basket_member_actions'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(ddl, "status IN ('reserved','submitting','unresolved','settling','committed','no_fill','held')", "status IN ('reserved','submitting','unresolved','settling','committed','no_fill')", 1)
	old = strings.Replace(strings.Replace(old, `CREATE TABLE "basket_member_actions"`, "CREATE TABLE basket_member_actions_v2", 1), "CREATE TABLE basket_member_actions ", "CREATE TABLE basket_member_actions_v2 ", 1)
	before := e.count(`SELECT COUNT(*) FROM basket_member_actions`)
	if err := sdb.rebuildTableTx("basket_member_actions", old, `INSERT INTO basket_member_actions_v2 SELECT * FROM basket_member_actions ORDER BY rowid`, basketActionIndexDDL); err != nil {
		t.Fatal(err)
	}
	if err := sdb.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'basket_member_actions'`).Scan(&ddl); err != nil || strings.Contains(ddl, "'held'") {
		t.Fatalf("setup: the older table must lack the held status: %v", err)
	}
	e.restart()
	if err := e.db().db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'basket_member_actions'`).Scan(&ddl); err != nil || !strings.Contains(ddl, "'held'") {
		t.Fatalf("opening the older file must add the held status: %v", err)
	}
	if got := e.count(`SELECT COUNT(*) FROM basket_member_actions`); got != before {
		t.Fatalf("the rebuild must keep every action row: %d -> %d", before, got)
	}
	f.hlOut = func() (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("transport crash") }
	f.fire()
	f.hlOut = func() (*HyperliquidCloseResult, error) {
		return pairHLNativeResult(pairNativeEvidence("931", "sell", e.nativeAt(), "59900", "0.9", "USDC", "0.06"), 59900), nil
	}
	f.fire()
	if n := e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE status = 'held'`); n != 1 {
		t.Fatalf("the migrated file must save a held native action, got %d", n)
	}
}

func TestBasketPairNativeEmergencyLiveResetWaitsForCommittedAccounting(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	f.fire()
	e.mu.Lock()
	for _, id := range []string{pairTestHL, pairTestOKX} {
		sc := e.bc.members[id]
		forceCloseKillSwitchPositions(e.state.Strategies[id], sc, map[string]float64{"BTC": 61000}, nil, nil, nil, nil, nil)
	}
	pending := pairEmergencyPendingFor(e.state, livePartition)
	e.mu.Unlock()
	e.assertBooks("the live venue-flat cleanup books nothing at the shared price", 0.06, -0.06)
	if len(pending) != 2 {
		t.Fatalf("both members must hold the live reset while their accounting is pending: %v", pending)
	}
	note := pairKillSwitchAccountingNote("all venue positions are flat", pending)
	if !strings.Contains(note, pairTestHL) || !strings.Contains(note, "no auto-reset") {
		t.Fatalf("the close message must say the pair accounting is pending: %q", note)
	}
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	e.mu.RLock()
	pending = pairEmergencyPendingFor(e.state, livePartition)
	e.mu.RUnlock()
	if len(pending) != 0 {
		t.Fatalf("committed native accounting must release the live reset: %v", pending)
	}
	e.assertBooks("committed native accounting", 0, 0)
}

func TestBasketPairNativeEmergencyRewrittenNotesNeverChangeSavedAccounting(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	f.fire()
	e.step(e.now.Add(time.Second))
	hl := f.member(pairTestHL)
	if len(hl) != 1 || hl[0].Status != basketActionCommitted {
		t.Fatalf("setup: the native close must commit: %+v", hl)
	}
	a := hl[0]
	var saved string
	if err := e.db().db.QueryRow(`SELECT settlement_json FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&saved); err != nil || saved == "" {
		t.Fatalf("setup: the native ledger must be saved: %v", err)
	}
	e.mu.Lock()
	a.RecoveryJSON = "{}"
	a.SettlementJSON = `{"v":1,"fills":[],"fees":[]}`
	e.rt().touchPairProgress()
	ok := e.bc.persistPair(e.rt())
	native := a.pairNative() && a.recoveryNote().NativeBooked
	e.mu.Unlock()
	var after string
	if err := e.db().db.QueryRow(`SELECT settlement_json FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !ok || after != saved || !native {
		t.Fatalf("a progress-only write with a rebuilt note must keep the saved native ledger and the native purpose: saved %v native %v", after == saved, native)
	}
	e.restart()
	for _, x := range f.member(pairTestHL) {
		if x.ActionID == a.ActionID && (!x.pairNative() || x.SettlementJSON != saved) {
			t.Fatalf("after a restart the native action keeps its purpose and ledger: %+v", x)
		}
	}
}
