//go:build pyintegration

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const momentumStudyRel = "backtest/candidates/cross_sectional_momentum_1787"

type momentumStudy struct {
	t    *testing.T
	root string
	dir  string
	bin  string
}

func readStudyProtocol(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var proto map[string]interface{}
	if err := json.Unmarshal(blob, &proto); err != nil {
		t.Fatal(err)
	}
	return proto
}

func applyStudyFixtureWindows(proto map[string]interface{}, basketID string) {
	data := proto["data"].(map[string]interface{})
	data["timeframe"] = "1h"
	data["funding_start"] = "2025-09-01T00:00:00Z"
	data["pinned_end"] = "2025-09-16T00:00:00Z"
	uni := proto["universe"].(map[string]interface{})
	uni["candidates"] = []interface{}{"BTC", "ETH", "SOL"}
	uni["listing_cutoff"] = "2025-09-01T00:00:00Z"
	pre := proto["prerequisites"].(map[string]interface{})
	pre["commits"] = map[string]interface{}{}
	proto["require_clean_source"] = false
	proto["windows"] = map[string]interface{}{
		"train":    []interface{}{"2025-09-12T00:00:00Z", "2025-09-12T13:00:00Z"},
		"held_out": []interface{}{"2025-09-12T12:00:00Z", "2025-09-13T13:00:00Z"},
	}
	proto["history_bars"] = 264
	proto["basket"] = map[string]interface{}{
		"id": basketID, "rebalance_interval_seconds": 14400, "evaluation_window_seconds": 3600,
		"ranker_timeout_seconds": 60, "lookback_bars": 48, "gross_budget_usd": 3000, "max_coin_notional_usd": 1500,
		"max_transition_net_usd": 900, "min_eligible_members": 2, "min_history_bars": 24,
		"eligibility_source": "recorded fixture frames", "eligibility_version": "fixture-1",
	}
	mc := proto["member_config"].(map[string]interface{})
	mc["capital"] = 2000
}

