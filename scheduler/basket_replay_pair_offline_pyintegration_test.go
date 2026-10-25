//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type pairOfflineFixture struct {
	t        *testing.T
	repo     string
	inputs   string
	parent   string
	doc      []byte
	t0       int64
	end      int64
	runID    string
	objects  []pairReplayObjectRef
	scenario pairReplayScenario
	maxSteps int
}

type pairOfflineOptions struct {
	sealed    bool
	failures  []pairReplayFailureScript
	runID     string
	quoteEnd  int64
	mutate    func(root map[string]interface{})
	durationS int64
	noHLFund  bool
}

func pairOfflineRealPath(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func pairOfflineWrite(t *testing.T, dir, name string, v interface{}) pairReplayObjectRef {
	t.Helper()
	var blob []byte
	switch b := v.(type) {
	case []byte:
		blob = b
	default:
		var err error
		if blob, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, name), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return pairReplayObjectRef{Path: name, SHA256: sha256Hex(blob), SizeBytes: int64(len(blob))}
}

func pairOfflineBooks(atMs, t0 int64, okx bool) ([][2]string, [][2]string) {
	converged := atMs >= t0+150_000
	ladder := func(start float64, step float64, sz string) [][2]string {
		var out [][2]string
		for i := 0; i < 6; i++ {
			out = append(out, [2]string{strconv.FormatFloat(start+step*float64(i), 'f', -1, 64), sz})
		}
		return out
	}
	switch {
	case okx && converged:
		return ladder(59999.9, -0.1, "30"), ladder(60000, 0.1, "30")
	case okx:
		return ladder(62000, -0.1, "30"), ladder(62000.1, 0.1, "30")
	case converged:
		return ladder(60000, -1, "0.5"), ladder(60002, 1, "0.5")
	}
	return ladder(59998, -1, "0.5"), ladder(60000, 1, "0.5")
}

func pairOfflineCandles(t0 int64) [][]float64 {
	hour := int64(3_600_000)
	open := t0 - t0%hour - 399*hour
	var rows [][]float64
	for i := int64(0); i < 400; i++ {
		px := 60000 * (1 + 0.003*math.Sin(float64(i)/4))
		rows = append(rows, []float64{float64(open + i*hour), px, px * 1.004, px * 0.996, px * 1.001, 100 + float64(i)})
	}
	return rows
}

func newPairOfflineFixture(t *testing.T, o pairOfflineOptions) *pairOfflineFixture {
	t.Helper()
	repo := basketPyRoot(t)
	f := &pairOfflineFixture{t: t, repo: repo, inputs: pairOfflineRealPath(t, t.TempDir()), parent: pairOfflineRealPath(t, t.TempDir()), runID: o.runID}
	if f.runID == "" {
		f.runID = "pair-run-1"
	}
	f.t0 = pairTestStart().Add(-2 * time.Minute).UnixMilli()
	dur := o.durationS
	if dur == 0 {
		dur = 360
	}
	f.end = f.t0 + dur*1000
	doc := pairTestDoc(t, f.parent, func(root map[string]interface{}) {
		delete(root, "db_file")
		delete(root, "log_dir")
		p := pairTestPair(root)
		p["policy"] = pairPolicyCrossExchangePerpArbitrage
		p["params"] = map[string]interface{}{"entry_threshold_bps": 40, "exit_threshold_bps": 30, "adverse_band_bps": 4, "funding_allowance_bps": 0}
		if !o.sealed {
			pairTestMemberEntry(root, 0)["source"] = pairSourcePublicSnapshot
		}
		if o.mutate != nil {
			o.mutate(root)
		}
	})
	f.doc = doc
	evidence := pairTestEvidenceBytes(t, doc, nil)
	keys := map[string]pairEvidenceKey{}
	for _, k := range pairTestMemberKeys(doc) {
		keys[k.MemberID] = k
	}
	fd := pairTestFundingDocument(doc)
	hourMs := pairTestStart().UnixMilli()
	fd.Rates = []pairFundingRate{pfRate(keys[pairTestHL], pairFundingRuleHyperliquid, hourMs, "0.0000125")}
	fd.Prices = []pairFundingPrice{pfPrice(keys[pairTestHL], pairFundingPriceOracle, hourMs, "59999.25")}
	if o.noHLFund {
		pairFundingAutoCoverage(&fd, keys, pairTestHL)
	} else {
		pairFundingAutoCoverage(&fd, keys)
	}
	funding := pairTestFundingSeal(t, &fd)
	add := func(kind, member, name string, v interface{}) {
		ref := pairOfflineWrite(t, f.inputs, name, v)
		ref.Kind, ref.MemberID = kind, member
		f.objects = append(f.objects, ref)
	}
	add(pairReplayKindEvidence, "", "evidence.json", evidence)
	add(pairReplayKindFunding, "", "funding.json", funding)
	quoteEnd := o.quoteEnd
	if quoteEnd == 0 {
		quoteEnd = f.end + 60_000
	}
	hl := pairReplayQuoteDoc{V: 1, MemberID: pairTestHL}
	okx := pairReplayQuoteDoc{V: 1, MemberID: pairTestOKX}
	book := pairReplayBookDoc{V: 1, MemberID: pairTestHL, Instance: "recorded-feed", CoverageFromMs: f.t0 - 120_000, CoverageThroughMs: quoteEnd + 200}
	for ts := f.t0 - 120_000; ts <= quoteEnd; ts += 1000 {
		receipt := ts + 200
		ob, oa := pairOfflineBooks(ts, f.t0, true)
		okx.Records = append(okx.Records, pairReplayQuoteRecord{ReceiptMs: receipt, Payload: pairFixtureOKXPayload("BTC-USDT-SWAP", 5, receipt-20, ob, oa)})
		hb, ha := pairOfflineBooks(ts, f.t0, false)
		hl.Records = append(hl.Records, pairReplayQuoteRecord{ReceiptMs: receipt, Payload: pairFixtureHLPublicPayload("BTC", 5, receipt-20, hb, ha)})
		book.Records = append(book.Records, pairReplayBookRecord{EventMs: receipt - 20, RecvMs: receipt, Bids: pairFixtureBookLevels(hb, 5), Asks: pairFixtureBookLevels(ha, 5)})
	}
	if o.sealed {
		add(pairReplayKindBooks, pairTestHL, "hl-books.json", book)
	} else {
		add(pairReplayKindQuotes, pairTestHL, "hl-quotes.json", hl)
	}
	add(pairReplayKindQuotes, pairTestOKX, "okx-quotes.json", okx)
	candles := pairOfflineCandles(f.t0)
	add(pairReplayKindCandles, pairTestHL, "hl-candles.json", pairReplayCandleDoc{V: 1, MemberID: pairTestHL, Timeframe: "1h", Rows: candles})
	add(pairReplayKindCandles, pairTestOKX, "okx-candles.json", pairReplayCandleDoc{V: 1, MemberID: pairTestOKX, Timeframe: "1h", Rows: candles})
	f.scenario = pairReplayScenario{SubmitLatencyMs: 0, ResponseLatencyMs: 200, ReceiptModel: pairReplayReceiptModel, VenueFailures: o.failures}
	if f.scenario.VenueFailures == nil {
		f.scenario.VenueFailures = []pairReplayFailureScript{}
	}
	f.maxSteps = 5000
	return f
}

func (f *pairOfflineFixture) spec(mode, stateDir string) *pairReplaySpec {
	return &pairReplaySpec{Protocol: basketReplayProtocolName, Version: pairReplayProtocolVersion, Mode: mode,
		Run:            basketReplayRunSpec{RunID: f.runID, StateDir: stateDir, Source: map[string]string{"quotes": "fixture recorded books", "evidence": "fixture schedule"}},
		ResearchConfig: json.RawMessage(f.doc), InputManifest: pairReplayInputManifest{Root: f.inputs, Objects: f.objects}, Scenario: f.scenario,
		Bounds: pairReplayBounds{Start: time.UnixMilli(f.t0).UTC().Format(time.RFC3339Nano), End: time.UnixMilli(f.end).UTC().Format(time.RFC3339Nano), MaxSteps: f.maxSteps}}
}

func (f *pairOfflineFixture) stateDir(name string) string {
	return filepath.Join(f.parent, name)
}

type pairReplayProc struct {
	Out    pairReplayOutput
	Raw    []byte
	Code   int
	Stderr string
}

func runPairReplayAdapter(t *testing.T, root string, spec interface{}, env []string, args ...string) pairReplayProc {
	t.Helper()
	bin := pyintegrationBinary(t)
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("spec-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"basket-replay", "--spec", path}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	p := pairReplayProc{Raw: stdout.Bytes(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		p.Code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("adapter did not run: %v", err)
	}
	if p.Code != basketReplayExitFault && p.Code != basketReplayExitUsage {
		dec := json.NewDecoder(bytes.NewReader(p.Raw))
		if err := dec.Decode(&p.Out); err != nil {
			t.Fatalf("adapter stdout is not one protocol JSON document (exit %d): %v\nstdout=%s\nstderr tail=%s", p.Code, err, p.Raw, tail(p.Stderr, 6000))
		}
		if dec.More() {
			t.Fatalf("adapter stdout carries more than one JSON document")
		}
	}
	return p
}

func pairOfflineRequireComplete(t *testing.T, label string, p pairReplayProc) {
	t.Helper()
	if p.Code != 0 || !p.Out.MechanicallyComplete || p.Out.Status != basketReplayStatusComplete || p.Out.Scored || p.Out.PromotionEligible || p.Out.Comparison == nil || !p.Out.Comparison.Eligible {
		t.Fatalf("%s: want a mechanically complete pair run with exit 0 and both flags false, got exit %d status %s complete=%v reasons=%v refusal=%+v error=%q comparison=%+v\nstderr tail=%s",
			label, p.Code, p.Out.Status, p.Out.MechanicallyComplete, p.Out.IncompleteReasons, p.Out.Refusal, p.Out.Error, p.Out.Comparison, tail(p.Stderr, 6000))
	}
}

func pairOfflineSection(t *testing.T, out pairReplayOutput, name string) []map[string]interface{} {
	t.Helper()
	raw, ok := out.Export[name]
	if !ok {
		t.Fatalf("export has no %s section", name)
	}
	blob, _ := json.Marshal(raw)
	var rows []map[string]interface{}
	if err := json.Unmarshal(blob, &rows); err != nil {
		t.Fatalf("export section %s: %v", name, err)
	}
	return rows
}

func pairOfflineDiff(t *testing.T, a, b pairReplayOutput) string {
	t.Helper()
	var names []string
	for k := range a.Export {
		names = append(names, k)
	}
	sort.Strings(names)
	var diffs []string
	for _, n := range names {
		ra, _ := json.Marshal(a.Export[n])
		rb, _ := json.Marshal(b.Export[n])
		if !bytes.Equal(ra, rb) {
			var la, lb []json.RawMessage
			_ = json.Unmarshal(ra, &la)
			_ = json.Unmarshal(rb, &lb)
			for i := 0; i < len(la) || i < len(lb); i++ {
				var x, y string
				if i < len(la) {
					x = string(la[i])
				}
				if i < len(lb) {
					y = string(lb[i])
				}
				if x != y {
					diffs = append(diffs, fmt.Sprintf("%s[%d]:\n  a=%s\n  b=%s", n, i, x, y))
					break
				}
			}
		}
	}
	return strings.Join(diffs, "\n")
}

func pairOfflineTraceHas(t *testing.T, out pairReplayOutput, phase string) bool {
	t.Helper()
	for _, raw := range out.Steps {
		var rec pairReplayStepRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		for _, p := range rec.Trace {
			if p == phase {
				return true
			}
		}
	}
	return false
}

func pairOfflineSteps(t *testing.T, out pairReplayOutput) []pairReplayStepRecord {
	t.Helper()
	var recs []pairReplayStepRecord
	for _, raw := range out.Steps {
		var rec pairReplayStepRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestBasketReplayPairOfflineRunsTheRealOwnersToAMatchedSpreadAndBack(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{})
	p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("run")), nil)
	pairOfflineRequireComplete(t, "uninterrupted run", p)
	actions := pairOfflineSection(t, p.Out, "basket_member_actions")
	purposes := map[string]int{}
	for _, a := range actions {
		purposes[fmt.Sprint(a["purpose"])]++
		if a["status"] != basketActionCommitted {
			t.Fatalf("every action must end committed, got %v", a)
		}
	}
	if purposes[pairPurposeEntry] < 2 || purposes[pairPurposeExit] < 2 {
		t.Fatalf("the run must open both legs and exit both legs, got %v", purposes)
	}
	funding := 0
	for _, tr := range pairOfflineSection(t, p.Out, "trades") {
		if tr["trade_type"] == TradeTypeFunding {
			funding++
		}
	}
	if funding != 1 {
		t.Fatalf("the Hyperliquid member must pay the frozen hourly funding once while matched, got %d funding rows", funding)
	}
	matched, managed, compared := 0, 0, 0
	var prev *pairReplayStepRecord
	recs := pairOfflineSteps(t, p.Out)
	for i := range recs {
		rec := recs[i]
		if i > 0 {
			prev = &recs[i-1]
		}
		if strings.Join(rec.Trace, ",") != "evidence,funding,prepare_risk,risk,manage,step" && strings.Join(rec.Trace, ",") != "evidence,funding,prepare_risk,risk,step" {
			t.Fatalf("step %d must follow the host order, got %v", rec.Ordinal, rec.Trace)
		}
		if rec.Managed {
			managed++
		}
		if len(rec.Members) != 2 || rec.Members[0].SignedQty == 0 || rec.Members[1].SignedQty == 0 || rec.Members[0].View == nil || rec.Members[1].View == nil {
			continue
		}
		hl, okx := rec.Members[0], rec.Members[1]
		if hl.View.MarkUSD == okx.View.MarkUSD || okx.View.ContractSize != 0.01 || hl.View.ContractSize != 1 {
			t.Fatalf("step %d members must carry distinct own marks and their own contract sizes: %+v %+v", rec.Ordinal, hl.View, okx.View)
		}
		want := hl.SignedQty*hl.View.MarkUSD + okx.SignedQty*okx.View.MarkUSD
		if got := rec.Owners.AssetDeltas["BTC"]; math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Fatalf("step %d computeAssetDeltas must net each member at its own mark: got %v want %v", rec.Ordinal, got, want)
		}
		gross := math.Abs(hl.SignedQty*hl.View.MarkUSD) + math.Abs(okx.SignedQty*okx.View.MarkUSD)
		if got := rec.Owners.PortfolioNotional; math.Abs(got-gross) > 1e-6*gross {
			t.Fatalf("step %d PortfolioNotional must value each member at its own mark: got %v want %v", rec.Ordinal, got, gross)
		}
		if len(rec.Risk) != 1 || rec.Risk[0].PairValuation != "" || !rec.Risk[0].EquityTrusted {
			t.Fatalf("step %d partition risk must read complete member views: %+v", rec.Ordinal, rec.Risk)
		}
		if prev != nil && len(prev.Members) == 2 && prev.Members[0].Cash == hl.Cash && prev.Members[1].Cash == okx.Cash && prev.Members[0].SignedQty == hl.SignedQty && prev.Members[1].SignedQty == okx.SignedQty {
			if sum := rec.Owners.PortfolioValue[pairTestHL] + rec.Owners.PortfolioValue[pairTestOKX]; math.Abs(rec.Risk[0].TotalPV-sum) > 1e-6*sum {
				t.Fatalf("step %d partition risk and PortfolioValue must agree on unchanged books: %v vs %v", rec.Ordinal, rec.Risk[0].TotalPV, sum)
			}
			compared++
		}
		if rec.Owners.PerpsMargin <= 0 || rec.Owners.MarginStatus != "" || rec.Owners.ExposureCapValuation != "" || rec.Owners.NotionalCapHeld {
			t.Fatalf("step %d margin, exposure and notional readings must be complete on the views: %+v", rec.Ordinal, rec.Owners)
		}
		matched++
	}
	if matched == 0 || managed != 7 || compared == 0 {
		t.Fatalf("want matched steps valued by each owner, risk compared on unchanged books and seven management cycles, got %d matched, %d compared and %d managed", matched, compared, managed)
	}
	var status map[string]map[string]interface{}
	if err := json.Unmarshal(p.Out.ServerStatus["strategies"], &status); err != nil {
		t.Fatal(err)
	}
	last := pairOfflineSteps(t, p.Out)
	final := last[len(last)-1]
	for _, m := range final.Members {
		if pv, _ := status[m.StrategyID]["portfolio_value"].(float64); math.Abs(pv-m.Value) > 1e-9*math.Max(1, m.Value) || m.SignedQty != 0 {
			t.Fatalf("the server status owner must report the flat member %s at its book value %v, got %v", m.StrategyID, m.Value, status[m.StrategyID])
		}
	}
	t.Logf("comparison %s, %d steps, actions %v", p.Out.Comparison.Hash, len(p.Out.Steps), purposes)
}

