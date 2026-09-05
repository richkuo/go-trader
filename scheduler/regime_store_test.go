package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testRegimeConfig() *RegimeConfig {
	return &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
}

func TestStrategyRegimeBundleRequestPlatformMapping(t *testing.T) {
	rc := testRegimeConfig()
	cases := []struct {
		name         string
		sc           StrategyConfig
		wantPlatform string
		wantSymbol   string
		wantTF       string
	}{
		{"hl perps", StrategyConfig{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}, "hyperliquid", "BTC", "1h"},
		{"okx perps", StrategyConfig{ID: "okx-a", Type: "perps", Platform: "okx", Args: []string{"momentum", "BTC-USDT-SWAP", "1h"}}, "okx", "BTC-USDT-SWAP", "1h"},
		{"okx spot", StrategyConfig{ID: "okx-s", Type: "spot", Platform: "okx", Args: []string{"sma", "BTC-USDT", "4h"}}, "okx", "BTC-USDT", "4h"},
		{"robinhood spot", StrategyConfig{ID: "rh-a", Type: "spot", Platform: "robinhood", Args: []string{"sma", "BTC", "1d"}}, "robinhood", "BTC", "1d"},
		{"default spot", StrategyConfig{ID: "spot-a", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}}, "binanceus", "BTC/USDT", "1h"},
		{"luno spot uses binanceus data", StrategyConfig{ID: "luno-a", Type: "spot", Platform: "luno", Args: []string{"sma", "BTC/USDT", "1h"}}, "binanceus", "BTC/USDT", "1h"},
		{"futures", StrategyConfig{ID: "ts-a", Type: "futures", Platform: "topstep", Args: []string{"momentum", "MES", "15m"}}, "topstep", "MES", "15m"},
		{"manual", StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"hold", "ETH", "1h", "--mode=live"}}, "hyperliquid", "ETH", "1h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := strategyRegimeBundleRequest(tc.sc, rc)
			if !ok {
				t.Fatalf("expected a bundle request")
			}
			if req.Key.Platform != tc.wantPlatform || req.Key.Symbol != tc.wantSymbol || req.Key.Timeframe != tc.wantTF {
				t.Errorf("key = %+v, want %s/%s/%s", req.Key, tc.wantPlatform, tc.wantSymbol, tc.wantTF)
			}
			if req.Key.SpecJSON != regimeWindowsSpecJSON(rc) {
				t.Errorf("spec = %q, want global windows spec", req.Key.SpecJSON)
			}
			if req.AllowSpotFallback {
				t.Errorf("non-options requests must not allow cross-venue fallback")
			}
			if req.MinBars != regimeBundleMinBars {
				t.Errorf("min bars = %d, want %d", req.MinBars, regimeBundleMinBars)
			}
		})
	}
}

func TestStrategyRegimeBundleRequestTimeframeOverride(t *testing.T) {
	rc := testRegimeConfig()
	rc.Timeframe = " 1D "
	sc := StrategyConfig{
		ID:       "hl-a",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"momentum", "BTC", "1h"},
	}

	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		t.Fatalf("expected a bundle request")
	}
	if req.Key.Symbol != "BTC" || req.Key.Timeframe != "1d" {
		t.Fatalf("key = %+v, want BTC/1d", req.Key)
	}
}

func TestStrategyRegimeBundleRequestDisabled(t *testing.T) {
	sc := StrategyConfig{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}
	if _, ok := strategyRegimeBundleRequest(sc, nil); ok {
		t.Error("nil regime config must yield no signature")
	}
	if _, ok := strategyRegimeBundleRequest(sc, &RegimeConfig{Enabled: false}); ok {
		t.Error("disabled regime must yield no signature")
	}
	bad := StrategyConfig{ID: "hl-b", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum"}}
	if _, ok := strategyRegimeBundleRequest(bad, testRegimeConfig()); ok {
		t.Error("missing symbol/timeframe args must yield no signature")
	}
}

func TestStrategyRegimeBundleRequestOptions(t *testing.T) {
	sc := StrategyConfig{ID: "deribit-theta", Type: "options", Platform: "deribit", Args: []string{"theta_harvest", "btc", "--platform=deribit"}}
	rc := testRegimeConfig()
	rc.Timeframe = "1d"
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		t.Fatalf("options strategy must always have a regime signature")
	}
	if req.Key.Platform != "deribit" || req.Key.Symbol != "BTC" || req.Key.Timeframe != optionsRegimeTimeframe {
		t.Errorf("options key = %+v", req.Key)
	}
	if req.Key.SpecJSON != optionsRegimeWindowsSpecJSON {
		t.Errorf("options spec = %q", req.Key.SpecJSON)
	}
	if !req.AllowSpotFallback {
		t.Error("options requests must allow the BinanceUS fallback (parity with check_options)")
	}
	if req.OhlcvLimit != optionsRegimeOhlcvLimit || req.MinBars != optionsRegimeMinBars {
		t.Errorf("options limits = %d/%d", req.OhlcvLimit, req.MinBars)
	}
}

