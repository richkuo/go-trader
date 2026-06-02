package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	trailingTPRatchetCloseName       = "trailing_tp_ratchet"
	trailingTPRatchetRegimeCloseName = "trailing_tp_ratchet_regime"
)

// trailingRatchetTier is one rung of a trailing_tp_ratchet* close ref.
type trailingRatchetTier struct {
	ATRMultiple       float64
	CloseFraction     float64
	TrailingMultAfter float64
}

func isTrailingTPRatchetCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case trailingTPRatchetCloseName, trailingTPRatchetRegimeCloseName:
		return true
	}
	return false
}

func strategyUsesTrailingTPRatchetClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if isTrailingTPRatchetCloseName(ref.Name) {
			return true
		}
	}
	return false
}

func resolveTrailingMultAfter(tier map[string]interface{}, firingMultiple float64) (float64, error) {
	_, hasAbs := tier["trailing_mult_after"]
	_, hasFrac := tier["tp_atr_fraction"]
	if hasAbs && hasFrac {
		return 0, fmt.Errorf("cannot combine trailing_mult_after with tp_atr_fraction")
	}
	if hasAbs {
		mult, err := floatFromAnyChecked(tier["trailing_mult_after"])
		if err != nil || mult <= 0 {
			return 0, fmt.Errorf("trailing_mult_after must be > 0")
		}
		return mult, nil
	}
	if hasFrac {
		frac, err := floatFromAnyChecked(tier["tp_atr_fraction"])
		if err != nil || frac <= 0 {
			return 0, fmt.Errorf("tp_atr_fraction must be > 0")
		}
		if firingMultiple <= 0 {
			return 0, fmt.Errorf("firing tier atr_multiple must be > 0 for tp_atr_fraction")
		}
		return frac * firingMultiple, nil
	}
	return 0, fmt.Errorf("requires exactly one of trailing_mult_after or tp_atr_fraction")
}

func parseTrailingRatchetTier(m map[string]interface{}, ctxLabel string, idx int) (trailingRatchetTier, []string) {
	var errs []string
	mult, err := floatFromAnyChecked(firstPresent(m, "atr_multiple", "multiple", "atr"))
	if err != nil || mult <= 0 {
		errs = append(errs, fmt.Sprintf("%s[%d].atr_multiple: must be > 0", ctxLabel, idx))
		return trailingRatchetTier{}, errs
	}
	frac := 0.0
	if raw := firstPresent(m, "close_fraction", "fraction"); raw != nil {
		frac, err = floatFromAnyChecked(raw)
		if err != nil || frac < 0 || frac > 1 {
			errs = append(errs, fmt.Sprintf("%s[%d].close_fraction: must be in [0, 1]", ctxLabel, idx))
			return trailingRatchetTier{}, errs
		}
	}
	trail, terr := resolveTrailingMultAfter(m, mult)
	if terr != nil {
		errs = append(errs, fmt.Sprintf("%s[%d]: %v", ctxLabel, idx, terr))
		return trailingRatchetTier{}, errs
	}
	allowed := map[string]bool{
		"atr_multiple": true, "multiple": true, "atr": true,
		"close_fraction": true, "fraction": true,
		"trailing_mult_after": true, "tp_atr_fraction": true,
	}
	for k := range m {
		if !allowed[k] {
			errs = append(errs, fmt.Sprintf("%s[%d]: unknown key %q", ctxLabel, idx, k))
		}
	}
	return trailingRatchetTier{
		ATRMultiple:       mult,
		CloseFraction:     frac,
		TrailingMultAfter: trail,
	}, errs
}

func parseTrailingRatchetTierList(raw interface{}, ctxLabel string) ([]trailingRatchetTier, []string) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, []string{fmt.Sprintf("%s: must be a list, got %T", ctxLabel, raw)}
	}
	var errs []string
	out := make([]trailingRatchetTier, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s[%d]: must be an object", ctxLabel, idx))
			continue
		}
		tier, sub := parseTrailingRatchetTier(m, ctxLabel, idx)
		errs = append(errs, sub...)
		if len(sub) == 0 {
			out = append(out, tier)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ATRMultiple < out[j].ATRMultiple })
	if len(out) == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Sprintf("%s: must contain at least one valid tier", ctxLabel))
	}
	return out, errs
}

