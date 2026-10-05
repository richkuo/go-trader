package main

import (
	"fmt"
	"sort"
	"strings"
)

const edgeStatusNoEdge = "no_edge"

const allowNoEdgeFlag = "--allow-no-edge"

type noEdgeEvidence struct {
	Source string
	Ref    string
}

const (
	noEdgeRefFeeAudit = "docs/research/fee-audit-m5.md"
	noEdgeRefLimbo    = "docs/research/1282-m5-limbo-verdicts.md"
)

var noEdgeStrategies = map[string]noEdgeEvidence{
	"adx_trend":                   {"fee_audit_m5", noEdgeRefFeeAudit},
	"amd_ifvg":                    {"study_fail", "docs/research/amd_ifvg_1023.md"},
	"analog_retrieval":            {"study_fail", "docs/research/1138-analog-retrieval-m1.md"},
	"atr_breakout":                {"fee_audit_m5", noEdgeRefFeeAudit},
	"awesome_oscillator":          {"study_fail", "backtest/candidates/awesome_oscillator_1660/REPORT.md"},
	"bollinger_bands":             {"fee_audit_m5", noEdgeRefFeeAudit},
	"chaikin_money_flow_breakout": {"study_fail", "backtest/candidates/chaikin_money_flow_1649/REPORT.md"},
	"commodity_channel_trend":     {"study_fail", "backtest/candidates/commodity_channel_trend_1656/REPORT.md"},
	"connors_rsi_reversion":       {"study_fail", "backtest/candidates/connors_rsi_reversion_1645/REPORT.md"},
	"consolidation_range":         {"fee_audit_m5", noEdgeRefLimbo},
	"donchian_breakout":           {"study_fail", "docs/research/985-donchian-regime-gate-m1.md"},
	"ema_crossover":               {"fee_audit_m5", noEdgeRefFeeAudit},
	"funding_skew":                {"fee_audit_m5", noEdgeRefLimbo},
	"heikin_ashi_ema":             {"fee_audit_m5", noEdgeRefFeeAudit},
	"ichimoku_cloud":              {"fee_audit_m5", noEdgeRefFeeAudit},
	"macd":                        {"fee_audit_m5", noEdgeRefFeeAudit},
	"mean_reversion":              {"fee_audit_m5", noEdgeRefFeeAudit},
	"momentum":                    {"fee_audit_m5", noEdgeRefFeeAudit},
	"mtf_confluence":              {"fee_audit_m5", noEdgeRefFeeAudit},
	"open_interest_breakout":      {"study_inconclusive", "backtest/candidates/open_interest_breakout_1637/REPORT.md"},
	"order_blocks":                {"fee_audit_m5", noEdgeRefFeeAudit},
	"pairs_spread":                {"fee_audit_m5", noEdgeRefFeeAudit},
	"parabolic_sar":               {"fee_audit_m5", noEdgeRefFeeAudit},
	"range_scalper":               {"study_fail", "docs/research/range_scalper_987.md"},
	"regime_adaptive":             {"fee_audit_m5", noEdgeRefLimbo},
	"relative_vigor_index":        {"study_fail", "backtest/candidates/relative_vigor_index_1666/REPORT.md"},
	"rsi":                         {"fee_audit_m5", noEdgeRefFeeAudit},
	"rsi_macd_combo":              {"fee_audit_m5", noEdgeRefFeeAudit},
	"session_breakout":            {"study_fail", "docs/research/1031-session-breakout-short-m1.md"},
	"sma_crossover":               {"fee_audit_m5", noEdgeRefFeeAudit},
	"squeeze_momentum":            {"fee_audit_m5", noEdgeRefFeeAudit},
	"stoch_rsi":                   {"fee_audit_m5", noEdgeRefFeeAudit},
	"supertrend":                  {"fee_audit_m5", noEdgeRefFeeAudit},
	"sweep_squeeze_combo":         {"fee_audit_m5", noEdgeRefFeeAudit},
	"tema_cross":                  {"fee_audit_m5", noEdgeRefLimbo},
	"tema_cross_bd":               {"fee_audit_m5", noEdgeRefLimbo},
	"triple_ema":                  {"fee_audit_m5", noEdgeRefFeeAudit},
	"triple_ema_bidir":            {"fee_audit_m5", noEdgeRefLimbo},
	"vol_momentum":                {"study_fail", "docs/research/vol_momentum_1021.md"},
	"volume_weighted":             {"fee_audit_m5", noEdgeRefFeeAudit},
	"vortex_trend":                {"study_fail", "backtest/candidates/vortex_trend_1647/REPORT.md"},
	"vwap_reversion":              {"fee_audit_m5", noEdgeRefFeeAudit},
}

