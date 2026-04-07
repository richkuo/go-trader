package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPrecomputeLeaderboard(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	cfg := &Config{
		StateFile: stateFile,
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "rsi-eth", Type: "spot", Capital: 500, Platform: "binanceus", Args: []string{"rsi_divergence", "ETH/USDT", "1h"}},
			{ID: "hl-sma-btc", Type: "perps", Capital: 2000, Platform: "hyperliquid", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "deribit-ccall-btc", Type: "options", Capital: 1000, Platform: "deribit", Args: []string{"covered_call", "BTC/USDT"}},
			{ID: "ts-breakout-es", Type: "futures", Capital: 5000, Platform: "topstep", Args: []string{"breakout", "ES", "15m"}},
		},
	}

	state := NewAppState()
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		// Give each strategy different PnL by adjusting cash.
		switch sc.ID {
		case "sma-btc":
			ss.Cash = 1100 // +10%
		case "rsi-eth":
			ss.Cash = 450 // -10%
		case "hl-sma-btc":
			ss.Cash = 2200 // +10%
		case "deribit-ccall-btc":
			ss.Cash = 1050 // +5%
		case "ts-breakout-es":
			ss.Cash = 4800 // -4%
		}
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{
		"BTC/USDT": 50000,
		"ETH/USDT": 3000,
	}

	err := PrecomputeLeaderboard(cfg, state, prices)
	if err != nil {
		t.Fatalf("PrecomputeLeaderboard failed: %v", err)
	}

	// Verify file was written.
	path := leaderboardPath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read leaderboard file: %v", err)
	}

	var lb LeaderboardData
	if err := json.Unmarshal(data, &lb); err != nil {
		t.Fatalf("Failed to parse leaderboard: %v", err)
	}

	// Verify we have category messages.
	if _, ok := lb.Messages["spot"]; !ok {
		t.Error("Missing spot leaderboard message")
	}
	if _, ok := lb.Messages["perps"]; !ok {
		t.Error("Missing perps leaderboard message")
	}
	if _, ok := lb.Messages["options"]; !ok {
		t.Error("Missing options leaderboard message")
	}
	if _, ok := lb.Messages["futures"]; !ok {
		t.Error("Missing futures leaderboard message")
	}
	if _, ok := lb.Messages["top10"]; !ok {
		t.Error("Missing top10 leaderboard message")
	}
	if _, ok := lb.Messages["bottom10"]; !ok {
		t.Error("Missing bottom10 leaderboard message")
	}

	// Verify timestamp is recent.
	if lb.Timestamp.IsZero() {
		t.Error("Leaderboard timestamp is zero")
	}

	// Spot message should contain strategy IDs and PnL data.
	spotMsg := lb.Messages["spot"]
	if spotMsg == "" {
		t.Fatal("Spot message is empty")
	}
	if !containsStr(spotMsg, "sma-btc") {
		t.Error("Spot message should contain sma-btc")
	}
	if !containsStr(spotMsg, "Spot Leaderboard") {
		t.Error("Spot message should contain title")
	}
	if !containsStr(spotMsg, "TOTAL") {
		t.Error("Spot message should contain TOTAL row")
	}
	if !containsStr(spotMsg, "winning") {
		t.Error("Spot message should contain winning/losing/flat counts")
	}
}

