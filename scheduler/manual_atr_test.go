package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHyperliquidFetchATROutput(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		result, _, err := parseHyperliquidFetchATROutput([]byte(`{"atr": 12.34, "candles": 200}`), "", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if result == nil {
			t.Fatal("nil result")
		}
		if result.ATR != 12.34 {
			t.Errorf("ATR=%v want 12.34", result.ATR)
		}
		if result.Candles != 200 {
			t.Errorf("Candles=%d want 200", result.Candles)
		}
		if result.Error != "" {
			t.Errorf("Error=%q want empty", result.Error)
		}
	})

	t.Run("structured error", func(t *testing.T) {
		result, _, err := parseHyperliquidFetchATROutput([]byte(`{"error": "insufficient candles: got 5, need 15", "candles": 5}`), "", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if result == nil || result.Error == "" {
			t.Fatal("expected structured error in result")
		}
	})

	t.Run("run error carries stderr and cause", func(t *testing.T) {
		_, _, err := parseHyperliquidFetchATROutput(nil, "missing python", errors.New("exit 127"))
		if err == nil {
			t.Fatal("expected error on runErr")
		}
		if !strings.Contains(err.Error(), "missing python") {
			t.Errorf("error should include stderr; got %q", err.Error())
		}
		if !strings.Contains(err.Error(), "exit 127") {
			t.Errorf("error should include underlying runErr; got %q", err.Error())
		}
	})

	t.Run("bad json", func(t *testing.T) {
		if _, _, err := parseHyperliquidFetchATROutput([]byte("not json"), "", nil); err == nil {
			t.Fatal("expected parse error")
		}
	})
}

func TestFetchManualEntryATR_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
	}{
		{"no script", StrategyConfig{Symbol: "ETH", Timeframe: "1h"}},
		{"no symbol", StrategyConfig{Script: "x.py", Timeframe: "1h"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atr, msg, ok := fetchManualEntryATR(c.sc, nil)
			if ok {
				t.Errorf("ok=true want false")
			}
			if atr != 0 {
				t.Errorf("atr=%v want 0", atr)
			}
			if msg == "" {
				t.Errorf("expected non-empty error message")
			}
		})
	}
}

func TestFetchManualEntryATR_SubprocessOutcomes(t *testing.T) {
	cases := []struct {
		name          string
		sc            StrategyConfig
		result        *HyperliquidFetchATRResult
		stderr        string
		runErr        error
		wantOK        bool
		wantATR       float64
		wantMsg       string
		wantTimeframe string
	}{
		{
			name:          "success passes args through",
			sc:            StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"},
			result:        &HyperliquidFetchATRResult{ATR: 25.5, Candles: 200},
			wantOK:        true,
			wantATR:       25.5,
			wantTimeframe: "1h",
		},
		{
			name:          "empty timeframe defaults to 1h",
			sc:            StrategyConfig{Script: "check.py", Symbol: "ETH"},
			result:        &HyperliquidFetchATRResult{ATR: 18.0, Candles: 200},
			wantOK:        true,
			wantATR:       18.0,
			wantTimeframe: "1h",
		},
		{
			name:    "script error fails closed",
			sc:      StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"},
			result:  &HyperliquidFetchATRResult{Error: "insufficient candles"},
			wantMsg: "insufficient candles",
		},
		{
			name:   "non-positive ATR fails closed",
			sc:     StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"},
			result: &HyperliquidFetchATRResult{ATR: 0, Candles: 200},
		},
		{
			name:   "run error fails closed",
			sc:     StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"},
			stderr: "boom",
			runErr: errors.New("subprocess died"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := runHyperliquidFetchATRFn
			defer func() { runHyperliquidFetchATRFn = prev }()
			var gotScript, gotSymbol, gotTimeframe string
			var gotPeriod int
			runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int, atrMethod string) (*HyperliquidFetchATRResult, string, error) {
				gotScript, gotSymbol, gotTimeframe, gotPeriod = script, symbol, timeframe, period
				return tc.result, tc.stderr, tc.runErr
			}
			atr, msg, ok := fetchManualEntryATR(tc.sc, nil)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (msg=%s)", ok, tc.wantOK, msg)
			}
			if tc.wantOK {
				if atr != tc.wantATR {
					t.Errorf("atr=%v want %v", atr, tc.wantATR)
				}
				if gotScript != tc.sc.Script || gotSymbol != tc.sc.Symbol || gotPeriod != 14 {
					t.Errorf("unexpected args: script=%s symbol=%s period=%d", gotScript, gotSymbol, gotPeriod)
				}
				if gotTimeframe != tc.wantTimeframe {
					t.Errorf("timeframe=%q want %q", gotTimeframe, tc.wantTimeframe)
				}
				return
			}
			if tc.wantMsg != "" && msg != tc.wantMsg {
				t.Errorf("msg=%q want %q", msg, tc.wantMsg)
			}
			if msg == "" {
				t.Error("expected non-empty error message")
			}
		})
	}
}

func TestResolveManualATRTimeframe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unset defaults to 1h", "", "1h"},
		{"explicit 1h", "1h", "1h"},
		{"explicit non-1h preserved", "4h", "4h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveManualATRTimeframe(StrategyConfig{Timeframe: c.in})
			if got != c.want {
				t.Errorf("resolveManualATRTimeframe(%q)=%q want %q", c.in, got, c.want)
			}
		})
	}
}
