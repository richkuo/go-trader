package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	closedBarDecisionsFlag      = "--closed-bar-decisions"
	decisionRegimeTimeframeFlag = "--decision-regime-timeframe"
)

var closedBarDecisionIntervalsMs = map[string]int64{
	"1m":  60_000,
	"3m":  180_000,
	"5m":  300_000,
	"15m": 900_000,
	"30m": 1_800_000,
	"1h":  3_600_000,
	"2h":  7_200_000,
	"4h":  14_400_000,
	"6h":  21_600_000,
	"8h":  28_800_000,
	"12h": 43_200_000,
	"1d":  86_400_000,
}

var closedBarUnsupportedOpenStrategies = map[string]string{
	fundingScalarStrategyName:        "its scalar and rolling funding average have no historical value at the decision boundary",
	openInterestBreakoutStrategyName: "its open-interest observations have no closed-bar decision contract yet",
}

type ClosedBarDecision struct {
	Enabled            bool   `json:"enabled"`
	Held               bool   `json:"held"`
	HoldReason         string `json:"hold_reason,omitempty"`
	CutoffMs           int64  `json:"cutoff_ms"`
	DecisionBoundaryMs int64  `json:"decision_boundary_ms,omitempty"`
	BarOpenMs          int64  `json:"bar_open_ms,omitempty"`
	ClosureRule        string `json:"closure_rule,omitempty"`
	IntervalMs         int64  `json:"interval_ms,omitempty"`
	Rows               int    `json:"rows,omitempty"`
	FormingRowsDropped int    `json:"forming_rows_dropped,omitempty"`
	InputSHA256        string `json:"input_sha256,omitempty"`
}

type closedBarProvider struct {
	Label  string
	Script string
}

func closedBarDecisionProvider(sc StrategyConfig) (closedBarProvider, string) {
	switch {
	case sc.Type == "manual":
		return closedBarProvider{}, "manual strategies have no candle decision"
	case sc.Type == "options":
		return closedBarProvider{}, "options strategies have no candle decision contract"
	case sc.Type == "futures":
		return closedBarProvider{}, "TopStep candles have no verified closure contract that accounts for trading sessions"
	case sc.Platform == "robinhood":
		return closedBarProvider{}, "Robinhood candles have no verified closure contract that accounts for trading sessions"
	case sc.Type == "perps" && sc.Platform == "hyperliquid":
		return closedBarProvider{Label: "Hyperliquid perps", Script: hyperliquidCheckScript}, ""
	case (sc.Type == "spot" || sc.Type == "perps") && sc.Platform == "okx":
		return closedBarProvider{Label: "OKX " + sc.Type, Script: "shared_scripts/check_okx.py"}, ""
	case sc.Type == "spot" && sc.Platform == "binanceus":
		return closedBarProvider{Label: "Binance.US spot", Script: "shared_scripts/check_strategy.py"}, ""
	}
	return closedBarProvider{}, fmt.Sprintf("type=%s platform=%s has no verified candle closure contract", sc.Type, sc.Platform)
}

func closedBarIntervalSupported(sc StrategyConfig, timeframe string) bool {
	if _, ok := closedBarDecisionIntervalsMs[timeframe]; !ok {
		return false
	}
	if sc.Platform == "hyperliquid" {
		_, ok := hlCandleIntervalMs(timeframe)
		return ok
	}
	return true
}

func closedBarSupportedIntervals() string {
	out := make([]string, 0, len(closedBarDecisionIntervalsMs))
	for tf := range closedBarDecisionIntervalsMs {
		out = append(out, tf)
	}
	sort.Slice(out, func(i, j int) bool {
		return closedBarDecisionIntervalsMs[out[i]] < closedBarDecisionIntervalsMs[out[j]]
	})
	return strings.Join(out, ", ")
}

