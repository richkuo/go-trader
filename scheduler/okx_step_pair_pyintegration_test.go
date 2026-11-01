//go:build pyintegration && parkedresearch

package main

import (
	"bytes"
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
				t.Fatalf("a member increase under the %s hold booked %+v", tc.name, out)
			}
			if !strings.Contains(p.logs.String(), "only the pair coordinator places member increases") {
				t.Fatalf("under the %s hold the member increase must still be dropped by the pair suppression, which runs before every risk gate:\n%s", tc.name, p.logs.String())
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
			adm, _, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, newPairTestVenues(false)))
			if adm != nil {
				adm.release()
			}
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("an admitted OKX pair member with %s must be refused, got %v", tc.key, err)
			}
		})
	}
}

func TestOKXSharedStepPairRefusesUnknownTypesAndMissingOwners(t *testing.T) {
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

func TestOKXSharedStepPairNativeReductionChargesTheMemberBinding(t *testing.T) {
	p := newOKXPairStepEnv(t, pairCostRates)
	p.setPosition(entrySideLong, 0.02, 90000)
	s := p.e.state.Strategies[pairTestOKX]
	p.e.mu.Lock()
	rt := s.Basket
	s.Basket = nil
	cash, trades := s.Cash, len(s.TradeHistory)
	p.e.mu.Unlock()
	if out := p.step(nil, nil); out.Trades != 0 || p.e.signed(pairTestOKX) != 0.02 || s.Cash != cash || len(s.TradeHistory) != trades {
		t.Fatalf("a member without pair ownership must not book through the shared owner: %+v book %v", out, p.e.signed(pairTestOKX))
	}
	if len(s.pairCostFaults) == 0 {
		t.Fatal("the refused native reduction must queue a cost provenance fault")
	}
	p.e.mu.Lock()
	s.Basket = rt
	s.pairCostFaults = nil
	p.e.mu.Unlock()
	out := p.step(nil, nil)
	if out.Trades != 1 || p.e.signed(pairTestOKX) != 0 {
		t.Fatalf("the native reduction must book once through the shared owner: %+v\n%s", out, p.logs.String())
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	want := p.e.binding(pairTestOKX)
	if tr.CostModelVersion != FillCostModelReplayCustomVersion || tr.CostPolicyID != want.PolicyID || !pairCostNear(tr.ExchangeFee, tr.Quantity*tr.Price*0.0007) {
		t.Fatalf("the native reduction row %+v must carry the member stamp and taker rate", tr)
	}
	if err := p.e.store.SavePartition(p.e.state, p.e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	rows := p.e.savedTrades(pairTestOKX)
	if len(rows) == 0 || rows[len(rows)-1].PolicyID != want.PolicyID {
		t.Fatalf("the saved native reduction row lost the member stamp: %+v", rows)
	}
}
