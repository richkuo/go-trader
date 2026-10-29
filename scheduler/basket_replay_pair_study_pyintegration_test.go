//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func pairStudyPython(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Join(repo, ".venv", "bin", "python3")
}

func pairStudyAbsent(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("the refusal must leave %s unclaimed: %v", dir, err)
	}
}

func pairStudySchedule(slots map[int64]map[string]pairPolicyMemberTarget) json.RawMessage {
	body := map[string]map[string]pairPolicyMemberTarget{}
	for ms, targets := range slots {
		body[strconv.FormatInt(ms, 10)] = targets
	}
	blob, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return blob
}

func pairStudyLegs(hl, okx, hlPx, okxPx float64) map[string]pairPolicyMemberTarget {
	return map[string]pairPolicyMemberTarget{
		pairTestHL:  {TargetBaseQty: hl, PriceBound: hlPx},
		pairTestOKX: {TargetBaseQty: okx, PriceBound: okxPx},
	}
}

func pairStudyInput(mode string, schedule json.RawMessage) *pairReplayTargetInput {
	return &pairReplayTargetInput{Mode: mode, Schedule: schedule}
}

func pairStudyHoldText(t *testing.T, out pairReplayOutput) string {
	t.Helper()
	var b strings.Builder
	for _, rb := range pairOfflineSection(t, out, "basket_rebalances") {
		fmt.Fprintf(&b, "%s %s\n", rb["status"], rb["hold_reason"])
	}
	return b.String()
}

func pairStudyPurposes(t *testing.T, out pairReplayOutput) map[string]int {
	t.Helper()
	purposes := map[string]int{}
	for _, a := range pairOfflineSection(t, out, "basket_member_actions") {
		purposes[fmt.Sprint(a["purpose"])]++
	}
	return purposes
}

