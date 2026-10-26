//go:build pyintegration

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	slots := make(chan struct{}, 2)
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
		l.Prefix = pairRecPrefixHash(f.runID, l.Seq, l.Partition, l.ContractFP, l.Kind, l.CallID, prev, l.PayloadHash)
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
