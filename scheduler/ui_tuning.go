package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tuningRunSchemaVersion       = 1
	tuningRunQueueCap            = 16
	tuningCacheEnv               = "GO_TRADER_OHLCV_CACHE_DB"
	tuningRunRecordFile          = "run.json"
	tuningRunSpecFile            = "spec.json"
	tuningRunOverridesFile       = "overrides.json"
	tuningRunProgressFile        = "tune_live.progress.json"
	tuningRunResultsFile         = "results.json"
	tuningFailureDetailMaxRunes  = 240
	tuningFailureSummaryMaxRunes = 4096
)

type tuningRunStatus string

const (
	tuningRunQueued      tuningRunStatus = "queued"
	tuningRunRunning     tuningRunStatus = "running"
	tuningRunCompleted   tuningRunStatus = "completed"
	tuningRunFailed      tuningRunStatus = "failed"
	tuningRunInterrupted tuningRunStatus = "interrupted"
	tuningRunRejected    tuningRunStatus = "rejected"
)

var errTuningQueueFull = errors.New("tuning run queue is full")

type tuningStrategyOverrides struct {
	Params map[string][]json.RawMessage `json:"params,omitempty"`
	Freeze []string                     `json:"freeze,omitempty"`
}

type tuningStartRequest struct {
	StrategyIDs []string                           `json:"strategy_ids"`
	Overrides   map[string]tuningStrategyOverrides `json:"overrides,omitempty"`
}

type tuningRunSpec struct {
	SchemaVersion int                                `json:"schema_version"`
	StrategyIDs   []string                           `json:"strategy_ids"`
	Overrides     map[string]tuningStrategyOverrides `json:"overrides,omitempty"`
}

