//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rsITSnap struct {
	Spike    string
	HTFTrend string
}

func rsITSnapshot(s rsITSnap) (*marketSnapshot, float64) {
	sealedAt := time.Now().UTC()
	deadline := sealedAt.Truncate(time.Hour)
	snap := &marketSnapshot{
		Version:      marketSnapshotVersion,
		EvaluationID: fmt.Sprintf("rs-it/%d", sealedAt.UnixNano()),
		SealedAt:     sealedAt,
		keys:         make(map[marketFeedKey]*marketSnapshotKey),
		mids:         make(map[string]feedMid),
		funding:      make(map[string]feedFunding),
		observations: make(map[feedObservationKey]*marketSnapshotObservation),
	}
	mark := 0.0
	add := func(tf string, interval int64, total int, spike, trend string) {
		key := feedKeyFor("BTC", tf)
		entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
		formingOpen := deadline.UnixMilli() - deadline.UnixMilli()%interval
		start := formingOpen - int64(total-1)*interval
		p := 100.0
		for i := 0; i < total; i++ {
			switch trend {
			case "up":
				p += 0.5
			case "down":
				p -= 0.2
			default:
				if i%2 == 0 {
					p += 0.4
				} else {
					p -= 0.35
				}
			}
			open := start + int64(i)*interval
			b := feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: i < total-1, Open: p - 0.1, High: p + 0.5, Low: p - 0.5, Close: p, Volume: 1000 + float64(i), RecvAt: sealedAt}
			if i == total-1 {
				switch spike {
				case "up":
					b.High, b.Close = p+12.5, p+12
				case "down":
					b.Low, b.Close = p-12.5, p-12
				}
			}
			entry.Bars = append(entry.Bars, b)
		}
		entry.Readiness = feedKeyReadiness{Key: key, Bars: total, Required: total, Ready: true, Status: feedStatusReady, Source: "test", LastRecvAt: sealedAt}
		snap.keys[key] = entry
		if tf == "1h" {
			mark = entry.Bars[total-1].Close
		}
	}
	add("1h", htfITHour, 260, s.Spike, "")
	add("4h", 4*htfITHour, 80, "", s.HTFTrend)
	snap.mids["BTC"] = feedMid{Px: mark, RecvAt: sealedAt, Source: "test"}
	return snap, mark
}

