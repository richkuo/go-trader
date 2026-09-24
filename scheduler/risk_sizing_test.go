package main

import (
	"math"
	"os"
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

func floatPtr(v float64) *float64 { return &v }

func TestPerpsRiskBasedNotionalConstantDollarRisk(t *testing.T) {
	cash, price, riskPct := 1000.0, 2000.0, 1.0
	wideDist := 60.0
	tightDist := 20.0

	wide := PerpsRiskBasedNotional(cash, price, riskPct, wideDist, 10)
	tight := PerpsRiskBasedNotional(cash, price, riskPct, tightDist, 10)
	if wide <= 0 || tight <= 0 {
		t.Fatalf("expected positive notionals, got wide=%g tight=%g", wide, tight)
	}
	wideRisk := wide / price * wideDist
	tightRisk := tight / price * tightDist
	if math.Abs(wideRisk-10) > 1e-9 || math.Abs(tightRisk-10) > 1e-9 {
		t.Fatalf("dollar risk must be constant $10: wide=%g tight=%g", wideRisk, tightRisk)
	}
	if tight <= wide {
		t.Fatalf("tighter stop must size larger notional: tight=%g wide=%g", tight, wide)
	}
}

func TestPerpsRiskBasedNotionalExchangeCap(t *testing.T) {
	got := PerpsRiskBasedNotional(1000, 2000, 1.0, 1.0, 5)
	if got != 5000 {
		t.Fatalf("notional = %g, want 5000 (cash × exchange_leverage cap)", got)
	}
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
		sc.StopLossATRMultRegime = &RegimeATRBlock{
			UseDefaults: true,
			TrendRegime: cloneRegimeMap(regimeATRDefaults.StopLoss),
			raw:         map[string]interface{}{"use_defaults": true},
		}
		if _, ok, reason := PerpsRiskStopDistance(sc, 2000, 15); ok || !strings.Contains(reason, "stop_loss_atr_mult_regime") {
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

func TestPerpsLiveOrderSizeRiskMode(t *testing.T) {
	riskSizing := func(dist float64) PerpsSizing {
		s := PerpsSizing{ExchangeLeverage: 1, RiskPerTradePct: 1.0, RiskStopDistance: dist}
		if dist <= 0 {
			s.RiskStopUnresolved = "no positive ATR in check payload"
		}
		return s
	}
	t.Run("fresh open sizes qty from stop distance", func(t *testing.T) {
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
		size, ok, _ := perpsLiveOrderSize(1, 2000, 1000, 0.1, 2000, riskSizing(1), "short", DirectionBoth, 0)
		if !ok {
			t.Fatal("flip must size")
		}
		newSide := size - 0.1
		if notional := newSide * 2000; notional > 1000+1e-9 {
			t.Fatalf("new-side notional %g exceeds cash × 1x cap", notional)
		}
	})
}

func TestExecutePerpsSignalRiskMode(t *testing.T) {
	logger := &StrategyLogger{stratID: "test", writer: os.Stdout}
	mkState := func() *StrategyState {
		return &StrategyState{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			Cash: 1000, Positions: map[string]*Position{},
		}
	}
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
		if notional := pos.Quantity * pos.AvgCost; math.Abs(notional-5000) > 1e-6 {
			t.Fatalf("legacy sizing notional = %g, want 5000 (cash × sizing_leverage)", notional)
		}
	})
}
