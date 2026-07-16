package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
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
	// #1085: load the directional-certification artifact so inspect reports the
	// real evidence-gate status (not the empty default store). Fail-closed.
	setDirectionalCertStore(LoadDirectionalCertSetFailClosed(directionalCertPath(), func(f string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	}))
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

	inspectState := loadInspectState(cfg)

	if *jsonOut {
		out := make([]map[string]interface{}, 0, len(targets))
		for _, sc := range targets {
			out = append(out, buildStrategyInspectionJSON(sc, explicit[sc.ID], cfg, inspectState))
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
		fmt.Print(formatStrategyInspection(sc, explicit[sc.ID], cfg, inspectState))
	}
	return 0
}

// loadInspectState best-effort loads persisted state so inspect can show
// position-aware effective direction (#783). Missing or unreadable DB is fine.
func loadInspectState(cfg *Config) *AppState {
	if cfg == nil || cfg.DBFile == "" {
		return nil
	}
	if _, err := os.Stat(cfg.DBFile); err != nil {
		return nil
	}
	sdb, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[inspect] state DB unavailable: %v\n", err)
		return nil
	}
	defer sdb.Close()
	state, err := sdb.LoadState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[inspect] state DB unavailable: %v\n", err)
		return nil
	}
	if state == nil {
		return NewAppState()
	}
	return state
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
		// #842: a legacy close_strategies array is read as the single
		// close_strategy; treat either spelling as an explicit close so
		// provenance display doesn't mark a configured close as "default".
		if keys["close_strategies"] {
			keys["close_strategy"] = true
		}
		out[id] = keys
	}
	return out, nil
}

// stopLossResolution summarizes which mutually-exclusive HL stop field owns
// the effective trigger, the resolved price % (or "deferred" for ATR-based
// stops), and whether the owner was set explicitly. Mirrors
// EffectiveStopLossPct so display can never lie about which field is winning
// on a hot-reload boundary.
type stopLossResolution struct {
	Source   string  // field name, e.g. "stop_loss_atr_mult"; "max_drawdown_pct"; "none"
	Value    string  // human-readable value ("1.5× ATR (deferred)", "2.0% from entry", "—")
	Explicit bool    // operator set the winning field explicitly
	PriceTag float64 // computed price % when known (post-arming for ATR stops); 0 for deferred
	Detail   []string
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
	if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		return stopLossResolution{
			Source:   "stop_loss_atr_regime",
			Value:    fmt.Sprintf("regime-aware fixed ATR (deferred until EntryATR + %s stamped)", regimeClassifierKey),
			Explicit: explicit["stop_loss_atr_regime"],
			Detail:   formatRegimeATRInspectDetail("stop_loss_atr_regime", *sc.StopLossATRRegime, explicit["stop_loss_atr_regime"]),
		}
	}
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
		return stopLossResolution{
			Source:   "trailing_stop_atr_regime",
			Value:    fmt.Sprintf("regime-aware trailing ATR (deferred until EntryATR + %s stamped)", regimeClassifierKey),
			Explicit: explicit["trailing_stop_atr_regime"],
			Detail:   formatRegimeATRInspectDetail("trailing_stop_atr_regime", *sc.TrailingStopATRRegime, explicit["trailing_stop_atr_regime"]),
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
// resolved tier shape. Returns ok=false when no tiered_tp_atr* close evaluator
// is wired.
type tpResolution struct {
	OK          bool
	CloseIndex  int
	CloseName   string
	RegimeTP    bool // tiered_tp_atr_regime or tiered_tp_atr_live_regime
	Tiers       []hlProtectionTier
	TiersFrom   string // "explicit on close ref" | "default (from registry)"
	DetailLines []string
	TierCount   int
}

func resolveTP(sc StrategyConfig, explicit map[string]bool) tpResolution {
	res := tpResolution{}
	for i, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTieredTPATRCloseName(n) {
			continue
		}
		res.OK = true
		res.CloseIndex = i
		res.CloseName = ref.Name
		_, hasTiers := closeTierListParam(ref.Params)
		useDefaults := false
		if ud, ok := ref.Params["use_defaults"].(bool); ok && ud {
			useDefaults = true
		}
		switch n {
		case "tiered_tp_atr_regime", "tiered_tp_atr_live_regime":
			res.RegimeTP = true
			res.DetailLines = formatInspectRegimeTPDetailLines(ref.Name, ref, hasTiers, useDefaults, explicit["close_strategy"])
			if tiers := strategyTPTiersForRegime(sc, canonicalTrendRegimeLabels[0]); len(tiers) > 0 {
				res.Tiers = tiers
			}
			res.TierCount = inferRegimeTPTierCount(ref, hasTiers, useDefaults)
			switch {
			case hasTiers:
				res.TiersFrom = "explicit tier list on close ref"
			case useDefaults:
				res.TiersFrom = "fleet baseline (use_defaults)"
			default:
				res.TiersFrom = "incomplete (needs tiers or use_defaults:true)"
			}
		default:
			res.Tiers = strategyTPTiers(sc)
			res.TierCount = len(res.Tiers)
			if hasTiers {
				res.TiersFrom = "explicit on close ref"
			} else {
				res.TiersFrom = "default (canonical [1.5×@40%, 3×@80%, 5×@100%])"
			}
		}
		return res
	}
	return res
}

