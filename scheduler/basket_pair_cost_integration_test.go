package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pairCostRates = pairAll(
	pairSetCost(0, "taker_fee_pct", 0.0003),
	pairSetCost(0, "slippage_pct", 0.001),
	pairSetCost(1, "taker_fee_pct", 0.0007),
	pairSetCost(1, "slippage_pct", 0.002),
)

type pairSavedTrade struct {
	Version  int
	PolicyID string
	Type     string
	Price    float64
	Qty      float64
	Fee      float64
	IsClose  bool
}

func pairCostLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: io.Discard}
}

func (e *pairTestEnv) savedTrades(id string) []pairSavedTrade {
	e.t.Helper()
	sid, err := e.db().toStorageID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	rows, err := e.db().db.Query(`SELECT cost_model_version, cost_policy_id, trade_type, price, quantity, exchange_fee, is_close FROM trades WHERE strategy_id = ? ORDER BY rowid`, sid)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []pairSavedTrade
	for rows.Next() {
		var tr pairSavedTrade
		var isClose int
		if err := rows.Scan(&tr.Version, &tr.PolicyID, &tr.Type, &tr.Price, &tr.Qty, &tr.Fee, &isClose); err != nil {
			e.t.Fatal(err)
		}
		tr.IsClose = isClose == 1
		out = append(out, tr)
	}
	return out
}

func (e *pairTestEnv) savedProgress() string {
	e.t.Helper()
	var raw string
	if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
		e.t.Fatal(err)
	}
	return raw
}

func (e *pairTestEnv) binding(id string) fillCostPolicy {
	e.t.Helper()
	p, ok := pairCostBindings(e.adm)[id]
	if !ok {
		e.t.Fatalf("no durable cost binding for %s", id)
	}
	return p
}

