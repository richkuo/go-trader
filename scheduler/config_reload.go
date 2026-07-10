package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// applyHotReloadConfig applies the subset of config fields that are safe to
// mutate while the scheduler keeps running. The caller must hold mu.Lock when
// invoking this from the main loop.
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
	// #1256: the GLOBAL notify_ratchet_triggers default (#1110) hot-reloads —
	// notification-only, never touches position/order state, mirroring the
	// per-strategy #1118 override handled below. Without this copy a dashboard
	// or Discord toggle of the global would silently wait for a restart.
	if !boolPtrEqual(cfg.NotifyRatchetTriggers, next.NotifyRatchetTriggers) {
		addChange("notify_ratchet_triggers: %s -> %s", formatNotifyRatchetTriggers(cfg.NotifyRatchetTriggers), formatNotifyRatchetTriggers(next.NotifyRatchetTriggers))
		cfg.NotifyRatchetTriggers = next.NotifyRatchetTriggers
	}
	if !floatPtrEqual(cfg.DefaultStopLossATRMult, next.DefaultStopLossATRMult) {
		addChange("default_stop_loss_atr_mult: %s -> %s (applies to strategies opened after restart; existing StopLossATRMult on currently-loaded strategies is unchanged)", formatFloatPtr(cfg.DefaultStopLossATRMult), formatFloatPtr(next.DefaultStopLossATRMult))
		cfg.DefaultStopLossATRMult = next.DefaultStopLossATRMult
	}
	if cfg.AlertThrottleInterval != next.AlertThrottleInterval {
		addChange("alert_throttle_interval: %q -> %q", cfg.AlertThrottleInterval, next.AlertThrottleInterval)
		cfg.AlertThrottleInterval = next.AlertThrottleInterval
		if err := applyAlertThrottleFromConfig(cfg); err != nil {
			return nil, fmt.Errorf("alert_throttle_interval: %w", err)
		}
	}
	// #1135: user_defaults flows through hot-reload so SIGHUP edits to the
	// operator-default layer shape subsequent manual-open invocations, new
	// type=manual defaults, and close-default injection. The CLI loads fresh
	// each invocation, but the in-process cfg keeps parity for code that reads
	// via cfg.resolveManual* helpers and the injected close defaults.
	if !reflect.DeepEqual(cfg.UserDefaults, next.UserDefaults) {
		addChange("user_defaults: %s -> %s", formatUserDefaults(cfg.UserDefaults), formatUserDefaults(next.UserDefaults))
		cfg.UserDefaults = cloneUserDefaults(next.UserDefaults)
	}

	// #1062/#1139: selected top-level regime fields hot-reload. display_windows
	// is display-only; timeframe is state-shifting and validateHotReloadStateCompatible
	// rejects it while affected strategies are open.
	if cfg.Regime != nil && next.Regime != nil && !reflect.DeepEqual(cfg.Regime.DisplayWindows, next.Regime.DisplayWindows) {
		addChange("regime.display_windows: %v -> %v", cfg.Regime.DisplayWindows, next.Regime.DisplayWindows)
		cfg.Regime.DisplayWindows = append([]string(nil), next.Regime.DisplayWindows...)
	}
	if cfg.Regime != nil && next.Regime != nil && normalizeRegimeTimeframe(cfg.Regime.Timeframe) != normalizeRegimeTimeframe(next.Regime.Timeframe) {
		addChange("regime.timeframe: %q -> %q", normalizeRegimeTimeframe(cfg.Regime.Timeframe), normalizeRegimeTimeframe(next.Regime.Timeframe))
		cfg.Regime.Timeframe = normalizeRegimeTimeframe(next.Regime.Timeframe)
	}
	// #1224: transitions alerting is alerting-only — always hot-reloadable.
	// Masked in regimeConfigEqualIgnoringReloadableFields so the pre-flight
	// compat gate above doesn't reject the reload before this copy runs.
	if cfg.Regime != nil && next.Regime != nil && !reflect.DeepEqual(cfg.Regime.Transitions, next.Regime.Transitions) {
		addChange("regime.transitions: %+v -> %+v", cfg.Regime.Transitions, next.Regime.Transitions)
		cfg.Regime.Transitions = cloneRegimeTransitionAlertsConfig(next.Regime.Transitions)
	}
	// #1278: global entry-gate failure-policy default is hot-reloadable —
	// same flat-only open-gating rationale as the per-strategy field below.
	if cfg.Regime != nil && next.Regime != nil &&
		normalizeRegimeGateOnFailure(cfg.Regime.GateOnFailure) != normalizeRegimeGateOnFailure(next.Regime.GateOnFailure) {
		addChange("regime.gate_on_failure: %q -> %q", cfg.Regime.GateOnFailure, next.Regime.GateOnFailure)
		cfg.Regime.GateOnFailure = next.Regime.GateOnFailure
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
		// #1048: per-strategy circuit-breaker toggle is hot-reloadable always,
		// including while a position is open (no state-compat guard). Disabling
		// only suppresses NEW fires from the next cycle; an already-latched CB
		// and any pending circuit close continue to drain. Re-enabling resumes
		// evaluation next cycle and may fire immediately if already past a
		// threshold — intended re-arming. The next CheckRisk reads sc.CircuitBreaker
		// directly, so no state mutation is needed here.
		if !boolPtrEqual(sc.CircuitBreaker, ns.CircuitBreaker) {
			addChange("strategy[%s].circuit_breaker: %s -> %s", sc.ID, formatCircuitBreaker(sc.CircuitBreaker), formatCircuitBreaker(ns.CircuitBreaker))
			sc.CircuitBreaker = ns.CircuitBreaker
		}
		// #1273: circuit-breaker timing/threshold overrides are hot-reloadable
		// always, including while a position is open — they only parameterize
		// FUTURE fires read via the accessors on the next CheckRisk cycle. An
		// already-latched CircuitBreakerUntil in RiskState is never rewritten,
		// so no state mutation is needed here (same stance as #1048).
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
		// #1118: per-strategy notify_ratchet_triggers override is hot-reloadable
		// always, including while a position is open — it only changes whether the
		// ratchet-tighten owner DM is sent, never position/order state. The next
		// cycle's notifyRatchetTrigger call reads the new value via
		// sc.NotifyRatchetTriggersEnabled(cfg), so no state mutation is needed.
		if !boolPtrEqual(sc.NotifyRatchetTriggers, ns.NotifyRatchetTriggers) {
			addChange("strategy[%s].notify_ratchet_triggers: %s -> %s", sc.ID, formatNotifyRatchetTriggers(sc.NotifyRatchetTriggers), formatNotifyRatchetTriggers(ns.NotifyRatchetTriggers))
			sc.NotifyRatchetTriggers = ns.NotifyRatchetTriggers
		}
		// #1137: llm_entry_analysis is hot-reloadable always, including while a
		// position is open — advisory commentary only, never position/order
		// state. New opens read the reloaded block at dispatch; in-flight jobs
		// keep their snapshotted params.
		if !llmEntryAnalysisConfigEqual(sc.LLMEntryAnalysis, ns.LLMEntryAnalysis) {
			addChange("strategy[%s].llm_entry_analysis: %s -> %s", sc.ID, formatLLMEntryAnalysis(sc.LLMEntryAnalysis), formatLLMEntryAnalysis(ns.LLMEntryAnalysis))
			sc.LLMEntryAnalysis = ns.LLMEntryAnalysis
		}
		// #1150: per-strategy pause is hot-reloadable always, including while a
		// position is open — pausing only holds position-increasing signals from
		// the next cycle (closes, trailing SL, ratchet, and protection sync keep
		// running), and resuming just lets entries flow again. The dispatch reads
		// sc.Paused from the reloaded config, so no state mutation is needed.
		if sc.Paused != ns.Paused {
			addChange("strategy[%s].paused: %t -> %t", sc.ID, sc.Paused, ns.Paused)
			sc.Paused = ns.Paused
		}
		// #1275: allow_deprecated is hot-reloadable always, including while a
		// position is open — an acknowledgment flag only, never gates loading,
		// probing, or trading. reloadConfig re-evaluates the deprecated-edge
		// warning after apply, so flipping the ack off re-warns immediately.
		if sc.AllowDeprecated != ns.AllowDeprecated {
			addChange("strategy[%s].allow_deprecated: %t -> %t", sc.ID, sc.AllowDeprecated, ns.AllowDeprecated)
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
		// #1268: risk_per_trade_pct value tweaks hot-reload (they only shape
		// the NEXT open/flip sizing); risk↔notional mode switches while a
		// position is open are blocked upstream in validateHotReloadStateCompatible.
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
		// #1278: entry-gate failure policy is hot-reloadable always, including
		// while a position is open — it only changes flat-strategy open gating
		// from the next cycle (mirrors the #1150 pause pattern); closes,
		// trailing SL, ratchet, and protection sync are untouched.
		if normalizeRegimeGateOnFailure(sc.RegimeGateOnFailure) != normalizeRegimeGateOnFailure(ns.RegimeGateOnFailure) {
			addChange("strategy[%s].regime_gate_on_failure: %q -> %q", sc.ID, sc.RegimeGateOnFailure, ns.RegimeGateOnFailure)
			sc.RegimeGateOnFailure = ns.RegimeGateOnFailure
		}
		// #486: Margin mode is hot-reloadable when flat. The state-compat
		// check above blocks the change when positions are open; if we got
		// here with new MarginMode != current, the strategy is flat and the
		// next fresh open will pick up the new mode via update_leverage.
		if sc.MarginMode != ns.MarginMode {
			addChange("strategy[%s].margin_mode: %q -> %q", sc.ID, sc.MarginMode, ns.MarginMode)
			sc.MarginMode = ns.MarginMode
		}
		if !floatPtrEqual(sc.TrailingStopPct, ns.TrailingStopPct) {
			addChange("strategy[%s].trailing_stop_pct: %s -> %s", sc.ID, formatFloatPtrPct(sc.TrailingStopPct), formatFloatPtrPct(ns.TrailingStopPct))
			sc.TrailingStopPct = ns.TrailingStopPct
		}
		if !floatPtrEqual(sc.TrailingStopATRMult, ns.TrailingStopATRMult) {
			addChange("strategy[%s].trailing_stop_atr_mult: %s -> %s", sc.ID, formatFloatPtr(sc.TrailingStopATRMult), formatFloatPtr(ns.TrailingStopATRMult))
			sc.TrailingStopATRMult = ns.TrailingStopATRMult
			// #505 review: when ATR-mult is disabled (or zeroed) the
			// missing-EntryATR throttle no longer applies — drop any
			// outstanding keys for this strategy so the next regime
			// (e.g. fixed trailing_stop_pct or no trailing stop at all)
			// starts with a clean slate.
			if ns.TrailingStopATRMult == nil || *ns.TrailingStopATRMult <= 0 {
				clearATRMultMissingEntryATRWarningsForStrategy(sc.ID)
			}
		}
		if !floatPtrEqual(sc.StopLossATRMult, ns.StopLossATRMult) {
			addChange("strategy[%s].stop_loss_atr_mult: %s -> %s", sc.ID, formatFloatPtr(sc.StopLossATRMult), formatFloatPtr(ns.StopLossATRMult))
			sc.StopLossATRMult = ns.StopLossATRMult
			// #562: same throttle key as trailing_stop_atr_mult — clear when disabled.
			if ns.StopLossATRMult == nil || *ns.StopLossATRMult <= 0 {
				clearATRMultMissingEntryATRWarningsForStrategy(sc.ID)
			}
		}
		if !sc.StopLossATRRegime.EqualForReload(ns.StopLossATRRegime) {
			addChange("strategy[%s].stop_loss_atr_regime: shape updated", sc.ID)
			sc.StopLossATRRegime = cloneRegimeATRBlock(ns.StopLossATRRegime)
		}
		if !sc.TrailingStopATRRegime.EqualForReload(ns.TrailingStopATRRegime) {
			addChange("strategy[%s].trailing_stop_atr_regime: shape updated", sc.ID)
			sc.TrailingStopATRRegime = cloneRegimeATRBlock(ns.TrailingStopATRRegime)
		}
		if !floatPtrEqual(sc.TrailingStopMinMovePct, ns.TrailingStopMinMovePct) {
			addChange("strategy[%s].trailing_stop_min_move_pct: %s -> %s", sc.ID, formatFloatPtrPct(sc.TrailingStopMinMovePct), formatFloatPtrPct(ns.TrailingStopMinMovePct))
			sc.TrailingStopMinMovePct = ns.TrailingStopMinMovePct
		}
		// #656: direction (long|short|both) is hot-reloadable when flat. The
		// state-compat check above blocks the change when positions are open;
		// if we got here with a different direction, the strategy is flat and
		// the next signal observes the new gate. Compare via EffectiveDirection
		// so legacy AllowShorts toggles map cleanly.
		if EffectiveDirection(*sc) != EffectiveDirection(ns) {
			addChange("strategy[%s].direction: %q -> %q", sc.ID, EffectiveDirection(*sc), EffectiveDirection(ns))
			sc.Direction = ns.Direction
			sc.AllowShorts = ns.AllowShorts
		}
		// #779: regime_directional_policy mutation when flat. State-compat
		// gate above blocks the change while a position is open; if we got
		// here with a different shape, the strategy is flat and the next
		// cycle's resolver reads the new map. Compare structural equality.
		if !sc.RegimeDirectionalPolicy.EqualForReload(ns.RegimeDirectionalPolicy) {
			addChange("strategy[%s].regime_directional_policy: shape updated", sc.ID)
			sc.RegimeDirectionalPolicy = ns.RegimeDirectionalPolicy
		}
		// #907: regime_window_divergence mutation when flat.
		if !sc.RegimeWindowDivergence.EqualForReload(ns.RegimeWindowDivergence) {
			addChange("strategy[%s].regime_window_divergence: shape updated", sc.ID)
			sc.RegimeWindowDivergence = ns.RegimeWindowDivergence
		}
		// #998: regime_profile_allocation mutation when flat. State-compat gate
		// blocks the reshape while a position is open; reaching here flat means
		// the next cycle resolves against the new profiles. A reshape invalidates
		// the running switch state machine, so reset the active profile to the
		// new initial and zero the pending counter.
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
		// #873: scale-in config is hot-reloadable when flat. The state-compat
		// gate above blocks the change while a position is open; if we got here
		// with a difference, the strategy is flat and the next signal/manual-add
		// reads the new gate.
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
	}

	if portfolioRiskMaxDrawdown(cfg.PortfolioRisk) != portfolioRiskMaxDrawdown(next.PortfolioRisk) {
		addChange("portfolio_risk.max_drawdown_pct: %.2f%% -> %.2f%%",
			portfolioRiskMaxDrawdown(cfg.PortfolioRisk), portfolioRiskMaxDrawdown(next.PortfolioRisk))
	}
	if portfolioRiskWarnThreshold(cfg.PortfolioRisk) != portfolioRiskWarnThreshold(next.PortfolioRisk) {
		addChange("portfolio_risk.warn_threshold_pct: %.2f%% -> %.2f%%",
			portfolioRiskWarnThreshold(cfg.PortfolioRisk), portfolioRiskWarnThreshold(next.PortfolioRisk))
	}
	// #1269: daily loss limit thresholds hot-reload with the same clone below —
	// including while tripped (the gate re-evaluates each cycle from config).
	if portfolioRiskDailyMaxLossUSD(cfg.PortfolioRisk) != portfolioRiskDailyMaxLossUSD(next.PortfolioRisk) {
		addChange("portfolio_risk.daily_max_loss_usd: $%.2f -> $%.2f",
			portfolioRiskDailyMaxLossUSD(cfg.PortfolioRisk), portfolioRiskDailyMaxLossUSD(next.PortfolioRisk))
	}
	if portfolioRiskDailyMaxLossPct(cfg.PortfolioRisk) != portfolioRiskDailyMaxLossPct(next.PortfolioRisk) {
		addChange("portfolio_risk.daily_max_loss_pct: %.2f%% -> %.2f%%",
			portfolioRiskDailyMaxLossPct(cfg.PortfolioRisk), portfolioRiskDailyMaxLossPct(next.PortfolioRisk))
	}
	// #1270: exposure-cap thresholds hot-reload with the same clone below —
	// including while blocking (the gate re-evaluates each cycle from config).
	// Deliberate divergence from max_notional_usd, which stays restart-required
	// (validateHotReloadCompatible).
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

// regimeConfigEqualIgnoringReloadableFields reports whether two regime configs
// are identical except for hot-reloadable fields (#1062/#1139/#1224). nil-vs-
// non-nil counts as a difference (regime add/remove is restart-required).
// Copies the structs before zeroing fields so the live configs are untouched;
// Windows (a map) is only read by DeepEqual. Every field masked here MUST have
// a matching copy branch in applyHotReloadConfig — otherwise the field is
// documented as hot-reloadable but this gate rejects the reload before the
// copy logic ever runs.
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
	ac.GateOnFailure = "" // #1278: hot-reloadable — explicit apply path in applyHotReloadConfig
	bc.GateOnFailure = ""
	return reflect.DeepEqual(ac, bc)
}