func inferRegimeTPTierCount(ref StrategyRef, hasTiers, useDefaults bool) int {
	if hasTiers {
		if raw, ok := closeTierListParam(ref.Params); ok {
			if items, ok := raw.([]interface{}); ok {
				return len(items)
			}
		}
	}
	if useDefaults {
		return len(InspectRegimeTPFleetDefaultBlocks())
	}
	return 0
}

func formatRegimeATRInspectDetail(field string, block RegimeATRBlock, operatorExplicit bool) []string {
	tag := explicitTag(operatorExplicit)
	if block.UseDefaults {
		return []string{fmt.Sprintf(
			"%s: %s (from use_defaults baseline, classifier=%s)%s",
			field, summarizeRegimeATRBlockATRs(block.TrendRegime), regimeClassifierKey, tag,
		)}
	}
	labels := sortedTrendRegimeLabels(block.TrendRegime)
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		e := block.TrendRegime[label]
		out = append(out, fmt.Sprintf(
			"%s: %g×ATR (from %s.%s.%s.atr)%s",
			field, e.ATR, field, regimeClassifierKey, label, tag,
		))
	}
	return out
}

func summarizeRegimeATRBlockATRs(m map[string]RegimeATREntry) string {
	labels := sortedTrendRegimeLabels(m)
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%g×", label, m[label].ATR))
	}
	return strings.Join(parts, ", ")
}

