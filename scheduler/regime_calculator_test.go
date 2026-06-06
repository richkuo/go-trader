package main

import (
	"reflect"
	"strconv"
	"testing"
)

func TestRegimeAssetKey(t *testing.T) {
	cases := []struct {
		name                       string
		sc                         StrategyConfig
		wantOK                     bool
		wantPlat, wantSym, wantInt string
	}{
		{
			name:     "perps positional symbol/interval",
			sc:       StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"triple_ema", "BTC", "1h", "--mode=live"}},
			wantOK:   true,
			wantPlat: "hyperliquid", wantSym: "BTC", wantInt: "1h",
		},
		{
			name:     "spot positional",
			sc:       StrategyConfig{Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "4h"}},
			wantOK:   true,
			wantPlat: "binanceus", wantSym: "BTC/USDT", wantInt: "4h",
		},
		{
			name:     "manual uses Symbol/Timeframe fields",
			sc:       StrategyConfig{Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "15m", Args: []string{"hold", "ETH", "15m", "--mode=live"}},
			wantOK:   true,
			wantPlat: "hyperliquid", wantSym: "ETH", wantInt: "15m",
		},
		{
			name:     "options fixed 4h, underlying uppercased",
			sc:       StrategyConfig{Type: "options", Platform: "deribit", Args: []string{"wheel", "btc"}},
			wantOK:   true,
			wantPlat: "deribit", wantSym: "BTC", wantInt: "4h",
		},
		{
			name:   "no timeframe arg → not applicable",
			sc:     StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"triple_ema", "BTC", "--mode=live"}},
			wantOK: false,
		},
		{
			name:   "missing platform → not applicable",
			sc:     StrategyConfig{Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plat, sym, intv, ok := regimeAssetKey(tc.sc)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if plat != tc.wantPlat || sym != tc.wantSym || intv != tc.wantInt {
				t.Fatalf("got (%q,%q,%q) want (%q,%q,%q)", plat, sym, intv, tc.wantPlat, tc.wantSym, tc.wantInt)
			}
		})
	}
}

