package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	regimeBundleScript         = "shared_tools/regime.py"
	optionsRegimeTimeframe     = "4h"
	optionsRegimePeriod        = 14
	optionsRegimeADXThreshold  = 20.0
	optionsRegimeOHLCVLimit    = 100
	regimeRawSourceOptionsKind = "options"
)

// RegimeRawKey identifies the expensive market-data + indicator pass. Source
// is intentionally included so peer strategies share math only when their
// candle source is the same.
type RegimeRawKey struct {
	Source   string
	Platform string
	Kind     string
	InstType string
	Symbol   string
	Interval string
	Period   int
}

func (k RegimeRawKey) trackerID() string {
	return "regime:" + k.Source + ":" + strings.ToUpper(k.Symbol) + ":" + k.Interval + ":" + strconv.Itoa(k.Period)
}

func (k RegimeRawKey) logLabel() string {
	parts := []string{k.Source, strings.ToUpper(k.Symbol), k.Interval, strconv.Itoa(k.Period)}
	return strings.Join(parts, "/")
}

type RegimeSignature struct {
	RawKey       RegimeRawKey
	Classifier   string
	ADXThreshold float64
	Thresholds   RegimeCompositeThresholds
}

type regimeWindowRequest struct {
	StrategyID string
	Window     string
	RawKey     RegimeRawKey
	Spec       RegimeWindowSpec
	Signature  RegimeSignature
}

type regimeStrategyRequest struct {
	Strategy StrategyConfig
	Windows  []regimeWindowRequest
	Options  bool
}

type regimeRawMetrics struct {
	ADX          float64 `json:"adx"`
	PlusDI       float64 `json:"plus_di"`
	MinusDI      float64 `json:"minus_di"`
	CompositeADX float64 `json:"composite_adx"`
	ReturnEff    float64 `json:"return_eff"`
	RangeEff     float64 `json:"range_eff"`
	Efficiency   float64 `json:"efficiency"`
	ATRPct       float64 `json:"atr_pct"`
}

type regimeRawBundle struct {
	BarTime string            `json:"bar_time,omitempty"`
	Raw     regimeRawMetrics  `json:"raw"`
	Labels  map[string]string `json:"labels,omitempty"`
}

type RegimeCycleStore struct {
	raw      map[RegimeRawKey]regimeRawBundle
	labels   map[RegimeSignature]RegimeSnapshot
	payloads map[string]RegimePayload
	failed   map[RegimeRawKey]string
}

type regimeCycleStats struct {
	RawRequested int
	RawComputed  int
	Labels       int
	Failures     int
	Elapsed      time.Duration
}

var runRegimeBundleComputeFn = runRegimeBundleCompute

func newRegimeCycleStore() *RegimeCycleStore {
	return &RegimeCycleStore{
		raw:      make(map[RegimeRawKey]regimeRawBundle),
		labels:   make(map[RegimeSignature]RegimeSnapshot),
		payloads: make(map[string]RegimePayload),
		failed:   make(map[RegimeRawKey]string),
	}
}

func (s *RegimeCycleStore) PayloadForStrategy(sc StrategyConfig) RegimePayload {
	if s == nil {
		return RegimePayload{}
	}
	return s.payloads[sc.ID]
}