func closedBarDecisionStrategyErrors(sc StrategyConfig, cfg *Config) []string {
	if !sc.ClosedBarDecisions {
		return nil
	}
	prefix := fmt.Sprintf("strategy[%s]: closed_bar_decisions", sc.ID)
	provider, why := closedBarDecisionProvider(sc)
	if why != "" {
		return []string{fmt.Sprintf("%s is not supported: %s", prefix, why)}
	}
	var errs []string
	if filepath.Clean(strings.TrimSpace(sc.Script)) != provider.Script {
		errs = append(errs, fmt.Sprintf("%s supports only the built-in %s check %s, got script %q", prefix, provider.Label, provider.Script, sc.Script))
	}
	_, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if timeframe == "" {
		errs = append(errs, fmt.Sprintf("%s needs args <strategy> <symbol> <timeframe>", prefix))
	} else if !closedBarIntervalSupported(sc, timeframe) {
		errs = append(errs, fmt.Sprintf("%s does not support timeframe %q (fixed-duration intervals only: %s)", prefix, timeframe, closedBarSupportedIntervals()))
	}
	names := []string{strategyNameFromArgs(sc.Args), effectiveOpenStrategy(sc)}
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if reason, ok := closedBarUnsupportedOpenStrategies[name]; ok {
			errs = append(errs, fmt.Sprintf("%s does not support %s: %s", prefix, name, reason))
		}
	}
	if sc.Platform == "okx" && effectiveOpenStrategy(sc) == fundingRecordsStrategyName {
		errs = append(errs, fmt.Sprintf("%s does not support %s on OKX: the OKX check has no timestamped funding history", prefix, fundingRecordsStrategyName))
	}
	if sc.HTFFilter && timeframe != "" && effectiveOpenStrategy(sc) != fundingScalarStrategyName && effectiveOpenStrategy(sc) != fundingRecordsStrategyName {
		if htf := hlFeedHTFTimeframe(timeframe); !closedBarIntervalSupported(sc, htf) {
			errs = append(errs, fmt.Sprintf("%s does not support the higher-timeframe filter interval %q for timeframe %q", prefix, htf, timeframe))
		}
	}
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	if tf := closedBarDecisionRegimeTimeframe(sc, rc); tf != "" && !closedBarIntervalSupported(sc, tf) {
		errs = append(errs, fmt.Sprintf("%s does not support regime timeframe %q (fixed-duration intervals only: %s)", prefix, tf, closedBarSupportedIntervals()))
	}
	if sc.RegimeDirectionalPolicy.IsConfigured() {
		errs = append(errs, fmt.Sprintf("%s does not support regime_directional_policy: the policy resolves before the check runs, so it would read the current regime", prefix))
	}
	if sc.RegimeWindowDivergence.IsConfigured() {
		errs = append(errs, fmt.Sprintf("%s does not support regime_window_divergence: the override resolves before the check runs, so it would read the current regime", prefix))
	}
	if sc.RegimeProfileAllocation.IsConfigured() {
		errs = append(errs, fmt.Sprintf("%s does not support regime_profile_allocation: the profile resolves before the check runs, so it would read the current regime", prefix))
	}
	return errs
}

func closedBarDecisionReplayErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		if !replayMirrorPaperActive(sc) {
			continue
		}
		srcID := strings.TrimSpace(sc.ReplaySourceID)
		if srcID == "" {
			if sc.ClosedBarDecisions {
				errs = append(errs, fmt.Sprintf("strategy[%s]: closed_bar_decisions on a replay mirror needs an explicit replay_source_id naming an enabled live source in this config; a twin in another config cannot prove it decides on closed bars", sc.ID))
			}
			continue
		}
		src, ok := byID[srcID]
		if !ok {
			continue
		}
		if src.ClosedBarDecisions != sc.ClosedBarDecisions {
			errs = append(errs, fmt.Sprintf("strategy[%s]: closed_bar_decisions=%t disagrees with its replay source %q (closed_bar_decisions=%t); the mirror and its source must decide on the same bars", sc.ID, sc.ClosedBarDecisions, srcID, src.ClosedBarDecisions))
		}
	}
	return errs
}

func closedBarDecisionsConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		errs = append(errs, closedBarDecisionStrategyErrors(sc, cfg)...)
	}
	errs = append(errs, closedBarDecisionReplayErrors(cfg)...)
	return errs
}

func closedBarDecisionRegimeTimeframe(sc StrategyConfig, rc *RegimeConfig) string {
	if rc == nil || !rc.Enabled {
		return ""
	}
	_, tf := strategyRegimeSymbolTimeframe(sc.Args, rc)
	return tf
}

func appendClosedBarDecisionArgs(args []string, sc StrategyConfig, rc *RegimeConfig) []string {
	if !sc.ClosedBarDecisions {
		return args
	}
	out := append(append([]string{}, args...), closedBarDecisionsFlag)
	if tf := closedBarDecisionRegimeTimeframe(sc, rc); tf != "" {
		out = append(out, decisionRegimeTimeframeFlag+"="+tf)
	}
	return out
}

