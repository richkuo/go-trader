package main

import (
	"testing"
)

func TestShouldSkipZeroCapital(t *testing.T) {
	cases := []struct {
		name       string
		capitalPct float64
		capital    float64
		want       bool
	}{
		{"capital_pct set and capital is zero", 0.5, 0, true},
		{"capital_pct set and capital is negative", 0.25, -100, true},
		{"capital_pct set and capital resolved", 0.5, 500, false},
		{"no capital_pct (fixed capital)", 0, 1000, false},
		{"no capital_pct and no capital", 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID:         "test-strategy",
				CapitalPct: tc.capitalPct,
				Capital:    tc.capital,
			}
			if got := shouldSkipZeroCapital(sc); got != tc.want {
				t.Errorf("shouldSkipZeroCapital(pct=%g, cap=%g) = %v, want %v",
					tc.capitalPct, tc.capital, got, tc.want)
			}
		})
	}
}

func TestIsLiveArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"live mode", []string{"sma", "BTC", "1h", "--mode=live"}, true},
		{"paper mode", []string{"sma", "BTC", "1h", "--mode=paper"}, false},
		{"no mode flag", []string{"sma", "BTC", "1h"}, false},
		{"empty args", []string{}, false},
		{"live at start", []string{"--mode=live", "sma"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLiveArgs(tc.args); got != tc.want {
				t.Errorf("isLiveArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestHyperliquidIsLive(t *testing.T) {
	if hyperliquidIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if hyperliquidIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestHyperliquidSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi", "ETH", "4h"}, "ETH"},
		{[]string{"sma"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := hyperliquidSymbol(tc.args)
			if got != tc.want {
				t.Errorf("hyperliquidSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestTopstepIsLive(t *testing.T) {
	if topstepIsLive([]string{"sma", "ES", "15m", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if topstepIsLive([]string{"sma", "ES", "15m"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestTopstepSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "ES", "15m"}, "ES"},
		{[]string{"rsi", "NQ", "5m"}, "NQ"},
		{[]string{"sma"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := topstepSymbol(tc.args)
			if got != tc.want {
				t.Errorf("topstepSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestRobinhoodIsLive(t *testing.T) {
	if robinhoodIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if robinhoodIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestRobinhoodSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := robinhoodSymbol(tc.args)
			if got != tc.want {
				t.Errorf("robinhoodSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestOKXIsLive(t *testing.T) {
	if okxIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if okxIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestOKXSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := okxSymbol(tc.args)
			if got != tc.want {
				t.Errorf("okxSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestOKXInstType(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"swap default", []string{"sma", "BTC", "1h"}, "swap"},
		{"explicit swap", []string{"sma", "BTC", "1h", "--inst-type=swap"}, "swap"},
		{"spot", []string{"sma", "BTC", "1h", "--inst-type=spot"}, "spot"},
		{"empty args", []string{}, "swap"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := okxInstType(tc.args)
			if got != tc.want {
				t.Errorf("okxInstType(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
