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

type DiscordConfig struct {
	Enabled            bool              `json:"enabled"`
	Token              string            `json:"token"`
	OwnerID            string            `json:"owner_id,omitempty"`
	DMChannels         map[string]string `json:"dm_channels,omitempty"`
	Channels           map[string]string `json:"channels"`
	TradeAlertChannels map[string]string `json:"trade_alert_channels,omitempty"`
	LeaderboardTopN    int               `json:"leaderboard_top_n,omitempty"`
	LeaderboardChannel string            `json:"leaderboard_channel,omitempty"`
	EphemeralReplies   bool              `json:"ephemeral_replies,omitempty"`
	ReportRepo         string            `json:"report_repo,omitempty"`
	ReportGitHubToken  string            `json:"report_github_token,omitempty"`
}

func (c DiscordConfig) reportRepo() string {
	if r := strings.TrimSpace(c.ReportRepo); r != "" {
		return r
	}
	return defaultReportRepo
}

func (c DiscordConfig) reportToken() string {
	if t := strings.TrimSpace(os.Getenv("GO_TRADER_GITHUB_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(c.ReportGitHubToken)
}

type TelegramConfig struct {
	Enabled            bool              `json:"enabled"`
	BotToken           string            `json:"bot_token"`
	OwnerChatID        string            `json:"owner_chat_id,omitempty"`
	DMChannels         map[string]string `json:"dm_channels,omitempty"`
	Channels           map[string]string `json:"channels"`
	TradeAlertChannels map[string]string `json:"trade_alert_channels,omitempty"`
}

type PortfolioRiskConfig struct {
	MaxDrawdownPct              float64 `json:"max_drawdown_pct"`
	MaxNotionalUSD              float64 `json:"max_notional_usd"`
	WarnThresholdPct            float64 `json:"warn_threshold_pct,omitempty"`
	DailyMaxLossUSD             float64 `json:"daily_max_loss_usd,omitempty"`
	DailyMaxLossPct             float64 `json:"daily_max_loss_pct,omitempty"`
	MaxSameDirectionNotionalUSD float64 `json:"max_same_direction_notional_usd,omitempty"`
	MaxAssetConcentrationPct    float64 `json:"max_asset_concentration_pct,omitempty"`

	Paper *PortfolioRiskConfig `json:"paper,omitempty"`
}

func scopeRiskConfig(cfg *Config, scope PortfolioScope) *PortfolioRiskConfig {
	if cfg == nil || cfg.PortfolioRisk == nil {
		return nil
	}
	parent := cfg.PortfolioRisk
	if scope != ScopePaper || parent.Paper == nil {
		return parent
	}
	merged := *parent
	merged.Paper = nil
	override := parent.Paper
	if override.MaxDrawdownPct != 0 {
		merged.MaxDrawdownPct = override.MaxDrawdownPct
	}
	if override.MaxNotionalUSD != 0 {
		merged.MaxNotionalUSD = override.MaxNotionalUSD
	}
	if override.WarnThresholdPct != 0 {
		merged.WarnThresholdPct = override.WarnThresholdPct
	}
	if override.DailyMaxLossUSD != 0 {
		merged.DailyMaxLossUSD = override.DailyMaxLossUSD
	}
	if override.DailyMaxLossPct != 0 {
		merged.DailyMaxLossPct = override.DailyMaxLossPct
	}
	if override.MaxSameDirectionNotionalUSD != 0 {
		merged.MaxSameDirectionNotionalUSD = override.MaxSameDirectionNotionalUSD
	}
	if override.MaxAssetConcentrationPct != 0 {
		merged.MaxAssetConcentrationPct = override.MaxAssetConcentrationPct
	}
	return &merged
}

type PlatformConfig struct {
	Risk *PortfolioRiskConfig `json:"risk,omitempty"`
}

type RegimeConfig struct {
	Enabled            bool                          `json:"enabled"`
	Period             int                           `json:"period"`
	ADXThreshold       float64                       `json:"adx_threshold"`
	Timeframe          string                        `json:"timeframe,omitempty"`
	Windows            RegimeWindowsMap              `json:"windows,omitempty"`
	DisplayWindows     []string                      `json:"display_windows,omitempty"`
	Transitions        *RegimeTransitionAlertsConfig `json:"transitions,omitempty"`
	GateOnFailure      string                        `json:"gate_on_failure,omitempty"`
	HurstGateOnFailure string                        `json:"hurst_gate_on_failure,omitempty"`
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

type CorrelationConfig struct {
	Enabled             bool    `json:"enabled"`
	MaxConcentrationPct float64 `json:"max_concentration_pct"`
	MaxSameDirectionPct float64 `json:"max_same_direction_pct"`
}

type LeaderboardSummaryConfig struct {
	Platform  string `json:"platform"`
	Ticker    string `json:"ticker,omitempty"`
	TopN      int    `json:"top_n,omitempty"`
	Channel   string `json:"channel"`
	Frequency string `json:"frequency,omitempty"`
}

type TradingViewExportConfig struct {
	SymbolOverrides map[string]string `json:"symbol_overrides,omitempty"`
}

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

func (lc LeaderboardSummaryConfig) Key() string {
	ticker := strings.ToLower(strings.TrimSpace(lc.Ticker))
	if ticker == "" {
		ticker = "*"
	}
	return fmt.Sprintf("%s:%s:%s", strings.ToLower(lc.Platform), ticker, lc.Channel)
}

type Config struct {
	ConfigVersion            int                        `json:"config_version,omitempty"`
	IntervalSeconds          int                        `json:"interval_seconds"`
	LogDir                   string                     `json:"log_dir"`
	DBFile                   string                     `json:"db_file,omitempty"`
	ReplayLogPath            string                     `json:"replay_log_path,omitempty"`
	StatusPort               int                        `json:"status_port,omitempty"`
	StatusToken              string                     `json:"-"`
	Discord                  DiscordConfig              `json:"discord"`
	Telegram                 TelegramConfig             `json:"telegram,omitempty"`
	AutoUpdate               string                     `json:"auto_update,omitempty"`
	LeaderboardPostTime      string                     `json:"leaderboard_post_time,omitempty"`
	Strategies               []StrategyConfig           `json:"strategies"`
	PortfolioRisk            *PortfolioRiskConfig       `json:"portfolio_risk,omitempty"`
	Correlation              *CorrelationConfig         `json:"correlation,omitempty"`
	Regime                   *RegimeConfig              `json:"regime,omitempty"`
	Platforms                map[string]*PlatformConfig `json:"platforms,omitempty"`
	LeaderboardSummaries     []LeaderboardSummaryConfig `json:"leaderboard_summaries,omitempty"`
	SummaryFrequency         map[string]string          `json:"summary_frequency,omitempty"`
	RiskFreeRate             *float64                   `json:"risk_free_rate,omitempty"`
	DefaultStopLossATRMult   *float64                   `json:"default_stop_loss_atr_mult,omitempty"`
	ATRMethod                string                     `json:"atr_method,omitempty"`
	NotifyTPSLFills          *bool                      `json:"notify_tp_sl_fills,omitempty"`
	NotifyRatchetTriggers    *bool                      `json:"notify_ratchet_triggers,omitempty"`
	AlertThrottleInterval    string                     `json:"alert_throttle_interval,omitempty"`
	KillSwitchResetDMTimeout string                     `json:"kill_switch_reset_dm_timeout,omitempty"`
	TradingViewExport        TradingViewExportConfig    `json:"tradingview_export,omitempty"`
	UserDefaults             *UserDefaultsConfig        `json:"user_defaults,omitempty"`
	Tuning                   *TuningConfig              `json:"tuning,omitempty"`

	migrationBaseVersion    int
	migrationBaseVersionSet bool
}

func (c *Config) MigrationBaseVersion() int {
	if c == nil {
		return CurrentConfigVersion
	}
	if c.migrationBaseVersionSet {
		return c.migrationBaseVersion
	}
	return c.ConfigVersion
}

type TuningConfig struct {
	MaxRetainedRuns int `json:"max_retained_runs,omitempty"`
}

type UserDefaultsConfig struct {
	Close     CloseDefaultsMap       `json:"close,omitempty"`
	RegimeATR map[string]interface{} `json:"regime_atr,omitempty"`
	Manual    *ManualDefaultsConfig  `json:"manual,omitempty"`
}

type CloseDefaultsMap map[string]map[string]interface{}

func (c *Config) userDefaultsClose() CloseDefaultsMap {
	if c == nil || c.UserDefaults == nil {
		return nil
	}
	return c.UserDefaults.Close
}

func (c *Config) tuningMaxRetainedRuns() int {
	if c == nil || c.Tuning == nil {
		return 0
	}
	return c.Tuning.MaxRetainedRuns
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

type ManualDefaultsConfig struct {
	MarginUSD       *float64       `json:"margin_usd,omitempty"`
	StopLossATRMult *float64       `json:"stop_loss_atr_mult,omitempty"`
	Side            string         `json:"side,omitempty"`
	TPTiers         []ManualTPTier `json:"tp_tiers,omitempty"`

	TrailingStopATRMultRegime *RegimeATRBlock `json:"trailing_stop_atr_mult_regime,omitempty"`
}

type ManualTPTier struct {
	ATRMultiple   float64 `json:"atr_multiple"`
	CloseFraction float64 `json:"close_fraction"`
}

func (c *Config) resolveManualMarginUSD() float64 {
	if md := c.userDefaultsManual(); md != nil && md.MarginUSD != nil {
		return *md.MarginUSD
	}
	return defaultManualMarginUSD
}

func (c *Config) resolveManualSide() string {
	if md := c.userDefaultsManual(); md != nil && md.Side != "" {
		return md.Side
	}
	return "long"
}

func (c *Config) resolveManualStopLossATRMult() float64 {
	if md := c.userDefaultsManual(); md != nil && md.StopLossATRMult != nil {
		return *md.StopLossATRMult
	}
	return defaultManualStopLossATRMult
}

func (c *Config) resolveManualRatchetFallbackATRMult() float64 {
	if md := c.userDefaultsManual(); md != nil && md.StopLossATRMult != nil && *md.StopLossATRMult > 0 {
		return *md.StopLossATRMult
	}
	return defaultManualStopLossATRMult
}

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

func (c *Config) resolveManualRatchetRegimeTrailBlock(sc StrategyConfig) (*RegimeATRBlock, bool) {
	if c == nil || c.Regime == nil || !c.Regime.Enabled {
		return nil, false
	}
	if sc.StopLossATRMult != nil || sc.StopLossPct != nil || sc.StopLossMarginPct != nil ||
		sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() {
		return nil, false
	}
	labels := regimeLabelsForStrategyWindow(sc, c.Regime, "atr")
	if len(labels) == 0 {
		return nil, false
	}
	if md := c.userDefaultsManual(); md != nil && md.TrailingStopATRMultRegime.IsConfigured() {
		if block := cloneRegimeATRBlock(md.TrailingStopATRMultRegime); block != nil {
			return block, true
		}
	}
	if block, ok := userCloseDefaultTrailingStopATRMultRegime(c.userDefaultsClose()); ok {
		return block, true
	}
	for _, label := range labels {
		if _, ok := mapRegimeToBaselineFamily(regimeATRDefaults.Trailing, label); !ok {
			return nil, false
		}
	}
	return &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}}, true
}

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

func (c *Config) NotifyTPSLFillsEnabled() bool {
	if c == nil || c.NotifyTPSLFills == nil {
		return true
	}
	return *c.NotifyTPSLFills
}

func (c *Config) NotifyRatchetTriggersEnabled() bool {
	if c == nil || c.NotifyRatchetTriggers == nil {
		return true
	}
	return *c.NotifyRatchetTriggers
}

func (sc *StrategyConfig) NotifyRatchetTriggersEnabled(cfg *Config) bool {
	if sc != nil && sc.NotifyRatchetTriggers != nil {
		return *sc.NotifyRatchetTriggers
	}
	return cfg.NotifyRatchetTriggersEnabled()
}

func (sc *StrategyConfig) CircuitBreakerEnabled() bool {
	if sc == nil || sc.CircuitBreaker == nil {
		return true
	}
	return *sc.CircuitBreaker
}

func (sc *StrategyConfig) AllowDeprecatedEffective() bool {
	if sc == nil {
		return false
	}
	if sc.AllowDeprecated != nil {
		return *sc.AllowDeprecated
	}
	return !isLiveArgs(sc.Args)
}

func (sc *StrategyConfig) AllowDeprecatedAcknowledged() bool {
	return sc != nil && sc.AllowDeprecated != nil && *sc.AllowDeprecated
}

const (
	DefaultCBDrawdownCooldown    = 24 * time.Hour
	DefaultCBLossStreakThreshold = 5
	DefaultCBLossStreakCooldown  = 1 * time.Hour

	maxCBCooldownMinutes     = 30 * 24 * 60
	maxCBLossStreakThreshold = 100
)

func (sc *StrategyConfig) CircuitBreakerDrawdownCooldown() time.Duration {
	if sc == nil || sc.CBDrawdownCooldownMinutes == nil {
		return DefaultCBDrawdownCooldown
	}
	return time.Duration(*sc.CBDrawdownCooldownMinutes) * time.Minute
}

func (sc *StrategyConfig) CircuitBreakerLossStreakThreshold() int {
	if sc == nil || sc.CBLossStreakThreshold == nil {
		return DefaultCBLossStreakThreshold
	}
	return *sc.CBLossStreakThreshold
}

func (sc *StrategyConfig) CircuitBreakerLossStreakCooldown() time.Duration {
	if sc == nil || sc.CBLossStreakCooldownMinutes == nil {
		return DefaultCBLossStreakCooldown
	}
	return time.Duration(*sc.CBLossStreakCooldownMinutes) * time.Minute
}

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

func ShouldPostSummary(freq string, continuous, hasTrades bool, lastPost, now time.Time) bool {
	if hasTrades {
		return true
	}
	dur, err := ParseSummaryFrequency(freq)
	if err != nil {
		dur = -1
	}
	switch {
	case dur < 0:
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

type ThetaHarvestConfig struct {
	Enabled         bool    `json:"enabled"`
	ProfitTargetPct float64 `json:"profit_target_pct"`
	StopLossPct     float64 `json:"stop_loss_pct"`
	MinDTEClose     float64 `json:"min_dte_close"`
}

type FuturesConfig struct {
	FeePerContract float64 `json:"fee_per_contract"`
	MaxContracts   int     `json:"max_contracts,omitempty"`
}

type StrategyRef struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type StrategyConfig struct {
	ID                          string                   `json:"id"`
	Type                        string                   `json:"type"`
	Platform                    string                   `json:"platform"`
	Symbol                      string                   `json:"symbol,omitempty"`
	Timeframe                   string                   `json:"timeframe,omitempty"`
	Script                      string                   `json:"script"`
	Args                        []string                 `json:"args"`
	OpenStrategy                StrategyRef              `json:"open_strategy"`
	CloseStrategy               *StrategyRef             `json:"close_strategy,omitempty"`
	closeStrategiesLegacy       []StrategyRef            `json:"-"`
	AllowedRegimes              []string                 `json:"allowed_regimes,omitempty"`
	RegimeGateOnFailure         string                   `json:"regime_gate_on_failure,omitempty"`
	RegimeGateWindow            string                   `json:"regime_gate_window,omitempty"`
	RegimeATRWindow             string                   `json:"regime_atr_window,omitempty"`
	RegimeDirectionalWindow     string                   `json:"regime_directional_window,omitempty"`
	HurstGate                   *HurstGateConfig         `json:"hurst_gate,omitempty"`
	Capital                     float64                  `json:"capital"`
	CapitalPct                  float64                  `json:"capital_pct,omitempty"`
	InitialCapital              float64                  `json:"initial_capital,omitempty"`
	sharedWalletPoolBudget      bool                     `json:"-"`
	sharedWalletModeDeferred    bool                     `json:"-"`
	MaxDrawdownPct              float64                  `json:"max_drawdown_pct"`
	CircuitBreaker              *bool                    `json:"circuit_breaker,omitempty"`
	CBDrawdownCooldownMinutes   *int                     `json:"cb_drawdown_cooldown_minutes,omitempty"`
	CBLossStreakThreshold       *int                     `json:"cb_loss_streak_threshold,omitempty"`
	CBLossStreakCooldownMinutes *int                     `json:"cb_loss_streak_cooldown_minutes,omitempty"`
	NotifyRatchetTriggers       *bool                    `json:"notify_ratchet_triggers,omitempty"`
	LLMEntryAnalysis            *LLMEntryAnalysisConfig  `json:"llm_entry_analysis,omitempty"`
	AllowDeprecated             *bool                    `json:"allow_deprecated,omitempty"`
	Paused                      bool                     `json:"paused,omitempty"`
	IntervalSeconds             int                      `json:"interval_seconds,omitempty"`
	HTFFilter                   bool                     `json:"htf_filter,omitempty"`
	ATRMethod                   string                   `json:"atr_method,omitempty"`
	InvertSignal                bool                     `json:"invert_signal,omitempty"`
	AllowShorts                 bool                     `json:"allow_shorts,omitempty"`
	Direction                   string                   `json:"direction,omitempty"`
	Leverage                    float64                  `json:"leverage,omitempty"`
	SizingLeverage              float64                  `json:"sizing_leverage,omitempty"`
	MarginPerTradeUSD           *float64                 `json:"margin_per_trade_usd,omitempty"`
	RiskPerTradePct             *float64                 `json:"risk_per_trade_pct,omitempty"`
	StopLossPct                 *float64                 `json:"stop_loss_pct,omitempty"`
	StopLossMarginPct           *float64                 `json:"stop_loss_margin_pct,omitempty"`
	TrailingStopPct             *float64                 `json:"trailing_stop_pct,omitempty"`
	TrailingStopATRMult         *float64                 `json:"trailing_stop_atr_mult,omitempty"`
	StopLossATRMult             *float64                 `json:"stop_loss_atr_mult,omitempty"`
	StopLossATRMultRegime       *RegimeATRBlock          `json:"stop_loss_atr_mult_regime,omitempty"`
	TrailingStopATRMultRegime   *RegimeATRBlock          `json:"trailing_stop_atr_mult_regime,omitempty"`
	TrailingStopMinMovePct      *float64                 `json:"trailing_stop_min_move_pct,omitempty"`
	MarginMode                  string                   `json:"margin_mode,omitempty"`
	ThetaHarvest                *ThetaHarvestConfig      `json:"theta_harvest,omitempty"`
	FuturesConfig               *FuturesConfig           `json:"futures,omitempty"`
	RegimeDirectionalPolicy     *RegimeDirectionalPolicy `json:"regime_directional_policy,omitempty"`
	RegimeWindowDivergence      *RegimeWindowDivergence  `json:"regime_window_divergence,omitempty"`
	RegimeProfileAllocation     *RegimeProfileAllocation `json:"regime_profile_allocation,omitempty"`
	AllowScaleIn                bool                     `json:"allow_scale_in,omitempty"`
	ReplaySharing               string                   `json:"replay_sharing,omitempty"`
	ReplaySourceID              string                   `json:"replay_source_id,omitempty"`
	ScaleIn                     *ScaleInConfig           `json:"scale_in,omitempty"`
	Hedge                       *HedgeConfig             `json:"hedge,omitempty"`
}

type HedgeConfig struct {
	Enabled    bool    `json:"enabled"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side,omitempty"`
	Ratio      float64 `json:"ratio,omitempty"`
	Platform   string  `json:"platform,omitempty"`
	Type       string  `json:"type,omitempty"`
	MarginMode string  `json:"margin_mode,omitempty"`
	Leverage   float64 `json:"leverage,omitempty"`
}

func HedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

func hedgeCoin(sc StrategyConfig) string {
	if !HedgeEnabled(sc) {
		return ""
	}
	return normalizeHedgeCoin(sc.Hedge.Symbol)
}

func normalizeHedgeCoin(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if idx := strings.IndexAny(s, "/:"); idx > 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func hedgeRatio(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Ratio <= 0 {
		return 1
	}
	return sc.Hedge.Ratio
}

func hedgeLeverage(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Leverage <= 0 {
		return 1
	}
	return sc.Hedge.Leverage
}

func hedgeMarginMode(sc StrategyConfig) string {
	if sc.Hedge == nil || strings.TrimSpace(sc.Hedge.MarginMode) == "" {
		return "isolated"
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.MarginMode))
}

func HedgeSideForPrimary(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

type ScaleInConfig struct {
	MaxAdds             int     `json:"max_adds,omitempty"`
	MaxAddedNotionalUSD float64 `json:"max_added_notional_usd,omitempty"`
	AddSpacingATR       float64 `json:"add_spacing_atr,omitempty"`
	AddNotionalUSD      float64 `json:"add_notional_usd,omitempty"`
}

func (sc *StrategyConfig) UnmarshalJSON(data []byte) error {
	type alias StrategyConfig
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

func (sc StrategyConfig) closeRefs() []StrategyRef {
	if sc.CloseStrategy == nil {
		return nil
	}
	return []StrategyRef{*sc.CloseStrategy}
}

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

func EffectiveExchangeLeverage(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.Leverage <= 0 {
		return 1
	}
	return sc.Leverage
}

func EffectiveMarginPerTradeUSD(sc StrategyConfig) float64 {
	if sc.Type != "perps" || sc.MarginPerTradeUSD == nil {
		return 0
	}
	if *sc.MarginPerTradeUSD <= 0 {
		return 0
	}
	return *sc.MarginPerTradeUSD
}

const (
	DirectionLong  = "long"
	DirectionShort = "short"
	DirectionBoth  = "both"
)

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

func PerpsAllowsLong(sc StrategyConfig) bool {
	d := EffectiveDirection(sc)
	return d == DirectionLong || d == DirectionBoth
}

func PerpsAllowsShort(sc StrategyConfig) bool {
	d := EffectiveDirection(sc)
	return d == DirectionShort || d == DirectionBoth
}

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

func ComputePerpsOpenNotional(sc StrategyConfig, cash float64) float64 {
	return PerpsOpenNotional(cash, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc))
}

const MaxAutoStopLossPct = 50.0

const DefaultStopLossATRMult = 1.0

func EffectiveStopLossPct(sc StrategyConfig) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		return 0
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return 0
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return 0
	}
	if sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero() {
		return 0
	}
	if sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero() {
		return 0
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return *sc.TrailingStopPct
		}
		return 0
	}
	if sc.StopLossPct != nil {
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

func EffectiveInitialCapital(sc StrategyConfig, ss *StrategyState) float64 {
	if usesSharedWalletPoolBudget(sc) {
		return 0
	}
	if sc.InitialCapital > 0 {
		return sc.InitialCapital
	}
	if ss != nil && ss.InitialCapital > 0 {
		return ss.InitialCapital
	}
	return sc.Capital
}

func LoadConfig(path string) (*Config, error) {
	return loadConfig(path, false)
}

func LoadConfigForProbe(path string) (*Config, error) {
	return loadConfig(path, true)
}

func loadConfig(path string, skipLiveCredentialChecks bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := checkRawConfigVersionSupported(data); err != nil {
		return nil, err
	}
	migrationBaseVersion := rawConfigVersion(data)
	if migrationBaseVersion == 0 {
		migrationBaseVersion = CurrentConfigVersion
	}
	if needsV13SchemaMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v13 schema migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v13 migration: %w", err)
		}
	}
	if needsV15CloseMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v15 close-key migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v15 migration: %w", err)
		}
	}
	if needsV16UserDefaultsMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v16 user-defaults migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v16 user-defaults migration: %w", err)
		}
	}
	if needsV18TrailStopKeyMigration(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v18 trail_stop_atr_regime key migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v18 trail_stop_atr_regime migration: %w", err)
		}
	}
	if needsV19AtrMultRegimeRename(data) {
		if err := MigrateConfig(path, nil, nil); err != nil {
			return nil, fmt.Errorf("v19 atr_mult_regime key migration: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config after v19 atr_mult_regime migration: %w", err)
		}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.migrationBaseVersion = migrationBaseVersion
	cfg.migrationBaseVersionSet = true
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

	if cfg.StatusPort != 0 {
		if cfg.StatusPort < 1024 {
			return nil, fmt.Errorf("status_port %d is below 1024 (privileged ports require root and are not supported)", cfg.StatusPort)
		}
		if cfg.StatusPort > 65535-statusPortMaxAttempts+1 {
			return nil, fmt.Errorf("status_port %d is too high (max %d to leave room for %d fallback attempts)", cfg.StatusPort, 65535-statusPortMaxAttempts+1, statusPortMaxAttempts)
		}
	}

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

	if ownerID := os.Getenv("DISCORD_OWNER_ID"); ownerID != "" {
		cfg.Discord.OwnerID = ownerID
	}

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
	if telegramOwner := os.Getenv("TELEGRAM_OWNER_CHAT_ID"); telegramOwner != "" {
		cfg.Telegram.OwnerChatID = telegramOwner
	}

	cfg.StatusToken = os.Getenv("STATUS_AUTH_TOKEN")

	if cfg.Platforms == nil {
		cfg.Platforms = make(map[string]*PlatformConfig)
	}

	for i := range cfg.Strategies {
		normalizeDeprecatedCloseRef(cfg.Strategies[i].CloseStrategy)
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

		if cfg.Strategies[i].MaxDrawdownPct == 0 {
			platform := cfg.Strategies[i].Platform
			if pc := cfg.Platforms[platform]; pc != nil && pc.Risk != nil && pc.Risk.MaxDrawdownPct > 0 {
				cfg.Strategies[i].MaxDrawdownPct = pc.Risk.MaxDrawdownPct
			} else if cfg.Strategies[i].Type == "options" {
				cfg.Strategies[i].MaxDrawdownPct = 40
			} else if cfg.Strategies[i].Type == "perps" {
				cfg.Strategies[i].MaxDrawdownPct = 50
			} else if cfg.Strategies[i].Type == "futures" {
				cfg.Strategies[i].MaxDrawdownPct = 45
			} else {
				cfg.Strategies[i].MaxDrawdownPct = 60
			}
		}

		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].Leverage <= 0 {
			cfg.Strategies[i].Leverage = 1
		}
		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].SizingLeverage == 0 && cfg.Strategies[i].RiskPerTradePct == nil {
			cfg.Strategies[i].SizingLeverage = cfg.Strategies[i].Leverage
		}

		if cfg.Strategies[i].Type == "perps" && cfg.Strategies[i].Platform == "hyperliquid" && cfg.Strategies[i].MarginMode == "" {
			cfg.Strategies[i].MarginMode = "isolated"
		}

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

	applyUserCloseDefaultRatchetRegimeTrails(&cfg)

	defaultStopLossATRMult := *cfg.DefaultStopLossATRMult
	if defaultStopLossATRMult > 0 {
		for i := range cfg.Strategies {
			sc := &cfg.Strategies[i]
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				continue
			}
			if sc.StopLossPct == nil && sc.StopLossMarginPct == nil && sc.TrailingStopPct == nil && sc.TrailingStopATRMult == nil && sc.StopLossATRMult == nil && !sc.StopLossATRMultRegime.IsConfigured() && !sc.TrailingStopATRMultRegime.IsConfigured() && !strategyUsesUnifiedRegimeClose(*sc) {
				defaultMult := defaultStopLossATRMult
				sc.StopLossATRMult = &defaultMult
				fmt.Printf("[INFO] %s: applied default stop_loss_atr_mult=%g (no stop fields set; set stop_loss_atr_mult=0 or default_stop_loss_atr_mult=0 to opt out)\n", sc.ID, defaultStopLossATRMult)
			}
		}
	}

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
			if block, ok := cfg.resolveManualRatchetRegimeTrailBlock(*sc); ok {
				sc.CloseStrategy = &StrategyRef{Name: trailingTPRatchetRegimeCloseName}
				sc.TrailingStopATRMultRegime = block
				fmt.Printf("[INFO] %s: manual close defaulted to %s (regime enabled; trailing_stop_atr_mult_regime owns the per-regime trail/SL)\n", sc.ID, trailingTPRatchetRegimeCloseName)
			} else {
				sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_live"}
				if cfg.Regime != nil && cfg.Regime.Enabled {
					fmt.Printf("[INFO] %s: manual close defaulted to tiered_tp_atr_live (regime enabled, but kept the scalar default — an explicit stop field is set or the classifier vocabulary has no default per-regime trail)\n", sc.ID)
				} else {
					fmt.Printf("[INFO] %s: manual close defaulted to tiered_tp_atr_live (regime disabled)\n", sc.ID)
				}
			}
		}
		if defaultStopLossATRMult > 0 && sc.StopLossATRMult == nil && sc.StopLossPct == nil && sc.StopLossMarginPct == nil && sc.TrailingStopPct == nil && sc.TrailingStopATRMult == nil && !sc.StopLossATRMultRegime.IsConfigured() && !sc.TrailingStopATRMultRegime.IsConfigured() {
			defaultMult := cfg.resolveManualStopLossATRMult()
			if defaultMult > 0 {
				sc.StopLossATRMult = &defaultMult
			}
		}
		if cs := sc.CloseStrategy; cs != nil && isTieredTPATRCloseName(cs.Name) &&
			cs.Name != "tiered_tp_atr_regime" && cs.Name != "tiered_tp_atr_live_regime" &&
			cs.Name != dynamicCloseStrategyName {
			if cs.Params == nil {
				cs.Params = map[string]interface{}{}
			}
			if _, hasTP := closeTierListParam(cs.Params); !hasTP {
				cs.Params["tp_tiers"] = cfg.resolveManualTPTiers()
			}
		}
	}

	if cfg.PortfolioRisk == nil {
		cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25}
	}
	if cfg.PortfolioRisk.WarnThresholdPct == 0 {
		cfg.PortfolioRisk.WarnThresholdPct = 60
	}

	if cfg.Correlation == nil {
		cfg.Correlation = &CorrelationConfig{Enabled: false, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}
	}

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

	applyUserCloseDefaults(&cfg)

	applyUserCloseDefaultRegimeATRs(&cfg)

	if err := validateConfig(&cfg, skipLiveCredentialChecks); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func normalizeHyperliquidPeerStopLosses(strategies []StrategyConfig) {
}

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