func sortedTrendRegimeLabels(m map[string]RegimeATREntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatInspectRegimeTPDetailLines(closeName string, ref StrategyRef, hasTiers, useDefaults, closeExplicit bool) []string {
	tag := explicitTag(closeExplicit)
	if !hasTiers {
		if !useDefaults {
			return []string{fmt.Sprintf(
				"%s: incomplete regime TP config (needs tiers or use_defaults:true)%s",
				closeName, tag,
			)}
		}
		blocks := InspectRegimeTPFleetDefaultBlocks()
		specs := make([]regimeTierSpec, len(blocks))
		for i, b := range blocks {
			specs[i] = regimeTierSpec{Block: b}
		}
		return summarizeInspectRegimeTPSpecs(closeName, specs, tag)
	}
	// LoadConfig + validateRegimeATRConfig already rejected malformed tier JSON;
	// re-parse here is defense-in-depth for inspect-only paths, not a hot path.
	// Infer the vocabulary from the config so composite tier lists render too.
	tiersRaw, _ := closeTierListParam(ref.Params)
	specs, errs := parseRegimeTPTiers(tiersRaw, closeName+".params", regimeLabelsFromTierRaw(tiersRaw))
	if len(errs) > 0 || len(specs) == 0 {
		return []string{fmt.Sprintf("%s: regime tiers: parse error — fix config (%v)", closeName, errs)}
	}
	return summarizeInspectRegimeTPSpecs(closeName, specs, tag)
}

func summarizeInspectRegimeTPSpecs(closeName string, specs []regimeTierSpec, closeExplicitTag string) []string {
	out := make([]string, 0, len(specs))
	for idx, spec := range specs {
		prov := inspectRegimeTPTierProvenance(spec.Block, closeExplicitTag)
		out = append(out, fmt.Sprintf("%s tier[%d]: %s%s", closeName, idx, summarizeRegimeTierSpecInspect(spec), prov))
	}
	return out
}

func summarizeRegimeTierSpecInspect(spec regimeTierSpec) string {
	labels := sortedTrendRegimeLabels(spec.Block.TrendRegime)
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		e, ok := spec.Block.TrendRegime[label]
		if !ok {
			continue
		}
		frac := 0.0
		switch {
		case spec.HasTierCloseFraction:
			frac = spec.TierCloseFraction
		case e.HasCloseFrac:
			frac = e.CloseFraction
		}
		if frac <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%g×@%g%%", label, e.ATR, frac*100))
	}
	return strings.Join(parts, ", ")
}

func inspectRegimeTPTierProvenance(block RegimeATRBlock, closeExplicitTag string) string {
	if block.UseDefaults {
		return fmt.Sprintf(" (from use_defaults baseline, classifier=%s)%s", regimeClassifierKey, closeExplicitTag)
	}
	return fmt.Sprintf(" (explicit %s per label)%s", regimeClassifierKey, closeExplicitTag)
}

