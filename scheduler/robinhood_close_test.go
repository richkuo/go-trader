package main

import (
	"context"
	"fmt"
	"testing"
)

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
