package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildLeaderboardMessages(t *testing.T) {
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
		switch sc.ID {
		case "sma-btc":
			ss.Cash = 1100
			ss.TradeHistory = []Trade{{StrategyID: "sma-btc"}, {StrategyID: "sma-btc"}, {StrategyID: "sma-btc"}}
		case "rsi-eth":
			ss.Cash = 450
			ss.TradeHistory = []Trade{{StrategyID: "rsi-eth"}}
		case "hl-sma-btc":
			ss.Cash = 2200
			ss.TradeHistory = []Trade{{StrategyID: "hl-sma-btc"}, {StrategyID: "hl-sma-btc"}}
		case "deribit-ccall-btc":
			ss.Cash = 1050
		case "ts-breakout-es":
			ss.Cash = 4800
		}
		state.Strategies[sc.ID] = ss
	}

	prices := map[string]float64{
		"BTC/USDT": 50000,
		"ETH/USDT": 3000,
	}

	messages := BuildLeaderboardMessages(cfg, state, prices, nil, nil, nil, nil)
	if messages == nil {
		t.Fatal("BuildLeaderboardMessages returned nil")
	}

	if _, ok := messages["top"]; !ok {
		t.Error("Missing top leaderboard message")
	}
	if _, ok := messages["bottom"]; !ok {
		t.Error("Missing bottom leaderboard message")
	}
	for _, key := range []string{"spot", "perps", "options", "futures"} {
		if _, ok := messages[key]; ok {
			t.Errorf("Per-product section %q should no longer be emitted", key)
		}
	}

	topMsg := messages["top"]
	if topMsg == "" {
		t.Fatal("top message is empty")
	}
	if !containsStr(topMsg, "sma-btc") {
		t.Error("top message should contain sma-btc")
	}
}

func TestBuildLeaderboardMessages_Empty(t *testing.T) {
	cfg := &Config{DBFile: filepath.Join(t.TempDir(), "state.db")}
	state := NewAppState()

	if messages := BuildLeaderboardMessages(cfg, state, nil, nil, nil, nil, nil); messages != nil {
		t.Errorf("Expected nil messages for empty state, got %v", messages)
	}
}

func TestLeaderboardTopN(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"zero falls back to default", &Config{}, 5},
		{"configured value wins", &Config{Discord: DiscordConfig{LeaderboardTopN: 10}}, 10},
		{"negative falls back to default", &Config{Discord: DiscordConfig{LeaderboardTopN: -1}}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaderboardTopN(tc.cfg); got != tc.want {
				t.Errorf("leaderboardTopN = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildLeaderboardMessages_TopN(t *testing.T) {
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
		Strategies: strats,
		Discord:    DiscordConfig{LeaderboardTopN: 3},
	}

	state := NewAppState()
	for i, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		ss.Cash = 1000 + float64(i)*10
		state.Strategies[sc.ID] = ss
	}

	messages := BuildLeaderboardMessages(cfg, state, map[string]float64{"BTC/USDT": 50000}, nil, nil, nil, nil)
	if messages == nil {
		t.Fatal("BuildLeaderboardMessages returned nil")
	}

	topMsg := messages["top"]
	if topMsg == "" {
		t.Fatal("Expected non-empty top all-time message")
	}
	if !containsStr(topMsg, "sma-s07") {
		t.Error("top all-time should contain sma-s07 when top_n=3")
	}
	if !containsStr(topMsg, "sma-s05") {
		t.Error("top all-time should contain sma-s05 when top_n=3")
	}
	if containsStr(topMsg, "sma-s04") {
		t.Error("top all-time should not contain sma-s04 when top_n=3")
	}

	bottomMsg := messages["bottom"]
	if bottomMsg == "" {
		t.Fatal("Expected non-empty bottom all-time message")
	}
	if !containsStr(bottomMsg, "sma-s00") {
		t.Error("bottom all-time should contain sma-s00 when top_n=3")
	}
	if !containsStr(bottomMsg, "sma-s02") {
		t.Error("bottom all-time should contain sma-s02 when top_n=3")
	}
	if containsStr(bottomMsg, "sma-s03") {
		t.Error("bottom all-time should not contain sma-s03 when top_n=3")
	}
}

