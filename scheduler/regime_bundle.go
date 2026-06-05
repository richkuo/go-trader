package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	regimeBundleScript          = "shared_scripts/regime_bundle.py"
	optionsDefaultRegimePeriod  = 14
	optionsDefaultRegimeADX     = 20.0
	optionsDefaultRegimeTimebar = "4h"
)

type regimeRawKey struct {
	Platform  string
	Type      string
	Symbol    string
	Timeframe string
	Period    int
	Mode      string
}

func (k regimeRawKey) String() string {
	return strings.Join([]string{k.Platform, k.Type, k.Symbol, k.Timeframe, strconv.Itoa(k.Period), k.Mode}, "|")
}

type regimeStrategySignature struct {
	StrategyID string
	Platform   string
	Type       string
	Symbol     string
	Timeframe  string
	Mode       string
	Windows    map[string]RegimeWindowSpec
}

type regimeRawMetrics struct {
	ADX        float64 `json:"adx"`
	PlusDI     float64 `json:"plus_di"`
	MinusDI    float64 `json:"minus_di"`
	ReturnEff  float64 `json:"return_eff"`
	RangeEff   float64 `json:"range_eff"`
	Efficiency float64 `json:"efficiency"`
	ATRPct     float64 `json:"atr_pct"`
}

type regimeRawResult struct {
	Symbol    string           `json:"symbol"`
	Timeframe string           `json:"timeframe"`
	Period    int              `json:"period"`
	BarTime   int64            `json:"bar_time,omitempty"`
	Source    string           `json:"source,omitempty"`
	Metrics   regimeRawMetrics `json:"metrics"`
	Error     string           `json:"error,omitempty"`
}

type RegimeBundleStore struct {
	raw      map[regimeRawKey]regimeRawResult
	payloads map[string]RegimePayload
}

func newRegimeBundleStore() *RegimeBundleStore {
	return &RegimeBundleStore{
		raw:      make(map[regimeRawKey]regimeRawResult),
		payloads: make(map[string]RegimePayload),
	}
}

func (s *RegimeBundleStore) Payload(sc StrategyConfig) (RegimePayload, bool) {
	if s == nil {
		return RegimePayload{}, false
	}
	p, ok := s.payloads[sc.ID]
	return p, ok
}

func (s *RegimeBundleStore) PayloadOr(sc StrategyConfig, fallback RegimePayload) RegimePayload {
	if p, ok := s.Payload(sc); ok {
		return p
	}
	return fallback
}

type regimeRawRunner func(regimeRawKey, int) (regimeRawResult, string, error)

var runRegimeRawBundleFn regimeRawRunner = runRegimeRawBundle

func buildRegimeBundleStore(due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) *RegimeBundleStore {
	store := newRegimeBundleStore()
	signatures := collectRegimeStrategySignatures(due, rc)
	if len(signatures) == 0 {
		return store
	}

	rawKeys := make(map[regimeRawKey]int)
	for _, sig := range signatures {
		for _, spec := range sig.Windows {
			period := spec.resolvedForEmit(rc).Period
			if period < 2 {
				continue
			}
			key := regimeRawKey{
				Platform:  sig.Platform,
				Type:      sig.Type,
				Symbol:    sig.Symbol,
				Timeframe: sig.Timeframe,
				Period:    period,
				Mode:      sig.Mode,
			}
			limit := regimeRawOhlcvLimit(period)
			if limit > rawKeys[key] {
				rawKeys[key] = limit
			}
		}
	}

	keys := make([]regimeRawKey, 0, len(rawKeys))
	for key := range rawKeys {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		result, stderr, err := runRegimeRawBundleFn(key, rawKeys[key])
		if stderr != "" {
			fmt.Printf("[INFO] regime bundle stderr %s: %s\n", key.String(), stderr)
		}
		if err != nil || result.Error != "" {
			msg := result.Error
			if msg == "" {
				msg = err.Error()
			}
			fmt.Printf("[WARN] regime bundle failed for %s: %s\n", key.String(), msg)
			notifyRegimeBundleFailure(notifier, key, msg)
			continue
		}
		store.raw[key] = result
	}

	for _, sig := range signatures {
		store.payloads[sig.StrategyID] = projectRegimePayload(sig, store.raw, rc)
	}
	return store
}

