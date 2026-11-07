package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type tflValidatorCalls struct {
	mu      sync.Mutex
	bundles [][]byte
	results []json.RawMessage
}

func (c *tflValidatorCalls) wrap(next tradeFeedbackValidator) tradeFeedbackValidator {
	return func(ctx context.Context, bundle []byte, result json.RawMessage, timeout time.Duration) ([]byte, string, error) {
		c.mu.Lock()
		c.bundles = append(c.bundles, append([]byte(nil), bundle...))
		c.results = append(c.results, append(json.RawMessage(nil), result...))
		c.mu.Unlock()
		return next(ctx, bundle, result, timeout)
	}
}

func (c *tflValidatorCalls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bundles)
}

func newTFLEnv(t *testing.T, store *StateStore, cfgs []StrategyConfig, validate tradeFeedbackValidator) (*tfaEnv, *tflValidatorCalls) {
	t.Helper()
	e := newTFAEnv(t, store, cfgs)
	e.mgr.shutdown()
	e.ss.feedbackAnalysisAdapters = map[string]tradeFeedbackAnalysisAdapter{
		tradeFeedbackProviderLocal: tradeFeedbackProductionAdapters()[tradeFeedbackProviderLocal],
		tradeFeedbackProviderAPI:   e.adapter,
	}
	calls := &tflValidatorCalls{}
	m := newTradeFeedbackAnalysisManager(calls.wrap(validate), tfaToken)
	m.start()
	e.ss.feedbackAnalysis = m
	e.mgr = m
	t.Cleanup(m.shutdown)
	return e, calls
}

func (e *tfaEnv) bundleURL(sid string) string {
	return "/api/strategies/" + url.PathEscape(sid) + "/trade-feedback/bundle"
}

func (e *tfaEnv) pull(sid string) ([]byte, tradeFeedbackBundle) {
	e.t.Helper()
	rec := e.do(http.MethodGet, e.bundleURL(sid), nil, tfaAuth)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Fatalf("pull %s = %d cache=%q: %s", sid, rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	var b tradeFeedbackBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		e.t.Fatalf("decode bundle: %v", err)
	}
	return rec.Body.Bytes(), b
}

func tflBody(model, digest string, ids []int64, result string) string {
	m, _ := json.Marshal(model)
	d, _ := json.Marshal(digest)
	i, _ := json.Marshal(ids)
	return `{"provider":"claude_code","model":` + string(m) + `,"bundle_sha256":` + string(d) + `,"feedback_rowids":` + string(i) + `,"result":` + result + `}`
}

func tflResult(id int64) string {
	b, _ := json.Marshal(id)
	return `{"summary":"entries were late","findings":[{"feedback_rowid":` + string(b) + `,"text":"late"}],"proposals":[{"kind":"param_grid","params":{"roc_period":[10,12]},"rationale":"faster"},{"kind":"note","text":"watch funding"}],"confidence":"medium"}`
}

