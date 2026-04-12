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
			ss.TradeHistory = []Trade{{StrategyID: "sma-btc"}, {StrategyID: "sma-btc"}, {StrategyID: "sma-btc"}}
		case "rsi-eth":
			ss.Cash = 450 // -10%
			ss.TradeHistory = []Trade{{StrategyID: "rsi-eth"}}
		case "hl-sma-btc":
			ss.Cash = 2200 // +10%
			ss.TradeHistory = []Trade{{StrategyID: "hl-sma-btc"}, {StrategyID: "hl-sma-btc"}}
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
	if !containsStr(spotMsg, "Trades") {
		t.Error("Spot message should contain Trades column header")
	}

	// Verify top10 message also contains Trades column.
	top10Msg := lb.Messages["top10"]
	if !containsStr(top10Msg, "Trades") {
		t.Error("Top10 message should contain Trades column header")
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

func TestFormatHyperliquidTopN(t *testing.T) {
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
			ss.TradeHistory = []Trade{{StrategyID: "hl-sma-btc"}, {StrategyID: "hl-sma-btc"}}
		case "hl-rsi-eth":
			ss.Cash = 400 // -20%
			ss.TradeHistory = []Trade{{StrategyID: "hl-rsi-eth"}}
		case "hl-mom-sol":
			ss.Cash = 880 // +10%
			ss.TradeHistory = []Trade{{StrategyID: "hl-mom-sol"}, {StrategyID: "hl-mom-sol"}, {StrategyID: "hl-mom-sol"}}
		case "sma-btc":
			ss.Cash = 1500 // +50% — should be excluded (not hyperliquid)
		}
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000, "SOL/USDT": 150}
	msg := FormatHyperliquidTopN(cfg, state, prices)

	if msg == "" {
		t.Fatal("Expected non-empty message")
	}
	// Default LeaderboardTopN is 5, so title should be "Hyperliquid Top 5".
	if !containsStr(msg, "Hyperliquid Top 5") {
		t.Error("Message should contain dynamic title reflecting default topN=5")
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
	// Trades column should appear.
	if !containsStr(msg, "Trades") {
		t.Error("Message should contain Trades column header")
	}
}

func TestFormatHyperliquidTopN_NoHLStrategies(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus"},
		},
	}
	state := NewAppState()
	state.Strategies["sma-btc"] = &StrategyState{Cash: 1100, InitialCapital: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{}}

	msg := FormatHyperliquidTopN(cfg, state, nil)
	if msg != "" {
		t.Errorf("Expected empty message when no HL strategies, got %q", msg)
	}
}

func TestFormatHyperliquidTopN_SortOrder(t *testing.T) {
	// Create 12 hyperliquid strategies to verify only topN are included.
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
	// Explicitly set LeaderboardTopN=10 to preserve original test intent.
	cfg := &Config{
		Strategies: strats,
		Discord:    DiscordConfig{LeaderboardTopN: 10},
	}

	state := NewAppState()
	for i, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		// Strategy 0 = worst (-10%), strategy 11 = best (+1%)
		ss.Cash = 1000 + float64(i-10)*10 // -100, -90, ..., +10
		state.Strategies[sc.ID] = ss
	}

	msg := FormatHyperliquidTopN(cfg, state, nil)
	if msg == "" {
		t.Fatal("Expected non-empty message")
	}

	// Dynamic title should reflect configured topN=10.
	if !containsStr(msg, "Hyperliquid Top 10") {
		t.Error("Message should contain dynamic title reflecting configured topN=10")
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

func TestFormatPlatformTopN(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "hl-sma-btc", Type: "perps", Capital: 1000, Platform: "hyperliquid", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "deribit-ccall-btc", Type: "options", Capital: 1000, Platform: "deribit", Args: []string{"covered_call", "BTC/USDT"}},
			{ID: "deribit-vol-eth", Type: "options", Capital: 500, Platform: "deribit", Args: []string{"vol_smile", "ETH/USDT"}},
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
		},
	}

	state := NewAppState()
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		switch sc.ID {
		case "hl-sma-btc":
			ss.Cash = 1200 // +20%
			ss.TradeHistory = []Trade{{StrategyID: "hl-sma-btc"}}
		case "deribit-ccall-btc":
			ss.Cash = 1150 // +15%
			ss.TradeHistory = []Trade{{StrategyID: "deribit-ccall-btc"}, {StrategyID: "deribit-ccall-btc"}}
		case "deribit-vol-eth":
			ss.Cash = 450 // -10%
			ss.TradeHistory = []Trade{{StrategyID: "deribit-vol-eth"}}
		case "sma-btc":
			ss.Cash = 1500 // +50%
		}
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000}

	// Test Deribit platform leaderboard.
	msg := FormatPlatformTopN("deribit", "🎯", "Deribit Top 10", cfg, state, prices)
	if msg == "" {
		t.Fatal("Expected non-empty message for deribit")
	}
	if !containsStr(msg, "Deribit Top 10") {
		t.Error("Message should contain Deribit title")
	}
	if !containsStr(msg, "deribit-ccall-btc") {
		t.Error("Message should contain deribit-ccall-btc")
	}
	if !containsStr(msg, "deribit-vol-eth") {
		t.Error("Message should contain deribit-vol-eth")
	}
	// Non-deribit strategies should NOT appear.
	if containsStr(msg, "hl-sma-btc") {
		t.Error("Message should not contain hyperliquid strategies")
	}
	if containsStr(msg, "sma-btc") {
		t.Error("Message should not contain binanceus strategies")
	}
	if !containsStr(msg, "Trades") {
		t.Error("Message should contain Trades column header")
	}

	// Test platform with no strategies returns empty.
	msg = FormatPlatformTopN("okx", "🔶", "OKX Top 10", cfg, state, prices)
	if msg != "" {
		t.Errorf("Expected empty message for platform with no strategies, got %q", msg)
	}

	// Test that FormatHyperliquidTopN still works via the wrapper.
	hlMsg := FormatHyperliquidTopN(cfg, state, prices)
	if hlMsg == "" {
		t.Fatal("Expected non-empty message from FormatHyperliquidTopN wrapper")
	}
	// Default topN is 5, so wrapper title should reflect "Hyperliquid Top 5".
	if !containsStr(hlMsg, "Hyperliquid Top 5") {
		t.Error("Wrapper message should contain Hyperliquid Top 5 title")
	}
	if !containsStr(hlMsg, "hl-sma-btc") {
		t.Error("Wrapper message should contain hl-sma-btc")
	}
}

