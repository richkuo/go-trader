package main

import (
	"fmt"
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
		{2500, 20, 12.5, true},
		{100, 10, 1.0, true},
		{100, 0, 0, false},
		{100, -1, 0, false},
		{0, 10, 0, false},
		{-1, 10, 0, false},
		{2500, 1, 250, true},
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
		Type:          "manual",
		Platform:      "hyperliquid",
		CloseStrategy: &StrategyRef{Name: "tp_at_pct"},
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

	runHLSyncProtectionFn = func(script, symbol, side string, size, avgCost, entryATR, stopLossATRMult float64, tiers []hlProtectionTier, stopLossOID int64, tpOIDs []int64, _ []bool, _ bool, _ []bool, _ []int64, _ []byte) (*HyperliquidProtectionSyncResult, string, error) {
		return &HyperliquidProtectionSyncResult{
			TPOIDs:   []int64{1001, 1002},
			TPErrors: []string{"TP1: rejected", ""},
		}, "", nil
	}

	sc := StrategyConfig{
		Type:          "manual",
		Platform:      "hyperliquid",
		Script:        "shared_scripts/check_hyperliquid.py",
		Symbol:        "ETH",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"},
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

	warnNotifier(nil, "test warning")
}

func TestAttemptManualOpenCleanup_Success(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	var gotSymbol string
	var gotSize *float64
	var gotCancelOIDs []int64
	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		gotSymbol = symbol
		gotSize = partialSz
		gotCancelOIDs = cancelOIDs
		return &HyperliquidCloseResult{}, "", nil
	}

	cleanedUp, msg := attemptManualOpenCleanup("ETH", 0.8, 12345, []int64{67890, 67891})
	if !cleanedUp {
		t.Fatalf("expected cleanedUp=true, got msg=%q", msg)
	}
	if gotSymbol != "ETH" {
		t.Errorf("symbol: got %q want ETH", gotSymbol)
	}
	if gotSize == nil || *gotSize != 0.8 {
		t.Errorf("partialSz: got %v want 0.8", gotSize)
	}
	if len(gotCancelOIDs) != 3 || gotCancelOIDs[0] != 12345 || gotCancelOIDs[1] != 67890 || gotCancelOIDs[2] != 67891 {
		t.Errorf("cancelOIDs: got %v want [12345 67890 67891]", gotCancelOIDs)
	}
}

func TestAttemptManualOpenCleanup_CloseFails(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return nil, "stderr noise", fmt.Errorf("rpc timeout")
	}

	cleanedUp, msg := attemptManualOpenCleanup("ETH", 0.8, 12345, []int64{67890})
	if cleanedUp {
		t.Fatalf("expected cleanedUp=false, got msg=%q", msg)
	}
	if !strings.Contains(msg, "rpc timeout") {
		t.Errorf("msg should mention close failure cause; got %q", msg)
	}
}

func TestAttemptManualOpenCleanup_CancelStopLossError(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return &HyperliquidCloseResult{CancelStopLossError: "trigger 12345 not found"}, "", nil
	}

	cleanedUp, msg := attemptManualOpenCleanup("ETH", 0.8, 12345, nil)
	if !cleanedUp {
		t.Fatalf("expected cleanedUp=true (position closed), got msg=%q", msg)
	}
	if !strings.Contains(msg, "trigger 12345 not found") {
		t.Errorf("msg should surface cancel error; got %q", msg)
	}
}

func TestAttemptManualOpenCleanup_FiltersZeroOIDs(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	var gotCancelOIDs []int64
	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		gotCancelOIDs = cancelOIDs
		return &HyperliquidCloseResult{}, "", nil
	}

	attemptManualOpenCleanup("ETH", 0.8, 0, []int64{0, 67891})
	if len(gotCancelOIDs) != 1 || gotCancelOIDs[0] != 67891 {
		t.Errorf("cancelOIDs should filter zeros; got %v want [67891]", gotCancelOIDs)
	}
}

func TestAttemptManualOpenCleanup_NilResultNoError(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return nil, "", nil
	}

	cleanedUp, msg := attemptManualOpenCleanup("ETH", 0.8, 12345, nil)
	if cleanedUp {
		t.Fatalf("expected cleanedUp=false for nil result, got msg=%q", msg)
	}
	if !strings.Contains(msg, "nil") {
		t.Errorf("msg should mention nil result; got %q", msg)
	}
}