func buildCycleRegimeStore(due []StrategyConfig, cfg *Config, notifier *MultiNotifier) (*RegimeCycleStore, regimeCycleStats) {
	start := time.Now()
	store := newRegimeCycleStore()
	requests := collectRegimeCycleRequests(due, cfg)
	rawKeys := make(map[RegimeRawKey]bool)
	for _, req := range requests {
		for _, win := range req.Windows {
			rawKeys[win.RawKey] = true
		}
	}
	stats := regimeCycleStats{RawRequested: len(rawKeys)}
	if len(requests) == 0 {
		stats.Elapsed = time.Since(start)
		return store, stats
	}
	keys := make([]RegimeRawKey, 0, len(rawKeys))
	for key := range rawKeys {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].logLabel() < keys[j].logLabel()
	})
	for _, key := range keys {
		raw, err := runRegimeBundleComputeFn(key, regimeRequiredOhlcvLimitForRawKey(key))
		if err != nil {
			store.failed[key] = err.Error()
			stats.Failures++
			fmt.Printf("[WARN] regime bundle %s failed: %v\n", key.logLabel(), err)
			notifyRegimeScriptFailure(notifier, key, err.Error())
			continue
		}
		store.raw[key] = raw
		stats.RawComputed++
		clearRegimeScriptFailure(notifier, key)
	}
	for _, req := range requests {
		ready := true
		for _, win := range req.Windows {
			if _, ok := store.raw[win.RawKey]; !ok {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		windows := make(map[string]RegimeSnapshot, len(req.Windows))
		for _, win := range req.Windows {
			raw := store.raw[win.RawKey]
			snap := store.labels[win.Signature]
			if strings.TrimSpace(snap.Regime) == "" {
				snap = projectRegimeSnapshot(raw, win.Spec)
				store.labels[win.Signature] = snap
				stats.Labels++
			}
			windows[win.Window] = snap
		}
		if len(windows) > 0 {
			store.payloads[req.Strategy.ID] = RegimePayload{Windows: windows, MultiMode: true}
		}
	}
	stats.Elapsed = time.Since(start)
	return store, stats
}

func collectRegimeCycleRequests(due []StrategyConfig, cfg *Config) []regimeStrategyRequest {
	out := make([]regimeStrategyRequest, 0, len(due))
	for _, sc := range due {
		if sc.Type == "options" {
			if req, ok := collectOptionsRegimeRequest(sc); ok {
				out = append(out, req)
			}
			continue
		}
		if cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
			continue
		}
		if req, ok := collectStrategyRegimeRequest(sc, cfg.Regime); ok {
			out = append(out, req)
		}
	}
	return out
}

func collectOptionsRegimeRequest(sc StrategyConfig) (regimeStrategyRequest, bool) {
	symbol := strings.TrimSpace(extractAsset(sc))
	if symbol == "" {
		return regimeStrategyRequest{}, false
	}
	spec := RegimeWindowSpec{
		Classifier:   regimeClassifierADX,
		Period:       optionsRegimePeriod,
		ADXThreshold: optionsRegimeADXThreshold,
	}
	key := RegimeRawKey{
		Source:   regimeSource(sc.Platform, regimeRawSourceOptionsKind, ""),
		Platform: strings.TrimSpace(sc.Platform),
		Kind:     regimeRawSourceOptionsKind,
		Symbol:   symbol,
		Interval: optionsRegimeTimeframe,
		Period:   optionsRegimePeriod,
	}
	win := regimeWindowRequest{
		StrategyID: sc.ID,
		Window:     regimeWindowDefaultKey,
		RawKey:     key,
		Spec:       spec,
		Signature:  regimeSignatureFor(key, spec),
	}
	return regimeStrategyRequest{Strategy: sc, Windows: []regimeWindowRequest{win}, Options: true}, true
}

func collectStrategyRegimeRequest(sc StrategyConfig, rc *RegimeConfig) (regimeStrategyRequest, bool) {
	market, ok := regimeMarketForStrategy(sc)
	if !ok {
		return regimeStrategyRequest{}, false
	}
	specs := resolvedRegimeWindowSpecs(rc)
	if len(specs) == 0 {
		return regimeStrategyRequest{}, false
	}
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	windows := make([]regimeWindowRequest, 0, len(names))
	for _, name := range names {
		spec := specs[name]
		key := RegimeRawKey{
			Source:   market.Source,
			Platform: market.Platform,
			Kind:     market.Kind,
			InstType: market.InstType,
			Symbol:   market.Symbol,
			Interval: market.Interval,
			Period:   spec.Period,
		}
		windows = append(windows, regimeWindowRequest{
			StrategyID: sc.ID,
			Window:     name,
			RawKey:     key,
			Spec:       spec,
			Signature:  regimeSignatureFor(key, spec),
		})
	}
	return regimeStrategyRequest{Strategy: sc, Windows: windows}, true
}

type regimeMarket struct {
	Source   string
	Platform string
	Kind     string
	InstType string
	Symbol   string
	Interval string
}