func leaderboardTestFixture() (*Config, *AppState, map[string]float64) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Capital: 1000, Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
			{ID: "rsi-eth", Type: "spot", Capital: 500, Platform: "binanceus", Args: []string{"rsi_divergence", "ETH/USDT", "1h"}},
		},
	}
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		ss := NewStrategyState(sc)
		ss.Cash = sc.Capital + 100
		state.Strategies[sc.ID] = ss
	}
	return cfg, state, map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000}
}

func TestPostLeaderboard_DedicatedChannel(t *testing.T) {
	cfg, state, prices := leaderboardTestFixture()

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier:           mock,
		channels:           map[string]string{"spot": "spot-ch", "perps": "perps-ch", "options": "options-ch", "futures": "futures-ch"},
		leaderboardChannel: "lb-ch",
	})

	if err := PostLeaderboard(cfg, state, prices, nil, nil, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	if len(mock.messages) != 2 {
		t.Fatalf("expected 2 messages on dedicated channel, got %d: %v", len(mock.messages), mock.messages)
	}
	for _, m := range mock.messages {
		if m.channelID != "lb-ch" {
			t.Errorf("expected channel lb-ch, got %s (content=%q)", m.channelID, m.content)
		}
	}
}

func TestPostLeaderboard_FallbackRouting(t *testing.T) {
	cfg, state, prices := leaderboardTestFixture()

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"spot": "spot-ch", "perps": "perps-ch"},
	})

	if err := PostLeaderboard(cfg, state, prices, nil, nil, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	if len(mock.messages) != 4 {
		t.Fatalf("expected 4 messages from fallback routing, got %d: %v", len(mock.messages), mock.messages)
	}

	for _, ch := range []string{"spot-ch", "perps-ch"} {
		seen := 0
		for _, m := range mock.messages {
			if m.channelID == ch {
				seen++
			}
		}
		if seen != 2 {
			t.Errorf("channel %s: expected 2 messages (top+bottom), got %d", ch, seen)
		}
	}
}

func TestPostLeaderboard_MixedBackends(t *testing.T) {
	cfg, state, prices := leaderboardTestFixture()

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

	if err := PostLeaderboard(cfg, state, prices, nil, nil, notifier); err != nil {
		t.Fatalf("PostLeaderboard: %v", err)
	}

	if len(discord.messages) != 2 {
		t.Fatalf("expected 2 discord messages on discord-lb, got %d: %v", len(discord.messages), discord.messages)
	}
	for _, m := range discord.messages {
		if m.channelID != "discord-lb" {
			t.Errorf("expected all discord messages on discord-lb, got %s (content=%q)", m.channelID, m.content)
		}
	}

	if len(telegram.messages) != 8 {
		t.Fatalf("expected 8 telegram messages from broadcast routing, got %d: %v", len(telegram.messages), telegram.messages)
	}

	for _, ch := range []string{"telegram-spot", "telegram-perps", "telegram-options", "telegram-futures"} {
		seen := 0
		for _, m := range telegram.messages {
			if m.channelID == ch {
				seen++
			}
		}
		if seen != 2 {
			t.Errorf("telegram channel %s: expected 2 messages (top+bottom), got %d", ch, seen)
		}
	}
}