func rsITStrategy(id, mode, direction string, htf bool, extra map[string]any) map[string]any {
	out := map[string]any{
		"id": id, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
		"args": []any{"breakout", "BTC", "1h", "--mode=" + mode}, "capital": 1000, "leverage": 1, "max_drawdown_pct": 90,
		"htf_filter": htf, "regime_side": true, "direction": direction,
		"regime_directional_window": "macro",
		"close_strategy":            map[string]any{"name": "tiered_tp_pct"},
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func rsITConfigBytes(t *testing.T, dir string, strategies ...map[string]any) []byte {
	t.Helper()
	list := make([]any, 0, len(strategies))
	for _, s := range strategies {
		list = append(list, s)
	}
	blob, err := json.Marshal(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 3600,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "market_feed": "websocket",
		"regime": map[string]any{"enabled": true, "windows": map[string]any{
			"medium": map[string]any{"classifier": "composite", "period": 20},
			"macro":  map[string]any{"classifier": "composite", "period": 40},
		}},
		"strategies": list,
	})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func rsITLoad(t *testing.T, strategies ...map[string]any) *Config {
	t.Helper()
	return rsITLoadIn(t, t.TempDir(), strategies...)
}

func rsITLoadIn(t *testing.T, dir string, strategies ...map[string]any) *Config {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, rsITConfigBytes(t, dir, strategies...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatalf("LoadConfigReadOnly: %v", err)
	}
	return cfg
}

func rsITSetRegime(t *testing.T, cfg *Config, macroLabel string, macroReady bool) string {
	t.Helper()
	gen := globalRegimeStore.resetForCycle(time.Now())
	t.Cleanup(func() { globalRegimeStore.resetForCycle(time.Now()) })
	if macroLabel == "" {
		return ""
	}
	window := func(label string, period int, ready bool) map[string]any {
		need := regimeRequiredBars(period, "composite")
		bars := need
		if !ready {
			bars, label = need-3, ""
		}
		return map[string]any{"regime": label, "score": 0.5, "classifier": "composite", "ready": ready, "bars": bars, "need": need, "metrics": map[string]any{"adx": 30.0}}
	}
	raw, err := json.Marshal(map[string]any{
		"medium": window("ranging_quiet", 20, true),
		"macro":  window(macroLabel, 40, macroReady),
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload RegimePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	seen := map[regimeBundleKey]bool{}
	for _, sc := range cfg.Strategies {
		req, ok := strategyRegimeBundleRequest(sc, cfg.Regime)
		if !ok {
			t.Fatalf("no regime bundle request for %s", sc.ID)
		}
		if seen[req.Key] {
			continue
		}
		seen[req.Key] = true
		globalRegimeStore.set(&RegimeBundle{Key: req.Key, Payload: payload, RawRegimeJSON: string(raw), At: time.Now()}, gen)
	}
	return string(raw)
}

type rsITEnv struct {
	cfg    *Config
	feed   *marketFeedContext
	prices map[string]float64
	log    *bytes.Buffer
}

func rsITSetup(t *testing.T, snap rsITSnap, strategies ...map[string]any) rsITEnv {
	t.Helper()
	root := htfITRoot(t)
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	resetHTFFilterTrackersForTest(t)
	t.Setenv(hlBatchDisabledEnv, "")
	fixture := filepath.Join(t.TempDir(), "offline.json")
	if err := os.WriteFile(fixture, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	htfITUseFixture(t, root, fixture)
	cfg := rsITLoad(t, strategies...)
	t.Chdir(root)
	s, mark := rsITSnapshot(snap)
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rsITEnv{cfg: cfg, feed: &marketFeedContext{Enabled: true, Requirements: req, Snapshot: s, Interval: cfg.IntervalSeconds}, prices: map[string]float64{"BTC": mark}, log: &bytes.Buffer{}}
}

func (e rsITEnv) logger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: e.log}
}

func (e rsITEnv) single(t *testing.T, id string, posCtx PositionCtx) *HyperliquidResult {
	t.Helper()
	for _, sc := range e.cfg.Strategies {
		if sc.ID == id {
			res, _, _, ok := runHyperliquidCheck(&sc, e.prices, posCtx, e.cfg.Regime, "simple", NewMultiNotifier(), e.logger(id), nil, e.feed)
			if !ok {
				return nil
			}
			return res
		}
	}
	t.Fatalf("no strategy %s", id)
	return nil
}

func rsITEcho(res *HyperliquidResult) string {
	if res == nil || res.OpenSideApplied == nil {
		return "<absent>"
	}
	return *res.OpenSideApplied
}

func TestRegimeSideIntegration(t *testing.T) {
	t.Run("EitherTriggerOpensTheRegimeSideSingleAndBatch", func(t *testing.T) {
		for _, tc := range []struct {
			spike, label, wantSide string
			wantSignal             int
		}{
			{"up", "trending_down_choppy", "short", -1},
			{"down", "trending_down_choppy", "short", -1},
			{"up", "trending_up_clean", "long", 1},
			{"down", "trending_up_clean", "long", 1},
		} {
			t.Run(tc.spike+"_"+tc.label, func(t *testing.T) {
				env := rsITSetup(t, rsITSnap{Spike: tc.spike}, rsITStrategy("rs-a", "paper", "both", false, nil), rsITStrategy("rs-b", "paper", "both", false, nil))
				rsITSetRegime(t, env.cfg, tc.label, true)
				res := env.single(t, "rs-a", PositionCtx{})
				if res == nil || res.Signal != tc.wantSignal || rsITEcho(res) != tc.wantSide || res.RegimeSide.Label != tc.label || res.RegimeSide.Window != "macro" {
					t.Fatalf("single: want signal %d side %s, got %+v echo=%s side=%+v\n%s", tc.wantSignal, tc.wantSide, res, rsITEcho(res), res.RegimeSide, env.log.String())
				}
				store := openSourceStore(t, env.cfg)
				state := NewAppState()
				for _, sc := range env.cfg.Strategies {
					state.Strategies[sc.ID] = NewStrategyState(sc)
				}
				mu := &sync.RWMutex{}
				batch := runHyperliquidBatchPrePass(env.cfg.Strategies, state, mu, env.cfg, env.prices, NewMultiNotifier(), func(format string, a ...any) { fmt.Fprintf(env.log, format+"\n", a...) }, env.feed)
				for _, id := range []string{"rs-a", "rs-b"} {
					out, ok := batch.lookup(id)
					if !ok || out.Result == nil || out.Result.Signal != tc.wantSignal || rsITEcho(out.Result) != tc.wantSide || out.RegimeSide.Side != tc.wantSide {
						t.Fatalf("batch %s: want signal %d side %s, got %+v\n%s", id, tc.wantSignal, tc.wantSide, out, env.log.String())
					}
				}
				for _, sc := range env.cfg.Strategies {
					htfITHLStep(t, env.cfg, sc, state, store, mu, env.prices, env.feed, batch, env.logger(sc.ID))
				}
				if strings.Contains(env.log.String(), "Batched check inputs changed") {
					t.Fatalf("the steps must consume the batch results\n%s", env.log.String())
				}
				for _, id := range []string{"rs-a", "rs-b"} {
					mu.RLock()
					pos := state.Strategies[id].Positions["BTC"]
					var opens []Trade
					for _, tr := range state.Strategies[id].TradeHistory {
						if !tr.IsClose {
							opens = append(opens, tr)
						}
					}
					mu.RUnlock()
					if pos == nil || pos.Quantity <= 0 || pos.Side != tc.wantSide || len(opens) != 1 {
						t.Fatalf("%s must open one %s position, got %+v opens=%d\n%s", id, tc.wantSide, pos, len(opens), env.log.String())
					}
					want := fmt.Sprintf("Regime side: %s window macro -> %s", tc.label, tc.wantSide)
					if opens[0].RegimeSideNote != want {
						t.Fatalf("%s entry alert note = %q, want %q", id, opens[0].RegimeSideNote, want)
					}
					extras := strings.Join(tradeAlertExtras(env.cfg.Strategies[0], opens[0], false, env.cfg.Regime), " | ")
					if !strings.Contains(extras, want) {
						t.Fatalf("entry alert extras %q must carry the entry selection", extras)
					}
				}
			})
		}
	})

	t.Run("NonDirectionalWarmingUnavailableAndZeroTriggersHold", func(t *testing.T) {
		for _, tc := range []struct {
			name, spike, label string
			ready              bool
			wantEcho           string
			holdHas            string
		}{
			{"ranging_quiet", "up", "ranging_quiet", true, "none", "selects no side"},
			{"bare_ranging_directional", "up", "ranging_directional", true, "none", "selects no side"},
			{"warming", "up", "trending_down_clean", false, "none", "warming"},
			{"unavailable", "up", "", true, "none", "unavailable"},
			{"zero_trigger", "", "trending_up_clean", true, "long", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := rsITSetup(t, rsITSnap{Spike: tc.spike}, rsITStrategy("rs-a", "paper", "both", false, nil))
				rsITSetRegime(t, env.cfg, tc.label, tc.ready)
				res := env.single(t, "rs-a", PositionCtx{})
				if res == nil || res.Signal != 0 || rsITEcho(res) != tc.wantEcho || !strings.Contains(res.RegimeSide.HoldReason, tc.holdHas) {
					t.Fatalf("want a held entry with echo %s and hold %q, got %+v echo=%s side=%+v\n%s", tc.wantEcho, tc.holdHas, res, rsITEcho(res), res.RegimeSide, env.log.String())
				}
			})
		}
	})

	t.Run("HigherTimeframeFilterJudgesTheMappedSide", func(t *testing.T) {
		t.Run("RawDisagreesMappedAgrees", func(t *testing.T) {
			env := rsITSetup(t, rsITSnap{Spike: "up", HTFTrend: "down"}, rsITStrategy("rs-a", "paper", "both", true, nil))
			rsITSetRegime(t, env.cfg, "trending_down_clean", true)
			res := env.single(t, "rs-a", PositionCtx{})
			if res == nil || res.Signal != -1 || res.HTFFilter == nil || res.HTFFilter.Trend == nil || *res.HTFFilter.Trend != -1 {
				t.Fatalf("a raw long trigger mapped short in an HTF downtrend must open short, got %+v\n%s", res, env.log.String())
			}
		})
		t.Run("RawAgreesMappedDisagrees", func(t *testing.T) {
			env := rsITSetup(t, rsITSnap{Spike: "up", HTFTrend: "up"}, rsITStrategy("rs-a", "paper", "both", true, nil))
			rsITSetRegime(t, env.cfg, "trending_down_clean", true)
			res := env.single(t, "rs-a", PositionCtx{})
			if res == nil || res.Signal != 0 || res.HTFFilter == nil || res.HTFFilter.Trend == nil || *res.HTFFilter.Trend != 1 || rsITEcho(res) != "short" {
				t.Fatalf("a raw long trigger mapped short in an HTF uptrend must hold, got %+v\n%s", res, env.log.String())
			}
		})
	})

	t.Run("BaseDirectionStaysAPermissionFilter", func(t *testing.T) {
		for _, tc := range []struct {
			direction, label string
			wantSignal       int
		}{
			{"long", "trending_down_clean", 0},
			{"long", "trending_up_clean", 1},
			{"short", "trending_up_clean", 0},
			{"short", "trending_down_clean", -1},
			{"both", "trending_down_clean", -1},
		} {
			t.Run(tc.direction+"_"+tc.label, func(t *testing.T) {
				env := rsITSetup(t, rsITSnap{Spike: "up"}, rsITStrategy("rs-a", "paper", tc.direction, false, nil))
				rsITSetRegime(t, env.cfg, tc.label, true)
				res := env.single(t, "rs-a", PositionCtx{})
				if res == nil || res.Signal != tc.wantSignal {
					t.Fatalf("direction %s label %s: want signal %d, got %+v\n%s", tc.direction, tc.label, tc.wantSignal, res, env.log.String())
				}
			})
		}
	})

	t.Run("OpenPositionKeepsItsSideAndCannotAddOrReopen", func(t *testing.T) {
		env := rsITSetup(t, rsITSnap{Spike: "up"}, rsITStrategy("rs-a", "paper", "both", false, map[string]any{"allow_scale_in": true}))
		rsITSetRegime(t, env.cfg, "trending_up_clean", true)
		store := openSourceStore(t, env.cfg)
		state := NewAppState()
		sc := env.cfg.Strategies[0]
		s := NewStrategyState(sc)
		state.Strategies[sc.ID] = s
		mark := env.prices["BTC"]
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, InitialQuantity: 1, AvgCost: mark, Side: "short", Leverage: 1, EntryATR: 1, OwnerStrategyID: sc.ID, OpenedAt: time.Now().Add(-2 * time.Hour), StopLossTriggerPx: mark - 1}
		s.Cash -= mark
		mu := &sync.RWMutex{}
		htfITHLStep(t, env.cfg, sc, state, store, mu, env.prices, env.feed, nil, env.logger(sc.ID))
		mu.RLock()
		pos := s.Positions["BTC"]
		opens := 0
		for _, tr := range s.TradeHistory {
			if !tr.IsClose {
				opens++
			}
		}
		mu.RUnlock()
		if opens != 0 {
			t.Fatalf("a decision that started with an open short must not open, add or reopen; opens=%d pos=%+v\n%s", opens, pos, env.log.String())
		}
		if pos != nil && pos.Quantity > 0 && pos.Side != "short" {
			t.Fatalf("the open position must keep its side under an up regime, got %+v", pos)
		}
		if pos != nil && pos.Quantity > 0 {
			t.Fatalf("the paper stop above the mark must close the short this decision, got %+v\n%s", pos, env.log.String())
		}
		if !strings.Contains(env.log.String(), "position open: regime_side opens only from a flat start") {
			t.Fatalf("the open-position hold must be reported\n%s", env.log.String())
		}
	})

	t.Run("ContractErrorsHoldTheSignal", func(t *testing.T) {
		root := htfITRoot(t)
		t.Chdir(root)
		stub := filepath.Join(t.TempDir(), "rs_stub.py")
		if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := rsITLoad(t, rsITStrategy("rs-a", "paper", "both", false, nil))
		rawRegime := rsITSetRegime(t, cfg, "trending_up_clean", true)
		sc := cfg.Strategies[0]
		sc.Script = stub
		base := map[string]any{"strategy": "breakout", "symbol": "BTC", "timeframe": "1h", "signal": 1, "price": 100,
			"indicators": map[string]any{}, "mode": "paper", "platform": "hyperliquid", "close_fraction": 0, "regime": json.RawMessage(rawRegime)}
		run := func(sc StrategyConfig, payload map[string]any) (bool, *HyperliquidResult, string) {
			blob, _ := json.Marshal(payload)
			t.Setenv("HTF_IT_STUB_JSON", string(blob))
			t.Setenv("HTF_IT_STUB_EXIT", "0")
			var buf bytes.Buffer
			res, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 100}, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), &StrategyLogger{stratID: sc.ID, writer: &buf}, nil, nil)
			return ok, res, buf.String()
		}
		if ok, _, log := run(sc, base); ok || !strings.Contains(log, "returned no open_side_applied echo") {
			t.Fatalf("a missing echo must be a script error, ok=%t\n%s", ok, log)
		}
		if ok, _, log := run(sc, htfITWith(base, map[string]any{"open_side_applied": "short"})); ok || !strings.Contains(log, `returned open_side_applied="short"`) {
			t.Fatalf("a mismatched echo must be a script error, ok=%t\n%s", ok, log)
		}
		if ok, res, log := run(sc, htfITWith(base, map[string]any{"open_side_applied": "long"})); !ok || res == nil || res.Signal != 1 {
			t.Fatalf("a matching echo must pass, ok=%t res=%+v\n%s", ok, res, log)
		}
		var flipped map[string]any
		if err := json.Unmarshal([]byte(rawRegime), &flipped); err != nil {
			t.Fatal(err)
		}
		flipped["macro"].(map[string]any)["regime"] = "trending_down_clean"
		if ok, res, log := run(sc, htfITWith(base, map[string]any{"open_side_applied": "long", "regime": flipped})); !ok || res == nil || res.Signal != 0 || !strings.Contains(log, `selects "short"`) {
			t.Fatalf("a returned regime that selects another side must hold the entry for the cycle, ok=%t res=%+v\n%s", ok, res, log)
		}
		closing := htfITWith(base, map[string]any{"open_side_applied": "none", "regime": flipped, "signal": 1, "close_fraction": 1})
		posCtx := PositionCtx{Side: "short", Quantity: 1, AvgCost: 100}
		blob, _ := json.Marshal(closing)
		t.Setenv("HTF_IT_STUB_JSON", string(blob))
		var cbuf bytes.Buffer
		openSC := sc
		cres, _, _, cok := runHyperliquidCheck(&openSC, map[string]float64{"BTC": 100}, posCtx, cfg.Regime, "simple", NewMultiNotifier(), &StrategyLogger{stratID: sc.ID, writer: &cbuf}, nil, nil)
		if !cok || cres == nil || cres.CloseFraction != 1 || cres.Signal != 1 {
			t.Fatalf("a regime transition must leave an open position's close work intact, ok=%t res=%+v\n%s", cok, cres, cbuf.String())
		}
		off := sc
		off.RegimeSide = false
		if ok, _, log := run(off, htfITWith(base, map[string]any{"open_side_applied": "long"})); ok || !strings.Contains(log, "no open_side was sent") {
			t.Fatalf("an unexpected echo must be a script error, ok=%t\n%s", ok, log)
		}
		if ok, res, log := run(off, base); !ok || res == nil || res.OpenSideApplied != nil {
			t.Fatalf("feature off keeps the current contract, ok=%t\n%s", ok, log)
		}
		refs, err := buildStrategyRefsArgWithSide(off, "", false, "")
		if err != nil || strings.Contains(strings.Join(refs, " "), "open_side") {
			t.Fatalf("feature off must send no open_side, got %v err=%v", refs, err)
		}
		if _, err := buildStrategyRefsArgWithSide(sc, "", false, "sideways"); err == nil {
			t.Fatal("an invalid request must not be encoded")
		}
	})

	t.Run("PythonRejectsMalformedRequests", func(t *testing.T) {
		env := rsITSetup(t, rsITSnap{Spike: "up"}, rsITStrategy("rs-a", "paper", "both", false, nil))
		rsITSetRegime(t, env.cfg, "trending_up_clean", true)
		sc := env.cfg.Strategies[0]
		for _, bad := range []string{`"sideways"`, `null`, `""`, `1`, `true`} {
			refs := `{"open":{"name":"breakout"},"closes":[{"name":"tiered_tp_pct"}],"open_side":` + bad + `}`
			args := append(append([]string{}, sc.Args...), "--strategy-refs", refs, "--probe-only")
			err := probeOneCheckScript(sc.Script, args)
			if err == nil || !strings.Contains(err.Error(), "open_side") {
				t.Fatalf("open_side %s must be refused by the check script, got %v", bad, err)
			}
		}
		for _, good := range []string{"long", "short", "none"} {
			refs := `{"open":{"name":"breakout"},"closes":[{"name":"tiered_tp_pct"}],"open_side":"` + good + `"}`
			args := append(append([]string{}, sc.Args...), "--strategy-refs", refs, "--probe-only")
			if err := probeOneCheckScript(sc.Script, args); err != nil {
				t.Fatalf("open_side %s must be accepted: %v", good, err)
			}
		}
		if err := probeRegimeSideContract(sc.Script); err != nil {
			t.Fatalf("the probe contract must pass against the current check script: %v", err)
		}
	})
}