func regimeMarketForStrategy(sc StrategyConfig) (regimeMarket, bool) {
	platform := strings.TrimSpace(sc.Platform)
	kind := strings.TrimSpace(sc.Type)
	interval := extractRegimeInterval(sc)
	symbol := ""
	instType := ""
	switch sc.Type {
	case "spot":
		if sc.Platform == "okx" {
			symbol = okxSymbol(sc.Args)
			instType = okxInstType(sc.Args)
		} else if sc.Platform == "robinhood" {
			symbol = robinhoodSymbol(sc.Args)
		} else {
			symbol = spotSymbol(sc.Args)
		}
	case "perps":
		if sc.Platform == "hyperliquid" {
			symbol = hyperliquidSymbol(sc.Args)
		} else if sc.Platform == "okx" {
			symbol = okxSymbol(sc.Args)
			instType = okxInstType(sc.Args)
		}
	case "futures":
		symbol = topstepSymbol(sc.Args)
	case "manual":
		platform = "hyperliquid"
		kind = "perps"
		symbol = strings.TrimSpace(sc.Symbol)
		if symbol == "" {
			symbol = hyperliquidSymbol(sc.Args)
		}
	default:
		return regimeMarket{}, false
	}
	if strings.TrimSpace(symbol) == "" || strings.TrimSpace(interval) == "" || interval == "—" {
		return regimeMarket{}, false
	}
	return regimeMarket{
		Source:   regimeSource(platform, kind, instType),
		Platform: platform,
		Kind:     kind,
		InstType: instType,
		Symbol:   symbol,
		Interval: interval,
	}, true
}

func extractRegimeInterval(sc StrategyConfig) string {
	if sc.Type == "manual" && len(sc.Args) < 3 {
		return "1h"
	}
	if len(sc.Args) > 2 && !strings.HasPrefix(sc.Args[2], "--") {
		return sc.Args[2]
	}
	return ""
}

func regimeSource(platform, kind, instType string) string {
	p := strings.TrimSpace(platform)
	k := strings.TrimSpace(kind)
	if p == "" {
		p = "binanceus"
	}
	if k == "" {
		k = "spot"
	}
	source := p + ":" + k
	if strings.TrimSpace(instType) != "" {
		source += ":" + strings.TrimSpace(instType)
	}
	return source
}

func resolvedRegimeWindowSpecs(rc *RegimeConfig) map[string]RegimeWindowSpec {
	if rc == nil || !rc.Enabled {
		return nil
	}
	out := make(map[string]RegimeWindowSpec)
	if len(rc.Windows) > 0 {
		for name, spec := range rc.Windows {
			key := normalizeRegimeWindowKey(name)
			if key == "" {
				continue
			}
			out[key] = spec.resolvedForEmit(rc)
		}
		return out
	}
	period := rc.Period
	if period <= 0 {
		period = 14
	}
	out[regimeWindowDefaultKey] = RegimeWindowSpec{
		Classifier:   regimeClassifierADX,
		Period:       period,
		ADXThreshold: rc.ADXThreshold,
	}.resolvedForEmit(rc)
	return out
}

func regimeSignatureFor(key RegimeRawKey, spec RegimeWindowSpec) RegimeSignature {
	resolved := spec.resolvedForEmit(nil)
	sig := RegimeSignature{
		RawKey:     key,
		Classifier: resolved.effectiveClassifier(),
	}
	if sig.Classifier == regimeClassifierComposite {
		sig.Thresholds = resolved.compositeThresholds()
	} else {
		sig.ADXThreshold = resolved.ADXThreshold
		if sig.ADXThreshold <= 0 {
			sig.ADXThreshold = 20.0
		}
	}
	return sig
}

func regimeRequiredOhlcvLimitForRawKey(key RegimeRawKey) int {
	if key.Kind == regimeRawSourceOptionsKind {
		return optionsRegimeOHLCVLimit
	}
	period := key.Period
	if period <= 0 {
		period = 14
	}
	warmup := 2*period - 1
	if period < 14 {
		warmup = 2*14 - 1
	}
	limit := warmup + regimeOhlcvMargin
	if limit < regimeOhlcvBaseLimit {
		limit = regimeOhlcvBaseLimit
	}
	return limit
}

