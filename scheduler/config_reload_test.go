package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
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
	var mu sync.RWMutex
	server := NewStatusServer(state, &mu, "", cfg.Strategies, nil)

	// SIGHUP path holds mu.Lock() across applyHotReloadConfig (see
	// reloadConfig in main.go). Mirror that here so this test also covers
	// the deadlock risk fixed by giving StatusServer.strategies its own mu.
	type reloadResult struct {
		changes []string
		err     error
	}
	resultCh := make(chan reloadResult, 1)
	go func() {
		mu.Lock()
		defer mu.Unlock()
		c, e := applyHotReloadConfig(cfg, next, state, notifier, server)
		resultCh <- reloadResult{changes: c, err: e}
	}()
	var res reloadResult
	select {
	case res = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("applyHotReloadConfig deadlocked while caller held mu.Lock()")
	}
	changes, err := res.changes, res.err
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

func TestApplyHotReloadConfigAllowsOpenCloseStrategyChanges(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py",
		Args: []string{"triple_ema", "BTC/USDT", "1h"}, Capital: 100, MaxDrawdownPct: 10,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py",
		Args: []string{"triple_ema", "BTC/USDT", "1h"}, Capital: 100, MaxDrawdownPct: 10,
		OpenStrategy: StrategyRef{Name: "triple_ema"}, CloseStrategy: &StrategyRef{Name: "tp_at_pct"},
	}})

	changes, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig returned error: %v", err)
	}
	joined := strings.Join(changes, "\n")
	for _, want := range []string{
		"strategy[s1].open_strategy:",
		"strategy[s1].close_strategy:",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("changes missing %q:\n%s", want, joined)
		}
	}
	if cfg.Strategies[0].OpenStrategy.Name != "triple_ema" {
		t.Fatalf("OpenStrategy.Name = %q, want triple_ema", cfg.Strategies[0].OpenStrategy.Name)
	}
	if cfg.Strategies[0].CloseStrategy == nil || cfg.Strategies[0].CloseStrategy.Name != "tp_at_pct" {
		t.Fatalf("CloseStrategy = %#v, want tp_at_pct", cfg.Strategies[0].CloseStrategy)
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

func TestApplyHotReloadConfigAllowsTrailingStopPctChangeWithOpenPosition(t *testing.T) {
	oldTrail := 3.0
	newTrail := 4.0
	oldMinMove := 0.5
	newMinMove := 0.25
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", TrailingStopPct: &oldTrail, TrailingStopMinMovePct: &oldMinMove,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", TrailingStopPct: &newTrail, TrailingStopMinMovePct: &newMinMove,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if cfg.Strategies[0].TrailingStopPct == nil || *cfg.Strategies[0].TrailingStopPct != 4 {
		t.Fatalf("TrailingStopPct=%v, want 4", cfg.Strategies[0].TrailingStopPct)
	}
	if cfg.Strategies[0].TrailingStopMinMovePct == nil || *cfg.Strategies[0].TrailingStopMinMovePct != 0.25 {
		t.Fatalf("TrailingStopMinMovePct=%v, want 0.25", cfg.Strategies[0].TrailingStopMinMovePct)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "trailing_stop_pct") || !strings.Contains(joined, "trailing_stop_min_move_pct") {
		t.Fatalf("changes=%v, want trailing_stop_pct and trailing_stop_min_move_pct entries", changes)
	}
}

func TestApplyHotReloadConfigRejectsFixedToTrailingWithOpenPosition(t *testing.T) {
	trail := 3.0
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", TrailingStopPct: &trail,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject fixed-to-trailing mode switch")
	}
	if !strings.Contains(err.Error(), "trailing_stop_pct mode changed") {
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

// #656 — direction change while a position is open must be rejected. Toggling
// from "long" → "short" mid-position would either orphan the existing long or
// flip it on the next signal; both desync virtual state from the exchange.
func TestApplyHotReloadConfigRejectsDirectionChangeWithOpenPerpsPosition(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionShort,
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
		t.Fatal("expected direction change with open position to be rejected")
	}
	if !strings.Contains(err.Error(), "direction changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].Direction != DirectionLong {
		t.Fatalf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
	}
}

// #716 item 1 — adding an sl_after rule while a position is open must be
// rejected. Without this guard, the new rule would engage on the next cleared
// tier (post-TP machinery + trailing walker for trail_from_here) without the
// validation the open respected.
func TestApplyHotReloadConfigRejectsSLAfterAddWithOpenPosition(t *testing.T) {
	tieredOpen := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		},
	}
	tieredWithSLAfter := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": "breakeven",
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		},
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tieredOpen,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tieredWithSLAfter,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject sl_after addition with open position")
	}
	if !strings.Contains(err.Error(), "sl_after rules changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// #716 item 1 — sl_after rule changes are allowed when the strategy is flat.
func TestApplyHotReloadConfigAllowsSLAfterAddWhenFlat(t *testing.T) {
	tieredOpen := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	tieredWithSLAfter := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": "breakeven",
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tieredOpen,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tieredWithSLAfter,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("expected sl_after change to be allowed when flat, got: %v", err)
	}
}

