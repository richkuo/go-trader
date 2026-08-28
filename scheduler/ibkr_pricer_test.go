package main

import (
	"fmt"
	"math"
	"testing"
)

func TestStdNormCDF(t *testing.T) {
	cases := []struct {
		x    float64
		want float64
	}{
		{0.0, 0.5},
		{1.0, 0.8413},
		{-1.0, 0.1587},
		{2.0, 0.9772},
		{-2.0, 0.0228},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("x=%g", tc.x), func(t *testing.T) {
			got := stdNormCDF(tc.x)
			if math.Abs(got-tc.want) > 0.001 {
				t.Errorf("stdNormCDF(%g) = %g, want ~%g", tc.x, got, tc.want)
			}
		})
	}
}

func TestStdNormPDF(t *testing.T) {
	cases := []struct {
		x    float64
		want float64
	}{
		{0.0, 0.3989},
		{1.0, 0.2420},
		{-1.0, 0.2420},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("x=%g", tc.x), func(t *testing.T) {
			got := stdNormPDF(tc.x)
			if math.Abs(got-tc.want) > 0.001 {
				t.Errorf("stdNormPDF(%g) = %g, want ~%g", tc.x, got, tc.want)
			}
		})
	}
}

func TestBsPriceCallBasic(t *testing.T) {
	S := 50000.0
	K := 50000.0
	T := 1.0
	r := 0.05
	sigma := 0.80

	price, delta, gamma, vega, theta := bsPrice(S, K, T, r, sigma, "call")

	if price <= 0 {
		t.Errorf("call price should be > 0, got %g", price)
	}
	if delta < 0.4 || delta > 0.8 {
		t.Errorf("ATM call delta = %g, expected ~0.5-0.7", delta)
	}
	if gamma <= 0 {
		t.Errorf("gamma should be > 0, got %g", gamma)
	}
	if vega <= 0 {
		t.Errorf("vega should be > 0, got %g", vega)
	}
	if theta >= 0 {
		t.Errorf("theta should be < 0, got %g", theta)
	}
}

func TestBsPricePutBasic(t *testing.T) {
	S := 50000.0
	K := 50000.0
	T := 1.0
	r := 0.05
	sigma := 0.80

	price, delta, _, _, _ := bsPrice(S, K, T, r, sigma, "put")

	if price <= 0 {
		t.Errorf("put price should be > 0, got %g", price)
	}
	if delta >= 0 {
		t.Errorf("put delta should be < 0, got %g", delta)
	}
}

func TestBsPriceZeroInputs(t *testing.T) {
	cases := []struct {
		name              string
		S, K, T, r, sigma float64
	}{
		{"zero T", 100, 100, 0, 0.05, 0.3},
		{"zero sigma", 100, 100, 1, 0.05, 0},
		{"zero S", 0, 100, 1, 0.05, 0.3},
		{"zero K", 100, 0, 1, 0.05, 0.3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, delta, gamma, vega, theta := bsPrice(tc.S, tc.K, tc.T, tc.r, tc.sigma, "call")
			if price != 0 || delta != 0 || gamma != 0 || vega != 0 || theta != 0 {
				t.Errorf("expected all zeros, got price=%g delta=%g gamma=%g vega=%g theta=%g",
					price, delta, gamma, vega, theta)
			}
		})
	}
}

func TestBsPricePutCallParity(t *testing.T) {
	S := 50000.0
	K := 55000.0
	T := 0.5
	r := 0.05
	sigma := 0.80

	callPrice, _, _, _, _ := bsPrice(S, K, T, r, sigma, "call")
	putPrice, _, _, _, _ := bsPrice(S, K, T, r, sigma, "put")

	expected := S - K*math.Exp(-r*T)
	actual := callPrice - putPrice

	if math.Abs(actual-expected) > 0.01 {
		t.Errorf("Put-call parity violated: C-P = %g, S-Ke^(-rT) = %g", actual, expected)
	}
}

func TestIBKRPricerFetchSpotPrice(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 60000,
		"ETH/USD":  3000,
	}
	pricer := NewIBKRPricer(prices)

	if pricer.Name() != "ibkr" {
		t.Errorf("Name() = %q, want %q", pricer.Name(), "ibkr")
	}

	spot, err := pricer.FetchSpotPrice("BTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spot != 60000 {
		t.Errorf("spot = %g, want 60000", spot)
	}

	spot, err = pricer.FetchSpotPrice("ETH")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spot != 3000 {
		t.Errorf("spot = %g, want 3000", spot)
	}

	_, err = pricer.FetchSpotPrice("DOGE")
	if err == nil {
		t.Error("expected error for unknown underlying")
	}
}

func TestIBKRPricerGetOptionPriceFull(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 60000,
	}
	pricer := NewIBKRPricer(prices)

	markPrice, spotPrice, greeks, err := pricer.GetOptionPriceFull("BTC", "call", 60000, "2027-12-31")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spotPrice != 60000 {
		t.Errorf("spotPrice = %g, want 60000", spotPrice)
	}
	if markPrice <= 0 {
		t.Errorf("markPrice should be > 0 for future expiry, got %g", markPrice)
	}
	if greeks.Delta <= 0 {
		t.Errorf("call delta should be > 0, got %g", greeks.Delta)
	}
}

func TestIBKRPricerGetOptionPriceExpired(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 60000,
	}
	pricer := NewIBKRPricer(prices)

	markPrice, spotPrice, greeks, err := pricer.GetOptionPriceFull("BTC", "call", 60000, "2020-01-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spotPrice != 60000 {
		t.Errorf("spotPrice = %g, want 60000", spotPrice)
	}
	if markPrice != 0 {
		t.Errorf("expired option markPrice should be 0, got %g", markPrice)
	}
	if greeks.Delta != 0 {
		t.Errorf("expired option delta should be 0, got %g", greeks.Delta)
	}
}

func TestIBKRPricerInvalidExpiry(t *testing.T) {
	prices := map[string]float64{"BTC/USDT": 60000}
	pricer := NewIBKRPricer(prices)

	_, _, _, err := pricer.GetOptionPriceFull("BTC", "call", 60000, "not-a-date")
	if err == nil {
		t.Error("expected error for invalid expiry format")
	}
}

func TestIBKRPricerUnknownUnderlying(t *testing.T) {
	pricer := NewIBKRPricer(map[string]float64{})
	_, _, _, err := pricer.GetOptionPriceFull("UNKNOWN", "call", 100, "2027-12-31")
	if err == nil {
		t.Error("expected error for unknown underlying")
	}
}

var _ OptionPricer = (*IBKRPricer)(nil)
