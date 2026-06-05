package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	regimeBundleScript    = "shared_scripts/compute_regime_bundle.py"
	optionsRegimeInterval = "4h"
	optionsRegimePeriod   = 14
)

// regimeMarketRef identifies the OHLCV source for one raw regime key.
type regimeMarketRef struct {
	Platform string
	Type     string
	Symbol   string
	Interval string
	Mode     string // topstep paper/live; empty otherwise
}

// regimeRawKey is the raw-layer store key: one ADX/efficiency computation per cycle.
type regimeRawKey struct {
	Platform string
	Type     string
	Symbol   string
	Interval string
	Period   int
}

func (k regimeRawKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", k.Platform, k.Type, k.Symbol, k.Interval, k.Period)
}

// regimeLabelKey is the label-layer key (classifier + thresholds over a raw entry).
type regimeLabelKey struct {
	Raw        regimeRawKey
	Classifier string
	Period     int
	// ADXThreshold set when Classifier is adx; composite uses Thresholds.
	ADXThreshold float64
	Thresholds   RegimeCompositeThresholds
}

func (k regimeLabelKey) String() string {
	if k.Classifier == regimeClassifierComposite {
		th := k.Thresholds
		return fmt.Sprintf("%s|composite|%d|%g|%g|%g|%g",
			k.Raw.String(), k.Period, th.ReturnPct, th.RangePct, th.ADX, th.Efficiency)
	}
	return fmt.Sprintf("%s|adx|%d|%g", k.Raw.String(), k.Period, k.ADXThreshold)
}

// RegimeBundleRaw holds indicator values from one raw subprocess result.
type RegimeBundleRaw struct {
	ADX          float64 `json:"adx"`
	PlusDI       float64 `json:"plus_di"`
	MinusDI      float64 `json:"minus_di"`
	CompositeADX float64 `json:"composite_adx"`
	ReturnEff    float64 `json:"return_eff"`
	RangeEff     float64 `json:"range_eff"`
	Efficiency   float64 `json:"efficiency"`
	ATRPct       float64 `json:"atr_pct"`
}

// regimeBundleEntry is one raw-layer value plus metadata from Python.
type regimeBundleEntry struct {
	BarTime int64
	Raw     RegimeBundleRaw
}

// MarketRegimeEntry is one projected label from the global regime store for
// portfolio/dashboard display (#879).
type MarketRegimeEntry struct {
	Platform   string `json:"platform"`
	Type       string `json:"type"`
	Symbol     string `json:"symbol"`
	Interval   string `json:"interval"`
	Period     int    `json:"period"`
	Classifier string `json:"classifier"`
	Label      string `json:"label"`
}

// globalRegimeStore is rebuilt every scheduler cycle (#879).
type globalRegimeStore struct {
	mu     sync.RWMutex
	raw    map[regimeRawKey]*regimeBundleEntry
	labels map[regimeLabelKey]string
}

func newGlobalRegimeStore() *globalRegimeStore {
	return &globalRegimeStore{
		raw:    make(map[regimeRawKey]*regimeBundleEntry),
		labels: make(map[regimeLabelKey]string),
	}
}

func regimeMarketContext(sc StrategyConfig) (regimeMarketRef, bool) {
	platform := strings.TrimSpace(sc.Platform)
	typ := strings.TrimSpace(sc.Type)
	if platform == "" || typ == "" {
		return regimeMarketRef{}, false
	}
	switch typ {
	case "manual":
		sym := strings.TrimSpace(sc.Symbol)
		if sym == "" && len(sc.Args) > 1 {
			sym = strings.TrimSpace(sc.Args[1])
		}
		tf := strings.TrimSpace(sc.Timeframe)
		if tf == "" {
			tf = extractTimeframe(sc)
		}
		if sym == "" || tf == "" || tf == "—" {
			return regimeMarketRef{}, false
		}
		return regimeMarketRef{Platform: platform, Type: typ, Symbol: sym, Interval: tf, Mode: topstepModeFromArgs(sc.Args)}, true
	case "options":
		if len(sc.Args) < 2 {
			return regimeMarketRef{}, false
		}
		return regimeMarketRef{
			Platform: platform,
			Type:     typ,
			Symbol:   strings.TrimSpace(sc.Args[1]),
			Interval: optionsRegimeInterval,
		}, true
	default:
		sym := ""
		if len(sc.Args) > 1 {
			sym = strings.TrimSpace(sc.Args[1])
		}
		tf := extractTimeframe(sc)
		if sym == "" || tf == "" || tf == "—" {
			return regimeMarketRef{}, false
		}
		return regimeMarketRef{Platform: platform, Type: typ, Symbol: sym, Interval: tf, Mode: topstepModeFromArgs(sc.Args)}, true
	}
}