// #716 item 1 — switching from breakeven to trail_from_here mid-position is
// the highest-risk transition (engages trailing walker without open validation).
func TestApplyHotReloadConfigRejectsSLAfterModeChangeWithOpenPosition(t *testing.T) {
	tierWithBreakeven := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": "breakeven",
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	tierWithTrail := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": map[string]interface{}{
				"kind":            "trail_from_here",
				"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
			},
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierWithBreakeven,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierWithTrail,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject sl_after mode switch")
	}
	if !strings.Contains(err.Error(), "sl_after rules changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// #736 — switching sl_after from scalar atr_offset to a regime-aware shape
// with a position open is a higher-risk transition than tweaking the scalar
// value: the post-TP machinery would arm against a re-derived target the
// open never saw. Verifies the existing tierSLAfterRules.EqualForReload site
// picks up the new SLAfterRule.Equal contract (which compares regime blocks
// via RegimeATRBlock.EqualForReload).
func TestApplyHotReloadConfigRejectsSLAfterScalarToRegimeWithOpenPosition(t *testing.T) {
	tierScalar := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": map[string]interface{}{"atr_mult": 0.25},
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	tierRegime := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": map[string]interface{}{
				"trend_regime": map[string]interface{}{
					"trending_up":   map[string]interface{}{"atr_multiple": 0.25},
					"trending_down": map[string]interface{}{"atr_multiple": 0.25},
					"ranging":       map[string]interface{}{"atr_multiple": 0.0},
				},
			},
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierScalar,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierRegime,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject sl_after scalar→regime shape change with open position")
	}
	if !strings.Contains(err.Error(), "sl_after rules changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// #736 — switching the regime block's per-label atr value (same shape, different
// numbers) is also a shape change for hot-reload purposes — the resting SL was
// armed against the old value at open.
func TestApplyHotReloadConfigRejectsSLAfterRegimeValueChangeWithOpenPosition(t *testing.T) {
	makeRef := func(ranging float64) *StrategyRef {
		return &StrategyRef{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"sl_after": map[string]interface{}{
					"trend_regime": map[string]interface{}{
						"trending_up":   map[string]interface{}{"atr_multiple": 0.25},
						"trending_down": map[string]interface{}{"atr_multiple": 0.25},
						"ranging":       map[string]interface{}{"atr_multiple": ranging},
					},
				},
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				},
			},
		}
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(0.0),
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(-0.5),
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject sl_after regime value change with open position")
	}
	if !strings.Contains(err.Error(), "sl_after rules changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// #736 — identical regime configs across reload should pass (no shape change).
// Guards against false-positive blocks once SLAfterRule.Equal handles regime
// blocks via RegimeATRBlock.EqualForReload.
func TestApplyHotReloadConfigAllowsSLAfterRegimeIdentical(t *testing.T) {
	tierRegime := &StrategyRef{
		Name: "tiered_tp_atr",
		Params: map[string]interface{}{
			"sl_after": map[string]interface{}{
				"trend_regime": map[string]interface{}{
					"trending_up":   map[string]interface{}{"atr_multiple": 0.25},
					"trending_down": map[string]interface{}{"atr_multiple": 0.25},
					"ranging":       map[string]interface{}{"atr_multiple": 0.0},
				},
			},
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		},
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierRegime,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: tierRegime,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("identical sl_after regime configs should reload cleanly with open position, got: %v", err)
	}
}

func TestApplyHotReloadConfigRejectsRegimeTierMultipleChangeWithTPATRFraction(t *testing.T) {
	makeRef := func(rangingATR float64) *StrategyRef {
		return &StrategyRef{
			Name: "tiered_tp_atr_regime",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"trend_regime": map[string]interface{}{
							"trending_up":   map[string]interface{}{"atr_multiple": 2.0},
							"trending_down": map[string]interface{}{"atr_multiple": 2.0},
							"ranging":       map[string]interface{}{"atr_multiple": rangingATR},
						},
						"close_fraction": 0.5,
						"sl_after": map[string]interface{}{
							"trail_from_here": map[string]interface{}{"tp_atr_fraction": 0.5},
						},
					},
					map[string]interface{}{
						"trend_regime": map[string]interface{}{
							"trending_up":   map[string]interface{}{"atr_multiple": 4.0},
							"trending_down": map[string]interface{}{"atr_multiple": 4.0},
							"ranging":       map[string]interface{}{"atr_multiple": 3.0},
						},
						"close_fraction": 1.0,
					},
				},
			},
		}
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(1.5),
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(2.5),
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", Regime: "ranging"},
		}},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected hot reload to reject regime tier multiple change with open position")
	}
	if !strings.Contains(err.Error(), "sl_after rules changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyHotReloadConfigAllowsRegimeTierMultipleChangeWithoutSLAfter(t *testing.T) {
	makeRef := func(rangingATR float64) *StrategyRef {
		return &StrategyRef{
			Name: "tiered_tp_atr_regime",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"trend_regime": map[string]interface{}{
							"trending_up":   map[string]interface{}{"atr_multiple": 2.0},
							"trending_down": map[string]interface{}{"atr_multiple": 2.0},
							"ranging":       map[string]interface{}{"atr_multiple": rangingATR},
						},
						"close_fraction": 0.5,
					},
					map[string]interface{}{
						"trend_regime": map[string]interface{}{
							"trending_up":   map[string]interface{}{"atr_multiple": 4.0},
							"trending_down": map[string]interface{}{"atr_multiple": 4.0},
							"ranging":       map[string]interface{}{"atr_multiple": 3.0},
						},
						"close_fraction": 1.0,
					},
				},
			},
		}
	}
	slMult := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(1.5),
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", StopLossATRMult: &slMult,
		CloseStrategy: makeRef(2.5),
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", Regime: "ranging"},
		}},
	}}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("tier changes without sl_after should not trip sl_after reload guard, got: %v", err)
	}
}

// #1062 — regime.display_windows is display-only and hot-reloads, but any other
// regime field change still requires a restart.
func TestApplyHotReloadConfigDisplayWindows(t *testing.T) {
	regimeWith := func(display []string) *RegimeConfig {
		return &RegimeConfig{
			Enabled: true, Period: 14, ADXThreshold: 20,
			Windows: RegimeWindowsMap{
				"long":           {Period: 2160},
				"composite_long": {Classifier: regimeClassifierComposite, Period: 2160},
			},
			DisplayWindows: display,
		}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", Regime: "ranging"},
			}},
		}}
	}
	stratWith := func(r *RegimeConfig) *Config {
		c := minimalReloadConfig([]StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated",
		}})
		c.Regime = r
		return c
	}

	// (1) display-only change applies while a position is open.
	t.Run("display-only change applies with open position", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		next := stratWith(regimeWith([]string{"composite_long"}))
		changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
		if err != nil {
			t.Fatalf("display-only regime change should hot-reload, got: %v", err)
		}
		if len(cfg.Regime.DisplayWindows) != 1 || cfg.Regime.DisplayWindows[0] != "composite_long" {
			t.Fatalf("DisplayWindows not applied: %v", cfg.Regime.DisplayWindows)
		}
		joined := strings.Join(changes, " | ")
		if !strings.Contains(joined, "regime.display_windows") {
			t.Fatalf("expected a display_windows change entry, got: %v", changes)
		}
	})

	// (2) compound change (display_windows + a real regime field) still rejects.
	t.Run("compound change still rejects", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		next := stratWith(regimeWith([]string{"composite_long"}))
		next.Regime.ADXThreshold = 25 // a genuinely restart-required edit
		if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err == nil {
			t.Fatal("regime change compounded with display_windows must still require restart")
		}
		if len(cfg.Regime.DisplayWindows) != 0 {
			t.Fatalf("rejected reload must not mutate DisplayWindows: %v", cfg.Regime.DisplayWindows)
		}
	})

	// (3) clearing display_windows reverts to render-all without a restart.
	t.Run("clearing reverts to render-all", func(t *testing.T) {
		cfg := stratWith(regimeWith([]string{"composite_long"}))
		next := stratWith(regimeWith(nil))
		if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
			t.Fatalf("clearing display_windows should hot-reload, got: %v", err)
		}
		if len(cfg.Regime.DisplayWindows) != 0 {
			t.Fatalf("DisplayWindows should be cleared, got: %v", cfg.Regime.DisplayWindows)
		}
	})
}