// formatStrategyInspection renders the multi-line human-facing inspect output
// for one strategy. Splitting from runInspect lets tests assert the formatter
// independently of os.Args / file IO, and lets the startup logger reuse the
// one-line summary helper below.
func formatStrategyInspection(sc StrategyConfig, explicit map[string]bool, cfg *Config, state *AppState) string {
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
	fmt.Fprintf(&b, "  close_strategy:      %s%s\n", formatCloseStrategyList(sc.closeRefs()), markIfDefault(explicit, "close_strategy"))
	for i, ref := range sc.closeRefs() {
		if len(ref.Params) == 0 {
			continue
		}
		fmt.Fprintf(&b, "    [%d] %s params: %s\n", i, ref.Name, stableParamSummary(ref.Params))
	}

	if sc.Type == "perps" || sc.Type == "manual" {
		appendDirectionInspectLines(&b, sc, explicit, cfg, state)
		fmt.Fprintf(&b, "  leverage:            %g%s\n", EffectiveExchangeLeverage(sc), markIfDefault(explicit, "leverage"))
		fmt.Fprintf(&b, "  sizing_leverage:     %g%s\n", EffectiveSizingLeverage(sc), sizingLeverageProvenance(sc, explicit))
		if m := EffectiveMarginPerTradeUSD(sc); m > 0 {
			fmt.Fprintf(&b, "  margin_per_trade:    $%.2f%s\n", m, markIfDefault(explicit, "margin_per_trade_usd"))
		}
		if sc.MarginMode != "" {
			fmt.Fprintf(&b, "  margin_mode:         %s%s\n", sc.MarginMode, markIfDefault(explicit, "margin_mode"))
		}
	}

	// #1159: correlated hedge leg. Surface config + live status so a hedge
	// position is never mistaken for an unmanaged one.
	if strategyHedgeEnabled(sc) {
		h := sc.Hedge
		fmt.Fprintf(&b, "  hedge:\n")
		fmt.Fprintf(&b, "    symbol:            %s (%s)\n", h.Symbol, h.Side)
		fmt.Fprintf(&b, "    ratio:             %g\n", h.Ratio)
		fmt.Fprintf(&b, "    margin_mode:       %s\n", h.MarginMode)
		fmt.Fprintf(&b, "    leverage:          %g\n", h.Leverage)
		hedgeCoin := hedgeCoinForStrategy(sc)
		if state != nil {
			if ss := state.Strategies[sc.ID]; ss != nil {
				if pos := ss.Positions[hedgeCoin]; pos != nil && pos.Quantity > 0 {
					fmt.Fprintf(&b, "    live:              %s %.6f @ avg $%.4f\n", pos.Side, pos.Quantity, pos.AvgCost)
				} else {
					fmt.Fprintf(&b, "    live:              flat\n")
				}
			}
		}
	}

	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		fmt.Fprintf(&b, "  stop_loss:\n")
		fmt.Fprintf(&b, "    source:            %s%s\n", sl.Source, explicitTag(sl.Explicit))
		fmt.Fprintf(&b, "    value:             %s\n", sl.Value)
		for _, line := range sl.Detail {
			fmt.Fprintf(&b, "%s%s\n", inspectHLDetailIndent, line)
		}

		tp := resolveTP(sc, explicit)
		fmt.Fprintf(&b, "  take_profit:\n")
		if !tp.OK {
			fmt.Fprintf(&b, "    source:            none (no tiered_tp_atr* close_strategy)\n")
		} else {
			fmt.Fprintf(&b, "    source:            close_strategy %s\n", tp.CloseName)
			for _, line := range tp.DetailLines {
				fmt.Fprintf(&b, "    %s\n", line)
			}
			if tp.RegimeTP && len(tp.Tiers) > 0 {
				fmt.Fprintf(&b, "    tiers (example: %s=%s): %s — %s\n", regimeClassifierKey, canonicalTrendRegimeLabels[0], formatTiers(tp.Tiers), tp.TiersFrom)
			} else {
				fmt.Fprintf(&b, "    tiers:             %s — %s\n", formatTiers(tp.Tiers), tp.TiersFrom)
			}
		}
	}

	fmt.Fprintf(&b, "  max_drawdown_pct:    %g%s\n", sc.MaxDrawdownPct, markIfDefault(explicit, "max_drawdown_pct"))
	// #1048: circuit-breaker state (skip manual — exempt from CheckRisk). Surface
	// only when explicitly disabled so the unprotected case is visible; the
	// default-on state stays uncluttered. #1273: when enabled with non-default
	// timing/threshold overrides, surface those too — an operator must see a
	// tuned breaker at a glance.
	if sc.Type != "manual" {
		if !sc.CircuitBreakerEnabled() {
			fmt.Fprintf(&b, "  circuit_breaker:     off (explicit) — drawdown + consecutive-loss halt disabled\n")
		} else if ov := circuitBreakerOverrideSummary(sc); ov != "" {
			fmt.Fprintf(&b, "  circuit_breaker:     on — %s\n", ov)
		}
	}
	// #1150: pause state. Surface only when paused so the normal case stays
	// uncluttered.
	if sc.Paused {
		fmt.Fprintf(&b, "  paused:              true — position-increasing signals held; closes and SL/TP management still run\n")
	}
	if sc.IntervalSeconds > 0 {
		fmt.Fprintf(&b, "  interval_seconds:    %d\n", sc.IntervalSeconds)
	} else if cfg != nil {
		fmt.Fprintf(&b, "  interval_seconds:    %d (inherited from global)\n", cfg.IntervalSeconds)
	}
	if len(sc.AllowedRegimes) > 0 {
		fmt.Fprintf(&b, "  allowed_regimes:     %v\n", sc.AllowedRegimes)
	}
	if cfg != nil && cfg.Regime != nil && len(cfg.Regime.Windows) > 0 {
		fmt.Fprintf(&b, "  regime_windows:      %s\n", formatRegimeWindowsInspectMap(cfg.Regime.Windows, cfg.Regime))
		fmt.Fprintf(&b, "  regime_gate_window:  %s\n", formatRegimeWindowSelectorInspect(sc, "gate", cfg.Regime))
		fmt.Fprintf(&b, "  regime_atr_window:   %s\n", formatRegimeWindowSelectorInspect(sc, "atr", cfg.Regime))
		fmt.Fprintf(&b, "  regime_directional_window: %s\n", formatRegimeWindowSelectorInspect(sc, "directional", cfg.Regime))
	}
	if sc.HTFFilter {
		fmt.Fprintf(&b, "  htf_filter:          true\n")
	}
	// #1277: only shown when non-default — simple is the frozen baseline.
	if sc.Type != "options" {
		if m := resolveATRMethod(sc, cfg); m != ATRMethodSimple {
			src := "inherited from global"
			if normalizeATRMethod(sc.ATRMethod) != "" {
				src = "per-strategy"
			}
			fmt.Fprintf(&b, "  atr_method:          %s (%s)\n", m, src)
		}
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
func formatStrategySummaryLine(sc StrategyConfig, explicit map[string]bool, cfg *Config) string {
	parts := []string{fmt.Sprintf("type=%s", sc.Type)}
	if sc.OpenStrategy.Name != "" {
		parts = append(parts, fmt.Sprintf("open=%s", sc.OpenStrategy.Name))
	}
	if sc.CloseStrategy != nil {
		parts = append(parts, fmt.Sprintf("close=%s", sc.CloseStrategy.Name))
	} else {
		parts = append(parts, "close=open-as-close")
	}
	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		parts = append(parts, fmt.Sprintf("sl=%s%s", sl.Source, explicitTag(sl.Explicit)))
		tp := resolveTP(sc, explicit)
		if tp.OK {
			n := tp.TierCount
			if n <= 0 {
				n = len(tp.Tiers)
			}
			parts = append(parts, fmt.Sprintf("tp=%s[%d-tier]", tp.CloseName, n))
		} else {
			parts = append(parts, "tp=none")
		}
	}
	// #1048: surface an explicitly disabled circuit breaker so a strategy
	// trading live without the auto-protective drawdown/loss-streak halt is not
	// silently unprotected. Manual is exempt from CheckRisk, so the flag is a
	// no-op there and not shown. #1273: non-default timing/threshold overrides
	// on an enabled breaker are surfaced the same way.
	if sc.Type != "manual" {
		if !sc.CircuitBreakerEnabled() {
			parts = append(parts, "cb=off")
		} else if ov := circuitBreakerOverrideSummary(sc); ov != "" {
			parts = append(parts, "cb["+ov+"]")
		}
	}
	// #1150: surface a paused strategy in the startup summary line.
	if sc.Paused {
		parts = append(parts, "paused")
	}
	// #1277: surface a non-default ATR smoothing method — wilder re-derives
	// every ATR-based stop/TP distance, so the audit line must show it
	// (resolved, so a global wilder default tags every inheriting strategy).
	if sc.Type != "options" {
		if m := resolveATRMethod(sc, cfg); m != ATRMethodSimple {
			parts = append(parts, "atr="+m)
		}
	}
	// #1275: surface an M5-deprecated open strategy (documented gross edge
	// <= 0) so the negative-edge evidence is visible in the audit line even
	// when the operator acknowledged it via allow_deprecated.
	if tag := edgeStatusSummaryTag(sc); tag != "" {
		parts = append(parts, tag)
	}
	return fmt.Sprintf("[config] %s: %s", sc.ID, strings.Join(parts, " "))
}

