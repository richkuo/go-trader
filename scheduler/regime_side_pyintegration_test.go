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
			{"warming", "up", "trending_down_clean", false, "none", "warming"},
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
			{"short", "trending_up_clean", 0},
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
		paperPath := rsITWriteConfigFile(t, t.TempDir(), rsITStrategy("rs-paper", "paper", "both", false, nil))
		art := func(name string, write func(string)) string {
			p := filepath.Join(dir, name)
			if write != nil {
				write(p)
			}
			return p
		}
		expired := art("expired.json", func(p string) {
			rsITWriteArtifact(t, p, map[string]string{"trending_up_clean": "long"}, time.Now().Add(-time.Hour))
		})
		valid := art("valid.json", func(p string) {
			rsITWriteArtifact(t, p, map[string]string{"trending_up_clean": "long"}, future)
		})
		start := func(cfg, artifact string, extraEnv ...string) func() (int, string) {
			cmd := exec.Command(bin, "probe", "--config", cfg)
			cmd.Dir = root
			cmd.Env = append(append(os.Environ(), "GO_TRADER_DIRECTIONAL_CERT_PATH="+artifact, "HYPERLIQUID_SECRET_KEY=0x"+strings.Repeat("1", 64), "GO_TRADER_HL_BATCH=0"), extraEnv...)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			return func() (int, string) {
				err := <-done
				if ee, ok := err.(*exec.ExitError); ok {
					return ee.ExitCode(), out.String()
				} else if err != nil {
					t.Fatal(err)
				}
				return 0, out.String()
			}
		}
		missing := start(cfgPath, filepath.Join(dir, "absent.json"))
		expiredProbe := start(cfgPath, expired)
		recovery := start(cfgPath, expired, "GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=off")
		admitted := start(cfgPath, valid)
		paper := start(paperPath, filepath.Join(dir, "absent.json"))
		for _, tc := range []struct {
			name, artifact, reason string
			wait                   func() (int, string)
		}{
			{"missing", filepath.Join(dir, "absent.json"), "is missing", missing},
			{"expired", expired, "has an expired cell", expiredProbe},
		} {
			code, out := tc.wait()
			if code != 1 || !strings.Contains(out, "strategy[rs-live].regime_side: live admission refused") || !strings.Contains(out, tc.artifact) || !strings.Contains(out, tc.reason) {
				t.Fatalf("%s: probe must refuse with the artifact path and reason %q, exit=%d\n%s", tc.name, tc.reason, code, out)
			}
		}
		if code, out := recovery(); code != 0 || !strings.Contains(out, "probe: OK") {
			t.Fatalf("the update recovery probe must not refuse the restored config on certification evidence, exit=%d\n%s", code, out)
		}
		if code, out := admitted(); code != 0 || !strings.Contains(out, "probe: OK") {
			t.Fatalf("a matching active cell must admit the live config, exit=%d\n%s", code, out)
		}
		if code, out := paper(); code != 0 || !strings.Contains(out, "probe: OK") {
			t.Fatalf("paper needs no certification and must pass the probe including the open_side contract, exit=%d\n%s", code, out)
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
			{"rs-live", "trending_down_clean", "none", `certifies "trending_down_clean" as long, not short`},
			{"rs-ack", "trending_up_choppy", "long", ""},
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

	t.Run("ReloadAppliesFlatAndRefusesOpen", func(t *testing.T) {
		t.Chdir(root)
		off := rsITStrategy("rs-r", "paper", "both", false, map[string]any{"regime_side": false})
		on := rsITStrategy("rs-r", "paper", "both", false, nil)
		dir := t.TempDir()
		cfg := rsITLoadIn(t, dir, off)
		state := NewAppState()
		state.Strategies["rs-r"] = NewStrategyState(cfg.Strategies[0])
		next := rsITLoadIn(t, dir, on)
		changes, err := applyHotReloadConfig(cfg, next, state, NewMultiNotifier(), nil)
		if err != nil || !cfg.Strategies[0].RegimeSide || !strings.Contains(strings.Join(changes, "\n"), "strategy[rs-r].regime_side: false -> true") {
			t.Fatalf("a flat regime_side toggle must apply without restart: err=%v changes=%v", err, changes)
		}
		state.Strategies["rs-r"].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", OwnerStrategyID: "rs-r"}
		before := cfg.Strategies[0]
		if _, err := applyHotReloadConfig(cfg, rsITLoadIn(t, dir, off), state, NewMultiNotifier(), nil); err == nil || !strings.Contains(err.Error(), "changed with open positions") {
			t.Fatalf("a toggle with an open position must be refused, got %v", err)
		}
		if cfg.Strategies[0].RegimeSide != before.RegimeSide {
			t.Fatalf("a rejected reload must keep the previous config, got %+v", cfg.Strategies[0])
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