// #1224 — regime.transitions is alerting-only and documented as always
// hot-reloadable, including while positions are open. The pre-flight compat
// gate (regimeConfigEqualIgnoringReloadableFields) must mask this field the
// same way it masks DisplayWindows/Timeframe, or the copy branch in
// applyHotReloadConfig is unreachable dead code.
func TestApplyHotReloadConfigRegimeTransitions(t *testing.T) {
	regimeWith := func(tr *RegimeTransitionAlertsConfig) *RegimeConfig {
		return &RegimeConfig{
			Enabled: true, Period: 14, ADXThreshold: 20,
			Transitions: tr,
		}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", Regime: "ranging"},
			}},
		}}
	}
	stratWith := func(r *RegimeConfig) *Config {
		c := minimalReloadConfig([]StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated",
		}})
		c.Regime = r
		return c
	}

	// (a) nil on both sides must stay a no-op — existing behavior must keep working.
	t.Run("nil to nil is a no-op", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		next := stratWith(regimeWith(nil))
		changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
		if err != nil {
			t.Fatalf("nil transitions on both sides should hot-reload cleanly, got: %v", err)
		}
		if cfg.Regime.Transitions != nil {
			t.Fatalf("Transitions should remain nil, got: %+v", cfg.Regime.Transitions)
		}
		if joined := strings.Join(changes, " | "); strings.Contains(joined, "regime.transitions") {
			t.Fatalf("expected no regime.transitions change entry, got: %v", changes)
		}
	})

	// nil -> enabled must be accepted (not rejected by the pre-flight compat
	// gate) and copied onto cfg, even with an open position (alerting-only,
	// never state-shifting) — and the copy must not alias next's struct.
	t.Run("nil to enabled is accepted and copied", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		nextTransitions := &RegimeTransitionAlertsConfig{Enabled: true, DebounceCycles: 3, RetentionDays: 30, ReversalMinOpposing: 2}
		next := stratWith(regimeWith(nextTransitions))
		changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
		if err != nil {
			t.Fatalf("enabling regime.transitions should hot-reload even with an open position, got: %v", err)
		}
		if cfg.Regime.Transitions == nil || *cfg.Regime.Transitions != *nextTransitions {
			t.Fatalf("Transitions not applied: %+v", cfg.Regime.Transitions)
		}
		if cfg.Regime.Transitions == nextTransitions {
			t.Fatal("Transitions should be deep-copied, not aliased to next's struct")
		}
		if joined := strings.Join(changes, " | "); !strings.Contains(joined, "regime.transitions") {
			t.Fatalf("expected a regime.transitions change entry, got: %v", changes)
		}
		nextTransitions.DebounceCycles = 99 // mutating next afterward must not leak into cfg
		if cfg.Regime.Transitions.DebounceCycles == 99 {
			t.Fatal("cfg.Regime.Transitions aliases next's struct")
		}
	})

	// (b) feature already enabled; only debounce_cycles/retention_days/
	// reversal_min_opposing differ. Must hot-reload — even with an open
	// position — and the new tunables take effect.
	t.Run("tunable-only change while enabled applies with open position", func(t *testing.T) {
		cfg := stratWith(regimeWith(&RegimeTransitionAlertsConfig{Enabled: true, DebounceCycles: 1, RetentionDays: 14, ReversalMinOpposing: 0}))
		next := stratWith(regimeWith(&RegimeTransitionAlertsConfig{Enabled: true, DebounceCycles: 3, RetentionDays: 30, ReversalMinOpposing: 2}))
		changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
		if err != nil {
			t.Fatalf("tunable-only regime.transitions change should hot-reload, got: %v", err)
		}
		if cfg.Regime.Transitions.DebounceCycles != 3 || cfg.Regime.Transitions.RetentionDays != 30 || cfg.Regime.Transitions.ReversalMinOpposing != 2 {
			t.Fatalf("Transitions tunables not applied: %+v", cfg.Regime.Transitions)
		}
		if joined := strings.Join(changes, " | "); !strings.Contains(joined, "regime.transitions") {
			t.Fatalf("expected a regime.transitions change entry, got: %v", changes)
		}
	})

	// (c) regime.transitions changing together with a genuinely incompatible
	// field (db_file) must still be rejected for the real reason — masking
	// Transitions alone must not let an unrelated restart-required change
	// silently pass.
	t.Run("compound change with genuinely incompatible field still rejects", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		next := stratWith(regimeWith(&RegimeTransitionAlertsConfig{Enabled: true}))
		next.DBFile = "scheduler/other.db"
		_, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
		if err == nil {
			t.Fatal("db_file change compounded with regime.transitions must still require restart")
		}
		if !strings.Contains(err.Error(), "db_file changed") {
			t.Fatalf("expected db_file rejection reason, got: %v", err)
		}
		if cfg.Regime.Transitions != nil {
			t.Fatalf("rejected reload must not mutate Transitions: %+v", cfg.Regime.Transitions)
		}
	})
}

// #1139 — regime.timeframe is live reloadable only while affected non-options
// strategies are flat. It changes the regime bundle/certification key, so open
// positions must preserve their original regime-timeframe interpretation.
func TestApplyHotReloadConfigRegimeTimeframe(t *testing.T) {
	regimeWith := func(tf string) *RegimeConfig {
		return &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, Timeframe: tf}
	}
	stratWith := func(r *RegimeConfig) *Config {
		c := minimalReloadConfig([]StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated",
		}})
		c.Regime = r
		return c
	}

	t.Run("applies while flat", func(t *testing.T) {
		cfg := stratWith(regimeWith(""))
		next := stratWith(regimeWith(" 1D "))
		state := &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
		}}

		changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
		if err != nil {
			t.Fatalf("flat regime.timeframe change should hot-reload, got: %v", err)
		}
		if cfg.Regime.Timeframe != "1d" {
			t.Fatalf("Timeframe = %q, want normalized 1d", cfg.Regime.Timeframe)
		}
		if joined := strings.Join(changes, " | "); !strings.Contains(joined, "regime.timeframe") {
			t.Fatalf("expected a regime.timeframe change entry, got: %v", changes)
		}
	})

	t.Run("rejects while open", func(t *testing.T) {
		cfg := stratWith(regimeWith(""))
		next := stratWith(regimeWith("1d"))
		state := &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
			}},
		}}

		_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
		if err == nil {
			t.Fatal("expected open-position regime.timeframe change to be rejected")
		}
		if !strings.Contains(err.Error(), "regime.timeframe changed with open positions") {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Regime.Timeframe != "" {
			t.Fatalf("rejected reload mutated Timeframe: %q", cfg.Regime.Timeframe)
		}
	})

	t.Run("options ignore open-position guard", func(t *testing.T) {
		cfg := minimalReloadConfig([]StrategyConfig{{
			ID: "deribit-theta", Type: "options", Platform: "deribit", Script: "shared_scripts/check_options.py",
			Args: []string{"theta_harvest", "BTC"}, Capital: 1000, MaxDrawdownPct: 10,
		}})
		cfg.Regime = regimeWith("")
		next := minimalReloadConfig([]StrategyConfig{{
			ID: "deribit-theta", Type: "options", Platform: "deribit", Script: "shared_scripts/check_options.py",
			Args: []string{"theta_harvest", "BTC"}, Capital: 1000, MaxDrawdownPct: 10,
		}})
		next.Regime = regimeWith("1d")
		state := &AppState{Strategies: map[string]*StrategyState{
			"deribit-theta": {ID: "deribit-theta", Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 1000, Side: "long"},
			}},
		}}

		if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
			t.Fatalf("options path keeps its hardcoded regime timeframe and should not trip the open-position guard: %v", err)
		}
		if cfg.Regime.Timeframe != "1d" {
			t.Fatalf("Timeframe = %q, want 1d", cfg.Regime.Timeframe)
		}
	})
}