func trailingRatchetTiersForRegime(sc StrategyConfig, regime string) []trailingRatchetTier {
	if !strategyUsesTrailingTPRatchetClose(sc) {
		return nil
	}
	for _, ref := range sc.closeRefs() {
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTrailingTPRatchetCloseName(name) {
			continue
		}
		raw, ok := closeTierListParam(ref.Params)
		if !ok {
			return nil
		}
		if name == trailingTPRatchetRegimeCloseName {
			table, ok := raw.(map[string]interface{})
			if !ok || strings.TrimSpace(regime) == "" {
				return nil
			}
			block, ok := table[strings.TrimSpace(regime)]
			if !ok {
				return nil
			}
			tiers, _ := parseTrailingRatchetTierList(block, ref.Name+".tp_tiers."+regime)
			return tiers
		}
		if table, ok := raw.(map[string]interface{}); ok {
			block := table["default"]
			if block == nil {
				block = table["ranging"]
			}
			tiers, _ := parseTrailingRatchetTierList(block, ref.Name+".tp_tiers")
			return tiers
		}
		tiers, _ := parseTrailingRatchetTierList(raw, ref.Name+".tp_tiers")
		return tiers
	}
	return nil
}

func validateTrailingTPRatchetClose(sc StrategyConfig, labels []string) []string {
	if !strategyUsesTrailingTPRatchetClose(sc) {
		return nil
	}
	prefix := fmt.Sprintf("strategy[%s]", sc.ID)
	var errs []string
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* is HL perps/manual only", prefix))
	}
	if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* requires trailing_stop_atr_mult > 0 (initial trail distance)", prefix))
	}
	if sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with trailing_stop_pct", prefix))
	}
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with trailing_stop_atr_regime", prefix))
	}
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		sub := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		raw, hasTiers := closeTierListParam(ref.Params)
		if !hasTiers {
			errs = append(errs, fmt.Sprintf("%s: missing tp_tiers", sub))
			continue
		}
		for k := range ref.Params {
			switch k {
			case "tp_tiers", "use_defaults":
			case "tiers":
				errs = append(errs, fmt.Sprintf("%s: legacy param %q is not supported — use tp_tiers (#841)", sub, k))
			default:
				errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: tp_tiers, use_defaults)", sub, k))
			}
		}
		if name == trailingTPRatchetRegimeCloseName {
			table, ok := raw.(map[string]interface{})
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: must be a regime-keyed object", sub))
				continue
			}
			labelSet := make(map[string]bool, len(labels))
			for _, l := range labels {
				labelSet[l] = true
			}
			for key := range table {
				if !labelSet[key] {
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: unknown regime key %q (valid: %s)", sub, key, strings.Join(labels, ", ")))
				}
			}
			for _, key := range labels {
				block, ok := table[key]
				if !ok {
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: missing required regime key %q", sub, key))
					continue
				}
				tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers."+key)
				errs = append(errs, subErrs...)
				errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers."+key)...)
			}
			continue
		}
		if table, ok := raw.(map[string]interface{}); ok {
			block := table["default"]
			if block == nil {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: object form requires a \"default\" key", sub))
				continue
			}
			tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers")
			errs = append(errs, subErrs...)
			errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
			continue
		}
		tiers, subErrs := parseTrailingRatchetTierList(raw, sub+".tp_tiers")
		errs = append(errs, subErrs...)
		errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
	}
	return errs
}

