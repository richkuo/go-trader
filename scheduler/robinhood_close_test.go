package main

import (
	"context"
	"fmt"
	"testing"
)

// forceCloseRobinhoodLive unit tests — mirror the OKX tests in
// okx_close_test.go. Each test asserts a single branch of the decision
// logic so a regression produces a targeted failure rather than a
// conflated ambiguous signal (#346).

func TestForceCloseRobinhoodLive_ClosesOwnedCoinsOnly(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: 0.01, AvgPrice: 42000},
		{Coin: "DOGE", Size: 100, AvgPrice: 0.08}, // unowned
	}
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: sym}}, nil
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, rhLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("expected closer to be called only for owned coin BTC, got %v", calls)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "BTC" {
		t.Errorf("ClosedCoins = %v, want [BTC]", report.ClosedCoins)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "DOGE" {
		t.Errorf("Unconfigured = %v, want [DOGE]", report.Unconfigured)
	}
}

func TestForceCloseRobinhoodLive_CloseErrorLatches(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01}}
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		return nil, fmt.Errorf("robin_stocks 503")
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, rhLive, closer)

	if report.ConfirmedFlat() {
		t.Fatal("expected NOT ConfirmedFlat on close error")
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC in errors, got %v", report.Errors)
	}
}

func TestForceCloseRobinhoodLive_ZeroSizeMarkedAlreadyFlat(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0}}
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{}, nil
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, rhLive, closer)

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

func TestForceCloseRobinhoodLive_CtxExpiredBeforeSubmit(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{}, nil
	}

	report := forceCloseRobinhoodLive(ctx, positions, rhLive, closer)

	if len(calls) != 0 {
		t.Errorf("closer must not be called after ctx expires, got %v", calls)
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC to be marked as error on ctx expiry, got %v", report.Errors)
	}
}

func TestForceCloseRobinhoodLive_NegativeSizeNotTraded(t *testing.T) {
	// Robinhood crypto is spot-only — negative sizes shouldn't appear in
	// practice. If a future change ever populates a negative balance (e.g.
	// lent / staked), the close gate must NOT fire a market sell for |size|.
	// Forward-compat guard against the Size > 0 vs Size != 0 ambiguity
	// flagged in the #346 review.
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: -0.01}, // owned coin, hypothetical negative
		{Coin: "DOGE", Size: -100}, // unowned coin, hypothetical negative
	}
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{}, nil
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, rhLive, closer)

	if len(calls) != 0 {
		t.Errorf("negative-size position must NOT trigger close, got calls=%v", calls)
	}
	if len(report.Unconfigured) != 0 {
		t.Errorf("negative-size unowned position must NOT be Unconfigured, got %+v", report.Unconfigured)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "BTC" {
		t.Errorf("negative-size owned position should be treated as already-flat, got %v", report.AlreadyFlat)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("negative-size positions must not block ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseRobinhoodLive_OptionsStrategiesIgnored(t *testing.T) {
	// Options strategies live in RHLiveOptions and must NOT appear in the
	// crypto close loop — forceCloseRobinhoodLive should not attempt to
	// "close" options as crypto. Guards against a future refactor that
	// flattens the crypto/options partition.
	mixed := []StrategyConfig{
		{ID: "rh-ccall-spy", Platform: "robinhood", Type: "options",
			Args: []string{"covered_call", "SPY", "1d", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01}}
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{}, nil
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, mixed, closer)

	if len(calls) != 0 {
		t.Errorf("options strategy must not drive crypto close, got calls=%v", calls)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("options-only config with non-traded crypto is ConfirmedFlat for the crypto branch, got errors=%v", report.Errors)
	}
}

// Adapter-side AlreadyFlat: closer returns success with already_flat=true
// (eventual-consistency window — Go-side fetch saw qty>0, but by the time
// the adapter ran get_crypto_positions it returned qty<=0). The coin must
// land in AlreadyFlat, NOT ClosedCoins, so operator messaging
// distinguishes "we sent a close order" from "nothing to close" (#350).
func TestForceCloseRobinhoodLive_AdapterAlreadyFlatRoutedCorrectly(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01, AvgPrice: 42000}}
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{
			Close:    &RobinhoodClose{Symbol: sym, AlreadyFlat: true},
			Platform: "robinhood",
		}, nil
	}

	report := forceCloseRobinhoodLive(context.Background(), positions, rhLive, closer)

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
		t.Errorf("closer should be called once (Go side saw qty>0), got %v", calls)
	}
}

