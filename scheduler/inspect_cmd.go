package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// runInspect is the `go-trader inspect <strategy-id>` subcommand: load the
// config, find the strategy, and print its effective (post-migration,
// post-default) shape. Built for the incident workflow in #704 — operators
// were grep'ing for plausible-sounding field names (`take_profit_atr_mult`,
// `tp_tiers` per-strategy) that don't exist and concluding "no TP configured"
// from the wrong inspection path. The output names the actual fields, their
// resolved values, and whether each was set explicitly or filled by defaults.
func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	jsonOut := fs.Bool("json", false, "Emit the effective view as JSON (machine-readable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "inspect: missing <strategy-id>")
		fmt.Fprintln(os.Stderr, "usage: go-trader inspect [--config <path>] [--json] <strategy-id>|--all")
		return 2
	}
	target := rest[0]

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect: failed to load config %s: %v\n", *configPath, err)
		return 1
	}
	explicit, err := loadStrategyExplicitKeys(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect: failed to read raw config for explicit-key detection: %v\n", err)
		return 1
	}

	var targets []StrategyConfig
	if target == "--all" {
		targets = append(targets, cfg.Strategies...)
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	} else {
		for _, sc := range cfg.Strategies {
			if sc.ID == target {
				targets = append(targets, sc)
				break
			}
		}
		if len(targets) == 0 {
			fmt.Fprintf(os.Stderr, "inspect: strategy %q not found in %s\n", target, *configPath)
			return 1
		}
	}

	if *jsonOut {
		out := make([]map[string]interface{}, 0, len(targets))
		for _, sc := range targets {
			out = append(out, buildStrategyInspectionJSON(sc, explicit[sc.ID], cfg))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "inspect: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	for i, sc := range targets {
		if i > 0 {
			fmt.Println()
		}
		fmt.Print(formatStrategyInspection(sc, explicit[sc.ID], cfg))
	}
	return 0
}

