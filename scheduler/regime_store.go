package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const regimeCheckScript = "shared_scripts/check_regime.py"

const regimeBundleMinBars = 30

const (
	optionsRegimeTimeframe       = "4h"
	optionsRegimeOhlcvLimit      = 100
	optionsRegimeMinBars         = 30
	optionsRegimeWindowsSpecJSON = `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`
)

type regimeBundleKey struct {
	Platform  string
	Symbol    string
	Timeframe string
	SpecJSON  string
}

func (k regimeBundleKey) String() string {
	return k.Platform + "/" + k.Symbol + "/" + k.Timeframe
}

type regimeBundleRequest struct {
	Key               regimeBundleKey
	OhlcvLimit        int
	MinBars           int
	AllowSpotFallback bool
}

type RegimeBundleViews struct {
	ADX3       string `json:"adx3"`
	Composite7 string `json:"composite7"`
}

type RegimeBundle struct {
	Key           regimeBundleKey
	Payload       RegimePayload
	RawRegimeJSON string
	Views         map[string]RegimeBundleViews
	BarTime       string
	Err           string
	At            time.Time
}

type regimeBundleOutput struct {
	Platform  string                       `json:"platform"`
	Symbol    string                       `json:"symbol"`
	Timeframe string                       `json:"timeframe"`
	BarTime   string                       `json:"bar_time"`
	Regime    json.RawMessage              `json:"regime"`
	Views     map[string]RegimeBundleViews `json:"views"`
	Error     string                       `json:"error"`
}

var regimeStorePhaseBudget = scriptTimeout + 15*time.Second

type RegimeStore struct {
	mu      sync.RWMutex
	entries map[regimeBundleKey]*RegimeBundle
	builtAt time.Time
	sealed  bool
	gen     uint64
}

var globalRegimeStore = &RegimeStore{}

func (s *RegimeStore) resetForCycle(now time.Time) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[regimeBundleKey]*RegimeBundle)
	s.builtAt = now
	s.sealed = false
	s.gen++
	return s.gen
}

func (s *RegimeStore) set(b *RegimeBundle, gen uint64) {
	if b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || gen != s.gen {

		return
	}
	if s.entries == nil {
		s.entries = make(map[regimeBundleKey]*RegimeBundle)
	}
	s.entries[b.Key] = b
}

func (s *RegimeStore) seal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
	return len(s.entries)
}

func (s *RegimeStore) get(key regimeBundleKey) (*RegimeBundle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.entries[key]
	return b, ok
}

func (s *RegimeStore) Snapshot() ([]*RegimeBundle, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RegimeBundle, 0, len(s.entries))
	for _, b := range s.entries {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Timeframe != b.Timeframe {
			return a.Timeframe < b.Timeframe
		}
		return a.SpecJSON < b.SpecJSON
	})
	return out, s.builtAt
}

func strategyRegimeDataPlatform(sc StrategyConfig) string {
	switch sc.Type {
	case "spot":
		switch sc.Platform {
		case "okx":
			return "okx"
		case "robinhood":
			return "robinhood"
		default:

			return "binanceus"
		}
	case "perps":
		if sc.Platform == "okx" {
			return "okx"
		}
		return "hyperliquid"
	case "futures":
		return "topstep"
	case "manual":
		return "hyperliquid"
	case "options":
		return strings.TrimSpace(sc.Platform)
	}
	return ""
}

func strategyArgSymbolTimeframe(args []string) (string, string) {
	if len(args) < 3 {
		return "", ""
	}
	symbol := strings.TrimSpace(args[1])
	timeframe := strings.TrimSpace(args[2])
	if symbol == "" || timeframe == "" || strings.HasPrefix(symbol, "-") || strings.HasPrefix(timeframe, "-") {
		return "", ""
	}
	return symbol, timeframe
}

func strategyRegimeSymbolTimeframe(args []string, rc *RegimeConfig) (string, string) {
	symbol, timeframe := strategyArgSymbolTimeframe(args)
	if symbol == "" || timeframe == "" {
		return "", ""
	}
	if rc != nil {
		if tf := normalizeRegimeTimeframe(rc.Timeframe); tf != "" {
			timeframe = tf
		}
	}
	return symbol, timeframe
}

