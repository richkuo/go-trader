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

func hlReloadStrategy(mut func(*StrategyConfig)) StrategyConfig {
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated",
	}
	if mut != nil {
		mut(&sc)
	}
	return sc
}

func hlReloadConfig(mut func(*StrategyConfig)) *Config {
	return minimalReloadConfig([]StrategyConfig{hlReloadStrategy(mut)})
}

func flatETHReloadState() *AppState {
	return &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}
}

func tieredATRRef(params map[string]interface{}) *StrategyRef {
	return &StrategyRef{Name: "tiered_tp_atr", Params: params}
}

func TestApplyHotReloadConfigOpenPositionGuards(t *testing.T) {
	spotStrategy := func(script string, capital float64) StrategyConfig {
		return StrategyConfig{ID: "s1", Type: "spot", Platform: "binanceus", Script: script, Args: []string{"a"}, Capital: capital, MaxDrawdownPct: 10}
	}
	tiers := func(entries ...map[string]interface{}) []interface{} {
		out := make([]interface{}, 0, len(entries))
		for _, e := range entries {
			out = append(out, e)
		}
		return out
	}
	twoTiers := func() []interface{} {
		return tiers(
			map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
			map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
		)
	}
	oneTier := func() []interface{} {
		return tiers(map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0})
	}
	slAfterRegime := func(ranging float64) map[string]interface{} {
		return map[string]interface{}{
			"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr_multiple": 0.25},
				"trending_down": map[string]interface{}{"atr_multiple": 0.25},
				"ranging":       map[string]interface{}{"atr_multiple": ranging},
			},
		}
	}
	withSL := func(params map[string]interface{}) func(*StrategyConfig) {
		return func(sc *StrategyConfig) {
			sc.StopLossATRMult = floatPtr(1.5)
			sc.CloseStrategy = tieredATRRef(params)
		}
	}
	regimeTierRef := func(rangingATR float64, slAfter bool) func(*StrategyConfig) {
		tier0 := map[string]interface{}{
			"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr_multiple": 2.0},
				"trending_down": map[string]interface{}{"atr_multiple": 2.0},
				"ranging":       map[string]interface{}{"atr_multiple": rangingATR},
			},
			"close_fraction": 0.5,
		}
		if slAfter {
			tier0["sl_after"] = map[string]interface{}{
				"trail_from_here": map[string]interface{}{"tp_atr_fraction": 0.5},
			}
		}
		return func(sc *StrategyConfig) {
			sc.StopLossATRMult = floatPtr(1.5)
			sc.CloseStrategy = &StrategyRef{
				Name: "tiered_tp_atr_regime",
				Params: map[string]interface{}{
					"tp_tiers": []interface{}{
						tier0,
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
	}
	openRangingState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", Regime: "ranging"},
			}},
		}}
	}
	openLeveredState := func() *AppState {
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
	openETH := func() *AppState { return openETHReloadState("hl-eth") }

	cases := []struct {
		name    string
		cfg     func() *Config
		next    func() *Config
		state   func() *AppState
		wantErr string
		check   func(t *testing.T, cfg *Config, state *AppState, changes []string)
	}{
		{
			name: "rejects strategy set change",
			cfg:  func() *Config { return minimalReloadConfig([]StrategyConfig{spotStrategy("x.py", 100)}) },
			next: func() *Config {
				return minimalReloadConfig([]StrategyConfig{spotStrategy("x.py", 200), {ID: "s2", Type: "spot", Platform: "binanceus", Script: "x.py", Capital: 100, MaxDrawdownPct: 10}})
			},
			state:   NewAppState,
			wantErr: "strategy set changed",
		},
		{
			name:    "rejects non-hot-reloadable strategy field (script)",
			cfg:     func() *Config { return minimalReloadConfig([]StrategyConfig{spotStrategy("x.py", 100)}) },
			next:    func() *Config { return minimalReloadConfig([]StrategyConfig{spotStrategy("y.py", 200)}) },
			state:   NewAppState,
			wantErr: "non-hot-reloadable",
		},
		{
			name: "allows open/close strategy ref changes",
			cfg: func() *Config {
				return minimalReloadConfig([]StrategyConfig{{ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Args: []string{"triple_ema", "BTC/USDT", "1h"}, Capital: 100, MaxDrawdownPct: 10}})
			},
			next: func() *Config {
				return minimalReloadConfig([]StrategyConfig{{ID: "s1", Type: "spot", Platform: "binanceus", Script: "x.py", Args: []string{"triple_ema", "BTC/USDT", "1h"}, Capital: 100, MaxDrawdownPct: 10,
					OpenStrategy: StrategyRef{Name: "triple_ema"}, CloseStrategy: &StrategyRef{Name: "tp_at_pct"}}})
			},
			state: NewAppState,
			check: func(t *testing.T, cfg *Config, _ *AppState, changes []string) {
				joined := strings.Join(changes, "\n")
				for _, want := range []string{"strategy[s1].open_strategy:", "strategy[s1].close_strategy:"} {
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
			},
		},
		{
			name: "rejects leverage change with open perps position",
			cfg: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = "" })
			},
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) {
					sc.Leverage = 5
					sc.MarginMode = ""
					sc.Capital = 1200
					sc.MaxDrawdownPct = 12
				})
			},
			state:   openLeveredState,
			wantErr: "leverage changed with open positions",
		},
		{
			name: "rejects margin_mode change with open perps position",
			cfg:  func() *Config { return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2 }) },
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = "cross" })
			},
			state:   openLeveredState,
			wantErr: "margin_mode changed with open positions",
		},
		{
			name: "allows margin_mode change when flat",
			cfg:  func() *Config { return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2 }) },
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = "cross" })
			},
			state: flatETHReloadState,
			check: func(t *testing.T, cfg *Config, _ *AppState, _ []string) {
				if cfg.Strategies[0].MarginMode != "cross" {
					t.Fatalf("MarginMode = %q, want %q", cfg.Strategies[0].MarginMode, "cross")
				}
			},
		},
		{
			name: "preserves runtime capital_pct capital while applying other fields",
			cfg: func() *Config {
				return minimalReloadConfig([]StrategyConfig{{ID: "s1", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "BTC", "1h"}, Capital: 2500, CapitalPct: 0.5, MaxDrawdownPct: 10, Leverage: 2}})
			},
			next: func() *Config {
				return minimalReloadConfig([]StrategyConfig{{ID: "s1", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"a", "BTC", "1h"}, Capital: 100, CapitalPct: 0.5, MaxDrawdownPct: 12, Leverage: 2}})
			},
			state: func() *AppState {
				return &AppState{Strategies: map[string]*StrategyState{"s1": {ID: "s1", Cash: 2400, RiskState: RiskState{MaxDrawdownPct: 10}}}}
			},
			check: func(t *testing.T, cfg *Config, state *AppState, changes []string) {
				if cfg.Strategies[0].Capital != 2500 {
					t.Errorf("runtime capital_pct capital = %g, want preserved 2500", cfg.Strategies[0].Capital)
				}
				if state.Strategies["s1"].Cash != 2400 {
					t.Errorf("cash = %g, want preserved 2400", state.Strategies["s1"].Cash)
				}
				if joined := strings.Join(changes, "\n"); strings.Contains(joined, ".capital:") {
					t.Fatalf("capital_pct fallback capital should not be hot-applied, changes:\n%s", joined)
				}
				if cfg.Strategies[0].MaxDrawdownPct != 12 || state.Strategies["s1"].RiskState.MaxDrawdownPct != 12 {
					t.Fatalf("other hot-reloadable fields should still apply, cfg=%+v state=%+v", cfg.Strategies[0], state.Strategies["s1"].RiskState)
				}
			},
		},
		{
			name: "rejects HL peer leverage mismatch in next",
			cfg: func() *Config {
				return minimalReloadConfig([]StrategyConfig{
					hlReloadStrategy(func(sc *StrategyConfig) { sc.ID = "hl-eth-a" }),
					hlReloadStrategy(func(sc *StrategyConfig) { sc.ID = "hl-eth-b"; sc.Args = []string{"b", "ETH", "1h"}; sc.Capital = 500 }),
				})
			},
			next: func() *Config {
				return minimalReloadConfig([]StrategyConfig{
					hlReloadStrategy(func(sc *StrategyConfig) { sc.ID = "hl-eth-a" }),
					hlReloadStrategy(func(sc *StrategyConfig) {
						sc.ID = "hl-eth-b"
						sc.Args = []string{"b", "ETH", "1h"}
						sc.Capital = 500
						sc.Leverage = 10
					}),
				})
			},
			state: func() *AppState {
				return &AppState{Strategies: map[string]*StrategyState{
					"hl-eth-a": {ID: "hl-eth-a", Cash: 1000},
					"hl-eth-b": {ID: "hl-eth-b", Cash: 500},
				}}
			},
			wantErr: "disagree on leverage",
		},
		{
			name: "allows trailing_stop_pct value change with open position",
			cfg: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.TrailingStopPct = floatPtr(3); sc.TrailingStopMinMovePct = floatPtr(0.5) })
			},
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.TrailingStopPct = floatPtr(4); sc.TrailingStopMinMovePct = floatPtr(0.25) })
			},
			state: openETH,
			check: func(t *testing.T, cfg *Config, _ *AppState, changes []string) {
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
			},
		},
		{
			name:    "rejects fixed-to-trailing stop mode switch with open position",
			cfg:     func() *Config { return hlReloadConfig(nil) },
			next:    func() *Config { return hlReloadConfig(func(sc *StrategyConfig) { sc.TrailingStopPct = floatPtr(3) }) },
			state:   openETH,
			wantErr: "trailing_stop_pct mode changed",
		},
		{
			name: "rejects direction change with open perps position",
			cfg: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = ""; sc.Direction = DirectionLong })
			},
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = ""; sc.Direction = DirectionShort })
			},
			state:   openLeveredState,
			wantErr: "direction changed with open positions",
		},
		{
			name: "allows direction change when flat",
			cfg: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = ""; sc.Direction = DirectionLong })
			},
			next: func() *Config {
				return hlReloadConfig(func(sc *StrategyConfig) { sc.Leverage = 2; sc.MarginMode = ""; sc.Direction = DirectionShort })
			},
			state: flatETHReloadState,
			check: func(t *testing.T, cfg *Config, _ *AppState, _ []string) {
				if cfg.Strategies[0].Direction != DirectionShort {
					t.Errorf("Direction = %q, want %q after applied reload", cfg.Strategies[0].Direction, DirectionShort)
				}
			},
		},
		{
			name: "rejects sl_after add with open position",
			cfg:  func() *Config { return hlReloadConfig(withSL(map[string]interface{}{"tp_tiers": twoTiers()})) },
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": "breakeven", "tp_tiers": twoTiers()}))
			},
			state:   openETH,
			wantErr: "sl_after rules changed with open positions",
		},
		{
			name: "allows sl_after add when flat",
			cfg:  func() *Config { return hlReloadConfig(withSL(map[string]interface{}{"tp_tiers": oneTier()})) },
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": "breakeven", "tp_tiers": oneTier()}))
			},
			state: flatETHReloadState,
		},
		{
			name: "rejects sl_after mode switch (breakeven -> trail_from_here) with open position",
			cfg: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": "breakeven", "tp_tiers": oneTier()}))
			},
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{
					"sl_after": map[string]interface{}{
						"kind":            "trail_from_here",
						"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
					},
					"tp_tiers": oneTier(),
				}))
			},
			state:   openETH,
			wantErr: "sl_after rules changed with open positions",
		},
		{
			name: "rejects sl_after scalar -> regime shape change with open position",
			cfg: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": map[string]interface{}{"atr_mult": 0.25}, "tp_tiers": oneTier()}))
			},
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": slAfterRegime(0.0), "tp_tiers": oneTier()}))
			},
			state:   openETH,
			wantErr: "sl_after rules changed with open positions",
		},
		{
			name: "rejects sl_after regime value change with open position",
			cfg: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": slAfterRegime(0.0), "tp_tiers": oneTier()}))
			},
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": slAfterRegime(-0.5), "tp_tiers": oneTier()}))
			},
			state:   openETH,
			wantErr: "sl_after rules changed with open positions",
		},
		{
			name: "allows identical sl_after regime block with open position",
			cfg: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": slAfterRegime(0.0), "tp_tiers": oneTier()}))
			},
			next: func() *Config {
				return hlReloadConfig(withSL(map[string]interface{}{"sl_after": slAfterRegime(0.0), "tp_tiers": oneTier()}))
			},
			state: openETH,
		},
		{
			name:    "rejects regime tier multiple change when sl_after uses tp_atr_fraction",
			cfg:     func() *Config { return hlReloadConfig(regimeTierRef(1.5, true)) },
			next:    func() *Config { return hlReloadConfig(regimeTierRef(2.5, true)) },
			state:   openRangingState,
			wantErr: "sl_after rules changed with open positions",
		},
		{
			name:  "allows regime tier multiple change without sl_after",
			cfg:   func() *Config { return hlReloadConfig(regimeTierRef(1.5, false)) },
			next:  func() *Config { return hlReloadConfig(regimeTierRef(2.5, false)) },
			state: openRangingState,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, state := tc.cfg(), tc.state()
			changes, err := applyHotReloadConfig(cfg, tc.next(), state, nil, nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected reload to be rejected with %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := tc.cfg(); !reflect.DeepEqual(cfg, want) {
					t.Fatalf("current config mutated after rejected reload:\n got %+v\nwant %+v", cfg.Strategies, want.Strategies)
				}
				if want := tc.state(); !reflect.DeepEqual(state, want) {
					t.Fatalf("state mutated after rejected reload:\n got %+v\nwant %+v", state.Strategies, want.Strategies)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyHotReloadConfig: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg, state, changes)
			}
		})
	}
}

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

	t.Run("compound change still rejects", func(t *testing.T) {
		cfg := stratWith(regimeWith(nil))
		next := stratWith(regimeWith([]string{"composite_long"}))
		next.Regime.ADXThreshold = 25
		if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err == nil {
			t.Fatal("regime change compounded with display_windows must still require restart")
		}
		if len(cfg.Regime.DisplayWindows) != 0 {
			t.Fatalf("rejected reload must not mutate DisplayWindows: %v", cfg.Regime.DisplayWindows)
		}
	})

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
		nextTransitions.DebounceCycles = 99
		if cfg.Regime.Transitions.DebounceCycles == 99 {
			t.Fatal("cfg.Regime.Transitions aliases next's struct")
		}
	})

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
				Args:           []string{"ema_crossover", "BTC/USDT", "1h"},
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
					Leverage: 5, MarginMode: "isolated",
				},
			}
		}, "leverage"},
		{"regime_gate_window only change returns nil", func(c *Config) { c.Strategies[0].RegimeGateWindow = "medium" }, ""},
		{"shared-wallet pool budgeting mode change requires restart", func(c *Config) { c.Strategies[0].sharedWalletPoolBudget = true }, "shared-wallet pool budgeting mode changed"},
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

