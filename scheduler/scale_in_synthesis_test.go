package main

import (
	"testing"
)

func TestScaleInFreezesFixedSLGeometry(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &mult}
	pos := &Position{
		Side: "long", Quantity: 200, InitialQuantity: 200,
		AvgCost: 2100, EntryATR: 50, RiskAnchorPrice: 2000, StopLossATRMult: &mult,
	}
	got := fixedStopLossATRTriggerPx(sc, "long", pos)
	if !approxEq(got, 1925) {
		t.Fatalf("fixed SL trigger = %v, want 1925 (frozen at riskAnchorPrice, not blended AvgCost)", got)
	}
}
