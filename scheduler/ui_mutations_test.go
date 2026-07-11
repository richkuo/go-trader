package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newMutationTestServer builds a StatusServer wired to a real temp config
// file containing one spot strategy, with the SIGHUP trigger stubbed to a
// counter so tests can assert the reload was signaled without killing the
// test process.
func newMutationTestServer(t *testing.T) (*StatusServer, string, *int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "config_version": 16,
  "interval_seconds": 60,
  "strategies": [
    {
      "id": "spot-btc",
      "type": "spot",
      "platform": "binanceus",
      "script": "shared_scripts/check_strategy.py",
      "args": ["sma", "BTC/USDT", "1h"],
      "capital": 1000,
      "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 20, "slow_period": 50}}
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	strategies := []StrategyConfig{{
		ID:       "spot-btc",
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h"},
	}}
	ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", strategies, nil)
	ss.SetConfigContext(path, &Config{IntervalSeconds: 60})
	reloads := 0
	ss.reloadConfig = func() error {
		reloads++
		return nil
	}
	return ss, path, &reloads
}

func mutationPost(ss *StatusServer, handler func(http.ResponseWriter, *http.Request), url, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func readConfigStrategy(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root struct {
		Strategies []map[string]interface{} `json:"strategies"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(root.Strategies) != 1 {
		t.Fatalf("strategies len = %d, want 1", len(root.Strategies))
	}
	return root.Strategies[0]
}

func readConfigRoot(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return root
}

func TestUIPauseEndpointWritesAndReloads(t *testing.T) {
	ss, path, reloads := newMutationTestServer(t)
	pause := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyPause(w, r, "spot-btc") }

	w := mutationPost(ss, pause, "/api/strategies/spot-btc/pause", `{"paused":true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body %s", w.Code, w.Body.String())
	}
	if got := readConfigStrategy(t, path)["paused"]; got != true {
		t.Fatalf("paused in config = %v, want true", got)
	}
	if *reloads != 1 {
		t.Fatalf("reloads = %d, want 1", *reloads)
	}

	// Unpause deletes the key (omitempty default).
	w = mutationPost(ss, pause, "/api/strategies/spot-btc/pause", `{"paused":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unpause status = %d, body %s", w.Code, w.Body.String())
	}
	if _, present := readConfigStrategy(t, path)["paused"]; present {
		t.Fatal("paused key should be deleted on unpause")
	}
	if *reloads != 2 {
		t.Fatalf("reloads = %d, want 2", *reloads)
	}
}

func TestUIPauseEndpointGuards(t *testing.T) {
	ss, path, _ := newMutationTestServer(t)
	pause := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyPause(w, r, "spot-btc") }

	// Cross-origin POST rejected (CSRF defense — non-negotiable per #1229).
	w := mutationPost(ss, pause, "http://127.0.0.1:8099/api/strategies/spot-btc/pause", `{"paused":true}`,
		map[string]string{"Origin": "http://evil.example"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
	if _, present := readConfigStrategy(t, path)["paused"]; present {
		t.Fatal("cross-origin request must not write config")
	}

	// Non-POST rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/strategies/spot-btc/pause", nil)
	rec := httptest.NewRecorder()
	pause(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	// Missing/invalid body rejected.
	if w := mutationPost(ss, pause, "/pause", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing paused key status = %d, want 400", w.Code)
	}
	if w := mutationPost(ss, pause, "/pause", `{"paused":"yes"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("non-bool paused status = %d, want 400", w.Code)
	}

	// Unknown strategy 404s.
	other := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyPause(w, r, "nope") }
	if w := mutationPost(ss, other, "/pause", `{"paused":true}`, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown strategy status = %d, want 404", w.Code)
	}
}

func TestUIMutationsEnforceConfiguredToken(t *testing.T) {
	ss, _, _ := newMutationTestServer(t)
	ss.statusToken = "secret"
	pause := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyPause(w, r, "spot-btc") }

	if w := mutationPost(ss, pause, "/pause", `{"paused":true}`, nil); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("no-token status = %d, want auth rejection", w.Code)
	}
	w := mutationPost(ss, pause, "/pause", `{"paused":true}`, map[string]string{"Authorization": "Bearer secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestUIStrategyNotificationsOverride(t *testing.T) {
	ss, path, _ := newMutationTestServer(t)
	notif := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyNotifications(w, r, "spot-btc") }

	w := mutationPost(ss, notif, "/notifications", `{"notify_ratchet_triggers":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("set status = %d, body %s", w.Code, w.Body.String())
	}
	if got := readConfigStrategy(t, path)["notify_ratchet_triggers"]; got != false {
		t.Fatalf("notify_ratchet_triggers = %v, want false", got)
	}

	// null clears the override → inherit global (#1118).
	w = mutationPost(ss, notif, "/notifications", `{"notify_ratchet_triggers":null}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body %s", w.Code, w.Body.String())
	}
	if _, present := readConfigStrategy(t, path)["notify_ratchet_triggers"]; present {
		t.Fatal("notify_ratchet_triggers key should be deleted on null")
	}

	if w := mutationPost(ss, notif, "/notifications", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", w.Code)
	}
}

func TestUIConfigNotificationsGlobal(t *testing.T) {
	ss, path, reloads := newMutationTestServer(t)

	// GET reports the built-in default (null → effective true).
	req := httptest.NewRequest(http.MethodGet, "/api/config/notifications", nil)
	rec := httptest.NewRecorder()
	ss.handleAPIConfigNotifications(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var got uiConfigNotificationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse GET: %v", err)
	}
	if got.NotifyRatchetTriggers != nil || !got.Effective {
		t.Fatalf("GET = %+v, want null/effective-true", got)
	}

	// POST false writes the root key, mirrors, and reloads.
	w := mutationPost(ss, ss.handleAPIConfigNotifications, "/api/config/notifications", `{"notify_ratchet_triggers":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body %s", w.Code, w.Body.String())
	}
	root := readConfigRoot(t, path)
	if string(root["notify_ratchet_triggers"]) != "false" {
		t.Fatalf("root notify_ratchet_triggers = %s, want false", root["notify_ratchet_triggers"])
	}
	if *reloads != 1 {
		t.Fatalf("reloads = %d, want 1", *reloads)
	}
	rec = httptest.NewRecorder()
	ss.handleAPIConfigNotifications(rec, httptest.NewRequest(http.MethodGet, "/api/config/notifications", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse GET: %v", err)
	}
	if got.NotifyRatchetTriggers == nil || *got.NotifyRatchetTriggers || got.Effective {
		t.Fatalf("GET after POST = %+v, want false/effective-false", got)
	}

	// POST null deletes the key.
	w = mutationPost(ss, ss.handleAPIConfigNotifications, "/api/config/notifications", `{"notify_ratchet_triggers":null}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST null status = %d, body %s", w.Code, w.Body.String())
	}
	if _, present := readConfigRoot(t, path)["notify_ratchet_triggers"]; present {
		t.Fatal("root notify_ratchet_triggers should be deleted on null")
	}

	// Cross-origin POST rejected.
	w = mutationPost(ss, ss.handleAPIConfigNotifications, "http://127.0.0.1:8099/api/config/notifications",
		`{"notify_ratchet_triggers":true}`, map[string]string{"Origin": "http://evil.example"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
}

// TestUIMutationsInterleavedWrites drives concurrent UI pause toggles and a
// Discord-style root mutation (mutateConfig shape: read → patch → validated
// write under the same configWriteMu) and asserts neither clobbers the other
// (#1229 parent acceptance criterion).
func TestUIMutationsInterleavedWrites(t *testing.T) {
	ss, path, _ := newMutationTestServer(t)
	pause := func(w http.ResponseWriter, r *http.Request) { ss.handleAPIStrategyPause(w, r, "spot-btc") }

	var wg sync.WaitGroup
	const rounds = 20
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			w := mutationPost(ss, pause, "/pause", `{"paused":true}`, nil)
			if w.Code != http.StatusOK {
				t.Errorf("pause round %d status = %d", i, w.Code)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			ss.configWriteMu.Lock()
			data, err := os.ReadFile(path)
			if err == nil {
				var root map[string]json.RawMessage
				if err = json.Unmarshal(data, &root); err == nil {
					root["notify_ratchet_triggers"], _ = json.Marshal(i%2 == 0)
					err = writeValidatedConfigRoot(path, root)
				}
			}
			ss.configWriteMu.Unlock()
			if err != nil {
				t.Errorf("root write round %d: %v", i, err)
				return
			}
		}
	}()
	wg.Wait()

	// Both writers' final state survives: paused=true from the UI lane and a
	// root notify_ratchet_triggers bool from the Discord-style lane.
	strat := readConfigStrategy(t, path)
	if strat["paused"] != true {
		t.Fatalf("paused = %v, want true after interleaved writes", strat["paused"])
	}
	root := readConfigRoot(t, path)
	if v := string(root["notify_ratchet_triggers"]); v != "true" && v != "false" {
		t.Fatalf("root notify_ratchet_triggers = %q, want a bool to survive", v)
	}
}

// TestApplyHotReloadGlobalNotifyRatchetTriggers pins the #1256 global
// notify_ratchet_triggers hot-reload copy: without it a dashboard/Discord
// toggle of the global default silently waits for a restart.
func TestApplyHotReloadGlobalNotifyRatchetTriggers(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 100, MaxDrawdownPct: 10,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 100, MaxDrawdownPct: 10,
	}})
	off := false
	next.NotifyRatchetTriggers = &off

	changes, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if cfg.NotifyRatchetTriggers == nil || *cfg.NotifyRatchetTriggers {
		t.Fatalf("global notify_ratchet_triggers not copied: %v", cfg.NotifyRatchetTriggers)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "notify_ratchet_triggers") {
		t.Fatalf("expected change entry, got %q", joined)
	}
}