func TestBasketReplayPairOfflineFaultsResumeToEqualResults(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{})
	base := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("base")), nil)
	pairOfflineRequireComplete(t, "uninterrupted run", base)
	for i, fault := range []string{"exit-after-collection:1", "exit-after-envelope-freeze:2", "exit-after-acceptance:1", "exit-after-acceptance:2", "exit-after-book-write:40", "exit-after-book-commit:3", "exit-after-management:2"} {
		t.Run(fault, func(t *testing.T) {
			dir := f.stateDir(fmt.Sprintf("fault-%d", i))
			crash := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--fault", fault)
			if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault "+fault+"\n") {
				t.Fatalf("the fault must exit 75 at its boundary and print it, got exit %d, stderr tail %s", crash.Code, tail(crash.Stderr, 3000))
			}
			resumed := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil)
			pairOfflineRequireComplete(t, "resumed run", resumed)
			if !resumed.Out.Run.Resumed {
				t.Fatal("the second process must report a resume")
			}
			if strings.HasPrefix(fault, "exit-after-collection") && !strings.Contains(resumed.Stderr, "the saved checked collection completed the reserved slot after a restart") {
				t.Fatalf("the resume must complete the reserved slot from the saved collection, stderr tail %s", tail(resumed.Stderr, 3000))
			}
			if strings.HasPrefix(fault, "exit-after-book-write") && !pairOfflineTraceHas(t, resumed.Out, "risk_restored") {
				t.Fatal("a crash inside the final save must resume from the coordinator checkpoint without a second risk or management pass")
			}
			if resumed.Out.Comparison.Hash != base.Out.Comparison.Hash {
				t.Fatalf("the resumed projection differs from the uninterrupted run:\n%s", pairOfflineDiff(t, base.Out, resumed.Out))
			}
			for _, rb := range pairOfflineSection(t, resumed.Out, "basket_rebalances") {
				if strings.Contains(fmt.Sprint(rb["hold_reason"]), "restarted") {
					t.Fatalf("a restart turned a slot into a hold: %v", rb)
				}
			}
		})
	}
}

