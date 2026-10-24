//go:build pyintegration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type pairPolicyCapture struct {
	mu       sync.Mutex
	requests [][]byte
	outputs  [][]byte
	mutate   func(stdin []byte) []byte
	during   func()
}

func (c *pairPolicyCapture) run(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	c.mu.Lock()
	mutate, during := c.mutate, c.during
	c.mu.Unlock()
	if mutate != nil {
		stdin = mutate(stdin)
	}
	if during != nil {
		during()
	}
	out, errOut, err := runPairPolicyProcess(ctx, stdin, timeout)
	c.mu.Lock()
	c.requests = append(c.requests, append([]byte(nil), stdin...))
	c.outputs = append(c.outputs, append([]byte(nil), out...))
	c.mu.Unlock()
	return out, errOut, err
}

func (c *pairPolicyCapture) last(t *testing.T) ([]byte, []byte) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("the real policy process never ran")
	}
	return c.requests[len(c.requests)-1], c.outputs[len(c.outputs)-1]
}

type pairPolicyFixture struct {
	params   map[string]interface{}
	limits   map[string]interface{}
	cost     map[string]interface{}
	depth    int
	hlBids   [][2]string
	hlAsks   [][2]string
	okxBids  [][2]string
	okxAsks  [][2]string
	doc      func(root map[string]interface{})
	evidence func(d *pairEvidenceDocument)
}

func pairPolicyBaseParams() map[string]interface{} {
	return map[string]interface{}{"entry_threshold_bps": 5, "exit_threshold_bps": 1, "adverse_band_bps": 0, "funding_allowance_bps": 0}
}

func pairPolicyDoc(t *testing.T, dir string, f pairPolicyFixture) []byte {
	t.Helper()
	return pairTestDoc(t, dir, func(r map[string]interface{}) {
		p := pairTestPair(r)
		p["policy"] = pairPolicyCrossExchangePerpArbitrage
		params := pairPolicyBaseParams()
		for k, v := range f.params {
			params[k] = v
		}
		p["params"] = params
		if f.limits != nil {
			lim := pairTestLimits()
			for k, v := range f.limits {
				lim[k] = v
			}
			p["limits"] = lim
		}
		if f.depth > 0 {
			p["book_need"] = map[string]interface{}{"depth": f.depth, "window_ms": 60000, "capacity": 256}
		}
		if f.cost != nil {
			for i := 0; i < 2; i++ {
				c := pairTestCostPolicy()
				for k, v := range f.cost {
					c[k] = v
				}
				pairTestMemberEntry(r, i)["cost_policy"] = c
			}
		}
		if f.doc != nil {
			f.doc(r)
		}
	})
}

func newPairPolicyEnv(t *testing.T, f pairPolicyFixture) (*pairTestEnv, *pairPolicyCapture, string) {
	t.Helper()
	root := basketPyRoot(t)
	t.Chdir(root)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairPolicyDoc(t, dir, f)
	venues := newPairTestVenues(false)
	q := newPairFixtureQuotes(t, doc)
	for _, s := range []struct {
		dst *[][2]string
		src [][2]string
	}{{&q.hlBids, f.hlBids}, {&q.hlAsks, f.hlAsks}, {&q.okxBids, f.okxBids}, {&q.okxAsks, f.okxAsks}} {
		if s.src != nil {
			*s.dst = s.src
		}
	}
	deps := pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, f.evidence), Quotes: q.collector}
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, deps)
	if err != nil {
		t.Fatalf("newPairResearchAdmission: %v", err)
	}
	t.Cleanup(adm.release)
	e := &pairTestEnv{t: t, dir: dir, doc: doc, adm: adm, cfg: cfg, venues: venues, mu: &sync.RWMutex{}, now: pairTestStart(), quotes: q}
	e.runner = &pairTestRunner{respond: func(req pairPolicyRequest) (pairPolicyResult, error) { return pairHoldResult(req, "scripted"), nil }}
	e.store = openSourceStore(t, cfg)
	e.state = NewAppState()
	for _, sc := range cfg.Strategies {
		e.state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	e.reopen(false)
	c := &pairPolicyCapture{}
	e.bc.pairPolicyRun = c.run
	return e, c, root
}

func (e *pairTestEnv) setBooks(hlBids, hlAsks, okxBids, okxAsks [][2]string) {
	e.quotes.mu.Lock()
	defer e.quotes.mu.Unlock()
	e.quotes.hlBids, e.quotes.hlAsks, e.quotes.okxBids, e.quotes.okxAsks = hlBids, hlAsks, okxBids, okxAsks
}

func pairPolicyEvidence(rates map[string]string, free, equity map[string]string) func(d *pairEvidenceDocument) {
	return func(d *pairEvidenceDocument) {
		for i := range d.Conversions {
			if v, ok := rates[d.Conversions[i].Currency]; ok {
				d.Conversions[i].RateUSD = pairDecimal(v)
			}
		}
		for i := range d.Accounts {
			id := d.Accounts[i].Key.MemberID
			if v, ok := free[id]; ok {
				d.Accounts[i].FreeCollateral = pairDecimal(v)
			}
			if v, ok := equity[id]; ok {
				d.Accounts[i].Equity = pairDecimal(v)
			}
		}
	}
}

func pairPolicyStoredDecision(t *testing.T, e *pairTestEnv, at time.Time) (*pairTargets, map[string]interface{}) {
	t.Helper()
	var stored string
	if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, at.UnixMilli()).Scan(&stored); err != nil {
		t.Fatalf("read the saved decision: %v", err)
	}
	got, err := parsePairTargets(stored)
	if err != nil || got == nil {
		t.Fatalf("the saved decision %q is unreadable: %v", stored, err)
	}
	var d map[string]interface{}
	if len(got.Decision) > 0 {
		if err := json.Unmarshal(got.Decision, &d); err != nil {
			t.Fatalf("the saved decision evidence is unreadable: %v", err)
		}
	}
	return got, d
}

func pairPolicyDecide(t *testing.T, e *pairTestEnv, i int) (time.Time, *pairTargets, map[string]interface{}) {
	t.Helper()
	at := e.slotStep(i)
	got, d := pairPolicyStoredDecision(t, e, at)
	return at, got, d
}

func pairPolicyPython(t *testing.T, root string, stdin []byte, args ...string) ([]byte, []byte, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("python failed to start: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.Bytes(), errOut.Bytes(), code
}

const pairPolicyBruteScript = `import importlib.util, json, os, sys
root = sys.argv[1]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("go_trader_pair_policy_cross_exchange_perp_arbitrage", os.path.join(root, "shared_strategies", "open", "cross_exchange_perp_arbitrage.py"))
mod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mod
spec.loader.exec_module(mod)
req = mod.pair_evidence.loads_strict(sys.stdin.buffer.read())
decision = {}
params = mod._admit(req, decision)
env = mod._verify(req, decision)
ctx = mod._context(req, env, params, decision)
out = {}
for buy, sell in ((ctx["members"][0], ctx["members"][1]), (ctx["members"][1], ctx["members"][0])):
    grid = mod._lcm(buy["lot"], sell["lot"])
    depth = min(sum(q for _, q in buy["asks"]), sum(q for _, q in sell["bids"]))
    best, k, cache = None, 1, {}
    while k * grid <= depth:
        if mod.entry_candidate(ctx, buy, sell, k * grid, cache)["passed"]:
            best = k * grid
        k += 1
    out[mod.ORIENTATIONS[(buy["provider"], sell["provider"])]] = {"qty": None if best is None else mod._x(best), "checked": k - 1}
print(json.dumps(out, sort_keys=True))
`

func pairPolicyBrute(t *testing.T, root string, request []byte) map[string]map[string]interface{} {
	t.Helper()
	out, errOut, code := pairPolicyPython(t, root, request, "-c", pairPolicyBruteScript, root)
	if code != 0 {
		t.Fatalf("the exhaustive common-lot enumeration failed (%d): %s", code, errOut)
	}
	var res map[string]map[string]interface{}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("the exhaustive enumeration output is unreadable: %v (%s)", err, out)
	}
	return res
}

