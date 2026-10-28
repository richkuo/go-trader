//go:build pyintegration

package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type pairPaperFixture struct {
	t        *testing.T
	repo     string
	inputs   string
	parent   string
	doc      []byte
	runID    string
	seed     pairReplayObjectRef
	config   string
	fixture  string
	callLog  string
	clock    string
	gate     string
	fromMs   int64
	through  int64
	drain    int64
	scenario pairPaperScenario
	maxSteps int
}

type pairPaperOptions2 struct {
	runID        string
	collectS     int64
	drainS       int64
	convergeS    int64
	interval     int
	capRefresh   int64
	minuteAfterS int64
	mutate       func(root map[string]interface{})
	fixtureMore  map[string]interface{}
}

func pairPaperLimits() map[string]interface{} {
	l := pairTestLimits()
	l["decision_cadence_ms"] = 2000
	l["acceptance_deadline_ms"] = 10000
	l["recovery_age_ms"] = 140000
	l["retry_initial_ms"] = 1000
	l["retry_max_ms"] = 8000
	l["quote_age_ms"] = 9000
	l["collection_timeout_ms"] = 8000
	l["receive_skew_ms"] = 8000
	return l
}

func newPairPaperFixture(t *testing.T, o pairPaperOptions2) *pairPaperFixture {
	t.Helper()
	repo := basketPyRoot(t)
	f := &pairPaperFixture{t: t, repo: repo, inputs: pairOfflineRealPath(t, t.TempDir()), parent: pairOfflineRealPath(t, t.TempDir()), runID: o.runID}
	if f.runID == "" {
		f.runID = "paper-run-1"
	}
	collect, drain, converge := o.collectS, o.drainS, o.convergeS
	if collect == 0 {
		collect = 50
	}
	if drain == 0 {
		drain = 30
	}
	if converge == 0 {
		converge = 20
	}
	interval := o.interval
	if interval == 0 {
		interval = 10
	}
	f.clock = filepath.Join(pairOfflineRealPath(t, t.TempDir()), "clock")
	if err := os.WriteFile(f.clock, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	f.gate = filepath.Join(filepath.Dir(f.clock), "gate")
	if err := os.MkdirAll(f.gate, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(2 * time.Second).UnixMilli()
	if o.minuteAfterS > 0 {
		m := (start/60000 + 1) * 60000
		for m-o.minuteAfterS*1000 < start {
			m += 60000
		}
		start = m - o.minuteAfterS*1000
	}
	f.fromMs, f.through, f.drain = start, start+collect*1000, start+(collect+drain)*1000
	doc := pairTestDoc(t, f.parent, func(root map[string]interface{}) {
		delete(root, "db_file")
		delete(root, "log_dir")
		root["interval_seconds"] = interval
		p := pairTestPair(root)
		p["policy"] = pairPolicyCrossExchangePerpArbitrage
		p["params"] = map[string]interface{}{"entry_threshold_bps": 40, "exit_threshold_bps": 30, "adverse_band_bps": 4, "funding_allowance_bps": 0}
		p["limits"] = pairPaperLimits()
		pairTestMemberEntry(root, 0)["source"] = pairSourcePublicSnapshot
		if o.mutate != nil {
			o.mutate(root)
		}
	})
	f.doc = doc
	seed := pairTestEvidenceBytes(t, doc, nil)
	f.seed = pairOfflineWrite(t, f.inputs, "seed-evidence.json", seed)
	f.seed.Kind = pairReplayKindEvidence
	f.config = filepath.Join(pairOfflineRealPath(t, t.TempDir()), "research-egress.json")
	if err := os.WriteFile(f.config, []byte(`{"egress":{"proxy_required":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := map[string]interface{}{"start_ms": start, "converge_ms": start + converge*1000, "follow_funding": true,
		"okx_funding": [][]interface{}{{start - 1000, "0.0001"}}, "hl_funding": [][]interface{}{{start + (collect+drain-10)*1000, "0.0000125"}}}
	for k, v := range o.fixtureMore {
		fx[k] = v
	}
	blob, _ := json.Marshal(fx)
	f.fixture = filepath.Join(pairOfflineRealPath(t, t.TempDir()), "fixture.json")
	if err := os.WriteFile(f.fixture, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	f.callLog = filepath.Join(filepath.Dir(f.fixture), "calls.jsonl")
	f.scenario = pairPaperScenario{SubmitLatencyMs: 0, ResponseLatencyMs: 200, ReceiptModel: pairReplayReceiptModel, VenueFailures: []pairReplayFailureScript{},
		PaperPollIntervalMs: 4000, PaperFundingPollMs: (collect + drain - 5) * 1000, PaperCapabilityRefreshMs: o.capRefresh}
	f.maxSteps = 2000
	return f
}

type pairPaperResearchOptions struct {
	runID       string
	fromBefore  int64
	throughAt   int64
	drainAt     int64
	clockAt     int64
	convergeAt  int64
	fixtureMore map[string]interface{}
	mutate      func(root map[string]interface{})
}

func newPairPaperResearchFixture(t *testing.T, o pairPaperResearchOptions) (*pairPaperFixture, int64) {
	t.Helper()
	f := newPairPaperFixture(t, pairPaperOptions2{runID: o.runID, mutate: o.mutate})
	target := (time.Now().Add(120*time.Second).UnixMilli()/3600000 + 1) * 3600000
	if o.fromBefore == 0 {
		o.fromBefore = 60000
	}
	if o.throughAt == 0 {
		o.throughAt = 30000
	}
	if o.drainAt == 0 {
		o.drainAt = 70000
	}
	if o.convergeAt == 0 {
		o.convergeAt = 20000
	}
	f.fromMs, f.through, f.drain = target-o.fromBefore, target+o.throughAt, target+o.drainAt
	policy := pairFundingPolicyV1{Version: pairFundingPolicyVersion, MemberRules: map[string]string{pairTestHL: pairFundingRuleHLReceiptResearch, pairTestOKX: pairFundingRuleOKX},
		WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: f.fromMs, DrainThroughMs: f.drain}
	f.scenario.FundingPolicy = &policy
	f.scenario.PaperFundingPollMs = 10000
	f.scenario.PaperPollIntervalMs = 4000
	fx := map[string]interface{}{"start_ms": f.fromMs, "converge_ms": target + o.convergeAt, "follow_funding": false, "oracle_target_ms": target, "oracle_px": "60010.5",
		"okx_funding": [][]interface{}{{f.fromMs - 1000, "0.0001"}, {target + 40000, "0.0001"}},
		"hl_funding":  [][]interface{}{{target + 3, "0.0000125"}, {target + 65000, "0.0000125"}}}
	for k, v := range o.fixtureMore {
		fx[k] = v
	}
	pairPaperWriteJSON(t, f.fixture, fx)
	clock := f.fromMs - 2000
	if o.clockAt != 0 {
		clock = target + o.clockAt
	}
	if err := os.WriteFile(f.clock, []byte(strconv.FormatInt(clock, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, target
}

func (f *pairPaperFixture) spec(mode, stateDir string) *pairPaperSpec {
	return &pairPaperSpec{Protocol: basketReplayProtocolName, Version: pairReplayProtocolVersion, Mode: mode,
		Run:            basketReplayRunSpec{RunID: f.runID, StateDir: stateDir, Source: map[string]string{"venues": "pair paper fixture", "seed": "fixture schedule"}},
		ResearchConfig: json.RawMessage(f.doc), InputManifest: pairReplayInputManifest{Root: f.inputs, Objects: []pairReplayObjectRef{f.seed}},
		Collection: pairPaperCollection{FromMs: f.fromMs, ThroughMs: f.through, DrainThroughMs: f.drain}, Scenario: f.scenario,
		Bounds: pairReplayBounds{Start: time.UnixMilli(f.fromMs).UTC().Format(time.RFC3339Nano), End: time.UnixMilli(f.drain).UTC().Format(time.RFC3339Nano), MaxSteps: f.maxSteps}}
}

func (f *pairPaperFixture) env(blocked bool) []string {
	env := []string{"PYTHONPATH=" + filepath.Join(f.repo, "scheduler", "testdata", "pair_paper"), "PAIR_PAPER_FIXTURE=" + f.fixture, "PAIR_PAPER_CALL_LOG=" + f.callLog}
	if blocked {
		env = append(env, "PAIR_PAPER_BLOCK_NETWORK=1")
	} else {
		env = append(env, pairPaperTestClockEnv+"="+f.clock)
		if f.gate != "" {
			env = append(env, pairPaperClockGateEnv+"="+f.gate)
		}
	}
	return env
}

type pairPaperProc struct {
	Out    pairReplayOutput
	Raw    []byte
	Code   int
	Stderr string
}

func runPairPaperAdapter(t *testing.T, f *pairPaperFixture, spec interface{}, env []string, args ...string) pairPaperProc {
	t.Helper()
	bin := pyintegrationBinary(t)
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("paper-spec-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"basket-replay-paper", "--spec", path}, args...)
	hasConfig := false
	for _, a := range args {
		if a == "--config" || strings.HasPrefix(a, "--config=") {
			hasConfig = true
		}
	}
	if !hasConfig {
		argv = append(argv, "--config", f.config)
	}
	cmd := exec.Command(bin, argv...)
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(), env...)
	if pairPaperRaceEnabled {
		cmd.Env = append(cmd.Env, "GORACE=halt_on_error=1 exitcode=66")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	p := pairPaperProc{Raw: stdout.Bytes(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		p.Code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("paper adapter did not run: %v", err)
	}
	if p.Code == 66 || strings.Contains(p.Stderr, "WARNING: DATA RACE") {
		t.Fatalf("paper child data race: exit %d\n%s", p.Code, tail(p.Stderr, 8000))
	}
	if p.Code != basketReplayExitFault && p.Code != basketReplayExitUsage {
		dec := json.NewDecoder(bytes.NewReader(p.Raw))
		if err := dec.Decode(&p.Out); err != nil {
			t.Fatalf("paper stdout is not one protocol JSON document (exit %d): %v\nstdout=%s\nstderr tail=%s", p.Code, err, p.Raw, tail(p.Stderr, 8000))
		}
	}
	return p
}

func pairPaperRequireComplete(t *testing.T, label string, p pairPaperProc) {
	t.Helper()
	if p.Code != 0 || !p.Out.MechanicallyComplete || p.Out.Status != basketReplayStatusComplete || p.Out.Scored || p.Out.PromotionEligible || p.Out.Comparison == nil || !p.Out.Comparison.Eligible {
		t.Fatalf("%s: want a mechanically complete paper run with exit 0, got exit %d status %s complete=%v reasons=%v refusal=%+v error=%q comparison=%+v recording=%+v\nstderr tail=%s",
			label, p.Code, p.Out.Status, p.Out.MechanicallyComplete, p.Out.IncompleteReasons, p.Out.Refusal, p.Out.Error, p.Out.Comparison, p.Out.Recording, tail(p.Stderr, 8000))
	}
}

func pairPaperLogRows(t *testing.T, stateDir, runID string) []pairRecRow {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := readPairRecRows(db.db, runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		ev, err := pairRecDecode(rows[i].Payload)
		if err != nil {
			t.Fatal(err)
		}
		rows[i].Event = ev
	}
	return rows
}

func pairPaperCallKinds(rows []pairRecRow) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		if r.Kind == pairRecEventStart {
			out[r.Event.Start.Kind]++
		}
	}
	return out
}

func pairPaperPurposes(t *testing.T, out pairReplayOutput) map[string]int {
	t.Helper()
	purposes := map[string]int{}
	for _, a := range pairOfflineSection(t, out, "basket_member_actions") {
		purposes[fmt.Sprint(a["purpose"])]++
	}
	return purposes
}

func TestBasketReplayPairPaperResearchPolicyAdmission(t *testing.T) {
	f, _ := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-research-admission"})
	cases := map[string]func(*pairFundingPolicyV1){
		"version":       func(p *pairFundingPolicyV1) { p.Version = 2 },
		"window":        func(p *pairFundingPolicyV1) { p.WindowMs = 5001 },
		"print offset":  func(p *pairFundingPolicyV1) { p.MaxPrintOffsetMs = 59999 },
		"prepare lead":  func(p *pairFundingPolicyV1) { p.PrepareLeadMs = 29999 },
		"legacy oracle": func(p *pairFundingPolicyV1) { p.MemberRules[pairTestHL] = pairFundingRuleHyperliquid },
		"swapped rules": func(p *pairFundingPolicyV1) {
			p.MemberRules[pairTestHL], p.MemberRules[pairTestOKX] = pairFundingRuleOKX, pairFundingRuleHLReceiptResearch
		},
		"unknown rule":      func(p *pairFundingPolicyV1) { p.MemberRules[pairTestOKX] = "okx_unknown_v9" },
		"missing member":    func(p *pairFundingPolicyV1) { delete(p.MemberRules, pairTestOKX) },
		"unknown member":    func(p *pairFundingPolicyV1) { p.MemberRules["stranger"] = pairFundingRuleOKX },
		"from bound":        func(p *pairFundingPolicyV1) { p.FromMs++ },
		"drain bound":       func(p *pairFundingPolicyV1) { p.DrainThroughMs-- },
		"nonpositive bound": func(p *pairFundingPolicyV1) { p.FromMs = 0 },
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		badDir := filepath.Join(f.parent, "bad-"+strings.ReplaceAll(name, " ", "-"))
		spec := f.spec(basketReplayModeNew, badDir)
		policy := pairFundingPolicyClone(*spec.Scenario.FundingPolicy)
		cases[name](&policy)
		spec.Scenario.FundingPolicy = &policy
		got := runPairPaperAdapter(t, f, spec, f.env(false))
		if got.Code != basketReplayExitRefused || got.Out.Refusal == nil {
			t.Fatalf("%s: an invalid policy was admitted: exit %d %+v", name, got.Code, got.Out.Refusal)
		}
		if _, err := os.Lstat(badDir); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused policy claimed a run directory", name)
		}
	}
	rawSpec, err := json.Marshal(f.spec(basketReplayModeNew, filepath.Join(f.parent, "raw-policy")))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]interface{}{
		"null policy":         nil,
		"float window":        "5000.0",
		"null window":         "null",
		"string window":       `"5000"`,
		"extra policy field":  "extra",
		"missing drain field": "missing",
	} {
		var spec map[string]interface{}
		dec := json.NewDecoder(bytes.NewReader(rawSpec))
		dec.UseNumber()
		if err := dec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		sc := spec["scenario"].(map[string]interface{})
		switch v := value.(type) {
		case nil:
			sc["funding_policy"] = nil
		case string:
			p := sc["funding_policy"].(map[string]interface{})
			switch v {
			case "extra":
				p["sample_ms"] = json.Number("1")
			case "missing":
				delete(p, "drain_through_ms")
			default:
				p["window_ms"] = json.RawMessage(v)
			}
		}
		dir := filepath.Join(f.parent, "raw-"+strings.ReplaceAll(name, " ", "-"))
		spec["run"].(map[string]interface{})["state_dir"] = dir
		got := runPairPaperAdapter(t, f, spec, f.env(false))
		if got.Code != basketReplayExitRefused {
			t.Fatalf("%s: the malformed policy was admitted: exit %d %+v", name, got.Code, got.Out.Refusal)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused policy claimed a run directory", name)
		}
	}
	dir := filepath.Join(f.parent, "paper")
	first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--max-steps", "3")
	if first.Code == basketReplayExitRefused || first.Out.Run == nil || first.Out.Run.Finished {
		t.Fatalf("the complete policy starts a resumable chunk after both startup probe routes pass, got exit %d %+v %+v\nstderr tail=%s", first.Code, first.Out.Refusal, first.Out.Run, tail(first.Stderr, 4000))
	}
	manifest, err := readPairPaperManifest(dir)
	if err != nil || manifest.Version != pairPaperManifestAsyncVer || manifest.EventOrder != pairPaperEventOrderAsync || manifest.Scenario.FundingPolicy == nil {
		t.Fatalf("a policy run writes the version 2 paper manifest with the research event order: %+v %v", manifest, err)
	}
	digest := dirDigest(t, dir)
	legacy := f.spec(basketReplayModeResume, dir)
	legacy.Scenario.FundingPolicy = nil
	edited := f.spec(basketReplayModeResume, dir)
	edited.Collection.DrainThroughMs++
	ep := pairFundingPolicyClone(*edited.Scenario.FundingPolicy)
	ep.DrainThroughMs++
	edited.Scenario.FundingPolicy = &ep
	for name, spec := range map[string]*pairPaperSpec{"policy removed": legacy, "policy bounds changed": edited} {
		got := runPairPaperAdapter(t, f, spec, f.env(false), "--max-steps", "3")
		if got.Code != basketReplayExitRefused {
			t.Fatalf("%s: a resume that changes the frozen policy is refused, got exit %d %+v", name, got.Code, got.Out.Refusal)
		}
		if dirDigest(t, dir) != digest {
			t.Fatalf("%s: the refused resume changed the run files", name)
		}
	}
	again := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--max-steps", "3")
	if again.Code == basketReplayExitRefused || again.Out.Run == nil || !again.Out.Run.Resumed {
		t.Fatalf("the unchanged policy resumes, got exit %d %+v", again.Code, again.Out.Refusal)
	}
}

func TestBasketReplayPairPaperRecordsBeforeUseAndReproducesOffline(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{capRefresh: 20000})
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--seal")
	pairPaperRequireComplete(t, "paper run", p)
	if p.Out.Recording == nil || p.Out.Recording.Seal == nil || !p.Out.Recording.Seal.Sealed {
		t.Fatalf("the finished paper run must seal: %+v", p.Out.Recording)
	}
	purposes := pairPaperPurposes(t, p.Out)
	if purposes[pairPurposeEntry] < 2 || purposes[pairPurposeExit] < 2 {
		t.Fatalf("the paper run must open both legs on the wide spread and exit both after it converges, got %v\nstderr tail=%s", purposes, tail(p.Stderr, 6000))
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	kinds := pairPaperCallKinds(rows)
	for _, k := range []string{pairRecKindCollect, pairRecKindMemberRead, pairRecKindReference, pairRecKindMatchPoll, pairRecKindCandles, pairRecKindMarks, pairRecKindPolicy,
		pairRecKindNativeCheck, pairRecKindFunding, pairRecKindCapability, pairRecKindAccount, pairRecKindConversions} {
		if kinds[k] == 0 {
			t.Fatalf("the recording must hold %s calls, got %v", k, kinds)
		}
	}
	ends := map[string]int64{}
	for _, r := range rows {
		switch r.Kind {
		case pairRecEventEnd:
			ends[r.CallID] = r.Seq
		case pairRecEventRecovery:
			t.Fatalf("an uninterrupted run holds no recovery event: %+v", r.Event)
		}
	}
	for _, r := range rows {
		if r.Kind == pairRecEventStart {
			if e, ok := ends[r.CallID]; !ok || e <= r.Seq {
				t.Fatalf("call %s (%s) must commit its start before its terminal outcome", r.CallID, r.Event.Start.Kind)
			}
		}
	}
	if rows[len(rows)-1].Kind != pairRecEventSeal {
		t.Fatalf("the sealed recording ends with its seal event, got %s", rows[len(rows)-1].Kind)
	}
	spec := filepath.Join(dir, pairPaperExportDirName, pairRecordingSpecName)
	blob, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	var repro map[string]interface{}
	if err := json.Unmarshal(blob, &repro); err != nil {
		t.Fatal(err)
	}
	repro["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "reproduction")
	playback := runPairReplayAdapter(t, f.repo, repro, f.env(true))
	pairOfflineRequireComplete(t, "sealed playback with every network route blocked", playback)
	if playback.Out.Comparison.Hash != p.Out.Comparison.Hash {
		t.Fatalf("the offline reproduction must equal the paper run: paper %s, playback %s\n%s", p.Out.Comparison.Hash, playback.Out.Comparison.Hash, pairOfflineDiff(t, p.Out, playback.Out))
	}
	forged := pairPaperForgeNativeResult(t, f, filepath.Join(dir, pairPaperExportDirName))
	forged["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "forged-reproduction")
	fp := runPairReplayAdapter(t, f.repo, forged, f.env(true))
	if fp.Out.Comparison == nil || fp.Out.Comparison.Eligible || !strings.Contains(strings.Join(fp.Out.Comparison.Reasons, " "), "rerun from its saved request produced result") {
		t.Fatalf("a recorded member check result that its offline rerun does not reproduce refuses the equality claim, got %+v (exit %d)\nstderr tail=%s", fp.Out.Comparison, fp.Code, tail(fp.Stderr, 3000))
	}
	var connects []string
	if b, err := os.ReadFile(f.callLog); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, `"connect"`) {
				connects = append(connects, line)
			}
		}
	}
	sort.Strings(connects)
	if len(connects) > 0 {
		t.Fatalf("no child may open a socket in the fixture: %v", connects)
	}
}

func pairPaperReproduce(t *testing.T, f *pairPaperFixture, stateDir, name string) pairReplayProc {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(stateDir, pairPaperExportDirName, pairRecordingSpecName))
	if err != nil {
		t.Fatal(err)
	}
	var repro map[string]interface{}
	if err := json.Unmarshal(blob, &repro); err != nil {
		t.Fatal(err)
	}
	repro["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, name)
	return runPairReplayAdapter(t, f.repo, repro, f.env(true))
}

func pairPaperNoRepeatedCall(t *testing.T, rows []pairRecRow) int {
	t.Helper()
	starts := map[string]pairRecStart{}
	for _, r := range rows {
		if r.Kind == pairRecEventStart {
			starts[r.CallID] = *r.Event.Start
		}
	}
	recovered := 0
	for _, r := range rows {
		if r.Kind != pairRecEventRecovery {
			continue
		}
		recovered++
		s := starts[r.CallID]
		for id, o := range starts {
			if id != r.CallID && o.StepID == s.StepID && o.Kind == s.Kind && o.ParentID == s.ParentID && o.RequestHash == s.RequestHash && o.MemberID == s.MemberID {
				t.Fatalf("the interrupted %s call %s was made again as %s after the restart", s.Kind, r.CallID, id)
			}
		}
	}
	return recovered
}

func TestBasketReplayPairPaperCrashesResumeWithoutRepeatingCommittedCalls(t *testing.T) {
	slots := make(chan struct{}, 4)
	for _, fault := range []string{"exit-after-call-start:45", "exit-after-input-commit:160", "exit-after-management:2", "exit-after-collection:2", "exit-after-envelope-freeze:1", "exit-after-acceptance:1", "exit-after-book-write:6", "exit-after-book-commit:2"} {
		fault := fault
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-fault-" + strings.SplitN(fault, ":", 2)[0][11:]})
			dir := filepath.Join(f.parent, "paper")
			crash := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", fault)
			if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault "+fault+"\n") {
				t.Fatalf("the fault must exit 75 at its boundary, got exit %d\nstderr tail=%s", crash.Code, tail(crash.Stderr, 4000))
			}
			before := pairPaperLogRows(t, dir, f.runID)
			resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
			if resumed.Code == basketReplayExitRefused || resumed.Out.Run == nil || !resumed.Out.Run.Resumed || !resumed.Out.Run.Finished {
				t.Fatalf("the resumed paper run must finish, got exit %d status %s refusal %+v error %q run %+v reasons %v\nstderr tail=%s", resumed.Code, resumed.Out.Status, resumed.Out.Refusal, resumed.Out.Error, resumed.Out.Run, resumed.Out.IncompleteReasons, tail(resumed.Stderr, 6000))
			}
			after := pairPaperLogRows(t, dir, f.runID)
			for i := range before {
				if after[i].Prefix != before[i].Prefix {
					t.Fatalf("the committed prefix changed at input %d across the restart", before[i].Seq)
				}
			}
			recovered := pairPaperNoRepeatedCall(t, after)
			interrupting := recovered > 0
			if strings.HasPrefix(fault, "exit-after-call-start") && recovered == 0 {
				t.Fatalf("a crash after a call start must leave an explicit unknown outcome")
			}
			if interrupting == resumed.Out.MechanicallyComplete {
				t.Fatalf("an interrupted call keeps the run incomplete and any other boundary resumes complete: complete=%v reasons=%v", resumed.Out.MechanicallyComplete, resumed.Out.IncompleteReasons)
			}
			if resumed.Out.Recording == nil || resumed.Out.Recording.Seal == nil || !resumed.Out.Recording.Seal.Sealed {
				t.Fatalf("a finished run with every call terminated seals: %+v", resumed.Out.Recording)
			}
			playback := pairPaperReproduce(t, f, dir, "reproduction")
			if playback.Out.Comparison == nil || playback.Out.Comparison.Hash != resumed.Out.Comparison.Hash || playback.Out.MechanicallyComplete != resumed.Out.MechanicallyComplete {
				t.Fatalf("the offline reproduction of the resumed recording must equal it: paper %+v, playback %+v (exit %d)\n%s\nplayback stderr tail=%s", resumed.Out.Comparison, playback.Out.Comparison, playback.Code, pairOfflineDiff(t, resumed.Out, playback.Out), tail(playback.Stderr, 4000))
			}
			if !interrupting && !playback.Out.Comparison.Eligible {
				t.Fatalf("a resumed recording with no interrupted call reproduces with an equality claim: %+v", playback.Out.Comparison)
			}
		})
	}
	t.Run("research oracle", func(t *testing.T) {
		counts := pairPaperResearchOracleFaultCounts(t, pairPaperResearchOptions{})
		for _, fault := range []string{fmt.Sprintf("exit-after-call-start:%d", counts.callStart), fmt.Sprintf("exit-after-input-commit:%d", counts.inputEnd), "exit-after-book-write:5", "exit-after-book-commit:5"} {
			fault := fault
			t.Run(fault, func(t *testing.T) {
				t.Parallel()
				slots <- struct{}{}
				defer func() { <-slots }()
				f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-fault-" + strings.SplitN(fault, ":", 2)[0][11:]})
				dir := filepath.Join(f.parent, "paper")
				crash := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", fault)
				if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault "+fault+"\n") {
					t.Fatalf("the fault must exit 75 at its boundary, got exit %d\nstderr tail=%s", crash.Code, tail(crash.Stderr, 4000))
				}
				before := pairPaperLogRows(t, dir, f.runID)
				resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
				if resumed.Code == basketReplayExitRefused || resumed.Out.Run == nil || !resumed.Out.Run.Resumed || !resumed.Out.Run.Finished {
					t.Fatalf("the resumed research run must finish, got exit %d refusal %+v run %+v\nstderr tail=%s", resumed.Code, resumed.Out.Refusal, resumed.Out.Run, tail(resumed.Stderr, 6000))
				}
				after := pairPaperLogRows(t, dir, f.runID)
				for i := range before {
					if after[i].Prefix != before[i].Prefix {
						t.Fatalf("the committed prefix changed at input %d across the restart", before[i].Seq)
					}
				}
				recovered := pairPaperNoRepeatedCall(t, after)
				starts, _, _ := pairPaperOracleRows(after)
				if starts != 1 {
					t.Fatalf("the hour keeps exactly one outside oracle attempt across the restart, got %d", starts)
				}
				interrupting := recovered > 0
				if interrupting == resumed.Out.MechanicallyComplete {
					t.Fatalf("an interrupted call keeps the run incomplete and any other boundary resumes complete: complete=%v reasons=%v", resumed.Out.MechanicallyComplete, resumed.Out.IncompleteReasons)
				}
				applied := 0
				for _, row := range pairPaperFundingRows(t, dir) {
					if row.StrategyID == pairTestHL && row.EventMs == target+3 && row.Status == pairFundingStatusApplied {
						applied++
					}
				}
				if applied != map[bool]int{true: 0, false: 1}[interrupting] {
					t.Fatalf("the held hour is charged once from its one sample, or never when the sample was interrupted, got %d", applied)
				}
				playback := pairPaperReproduce(t, f, dir, "reproduction")
				if playback.Out.Comparison == nil || playback.Out.Comparison.Hash != resumed.Out.Comparison.Hash || playback.Out.MechanicallyComplete != resumed.Out.MechanicallyComplete {
					t.Fatalf("the offline reproduction of the resumed research recording must equal it: paper %+v, playback %+v\n%s", resumed.Out.Comparison, playback.Out.Comparison, pairOfflineDiff(t, resumed.Out, playback.Out))
				}
			})
		}
	})
}

func pairPaperCallLogLines(f *pairPaperFixture) []string {
	b, err := os.ReadFile(f.callLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestBasketReplayPairPaperRefusesUncheckedResearchConfigurationsBeforeAnyRead(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-config"})
	cfgDir := filepath.Dir(f.config)
	write := func(name, body string) string {
		p := filepath.Join(cfgDir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	symlink := filepath.Join(cfgDir, "linked.json")
	if err := os.Symlink(f.config, symlink); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(pairOfflineRealPath(t, t.TempDir()), "alias")
	if err := os.Symlink(cfgDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	hard := write("hard-original.json", `{"egress":{"proxy_required":false}}`)
	if err := os.Link(hard, filepath.Join(cfgDir, "hard-link.json")); err != nil {
		t.Fatal(err)
	}
	foreign := ""
	if os.Getuid() != 0 {
		for _, p := range []string{"/etc/hosts", "/etc/passwd"} {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				if info, err := os.Lstat(real); err == nil && info.Mode().IsRegular() {
					if _, _, n, uid, ok := pairStatIdentity(info); ok && int(uid) != os.Getuid() && n == 1 {
						foreign = real
						break
					}
				}
			}
		}
	}
	cases := []struct {
		name   string
		args   []string
		env    []string
		reason string
	}{
		{"omitted", nil, nil, "config_refused"},
		{"relative", []string{"--config", "research-egress.json"}, nil, "config_refused"},
		{"missing", []string{"--config", filepath.Join(cfgDir, "absent.json")}, nil, "config_refused"},
		{"malformed", []string{"--config", write("malformed.json", `{"egress":`)}, nil, "config_refused"},
		{"duplicate key", []string{"--config", write("dup.json", `{"egress":{"proxy_required":false},"egress":{"proxy_required":true}}`)}, nil, "config_refused"},
		{"strategy block", []string{"--config", write("strategies.json", `{"egress":{"proxy_required":false},"strategies":[]}`)}, nil, "config_refused"},
		{"unknown egress key", []string{"--config", write("bad-egress.json", `{"egress":{"proxy":"x"}}`)}, nil, "config_refused"},
		{"deployed prefix", []string{"--config", "/var/lib/go-trader/config.json"}, nil, "config_refused"},
		{"symbolic link", []string{"--config", symlink}, nil, "config_refused"},
		{"realpath alias", []string{"--config", filepath.Join(aliasDir, filepath.Base(f.config))}, nil, "config_refused"},
		{"hard link", []string{"--config", hard}, nil, "config_refused"},
		{"twice", []string{"--config", f.config, "--config", f.config}, nil, "config_refused"},
		{"proxy required without a proxy", []string{"--config", write("proxy.json", `{"egress":{"proxy_required":true}}`)}, nil, "egress_refused"},
		{"proxy without enforcement", nil, []string{"GO_TRADER_EGRESS_PROXY=http://127.0.0.1:9"}, "egress_refused"},
	}
	if foreign != "" {
		cases = append(cases, struct {
			name   string
			args   []string
			env    []string
			reason string
		}{"foreign owner", []string{"--config", foreign}, nil, "config_refused"})
	}
	bin := pyintegrationBinary(t)
	for i, c := range cases {
		dir := filepath.Join(f.parent, fmt.Sprintf("refused-%d", i))
		blob, _ := json.Marshal(f.spec(basketReplayModeNew, dir))
		specPath := filepath.Join(t.TempDir(), "spec.json")
		if err := os.WriteFile(specPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"basket-replay-paper", "--spec", specPath}, c.args...)
		if c.args == nil && c.name != "omitted" {
			args = append(args, "--config", f.config)
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = f.repo
		cmd.Env = append(append(os.Environ(), f.env(false)...), c.env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		var out pairReplayOutput
		_ = json.Unmarshal(stdout.Bytes(), &out)
		if code != basketReplayExitRefused || out.Refusal == nil || out.Refusal.Reason != c.reason {
			t.Fatalf("%s: want exit 3 with refusal %s, got exit %d %+v\nstderr=%s", c.name, c.reason, code, out.Refusal, tail(stderr.String(), 2000))
		}
		if _, err := os.Lstat(dir); err == nil {
			t.Fatalf("%s: a refused configuration must not create the run directory", c.name)
		}
		if lines := pairPaperCallLogLines(f); len(lines) > 0 {
			t.Fatalf("%s: a refused configuration must reach no venue, got %v", c.name, lines)
		}
	}
}

func TestBasketReplayPairPaperRefusesBadSpecificationsAndFaultPairings(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-usage"})
	bin := pyintegrationBinary(t)
	run := func(name string, sub string, spec interface{}, args ...string) (int, pairReplayOutput, string) {
		blob, _ := json.Marshal(spec)
		specPath := filepath.Join(t.TempDir(), "spec.json")
		if err := os.WriteFile(specPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		argv := append([]string{sub, "--spec", specPath}, args...)
		cmd := exec.Command(bin, argv...)
		cmd.Dir = f.repo
		cmd.Env = append(os.Environ(), f.env(false)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		var out pairReplayOutput
		_ = json.Unmarshal(stdout.Bytes(), &out)
		return code, out, stderr.String()
	}
	dir := filepath.Join(f.parent, "usage")
	good := f.spec(basketReplayModeNew, dir)
	if code, _, _ := run("v1 fault", "basket-replay-paper", good, "--config", f.config, "--fault", "exit-after-submit:1"); code != basketReplayExitUsage {
		t.Fatalf("a version-1 fault on the paper command is a usage error, got %d", code)
	}
	if code, _, _ := run("unknown fault", "basket-replay-paper", good, "--config", f.config, "--fault", "exit-after-everything:1"); code != basketReplayExitUsage {
		t.Fatalf("an unknown fault is a usage error, got %d", code)
	}
	of := newPairOfflineFixture(t, pairOfflineOptions{})
	if code, _, _ := run("paper fault on replay", "basket-replay", of.spec(basketReplayModeNew, of.stateDir("never")), "--fault", "fail-record-append:1"); code != basketReplayExitUsage {
		t.Fatalf("a paper fault on the offline replay is a usage error, got %d", code)
	}
	mutate := func(edit func(s *pairPaperSpec)) *pairPaperSpec {
		s := f.spec(basketReplayModeNew, dir)
		edit(s)
		return s
	}
	sealedDoc := pairTestDoc(t, f.parent, func(root map[string]interface{}) {
		delete(root, "db_file")
		delete(root, "log_dir")
		root["interval_seconds"] = 10
		p := pairTestPair(root)
		p["policy"] = pairPolicyCrossExchangePerpArbitrage
		p["params"] = map[string]interface{}{"entry_threshold_bps": 40, "exit_threshold_bps": 30, "adverse_band_bps": 4, "funding_allowance_bps": 0}
		p["limits"] = pairPaperLimits()
	})
	bad := map[string]*pairPaperSpec{
		"sealed member":        mutate(func(s *pairPaperSpec) { s.ResearchConfig = json.RawMessage(sealedDoc) }),
		"through before from":  mutate(func(s *pairPaperSpec) { s.Collection.ThroughMs = s.Collection.FromMs }),
		"drain before through": mutate(func(s *pairPaperSpec) { s.Collection.DrainThroughMs = s.Collection.ThroughMs - 1 }),
		"zero poll interval":   mutate(func(s *pairPaperSpec) { s.Scenario.PaperPollIntervalMs = 0 }),
		"funding object": mutate(func(s *pairPaperSpec) {
			s.InputManifest.Objects = append(s.InputManifest.Objects, pairReplayObjectRef{Kind: pairReplayKindFunding, Path: "f.json", SHA256: strings.Repeat("a", 64), SizeBytes: 1})
		}),
		"bounds differ": mutate(func(s *pairPaperSpec) {
			s.Bounds.End = time.UnixMilli(s.Collection.DrainThroughMs + 1000).UTC().Format(time.RFC3339Nano)
		}),
		"scripted failure": mutate(func(s *pairPaperSpec) {
			s.Scenario.VenueFailures = []pairReplayFailureScript{{Kind: pairReplayFailSave, At: s.Bounds.Start}}
		}),
	}
	names := make([]string, 0, len(bad))
	for n := range bad {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		code, out, stderr := run(n, "basket-replay-paper", bad[n], "--config", f.config)
		if code != basketReplayExitRefused || out.Refusal == nil || out.Refusal.Reason != "spec_invalid" {
			t.Fatalf("%s: want exit 3 spec_invalid, got exit %d %+v\nstderr=%s", n, code, out.Refusal, tail(stderr, 2000))
		}
		if _, err := os.Lstat(dir); err == nil {
			t.Fatalf("%s: a refused specification must not create the run directory", n)
		}
	}
	if lines := pairPaperCallLogLines(f); len(lines) > 0 {
		t.Fatalf("refused specifications must reach no venue, got %v", lines)
	}
}

func pairPaperProgressJSON(t *testing.T, stateDir string) string {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.db.QueryRow(`SELECT progress_json FROM basket_contracts LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBasketReplayPairPaperRecordAppendFailureHoldsIncreasesThroughARestart(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-barrier"})
	dir := filepath.Join(f.parent, "paper")
	first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", "fail-record-append:150", "--seal")
	if first.Code == basketReplayExitRefused || first.Out.Recording == nil || !first.Out.Recording.BarrierSet || first.Out.MechanicallyComplete {
		t.Fatalf("a store that refuses appends keeps the barrier and the run incomplete, got exit %d recording %+v reasons %v\nstderr tail=%s", first.Code, first.Out.Recording, first.Out.IncompleteReasons, tail(first.Stderr, 4000))
	}
	if !strings.Contains(first.Stderr, "fault fail-record-append:150") || first.Out.Recording.Seal == nil || first.Out.Recording.Seal.Sealed {
		t.Fatalf("the fault prints once and a blocked run never seals: seal %+v", first.Out.Recording.Seal)
	}
	if prog := pairPaperProgressJSON(t, dir); !strings.Contains(prog, `"recording_blocked"`) || !strings.Contains(prog, `"v":4`) {
		t.Fatalf("the barrier is saved in sequence-guarded pair progress as version 4, got %s", prog)
	}
	var blockedAt int64
	for _, rec := range pairOfflineSteps(t, first.Out) {
		if rec.Pair.RecordingBlocked != "" && blockedAt == 0 {
			blockedAt = rec.AtMs
		}
	}
	if blockedAt == 0 {
		t.Fatal("the step records show the recording barrier")
	}
	for _, a := range pairOfflineSection(t, first.Out, "basket_member_actions") {
		created, _ := a["created_at_ms"].(float64)
		if a["purpose"] == pairPurposeEntry && int64(created) >= blockedAt {
			t.Fatalf("no increase is reserved while the recording barrier holds, got %v", a)
		}
	}
	resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
	if resumed.Out.Recording == nil || resumed.Out.Recording.BarrierSet || resumed.Out.Recording.Seal == nil || !resumed.Out.Recording.Seal.Sealed {
		t.Fatalf("a restart verifies the full committed record, clears the barrier and seals the finished run, got exit %d recording %+v refusal %+v\nstderr tail=%s", resumed.Code, resumed.Out.Recording, resumed.Out.Refusal, tail(resumed.Stderr, 4000))
	}
	if prog := pairPaperProgressJSON(t, dir); strings.Contains(prog, `"recording_blocked"`) {
		t.Fatalf("the cleared barrier is saved: %s", prog)
	}
	cleared := false
	for _, r := range pairPaperLogRows(t, dir, f.runID) {
		if r.Kind == pairRecEventClear && strings.Contains(r.Event.Clear.Reason, "restart verification") {
			cleared = true
		}
	}
	if !cleared || resumed.Out.Comparison == nil || resumed.Out.Comparison.Eligible {
		t.Fatalf("the release is recorded and the run makes no equality claim, got cleared=%v comparison %+v", cleared, resumed.Out.Comparison)
	}
}

func pairPaperTradingDigest(t *testing.T, stateDir string) string {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var parts []string
	for _, q := range []string{`SELECT progress_json FROM basket_contracts`, `SELECT deadline_ms || status || targets_json FROM basket_rebalances ORDER BY deadline_ms`,
		`SELECT action_id || status FROM basket_member_actions ORDER BY action_id`, `SELECT steps_applied || cursor_step_id || pending_step_id FROM basket_replay_runs`} {
		rows, err := db.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, s)
		}
		rows.Close()
	}
	return sha256Hex([]byte(strings.Join(parts, "\n")))
}

func TestBasketReplayPairPaperResumeRefusesAnAlteredRecordingBeforeTrading(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-tamper"})
	base := filepath.Join(f.parent, "paper")
	chunk := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, base), f.env(false), "--max-steps", "4")
	if chunk.Out.Status != basketReplayStatusStopped || chunk.Out.Run == nil || chunk.Out.Run.StepsApplied != 4 {
		t.Fatalf("--max-steps stops a resumable chunk, got status %s run %+v\nstderr tail=%s", chunk.Out.Status, chunk.Out.Run, tail(chunk.Stderr, 3000))
	}
	tampers := []struct {
		name string
		sql  []string
	}{
		{"altered payload", []string{`DROP TRIGGER basket_replay_inputs_immutable`, `UPDATE basket_replay_inputs SET payload = payload || x'20' WHERE seq = (SELECT MIN(seq) FROM basket_replay_inputs WHERE event_kind = 'call_end')`}},
		{"missing row", []string{`DROP TRIGGER basket_replay_inputs_append_only`, `DELETE FROM basket_replay_inputs WHERE seq = (SELECT MIN(seq) + 3 FROM basket_replay_inputs)`}},
		{"reordered rows", []string{`DROP TRIGGER basket_replay_inputs_immutable`, `UPDATE basket_replay_inputs SET seq = -seq WHERE seq IN (5, 6)`, `UPDATE basket_replay_inputs SET seq = 11 - (-seq) WHERE seq IN (-5, -6)`}},
		{"changed call index", []string{`DROP TRIGGER basket_replay_calls_terminal_once`, `UPDATE basket_replay_calls SET result_hash = 'x' WHERE end_seq != 0 AND rowid = (SELECT MIN(rowid) FROM basket_replay_calls WHERE end_seq != 0)`}},
	}
	for i, tc := range tampers {
		dir := filepath.Join(f.parent, fmt.Sprintf("tamper-%d", i))
		if err := exec.Command("cp", "-R", base, dir).Run(); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, basketReplayDBName))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range tc.sql {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", tc.name, q, err)
			}
		}
		db.Close()
		before := pairPaperTradingDigest(t, dir)
		p := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false))
		if p.Code != basketReplayExitRefused || p.Out.Refusal == nil || p.Out.Refusal.Reason != "resume_refused" {
			t.Fatalf("%s: an altered committed recording refuses the resume, got exit %d refusal %+v error %q", tc.name, p.Code, p.Out.Refusal, p.Out.Error)
		}
		if after := pairPaperTradingDigest(t, dir); after != before {
			t.Fatalf("%s: a refused resume changes no trading state", tc.name)
		}
	}
}

