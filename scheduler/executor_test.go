package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These tests verify JSON deserialization of executor result structs, not subprocess
// execution behavior (timeouts, concurrency limits, etc.).

func TestSpotResultJSON(t *testing.T) {
	raw := `{
		"strategy": "sma_crossover",
		"symbol": "BTC/USDT",
		"timeframe": "1h",
		"signal": 1,
		"price": 60000.5,
		"indicators": {"sma_fast": 59000, "sma_slow": 58000},
		"timestamp": "2026-01-01T00:00:00Z"
	}`

	var result SpotResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if result.Strategy != "sma_crossover" {
		t.Errorf("Strategy = %q, want %q", result.Strategy, "sma_crossover")
	}
	if result.Signal != 1 {
		t.Errorf("Signal = %d, want 1", result.Signal)
	}
	if result.Price != 60000.5 {
		t.Errorf("Price = %g, want 60000.5", result.Price)
	}
	if result.Error != "" {
		t.Errorf("Error should be empty, got %q", result.Error)
	}
}

func TestSpotResultErrorJSON(t *testing.T) {
	raw := `{"strategy": "sma", "error": "API timeout"}`
	var result SpotResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "API timeout" {
		t.Errorf("Error = %q, want %q", result.Error, "API timeout")
	}
}

func TestHyperliquidResultJSON(t *testing.T) {
	raw := `{
		"strategy": "sma",
		"symbol": "BTC",
		"timeframe": "1h",
		"signal": -1,
		"price": 55000,
		"mode": "paper",
		"platform": "hyperliquid",
		"timestamp": "2026-01-01T00:00:00Z"
	}`

	var result HyperliquidResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Signal != -1 {
		t.Errorf("Signal = %d, want -1", result.Signal)
	}
	if result.Mode != "paper" {
		t.Errorf("Mode = %q, want %q", result.Mode, "paper")
	}
	if result.Platform != "hyperliquid" {
		t.Errorf("Platform = %q, want %q", result.Platform, "hyperliquid")
	}
}

func TestHyperliquidExecuteResultJSON(t *testing.T) {
	raw := `{
		"execution": {
			"action": "buy",
			"symbol": "BTC",
			"size": 0.01,
			"fill": {"avg_px": 55000.5, "total_sz": 0.01}
		},
		"platform": "hyperliquid",
		"timestamp": "2026-01-01T00:00:00Z"
	}`

	var result HyperliquidExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution == nil {
		t.Fatal("Execution should not be nil")
	}
	if result.Execution.Action != "buy" {
		t.Errorf("Action = %q, want %q", result.Execution.Action, "buy")
	}
	if result.Execution.Fill == nil {
		t.Fatal("Fill should not be nil")
	}
	if result.Execution.Fill.AvgPx != 55000.5 {
		t.Errorf("AvgPx = %g, want 55000.5", result.Execution.Fill.AvgPx)
	}
}

func TestHyperliquidExecuteResultJSON_WithOID(t *testing.T) {
	raw := `{
		"execution": {
			"action": "buy",
			"symbol": "BTC",
			"size": 0.01,
			"fill": {"avg_px": 55000.5, "total_sz": 0.01, "oid": 1234567890, "fee": 0.35}
		},
		"platform": "hyperliquid",
		"timestamp": "2026-01-01T00:00:00Z"
	}`

	var result HyperliquidExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution == nil || result.Execution.Fill == nil {
		t.Fatal("Execution and Fill should not be nil")
	}
	if result.Execution.Fill.OID != 1234567890 {
		t.Errorf("OID = %d, want 1234567890", result.Execution.Fill.OID)
	}
	if result.Execution.Fill.Fee != 0.35 {
		t.Errorf("Fee = %g, want 0.35", result.Execution.Fill.Fee)
	}
}

func TestHyperliquidExecuteResultJSON_NoOID(t *testing.T) {
	// Backwards compatibility: fill without oid/fee should still parse
	raw := `{
		"execution": {
			"action": "sell",
			"symbol": "ETH",
			"size": 0.5,
			"fill": {"avg_px": 2100, "total_sz": 0.5}
		},
		"platform": "hyperliquid"
	}`

	var result HyperliquidExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution.Fill.OID != 0 {
		t.Errorf("OID should be 0 when absent, got %d", result.Execution.Fill.OID)
	}
	if result.Execution.Fill.Fee != 0 {
		t.Errorf("Fee should be 0 when absent, got %g", result.Execution.Fill.Fee)
	}
}