type tuningRunRecord struct {
	ID          string          `json:"id"`
	Status      tuningRunStatus `json:"status"`
	StrategyIDs []string        `json:"strategy_ids"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Error       string          `json:"error,omitempty"`
}

type tuningRunDetail struct {
	Run      tuningRunRecord `json:"run"`
	Spec     tuningRunSpec   `json:"spec"`
	Progress map[string]any  `json:"progress,omitempty"`
	Results  map[string]any  `json:"results,omitempty"`
}

type tuningRunJob struct {
	ID          string
	RunDir      string
	ConfigPath  string
	CacheDBPath string
	Spec        tuningRunSpec
}

type tuningRunRunner func(context.Context, tuningRunJob) error

type tuningRunManager struct {
	rootDir     string
	configPath  string
	cacheDBPath string
	jobs        chan tuningRunJob
	runner      tuningRunRunner
	now         func() time.Time

	mu   sync.RWMutex
	runs map[string]tuningRunRecord
}

func resolveTuningPaths(configPath string) (string, string, error) {
	if strings.TrimSpace(configPath) == "" {
		return "", "", errors.New("config path is not configured")
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve config path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("resolve config symlink %s: %w", abs, err)
	}
	stateDir := filepath.Dir(resolved)
	return filepath.Join(stateDir, "tuning_runs"),
		filepath.Join(stateDir, "ohlcv_cache.sqlite3"), nil
}

func newTuningRunManager(configPath string, runner tuningRunRunner) (*tuningRunManager, error) {
	rootDir, cacheDBPath, err := resolveTuningPaths(configPath)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	resolvedConfig, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve config symlink %s: %w", abs, err)
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tuning artifact directory %s: %w", rootDir, err)
	}
	rootInfo, err := os.Lstat(rootDir)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("tuning artifact path %s is not a real directory", rootDir)
	}
	if err := ensureTuningCacheWritable(cacheDBPath); err != nil {
		return nil, err
	}
	if runner == nil {
		runner = runTuningProcess
	}
	m := &tuningRunManager{
		rootDir:     rootDir,
		configPath:  resolvedConfig,
		cacheDBPath: cacheDBPath,
		jobs:        make(chan tuningRunJob, tuningRunQueueCap),
		runner:      runner,
		now:         func() time.Time { return time.Now().UTC() },
		runs:        make(map[string]tuningRunRecord),
	}
	if err := m.loadPersistedRuns(); err != nil {
		return nil, err
	}
	return m, nil
}

func ensureTuningCacheWritable(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("tuning OHLCV cache path %s is not a regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect tuning OHLCV cache path %s: %w", path, err)
	}
	// SQLite WAL mode needs to create -wal/-shm siblings. Probe the directory,
	// not only the DB inode, so a writable existing DB in a read-only directory
	// cannot pass startup and then fail on the first cache write.
	probe, err := os.CreateTemp(filepath.Dir(path), ".ohlcv-cache-write-*")
	if err != nil {
		return fmt.Errorf("tuning OHLCV cache directory %s is not writable: %w", filepath.Dir(path), err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close tuning OHLCV cache probe %s: %w", probePath, err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove tuning OHLCV cache probe %s: %w", probePath, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("tuning OHLCV cache path %s is not writable: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tuning OHLCV cache path %s: %w", path, err)
	}
	return nil
}

func (m *tuningRunManager) loadPersistedRuns() error {
	entries, err := os.ReadDir(m.rootDir)
	if err != nil {
		return fmt.Errorf("list tuning runs: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validTuningRunID(entry.Name()) {
			continue
		}
		path := filepath.Join(m.rootDir, entry.Name(), tuningRunRecordFile)
		var rec tuningRunRecord
		if err := readTuningJSON(path, &rec); err != nil {
			// Spec-only orphans (crash between Mkdir and run.json) and corrupt
			// records must not disable recovery of every other valid run.
			log.Printf("[tuning] skipping run directory %s: %v", entry.Name(), err)
			continue
		}
		if rec.ID != entry.Name() {
			log.Printf("[tuning] skipping run directory %s: record id %q does not match directory",
				entry.Name(), rec.ID)
			continue
		}
		if !validTuningRunStatus(rec.Status) {
			log.Printf("[tuning] skipping run directory %s: invalid status %q",
				entry.Name(), rec.Status)
			continue
		}
		if rec.Status == tuningRunQueued || rec.Status == tuningRunRunning {
			now := m.now()
			rec.Status = tuningRunInterrupted
			rec.CompletedAt = &now
			rec.Error = "scheduler restarted before the tuning run completed"
			if err := writeTuningJSON(path, rec); err != nil {
				// Keep the in-memory interrupted view so the API still serves
				// this run; the next restart will retry the persist.
				log.Printf("[tuning] mark run %s interrupted on disk failed: %v", rec.ID, err)
			}
		}
		m.runs[rec.ID] = rec
	}
	return nil
}

func (m *tuningRunManager) enqueue(spec tuningRunSpec) (tuningRunRecord, error) {
	if spec.SchemaVersion == 0 {
		spec.SchemaVersion = tuningRunSchemaVersion
	}
	id, err := newTuningRunID(m.now())
	if err != nil {
		return tuningRunRecord{}, err
	}
	runDir := filepath.Join(m.rootDir, id)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return tuningRunRecord{}, fmt.Errorf("create tuning run %s: %w", id, err)
	}
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunSpecFile), spec); err != nil {
		return tuningRunRecord{}, fmt.Errorf("persist tuning run spec: %w", err)
	}
	if len(spec.Overrides) > 0 {
		if err := writeTuningJSON(filepath.Join(runDir, tuningRunOverridesFile), spec.Overrides); err != nil {
			return tuningRunRecord{}, fmt.Errorf("persist tuning overrides: %w", err)
		}
	}
	rec := tuningRunRecord{
		ID:          id,
		Status:      tuningRunQueued,
		StrategyIDs: append([]string(nil), spec.StrategyIDs...),
		CreatedAt:   m.now(),
	}
	if err := writeTuningJSON(filepath.Join(runDir, tuningRunRecordFile), rec); err != nil {
		return tuningRunRecord{}, fmt.Errorf("persist tuning run: %w", err)
	}
	m.mu.Lock()
	m.runs[id] = rec
	m.mu.Unlock()
	job := tuningRunJob{ID: id, RunDir: runDir, ConfigPath: m.configPath,
		CacheDBPath: m.cacheDBPath, Spec: spec}
	select {
	case m.jobs <- job:
		return rec, nil
	default:
		now := m.now()
		rec.Status = tuningRunRejected
		rec.CompletedAt = &now
		rec.Error = errTuningQueueFull.Error()
		if updateErr := m.storeRecord(rec); updateErr != nil {
			return rec, fmt.Errorf("%w; persist rejection: %v", errTuningQueueFull, updateErr)
		}
		return rec, errTuningQueueFull
	}
}

func (m *tuningRunManager) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.jobs:
			m.process(ctx, job)
		}
	}
}

func (m *tuningRunManager) process(ctx context.Context, job tuningRunJob) {
	rec, ok := m.record(job.ID)
	if !ok {
		log.Printf("[tuning] queued run %s has no persisted record", job.ID)
		return
	}
	started := m.now()
	rec.Status = tuningRunRunning
	rec.StartedAt = &started
	if err := m.storeRecord(rec); err != nil {
		log.Printf("[tuning] %s: persist running status: %v", job.ID, err)
		return
	}
	err := m.runner(ctx, job)
	completed := m.now()
	rec.CompletedAt = &completed
	if ctx.Err() != nil {
		rec.Status = tuningRunInterrupted
		rec.Error = "tuning run interrupted by scheduler shutdown"
	} else if err != nil {
		rec.Status = tuningRunFailed
		rec.Error = err.Error()
	} else {
		rec.Status = tuningRunCompleted
		rec.Error = ""
	}
	if storeErr := m.storeRecord(rec); storeErr != nil {
		log.Printf("[tuning] %s: persist terminal status: %v", job.ID, storeErr)
	}
}

func (m *tuningRunManager) storeRecord(rec tuningRunRecord) error {
	path := filepath.Join(m.rootDir, rec.ID, tuningRunRecordFile)
	if err := writeTuningJSON(path, rec); err != nil {
		return err
	}
	m.mu.Lock()
	m.runs[rec.ID] = rec
	m.mu.Unlock()
	return nil
}

func (m *tuningRunManager) record(id string) (tuningRunRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.runs[id]
	return rec, ok
}

func (m *tuningRunManager) list() []tuningRunRecord {
	m.mu.RLock()
	runs := make([]tuningRunRecord, 0, len(m.runs))
	for _, rec := range m.runs {
		runs = append(runs, rec)
	}
	m.mu.RUnlock()
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
	return runs
}

func (m *tuningRunManager) detail(id string) (tuningRunDetail, error) {
	if !validTuningRunID(id) {
		return tuningRunDetail{}, os.ErrNotExist
	}
	rec, ok := m.record(id)
	if !ok {
		return tuningRunDetail{}, os.ErrNotExist
	}
	runDir := filepath.Join(m.rootDir, id)
	var spec tuningRunSpec
	if err := readTuningJSON(filepath.Join(runDir, tuningRunSpecFile), &spec); err != nil {
		return tuningRunDetail{}, fmt.Errorf("read tuning spec: %w", err)
	}
	detail := tuningRunDetail{Run: rec, Spec: spec}
	if err := readOptionalTuningMap(filepath.Join(runDir, tuningRunProgressFile), &detail.Progress); err != nil {
		return tuningRunDetail{}, fmt.Errorf("read tuning progress: %w", err)
	}
	if err := readOptionalTuningMap(filepath.Join(runDir, tuningRunResultsFile), &detail.Results); err != nil {
		return tuningRunDetail{}, fmt.Errorf("read tuning results: %w", err)
	}
	return detail, nil
}

func readOptionalTuningMap(path string, dst *map[string]any) error {
	err := readTuningJSON(path, dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func newTuningRunID(now time.Time) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate tuning run id: %w", err)
	}
	return now.UTC().Format("20060102T150405000000000Z") + "-" + hex.EncodeToString(random), nil
}

func validTuningRunID(id string) bool {
	if len(id) < 20 || len(id) > 80 || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validTuningRunStatus(status tuningRunStatus) bool {
	switch status {
	case tuningRunQueued, tuningRunRunning, tuningRunCompleted,
		tuningRunFailed, tuningRunInterrupted, tuningRunRejected:
		return true
	default:
		return false
	}
}

func writeTuningJSON(path string, value any) (retErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tuning-json-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func readTuningJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after JSON document")
	}
	return nil
}

var tuningProcessSpawner = func(ctx context.Context, script string, args []string, env map[string]string) ([]byte, []byte, error) {
	return spawnPythonProcessWithEnv(ctx, script, args, nil, 0, env)
}

func runTuningProcess(ctx context.Context, job tuningRunJob) error {
	args := []string{"--config", job.ConfigPath}
	for _, strategyID := range job.Spec.StrategyIDs {
		args = append(args, "--strategy", strategyID)
	}
	args = append(args, "--out-dir", job.RunDir,
		"--json", filepath.Join(job.RunDir, tuningRunResultsFile))
	if len(job.Spec.Overrides) > 0 {
		args = append(args, "--overrides", filepath.Join(job.RunDir, tuningRunOverridesFile))
	}
	_, stderr, processErr := tuningProcessSpawner(ctx, "backtest/tune_live.py", args,
		map[string]string{tuningCacheEnv: job.CacheDBPath})
	resultsPath := filepath.Join(job.RunDir, tuningRunResultsFile)
	results, resultsErr := readTuningResults(resultsPath)
	if processErr != nil {
		if resultsErr == nil {
			if summary := tuningFailureSummary(results); summary != "" {
				return fmt.Errorf("tune_live failed: %w (results: %s)", processErr, summary)
			}
		}
		return fmt.Errorf("tune_live failed: %w (stderr: %s)", processErr, firstLine(stderr))
	}
	if resultsErr != nil {
		return resultsErr
	}
	return nil
}

func readTuningResults(path string) (map[string]any, error) {
	var results map[string]any
	if err := readTuningJSON(path, &results); err != nil {
		return nil, fmt.Errorf("read tuning results %s: %w", path, err)
	}
	if _, ok := results["schema_version"]; !ok {
		return nil, fmt.Errorf("tuning results %s missing schema_version", path)
	}
	if _, ok := results["strategies"].([]any); !ok {
		return nil, fmt.Errorf("tuning results %s missing strategies array", path)
	}
	return results, nil
}

func tuningFailureSummary(results map[string]any) string {
	strategies, _ := results["strategies"].([]any)
	parts := make([]string, 0, len(strategies))
	usedRunes := 0
	for i, raw := range strategies {
		strategy, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := tuningSummaryString(strategy["strategy_id"])
		if id == "" {
			id = fmt.Sprintf("strategy[%d]", i)
		}
		status := tuningSummaryString(strategy["status"])
		detail := tuningSummaryString(strategy["error"])
		if detail == "" {
			detail = tuningSummaryString(strategy["reason"])
		}
		var part string
		switch {
		case status != "" && detail != "":
			part = fmt.Sprintf("%s [%s]: %s", id, status, detail)
		case detail != "":
			part = fmt.Sprintf("%s: %s", id, detail)
		case status != "":
			part = fmt.Sprintf("%s: %s", id, status)
		default:
			continue
		}
		separatorRunes := 0
		if len(parts) > 0 {
			separatorRunes = 2
		}
		if usedRunes+separatorRunes+len([]rune(part)) > tuningFailureSummaryMaxRunes {
			parts = append(parts, fmt.Sprintf("%d additional strategy result(s) omitted", len(strategies)-i))
			break
		}
		parts = append(parts, part)
		usedRunes += separatorRunes + len([]rune(part))
	}
	summary := strings.Join(parts, "; ")
	runes := []rune(summary)
	if len(runes) > tuningFailureSummaryMaxRunes {
		return string(runes[:tuningFailureSummaryMaxRunes-1]) + "…"
	}
	return summary
}

func tuningSummaryString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= tuningFailureDetailMaxRunes {
		return text
	}
	return string(runes[:tuningFailureDetailMaxRunes-1]) + "…"
}

func validateTuningStartRequest(req tuningStartRequest, strategies []StrategyConfig) (tuningRunSpec, error) {
	if len(req.StrategyIDs) == 0 {
		return tuningRunSpec{}, errors.New("strategy_ids must contain at least one strategy id")
	}
	known := make(map[string]struct{}, len(strategies))
	for _, sc := range strategies {
		known[sc.ID] = struct{}{}
	}
	selected := make(map[string]struct{}, len(req.StrategyIDs))
	strategyIDs := make([]string, 0, len(req.StrategyIDs))
	for _, rawID := range req.StrategyIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return tuningRunSpec{}, errors.New("strategy_ids must not contain blank ids")
		}
		if _, duplicate := selected[id]; duplicate {
			return tuningRunSpec{}, fmt.Errorf("duplicate strategy id %q", id)
		}
		if _, ok := known[id]; !ok {
			return tuningRunSpec{}, fmt.Errorf("unknown strategy %q", id)
		}
		selected[id] = struct{}{}
		strategyIDs = append(strategyIDs, id)
	}
	overridesCopy := make(map[string]tuningStrategyOverrides, len(req.Overrides))
	for id, overrides := range req.Overrides {
		if _, ok := selected[id]; !ok {
			return tuningRunSpec{}, fmt.Errorf("override strategy %q is not selected", id)
		}
		frozen := make(map[string]struct{}, len(overrides.Freeze))
		freezeCopy := make([]string, 0, len(overrides.Freeze))
		for _, rawName := range overrides.Freeze {
			name := strings.TrimSpace(rawName)
			if name == "" {
				return tuningRunSpec{}, fmt.Errorf("%s: freeze contains a blank parameter name", id)
			}
			if _, duplicate := frozen[name]; duplicate {
				return tuningRunSpec{}, fmt.Errorf("%s: duplicate frozen parameter %q", id, name)
			}
			frozen[name] = struct{}{}
			freezeCopy = append(freezeCopy, name)
		}
		paramsCopy := make(map[string][]json.RawMessage, len(overrides.Params))
		for rawName, values := range overrides.Params {
			name := strings.TrimSpace(rawName)
			if name == "" || len(values) == 0 {
				return tuningRunSpec{}, fmt.Errorf("%s: override parameters need a name and non-empty value list", id)
			}
			if _, duplicate := paramsCopy[name]; duplicate {
				return tuningRunSpec{}, fmt.Errorf("%s: duplicate override parameter %q", id, name)
			}
			if _, both := frozen[name]; both {
				return tuningRunSpec{}, fmt.Errorf("%s: parameter %q cannot be both overridden and frozen", id, name)
			}
			for _, value := range values {
				if len(value) == 0 || !json.Valid(value) {
					return tuningRunSpec{}, fmt.Errorf("%s: override parameter %q contains invalid JSON", id, name)
				}
			}
			paramsCopy[name] = append([]json.RawMessage(nil), values...)
		}
		overridesCopy[id] = tuningStrategyOverrides{Params: paramsCopy, Freeze: freezeCopy}
	}
	return tuningRunSpec{SchemaVersion: tuningRunSchemaVersion,
		StrategyIDs: strategyIDs, Overrides: overridesCopy}, nil
}

func (ss *StatusServer) tuningStrategySnapshot() []StrategyConfig {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	return append([]StrategyConfig(nil), ss.strategies...)
}

func (ss *StatusServer) handleAPITuningRuns(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !ss.requireAPIAuth(w, r) {
			return
		}
		if ss.tuning == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "tuning service unavailable")
			return
		}
		writeJSON(w, map[string]any{"runs": ss.tuning.list()})
	case http.MethodPost:
		if !ss.requireMutatingAPIAuth(w, r) || !requireJSONContentType(w, r) || !requireSameOrigin(w, r) {
			return
		}
		if ss.tuning == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "tuning service unavailable")
			return
		}
		var req tuningStartRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid json body: trailing data")
			return
		}
		spec, err := validateTuningStartRequest(req, ss.tuningStrategySnapshot())
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		rec, err := ss.tuning.enqueue(spec)
		if errors.Is(err, errTuningQueueFull) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "run": rec})
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(rec)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (ss *StatusServer) handleAPITuningRun(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if ss.tuning == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "tuning service unavailable")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/tuning/runs/")
	if id == "" || strings.Contains(id, "/") {
		writeJSONError(w, http.StatusNotFound, "tuning run not found")
		return
	}
	detail, err := ss.tuning.detail(id)
	if errors.Is(err, os.ErrNotExist) {
		writeJSONError(w, http.StatusNotFound, "tuning run not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, detail)
}