const hedgeMaxRatio = 10.0

const hedgeMaxLeverage = 50.0

func hedgeCollisionCoin(sc StrategyConfig) string {
	return normalizeHedgeCoin(hyperliquidConfiguredCoin(sc))
}

func validateHedgeConfigs(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string

	primaryCoinOwners := make(map[string][]string)
	for _, sc := range cfg.Strategies {
		coin := hedgeCollisionCoin(sc)
		if coin == "" {
			continue
		}
		primaryCoinOwners[coin] = append(primaryCoinOwners[coin], sc.ID)
	}

	hedgeCoinOwners := make(map[string][]string)

	for _, sc := range cfg.Strategies {
		if sc.Hedge == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)

		if p := strings.ToLower(strings.TrimSpace(sc.Hedge.Platform)); p != "" && p != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge.platform must be empty or %q (phase 1 is hyperliquid-only, #1159), got %q", prefix, "hyperliquid", sc.Hedge.Platform))
		}
		if t := strings.ToLower(strings.TrimSpace(sc.Hedge.Type)); t != "" && t != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge.type must be empty or %q (phase 1 is perps-only, #1159), got %q", prefix, "perps", sc.Hedge.Type))
		}
		if s := strings.ToLower(strings.TrimSpace(sc.Hedge.Side)); s != "" && s != "inverse" {
			errs = append(errs, fmt.Sprintf("%s: hedge.side must be empty or %q (the only phase-1 side policy, #1159), got %q", prefix, "inverse", sc.Hedge.Side))
		}
		if sc.Hedge.Ratio < 0 || sc.Hedge.Ratio > hedgeMaxRatio {
			errs = append(errs, fmt.Sprintf("%s: hedge.ratio must be in (0, %g] (0/omitted defaults to 1.0), got %g", prefix, hedgeMaxRatio, sc.Hedge.Ratio))
		}
		if sc.Hedge.Leverage < 0 || sc.Hedge.Leverage > hedgeMaxLeverage {
			errs = append(errs, fmt.Sprintf("%s: hedge.leverage must be in (0, %g] (0/omitted defaults to 1), got %g", prefix, hedgeMaxLeverage, sc.Hedge.Leverage))
		}
		switch mm := strings.ToLower(strings.TrimSpace(sc.Hedge.MarginMode)); mm {
		case "", "isolated", "cross":
		default:
			errs = append(errs, fmt.Sprintf("%s: hedge.margin_mode must be %q or %q (empty defaults to isolated), got %q", prefix, "isolated", "cross", sc.Hedge.MarginMode))
		}

		coin := normalizeHedgeCoin(sc.Hedge.Symbol)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol is required and must name an HL coin (e.g. \"BTC\" or \"BTC/USDC:USDC\")", prefix))
		}

		if !sc.Hedge.Enabled {
			continue
		}

		if sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported for perps strategies in phase 1 (got type %q, #1159)", prefix, sc.Type))
		}
		if sc.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported on hyperliquid in phase 1 (got platform %q, #1159)", prefix, sc.Platform))
		}
		if EffectiveDirection(sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s: hedge is not supported with direction=%q in phase 1 — a bidirectional flip changes the hedge side mid-position and the catastrophic-flip close-only path cannot be mirrored deterministically (#1159); use direction=%q or %q", prefix, DirectionBoth, DirectionLong, DirectionShort))
		}

		if coin == "" {
			continue
		}

		if own := hedgeCollisionCoin(sc); own != "" && own == coin {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q is the strategy's own coin — a same-coin hedge nets the position on-chain instead of hedging it (#1159)", prefix, coin))
		}

		if owners := primaryCoinOwners[coin]; len(owners) > 0 {
			ids := append([]string(nil), owners...)
			sort.Strings(ids)
			filtered := ids[:0]
			for _, id := range ids {
				if id != sc.ID {
					filtered = append(filtered, id)
				}
			}
			if len(filtered) > 0 {
				errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q is the primary coin of strategy/strategies %s — HL aggregates positions per coin per account, and every shared-coin mechanism (peer margin checks, circuit-breaker drain, kill-switch fill share, reconcile ownership) is blind to hedge legs in phase 1 (#1159)", prefix, coin, strings.Join(filtered, ", ")))
			}
		}

		hedgeCoinOwners[coin] = append(hedgeCoinOwners[coin], sc.ID)
	}

	sharedHedgeCoins := make([]string, 0, len(hedgeCoinOwners))
	for coin, owners := range hedgeCoinOwners {
		if len(owners) > 1 {
			sharedHedgeCoins = append(sharedHedgeCoins, coin)
		}
	}
	sort.Strings(sharedHedgeCoins)
	for _, coin := range sharedHedgeCoins {
		ids := append([]string(nil), hedgeCoinOwners[coin]...)
		sort.Strings(ids)
		errs = append(errs, fmt.Sprintf("hedge coin %s is claimed by multiple hedge-enabled strategies (%s): HL aggregates positions per coin per account, so two hedge legs on one coin would share an on-chain position, margin assignment, and reduce-only order slots (#1159)", coin, strings.Join(ids, ", ")))
	}

	return errs
}

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
	mirroredReplaySources := make(map[string]string)
	strategyByID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		if sc.ID != "" {
			strategyByID[sc.ID] = sc
		}
	}
	sharedWalletPoolIDs, poolErrs := validateConfiguredSharedWalletPools(cfg.Strategies)
	errs = append(errs, poolErrs...)
	for i := range cfg.Strategies {
		cfg.Strategies[i].sharedWalletPoolBudget = sharedWalletPoolIDs[cfg.Strategies[i].ID]
	}

	if cfg.LeaderboardPostTime != "" {
		if _, _, ok := ParseLeaderboardPostTime(cfg.LeaderboardPostTime); !ok {
			errs = append(errs, fmt.Sprintf("leaderboard_post_time must be in \"HH:MM\" format (24h UTC), got %q", cfg.LeaderboardPostTime))
		}
	}

	errs = append(errs, validateUserDefaults(cfg.UserDefaults)...)

	if !validATRMethodValue(cfg.ATRMethod) {
		errs = append(errs, fmt.Sprintf("atr_method must be %q or %q, got %q", ATRMethodSimple, ATRMethodWilder, cfg.ATRMethod))
	}

	for i, sc := range cfg.Strategies {
		prefix := fmt.Sprintf("strategy[%d]", i)

		if sc.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is empty", prefix))
		} else if seenIDs[sc.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate id %q", prefix, sc.ID))
		} else {
			seenIDs[sc.ID] = true
			prefix = fmt.Sprintf("strategy[%s]", sc.ID)
		}

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

		if sc.Type != "spot" && sc.Type != "options" && sc.Type != "perps" && sc.Type != "futures" && sc.Type != "manual" {
			errs = append(errs, fmt.Sprintf("%s: type must be \"spot\", \"options\", \"perps\", \"futures\", or \"manual\", got %q", prefix, sc.Type))
		}
		errs = append(errs, validateLLMEntryAnalysis(prefix, sc)...)
		if !validATRMethodValue(sc.ATRMethod) {
			errs = append(errs, fmt.Sprintf("%s: atr_method must be %q or %q, got %q", prefix, ATRMethodSimple, ATRMethodWilder, sc.ATRMethod))
		} else if sc.Type == "options" && normalizeATRMethod(sc.ATRMethod) != "" {
			errs = append(errs, fmt.Sprintf("%s: atr_method is not supported on options strategies (no ATR surface); remove it", prefix))
		}
		if len(sc.closeStrategiesLegacy) > 1 {
			names := make([]string, 0, len(sc.closeStrategiesLegacy))
			for _, ref := range sc.closeStrategiesLegacy {
				names = append(names, ref.Name)
			}
			errs = append(errs, fmt.Sprintf("%s: close_strategies has %d entries %v — the array model was collapsed to a single close_strategy (#842); keep one profit-taking close and move risk backstops (hard caps, time stops) to the strategy level", prefix, len(names), names))
		}
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

		if sc.Type == "options" && len(sc.AllowedRegimes) > 0 {
			errs = append(errs, fmt.Sprintf("%s: allowed_regimes is not enforced for type=options (gate not wired at options dispatch; see issue #553)", prefix))
		}

		if _, err := parseRegimeGateOnFailure(sc.RegimeGateOnFailure); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", prefix, err))
		}

		if !validReplaySharing(sc.ReplaySharing) {
			errs = append(errs, fmt.Sprintf("%s: replay_sharing must be %q or %q, got %q", prefix, ReplaySharingNone, ReplaySharingLiveMirror, sc.ReplaySharing))
		} else if normalizeReplaySharing(sc.ReplaySharing) == ReplaySharingLiveMirror {
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: replay_sharing=%q is supported for HL perps strategies only (type=perps, platform=hyperliquid)", prefix, ReplaySharingLiveMirror))
			}
			if strings.TrimSpace(cfg.ReplayLogPath) == "" {
				errs = append(errs, fmt.Sprintf("%s: replay_sharing=%q requires the root replay_log_path to be set", prefix, ReplaySharingLiveMirror))
			}
		}

		if srcID := strings.TrimSpace(sc.ReplaySourceID); srcID != "" {
			src, srcFound := strategyByID[srcID]
			switch {
			case normalizeReplaySharing(sc.ReplaySharing) != ReplaySharingLiveMirror:
				errs = append(errs, fmt.Sprintf("%s: replay_source_id requires replay_sharing=%q", prefix, ReplaySharingLiveMirror))
			case sc.Type != "perps" || sc.Platform != "hyperliquid":
				errs = append(errs, fmt.Sprintf("%s: replay_source_id is supported for HL perps strategies only (type=perps, platform=hyperliquid)", prefix))
			case isLiveArgs(sc.Args):
				errs = append(errs, fmt.Sprintf("%s: replay_source_id is only valid on a paper strategy — the live source writes the decision log", prefix))
			case srcID == sc.ID:
				errs = append(errs, fmt.Sprintf("%s: replay_source_id must name another strategy; omit it to mirror the live twin that shares this id", prefix))
			case !srcFound:
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q names no strategy in this config", prefix, srcID))
			case !isLiveArgs(src.Args):
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q names a paper strategy; the replay source must run live (--mode=live)", prefix, srcID))
			case src.Type != "perps" || src.Platform != "hyperliquid":
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q must name an HL perps strategy (type=perps, platform=hyperliquid)", prefix, srcID))
			case normalizeReplaySharing(src.ReplaySharing) != ReplaySharingLiveMirror:
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q must set replay_sharing=%q or it never writes decisions", prefix, srcID, ReplaySharingLiveMirror))
			case extractAsset(src) != extractAsset(sc) || extractTimeframe(src) != extractTimeframe(sc):
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q trades %s/%s but this strategy trades %s/%s — the mirror must track the same symbol and timeframe",
					prefix, srcID, extractAsset(src), extractTimeframe(src), extractAsset(sc), extractTimeframe(sc)))
			case mirroredReplaySources[srcID] != "":
				errs = append(errs, fmt.Sprintf("%s: replay_source_id %q is already mirrored by strategy %q — one paper mirror per live source (the mirrors would consume each other's rows)",
					prefix, srcID, mirroredReplaySources[srcID]))
			default:
				mirroredReplaySources[srcID] = sc.ID
			}
		}

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

		if sc.CapitalPct != 0 {
			if sc.CapitalPct < 0 || sc.CapitalPct > 1 {
				errs = append(errs, fmt.Sprintf("%s: capital_pct must be in (0, 1], got %g", prefix, sc.CapitalPct))
			}
			if sc.Capital > 0 {
				fmt.Printf("[WARN] %s: both capital ($%.0f) and capital_pct (%.0f%%) set — capital_pct takes priority\n", sc.ID, sc.Capital, sc.CapitalPct*100)
			}
			if !skipLiveCredentialChecks && sc.CapitalPct > 0 && sc.Platform == "hyperliquid" {
				if os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS") == "" {
					errs = append(errs, fmt.Sprintf("%s: capital_pct requires HYPERLIQUID_ACCOUNT_ADDRESS env var", prefix))
				}
			}
		}

		if sc.InitialCapital < 0 {
			errs = append(errs, fmt.Sprintf("%s: initial_capital must be > 0 when set, got %g", prefix, sc.InitialCapital))
		}

		if sc.Capital <= 0 && sc.CapitalPct == 0 && !sharedWalletPoolIDs[sc.ID] {
			errs = append(errs, fmt.Sprintf("%s: capital must be > 0 (or set capital_pct), got %g", prefix, sc.Capital))
		}

		if sc.MaxDrawdownPct <= 0 || sc.MaxDrawdownPct > 100 {
			errs = append(errs, fmt.Sprintf("%s: max_drawdown_pct must be in (0, 100], got %g", prefix, sc.MaxDrawdownPct))
		}

		type cbOverrideField struct {
			key string
			val *int
			max int
		}
		for _, f := range []cbOverrideField{
			{"cb_drawdown_cooldown_minutes", sc.CBDrawdownCooldownMinutes, maxCBCooldownMinutes},
			{"cb_loss_streak_threshold", sc.CBLossStreakThreshold, maxCBLossStreakThreshold},
			{"cb_loss_streak_cooldown_minutes", sc.CBLossStreakCooldownMinutes, maxCBCooldownMinutes},
		} {
			if f.val == nil {
				continue
			}
			if sc.Type == "manual" {
				errs = append(errs, fmt.Sprintf("%s: %s is not supported for manual strategies (exempt from CheckRisk)", prefix, f.key))
			}
			if *f.val <= 0 {
				errs = append(errs, fmt.Sprintf("%s: %s must be positive, got %d", prefix, f.key, *f.val))
			} else if *f.val > f.max {
				errs = append(errs, fmt.Sprintf("%s: %s must be <= %d, got %d", prefix, f.key, f.max, *f.val))
			}
		}

		if sc.IntervalSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s: interval_seconds must be >= 0, got %d", prefix, sc.IntervalSeconds))
		}

		if msg := strategyIntervalExceedsGlobalWarning(sc, cfg.IntervalSeconds); msg != "" {
			fmt.Println(msg)
		}

		if sc.Leverage != 0 {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.Leverage < 1 || sc.Leverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: leverage must be in [1, 100], got %g", prefix, sc.Leverage))
			}
		}
		if sc.SizingLeverage != 0 {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if sc.SizingLeverage < 0.01 || sc.SizingLeverage > 100 {
				errs = append(errs, fmt.Sprintf("%s: sizing_leverage must be in [0.01, 100], got %g", prefix, sc.SizingLeverage))
			}
		}

		if sc.MarginPerTradeUSD != nil {
			if sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: margin_per_trade_usd is only supported for perps strategies (got type %q)", prefix, sc.Type))
			}
			if *sc.MarginPerTradeUSD <= 0 {
				errs = append(errs, fmt.Sprintf("%s: margin_per_trade_usd must be positive, got %g", prefix, *sc.MarginPerTradeUSD))
			}
		}

		errs = append(errs, validateRiskPerTradePct(sc, prefix)...)

		if sc.AllowScaleIn {
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in is only supported for perps/manual strategies (got type %q)", prefix, sc.Type))
			}
			if sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in is only supported on hyperliquid (got platform %q)", prefix, sc.Platform))
			}
			if sc.Type == "perps" && sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args) && !scaleInLiveProtectionResizable(sc) {
				errs = append(errs, fmt.Sprintf("%s: allow_scale_in on live perps requires an ATR/regime or trailing stop-loss that can be re-sized after an add — stop_loss_pct/stop_loss_margin_pct and the max_drawdown fallback cannot (set stop_loss_atr_mult, stop_loss_atr_mult_regime, or a trailing stop)", prefix))
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

		if sc.Direction != "" {
			switch sc.Direction {
			case DirectionLong, DirectionShort, DirectionBoth:
			default:
				errs = append(errs, fmt.Sprintf("%s: direction must be %q, %q, or %q, got %q", prefix, DirectionLong, DirectionShort, DirectionBoth, sc.Direction))
			}
			if sc.Type != "perps" && sc.Type != "manual" {
				errs = append(errs, fmt.Sprintf("%s: direction is only supported for perps/manual strategies (got type %q)", prefix, sc.Type))
			}
			if sc.AllowShorts && sc.Direction == DirectionLong {
				errs = append(errs, fmt.Sprintf("%s: direction=%q conflicts with legacy allow_shorts=true (remove allow_shorts; v14 migration normally handles this)", prefix, sc.Direction))
			}
		}

		if sc.InvertSignal {
			if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
				errs = append(errs, fmt.Sprintf("%s: invert_signal is only supported for HL perps/manual strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

		if sc.RegimeDirectionalPolicy.IsConfigured() {
			if sc.Platform != "hyperliquid" || sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: regime_directional_policy is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if cfg.Regime == nil || !cfg.Regime.Enabled {
				errs = append(errs, fmt.Sprintf("%s: regime_directional_policy requires top-level regime.enabled=true", prefix))
			}
		}

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

		if sc.RegimeProfileAllocation.IsConfigured() {
			if sc.Platform != "hyperliquid" || sc.Type != "perps" {
				errs = append(errs, fmt.Sprintf("%s: regime_profile_allocation is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
			if cfg.Regime == nil || !cfg.Regime.Enabled {
				errs = append(errs, fmt.Sprintf("%s: regime_profile_allocation requires top-level regime.enabled=true", prefix))
			}
		}

		if sc.MarginMode != "" {
			if sc.MarginMode != "isolated" && sc.MarginMode != "cross" {
				errs = append(errs, fmt.Sprintf("%s: margin_mode must be \"isolated\" or \"cross\", got %q", prefix, sc.MarginMode))
			}
			if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: margin_mode is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

		if sc.StopLossPct != nil {
			pct := *sc.StopLossPct
			if pct < 0 || pct > 50 {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_pct must be in [0, 50], got %g", prefix, pct))
			}
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				errs = append(errs, fmt.Sprintf("%s: stop_loss_pct is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
			}
		}

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

		for _, msg := range validateHLStopWithinBankruptcyBound(sc) {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, msg))
		}
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
			regimeTrail := sc.TrailingStopATRMultRegime.IsConfigured()
			if fixedTrailingPct <= 0 && atrMult <= 0 && !regimeTrail {
				errs = append(errs, fmt.Sprintf("%s: trailing_stop_min_move_pct requires trailing_stop_pct > 0, trailing_stop_atr_mult > 0, or trailing_stop_atr_mult_regime", prefix))
			}
		}

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

	for _, msg := range hyperliquidPeerStrategyErrors(cfg.Strategies) {
		errs = append(errs, msg)
	}

	errs = append(errs, validateHedgeConfigs(cfg)...)

	if cfg.PortfolioRisk != nil {
		errs = append(errs, validatePortfolioRiskFields(cfg.PortfolioRisk, "portfolio_risk.", false)...)
		if cfg.PortfolioRisk.Paper != nil {
			errs = append(errs, validatePortfolioRiskFields(cfg.PortfolioRisk.Paper, "portfolio_risk.paper.", true)...)
			if cfg.PortfolioRisk.Paper.Paper != nil {
				errs = append(errs, "portfolio_risk.paper.paper is not allowed (the paper override cannot nest another override)")
			}
		}
	}

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

	if cfg.Correlation != nil && cfg.Correlation.Enabled {
		if cfg.Correlation.MaxConcentrationPct <= 0 || cfg.Correlation.MaxConcentrationPct > 100 {
			errs = append(errs, fmt.Sprintf("correlation.max_concentration_pct must be in (0, 100], got %g", cfg.Correlation.MaxConcentrationPct))
		}
		if cfg.Correlation.MaxSameDirectionPct <= 0 || cfg.Correlation.MaxSameDirectionPct > 100 {
			errs = append(errs, fmt.Sprintf("correlation.max_same_direction_pct must be in (0, 100], got %g", cfg.Correlation.MaxSameDirectionPct))
		}
	}

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
	if cfg.Regime != nil {
		if _, err := parseRegimeGateOnFailure(cfg.Regime.GateOnFailure); err != nil {
			errs = append(errs, fmt.Sprintf("regime.gate_on_failure: %v", err))
		}
	}
	errs = append(errs, validateRegimeWindowsConfig(cfg)...)
	errs = append(errs, validateStrategyRegimeVocabulary(cfg)...)
	errs = append(errs, validateRegimeTransitionsConfig(cfg)...)
	errs = append(errs, validateHurstGateConfigs(cfg)...)

	if cfg.Regime == nil || !cfg.Regime.Enabled {
		for _, sc := range cfg.Strategies {
			if len(sc.AllowedRegimes) == 0 {
				continue
			}
			if resolveRegimeGateOnFailure(sc, cfg.Regime) == RegimeGateOnFailureClosed {
				errs = append(errs, fmt.Sprintf("strategy[%s]: regime_gate_on_failure=closed with allowed_regimes but regime.enabled=false — the gate label is always empty, so the strategy could never open; enable regime detection or use the fail-open policy", sc.ID))
				continue
			}
			fmt.Printf("[WARN] %s: allowed_regimes is set but regime.enabled=false — gate is a no-op until regime detection is enabled\n", sc.ID)
		}
	}

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
	if _, err := ParseKillSwitchResetDMTimeout(cfg.KillSwitchResetDMTimeout); err != nil {
		errs = append(errs, err.Error())
	}
	if cfg.Tuning != nil && cfg.Tuning.MaxRetainedRuns < 0 {
		errs = append(errs, fmt.Sprintf("tuning.max_retained_runs must be >= 0 (0 = keep-all), got %d", cfg.Tuning.MaxRetainedRuns))
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

	errs = append(errs, validateRegimeATRConfig(cfg)...)

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

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

func validatePortfolioRiskFields(pr *PortfolioRiskConfig, prefix string, inheritZero bool) []string {
	if pr == nil {
		return nil
	}
	var errs []string
	if inheritZero {
		if pr.MaxDrawdownPct < 0 || pr.MaxDrawdownPct > 100 {
			errs = append(errs, fmt.Sprintf("%smax_drawdown_pct must be in [0, 100] (0 = inherit), got %g", prefix, pr.MaxDrawdownPct))
		}
		if pr.WarnThresholdPct < 0 || pr.WarnThresholdPct > 100 {
			errs = append(errs, fmt.Sprintf("%swarn_threshold_pct must be in [0, 100] (0 = inherit), got %g", prefix, pr.WarnThresholdPct))
		}
	} else {
		if pr.MaxDrawdownPct <= 0 || pr.MaxDrawdownPct > 100 {
			errs = append(errs, fmt.Sprintf("%smax_drawdown_pct must be in (0, 100], got %g", prefix, pr.MaxDrawdownPct))
		}
		if pr.WarnThresholdPct <= 0 || pr.WarnThresholdPct > 100 {
			errs = append(errs, fmt.Sprintf("%swarn_threshold_pct must be in (0, 100], got %g", prefix, pr.WarnThresholdPct))
		}
	}
	if pr.MaxNotionalUSD < 0 {
		errs = append(errs, fmt.Sprintf("%smax_notional_usd must be >= 0, got %g", prefix, pr.MaxNotionalUSD))
	}
	if pr.DailyMaxLossUSD < 0 {
		errs = append(errs, fmt.Sprintf("%sdaily_max_loss_usd must be >= 0 (0 = disabled), got %g", prefix, pr.DailyMaxLossUSD))
	}
	if pr.DailyMaxLossPct < 0 || pr.DailyMaxLossPct > 100 {
		errs = append(errs, fmt.Sprintf("%sdaily_max_loss_pct must be in [0, 100] (0 = disabled), got %g", prefix, pr.DailyMaxLossPct))
	}
	if pr.MaxSameDirectionNotionalUSD < 0 {
		errs = append(errs, fmt.Sprintf("%smax_same_direction_notional_usd must be >= 0 (0 = disabled), got %g", prefix, pr.MaxSameDirectionNotionalUSD))
	}
	if pr.MaxAssetConcentrationPct < 0 || pr.MaxAssetConcentrationPct > 100 {
		errs = append(errs, fmt.Sprintf("%smax_asset_concentration_pct must be in [0, 100] (0 = disabled), got %g", prefix, pr.MaxAssetConcentrationPct))
	}
	return errs
}
