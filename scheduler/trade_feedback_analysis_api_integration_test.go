package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const tfaAPISecret = "sk-ant-api-test-6f1c2b9e4d7a8c3f5e0b1a2d9c8e7f6a"

type tfaAPIStubRequest struct {
	APIKey string
	Body   map[string]interface{}
}

type tfaAPIStub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	mode     string
	result   string
	requests []tfaAPIStubRequest
	release  chan struct{}
	received chan struct{}
}

func newTFAAPIStub(t *testing.T) *tfaAPIStub {
	t.Helper()
	s := &tfaAPIStub{mode: "ok", release: make(chan struct{}), received: make(chan struct{}, 32)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	saved := tradeFeedbackAPIURLOverride
	tradeFeedbackAPIURLOverride = s.srv.URL + "/v1/messages"
	t.Cleanup(func() {
		tradeFeedbackAPIURLOverride = saved
		s.unblock()
		s.srv.Close()
	})
	return s
}

func (s *tfaAPIStub) unblock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

func (s *tfaAPIStub) set(mode, result string) {
	s.mu.Lock()
	s.mode, s.result = mode, result
	s.mu.Unlock()
}

func (s *tfaAPIStub) calls() []tfaAPIStubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tfaAPIStubRequest(nil), s.requests...)
}

func (s *tfaAPIStub) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, tfaAPIStubRequest{APIKey: r.Header.Get("x-api-key"), Body: body})
	mode, result, release := s.mode, s.result, s.release
	s.mu.Unlock()
	s.received <- struct{}{}
	w.Header().Set("Content-Type", "application/json")
	reply := map[string]interface{}{"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-reported-by-stub",
		"usage": map[string]interface{}{"input_tokens": 1200, "output_tokens": 300}}
	switch mode {
	case "block":
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		reply["stop_reason"] = "end_turn"
		reply["content"] = []map[string]interface{}{{"type": "text", "text": result}}
	case "refusal":
		reply["stop_reason"] = "refusal"
		reply["stop_details"] = map[string]interface{}{"type": "refusal", "category": "cyber"}
		reply["content"] = []map[string]interface{}{}
	case "max_tokens":
		reply["stop_reason"] = "max_tokens"
		reply["content"] = []map[string]interface{}{{"type": "text", "text": `{"summary":"cut`}}
	case "echo":
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"type": "error",
			"error": map[string]interface{}{"type": "authentication_error", "message": "invalid x-api-key " + r.Header.Get("x-api-key")}})
		return
	default:
		reply["stop_reason"] = "end_turn"
		reply["content"] = []map[string]interface{}{{"type": "text", "text": result}}
	}
	json.NewEncoder(w).Encode(reply)
}

type tfaLockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *tfaLockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *tfaLockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureTFALog(t *testing.T) *tfaLockedLog {
	t.Helper()
	l := &tfaLockedLog{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

func tfaAPIConfig(model, effort string) *Config {
	cfg := &Config{Strategies: []StrategyConfig{trConfig()}}
	if model == "" && effort == "" {
		return cfg
	}
	block := &TradeFeedbackAnalysisConfig{}
	if model != "" {
		block.APIModel = &model
	}
	if effort != "" {
		block.APIEffort = &effort
	}
	cfg.TradeFeedbackAnalysis = block
	return cfg
}

func newTFAAPIEnv(t *testing.T) (*tfaEnv, *tfaAPIStub) {
	t.Helper()
	t.Setenv(tradeFeedbackAPIKeyEnv, tfaAPISecret)
	stub := newTFAAPIStub(t)
	e := newTFAEnv(t, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
	e.ss.feedbackAnalysisAdapters = tradeFeedbackProductionAdapters(e.ss)
	return e, stub
}

func tfaAllAnalysisText(t *testing.T, db *StateDB) string {
	t.Helper()
	rows, err := db.db.Query(`SELECT request_id, event_status, strategy_id, partition, provider, model, model_verification, effort,
		bundle_sha256, feedback_rowids, result_json, validation_json, error, usage_json, created_at, bundle_json FROM trade_feedback_analyses`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		cols := make([]string, 16)
		ptrs := make([]interface{}, len(cols))
		for i := range cols {
			ptrs[i] = &cols[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		sb.WriteString(strings.Join(cols, "\x00"))
	}
	return sb.String()
}

func TestTradeFeedbackAnalysisAPIConfigLoadAndRefusals(t *testing.T) {
	sc := noEdgeHLStrategy("hl-eth", "momentum", "ETH", "1h", "--mode=paper")
	load := func(block interface{}) (*Config, error) {
		extra := map[string]interface{}{}
		if block != nil {
			extra["trade_feedback_analysis"] = block
		}
		return LoadConfigReadOnly(writeNoEdgeConfigRoot(t, CurrentConfigVersion, extra, sc))
	}
	accepted := []struct {
		block interface{}
		want  tradeFeedbackAPISettings
	}{
		{nil, tradeFeedbackAPISettings{Effort: "high"}},
		{map[string]interface{}{}, tradeFeedbackAPISettings{Effort: "high"}},
		{map[string]interface{}{"api_model": "claude-opus-5-5"}, tradeFeedbackAPISettings{Model: "claude-opus-5-5", Effort: "high"}},
		{map[string]interface{}{"api_model": "claude-opus-5-5", "api_effort": "xhigh", "timeout_s": 600}, tradeFeedbackAPISettings{Model: "claude-opus-5-5", Effort: "xhigh"}},
		{map[string]interface{}{"api_effort": "low"}, tradeFeedbackAPISettings{Effort: "low"}},
	}
	for _, c := range accepted {
		cfg, err := load(c.block)
		if err != nil {
			t.Fatalf("%v refused: %v", c.block, err)
		}
		if got := cfg.tradeFeedbackAPISettings(); got != c.want {
			t.Fatalf("%v api settings = %+v, want %+v", c.block, got, c.want)
		}
	}
	refused := []map[string]interface{}{
		{"api_model": ""},
		{"api_model": " "},
		{"api_model": "claude opus"},
		{"api_model": "claude-opus-5-5\n"},
		{"api_model": strings.Repeat("a", 129)},
		{"api_model": 5},
		{"api_model": nil},
		{"api_model": true},
		{"api_effort": "extreme"},
		{"api_effort": "HIGH"},
		{"api_effort": ""},
		{"api_effort": nil},
		{"api_effort": 3},
		{"api_url": "http://127.0.0.1:1"},
		{"api_key": "sk-ant-x"},
	}
	for _, block := range refused {
		if _, err := load(block); err == nil {
			t.Errorf("block %#v was accepted", block)
		}
	}
}

func TestTradeFeedbackAnalysisAPIAdmissionRefusals(t *testing.T) {
	e, stub := newTFAAPIEnv(t)
	db := e.store.primary()
	e.seedDefault()
	e.ss.SetConfigContext("", tfaAPIConfig("claude-opus-5-5", ""))

	if rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no usable saved marks") {
		t.Fatalf("no marks = %d: %s", rec.Code, rec.Body.String())
	}
	e.saveMark(trSID, "pos-a", "mark")

	e.ss.SetConfigContext("", tfaAPIConfig("", ""))
	rec := e.submit(trSID, `{"provider":"anthropic_api"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "api_model") {
		t.Fatalf("missing api_model = %d: %s", rec.Code, rec.Body.String())
	}
	view := e.list(trSID, "")
	if p := tfaProviderView(view, tradeFeedbackProviderAPI); p.Available || !strings.Contains(p.Reason, "api_model") {
		t.Fatalf("provider view without api_model = %+v", p)
	}

	e.ss.SetConfigContext("", tfaAPIConfig("claude-opus-5-5", "max"))
	t.Setenv(tradeFeedbackAPIKeyEnv, "")
	rec = e.submit(trSID, `{"provider":"anthropic_api"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tradeFeedbackAPIKeyEnv) {
		t.Fatalf("missing key = %d: %s", rec.Code, rec.Body.String())
	}
	if p := tfaProviderView(e.list(trSID, ""), tradeFeedbackProviderAPI); p.Available || !strings.Contains(p.Reason, tradeFeedbackAPIKeyEnv+" is not set") {
		t.Fatalf("provider view without key = %+v", p)
	}

	t.Setenv(tradeFeedbackAPIKeyEnv, tfaAPISecret)
	if p := tfaProviderView(e.list(trSID, ""), tradeFeedbackProviderAPI); !p.Available || p.Reason != "model claude-opus-5-5, effort max" {
		t.Fatalf("configured provider view = %+v", p)
	}
	for _, body := range []string{
		`{"provider":"anthropic_api","result":{"summary":"s"}}`,
		`{"provider":"anthropic_api","model":"claude-other"}`,
		`{"provider":"anthropic_api","bundle_sha256":"` + strings.Repeat("a", 64) + `"}`,
	} {
		if rec := e.submit(trSID, body, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}

	if rows := tfaAnalysisRows(t, db); len(rows) != 0 {
		t.Fatalf("a refused admission wrote rows: %v", rows)
	}
	if n := len(stub.calls()); n != 0 {
		t.Fatalf("a refused admission called the API %d time(s)", n)
	}
	if e.mgr.waiting != 0 {
		t.Fatalf("a refused admission kept a reservation: waiting=%d", e.mgr.waiting)
	}
}

func tfaProviderView(resp tradeFeedbackAnalysesResponse, id string) tradeFeedbackProviderView {
	for _, p := range resp.Providers {
		if p.ID == id {
			return p
		}
	}
	return tradeFeedbackProviderView{}
}

func TestTradeFeedbackAnalysisAPIRestartRecordsInterruptedRequestWithoutACall(t *testing.T) {
	e, stub := newTFAAPIEnv(t)
	e.mgr.shutdown()
	store := e.store
	pending := &tradeFeedbackAnalysisEvent{RequestID: "tfa-api-interrupted", Partition: "paper", Provider: tradeFeedbackProviderAPI,
		Model: "claude-opus-5-5", Effort: "high", TimeoutS: 300, BundleVersion: 1, BundleSHA256: strings.Repeat("b", 64),
		BundleJSON: `{"bundle_version":1}`, FeedbackRowIDs: []int64{1}, CreatedAt: time.Now().UTC()}
	if _, err := store.InsertTradeFeedbackAnalysisAccepted(trSID, pending); err != nil {
		t.Fatal(err)
	}
	e.ss.SetConfigContext("", tfaAPIConfig("claude-opus-5-5", "high"))
	m := startTradeFeedbackAnalysis(e.ss, store, tfaToken, tfaStubValidator)
	if m == nil {
		t.Fatalf("manager did not start: %s", e.ss.feedbackAnalysisDown)
	}
	t.Cleanup(m.shutdown)
	if _, ok := e.ss.feedbackAnalysisAdapters[tradeFeedbackProviderAPI].(*tradeFeedbackAPIAdapter); !ok {
		t.Fatalf("the restart did not keep the API adapter")
	}
	var got tradeFeedbackAnalysisView
	for _, a := range e.list(trSID, "").Analyses {
		if a.RequestID == pending.RequestID {
			got = a
		}
	}
	if got.Status != tradeFeedbackAnalysisFailed || got.Error != tradeFeedbackInterruptedError {
		t.Fatalf("interrupted request = %+v", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(stub.calls()); n != 0 {
		t.Fatalf("restart recovery called the API %d time(s)", n)
	}
	if rows := tfaAnalysisRows(t, e.store.primary()); rows[tradeFeedbackAnalysisAccepted] != 1 || rows[tradeFeedbackAnalysisFailed] != 1 {
		t.Fatalf("rows after recovery = %v", rows)
	}
}

func TestTradeFeedbackAnalysisAPIReloadPublishesSettings(t *testing.T) {
	e, _ := newTFAAPIEnv(t)
	cfg := tfaAPIConfig("", "")
	e.ss.SetConfigContext("", cfg)
	steps := []struct {
		next *Config
		want tradeFeedbackAPISettings
	}{
		{tfaAPIConfig("claude-opus-5-5", ""), tradeFeedbackAPISettings{Model: "claude-opus-5-5", Effort: "high"}},
		{tfaAPIConfig("claude-sonnet-5-5", "low"), tradeFeedbackAPISettings{Model: "claude-sonnet-5-5", Effort: "low"}},
		{tfaAPIConfig("", ""), tradeFeedbackAPISettings{Effort: "high"}},
	}
	for i, step := range steps {
		changes, err := applyHotReloadConfig(cfg, step.next, e.state, nil, e.ss)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if _, got := e.ss.currentFeedbackAnalysisConfig(); got != step.want {
			t.Fatalf("reload %d published %+v, want %+v", i, got, step.want)
		}
		if got := cfg.tradeFeedbackAPISettings(); got != step.want {
			t.Fatalf("reload %d left the running config at %+v, want %+v", i, got, step.want)
		}
		joined := strings.Join(changes, "\n")
		if !strings.Contains(joined, "trade_feedback_analysis:") {
			t.Fatalf("reload %d reported no change: %q", i, joined)
		}
		if b := step.next.TradeFeedbackAnalysis; b != nil && b.APIModel != nil {
			*b.APIModel = "mutated-after-reload"
			if got := cfg.tradeFeedbackAPISettings(); got != step.want {
				t.Fatalf("reload %d shares the api_model value with the parsed config: %+v", i, got)
			}
		}
	}
}
