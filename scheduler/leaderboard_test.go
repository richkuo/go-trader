package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrecomputeLeaderboard(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.db")

	cfg := &Config{
		DBFile: stateFile,
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
	stateFile := filepath.Join(dir, "state.db")
	cfg := &Config{DBFile: stateFile}

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
	stateFile := fmt.Sprintf("%s/state.db", dir)

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
		DBFile:     stateFile,
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

// TestPostLeaderboard_DedicatedChannel verifies that when DiscordConfig.LeaderboardChannel
// is set (wired into notifierBackend.leaderboardChannel), PostLeaderboard routes
// every category and all-time message to the dedicated channel instead of
// broadcasting across the per-platform channels.
func TestPostLeaderboard_DedicatedChannel(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "state.db")}

	// Pre-write a leaderboard file with messages for every category.
	lb := LeaderboardData{
		Messages: map[string]string{
			"spot":     "spot-msg",
			"perps":    "perps-msg",
			"options":  "options-msg",
			"futures":  "futures-msg",
			"top10":    "top10-msg",
			"bottom10": "bottom10-msg",
		},
	}
	raw, _ := json.Marshal(lb)
	if err := os.WriteFile(leaderboardPath(cfg), raw, 0600); err != nil {
		t.Fatalf("write leaderboard: %v", err)
	}

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier:           mock,
		channels:           map[string]string{"spot": "spot-ch", "perps": "perps-ch", "options": "options-ch", "futures": "futures-ch"},
		leaderboardChannel: "lb-ch",
	})

	if err := PostLeaderboard(cfg, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	// All 6 messages should land on the dedicated channel.
	if len(mock.messages) != 6 {
		t.Fatalf("expected 6 messages on dedicated channel, got %d: %v", len(mock.messages), mock.messages)
	}
	for _, m := range mock.messages {
		if m.channelID != "lb-ch" {
			t.Errorf("expected channel lb-ch, got %s (content=%q)", m.channelID, m.content)
		}
	}
}

// TestPostLeaderboard_FallbackRouting verifies that when no LeaderboardChannel is
// configured, PostLeaderboard preserves the legacy behavior: category messages
// go to the matching platform channel, and top10/bottom10 broadcast to all
// channels.
func TestPostLeaderboard_FallbackRouting(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "state.db")}

	lb := LeaderboardData{
		Messages: map[string]string{
			"spot":     "spot-msg",
			"top10":    "top10-msg",
			"bottom10": "bottom10-msg",
		},
	}
	raw, _ := json.Marshal(lb)
	if err := os.WriteFile(leaderboardPath(cfg), raw, 0600); err != nil {
		t.Fatalf("write leaderboard: %v", err)
	}

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"spot": "spot-ch", "perps": "perps-ch"},
	})

	if err := PostLeaderboard(cfg, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	// Expected sends:
	//   spot     → spot-ch  (1)
	//   top10    → broadcast to all unique channels (spot-ch, perps-ch) = 2
	//   bottom10 → broadcast to all unique channels (spot-ch, perps-ch) = 2
	// Total = 5
	if len(mock.messages) != 5 {
		t.Fatalf("expected 5 messages from fallback routing, got %d: %v", len(mock.messages), mock.messages)
	}

	// Spot message should hit only spot-ch.
	spotHits := 0
	for _, m := range mock.messages {
		if m.content == "spot-msg" {
			if m.channelID != "spot-ch" {
				t.Errorf("spot-msg should route to spot-ch, got %s", m.channelID)
			}
			spotHits++
		}
	}
	if spotHits != 1 {
		t.Errorf("expected 1 spot-msg send, got %d", spotHits)
	}

	// top10 and bottom10 each broadcast to both channels.
	for _, key := range []string{"top10-msg", "bottom10-msg"} {
		channels := map[string]bool{}
		for _, m := range mock.messages {
			if m.content == key {
				channels[m.channelID] = true
			}
		}
		if !channels["spot-ch"] || !channels["perps-ch"] {
			t.Errorf("%s should broadcast to spot-ch and perps-ch, got %v", key, channels)
		}
	}
}

