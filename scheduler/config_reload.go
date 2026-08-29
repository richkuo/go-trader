package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

func applyHotReloadConfig(cfg, next *Config, state *AppState, notifier *MultiNotifier, server *StatusServer) ([]string, error) {
	if cfg == nil || next == nil {
		return nil, fmt.Errorf("config reload requires current and next config")
	}
	if err := validateHotReloadCompatible(cfg, next); err != nil {
		return nil, err
	}
	if err := validateHotReloadStateCompatible(cfg, next, state); err != nil {
		return nil, err
	}

	var changes []string
	addChange := func(format string, args ...interface{}) {
		changes = append(changes, fmt.Sprintf(format, args...))
	}

	if cfg.IntervalSeconds != next.IntervalSeconds {
		addChange("interval_seconds: %d -> %d", cfg.IntervalSeconds, next.IntervalSeconds)
		cfg.IntervalSeconds = next.IntervalSeconds
	}
	if !boolPtrEqual(cfg.NotifyRatchetTriggers, next.NotifyRatchetTriggers) {
		addChange("notify_ratchet_triggers: %s -> %s", formatNotifyRatchetTriggers(cfg.NotifyRatchetTriggers), formatNotifyRatchetTriggers(next.NotifyRatchetTriggers))
		cfg.NotifyRatchetTriggers = next.NotifyRatchetTriggers
	}
	if !floatPtrEqual(cfg.DefaultStopLossATRMult, next.DefaultStopLossATRMult) {
		addChange("default_stop_loss_atr_mult: %s -> %s (applies to strategies opened after restart; existing StopLossATRMult on currently-loaded strategies is unchanged)", formatFloatPtr(cfg.DefaultStopLossATRMult), formatFloatPtr(next.DefaultStopLossATRMult))
		cfg.DefaultStopLossATRMult = next.DefaultStopLossATRMult
	}
	if normalizeATRMethod(cfg.ATRMethod) != normalizeATRMethod(next.ATRMethod) {
		addChange("atr_method: %q -> %q", cfg.ATRMethod, next.ATRMethod)
		cfg.ATRMethod = next.ATRMethod
	}
	if cfg.AlertThrottleInterval != next.AlertThrottleInterval {
		addChange("alert_throttle_interval: %q -> %q", cfg.AlertThrottleInterval, next.AlertThrottleInterval)
		cfg.AlertThrottleInterval = next.AlertThrottleInterval
		if err := applyAlertThrottleFromConfig(cfg); err != nil {
			return nil, fmt.Errorf("alert_throttle_interval: %w", err)
		}
	}
	if cfg.KillSwitchResetDMTimeout != next.KillSwitchResetDMTimeout {
		addChange("kill_switch_reset_dm_timeout: %q -> %q", cfg.KillSwitchResetDMTimeout, next.KillSwitchResetDMTimeout)
		cfg.KillSwitchResetDMTimeout = next.KillSwitchResetDMTimeout
		if err := applyKillSwitchResetDMTimeoutFromConfig(cfg); err != nil {
			return nil, fmt.Errorf("kill_switch_reset_dm_timeout: %w", err)
		}
	}
	if !reflect.DeepEqual(cfg.Tuning, next.Tuning) {
		addChange("tuning: %+v -> %+v", cfg.Tuning, next.Tuning)
		cfg.Tuning = cloneTuningConfig(next.Tuning)
		if server != nil && server.tuning != nil {
			server.tuning.setMaxRetainedRuns(cfg.tuningMaxRetainedRuns())
		}
	}
	if !reflect.DeepEqual(cfg.UserDefaults, next.UserDefaults) {
		addChange("user_defaults: %s -> %s", formatUserDefaults(cfg.UserDefaults), formatUserDefaults(next.UserDefaults))
		cfg.UserDefaults = cloneUserDefaults(next.UserDefaults)
	}

	if cfg.Regime != nil && next.Regime != nil && !reflect.DeepEqual(cfg.Regime.DisplayWindows, next.Regime.DisplayWindows) {
		addChange("regime.display_windows: %v -> %v", cfg.Regime.DisplayWindows, next.Regime.DisplayWindows)
		cfg.Regime.DisplayWindows = append([]string(nil), next.Regime.DisplayWindows...)
	}
	if cfg.Regime != nil && next.Regime != nil && normalizeRegimeTimeframe(cfg.Regime.Timeframe) != normalizeRegimeTimeframe(next.Regime.Timeframe) {
		addChange("regime.timeframe: %q -> %q", normalizeRegimeTimeframe(cfg.Regime.Timeframe), normalizeRegimeTimeframe(next.Regime.Timeframe))
		cfg.Regime.Timeframe = normalizeRegimeTimeframe(next.Regime.Timeframe)
	}
	if cfg.Regime != nil && next.Regime != nil && !reflect.DeepEqual(cfg.Regime.Transitions, next.Regime.Transitions) {
		addChange("regime.transitions: %+v -> %+v", cfg.Regime.Transitions, next.Regime.Transitions)
		cfg.Regime.Transitions = cloneRegimeTransitionAlertsConfig(next.Regime.Transitions)
	}
	if cfg.Regime != nil && next.Regime != nil &&
		normalizeRegimeGateOnFailure(cfg.Regime.GateOnFailure) != normalizeRegimeGateOnFailure(next.Regime.GateOnFailure) {
		addChange("regime.gate_on_failure: %q -> %q", cfg.Regime.GateOnFailure, next.Regime.GateOnFailure)
		cfg.Regime.GateOnFailure = next.Regime.GateOnFailure
	}
	if cfg.Regime != nil && next.Regime != nil &&
		normalizeRegimeGateOnFailure(cfg.Regime.HurstGateOnFailure) != normalizeRegimeGateOnFailure(next.Regime.HurstGateOnFailure) {
		addChange("regime.hurst_gate_on_failure: %q -> %q", cfg.Regime.HurstGateOnFailure, next.Regime.HurstGateOnFailure)
		cfg.Regime.HurstGateOnFailure = next.Regime.HurstGateOnFailure
	}

	nextByID := strategyConfigByID(next.Strategies)
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		ns := nextByID[sc.ID]
		oldCapital := sc.Capital

		if sc.MaxDrawdownPct != ns.MaxDrawdownPct {
			addChange("strategy[%s].max_drawdown_pct: %.2f%% -> %.2f%%", sc.ID, sc.MaxDrawdownPct, ns.MaxDrawdownPct)
			sc.MaxDrawdownPct = ns.MaxDrawdownPct
			if ss := stateStrategy(state, sc.ID); ss != nil {
				ss.RiskState.MaxDrawdownPct = ns.MaxDrawdownPct
			}
		}
		if !boolPtrEqual(sc.CircuitBreaker, ns.CircuitBreaker) {
			addChange("strategy[%s].circuit_breaker: %s -> %s", sc.ID, formatCircuitBreaker(sc.CircuitBreaker), formatCircuitBreaker(ns.CircuitBreaker))
			sc.CircuitBreaker = ns.CircuitBreaker
		}
		if !intPtrEqual(sc.CBDrawdownCooldownMinutes, ns.CBDrawdownCooldownMinutes) {
			addChange("strategy[%s].cb_drawdown_cooldown_minutes: %s -> %s", sc.ID, formatCBMinutes(sc.CBDrawdownCooldownMinutes, DefaultCBDrawdownCooldown), formatCBMinutes(ns.CBDrawdownCooldownMinutes, DefaultCBDrawdownCooldown))
			sc.CBDrawdownCooldownMinutes = ns.CBDrawdownCooldownMinutes
		}
		if !intPtrEqual(sc.CBLossStreakThreshold, ns.CBLossStreakThreshold) {
			addChange("strategy[%s].cb_loss_streak_threshold: %s -> %s", sc.ID, formatCBThreshold(sc.CBLossStreakThreshold), formatCBThreshold(ns.CBLossStreakThreshold))
			sc.CBLossStreakThreshold = ns.CBLossStreakThreshold
		}
		if !intPtrEqual(sc.CBLossStreakCooldownMinutes, ns.CBLossStreakCooldownMinutes) {
			addChange("strategy[%s].cb_loss_streak_cooldown_minutes: %s -> %s", sc.ID, formatCBMinutes(sc.CBLossStreakCooldownMinutes, DefaultCBLossStreakCooldown), formatCBMinutes(ns.CBLossStreakCooldownMinutes, DefaultCBLossStreakCooldown))
			sc.CBLossStreakCooldownMinutes = ns.CBLossStreakCooldownMinutes
		}
		if !boolPtrEqual(sc.NotifyRatchetTriggers, ns.NotifyRatchetTriggers) {
			addChange("strategy[%s].notify_ratchet_triggers: %s -> %s", sc.ID, formatNotifyRatchetTriggers(sc.NotifyRatchetTriggers), formatNotifyRatchetTriggers(ns.NotifyRatchetTriggers))
			sc.NotifyRatchetTriggers = ns.NotifyRatchetTriggers
		}
		if !llmEntryAnalysisConfigEqual(sc.LLMEntryAnalysis, ns.LLMEntryAnalysis) {
			addChange("strategy[%s].llm_entry_analysis: %s -> %s", sc.ID, formatLLMEntryAnalysis(sc.LLMEntryAnalysis), formatLLMEntryAnalysis(ns.LLMEntryAnalysis))
			sc.LLMEntryAnalysis = ns.LLMEntryAnalysis
		}
		if sc.Paused != ns.Paused && !sc.sharedWalletModeDeferred {
			addChange("strategy[%s].paused: %t -> %t", sc.ID, sc.Paused, ns.Paused)
			sc.Paused = ns.Paused
		}
		if !boolPtrEqual(sc.AllowDeprecated, ns.AllowDeprecated) {
			addChange("strategy[%s].allow_deprecated: %s -> %s", sc.ID, formatAllowDeprecated(sc.AllowDeprecated), formatAllowDeprecated(ns.AllowDeprecated))
			sc.AllowDeprecated = ns.AllowDeprecated
		}
		if sc.CapitalPct == 0 && sc.Capital != ns.Capital {
			addChange("strategy[%s].capital: $%.2f -> $%.2f", sc.ID, sc.Capital, ns.Capital)
			sc.Capital = ns.Capital
			if ss := stateStrategy(state, sc.ID); ss != nil {
				ss.Cash += ns.Capital - oldCapital
			}
		}
		if sc.Leverage != ns.Leverage {
			addChange("strategy[%s].leverage: %.2fx -> %.2fx", sc.ID, sc.Leverage, ns.Leverage)
			sc.Leverage = ns.Leverage
			if ss := stateStrategy(state, sc.ID); ss != nil && sc.Type == "perps" && ns.Leverage > 0 {
				for _, pos := range ss.Positions {
					if pos != nil {
						pos.Leverage = ns.Leverage
					}
				}
			}
		}
		if sc.SizingLeverage != ns.SizingLeverage {
			addChange("strategy[%s].sizing_leverage: %.2fx -> %.2fx", sc.ID, sc.SizingLeverage, ns.SizingLeverage)
			sc.SizingLeverage = ns.SizingLeverage
		}
		if !floatPtrEqual(sc.MarginPerTradeUSD, ns.MarginPerTradeUSD) {
			addChange("strategy[%s].margin_per_trade_usd: %s -> %s", sc.ID, formatFloatPtrUSD(sc.MarginPerTradeUSD), formatFloatPtrUSD(ns.MarginPerTradeUSD))
			sc.MarginPerTradeUSD = ns.MarginPerTradeUSD
		}
		if !floatPtrEqual(sc.RiskPerTradePct, ns.RiskPerTradePct) {
			addChange("strategy[%s].risk_per_trade_pct: %s -> %s", sc.ID, formatFloatPtrPct(sc.RiskPerTradePct), formatFloatPtrPct(ns.RiskPerTradePct))
			sc.RiskPerTradePct = ns.RiskPerTradePct
		}
		if sc.IntervalSeconds != ns.IntervalSeconds {
			addChange("strategy[%s].interval_seconds: %d -> %d", sc.ID, sc.IntervalSeconds, ns.IntervalSeconds)
			sc.IntervalSeconds = ns.IntervalSeconds
		}
		if sc.InvertSignal != ns.InvertSignal {
			addChange("strategy[%s].invert_signal: %t -> %t", sc.ID, sc.InvertSignal, ns.InvertSignal)
			sc.InvertSignal = ns.InvertSignal
		}
		if !reflect.DeepEqual(sc.OpenStrategy, ns.OpenStrategy) {
			addChange("strategy[%s].open_strategy: %s -> %s", sc.ID, formatStrategyRef(sc.OpenStrategy), formatStrategyRef(ns.OpenStrategy))
			sc.OpenStrategy = ns.OpenStrategy
		}
		if !reflect.DeepEqual(sc.CloseStrategy, ns.CloseStrategy) {
			addChange("strategy[%s].close_strategy: %s -> %s", sc.ID, formatStrategyRefList(sc.closeRefs()), formatStrategyRefList(ns.closeRefs()))
			if ns.CloseStrategy != nil {
				ref := *ns.CloseStrategy
				sc.CloseStrategy = &ref
			} else {
				sc.CloseStrategy = nil
			}
		}
		if !reflect.DeepEqual(sc.AllowedRegimes, ns.AllowedRegimes) {
			addChange("strategy[%s].allowed_regimes: %v -> %v", sc.ID, sc.AllowedRegimes, ns.AllowedRegimes)
			sc.AllowedRegimes = append([]string{}, ns.AllowedRegimes...)
		}
		if normalizeRegimeGateOnFailure(sc.RegimeGateOnFailure) != normalizeRegimeGateOnFailure(ns.RegimeGateOnFailure) {
			addChange("strategy[%s].regime_gate_on_failure: %q -> %q", sc.ID, sc.RegimeGateOnFailure, ns.RegimeGateOnFailure)
			sc.RegimeGateOnFailure = ns.RegimeGateOnFailure
		}
		if !reflect.DeepEqual(sc.HurstGate, ns.HurstGate) {
			addChange("strategy[%s].hurst_gate: %s -> %s", sc.ID, formatHurstGateForLog(sc.HurstGate), formatHurstGateForLog(ns.HurstGate))
			sc.HurstGate = cloneHurstGateConfig(ns.HurstGate)
		}
		if sc.MarginMode != ns.MarginMode {
			addChange("strategy[%s].margin_mode: %q -> %q", sc.ID, sc.MarginMode, ns.MarginMode)
			sc.MarginMode = ns.MarginMode
		}
		if normalizeATRMethod(sc.ATRMethod) != normalizeATRMethod(ns.ATRMethod) {
			addChange("strategy[%s].atr_method: %q -> %q", sc.ID, sc.ATRMethod, ns.ATRMethod)
			sc.ATRMethod = ns.ATRMethod
		}
		if !floatPtrEqual(sc.TrailingStopPct, ns.TrailingStopPct) {
			addChange("strategy[%s].trailing_stop_pct: %s -> %s", sc.ID, formatFloatPtrPct(sc.TrailingStopPct), formatFloatPtrPct(ns.TrailingStopPct))
			sc.TrailingStopPct = ns.TrailingStopPct
		}
		if !floatPtrEqual(sc.TrailingStopATRMult, ns.TrailingStopATRMult) {
			addChange("strategy[%s].trailing_stop_atr_mult: %s -> %s", sc.ID, formatFloatPtr(sc.TrailingStopATRMult), formatFloatPtr(ns.TrailingStopATRMult))
			sc.TrailingStopATRMult = ns.TrailingStopATRMult
			if ns.TrailingStopATRMult == nil || *ns.TrailingStopATRMult <= 0 {
				clearATRMultMissingEntryATRWarningsForStrategy(sc.ID)
			}
		}
		if !floatPtrEqual(sc.StopLossATRMult, ns.StopLossATRMult) {
			addChange("strategy[%s].stop_loss_atr_mult: %s -> %s", sc.ID, formatFloatPtr(sc.StopLossATRMult), formatFloatPtr(ns.StopLossATRMult))
			sc.StopLossATRMult = ns.StopLossATRMult
			if ns.StopLossATRMult == nil || *ns.StopLossATRMult <= 0 {
				clearATRMultMissingEntryATRWarningsForStrategy(sc.ID)
			}
		}
		if !sc.StopLossATRMultRegime.EqualForReload(ns.StopLossATRMultRegime) {
			addChange("strategy[%s].stop_loss_atr_mult_regime: shape updated", sc.ID)
			sc.StopLossATRMultRegime = cloneRegimeATRBlock(ns.StopLossATRMultRegime)
		}
		if !sc.TrailingStopATRMultRegime.EqualForReload(ns.TrailingStopATRMultRegime) {
			addChange("strategy[%s].trailing_stop_atr_mult_regime: shape updated", sc.ID)
			sc.TrailingStopATRMultRegime = cloneRegimeATRBlock(ns.TrailingStopATRMultRegime)
		}
		if !floatPtrEqual(sc.TrailingStopMinMovePct, ns.TrailingStopMinMovePct) {
			addChange("strategy[%s].trailing_stop_min_move_pct: %s -> %s", sc.ID, formatFloatPtrPct(sc.TrailingStopMinMovePct), formatFloatPtrPct(ns.TrailingStopMinMovePct))
			sc.TrailingStopMinMovePct = ns.TrailingStopMinMovePct
		}
		if EffectiveDirection(*sc) != EffectiveDirection(ns) {
			addChange("strategy[%s].direction: %q -> %q", sc.ID, EffectiveDirection(*sc), EffectiveDirection(ns))
			sc.Direction = ns.Direction
			sc.AllowShorts = ns.AllowShorts
		}
		if !sc.RegimeDirectionalPolicy.EqualForReload(ns.RegimeDirectionalPolicy) {
			addChange("strategy[%s].regime_directional_policy: shape updated", sc.ID)
			sc.RegimeDirectionalPolicy = ns.RegimeDirectionalPolicy
		}
		if !sc.RegimeWindowDivergence.EqualForReload(ns.RegimeWindowDivergence) {
			addChange("strategy[%s].regime_window_divergence: shape updated", sc.ID)
			sc.RegimeWindowDivergence = ns.RegimeWindowDivergence
		}
		if !sc.RegimeProfileAllocation.EqualForReload(ns.RegimeProfileAllocation) {
			addChange("strategy[%s].regime_profile_allocation: shape updated", sc.ID)
			sc.RegimeProfileAllocation = ns.RegimeProfileAllocation
			if stratState := stateStrategy(state, sc.ID); stratState != nil {
				if ns.RegimeProfileAllocation.IsConfigured() {
					stratState.RegimeProfile = &RegimeProfileState{ActiveProfile: ns.RegimeProfileAllocation.InitialProfile}
				} else {
					stratState.RegimeProfile = nil
				}
			}
		}
		if !regimeWindowFieldsEqual(*sc, ns) {
			addChange("strategy[%s].regime_*_window: gate=%q atr=%q directional=%q updated",
				sc.ID, ns.RegimeGateWindow, ns.RegimeATRWindow, ns.RegimeDirectionalWindow)
			sc.RegimeGateWindow = ns.RegimeGateWindow
			sc.RegimeATRWindow = ns.RegimeATRWindow
			sc.RegimeDirectionalWindow = ns.RegimeDirectionalWindow
		}
		if sc.AllowScaleIn != ns.AllowScaleIn {
			addChange("strategy[%s].allow_scale_in: %t -> %t", sc.ID, sc.AllowScaleIn, ns.AllowScaleIn)
			sc.AllowScaleIn = ns.AllowScaleIn
		}
		if !scaleInConfigEqual(sc.ScaleIn, ns.ScaleIn) {
			addChange("strategy[%s].scale_in: shape updated", sc.ID)
			if ns.ScaleIn != nil {
				clone := *ns.ScaleIn
				sc.ScaleIn = &clone
			} else {
				sc.ScaleIn = nil
			}
		}
		if !hedgeConfigEqual(sc.Hedge, ns.Hedge) {
			addChange("strategy[%s].hedge: shape updated", sc.ID)
			if ns.Hedge != nil {
				clone := *ns.Hedge
				sc.Hedge = &clone
			} else {
				sc.Hedge = nil
			}
		}
		if normalizeReplaySharing(sc.ReplaySharing) != normalizeReplaySharing(ns.ReplaySharing) {
			addChange("strategy[%s].replay_sharing: %q -> %q", sc.ID, normalizeReplaySharing(sc.ReplaySharing), normalizeReplaySharing(ns.ReplaySharing))
			sc.ReplaySharing = ns.ReplaySharing
		}
	}

	if portfolioRiskMaxDrawdown(cfg.PortfolioRisk) != portfolioRiskMaxDrawdown(next.PortfolioRisk) {
		addChange("portfolio_risk.max_drawdown_pct: %.2f%% -> %.2f%%",
			portfolioRiskMaxDrawdown(cfg.PortfolioRisk), portfolioRiskMaxDrawdown(next.PortfolioRisk))
	}
	if portfolioRiskWarnThreshold(cfg.PortfolioRisk) != portfolioRiskWarnThreshold(next.PortfolioRisk) {
		addChange("portfolio_risk.warn_threshold_pct: %.2f%% -> %.2f%%",
			portfolioRiskWarnThreshold(cfg.PortfolioRisk), portfolioRiskWarnThreshold(next.PortfolioRisk))
	}
	if portfolioRiskDailyMaxLossUSD(cfg.PortfolioRisk) != portfolioRiskDailyMaxLossUSD(next.PortfolioRisk) {
		addChange("portfolio_risk.daily_max_loss_usd: $%.2f -> $%.2f",
			portfolioRiskDailyMaxLossUSD(cfg.PortfolioRisk), portfolioRiskDailyMaxLossUSD(next.PortfolioRisk))
	}
	if portfolioRiskDailyMaxLossPct(cfg.PortfolioRisk) != portfolioRiskDailyMaxLossPct(next.PortfolioRisk) {
		addChange("portfolio_risk.daily_max_loss_pct: %.2f%% -> %.2f%%",
			portfolioRiskDailyMaxLossPct(cfg.PortfolioRisk), portfolioRiskDailyMaxLossPct(next.PortfolioRisk))
	}
	if portfolioRiskMaxSameDirectionNotional(cfg.PortfolioRisk) != portfolioRiskMaxSameDirectionNotional(next.PortfolioRisk) {
		addChange("portfolio_risk.max_same_direction_notional_usd: $%.2f -> $%.2f",
			portfolioRiskMaxSameDirectionNotional(cfg.PortfolioRisk), portfolioRiskMaxSameDirectionNotional(next.PortfolioRisk))
	}
	if portfolioRiskMaxAssetConcentration(cfg.PortfolioRisk) != portfolioRiskMaxAssetConcentration(next.PortfolioRisk) {
		addChange("portfolio_risk.max_asset_concentration_pct: %.2f%% -> %.2f%%",
			portfolioRiskMaxAssetConcentration(cfg.PortfolioRisk), portfolioRiskMaxAssetConcentration(next.PortfolioRisk))
	}
	cfg.PortfolioRisk = clonePortfolioRiskConfig(next.PortfolioRisk)

	if !reflect.DeepEqual(cfg.Discord.Channels, next.Discord.Channels) {
		addChange("discord.channels: %s -> %s", formatStringMap(cfg.Discord.Channels), formatStringMap(next.Discord.Channels))
	}
	if !reflect.DeepEqual(cfg.Discord.DMChannels, next.Discord.DMChannels) {
		addChange("discord.dm_channels: %s -> %s", formatStringMap(cfg.Discord.DMChannels), formatStringMap(next.Discord.DMChannels))
	}
	if !reflect.DeepEqual(cfg.Discord.TradeAlertChannels, next.Discord.TradeAlertChannels) {
		addChange("discord.trade_alert_channels: %s -> %s", formatStringMap(cfg.Discord.TradeAlertChannels), formatStringMap(next.Discord.TradeAlertChannels))
	}
	if cfg.Discord.LeaderboardTopN != next.Discord.LeaderboardTopN {
		addChange("discord.leaderboard_top_n: %d -> %d", cfg.Discord.LeaderboardTopN, next.Discord.LeaderboardTopN)
	}
	if cfg.Discord.LeaderboardChannel != next.Discord.LeaderboardChannel {
		addChange("discord.leaderboard_channel: %q -> %q", cfg.Discord.LeaderboardChannel, next.Discord.LeaderboardChannel)
	}
	cfg.Discord.Channels = cloneStringMap(next.Discord.Channels)
	cfg.Discord.DMChannels = cloneStringMap(next.Discord.DMChannels)
	cfg.Discord.TradeAlertChannels = cloneStringMap(next.Discord.TradeAlertChannels)
	cfg.Discord.LeaderboardTopN = next.Discord.LeaderboardTopN
	cfg.Discord.LeaderboardChannel = next.Discord.LeaderboardChannel

	if !reflect.DeepEqual(cfg.Telegram.Channels, next.Telegram.Channels) {
		addChange("telegram.channels: %s -> %s", formatStringMap(cfg.Telegram.Channels), formatStringMap(next.Telegram.Channels))
	}
	if !reflect.DeepEqual(cfg.Telegram.DMChannels, next.Telegram.DMChannels) {
		addChange("telegram.dm_channels: %s -> %s", formatStringMap(cfg.Telegram.DMChannels), formatStringMap(next.Telegram.DMChannels))
	}
	if !reflect.DeepEqual(cfg.Telegram.TradeAlertChannels, next.Telegram.TradeAlertChannels) {
		addChange("telegram.trade_alert_channels: %s -> %s", formatStringMap(cfg.Telegram.TradeAlertChannels), formatStringMap(next.Telegram.TradeAlertChannels))
	}
	cfg.Telegram.Channels = cloneStringMap(next.Telegram.Channels)
	cfg.Telegram.DMChannels = cloneStringMap(next.Telegram.DMChannels)
	cfg.Telegram.TradeAlertChannels = cloneStringMap(next.Telegram.TradeAlertChannels)

	if !reflect.DeepEqual(cfg.SummaryFrequency, next.SummaryFrequency) {
		addChange("summary_frequency: %s -> %s", formatStringMap(cfg.SummaryFrequency), formatStringMap(next.SummaryFrequency))
	}
	cfg.SummaryFrequency = cloneStringMap(next.SummaryFrequency)

	cfg.ConfigVersion = next.ConfigVersion
	cfg.Platforms = next.Platforms

	rebuildReplayLiveSources(cfg)

	if notifier != nil {
		notifier.ReloadConfig(cfg)
	}
	if server != nil {
		server.UpdateStrategies(cfg.Strategies)
		server.SetConfigContext(server.configPath, cfg)
	}

	sort.Strings(changes)
	return changes, nil
}

