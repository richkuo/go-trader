//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tfaRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	return root
}

func TestTradeFeedbackAnalysisRealValidatorOutcomes(t *testing.T) {
	root := tfaRepoRoot(t)
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, runTradeFeedbackValidator)
	t.Chdir(root)
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	rowID := tfaLatestRecord(t, e.store.primary(), trSID).RowID

	run := func(result string) tradeFeedbackAnalysisView {
		t.Helper()
		e.adapter.mu.Lock()
		e.adapter.result = json.RawMessage(result)
		e.adapter.mu.Unlock()
		return e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	}
	cite := func(id int64) string {
		b, _ := json.Marshal(id)
		return string(b)
	}
	ok := run(`{"summary":"s","findings":[{"feedback_rowid":` + cite(rowID) + `,"text":"t"}],"proposals":[{"kind":"param_grid","params":{"roc_period":[10,12],"threshold":[2.5]},"rationale":"r"},{"kind":"note","text":"n"}],"confidence":"high"}`)
	if ok.Status != tradeFeedbackAnalysisOK {
		t.Fatalf("valid result = %s (%s): %s", ok.Status, ok.Error, ok.Validation)
	}
	cases := []struct{ name, result string }{
		{"schema_unknown_field", `{"summary":"s","findings":[],"proposals":[],"confidence":"high","extra":1}`},
		{"schema_bad_confidence", `{"summary":"s","findings":[],"proposals":[],"confidence":"sure"}`},
		{"bad_row_reference", `{"summary":"s","findings":[{"feedback_rowid":999999,"text":"t"}],"proposals":[],"confidence":"low"}`},
		{"unknown_param", `{"summary":"s","findings":[],"proposals":[{"kind":"param_grid","params":{"not_a_param":[1]},"rationale":"r"}],"confidence":"low"}`},
		{"rejected_single_value", `{"summary":"s","findings":[],"proposals":[{"kind":"param_grid","params":{"roc_period":[-3]},"rationale":"r"}],"confidence":"low"}`},
		{"empty_grid", `{"summary":"s","findings":[],"proposals":[{"kind":"param_grid","params":{},"rationale":"r"}],"confidence":"low"}`},
		{"not_json", `"plain text that is not a JSON object"`},
	}
	for _, c := range cases {
		v := run(c.result)
		var val struct {
			Outcome string   `json:"outcome"`
			Reasons []string `json:"reasons"`
		}
		if err := json.Unmarshal(v.Validation, &val); err != nil {
			t.Fatalf("%s validation = %s: %v", c.name, v.Validation, err)
		}
		if v.Status != tradeFeedbackAnalysisInvalid || val.Outcome != tradeFeedbackAnalysisInvalid || len(val.Reasons) == 0 {
			t.Errorf("%s = %s outcome %s reasons %v, want invalid with reasons", c.name, v.Status, val.Outcome, val.Reasons)
		}
		for _, r := range v.Research {
			if r.Available {
				t.Errorf("%s left a research action enabled", c.name)
			}
		}
	}
	dup := run(`{"summary":"a","summary":"b","findings":[],"proposals":[],"confidence":"low"}`)
	var dupVal struct {
		Outcome string   `json:"outcome"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(dup.Validation, &dupVal); err != nil {
		t.Fatalf("duplicate key = %s (%s), validation %s: %v", dup.Status, dup.Error, dup.Validation, err)
	}
	if dup.Status != tradeFeedbackAnalysisInvalid || dupVal.Outcome != tradeFeedbackAnalysisInvalid || len(dupVal.Reasons) != 1 || !strings.Contains(dupVal.Reasons[0], "duplicate object key 'summary'") {
		t.Fatalf("duplicate key = %s (%s) outcome %s reasons %v, want invalid with a duplicate-key reason", dup.Status, dup.Error, dupVal.Outcome, dupVal.Reasons)
	}
}

func startTradeFeedbackAnalysisForTest(t *testing.T, e *tfaEnv, validate tradeFeedbackValidator) *tradeFeedbackAnalysisManager {
	t.Helper()
	m := newTradeFeedbackAnalysisManager(validate, tfaToken)
	m.start()
	e.ss.feedbackAnalysis = m
	t.Cleanup(m.shutdown)
	return m
}

func TestTradeFeedbackAnalysisRealValidatorFailures(t *testing.T) {
	root := tfaRepoRoot(t)
	t.Chdir(root)
	bundle := []byte(`{"selection":{"feedback_rowids":[1]},"strategy":{"type":"perps","open_strategy":{"name":"momentum"}}}`)
	result := json.RawMessage(`{"summary":"s","findings":[],"proposals":[],"confidence":"low"}`)
	if _, outcome, err := runTradeFeedbackValidator(context.Background(), bundle, result, 60*time.Second); err != nil || outcome != tradeFeedbackAnalysisOK {
		t.Fatalf("baseline = %s, %v", outcome, err)
	}
	if _, _, err := runTradeFeedbackValidator(context.Background(), []byte(`{"selection":{}}`), result, 60*time.Second); err == nil {
		t.Fatalf("a broken server bundle did not fail the process")
	}
	if _, _, err := runTradeFeedbackValidator(context.Background(), bundle, result, time.Millisecond); err == nil {
		t.Fatalf("a 1ms deadline did not fail")
	}
	saved := tradeFeedbackValidatorStdoutCap
	tradeFeedbackValidatorStdoutCap = 8
	_, _, err := runTradeFeedbackValidator(context.Background(), bundle, result, 60*time.Second)
	tradeFeedbackValidatorStdoutCap = saved
	var overflow *pythonOutputOverflowError
	if err == nil || !errors.As(err, &overflow) {
		t.Fatalf("an output overflow = %v, want a pythonOutputOverflowError", err)
	}

	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, runTradeFeedbackValidator)
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: 1, MaxPositions: 50}
	e.adapter.mu.Lock()
	e.adapter.block = make(chan struct{})
	e.adapter.mu.Unlock()
	timedOut := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if timedOut.Status != tradeFeedbackAnalysisFailed || !strings.Contains(timedOut.Error, "deadline") {
		t.Fatalf("timeout = %s %q, want failed", timedOut.Status, timedOut.Error)
	}
	e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: 300, MaxPositions: 50}
	e.adapter.mu.Lock()
	e.adapter.block = nil
	e.adapter.mu.Unlock()
	tradeFeedbackValidatorStdoutCap = 8
	overflowed := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	tradeFeedbackValidatorStdoutCap = saved
	if overflowed.Status != tradeFeedbackAnalysisFailed {
		t.Fatalf("overflow = %s %q, want failed", overflowed.Status, overflowed.Error)
	}
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, func(ctx context.Context, b []byte, r json.RawMessage, to time.Duration) ([]byte, string, error) {
		return runTradeFeedbackValidator(ctx, []byte(`{"broken":true}`), r, to)
	})
	broken := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if broken.Status != tradeFeedbackAnalysisFailed {
		t.Fatalf("subprocess failure = %s %q, want failed", broken.Status, broken.Error)
	}
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, func(ctx context.Context, b []byte, r json.RawMessage, to time.Duration) ([]byte, string, error) {
		return runTradeFeedbackValidator(ctx, []byte(`{"selection":{}}`), r, to)
	})
	noRows := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if noRows.Status != tradeFeedbackAnalysisFailed || !strings.Contains(noRows.Error, "validator:") {
		t.Fatalf("a bundle with no selected rows = %s %q, want failed", noRows.Status, noRows.Error)
	}
}

func TestTradeFeedbackAnalysisRealStrategyMetadata(t *testing.T) {
	root := tfaRepoRoot(t)
	t.Chdir(root)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{"config_version":` + itoaTFA(CurrentConfigVersion) + `,"strategies":[
		{"id":"hl-eth","type":"perps","platform":"hyperliquid","args":["momentum","ETH","1h","--mode=paper"],"open_strategy":{"name":"momentum","params":{}}},
		{"id":"hl-inv","type":"perps","platform":"hyperliquid","invert_signal":true,"args":["momentum","ETH","1h","--mode=paper"],"open_strategy":{"name":"momentum","params":{}}}]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", OpenStrategy: StrategyRef{Name: "momentum"}}
	meta, err := loadTradeFeedbackStrategyMeta(context.Background(), sc, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta.DefaultParams["roc_period"]; !ok || meta.Description == "" || !meta.Research.Supported {
		t.Fatalf("meta = %+v", meta)
	}
	inv := sc
	inv.ID = "hl-inv"
	meta, err = loadTradeFeedbackStrategyMeta(context.Background(), inv, path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Research.Supported || meta.Research.Reason != "unsupported_invert_signal" {
		t.Fatalf("inverted strategy research = %+v, want the tuning harness's unsupported_invert_signal", meta.Research)
	}
}

func itoaTFA(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
