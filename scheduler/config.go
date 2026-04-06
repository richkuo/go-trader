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
	Enabled       bool              `json:"enabled"`
	Token         string            `json:"token"`
	OwnerID       string            `json:"owner_id,omitempty"`        // Discord user ID for DM features (upgrade prompts, config migration)
	DMPaperTrades bool              `json:"dm_paper_trades,omitempty"` // DM owner on paper trade execution
	DMLiveTrades  bool              `json:"dm_live_trades,omitempty"`  // DM owner on live trade execution
	Channels      map[string]string `json:"channels"`                  // keyed by platform or type ("spot", "hyperliquid", "deribit", etc.)
}

// TelegramConfig holds Telegram notification settings.
type TelegramConfig struct {
	Enabled       bool              `json:"enabled"`
	BotToken      string            `json:"bot_token"`
	OwnerChatID   string            `json:"owner_chat_id,omitempty"`   // Owner's Telegram chat ID for DMs/upgrade prompts
	DMPaperTrades bool              `json:"dm_paper_trades,omitempty"` // send message on paper trade execution
	DMLiveTrades  bool              `json:"dm_live_trades,omitempty"`  // send message on live trade execution
	Channels      map[string]string `json:"channels"`                  // keyed by platform or type ("spot", "hyperliquid", etc.)
}

// PortfolioRiskConfig controls aggregate portfolio-level risk (#42).
type PortfolioRiskConfig struct {
	MaxDrawdownPct   float64 `json:"max_drawdown_pct"`             // kill switch threshold (default 25)
	MaxNotionalUSD   float64 `json:"max_notional_usd"`             // 0 = disabled
	WarnThresholdPct float64 `json:"warn_threshold_pct,omitempty"` // % of MaxDrawdownPct to warn (default 80)
}

// PlatformConfig holds per-platform settings (state file path and optional risk overrides).
type PlatformConfig struct {
	StateFile string               `json:"state_file"`     // e.g. "platforms/deribit/state.json"
	Risk      *PortfolioRiskConfig `json:"risk,omitempty"` // overrides portfolio-level defaults
}

// CorrelationConfig controls portfolio-level directional exposure tracking.
type CorrelationConfig struct {
	Enabled             bool    `json:"enabled"`
	MaxConcentrationPct float64 `json:"max_concentration_pct"`  // warn when one asset > X% of gross (default 60)
	MaxSameDirectionPct float64 `json:"max_same_direction_pct"` // warn when >X% of strategies share direction (default 75)
}

// Config is the top-level scheduler configuration.
type Config struct {
	ConfigVersion   int                        `json:"config_version,omitempty"` // bumped when new fields are added; 0/missing = v1 baseline
	IntervalSeconds int                        `json:"interval_seconds"`
	LogDir          string                     `json:"log_dir"`
	StateFile       string                     `json:"state_file"`
	StatusToken     string                     `json:"-"` // loaded from STATUS_AUTH_TOKEN env var only
	Discord         DiscordConfig              `json:"discord"`
	Telegram        TelegramConfig             `json:"telegram,omitempty"`
	AutoUpdate      string                     `json:"auto_update,omitempty"` // "off", "daily", "heartbeat" (default: "off")
	Strategies      []StrategyConfig           `json:"strategies"`
	PortfolioRisk   *PortfolioRiskConfig       `json:"portfolio_risk,omitempty"`
	Correlation     *CorrelationConfig         `json:"correlation,omitempty"`
	Platforms       map[string]*PlatformConfig `json:"platforms,omitempty"`
}

// ThetaHarvestConfig controls early exit on sold options.
type ThetaHarvestConfig struct {
	Enabled         bool    `json:"enabled"`
	ProfitTargetPct float64 `json:"profit_target_pct"` // Close sold options when this % of premium captured (e.g. 60)
	StopLossPct     float64 `json:"stop_loss_pct"`     // Close if loss exceeds this % of premium (e.g. 200 = 2x premium)
	MinDTEClose     float64 `json:"min_dte_close"`     // Force-close positions with fewer than N days to expiry
}

// FuturesConfig holds per-contract futures trading parameters.
type FuturesConfig struct {
	FeePerContract float64 `json:"fee_per_contract"`
	MaxContracts   int     `json:"max_contracts,omitempty"`
}

