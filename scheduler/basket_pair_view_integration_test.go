package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func pairViewNear(a, b float64) bool {
	return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b))
}

func pairViewExpected(s *StrategyState, mark float64) float64 {
	pos := s.Positions["BTC"]
	if pos == nil {
		return s.Cash
	}
	if pos.Side == entrySideShort {
		return s.Cash + pos.Quantity*(pos.AvgCost-mark)
	}
	return s.Cash + pos.Quantity*(mark-pos.AvgCost)
}

func pairViewMatched(t *testing.T, e *pairTestEnv) {
	t.Helper()
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
}

func pairViewEvidence(member string, edit func(stamp *pairEvidenceStamp)) func(d *pairEvidenceDocument) {
	return func(d *pairEvidenceDocument) {
		for i := range d.Accounts {
			if d.Accounts[i].Key.MemberID == member && edit != nil {
				edit(&d.Accounts[i].pairEvidenceStamp)
			}
		}
	}
}

func pairViewStatus(t *testing.T, ss *StatusServer) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	ss.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status body: %v", err)
	}
	return out
}

func pairViewServer(e *pairTestEnv) *StatusServer {
	ss := NewStatusServer(e.state, e.mu, "", e.cfg.Strategies, e.store)
	ss.priceSymbols, ss.hlPerpsCoins, ss.okxPerpsCoins, ss.futuresSymbols = nil, nil, nil, nil
	return ss
}

func TestBasketMemberViewValuesEachMemberAtItsOwnMarkThroughEveryOwner(t *testing.T) {
	e := newPairTestEnvEvidence(t, func(root map[string]interface{}) {
		root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 90, "max_same_direction_notional_usd": 1000000}
	}, func(d *pairEvidenceDocument) {
		for i := range d.Conversions {
			if d.Conversions[i].Currency == "USDT" {
				d.Conversions[i].RateUSD = "0.999"
			}
		}
	})
	pairViewMatched(t, e)
	at := pairSlotAt(pairTestStart(), 2)
	e.now = at
	e.prepare(at)
	hlMark, okxMark := 60000.0, 60100*0.999

	e.mu.RLock()
	views := e.bc.pairViewsLocked(at)
	pm := e.bc.pairMarksLocked()
	hl, okx := e.state.Strategies[pairTestHL], e.state.Strategies[pairTestOKX]
	hlV, okxV := views[pairTestHL], views[pairTestOKX]
	gotHL, stHL := portfolioValueMarked(hl, map[string]float64{"BTC": 1}, pm)
	gotOKX, stOKX := portfolioValueMarked(okx, map[string]float64{"BTC": 1}, pm)
	legacyOKX := PortfolioValue(okx, map[string]float64{"BTC": 1})
	states := filterStatesByPartition(e.state.Strategies, e.cfg.Strategies, e.rt().Partition)
	notional, notionalStatus := portfolioNotionalMarked(states, map[string]float64{"BTC": 1}, pm)
	part := e.rt().Partition
	sr := measureScopeCycleRiskMarked(part, partitionRiskConfig(e.cfg, part), e.cfg.Strategies, e.state, map[string]float64{"BTC": 1}, nil, nil, true, false, at, pm)
	assets, _, assetStatus := computeAssetDeltasMarked(states, e.cfg.Strategies, map[string]float64{"BTC": 1}, pm)
	entries := buildLeaderboardEntries(e.cfg.Strategies, e.state, nil, nil, nil, 60)
	wantHL, wantOKX := pairViewExpected(hl, hlMark), pairViewExpected(okx, okxMark)
	okxCash := okx.Cash
	e.mu.RUnlock()

	if !hlV.MarkValid || !okxV.MarkValid || !hlV.CollateralValid || !okxV.CapabilityValid || hlV.Generation == 0 || hlV.Generation != okxV.Generation {
		t.Fatalf("both members need a complete view from one publication: %+v / %+v", hlV, okxV)
	}
	if !pairViewNear(hlV.MarkUSD, hlMark) || !pairViewNear(okxV.MarkUSD, okxMark) || okxV.ContractSize != 0.01 || okxV.Key.Instrument != "BTC-USDT-SWAP" || okxV.Key.AccountKey != "synthetic:okx-research" {
		t.Fatalf("the members must carry their own marks, keys and contract sizes: hl %v okx %v (%v %s %s)", hlV.MarkUSD, okxV.MarkUSD, okxV.ContractSize, okxV.Key.Instrument, okxV.Key.AccountKey)
	}
	if !stHL.complete() || !stOKX.complete() || !pairViewNear(gotHL, wantHL) || !pairViewNear(gotOKX, wantOKX) {
		t.Fatalf("each member book must be valued at its own converted mark: hl %v want %v, okx %v want %v", gotHL, wantHL, gotOKX, wantOKX)
	}
	if legacyOKX != okxCash {
		t.Fatalf("a nil-view wrapper must never value a pair member at a shared symbol price, got %v with cash %v", legacyOKX, okxCash)
	}
	if !notionalStatus.complete() || !pairViewNear(notional, 0.06*hlMark+0.06*okxMark) {
		t.Fatalf("the base quantity must be valued once per member, got notional %v", notional)
	}
	if !sr.PairValuation.complete() || !pairViewNear(sr.TotalPV, wantHL+wantOKX) || !pairViewNear(sr.TotalNotional, notional) {
		t.Fatalf("partition risk must use the same view: pv %v want %v, notional %v (%s)", sr.TotalPV, wantHL+wantOKX, sr.TotalNotional, sr.PairValuation.label())
	}
	if a := assets["BTC"]; !assetStatus.complete() || a == nil || !pairViewNear(a.NetDeltaUSD, 0.06*hlMark-0.06*okxMark) {
		t.Fatalf("the same-symbol members must net at their own marks, got %+v", a)
	}
	for _, en := range entries {
		want := map[string]float64{pairTestHL: wantHL, pairTestOKX: wantOKX}[en.ID]
		if en.Incomplete != "" || !pairViewNear(en.Value, want) {
			t.Fatalf("leaderboard entry %s must carry the view value %v, got %+v", en.ID, want, en)
		}
	}

	detached := e.bc.pairViews(at)
	d := detached[pairTestOKX]
	d.Conversions["USDT"] = 5
	d.ConversionEvidence[0].RateUSD = "9"
	d.Observation.NativeAsks[0].Px = "1"
	d.Capability.Meta.FeeCurrencies[0] = "XXX"
	again := e.bc.pairViews(at)[pairTestOKX]
	if again.Conversions["USDT"] != 0.999 || again.ConversionEvidence[0].RateUSD != "0.999" || again.Observation.NativeAsks[0].Px != "60100.1" || again.Capability.Meta.FeeCurrencies[0] != "USDT" {
		t.Fatalf("a returned view must be a detached copy: %+v", again)
	}

	ss := pairViewServer(e)
	status := pairViewStatus(t, ss)
	strats := status["strategies"].(map[string]interface{})
	if v := strats[pairTestOKX].(map[string]interface{})["portfolio_value"].(float64); !pairViewNear(v, wantOKX) || status["pair_valuation"] != nil {
		t.Fatalf("the status member row must reconcile to the view, got %v want %v (%v)", v, wantOKX, status["pair_valuation"])
	}
	if total := status["total_value"].(float64); !pairViewNear(total, wantHL+wantOKX) {
		t.Fatalf("the status total must reconcile to the member rows, got %v", total)
	}
	if accts, _ := status["pair_accounts"].([]interface{}); len(accts) != 2 {
		t.Fatalf("each member account resource must be listed once, got %v", status["pair_accounts"])
	}

	e.quotes.mu.Lock()
	e.quotes.refErr = errors.New("okx book outage")
	e.quotes.mu.Unlock()
	later := at.Add(time.Second)
	e.now = later
	e.prepare(later)
	status = pairViewStatus(t, ss)
	label, _ := status["pair_valuation"].(string)
	okxRow := status["strategies"].(map[string]interface{})[pairTestOKX].(map[string]interface{})
	if !strings.Contains(label, pairTestOKX+" "+pairMarkSubstituted) || okxRow["pair_valuation"] == nil {
		t.Fatalf("an outage must label the substituted member and the total, got %q / %v", label, okxRow["pair_valuation"])
	}
	e.mu.RLock()
	statusText := formatStatusResponse(e.state, nil)
	pnlText := formatPnLResponse(e.state, nil)
	posText := formatPositionsResponse(e.state, nil)
	board := formatLeaderboardResponse(e.cfg, e.state, nil, nil, 5)
	exposure := exposureCapStatusNote(e.cfg, e.state, nil)
	contribs, _ := portfolioWarningContributors(e.state, e.cfg.Strategies, part, nil, true)
	summary := strings.Join(FormatCategorySummary(1, 0, 0, 0, -1, nil, nil, e.cfg.Strategies, e.state, "perps", "", 60, 0, nil, nil), "\n")
	entries = buildLeaderboardEntries(e.cfg.Strategies, e.state, nil, nil, nil, 60)
	e.mu.RUnlock()
	overview, _, _ := ss.uiStrategyOverview(pairTestOKX)
	for name, text := range map[string]string{"status": statusText, "pnl": pnlText, "positions": posText, "leaderboard": board, "exposure": exposure, "summary": summary} {
		if !strings.Contains(text, "incomplete") && !strings.Contains(text, pairMarkSubstituted) {
			t.Fatalf("the %s report must label the incomplete value: %s", name, text)
		}
	}
	incompleteEntries := 0
	for _, en := range entries {
		if en.Incomplete != "" {
			incompleteEntries++
		}
	}
	if incompleteEntries != 1 || overview.PairValuation == "" {
		t.Fatalf("one leaderboard entry must be unranked and the overview labelled, got %d and %q", incompleteEntries, overview.PairValuation)
	}
	labelled := 0
	for _, c := range contribs {
		if c.ID == pairTestOKX && c.PnLLabel == "incomplete" {
			labelled++
		}
	}
	if labelled != 1 {
		t.Fatalf("the warning contributor row must be labelled incomplete: %+v", contribs)
	}
}