// TestPostLeaderboard_MixedBackends is the regression test for the bug where
// HasLeaderboardChannel returning true on *any* backend caused all other
// backends to silently drop leaderboard messages. With per-backend routing,
// Discord (with dedicated channel) should receive every message on lb-ch and
// Telegram (without) should still get the legacy per-category / broadcast
// routing.
func TestPostLeaderboard_MixedBackends(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "state.db")}

	lb := LeaderboardData{
		Messages: map[string]string{
			"spot":     "spot-msg",
			"perps":    "perps-msg",
			"options":  "options-msg",
			"futures":  "futures-msg",
			"top10":    "top10-msg",
			"bottom10": "bottom10-msg",
		},
	}
	raw, _ := json.Marshal(lb)
	if err := os.WriteFile(leaderboardPath(cfg), raw, 0600); err != nil {
		t.Fatalf("write leaderboard: %v", err)
	}

	discord := &mockNotifier{}
	telegram := &mockNotifier{}
	notifier := NewMultiNotifier(
		notifierBackend{
			notifier: discord,
			channels: map[string]string{
				"spot":    "discord-spot",
				"perps":   "discord-perps",
				"options": "discord-options",
				"futures": "discord-futures",
			},
			leaderboardChannel: "discord-lb",
		},
		notifierBackend{
			notifier: telegram,
			channels: map[string]string{
				"spot":    "telegram-spot",
				"perps":   "telegram-perps",
				"options": "telegram-options",
				"futures": "telegram-futures",
			},
		},
	)

	if err := PostLeaderboard(cfg, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	// Discord: every one of the 6 messages should land on discord-lb.
	if len(discord.messages) != 6 {
		t.Fatalf("expected 6 discord messages on discord-lb, got %d: %v", len(discord.messages), discord.messages)
	}
	for _, m := range discord.messages {
		if m.channelID != "discord-lb" {
			t.Errorf("expected all discord messages on discord-lb, got %s (content=%q)", m.channelID, m.content)
		}
	}

	// Telegram: legacy routing.
	//   spot     → telegram-spot     (1)
	//   perps    → telegram-perps    (1)
	//   options  → telegram-options  (1)
	//   futures  → telegram-futures  (1)
	//   top10    → broadcast to all 4 unique channels (4)
	//   bottom10 → broadcast to all 4 unique channels (4)
	// Total = 12.
	if len(telegram.messages) != 12 {
		t.Fatalf("expected 12 telegram messages from legacy routing, got %d: %v", len(telegram.messages), telegram.messages)
	}

	// Verify each per-category message lands on its matching telegram channel.
	expectCategory := map[string]string{
		"spot-msg":    "telegram-spot",
		"perps-msg":   "telegram-perps",
		"options-msg": "telegram-options",
		"futures-msg": "telegram-futures",
	}
	for content, wantCh := range expectCategory {
		hits := 0
		for _, m := range telegram.messages {
			if m.content == content {
				if m.channelID != wantCh {
					t.Errorf("%s: expected telegram channel %s, got %s", content, wantCh, m.channelID)
				}
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("%s: expected 1 telegram send, got %d", content, hits)
		}
	}

	// top10 / bottom10 should each broadcast to all 4 telegram channels.
	for _, content := range []string{"top10-msg", "bottom10-msg"} {
		seen := map[string]bool{}
		for _, m := range telegram.messages {
			if m.content == content {
				seen[m.channelID] = true
			}
		}
		expected := []string{"telegram-spot", "telegram-perps", "telegram-options", "telegram-futures"}
		for _, ch := range expected {
			if !seen[ch] {
				t.Errorf("%s: expected broadcast to %s, missing (got %v)", content, ch, seen)
			}
		}
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

func TestBuildLeaderboardSummary_PlatformOnly(t *testing.T) {
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
			ss.Cash = 1200
		case "hl-rsi-eth":
			ss.Cash = 400
		case "hl-mom-sol":
			ss.Cash = 880
		case "sma-btc":
			ss.Cash = 1500
		}
		state.Strategies[sc.ID] = ss
	}

	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", TopN: 10, Channel: "chan-1"}
	msg := BuildLeaderboardSummary(lc, cfg, state, nil)
	if msg == "" {
		t.Fatal("Expected non-empty message")
	}
	if !containsStr(msg, "Hyperliquid Top 3") {
		t.Errorf("Expected title 'Hyperliquid Top 3' (3 HL strategies), got:\n%s", msg)
	}
	if !containsStr(msg, "hl-sma-btc") || !containsStr(msg, "hl-rsi-eth") || !containsStr(msg, "hl-mom-sol") {
		t.Errorf("Expected all 3 HL strategies in message, got:\n%s", msg)
	}
	// Non-HL strategy must be excluded.
	if containsStr(msg, " sma-btc ") {
		t.Errorf("Expected non-HL strategy to be excluded, got:\n%s", msg)
	}
}

func TestBuildLeaderboardSummary_TickerFilter(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "hl-sma-btc", Type: "perps", Capital: 1000, Platform: "hyperliquid", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "hl-rsi-eth", Type: "perps", Capital: 500, Platform: "hyperliquid", Args: []string{"rsi_divergence", "ETH/USDT", "1h"}},
			{ID: "hl-mom-eth", Type: "perps", Capital: 800, Platform: "hyperliquid", Args: []string{"momentum", "ETH/USDT", "1h"}},
		},
	}
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		ss.Cash = sc.Capital + 100 // all profitable
		state.Strategies[sc.ID] = ss
	}

	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", Ticker: "eth", TopN: 5, Channel: "chan-1"}
	msg := BuildLeaderboardSummary(lc, cfg, state, nil)
	if msg == "" {
		t.Fatal("Expected non-empty message")
	}
	if !containsStr(msg, "Hyperliquid ETH Top 2") {
		t.Errorf("Expected title with ETH ticker, got:\n%s", msg)
	}
	if containsStr(msg, "hl-sma-btc") {
		t.Errorf("BTC strategy should be excluded by ticker filter, got:\n%s", msg)
	}
	if !containsStr(msg, "hl-rsi-eth") || !containsStr(msg, "hl-mom-eth") {
		t.Errorf("Expected both ETH strategies, got:\n%s", msg)
	}
}

