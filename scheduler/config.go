package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DiscordConfig holds Discord notification settings.
type DiscordConfig struct {
	Enabled            bool              `json:"enabled"`
	Token              string            `json:"token"`
	OwnerID            string            `json:"owner_id,omitempty"`            // Discord user ID for DM features (upgrade prompts, config migration)
	DMChannels         map[string]string `json:"dm_channels,omitempty"`         // per-platform DM-style trade alerts: "<platform>" (live), "<platform>-paper" (paper); value = user ID or channel ID
	Channels           map[string]string `json:"channels"`                      // keyed by platform or type; "<platform>-paper" for paper-specific channels
	LeaderboardTopN    int               `json:"leaderboard_top_n,omitempty"`   // number of entries shown in leaderboard messages (default 5)
	LeaderboardChannel string            `json:"leaderboard_channel,omitempty"` // dedicated Discord channel ID for leaderboard posts; when set, all leaderboards route here instead of being broadcast across platform channels
}

// TelegramConfig holds Telegram notification settings.
type TelegramConfig struct {
	Enabled     bool              `json:"enabled"`
	BotToken    string            `json:"bot_token"`
	OwnerChatID string            `json:"owner_chat_id,omitempty"` // Owner's Telegram chat ID for DMs/upgrade prompts
	DMChannels  map[string]string `json:"dm_channels,omitempty"`   // per-platform trade alerts: "<platform>" (live), "<platform>-paper" (paper); value = chat ID
	Channels    map[string]string `json:"channels"`                // keyed by platform or type; "<platform>-paper" for paper-specific channels
}

// PortfolioRiskConfig controls aggregate portfolio-level risk (#42).
type PortfolioRiskConfig struct {
	MaxDrawdownPct   float64 `json:"max_drawdown_pct"`             // kill switch threshold (default 25)
	MaxNotionalUSD   float64 `json:"max_notional_usd"`             // 0 = disabled
	WarnThresholdPct float64 `json:"warn_threshold_pct,omitempty"` // % of MaxDrawdownPct to warn (default 60)
}

// PlatformConfig holds per-platform optional risk overrides.
type PlatformConfig struct {
	Risk *PortfolioRiskConfig `json:"risk,omitempty"` // overrides portfolio-level defaults
}

// CorrelationConfig controls portfolio-level directional exposure tracking.
type CorrelationConfig struct {
	Enabled             bool    `json:"enabled"`
	MaxConcentrationPct float64 `json:"max_concentration_pct"`  // warn when one asset > X% of gross (default 60)
	MaxSameDirectionPct float64 `json:"max_same_direction_pct"` // warn when >X% of strategies share direction (default 75)
}

// LeaderboardSummaryConfig describes a single configurable leaderboard-summary
// post: a platform slice (optionally filtered to one ticker), top-N sort by
// PnL%, sent to a specific channel, optionally on a recurring frequency.
// Issue #308.
type LeaderboardSummaryConfig struct {
	Platform  string `json:"platform"`            // required: e.g. "hyperliquid", "binanceus", "deribit"; matches StrategyConfig.Platform
	Ticker    string `json:"ticker,omitempty"`    // optional: e.g. "ETH", "BTC" (case-insensitive); empty = all tickers
	TopN      int    `json:"top_n,omitempty"`     // optional: entries shown; defaults to 5
	Channel   string `json:"channel"`             // required: channel ID to post to (Discord)
	Frequency string `json:"frequency,omitempty"` // optional: Go duration like "6h"; empty = on-demand only
}

// TradingViewExportConfig controls optional symbol mappings for TradingView
// portfolio CSV exports.
type TradingViewExportConfig struct {
	SymbolOverrides map[string]string `json:"symbol_overrides,omitempty"` // keys may be strategy:symbol, platform:symbol, or symbol
}

// ParsedFrequency returns the parsed duration of Frequency, or 0 if empty/invalid.
// Validation catches invalid values at startup; callers can treat 0 as "disabled".
func (lc LeaderboardSummaryConfig) ParsedFrequency() time.Duration {
	if lc.Frequency == "" {
		return 0
	}
	d, err := time.ParseDuration(lc.Frequency)
	if err != nil {
		return 0
	}
	return d
}