func collectRegimeStrategySignatures(strategies []StrategyConfig, rc *RegimeConfig) []regimeStrategySignature {
	out := make([]regimeStrategySignature, 0, len(strategies))
	for _, sc := range strategies {
		sig, ok := regimeStrategySignatureFor(sc, rc)
		if ok {
			out = append(out, sig)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StrategyID < out[j].StrategyID })
	return out
}

func regimeStrategySignatureFor(sc StrategyConfig, rc *RegimeConfig) (regimeStrategySignature, bool) {
	platform := strings.TrimSpace(strings.ToLower(sc.Platform))
	if platform == "" {
		platform = "binanceus"
	}
	typ := strings.TrimSpace(strings.ToLower(sc.Type))
	symbol := regimeStrategySymbol(sc)
	timeframe := regimeStrategyTimeframe(sc)
	mode := argValue(sc.Args, "--mode")

	var windows map[string]RegimeWindowSpec
	if typ == "options" {
		if timeframe == "" {
			timeframe = optionsDefaultRegimeTimebar
		}
		windows = map[string]RegimeWindowSpec{
			regimeWindowDefaultKey: {
				Classifier:   regimeClassifierADX,
				Period:       optionsDefaultRegimePeriod,
				ADXThreshold: optionsDefaultRegimeADX,
			},
		}
	} else {
		if rc == nil || !rc.Enabled {
			return regimeStrategySignature{}, false
		}
		windows = regimeResolvedWindows(rc)
	}

	if symbol == "" || timeframe == "" || len(windows) == 0 {
		return regimeStrategySignature{}, false
	}
	return regimeStrategySignature{
		StrategyID: sc.ID,
		Platform:   platform,
		Type:       typ,
		Symbol:     symbol,
		Timeframe:  timeframe,
		Mode:       mode,
		Windows:    windows,
	}, true
}

func regimeResolvedWindows(rc *RegimeConfig) map[string]RegimeWindowSpec {
	out := make(map[string]RegimeWindowSpec)
	if rc == nil || !rc.Enabled {
		return out
	}
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

func regimeStrategySymbol(sc StrategyConfig) string {
	switch strings.ToLower(sc.Type) {
	case "manual":
		return strings.TrimSpace(sc.Symbol)
	case "options":
		if len(sc.Args) >= 2 {
			return strings.TrimSpace(sc.Args[1])
		}
	case "perps":
		if strings.EqualFold(sc.Platform, "okx") {
			return okxSymbol(sc.Args)
		}
		return hyperliquidSymbol(sc.Args)
	case "futures":
		return topstepSymbol(sc.Args)
	default:
		return spotSymbol(sc.Args)
	}
	return ""
}

func regimeStrategyTimeframe(sc StrategyConfig) string {
	if strings.TrimSpace(sc.Timeframe) != "" {
		return strings.TrimSpace(sc.Timeframe)
	}
	if strings.EqualFold(sc.Type, "options") {
		return optionsDefaultRegimeTimebar
	}
	if len(sc.Args) >= 3 && !strings.HasPrefix(sc.Args[2], "-") {
		return strings.TrimSpace(sc.Args[2])
	}
	return ""
}

func argValue(args []string, name string) string {
	prefix := name + "="
	for i, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(arg, prefix))
		}
		if arg == name && i+1 < len(args) {
			return strings.TrimSpace(args[i+1])
		}
	}
	return ""
}

func regimeRawOhlcvLimit(period int) int {
	warmup := 2*period - 1
	limit := warmup + regimeOhlcvMargin
	if limit < regimeOhlcvBaseLimit {
		return regimeOhlcvBaseLimit
	}
	return limit
}

