package main

import (
	"math"
	"os"
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

// #1268 — the core invariant: with risk_per_trade_pct set, two strategies
// with different stop distances on the same equity open positions whose
// qty × stop_distance dollar risk is EQUAL.
func TestPerpsRiskBasedNotionalConstantDollarRisk(t *testing.T) {
	cash, price, riskPct := 1000.0, 2000.0, 1.0 // risk $10/trade
	wideDist := 60.0                            // e.g. 3.0×ATR(20)
	tightDist := 20.0                           // e.g. 1.0×ATR(20)

	wide := PerpsRiskBasedNotional(cash, price, riskPct, wideDist, 10)
	tight := PerpsRiskBasedNotional(cash, price, riskPct, tightDist, 10)
	if wide <= 0 || tight <= 0 {
		t.Fatalf("expected positive notionals, got wide=%g tight=%g", wide, tight)
	}
	// qty = notional / price; dollar risk = qty × dist.
	wideRisk := wide / price * wideDist
	tightRisk := tight / price * tightDist
	if math.Abs(wideRisk-10) > 1e-9 || math.Abs(tightRisk-10) > 1e-9 {
		t.Fatalf("dollar risk must be constant $10: wide=%g tight=%g", wideRisk, tightRisk)
	}
	if tight <= wide {
		t.Fatalf("tighter stop must size larger notional: tight=%g wide=%g", tight, wide)
	}
}

// #1268 — the derived notional never exceeds cash × exchange_leverage.
func TestPerpsRiskBasedNotionalExchangeCap(t *testing.T) {
	// risk $10 with a $1 stop distance at price $2000 wants $20k notional;
	// cash $1000 × 5x leverage caps it at $5000.
	got := PerpsRiskBasedNotional(1000, 2000, 1.0, 1.0, 5)
	if got != 5000 {
		t.Fatalf("notional = %g, want 5000 (cash × exchange_leverage cap)", got)
	}
	// Leverage <= 0 normalizes to 1x.
	got = PerpsRiskBasedNotional(1000, 2000, 1.0, 1.0, 0)
	if got != 1000 {
		t.Fatalf("notional = %g, want 1000 (1x cap when exchangeLeverage<=0)", got)
	}
}

func TestPerpsRiskBasedNotionalBadInputs(t *testing.T) {
	cases := []struct {
		name                               string
		cash, price, pct, dist, exchangeLv float64
	}{
		{"zero cash", 0, 2000, 1, 20, 1},
		{"negative cash", -5, 2000, 1, 20, 1},
		{"zero price", 1000, 0, 1, 20, 1},
		{"zero pct", 1000, 2000, 0, 20, 1},
		{"zero dist", 1000, 2000, 1, 0, 1},
		{"negative dist", 1000, 2000, 1, -3, 1},
	}
	for _, tc := range cases {
		if got := PerpsRiskBasedNotional(tc.cash, tc.price, tc.pct, tc.dist, tc.exchangeLv); got != 0 {
			t.Errorf("%s: notional = %g, want 0", tc.name, got)
		}
	}
}

func TestPerpsRiskStopDistance(t *testing.T) {
	base := func() StrategyConfig {
		return StrategyConfig{ID: "hl-x", Type: "perps", Platform: "hyperliquid"}
	}
	t.Run("trailing ATR owner", func(t *testing.T) {
		sc := base()
		sc.TrailingStopATRMult = fp(2.0)
		dist, ok, reason := PerpsRiskStopDistance(sc, 2000, 15)
		if !ok || dist != 30 {
			t.Fatalf("got (%g, %v, %q), want (30, true, \"\")", dist, ok, reason)
		}
	})
	t.Run("fixed ATR owner", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = fp(1.5)
		dist, ok, _ := PerpsRiskStopDistance(sc, 2000, 10)
		if !ok || dist != 15 {
			t.Fatalf("got (%g, %v), want (15, true)", dist, ok)
		}
	})
	t.Run("ATR owner with no ATR fails closed", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = fp(1.0)
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 0); ok || !strings.Contains(reason, "ATR") {
			t.Fatalf("expected ATR failure, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("NaN ATR fails closed", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = fp(1.0)
		if _, ok, _ := PerpsRiskStopDistance(sc, 2000, math.NaN()); ok {
			t.Fatal("NaN ATR must fail closed")
		}
	})
	t.Run("implausible ATR fails closed", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = fp(1.0)
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 1500); ok || !strings.Contains(reason, "implausible") {
			t.Fatalf("ATR > 50%% of price must fail closed, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("fixed pct owner", func(t *testing.T) {
		sc := base()
		sc.StopLossPct = fp(2.0)
		dist, ok, _ := PerpsRiskStopDistance(sc, 2000, 0)
		if !ok || dist != 40 {
			t.Fatalf("got (%g, %v), want (40, true) — price × 2%%", dist, ok)
		}
	})
	t.Run("trailing pct owner", func(t *testing.T) {
		sc := base()
		sc.TrailingStopPct = fp(3.0)
		dist, ok, _ := PerpsRiskStopDistance(sc, 2000, 0)
		if !ok || dist != 60 {
			t.Fatalf("got (%g, %v), want (60, true)", dist, ok)
		}
	})
	t.Run("margin pct owner derives via leverage", func(t *testing.T) {
		sc := base()
		sc.StopLossMarginPct = fp(20.0)
		sc.Leverage = 10
		dist, ok, _ := PerpsRiskStopDistance(sc, 2000, 0)
		if !ok || dist != 40 {
			t.Fatalf("got (%g, %v), want (40, true) — price × (20/10)%%", dist, ok)
		}
	})
	t.Run("regime SL owner fails closed", func(t *testing.T) {
		sc := base()
		sc.StopLossATRRegime = &RegimeATRBlock{
			UseDefaults: true,
			TrendRegime: cloneRegimeMap(regimeATRDefaults.StopLoss),
			raw:         map[string]interface{}{"use_defaults": true},
		}
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 15); ok || !strings.Contains(reason, "stop_loss_atr_regime") {
			t.Fatalf("regime owner must fail closed, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("unified regime close fails closed", func(t *testing.T) {
		sc := base()
		sc.CloseStrategy = &StrategyRef{
			Name: "tiered_tp_atr_regime",
			Params: map[string]interface{}{
				regimeClassifierKey: map[string]interface{}{
					"ranging": map[string]interface{}{"tp_tiers": []interface{}{}, "stop_loss_atr": 1.0},
				},
			},
		}
		if !strategyUsesUnifiedRegimeClose(sc) {
			t.Fatal("fixture must register as unified regime close")
		}
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 15); ok || !strings.Contains(reason, "unified") {
			t.Fatalf("unified close must fail closed, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("explicitly disabled stop fails closed", func(t *testing.T) {
		sc := base()
		sc.StopLossPct = fp(0)
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 15); ok || !strings.Contains(reason, "disables") {
			t.Fatalf("disabled stop must fail closed, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("no stop owner (mdd fallback) fails closed", func(t *testing.T) {
		sc := base()
		sc.MaxDrawdownPct = 30
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 15); ok || !strings.Contains(reason, "max_drawdown_pct") {
			t.Fatalf("mdd fallback must not be a sizing stop, got ok=%v reason=%q", ok, reason)
		}
	})
}

func TestEffectiveRiskPerTradePct(t *testing.T) {
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", RiskPerTradePct: fp(1.5)}
	if got := EffectiveRiskPerTradePct(sc); got != 1.5 {
		t.Fatalf("got %g, want 1.5", got)
	}
	sc.Platform = "okx"
	if got := EffectiveRiskPerTradePct(sc); got != 0 {
		t.Fatalf("non-HL platform must return 0, got %g", got)
	}
	sc = StrategyConfig{Type: "perps", Platform: "hyperliquid"}
	if got := EffectiveRiskPerTradePct(sc); got != 0 {
		t.Fatalf("nil field must return 0, got %g", got)
	}
}

func TestPerpsSizingFor(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-x", Type: "perps", Platform: "hyperliquid",
		Leverage: 5, RiskPerTradePct: fp(1.0), TrailingStopATRMult: fp(2.0),
	}
	s := PerpsSizingFor(sc, 2000, 15)
	if s.RiskPerTradePct != 1.0 || s.RiskStopDistance != 30 || s.RiskStopUnresolved != "" {
		t.Fatalf("resolved sizing = %+v, want pct=1 dist=30", s)
	}
	if s.ExchangeLeverage != 5 {
		t.Fatalf("exchange leverage = %g, want 5", s.ExchangeLeverage)
	}
	// No ATR in the payload → unresolved, distance 0, reason carried.
	s = PerpsSizingFor(sc, 2000, 0)
	if s.RiskStopDistance != 0 || s.RiskStopUnresolved == "" {
		t.Fatalf("unresolved sizing = %+v, want dist=0 with reason", s)
	}
	// Config without the field resolves to pure notional sizing.
	sc.RiskPerTradePct = nil
	s = PerpsSizingFor(sc, 2000, 15)
	if s.RiskPerTradePct != 0 || s.RiskStopDistance != 0 {
		t.Fatalf("notional-mode sizing = %+v, want zero risk fields", s)
	}
}

func TestValidateRiskPerTradePct(t *testing.T) {
	base := func() StrategyConfig {
		return StrategyConfig{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			StopLossATRMult: fp(1.0), RiskPerTradePct: fp(1.0),
		}
	}
	if errs := validateRiskPerTradePct(base(), "strategy[hl-x]"); len(errs) != 0 {
		t.Fatalf("valid config must pass, got %v", errs)
	}
	sc := base()
	sc.RiskPerTradePct = nil
	if errs := validateRiskPerTradePct(sc, "p"); len(errs) != 0 {
		t.Fatalf("unset field must pass, got %v", errs)
	}
	expectErr := func(t *testing.T, sc StrategyConfig, substr string) {
		t.Helper()
		errs := validateRiskPerTradePct(sc, "p")
		for _, e := range errs {
			if strings.Contains(e, substr) {
				return
			}
		}
		t.Fatalf("expected error containing %q, got %v", substr, errs)
	}
	t.Run("bounds", func(t *testing.T) {
		sc := base()
		sc.RiskPerTradePct = fp(0)
		expectErr(t, sc, "(0, 10]")
		sc.RiskPerTradePct = fp(12)
		expectErr(t, sc, "(0, 10]")
	})
	t.Run("mutually exclusive with sizing_leverage", func(t *testing.T) {
		sc := base()
		sc.SizingLeverage = 2
		expectErr(t, sc, "sizing_leverage")
	})
	t.Run("mutually exclusive with margin_per_trade_usd", func(t *testing.T) {
		sc := base()
		sc.MarginPerTradeUSD = fp(50)
		expectErr(t, sc, "margin_per_trade_usd")
	})
	t.Run("incompatible with scale-in", func(t *testing.T) {
		sc := base()
		sc.AllowScaleIn = true
		expectErr(t, sc, "allow_scale_in")
	})
	t.Run("HL perps only", func(t *testing.T) {
		sc := base()
		sc.Platform = "okx"
		expectErr(t, sc, "HL perps")
		sc = base()
		sc.Type = "manual"
		expectErr(t, sc, "HL perps")
	})
	t.Run("unresolvable stop owner", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = nil
		sc.StopLossATRRegime = &RegimeATRBlock{
			UseDefaults: true,
			TrendRegime: cloneRegimeMap(regimeATRDefaults.StopLoss),
			raw:         map[string]interface{}{"use_defaults": true},
		}
		expectErr(t, sc, "resolvable at sizing time")
	})
	t.Run("no stop owner at all", func(t *testing.T) {
		sc := base()
		sc.StopLossATRMult = nil
		expectErr(t, sc, "resolvable at sizing time")
	})
}