func TestTradeFeedbackAnalysisLocalProviderSubmissionContract(t *testing.T) {
	other := trConfig()
	other.ID = "hl-btc"
	other.Symbol = "BTC"
	other.Args = []string{"momentum", "BTC", "1h", "--mode=paper"}
	e, calls := newTFLEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig(), other}, tfaStubValidator)
	db := e.store.primary()
	if rec := e.do(http.MethodGet, e.bundleURL(trSID), nil, tfaAuth); rec.Code != http.StatusConflict {
		t.Fatalf("pull with no marks = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	e.seedClosedLong(trSID, "pos-a", "ETH", e.at(2.2), e.at(6.1))
	e.seedClosedLong(trSID, "pos-b", "ETH", e.at(2.2), e.at(6.1))
	e.seedClosedLong("hl-btc", "pos-c", "BTC", e.at(2.2), e.at(6.1))
	e.saveMark(trSID, "pos-a", "a")
	e.saveMark(trSID, "pos-b", "b")
	e.saveMark("hl-btc", "pos-c", "c")

	_, stale := e.pull(trSID)
	e.saveMark(trSID, "pos-a", "a newer note")
	raw, cur := e.pull(trSID)
	ids := cur.Selection.FeedbackRowIDs
	digest := cur.BundleSHA256
	_, foreign := e.pull("hl-btc")
	if len(ids) != 2 || digest == stale.BundleSHA256 {
		t.Fatalf("current selection = %v digest %s, stale digest %s", ids, digest, stale.BundleSHA256)
	}
	result := tflResult(ids[0])

	noAuth := map[string]string{"Content-Type": "application/json"}
	wrong := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer nope"}
	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
		code                 int
	}{
		{"pull_missing_token", http.MethodGet, e.bundleURL(trSID), noAuth, http.StatusUnauthorized},
		{"pull_wrong_token", http.MethodGet, e.bundleURL(trSID), wrong, http.StatusUnauthorized},
		{"pull_method", http.MethodPost, e.bundleURL(trSID), tfaAuth, http.StatusMethodNotAllowed},
		{"pull_foreign_partition", http.MethodGet, e.bundleURL(trSID) + "?partition=live", tfaAuth, http.StatusNotFound},
		{"pull_bad_partition", http.MethodGet, e.bundleURL(trSID) + "?partition=bogus:x:y", tfaAuth, http.StatusBadRequest},
		{"pull_unknown_strategy", http.MethodGet, e.bundleURL("nope"), tfaAuth, http.StatusNotFound},
	} {
		if rec := e.do(c.method, c.target, nil, c.hdr); rec.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}

	valid := tflBody("claude-opus-5-5", digest, ids, result)
	refusals := []struct {
		name string
		body string
		hdr  map[string]string
		code int
	}{
		{"missing_token", valid, noAuth, http.StatusUnauthorized},
		{"wrong_token", valid, wrong, http.StatusUnauthorized},
		{"missing_result", `{"provider":"claude_code","model":"m","bundle_sha256":"` + digest + `","feedback_rowids":[1]}`, nil, http.StatusBadRequest},
		{"null_result", tflBody("m", digest, ids, "null"), nil, http.StatusBadRequest},
		{"missing_model", `{"provider":"claude_code","bundle_sha256":"` + digest + `","feedback_rowids":[1],"result":` + result + `}`, nil, http.StatusBadRequest},
		{"missing_ids", `{"provider":"claude_code","model":"m","bundle_sha256":"` + digest + `","result":` + result + `}`, nil, http.StatusBadRequest},
		{"missing_digest", `{"provider":"claude_code","model":"m","feedback_rowids":[1],"result":` + result + `}`, nil, http.StatusBadRequest},
		{"bad_model_label", tflBody("model\nwith a line break", digest, ids, result), nil, http.StatusBadRequest},
		{"empty_model_label", tflBody("  ", digest, ids, result), nil, http.StatusBadRequest},
		{"bad_digest_format", tflBody("m", "ABC", ids, result), nil, http.StatusBadRequest},
		{"fractional_id", `{"provider":"claude_code","model":"m","bundle_sha256":"` + digest + `","feedback_rowids":[1.5],"result":` + result + `}`, nil, http.StatusBadRequest},
		{"client_validation", valid[:len(valid)-1] + `,"validation":{"outcome":"ok","reasons":[],"proposals":[]}}`, nil, http.StatusBadRequest},
		{"client_status", valid[:len(valid)-1] + `,"status":"ok"}`, nil, http.StatusBadRequest},
		{"api_provider_with_result", `{"provider":"anthropic_api","result":` + result + `}`, nil, http.StatusBadRequest},
		{"api_provider_with_selection", `{"provider":"anthropic_api","bundle_sha256":"` + digest + `","feedback_rowids":[1]}`, nil, http.StatusBadRequest},
		{"duplicate_id", tflBody("m", digest, []int64{ids[0], ids[0]}, result), nil, http.StatusConflict},
		{"missing_id", tflBody("m", digest, ids[:1], result), nil, http.StatusConflict},
		{"extra_id", tflBody("m", digest, append(append([]int64(nil), ids...), 999), result), nil, http.StatusConflict},
		{"reordered_ids", tflBody("m", digest, []int64{ids[1], ids[0]}, result), nil, http.StatusConflict},
		{"empty_ids", tflBody("m", digest, []int64{}, result), nil, http.StatusConflict},
		{"stale_selection", tflBody("m", stale.BundleSHA256, stale.Selection.FeedbackRowIDs, result), nil, http.StatusConflict},
		{"stale_digest_current_ids", tflBody("m", stale.BundleSHA256, ids, result), nil, http.StatusConflict},
		{"cross_strategy_selection", tflBody("m", foreign.BundleSHA256, foreign.Selection.FeedbackRowIDs, result), nil, http.StatusConflict},
		{"cross_strategy_digest", tflBody("m", foreign.BundleSHA256, ids, result), nil, http.StatusConflict},
	}
	for _, c := range refusals {
		if rec := e.submit(trSID, c.body, c.hdr); rec.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}
	if rows := tfaAnalysisRows(t, db); len(rows) != 0 || calls.count() != 0 || e.adapter.callCount() != 0 {
		t.Fatalf("refused submissions wrote %v, ran the validator %d times and the API adapter %d times", rows, calls.count(), e.adapter.callCount())
	}

	okID := tfaRequestID(t, e.submit(trSID, valid, nil))
	done := e.waitTerminal(trSID, okID)
	if done.Status != tradeFeedbackAnalysisOK || done.Provider != tradeFeedbackProviderLocal || done.Model != "claude-opus-5-5" ||
		done.ModelVerification != tradeFeedbackLocalModelVerification || done.BundleSHA256 != digest {
		t.Fatalf("local ok view = %+v", done)
	}
	if rows := tfaAnalysisRows(t, db); rows[tradeFeedbackAnalysisAccepted] != 1 || rows[tradeFeedbackAnalysisOK] != 1 || len(rows) != 2 {
		t.Fatalf("rows = %v, want one accepted and one ok", rows)
	}
	var frozen, acceptedModel, terminalVerification string
	db.db.QueryRow(`SELECT bundle_json, model FROM trade_feedback_analyses WHERE event_status = 'accepted'`).Scan(&frozen, &acceptedModel)
	db.db.QueryRow(`SELECT model_verification FROM trade_feedback_analyses WHERE event_status = 'ok'`).Scan(&terminalVerification)
	if frozen != string(raw) || acceptedModel != "claude-opus-5-5" || terminalVerification != tradeFeedbackLocalModelVerification {
		t.Fatalf("accepted row froze a different bundle or label (model %q, verification %q)", acceptedModel, terminalVerification)
	}
	calls.mu.Lock()
	var gotResult string
	json.Unmarshal(calls.results[0], &gotResult)
	sameBundle := bytes.Equal(calls.bundles[0], raw)
	calls.mu.Unlock()
	if !sameBundle || gotResult != result {
		t.Fatalf("the validator did not receive the matched bundle and the posted result bytes")
	}

	badID := tfaRequestID(t, e.submit(trSID, tflBody("claude-opus-5-5", digest, ids, tflResult(999999)), nil))
	if bad := e.waitTerminal(trSID, badID); bad.Status != tradeFeedbackAnalysisInvalid || len(bad.Validation) == 0 {
		t.Fatalf("invalid local result = %+v", bad)
	}
	if rows := tfaAnalysisRows(t, db); rows[tradeFeedbackAnalysisAccepted] != 2 || rows[tradeFeedbackAnalysisOK] != 1 || rows[tradeFeedbackAnalysisInvalid] != 1 {
		t.Fatalf("rows = %v, want two accepted, one ok and one invalid", rows)
	}
	if calls.count() != 2 || e.adapter.callCount() != 0 {
		t.Fatalf("validator ran %d times and the API adapter %d times, want 2 and 0", calls.count(), e.adapter.callCount())
	}
}