func TestBasketMemberViewPreparationPublishesAtomicallyAndNeverKeepsAnOlderView(t *testing.T) {
	at := pairTestStart()
	t.Run("a newer preparation replaces one still reading", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.now = at
		nested := false
		e.quotes.mu.Lock()
		e.quotes.onRead = func(r pairMemberRead) {
			if r.Side != "" && !nested {
				nested = true
				e.prepare(at)
			}
		}
		e.quotes.mu.Unlock()
		e.prepare(at)
		e.mu.RLock()
		gen, view := e.rt().pairViewGen, e.rt().pairView
		e.mu.RUnlock()
		if !nested || gen != 1 || !view[pairTestOKX].MarkValid || !view[pairTestHL].MarkValid {
			t.Fatalf("only the newer preparation may publish, got generation %d valid %v/%v", gen, view[pairTestHL].MarkValid, view[pairTestOKX].MarkValid)
		}
	})
	t.Run("changed records invalidate both members together", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.now = at
		e.prepare(at)
		changed := false
		e.quotes.mu.Lock()
		e.quotes.onRead = func(r pairMemberRead) {
			if r.Side != "" && !changed {
				changed = true
				e.mu.Lock()
				e.state.Strategies[pairTestOKX].Cash -= 1
				e.mu.Unlock()
			}
		}
		e.quotes.mu.Unlock()
		e.prepare(at.Add(time.Second))
		e.mu.RLock()
		gen, view := e.rt().pairViewGen, e.rt().pairView
		e.mu.RUnlock()
		hl, okx := view[pairTestHL], view[pairTestOKX]
		if gen != 2 || hl.MarkValid || okx.MarkValid || hl.Generation != okx.Generation || !strings.Contains(hl.MissingReason, "records changed") {
			t.Fatalf("a records change during preparation must publish explicit invalidity for both members, got generation %d: %+v", gen, hl.MissingReason)
		}
	})
	t.Run("a cancelled read holds the frozen entry and keeps the older view from authorizing it", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		e.now = at
		e.prepare(at)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.bc.parent = ctx
		e.slot(at, "in-0")
		e.now = at.Add(time.Second)
		e.bc.preparePairRisk(basketCycleInputs{Now: e.now, Once: true, PairSnapshotFor: e.sealFor, PairGeneration: e.feedGeneration})
		e.bc.parent = context.Background()
		e.bc.beginCycle()
		e.observe("")
		e.bc.step(e.inputs(e.now))
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
			t.Fatalf("an older valid view must not authorize an increase after a cancelled preparation, got %d actions", n)
		}
		if rb := e.rt().Rebalances[at.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "valuation is unavailable") {
			t.Fatalf("the frozen entry must end held on the unavailable view, got %+v", rb)
		}
	})
	t.Run("a restart starts with no view and rebuilds it before an increase", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.now = at
		e.prepare(at)
		e.restart()
		e.mu.RLock()
		empty := len(e.rt().pairView)
		e.mu.RUnlock()
		pairEnterHoldRunner(e, 0.06)
		e.slot(at, "in-0")
		e.now = at.Add(time.Second)
		e.bc.beginCycle()
		e.observe("")
		e.bc.step(e.inputs(e.now))
		if empty != 0 || len(e.actionsByPurpose(pairPurposeEntry)) != 0 {
			t.Fatalf("a restarted coordinator must hold increases until it prepares a view, got %d views and %d actions", empty, len(e.actionsByPurpose(pairPurposeEntry)))
		}
		e.slot(pairSlotAt(at, 1), "in-1")
		e.step(pairSlotAt(at, 1).Add(time.Second))
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 1 {
			t.Fatalf("the rebuilt view must authorize the next frozen entry, got %d actions", n)
		}
	})
}