func TestBasketReplayPairStudyRefusesTargetInputBeforeClaim(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-refuse", durationS: 30})
	base := f.spec(basketReplayModeNew, f.stateDir("unused"))
	refuse := func(t *testing.T, name string, spec interface{}, raw []byte, want string) {
		t.Helper()
		dir := f.stateDir(name)
		var p pairReplayProc
		if raw != nil {
			bin := pyintegrationBinary(t)
			path := filepath.Join(t.TempDir(), name+".json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "basket-replay", "--spec", path)
			cmd.Dir = f.repo
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			p = pairReplayProc{Raw: stdout.Bytes(), Stderr: stderr.String()}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				p.Code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(p.Raw, &p.Out); err != nil {
				t.Fatalf("stdout is not a refusal document: %v\n%s", err, p.Raw)
			}
		} else {
			specValue := spec
			if s, ok := specValue.(*pairReplaySpec); ok {
				s.Run.StateDir = dir
				specValue = s
			}
			p = runPairReplayAdapter(t, f.repo, specValue, nil)
		}
		if p.Code != basketReplayExitRefused || p.Out.Refusal == nil || !strings.Contains(strings.Join(p.Out.Refusal.Detail, "\n")+p.Out.Refusal.Reason, want) {
			t.Fatalf("want a refusal containing %q, got exit %d %+v\nstderr %s", want, p.Code, p.Out.Refusal, tail(p.Stderr, 2000))
		}
		pairStudyAbsent(t, dir)
	}
	t.Run("null", func(t *testing.T) {
		blob, _ := json.Marshal(base)
		raw := bytes.Replace(blob, []byte(`"protocol"`), []byte(`"target_input":null,"protocol"`), 1)
		refuse(t, "null", nil, raw, "null is refused")
	})
	t.Run("duplicate key", func(t *testing.T) {
		blob, _ := json.Marshal(base)
		raw := bytes.Replace(blob, []byte(`"protocol"`), []byte(`"target_input":{"mode":"no_trade"},"target_input":{"mode":"policy"},"protocol"`), 1)
		refuse(t, "duplicate", nil, raw, "duplicate")
	})
	t.Run("unknown mode", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("mode"))
		spec.TargetInput = pairStudyInput("later", nil)
		refuse(t, "mode", spec, nil, "frozen_schedule")
	})
	t.Run("policy with schedule", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("policy-schedule"))
		spec.TargetInput = pairStudyInput(pairTargetModePolicy, json.RawMessage(`{}`))
		refuse(t, "policy-schedule", spec, nil, "carries no schedule")
	})
	t.Run("unknown member", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("member"))
		slot := strconv.FormatInt(f.t0, 10)
		spec.TargetInput = pairStudyInput(pairTargetModeFrozen, json.RawMessage(`{"`+slot+`":{"`+pairTestHL+`":{"target_base_qty":0.06,"price_bound":60400},"other":{"target_base_qty":-0.06,"price_bound":59700}}}`))
		refuse(t, "member", spec, nil, "unknown member")
	})
	t.Run("bad slot", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("slot"))
		spec.TargetInput = pairStudyInput(pairTargetModeFrozen, json.RawMessage(`{"001":{"`+pairTestHL+`":{"target_base_qty":0.06,"price_bound":60400},"`+pairTestOKX+`":{"target_base_qty":-0.06,"price_bound":59700}}}`))
		refuse(t, "slot", spec, nil, "canonical")
	})
	t.Run("nonpositive bound", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("bound"))
		slot := strconv.FormatInt(f.t0, 10)
		spec.TargetInput = pairStudyInput(pairTargetModeFrozen, json.RawMessage(`{"`+slot+`":{"`+pairTestHL+`":{"target_base_qty":0.06,"price_bound":0},"`+pairTestOKX+`":{"target_base_qty":-0.06,"price_bound":59700}}}`))
		refuse(t, "bound", spec, nil, "above zero")
	})
	t.Run("recording", func(t *testing.T) {
		spec := f.spec(basketReplayModeNew, f.stateDir("recording"))
		spec.TargetInput = pairStudyInput(pairTargetModeNoTrade, nil)
		spec.Recording = &pairReplayRecordingRef{Version: 1, Path: "recording-manifest.json", SHA256: strings.Repeat("ab", 32), SizeBytes: 10}
		spec.Scenario.VenueFailures = []pairReplayFailureScript{}
		refuse(t, "recording", spec, nil, "refused with a recording")
	})
	t.Run("paper", func(t *testing.T) {
		paper := newPairPaperFixture(t, pairPaperOptions2{runID: "pair-study-paper"})
		dir := filepath.Join(paper.parent, "paper-target")
		spec := paper.spec(basketReplayModeNew, dir)
		blob, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		raw := bytes.Replace(blob, []byte(`"protocol"`), []byte(`"target_input":{"mode":"no_trade"},"protocol"`), 1)
		path := filepath.Join(t.TempDir(), "paper-spec.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		bin := pyintegrationBinary(t)
		cmd := exec.Command(bin, "basket-replay-paper", "--spec", path, "--config", paper.config)
		cmd.Dir = paper.repo
		cmd.Env = append(os.Environ(), paper.env(true)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		var exitErr *exec.ExitError
		code := 0
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		var out pairReplayOutput
		if json.Unmarshal(stdout.Bytes(), &out) != nil || code != basketReplayExitRefused || out.Refusal == nil {
			t.Fatalf("paper must refuse target_input, got exit %d stdout %s stderr %s", code, stdout.String(), tail(stderr.String(), 2000))
		}
		pairStudyAbsent(t, dir)
	})
	t.Run("driver", func(t *testing.T) {
		dir := f.stateDir("driver")
		plan := map[string]interface{}{"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": "pair-study-driver", "state_dir": dir,
			"source": map[string]string{"quotes": "fixture"}, "research_config": map[string]string{"path": "missing.json", "sha256": strings.Repeat("ab", 32)},
			"objects":  []map[string]string{{"kind": "evidence", "path": "evidence.json", "sha256": strings.Repeat("cd", 32)}},
			"scenario": map[string]interface{}{}, "bounds": map[string]string{"start": "2026-10-01T00:00:00Z", "end": "2026-10-01T01:00:00Z"},
			"target_input": nil}
		blob, _ := json.Marshal(plan)
		planPath := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(planPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		outPath := filepath.Join(t.TempDir(), "result.json")
		cmd := exec.Command(pairStudyPython(t, f.repo), filepath.Join(f.repo, "backtest", "basket_replay.py"), "--plan", planPath, "--binary", filepath.Join(t.TempDir(), "unused"), "--out", outPath)
		cmd.Dir = f.repo
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("the driver must refuse a null target_input with exit 3, got %v\n%s", err, out)
		}
		pairStudyAbsent(t, dir)
		plan["target_input"] = map[string]string{"mode": "no_trade"}
		plan["collection"] = map[string]int64{"from_ms": 1, "through_ms": 2, "drain_through_ms": 3}
		blob, _ = json.Marshal(plan)
		if err := os.WriteFile(planPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		outPath = filepath.Join(t.TempDir(), "result-collection.json")
		cmd = exec.Command(pairStudyPython(t, f.repo), filepath.Join(f.repo, "backtest", "basket_replay.py"), "--plan", planPath, "--binary", filepath.Join(t.TempDir(), "unused"), "--out", outPath, "--paper", "--config", filepath.Join(t.TempDir(), "egress.json"))
		cmd.Dir = f.repo
		out, err = cmd.CombinedOutput()
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !bytes.Contains(out, []byte("refused with a paper collection")) {
			t.Fatalf("the driver must refuse target_input with a collection, got %v\n%s", err, out)
		}
		pairStudyAbsent(t, dir)
	})
}

func TestBasketReplayPairStudyOmissionKeepsTheFingerprint(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-omit", durationS: 30, mutate: func(root map[string]interface{}) {
		pairTestPair(root)["params"].(map[string]interface{})["entry_threshold_bps"] = 9000
	}})
	dir := f.stateDir("omit")
	spec := f.spec(basketReplayModeNew, dir)
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("target_input")) {
		t.Fatal("an omitted target input must stay out of the specification bytes")
	}
	want, err := pairReplayInputFingerprint(blob)
	if err != nil {
		t.Fatal(err)
	}
	p := runPairReplayAdapter(t, f.repo, spec, nil, "--max-steps", "1")
	if p.Out.Run == nil || p.Out.Run.Fingerprints.Input != want || p.Out.Refusal != nil {
		t.Fatalf("the omitted specification fingerprint must pass through unchanged, got %+v refusal %+v", p.Out.Run, p.Out.Refusal)
	}
	explicit := f.spec(basketReplayModeNew, f.stateDir("policy"))
	explicit.TargetInput = pairStudyInput(pairTargetModePolicy, nil)
	blob, _ = json.Marshal(explicit)
	other, err := pairReplayInputFingerprint(blob)
	if err != nil {
		t.Fatal(err)
	}
	if other == want {
		t.Fatal("an explicit policy target input must change the fingerprint")
	}
	got := runPairReplayAdapter(t, f.repo, explicit, nil, "--max-steps", "1")
	if got.Out.Run == nil || got.Out.Run.Fingerprints.Input != other || got.Out.Refusal != nil {
		t.Fatalf("explicit policy mode must be admitted and bound, got %+v refusal %+v", got.Out.Run, got.Out.Refusal)
	}
}

