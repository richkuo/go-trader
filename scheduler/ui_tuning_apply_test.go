package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeTuningApplyTestConfig(t *testing.T, dir string, extraRoot map[string]any) string {
	t.Helper()
	root := map[string]any{
		"config_version": 17,
		"strategies": []any{
			map[string]any{
				"id":       "spot-a",
				"type":     "spot",
				"platform": "binanceus",
				"script":   "shared_scripts/check_strategy.py",
				"capital":  1000,
				"args":     []any{"sma_crossover", "BTC/USDT", "1h"},
				"open_strategy": map[string]any{
					"name":   "sma_crossover",
					"params": map[string]any{"fast": 10.0, "slow": 50.0},
				},
			},
			map[string]any{
				"id":       "spot-b",
				"type":     "spot",
				"platform": "binanceus",
				"script":   "shared_scripts/check_strategy.py",
				"capital":  1000,
				"args":     []any{"sma_crossover", "ETH/USDT", "1h"},
				"open_strategy": map[string]any{
					"name":   "sma_crossover",
					"params": map[string]any{"fast": 12.0, "slow": 40.0},
				},
			},
		},
	}
	for k, v := range extraRoot {
		root[k] = v
	}
	path := filepath.Join(dir, "config.json")
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func samplePromotionBaseline(open map[string]any, userDefaults any, userDefaultsPresent bool, userClose any, userClosePresent bool) map[string]any {
	return map[string]any{
		"open_strategy":               open,
		"open_strategy_present":       true,
		"user_defaults":               userDefaults,
		"user_defaults_present":       userDefaultsPresent,
		"user_close_defaults":         userClose,
		"user_close_defaults_present": userClosePresent,
	}
}

func sampleV2Results(strategyA, strategyB map[string]any) map[string]any {
	return map[string]any{
		"schema_version": 2,
		"tool":           "tune_live",
		"strategies":     []any{strategyA, strategyB},
	}
}

func survivorRow(key string, patchOpen map[string]any) map[string]any {
	return map[string]any{
		"key":     key,
		"verdict": "survivor",
		"params":  patchOpen["params"],
		"patch": map[string]any{
			"strategy_id":   "spot-a",
			"open_strategy": patchOpen,
			"param_changes": map[string]any{"fast": map[string]any{"from": 10.0, "to": 20.0}},
		},
		"evidence":    map[string]any{},
		"limitations": []any{},
	}
}

func seedCompletedTuningRun(t *testing.T, mgr *tuningRunManager, id string, results map[string]any) {
	t.Helper()
	dir := filepath.Join(mgr.rootDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	rec := tuningRunRecord{
		ID:          id,
		Status:      tuningRunCompleted,
		StrategyIDs: []string{"spot-a", "spot-b"},
		CreatedAt:   now,
		CompletedAt: &now,
	}
	mgr.mu.Lock()
	mgr.runs[id] = rec
	mgr.mu.Unlock()
	if err := writeTuningJSON(filepath.Join(dir, tuningRunRecordFile), rec); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(dir, tuningRunSpecFile), tuningRunSpec{
		SchemaVersion: 1, StrategyIDs: []string{"spot-a", "spot-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(dir, tuningRunResultsFile), results); err != nil {
		t.Fatal(err)
	}
}

func newTuningApplyServer(t *testing.T, configPath string) (*StatusServer, *tuningRunManager) {
	t.Helper()
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ss := NewStatusServer(nil, nil, "", []StrategyConfig{{ID: "spot-a"}, {ID: "spot-b"}}, nil)
	ss.configPath = configPath
	ss.tuning = mgr
	// Neutralize production SIGHUP so apply tests do not signal this process.
	ss.reloadConfig = func() error { return nil }
	return ss, mgr
}

func defaultApplyArtifacts(openA map[string]any) map[string]any {
	baselineOpen := map[string]any{
		"name":   "sma_crossover",
		"params": map[string]any{"fast": 10.0, "slow": 50.0},
	}
	patchOpen := openA
	if patchOpen == nil {
		patchOpen = map[string]any{
			"name":   "sma_crossover",
			"params": map[string]any{"fast": 20.0, "slow": 50.0},
		}
	}
	stratA := map[string]any{
		"strategy_id":        "spot-a",
		"status":             "ok",
		"open_strategy":      "sma_crossover",
		"baseline_params":    baselineOpen["params"],
		"promotion_baseline": samplePromotionBaseline(baselineOpen, nil, false, nil, false),
		"ranked": []any{
			map[string]any{"key": "baseline", "verdict": "baseline", "params": baselineOpen["params"]},
			survivorRow("cand_1", patchOpen),
			map[string]any{"key": "cand_2", "verdict": "rejected", "params": map[string]any{"fast": 30.0}},
		},
	}
	stratB := map[string]any{
		"strategy_id":     "spot-b",
		"status":          "ok",
		"open_strategy":   "sma_crossover",
		"baseline_params": map[string]any{"fast": 12.0, "slow": 40.0},
		"promotion_baseline": samplePromotionBaseline(map[string]any{
			"name": "sma_crossover", "params": map[string]any{"fast": 12.0, "slow": 40.0},
		}, nil, false, nil, false),
		"ranked": []any{
			survivorRow("cand_1", map[string]any{
				"name": "sma_crossover", "params": map[string]any{"fast": 99.0, "slow": 40.0},
			}),
		},
	}
	// Fix strategy_id inside spot-b's patch.
	ranked := stratB["ranked"].([]any)
	row := ranked[0].(map[string]any)
	patch := row["patch"].(map[string]any)
	patch["strategy_id"] = "spot-b"
	return sampleV2Results(stratA, stratB)
}

func postTuningApply(ss *StatusServer, body string, token, origin, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/tuning/apply", strings.NewReader(body))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	ss.handleAPITuningApply(w, r)
	return w
}

func TestReplaceStrategyOpenStrategyExact(t *testing.T) {
	root := map[string]json.RawMessage{}
	list := []json.RawMessage{
		json.RawMessage(`{"id":"spot-a","open_strategy":{"name":"old","params":{"fast":1}},"type":"spot"}`),
		json.RawMessage(`{"id":"spot-b","open_strategy":{"name":"keep","params":{"fast":2}},"type":"spot"}`),
	}
	if err := setConfigStrategies(root, list); err != nil {
		t.Fatal(err)
	}
	root["user_defaults"] = json.RawMessage(`{"close":{"x":1}}`)
	patch := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	if err := replaceStrategyOpenStrategy(root, "spot-a", patch); err != nil {
		t.Fatal(err)
	}
	updated, err := configStrategies(root)
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]json.RawMessage
	if err := json.Unmarshal(updated[0], &a); err != nil {
		t.Fatal(err)
	}
	if !canonicalJSONEqual(a["open_strategy"], patch) {
		t.Fatalf("open_strategy = %s, want %s", a["open_strategy"], patch)
	}
	if string(a["type"]) != `"spot"` {
		t.Fatalf("untargeted type mutated: %s", a["type"])
	}
	var b map[string]json.RawMessage
	if err := json.Unmarshal(updated[1], &b); err != nil {
		t.Fatal(err)
	}
	if !canonicalJSONEqual(b["open_strategy"], json.RawMessage(`{"name":"keep","params":{"fast":2}}`)) {
		t.Fatalf("peer strategy mutated: %s", b["open_strategy"])
	}
	if !canonicalJSONEqual(root["user_defaults"], json.RawMessage(`{"close":{"x":1}}`)) {
		t.Fatalf("root user_defaults mutated: %s", root["user_defaults"])
	}
}

func TestPromotionBaselinesEqualCanonical(t *testing.T) {
	a := tuningPromotionBaseline{
		OpenStrategyPresent: true,
		OpenStrategy:        json.RawMessage(`{"name":"sma","params":{"fast":1,"slow":2}}`),
	}
	b := tuningPromotionBaseline{
		OpenStrategyPresent: true,
		OpenStrategy:        json.RawMessage(`{"params":{"slow":2.0,"fast":1},"name":"sma"}`),
	}
	if !promotionBaselinesEqual(a, b) {
		t.Fatal("reordered keys / 1 vs 1.0 should not count as drift")
	}
	c := a
	c.OpenStrategyPresent = false
	if promotionBaselinesEqual(a, c) {
		t.Fatal("presence flip must count as drift")
	}
}

func TestTuningApplyAuthAndSameOrigin(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	ss.statusToken = "secret"
	runID := "20260720T120000000000000Z-applyauth"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if rr := postTuningApply(ss, body, "", "http://localhost:8099", "localhost:8099"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rr.Code)
	}
	if rr := postTuningApply(ss, body, "wrong", "http://localhost:8099", "localhost:8099"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d", rr.Code)
	}
	if rr := postTuningApply(ss, body, "secret", "http://evil.example", "localhost:8099"); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", rr.Code)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("auth failures must not write config")
	}

	rr := postTuningApply(ss, body, "secret", "", "localhost:8099") // absent Origin accepted
	if rr.Code != http.StatusOK {
		t.Fatalf("absent origin status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTuningApplyTokenlessLoopbackAllowed(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appyloop"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`
	rr := postTuningApply(ss, body, "", "http://127.0.0.1:8099", "127.0.0.1:8099")
	if rr.Code != http.StatusOK {
		t.Fatalf("tokenless loopback status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTuningApplyIdentityResolvesPerStrategy(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appyid"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))

	rr := postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("spot-a apply status = %d body=%s", rr.Code, rr.Body.String())
	}
	root, err := readConfigRootMap(configPath)
	if err != nil {
		t.Fatal(err)
	}
	live, err := extractLivePromotionBaseline(root, "spot-a")
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	if !canonicalJSONEqual(live.OpenStrategy, want) {
		t.Fatalf("spot-a open_strategy = %s", live.OpenStrategy)
	}
	peer, err := extractLivePromotionBaseline(root, "spot-b")
	if err != nil {
		t.Fatal(err)
	}
	peerWant := json.RawMessage(`{"name":"sma_crossover","params":{"fast":12,"slow":40}}`)
	if !canonicalJSONEqual(peer.OpenStrategy, peerWant) {
		t.Fatalf("spot-b should be untouched: %s", peer.OpenStrategy)
	}
}

func TestTuningApplyRefusesLegacyAndNonSurvivors(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	before, _ := os.ReadFile(configPath)

	v1ID := "20260720T120000000000000Z-appyv1"
	seedCompletedTuningRun(t, mgr, v1ID, map[string]any{
		"schema_version": 1,
		"strategies": []any{map[string]any{
			"strategy_id": "spot-a",
			"ranked":      []any{survivorRow("cand_1", map[string]any{"name": "sma_crossover", "params": map[string]any{"fast": 20.0}})},
		}},
	})
	rr := postTuningApply(ss, `{"run_id":"`+v1ID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonLegacy) {
		t.Fatalf("v1 refuse = %d %s", rr.Code, rr.Body.String())
	}

	v2ID := "20260720T120000000000000Z-appyv2"
	seedCompletedTuningRun(t, mgr, v2ID, defaultApplyArtifacts(nil))
	for _, key := range []string{"baseline", "cand_2", "missing"} {
		rr := postTuningApply(ss, `{"run_id":"`+v2ID+`","strategy_id":"spot-a","suggestion_key":"`+key+`"}`, "", "", "localhost")
		if rr.Code == http.StatusOK {
			t.Fatalf("key %s should be refused", key)
		}
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Fatal("refused applies must not write config")
	}
}

func TestTuningApplyRefusesBaselineDrift(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		mut   func(path string)
	}{
		{
			name: "open_strategy",
			mut: func(path string) {
				root, _ := readConfigRootMap(path)
				_ = replaceStrategyOpenStrategy(root, "spot-a", json.RawMessage(`{"name":"sma_crossover","params":{"fast":11,"slow":50}}`))
				_ = writeValidatedConfigRoot(path, root)
			},
		},
		{
			name:  "user_defaults_added",
			extra: nil,
			mut: func(path string) {
				root, _ := readConfigRootMap(path)
				root["user_defaults"] = json.RawMessage(`{"close":{"tiered_tp_atr":{"tp_tiers":[{"atr_multiple":1.5,"fraction":1.0}]}}}`)
				_ = writeValidatedConfigRoot(path, root)
			},
		},
		{
			name: "user_close_defaults_added",
			mut: func(path string) {
				root, _ := readConfigRootMap(path)
				root["user_close_defaults"] = json.RawMessage(`{"tiered_tp_atr":{"tp_tiers":[{"atr_multiple":1.5,"fraction":1.0}]}}`)
				_ = writeValidatedConfigRoot(path, root)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeTuningApplyTestConfig(t, t.TempDir(), tc.extra)
			ss, mgr := newTuningApplyServer(t, configPath)
			runID := "20260720T120000000000000Z-appydrift"
			seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
			tc.mut(configPath)
			before, _ := os.ReadFile(configPath)
			rr := postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonBaselineDrift) {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			after, _ := os.ReadFile(configPath)
			if string(before) != string(after) {
				t.Fatal("drift refusal must leave config byte-identical")
			}
		})
	}
}

func TestTuningApplyIdempotentAndCrashRecovery(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appycrash"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`

	// Crash before config write: pending journal, config untouched → retry applies.
	patch := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	hash, err := patchSHA256(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.upsertPromotion(tuningPromotionRecord{
		RunID: runID, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr.now(),
	}); err != nil {
		t.Fatal(err)
	}
	rr := postTuningApply(ss, body, "", "", "localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("pending retry status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Crash after config write but before finalization: target equals patch → finalize.
	configPath2 := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss2, mgr2 := newTuningApplyServer(t, configPath2)
	runID2 := "20260720T120000000000000Z-appyfin"
	seedCompletedTuningRun(t, mgr2, runID2, defaultApplyArtifacts(nil))
	root, _ := readConfigRootMap(configPath2)
	_ = replaceStrategyOpenStrategy(root, "spot-a", patch)
	_ = writeValidatedConfigRoot(configPath2, root)
	before, _ := os.ReadFile(configPath2)
	if err := mgr2.upsertPromotion(tuningPromotionRecord{
		RunID: runID2, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr2.now(),
	}); err != nil {
		t.Fatal(err)
	}
	rr = postTuningApply(ss2, `{"run_id":"`+runID2+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tuningReasonAlreadyApplied) {
		t.Fatalf("finalize pending status=%d body=%s", rr.Code, rr.Body.String())
	}
	after, _ := os.ReadFile(configPath2)
	// Validated writer may re-indent; semantic equality is the invariant.
	rootAfter, _ := readConfigRootMap(configPath2)
	live, present, err := extractLiveOpenStrategy(rootAfter, "spot-a")
	if err != nil || !present || !canonicalJSONEqual(live, patch) {
		t.Fatalf("finalize changed semantics: present=%v live=%s before_len=%d after_len=%d", present, live, len(before), len(after))
	}

	// Idempotent double-apply.
	rr = postTuningApply(ss2, `{"run_id":"`+runID2+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tuningReasonAlreadyApplied) {
		t.Fatalf("double apply status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTuningApplyJournalSurvivesPrune(t *testing.T) {
	dir := t.TempDir()
	configPath := writeTuningApplyTestConfig(t, dir, nil)
	mgr, err := newTuningRunManager(configPath, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	ss := NewStatusServer(nil, nil, "", []StrategyConfig{{ID: "spot-a"}, {ID: "spot-b"}}, nil)
	ss.configPath = configPath
	ss.tuning = mgr
	ss.reloadConfig = func() error { return nil }

	runID := "20260720T120000000000000Z-appyprune"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	rr := postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Seed a newer terminal run and prune the applied one away.
	newer := "20260720T130000000000000Z-appynew"
	seedCompletedTuningRun(t, mgr, newer, defaultApplyArtifacts(nil))
	mgr.pruneRetainedRuns()
	if _, err := os.Stat(filepath.Join(mgr.rootDir, runID)); !os.IsNotExist(err) {
		t.Fatalf("expected pruned run dir, err=%v", err)
	}
	if _, err := os.Stat(mgr.promotionsPath()); err != nil {
		t.Fatalf("journal missing after prune: %v", err)
	}
	rec, ok, err := mgr.getPromotion(runID, "spot-a", "cand_1")
	if err != nil || !ok || rec.State != tuningPromoApplied {
		t.Fatalf("journal record lost: ok=%v rec=%+v err=%v", ok, rec, err)
	}

	// Pruned run + applied journal → idempotent already_applied, no write.
	beforeApplied, _ := os.ReadFile(configPath)
	rr = postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tuningReasonAlreadyApplied) {
		t.Fatalf("pruned applied retry status=%d body=%s", rr.Code, rr.Body.String())
	}
	afterApplied, _ := os.ReadFile(configPath)
	if string(beforeApplied) != string(afterApplied) {
		t.Fatal("pruned applied retry must not rewrite config")
	}

	// Pending + pruned run → manual_review, no config write.
	configPath2 := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss2, mgr2 := newTuningApplyServer(t, configPath2)
	pendingID := "20260720T140000000000000Z-appypend"
	seedCompletedTuningRun(t, mgr2, pendingID, defaultApplyArtifacts(nil))
	hash, _ := patchSHA256(json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`))
	if err := mgr2.upsertPromotion(tuningPromotionRecord{
		RunID: pendingID, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr2.now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(mgr2.rootDir, pendingID)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(configPath2)
	rr = postTuningApply(ss2, `{"run_id":"`+pendingID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonManualReview) {
		t.Fatalf("pruned pending status=%d body=%s", rr.Code, rr.Body.String())
	}
	after, _ := os.ReadFile(configPath2)
	if string(before) != string(after) {
		t.Fatal("pruned pending must not write config")
	}
	rec, ok, err = mgr2.getPromotion(pendingID, "spot-a", "cand_1")
	if err != nil || !ok || rec.State != tuningPromoManualReview {
		t.Fatalf("pruned pending journal state=%q ok=%v err=%v", rec.State, ok, err)
	}
}

func TestTuningApplySerializesWithConfigWriteMu(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appyrace"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))

	var started sync.WaitGroup
	started.Add(1)
	var release sync.WaitGroup
	release.Add(1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
			started.Done()
			release.Wait()
			root["user_defaults"] = json.RawMessage(`{"close":{"tiered_tp_atr":{"tp_tiers":[{"atr_multiple":2.0,"fraction":1.0}]}}}`)
			return nil
		})
	}()
	started.Wait()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	}()

	select {
	case <-done:
		t.Fatal("apply returned while configWriteMu held")
	case <-time.After(50 * time.Millisecond):
	}
	release.Done()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	rr := <-done
	// After the racing write, baseline (no user_defaults → present) has drifted.
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonBaselineDrift) {
		t.Fatalf("loser should see drift, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestTuningRunDetailOverlaysEligibility(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	ss.statusToken = "secret"
	runID := "20260720T120000000000000Z-appyelig"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))

	r := httptest.NewRequest(http.MethodGet, "/api/tuning/runs/"+runID, nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	ss.handleAPITuningRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	var detail tuningRunDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	strategies := detail.Results["strategies"].([]any)
	stratA := strategies[0].(map[string]any)
	ranked := stratA["ranked"].([]any)
	cand1 := ranked[1].(map[string]any)
	if cand1["apply_eligibility"] != tuningEligEligible {
		t.Fatalf("cand_1 eligibility = %#v", cand1["apply_eligibility"])
	}
	baseline := ranked[0].(map[string]any)
	if baseline["apply_eligibility"] != tuningEligNotSurvivor {
		t.Fatalf("baseline eligibility = %#v", baseline["apply_eligibility"])
	}
}

func TestTuningEligibilityOverlayLoadsJournalOnce(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	ss.statusToken = "secret"
	runID := "20260720T120000000000000Z-appyonce"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	// Seed a journal entry so the load path is exercised (missing file also
	// loads once, but an existing file matches the polled production case).
	if err := mgr.upsertPromotion(tuningPromotionRecord{
		RunID: runID, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoApplied, PatchHash: "abc", CreatedAt: mgr.now(),
	}); err != nil {
		t.Fatal(err)
	}

	var loads atomic.Int32
	tuningJournalLoadHook = func() { loads.Add(1) }
	t.Cleanup(func() { tuningJournalLoadHook = nil })

	r := httptest.NewRequest(http.MethodGet, "/api/tuning/runs/"+runID, nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	ss.handleAPITuningRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("journal loads during detail overlay = %d, want 1 (one read for all ranked rows)", got)
	}

	var detail tuningRunDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	strategies := detail.Results["strategies"].([]any)
	stratA := strategies[0].(map[string]any)
	ranked := stratA["ranked"].([]any)
	cand1 := ranked[1].(map[string]any)
	if cand1["apply_eligibility"] != tuningEligAlreadyApplied {
		t.Fatalf("cand_1 eligibility = %#v, want already_applied", cand1["apply_eligibility"])
	}
	// Missing journal still overlays correctly (second GET after clearing hook file).
	loads.Store(0)
	if err := os.Remove(mgr.promotionsPath()); err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	ss.handleAPITuningRun(w2, r)
	if w2.Code != http.StatusOK {
		t.Fatalf("detail after journal remove status=%d", w2.Code)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("missing-journal detail loads = %d, want 1", got)
	}
}

func TestTuningApplySignalsReloadOnSuccessNotRefusal(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	var reloads atomic.Int32
	ss.reloadConfig = func() error {
		reloads.Add(1)
		return nil
	}
	runID := "20260720T120000000000000Z-appyreload"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`

	// Refusal paths must never signal.
	rr := postTuningApply(ss, `{"run_id":"`+runID+`","strategy_id":"spot-a","suggestion_key":"baseline"}`, "", "", "localhost")
	if rr.Code == http.StatusOK {
		t.Fatal("baseline row should be refused")
	}
	if reloads.Load() != 0 {
		t.Fatalf("refusal signaled reload %d times", reloads.Load())
	}

	rr = postTuningApply(ss, body, "", "", "localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rr.Code, rr.Body.String())
	}
	if reloads.Load() != 1 {
		t.Fatalf("successful apply reload count=%d, want 1", reloads.Load())
	}
	if !strings.Contains(rr.Body.String(), "Applied via SIGHUP") && !strings.Contains(rr.Body.String(), `"message"`) {
		t.Fatalf("apply response missing reload message: %s", rr.Body.String())
	}

	// Idempotent retry of applied must not signal again.
	rr = postTuningApply(ss, body, "", "", "localhost")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tuningReasonAlreadyApplied) {
		t.Fatalf("idempotent retry status=%d body=%s", rr.Code, rr.Body.String())
	}
	if reloads.Load() != 1 {
		t.Fatalf("idempotent applied retry re-signaled: count=%d", reloads.Load())
	}

	// Crash-recovery finalize (pending + config already equals patch) must signal.
	configPath2 := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss2, mgr2 := newTuningApplyServer(t, configPath2)
	var reloads2 atomic.Int32
	ss2.reloadConfig = func() error {
		reloads2.Add(1)
		return nil
	}
	runID2 := "20260720T120000000000000Z-appyfinrl"
	seedCompletedTuningRun(t, mgr2, runID2, defaultApplyArtifacts(nil))
	patch := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	hash, err := patchSHA256(patch)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := readConfigRootMap(configPath2)
	_ = replaceStrategyOpenStrategy(root, "spot-a", patch)
	_ = writeValidatedConfigRoot(configPath2, root)
	if err := mgr2.upsertPromotion(tuningPromotionRecord{
		RunID: runID2, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr2.now(),
	}); err != nil {
		t.Fatal(err)
	}
	rr = postTuningApply(ss2, `{"run_id":"`+runID2+`","strategy_id":"spot-a","suggestion_key":"cand_1"}`, "", "", "localhost")
	if rr.Code != http.StatusOK {
		t.Fatalf("finalize pending status=%d body=%s", rr.Code, rr.Body.String())
	}
	if reloads2.Load() != 1 {
		t.Fatalf("pending finalize reload count=%d, want 1", reloads2.Load())
	}
}

func TestTuningApplyStrategyRemovedSettlesManualReview(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appymiss"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	patch := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	hash, err := patchSHA256(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.upsertPromotion(tuningPromotionRecord{
		RunID: runID, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr.now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate concurrent remove-strategy between pre-check and locked write:
	// pending already exists, live config no longer has spot-a.
	root, err := readConfigRootMap(configPath)
	if err != nil {
		t.Fatal(err)
	}
	list, err := configStrategies(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := list[:0]
	for _, raw := range list {
		if strategyRawID(raw) != "spot-a" {
			kept = append(kept, raw)
		}
	}
	if err := setConfigStrategies(root, kept); err != nil {
		t.Fatal(err)
	}
	if err := writeValidatedConfigRoot(configPath, root); err != nil {
		t.Fatal(err)
	}

	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`
	rr := postTuningApply(ss, body, "", "", "localhost")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonManualReview) {
		t.Fatalf("missing strategy status=%d body=%s", rr.Code, rr.Body.String())
	}
	rec, ok, err := mgr.getPromotion(runID, "spot-a", "cand_1")
	if err != nil || !ok || rec.State != tuningPromoManualReview {
		t.Fatalf("journal state=%q ok=%v err=%v, want manual_review", rec.State, ok, err)
	}

	// Retry of manual_review stays refused — never silently applies.
	rr = postTuningApply(ss, body, "", "", "localhost")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tuningReasonManualReview) {
		t.Fatalf("manual_review retry status=%d body=%s", rr.Code, rr.Body.String())
	}
	rec, ok, err = mgr.getPromotion(runID, "spot-a", "cand_1")
	if err != nil || !ok || rec.State != tuningPromoManualReview {
		t.Fatalf("retry mutated journal to %q", rec.State)
	}
}

func TestTuningApplyTransientErrorLeavesPending(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	runID := "20260720T120000000000000Z-appytrans"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))
	patch := json.RawMessage(`{"name":"sma_crossover","params":{"fast":20,"slow":50}}`)
	hash, err := patchSHA256(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.upsertPromotion(tuningPromotionRecord{
		RunID: runID, StrategyID: "spot-a", SuggestionKey: "cand_1",
		State: tuningPromoPending, PatchHash: hash, CreatedAt: mgr.now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Transient: corrupt config so the recover path fails before identity checks.
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"run_id":"` + runID + `","strategy_id":"spot-a","suggestion_key":"cand_1"}`
	rr := postTuningApply(ss, body, "", "", "localhost")
	if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), tuningReasonManualReview) {
		t.Fatalf("transient failure should not settle manual_review: %d %s", rr.Code, rr.Body.String())
	}
	rec, ok, err := mgr.getPromotion(runID, "spot-a", "cand_1")
	if err != nil || !ok || rec.State != tuningPromoPending {
		t.Fatalf("transient failure mutated journal to %q ok=%v err=%v", rec.State, ok, err)
	}
}

func TestTuningEligibilityOverlayCachesLiveBaselinePerStrategy(t *testing.T) {
	configPath := writeTuningApplyTestConfig(t, t.TempDir(), nil)
	ss, mgr := newTuningApplyServer(t, configPath)
	ss.statusToken = "secret"
	runID := "20260720T120000000000000Z-appybaseonce"
	seedCompletedTuningRun(t, mgr, runID, defaultApplyArtifacts(nil))

	var extracts atomic.Int32
	extractLiveBaselineHook = func() { extracts.Add(1) }
	t.Cleanup(func() { extractLiveBaselineHook = nil })

	r := httptest.NewRequest(http.MethodGet, "/api/tuning/runs/"+runID, nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	ss.handleAPITuningRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	// defaultApplyArtifacts has 2 strategies; ranked rows per strategy > 1.
	// One extract per strategy id — not one per ranked row.
	if got := extracts.Load(); got != 2 {
		t.Fatalf("live baseline extracts = %d, want 2 (one per strategy)", got)
	}
}
