package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const tfaToken = "tfa-status-token-for-tests"

var tfaAuth = map[string]string{"Authorization": "Bearer " + tfaToken, "Content-Type": "application/json"}

const tfaOKResult = `{"summary":"entries were late","findings":[{"feedback_rowid":1,"text":"late"}],"proposals":[{"kind":"param_grid","params":{"roc_period":[10,12]},"rationale":"faster"},{"kind":"note","text":"watch funding"}],"confidence":"medium"}`

type tfaAdapter struct {
	mu      sync.Mutex
	calls   int
	result  json.RawMessage
	block   chan struct{}
	started chan string
	runErr  error
	seen    []tradeFeedbackAnalysisSettings
}

func (a *tfaAdapter) Available() (bool, string) { return true, "" }

func (a *tfaAdapter) Prepare(req tradeFeedbackAnalysisSubmit, build *tradeFeedbackBundleBuild) (tradeFeedbackAdmission, *tradeReviewError) {
	return tradeFeedbackAdmission{Model: "test-model", Effort: "test-effort"}, nil
}

func (a *tfaAdapter) Run(ctx context.Context, job *tradeFeedbackAnalysisJob) (tradeFeedbackProviderOutput, error) {
	a.mu.Lock()
	a.calls++
	a.seen = append(a.seen, job.Settings)
	block, started, result, runErr := a.block, a.started, a.result, a.runErr
	a.mu.Unlock()
	if started != nil {
		started <- job.RequestID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return tradeFeedbackProviderOutput{}, ctx.Err()
		}
	}
	return tradeFeedbackProviderOutput{Result: result, Model: "test-model", ModelVerification: "test-label"}, runErr
}

func (a *tfaAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func tfaStubValidator(ctx context.Context, bundle []byte, result json.RawMessage, timeout time.Duration) ([]byte, string, error) {
	var b tradeFeedbackBundle
	if err := json.Unmarshal(bundle, &b); err != nil {
		return nil, "", err
	}
	var r struct {
		Findings []struct {
			FeedbackRowID int64 `json:"feedback_rowid"`
		} `json:"findings"`
	}
	var text string
	if json.Unmarshal(result, &text) == nil {
		result = json.RawMessage(text)
	}
	outcome := tradeFeedbackAnalysisOK
	if json.Unmarshal(result, &r) != nil {
		outcome = tradeFeedbackAnalysisInvalid
	}
	included := map[int64]bool{}
	for _, id := range b.Selection.FeedbackRowIDs {
		included[id] = true
	}
	for _, f := range r.Findings {
		if !included[f.FeedbackRowID] {
			outcome = tradeFeedbackAnalysisInvalid
		}
	}
	status := "valid"
	if outcome != tradeFeedbackAnalysisOK {
		status = "invalid"
	}
	out, _ := json.Marshal(map[string]interface{}{"outcome": outcome, "reasons": []string{},
		"proposals": []map[string]interface{}{{"index": 0, "kind": "param_grid", "status": status, "reasons": []string{}}, {"index": 1, "kind": "note", "status": "valid", "reasons": []string{}}}})
	return out, outcome, nil
}

type tfaEnv struct {
	*trEnv
	adapter *tfaAdapter
	mgr     *tradeFeedbackAnalysisManager
	meta    tradeFeedbackStrategyMeta
	metaMu  sync.Mutex
}

func tfaMeta() tradeFeedbackStrategyMeta {
	return tradeFeedbackStrategyMeta{Description: "momentum on rate of change",
		DefaultParams: map[string]interface{}{"roc_period": 14.0, "threshold": 3.0},
		Research:      tradeFeedbackResearchSupport{Supported: true, Status: "supported"}}
}

func newTFAEnv(t *testing.T, store *StateStore, cfgs []StrategyConfig) *tfaEnv {
	t.Helper()
	e := &tfaEnv{trEnv: newTREnvWith(t, tfaToken, store, cfgs), adapter: &tfaAdapter{result: json.RawMessage(tfaOKResult)}, meta: tfaMeta()}
	e.ss.feedbackAnalysisMeta = func(ctx context.Context, sc StrategyConfig, path string) (tradeFeedbackStrategyMeta, error) {
		e.metaMu.Lock()
		defer e.metaMu.Unlock()
		return cloneTradeFeedbackMeta(e.meta), nil
	}
	e.ss.feedbackAnalysisAdapters = map[string]tradeFeedbackAnalysisAdapter{tradeFeedbackProviderAPI: e.adapter}
	e.mgr = startTradeFeedbackAnalysis(e.ss, store, tfaToken, tfaStubValidator)
	if e.mgr == nil {
		t.Fatalf("manager did not start: %s", e.ss.feedbackAnalysisDown)
	}
	t.Cleanup(e.mgr.shutdown)
	return e
}

func (e *tfaEnv) saveMark(sid, pid, note string) {
	e.t.Helper()
	if rec := e.post(sid, e.validBody(pid, note), tfaAuth); rec.Code != http.StatusOK {
		e.t.Fatalf("save mark %s/%s = %d: %s", sid, pid, rec.Code, rec.Body.String())
	}
}

func (e *tfaEnv) analysesURL(sid string) string {
	return "/api/strategies/" + url.PathEscape(sid) + "/trade-feedback/analyses"
}

func (e *tfaEnv) submit(sid, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	if hdr == nil {
		hdr = tfaAuth
	}
	return e.do(http.MethodPost, e.analysesURL(sid), []byte(body), hdr)
}

func (e *tfaEnv) list(sid, query string) tradeFeedbackAnalysesResponse {
	e.t.Helper()
	rec := e.do(http.MethodGet, e.analysesURL(sid)+query, nil, tfaAuth)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("list %s = %d: %s", sid, rec.Code, rec.Body.String())
	}
	var resp tradeFeedbackAnalysesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("decode list: %v", err)
	}
	return resp
}

