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
	accountBalance := 1030.0

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}
	if got := sumValues(res.Values); math.Abs(got-accountBalance) > 0.01 {
		t.Fatalf("sum %v != balance %v", got, accountBalance)
	}

	if math.Abs(res.Values["a"]-650) > 0.01 {
		t.Errorf("a = %v, want 650", res.Values["a"])
	}
	if math.Abs(res.Values["b"]-380) > 0.01 {
		t.Errorf("b = %v, want 380", res.Values["b"])
	}
}

func TestReconcileSharedWallet_SharedCoin_SplitsByVirtualQty(t *testing.T) {
	members := []string{"a", "b"}
	capital := map[string]float64{"a": 500, "b": 500}
	positions := []SharedWalletPosition{
		{Coin: "BTC", UnrealizedPnL: 90},
	}
	virtualQty := map[string]map[string]float64{
		"BTC": {"a": 2.0, "b": 1.0},
	}
	accountBalance := 1090.0

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift) > 1e-9 {
		t.Fatalf("expected ~0 drift, got %v", res.Drift)
	}

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

func TestReconcileSharedWallet_OrphanPosition_SurfacesAsDrift(t *testing.T) {
	members := []string{"a", "b"}
	capital := map[string]float64{"a": 500, "b": 500}
	positions := []SharedWalletPosition{
		{Coin: "BTC", UnrealizedPnL: 40},
		{Coin: "SOL", UnrealizedPnL: 25},
	}
	virtualQty := map[string]map[string]float64{
		"BTC": {"a": 1.0},
	}
	accountBalance := 1065.0

	res := reconcileSharedWalletMemberValues(members, capital, positions, virtualQty, accountBalance)

	if math.Abs(res.Drift-25) > 0.01 {
		t.Fatalf("expected drift ~25 (orphan SOL uPnL), got %v", res.Drift)
	}

	if got := sumValues(res.Values); math.Abs(got-(accountBalance-25)) > 0.02 {
		t.Fatalf("sum %v != balance-orphan %v", got, accountBalance-25)
	}

	if len(res.OrphanCoins) != 1 || res.OrphanCoins[0] != "SOL" {
		t.Fatalf("expected OrphanCoins [SOL], got %v", res.OrphanCoins)
	}
}

func TestReconcileSharedWallet_CentResidual_AbsorbedNotDrifted(t *testing.T) {
	members := []string{"a", "b", "c"}
	capital := map[string]float64{"a": 1, "b": 1, "c": 1}
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