func TestTradeFeedbackAnalysisLocalProviderCrossPartition(t *testing.T) {
	dir := t.TempDir()
	mk := func(id, src string) StrategyConfig {
		return StrategyConfig{ID: id, StorageStrategyID: "eth", PaperSource: src, Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h",
			Args: []string{"momentum", "ETH", "1h", "--mode=paper"}}
	}
	cfg := &Config{
		DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}, {ID: "s2", DBFile: filepath.Join(dir, "s2.db")}},
		Strategies:   []StrategyConfig{mk("s1-eth", "s1"), mk("s2-eth", "s2")},
	}
	store := openSplitStore(t, cfg)
	e, calls := newTFLEnv(t, store, cfg.Strategies, tfaStubValidator)
	for _, id := range []string{"s1-eth", "s2-eth"} {
		e.seedClosedLong(id, "pos-shared", "ETH", e.at(2.2), e.at(6.1))
		e.saveMark(id, "pos-shared", id)
	}
	_, s1 := e.pull("s1-eth")
	_, s2 := e.pull("s2-eth")
	if s1.Strategy.Partition == s2.Strategy.Partition || s1.BundleSHA256 == s2.BundleSHA256 {
		t.Fatalf("partitions %s/%s digests %s/%s", s1.Strategy.Partition, s2.Strategy.Partition, s1.BundleSHA256, s2.BundleSHA256)
	}
	if rec := e.do(http.MethodGet, e.bundleURL("s1-eth")+"?partition=paper:s2", nil, tfaAuth); rec.Code != http.StatusNotFound {
		t.Errorf("cross-partition pull = %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodGet, e.bundleURL("s1-eth")+"?partition=paper:s1", nil, tfaAuth); rec.Code != http.StatusOK {
		t.Errorf("own-partition pull = %d, want 200", rec.Code)
	}
	if rec := e.submit("s2-eth", tflBody("m", s1.BundleSHA256, s1.Selection.FeedbackRowIDs, tflResult(s1.Selection.FeedbackRowIDs[0])), nil); rec.Code != http.StatusConflict {
		t.Errorf("s1 selection posted to s2 = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	for _, role := range []storageRole{storageRolePrimary, storageRolePaper, paperSourceRole("s1"), paperSourceRole("s2")} {
		if rows := tfaAnalysisRows(t, store.file(role)); len(rows) != 0 {
			t.Errorf("%s holds analysis rows %v after a refused cross-partition submission", role, rows)
		}
	}
	if calls.count() != 0 {
		t.Fatalf("a refused cross-partition submission ran the validator")
	}
	id := tfaRequestID(t, e.submit("s2-eth", tflBody("m", s2.BundleSHA256, s2.Selection.FeedbackRowIDs, tflResult(s2.Selection.FeedbackRowIDs[0])), nil))
	if done := e.waitTerminal("s2-eth", id); done.Status != tradeFeedbackAnalysisOK {
		t.Fatalf("own-partition submission = %+v", done)
	}
	if rows := tfaAnalysisRows(t, store.file(paperSourceRole("s1"))); len(rows) != 0 {
		t.Fatalf("the s2 analysis wrote into the s1 file: %v", rows)
	}
	if rows := tfaAnalysisRows(t, store.file(paperSourceRole("s2"))); rows[tradeFeedbackAnalysisAccepted] != 1 || rows[tradeFeedbackAnalysisOK] != 1 {
		t.Fatalf("s2 file rows = %v", rows)
	}
}