func TestBasketMemberViewIndependentEvidenceExpiryHoldsIncreases(t *testing.T) {
	at := pairTestStart()
	expiry := at.Add(1500 * time.Millisecond).UnixMilli()
	cases := []struct {
		name string
		edit func(d *pairEvidenceDocument)
	}{
		{"account", pairViewEvidence(pairTestOKX, func(s *pairEvidenceStamp) { s.ValidThroughMs = expiry })},
		{"conversion", func(d *pairEvidenceDocument) {
			for i := range d.Conversions {
				if d.Conversions[i].Key.MemberID == pairTestOKX {
					d.Conversions[i].ValidThroughMs = expiry
				}
			}
		}},
		{"capability", func(d *pairEvidenceDocument) {
			for i := range d.Capabilities {
				if d.Capabilities[i].Key.MemberID == pairTestOKX {
					d.Capabilities[i].ValidThroughMs = expiry
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" expired after preparation with a fresh mark", func(t *testing.T) {
			e := newPairTestEnvEvidence(t, nil, tc.edit)
			e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
				e.now = at.Add(2 * time.Second)
				return pairEnterResult(r, 0.06), nil
			}
			e.slot(at, "in-0")
			e.step(at.Add(time.Second))
			if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
				t.Fatalf("an expired %s record must hold the increase at its dispatch time, got %d actions", tc.name, n)
			}
			if rb := e.rt().Rebalances[at.UnixMilli()]; rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "not valid at") {
				t.Fatalf("the first dispatch must end held on the expired %s record, got %s %q", tc.name, rb.Status, rb.HoldReason)
			}
		})
	}
	t.Run("unexpired evidence dispatches", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			e.now = at.Add(2 * time.Second)
			return pairEnterResult(r, 0.06), nil
		}
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 1 {
			t.Fatalf("valid evidence must dispatch the first leg, got %d actions", n)
		}
	})
	t.Run("account expiry at the unlocked transport boundary sends nothing", func(t *testing.T) {
		e := newPairTestEnvEvidence(t, nil, pairViewEvidence(pairTestOKX, func(s *pairEvidenceStamp) { s.ValidThroughMs = expiry }))
		pairEnterHoldRunner(e, 0.06)
		e.slot(at, "in-0")
		e.bc.pairClock = func() time.Time {
			if e.mu.TryLock() {
				e.mu.Unlock()
				return e.now.Add(600 * time.Millisecond)
			}
			return e.now
		}
		e.step(at.Add(time.Second))
		acts := e.actionsByPurpose(pairPurposeEntry)
		if len(acts) != 1 || acts[0].Status != basketActionNoFill || len(e.venues.okx.submits) != 0 || !strings.Contains(acts[0].HoldReason, "transport boundary") {
			t.Fatalf("an account record expiring before the unlocked send must send nothing: %+v sends=%d", acts, len(e.venues.okx.submits))
		}
	})
}

func TestBasketMemberViewCollateralCountsPostedMarginAndFeesOnce(t *testing.T) {
	later := func(d *pairEvidenceDocument) {
		for _, a := range append([]pairAccountEvidence{}, d.Accounts...) {
			if a.Key.MemberID != pairTestOKX {
				continue
			}
			a.EvidenceID = "acct-later-" + pairTestOKX
			a.ObservedMs, a.FirstReceiptMs, a.ValidFromMs = 1_750_000_000_000, 1_750_000_000_000, 1_750_000_000_000
			a.Equity, a.FreeCollateral, a.MarginUsed = "1000000000", "1000000000", "0"
			d.Accounts = append(d.Accounts, a)
		}
	}
	for _, tc := range []struct {
		name     string
		evidence func(d *pairEvidenceDocument)
	}{{"the admitted account record", nil}, {"a later account record that cannot prove inclusion", later}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnvEvidence(t, nil, tc.evidence)
			pairEnterHoldRunner(e, 0.06)
			pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
			e.slotStep(0)
			e.slotStep(1)
			e.mu.RLock()
			okx := e.state.Strategies[pairTestOKX]
			pos := okx.Positions["BTC"]
			plan := e.bc.pairPlanFor(e.rt(), e.bc.pairValuationsLocked(e.rt()).at(e.now))
			none, err := plan.reading(plan.Pending, make([]bool, len(plan.Pending)))
			all := make([]bool, len(plan.Pending))
			for i := range all {
				all[i] = true
			}
			with, err2 := plan.reading(plan.Pending, all)
			cash := okx.Cash
			capital := okx.InitialCapital
			leg := plan.Legs[pairTestOKX]
			hlLeg := plan.Legs[pairTestHL]
			e.mu.RUnlock()
			if err != nil || err2 != nil || pos == nil || len(plan.Pending) != 1 {
				t.Fatalf("setup: one pending hedge and a booked first leg are needed: %v %v %d", err, err2, len(plan.Pending))
			}
			posted := pos.Quantity * pos.AvgCost * 0.5
			if !pairViewNear(none.Available[pairTestOKX], cash-posted) || !pairViewNear(capital-cash, pos.Quantity*pos.AvgCost*0.00045) {
				t.Fatalf("free collateral must subtract posted margin from cash once and the booked fee never again: %v want %v (fee %v)", none.Available[pairTestOKX], cash-posted, capital-cash)
			}
			upnl := -pos.Quantity * (leg.UpperUSD - pos.AvgCost)
			if !pairViewNear(none.Equity[pairTestOKX], cash+upnl) || pairViewNear(none.Equity[pairTestOKX], cash+posted) {
				t.Fatalf("equity must be cash plus the member's adverse unrealized result, without posted margin added: %v want %v", none.Equity[pairTestOKX], cash+upnl)
			}
			bound := plan.Pending[0].PriceBound
			u := math.Max(hlLeg.UpperUSD, bound)
			want := 0.06*u*0.5 + 0.06*u*0.00045
			if none.Required[pairTestHL] != 0 || !pairViewNear(with.Required[pairTestHL], want) {
				t.Fatalf("the pending hedge must reserve its margin and fee once at its endpoint: none %v with %v want %v", none.Required[pairTestHL], with.Required[pairTestHL], want)
			}
		})
	}
}

