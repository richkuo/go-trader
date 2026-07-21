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
// acknowledged for the warning surface (#1275/#1402). Emitted once at
// startup — logged and DM'd — so an operator live-trading a strategy the
// project's own research says loses money before fees gets a loud, explicit
// signal. Paper strategies auto-suppress unless allow_deprecated is
// explicitly false (AllowDeprecatedEffective).
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

// newlyDeprecatedEdgeWarnings returns the deprecated-edge warning lines that
// apply after a SIGHUP hot reload but did not apply before it (#1275/#1402). A
// strategy warns "newly" when its post-reload shape is deprecated-and-unacked
// (AllowDeprecatedEffective false) while its pre-reload shape (matched by ID)
// either warned for a different open name, was acked/paper-suppressed, was
// clean, or did not exist. This keeps the reload path loud for the live
// transitions that can introduce the risk — open_strategy switched onto an
// M5 name, or allow_deprecated flipped off — without re-spamming unchanged
// deprecated strategies on every SIGHUP and without warning when a strategy
// switches AWAY from a deprecated name. Paper auto-suppression uses the same
// AllowDeprecatedEffective predicate as startup.
func newlyDeprecatedEdgeWarnings(oldStrategies, newStrategies []StrategyConfig) []string {
	prevWarned := make(map[string]string, len(oldStrategies)) // ID -> warned open name
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
			continue // already warned for this exact shape before the reload
		}
		fresh = append(fresh, sc)
	}
	return deprecatedEdgeStartupWarnings(fresh)
}

// edgeStatusSummaryTag returns the startup-summary token for a strategy whose
// open leg is M5-deprecated: "edge=deprecated_m5", with "(ack)" appended when
// the operator explicitly set allow_deprecated:true, or "(paper)" when a
// paper strategy is auto-suppressed via the unset default (#1402). Empty for
// clean strategies. The tag is never hidden by acknowledgment or paper
// suppression — like cb=off, a documented-negative-edge strategy should
// always be visible in the [config] audit line (also consumed by inspect).
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