// TestLeaderboardTopNDefault verifies that leaderboardTopN returns 5 when unset.
func TestLeaderboardTopNDefault(t *testing.T) {
	cfg := &Config{}
	if got := leaderboardTopN(cfg); got != 5 {
		t.Errorf("leaderboardTopN with zero value = %d, want 5", got)
	}
}

// TestLeaderboardTopNConfigured verifies that leaderboardTopN respects the configured value.
func TestLeaderboardTopNConfigured(t *testing.T) {
	cfg := &Config{Discord: DiscordConfig{LeaderboardTopN: 10}}
	if got := leaderboardTopN(cfg); got != 10 {
		t.Errorf("leaderboardTopN with configured value = %d, want 10", got)
	}
}

// TestLeaderboardTopNNegative verifies that leaderboardTopN ignores negative values.
func TestLeaderboardTopNNegative(t *testing.T) {
	cfg := &Config{Discord: DiscordConfig{LeaderboardTopN: -1}}
	if got := leaderboardTopN(cfg); got != 5 {
		t.Errorf("leaderboardTopN with negative value = %d, want 5", got)
	}
}

// TestPrecomputeLeaderboardTopN verifies that LeaderboardTopN limits the entries shown.
func TestPrecomputeLeaderboardTopN(t *testing.T) {
	dir := t.TempDir()
	stateFile := fmt.Sprintf("%s/state.json", dir)

	// Create 8 spot strategies.
	var strats []StrategyConfig
	for i := 0; i < 8; i++ {
		strats = append(strats, StrategyConfig{
			ID:       fmt.Sprintf("sma-s%02d", i),
			Type:     "spot",
			Capital:  1000,
			Platform: "binanceus",
			Args:     []string{"sma_crossover", "BTC/USDT", "1h"},
		})
	}

	cfg := &Config{
		StateFile:  stateFile,
		Strategies: strats,
		Discord:    DiscordConfig{LeaderboardTopN: 3},
	}

	state := NewAppState()
	for i, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		ss.Cash = 1000 + float64(i)*10 // different PnL for each
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{"BTC/USDT": 50000}
	if err := PrecomputeLeaderboard(cfg, state, prices); err != nil {
		t.Fatalf("PrecomputeLeaderboard failed: %v", err)
	}

	lb, err := LoadLeaderboard(cfg)
	if err != nil {
		t.Fatalf("LoadLeaderboard failed: %v", err)
	}

	spotMsg := lb.Messages["spot"]
	if spotMsg == "" {
		t.Fatal("Expected non-empty spot message")
	}

	// The best strategy (sma-s07) should appear (top 3).
	if !containsStr(spotMsg, "sma-s07") {
		t.Error("Best strategy sma-s07 should appear in top 3")
	}
	if !containsStr(spotMsg, "sma-s06") {
		t.Error("Second-best strategy sma-s06 should appear in top 3")
	}
	if !containsStr(spotMsg, "sma-s05") {
		t.Error("Third-best strategy sma-s05 should appear in top 3")
	}
	// sma-s04 is 4th — should NOT appear in top 3.
	if containsStr(spotMsg, "sma-s04") {
		t.Error("4th strategy sma-s04 should not appear when top_n=3")
	}

	// All-time top/bottom messages use a different code path
	// (formatAllTimeMessage) and must also respect LeaderboardTopN.
	top10Msg := lb.Messages["top10"]
	if top10Msg == "" {
		t.Fatal("Expected non-empty top10 all-time message")
	}
	// Top 3 by PnL%: sma-s07, sma-s06, sma-s05.
	if !containsStr(top10Msg, "sma-s07") {
		t.Error("top10 all-time should contain sma-s07 when top_n=3")
	}
	if !containsStr(top10Msg, "sma-s05") {
		t.Error("top10 all-time should contain sma-s05 when top_n=3")
	}
	// sma-s04 is 4th — should NOT appear.
	if containsStr(top10Msg, "sma-s04") {
		t.Error("top10 all-time should not contain sma-s04 when top_n=3")
	}

	bottom10Msg := lb.Messages["bottom10"]
	if bottom10Msg == "" {
		t.Fatal("Expected non-empty bottom10 all-time message")
	}
	// Bottom 3 by PnL%: sma-s00, sma-s01, sma-s02.
	if !containsStr(bottom10Msg, "sma-s00") {
		t.Error("bottom10 all-time should contain sma-s00 when top_n=3")
	}
	if !containsStr(bottom10Msg, "sma-s02") {
		t.Error("bottom10 all-time should contain sma-s02 when top_n=3")
	}
	// sma-s03 is 4th-worst — should NOT appear.
	if containsStr(bottom10Msg, "sma-s03") {
		t.Error("bottom10 all-time should not contain sma-s03 when top_n=3")
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
