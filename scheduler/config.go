Total output lines: 1745

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
	OwnerID            string            `json:"owner_id,omitempty"`             // Discord user ID for DM features (upgrade prompts, config migration)
	DMChannels         map[string]string `json:"dm_channels,omitempty"`          // per-platform DM-style trade alerts: "<platform>" (live), "<platform>-paper" (paper); value = user ID or channel ID
	Channels           map[string]string `json:"channels"`                       // keyed by platform or type; "<platform>-paper" for paper-specific channels
	TradeAlertChannels map[string]string `json:"trade_alert_channels,omitempty"` // optional override: route trade alerts to different channels than summaries; same key scheme as Channels; falls back to Channels on miss
	LeaderboardTopN    int               `json:"leaderboard_top_n,omitempty"`    // number of entries shown in leaderboard messages (default 5)
	LeaderboardChannel string            `json:"leaderboard_channel,omitempty"`  // dedicated Discord channel ID for leaderboard posts; when set, all leaderboards route here instead of being broadcast across platform channels
}

// TelegramConfig holds Telegram notification settings.
type TelegramConfig struct {
	Enabled            bool              `json:"enabled"`
	BotToken           string            `json:"bot_token"`
	OwnerChatID        string            `json:"owner_chat_id,omitempty"`        // Owner's Telegram chat ID for DMs/upgrade prompts
	DMChannels         map[string]string `json:"dm_channels,omitempty"`          // per-platform trade alerts: "<platform>" (live), "<platform>-paper" (paper); value = chat ID
	Channels           map[string]string `json:"channels"`                       // keyed by platform or type; "<platform>-paper" for paper-specific channels
	TradeAlertChannels map[string]string `json:"trade_alert_channels,omitempty"` // optional override: route trade alerts to different channels than summaries; same key scheme as Channels; falls back to Channels on miss
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

// RegimeConfig controls the market regime detector run once per (symbol, timeframe) cycle.
// Default disabled; strategies opt in via AllowedRegimes or by reading params["regime"].
type RegimeConfig struct {
	Enabled      bool    `json:"enabled"`
	Period       int     `json:"period"`        // ADX lookback (Wilder's smoothing); default 14
	ADXThreshold float64 `json:"adx_threshold"` // ADX below this is "ranging"; default 20.0
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
	ConfigVersion          int                        `json:"config_version,omitempty"` // bumped when new fields are added; 0/missing = v1 baseline
	IntervalSeconds        int                        `json:"interval_seconds"`
	LogDir                 string                     `json:"log_dir"`
	DBFile                 string                     `json:"db_file,omitempty"`     // SQLite state DB path (default: "scheduler/state.db")
	StatusPort             int                        `json:"status_port,omitempty"` // HTTP status server port (default: 8099; auto-fallback if taken)
	StatusToken            string                     `json:"-"`                     // loaded from STATUS_AUTH_TOKEN env var only
	Discord                DiscordConfig              `json:"discord"`
	Telegram               TelegramConfig             `json:"telegram,omitempty"`
	AutoUpdate             string                     `json:"auto_update,omitempty"`           // "off", "daily", "heartbeat" (default: "off")
	LeaderboardPostTime    string                     `json:"leaderboard_post_time,omitempty"` // "HH:MM" in UTC; auto-post daily leaderboard at this time (empty = disabled)
	Strategies             []StrategyConfig           `json:"strategies"`
	PortfolioRisk          *PortfolioRiskConfig       `json:"portfolio_risk,omitempty"`
	Correlation            *CorrelationConfig         `json:"correlation,omitempty"`
	Regime                 *RegimeConfig              `json:"regime,omitempty"`
	Platforms              map[string]*PlatformConfig `json:"platforms,omitempty"`
	LeaderboardSummaries   []LeaderboardSummaryConfig `json:"leaderboard_summaries,omitempty"`      // #308 — configurable per-channel leaderboards
	SummaryFrequency       map[string]string          `json:"summary_frequency,omitempty"`          // #30 — per-channel summary cadence; keys match Discord/Telegram channel keys (e.g. "spot", "options", "hyperliquid"). Values: Go duration ("30m", "2h"), alias ("hourly", "every"/"per_check"/"always"), or empty for legacy default (continuous: every channel run; spot: hourly)
	RiskFreeRate           *float64                   `json:"risk_free_rate,omitempty"`             // #397 — annualized risk-free rate used in Sharpe-ratio calculations (e.g. 0.02 for 2%). Nil/missing falls back to DefaultAnnualRiskFreeRate; an explicit 0 is respected so backtest comparisons can pin to a 0% benchmark.
	DefaultStopLossATRMult *float64                   `json:"default_stop_loss_atr_mult,omitempty"` // #605 — top-level default applied to HL perps/manual strategies that omit all stop_loss_* / trailing_stop_* fields. Nil/missing falls back to 1.0; explicit values let operators tune the ATR stop without recompiling.
	NotifyTPSLFills        *bool                      `json:"notify_tp_sl_fills,omitempty"`         // #661 — owner DM when HL on-chain TP/SL fills are detected by the reconciler. Nil/missing → enabled; explicit false disables.
	ManualDefaults         *ManualDefaultsConfig      `json:"manual_defaults,omitempty"`            // #696 — operator-tunable defaults for `manual-open` CLI and `type=manual` strategy auto-config. Each field optional; absent values fall back to the hardcoded defaults.
	TradingViewExport      TradingViewExportConfig    `json:"tradingview_export,omitempty"`         // #3 — optional symbol overrides for TradingView portfolio CSV exports
}

// ManualDefaultsConfig holds operator-tunable defaults for the manual-open CLI
// and type=manual strategy auto-config. All fields are optional; missing values
// fall back to the hardcoded constants (defaultManualMarginUSD,
// defaultManualStopLossATRMult, "long", and the inline [{2×, 0.5}, {3×, 1.0}]
// tier literal). The fleet-wide default_stop_loss_atr_mult=0 opt-out still
// wins over StopLossATRMult: setting it to 0 disables the auto-default
// globally, including for manual strategies (#696).
type ManualDefaultsConfig struct {
	MarginUSD       *float64       `json:"margin_usd,omitempty"`         // implicit --margin (USD) when manual-open is invoked without --size/--notional/--margin (live mode only; --record-only still requires --size). Nil → 50.0.
	StopLossATRMult *float64       `json:"stop_loss_atr_mult,omitempty"` // implicit stop_loss_atr_mult applied to type=manual strategies that omit all five HL stop fields. Nil → 1.5; explicit 0 opts manual strategies out without affecting non-manual perps.
	Side            string         `json:"side,omitempty"`               // implicit --side for manual-open. Lowercase "long" or "short". Empty → "long".
	TPTiers         []ManualTPTier `json:"tp_tiers,omitempty"`           // implicit `tiers` params for tiered_tp_atr / tiered_tp_atr_live close strategies on type=manual. Nil/omitted → [{2.0, 0.5}, {3.0, 1.0}]; empty array is rejected so operators can't accidentally fall back to defaults by zeroing the list.
}

// ManualTPTier is one entry of ManualDefaultsConfig.TPTiers. Matches the JSON
// shape consumed by the tiered_tp_atr* close evaluators ({atr_multiple,
// close_fraction}); the final tier's close_fraction is always coerced to 1.0
// by the evaluator regardless of the configured value.
type ManualTPTier struct {
	ATRMultiple   float64 `json:"atr_multiple"`
	CloseFraction float64 `json:"close_fraction"`
}

// resolveManualMarginUSD returns the implicit margin used when manual-open is
// invoked without any sizing flag. Operator config wins; hardcoded constant is
// the fallback.
func (c *Config) resolveManualMarginUSD() float64 {
	if c != nil && c.ManualDefaults != nil && c.ManualDefaults.MarginUSD != nil {
		return *c.ManualDefaults.MarginUSD
	}
	return defaultManualMarginUSD
}

// resolveManualSide returns the implicit --side for manual-open. Operator
// config wins; "long" is the fallback.
func (c *Config) resolveManualSide() string {
	if c != nil && c.ManualDefaults != nil && c.ManualDefaults.Side != "" {
		return c.ManualDefaults.Side
	}
	return "long"
}

// resolveManualStopLossATRMult returns the implicit stop_loss_atr_mult for
// type=manual strategies that omit all five HL stop fields. Operator config
// wins; the 1.5× hardcoded fallback is preserved when absent.
func (c *Config) resolveManualStopLossATRMult() float64 {
	if c != nil && c.ManualDefaults != nil && c.ManualDefaults.StopLossATRMult != nil {
		return *c.ManualDefaults.StopLossATRMult
	}
	return defaultManualStopLossATRMult
}

// resolveManualTPTiers returns the implicit `tiers` params for
// tiered_tp_atr* close strategies on type=manual. Operator config wins; the
// inline [{2×, 0.5}, {3×, 1.0}] literal is preserved when absent. Returns a
// fresh slice so callers can stamp it onto Params without aliasing.
func (c *Config) resolveManualTPTiers() []interface{} {
	if c != nil && c.ManualDefaults != nil && len(c.ManualDefaults.TPTiers) > 0 {
		tiers := make([]interface{}, len(c.ManualDefaults.TPTiers))
		for i, t := range c.ManualDefaults.TPTiers {
			tiers[i] = map[string]interface{}{
				"atr_multiple":   t.ATRMultiple,
				"close_fraction": t.CloseFraction,
			}
		}
		return tiers
	}
	return []interface{}{
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
	}
}

// NotifyTPSLFillsEnabled reports whether reconciler-detected TP/SL fills should
// trigger an owner DM. Nil pointer (missing field) defaults to true so existing
// configs get the alert without an explicit opt-in.
func (c *Config) NotifyTPSLFillsEnabled() bool {
	if c == nil || c.NotifyTPSLFills == nil {
		return true
	}
	return *c.NotifyTPSLFills
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

// StrategyRef pairs a strategy name with its evaluator params. Used for both
// the open strategy and each close strategy on a StrategyConfig so per-strategy
// params don't leak across roles (#640). Empty Params means "use registry
// defaults"; the open and close registries each merge their default_params
// over user-provided keys at evaluation time.
type StrategyRef struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// StrategyConfig describes a single strategy job.
type StrategyConfig struct {
	ID                     string              `json:"id"`
	Type                   string              `json:"type"`                // "spot", "options", "perps", "futures", or "manual"
	Platform               string              `json:"platform"`            // "deribit", "ibkr", "binanceus", "hyperliquid", "topstep"
	Symbol                 string              `json:"symbol,omitempty"`    // manual strategies: trading symbol (e.g. "ETH")
	Timeframe              string              `json:"timeframe,omitempty"` // manual strategies: OHLCV timeframe (e.g. "1h")
	Script                 string              `json:"script"`
	Args                   []string            `json:"args"`
	OpenStrategy           StrategyRef         `json:"open_strategy"`              // entry strategy ref (name + params). Migrated from legacy string-typed open_strategy / args[0] in v13 (#640)
	CloseStrategies        []StrategyRef       `json:"close_strategies,omitempty"` // exit strategy refs (name + params); max close_fraction wins (#480). Migrated from legacy []string in v13 (#640)
	AllowedRegimes         []string            `json:"allowed_regimes,omitempty"`  // gate entries: skip signal when current regime not in this list; empty = allow all (#482)
	Capital                float64             `json:"capital"`
	CapitalPct             float64             `json:"capital_pct,omitempty"`     // 0-1; dynamic capital = wallet_balance * capital_pct (overrides capital)
	InitialCapital         float64             `json:"initial_capital,omitempty"` // fixed starting balance for PnL display (never overwritten by capital_pct)
	MaxDrawdownPct         float64             `json:"max_drawdown_pct"`
	IntervalSeconds        int                 `json:"interval_seconds,omitempty"`           // per-strategy override (0 = use global)
	HTFFilter              bool                `json:"htf_filter,omitempty"`                 // higher-timeframe trend filter
	InvertSignal           bool                `json:"invert_signal,omitempty"`              // flip strategy signal sign before execution (BUY<->SELL); useful for inverse variants that reuse the same open/close refs
	AllowShorts            bool                `json:"allow_shorts,omitempty"`               // DEPRECATED — use Direction. Perps only; legacy boolean retained on the struct so pre-v14 JSON unmarshals cleanly. Read via EffectiveDirection / PerpsAllowsShort / PerpsAllowsLong, never directly. Migrated to Direction in v14 (#656).
	Direction              string              `json:"direction,omitempty"`                  // perps only: "long" (default; signal=1 opens, signal=-1 closes long), "short" (signal=-1 opens, signal=1 closes short), "both" (bidirectional). Empty falls back to AllowShorts (legacy). v14 migration converts allow_shorts→direction. (#656)
	Leverage               float64             `json:"leverage,omitempty"`                   // perps exchange leverage (default 1 = no leverage); used for exchange margin/risk and HL update_leverage (#254/#497)
	SizingLeverage         float64             `json:"sizing_leverage,omitempty"`            // perps notional multiplier; defaults to Leverage for backwards compatibility (#497). Notional formula: notional = cash * sizing_leverage; size = notional / price. For margin-based sizing, prefer MarginPerTradeUSD (#518).
	MarginPerTradeUSD      *float64            `json:"margin_per_trade_usd,omitempty"`       // perps only: USD margin to deploy per open. When set (positive), overrides SizingLeverage: notional = min(MarginPerTradeUSD, cash) * exchange_leverage; size = notional / price. Lets operators size in margin-space directly so high exchange_leverage doesn't decouple intent from outcome (#518).
	StopLossPct  …10758 tokens truncated…nd(errs, fmt.Sprintf("%s: initial_capital must be > 0 when set, got %g", prefix, sc.InitialCapital))
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
		// Only applicable to perps and manual (#569: manual uses leverage for sizing).
		if sc.Leverage != 0 {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.Leverage < 1 || sc.Leverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: leverage must be in [1, 100], got %g", prefix, sc.Leverage))
			}
		}
		// SizingLeverage decouples position sizing from exchange margin (#497).
		// A legitimate use case is high exchange leverage with conservative
		// position size (e.g. leverage=20, sizing_leverage=0.5), so the lower
		// bound is a small positive value rather than 1. The math
		// (cash * sizing_leverage) tolerates fractional values fine.
		if sc.SizingLeverage != 0 {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.SizingLeverage < 0.01 || sc.SizingLeverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage must be in [0.01, 100], got %g", prefix, sc.SizingLeverage))
			}
		}

		// MarginPerTradeUSD lets operators express open size in margin-space
		// (#518). Mutually compatible with sizing_leverage at the schema level —
		// when set, MarginPerTradeUSD wins inside ComputePerpsOpenNotional —
		// but we still require a positive value because nil/0 means "use the
		// legacy formula" and a negative value is meaningless.
		if sc.MarginPerTradeUSD != nil {
			if sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: margin_per_trade_usd is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if *sc.MarginPerTradeUSD <= 0 {
				errs = append(errs, fmt.Sprintf("%s: margin_per_trade_usd must be positive, got %g", prefix, *sc.MarginPerTradeUSD))
			}
		}

		// #656: validate direction (perps only). Empty is allowed and falls
		// back to AllowShorts via EffectiveDirection (legacy pre-v14 configs).
		if sc.Direction != "" {
			switch sc.Direction {
			case DirectionLong, DirectionShort, DirectionBoth:
			default:
				errs = append(errs, fmt.Sprintf("%s: direction must be %q, %q, or %q, got %q", prefix, DirectionLong, DirectionShort, DirectionBoth, sc.Direction))
			}
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: direction is only supported for perps/manual strategies (got type %q)", prefix, sc.Type))
			}
			// Hand-edit detection: direction="long" alongside an explicit
			// allow_shorts=true is contradictory (Direction wins; the
			// AllowShorts=true is dead). Catch it so the operator can clean up.
			// The opposite case (direction set, AllowShorts=false zero value)
			// is indistinguishable from "AllowShorts not set in JSON" so we
			// can't reliably warn about it.
			if sc.AllowShorts && sc.Direction == DirectionLong {
				errs = append(errs, fmt.Sprintf("%s: direction=%q conflicts with legacy allow_shorts=true (remove allow_shorts; v14 migration normally handles this)", prefix, sc.Direction))
			}
		}

		// #486: validate margin_mode (HL perps only). Empty is allowed
		// (LoadConfig defaults it to "isolated" before this point); any
		// non-default value must match the SDK's allowed set.
		if sc.MarginMode != "" {
			if sc.MarginMode != "isolated" && sc.MarginMode != "cross" {
				errs = append(errs, fmt.Sprintf("%s: margin_mode must be \"isolated\" or \"cross\", got %q", prefix, sc.MarginMode))
			}
			if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
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

		// #501: synthetic trailing stops reuse the same HL reduce-only trigger
		// slot as fixed stop_loss_pct / stop_loss_margin_pct. Only one positive
		// stop owner may be configured for a strategy.
		if sc.TrailingStopPct != nil {
			pct := *sc.TrailingStopPct
			if pct < 0 || pct > 50 {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_pct must be in [0, 50], got %g", prefix, pct))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_pct is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			fixedPct := 0.0
			if sc.StopLossPct != nil {
				fixedPct = *sc.StopLossPct
			}
			marginPct := 0.0
			if sc.StopLossMarginPct != nil {
				marginPct = *sc.StopLossMarginPct
			}
			if pct > 0 && (fixedPct > 0 || marginPct > 0) {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_pct is mutually exclusive with stop_loss_pct and stop_loss_margin_pct", prefix))
			}
		}
		// #505: ATR-derived trailing stops. The price % is resolved per-position
		// at runtime from EntryATR / AvgCost, so validation only enforces shape:
		// HL perps only, > 0, mutually exclusive with the fixed-distance stops.
		if sc.TrailingStopATRMult != nil {
			mult := *sc.TrailingStopATRMult
			if mult < 0 {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult must be >= 0, got %g", prefix, mult))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if mult > 0 {
				fixedPct := 0.0
				if sc.StopLossPct != nil {
					fixedPct = *sc.StopLossPct
				}
				marginPct := 0.0
				if sc.StopLossMarginPct != nil {
					marginPct = *sc.StopLossMarginPct
				}
				trailingPct := 0.0
				if sc.TrailingStopPct != nil {
					trailingPct = *sc.TrailingStopPct
				}
				if fixedPct > 0 || marginPct > 0 || trailingPct > 0 {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult is mutually exclusive with stop_loss_pct, stop_loss_margin_pct, and trailing_stop_pct", prefix))
				}
			}
		}
		// #562: Fixed (non-trailing) ATR-derived stop loss. Same shape rules as
		// trailing_stop_atr_mult: HL perps only, >= 0, mutually exclusive with
		// the other four stop-loss / trailing-stop fields. Per-position price %
		// is derived at arming time from EntryATR / AvgCost.
		if sc.StopLossATRMult != nil {
			mult := *sc.StopLossATRMult
			if mult < 0 {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult must be >= 0, got %g", prefix, mult))
			}
			if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if mult > 0 {
				fixedPct := 0.0
				if sc.StopLossPct != nil {
					fixedPct = *sc.StopLossPct
				}
				marginPct := 0.0
				if sc.StopLossMarginPct != nil {
					marginPct = *sc.StopLossMarginPct
				}
				trailingPct := 0.0
				if sc.TrailingStopPct != nil {
					trailingPct = *sc.TrailingStopPct
				}
				atrTrailingMult := 0.0
				if sc.TrailingStopATRMult != nil {
					atrTrailingMult = *sc.TrailingStopATRMult
				}
				if fixedPct > 0 || marginPct > 0 || trailingPct > 0 || atrTrailingMult > 0 {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult is mutually exclusive with stop_loss_pct, stop_loss_margin_pct, trailing_stop_pct, and trailing_stop_atr_mult", prefix))
				}
			}
		}
		// #708: sl_after rules on tiered TPs (post-fill SL adjustment).
		for _, msg := range validatePostTPStopLossRules(sc) {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, msg))
		}

		if sc.TrailingStopMinMovePct != nil {
			pct := *sc.TrailingStopMinMovePct
			if pct < 0 || pct > 100 {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct must be in [0, 100], got %g", prefix, pct))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			fixedTrailingPct := 0.0
			if sc.TrailingStopPct != nil {
				fixedTrailingPct = *sc.TrailingStopPct
			}
			atrMult := 0.0
			if sc.TrailingStopATRMult != nil {
				atrMult = *sc.TrailingStopATRMult
			}
			if fixedTrailingPct <= 0 && atrMult <= 0 {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct requires trailing_stop_pct > 0 or trailing_stop_atr_mult > 0", prefix))
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

	// Validate regime config.
	if cfg.Regime != nil && cfg.Regime.Enabled {
		if cfg.Regime.Period <= 0 {
			errs = append(errs, fmt.Sprintf("regime.period must be > 0, got %d", cfg.Regime.Period))
		}
		if cfg.Regime.ADXThreshold <= 0 || cfg.Regime.ADXThreshold > 100 {
			errs = append(errs, fmt.Sprintf("regime.adx_threshold must be in (0, 100], got %g", cfg.Regime.ADXThreshold))
		}
	}

	// Warn when allowed_regimes is configured but regime.enabled=false — the
	// gate reads result.Regime from the check script output, which requires
	// regime detection to be running. Without it the gate is a no-op.
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		for _, sc := range cfg.Strategies {
			if len(sc.AllowedRegimes) > 0 {
				fmt.Printf("[WARN] %s: allowed_regimes is set but regime.enabled=false — gate is a no-op until regime detection is enabled\n", sc.ID)
			}
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

	// #733: regime-aware ATR multiplier validation. Runs the surface-aware
	// parsing pass on each strategy's StopLossATRRegime / TrailingStopATRRegime
	// and on every tiered_tp_atr_regime / tiered_tp_atr_live_regime close ref.
	// Also enforces mutex with scalar siblings + regime-enabled requirement.
	errs = append(errs, validateRegimeATRConfig(cfg)...)

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