// #656 — direction change is allowed when the strategy is flat.
func TestApplyHotReloadConfigAllowsDirectionChangeWhenFlat(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionShort,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("expected direction change to be allowed when flat, got: %v", err)
	}
	if cfg.Strategies[0].Direction != DirectionShort {
		t.Errorf("Direction = %q, want %q after applied reload", cfg.Strategies[0].Direction, DirectionShort)
	}
}

func TestValidateHotReloadCompatible(t *testing.T) {
	baseStrategy := StrategyConfig{
		ID:             "spot-btc",
		Type:           "spot",
		Platform:       "binanceus",
		Script:         "shared_scripts/check_strategy.py",
		Args:           []string{"momentum", "BTC/USDT", "1h"},
		Capital:        1000,
		MaxDrawdownPct: 10,
	}

	rfr := 0.04
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"db_file changed", func(c *Config) { c.DBFile = "other.db" }, "db_file"},
		{"log_dir changed", func(c *Config) { c.LogDir = "newlogs" }, "log_dir"},
		{"status_port changed", func(c *Config) { c.StatusPort = 9090 }, "status_port"},
		{"status_token changed", func(c *Config) { c.StatusToken = "tok" }, "status token"},
		{"auto_update changed", func(c *Config) { c.AutoUpdate = "daily" }, "auto_update"},
		{"leaderboard_post_time changed", func(c *Config) { c.LeaderboardPostTime = "09:00" }, "leaderboard_post_time"},
		{"correlation changed", func(c *Config) {
			c.Correlation = &CorrelationConfig{Enabled: true}
		}, "correlation"},
		{"regime changed", func(c *Config) {
			c.Regime = &RegimeConfig{Enabled: true}
		}, "regime"},
		{"leaderboard_summaries changed", func(c *Config) {
			c.LeaderboardSummaries = []LeaderboardSummaryConfig{{Platform: "hyperliquid", Channel: "123"}}
		}, "leaderboard_summaries"},
		{"risk_free_rate changed", func(c *Config) { c.RiskFreeRate = &rfr }, "risk_free_rate"},
		{"tradingview_export changed", func(c *Config) {
			c.TradingViewExport = TradingViewExportConfig{SymbolOverrides: map[string]string{"BTC": "BTCUSD"}}
		}, "tradingview_export"},
		{"portfolio_risk max_notional changed", func(c *Config) {
			c.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60, MaxNotionalUSD: 10000}
		}, "max_notional"},
		{"discord.enabled changed", func(c *Config) { c.Discord.Enabled = true }, "discord.enabled"},
		{"discord.token changed", func(c *Config) { c.Discord.Token = "tok" }, "discord.token"},
		{"discord.owner_id changed", func(c *Config) { c.Discord.OwnerID = "999" }, "discord.owner_id"},
		{"telegram.enabled changed", func(c *Config) { c.Telegram.Enabled = true }, "telegram.enabled"},
		{"telegram.bot_token changed", func(c *Config) { c.Telegram.BotToken = "tok" }, "telegram.bot_token"},
		{"telegram.owner_chat_id changed", func(c *Config) { c.Telegram.OwnerChatID = "999" }, "telegram.owner_chat_id"},
		{"strategy set diverges", func(c *Config) { c.Strategies = nil }, "strategy set changed"},
		{"strategy shape changed", func(c *Config) {
			c.Strategies = []StrategyConfig{{
				ID:             "spot-btc",
				Type:           "spot",
				Platform:       "binanceus",
				Script:         "shared_scripts/check_strategy.py",
				Args:           []string{"ema_crossover", "BTC/USDT", "1h"}, // changed strategy name
				Capital:        1000,
				MaxDrawdownPct: 10,
			}}
		}, "non-hot-reloadable"},
		{"hl peer conflict in next", func(c *Config) {
			slPct := 5.0
			c.Strategies = []StrategyConfig{
				{
					ID: "hl-a-btc", Type: "perps", Platform: "hyperliquid",
					Script:  "shared_scripts/check_hyperliquid.py",
					Args:    []string{"momentum", "BTC", "1h", "--mode=live"},
					Capital: 1000, MaxDrawdownPct: 10,
					Leverage: 3, MarginMode: "isolated", StopLossPct: &slPct,
				},
				{
					ID: "hl-b-btc", Type: "perps", Platform: "hyperliquid",
					Script:  "shared_scripts/check_hyperliquid.py",
					Args:    []string{"triple_ema", "BTC", "1h", "--mode=live"},
					Capital: 1000, MaxDrawdownPct: 10,
					Leverage: 5, MarginMode: "isolated", // mismatched leverage
				},
			}
		}, "leverage"},
		{"identical configs returns nil", func(*Config) {}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalReloadConfig([]StrategyConfig{baseStrategy})
			next := minimalReloadConfig([]StrategyConfig{baseStrategy})
			tc.mutate(next)
			err := validateHotReloadCompatible(cfg, next)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.wantErr)
				} else if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestFormatFloatPtr(t *testing.T) {
	v1 := float64(3.14)
	v2 := float64(0)
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"nil", nil, "<nil>"},
		{"positive", &v1, "3.14"},
		{"zero", &v2, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFloatPtr(tc.in)
			if got != tc.want {
				t.Errorf("formatFloatPtr(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatFloatPtrUSD(t *testing.T) {
	v1 := float64(12.5)
	v2 := float64(0)
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"nil", nil, "<nil>"},
		{"positive", &v1, "$12.50"},
		{"zero", &v2, "$0.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFloatPtrUSD(tc.in)
			if got != tc.want {
				t.Errorf("formatFloatPtrUSD(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatFloatPtrPct(t *testing.T) {
	v1 := float64(12.5)
	v2 := float64(0)
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"nil", nil, "<nil>"},
		{"positive", &v1, "12.50%"},
		{"zero", &v2, "0.00%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFloatPtrPct(tc.in)
			if got != tc.want {
				t.Errorf("formatFloatPtrPct(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStateStrategy(t *testing.T) {
	ss := &StrategyState{}
	state := &AppState{Strategies: map[string]*StrategyState{"s1": ss}}

	t.Run("nil state", func(t *testing.T) {
		if stateStrategy(nil, "s1") != nil {
			t.Error("expected nil for nil state")
		}
	})
	t.Run("nil strategies map", func(t *testing.T) {
		if stateStrategy(&AppState{}, "s1") != nil {
			t.Error("expected nil for nil Strategies")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		if stateStrategy(state, "missing") != nil {
			t.Error("expected nil for missing key")
		}
	})
	t.Run("present key", func(t *testing.T) {
		if got := stateStrategy(state, "s1"); got != ss {
			t.Errorf("expected strategy state, got %v", got)
		}
	})
}

func TestStrategyHasOpenPositions(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if strategyHasOpenPositions(nil) {
			t.Error("expected false for nil")
		}
	})
	t.Run("empty maps", func(t *testing.T) {
		s := &StrategyState{
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
		}
		if strategyHasOpenPositions(s) {
			t.Error("expected false for empty maps")
		}
	})
	t.Run("position with zero qty", func(t *testing.T) {
		s := &StrategyState{
			Positions: map[string]*Position{"BTC": {Quantity: 0}},
		}
		if strategyHasOpenPositions(s) {
			t.Error("expected false for zero quantity")
		}
	})
	t.Run("position with positive qty", func(t *testing.T) {
		s := &StrategyState{
			Positions: map[string]*Position{"BTC": {Quantity: 1.0}},
		}
		if !strategyHasOpenPositions(s) {
			t.Error("expected true for positive quantity")
		}
	})
	t.Run("nil position entry skipped", func(t *testing.T) {
		s := &StrategyState{
			Positions: map[string]*Position{"BTC": nil},
		}
		if strategyHasOpenPositions(s) {
			t.Error("expected false for nil position entry")
		}
	})
	t.Run("option position with nonzero qty", func(t *testing.T) {
		s := &StrategyState{
			OptionPositions: map[string]*OptionPosition{"BTC-C": {Quantity: -2}},
		}
		if !strategyHasOpenPositions(s) {
			t.Error("expected true for nonzero option quantity")
		}
	})
	t.Run("option position with zero qty", func(t *testing.T) {
		s := &StrategyState{
			OptionPositions: map[string]*OptionPosition{"BTC-C": {Quantity: 0}},
		}
		if strategyHasOpenPositions(s) {
			t.Error("expected false for zero option quantity")
		}
	})
}

func TestPortfolioRiskMaxDrawdown(t *testing.T) {
	cases := []struct {
		name string
		in   *PortfolioRiskConfig
		want float64
	}{
		{"nil", nil, 0},
		{"populated", &PortfolioRiskConfig{MaxDrawdownPct: 15.5}, 15.5},
		{"zero value", &PortfolioRiskConfig{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := portfolioRiskMaxDrawdown(tc.in)
			if got != tc.want {
				t.Errorf("portfolioRiskMaxDrawdown(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPortfolioRiskWarnThreshold(t *testing.T) {
	cases := []struct {
		name string
		in   *PortfolioRiskConfig
		want float64
	}{
		{"nil", nil, 0},
		{"populated", &PortfolioRiskConfig{WarnThresholdPct: 60.0}, 60.0},
		{"zero value", &PortfolioRiskConfig{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := portfolioRiskWarnThreshold(tc.in)
			if got != tc.want {
				t.Errorf("portfolioRiskWarnThreshold(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestClonePortfolioRiskConfig(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		if clonePortfolioRiskConfig(nil) != nil {
			t.Error("expected nil")
		}
	})
	t.Run("populated returns independent copy", func(t *testing.T) {
		orig := &PortfolioRiskConfig{MaxDrawdownPct: 20, WarnThresholdPct: 60}
		got := clonePortfolioRiskConfig(orig)
		if got == orig {
			t.Error("expected a distinct pointer")
		}
		if got.MaxDrawdownPct != 20 || got.WarnThresholdPct != 60 {
			t.Errorf("clone values wrong: %+v", got)
		}
		got.MaxDrawdownPct = 99
		if orig.MaxDrawdownPct != 20 {
			t.Error("mutating clone affected original")
		}
	})
}

func TestFormatStringMap(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"nil", nil, "{}"},
		{"empty", map[string]string{}, "{}"},
		{"single", map[string]string{"a": "1"}, `{"a":"1"}`},
		{"multi sorted", map[string]string{"b": "2", "a": "1"}, `{"a":"1","b":"2"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStringMap(tc.in)
			if got != tc.want {
				t.Errorf("formatStringMap(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// #696/#1135: user_defaults.manual flows through hot-reload so SIGHUP edits
// to margin_usd / stop_loss_atr_mult / side / tp_tiers propagate without restart.
func TestApplyHotReloadConfigPropagatesManualDefaults(t *testing.T) {
	oldMargin := 50.0
	newMargin := 125.0
	newSL := 2.0
	cfg := minimalReloadConfig(nil)
	cfg.UserDefaults = &UserDefaultsConfig{
		Manual: &ManualDefaultsConfig{MarginUSD: &oldMargin, Side: "long"},
	}
	next := minimalReloadConfig(nil)
	next.UserDefaults = &UserDefaultsConfig{
		Manual: &ManualDefaultsConfig{
			MarginUSD:       &newMargin,
			StopLossATRMult: &newSL,
			Side:            "short",
			TPTiers: []ManualTPTier{
				{ATRMultiple: 1.5, CloseFraction: 0.4},
				{ATRMultiple: 2.5, CloseFraction: 1.0},
			},
		},
	}
	state := &AppState{Strategies: map[string]*StrategyState{}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "user_defaults") {
		t.Fatalf("changes missing user_defaults entry: %v", changes)
	}
	if cfg.UserDefaults == nil || cfg.UserDefaults.Manual == nil {
		t.Fatal("cfg.UserDefaults.Manual nil after reload")
	}
	if got := cfg.resolveManualMarginUSD(); got != 125.0 {
		t.Errorf("resolveManualMarginUSD = %g, want 125.0", got)
	}
	if got := cfg.resolveManualSide(); got != "short" {
		t.Errorf("resolveManualSide = %q, want %q", got, "short")
	}
	if got := cfg.resolveManualStopLossATRMult(); got != 2.0 {
		t.Errorf("resolveManualStopLossATRMult = %g, want 2.0", got)
	}
	if got := len(cfg.resolveManualTPTiers()); got != 2 {
		t.Errorf("resolveManualTPTiers length = %d, want 2", got)
	}
	// Mutating the next block after reload must not affect cfg (clone, not alias).
	*next.UserDefaults.Manual.MarginUSD = 999
	if got := cfg.resolveManualMarginUSD(); got != 125.0 {
		t.Errorf("cfg margin aliased to next: got %g after next-mutation, want 125.0", got)
	}
}

func TestApplyHotReloadConfigCopiesFlatRegimeTrailAndUserCloseDefaults(t *testing.T) {
	oldTrail := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.0},
		"trending_down": {ATR: 2.0},
		"ranging":       {ATR: 1.0},
	}}
	newTrail := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.75},
		"trending_down": {ATR: 2.75},
		"ranging":       {ATR: 1.5},
	}}
	strategy := func(block *RegimeATRBlock) StrategyConfig {
		return StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:                "shared_scripts/check_hyperliquid.py",
			Args:                  []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
			CloseStrategy:         &StrategyRef{Name: trailingTPRatchetRegimeCloseName},
			TrailingStopATRRegime: block,
			Capital:               1000,
			MaxDrawdownPct:        10,
			Leverage:              1,
		}
	}
	cfg := minimalReloadConfig([]StrategyConfig{strategy(oldTrail)})
	next := minimalReloadConfig([]StrategyConfig{strategy(newTrail)})
	next.UserDefaults = &UserDefaultsConfig{
		Close: CloseDefaultsMap{
			trailingTPRatchetRegimeCloseName: {
				"tp_tiers":                 ratchetRegimeUserTiers(),
				"trailing_stop_atr_regime": ratchetRegimeTrailRaw(2.75, 2.75, 1.5),
			},
		},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "trailing_stop_atr_regime") {
		t.Fatalf("changes missing trailing_stop_atr_regime update: %v", changes)
	}
	if !strings.Contains(joined, "user_defaults") {
		t.Fatalf("changes missing user_defaults update: %v", changes)
	}
	got, ok := resolveRegimeATR(*cfg.Strategies[0].TrailingStopATRRegime, "ranging")
	if !ok || got != 1.5 {
		t.Fatalf("reloaded ranging trail = (%g, %v), want (1.5, true)", got, ok)
	}
	next.Strategies[0].TrailingStopATRRegime.TrendRegime["ranging"] = RegimeATREntry{ATR: 9.0}
	got, ok = resolveRegimeATR(*cfg.Strategies[0].TrailingStopATRRegime, "ranging")
	if !ok || got != 1.5 {
		t.Fatalf("reloaded trail aliases next after mutation: (%g, %v)", got, ok)
	}
	next.UserDefaults.Close[trailingTPRatchetRegimeCloseName]["trailing_stop_atr_regime"] = map[string]interface{}{"use_defaults": true}
	raw := cfg.UserDefaults.Close[trailingTPRatchetRegimeCloseName]["trailing_stop_atr_regime"].(map[string]interface{})
	if _, ok := raw["use_defaults"]; ok {
		t.Fatal("cfg.UserDefaults.Close aliases next after reload")
	}
}

func TestApplyHotReloadConfigRejectsUserCloseDefaultRegimeTrailChangeWithOpenPosition(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		strategy string
	}{
		{
			name: "perps",
			id:   "hl-eth",
			strategy: `{
				"id": "hl-eth",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 1,
				"max_drawdown_pct": 20,
				"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": true}}
			}`,
		},
		{
			name: "manual",
			id:   "hl-manual-eth",
			strategy: `{
				"id": "hl-manual-eth",
				"type": "manual",
				"platform": "hyperliquid",
				"symbol": "ETH",
				"timeframe": "1h",
				"capital": 1000,
				"leverage": 1,
				"max_drawdown_pct": 20
			}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadUserDefaultRatchetRegimeReloadConfig(t, tc.strategy, explicitUserDefaultTrailJSON(2.5, 2.5, 2.0))
			next := loadUserDefaultRatchetRegimeReloadConfig(t, tc.strategy, explicitUserDefaultTrailJSON(2.5, 2.5, 1.5))

			_, err := applyHotReloadConfig(cfg, next, openETHReloadState(tc.id), nil, nil)
			if err == nil {
				t.Fatal("expected open-position reload to reject changed user_defaults.close trailing_stop_atr_regime")
			}
			if !strings.Contains(err.Error(), "trailing_stop_atr_regime shape changed with open positions") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestApplyHotReloadConfigAllowsUserCloseDefaultRegimeTrailEquivalentEditWithOpenPosition(t *testing.T) {
	strategy := `{
		"id": "hl-eth",
		"type": "perps",
		"platform": "hyperliquid",
		"script": "shared_scripts/check_hyperliquid.py",
		"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
		"capital": 1000,
		"leverage": 1,
		"max_drawdown_pct": 20,
		"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": true}}
	}`
	cfg := loadUserDefaultRatchetRegimeReloadConfig(t, strategy, explicitUserDefaultTrailJSON(2.5, 2.5, 2.0))
	next := loadUserDefaultRatchetRegimeReloadConfig(t, strategy, `{"use_defaults": true}`)

	changes, err := applyHotReloadConfig(cfg, next, openETHReloadState("hl-eth"), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig rejected equivalent effective trail: %v", err)
	}
	if cfg.Strategies[0].TrailingStopATRRegime == nil || !cfg.Strategies[0].TrailingStopATRRegime.UseDefaults {
		t.Fatalf("equivalent trail edit was not copied into cfg: %#v", cfg.Strategies[0].TrailingStopATRRegime)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "trailing_stop_atr_regime") || !strings.Contains(joined, "user_defaults") {
		t.Fatalf("changes=%v, want trailing_stop_atr_regime and user_defaults entries", changes)
	}
}

func TestApplyHotReloadConfigCopiesFlatStandaloneRegimeATRDefault(t *testing.T) {
	cfg := loadUserDefaultStandaloneRegimeATRReloadConfig(t, explicitUserDefaultStopLossJSON(2.0, 2.0, 1.5))
	next := loadUserDefaultStandaloneRegimeATRReloadConfig(t, explicitUserDefaultStopLossJSON(2.25, 2.25, 1.25))
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "user_defaults") || !strings.Contains(joined, "stop_loss_atr_regime") {
		t.Fatalf("changes=%v, want user_defaults and stop_loss_atr_regime entries", changes)
	}
	got, ok := resolveRegimeATR(*cfg.Strategies[0].StopLossATRRegime, "ranging")
	if !ok || got != 1.25 {
		t.Fatalf("reloaded ranging SL = (%g, %v), want (1.25, true)", got, ok)
	}
	next.Strategies[0].StopLossATRRegime.TrendRegime["ranging"] = RegimeATREntry{ATR: 9.0}
	got, ok = resolveRegimeATR(*cfg.Strategies[0].StopLossATRRegime, "ranging")
	if !ok || got != 1.25 {
		t.Fatalf("reloaded standalone SL aliases next after mutation: (%g, %v)", got, ok)
	}
}

func loadUserDefaultRatchetRegimeReloadConfig(t *testing.T, strategyJSON, trailJSON string) *Config {
	t.Helper()
	cfgJSON := fmt.Sprintf(`{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_defaults": {
			"close": {
				"trailing_tp_ratchet_regime": {
					"tp_tiers": {
						"trending_up": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
						"trending_down": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
						"ranging": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
					},
					"trailing_stop_atr_regime": %s
				}
			}
		},
		"strategies": [%s]
	}`, trailJSON, strategyJSON)
	cfg, err := LoadConfig(writeTestConfig(t, t.TempDir(), cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func loadUserDefaultStandaloneRegimeATRReloadConfig(t *testing.T, slJSON string) *Config {
	t.Helper()
	cfgJSON := fmt.Sprintf(`{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": %s
			}
		},
		"strategies": [{
			"id": "hl-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 1,
			"max_drawdown_pct": 20,
			"stop_loss_atr_regime": {"use_defaults": true}
		}]
	}`, slJSON)
	cfg, err := LoadConfig(writeTestConfig(t, t.TempDir(), cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func explicitUserDefaultStopLossJSON(up, down, ranging float64) string {
	return fmt.Sprintf(`{
		"trend_regime": {
			"trending_up": {"atr_multiple": %g},
			"trending_down": {"atr_multiple": %g},
			"ranging": {"atr_multiple": %g}
		}
	}`, up, down, ranging)
}

func explicitUserDefaultTrailJSON(up, down, ranging float64) string {
	return fmt.Sprintf(`{
		"trend_regime": {
			"trending_up": {"atr_multiple": %g},
			"trending_down": {"atr_multiple": %g},
			"ranging": {"atr_multiple": %g}
		}
	}`, up, down, ranging)
}

func openETHReloadState(strategyID string) *AppState {
	return &AppState{Strategies: map[string]*StrategyState{
		strategyID: {ID: strategyID, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}
}

// #696/#1135: empty tp_tiers array is rejected by validation; LoadConfig surfaces
// the misuse instead of silently falling back to defaults.
func TestLoadConfigManualDefaultsRejectsEmptyTPTiersArray(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_defaults": {"manual": {"tp_tiers": []}},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted empty user_defaults.manual.tp_tiers array")
	}
	if !strings.Contains(err.Error(), "tp_tiers") {
		t.Errorf("error %q does not mention tp_tiers", err)
	}
}

func TestStrategyRestartShape_RegimeWindowOnlyChange(t *testing.T) {
	a := StrategyConfig{ID: "hl-a", RegimeGateWindow: "short", RegimeATRWindow: "medium"}
	b := StrategyConfig{ID: "hl-a", RegimeGateWindow: "long", RegimeATRWindow: "short"}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("regime_*_window-only change should not affect restart shape")
	}
}

func TestValidateHotReloadCompatible_RegimeWindowOnlyChange(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID:               "hl-a",
		Type:             "perps",
		Platform:         "hyperliquid",
		Script:           "shared_scripts/check_hyperliquid.py",
		Args:             []string{"momentum", "BTC", "1h", "--mode=paper"},
		Capital:          1000,
		MaxDrawdownPct:   10,
		RegimeGateWindow: "short",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID:               "hl-a",
		Type:             "perps",
		Platform:         "hyperliquid",
		Script:           "shared_scripts/check_hyperliquid.py",
		Args:             []string{"momentum", "BTC", "1h", "--mode=paper"},
		Capital:          1000,
		MaxDrawdownPct:   10,
		RegimeGateWindow: "medium",
	}})
	if err := validateHotReloadCompatible(cfg, next); err != nil {
		t.Fatalf("pure regime_gate_window change should be hot-reloadable: %v", err)
	}
}

// #1048: the circuit-breaker toggle is hot-reloadable always, including while a
// position is open — it must NOT be rejected by the reload validators, and the
// new value must actually be applied to the running config.
func TestApplyHotReloadConfig_CircuitBreakerToggleWhileOpen(t *testing.T) {
	falseVal, trueVal := false, true
	base := func(cb *bool) []StrategyConfig {
		return []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
			CircuitBreaker: cb,
		}}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 10},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
				},
			},
		}}
	}

	// on (nil) → off while a position is open: accepted, value applied, change logged.
	cfg := minimalReloadConfig(base(nil))
	next := minimalReloadConfig(base(&falseVal))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("circuit_breaker on->off while open should be hot-reloadable: %v", err)
	}
	if cfg.Strategies[0].CircuitBreakerEnabled() {
		t.Fatal("expected circuit breaker disabled after reload")
	}
	if !strings.Contains(strings.Join(changes, "\n"), "circuit_breaker") {
		t.Fatalf("expected a circuit_breaker change entry, got %v", changes)
	}

	// off → on (re-arm) while a position is open: accepted, value applied.
	cfg = minimalReloadConfig(base(&falseVal))
	next = minimalReloadConfig(base(&trueVal))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("circuit_breaker off->on while open should be hot-reloadable: %v", err)
	}
	if !cfg.Strategies[0].CircuitBreakerEnabled() {
		t.Fatal("expected circuit breaker re-enabled after reload")
	}
}

// #1273: the cb_* timing/threshold overrides are hot-reloadable while a
// position is open (they only parameterize FUTURE fires), values land on the
// running config, change entries are logged, and clearing back to nil restores
// the accessors' historical defaults.
func TestApplyHotReloadConfig_CBOverridesWhileOpen(t *testing.T) {
	intp := func(v int) *int { return &v }
	base := func(mut func(*StrategyConfig)) []StrategyConfig {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
		}
		if mut != nil {
			mut(&sc)
		}
		return []StrategyConfig{sc}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 10},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
				},
			},
		}}
	}
	setOverrides := func(sc *StrategyConfig) {
		sc.CBDrawdownCooldownMinutes = intp(720)
		sc.CBLossStreakThreshold = intp(3)
		sc.CBLossStreakCooldownMinutes = intp(30)
	}

	// defaults → overrides while a position is open: accepted, applied, logged.
	cfg := minimalReloadConfig(base(nil))
	next := minimalReloadConfig(base(setOverrides))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("cb_* overrides while open should be hot-reloadable: %v", err)
	}
	sc := &cfg.Strategies[0]
	if got := sc.CircuitBreakerDrawdownCooldown(); got != 12*time.Hour {
		t.Fatalf("drawdown cooldown after reload = %v, want 12h", got)
	}
	if got := sc.CircuitBreakerLossStreakThreshold(); got != 3 {
		t.Fatalf("loss-streak threshold after reload = %d, want 3", got)
	}
	if got := sc.CircuitBreakerLossStreakCooldown(); got != 30*time.Minute {
		t.Fatalf("loss-streak cooldown after reload = %v, want 30m", got)
	}
	joined := strings.Join(changes, "\n")
	for _, want := range []string{"cb_drawdown_cooldown_minutes", "cb_loss_streak_threshold", "cb_loss_streak_cooldown_minutes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected a %s change entry, got %v", want, changes)
		}
	}

	// overrides → defaults (fields removed) while open: accepted, accessors
	// fall back to the historical values.
	cfg = minimalReloadConfig(base(setOverrides))
	next = minimalReloadConfig(base(nil))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("clearing cb_* overrides while open should be hot-reloadable: %v", err)
	}
	sc = &cfg.Strategies[0]
	if sc.CircuitBreakerDrawdownCooldown() != 24*time.Hour || sc.CircuitBreakerLossStreakThreshold() != 5 || sc.CircuitBreakerLossStreakCooldown() != time.Hour {
		t.Fatal("cleared overrides should fall back to the historical defaults")
	}
}