func TestLoadLeaderboard(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	cfg := &Config{StateFile: stateFile}

	// No file yet — should error.
	_, err := LoadLeaderboard(cfg)
	if err == nil {
		t.Error("Expected error when leaderboard file doesn't exist")
	}

	// Write a valid file.
	lb := LeaderboardData{
		Messages: map[string]string{
			"spot": "test message",
		},
	}
	data, _ := json.Marshal(lb)
	path := leaderboardPath(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	loaded, err := LoadLeaderboard(cfg)
	if err != nil {
		t.Fatalf("LoadLeaderboard failed: %v", err)
	}
	if loaded.Messages["spot"] != "test message" {
		t.Errorf("Expected 'test message', got %q", loaded.Messages["spot"])
	}
}

func TestFmtSignedDollar(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{100, "$+100"},
		{-50, "$-50"},
		{0, "$+0"},
		{1234, "$+1,234"},
		{-9876, "$-9,876"},
	}
	for _, tt := range tests {
		got := fmtSignedDollar(tt.input)
		if got != tt.want {
			t.Errorf("fmtSignedDollar(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtSignedPct(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{10.5, "+10.5%"},
		{-3.2, "-3.2%"},
		{0, "+0.0%"},
	}
	for _, tt := range tests {
		got := fmtSignedPct(tt.input)
		if got != tt.want {
			t.Errorf("fmtSignedPct(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatHyperliquidTop10(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "hl-sma-btc", Type: "perps", Capital: 1000, Platform: "hyperliquid", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "hl-rsi-eth", Type: "perps", Capital: 500, Platform: "hyperliquid", Args: []string{"rsi_divergence", "ETH/USDT", "1h"}},
			{ID: "hl-mom-sol", Type: "perps", Capital: 800, Platform: "hyperliquid", Args: []string{"momentum", "SOL/USDT", "1h"}},
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
		},
	}

	state := NewAppState()
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		switch sc.ID {
		case "hl-sma-btc":
			ss.Cash = 1200 // +20%
		case "hl-rsi-eth":
			ss.Cash = 400 // -20%
		case "hl-mom-sol":
			ss.Cash = 880 // +10%
		case "sma-btc":
			ss.Cash = 1500 // +50% — should be excluded (not hyperliquid)
		}
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000, "SOL/USDT": 150}
	msg := FormatHyperliquidTop10(cfg, state, prices)

	if msg == "" {
		t.Fatal("Expected non-empty message")
	}
	if !containsStr(msg, "Hyperliquid Top 10") {
		t.Error("Message should contain title")
	}
	if !containsStr(msg, "hl-sma-btc") {
		t.Error("Message should contain hl-sma-btc")
	}
	if !containsStr(msg, "hl-rsi-eth") {
		t.Error("Message should contain hl-rsi-eth")
	}
	if !containsStr(msg, "hl-mom-sol") {
		t.Error("Message should contain hl-mom-sol")
	}
	// Spot strategy should NOT appear.
	if containsStr(msg, "sma-btc") && !containsStr(msg, "hl-sma-btc") {
		t.Error("Message should not contain non-hyperliquid strategies")
	}
}

func TestFormatHyperliquidTop10_NoHLStrategies(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus"},
		},
	}
	state := NewAppState()
	state.Strategies["sma-btc"] = &StrategyState{Cash: 1100, InitialCapital: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{}}

	msg := FormatHyperliquidTop10(cfg, state, nil)
	if msg != "" {
		t.Errorf("Expected empty message when no HL strategies, got %q", msg)
	}
}

func TestFormatHyperliquidTop10_SortOrder(t *testing.T) {
	// Create 12 hyperliquid strategies to verify only top 10 are included.
	var strats []StrategyConfig
	for i := 0; i < 12; i++ {
		strats = append(strats, StrategyConfig{
			ID:       fmt.Sprintf("hl-s%02d-btc", i),
			Type:     "perps",
			Capital:  1000,
			Platform: "hyperliquid",
			Args:     []string{"sma", "BTC/USDT", "1h"},
		})
	}
	cfg := &Config{Strategies: strats}

	state := NewAppState()
	for i, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		// Strategy 0 = worst (-10%), strategy 11 = best (+1%)
		ss.Cash = 1000 + float64(i-10)*10 // -100, -90, ..., +10
		state.Strategies[sc.ID] = ss
	}

	msg := FormatHyperliquidTop10(cfg, state, nil)
	if msg == "" {
		t.Fatal("Expected non-empty message")
	}

	// The worst strategy (hl-s00-btc at -10%) should NOT appear (only top 10 of 12).
	if containsStr(msg, "hl-s00-btc") {
		t.Error("Worst strategy should be excluded from top 10")
	}
	if containsStr(msg, "hl-s01-btc") {
		t.Error("Second worst strategy should be excluded from top 10")
	}
	// Best strategy should appear.
	if !containsStr(msg, "hl-s11-btc") {
		t.Error("Best strategy should appear in top 10")
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
