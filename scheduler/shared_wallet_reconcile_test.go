package main

import (
	"math"
	"testing"
)

func sumValues(m map[string]float64) float64 {
	s := 0.0
	for _, v := range m {
		s += v
	}
	return s
}

func TestReconcileSharedWalletMemberValues(t *testing.T) {
	cases := []struct {
		name        string
		members     []string
		capital     map[string]float64
		positions   []SharedWalletPosition
		virtualQty  map[string]map[string]float64
		balance     float64
		wantDrift   float64
		wantValues  map[string]float64
		wantSum     float64
		sumTol      float64
		wantOrphans []string
	}{
		{
			name:       "distinct coins sum exact",
			members:    []string{"a", "b"},
			capital:    map[string]float64{"a": 600, "b": 400},
			positions:  []SharedWalletPosition{{Coin: "BTC", UnrealizedPnL: 50}, {Coin: "ETH", UnrealizedPnL: -20}},
			virtualQty: map[string]map[string]float64{"BTC": {"a": 0.1}, "ETH": {"b": 2.0}},
			balance:    1030.0,
			wantValues: map[string]float64{"a": 650, "b": 380},
			wantSum:    1030.0, sumTol: 0.01,
		},
		{
			name:       "shared coin splits by virtual qty",
			members:    []string{"a", "b"},
			capital:    map[string]float64{"a": 500, "b": 500},
			positions:  []SharedWalletPosition{{Coin: "BTC", UnrealizedPnL: 90}},
			virtualQty: map[string]map[string]float64{"BTC": {"a": 2.0, "b": 1.0}},
			balance:    1090.0,
			wantValues: map[string]float64{"a": 560, "b": 530},
			wantSum:    1090.0, sumTol: 0.01,
		},
		{
			name:       "orphan position surfaces as drift",
			members:    []string{"a", "b"},
			capital:    map[string]float64{"a": 500, "b": 500},
			positions:  []SharedWalletPosition{{Coin: "BTC", UnrealizedPnL: 40}, {Coin: "SOL", UnrealizedPnL: 25}},
			virtualQty: map[string]map[string]float64{"BTC": {"a": 1.0}},
			balance:    1065.0,
			wantDrift:  25,
			wantSum:    1065.0 - 25, sumTol: 0.02,
			wantOrphans: []string{"SOL"},
		},
		{
			name:      "cent residual absorbed, not drifted",
			members:   []string{"a", "b", "c"},
			capital:   map[string]float64{"a": 1, "b": 1, "c": 1},
			positions: []SharedWalletPosition{},
			balance:   100.00,
			wantSum:   100.00, sumTol: 0.005,
		},
		{
			name:       "no capital splits equally",
			members:    []string{"a", "b"},
			balance:    500.0,
			wantValues: map[string]float64{"a": 250, "b": 250},
			wantSum:    500.0, sumTol: 0.01,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := reconcileSharedWalletMemberValues(tc.members, tc.capital, tc.positions, tc.virtualQty, tc.balance)
			driftTol := 1e-9
			if tc.wantDrift != 0 {
				driftTol = 0.01
			}
			if math.Abs(res.Drift-tc.wantDrift) > driftTol {
				t.Fatalf("drift = %v, want %v", res.Drift, tc.wantDrift)
			}
			if got := sumValues(res.Values); math.Abs(got-tc.wantSum) > tc.sumTol {
				t.Fatalf("sum %v != %v (tol %v)", got, tc.wantSum, tc.sumTol)
			}
			for id, want := range tc.wantValues {
				if math.Abs(res.Values[id]-want) > 0.01 {
					t.Errorf("%s = %v, want %v", id, res.Values[id], want)
				}
			}
			if tc.wantOrphans != nil {
				if len(res.OrphanCoins) != len(tc.wantOrphans) {
					t.Fatalf("OrphanCoins = %v, want %v", res.OrphanCoins, tc.wantOrphans)
				}
				for i := range tc.wantOrphans {
					if res.OrphanCoins[i] != tc.wantOrphans[i] {
						t.Fatalf("OrphanCoins = %v, want %v", res.OrphanCoins, tc.wantOrphans)
					}
				}
			}
		})
	}
}