func pairPolicyOrientationReport(t *testing.T, d map[string]interface{}, orientation string) map[string]interface{} {
	t.Helper()
	reports, _ := d["orientations"].([]interface{})
	for _, r := range reports {
		m := r.(map[string]interface{})
		if m["orientation"] == orientation {
			return m
		}
	}
	t.Fatalf("the decision has no %s orientation report: %v", orientation, d)
	return nil
}

func pairPolicyHasString(list interface{}, want string) bool {
	items, _ := list.([]interface{})
	for _, it := range items {
		if s, _ := it.(string); s == want || strings.HasPrefix(s, want) {
			return true
		}
	}
	return false
}

func pairPolicyGoodBooks() pairPolicyFixture {
	return pairPolicyFixture{
		depth:   3,
		hlBids:  [][2]string{{"59999", "1"}, {"59998", "1"}, {"59997", "1"}},
		hlAsks:  [][2]string{{"60000", "1"}, {"60001", "1"}, {"60002", "1"}},
		okxBids: [][2]string{{"60300", "100"}, {"60299.9", "100"}, {"60299.8", "100"}},
		okxAsks: [][2]string{{"60300.1", "100"}, {"60300.2", "100"}, {"60300.3", "100"}},
	}
}

func pairPolicyWideLimits() map[string]interface{} {
	return map[string]interface{}{"gross_budget_usd": 1000000, "hyperliquid_notional_cap_usd": 1000000, "okx_notional_cap_usd": 1000000, "unmatched_cap_usd": 1000000}
}

var pairPolicyRichAccounts = pairPolicyEvidence(nil, map[string]string{pairTestHL: "1000000", pairTestOKX: "1000000"}, map[string]string{pairTestHL: "1000000", pairTestOKX: "1000000"})

func TestBasketPairPolicyRealEntrySizingMatchesExhaustiveSearch(t *testing.T) {
	cases := []struct {
		name        string
		fixture     func() pairPolicyFixture
		orientation string
		qty         string
		deepFails   string
	}{
		{name: "costs bind before depth", orientation: pairOrientationLongHLShortOKX, qty: "0.03", deepFails: "entry_threshold", fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits, f.evidence = pairPolicyWideLimits(), pairPolicyRichAccounts
			f.hlAsks = [][2]string{{"60000", "0.03"}, {"60150", "0.03"}, {"60300", "0.05"}}
			f.okxBids = [][2]string{{"60300", "3"}, {"60200", "3"}, {"60050", "5"}}
			return f
		}},
		{name: "gross budget binds", orientation: pairOrientationLongHLShortOKX, qty: "0.06", deepFails: "gross_budget", fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
			return f
		}},
		{name: "collateral binds", orientation: pairOrientationLongHLShortOKX, qty: "0.03", deepFails: "collateral:" + pairTestOKX, fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits = pairPolicyWideLimits()
			f.evidence = pairPolicyEvidence(nil, map[string]string{pairTestHL: "1000000", pairTestOKX: "1000"}, map[string]string{pairTestHL: "1000000", pairTestOKX: "1000000"})
			return f
		}},
		{name: "rounded bound cannot reach the deeper level", orientation: pairOrientationLongHLShortOKX, qty: "0.05", deepFails: "reach:" + pairTestHL, fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits, f.evidence = pairPolicyWideLimits(), pairPolicyRichAccounts
			f.cost = map[string]interface{}{"slippage_pct": 0}
			f.depth = 2
			f.hlAsks = [][2]string{{"60000", "0.05"}, {"60000.5", "0.5"}}
			return f
		}},
		{name: "non-parity conversions", orientation: pairOrientationLongHLShortOKX, qty: "0.06", deepFails: "gross_budget", fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
			f.evidence = pairPolicyEvidence(map[string]string{"USDC": "0.9995", "USDT": "1.0008"}, nil, nil)
			return f
		}},
		{name: "long okx short hyperliquid", orientation: pairOrientationLongOKXShortHL, qty: "0.06", deepFails: "gross_budget", fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
			f.hlBids = [][2]string{{"60300", "1"}, {"60299", "1"}, {"60298", "1"}}
			f.hlAsks = [][2]string{{"60301", "1"}, {"60302", "1"}, {"60303", "1"}}
			f.okxBids = [][2]string{{"59999.9", "100"}, {"59999.8", "100"}, {"59999.7", "100"}}
			f.okxAsks = [][2]string{{"60000", "100"}, {"60000.1", "100"}, {"60000.2", "100"}}
			return f
		}},
		{name: "two lot grids, contract size 0.001 and native minimums", orientation: pairOrientationLongHLShortOKX, qty: "0.06", deepFails: "gross_budget", fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.limits = map[string]interface{}{"gross_budget_usd": 9000, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
			f.doc = pairPolicyGridDoc
			f.hlAsks = [][2]string{{"60000", "0.05"}, {"60010", "0.07"}, {"60020", "0.2"}}
			f.hlBids = [][2]string{{"59999", "1"}, {"59998", "1"}, {"59997", "1"}}
			f.okxBids = [][2]string{{"60300", "90"}, {"60290", "90"}, {"60250", "300"}}
			f.okxAsks = [][2]string{{"60300.1", "300"}, {"60300.2", "300"}, {"60300.3", "300"}}
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, c, root := newPairPolicyEnv(t, tc.fixture())
			_, got, d := pairPolicyDecide(t, e, 0)
			if got.Kind != pairPolicyStatusEnter || got.Orientation != tc.orientation {
				t.Fatalf("decision %s %s (%s %s), want enter %s", got.Kind, got.Orientation, got.PolicyReason, got.CoordinatorReason, tc.orientation)
			}
			sel, _ := d["selected"].(map[string]interface{})
			if sel["qty"] != tc.qty {
				t.Fatalf("selected %v base, want %s: %v", sel["qty"], tc.qty, sel)
			}
			report := pairPolicyOrientationReport(t, d, tc.orientation)
			deep, _ := report["deepest"].(map[string]interface{})
			if deep == nil || deep["passed"] == true || !pairPolicyHasString(deep["failed"], tc.deepFails) || deep["qty"] == tc.qty {
				t.Fatalf("the deepest candidate must fail %s and differ from the selected size: %v", tc.deepFails, deep)
			}
			req, _ := c.last(t)
			brute := pairPolicyBrute(t, root, req)
			for _, o := range []string{pairOrientationLongHLShortOKX, pairOrientationLongOKXShortHL} {
				r := pairPolicyOrientationReport(t, d, o)
				if r["selected_qty"] != brute[o]["qty"] {
					t.Fatalf("%s: the bounded search selected %v, the exhaustive common-lot enumeration %v (%v candidates)", o, r["selected_qty"], brute[o]["qty"], brute[o]["checked"])
				}
			}
			for id, m := range got.Members {
				want := 0.0
				fmt.Sscanf(tc.qty, "%g", &want)
				if id == sel["buy_member"] && m.TargetBaseQty != want || id == sel["sell_member"] && m.TargetBaseQty != -want {
					t.Fatalf("member %s target %v does not carry the selected quantity %s", id, m.TargetBaseQty, tc.qty)
				}
			}
		})
	}
}

func pairPolicyGridDoc(root map[string]interface{}) {
	hl := pairTestMemberEntry(root, 0)["meta"].(map[string]interface{})
	hl["sz_decimals"], hl["lot_step"], hl["min_qty"], hl["tick_step"] = 2, 0.01, 0.01, 0.0001
	okx := pairTestMemberEntry(root, 1)["meta"].(map[string]interface{})
	okx["contract_size"], okx["lot_step"], okx["lot_step_text"], okx["min_qty"] = 0.001, 3, "3", 30
}

