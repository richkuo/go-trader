package main

import (
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

func TestApplyHotReloadConfigAllowsScaleInChangeWhenFlat(t *testing.T) {
	cap := 5000.0
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
		AllowScaleIn: true, ScaleInMaxPositionNotionalUSD: &cap,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("expected scale-in settings to reload while flat, got: %v", err)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "strategy[hl-eth].allow_scale_in: false -> true") ||
		!strings.Contains(joined, "strategy[hl-eth].scale_in_max_position_notional_usd: <nil> -> $5000.00") {
		t.Fatalf("scale-in changes missing:\n%s", joined)
	}
	if !cfg.Strategies[0].AllowScaleIn || cfg.Strategies[0].ScaleInMaxPositionNotionalUSD == nil || *cfg.Strategies[0].ScaleInMaxPositionNotionalUSD != cap {
		t.Fatalf("scale-in settings not applied: %+v", cfg.Strategies[0])
	}
}

func TestApplyHotReloadConfigRejectsScaleInChangeWithOpenPerpsPosition(t *testing.T) {
	cap := 5000.0
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, MarginMode: "isolated",
		AllowScaleIn: true, ScaleInMaxPositionNotionalUSD: &cap,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Cash: 900,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
			},
		},
	}}

	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil {
		t.Fatal("expected scale-in settings to be rejected while open")
	}
	if !strings.Contains(err.Error(), "allow_scale_in changed with open positions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Strategies[0].AllowScaleIn || cfg.Strategies[0].ScaleInMaxPositionNotionalUSD != nil {
		t.Fatalf("current config mutated after rejected reload: %+v", cfg.Strategies[0])
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

// #696: manual_defaults flows through hot-reload so SIGHUP edits to
// margin_usd / stop_loss_atr_mult / side / tp_tiers propagate without restart.
func TestApplyHotReloadConfigPropagatesManualDefaults(t *testing.T) {
	oldMargin := 50.0
	newMargin := 125.0
	newSL := 2.0
	cfg := minimalReloadConfig(nil)
	cfg.ManualDefaults = &ManualDefaultsConfig{MarginUSD: &oldMargin, Side: "long"}
	next := minimalReloadConfig(nil)
	next.ManualDefaults = &ManualDefaultsConfig{
		MarginUSD:       &newMargin,
		StopLossATRMult: &newSL,
		Side:            "short",
		TPTiers: []ManualTPTier{
			{ATRMultiple: 1.5, CloseFraction: 0.4},
			{ATRMultiple: 2.5, CloseFraction: 1.0},
		},
	}
	state := &AppState{Strategies: map[string]*StrategyState{}}

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "manual_defaults") {
		t.Fatalf("changes missing manual_defaults entry: %v", changes)
	}
	if cfg.ManualDefaults == nil {
		t.Fatal("cfg.ManualDefaults nil after reload")
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
	*next.ManualDefaults.MarginUSD = 999
	if got := cfg.resolveManualMarginUSD(); got != 125.0 {
		t.Errorf("cfg margin aliased to next: got %g after next-mutation, want 125.0", got)
	}
}

// #696: empty tp_tiers array is rejected by validation; LoadConfig surfaces
// the misuse instead of silently falling back to defaults.
func TestLoadConfigManualDefaultsRejectsEmptyTPTiersArray(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"manual_defaults": {"tp_tiers": []},
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
		t.Fatal("LoadConfig accepted empty manual_defaults.tp_tiers array")
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