func TestBasketMemberViewIncompleteValuationDefersTheKillSwitchAndNeverRaisesThePeak(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 2}
		for _, id := range []string{pairTestHL, pairTestOKX} {
			pairTestStrategy(root, id)["max_drawdown_pct"] = 95
		}
	})
	if err := bindTradingClock("b1876-view", func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock("b1876-view") })
	pairViewMatched(t, e)
	h := newPairTestHost(e, time.Minute)
	var memberReasons []string
	h.manage = func(time.Time, map[RiskPartition]*scopeCycleRisk) {
		e.observe("")
		e.mu.Lock()
		pm := e.bc.pairMarksLocked()
		for _, id := range []string{pairTestHL, pairTestOKX} {
			sc := e.bc.members[id]
			_, reason, _ := checkPairMemberRisk(&sc, e.state.Strategies[id], pm, nil, nil)
			memberReasons = append(memberReasons, reason)
		}
		e.mu.Unlock()
	}
	part := e.rt().Partition
	read := func() (PortfolioRiskState, float64) {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return *e.state.partitionRisk(part), e.state.Strategies[pairTestHL].RiskState.CurrentDrawdownPct
	}
	t0 := pairSlotAt(pairTestStart(), 2)
	h.run(t0)
	base, _ := read()
	if base.PeakValue <= 0 || base.KillSwitchActive {
		t.Fatalf("a complete reading must set the peak without a latch: %+v", base)
	}
	setHL := func(px float64) {
		e.quotes.mu.Lock()
		defer e.quotes.mu.Unlock()
		e.quotes.hlBids = [][2]string{{pairViewPx(px - 1), "5"}, {pairViewPx(px - 2), "5"}, {pairViewPx(px - 3), "5"}, {pairViewPx(px - 4), "5"}, {pairViewPx(px - 5), "5"}}
		e.quotes.hlAsks = [][2]string{{pairViewPx(px + 1), "5"}, {pairViewPx(px + 2), "5"}, {pairViewPx(px + 3), "5"}, {pairViewPx(px + 4), "5"}, {pairViewPx(px + 5), "5"}}
	}
	e.quotes.mu.Lock()
	e.quotes.refErr = errors.New("okx book outage")
	e.quotes.mu.Unlock()
	setHL(90000)
	h.run(t0.Add(time.Minute))
	risen, _ := read()
	if risen.PeakValue != base.PeakValue || risen.KillSwitchActive {
		t.Fatalf("an incomplete reading must never raise the peak, got %v from %v", risen.PeakValue, base.PeakValue)
	}
	setHL(50000)
	h.run(t0.Add(2 * time.Minute))
	fell, hlDD := read()
	if fell.KillSwitchActive || fell.UntrustedOverLimitSince.IsZero() || fell.CurrentDrawdownPct <= 2 || fell.PeakValue != base.PeakValue {
		t.Fatalf("an incomplete over-limit reading must defer the latch, keep the peak and show the drawdown: %+v", fell)
	}
	if e.signed(pairTestHL) == 0 || e.signed(pairTestOKX) == 0 {
		t.Fatal("a deferred latch must not force-close the pair")
	}
	if hlDD <= 0 {
		t.Fatalf("the member circuit-breaker drawdown must use the member's own falling mark, got %v (%v)", hlDD, memberReasons)
	}
	h.run(t0.Add(2*time.Minute + untrustedEquityLatchDeferral + time.Second))
	latched, _ := read()
	if !latched.KillSwitchActive {
		t.Fatalf("an incomplete over-limit reading that lasts past the deferral must latch, got %+v", latched)
	}
}

func pairViewPx(px float64) string {
	b, _ := json.Marshal(px)
	return string(b)
}

func TestBasketMemberViewNilViewRiskReadersFailClosed(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 90, "max_notional_usd": 10000000}
	})
	pairViewMatched(t, e)
	at := pairSlotAt(pairTestStart(), 2)
	e.now = at
	e.prepare(at)
	e.mu.Lock()
	defer e.mu.Unlock()
	sc := e.bc.members[pairTestHL]
	hl := e.state.Strategies[pairTestHL]
	pos := hl.Positions["BTC"]
	if pos == nil || pos.Side != "long" {
		t.Fatalf("setup: the hyperliquid member must hold a long book: %+v", pos)
	}
	pos.AvgCost = 60000
	shared := map[string]float64{"BTC": 50000}

	hl.RiskState.PeakValue = 1
	peak, dd := hl.RiskState.PeakValue, hl.RiskState.CurrentDrawdownPct
	pv := PortfolioValue(hl, shared)
	allowed, reason := CheckRisk(&sc, hl, pv, shared, nil, nil)
	if allowed || !strings.Contains(reason, pairTestHL+" "+pairMarkUnavailable) || !strings.Contains(reason, "no coordinator valuation view") {
		t.Fatalf("a nil-view risk check of a held pair member must hold with a reason, got %v %q", allowed, reason)
	}
	if hl.RiskState.PeakValue != peak || hl.RiskState.CurrentDrawdownPct != dd || hl.RiskState.CircuitBreaker {
		t.Fatalf("a nil-view risk check must not raise the peak or write a drawdown: %+v", hl.RiskState)
	}
	hl.RiskState.CircuitBreaker, hl.RiskState.CircuitBreakerUntil = true, tradingNow().Add(time.Hour)
	if ok, why := CheckRisk(&sc, hl, pv, shared, nil, nil); ok || why != RiskReasonCircuitBreakerActive {
		t.Fatalf("an active circuit breaker must keep its own reason without a view, got %v %q", ok, why)
	}
	hl.RiskState.CircuitBreaker, hl.RiskState.CircuitBreakerUntil = false, time.Time{}

	pm := e.bc.pairMarksLocked()
	hlMark := e.bc.pairViewsLocked(at)[pairTestHL].MarkUSD
	viewPV, _ := portfolioValueMarked(hl, shared, pm)
	if ok, why := checkStrategyRiskWithView(&sc, hl, viewPV, shared, pm, nil, nil); !ok || why != "" || !pairViewNear(hl.RiskState.PeakValue, pairViewExpected(hl, hlMark)) {
		t.Fatalf("the host risk check must value the member at its own view mark, got %v %q peak %v want %v", ok, why, hl.RiskState.PeakValue, pairViewExpected(hl, hlMark))
	}

	states := filterStatesByPartition(e.state.Strategies, e.cfg.Strategies, e.rt().Partition)
	if _, st := portfolioNotionalMarked(states, shared, nil); st.complete() || st.Members[pairTestHL] != pairMarkUnavailable {
		t.Fatalf("a nil-view notional over a held member must read incomplete, got %q", st.label())
	}
	if _, _, st := aggregatePerpsMarginInputsMarked(states, e.cfg.Strategies, shared, nil); st.complete() {
		t.Fatal("a nil-view margin reading over a held member must read incomplete")
	}
	if held, detail := evaluateNotionalCapHold(partitionRiskConfig(e.cfg, e.rt().Partition), states, shared); !held || !strings.Contains(detail, "incomplete") {
		t.Fatalf("the nil-view notional cap must hold, got %v %q", held, detail)
	}

	newOrdinary := func() *StrategyState {
		return &StrategyState{ID: "ordinary-perps", Type: "perps", Cash: 1000, RiskState: RiskState{MaxDrawdownPct: 50}, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.01, AvgCost: 60000, Side: "long", Multiplier: 1},
		}}
	}
	ordinaryCfg := StrategyConfig{ID: "ordinary-perps", Type: "perps", Platform: "hyperliquid", Leverage: 2, MaxDrawdownPct: 50}
	ordinary, twin := newOrdinary(), newOrdinary()
	wantOK, wantWhy := checkRiskValued(&ordinaryCfg, twin, PortfolioValue(twin, shared), shared, true, nil, nil, nil)
	gotOK, gotWhy := CheckRisk(&ordinaryCfg, ordinary, PortfolioValue(ordinary, shared), shared, nil, nil)
	if gotOK != wantOK || gotWhy != wantWhy || ordinary.RiskState.PeakValue != twin.RiskState.PeakValue || ordinary.RiskState.CurrentDrawdownPct != twin.RiskState.CurrentDrawdownPct || ordinary.RiskState.CircuitBreaker != twin.RiskState.CircuitBreaker {
		t.Fatalf("an ordinary strategy risk check must not change: %v %q %+v vs %v %q %+v", gotOK, gotWhy, ordinary.RiskState, wantOK, wantWhy, twin.RiskState)
	}
	if !gotOK || gotWhy != "" || ordinary.RiskState.PeakValue != 900 || !pairViewNear(ordinary.RiskState.CurrentDrawdownPct, 40) {
		t.Fatalf("an ordinary strategy must keep its symbol-price drawdown, got %v %q %+v", gotOK, gotWhy, ordinary.RiskState)
	}
	if v, st := portfolioValueMarked(ordinary, shared, pm); !st.complete() || v != PortfolioValue(ordinary, shared) || !pairViewNear(v, 1000-100) {
		t.Fatalf("an ordinary strategy value must not change, got %v", v)
	}
	if n, st := portfolioNotionalMarked(map[string]*StrategyState{ordinary.ID: ordinary}, shared, pm); !st.complete() || !pairViewNear(n, 500) {
		t.Fatalf("an ordinary strategy notional must not change, got %v", n)
	}
}