func pairCostNear(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

func TestBasketPairCostBindingsAreCanonicalPerMemberAndPerAdmission(t *testing.T) {
	e := newPairTestEnv(t, nil)
	if pairCostBindings(e.adm) != nil {
		t.Fatal("bindings must stay unpublished until the run saves them")
	}
	if _, err := fillCostPolicyForStrategy(e.adm, pairTestHL); err == nil {
		t.Fatal("a member policy must not resolve before the bindings are durable")
	}
	if m, ok := e.adm.ExecutionModel(pairTestHL); !ok || m != pairExecutionVirtualIOCAdverseMax {
		t.Fatalf("the research transport must freeze %s, got %q %v", pairExecutionVirtualIOCAdverseMax, m, ok)
	}
	e.slotStep(0)
	b := pairCostBindings(e.adm)
	if len(b) != 2 {
		t.Fatalf("want one binding per member, got %+v", b)
	}
	fp := e.rt().Contract.Fingerprint()
	for _, id := range []string{pairTestHL, pairTestOKX} {
		provider, account, instrument := entryProviderHyperliquid, "synthetic:hl-research", "BTC"
		if id == pairTestOKX {
			provider, account, instrument = entryProviderOKX, "synthetic:okx-research", "BTC-USDT-SWAP"
		}
		canonical := fmt.Sprintf(`{"contract_fingerprint":"%s","cost_policy_rates":{"fill_role":"taker_all","slippage_pct":"0.0005","spread":"inside_slippage","taker_fee_pct":"0.00045"},"execution_model":"virtual_ioc_adverse_max_v1","member_key":{"account_key":"%s","instrument":"%s","member_id":"%s","partition":"paper","provider":"%s"},"v":2}`, fp, account, instrument, id, provider)
		p := b[id]
		if p.ModelVersion != FillCostModelReplayCustomVersion || p.PolicyID != "pair-cost-"+sha256Hex([]byte(canonical)) {
			t.Fatalf("%s binding %+v does not hash the documented canonical bytes %s", id, p, canonical)
		}
		if p.TakerFeePct != 0.00045 || p.SlippagePct != 0.0005 || p.FillRole != fillCostRoleTakerAll || p.Spread != fillCostSpreadInSlippage {
			t.Fatalf("%s binding rates %+v are not the contract rates", id, p)
		}
	}
	if b[pairTestHL].PolicyID == b[pairTestOKX].PolicyID {
		t.Fatal("equal rates on different members must have different policy ids")
	}
	if strings.Contains(e.rt().Contract.JSON(), "pair-cost-") || strings.Contains(e.rt().Contract.JSON(), `"model_version":1001`) {
		t.Fatalf("the contract bytes must stay independent of the resolved ids: %s", e.rt().Contract.JSON())
	}
	rebuilt, err := buildPairContract(e.cfg, e.adm.pair, e.adm.limits)
	if err != nil || rebuilt.Fingerprint() != fp {
		t.Fatalf("the contract fingerprint changed after binding: %v", err)
	}
	b[pairTestHL] = fillCostPolicy{}
	if again := pairCostBindings(e.adm); again[pairTestHL].PolicyID == "" {
		t.Fatal("changing a copied map changed the admission's binding")
	}
	if p, err := fillCostPolicyForStrategy(nil, pairTestHL); err != nil || p != activeFillCostPolicy() {
		t.Fatalf("a nil owner must keep the legacy selection: %+v %v", p, err)
	}

	other := newPairTestEnv(t, pairSetCost(1, "taker_fee_pct", 0.0006))
	other.slotStep(0)
	ob := pairCostBindings(other.adm)
	if ob[pairTestOKX].TakerFeePct != 0.0006 || ob[pairTestHL].PolicyID == e.binding(pairTestHL).PolicyID {
		t.Fatalf("another admission with reused member ids must bind its own contract: %+v", ob)
	}
	for _, env := range []*pairTestEnv{e, other} {
		got, err := strategyFillCostPolicy(env.state.Strategies[pairTestOKX])
		if err != nil || got != env.binding(pairTestOKX) {
			t.Fatalf("a member book must resolve its own admission's binding: %+v %v", got, err)
		}
	}
	other.adm.release()
	if pairCostBindings(other.adm) != nil {
		t.Fatal("a released admission must publish no bindings")
	}
	if _, err := strategyFillCostPolicy(other.state.Strategies[pairTestOKX]); err == nil {
		t.Fatal("a released admission must refuse its members' costs")
	}
	if got, err := strategyFillCostPolicy(e.state.Strategies[pairTestOKX]); err != nil || got != e.binding(pairTestOKX) {
		t.Fatalf("releasing another admission changed this admission's costs: %+v %v", got, err)
	}

	want := e.binding(pairTestHL)
	e.store.Close()
	fresh, cfg, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, pairTestDeps(t, e.doc, e.venues))
	if err != nil {
		t.Fatalf("a resumed admission: %v", err)
	}
	t.Cleanup(fresh.release)
	e.adm.release()
	e.adm, e.cfg = fresh, cfg
	e.store = openSourceStore(t, cfg)
	loaded, _, err := LoadStateWithStore(cfg, e.store)
	if err != nil {
		t.Fatal(err)
	}
	e.state = loaded
	e.reopen(true)
	if got := e.binding(pairTestHL); got != want {
		t.Fatalf("a fresh admission rebuilt %+v, the saved run bound %+v", got, want)
	}
}

func TestBasketPairCostEvidenceCommitsBeforeAnyBooking(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	cash := e.state.Strategies[pairTestOKX].Cash
	f := &pairCommitFault{from: 1}
	f.install(t)
	e.step(pairSlotAt(pairTestStart(), 0).Add(time.Second))
	if pairCostBindings(e.adm) != nil || e.adm.costDurable() {
		t.Fatal("a failed evidence commit must keep cost readiness false")
	}
	if n := e.count(`SELECT COUNT(*) FROM basket_contracts`); n != 0 {
		t.Fatalf("the failed commit left %d contract rows", n)
	}
	if len(e.rt().Actions) != 0 || e.signed(pairTestOKX) != 0 || e.state.Strategies[pairTestOKX].Cash != cash {
		t.Fatalf("no pair work may run before the bindings are durable: %d actions", len(e.rt().Actions))
	}
	if _, err := strategyFillCostPolicy(e.state.Strategies[pairTestOKX]); err == nil {
		t.Fatal("a native owner must not resolve a member policy before the bindings are durable")
	}
	f.to = f.n
	e.slotStep(1)
	e.assertBooks("first leg after the commit", 0, -0.06)
	var prog struct {
		CostEvidence json.RawMessage `json:"cost_evidence"`
	}
	if err := json.Unmarshal([]byte(e.savedProgress()), &prog); err != nil {
		t.Fatal(err)
	}
	want, _ := e.adm.costEvidence()
	got, _, err := pairCostEvidenceCanonical(prog.CostEvidence)
	if err != nil || string(got) != string(want) {
		t.Fatalf("the saved cost evidence is not the canonical binding set: %s (%v)", prog.CostEvidence, err)
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		if !strings.Contains(string(want), e.binding(id).PolicyID) {
			t.Fatalf("the saved evidence does not carry %s", e.binding(id).PolicyID)
		}
	}
}