func regimeConfigEqualIgnoringReloadableFields(a, b *RegimeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	ac.DisplayWindows = nil
	bc.DisplayWindows = nil
	ac.Timeframe = ""
	bc.Timeframe = ""
	ac.Transitions = nil
	bc.Transitions = nil
	ac.GateOnFailure = ""
	bc.GateOnFailure = ""
	ac.HurstGateOnFailure = ""
	bc.HurstGateOnFailure = ""
	return reflect.DeepEqual(ac, bc)
}

func validateHotReloadCompatible(cfg, next *Config) error {
	var errs []string
	if cfg.DBFile != next.DBFile {
		errs = append(errs, fmt.Sprintf("db_file changed (%q -> %q; restart required)", cfg.DBFile, next.DBFile))
	}
	if cfg.ReplayLogPath != next.ReplayLogPath {
		errs = append(errs, fmt.Sprintf("replay_log_path changed (%q -> %q; restart required)", cfg.ReplayLogPath, next.ReplayLogPath))
	}
	if cfg.LogDir != next.LogDir {
		errs = append(errs, fmt.Sprintf("log_dir changed (%q -> %q; restart required)", cfg.LogDir, next.LogDir))
	}
	if cfg.StatusPort != next.StatusPort {
		errs = append(errs, fmt.Sprintf("status_port changed (%d -> %d; restart required)", cfg.StatusPort, next.StatusPort))
	}
	if cfg.StatusToken != next.StatusToken {
		errs = append(errs, "status token changed (restart required)")
	}
	if cfg.AutoUpdate != next.AutoUpdate {
		errs = append(errs, fmt.Sprintf("auto_update changed (%q -> %q; restart required)", cfg.AutoUpdate, next.AutoUpdate))
	}
	if cfg.LeaderboardPostTime != next.LeaderboardPostTime {
		errs = append(errs, fmt.Sprintf("leaderboard_post_time changed (%q -> %q; restart required)", cfg.LeaderboardPostTime, next.LeaderboardPostTime))
	}
	if !reflect.DeepEqual(cfg.Correlation, next.Correlation) {
		errs = append(errs, "correlation changed (restart required)")
	}
	if !regimeConfigEqualIgnoringReloadableFields(cfg.Regime, next.Regime) {
		errs = append(errs, "regime changed (restart required)")
	}
	if !reflect.DeepEqual(cfg.LeaderboardSummaries, next.LeaderboardSummaries) {
		errs = append(errs, "leaderboard_summaries changed (restart required)")
	}
	if !reflect.DeepEqual(cfg.RiskFreeRate, next.RiskFreeRate) {
		errs = append(errs, "risk_free_rate changed (restart required)")
	}
	if !reflect.DeepEqual(cfg.TradingViewExport, next.TradingViewExport) {
		errs = append(errs, "tradingview_export changed (restart required)")
	}
	if portfolioRiskMaxNotional(cfg.PortfolioRisk) != portfolioRiskMaxNotional(next.PortfolioRisk) {
		errs = append(errs, fmt.Sprintf("portfolio_risk.max_notional_usd changed (%.2f -> %.2f; restart required)",
			portfolioRiskMaxNotional(cfg.PortfolioRisk), portfolioRiskMaxNotional(next.PortfolioRisk)))
	}
	if cfg.Discord.Enabled != next.Discord.Enabled {
		errs = append(errs, "discord.enabled changed (restart required)")
	}
	if cfg.Discord.Token != next.Discord.Token {
		errs = append(errs, "discord.token changed (restart required)")
	}
	if cfg.Discord.OwnerID != next.Discord.OwnerID {
		errs = append(errs, "discord.owner_id changed (restart required)")
	}
	if cfg.Telegram.Enabled != next.Telegram.Enabled {
		errs = append(errs, "telegram.enabled changed (restart required)")
	}
	if cfg.Telegram.BotToken != next.Telegram.BotToken {
		errs = append(errs, "telegram.bot_token changed (restart required)")
	}
	if cfg.Telegram.OwnerChatID != next.Telegram.OwnerChatID {
		errs = append(errs, "telegram.owner_chat_id changed (restart required)")
	}
	if !sameStrategyIDSet(cfg.Strategies, next.Strategies) {
		errs = append(errs, fmt.Sprintf("strategy set changed (current=%v next=%v; restart required)",
			sortedStrategyIDs(cfg.Strategies), sortedStrategyIDs(next.Strategies)))
	}

	nextByID := strategyConfigByID(next.Strategies)
	for _, sc := range cfg.Strategies {
		ns, ok := nextByID[sc.ID]
		if !ok {
			continue
		}
		if usesSharedWalletPoolBudget(sc) != usesSharedWalletPoolBudget(ns) {
			errs = append(errs, fmt.Sprintf(
				"strategy[%s] shared-wallet pool budgeting mode changed (restart required)",
				sc.ID))
		}
		oldShape := strategyRestartShape(sc)
		newShape := strategyRestartShape(ns)
		if !reflect.DeepEqual(oldShape, newShape) {
			errs = append(errs, fmt.Sprintf("strategy[%s] changed non-hot-reloadable fields (restart required)", sc.ID))
		}
	}

	for _, msg := range hyperliquidPeerStrategyErrors(next.Strategies) {
		errs = append(errs, msg)
	}

	for _, msg := range validateHedgeConfigs(next) {
		errs = append(errs, msg)
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config reload rejected:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func validateHotReloadStateCompatible(cfg, next *Config, state *AppState) error {
	var errs []string
	nextByID := strategyConfigByID(next.Strategies)
	for _, sc := range cfg.Strategies {
		ns, ok := nextByID[sc.ID]
		if !ok {
			continue
		}
		if cfg.Regime != nil && next.Regime != nil && sc.Type != "options" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			_, oldTF := strategyRegimeSymbolTimeframe(sc.Args, cfg.Regime)
			_, newTF := strategyRegimeSymbolTimeframe(ns.Args, next.Regime)
			if oldTF != "" && newTF != "" && oldTF != newTF {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime.timeframe changed with open positions (%q -> %q; flatten first or restart after close)",
					sc.ID, oldTF, newTF))
			}
		}
		if sc.Type == "perps" && sc.Leverage != ns.Leverage && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] leverage changed with open positions (%.2fx -> %.2fx; flatten first or restart after close)",
				sc.ID, sc.Leverage, ns.Leverage))
		}
		if (sc.Type == "perps" || sc.Type == "manual") && EffectiveDirection(sc) != EffectiveDirection(ns) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] direction changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, EffectiveDirection(sc), EffectiveDirection(ns)))
		}
		if sc.InvertSignal != ns.InvertSignal && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] invert_signal changed with open positions (%t -> %t; flatten first or restart after close)",
				sc.ID, sc.InvertSignal, ns.InvertSignal))
		}
		if sc.Type != "options" && resolveATRMethod(sc, cfg) != resolveATRMethod(ns, next) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] effective atr_method changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, resolveATRMethod(sc, cfg), resolveATRMethod(ns, next)))
		}
		if (sc.Type == "perps" || sc.Type == "manual") && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			if sc.AllowScaleIn != ns.AllowScaleIn {
				errs = append(errs, fmt.Sprintf("strategy[%s] allow_scale_in changed with open positions (%t -> %t; flatten first or restart after close)",
					sc.ID, sc.AllowScaleIn, ns.AllowScaleIn))
			} else if !scaleInConfigEqual(sc.ScaleIn, ns.ScaleIn) {
				errs = append(errs, fmt.Sprintf("strategy[%s] scale_in shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if !hedgeConfigEqual(sc.Hedge, ns.Hedge) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] hedge block changed with open positions (flatten both the primary and the hedge leg first, or restart after close)",
				sc.ID))
		}
		if normalizeReplaySharing(sc.ReplaySharing) != normalizeReplaySharing(ns.ReplaySharing) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] replay_sharing changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, normalizeReplaySharing(sc.ReplaySharing), normalizeReplaySharing(ns.ReplaySharing)))
		}
		if sc.Type == "perps" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldRiskMode := sc.RiskPerTradePct != nil && *sc.RiskPerTradePct > 0
			newRiskMode := ns.RiskPerTradePct != nil && *ns.RiskPerTradePct > 0
			if oldRiskMode != newRiskMode {
				errs = append(errs, fmt.Sprintf("strategy[%s] risk_per_trade_pct sizing mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && sc.MarginMode != ns.MarginMode && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] margin_mode changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, sc.MarginMode, ns.MarginMode))
		}
		if hyperliquidManagedStopReloadGuard(sc) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldTrailing := sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0
			newTrailing := ns.TrailingStopPct != nil && *ns.TrailingStopPct > 0
			if oldTrailing != newTrailing {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_pct mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldATR := sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0
			newATR := ns.TrailingStopATRMult != nil && *ns.TrailingStopATRMult > 0
			if oldATR != newATR {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_mult mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldFixedATR := sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0
			newFixedATR := ns.StopLossATRMult != nil && *ns.StopLossATRMult > 0
			if oldFixedATR != newFixedATR {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_mult mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldFixedRegime := sc.StopLossATRMultRegime.IsConfigured()
			newFixedRegime := ns.StopLossATRMultRegime.IsConfigured()
			if oldFixedRegime != newFixedRegime {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_mult_regime mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldFixedRegime && !sc.StopLossATRMultRegime.EqualEffectiveForReload(ns.StopLossATRMultRegime) {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_mult_regime shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldTrailingRegime := sc.TrailingStopATRMultRegime.IsConfigured()
			newTrailingRegime := ns.TrailingStopATRMultRegime.IsConfigured()
			if oldTrailingRegime != newTrailingRegime {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_mult_regime mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldTrailingRegime && !sc.TrailingStopATRMultRegime.EqualEffectiveForReload(ns.TrailingStopATRMultRegime) {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_mult_regime shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldConfigured := sc.RegimeDirectionalPolicy.IsConfigured()
			newConfigured := ns.RegimeDirectionalPolicy.IsConfigured()
			if oldConfigured != newConfigured {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_directional_policy mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldConfigured && !sc.RegimeDirectionalPolicy.EqualForReload(ns.RegimeDirectionalPolicy) {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_directional_policy shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldDivConfigured := sc.RegimeWindowDivergence.IsConfigured()
			newDivConfigured := ns.RegimeWindowDivergence.IsConfigured()
			if oldDivConfigured != newDivConfigured {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_window_divergence mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldDivConfigured && !sc.RegimeWindowDivergence.EqualForReload(ns.RegimeWindowDivergence) {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_window_divergence shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldPALConfigured := sc.RegimeProfileAllocation.IsConfigured()
			newPALConfigured := ns.RegimeProfileAllocation.IsConfigured()
			if oldPALConfigured != newPALConfigured {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_profile_allocation mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldPALConfigured && !sc.RegimeProfileAllocation.EqualForReload(ns.RegimeProfileAllocation) {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime_profile_allocation shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		if strategyHasOpenPositions(stateStrategy(state, sc.ID)) && !regimeWindowFieldsEqual(sc, ns) {
			errs = append(errs, fmt.Sprintf("strategy[%s] regime_*_window changed with open positions (flatten first or restart after close)",
				sc.ID))
		}
		if cfg.Regime != nil && next.Regime != nil && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			for _, win := range sortedRegimeWindowNamesFromConfig(cfg.Regime.Windows) {
				if !openPositionsReferenceRegimeWindow(state, win) {
					continue
				}
				newSpec, ok := regimeWindowSpec(next.Regime, win)
				if !ok {
					errs = append(errs, fmt.Sprintf("strategy[%s]: regime.windows[%q] removed while open positions reference it (flatten first)",
						sc.ID, win))
					continue
				}
				oldCls := cfg.Regime.Windows[win].effectiveClassifier()
				newCls := newSpec.effectiveClassifier()
				if oldCls != newCls {
					errs = append(errs, fmt.Sprintf("strategy[%s]: regime.windows[%q] classifier changed with open positions (%q -> %q; flatten first)",
						sc.ID, win, oldCls, newCls))
				}
			}
		}
		if (sc.Type == "perps" || sc.Type == "manual") && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldRules, _ := parseStrategyTPSLAfterRules(sc)
			newRules, _ := parseStrategyTPSLAfterRules(ns)
			if !oldRules.EqualForReload(newRules) {
				errs = append(errs, fmt.Sprintf("strategy[%s] sl_after rules changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			if !unifiedCloseParamsEqualForReload(sc, ns) {
				errs = append(errs, fmt.Sprintf("strategy[%s] unified per-regime close block changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			if strategyUsesTrailingTPRatchetClose(sc) && !trailingRatchetRulesEqualForReload(sc, ns) {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_tp_ratchet tier table changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config reload rejected:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func strategyRestartShape(sc StrategyConfig) StrategyConfig {
	sc.MaxDrawdownPct = 0
	sc.CircuitBreaker = nil
	sc.CBDrawdownCooldownMinutes = nil
	sc.CBLossStreakThreshold = nil
	sc.CBLossStreakCooldownMinutes = nil
	sc.NotifyRatchetTriggers = nil
	sc.Paused = false
	sc.sharedWalletModeDeferred = false
	sc.LLMEntryAnalysis = nil
	sc.AllowDeprecated = nil
	sc.Capital = 0
	sc.Leverage = 0
	sc.SizingLeverage = 0
	sc.MarginPerTradeUSD = nil
	sc.RiskPerTradePct = nil
	sc.IntervalSeconds = 0
	sc.OpenStrategy = StrategyRef{}
	sc.CloseStrategy = nil
	sc.closeStrategiesLegacy = nil
	sc.AllowedRegimes = nil
	sc.RegimeGateOnFailure = ""
	sc.HurstGate = nil
	sc.MarginMode = ""
	sc.TrailingStopPct = nil
	sc.TrailingStopATRMult = nil
	sc.StopLossATRMult = nil
	sc.StopLossATRMultRegime = nil
	sc.TrailingStopATRMultRegime = nil
	sc.TrailingStopMinMovePct = nil
	sc.Direction = ""
	sc.AllowShorts = false
	sc.InvertSignal = false
	sc.RegimeDirectionalPolicy = nil
	sc.RegimeProfileAllocation = nil
	sc.RegimeGateWindow = ""
	sc.RegimeATRWindow = ""
	sc.RegimeDirectionalWindow = ""
	sc.AllowScaleIn = false
	sc.ScaleIn = nil
	sc.ATRMethod = ""
	sc.Hedge = nil
	sc.ReplaySharing = ""
	return sc
}

func scaleInConfigEqual(a, b *ScaleInConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func formatFloatPtrPct(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%.2f%%", *p)
}

func formatFloatPtr(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%g", *p)
}

func formatFloatPtrUSD(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("$%.2f", *p)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func formatCBMinutes(p *int, def time.Duration) string {
	if p == nil {
		return fmt.Sprintf("default(%dm)", int(def/time.Minute))
	}
	return fmt.Sprintf("%dm", *p)
}

func formatCBThreshold(p *int) string {
	if p == nil {
		return fmt.Sprintf("default(%d)", DefaultCBLossStreakThreshold)
	}
	return fmt.Sprintf("%d", *p)
}

func formatCircuitBreaker(p *bool) string {
	if p == nil {
		return "default(on)"
	}
	if *p {
		return "on"
	}
	return "off"
}

func formatAllowDeprecated(p *bool) string {
	if p == nil {
		return "unset"
	}
	if *p {
		return "true"
	}
	return "false"
}

func formatNotifyRatchetTriggers(p *bool) string {
	if p == nil {
		return "inherit-global"
	}
	if *p {
		return "on"
	}
	return "off"
}

func strategyConfigByID(strategies []StrategyConfig) map[string]StrategyConfig {
	out := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		out[sc.ID] = sc
	}
	return out
}

func sameStrategyIDSet(a, b []StrategyConfig) bool {
	aa := sortedStrategyIDs(a)
	bb := sortedStrategyIDs(b)
	return reflect.DeepEqual(aa, bb)
}

func sortedStrategyIDs(strategies []StrategyConfig) []string {
	ids := make([]string, 0, len(strategies))
	for _, sc := range strategies {
		ids = append(ids, sc.ID)
	}
	sort.Strings(ids)
	return ids
}

func stateStrategy(state *AppState, id string) *StrategyState {
	if state == nil || state.Strategies == nil {
		return nil
	}
	return state.Strategies[id]
}

func hyperliquidManagedStopReloadGuard(sc StrategyConfig) bool {
	return sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual")
}

func strategyHasOpenPositions(s *StrategyState) bool {
	if s == nil {
		return false
	}
	for _, pos := range s.Positions {
		if pos != nil && pos.Quantity > 0 {
			return true
		}
	}
	for _, pos := range s.OptionPositions {
		if pos != nil && pos.Quantity != 0 {
			return true
		}
	}
	return false
}

func portfolioRiskMaxDrawdown(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.MaxDrawdownPct
}

func portfolioRiskWarnThreshold(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.WarnThresholdPct
}

func portfolioRiskMaxNotional(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.MaxNotionalUSD
}

func portfolioRiskDailyMaxLossUSD(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.DailyMaxLossUSD
}

func portfolioRiskDailyMaxLossPct(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.DailyMaxLossPct
}

func portfolioRiskMaxSameDirectionNotional(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.MaxSameDirectionNotionalUSD
}

func portfolioRiskMaxAssetConcentration(pr *PortfolioRiskConfig) float64 {
	if pr == nil {
		return 0
	}
	return pr.MaxAssetConcentrationPct
}

func clonePortfolioRiskConfig(pr *PortfolioRiskConfig) *PortfolioRiskConfig {
	if pr == nil {
		return nil
	}
	cp := *pr
	return &cp
}

func cloneTuningConfig(t *TuningConfig) *TuningConfig {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

func cloneRegimeTransitionAlertsConfig(t *RegimeTransitionAlertsConfig) *RegimeTransitionAlertsConfig {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

func cloneManualDefaults(md *ManualDefaultsConfig) *ManualDefaultsConfig {
	if md == nil {
		return nil
	}
	cp := *md
	if md.MarginUSD != nil {
		v := *md.MarginUSD
		cp.MarginUSD = &v
	}
	if md.StopLossATRMult != nil {
		v := *md.StopLossATRMult
		cp.StopLossATRMult = &v
	}
	if len(md.TPTiers) > 0 {
		cp.TPTiers = append([]ManualTPTier(nil), md.TPTiers...)
	}
	cp.TrailingStopATRMultRegime = cloneRegimeATRBlock(md.TrailingStopATRMultRegime)
	return &cp
}

func cloneUserDefaults(ud *UserDefaultsConfig) *UserDefaultsConfig {
	if ud == nil {
		return nil
	}
	return &UserDefaultsConfig{
		Close:     cloneCloseDefaultsMap(ud.Close),
		RegimeATR: cloneInterfaceMap(ud.RegimeATR),
		Manual:    cloneManualDefaults(ud.Manual),
	}
}

func formatUserDefaults(ud *UserDefaultsConfig) string {
	if ud == nil {
		return "(unset)"
	}
	parts := []string{}
	if len(ud.Close) > 0 {
		parts = append(parts, fmt.Sprintf("close=%d", len(ud.Close)))
	}
	if len(ud.RegimeATR) > 0 {
		parts = append(parts, fmt.Sprintf("regime_atr=%d", len(ud.RegimeATR)))
	}
	if ud.Manual != nil {
		parts = append(parts, "manual="+formatManualDefaults(ud.Manual))
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func formatManualDefaults(md *ManualDefaultsConfig) string {
	if md == nil {
		return "(unset)"
	}
	parts := []string{}
	if md.MarginUSD != nil {
		parts = append(parts, fmt.Sprintf("margin_usd=%g", *md.MarginUSD))
	}
	if md.StopLossATRMult != nil {
		parts = append(parts, fmt.Sprintf("stop_loss_atr_mult=%g", *md.StopLossATRMult))
	}
	if md.Side != "" {
		parts = append(parts, fmt.Sprintf("side=%q", md.Side))
	}
	if len(md.TPTiers) > 0 {
		parts = append(parts, fmt.Sprintf("tp_tiers=%d", len(md.TPTiers)))
	}
	if md.TrailingStopATRMultRegime.IsConfigured() {
		parts = append(parts, "trailing_stop_atr_mult_regime=configured")
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func formatStringMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(m))
	for _, k := range keys {
		ordered[k] = m[k]
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(b)
}

func schedulerTickSeconds(cfg *Config) int {
	if cfg == nil {
		return 60
	}
	tickSeconds := cfg.IntervalSeconds
	for _, sc := range cfg.Strategies {
		si := sc.IntervalSeconds
		if si <= 0 {
			si = cfg.IntervalSeconds
		}
		if si < tickSeconds {
			tickSeconds = si
		}
	}
	if tickSeconds < 60 {
		tickSeconds = 60
	}
	return tickSeconds
}