func (e *tfaEnv) waitTerminal(sid, requestID string) tradeFeedbackAnalysisView {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range e.list(sid, "").Analyses {
			if a.RequestID == requestID && a.Status != tradeFeedbackAnalysisAccepted {
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("analysis %s never reached a terminal status", requestID)
	return tradeFeedbackAnalysisView{}
}

func tfaRequestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.RequestID == "" {
		t.Fatalf("decode submit: %v (%s)", err, rec.Body.String())
	}
	return out.RequestID
}

func tfaAnalysisRows(t *testing.T, db *StateDB) map[string]int {
	t.Helper()
	rows, err := db.db.Query(`SELECT event_status, COUNT(*) FROM trade_feedback_analyses GROUP BY event_status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func tfaInsertRawMark(t *testing.T, db *StateDB, sid string, rec tradeFeedbackRecord) {
	t.Helper()
	if err := db.InsertTradeFeedbackChecked(sid, &rec, func(*positionEvidence) error { return nil }); err != nil {
		t.Fatalf("insert raw mark: %v", err)
	}
}

func tfaLatestRecord(t *testing.T, db *StateDB, sid string) tradeFeedbackRecord {
	t.Helper()
	recs, _, err := db.ReadLatestTradeFeedback(sid, 1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("latest mark: %v (%d)", err, len(recs))
	}
	return recs[0]
}

func TestTradeFeedbackAnalysisConfigLoadAndRefusals(t *testing.T) {
	sc := noEdgeHLStrategy("hl-eth", "momentum", "ETH", "1h", "--mode=paper")
	load := func(block interface{}, present bool) (*Config, error) {
		extra := map[string]interface{}{}
		if present {
			extra["trade_feedback_analysis"] = block
		}
		return LoadConfigReadOnly(writeNoEdgeConfigRoot(t, CurrentConfigVersion, extra, sc))
	}
	cfg, err := load(nil, false)
	if err != nil {
		t.Fatalf("absent block: %v", err)
	}
	if got := cfg.tradeFeedbackAnalysisSettings(); got != (tradeFeedbackAnalysisSettings{TimeoutS: 300, MaxPositions: 50}) {
		t.Fatalf("absent block settings = %v", got)
	}
	accepted := []struct {
		block map[string]interface{}
		want  tradeFeedbackAnalysisSettings
	}{
		{map[string]interface{}{}, tradeFeedbackAnalysisSettings{300, 50}},
		{map[string]interface{}{"timeout_s": 1, "max_positions": 1}, tradeFeedbackAnalysisSettings{1, 1}},
		{map[string]interface{}{"timeout_s": 900, "max_positions": 200}, tradeFeedbackAnalysisSettings{900, 200}},
		{map[string]interface{}{"max_positions": 7}, tradeFeedbackAnalysisSettings{300, 7}},
	}
	for _, c := range accepted {
		cfg, err := load(c.block, true)
		if err != nil {
			t.Fatalf("%v refused: %v", c.block, err)
		}
		if got := cfg.tradeFeedbackAnalysisSettings(); got != c.want {
			t.Fatalf("%v settings = %v, want %v", c.block, got, c.want)
		}
	}
	refused := []interface{}{
		nil,
		"300",
		[]int{1},
		map[string]interface{}{"timeout_s": 0},
		map[string]interface{}{"timeout_s": 901},
		map[string]interface{}{"max_positions": 0},
		map[string]interface{}{"max_positions": 201},
		map[string]interface{}{"max_positions": 2.5},
		map[string]interface{}{"timeout_s": "60"},
		map[string]interface{}{"timeout_s": nil},
		map[string]interface{}{"timeout_s": true},
		map[string]interface{}{"api_key": "x"},
	}
	for _, block := range refused {
		if _, err := load(block, true); err == nil {
			t.Errorf("block %#v was accepted", block)
		}
	}
	path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{}, sc)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fractional := strings.Replace(string(raw), `"interval_seconds"`, `"trade_feedback_analysis": {"timeout_s": 60.0}, "interval_seconds"`, 1)
	if err := os.WriteFile(path, []byte(fractional), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigReadOnly(path); err == nil {
		t.Errorf("a fractional-notation timeout_s was accepted")
	}
}

func TestTradeFeedbackAnalysisBundleSelectionAndDigest(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	db := e.store.primary()
	for _, pid := range []string{"pos-a", "pos-b", "pos-c", "pos-d"} {
		e.seedClosedLong(trSID, pid, "ETH", e.at(2.2), e.at(6.1))
	}
	e.saveMark(trSID, "pos-a", "a-first")
	e.saveMark(trSID, "pos-b", "b-only")
	e.saveMark(trSID, "pos-a", "a-second")
	e.saveMark(trSID, "pos-c", "c-only")
	e.saveMark(trSID, "pos-d", "d-only <script>alert(1)</script>")

	build, rerr := e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if rerr != nil {
		t.Fatalf("build: %v", rerr)
	}
	sel := build.bundle.Selection
	if len(sel.FeedbackRowIDs) != 4 || sel.FeedbackRowIDs[0] != 5 || sel.FeedbackRowIDs[1] != 4 || sel.FeedbackRowIDs[2] != 3 || sel.FeedbackRowIDs[3] != 2 {
		t.Fatalf("included = %v, want the latest row of each position newest first [5 4 3 2]", sel.FeedbackRowIDs)
	}
	notes := map[string]string{}
	for _, m := range build.bundle.Marks {
		notes[m.PositionID] = m.Note
		if len(m.Fills) != 2 || len(m.Candles.Bars) == 0 || m.HistoricalStrategy.StrategyID != trSID {
			t.Errorf("mark %s lacks frozen evidence: fills=%d bars=%d", m.PositionID, len(m.Fills), len(m.Candles.Bars))
		}
	}
	if notes["pos-a"] != "a-second" || notes["pos-d"] != "d-only <script>alert(1)</script>" {
		t.Fatalf("notes = %v", notes)
	}
	again, _ := e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if !bytes.Equal(again.bundleJSON, build.bundleJSON) {
		t.Fatalf("identical rows and context gave different bundle bytes")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(build.bundleJSON, &probe); err != nil {
		t.Fatal(err)
	}
	delete(probe, "bundle_sha256")
	var unhashed tradeFeedbackBundle
	json.Unmarshal(build.bundleJSON, &unhashed)
	unhashed.BundleSHA256 = ""
	payload, _ := json.Marshal(unhashed)
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != build.digest() {
		t.Fatalf("digest is not sha256 of the bundle without bundle_sha256")
	}

	e.metaMu.Lock()
	e.meta.Description = "changed description"
	e.metaMu.Unlock()
	if b, _ := e.ss.buildTradeFeedbackBundle(context.Background(), trSID); b.digest() == build.digest() {
		t.Fatalf("a changed registry description kept the digest")
	}
	e.metaMu.Lock()
	e.meta = tfaMeta()
	e.metaMu.Unlock()
	changed := trConfig()
	changed.OpenStrategy = StrategyRef{Name: "momentum", Params: map[string]interface{}{"roc_period": 9}}
	e.ss.UpdateStrategies([]StrategyConfig{changed})
	bParams, _ := e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if bParams.digest() == build.digest() || bParams.bundle.Strategy.OpenStrategy.Params["roc_period"] != 9.0 || bParams.bundle.Strategy.OpenStrategy.Params["threshold"] != 3.0 {
		t.Fatalf("changed open params: digest kept or merge wrong: %v", bParams.bundle.Strategy.OpenStrategy.Params)
	}
	e.ss.UpdateStrategies([]StrategyConfig{trConfig()})
	if b, _ := e.ss.buildTradeFeedbackBundle(context.Background(), trSID); b.digest() != build.digest() {
		t.Fatalf("restoring the settings did not restore the digest")
	}

	latest := tfaLatestRecord(t, db, trSID)
	sameEvidenceNewNote := latest
	sameEvidenceNewNote.Note = "a different note"
	tfaInsertRawMark(t, db, trSID, sameEvidenceNewNote)
	noteBuild, _ := e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if noteBuild.digest() == build.digest() || noteBuild.bundle.Marks[0].Note != "a different note" {
		t.Fatalf("a new latest mark with only a changed note kept the digest")
	}

	if m := tradeFeedbackSelectionMismatch(noteBuild, noteBuild.bundle.Selection.FeedbackRowIDs, noteBuild.digest()); m != "" {
		t.Fatalf("the exact current selection was refused: %s", m)
	}
	ids := noteBuild.bundle.Selection.FeedbackRowIDs
	mismatches := map[string][]int64{
		"missing":   ids[1:],
		"extra":     append(append([]int64(nil), ids...), 99),
		"duplicate": append([]int64{ids[0]}, ids[:len(ids)-1]...),
		"stale":     build.bundle.Selection.FeedbackRowIDs,
		"foreign":   append([]int64{777}, ids[1:]...),
	}
	for name, sel := range mismatches {
		if tradeFeedbackSelectionMismatch(noteBuild, sel, noteBuild.digest()) == "" {
			t.Errorf("%s selection was accepted", name)
		}
	}
	if tradeFeedbackSelectionMismatch(noteBuild, ids, build.digest()) == "" {
		t.Errorf("a stale digest was accepted")
	}
}

func TestTradeFeedbackAnalysisCandidateLimitExclusionsAndSecrets(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	db := e.store.primary()
	for _, pid := range []string{"pos-old", "pos-mid", "pos-new"} {
		e.seedClosedLong(trSID, pid, "ETH", e.at(2.2), e.at(6.1))
	}
	e.saveMark(trSID, "pos-old", "old")
	e.saveMark(trSID, "pos-mid", "mid")
	e.saveMark(trSID, "pos-new", "good")
	good := tfaLatestRecord(t, db, trSID)
	tampered := good
	tampered.EvidenceJSON = strings.Replace(good.EvidenceJSON, `"price":110`, `"price":111`, 1)
	tampered.Note = "tampered"
	tfaInsertRawMark(t, db, trSID, tampered)

	limit := 2
	e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: 300, MaxPositions: limit}
	b, rerr := e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	sel := b.bundle.Selection
	if len(sel.CandidateRowIDs) != 2 || sel.CandidateRowIDs[0] != 4 || sel.CandidateRowIDs[1] != 2 {
		t.Fatalf("candidates = %v, want [4 2]: the limit applies before integrity checks", sel.CandidateRowIDs)
	}
	if len(sel.FeedbackRowIDs) != 1 || sel.FeedbackRowIDs[0] != 2 || len(sel.Exclusions) != 1 || sel.Exclusions[0].FeedbackRowID != 4 {
		t.Fatalf("included=%v exclusions=%v, want only row 2 and row 4 excluded with no fallback", sel.FeedbackRowIDs, sel.Exclusions)
	}

	mk := func(mut func(*tradeFeedbackEvidence), pid string) tradeFeedbackRecord {
		var ev tradeFeedbackEvidence
		if err := json.Unmarshal([]byte(good.EvidenceJSON), &ev); err != nil {
			t.Fatal(err)
		}
		mut(&ev)
		raw, _ := json.Marshal(ev)
		sum := sha256.Sum256(raw)
		r := good
		r.PositionID = pid
		r.EvidenceJSON = string(raw)
		r.EvidenceSHA256 = hex.EncodeToString(sum[:])
		return r
	}
	e.seedClosedLong(trSID, "pos-ver", "ETH", e.at(2.2), e.at(6.1))
	e.seedClosedLong(trSID, "pos-ident", "ETH", e.at(2.2), e.at(6.1))
	e.seedClosedLong(trSID, "pos-store", "ETH", e.at(2.2), e.at(6.1))
	tfaInsertRawMark(t, db, trSID, mk(func(ev *tradeFeedbackEvidence) { ev.Version = 2; ev.PositionID = "pos-ver" }, "pos-ver"))
	tfaInsertRawMark(t, db, trSID, mk(func(ev *tradeFeedbackEvidence) {}, "pos-ident"))
	tfaInsertRawMark(t, db, trSID, mk(func(ev *tradeFeedbackEvidence) {
		ev.PositionID = "pos-store"
		ev.Strategy.StorageStrategyID = "someone-else"
	}, "pos-store"))
	e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: 300, MaxPositions: 50}
	b, _ = e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	excluded := map[int64]bool{}
	for _, x := range b.bundle.Selection.Exclusions {
		if x.Reason == "" {
			t.Errorf("exclusion %d has no reason", x.FeedbackRowID)
		}
		excluded[x.FeedbackRowID] = true
	}
	for _, id := range []int64{4, 5, 6, 7} {
		if !excluded[id] {
			t.Errorf("row %d was not excluded: %+v", id, b.bundle.Selection.Exclusions)
		}
	}
	if len(b.bundle.Selection.FeedbackRowIDs) != 2 {
		t.Errorf("included = %v, want rows 2 and 1", b.bundle.Selection.FeedbackRowIDs)
	}

	secret := "sk-ant-structured-secret-value-0001"
	t.Setenv("ANTHROPIC_API_KEY", secret)
	sc := trConfig()
	sc.Args = append(sc.Args, "--api-key="+secret)
	sc.Script = "/opt/" + secret + ".py"
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true, Model: secret}
	e.ss.UpdateStrategies([]StrategyConfig{sc})
	e.ss.statusToken = tfaToken
	b, _ = e.ss.buildTradeFeedbackBundle(context.Background(), trSID)
	if bytes.Contains(b.bundleJSON, []byte(secret)) || bytes.Contains(b.bundleJSON, []byte(tfaToken)) {
		t.Fatalf("a structured secret field entered the bundle")
	}
}

func TestTradeFeedbackAnalysisEndpointGuardsAndRouting(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	db := e.store.primary()
	u := e.analysesURL(trSID)
	big := `{"provider":"anthropic_api","x":"` + strings.Repeat("y", tradeFeedbackAnalysisBodyLimit) + `"}`
	spaces := `{"provider":"anthropic_api"}` + strings.Repeat(" ", tradeFeedbackAnalysisBodyLimit)
	noAuth := map[string]string{"Content-Type": "application/json"}
	wrong := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer nope"}
	cross := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + tfaToken, "Origin": "https://evil.example"}
	plain := map[string]string{"Content-Type": "text/plain", "Authorization": "Bearer " + tfaToken}
	cases := []struct {
		name, method, target, body string
		hdr                        map[string]string
		code                       int
	}{
		{"missing_token", http.MethodPost, u, `{"provider":"anthropic_api"}`, noAuth, 401},
		{"wrong_token", http.MethodGet, u, "", wrong, 401},
		{"cross_origin", http.MethodPost, u, `{"provider":"anthropic_api"}`, cross, 403},
		{"media_type", http.MethodPost, u, `{"provider":"anthropic_api"}`, plain, 415},
		{"unknown_field", http.MethodPost, u, `{"provider":"anthropic_api","extra":1}`, tfaAuth, 400},
		{"trailing_json", http.MethodPost, u, `{"provider":"anthropic_api"}{}`, tfaAuth, 400},
		{"oversize", http.MethodPost, u, big, tfaAuth, 413},
		{"oversize_trailing_whitespace", http.MethodPost, u, spaces, tfaAuth, 413},
		{"unknown_provider", http.MethodPost, u, `{"provider":"openai"}`, tfaAuth, 400},
		{"not_installed_provider", http.MethodPost, u, `{"provider":"claude_code"}`, tfaAuth, 503},
		{"method", http.MethodPut, u, "", tfaAuth, 405},
		{"deeper_path", http.MethodGet, u + "/x", "", tfaAuth, 404},
		{"unlisted_nested", http.MethodGet, "/api/strategies/" + trSID + "/trade-feedback/other", "", tfaAuth, 404},
		{"bundle_not_installed", http.MethodGet, "/api/strategies/" + trSID + "/trade-feedback/bundle", "", tfaAuth, 503},
		{"unknown_strategy", http.MethodPost, "/api/strategies/nope/trade-feedback/analyses", `{"provider":"anthropic_api"}`, tfaAuth, 404},
		{"foreign_partition", http.MethodGet, u + "?partition=live", "", tfaAuth, 404},
		{"bad_partition", http.MethodGet, u + "?partition=bogus:x:y", "", tfaAuth, 400},
	}
	for _, c := range cases {
		rec := e.do(c.method, c.target, []byte(c.body), c.hdr)
		if rec.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}
	if n := tfaAnalysisRows(t, db); len(n) != 0 || e.adapter.callCount() != 0 {
		t.Fatalf("refused requests wrote %v or called the provider %d times", n, e.adapter.callCount())
	}
	if rec := e.post(trSID, e.validBody("pos-a", "save route still works"), tfaAuth); rec.Code != http.StatusOK {
		t.Fatalf("existing save route = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodGet, u+"?partition=paper", nil, tfaAuth); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("own partition list = %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	open := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	open.ss.statusToken = ""
	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, open.analysesURL(trSID), ""},
		{http.MethodPost, open.analysesURL(trSID), `{"provider":"anthropic_api"}`},
		{http.MethodGet, "/api/strategies/" + trSID + "/trade-feedback/bundle", ""},
	} {
		if rec := open.do(c.method, c.target, []byte(c.body), map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with no configured token = %d, want 503", c.method, c.target, rec.Code)
		}
	}
	if rec := open.do(http.MethodGet, "/api/strategies/"+trSID+"/trades", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("existing open read with no token = %d, want the unchanged 200", rec.Code)
	}
}

func TestTradeFeedbackAnalysisLifecycleQueueAndReload(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	db := e.store.primary()
	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("zero marks = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")

	id := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	done := e.waitTerminal(trSID, id)
	if done.Status != tradeFeedbackAnalysisOK || done.Model != "test-model" || done.ModelVerification != "test-label" || len(done.Research) != 1 || done.Research[0].Available {
		t.Fatalf("terminal view = %+v", done)
	}
	if rows := tfaAnalysisRows(t, db); rows[tradeFeedbackAnalysisAccepted] != 1 || rows[tradeFeedbackAnalysisOK] != 1 {
		t.Fatalf("rows = %v, want one accepted and one ok", rows)
	}
	var bundleJSON, terminalBundle string
	db.db.QueryRow(`SELECT bundle_json FROM trade_feedback_analyses WHERE event_status = 'accepted'`).Scan(&bundleJSON)
	db.db.QueryRow(`SELECT bundle_json FROM trade_feedback_analyses WHERE event_status = 'ok'`).Scan(&terminalBundle)
	if !json.Valid([]byte(bundleJSON)) || terminalBundle != "" {
		t.Fatalf("frozen input must be on the accepted row only")
	}

	e.adapter.mu.Lock()
	e.adapter.result = json.RawMessage(`{"summary":"x","findings":[{"feedback_rowid":999,"text":"bad cite"}],"proposals":[],"confidence":"low"}`)
	e.adapter.mu.Unlock()
	bad := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if bad.Status != tradeFeedbackAnalysisInvalid {
		t.Fatalf("bad citation status = %s", bad.Status)
	}
	e.adapter.mu.Lock()
	e.adapter.runErr = fmt.Errorf("upstream said no with key %s", tfaToken)
	e.adapter.mu.Unlock()
	failed := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if failed.Status != tradeFeedbackAnalysisFailed || strings.Contains(failed.Error, tfaToken) {
		t.Fatalf("provider failure = %+v", failed)
	}

	e.adapter.mu.Lock()
	e.adapter.runErr = nil
	e.adapter.result = json.RawMessage(tfaOKResult)
	e.adapter.block = make(chan struct{})
	e.adapter.started = make(chan string, 10)
	e.adapter.mu.Unlock()
	cfg := &Config{Strategies: []StrategyConfig{trConfig()}, TradeFeedbackAnalysis: &TradeFeedbackAnalysisConfig{TimeoutS: intPtrTFA(120)}}
	e.ss.SetConfigContext("", cfg)
	callsBefore := e.adapter.callCount()
	running := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	<-e.adapter.started
	next := &Config{Strategies: []StrategyConfig{trConfig()}, TradeFeedbackAnalysis: &TradeFeedbackAnalysisConfig{TimeoutS: intPtrTFA(240)}}
	if _, err := applyHotReloadConfig(cfg, next, e.state, nil, e.ss); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := e.ss.currentFeedbackAnalysisSettings().TimeoutS; got != 240 {
		t.Fatalf("reloaded timeout = %d, want 240", got)
	}
	var queued []string
	for i := 0; i < tradeFeedbackAnalysisQueueCap; i++ {
		queued = append(queued, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	}
	before := tfaAnalysisRows(t, db)[tradeFeedbackAnalysisAccepted]
	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over capacity = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if after := tfaAnalysisRows(t, db)[tradeFeedbackAnalysisAccepted]; after != before {
		t.Fatalf("a refused admission wrote an accepted row (%d -> %d)", before, after)
	}
	list := e.list(trSID, "")
	progress := map[string]string{}
	for _, a := range list.Analyses {
		progress[a.RequestID] = a.Progress
	}
	if progress[running] != "running" || progress[queued[0]] != "queued" {
		t.Fatalf("progress = %v", progress)
	}
	close(e.adapter.block)
	for i := 0; i < tradeFeedbackAnalysisQueueCap; i++ {
		<-e.adapter.started
	}
	for _, id := range append([]string{running}, queued...) {
		if v := e.waitTerminal(trSID, id); v.Status != tradeFeedbackAnalysisOK {
			t.Fatalf("%s status = %s", id, v.Status)
		}
	}
	e.adapter.mu.Lock()
	seen := append([]tradeFeedbackAnalysisSettings(nil), e.adapter.seen[callsBefore:]...)
	e.adapter.mu.Unlock()
	if seen[0].TimeoutS != 120 || seen[1].TimeoutS != 240 {
		t.Fatalf("captured settings = %v, want the running job at 120 and later admissions at 240", seen)
	}
	removed := &Config{Strategies: []StrategyConfig{trConfig()}}
	if _, err := applyHotReloadConfig(next, removed, e.state, nil, e.ss); err != nil {
		t.Fatalf("reload remove: %v", err)
	}
	if got := e.ss.currentFeedbackAnalysisSettings(); got != (tradeFeedbackAnalysisSettings{300, 50}) {
		t.Fatalf("removed block settings = %v", got)
	}

	if err := db.db.Close(); err != nil {
		t.Fatal(err)
	}
	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code == http.StatusAccepted {
		t.Fatalf("a closed database admitted a request")
	}
	if e.mgr.waiting != 0 {
		t.Fatalf("a failed admission kept a reservation: waiting=%d", e.mgr.waiting)
	}
	resp := e.do(http.MethodGet, e.analysesURL(trSID), nil, tfaAuth)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("history read on a failed store = %d, want an error", resp.Code)
	}
}

func intPtrTFA(v int) *int { return &v }

func TestTradeFeedbackAnalysisFailedAdmissionWriteRollsBackReservation(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	db := e.store.primary()
	if _, err := db.db.Exec(`CREATE TRIGGER tfa_refuse BEFORE INSERT ON trade_feedback_analyses BEGIN SELECT RAISE(ABORT, 'disk full'); END;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < tradeFeedbackAnalysisQueueCap+2; i++ {
		if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d = %d, want 500 and never 429: %s", i, rec.Code, rec.Body.String())
		}
	}
	if e.adapter.callCount() != 0 || e.mgr.waiting != 0 {
		t.Fatalf("a failed admission write called the provider (%d) or kept capacity (%d)", e.adapter.callCount(), e.mgr.waiting)
	}
	db.db.Exec(`DROP TRIGGER tfa_refuse`)
	id := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	if v := e.waitTerminal(trSID, id); v.Status != tradeFeedbackAnalysisOK {
		t.Fatalf("after recovery status = %s", v.Status)
	}
}

func TestTradeFeedbackAnalysisShutdownRecoveryAndImmutability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 2; i++ {
		db, err := OpenStateDB(path)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []string{"trade_feedback_analyses", "idx_trade_feedback_analyses_accepted", "idx_trade_feedback_analyses_terminal",
		"idx_trade_feedback_analyses_strategy", "trade_feedback_analyses_immutable", "trade_feedback_analyses_retained"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, obj).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s count = %d (%v), want 1", obj, n, err)
		}
	}
	store := singleFileStore(db)
	e := newTFAEnv(t, store, []StrategyConfig{trConfig()})
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")

	digest := func() string {
		conn, err := db.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		d, err := captureTableDigest(context.Background(), conn)
		if err != nil {
			t.Fatal(err)
		}
		return d.Sum
	}
	before := digest()
	e.adapter.mu.Lock()
	e.adapter.block = make(chan struct{})
	e.adapter.started = make(chan string, 10)
	e.adapter.mu.Unlock()
	running := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	<-e.adapter.started
	if digest() == before {
		t.Fatalf("the capture digest did not change on an accepted insert")
	}
	queued := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	e.mgr.shutdown()
	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("submit after shutdown = %d, want 503", rec.Code)
	}
	for _, id := range []string{running, queued} {
		req, err := store.ReadTradeFeedbackAnalysis(trSID, id)
		if err != nil || req == nil || req.Terminal == nil || req.Terminal.EventStatus != tradeFeedbackAnalysisFailed {
			t.Fatalf("%s after shutdown = %+v (%v), want a failed outcome written before store close", id, req, err)
		}
	}

	if _, err := db.db.Exec(`UPDATE trade_feedback_analyses SET error = 'x'`); err == nil {
		t.Fatalf("trade_feedback_analyses accepted an update")
	}
	if _, err := db.db.Exec(`DELETE FROM trade_feedback_analyses`); err == nil {
		t.Fatalf("trade_feedback_analyses accepted a delete")
	}
	term := tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisFailed, Error: tradeFeedbackAnalysisShutdownMessage}
	if inserted, err := db.AppendTradeFeedbackAnalysisTerminal(queued, term); err != nil || inserted {
		t.Fatalf("identical repeat = inserted %v err %v, want an idempotent no-op", inserted, err)
	}
	if _, err := db.AppendTradeFeedbackAnalysisTerminal(running, tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisOK}); err == nil {
		t.Fatalf("a conflicting terminal outcome was accepted")
	}
	if _, err := db.AppendTradeFeedbackAnalysisTerminal("tfa-unknown", term); !errors.Is(err, errTradeFeedbackAnalysisUnknownRequest) {
		t.Fatalf("unknown request terminal = %v", err)
	}

	pending := &tradeFeedbackAnalysisEvent{RequestID: "tfa-crash", Partition: "paper", Provider: tradeFeedbackProviderAPI, TimeoutS: 300,
		BundleVersion: 1, BundleSHA256: strings.Repeat("a", 64), BundleJSON: "{}", FeedbackRowIDs: []int64{1}, CreatedAt: time.Now().UTC()}
	if _, err := store.InsertTradeFeedbackAnalysisAccepted(trSID, pending); err != nil {
		t.Fatal(err)
	}
	calls := e.adapter.callCount()
	n, err := store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("recovery = %d, %v; want 1", n, err)
	}
	if n, err := store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("second recovery = %d, %v; want 0", n, err)
	}
	if e.adapter.callCount() != calls {
		t.Fatalf("recovery called the provider")
	}
	req, _ := store.ReadTradeFeedbackAnalysis(trSID, "tfa-crash")
	if req == nil || req.Terminal == nil || req.Terminal.Error != tradeFeedbackInterruptedError {
		t.Fatalf("recovered request = %+v", req)
	}
	if err := SaveStateWithStore(NewAppState(), store); err != nil {
		t.Fatalf("save without the strategy: %v", err)
	}
	if rows := tfaAnalysisRows(t, db); rows[tradeFeedbackAnalysisAccepted] != 3 || rows[tradeFeedbackAnalysisFailed] != 3 {
		t.Fatalf("rows after a removal save = %v", rows)
	}
}