func TestPortfolioRiskAccessorsNilSafe(t *testing.T) {
	cases := []struct {
		name     string
		in       *PortfolioRiskConfig
		wantDD   float64
		wantWarn float64
	}{
		{"nil", nil, 0, 0},
		{"populated", &PortfolioRiskConfig{MaxDrawdownPct: 15.5, WarnThresholdPct: 60.0}, 15.5, 60.0},
		{"zero value", &PortfolioRiskConfig{}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := portfolioRiskMaxDrawdown(tc.in); got != tc.wantDD {
				t.Errorf("portfolioRiskMaxDrawdown(%v) = %v, want %v", tc.in, got, tc.wantDD)
			}
			if got := portfolioRiskWarnThreshold(tc.in); got != tc.wantWarn {
				t.Errorf("portfolioRiskWarnThreshold(%v) = %v, want %v", tc.in, got, tc.wantWarn)
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
			Script:                    "shared_scripts/check_hyperliquid.py",
			Args:                      []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
			CloseStrategy:             &StrategyRef{Name: trailingTPRatchetRegimeCloseName},
			TrailingStopATRMultRegime: block,
			Capital:                   1000,
			MaxDrawdownPct:            10,
			Leverage:                  1,
		}
	}
	cfg := minimalReloadConfig([]StrategyConfig{strategy(oldTrail)})
	next := minimalReloadConfig([]StrategyConfig{strategy(newTrail)})
	next.UserDefaults = &UserDefaultsConfig{
		Close: CloseDefaultsMap{
			trailingTPRatchetRegimeCloseName: {
				"tp_tiers":                      ratchetRegimeUserTiers(),
				"trailing_stop_atr_mult_regime": ratchetRegimeTrailRaw(2.75, 2.75, 1.5),
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
	if !strings.Contains(joined, "trailing_stop_atr_mult_regime") {
		t.Fatalf("changes missing trailing_stop_atr_mult_regime update: %v", changes)
	}
	if !strings.Contains(joined, "user_defaults") {
		t.Fatalf("changes missing user_defaults update: %v", changes)
	}
	got, ok := resolveRegimeATR(*cfg.Strategies[0].TrailingStopATRMultRegime, "ranging")
	if !ok || got != 1.5 {
		t.Fatalf("reloaded ranging trail = (%g, %v), want (1.5, true)", got, ok)
	}
	next.Strategies[0].TrailingStopATRMultRegime.TrendRegime["ranging"] = RegimeATREntry{ATR: 9.0}
	got, ok = resolveRegimeATR(*cfg.Strategies[0].TrailingStopATRMultRegime, "ranging")
	if !ok || got != 1.5 {
		t.Fatalf("reloaded trail aliases next after mutation: (%g, %v)", got, ok)
	}
	next.UserDefaults.Close[trailingTPRatchetRegimeCloseName]["trailing_stop_atr_mult_regime"] = map[string]interface{}{"use_defaults": true}
	raw := cfg.UserDefaults.Close[trailingTPRatchetRegimeCloseName]["trailing_stop_atr_mult_regime"].(map[string]interface{})
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
				t.Fatal("expected open-position reload to reject changed user_defaults.close trailing_stop_atr_mult_regime")
			}
			if !strings.Contains(err.Error(), "trailing_stop_atr_mult_regime shape changed with open positions") {
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
	if cfg.Strategies[0].TrailingStopATRMultRegime == nil || !cfg.Strategies[0].TrailingStopATRMultRegime.UseDefaults {
		t.Fatalf("equivalent trail edit was not copied into cfg: %#v", cfg.Strategies[0].TrailingStopATRMultRegime)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "trailing_stop_atr_mult_regime") || !strings.Contains(joined, "user_defaults") {
		t.Fatalf("changes=%v, want trailing_stop_atr_mult_regime and user_defaults entries", changes)
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
	if !strings.Contains(joined, "user_defaults") || !strings.Contains(joined, "stop_loss_atr_mult_regime") {
		t.Fatalf("changes=%v, want user_defaults and stop_loss_atr_mult_regime entries", changes)
	}
	got, ok := resolveRegimeATR(*cfg.Strategies[0].StopLossATRMultRegime, "ranging")
	if !ok || got != 1.25 {
		t.Fatalf("reloaded ranging SL = (%g, %v), want (1.25, true)", got, ok)
	}
	next.Strategies[0].StopLossATRMultRegime.TrendRegime["ranging"] = RegimeATREntry{ATR: 9.0}
	got, ok = resolveRegimeATR(*cfg.Strategies[0].StopLossATRMultRegime, "ranging")
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
					"trailing_stop_atr_mult_regime": %s
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
				"stop_loss_atr_mult_regime": %s
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
			"stop_loss_atr_mult_regime": {"use_defaults": true}
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

func TestApplyHotReloadConfigPreservesDeferredPoolTransitionManageOnlyLatch(t *testing.T) {
	strategy := StrategyConfig{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid",
		Script:     "shared_scripts/check_hyperliquid.py",
		Args:       []string{"momentum", "BTC", "1h", "--mode=live"},
		CapitalPct: 0.5, MaxDrawdownPct: 10,
		Paused: true, sharedWalletModeDeferred: true,
	}
	cfg := minimalReloadConfig([]StrategyConfig{strategy})
	nextStrategy := strategy
	nextStrategy.Paused = false
	nextStrategy.sharedWalletModeDeferred = false
	next := minimalReloadConfig([]StrategyConfig{nextStrategy})

	if _, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil); err != nil {
		t.Fatalf("unrelated reload must not be rejected by process-local deferred latch: %v", err)
	}
	got := cfg.Strategies[0]
	if !got.Paused || !got.sharedWalletModeDeferred {
		t.Fatalf("SIGHUP must not resume a deferred pool transition before restart: %+v", got)
	}
}

func TestStrategyRestartShapeIgnoresHotReloadableFields(t *testing.T) {
	on, off := true, false
	dd, th, lc := 720, 3, 30
	cases := []struct {
		name string
		a, b StrategyConfig
	}{
		{"regime_*_window only change", StrategyConfig{ID: "hl-a", RegimeGateWindow: "short", RegimeATRWindow: "medium"}, StrategyConfig{ID: "hl-a", RegimeGateWindow: "long", RegimeATRWindow: "short"}},
		{"cb_* override set-vs-nil", StrategyConfig{ID: "hl-a"}, StrategyConfig{ID: "hl-a", CBDrawdownCooldownMinutes: &dd, CBLossStreakThreshold: &th, CBLossStreakCooldownMinutes: &lc}},
		{"circuit_breaker on/off", StrategyConfig{ID: "hl-a", CircuitBreaker: &on}, StrategyConfig{ID: "hl-a", CircuitBreaker: &off}},
		{"circuit_breaker set-vs-nil", StrategyConfig{ID: "hl-a", CircuitBreaker: &on}, StrategyConfig{ID: "hl-a", CircuitBreaker: nil}},
		{"notify_ratchet_triggers on/off", StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: &on}, StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: &off}},
		{"notify_ratchet_triggers set-vs-nil", StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: &on}, StrategyConfig{ID: "hl-a", NotifyRatchetTriggers: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(strategyRestartShape(tc.a), strategyRestartShape(tc.b)) {
				t.Fatalf("%s should not affect restart shape", tc.name)
			}
		})
	}
}

func TestApplyHotReloadConfigTogglesWhileOpen(t *testing.T) {
	falseVal, trueVal := false, true
	intp := func(v int) *int { return &v }
	base := func(mut func(*StrategyConfig)) *Config {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
		}
		if mut != nil {
			mut(&sc)
		}
		return minimalReloadConfig([]StrategyConfig{sc})
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
	cases := []struct {
		name        string
		old, new    func(*StrategyConfig)
		wantChanges []string
		check       func(t *testing.T, sc *StrategyConfig)
	}{
		{
			name: "circuit_breaker nil->off",
			old:  nil, new: func(sc *StrategyConfig) { sc.CircuitBreaker = &falseVal },
			wantChanges: []string{"circuit_breaker"},
			check: func(t *testing.T, sc *StrategyConfig) {
				if sc.CircuitBreakerEnabled() {
					t.Fatal("expected circuit breaker disabled after reload")
				}
			},
		},
		{
			name: "circuit_breaker off->on",
			old:  func(sc *StrategyConfig) { sc.CircuitBreaker = &falseVal },
			new:  func(sc *StrategyConfig) { sc.CircuitBreaker = &trueVal },
			check: func(t *testing.T, sc *StrategyConfig) {
				if !sc.CircuitBreakerEnabled() {
					t.Fatal("expected circuit breaker re-enabled after reload")
				}
			},
		},
		{
			name: "cb_* overrides set",
			old:  nil, new: setOverrides,
			wantChanges: []string{"cb_drawdown_cooldown_minutes", "cb_loss_streak_threshold", "cb_loss_streak_cooldown_minutes"},
			check: func(t *testing.T, sc *StrategyConfig) {
				if got := sc.CircuitBreakerDrawdownCooldown(); got != 12*time.Hour {
					t.Fatalf("drawdown cooldown after reload = %v, want 12h", got)
				}
				if got := sc.CircuitBreakerLossStreakThreshold(); got != 3 {
					t.Fatalf("loss-streak threshold after reload = %d, want 3", got)
				}
				if got := sc.CircuitBreakerLossStreakCooldown(); got != 30*time.Minute {
					t.Fatalf("loss-streak cooldown after reload = %v, want 30m", got)
				}
			},
		},
		{
			name: "cb_* overrides cleared fall back to historical defaults",
			old:  setOverrides, new: nil,
			check: func(t *testing.T, sc *StrategyConfig) {
				if sc.CircuitBreakerDrawdownCooldown() != 24*time.Hour || sc.CircuitBreakerLossStreakThreshold() != 5 || sc.CircuitBreakerLossStreakCooldown() != time.Hour {
					t.Fatal("cleared overrides should fall back to the historical defaults")
				}
			},
		},
		{
			name: "notify_ratchet_triggers nil->off",
			old:  nil, new: func(sc *StrategyConfig) { sc.NotifyRatchetTriggers = &falseVal },
			wantChanges: []string{"notify_ratchet_triggers"},
			check: func(t *testing.T, sc *StrategyConfig) {
				if sc.NotifyRatchetTriggers == nil || *sc.NotifyRatchetTriggers {
					t.Fatal("expected notify_ratchet_triggers=false after reload")
				}
			},
		},
		{
			name: "notify_ratchet_triggers off->on",
			old:  func(sc *StrategyConfig) { sc.NotifyRatchetTriggers = &falseVal },
			new:  func(sc *StrategyConfig) { sc.NotifyRatchetTriggers = &trueVal },
			check: func(t *testing.T, sc *StrategyConfig) {
				if sc.NotifyRatchetTriggers == nil || !*sc.NotifyRatchetTriggers {
					t.Fatal("expected notify_ratchet_triggers=true after reload")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base(tc.old)
			changes, err := applyHotReloadConfig(cfg, base(tc.new), openState(), nil, nil)
			if err != nil {
				t.Fatalf("%s while open should be hot-reloadable: %v", tc.name, err)
			}
			joined := strings.Join(changes, "\n")
			for _, want := range tc.wantChanges {
				if !strings.Contains(joined, want) {
					t.Fatalf("expected a %s change entry, got %v", want, changes)
				}
			}
			tc.check(t, &cfg.Strategies[0])
		})
	}
}

func TestValidateHotReloadStateCompatible_StopOwnerModeToggles(t *testing.T) {
	pf := floatPtr
	mkCfg := hlReloadConfig
	openState := openETHReloadState("hl-eth")
	flatState := flatETHReloadState()
	regimeBlock := func(sc *StrategyConfig, trailing bool) {
		b := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}
		if trailing {
			sc.TrailingStopATRMultRegime = b
		} else {
			sc.StopLossATRMultRegime = b
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
		{"scalar->regime swap (stop_loss_atr_mult -> stop_loss_atr_mult_regime)",
			func(sc *StrategyConfig) { sc.StopLossATRMult = pf(2) },
			func(sc *StrategyConfig) { regimeBlock(sc, false) },
			"stop_loss_atr_mult_regime mode changed"},
		{"trailing_stop_atr_mult_regime added (nil->configured)",
			nil, func(sc *StrategyConfig) { regimeBlock(sc, true) },
			"trailing_stop_atr_mult_regime mode changed"},
		{"trailing_stop_atr_mult_regime removed (configured->nil)",
			func(sc *StrategyConfig) { regimeBlock(sc, true) }, nil,
			"trailing_stop_atr_mult_regime mode changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHotReloadStateCompatible(mkCfg(tc.old), mkCfg(tc.new), openState)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("open position: want error containing %q, got: %v", tc.wantErr, err)
			}
			if err := validateHotReloadStateCompatible(mkCfg(tc.old), mkCfg(tc.new), flatState); err != nil {
				t.Fatalf("flat: same toggle must be accepted, got: %v", err)
			}
		})
	}
}

func TestFormatFloatPtrVariants(t *testing.T) {
	v1 := float64(12.5)
	v2 := float64(0)
	v3 := float64(3.14)
	cases := []struct {
		name string
		fn   func(*float64) string
		in   *float64
		want string
	}{
		{"plain nil", formatFloatPtr, nil, "<nil>"},
		{"plain positive", formatFloatPtr, &v3, "3.14"},
		{"plain zero", formatFloatPtr, &v2, "0"},
		{"usd nil", formatFloatPtrUSD, nil, "<nil>"},
		{"usd positive", formatFloatPtrUSD, &v1, "$12.50"},
		{"usd zero", formatFloatPtrUSD, &v2, "$0.00"},
		{"pct nil", formatFloatPtrPct, nil, "<nil>"},
		{"pct positive", formatFloatPtrPct, &v1, "12.50%"},
		{"pct zero", formatFloatPtrPct, &v2, "0.00%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