// #1268 — live sizer in risk mode: constant dollar risk, fail-closed fresh
// open, flip degrade to close-only, and closes never blocked.
func TestPerpsLiveOrderSizeRiskMode(t *testing.T) {
	riskSizing := func(dist float64) PerpsSizing {
		s := PerpsSizing{ExchangeLeverage: 1, RiskPerTradePct: 1.0, RiskStopDistance: dist}
		if dist <= 0 {
			s.RiskStopUnresolved = "no positive ATR in check payload"
		}
		return s
	}
	t.Run("fresh open sizes qty from stop distance", func(t *testing.T) {
		// cash $1000, risk 1% = $10; dist $20 → qty 0.5; dist $60 → qty 1/6.
		size1, ok1, _ := perpsLiveOrderSize(1, 2000, 1000, 0, 0, riskSizing(20), "", DirectionLong, 0)
		size2, ok2, _ := perpsLiveOrderSize(1, 2000, 1000, 0, 0, riskSizing(60), "", DirectionLong, 0)
		if !ok1 || !ok2 {
			t.Fatalf("both opens must size, got ok1=%v ok2=%v", ok1, ok2)
		}
		if math.Abs(size1*20-10) > 1e-9 || math.Abs(size2*60-10) > 1e-9 {
			t.Fatalf("dollar risk must be $10 for both: size1×20=%g size2×60=%g", size1*20, size2*60)
		}
	})
	t.Run("unresolvable stop refuses fresh open", func(t *testing.T) {
		size, ok, reason := perpsLiveOrderSize(1, 2000, 1000, 0, 0, riskSizing(0), "", DirectionLong, 0)
		if ok || size != 0 {
			t.Fatalf("open must be refused, got size=%g ok=%v", size, ok)
		}
		if !strings.Contains(reason, "fail-closed") || !strings.Contains(reason, "no positive ATR") {
			t.Fatalf("reason must carry the resolver cause, got %q", reason)
		}
	})
	t.Run("unresolvable stop degrades flip to close-only", func(t *testing.T) {
		size, ok, _ := perpsLiveOrderSize(1, 2000, 1000, 0.4, 2100, riskSizing(0), "short", DirectionBoth, 0)
		if !ok || size != 0.4 {
			t.Fatalf("flip must degrade to close-only posQty, got size=%g ok=%v", size, ok)
		}
	})
	t.Run("close never blocked by risk mode", func(t *testing.T) {
		size, ok, _ := perpsLiveOrderSize(-1, 2000, 1000, 0.4, 2000, riskSizing(0), "long", DirectionLong, 0)
		if !ok || size != 0.4 {
			t.Fatalf("close must pass through, got size=%g ok=%v", size, ok)
		}
	})
	t.Run("exchange cap bounds the flip open leg", func(t *testing.T) {
		// dist $1 wants $20k notional; 1x leverage caps at post-close cash.
		size, ok, _ := perpsLiveOrderSize(1, 2000, 1000, 0.1, 2000, riskSizing(1), "short", DirectionBoth, 0)
		if !ok {
			t.Fatal("flip must size")
		}
		newSide := size - 0.1 // flip order = posQty + newSize
		if notional := newSide * 2000; notional > 1000+1e-9 {
			t.Fatalf("new-side notional %g exceeds cash × 1x cap", notional)
		}
	})
}

