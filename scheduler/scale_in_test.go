package main

import (
	"testing"
)

func TestApplyScaleInBlendsPriceAndSizeFreezesRiskPlan(t *testing.T) {
	mult := 1.5
	pos := &Position{
		Symbol:                   "ETH",
		Side:                     "long",
		Quantity:                 100,
		InitialQuantity:          100,
		AvgCost:                  2000,
		EntryATR:                 50,
		Regime:                   "trending",
		RegimeWindows:            map[string]string{"medium": "trending"},
		SLAdjustedTiersProcessed: 1,
		TPArmedTiers:             []bool{true, false},
		StopLossATRMult:          &mult,
	}
	applyScaleIn(pos, 100, 2200)

	if !approxEq(pos.AvgCost, 2100) {
		t.Fatalf("AvgCost = %v, want 2100", pos.AvgCost)
	}
	if !approxEq(pos.Quantity, 200) {
		t.Fatalf("Quantity = %v, want 200", pos.Quantity)
	}
	if !approxEq(pos.InitialQuantity, 200) {
		t.Fatalf("InitialQuantity = %v, want 200", pos.InitialQuantity)
	}
	if pos.ScaleInCount != 1 {
		t.Fatalf("ScaleInCount = %d, want 1", pos.ScaleInCount)
	}
	if !approxEq(pos.LastAddPrice, 2200) {
		t.Fatalf("LastAddPrice = %v, want 2200", pos.LastAddPrice)
	}
	if !approxEq(pos.AddedNotionalUSD, 100*2200) {
		t.Fatalf("AddedNotionalUSD = %v, want %v", pos.AddedNotionalUSD, 100*2200.0)
	}
	if !pos.ScaleInResizePending {
		t.Fatalf("ScaleInResizePending = false, want true")
	}
	if !approxEq(pos.EntryATR, 50) {
		t.Fatalf("EntryATR moved: %v, want 50 (frozen)", pos.EntryATR)
	}
	if pos.Regime != "trending" {
		t.Fatalf("Regime moved: %q, want trending (frozen)", pos.Regime)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("SLAdjustedTiersProcessed = %d, want 1 (watermark not reset)", pos.SLAdjustedTiersProcessed)
	}
	if len(pos.TPArmedTiers) != 2 || !pos.TPArmedTiers[0] || pos.TPArmedTiers[1] {
		t.Fatalf("TPArmedTiers changed: %v, want [true false] (watermark not reset)", pos.TPArmedTiers)
	}
}
