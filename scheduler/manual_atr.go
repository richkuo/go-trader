package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type HyperliquidFetchATRResult struct {
	ATR     float64 `json:"atr,omitempty"`
	Candles int     `json:"candles,omitempty"`
	Error   string  `json:"error,omitempty"`
}

func RunHyperliquidFetchATR(script, symbol, timeframe string, period int, atrMethod string) (*HyperliquidFetchATRResult, string, error) {
	if period <= 0 {
		period = 14
	}
	if atrMethod == "" {
		atrMethod = ATRMethodSimple
	}
	args := []string{
		"--fetch-atr",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--timeframe=%s", timeframe),
		fmt.Sprintf("--period=%d", period),
		"--atr-method=" + atrMethod,
	}
	stdout, stderr, err := RunPythonScript(script, args)
	return parseHyperliquidFetchATROutput(stdout, string(stderr), err)
}

func parseHyperliquidFetchATROutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidFetchATRResult, string, error) {
	if runErr != nil {

		return nil, stderrStr, fmt.Errorf("fetch-atr error: %w (stderr: %s)", runErr, stderrStr)
	}
	var result HyperliquidFetchATRResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse fetch-atr output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

var runHyperliquidFetchATRFn = RunHyperliquidFetchATR

func resolveManualATRTimeframe(sc StrategyConfig) string {
	if sc.Timeframe == "" {
		return "1h"
	}
	return sc.Timeframe
}

func fetchManualEntryATR(sc StrategyConfig, cfg *Config) (float64, string, bool) {
	if sc.Script == "" || sc.Symbol == "" {
		return 0, "missing script/symbol on strategy config", false
	}

	timeframe := resolveManualATRTimeframe(sc)
	if sc.Timeframe == "" {
		fmt.Fprintf(os.Stderr, "[manual-open] defaulting to 1h ATR (strategy timeframe unset)\n")
	}
	result, stderr, err := runHyperliquidFetchATRFn(sc.Script, sc.Symbol, timeframe, 14, resolveATRMethod(sc, cfg))
	if err != nil {
		msg := err.Error()
		if stderr != "" {
			msg = fmt.Sprintf("%s; stderr=%s", msg, stderr)
		}
		return 0, msg, false
	}
	if result == nil {
		return 0, "nil fetch-atr result", false
	}
	if result.Error != "" {
		return 0, result.Error, false
	}
	if result.ATR <= 0 {
		return 0, "fetch-atr returned non-positive ATR", false
	}
	return result.ATR, "", true
}
