package main

import (
	"errors"
	"testing"
)

func TestProjectRegimeSnapshotADXUsesFullPeriodDI(t *testing.T) {
	raw := regimeRawBundle{Raw: regimeRawMetrics{
		ADX:          32,
		PlusDI:       28,
		MinusDI:      14,
		CompositeADX: 8,
		ATRPct:       1.25,
	}}
	spec := RegimeWindowSpec{Classifier: regimeClassifierADX, Period: 28, ADXThreshold: 20}

	snap := projectRegimeSnapshot(raw, spec)

	if snap.Regime != "trending_up" {
		t.Fatalf("ADX projection regime = %q, want trending_up", snap.Regime)
	}
	if snap.Metrics["adx"] != 32 {
		t.Fatalf("ADX projection used adx=%g, want full-period ADX 32", snap.Metrics["adx"])
	}
}

func TestProjectRegimeSnapshotCompositeUsesCompositeADX(t *testing.T) {
	raw := regimeRawBundle{Raw: regimeRawMetrics{
		ADX:          5,
		PlusDI:       30,
		MinusDI:      10,
		CompositeADX: 30,
		ReturnEff:    0.08,
		RangeEff:     0.04,
		Efficiency:   0.8,
		ATRPct:       2,
	}}
	th := defaultCompositeThresholds
	spec := RegimeWindowSpec{Classifier: regimeClassifierComposite, Period: 28, Thresholds: &th}

	snap := projectRegimeSnapshot(raw, spec)

	if snap.Regime != "trending_up_clean" {
		t.Fatalf("composite projection regime = %q, want trending_up_clean", snap.Regime)
	}
	if snap.Metrics["adx"] != 30 {
		t.Fatalf("composite projection used metrics.adx=%g, want capped composite ADX 30", snap.Metrics["adx"])
	}
}

func TestBuildCycleRegimeStoreFailureInjectsEmptyPayload(t *testing.T) {
	orig := runRegimeBundleComputeFn
	defer func() { runRegimeBundleComputeFn = orig }()
	runRegimeBundleComputeFn = func(RegimeRawKey, int) (regimeRawBundle, error) {
		return regimeRawBundle{}, errors.New("synthetic regime outage")
	}
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
		Strategies: []StrategyConfig{{
			ID:       "spot-btc",
			Type:     "spot",
			Platform: "binanceus",
			Script:   "shared_scripts/check_strategy.py",
			Args:     []string{"sma", "BTC/USDT", "1h"},
		}},
	}

	store, stats := buildCycleRegimeStore(cfg.Strategies, cfg, nil)
	payload := store.PayloadForStrategy(cfg.Strategies[0])

	if stats.RawRequested != 1 || stats.RawComputed != 0 || stats.Failures != 1 {
		t.Fatalf("stats = %+v, want one failed raw compute", stats)
	}
	if payload.IsEmpty() {
		t.Fatalf("failed global compute should still inject an explicit empty payload")
	}
	if got := payload.PrimaryLabel(cfg.Regime); got != "" {
		t.Fatalf("failed global compute label = %q, want empty", got)
	}
}

func TestCollectRegimeRequestsSharesRawKeyAcrossThresholds(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"fast": {Classifier: regimeClassifierADX, Period: 14, ADXThreshold: 20},
				"slow": {Classifier: regimeClassifierADX, Period: 14, ADXThreshold: 30},
			},
		},
	}
	sc := StrategyConfig{
		ID:       "hl-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"momentum", "BTC", "1h"},
	}

	req, ok := collectStrategyRegimeRequest(sc, cfg.Regime)
	if !ok {
		t.Fatal("expected regime request")
	}
	if len(req.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(req.Windows))
	}
	if req.Windows[0].RawKey != req.Windows[1].RawKey {
		t.Fatalf("threshold-only differences should share raw key: %+v vs %+v", req.Windows[0].RawKey, req.Windows[1].RawKey)
	}
	if req.Windows[0].Signature == req.Windows[1].Signature {
		t.Fatalf("threshold-only differences should keep separate label signatures")
	}
}