// #1273: a cb_*-override-only change must not register in the restart shape
// (else validateHotReloadCompatible would flag it as restart-required).
func TestStrategyRestartShape_CBOverrideOnlyChange(t *testing.T) {
	dd, th, lc := 720, 3, 30
	a := StrategyConfig{ID: "hl-a"}
	b := StrategyConfig{ID: "hl-a", CBDrawdownCooldownMinutes: &dd, CBLossStreakThreshold: &th, CBLossStreakCooldownMinutes: &lc}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("cb_* override set-vs-nil should not affect restart shape")
	}
}

// #1048: a circuit_breaker-only change must not register in the restart shape
// (else validateHotReloadCompatible would flag it as restart-required).
func TestStrategyRestartShape_CircuitBreakerOnlyChange(t *testing.T) {
	on, off := true, false
	a := StrategyConfig{ID: "hl-a", CircuitBreaker: &on}
	b := StrategyConfig{ID: "hl-a", CircuitBreaker: &off}
	c := StrategyConfig{ID: "hl-a", CircuitBreaker: nil}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("circuit_breaker on/off change should not affect restart shape")
	}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(c)) {
		t.Fatal("circuit_breaker set-vs-nil should not affect restart shape")
	}
}

// #1118: per-strategy notify_ratchet_triggers is notification-only, so a change
// must hot-reload even while a position is open (accepted, applied, logged).
func TestApplyHotReloadConfig_NotifyRatchetTriggersWhileOpen(t *testing.T) {
	falseVal, trueVal := false, true
	base := func(nrt *bool) []StrategyConfig {
		return []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
			NotifyRatchetTriggers: nrt,
		}}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 10},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
				},
			},
		}}
	}

	// inherit-global (nil) → off while open: accepted, value applied, change logged.
	cfg := minimalReloadConfig(base(nil))
	next := minimalReloadConfig(base(&falseVal))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("notify_ratchet_triggers nil->off while open should be hot-reloadable: %v", err)
	}
	if cfg.Strategies[0].NotifyRatchetTriggers == nil || *cfg.Strategies[0].NotifyRatchetTriggers {
		t.Fatal("expected notify_ratchet_triggers=false after reload")
	}
	if !strings.Contains(strings.Join(changes, "\n"), "notify_ratchet_triggers") {
		t.Fatalf("expected a notify_ratchet_triggers change entry, got %v", changes)
	}

	// off → on while open: accepted, value applied.
	cfg = minimalReloadConfig(base(&falseVal))
	next = minimalReloadConfig(base(&trueVal))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("notify_ratchet_triggers off->on while open should be hot-reloadable: %v", err)
	}
	if cfg.Strategies[0].NotifyRatchetTriggers == nil || !*cfg.Strategies[0].NotifyRatchetTriggers {
		t.Fatal("expected notify_ratchet_triggers=true after reload")
	}
}

