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

func TestCalculatePlatformSpotFee(t *testing.T) {
	cases := []struct {
		platform string
		want     float64
	}{
		{"okx", 1000.0 * OKXSpotTakerFeePct},
		{"okx-perps", 1000.0 * OKXPerpsTakerFeePct},
	}
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			if fee := CalculatePlatformSpotFee(tc.platform, 1000.0); fee != tc.want {
				t.Errorf("%s fee: got %f, want %f", tc.platform, fee, tc.want)
			}
		})
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
