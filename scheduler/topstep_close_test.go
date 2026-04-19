package main

import (
	"context"
	"fmt"
	"testing"
)

// forceCloseTopStepLive unit tests — mirror the Robinhood/OKX tests. Each
// test asserts a single branch of the decision logic so a regression
// produces a targeted failure rather than a conflated signal (#347).

func TestForceCloseTopStepLive_ClosesOwnedSymbolsOnly(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{
		{Coin: "ES", Size: 2, AvgPrice: 5000, Side: "long"},
		{Coin: "NQ", Size: 1, AvgPrice: 17000, Side: "long"}, // unowned
	}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(calls) != 1 || calls[0] != "ES" {
		t.Errorf("expected closer to be called only for owned symbol ES, got %v", calls)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ES" {
		t.Errorf("ClosedCoins = %v, want [ES]", report.ClosedCoins)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "NQ" {
		t.Errorf("Unconfigured = %v, want [NQ]", report.Unconfigured)
	}
}

func TestForceCloseTopStepLive_CloseErrorLatches(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	closer := func(sym string) (*TopStepCloseResult, error) {
		return nil, fmt.Errorf("topstepx 503")
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if report.ConfirmedFlat() {
		t.Fatal("expected NOT ConfirmedFlat on close error")
	}
	if _, ok := report.Errors["ES"]; !ok {
		t.Errorf("expected ES in errors, got %v", report.Errors)
	}
}

func TestForceCloseTopStepLive_ZeroSizeMarkedAlreadyFlat(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 0}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if len(calls) != 0 {
		t.Errorf("zero-size position must short-circuit before closer, got calls=%v", calls)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "ES" {
		t.Errorf("AlreadyFlat = %v, want [ES]", report.AlreadyFlat)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("zero-size short-circuit must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseTopStepLive_CtxExpiredBeforeSubmit(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(ctx, positions, tsLive, closer)

	if len(calls) != 0 {
		t.Errorf("closer must not be called after ctx expires, got %v", calls)
	}
	if _, ok := report.Errors["ES"]; !ok {
		t.Errorf("expected ES to be marked as error on ctx expiry, got %v", report.Errors)
	}
}

func TestForceCloseTopStepLive_ShortPositionClosed(t *testing.T) {
	// Futures support bidirectional positions (unlike Robinhood crypto).
	// A negative size (short) for an owned symbol must trigger a close —
	// market_close flattens regardless of direction. Guards against a
	// future refactor that copies Robinhood's Size > 0 check, which would
	// leave shorts open on kill-switch fire.
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: -2, Side: "short"}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if len(calls) != 1 || calls[0] != "ES" {
		t.Errorf("short must trigger close, got calls=%v", calls)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("successful short close must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseTopStepLive_NonFuturesStrategiesIgnored(t *testing.T) {
	// Non-futures entries in TSLiveAll must not drive closes (type partition
	// guard). Mirrors the options-ignored test on the Robinhood close path.
	mixed := []StrategyConfig{
		{ID: "hl-mom-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"momentum", "BTC", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, mixed, closer)

	if len(calls) != 0 {
		t.Errorf("non-futures strategy must not drive TopStep close, got calls=%v", calls)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "ES" {
		t.Errorf("unowned ES must be Unconfigured, got %+v", report.Unconfigured)
	}
}

// SortedErrorCoins determinism — same rationale as HL / OKX / Robinhood.
func TestTopStepLiveCloseReport_SortedErrorCoins(t *testing.T) {
	r := TopStepLiveCloseReport{Errors: map[string]error{
		"NQ": fmt.Errorf("e"), "ES": fmt.Errorf("e"), "CL": fmt.Errorf("e"),
	}}
	coins := r.SortedErrorCoins()
	want := []string{"CL", "ES", "NQ"}
	if len(coins) != len(want) {
		t.Fatalf("len = %d, want %d", len(coins), len(want))
	}
	for i, c := range coins {
		if c != want[i] {
			t.Errorf("coins[%d] = %q, want %q", i, c, want[i])
		}
	}
}

// parseTopStepCloseOutput tests — 5-case matrix mirroring HL / OKX / Robinhood.

func TestParseTopStepCloseOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{"avg_px":5000,"total_contracts":2,"oid":"ord-1"}},"platform":"topstep","timestamp":"2026-04-19T10:00:00Z"}`)
	result, _, err := parseTopStepCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil || result.Close.Symbol != "ES" {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.Close.Fill == nil || result.Close.Fill.TotalContracts != 2 {
		t.Errorf("Fill = %+v, want TotalContracts=2", result.Close.Fill)
	}
}

func TestParseTopStepCloseOutput_Exit0WithErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"venue down"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err when error field populated, even on exit 0")
	}
}

func TestParseTopStepCloseOutput_ExitNonZeroWithErrorEnvelope(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"market closed"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err on error envelope")
	}
}

func TestParseTopStepCloseOutput_ExitNonZeroNoErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "stderr msg", fmt.Errorf("exit 2"))
	if err == nil {
		t.Fatal("expected non-nil err on non-zero exit with no error field")
	}
}

func TestParseTopStepCloseOutput_MalformedJSON(t *testing.T) {
	result, _, err := parseTopStepCloseOutput([]byte(`not json`), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
	if result != nil {
		t.Errorf("expected nil result on parse failure, got %+v", result)
	}
}

// parseTopStepPositionsOutput tests.

func TestParseTopStepPositionsOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"ES","size":2,"avg_price":5000,"side":"long"}],"platform":"topstep","timestamp":"x"}`)
	result, _, err := parseTopStepPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(result.Positions) != 1 || result.Positions[0].Coin != "ES" {
		t.Errorf("unexpected positions: %+v", result.Positions)
	}
	if result.Positions[0].Size != 2 {
		t.Errorf("Size = %d, want 2", result.Positions[0].Size)
	}
}

func TestParseTopStepPositionsOutput_ErrorEnvelopeLatchesSwitch(t *testing.T) {
	// Load-bearing contract: a failed fetch must surface as err so the kill
	// switch latches. A silent parse that returned {Positions: nil, err: nil}
	// would look like "no positions" and clear virtual state while live
	// exposure remained (the #341/#345/#346 bug class, now #347).
	stdout := []byte(`{"positions":[],"platform":"topstep","timestamp":"x","error":"credentials missing"}`)
	_, _, err := parseTopStepPositionsOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err when error envelope populated")
	}
}

func TestParseTopStepPositionsOutput_MalformedJSON(t *testing.T) {
	_, _, err := parseTopStepPositionsOutput([]byte(`garbage`), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
}