func TestParseRegimeSubprocessOutput(t *testing.T) {
	t.Run("multi window", func(t *testing.T) {
		payload, bar, err := parseRegimeSubprocessOutput([]byte(
			`{"ok":true,"regime":{"medium":{"regime":"trending_up","score":0.5,"metrics":{}}},"bar_time":123}`))
		if err != nil {
			t.Fatal(err)
		}
		if !payload.MultiMode || payload.Windows["medium"].Regime != "trending_up" {
			t.Fatalf("unexpected payload %+v", payload)
		}
		if bar != 123 {
			t.Fatalf("bar=%v want 123", bar)
		}
	})
	t.Run("legacy string", func(t *testing.T) {
		payload, _, err := parseRegimeSubprocessOutput([]byte(`{"ok":true,"regime":"trending_down","bar_time":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if payload.MultiMode || payload.Legacy != "trending_down" {
			t.Fatalf("unexpected payload %+v", payload)
		}
	})
	t.Run("ok false carries error", func(t *testing.T) {
		_, _, err := parseRegimeSubprocessOutput([]byte(`{"ok":false,"error":"insufficient data: 5 bars"}`))
		if err == nil || err.Error() != "insufficient data: 5 bars" {
			t.Fatalf("want error 'insufficient data', got %v", err)
		}
	})
	t.Run("bad json errors", func(t *testing.T) {
		if _, _, err := parseRegimeSubprocessOutput([]byte(`not json`)); err == nil {
			t.Fatal("want parse error")
		}
	})
}

func adxWindowsConfig() *RegimeConfig {
	return &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
}

func TestRegimeStorePayloadFor(t *testing.T) {
	rc := adxWindowsConfig()
	store := newRegimeStore(rc)
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"x", "BTC", "1h"}}
	key := regimeAssetKeyString("hyperliquid", "BTC", "1h")

	if p := store.payloadFor(sc); p != nil {
		t.Fatalf("empty store should return nil, got %+v", p)
	}

	store.set(key, &regimeBundle{payload: RegimePayload{Legacy: "trending_up"}, ok: true})
	p := store.payloadFor(sc)
	if p == nil || p.Legacy != "trending_up" {
		t.Fatalf("hit should return payload, got %+v", p)
	}

	// A failed bundle (ok=false) must read as nil → fail open (#879 policy).
	store.set(key, &regimeBundle{ok: false})
	if p := store.payloadFor(sc); p != nil {
		t.Fatalf("failed bundle should return nil, got %+v", p)
	}
}

func TestRegimeStoreFailureFailsOpenGate(t *testing.T) {
	rc := adxWindowsConfig()
	store := newRegimeStore(rc)
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"x", "BTC", "1h"},
		AllowedRegimes: []string{"trending_up"}} // would block ranging/empty if a label were present
	store.set(regimeAssetKeyString("hyperliquid", "BTC", "1h"), &regimeBundle{ok: false})

	gateLabel, blocked := applyRegimeGate(sc, regimePayloadValue(store.payloadFor(sc)), rc, 0)
	if blocked {
		t.Fatalf("failed bundle must fail open (not block), gateLabel=%q", gateLabel)
	}
}

func TestRegimeInjectionArgs(t *testing.T) {
	rc := adxWindowsConfig()
	store := newRegimeStore(rc)
	scPerps := StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"x", "BTC", "1h"}}
	store.set(regimeAssetKeyString("hyperliquid", "BTC", "1h"),
		&regimeBundle{payload: RegimePayload{Legacy: "trending_up"}, ok: true})

	args := store.injectionArgs(scPerps)
	if len(args) != 3 || args[0] != "--regime-injected" || args[1] != "--regime-injected-json" {
		t.Fatalf("unexpected injection args %v", args)
	}
	if args[2] != `"trending_up"` {
		t.Fatalf("injected json should be the serialized payload, got %q", args[2])
	}

	// Regime disabled → non-options not covered → no injection.
	disabled := newRegimeStore(&RegimeConfig{Enabled: false})
	if got := disabled.injectionArgs(scPerps); got != nil {
		t.Fatalf("disabled regime should not inject, got %v", got)
	}

	// Options always injects, even with regime disabled and a missing bundle
	// (empty json so the check uses empty regime → fail open, parity Phase 1/2).
	scOpt := StrategyConfig{Type: "options", Platform: "deribit", Args: []string{"wheel", "BTC"}}
	got := disabled.injectionArgs(scOpt)
	if len(got) != 3 || got[2] != "" {
		t.Fatalf("options injection on miss should be present with empty json, got %v", got)
	}
}

func TestCollectRegimeAssetJobs(t *testing.T) {
	rc := adxWindowsConfig()
	due := []StrategyConfig{
		{Type: "perps", Platform: "hyperliquid", Args: []string{"a", "BTC", "1h"}},
		{Type: "perps", Platform: "hyperliquid", Args: []string{"b", "BTC", "1h"}}, // same asset → dedup
		{Type: "spot", Platform: "binanceus", Args: []string{"c", "ETH/USDT", "4h"}},
		{Type: "options", Platform: "deribit", Args: []string{"wheel", "BTC"}},
	}
	jobs := collectRegimeAssetJobs(due, rc)
	keys := map[string]bool{}
	for _, j := range jobs {
		keys[j.key] = true
	}
	want := []string{
		regimeAssetKeyString("hyperliquid", "BTC", "1h"),
		regimeAssetKeyString("binanceus", "ETH/USDT", "4h"),
		regimeAssetKeyString("deribit", "BTC", "4h"),
	}
	if len(jobs) != len(want) {
		t.Fatalf("got %d jobs (%v) want %d", len(jobs), jobs, len(want))
	}
	for _, w := range want {
		if !keys[w] {
			t.Fatalf("missing job %q in %v", w, keys)
		}
	}

	// Regime disabled: only options is computed (independent 4h ADX).
	jobsDisabled := collectRegimeAssetJobs(due, &RegimeConfig{Enabled: false})
	if len(jobsDisabled) != 1 || jobsDisabled[0].key != regimeAssetKeyString("deribit", "BTC", "4h") {
		t.Fatalf("disabled regime should yield only options job, got %v", jobsDisabled)
	}
}

func TestRegimeCheckArgvSharesOhlcvLimit(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	argv := regimeCheckArgv("hyperliquid", "BTC", "1h", false, rc)
	// Must request the same ohlcv-limit a regime-enabled check uses so the #839
	// HL cache is shared (regimeRequiredOhlcvLimit).
	wantLimit := regimeRequiredOhlcvLimit(rc)
	foundLimit := ""
	for i, a := range argv {
		if a == "--ohlcv-limit" && i+1 < len(argv) {
			foundLimit = argv[i+1]
		}
	}
	if foundLimit == "" {
		t.Fatalf("argv missing --ohlcv-limit: %v", argv)
	}
	if want := strconv.Itoa(wantLimit); foundLimit != want {
		t.Fatalf("ohlcv-limit=%s want %s (regimeRequiredOhlcvLimit)", foundLimit, want)
	}
	// First two positionals mirror a check: symbol, interval.
	if argv[0] != "BTC" || argv[1] != "1h" {
		t.Fatalf("positional symbol/interval wrong: %v", argv)
	}
}

func TestRegimeStoreSnapshotSorted(t *testing.T) {
	rc := adxWindowsConfig()
	store := newRegimeStore(rc)
	store.set(regimeAssetKeyString("hyperliquid", "ETH", "1h"), &regimeBundle{payload: RegimePayload{Legacy: "ranging"}, ok: true})
	store.set(regimeAssetKeyString("hyperliquid", "BTC", "1h"), &regimeBundle{payload: RegimePayload{Legacy: "trending_up"}, ok: true})
	store.set(regimeAssetKeyString("deribit", "BTC", "4h"), &regimeBundle{ok: false})

	snap := store.snapshot()
	gotOrder := make([]string, 0, len(snap.Assets))
	for _, a := range snap.Assets {
		gotOrder = append(gotOrder, regimeAssetKeyString(a.Platform, a.Symbol, a.Interval))
	}
	wantOrder := []string{
		regimeAssetKeyString("deribit", "BTC", "4h"),
		regimeAssetKeyString("hyperliquid", "BTC", "1h"),
		regimeAssetKeyString("hyperliquid", "ETH", "1h"),
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("snapshot order %v want %v", gotOrder, wantOrder)
	}
	// Failed asset shows ok=false with empty label; healthy asset shows its label.
	if snap.Assets[0].OK || snap.Assets[0].Regime != "" {
		t.Fatalf("failed asset should be ok=false empty, got %+v", snap.Assets[0])
	}
	if !snap.Assets[1].OK || snap.Assets[1].Regime != "trending_up" {
		t.Fatalf("healthy asset wrong, got %+v", snap.Assets[1])
	}
}