// SortedErrorCoins determinism — same rationale as HL / OKX: Go map
// iteration is randomized and Discord output must be byte-stable across
// calls for operator triage.
func TestRobinhoodLiveCloseReport_SortedErrorCoins(t *testing.T) {
	r := RobinhoodLiveCloseReport{Errors: map[string]error{
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

// parseRobinhoodCloseOutput tests — mirror the 5-case matrix established
// by parseHyperliquidCloseOutput / parseOKXCloseOutput. These test the
// load-bearing kill-switch contract that any ambiguous subprocess
// response must surface as a non-nil error so the switch stays latched.

func TestParseRobinhoodCloseOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{"avg_px":42000,"total_sz":0.01,"oid":"abc-123"}},"platform":"robinhood","timestamp":"2026-04-19T10:00:00Z"}`)
	result, _, err := parseRobinhoodCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err on clean success, got %v", err)
	}
	if result == nil || result.Close == nil || result.Close.Symbol != "BTC" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestParseRobinhoodCloseOutput_Exit0WithErrorField(t *testing.T) {
	// Contract drift guard: Python shouldn't exit 0 with error populated,
	// but if it happens the envelope is authoritative.
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x","error":"bad thing"}`)
	_, _, err := parseRobinhoodCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err when error field populated, even on exit 0")
	}
}

func TestParseRobinhoodCloseOutput_ExitNonZeroWithErrorEnvelope(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x","error":"auth failed"}`)
	_, _, err := parseRobinhoodCloseOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err on error envelope")
	}
}

func TestParseRobinhoodCloseOutput_ExitNonZeroNoErrorField(t *testing.T) {
	// Unexpected: must surface as failure so kill switch latches rather than
	// silently report success on non-zero exit.
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x"}`)
	_, _, err := parseRobinhoodCloseOutput(stdout, "stderr msg", fmt.Errorf("exit 2"))
	if err == nil {
		t.Fatal("expected non-nil err on non-zero exit with no error field")
	}
}

func TestParseRobinhoodCloseOutput_AlreadyFlatFieldParsed(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{},"already_flat":true},"platform":"robinhood","timestamp":"x"}`)
	result, _, err := parseRobinhoodCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil {
		t.Fatalf("expected populated result.Close, got %+v", result)
	}
	if !result.Close.AlreadyFlat {
		t.Errorf("AlreadyFlat = false, want true (#350)")
	}
}

func TestParseRobinhoodCloseOutput_MalformedJSON(t *testing.T) {
	stdout := []byte(`not json`)
	result, _, err := parseRobinhoodCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
	if result != nil {
		t.Errorf("expected nil result on parse failure, got %+v", result)
	}
}

// parseRobinhoodPositionsOutput tests — mirror parseOKXPositionsOutput.

func TestParseRobinhoodPositionsOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"BTC","size":0.01,"avg_price":42000}],"platform":"robinhood","timestamp":"x"}`)
	result, _, err := parseRobinhoodPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(result.Positions) != 1 || result.Positions[0].Coin != "BTC" {
		t.Errorf("unexpected positions: %+v", result.Positions)
	}
}

func TestParseRobinhoodPositionsOutput_ErrorEnvelope(t *testing.T) {
	stdout := []byte(`{"positions":[],"platform":"robinhood","timestamp":"x","error":"not live"}`)
	_, _, err := parseRobinhoodPositionsOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err when error envelope populated — silent parse would make kill switch misread as 'no positions' and clear virtual state while on-chain remained live (#346/#345 bug class)")
	}
}

func TestParseRobinhoodPositionsOutput_MalformedJSON(t *testing.T) {
	_, _, err := parseRobinhoodPositionsOutput([]byte(`garbage`), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
}
