package main

import (
	"context"
	"fmt"
	"testing"
)

func TestForceCloseRobinhoodLive_ClosesOwnedCoinsOnly(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: 0.01, AvgPrice: 42000},
		{Coin: "DOGE", Size: 100, AvgPrice: 0.08},
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
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: -0.01},
		{Coin: "DOGE", Size: -100},
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

func TestParseRobinhoodCloseOutput(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		stderr  string
		exitErr error
		wantErr bool
		check   func(t *testing.T, result *RobinhoodCloseResult)
	}{
		{
			name:   "clean success",
			stdout: `{"close":{"symbol":"BTC","fill":{"avg_px":42000,"total_sz":0.01,"oid":"abc-123"}},"platform":"robinhood","timestamp":"2026-04-19T10:00:00Z"}`,
			check: func(t *testing.T, result *RobinhoodCloseResult) {
				if result == nil || result.Close == nil || result.Close.Symbol != "BTC" {
					t.Errorf("unexpected result: %+v", result)
				}
			},
		},
		{
			name:    "exit 0 with error field",
			stdout:  `{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x","error":"bad thing"}`,
			wantErr: true,
		},
		{
			name:    "non-zero exit with error envelope",
			stdout:  `{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x","error":"auth failed"}`,
			exitErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "non-zero exit with no error field",
			stdout:  `{"close":{"symbol":"BTC","fill":{}},"platform":"robinhood","timestamp":"x"}`,
			stderr:  "stderr msg",
			exitErr: fmt.Errorf("exit 2"),
			wantErr: true,
		},
		{
			name:   "already flat field parsed",
			stdout: `{"close":{"symbol":"BTC","fill":{},"already_flat":true},"platform":"robinhood","timestamp":"x"}`,
			check: func(t *testing.T, result *RobinhoodCloseResult) {
				if result == nil || result.Close == nil {
					t.Fatalf("expected populated result.Close, got %+v", result)
				}
				if !result.Close.AlreadyFlat {
					t.Errorf("AlreadyFlat = false, want true (#350)")
				}
			},
		},
		{
			name:    "malformed json",
			stdout:  `not json`,
			wantErr: true,
			check: func(t *testing.T, result *RobinhoodCloseResult) {
				if result != nil {
					t.Errorf("expected nil result on parse failure, got %+v", result)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := parseRobinhoodCloseOutput([]byte(tc.stdout), tc.stderr, tc.exitErr)
			if tc.wantErr && err == nil {
				t.Fatalf("expected non-nil err")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil err, got %v", err)
			}
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

func TestParseRobinhoodPositionsOutput(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		exitErr error
		wantErr bool
	}{
		{name: "clean success", stdout: `{"positions":[{"coin":"BTC","size":0.01,"avg_price":42000}],"platform":"robinhood","timestamp":"x"}`},
		{name: "error envelope", stdout: `{"positions":[],"platform":"robinhood","timestamp":"x","error":"not live"}`, exitErr: fmt.Errorf("exit 1"), wantErr: true},
		{name: "malformed json", stdout: `garbage`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := parseRobinhoodPositionsOutput([]byte(tc.stdout), "", tc.exitErr)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected non-nil err — a silent parse would make the kill switch misread as 'no positions' and clear virtual state while on-account remained live (#346/#345 bug class)")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected nil err, got %v", err)
			}
			if len(result.Positions) != 1 || result.Positions[0].Coin != "BTC" {
				t.Errorf("unexpected positions: %+v", result.Positions)
			}
		})
	}
}
