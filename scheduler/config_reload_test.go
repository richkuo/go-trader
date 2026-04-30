package main

import (
	"strings"
	"testing"
	"time"
)

func TestApplyHotReloadConfigAppliesAllowedFields(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 600,
		DBFile:          "scheduler/state.db",
		Discord: DiscordConfig{
			Enabled:            true,
			Channels:           map[string]string{"spot": "old-spot"},
			DMChannels:         map[string]string{"binanceus-paper": "old-dm"},
			LeaderboardTopN:    5,
			LeaderboardChannel: "old-lb",
		},
		Telegram: TelegramConfig{
			Enabled:    true,
			Channels:   map[string]string{"spot": "old-tg"},
			DMChannels: map[string]string{"binanceus-paper": "old-tg-dm"},
		},
		SummaryFrequency: map[string]string{"spot": "hourly"},
		Strategies: []StrategyConfig{{
			ID:              "spot-btc",
			Type:            "spot",
			Platform:        "binanceus",
			Script:          "shared_scripts/check_strategy.py",
			Args:            []string{"sma_crossover", "BTC/USDT", "1h"},
			Capital:         1000,
			MaxDrawdownPct:  20,
			IntervalSeconds: 600,
		}, {
			ID:              "hl-eth",
			Type:            "perps",
			Platform:        "hyperliquid",
			Script:          "shared_scripts/check_hyperliquid.py",
			Args:            []string{"triple_ema_bidir", "ETH", "1h", "--mode=paper"},
			Capital:         500,
			MaxDrawdownPct:  50,
			IntervalSeconds: 600,
			Leverage:        2,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
	}
	next := &Config{
		IntervalSeconds: 300,
		DBFile:          "scheduler/state.db",
		Discord: DiscordConfig{
			Enabled:            true,
			Channels:           map[string]string{"spot": "new-spot"},
			DMChannels:         map[string]string{"binanceus-paper": "new-dm"},
			LeaderboardTopN:    7,
			LeaderboardChannel: "new-lb",
		},
		Telegram: TelegramConfig{
			Enabled:    true,
			Channels:   map[string]string{"spot": "new-tg"},
			DMChannels: map[string]string{"binanceus-paper": "new-tg-dm"},
		},
		SummaryFrequency: map[string]string{"spot": "30m"},
		Strategies: []StrategyConfig{{
			ID:              "spot-btc",
			Type:            "spot",
			Platform:        "binanceus",
			Script:          "shared_scripts/check_strategy.py",
			Args:            []string{"sma_crossover", "BTC/USDT", "1h"},
			Capital:         1200,
			MaxDrawdownPct:  15,
			IntervalSeconds: 300,
		}, {
			ID:              "hl-eth",
			Type:            "perps",
			Platform:        "hyperliquid",
			Script:          "shared_scripts/check_hyperliquid.py",
			Args:            []string{"triple_ema_bidir", "ETH", "1h", "--mode=paper"},
			Capital:         700,
			MaxDrawdownPct:  45,
			IntervalSeconds: 900,
			Leverage:        5,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 70},
	}
	summaryLast := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	state := &AppState{
		LastSummaryPost: map[string]time.Time{"spot": summaryLast},
		Strategies: map[string]*StrategyState{
			"spot-btc": {
				ID: "spot-btc", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 20},
			},
			"hl-eth": {
				ID:        "hl-eth",
				Cash:      450,
				RiskState: RiskState{MaxDrawdownPct: 50},
			},
		},
	}
	mock := &mockNotifier{}
	tgMock := &mockNotifier{}
	notifier := NewMultiNotifier(
		notifierBackend{notifier: mock, channels: cfg.Discord.Channels, dmChannels: cfg.Discord.DMChannels, leaderboardChannel: cfg.Discord.LeaderboardChannel},
		notifierBackend{notifier: tgMock, channels: cfg.Telegram.Channels, dmChannels: cfg.Telegram.DMChannels, plainText: true},
	)
	server := NewStatusServer(state, nil, "", cfg.Strategies, nil)

	changes, err := applyHotReloadConfig(cfg, next, state, notifier, server)
	if err != nil {
		t.Fatalf("applyHotReloadConfig returned error: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected reload changes")
	}
	joined := strings.Join(changes, "\n")
	for _, want := range []string{
		"interval_seconds: 600 -> 300",
		"strategy[spot-btc].capital: $1000.00 -> $1200.00",
		"strategy[spot-btc].max_drawdown_pct: 20.00% -> 15.00%",
		"strategy[hl-eth].leverage: 2.00x -> 5.00x",
		"portfolio_risk.max_drawdown_pct: 25.00% -> 30.00%",
		"discord.channels:",
		"telegram.channels:",
		"summary_frequency:",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("changes missing %q:\n%s", want, joined)
		}
	}
	if cfg.IntervalSeconds != 300 {
		t.Errorf("IntervalSeconds = %d, want 300", cfg.IntervalSeconds)
	}
	if cfg.Strategies[0].Capital != 1200 || cfg.Strategies[0].MaxDrawdownPct != 15 || cfg.Strategies[0].IntervalSeconds != 300 {
		t.Errorf("spot config not reloaded: %+v", cfg.Strategies[0])
	}
	if cfg.Strategies[1].Leverage != 5 || cfg.Strategies[1].IntervalSeconds != 900 {
		t.Errorf("perps config not reloaded: %+v", cfg.Strategies[1])
	}
	if got := state.Strategies["spot-btc"].Cash; got != 1100 {
		t.Errorf("spot cash = %g, want 1100 (capital delta applied)", got)
	}
	if got := state.Strategies["spot-btc"].RiskState.MaxDrawdownPct; got != 15 {
		t.Errorf("spot risk max drawdown = %g, want 15", got)
	}
	if got := state.LastSummaryPost["spot"]; !got.Equal(summaryLast) {
		t.Errorf("summary last post changed during reload: got %v, want %v", got, summaryLast)
	}
	notifier.SendToChannel("binanceus", "spot", "hello")
	if len(mock.messages) != 1 || mock.messages[0].channelID != "new-spot" {
		t.Fatalf("discord channel not reloaded, messages=%#v", mock.messages)
	}
	if len(tgMock.messages) != 1 || tgMock.messages[0].channelID != "new-tg" {
		t.Fatalf("telegram channel not reloaded, messages=%#v", tgMock.messages)
	}
	if server.strategies[0].Capital != 1200 {
		t.Errorf("status server strategies not updated: %+v", server.strategies[0])
	}
}