// loadStrategyExplicitKeys re-reads the raw config bytes and records which
// JSON keys are explicitly present on each strategy entry. Used by inspect to
// distinguish "operator wrote this value" from "LoadConfig filled it in".
// Keyed by strategy id — entries without an id fall back to "strategy[<i>]".
func loadStrategyExplicitKeys(path string) (map[string]map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Strategies []map[string]json.RawMessage `json:"strategies"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(envelope.Strategies))
	for i, s := range envelope.Strategies {
		id := fmt.Sprintf("strategy[%d]", i)
		if raw, ok := s["id"]; ok {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				id = v
			}
		}
		keys := make(map[string]bool, len(s))
		for k := range s {
			keys[k] = true
		}
		out[id] = keys
	}
	return out, nil
}

// stopLossResolution summarizes which of the five mutually-exclusive HL stop
// fields owns the effective trigger, the resolved price % (or "deferred" for
// ATR-based stops), and whether the owner was set explicitly. Mirrors the
// resolution logic in EffectiveStopLossPct so display can never lie about
// which field is winning on a hot-reload boundary.
type stopLossResolution struct {
	Source   string  // field name, e.g. "stop_loss_atr_mult"; "max_drawdown_pct"; "none"
	Value    string  // human-readable value ("1.5× ATR (deferred)", "2.0% from entry", "—")
	Explicit bool    // operator set the winning field explicitly
	PriceTag float64 // computed price % when known (post-arming for ATR stops); 0 for deferred
}

func resolveStopLoss(sc StrategyConfig, explicit map[string]bool) stopLossResolution {
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		return stopLossResolution{Source: "n/a", Value: "—"}
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return stopLossResolution{
			Source:   "trailing_stop_atr_mult",
			Value:    fmt.Sprintf("%g× ATR (trailing, deferred until EntryATR stamped)", *sc.TrailingStopATRMult),
			Explicit: explicit["trailing_stop_atr_mult"],
		}
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return stopLossResolution{
			Source:   "stop_loss_atr_mult",
			Value:    fmt.Sprintf("%g× ATR (fixed, deferred until EntryATR stamped)", *sc.StopLossATRMult),
			Explicit: explicit["stop_loss_atr_mult"],
		}
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return stopLossResolution{
				Source:   "trailing_stop_pct",
				Value:    fmt.Sprintf("%g%% trailing", *sc.TrailingStopPct),
				Explicit: explicit["trailing_stop_pct"],
				PriceTag: *sc.TrailingStopPct,
			}
		}
		return stopLossResolution{Source: "trailing_stop_pct", Value: "disabled (explicit 0)", Explicit: true}
	}
	if sc.StopLossPct != nil {
		if *sc.StopLossPct > 0 {
			return stopLossResolution{
				Source:   "stop_loss_pct",
				Value:    fmt.Sprintf("%g%% from entry", *sc.StopLossPct),
				Explicit: explicit["stop_loss_pct"],
				PriceTag: *sc.StopLossPct,
			}
		}
		return stopLossResolution{Source: "stop_loss_pct", Value: "disabled (explicit 0)", Explicit: true}
	}
	if sc.StopLossMarginPct != nil {
		if *sc.StopLossMarginPct > 0 && sc.Leverage > 0 {
			derived := *sc.StopLossMarginPct / sc.Leverage
			return stopLossResolution{
				Source:   "stop_loss_margin_pct",
				Value:    fmt.Sprintf("%g%% margin → %.3g%% from entry (÷ leverage %g)", *sc.StopLossMarginPct, derived, sc.Leverage),
				Explicit: explicit["stop_loss_margin_pct"],
				PriceTag: derived,
			}
		}
		return stopLossResolution{Source: "stop_loss_margin_pct", Value: "disabled (explicit 0 or zero leverage)", Explicit: true}
	}
	// All five nil — only reachable when DefaultStopLossATRMult was explicitly
	// disabled (=0). Falls back to MaxDrawdownPct (capped). LoadConfig should
	// have filled stop_loss_atr_mult otherwise.
	if sc.MaxDrawdownPct > 0 {
		v := sc.MaxDrawdownPct
		if v > MaxAutoStopLossPct {
			v = MaxAutoStopLossPct
		}
		return stopLossResolution{
			Source:   "max_drawdown_pct (fallback)",
			Value:    fmt.Sprintf("%g%% from entry (capped at %g%%)", v, MaxAutoStopLossPct),
			Explicit: false,
			PriceTag: v,
		}
	}
	return stopLossResolution{Source: "none", Value: "no exchange-side stop"}
}

// tpResolution summarizes which close ref owns the take-profit logic and its
// resolved tier shape. Returns ok=false when no TP close evaluator is wired
// (i.e. close strategy isn't tiered_tp_atr or tiered_tp_atr_live).
type tpResolution struct {
	OK         bool
	CloseIndex int
	CloseName  string
	Tiers      []hlProtectionTier
	TiersFrom  string // "explicit on close ref" | "default (from registry)"
}

func resolveTP(sc StrategyConfig, explicit map[string]bool) tpResolution {
	res := tpResolution{}
	for i, ref := range sc.CloseStrategies {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n != "tiered_tp_atr" && n != "tiered_tp_atr_live" {
			continue
		}
		res.OK = true
		res.CloseIndex = i
		res.CloseName = ref.Name
		_, hasTiers := ref.Params["tiers"]
		res.Tiers = strategyTPTiers(sc)
		if hasTiers {
			res.TiersFrom = "explicit on close ref"
		} else {
			res.TiersFrom = "default (canonical [1×@50%, 2×@100%])"
		}
		return res
	}
	return res
}

// formatStrategyInspection renders the multi-line human-facing inspect output
// for one strategy. Splitting from runInspect lets tests assert the formatter
// independently of os.Args / file IO, and lets the startup logger reuse the
// one-line summary helper below.
func formatStrategyInspection(sc StrategyConfig, explicit map[string]bool, cfg *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "strategy %s\n", sc.ID)
	fmt.Fprintf(&b, "  type:                %s\n", sc.Type)
	fmt.Fprintf(&b, "  platform:            %s%s\n", sc.Platform, markIfDefault(explicit, "platform"))
	if sc.Symbol != "" {
		fmt.Fprintf(&b, "  symbol:              %s\n", sc.Symbol)
	}
	if sc.Timeframe != "" {
		fmt.Fprintf(&b, "  timeframe:           %s\n", sc.Timeframe)
	}
	fmt.Fprintf(&b, "  script:              %s%s\n", sc.Script, markIfDefault(explicit, "script"))
	if len(sc.Args) > 0 {
		fmt.Fprintf(&b, "  args:                %v\n", sc.Args)
	}

	fmt.Fprintf(&b, "  open_strategy:       %s%s\n", strategyRefDisplayName(sc.OpenStrategy), markIfDefault(explicit, "open_strategy"))
	if len(sc.OpenStrategy.Params) > 0 {
		fmt.Fprintf(&b, "    params:            %s\n", stableParamSummary(sc.OpenStrategy.Params))
	}
	fmt.Fprintf(&b, "  close_strategies:    %s%s\n", formatCloseStrategyList(sc.CloseStrategies), markIfDefault(explicit, "close_strategies"))
	for i, ref := range sc.CloseStrategies {
		if len(ref.Params) == 0 {
			continue
		}
		fmt.Fprintf(&b, "    [%d] %s params: %s\n", i, ref.Name, stableParamSummary(ref.Params))
	}

	if sc.Type == "perps" || sc.Type == "manual" {
		fmt.Fprintf(&b, "  direction:           %s (%s)\n", EffectiveDirection(sc), directionProvenance(sc, explicit))
		fmt.Fprintf(&b, "  leverage:            %g%s\n", EffectiveExchangeLeverage(sc), markIfDefault(explicit, "leverage"))
		fmt.Fprintf(&b, "  sizing_leverage:     %g%s\n", EffectiveSizingLeverage(sc), sizingLeverageProvenance(sc, explicit))
		if m := EffectiveMarginPerTradeUSD(sc); m > 0 {
			fmt.Fprintf(&b, "  margin_per_trade:    $%.2f%s\n", m, markIfDefault(explicit, "margin_per_trade_usd"))
		}
		if sc.MarginMode != "" {
			fmt.Fprintf(&b, "  margin_mode:         %s%s\n", sc.MarginMode, markIfDefault(explicit, "margin_mode"))
		}
	}

	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		fmt.Fprintf(&b, "  stop_loss:\n")
		fmt.Fprintf(&b, "    source:            %s%s\n", sl.Source, explicitTag(sl.Explicit))
		fmt.Fprintf(&b, "    value:             %s\n", sl.Value)

		tp := resolveTP(sc, explicit)
		fmt.Fprintf(&b, "  take_profit:\n")
		if !tp.OK {
			fmt.Fprintf(&b, "    source:            none (no tiered_tp_atr / tiered_tp_atr_live in close_strategies)\n")
		} else {
			fmt.Fprintf(&b, "    source:            close_strategies[%d] %s\n", tp.CloseIndex, tp.CloseName)
			fmt.Fprintf(&b, "    tiers:             %s — %s\n", formatTiers(tp.Tiers), tp.TiersFrom)
		}
	}

	fmt.Fprintf(&b, "  max_drawdown_pct:    %g%s\n", sc.MaxDrawdownPct, markIfDefault(explicit, "max_drawdown_pct"))
	if sc.IntervalSeconds > 0 {
		fmt.Fprintf(&b, "  interval_seconds:    %d\n", sc.IntervalSeconds)
	} else if cfg != nil {
		fmt.Fprintf(&b, "  interval_seconds:    %d (inherited from global)\n", cfg.IntervalSeconds)
	}
	if len(sc.AllowedRegimes) > 0 {
		fmt.Fprintf(&b, "  allowed_regimes:     %v\n", sc.AllowedRegimes)
	}
	if sc.HTFFilter {
		fmt.Fprintf(&b, "  htf_filter:          true\n")
	}
	if sc.ThetaHarvest != nil {
		fmt.Fprintf(&b, "  theta_harvest:       enabled=%v profit=%g%% stop=%g%% min_dte=%g%s\n",
			sc.ThetaHarvest.Enabled, sc.ThetaHarvest.ProfitTargetPct, sc.ThetaHarvest.StopLossPct, sc.ThetaHarvest.MinDTEClose,
			markIfDefault(explicit, "theta_harvest"))
	}
	return b.String()
}

// formatStrategySummaryLine compresses the effective resolution into one line
// for startup logging — meant to be the operator's "did my close/SL config
// actually land?" sanity check the moment the daemon boots (#704 suggestion 2).
func formatStrategySummaryLine(sc StrategyConfig, explicit map[string]bool) string {
	parts := []string{fmt.Sprintf("type=%s", sc.Type)}
	if sc.OpenStrategy.Name != "" {
		parts = append(parts, fmt.Sprintf("open=%s", sc.OpenStrategy.Name))
	}
	if len(sc.CloseStrategies) > 0 {
		names := make([]string, 0, len(sc.CloseStrategies))
		for _, ref := range sc.CloseStrategies {
			names = append(names, ref.Name)
		}
		parts = append(parts, fmt.Sprintf("close=[%s]", strings.Join(names, ",")))
	} else {
		parts = append(parts, "close=open-as-close")
	}
	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		parts = append(parts, fmt.Sprintf("sl=%s%s", sl.Source, explicitTag(sl.Explicit)))
		tp := resolveTP(sc, explicit)
		if tp.OK {
			parts = append(parts, fmt.Sprintf("tp=%s[%d-tier]", tp.CloseName, len(tp.Tiers)))
		} else {
			parts = append(parts, "tp=none")
		}
	}
	return fmt.Sprintf("[config] %s: %s", sc.ID, strings.Join(parts, " "))
}

// buildStrategyInspectionJSON mirrors formatStrategyInspection in
// machine-readable form. Keeps the same provenance info so external tools
// (dashboards, audit scripts) can spot "field is at the default" cases.
func buildStrategyInspectionJSON(sc StrategyConfig, explicit map[string]bool, cfg *Config) map[string]interface{} {
	if explicit == nil {
		explicit = map[string]bool{}
	}
	closeRefs := make([]map[string]interface{}, 0, len(sc.CloseStrategies))
	for _, ref := range sc.CloseStrategies {
		closeRefs = append(closeRefs, map[string]interface{}{
			"name":   ref.Name,
			"params": ref.Params,
		})
	}
	out := map[string]interface{}{
		"id":       sc.ID,
		"type":     sc.Type,
		"platform": sc.Platform,
		"open_strategy": map[string]interface{}{
			"name":     sc.OpenStrategy.Name,
			"params":   sc.OpenStrategy.Params,
			"explicit": explicit["open_strategy"],
		},
		"close_strategies":          closeRefs,
		"close_strategies_explicit": explicit["close_strategies"],
		"max_drawdown_pct":          sc.MaxDrawdownPct,
		"max_drawdown_pct_explicit": explicit["max_drawdown_pct"],
	}
	if sc.Type == "perps" || sc.Type == "manual" {
		out["direction"] = EffectiveDirection(sc)
		out["leverage"] = EffectiveExchangeLeverage(sc)
		out["sizing_leverage"] = EffectiveSizingLeverage(sc)
		out["margin_mode"] = sc.MarginMode
	}
	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		out["stop_loss"] = map[string]interface{}{
			"source":   sl.Source,
			"value":    sl.Value,
			"explicit": sl.Explicit,
		}
		tp := resolveTP(sc, explicit)
		tpMap := map[string]interface{}{"configured": tp.OK}
		if tp.OK {
			tiers := make([]map[string]interface{}, len(tp.Tiers))
			for i, t := range tp.Tiers {
				tiers[i] = map[string]interface{}{"atr_multiple": t.Multiple, "fraction": t.Fraction}
			}
			tpMap["close_index"] = tp.CloseIndex
			tpMap["close_name"] = tp.CloseName
			tpMap["tiers"] = tiers
			tpMap["tiers_source"] = tp.TiersFrom
		}
		out["take_profit"] = tpMap
	}
	if sc.IntervalSeconds > 0 {
		out["interval_seconds"] = sc.IntervalSeconds
		out["interval_seconds_explicit"] = true
	} else if cfg != nil {
		out["interval_seconds"] = cfg.IntervalSeconds
		out["interval_seconds_explicit"] = false
	}
	return out
}

// --- formatting helpers ---

func markIfDefault(explicit map[string]bool, key string) string {
	if explicit[key] {
		return ""
	}
	return " (default)"
}

func explicitTag(explicit bool) string {
	if explicit {
		return " (explicit)"
	}
	return " (default)"
}

func strategyRefDisplayName(ref StrategyRef) string {
	if ref.Name == "" {
		return "(unset)"
	}
	return ref.Name
}

func formatCloseStrategyList(refs []StrategyRef) string {
	if len(refs) == 0 {
		return "[] (falls back to open-as-close)"
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return "[" + strings.Join(names, ", ") + "]"
}

func formatTiers(tiers []hlProtectionTier) string {
	if len(tiers) == 0 {
		return "(none)"
	}
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		parts[i] = fmt.Sprintf("%g× ATR @ %g%%", t.Multiple, t.Fraction*100)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// stableParamSummary renders a params map with deterministic key ordering so
// inspect output is comparable across runs. Lifted instead of using fmt.Sprintf
// directly because Go map iteration is randomized (CLAUDE.md "Map iteration").
func stableParamSummary(params map[string]interface{}) string {
	if len(params) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// directionProvenance distinguishes the four ways EffectiveDirection can land
// on "long"/"short"/"both": explicit direction field, legacy allow_shorts
// bool, or the implicit "long" default. Surfacing this matters because the
// v14 migration silently rewrites allow_shorts → direction and operators
// won't see that on a stale checkout.
func directionProvenance(sc StrategyConfig, explicit map[string]bool) string {
	switch {
	case explicit["direction"]:
		return "explicit"
	case explicit["allow_shorts"]:
		return "legacy allow_shorts (pre-v14)"
	default:
		return "default long"
	}
}

func sizingLeverageProvenance(sc StrategyConfig, explicit map[string]bool) string {
	switch {
	case explicit["sizing_leverage"]:
		return ""
	case explicit["leverage"]:
		return " (inherited from leverage)"
	default:
		return " (default)"
	}
}
