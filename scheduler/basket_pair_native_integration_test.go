package main

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type pairLiveShapedDeps struct {
	VenueFor     func(StrategyConfig) (basketVenue, error)
	HLCloser     HyperliquidLiveCloser
	OKXCloser    OKXLiveCloser
	HLFetcher    HLStateFetcher
	OKXFetcher   OKXPositionsFetcher
	EntryQueries func(*entryIntent) error
}

func newPairLiveShapedAdmission(t *testing.T, doc []byte, dir string, deps pairLiveShapedDeps) (*pairResearchAdmission, *Config, error) {
	t.Helper()
	missing := []string{}
	if deps.VenueFor == nil {
		missing = append(missing, "transport")
	}
	if deps.HLCloser == nil || deps.OKXCloser == nil {
		missing = append(missing, "close")
	}
	if deps.HLFetcher == nil || deps.OKXFetcher == nil {
		missing = append(missing, "account")
	}
	if deps.EntryQueries == nil {
		missing = append(missing, "recovery")
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("the live-shaped fixture refuses to open a pair until every dependency is replaced: missing %v", missing)
	}
	raw, err := pairResearchRawPair(doc)
	if err != nil {
		return nil, nil, err
	}
	nonce, err := newPairAdmissionNonce()
	if err != nil {
		return nil, nil, err
	}
	adm := &pairResearchAdmission{nonce: nonce, kind: pairAdmissionLiveShapedFixture, runID: "fixture", stateDir: dir, deps: pairResearchDeps{VenueFor: deps.VenueFor, Evidence: pairTestEvidence(t, doc, nil)}, raw: raw}
	pairAdmissionRegistry.Store(nonce, adm)
	t.Cleanup(adm.release)
	cfg, err := loadPairResearchConfig(adm, doc)
	if err != nil {
		return nil, nil, err
	}
	if err := adm.checkStateDir(cfg); err != nil {
		return nil, nil, err
	}
	return adm, cfg, nil
}

func pairLiveShapedMutate(root map[string]interface{}) {
	for _, id := range []string{pairTestHL, pairTestOKX} {
		sc := pairTestStrategy(root, id)
		args := sc["args"].([]interface{})
		for i, a := range args {
			if a == "--mode=paper" {
				args[i] = "--mode=live"
			}
		}
	}
}

func newPairLiveShapedEnv(t *testing.T, closers *pairNativeClosers) *pairTestEnv {
	t.Helper()
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "fixture")
	doc := pairTestDoc(t, dir, pairLiveShapedMutate)
	venues := newPairTestVenues(true)
	if closers.hl == nil {
		closers.hl = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
			return nil, fmt.Errorf("the controlled fixture sends no hyperliquid close")
		}
	}
	if closers.okx == nil {
		closers.okx = func(string, *float64) (*OKXCloseResult, error) {
			return nil, fmt.Errorf("the controlled fixture sends no okx close")
		}
	}
	deps := pairLiveShapedDeps{
		VenueFor:     venues.factory(),
		HLCloser:     closers.hl,
		OKXCloser:    closers.okx,
		HLFetcher:    func(string) ([]HLPosition, error) { return nil, nil },
		OKXFetcher:   func() ([]OKXPosition, error) { return nil, nil },
		EntryQueries: func(*entryIntent) error { return fmt.Errorf("no live entry query is permitted") },
	}
	adm, cfg, err := newPairLiveShapedAdmission(t, doc, dir, deps)
	if err != nil {
		t.Fatalf("live-shaped admission: %v", err)
	}
	e := &pairTestEnv{t: t, dir: dir, doc: doc, adm: adm, cfg: cfg, venues: venues, mu: &sync.RWMutex{}, now: pairTestStart(), quotes: newPairFixtureQuotes(t, doc)}
	e.runner = &pairTestRunner{respond: func(req pairPolicyRequest) (pairPolicyResult, error) { return pairEnterResult(req, 0.06), nil }}
	e.store = openSourceStore(t, cfg)
	e.state = NewAppState()
	for _, sc := range cfg.Strategies {
		e.state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	e.reopen(false)
	return e
}

type pairNativeClosers struct {
	hl  HyperliquidLiveCloser
	okx OKXLiveCloser
}