// Key returns a stable identifier for tracking last-post timestamps in state.
// Matches the "platform:ticker:channel" format (ticker lowercased, empty = "*").
func (lc LeaderboardSummaryConfig) Key() string {
	ticker := strings.ToLower(strings.TrimSpace(lc.Ticker))
	if ticker == "" {
		ticker = "*"
	}
	return fmt.Sprintf("%s:%s:%s", strings.ToLower(lc.Platform), ticker, lc.Channel)
}

// Config is the top-level scheduler configuration.
type Config struct {
	ConfigVersion        int                        `json:"config_version,omitempty"` // bumped when new fields are added; 0/missing = v1 baseline
	IntervalSeconds      int                        `json:"interval_seconds"`
	LogDir               string                     `json:"log_dir"`
	DBFile               string                     `json:"db_file,omitempty"`     // SQLite state DB path (default: "scheduler/state.db")
	StatusPort           int                        `json:"status_port,omitempty"` // HTTP status server port (default: 8099; auto-fallback if taken)
	StatusToken          string                     `json:"-"`                     // loaded from STATUS_AUTH_TOKEN env var only
	Discord              DiscordConfig              `json:"discord"`
	Telegram             TelegramConfig             `json:"telegram,omitempty"`
	AutoUpdate           string                     `json:"auto_update,omitempty"`           // "off", "daily", "heartbeat" (default: "off")
	LeaderboardPostTime  string                     `json:"leaderboard_post_time,omitempty"` // "HH:MM" in UTC; auto-post daily leaderboard at this time (empty = disabled)
	Strategies           []StrategyConfig           `json:"strategies"`
	PortfolioRisk        *PortfolioRiskConfig       `json:"portfolio_risk,omitempty"`
	Correlation          *CorrelationConfig         `json:"correlation,omitempty"`
	Platforms            map[string]*PlatformConfig `json:"platforms,omitempty"`
	LeaderboardSummaries []LeaderboardSummaryConfig `json:"leaderboard_summaries,omitempty"` // #308 — configurable per-channel leaderboards
	SummaryFrequency     map[string]string          `json:"summary_frequency,omitempty"`     // #30 — per-channel summary cadence; keys match Discord/Telegram channel keys (e.g. "spot", "options", "hyperliquid"). Values: Go duration ("30m", "2h"), alias ("hourly", "every"/"per_check"/"always"), or empty for legacy default (continuous: every channel run; spot: hourly)
	RiskFreeRate         *float64                   `json:"risk_free_rate,omitempty"`        // #397 — annualized risk-free rate used in Sharpe-ratio calculations (e.g. 0.02 for 2%). Nil/missing falls back to DefaultAnnualRiskFreeRate; an explicit 0 is respected so backtest comparisons can pin to a 0% benchmark.
	TradingViewExport    TradingViewExportConfig    `json:"tradingview_export,omitempty"`    // #3 — optional symbol overrides for TradingView portfolio CSV exports
}

// ParseSummaryFrequency converts a summary_frequency value to a duration.
// Returns -1 to mean "use legacy default", 0 to mean "every channel run", or a
// positive duration when caller should post every duration. An unrecognized
// value returns a non-nil error.
func ParseSummaryFrequency(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1, nil
	}
	switch strings.ToLower(s) {
	case "every", "per_check", "always":
		return 0, nil
	case "hourly":
		return time.Hour, nil
	case "daily":
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative, got %s", s)
	}
	return d, nil
}