func TestTopStepResultJSON(t *testing.T) {
	raw := `{
		"strategy": "sma",
		"symbol": "ES",
		"timeframe": "15m",
		"signal": 1,
		"price": 5200.5,
		"contract_spec": {"tick_size": 0.25, "tick_value": 12.5, "multiplier": 50, "margin": 500},
		"market_open": true,
		"mode": "paper",
		"platform": "topstep"
	}`

	var result TopStepResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.ContractSpec.Multiplier != 50 {
		t.Errorf("Multiplier = %g, want 50", result.ContractSpec.Multiplier)
	}
	if !result.MarketOpen {
		t.Error("MarketOpen should be true")
	}
	if result.ContractSpec.Margin != 500 {
		t.Errorf("Margin = %g, want 500", result.ContractSpec.Margin)
	}
}

func TestTopStepExecuteResultJSON(t *testing.T) {
	raw := `{
		"execution": {
			"action": "buy",
			"symbol": "ES",
			"contracts": 2,
			"fill": {"avg_px": 5200.25, "total_contracts": 2}
		},
		"platform": "topstep"
	}`

	var result TopStepExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution.Contracts != 2 {
		t.Errorf("Contracts = %d, want 2", result.Execution.Contracts)
	}
	if result.Execution.Fill.TotalContracts != 2 {
		t.Errorf("TotalContracts = %d, want 2", result.Execution.Fill.TotalContracts)
	}
}

func TestRobinhoodResultJSON(t *testing.T) {
	raw := `{
		"strategy": "sma",
		"symbol": "BTC",
		"signal": 1,
		"price": 60000,
		"mode": "paper",
		"platform": "robinhood"
	}`

	var result RobinhoodResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Platform != "robinhood" {
		t.Errorf("Platform = %q, want %q", result.Platform, "robinhood")
	}
}

func TestRobinhoodExecuteResultJSON(t *testing.T) {
	raw := `{
		"execution": {
			"action": "buy",
			"symbol": "BTC",
			"amount_usd": 500,
			"fill": {"avg_px": 60000.5, "quantity": 0.00833}
		},
		"platform": "robinhood"
	}`

	var result RobinhoodExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution.AmountUSD != 500 {
		t.Errorf("AmountUSD = %g, want 500", result.Execution.AmountUSD)
	}
}

func TestOKXResultJSON(t *testing.T) {
	raw := `{
		"strategy": "sma",
		"symbol": "BTC",
		"signal": -1,
		"price": 55000,
		"mode": "live",
		"platform": "okx"
	}`

	var result OKXResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Signal != -1 {
		t.Errorf("Signal = %d, want -1", result.Signal)
	}
	if result.Platform != "okx" {
		t.Errorf("Platform = %q, want %q", result.Platform, "okx")
	}
}

func TestOKXExecuteResultJSON(t *testing.T) {
	raw := `{
		"execution": {
			"action": "sell",
			"symbol": "BTC",
			"size": 0.05,
			"fill": {"avg_px": 55000, "total_sz": 0.05}
		},
		"platform": "okx"
	}`

	var result OKXExecuteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution.Size != 0.05 {
		t.Errorf("Size = %g, want 0.05", result.Execution.Size)
	}
}

func TestContractSpecJSON(t *testing.T) {
	raw := `{"tick_size": 0.25, "tick_value": 12.5, "multiplier": 50, "margin": 6600}`
	var spec ContractSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.TickSize != 0.25 {
		t.Errorf("TickSize = %g, want 0.25", spec.TickSize)
	}
	if spec.TickValue != 12.5 {
		t.Errorf("TickValue = %g, want 12.5", spec.TickValue)
	}
	if spec.Multiplier != 50 {
		t.Errorf("Multiplier = %g, want 50", spec.Multiplier)
	}
	if spec.Margin != 6600 {
		t.Errorf("Margin = %g, want 6600", spec.Margin)
	}
}

// --- RunHyperliquidClose contract tests (#341) ---
//
// RunHyperliquidClose has FIVE distinct return paths and the kill-switch
// correctness depends on each one returning the right (result, err) shape:
//
//   1. exit 0 + valid JSON + Error == ""   → (result, nil) — clean success
//   2. exit 0 + valid JSON + Error != ""   → (result, err) — anomalous; envelope wins
//   3. exit !=0 + valid JSON + Error != "" → (result, err) — expected failure path
//   4. exit !=0 + valid JSON + Error == "" → (result, err) — defensive; never silently OK
//   5. malformed JSON                       → (nil, err)   — always failure
//
// Without these tests, a future "simplification" of the parse logic could
// collapse case (4) into success, reintroducing the #341-class bug at the
// JSON-parse boundary. Test-side: writes a temporary Python script that
// behaves like close_hyperliquid_position.py but with controllable output.

// These tests exercise parseHyperliquidCloseOutput directly (the pure decision
// helper extracted from RunHyperliquidClose) so they don't depend on
// .venv/bin/python3, which isn't installed in the Go CI job.