func pairFee(out basketVenueOutcome, version int64, amount float64, currency string, final bool) basketVenueOutcome {
	out.FeePresent = true
	out.FeeCurrency = currency
	out.FeeAmount = amount
	out.FeeEvidenceID = "fee-1"
	out.FeeEvidenceVersion = version
	out.FeeFinal = final
	return out
}

func TestBasketPairLiveShapedFactoryRefusesUncontrolledDependencies(t *testing.T) {
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "fixture")
	doc := pairTestDoc(t, dir, pairLiveShapedMutate)
	venues := newPairTestVenues(true)
	full := pairLiveShapedDeps{
		VenueFor:     venues.factory(),
		HLCloser:     func(string, *float64, []int64) (*HyperliquidCloseResult, error) { return nil, nil },
		OKXCloser:    func(string, *float64) (*OKXCloseResult, error) { return nil, nil },
		HLFetcher:    func(string) ([]HLPosition, error) { return nil, nil },
		OKXFetcher:   func() ([]OKXPosition, error) { return nil, nil },
		EntryQueries: func(*entryIntent) error { return nil },
	}
	for name, strip := range map[string]func(*pairLiveShapedDeps){
		"transport": func(d *pairLiveShapedDeps) { d.VenueFor = nil },
		"close":     func(d *pairLiveShapedDeps) { d.OKXCloser = nil },
		"account":   func(d *pairLiveShapedDeps) { d.HLFetcher = nil },
		"recovery":  func(d *pairLiveShapedDeps) { d.EntryQueries = nil },
	} {
		deps := full
		strip(&deps)
		if _, _, err := newPairLiveShapedAdmission(t, doc, dir, deps); err == nil {
			t.Fatalf("the live-shaped fixture opened a pair without its controlled %s dependency", name)
		}
		if pairFileDigest(t, filepath.Join(dir, pairResearchManifestName)) != "missing" {
			t.Fatalf("a refused fixture wrote state for the missing %s dependency", name)
		}
	}
	if _, _, err := newPairResearchAdmission(doc, "run-live", filepath.Join(t.TempDir(), "r"), false, pairTestDeps(t, doc, venues)); err == nil {
		t.Fatal("the research admission must refuse a live-partition pair")
	}
}