// ShouldPostSummary reports whether a channel summary should be posted at now.
// hasTrades unconditionally forces a post (users want immediate trade
// visibility). Otherwise the cadence is derived from freq:
//   - freq empty or invalid → legacy default: continuous channels post every
//     channel run; non-continuous channels post hourly.
//   - freq "every"/"per_check"/"always" → every channel run.
//   - freq parseable as Go duration or alias → post when that wall-clock
//     duration has elapsed since lastPost.
//
// continuous is true for channel types (options/perps/futures) that legacy
// posted every channel run.
func ShouldPostSummary(freq string, continuous, hasTrades bool, lastPost, now time.Time) bool {
	if hasTrades {
		return true
	}
	dur, err := ParseSummaryFrequency(freq)
	if err != nil {
		dur = -1
	}
	switch {
	case dur < 0: // legacy default
		if continuous {
			return true
		}
		dur = time.Hour
	case dur == 0:
		return true
	}
	if lastPost.IsZero() {
		return true
	}
	return now.Sub(lastPost) >= dur
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
	ID                   string                 `json:"id"`
	Type                 string                 `json:"type"`     // "spot", "options", "perps", or "futures"
	Platform             string                 `json:"platform"` // "deribit", "ibkr", "binanceus", "hyperliquid", "topstep"
	Script               string                 `json:"script"`
	Args                 []string               `json:"args"`
	OpenStrategy         string                 `json:"open_strategy,omitempty"`          // optional entry strategy override; defaults to Args[0] for backwards compatibility (#480)
	CloseStrategies      []string               `json:"close_strategies,omitempty"`       // optional exit strategy list; max close_fraction wins (#480)
	DisableImplicitClose bool                   `json:"disable_implicit_close,omitempty"` // when true, legacy signal-reversal exits are disabled unless close_strategies are configured (#480)
	Capital              float64                `json:"capital"`
	CapitalPct           float64                `json:"capital_pct,omitempty"`     // 0-1; dynamic capital = wallet_balance * capital_pct (overrides capital)
	InitialCapital       float64                `json:"initial_capital,omitempty"` // fixed starting balance for PnL display (never overwritten by capital_pct)
	MaxDrawdownPct       float64                `json:"max_drawdown_pct"`
	IntervalSeconds      int                    `json:"interval_seconds,omitempty"`     // per-strategy override (0 = use global)
	HTFFilter            bool                   `json:"htf_filter,omitempty"`           // higher-timeframe trend filter
	AllowShorts          bool                   `json:"allow_shorts,omitempty"`         // perps only: opt-in to bidirectional execution — signal=-1 from flat opens a short, long+(-1) closes-and-flips. Default false preserves close-long-only behavior for strategies like triple_ema that emit -1 only as a long-exit (#328)
	Leverage             float64                `json:"leverage,omitempty"`             // perps exchange leverage (default 1 = no leverage); used for exchange margin/risk and HL update_leverage (#254/#497)
	SizingLeverage       float64                `json:"sizing_leverage,omitempty"`      // perps sizing multiplier; defaults to Leverage for backwards compatibility (#497)
	StopLossPct          *float64               `json:"stop_loss_pct,omitempty"`        // HL perps only: % from entry to place a reduce-only stop-loss trigger. Pointer so omitted (nil) falls through to StopLossMarginPct then MaxDrawdownPct for single-coin strategies (#484); LoadConfig normalizes omitted same-coin peers to explicit 0 (#494); explicit 0 disables auto-SL (#412)
	StopLossMarginPct    *float64               `json:"stop_loss_margin_pct,omitempty"` // HL perps only: % of deployed margin to lose before stop-loss trigger; mutually exclusive with stop_loss_pct; price % derived as StopLossMarginPct / Leverage at order time. Pointer so omitted falls through to MaxDrawdownPct for single-coin strategies; LoadConfig normalizes omitted same-coin peers to explicit 0 (#494); explicit 0 disables (#487, #484)
	MarginMode           string                 `json:"margin_mode,omitempty"`          // HL perps only: "isolated" (default) or "cross"; sent via update_leverage on fresh opens to enforce per-position liq isolation (#486)
	Params               map[string]interface{} `json:"params,omitempty"`               // custom strategy parameters passed to Python
	ThetaHarvest         *ThetaHarvestConfig    `json:"theta_harvest,omitempty"`
	FuturesConfig        *FuturesConfig         `json:"futures,omitempty"`
}

// EffectiveSizingLeverage returns the notional-sizing multiplier for perps.
// Omitted sizing_leverage inherits leverage so legacy configs keep the exact
// old position sizing while new configs can run higher exchange leverage for
// margin/risk math without increasing order size (#497).
func EffectiveSizingLeverage(sc StrategyConfig) float64 {
	if sc.Type != "perps" {
		return 1
	}
	if sc.SizingLeverage > 0 {
		return sc.SizingLeverage
	}
	if sc.Leverage > 0 {
		return sc.Leverage
	}
	return 1
}

// EffectiveExchangeLeverage returns the actual exchange leverage for perps.
func EffectiveExchangeLeverage(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.Leverage <= 0 {
		return 1
	}
	return sc.Leverage
}

