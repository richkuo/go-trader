package main

import (
	"fmt"
	"sort"
	"strings"
)

// m5DeprecatedEdgeStrategies mirrors M5_DEPRECATED_EDGE_STRATEGIES in
// shared_strategies/open/registry.py (#1275): open strategies the M5 fee
// audit (#999, docs/research/fee-audit-m5.md) assigned the `deprecate`
// verdict — gross edge <= 0 on every measured leg, so no fee/selectivity
// tuning can salvage them. They stay registered and loadable (explicit
// configs keep trading, backtests keep running) but are hidden from
// discovery, and configuring one live surfaces a startup warning here.
// Keep the two rosters identical — TestM5DeprecatedRosterMatchesPythonRegistry
// enforces parity against the Python source.
var m5DeprecatedEdgeStrategies = map[string]struct{}{
	"adx_trend":           {},
	"amd_ifvg":            {},
	"atr_breakout":        {},
	"bollinger_bands":     {},
	"ema_crossover":       {},
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
	"rsi":                 {},
	"rsi_macd_combo":      {},
	"sma_crossover":       {},
	"squeeze_momentum":    {},
	"stoch_rsi":           {},
	"supertrend":          {},
	"sweep_squeeze_combo": {},
	"triple_ema":          {},
	"volume_weighted":     {},
	"vol_momentum":        {},
	"vwap_reversion":      {},
}

// strategyOpenNameForEdgeStatus resolves the open-strategy name the M5 roster
// keys on: the explicit open_strategy ref when set, else args[0] (the legacy
// positional strategy name every check dispatcher passes). Options strategies
// live in a separate registry whose names never carry M5 verdicts, and manual
// strategies auto-fill args[0]="hold" — both resolve to names outside the
// roster, so no type gating is needed beyond the options exclusion below.
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

// strategyEdgeDeprecated reports whether the strategy's open leg carries the
// M5 deprecate verdict (#1275). Advisory only — callers surface warnings,
// never gate loading or trading.
func strategyEdgeDeprecated(sc StrategyConfig) bool {
	name := strategyOpenNameForEdgeStatus(sc)
	if name == "" {
		return false
	}
	_, deprecated := m5DeprecatedEdgeStrategies[name]
	return deprecated
}

// deprecatedEdgeStartupWarnings returns one operator-facing warning line per
// configured strategy whose open leg is M5-deprecated and that has not been
// acknowledged via allow_deprecated (#1275). Emitted once at startup — logged
// and DM'd — so an operator live-trading a strategy the project's own
// research says loses money before fees gets a loud, explicit signal.
func deprecatedEdgeStartupWarnings(strategies []StrategyConfig) []string {
	var lines []string
	for _, sc := range strategies {
		if !strategyEdgeDeprecated(sc) || sc.AllowDeprecated {
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

// edgeStatusSummaryTag returns the startup-summary token for a strategy whose
// open leg is M5-deprecated: "edge=deprecated_m5", with "(ack)" appended when
// the operator acknowledged it via allow_deprecated. Empty for clean
// strategies. The tag is never hidden by the acknowledgment — like cb=off, a
// documented-negative-edge strategy trading live should always be visible in
// the [config] audit line.
func edgeStatusSummaryTag(sc StrategyConfig) string {
	if !strategyEdgeDeprecated(sc) {
		return ""
	}
	var b strings.Builder
	b.WriteString("edge=deprecated_m5")
	if sc.AllowDeprecated {
		b.WriteString("(ack)")
	}
	return b.String()
}