func topstepModeFromArgs(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--mode=") {
			return strings.TrimPrefix(a, "--mode=")
		}
	}
	return ""
}

func regimeWindowSpecsForCycle(rc *RegimeConfig) map[string]RegimeWindowSpec {
	out := make(map[string]RegimeWindowSpec)
	if rc == nil {
		return out
	}
	if len(rc.Windows) > 0 {
		for name, spec := range rc.Windows {
			out[name] = spec.resolvedForEmit(rc)
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

func collectRegimeCycleNeeds(due []StrategyConfig, cfg *Config) ([]regimeRawKey, []regimeLabelKey) {
	if cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
		return nil, nil
	}
	rc := cfg.Regime
	rawSeen := make(map[regimeRawKey]bool)
	labelSeen := make(map[regimeLabelKey]bool)
	var rawKeys []regimeRawKey
	var labelKeys []regimeLabelKey

	addRaw := func(k regimeRawKey) {
		if rawSeen[k] {
			return
		}
		rawSeen[k] = true
		rawKeys = append(rawKeys, k)
	}
	addLabel := func(k regimeLabelKey) {
		if labelSeen[k] {
			return
		}
		labelSeen[k] = true
		labelKeys = append(labelKeys, k)
	}

	windows := regimeWindowSpecsForCycle(rc)
	for _, sc := range due {
		market, ok := regimeMarketContext(sc)
		if !ok {
			continue
		}
		if sc.Type == "options" {
			rk := regimeRawKey{market.Platform, market.Type, market.Symbol, optionsRegimeInterval, optionsRegimePeriod}
			addRaw(rk)
			spec := RegimeWindowSpec{Classifier: regimeClassifierADX, Period: optionsRegimePeriod, ADXThreshold: rc.ADXThreshold}.resolvedForEmit(rc)
			addLabel(regimeLabelKey{Raw: rk, Classifier: regimeClassifierADX, Period: optionsRegimePeriod, ADXThreshold: spec.adxThreshold(rc)})
			continue
		}
		for name, spec := range windows {
			_ = name
			resolved := spec.resolvedForEmit(rc)
			rk := regimeRawKey{market.Platform, market.Type, market.Symbol, market.Interval, resolved.Period}
			addRaw(rk)
			lk := regimeLabelKey{Raw: rk, Classifier: resolved.effectiveClassifier(), Period: resolved.Period}
			if lk.Classifier == regimeClassifierComposite {
				lk.Thresholds = resolved.compositeThresholds()
			} else {
				lk.ADXThreshold = resolved.adxThreshold(rc)
			}
			addLabel(lk)
		}
	}
	return rawKeys, labelKeys
}

type regimeBundleScriptResult struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error"`
	BarTime int64             `json:"bar_time"`
	Raw     RegimeBundleRaw   `json:"raw"`
	Labels  map[string]string `json:"labels_default"`
}

var runRegimeBundleScriptFn = runRegimeBundleScript

func runRegimeBundleScript(ctx context.Context, market regimeMarketRef, period int, rc *RegimeConfig) (*regimeBundleEntry, error) {
	limit := regimeRequiredOhlcvLimit(rc)
	args := []string{
		"--platform=" + market.Platform,
		"--type=" + market.Type,
		"--symbol=" + market.Symbol,
		"--timeframe=" + market.Interval,
		"--period=" + strconv.Itoa(period),
		"--ohlcv-limit=" + strconv.Itoa(limit),
	}
	if market.Mode != "" {
		args = append(args, "--mode="+market.Mode)
	}
	stdout, stderr, err := runPythonWithTimeout(ctx, regimeBundleScript, args, nil, scriptTimeout)
	if err != nil {
		if len(stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return nil, err
	}
	var parsed regimeBundleScriptResult
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return nil, fmt.Errorf("regime bundle JSON: %w", err)
	}
	if !parsed.OK {
		msg := strings.TrimSpace(parsed.Error)
		if msg == "" {
			msg = "regime bundle script returned ok=false"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return &regimeBundleEntry{BarTime: parsed.BarTime, Raw: parsed.Raw}, nil
}

func regimeBundleFailureStrategyID(key regimeRawKey) string {
	return fmt.Sprintf("regime-bundle:%s", key.String())
}

func notifyRegimeBundleFailure(notifier *MultiNotifier, key regimeRawKey, err error) {
	fmt.Fprintf(os.Stderr, "[WARN] regime bundle %s: %v\n", key.String(), err)
	sc := StrategyConfig{
		ID:       regimeBundleFailureStrategyID(key),
		Platform: key.Platform,
		Script:   regimeBundleScript,
	}
	notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
}

func clearRegimeBundleFailure(notifier *MultiNotifier, key regimeRawKey) {
	sc := StrategyConfig{ID: regimeBundleFailureStrategyID(key)}
	clearScriptFailure(notifier, sc)
}

func populateGlobalRegimeStore(ctx context.Context, store *globalRegimeStore, due []StrategyConfig, cfg *Config, notifier *MultiNotifier) {
	if store == nil || cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
		return
	}
	rawKeys, labelKeys := collectRegimeCycleNeeds(due, cfg)
	if len(rawKeys) == 0 {
		return
	}
	rc := cfg.Regime

	var wg sync.WaitGroup
	for _, rk := range rawKeys {
		wg.Add(1)
		go func(key regimeRawKey) {
			defer wg.Done()
			market := regimeMarketRef{
				Platform: key.Platform,
				Type:     key.Type,
				Symbol:   key.Symbol,
				Interval: key.Interval,
			}
			entry, err := runRegimeBundleScriptFn(ctx, market, key.Period, rc)
			store.mu.Lock()
			if err != nil {
				delete(store.raw, key)
				store.mu.Unlock()
				notifyRegimeBundleFailure(notifier, key, err)
				return
			}
			store.raw[key] = entry
			store.mu.Unlock()
			clearRegimeBundleFailure(notifier, key)
		}(rk)
	}
	wg.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, lk := range labelKeys {
		entry := store.raw[lk.Raw]
		if entry == nil {
			delete(store.labels, lk)
			continue
		}
		label := projectRegimeLabel(entry.Raw, lk)
		if label == "" {
			delete(store.labels, lk)
			continue
		}
		store.labels[lk] = label
	}
}

func projectRegimeLabel(raw RegimeBundleRaw, lk regimeLabelKey) string {
	switch lk.Classifier {
	case regimeClassifierComposite:
		return projectCompositeLabel(raw, lk.Thresholds)
	default:
		th := lk.ADXThreshold
		if th <= 0 {
			th = 20
		}
		return projectADXLabel(raw, th)
	}
}

func projectADXLabel(raw RegimeBundleRaw, adxThreshold float64) string {
	if raw.ADX < adxThreshold {
		return "ranging"
	}
	if raw.PlusDI > raw.MinusDI {
		return "trending_up"
	}
	if raw.MinusDI > raw.PlusDI {
		return "trending_down"
	}
	return "ranging"
}

func projectCompositeLabel(raw RegimeBundleRaw, th RegimeCompositeThresholds) string {
	th = th.withDefaults()
	retTh := th.ReturnPct
	rangeTh := th.RangePct
	adxTh := th.ADX
	effTh := th.Efficiency

	bigMove := absFloat(raw.ReturnEff) >= retTh
	up := raw.ReturnEff > 0
	highADX := raw.CompositeADX >= adxTh
	wide := raw.RangeEff >= rangeTh
	clean := raw.Efficiency >= effTh && highADX

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

func (store *globalRegimeStore) labelForWindow(sc StrategyConfig, rc *RegimeConfig, windowName string, spec RegimeWindowSpec) string {
	if store == nil {
		return ""
	}
	market, ok := regimeMarketContext(sc)
	if !ok {
		return ""
	}
	if sc.Type == "options" {
		market.Interval = optionsRegimeInterval
	}
	resolved := spec.resolvedForEmit(rc)
	rk := regimeRawKey{market.Platform, market.Type, market.Symbol, market.Interval, resolved.Period}
	lk := regimeLabelKey{Raw: rk, Classifier: resolved.effectiveClassifier(), Period: resolved.Period}
	if lk.Classifier == regimeClassifierComposite {
		lk.Thresholds = resolved.compositeThresholds()
	} else {
		lk.ADXThreshold = resolved.adxThreshold(rc)
	}
	_ = windowName
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.labels[lk]
}

// PayloadForStrategy resolves the cycle's precomputed RegimePayload for a strategy.
func (store *globalRegimeStore) PayloadForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimePayload {
	if store == nil || rc == nil || !rc.Enabled {
		return RegimePayload{}
	}
	if sc.Type == "options" {
		label := store.labelForWindow(sc, rc, regimeWindowDefaultKey, RegimeWindowSpec{
			Classifier: regimeClassifierADX,
			Period:     optionsRegimePeriod,
		})
		if label == "" {
			return RegimePayload{}
		}
		return RegimePayload{Legacy: label}
	}
	windows := regimeWindowSpecsForCycle(rc)
	if regimeMultiWindowEnabled(rc) || len(windows) > 1 {
		out := make(map[string]RegimeSnapshot, len(windows))
		for name, spec := range windows {
			label := store.labelForWindow(sc, rc, name, spec)
			if label == "" {
				continue
			}
			out[name] = RegimeSnapshot{Regime: label}
		}
		if len(out) == 0 {
			return RegimePayload{}
		}
		return RegimePayload{MultiMode: true, Windows: out}
	}
	for _, spec := range windows {
		label := store.labelForWindow(sc, rc, regimeWindowDefaultKey, spec)
		if label == "" {
			return RegimePayload{}
		}
		return RegimePayload{Legacy: label}
	}
	return RegimePayload{}
}

func cycleRegimePayload(store *globalRegimeStore, sc StrategyConfig, rc *RegimeConfig) RegimePayload {
	if store == nil {
		return RegimePayload{}
	}
	return store.PayloadForStrategy(sc, rc)
}

// MarketRegimeEntries returns a stable-sorted snapshot of label-layer entries.
func (store *globalRegimeStore) MarketRegimeEntries() []MarketRegimeEntry {
	if store == nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.labels) == 0 {
		return nil
	}
	out := make([]MarketRegimeEntry, 0, len(store.labels))
	for lk, label := range store.labels {
		if strings.TrimSpace(label) == "" {
			continue
		}
		out = append(out, MarketRegimeEntry{
			Platform:   lk.Raw.Platform,
			Type:       lk.Raw.Type,
			Symbol:     lk.Raw.Symbol,
			Interval:   lk.Raw.Interval,
			Period:     lk.Period,
			Classifier: lk.Classifier,
			Label:      label,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Interval != b.Interval {
			return a.Interval < b.Interval
		}
		if a.Period != b.Period {
			return a.Period < b.Period
		}
		if a.Classifier != b.Classifier {
			return a.Classifier < b.Classifier
		}
		return a.Platform < b.Platform
	})
	return out
}

// activeCycleRegimeStore is set for the duration of one scheduler cycle's strategy
// fan-out so execute*Result can stamp opens from the global store without threading
// the store through every signature (#879).
var activeCycleRegimeStore *globalRegimeStore

func setActiveCycleRegimeStore(store *globalRegimeStore) {
	activeCycleRegimeStore = store
}

// openStampRegimePayload prefers the cycle global store; falls back to check output.
func openStampRegimePayload(sc StrategyConfig, rc *RegimeConfig, fallback RegimePayload) RegimePayload {
	if activeCycleRegimeStore != nil && rc != nil && rc.Enabled {
		if p := activeCycleRegimeStore.PayloadForStrategy(sc, rc); !p.IsEmpty() {
			return p
		}
	}
	return fallback
}
