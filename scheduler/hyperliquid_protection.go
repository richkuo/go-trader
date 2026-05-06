package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type hlProtectionPlan struct {
	Symbol          string
	Side            string
	Size            float64
	AvgCost         float64
	EntryATR        float64
	StopLossATRMult float64
	StopLossOID     int64
	Tiers           []hlProtectionTier
	TPOIDs          []int64
}

func buildHyperliquidProtectionPlan(sc StrategyConfig, pos *Position) (hlProtectionPlan, bool) {
	if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" || pos == nil {
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
	tiers := hyperliquidProtectionTiers(sc)
	if slMult <= 0 && len(tiers) == 0 {
		return hlProtectionPlan{}, false
	}
	return hlProtectionPlan{
		Symbol:          pos.Symbol,
		Side:            pos.Side,
		Size:            pos.Quantity,
		AvgCost:         pos.AvgCost,
		EntryATR:        pos.EntryATR,
		StopLossATRMult: slMult,
		StopLossOID:     pos.StopLossOID,
		Tiers:           tiers,
		TPOIDs:          tpOIDsForTierCount(pos.TPOIDs, len(tiers)),
	}, true
}

// hyperliquidProtectionTiers returns the cumulative ATR take-profit tiers used
// to place per-strategy reduce-only limit orders. Fractions are cumulative,
// matching the tiered_tp_atr close evaluator: 0.5/0.8/1.0 becomes order sizes
// of 50%, 30%, and 20% of the current virtual quantity (#612). The final tier
// is coerced to 1.0 so older two-tier configs that ended below 100% keep the
// prior "everything remaining" TP2 behavior from #604.
func hyperliquidProtectionTiers(sc StrategyConfig) []hlProtectionTier {
	if !strategyUsesTieredTPATRClose(sc) {
		return nil
	}
	tiers := parseHLProtectionTiers(sc.Params["tiers"])
	if len(tiers) == 0 {
		tiers = []hlProtectionTier{
			{Multiple: 1, Fraction: 0.5},
			{Multiple: 2, Fraction: 1},
		}
	}
	if len(tiers) < 2 {
		return nil
	}
	prevFraction := 0.0
	for _, tier := range tiers {
		if tier.Multiple <= 0 || tier.Fraction <= prevFraction {
			return nil
		}
		prevFraction = tier.Fraction
	}
	tiers[len(tiers)-1].Fraction = 1
	return tiers
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
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].Multiple < tiers[j].Multiple })
	return tiers
}

func tpOIDsForTierCount(oids []int64, tierCount int) []int64 {
	if tierCount <= 0 {
		return nil
	}
	out := make([]int64, tierCount)
	copy(out, oids)
	return out
}

func cloneInt64s(vals []int64) []int64 {
	if len(vals) == 0 {
		return nil
	}
	out := make([]int64, len(vals))
	copy(out, vals)
	return out
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

// syncHyperliquidProtection is a package var so tests can stub the subprocess
// call without spawning Python. Production callers use runHyperliquidProtectionSync.
var syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidProtectionSyncResult, bool) {
	result, stderr, err := RunHyperliquidSyncProtection(
		sc.Script, plan.Symbol, plan.Side, plan.Size, plan.AvgCost, plan.EntryATR,
		plan.StopLossATRMult, plan.Tiers, plan.StopLossOID, plan.TPOIDs,
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
	for idx, errMsg := range result.TPErrors {
		if errMsg != "" {
			warnings = append(warnings, fmt.Sprintf("TP%d: %s", idx+1, errMsg))
		}
	}
	if len(result.TPErrors) == 0 {
		if result.TP1Error != "" {
			warnings = append(warnings, "TP1: "+result.TP1Error)
		}
		if result.TP2Error != "" {
			warnings = append(warnings, "TP2: "+result.TP2Error)
		}
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
	if result.StopLossFilledExternally {
		pos.StopLossOID = 0
	}
	if result.StopLossOID > 0 {
		pos.StopLossOID = result.StopLossOID
	}
	if result.StopLossTriggerPx > 0 {
		pos.StopLossTriggerPx = result.StopLossTriggerPx
	}
	if result.TPOIDs != nil {
		pos.TPOIDs = cloneInt64s(result.TPOIDs)
	} else if result.TP1OID > 0 || result.TP2OID > 0 {
		pos.TPOIDs = []int64{result.TP1OID, result.TP2OID}
	}
	// Clear stale TP OIDs after applying the latest echoed/placed OID list.
	// The reconciler will book externally-filled closes; here we only stop
	// pointing at dead OIDs that would otherwise be re-placed against stale
	// virtual quantity (#604 review #1). Python already zeros the echoed
	// tiered list; this keeps the Go state defensive for legacy or partial
	// script responses.
	if len(result.TPFilledExternally) > 0 {
		if len(pos.TPOIDs) < len(result.TPFilledExternally) {
			pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, len(result.TPFilledExternally))
		}
		for idx, filled := range result.TPFilledExternally {
			if filled {
				pos.TPOIDs[idx] = 0
			}
		}
	} else if result.TP1FilledExternally || result.TP2FilledExternally {
		if len(pos.TPOIDs) < 2 {
			pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, 2)
		}
		if result.TP1FilledExternally {
			pos.TPOIDs[0] = 0
		}
		if result.TP2FilledExternally {
			pos.TPOIDs[1] = 0
		}
	}
}

// runHyperliquidProtectionSync is the locking + plan + subprocess + apply
// pipeline shared by perps (open-no-trade and post-trade) and manual cycles.
//
//  1. RLock to build a plan from the current position; release.
//  2. If a plan is required, call syncHyperliquidProtection (subprocess, no lock).
//  3. Lock to re-validate position state and apply OID updates if the position
//     is still the same side and qty>0 — guards against external close racing
//     the subprocess.
//
// logTag is prepended to the success log line so callers can distinguish
// open-no-trade vs. post-trade vs. manual sync sites. Returns true when the
// apply step ran (false when no plan, subprocess failed, or the position
// vanished/flipped during the subprocess).
func runHyperliquidProtectionSync(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
	logTag string,
) bool {
	if stratState == nil || symbol == "" {
		return false
	}
	var plan hlProtectionPlan
	var syncOK bool
	mu.RLock()
	if pos, ok := stratState.Positions[symbol]; ok {
		plan, syncOK = buildHyperliquidProtectionPlan(sc, pos)
	}
	mu.RUnlock()
	if !syncOK {
		return false
	}
	protection, ok := syncHyperliquidProtection(sc, plan, notifier, logger)
	if !ok || protection == nil {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != plan.Side {
		return false
	}
	applyHyperliquidProtectionSync(pos, protection)
	if logger != nil {
		logger.Info("%s (sl_oid=%d tp_oids=%v)", logTag, pos.StopLossOID, pos.TPOIDs)
	}
	return true
}

// hyperliquidPlacesOnChainTPs reports whether sc is configured to place
// per-strategy on-chain reduce-only TP orders for HL perps/manual. When true the
// in-process tiered close evaluator MUST be suppressed — the on-chain limits
// are the source of truth for tiered exits, and running both produces a race
// where the limit fills on-chain (shrinking position) and then the close
// evaluator emits another close_fraction sized off the stale virtual qty
// (#604 review #2).
func hyperliquidPlacesOnChainTPs(sc StrategyConfig) bool {
	if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
		return false
	}
	return len(hyperliquidProtectionTiers(sc)) > 0
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