func TestPostLeaderboard_NoStrategies(t *testing.T) {
	cfg := &Config{}
	state := NewAppState()
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{notifier: mock, channels: map[string]string{"spot": "spot-ch"}})

	err := PostLeaderboard(cfg, state, nil, nil, nil, notifier)
	if err == nil {
		t.Error("expected error when no strategies configured")
	}
	if err != nil && !strings.Contains(err.Error(), "no strategies to leaderboard") {
		t.Errorf("unexpected error message: %q", err.Error())
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected no messages sent, got %d", len(mock.messages))
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
	msg := BuildLeaderboardSummary(lc, cfg, state, nil, nil, nil, nil, nil)
	if msg == "" {
		t.Fatal("Expected non-empty message")
	}
	if !containsStr(msg, "Hyperliquid Top 3") {
		t.Errorf("Expected title 'Hyperliquid Top 3' (3 HL strategies), got:\n%s", msg)
	}
	if !containsStr(msg, "hl-sma-btc") || !containsStr(msg, "hl-rsi-eth") || !containsStr(msg, "hl-mom-sol") {
		t.Errorf("Expected all 3 HL strategies in message, got:\n%s", msg)
	}
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
		ss.Cash = sc.Capital + 100
		state.Strategies[sc.ID] = ss
	}

	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", Ticker: "eth", TopN: 5, Channel: "chan-1"}
	msg := BuildLeaderboardSummary(lc, cfg, state, nil, nil, nil, nil, nil)
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
	rsiIdx := strings.Index(msg, "hl-rsi-eth")
	momIdx := strings.Index(msg, "hl-mom-eth")
	if rsiIdx < 0 || momIdx < 0 || rsiIdx >= momIdx {
		t.Errorf("Expected hl-rsi-eth (+20%%) before hl-mom-eth (+12.5%%), got rsi=%d mom=%d in:\n%s", rsiIdx, momIdx, msg)
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
	if msg := BuildLeaderboardSummary(lc, cfg, state, nil, nil, nil, nil, nil); msg != "" {
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

func TestBuildLeaderboardMessages_AdjustedTotal(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	cfg := &Config{
		DBFile: t.TempDir() + "/state.db",
		Strategies: []StrategyConfig{
			{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
			{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 5000, InitialCapital: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth": {ID: "hl-eth", Cash: 5000, InitialCapital: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	prices := map[string]float64{}

	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 8000,
	}
	accountShared := detectSharedWallets(cfg.Strategies)

	msgs := BuildLeaderboardMessages(cfg, state, prices, nil, nil, walletBalances, accountShared)
	if msgs == nil {
		t.Fatal("BuildLeaderboardMessages returned nil")
	}

	for key, msg := range msgs {
		var totalLine string
		for _, line := range strings.Split(msg, "\n") {
			if strings.HasPrefix(line, "TOTAL") {
				totalLine = line
				break
			}
		}
		if totalLine == "" {
			t.Errorf("[%s] no TOTAL row found", key)
			continue
		}
		if !strings.Contains(totalLine, "8,000") {
			t.Errorf("[%s] TOTAL row should show adjusted $8,000; got: %q", key, totalLine)
		}
		if strings.Contains(totalLine, "10,000") {
			t.Errorf("[%s] TOTAL row must NOT show naive $10,000; got: %q", key, totalLine)
		}
	}
}

func TestBuildLeaderboardSummary_AdjustedTotal(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	cfg := &Config{
		DBFile: t.TempDir() + "/state.db",
		Strategies: []StrategyConfig{
			{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
			{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 5000, InitialCapital: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth": {ID: "hl-eth", Cash: 5000, InitialCapital: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	prices := map[string]float64{}

	lc := LeaderboardSummaryConfig{Platform: "hyperliquid", TopN: 5, Channel: "test"}

	walletKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{walletKey: 8000}
	accountShared := detectSharedWallets(cfg.Strategies)

	msg := BuildLeaderboardSummary(lc, cfg, state, prices, nil, nil, walletBalances, accountShared)
	if msg == "" {
		t.Fatal("BuildLeaderboardSummary returned empty string")
	}
	var totalLine string
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "TOTAL") {
			totalLine = line
			break
		}
	}
	if totalLine == "" {
		t.Fatalf("no TOTAL row found in:\n%s", msg)
	}
	if !strings.Contains(totalLine, "8,000") {
		t.Errorf("TOTAL row should show adjusted $8,000; got: %q\nfull msg:\n%s", totalLine, msg)
	}
}