func TestBasketReplayPairOfflineSealedFeedResumesFromASavedCollection(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{sealed: true})
	base := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("base")), nil)
	pairOfflineRequireComplete(t, "sealed uninterrupted run", base)
	dir := f.stateDir("crash")
	crash := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--fault", "exit-after-collection:2")
	if crash.Code != basketReplayExitFault {
		t.Fatalf("want exit 75, got %d", crash.Code)
	}
	resumed := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil)
	pairOfflineRequireComplete(t, "sealed resumed run", resumed)
	if resumed.Out.Comparison.Hash != base.Out.Comparison.Hash {
		t.Fatalf("the sealed resume differs:\n%s", pairOfflineDiff(t, base.Out, resumed.Out))
	}
	for _, rb := range pairOfflineSection(t, base.Out, "basket_rebalances") {
		if rb["snapshot_id"] == "" || rb["generation"] != float64(1) {
			t.Fatalf("a sealed slot must carry the rebuilt seal identity and generation 1: %v", rb)
		}
	}
}

func TestBasketReplayPairOfflineEconomicHoldIsComplete(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-hold", mutate: func(root map[string]interface{}) {
		pairTestPair(root)["params"].(map[string]interface{})["entry_threshold_bps"] = 9000
	}})
	p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("hold")), nil)
	pairOfflineRequireComplete(t, "economic hold run", p)
	if n := len(pairOfflineSection(t, p.Out, "basket_member_actions")); n != 0 {
		t.Fatalf("a policy that always holds sends nothing, got %d actions", n)
	}
	held := 0
	for _, rb := range pairOfflineSection(t, p.Out, "basket_rebalances") {
		if rb["status"] == basketRebalanceHeld {
			held++
		}
	}
	if held == 0 || len(p.Out.Failures) != 0 {
		t.Fatalf("the slots must hold as economic policy outcomes with no infrastructure failure, got %d holds and failures %v", held, p.Out.Failures)
	}
}