func TestBasketReplayPairStudyNoTradeHoldsAndResumes(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-none", durationS: 90})
	specFor := func(dir string) *pairReplaySpec {
		spec := f.spec(basketReplayModeNew, dir)
		spec.TargetInput = pairStudyInput(pairTargetModeNoTrade, nil)
		return spec
	}
	base := runPairReplayAdapter(t, f.repo, specFor(f.stateDir("base")), nil)
	pairOfflineRequireComplete(t, "no trade", base)
	if n := len(pairOfflineSection(t, base.Out, "basket_member_actions")); n != 0 || !strings.Contains(pairStudyHoldText(t, base.Out), pairTargetHoldNoTrade) {
		t.Fatalf("no trade must hold without actions, holds:\n%s", pairStudyHoldText(t, base.Out))
	}
	dir := f.stateDir("fault")
	crash := runPairReplayAdapter(t, f.repo, specFor(dir), nil, "--fault", "exit-after-book-commit:2")
	if crash.Code != basketReplayExitFault {
		t.Fatalf("the fault must exit 75, got %d", crash.Code)
	}
	resumedSpec := specFor(dir)
	resumedSpec.Mode = basketReplayModeResume
	resumed := runPairReplayAdapter(t, f.repo, resumedSpec, nil)
	pairOfflineRequireComplete(t, "resumed no trade", resumed)
	if resumed.Out.Comparison.Hash != base.Out.Comparison.Hash || !resumed.Out.Comparison.Eligible {
		t.Fatalf("an unchanged no-trade schedule must resume to the same eligible comparison\n%s", pairOfflineDiff(t, base.Out, resumed.Out))
	}
	changedDir := f.stateDir("changed")
	runPairReplayAdapter(t, f.repo, specFor(changedDir), nil, "--max-steps", "2")
	changed := specFor(changedDir)
	changed.Mode = basketReplayModeResume
	changed.TargetInput = pairStudyInput(pairTargetModePolicy, nil)
	refused := runPairReplayAdapter(t, f.repo, changed, nil)
	if refused.Code != basketReplayExitRefused || refused.Out.Refusal == nil || !strings.Contains(strings.Join(refused.Out.Refusal.Detail, "\n"), "input") {
		t.Fatalf("a changed target input must refuse resume before trading, got %d %+v", refused.Code, refused.Out.Refusal)
	}
}

