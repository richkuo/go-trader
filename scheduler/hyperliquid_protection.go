package main

import (
	"fmt"
	"sort"
	"strings"
)

type hlProtectionPlan struct {
	Symbol          string
	Side            string
	Size            float64
	AvgCost         float64
	EntryATR        float64
	StopLossATRMult float64
	TP1Mult         float64
	TP1Fraction     float64
	TP2Mult         float64
	StopLossOID     int64
	TP1OID          int64
	TP2OID          int64
}

func buildHyperliquidProtectionPlan(sc StrategyConfig, pos *Position) (hlProtectionPlan, bool) {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" || pos == nil {
		return hlProtectionPlan{}, false
	}
	if pos.Symbol == "" || pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		return hlProtectionPlan{}, false
	}
	if pos.Side != "long" && pos.Side != "short" {
		return hlProtectionPlan{}, false
	}
	slMult := 0.0
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		slMult = *sc.StopLossATRMult
	}
	tp1Mult, tp1Fraction, tp2Mult := hyperliquidProtectionTiers(sc)
	if slMult <= 0 && (tp1Mult <= 0 || tp1Fraction <= 0 || tp2Mult <= 0) {
		return hlProtectionPlan{}, false
	}
	return hlProtectionPlan{
		Symbol:          pos.Symbol,
		Side:            pos.Side,
		Size:            pos.Quantity,
		AvgCost:         pos.AvgCost,
		EntryATR:        pos.EntryATR,
		StopLossATRMult: slMult,
		TP1Mult:         tp1Mult,
		TP1Fraction:     tp1Fraction,
		TP2Mult:         tp2Mult,
		StopLossOID:     pos.StopLossOID,
		TP1OID:          pos.TP1OID,
		TP2OID:          pos.TP2OID,
	}, true
}

// hyperliquidProtectionTiers returns (tp1AtrMultiple, tp1Fraction, tp2AtrMultiple).
//
// Note: tier 2's close_fraction is INTENTIONALLY ignored — the on-chain TP2
// limit is sized as `size * (1 - tp1Fraction)` so that TP1 + TP2 always equal
// the strategy's full virtual quantity. The validation `tp2.Fraction <=
// tp1.Fraction` exists only to enforce a sane ordering of the operator's
// configured tiers; it does not flow into sizing. This mirrors the
// `tiered_tp_atr` close-evaluator's "everything remaining" semantics for the
// last tier and avoids leaving a residual sliver of position open after TP2.
//
// If you want a non-1.0 final tier (e.g. 50%/30%/20% across three tiers),
// extend hlProtectionPlan to carry a slice of tiers and rework the Python
// run_sync_protection placement loop accordingly. The current implementation
// is fixed at exactly two tiers (#604 review optional 3).
func hyperliquidProtectionTiers(sc StrategyConfig) (float64, float64, float64) {
	if !strategyUsesTieredTPATRClose(sc) {
		return 0, 0, 0
	}
	tiers := parseHLProtectionTiers(sc.Params["tiers"])
	if len(tiers) == 0 {
		tiers = []hlProtectionTier{
			{Multiple: 1, Fraction: 0.5},
			{Multiple: 2, Fraction: 1},
		}
	}
	if len(tiers) < 2 {
		return 0, 0, 0
	}
	tp1 := tiers[0]
	tp2 := tiers[1]
	if tp1.Multiple <= 0 || tp1.Fraction <= 0 || tp2.Multiple <= 0 || tp2.Fraction <= tp1.Fraction {
		return 0, 0, 0
	}
	return tp1.Multiple, tp1.Fraction, tp2.Multiple
}

type hlProtectionTier struct {
	Multiple float64
	Fraction float64
}

func parseHLProtectionTiers(raw interface{}) []hlProtectionTier {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	tiers := make([]hlProtectionTier, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			fmt.Printf("[WARN] hl-protection: tier[%d] is not an object, skipping (got %T)\n", idx, item)
			continue
		}
		multiple, mErr := floatFromAnyChecked(firstPresent(m, "atr_multiple", "multiple"))
		if mErr != nil {
			fmt.Printf("[WARN] hl-protection: tier[%d] atr_multiple/multiple invalid: %v — tier skipped\n", idx, mErr)
			continue
		}
		fraction, fErr := floatFromAnyChecked(firstPresent(m, "close_fraction", "fraction"))
		if fErr != nil {
			fmt.Printf("[WARN] hl-protection: tier[%d] close_fraction/fraction invalid: %v — tier skipped\n", idx, fErr)
			continue
		}
		if multiple <= 0 || fraction <= 0 {
			fmt.Printf("[WARN] hl-protection: tier[%d] non-positive multiple=%g fraction=%g — tier skipped\n", idx, multiple, fraction)
			continue
		}
		if fraction > 1 {
			fraction = 1
		}
		tiers = append(tiers, hlProtectionTier{Multiple: multiple, Fraction: fraction})
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Multiple < tiers[j].Multiple })
	return tiers
}