func TestTradeFeedbackAnalysisSplitOwnershipAndRecovery(t *testing.T) {
	dir := t.TempDir()
	mk := func(id, src, mode string) StrategyConfig {
		return StrategyConfig{ID: id, StorageStrategyID: "eth", PaperSource: src, Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h",
			Args: []string{"momentum", "ETH", "1h", "--mode=" + mode}}
	}
	cfg := &Config{
		DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}, {ID: "s2", DBFile: filepath.Join(dir, "s2.db")}},
		Strategies:   []StrategyConfig{mk("live-eth", "", "live"), mk("paper-eth", "", "paper"), mk("s1-eth", "s1", "paper"), mk("s2-eth", "s2", "paper")},
	}
	store := openSplitStore(t, cfg)
	stray := mk("stray-eth", "", "paper")
	stray.StorageStrategyID = ""
	e := newTFAEnv(t, store, append(append([]StrategyConfig(nil), cfg.Strategies...), stray))
	owners := map[string]storageRole{"live-eth": storageRolePrimary, "paper-eth": storageRolePaper, "s1-eth": paperSourceRole("s1"), "s2-eth": paperSourceRole("s2")}
	ids := []string{"live-eth", "paper-eth", "s1-eth", "s2-eth"}
	reqs := map[string]string{}
	for _, id := range ids {
		e.seedClosedLong(id, "pos-shared", "ETH", e.at(2.2), e.at(6.1))
		e.saveMark(id, "pos-shared", id)
		reqs[id] = tfaRequestID(t, e.submit(id, `{"provider":"anthropic_api"}`, nil))
	}
	for _, id := range ids {
		e.waitTerminal(id, reqs[id])
		db := store.file(owners[id])
		var sid, req string
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*), MIN(strategy_id), MIN(request_id) FROM trade_feedback_analyses`).Scan(&n, &sid, &req); err != nil {
			t.Fatal(err)
		}
		if n != 2 || sid != "eth" || req != reqs[id] {
			t.Errorf("%s file holds %d rows of %s/%s, want 2 rows of eth/%s", owners[id], n, sid, req, reqs[id])
		}
		hist := e.list(id, "")
		if len(hist.Analyses) != 1 || hist.Analyses[0].RequestID != reqs[id] || hist.Selection.Included != 1 {
			t.Errorf("%s history = %+v", id, hist)
		}
	}
	if rec := e.do(http.MethodGet, e.analysesURL("s1-eth")+"?partition=paper:s2", nil, tfaAuth); rec.Code != http.StatusNotFound {
		t.Errorf("cross-partition read = %d, want 404", rec.Code)
	}
	if rec := e.submit("stray-eth", `{"provider":"anthropic_api"}`, nil); rec.Code == http.StatusAccepted {
		t.Fatalf("an unmapped owner was admitted")
	}
	if _, err := store.InsertTradeFeedbackAnalysisAccepted("stray-eth", &tradeFeedbackAnalysisEvent{RequestID: "tfa-stray", BundleJSON: "{}", BundleSHA256: "x"}); err == nil {
		t.Fatalf("an unknown owner wrote an accepted row")
	}

	s2 := store.file(paperSourceRole("s2"))
	if err := s2.InsertTradeFeedbackAnalysisAccepted("s2-eth", &tradeFeedbackAnalysisEvent{RequestID: "tfa-s2-crash", Partition: "paper:s2", Provider: tradeFeedbackProviderAPI,
		BundleVersion: 1, BundleSHA256: strings.Repeat("b", 64), BundleJSON: "{}", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s2.InsertTradeFeedbackAnalysisAccepted("s2-eth", &tradeFeedbackAnalysisEvent{RequestID: "tfa-foreign", Partition: "paper:s1", Provider: tradeFeedbackProviderAPI,
		BundleVersion: 1, BundleSHA256: strings.Repeat("c", 64), BundleJSON: "{}", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	n, err := store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC())
	if n != 1 || err == nil {
		t.Fatalf("recovery = %d, %v; want one recovered and the foreign-partition row reported", n, err)
	}
	foreign, _ := s2.ReadTradeFeedbackAnalysis("s2-eth", "tfa-foreign")
	if foreign == nil || foreign.Terminal != nil {
		t.Fatalf("a foreign-partition request was reassigned: %+v", foreign)
	}
	for _, role := range []storageRole{storageRolePrimary, storageRolePaper, paperSourceRole("s1")} {
		var n int
		store.file(role).db.QueryRow(`SELECT COUNT(*) FROM trade_feedback_analyses WHERE request_id IN ('tfa-s2-crash','tfa-foreign')`).Scan(&n)
		if n != 0 {
			t.Errorf("%s file holds another file's request", role)
		}
	}
	if errs := store.SaveAll(NewAppState()); len(errs) > 0 {
		for p, err := range errs {
			if err != nil {
				t.Fatalf("save %s: %v", p, err)
			}
		}
	}
	for _, id := range ids {
		if rows := tfaAnalysisRows(t, store.file(owners[id])); rows[tradeFeedbackAnalysisAccepted] == 0 {
			t.Errorf("%s lost its analysis rows on a full save", owners[id])
		}
	}
}

func TestTradeFeedbackAnalysisResearchLaunchContract(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"strategies":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.ss.tuning = mgr
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")
	id := tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil))
	v := e.waitTerminal(trSID, id)
	if len(v.Research) != 1 || !v.Research[0].Available {
		t.Fatalf("research = %+v", v.Research)
	}
	launch := func(body string, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tuning/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tfaToken)
		if header != "" {
			req.Header.Set(tradeFeedbackResearchHeader, header)
		}
		rec := httptest.NewRecorder()
		e.ss.handleAPITuningRuns(rec, req)
		return rec
	}
	exact := `{"strategy_ids":["` + trSID + `"],"overrides":{"` + trSID + `":{"params":{"roc_period":[10,12]}}}}`
	hdr := id + ":0:" + v.BundleSHA256
	refusals := []struct {
		name, body, header string
		code               int
	}{
		{"changed_grid", `{"strategy_ids":["` + trSID + `"],"overrides":{"` + trSID + `":{"params":{"roc_period":[10,13]}}}}`, hdr, 409},
		{"extra_param", `{"strategy_ids":["` + trSID + `"],"overrides":{"` + trSID + `":{"params":{"roc_period":[10,12],"threshold":[1]}}}}`, hdr, 409},
		{"freeze", `{"strategy_ids":["` + trSID + `"],"overrides":{"` + trSID + `":{"params":{"roc_period":[10,12]},"freeze":["threshold"]}}}`, hdr, 400},
		{"note_proposal", exact, id + ":1:" + v.BundleSHA256, 409},
		{"other_digest", exact, id + ":0:" + strings.Repeat("0", 64), 409},
		{"unknown_request", exact, "tfa-nope:0:" + v.BundleSHA256, 409},
		{"malformed", exact, "garbage", 400},
	}
	for _, c := range refusals {
		if rec := launch(c.body, c.header); rec.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}
	if runs := mgr.list(); len(runs) != 0 {
		t.Fatalf("refused launches started %d runs", len(runs))
	}
	if rec := launch(exact, hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("exact launch = %d: %s", rec.Code, rec.Body.String())
	}
	runs := mgr.list()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	spec := filepath.Join(mgr.rootDir, runs[0].ID, tuningRunSpecFile)
	raw, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	var stored tuningRunSpec
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(stored.Overrides)
	if string(got) != `{"`+trSID+`":{"params":{"roc_period":[10,12]}}}` || len(stored.StrategyIDs) != 1 {
		t.Fatalf("stored tuning spec = %s %v", got, stored.StrategyIDs)
	}
	if rec := launch(`{"strategy_ids":["`+trSID+`"]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("an ordinary tuning launch = %d, want the unchanged 202", rec.Code)
	}

	e.saveMark(trSID, "pos-a", "a newer mark changes the context")
	if rec := launch(exact, hdr); rec.Code != http.StatusConflict {
		t.Fatalf("launch after the marks changed = %d, want 409", rec.Code)
	}
	if v := e.waitTerminal(trSID, id); v.Research[0].Available || v.CurrentContext {
		t.Fatalf("a stale analysis still offers research: %+v", v.Research)
	}
	e.metaMu.Lock()
	e.meta.Research = tradeFeedbackResearchSupport{Status: "unsupported", Reason: "unsupported_invert_signal"}
	e.metaMu.Unlock()
	fresh := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
	if fresh.Research[0].Available {
		t.Fatalf("an unsupported strategy offers research")
	}
	if rec := launch(exact, fresh.RequestID+":0:"+fresh.BundleSHA256); rec.Code != http.StatusConflict {
		t.Fatalf("unsupported launch = %d, want 409", rec.Code)
	}
}

func tfaAllocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestTradeFeedbackAnalysisListAndRecoveryLeaveStoredBundlesUnread(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := singleFileStore(db)
	e := newTFAEnv(t, store, []StrategyConfig{trConfig()})
	e.seedDefault()
	e.saveMark(trSID, "pos-a", "mark")

	const bundleBytes = 4 << 20
	bundle := `{"pad":"` + strings.Repeat("x", bundleBytes) + `"}`
	want := map[string]string{}
	for i := 0; i < tradeFeedbackAnalysisHistoryLimit; i++ {
		id := fmt.Sprintf("tfa-big-%02d", i)
		digest := fmt.Sprintf("%064x", i+1)
		ev := &tradeFeedbackAnalysisEvent{RequestID: id, Partition: "paper", Provider: tradeFeedbackProviderAPI, TimeoutS: 300,
			BundleVersion: 1, BundleSHA256: digest, BundleJSON: bundle, FeedbackRowIDs: []int64{1}, CreatedAt: time.Now().UTC()}
		if _, err := store.InsertTradeFeedbackAnalysisAccepted(trSID, ev); err != nil {
			t.Fatal(err)
		}
		want[id] = tradeFeedbackAnalysisAccepted
		if i%2 == 0 {
			if _, err := db.AppendTradeFeedbackAnalysisTerminal(id, tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisInvalid,
				Error: "bad content", ValidationJSON: `{"outcome":"invalid","reasons":["bad content"],"proposals":[]}`}); err != nil {
				t.Fatal(err)
			}
			want[id] = tradeFeedbackAnalysisInvalid
		}
	}
	var lastColumn string
	if err := db.db.QueryRow(`SELECT name FROM pragma_table_info('trade_feedback_analyses') ORDER BY cid DESC LIMIT 1`).Scan(&lastColumn); err != nil || lastColumn != "bundle_json" {
		t.Fatalf("last column = %q (%v), want bundle_json after every column a read selects", lastColumn, err)
	}
	var stored int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM trade_feedback_analyses WHERE event_status = 'accepted' AND length(bundle_json) = ?`, len(bundle)).Scan(&stored); err != nil || stored != tradeFeedbackAnalysisHistoryLimit {
		t.Fatalf("accepted rows keeping the full bundle = %d (%v)", stored, err)
	}

	var resp tradeFeedbackAnalysesResponse
	listed := tfaAllocated(func() { resp = e.list(trSID, "") })
	if listed >= bundleBytes {
		t.Fatalf("one list request allocated %d bytes, at least one stored bundle (%d bytes)", listed, bundleBytes)
	}
	if len(resp.Analyses) != tradeFeedbackAnalysisHistoryLimit {
		t.Fatalf("listed %d analyses, want %d", len(resp.Analyses), tradeFeedbackAnalysisHistoryLimit)
	}
	for i, v := range resp.Analyses {
		wantID := fmt.Sprintf("tfa-big-%02d", tradeFeedbackAnalysisHistoryLimit-1-i)
		n := tradeFeedbackAnalysisHistoryLimit - i
		if v.RequestID != wantID || v.Status != want[wantID] || v.BundleSHA256 != fmt.Sprintf("%064x", n) || v.CurrentContext {
			t.Fatalf("view %d = %s %s %s current %v, want %s %s", i, v.RequestID, v.Status, v.BundleSHA256, v.CurrentContext, wantID, want[wantID])
		}
		if v.Status == tradeFeedbackAnalysisAccepted && v.Progress != "unresolved" {
			t.Fatalf("view %s progress = %q, want unresolved", v.RequestID, v.Progress)
		}
		if v.Status == tradeFeedbackAnalysisInvalid && v.Error != "bad content" {
			t.Fatalf("view %s error = %q", v.RequestID, v.Error)
		}
	}
	reads, err := store.ReadTradeFeedbackAnalyses(trSID, tradeFeedbackAnalysisHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reads {
		if r.Accepted.BundleJSON != "" || (r.Terminal != nil && r.Terminal.BundleJSON != "") {
			t.Fatalf("a history read carried the stored bundle for %s", r.Accepted.RequestID)
		}
	}

	var recovered int
	recoveredBytes := tfaAllocated(func() {
		recovered, err = store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC())
	})
	if err != nil || recovered != tradeFeedbackAnalysisHistoryLimit/2 {
		t.Fatalf("recovery = %d, %v; want %d", recovered, err, tradeFeedbackAnalysisHistoryLimit/2)
	}
	t.Logf("list allocated %d bytes, recovery allocated %d bytes, stored bundle %d bytes each", listed, recoveredBytes, bundleBytes)
	if recoveredBytes >= bundleBytes {
		t.Fatalf("recovery allocated %d bytes, at least one stored bundle (%d bytes)", recoveredBytes, bundleBytes)
	}
	for _, v := range e.list(trSID, "").Analyses {
		switch want[v.RequestID] {
		case tradeFeedbackAnalysisAccepted:
			if v.Status != tradeFeedbackAnalysisFailed || v.Error != tradeFeedbackInterruptedError {
				t.Fatalf("recovered %s = %s %q, want failed with the interrupted reason", v.RequestID, v.Status, v.Error)
			}
		default:
			if v.Status != want[v.RequestID] {
				t.Fatalf("%s = %s after recovery, want %s", v.RequestID, v.Status, want[v.RequestID])
			}
		}
	}
	if n, err := store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("second recovery = %d, %v; want 0", n, err)
	}
}

func TestTradeFeedbackAnalysisResearchLaunchRereadsSettingsUnderLock(t *testing.T) {
	exact := `{"strategy_ids":["` + trSID + `"],"overrides":{"` + trSID + `":{"params":{"roc_period":[10,12]}}}}`
	setup := func(t *testing.T) (*tfaEnv, *tuningRunManager, string) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath, []byte(`{"strategies":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr, err := newTuningRunManager(configPath, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
		e.ss.tuning = mgr
		e.seedDefault()
		e.saveMark(trSID, "pos-a", "mark")
		v := e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))
		if len(v.Research) != 1 || !v.Research[0].Available {
			t.Fatalf("research = %+v", v.Research)
		}
		return e, mgr, v.RequestID + ":0:" + v.BundleSHA256
	}
	launch := func(e *tfaEnv, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tuning/runs", strings.NewReader(exact))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tfaToken)
		req.Header.Set(tradeFeedbackResearchHeader, header)
		rec := httptest.NewRecorder()
		e.ss.handleAPITuningRuns(rec, req)
		return rec
	}
	publishDuringMetadata := func(e *tfaEnv, publish func([]StrategyConfig) []StrategyConfig) *int {
		calls := 0
		e.ss.feedbackAnalysisMeta = func(ctx context.Context, sc StrategyConfig, path string) (tradeFeedbackStrategyMeta, error) {
			calls++
			if calls == 1 {
				e.ss.strategiesMu.Lock()
				e.ss.strategies = publish(append([]StrategyConfig(nil), e.ss.strategies...))
				e.ss.strategiesMu.Unlock()
			}
			return tfaMeta(), nil
		}
		return &calls
	}

	t.Run("params_changed_during_metadata", func(t *testing.T) {
		e, mgr, hdr := setup(t)
		calls := publishDuringMetadata(e, func(list []StrategyConfig) []StrategyConfig {
			for i := range list {
				if list[i].ID == trSID {
					list[i].OpenStrategy.Params = map[string]interface{}{"roc_period": 20.0}
				}
			}
			return list
		})
		rec := launch(e, hdr)
		if *calls != 1 || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no tuning run was started") {
			t.Fatalf("launch after a params change during the metadata call = %d (metadata calls %d): %s", rec.Code, *calls, rec.Body.String())
		}
		if runs := mgr.list(); len(runs) != 0 {
			t.Fatalf("a refused launch started %d runs", len(runs))
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		e, mgr, hdr := setup(t)
		calls := publishDuringMetadata(e, func(list []StrategyConfig) []StrategyConfig { return list })
		if rec := launch(e, hdr); *calls != 1 || rec.Code != http.StatusAccepted {
			t.Fatalf("unchanged launch = %d (metadata calls %d): %s", rec.Code, *calls, rec.Body.String())
		}
		runs := mgr.list()
		if len(runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(runs))
		}
		raw, err := os.ReadFile(filepath.Join(mgr.rootDir, runs[0].ID, tuningRunSpecFile))
		if err != nil {
			t.Fatal(err)
		}
		var stored tuningRunSpec
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(stored.Overrides)
		if string(got) != `{"`+trSID+`":{"params":{"roc_period":[10,12]}}}` || len(stored.StrategyIDs) != 1 || stored.StrategyIDs[0] != trSID {
			t.Fatalf("stored tuning spec = %s %v, want exactly the proposal grid", got, stored.StrategyIDs)
		}
	})

	t.Run("removed_during_metadata", func(t *testing.T) {
		e, mgr, hdr := setup(t)
		calls := publishDuringMetadata(e, func(list []StrategyConfig) []StrategyConfig {
			out := list[:0]
			for _, sc := range list {
				if sc.ID != trSID {
					out = append(out, sc)
				}
			}
			return out
		})
		rec := launch(e, hdr)
		if *calls != 1 || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "left the running roster") {
			t.Fatalf("launch after a removal during the metadata call = %d (metadata calls %d): %s", rec.Code, *calls, rec.Body.String())
		}
		if runs := mgr.list(); len(runs) != 0 {
			t.Fatalf("a refused launch started %d runs", len(runs))
		}
	})
}