func TestBasketReplayPairStudyFrozenScheduleKeepsTheGates(t *testing.T) {
	run := func(t *testing.T, name string, hl, okx float64, full bool, mutate func(map[string]interface{})) (pairReplayProc, int64) {
		t.Helper()
		dur := int64(90)
		if full {
			dur = 240
		}
		f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-" + name, durationS: dur, mutate: mutate})
		slot := f.t0
		hlPx, okxPx, exitHL, exitOKX := 60400.0, 59700.0, 59500.0, 60600.0
		if hl < 0 {
			hlPx, okxPx, exitHL, exitOKX = 59000, 62500, 61000, 59000
		}
		slots := map[int64]map[string]pairPolicyMemberTarget{slot: pairStudyLegs(hl, okx, hlPx, okxPx)}
		if full {
			slots[slot+120000] = pairStudyLegs(hl*2, okx*2, hlPx, okxPx)
			slots[slot+180000] = pairStudyLegs(0, 0, exitHL, exitOKX)
		}
		spec := f.spec(basketReplayModeNew, f.stateDir(name))
		spec.TargetInput = pairStudyInput(pairTargetModeFrozen, pairStudySchedule(slots))
		p := runPairReplayAdapter(t, f.repo, spec, nil)
		return p, slot
	}
	t.Run("long hyperliquid", func(t *testing.T) {
		p, _ := run(t, "long-hl", 0.06, -0.06, true, nil)
		pairOfflineRequireComplete(t, "long hyperliquid schedule", p)
		text := pairStudyHoldText(t, p.Out)
		purposes := pairStudyPurposes(t, p.Out)
		if purposes[pairPurposeEntry] < 2 || purposes[pairPurposeExit] < 2 || !strings.Contains(text, pairTargetHoldMissing) || !strings.Contains(text, pairTargetHoldConflict) {
			t.Fatalf("the schedule must enter, miss a slot, hold a lifecycle conflict and exit, purposes %v holds:\n%s", purposes, text)
		}
	})
	t.Run("long okx", func(t *testing.T) {
		p, _ := run(t, "long-okx", -0.06, 0.06, true, nil)
		pairOfflineRequireComplete(t, "long okx schedule", p)
		if pairStudyPurposes(t, p.Out)[pairPurposeEntry] < 2 {
			t.Fatalf("the opposite orientation must enter, purposes %v holds:\n%s", pairStudyPurposes(t, p.Out), pairStudyHoldText(t, p.Out))
		}
	})
	t.Run("illegal lot", func(t *testing.T) {
		p, _ := run(t, "lot", 0.055, -0.055, false, nil)
		pairOfflineRequireComplete(t, "illegal lot", p)
		if len(pairOfflineSection(t, p.Out, "basket_member_actions")) != 0 || !strings.Contains(pairStudyHoldText(t, p.Out), "not a legal lot") {
			t.Fatalf("an illegal lot must hold inside the coordinator, holds:\n%s", pairStudyHoldText(t, p.Out))
		}
	})
	t.Run("cap", func(t *testing.T) {
		p, _ := run(t, "cap", 0.06, -0.06, false, func(root map[string]interface{}) {
			pairTestPair(root)["limits"].(map[string]interface{})["gross_budget_usd"] = 100
			pairTestPair(root)["limits"].(map[string]interface{})["hyperliquid_notional_cap_usd"] = 100
			pairTestPair(root)["limits"].(map[string]interface{})["okx_notional_cap_usd"] = 100
			pairTestPair(root)["limits"].(map[string]interface{})["unmatched_cap_usd"] = 100
		})
		pairOfflineRequireComplete(t, "cap", p)
		if len(pairOfflineSection(t, p.Out, "basket_member_actions")) != 0 || !strings.Contains(pairStudyHoldText(t, p.Out), "budget") {
			t.Fatalf("a notional above the cap must hold, holds:\n%s", pairStudyHoldText(t, p.Out))
		}
	})
	t.Run("rounded bound", func(t *testing.T) {
		f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-round", durationS: 90})
		spec := f.spec(basketReplayModeNew, f.stateDir("round"))
		spec.TargetInput = pairStudyInput(pairTargetModeFrozen, pairStudySchedule(map[int64]map[string]pairPolicyMemberTarget{
			f.t0: pairStudyLegs(0.06, -0.06, 1, 1),
		}))
		p := runPairReplayAdapter(t, f.repo, spec, nil)
		pairOfflineRequireComplete(t, "rounded bound", p)
		text := pairStudyHoldText(t, p.Out)
		if len(pairOfflineSection(t, p.Out, "basket_member_actions")) != 0 || (!strings.Contains(text, "bound") && !strings.Contains(text, "round") && !strings.Contains(text, "reach")) {
			t.Fatalf("a bound that cannot reach the book must hold, holds:\n%s", text)
		}
	})
}