func validateHotReloadCompatible(cfg, next *Config) error {
	var errs []string
	if cfg.DBFile != next.DBFile {
		errs = append(errs, fmt.Sprintf("db_file changed (%q -> %q; restart required)", cfg.DBFile, next.DBFile))
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
	// #1062/#1139: mask top-level regime fields with explicit apply paths.
	// Any OTHER regime field change still rejects.
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
		oldShape := strategyRestartShape(sc)
		newShape := strategyRestartShape(ns)
		if !reflect.DeepEqual(oldShape, newShape) {
			errs = append(errs, fmt.Sprintf("strategy[%s] changed non-hot-reloadable fields (restart required)", sc.ID))
		}
	}

	// #491: re-run peer-strategy validation against the new config so that
	// reloads can't introduce a peer-conflict that startup would have caught.
	for _, msg := range hyperliquidPeerStrategyErrors(next.Strategies) {
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
		// #656: direction change with open positions risks orphaning the
		// existing side or flipping it on the next signal. Block until flat;
		// numeric changes when flat take effect on the next cycle. Compares
		// EffectiveDirection so legacy AllowShorts toggles map to "long"/"both"
		// and behave identically. Manual strategies use the same Direction
		// gate to authorize manual-open --side, so they get the same flatten-
		// first guard for symmetry.
		if (sc.Type == "perps" || sc.Type == "manual") && EffectiveDirection(sc) != EffectiveDirection(ns) && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] direction changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, EffectiveDirection(sc), EffectiveDirection(ns)))
		}
		// invert_signal flips BUY<->SELL on the very next signal — toggling
		// while a position is open re-interprets the same signal as a close
		// (for the side that's now opposite direction), risking an unintended
		// flatten. Block until flat, same shape as the Direction guard above.
		if sc.InvertSignal != ns.InvertSignal && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] invert_signal changed with open positions (%t -> %t; flatten first or restart after close)",
				sc.ID, sc.InvertSignal, ns.InvertSignal))
		}
		// #873: scale-in config only gates the NEXT add decision, but mutating
		// it mid-position is surprising (e.g. flipping add_spacing_atr sign, or
		// lowering a cap below the current count). Block toggle/shape changes
		// while open; edits when flat take effect on the next cycle. Applies to
		// both perps (strategy-flag adds) and manual (manual-add).
		if (sc.Type == "perps" || sc.Type == "manual") && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			if sc.AllowScaleIn != ns.AllowScaleIn {
				errs = append(errs, fmt.Sprintf("strategy[%s] allow_scale_in changed with open positions (%t -> %t; flatten first or restart after close)",
					sc.ID, sc.AllowScaleIn, ns.AllowScaleIn))
			} else if !scaleInConfigEqual(sc.ScaleIn, ns.ScaleIn) {
				errs = append(errs, fmt.Sprintf("strategy[%s] scale_in shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		// #1268: switching between risk-per-trade and notional sizing while a
		// position is open changes what the NEXT flip/re-entry sizing means
		// under the operator's feet — same shape as the scalar↔regime stop
		// owner rule. Value tweaks (set→set) stay hot-reloadable; the switch
		// applies when flat.
		if sc.Type == "perps" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldRiskMode := sc.RiskPerTradePct != nil && *sc.RiskPerTradePct > 0
			newRiskMode := ns.RiskPerTradePct != nil && *ns.RiskPerTradePct > 0
			if oldRiskMode != newRiskMode {
				errs = append(errs, fmt.Sprintf("strategy[%s] risk_per_trade_pct sizing mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		// #486: HL rejects margin-mode changes on an open position; treat
		// the same way as Leverage. Stays hot-reloadable when flat.
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
			// #505/#1115: ATR-derived trailing stop pct is computed once per
			// position from the entry ATR; toggling the mode mid-position would
			// mix two distance regimes against the same on-chain trigger. Applies
			// to both HL perps and HL manual protection.
			oldATR := sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0
			newATR := ns.TrailingStopATRMult != nil && *ns.TrailingStopATRMult > 0
			if oldATR != newATR {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_mult mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			// #562: Fixed ATR-derived stop loss is armed once at open from the
			// entry ATR; toggling on/off mid-position would either leave the
			// resting trigger orphaned or arm a second trigger that races. Block
			// the mode switch while open. Numeric changes (positive→positive)
			// take effect on the next fresh open since the trigger is fixed.
			oldFixedATR := sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0
			newFixedATR := ns.StopLossATRMult != nil && *ns.StopLossATRMult > 0
			if oldFixedATR != newFixedATR {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_mult mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			// #733: regime-aware SL / trailing fields. Scalar↔regime mode
			// flips are blocked because the resting on-chain trigger was
			// sized for one distance regime and would race against a
			// re-derived target under the new shape. Shape-level changes
			// (use_defaults ↔ explicit, mutating per-regime ATR values) are
			// blocked for the same reason — the existing trigger is armed
			// against the resolved-at-open value.
			oldFixedRegime := sc.StopLossATRRegime.IsConfigured()
			newFixedRegime := ns.StopLossATRRegime.IsConfigured()
			if oldFixedRegime != newFixedRegime {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_regime mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldFixedRegime && !sc.StopLossATRRegime.EqualEffectiveForReload(ns.StopLossATRRegime) {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_regime shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldTrailingRegime := sc.TrailingStopATRRegime.IsConfigured()
			newTrailingRegime := ns.TrailingStopATRRegime.IsConfigured()
			if oldTrailingRegime != newTrailingRegime {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_regime mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldTrailingRegime && !sc.TrailingStopATRRegime.EqualEffectiveForReload(ns.TrailingStopATRRegime) {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_regime shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
		}
		// #779: regime_directional_policy shape changes (add/remove/mutate)
		// while a position is open would shift the resolver's effective
		// (Direction, InvertSignal) underneath the held position. Since
		// effectiveRegimeForPolicy uses pos.Regime while open — by design
		// so the policy that opened the position governs its lifecycle —
		// mutating the per-regime entry for pos.Regime mid-position can
		// silently change what counts as a "close" signal. Block the
		// reshape; changes when flat take effect on the next cycle.
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
		// #907: regime_window_divergence shape changes while a position is open
		// would shift the override direction underneath the held position. Block
		// the reshape; changes when flat take effect on the next cycle.
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
		// #998: regime_profile_allocation shape changes while a position is open
		// would re-bind the frozen active profile (pos.OpenProfile governs the
		// held position by design). Block the reshape; changes when flat take
		// effect on the next cycle.
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
		// #792: per-feature regime window selectors route live gate/ATR/policy
		// lookups; changing them while open would rebind stamped semantics.
		if strategyHasOpenPositions(stateStrategy(state, sc.ID)) && !regimeWindowFieldsEqual(sc, ns) {
			errs = append(errs, fmt.Sprintf("strategy[%s] regime_*_window changed with open positions (flatten first or restart after close)",
				sc.ID))
		}
		// #795: classifier swap or window removal on a referenced window blocks reload.
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
		// #716 item 1: sl_after rules are armed at the next cleared TP tier; a
		// mid-position add/remove/mode change would engage the post-TP machinery
		// (and, for trail_from_here, the trailing walker) without the validation
		// the open respected. Numeric mult changes are also gated because they
		// alter the trigger target a future tier-fill will install — operators
		// must flatten to opt into the new rule set.
		if (sc.Type == "perps" || sc.Type == "manual") && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldRules, _ := parseStrategyTPSLAfterRules(sc)
			newRules, _ := parseStrategyTPSLAfterRules(ns)
			if !oldRules.EqualForReload(newRules) {
				errs = append(errs, fmt.Sprintf("strategy[%s] sl_after rules changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			// #841 2b: the unified per-regime close block carries the whole exit
			// plan (per-regime TP ladder + SL + sl_after) armed at open. The
			// empty-regime sl_after parse above can't see it, so gate the block
			// as a unit — any change (incl. add/remove) re-arms a plan the open
			// didn't respect.
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
	sc.CircuitBreaker = nil              // #1048: hot-reloadable always, including while open. No state-compat guard — disabling only suppresses new fires; an already-latched CB and pending close still drain, and re-enabling just resumes evaluation on the next cycle.
	sc.CBDrawdownCooldownMinutes = nil   // #1273: hot-reloadable always, including while open — parameterizes only FUTURE fires; a latched CircuitBreakerUntil is never rewritten. Applied in applyHotReloadConfig.
	sc.CBLossStreakThreshold = nil       // #1273: same stance — the next CheckRisk cycle reads the new threshold via the accessor.
	sc.CBLossStreakCooldownMinutes = nil // #1273: same stance as the drawdown cooldown.
	sc.NotifyRatchetTriggers = nil       // #1118: hot-reloadable always, including while open — notification preference only, never touches position/order state. Masked here so a pure notify_ratchet_triggers toggle isn't flagged "restart required"; applied in applyHotReloadConfig.
	sc.Paused = false                    // #1150: hot-reloadable always, including while open. Pausing only holds position-increasing signals from the next cycle — closes, trailing SL, ratchet, and protection sync keep running — so toggling mid-position never strands protection. Applied in applyHotReloadConfig.
	sc.LLMEntryAnalysis = nil            // #1137: hot-reloadable always, including while open — advisory-only entry commentary, never touches position/order state. Applied in applyHotReloadConfig.
	sc.AllowDeprecated = false           // #1275: hot-reloadable always, including while open — acknowledgment flag only, never gates loading, probing, or trading. Applied in applyHotReloadConfig; reloadConfig re-evaluates the deprecated-edge warning after apply, so flipping the ack off re-warns.
	sc.Capital = 0
	sc.Leverage = 0
	sc.SizingLeverage = 0
	sc.MarginPerTradeUSD = nil // #518: hot-reloadable; nil/positive switching is purely additive
	sc.RiskPerTradePct = nil   // #1268: hot-reloadable; state-compat blocks risk↔notional mode switches while open
	sc.IntervalSeconds = 0
	sc.OpenStrategy = StrategyRef{}
	sc.CloseStrategy = nil
	sc.closeStrategiesLegacy = nil
	sc.AllowedRegimes = nil
	sc.RegimeGateOnFailure = ""      // #1278: hot-reloadable always, including while open — flat-only open gating, never state-shifting. Applied in applyHotReloadConfig.
	sc.MarginMode = ""               // #486: hot-reloadable when flat (state-compat check enforces flat-only change)
	sc.TrailingStopPct = nil         // #501: hot-reloadable; state-compat allows pct changes but blocks mode switches while open
	sc.TrailingStopATRMult = nil     // #505: hot-reloadable; same state-compat treatment as TrailingStopPct
	sc.StopLossATRMult = nil         // #562: hot-reloadable; mode toggle blocked while open
	sc.StopLossATRRegime = nil       // #733: hot-reloadable; state-compat blocks scalar↔regime + shape changes while open
	sc.TrailingStopATRRegime = nil   // #733: hot-reloadable; state-compat blocks scalar↔regime + shape changes while open
	sc.TrailingStopMinMovePct = nil  // #501: hot-reloadable tuning knob for trailing trigger churn
	sc.Direction = ""                // #656: hot-reloadable when flat; state-compat blocks change while open
	sc.AllowShorts = false           // #656: legacy field — direction change is what gates hot reload
	sc.InvertSignal = false          // #775: hot-reloadable; state-compat blocks change while open. Needed in shape mask so the immutable-fields DeepEqual doesn't flag a pure invert_signal toggle as "restart required" (parallel to Direction above).
	sc.RegimeDirectionalPolicy = nil // #779: hot-reloadable; state-compat blocks add/remove/reshape while open
	sc.RegimeProfileAllocation = nil // #998: hot-reloadable when flat; state-compat blocks add/remove/reshape while open
	sc.RegimeGateWindow = ""         // #792: hot-reloadable when flat; state-compat blocks change while open
	sc.RegimeATRWindow = ""          // #792: hot-reloadable when flat; state-compat blocks change while open
	sc.RegimeDirectionalWindow = ""  // #792: hot-reloadable when flat; state-compat blocks change while open
	sc.AllowScaleIn = false          // #873: hot-reloadable when flat; state-compat blocks change while open
	sc.ScaleIn = nil                 // #873: hot-reloadable when flat; state-compat blocks change while open
	return sc
}

// scaleInConfigEqual reports whether two scale_in blocks are identical for
// hot-reload purposes (#873). Treats nil and a zero-value block as distinct
// only by pointer presence so a bare allow_scale_in toggle is caught by the
// AllowScaleIn comparison, not here.
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

// formatCBMinutes renders an optional minutes override for reload change logs.
// nil → "default(<n>m)" derived from the built-in cooldown so an operator sees
// the implicit value a cleared override falls back to. (#1273)
func formatCBMinutes(p *int, def time.Duration) string {
	if p == nil {
		return fmt.Sprintf("default(%dm)", int(def/time.Minute))
	}
	return fmt.Sprintf("%dm", *p)
}

// formatCBThreshold renders the optional loss-streak-threshold override for
// reload change logs. nil → "default(<n>)". (#1273)
func formatCBThreshold(p *int) string {
	if p == nil {
		return fmt.Sprintf("default(%d)", DefaultCBLossStreakThreshold)
	}
	return fmt.Sprintf("%d", *p)
}

// formatCircuitBreaker renders the per-strategy circuit-breaker flag for reload
// change logs. nil → "default(on)" so an operator sees the implicit-enabled
// state explicitly; explicit true/false → "on"/"off". (#1048)
func formatCircuitBreaker(p *bool) string {
	if p == nil {
		return "default(on)"
	}
	if *p {
		return "on"
	}
	return "off"
}

// formatNotifyRatchetTriggers renders the per-strategy notify_ratchet_triggers
// override for reload change logs. nil → "inherit-global" so an operator sees the
// strategy is falling through to Config.NotifyRatchetTriggersEnabled() rather than
// pinning a value; explicit true/false → "on"/"off". (#1118)
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

// cloneRegimeTransitionAlertsConfig deep-copies the #1224 transitions-alerting
// block for hot-reload. The struct is flat (bool + three ints, no nested
// pointers/slices) so a shallow copy is a full deep copy; mirrors the
// defensive posture of clonePortfolioRiskConfig even though the freshly
// LoadConfig'd `next` this is copied from is discarded after the reload call.
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
	cp.TrailingStopATRRegime = cloneRegimeATRBlock(md.TrailingStopATRRegime)
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
	if md.TrailingStopATRRegime.IsConfigured() {
		parts = append(parts, "trailing_stop_atr_regime=configured")
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
