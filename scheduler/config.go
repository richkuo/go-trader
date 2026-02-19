package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ValidateConfig checks script paths and strategy fields (#34, #36).
func ValidateConfig(cfg *Config) error {
	var errs []string
	seenIDs := make(map[string]bool)

	for i, sc := range cfg.Strategies {
		prefix := fmt.Sprintf("strategy[%d]", i)

		// ID must be non-empty and unique.
		if sc.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is empty", prefix))
		} else if seenIDs[sc.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate id %q", prefix, sc.ID))
		} else {
			seenIDs[sc.ID] = true
			prefix = fmt.Sprintf("strategy[%s]", sc.ID)
		}

		// #34: Script path validation.
		if sc.Script == "" {
			errs = append(errs, fmt.Sprintf("%s: script is empty", prefix))
		} else {
			if filepath.IsAbs(sc.Script) {
				errs = append(errs, fmt.Sprintf("%s: script must be a relative path, got %q", prefix, sc.Script))
			}
			if !strings.HasSuffix(sc.Script, ".py") {
				errs = append(errs, fmt.Sprintf("%s: script must end with .py, got %q", prefix, sc.Script))
			}
			if strings.HasPrefix(filepath.Clean(sc.Script), "..") {
				errs = append(errs, fmt.Sprintf("%s: script path escapes working directory: %q", prefix, sc.Script))
			}
		}

		// #36: Type must be "spot" or "options".
		if sc.Type != "spot" && sc.Type != "options" {
			errs = append(errs, fmt.Sprintf("%s: type must be \"spot\" or \"options\", got %q", prefix, sc.Type))
		}

		// #36: Capital must be > 0.
		if sc.Capital <= 0 {
			errs = append(errs, fmt.Sprintf("%s: capital must be > 0, got %g", prefix, sc.Capital))
		}

		// #36: MaxDrawdownPct must be in (0, 100].
		if sc.MaxDrawdownPct <= 0 || sc.MaxDrawdownPct > 100 {
			errs = append(errs, fmt.Sprintf("%s: max_drawdown_pct must be in (0, 100], got %g", prefix, sc.MaxDrawdownPct))
		}

		// #36: IntervalSeconds must be >= 0 (0 means use global).
		if sc.IntervalSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s: interval_seconds must be >= 0, got %d", prefix, sc.IntervalSeconds))
		}

		// #36: ThetaHarvest fields must be non-negative when present.
		if sc.ThetaHarvest != nil {
			th := sc.ThetaHarvest
			if th.ProfitTargetPct < 0 {
				errs = append(errs, fmt.Sprintf("%s: theta_harvest.profit_target_pct must be >= 0", prefix))
			}
			if th.StopLossPct < 0 {
				errs = append(errs, fmt.Sprintf("%s: theta_harvest.stop_loss_pct must be >= 0", prefix))
			}
			if th.MinDTEClose < 0 {
				errs = append(errs, fmt.Sprintf("%s: theta_harvest.min_dte_close must be >= 0", prefix))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
