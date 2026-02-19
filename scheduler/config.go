package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// DiscordChannels holds channel IDs for different report types.
type DiscordChannels struct {
	Spot    string `json:"spot"`
	Options string `json:"options"`
}

// DiscordConfig holds Discord notification settings.
type DiscordConfig struct {
	Enabled  bool            `json:"enabled"`
	Token    string          `json:"token"`
	Channels DiscordChannels `json:"channels"`
}

// Config is the top-level scheduler configuration.
type Config struct {
	IntervalSeconds int              `json:"interval_seconds"`
	LogDir          string           `json:"log_dir"`
	StateFile       string           `json:"state_file"`
	Discord         DiscordConfig    `json:"discord"`
	Strategies      []StrategyConfig `json:"strategies"`
}

// ThetaHarvestConfig controls early exit on sold options.
type ThetaHarvestConfig struct {
	Enabled         bool    `json:"enabled"`
	ProfitTargetPct float64 `json:"profit_target_pct"` // Close sold options when this % of premium captured (e.g. 60)
	StopLossPct     float64 `json:"stop_loss_pct"`     // Close if loss exceeds this % of premium (e.g. 200 = 2x premium)
	MinDTEClose     float64 `json:"min_dte_close"`     // Force-close positions with fewer than N days to expiry
}

// StrategyConfig describes a single strategy job.
type StrategyConfig struct {
	ID              string              `json:"id"`
	Type            string              `json:"type"` // "spot" or "options"
	Script          string              `json:"script"`
	Args            []string            `json:"args"`
	Capital         float64             `json:"capital"`
	MaxDrawdownPct  float64             `json:"max_drawdown_pct"`
	IntervalSeconds int                 `json:"interval_seconds,omitempty"` // per-strategy override (0 = use global)
	ThetaHarvest    *ThetaHarvestConfig `json:"theta_harvest,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 600
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "logs"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "scheduler/state.json"
	}

	// Discord token from env var takes priority over config file.
	// Warn if token is present in config file (env var is preferred).
	configHasToken := cfg.Discord.Token != ""
	envToken := os.Getenv("DISCORD_BOT_TOKEN")
	if envToken != "" {
		if configHasToken {
			fmt.Println("[WARN] Discord token found in both config file and DISCORD_BOT_TOKEN env var. Remove it from config.json to avoid accidental exposure.")
		}
		cfg.Discord.Token = envToken
	} else if configHasToken {
		fmt.Println("[WARN] Discord token found in config file. Prefer setting DISCORD_BOT_TOKEN env var instead.")
	}

	return &cfg, nil
}
