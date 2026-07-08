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
	EphemeralReplies   bool              `json:"ephemeral_replies,omitempty"`    // when true, read-only slash-command replies (/status, /pnl, etc.) are ephemeral (visible only to the invoker); default false (public in channel)
	ReportRepo         string            `json:"report_repo,omitempty"`          // GitHub repo (owner/name) the /report-an-issue command files issues against; defaults to richkuo/go-trader
	ReportGitHubToken  string            `json:"report_github_token,omitempty"`  // GitHub token for /report-an-issue; prefer the GO_TRADER_GITHUB_TOKEN / GITHUB_TOKEN env var over storing it here
}

// reportRepo returns the owner/name repo for the /report-an-issue command, defaulting to
// this project's repo when unset.
func (c DiscordConfig) reportRepo() string {
	if r := strings.TrimSpace(c.ReportRepo); r != "" {
		return r
	}
	return defaultReportRepo
}

// reportToken resolves the GitHub token for /report-an-issue. Env vars win over the config
// field so the secret can live in /opt/go-trader/.env rather than the config JSON.
func (c DiscordConfig) reportToken() string {
	if t := strings.TrimSpace(os.Getenv("GO_TRADER_GITHUB_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(c.ReportGitHubToken)
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
	Enabled      bool             `json:"enabled"`
	Period       int              `json:"period"`              // ADX lookback (Wilder's smoothing); default 14; legacy single-window mode
	ADXThreshold float64          `json:"adx_threshold"`       // ADX below this is "ranging"; default 20.0
	Timeframe    string           `json:"timeframe,omitempty"` // optional candle timeframe for non-options regime bundles; empty = strategy args[2]
	Windows      RegimeWindowsMap `json:"windows,omitempty"`   // name -> classifier+period; bare int = ADX period (#792/#795)
	// DisplayWindows optionally restricts which regime windows appear in the
	// Discord/cycle summary (#1062). Display-only: it never affects regime
	// calculation or gating. Names match window keys case-insensitively (e.g.
	// "composite_long", "long"). Empty/omitted preserves the legacy behavior of
	// rendering every window. When set but no configured window matches a
	// populated label, the summary falls back to the single primary regime
	// string (same fallback as the multi-window-disabled path).
	DisplayWindows []string `json:"display_windows,omitempty"`
	// Transitions enables per-window regime transition history + operator
	// alerting (#1224). Alerting-only; hot-reloadable via SIGHUP.
	Transitions *RegimeTransitionAlertsConfig `json:"transitions,omitempty"`
}

var regimeTimeframeAllowSet = map[string]bool{
	"1m": true, "2m": true, "3m": true, "5m": true, "15m": true, "30m": true,
	"60m": true, "90m": true,
	"1h": true, "2h": true, "4h": true, "6h": true, "8h": true, "12h": true,
	"1d": true, "3d": true, "5d": true, "1w": true, "1mo": true, "3mo": true,
}

func normalizeRegimeTimeframe(tf string) string {
	return strings.ToLower(strings.TrimSpace(tf))
}

func validRegimeTimeframe(tf string) bool {
	return regimeTimeframeAllowSet[normalizeRegimeTimeframe(tf)]
}

func validRegimeTimeframes() []string {
	out := make([]string, 0, len(regimeTimeframeAllowSet))
	for tf := range regimeTimeframeAllowSet {
		out = append(out, tf)
	}
	sort.Strings(out)
	return out
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
	NotifyRatchetTriggers  *bool                      `json:"notify_ratchet_triggers,omitempty"`    // #1110 — owner DM when a trailing_tp_ratchet* tier clears and tightens the trail. Nil/missing → enabled; explicit false disables.
	AlertThrottleInterval  string                     `json:"alert_throttle_interval,omitempty"`    // #1266 — fleet-wide re-alert back-off for throttled operator alerts. Go duration ("6h", "30m"); empty → 6h.
	TradingViewExport      TradingViewExportConfig    `json:"tradingview_export,omitempty"`         // #3 — optional symbol overrides for TradingView portfolio CSV exports
	UserDefaults           *UserDefaultsConfig        `json:"user_defaults,omitempty"`              // #1135 — canonical operator override layer for defaults. close → close-evaluator tier ladders; regime_atr → standalone use_defaults-only *_atr_regime owners; manual → manual-open/type=manual defaults. Legacy user_close_defaults/manual_defaults are migrated to this tree at load.
}

// UserDefaultsConfig is the canonical #1135 operator defaults block.
type UserDefaultsConfig struct {
	Close     CloseDefaultsMap       `json:"close,omitempty"`      // #866/#1133 — close-evaluator name → params object carrying tp_tiers, and trailing_tp_ratchet_regime may also carry trailing_stop_atr_regime.
	RegimeATR map[string]interface{} `json:"regime_atr,omitempty"` // #1134 — optional stop_loss_atr_regime / trailing_stop_atr_regime maps for standalone use_defaults-only strategy owners.
	Manual    *ManualDefaultsConfig  `json:"manual,omitempty"`     // #696/#1115 — operator-tunable defaults for manual-open and type=manual strategy auto-config.
}

// CloseDefaultsMap is the #866 user_defaults.close block: close-evaluator name →
// params object carrying tp_tiers (a scalar tier list, or a regime-keyed map for
// the *_regime evaluators). The inner values are left as decoded interface{}s so
// they can be injected straight into a close ref's Params at load.
type CloseDefaultsMap map[string]map[string]interface{}

func (c *Config) userDefaultsClose() CloseDefaultsMap {
	if c == nil || c.UserDefaults == nil {
		return nil
	}
	return c.UserDefaults.Close
}

func (c *Config) userDefaultsRegimeATR() map[string]interface{} {
	if c == nil || c.UserDefaults == nil {
		return nil
	}
	return c.UserDefaults.RegimeATR
}

func (c *Config) userDefaultsManual() *ManualDefaultsConfig {
	if c == nil || c.UserDefaults == nil {
		return nil
	}
	return c.UserDefaults.Manual
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
	StopLossATRMult *float64       `json:"stop_loss_atr_mult,omitempty"` // implicit stop_loss_atr_mult applied to type=manual strategies that omit all five HL stop fields. Nil → 2.0; explicit 0 opts scalar manual strategies out without affecting non-manual perps. Ratchet fallback ignores 0 to preserve no-naked protection.
	Side            string         `json:"side,omitempty"`               // implicit --side for manual-open. Lowercase "long" or "short". Empty → "long".
	TPTiers         []ManualTPTier `json:"tp_tiers,omitempty"`           // implicit `tiers` params for tiered_tp_atr / tiered_tp_atr_live close strategies on type=manual. Nil/omitted → [{2.0, 0.5}, {3.0, 1.0}]; empty array is rejected so operators can't accidentally fall back to defaults by zeroing the list.

	// #1115: implicit per-regime opening trail / SL block applied to type=manual
	// strategies that DEFAULT to trailing_tp_ratchet_regime (regime enabled, no
	// explicit close_strategy). Omitted → a use_defaults baseline keyed to the
	// strategy's active classifier vocabulary; an explicit block lets operators
	// tune the per-regime trail widths. Resolved per-strategy at validateConfig
	// against that strategy's classifier labels (never resolved standalone, so a
	// malformed block surfaces its error only on a strategy that actually adopts
	// it). Mirrors the stop_loss_atr_mult / tp_tiers knobs above.
	TrailingStopATRRegime *RegimeATRBlock `json:"trailing_stop_atr_regime,omitempty"`
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
	if md := c.userDefaultsManual(); md != nil && md.MarginUSD != nil {
		return *md.MarginUSD
	}
	return defaultManualMarginUSD
}

// resolveManualSide returns the implicit --side for manual-open. Operator
// config wins; "long" is the fallback.
func (c *Config) resolveManualSide() string {
	if md := c.userDefaultsManual(); md != nil && md.Side != "" {
		return md.Side
	}
	return "long"
}

// resolveManualStopLossATRMult returns the implicit stop_loss_atr_mult for
// type=manual strategies that omit all five HL stop fields. Operator config
// wins; the 2.0× hardcoded fallback is preserved when absent.
func (c *Config) resolveManualStopLossATRMult() float64 {
	if md := c.userDefaultsManual(); md != nil && md.StopLossATRMult != nil {
		return *md.StopLossATRMult
	}
	return defaultManualStopLossATRMult
}

// resolveManualRatchetFallbackATRMult returns the protective fallback used when
// manual-open cannot resolve the current per-regime ratchet trail. It is always
// strictly positive so a regime-read failure cannot intentionally or accidentally
// open a naked manual position; user_defaults.manual.stop_loss_atr_mult=0 only
// opts out the scalar manual default.
func (c *Config) resolveManualRatchetFallbackATRMult() float64 {
	if md := c.userDefaultsManual(); md != nil && md.StopLossATRMult != nil && *md.StopLossATRMult > 0 {
		return *md.StopLossATRMult
	}
	return defaultManualStopLossATRMult
}

// resolveManualTPTiers returns the implicit `tiers` params for
// tiered_tp_atr* close strategies on type=manual. Operator config wins; the
// inline [{2×, 0.5}, {3×, 1.0}] literal is preserved when absent. Returns a
// fresh slice so callers can stamp it onto Params without aliasing.
func (c *Config) resolveManualTPTiers() []interface{} {
	if md := c.userDefaultsManual(); md != nil && len(md.TPTiers) > 0 {
		tiers := make([]interface{}, len(md.TPTiers))
		for i, t := range md.TPTiers {
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

// resolveManualRatchetRegimeTrailBlock decides whether a type=manual strategy
// with no explicit close_strategy should default to trailing_tp_ratchet_regime
// (#1115) and, if so, returns the per-regime opening trail / SL block to attach.
// It returns (block, true) only when (a) regime detection is enabled and (b)
// every label in the strategy's active ATR-window classifier vocabulary resolves
// to a default opening trail — otherwise (nil, false), so the caller keeps the
// historical tiered_tp_atr_live default and a regime-less or unmappable config is
// unchanged. The returned block is fresh per call (its raw shape is resolved
// per-strategy during validateConfig, which mutates UseDefaults/TrendRegime), so
// the caller can safely assign it to one strategy's sc.TrailingStopATRRegime
// without aliasing another's.
func (c *Config) resolveManualRatchetRegimeTrailBlock(sc StrategyConfig) (*RegimeATRBlock, bool) {
	if c == nil || c.Regime == nil || !c.Regime.Enabled {
		return nil, false
	}
	// Honor explicit operator stop-field overrides: trailing_tp_ratchet_regime
	// forbids every scalar/regime stop field, so if the operator set one (with no
	// close_strategy) selecting the ratchet would turn a previously-valid config
	// into a validation error. Fall back to tiered_tp_atr_live (compatible with a
	// scalar stop) so their intent is preserved. Mirrors the manual scalar-SL
	// default predicate below. IsConfigured() is the raw-aware check (this runs
	// before ResolveSurface populates the typed regime fields, review #735.1).
	if sc.StopLossATRMult != nil || sc.StopLossPct != nil || sc.StopLossMarginPct != nil ||
		sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRRegime.IsConfigured() || sc.TrailingStopATRRegime.IsConfigured() {
		return nil, false
	}
	labels := regimeLabelsForStrategyWindow(sc, c.Regime, "atr")
	if len(labels) == 0 {
		return nil, false
	}
	// Operator override: user_defaults.manual.trailing_stop_atr_regime supplies
	// the per-regime opening trail (mirrors the stop_loss_atr_mult / tp_tiers
	// knobs).
	// Clone its raw shape so each adopting strategy resolves an independent copy.
	if md := c.userDefaultsManual(); md != nil && md.TrailingStopATRRegime.IsConfigured() {
		if block := cloneRegimeATRBlock(md.TrailingStopATRRegime); block != nil {
			return block, true
		}
	}
	// #1133: the fleet-wide ratchet package can also provide the coupled
	// per-regime opening trail. Manual-specific defaults still win above; this
	// user_defaults.close layer wins over the system use_defaults baseline below.
	if block, ok := userCloseDefaultTrailingStopATRRegime(c.userDefaultsClose()); ok {
		return block, true
	}
	// Default: synthesize a use_defaults block, but only when every active label
	// maps onto the baseline opening-trail family — else the ratchet would carry
	// a per-regime hole and we must not silently default into an un-resolvable
	// close (fail back to tiered_tp_atr_live instead).
	for _, label := range labels {
		if _, ok := mapRegimeToBaselineFamily(regimeATRDefaults.Trailing, label); !ok {
			return nil, false
		}
	}
	return &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}}, true
}

// cloneRegimeATRBlock deep-copies a RegimeATRBlock so an operator-supplied
// user_defaults.manual block can be attached to multiple strategies independently
// (#1115). The raw shape is the source of truth before validateConfig resolves
// it, so it is JSON-round-tripped; the typed fields are copied too for blocks
// that were already resolved. Returns nil for a nil input.
func cloneRegimeATRBlock(b *RegimeATRBlock) *RegimeATRBlock {
	if b == nil {
		return nil
	}
	out := &RegimeATRBlock{UseDefaults: b.UseDefaults}
	if b.raw != nil {
		if blob, err := json.Marshal(b.raw); err == nil {
			var cp map[string]interface{}
			if json.Unmarshal(blob, &cp) == nil {
				out.raw = cp
			}
		}
	}
	if len(b.TrendRegime) > 0 {
		out.TrendRegime = make(map[string]RegimeATREntry, len(b.TrendRegime))
		for k, v := range b.TrendRegime {
			out.TrendRegime[k] = v
		}
	}
	return out
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

// NotifyRatchetTriggersEnabled reports whether a trailing_tp_ratchet* tier
// clearing (and tightening the trail) should trigger an owner DM. Nil pointer
// (missing field) defaults to true so existing configs get the alert without an
// explicit opt-in (mirrors NotifyTPSLFillsEnabled).
func (c *Config) NotifyRatchetTriggersEnabled() bool {
	if c == nil || c.NotifyRatchetTriggers == nil {
		return true
	}
	return *c.NotifyRatchetTriggers
}

// NotifyRatchetTriggersEnabled reports whether THIS strategy's ratchet-tighten
// owner DM (#1110) is enabled, using a two-layer resolve: the per-strategy
// notify_ratchet_triggers (#1118) wins when set, else it inherits the global
// Config.NotifyRatchetTriggersEnabled(). A nil strategy field therefore
// preserves existing behavior for anyone who never sets it.
func (sc *StrategyConfig) NotifyRatchetTriggersEnabled(cfg *Config) bool {
	if sc != nil && sc.NotifyRatchetTriggers != nil {
		return *sc.NotifyRatchetTriggers
	}
	return cfg.NotifyRatchetTriggersEnabled()
}

// CircuitBreakerEnabled reports whether the per-strategy circuit breaker is
// active. Nil pointer (missing field) defaults to true so existing configs keep
// the auto-protective behavior without an explicit opt-in; an explicit false
// disables both firing arms in CheckRisk (drawdown and consecutive-loss). Safe
// on a nil receiver (treated as enabled). (#1048)
func (sc *StrategyConfig) CircuitBreakerEnabled() bool {
	if sc == nil || sc.CircuitBreaker == nil {
		return true
	}
	return *sc.CircuitBreaker
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
	ID                      string                   `json:"id"`
	Type                    string                   `json:"type"`                // "spot", "options", "perps", "futures", or "manual"
	Platform                string                   `json:"platform"`            // "deribit", "ibkr", "binanceus", "hyperliquid", "topstep"
	Symbol                  string                   `json:"symbol,omitempty"`    // manual strategies: trading symbol (e.g. "ETH")
	Timeframe               string                   `json:"timeframe,omitempty"` // manual strategies: OHLCV timeframe (e.g. "1h")
	Script                  string                   `json:"script"`
	Args                    []string                 `json:"args"`
	OpenStrategy            StrategyRef              `json:"open_strategy"`                       // entry strategy ref (name + params). Migrated from legacy string-typed open_strategy / args[0] in v13 (#640)
	CloseStrategy           *StrategyRef             `json:"close_strategy,omitempty"`            // single exit strategy ref (name + params). Collapsed from the legacy close_strategies array in #842 — one profit-taking close owns the exit ladder; risk backstops live at strategy level. Nil = open-as-close. UnmarshalJSON still reads the legacy close_strategies array for back-compat (len 1 lifted here, len>1 rejected at validation with the strategy id).
	closeStrategiesLegacy   []StrategyRef            `json:"-"`                                   // #842: legacy close_strategies array captured by UnmarshalJSON for back-compat; only used to reject len>1 during validation. Never marshaled.
	AllowedRegimes          []string                 `json:"allowed_regimes,omitempty"`           // gate entries: skip signal when current regime not in this list; empty = allow all (#482)
	RegimeGateWindow        string                   `json:"regime_gate_window,omitempty"`        // window key for allowed_regimes gate; "" or "default" = legacy single lookback (#792)
	RegimeATRWindow         string                   `json:"regime_atr_window,omitempty"`         // window key for *_atr_regime resolution (#792)
	RegimeDirectionalWindow string                   `json:"regime_directional_window,omitempty"` // window key for regime_directional_policy (#792)
	Capital                 float64                  `json:"capital"`
	CapitalPct              float64                  `json:"capital_pct,omitempty"`     // 0-1; dynamic capital = wallet_balance * capital_pct (overrides capital)
	InitialCapital          float64                  `json:"initial_capital,omitempty"` // fixed starting balance for PnL display (never overwritten by capital_pct)
	MaxDrawdownPct          float64                  `json:"max_drawdown_pct"`
	CircuitBreaker          *bool                    `json:"circuit_breaker,omitempty"`            // #1048 — per-strategy circuit-breaker opt-out. Nil/missing → enabled (the safe default); explicit false disables BOTH firing arms in CheckRisk (drawdown > max_drawdown_pct AND 5 consecutive losses), uniformly for live and paper (no platform/live gating). Hot-reloadable via SIGHUP including while a position is open: disabling only suppresses NEW fires — an already-latched CB and any pending circuit close still drain. No effect on type=manual (exempt from CheckRisk). Read via CircuitBreakerEnabled(), never directly.
	NotifyRatchetTriggers   *bool                    `json:"notify_ratchet_triggers,omitempty"`    // #1118 — per-strategy override of the global notify_ratchet_triggers (#1110) ratchet-tighten owner DM. Nil/missing → inherit the global Config.NotifyRatchetTriggersEnabled(); explicit value wins. Notification-only (never affects position/order state), so SIGHUP hot-reloads it unconditionally even while a position is open. Read via NotifyRatchetTriggersEnabled(cfg), never directly.
	LLMEntryAnalysis        *LLMEntryAnalysisConfig  `json:"llm_entry_analysis,omitempty"`         // #1137 — optional post-open LLM multi-agent entry analysis (advisory-only commentary; never gates/sizes/closes anything). Default off. Runs async on a dedicated lane after a FRESH position-open (not adds/flips/manual), posts a digest to the strategy's trade-alert DM by default (notify_dm on / notify_channel off; both per-strategy overridable), and stamps the verdict for trade_diagnostics.llm_verdict. Notification-only, so SIGHUP hot-reloads it unconditionally even while a position is open. Read via LLMEntryAnalysisEnabled()/resolveLLMEntryAnalysisParams().
	Paused                  bool                     `json:"paused,omitempty"`                     // #1150 — per-strategy pause. The strategy stays in dueStrategies and runs its full cycle (manage-only, mirroring the #1046 latched-CB shape), but position-INCREASING signals are forced to hold via pausedBlocksSignal: fresh opens, scale-in adds, and bidirectional flips. Position-REDUCING actions pass through — close-registry actions (closeFraction>0) and pure-close directional exits — so an open position rides its natural exit; trailing SL, ratchet, protection sync, and paper SL/TP simulation all keep running on the Signal==0 manage path. Hot-reloadable via SIGHUP unconditionally, including while a position is open (pausing never strands protection). No effect on type=manual (no open signal to suppress; the manual dispatch is pure management).
	IntervalSeconds         int                      `json:"interval_seconds,omitempty"`           // per-strategy override (0 = use global)
	HTFFilter               bool                     `json:"htf_filter,omitempty"`                 // higher-timeframe trend filter
	InvertSignal            bool                     `json:"invert_signal,omitempty"`              // HL perps/manual only: flip BUY<->SELL on a non-zero signal before execution (HOLD/0 is never flipped). Lets inverse variants reuse the same open/close refs. Composes with Direction — invert runs in the Go layer before direction interprets the resulting sign (e.g. direction="short" + invert_signal=true opens short on raw-BUY triggers, distinct from plain direction="short" which opens on raw-SELL). Rejected outside HL perps/manual.
	AllowShorts             bool                     `json:"allow_shorts,omitempty"`               // DEPRECATED — use Direction. Perps only; legacy boolean retained on the struct so pre-v14 JSON unmarshals cleanly. Read via EffectiveDirection / PerpsAllowsShort / PerpsAllowsLong, never directly. Migrated to Direction in v14 (#656).
	Direction               string                   `json:"direction,omitempty"`                  // perps only: "long" (default; signal=1 opens, signal=-1 closes long), "short" (signal=-1 opens, signal=1 closes short), "both" (bidirectional). Empty falls back to AllowShorts (legacy). v14 migration converts allow_shorts→direction. (#656)
	Leverage                float64                  `json:"leverage,omitempty"`                   // perps exchange leverage (default 1 = no leverage); used for exchange margin/risk and HL update_leverage (#254/#497)
	SizingLeverage          float64                  `json:"sizing_leverage,omitempty"`            // perps notional multiplier; defaults to Leverage for backwards compatibility (#497). Notional formula: notional = cash * sizing_leverage; size = notional / price. For margin-based sizing, prefer MarginPerTradeUSD (#518).
	MarginPerTradeUSD       *float64                 `json:"margin_per_trade_usd,omitempty"`       // perps only: USD margin to deploy per open. When set (positive), overrides SizingLeverage: notional = min(MarginPerTradeUSD, cash) * exchange_leverage; size = notional / price. Lets operators size in margin-space directly so high exchange_leverage doesn't decouple intent from outcome (#518).
	StopLossPct             *float64                 `json:"stop_loss_pct,omitempty"`              // HL perps only: % from entry to place a reduce-only stop-loss trigger. Pointer so omitted (nil) falls through to StopLossMarginPct then MaxDrawdownPct for single-coin strategies (#484); LoadConfig normalizes omitted same-coin peers to explicit 0 (#494); explicit 0 disables auto-SL (#412)
	StopLossMarginPct       *float64                 `json:"stop_loss_margin_pct,omitempty"`       // HL perps only: % of deployed margin to lose before stop-loss trigger; mutually exclusive with stop_loss_pct; price % derived as StopLossMarginPct / Leverage at order time. Pointer so omitted falls through to MaxDrawdownPct for single-coin strategies; LoadConfig normalizes omitted same-coin peers to explicit 0 (#494); explicit 0 disables (#487, #484)
	TrailingStopPct         *float64                 `json:"trailing_stop_pct,omitempty"`          // HL perps only: synthetic trailing SL distance from the best mark seen while open; mutually exclusive with stop_loss_pct and stop_loss_margin_pct (#501)
	TrailingStopATRMult     *float64                 `json:"trailing_stop_atr_mult,omitempty"`     // HL perps only: trailing SL distance derived from entry ATR at open (effective_pct = mult * entry_atr / avg_cost * 100); fixed for the life of the position; mutually exclusive with trailing_stop_pct, stop_loss_pct, stop_loss_margin_pct (#505)
	StopLossATRMult         *float64                 `json:"stop_loss_atr_mult,omitempty"`         // HL perps only: fixed (non-trailing) SL distance derived from entry ATR at open (trigger_px = avg_cost ± mult * entry_atr); armed once on the cycle after open and never updated; mutually exclusive with stop_loss_pct, stop_loss_margin_pct, trailing_stop_pct, trailing_stop_atr_mult. When all five stop fields are omitted on a sole-owner HL perps strategy, LoadConfig defaults this to 1.0 so every position has volatility-adjusted exchange-side protection (#562)
	StopLossATRRegime       *RegimeATRBlock          `json:"stop_loss_atr_regime,omitempty"`       // HL perps only: regime-aware sibling of stop_loss_atr_mult — resolves the ATR multiplier from pos.Regime stamped at open. Mutually exclusive with the four scalar siblings AND stop_loss_atr_mult. Requires regime detection enabled at the top-level cfg.Regime. (#733)
	TrailingStopATRRegime   *RegimeATRBlock          `json:"trailing_stop_atr_regime,omitempty"`   // HL perps only: regime-aware sibling of trailing_stop_atr_mult — trailing distance frozen at open via pos.Regime. Mutually exclusive with the scalar siblings. Requires regime detection. (#733)
	TrailingStopMinMovePct  *float64                 `json:"trailing_stop_min_move_pct,omitempty"` // HL perps trailing SL only: minimum trigger-price move before cancel/replace; nil defaults to 0.5% (#501)
	MarginMode              string                   `json:"margin_mode,omitempty"`                // HL perps only: "isolated" (default) or "cross"; sent via update_leverage on fresh opens to enforce per-position liq isolation (#486)
	ThetaHarvest            *ThetaHarvestConfig      `json:"theta_harvest,omitempty"`
	FuturesConfig           *FuturesConfig           `json:"futures,omitempty"`
	RegimeDirectionalPolicy *RegimeDirectionalPolicy `json:"regime_directional_policy,omitempty"` // HL perps only: regime-aware override for Direction + InvertSignal. When set, runHyperliquidCheck resolves the effective pair per-cycle from the current regime (when flat) or pos.Regime (when an open position is held — "hold until natural exit" semantics). Static Direction/InvertSignal are the base; the policy overrides per regime. Requires regime detection enabled at top-level cfg.Regime. (#779)
	RegimeWindowDivergence  *RegimeWindowDivergence  `json:"regime_window_divergence,omitempty"`  // HL perps live only: detect divergence between two regime windows (short vs medium) and optionally override effective direction when they hard-diverge. Builds on regime_directional_policy surface (#907).
	RegimeProfileAllocation *RegimeProfileAllocation `json:"regime_profile_allocation,omitempty"` // HL perps only: slow regime switch between two validated open_strategy param profiles. A long-window regime label (from the #879 store) selects the active profile; switching is hysteretic (confirm_bars closed bars) and flat-only. Requires regime.enabled=true. Backtester replays the switch. (#998)
	AllowScaleIn            bool                     `json:"allow_scale_in,omitempty"`            // HL perps/manual only: opt in to scale-in / pyramiding — a same-direction signal on an open position ADDS size (blends price+size, freezes EntryATR/regime/TP geometry) instead of being skipped. Default false preserves the legacy skip-on-same-direction behavior for every strategy that does not opt in. Gated by ScaleIn caps + spacing. (#873)
	ScaleIn                 *ScaleInConfig           `json:"scale_in,omitempty"`                  // scale-in tuning; only consulted when AllowScaleIn is true. Nil = defaults (unlimited adds/notional, no spacing, per-add size = standard open notional). (#873)
}

// ScaleInConfig tunes the opt-in scale-in / pyramiding path (#873). All fields
// are optional. Consulted only when StrategyConfig.AllowScaleIn is true.
type ScaleInConfig struct {
	// MaxAdds caps the number of add legs per position (0 = unlimited). A fresh
	// open is not an add; the first add takes ScaleInCount 0→1.
	MaxAdds int `json:"max_adds,omitempty"`
	// MaxAddedNotionalUSD caps the cumulative USD notional added across all add
	// legs of a position (0 = unlimited). The initial open notional does not
	// count against this cap — only subsequent adds.
	MaxAddedNotionalUSD float64 `json:"max_added_notional_usd,omitempty"`
	// AddSpacingATR is the signed price-move requirement, in multiples of the
	// frozen EntryATR, before the next add is allowed (measured from the last
	// entry leg's fill price). >0 = add-to-winners (price must have moved
	// in-favor by N×EntryATR); <0 = average-down (price must have moved adverse
	// by |N|×EntryATR); 0 = no spacing gate (add on every same-direction signal
	// up to the caps). Requires a positive frozen EntryATR to evaluate.
	AddSpacingATR float64 `json:"add_spacing_atr,omitempty"`
	// AddNotionalUSD is the USD notional to add per leg (0 = default to the
	// strategy's standard open notional, i.e. the same sizing a fresh open uses).
	AddNotionalUSD float64 `json:"add_notional_usd,omitempty"`
}

// UnmarshalJSON parses a StrategyConfig while accepting both the canonical
// single `close_strategy` ref and the legacy `close_strategies` array (#842).
// The array model (max close_fraction wins across N peers) was collapsed to a
// single profit-taking close: a length-1 legacy array is lifted into
// CloseStrategy; a length>1 array is retained in closeStrategiesLegacy so
// validateConfig can reject it with the strategy id (the operator must pick one
// close and move risk backstops to the strategy level). An explicit
// `close_strategy` always wins over a legacy array if both are somehow present.
func (sc *StrategyConfig) UnmarshalJSON(data []byte) error {
	type alias StrategyConfig // shed UnmarshalJSON to avoid infinite recursion
	aux := struct {
		*alias
		LegacyCloses []StrategyRef `json:"close_strategies"`
	}{alias: (*alias)(sc)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if sc.CloseStrategy == nil && len(aux.LegacyCloses) > 0 {
		sc.closeStrategiesLegacy = aux.LegacyCloses
		if len(aux.LegacyCloses) == 1 {
			ref := aux.LegacyCloses[0]
			sc.CloseStrategy = &ref
		}
	}
	return nil
}

// closeRefs returns the strategy's close evaluator as a 0-or-1 element slice.
// Post-#842 a strategy has at most one close, but many call sites still scan
// "the close refs" looking for a tiered-TP evaluator; this adapter lets those
// loops stay correct against the single-close model without special-casing nil.
func (sc StrategyConfig) closeRefs() []StrategyRef {
	if sc.CloseStrategy == nil {
		return nil
	}
	return []StrategyRef{*sc.CloseStrategy}
}

// cloneCloseStrategyRef deep-copies a close ref (including its params map) so
// callers that hand a StrategyConfig to the UI/reload layers don't alias the
// live config's pointer/map. Returns nil for a nil ref (open-as-close).
func cloneCloseStrategyRef(ref *StrategyRef) *StrategyRef {
	if ref == nil {
		return nil
	}
	out := StrategyRef{Name: ref.Name}
	if len(ref.Params) > 0 {
		out.Params = make(map[string]interface{}, len(ref.Params))
		for k, v := range ref.Params {
			out.Params[k] = v
		}
	}
	return &out
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

// EffectiveMarginPerTradeUSD returns the configured margin-per-trade in USD,
// or 0 when unset / non-positive. When positive, callers should size from
// margin-space (margin × exchange_leverage = notional) instead of the legacy
// sizing_leverage × cash notional formula. Perps-only — returns 0 for any
// other strategy type because validation rejects the field elsewhere (#518).
func EffectiveMarginPerTradeUSD(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.MarginPerTradeUSD == nil {
		return 0
	}
	if *sc.MarginPerTradeUSD <= 0 {
		return 0
	}
	return *sc.MarginPerTradeUSD
}

// Direction enum constants for StrategyConfig.Direction (#656).
const (
	DirectionLong  = "long"
	DirectionShort = "short"
	DirectionBoth  = "both"
)

// EffectiveDirection returns the canonical direction for a perps or manual
// strategy: "long" (signal=1 opens, signal=-1 closes long), "short" (signal=-1
// opens, signal=1 closes short), or "both" (bidirectional). Empty Direction
// falls back to AllowShorts (legacy pre-v14): false→"long", true→"both".
// Non-perps/manual strategies always return "long" — direction is meaningful
// only for perps and manual (which trades HL perps via the manual-open CLI),
// and validation rejects Direction on other types. (#656)
func EffectiveDirection(sc StrategyConfig) string {
	if sc.Type != "perps" && sc.Type != "manual" {
		return DirectionLong
	}
	switch sc.Direction {
	case DirectionLong, DirectionShort, DirectionBoth:
		return sc.Direction
	}
	if sc.AllowShorts {
		return DirectionBoth
	}
	return DirectionLong
}

// PerpsAllowsLong reports whether the strategy may open long positions —
// i.e. EffectiveDirection is "long" or "both". (#656)
func PerpsAllowsLong(sc StrategyConfig) bool {
	d := EffectiveDirection(sc)
	return d == DirectionLong || d == DirectionBoth
}

// PerpsAllowsShort reports whether the strategy may open short positions —
// i.e. EffectiveDirection is "short" or "both". (#656)
func PerpsAllowsShort(sc StrategyConfig) bool {
	d := EffectiveDirection(sc)
	return d == DirectionShort || d == DirectionBoth
}

// directionFromAllowShorts is the legacy bool→direction mapping used by
// migration and by call sites that still pass the bool. (#656)
func directionFromAllowShorts(allowShorts bool) string {
	if allowShorts {
		return DirectionBoth
	}
	return DirectionLong
}

// PerpsOpenNotional is the primitive sizing helper: returns the USD notional
// to open a perps position given primitive inputs. When marginPerTradeUSD is
// positive, the formula is margin-based: min(marginPerTradeUSD, cash) ×
// exchangeLeverage — matching the operator's mental model of "deploy $X as
// margin per trade" regardless of how high exchange_leverage is set (#518).
// Otherwise the legacy notional formula applies: cash × sizingLeverage. The
// hardcoded 0.95 safety buffer was removed in #518 — operators wanting headroom
// should set a smaller sizing_leverage (or margin_per_trade_usd) explicitly.
//
// Returns 0 when cash <= 0; callers must still guard for non-positive notional
// (e.g. flip path with realized loss) before placing an order.
func PerpsOpenNotional(cash, sizingLeverage, exchangeLeverage, marginPerTradeUSD float64) float64 {
	if cash <= 0 {
		return 0
	}
	if marginPerTradeUSD > 0 {
		margin := marginPerTradeUSD
		if margin > cash {
			margin = cash
		}
		if exchangeLeverage <= 0 {
			exchangeLeverage = 1
		}
		return margin * exchangeLeverage
	}
	if sizingLeverage <= 0 {
		sizingLeverage = 1
	}
	return cash * sizingLeverage
}

// ComputePerpsOpenNotional is the StrategyConfig-aware wrapper around
// PerpsOpenNotional, resolving the three sizing inputs from the strategy
// config. See PerpsOpenNotional for the formula.
func ComputePerpsOpenNotional(sc StrategyConfig, cash float64) float64 {
	return PerpsOpenNotional(cash, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc))
}

// MaxAutoStopLossPct caps the auto-derived per-trade stop at 50% to mirror the
// hand-edited bound enforced on StopLossPct (#421). MaxDrawdownPct can default
// to 50–60 across platforms; using it raw as a price stop would land triggers
// at entry×0 / entry×2 on long/short legs.
const MaxAutoStopLossPct = 50.0

// DefaultStopLossATRMult is the fallback value for Config.DefaultStopLossATRMult
// when the top-level config omits default_stop_loss_atr_mult. 1.0× ATR gives a
// sensible volatility-adjusted exchange-side stop on fresh opens without any
// operator config (#562/#605).
const DefaultStopLossATRMult = 1.0

// EffectiveStopLossPct returns the price % to use as the HL reduce-only stop-loss
// trigger for a given strategy. Resolution order (#484):
//  1. Explicit TrailingStopATRMult > 0 returns 0 because the price % can only
//     be derived once a position carries EntryATR and AvgCost — initial
//     trigger placement is deferred to the next trailing-stop cycle (#505).
//     Explicit 0 falls through to the next priority instead of short-
//     circuiting; a config like {trailing_stop_atr_mult: 0, stop_loss_pct: 2}
//     is rare but well-defined and the explicit fixed stop should still arm.
//  2. Explicit StopLossATRMult > 0 returns 0 for the same reason as
//     TrailingStopATRMult — the per-position EntryATR/AvgCost are required
//     to derive the price %, so initial trigger placement is deferred to the
//     next cycle once stampEntryATRIfOpened has populated Position.EntryATR (#562).
//     Explicit 0 falls through.
//  3. Explicit TrailingStopPct (nil → fall through; explicit 0 → disabled).
//  4. Explicit StopLossPct (nil → fall through; explicit 0 → disabled).
//  5. StopLossMarginPct / Leverage (nil → fall through; explicit 0 → disabled).
//  6. MaxDrawdownPct as a fallback for any HL perps strategy where all five
//     stop fields are nil. Capped at MaxAutoStopLossPct. Rarely reached in
//     practice because LoadConfig defaults all-five-omitted strategies
//     (including shared-coin peers since #601) to Config.DefaultStopLossATRMult
//     (#562/#605); only strategies that opt out via default_stop_loss_atr_mult=0
//     (or an explicit per-strategy stop_loss_atr_mult=0 with no other stop
//     field set) can reach this fallback.
//
// HL perps only — returns 0 for non-HL platforms or non-perps types so the
// caller can skip the trigger placement unconditionally.
func EffectiveStopLossPct(sc StrategyConfig) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		// #841 2b: the unified close owns an ATR-based SL armed on the cycle
		// after open (same deferral as stop_loss_atr_regime). Returning 0 here
		// avoids falling through to the max-drawdown pct fallback below.
		return 0
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		// ATR-derived trailing stop. The price % depends on per-position
		// EntryATR and AvgCost which are not available at order placement
		// time (the position record is created after the fill). The trailing
		// stop loop arms the initial trigger on the next cycle once
		// stampEntryATRIfOpened has populated Position.EntryATR (#505).
		return 0
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		// Fixed (non-trailing) ATR-derived stop loss. Same deferral as
		// TrailingStopATRMult — the trigger is armed on the cycle after
		// open by hyperliquidArmFixedATRStopLoss once EntryATR is stamped (#562).
		return 0
	}
	if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		// #733: regime-aware fixed SL. Same deferral as the scalar ATR
		// variants — initial trigger placement is deferred to the next
		// cycle once both Position.EntryATR AND Position.Regime are stamped.
		return 0
	}
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
		// #733: regime-aware trailing distance. Same deferral story.
		return 0
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return *sc.TrailingStopPct
		}
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

// LoadConfig loads and validates a config file. Live-mode strategies require
// platform credential env vars to be set in the process environment.
func LoadConfig(path string) (*Config, error) {
	return loadConfig(path, false)
}

// LoadConfigForProbe loads config for `go-trader probe` / update.sh pre-swap
// validation. Skips live-credential env checks (#787): probe only verifies the
// Python argv contract and never connects to exchanges; secrets may live only in
// the running process (systemd EnvironmentFile, etc.).
func LoadConfigForProbe(path string) (*Config, error) {
	return loadConfig(path, true)
}

func loadConfig(path string, skipLiveCredentialChecks bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// #640: v13 introduced co-located StrategyRef shape, which is a type-changing
	// migration json.Unmarshal cannot do on its own. Detect pre-v13 configs and
	// run the schema rewrite synchronously before parsing — MigrateConfig writes
	// the migrated JSON back to disk so downstream loads see the new shape and
	// the async DM-based field migration (runConfigMigrationDM) finds the file
	// already at the current version.
	if needsV13SchemaMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v13 schema migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v13 migration: %w", err)
		}
	}
	// #841: v15 rewrites close-strategy keys on disk (tiers→tp_tiers, unified
	// regime block, tp_at_pct→tiered_tp_pct). Run synchronously before parse
	// so validation sees canonical keys after alias reads are dropped.
	if needsV15CloseMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v15 close-key migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v15 migration: %w", err)
		}
	}
	// #1135: v16 consolidates operator defaults under user_defaults and rewrites
	// the legacy top-level user_close_defaults/manual_defaults aliases on disk so
	// the runtime has exactly one operator-defaults tree.
	if needsV16UserDefaultsMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v16 user-defaults migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v16 user-defaults migration: %w", err)
		}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// #704: flag unknown per-strategy fields (typos like `take_profit_atr_mult`)
	// before applying defaults; json.Unmarshal silently drops them and would
	// otherwise produce a struct indistinguishable from "no protection configured".
	unknownErrs := validateStrategyJSONKeys(data)
	unknownErrs = append(unknownErrs, validateUserDefaultsJSONKeys(data)...)
	if len(unknownErrs) > 0 {
		return nil, fmt.Errorf("config validation errors:\n  %s", strings.Join(unknownErrs, "\n  "))
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
	if cfg.DefaultStopLossATRMult == nil {
		defaultMult := DefaultStopLossATRMult
		cfg.DefaultStopLossATRMult = &defaultMult
	}
	if *cfg.DefaultStopLossATRMult < 0 {
		return nil, fmt.Errorf("default_stop_loss_atr_mult must be >= 0, got %g", *cfg.DefaultStopLossATRMult)
	}

	if md := cfg.userDefaultsManual(); md != nil {
		if md.MarginUSD != nil && *md.MarginUSD <= 0 {
			return nil, fmt.Errorf("user_defaults.manual.margin_usd must be > 0, got %g", *md.MarginUSD)
		}
		if md.StopLossATRMult != nil && *md.StopLossATRMult < 0 {
			return nil, fmt.Errorf("user_defaults.manual.stop_loss_atr_mult must be >= 0, got %g", *md.StopLossATRMult)
		}
		if md.Side != "" && md.Side != "long" && md.Side != "short" {
			return nil, fmt.Errorf("user_defaults.manual.side must be lowercase \"long\" or \"short\", got %q", md.Side)
		}
		// Reject empty tp_tiers array: omitting the field falls back to the
		// hardcoded default, but writing `"tp_tiers": []` looks intentional
		// (operator trying to disable tiered TPs) and would silently revert
		// to the default — surface the misuse loudly instead.
		if md.TPTiers != nil && len(md.TPTiers) == 0 {
			return nil, fmt.Errorf("user_defaults.manual.tp_tiers must have at least one tier (omit the field to use defaults)")
		}
		for i, t := range md.TPTiers {
			if t.ATRMultiple <= 0 {
				return nil, fmt.Errorf("user_defaults.manual.tp_tiers[%d].atr_multiple must be > 0, got %g", i, t.ATRMultiple)
			}
			if t.CloseFraction <= 0 || t.CloseFraction > 1 {
				return nil, fmt.Errorf("user_defaults.manual.tp_tiers[%d].close_fraction must be in (0, 1], got %g", i, t.CloseFraction)
			}
		}
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
		normalizeDeprecatedCloseRef(cfg.Strategies[i].CloseStrategy)
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

	// #1133: user_defaults.close["trailing_tp_ratchet_regime"] may carry the
	// coupled strategy-level trailing_stop_atr_regime owner. Apply it before the
	// generic scalar ATR-stop default below so eligible ratchet-regime perps do not
	// first acquire stop_loss_atr_mult and then fail the single-owner validation.
	applyUserCloseDefaultRatchetRegimeTrails(&cfg)

	// #562/#601/#605: Default HL perps strategies with no explicit stop-loss /
	// trailing-stop fields to the configurable top-level
	// default_stop_loss_atr_mult (1.0× ATR by default). Volatility-adjusted
	// exchange-side protection out of the box. Shared-coin peers are included
	// because #601 places per-strategy sized reduce-only orders instead of one
	// shared trigger owner. An explicit default_stop_loss_atr_mult=0 opts out
	// of the auto-default entirely so the per-strategy MaxDrawdownPct fallback
	// in EffectiveStopLossPct stays in play.
	defaultStopLossATRMult := *cfg.DefaultStopLossATRMult
	if defaultStopLossATRMult > 0 {
		for i := range cfg.Strategies {
			sc := &cfg.Strategies[i]
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				continue
			}
			// LoadConfig runs BEFORE ResolveSurface populates the typed
			// UseDefaults/TrendRegime fields, so the raw-aware IsConfigured()
			// is the correct predicate here — IsZero() would return true on a
			// freshly-unmarshaled regime block and cause the scalar default to
			// be applied on top, triggering a spurious mutex error in
			// validateRegimeATRConfig (review #735.1).
			if sc.StopLossPct == nil && sc.StopLossMarginPct == nil && sc.TrailingStopPct == nil && sc.TrailingStopATRMult == nil && sc.StopLossATRMult == nil && !sc.StopLossATRRegime.IsConfigured() && !sc.TrailingStopATRRegime.IsConfigured() && !strategyUsesUnifiedRegimeClose(*sc) {
				defaultMult := defaultStopLossATRMult
				sc.StopLossATRMult = &defaultMult
				fmt.Printf("[INFO] %s: applied default stop_loss_atr_mult=%g (no stop fields set; set stop_loss_atr_mult=0 or default_stop_loss_atr_mult=0 to opt out)\n", sc.ID, defaultStopLossATRMult)
			}
		}
	}

	// #569: Apply defaults for type=manual HL strategies: auto-set script/args,
	// default close_strategies, default stop_loss_atr_mult, default TP tiers.
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Type != "manual" || sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Script == "" {
			sc.Script = "shared_scripts/check_hyperliquid.py"
		}
		if len(sc.Args) == 0 && sc.Symbol != "" && sc.Timeframe != "" {
			mode := "live"
			sc.Args = []string{"hold", sc.Symbol, sc.Timeframe, "--mode=" + mode}
		}
		if sc.Leverage > 0 && sc.SizingLeverage == 0 {
			sc.SizingLeverage = sc.Leverage
		}
		if sc.MarginMode == "" {
			sc.MarginMode = "isolated"
		}
		if sc.CloseStrategy == nil {
			// #1115: when regime detection is enabled and the active classifier's
			// vocabulary maps cleanly onto the default per-regime opening-trail
			// baseline, default manual closes to the regime-adaptive trailing
			// take-profit ratchet (trailing_tp_ratchet_regime) so the trail width
			// tracks volatility per regime. The synthesized trailing_stop_atr_regime
			// block becomes the SL owner — the scalar stop_loss_atr_mult default
			// below then self-suppresses via its !TrailingStopATRRegime.IsConfigured()
			// guard, so it MUST be attached here (before that check runs). Falls back
			// to today's tiered_tp_atr_live whenever regime is off or any label can't
			// resolve, leaving a regime-less config unchanged. The choice is logged
			// (never silently divergent in a protection path).
			if block, ok := cfg.resolveManualRatchetRegimeTrailBlock(*sc); ok {
				sc.CloseStrategy = &StrategyRef{Name: trailingTPRatchetRegimeCloseName}
				sc.TrailingStopATRRegime = block
				fmt.Printf("[INFO] %s: manual close defaulted to %s (regime enabled; trailing_stop_atr_regime owns the per-regime trail/SL)\n", sc.ID, trailingTPRatchetRegimeCloseName)
			} else {
				sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_live"}
				if cfg.Regime != nil && cfg.Regime.Enabled {
					fmt.Printf("[INFO] %s: manual close defaulted to tiered_tp_atr_live (regime enabled, but kept the scalar default — an explicit stop field is set or the classifier vocabulary has no default per-regime trail)\n", sc.ID)
				} else {
					fmt.Printf("[INFO] %s: manual close defaulted to tiered_tp_atr_live (regime disabled)\n", sc.ID)
				}
			}
		}
		// #691/#696: type=manual gets its own SL default (2.0× ATR by default,
		// overridable via user_defaults.manual.stop_loss_atr_mult) so non-manual
		// perps strategies stay on the fleet-wide default_stop_loss_atr_mult
		// (typically 1.0×). Skip if any explicit stop field is set so peers
		// and operator overrides still win. Honor the fleet-wide
		// default_stop_loss_atr_mult=0 opt-out: when the operator disables
		// the auto-default globally, manual strategies opt out too (the
		// INFO message at config.go:675 advertises =0 as the global switch).
		// Same raw-aware predicate as the perps default loop above —
		// IsConfigured covers the pre-ResolveSurface phase (review #735.1).
		if defaultStopLossATRMult > 0 && sc.StopLossATRMult == nil && sc.StopLossPct == nil && sc.StopLossMarginPct == nil && sc.TrailingStopPct == nil && sc.TrailingStopATRMult == nil && !sc.StopLossATRRegime.IsConfigured() && !sc.TrailingStopATRRegime.IsConfigured() {
			defaultMult := cfg.resolveManualStopLossATRMult()
			if defaultMult > 0 {
				sc.StopLossATRMult = &defaultMult
			}
		}
		// #696: Default TP tiers for manual strategies onto the close ref,
		// overridable via user_defaults.manual.tp_tiers. Only the tiered_tp_atr*
		// close evaluators consume `tp_tiers`; if the operator overrode
		// close_strategy to something else, leave it alone.
		if cs := sc.CloseStrategy; cs != nil && isTieredTPATRCloseName(cs.Name) &&
			cs.Name != "tiered_tp_atr_regime" && cs.Name != "tiered_tp_atr_live_regime" &&
			cs.Name != dynamicCloseStrategyName {
			// Regime-aware variants resolve their own tier list from the
			// trend_regime block / use_defaults shortcut — user_defaults.manual
			// tier seeding doesn't apply.
			if cs.Params == nil {
				cs.Params = map[string]interface{}{}
			}
			if _, hasTP := closeTierListParam(cs.Params); !hasTP {
				cs.Params["tp_tiers"] = cfg.resolveManualTPTiers()
			}
		}
	}

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

	// Regime detection defaults. Defaults are only injected when Enabled=true so
	// that an explicit zero in a disabled block (e.g. {"period": 0}) round-trips
	// instead of being silently rewritten to 14.
	if cfg.Regime == nil {
		cfg.Regime = &RegimeConfig{Enabled: false}
	}
	cfg.Regime.Timeframe = normalizeRegimeTimeframe(cfg.Regime.Timeframe)
	if cfg.Regime.Enabled {
		if cfg.Regime.Period == 0 {
			cfg.Regime.Period = 14
		}
		if cfg.Regime.ADXThreshold == 0 {
			cfg.Regime.ADXThreshold = 20.0
		}
	}

	if cfg.Correlation.MaxConcentrationPct == 0 {
		cfg.Correlation.MaxConcentrationPct = 60
	}
	if cfg.Correlation.MaxSameDirectionPct == 0 {
		cfg.Correlation.MaxSameDirectionPct = 75
	}

	// #866: inject user_defaults.close into close refs that omit tp_tiers, after
	// all per-strategy close-ref normalization/auto-config is complete. The
	// strategy layer (explicit tp_tiers) still wins; refs with no matching entry
	// fall through to the evaluator's system default.
	applyUserCloseDefaults(&cfg)

	// #1134: inject user_defaults.regime_atr into standalone
	// stop_loss_atr_regime / trailing_stop_atr_regime owners that are
	// use_defaults-only. Runs after manual auto-config and close-ref
	// injection; skips ratchet/manual strategies.
	applyUserCloseDefaultRegimeATRs(&cfg)

	if err := validateConfig(&cfg, skipLiveCredentialChecks); err != nil {
		return nil, err
	}
	throttle, err := ParseAlertThrottleInterval(cfg.AlertThrottleInterval)
	if err != nil {
		return nil, err
	}
	applyAlertThrottleInterval(throttle)
	return &cfg, nil
}