// #1268 — paper executor in risk mode: equal dollar risk across differing
// stops, fail-closed skip leaves state untouched, and legacy behavior stays
// byte-identical when the field is unset.
func TestExecutePerpsSignalRiskMode(t *testing.T) {
	logger := &StrategyLogger{stratID: "test", writer: os.Stdout}
	mkState := func() *StrategyState {
		return &StrategyState{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			Cash: 1000, Positions: map[string]*Position{},
		}
	}
	// 2x exchange leverage keeps the cash × leverage cap from binding on the
	// tight-stop case (slippage nudges the wanted notional just past 1× cash).
	riskSizing := func(dist float64) PerpsSizing {
		s := PerpsSizing{ExchangeLeverage: 2, RiskPerTradePct: 1.0, RiskStopDistance: dist}
		if dist <= 0 {
			s.RiskStopUnresolved = "no positive ATR in check payload"
		}
		return s
	}
	t.Run("open long sizes constant dollar risk", func(t *testing.T) {
		sTight := mkState()
		sWide := mkState()
		if _, err := ExecutePerpsSignalWithLeverage(sTight, 1, "ETH", 2000, riskSizing(20), 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		if _, err := ExecutePerpsSignalWithLeverage(sWide, 1, "ETH", 2000, riskSizing(60), 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		pt, pw := sTight.Positions["ETH"], sWide.Positions["ETH"]
		if pt == nil || pw == nil {
			t.Fatal("both opens must create positions")
		}
		// qty = budget/execPrice and budget = riskDollars/dist × execPrice, so
		// qty × dist = riskDollars exactly, independent of slippage.
		if math.Abs(pt.Quantity*20-10) > 1e-9 || math.Abs(pw.Quantity*60-10) > 1e-9 {
			t.Fatalf("dollar risk must be $10: tight=%g wide=%g", pt.Quantity*20, pw.Quantity*60)
		}
	})
	t.Run("open short sizes constant dollar risk", func(t *testing.T) {
		s := mkState()
		if _, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, riskSizing(20), 0, "", 0, DirectionShort, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["ETH"]
		if pos == nil || pos.Side != "short" {
			t.Fatalf("expected short position, got %+v", pos)
		}
		if math.Abs(pos.Quantity*20-10) > 1e-9 {
			t.Fatalf("dollar risk must be $10, got %g", pos.Quantity*20)
		}
	})
	t.Run("unresolvable stop refuses open and mutates nothing", func(t *testing.T) {
		s := mkState()
		trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, riskSizing(0), 0, "", 0, DirectionLong, 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 0 || len(s.Positions) != 0 || s.Cash != 1000 {
			t.Fatalf("fail-closed open must not mutate state: trades=%d positions=%d cash=%g", trades, len(s.Positions), s.Cash)
		}
	})
	t.Run("risk-mode flip closes then sizes new side from post-close cash", func(t *testing.T) {
		s := mkState()
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.4, AvgCost: 2100, Side: "short", Multiplier: 1, OwnerStrategyID: "hl-x"}
		if _, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, riskSizing(20), 0, "", 0, DirectionBoth, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["ETH"]
		if pos == nil || pos.Side != "long" {
			t.Fatalf("expected flipped long, got %+v", pos)
		}
		// New side risks 1% of post-close cash (short closed in profit).
		if riskDollars := pos.Quantity * 20; math.Abs(riskDollars-s.Cash*0.01) > s.Cash*0.01*0.01 {
			t.Fatalf("flip risk %g must be ~1%% of post-close cash %g", riskDollars, s.Cash)
		}
	})
	t.Run("unset field keeps legacy sizing byte-identical", func(t *testing.T) {
		s := mkState()
		if _, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 5, ExchangeLeverage: 5}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["ETH"]
		if pos == nil {
			t.Fatal("expected position")
		}
		// ApplySlippage is randomized, so assert via the stamped fill price:
		// notional must be exactly cash × sizing_leverage.
		if notional := pos.Quantity * pos.AvgCost; math.Abs(notional-5000) > 1e-6 {
			t.Fatalf("legacy sizing notional = %g, want 5000 (cash × sizing_leverage)", notional)
		}
	})
}

