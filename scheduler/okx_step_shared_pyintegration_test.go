//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOKXSharedStepRefusesUnknownTypesAndMissingOwners(t *testing.T) {
	sc := StrategyConfig{ID: "okx-refuse", Type: "spot", Platform: "okx", Script: okxGoldenCheckScript, Args: []string{"fixture", "BTC", "1h", "--mode=paper", "--inst-type=spot"},
		Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	logs := &bytes.Buffer{}
	part := partitionFor(sc)
	context := func() okxStepContext {
		return okxStepContext{Cfg: env.cfg, State: env.state, StratState: env.state.Strategies[sc.ID], Store: env.store, StratDB: env.fileFor(sc.ID),
			Mu: &env.mu, Logger: &StrategyLogger{stratID: sc.ID, writer: io.MultiWriter(os.Stdout, logs)}, Prices: map[string]float64{"BTC": 60000},
			Risk: &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(env.cfg, part)}, Scope: part.Scope}
	}
	called := false
	check := func(StrategyConfig, PositionCtx, *StrategyLogger) (*OKXResult, string, float64, bool) {
		called = true
		return nil, "", 0, false
	}
	for name, tc := range map[string]struct {
		sc     StrategyConfig
		mutate func(c *okxStepContext)
	}{
		"futures type":   {func() StrategyConfig { s := sc; s.Type = "futures"; return s }(), nil},
		"empty type":     {func() StrategyConfig { s := sc; s.Type = ""; return s }(), nil},
		"other platform": {func() StrategyConfig { s := sc; s.Platform = "hyperliquid"; return s }(), nil},
		"no store":       {sc, func(c *okxStepContext) { c.Store = nil }},
	} {
		c := context()
		if tc.mutate != nil {
			tc.mutate(&c)
		}
		out := runOKXStep(c, tc.sc, check)
		if called || out.Trades != 0 {
			t.Fatalf("%s: the step must refuse before running the check", name)
		}
	}
	if !strings.Contains(logs.String(), "has no OKX direction rule") {
		t.Fatalf("an unknown type must be refused with no long-only fallback:\n%s", logs.String())
	}
}