func pairPaperForgeNativeResult(t *testing.T, f *pairPaperFixture, exportDir string) map[string]interface{} {
	t.Helper()
	forgeDir := pairOfflineRealPath(t, t.TempDir())
	if err := exec.Command("cp", "-R", exportDir+"/.", forgeDir).Run(); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(forgeDir, pairRecordingLogName))
	if err != nil {
		t.Fatal(err)
	}
	var lines []pairRecLogLine
	for _, l := range strings.Split(strings.TrimSpace(string(blob)), "\n") {
		var line pairRecLogLine
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	kinds := map[string]string{}
	changed := false
	for i := range lines {
		ev, err := pairRecDecode(lines[i].Payload)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Start != nil {
			kinds[ev.CallID] = ev.Start.Kind + "/" + ev.Start.Phase
		}
		if !changed && ev.End != nil && kinds[ev.CallID] == pairRecKindNativeCheck+"/"+pairRecPhaseStep && ev.End.Class == pairRecClassResult {
			ev.End.Result = append(append([]byte(nil), ev.End.Result...), ' ')
			ev.End.ResultHash = pairRecResultHash(ev.End.Result)
			payload, err := pairRecEncode(ev)
			if err != nil {
				t.Fatal(err)
			}
			lines[i].Payload, lines[i].PayloadHash = payload, sha256Hex(payload)
			changed = true
		}
	}
	if !changed {
		t.Fatal("the recording holds no member check result to forge")
	}
	prev := ""
	var out bytes.Buffer
	for i := range lines {
		l := &lines[i]
		l.PrevPrefix = prev
		l.Prefix = pairRecPrefixHash(l.Schema, f.runID, l.Seq, l.Partition, l.ContractFP, l.Kind, l.CallID, prev, l.PayloadHash)
		prev = l.Prefix
		b, _ := json.Marshal(l)
		out.Write(b)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(forgeDir, pairRecordingLogName), out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var man pairRecordingManifest
	mb, _ := os.ReadFile(filepath.Join(forgeDir, pairRecordingManifestName))
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatal(err)
	}
	man.Log.SHA256, man.Log.SizeBytes, man.Log.FinalPrefixHash = sha256Hex(out.Bytes()), int64(out.Len()), prev
	mb, _ = json.MarshalIndent(man, "", "  ")
	mb = append(mb, '\n')
	if err := os.WriteFile(filepath.Join(forgeDir, pairRecordingManifestName), mb, 0o600); err != nil {
		t.Fatal(err)
	}
	var spec map[string]interface{}
	sb, _ := os.ReadFile(filepath.Join(forgeDir, pairRecordingSpecName))
	if err := json.Unmarshal(sb, &spec); err != nil {
		t.Fatal(err)
	}
	spec["input_manifest"].(map[string]interface{})["root"] = forgeDir
	spec["recording"] = map[string]interface{}{"version": 1, "path": pairRecordingManifestName, "sha256": sha256Hex(mb), "size_bytes": len(mb)}
	return spec
}