func TestApplyHotReloadConfigRejectsStrategySetChange(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 100, MaxDrawdownPct: 10,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 200, MaxDrawdownPct: 10,
	}, {
		ID: "s2", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 100, MaxDrawdownPct: 10,
	}})

	_, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err == nil {
		t.Fatal("expected strategy set change to be rejected")
	}
	if !strings.Contains(err.Error(), "strategy set changed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].Capital != 100 {
		t.Errorf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
	}
}

func TestApplyHotReloadConfigRejectsNonReloadableStrategyField(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Args: []string{"a"}, Capital: 100, MaxDrawdownPct: 10,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "y.py", Args: []string{"a"}, Capital: 200, MaxDrawdownPct: 10,
	}})

	_, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err == nil {
		t.Fatal("expected script change to be rejected")
	}
	if !strings.Contains(err.Error(), "non-hot-reloadable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].Capital != 100 {
		t.Errorf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
	}
}

func TestApplyHotReloadConfigRejectsLeverageChangeWithOpenPerpsPosition(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1200, MaxDrawdownPct: 12, Leverage: 5,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Cash: 900,
			RiskState: RiskState{MaxDrawdownPct: 10},
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
			},
		},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected open perps leverage change to be rejected")
	}
	if !strings.Contains(err.Error(), "leverage changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].Capital != 1000 || cfg.Strategies[0].MaxDrawdownPct != 10 || cfg.Strategies[0].Leverage != 2 {
		t.Fatalf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
	}
	if state.Strategies["hl-eth"].Cash != 900 || state.Strategies["hl-eth"].RiskState.MaxDrawdownPct != 10 {
		t.Fatalf("state mutated after rejected reload: %+v", state.Strategies["hl-eth"])
	}
	if state.Strategies["hl-eth"].Positions["ETH"].Leverage != 2 {
		t.Fatalf("position leverage mutated after rejected reload: %+v", state.Strategies["hl-eth"].Positions["ETH"])
	}
}