func TestBasketPairCostResumeRefusesMissingOrChangedEvidence(t *testing.T) {
	tamper := map[string]func(ev map[string]interface{}) interface{}{
		"missing": func(map[string]interface{}) interface{} { return nil },
		"policy id": func(ev map[string]interface{}) interface{} {
			ev["members"].([]interface{})[0].(map[string]interface{})["policy_id"] = "pair-cost-00"
			return ev
		},
		"execution model": func(ev map[string]interface{}) interface{} {
			m := ev["members"].([]interface{})[1].(map[string]interface{})
			m["binding"].(map[string]interface{})["execution_model"] = pairExecutionVenueConfirmed
			return ev
		},
		"rate": func(ev map[string]interface{}) interface{} {
			m := ev["members"].([]interface{})[0].(map[string]interface{})
			m["binding"].(map[string]interface{})["cost_policy_rates"].(map[string]interface{})["taker_fee_pct"] = "0.0004"
			return ev
		},
		"extra member": func(ev map[string]interface{}) interface{} {
			ms := ev["members"].([]interface{})
			ev["members"] = append(ms, ms[0])
			return ev
		},
		"extra field": func(ev map[string]interface{}) interface{} {
			ev["note"] = "x"
			return ev
		},
		"model version": func(ev map[string]interface{}) interface{} {
			ev["members"].([]interface{})[0].(map[string]interface{})["model_version"] = 1
			return ev
		},
		"malformed": func(map[string]interface{}) interface{} { return "not an object" },
	}
	for _, matched := range []bool{false, true} {
		t.Run(fmt.Sprintf("matched_%v", matched), func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterHoldRunner(e, 0.06)
			if matched {
				e.slotStep(0)
				e.slotStep(1)
				e.assertBooks("matched", 0.06, -0.06)
			} else {
				e.step(pairTestStart())
				if len(e.rt().Actions) != 0 || len(e.savedTrades(pairTestHL)) != 0 {
					t.Fatal("setup: the zero-trade run booked something")
				}
			}
			original := e.savedProgress()
			e.store.Close()
			db, err := OpenStateDB(e.cfg.DBFile)
			if err != nil {
				t.Fatal(err)
			}
			write := func(progress string) {
				if _, err := db.db.Exec(`UPDATE basket_contracts SET progress_json = ? WHERE basket_id = ?`, progress, pairTestID); err != nil {
					t.Fatal(err)
				}
			}
			for name, fn := range tamper {
				var root map[string]interface{}
				if err := json.Unmarshal([]byte(original), &root); err != nil {
					t.Fatal(err)
				}
				var ev map[string]interface{}
				raw, _ := json.Marshal(root["cost_evidence"])
				if err := json.Unmarshal(raw, &ev); err != nil {
					t.Fatal(err)
				}
				if next := fn(ev); next == nil {
					delete(root, "cost_evidence")
				} else {
					root["cost_evidence"] = next
				}
				changed, _ := json.Marshal(root)
				write(string(changed))
				if err := pairLoadBaskets(t, e.cfg); err == nil || !strings.Contains(err.Error(), "resume refused") {
					t.Fatalf("%s: a resume with changed cost evidence must be refused, got %v", name, err)
				}
				var after string
				if err := db.db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&after); err != nil || after != string(changed) {
					t.Fatalf("%s: the refused resume changed the saved progress: %v", name, err)
				}
				fresh, freshCfg, ferr := newPairResearchAdmission(e.doc, "run-1", e.dir, true, pairTestDeps(t, e.doc, e.venues))
				if ferr != nil {
					t.Fatal(ferr)
				}
				err := pairLoadBaskets(t, freshCfg)
				fresh.release()
				if err == nil {
					t.Fatalf("%s: a fresh resumed admission accepted changed cost evidence", name)
				}
			}
			write(original)
			db.Close()
			if err := pairLoadBaskets(t, e.cfg); err != nil {
				t.Fatalf("the unchanged evidence must resume: %v", err)
			}
		})
	}
}

