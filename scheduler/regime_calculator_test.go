package main

import (
	"strings"
	"sync/atomic"
	"testing"
)

type errString string

func (e errString) Error() string { return string(e) }

var errSentinel = errString("boom")

func TestRegimeSignatureForStrategy(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	sc := StrategyConfig{ID: "hl-btc", Symbol: "BTC", Args: []string{"momentum", "BTC", "1h"}, Platform: "hyperliquid"}
	sig := regimeSignatureForStrategy(sc, rc)
	if sig.Symbol != "BTC" || sig.Interval != "1h" {
		t.Fatalf("got %+v", sig)
	}
	sc2 := StrategyConfig{ID: "hl-btc-2", Symbol: "BTC", Args: []string{"meanrev", "BTC", "1h"}, Platform: "hyperliquid"}
	if regimeSignatureForStrategy(sc2, rc) != sig {
		t.Fatal("peers on same asset/interval must share a signature")
	}
	// distinct interval → distinct signature
	sc3 := StrategyConfig{ID: "hl-btc-4h", Symbol: "BTC", Args: []string{"momentum", "BTC", "4h"}, Platform: "hyperliquid"}
	if regimeSignatureForStrategy(sc3, rc) == sig {
		t.Fatal("distinct interval must produce distinct signature")
	}
}

func TestPositionalArgSkipsFlags(t *testing.T) {
	if got := positionalArg([]string{"strat", "BTC", "--mode=paper"}, 2); got != "" {
		t.Fatalf("flag at idx 2 must yield empty, got %q", got)
	}
	if got := positionalArg([]string{"strat", "BTC", "1h"}, 2); got != "1h" {
		t.Fatalf("got %q", got)
	}
}

func TestRegimeStorePutGet(t *testing.T) {
	s := newRegimeStore()
	sig := RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "abc"}
	s.put(sig, RegimePayload{Legacy: "trending_up"}, nil)
	got, ok := s.get(sig)
	if !ok || got.PrimaryLabel(nil) != "trending_up" {
		t.Fatalf("get failed: %+v ok=%v", got, ok)
	}
}

func TestRegimeStoreFailureClears(t *testing.T) {
	s := newRegimeStore()
	sig := RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "abc"}
	s.put(sig, RegimePayload{Legacy: "ignored"}, errSentinel)
	got, ok := s.get(sig)
	if !ok {
		t.Fatal("failed signature must still be present (empty), to signal fail-open not missing")
	}
	if !got.IsEmpty() {
		t.Fatal("failed signature payload must be empty")
	}
	if !s.failed(sig) {
		t.Fatal("failed() must report the failure for alerting")
	}
}

func TestPayloadForStrategyDisabledRegime(t *testing.T) {
	s := newRegimeStore()
	sc := StrategyConfig{Args: []string{"m", "BTC", "1h"}}
	if !s.payloadForStrategy(sc, &RegimeConfig{Enabled: false}).IsEmpty() {
		t.Fatal("disabled regime must yield empty payload")
	}
	if !s.payloadForStrategy(sc, nil).IsEmpty() {
		t.Fatal("nil regime must yield empty payload")
	}
}

func TestRegimeSubprocessArgv(t *testing.T) {
	argv := regimeSubprocessArgv("hyperliquid", "BTC", "1h",
		`{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`, 200, "", "")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--platform=hyperliquid", "--symbol=BTC", "--interval=1h",
		"--regime-windows-spec-json", "--ohlcv-limit=200"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, argv)
		}
	}
}

func TestRegimeSubprocessArgvInstType(t *testing.T) {
	argv := regimeSubprocessArgv("okx", "BTC", "1h", `{}`, 200, "swap", "")
	if !strings.Contains(strings.Join(argv, " "), "--inst-type=swap") {
		t.Fatalf("expected --inst-type=swap: %v", argv)
	}
	argv2 := regimeSubprocessArgv("hyperliquid", "BTC", "1h", `{}`, 200, "", "")
	if strings.Contains(strings.Join(argv2, " "), "--inst-type") {
		t.Fatalf("empty inst-type must be omitted: %v", argv2)
	}
}

func TestBuildRegimeStoreDedupsAndClearsFailures(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	due := []StrategyConfig{
		{ID: "a", Symbol: "BTC", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "BTC", "1h"}},
		{ID: "b", Symbol: "BTC", Type: "perps", Platform: "hyperliquid", Args: []string{"r", "BTC", "1h"}}, // peer → dedup
		{ID: "c", Symbol: "ETH", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"}},
		{ID: "d", Symbol: "ETH", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "4h"}}, // distinct interval, fails
	}
	var calls int32
	orig := runRegimeSubprocessFn
	defer func() { runRegimeSubprocessFn = orig }()
	runRegimeSubprocessFn = func(platform, symbol, interval, spec string, limit int, instType, mode string) (RegimePayload, error) {
		atomic.AddInt32(&calls, 1)
		if symbol == "ETH" && interval == "4h" {
			return RegimePayload{}, errSentinel
		}
		return RegimePayload{Legacy: "trending_up"}, nil
	}
	store := buildRegimeStore(due, rc)
	if calls != 3 {
		t.Fatalf("expected 3 distinct signatures, got %d", calls)
	}
	if lbl := store.payloadForStrategy(due[0], rc).PrimaryLabel(rc); lbl != "trending_up" {
		t.Fatalf("BTC/1h label = %q", lbl)
	}
	sigFail := regimeSignatureForStrategy(due[3], rc)
	if !store.failed(sigFail) {
		t.Fatal("ETH/4h must be marked failed")
	}
	if !store.payloadForStrategy(due[3], rc).IsEmpty() {
		t.Fatal("failed signature must read empty (fail-open)")
	}
}

func TestBuildRegimeStoreDisabledSkips(t *testing.T) {
	due := []StrategyConfig{{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "BTC", "1h"}}}
	orig := runRegimeSubprocessFn
	defer func() { runRegimeSubprocessFn = orig }()
	var calls int32
	runRegimeSubprocessFn = func(platform, symbol, interval, spec string, limit int, instType, mode string) (RegimePayload, error) {
		atomic.AddInt32(&calls, 1)
		return RegimePayload{}, nil
	}
	buildRegimeStore(due, &RegimeConfig{Enabled: false})
	if calls != 0 {
		t.Fatalf("disabled regime must not spawn subprocesses, got %d", calls)
	}
}