func runRegimeBundleCompute(key RegimeRawKey, limit int) (regimeRawBundle, error) {
	args := []string{
		"--bundle",
		"--platform", key.Platform,
		"--type", key.Kind,
		"--symbol", key.Symbol,
		"--timeframe", key.Interval,
		"--period", strconv.Itoa(key.Period),
		"--limit", strconv.Itoa(limit),
	}
	if key.InstType != "" {
		args = append(args, "--inst-type", key.InstType)
	}
	stdout, stderr, err := runPythonReadOnly(regimeBundleScript, args)
	if err != nil {
		return regimeRawBundle{}, fmt.Errorf("script error: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	var result regimeRawBundle
	if err := json.Unmarshal(stdout, &result); err != nil {
		return regimeRawBundle{}, fmt.Errorf("parse output: %w (stdout: %s)", err, strings.TrimSpace(string(stdout)))
	}
	return result, nil
}

func projectRegimeSnapshot(bundle regimeRawBundle, spec RegimeWindowSpec) RegimeSnapshot {
	resolved := spec.resolvedForEmit(nil)
	raw := bundle.Raw
	if resolved.effectiveClassifier() == regimeClassifierComposite {
		th := resolved.compositeThresholds()
		label := mapCompositeRegimeLabel(raw.ReturnEff, raw.CompositeADX, raw.RangeEff, raw.Efficiency, th)
		return RegimeSnapshot{
			Regime: label,
			Score:  clamp01(raw.CompositeADX / 100.0),
			Metrics: map[string]float64{
				"adx":        raw.CompositeADX,
				"return_eff": raw.ReturnEff,
				"range_eff":  raw.RangeEff,
				"efficiency": raw.Efficiency,
				"atr_pct":    raw.ATRPct,
			},
		}
	}
	th := resolved.ADXThreshold
	if th <= 0 {
		th = 20.0
	}
	label := mapADXRegimeLabel(raw.ADX, raw.PlusDI, raw.MinusDI, th)
	return RegimeSnapshot{
		Regime: label,
		Score:  clamp01(raw.ADX / 100.0),
		Metrics: map[string]float64{
			"adx":      raw.ADX,
			"plus_di":  raw.PlusDI,
			"minus_di": raw.MinusDI,
			"atr_pct":  raw.ATRPct,
		},
	}
}

// Keep in sync with shared_tools/regime.py:map_adx_label.
func mapADXRegimeLabel(adx, plusDI, minusDI, threshold float64) string {
	if adx < threshold {
		return "ranging"
	}
	if plusDI > minusDI {
		return "trending_up"
	}
	if minusDI > plusDI {
		return "trending_down"
	}
	return "ranging"
}

// Keep in sync with shared_tools/regime.py:map_composite_label.
func mapCompositeRegimeLabel(returnEff, adxVal, rangeEff, efficiency float64, th RegimeCompositeThresholds) string {
	t := th.withDefaults()
	bigMove := math.Abs(returnEff) >= t.ReturnPct
	up := returnEff > 0
	highADX := adxVal >= t.ADX
	wide := rangeEff >= t.RangePct
	clean := efficiency >= t.Efficiency && highADX
	if bigMove {
		if up {
			if clean {
				return "trending_up_clean"
			}
			return "trending_up_choppy"
		}
		if clean {
			return "trending_down_clean"
		}
		return "trending_down_choppy"
	}
	if highADX {
		return "ranging_directional"
	}
	if wide {
		return "ranging_volatile"
	}
	return "ranging_quiet"
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func appendRegimePayloadArg(args []string, payload RegimePayload) []string {
	if payload.IsEmpty() {
		return args
	}
	blob, err := json.Marshal(payload)
	if err != nil || len(blob) == 0 {
		return args
	}
	return append(args, "--regime-payload-json", string(blob))
}

func cycleRegimePayload(precomputed RegimePayload, result *RegimePayload) RegimePayload {
	if !precomputed.IsEmpty() {
		return precomputed
	}
	return regimePayloadValue(result)
}

func notifyRegimeScriptFailure(notifier *MultiNotifier, key RegimeRawKey, errMsg string) {
	sc := StrategyConfig{
		ID:       key.trackerID(),
		Platform: key.Platform,
		Script:   regimeBundleScript,
	}
	notifyScriptFailure(notifier, sc, scriptFailureCrash, errMsg)
}

func clearRegimeScriptFailure(notifier *MultiNotifier, key RegimeRawKey) {
	sc := StrategyConfig{
		ID:       key.trackerID(),
		Platform: key.Platform,
		Script:   regimeBundleScript,
	}
	clearScriptFailure(notifier, sc)
}