func pairPolicyFlatSpreadBooks() pairPolicyFixture {
	return pairPolicyFixture{
		depth:   3,
		cost:    map[string]interface{}{"slippage_pct": 0, "taker_fee_pct": 0},
		hlBids:  [][2]string{{"59999", "1"}, {"59998", "1"}, {"59997", "1"}},
		hlAsks:  [][2]string{{"60000", "1"}, {"60001", "1"}, {"60002", "1"}},
		okxBids: [][2]string{{"60060", "100"}, {"60059.9", "100"}, {"60059.8", "100"}},
		okxAsks: [][2]string{{"60060.1", "100"}, {"60060.2", "100"}, {"60060.3", "100"}},
	}
}

func TestBasketPairPolicyRealBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		fixture func() pairPolicyFixture
		kind    string
		qty     string
		fields  map[string]string
		holdHas string
		binding string
	}{
		{name: "entry threshold equality holds", kind: pairPolicyStatusHold, holdHas: "entry_threshold", fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.params = map[string]interface{}{"entry_threshold_bps": 10, "exit_threshold_bps": 1}
			return f
		}},
		{name: "entry threshold just below enters", kind: pairPolicyStatusEnter, qty: "0.09", fields: map[string]string{"capture_bps": "10", "buy_cost_usd": "5400", "sell_proceeds_usd": "5405.4"}, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.params = map[string]interface{}{"entry_threshold_bps": 9.99, "exit_threshold_bps": 1}
			return f
		}},
		{name: "funding reserve equality holds", kind: pairPolicyStatusHold, holdHas: "entry_threshold", fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.params = map[string]interface{}{"entry_threshold_bps": 6, "exit_threshold_bps": 1, "funding_allowance_bps": 4}
			return f
		}},
		{name: "funding reserve below the threshold enters", kind: pairPolicyStatusEnter, qty: "0.09", fields: map[string]string{"funding_reserve_usd": "2.16", "capture_usd": "3.24", "capture_bps": "6"}, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.params = map[string]interface{}{"entry_threshold_bps": 5.99, "exit_threshold_bps": 1, "funding_allowance_bps": 4}
			return f
		}},
		{name: "fee reserve equality holds", kind: pairPolicyStatusHold, holdHas: "entry_threshold", fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.cost = map[string]interface{}{"slippage_pct": 0, "taker_fee_pct": 0.0001}
			f.params = map[string]interface{}{"entry_threshold_bps": 5.998, "exit_threshold_bps": 1}
			return f
		}},
		{name: "fee reserve below the threshold enters", kind: pairPolicyStatusEnter, qty: "0.09", fields: map[string]string{"entry_fees_usd": "1.08054", "exit_fee_reserve_usd": "1.08054", "capture_usd": "3.23892", "capture_bps": "5.998"}, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.cost = map[string]interface{}{"slippage_pct": 0, "taker_fee_pct": 0.0001}
			f.params = map[string]interface{}{"entry_threshold_bps": 5.997, "exit_threshold_bps": 1}
			return f
		}},
		{name: "venue cap equality enters at the cap", kind: pairPolicyStatusEnter, qty: "0.05", fields: map[string]string{"proposed_buy_bound": "60000", "rounded_buy_bound": "60000"}, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.limits = map[string]interface{}{"hyperliquid_notional_cap_usd": 3000}
			return f
		}},
		{name: "venue cap one cent lower takes the next lot down", kind: pairPolicyStatusEnter, qty: "0.04", fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.limits = map[string]interface{}{"hyperliquid_notional_cap_usd": 2999.99}
			return f
		}},
		{name: "sell-side minimum notional equality enters", kind: pairPolicyStatusEnter, qty: "0.01", fields: map[string]string{"proposed_sell_bound": "1000", "rounded_sell_bound": "1000", "sell_member": pairTestHL}, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.depth = 1
			f.hlBids, f.hlAsks = [][2]string{{"1000", "0.01"}}, [][2]string{{"1000.5", "1"}}
			f.okxBids, f.okxAsks = [][2]string{{"989.9", "100"}}, [][2]string{{"990", "100"}}
			return f
		}},
		{name: "sell-side minimum notional below the venue minimum holds", kind: pairPolicyStatusHold, holdHas: "min_notional:" + pairTestHL, fixture: func() pairPolicyFixture {
			f := pairPolicyFlatSpreadBooks()
			f.depth = 1
			f.hlBids, f.hlAsks = [][2]string{{"999.9", "0.01"}}, [][2]string{{"1000.5", "1"}}
			f.okxBids, f.okxAsks = [][2]string{{"989.9", "100"}}, [][2]string{{"990", "100"}}
			return f
		}},
		{name: "native minimum quantity equality enters", kind: pairPolicyStatusEnter, qty: "0.03", fields: map[string]string{"sell_native_qty": "30", "buy_native_qty": "0.03"}, fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.doc = pairPolicyGridDoc
			f.limits = map[string]interface{}{"gross_budget_usd": 3700, "hyperliquid_notional_cap_usd": 3700, "okx_notional_cap_usd": 3700, "unmatched_cap_usd": 3700}
			return f
		}},
		{name: "below the native minimum quantity holds", kind: pairPolicyStatusHold, holdHas: "gross_budget", binding: "min_qty:" + pairTestOKX, fixture: func() pairPolicyFixture {
			f := pairPolicyGoodBooks()
			f.doc = pairPolicyGridDoc
			f.limits = map[string]interface{}{"gross_budget_usd": 3000, "hyperliquid_notional_cap_usd": 3000, "okx_notional_cap_usd": 3000, "unmatched_cap_usd": 3000}
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, c, root := newPairPolicyEnv(t, tc.fixture())
			_, got, d := pairPolicyDecide(t, e, 0)
			if got.Kind != tc.kind {
				t.Fatalf("decision %s (%s %s), want %s", got.Kind, got.PolicyReason, got.CoordinatorReason, tc.kind)
			}
			if p, _ := d["params"].(map[string]interface{}); len(p) != 4 {
				t.Fatalf("every decision records the four effective parameters: %v", d["params"])
			}
			if ev, _ := d["evidence"].(map[string]interface{}); len(ev) != 2 || d["input_fingerprint"] == "" {
				t.Fatalf("every decision records the frozen input evidence: %v", d)
			}
			if os.Getenv("PAIR_POLICY_DUMP") != "" {
				b, _ := json.MarshalIndent(d, "", " ")
				t.Logf("%s", b)
			}
			if tc.kind == pairPolicyStatusHold {
				if !strings.Contains(got.PolicyReason, tc.holdHas) {
					t.Fatalf("hold reason %q must name %s", got.PolicyReason, tc.holdHas)
				}
				if tc.binding != "" {
					report := pairPolicyOrientationReport(t, d, pairOrientationLongHLShortOKX)
					sums, _ := report["interval_summaries"].([]interface{})
					shallow, _ := sums[len(sums)-1].(map[string]interface{})
					if !pairPolicyHasString(shallow["binding_constraints"], tc.binding) {
						t.Fatalf("the shallowest interval must be bound by %s: %v", tc.binding, shallow)
					}
				}
				return
			}
			sel, _ := d["selected"].(map[string]interface{})
			if sel["qty"] != tc.qty {
				t.Fatalf("selected %v, want %s: %v", sel["qty"], tc.qty, sel)
			}
			for k, want := range tc.fields {
				if sel[k] != want {
					t.Fatalf("selected %s = %v, want %s: %v", k, sel[k], want, sel)
				}
			}
			req, _ := c.last(t)
			brute := pairPolicyBrute(t, root, req)
			if brute[got.Orientation]["qty"] != tc.qty {
				t.Fatalf("the exhaustive enumeration selects %v, want %s", brute[got.Orientation]["qty"], tc.qty)
			}
		})
	}
}

func TestBasketPairPolicyRealConvertedCapCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		okxCap float64
		kind   string
	}{{"converted cap equality passes", 3750, pairPolicyStatusEnter}, {"converted excess holds", 3700, pairPolicyStatusHold}} {
		t.Run(tc.name, func(t *testing.T) {
			f := pairPolicyGoodBooks()
			f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": tc.okxCap}
			f.evidence = pairPolicyEvidence(map[string]string{"USDT": "1.25"}, nil, nil)
			e, _, _ := newPairPolicyEnv(t, f)
			e.bc.pairPolicyRun = e.runner.run
			e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
				res := pairEnterResult(r, 0.5)
				res.Targets = map[string]pairPolicyMemberTarget{pairTestHL: {TargetBaseQty: 0.5, PriceBound: 5000}, pairTestOKX: {TargetBaseQty: -0.5, PriceBound: 6000}}
				return res, nil
			}
			at := pairTestStart()
			e.slot(at, "in-0")
			e.step(at.Add(time.Second))
			got, _ := pairPolicyStoredDecision(t, e, at)
			if got.Kind != tc.kind {
				t.Fatalf("decision %s (%s), want %s: the okx leg is 0.5 x 6000 USDT x 1.25 = $3750", got.Kind, got.CoordinatorReason, tc.kind)
			}
			if tc.kind == pairPolicyStatusHold && !strings.Contains(got.CoordinatorReason, "okx $3750.00 > venue cap $3700.00") {
				t.Fatalf("the cap hold must use the converted notional: %q", got.CoordinatorReason)
			}
		})
	}
}

func pairPolicyLevels(prices []string, qty string) [][2]string {
	out := make([][2]string, 0, len(prices))
	for _, p := range prices {
		out = append(out, [2]string{p, qty})
	}
	return out
}

func pairPolicyExitInfo(t *testing.T, d map[string]interface{}) map[string]interface{} {
	t.Helper()
	info, _ := d["exit"].(map[string]interface{})
	if info == nil {
		t.Fatalf("the matched decision records no exit evidence: %v", d)
	}
	return info
}

func TestBasketPairPolicyRealLifecycleBothOrientations(t *testing.T) {
	type books struct{ hlBids, hlAsks, okxBids, okxAsks [][2]string }
	cases := []struct {
		name        string
		orientation string
		hl          float64
		entry       books
		wide        books
		converge    books
	}{
		{name: "long hyperliquid short okx", orientation: pairOrientationLongHLShortOKX, hl: 0.06,
			entry: books{pairPolicyLevels([]string{"59999", "59998", "59997"}, "1"), pairPolicyLevels([]string{"60000", "60001", "60002"}, "1"),
				pairPolicyLevels([]string{"60600", "60599.9", "60599.8"}, "100"), pairPolicyLevels([]string{"60600.1", "60600.2", "60600.3"}, "100")},
			wide: books{pairPolicyLevels([]string{"130000", "129990", "129980"}, "1"), pairPolicyLevels([]string{"130010", "130020", "130030"}, "1"),
				pairPolicyLevels([]string{"130499.9", "130499.8", "130499.7"}, "100"), pairPolicyLevels([]string{"130500", "130500.1", "130500.2"}, "100")},
			converge: books{pairPolicyLevels([]string{"130000", "129990", "129980"}, "1"), pairPolicyLevels([]string{"130010", "130020", "130030"}, "1"),
				pairPolicyLevels([]string{"130349.9", "130349.8", "130349.7"}, "100"), pairPolicyLevels([]string{"130350", "130350.1", "130350.2"}, "100")}},
		{name: "long okx short hyperliquid", orientation: pairOrientationLongOKXShortHL, hl: -0.06,
			entry: books{pairPolicyLevels([]string{"60600", "60599", "60598"}, "1"), pairPolicyLevels([]string{"60601", "60602", "60603"}, "1"),
				pairPolicyLevels([]string{"59999.9", "59999.8", "59999.7"}, "100"), pairPolicyLevels([]string{"60000", "60000.1", "60000.2"}, "100")},
			wide: books{pairPolicyLevels([]string{"130490", "130480", "130470"}, "1"), pairPolicyLevels([]string{"130500", "130510", "130520"}, "1"),
				pairPolicyLevels([]string{"130000", "129999.9", "129999.8"}, "100"), pairPolicyLevels([]string{"130000.1", "130000.2", "130000.3"}, "100")},
			converge: books{pairPolicyLevels([]string{"130340", "130330", "130320"}, "1"), pairPolicyLevels([]string{"130350", "130360", "130370"}, "1"),
				pairPolicyLevels([]string{"130000", "129999.9", "129999.8"}, "100"), pairPolicyLevels([]string{"130000.1", "130000.2", "130000.3"}, "100")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := pairPolicyFixture{depth: 3, params: map[string]interface{}{"entry_threshold_bps": 40, "exit_threshold_bps": 38},
				limits: map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000},
				hlBids: tc.entry.hlBids, hlAsks: tc.entry.hlAsks, okxBids: tc.entry.okxBids, okxAsks: tc.entry.okxAsks}
			e, c, _ := newPairPolicyEnv(t, f)
			at0, got, d := pairPolicyDecide(t, e, 0)
			if got.Kind != pairPolicyStatusEnter || got.Orientation != tc.orientation {
				t.Fatalf("slot 0 decision %s %s (%s %s)", got.Kind, got.Orientation, got.PolicyReason, got.CoordinatorReason)
			}
			if sel, _ := d["selected"].(map[string]interface{}); sel["qty"] != "0.06" {
				t.Fatalf("the entry must size 0.06 base from the gross budget: %v", sel)
			}
			e.slotStep(1)
			e.assertBooks("hedged", tc.hl, -tc.hl)
			if e.lifecycle() != pairLifecycleMatched {
				t.Fatalf("lifecycle after the hedge is %s", e.lifecycle())
			}
			e.setBooks(tc.wide.hlBids, tc.wide.hlAsks, tc.wide.okxBids, tc.wide.okxAsks)
			_, got, d = pairPolicyDecide(t, e, 2)
			if got.Kind != pairPolicyStatusHold || !strings.Contains(got.PolicyReason, "above the exit threshold") {
				t.Fatalf("a matched pair above the exit threshold must hold: %s %q", got.Kind, got.PolicyReason)
			}
			info := pairPolicyExitInfo(t, d)
			if info["booked_orientation"] != tc.orientation || info["long_qty"] != "0.06" || info["short_qty"] != "0.06" {
				t.Fatalf("the exit evaluation must use the booked orientation and quantities: %v", info)
			}
			e.assertBooks("held", tc.hl, -tc.hl)
			e.setBooks(tc.converge.hlBids, tc.converge.hlAsks, tc.converge.okxBids, tc.converge.okxAsks)
			_, got, d = pairPolicyDecide(t, e, 3)
			if got.Kind != pairPolicyStatusExit {
				t.Fatalf("the converged pair must exit: %s %q %q", got.Kind, got.PolicyReason, got.CoordinatorReason)
			}
			for id, m := range got.Members {
				if m.TargetBaseQty != 0 || !(m.PriceBound > 0) {
					t.Fatalf("exit target for %s must be zero with a closing bound: %+v", id, m)
				}
			}
			info = pairPolicyExitInfo(t, d)
			if s, _ := info["booked_pnl_estimate_usd"].(string); !strings.HasPrefix(s, "-") {
				t.Fatalf("this convergence exit after a doubled price level must record a loss, got %v", info)
			}
			for _, k := range []string{"close_fees_usd", "net_liquidation_usd", "long_sale_proceeds_usd", "short_cover_cost_usd", "remaining_spread_bps", "rounded_long_sell_bound", "rounded_short_buy_bound"} {
				if _, ok := info[k].(string); !ok {
					t.Fatalf("exit evidence misses %s: %v", k, info)
				}
			}
			e.assertBooks("exited", 0, 0)
			if len(c.requests) != 3 {
				t.Fatalf("the policy process must run for the flat and the two matched slots only, ran %d times", len(c.requests))
			}
			e.restart()
			e.bc.pairPolicyRun = c.run
			for i, want := range []string{pairPolicyStatusEnter, "", pairPolicyStatusHold, pairPolicyStatusExit} {
				at := pairSlotAt(at0, i)
				var stored string
				if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, at.UnixMilli()).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if want == "" {
					if stored != "" {
						t.Fatalf("slot %d served only as evidence, but saved %q", i, stored)
					}
					continue
				}
				got, err := parsePairTargets(stored)
				if err != nil || got.Kind != want || len(got.Decision) == 0 {
					t.Fatalf("slot %d persisted %q (%v) after a restart, want %s with its decision", i, stored, err, want)
				}
			}
		})
	}
}

