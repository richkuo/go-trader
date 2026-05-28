package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
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
	if !floatPtrEqual(cfg.DefaultStopLossATRMult, next.DefaultStopLossATRMult) {
		addChange("default_stop_loss_atr_mult: %s -> %s (applies to strategies opened after restart; existing StopLossATRMult on currently-loaded strategies is unchanged)", formatFloatPtr(cfg.DefaultStopLossATRMult), formatFloatPtr(next.DefaultStopLossATRMult))
		cfg.DefaultStopLossATRMult = next.DefaultStopLossATRMult
	}
	// #696: manual_defaults flows through hot-reload so SIGHUP edits to
	// margin_usd / side propagate to subsequent manual-open invocations and
	// stop_loss_atr_mult / tp_tiers shape new type=manual strategy opens. The
	// CLI loads fresh each invocation so it would already pick up changes from
	// disk; the in-process cfg keeps parity for any code that reads via
	// cfg.ManualDefaults / cfg.resolveManual* helpers.
	if !reflect.DeepEqual(cfg.ManualDefaults, next.ManualDefaults) {
		addChange("manual_defaults: %s -> %s", formatManualDefaults(cfg.ManualDefaults), formatManualDefaults(next.ManualDefaults))
		cfg.ManualDefaults = cloneManualDefaults(next.ManualDefaults)
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
		if !reflect.DeepEqual(sc.CloseStrategies, ns.CloseStrategies) {
			addChange("strategy[%s].close_strategies: %s -> %s", sc.ID, formatStrategyRefList(sc.CloseStrategies), formatStrategyRefList(ns.CloseStrategies))
			sc.CloseStrategies = append([]StrategyRef{}, ns.CloseStrategies...)
		}
		if !reflect.DeepEqual(sc.AllowedRegimes, ns.AllowedRegimes) {
			addChange("strategy[%s].allowed_regimes: %v -> %v", sc.ID, sc.AllowedRegimes, ns.AllowedRegimes)
			sc.AllowedRegimes = append([]string{}, ns.AllowedRegimes...)
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
		if !regimeWindowFieldsEqual(*sc, ns) {
			addChange("strategy[%s].regime_*_window: gate=%q atr=%q directional=%q updated",
				sc.ID, ns.RegimeGateWindow, ns.RegimeATRWindow, ns.RegimeDirectionalWindow)
			sc.RegimeGateWindow = ns.RegimeGateWindow
			sc.RegimeATRWindow = ns.RegimeATRWindow
			sc.RegimeDirectionalWindow = ns.RegimeDirectionalWindow
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
	}

	sort.Strings(changes)
	return changes, nil
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
	if !reflect.DeepEqual(cfg.Regime, next.Regime) {
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
		// #486: HL rejects margin-mode changes on an open position; treat
		// the same way as Leverage. Stays hot-reloadable when flat.
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && sc.MarginMode != ns.MarginMode && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			errs = append(errs, fmt.Sprintf("strategy[%s] margin_mode changed with open positions (%q -> %q; flatten first or restart after close)",
				sc.ID, sc.MarginMode, ns.MarginMode))
		}
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && strategyHasOpenPositions(stateStrategy(state, sc.ID)) {
			oldTrailing := sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0
			newTrailing := ns.TrailingStopPct != nil && *ns.TrailingStopPct > 0
			if oldTrailing != newTrailing {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_pct mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			// #505: ATR-derived trailing stop pct is computed once per
			// position from the entry ATR; toggling the mode mid-position
			// would mix two distance regimes against the same on-chain
			// trigger. Treat exactly like trailing_stop_pct.
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
			} else if oldFixedRegime && !sc.StopLossATRRegime.EqualForReload(ns.StopLossATRRegime) {
				errs = append(errs, fmt.Sprintf("strategy[%s] stop_loss_atr_regime shape changed with open positions (flatten first or restart after close)",
					sc.ID))
			}
			oldTrailingRegime := sc.TrailingStopATRRegime.IsConfigured()
			newTrailingRegime := ns.TrailingStopATRRegime.IsConfigured()
			if oldTrailingRegime != newTrailingRegime {
				errs = append(errs, fmt.Sprintf("strategy[%s] trailing_stop_atr_regime mode changed with open positions (flatten first or restart after close)",
					sc.ID))
			} else if oldTrailingRegime && !sc.TrailingStopATRRegime.EqualForReload(ns.TrailingStopATRRegime) {
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
		// #792: per-feature regime window selectors route live gate/ATR/policy
		// lookups; changing them while open would rebind stamped semantics.
		if strategyHasOpenPositions(stateStrategy(state, sc.ID)) && !regimeWindowFieldsEqual(sc, ns) {
			errs = append(errs, fmt.Sprintf("strategy[%s] regime_*_window changed with open positions (flatten first or restart after close)",
				sc.ID))
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
	sc.Capital = 0
	sc.Leverage = 0
	sc.SizingLeverage = 0
	sc.MarginPerTradeUSD = nil // #518: hot-reloadable; nil/positive switching is purely additive
	sc.IntervalSeconds = 0
	sc.OpenStrategy = StrategyRef{}
	sc.CloseStrategies = nil
	sc.AllowedRegimes = nil
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
	return sc
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

func clonePortfolioRiskConfig(pr *PortfolioRiskConfig) *PortfolioRiskConfig {
	if pr == nil {
		return nil
	}
	cp := *pr
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
	return &cp
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