func newMomentumStudy(t *testing.T, mutate func(map[string]interface{}), frames string) *momentumStudy {
	t.Helper()
	root := basketPyRoot(t)
	base := realTempDir(t)
	s := &momentumStudy{t: t, root: root, dir: filepath.Join(base, "study"), bin: replayBinary(t)}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proto := readStudyProtocol(t, filepath.Join(root, momentumStudyRel, "protocol.json"))
	applyStudyFixtureWindows(proto, "xsmom-study")
	proto["grid"] = map[string]interface{}{
		"return_lookback_bars": []interface{}{12, 24}, "quantile": []interface{}{0.25}, "volatility_lookback_bars": []interface{}{24},
	}
	for _, name := range []string{"equal_weight_hold", "momentum", "vol_momentum"} {
		proto["baselines"].(map[string]interface{})[name].(map[string]interface{})["net_allowance_usd"] = 3000
	}
	if mutate != nil {
		mutate(proto)
	}
	out, _ := json.MarshalIndent(proto, "", "  ")
	if err := os.WriteFile(filepath.Join(s.dir, "protocol.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if frames == "" {
		frames = filepath.Join(root, "scheduler", "testdata", "basket_replay_recorded_frames.json")
	}
	lots := filepath.Join(base, "lots.json")
	if err := os.WriteFile(lots, []byte(`{"BTC": 5, "ETH": 4, "SOL": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, outText := s.cli("prepare", "--recorded-frames", frames, "--recorded-lots", lots); code != 0 {
		t.Fatalf("prepare exit %d: %s", code, outText)
	}
	if code, outText := s.cli("lock"); code != 0 {
		t.Fatalf("lock exit %d: %s", code, outText)
	}
	return s
}

func (s *momentumStudy) cli(args ...string) (int, string) {
	s.t.Helper()
	argv := append([]string{"run", "--no-sync", "python", filepath.Join(momentumStudyRel, "run_study.py"), "--study-dir", s.dir}, args...)
	cmd := exec.Command("uv", argv...)
	cmd.Dir = s.root
	combined, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(combined)
	}
	if err != nil {
		s.t.Fatalf("study driver did not run: %v %s", err, combined)
	}
	return 0, string(combined)
}

func (s *momentumStudy) run(out, work string) (int, map[string]interface{}, string) {
	s.t.Helper()
	code, text := s.cli("run", "--binary", s.bin, "--work-dir", work, "--jobs", "4", "--timeout", "900", "--out", out)
	blob, err := os.ReadFile(out)
	if err != nil {
		s.t.Fatalf("the study wrote no result record (exit %d): %s", code, tail(text, 4000))
	}
	var rec map[string]interface{}
	if err := json.Unmarshal(blob, &rec); err != nil {
		s.t.Fatal(err)
	}
	return code, rec, text
}

func studyRuns(rec map[string]interface{}, key string) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	list, _ := rec[key].([]interface{})
	for _, item := range list {
		m := item.(map[string]interface{})
		out[m["run_id"].(string)] = m
	}
	return out
}

func studyVerdict(rec map[string]interface{}) map[string]interface{} {
	v, _ := rec["study_verdict"].(map[string]interface{})
	return v
}

func readStudyRaw(t *testing.T, dir, runID string) map[string]interface{} {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "raw", runID+".json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]interface{}
	if err := json.Unmarshal(blob, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestBasketReplayMomentumStudyCompleteReplayStaysAnUnscoredDiagnostic(t *testing.T) {
	s := newMomentumStudy(t, nil, "")
	base := filepath.Dir(s.dir)
	code, rec, text := s.run(filepath.Join(base, "first.json"), filepath.Join(base, "work-first"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	v := studyVerdict(rec)
	if v["verdict"] != "inconclusive" || v["scored"] != false || v["valid_economic_result"] != false || v["reason"] != "insufficient_independent_evidence" || v["edge_source"] != "study_inconclusive" {
		t.Fatalf("a complete replay with too few independent clusters must stay an unscored inconclusive diagnostic, got %v", v)
	}
	held := studyRuns(rec, "held_out")
	want := []string{"heldout-baseline-equal-weight-hold", "heldout-baseline-momentum", "heldout-baseline-vol-momentum", "heldout-candidate", "heldout-candidate-stress"}
	got := make([]string, 0, len(held))
	for id := range held {
		got = append(got, id)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("held-out arms %v, want %v", got, want)
	}
	for id, r := range held {
		if r["replay_status"] != "complete" || r["replay_scored"] != true || r["complete"] != true {
			t.Fatalf("%s: the raw replay must be complete and scored while the study stays unscored: %v %v", id, r["replay_status"], r["refusal"])
		}
		if a, _ := r["accounting"].(map[string]interface{}); a == nil || math.Abs(a["residual_usd"].(float64)) > 0.01 {
			t.Fatalf("%s: equity does not reconcile with booked fills, fees, funding and the endpoint book: %v", id, r["accounting"])
		}
	}
	cand := held["heldout-candidate"]
	ca := cand["accounting"].(map[string]interface{})
	if cand["positions_opened"].(float64) == 0 || ca["fill_rows"].(float64) == 0 {
		t.Fatalf("the real ranker targets must book member fills through the coordinator: %v", ca)
	}
	if ca["funding_rows_positive"].(float64) == 0 || ca["funding_rows_negative"].(float64) == 0 {
		t.Fatalf("a long and short basket must book signed funding on both sides from the frozen records: %v", ca)
	}
	if !reflect.DeepEqual(ca["cost_model_versions"], []interface{}{float64(1)}) {
		t.Fatalf("the primary policy equals the frozen paper model and books version 1, got %v", ca["cost_model_versions"])
	}
	sa := held["heldout-candidate-stress"]["accounting"].(map[string]interface{})
	applied := held["heldout-candidate-stress"]["cost_identity"].(map[string]interface{})["applied"].(map[string]interface{})
	if !reflect.DeepEqual(sa["cost_model_versions"], []interface{}{float64(1001)}) || applied["taker_fee_pct"] != 0.0009 || applied["slippage_pct"] != 0.001 || applied["fill_role"] != "taker_all" || applied["spread"] != "inside_slippage" {
		t.Fatalf("the stressed run must book the custom replay cost policy: %v %v", sa["cost_model_versions"], applied)
	}
	if sa["fees_usd"].(float64) <= ca["fees_usd"].(float64) {
		t.Fatalf("doubled fees and slippage must cost more than the primary policy: stress %v primary %v", sa["fees_usd"], ca["fees_usd"])
	}
	targets := rec["baseline_targets"].(map[string]interface{})
	for name, allowed := range map[string][]float64{"equal_weight_hold": {1}, "momentum": {0, 1}, "vol_momentum": {-1, 0, 1}} {
		meta := targets[name].(map[string]interface{})
		if probs, _ := meta["bound_problems"].([]interface{}); len(probs) != 0 {
			t.Fatalf("%s weights break the declared bounds: %v", name, probs)
		}
		for at, row := range meta["positions"].(map[string]interface{}) {
			for coin, p := range row.(map[string]interface{}) {
				ok := false
				for _, a := range allowed {
					ok = ok || p.(float64) == a
				}
				if !ok {
					t.Fatalf("%s at %s gives %s position %v outside its frozen conversion %v", name, at, coin, p, allowed)
				}
			}
		}
	}
	lots := map[string]float64{"BTC": 5, "ETH": 4, "SOL": 2}
	raw := readStudyRaw(t, s.dir, "heldout-candidate")
	trades, _ := raw["adapter_output"].(map[string]interface{})["trades"].([]interface{})
	for _, tr := range trades {
		m := tr.(map[string]interface{})
		if m["trade_type"] == "funding" {
			continue
		}
		units := m["quantity"].(float64) * math.Pow(10, lots[m["symbol"].(string)])
		if math.Abs(units-math.Round(units)) > 1e-6 {
			t.Fatalf("fill %v %v is not on the frozen lot grid", m["symbol"], m["quantity"])
		}
	}
	code, again, text := s.run(filepath.Join(base, "second.json"), filepath.Join(base, "work-second"))
	if code != 0 {
		t.Fatalf("second study exit %d: %s", code, tail(text, 4000))
	}
	for _, key := range []string{"training", "held_out"} {
		a, b := studyRuns(rec, key), studyRuns(again, key)
		for id, r := range a {
			for _, field := range []string{"equity_daily", "clusters", "accounting", "member_pnl_usd", "rebalances"} {
				if !reflect.DeepEqual(r[field], b[id][field]) {
					t.Fatalf("%s %s: the frozen study does not reproduce", id, field)
				}
			}
		}
	}
	if !reflect.DeepEqual(studyVerdict(rec), studyVerdict(again)) || !reflect.DeepEqual(rec["selected_params"], again["selected_params"]) {
		t.Fatal("the frozen study verdict or training selection does not reproduce")
	}

	plansDir := filepath.Join(s.dir, "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string][]byte{}
	for _, e := range entries {
		blob, err := os.ReadFile(filepath.Join(plansDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		saved[e.Name()] = blob
	}
	restorePlans := func() {
		for name, blob := range saved {
			if err := os.WriteFile(filepath.Join(plansDir, name), blob, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	sortedRunIDs := func(key string) []string {
		ids := []string{}
		for id := range studyRuns(again, key) {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}
	trainIDs, heldIDs := sortedRunIDs("training"), sortedRunIDs("held_out")
	if len(trainIDs) < 2 || len(heldIDs) == 0 {
		t.Fatalf("the fixture study needs two training runs and a held-out run, got %v %v", trainIDs, heldIDs)
	}
	manifestPath := filepath.Join(s.dir, "study_manifest.json")
	manifestState := func() string {
		if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
			return "absent"
		}
		return fileSHA256(t, manifestPath)
	}
	manifestBefore := manifestState()
	changePlan := func(id string) {
		p := filepath.Join(plansDir, id+".json")
		plan := readStudyProtocol(t, p)
		plan["history_bars"] = plan["history_bars"].(float64) + 1
		writeStudyJSON(t, p, plan)
	}
	refusedRun := func(label, changed string) (map[string]interface{}, string) {
		work := filepath.Join(base, "work-"+label)
		code, refused, text := s.run(filepath.Join(base, label+".json"), work)
		ref, _ := refused["refusal"].(map[string]interface{})
		detail, _ := ref["detail"].([]interface{})
		if code != 3 || refused["verdict"] != "refused" || refused["scored"] != false || ref["reason"] != "plan_file_mismatch" || len(detail) != 1 || !strings.Contains(fmt.Sprint(detail[0]), "plans/"+changed+".json") {
			t.Fatalf("%s: a changed plan file %s must refuse the study, got exit %d %v %v\n%s", label, changed, code, refused["verdict"], refused["refusal"], tail(text, 2000))
		}
		if _, ok := refused["study_verdict"]; ok {
			t.Fatalf("%s: a plan file refusal must write no success result: %v", label, refused["study_verdict"])
		}
		if got := manifestState(); got != manifestBefore {
			t.Fatalf("%s: a plan file refusal must leave study_manifest.json unchanged: %s -> %s", label, manifestBefore, got)
		}
		return refused, work
	}
	attemptIDs := func(rec map[string]interface{}) []string {
		ids := []string{}
		list, _ := rec["attempts"].([]interface{})
		for _, a := range list {
			ids = append(ids, a.(map[string]interface{})["run_id"].(string))
		}
		sort.Strings(ids)
		return ids
	}
	requireNoReplay := func(label string, rec map[string]interface{}, work string) {
		if ids := attemptIDs(rec); len(ids) != 0 {
			t.Fatalf("%s: a changed training plan must refuse before any replay, got attempts %v", label, ids)
		}
		if left, err := os.ReadDir(filepath.Join(work, "results")); err != nil || len(left) != 0 {
			t.Fatalf("%s: a changed training plan must refuse before any replay writes a result: %v %v", label, left, err)
		}
	}

	changePlan(trainIDs[0])
	refused, work := refusedRun("stale-first-train", trainIDs[0])
	requireNoReplay("stale-first-train", refused, work)
	restorePlans()

	deleted := filepath.Join(plansDir, trainIDs[0]+".json")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	changePlan(trainIDs[len(trainIDs)-1])
	refused, work = refusedRun("stale-later-train", trainIDs[len(trainIDs)-1])
	requireNoReplay("stale-later-train", refused, work)
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the batch check must refuse before it writes any plan, but %s exists again: %v", deleted, err)
	}
	restorePlans()

	changePlan(heldIDs[0])
	refused, _ = refusedRun("stale-heldout", heldIDs[0])
	if got := attemptIDs(refused); !reflect.DeepEqual(got, trainIDs) {
		t.Fatalf("a changed held-out plan must refuse after the training replays and before any held-out replay, got attempts %v want %v", got, trainIDs)
	}
	restorePlans()
}

func TestBasketReplayMomentumStudyRefusesChangedInputsBeforeAnyReplay(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]interface{})
		frames func(t *testing.T) string
		after  func(t *testing.T, s *momentumStudy)
		reason string
	}{
		{name: "protocol-changed-after-lock", reason: "lock_mismatch", after: func(t *testing.T, s *momentumStudy) {
			p := filepath.Join(s.dir, "protocol.json")
			blob, _ := os.ReadFile(p)
			if err := os.WriteFile(p, bytes.Replace(blob, []byte(`"quantile": [`), []byte(`"quantile": [0.3, `), 1), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "frozen-data-changed", reason: "data_hash_mismatch", after: func(t *testing.T, s *momentumStudy) {
			p := filepath.Join(s.dir, "data", "ETH_funding.csv.gz")
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			fmt.Fprintln(gz, "timestamp,rate")
			fmt.Fprintln(gz, "1756684800052,0.0001")
			gz.Close()
			if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "prerequisite-missing", reason: "prerequisite_missing", mutate: func(p map[string]interface{}) {
			pre := p["prerequisites"].(map[string]interface{})
			pre["files"] = append(pre["files"].([]interface{}), "scheduler/basket_replay_removed.go")
		}},
		{name: "funding-hour-missing", reason: "coverage_incomplete", frames: func(t *testing.T) string {
			fx := loadReplayFixture(t)
			cut := replayFixtureBaseMs + 300*3_600_000
			var kept [][]float64
			for _, r := range fx.Funding["SOL"] {
				if int64(r[0])/3_600_000*3_600_000 != cut {
					kept = append(kept, r)
				}
			}
			fx.Funding["SOL"] = kept
			p := filepath.Join(realTempDir(t), "frames.json")
			blob, _ := json.Marshal(fx)
			if err := os.WriteFile(p, blob, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := ""
			if tc.frames != nil {
				frames = tc.frames(t)
			}
			s := newMomentumStudy(t, tc.mutate, frames)
			if tc.after != nil {
				tc.after(t, s)
			}
			base := filepath.Dir(s.dir)
			work := filepath.Join(base, "work")
			code, rec, text := s.run(filepath.Join(base, "out.json"), work)
			ref, _ := rec["refusal"].(map[string]interface{})
			if code != 3 || rec["verdict"] != "refused" || rec["scored"] != false || rec["valid_economic_result"] != false || ref["reason"] != tc.reason {
				t.Fatalf("want an unscored refusal %s, got exit %d %v %v\n%s", tc.reason, code, rec["verdict"], rec["refusal"], tail(text, 2000))
			}
			if _, err := os.Lstat(work); err == nil {
				t.Fatal("a refused study created replay work state")
			}
			if code, _, _ := s.run(filepath.Join(base, "out.json"), work); code != 2 {
				t.Fatalf("an existing study result must never be replaced, exit %d", code)
			}
		})
	}
}

func TestBasketReplayMomentumStudyIncompleteReplayIsNotScoredDespiteExitZero(t *testing.T) {
	s := newMomentumStudy(t, func(p map[string]interface{}) { p["history_bars"] = 100 }, "")
	base := filepath.Dir(s.dir)
	code, rec, text := s.run(filepath.Join(base, "out.json"), filepath.Join(base, "work"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	v := studyVerdict(rec)
	if v["verdict"] != "inconclusive" || v["scored"] != false || v["reason"] != "no_complete_training_variant" {
		t.Fatalf("an incomplete replay must leave the study unscored, got %v", v)
	}
	train := studyRuns(rec, "training")
	if len(train) != 2 {
		t.Fatalf("every grid variant must be recorded, got %d", len(train))
	}
	for id, r := range train {
		if r["adapter_exit_code"] != float64(0) || r["replay_status"] != "incomplete" || r["replay_scored"] != false || r["complete"] != false {
			t.Fatalf("%s: adapter exit 0 with short frames must stay an unscored incomplete replay: %v %v %v", id, r["adapter_exit_code"], r["replay_status"], r["replay_scored"])
		}
	}
	if held, _ := rec["held_out"].([]interface{}); len(held) != 0 {
		t.Fatal("no held-out replay may run without a complete training selection")
	}
}

const reversalStudyRel = "backtest/candidates/cross_sectional_reversal_1793"

type reversalStudy struct {
	t       *testing.T
	root    string
	dir     string
	dataDir string
	bin     string
}

func writeStudyJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	out, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func newReversalStudy(t *testing.T, mutate func(map[string]interface{}), frames string) *reversalStudy {
	t.Helper()
	root := basketPyRoot(t)
	base := realTempDir(t)
	inputs := &momentumStudy{t: t, root: root, dir: filepath.Join(base, "inputs"), bin: replayBinary(t)}
	if err := os.MkdirAll(inputs.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mp := readStudyProtocol(t, filepath.Join(root, momentumStudyRel, "protocol.json"))
	applyStudyFixtureWindows(mp, "xsmom-study")
	writeStudyJSON(t, filepath.Join(inputs.dir, "protocol.json"), mp)
	if frames == "" {
		frames = filepath.Join(root, "scheduler", "testdata", "basket_replay_recorded_frames.json")
	}
	lots := filepath.Join(base, "lots.json")
	if err := os.WriteFile(lots, []byte(`{"BTC": 5, "ETH": 4, "SOL": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, outText := inputs.cli("prepare", "--recorded-frames", frames, "--recorded-lots", lots); code != 0 {
		t.Fatalf("input preparation exit %d: %s", code, outText)
	}
	s := &reversalStudy{t: t, root: root, dir: filepath.Join(base, "study"), dataDir: filepath.Join(inputs.dir, "data"), bin: inputs.bin}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proto := readStudyProtocol(t, filepath.Join(root, reversalStudyRel, "protocol.json"))
	applyStudyFixtureWindows(proto, "xsrev-study")
	data := proto["data"].(map[string]interface{})
	data["reference_dir"] = s.dataDir
	data["reference_manifest_sha256"] = fileSHA256(t, filepath.Join(s.dataDir, "data_manifest.json"))
	proto["grid"] = map[string]interface{}{
		"return_lookback_bars": []interface{}{12, 24}, "quantile": []interface{}{0.25}, "volatility_lookback_bars": []interface{}{24},
		"holding_horizon_seconds": []interface{}{3600, 7200},
	}
	if mutate != nil {
		mutate(proto)
	}
	writeStudyJSON(t, filepath.Join(s.dir, "protocol.json"), proto)
	if code, outText := s.cli("reference"); code != 0 {
		t.Fatalf("reference exit %d: %s", code, outText)
	}
	if code, outText := s.cli("lock"); code != 0 {
		t.Fatalf("lock exit %d: %s", code, outText)
	}
	return s
}

func (s *reversalStudy) cli(args ...string) (int, string) {
	s.t.Helper()
	argv := append([]string{"run", "--no-sync", "python", filepath.Join(reversalStudyRel, "run_study.py"), "--study-dir", s.dir}, args...)
	cmd := exec.Command("uv", argv...)
	cmd.Dir = s.root
	combined, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(combined)
	}
	if err != nil {
		s.t.Fatalf("study driver did not run: %v %s", err, combined)
	}
	return 0, string(combined)
}

func (s *reversalStudy) run(out, work string) (int, map[string]interface{}, string) {
	s.t.Helper()
	code, text := s.cli("run", "--binary", s.bin, "--work-dir", work, "--jobs", "4", "--timeout", "900", "--out", out)
	blob, err := os.ReadFile(out)
	if err != nil {
		s.t.Fatalf("the study wrote no result record (exit %d): %s", code, tail(text, 4000))
	}
	var rec map[string]interface{}
	if err := json.Unmarshal(blob, &rec); err != nil {
		s.t.Fatal(err)
	}
	return code, rec, text
}

func studyRawSteps(raw map[string]interface{}) []interface{} {
	steps, _ := raw["adapter_output"].(map[string]interface{})["steps"].([]interface{})
	return steps
}

func studyFrozenWeights(t *testing.T, raw map[string]interface{}) map[string]map[string]float64 {
	t.Helper()
	out := map[string]map[string]float64{}
	for _, st := range studyRawSteps(raw) {
		rbs, _ := st.(map[string]interface{})["rebalances"].([]interface{})
		for _, r := range rbs {
			rb := r.(map[string]interface{})
			targets, _ := rb["targets"].(map[string]interface{})
			if len(targets) == 0 {
				continue
			}
			at := time.UnixMilli(int64(rb["deadline_ms"].(float64))).UTC().Format(time.RFC3339)
			w := map[string]float64{}
			for _, tg := range targets {
				m := tg.(map[string]interface{})
				w[m["coin"].(string)] = m["weight"].(float64)
			}
			out[at] = w
		}
	}
	return out
}

func studyRebalances(raw map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, st := range studyRawSteps(raw) {
		rbs, _ := st.(map[string]interface{})["rebalances"].([]interface{})
		for _, r := range rbs {
			rb := r.(map[string]interface{})
			out[time.UnixMilli(int64(rb["deadline_ms"].(float64))).UTC().Format(time.RFC3339)] = rb
		}
	}
	return out
}

func studyBaselineWeights(rec map[string]interface{}, name string) map[string]map[string]float64 {
	meta := rec["baseline_targets"].(map[string]interface{})[name].(map[string]interface{})
	out := map[string]map[string]float64{}
	for at, row := range meta["weights"].(map[string]interface{}) {
		w := map[string]float64{}
		for coin, v := range row.(map[string]interface{}) {
			w[coin] = v.(float64)
		}
		out[at] = w
	}
	return out
}

func requireValidStudyRun(t *testing.T, id string, r map[string]interface{}) {
	t.Helper()
	if r["replay_status"] != "complete" || r["replay_scored"] != true || r["complete"] != true || r["valid"] != true {
		t.Fatalf("%s: want a complete, valid replay, got %v scored=%v complete=%v valid=%v problems %v refusal %v", id, r["replay_status"], r["replay_scored"], r["complete"], r["valid"], r["validity_problems"], r["refusal"])
	}
	if a, _ := r["accounting"].(map[string]interface{}); a == nil || math.Abs(a["residual_usd"].(float64)) > 0.01 {
		t.Fatalf("%s: equity does not reconcile with booked fills, fees, funding and the endpoint book: %v", id, r["accounting"])
	}
}

func TestBasketReplayReversalStudyCompleteReplayStaysAnUnscoredDiagnostic(t *testing.T) {
	s := newReversalStudy(t, nil, "")
	base := filepath.Dir(s.dir)
	first := filepath.Join(s.dir, "results.json")
	code, rec, text := s.run(first, filepath.Join(base, "work-first"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	v := studyVerdict(rec)
	if v["verdict"] != "inconclusive" || v["scored"] != false || v["valid_economic_result"] != false || v["reason"] != "insufficient_independent_evidence" || v["edge_source"] != "study_inconclusive" {
		t.Fatalf("a complete replay with too few independent clusters must stay an unscored inconclusive diagnostic, got %v", v)
	}
	train := studyRuns(rec, "training")
	if len(train) != 4 {
		t.Fatalf("every grid variant must be recorded, got %d", len(train))
	}
	horizons := map[float64]bool{}
	bestID, best := "", math.Inf(-1)
	ids := make([]string, 0, len(train))
	for id := range train {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := train[id]
		requireValidStudyRun(t, id, r)
		horizons[r["holding_horizon_seconds"].(float64)] = true
		if r["expiry_actions"].(map[string]interface{})["committed"].(float64) == 0 {
			t.Fatalf("%s: the holding horizon must close positions through committed expiry actions", id)
		}
		var sum float64
		clusters := r["clusters"].([]interface{})
		for _, c := range clusters {
			sum += c.(map[string]interface{})["return_on_gross"].(float64)
		}
		if mean := sum / float64(len(clusters)); mean > best+1e-15 {
			bestID, best = id, mean
		}
	}
	if !horizons[3600] || !horizons[7200] {
		t.Fatalf("every tested holding horizon must run, got %v", horizons)
	}
	sel := rec["selected"].(map[string]interface{})
	if sel["run_id"] != bestID || rec["training_ranking"].([]interface{})[0] != bestID {
		t.Fatalf("selection must use training results only: selected %v, best training mean %s", sel, bestID)
	}
	held := studyRuns(rec, "held_out")
	want := []string{"heldout-baseline-cross-sectional-momentum", "heldout-baseline-mean-reversion-pro", "heldout-candidate", "heldout-candidate-stress"}
	got := make([]string, 0, len(held))
	for id := range held {
		got = append(got, id)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("held-out arms %v, want %v", got, want)
	}
	for id, r := range held {
		requireValidStudyRun(t, id, r)
		if r["holding_horizon_seconds"] != sel["holding_horizon_seconds"] {
			t.Fatalf("%s runs horizon %v, the training selection is %v", id, r["holding_horizon_seconds"], sel["holding_horizon_seconds"])
		}
	}
	cand := held["heldout-candidate"]
	ca := cand["accounting"].(map[string]interface{})
	if cand["positions_opened"].(float64) == 0 || ca["fill_rows"].(float64) == 0 {
		t.Fatalf("the real ranker targets must book member fills through the coordinator: %v", ca)
	}
	if ca["funding_rows_positive"].(float64) == 0 || ca["funding_rows_negative"].(float64) == 0 {
		t.Fatalf("a long and short basket must book signed funding on both sides from the frozen records: %v", ca)
	}
	if !reflect.DeepEqual(ca["cost_model_versions"], []interface{}{float64(1)}) {
		t.Fatalf("the primary policy equals the frozen paper model and books version 1, got %v", ca["cost_model_versions"])
	}
	sa := held["heldout-candidate-stress"]["accounting"].(map[string]interface{})
	if !reflect.DeepEqual(sa["cost_model_versions"], []interface{}{float64(1001)}) || sa["fees_usd"].(float64) <= ca["fees_usd"].(float64) {
		t.Fatalf("the stressed run must book the doubled custom replay cost policy: %v fees %v against %v", sa["cost_model_versions"], sa["fees_usd"], ca["fees_usd"])
	}
	lots := map[string]float64{"BTC": 5, "ETH": 4, "SOL": 2}
	candRaw := readStudyRaw(t, s.dir, "heldout-candidate")
	trades, _ := candRaw["adapter_output"].(map[string]interface{})["trades"].([]interface{})
	for _, tr := range trades {
		m := tr.(map[string]interface{})
		if m["trade_type"] == "funding" {
			continue
		}
		units := m["quantity"].(float64) * math.Pow(10, lots[m["symbol"].(string)])
		if math.Abs(units-math.Round(units)) > 1e-6 {
			t.Fatalf("fill %v %v is not on the frozen lot grid", m["symbol"], m["quantity"])
		}
	}

	momentum := studyBaselineWeights(rec, "cross_sectional_momentum")
	revFrozen := studyFrozenWeights(t, candRaw)
	if len(momentum) == 0 || len(revFrozen) == 0 {
		t.Fatalf("both the candidate and the momentum baseline must freeze targets: %d %d", len(revFrozen), len(momentum))
	}
	for at, w := range revFrozen {
		m, ok := momentum[at]
		if !ok {
			t.Fatalf("the momentum conversion holds %s, where the reversal ranker froze %v", at, w)
		}
		for coin, x := range w {
			if m[coin] != -x {
				t.Fatalf("%s %s: the matched momentum weight %v is not the mirror of the reversal weight %v", at, coin, m[coin], x)
			}
		}
	}
	plan := readStudyProtocol(t, filepath.Join(base, "work-first", "plans", "heldout-baseline-cross-sectional-momentum.json"))
	pb := plan["basket"].(map[string]interface{})
	pb["ranker"], pb["params"] = "cross_sectional_momentum", sel["params"]
	plan["target_input"] = map[string]interface{}{"mode": "ranker"}
	plan["run_id"] = "momentum-ranker-check"
	plan["state_dir"] = filepath.Join(base, "momentum-ranker-state")
	planPath := filepath.Join(base, "momentum-ranker-plan.json")
	writeStudyJSON(t, planPath, plan)
	outPath := filepath.Join(base, "momentum-ranker-out.json")
	cmd := exec.Command("uv", "run", "--no-sync", "python", "backtest/basket_replay.py", "--plan", planPath, "--binary", s.bin, "--out", outPath, "--timeout", "900")
	cmd.Dir = s.root
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ranker-mode momentum replay: %v %s", err, tail(string(combined), 4000))
	}
	ranked := studyFrozenWeights(t, readStudyProtocol(t, outPath))
	if !reflect.DeepEqual(ranked, momentum) {
		t.Fatalf("the frozen momentum conversion must equal the targets the real ranker runner freezes in the adapter\nconversion %v\nranker %v", momentum, ranked)
	}
	for at, w := range studyBaselineWeights(rec, "mean_reversion_pro") {
		for coin, x := range w {
			if x != 0 {
				t.Fatalf("the fixture has no mean_reversion_pro entry signal at a held-out deadline, got %s %s %v", at, coin, x)
			}
		}
	}
	if held["heldout-baseline-mean-reversion-pro"]["positions_opened"].(float64) != 0 {
		t.Fatal("zero entry signals must set zero targets and open no position")
	}

	before := mustReadFile(t, first)
	if code, text := s.cli("render"); code != 0 {
		t.Fatalf("render exit %d: %s", code, text)
	}
	report, err := os.ReadFile(filepath.Join(s.dir, "REPORT.md"))
	if err != nil || !bytes.Contains(report, []byte("Verdict: **inconclusive**")) {
		t.Fatalf("render must write the report from the saved record: %v", err)
	}
	if !bytes.Equal(before, mustReadFile(t, first)) {
		t.Fatal("render changed the result record")
	}

	code, again, text := s.run(filepath.Join(base, "second.json"), filepath.Join(base, "work-second"))
	if code != 0 {
		t.Fatalf("second study exit %d: %s", code, tail(text, 4000))
	}
	for _, key := range []string{"training", "held_out"} {
		a, b := studyRuns(rec, key), studyRuns(again, key)
		for id, r := range a {
			for _, field := range []string{"equity_daily", "clusters", "accounting", "member_pnl_usd", "rebalances", "portable_plan_sha256", "validity_problems"} {
				if !reflect.DeepEqual(r[field], b[id][field]) {
					t.Fatalf("%s %s: the frozen study does not reproduce", id, field)
				}
			}
		}
	}
	if !reflect.DeepEqual(studyVerdict(rec), studyVerdict(again)) || !reflect.DeepEqual(rec["selected"], again["selected"]) || !reflect.DeepEqual(rec["baseline_targets"], again["baseline_targets"]) {
		t.Fatal("the frozen study verdict, training selection or baseline conversion does not reproduce")
	}

	pinned := readStudyProtocol(t, filepath.Join(s.dir, "study_manifest.json"))["plans"].(map[string]interface{})
	for label, out := range map[string]map[string]interface{}{"first": rec, "second": again} {
		ran := 0
		for _, key := range []string{"training", "held_out"} {
			for id, r := range studyRuns(out, key) {
				ran++
				onDisk := fileSHA256(t, filepath.Join(s.dir, "plans", id+".json"))
				if pinned[id] != r["portable_plan_sha256"] || onDisk != r["portable_plan_sha256"] {
					t.Fatalf("%s run %s: the plan file %s and the manifest pin %v must equal the replayed plan %v", label, id, onDisk, pinned[id], r["portable_plan_sha256"])
				}
			}
		}
		if ran != len(pinned) {
			t.Fatalf("%s run: the manifest pins %d plans for %d replays", label, len(pinned), ran)
		}
	}

	var staleID string
	for id := range train {
		if staleID == "" || id < staleID {
			staleID = id
		}
	}
	stalePath := filepath.Join(s.dir, "plans", staleID+".json")
	stale := readStudyProtocol(t, stalePath)
	stale["history_bars"] = stale["history_bars"].(float64) + 1
	writeStudyJSON(t, stalePath, stale)
	work := filepath.Join(base, "work-stale")
	code, refused, text := s.run(filepath.Join(base, "stale.json"), work)
	ref, _ := refused["refusal"].(map[string]interface{})
	if code != 3 || refused["verdict"] != "refused" || refused["scored"] != false || ref["reason"] != "plan_file_mismatch" || !strings.Contains(fmt.Sprint(ref["detail"]), staleID) {
		t.Fatalf("a stale plan file left by an earlier attempt must refuse the study, got exit %d %v %v\n%s", code, refused["verdict"], refused["refusal"], tail(text, 2000))
	}
	if attempts, _ := refused["attempts"].([]interface{}); len(attempts) != 0 {
		t.Fatalf("a stale plan file must refuse before any replay, got attempts %v", attempts)
	}
	if left, err := os.ReadDir(filepath.Join(work, "results")); err != nil || len(left) != 0 {
		t.Fatalf("a stale plan file must refuse before any replay writes a result: %v %v", left, err)
	}
}

func hourlyMeanReversionStudy(net float64, prefix int) func(map[string]interface{}) {
	return func(p map[string]interface{}) {
		b := p["basket"].(map[string]interface{})
		b["rebalance_interval_seconds"], b["evaluation_window_seconds"], b["max_transition_net_usd"] = 3600, 1800, net
		p["windows"] = map[string]interface{}{
			"train":    []interface{}{"2025-09-12T00:00:00Z", "2025-09-12T02:00:00Z"},
			"held_out": []interface{}{"2025-09-12T01:00:00Z", "2025-09-12T06:00:00Z"},
		}
		p["grid"] = map[string]interface{}{
			"return_lookback_bars": []interface{}{12}, "quantile": []interface{}{0.25}, "volatility_lookback_bars": []interface{}{24},
			"holding_horizon_seconds": []interface{}{3600},
		}
		p["baselines"].(map[string]interface{})["mean_reversion_pro"].(map[string]interface{})["input_prefix_bars"] = prefix
	}
}

func TestBasketReplayReversalStudyBaselineConversionsHoldConvertAndIgnoreLaterBars(t *testing.T) {
	s := newReversalStudy(t, hourlyMeanReversionStudy(1500, 266), "")
	base := filepath.Dir(s.dir)
	code, rec, text := s.run(filepath.Join(base, "out.json"), filepath.Join(base, "work"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	held := studyRuns(rec, "held_out")
	for id, r := range held {
		requireValidStudyRun(t, id, r)
	}
	meta := rec["baseline_targets"].(map[string]interface{})["mean_reversion_pro"].(map[string]interface{})
	holds := meta["held_deadlines"].(map[string]interface{})
	if reason, _ := holds["2025-09-12T01:00:00Z"].(string); len(holds) != 1 || !bytes.Contains([]byte(reason), []byte("closed bars at the deadline")) {
		t.Fatalf("a member without the frozen closed prefix must hold the whole set at 01:00 only, got %v", holds)
	}
	mrp := studyBaselineWeights(rec, "mean_reversion_pro")
	if w := mrp["2025-09-12T02:00:00Z"]; w["ETH"] != -1.0/3 || w["BTC"] != 0 || w["SOL"] != 0 {
		t.Fatalf("the 02:00 ETH short entry signal must convert to weight -1/N with zero targets elsewhere, got %v", w)
	}
	for at, w := range mrp {
		if at == "2025-09-12T02:00:00Z" {
			continue
		}
		for coin, x := range w {
			if x != 0 {
				t.Fatalf("%s %s: a zero entry signal must give a zero target, got %v", at, coin, x)
			}
		}
	}
	raw := readStudyRaw(t, s.dir, "heldout-baseline-mean-reversion-pro")
	rbs := studyRebalances(raw)
	if rb := rbs["2025-09-12T01:00:00Z"]; rb == nil || rb["status"] != "held" || !bytes.Contains([]byte(rb["hold_reason"].(string)), []byte("no frozen baseline weights")) {
		t.Fatalf("the adapter must hold the deadline without frozen weights, got %v", rb)
	}
	opened := false
	trades, _ := raw["adapter_output"].(map[string]interface{})["trades"].([]interface{})
	for _, tr := range trades {
		m := tr.(map[string]interface{})
		if m["symbol"] == "ETH" && m["side"] == "sell" && m["is_close"] == false && m["timestamp"] == "2025-09-12T02:00:00Z" {
			opened = true
		}
		if m["trade_type"] != "funding" && m["symbol"] != "ETH" {
			t.Fatalf("only the ETH signal may trade in the baseline, got %v", m)
		}
	}
	if !opened {
		t.Fatalf("the converted ETH short target must open through the coordinator at the 02:00 step: %v", trades)
	}

	tight := newReversalStudy(t, hourlyMeanReversionStudy(900, 266), "")
	tbase := filepath.Dir(tight.dir)
	code, trec, text := tight.run(filepath.Join(tbase, "out.json"), filepath.Join(tbase, "work"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	requireValidStudyRun(t, "tight mean_reversion_pro", studyRuns(trec, "held_out")["heldout-baseline-mean-reversion-pro"])
	if !reflect.DeepEqual(studyBaselineWeights(trec, "mean_reversion_pro"), mrp) {
		t.Fatal("the net bound must not change the frozen conversion")
	}
	traw := readStudyRaw(t, tight.dir, "heldout-baseline-mean-reversion-pro")
	rb := studyRebalances(traw)["2025-09-12T02:00:00Z"]
	if rb == nil || rb["status"] != "held" || !bytes.Contains([]byte(rb["hold_reason"].(string)), []byte("max_transition_net_usd")) {
		t.Fatalf("a $1000 net target above the $900 bound must hold the whole set in Go, got %v", rb)
	}
	ttrades, _ := traw["adapter_output"].(map[string]interface{})["trades"].([]interface{})
	for _, tr := range ttrades {
		if tr.(map[string]interface{})["trade_type"] != "funding" {
			t.Fatalf("a net-bound hold must send no order, got %v", tr)
		}
	}
	tplan := readStudyProtocol(t, filepath.Join(tight.dir, "plans", "heldout-baseline-mean-reversion-pro.json"))
	if tplan["basket"].(map[string]interface{})["max_transition_net_usd"] != float64(900) {
		t.Fatal("the baseline must keep the candidate net bound")
	}

	fx := loadReplayFixture(t)
	cut := int64(1757646000000)
	for key, rows := range fx.Candles {
		if key == "SOL|1h" {
			continue
		}
		for i, r := range rows {
			if int64(r[0]) >= cut {
				for j := 1; j <= 4; j++ {
					rows[i][j] = r[j] * 1.25
				}
			}
		}
	}
	framesPath := filepath.Join(realTempDir(t), "frames.json")
	blob, _ := json.Marshal(fx)
	if err := os.WriteFile(framesPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	later := newReversalStudy(t, hourlyMeanReversionStudy(1500, 266), framesPath)
	lbase := filepath.Dir(later.dir)
	code, lrec, text := later.run(filepath.Join(lbase, "out.json"), filepath.Join(lbase, "work"))
	if code != 0 {
		t.Fatalf("study exit %d: %s", code, tail(text, 4000))
	}
	for _, name := range []string{"cross_sectional_momentum", "mean_reversion_pro"} {
		a, b := studyBaselineWeights(rec, name), studyBaselineWeights(lrec, name)
		for _, at := range []string{"2025-09-12T02:00:00Z", "2025-09-12T03:00:00Z"} {
			if !reflect.DeepEqual(a[at], b[at]) {
				t.Fatalf("%s %s: bars that close after the deadline changed the frozen conversion: %v against %v", name, at, a[at], b[at])
			}
		}
	}
}

func TestBasketReplayReversalStudyRefusesChangedInputsBeforeAnyReplay(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]interface{})
		frames func(t *testing.T) string
		after  func(t *testing.T, s *reversalStudy)
		reason string
	}{
		{name: "protocol-changed-after-lock", reason: "lock_mismatch", after: func(t *testing.T, s *reversalStudy) {
			p := filepath.Join(s.dir, "protocol.json")
			blob, _ := os.ReadFile(p)
			if err := os.WriteFile(p, bytes.Replace(blob, []byte(`"quantile": [`), []byte(`"quantile": [0.3, `), 1), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "signal-spec-changed-after-lock", reason: "lock_mismatch", mutate: func(p map[string]interface{}) {
			spec := p["signal_spec"].(map[string]interface{})
			copyPath := filepath.Join(realTempDir(t), "SIGNAL_SPEC.md")
			blob, err := os.ReadFile(filepath.Join(basketPyRoot(t), spec["path"].(string)))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyPath, blob, 0o644); err != nil {
				t.Fatal(err)
			}
			spec["path"] = copyPath
		}, after: func(t *testing.T, s *reversalStudy) {
			p := readStudyProtocol(t, filepath.Join(s.dir, "protocol.json"))["signal_spec"].(map[string]interface{})["path"].(string)
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(f, "changed")
			f.Close()
		}},
		{name: "data-reference-changed-after-lock", reason: "lock_mismatch", after: func(t *testing.T, s *reversalStudy) {
			p := filepath.Join(s.dir, "data_reference.json")
			blob, _ := os.ReadFile(p)
			if err := os.WriteFile(p, append(blob, ' '), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "referenced-data-changed", reason: "data_hash_mismatch", after: func(t *testing.T, s *reversalStudy) {
			p := filepath.Join(s.dataDir, "ETH_funding.csv.gz")
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			fmt.Fprintln(gz, "timestamp,rate")
			fmt.Fprintln(gz, "1756684800052,0.0001")
			gz.Close()
			if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "prerequisite-missing", reason: "prerequisite_missing", mutate: func(p map[string]interface{}) {
			pre := p["prerequisites"].(map[string]interface{})
			pre["files"] = append(pre["files"].([]interface{}), "scheduler/basket_replay_removed.go")
		}},
		{name: "history-short", reason: "coverage_incomplete", mutate: func(p map[string]interface{}) { p["history_bars"] = 300 }},
		{name: "funding-hour-missing", reason: "coverage_incomplete", frames: func(t *testing.T) string {
			fx := loadReplayFixture(t)
			cut := replayFixtureBaseMs + 300*3_600_000
			var kept [][]float64
			for _, r := range fx.Funding["SOL"] {
				if int64(r[0])/3_600_000*3_600_000 != cut {
					kept = append(kept, r)
				}
			}
			fx.Funding["SOL"] = kept
			p := filepath.Join(realTempDir(t), "frames.json")
			blob, _ := json.Marshal(fx)
			if err := os.WriteFile(p, blob, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := ""
			if tc.frames != nil {
				frames = tc.frames(t)
			}
			s := newReversalStudy(t, tc.mutate, frames)
			if tc.after != nil {
				tc.after(t, s)
			}
			base := filepath.Dir(s.dir)
			work := filepath.Join(base, "work")
			code, rec, text := s.run(filepath.Join(base, "out.json"), work)
			ref, _ := rec["refusal"].(map[string]interface{})
			if code != 3 || rec["verdict"] != "refused" || rec["scored"] != false || rec["valid_economic_result"] != false || ref["reason"] != tc.reason {
				t.Fatalf("want an unscored refusal %s, got exit %d %v %v\n%s", tc.reason, code, rec["verdict"], rec["refusal"], tail(text, 2000))
			}
			if _, err := os.Lstat(work); err == nil {
				t.Fatal("a refused study created replay work state")
			}
			if code, _, _ := s.run(filepath.Join(base, "out.json"), work); code != 2 {
				t.Fatalf("an existing study result must never be replaced, exit %d", code)
			}
		})
	}
}

func TestBasketReplayReversalStudyNeverScoresARunWithASaveFailure(t *testing.T) {
	cases := []struct {
		name, at, status string
		complete         bool
	}{
		{name: "recovered", at: "2025-09-12T01:00:00Z", status: "complete", complete: true},
		{name: "terminal", at: "2025-09-12T12:00:00Z", status: "incomplete", complete: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newReversalStudy(t, func(p map[string]interface{}) {
				p["venue_script"] = map[string]interface{}{tc.at: map[string]interface{}{"save_failure": true}}
			}, "")
			base := filepath.Dir(s.dir)
			code, rec, text := s.run(filepath.Join(base, "out.json"), filepath.Join(base, "work"))
			if code != 0 {
				t.Fatalf("study exit %d: %s", code, tail(text, 4000))
			}
			v := studyVerdict(rec)
			if v["verdict"] != "inconclusive" || v["scored"] != false || v["valid_economic_result"] != false || v["reason"] != "no_valid_training_variant" {
				t.Fatalf("a save failure in every training run must leave the study unscored, got %v", v)
			}
			if held, _ := rec["held_out"].([]interface{}); len(held) != 0 {
				t.Fatal("no held-out replay may run without a valid training selection")
			}
			for id, r := range studyRuns(rec, "training") {
				saves, _ := r["save_errors"].([]interface{})
				if r["replay_status"] != tc.status || r["complete"] != tc.complete || r["valid"] != false || r["adapter_exit_code"] != float64(0) || len(saves) == 0 {
					t.Fatalf("%s: want replay %s complete=%v and an invalid study run with recorded save errors, got %v complete=%v valid=%v exit=%v saves=%v", id, tc.status, tc.complete, r["replay_status"], r["complete"], r["valid"], r["adapter_exit_code"], saves)
				}
				if tc.complete && r["replay_scored"] != true {
					t.Fatalf("%s: the adapter recovered the save, so the replay itself is scored; only the study refuses it", id)
				}
			}
		})
	}
}