func pairStudyCopy(t *testing.T, repo string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "study")
	cmd := exec.Command("cp", "-R", filepath.Join(repo, "backtest", "candidates", "cross_exchange_perp_arbitrage_1655"), dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy study: %v\n%s", err, out)
	}
	for _, name := range []string{"protocol.lock.json", "results.json"} {
		if err := os.Remove(filepath.Join(dst, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return dst
}

func pairStudyRun(t *testing.T, repo, study string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(pairStudyPython(t, repo), append([]string{filepath.Join(repo, "backtest", "candidates", "cross_exchange_perp_arbitrage_1655", "run_study.py"), "--study", study}, args...)...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestBasketReplayPairStudyRunnerLocksAnInconclusiveReport(t *testing.T) {
	repo := basketPyRoot(t)
	src := filepath.Join(repo, "backtest", "candidates", "cross_exchange_perp_arbitrage_1655")
	bin := pyintegrationBinary(t)
	study := pairStudyCopy(t, repo)
	if code, out := pairStudyRun(t, repo, study, "validate"); code != 0 {
		t.Fatalf("validate: %s", out)
	}
	if code, out := pairStudyRun(t, repo, study, "lock", "--binary", bin); code != 0 {
		t.Fatalf("lock: %s", out)
	}
	work := filepath.Join(t.TempDir(), "work")
	if code, out := pairStudyRun(t, repo, study, "run", "--binary", bin, "--work", work); code != 0 {
		t.Fatalf("run: %s", out)
	}
	result := map[string]interface{}{}
	blob, err := os.ReadFile(filepath.Join(study, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(blob, &result) != nil || result["verdict"] != "inconclusive" || result["scored"] != false || result["promotion_eligible"] != false {
		t.Fatalf("the missing-evidence study must stay unscored and inconclusive: %s", blob)
	}
	if _, err := os.Lstat(filepath.Join(work, "state")); !os.IsNotExist(err) {
		t.Fatal("an inconclusive study must not claim a replay state directory")
	}
	if code, out := pairStudyRun(t, repo, study, "render"); code != 0 {
		t.Fatalf("render: %s", out)
	}
	if code, out := pairStudyRun(t, repo, study, "check"); code != 0 {
		t.Fatalf("render must leave the lock valid: %s", out)
	}
	tampered := pairStudyCopy(t, repo)
	if code, out := pairStudyRun(t, repo, tampered, "lock", "--binary", bin); code != 0 {
		t.Fatalf("tamper lock: %s", out)
	}
	if err := os.WriteFile(filepath.Join(tampered, "plans", "extra.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "tamper-work")
	if code, out := pairStudyRun(t, repo, tampered, "run", "--binary", bin, "--work", state); code != 3 || !strings.Contains(out, "lock_mismatch") {
		t.Fatalf("an added source file must refuse before replay, got %d\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(state, "state")); !os.IsNotExist(err) {
		t.Fatal("a lock refusal must not claim state")
	}
	other := filepath.Join(t.TempDir(), "other-binary")
	if err := os.WriteFile(other, []byte("not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := pairStudyRun(t, repo, tampered, "run", "--binary", other, "--work", filepath.Join(t.TempDir(), "bin-work")); code != 3 || !strings.Contains(out, "binary") {
		t.Fatalf("a changed binary must refuse, got %d\n%s", code, out)
	}

	cache := t.TempDir()
	remote := filepath.Join(cache, "remote")
	key := "market_data/sample.json"
	payload := []byte("archive-bytes")
	if err := os.MkdirAll(filepath.Join(remote, "market_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, key), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	stubDir := filepath.Join(cache, "bin")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!" + pairStudyPython(t, repo) + "\nimport os, shutil, sys\nargs=sys.argv[1:]\nremote=os.environ['STUB_REMOTE']\nlog=os.environ['STUB_LOG']\nopen(log,'a').write(' '.join(args)+'\\n')\n" +
		"assert args[-2:]==['--request-payer','requester']\n" +
		"if args[:2]==['s3','cp']:\n    key=args[2].split('/',3)[3]\n    src=os.path.join(remote,key)\n    \n" +
		"    if not os.path.exists(src):\n        sys.stderr.write('fatal error: An error occurred (404) when calling the HeadObject operation: Not Found\\n'); sys.exit(1)\n" +
		"    shutil.copyfile(src,args[3]); sys.exit(0)\n" +
		"if args[:2]==['s3api','head-object']:\n    key=args[args.index('--key')+1]\n" +
		"    if os.path.exists(os.path.join(remote,key)): sys.exit(0)\n" +
		"    sys.stderr.write('An error occurred (404) when calling the HeadObject operation: Not Found\\n'); sys.exit(254)\n" +
		"sys.exit(2)\n"
	if err := os.WriteFile(filepath.Join(stubDir, "aws"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(cache, "aws.log")
	manifest := map[string]interface{}{"bucket": "stub-bucket", "files": []map[string]interface{}{
		{"key": key, "status": "present", "size": len(payload), "sha256": sha256Hex(payload)},
		{"key": "market_data/missing.json", "status": "absent"},
	}}
	manPath := filepath.Join(cache, "manifest.json")
	blob, _ = json.Marshal(manifest)
	if err := os.WriteFile(manPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(pairStudyPython(t, repo), filepath.Join(src, "run_study.py"), "cache-check", "--manifest", manPath, "--cache", filepath.Join(cache, "objects"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_REMOTE="+remote, "STUB_LOG="+log)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cache-check: %v\n%s", err, out)
	}
	var report struct {
		First  map[string]interface{} `json:"first"`
		Second map[string]interface{} `json:"second"`
	}
	if json.Unmarshal(out, &report) != nil || report.First["downloaded"] != float64(1) || report.Second["downloaded"] != float64(0) || report.Second["verified"] != float64(1) {
		t.Fatalf("the second cache read must not download: %s", out)
	}
	downloads := 0
	for _, line := range strings.Split(string(mustRead(t, log)), "\n") {
		if strings.Contains(line, "s3 cp") {
			downloads++
		}
	}
	if downloads != 1 {
		t.Fatalf("the stub must see one download, got %d\n%s", downloads, mustRead(t, log))
	}
	cached := filepath.Join(cache, "objects", "stub-bucket", key)
	if err := os.Chmod(cached, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("altered"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(pairStudyPython(t, repo), filepath.Join(src, "run_study.py"), "cache-check", "--manifest", manPath, "--cache", filepath.Join(cache, "objects"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_REMOTE="+remote, "STUB_LOG="+log)
	out, err = cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !bytes.Contains(out, []byte("sha256")) {
		t.Fatalf("an altered cache object must refuse, got %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

func TestBasketReplayPairStudyRunnerReplaysAFixture(t *testing.T) {
	f := newPairOfflineFixture(t, pairOfflineOptions{runID: "pair-study-fixture", durationS: 90})
	repo := f.repo
	bundle := t.TempDir()
	for _, obj := range f.objects {
		blob, err := os.ReadFile(filepath.Join(f.inputs, obj.Path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, obj.Path), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "research-config.json"), f.doc, 0o644); err != nil {
		t.Fatal(err)
	}
	var objs []map[string]interface{}
	for _, obj := range f.objects {
		item := map[string]interface{}{"kind": obj.Kind, "path": obj.Path, "sha256": obj.SHA256}
		if obj.MemberID != "" {
			item["member_id"] = obj.MemberID
		}
		objs = append(objs, item)
	}
	plan := map[string]interface{}{
		"plan": "go-trader.basket-replay-plan", "version": 2, "run_id": f.runID, "state_dir": "RUNTIME",
		"source":          map[string]string{"quotes": "mechanical fixture"},
		"research_config": map[string]string{"path": "research-config.json", "sha256": sha256Hex(f.doc)},
		"objects":         objs, "scenario": f.scenario, "bounds": f.spec(basketReplayModeNew, "/x").Bounds,
		"target_input": map[string]string{"mode": "no_trade"},
	}
	blob, _ := json.Marshal(plan)
	if err := os.WriteFile(filepath.Join(bundle, "plan.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]interface{}{"classification": "mechanical_fixture", "source_identity": "integration fixture", "funding_policy": nil, "recording_family": nil,
		"standalone_notes": []string{"synthetic accounts and conversions are mechanical diagnostics"}}
	blob, _ = json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(bundle, "bundle.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	study := pairStudyCopy(t, repo)
	bin := pyintegrationBinary(t)
	if code, out := pairStudyRun(t, repo, study, "prepare", "--bundle", bundle); code != 0 {
		t.Fatalf("prepare: %s", out)
	}
	if code, out := pairStudyRun(t, repo, study, "lock", "--binary", bin); code != 0 {
		t.Fatalf("lock: %s", out)
	}
	work := filepath.Join(t.TempDir(), "work")
	if code, out := pairStudyRun(t, repo, study, "run", "--binary", bin, "--work", work); code != 0 {
		t.Fatalf("run: %s", out)
	}
	var result struct {
		Verdict           string `json:"verdict"`
		Scored            bool   `json:"scored"`
		PromotionEligible bool   `json:"promotion_eligible"`
		Experiments       []struct {
			MechanicallyComplete bool `json:"mechanically_complete"`
			Reconciled           bool `json:"reconciled"`
			EconomicClaim        bool `json:"economic_claim"`
		} `json:"experiments"`
	}
	raw, err := os.ReadFile(filepath.Join(study, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &result) != nil || result.Verdict != "inconclusive" || result.Scored || result.PromotionEligible || len(result.Experiments) != 1 || !result.Experiments[0].MechanicallyComplete || !result.Experiments[0].Reconciled || result.Experiments[0].EconomicClaim {
		t.Fatalf("a mechanical fixture must replay and stay an unscored inconclusive diagnostic: %s", raw)
	}
	if _, err := os.Lstat(filepath.Join(study, "plans", "mechanical", "state")); !os.IsNotExist(err) {
		t.Fatal("replay state must stay outside the locked study directory")
	}
}

func TestBasketReplayPairStudyProtocolRejectsAMissingRule(t *testing.T) {
	repo := basketPyRoot(t)
	study := filepath.Join(t.TempDir(), "study")
	if out, err := exec.Command("cp", "-R", filepath.Join(repo, "backtest", "candidates", "cross_exchange_perp_arbitrage_1655"), study).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	proto := map[string]interface{}{}
	blob, err := os.ReadFile(filepath.Join(study, "protocol.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(blob, &proto) != nil {
		t.Fatal(err)
	}
	delete(proto, "sample_rules")
	blob, _ = json.MarshalIndent(proto, "", "  ")
	if err := os.WriteFile(filepath.Join(study, "protocol.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := pairStudyRun(t, repo, study, "validate")
	if code != 3 || !strings.Contains(out, "min_independent_pairs") {
		t.Fatalf("protocol validation must reject a missing sample rule, got %d\n%s", code, out)
	}
}