type pairPaperDriverResult struct {
	Status               string                 `json:"status"`
	MechanicallyComplete bool                   `json:"mechanically_complete"`
	Scored               bool                   `json:"scored"`
	PromotionEligible    bool                   `json:"promotion_eligible"`
	Paper                bool                   `json:"paper"`
	Recording            *pairPaperOutputInfo   `json:"recording"`
	Refusal              map[string]interface{} `json:"refusal"`
	Comparison           *pairReplayComparison  `json:"comparison"`
}

func TestBasketReplayPairPaperDriverRunsChunksWithoutADefaultTimeout(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-driver"})
	planDir := pairOfflineRealPath(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(planDir, "research.json"), f.doc, 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(f.inputs, f.seed.Path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "seed.json"), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := f.spec(basketReplayModeNew, filepath.Join(f.parent, "driver"))
	plan := map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": f.runID, "state_dir": spec.Run.StateDir, "source": spec.Run.Source,
		"research_config": map[string]string{"path": "research.json", "sha256": sha256Hex(f.doc)},
		"objects":         []map[string]string{{"kind": pairReplayKindEvidence, "path": "seed.json", "sha256": sha256Hex(seed)}},
		"scenario":        spec.Scenario, "bounds": spec.Bounds, "collection": spec.Collection}
	pb, _ := json.Marshal(plan)
	planPath := filepath.Join(planDir, "plan.json")
	if err := os.WriteFile(planPath, pb, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := pyintegrationBinary(t)
	drive := func(plan string, args ...string) (int, pairPaperDriverResult, time.Duration) {
		out := filepath.Join(t.TempDir(), fmt.Sprintf("result-%d.json", time.Now().UnixNano()))
		argv := append([]string{filepath.Join(f.repo, "backtest", "basket_replay.py"), "--plan", plan, "--binary", bin, "--out", out}, args...)
		cmd := exec.Command(filepath.Join(f.repo, ".venv", "bin", "python3"), argv...)
		cmd.Dir = f.repo
		cmd.Env = append(os.Environ(), f.env(false)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		start := time.Now()
		err := cmd.Run()
		took := time.Since(start)
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		var res pairPaperDriverResult
		b, rerr := os.ReadFile(out)
		if rerr != nil && code == 2 {
			return code, res, took
		}
		if rerr != nil {
			t.Fatalf("driver wrote no result (exit %d): %v\n%s", code, rerr, stderr.String())
		}
		if err := json.Unmarshal(b, &res); err != nil {
			t.Fatal(err)
		}
		return code, res, took
	}
	if code, _, _ := drive(planPath, "--config", f.config); code != 2 {
		t.Fatalf("--config without --paper is a usage error, got %d", code)
	}
	timedPlan := filepath.Join(planDir, "timed.json")
	tp := map[string]interface{}{}
	_ = json.Unmarshal(pb, &tp)
	tp["state_dir"] = filepath.Join(f.parent, "driver-timed")
	tb, _ := json.Marshal(tp)
	if err := os.WriteFile(timedPlan, tb, 0o600); err != nil {
		t.Fatal(err)
	}
	code, res, took := drive(timedPlan, "--paper", "--config", f.config, "--timeout", "4")
	if code != 1 || res.Status != "process_error" || took > 30*time.Second {
		t.Fatalf("an explicit timeout stops a paper run as a process error, got exit %d status %s after %s", code, res.Status, took)
	}
	code, res, _ = drive(planPath, "--paper", "--config", f.config, "--max-steps", "3")
	if code != 1 || res.Status != basketReplayStatusStopped || res.MechanicallyComplete || !res.Paper || res.Scored || res.PromotionEligible {
		t.Fatalf("a chunk limit stops the paper run resumable, got exit %d %+v", code, res)
	}
	code, res, took = drive(planPath, "--paper", "--config", f.config, "--resume", "--seal")
	if code != 0 || res.Status != basketReplayStatusComplete || !res.MechanicallyComplete || res.Recording == nil || res.Recording.Seal == nil || !res.Recording.Seal.Sealed {
		t.Fatalf("the resumed paper run finishes and seals with no default wall timeout, got exit %d %+v after %s", code, res, took)
	}
	exportDir := filepath.Join(f.parent, "driver", pairPaperExportDirName)
	var man pairRecordingManifest
	mb, err := os.ReadFile(filepath.Join(exportDir, pairRecordingManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportDir, "research.json"), f.doc, 0o600); err != nil {
		t.Fatal(err)
	}
	var objs []map[string]string
	for _, o := range man.Objects {
		m := map[string]string{"kind": o.Kind, "path": o.Path, "sha256": o.SHA256}
		if o.MemberID != "" {
			m["member_id"] = o.MemberID
		}
		objs = append(objs, m)
	}
	replayPlan := map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": f.runID, "state_dir": filepath.Join(f.parent, "driver-replay"),
		"source": map[string]string{"recording": "driver"}, "research_config": map[string]string{"path": "research.json", "sha256": sha256Hex(f.doc)},
		"objects": objs, "scenario": f.scenario.replay(), "bounds": spec.Bounds, "recording": map[string]string{"path": pairRecordingManifestName, "sha256": sha256Hex(mb)}}
	rb, _ := json.Marshal(replayPlan)
	replayPath := filepath.Join(exportDir, "replay-plan.json")
	if err := os.WriteFile(replayPath, rb, 0o600); err != nil {
		t.Fatal(err)
	}
	code, res, _ = drive(replayPath)
	if code != 0 || !res.MechanicallyComplete || res.Comparison == nil || !res.Comparison.Eligible {
		t.Fatalf("the driver replays the sealed recording offline to a mechanical result, got exit %d %+v", code, res)
	}
}

func pairPaperResearchSealedRun(t *testing.T, runID string) (*pairPaperFixture, int64, string, pairPaperProc) {
	t.Helper()
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: runID})
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--seal")
	pairPaperRequireComplete(t, "research oracle run", p)
	if p.Out.Recording == nil || p.Out.Recording.Seal == nil || !p.Out.Recording.Seal.Sealed {
		t.Fatalf("the research run seals: %+v", p.Out.Recording)
	}
	return f, target, dir, p
}

