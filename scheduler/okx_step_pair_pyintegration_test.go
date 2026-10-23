//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type okxPairStepEnv struct {
	t        *testing.T
	e        *pairTestEnv
	netLog   string
	rec      *okxGoldenRecNotifier
	notifier *MultiNotifier
	logs     *bytes.Buffer
	deny     *okxGoldenDenyTransport
}

func okxBlockOrderSends(t *testing.T) {
	t.Helper()
	prevPlain, prevGuarded := okxExecuteFn, okxExecuteGuardedFn
	t.Cleanup(func() { okxExecuteFn, okxExecuteGuardedFn = prevPlain, prevGuarded })
	okxExecuteFn = func(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
		t.Errorf("the step sent an OKX %s order of %v", side, size)
		return nil, "", errors.New("order sends are blocked in this fixture")
	}
	okxExecuteGuardedFn = func(script, symbol, side string, size float64, instType string, opts okxExecuteOptions) (*OKXExecuteResult, string, error) {
		t.Errorf("the step sent a guarded OKX %s order of %v", side, size)
		return nil, "", errors.New("order sends are blocked in this fixture")
	}
}

func newOKXPairStepEnv(t *testing.T, mutate func(root map[string]interface{})) *okxPairStepEnv {
	t.Helper()
	e, root := pairPyEnv(t, mutate)
	t.Chdir(root)
	dir := t.TempDir()
	p := &okxPairStepEnv{t: t, e: e, netLog: filepath.Join(dir, "net.jsonl"), rec: &okxGoldenRecNotifier{}, logs: &bytes.Buffer{}, deny: &okxGoldenDenyTransport{}}
	p.notifier = NewMultiNotifier(notifierBackend{notifier: p.rec, channels: map[string]string{"okx": "chan-okx"}, ownerID: "owner-1"})
	t.Setenv("PYTHONPATH", filepath.Join(root, "scheduler", "testdata", "okx_step", "offline"))
	t.Setenv("OKX_STEP_OFFLINE_LOG", p.netLog)
	okxGoldenResetTrackers(t)
	okxBlockOrderSends(t)
	prevTransport := http.DefaultTransport
	http.DefaultTransport = p.deny
	t.Cleanup(func() {
		http.DefaultTransport = prevTransport
		if n := p.deny.attempts.Load(); n != 0 {
			t.Errorf("the pair step attempted %d Go HTTP request(s)", n)
		}
		if blob, err := os.ReadFile(p.netLog); err == nil && len(bytes.TrimSpace(blob)) > 0 {
			t.Errorf("the frozen management check reached for the exchange: %s", blob)
		}
	})
	return p
}

func (p *okxPairStepEnv) sc() StrategyConfig {
	return pairPySC(p.e, pairTestOKX)
}

func (p *okxPairStepEnv) setPosition(side string, qty, avgCost float64) {
	p.t.Helper()
	signed := qty
	if side == entrySideShort {
		signed = -qty
	}
	p.e.externalBookChange(pairTestOKX, signed)
	p.e.mu.Lock()
	pos := p.e.state.Strategies[pairTestOKX].Positions["BTC"]
	pos.AvgCost, pos.InitialQuantity, pos.EntryATR = avgCost, qty, 1000
	p.e.mu.Unlock()
}

func (p *okxPairStepEnv) context(sc StrategyConfig) okxStepContext {
	p.t.Helper()
	s := p.e.state.Strategies[sc.ID]
	db, err := p.e.store.dbForStrategy(sc.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	part := partitionFor(sc)
	c := okxStepContext{Cfg: p.e.cfg, State: p.e.state, StratState: s, Store: p.e.store, StratDB: db, Mu: p.e.mu, Notifier: p.notifier,
		Logger: &StrategyLogger{stratID: sc.ID, writer: io.MultiWriter(os.Stdout, p.logs)}, Prices: map[string]float64{"BTC": 60000},
		Risk: &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(p.e.cfg, part)}, Scope: part.Scope,
		PersistenceHold: p.e.store.persistenceHoldsPartition(part), Coord: p.e.bc}
	p.e.mu.RLock()
	if pos, ok := s.Positions["BTC"]; ok {
		ctx := positionCtxForCheck(sc, pos, p.e.cfg.Regime)
		c.Pos = okxStepPosition{Ctx: ctx, Qty: ctx.Quantity, Side: ctx.Side, AvgCost: ctx.AvgCost, Leverage: pos.Leverage}
	}
	p.e.mu.RUnlock()
	return c
}