func TestBasketPairPolicyRealExitBoundaries(t *testing.T) {
	entry := pairPolicyFixture{depth: 3, cost: map[string]interface{}{"slippage_pct": 0},
		limits:  map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000},
		hlBids:  pairPolicyLevels([]string{"59999", "59998", "59997"}, "1"),
		hlAsks:  pairPolicyLevels([]string{"60000", "60001", "60002"}, "1"),
		okxBids: pairPolicyLevels([]string{"60300", "60299.9", "60299.8"}, "100"),
		okxAsks: pairPolicyLevels([]string{"60300.1", "60300.2", "60300.3"}, "100"),
	}
	converged := func(e *pairTestEnv, okxAsk string) {
		e.setBooks(pairPolicyLevels([]string{"60000", "59999", "59998"}, "1"), pairPolicyLevels([]string{"60001", "60002", "60003"}, "1"),
			pairPolicyLevels([]string{"60005.5", "60005.4", "60005.3"}, "100"), pairPolicyLevels([]string{okxAsk, okxAsk + "1", okxAsk + "2"}, "100"))
	}
	t.Run("inclusive exit equality after a held slot just above it", func(t *testing.T) {
		e, _, _ := newPairPolicyEnv(t, entry)
		if _, got, _ := pairPolicyDecide(t, e, 0); got.Kind != pairPolicyStatusEnter {
			t.Fatalf("entry %s %q", got.Kind, got.PolicyReason)
		}
		e.slotStep(1)
		e.assertBooks("hedged", 0.06, -0.06)
		converged(e, "60006.1")
		if _, got, d := pairPolicyDecide(t, e, 2); got.Kind != pairPolicyStatusHold || pairPolicyExitInfo(t, d)["remaining_spread_bps"] != "61/60" {
			t.Fatalf("1.0167 bps is above the 1 bp exit threshold and must hold: %s %v", got.Kind, d["exit"])
		}
		converged(e, "60006")
		_, got, d := pairPolicyDecide(t, e, 3)
		if got.Kind != pairPolicyStatusExit || pairPolicyExitInfo(t, d)["remaining_spread_bps"] != "1" {
			t.Fatalf("a remaining spread equal to the exit threshold must exit: %s %v", got.Kind, d["exit"])
		}
		e.assertBooks("exited", 0, 0)
	})
	t.Run("unequal sub-lot matched quantities exit on each leg's own size", func(t *testing.T) {
		e, _, _ := newPairPolicyEnv(t, entry)
		if _, got, _ := pairPolicyDecide(t, e, 0); got.Kind != pairPolicyStatusEnter {
			t.Fatalf("entry %s %q", got.Kind, got.PolicyReason)
		}
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenuePartial, CumQty: 0.0595, CumNotional: 0.0595 * 60000, Terminal: true})
		e.slotStep(1)
		e.assertBooks("partial hedge", 0.0595, -0.06)
		if e.lifecycle() != pairLifecycleMatched {
			t.Fatalf("a hedge short of one lot must still match the pair, lifecycle %s", e.lifecycle())
		}
		converged(e, "60006")
		_, got, d := pairPolicyDecide(t, e, 2)
		info := pairPolicyExitInfo(t, d)
		if got.Kind != pairPolicyStatusExit || info["long_qty"] != "0.0595" || info["short_qty"] != "0.06" || info["qty_difference"] != "-0.0005" {
			t.Fatalf("unequal matched legs must exit on their own quantities: %s %q %v", got.Kind, got.PolicyReason, info)
		}
		if info["remaining_spread_bps"] != "1" {
			t.Fatalf("the per-unit remaining spread must not depend on the size difference: %v", info["remaining_spread_bps"])
		}
		e.assertBooks("exited", 0, 0)
	})
	t.Run("missing closing depth holds the policy while the hold-age exit still runs", func(t *testing.T) {
		aged := entry
		aged.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000, "max_hold_age_ms": 150000}
		e, c, _ := newPairPolicyEnv(t, aged)
		if _, got, _ := pairPolicyDecide(t, e, 0); got.Kind != pairPolicyStatusEnter {
			t.Fatalf("entry %s %q", got.Kind, got.PolicyReason)
		}
		e.slotStep(1)
		e.setBooks(pairPolicyLevels([]string{"60000", "59999", "59998"}, "0.01"), pairPolicyLevels([]string{"60001", "60002", "60003"}, "1"),
			pairPolicyLevels([]string{"60005.5", "60005.4", "60005.3"}, "100"), pairPolicyLevels([]string{"60006", "60006.1", "60006.2"}, "100"))
		_, got, _ := pairPolicyDecide(t, e, 2)
		if got.Kind != pairPolicyStatusHold || !strings.Contains(got.PolicyReason, "closing depth") {
			t.Fatalf("missing closing depth must hold with a reason: %s %q", got.Kind, got.PolicyReason)
		}
		e.assertBooks("held", 0.06, -0.06)
		e.slotStep(3)
		e.assertBooks("hold-age exit", 0, 0)
		if len(c.requests) != 2 {
			t.Fatalf("the hold-age exit belongs to the coordinator; the policy ran %d times, want 2", len(c.requests))
		}
	})
}

const pairPolicyStubScript = `import json, os, sys, time
req = json.loads(sys.stdin.buffer.read())
mode = os.environ.get("PAIR_POLICY_STUB", "")
res = {"v": 2, "pair_id": req["pair_id"], "slot_ms": req["slot_ms"], "contract_fingerprint": req["contract_fingerprint"],
       "input_fingerprint": req["input_fingerprint"], "status": "enter", "orientation": "long_hyperliquid_short_okx", "decision": {"reason": "stub"},
       "targets": {"pair-hl-btc": {"target_base_qty": 0.05, "price_bound": 60400.0}, "pair-okx-btc": {"target_base_qty": -0.05, "price_bound": 59700.0}}}
text = json.dumps(res)
if mode == "valid":
    print(text)
elif mode == "exit1":
    print(text)
    sys.exit(1)
elif mode == "duplicate":
    print(text[:-1] + ', "status": "enter"}')
elif mode == "trailing":
    print(text + " {}")
elif mode == "unknown_target_field":
    res["targets"]["pair-hl-btc"]["note"] = 1
    print(json.dumps(res))
elif mode == "malformed":
    print("{not json")
elif mode == "null_decision":
    res["decision"] = None
    print(json.dumps(res))
elif mode == "oversize_decision":
    res["decision"] = {"pad": "z" * (70 << 10)}
    print(json.dumps(res))
elif mode == "escaped_decision":
    res["decision"] = {"pad": "<" * (60 << 10)}
    print(json.dumps(res))
elif mode == "escaped_hold":
    for k in ("targets", "orientation"):
        res.pop(k)
    res["status"] = "hold"
    res["hold_reason"] = "stub policy hold"
    res["decision"] = {"pad": "<" * (60 << 10)}
    print(json.dumps(res))
elif mode == "stdout_overflow":
    sys.stdout.write("x" * (300 << 10))
    sys.stdout.flush()
    time.sleep(30)
elif mode == "stderr_overflow":
    sys.stderr.write("y" * (100 << 10))
    sys.stderr.flush()
    time.sleep(30)
elif mode == "sleep":
    with open(os.environ["PAIR_POLICY_PIDFILE"], "w") as fh:
        fh.write(str(os.getpid()))
    time.sleep(60)
`

func pairPolicyUseStub(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "b1875-policy-stub.py")
	if err := os.WriteFile(path, []byte(pairPolicyStubScript), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := pairPolicyScript
	pairPolicyScript = path
	t.Cleanup(func() { pairPolicyScript = prev })
}

func pairCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	defer func() {
		os.Stdout = orig
		w.Close()
		<-done
		r.Close()
	}()
	fn()
	os.Stdout = orig
	w.Close()
	<-done
	return buf.String()
}