func TestBasketMemberViewManualOpenGatesReadTheView(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 90, "max_notional_usd": 10000000, "max_same_direction_notional_usd": 10000000}
		root["strategies"] = append(root["strategies"].([]interface{}), map[string]interface{}{
			"id": "hl-manual-eth", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h",
			"args": []interface{}{"hold", "ETH", "1h", "--mode=paper"}, "leverage": 1.0, "capital": 1000.0,
		})
	})
	pairViewMatched(t, e)
	at := pairSlotAt(pairTestStart(), 2)
	e.now = at
	e.prepare(at)
	manual := func() manualStateView {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return manualStateViewFromStateWithStore(e.cfg, e.state, e.store, "hl-manual-eth", "ETH")
	}
	v := manual()
	if p, ok := partitionOfStrategyID(e.cfg.Strategies, "hl-manual-eth"); !ok || p != e.rt().Partition {
		t.Fatalf("setup: the manual strategy must share the pair partition, got %v %v", p, ok)
	}
	if v.NotionalHold || !v.ExposureCap.Configured || v.ExposureCap.ValuationIncomplete != "" {
		t.Fatalf("a complete view must not hold the manual open: notional %v %q, exposure %+v", v.NotionalHold, v.NotionalNote, v.ExposureCap)
	}
	if blocked, why := exposureCapManualEntryBlock(v.ExposureCap, "ETH", "long"); blocked {
		t.Fatalf("a complete view must not block a manual ETH open: %s", why)
	}

	e.quotes.mu.Lock()
	e.quotes.refErr = errors.New("okx book outage")
	e.quotes.mu.Unlock()
	later := at.Add(time.Second)
	e.now = later
	e.prepare(later)
	v = manual()
	label := pairTestOKX + " " + pairMarkSubstituted
	if !v.NotionalHold || !strings.Contains(v.NotionalNote, label) {
		t.Fatalf("an incomplete view must hold the manual open through the notional gate with its label, got %v %q", v.NotionalHold, v.NotionalNote)
	}
	if blocked, _ := exposureCapManualEntryBlock(v.ExposureCap, "ETH", "long"); !blocked || !strings.Contains(v.ExposureCap.ValuationIncomplete, label) {
		t.Fatalf("an incomplete view must hold the manual open through the exposure gate with its label, got %v %+v", blocked, v.ExposureCap)
	}
}

func pairViewSetHL(e *pairTestEnv, px float64) {
	e.quotes.mu.Lock()
	defer e.quotes.mu.Unlock()
	e.quotes.hlBids = [][2]string{{pairViewPx(px - 1), "5"}, {pairViewPx(px - 2), "5"}, {pairViewPx(px - 3), "5"}, {pairViewPx(px - 4), "5"}, {pairViewPx(px - 5), "5"}}
	e.quotes.hlAsks = [][2]string{{pairViewPx(px + 1), "5"}, {pairViewPx(px + 2), "5"}, {pairViewPx(px + 3), "5"}, {pairViewPx(px + 4), "5"}, {pairViewPx(px + 5), "5"}}
}

type pairEscalationEnv struct {
	e       *pairTestEnv
	h       *pairTestHost
	reasons []string
	hlSeal  string
}

func newPairEscalationEnv(t *testing.T, portfolioDD float64) *pairEscalationEnv {
	t.Helper()
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		if portfolioDD > 0 {
			root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": portfolioDD}
		}
		for _, id := range []string{pairTestHL, pairTestOKX} {
			pairTestStrategy(root, id)["max_drawdown_pct"] = 95
		}
		pairTestStrategy(root, pairTestHL)["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 50.0, "close_fraction": 1.0}}}}
	})
	clock := "b1876-escalate-" + t.Name()
	if err := bindTradingClock(clock, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock(clock) })
	pairViewMatched(t, e)
	e.mu.Lock()
	pos := e.state.Strategies[pairTestHL].Positions["BTC"]
	if pos == nil || pos.Side != "long" || !basketQtyEqual(pos.Quantity, 0.06) {
		e.mu.Unlock()
		t.Fatalf("setup: the hyperliquid member must be long 0.06 BTC: %+v", pos)
	}
	pos.AvgCost = 60000
	e.mu.Unlock()
	pairViewSetHL(e, 61000)
	x := &pairEscalationEnv{e: e, h: newPairTestHost(e, time.Minute)}
	x.h.prepare = func(in basketCycleInputs) {
		in.PairSnapshotFor = func(rt *basketRuntime, at time.Time) *marketSnapshot {
			snap := e.sealFor(rt, at)
			for _, m := range rt.Contract.pairMembersSorted() {
				if m.Source != pairSourceSealedFeed {
					continue
				}
				for k := range snap.books {
					switch x.hlSeal {
					case "outage":
						delete(snap.books, k)
					case "stale":
						e.quotes.mu.Lock()
						bids, asks := e.quotes.hlBids, e.quotes.hlAsks
						e.quotes.mu.Unlock()
						old := pairFixtureBook(m.Instrument, at.UnixMilli()-50-6000, rt.Contract.Pair.BookNeed, bids, asks)
						snap.books[k] = old
					}
				}
			}
			return snap
		}
		e.bc.preparePairRisk(in)
	}
	x.h.manage = func(time.Time, map[RiskPartition]*scopeCycleRisk) {
		e.observe("")
		e.mu.Lock()
		defer e.mu.Unlock()
		sc := e.bc.members[pairTestHL]
		_, why := checkStrategyRiskWithView(&sc, e.state.Strategies[pairTestHL], 0, nil, e.bc.pairMarksLocked(), nil, nil)
		x.reasons = append(x.reasons, why)
	}
	return x
}