func (p *okxPairStepEnv) step(mutate func(c *okxStepContext), check okxCheckFunc) okxStepResult {
	p.t.Helper()
	sc := p.sc()
	c := p.context(sc)
	if mutate != nil {
		mutate(&c)
	}
	if check == nil {
		check = okxPairManagementCheck(pairPyManagementInput(60000), p.notifier)
	}
	p.e.bc.beginCycle()
	return runOKXStep(c, sc, check)
}

func (p *okxPairStepEnv) observed() bool {
	_, ok := p.e.rt().observations[pairTestOKX]
	return ok
}

func (p *okxPairStepEnv) echoFailure() string {
	return p.e.rt().pairEchoFailures[pairTestOKX]
}

func okxPairRawCheck(input pairManagementInput, notifier *MultiNotifier, mangle func(res *OKXResult) (*OKXResult, error)) okxCheckFunc {
	return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
		args, result, stderr, err := runOKXPairManagementCheckRaw(sc, posCtx, input, logger)
		if err == nil && result != nil && result.Error == "" {
			result, err = mangle(result)
		}
		frozen := map[string]float64{}
		if okxUsablePrice(input.MarkPrice) {
			frozen[okxSymbol(sc.Args)] = input.MarkPrice
		}
		return completeOKXCheck(sc, args, result, stderr, err, frozen, notifier, logger)
	}
}

func okxPairInjected(signal int, fraction float64) okxCheckFunc {
	base := okxPairManagementCheck(pairPyManagementInput(60000), nil)
	return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
		res, label, px, ok := base(sc, posCtx, logger)
		if ok {
			res.Signal, res.CloseFraction = signal, fraction
			label = map[int]string{1: "BUY", -1: "SELL", 0: "HOLD"}[signal]
		}
		return res, label, px, ok
	}
}

func TestOKXSharedStepPairValidManagementObservesAndBooksNothingFromFlat(t *testing.T) {
	p := newOKXPairStepEnv(t, nil)
	out := p.step(nil, nil)
	if out.Trades != 0 || p.e.bookedTrades(pairTestOKX) != 0 || p.e.signed(pairTestOKX) != 0 {
		t.Fatalf("a flat management check booked %+v", out)
	}
	if !p.observed() || p.echoFailure() != "" {
		t.Fatalf("a valid frozen echo must feed the coordinator observation (failure %q)", p.echoFailure())
	}
	obs := p.e.rt().observations[pairTestOKX]
	if obs.Price != 60000 || indicatorsATRValue(obs.Indicators) <= 0 {
		t.Fatalf("the observation must carry the frozen mark and the management ATR: %+v", obs)
	}
}

func TestOKXSharedStepPairNativeReductionsBookThroughTheSharedOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		side    string
		avgCost float64
		check   okxCheckFunc
		left    float64
	}{
		{"long below its stop", entrySideLong, 90000, nil, 0},
		{"short above its stop", entrySideShort, 30000, nil, 0},
		{"partial long reduction", entrySideLong, 61000, okxPairInjected(-1, 0.5), 0.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newOKXPairStepEnv(t, nil)
			p.setPosition(tc.side, 0.02, tc.avgCost)
			out := p.step(nil, tc.check)
			if out.Trades != 1 || !strings.Contains(out.Detail, "BTC") {
				t.Fatalf("the native reduction must book once through the shared owner: %+v\n%s", out, p.logs.String())
			}
			if q := p.e.signed(pairTestOKX); q < -tc.left-1e-9 || q > tc.left+1e-9 || (tc.left > 0 && q == 0) {
				t.Fatalf("after the reduction the member holds %v, want |%v|", q, tc.left)
			}
			if !p.observed() {
				t.Fatal("a valid reduction check must still feed the coordinator observation")
			}
		})
	}
}