func (e *pairTestEnv) assertNoPairAction(label string) {
	e.t.Helper()
	if n := e.count(`SELECT COUNT(*) FROM basket_member_actions`); n != 0 {
		e.t.Fatalf("%s: a held decision reserved %d member actions", label, n)
	}
	e.assertBooks(label, 0, 0)
}

func TestBasketPairPolicyRealProcessFailuresHold(t *testing.T) {
	cases := []struct {
		mode     string
		at       time.Duration
		kind     string
		reason   string
		decision bool
		logHas   []string
		logNot   []string
	}{
		{mode: "valid", at: time.Second, kind: pairPolicyStatusEnter, decision: true, logHas: []string{"enter decision frozen (long_hyperliquid_short_okx"}, logNot: []string{" held: "}},
		{mode: "exit1", at: time.Second, kind: pairPolicyStatusHold, reason: "the policy run failed: exit status 1"},
		{mode: "duplicate", at: time.Second, kind: pairPolicyStatusHold, reason: "duplicate object key"},
		{mode: "trailing", at: time.Second, kind: pairPolicyStatusHold, reason: "trailing value"},
		{mode: "unknown_target_field", at: time.Second, kind: pairPolicyStatusHold, reason: "exactly {target_base_qty, price_bound}"},
		{mode: "malformed", at: time.Second, kind: pairPolicyStatusHold, reason: "invalid policy output: unreadable result"},
		{mode: "null_decision", at: time.Second, kind: pairPolicyStatusHold, reason: "decision must be a JSON object"},
		{mode: "oversize_decision", at: time.Second, kind: pairPolicyStatusHold, reason: "above the 65536-byte limit"},
		{mode: "escaped_decision", at: time.Second, kind: pairPolicyStatusHold, reason: "above the 98304-byte limit", decision: true,
			logHas: []string{"held: the decision record is", "above the 98304-byte limit"}, logNot: []string{"decision frozen"}},
		{mode: "escaped_hold", at: time.Second, kind: pairPolicyStatusHold, reason: "above the 98304-byte limit", decision: true,
			logHas: []string{"held: the decision record is", "above the 98304-byte limit"}, logNot: []string{"stub policy hold", "decision frozen"}},
		{mode: "stdout_overflow", at: time.Second, kind: pairPolicyStatusHold, reason: "script stdout exceeded the 262144 byte output limit"},
		{mode: "stderr_overflow", at: time.Second, kind: pairPolicyStatusHold, reason: "script stderr exceeded the 65536 byte output limit"},
		{mode: "sleep", at: 4 * time.Second, kind: pairPolicyStatusHold, reason: "the policy run ended by its context (context deadline exceeded)"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			e, _, _ := newPairPolicyEnv(t, pairPolicyFixture{})
			pairPolicyUseStub(t)
			pidFile := filepath.Join(t.TempDir(), "pid")
			t.Setenv("PAIR_POLICY_STUB", tc.mode)
			t.Setenv("PAIR_POLICY_PIDFILE", pidFile)
			e.bc.pairPolicyRun = runPairPolicyProcess
			at := pairTestStart()
			e.slot(at, "in-0")
			logged := pairCaptureStdout(t, func() { e.step(at.Add(tc.at)) })
			for _, w := range tc.logHas {
				if !strings.Contains(logged, w) {
					t.Fatalf("the slot log must name %q, the saved decision:\n%s", w, logged)
				}
			}
			for _, w := range tc.logNot {
				if strings.Contains(logged, w) {
					t.Fatalf("the slot log must not name %q, a decision that was never saved:\n%s", w, logged)
				}
			}
			got, _ := pairPolicyStoredDecision(t, e, at)
			if got.Kind != tc.kind || !strings.Contains(got.CoordinatorReason, tc.reason) {
				t.Fatalf("decision %s %q, want %s naming %q", got.Kind, got.CoordinatorReason, tc.kind, tc.reason)
			}
			if (len(got.Decision) > 0 && string(got.Decision) != "null") != tc.decision {
				t.Fatalf("decision evidence kept=%v, want %v: %s", len(got.Decision) > 0, tc.decision, got.Decision)
			}
			if len(pythonSemaphore) != 0 {
				t.Fatalf("the shared python permit was not released (%d held)", len(pythonSemaphore))
			}
			if tc.kind == pairPolicyStatusHold {
				e.assertNoPairAction(tc.mode)
			}
			if tc.mode == "sleep" {
				if _, ok := pairIncompleteEvidence(e.rt()); !ok {
					t.Fatal("the timed-out policy run must save the policy_infrastructure_timeout marker")
				}
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatalf("the sleeping policy never started: %v", err)
				}
				var pid int
				fmt.Sscanf(string(raw), "%d", &pid)
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("the timed-out policy process %d is still running (%v)", pid, err)
				}
			}
		})
	}
	t.Run("saturated admission", func(t *testing.T) {
		e, _, _ := newPairPolicyEnv(t, pairPolicyFixture{})
		pairPolicyUseStub(t)
		t.Setenv("PAIR_POLICY_STUB", "valid")
		e.bc.pairPolicyRun = runPairPolicyProcess
		for i := 0; i < cap(pythonSemaphore); i++ {
			pythonSemaphore <- struct{}{}
		}
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(4 * time.Second))
		for i := 0; i < cap(pythonSemaphore); i++ {
			<-pythonSemaphore
		}
		got, _ := pairPolicyStoredDecision(t, e, at)
		if got.Kind != pairPolicyStatusHold || !strings.Contains(got.CoordinatorReason, "the policy run ended by its context (context deadline exceeded)") {
			t.Fatalf("saturated admission must hold inside the policy budget: %s %q", got.Kind, got.CoordinatorReason)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); !ok {
			t.Fatal("a saturated admission that used up the policy budget must save the policy_infrastructure_timeout marker")
		}
		e.assertNoPairAction("saturated")
	})
	t.Run("cancelled admission", func(t *testing.T) {
		e, _, _ := newPairPolicyEnv(t, pairPolicyFixture{})
		pairPolicyUseStub(t)
		t.Setenv("PAIR_POLICY_STUB", "valid")
		e.bc.pairPolicyRun = runPairPolicyProcess
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.bc.parent = ctx
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		got, _ := pairPolicyStoredDecision(t, e, at)
		if got.Kind != pairPolicyStatusHold || !strings.Contains(got.CoordinatorReason, "context canceled") {
			t.Fatalf("a cancelled admission must hold: %s %q", got.Kind, got.CoordinatorReason)
		}
		e.assertNoPairAction("cancelled")
	})
}