func TestBasketPairFeeEvidenceAppliesOnceByVersion(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	fill := basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true}
	pairOpenVenue(e, pairTestOKX, fill)
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	if a.AppliedQty != 0 || a.Status != basketActionUnresolved && a.Status != basketActionSettling {
		t.Fatalf("a live-shaped fill without fee evidence must hold settlement: %+v", a)
	}
	if o := e.rt().Pair.Progress.obligation("fee_unconfirmed", pairTestOKX, a.ActionID); o == nil {
		t.Fatal("the held settlement must keep a visible fee obligation")
	}
	e.assertBooks("held for fee evidence", 0, 0)
	for _, bad := range []basketVenueOutcome{pairFee(fill, 1, 0, "USDT", false), pairFee(fill, 2, -0.5, "USDT", false), pairFee(fill, 3, 1.2, "", false)} {
		pairAnswerVenue(e, a, bad)
		e.step(e.now.Add(time.Second))
		if a.AppliedQty != 0 {
			t.Fatalf("zero, negative or currency-less fee evidence version %d booked a fill", bad.FeeEvidenceVersion)
		}
	}
	e.restart()
	a = e.actionsByPurpose(pairPurposeEntry)[0]
	if e.rt().Pair.Progress.obligation("fee_unconfirmed", pairTestOKX, a.ActionID) == nil || a.AppliedQty != 0 {
		t.Fatal("the interim fee hold must persist unchanged across a restart")
	}
	pairAnswerVenue(e, a, pairFee(fill, 4, 1.80, "USDT", false))
	e.step(e.now.Add(time.Second))
	e.assertBooks("booked with the confirmed fee", 0, -0.06)
	if math.Abs(a.AppliedFee-1.80) > 1e-9 || a.AppliedFeeVersion != 4 || a.Status != basketActionUnresolved {
		t.Fatalf("the booking must apply fee version 4 and stay queryable until the fee is final: %+v", a)
	}
	var tradeFee float64
	if err := e.db().db.QueryRow(`SELECT exchange_fee FROM trades WHERE symbol = 'BTC' AND exchange_order_id = ?`, a.ClientOrderID).Scan(&tradeFee); err != nil {
		t.Fatal(err)
	}
	if math.Abs(tradeFee-1.80) > 1e-9 {
		t.Fatalf("the trade row fee is %v, want 1.80", tradeFee)
	}
	prevFee := 1.80
	for _, step := range []struct {
		version int64
		amount  float64
		final   bool
	}{{5, 2.10, false}, {6, 1.95, true}} {
		cash := e.state.Strategies[pairTestOKX].Cash
		pairAnswerVenue(e, a, pairFee(fill, step.version, step.amount, "USDT", step.final))
		e.step(e.now.Add(time.Second))
		e.step(e.now.Add(time.Second))
		if got := e.state.Strategies[pairTestOKX].Cash; math.Abs((cash-got)-(step.amount-prevFee)) > 1e-9 {
			t.Fatalf("fee version %d moved cash by %v, want the signed delta %v once", step.version, cash-got, step.amount-prevFee)
		}
		prevFee = step.amount
		if math.Abs(a.AppliedFee-step.amount) > 1e-9 || a.AppliedFeeVersion != step.version {
			t.Fatalf("fee version %d must apply its signed delta once: %+v", step.version, a)
		}
		if err := e.db().db.QueryRow(`SELECT exchange_fee FROM trades WHERE symbol = 'BTC' AND exchange_order_id = ?`, a.ClientOrderID).Scan(&tradeFee); err != nil {
			t.Fatal(err)
		}
		if math.Abs(tradeFee-step.amount) > 1e-9 {
			t.Fatalf("after fee version %d the trade row fee is %v", step.version, tradeFee)
		}
	}
	if a.Status != basketActionCommitted {
		t.Fatalf("final fee evidence must commit the action, got %s", a.Status)
	}
	gotCash := e.state.Strategies[pairTestOKX].Cash
	e.restart()
	if math.Abs(e.state.Strategies[pairTestOKX].Cash-gotCash) > 1e-9 {
		t.Fatalf("a restart changed the cash after the fee deltas: %v -> %v", gotCash, e.state.Strategies[pairTestOKX].Cash)
	}
	var appliedFee float64
	var version int64
	if err := e.db().db.QueryRow(`SELECT applied_fee, applied_fee_version FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&appliedFee, &version); err != nil {
		t.Fatal(err)
	}
	if math.Abs(appliedFee-1.95) > 1e-9 || version != 6 {
		t.Fatalf("the saved applied fee is %v at version %d", appliedFee, version)
	}
	var intentFee float64
	if err := e.db().db.QueryRow(`SELECT applied_fee FROM entry_order_intents WHERE intent_id = ?`, a.EntryIntentID).Scan(&intentFee); err != nil {
		t.Fatal(err)
	}
	if math.Abs(intentFee-1.95) > 1e-9 {
		t.Fatalf("the linked intent applied fee is %v", intentFee)
	}
	conflict := pairFee(fill, 6, 9.99, "USDT", true)
	e2 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairOpenVenue(e2, pairTestOKX, pairFee(fill, 1, 1.5, "USDT", false))
	e2.slotStep(0)
	b := e2.actionsByPurpose(pairPurposeEntry)[0]
	conflict.FeeEvidenceVersion = 1
	pairAnswerVenue(e2, b, conflict)
	e2.step(e2.now.Add(time.Second))
	if math.Abs(b.AppliedFee-1.5) > 1e-9 || !e2.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("conflicting content at one fee identity and version must hold without applying: %+v", b)
	}
}

func TestBasketPairKillSwitchRostersResolveBothLegs(t *testing.T) {
	var env *pairTestEnv
	var seen []string
	hlFail := false
	closers := &pairNativeClosers{
		hl: func(symbol string, partialSz *float64, cancel []int64) (*HyperliquidCloseResult, error) {
			var status, purpose string
			if err := env.db().db.QueryRow(`SELECT status, purpose FROM basket_member_actions WHERE purpose = 'pair_kill_switch' AND instrument = 'BTC' ORDER BY created_at_ms DESC LIMIT 1`).Scan(&status, &purpose); err != nil {
				return nil, fmt.Errorf("no reserved reduction before the hyperliquid callback: %v", err)
			}
			seen = append(seen, "hl:"+status)
			if hlFail {
				return nil, fmt.Errorf("transport crash")
			}
			return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{AvgPx: 59900, TotalSz: 0.06}}}, nil
		},
		okx: func(symbol string, partialSz *float64) (*OKXCloseResult, error) {
			var status string
			if err := env.db().db.QueryRow(`SELECT status FROM basket_member_actions WHERE purpose = 'pair_kill_switch' AND instrument = 'BTC-USDT-SWAP'`).Scan(&status); err != nil {
				return nil, fmt.Errorf("no reserved reduction before the okx callback: %v", err)
			}
			seen = append(seen, "okx:"+status)
			return &OKXCloseResult{Close: &OKXClose{Symbol: symbol, Fill: &OKXCloseFill{AvgPx: 60050, TotalSz: 6}}}, nil
		},
	}
	env = newPairLiveShapedEnv(t, closers)
	env.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	fill := func(px float64) basketVenueOutcome {
		return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1, 1.5, "USDT", true)
	}
	pairOpenVenue(env, pairTestOKX, fill(59800))
	env.slotStep(0)
	pairOpenVenue(env, pairTestHL, fill(60200))
	env.slotStep(1)
	env.assertBooks("matched live-shaped pair", 0.06, -0.06)
	env.mu.Lock()
	env.state.Strategies[pairTestHL].Positions["BTC"].StopLossOID = 4242
	env.mu.Unlock()
	hlRoster := []StrategyConfig{env.bc.members[pairTestHL]}
	okxRoster := []StrategyConfig{env.bc.members[pairTestOKX]}
	hlFail = true
	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLStateFetched:  true,
		HLPositions:     []HLPosition{{Coin: "BTC", Size: 0.06}},
		HLLiveAll:       hlRoster,
		HLCloser:        env.bc.pairKillSwitchHLCloser(closers.hl),
		OKXLiveAllPerps: okxRoster,
		OKXCloser:       env.bc.pairKillSwitchOKXCloser(closers.okx),
		OKXFetcher:      func() ([]OKXPosition, error) { return []OKXPosition{{Coin: "BTC", Size: -6, Side: "short"}}, nil },
		CloseTimeout:    5 * time.Second,
	})
	if len(plan.Unconfigured) != 0 || len(plan.OKXUnconfigured) != 0 {
		t.Fatalf("the native rosters must resolve both pair legs without an unconfigured position: %+v %+v", plan.Unconfigured, plan.OKXUnconfigured)
	}
	if qty, ok := computeOKXCircuitCloseQty("BTC", pairTestOKX, []OKXPosition{{Coin: "BTC", Size: -6, Side: "short"}}, okxRoster); !ok || qty != 6 {
		t.Fatalf("the okx circuit-close roster must resolve the pair leg's full position, got %v %v", qty, ok)
	}
	if len(seen) != 2 || seen[0] != "hl:submitting" || seen[1] != "okx:submitting" {
		t.Fatalf("each native close must run after its reduction is reserved and marked submitting: %v", seen)
	}
	okxAct := env.actionsByPurpose(pairPurposeKillSwitch)
	var hlAct *basketAction
	for _, a := range okxAct {
		if a.StrategyID == pairTestHL {
			hlAct = a
		}
	}
	if hlAct == nil || hlAct.Status != basketActionUnresolved {
		t.Fatalf("a crashed native close must leave its reduction unresolved and queryable: %+v", hlAct)
	}
	env.externalBookChange(pairTestOKX, 0)
	if pos := env.state.Strategies[pairTestHL].Positions["BTC"]; pos == nil || pos.StopLossOID != 4242 {
		t.Fatalf("the surviving leg lost its protection: %+v", pos)
	}
	env.restart()
	var restored *basketAction
	for _, a := range env.rt().pairUnresolvedActions() {
		if a.Purpose == pairPurposeKillSwitch {
			restored = a
		}
	}
	if restored == nil || restored.ClientOrderID != hlAct.ClientOrderID {
		t.Fatalf("the unknown reduction must stay queryable by its saved identity after a crash: %+v", restored)
	}
	env.externalBookChange(pairTestHL, 0)
	trades := env.bookedTrades(pairTestHL)
	env.step(env.now.Add(time.Minute))
	if restored.Status != basketActionCommitted || env.bookedTrades(pairTestHL) != trades {
		t.Fatalf("the flat member book must end the unconfirmed native reduction without a client-id query or a second booking: %+v", restored)
	}
	if env.lifecycle() != pairLifecycleRecovery && env.lifecycle() != pairLifecycleFlat {
		t.Fatalf("the native closes must be observed as a pair exception, got %s", env.lifecycle())
	}
}

type pairKillSwitchFixture struct {
	env      *pairTestEnv
	closers  *pairNativeClosers
	hlCalls  int
	okxCalls int
	hlOut    func() (*HyperliquidCloseResult, error)
	okxOut   func() (*OKXCloseResult, error)
}

func newPairKillSwitchFixture(t *testing.T) *pairKillSwitchFixture {
	t.Helper()
	f := &pairKillSwitchFixture{}
	f.hlOut = func() (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "BTC", Fill: &HyperliquidCloseFill{AvgPx: 59900, TotalSz: 0.06}}}, nil
	}
	f.okxOut = func() (*OKXCloseResult, error) {
		return &OKXCloseResult{Close: &OKXClose{Symbol: "BTC", Fill: &OKXCloseFill{AvgPx: 60050, TotalSz: 6}}}, nil
	}
	f.closers = &pairNativeClosers{
		hl: func(symbol string, partialSz *float64, cancel []int64) (*HyperliquidCloseResult, error) {
			f.hlCalls++
			return f.hlOut()
		},
		okx: func(symbol string, partialSz *float64) (*OKXCloseResult, error) {
			f.okxCalls++
			return f.okxOut()
		},
	}
	f.env = newPairLiveShapedEnv(t, f.closers)
	f.env.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	fill := func(px float64) basketVenueOutcome {
		return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1, 1.5, "USDT", true)
	}
	pairOpenVenue(f.env, pairTestOKX, fill(59800))
	f.env.slotStep(0)
	pairOpenVenue(f.env, pairTestHL, fill(60200))
	f.env.slotStep(1)
	f.env.assertBooks("matched live-shaped pair", 0.06, -0.06)
	return f
}

func (f *pairKillSwitchFixture) fire() KillSwitchClosePlan {
	return planKillSwitchClose(KillSwitchCloseInputs{
		HLStateFetched:  true,
		HLPositions:     []HLPosition{{Coin: "BTC", Size: 0.06}},
		HLLiveAll:       []StrategyConfig{f.env.bc.members[pairTestHL]},
		HLCloser:        f.env.bc.pairKillSwitchHLCloser(f.closers.hl),
		OKXLiveAllPerps: []StrategyConfig{f.env.bc.members[pairTestOKX]},
		OKXCloser:       f.env.bc.pairKillSwitchOKXCloser(f.closers.okx),
		OKXFetcher:      func() ([]OKXPosition, error) { return []OKXPosition{{Coin: "BTC", Size: -6, Side: "short"}}, nil },
		CloseTimeout:    5 * time.Second,
	})
}

func (f *pairKillSwitchFixture) killSwitchActions(memberID string) []*basketAction {
	var out []*basketAction
	for _, a := range f.env.actionsByPurpose(pairPurposeKillSwitch) {
		if a.StrategyID == memberID {
			out = append(out, a)
		}
	}
	return out
}

func (f *pairKillSwitchFixture) assertRecoveryRecorded(t *testing.T, label string) {
	t.Helper()
	p := f.env.rt().Pair.Progress
	if p.Lifecycle != pairLifecycleRecovery || !p.IncreasesFrozen || !strings.Contains(p.FreezeReason+p.RecoveryReason, "without a complete pair record") {
		t.Fatalf("%s: the unrecorded native close must open recovery and freeze increases: %+v", label, p)
	}
	f.env.restart()
	p = f.env.rt().Pair.Progress
	if p.Lifecycle != pairLifecycleRecovery || !p.IncreasesFrozen {
		t.Fatalf("%s: the recovery record for the unrecorded native close must be saved: %+v", label, p)
	}
}

func TestBasketPairKillSwitchNeverGatesTheNativeClose(t *testing.T) {
	t.Run("a failed close does not block the next kill-switch cycle", func(t *testing.T) {
		f := newPairKillSwitchFixture(t)
		f.hlOut = func() (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("transport crash") }
		f.fire()
		first := f.killSwitchActions(pairTestHL)
		if f.hlCalls != 1 || len(first) != 1 || first[0].Status != basketActionUnresolved {
			t.Fatalf("cycle 1: the failed close must leave one unresolved native action: calls %d actions %+v", f.hlCalls, first)
		}
		f.hlOut = func() (*HyperliquidCloseResult, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "BTC", Fill: &HyperliquidCloseFill{AvgPx: 59900, TotalSz: 0.06}}}, nil
		}
		f.fire()
		if f.hlCalls != 2 {
			t.Fatalf("cycle 2: the kill switch must call the native close again, calls %d", f.hlCalls)
		}
		acts := f.killSwitchActions(pairTestHL)
		if len(acts) != 2 {
			t.Fatalf("cycle 2 must reserve a new native action: %+v %+v %+v", acts[0], f.env.rt().Pair.Progress.FreezeReason, f.env.rt().Pair.Progress.Alerts)
		}
		var old, next *basketAction
		for _, a := range acts {
			if a.ActionID == first[0].ActionID {
				old = a
			} else {
				next = a
			}
		}
		if old.Status != basketActionCommitted || !strings.Contains(old.HoldReason, "supersedes") {
			t.Fatalf("the earlier unconfirmed native action must end as superseded: %+v", old)
		}
		if next.Status != basketActionCommitted || !basketQtyEqual(next.ConfirmedQty, 0.06) {
			t.Fatalf("the cycle 2 native fill must commit: %+v", next)
		}
		if n := f.env.count(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'pair_kill_switch' AND instrument = 'BTC' AND status = 'committed'`); n != 2 {
			t.Fatalf("both native actions must be saved as committed, got %d", n)
		}
	})
	t.Run("a failed reservation commit still sends the close", func(t *testing.T) {
		f := newPairKillSwitchFixture(t)
		prev := storeCommitHook
		failed := false
		storeCommitHook = func(role storageRole) error {
			if !failed {
				failed = true
				return fmt.Errorf("disk full")
			}
			return nil
		}
		f.fire()
		storeCommitHook = prev
		if !failed || f.hlCalls != 1 {
			t.Fatalf("the native close must run after the reservation commit fails: failed %v calls %d", failed, f.hlCalls)
		}
		if acts := f.killSwitchActions(pairTestHL); len(acts) != 0 {
			t.Fatalf("a failed reservation must not leave an in-memory action: %+v", acts)
		}
		f.assertRecoveryRecorded(t, "failed reservation")
	})
	t.Run("a held entry lock still sends the close", func(t *testing.T) {
		f := newPairKillSwitchFixture(t)
		release, ok := tryLockEntryAttempt(pairTestHL)
		if !ok {
			t.Fatal("setup: the entry lock is busy")
		}
		f.fire()
		release()
		if f.hlCalls != 1 {
			t.Fatalf("the native close must run while another goroutine holds the entry lock, calls %d", f.hlCalls)
		}
		if acts := f.killSwitchActions(pairTestHL); len(acts) != 0 {
			t.Fatalf("no native action can be reserved without the entry lock: %+v", acts)
		}
		f.assertRecoveryRecorded(t, "held entry lock")
	})
}

