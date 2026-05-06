package main

import (
	"strings"
	"testing"
)

func TestComputeFallbackATR(t *testing.T) {
	cases := []struct {
		fillPrice float64
		leverage  float64
		wantATR   float64
		wantOK    bool
	}{
		{2500, 20, 12.5, true}, // issue worked example: ETH @ $2500, 20× lev
		{100, 10, 1.0, true},   // basic
		{100, 0, 0, false},     // leverage == 0
		{100, -1, 0, false},    // leverage < 0
		{0, 10, 0, false},      // fillPrice == 0
		{-1, 10, 0, false},     // fillPrice < 0
		{2500, 1, 250, true},   // 1× leverage → 10% of price
	}
	for _, c := range cases {
		got, ok := computeFallbackATR(c.fillPrice, c.leverage)
		if ok != c.wantOK {
			t.Errorf("computeFallbackATR(%.2f, %.2f): ok=%v want %v", c.fillPrice, c.leverage, ok, c.wantOK)
		}
		if ok && (got < c.wantATR*0.9999 || got > c.wantATR*1.0001) {
			t.Errorf("computeFallbackATR(%.2f, %.2f): atr=%.6f want %.6f", c.fillPrice, c.leverage, got, c.wantATR)
		}
	}
}

func TestPlaceManualProtectionInline_NoTiers(t *testing.T) {
	sc := StrategyConfig{
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategies: []string{"tp_at_pct"}, // not tiered_tp_atr*
	}
	oids, warn, err := placeManualProtectionInline(sc, "long", 0.8, 2500, 12.5, 1.0, 0)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if warn != "" {
		t.Errorf("expected empty warn, got %q", warn)
	}
	if len(oids) != 0 {
		t.Errorf("expected no OIDs for non-tiered strategy, got %v", oids)
	}
}

func TestPlaceManualProtectionInline_TPErrorsSurface(t *testing.T) {
	orig := runHLSyncProtectionFn
	defer func() { runHLSyncProtectionFn = orig }()

	runHLSyncProtectionFn = func(script, symbol, side string, size, avgCost, entryATR, stopLossATRMult float64, tiers []hlProtectionTier, stopLossOID int64, tpOIDs []int64) (*HyperliquidProtectionSyncResult, string, error) {
		return &HyperliquidProtectionSyncResult{
			TPOIDs:   []int64{1001, 1002},
			TPErrors: []string{"TP1: rejected", ""},
		}, "", nil
	}

	sc := StrategyConfig{
		Type:            "manual",
		Platform:        "hyperliquid",
		Script:          "shared_scripts/check_hyperliquid.py",
		Symbol:          "ETH",
		CloseStrategies: []string{"tiered_tp_atr_live"},
	}
	oids, warn, err := placeManualProtectionInline(sc, "long", 0.8, 2500, 12.5, 1.0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(warn, "TP1") {
		t.Errorf("expected warn to mention TP1, got %q", warn)
	}
	if len(oids) != 2 {
		t.Errorf("expected 2 OIDs, got %v", oids)
	}
}

func TestWarnNotifier_NilNotifier(t *testing.T) {
	// Should not panic when notifier is nil.
	warnNotifier(nil, "test warning")
}