func TestBasketPairPolicyRealDecisionAuthority(t *testing.T) {
	good := func() pairPolicyFixture {
		f := pairPolicyGoodBooks()
		f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
		return f
	}
	t.Run("semantic invalid evidence is frozen as a hold", func(t *testing.T) {
		e, c, _ := newPairPolicyEnv(t, good())
		c.mutate = func(stdin []byte) []byte {
			return bytes.Replace(stdin, []byte(`"equity":"6000"`), []byte(`"equity":"6001"`), 1)
		}
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		_, out := c.last(t)
		var res map[string]interface{}
		if err := json.Unmarshal(out, &res); err != nil || res["status"] != pairPolicyStatusInvalid {
			t.Fatalf("damaged evidence must give a semantic invalid result with exit 0: %s", out)
		}
		got, d := pairPolicyStoredDecision(t, e, at)
		if got.Kind != pairPolicyStatusHold || !strings.Contains(got.CoordinatorReason, "invalid policy output: the saved quote envelope fails verification") || d["stage"] != "evidence" {
			t.Fatalf("the invalid result must be frozen as a hold with its evidence: %s %q %v", got.Kind, got.CoordinatorReason, d)
		}
		e.assertNoPairAction("invalid")
	})
	t.Run("a changed pair snapshot discards the result", func(t *testing.T) {
		e, c, _ := newPairPolicyEnv(t, good())
		c.during = func() {
			e.mu.Lock()
			e.state.Strategies[pairTestHL].Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
			e.mu.Unlock()
		}
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		if rb := e.rt().Rebalances[at.UnixMilli()]; rb.TargetsJSON != "" {
			t.Fatalf("a result computed on a changed snapshot was frozen: %s", rb.TargetsJSON)
		}
		if n := e.count(`SELECT COUNT(*) FROM basket_member_actions`); n != 0 {
			t.Fatalf("a discarded result reserved %d actions", n)
		}
	})
	t.Run("a failed decision save permits no execution", func(t *testing.T) {
		e, _, _ := newPairPolicyEnv(t, good())
		if _, err := e.db().db.Exec(`CREATE TRIGGER b1875_fail_decision BEFORE UPDATE OF targets_json ON basket_rebalances WHEN NEW.targets_json <> '' BEGIN SELECT RAISE(ABORT, 'injected decision save failure'); END`); err != nil {
			t.Fatal(err)
		}
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		var stored string
		if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, at.UnixMilli()).Scan(&stored); err != nil || stored != "" {
			t.Fatalf("a failed save must leave the slot undecided: %q %v", stored, err)
		}
		e.assertNoPairAction("failed save")
		if _, err := e.db().db.Exec(`DROP TRIGGER b1875_fail_decision`); err != nil {
			t.Fatal(err)
		}
		e.step(at.Add(2 * time.Second))
		if got, _ := pairPolicyStoredDecision(t, e, at); got.Kind != pairPolicyStatusEnter {
			t.Fatalf("the slot must be decided again from its saved envelope after the save recovers: %s %q", got.Kind, got.PolicyReason)
		}
	})
	t.Run("a restart decides the saved envelope", func(t *testing.T) {
		e, c, _ := newPairPolicyEnv(t, good())
		at := pairTestStart()
		e.slot(at, "in-0")
		e.restart()
		e.bc.pairPolicyRun = c.run
		e.step(at.Add(time.Second))
		got, d := pairPolicyStoredDecision(t, e, at)
		if got.Kind != pairPolicyStatusEnter || d["input_fingerprint"] != got.InputFingerprint {
			t.Fatalf("the restarted run must decide from the saved envelope: %s %v", got.Kind, d["input_fingerprint"])
		}
	})
}

func pairPolicyMutateRequest(t *testing.T, raw []byte, mutate func(req map[string]interface{})) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var req map[string]interface{}
	if err := dec.Decode(&req); err != nil {
		t.Fatal(err)
	}
	mutate(req)
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pairPolicyRunScript(t *testing.T, root string, stdin []byte) map[string]interface{} {
	t.Helper()
	out, errOut, code := pairPolicyPython(t, root, stdin, filepath.Join("shared_scripts", "run_pair_policy.py"))
	if code != 0 {
		t.Fatalf("a semantic result must exit 0, got %d: %s", code, errOut)
	}
	if bytes.Count(bytes.TrimSpace(out), []byte("\n")) != 0 {
		t.Fatalf("the policy must print exactly one JSON object: %q", out)
	}
	tree, err := pairStrictParse(out)
	if err != nil {
		t.Fatalf("the policy output is not one strict JSON value: %v (%s)", err, out)
	}
	if _, ok := tree.(map[string]interface{}); !ok {
		t.Fatalf("the policy output is not an object: %s", out)
	}
	var res map[string]interface{}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBasketPairPolicyRealProcessContract(t *testing.T) {
	f := pairPolicyGoodBooks()
	f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
	e, c, root := newPairPolicyEnv(t, f)
	if _, got, _ := pairPolicyDecide(t, e, 0); got.Kind != pairPolicyStatusEnter {
		t.Fatalf("the capture slot must enter: %s %q", got.Kind, got.PolicyReason)
	}
	req, coordinatorOut := c.last(t)
	var ident map[string]interface{}
	_ = json.Unmarshal(req, &ident)
	first, _, _ := pairPolicyPython(t, root, req, filepath.Join("shared_scripts", "run_pair_policy.py"))
	second, _, _ := pairPolicyPython(t, root, req, filepath.Join("shared_scripts", "run_pair_policy.py"))
	if !bytes.Equal(first, second) || !bytes.Equal(first, coordinatorOut) {
		t.Fatalf("identical request bytes must give identical decisions:\n%s\n%s\n%s", first, second, coordinatorOut)
	}
	damage := func(r map[string]interface{}) {
		env := r["envelope"].(map[string]interface{})
		acct := env["members"].([]interface{})[0].(map[string]interface{})["account"].(map[string]interface{})
		acct["equity"] = "6001"
	}
	booked := func(hl, okx string) func(r map[string]interface{}) {
		return func(r map[string]interface{}) {
			for _, m := range r["members"].([]interface{}) {
				mm := m.(map[string]interface{})
				if mm["member_id"] == pairTestHL {
					mm["booked_base_qty"] = json.Number(hl)
				} else {
					mm["booked_base_qty"] = json.Number(okx)
				}
			}
		}
	}
	cases := []struct {
		name   string
		stdin  []byte
		status string
		has    string
		stage  string
	}{
		{name: "duplicate request key", stdin: bytes.Replace(req, []byte(`"policy":`), []byte(`"policy":"x","policy":`), 1), status: pairPolicyStatusInvalid, has: "duplicate object key"},
		{name: "trailing request value", stdin: append(append([]byte(nil), req...), []byte(` {}`)...), status: pairPolicyStatusInvalid, has: "not strict JSON"},
		{name: "unknown policy", stdin: pairPolicyMutateRequest(t, req, func(r map[string]interface{}) { r["policy"] = "other_policy" }), status: pairPolicyStatusInvalid, has: "not a known pair policy"},
		{name: "unsupported version", stdin: pairPolicyMutateRequest(t, req, func(r map[string]interface{}) { r["v"] = json.Number("3") }), status: pairPolicyStatusInvalid, has: "request version 3"},
		{name: "damaged evidence", stdin: pairPolicyMutateRequest(t, req, damage), status: pairPolicyStatusInvalid, has: "input_fingerprint", stage: "evidence"},
		{name: "parameter refusal precedes envelope verification", stdin: pairPolicyMutateRequest(t, req, func(r map[string]interface{}) {
			damage(r)
			r["params"].(map[string]interface{})["entry_threshold_bps"] = "5"
		}), status: pairPolicyStatusInvalid, has: "params.entry_threshold_bps must be a finite JSON number, got string", stage: "params"},
		{name: "unsupported lifecycle", stdin: pairPolicyMutateRequest(t, req, func(r map[string]interface{}) { r["lifecycle"] = "opening" }), status: pairPolicyStatusHold, has: "has no policy decision"},
		{name: "flat lifecycle with booked exposure", stdin: pairPolicyMutateRequest(t, req, booked("0.06", "0")), status: pairPolicyStatusHold, has: "lifecycle is flat but"},
		{name: "matched legs with the same sign", stdin: pairPolicyMutateRequest(t, req, func(r map[string]interface{}) {
			booked("0.06", "0.06")(r)
			r["lifecycle"] = "matched"
		}), status: pairPolicyStatusHold, has: "one long and one short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := pairPolicyRunScript(t, root, tc.stdin)
			text, _ := res["error"].(string)
			if tc.status == pairPolicyStatusHold {
				text, _ = res["hold_reason"].(string)
			}
			if res["status"] != tc.status || !strings.Contains(text, tc.has) {
				t.Fatalf("result %v %q, want %s naming %q", res["status"], text, tc.status, tc.has)
			}
			d, _ := res["decision"].(map[string]interface{})
			if d == nil || d["reason"] == "" || (tc.stage != "" && d["stage"] != tc.stage) {
				t.Fatalf("the result must carry decision evidence at stage %q: %v", tc.stage, d)
			}
			if tc.name != "duplicate request key" && tc.name != "trailing request value" {
				for _, k := range []string{"pair_id", "contract_fingerprint", "input_fingerprint"} {
					if res[k] != ident[k] {
						t.Fatalf("the result must preserve the request %s: %v vs %v", k, res[k], ident[k])
					}
				}
			}
		})
	}
}