// MaxAutoStopLossPct caps the auto-derived per-trade stop at 50% to mirror the
// hand-edited bound enforced on StopLossPct (#421). MaxDrawdownPct can default
// to 50–60 across platforms; using it raw as a price stop would land triggers
// at entry×0 / entry×2 on long/short legs.
const MaxAutoStopLossPct = 50.0

// EffectiveStopLossPct returns the price % to use as the HL reduce-only stop-loss
// trigger for a given strategy. Resolution order (#484):
//  1. Explicit StopLossPct (nil → fall through; explicit 0 → disabled).
//  2. StopLossMarginPct / Leverage (nil → fall through; explicit 0 → disabled).
//  3. MaxDrawdownPct as a fallback so a sole-owner HL perps strategy with a
//     configured drawdown automatically gets exchange-side protection. Capped
//     at MaxAutoStopLossPct. Only applies to single-strategy coins because
//     LoadConfig normalizes omitted stop_loss_* fields to explicit 0 for
//     same-coin HL peer strategies (#494).
//
// HL perps only — returns 0 for non-HL platforms or non-perps types so the
// caller can skip the trigger placement unconditionally.
func EffectiveStopLossPct(sc StrategyConfig) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
	}
	if sc.StopLossPct != nil {
		// Explicit value (including 0 = disabled) wins.
		if *sc.StopLossPct > 0 {
			return *sc.StopLossPct
		}
		return 0
	}
	if sc.StopLossMarginPct != nil {
		if *sc.StopLossMarginPct > 0 && sc.Leverage > 0 {
			return *sc.StopLossMarginPct / sc.Leverage
		}
		return 0
	}
	if sc.MaxDrawdownPct > 0 {
		fallback := sc.MaxDrawdownPct
		if fallback > MaxAutoStopLossPct {
			fallback = MaxAutoStopLossPct
		}
		return fallback
	}
	return 0
}

