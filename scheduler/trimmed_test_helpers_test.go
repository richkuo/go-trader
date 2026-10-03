package main

import (
	"io"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func silentStrategyLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: io.Discard}
}

func runScopeCycle(t *testing.T, cfg *Config, state *AppState, peaks map[RiskPartition]float64) map[RiskPartition]*scopeCycleRisk {
	t.Helper()
	out := make(map[RiskPartition]*scopeCycleRisk)
	now := time.Now().UTC()
	for _, part := range activePartitions(cfg.Strategies) {
		sr := measureScopeCycleRisk(part, partitionRiskConfig(cfg, part), cfg.Strategies, state, nil, nil, nil, true, false, now)
		prs := state.partitionRisk(part)
		if peak, ok := peaks[part]; ok {
			prs.PeakValue = peak
		}
		applyScopeCycleRisk(sr, prs)
		out[part] = sr
	}
	return out
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func newLedgerTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func withAlertThrottleInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := alertThrottleInterval
	alertThrottleInterval = d
	t.Cleanup(func() { alertThrottleInterval = prev })
}

func hlLivePerps(id, symbol string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Platform: "hyperliquid", Type: "perps", Args: []string{"sma", symbol, "1h", "--mode=live"}, Capital: capital}
}

func hlLiveManual(id string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: capital}
}

func splitTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		DBFile:      filepath.Join(dir, "live.db"),
		PaperDBFile: filepath.Join(dir, "paper.db"),
		Strategies: []StrategyConfig{
			{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}},
			{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}},
		},
	}
}

func openSplitStore(t *testing.T, cfg *Config) *StateStore {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func splitTestState(t *testing.T) *AppState {
	t.Helper()
	state := NewAppState()
	state.Strategies["hl-live"] = &StrategyState{
		ID: "hl-live", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
	}
	state.Strategies["hl-paper"] = &StrategyState{
		ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Cash: 2000, InitialCapital: 2000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
	}
	return state
}

const minimalConfigJSON = `{
  "config_version": 15,
  "interval_seconds": 300,
  "log_dir": "logs",
  "db_file": "scheduler/state.db",
  "auto_update": "off",
  "discord": {"enabled": false, "token": "discord-secret", "channels": {}, "leaderboard_top_n": 5},
  "telegram": {"enabled": false, "bot_token": "tg-secret", "channels": {}},
  "strategies": [
    {"id": "sma-btc", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py", "args": ["sma_crossover", "BTC/USDT", "1h"], "capital": 1000, "max_drawdown_pct": 5},
    {"id": "hl-momentum-eth", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["momentum", "ETH", "1h", "--mode=paper"], "capital": 100, "max_drawdown_pct": 10, "leverage": 1, "direction": "long", "margin_mode": "isolated"}
  ]
}`

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

func tieredTPCloseStrategy() *StrategyRef {
	return &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
			map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.8},
			map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
		},
	}}
}