func TestBasketPairKillSwitchPartialNativeFillIsConfirmedPartial(t *testing.T) {
	cases := []struct {
		name   string
		hlSz   float64
		okxSz  float64
		member string
		want   float64
		status string
	}{
		{name: "hyperliquid partial", hlSz: 0.03, okxSz: 6, member: pairTestHL, want: 0.03},
		{name: "okx partial", hlSz: 0.06, okxSz: 3, member: pairTestOKX, want: 0.03},
		{name: "hyperliquid full", hlSz: 0.06, okxSz: 6, member: pairTestHL, want: 0.06},
		{name: "okx full", hlSz: 0.06, okxSz: 6, member: pairTestOKX, want: 0.06},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPairKillSwitchFixture(t)
			f.hlOut = func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "BTC", Fill: &HyperliquidCloseFill{AvgPx: 59900, TotalSz: tc.hlSz}}}, nil
			}
			f.okxOut = func() (*OKXCloseResult, error) {
				return &OKXCloseResult{Close: &OKXClose{Symbol: "BTC", Fill: &OKXCloseFill{AvgPx: 60050, TotalSz: tc.okxSz}}}, nil
			}
			f.fire()
			acts := f.killSwitchActions(tc.member)
			if len(acts) != 1 {
				t.Fatalf("one native action expected: %+v", acts)
			}
			a := acts[0]
			if a.Status != basketActionCommitted || !basketQtyEqual(a.ConfirmedQty, tc.want) || strings.Contains(a.HoldReason, "inconsistent") {
				t.Fatalf("the native close must save %v confirmed with no inconsistency hold: %+v", tc.want, a)
			}
			if p := f.env.rt().Pair.Progress; strings.Contains(p.FreezeReason, "inconsistent") {
				t.Fatalf("the native fill must not freeze increases as inconsistent evidence: %s", p.FreezeReason)
			}
		})
	}
}