func TestOKXSharedStepSimulatedClockDrivesAdmissionBookingAndReminder(t *testing.T) {
	root := basketPyRoot(t)
	sim := time.Date(2026, 3, 2, 10, 20, 0, 0, time.UTC)
	clock := &simulatedClock{}
	if err := clock.Set(sim); err != nil {
		t.Fatal(err)
	}
	if err := bindTradingClock("okx-step-test", clock.Now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock("okx-step-test") })
	okxGoldenResetTrackers(t)
	t.Setenv("OKX_API_KEY", "synthetic-okx-step-fixture-key")
	t.Chdir(root)
	cutoff := sim.Add(-time.Minute)
	bar := cutoff.Truncate(time.Hour).Add(-time.Hour)
	run := func(t *testing.T, sc StrategyConfig, venue okxGoldenVenue) (*okxGoldenHarness, okxStepResult) {
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
		useEntryTradeRecorder(t, env.store)
		dir := t.TempDir()
		rec := &okxGoldenRecNotifier{}
		h := &okxGoldenHarness{t: t, env: env, sc: sc, rec: rec, notifier: NewMultiNotifier(notifierBackend{notifier: rec, channels: map[string]string{"okx": "chan-okx"}, ownerID: "owner-1"}),
			logBuf: &bytes.Buffer{}, dir: dir, spec: filepath.Join(dir, "spec.json"), argvLog: filepath.Join(dir, "argv.jsonl"), barMs: bar.UnixMilli(), cutoffMs: cutoff.UnixMilli()}
		h.logger = &StrategyLogger{stratID: sc.ID, writer: io.MultiWriter(os.Stdout, h.logBuf)}
		part := partitionFor(sc)
		h.sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(env.cfg, part)}
		t.Setenv("OKX_STEP_CHECK_FIXTURE", h.spec)
		h.installVenue()
		if venue != nil {
			h.venue = []okxGoldenVenue{venue}
		}
		h.writeSpec(okxGoldenBuy(100))
		prices := okxGoldenPx(100)
		in := h.captureInputs(prices)
		out := runOKXStep(okxStepContext{Cfg: env.cfg, State: env.state, StratState: env.state.Strategies[sc.ID], Store: env.store, StratDB: env.fileFor(sc.ID),
			Mu: &env.mu, Notifier: h.notifier, Logger: h.logger, Prices: prices, Risk: h.sr, Scope: part.Scope,
			Pos: okxStepPosition{Ctx: in.PosCtx, Qty: in.PosQty, Side: in.PosSide, AvgCost: in.AvgCost, Leverage: in.Leverage}, Cash: in.Cash, CashReconcile: in.CashReconcile}, sc, okxOrdinaryCheck(prices, env.cfg, h.notifier))
		return h, out
	}
	for _, typ := range []string{"spot", "perps"} {
		t.Run("paper closed-bar "+typ, func(t *testing.T) {
			sc := StrategyConfig{ID: "okx-clock-" + typ, Type: typ, Platform: "okx", Script: okxGoldenCheckScript, Args: []string{"fixture", "BTC", "1h", "--mode=paper", "--inst-type=" + map[string]string{"spot": "spot", "perps": "swap"}[typ]},
				ClosedBarDecisions: true, Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
			h, out := run(t, sc, nil)
			if out.Trades != 1 {
				t.Fatalf("the simulated-clock admission must admit the decision candle: %+v\n%s", out, h.logBuf.String())
			}
			s := h.env.strat(sc.ID)
			h.env.mu.RLock()
			tradeAt, openedAt := s.TradeHistory[len(s.TradeHistory)-1].Timestamp, s.Positions["BTC"].OpenedAt
			var updated int64
			for k, rec := range s.EntryGuard.Decisions {
				if k.Side == entrySideLong {
					updated = rec.UpdatedAtMs
				}
			}
			h.env.mu.RUnlock()
			if !tradeAt.Equal(sim) || !openedAt.Equal(sim) {
				t.Fatalf("paper booking must read the simulated clock: trade %s opened %s, want %s", tradeAt, openedAt, sim)
			}
			if updated != sim.UnixMilli() {
				t.Fatalf("admission must stamp the simulated clock: %d, want %d", updated, sim.UnixMilli())
			}
			h.env.mu.RLock()
			var consumed int64
			for k, rec := range s.EntryGuard.Decisions {
				if k.Side == entrySideLong {
					consumed = rec.ConsumedThroughBarMs
				}
			}
			h.env.mu.RUnlock()
			if consumed != bar.UnixMilli() {
				t.Fatalf("consumed bar %d, want %d", consumed, bar.UnixMilli())
			}
		})
	}
	t.Run("live spot reminder", func(t *testing.T) {
		sc := StrategyConfig{ID: "okx-clock-live", Type: "spot", Platform: "okx", Script: okxGoldenCheckScript, Args: []string{"fixture", "BTC", "1h", "--mode=live", "--inst-type=spot"},
			Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
		h, out := run(t, sc, okxGoldenFillAt(100, 20))
		if out.Trades != 1 || len(h.sends) != 1 {
			t.Fatalf("the over-budget live fill must book once: %+v sends %d", out, len(h.sends))
		}
		globalSpotCashReconcileReminder.mu.Lock()
		at := globalSpotCashReconcileReminder.lastNotifiedAt
		globalSpotCashReconcileReminder.mu.Unlock()
		if !at.Equal(sim) {
			t.Fatalf("the cash reminder must be stamped from the simulated clock: %s, want %s", at, sim)
		}
		alerts, _ := json.Marshal(h.rec.drain())
		if !strings.Contains(string(alerts), "LIVE SPOT CASH OVER BUDGET") {
			t.Fatalf("the cash alert must be sent: %s", alerts)
		}
	})
}

func TestOKXSharedStepSplitLayoutRoutesEachPartitionToItsOwnFile(t *testing.T) {
	root := basketPyRoot(t)
	okxGoldenResetTrackers(t)
	t.Setenv("OKX_API_KEY", "synthetic-okx-step-fixture-key")
	t.Chdir(root)
	dir := t.TempDir()
	mk := func(id, mode, source string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "spot", Platform: "okx", Script: okxGoldenCheckScript, Args: []string{"fixture", "BTC", "1h", "--mode=" + mode, "--inst-type=spot"},
			ClosedBarDecisions: true, Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1, PaperSource: source}
	}
	live, paper, sourced := mk("okx-split-live", "live", ""), mk("okx-split-paper", "paper", ""), mk("okx-split-s1", "paper", "s1")
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}}, Strategies: []StrategyConfig{live, paper, sourced}}
	env := openEntryTestEnv(t, cfg)
	useEntryTradeRecorder(t, env.store)
	bar, cutoff := okxGoldenTimes()
	for _, sc := range []StrategyConfig{live, paper, sourced} {
		sdir := t.TempDir()
		rec := &okxGoldenRecNotifier{}
		h := &okxGoldenHarness{t: t, env: env, sc: sc, rec: rec, notifier: NewMultiNotifier(notifierBackend{notifier: rec, channels: map[string]string{"okx": "chan-okx"}, ownerID: "owner-1"}),
			logBuf: &bytes.Buffer{}, dir: sdir, spec: filepath.Join(sdir, "spec.json"), argvLog: filepath.Join(sdir, "argv.jsonl"), barMs: bar.UnixMilli(), cutoffMs: cutoff.UnixMilli()}
		h.logger = &StrategyLogger{stratID: sc.ID, writer: io.MultiWriter(os.Stdout, h.logBuf)}
		part := partitionFor(sc)
		h.sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(cfg, part)}
		t.Setenv("OKX_STEP_CHECK_FIXTURE", h.spec)
		h.installVenue()
		h.venue = []okxGoldenVenue{okxGoldenFill(1)}
		if !okxIsLive(sc.Args) {
			h.venue = nil
		}
		h.writeSpec(okxGoldenBuy(100))
		out, _ := okxGoldenDispatch(h, okxGoldenPx(100), h.captureInputs(okxGoldenPx(100)), env.store.persistenceHoldsPartition(part))
		if out != 1 {
			t.Fatalf("%s booked %d trades\n%s", sc.ID, out, h.logBuf.String())
		}
	}
	for _, line := range (&okxGoldenHarness{t: t, env: env}).save(false) {
		if !strings.HasSuffix(line, ": ok") {
			t.Fatalf("cycle save: %s", line)
		}
	}
	owners := func(path string) map[string]bool {
		db, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer db.Close()
		out := map[string]bool{}
		for _, q := range []string{`SELECT strategy_id FROM trades`, `SELECT strategy_id FROM positions`, `SELECT strategy_id FROM entry_decision_state WHERE consumed_through_bar_ms > 0`} {
			rows, err := db.db.Query(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				out[id] = true
			}
			rows.Close()
		}
		return out
	}
	for path, want := range map[string]string{cfg.DBFile: live.ID, cfg.PaperDBFile: paper.ID, filepath.Join(dir, "s1.db"): sourced.ID} {
		got := owners(path)
		if len(got) != 1 || !got[want] {
			t.Fatalf("%s holds trade, position or entry rows for %v, want only %s", filepath.Base(path), got, want)
		}
	}
}
