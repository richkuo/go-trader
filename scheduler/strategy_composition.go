package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// StrategyDecisionFields is the optional open/close decision metadata emitted
// by check scripts when a strategy opts into issue #480's split entry/exit
// model. The legacy signal field remains authoritative for execution.
type StrategyDecisionFields struct {
	OpenStrategy    string         `json:"open_strategy,omitempty"`
	CloseStrategies []string       `json:"close_strategies,omitempty"`
	OpenAction      string         `json:"open_action,omitempty"`
	CloseFraction   float64        `json:"close_fraction"`
	CloseStrategy   string         `json:"close_strategy,omitempty"`
	Regime          *RegimePayload `json:"regime,omitempty"`
	// PostTPTrailingATRMult is set only by the trailing_tp_ratchet close family
	// (#844): the tightened trailing ATR multiple for the highest cleared tier.
	// The runtime stamps it onto Position.PostTPTrailingATRMult (tighten-only)
	// and the trailing-stop walker takes over at that distance. nil otherwise.
	PostTPTrailingATRMult *float64 `json:"post_tp_trailing_atr_mult,omitempty"`
}

// PositionCtx is the optional state snapshot threaded into close evaluators
// when a strategy opts into the open/close composition model (#496).
// Regime carries the stamped label for the strategy's ATR window so
// regime-aware close evaluators (tiered_tp_atr_regime, #733) can resolve
// tier multipliers without re-running the classifier.
type PositionCtx struct {
	Side              string
	AvgCost           float64
	Quantity          float64
	InitialQuantity   float64
	EntryATR          float64
	Regime            string
	DirectionalRegime string
	RegimeWindows     map[string]string
}

func usesOpenCloseConfig(sc StrategyConfig) bool {
	return strings.TrimSpace(sc.OpenStrategy.Name) != "" || sc.CloseStrategy != nil
}

func strategyNameFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.TrimSpace(args[0])
}

func effectiveOpenStrategy(sc StrategyConfig) string {
	if name := strings.TrimSpace(sc.OpenStrategy.Name); name != "" {
		return name
	}
	return strategyNameFromArgs(sc.Args)
}

func validateStrategyConceptName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(trimmed, " \t\r\n,") {
		return fmt.Errorf("must be a single strategy id, got %q", name)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("must not start with '-'")
	}
	return nil
}

// appendOpenCloseArgs adds position-context flags. Strategy refs (open/close
// names + per-ref params) are sent separately via buildStrategyRefsArg (#640).
func appendOpenCloseArgs(args []string, sc StrategyConfig, pos PositionCtx) []string {
	if !usesOpenCloseConfig(sc) {
		return args
	}
	out := append([]string{}, args...)
	if side := strings.TrimSpace(pos.Side); side != "" {
		out = append(out, "--position-side", side)
	}
	out = appendPositionFloatArg(out, "--position-avg-cost", pos.AvgCost)
	out = appendPositionFloatArg(out, "--position-qty", pos.Quantity)
	out = appendPositionFloatArg(out, "--position-initial-qty", pos.InitialQuantity)
	out = appendPositionFloatArg(out, "--position-entry-atr", pos.EntryATR)
	if r := strings.TrimSpace(pos.Regime); r != "" {
		out = append(out, "--position-regime", r)
	}
	return out
}