// normalizeHyperliquidPeerStopLosses is retained for callers/tests that still
// reference the old #494 normalizer. It intentionally no-ops after #601:
// shared-coin HL perps strategies now place per-strategy sized reduce-only
// protection orders, so omitted stop fields should keep normal defaulting.
func normalizeHyperliquidPeerStopLosses(strategies []StrategyConfig) {
}

// hyperliquidPeerStrategyErrors returns validation messages for HL wallet
// strategies that share a coin but disagree on MarginMode or exchange Leverage (#491/#619).
// Returns an empty slice when no peer conflicts exist.
//
// HL aggregates positions per coin per account, so two go-trader strategies
// on the same coin share an on-chain position, margin assignment, and
// reduce-only order slots. Mismatched leverage/margin would either fail
// at first peer trade (HL rejects mode changes on an open position) or
// silently land in the wrong mode. Per-strategy bookkeeping in SQLite keeps
// the legs separated when peers agree.
//
// Sub-account isolation is the only correct path for full per-strategy
// independence (different direction, leverage, margin); it is intentionally
// out of scope here and tracked separately.
//
// Manual strategies participate in the same peer set as automated perps: HL
// still aggregates them into one on-chain position per coin, so they must
// agree on exchange leverage and margin mode even though their virtual state
// and close sizing are isolated.
//
// Note: Direction (#656) mismatches across peers on the same coin are NOT
// validated here. A direction="long" and a direction="short"/"both" peer on
// the same HL coin would silently net/flip at the position level —
// directional independence requires HL sub-accounts (out of scope for #491).
//
// Note: SizingLeverage is intentionally NOT required to match across peers
// (#497). It only affects per-strategy order sizing — exchange margin and
// liquidation are governed by the shared exchange Leverage, which IS
// required to match. Two peers can size their entries differently without
// any on-chain conflict.
func hyperliquidPeerStrategyErrors(strategies []StrategyConfig) []string {
	type peer struct {
		ID         string
		Coin       string
		MarginMode string
		Leverage   float64
	}
	groups := make(map[string][]peer)
	for _, sc := range strategies {
		if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
			continue
		}
		coin := hyperliquidConfiguredCoin(sc)
		if coin == "" {
			continue
		}
		groups[coin] = append(groups[coin], peer{
			ID:         sc.ID,
			Coin:       coin,
			MarginMode: sc.MarginMode,
			Leverage:   sc.Leverage,
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
	return validateConfig(cfg, false)
}

// regimeDirectionalPolicyWarnings returns one operator warning per strategy that selects
// trade side from the regime label (regime_directional_policy, #779). #1076 validated that
// premise — regime -> forward DIRECTION — and found it empirically false across BTC/ETH/SOL/
// BNB/XRP and five timeframes: 0 of 2121 per-state forward-return tests survive global
// Benjamini-Hochberg/Bonferroni correction, and a look-ahead-safe regime-timing book never
// beats its own block-shuffled-label null (0/60 after FDR). So this surface chooses long vs
// short on noise; its only realized effect is a change in exposure (defensive beta in a down
// sample), not a directional forecast. The warning is advisory and NON-BREAKING — existing
// live configs still load — because hard-rejecting the keys is the less safe option: a forced
// disable relies on the #822 orphan auto-close, which fires only for sole-owner coins
// (hyperliquid_balance.go), so a shared-coin live short would be stranded for manual close.
// Operators should disable from FLAT (SIGHUP blocks the change while a position is open,
// config_reload.go) and use the regime for ATR-scaled SL/TP sizing (#1078), its real signal.
// Returned (not printed) so the set is unit-testable; validateConfig prints them.
func regimeDirectionalPolicyWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, sc := range cfg.Strategies {
		if sc.RegimeDirectionalPolicy.IsConfigured() {
			out = append(out, fmt.Sprintf("[WARN] %s: regime_directional_policy selects long/short by regime, but the regime→forward-direction premise is empirically unvalidated (#1076 negative result). It is now DEFAULT-OFF / evidence-gated (#1085): the side resolves to base direction unless a per-(asset,timeframe,classifier) certification passes (none currently does). Prefer the regime for ATR-scaled SL/TP sizing (#1078); disable from flat.", sc.ID))
		}
	}
	return out
}

func validateConfig(cfg *Config, skipLiveCredentialChecks bool) error {
	var errs []string
	seenIDs := make(map[string]bool)

	// Validate leaderboard_post_time format if set.
	if cfg.LeaderboardPostTime != "" {
		if _, _, ok := ParseLeaderboardPostTime(cfg.LeaderboardPostTime); !ok {
			errs = append(errs, fmt.Sprintf("leaderboard_post_time must be in \"HH:MM\" format (24h UTC), got %q", cfg.LeaderboardPostTime))
		}
	}

	// #866/#1135: validate the user_defaults block shape. Tier *contents* are
	// validated per-strategy below once injected, against each consuming strategy's
	// regime vocabulary.
	errs = append(errs, validateUserDefaults(cfg.UserDefaults)...)

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

		// #34: Script path validation (manual strategies auto-set their script in LoadConfig).
		if sc.Type != "manual" {
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
		}

		// #36: Type must be "spot", "options", "perps", "futures", or "manual" (#569).
		if sc.Type != "spot" && sc.Type != "options" && sc.Type != "perps" && sc.Type != "futures" && sc.Type != "manual" {
			errs = append(errs, fmt.Sprintf("%s: type must be \"spot\", \"options\", \"perps\", \"futures\", or \"manual\", got %q", prefix, sc.Type))
		}
		// #1137: LLM entry-analysis block bounds (nil = feature off).
		errs = append(errs, validateLLMEntryAnalysis(prefix, sc)...)
		// #842: a strategy has at most one close. A legacy close_strategies
		// array with >1 entry no longer composes via max close_fraction —
		// reject it so the operator picks one profit-taking close and moves any
		// risk backstops to the strategy level.
		if len(sc.closeStrategiesLegacy) > 1 {
			names := make([]string, 0, len(sc.closeStrategiesLegacy))
			for _, ref := range sc.closeStrategiesLegacy {
				names = append(names, ref.Name)
			}
			errs = append(errs, fmt.Sprintf("%s: close_strategies has %d entries %v — the array model was collapsed to a single close_strategy (#842); keep one profit-taking close and move risk backstops (hard caps, time stops) to the strategy level", prefix, len(names), names))
		}
		// Options strategies don't compose a close evaluator yet. open_strategy
		// is allowed as canonical metadata (post-v13 it mirrors args[0]); only
		// close_strategy remains rejected here.
		if sc.CloseStrategy != nil && sc.Type == "options" {
			errs = append(errs, fmt.Sprintf("%s: close_strategy is supported for spot, perps, and futures strategies only", prefix))
		}
		if sc.OpenStrategy.Name != "" {
			if err := validateStrategyConceptName(sc.OpenStrategy.Name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: open_strategy %v", prefix, err))
			}
		}
		if sc.CloseStrategy != nil {
			if err := validateStrategyConceptName(sc.CloseStrategy.Name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: close_strategy %v", prefix, err))
			}
		}

		// allowed_regimes vocabulary is validated in validateStrategyRegimeVocabulary
		// against the classifier on regime_gate_window (#795).
		// The regime gate is not wired at the options dispatch site (#553), so
		// allowed_regimes is a silent no-op for options strategies. Reject it
		// here until the gate is properly implemented for the multi-position model.
		if sc.Type == "options" && len(sc.AllowedRegimes) > 0 {
			errs = append(errs, fmt.Sprintf("%s: allowed_regimes is not enforced for type=options (gate not wired at options dispatch; see issue #553)", prefix))
		}

		// #569: manual strategies require symbol + timeframe + leverage.
		if sc.Type == "manual" {
			if sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: type=manual is only supported for platform=hyperliquid", prefix))
			}
			if strings.TrimSpace(sc.Symbol) == "" {
				errs = append(errs, fmt.Sprintf("%s: type=manual requires symbol (e.g. \"ETH\")", prefix))
			}
			if strings.TrimSpace(sc.Timeframe) == "" {
				errs = append(errs, fmt.Sprintf("%s: type=manual requires timeframe (e.g. \"1h\")", prefix))
			}
			if sc.Leverage <= 0 {
				errs = append(errs, fmt.Sprintf("%s: type=manual requires leverage > 0", prefix))
			}
		}

		if !skipLiveCredentialChecks {
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
			if !skipLiveCredentialChecks && sc.CapitalPct > 0 && sc.Platform == "hyperliquid" {
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

		// #873: scale-in / pyramiding is opt-in and scoped to HL perps + manual
		// (live + paper). The blend math is platform-agnostic, but the on-chain
		// protection re-size is HL-specific and the dispatch wiring only covers
		// these two types — reject the flag elsewhere so an operator can't
		// silently enable a no-op.
		if sc.AllowScaleIn {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in is only supported for perps/manual strategies (got type %q)", prefix, sc.Type))
			}
			if sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in is only supported on hyperliquid (got platform %q)", prefix, sc.Platform))
			}
			// #873 (from #875): on HL LIVE perps the on-chain SL must be one the
			// scale-in resize path can grow — an ATR/regime fixed SL (sync
			// force-replace) or a trailing SL (walker forceResize). A static
			// scalar SL (stop_loss_pct / stop_loss_margin_pct / the max_drawdown
			// fallback) is placed once at open with no resize path, so after an
			// add it would silently under-cover the grown position. Reject it up
			// front rather than leave a naked-increment SL at runtime. Paper
			// places no on-chain orders; manual auto-configures an ATR SL.
			if sc.Type == "perps" && sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args) && !scaleInLiveProtectionResizable(sc) {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in on live perps requires an ATR/regime or trailing stop-loss that can be re-sized after an add — stop_loss_pct/stop_loss_margin_pct and the max_drawdown fallback cannot (set stop_loss_atr_mult, stop_loss_atr_regime, or a trailing stop)", prefix))
			}
		}
		if sc.ScaleIn != nil {
			if !sc.AllowScaleIn {
				errs = append(errs, fmt.Sprintf("%s: scale_in block is set but allow_scale_in is false — enable allow_scale_in or remove the block", prefix))
			}
			if sc.ScaleIn.MaxAdds < 0 {
				errs = append(errs, fmt.Sprintf("%s: scale_in.max_adds must be >= 0, got %d", prefix, sc.ScaleIn.MaxAdds))
			}
			if sc.ScaleIn.MaxAddedNotionalUSD < 0 {
				errs = append(errs, fmt.Sprintf("%s: scale_in.max_added_notional_usd must be >= 0, got %g", prefix, sc.ScaleIn.MaxAddedNotionalUSD))
			}
			if sc.ScaleIn.AddNotionalUSD < 0 {
				errs = append(errs, fmt.Sprintf("%s: scale_in.add_notional_usd must be >= 0, got %g", prefix, sc.ScaleIn.AddNotionalUSD))
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

		// invert_signal is only honored by runHyperliquidCheck — flipping a
		// signal at the Go layer only matters for HL perps/manual where the
		// executor consumes a numeric +1/-1/0. Spot/options/futures check
		// scripts emit their own buy/sell logic that runHyperliquidCheck
		// doesn't see, so the flag would be a silent no-op there. Reject
		// the config at startup rather than letting it appear to work.
		//
		// invert_signal composes cleanly with direction="short": the invert
		// runs before direction interprets the sign, so the combination opens
		// short on raw-BUY triggers (an "inverse short-only" strategy),
		// distinct from plain direction="short" which opens short on
		// raw-SELL. Both are valid (#775).
		if sc.InvertSignal {
			if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
				errs = append(errs, fmt.Sprintf("%s: invert_signal is only supported for HL perps/manual strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

		// regime_directional_policy: HL perps only (same surface as invert_signal
		// since both override the same fields runHyperliquidCheck consumes).
		// Requires regime detection enabled at top-level cfg.Regime — without it
		// result.Regime stays empty and the resolver always falls back to the
		// static base config, which silently defeats the policy. Reject the
		// asymmetric config at startup so the operator sees the gap. (#779)
		if sc.RegimeDirectionalPolicy.IsConfigured() {
			if sc.Platform != "hyperliquid" || sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: regime_directional_policy is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if cfg.Regime == nil || !cfg.Regime.Enabled {
				errs = append(errs, fmt.Sprintf("%s: regime_directional_policy requires top-level regime.enabled=true", prefix))
			}
			// Shape validation also runs in validateStrategyRegimeVocabulary (ADX labels when
			// regime.enabled=false so typos surface alongside the enabled=true error).
		}

		// regime_window_divergence: HL perps live only. Requires regime.enabled=true
		// and at least two windows configured. Shape validation runs in
		// validateStrategyRegimeVocabulary (ResolveRaw). (#907)
		if sc.RegimeWindowDivergence.IsConfigured() {
			if sc.Platform != "hyperliquid" || sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: regime_window_divergence is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if cfg.Regime == nil || !cfg.Regime.Enabled {
				errs = append(errs, fmt.Sprintf("%s: regime_window_divergence requires top-level regime.enabled=true", prefix))
			}
			if cfg.Regime != nil && cfg.Regime.Enabled && len(cfg.Regime.Windows) < 2 {
				errs = append(errs, fmt.Sprintf("%s: regime_window_divergence requires at least two windows in regime.windows", prefix))
			}
		}

		// regime_profile_allocation: HL perps only (live + paper). Requires
		// regime.enabled=true — the switch reads the global regime store, which
		// is only populated when regime detection is on. Shape validation
		// (param_sets count, label coverage, window existence) runs in
		// validateStrategyRegimeVocabulary (ResolveRaw). (#998)
		if sc.RegimeProfileAllocation.IsConfigured() {
			if sc.Platform != "hyperliquid" || sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: regime_profile_allocation is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if cfg.Regime == nil || !cfg.Regime.Enabled {
				errs = append(errs, fmt.Sprintf("%s: regime_profile_allocation requires top-level regime.enabled=true", prefix))
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
			manualRatchet := sc.Type == "manual" && strategyUsesTrailingTPRatchetClose(sc)
			if sc.Platform != "hyperliquid" || (sc.Type != "perps" && !manualRatchet) {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult is only supported for HL perps strategies or HL manual trailing_tp_ratchet strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
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
		slAfterLabels := canonicalTrendRegimeLabels
		if cfg.Regime != nil && cfg.Regime.Enabled {
			slAfterLabels = regimeLabelsForStrategyWindow(sc, cfg.Regime, "atr")
		}
		for _, msg := range validatePostTPStopLossRulesWithLabels(sc, slAfterLabels) {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, msg))
		}

		if sc.TrailingStopMinMovePct != nil {
			pct := *sc.TrailingStopMinMovePct
			if pct < 0 || pct > 100 {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct must be in [0, 100], got %g", prefix, pct))
			}
			manualRatchet := sc.Type == "manual" && strategyUsesTrailingTPRatchetClose(sc)
			if sc.Platform != "hyperliquid" || (sc.Type != "perps" && !manualRatchet) {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct is only supported for HL perps strategies or HL manual trailing_tp_ratchet strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			fixedTrailingPct := 0.0
			if sc.TrailingStopPct != nil {
				fixedTrailingPct = *sc.TrailingStopPct
			}
			atrMult := 0.0
			if sc.TrailingStopATRMult != nil {
				atrMult = *sc.TrailingStopATRMult
			}
			// #870: the regime ratchet owns its trail via trailing_stop_atr_regime
			// rather than the scalar trailing_stop_atr_mult, so accept that too.
			// #1111: use IsConfigured (raw-aware), NOT !IsZero() — this check runs
			// before validateRegimeATRConfig resolves the raw block, and IsZero()
			// reports true on an unresolved-but-configured block (see its doc), so
			// !IsZero() would wrongly reject a strategy that did set the regime trail.
			regimeTrail := sc.TrailingStopATRRegime.IsConfigured()
			if fixedTrailingPct <= 0 && atrMult <= 0 && !regimeTrail {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct requires trailing_stop_pct > 0, trailing_stop_atr_mult > 0, or trailing_stop_atr_regime", prefix))
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
		if tf := normalizeRegimeTimeframe(cfg.Regime.Timeframe); tf != "" && !validRegimeTimeframe(tf) {
			errs = append(errs, fmt.Sprintf("regime.timeframe must be one of %s, got %q", strings.Join(validRegimeTimeframes(), ", "), cfg.Regime.Timeframe))
		}
	}
	errs = append(errs, validateRegimeWindowsConfig(cfg)...)
	errs = append(errs, validateStrategyRegimeVocabulary(cfg)...)
	errs = append(errs, validateRegimeTransitionsConfig(cfg)...)

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

	// #1076: warn on the regime→direction selection surface (premise empirically refuted).
	for _, w := range regimeDirectionalPolicyWarnings(cfg) {
		fmt.Println(w)
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

	if _, err := ParseAlertThrottleInterval(cfg.AlertThrottleInterval); err != nil {
		errs = append(errs, err.Error())
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
