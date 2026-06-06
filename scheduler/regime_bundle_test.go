package main

import (
	"errors"
	"testing"
	"time"
)

func TestRegimeBundleStoreProjectsSharedRawMetrics(t *testing.T) {
	orig := runRegimeRawBundleFn
	defer func() { runRegimeRawBundleFn = orig }()

	calls := map[regimeRawKey]int{}
	runRegimeRawBundleFn = func(key regimeRawKey, limit int) (regimeRawResult, string, error) {
		calls[key]++
		return regimeRawResult{
			Symbol:    key.Symbol,
			Timeframe: key.Timeframe,
			Period:    key.Period,
			Metrics: regimeRawMetrics{
				ADX:          30,
				CompositeADX: floatPtr(30),
				PlusDI:       35,
				MinusDI:      10,
				ReturnEff:    0.08,
				RangeEff:     0.04,
				Efficiency:   0.7,
				ATRPct:       1.2,
			},
		}, "", nil
	}

	rc := &RegimeConfig{
		Enabled:      true,
		Period:       14,
		ADXThreshold: 20,
		Windows: RegimeWindowsMap{
			"fast": {Classifier: regimeClassifierADX, Period: 14, ADXThreshold: 25},
			"wide": {Classifier: regimeClassifierComposite, Period: 14, Thresholds: &RegimeCompositeThresholds{
				ReturnPct:  0.05,
				RangePct:   0.03,
				ADX:        25,
				Efficiency: 0.5,
			}},
		},
	}
	due := []StrategyConfig{
		{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h", "--mode=paper"}},
		{ID: "b", Type: "perps", Platform: "hyperliquid", Args: []string{"breakout", "BTC", "1h", "--mode=paper"}},
	}
	store := buildRegimeBundleStore(due, rc, nil)

	if len(calls) != 1 {
		t.Fatalf("raw calls = %d, want one shared raw key: %#v", len(calls), calls)
	}
	payload, ok := store.Payload(due[0])
	if !ok {
		t.Fatal("missing payload for strategy a")
	}
	if got := payload.Label("fast", rc); got != "trending_up" {
		t.Fatalf("ADX label = %q, want trending_up", got)
	}
	if got := payload.Label("wide", rc); got != "trending_up_clean" {
		t.Fatalf("composite label = %q, want trending_up_clean", got)
	}
	payloadB, ok := store.Payload(due[1])
	if !ok {
		t.Fatal("missing payload for strategy b")
	}
	if got := payloadB.PrimaryLabel(rc); got == "" {
		t.Fatal("strategy b got empty primary label")
	}
}

func TestRegimeBundleCompositeUsesCappedADXMetrics(t *testing.T) {
	orig := runRegimeRawBundleFn
	defer func() { runRegimeRawBundleFn = orig }()

	runRegimeRawBundleFn = func(key regimeRawKey, limit int) (regimeRawResult, string, error) {
		return regimeRawResult{
			Symbol:    key.Symbol,
			Timeframe: key.Timeframe,
			Period:    key.Period,
			Metrics: regimeRawMetrics{
				ADX:          10,
				CompositeADX: floatPtr(30),
				PlusDI:       35,
				MinusDI:      10,
				ReturnEff:    0.08,
				RangeEff:     0.04,
				Efficiency:   0.7,
			},
		}, "", nil
	}

	rc := &RegimeConfig{
		Enabled: true,
		Windows: RegimeWindowsMap{
			"adx_full": {Classifier: regimeClassifierADX, Period: 50, ADXThreshold: 25},
			"composite": {Classifier: regimeClassifierComposite, Period: 50, Thresholds: &RegimeCompositeThresholds{
				ReturnPct:  0.05,
				RangePct:   0.03,
				ADX:        25,
				Efficiency: 0.5,
			}},
		},
	}
	sc := StrategyConfig{ID: "a", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}}
	store := buildRegimeBundleStore([]StrategyConfig{sc}, rc, nil)
	payload, ok := store.Payload(sc)
	if !ok {
		t.Fatal("missing payload")
	}
	if got := payload.Label("adx_full", rc); got != "ranging" {
		t.Fatalf("ADX full-period label = %q, want ranging", got)
	}
	if got := payload.Label("composite", rc); got != "trending_up_clean" {
		t.Fatalf("composite capped-ADX label = %q, want trending_up_clean", got)
	}
	if got := payload.Windows["composite"].Metrics["adx"]; got != 30 {
		t.Fatalf("composite metrics.adx = %g, want capped 30", got)
	}
}