func strategyRegimeBundleRequest(sc StrategyConfig, rc *RegimeConfig) (regimeBundleRequest, bool) {
	if sc.Type == "options" {
		platform := strategyRegimeDataPlatform(sc)
		if platform == "" || len(sc.Args) < 2 {
			return regimeBundleRequest{}, false
		}
		underlying := strings.TrimSpace(sc.Args[1])
		if underlying == "" || strings.HasPrefix(underlying, "-") {
			return regimeBundleRequest{}, false
		}
		return regimeBundleRequest{
			Key: regimeBundleKey{
				Platform:  platform,
				Symbol:    strings.ToUpper(underlying),
				Timeframe: optionsRegimeTimeframe,
				SpecJSON:  optionsRegimeWindowsSpecJSON,
			},
			OhlcvLimit:        optionsRegimeOhlcvLimit,
			MinBars:           optionsRegimeMinBars,
			AllowSpotFallback: true,
		}, true
	}
	if rc == nil || !rc.Enabled {
		return regimeBundleRequest{}, false
	}
	specJSON := regimeWindowsSpecJSON(rc)
	if specJSON == "" {
		return regimeBundleRequest{}, false
	}
	platform := strategyRegimeDataPlatform(sc)
	if platform == "" {
		return regimeBundleRequest{}, false
	}
	symbol, timeframe := strategyRegimeSymbolTimeframe(sc.Args, rc)
	if symbol == "" || timeframe == "" {
		return regimeBundleRequest{}, false
	}
	return regimeBundleRequest{
		Key: regimeBundleKey{
			Platform:  platform,
			Symbol:    symbol,
			Timeframe: timeframe,
			SpecJSON:  specJSON,
		},
		OhlcvLimit: regimeRequiredOhlcvLimit(rc),
		MinBars:    regimeBundleMinBars,
	}, true
}

func collectRegimeBundleRequests(due []StrategyConfig, rc *RegimeConfig) []regimeBundleRequest {
	seen := make(map[regimeBundleKey]bool)
	out := make([]regimeBundleRequest, 0, len(due))
	for _, sc := range due {
		req, ok := strategyRegimeBundleRequest(sc, rc)
		if !ok || seen[req.Key] {
			continue
		}
		seen[req.Key] = true
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Timeframe != b.Timeframe {
			return a.Timeframe < b.Timeframe
		}
		return a.SpecJSON < b.SpecJSON
	})
	return out
}

func regimeBundleCheckArgs(req regimeBundleRequest) []string {
	args := []string{
		"--platform", req.Key.Platform,
		"--symbol", req.Key.Symbol,
		"--timeframe", req.Key.Timeframe,
		"--regime-windows-spec-json", req.Key.SpecJSON,
		"--ohlcv-limit", strconv.Itoa(req.OhlcvLimit),
		"--min-bars", strconv.Itoa(req.MinBars),
	}
	if req.AllowSpotFallback {
		args = append(args, "--allow-spot-fallback")
	}
	return args
}

func parseRegimeBundleOutput(key regimeBundleKey, data []byte, now time.Time) (*RegimeBundle, error) {
	var out regimeBundleOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("regime bundle %s: bad JSON: %w", key, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("regime bundle %s: %s", key, out.Error)
	}
	if len(out.Regime) == 0 || string(out.Regime) == "null" {
		return nil, fmt.Errorf("regime bundle %s: missing regime payload", key)
	}
	var payload RegimePayload
	if err := json.Unmarshal(out.Regime, &payload); err != nil {
		return nil, fmt.Errorf("regime bundle %s: bad regime payload: %w", key, err)
	}
	if payload.IsEmpty() {
		return nil, fmt.Errorf("regime bundle %s: empty regime payload", key)
	}
	return &RegimeBundle{
		Key:           key,
		Payload:       payload,
		RawRegimeJSON: string(out.Regime),
		Views:         out.Views,
		BarTime:       out.BarTime,
		At:            now,
	}, nil
}

var runRegimeBundleCheckFn = runRegimeBundleCheck

func runRegimeBundleCheck(ctx context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
	stdout, stderr, err := runPython(ctx, regimeCheckScript, regimeBundleCheckArgs(req), nil)
	now := time.Now().UTC()

	if bundle, perr := parseRegimeBundleOutput(req.Key, stdout, now); perr == nil {
		return bundle, nil
	} else if err == nil {
		return nil, perr
	}
	detail := strings.TrimSpace(string(stderr))
	if msg := regimeBundleErrorMessage(stdout); msg != "" {
		detail = msg
	}
	if detail == "" {
		detail = err.Error()
	}
	return nil, fmt.Errorf("regime bundle %s: %s", req.Key, detail)
}

func regimeBundleErrorMessage(stdout []byte) string {
	var out regimeBundleOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Error)
}

