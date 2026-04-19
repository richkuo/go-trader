package main

import (
	"context"
	"fmt"
	"testing"
)

// forceCloseOKXLive unit tests — mirror the HL tests in
// hyperliquid_balance_test.go (TestForceCloseHyperliquidLive_*). Each
// test asserts a single branch of the decision logic so a regression
// produces a targeted failure rather than a conflated ambiguous signal.

func TestForceCloseOKXLive_ClosesOwnedCoinsOnly(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "SOL", Size: 50, Side: "long"}, // unowned: no configured strategy
	}
	var calls []string
	closer := func(sym string) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{Close: &OKXClose{Symbol: sym}}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("expected closer to be called only for owned coin BTC, got %v", calls)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "BTC" {
		t.Errorf("ClosedCoins = %v, want [BTC]", report.ClosedCoins)
	}
	// Unowned positions must be surfaced via Unconfigured so the kill-switch
	// plan can latch for manual intervention (dedup: single source of truth
	// for the traded-coins partition lives in forceCloseOKXLive, not the
	// plan builder).
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "SOL" {
		t.Errorf("Unconfigured = %v, want [SOL]", report.Unconfigured)
	}
}

func TestForceCloseOKXLive_CloseErrorLatches(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	closer := func(sym string) (*OKXCloseResult, error) {
		return nil, fmt.Errorf("okx 503")
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if report.ConfirmedFlat() {
		t.Fatal("expected NOT ConfirmedFlat on close error")
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC in errors, got %v", report.Errors)
	}
}

func TestForceCloseOKXLive_ZeroSizeMarkedAlreadyFlat(t *testing.T) {
	// Defense-in-depth: fetcher filters size==0 upstream, but if it ever
	// loosens we must not submit a zero-size order (OKX would reject it
	// and the kill switch would latch on a false failure).
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0, Side: ""}}
	var calls []string
	closer := func(sym string) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if len(calls) != 0 {
		t.Errorf("zero-size position must short-circuit before closer, got calls=%v", calls)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "BTC" {
		t.Errorf("AlreadyFlat = %v, want [BTC]", report.AlreadyFlat)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("zero-size short-circuit must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseOKXLive_CtxExpiredBeforeSubmit(t *testing.T) {
	// When the overall budget expires, remaining coins must be flagged as
	// errors so the kill switch latches and retries next cycle — rather
	// than silently skipping them and clearing virtual state.
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	var calls []string
	closer := func(sym string) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(ctx, positions, okxLive, closer)

	if len(calls) != 0 {
		t.Errorf("closer must not be called after ctx expires, got %v", calls)
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC to be marked as error on ctx expiry, got %v", report.Errors)
	}
}

func TestForceCloseOKXLive_SpotStrategiesIgnored(t *testing.T) {
	// Spot strategies live in OKXLiveAllSpot and must NOT appear in the
	// perps close loop — forceCloseOKXLive should not attempt to "close"
	// spot coins. Guards against a future refactor that flattens the
	// perps/spot partition and triggers unsafe sell-all behavior.
	mixed := []StrategyConfig{
		{ID: "okx-btc-spot", Platform: "okx", Type: "spot",
			Args: []string{"sma", "BTC", "1h", "--mode=live", "--inst-type=spot"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	var calls []string
	closer := func(sym string) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, mixed, closer)

	if len(calls) != 0 {
		t.Errorf("spot strategy must not drive perps close, got calls=%v", calls)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("spot-only config with non-traded perps position is ConfirmedFlat for the OKX perps branch, got errors=%v", report.Errors)
	}
}

// Adapter-side AlreadyFlat: closer returns success with already_flat=true
// (eventual-consistency window between Go-side fetch and adapter submit).
// The coin must land in AlreadyFlat, NOT ClosedCoins, so operator messaging
// distinguishes "we sent a close order" from "nothing to close" (#350).
func TestForceCloseOKXLive_AdapterAlreadyFlatRoutedCorrectly(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	var calls []string
	closer := func(sym string) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{
			Close:    &OKXClose{Symbol: sym, AlreadyFlat: true},
			Platform: "okx",
		}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(report.ClosedCoins) != 0 {
		t.Errorf("ClosedCoins should be empty when adapter reports already_flat, got %v", report.ClosedCoins)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "BTC" {
		t.Errorf("AlreadyFlat = %v, want [BTC]", report.AlreadyFlat)
	}
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("closer should be called once (Go side saw non-zero size), got %v", calls)
	}
}

// SortedErrorCoins determinism — same rationale as HL
// (HyperliquidLiveCloseReport.SortedErrorCoins): Go map iteration is
// randomized and the Discord output must be byte-stable across calls for
// operator triage.
func TestOKXLiveCloseReport_SortedErrorCoins(t *testing.T) {
	r := OKXLiveCloseReport{Errors: map[string]error{
		"SOL": fmt.Errorf("e"), "BTC": fmt.Errorf("e"), "ETH": fmt.Errorf("e"),
	}}
	coins := r.SortedErrorCoins()
	want := []string{"BTC", "ETH", "SOL"}
	if len(coins) != len(want) {
		t.Fatalf("len = %d, want %d", len(coins), len(want))
	}
	for i, c := range coins {
		if c != want[i] {
			t.Errorf("coins[%d] = %q, want %q", i, c, want[i])
		}
	}
}
