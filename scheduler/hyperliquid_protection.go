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
	// SL ATR multiplier: legacy scalar `stop_loss_atr_mult` wins when present;
	// otherwise the regime-aware sibling resolves via pos.Regime. Validation
	// ensures only one is set (#733).
	slMult := 0.0
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		slMult = *sc.StopLossATRMult
	} else if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		if v, ok := resolveRegimeATR(*sc.StopLossATRRegime, pos.Regime); ok {
			slMult = v
		}
	}
	// Regime-aware tier multipliers freeze at first protection-sync after open
	// (when pos.Regime is stamped). Empty pos.Regime → returns nil from
	// strategyTPTiersForRegime, so the plan emits SL only this cycle and
	// re-emits TPs next cycle once stampPositionRegimeIfOpened populates the
	// regime label.
	tiers := strategyTPTiersForRegime(sc, pos.Regime)
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
	for _, ref := range sc.CloseStrategies {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTieredTPATRCloseName(n) {
			continue
		}
		if n == "tiered_tp_atr_regime" || n == "tiered_tp_atr_live_regime" {
			regimeAware = true
		}
		if v, ok := ref.Params["tiers"]; ok {
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
		tiers = []hlProtectionTier{
			{Multiple: 1, Fraction: 0.5},
			{Multiple: 2, Fraction: 1},
		}
	}
	if len(tiers) < 2 {
		return nil
	}
	return finalizeProtectionTiers(tiers)
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
	"tiered_tp_atr":             {},
	"tiered_tp_atr_live":        {},
	"tiered_tp_atr_regime":      {},
	"tiered_tp_atr_live_regime": {},
}

// isTieredTPATRCloseName returns true when name is any of the four
// tiered-TP-ATR close evaluators (scalar/regime × frozen/live).
func isTieredTPATRCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tiered_tp_atr", "tiered_tp_atr_live",
		"tiered_tp_atr_regime", "tiered_tp_atr_live_regime":
		return true
	}
	return false
}

// filterCloseStrategiesForHLOnChainProtection returns the close-strategy list
// with names that overlap on-chain reduce-only TPs removed. Other close
// strategies (tp_at_pct, tiered_tp_pct, …) pass through unchanged so an
// operator can layer a percent-based stop alongside the ATR-tiered TP limits.
func filterCloseStrategiesForHLOnChainProtection(sc StrategyConfig) []StrategyRef {
	if !hyperliquidPlacesOnChainTPs(sc) {
		return sc.CloseStrategies
	}
	if len(sc.CloseStrategies) == 0 {
		return sc.CloseStrategies
	}
	out := make([]StrategyRef, 0, len(sc.CloseStrategies))
	for _, ref := range sc.CloseStrategies {
		trimmed := strings.TrimSpace(ref.Name)
		if _, suppress := closeStrategiesSuppressedByOnChainProtection[trimmed]; suppress {
			continue
		}
		out = append(out, ref)
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