// EffectiveInitialCapital returns the fixed starting balance for PnL display.
// Priority: config InitialCapital > state InitialCapital > config Capital.
func EffectiveInitialCapital(sc StrategyConfig, ss *StrategyState) float64 {
	if sc.InitialCapital > 0 {
		return sc.InitialCapital
	}
	if ss != nil && ss.InitialCapital > 0 {
		return ss.InitialCapital
	}
	return sc.Capital
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
	if cfg.DBFile == "" {
		cfg.DBFile = "scheduler/state.db"
	}
	if cfg.AutoUpdate == "" {
		cfg.AutoUpdate = "off"
	}

	// Bounds-check status_port. Reject privileged ports (<1024 needs root)
	// and values that would push the auto-fallback sweep past the TCP port
	// ceiling. Zero/missing falls through to resolveStatusPort's default.
	if cfg.StatusPort != 0 {
		if cfg.StatusPort < 1024 {
			return nil, fmt.Errorf("status_port %d is below 1024 (privileged ports require root and are not supported)", cfg.StatusPort)
		}
		if cfg.StatusPort > 65535-statusPortMaxAttempts+1 {
			return nil, fmt.Errorf("status_port %d is too high (max %d to leave room for %d fallback attempts)", cfg.StatusPort, 65535-statusPortMaxAttempts+1, statusPortMaxAttempts)
		}
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

		// #254/#497: Default exchange leverage for perps strategies is 1x
		// (no leverage) when unset. sizing_leverage inherits leverage unless
		// explicitly set so old configs keep their order sizing.
		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].Leverage <= 0 {
			cfg.Strategies[i].Leverage = 1
		}
		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].SizingLeverage == 0 {
			cfg.Strategies[i].SizingLeverage = cfg.Strategies[i].Leverage
		}

		// #486: Default margin mode for HL perps is "isolated". Cross is the
		// HL account default for new accounts, but cross lets a single losing
		// strategy drain margin from unrelated positions before per-strategy
		// drawdown checks fire — isolated aligns on-chain margin with
		// go-trader's per-strategy risk model.
		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].Platform == "hyperliquid" && cfg.Strategies[i].MarginMode == "" {
			cfg.Strategies[i].MarginMode = "isolated"
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
	normalizeHyperliquidPeerStopLosses(cfg.Strategies)

	// #42: Apply portfolio risk defaults if not configured.
	if cfg.PortfolioRisk == nil {
		cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25}
	}
	if cfg.PortfolioRisk.WarnThresholdPct == 0 {
		cfg.PortfolioRisk.WarnThresholdPct = 60
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

// normalizeHyperliquidPeerStopLosses preserves the single-strategy auto-SL
// fallback while avoiding accidental reduce-only trigger races for same-coin
// peer strategies (#494). When multiple HL perps strategies share a coin,
// omitted stop_loss_* fields are treated as an explicit opt-out; operators can
// still select one owner with a positive stop_loss_pct or stop_loss_margin_pct.
func normalizeHyperliquidPeerStopLosses(strategies []StrategyConfig) {
	type peerRef struct {
		ID    string
		Index int
	}
	groups := make(map[string][]peerRef)
	for i, sc := range strategies {
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			continue
		}
		coin := hyperliquidSymbol(sc.Args)
		if coin == "" {
			continue
		}
		groups[coin] = append(groups[coin], peerRef{ID: sc.ID, Index: i})
	}

	coins := make([]string, 0, len(groups))
	for coin := range groups {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		peers := groups[coin]
		if len(peers) < 2 {
			continue
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
		for _, p := range peers {
			sc := &strategies[p.Index]
			if sc.StopLossPct == nil && sc.StopLossMarginPct == nil {
				zero := 0.0
				sc.StopLossPct = &zero
			}
		}
	}
}

// hyperliquidPeerStrategyErrors returns validation messages for HL perps
// strategies that share a coin but disagree on MarginMode or exchange Leverage (#491),
// or that have more than one stop-loss owner (EffectiveStopLossPct > 0 after
// LoadConfig peer normalization, #494). Returns an empty slice when no peer
// conflicts exist.
//
// HL aggregates positions per coin per account, so two go-trader strategies
// on the same coin share an on-chain position, margin assignment, and
// reduce-only stop-loss slots. Mismatched leverage/margin would either fail
// at first peer trade (HL rejects mode changes on an open position) or
// silently land in the wrong mode; conflicting stop-loss triggers race on
// a single position. Per-strategy bookkeeping in SQLite keeps the legs
// separated when peers agree, so this validation is the only thing
// preventing a foot-gun config.
//
// Sub-account isolation is the only correct path for full per-strategy
// independence (different direction, leverage, margin); it is intentionally
// out of scope here and tracked separately.
//
// Note: AllowShorts mismatches across peers on the same coin are NOT
// validated here. A long-only and a short-allowed strategy on the same HL
// coin would silently net/flip at the position level — directional
// independence requires HL sub-accounts (out of scope for #491).
func hyperliquidPeerStrategyErrors(strategies []StrategyConfig) []string {
	type peer struct {
		ID             string
		Coin           string
		MarginMode     string
		Leverage       float64
		EffectiveSLPct float64 // resolved after LoadConfig peer normalization: >0 means this strategy owns a trigger
	}
	groups := make(map[string][]peer)
	for _, sc := range strategies {
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			continue
		}
		coin := hyperliquidSymbol(sc.Args)
		if coin == "" {
			continue
		}
		groups[coin] = append(groups[coin], peer{
			ID:             sc.ID,
			Coin:           coin,
			MarginMode:     sc.MarginMode,
			Leverage:       sc.Leverage,
			EffectiveSLPct: EffectiveStopLossPct(sc),
		})
	}
	var errs []string
	coins := make([]string, 0, len(groups))
	for coin := range groups {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		peers := groups[coin]
		if len(peers) < 2 {
			continue
		}
		// Sort peers by ID so `base` (the comparison reference) is deterministic;
		// any mismatch still triggers regardless of base, but a stable base lets
		// future "report which peer is the outlier" extensions stay reproducible.
		sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
		ids := make([]string, len(peers))
		for i, p := range peers {
			ids[i] = p.ID
		}
		idList := strings.Join(ids, ", ")
		base := peers[0]
		for _, p := range peers[1:] {
			if p.MarginMode != base.MarginMode {
				errs = append(errs, fmt.Sprintf(
					"hyperliquid peers on %s disagree on margin_mode (strategies %s): HL aggregates per coin per account, all peers must share margin_mode",
					coin, idList))
				break
			}
		}
		for _, p := range peers[1:] {
			if p.Leverage != base.Leverage {
				errs = append(errs, fmt.Sprintf(
					"hyperliquid peers on %s disagree on leverage (strategies %s): HL aggregates per coin per account, all peers must share leverage",
					coin, idList))
				break
			}
		}
		stopLossOwners := make([]string, 0)
		for _, p := range peers {
			if p.EffectiveSLPct > 0 {
				stopLossOwners = append(stopLossOwners, p.ID)
			}
		}
		if len(stopLossOwners) > 1 {
			sort.Strings(stopLossOwners)
			errs = append(errs, fmt.Sprintf(
				"hyperliquid peers on %s have conflicting stop_loss_pct (strategies %s): at most one peer may place a reduce-only SL trigger; the others' OIDs would race on the shared on-chain position",
				coin, strings.Join(stopLossOwners, ", ")))
		}
	}
	return errs
}

