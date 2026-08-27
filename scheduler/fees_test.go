package main

import (
	"math"
	"testing"
)

func TestCalculateFuturesFee(t *testing.T) {
	cases := []struct {
		contracts      int
		feePerContract float64
		want           float64
	}{
		{1, 1.50, 1.50},
		{2, 1.50, 3.00},
		{10, 0.50, 5.00},
		{0, 1.50, 0.00},
		{5, 0, 0.00},
	}
	for _, tc := range cases {
		got := CalculateFuturesFee(tc.contracts, tc.feePerContract)
		if math.Abs(got-tc.want) > 0.001 {
			t.Errorf("CalculateFuturesFee(%d, %.2f) = %.2f, want %.2f", tc.contracts, tc.feePerContract, got, tc.want)
		}
	}
}

func TestHyperliquidFeeConstants(t *testing.T) {

	if HyperliquidTakerFeePct != 0.00045 {
		t.Errorf("HyperliquidTakerFeePct = %v, want 0.00045", HyperliquidTakerFeePct)
	}
	if HyperliquidMakerFeePct != 0.00015 {
		t.Errorf("HyperliquidMakerFeePct = %v, want 0.00015", HyperliquidMakerFeePct)
	}
	if HyperliquidMakerFeePct >= HyperliquidTakerFeePct {
		t.Errorf("maker rate %v must be below taker rate %v", HyperliquidMakerFeePct, HyperliquidTakerFeePct)
	}

	if got := CalculateHyperliquidFee(1000.0); got != 1000.0*HyperliquidTakerFeePct {
		t.Errorf("CalculateHyperliquidFee(1000) = %v, want %v", got, 1000.0*HyperliquidTakerFeePct)
	}
}

func TestCalculatePlatformSpotFeeOKX(t *testing.T) {

	fee := CalculatePlatformSpotFee("okx", 1000.0)
	expected := 1000.0 * OKXSpotTakerFeePct
	if fee != expected {
		t.Errorf("OKX spot fee: got %f, want %f", fee, expected)
	}
}

func TestCalculatePlatformSpotFeeOKXPerps(t *testing.T) {

	fee := CalculatePlatformSpotFee("okx-perps", 1000.0)
	expected := 1000.0 * OKXPerpsTakerFeePct
	if fee != expected {
		t.Errorf("OKX perps fee: got %f, want %f", fee, expected)
	}
}

func TestCalculatePlatformFuturesFee(t *testing.T) {

	sc := StrategyConfig{
		FuturesConfig: &FuturesConfig{FeePerContract: 1.50},
	}
	got := CalculatePlatformFuturesFee(sc, 3)
	if math.Abs(got-4.50) > 0.001 {
		t.Errorf("expected 4.50, got %.2f", got)
	}

	sc2 := StrategyConfig{}
	got2 := CalculatePlatformFuturesFee(sc2, 3)
	if got2 != 0 {
		t.Errorf("expected 0 with no FuturesConfig, got %.2f", got2)
	}
}