func (x *pairEscalationEnv) outage() {
	x.hlSeal = "outage"
}

func (x *pairEscalationEnv) read() (PortfolioRiskState, RiskState) {
	x.e.mu.RLock()
	defer x.e.mu.RUnlock()
	return *x.e.state.partitionRisk(x.e.rt().Partition), x.e.state.Strategies[pairTestHL].RiskState
}

func (x *pairEscalationEnv) runEvery(from, through time.Time) {
	for at := from; !at.After(through); at = at.Add(time.Minute) {
		x.h.run(at)
	}
}

func (x *pairEscalationEnv) lostAt() int64 {
	x.e.mu.RLock()
	defer x.e.mu.RUnlock()
	return x.e.rt().pairView[pairTestHL].MarkLostAtMs
}

func TestBasketMemberViewIncompleteValuationEscalatesAfterTheDeferral(t *testing.T) {
	limit := untrustedEquityLatchDeferral
	t.Run("the partition latches after an unbroken outage", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		base, _ := x.read()
		if base.PeakValue <= 0 || base.KillSwitchActive {
			t.Fatalf("a complete reading must set the peak without a latch: %+v", base)
		}
		t0 := tPre.Add(time.Minute)
		x.outage()
		pairViewSetHL(x.e, 50000)
		x.h.marks = map[string]float64{"BTC": 50000}
		x.runEvery(t0, t0.Add(limit-2*time.Minute))
		held, hl := x.read()
		if held.KillSwitchActive || held.PeakValue != base.PeakValue || hl.CircuitBreaker {
			t.Fatalf("inside the deferral the reading must hold without a latch and keep the peak: %+v %+v", held, hl)
		}
		x.h.run(t0.Add(limit + time.Second))
		latched, _ := x.read()
		if !latched.KillSwitchActive || latched.PeakValue != base.PeakValue {
			t.Fatalf("an outage longer than the deferral must latch the partition, got %+v", latched)
		}
		last := latched.Events[len(latched.Events)-1]
		if last.Source != "pair_valuation" || !strings.Contains(last.Details, pairTestHL+" has had no checked mark since") {
			t.Fatalf("the latch must name the member with no checked mark: %+v", last)
		}
	})
	t.Run("the latch closes each member only at its own checked mark", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		pairViewSetHL(x.e, 50000)
		x.h.marks = map[string]float64{"BTC": 50000}
		x.runEvery(t0, t0.Add(limit-2*time.Minute))
		tl := t0.Add(limit + time.Second)
		x.h.run(tl)
		latched, _ := x.read()
		if !latched.KillSwitchActive || latched.KillSwitchCloseApplied {
			t.Fatalf("the latch must hold its close as not applied while a pair member has no checked mark: %+v", latched)
		}
		x.e.mu.RLock()
		okxView := x.e.rt().pairView[pairTestOKX]
		x.e.mu.RUnlock()
		own, valid, why := okxView.usdMarkAt(tl.UnixMilli())
		tr, ok := x.e.closeTrade(pairTestOKX)
		if x.e.signed(pairTestOKX) != 0 || !valid || !ok || !pairCostNear(tr.Price, own) || pairCostNear(tr.Price, 50000) {
			t.Fatalf("the healthy okx member must close at its own checked mark %v (%s), never the shared 50000: book %v trade %+v", own, why, x.e.signed(pairTestOKX), tr)
		}
		if x.e.signed(pairTestHL) != 0.06 || !x.e.hasObligation(pairObligationEmergencyClose, pairTestHL) {
			t.Fatalf("the hyperliquid member without a mark must keep its book and an emergency obligation: %v %v", x.e.signed(pairTestHL), x.e.obligationKinds(pairTestHL))
		}
		if got, ok := x.h.incomplete(); !ok || got.Kind != pairObligationEmergencyClose || got.MemberID != pairTestHL {
			t.Fatalf("the incomplete-result reader must name the held member, got %+v %v", got, ok)
		}
		x.runEvery(tl.Add(time.Minute), tl.Add(3*time.Minute))
		x.restartAt(tl.Add(4 * time.Minute))
		x.h.run(tl.Add(4 * time.Minute))
		if prs, _ := x.read(); prs.KillSwitchCloseApplied || x.e.signed(pairTestHL) != 0.06 || !x.e.hasObligation(pairObligationEmergencyClose, pairTestHL) {
			t.Fatalf("the held member must stay held with its latch across latched cycles and a restart: %+v %v", prs, x.e.signed(pairTestHL))
		}
		x.hlSeal = ""
		pairViewSetHL(x.e, 49000)
		tb := tl.Add(5 * time.Minute)
		x.h.run(tb)
		x.e.mu.RLock()
		hlView := x.e.rt().pairView[pairTestHL]
		x.e.mu.RUnlock()
		hlOwn, hlValid, hlWhy := hlView.usdMarkAt(tb.UnixMilli())
		hlTr, ok := x.e.closeTrade(pairTestHL)
		if x.e.signed(pairTestHL) != 0 || !hlValid || !ok || !pairCostNear(hlTr.Price, hlOwn) || x.e.hasObligation(pairObligationEmergencyClose, pairTestHL) {
			t.Fatalf("a later checked mark must close the held member at that own mark %v (%s): book %v trade %+v", hlOwn, hlWhy, x.e.signed(pairTestHL), hlTr)
		}
		x.h.run(tb.Add(time.Minute))
		if prs, _ := x.read(); !prs.KillSwitchCloseApplied {
			t.Fatalf("the latch close applies only after every pair close is committed: %+v", prs)
		}
	})
	t.Run("both members without marks stay held", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		x.h.marks = map[string]float64{"BTC": 50000}
		x.runEvery(t0, t0.Add(limit-2*time.Minute))
		x.e.quotes.mu.Lock()
		x.e.quotes.refErr = errors.New("quote outage")
		x.e.quotes.mu.Unlock()
		tl := t0.Add(limit + time.Second)
		x.h.run(tl)
		x.h.run(tl.Add(time.Minute))
		prs, _ := x.read()
		if !prs.KillSwitchActive || prs.KillSwitchCloseApplied {
			t.Fatalf("the latch must stay with no close applied: %+v", prs)
		}
		x.e.assertBooks("both members held", 0.06, -0.06)
		for _, id := range []string{pairTestHL, pairTestOKX} {
			if !x.e.hasObligation(pairObligationEmergencyClose, id) {
				t.Fatalf("%s without a checked mark must keep its own emergency obligation: %v", id, x.e.obligationKinds(id))
			}
		}
		if !x.h.savedIncomplete() && !x.e.savedProgressHas(pairObligationEmergencyClose) {
			t.Fatal("the emergency obligations must be saved")
		}
		x.e.mu.Lock()
		ResetPortfolioKillSwitchManual(x.e.state.partitionRisk(x.e.rt().Partition))
		x.e.mu.Unlock()
		if prs, _ := x.read(); prs.KillSwitchActive {
			t.Fatalf("the operator reset must clear the latch even while pair accounting is held: %+v", prs)
		}
		if got, ok := x.h.incomplete(); !ok || got.Kind != pairObligationEmergencyClose {
			t.Fatalf("an operator reset must keep the research result incomplete, got %+v %v", got, ok)
		}
	})
	t.Run("the member circuit breaker trips after an unbroken outage", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		x.runEvery(t0, t0.Add(limit-2*time.Minute))
		if _, hl := x.read(); hl.CircuitBreaker {
			t.Fatalf("inside the deferral the member breaker must not trip: %+v (%v)", hl, x.reasons)
		}
		tf := t0.Add(limit + time.Second)
		x.e.now = tf
		x.h.prepare(basketCycleInputs{Now: tf, Once: true, PairSnapshotFor: x.e.sealFor, PairGeneration: x.e.feedGeneration})
		x.e.mu.Lock()
		sc := x.e.bc.members[pairTestHL]
		hlState := x.e.state.Strategies[pairTestHL]
		ok, why := checkStrategyRiskWithView(&sc, hlState, 0, nil, x.e.bc.pairMarksLocked(), nil, nil)
		hl, held := hlState.RiskState, hlState.Positions["BTC"]
		x.e.mu.Unlock()
		if ok || !hl.CircuitBreaker || !strings.HasPrefix(why, RiskReasonPairValuationStale) || !strings.Contains(why, pairTestHL+" has had no checked mark since") || !isFreshPerStrategyCircuitBreaker(why) {
			t.Fatalf("an outage longer than the deferral must trip the member breaker with a missing-mark reason, got %v %+v %q", ok, hl, why)
		}
		if held == nil || !basketQtyEqual(held.Quantity, 0.06) {
			t.Fatalf("the missing-mark breaker must book no close without a checked price, got %+v", held)
		}
	})
	t.Run("a restart with an outage values the member at average cost and escalates", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		base, _ := x.read()
		x.outage()
		r0 := tPre.Add(time.Minute)
		x.e.now = r0
		x.e.restart()
		x.h.run(r0)
		x.e.mu.RLock()
		hl := x.e.state.Strategies[pairTestHL]
		v, st := portfolioValueMarked(hl, nil, x.e.bc.pairMarksLocked())
		cash := hl.Cash
		x.e.mu.RUnlock()
		if st.Members[pairTestHL] != pairMarkUnavailable || !pairViewNear(v, cash) {
			t.Fatalf("after a restart with no view the member must be valued at average cost, got %v cash %v (%s)", v, cash, st.label())
		}
		if got := x.lostAt(); got != r0.UnixMilli() {
			t.Fatalf("the incomplete timer must start at the first preparation after the restart, got %d want %d", got, r0.UnixMilli())
		}
		x.runEvery(r0.Add(time.Minute), r0.Add(limit-time.Minute))
		if held, _ := x.read(); held.KillSwitchActive || held.PeakValue != base.PeakValue {
			t.Fatalf("inside the deferral the restarted reading must hold without a latch: %+v", held)
		}
		x.h.run(r0.Add(limit + time.Second))
		if latched, _ := x.read(); !latched.KillSwitchActive {
			t.Fatalf("a restarted outage longer than the deferral must latch, got %+v", latched)
		}
	})
	t.Run("a valid mark resets the timer", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		lag := func(ms int64) {
			x.hlSeal = ""
			if ms > 0 {
				x.hlSeal = "stale"
			}
		}
		s0 := tPre.Add(time.Minute)
		lag(6000)
		x.h.run(s0)
		if x.lostAt() == 0 {
			t.Fatal("setup: a reference older than quote_age_ms must start the incomplete timer")
		}
		lag(0)
		x.h.run(s0.Add(time.Minute))
		if got := x.lostAt(); got != 0 {
			t.Fatalf("a valid mark must clear the incomplete timer, got %d", got)
		}
		lag(6000)
		x.h.run(s0.Add(limit + 30*time.Second))
		if got := x.lostAt(); got < s0.Add(time.Minute).UnixMilli() {
			t.Fatalf("a new stale reading must start a new timer after the valid mark, got %d", got)
		}
		lag(0)
		x.h.run(s0.Add(limit + time.Minute))
		prs, hl := x.read()
		if prs.KillSwitchActive || hl.CircuitBreaker {
			t.Fatalf("a stale reading followed by a valid mark must never latch or trip: %+v %+v (%v)", prs, hl, x.reasons)
		}
	})
}