func closedBarDecisionContractError(sc StrategyConfig, fields StrategyDecisionFields) string {
	switch {
	case sc.ClosedBarDecisions && (fields.ClosedBar == nil || !fields.ClosedBar.Enabled):
		return "closed-bar decision contract mismatch: sent closed_bar_decisions=true but the check returned no closed_bar_decision block; holding this signal because the check may have decided on a forming bar (redeploy with scripts/update.sh so Go and Python match)"
	case !sc.ClosedBarDecisions && fields.ClosedBar != nil:
		return "closed-bar decision contract mismatch: closed_bar_decisions is off but the check returned a closed_bar_decision block (redeploy with scripts/update.sh so Go and Python match)"
	}
	return ""
}

func closedBarDecisionNeedsRegime(sc StrategyConfig, rc *RegimeConfig) bool {
	if rc == nil || !rc.Enabled {
		return false
	}
	return len(sc.AllowedRegimes) > 0 || hurstGateConfigured(sc)
}

type closedBarGateView struct {
	Regime     RegimePayload
	Held       bool
	HoldReason string
}

func closedBarGateViewFor(sc StrategyConfig, fields StrategyDecisionFields, storeRegime RegimePayload, rc *RegimeConfig) closedBarGateView {
	if !sc.ClosedBarDecisions {
		return closedBarGateView{Regime: storeRegime}
	}
	if fields.ClosedBar == nil {
		return closedBarGateView{Held: true, HoldReason: "the check returned no closed-bar decision"}
	}
	if fields.ClosedBar.Held {
		return closedBarGateView{Held: true, HoldReason: "closed-bar decision held: " + fields.ClosedBar.HoldReason}
	}
	if fields.DecisionRegime != nil && !fields.DecisionRegime.IsEmpty() {
		return closedBarGateView{Regime: *fields.DecisionRegime}
	}
	if closedBarDecisionNeedsRegime(sc, rc) {
		return closedBarGateView{HoldReason: "the check returned no closed-bar decision regime, so entry gates cannot read the decision bar"}
	}
	return closedBarGateView{}
}

func closedBarHurstGate(sc StrategyConfig, view closedBarGateView, rc *RegimeConfig, stratState *StrategyState, mu *sync.RWMutex, posQty float64) HurstGateDecision {
	if !sc.ClosedBarDecisions || (!view.Held && view.HoldReason == "") {
		return advanceHurstGate(sc, view.Regime, rc, stratState, mu, posQty)
	}
	var prior HurstGateState
	if stratState != nil {
		if mu != nil {
			mu.RLock()
		}
		prior = stratState.HurstGate
		if mu != nil {
			mu.RUnlock()
		}
	}
	return evaluateHurstGate(sc, RegimePayload{}, rc, prior, posQty)
}

func logClosedBarDecision(sc StrategyConfig, fields StrategyDecisionFields, logger *StrategyLogger) {
	if !sc.ClosedBarDecisions || fields.ClosedBar == nil || logger == nil {
		return
	}
	cb := fields.ClosedBar
	if cb.Held {
		if logger.Changed("closed-bar-hold", cb.HoldReason) {
			logger.Warn("Closed-bar decision held: %s — candle-derived opens and closes wait for verified closed history; protection continues (#1712)", cb.HoldReason)
		}
		return
	}
	if logger.Changed("closed-bar-hold", "") {
		logger.Info("Closed-bar decisions active: signal, entry ATR and sizing use the bar closed at %s (#1712)", formatClosedBarMs(cb.DecisionBoundaryMs))
	}
	logger.Debug("Closed-bar decision: bar opened %s closed %s (cutoff %s, %d forming row(s) dropped, %s)",
		formatClosedBarMs(cb.BarOpenMs), formatClosedBarMs(cb.DecisionBoundaryMs), formatClosedBarMs(cb.CutoffMs), cb.FormingRowsDropped, cb.ClosureRule)
}

func closedBarStampEntryATR(sc StrategyConfig, s *StrategyState, symbol string, opened bool, indicators map[string]interface{}) {
	if sc.ClosedBarDecisions && !opened {
		return
	}
	stampEntryATRIfOpened(s, symbol, indicators)
}

func formatClosedBarMs(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