func rsITWriteArtifact(t *testing.T, path string, states map[string]string, expires time.Time) {
	t.Helper()
	entry := map[string]any{"asset": "BTC", "timeframe": "1h", "classifier": "composite", "states": states, "expires_at": expires.UTC().Format(time.RFC3339)}
	blob, err := json.Marshal(map[string]any{"schema_version": 1, "certified": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rsITWriteConfigFile(t *testing.T, dir string, strategies ...map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, rsITConfigBytes(t, dir, strategies...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegimeSideCertificationIntegration(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	future := time.Now().Add(30 * 24 * time.Hour)

	t.Run("LiveAdmissionNeedsMatchingEvidenceAtProbe", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := rsITWriteConfigFile(t, dir, rsITStrategy("rs-live", "live", "both", false, nil))
		ackPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-ack", "live", "both", false, map[string]any{"allow_uncertified_regime_side": true}))
		paperPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-paper", "paper", "both", false, nil))
		art := func(name string, write func(string)) string {
			p := filepath.Join(dir, name)
			if write != nil {
				write(p)
			}
			return p
		}
		malformed := art("malformed.json", func(p string) { os.WriteFile(p, []byte("{not json"), 0o600) })
		empty := art("empty.json", func(p string) { os.WriteFile(p, []byte(`{"schema_version":1,"certified":[]}`), 0o600) })
		unrelated := art("unrelated.json", func(p string) {
			blob, _ := json.Marshal(map[string]any{"schema_version": 1, "certified": []any{map[string]any{"asset": "ETH", "timeframe": "1h", "classifier": "composite", "states": map[string]string{"trending_up_clean": "long"}}}})
			os.WriteFile(p, blob, 0o600)
		})
		opposite := art("opposite.json", func(p string) {
			rsITWriteArtifact(t, p, map[string]string{"trending_up_clean": "short", "trending_down_clean": "long"}, future)
		})
		emptyStates := art("empty-states.json", func(p string) { rsITWriteArtifact(t, p, map[string]string{}, future) })
		expired := art("expired.json", func(p string) {
			rsITWriteArtifact(t, p, map[string]string{"trending_up_clean": "long"}, time.Now().Add(-time.Hour))
		})
		valid := art("valid.json", func(p string) {
			rsITWriteArtifact(t, p, map[string]string{"trending_up_clean": "long"}, future)
		})
		run := func(cfg, artifact string, args ...string) (int, string) {
			cmd := exec.Command(bin, args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GO_TRADER_DIRECTIONAL_CERT_PATH="+artifact, "HYPERLIQUID_SECRET_KEY=0x"+strings.Repeat("1", 64), "GO_TRADER_HL_BATCH=0")
			out, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			return code, string(out)
		}
		for _, tc := range []struct {
			name, artifact, reason string
		}{
			{"missing", filepath.Join(dir, "absent.json"), "is missing"},
			{"malformed", malformed, "is malformed"},
			{"empty", empty, "has no cell (BTC,1h,composite)"},
			{"unrelated", unrelated, "has no cell (BTC,1h,composite)"},
			{"opposite", opposite, "has no exact label certified"},
			{"empty_states", emptyStates, "has no exact label certified"},
			{"expired", expired, "has an expired cell"},
		} {
			code, out := run(cfgPath, tc.artifact, "probe", "--config", cfgPath)
			if code != 1 || !strings.Contains(out, "strategy[rs-live].regime_side: live admission refused") || !strings.Contains(out, tc.artifact) || !strings.Contains(out, tc.reason) {
				t.Fatalf("%s: probe must refuse with the artifact path and reason %q, exit=%d\n%s", tc.name, tc.reason, code, out)
			}
		}
		recovery := func(cfg, artifact string) (int, string) {
			cmd := exec.Command(bin, "probe", "--config", cfg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GO_TRADER_DIRECTIONAL_CERT_PATH="+artifact, "HYPERLIQUID_SECRET_KEY=0x"+strings.Repeat("1", 64), "GO_TRADER_HL_BATCH=0", "GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=off")
			out, err := cmd.CombinedOutput()
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), string(out)
			} else if err != nil {
				t.Fatal(err)
			}
			return 0, string(out)
		}
		if code, out := recovery(cfgPath, expired); code != 0 || !strings.Contains(out, "probe: OK") {
			t.Fatalf("the update recovery probe must not refuse the restored config on certification evidence, exit=%d\n%s", code, out)
		}
		if code, out := run(cfgPath, expired, "inspect", "--config", cfgPath, "rs-live"); code != 0 || !strings.Contains(out, "entries held") || !strings.Contains(out, "has an expired cell") {
			t.Fatalf("inspect must load an expired-evidence config and report the held entries, exit=%d\n%s", code, out)
		}
		if code, out := run(cfgPath, opposite, "inspect", "--config", cfgPath, "rs-live"); code != 0 || !strings.Contains(out, "entries held") || !strings.Contains(out, "has no exact label certified") {
			t.Fatalf("inspect must report held entries when the active cell certifies no matching label, exit=%d\n%s", code, out)
		}
		if code, out := run(cfgPath, valid, "probe", "--config", cfgPath); code != 0 || !strings.Contains(out, "probe: OK") {
			t.Fatalf("a matching active cell must admit the live config, exit=%d\n%s", code, out)
		}
		if code, out := run(ackPath, filepath.Join(dir, "absent.json"), "probe", "--config", ackPath); code != 0 {
			t.Fatalf("an acknowledged live config passes the feature gate without evidence, exit=%d\n%s", code, out)
		}
		if code, out := run(paperPath, filepath.Join(dir, "absent.json"), "probe", "--config", paperPath); code != 0 {
			t.Fatalf("paper needs no certification, exit=%d\n%s", code, out)
		}
	})

	t.Run("ExpiredEvidenceWithAnOpenPositionLoadsAtStartupAndKeepsCloses", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		artifact := filepath.Join(t.TempDir(), "certs.json")
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long"}, time.Now().Add(-time.Hour))
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", artifact)
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		cfgPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-live", "live", "both", false, nil))
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("startup must load a live regime_side config whose cell expired: %v", err)
		}
		installRegimeSideEvidence(startupRegimeSideEvidence(cfg))
		lines := regimeSideStartupLines(cfg)
		if len(lines) != 1 || !strings.Contains(lines[0], "rs-live: regime_side live entries HELD") || !strings.Contains(lines[0], "has an expired cell") {
			t.Fatalf("startup must report the held entries, got %q", lines)
		}
		if dm := directionalCertStartupLinesNeedingOwnerDM(lines); len(dm) != 1 {
			t.Fatalf("the held-entries line must reach the owner DM path, got %q", dm)
		}
		rawRegime := rsITSetRegime(t, cfg, "trending_up_clean", true)
		sc := cfg.Strategies[0]
		if got := regimeSideRequestForCheck(sc, PositionCtx{}, cfg.Regime); got.Side != "none" || !strings.Contains(got.HoldReason, "expired cell") {
			t.Fatalf("the next flat decision must hold under the expired cell, got %+v", got)
		}
		pos := &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, EntryATR: 2, Side: "short", OwnerStrategyID: sc.ID}
		if plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0); !ok || plan.StopLossATRMult <= 0 || plan.Side != "short" {
			t.Fatalf("the open position's stop plan must still build, ok=%t plan=%+v", ok, plan)
		}
		stub := filepath.Join(t.TempDir(), "rs_stub.py")
		if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
			t.Fatal(err)
		}
		sc.Script = stub
		closing := map[string]any{"strategy": "breakout", "symbol": "BTC", "timeframe": "1h", "signal": 1, "price": 100, "indicators": map[string]any{},
			"mode": "live", "platform": "hyperliquid", "close_fraction": 1, "open_side_applied": "none", "regime": json.RawMessage(rawRegime)}
		blob, _ := json.Marshal(closing)
		t.Setenv("HTF_IT_STUB_JSON", string(blob))
		t.Setenv("HTF_IT_STUB_EXIT", "0")
		var buf bytes.Buffer
		res, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 100}, PositionCtx{Side: "short", Quantity: 1, AvgCost: 100}, cfg.Regime, "simple", NewMultiNotifier(), &StrategyLogger{stratID: sc.ID, writer: &buf}, nil, nil)
		if !ok || res == nil || res.CloseFraction != 1 {
			t.Fatalf("the open position's close must run under the expired cell, ok=%t res=%+v\n%s", ok, res, buf.String())
		}
	})

	t.Run("ForceCloseFromAShellWithoutTheUnitCertPathPassesConfigLoad", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := rsITWriteConfigFile(t, dir, rsITStrategy("rs-live", "live", "both", false, nil))
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GO_TRADER_DIRECTIONAL_CERT_PATH=") {
				env = append(env, kv)
			}
		}
		cmd := exec.Command(bin, "force-close", "--config", cfgPath, "--dry-run", "rs-live")
		cmd.Dir = root
		cmd.Env = append(env, "HYPERLIQUID_SECRET_KEY=0x"+strings.Repeat("1", 64), "GO_TRADER_HL_BATCH=0")
		out, _ := cmd.CombinedOutput()
		text := string(out)
		if strings.Contains(text, "live admission refused") || strings.Contains(text, "Failed to load config") || !strings.Contains(text, `strategy state for "rs-live" not found`) {
			t.Fatalf("force-close must pass config load on the default artifact and reach the close core\n%s", text)
		}
	})

	t.Run("ReloadWithAnExpiredCellAppliesUnrelatedChangesAndRefusesNewLiveRegimeSide", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		artifact := filepath.Join(t.TempDir(), "certs.json")
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long"}, time.Now().Add(-time.Hour))
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", artifact)
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		dir := t.TempDir()
		live := rsITStrategy("rs-live", "live", "both", false, nil)
		other := func(extra map[string]any) map[string]any {
			base := map[string]any{"regime_side": false}
			for k, v := range extra {
				base[k] = v
			}
			return rsITStrategy("rs-other", "live", "both", false, base)
		}
		cfgPath := rsITWriteConfigFile(t, dir, live, other(nil))
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("startup must load: %v", err)
		}
		installRegimeSideEvidence(startupRegimeSideEvidence(cfg))
		state := NewAppState()
		for _, sc := range cfg.Strategies {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
		state.Strategies["rs-live"].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", OwnerStrategyID: "rs-live"}
		reload := func(strategies ...map[string]any) ([]string, error) {
			rsITWriteConfigFile(t, dir, strategies...)
			ev := refreshRegimeSideEvidenceForReload()
			next, err := LoadConfigWithRegimeSideEvidence(cfgPath, ev)
			if err != nil {
				return nil, err
			}
			return applyHotReloadConfig(cfg, next, state, NewMultiNotifier(), nil)
		}
		byID := func(id string) StrategyConfig {
			for _, sc := range cfg.Strategies {
				if sc.ID == id {
					return sc
				}
			}
			t.Fatalf("no strategy %s", id)
			return StrategyConfig{}
		}
		changes, err := reload(live, other(map[string]any{"paused": true}))
		if err != nil || !byID("rs-other").Paused || !strings.Contains(strings.Join(changes, "\n"), "strategy[rs-other].paused: false -> true") {
			t.Fatalf("an unrelated paused change must apply while the regime_side cell is expired and its position open: err=%v changes=%v", err, changes)
		}
		_, err = reload(live, other(map[string]any{"paused": true, "regime_side": true}))
		if err == nil || !strings.Contains(err.Error(), "strategy[rs-other].regime_side: live admission refused") || !strings.Contains(err.Error(), "has an expired cell") || strings.Contains(err.Error(), "strategy[rs-live]") {
			t.Fatalf("turning on live regime_side for a flat strategy without evidence must be refused, naming only that strategy, got %v", err)
		}
		if byID("rs-other").RegimeSide || !byID("rs-live").RegimeSide {
			t.Fatalf("a refused reload must keep the previous config, got %+v", cfg.Strategies)
		}
	})

	t.Run("OnlyExactCertifiedLabelsAuthorizeLiveEntries", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		artifact := filepath.Join(t.TempDir(), "certs.json")
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long", "trending_down_clean": "long"}, future)
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", artifact)
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		cfgPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-live", "live", "both", false, nil), rsITStrategy("rs-ack", "live", "both", false, map[string]any{"allow_uncertified_regime_side": true}))
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("a matching cell must admit: %v", err)
		}
		installRegimeSideEvidence(startupRegimeSideEvidence(cfg))
		byID := map[string]StrategyConfig{}
		for _, sc := range cfg.Strategies {
			byID[sc.ID] = sc
		}
		for _, tc := range []struct {
			id, label, wantSide, holdHas string
		}{
			{"rs-live", "trending_up_clean", "long", ""},
			{"rs-live", "trending_up_choppy", "none", `does not certify the exact label "trending_up_choppy"`},
			{"rs-live", "ranging_directional_up", "none", "does not certify the exact label"},
			{"rs-live", "trending_down_clean", "none", `certifies "trending_down_clean" as long, not short`},
			{"rs-ack", "trending_up_choppy", "long", ""},
			{"rs-ack", "trending_down_clean", "short", ""},
		} {
			rsITSetRegime(t, cfg, tc.label, true)
			got := regimeSideRequestForCheck(byID[tc.id], PositionCtx{}, cfg.Regime)
			if got.Side != tc.wantSide || !strings.Contains(got.HoldReason, tc.holdHas) {
				t.Fatalf("%s %s: want side %s hold %q, got %+v", tc.id, tc.label, tc.wantSide, tc.holdHas, got)
			}
		}
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long"}, time.Now().Add(-time.Minute))
		installRegimeSideEvidence(readRegimeSideEvidence(artifact))
		rsITSetRegime(t, cfg, "trending_up_clean", true)
		if got := regimeSideRequestForCheck(byID["rs-live"], PositionCtx{}, cfg.Regime); got.Side != "none" || !strings.Contains(got.HoldReason, "expired cell") {
			t.Fatalf("an expired certificate must hold entries at decision time, got %+v", got)
		}
	})

	t.Run("ReloadThatRevokesEvidenceAppliesHoldsEntriesAndKeepsCloses", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		artifact := filepath.Join(t.TempDir(), "certs.json")
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long"}, future)
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", artifact)
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		cfgPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-live", "live", "both", false, nil))
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		installRegimeSideEvidence(startupRegimeSideEvidence(cfg))
		rawRegime := rsITSetRegime(t, cfg, "trending_up_clean", true)
		sc := cfg.Strategies[0]
		if got := regimeSideRequestForCheck(sc, PositionCtx{}, cfg.Regime); got.Side != "long" {
			t.Fatalf("certified label before revocation must resolve long, got %+v", got)
		}
		rsITWriteArtifact(t, artifact, map[string]string{"trending_down_clean": "long"}, future)
		ev := refreshRegimeSideEvidenceForReload()
		next, err := LoadConfigWithRegimeSideEvidence(cfgPath, ev)
		if err != nil {
			t.Fatalf("a reload that leaves regime_side unchanged must load under revoked evidence: %v", err)
		}
		state := NewAppState()
		state.Strategies[sc.ID] = NewStrategyState(sc)
		if _, err := applyHotReloadConfig(cfg, next, state, NewMultiNotifier(), nil); err != nil {
			t.Fatalf("a reload that leaves regime_side unchanged must apply under revoked evidence: %v", err)
		}
		if got := regimeSideRequestForCheck(sc, PositionCtx{}, cfg.Regime); got.Side != "none" || !strings.Contains(got.HoldReason, "does not certify the exact label") {
			t.Fatalf("the next flat decision must hold under the revoked artifact, got %+v", got)
		}
		stub := filepath.Join(t.TempDir(), "rs_stub.py")
		if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
			t.Fatal(err)
		}
		sc.Script = stub
		closing := map[string]any{"strategy": "breakout", "symbol": "BTC", "timeframe": "1h", "signal": 1, "price": 100, "indicators": map[string]any{},
			"mode": "live", "platform": "hyperliquid", "close_fraction": 1, "open_side_applied": "none", "regime": json.RawMessage(rawRegime)}
		blob, _ := json.Marshal(closing)
		t.Setenv("HTF_IT_STUB_JSON", string(blob))
		t.Setenv("HTF_IT_STUB_EXIT", "0")
		var buf bytes.Buffer
		res, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 100}, PositionCtx{Side: "short", Quantity: 1, AvgCost: 100}, cfg.Regime, "simple", NewMultiNotifier(), &StrategyLogger{stratID: sc.ID, writer: &buf}, nil, nil)
		if !ok || res == nil || res.CloseFraction != 1 || res.Signal != 1 {
			t.Fatalf("closes must continue under revoked evidence, ok=%t res=%+v\n%s", ok, res, buf.String())
		}
	})

	t.Run("AcknowledgedLiveWarnsAndDMsTheOwner", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", filepath.Join(t.TempDir(), "absent.json"))
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		cfgPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-ack", "live", "both", false, map[string]any{"allow_uncertified_regime_side": true}), rsITStrategy("rs-paper", "paper", "both", false, nil))
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		installRegimeSideEvidence(startupRegimeSideEvidence(cfg))
		lines := append(directionalCertStartupSummary(cfg), regimeSideStartupLines(cfg)...)
		if len(lines) != 2 || !strings.Contains(lines[0], "rs-ack: regime_side LIVE UNCERTIFIED") || !strings.Contains(lines[0], "#1076") || !strings.Contains(lines[1], "rs-paper: regime_side on (paper)") {
			t.Fatalf("startup summary lines = %q", lines)
		}
		sink := &htfITSink{}
		notifier := NewMultiNotifier(notifierBackend{notifier: sink, ownerID: "owner"})
		directionalCertOwnerDMMu.Lock()
		directionalCertOwnerDMLastSnap = ""
		directionalCertOwnerDMMu.Unlock()
		notifyDirectionalCertStartupSummary(notifier, lines)
		_, dms := sink.drain()
		if len(dms) != 1 || !strings.Contains(dms[0].content, "rs-ack: regime_side LIVE UNCERTIFIED") {
			t.Fatalf("the owner must get exactly the acknowledged-live warning, got %+v", dms)
		}
	})
}

