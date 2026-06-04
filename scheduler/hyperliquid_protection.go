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
	// TPArmedTiers mirrors Position.TPArmedTiers padded to len(Tiers): tier i
	// was successfully placed at least once. When TPOIDs[i]==0 and
	// TPArmedTiers[i]==true, the tier is treated as consumed (filled) and must
	// not be re-placed from a zero OID — see #716 / #749.
	TPArmedTiers []bool
	// #843: cancel+replace resting protection when the applied ATR regime changes
	// and the new trigger price clears the min-move debounce.
	ForceSLReplace bool
	ForceTPReplace []bool
	// CancelTPOIDs lists resting reduce-only TP OIDs dropped when the active
	// regime's ladder has fewer tiers than pos.TPOIDs (#843 tier-count shrink).
	CancelTPOIDs []int64
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
	// SL ATR multiplier: legacy scalar `stop_loss_atr_mult` wins when present;
	// otherwise the regime-aware sibling resolves via pos.Regime. Validation
	// ensures only one is set (#733).
	atrRegime := protectionATRRegimeLabel(pos, sc)
	slMult := 0.0
	if v, ok := unifiedCloseStopLossATR(sc, atrRegime); ok {
		// #841 2b: unified close owns the per-regime SL.
		slMult = v
	} else if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		slMult = *sc.StopLossATRMult
	} else if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		if v, ok := resolveRegimeATR(*sc.StopLossATRRegime, positionATRRegimeLabel(pos, sc)); ok {
			slMult = v
		}
	}
	// Regime-aware tier multipliers freeze at first protection-sync after open
	// (when pos.RegimeWindows is stamped). Empty label → returns nil from
	// strategyTPTiersForRegime, so the plan emits SL only this cycle and
	// re-emits TPs next cycle once stampPositionRegimeIfOpened populates the
	// regime labels.
	tiers := strategyTPTiersForRegime(sc, atrRegime)
	if slMult <= 0 && len(tiers) == 0 {
		return hlProtectionPlan{}, false
	}
	tierCount := len(tiers)
	return hlProtectionPlan{
		Symbol: pos.Symbol,
		Side:   pos.Side,
		Size:   pos.Quantity,
		// #873: SL/TP triggers anchor to the FROZEN entry (riskAnchorPrice), not
		// the blended AvgCost — so a scale-in re-sizes protection to the new
		// total at the unchanged trigger geometry. Equals AvgCost for a position
		// that never scaled in.
		AvgCost:         pos.riskAnchorPrice(),
		EntryATR:        pos.EntryATR,
		StopLossATRMult: slMult,
		StopLossOID:     pos.StopLossOID,
		Tiers:           tiers,
		TPOIDs:          tpOIDsForTierCount(pos.TPOIDs, tierCount),
		TPArmedTiers:    tpArmedTiersForTierCount(pos.TPArmedTiers, tierCount),
	}, true
}

// strategyTPTiers returns the cumulative ATR take-profit tiers for the
// given strategy. Backwards-compatible shim that calls
// strategyTPTiersForRegime with an empty regime — fine for legacy
// scalar tiered_tp_atr* configs, but regime-aware evaluators return nil
// without a stamped pos.Regime.
//
// Callers that have a position in hand (e.g. buildHyperliquidProtectionPlan)
// should call strategyTPTiersForRegime directly so tiered_tp_atr_regime
// resolves with the position's stamped regime label (#733).
func strategyTPTiers(sc StrategyConfig) []hlProtectionTier {
	return strategyTPTiersForRegime(sc, "")
}