func TestCollectRegimeBundleRequestsDedupesPeers(t *testing.T) {
	rc := testRegimeConfig()
	due := []StrategyConfig{
		{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b", Type: "perps", Platform: "hyperliquid", Args: []string{"mean_reversion", "BTC", "1h"}},
		{ID: "hl-c", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
		{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Args: []string{"hold", "BTC", "1h"}},
		{ID: "deribit-theta", Type: "options", Platform: "deribit", Args: []string{"theta_harvest", "BTC"}},
	}
	reqs := collectRegimeBundleRequests(due, rc)
	if len(reqs) != 3 {
		t.Fatalf("expected 3 distinct signatures, got %d: %+v", len(reqs), reqs)
	}
	if reqs[0].Key.Platform != "deribit" || reqs[1].Key.Symbol != "BTC" || reqs[2].Key.Symbol != "ETH" {
		t.Errorf("unexpected order: %+v", reqs)
	}
}

func TestCollectRegimeBundleRequestsDisabledKeepsOptions(t *testing.T) {
	due := []StrategyConfig{
		{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "deribit-theta", Type: "options", Platform: "deribit", Args: []string{"theta_harvest", "BTC"}},
	}
	reqs := collectRegimeBundleRequests(due, nil)
	if len(reqs) != 1 || reqs[0].Key.Platform != "deribit" {
		t.Fatalf("disabled regime must keep only the options signature, got %+v", reqs)
	}
}

func TestParseRegimeBundleOutput(t *testing.T) {
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: "{}"}
	now := time.Now().UTC()
	raw := `{"platform":"hyperliquid","symbol":"BTC","timeframe":"1h","bar_time":"2026-06-09T12:00:00+00:00",` +
		`"regime":{"default":{"regime":"trending_up","score":0.42,"classifier":"adx","metrics":{"adx":31.5}}},` +
		`"views":{"default":{"adx3":"trending_up","composite7":"trending_up_choppy"}}}`
	b, err := parseRegimeBundleOutput(key, []byte(raw), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := b.Payload.Label("default", nil); got != "trending_up" {
		t.Errorf("label = %q", got)
	}
	if !strings.Contains(b.RawRegimeJSON, `"classifier":"adx"`) {
		t.Errorf("raw payload lost subprocess fields: %s", b.RawRegimeJSON)
	}
	if b.Views["default"].Composite7 != "trending_up_choppy" {
		t.Errorf("views = %+v", b.Views)
	}
	if b.BarTime != "2026-06-09T12:00:00+00:00" {
		t.Errorf("bar_time = %q", b.BarTime)
	}
}

func TestParseRegimeBundleOutputErrors(t *testing.T) {
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h"}
	now := time.Now().UTC()
	cases := map[string]string{
		"script error":   `{"regime":null,"error":"Insufficient data: 3 candles (need 30)"}`,
		"missing regime": `{"platform":"hyperliquid"}`,
		"empty regime":   `{"regime":{}}`,
		"bad json":       `not-json`,
	}
	for name, raw := range cases {
		if _, err := parseRegimeBundleOutput(key, []byte(raw), now); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func stubRegimeBundleCheck(t *testing.T, fn func(context.Context, regimeBundleRequest) (*RegimeBundle, error)) {
	t.Helper()
	stubRegimeBundleCheckWithMarket(t, func(ctx context.Context, req regimeBundleRequest, _ []byte) (*RegimeBundle, error) {
		return fn(ctx, req)
	})
}

func stubRegimeBundleCheckWithMarket(t *testing.T, fn func(context.Context, regimeBundleRequest, []byte) (*RegimeBundle, error)) {
	t.Helper()
	orig := runRegimeBundleCheckFn
	runRegimeBundleCheckFn = fn
	t.Cleanup(func() { runRegimeBundleCheckFn = orig })
}

func TestPopulateRegimeStoreSharesBundleAcrossPeers(t *testing.T) {
	rc := testRegimeConfig()
	due := []StrategyConfig{
		{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b", Type: "perps", Platform: "hyperliquid", Args: []string{"mean_reversion", "BTC", "1h"}},
	}
	var calls int
	stubRegimeBundleCheck(t, func(_ context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
		calls++
		payload := RegimePayload{Legacy: "trending_up"}
		return &RegimeBundle{Key: req.Key, Payload: payload, RawRegimeJSON: `"trending_up"`, At: time.Now().UTC()}, nil
	})
	store := &RegimeStore{}
	populateRegimeStore(store, due, rc, nil, nil)
	if calls != 1 {
		t.Fatalf("two peers sharing a signature must compute ONCE, got %d calls", calls)
	}
	for _, sc := range due {
		if got := store.PayloadForStrategy(sc, rc).Label("", rc); got != "trending_up" {
			t.Errorf("%s: label = %q, want trending_up", sc.ID, got)
		}
	}
}

func TestPopulateRegimeStoreFailureYieldsEmptyPayload(t *testing.T) {
	rc := testRegimeConfig()
	sc := StrategyConfig{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}
	stubRegimeBundleCheck(t, func(_ context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
		return nil, fmt.Errorf("regime bundle %s: boom", req.Key)
	})
	store := &RegimeStore{}
	populateRegimeStore(store, []StrategyConfig{sc}, rc, nil, nil)

	if payload := store.PayloadForStrategy(sc, rc); !payload.IsEmpty() {
		t.Errorf("failed bundle must yield an empty payload, got %+v", payload)
	}
	if label, blocked := applyRegimeGate(StrategyConfig{AllowedRegimes: []string{"trending_up"}}, store.PayloadForStrategy(sc, rc), rc, 0); blocked {
		t.Errorf("entry gate must FAIL OPEN on empty label, blocked with label %q", label)
	}
	raw, ok := store.InjectionJSONForStrategy(sc, rc)
	if !ok || raw != "" {
		t.Errorf("injection = (%q, %v), want empty-value flag presence", raw, ok)
	}
}

func TestInjectionJSONOmittedWhenRegimeDisabled(t *testing.T) {
	sc := StrategyConfig{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}
	store := &RegimeStore{}
	if _, ok := store.InjectionJSONForStrategy(sc, nil); ok {
		t.Error("no signature (regime disabled) must omit the flag so manual CLI runs keep the inline path")
	}
}

func TestPopulateRegimeStoreClearsPriorCycle(t *testing.T) {
	rc := testRegimeConfig()
	sc := StrategyConfig{ID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}
	ok := true
	stubRegimeBundleCheck(t, func(_ context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
		if !ok {
			return nil, fmt.Errorf("outage")
		}
		return &RegimeBundle{Key: req.Key, Payload: RegimePayload{Legacy: "trending_up"}, RawRegimeJSON: `"trending_up"`, At: time.Now().UTC()}, nil
	})
	store := &RegimeStore{}
	populateRegimeStore(store, []StrategyConfig{sc}, rc, nil, nil)
	if store.PayloadForStrategy(sc, rc).IsEmpty() {
		t.Fatal("first cycle should have a payload")
	}
	ok = false
	populateRegimeStore(store, []StrategyConfig{sc}, rc, nil, nil)
	if !store.PayloadForStrategy(sc, rc).IsEmpty() {
		t.Error("failed cycle must not serve the previous cycle's label")
	}
}

func TestRegimeStoreSetAfterSealDiscards(t *testing.T) {
	store := &RegimeStore{}
	gen := store.resetForCycle(time.Now().UTC())
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h"}
	if kept := store.seal(); kept != 0 {
		t.Fatalf("seal kept = %d", kept)
	}
	store.set(&RegimeBundle{Key: key, Payload: RegimePayload{Legacy: "trending_up"}}, gen)
	if _, ok := store.get(key); ok {
		t.Error("a sealed store must discard late bundles (no mid-cycle flips)")
	}
	gen2 := store.resetForCycle(time.Now().UTC())
	store.set(&RegimeBundle{Key: key, Payload: RegimePayload{Legacy: "trending_up"}}, gen2)
	if _, ok := store.get(key); !ok {
		t.Error("resetForCycle must clear the seal for the new generation")
	}
}

func TestRegimeStoreStaleGenerationWriteDropped(t *testing.T) {
	store := &RegimeStore{}
	genN := store.resetForCycle(time.Now().UTC())
	store.seal()
	genN1 := store.resetForCycle(time.Now().UTC())
	if genN1 == genN {
		t.Fatal("resetForCycle must advance the generation")
	}
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h"}
	store.set(&RegimeBundle{Key: key, Payload: RegimePayload{Legacy: "trending_up"}}, genN)
	if _, ok := store.get(key); ok {
		t.Error("stale-generation straggler write must be dropped after the next cycle's reset")
	}
	store.set(&RegimeBundle{Key: key, Payload: RegimePayload{Legacy: "trending_down"}}, genN1)
	if b, ok := store.get(key); !ok || b.Payload.Legacy != "trending_down" {
		t.Error("current-generation write must land")
	}
}

func TestRegimeStorePhaseBudgetSealsStragglers(t *testing.T) {
	origBudget := regimeStorePhaseBudget
	regimeStorePhaseBudget = 50 * time.Millisecond
	t.Cleanup(func() { regimeStorePhaseBudget = origBudget })

	rc := testRegimeConfig()
	fast := StrategyConfig{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}}
	slow := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}}
	release := make(chan struct{})
	stubRegimeBundleCheck(t, func(_ context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
		if req.Key.Symbol == "ETH" {
			<-release
		}
		return &RegimeBundle{Key: req.Key, Payload: RegimePayload{Legacy: "trending_up"}, RawRegimeJSON: `"trending_up"`, At: time.Now().UTC()}, nil
	})

	store := &RegimeStore{}
	wait := startRegimeStorePopulation(store, []StrategyConfig{fast, slow}, rc, nil, nil)
	start := time.Now()
	wait()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait exceeded the phase budget by far: %s", elapsed)
	}
	if store.PayloadForStrategy(fast, rc).IsEmpty() {
		t.Error("fast signature should have landed before the budget expired")
	}
	if !store.PayloadForStrategy(slow, rc).IsEmpty() {
		t.Error("hung signature must fail open this cycle")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if !store.PayloadForStrategy(slow, rc).IsEmpty() {
		t.Error("straggler result after seal must be discarded, not applied mid-cycle")
	}
}

func TestRegimeBundleCheckArgs(t *testing.T) {
	req := regimeBundleRequest{
		Key:        regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: `{"default":{"period":14}}`},
		OhlcvLimit: 200,
		MinBars:    30,
	}
	args := strings.Join(regimeBundleCheckArgs(req), " ")
	for _, want := range []string{"--platform hyperliquid", "--symbol BTC", "--timeframe 1h", "--ohlcv-limit 200", "--min-bars 30"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--allow-spot-fallback") {
		t.Error("non-options request must not allow spot fallback")
	}
	req.AllowSpotFallback = true
	if !strings.Contains(strings.Join(regimeBundleCheckArgs(req), " "), "--allow-spot-fallback") {
		t.Error("options request must pass --allow-spot-fallback")
	}
}

func TestUIRegimeEntriesProjection(t *testing.T) {
	store := &RegimeStore{}
	gen := store.resetForCycle(time.Now().UTC())
	payloadJSON := `{"medium":{"regime":"trending_up","score":0.4,"metrics":{"adx":28.0}}}`
	var payload RegimePayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	store.set(&RegimeBundle{
		Key:           regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h"},
		Payload:       payload,
		RawRegimeJSON: payloadJSON,
		Views:         map[string]RegimeBundleViews{"medium": {ADX3: "trending_up", Composite7: "trending_up_clean"}},
		BarTime:       "2026-06-09T12:00:00+00:00",
		At:            time.Now().UTC(),
	}, gen)
	entries, _ := uiRegimeEntries(store)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	w := entries[0].Windows["medium"]
	if w.Regime != "trending_up" || w.ADX3 != "trending_up" || w.Composite7 != "trending_up_clean" {
		t.Errorf("window = %+v", w)
	}
	if entries[0].Platform != "hyperliquid" || entries[0].BarTime == "" {
		t.Errorf("entry = %+v", entries[0])
	}
}