func TestRegimeSideConfigAndReloadIntegration(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)

	t.Run("ProbeRefusesInvalidCombinations", func(t *testing.T) {
		base := func(extra map[string]any) map[string]any {
			return rsITStrategy("rs-bad", "paper", "both", false, extra)
		}
		cases := []struct {
			name   string
			strat  map[string]any
			regime bool
			field  string
		}{
			{"manual", map[string]any{"id": "rs-bad", "type": "manual", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h", "capital": 1000, "max_drawdown_pct": 50, "regime_side": true}, true, "strategy[rs-bad].regime_side: only supported for HL perps"},
			{"regime_off", base(nil), false, "strategy[rs-bad].regime_side: requires top-level regime.enabled=true"},
			{"window", base(map[string]any{"regime_directional_window": "nope"}), true, `regime_directional_window="nope" not found`},
			{"no_close", base(map[string]any{"close_strategy": nil}), true, "strategy[rs-bad].regime_side: requires an explicit close_strategy"},
			{"same_close", base(map[string]any{"close_strategy": map[string]any{"name": "breakout"}}), true, `strategy[rs-bad].regime_side: close_strategy "breakout" is the effective open strategy`},
			{"policy", base(map[string]any{"regime_directional_policy": map[string]any{"trend_regime": map[string]any{"trending_up": map[string]any{"direction": "long"}, "trending_down": map[string]any{"direction": "short"}, "ranging": map[string]any{"direction": "both"}}}}), true, "cannot be combined with regime_directional_policy"},
			{"divergence", base(map[string]any{"regime_window_divergence": map[string]any{"short_window": "medium", "medium_window": "macro", "on_divergence": "stand_aside"}}), true, "cannot be combined with regime_window_divergence"},
			{"profile", base(map[string]any{"regime_profile_allocation": map[string]any{"window": "macro", "profiles": map[string]any{}}}), true, "cannot be combined with regime_profile_allocation"},
			{"invert", base(map[string]any{"invert_signal": true}), true, "cannot be combined with invert_signal"},
			{"closed_bar", base(map[string]any{"closed_bar_decisions": true}), true, "cannot be combined with closed_bar_decisions"},
			{"ack_alone", map[string]any{"id": "rs-bad", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": []any{"breakout", "BTC", "1h", "--mode=paper"}, "capital": 1000, "max_drawdown_pct": 50, "allow_uncertified_regime_side": true}, true, "strategy[rs-bad].allow_uncertified_regime_side: requires regime_side: true"},
		}
		for _, tc := range cases {
			strat := map[string]any{}
			for k, v := range tc.strat {
				if v != nil {
					strat[k] = v
				}
			}
			dir := t.TempDir()
			var doc map[string]any
			if err := json.Unmarshal(rsITConfigBytes(t, dir, strat), &doc); err != nil {
				t.Fatal(err)
			}
			if !tc.regime {
				delete(doc, "regime")
			}
			blob, _ := json.Marshal(doc)
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "probe", "--config", path)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			ee, isExit := err.(*exec.ExitError)
			if !isExit || ee.ExitCode() != 1 || !strings.Contains(string(out), tc.field) {
				t.Fatalf("%s: probe must exit 1 naming %q, err=%v\n%s", tc.name, tc.field, err, out)
			}
		}
		okPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-ok", "paper", "both", false, nil))
		cmd := exec.Command(bin, "probe", "--config", okPath)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "probe: OK") {
			t.Fatalf("a valid paper regime_side config must pass the probe including the open_side contract: %v\n%s", err, out)
		}
	})

	t.Run("ReloadAppliesFlatAndRefusesOpen", func(t *testing.T) {
		t.Chdir(root)
		off := rsITStrategy("rs-r", "paper", "both", false, map[string]any{"regime_side": false})
		on := rsITStrategy("rs-r", "paper", "both", false, nil)
		ack := rsITStrategy("rs-r", "paper", "both", false, map[string]any{"allow_uncertified_regime_side": true})
		dir := t.TempDir()
		cfg := rsITLoadIn(t, dir, off)
		state := NewAppState()
		state.Strategies["rs-r"] = NewStrategyState(cfg.Strategies[0])
		next := rsITLoadIn(t, dir, on)
		changes, err := applyHotReloadConfig(cfg, next, state, NewMultiNotifier(), nil)
		if err != nil || !cfg.Strategies[0].RegimeSide || !strings.Contains(strings.Join(changes, "\n"), "strategy[rs-r].regime_side: false -> true") {
			t.Fatalf("a flat regime_side toggle must apply without restart: err=%v changes=%v", err, changes)
		}
		changes, err = applyHotReloadConfig(cfg, rsITLoadIn(t, dir, ack), state, NewMultiNotifier(), nil)
		if err != nil || !cfg.Strategies[0].AllowUncertifiedRegimeSide || len(changes) != 1 {
			t.Fatalf("a flat acknowledgment toggle must apply: err=%v changes=%v", err, changes)
		}
		state.Strategies["rs-r"].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", OwnerStrategyID: "rs-r"}
		for _, target := range []map[string]any{off, on} {
			before := cfg.Strategies[0]
			if _, err := applyHotReloadConfig(cfg, rsITLoadIn(t, dir, target), state, NewMultiNotifier(), nil); err == nil || !strings.Contains(err.Error(), "changed with open positions") {
				t.Fatalf("a toggle with an open position must be refused, got %v", err)
			}
			if cfg.Strategies[0].RegimeSide != before.RegimeSide || cfg.Strategies[0].AllowUncertifiedRegimeSide != before.AllowUncertifiedRegimeSide {
				t.Fatalf("a rejected reload must keep the previous config, got %+v", cfg.Strategies[0])
			}
		}
	})

	t.Run("StatusAndInspectReportTheResolution", func(t *testing.T) {
		t.Chdir(root)
		cfg := rsITLoad(t, rsITStrategy("rs-s", "paper", "long", false, nil))
		rsITSetRegime(t, cfg, "trending_down_clean", true)
		sc := cfg.Strategies[0]
		s := NewStrategyState(sc)
		got := regimeSideStatusForState(sc, s, cfg.Regime)
		if got == nil || got.Side != "none" || got.Window != "macro" || got.Label != "trending_down_clean" || !strings.Contains(got.HoldReason, `base direction "long" does not permit short`) {
			t.Fatalf("flat status must show the window, label and base-direction hold, got %+v", got)
		}
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", OwnerStrategyID: sc.ID}
		if got := regimeSideStatusForState(sc, s, cfg.Regime); got == nil || got.Side != "none" || !strings.Contains(got.HoldReason, "position open") {
			t.Fatalf("open status must show the open-position hold, got %+v", got)
		}
		dir := t.TempDir()
		path := rsITWriteConfigFile(t, dir, rsITStrategy("rs-s", "paper", "long", false, nil))
		cmd := exec.Command(bin, "inspect", "--config", path, "--json", "rs-s")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("inspect: %v\n%s", err, out)
		}
		var doc any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("inspect JSON: %v\n%s", err, out)
		}
		text := string(out)
		if !strings.Contains(text, `"regime_side"`) || !strings.Contains(text, `"window": "macro"`) || !strings.Contains(text, "unresolved (inspect reads no live regime payload") {
			t.Fatalf("inspect must report regime_side with explicit unresolved runtime values\n%s", text)
		}
	})

	t.Run("TunerPreviewArmsCarryTheModeAndEvidence", func(t *testing.T) {
		t.Chdir(root)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
		artifact := filepath.Join(t.TempDir(), "certs.json")
		rsITWriteArtifact(t, artifact, map[string]string{"trending_up_clean": "long"}, time.Now().Add(24*time.Hour))
		t.Setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", artifact)
		prev := currentRegimeSideEvidence()
		t.Cleanup(func() { installRegimeSideEvidence(prev) })
		paper := rsITLoad(t, rsITStrategy("rs-p", "paper", "both", false, nil))
		live := rsITLoad(t, rsITStrategy("rs-l", "live", "both", false, nil))
		installRegimeSideEvidence(startupRegimeSideEvidence(live))
		pp := simulateConfigPayload(paper.Strategies[0], paper.Regime)
		lp := simulateConfigPayload(live.Strategies[0], live.Regime)
		pc, _ := pp["regime_side_certification"].(map[string]interface{})
		lc, _ := lp["regime_side_certification"].(map[string]interface{})
		if pp["regime_side"] != true || pp["direction"] != "both" || pc["context"] != "paper" {
			t.Fatalf("paper arm payload = %+v", pp)
		}
		states, _ := lc["states"].(map[string]string)
		if lc["context"] != "live" || states["trending_up_clean"] != "long" || lc["expires_at"] == nil || lc["cell"] != "BTC|1h|composite" {
			t.Fatalf("live arm must carry the installed regime_side evidence, got %+v", lc)
		}
		candles := stopPreviewCandles()
		for i := 0; i < 160; i++ {
			px := 120 + float64(i%13) - float64(i%5)
			candles = append(candles, UICandle{Time: candles[len(candles)-1].Time + 3600, Open: px, High: px + 1, Low: px - 1, Close: px + 0.5, Volume: 10})
		}
		run, err := runStrategySimulateArms(candles, map[string]map[string]interface{}{"live": lp, "simulated": pp})
		if err != nil || len(run.Refusals) != 0 {
			t.Fatalf("both regime_side preview arms must run, err=%v refusals=%v", err, run.Refusals)
		}
	})

	t.Run("InitGeneratesAnAdmittedPaperConfig", func(t *testing.T) {
		t.Chdir(root)
		cfg := generateConfig(InitOptions{Assets: []string{"BTC"}, EnablePerps: true, PerpsMode: "paper", PerpsStrategies: []string{"breakout"}, PerpsCapital: 1000, PerpsDrawdown: 50, RegimeSide: true})
		enabled := 0
		for _, sc := range cfg.Strategies {
			if sc.RegimeSide {
				enabled++
			}
		}
		if enabled != 1 || cfg.Regime == nil || !cfg.Regime.Enabled {
			t.Fatalf("init must enable regime_side with a regime block, got enabled=%d regime=%+v", enabled, cfg.Regime)
		}
		blob, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfigForProbe(path)
		if err != nil || !loaded.Strategies[0].RegimeSide {
			t.Fatalf("the generated config must pass admission: %v", err)
		}
		live := generateConfig(InitOptions{Assets: []string{"BTC"}, EnablePerps: true, PerpsMode: "live", PerpsStrategies: []string{"breakout"}, PerpsCapital: 1000, PerpsDrawdown: 50, RegimeSide: true})
		for _, sc := range live.Strategies {
			if sc.RegimeSide {
				t.Fatalf("init must not enable regime_side on live strategies, got %s", sc.ID)
			}
		}
	})
}