func TestRegimeBundleStoreFailureYieldsEmptyPayload(t *testing.T) {
	orig := runRegimeRawBundleFn
	defer func() { runRegimeRawBundleFn = orig }()
	runRegimeRawBundleFn = func(key regimeRawKey, limit int) (regimeRawResult, string, error) {
		return regimeRawResult{}, "", errors.New("boom")
	}

	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	sc := StrategyConfig{ID: "a", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}}
	store := buildRegimeBundleStore([]StrategyConfig{sc}, rc, nil)

	payload, ok := store.Payload(sc)
	if !ok {
		t.Fatal("expected an explicit empty payload for the strategy")
	}
	if !payload.IsEmpty() {
		t.Fatalf("payload should be empty on raw failure: %#v", payload)
	}
	gateLabel, blocked := applyRegimeGate(StrategyConfig{AllowedRegimes: []string{"trending_up"}}, payload, rc, 0)
	if blocked || gateLabel != "" {
		t.Fatalf("empty global payload should fail open, got label=%q blocked=%t", gateLabel, blocked)
	}
}

func TestRegimeBundleSuccessClearsFailureStreak(t *testing.T) {
	origRunner := runRegimeRawBundleFn
	origTracker := scriptFailureTracker
	defer func() {
		runRegimeRawBundleFn = origRunner
		scriptFailureTracker = origTracker
	}()
	scriptFailureTracker = &ScriptFailureTracker{}

	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	sc := StrategyConfig{ID: "a", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}}
	key := regimeRawKey{Platform: "binanceus", Type: "spot", Symbol: "BTC/USDT", Timeframe: "1h", Period: 14}
	notifyRegimeBundleFailure(nil, key, "boom one")
	notifyRegimeBundleFailure(nil, key, "boom two")

	runRegimeRawBundleFn = func(key regimeRawKey, limit int) (regimeRawResult, string, error) {
		return regimeRawResult{Metrics: regimeRawMetrics{ADX: 30, PlusDI: 35, MinusDI: 10}}, "", nil
	}
	buildRegimeBundleStore([]StrategyConfig{sc}, rc, nil)

	_, count := scriptFailureTracker.Record("regime:"+key.String(), "boom three", time.Now().UTC())
	if count != 1 {
		t.Fatalf("failure streak count after success = %d, want reset to 1", count)
	}
}

func TestRegimeStrategySignatureOptionsDefault(t *testing.T) {
	sc := StrategyConfig{ID: "opt", Type: "options", Platform: "deribit", Args: []string{"vol_mean_reversion", "ETH", "--platform=deribit"}}
	if _, ok := regimeStrategySignatureFor(sc, nil); ok {
		t.Fatal("options strategy should not build a bundle signature when regime is disabled")
	}
	sig, ok := regimeStrategySignatureFor(sc, &RegimeConfig{Enabled: true})
	if !ok {
		t.Fatal("options strategy should get a default regime signature when regime is enabled")
	}
	if sig.Symbol != "ETH" || sig.Timeframe != optionsDefaultRegimeTimebar {
		t.Fatalf("signature market = %s/%s, want ETH/%s", sig.Symbol, sig.Timeframe, optionsDefaultRegimeTimebar)
	}
	spec := sig.Windows[regimeWindowDefaultKey]
	if spec.effectiveClassifier() != regimeClassifierADX || spec.Period != optionsDefaultRegimePeriod {
		t.Fatalf("options spec = %+v", spec)
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