// circuitBreakerOverrideSummary renders the non-default #1273 circuit-breaker
// timing/threshold overrides as a compact comma-joined list (e.g.
// "losses>=3, loss_cooldown=30m, dd_cooldown=12h0m"). Empty when all three
// fields are nil (pure defaults) so the common case stays uncluttered in both
// the startup summary line and the inspect text view.
func circuitBreakerOverrideSummary(sc StrategyConfig) string {
	var parts []string
	if sc.CBLossStreakThreshold != nil {
		parts = append(parts, fmt.Sprintf("losses>=%d", sc.CircuitBreakerLossStreakThreshold()))
	}
	if sc.CBLossStreakCooldownMinutes != nil {
		parts = append(parts, "loss_cooldown="+formatCBDuration(sc.CircuitBreakerLossStreakCooldown()))
	}
	if sc.CBDrawdownCooldownMinutes != nil {
		parts = append(parts, "dd_cooldown="+formatCBDuration(sc.CircuitBreakerDrawdownCooldown()))
	}
	return strings.Join(parts, ", ")
}

// buildStrategyInspectionJSON mirrors formatStrategyInspection in
// machine-readable form. Keeps the same provenance info so external tools
// (dashboards, audit scripts) can spot "field is at the default" cases.
func buildStrategyInspectionJSON(sc StrategyConfig, explicit map[string]bool, cfg *Config, state *AppState) map[string]interface{} {
	if explicit == nil {
		explicit = map[string]bool{}
	}
	var closeRefJSON interface{} // null when open-as-close (#842: single close)
	if sc.CloseStrategy != nil {
		closeRefJSON = map[string]interface{}{
			"name":   sc.CloseStrategy.Name,
			"params": sc.CloseStrategy.Params,
		}
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
		"close_strategy":            closeRefJSON,
		"close_strategy_explicit":   explicit["close_strategy"],
		"max_drawdown_pct":          sc.MaxDrawdownPct,
		"max_drawdown_pct_explicit": explicit["max_drawdown_pct"],
	}
	// #1048: circuit-breaker enable state. Manual is exempt from CheckRisk, so
	// the flag is meaningless there and omitted. #1273: effective timing/
	// threshold parameters with the usual explicit-provenance flags so external
	// tools can spot "field is at the default".
	if sc.Type != "manual" {
		out["circuit_breaker_enabled"] = sc.CircuitBreakerEnabled()
		out["circuit_breaker_explicit"] = explicit["circuit_breaker"]
		out["cb_drawdown_cooldown_minutes"] = int(sc.CircuitBreakerDrawdownCooldown() / time.Minute)
		out["cb_drawdown_cooldown_minutes_explicit"] = explicit["cb_drawdown_cooldown_minutes"]
		out["cb_loss_streak_threshold"] = sc.CircuitBreakerLossStreakThreshold()
		out["cb_loss_streak_threshold_explicit"] = explicit["cb_loss_streak_threshold"]
		out["cb_loss_streak_cooldown_minutes"] = int(sc.CircuitBreakerLossStreakCooldown() / time.Minute)
		out["cb_loss_streak_cooldown_minutes_explicit"] = explicit["cb_loss_streak_cooldown_minutes"]
	}
	// #1150: pause state, always emitted so dashboards can key off it.
	out["paused"] = sc.Paused
	if cfg != nil && cfg.Regime != nil && len(cfg.Regime.Windows) > 0 {
		out["regime_windows"] = cfg.Regime.Windows
		out["regime_gate_window"] = regimeWindowSelectorJSON(sc, "gate", cfg.Regime)
		out["regime_atr_window"] = regimeWindowSelectorJSON(sc, "atr", cfg.Regime)
		out["regime_directional_window"] = regimeWindowSelectorJSON(sc, "directional", cfg.Regime)
	}
	if sc.Type == "perps" || sc.Type == "manual" {
		for k, v := range directionInspectJSON(sc, cfg, state) {
			out[k] = v
		}
		out["leverage"] = EffectiveExchangeLeverage(sc)
		out["sizing_leverage"] = EffectiveSizingLeverage(sc)
		out["margin_mode"] = sc.MarginMode
	}
	if sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual") {
		sl := resolveStopLoss(sc, explicit)
		slMap := map[string]interface{}{
			"source":   sl.Source,
			"value":    sl.Value,
			"explicit": sl.Explicit,
		}
		if len(sl.Detail) > 0 {
			slMap["detail"] = sl.Detail
		}
		out["stop_loss"] = slMap
		tp := resolveTP(sc, explicit)
		tpMap := map[string]interface{}{"configured": tp.OK}
		if tp.OK {
			tiers := make([]map[string]interface{}, len(tp.Tiers))
			for i, t := range tp.Tiers {
				tiers[i] = map[string]interface{}{"atr_multiple": t.Multiple, "close_fraction": t.Fraction}
			}
			tpMap["close_index"] = tp.CloseIndex
			tpMap["close_name"] = tp.CloseName
			tpMap["tp_tiers"] = tiers
			tpMap["tiers_source"] = tp.TiersFrom
			tpMap["tier_count"] = tp.TierCount
			if len(tp.DetailLines) > 0 {
				tpMap["detail"] = tp.DetailLines
			}
			if tp.RegimeTP && len(tp.Tiers) > 0 {
				tpMap["example_classifier_label"] = canonicalTrendRegimeLabels[0]
			}
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
	// #1159: correlated hedge leg — config + live status.
	if strategyHedgeEnabled(sc) {
		h := sc.Hedge
		hedgeCoin := hedgeCoinForStrategy(sc)
		hedgeMap := map[string]interface{}{
			"symbol":      h.Symbol,
			"side":        h.Side,
			"ratio":       h.Ratio,
			"margin_mode": h.MarginMode,
			"leverage":    h.Leverage,
			"live":        "flat",
		}
		if state != nil {
			if ss := state.Strategies[sc.ID]; ss != nil {
				if pos := ss.Positions[hedgeCoin]; pos != nil && pos.Quantity > 0 {
					hedgeMap["live"] = map[string]interface{}{
						"side":     pos.Side,
						"quantity": pos.Quantity,
						"avg_cost": pos.AvgCost,
					}
				}
			}
		}
		out["hedge"] = hedgeMap
	}
	return out
}

// --- formatting helpers ---

// inspectHLDetailIndent aligns stop_loss continuation lines with the payload
// column of "    source:" / "    value:" (23 runes). See PR #750 review.
const inspectHLDetailIndent = "                       "

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

func appendDirectionInspectLines(b *strings.Builder, sc StrategyConfig, explicit map[string]bool, cfg *Config, state *AppState) {
	baseDir := EffectiveDirection(sc)
	prov := directionProvenance(sc, explicit)
	policyConfigured := sc.RegimeDirectionalPolicy != nil && sc.RegimeDirectionalPolicy.IsConfigured()
	if policyConfigured {
		// Policy present: distinguish static base from per-regime overrides.
		fmt.Fprintf(b, "  base_direction:      %s (%s)\n", baseDir, prov)
	} else {
		// No policy: keep legacy "direction:" label for operator scripts (#784).
		fmt.Fprintf(b, "  direction:           %s (%s)\n", baseDir, prov)
	}
	if policyConfigured {
		fmt.Fprintf(b, "  regime_directional_policy:\n")
		// #1085: surface the evidence gate. The per-label rows below are the
		// CONFIGURED mapping; it is only honored when the cell is certified.
		certStatus, certCell := directionalCertInspectStatus(sc, cfg)
		fmt.Fprintf(b, "    certification:     %s %s (#1085)\n", certStatus, certCell)
		for _, label := range canonicalTrendRegimeLabels {
			dir := EffectiveDirectionForRegime(sc, label)
			inv := false
			if entry, ok := sc.RegimeDirectionalPolicy.Resolve(label); ok {
				inv = entry.InvertSignal
			}
			fmt.Fprintf(b, "    %s: direction=%s invert_signal=%v (configured)\n", label, dir, inv)
		}
	}
	var stratState *StrategyState
	if state != nil {
		stratState = state.Strategies[sc.ID]
	}
	if stratState != nil {
		syms := make([]string, 0, len(stratState.Positions))
		for sym, pos := range stratState.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := stratState.Positions[sym]
			currentDirRegime := stratState.Regime
			if cfg != nil && cfg.Regime != nil {
				currentDirRegime = regimeLabelFromWindows(stratState.RegimeWindows, sc.RegimeDirectionalWindow, cfg.Regime)
				if currentDirRegime == "" {
					currentDirRegime = stratState.Regime
				}
			}
			posDirRegime := positionDirectionalRegimeLabel(pos, sc)
			effRegime := effectiveRegimeForPolicy(currentDirRegime, posDirRegime, pos.Quantity)
			// #1085: gate by the open stamp so the reported effective direction is
			// what the runtime actually uses (base for uncertified/legacy).
			effDir := EffectiveDirectionForPositionGated(sc, currentDirRegime, posDirRegime, pos.Quantity, pos.DirectionCertifiedStatesAtOpen)
			regimeSrc := "stamped at open"
			if strings.TrimSpace(posDirRegime) == "" {
				regimeSrc = "current cycle (position regime unknown)"
			}
			certSrc := "uncertified at open → base"
			if pos.DirectionCertifiedAtOpen {
				certSrc = "certified at open → policy"
			}
			fmt.Fprintf(b, "  position %s:         side=%s effective_direction=%s (regime=%s, %s; %s)\n", sym, pos.Side, effDir, effRegime, regimeSrc, certSrc)
			if len(pos.RegimeWindows) > 0 {
				fmt.Fprintf(b, "    regime_windows:    %v\n", pos.RegimeWindows)
			}
		}
	}
}