func (x *pairEscalationEnv) savedSince() time.Time {
	x.e.mu.RLock()
	defer x.e.mu.RUnlock()
	if prs := x.e.state.partitionRiskIfPresent(x.e.rt().Partition); prs != nil {
		return prs.PairValuationIncompleteSince
	}
	return time.Time{}
}

func (x *pairEscalationEnv) restartAt(at time.Time) {
	x.e.now = at
	x.e.restart()
}

func TestBasketMemberViewIncompleteValuationTimerSurvivesRestarts(t *testing.T) {
	limit := untrustedEquityLatchDeferral
	t.Run("restarts inside the deferral do not move the start", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		x.runEvery(t0, t0.Add(4*time.Minute))
		start := x.savedSince()
		if start.IsZero() || start.After(t0) {
			t.Fatalf("the outage start must be saved with the partition risk, got %v (t0 %v)", start, t0)
		}
		x.restartAt(t0.Add(5 * time.Minute))
		x.runEvery(t0.Add(5*time.Minute), t0.Add(9*time.Minute))
		x.restartAt(t0.Add(10 * time.Minute))
		x.runEvery(t0.Add(10*time.Minute), t0.Add(13*time.Minute))
		if got := x.savedSince(); !got.Equal(start) {
			t.Fatalf("a restart must keep the saved start %v, got %v", start, got)
		}
		if held, _ := x.read(); held.KillSwitchActive {
			t.Fatalf("inside the deferral the reading must not latch: %+v", held)
		}
		x.h.run(t0.Add(limit + time.Second))
		latched, _ := x.read()
		if !latched.KillSwitchActive {
			t.Fatalf("an outage longer than the deferral across two restarts must latch, got %+v", latched)
		}
		if last := latched.Events[len(latched.Events)-1]; last.Source != "pair_valuation" || !strings.Contains(last.Details, pairTestHL+" has had no checked mark since") {
			t.Fatalf("the latch must come from the pair valuation timer: %+v", last)
		}
	})
	t.Run("a valid mark after a restart clears the saved start", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		x.runEvery(t0, t0.Add(4*time.Minute))
		x.restartAt(t0.Add(5 * time.Minute))
		x.runEvery(t0.Add(5*time.Minute), t0.Add(6*time.Minute))
		if x.savedSince().IsZero() {
			t.Fatal("setup: the restarted outage must keep a saved start")
		}
		x.hlSeal = ""
		x.h.run(t0.Add(7 * time.Minute))
		if got := x.savedSince(); !got.IsZero() {
			t.Fatalf("a valid mark must clear the saved start, got %v", got)
		}
		x.runEvery(t0.Add(8*time.Minute), t0.Add(limit+time.Minute))
		prs, hl := x.read()
		if prs.KillSwitchActive || hl.CircuitBreaker {
			t.Fatalf("a cleared outage must never latch or trip: %+v %+v (%v)", prs, hl, x.reasons)
		}
	})
	t.Run("a saved start left by a lost save clears on the first valid mark", func(t *testing.T) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		x.runEvery(t0, t0.Add(4*time.Minute))
		if x.savedSince().IsZero() {
			t.Fatal("setup: the outage start must be saved")
		}
		x.hlSeal = ""
		x.restartAt(t0.Add(limit + time.Minute))
		if x.savedSince().IsZero() {
			t.Fatal("setup: the restart must load the old saved start")
		}
		x.h.nextManage = t0.Add(limit + time.Minute)
		x.h.run(t0.Add(limit + time.Minute))
		prs, hl := x.read()
		if prs.KillSwitchActive || hl.CircuitBreaker || !prs.PairValuationIncompleteSince.IsZero() {
			t.Fatalf("a valid first preparation must not latch or trip and must clear the saved start: %+v %+v (%v)", prs, hl, x.reasons)
		}
		x.restartAt(t0.Add(limit + 2*time.Minute))
		if got := x.savedSince(); !got.IsZero() {
			t.Fatalf("the cleared start must be saved, got %v", got)
		}
	})
}

