//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fcPyConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	strat := func(id, name string, closed bool) map[string]any {
		return map[string]any{
			"id": id, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []any{name, "BTC", "1h", "--mode=paper"}, "capital": 1000, "leverage": 1, "max_drawdown_pct": 90,
			"closed_bar_decisions": closed, "close_strategy": map[string]any{"name": "tiered_tp_pct"},
		}
	}
	blob, err := json.Marshal(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 300,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "market_feed": "websocket",
		"strategies": []any{strat("skew-now", "funding_skew", false), strat("skew-closed", "funding_skew", true), strat("peer", "breakout", false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatalf("LoadConfigReadOnly: %v", err)
	}
	return cfg
}

func TestFeedFundingProofRealCheckScript(t *testing.T) {
	root := pyRepoRoot(t)
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	t.Setenv(hlBatchDisabledEnv, "")
	fixture := filepath.Join(t.TempDir(), "offline.json")
	if err := os.WriteFile(fixture, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	htfITUseFixture(t, root, fixture)
	cfg := fcPyConfig(t)
	t.Chdir(root)
	var req feedRequirements
	t0 := rdBase() + 3600
	rd, _ := newRDFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer {
		derived, err := deriveFeedRequirements(cfg)
		if err != nil {
			t.Fatal(err)
		}
		req = derived
		return []feedConsumer{{Path: "/py.json", Loaded: true, Req: req, Cadences: feedConsumerCadences(cfg)}}
	})
	f := &fcFixture{rdFixture: rd}
	f.enforce(200, 100, 900, 100, 200)
	var doc *feedSealDoc
	for d := t0 - 600; d <= t0; d += 75 {
		doc, _ = f.seal(d)
	}
	byID := map[string]StrategyConfig{}
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var log bytes.Buffer
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&log, format+"\n", a...)
	}
	logger := func(id string) *StrategyLogger { return &StrategyLogger{stratID: id, writer: &log} }
	feedFor := func(mutate func(e *feedFunding)) *marketFeedContext {
		snap := doc.snapshot()
		e := snap.funding["BTC"]
		mutate(&e)
		snap.funding["BTC"] = e
		return &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: cfg.IntervalSeconds}
	}
	prices := map[string]float64{"BTC": 101}
	check := func(feed *marketFeedContext, id string) *HyperliquidResult {
		t.Helper()
		sc := byID[id]
		res, _, _, ok := runHyperliquidCheck(&sc, prices, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), logger(id), nil, feed)
		if !ok || res == nil {
			t.Fatalf("%s check failed:\n%s", id, log.String())
		}
		return res
	}
	inputs := func(res *HyperliquidResult) map[string]any {
		out := map[string]any{"signal": res.Signal, "open_action": res.OpenAction}
		for k, v := range res.Indicators {
			out[k] = v
		}
		if res.ClosedBar != nil {
			out["closed"] = *res.ClosedBar
		}
		return out
	}
	proved := feedFor(func(*feedFunding) {})
	reference := feedFor(func(e *feedFunding) {
		from := (t0 - 3000*3600) * 1000
		e.Records, e.RecordsFromMs = f.truth("BTC", from, e.RecordsToMs), from
	})
	single := map[string]map[string]any{}
	for _, id := range []string{"skew-now", "skew-closed", "peer"} {
		got, want := inputs(check(proved, id)), inputs(check(reference, id))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s decision inputs = %v, the full-range reference gives %v", id, got, want)
		}
		single[id] = got
	}
	for _, id := range []string{"skew-now", "skew-closed"} {
		if v, ok := single[id]["funding_z"]; !ok || v == nil {
			t.Fatalf("%s inputs %v carry no funding_z, so the parity proves nothing", id, single[id])
		}
	}

	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	mu := &sync.RWMutex{}
	batch := runHyperliquidBatchPrePass(cfg.Strategies, state, mu, cfg, prices, NewMultiNotifier(), logf, proved)
	for id := range byID {
		out, ok := batch.lookup(id)
		if !ok || out.Err != "" || out.Result == nil {
			t.Fatalf("batch slot %s = %+v\n%s", id, out, log.String())
		}
		if got := inputs(out.Result); got["signal"] != single[id]["signal"] || !reflect.DeepEqual(got["funding_z"], single[id]["funding_z"]) {
			t.Fatalf("batch %s inputs = %v, single %v", id, got, single[id])
		}
	}

	for name, mutate := range map[string]func(e *feedFunding){
		"missing proof": func(e *feedFunding) {
			e.RecordsContract, e.RecordsFromMs, e.RecordsToMs, e.RecordsThroughMs = 0, 0, 0, 0
		},
		"short range": func(e *feedFunding) { e.RecordsFromMs = (t0 - 50*3600) * 1000 },
		"stale read":  func(e *feedFunding) { e.RecordsToMs -= 2 * feedFundingTTL.Milliseconds() },
	} {
		log.Reset()
		broken := feedFor(mutate)
		batch := runHyperliquidBatchPrePass(cfg.Strategies, state, mu, cfg, prices, NewMultiNotifier(), logf, broken)
		if _, ok := batch.lookup("peer"); ok || !strings.Contains(log.String(), "fall back to their own sealed-snapshot checks") {
			t.Fatalf("%s: the batch must fall back to per-strategy checks:\n%s", name, log.String())
		}
		for _, id := range []string{"skew-now", "skew-closed"} {
			if res := check(broken, id); res.Degraded == "" || res.Signal != 0 {
				t.Fatalf("%s: %s = %+v, want a degraded result with no signal", name, id, res)
			}
		}
		peer := check(broken, "peer")
		if peer.Degraded != "" || !reflect.DeepEqual(inputs(peer), single["peer"]) {
			t.Fatalf("%s: the peer = %+v, want its normal evaluation", name, inputs(peer))
		}
	}

	slot := func(id string, mutate func(m *marketFundingPayload)) (*HyperliquidBatchResult, string) {
		t.Helper()
		key, ok := hlBatchKeyForStrategy(byID[id], cfg)
		if !ok {
			t.Fatalf("%s is not batch eligible", id)
		}
		payload, err := proved.batchPayload(key, cfg.Strategies)
		if err != nil {
			t.Fatal(err)
		}
		entry := payload.Funding["BTC"]
		mutate(&entry)
		payload.Funding["BTC"] = entry
		breq := hlBatchRequest{Version: hlBatchProtocolVersionMarket, Market: payload}
		for _, sc := range cfg.Strategies {
			s, err := buildHyperliquidBatchSlot(sc, PositionCtx{}, cfg.Regime)
			if err != nil {
				t.Fatal(err)
			}
			breq.Slots = append(breq.Slots, s)
		}
		stdin, err := json.Marshal(breq)
		if err != nil {
			t.Fatal(err)
		}
		res, stderr, _ := runHyperliquidBatchCheck(hlBatchGroupPriority(cfg.Strategies), hyperliquidCheckScript,
			hlBatchSharedArgs(key, cfg.Regime, "", false, 101, true, "1h"), stdin)
		if res == nil {
			t.Fatalf("no batch result: %s", stderr)
		}
		return res, stderr
	}
	for name, mutate := range map[string]func(m *marketFundingPayload){
		"absent contract":   func(m *marketFundingPayload) { m.RecordsContract = 0 },
		"malformed bounds":  func(m *marketFundingPayload) { m.RecordsThroughMs = m.RecordsToMs - 1 },
		"frame not covered": func(m *marketFundingPayload) { m.RecordsFromMs = m.RecordsToMs - 1000 },
		"records error":     func(m *marketFundingPayload) { m.RecordsError = "loading" },
		"unordered records": func(m *marketFundingPayload) { m.Records = append([]feedFundingRecord{m.Records[5]}, m.Records...) },
		"record past to_ms": func(m *marketFundingPayload) {
			m.Records = append(m.Records, feedFundingRecord{TimeMs: m.RecordsToMs + 1})
		},
		"boundary not closed": func(m *marketFundingPayload) {
			m.RecordsToMs, m.RecordsThroughMs = t0*1000-1, t0*1000-1
			m.Records = m.Records[:len(m.Records)-1]
			if t0*1000-1 < m.RecordsFromMs {
				t.Fatal("fixture range")
			}
		},
	} {
		res, _ := slot("peer", mutate)
		byResult := map[string]HyperliquidBatchSlotResult{}
		for _, r := range res.Results {
			byResult[r.ID] = r
		}
		if peer := byResult["peer"]; peer.Error != "" {
			t.Fatalf("%s: the peer slot failed: %s", name, peer.Error)
		}
		if closed := byResult["skew-closed"]; closed.Error != "" || closed.ClosedBar == nil || !closed.ClosedBar.Held || closed.Signal != 0 {
			t.Fatalf("%s: closed slot = %+v, want a held decision", name, closed)
		}
		if now := byResult["skew-now"]; name != "boundary not closed" && now.Error == "" {
			t.Fatalf("%s: the current-bar slot evaluated: %+v", name, now.Indicators)
		}
	}
}