// #1268 — full load path: a risk-mode strategy with NO stop fields acquires
// the default_stop_loss_atr_mult owner BEFORE validation runs (ordering the
// validator depends on), and the mutual-exclusion rejects fire through
// LoadConfig with the strategy id.
func TestRiskPerTradePct_LoadConfigEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Run("default stop owner materializes before validation", func(t *testing.T) {
		path := writeTestConfig(t, dir, `{
			"strategies": [{
				"id": "hl-risk-eth",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"risk_per_trade_pct": 1.0
			}]
		}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}
		sc := cfg.Strategies[0]
		if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
			t.Fatalf("expected defaulted stop_loss_atr_mult=1.0 stop owner, got %+v", sc.StopLossATRMult)
		}
		if got := EffectiveRiskPerTradePct(sc); got != 1.0 {
			t.Fatalf("EffectiveRiskPerTradePct = %g, want 1.0", got)
		}
	})
	t.Run("sizing_leverage combo rejected at load", func(t *testing.T) {
		path := writeTestConfig(t, dir, `{
			"strategies": [{
				"id": "hl-risk-eth",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"sizing_leverage": 2,
				"risk_per_trade_pct": 1.0
			}]
		}`)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected mutual-exclusion rejection, got: %v", err)
		}
	})
}

// #1268 — init --json surface: perpsRiskPerTradePct emits risk_per_trade_pct
// and suppresses the sizing_leverage field (mutual exclusion).
func TestGenerateConfig_RiskPerTradePct(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.SpotStrategies = nil
	opts.EnablePerps = true
	opts.PerpsStrategies = []string{"momentum"}
	opts.PerpsRiskPerTradePct = 1.5

	cfg := generateConfig(opts)
	sc, ok := findStrategy(cfg, "hl-momentum-btc")
	if !ok {
		t.Fatalf("expected strategy hl-momentum-btc, got %v", cfg.Strategies)
	}
	if sc.RiskPerTradePct == nil || *sc.RiskPerTradePct != 1.5 {
		t.Fatalf("risk_per_trade_pct = %+v, want 1.5", sc.RiskPerTradePct)
	}
	if sc.SizingLeverage != 0 {
		t.Fatalf("sizing_leverage = %g, want 0 (suppressed under risk mode)", sc.SizingLeverage)
	}
	if err := validateConfig(cfg, true); err == nil {
		// generateConfig emits no stop field; the daemon's LoadConfig
		// materializes the default stop owner before validation. Direct
		// validateConfig on the raw generated config is therefore expected
		// to fail on the missing stop owner and NOTHING else.
		t.Log("validateConfig passed — a stop owner was emitted directly")
	} else if !strings.Contains(err.Error(), "resolvable at sizing time") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

// #1268 — hot reload: risk↔notional mode switches are blocked while a
// position is open; value tweaks and flat-state switches pass.
func TestValidateHotReloadStateCompatible_RiskPerTradePctModeSwitch(t *testing.T) {
	mkSC := func(risk *float64, sizingLev float64) StrategyConfig {
		return StrategyConfig{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			SizingLeverage: sizingLev, RiskPerTradePct: risk, StopLossATRMult: fp(1.0),
		}
	}
	mkCfg := func(sc StrategyConfig) *Config {
		return &Config{Strategies: []StrategyConfig{sc}}
	}
	openState := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-x": {
				ID: "hl-x",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"},
				},
			},
		},
	}
	flatState := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-x": {ID: "hl-x", Positions: map[string]*Position{}},
		},
	}
	// notional → risk while open: REJECTED.
	err := validateHotReloadStateCompatible(mkCfg(mkSC(nil, 2)), mkCfg(mkSC(fp(1.0), 0)), openState)
	if err == nil || !strings.Contains(err.Error(), "risk_per_trade_pct sizing mode changed") {
		t.Fatalf("expected mode-switch rejection while open, got: %v", err)
	}
	// risk → notional while open: REJECTED.
	err = validateHotReloadStateCompatible(mkCfg(mkSC(fp(1.0), 0)), mkCfg(mkSC(nil, 2)), openState)
	if err == nil || !strings.Contains(err.Error(), "risk_per_trade_pct sizing mode changed") {
		t.Fatalf("expected mode-switch rejection while open, got: %v", err)
	}
	// Same switch while flat: ACCEPTED.
	if err := validateHotReloadStateCompatible(mkCfg(mkSC(nil, 2)), mkCfg(mkSC(fp(1.0), 0)), flatState); err != nil {
		t.Fatalf("flat mode switch must pass, got: %v", err)
	}
	// Value tweak while open: ACCEPTED.
	if err := validateHotReloadStateCompatible(mkCfg(mkSC(fp(1.0), 0)), mkCfg(mkSC(fp(2.0), 0)), openState); err != nil {
		t.Fatalf("value tweak while open must pass, got: %v", err)
	}
}