// StrategyConfig describes a single strategy job.
type StrategyConfig struct {
	ID              string                 `json:"id"`
	Type            string                 `json:"type"`     // "spot", "options", "perps", or "futures"
	Platform        string                 `json:"platform"` // "deribit", "ibkr", "binanceus", "hyperliquid", "topstep"
	Script          string                 `json:"script"`
	Args            []string               `json:"args"`
	Capital         float64                `json:"capital"`
	CapitalPct      float64                `json:"capital_pct,omitempty"` // 0-1; dynamic capital = wallet_balance * capital_pct (overrides capital)
	MaxDrawdownPct  float64                `json:"max_drawdown_pct"`
	IntervalSeconds int                    `json:"interval_seconds,omitempty"` // per-strategy override (0 = use global)
	HTFFilter       bool                   `json:"htf_filter,omitempty"`       // higher-timeframe trend filter
	Params          map[string]interface{} `json:"params,omitempty"`           // custom strategy parameters passed to Python
	ThetaHarvest    *ThetaHarvestConfig    `json:"theta_harvest,omitempty"`
	FuturesConfig   *FuturesConfig         `json:"futures,omitempty"`
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

	// Discord owner ID from env var takes priority over config file.
	if ownerID := os.Getenv("DISCORD_OWNER_ID"); ownerID != "" {
		cfg.Discord.OwnerID = ownerID
	}

	// Telegram bot token from env var takes priority over config file.
	// Warn if token is present in config file (env var is preferred).
	configHasTelegramToken := cfg.Telegram.BotToken != ""
	envTelegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if envTelegramToken != "" {
		if configHasTelegramToken {
			fmt.Println("[WARN] Telegram bot token found in both config file and TELEGRAM_BOT_TOKEN env var. Remove it from config.json to avoid accidental exposure.")
		}
		cfg.Telegram.BotToken = envTelegramToken
	} else if configHasTelegramToken {
		fmt.Println("[WARN] Telegram bot token found in config file. Prefer setting TELEGRAM_BOT_TOKEN env var instead.")
	}
	// Telegram owner chat ID from env var takes priority over config file.
	if telegramOwner := os.Getenv("TELEGRAM_OWNER_CHAT_ID"); telegramOwner != "" {
		cfg.Telegram.OwnerChatID = telegramOwner
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
			case strings.HasPrefix(cfg.Strategies[i].ID, "ts-"):
				cfg.Strategies[i].Platform = "topstep"
			case strings.HasPrefix(cfg.Strategies[i].ID, "rh-"):
				cfg.Strategies[i].Platform = "robinhood"
			case strings.HasPrefix(cfg.Strategies[i].ID, "luno-"):
				cfg.Strategies[i].Platform = "luno"
			case strings.HasPrefix(cfg.Strategies[i].ID, "okx-"):
				cfg.Strategies[i].Platform = "okx"
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
			} else if cfg.Strategies[i].Type == "futures" {
				cfg.Strategies[i].MaxDrawdownPct = 45 // futures: prop firm risk rules are strict
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
	if cfg.PortfolioRisk.WarnThresholdPct == 0 {
		cfg.PortfolioRisk.WarnThresholdPct = 80
	}

	// Correlation tracking defaults.
	if cfg.Correlation == nil {
		cfg.Correlation = &CorrelationConfig{Enabled: false, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}
	}

	if cfg.Correlation.MaxConcentrationPct == 0 {
		cfg.Correlation.MaxConcentrationPct = 60
	}
	if cfg.Correlation.MaxSameDirectionPct == 0 {
		cfg.Correlation.MaxSameDirectionPct = 75
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

		// #36: Type must be "spot", "options", "perps", or "futures".
		if sc.Type != "spot" && sc.Type != "options" && sc.Type != "perps" && sc.Type != "futures" {
			errs = append(errs, fmt.Sprintf("%s: type must be \"spot\", \"options\", \"perps\", or \"futures\", got %q", prefix, sc.Type))
		}

		// Live-mode futures require TopStep API credentials.
		if sc.Type == "futures" {
			for _, arg := range sc.Args {
				if arg == "--mode=live" {
					if os.Getenv("TOPSTEP_API_KEY") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires TOPSTEP_API_KEY env var", prefix))
					}
					if os.Getenv("TOPSTEP_API_SECRET") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires TOPSTEP_API_SECRET env var", prefix))
					}
					if os.Getenv("TOPSTEP_ACCOUNT_ID") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires TOPSTEP_ACCOUNT_ID env var", prefix))
					}
					break
				}
			}
		}

		// Live-mode Robinhood crypto requires credentials.
		if sc.Platform == "robinhood" {
			for _, arg := range sc.Args {
				if arg == "--mode=live" {
					if os.Getenv("ROBINHOOD_USERNAME") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires ROBINHOOD_USERNAME env var", prefix))
					}
					if os.Getenv("ROBINHOOD_PASSWORD") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires ROBINHOOD_PASSWORD env var", prefix))
					}
					if os.Getenv("ROBINHOOD_TOTP_SECRET") == "" {
						errs = append(errs, fmt.Sprintf("%s: --mode=live requires ROBINHOOD_TOTP_SECRET env var", prefix))
					}
					break
				}
			}
		}

		// Live-mode perps require platform-specific env vars.
		if sc.Type == "perps" || (sc.Platform == "okx" && sc.Type == "spot") {
			for _, arg := range sc.Args {
				if arg == "--mode=live" {
					if sc.Platform == "okx" {
						if os.Getenv("OKX_API_KEY") == "" {
							errs = append(errs, fmt.Sprintf("%s: --mode=live requires OKX_API_KEY env var", prefix))
						}
						if os.Getenv("OKX_API_SECRET") == "" {
							errs = append(errs, fmt.Sprintf("%s: --mode=live requires OKX_API_SECRET env var", prefix))
						}
						if os.Getenv("OKX_PASSPHRASE") == "" {
							errs = append(errs, fmt.Sprintf("%s: --mode=live requires OKX_PASSPHRASE env var", prefix))
						}
					} else if sc.Platform == "hyperliquid" || sc.Platform == "" {
						if os.Getenv("HYPERLIQUID_SECRET_KEY") == "" {
							errs = append(errs, fmt.Sprintf("%s: --mode=live requires HYPERLIQUID_SECRET_KEY env var", prefix))
						}
					}
					break
				}
			}
		}

		// #87: capital_pct validation.
		if sc.CapitalPct != 0 {
			if sc.CapitalPct < 0 || sc.CapitalPct > 1 {
				errs = append(errs, fmt.Sprintf("%s: capital_pct must be in (0, 1], got %g", prefix, sc.CapitalPct))
			}
			if sc.Capital > 0 {
				fmt.Printf("[WARN] %s: both capital ($%.0f) and capital_pct (%.0f%%) set — capital_pct takes priority\n", sc.ID, sc.Capital, sc.CapitalPct*100)
			}
		}

		// #36: Capital must be > 0 (unless capital_pct is set).
		if sc.Capital <= 0 && sc.CapitalPct == 0 {
			errs = append(errs, fmt.Sprintf("%s: capital must be > 0 (or set capital_pct), got %g", prefix, sc.Capital))
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
		if cfg.PortfolioRisk.WarnThresholdPct <= 0 || cfg.PortfolioRisk.WarnThresholdPct > 100 {
			errs = append(errs, fmt.Sprintf("portfolio_risk.warn_threshold_pct must be in (0, 100], got %g", cfg.PortfolioRisk.WarnThresholdPct))
		}
	}

	// Validate correlation config.
	if cfg.Correlation != nil && cfg.Correlation.Enabled {
		if cfg.Correlation.MaxConcentrationPct <= 0 || cfg.Correlation.MaxConcentrationPct > 100 {
			errs = append(errs, fmt.Sprintf("correlation.max_concentration_pct must be in (0, 100], got %g", cfg.Correlation.MaxConcentrationPct))
		}
		if cfg.Correlation.MaxSameDirectionPct <= 0 || cfg.Correlation.MaxSameDirectionPct > 100 {
			errs = append(errs, fmt.Sprintf("correlation.max_same_direction_pct must be in (0, 100], got %g", cfg.Correlation.MaxSameDirectionPct))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