func TestBasketReplayPairOfflineProcessFailuresStayIncompleteAfterResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure pairReplayFailureScript
		stage   string
	}{
		{"policy", pairReplayFailureScript{Kind: pairReplayFailPolicy}, pairReplayStagePolicy},
		{"native check", pairReplayFailureScript{Kind: pairReplayFailManagement, MemberID: pairTestOKX}, pairReplayStageManagement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t0 := pairTestStart().Add(-2 * time.Minute)
			tc.failure.At = t0.UTC().Format(time.RFC3339Nano)
			if tc.failure.Kind == pairReplayFailManagement {
				tc.failure.At = t0.Add(time.Minute).UTC().Format(time.RFC3339Nano)
			}
			f := newPairOfflineFixture(t, pairOfflineOptions{failures: []pairReplayFailureScript{tc.failure}})
			dir := f.stateDir("run")
			first := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--max-steps", "120")
			if first.Code != basketReplayExitError || first.Out.MechanicallyComplete || first.Out.Status != basketReplayStatusStopped {
				t.Fatalf("a stopped run is never complete, got exit %d status %s", first.Code, first.Out.Status)
			}
			resumed := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil)
			if resumed.Code != basketReplayExitError || resumed.Out.MechanicallyComplete || resumed.Out.Status != basketReplayStatusIncomplete || resumed.Out.Comparison.Eligible {
				t.Fatalf("a %s process failure must keep the finished run incomplete after the resume, got exit %d status %s reasons %v", tc.name, resumed.Code, resumed.Out.Status, resumed.Out.IncompleteReasons)
			}
			found := false
			for _, fl := range resumed.Out.Failures {
				found = found || fl.Stage == tc.stage
			}
			if !found || resumed.Out.Scored || resumed.Out.PromotionEligible {
				t.Fatalf("the persisted %s failure must survive the resume and both flags stay false: %+v", tc.stage, resumed.Out.Failures)
			}
			if unapplied := pairOfflineUnapplied(resumed.Out); !resumed.Out.Run.Finished || unapplied != nil {
				t.Fatalf("the scripted %s failure fired, so the finished run must not report it unapplied: finished=%v unapplied=%v", tc.name, resumed.Out.Run.Finished, unapplied)
			}
		})
	}
}

