package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Global per-cycle regime calculator (#879).
//
// Regime config is GLOBAL (cfg.Regime.{Period,ADXThreshold,Windows}); the only
// per-strategy regime fields are window *selectors* (gate/atr/directional →
// which global window to read), resolved at read time. So every strategy on the
// same (platform, symbol, interval) computes an identical multi-window regime.
//
// That collapses the two-layer "raw + per-signature label projection" sketch in
// the issue (built for per-strategy thresholds that don't exist here) into a
// single bundle per asset, computed ONCE per cycle by running the existing
// `prepare_check_regime` standalone (shared_scripts/check_regime.py). No new
// regime math, no Go-side label projection, no dual-implementation — the labels
// are byte-identical to the old inline path. The bundle holds the FULL
// multi-window payload; each strategy then projects its own selector windows out
// of it (RegimePayload.Label / the injected `prepare_check_regime`).
//
// Lifetime: in-memory, rebuilt every cycle (empty at loop start), never
// persisted — mirrors stratState.Regime. pos.Regime stays the frozen-at-open
// stamp and is unaffected by this store.

const (
	regimeCheckScript     = "shared_scripts/check_regime.py"
	optionsRegimeInterval = "4h"
	optionsRegimeOhlcvLim = 100
	optionsRegimeMinBars  = 30
)

// regimeBundle is one asset's regime for the current cycle.
type regimeBundle struct {
	payload RegimePayload
	barTime float64
	ok      bool // false ⇒ subprocess failed → consumers fall back to empty (fail open)
}

// RegimeStore is the per-cycle, in-memory two-layer store. Keyed by
// regimeAssetKeyString(platform, symbol, interval). Built before the per-strategy
// loop and read (read-only) during it and by the dashboard, so reads are guarded.
type RegimeStore struct {
	mu      sync.RWMutex
	bundles map[string]*regimeBundle
	rc      *RegimeConfig
}

func newRegimeStore(rc *RegimeConfig) *RegimeStore {
	return &RegimeStore{bundles: make(map[string]*regimeBundle), rc: rc}
}

func strategyArg(sc StrategyConfig, i int) string {
	if i >= 0 && i < len(sc.Args) {
		return strings.TrimSpace(sc.Args[i])
	}
	return ""
}

// regimeAssetKey derives the (platform, symbol, interval) a strategy's regime is
// computed on. ok=false when it can't be determined (no timeframe arg, etc.) —
// such a strategy simply has no bundle and falls back to empty regime.
func regimeAssetKey(sc StrategyConfig) (platform, symbol, interval string, ok bool) {
	platform = strings.TrimSpace(sc.Platform)
	switch sc.Type {
	case "options":
		symbol = strings.ToUpper(strategyArg(sc, 1))
		interval = optionsRegimeInterval
	case "manual":
		symbol = strings.TrimSpace(sc.Symbol)
		interval = strings.TrimSpace(sc.Timeframe)
		if symbol == "" {
			symbol = strategyArg(sc, 1)
		}
		if interval == "" {
			interval = strategyArg(sc, 2)
		}
	default:
		symbol = strategyArg(sc, 1)
		interval = strategyArg(sc, 2)
	}
	symbol = strings.TrimSpace(symbol)
	interval = strings.TrimSpace(interval)
	// A timeframe arg starting with "--" means the strategy has no positional
	// timeframe (mirrors extractTimeframe's guard) — regime not applicable.
	if platform == "" || symbol == "" || interval == "" || strings.HasPrefix(interval, "--") {
		return "", "", "", false
	}
	return platform, symbol, interval, true
}

func regimeAssetKeyString(platform, symbol, interval string) string {
	return platform + "|" + symbol + "|" + interval
}

// regimeStoreCoversStrategy reports whether Go owns regime for sc this cycle:
// non-options require regime.enabled; options always (independent 4h ADX).
func regimeStoreCoversStrategy(sc StrategyConfig, rc *RegimeConfig) bool {
	if sc.Type == "options" {
		return true
	}
	return rc != nil && rc.Enabled
}

func (s *RegimeStore) get(key string) (*regimeBundle, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bundles[key]
	return b, ok
}

func (s *RegimeStore) set(key string, b *regimeBundle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles[key] = b
}

// payloadFor returns the bundle payload for sc's asset, or nil when the bundle is
// missing or failed — consumers then fall back to the empty case (#879 failure
// policy: clear→empty→fail open). Returns a copy so callers can take its address.
func (s *RegimeStore) payloadFor(sc StrategyConfig) *RegimePayload {
	if s == nil {
		return nil
	}
	platform, symbol, interval, ok := regimeAssetKey(sc)
	if !ok {
		return nil
	}
	b, found := s.get(regimeAssetKeyString(platform, symbol, interval))
	if !found || b == nil || !b.ok {
		return nil
	}
	p := b.payload
	return &p
}

// primaryLabelFor returns sc's asset's display label (status/dashboard/options).
func (s *RegimeStore) primaryLabelFor(sc StrategyConfig) string {
	p := s.payloadFor(sc)
	if p == nil {
		return ""
	}
	return p.PrimaryLabel(s.rcConfig())
}

func (s *RegimeStore) rcConfig() *RegimeConfig {
	if s == nil {
		return nil
	}
	return s.rc
}

// injectionArgs returns the check-script flags carrying the precomputed regime
// (#879 Phase 2). When Go owns regime for sc, ALWAYS injects — even on a store
// miss/failure (empty payload) — so the check uses empty regime internally and
// fails open, matching Go-side consumers (no fail-open-vs-skip divergence).
func (s *RegimeStore) injectionArgs(sc StrategyConfig) []string {
	if s == nil || !regimeStoreCoversStrategy(sc, s.rcConfig()) {
		return nil
	}
	platform, symbol, interval, ok := regimeAssetKey(sc)
	if !ok {
		return nil
	}
	jsonStr := ""
	if b, found := s.get(regimeAssetKeyString(platform, symbol, interval)); found && b != nil && b.ok {
		if blob, err := json.Marshal(b.payload); err == nil {
			jsonStr = string(blob)
		}
	}
	return []string{"--regime-injected", "--regime-injected-json", jsonStr}
}

// regimeStoreSnapshot is a sorted, immutable view for operator/dashboard display.
type regimeStoreSnapshot struct {
	Assets []RegimeAssetView `json:"assets"`
}

// RegimeAssetView is one asset's portfolio-level regime for the dashboard (#879 scope 5).
type RegimeAssetView struct {
	Platform string `json:"platform"`
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	Regime   string `json:"regime"`
	OK       bool   `json:"ok"`
}

func (s *RegimeStore) snapshot() regimeStoreSnapshot {
	if s == nil {
		return regimeStoreSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.bundles))
	for k := range s.bundles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := regimeStoreSnapshot{Assets: make([]RegimeAssetView, 0, len(keys))}
	for _, k := range keys {
		b := s.bundles[k]
		parts := strings.SplitN(k, "|", 3)
		view := RegimeAssetView{}
		if len(parts) == 3 {
			view.Platform, view.Symbol, view.Interval = parts[0], parts[1], parts[2]
		}
		if b != nil {
			view.OK = b.ok
			if b.ok {
				view.Regime = b.payload.PrimaryLabel(s.rc)
			}
		}
		out.Assets = append(out.Assets, view)
	}
	return out
}