// buildStrategyRefsArg emits the --strategy-refs JSON carrying the open ref
// and close refs (each name + params) to the Python check script (#640). Open
// name falls back to args[0] when sc.OpenStrategy.Name is empty so legacy
// configs that rely on the positional strategy arg keep working post-migration.
func buildStrategyRefsArg(sc StrategyConfig) ([]string, error) {
	openName := effectiveOpenStrategy(sc)
	if openName == "" && sc.CloseStrategy == nil {
		return nil, nil
	}
	payload := map[string]interface{}{}
	if openName != "" {
		payload["open"] = StrategyRef{Name: openName, Params: sc.OpenStrategy.Params}
	}
	// #842: a strategy has a single close. The Go↔Python wire still carries a
	// "closes" list (length ≤ 1) so the Python composition layer's generic
	// evaluator stays unchanged; max close_fraction over one element is the
	// element itself.
	if refs := sc.closeRefs(); len(refs) > 0 {
		payload["closes"] = refs
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []string{"--strategy-refs", string(blob)}, nil
}

func appendPositionFloatArg(args []string, flag string, value float64) []string {
	if value == 0 {
		return args
	}
	return append(args, flag+"="+strconv.FormatFloat(value, 'f', -1, 64))
}

func appendRegimeArgs(args []string, regime *RegimeConfig) []string {
	if regime == nil || !regime.Enabled {
		return args
	}
	out := append(args, "--regime-enabled")
	if blob := regimeWindowsSpecJSON(regime); blob != "" {
		out = append(out, "--regime-windows-spec-json", blob)
	}
	out = append(out, "--ohlcv-limit", strconv.Itoa(regimeRequiredOhlcvLimit(regime)))
	return out
}

func appendStrategyRegimeWindowArgs(args []string, sc StrategyConfig, regime *RegimeConfig) []string {
	if regime == nil || !regime.Enabled || !regimeMultiWindowEnabled(regime) {
		return args
	}
	out := append([]string{}, args...)
	if key := resolveStrategyRegimeWindow(sc, "atr", regime); key != "" && key != regimeWindowDefaultKey {
		out = append(out, "--regime-atr-window", key)
	}
	// Directional window is resolved Go-side from RegimePayload; not forwarded to Python.
	return out
}

func positionCtxForSymbol(s *StrategyState, symbol string, sc StrategyConfig, regime *RegimeConfig) PositionCtx {
	if s == nil || strings.TrimSpace(symbol) == "" {
		return PositionCtx{}
	}
	return positionCtxForCheck(sc, s.Positions[symbol], regime)
}

func positionCtxFromPosition(pos *Position) PositionCtx {
	if pos == nil {
		return PositionCtx{}
	}
	return PositionCtx{
		Side:              pos.Side,
		AvgCost:           pos.AvgCost,
		Quantity:          pos.Quantity,
		InitialQuantity:   pos.InitialQuantity,
		EntryATR:          pos.EntryATR,
		Regime:            pos.Regime,
		DirectionalRegime: pos.Regime,
		RegimeWindows:     cloneStringMap(pos.RegimeWindows),
	}
}

// formatStrategyRef renders a ref for human-readable change logs (#640). When
// no params are set, prints just the quoted name; otherwise appends a
// stable-key summary so reload diffs show param-only changes.
func formatStrategyRef(ref StrategyRef) string {
	if len(ref.Params) == 0 {
		return strconv.Quote(ref.Name)
	}
	return strconv.Quote(ref.Name) + formatParamsSummary(ref.Params)
}

func formatStrategyRefList(refs []StrategyRef) string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, formatStrategyRef(r))
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func formatParamsSummary(params map[string]interface{}) string {
	if len(params) == 0 {
		return ""
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
	return "{" + strings.Join(parts, ",") + "}"
}

func explicitCloseStrategies(sc StrategyConfig) []string {
	if sc.CloseStrategy == nil {
		return nil
	}
	if trimmed := strings.TrimSpace(sc.CloseStrategy.Name); trimmed != "" {
		return []string{trimmed}
	}
	return nil
}

func maxCloseFraction(fractions []float64) float64 {
	max := 0.0
	for _, f := range fractions {
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		if f > max {
			max = f
		}
	}
	return max
}

func composeOpenCloseSignal(openAction string, closeFraction float64, positionSide string) int {
	if closeFraction < 0 {
		closeFraction = 0
	}
	if closeFraction > 1 {
		closeFraction = 1
	}
	if closeFraction > 0 {
		switch positionSide {
		case "long":
			return -1
		case "short":
			return 1
		default:
			return 0
		}
	}
	if positionSide != "" {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(openAction)) {
	case "long":
		return 1
	case "short":
		return -1
	default:
		return 0
	}
}