func TestOKXSharedStepPairReturnedIncreaseIsSuppressed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		side  string
		check okxCheckFunc
	}{
		{"open from flat", "", okxPairInjected(1, 0)},
		{"add to a long", entrySideLong, okxPairInjected(1, 0)},
		{"add to a short", entrySideShort, okxPairInjected(-1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newOKXPairStepEnv(t, nil)
			before := 0.0
			if tc.side != "" {
				p.setPosition(tc.side, 0.02, 60000)
				before = p.e.signed(pairTestOKX)
			}
			out := p.step(nil, tc.check)
			if out.Trades != 0 || p.e.signed(pairTestOKX) != before {
				t.Fatalf("a returned member increase booked %+v (book %v, was %v)", out, p.e.signed(pairTestOKX), before)
			}
			if !strings.Contains(p.logs.String(), "only the pair coordinator places member increases") {
				t.Fatalf("the increase must be dropped by the pair suppression:\n%s", p.logs.String())
			}
			if !p.observed() {
				t.Fatal("a valid echo with a dropped increase must still feed the coordinator observation")
			}
		})
	}
}

func TestOKXSharedStepPairFailedChecksLeaveTheObservationInvalid(t *testing.T) {
	input := pairPyManagementInput(60000)
	for _, tc := range []struct {
		name  string
		check func(p *okxPairStepEnv) okxCheckFunc
		want  string
	}{
		{"missing echo", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) { res.PairMember = nil; return res, nil })
		}, "pair member"},
		{"wrong echo", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) {
				other := *res.PairMember
				other.MemberID = "pair-okx-other"
				res.PairMember = &other
				return res, nil
			})
		}, "pair member"},
		{"nil result", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) { return nil, nil })
		}, "the check returned no result"},
		{"reported error", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) { res.Error = "fixture management error"; return res, nil })
		}, "fixture management error"},
		{"process error", func(p *okxPairStepEnv) okxCheckFunc {
			return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
				sc.Script = "shared_scripts/no_such_check_okx.py"
				return okxPairManagementCheck(input, p.notifier)(sc, posCtx, logger)
			}
		}, "script error"},
		{"closed-bar block returned", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) {
				res.ClosedBar = &ClosedBarDecision{Enabled: true}
				return res, nil
			})
		}, "closed-bar decision contract mismatch"},
		{"higher-timeframe block returned", func(p *okxPairStepEnv) okxCheckFunc {
			return okxPairRawCheck(input, p.notifier, func(res *OKXResult) (*OKXResult, error) {
				res.HTFFilter = &HTFFilterEcho{Status: "ok", Timeframe: "4h"}
				return res, nil
			})
		}, "higher-timeframe filter contract mismatch"},
		{"frozen input without a mark", func(p *okxPairStepEnv) okxCheckFunc {
			noMark := input
			noMark.MarkPrice = 0
			return okxPairManagementCheck(noMark, p.notifier)
		}, "finite positive mark_price"},
		{"no usable price", func(p *okxPairStepEnv) okxCheckFunc {
			return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
				args, result, stderr, err := runOKXPairManagementCheckRaw(sc, posCtx, input, logger)
				if err == nil && result != nil {
					result.Price = 0
				}
				return completeOKXCheck(sc, args, result, stderr, err, map[string]float64{}, p.notifier, logger)
			}
		}, "No price available for BTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newOKXPairStepEnv(t, nil)
			p.setPosition(entrySideLong, 0.02, 90000)
			p.e.observe("")
			before := p.e.signed(pairTestOKX)
			out := p.step(func(c *okxStepContext) { c.Prices = map[string]float64{} }, tc.check(p))
			if out.Trades != 0 || p.e.signed(pairTestOKX) != before || p.e.bookedTrades(pairTestOKX) != 0 {
				t.Fatalf("a failed check booked %+v (book %v, was %v)", out, p.e.signed(pairTestOKX), before)
			}
			if p.observed() || p.echoFailure() == "" {
				t.Fatalf("a failed check must leave the pair observation invalid (observed %t, failure %q)", p.observed(), p.echoFailure())
			}
			if !strings.Contains(p.logs.String(), tc.want) {
				t.Fatalf("the failure must surface %q:\n%s", tc.want, p.logs.String())
			}
			p.e.mu.Lock()
			hold := p.e.bc.pairIncreaseHold(p.e.rt(), basketCycleInputs{Now: time.Now()})
			p.e.mu.Unlock()
			if hold == "" {
				t.Fatal("pair entry must hold after a failed member check")
			}
		})
	}
}