func TestBasketPairCostRowsCarryTheMemberStampAndRates(t *testing.T) {
	for _, hlQty := range []float64{0.06, -0.06} {
		t.Run(fmt.Sprintf("hl_%v", hlQty), func(t *testing.T) {
			e := newPairTestEnv(t, pairCostRates)
			pairEnterExitRunner(e, hlQty)
			e.slotStep(0)
			e.slotStep(1)
			e.assertBooks("matched", hlQty, -hlQty)
			e.slotStep(2)
			e.assertBooks("exited", 0, 0)
			hlOpen, okxOpen, hlExit, okxExit := 60400.0, 59700.0, 59500.0, 60600.0
			if hlQty < 0 {
				hlOpen, okxOpen, hlExit, okxExit = 59600.0, 60500.0, 60600.0, 59500.0
			}
			for _, c := range []struct {
				id        string
				taker     float64
				open, out float64
			}{{pairTestHL, 0.0003, hlOpen, hlExit}, {pairTestOKX, 0.0007, okxOpen, okxExit}} {
				want := e.binding(c.id)
				rows := e.savedTrades(c.id)
				if len(rows) != 2 {
					t.Fatalf("%s: want an open and a close row, got %+v", c.id, rows)
				}
				for _, r := range rows {
					if r.Version != FillCostModelReplayCustomVersion || r.PolicyID != want.PolicyID {
						t.Fatalf("%s row %+v does not carry the member stamp %s", c.id, r, want.PolicyID)
					}
					px := c.open
					if r.IsClose {
						px = c.out
					}
					if r.Price != px {
						t.Fatalf("%s row %+v must book at the IOC price %v with no second slippage", c.id, r, px)
					}
					if !pairCostNear(r.Fee, r.Qty*r.Price*c.taker) {
						t.Fatalf("%s row %+v must charge the member taker rate %v once, fee want %v", c.id, r, c.taker, r.Qty*r.Price*c.taker)
					}
				}
			}
		})
	}

	e := newPairTestEnv(t, pairCostRates)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.02*59800 + 0.04*59750, Terminal: true})
	e.step(pairSlotAt(pairTestStart(), 1))
	e.assertBooks("added", 0, -0.06)
	rows := e.savedTrades(pairTestOKX)
	if len(rows) != 2 || rows[1].Type != scaleInTradeType {
		t.Fatalf("the second confirmed delta must book as an add: %+v", rows)
	}
	for _, r := range rows {
		if r.PolicyID != e.binding(pairTestOKX).PolicyID || !pairCostNear(r.Fee, r.Qty*r.Price*0.0007) {
			t.Fatalf("the add row %+v must carry the member stamp and taker rate", r)
		}
	}
}