// #1118: a notify_ratchet_triggers-only change must not register in the restart
// shape (else validateHotReloadCompatible would flag it as restart-required).
func TestStrategyRestartShape_NotifyRatchetTriggersOnlyChange(t *testing.T) {
	on, off := true, false
	a := StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: &on}
	b := StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: &off}
	c := StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: nil}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("notify_ratchet_triggers on/off change should not affect restart shape")
	}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(c)) {
		t.Fatal("notify_ratchet_triggers set-vs-nil should not affect restart shape")
	}
}

// TestValidateHotReloadStateCompatible_StopOwnerModeToggles pins the #1234
// audit invariant class: toggling ANY Hyperliquid stop-loss owner on or off
// (nil<->positive scalars, nil<->configured regime blocks, scalar<->regime
// swaps) is blocked while the strategy holds an open position — the resting
// on-chain trigger was sized for one distance regime — and allowed while flat.
func TestValidateHotReloadStateCompatible_StopOwnerModeToggles(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	mkCfg := func(mutate func(sc *StrategyConfig)) *Config {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated",
		}
		if mutate != nil {
			mutate(&sc)
		}
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	regimeBlock := func(sc *StrategyConfig, trailing bool) {
		b := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}
		if trailing {
			sc.TrailingStopATRRegime = b
		} else {
			sc.StopLossATRRegime = b
		}
	}

	cases := []struct {
		name     string
		old, new func(sc *StrategyConfig)
		wantErr  string
	}{
		{"trailing_stop_pct removed (positive->nil)",
			func(sc *StrategyConfig) { sc.TrailingStopPct = pf(3) }, nil,
			"trailing_stop_pct mode changed"},
		{"trailing_stop_atr_mult added (nil->positive)",
			nil, func(sc *StrategyConfig) { sc.TrailingStopATRMult = pf(2) },
			"trailing_stop_atr_mult mode changed"},
		{"trailing_stop_atr_mult removed (positive->nil)",
			func(sc *StrategyConfig) { sc.TrailingStopATRMult = pf(2) }, nil,
			"trailing_stop_atr_mult mode changed"},
		{"stop_loss_atr_mult added (nil->positive)",
			nil, func(sc *StrategyConfig) { sc.StopLossATRMult = pf(2) },
			"stop_loss_atr_mult mode changed"},
		{"stop_loss_atr_mult removed (positive->nil)",
			func(sc *StrategyConfig) { sc.StopLossATRMult = pf(2) }, nil,
			"stop_loss_atr_mult mode changed"},
		{"scalar->regime swap (stop_loss_atr_mult -> stop_loss_atr_regime)",
			func(sc *StrategyConfig) { sc.StopLossATRMult = pf(2) },
			func(sc *StrategyConfig) { regimeBlock(sc, false) },
			"stop_loss_atr_regime mode changed"},
		{"trailing_stop_atr_regime added (nil->configured)",
			nil, func(sc *StrategyConfig) { regimeBlock(sc, true) },
			"trailing_stop_atr_regime mode changed"},
		{"trailing_stop_atr_regime removed (configured->nil)",
			func(sc *StrategyConfig) { regimeBlock(sc, true) }, nil,
			"trailing_stop_atr_regime mode changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHotReloadStateCompatible(mkCfg(tc.old), mkCfg(tc.new), openState)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("open position: want error containing %q, got: %v", tc.wantErr, err)
			}
			// Inverse: the same owner toggle while FLAT must be accepted.
			if err := validateHotReloadStateCompatible(mkCfg(tc.old), mkCfg(tc.new), flatState); err != nil {
				t.Fatalf("flat: same toggle must be accepted, got: %v", err)
			}
		})
	}
}

func TestValidateHotReloadStateCompatible_BlocksHedgeChangeWhileAnyLegOpen(t *testing.T) {
	old := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"hold", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5},
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"hold", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1},
	}})
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.02, AvgCost: 100000, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
		}},
	}}
	if err := validateHotReloadStateCompatible(old, next, openState); err == nil || !strings.Contains(err.Error(), "hedge changed with open positions") {
		t.Fatalf("open hedge leg: want hedge compatibility error, got %v", err)
	}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	if err := validateHotReloadStateCompatible(old, next, flatState); err != nil {
		t.Fatalf("flat hedge change should be accepted: %v", err)
	}
}