func TestBasketMemberViewPreparationNamesMissingHostSealInputs(t *testing.T) {
	idle := newBasketCoordinator(&Config{}, NewAppState(), nil, &sync.RWMutex{}, nil, nil, map[string]*basketRuntime{})
	if out := captureORCStdout(t, func() { idle.preparePairRisk(basketCycleInputs{Now: pairTestStart()}) }); out != "" {
		t.Fatalf("a coordinator with no pair must log nothing, got %q", out)
	}

	e := newPairTestEnv(t, nil)
	pairViewMatched(t, e)
	at := pairSlotAt(pairTestStart(), 2)
	e.now = at
	e.prepare(at)
	e.mu.RLock()
	healthy := e.bc.pairViewsLocked(at)[pairTestHL]
	e.mu.RUnlock()
	if !healthy.MarkValid {
		t.Fatalf("a host with seal inputs must publish a valid sealed mark: %s", healthy.MissingReason)
	}
	bare := func(when time.Time) string {
		return captureORCStdout(t, func() { e.bc.preparePairRisk(basketCycleInputs{Now: when, Once: true}) })
	}
	first := bare(at.Add(time.Second))
	second := bare(at.Add(2 * time.Second))
	if strings.Count(first, "[CRITICAL]") != 1 || !strings.Contains(first, "without PairSnapshotFor and PairGeneration") || !strings.Contains(first, pairTestHL) {
		t.Fatalf("a host without seal inputs must raise one CRITICAL that names them, got %q", first)
	}
	if strings.Contains(second, "PairSnapshotFor") {
		t.Fatalf("the missing host input must be reported once, got %q", second)
	}
	e.mu.RLock()
	missing := e.bc.pairViewsLocked(at.Add(2 * time.Second))[pairTestHL]
	e.mu.RUnlock()
	if missing.MarkValid || !strings.Contains(missing.MissingReason, "PairSnapshotFor") {
		t.Fatalf("the sealed member must hold with the missing host input as its reason: %+v", missing.MissingReason)
	}
	if out := captureORCStdout(t, func() { e.prepare(at.Add(3 * time.Second)) }); strings.Contains(out, "[CRITICAL]") {
		t.Fatalf("a host that passes its seal inputs again must raise nothing, got %q", out)
	}
}

func TestBasketMemberViewBreakerUsesEachMemberOwnStart(t *testing.T) {
	limit := untrustedEquityLatchDeferral
	okxStale := func(x *pairEscalationEnv, on bool) {
		x.e.quotes.mu.Lock()
		defer x.e.quotes.mu.Unlock()
		x.e.quotes.receiptLagMs = 0
		if on {
			x.e.quotes.receiptLagMs = 6000
		}
	}
	check := func(x *pairEscalationEnv, at time.Time) (map[string]string, map[string]bool) {
		x.e.now = at
		x.h.prepare(basketCycleInputs{Now: at, Once: true, PairSnapshotFor: x.e.sealFor, PairGeneration: x.e.feedGeneration})
		x.e.mu.Lock()
		defer x.e.mu.Unlock()
		pm := x.e.bc.pairMarksLocked()
		reasons, tripped := map[string]string{}, map[string]bool{}
		for _, id := range []string{pairTestHL, pairTestOKX} {
			sc := x.e.bc.members[id]
			s := x.e.state.Strategies[id]
			if s.Positions["BTC"] == nil {
				panic("setup: both members must hold a position")
			}
			_, reasons[id] = checkStrategyRiskWithView(&sc, s, 0, nil, pm, nil, nil)
			tripped[id] = s.RiskState.CircuitBreaker
		}
		return reasons, tripped
	}
	run := func(t *testing.T, restartAt time.Duration, final time.Duration) {
		x := newPairEscalationEnv(t, 50)
		tPre := pairSlotAt(pairTestStart(), 2)
		x.h.run(tPre)
		t0 := tPre.Add(time.Minute)
		x.outage()
		if restartAt > 0 {
			x.runEvery(t0, t0.Add(restartAt-time.Minute))
			x.restartAt(t0.Add(restartAt))
			x.runEvery(t0.Add(restartAt), t0.Add(limit-2*time.Minute))
		} else {
			x.runEvery(t0, t0.Add(limit-2*time.Minute))
		}
		okxStale(x, true)
		reasons, tripped := check(x, t0.Add(final))
		if tripped[pairTestOKX] || strings.HasPrefix(reasons[pairTestOKX], RiskReasonPairValuationStale) {
			t.Fatalf("one stale okx preparation must not trip its breaker from the other member's start, got %q", reasons[pairTestOKX])
		}
		if !strings.Contains(reasons[pairTestOKX], "pair valuation incomplete: "+pairTestOKX+" ") {
			t.Fatalf("setup: the okx preparation must be stale, got %q", reasons[pairTestOKX])
		}
		if !tripped[pairTestHL] || !strings.HasPrefix(reasons[pairTestHL], RiskReasonPairValuationStale) {
			t.Fatalf("the hyperliquid breaker must trip after its own outage passes the deferral, got %q", reasons[pairTestHL])
		}
		x.h.run(t0.Add(final))
		if latched, _ := x.read(); !latched.KillSwitchActive {
			t.Fatalf("the partition must latch from its continuous incomplete start, got %+v", latched)
		}
	}
	t.Run("one stale okx preparation after a long hyperliquid outage", func(t *testing.T) {
		run(t, 0, 16*time.Minute)
	})
	t.Run("the same after a restart inside the outage", func(t *testing.T) {
		run(t, 10*time.Minute, limit+time.Second)
	})
}