func TestBasketPairCostOrdinaryCallersKeepTheirCosts(t *testing.T) {
	logger := pairCostLogger("plain")
	for _, c := range []struct {
		platform string
		fee      float64
	}{{"okx", OKXPerpsTakerFeePct}, {"hyperliquid", HyperliquidTakerFeePct}} {
		s := NewStrategyState(StrategyConfig{ID: "plain-" + c.platform, Platform: c.platform, Type: "perps", Capital: 1000})
		if _, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["BTC"]
		px := ApplyAdverseSlippage(60000, true)
		if pos == nil || pos.AvgCost != px {
			t.Fatalf("%s: an ordinary open must keep the process slippage: %+v", c.platform, pos)
		}
		if _, err := ExecutePerpsSignalWithLeverage(s, -1, "BTC", 61000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		for _, tr := range s.TradeHistory {
			if tr.CostModelVersion != FillCostModelVersion || tr.CostPolicyID != "" {
				t.Fatalf("%s: an ordinary row %+v must keep the process stamp", c.platform, tr)
			}
			if !pairCostNear(tr.ExchangeFee, tr.Quantity*tr.Price*c.fee) {
				t.Fatalf("%s: an ordinary row %+v must keep the venue fee rate %v", c.platform, tr, c.fee)
			}
		}
		if len(s.pairCostFaults) != 0 {
			t.Fatalf("an ordinary strategy raised a pair cost fault: %v", s.pairCostFaults)
		}
	}
}

func TestBasketPairCostTransportIdentityIsFrozen(t *testing.T) {
	registerPairTestPolicy(t)
	for name, change := range map[string]func(v *pairTestVenues){
		"unknown model":         func(v *pairTestVenues) { v.hl.model = "mid_fill_v0" },
		"live model on virtual": func(v *pairTestVenues) { v.okx.model = pairExecutionVenueConfirmed },
		"account":               func(v *pairTestVenues) { v.okx.account = "synthetic:other" },
		"provider":              func(v *pairTestVenues) { v.hl.provider = entryProviderOKX },
	} {
		dir := filepath.Join(t.TempDir(), "run")
		doc := pairTestDoc(t, dir, nil)
		venues := newPairTestVenues(false)
		change(venues)
		if adm, _, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, venues)); err == nil {
			adm.release()
			t.Fatalf("%s: the admission accepted a transport that does not match its frozen identity", name)
		}
		if pairFileDigest(t, filepath.Join(dir, pairResearchManifestName)) != "missing" {
			t.Fatalf("%s: a refused admission wrote state", name)
		}
	}

	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.venues.mu.Lock()
	e.venues.okx.model = pairExecutionVenueConfirmed
	e.venues.mu.Unlock()
	e.slotStep(0)
	if len(e.rt().Actions) != 0 || len(e.venues.okx.submits) != 0 {
		t.Fatalf("a changed transport model must refuse dispatch: %d actions, %d submits", len(e.rt().Actions), len(e.venues.okx.submits))
	}
	e.venues.mu.Lock()
	e.venues.okx.model = ""
	e.venues.okx.account = "synthetic:moved"
	e.venues.mu.Unlock()
	e.slotStep(1)
	if len(e.rt().Actions) != 0 || len(e.venues.okx.submits) != 0 {
		t.Fatal("a changed transport account must refuse dispatch")
	}
	e.venues.mu.Lock()
	e.venues.okx.account = "synthetic:okx-research"
	e.venues.mu.Unlock()
	e.slotStep(2)
	e.assertBooks("restored transport", 0, -0.06)
}

func (e *pairTestEnv) savedCostEvidence() string {
	e.t.Helper()
	var prog struct {
		CostEvidence json.RawMessage `json:"cost_evidence"`
	}
	if err := json.Unmarshal([]byte(e.savedProgress()), &prog); err != nil {
		e.t.Fatal(err)
	}
	if len(prog.CostEvidence) == 0 {
		e.t.Fatal("the saved pair progress has no cost evidence")
	}
	return string(prog.CostEvidence)
}

func (e *pairTestEnv) assertFlatResetKeepsCostEvidence(label, want string) {
	e.t.Helper()
	if e.lifecycle() != pairLifecycleFlat {
		e.t.Fatalf("%s: the pair lifecycle is %s, want flat after the reset", label, e.lifecycle())
	}
	saved, err := parsePairProgress(e.savedProgress())
	if err != nil || saved.Lifecycle != pairLifecycleFlat {
		e.t.Fatalf("%s: the reset write is not durable: lifecycle %q (%v)", label, saved.Lifecycle, err)
	}
	if got := e.savedCostEvidence(); got != want {
		e.t.Fatalf("%s: the saved cost evidence changed across the reset:\n got %s\nwant %s", label, got, want)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		e.t.Fatalf("%s: SavePartition after the reset: %v", label, err)
	}
	if err := e.store.PersistBasketProgress(e.rt()); err != nil {
		e.t.Fatalf("%s: PersistBasketProgress after the reset: %v", label, err)
	}
	if got := e.savedCostEvidence(); got != want {
		e.t.Fatalf("%s: the saved cost evidence changed after the post-reset saves:\n got %s\nwant %s", label, got, want)
	}
}

