package main

import (
	"math"
	"testing"
	"time"
)

func TestIBKROptionPricing(t *testing.T) {
	t.Run("BsPricePutCallParity", func(t *testing.T) {
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
	})
	t.Run("IBKRPricerRefresh", func(t *testing.T) {
		refreshAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		futureExpiry := refreshAt.AddDate(1, 0, 0).Format("2006-01-02")
		cases := []struct {
			name       string
			prices     map[string]float64
			underlying string
			expiry     string
			wantErr    bool
			check      func(t *testing.T, r optionLegReply)
		}{
			{
				name:       "future expiry prices and greeks",
				prices:     map[string]float64{"BTC/USDT": 60000},
				underlying: "BTC",
				expiry:     futureExpiry,
				check: func(t *testing.T, r optionLegReply) {
					if r.Mark == nil {
						t.Fatalf("no mark: %+v", r)
					}
					if r.Mark.Spot != 60000 {
						t.Errorf("spotPrice = %g, want 60000", r.Mark.Spot)
					}
					if r.Mark.PerUnitUSD <= 0 {
						t.Errorf("mark value should be > 0 for future expiry, got %g", r.Mark.PerUnitUSD)
					}
					if r.Mark.Greeks.Delta <= 0 {
						t.Errorf("call delta should be > 0, got %g", r.Mark.Greeks.Delta)
					}
					if r.Mark.Source != ibkrMarkSource {
						t.Errorf("source = %q, want %q", r.Mark.Source, ibkrMarkSource)
					}
				},
			},
			{
				name:       "expired option settles at the labelled cycle price",
				prices:     map[string]float64{"BTC/USDT": 60000},
				underlying: "BTC",
				expiry:     "2020-01-01",
				check: func(t *testing.T, r optionLegReply) {
					if r.Mark != nil || r.Settlement == nil {
						t.Fatalf("expired leg reply = %+v, want a settlement and no mark", r)
					}
					if r.Settlement.Price != 60000 || r.Settlement.Source != ibkrSettlementSource || r.Settlement.Basis != optionSettlementBasisIBKR {
						t.Errorf("settlement = %+v", r.Settlement)
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
				replies := pricer.Refresh([]optionLegRequest{{Key: "leg", Underlying: tc.underlying, OptionType: "call", Strike: 60000, Expiry: tc.expiry, Action: "buy", Quantity: 1, Multiplier: 1}}, refreshAt)
				if len(replies) != 1 {
					t.Fatalf("replies = %+v, want one", replies)
				}
				if tc.wantErr {
					if replies[0].Err == "" {
						t.Fatalf("expected error, got %+v", replies[0])
					}
					return
				}
				if replies[0].Err != "" {
					t.Fatalf("unexpected error: %v", replies[0].Err)
				}
				tc.check(t, replies[0])
			})
		}
	})
}