const pairPolicyIsolationPrelude = `import builtins, importlib.abc, io, os, runpy, socket, sys
preloaded = set(sys.modules)
allowed = set(sys.stdlib_module_names) | {"pair_evidence", "pair_inputs", "go_trader_pair_policy_cross_exchange_perp_arbitrage", "__main__"}
class Block(importlib.abc.MetaPathFinder):
    def find_spec(self, name, path=None, target=None):
        if name.split(".")[0] not in allowed:
            raise ImportError("blocked non-standard-library import: " + name)
        return None
sys.meta_path.insert(0, Block())
def no_network(*a, **k):
    raise OSError("network access is blocked")
socket.socket = no_network
socket.create_connection = no_network
real_open = builtins.open
def guarded_open(file, mode="r", *a, **k):
    if any(c in mode for c in "wax+"):
        raise OSError("file writes are blocked: " + str(file))
    return real_open(file, mode, *a, **k)
builtins.open = guarded_open
io.open = guarded_open
script = sys.argv[1]
sys.argv = sys.argv[1:]
try:
    runpy.run_path(script, run_name="__main__")
finally:
    foreign = sorted(n for n in sys.modules if n not in preloaded and n.split(".")[0] not in allowed)
    if foreign:
        sys.stderr.write("non-standard-library modules loaded: " + ",".join(foreign) + "\n")
        os._exit(3)
`

func TestBasketPairPolicyRealImportBoundaryAndProbe(t *testing.T) {
	f := pairPolicyGoodBooks()
	f.limits = map[string]interface{}{"gross_budget_usd": 7300, "hyperliquid_notional_cap_usd": 7000, "okx_notional_cap_usd": 7000}
	e, c, root := newPairPolicyEnv(t, f)
	if _, got, _ := pairPolicyDecide(t, e, 0); got.Kind != pairPolicyStatusEnter {
		t.Fatalf("the capture slot must enter: %s %q", got.Kind, got.PolicyReason)
	}
	req, want := c.last(t)
	python := filepath.Join(root, ".venv", "bin", "python3")
	script := filepath.Join(root, "shared_scripts", "run_pair_policy.py")
	cache := t.TempDir()
	run := func(stdin func(cmd *exec.Cmd) func(), args ...string) ([]byte, []byte, error) {
		cmd := exec.Command(python, append([]string{"-I", "-c", pairPolicyIsolationPrelude, script}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "PYTHONPYCACHEPREFIX="+cache)
		done := stdin(cmd)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		ch := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { ch <- cmd.Wait() }()
		select {
		case err := <-ch:
			done()
			return out.Bytes(), errOut.Bytes(), err
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			done()
			t.Fatalf("the isolated policy run did not finish: %s", errOut.String())
		}
		return nil, nil, nil
	}
	out, errOut, err := run(func(cmd *exec.Cmd) func() {
		cmd.Stdin = bytes.NewReader(req)
		return func() {}
	})
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("the evaluator must run with only the standard library and its two helpers and give the same decision: %v %s\n%s\n%s", err, errOut, out, want)
	}
	out, errOut, err = run(func(cmd *exec.Cmd) func() {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		cmd.Stdin = r
		return func() { r.Close(); w.Close() }
	}, "--probe-only")
	if err != nil || !strings.Contains(string(out), `"probe":true`) {
		t.Fatalf("the probe must load the evaluator without stdin, network or writes: %v %s %s", err, out, errOut)
	}
	var written []string
	_ = filepath.Walk(cache, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			written = append(written, path)
		}
		return nil
	})
	if len(written) > 0 {
		t.Fatalf("the policy runner wrote bytecode: %v", written)
	}
	if err := probeOneCheckScript(pairPolicyScript, pairPolicyProbeArgv); err != nil {
		t.Fatalf("the deployment probe must reach the policy runner: %v", err)
	}
	var calls []string
	prev := probeOneCheckScriptFn
	probeOneCheckScriptFn = func(s string, argv []string) error {
		if s == pairPolicyScript {
			calls = append(calls, strings.Join(argv, " "))
		}
		return nil
	}
	defer func() { probeOneCheckScriptFn = prev }()
	if err := probeCheckScripts(&Config{Strategies: []StrategyConfig{{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Script: hyperliquidCheckScript}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "|") != "--probe-only" {
		t.Fatalf("an ordinary configuration with a check script must probe the policy runner once: %v", calls)
	}
}

func TestBasketPairPolicyRealParameterRefusals(t *testing.T) {
	e, c, root := newPairPolicyEnv(t, pairPolicyFixture{})
	e.slotStep(0)
	req, _ := c.last(t)
	set := func(key string, value interface{}) func(p map[string]interface{}) {
		return func(p map[string]interface{}) { p[key] = value }
	}
	cases := []struct {
		name   string
		mutate func(p map[string]interface{})
		want   string
	}{
		{"missing", func(p map[string]interface{}) { delete(p, "funding_allowance_bps") }, "params.funding_allowance_bps is missing"},
		{"unknown", set("entry_bps", json.Number("5")), "params has unknown field 'entry_bps'"},
		{"null", set("entry_threshold_bps", nil), "params.entry_threshold_bps must be a finite JSON number, got null"},
		{"boolean", set("exit_threshold_bps", true), "params.exit_threshold_bps must be a finite JSON number, got boolean"},
		{"string", set("adverse_band_bps", "5"), "params.adverse_band_bps must be a finite JSON number, got string"},
		{"array", set("funding_allowance_bps", []interface{}{json.Number("1")}), "params.funding_allowance_bps must be a finite JSON number, got array"},
		{"object", set("funding_allowance_bps", map[string]interface{}{"v": json.Number("1")}), "params.funding_allowance_bps must be a finite JSON number, got object"},
		{"overflowing number", set("entry_threshold_bps", json.Number("1e400")), "params.entry_threshold_bps must be a finite JSON number"},
		{"exit equals entry", set("exit_threshold_bps", json.Number("5")), "params.exit_threshold_bps 5 must be below entry_threshold_bps 5"},
		{"exit below zero", set("exit_threshold_bps", json.Number("-1")), "params.exit_threshold_bps -1 must be at least 0"},
		{"entry above 10000", set("entry_threshold_bps", json.Number("10000.5")), "params.entry_threshold_bps 10000.5 must not exceed 10000"},
		{"adverse band at 10000", set("adverse_band_bps", json.Number("10000")), "params.adverse_band_bps 10000 must be at least 0 and below 10000"},
		{"negative funding allowance", set("funding_allowance_bps", json.Number("-0.01")), "params.funding_allowance_bps -0.01 must be at least 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdin := pairPolicyMutateRequest(t, req, func(r map[string]interface{}) { tc.mutate(r["params"].(map[string]interface{})) })
			res := pairPolicyRunScript(t, root, stdin)
			d, _ := res["decision"].(map[string]interface{})
			if res["status"] != pairPolicyStatusInvalid || d["stage"] != "params" || !strings.Contains(fmt.Sprint(res["error"]), tc.want) {
				t.Fatalf("result %v stage %v %q, want an invalid parameter result naming %q", res["status"], d["stage"], res["error"], tc.want)
			}
		})
	}
	t.Run("boundary values pass the parameter stage", func(t *testing.T) {
		stdin := pairPolicyMutateRequest(t, req, func(r map[string]interface{}) {
			r["params"] = map[string]interface{}{"entry_threshold_bps": json.Number("10000"), "exit_threshold_bps": json.Number("0"),
				"adverse_band_bps": json.Number("9999.99"), "funding_allowance_bps": json.Number("0")}
		})
		res := pairPolicyRunScript(t, root, stdin)
		d, _ := res["decision"].(map[string]interface{})
		if res["status"] != pairPolicyStatusHold || d["stage"] != "entry" || d["params"] == nil {
			t.Fatalf("boundary parameters must reach the economic stage: %v %v", res["status"], d["stage"])
		}
	})
}
