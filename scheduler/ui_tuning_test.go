package main

import (
	"context"
	"encoding/json"
	"errors"
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

func writeTuningTestConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	body := `{"config_version":17,"strategies":[` +
		`{"id":"spot-a","type":"spot","platform":"binanceus","args":["sma_crossover","BTC/USDT","1h"]},` +
		`{"id":"spot-b","type":"spot","platform":"binanceus","args":["sma_crossover","ETH/USDT","1h"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveTuningPathsUsesResolvedConfigSibling(t *testing.T) {
	stateDir := t.TempDir()
	configPath := writeTuningTestConfig(t, stateDir)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "config.json")
	if err := os.Symlink(configPath, link); err != nil {
		t.Fatal(err)
	}

	root, cache, err := resolveTuningPaths(link)
	if err != nil {
		t.Fatal(err)
	}
	resolvedStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(resolvedStateDir, "tuning_runs") {
		t.Fatalf("root = %q, want state-directory sibling", root)
	}
	if cache != filepath.Join(resolvedStateDir, "ohlcv_cache.sqlite3") {
		t.Fatalf("cache = %q, want state-directory sibling", cache)
	}
}

func TestNewTuningRunManagerFailsLoudlyForUnwritableCachePath(t *testing.T) {
	stateDir := t.TempDir()
	configPath := writeTuningTestConfig(t, stateDir)
	if err := os.Mkdir(filepath.Join(stateDir, "ohlcv_cache.sqlite3"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := newTuningRunManager(configPath, nil)
	if err == nil || !strings.Contains(err.Error(), "ohlcv_cache.sqlite3") {
		t.Fatalf("err = %v, want loud cache-path failure", err)
	}
}

func TestTuningRunManagerSerializesJobs(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	var active atomic.Int32
	var maxActive atomic.Int32
	runner := func(ctx context.Context, job tuningRunJob) error {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- job.ID
		select {
		case <-ctx.Done():
			active.Add(-1)
			return ctx.Err()
		case <-release:
			active.Add(-1)
			return nil
		}
	}
	mgr, err := newTuningRunManager(configPath, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.run(ctx)

	for _, id := range []string{"spot-a", "spot-b"} {
		if _, err := mgr.enqueue(tuningRunSpec{StrategyIDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	select {
	case id := <-started:
		t.Fatalf("second job %s started before the first completed", id)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued job did not start")
	}
	release <- struct{}{}
	if maxActive.Load() != 1 {
		t.Fatalf("max active = %d, want 1", maxActive.Load())
	}
	waitForTuningStatus(t, mgr, tuningRunCompleted, 2)
}

func TestTuningRunManagerMarksActiveRunInterruptedOnRestart(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "20260718T010203000000000Z-aabbccdd")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := tuningRunRecord{ID: filepath.Base(runDir), Status: tuningRunRunning, CreatedAt: time.Now().UTC()}
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunRecordFile), record); err != nil {
		t.Fatal(err)
	}

	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := mgr.record(record.ID)
	if !ok || got.Status != tuningRunInterrupted {
		t.Fatalf("record = %#v, ok=%v; want interrupted", got, ok)
	}
	if got.CompletedAt == nil {
		t.Fatal("interrupted run missing completion timestamp")
	}
}

func TestRunTuningProcessBypassesTradingSemaphoreAndSetsCacheEnv(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(mgr.rootDir, "test-run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	job := tuningRunJob{ID: "test-run", RunDir: runDir, ConfigPath: configPath,
		CacheDBPath: mgr.cacheDBPath, Spec: tuningRunSpec{
			StrategyIDs: []string{"spot-b", "spot-a"},
			Overrides: map[string]tuningStrategyOverrides{
				"spot-a": {Freeze: []string{"period"}},
			},
		}}
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunOverridesFile), job.Spec.Overrides); err != nil {
		t.Fatal(err)
	}

	oldSpawner := tuningProcessSpawner
	defer func() { tuningProcessSpawner = oldSpawner }()
	called := make(chan struct{}, 1)
	tuningProcessSpawner = func(ctx context.Context, script string, args []string, env map[string]string) ([]byte, []byte, error) {
		if script != "backtest/tune_live.py" {
			t.Errorf("script = %q", script)
		}
		if env[tuningCacheEnv] != mgr.cacheDBPath {
			t.Errorf("cache env = %q, want %q", env[tuningCacheEnv], mgr.cacheDBPath)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--strategy spot-b --strategy spot-a") {
			t.Errorf("args do not preserve selected strategy order: %q", joined)
		}
		if !strings.Contains(joined, "--overrides "+filepath.Join(runDir, tuningRunOverridesFile)) {
			t.Errorf("args do not carry persisted overrides: %q", joined)
		}
		if err := os.WriteFile(filepath.Join(runDir, tuningRunResultsFile), []byte(`{"schema_version":1,"strategies":[]}`), 0o600); err != nil {
			t.Error(err)
		}
		called <- struct{}{}
		return nil, nil, nil
	}

	for i := 0; i < cap(pythonSemaphore); i++ {
		pythonSemaphore <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(pythonSemaphore); i++ {
			<-pythonSemaphore
		}
	}()
	done := make(chan error, 1)
	go func() { done <- runTuningProcess(context.Background(), job) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tuning process blocked on the trading python semaphore")
	}
	select {
	case <-called:
	default:
		t.Fatal("dedicated process spawner was not called")
	}
}

func TestTuningPOSTRequiresAuthAndSameOrigin(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	mgr, err := newTuningRunManager(configPath, func(ctx context.Context, job tuningRunJob) error {
		if err := writeTuningJSON(filepath.Join(job.RunDir, tuningRunProgressFile), map[string]any{
			"phase": "tuning", "strategy": "spot-a",
		}); err != nil {
			return err
		}
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.run(ctx)
	state := NewAppState()
	var mu sync.RWMutex
	strategies := []StrategyConfig{{ID: "spot-a"}, {ID: "spot-b"}}
	ss := NewStatusServer(state, &mu, "secret", strategies, nil)
	ss.tuning = mgr

	request := func(origin, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/tuning/runs", strings.NewReader(`{"strategy_ids":["spot-a"]}`))
		r.Host = "localhost:8099"
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		ss.handleAPITuningRuns(w, r)
		return w
	}
	if w := request("", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}
	if w := request("https://evil.example", "secret"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
	before, _ := os.ReadFile(configPath)
	w := request("http://localhost:8099", "secret")
	if w.Code != http.StatusAccepted {
		t.Fatalf("authorized status = %d, body=%s", w.Code, w.Body.String())
	}
	var accepted tuningRunRecord
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(configPath)
	if string(after) != string(before) {
		t.Fatal("starting a tuning run mutated the config")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authorized run did not start")
	}
	detail, err := mgr.detail(accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.Status != tuningRunRunning || detail.Progress["phase"] != "tuning" {
		t.Fatalf("live run detail = %#v", detail)
	}
	close(release)
	waitForTuningStatus(t, mgr, tuningRunCompleted, 1)
}

func TestTuningGETSurvivesManagerRestart(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := mgr.enqueue(tuningRunSpec{StrategyIDs: []string{"spot-a"}})
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(mgr.rootDir, rec.ID)
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunProgressFile), map[string]any{"phase": "tuning"}); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunResultsFile), map[string]any{"schema_version": 1, "strategies": []any{}}); err != nil {
		t.Fatal(err)
	}
	rec.Status = tuningRunCompleted
	now := time.Now().UTC()
	rec.CompletedAt = &now
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunRecordFile), rec); err != nil {
		t.Fatal(err)
	}

	restarted, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := restarted.detail(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.Status != tuningRunCompleted || detail.Progress["phase"] != "tuning" {
		t.Fatalf("detail after restart = %#v", detail)
	}
	if detail.Results["schema_version"] != float64(1) {
		t.Fatalf("results after restart = %#v", detail.Results)
	}
	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "secret", nil, nil)
	ss.tuning = restarted
	for _, path := range []string{"/api/tuning/runs", "/api/tuning/runs/" + rec.ID} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		if path == "/api/tuning/runs" {
			ss.handleAPITuningRuns(w, r)
		} else {
			ss.handleAPITuningRun(w, r)
		}
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), rec.ID) {
			t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestTuningStartValidationRejectsUnknownOrUnselectedOverrides(t *testing.T) {
	strategies := []StrategyConfig{{ID: "spot-a"}, {ID: "spot-b"}}
	_, err := validateTuningStartRequest(tuningStartRequest{StrategyIDs: []string{"missing"}}, strategies)
	if err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Fatalf("unknown strategy err = %v", err)
	}
	_, err = validateTuningStartRequest(tuningStartRequest{
		StrategyIDs: []string{"spot-a"},
		Overrides:   map[string]tuningStrategyOverrides{"spot-b": {Freeze: []string{"period"}}},
	}, strategies)
	if err == nil || !strings.Contains(err.Error(), "not selected") {
		t.Fatalf("unselected override err = %v", err)
	}
}

func TestTuningRunProcessReportsInvalidArtifact(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(mgr.rootDir, "bad-artifact")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldSpawner := tuningProcessSpawner
	defer func() { tuningProcessSpawner = oldSpawner }()
	tuningProcessSpawner = func(context.Context, string, []string, map[string]string) ([]byte, []byte, error) {
		return nil, nil, nil
	}
	err = runTuningProcess(context.Background(), tuningRunJob{
		ID: "bad-artifact", RunDir: runDir, ConfigPath: configPath,
		CacheDBPath: mgr.cacheDBPath, Spec: tuningRunSpec{StrategyIDs: []string{"spot-a"}},
	})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want missing-results error", err)
	}
}

func TestTuningRunProcessReportsOrderedArtifactFailures(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(mgr.rootDir, "failed-artifact")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldSpawner := tuningProcessSpawner
	defer func() { tuningProcessSpawner = oldSpawner }()
	processErr := errors.New("exit status 1")
	tuningProcessSpawner = func(context.Context, string, []string, map[string]string) ([]byte, []byte, error) {
		results := map[string]any{
			"schema_version": 1,
			"strategies": []any{
				map[string]any{"strategy_id": "spot-a", "status": "stage1_failed", "error": "market data unavailable"},
				map[string]any{"strategy_id": "perps-b", "status": "unsupported", "reason": "unsupported risk sizing"},
				map[string]any{"strategy_id": "futures-c", "status": "stage2_failed"},
			},
		}
		if err := writeTuningJSON(filepath.Join(runDir, tuningRunResultsFile), results); err != nil {
			t.Error(err)
		}
		return nil, []byte("generic stderr should not hide artifact detail"), processErr
	}

	err = runTuningProcess(context.Background(), tuningRunJob{
		ID: "failed-artifact", RunDir: runDir, ConfigPath: configPath,
		CacheDBPath: mgr.cacheDBPath, Spec: tuningRunSpec{
			StrategyIDs: []string{"spot-a", "perps-b", "futures-c"},
		},
	})
	if !errors.Is(err, processErr) {
		t.Fatalf("err = %v, want wrapped process error", err)
	}
	got := err.Error()
	wants := []string{
		"spot-a [stage1_failed]: market data unavailable",
		"perps-b [unsupported]: unsupported risk sizing",
		"futures-c: stage2_failed",
	}
	last := -1
	for _, want := range wants {
		at := strings.Index(got, want)
		if at < 0 {
			t.Fatalf("err = %q, missing %q", got, want)
		}
		if at <= last {
			t.Fatalf("err diagnostics are not in artifact order: %q", got)
		}
		last = at
	}
	if strings.Contains(got, "generic stderr") {
		t.Fatalf("valid artifact diagnostics should take precedence over stderr: %q", got)
	}
}

func TestTuningRunProcessFallsBackToStderrWithoutArtifact(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(mgr.rootDir, "no-artifact")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldSpawner := tuningProcessSpawner
	defer func() { tuningProcessSpawner = oldSpawner }()
	processErr := errors.New("launch failed")
	tuningProcessSpawner = func(context.Context, string, []string, map[string]string) ([]byte, []byte, error) {
		return nil, []byte("python could not start\nmore detail"), processErr
	}

	err = runTuningProcess(context.Background(), tuningRunJob{
		ID: "no-artifact", RunDir: runDir, ConfigPath: configPath,
		CacheDBPath: mgr.cacheDBPath,
		Spec:        tuningRunSpec{StrategyIDs: []string{"spot-a"}},
	})
	if !errors.Is(err, processErr) || !strings.Contains(err.Error(), "stderr: python could not start") {
		t.Fatalf("err = %v, want wrapped process error with stderr fallback", err)
	}
	if strings.Contains(err.Error(), "more detail") {
		t.Fatalf("stderr fallback should remain first-line bounded: %q", err.Error())
	}
}

func TestLoadPersistedRunsSkipsMalformedDirectories(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	mgr, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	goodID := "20260718T000000000000000Z-aabbcc"
	goodRec := tuningRunRecord{
		ID:          goodID,
		Status:      tuningRunCompleted,
		StrategyIDs: []string{"spot-a"},
		CreatedAt:   time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
	}
	goodDir := filepath.Join(mgr.rootDir, goodID)
	if err := os.Mkdir(goodDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(goodDir, tuningRunRecordFile), goodRec); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(goodDir, tuningRunSpecFile), tuningRunSpec{
		SchemaVersion: 1, StrategyIDs: []string{"spot-a"},
	}); err != nil {
		t.Fatal(err)
	}

	// (a) enqueue crash window: directory exists with spec.json only.
	orphanID := "20260718T000001000000000Z-orphan1"
	orphanDir := filepath.Join(mgr.rootDir, orphanID)
	if err := os.Mkdir(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(orphanDir, tuningRunSpecFile), tuningRunSpec{
		SchemaVersion: 1, StrategyIDs: []string{"spot-a"},
	}); err != nil {
		t.Fatal(err)
	}

	// (b) truncated / syntactically corrupt run.json.
	corruptID := "20260718T000002000000000Z-corrupt"
	corruptDir := filepath.Join(mgr.rootDir, corruptID)
	if err := os.Mkdir(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, tuningRunRecordFile), []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}

	// (c) record ID disagrees with directory name.
	mismatchID := "20260718T000003000000000Z-mismatc"
	mismatchDir := filepath.Join(mgr.rootDir, mismatchID)
	if err := os.Mkdir(mismatchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(mismatchDir, tuningRunRecordFile), tuningRunRecord{
		ID:          "20260718T000003000000000Z-otherid",
		Status:      tuningRunCompleted,
		StrategyIDs: []string{"spot-a"},
		CreatedAt:   time.Date(2026, 7, 18, 0, 0, 3, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	// Invalid status should also be skipped without taking down recovery.
	badStatusID := "20260718T000004000000000Z-badstat"
	badStatusDir := filepath.Join(mgr.rootDir, badStatusID)
	if err := os.Mkdir(badStatusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badStatusDir, tuningRunRecordFile), []byte(
		`{"id":"20260718T000004000000000Z-badstat","status":"not-a-status","strategy_ids":["spot-a"],"created_at":"2026-07-18T00:00:04Z"}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := newTuningRunManager(configPath, nil)
	if err != nil {
		t.Fatalf("manager must start despite malformed dirs: %v", err)
	}
	listed := restarted.list()
	if len(listed) != 1 || listed[0].ID != goodID || listed[0].Status != tuningRunCompleted {
		t.Fatalf("listed = %#v, want only the valid completed run", listed)
	}
	detail, err := restarted.detail(goodID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.ID != goodID {
		t.Fatalf("detail = %#v", detail)
	}
	for _, badID := range []string{orphanID, corruptID, mismatchID, badStatusID} {
		if _, err := restarted.detail(badID); err == nil {
			t.Fatalf("expected missing detail for skipped run %s", badID)
		}
	}
}

func waitForTuningStatus(t *testing.T, mgr *tuningRunManager, status tuningRunStatus, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		matched := 0
		for _, rec := range mgr.list() {
			if rec.Status == status {
				matched++
			}
		}
		if matched == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runs did not reach status %q: %#v", status, mgr.list())
}
