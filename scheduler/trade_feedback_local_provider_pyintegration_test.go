//go:build pyintegration

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTradeFeedbackAnalysisRealLocalProviderValidator(t *testing.T) {
	root := tfaRepoRoot(t)
	e, calls := newTFLEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()}, runTradeFeedbackValidator)
	t.Chdir(root)
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	_, b := e.pull(trSID)
	ids := b.Selection.FeedbackRowIDs
	cite, _ := json.Marshal(ids[0])

	submit := func(result string) tradeFeedbackAnalysisView {
		t.Helper()
		return e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, tflBody("claude-opus-5-5", b.BundleSHA256, ids, result), nil)))
	}
	reasons := func(v tradeFeedbackAnalysisView) (string, []string) {
		t.Helper()
		var val struct {
			Outcome string   `json:"outcome"`
			Reasons []string `json:"reasons"`
		}
		if err := json.Unmarshal(v.Validation, &val); err != nil {
			t.Fatalf("validation = %s: %v", v.Validation, err)
		}
		return val.Outcome, val.Reasons
	}

	ok := submit(`{"summary":"s","findings":[{"feedback_rowid":` + string(cite) + `,"text":"t"}],"proposals":[{"kind":"param_grid","params":{"roc_period":[10,12]},"rationale":"r"}],"confidence":"high"}`)
	if outcome, _ := reasons(ok); ok.Status != tradeFeedbackAnalysisOK || outcome != tradeFeedbackAnalysisOK || ok.ModelVerification != tradeFeedbackLocalModelVerification {
		t.Fatalf("valid local result = %s (%s), outcome %s, verification %s", ok.Status, ok.Error, outcome, ok.ModelVerification)
	}

	for name, result := range map[string]string{
		"unknown_param":     `{"summary":"s","findings":[],"proposals":[{"kind":"param_grid","params":{"not_a_param":[1]},"rationale":"r"}],"confidence":"low"}`,
		"uncited_mark":      `{"summary":"s","findings":[{"feedback_rowid":999999,"text":"t"}],"proposals":[],"confidence":"low"}`,
		"client_validation": `{"summary":"s","findings":[],"proposals":[],"confidence":"low","validation":{"outcome":"ok"}}`,
		"not_an_object":     `"plain text"`,
	} {
		v := submit(result)
		outcome, rs := reasons(v)
		if v.Status != tradeFeedbackAnalysisInvalid || outcome != tradeFeedbackAnalysisInvalid || len(rs) == 0 {
			t.Errorf("%s = %s outcome %s reasons %v, want invalid with reasons", name, v.Status, outcome, rs)
		}
	}

	dup := submit(`{"summary":"a","summary":"b","findings":[],"proposals":[],"confidence":"low"}`)
	if outcome, rs := reasons(dup); dup.Status != tradeFeedbackAnalysisInvalid || outcome != tradeFeedbackAnalysisInvalid || len(rs) != 1 || !strings.Contains(rs[0], "duplicate object key 'summary'") {
		t.Fatalf("duplicate key = %s outcome %s reasons %v, want invalid from the real validator", dup.Status, outcome, rs)
	}
	if rows := tfaAnalysisRows(t, e.store.primary()); rows[tradeFeedbackAnalysisAccepted] != 6 || rows[tradeFeedbackAnalysisOK] != 1 || rows[tradeFeedbackAnalysisInvalid] != 5 {
		t.Fatalf("rows = %v, want six accepted, one ok and five invalid", rows)
	}
	if calls.count() != 6 || e.adapter.callCount() != 0 {
		t.Fatalf("the real validator ran %d times and the API adapter %d times, want 6 and 0", calls.count(), e.adapter.callCount())
	}
}
