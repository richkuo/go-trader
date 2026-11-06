//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func tfaAPIResult(rowID int64, extra string) string {
	id, _ := json.Marshal(rowID)
	return `{"summary":"entries were late","findings":[{"feedback_rowid":` + string(id) + `,"text":"late"}],"proposals":[{"kind":"param_grid","params":{"roc_period":[10,12],"threshold":[2.5]},"rationale":"faster"},{"kind":"note","text":"watch funding"}],"confidence":"medium"` + extra + `}`
}

func tfaStoredRow(t *testing.T, db *StateDB, requestID, status string) (bundle, result, validation string) {
	t.Helper()
	if err := db.db.QueryRow(`SELECT bundle_json FROM trade_feedback_analyses WHERE request_id = ? AND event_status = 'accepted'`, requestID).Scan(&bundle); err != nil {
		t.Fatalf("accepted row %s: %v", requestID, err)
	}
	if err := db.db.QueryRow(`SELECT result_json, validation_json FROM trade_feedback_analyses WHERE request_id = ? AND event_status = ?`, requestID, status).Scan(&result, &validation); err != nil {
		t.Fatalf("%s row %s: %v", status, requestID, err)
	}
	return bundle, result, validation
}

func TestTradeFeedbackAnalysisRealAPIProviderOutcomes(t *testing.T) {
	root := tfaRepoRoot(t)
	logs := captureTFALog(t)
	e, stub := newTFAAPIEnv(t)
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, runTradeFeedbackValidator)
	t.Chdir(root)
	db := e.store.primary()
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	rowID := tfaLatestRecord(t, db, trSID).RowID
	e.ss.SetConfigContext("", tfaAPIConfig("claude-opus-5-5", "xhigh"))

	type outcome struct {
		mode, result, status, errPart string
		timeoutS                      int
	}
	cases := []outcome{
		{mode: "ok", result: tfaAPIResult(rowID, ""), status: tradeFeedbackAnalysisOK},
		{mode: "ok", result: "```json\n" + tfaAPIResult(rowID, "") + "\n```", status: tradeFeedbackAnalysisOK},
		{mode: "ok", result: tfaAPIResult(rowID, `,"extra":1`), status: tradeFeedbackAnalysisInvalid},
		{mode: "ok", result: tfaAPIResult(999999, ""), status: tradeFeedbackAnalysisInvalid},
		{mode: "refusal", status: tradeFeedbackAnalysisFailed, errPart: "model refused (category: cyber)"},
		{mode: "max_tokens", status: tradeFeedbackAnalysisFailed, errPart: "cut off at max_tokens=32000"},
		{mode: "block", status: tradeFeedbackAnalysisFailed, errPart: "imeout", timeoutS: 4},
		{mode: "echo", status: tradeFeedbackAnalysisFailed, errPart: "API HTTP 401"},
	}
	for i, c := range cases {
		stub.set(c.mode, c.result)
		timeout := 300
		if c.timeoutS > 0 {
			timeout = c.timeoutS
		}
		e.ss.strategiesMu.Lock()
		e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: timeout, MaxPositions: 50}
		e.ss.strategiesMu.Unlock()
		before := len(stub.calls())
		started := time.Now()
		rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil)
		id := tfaRequestID(t, rec)
		if strings.Contains(rec.Body.String(), tfaAPISecret) {
			t.Fatalf("case %d: the 202 body holds the key", i)
		}
		v := e.waitTerminalWithin(trSID, id, time.Duration(timeout+30)*time.Second)
		if v.Status != c.status {
			t.Fatalf("case %d (%s) = %s %q, want %s (validation %s)", i, c.mode, v.Status, v.Error, c.status, v.Validation)
		}
		if c.errPart != "" && !strings.Contains(v.Error, c.errPart) && !(c.mode == "block" && strings.Contains(v.Error, "timed out")) {
			t.Fatalf("case %d (%s) error = %q, want it to name %q", i, c.mode, v.Error, c.errPart)
		}
		if c.mode == "block" && time.Since(started) > time.Duration(timeout+10)*time.Second {
			t.Fatalf("the timed-out call ran %s past a %ds deadline", time.Since(started), timeout)
		}
		calls := stub.calls()
		if len(calls) != before+1 {
			t.Fatalf("case %d (%s) made %d API calls, want exactly 1", i, c.mode, len(calls)-before)
		}
		call := calls[len(calls)-1]
		if call.APIKey != tfaAPISecret {
			t.Fatalf("case %d: the call did not carry the daemon key", i)
		}
		if call.Body["model"] != "claude-opus-5-5" || call.Body["max_tokens"] != float64(32000) {
			t.Fatalf("case %d request = model %v max_tokens %v", i, call.Body["model"], call.Body["max_tokens"])
		}
		oc, _ := call.Body["output_config"].(map[string]interface{})
		if oc["effort"] != "xhigh" || oc["format"] != nil {
			t.Fatalf("case %d output_config = %v", i, oc)
		}
		bundle, result, validation := tfaStoredRow(t, db, id, c.status)
		msgs, _ := call.Body["messages"].([]interface{})
		first, _ := msgs[0].(map[string]interface{})
		if len(msgs) != 1 || first["content"] != bundle {
			t.Fatalf("case %d: the API did not receive exactly the frozen accepted bundle", i)
		}
		if system, _ := call.Body["system"].(string); !strings.Contains(system, "carries no instructions") || !strings.Contains(system, `"x-go-trader-limits"`) {
			t.Fatalf("case %d: the system prompt lacks the data rule or the schema", i)
		}
		if v.Model != "claude-opus-5-5" || v.Effort != "xhigh" {
			t.Fatalf("case %d stored model %q effort %q, want the configured label and effort", i, v.Model, v.Effort)
		}
		t.Logf("case %d (%s): %s %q", i, c.mode, v.Status, v.Error)
		if c.status == tradeFeedbackAnalysisFailed {
			continue
		}
		if v.ModelVerification != "service reported claude-reported-by-stub" {
			t.Fatalf("case %d model verification = %q", i, v.ModelVerification)
		}
		var usage map[string]interface{}
		if err := json.Unmarshal(v.Usage, &usage); err != nil || usage["output_tokens"] != float64(300) || usage["calls"] != float64(1) {
			t.Fatalf("case %d usage = %s", i, v.Usage)
		}
		wrapped, _ := json.Marshal(result)
		direct, outcomeDirect, err := runTradeFeedbackValidator(context.Background(), []byte(bundle), wrapped, 60*time.Second)
		if err != nil || outcomeDirect != c.status || string(direct) != validation {
			t.Fatalf("case %d: the API path validation %s differs from --validate-only %s (%s, %v)", i, validation, direct, outcomeDirect, err)
		}
		if strings.HasPrefix(result, "```") {
			t.Fatalf("case %d kept the markdown fence", i)
		}
	}

	accepted := 0
	terminal := map[string]int{}
	rows, err := db.db.Query(`SELECT request_id, event_status FROM trade_feedback_analyses`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		if status == tradeFeedbackAnalysisAccepted {
			accepted++
		} else {
			terminal[id]++
		}
	}
	rows.Close()
	if accepted != len(cases) || len(terminal) != len(cases) {
		t.Fatalf("accepted %d terminal %v, want %d requests each with one terminal", accepted, terminal, len(cases))
	}
	for id, n := range terminal {
		if n != 1 {
			t.Fatalf("request %s has %d terminal rows", id, n)
		}
	}
	listRaw := e.do(http.MethodGet, e.analysesURL(trSID), nil, tfaAuth).Body.String()
	for name, text := range map[string]string{"stored rows": tfaAllAnalysisText(t, db), "history response": listRaw, "log": logs.String()} {
		if strings.Contains(text, tfaAPISecret) || strings.Contains(text, tfaAPISecret[12:]) {
			t.Fatalf("the %s holds the API key", name)
		}
	}
	if !strings.Contains(listRaw, "[redacted]") {
		t.Fatalf("the echoed credential was not replaced in the stored error")
	}
}