func pairPaperResearchEvent(t *testing.T, out pairReplayOutput, eventMs int64) (map[string]interface{}, pairFundingEventValue) {
	t.Helper()
	want := pairFundingEventID(entryProviderHyperliquid, "BTC", eventMs)
	for _, row := range pairOfflineSection(t, out, "pair_funding_events") {
		if row["event_id"] != want {
			continue
		}
		raw, _ := row["event_json"].(string)
		ev, err := pairDecodeFundingEvent([]byte(raw))
		if err != nil {
			t.Fatalf("the exported research event decodes strictly: %v", err)
		}
		return row, ev
	}
	t.Fatalf("the export holds no funding event %s", want)
	return nil, pairFundingEventValue{}
}

func pairPaperMetaReads(f *pairPaperFixture) int {
	n := 0
	for _, line := range pairPaperCallLogLines(f) {
		if strings.Contains(line, `"metaAndAssetCtxs"`) {
			n++
		}
	}
	return n
}

func TestBasketReplayPairPaperResearchOracleReproducesOffline(t *testing.T) {
	t.Parallel()
	f, target, dir, p := pairPaperResearchSealedRun(t, "paper-research-oracle")
	policy := *f.scenario.FundingPolicy
	prepare, _ := pairPaperOraclePrepareAt(target, policy)
	deadline, _ := pairFundingOracleDeadline(policy, target)
	rows := pairPaperLogRows(t, dir, f.runID)
	ends := map[string]pairRecRow{}
	for _, r := range rows {
		if r.Kind == pairRecEventEnd {
			ends[r.CallID] = r
		}
	}
	var oracleStarts []pairRecRow
	for _, r := range rows {
		if r.Kind == pairRecEventStart && r.Event.Start.Kind == pairRecKindFundingOracle {
			oracleStarts = append(oracleStarts, r)
		}
	}
	if len(oracleStarts) != 1 {
		t.Fatalf("one held hour makes exactly one recorded oracle attempt, got %d", len(oracleStarts))
	}
	st := oracleStarts[0]
	end, ok := ends[st.CallID]
	if !ok || end.Seq <= st.Seq || end.Event.End.Class != pairRecClassResult || st.Event.Start.RawStartMs < prepare || st.Event.Start.SlotMs != target {
		t.Fatalf("the oracle start commits at or after preparation and before its terminal result: start %+v end %+v", st.Event.Start, end.Event)
	}
	var obs pairFundingOracleObservation
	if err := pairReplayStrictDecode(end.Event.End.Result, &obs); err != nil {
		t.Fatalf("the oracle terminal holds the raw observation: %v", err)
	}
	if obs.TargetEventMs != target || obs.RequestMs < target || obs.ReceiptMs > deadline || obs.Price != "60010.5" || end.Event.End.RawCompletionMs > deadline {
		t.Fatalf("the committed observation lies inside [%d, %d]: %+v", target, deadline, obs)
	}
	var consumed bool
	for _, r := range rows {
		if r.Kind == pairRecEventConsume && r.CallID == st.CallID && r.Event.Consume != nil && r.Event.Consume.Disposition == "accepted" {
			consumed = true
		}
	}
	if !consumed {
		t.Fatal("the accepted observation is published by call_consume")
	}
	if n := pairPaperMetaReads(f); n != 1 {
		t.Fatalf("the paper run reads metaAndAssetCtxs once for its one hour, got %d", n)
	}
	row, ev := pairPaperResearchEvent(t, p.Out, target+3)
	if ev.Research == nil || ev.RuleVersion != pairFundingRuleHLReceiptResearch || ev.PriceHash != obs.RecordHash || ev.NativeMark != obs.Price ||
		string(ev.SignedBaseQty) != "0.09" || string(ev.NativeAmount) != "-0.0675118125" || row["status"] != pairFundingStatusApplied {
		t.Fatalf("the held hour settles once from the committed observation: %+v %+v", row, ev)
	}
	var pollsInWait []int64
	for _, r := range rows {
		if r.Kind != pairRecEventStart || r.Event.Start.Kind != pairRecKindMatchPoll || r.Seq >= end.Seq {
			continue
		}
		if ms := r.Event.Start.RawStartMs; ms > prepare && ms <= deadline {
			pollsInWait = append(pollsInWait, ms)
		}
	}
	if len(pollsInWait) == 0 {
		t.Fatal("the host commits a match poll while the oracle read is still pending")
	}
	sort.Slice(pollsInWait, func(i, j int) bool { return pollsInWait[i] < pollsInWait[j] })
	slotLine := regexp.MustCompile(`\] slot (\d{4}-\d\d-\d\dT[0-9:.]+Z): (.*)$`)
	for _, line := range strings.Split(p.Stderr, "\n") {
		m := slotLine.FindStringSubmatch(line)
		if m == nil || !strings.Contains(m[2], "decision frozen") {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			t.Fatalf("slot time %q: %v", m[1], err)
		}
		ms := at.UnixMilli()
		if ms <= prepare || ms > target {
			continue
		}
		preceded := false
		for _, poll := range pollsInWait {
			if poll <= ms {
				preceded = true
			}
		}
		if !preceded {
			t.Fatalf("a decision inside the oracle wait has no earlier committed poll: %s", line)
		}
	}
	reads := pairPaperMetaReads(f)
	playback := pairPaperReproduce(t, f, dir, "reproduction")
	pairOfflineRequireComplete(t, "sealed research playback with every network route blocked", playback)
	if playback.Out.Comparison.Hash != p.Out.Comparison.Hash {
		t.Fatalf("the offline reproduction equals the research run: paper %s, playback %s\n%s", p.Out.Comparison.Hash, playback.Out.Comparison.Hash, pairOfflineDiff(t, p.Out, playback.Out))
	}
	if _, pev := pairPaperResearchEvent(t, playback.Out, target+3); pev.EvidenceHash != ev.EvidenceHash || pev.USDAmount != ev.USDAmount {
		t.Fatalf("the playback rebuilds the same version 3 event: %+v", pev)
	}
	if n := pairPaperMetaReads(f); n != reads {
		t.Fatalf("playback never runs the public oracle reader, saw %d new reads", n-reads)
	}
	for _, line := range pairPaperCallLogLines(f) {
		if strings.Contains(line, `"connect"`) {
			t.Fatalf("no child opens a socket: %s", line)
		}
	}
	exportDir := filepath.Join(dir, pairPaperExportDirName)
	manBlob, err := os.ReadFile(filepath.Join(exportDir, pairRecordingManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]interface{}
	if err := json.Unmarshal(manBlob, &man); err != nil {
		t.Fatal(err)
	}
	if man["v"] != float64(pairRecordingManifestAsyncVer) {
		t.Fatalf("a research recording manifest is version 3, got %v", man["v"])
	}
	tampered := map[string]func(m map[string]interface{}){
		"policy window": func(m map[string]interface{}) {
			m["scenario"].(map[string]interface{})["funding_policy"].(map[string]interface{})["window_ms"] = 6000
		},
		"policy removed": func(m map[string]interface{}) { delete(m["scenario"].(map[string]interface{}), "funding_policy") },
		"binding policy digest": func(m map[string]interface{}) {
			m["funding_binding"].(map[string]interface{})["policy_digest"] = strings.Repeat("0", 64)
		},
	}
	names := make([]string, 0, len(tampered))
	for name := range tampered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var m map[string]interface{}
		_ = json.Unmarshal(manBlob, &m)
		tampered[name](m)
		mb, _ := json.Marshal(m)
		root := filepath.Join(f.parent, "tamper-"+strings.ReplaceAll(name, " ", "-"))
		if err := exec.Command("cp", "-R", exportDir, root).Run(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pairRecordingManifestName), mb, 0o600); err != nil {
			t.Fatal(err)
		}
		blob, _ := os.ReadFile(filepath.Join(exportDir, pairRecordingSpecName))
		var repro map[string]interface{}
		_ = json.Unmarshal(blob, &repro)
		repro["input_manifest"].(map[string]interface{})["root"] = root
		repro["recording"] = map[string]interface{}{"version": man["v"], "path": pairRecordingManifestName, "sha256": sha256Hex(mb), "size_bytes": len(mb)}
		repro["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "tamper-state-"+strings.ReplaceAll(name, " ", "-"))
		got := runPairReplayAdapter(t, f.repo, repro, f.env(true))
		if got.Code == 0 && got.Out.Comparison != nil && got.Out.Comparison.Eligible && got.Out.Comparison.Hash == p.Out.Comparison.Hash {
			t.Fatalf("%s: a changed policy or binding must be refused or fail the equality check", name)
		}
	}
	logBlob, err := os.ReadFile(filepath.Join(exportDir, pairRecordingLogName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBlob)), "\n")
	edited := false
	for i, line := range lines {
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["call_id"] != st.CallID || row["event_kind"] != pairRecEventEnd {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(row["payload"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var ev map[string]interface{}
		if err := json.Unmarshal(payload, &ev); err != nil {
			t.Fatal(err)
		}
		endObj := ev["end"].(map[string]interface{})
		result, err := base64.StdEncoding.DecodeString(endObj["result"].(string))
		if err != nil || !bytes.Contains(result, []byte(`"60010.5"`)) {
			t.Fatalf("the oracle terminal result holds the observation price: %v", err)
		}
		endObj["result"] = base64.StdEncoding.EncodeToString(bytes.Replace(result, []byte(`"60010.5"`), []byte(`"60011.5"`), 1))
		pb, _ := json.Marshal(ev)
		row["payload"] = base64.StdEncoding.EncodeToString(pb)
		rb, _ := json.Marshal(row)
		lines[i] = string(rb)
		edited = true
	}
	if !edited {
		t.Fatal("the sealed recording log holds the oracle terminal")
	}
	tamperRoot := filepath.Join(f.parent, "tamper-observation")
	if err := exec.Command("cp", "-R", exportDir, tamperRoot).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tamperRoot, pairRecordingLogName), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specBlob, _ := os.ReadFile(filepath.Join(exportDir, pairRecordingSpecName))
	var tamperSpec map[string]interface{}
	_ = json.Unmarshal(specBlob, &tamperSpec)
	tamperSpec["input_manifest"].(map[string]interface{})["root"] = tamperRoot
	tamperSpec["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "tamper-observation-state")
	if got := runPairReplayAdapter(t, f.repo, tamperSpec, f.env(true)); got.Code == 0 && got.Out.Comparison != nil && got.Out.Comparison.Eligible {
		t.Fatal("an edited observation in the sealed log must be refused")
	}
}

func TestBasketReplayPairPaperResearchOracleStandaloneFunding(t *testing.T) {
	t.Parallel()
	f, target, dir, p := pairPaperResearchSealedRun(t, "paper-research-standalone")
	exportDir := filepath.Join(dir, pairPaperExportDirName)
	blob, err := os.ReadFile(filepath.Join(exportDir, pairRecordingSpecName))
	if err != nil {
		t.Fatal(err)
	}
	var repro map[string]interface{}
	if err := json.Unmarshal(blob, &repro); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.parent, "standalone-inputs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	objects := repro["input_manifest"].(map[string]interface{})["objects"].([]interface{})
	for _, o := range objects {
		path := o.(map[string]interface{})["path"].(string)
		raw, err := os.ReadFile(filepath.Join(exportDir, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fundingRaw, err := os.ReadFile(filepath.Join(root, "funding.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pairDecodeFundingDocument(fundingRaw)
	if err != nil || doc.Research == nil || len(doc.Research.Observations) != 1 || doc.Research.Policy.FromMs != f.fromMs || doc.Research.Policy.DrainThroughMs != f.drain {
		t.Fatalf("the exported funding object is a strict version 2 document with the policy and its observation: %v", err)
	}
	delete(repro, "recording")
	repro["input_manifest"].(map[string]interface{})["root"] = root
	repro["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "standalone-state")
	repro["run"].(map[string]interface{})["source"] = map[string]string{"funding": "exported research funding document"}
	got := runPairReplayAdapter(t, f.repo, repro, f.env(true))
	if (got.Code != 0 && got.Code != 1) || got.Out.Refusal != nil {
		t.Fatalf("the standalone frozen research replay runs without a refusal, got exit %d refusal %+v error %q\nstderr tail=%s", got.Code, got.Out.Refusal, got.Out.Error, tail(got.Stderr, 4000))
	}
	for _, reason := range got.Out.IncompleteReasons {
		if strings.Contains(reason, "funding") {
			t.Fatalf("the standalone frozen research replay has no funding gap: %v", got.Out.IncompleteReasons)
		}
	}
	paperRow, paperEvent := pairPaperResearchEvent(t, p.Out, target+3)
	row, ev := pairPaperResearchEvent(t, got.Out, target+3)
	if row["status"] != pairFundingStatusApplied || ev.USDAmount != paperEvent.USDAmount || ev.NativeAmount != paperEvent.NativeAmount || ev.PriceHash != paperEvent.PriceHash ||
		ev.Research == nil || ev.Research.ObservationRecordHash != paperEvent.Research.ObservationRecordHash || row["usd_amount"] != paperRow["usd_amount"] {
		t.Fatalf("the frozen version 2 document settles the selected-rule amount the recorded run settled: %+v vs %+v", ev, paperEvent)
	}
	refusals := map[string]func(m map[string]interface{}){
		"policy digest": func(m map[string]interface{}) { m["policy_digest"] = strings.Repeat("1", 64) },
		"policy bounds": func(m map[string]interface{}) {
			m["policy"].(map[string]interface{})["drain_through_ms"] = json.Number(strconv.FormatInt(f.drain+1, 10))
		},
		"observation price": func(m map[string]interface{}) {
			m["observations"].([]interface{})[0].(map[string]interface{})["price"] = "60011.5"
		},
		"observation target": func(m map[string]interface{}) {
			m["observations"].([]interface{})[0].(map[string]interface{})["target_event_ms"] = json.Number(strconv.FormatInt(target+3600000, 10))
		},
	}
	names := make([]string, 0, len(refusals))
	for name := range refusals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dec := json.NewDecoder(bytes.NewReader(fundingRaw))
		dec.UseNumber()
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		refusals[name](m)
		edited, err := pairCanonicalJSON(m)
		if err != nil {
			t.Fatal(err)
		}
		bad := filepath.Join(f.parent, "standalone-"+strings.ReplaceAll(name, " ", "-"))
		if err := exec.Command("cp", "-R", root, bad).Run(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bad, "funding.json"), edited, 0o600); err != nil {
			t.Fatal(err)
		}
		spec := map[string]interface{}{}
		_ = json.Unmarshal(blob, &spec)
		delete(spec, "recording")
		im := spec["input_manifest"].(map[string]interface{})
		im["root"] = bad
		for _, o := range im["objects"].([]interface{}) {
			if obj := o.(map[string]interface{}); obj["path"] == "funding.json" {
				obj["sha256"], obj["size_bytes"] = sha256Hex(edited), len(edited)
			}
		}
		spec["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "standalone-state-"+strings.ReplaceAll(name, " ", "-"))
		got := runPairReplayAdapter(t, f.repo, spec, f.env(true))
		if got.Code != basketReplayExitRefused {
			t.Fatalf("%s: an edited frozen research document is refused before trading, got exit %d %+v", name, got.Code, got.Out.Refusal)
		}
	}
}

type pairPaperOracleOutcome struct {
	starts     []pairRecRow
	ends       []pairRecRow
	admitted   []pairFundingOracleObservation
	refusedSeq []int64
	chainSeqs  map[int64]string
}

func pairPaperOracleOutcomeOf(t *testing.T, dir, runID string) pairPaperOracleOutcome {
	t.Helper()
	rows := pairPaperLogRows(t, dir, runID)
	out := pairPaperOracleOutcome{chainSeqs: map[int64]string{}}
	byCall := map[string]pairRecRow{}
	for _, r := range rows {
		switch {
		case r.Kind == pairRecEventStart && r.Event.Start.Kind == pairRecKindFundingOracle:
			out.starts = append(out.starts, r)
			byCall[r.CallID] = r
		case r.Kind == pairRecEventEnd:
			if _, ok := byCall[r.CallID]; ok {
				out.ends = append(out.ends, r)
			}
		}
	}
	db, err := openStateDBReadOnly(filepath.Join(dir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q, err := db.db.Query(`SELECT seq, batch_json FROM pair_funding_chain ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for q.Next() {
		var seq int64
		var raw string
		if err := q.Scan(&seq, &raw); err != nil {
			t.Fatal(err)
		}
		b, err := pairDecodeFundingBatch([]byte(raw))
		if err != nil {
			t.Fatalf("saved chain batch %d: %v", seq, err)
		}
		out.chainSeqs[seq] = b.BatchHash
		if b.Research != nil {
			out.admitted = append(out.admitted, b.Research.Observations...)
		}
	}
	for _, e := range out.ends {
		if e.Event.End.Class != pairRecClassResult {
			continue
		}
		if b, err := pairDecodeFundingBatch(e.Event.End.Result); err == nil {
			if out.chainSeqs[b.Seq] != b.BatchHash {
				out.refusedSeq = append(out.refusedSeq, b.Seq)
			}
			continue
		}
		var obs pairFundingOracleObservation
		if err := pairReplayStrictDecode(e.Event.End.Result, &obs); err != nil {
			t.Fatalf("oracle terminal %s is neither a batch nor an observation: %v", e.CallID, err)
		}
		found := false
		for _, admitted := range out.admitted {
			if admitted.RecordHash == obs.RecordHash && obs.RecordHash != "" {
				found = true
			}
		}
		if !found {
			out.refusedSeq = append(out.refusedSeq, 0)
		}
	}
	return out
}

func TestBasketReplayPairPaperResearchOracleTiming(t *testing.T) {
	t.Run("decisions use only the saved wake", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		target := int64(1_800_003_600_000)
		policy := pairFundingPolicyV1{Version: pairFundingPolicyVersion, MemberRules: map[string]string{pairTestHL: pairFundingRuleHLReceiptResearch, pairTestOKX: pairFundingRuleOKX},
			WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: target - 120000, DrainThroughMs: target + 7200000}
		prepare, _ := pairPaperOraclePrepareAt(target, policy)
		deadline, _ := pairFundingOracleDeadline(policy, target)
		clock := filepath.Join(t.TempDir(), "clock")
		prevWall := pairPaperWall
		t.Cleanup(func() { pairPaperWall = prevWall })
		decide := func(wake pairRecWake, wallMs int64) ([]string, int64) {
			if err := os.WriteFile(clock, []byte(strconv.FormatInt(wallMs, 10)), 0o600); err != nil {
				t.Fatal(err)
			}
			pairPaperWall = &pairPaperTestWall{path: clock, ms: wallMs}
			rec := newPairRecorder(pairRecLive, "timing-run", "paper", "contract", nil, &simulatedClock{}, pairRecFaults{})
			r := &pairReplayRuntime{rt: e.rt(), rec: rec, oracleNextMs: target, paper: &pairPaperRun{scenario: pairPaperScenario{FundingPolicy: &policy}}}
			trace := r.oracleHourAction(wake)
			return trace, r.oracleNextMs
		}
		cases := []struct {
			name string
			wake pairRecWake
			want string
			next int64
		}{
			{"before preparation", pairRecWake{WallMs: prepare - 1}, "", target},
			{"past the deadline", pairRecWake{WallMs: deadline + 1}, "oracle_missed", target + 3600000},
			{"raw backward wake", pairRecWake{WallMs: prepare + 1000, ClockBackward: true}, "oracle_missed", target + 3600000},
		}
		for _, c := range cases {
			for _, wall := range []int64{c.wake.WallMs - 600000, c.wake.WallMs + 600000} {
				trace, next := decide(c.wake, wall)
				if strings.Join(trace, ",") != c.want || next != c.next {
					t.Fatalf("%s at wall %d: want trace %q next %d, got %v next %d", c.name, wall, c.want, c.next, trace, next)
				}
			}
		}
		late := policy
		late.DrainThroughMs = target + 3000
		rec := newPairRecorder(pairRecLive, "timing-run", "paper", "contract", nil, &simulatedClock{}, pairRecFaults{})
		r := &pairReplayRuntime{rt: e.rt(), rec: rec, oracleNextMs: target, paper: &pairPaperRun{scenario: pairPaperScenario{FundingPolicy: &late}}}
		if trace := r.oracleHourAction(pairRecWake{WallMs: target + 3001}); strings.Join(trace, ",") != "oracle_missed" || r.oracleNextMs != 0 {
			t.Fatalf("drain clips the deadline to drain_through_ms and no later hour exists, got %v next %d", trace, r.oracleNextMs)
		}
	})
	type paperCase struct {
		opts      pairPaperResearchOptions
		admitted  bool
		noStart   bool
		resultEnd bool
		check     func(t *testing.T, target int64, o pairPaperOracleOutcome)
	}
	short := func(more map[string]interface{}) pairPaperResearchOptions {
		return pairPaperResearchOptions{fromBefore: 32000, throughAt: 10000, drainAt: 20000, convergeAt: 1, fixtureMore: more}
	}
	cases := map[string]paperCase{
		"sample at the hour": {opts: short(nil), admitted: true, resultEnd: true, check: func(t *testing.T, target int64, o pairPaperOracleOutcome) {
			if o.admitted[0].RequestMs != target || o.starts[0].Event.Start.RawStartMs < target-30000 {
				t.Fatalf("the reader starts at preparation and requests at the hour: start %d request %d", o.starts[0].Event.Start.RawStartMs, o.admitted[0].RequestMs)
			}
		}},
		"receipt with completion inside the deadline": {opts: short(map[string]interface{}{"oracle_response_delay_ms": 4960}), admitted: true, resultEnd: true, check: func(t *testing.T, target int64, o pairPaperOracleOutcome) {
			if c := o.ends[0].Event.End.RawCompletionMs; o.admitted[0].ReceiptMs != target+4960 || c > target+5000 {
				t.Fatalf("receipt %d and completion %d stay inside the deadline", o.admitted[0].ReceiptMs, c)
			}
		}},
		"receipt at the deadline with late completion": {opts: short(map[string]interface{}{"oracle_response_delay_ms": 5000}), resultEnd: true, check: func(t *testing.T, target int64, o pairPaperOracleOutcome) {
			if c := o.ends[0].Event.End.RawCompletionMs; c <= target+5000 {
				t.Fatalf("the parent completion %d lies after the deadline in this case", c)
			}
			if len(o.admitted) != 0 {
				t.Fatalf("a late completion is not admitted: %+v", o.admitted)
			}
			var seqs []int
			for seq := range o.chainSeqs {
				seqs = append(seqs, int(seq))
			}
			sort.Ints(seqs)
			for i, seq := range seqs {
				if seq != i+1 {
					t.Fatalf("a refused oracle sample allocates no sequence, chain sequences are %v", seqs)
				}
			}
		}},
		"receipt after the deadline": {opts: short(map[string]interface{}{"oracle_response_delay_ms": 5001})},
		"request before the hour":    {opts: short(map[string]interface{}{"oracle_context_mode": "oracle_envelope_override"})},
		"backward terminal":          {opts: short(map[string]interface{}{"oracle_clock_jump_ms": -10000})},
		"testnet host":               {opts: short(map[string]interface{}{"oracle_context_mode": "wrong_host"})},
		"missing context":            {opts: short(map[string]interface{}{"oracle_context_mode": "missing_oracle"})},
		"setup consumes the lead": {opts: short(map[string]interface{}{"oracle_setup_delay_ms": 31000}), admitted: true, resultEnd: true, check: func(t *testing.T, target int64, o pairPaperOracleOutcome) {
			if o.admitted[0].RequestMs <= target {
				t.Fatalf("late setup moves the request after the hour, got %d", o.admitted[0].RequestMs)
			}
		}},
		"first wake after the hour":  {opts: pairPaperResearchOptions{fromBefore: 32000, throughAt: 10000, drainAt: 20000, convergeAt: 1, clockAt: 2000}, admitted: true, resultEnd: true},
		"startup after the deadline": {opts: pairPaperResearchOptions{fromBefore: 32000, throughAt: 10000, drainAt: 20000, convergeAt: 1, clockAt: 6000}, noStart: true},
		"drain clips the deadline":   {opts: pairPaperResearchOptions{fromBefore: 32000, throughAt: -10000, drainAt: 3000, convergeAt: 1, fixtureMore: map[string]interface{}{"oracle_response_delay_ms": 2900}}, admitted: true, resultEnd: true},
		"receipt after the drain":    {opts: pairPaperResearchOptions{fromBefore: 32000, throughAt: -10000, drainAt: 3000, convergeAt: 1, fixtureMore: map[string]interface{}{"oracle_response_delay_ms": 3100}}},
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	slots := make(chan struct{}, 4)
	for _, name := range names {
		c := cases[name]
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			c.opts.runID = "paper-timing-" + strings.ReplaceAll(name, " ", "-")
			f, target := newPairPaperResearchFixture(t, c.opts)
			dir := filepath.Join(f.parent, "paper")
			p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false))
			if p.Code == basketReplayExitRefused || p.Out.Run == nil || !p.Out.Run.Finished {
				t.Fatalf("the timing run finishes, got exit %d %+v %+v\nstderr tail=%s", p.Code, p.Out.Refusal, p.Out.Run, tail(p.Stderr, 4000))
			}
			o := pairPaperOracleOutcomeOf(t, dir, f.runID)
			if c.noStart {
				if len(o.starts) != 0 || len(o.admitted) != 0 {
					t.Fatalf("a first wake after the deadline makes no outside call, got %d starts", len(o.starts))
				}
				return
			}
			if len(o.starts) != 1 || len(o.ends) != 1 {
				t.Fatalf("the hour has exactly one committed attempt with one terminal, got %d starts and %d ends", len(o.starts), len(o.ends))
			}
			if got := o.ends[0].Event.End.Class == pairRecClassResult; got != c.resultEnd {
				t.Fatalf("terminal class %s (cause %q), want result=%v", o.ends[0].Event.End.Class, o.ends[0].Event.End.Cause, c.resultEnd)
			}
			if (len(o.admitted) == 1) != c.admitted || len(o.admitted) > 1 {
				t.Fatalf("admitted observations %+v, want admitted=%v", o.admitted, c.admitted)
			}
			if c.admitted {
				deadline, _ := pairFundingOracleDeadline(*f.scenario.FundingPolicy, target)
				if obs := o.admitted[0]; obs.TargetEventMs != target || obs.RequestMs < target || obs.ReceiptMs > deadline || o.ends[0].Event.End.RawCompletionMs > deadline {
					t.Fatalf("an admitted sample lies inside [%d, %d]: %+v", target, deadline, obs)
				}
			}
			if c.check != nil {
				c.check(t, target, o)
			}
		})
	}
}

func pairPaperPatchFixture(t *testing.T, f *pairPaperFixture, extra map[string]interface{}) {
	t.Helper()
	blob, err := os.ReadFile(f.fixture)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.fixture, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

type pairPaperFill struct {
	id                 string
	side, status       string
	accepted, deadline int64
	event              int64
	bid, ask           string
}

func pairPaperJoinedFills(t *testing.T, dir string) []pairPaperFill {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(dir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.db.Query(`SELECT kind, client_order_id, body_json FROM pair_virtual_events WHERE kind IN ('submit','match') ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	submits := map[string]pairVirtualSubmitBody{}
	var matches []pairVirtualMatchBody
	for rows.Next() {
		var kind, id, body string
		if err := rows.Scan(&kind, &id, &body); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "submit":
			var b pairVirtualSubmitBody
			if err := json.Unmarshal([]byte(body), &b); err != nil {
				t.Fatal(err)
			}
			submits[id] = b
		case "match":
			var b pairVirtualMatchBody
			if err := json.Unmarshal([]byte(body), &b); err != nil {
				t.Fatal(err)
			}
			matches = append(matches, b)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var out []pairPaperFill
	for _, m := range matches {
		s, ok := submits[m.ClientOrderID]
		if !ok {
			continue
		}
		fill := pairPaperFill{id: m.ClientOrderID, side: m.Side, status: m.Status, accepted: s.AcceptedAtMs, deadline: s.AcceptDeadlineMs, event: m.Book.EventMs}
		if len(m.Book.Bids) > 0 {
			fill.bid = m.Book.Bids[0].Px
		}
		if len(m.Book.Asks) > 0 {
			fill.ask = m.Book.Asks[0].Px
		}
		out = append(out, fill)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].accepted != out[j].accepted {
			return out[i].accepted < out[j].accepted
		}
		return out[i].side < out[j].side
	})
	return out
}

func pairPaperOpenOrderRun(t *testing.T, runID string, delay int, gapUntil, throughAt, drainAt int64) (*pairPaperFixture, int64, string, pairPaperProc) {
	t.Helper()
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{
		runID: runID, fromBefore: 40000, throughAt: throughAt, drainAt: drainAt, convergeAt: 12000,
		mutate: func(root map[string]interface{}) {
			limits := pairTestPair(root)["limits"].(map[string]interface{})
			limits["acceptance_deadline_ms"] = 120000
			limits["recovery_age_ms"] = 240000
			limits["quote_age_ms"] = 20000
		},
	})
	f.scenario.PaperPollIntervalMs = 2000
	extra := map[string]interface{}{
		"books_gap_from_ms": target - 38000, "books_gap_until_ms": target + gapUntil,
		"hl_funding": []interface{}{[]interface{}{target + 3, "0.0000125"}, []interface{}{target + 8000, "0.0000125"}, []interface{}{target + 40000, "0.0000125"}},
	}
	if delay > 0 {
		extra["oracle_response_delay_ms"] = delay
	}
	pairPaperPatchFixture(t, f, extra)
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--seal")
	return f, target, dir, p
}

func TestBasketReplayPairPaperResearchOracleOpenOrder(t *testing.T) {
	held, target, heldDir, heldRun := pairPaperOpenOrderRun(t, "paper-open-order-held", 3500, 2000, 25000, 50000)
	prepare, _ := pairPaperOraclePrepareAt(target, *held.scenario.FundingPolicy)
	pairPaperRequireComplete(t, "an order accepted before preparation fills after the hour while the oracle waits", heldRun)
	if heldRun.Out.Recording == nil || heldRun.Out.Recording.Seal == nil || !heldRun.Out.Recording.Seal.Sealed {
		t.Fatalf("the open-order run seals: %+v", heldRun.Out.Recording)
	}
	heldFills := pairPaperEntryFills(t, heldDir, prepare, target)
	fast, fastTarget, fastDir, fastRun := pairPaperOpenOrderRun(t, "paper-open-order-fast", 0, 2000, 25000, 50000)
	pairPaperRequireComplete(t, "the same books fill when the oracle does not wait", fastRun)
	fastPrepare, _ := pairPaperOraclePrepareAt(fastTarget, *fast.scenario.FundingPolicy)
	fastFills := pairPaperEntryFills(t, fastDir, fastPrepare, fastTarget)
	if got, want := pairPaperFillKey(heldFills), pairPaperFillKey(fastFills); got != want {
		t.Fatalf("the waiting oracle and the immediate oracle fill the same eligible books:\nheld %s\nfast %s", got, want)
	}
	_ = fast
	missing, _, _, missingRun := pairPaperOpenOrderRun(t, "paper-open-order-missing", 1000, 500000, 8000, 20000)
	if missingRun.Out.Run == nil || !missingRun.Out.Run.Finished || missingRun.Out.MechanicallyComplete || missingRun.Out.Status != basketReplayStatusIncomplete {
		t.Fatalf("a missing book keeps the finished run incomplete, got exit %d status %s complete=%v reasons=%v\nstderr tail=%s", missingRun.Code, missingRun.Out.Status, missingRun.Out.MechanicallyComplete, missingRun.Out.IncompleteReasons, tail(missingRun.Stderr, 4000))
	}
	var named bool
	for _, reason := range missingRun.Out.IncompleteReasons {
		if strings.Contains(reason, "no saved match or absence proof") || strings.Contains(reason, "unresolved") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the missing book leaves an unresolved order, got %v", missingRun.Out.IncompleteReasons)
	}
	if submits := pairPaperSubmitCount(t, filepath.Join(missing.parent, "paper")); submits == 0 {
		t.Fatal("the missing-book run still accepted an entry")
	}
}

func pairPaperEntryFills(t *testing.T, dir string, prepare, hour int64) []pairPaperFill {
	t.Helper()
	var out []pairPaperFill
	for _, f := range pairPaperJoinedFills(t, dir) {
		if f.accepted >= prepare {
			continue
		}
		if f.event <= hour || f.event >= f.deadline || f.status != basketVenueFilled {
			t.Fatalf("an entry accepted at %d before preparation %d fills from a book after the hour %d and before its deadline %d, got %+v", f.accepted, prepare, hour, f.deadline, f)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("the run accepted no entry before preparation")
	}
	return out
}

func pairPaperFillKey(fills []pairPaperFill) string {
	parts := make([]string, 0, len(fills))
	for _, f := range fills {
		parts = append(parts, fmt.Sprintf("%s %s %d %s %s", f.side, f.status, f.event, f.bid, f.ask))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func pairPaperSubmitCount(t *testing.T, dir string) int {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(dir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pair_virtual_events WHERE kind = 'submit'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBasketReplayPairPaperResearchOracleHostContinues(t *testing.T) {
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{
		runID: "paper-oracle-progress", fromBefore: 32000, throughAt: 10000, drainAt: 20000, convergeAt: 1,
		fixtureMore: map[string]interface{}{"oracle_response_delay_ms": 2000},
	})
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false))
	if p.Code == basketReplayExitRefused || p.Out.Run == nil || !p.Out.Run.Finished {
		t.Fatalf("the progress run finishes, got exit %d %+v\nstderr tail=%s", p.Code, p.Out.Refusal, tail(p.Stderr, 4000))
	}
	policy := *f.scenario.FundingPolicy
	prepare, _ := pairPaperOraclePrepareAt(target, policy)
	deadline, _ := pairFundingOracleDeadline(policy, target)
	rows := pairPaperLogRows(t, dir, f.runID)
	var oracleStart, oracleEnd int64
	var pollTimes []int64
	seenPoll := map[int64]bool{}
	var managed, funded bool
	for _, r := range rows {
		if r.Kind == pairRecEventStart && r.Event.Start != nil && r.Event.Start.Kind == pairRecKindMatchPoll {
			ms := r.Event.Start.RawStartMs
			if !seenPoll[ms] {
				seenPoll[ms] = true
				pollTimes = append(pollTimes, ms)
			}
		}
		if r.Kind == pairRecEventStart && r.Event.Start != nil && r.Event.Start.Kind == pairRecKindFundingOracle {
			oracleStart = r.Seq
		}
		if r.Kind == pairRecEventEnd && oracleStart > 0 && r.Seq > oracleStart && oracleEnd == 0 {
			if st := r.Event.End; st != nil {
				for _, s := range rows {
					if s.CallID == r.CallID && s.Kind == pairRecEventStart && s.Event.Start != nil && s.Event.Start.Kind == pairRecKindFundingOracle {
						oracleEnd = r.Seq
					}
				}
			}
		}
	}
	if oracleStart == 0 || oracleEnd <= oracleStart {
		t.Fatalf("the hour records an oracle start before its terminal, start %d end %d", oracleStart, oracleEnd)
	}
	sort.Slice(pollTimes, func(i, j int) bool { return pollTimes[i] < pollTimes[j] })
	var during int
	var brackets bool
	for i, ms := range pollTimes {
		if i > 0 && pollTimes[i]-pollTimes[i-1] > int64(f.scenario.PaperPollIntervalMs) {
			t.Fatalf("match polls gap %d ms, above one interval %d: %v", pollTimes[i]-pollTimes[i-1], f.scenario.PaperPollIntervalMs, pollTimes)
		}
		if i > 0 && pollTimes[i-1] <= deadline && ms >= deadline && ms-pollTimes[i-1] <= int64(f.scenario.PaperPollIntervalMs) {
			brackets = true
		}
	}
	for _, r := range rows {
		if r.Seq <= oracleStart || r.Seq >= oracleEnd || r.Kind != pairRecEventStart || r.Event.Start == nil {
			continue
		}
		switch r.Event.Start.Kind {
		case pairRecKindMatchPoll:
			if r.Event.Start.RawStartMs > prepare && r.Event.Start.RawStartMs <= deadline {
				during++
			}
		case pairRecKindCandles, pairRecKindMarks, pairRecKindNativeCheck:
			managed = true
		case pairRecKindFunding:
			funded = true
		}
	}
	if during < 2 || !brackets || !managed || !funded {
		t.Fatalf("while the oracle is pending the host records polls (%d), a deadline bracket (%v), management (%v) and a funding read (%v)", during, brackets, managed, funded)
	}
	var decided bool
	slotLine := regexp.MustCompile(`\] slot (\d{4}-\d\d-\d\dT[0-9:.]+Z): (.*)$`)
	for _, line := range strings.Split(p.Stderr, "\n") {
		m := slotLine.FindStringSubmatch(line)
		if m == nil || !strings.Contains(m[2], "decision frozen") {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			t.Fatal(err)
		}
		ms := at.UnixMilli()
		if ms > prepare && ms <= deadline {
			decided = true
		}
	}
	if !decided {
		t.Fatal("an eligible decision is recorded from a fresh quote while the oracle read is pending")
	}
}