func regimeBundleAlertConfig(key regimeBundleKey) StrategyConfig {
	return StrategyConfig{
		ID:       "regime[" + key.String() + "]",
		Platform: key.Platform,
		Script:   regimeCheckScript,
	}
}

func regimeGateOutagePolicyNote(key regimeBundleKey, due []StrategyConfig, rc *RegimeConfig) string {
	var closed, open []string
	for _, sc := range due {
		if len(sc.AllowedRegimes) == 0 {
			continue
		}
		req, ok := strategyRegimeBundleRequest(sc, rc)
		if !ok || req.Key != key {
			continue
		}
		if resolveRegimeGateOnFailure(sc, rc) == RegimeGateOnFailureClosed {
			closed = append(closed, sc.ID)
		} else {
			open = append(open, sc.ID)
		}
	}
	if len(closed) == 0 && len(open) == 0 {
		return ""
	}
	sort.Strings(closed)
	sort.Strings(open)
	var parts []string
	if len(closed) > 0 {
		parts = append(parts, "fail-closed (opens held): "+strings.Join(closed, ", "))
	}
	if len(open) > 0 {
		parts = append(parts, "fail-open (entries ungated): "+strings.Join(open, ", "))
	}
	return "; entry gates — " + strings.Join(parts, "; ")
}

func startRegimeStorePopulation(store *RegimeStore, due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) func() {
	gen := store.resetForCycle(time.Now().UTC())
	reqs := collectRegimeBundleRequests(due, rc)
	if len(reqs) == 0 {
		return func() {}
	}

	popCtx, popCancel := context.WithCancel(shutdownReadOnlyCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		errs := make([]error, len(reqs))
		var wg sync.WaitGroup
		for i, req := range reqs {
			i, req := i, req
			wg.Add(1)
			go func() {
				defer wg.Done()
				bundle, err := runRegimeBundleCheckFn(popCtx, req)
				if err != nil {
					errs[i] = err
					return
				}
				store.set(bundle, gen)
			}()
		}
		wg.Wait()

		for i, req := range reqs {
			if errs[i] != nil {
				msg := errs[i].Error()
				if errors.Is(errs[i], context.Canceled) {
					msg = fmt.Sprintf("cancelled at phase-budget seal (%s); signature unavailable this cycle", regimeStorePhaseBudget)
				}

				msg += regimeGateOutagePolicyNote(req.Key, due, rc)
				fmt.Printf("[WARN] regime store %s: %s\n", req.Key, msg)
				notifyScriptFailure(notifier, regimeBundleAlertConfig(req.Key), scriptFailureError, msg)
			} else {
				clearScriptFailure(notifier, regimeBundleAlertConfig(req.Key))
			}
		}
	}()
	return func() {
		select {
		case <-done:
			popCancel()
		case <-time.After(regimeStorePhaseBudget):
			kept := store.seal()

			popCancel()
			fmt.Printf("[WARN] regime store: phase budget %s exceeded; sealed with %d/%d bundles — missing signatures resolve per regime_gate_on_failure this cycle (default fail-open)\n",
				regimeStorePhaseBudget, kept, len(reqs))
		}
		if summary := regimeStoreSummary(store); summary != "" {
			fmt.Printf("Regime: %s\n", summary)
		}
	}
}

func populateRegimeStore(store *RegimeStore, due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) {
	startRegimeStorePopulation(store, due, rc, notifier)()
}

func regimeStoreSummary(store *RegimeStore) string {
	bundles, _ := store.Snapshot()
	parts := make([]string, 0, len(bundles))
	for _, b := range bundles {
		label := b.Payload.PrimaryLabel(nil)
		if label == "" {
			label = "-"
		}
		parts = append(parts, b.Key.String()+"="+label)
	}
	return strings.Join(parts, "; ")
}

func (s *RegimeStore) PayloadForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimePayload {
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		return RegimePayload{}
	}
	b, found := s.get(req.Key)
	if !found || b == nil {
		return RegimePayload{}
	}
	return b.Payload
}

func (s *RegimeStore) BarTimeForStrategy(sc StrategyConfig, rc *RegimeConfig) string {
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		return ""
	}
	b, found := s.get(req.Key)
	if !found || b == nil {
		return ""
	}
	return b.BarTime
}

func (s *RegimeStore) InjectionJSONForStrategy(sc StrategyConfig, rc *RegimeConfig) (string, bool) {
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		return "", false
	}
	b, found := s.get(req.Key)
	if !found || b == nil {
		return "", true
	}
	return b.RawRegimeJSON, true
}
