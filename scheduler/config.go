package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DiscordConfig holds Discord notification settings.
type DiscordConfig struct {
	Enabled  bool              `json:"enabled"`
	Token    string            `json:"token"`
	Channels map[string]string `json:"channels"` // keyed by platform or type ("spot", "hyperliquid", "deribit", etc.)
}

// PortfolioRiskConfig controls aggregate portfolio-level risk (#42).
type PortfolioRiskConfig struct {
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // kill switch threshold (default 25)
	MaxNotionalUSD float64 `json:"max_notional_usd"` // 0 = disabled
}

// PlatformConfig holds per-platform settings (state file path and optional risk overrides).
type PlatformConfig struct {
	StateFile string               `json:"state_file"`     // e.g. "platforms/deribit/state.json"
	Risk      *PortfolioRiskConfig `json:"risk,omitempty"` // overrides portfolio-level defaults
}

// Config is the top-level scheduler configuration.
type Config struct {
	IntervalSeconds int                        `json:"interval_seconds"`
	LogDir          string                     `json:"log_dir"`
	StateFile       string                     `json:"state_file"`
	StatusToken     string                     `json:"-"` // loaded from STATUS_AUTH_TOKEN env var only
	Discord         DiscordConfig              `json:"discord"`
	AutoUpdate      string                     `json:"auto_update,omitempty"` // "off", "daily", "heartbeat" (default: "off")
	Strategies      []StrategyConfig           `json:"strategies"`
	PortfolioRisk   *PortfolioRiskConfig       `json:"portfolio_risk,omitempty"`
	Platforms       map[string]*PlatformConfig `json:"platforms,omitempty"`
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
	Type            string              `json:"type"`     // "spot" or "options"
	Platform        string              `json:"platform"` // "deribit", "ibkr", "binanceus", etc.
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
	if cfg.AutoUpdate == "" {
		cfg.AutoUpdate = "off"
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

	// Optional auth token for the /status HTTP endpoint.
	cfg.StatusToken = os.Getenv("STATUS_AUTH_TOKEN")

	// Initialize platforms map.
	if cfg.Platforms == nil {
		cfg.Platforms = make(map[string]*PlatformConfig)
	}
	// Apply default state file paths for any declared platforms.
	for name, pc := range cfg.Platforms {
		if pc.StateFile == "" {
			pc.StateFile = fmt.Sprintf("platforms/%s/state.json", name)
		}
	}

	// Apply per-strategy defaults.
	for i := range cfg.Strategies {
		// Infer platform from ID prefix for backwards compatibility.
		if cfg.Strategies[i].Platform == "" {
			switch {
			case strings.HasPrefix(cfg.Strategies[i].ID, "ibkr-"):
				cfg.Strategies[i].Platform = "ibkr"
			case strings.HasPrefix(cfg.Strategies[i].ID, "deribit-"):
				cfg.Strategies[i].Platform = "deribit"
			case strings.HasPrefix(cfg.Strategies[i].ID, "hl-"):
				cfg.Strategies[i].Platform = "hyperliquid"
			case cfg.Strategies[i].Type == "options":
				cfg.Strategies[i].Platform = "deribit"
			default:
				cfg.Strategies[i].Platform = "binanceus"
			}
		}

		// Hierarchical risk: strategy-specific > platform > type default.
		if cfg.Strategies[i].MaxDrawdownPct == 0 {
			platform := cfg.Strategies[i].Platform
			if pc := cfg.Platforms[platform]; pc != nil && pc.Risk != nil && pc.Risk.MaxDrawdownPct > 0 {
				cfg.Strategies[i].MaxDrawdownPct = pc.Risk.MaxDrawdownPct
			} else if cfg.Strategies[i].Type == "options" {
				cfg.Strategies[i].MaxDrawdownPct = 40 // options are volatile
			} else if cfg.Strategies[i].Type == "perps" {
				cfg.Strategies[i].MaxDrawdownPct = 50 // perps: between spot (60) and options (40)
			} else {
				cfg.Strategies[i].MaxDrawdownPct = 60
			}
		}

		// #56: Default theta harvest for options strategies — sold options
		// must always have an automatic exit to prevent unbounded losses.
		if cfg.Strategies[i].Type == "options" && cfg.Strategies[i].ThetaHarvest == nil {
			cfg.Strategies[i].ThetaHarvest = &ThetaHarvestConfig{
				Enabled:         true,
				ProfitTargetPct: 60,
				StopLossPct:     200,
				MinDTEClose:     3,
			}
			fmt.Printf("[INFO] %s: no theta_harvest config, applying defaults (profit=60%%, stop=200%%, dte=3)\n", cfg.Strategies[i].ID)
		}
	}

	// #42: Apply portfolio risk defaults if not configured.
	if cfg.PortfolioRisk == nil {
		cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25}
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

		// #36: Type must be "spot", "options", or "perps".
		if sc.Type != "spot" && sc.Type != "options" && sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: type must be \"spot\", \"options\", or \"perps\", got %q", prefix, sc.Type))
		}

		// Live-mode perps require HYPERLIQUID_SECRET_KEY env var.
		if sc.Type == "perps" {
			for _, arg := range sc.Args {
				if arg == "--mode=live" {
					if os.Getenv("HYPERLIQUID_SECRET_KEY") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires HYPERLIQUID_SECRET_KEY env var", prefix))
					}
					break
				}
			}
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

	// #42: Validate portfolio risk config.
	if cfg.PortfolioRisk != nil {
		if cfg.PortfolioRisk.MaxDrawdownPct <= 0 || cfg.PortfolioRisk.MaxDrawdownPct > 100 {
			errs = append(errs, fmt.Sprintf("portfolio_risk.max_drawdown_pct must be in (0, 100], got %g", cfg.PortfolioRisk.MaxDrawdownPct))
		}
		if cfg.PortfolioRisk.MaxNotionalUSD < 0 {
			errs = append(errs, fmt.Sprintf("portfolio_risk.max_notional_usd must be >= 0, got %g", cfg.PortfolioRisk.MaxNotionalUSD))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
