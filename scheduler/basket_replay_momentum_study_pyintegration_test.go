//go:build pyintegration

package main

import (
	"bytes"
	"compress/gzip"
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
	"testing"
)

const momentumStudyRel = "backtest/candidates/cross_sectional_momentum_1787"

type momentumStudy struct {
	t    *testing.T
	root string
	dir  string
	bin  string
}

func newMomentumStudy(t *testing.T, mutate func(map[string]interface{}), frames string) *momentumStudy {
	t.Helper()
	root := basketPyRoot(t)
	base := realTempDir(t)
	s := &momentumStudy{t: t, root: root, dir: filepath.Join(base, "study"), bin: replayBinary(t)}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(root, momentumStudyRel, "protocol.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proto map[string]interface{}
	if err := json.Unmarshal(blob, &proto); err != nil {
		t.Fatal(err)
	}
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
		"id": "xsmom-study", "rebalance_interval_seconds": 14400, "evaluation_window_seconds": 3600,
		"ranker_timeout_seconds": 60, "lookback_bars": 48, "gross_budget_usd": 3000, "max_coin_notional_usd": 1500,
		"max_transition_net_usd": 900, "min_eligible_members": 2, "min_history_bars": 24,
		"eligibility_source": "recorded fixture frames", "eligibility_version": "fixture-1",
	}
	mc := proto["member_config"].(map[string]interface{})
	mc["capital"] = 2000
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