// strategyTPTiersForRegime returns the tiers resolved against a specific
// regime label. For scalar tiered_tp_atr* configs the regime is ignored.
// For regime-aware variants, an empty regime returns nil — the protection
// loop will simply emit only the SL this cycle and re-emit TPs next cycle
// once stampPositionRegimeIfOpened populates pos.Regime.
func strategyTPTiersForRegime(sc StrategyConfig, regime string) []hlProtectionTier {
	if !strategyUsesTieredTPATRClose(sc) {
		return nil
	}
	var raw interface{}
	regimeAware := false
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTieredTPATRCloseName(n) {
			continue
		}
		if n == "tiered_tp_atr_regime" || n == "tiered_tp_atr_live_regime" || n == dynamicCloseStrategyName {
			regimeAware = true
		}
		// #841 2b: unified per-regime block — select the active regime's scalar
		// ladder and resolve it through the scalar tier parser below. An unknown
		// / empty regime yields no tiers this cycle (SL-only), retried next cycle
		// once stampPositionRegimeIfOpened populates the label.
		if regimeAware && closeParamsAreUnifiedRegime(ref.Params) {
			scalar, _, ok := unifiedRegimeScalarParams(ref.Params, regime)
			if !ok {
				return nil
			}
			sel, _ := closeTierListParam(scalar)
			tiers := parseHLProtectionTiers(sel)
			if len(tiers) < 2 {
				return nil
			}
			return finalizeProtectionTiers(tiers)
		}
		if v, ok := closeTierListParam(ref.Params); ok {
			raw = v
			break
		}
		if regimeAware {
			// regime-aware variant with no explicit tiers → fall back to
			// the default regime block list if use_defaults is set.
			if useDefaults, ok := ref.Params["use_defaults"].(bool); ok && useDefaults {
				return defaultRegimeTPTiersForRegime(regime)
			}
			break
		}
	}
	if regimeAware {
		// Resolve regime-aware tier specs against the runtime regime label.
		tiers := resolveRegimeTPTiers(raw, regime)
		if len(tiers) < 2 {
			return nil
		}
		return finalizeProtectionTiers(tiers)
	}
	// Legacy scalar tiered_tp_atr*.
	tiers := parseHLProtectionTiers(raw)
	if len(tiers) == 0 {
		tiers = defaultHLProtectionTiers()
	}
	if len(tiers) < 2 {
		return nil
	}
	return finalizeProtectionTiers(tiers)
}

// defaultHLProtectionTiers is the canonical fallback tier ladder used when a
// tiered_tp_atr* close ref omits explicit tiers. #870 retuned it from the eager
// 1×/2× to a patient 3-rung 1.5×/3×/5× ladder (40%/80%/100% cumulative). Single
// source of truth for the scalar default — post_tp_sl.go derives
// tp_atr_fraction's default tier multiples from this (so the firing-tier
// multiple stays in sync if the ladder ever changes), and post_tp_sl.py mirrors
// it as _DEFAULT_SCALAR_TP_TIERS. NOTE: this also seeds HL on-chain reduce-only
// TP placement for every tiered_tp_atr* strategy on defaults — changing it
// moves live TP orders (#870 ⚠️ on-chain).
func defaultHLProtectionTiers() []hlProtectionTier {
	return []hlProtectionTier{
		{Multiple: 1.5, Fraction: 0.40},
		{Multiple: 3.0, Fraction: 0.80},
		{Multiple: 5.0, Fraction: 1.00},
	}
}