func noEdgeEvidenceFor(name string) (noEdgeEvidence, bool) {
	ev, ok := noEdgeStrategies[strings.TrimSpace(name)]
	return ev, ok
}

const (
	edgeRefRoleOpen  = "open"
	edgeRefRoleClose = "close"
)

type edgeReference struct {
	Role string
	Name string
}

func (r edgeReference) String() string {
	return r.Role + "=" + r.Name
}

func isNativeCloseStrategy(name string) bool {
	_, ok := closeStrategyOwnedKeys[strings.TrimSpace(name)]
	return ok
}

func canonicalCloseStrategyName(name string) string {
	name = strings.TrimSpace(name)
	if name == "tp_at_pct" {
		return "tiered_tp_pct"
	}
	return name
}

func effectiveStrategyReferences(sc StrategyConfig) []edgeReference {
	if sc.Type == "options" {
		return nil
	}
	var refs []edgeReference
	if open := effectiveOpenStrategy(sc); open != "" {
		refs = append(refs, edgeReference{Role: edgeRefRoleOpen, Name: open})
	}
	for _, ref := range sc.closeRefs() {
		name := canonicalCloseStrategyName(ref.Name)
		if name == "" || isNativeCloseStrategy(name) {
			continue
		}
		refs = append(refs, edgeReference{Role: edgeRefRoleClose, Name: name})
	}
	return refs
}

func noEdgeReferences(sc StrategyConfig) []edgeReference {
	var out []edgeReference
	for _, ref := range effectiveStrategyReferences(sc) {
		if _, ok := noEdgeEvidenceFor(ref.Name); ok {
			out = append(out, ref)
		}
	}
	return out
}

const (
	edgeGateModePaper   = "paper"
	edgeGateModeLive    = "live"
	edgeGateModeMissing = "missing"
	edgeGateModeInvalid = "invalid"
)

type edgeGateMode struct {
	Kind   string
	Detail string
}

func rawEdgeGateMode(args []string) edgeGateMode {
	var values []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--mode":
			if i+1 >= len(args) {
				return edgeGateMode{Kind: edgeGateModeInvalid, Detail: "dangling --mode with no value"}
			}
			values = append(values, args[i+1])
			i++
		case strings.HasPrefix(arg, "--mode="):
			values = append(values, strings.TrimPrefix(arg, "--mode="))
		}
	}
	switch {
	case len(values) == 0:
		return edgeGateMode{Kind: edgeGateModeMissing}
	case len(values) > 1:
		return edgeGateMode{Kind: edgeGateModeInvalid, Detail: fmt.Sprintf("--mode given %d times (%q)", len(values), values)}
	case values[0] == edgeGateModePaper:
		return edgeGateMode{Kind: edgeGateModePaper}
	case values[0] == edgeGateModeLive:
		return edgeGateMode{Kind: edgeGateModeLive}
	}
	return edgeGateMode{Kind: edgeGateModeInvalid, Detail: fmt.Sprintf("--mode value %q is not exactly \"paper\" or \"live\"", values[0])}
}

func usesGoSpotPaperDispatch(sc StrategyConfig) bool {
	return sc.Type == "spot" && sc.Platform != "okx" && sc.Platform != "robinhood"
}

func spotPaperDispatchArgs(sc StrategyConfig, args []string) []string {
	if !usesGoSpotPaperDispatch(sc) || rawEdgeGateMode(sc.Args).Kind != edgeGateModeMissing {
		return args
	}
	return append(args, "--mode=paper")
}

func edgeGateModeForStrategy(sc StrategyConfig) edgeGateMode {
	return rawEdgeGateMode(spotPaperDispatchArgs(sc, append([]string{}, sc.Args...)))
}

func (sc *StrategyConfig) AllowNoEdgeAcknowledged() bool {
	return sc != nil && sc.AllowNoEdge != nil && *sc.AllowNoEdge
}

func appendAllowNoEdgeArg(args []string, sc StrategyConfig) []string {
	if !sc.AllowNoEdgeAcknowledged() {
		return args
	}
	return append(args, allowNoEdgeFlag)
}

func noEdgeAdmissionErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		refs := noEdgeReferences(sc)
		if len(refs) == 0 {
			continue
		}
		mode := edgeGateModeForStrategy(sc)
		for _, ref := range refs {
			ev, _ := noEdgeEvidenceFor(ref.Name)
			label := fmt.Sprintf("strategy %s: %s strategy %q is edge_status=no_edge (source %s, evidence %s)",
				sc.ID, ref.Role, ref.Name, ev.Source, ev.Ref)
			switch mode.Kind {
			case edgeGateModeInvalid:
				errs = append(errs, fmt.Sprintf("%s: invalid mode in args refused (%s); use exactly one --mode=paper or --mode=live", label, mode.Detail))
			case edgeGateModePaper:
			default:
				if sc.AllowNoEdgeAcknowledged() {
					continue
				}
				how := "explicit live mode"
				if mode.Kind == edgeGateModeMissing {
					how = "missing --mode (treated as live for edge admission)"
				}
				errs = append(errs, fmt.Sprintf("%s: %s requires \"allow_no_edge\": true on the strategy, or set an explicit \"--mode=paper\" in args", label, how))
			}
		}
	}
	return errs
}

type noEdgeWarning struct {
	StrategyID string
	Ref        edgeReference
	Evidence   noEdgeEvidence
}

func (w noEdgeWarning) identity() string {
	return strings.Join([]string{w.StrategyID, w.Ref.Role, w.Ref.Name, w.Evidence.Source, w.Evidence.Ref}, "|")
}

func (w noEdgeWarning) message(acknowledged bool) string {
	ack := "without an acknowledgement"
	if acknowledged {
		ack = "acknowledged with allow_no_edge: true"
	}
	return fmt.Sprintf("WARNING: live strategy %s uses %s strategy %s, which has edge_status=no_edge (source %s; evidence %s): "+
		"no approved edge evidence is established for it. It runs live %s.",
		w.StrategyID, w.Ref.Role, w.Ref.Name, w.Evidence.Source, w.Evidence.Ref, ack)
}

func liveNoEdgeWarnings(sc StrategyConfig) []noEdgeWarning {
	if !isLiveArgs(sc.Args) {
		return nil
	}
	var out []noEdgeWarning
	for _, ref := range noEdgeReferences(sc) {
		ev, _ := noEdgeEvidenceFor(ref.Name)
		out = append(out, noEdgeWarning{StrategyID: sc.ID, Ref: ref, Evidence: ev})
	}
	return out
}

func noEdgeStartupWarnings(strategies []StrategyConfig) []string {
	var lines []string
	for _, sc := range strategies {
		for _, w := range liveNoEdgeWarnings(sc) {
			lines = append(lines, w.message(sc.AllowNoEdgeAcknowledged()))
		}
	}
	sort.Strings(lines)
	return lines
}

func newlyIntroducedNoEdgeWarnings(oldStrategies, newStrategies []StrategyConfig) []string {
	prev := make(map[string]struct{})
	for _, sc := range oldStrategies {
		for _, w := range liveNoEdgeWarnings(sc) {
			prev[w.identity()] = struct{}{}
		}
	}
	var lines []string
	for _, sc := range newStrategies {
		for _, w := range liveNoEdgeWarnings(sc) {
			if _, seen := prev[w.identity()]; seen {
				continue
			}
			lines = append(lines, w.message(sc.AllowNoEdgeAcknowledged()))
		}
	}
	sort.Strings(lines)
	return lines
}

func edgeStatusSummaryTag(sc StrategyConfig) string {
	refs := noEdgeReferences(sc)
	if len(refs) == 0 {
		return ""
	}
	suffix := ""
	switch {
	case !isLiveArgs(sc.Args):
		suffix = "(paper)"
	case sc.AllowNoEdgeAcknowledged():
		suffix = "(ack)"
	}
	sources := make(map[string]struct{})
	for _, ref := range refs {
		ev, _ := noEdgeEvidenceFor(ref.Name)
		sources[ev.Source] = struct{}{}
	}
	if len(refs) == 1 || len(sources) == 1 {
		ev, _ := noEdgeEvidenceFor(refs[0].Name)
		tag := "edge=no_edge:" + ev.Source + suffix
		if len(refs) == 1 && refs[0].Role == edgeRefRoleOpen {
			return tag
		}
		names := make([]string, 0, len(refs))
		for _, ref := range refs {
			names = append(names, ref.String())
		}
		return tag + "[" + strings.Join(names, ",") + "]"
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		ev, _ := noEdgeEvidenceFor(ref.Name)
		parts = append(parts, fmt.Sprintf("%s:%s", ref.String(), ev.Source))
	}
	return "edge=no_edge" + suffix + "[" + strings.Join(parts, ",") + "]"
}
