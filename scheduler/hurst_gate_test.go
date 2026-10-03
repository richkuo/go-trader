package main

import (
	"math"
	"testing"
)

func TestHurstSizeMultiplierFormulaAndClamps(t *testing.T) {
	cases := []struct {
		h, floor, want float64
	}{
		{0.500, 0.25, 0.25},
		{0.5375, 0.25, 0.25},
		{0.575, 0.25, 0.5},
		{0.65, 0.25, 1.0},
		{0.80, 0.25, 1.0},
		{0.35, 0.25, 1.0},
		{2.0033, 0.25, 1.0},
		{0.50, 0.9, 0.9},
		{0.625, hurstDefaultSizeFloor, 0.8333333333333334},
	}
	for _, tc := range cases {
		got := hurstSizeMultiplier(tc.h, tc.floor)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("h=%v floor=%v: got %v want %v", tc.h, tc.floor, got, tc.want)
		}
		if got > 1.0 {
			t.Fatalf("h=%v: multiplier %v exceeds 1.0 — the gate must never grow an open", tc.h, got)
		}
	}
	if got := hurstSizeMultiplier(math.NaN(), 0.25); got != 1.0 {
		t.Fatalf("NaN H must resolve to a neutral 1.0 multiplier, got %v", got)
	}
}

func TestPerpsOpenNotionalSizedAppliesEntryMultInNotionalMode(t *testing.T) {
	sizing := PerpsSizing{SizingLeverage: 2, ExchangeLeverage: 5}
	full := PerpsOpenNotionalSized(1000, 100, sizing)
	half := PerpsOpenNotionalSized(1000, 100, withEntrySizeMult(sizing, 0.5))
	if math.Abs(half-full*0.5) > 1e-9 {
		t.Fatalf("multiplier should halve notional: full=%v scaled=%v", full, half)
	}
	if PerpsOpenNotionalSized(1000, 100, sizing) != PerpsOpenNotional(1000, 2, 5, 0) {
		t.Fatal("zero-value EntrySizeMult must be a no-op")
	}
}

func TestPerpsOpenNotionalSizedComposesWithRiskPerTradePct(t *testing.T) {
	sizing := PerpsSizing{RiskPerTradePct: 1, RiskStopDistance: 5, ExchangeLeverage: 10}
	full := PerpsOpenNotionalSized(10000, 100, sizing)
	scaled := PerpsOpenNotionalSized(10000, 100, withEntrySizeMult(sizing, 0.4))
	if math.Abs(scaled-full*0.4) > 1e-9 {
		t.Fatalf("risk-mode notional should scale linearly: full=%v scaled=%v", full, scaled)
	}
	tight := PerpsSizing{RiskPerTradePct: 5, RiskStopDistance: 0.01, ExchangeLeverage: 3}
	capped := PerpsOpenNotionalSized(1000, 100, tight)
	if math.Abs(capped-1000*3) > 1e-9 {
		t.Fatalf("expected the cash x leverage cap to bind, got %v", capped)
	}
	cappedScaled := PerpsOpenNotionalSized(1000, 100, withEntrySizeMult(tight, 0.25))
	if cappedScaled > capped {
		t.Fatalf("scaled notional %v must never exceed the capped notional %v", cappedScaled, capped)
	}
	if math.Abs(cappedScaled-capped*0.25) > 1e-9 {
		t.Fatalf("scaling applies after the cap: got %v want %v", cappedScaled, capped*0.25)
	}
}

func TestEntrySizeMultOutOfRangeIsNeutral(t *testing.T) {
	for _, v := range []float64{-0.5, 0, 1.0001, 42} {
		s := PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1, EntrySizeMult: v}
		if s.entrySizeMult() != 1.0 {
			t.Fatalf("EntrySizeMult=%v must resolve neutral, got %v", v, s.entrySizeMult())
		}
	}
}

func TestSpotPaperOpenScalesWithMultiplierAndClosesDoNot(t *testing.T) {
	logger := silentStrategyLogger("spot-hurst")
	newState := func() *StrategyState {
		return &StrategyState{ID: "spot-hurst", Type: "spot", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	}
	full := newState()
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(full, 1, "BTC/USD", 100, 0, 0, "", 0, 1.0, logger); err != nil {
		t.Fatal(err)
	}
	scaled := newState()
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(scaled, 1, "BTC/USD", 100, 0, 0, "", 0, 0.5, logger); err != nil {
		t.Fatal(err)
	}
	fullPos, scaledPos := full.Positions["BTC/USD"], scaled.Positions["BTC/USD"]
	fn := fullPos.Quantity * fullPos.AvgCost
	sn := scaledPos.Quantity * scaledPos.AvgCost
	if math.Abs(fn-1000) > 1e-6 {
		t.Fatalf("unscaled open should commit the full $1000 budget, got %v", fn)
	}
	if math.Abs(sn-500) > 1e-6 {
		t.Fatalf("half multiplier should commit half the budget: got %v want 500", sn)
	}
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(scaled, -1, "BTC/USD", 110, 0, 0, "", 0, 0.5, logger); err != nil {
		t.Fatal(err)
	}
	if _, still := scaled.Positions["BTC/USD"]; still {
		t.Fatal("a close must fully exit regardless of the size multiplier")
	}
}

func TestFuturesPaperOpenFloorsToZeroAndRefuses(t *testing.T) {
	logger := silentStrategyLogger("fut-hurst")
	spec := ContractSpec{Margin: 400, Multiplier: 1}
	s := &StrategyState{ID: "fut-hurst", Type: "futures", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	res, err := ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s, 1, "ES", 100, spec, 1, 0, 0, 0, "", 0, 1.0, logger)
	if err != nil || res.TradesExecuted != 1 || s.Positions["ES"].Quantity != 2 {
		t.Fatalf("expected 2 contracts, got %+v err=%v", s.Positions["ES"], err)
	}
	s2 := &StrategyState{ID: "fut-hurst", Type: "futures", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	res2, err := ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s2, 1, "ES", 100, spec, 1, 0, 0, 0, "", 0, 0.3, logger)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TradesExecuted != 0 || len(s2.Positions) != 0 {
		t.Fatalf("a floor-to-zero scaled size must refuse the open, got %d trades / %d positions", res2.TradesExecuted, len(s2.Positions))
	}
	if s2.Cash != 1000 {
		t.Fatalf("a refused open must not move cash, got %v", s2.Cash)
	}
}
