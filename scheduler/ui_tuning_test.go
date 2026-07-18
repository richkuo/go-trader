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
	_, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, runner, 0)
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

	mgr, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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
	}, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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

	restarted, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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
	mgr, err := newTuningRunManager(configPath, nil, 0)
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

	restarted, err := newTuningRunManager(configPath, nil, 0)
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

func seedTuningTerminalRun(t *testing.T, root, id string, status tuningRunStatus, created time.Time) {
	t.Helper()
	if !tuningRunStatusIsTerminal(status) {
		t.Fatalf("seed status %q is not terminal", status)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := tuningRunRecord{
		ID:          id,
		Status:      status,
		StrategyIDs: []string{"spot-a"},
		CreatedAt:   created,
	}
	if err := writeTuningJSON(filepath.Join(dir, tuningRunRecordFile), rec); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(dir, tuningRunSpecFile), tuningRunSpec{
		SchemaVersion: 1, StrategyIDs: []string{"spot-a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(dir, tuningRunResultsFile), map[string]any{
		"schema_version": 1, "marker": id,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTuningRunManagerKeepAllDoesNotPrune(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	ids := []string{
		"20260701T000000000000000Z-keep0001",
		"20260701T010000000000000Z-keep0002",
		"20260701T020000000000000Z-keep0003",
	}
	statuses := []tuningRunStatus{tuningRunCompleted, tuningRunFailed, tuningRunInterrupted}
	for i, id := range ids {
		seedTuningTerminalRun(t, root, id, statuses[i], base.Add(time.Duration(i)*time.Hour))
	}
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := mgr.list()
	if len(listed) != 3 {
		t.Fatalf("keep-all listed %d runs, want 3", len(listed))
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(root, id, tuningRunRecordFile)); err != nil {
			t.Fatalf("keep-all deleted %s: %v", id, err)
		}
	}
}

func TestTuningRunManagerPrunesOldestTerminalOverCap(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	type seeded struct {
		id     string
		status tuningRunStatus
	}
	seeds := []seeded{
		{"20260702T000000000000000Z-oldcomp1", tuningRunCompleted},
		{"20260702T010000000000000Z-oldfail2", tuningRunFailed},
		{"20260702T020000000000000Z-oldintr3", tuningRunInterrupted},
		{"20260702T030000000000000Z-oldrej04", tuningRunRejected},
	}
	for i, s := range seeds {
		seedTuningTerminalRun(t, root, s.id, s.status, base.Add(time.Duration(i)*time.Hour))
	}

	// Spec-only orphan must stay skippable (never entered m.runs).
	orphanID := "20260702T050000000000000Z-orphan99"
	orphanDir := filepath.Join(root, orphanID)
	if err := os.Mkdir(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTuningJSON(filepath.Join(orphanDir, tuningRunSpecFile), tuningRunSpec{
		SchemaVersion: 1, StrategyIDs: []string{"spot-a"},
	}); err != nil {
		t.Fatal(err)
	}

	mgr, err := newTuningRunManager(configPath, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	listed := mgr.list()
	if len(listed) != 2 {
		t.Fatalf("listed %d runs after prune, want 2: %#v", len(listed), listed)
	}
	wantKeep := map[string]bool{
		"20260702T020000000000000Z-oldintr3": true,
		"20260702T030000000000000Z-oldrej04": true,
	}
	for _, rec := range listed {
		if !wantKeep[rec.ID] {
			t.Fatalf("unexpected retained run %s status=%s", rec.ID, rec.Status)
		}
	}
	for _, gone := range []string{
		"20260702T000000000000000Z-oldcomp1",
		"20260702T010000000000000Z-oldfail2",
	} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected pruned dir %s gone, err=%v", gone, err)
		}
		if _, ok := mgr.record(gone); ok {
			t.Fatalf("dangling in-memory entry for pruned %s", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(orphanDir, tuningRunSpecFile)); err != nil {
		t.Fatalf("orphan skippable dir was deleted: %v", err)
	}
}

func TestTuningRunManagerNeverPrunesQueuedOrRunning(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{
		"20260705T000000000000000Z-term0001",
		"20260705T010000000000000Z-term0002",
		"20260705T020000000000000Z-term0003",
	} {
		seedTuningTerminalRun(t, root, id, tuningRunCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Inject live in-flight records after load (startup would rewrite them to
	// interrupted). Cap=1 must still preserve both active entries.
	for _, rec := range []tuningRunRecord{
		{ID: "20260705T030000000000000Z-queued01", Status: tuningRunQueued,
			StrategyIDs: []string{"spot-a"}, CreatedAt: base.Add(3 * time.Hour)},
		{ID: "20260705T040000000000000Z-running1", Status: tuningRunRunning,
			StrategyIDs: []string{"spot-a"}, CreatedAt: base.Add(4 * time.Hour)},
	} {
		dir := filepath.Join(root, rec.ID)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeTuningJSON(filepath.Join(dir, tuningRunRecordFile), rec); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		mgr.runs[rec.ID] = rec
		mgr.mu.Unlock()
	}
	mgr.setMaxRetainedRuns(1)
	listed := mgr.list()
	ids := map[string]tuningRunStatus{}
	for _, rec := range listed {
		ids[rec.ID] = rec.Status
	}
	if ids["20260705T030000000000000Z-queued01"] != tuningRunQueued {
		t.Fatalf("queued pruned or rewritten: %#v", listed)
	}
	if ids["20260705T040000000000000Z-running1"] != tuningRunRunning {
		t.Fatalf("running pruned or rewritten: %#v", listed)
	}
	if ids["20260705T020000000000000Z-term0003"] != tuningRunCompleted {
		t.Fatalf("newest terminal missing: %#v", listed)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d, want 3 (2 active + 1 terminal): %#v", len(listed), listed)
	}
	for _, gone := range []string{
		"20260705T000000000000000Z-term0001",
		"20260705T010000000000000Z-term0002",
	} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected terminal %s pruned, err=%v", gone, err)
		}
	}
}

func TestTuningRunManagerSetMaxRetainedRunsHotReloadPrunes(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	ids := []string{
		"20260703T000000000000000Z-hot00001",
		"20260703T010000000000000Z-hot00002",
		"20260703T020000000000000Z-hot00003",
	}
	for i, id := range ids {
		seedTuningTerminalRun(t, root, id, tuningRunCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mgr.list()) != 3 {
		t.Fatalf("pre-reload listed %d, want 3", len(mgr.list()))
	}
	mgr.setMaxRetainedRuns(1)
	listed := mgr.list()
	if len(listed) != 1 || listed[0].ID != ids[2] {
		t.Fatalf("after hot-cap listed %#v, want only %s", listed, ids[2])
	}
	for _, gone := range ids[:2] {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s pruned after hot-reload cap, err=%v", gone, err)
		}
	}
}

func TestApplyHotReloadConfigAdoptsTuningRetention(t *testing.T) {
	configPath := writeTuningTestConfig(t, t.TempDir())
	root, _, err := resolveTuningPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	ids := []string{
		"20260704T000000000000000Z-sighup01",
		"20260704T010000000000000Z-sighup02",
	}
	for i, id := range ids {
		seedTuningTerminalRun(t, root, id, tuningRunCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	mgr, err := newTuningRunManager(configPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := NewAppState()
	ss := NewStatusServer(state, &sync.RWMutex{}, "", nil, nil)
	ss.tuning = mgr
	strat := StrategyConfig{
		ID: "spot-a", Type: "spot", Platform: "binanceus",
		Script:  "shared_scripts/check_strategy.py",
		Args:    []string{"sma_crossover", "BTC/USDT", "1h"},
		Capital: 1000, MaxDrawdownPct: 20, IntervalSeconds: 60,
	}
	cfg := &Config{IntervalSeconds: 60, DBFile: "scheduler/state.db", Strategies: []StrategyConfig{strat}}
	next := &Config{
		IntervalSeconds: 60,
		DBFile:          "scheduler/state.db",
		Strategies:      []StrategyConfig{strat},
		Tuning:          &TuningConfig{MaxRetainedRuns: 1},
	}
	changes, err := applyHotReloadConfig(cfg, next, state, nil, ss)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "tuning:") {
		t.Fatalf("expected tuning change, got %v", changes)
	}
	if cfg.tuningMaxRetainedRuns() != 1 {
		t.Fatalf("cfg retention = %d, want 1", cfg.tuningMaxRetainedRuns())
	}
	listed := mgr.list()
	if len(listed) != 1 || listed[0].ID != ids[1] {
		t.Fatalf("after SIGHUP prune listed %#v, want only %s", listed, ids[1])
	}
}

func TestValidateConfigRejectsNegativeTuningRetention(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		LogDir:          t.TempDir(),
		Strategies: []StrategyConfig{{
			ID: "spot-a", Type: "spot", Platform: "binanceus", Script: "check.py",
			Args:    []string{"sma_crossover", "BTC/USDT", "1h"},
			Capital: 1000, MaxDrawdownPct: 10,
		}},
		Tuning: &TuningConfig{MaxRetainedRuns: -1},
	}
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "tuning.max_retained_runs") {
		t.Fatalf("err = %v, want tuning.max_retained_runs rejection", err)
	}
}