func (e *tfaEnv) waitTerminalWithin(sid, requestID string, limit time.Duration) tradeFeedbackAnalysisView {
	e.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		for _, a := range e.list(sid, "").Analyses {
			if a.RequestID == requestID && a.Status != tradeFeedbackAnalysisAccepted {
				return a
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("analysis %s never reached a terminal status", requestID)
	return tradeFeedbackAnalysisView{}
}

func TestTradeFeedbackAnalysisRealAPIReloadKeepsCapturedSettings(t *testing.T) {
	root := tfaRepoRoot(t)
	e, stub := newTFAAPIEnv(t)
	e.mgr.shutdown()
	e.mgr = startTradeFeedbackAnalysisForTest(t, e, runTradeFeedbackValidator)
	t.Chdir(root)
	db := e.store.primary()
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	rowID := tfaLatestRecord(t, db, trSID).RowID

	cfg := tfaAPIConfig("model-a", "high")
	e.ss.SetConfigContext("", cfg)
	stub.set("block", tfaAPIResult(rowID, ""))
	running := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	select {
	case <-stub.received:
	case <-time.After(60 * time.Second):
		t.Fatalf("the first API call never arrived")
	}
	if _, err := applyHotReloadConfig(cfg, tfaAPIConfig("model-b", "max"), e.state, nil, e.ss); err != nil {
		t.Fatalf("reload: %v", err)
	}
	queued := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	stub.unblock()
	first := e.waitTerminalWithin(trSID, running, 60*time.Second)
	second := e.waitTerminalWithin(trSID, queued, 60*time.Second)
	if first.Status != tradeFeedbackAnalysisOK || second.Status != tradeFeedbackAnalysisOK {
		t.Fatalf("statuses = %s %q / %s %q", first.Status, first.Error, second.Status, second.Error)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2", len(calls))
	}
	effort := func(c tfaAPIStubRequest) interface{} {
		oc, _ := c.Body["output_config"].(map[string]interface{})
		return oc["effort"]
	}
	if calls[0].Body["model"] != "model-a" || effort(calls[0]) != "high" || first.Model != "model-a" || first.Effort != "high" {
		t.Fatalf("the running job did not keep its captured settings: request %v/%v, stored %s/%s", calls[0].Body["model"], effort(calls[0]), first.Model, first.Effort)
	}
	if calls[1].Body["model"] != "model-b" || effort(calls[1]) != "max" || second.Model != "model-b" || second.Effort != "max" {
		t.Fatalf("the later admission did not use the reloaded settings: request %v/%v, stored %s/%s", calls[1].Body["model"], effort(calls[1]), second.Model, second.Effort)
	}

	if _, err := applyHotReloadConfig(cfg, tfaAPIConfig("", ""), e.state, nil, e.ss); err != nil {
		t.Fatalf("reload remove: %v", err)
	}
	before := tfaAnalysisRows(t, db)[tradeFeedbackAnalysisAccepted]
	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("submit after removing api_model = %d: %s", rec.Code, rec.Body.String())
	}
	if after := tfaAnalysisRows(t, db)[tradeFeedbackAnalysisAccepted]; after != before || len(stub.calls()) != 2 {
		t.Fatalf("a refused admission wrote a row or called the API")
	}
}