// regimeSubprocessResult is the JSON contract emitted by check_regime.py.
type regimeSubprocessResult struct {
	OK         bool           `json:"ok"`
	Regime     *RegimePayload `json:"regime"`
	LiveRegime string         `json:"live_regime"`
	BarTime    float64        `json:"bar_time"`
	Error      string         `json:"error"`
}

// parseRegimeSubprocessOutput parses check_regime.py stdout into a payload. Pure /
// subprocess-free so it is unit-testable. Returns an error on bad JSON or ok:false.
func parseRegimeSubprocessOutput(stdout []byte) (RegimePayload, float64, error) {
	var res regimeSubprocessResult
	if err := json.Unmarshal(stdout, &res); err != nil {
		return RegimePayload{}, 0, fmt.Errorf("parse regime output: %w", err)
	}
	if !res.OK {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "regime subprocess reported failure"
		}
		return RegimePayload{}, 0, fmt.Errorf("%s", msg)
	}
	return regimePayloadValue(res.Regime), res.BarTime, nil
}

// regimeCheckArgv builds the read-only check_regime.py argv for one asset. The
// --ohlcv-limit mirrors the per-strategy check so the #839 HL OHLCV cache is shared.
func regimeCheckArgv(platform, symbol, interval string, isOptions bool, rc *RegimeConfig) []string {
	argv := []string{symbol, interval, "--platform", platform}
	if isOptions {
		argv = append(argv,
			"--ohlcv-limit", strconv.Itoa(optionsRegimeOhlcvLim),
			"--min-bars", strconv.Itoa(optionsRegimeMinBars),
		)
		return argv
	}
	if blob := regimeWindowsSpecJSON(rc); blob != "" {
		argv = append(argv, "--regime-windows-spec-json", blob)
	}
	argv = append(argv, "--ohlcv-limit", strconv.Itoa(regimeRequiredOhlcvLimit(rc)))
	return argv
}