// #486: margin_mode is hot-reloadable when flat — same envelope as Leverage.
func TestApplyHotReloadConfigRejectsMarginModeChangeWithOpenPerpsPosition(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "cross",
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Cash: 900,
			RiskState: RiskState{MaxDrawdownPct: 10},
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
			},
		},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected margin_mode change with open position to be rejected")
	}
	if !strings.Contains(err.Error(), "margin_mode changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].MarginMode != "isolated" {
		t.Fatalf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
	}
}

// #486: margin_mode change is allowed when the strategy is flat.
func TestApplyHotReloadConfigAllowsMarginModeChangeWhenFlat(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "cross",
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("expected margin_mode change to succeed when flat, got: %v", err)
	}
	if cfg.Strategies[0].MarginMode != "cross" {
		t.Fatalf("MarginMode = %q, want %q", cfg.Strategies[0].MarginMode, "cross")
	}
}

func TestApplyHotReloadConfigPreservesRuntimeCapitalPctCapital(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "BTC", "1h"}, Capital: 2500, CapitalPct: 0.5, MaxDrawdownPct: 10, Leverage: 2,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "BTC", "1h"}, Capital: 100, CapitalPct: 0.5, MaxDrawdownPct: 12, Leverage: 2,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"s1": {ID: "s1", Cash: 2400, RiskState: RiskState{MaxDrawdownPct: 10}},
	}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig returned error: %v", err)
	}
	if cfg.Strategies[0].Capital != 2500 {
		t.Errorf("runtime capital_pct capital = %g, want preserved 2500", cfg.Strategies[0].Capital)
	}
	if state.Strategies["s1"].Cash != 2400 {
		t.Errorf("cash = %g, want preserved 2400", state.Strategies["s1"].Cash)
	}
	joined := strings.Join(changes, "\n")
	if strings.Contains(joined, ".capital:") {
		t.Fatalf("capital_pct fallback capital should not be hot-applied, changes:\n%s", joined)
	}
	if cfg.Strategies[0].MaxDrawdownPct != 12 || state.Strategies["s1"].RiskState.MaxDrawdownPct != 12 {
		t.Fatalf("other hot-reloadable fields should still apply, cfg=%+v state=%+v", cfg.Strategies[0], state.Strategies["s1"].RiskState)
	}
}

// #491: hot-reload mirrors LoadConfig peer validation — a reload that would
// introduce two HL perps strategies on the same coin with mismatched
// margin_mode/leverage must be rejected.
func TestApplyHotReloadConfigRejectsHLPeerMismatchOnReload(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth-a", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 5, MarginMode: "isolated",
	}, {
		ID: "hl-eth-b", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"b", "ETH", "1h"}, Capital: 500, MaxDrawdownPct: 10, Leverage: 5, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth-a", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 5, MarginMode: "isolated",
	}, {
		ID: "hl-eth-b", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"b", "ETH", "1h"}, Capital: 500, MaxDrawdownPct: 10, Leverage: 10, MarginMode: "isolated",
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth-a": {ID: "hl-eth-a", Cash: 1000},
		"hl-eth-b": {ID: "hl-eth-b", Cash: 500},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject peer leverage mismatch")
	}
	if !strings.Contains(err.Error(), "disagree on leverage") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func minimalReloadConfig(strategies []StrategyConfig) *Config {
	return &Config{
		IntervalSeconds: 600,
		DBFile:          "scheduler/state.db",
		Discord:         DiscordConfig{Channels: map[string]string{}},
		Telegram:        TelegramConfig{Channels: map[string]string{}},
		Strategies:      strategies,
		PortfolioRisk:   &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
	}
}