func TestBasketPairCostEvidenceSurvivesTheFlatReset(t *testing.T) {
	t.Run("elective exit then a second entry", func(t *testing.T) {
		e := newPairTestEnv(t, pairCostRates)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("hedged", 0.06, -0.06)
		want := e.savedCostEvidence()
		hl, okx := e.binding(pairTestHL), e.binding(pairTestOKX)
		e.slotStep(2)
		e.assertBooks("exited", 0, 0)
		e.step(pairSlotAt(pairTestStart(), 2).Add(30 * time.Second))
		e.assertFlatResetKeepsCostEvidence("elective exit", want)
		okxRows := len(e.savedTrades(pairTestOKX))
		e.slotStep(3)
		e.assertBooks("second entry first leg", 0, -0.06)
		rows := e.savedTrades(pairTestOKX)
		if len(rows) != okxRows+1 {
			t.Fatalf("the second entry saved %d okx rows, want %d", len(rows), okxRows+1)
		}
		for id, want := range map[string]fillCostPolicy{pairTestHL: hl, pairTestOKX: okx} {
			for i, tr := range e.savedTrades(id) {
				if tr.Version != FillCostModelReplayCustomVersion || tr.PolicyID != want.PolicyID {
					t.Fatalf("%s row %d carries %d/%q, want the unchanged member binding %d/%q", id, i, tr.Version, tr.PolicyID, want.ModelVersion, want.PolicyID)
				}
			}
			if got := e.binding(id); got.PolicyID != want.PolicyID {
				t.Fatalf("%s binding changed across the reset: %q, want %q", id, got.PolicyID, want.PolicyID)
			}
		}
		if got := e.savedCostEvidence(); got != want {
			t.Fatalf("the second entry changed the saved cost evidence:\n got %s\nwant %s", got, want)
		}
	})
	t.Run("recovery reset then restart", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("matched", 0.06, -0.06)
		want := e.savedCostEvidence()
		e.externalBookChange(pairTestHL, 0)
		e.step(pairSlotAt(pairTestStart(), 2))
		if e.lifecycle() != pairLifecycleRecovery {
			t.Fatalf("a unilateral close must open recovery, got %s", e.lifecycle())
		}
		e.step(pairSlotAt(pairTestStart(), 3))
		e.assertBooks("survivor flat", 0, 0)
		e.step(pairSlotAt(pairTestStart(), 4))
		e.assertFlatResetKeepsCostEvidence("recovery", want)
		e.restart()
		if e.lifecycle() != pairLifecycleFlat {
			t.Fatalf("the restarted pair lifecycle is %s, want flat", e.lifecycle())
		}
		if pairCostBindings(e.adm) == nil {
			t.Fatal("the restarted pair resumed without durable member cost bindings")
		}
		if got := string(e.rt().Pair.Progress.CostEvidence); got != want {
			t.Fatalf("the restarted pair holds different cost evidence:\n got %s\nwant %s", got, want)
		}
		e.slotStep(5)
		e.assertBooks("entry after the restart", 0, -0.06)
		if got := e.savedCostEvidence(); got != want {
			t.Fatalf("the entry after the restart changed the saved cost evidence")
		}
	})
	t.Run("operator held reset", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		want := e.savedCostEvidence()
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		e.externalBookChange(pairTestHL, 0.01)
		e.step(pairSlotAt(pairTestStart(), 1))
		start := e.rt().Pair.Progress.RecoveryStartMs
		if e.lifecycle() != pairLifecycleRecovery || start == 0 {
			t.Fatalf("an unexplained book change must open recovery, got %s", e.lifecycle())
		}
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		at := time.UnixMilli(start + e.rt().Pair.Limits.RecoveryAgeMs).UTC()
		e.step(at)
		if e.lifecycle() != pairLifecycleOperatorHeld {
			t.Fatalf("at the recovery age the pair must be operator held, got %s", e.lifecycle())
		}
		for i := 1; i <= 6 && (e.lifecycle() != pairLifecycleFlat); i++ {
			e.step(at.Add(time.Duration(i) * time.Minute))
		}
		e.assertBooks("operator held flat", 0, 0)
		e.assertFlatResetKeepsCostEvidence("operator held", want)
	})
}