// regimeAssetJob is one distinct asset to compute this cycle.
type regimeAssetJob struct {
	key      string
	platform string
	argv     []string
}

// collectRegimeAssetJobs returns the distinct assets to compute for due strategies.
func collectRegimeAssetJobs(due []StrategyConfig, rc *RegimeConfig) []regimeAssetJob {
	seen := make(map[string]bool)
	var jobs []regimeAssetJob
	for _, sc := range due {
		if !regimeStoreCoversStrategy(sc, rc) {
			continue
		}
		platform, symbol, interval, ok := regimeAssetKey(sc)
		if !ok {
			continue
		}
		key := regimeAssetKeyString(platform, symbol, interval)
		if seen[key] {
			continue
		}
		seen[key] = true
		jobs = append(jobs, regimeAssetJob{
			key:      key,
			platform: platform,
			argv:     regimeCheckArgv(platform, symbol, interval, sc.Type == "options", rc),
		})
	}
	return jobs
}

// buildRegimeStore computes one regime bundle per distinct asset of the due
// strategies, via a dedicated read-only check_regime.py per asset. Subprocesses
// run concurrently (runPythonReadOnly gates each at pythonSemaphore). A failed
// asset gets an empty bundle and a throttled operator alert; everything else
// falls back to empty regime → fail open. Returns immediately when nothing is due.
func buildRegimeStore(due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) *RegimeStore {
	store := newRegimeStore(rc)
	jobs := collectRegimeAssetJobs(due, rc)
	if len(jobs) == 0 {
		return store
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job regimeAssetJob) {
			defer wg.Done()
			syntheticSC := StrategyConfig{ID: "regime:" + job.key, Platform: job.platform, Script: regimeCheckScript}
			stdout, _, runErr := runPythonReadOnly(regimeCheckScript, job.argv)
			payload, barTime, perr := parseRegimeSubprocessOutput(stdout)
			if perr != nil {
				store.set(job.key, &regimeBundle{ok: false})
				mode := scriptFailureError
				msg := perr.Error()
				if len(bytes.TrimSpace(stdout)) == 0 {
					mode = scriptFailureCrash
					if runErr != nil {
						msg = runErr.Error()
					}
				}
				notifyScriptFailure(notifier, syntheticSC, mode, msg)
				return
			}
			store.set(job.key, &regimeBundle{payload: payload, barTime: barTime, ok: true})
			clearScriptFailure(notifier, syntheticSC)
		}(job)
	}
	wg.Wait()
	return store
}
