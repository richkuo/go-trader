package main

import (
	"math"
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
