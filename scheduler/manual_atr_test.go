package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHyperliquidFetchATROutput_Success(t *testing.T) {
	stdout := []byte(`{"atr": 12.34, "candles": 200}`)
	result, _, err := parseHyperliquidFetchATROutput(stdout, "", nil)
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
}

func TestParseHyperliquidFetchATROutput_StructuredError(t *testing.T) {
	stdout := []byte(`{"error": "insufficient candles: got 5, need 15", "candles": 5}`)
	result, _, err := parseHyperliquidFetchATROutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result == nil || result.Error == "" {
		t.Fatal("expected structured error in result")
	}
}

func TestParseHyperliquidFetchATROutput_RunError(t *testing.T) {
	_, _, err := parseHyperliquidFetchATROutput(nil, "missing python", errors.New("exit 127"))
	if err == nil {
		t.Fatal("expected error on runErr")
	}
	// fetchManualEntryATR weaves stderr into the message; the parser must
	// surface stderr in its returned error so that contract is satisfiable.
	if !strings.Contains(err.Error(), "missing python") {
		t.Errorf("error should include stderr; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "exit 127") {
		t.Errorf("error should include underlying runErr; got %q", err.Error())
	}
}

func TestParseHyperliquidFetchATROutput_BadJSON(t *testing.T) {
	_, _, err := parseHyperliquidFetchATROutput([]byte("not json"), "", nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestFetchManualEntryATR_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
	}{
		{"no script", StrategyConfig{Symbol: "ETH", Timeframe: "1h"}},
		{"no symbol", StrategyConfig{Script: "x.py", Timeframe: "1h"}},
		{"no timeframe", StrategyConfig{Script: "x.py", Symbol: "ETH"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atr, msg, ok := fetchManualEntryATR(c.sc)
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

func TestFetchManualEntryATR_StubSuccess(t *testing.T) {
	prev := runHyperliquidFetchATRFn
	defer func() { runHyperliquidFetchATRFn = prev }()
	runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int) (*HyperliquidFetchATRResult, string, error) {
		if script != "check.py" || symbol != "ETH" || timeframe != "1h" || period != 14 {
			t.Errorf("unexpected args: script=%s symbol=%s tf=%s period=%d", script, symbol, timeframe, period)
		}
		return &HyperliquidFetchATRResult{ATR: 25.5, Candles: 200}, "", nil
	}
	sc := StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"}
	atr, msg, ok := fetchManualEntryATR(sc)
	if !ok {
		t.Fatalf("ok=false msg=%s", msg)
	}
	if atr != 25.5 {
		t.Errorf("atr=%v want 25.5", atr)
	}
}

func TestFetchManualEntryATR_StubScriptError(t *testing.T) {
	prev := runHyperliquidFetchATRFn
	defer func() { runHyperliquidFetchATRFn = prev }()
	runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int) (*HyperliquidFetchATRResult, string, error) {
		return &HyperliquidFetchATRResult{Error: "insufficient candles"}, "", nil
	}
	sc := StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"}
	_, msg, ok := fetchManualEntryATR(sc)
	if ok {
		t.Fatal("ok=true on script error")
	}
	if msg != "insufficient candles" {
		t.Errorf("msg=%q want insufficient candles", msg)
	}
}

func TestFetchManualEntryATR_StubNonPositiveATR(t *testing.T) {
	prev := runHyperliquidFetchATRFn
	defer func() { runHyperliquidFetchATRFn = prev }()
	runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int) (*HyperliquidFetchATRResult, string, error) {
		return &HyperliquidFetchATRResult{ATR: 0, Candles: 200}, "", nil
	}
	sc := StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"}
	_, _, ok := fetchManualEntryATR(sc)
	if ok {
		t.Fatal("ok=true on zero ATR")
	}
}

func TestFetchManualEntryATR_StubRunError(t *testing.T) {
	prev := runHyperliquidFetchATRFn
	defer func() { runHyperliquidFetchATRFn = prev }()
	runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int) (*HyperliquidFetchATRResult, string, error) {
		return nil, "boom", errors.New("subprocess died")
	}
	sc := StrategyConfig{Script: "check.py", Symbol: "ETH", Timeframe: "1h"}
	_, msg, ok := fetchManualEntryATR(sc)
	if ok {
		t.Fatal("ok=true on run error")
	}
	if msg == "" {
		t.Error("expected non-empty error message")
	}
}