func directionInspectJSON(sc StrategyConfig, cfg *Config, state *AppState) map[string]interface{} {
	out := map[string]interface{}{
		// "direction" is the legacy alias; prefer "base_direction" for new consumers.
		"direction":      EffectiveDirection(sc),
		"base_direction": EffectiveDirection(sc),
	}
	if sc.RegimeDirectionalPolicy != nil && sc.RegimeDirectionalPolicy.IsConfigured() {
		byRegime := make(map[string]interface{}, len(canonicalTrendRegimeLabels))
		for _, label := range canonicalTrendRegimeLabels {
			entry := map[string]interface{}{
				"direction": EffectiveDirectionForRegime(sc, label),
			}
			if e, ok := sc.RegimeDirectionalPolicy.Resolve(label); ok {
				entry["invert_signal"] = e.InvertSignal
			}
			byRegime[label] = entry
		}
		out["regime_directional_policy"] = byRegime
		// #1085: the evidence gate status for this strategy's cell.
		certStatus, certCell := directionalCertInspectStatus(sc, cfg)
		out["regime_directional_certification"] = map[string]interface{}{
			"status": certStatus,
			"cell":   certCell,
		}
	}
	var stratState *StrategyState
	if state != nil {
		stratState = state.Strategies[sc.ID]
	}
	if stratState != nil {
		currentDirRegime := strategyCurrentDirectionalRegime(stratState, sc)
		if cfg != nil && cfg.Regime != nil {
			if label := regimeLabelFromWindows(stratState.RegimeWindows, sc.RegimeDirectionalWindow, cfg.Regime); label != "" {
				currentDirRegime = label
			}
		}
		positions := make([]map[string]interface{}, 0)
		for sym, pos := range stratState.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			posDirRegime := positionDirectionalRegimeLabel(pos, sc)
			positions = append(positions, map[string]interface{}{
				"symbol":                      sym,
				"side":                        pos.Side,
				"quantity":                    pos.Quantity,
				"regime":                      pos.Regime,
				"regime_windows":              pos.RegimeWindows,
				"effective_direction":         EffectiveDirectionForPositionGated(sc, currentDirRegime, posDirRegime, pos.Quantity, pos.DirectionCertifiedStatesAtOpen),
				"effective_policy_regime":     effectiveRegimeForPolicy(currentDirRegime, posDirRegime, pos.Quantity),
				"direction_certified_at_open": pos.DirectionCertifiedAtOpen,
			})
		}
		if len(positions) > 0 {
			sort.Slice(positions, func(i, j int) bool {
				return positions[i]["symbol"].(string) < positions[j]["symbol"].(string)
			})
			out["open_positions"] = positions
		}
	}
	return out
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
