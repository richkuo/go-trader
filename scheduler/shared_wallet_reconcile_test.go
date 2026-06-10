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

// Distinct-coin members: each owns one coin, sum is exactly the balance and
// each member sees its own position P&L plus its capital-weighted base.
func TestReconcileSharedWallet_DistinctCoins_SumsExact(t *testing.T) {
	members := []string{"a", "b"}
	capital := map[string]float64{"a": 600, "b": 400}
	positions := []SharedWalletPosition{
		{Coin: "BTC", UnrealizedPnL: 50},
		{Coin: "ETH", UnrealizedPnL: -20},
	}
	virtualQty := map[string]map[string]float64{
		"BTC": {"a": 0.1},
		"ETH": {"b": 2.0},
	}
	accountBalance := 1030.0 // base 1000 + 50 - 20

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}
	if got := sumValues(res.Values); math.Abs(got-accountBalance) > 0.01 {
		t.Fatalf("sum %v != balance %v", got, accountBalance)
	}
	// base = 1000; a: 0.6*1000 + 50 = 650; b: 0.4*1000 - 20 = 380.
	if math.Abs(res.Values["a"]-650) > 0.01 {
		t.Errorf("a = %v, want 650", res.Values["a"])
	}
	if math.Abs(res.Values["b"]-380) > 0.01 {
		t.Errorf("b = %v, want 380", res.Values["b"])
	}
}

// A shared coin (both members hold BTC) splits that position's uPnL by virtual
// quantity share; the sum is still exactly the balance.
func TestReconcileSharedWallet_SharedCoin_SplitsByVirtualQty(t *testing.T) {
	members := []string{"a", "b"}
	capital := map[string]float64{"a": 500, "b": 500}
	positions := []SharedWalletPosition{
		{Coin: "BTC", UnrealizedPnL: 90}, // netted on-chain
	}
	virtualQty := map[string]map[string]float64{
		"BTC": {"a": 2.0, "b": 1.0}, // a owns 2/3, b owns 1/3
	}
	accountBalance := 1090.0 // base 1000 + 90

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}
	// a: 0.5*1000 + (2/3)*90 = 500 + 60 = 560; b: 500 + 30 = 530.
	if math.Abs(res.Values["a"]-560) > 0.01 {
		t.Errorf("a = %v, want 560", res.Values["a"])
	}
	if math.Abs(res.Values["b"]-530) > 0.01 {
		t.Errorf("b = %v, want 530", res.Values["b"])
	}
	if got := sumValues(res.Values); math.Abs(got-accountBalance) > 0.01 {
		t.Fatalf("sum %v != balance %v", got, accountBalance)
	}
}

// An on-chain position no member owns (orphan) must surface as drift, NOT be
// hidden by inflating a member row to force the sum to the balance.
func TestReconcileSharedWallet_OrphanPosition_SurfacesAsDrift(t *testing.T) {
	members := []string{"a", "b"}
	capital := map[string]float64{"a": 500, "b": 500}
	positions := []SharedWalletPosition{
		{Coin: "BTC", UnrealizedPnL: 40}, // owned by a
		{Coin: "SOL", UnrealizedPnL: 25}, // orphan: no member holds SOL
	}
	virtualQty := map[string]map[string]float64{
		"BTC": {"a": 1.0},
	}
	accountBalance := 1065.0 // base 1000 + 40 + 25

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift-25) > 0.01 {
		t.Fatalf("expected drift ~25 (orphan SOL uPnL), got %v", res.Drift)
	}
	// Sum should be balance MINUS the orphan, so the shortfall is visible.
	if got := sumValues(res.Values); math.Abs(got-(accountBalance-25)) > 0.02 {
		t.Fatalf("sum %v != balance-orphan %v", got, accountBalance-25)
	}
	// The unattributed coin is identified for the alert + streak signature.
	if len(res.OrphanCoins) != 1 || res.OrphanCoins[0] != "SOL" {
		t.Fatalf("expected OrphanCoins [SOL], got %v", res.OrphanCoins)
	}
}

// Cent rounding: values still sum to the rounded balance to the cent.
func TestReconcileSharedWallet_CentResidual_AbsorbedNotDrifted(t *testing.T) {
	members := []string{"a", "b", "c"}
	capital := map[string]float64{"a": 1, "b": 1, "c": 1} // 1/3 each → repeating
	positions := []SharedWalletPosition{}
	accountBalance := 100.00

	res := reconcileSharedWalletMemberValues(members, capital, positions, nil, accountBalance)

	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}
	if got := roundCents(sumValues(res.Values)); math.Abs(got-100.00) > 1e-9 {
		t.Fatalf("rounded sum %v != 100.00 (cent residual not absorbed)", got)
	}
}

// Zero/absent capital everywhere falls back to an equal split so no value
// leaks into drift.
func TestReconcileSharedWallet_NoCapital_EqualSplit(t *testing.T) {
	members := []string{"a", "b"}
	res := reconcileSharedWalletMemberValues(members, nil, nil, nil, 500.0)
	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}
	if math.Abs(res.Values["a"]-250) > 0.01 || math.Abs(res.Values["b"]-250) > 0.01 {
		t.Errorf("expected 250/250 equal split, got a=%v b=%v", res.Values["a"], res.Values["b"])
	}
}