func runRegimeRawBundle(key regimeRawKey, limit int) (regimeRawResult, string, error) {
	args := []string{
		"--platform", key.Platform,
		"--type", key.Type,
		"--symbol", key.Symbol,
		"--timeframe", key.Timeframe,
		"--period", strconv.Itoa(key.Period),
		"--limit", strconv.Itoa(limit),
	}
	if key.Mode != "" {
		args = append(args, "--mode", key.Mode)
	}
	stdout, stderr, err := runPythonReadOnly(regimeBundleScript, args)
	var result regimeRawResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if err != nil {
			return result, string(stderr), fmt.Errorf("%w (parse output: %v)", err, jsonErr)
		}
		return result, string(stderr), fmt.Errorf("parse output: %w (stdout: %s)", jsonErr, string(stdout))
	}
	if err != nil {
		return result, string(stderr), err
	}
	return result, string(stderr), nil
}

func projectRegimePayload(sig regimeStrategySignature, raw map[regimeRawKey]regimeRawResult, rc *RegimeConfig) RegimePayload {
	windows := make(map[string]RegimeSnapshot, len(sig.Windows))
	names := make([]string, 0, len(sig.Windows))
	for name := range sig.Windows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := sig.Windows[name].resolvedForEmit(rc)
		key := regimeRawKey{
			Platform:  sig.Platform,
			Type:      sig.Type,
			Symbol:    sig.Symbol,
			Timeframe: sig.Timeframe,
			Period:    spec.Period,
			Mode:      sig.Mode,
		}
		result, ok := raw[key]
		if !ok {
			continue
		}
		windows[name] = projectRegimeSnapshot(spec, result.Metrics)
	}
	if len(windows) == 0 {
		return RegimePayload{}
	}
	return RegimePayload{Windows: windows, MultiMode: true}
}

func projectRegimeSnapshot(spec RegimeWindowSpec, metrics regimeRawMetrics) RegimeSnapshot {
	label := ""
	switch spec.effectiveClassifier() {
	case regimeClassifierComposite:
		label = mapCompositeRegimeLabel(metrics, spec.compositeThresholds())
	default:
		label = mapADXRegimeLabel(metrics, spec.adxThreshold(nil))
	}
	score := metrics.ADX / 100.0
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return RegimeSnapshot{
		Regime: label,
		Score:  score,
		Metrics: map[string]float64{
			"adx":        metrics.ADX,
			"plus_di":    metrics.PlusDI,
			"minus_di":   metrics.MinusDI,
			"return_eff": metrics.ReturnEff,
			"range_eff":  metrics.RangeEff,
			"efficiency": metrics.Efficiency,
			"atr_pct":    metrics.ATRPct,
		},
	}
}

func mapADXRegimeLabel(metrics regimeRawMetrics, threshold float64) string {
	if threshold <= 0 {
		threshold = 20
	}
	if metrics.ADX < threshold {
		return "ranging"
	}
	if metrics.PlusDI > metrics.MinusDI {
		return "trending_up"
	}
	if metrics.MinusDI > metrics.PlusDI {
		return "trending_down"
	}
	return "ranging"
}

func mapCompositeRegimeLabel(metrics regimeRawMetrics, thresholds RegimeCompositeThresholds) string {
	retTh := thresholds.ReturnPct
	rangeTh := thresholds.RangePct
	adxTh := thresholds.ADX
	effTh := thresholds.Efficiency
	bigMove := absFloat(metrics.ReturnEff) >= retTh
	up := metrics.ReturnEff > 0
	highADX := metrics.ADX >= adxTh
	wide := metrics.RangeEff >= rangeTh
	clean := metrics.Efficiency >= effTh && highADX
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

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func notifyRegimeBundleFailure(notifier *MultiNotifier, key regimeRawKey, errMsg string) {
	sc := StrategyConfig{
		ID:       "regime:" + key.String(),
		Platform: key.Platform,
		Type:     key.Type,
		Script:   regimeBundleScript,
	}
	notifyScriptFailure(notifier, sc, scriptFailureCrash, errMsg)
}