func TestTradeFeedbackAnalysisHistoryPollSkipsBundleBuild(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := singleFileStore(db)
	e := newTFAEnv(t, store, []StrategyConfig{trConfig()})
	var metaCalls int
	e.ss.feedbackAnalysisMeta = func(ctx context.Context, sc StrategyConfig, path string) (tradeFeedbackStrategyMeta, error) {
		e.metaMu.Lock()
		defer e.metaMu.Unlock()
		metaCalls++
		return cloneTradeFeedbackMeta(e.meta), nil
	}
	metaCount := func() int {
		e.metaMu.Lock()
		defer e.metaMu.Unlock()
		return metaCalls
	}
	const marks = 50
	saveLong := func(i int, note string) {
		pid := fmt.Sprintf("pos-%02d", i)
		open := float64(-1500 * (i + 1))
		e.seedClosedLong(trSID, pid, "ETH", e.at(open+0.2), e.at(open+990.1))
		if rec := e.post(trSID, trBody(pid, e.at(open+1).Unix(), e.at(open+500).Unix(), 95, 115, note), tfaAuth); rec.Code != http.StatusOK {
			t.Fatalf("save mark %s = %d: %s", pid, rec.Code, rec.Body.String())
		}
	}
	for i := 0; i < marks; i++ {
		saveLong(i, "mark")
	}
	var minBars, evidenceBytes int64
	if err := db.db.QueryRow(`SELECT MIN(json_array_length(json_extract(evidence_json, '$.candles.bars'))), SUM(length(evidence_json)) FROM trade_feedback`).Scan(&minBars, &evidenceBytes); err != nil {
		t.Fatal(err)
	}
	if minBars < 1480 || minBars > diagMaxFetchBars {
		t.Fatalf("smallest saved window holds %d bars, want about %d", minBars, diagMaxFetchBars)
	}
	e.waitTerminal(trSID, tfaRequestID(t, e.submit(trSID, `{"provider":"anthropic_api"}`, nil)))

	getRaw := func(query string) *httptest.ResponseRecorder {
		return e.do(http.MethodGet, e.analysesURL(trSID)+query, nil, tfaAuth)
	}
	beforeFull := metaCount()
	var full tradeFeedbackAnalysesResponse
	fullBytes := tfaAllocated(func() { full = e.list(trSID, "") })
	if full.View != tradeFeedbackAnalysesViewFull || full.Selection.Included != marks || full.InputSignature == "" || len(full.Analyses) != 1 || !full.Analyses[0].CurrentContext {
		t.Fatalf("full view = view %q included %d signature %q analyses %+v", full.View, full.Selection.Included, full.InputSignature, full.Analyses)
	}
	if metaCount() != beforeFull+1 || fullBytes < uint64(evidenceBytes) {
		t.Fatalf("full view: metadata calls %d, allocated %d bytes for %d evidence bytes; the measurement does not see the bundle build", metaCount()-beforeFull, fullBytes, evidenceBytes)
	}

	const ticks = 10
	beforeHistory := metaCount()
	var bodies []*httptest.ResponseRecorder
	historyBytes := tfaAllocated(func() {
		for i := 0; i < ticks; i++ {
			bodies = append(bodies, getRaw("?view=history"))
		}
	})
	t.Logf("full view allocated %d bytes; %d history views allocated %d bytes; %d marks hold %d evidence bytes", fullBytes, ticks, historyBytes, marks, evidenceBytes)
	if metaCount() != beforeHistory {
		t.Fatalf("history views loaded metadata %d times", metaCount()-beforeHistory)
	}
	if perView, perMark := historyBytes/ticks, uint64(evidenceBytes)/marks; perView >= perMark {
		t.Fatalf("one history view allocated %d bytes, at least the evidence of one mark (%d bytes)", perView, perMark)
	}
	for i, rec := range bodies {
		if rec.Code != http.StatusOK {
			t.Fatalf("history view %d = %d: %s", i, rec.Code, rec.Body.String())
		}
		var top map[string]json.RawMessage
		var hist tradeFeedbackAnalysesHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
			t.Fatal(err)
		}
		if _, ok := top["selection"]; ok {
			t.Fatalf("history view %d carries a selection", i)
		}
		if _, ok := top["research"]; ok {
			t.Fatalf("history view %d carries research support", i)
		}
		var analyses []map[string]json.RawMessage
		if err := json.Unmarshal(top["analyses"], &analyses); err != nil || len(analyses) != 1 {
			t.Fatalf("history view %d analyses = %s (%v)", i, top["analyses"], err)
		}
		for _, key := range []string{"research", "current_context"} {
			if _, ok := analyses[0][key]; ok {
				t.Fatalf("history view %d analysis carries %s", i, key)
			}
		}
		if hist.View != tradeFeedbackAnalysesViewHistory || hist.InputSignature != full.InputSignature ||
			hist.Analyses[0].RequestID != full.Analyses[0].RequestID || hist.Analyses[0].Status != full.Analyses[0].Status || len(hist.Analyses[0].Result) == 0 {
			t.Fatalf("history view %d = view %q signature %q analysis %+v; want signature %q", i, hist.View, hist.InputSignature, hist.Analyses[0], full.InputSignature)
		}
	}

	historySignature := func() string {
		rec := getRaw("?view=history")
		var hist tradeFeedbackAnalysesHistoryResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &hist) != nil || hist.InputSignature == "" {
			t.Fatalf("history view = %d: %s", rec.Code, rec.Body.String())
		}
		return hist.InputSignature
	}
	seen := map[string]string{full.InputSignature: "initial"}
	expectNew := func(change string) string {
		t.Helper()
		sig := historySignature()
		if prev, ok := seen[sig]; ok {
			t.Fatalf("after %s the signature equals the one after %s", change, prev)
		}
		seen[sig] = change
		if again := e.list(trSID, "").InputSignature; again != sig {
			t.Fatalf("after %s the full view signature %q differs from the history signature %q", change, again, sig)
		}
		return sig
	}
	saveLong(marks, "a new position")
	expectNew("a new mark")
	if rec := e.post(trSID, trBody("pos-03", e.at(-1500*4+2).Unix(), e.at(-1500*4+400).Unix(), 96, 114, "replaced"), tfaAuth); rec.Code != http.StatusOK {
		t.Fatalf("replace mark = %d: %s", rec.Code, rec.Body.String())
	}
	replaced := expectNew("a replacement mark")
	if historySignature() != replaced {
		t.Fatalf("an unchanged poll changed the signature")
	}
	e.ss.strategiesMu.Lock()
	e.ss.feedbackAnalysisSettings = tradeFeedbackAnalysisSettings{TimeoutS: 300, MaxPositions: 10}
	e.ss.strategiesMu.Unlock()
	expectNew("a max_positions change")
	changed := trConfig()
	changed.OpenStrategy = StrategyRef{Name: "momentum", Params: map[string]interface{}{"roc_period": 9}}
	e.ss.UpdateStrategies([]StrategyConfig{changed})
	expectNew("a strategy params change")

	for _, q := range []string{"?view=full", "?view=", "?view=HISTORY", "?view=history&view=history"} {
		if rec := getRaw(q); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400: %s", q, rec.Code, rec.Body.String())
		}
	}
}

