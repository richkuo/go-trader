package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the top-level scheduler configuration.
type Config struct {
	IntervalSeconds int              `json:"interval_seconds"`
	LogDir          string           `json:"log_dir"`
	StateFile       string           `json:"state_file"`
	Strategies      []StrategyConfig `json:"strategies"`
}

// StrategyConfig describes a single strategy job.
type StrategyConfig struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"` // "spot" or "options"
	Script         string   `json:"script"`
	Args           []string `json:"args"`
	Capital        float64  `json:"capital"`
	MaxDrawdownPct float64  `json:"max_drawdown_pct"`
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
	return &cfg, nil
}