func validateTrailingRatchetTierMonotonicity(tiers []trailingRatchetTier, ctxLabel string) []string {
	if len(tiers) < 2 {
		return nil
	}
	var errs []string
	prev := tiers[0].TrailingMultAfter
	for i := 1; i < len(tiers); i++ {
		cur := tiers[i].TrailingMultAfter
		if cur > prev+1e-12 {
			errs = append(errs, fmt.Sprintf(
				"%s[%d].trailing distance %.4g×ATR must be <= tier[%d] (%.4g×ATR) — ratchet tiers tighten monotonically",
				ctxLabel, i, cur, i-1, prev,
			))
		}
		prev = cur
	}
	return errs
}

func effectiveTrailingRatchetMult(pos *Position, sc StrategyConfig) float64 {
	if pos != nil && pos.PostTPTrailingATRMult != nil && *pos.PostTPTrailingATRMult > 0 {
		return *pos.PostTPTrailingATRMult
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return *sc.TrailingStopATRMult
	}
	return 0
}

func findHighestMarkClearedRatchetTier(tiers []trailingRatchetTier, atrProfit float64, fromIdx int) (int, bool) {
	if fromIdx < 0 {
		fromIdx = 0
	}
	highest := -1
	for i := fromIdx; i < len(tiers); i++ {
		if atrProfit+1e-12 >= tiers[i].ATRMultiple {
			highest = i
		}
	}
	return highest, highest >= 0
}

// applyTrailingTPRatchet stamps a tighter PostTPTrailingATRMult when mark-based
// tier thresholds are newly cleared. Reuses SLAdjustedTiersProcessed as the
// idempotency watermark (ratchet closes do not use on-chain TP OIDs or sl_after).
func applyTrailingTPRatchet(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	mu *sync.RWMutex,
	logger *StrategyLogger,
) {
	if !strategyUsesTrailingTPRatchetClose(sc) || stratState == nil || symbol == "" || mark <= 0 {
		return
	}
	mu.Lock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		mu.Unlock()
		return
	}
	side := pos.Side
	avgCost := pos.AvgCost
	entryATR := pos.EntryATR
	regime := protectionATRRegimeLabel(pos, sc)
	watermark := pos.SLAdjustedTiersProcessed
	mu.Unlock()

	tiers := trailingRatchetTiersForRegime(sc, regime)
	if len(tiers) == 0 {
		return
	}
	profitDistance := mark - avgCost
	if side == "short" {
		profitDistance = avgCost - mark
	}
	atrProfit := profitDistance / entryATR
	clearedIdx, clearedOK := findHighestMarkClearedRatchetTier(tiers, atrProfit, watermark)
	if !clearedOK {
		return
	}
	newMult := tiers[clearedIdx].TrailingMultAfter
	current := effectiveTrailingRatchetMult(pos, sc)
	if newMult >= current-1e-12 {
		mu.Lock()
		if p, ok := stratState.Positions[symbol]; ok && p != nil && p.SLAdjustedTiersProcessed <= clearedIdx {
			p.SLAdjustedTiersProcessed = clearedIdx + 1
		}
		mu.Unlock()
		return
	}
	mu.Lock()
	p, ok := stratState.Positions[symbol]
	if !ok || p == nil {
		mu.Unlock()
		return
	}
	mult := newMult
	p.PostTPTrailingATRMult = &mult
	p.SLAdjustedTiersProcessed = clearedIdx + 1
	mu.Unlock()
	if logger != nil {
		logger.Info("trailing_tp_ratchet: %s tier %d cleared — trail tightened to %.4g×ATR (from %.4g×ATR)",
			symbol, clearedIdx, newMult, current)
	}
}

func trailingRatchetRulesEqualForReload(a, b StrategyConfig) bool {
	return trailingRatchetFingerprint(a) == trailingRatchetFingerprint(b)
}

func trailingRatchetFingerprint(sc StrategyConfig) string {
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		b, err := json.Marshal(ref.Params)
		if err != nil {
			return fmt.Sprintf("%s:%v", ref.Name, ref.Params)
		}
		return ref.Name + ":" + string(b)
	}
	return ""
}