func TestBasketReplayPairOfflineScriptedManagementFailureFiresOnceAcrossARestart(t *testing.T) {
	at := pairTestStart().Add(-time.Minute)
	f := newPairOfflineFixture(t, pairOfflineOptions{failures: []pairReplayFailureScript{{Kind: pairReplayFailManagement, MemberID: pairTestOKX, At: at.UTC().Format(time.RFC3339Nano)}}})
	scripted := func(p pairReplayProc) int {
		n := 0
		for _, fl := range p.Out.Failures {
			if fl.Stage == pairReplayStageManagement && fl.MemberID == pairTestOKX && fl.AtMs == at.UnixMilli() && strings.HasPrefix(fl.Cause, pairReplayScenarioManagementCause) {
				n++
			}
		}
		return n
	}
	base := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("base")), nil)
	if base.Out.MechanicallyComplete || scripted(base) != 1 {
		t.Fatalf("an uninterrupted run must record the scripted management failure once and stay incomplete, got complete=%v failures=%+v", base.Out.MechanicallyComplete, base.Out.Failures)
	}
	pass, injectedPass := 0, 0
	for _, raw := range base.Out.Steps {
		var st struct {
			AtMs    int64 `json:"at_ms"`
			Managed bool  `json:"managed"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if st.Managed {
			pass++
			if st.AtMs == at.UnixMilli() {
				injectedPass = pass
			}
		}
	}
	if injectedPass == 0 {
		t.Fatalf("the fixture must manage the members at the scripted time %d", at.UnixMilli())
	}
	dir := f.stateDir("crash")
	fault := fmt.Sprintf("exit-after-management:%d", injectedPass)
	crash := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--fault", fault)
	if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault "+fault+"\n") {
		t.Fatalf("the fault must stop the process after the scripted management pass, got exit %d, stderr tail %s", crash.Code, tail(crash.Stderr, 3000))
	}
	resumed := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil)
	if !resumed.Out.Run.Resumed || resumed.Out.MechanicallyComplete || scripted(resumed) != 1 {
		t.Fatalf("the resume reruns the management pass, so the scripted failure must not fire again: resumed=%v complete=%v failures=%+v", resumed.Out.Run.Resumed, resumed.Out.MechanicallyComplete, resumed.Out.Failures)
	}
	if !resumed.Out.Run.Finished || pairOfflineUnapplied(resumed.Out) != nil {
		t.Fatalf("a scripted failure that fired before the crash is applied after the resume: finished=%v unapplied=%v", resumed.Out.Run.Finished, pairOfflineUnapplied(resumed.Out))
	}
}

func pairOfflineUnapplied(out pairReplayOutput) []string {
	var got []string
	for _, r := range out.IncompleteReasons {
		if strings.Contains(r, "was declared but never applied") {
			got = append(got, r)
		}
	}
	return got
}

func TestBasketReplayPairOfflineUnappliedScriptedFailuresAreReported(t *testing.T) {
	offStep := pairTestStart().Add(-time.Minute + time.Second)
	for _, tc := range []struct {
		name    string
		failure pairReplayFailureScript
		applied bool
	}{
		{"management one second off the management steps", pairReplayFailureScript{Kind: pairReplayFailManagement, MemberID: pairTestOKX, At: offStep.UTC().Format(time.RFC3339Nano)}, false},
		{"submit nth past the entry submits", pairReplayFailureScript{Kind: pairReplayFailSubmit, Purpose: pairPurposeEntry, Nth: 99}, false},
		{"submit of the first entry", pairReplayFailureScript{Kind: pairReplayFailSubmit, Purpose: pairPurposeEntry, Nth: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPairOfflineFixture(t, pairOfflineOptions{failures: []pairReplayFailureScript{tc.failure}})
			p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("run")), nil)
			if !p.Out.Run.Finished {
				t.Fatalf("the run must finish so the scripted failure is judged, got stop reason %q", p.Out.Run.StopReason)
			}
			if tc.failure.Kind == pairReplayFailManagement {
				for _, raw := range p.Out.Steps {
					var st struct {
						AtMs    int64 `json:"at_ms"`
						Managed bool  `json:"managed"`
					}
					if err := json.Unmarshal(raw, &st); err != nil {
						t.Fatal(err)
					}
					if st.Managed && st.AtMs == offStep.UnixMilli() {
						t.Fatalf("the fixture manages the members at %d, so the script is not off the management steps", st.AtMs)
					}
				}
			}
			unapplied := pairOfflineUnapplied(p.Out)
			if tc.applied {
				fired := false
				for _, fl := range p.Out.Failures {
					fired = fired || (fl.Stage == pairReplayStageScenario && fl.Kind == pairReplayFailSubmit && strings.HasPrefix(fl.Cause, pairReplayScenarioSubmitCause(tc.failure.Purpose, tc.failure.Nth)))
				}
				if !fired || unapplied != nil {
					t.Fatalf("a scripted submit failure that fired must leave its row and no unapplied reason: fired=%v unapplied=%v failures=%+v", fired, unapplied, p.Out.Failures)
				}
				return
			}
			if p.Out.MechanicallyComplete || p.Code != basketReplayExitError || len(unapplied) != 1 || !strings.Contains(unapplied[0], "scenario.venue_failures[0] "+tc.failure.Kind) {
				t.Fatalf("a scripted failure that never fired must keep the finished run incomplete with one unapplied reason, got exit %d complete=%v reasons=%v", p.Code, p.Out.MechanicallyComplete, p.Out.IncompleteReasons)
			}
		})
	}
}

func TestBasketReplayPairOfflineMissingEvidenceIsNeverMechanicalSuccess(t *testing.T) {
	t.Run("quotes end while the spread is matched", func(t *testing.T) {
		t0 := pairTestStart().Add(-2 * time.Minute).UnixMilli()
		f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-stale", quoteEnd: t0 + 100_000})
		p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("stale")), nil)
		if p.Code != basketReplayExitError || p.Out.MechanicallyComplete || len(p.Out.IncompleteReasons) == 0 {
			t.Fatalf("an incomplete valuation or unfinished exit at the declared end must not succeed, got exit %d %v", p.Code, p.Out.IncompleteReasons)
		}
	})
	t.Run("funding coverage is missing while a member holds a position", func(t *testing.T) {
		f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-gap", noHLFund: true})
		p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("gap")), nil)
		gap := false
		for _, why := range p.Out.IncompleteReasons {
			gap = gap || strings.Contains(why, "funding")
		}
		if p.Code != basketReplayExitError || p.Out.MechanicallyComplete || !gap {
			t.Fatalf("missing funding coverage is never settled as zero and never mechanical success, got exit %d %v", p.Code, p.Out.IncompleteReasons)
		}
	})
	t.Run("the step budget runs out", func(t *testing.T) {
		f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-budget"})
		f.maxSteps = 30
		p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("budget")), nil)
		if p.Code != basketReplayExitError || p.Out.MechanicallyComplete || p.Out.Run.Finished {
			t.Fatalf("an exhausted step budget is an unfinished bounded run, got exit %d finished %v", p.Code, p.Out.Run.Finished)
		}
	})
	t.Run("a failed required save", func(t *testing.T) {
		probe := newPairOfflineFixture(t, pairOfflineOptions{})
		base := runPairReplayAdapter(t, probe.repo, probe.spec(basketReplayModeNew, probe.stateDir("base")), nil)
		pairOfflineRequireComplete(t, "baseline", base)
		at := time.UnixMilli(probe.t0 + 60_000).UTC().Format(time.RFC3339Nano)
		f := newPairOfflineFixture(t, pairOfflineOptions{failures: []pairReplayFailureScript{{Kind: pairReplayFailSave, At: at}}})
		dir := f.stateDir("save")
		p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil)
		if p.Code != basketReplayExitError || p.Out.MechanicallyComplete || !strings.Contains(p.Out.Run.StopReason, "required state save") {
			t.Fatalf("a failed final save stops the run incomplete, got exit %d stop %q", p.Code, p.Out.Run.StopReason)
		}
		resumed := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil)
		pairOfflineRequireComplete(t, "resume after the failed save", resumed)
		if resumed.Out.Export == nil || len(pairOfflineSection(t, resumed.Out, "basket_member_actions")) != len(pairOfflineSection(t, base.Out, "basket_member_actions")) {
			t.Fatal("the resumed run must reach the same actions as an uninterrupted run")
		}
	})
}

func TestBasketReplayPairOfflineFutureInputsCannotChangeEarlierDecisions(t *testing.T) {
	a := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-future"})
	b := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-future", mutate: nil})
	cut := a.t0 + 200_000
	var doc pairReplayQuoteDoc
	for i, o := range b.objects {
		if o.Kind != pairReplayKindQuotes || o.MemberID != pairTestOKX {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(b.inputs, o.Path))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(blob, &doc); err != nil {
			t.Fatal(err)
		}
		for j, r := range doc.Records {
			if r.ReceiptMs > cut {
				doc.Records[j].Payload = pairFixtureOKXPayload("BTC-USDT-SWAP", 5, r.ReceiptMs-20, [][2]string{{"58000", "30"}, {"57999.9", "30"}, {"57999.8", "30"}, {"57999.7", "30"}, {"57999.6", "30"}, {"57999.5", "30"}},
					[][2]string{{"58000.1", "30"}, {"58000.2", "30"}, {"58000.3", "30"}, {"58000.4", "30"}, {"58000.5", "30"}, {"58000.6", "30"}})
			}
		}
		ref := pairOfflineWrite(t, b.inputs, o.Path, doc)
		b.objects[i].SHA256, b.objects[i].SizeBytes = ref.SHA256, ref.SizeBytes
	}
	pa := runPairReplayAdapter(t, a.repo, a.spec(basketReplayModeNew, a.stateDir("a")), nil)
	pb := runPairReplayAdapter(t, b.repo, b.spec(basketReplayModeNew, b.stateDir("b")), nil)
	if pa.Out.Run == nil || pb.Out.Run == nil {
		t.Fatalf("both runs must produce a result: %+v %+v", pa.Out.Refusal, pb.Out.Refusal)
	}
	early := func(out pairReplayOutput) string {
		var parts []string
		for _, raw := range out.Steps {
			var rec pairReplayStepRecord
			_ = json.Unmarshal(raw, &rec)
			if rec.AtMs <= cut {
				rec.Trace = nil
				for i := range rec.Risk {
					rec.Risk[i].TotalPV, rec.Risk[i].TotalNotional = 0, 0
				}
				blob, _ := json.Marshal(rec)
				parts = append(parts, string(blob))
			}
		}
		for _, row := range pairOfflineSection(t, out, "basket_rebalances") {
			if ms, _ := row["deadline_ms"].(float64); int64(ms) <= cut {
				blob, _ := json.Marshal(row)
				parts = append(parts, string(blob))
			}
		}
		return strings.Join(parts, "\n")
	}
	if early(pa.Out) != early(pb.Out) {
		t.Fatal("a change to records received after the cut changed an earlier step, valuation or decision")
	}
	if pa.Out.Comparison.Hash == pb.Out.Comparison.Hash {
		t.Fatal("the changed future records must change the later run, or the proof shows nothing")
	}
}

func TestBasketReplayPairOfflineUsageRefusalsAndBlockedNetwork(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-net"})
	dir := f.stateDir("usage")
	if p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--fault", "exit-after-submit:1"); p.Code != basketReplayExitUsage {
		t.Fatalf("a version-1 fault on a version-2 specification is a usage error, got %d", p.Code)
	}
	if p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, dir), nil, "--fault", "exit-after-nothing:1"); p.Code != basketReplayExitUsage {
		t.Fatalf("an unknown fault is a usage error, got %d", p.Code)
	}
	if p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeResume, dir), nil); p.Code != basketReplayExitRefused || p.Out.Refusal == nil {
		t.Fatalf("resuming a run that never started is refused, got %d", p.Code)
	}
	bad := f.spec(basketReplayModeNew, dir)
	bad.Bounds.MaxSteps = 0
	if p := runPairReplayAdapter(t, f.repo, bad, nil); p.Code != basketReplayExitRefused || p.Out.Refusal == nil || p.Out.Refusal.Reason != "spec_invalid" {
		t.Fatalf("an invalid specification is refused, got %d %+v", p.Code, p.Out.Refusal)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("usage errors and pure refusals create no directory: %v", err)
	}
	empty := f.stateDir("empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, empty), nil); p.Code != basketReplayExitRefused || p.Out.Refusal.Reason != "state_exists" {
		t.Fatalf("a new run refuses an existing empty directory, got %d %+v", p.Code, p.Out.Refusal)
	}
	blocked := []string{"GO_TRADER_EGRESS_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9", "NO_PROXY=", "no_proxy="}
	p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("offline")), blocked)
	pairOfflineRequireComplete(t, "run with every exchange route blocked and no deployed configuration", p)
	cmd := exec.Command(filepath.Join(f.repo, ".venv", "bin", "python3"), "-c", "import urllib.request; urllib.request.urlopen('https://api.hyperliquid.xyz/info', timeout=5)")
	cmd.Env = append(os.Environ(), blocked...)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("an intentional exchange request under the same environment must fail, it succeeded: %s", out)
	}
}

func TestBasketReplayPairOfflineDriverReportsAMechanicalResult(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-driver"})
	var objs []map[string]interface{}
	for _, o := range f.objects {
		m := map[string]interface{}{"kind": o.Kind, "path": o.Path, "sha256": o.SHA256}
		if o.MemberID != "" {
			m["member_id"] = o.MemberID
		}
		objs = append(objs, m)
	}
	cfgPath := filepath.Join(f.inputs, "research-config.json")
	if err := os.WriteFile(cfgPath, f.doc, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": f.runID, "state_dir": f.stateDir("driver"),
		"source": map[string]string{"quotes": "fixture"}, "research_config": map[string]string{"path": "research-config.json", "sha256": sha256Hex(f.doc)},
		"objects": objs, "scenario": f.scenario, "bounds": f.spec(basketReplayModeNew, "/x").Bounds}
	blob, _ := json.Marshal(plan)
	planPath := filepath.Join(f.inputs, "plan.json")
	if err := os.WriteFile(planPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "result.json")
	cmd := exec.Command(filepath.Join(f.repo, ".venv", "bin", "python3"), filepath.Join(f.repo, "backtest", "basket_replay.py"), "--plan", planPath, "--binary", pyintegrationBinary(t), "--out", outPath)
	cmd.Dir = f.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the driver must exit 0 for a mechanically complete pair run: %v\n%s", err, tail(string(out), 4000))
	}
	var rec map[string]interface{}
	resBlob, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resBlob, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["mechanically_complete"] != true || rec["scored"] != false || rec["promotion_eligible"] != false || rec["adapter_exit_code"] != float64(0) {
		t.Fatalf("the pair result record must be mechanically complete with scored and promotion_eligible false: %v", rec)
	}
}

func TestBasketReplayPairOfflineDriverRefusalKeepsThePlanVersion(t *testing.T) {
	repo := basketPyRoot(t)
	pairPlan := func(path string) map[string]interface{} {
		return map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": "pair-refused", "state_dir": filepath.Join(t.TempDir(), "state"),
			"source": map[string]string{"quotes": "fixture"}, "research_config": map[string]string{"path": "research-config.json", "sha256": sha256Hex([]byte("{}"))},
			"objects": []map[string]string{{"kind": "quotes", "member_id": pairTestHL, "path": path, "sha256": sha256Hex([]byte("x"))}}, "scenario": map[string]interface{}{}, "bounds": map[string]interface{}{}}
	}
	noBounds := pairPlan("quotes.jsonl")
	delete(noBounds, "bounds")
	for _, tc := range []struct {
		name    string
		plan    map[string]interface{}
		version float64
		pair    bool
	}{
		{"version 2 without bounds", noBounds, 2, true},
		{"version 2 with an escaping object path", pairPlan("../x"), 2, true},
		{"version 1 with an unknown key", map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 1, "unknown_key": true}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			blob, _ := json.Marshal(tc.plan)
			planPath := filepath.Join(dir, "plan.json")
			if err := os.WriteFile(planPath, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(dir, "result.json")
			cmd := exec.Command(filepath.Join(repo, ".venv", "bin", "python3"), filepath.Join(repo, "backtest", "basket_replay.py"), "--plan", planPath, "--binary", filepath.Join(dir, "unused"), "--out", outPath)
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
				t.Fatalf("a refused plan must exit 3, got %v\n%s", err, tail(string(out), 4000))
			}
			resBlob, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			var rec map[string]interface{}
			if err := json.Unmarshal(resBlob, &rec); err != nil {
				t.Fatal(err)
			}
			complete, has := rec["mechanically_complete"]
			if rec["version"] != tc.version || rec["status"] != "refused" || rec["scored"] != false || rec["promotion_eligible"] != false || has != tc.pair || (tc.pair && complete != false) {
				t.Fatalf("the refused record must keep the plan version %v (mechanically_complete false only for version 2), got %v", tc.version, rec)
			}
		})
	}
}

func TestBasketReplayPairOfflineKillSwitchClosesEachMemberAtItsOwnMark(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-kill", mutate: func(root map[string]interface{}) {
		root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 0.03}
	}})
	p := runPairReplayAdapter(t, f.repo, f.spec(basketReplayModeNew, f.stateDir("kill")), nil)
	if p.Out.Run == nil || p.Out.Scored || p.Out.PromotionEligible {
		t.Fatalf("the run must report a pair result with both flags false: %+v", p.Out)
	}
	fired := false
	views := map[int64]map[string]float64{}
	for _, rec := range pairOfflineSteps(t, p.Out) {
		for _, r := range rec.Risk {
			fired = fired || r.KillSwitchFired
		}
		views[rec.AtMs] = map[string]float64{}
		for _, m := range rec.Members {
			if m.View != nil {
				views[rec.AtMs][m.StrategyID] = m.View.MarkUSD
			}
		}
	}
	if !fired {
		t.Fatal("the partition kill switch must fire on the configured drawdown")
	}
	var progress pairProgress
	contracts := pairOfflineSection(t, p.Out, "basket_contracts")
	if len(contracts) != 1 || json.Unmarshal([]byte(fmt.Sprint(contracts[0]["progress_json"])), &progress) != nil {
		t.Fatalf("the pair progress must be exported: %v", contracts)
	}
	prices := map[string]float64{}
	for _, c := range progress.PaperCloses {
		if c.Status != pairPaperCloseCommitted || c.Trigger != "kill_switch" || c.PriceUSD != views[c.CloseAtMs][c.MemberID] {
			t.Fatalf("the paper kill switch must commit each member close at that member's own view mark %v: %+v", views[c.CloseAtMs][c.MemberID], c)
		}
		prices[c.MemberID] = c.PriceUSD
	}
	if len(prices) != 2 || prices[pairTestHL] == prices[pairTestOKX] {
		t.Fatalf("both members must close at their own distinct marks, got %v", prices)
	}
}
