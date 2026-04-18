package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Only aggregate top10/bottom10 messages are produced now; per-product
	// sections were removed in issue #310.
	if _, ok := lb.Messages["top10"]; !ok {
		t.Error("Missing top10 leaderboard message")
	}
	if _, ok := lb.Messages["bottom10"]; !ok {
		t.Error("Missing bottom10 leaderboard message")
	}
	for _, key := range []string{"spot", "perps", "options", "futures"} {
		if _, ok := lb.Messages[key]; ok {
			t.Errorf("Per-product section %q should no longer be emitted", key)
		}
	}

	// Verify timestamp is recent.
	if lb.Timestamp.IsZero() {
		t.Error("Leaderboard timestamp is zero")
	}

	// Top10 should contain strategy IDs, PnL data, and the Trades column.
	top10Msg := lb.Messages["top10"]
	if top10Msg == "" {
		t.Fatal("top10 message is empty")
	}
	if !containsStr(top10Msg, "sma-btc") {
		t.Error("top10 message should contain sma-btc")
	}
	if !containsStr(top10Msg, "Top All-Time Performers") {
		t.Error("top10 message should contain title")
	}
	if !containsStr(top10Msg, "TOTAL") {
		t.Error("top10 message should contain TOTAL row")
	}
	if !containsStr(top10Msg, "winning") {
		t.Error("top10 message should contain winning/losing/flat counts")
	}
	if !containsStr(top10Msg, "Trades") {
		t.Error("top10 message should contain Trades column header")
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

	// All-time top/bottom messages must respect LeaderboardTopN.
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

	// Pre-write a leaderboard file. Per-product keys, even if present in a
	// stale file, should be ignored — only top10 + bottom10 are posted.
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

	// Only top10 + bottom10 should land on the dedicated channel.
	if len(mock.messages) != 2 {
		t.Fatalf("expected 2 messages on dedicated channel, got %d: %v", len(mock.messages), mock.messages)
	}
	for _, m := range mock.messages {
		if m.channelID != "lb-ch" {
			t.Errorf("expected channel lb-ch, got %s (content=%q)", m.channelID, m.content)
		}
		if m.content != "top10-msg" && m.content != "bottom10-msg" {
			t.Errorf("unexpected message content %q on dedicated channel", m.content)
		}
	}
}

// TestPostLeaderboard_FallbackRouting verifies that when no LeaderboardChannel
// is configured, top10/bottom10 broadcast to all configured channels. Stale
// per-product keys in the file are ignored.
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
	//   top10    → broadcast to all unique channels (spot-ch, perps-ch) = 2
	//   bottom10 → broadcast to all unique channels (spot-ch, perps-ch) = 2
	// Total = 4. Stale "spot" key must be ignored.
	if len(mock.messages) != 4 {
		t.Fatalf("expected 4 messages from fallback routing, got %d: %v", len(mock.messages), mock.messages)
	}

	for _, m := range mock.messages {
		if m.content == "spot-msg" {
			t.Errorf("stale per-product spot-msg should not be posted, got channel=%s", m.channelID)
		}
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
// Discord (with dedicated channel) should receive top10/bottom10 on lb-ch and
// Telegram (without) should broadcast top10/bottom10 across all its channels.
func TestPostLeaderboard_MixedBackends(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "state.db")}

	lb := LeaderboardData{
		Messages: map[string]string{
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

	// Discord: top10 + bottom10 should land on discord-lb.
	if len(discord.messages) != 2 {
		t.Fatalf("expected 2 discord messages on discord-lb, got %d: %v", len(discord.messages), discord.messages)
	}
	for _, m := range discord.messages {
		if m.channelID != "discord-lb" {
			t.Errorf("expected all discord messages on discord-lb, got %s (content=%q)", m.channelID, m.content)
		}
	}

	// Telegram: top10 and bottom10 each broadcast to all 4 channels = 8 total.
	if len(telegram.messages) != 8 {
		t.Fatalf("expected 8 telegram messages from broadcast routing, got %d: %v", len(telegram.messages), telegram.messages)
	}

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
	// Sort order: hl-rsi-eth yields +$100/$500 = +20%; hl-mom-eth yields
	// +$100/$800 = +12.5%. Higher PnL% must appear first. (#309 review nit)
	rsiIdx := strings.Index(msg, "hl-rsi-eth")
	momIdx := strings.Index(msg, "hl-mom-eth")
	if rsiIdx < 0 || momIdx < 0 || rsiIdx >= momIdx {
		t.Errorf("Expected hl-rsi-eth (+20%%) before hl-mom-eth (+12.5%%), got rsi=%d mom=%d in:\n%s", rsiIdx, momIdx, msg)
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

// TestFindLeaderboardSummariesByChannel covers the multi-match case called out
// in review item 3 on #309: -summary <ch> should surface every configured entry
// sharing a channel, in config order.
func TestFindLeaderboardSummariesByChannel(t *testing.T) {
	cfg := &Config{
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", Channel: "hl-ch", Frequency: "6h"},
			{Platform: "hyperliquid", Ticker: "ETH", Channel: "hl-ch", Frequency: "12h"},
			{Platform: "okx", Channel: "okx-ch", Frequency: "6h"},
		},
	}

	got := findLeaderboardSummariesByChannel(cfg, "hl-ch")
	if len(got) != 2 {
		t.Fatalf("hl-ch matches: got %d, want 2", len(got))
	}
	if got[0].Ticker != "" || got[1].Ticker != "ETH" {
		t.Errorf("expected config order [unfiltered, ETH], got [%q, %q]", got[0].Ticker, got[1].Ticker)
	}

	if got := findLeaderboardSummariesByChannel(cfg, "okx-ch"); len(got) != 1 {
		t.Errorf("okx-ch matches: got %d, want 1", len(got))
	}

	if got := findLeaderboardSummariesByChannel(cfg, "none"); got != nil {
		t.Errorf("unknown channel should return nil, got %v", got)
	}
}