func TestOKXSharedStepPairRiskHoldsKeepNativeReductions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(c *okxStepContext)
	}{
		{"paused", func(c *okxStepContext) {}},
		{"daily loss", func(c *okxStepContext) { c.Risk.DailyLossEntriesHeld = true }},
		{"notional cap", func(c *okxStepContext) { c.Risk.NotionalBlocked = true }},
		{"persistence hold", func(c *okxStepContext) { c.PersistenceHold = true }},
		{"exposure cap", func(c *okxStepContext) {
			c.Risk.ExposureCapStatus = ExposureCapStatus{Configured: true, CapUSD: 1, LongUSD: 2, ShortUSD: 2, LongBlocked: true, ShortBlocked: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newOKXPairStepEnv(t, nil)
			sc := p.sc()
			if tc.name == "paused" {
				for i := range p.e.cfg.Strategies {
					if p.e.cfg.Strategies[i].ID == pairTestOKX {
						p.e.cfg.Strategies[i].Paused = true
					}
				}
				sc = pairPySC(p.e, pairTestOKX)
			}
			p.setPosition(entrySideLong, 0.02, 60000)
			c := p.context(sc)
			tc.mutate(&c)
			out := runOKXStep(c, sc, okxPairInjected(1, 0))
			if out.Trades != 0 || p.e.signed(pairTestOKX) != 0.02 {
				t.Fatalf("a held member increase booked %+v", out)
			}
			p.setPosition(entrySideLong, 0.02, 90000)
			c = p.context(sc)
			tc.mutate(&c)
			out = runOKXStep(c, sc, okxPairManagementCheck(pairPyManagementInput(60000), p.notifier))
			if out.Trades != 1 || p.e.signed(pairTestOKX) != 0 {
				t.Fatalf("the native reduction must pass the %s hold: %+v (book %v)\n%s", tc.name, out, p.e.signed(pairTestOKX), p.logs.String())
			}
		})
	}
}

func TestOKXSharedStepPairAdmissionRefusesIndependentEntryFeatures(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value interface{}
	}{
		{"closed_bar_decisions", true},
		{"htf_filter", true},
		{"hurst_gate", map[string]interface{}{"enabled": true, "min": 0.55}},
		{"allowed_regimes", []interface{}{"trending_up"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			registerPairTestPolicy(t)
			dir := filepath.Join(t.TempDir(), "run")
			doc := pairTestDoc(t, dir, func(root map[string]interface{}) { pairTestStrategy(root, pairTestOKX)[tc.key] = tc.value })
			adm, _, err := newPairResearchAdmission(doc, "run-1", dir, false, pairResearchDeps{VenueFor: newPairTestVenues(false).factory()})
			if adm != nil {
				adm.release()
			}
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("an admitted OKX pair member with %s must be refused, got %v", tc.key, err)
			}
		})
	}
}

func TestOKXSharedStepRefusesUnknownTypesAndMissingOwners(t *testing.T) {
	p := newOKXPairStepEnv(t, nil)
	sc := p.sc()
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
		"no coordinator": {sc, func(c *okxStepContext) { c.Coord = nil }},
		"no store":       {sc, func(c *okxStepContext) { c.Store = nil }},
	} {
		c := p.context(sc)
		if tc.mutate != nil {
			tc.mutate(&c)
		}
		out := runOKXStep(c, tc.sc, check)
		if called || out.Trades != 0 {
			t.Fatalf("%s: the step must refuse before running the check", name)
		}
	}
	if !strings.Contains(p.logs.String(), "has no OKX direction rule") {
		t.Fatalf("an unknown type must be refused with no long-only fallback:\n%s", p.logs.String())
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
