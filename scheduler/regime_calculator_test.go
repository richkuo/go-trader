package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	store := buildRegimeStore(due, rc, nil)
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
	buildRegimeStore(due, &RegimeConfig{Enabled: false}, nil)
	if calls != 0 {
		t.Fatalf("disabled regime must not spawn subprocesses, got %d", calls)
	}
}

func TestOptionsRegimeSignature(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	sig := optionsRegimeSignature(StrategyConfig{Platform: "deribit", Type: "options"}, "BTC", rc)
	if sig.Symbol != "BTC" || sig.Interval != "4h" {
		t.Fatalf("got %+v", sig)
	}
	// store round-trips an options-sourced label
	s := newRegimeStore()
	s.put(sig, RegimePayload{Legacy: "trending_up"}, nil)
	got, ok := s.get(sig)
	if !ok || got.PrimaryLabel(rc) != "trending_up" {
		t.Fatalf("options store read failed: %+v ok=%v", got, ok)
	}
}

func TestRegimeStoreSnapshotSorted(t *testing.T) {
	s := newRegimeStore()
	s.put(RegimeSignature{Symbol: "ETH", Interval: "1h", SpecHash: "x"}, RegimePayload{Legacy: "ranging"}, nil)
	s.put(RegimeSignature{Symbol: "BTC", Interval: "4h", SpecHash: "x"}, RegimePayload{Legacy: "trending_up"}, nil)
	s.put(RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "x"}, RegimePayload{}, errSentinel)
	snap := s.snapshot(nil)
	if len(snap) != 3 {
		t.Fatalf("want 3 entries, got %d", len(snap))
	}
	// sorted by symbol then interval: BTC/1h, BTC/4h, ETH/1h
	if snap[0].Symbol != "BTC" || snap[0].Interval != "1h" || !snap[0].Failed {
		t.Fatalf("entry0 = %+v", snap[0])
	}
	if snap[1].Symbol != "BTC" || snap[1].Interval != "4h" || snap[1].Regime != "trending_up" {
		t.Fatalf("entry1 = %+v", snap[1])
	}
	if snap[2].Symbol != "ETH" || snap[2].Regime != "ranging" {
		t.Fatalf("entry2 = %+v", snap[2])
	}
}

func TestRegimeFailureThrottleCrossesThreshold(t *testing.T) {
	regimeFailureTracker = &ScriptFailureTracker{} // isolate
	sig := RegimeSignature{Symbol: "BTC", Interval: "1h", SpecHash: "z"}
	key := regimeSignatureKey(sig)
	now := time.Now().UTC()
	for i := 1; i < scriptFailureAlertThreshold; i++ {
		if notify, _ := regimeFailureTracker.Record(key, "boom", now); notify {
			t.Fatalf("alert fired early at failure %d", i)
		}
	}
	if notify, count := regimeFailureTracker.Record(key, "boom", now); !notify || count != scriptFailureAlertThreshold {
		t.Fatalf("expected alert at threshold, notify=%v count=%d", notify, count)
	}
	// recovery clears
	if recovered, _ := regimeFailureTracker.Clear(key); !recovered {
		t.Fatal("expected recovery flag after alerted streak")
	}
}

func TestOptionsSignatureCannotClobberBundle(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	// A perps strategy on BTC at 4h and an options strategy on underlying BTC at
	// the hardcoded 4h interval must NOT collapse to the same store key (#901 #1).
	perps := StrategyConfig{ID: "hl-btc-4h", Symbol: "BTC", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "BTC", "4h"}}
	bundleSig := regimeSignatureForStrategy(perps, rc)
	optSig := optionsRegimeSignature(StrategyConfig{Platform: "deribit", Type: "options"}, "BTC", rc)
	if bundleSig == optSig {
		t.Fatalf("options and bundle signatures collided: %+v", bundleSig)
	}

	store := newRegimeStore()
	store.put(bundleSig, RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
		"medium": {Regime: "trending_up", Score: 0.5}}}, nil)
	// Options put must not overwrite the real bundle.
	store.put(optSig, RegimePayload{Legacy: "ranging"}, nil)
	if lbl := store.payloadForStrategy(perps, rc).PrimaryLabel(rc); lbl != "trending_up" {
		t.Fatalf("bundle was clobbered by options put: got %q", lbl)
	}
}

func TestRegimeSignatureIncludesPlatform(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	// Same symbol string + interval on different platforms must differ (#901 #2).
	hl := StrategyConfig{Type: "perps", Platform: "hyperliquid", Args: []string{"m", "BTC", "1h"}}
	okx := StrategyConfig{Type: "perps", Platform: "okx", Args: []string{"m", "BTC", "1h"}}
	if regimeSignatureForStrategy(hl, rc) == regimeSignatureForStrategy(okx, rc) {
		t.Fatal("cross-platform same-symbol signatures must differ")
	}
}

func TestSnapshotHonorsPrimaryWindow(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{
		"macro":  {Classifier: "adx", Period: 50},
		"medium": {Classifier: "adx", Period: 14},
	}}
	s := newRegimeStore()
	sig := RegimeSignature{Platform: "hyperliquid", Symbol: "BTC", Interval: "1h", Kind: regimeSignatureKindBundle}
	s.put(sig, RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
		"macro":  {Regime: "ranging"},
		"medium": {Regime: "trending_up"},
	}}, nil)
	snap := s.snapshot(rc)
	if len(snap) != 1 || snap[0].Regime != "trending_up" {
		t.Fatalf("snapshot should honor primary window 'medium', got %+v", snap)
	}
}
