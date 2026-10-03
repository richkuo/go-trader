package main

import (
	"fmt"
	"sort"
	"strings"
)

var m5DeprecatedEdgeStrategies = map[string]struct{}{
	"adx_trend":           {},
	"amd_ifvg":            {},
	"atr_breakout":        {},
	"bollinger_bands":     {},
	"consolidation_range": {},
	"ema_crossover":       {},
	"funding_skew":        {},
	"heikin_ashi_ema":     {},
	"ichimoku_cloud":      {},
	"macd":                {},
	"mean_reversion":      {},
	"momentum":            {},
	"mtf_confluence":      {},
	"order_blocks":        {},
	"pairs_spread":        {},
	"parabolic_sar":       {},
	"range_scalper":       {},
	"regime_adaptive":     {},
	"rsi":                 {},
	"rsi_macd_combo":      {},
	"sma_crossover":       {},
	"squeeze_momentum":    {},
	"stoch_rsi":           {},
	"supertrend":          {},
	"sweep_squeeze_combo": {},
	"tema_cross":          {},
	"tema_cross_bd":       {},
	"triple_ema":          {},
	"triple_ema_bidir":    {},
	"vol_momentum":        {},
	"volume_weighted":     {},
	"vwap_reversion":      {},
}

func strategyOpenNameForEdgeStatus(sc StrategyConfig) string {
	if sc.Type == "options" {
		return ""
	}
	if sc.OpenStrategy.Name != "" {
		return sc.OpenStrategy.Name
	}
	if len(sc.Args) > 0 {
		return sc.Args[0]
	}
	return ""
}

func strategyEdgeDeprecated(sc StrategyConfig) bool {
	name := strategyOpenNameForEdgeStatus(sc)
	if name == "" {
		return false
	}
	_, deprecated := m5DeprecatedEdgeStrategies[name]
	return deprecated
}

func deprecatedEdgeStartupWarnings(strategies []StrategyConfig) []string {
	var lines []string
	for _, sc := range strategies {
		if !strategyEdgeDeprecated(sc) || sc.AllowDeprecatedEffective() {
			continue
		}
		lines = append(lines, fmt.Sprintf(
			"WARNING: strategy %s trades open=%s, which the M5 fee audit deprecated "+
				"(gross edge <= 0; docs/research/fee-audit-m5.md). It is documented to lose "+
				"money before fees — set \"allow_deprecated\": true on the strategy to "+
				"acknowledge and silence this warning, or switch its open strategy.",
			sc.ID, strategyOpenNameForEdgeStatus(sc)))
	}
	sort.Strings(lines)
	return lines
}

func newlyDeprecatedEdgeWarnings(oldStrategies, newStrategies []StrategyConfig) []string {
	prevWarned := make(map[string]string, len(oldStrategies))
	for _, sc := range oldStrategies {
		if strategyEdgeDeprecated(sc) && !sc.AllowDeprecatedEffective() {
			prevWarned[sc.ID] = strategyOpenNameForEdgeStatus(sc)
		}
	}
	var fresh []StrategyConfig
	for _, sc := range newStrategies {
		if !strategyEdgeDeprecated(sc) || sc.AllowDeprecatedEffective() {
			continue
		}
		if prevWarned[sc.ID] == strategyOpenNameForEdgeStatus(sc) {
			continue
		}
		fresh = append(fresh, sc)
	}
	return deprecatedEdgeStartupWarnings(fresh)
}

func edgeStatusSummaryTag(sc StrategyConfig) string {
	if !strategyEdgeDeprecated(sc) {
		return ""
	}
	var b strings.Builder
	b.WriteString("edge=deprecated_m5")
	switch {
	case sc.AllowDeprecatedAcknowledged():
		b.WriteString("(ack)")
	case !isLiveArgs(sc.Args) && sc.AllowDeprecatedEffective():
		b.WriteString("(paper)")
	}
	return b.String()
}