// finalizeProtectionTiers enforces the cumulative-fraction invariant and
// coerces the final tier to 1.0 so older two-tier configs preserve the
// "everything remaining" behavior from #604.
func finalizeProtectionTiers(tiers []hlProtectionTier) []hlProtectionTier {
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

// tieredTPATRPrices returns the take-profit prices for each configured tier
// given a position's entry price, side ("long"/"short"), and EntryATR. Display
// helper for Discord/Telegram alerts so the rendered TPs can never diverge
// from the on-chain reduce-only orders placed via strategyTPTiers
// (#659). Returns nil when the strategy doesn't use tiered_tp_atr* or any
// required input is missing.
func tieredTPATRPrices(sc StrategyConfig, side string, entryPrice, entryATR float64) []float64 {
	return tieredTPATRPricesFromTiers(strategyTPTiers(sc), side, entryPrice, entryATR)
}

// tieredTPATRPricesForRegime is tieredTPATRPrices with an explicit stamped regime
// label so tiered_tp_atr_regime / tiered_tp_atr_live_regime resolve like
// buildHyperliquidProtectionPlan (#738).
func tieredTPATRPricesForRegime(sc StrategyConfig, side string, entryPrice, entryATR float64, regime string) []float64 {
	return tieredTPATRPricesFromTiers(strategyTPTiersForRegime(sc, regime), side, entryPrice, entryATR)
}

// tieredTPATRPricesFromTiers is the price-only computation when the caller
// already has tiers in hand — lets trade-alert extras call
// strategyTPTiers once and zip prices with multiples (#665 review).
func tieredTPATRPricesFromTiers(tiers []hlProtectionTier, side string, entryPrice, entryATR float64) []float64 {
	if len(tiers) == 0 || entryATR <= 0 || entryPrice <= 0 {
		return nil
	}
	sideLower := strings.ToLower(strings.TrimSpace(side))
	prices := make([]float64, len(tiers))
	for i, t := range tiers {
		offset := t.Multiple * entryATR
		switch sideLower {
		case "short":
			prices[i] = entryPrice - offset
		case "long":
			prices[i] = entryPrice + offset
		default:
			return nil
		}
	}
	return prices
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
		multiple, mErr := floatFromAnyChecked(m["atr_multiple"])
		if mErr != nil {
			fmt.Printf("[WARN] hl-protection: tier[%d] atr_multiple invalid: %v — tier skipped\n", idx, mErr)
			continue
		}
		fraction, fErr := floatFromAnyChecked(m["close_fraction"])
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

func tpArmedTiersForTierCount(armed []bool, tierCount int) []bool {
	if tierCount <= 0 {
		return nil
	}
	out := make([]bool, tierCount)
	copy(out, armed)
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
var syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, bool) {
	result, stderr, err := RunHyperliquidSyncProtection(
		sc.Script, plan.Symbol, plan.Side, plan.Size, plan.AvgCost, plan.EntryATR,
		plan.StopLossATRMult, plan.Tiers, plan.StopLossOID, plan.TPOIDs, plan.TPArmedTiers,
		plan.ForceSLReplace, plan.ForceTPReplace, plan.CancelTPOIDs,
		reconcileFillHintsJSON,
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
	warnings := formatProtectionSyncWarnings(result)
	if len(warnings) > 0 {
		msg := fmt.Sprintf("%s %s protection partially failed: %v", sc.ID, plan.Symbol, warnings)
		if logger != nil {
			logger.Warn("%s", msg)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, msg)
	}
	return result, true
}

func applyHyperliquidProtectionSync(pos *Position, result *HyperliquidProtectionSyncResult, cancelTPOIDs []int64) {
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
	// #716 item 2: mark tiers as armed once Python reports a positive OID for
	// them. Done BEFORE the stale-OID clearing below so a tier that fills in
	// the same cycle it was first armed is recorded as armed —
	// findHighestClearedTier requires armed=true before treating OID=0 as
	// "cleared." This distinguishes a tier that filled (armed → 0) from a
	// tier whose first placement failed transiently (never armed, still 0).
	if len(pos.TPOIDs) > 0 {
		if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
			extended := make([]bool, len(pos.TPOIDs))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		for i, oid := range pos.TPOIDs {
			if oid > 0 {
				pos.TPArmedTiers[i] = true
			}
		}
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
		if len(pos.TPArmedTiers) < len(result.TPFilledExternally) {
			extended := make([]bool, len(result.TPFilledExternally))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		for idx, filled := range result.TPFilledExternally {
			if filled {
				pos.TPOIDs[idx] = 0
				// A filled tier is by definition one that was armed.
				pos.TPArmedTiers[idx] = true
			}
		}
	} else if result.TP1FilledExternally || result.TP2FilledExternally {
		if len(pos.TPOIDs) < 2 {
			pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, 2)
		}
		if len(pos.TPArmedTiers) < 2 {
			extended := make([]bool, 2)
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		if result.TP1FilledExternally {
			pos.TPOIDs[0] = 0
			pos.TPArmedTiers[0] = true
		}
		if result.TP2FilledExternally {
			pos.TPOIDs[1] = 0
			pos.TPArmedTiers[1] = true
		}
	}
	applySurplusTPCancelOutcome(pos, result, cancelTPOIDs)
}

// applySurplusTPCancelOutcome updates pos.TPOIDs after dynamic tier-count shrink
// cancels (#843): re-append failed cancels for retry, clear filled or successfully
// canceled surplus OIDs so dust cycles do not leave stale slots.
func applySurplusTPCancelOutcome(pos *Position, result *HyperliquidProtectionSyncResult, cancelTPOIDs []int64) {
	if pos == nil || result == nil {
		return
	}
	for _, oid := range result.TPCancelFailedOIDs {
		if oid <= 0 {
			continue
		}
		found := false
		for _, existing := range pos.TPOIDs {
			if existing == oid {
				found = true
				break
			}
		}
		if found {
			continue
		}
		pos.TPOIDs = append(pos.TPOIDs, oid)
		if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
			extended := make([]bool, len(pos.TPOIDs))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		pos.TPArmedTiers[len(pos.TPOIDs)-1] = true
	}
	if len(cancelTPOIDs) == 0 {
		return
	}
	failed := make(map[int64]struct{}, len(result.TPCancelFailedOIDs))
	for _, oid := range result.TPCancelFailedOIDs {
		if oid > 0 {
			failed[oid] = struct{}{}
		}
	}
	clear := make(map[int64]struct{})
	for _, oid := range result.TPCancelFilledOIDs {
		if oid > 0 {
			clear[oid] = struct{}{}
		}
	}
	for _, oid := range cancelTPOIDs {
		if oid <= 0 {
			continue
		}
		if _, isFailed := failed[oid]; isFailed {
			continue
		}
		clear[oid] = struct{}{}
	}
	if len(clear) == 0 || len(pos.TPOIDs) == 0 {
		return
	}
	if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
		extended := make([]bool, len(pos.TPOIDs))
		copy(extended, pos.TPArmedTiers)
		pos.TPArmedTiers = extended
	}
	for i, oid := range pos.TPOIDs {
		if _, ok := clear[oid]; ok {
			pos.TPOIDs[i] = 0
			pos.TPArmedTiers[i] = true
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
	db *StateDB,
	symbol string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
	logTag string,
	reconcileFillHintsJSON []byte,
) bool {
	if stratState == nil || symbol == "" {
		return false
	}
	var plan hlProtectionPlan
	var syncOK bool
	if strategyUsesDynamicRegimeClose(sc) {
		// Confirm-cycle state mutates Position fields — exclusive lock required
		// (RLock would race /status JSON reads of RegimeAppliedLabel, etc.).
		mu.Lock()
		if pos, ok := stratState.Positions[symbol]; ok {
			oldAppliedRegime := pos.RegimeAppliedLabel
			regimeChanged := advanceDynamicCloseRegime(pos, stratState, sc)
			plan, syncOK = buildHyperliquidProtectionPlan(sc, pos)
			if syncOK {
				plan.CancelTPOIDs = dynamicProtectionSurplusTPOIDs(pos.TPOIDs, len(plan.Tiers))
				if regimeChanged {
					forceSL, forceTP := dynamicProtectionForceReplace(sc, pos, plan, oldAppliedRegime, true)
					plan.ForceSLReplace = forceSL
					plan.ForceTPReplace = forceTP
				}
				if pos.ScaleInResizePending {
					// #873: a scale-in grew the size at the frozen triggers —
					// force-replace SL + already-placed TP tiers so they cover
					// the new total. OR with any regime-change force flags.
					fSL, fTP := scaleInProtectionForceReplace(pos, plan)
					plan.ForceSLReplace = plan.ForceSLReplace || fSL
					plan.ForceTPReplace = orForceReplace(plan.ForceTPReplace, fTP)
				}
			}
		}
		mu.Unlock()
	} else {
		mu.RLock()
		if pos, ok := stratState.Positions[symbol]; ok {
			plan, syncOK = buildHyperliquidProtectionPlan(sc, pos)
			if syncOK && pos.ScaleInResizePending {
				// #873: re-size SL + un-cleared TP tiers to the grown total at
				// the frozen trigger geometry; the watermark is not reset.
				plan.ForceSLReplace, plan.ForceTPReplace = scaleInProtectionForceReplace(pos, plan)
			}
		}
		mu.RUnlock()
	}
	if !syncOK {
		return false
	}
	protection, ok := syncHyperliquidProtection(sc, plan, notifier, logger, reconcileFillHintsJSON)
	if !ok || protection == nil {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != plan.Side {
		return false
	}
	applyHyperliquidProtectionSync(pos, protection, plan.CancelTPOIDs)
	// #873: the scale-in re-size has been applied on-chain; clear the one-shot
	// flag so subsequent syncs don't keep force-replacing unchanged triggers.
	// EXCEPT when the trailing walker owns the SL (effectiveTrailingStopPct > 0):
	// this sync only re-sizes the on-chain TP tiers (plan.StopLossATRMult == 0 →
	// no forceSL), and the SL re-size happens on a later Signal==0 cycle in the
	// trailing walker, which reads this flag from its Phase-1 snapshot and clears
	// it itself. Clearing here would hide the pending re-size from the walker and
	// leave the trailing SL covering only the pre-add size (#882 review).
	if effectiveTrailingStopPct(sc, pos) <= 0 {
		pos.ScaleInResizePending = false
	}
	if logger != nil && len(protection.TPCancelFilledOIDs) > 0 {
		logger.Info("surplus TP OIDs filled on-chain (reconciler will book): %v", protection.TPCancelFilledOIDs)
	}
	// Re-stamp TradeHistory so the trade alert picks up SL/TP prices placed
	// by the protection sync (#625). Without this, execute-path SL=0 leaves the
	// trade's StopLossTriggerPx unset even though the sync correctly populated
	// pos.StopLossTriggerPx — the alert then shows no SL price. Also stamp the
	// SL ATR mult + TP tier snapshot so trades opened pre-arming get the
	// fill-time config recorded in SQLite (#669).
	stampOpenTradeWithProtectionSnapshot(stratState, db, sc, symbol, pos)
	if logger != nil {
		logger.Info("%s (sl_oid=%d tp_oids=%v)", logTag, pos.StopLossOID, pos.TPOIDs)
	}
	return true
}

// hyperliquidPlacesOnChainTPs reports whether sc is configured to place
// per-strategy on-chain reduce-only TP orders for HL perps/manual in live mode.
// When true the in-process tiered close evaluator MUST be suppressed — the
// on-chain limits are the source of truth for tiered exits, and running both
// produces a race where the limit fills on-chain (shrinking position) and then
// the close evaluator emits another close_fraction sized off the stale virtual
// qty (#604 review #2). Paper mode has no on-chain TPs; the close evaluator is
// the only TP mechanism (#781).
func hyperliquidPlacesOnChainTPs(sc StrategyConfig) bool {
	if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
		return false
	}
	if !hyperliquidIsLive(sc.Args) {
		return false
	}
	// Use strategyUsesTieredTPATRClose — not strategyTPTiers(sc) — because the
	// latter passes an empty regime and returns nil for tiered_tp_atr_regime
	// configs until pos.Regime is stamped, which left this gate false forever
	// and skipped suppressing Python close evaluators (#750 / Sonnet review).
	return strategyUsesTieredTPATRClose(sc)
}

// closeStrategiesSuppressedByOnChainProtection is the set of close evaluator
// names that are functionally replaced by on-chain reduce-only TP orders.
// Adding a new ATR-tiered close evaluator? It probably belongs here too.
//
// Regime-aware variants (#733) are included — the frozen variant
// (`tiered_tp_atr_regime`) has its multipliers resolved at first protection
// sync and placed on-chain identically to scalar `tiered_tp_atr`. The live
// variant is virtual-only by spec — it's also suppressed here so an operator
// who configures both the live variant and on-chain reduce-only TPs doesn't
// end up with a race; instead they get a clean signal that on-chain
// protection trumps the in-process evaluator.
var closeStrategiesSuppressedByOnChainProtection = map[string]struct{}{
	"tiered_tp_atr":                     {},
	"tiered_tp_atr_live":                {},
	"tiered_tp_atr_regime":              {},
	"tiered_tp_atr_live_regime":         {},
	"tiered_tp_atr_live_regime_dynamic": {},
}

// isTieredTPATRCloseName returns true when name is any of the four
// tiered-TP-ATR close evaluators (scalar/regime × frozen/live).
func isTieredTPATRCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tiered_tp_atr", "tiered_tp_atr_live",
		"tiered_tp_atr_regime", "tiered_tp_atr_live_regime",
		dynamicCloseStrategyName:
		return true
	}
	return false
}

// closeStrategySuppressedByOnChainProtection reports whether sc's single close
// evaluator (#842) overlaps the on-chain reduce-only TPs and must be hidden
// from the Python check script — the ATR-tiered TP ladder is placed on-chain
// instead, so letting the software evaluator also fire would race the limit
// fills. Returns false when the strategy doesn't place on-chain TPs (paper, or
// non-tiered close) or has no close.
func closeStrategySuppressedByOnChainProtection(sc StrategyConfig) bool {
	if !hyperliquidPlacesOnChainTPs(sc) || sc.CloseStrategy == nil {
		return false
	}
	_, suppress := closeStrategiesSuppressedByOnChainProtection[strings.TrimSpace(sc.CloseStrategy.Name)]
	return suppress
}

// strategyConfigWithOnChainProtectionFilter returns a shallow copy of sc with
// the close ref dropped when it overlaps on-chain reduce-only TPs. Used at the
// runHyperliquidCheck call site so the Python check script doesn't evaluate the
// suppressed close. Other fields share storage with the original — callers must
// not mutate slice/map fields on the returned copy.
func strategyConfigWithOnChainProtectionFilter(sc StrategyConfig) StrategyConfig {
	if !closeStrategySuppressedByOnChainProtection(sc) {
		return sc
	}
	clone := sc
	clone.CloseStrategy = nil
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