func TestBasketPairFeeCorrectionOnCloseKeepsTheLossStreak(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   float64
	}{{"fee raised", 2.30}, {"fee lowered", 1.30}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairLiveShapedEnv(t, &pairNativeClosers{})
			pairEnterExitRunner(e, 0.06)
			open := func(px float64) basketVenueOutcome {
				return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1, 1.5, "USDT", true)
			}
			pairOpenVenue(e, pairTestOKX, open(59800))
			e.slotStep(0)
			pairOpenVenue(e, pairTestHL, open(60200))
			e.slotStep(1)
			e.assertBooks("matched", 0.06, -0.06)
			exitFill := func(px float64) basketVenueOutcome {
				return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1, 1.80, "USDT", false)
			}
			hlExit := exitFill(59600)
			hlExit.FeeFinal = true
			pairOpenVenue(e, pairTestOKX, exitFill(60300))
			pairOpenVenue(e, pairTestHL, hlExit)
			for i := 2; i < 8 && e.signed(pairTestOKX) != 0; i++ {
				at := pairSlotAt(pairTestStart(), i)
				e.slot(at, fmt.Sprintf("in-%d", i))
				e.step(at.Add(time.Second + 123456789*time.Nanosecond))
			}
			if got := e.signed(pairTestOKX); got != 0 {
				t.Fatalf("setup: the okx member must exit, holds %v", got)
			}
			var exit *basketAction
			for _, a := range e.actionsByPurpose(pairPurposeExit) {
				if a.StrategyID == pairTestOKX {
					exit = a
				}
			}
			if exit == nil || exit.Status != basketActionUnresolved || !basketQtyEqual(exit.AppliedQty, 0.06) {
				t.Fatalf("setup: the okx exit must be booked and wait for final fee evidence: %+v", exit)
			}
			rows := exit.bookedTrades()
			if len(rows) == 0 || rows[len(rows)-1].ClosedPositionID == "" {
				t.Fatalf("setup: the okx exit must have closed the position: %+v", rows)
			}
			e.mu.Lock()
			s := e.state.Strategies[pairTestOKX]
			rolloverDailyPnL(&s.RiskState)
			s.RiskState.ConsecutiveLosses = 2
			daily := s.RiskState.DailyPnL
			e.mu.Unlock()
			if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
				t.Fatal(err)
			}
			pairAnswerVenue(e, exit, pairFee(exitFill(60300), 2, tc.v2, "USDT", true))
			e.step(e.now.Add(time.Second))
			e.step(e.now.Add(time.Second))
			if exit.AppliedFeeVersion != 2 {
				t.Fatalf("fee version 2 must apply: %+v", exit)
			}
			want := daily - (tc.v2 - 1.80)
			check := func(label string) {
				t.Helper()
				r := e.state.Strategies[pairTestOKX].RiskState
				if r.ConsecutiveLosses != 2 {
					t.Fatalf("%s: the fee correction changed the consecutive-loss count to %d", label, r.ConsecutiveLosses)
				}
				if math.Abs(r.DailyPnL-want) > 1e-9 {
					t.Fatalf("%s: daily PnL is %v, want %v (the signed delta once)", label, r.DailyPnL, want)
				}
			}
			check("after the fee update")
			var closedPnL, tradeFee float64
			if err := e.db().db.QueryRow(`SELECT realized_pnl FROM closed_positions WHERE symbol = 'BTC' AND side = 'short'`).Scan(&closedPnL); err != nil {
				t.Fatal(err)
			}
			if err := e.db().db.QueryRow(`SELECT exchange_fee FROM trades WHERE symbol = 'BTC' AND is_close = 1 AND exchange_order_id = ?`, exit.ClientOrderID).Scan(&tradeFee); err != nil {
				t.Fatal(err)
			}
			if math.Abs(closedPnL-(-31.80-(tc.v2-1.80))) > 1e-6 || math.Abs(tradeFee-tc.v2) > 1e-9 {
				t.Fatalf("the saved close must carry the corrected fee once: realized %v fee %v", closedPnL, tradeFee)
			}
			e.restart()
			check("after a restart")
			e.step(e.now.Add(time.Second))
			check("after the next cycle")
		})
	}
}

