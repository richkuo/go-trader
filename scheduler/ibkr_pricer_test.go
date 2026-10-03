package main

import (
	"math"
	"testing"
	"time"
)

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

func TestIBKRPricerGetOptionPriceFull(t *testing.T) {
	futureExpiry := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	cases := []struct {
		name       string
		prices     map[string]float64
		underlying string
		expiry     string
		wantErr    bool
		check      func(t *testing.T, markPrice, spotPrice float64, greeks OptGreeks)
	}{
		{
			name:       "future expiry prices and greeks",
			prices:     map[string]float64{"BTC/USDT": 60000},
			underlying: "BTC",
			expiry:     futureExpiry,
			check: func(t *testing.T, markPrice, spotPrice float64, greeks OptGreeks) {
				if spotPrice != 60000 {
					t.Errorf("spotPrice = %g, want 60000", spotPrice)
				}
				if markPrice <= 0 {
					t.Errorf("markPrice should be > 0 for future expiry, got %g", markPrice)
				}
				if greeks.Delta <= 0 {
					t.Errorf("call delta should be > 0, got %g", greeks.Delta)
				}
			},
		},
		{
			name:       "expired option zeroes mark and greeks",
			prices:     map[string]float64{"BTC/USDT": 60000},
			underlying: "BTC",
			expiry:     "2020-01-01",
			check: func(t *testing.T, markPrice, spotPrice float64, greeks OptGreeks) {
				if spotPrice != 60000 {
					t.Errorf("spotPrice = %g, want 60000", spotPrice)
				}
				if markPrice != 0 {
					t.Errorf("expired option markPrice should be 0, got %g", markPrice)
				}
				if greeks.Delta != 0 {
					t.Errorf("expired option delta should be 0, got %g", greeks.Delta)
				}
			},
		},
		{
			name:       "invalid expiry format",
			prices:     map[string]float64{"BTC/USDT": 60000},
			underlying: "BTC",
			expiry:     "not-a-date",
			wantErr:    true,
		},
		{
			name:       "unknown underlying",
			prices:     map[string]float64{},
			underlying: "UNKNOWN",
			expiry:     futureExpiry,
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pricer := NewIBKRPricer(tc.prices)
			markPrice, spotPrice, greeks, err := pricer.GetOptionPriceFull(tc.underlying, "call", 60000, tc.expiry)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, markPrice, spotPrice, greeks)
		})
	}
}
