package main

import (
	"encoding/json"
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
	json.Unmarshal([]byte(raw), &result)
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