func TestBuildLeaderboardSummary_DefaultTopN(t *testing.T) {
	var strats []StrategyConfig
	for i := 0; i < 8; i++ {
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
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		ss.Cash = 1100
		state.Strategies[sc.ID] = ss
	}

	// TopN=0 means default (5).
	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", Channel: "c1"}
	msg := BuildLeaderboardSummary(lc, cfg, state, nil)
	if !containsStr(msg, "Hyperliquid Top 5") {
		t.Errorf("Expected default TopN=5 in title, got:\n%s", msg)
	}
}

func TestBuildLeaderboardSummary_NoMatches(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
		},
	}
	state := NewAppState()
	state.Strategies["sma-btc"] = NewStrategyState(cfg.Strategies[0])

	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", Channel: "c1"}
	if msg := BuildLeaderboardSummary(lc, cfg, state, nil); msg != "" {
		t.Errorf("Expected empty message when no strategies match, got:\n%s", msg)
	}
}

func TestLeaderboardSummaryConfig_Key(t *testing.T) {
	tests := []struct {
		lc   LeaderboardSummaryConfig
		want string
	}{
		{LeaderboardSummaryConfig{Platform: "hyperliquid", Ticker: "ETH", Channel: "123"}, "hyperliquid:eth:123"},
		{LeaderboardSummaryConfig{Platform: "hyperliquid", Channel: "123"}, "hyperliquid:*:123"},
		{LeaderboardSummaryConfig{Platform: "BinanceUS", Ticker: "btc", Channel: "456"}, "binanceus:btc:456"},
	}
	for i, tt := range tests {
		if got := tt.lc.Key(); got != tt.want {
			t.Errorf("case %d: Key()=%q, want %q", i, got, tt.want)
		}
	}
}

func TestLeaderboardSummaryConfig_ParsedFrequency(t *testing.T) {
	tests := []struct {
		freq string
		want time.Duration
	}{
		{"", 0},
		{"6h", 6 * time.Hour},
		{"12h", 12 * time.Hour},
		{"invalid", 0},
	}
	for _, tt := range tests {
		lc := LeaderboardSummaryConfig{Frequency: tt.freq}
		if got := lc.ParsedFrequency(); got != tt.want {
			t.Errorf("Frequency=%q: got %v, want %v", tt.freq, got, tt.want)
		}
	}
}