// ParseLeaderboardPostTime parses a "HH:MM" string and returns (hour, minute, ok).
func ParseLeaderboardPostTime(s string) (int, int, bool) {
	if s == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	m, err2 := strconv.Atoi(parts[1])
	if err2 != nil || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// strategyIntervalExceedsGlobalWarning returns a [WARN] message when the
// per-strategy interval exceeds the top-level interval (#409), or "" otherwise.
func strategyIntervalExceedsGlobalWarning(sc StrategyConfig, globalInterval int) string {
	if sc.IntervalSeconds <= 0 || globalInterval <= 0 || sc.IntervalSeconds <= globalInterval {
		return ""
	}
	ratio := sc.IntervalSeconds / globalInterval
	if sc.IntervalSeconds%globalInterval != 0 {
		ratio++
	}
	return fmt.Sprintf("[WARN] strategy %q interval_seconds=%d exceeds top-level interval_seconds=%d. Strategy will only run every %s portfolio cycle.",
		sc.ID, sc.IntervalSeconds, globalInterval, ordinal(ratio))
}

// ordinal returns the English ordinal suffix form of n (e.g. 1 → "1st", 3 → "3rd", 11 → "11th").
func ordinal(n int) string {
	if n < 0 {
		n = -n
	}
	mod100 := n % 100
	if mod100 >= 11 && mod100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// ValidateConfig checks script paths and strategy fields (#34, #36).
func ValidateConfig(cfg *Config) error {
	var errs []string
	seenIDs := make(map[string]bool)

	// Validate leaderboard_post_time format if set.
	if cfg.LeaderboardPostTime != "" {
		if _, _, ok := ParseLeaderboardPostTime(cfg.LeaderboardPostTime); !ok {
			errs = append(errs, fmt.Sprintf("leaderboard_post_time must be in \"HH:MM\" format (24h UTC), got %q", cfg.LeaderboardPostTime))
		}
	}

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
		if usesOpenCloseConfig(sc) && sc.Type == "options" {
			errs = append(errs, fmt.Sprintf("%s: open_strategy/close_strategies are supported for spot, perps, and futures strategies only", prefix))
		}
		if sc.OpenStrategy != "" {
			if err := validateStrategyConceptName(sc.OpenStrategy); err != nil {
				errs = append(errs, fmt.Sprintf("%s: open_strategy %v", prefix, err))
			}
		}
		for j, name := range sc.CloseStrategies {
			if err := validateStrategyConceptName(name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: close_strategies[%d] %v", prefix, j, err))
			}
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
			// #101: capital_pct on hyperliquid requires account address for balance fetch.
			if sc.CapitalPct > 0 && sc.Platform == "hyperliquid" {
				if os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS") == "" {
					errs = append(errs, fmt.Sprintf("%s: capital_pct requires HYPERLIQUID_ACCOUNT_ADDRESS env var", prefix))
				}
			}
		}

		// initial_capital validation: must be > 0 when set.
		if sc.InitialCapital < 0 {
			errs = append(errs, fmt.Sprintf("%s: initial_capital must be > 0 when set, got %g", prefix, sc.InitialCapital))
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

		// #409: warn when per-strategy interval exceeds the top-level interval;
		// the strategy will only run every Nth portfolio cycle.
		if msg := strategyIntervalExceedsGlobalWarning(sc, cfg.IntervalSeconds); msg != "" {
			fmt.Println(msg)
		}

		// #254/#497: Leverage is exchange leverage and must be >= 1 when set.
		// Only applicable to perps.
		if sc.Leverage != 0 {
			if sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.Leverage < 1 || sc.Leverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: leverage must be in [1, 100], got %g", prefix, sc.Leverage))
			}
		}
		if sc.SizingLeverage != 0 {
			if sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.SizingLeverage < 1 || sc.SizingLeverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage must be in [1, 100], got %g", prefix, sc.SizingLeverage))
			}
		}

		// #486: validate margin_mode (HL perps only). Empty is allowed
		// (LoadConfig defaults it to "isolated" before this point); any
		// non-default value must match the SDK's allowed set.
		if sc.MarginMode != "" {
			if sc.MarginMode != "isolated" && sc.MarginMode != "cross" {
				errs = append(errs, fmt.Sprintf("%s: margin_mode must be \"isolated\" or \"cross\", got %q", prefix, sc.MarginMode))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: margin_mode is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

		// #421: bound-check stop_loss_pct to mirror the init wizard's range.
		// A hand-edited config with stop_loss_pct=200 would otherwise silently
		// place an SL at $0 (long) or 3× entry (short) — both never trigger,
		// breaking the safety feature without any warning. Pointer-aware (#484):
		// nil means the field was omitted (auto-SL falls through to margin/DD
		// for single-coin strategies); explicit 0 means the operator opted out
		// and is allowed. LoadConfig rewrites omitted same-coin peers to 0 (#494).
		if sc.StopLossPct != nil {
			pct := *sc.StopLossPct
			if pct < 0 || pct > 50 {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_pct must be in [0, 50], got %g", prefix, pct))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_pct is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

		// #487: stop_loss_margin_pct expresses the trigger as a % of deployed
		// margin (leverage-aware) and is converted to a price % at order time.
		// Mutually exclusive with stop_loss_pct so the operator can't double up.
		// Pointer-aware (#484): same explicit-vs-omitted distinction. The
		// mutual-exclusion check fires only when at least one field is
		// non-zero; both = 0 is benign (both mean "disabled" — neither
		// places a trigger at runtime, so there is nothing to conflict).
		if sc.StopLossMarginPct != nil {
			marginPct := *sc.StopLossMarginPct
			if sc.StopLossPct != nil && (*sc.StopLossPct > 0 || marginPct > 0) {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_pct and stop_loss_margin_pct are mutually exclusive — set only one (note: stop_loss_pct=0 counts as \"set\" and explicitly disables the auto-SL; remove the field entirely to fall through to stop_loss_margin_pct)", prefix))
			}
			if marginPct < 0 || marginPct > 100 {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_margin_pct must be in [0, 100], got %g", prefix, marginPct))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_margin_pct is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			// Mirror the #421 [0, 50] cap on the *derived* price stop so a
			// hand-edited config like {StopLossMarginPct: 80, Leverage: 1}
			// can't pass validation and silently land an HL trigger at
			// entry×0 (long) or entry×1.8 (short). Skip when explicitly 0
			// (disabled) — derived stop is also 0.
			if marginPct > 0 {
				lev := sc.Leverage
				if lev < 1 {
					lev = 1
				}
				if derived := marginPct / lev; derived > 50 {
					errs = append(errs, fmt.Sprintf("%s: derived stop-loss price %% (stop_loss_margin_pct / leverage = %g) must be <= 50; lower stop_loss_margin_pct or raise leverage", prefix, derived))
				}
			}
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

	// #491: Two HL perps strategies on the same coin land on a single on-chain
	// position (HL nets per coin per account). Peer strategies must agree on
	// MarginMode and Leverage, and at most one peer may carry a per-trade
	// stop-loss — otherwise reduce-only triggers placed by both peers will
	// race on the shared position. Validate up front instead of failing at
	// first trade.
	for _, msg := range hyperliquidPeerStrategyErrors(cfg.Strategies) {
		errs = append(errs, msg)
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

	// Validate leaderboard_summaries (#308).
	// seenKeys detects collisions on Key() (platform:ticker:channel). Two entries
	// that share a key would share one LastLeaderboardSummaries[key] timestamp,
	// so whichever fires first silently blocks the other for the whole Frequency
	// window — review item 4 on #309.
	seenKeys := make(map[string]int)
	for i, lc := range cfg.LeaderboardSummaries {
		prefix := fmt.Sprintf("leaderboard_summaries[%d]", i)
		platformOK := strings.TrimSpace(lc.Platform) != ""
		channelOK := strings.TrimSpace(lc.Channel) != ""
		if !platformOK {
			errs = append(errs, fmt.Sprintf("%s: platform is required", prefix))
		}
		if !channelOK {
			errs = append(errs, fmt.Sprintf("%s: channel is required", prefix))
		}
		if lc.TopN < 0 {
			errs = append(errs, fmt.Sprintf("%s: top_n must be >= 0, got %d", prefix, lc.TopN))
		}
		if lc.Frequency != "" {
			d, err := time.ParseDuration(lc.Frequency)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: frequency invalid duration %q: %v", prefix, lc.Frequency, err))
			} else if d < 1*time.Minute {
				errs = append(errs, fmt.Sprintf("%s: frequency must be >= 1m, got %s", prefix, lc.Frequency))
			}
		}
		if platformOK && channelOK {
			key := lc.Key()
			if prev, dup := seenKeys[key]; dup {
				errs = append(errs, fmt.Sprintf("%s: duplicate entry — platform/ticker/channel (%s) already defined at leaderboard_summaries[%d]", prefix, key, prev))
			} else {
				seenKeys[key] = i
			}
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

	knownPlatforms := make(map[string]bool)
	for _, sc := range cfg.Strategies {
		if p := strings.TrimSpace(sc.Platform); p != "" {
			knownPlatforms[p] = true
		}
	}
	validateDMChannelsMap(cfg.Discord.DMChannels, "discord", knownPlatforms, &errs)
	validateDMChannelsMap(cfg.Telegram.DMChannels, "telegram", knownPlatforms, &errs)

	// Validate summary_frequency values (#30). Keys are free-form channel
	// keys (matching DiscordConfig.Channels), so we don't validate them
	// against a fixed allow-list — only the cadence values.
	for k, v := range cfg.SummaryFrequency {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, "summary_frequency: empty key")
			continue
		}
		if _, err := ParseSummaryFrequency(v); err != nil {
			errs = append(errs, fmt.Sprintf("summary_frequency[%q]: %v", k, err))
		}
	}

	for k, v := range cfg.TradingViewExport.SymbolOverrides {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, "tradingview_export.symbol_overrides: empty key")
			continue
		}
		if !strings.Contains(strings.TrimSpace(v), ":") {
			errs = append(errs, fmt.Sprintf("tradingview_export.symbol_overrides[%q]: value must be in EXCHANGE:TICKER format, got %q", k, v))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateDMChannelsMap checks dm_channels keys and values (#248).
// Keys must be "<platform>" or "<platform>-paper" with a non-empty platform prefix.
// Unknown platforms (not present in cfg.Strategies) produce a warning log but not a validation error.
func validateDMChannelsMap(m map[string]string, label string, knownPlatforms map[string]bool, errs *[]string) {
	if m == nil {
		return
	}
	for k, v := range m {
		if k == "" {
			*errs = append(*errs, fmt.Sprintf("%s: dm_channels has empty key", label))
			continue
		}
		if strings.Contains(k, "-paper") && !strings.HasSuffix(k, "-paper") {
			*errs = append(*errs, fmt.Sprintf("%s: dm_channels key %q is invalid (only optional suffix is \"-paper\")", label, k))
			continue
		}
		platform := strings.TrimSuffix(k, "-paper")
		if platform == "" {
			*errs = append(*errs, fmt.Sprintf("%s: dm_channels key %q is invalid (platform prefix is empty)", label, k))
			continue
		}
		if strings.TrimSpace(v) == "" {
			*errs = append(*errs, fmt.Sprintf("%s: dm_channels[%q] must be non-empty", label, k))
			continue
		}
		if len(knownPlatforms) > 0 && !knownPlatforms[platform] {
			fmt.Printf("[WARN] %s: dm_channels[%q] references platform %q with no configured strategies — possible typo\n", label, k, platform)
		}
	}
}