// Case 1: clean success — exit 0, valid JSON, no error field.
func TestParseHyperliquidCloseOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ETH","fill":{"avg_px":3000,"total_sz":0.5,"oid":12345,"fee":0.6}},"platform":"hyperliquid","timestamp":"2026-04-19T00:00:00Z"}`)
	result, _, err := parseHyperliquidCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil || result.Close.Fill == nil {
		t.Fatalf("expected populated result, got %+v", result)
	}
	if result.Close.Fill.TotalSz != 0.5 {
		t.Errorf("TotalSz = %g, want 0.5", result.Close.Fill.TotalSz)
	}
	if result.Close.Fill.Fee != 0.6 {
		t.Errorf("Fee = %g, want 0.6 — Fee field must be parsed for accounting", result.Close.Fill.Fee)
	}
	if result.Close.Fill.OID != 12345 {
		t.Errorf("OID = %d, want 12345", result.Close.Fill.OID)
	}
}

// Case 2: exit 0 with populated error field — should NOT be silently treated
// as success (the JSON envelope is authoritative).
func TestParseHyperliquidCloseOutput_Exit0WithError(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ETH","fill":{}},"platform":"hyperliquid","timestamp":"x","error":"sdk timeout"}`)
	result, _, err := parseHyperliquidCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err for exit 0 with error envelope")
	}
	if result == nil || result.Error != "sdk timeout" {
		t.Errorf("expected populated result.Error, got %+v", result)
	}
	if !strings.Contains(err.Error(), "sdk timeout") {
		t.Errorf("err must surface envelope error message, got %v", err)
	}
}

// Case 3: exit 1 with valid JSON error — the expected failure path.
func TestParseHyperliquidCloseOutput_Exit1WithError(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ETH","fill":{}},"platform":"hyperliquid","timestamp":"x","error":"hl rate limited"}`)
	runErr := fmt.Errorf("exit status 1")
	result, _, err := parseHyperliquidCloseOutput(stdout, "", runErr)
	if err == nil {
		t.Fatal("expected non-nil err for exit 1 — kill switch must latch")
	}
	if result == nil || result.Error != "hl rate limited" {
		t.Errorf("expected populated result.Error, got %+v", result)
	}
	if !strings.Contains(err.Error(), "hl rate limited") {
		t.Errorf("err must include underlying error, got %v", err)
	}
}

// Case 4: exit non-zero with valid JSON but no error field. Tightened
// contract (item #2 from review): never silently report success on a
// non-zero exit. Without this test, a regression that drops the exit-code
// check would let the kill switch clear virtual state on a script crash
// that happened to print parseable JSON before dying.
func TestParseHyperliquidCloseOutput_Exit1WithoutErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ETH","fill":{}},"platform":"hyperliquid","timestamp":"x"}`)
	runErr := fmt.Errorf("exit status 1")
	_, _, err := parseHyperliquidCloseOutput(stdout, "", runErr)
	if err == nil {
		t.Fatal("expected non-nil err for exit 1 even without error field")
	}
	if !strings.Contains(err.Error(), "no error field") {
		t.Errorf("err message should mention missing error field, got %v", err)
	}
}

// Case 5: malformed JSON. Always a failure regardless of exit code, because
// the kill switch cannot infer outcome from garbage.
func TestParseHyperliquidCloseOutput_MalformedJSON(t *testing.T) {
	result, _, err := parseHyperliquidCloseOutput([]byte("this is not json"), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err for malformed JSON")
	}
	if result != nil {
		t.Errorf("result should be nil for unparseable output, got %+v", result)
	}
}

// already_flat field round-trips through the parser so the Go-side
// AlreadyFlat routing has the signal it needs (#350). Without this, a
// silent struct-tag regression would make every adapter-side already-flat
// case fall back to ClosedCoins.
func TestParseHyperliquidCloseOutput_AlreadyFlatFieldParsed(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ETH","fill":{},"already_flat":true},"platform":"hyperliquid","timestamp":"x"}`)
	result, _, err := parseHyperliquidCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil {
		t.Fatalf("expected populated result.Close, got %+v", result)
	}
	if !result.Close.AlreadyFlat {
		t.Errorf("AlreadyFlat = false, want true — Go side cannot route to AlreadyFlat slice without this field")
	}
}

// ── OKX close parser tests (#345) ──────────────────────────────────────
// Same 5-case matrix as parseHyperliquidCloseOutput — mirrors the HL tests
// one-to-one because the two parsers implement the same contract. Any
// relaxation of the contract on one side must fail a test on that side so
// kill-switch correctness parity is mechanically enforced.

func TestParseOKXCloseOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{"avg_px":42000,"total_sz":0.01,"oid":"abc123","fee":0.02}},"platform":"okx","timestamp":"2026-04-19T00:00:00Z"}`)
	result, _, err := parseOKXCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil || result.Close.Fill == nil {
		t.Fatalf("expected populated result, got %+v", result)
	}
	if result.Close.Fill.TotalSz != 0.01 {
		t.Errorf("TotalSz = %g, want 0.01", result.Close.Fill.TotalSz)
	}
	if result.Close.Fill.OID != "abc123" {
		t.Errorf("OID = %q, want abc123 (ccxt IDs are strings, unlike HL ints)", result.Close.Fill.OID)
	}
	if result.Close.Fill.Fee != 0.02 {
		t.Errorf("Fee = %g, want 0.02 — fee parsing is load-bearing for post-kill accounting", result.Close.Fill.Fee)
	}
}

func TestParseOKXCloseOutput_Exit0WithError(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"okx","timestamp":"x","error":"okx auth failed"}`)
	_, _, err := parseOKXCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err for exit 0 with error envelope")
	}
	if !strings.Contains(err.Error(), "okx auth failed") {
		t.Errorf("err must surface envelope error, got %v", err)
	}
}

func TestParseOKXCloseOutput_Exit1WithError(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"okx","timestamp":"x","error":"okx rate limited"}`)
	runErr := fmt.Errorf("exit status 1")
	_, _, err := parseOKXCloseOutput(stdout, "", runErr)
	if err == nil {
		t.Fatal("expected non-nil err for exit 1 — kill switch must latch")
	}
	if !strings.Contains(err.Error(), "okx rate limited") {
		t.Errorf("err must include underlying error, got %v", err)
	}
}

func TestParseOKXCloseOutput_Exit1WithoutErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{}},"platform":"okx","timestamp":"x"}`)
	runErr := fmt.Errorf("exit status 1")
	_, _, err := parseOKXCloseOutput(stdout, "", runErr)
	if err == nil {
		t.Fatal("expected non-nil err for exit 1 even without error field — silent crash must not clear virtual state")
	}
	if !strings.Contains(err.Error(), "no error field") {
		t.Errorf("err message should mention missing error field, got %v", err)
	}
}

func TestParseOKXCloseOutput_AlreadyFlatFieldParsed(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"BTC","fill":{},"already_flat":true},"platform":"okx","timestamp":"x"}`)
	result, _, err := parseOKXCloseOutput(stdout, "", nil)
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

func TestParseOKXCloseOutput_MalformedJSON(t *testing.T) {
	result, _, err := parseOKXCloseOutput([]byte("not json"), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err for malformed JSON")
	}
	if result != nil {
		t.Errorf("result should be nil for unparseable output, got %+v", result)
	}
}

// ── OKX positions fetcher parser tests (#345) ───────────────────────────

func TestParseOKXPositionsOutput_Success(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"BTC","size":0.01,"entry_price":42000,"side":"long"},{"coin":"ETH","size":-0.5,"entry_price":3000,"side":"short"}],"platform":"okx","timestamp":"x"}`)
	result, _, err := parseOKXPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(result.Positions) != 2 {
		t.Fatalf("expected 2 positions, got %d", len(result.Positions))
	}
	if result.Positions[0].Coin != "BTC" || result.Positions[0].Size != 0.01 {
		t.Errorf("position[0] = %+v", result.Positions[0])
	}
	// Short size must be negative — load-bearing for on-chain direction
	// classification in forceCloseOKXLive.
	if result.Positions[1].Size != -0.5 {
		t.Errorf("short size must be signed negative, got %g", result.Positions[1].Size)
	}
}

func TestParseOKXPositionsOutput_EmptyIsSuccess(t *testing.T) {
	stdout := []byte(`{"positions":[],"platform":"okx","timestamp":"x"}`)
	result, _, err := parseOKXPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("empty positions must be success, got err=%v", err)
	}
	if len(result.Positions) != 0 {
		t.Errorf("expected 0 positions, got %d", len(result.Positions))
	}
}

func TestParseOKXPositionsOutput_ErrorEnvelope(t *testing.T) {
	stdout := []byte(`{"positions":[],"platform":"okx","timestamp":"x","error":"OKX auth failed"}`)
	runErr := fmt.Errorf("exit status 1")
	_, _, err := parseOKXPositionsOutput(stdout, "", runErr)
	if err == nil {
		t.Fatal("expected non-nil err when envelope has error field — kill switch must latch")
	}
	if !strings.Contains(err.Error(), "OKX auth failed") {
		t.Errorf("err must include envelope error, got %v", err)
	}
}

func TestParseOKXPositionsOutput_MalformedJSON(t *testing.T) {
	_, _, err := parseOKXPositionsOutput([]byte("garbage"), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err for malformed JSON — cannot infer positions from garbage")
	}
}
