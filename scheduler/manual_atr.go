package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// HyperliquidFetchATRResult is the JSON output from check_hyperliquid.py
// --fetch-atr (#689). Used by manual-open to derive entry ATR from the same
// OHLCV/period the strategy script would, without requiring --atr.
type HyperliquidFetchATRResult struct {
	ATR     float64 `json:"atr,omitempty"`
	Candles int     `json:"candles,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// RunHyperliquidFetchATR invokes check_hyperliquid.py --fetch-atr to compute
// latest ATR from HL OHLCV. Read-only (no on-chain side effects).
func RunHyperliquidFetchATR(script, symbol, timeframe string, period int) (*HyperliquidFetchATRResult, string, error) {
	if period <= 0 {
		period = 14
	}
	args := []string{
		"--fetch-atr",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--timeframe=%s", timeframe),
		fmt.Sprintf("--period=%d", period),
	}
	stdout, stderr, err := RunPythonScript(script, args)
	return parseHyperliquidFetchATROutput(stdout, string(stderr), err)
}

func parseHyperliquidFetchATROutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidFetchATRResult, string, error) {
	if runErr != nil {
		// fetch-atr emits structured JSON even on its own internal failures (it
		// catches and reports), so a process-level error is real (e.g. Python
		// missing). Surface it without trying to parse stdout.
		return nil, stderrStr, fmt.Errorf("fetch-atr error: %w (stderr: %s)", runErr, stderrStr)
	}
	var result HyperliquidFetchATRResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse fetch-atr output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// runHyperliquidFetchATRFn is a package var so manual.go callers and tests can
// stub the subprocess without spawning Python.
var runHyperliquidFetchATRFn = RunHyperliquidFetchATR

// fetchManualEntryATR resolves the ATR for a manual-open by calling the
// HL --fetch-atr path with the strategy's configured symbol+timeframe. Returns
// (atr, errMsg, ok). ok=false signals callers should fall back (typically to
// computeFallbackATR). Period is fixed at 14 to match ensure_atr_indicator's
// default — same baseline strategy opens see via stampEntryATRIfOpened.
// Strategies that override ATR period via params will see drift between fetched
// and stamped ATR; if that becomes a problem, plumb a per-strategy ATR period
// from sc.OpenStrategy.Params here.
func fetchManualEntryATR(sc StrategyConfig) (float64, string, bool) {
	if sc.Script == "" || sc.Symbol == "" {
		return 0, "missing script/symbol on strategy config", false
	}
	// 1h is the canonical default across the manual flow (the init wizard and
	// generateConfig both default to "1h"), so an unset timeframe falls back to
	// it rather than failing closed to the coarse heuristic ATR. Only a genuine
	// fetch failure should drop callers to computeFallbackATR.
	timeframe := sc.Timeframe
	if timeframe == "" {
		timeframe = "1h"
		fmt.Fprintf(os.Stderr, "[manual-open] defaulting to 1h ATR (strategy timeframe unset)\n")
	}
	result, stderr, err := runHyperliquidFetchATRFn(sc.Script, sc.Symbol, timeframe, 14)
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