func firstPresent(m map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func floatFromAny(v interface{}) float64 {
	f, _ := floatFromAnyChecked(v)
	return f
}

// floatFromAnyChecked is the error-aware variant of floatFromAny. It accepts
// numeric JSON values plus encoding/json.Number-shaped types and returns an
// error for anything else (string, nil, bool, …) so the caller can surface
// a config-author mistake instead of silently coercing to 0 (#604 review #6).
func floatFromAnyChecked(v interface{}) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, fmt.Errorf("missing value")
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case jsonNumber:
		f, err := x.Float64()
		if err != nil {
			return 0, fmt.Errorf("jsonNumber: %w", err)
		}
		return f, nil
	case string:
		return 0, fmt.Errorf("string %q is not a number; quote-strip the value in config.json", x)
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

type jsonNumber interface {
	Float64() (float64, error)
}

func syncHyperliquidProtection(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidProtectionSyncResult, bool) {
	result, stderr, err := RunHyperliquidSyncProtection(
		sc.Script, plan.Symbol, plan.Side, plan.Size, plan.AvgCost, plan.EntryATR,
		plan.StopLossATRMult, plan.TP1Mult, plan.TP1Fraction, plan.TP2Mult,
		plan.StopLossOID, plan.TP1OID, plan.TP2OID,
	)
	if stderr != "" && logger != nil {
		logger.Info("protection sync stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("HL protection sync failed: %v", err)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, err.Error())
		return result, false
	}
	if result == nil {
		return nil, false
	}
	if result.Error != "" {
		if logger != nil {
			logger.Error("HL protection sync returned error: %s", result.Error)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, result.Error)
		return result, false
	}
	var warnings []string
	if result.StopLossError != "" {
		warnings = append(warnings, "SL: "+result.StopLossError)
	}
	if result.TP1Error != "" {
		warnings = append(warnings, "TP1: "+result.TP1Error)
	}
	if result.TP2Error != "" {
		warnings = append(warnings, "TP2: "+result.TP2Error)
	}
	if len(warnings) > 0 {
		msg := fmt.Sprintf("%s %s protection partially failed: %v", sc.ID, plan.Symbol, warnings)
		if logger != nil {
			logger.Warn("%s", msg)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, msg)
	}
	return result, true
}

func applyHyperliquidProtectionSync(pos *Position, result *HyperliquidProtectionSyncResult) {
	if pos == nil || result == nil {
		return
	}
	// Clear stale OIDs first when the Python side detected the prior order
	// already filled on-chain. The reconciler will book the close in the
	// next pass; here we just stop pointing at a dead OID that would be
	// re-placed against stale virtual qty (#604 review #1).
	if result.StopLossFilledExternally {
		pos.StopLossOID = 0
	}
	if result.TP1FilledExternally {
		pos.TP1OID = 0
	}
	if result.TP2FilledExternally {
		pos.TP2OID = 0
	}
	if result.StopLossOID > 0 {
		pos.StopLossOID = result.StopLossOID
	}
	if result.StopLossTriggerPx > 0 {
		pos.StopLossTriggerPx = result.StopLossTriggerPx
	}
	if result.TP1OID > 0 {
		pos.TP1OID = result.TP1OID
	}
	if result.TP2OID > 0 {
		pos.TP2OID = result.TP2OID
	}
}

// hyperliquidPlacesOnChainTPs reports whether sc is configured to place
// per-strategy on-chain reduce-only TP orders for HL perps. When true the
// in-process tiered close evaluator MUST be suppressed — the on-chain limits
// are the source of truth for tiered exits, and running both produces a race
// where the limit fills on-chain (shrinking position) and then the close
// evaluator emits another close_fraction sized off the stale virtual qty
// (#604 review #2).
func hyperliquidPlacesOnChainTPs(sc StrategyConfig) bool {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		return false
	}
	tp1Mult, tp1Fraction, tp2Mult := hyperliquidProtectionTiers(sc)
	return tp1Mult > 0 && tp1Fraction > 0 && tp2Mult > 0
}

// closeStrategiesSuppressedByOnChainProtection is the set of close evaluator
// names that are functionally replaced by on-chain reduce-only TP orders.
// Adding a new ATR-tiered close evaluator? It probably belongs here too.
var closeStrategiesSuppressedByOnChainProtection = map[string]struct{}{
	"tiered_tp_atr":      {},
	"tiered_tp_atr_live": {},
}

// filterCloseStrategiesForHLOnChainProtection returns the close-strategy list
// with names that overlap on-chain reduce-only TPs removed. Other close
// strategies (tp_at_pct, tiered_tp_pct, …) pass through unchanged so an
// operator can layer a percent-based stop alongside the ATR-tiered TP limits.
func filterCloseStrategiesForHLOnChainProtection(sc StrategyConfig) []string {
	if !hyperliquidPlacesOnChainTPs(sc) {
		return sc.CloseStrategies
	}
	if len(sc.CloseStrategies) == 0 {
		return sc.CloseStrategies
	}
	out := make([]string, 0, len(sc.CloseStrategies))
	for _, name := range sc.CloseStrategies {
		trimmed := strings.TrimSpace(name)
		if _, suppress := closeStrategiesSuppressedByOnChainProtection[trimmed]; suppress {
			continue
		}
		out = append(out, name)
	}
	return out
}

// strategyConfigWithOnChainProtectionFilter returns a shallow copy of sc with
// CloseStrategies filtered to drop on-chain-overlapping evaluators. Used at the
// runHyperliquidCheck call site so the Python check script doesn't see the
// suppressed names. Other fields share storage with the original — callers
// must not mutate slice/map fields on the returned copy.
func strategyConfigWithOnChainProtectionFilter(sc StrategyConfig) StrategyConfig {
	filtered := filterCloseStrategiesForHLOnChainProtection(sc)
	if len(filtered) == len(sc.CloseStrategies) {
		return sc
	}
	clone := sc
	clone.CloseStrategies = filtered
	return clone
}

func notifyHLProtectionFailure(notifier *MultiNotifier, sc StrategyConfig, symbol, reason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**HL PROTECTION WARNING** [%s] %s reduce-only SL/TP sync failed: %s", sc.ID, symbol, reason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}