type tfaPrepareHookAdapter struct {
	*tfaAdapter
	before func()
}

func (a *tfaPrepareHookAdapter) Prepare(req tradeFeedbackAnalysisSubmit, build *tradeFeedbackBundleBuild) (tradeFeedbackAdmission, *tradeReviewError) {
	if a.before != nil {
		a.before()
	}
	return a.tfaAdapter.Prepare(req, build)
}

func TestTradeFeedbackAnalysisAdmissionRecordsCurrentSelectionAndRefusesStale(t *testing.T) {
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	db := e.store.primary()
	for _, pid := range []string{"pos-a", "pos-b", "pos-c"} {
		e.seedClosedLong(trSID, pid, "ETH", e.at(2.2), e.at(6.1))
	}
	e.saveMark(trSID, "pos-a", "a")
	e.saveMark(trSID, "pos-b", "b")
	hook := &tfaPrepareHookAdapter{tfaAdapter: e.adapter}
	e.ss.feedbackAnalysisAdapters = map[string]tradeFeedbackAnalysisAdapter{tradeFeedbackProviderAPI: hook}

	cur := e.list(trSID, "")
	rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil)
	id := tfaRequestID(t, rec)
	var accepted struct {
		BundleSHA256   string  `json:"bundle_sha256"`
		FeedbackRowIDs []int64 `json:"feedback_rowids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.BundleSHA256 != cur.Selection.BundleSHA256 || fmt.Sprint(accepted.FeedbackRowIDs) != fmt.Sprint(cur.Selection.FeedbackRowIDs) {
		t.Fatalf("admission recorded %s %v, current selection is %s %v", accepted.BundleSHA256, accepted.FeedbackRowIDs, cur.Selection.BundleSHA256, cur.Selection.FeedbackRowIDs)
	}
	stored, err := e.store.ReadTradeFeedbackAnalysis(trSID, id)
	if err != nil || stored == nil || stored.Accepted.BundleSHA256 != cur.Selection.BundleSHA256 || fmt.Sprint(stored.Accepted.FeedbackRowIDs) != fmt.Sprint(cur.Selection.FeedbackRowIDs) {
		t.Fatalf("stored accepted event = %+v (%v), want the current selection", stored, err)
	}
	e.waitTerminal(trSID, id)

	before := tfaAnalysisRows(t, db)
	calls := e.adapter.callCount()
	hook.before = func() { e.saveMark(trSID, "pos-c", "saved during admission") }
	rec = e.submit(trSID, `{"provider":"anthropic_api"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed while the request was prepared") {
		t.Fatalf("submit with a mark saved during admission = %d: %s", rec.Code, rec.Body.String())
	}
	if after := tfaAnalysisRows(t, db); fmt.Sprint(after) != fmt.Sprint(before) || e.adapter.callCount() != calls {
		t.Fatalf("a refused admission wrote rows %v -> %v or called the provider %d -> %d", before, after, calls, e.adapter.callCount())
	}
}