func TestBasketPairOrdinaryRecoveryAndReleaseRefusePairIntents(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	readIntent := func() (string, string) {
		var status, evidence string
		if err := e.db().db.QueryRow(`SELECT status, evidence_json FROM entry_order_intents WHERE intent_id = ?`, a.EntryIntentID).Scan(&status, &evidence); err != nil {
			t.Fatal(err)
		}
		return status, evidence
	}
	status0, ev0 := readIntent()
	if status0 != entryIntentSubmitting {
		t.Fatalf("setup: the pair intent is %s", status0)
	}
	far := time.Now().Add(24 * time.Hour)
	recoverEntryIntents(e.cfg, e.state, e.store, e.mu, nil, nil, far)
	deps := entryOperatorDeps{state: e.state, mu: e.mu, store: e.store, strategy: func(id string) (StrategyConfig, bool) {
		for _, sc := range e.cfg.Strategies {
			if sc.ID == id {
				return sc, true
			}
		}
		return StrategyConfig{}, false
	}}
	out := resolveEntryAttemptByOperator(deps, "op", pairTestOKX, a.EntryIntentID)
	if out.Released {
		t.Fatal("the operator released a pair-owned intent")
	}
	if s, ev := readIntent(); s != status0 || ev != ev0 {
		t.Fatalf("ordinary recovery or release changed the pair intent: %s %s", s, ev)
	}
	ordinary := []StrategyConfig{}
	for _, sc := range e.cfg.Strategies {
		c := sc
		c.PairID = ""
		c.pairMember = nil
		ordinary = append(ordinary, c)
	}
	orphanCfg := *e.cfg
	orphanCfg.Strategies = ordinary
	orphanCfg.Pairs = nil
	orphanCfg.pairAdmission = nil
	state := NewAppState()
	for _, sc := range ordinary {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	known := map[string]bool{pairTestHL: true, pairTestOKX: true}
	if err := e.store.LoadEntryGuards(state, ordinary, known); err != nil {
		t.Fatal(err)
	}
	in := state.Strategies[pairTestOKX].EntryGuard.Intents[a.EntryIntentID]
	if in == nil || !in.pairOwned() {
		t.Fatalf("the saved owner must survive without the in-memory group link: %+v", in)
	}
	recoverEntryIntents(&orphanCfg, state, e.store, e.mu, nil, nil, far)
	out = resolveEntryAttemptByOperator(entryOperatorDeps{state: state, mu: e.mu, store: e.store, strategy: func(id string) (StrategyConfig, bool) {
		for _, sc := range ordinary {
			if sc.ID == id {
				return sc, true
			}
		}
		return StrategyConfig{}, false
	}}, "op", pairTestOKX, a.EntryIntentID)
	if out.Released {
		t.Fatal("the operator released a pair intent through a config without the pair link")
	}
	if s, ev := readIntent(); s != status0 || ev != ev0 {
		t.Fatalf("recovery without the group link changed the pair intent: %s %s", s, ev)
	}
	next := *in
	next.EvidenceJSON = `{"outcome":"no_fill"}`
	if err := e.store.TransitionEntryIntent(pairTestOKX, &next, []string{entryIntentSubmitting}, entryIntentNoFill); err == nil {
		t.Fatal("the ordinary transition owner must refuse a pair-owned row from its saved owner")
	}
	if s, _ := readIntent(); s != status0 {
		t.Fatalf("the refused transition changed the pair intent to %s", s)
	}
	_ = context.Background()
}
