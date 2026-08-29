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

func defaultTrailingRatchetTiers() []trailingRatchetTier {
	return []trailingRatchetTier{
		{ATRMultiple: 2.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 2.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	}
}

var ratchetTierGroupDefaults = map[string][]trailingRatchetTier{
	"clean": {
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 4.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 6.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	},
	"choppy": {
		{ATRMultiple: 2.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 2.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	},
	"ranging_quiet": {
		{ATRMultiple: 0.75, CloseFraction: 0.4, TrailingMultAfter: 1.0},
		{ATRMultiple: 1.5, CloseFraction: 0.8, TrailingMultAfter: 0.75},
		{ATRMultiple: 2.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
	},
	"ranging_volatile": {
		{ATRMultiple: 1.0, CloseFraction: 0.4, TrailingMultAfter: 1.0},
		{ATRMultiple: 2.0, CloseFraction: 0.8, TrailingMultAfter: 0.75},
		{ATRMultiple: 3.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
	},
	"ranging_directional": {
		{ATRMultiple: 1.0, CloseFraction: 0.25, TrailingMultAfter: 1.0},
		{ATRMultiple: 2.0, CloseFraction: 0.50, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0.75, TrailingMultAfter: 0.8},
		{ATRMultiple: 4.5, CloseFraction: 0.75, TrailingMultAfter: 0.6},
	},
}

func ratchetCloseDefaultGroup(label string) (string, bool) {
	l := strings.TrimSpace(label)
	switch l {
	case "ranging_quiet", "ranging_volatile", "ranging_directional":
		return l, true
	case "ranging_directional_up", "ranging_directional_down":
		return "ranging_directional", true
	case "ranging":
		return "ranging_quiet", true
	}
	return regimeCloseDefaultGroup(l)
}

func defaultTrailingRatchetTiersForRegime(regime string) []trailingRatchetTier {
	group, ok := ratchetCloseDefaultGroup(regime)
	if !ok {
		return nil
	}
	src := ratchetTierGroupDefaults[group]
	if len(src) == 0 {
		return nil
	}
	out := make([]trailingRatchetTier, len(src))
	copy(out, src)
	return out
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
			if name == trailingTPRatchetRegimeCloseName {
				return defaultTrailingRatchetTiersForRegime(regime)
			}
			return defaultTrailingRatchetTiers()
		}
		if name == trailingTPRatchetRegimeCloseName {
			table, ok := raw.(map[string]interface{})
			if !ok || strings.TrimSpace(regime) == "" {
				return nil
			}
			key := strings.TrimSpace(regime)
			block, ok := table[key]
			if !ok {
				if regimeDirectionalSubs[key] {
					block, ok = table[regimeDirectionalBare]
				}
				if !ok {
					return nil
				}
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

func validateTrailingTPRatchetClose(sc StrategyConfig, labels []string, regimeEnabled bool) []string {
	if !strategyUsesTrailingTPRatchetClose(sc) {
		return nil
	}
	prefix := fmt.Sprintf("strategy[%s]", sc.ID)
	var errs []string
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* is HL perps/manual only", prefix))
	}
	regimeVariant := false
	for _, ref := range sc.closeRefs() {
		if strings.ToLower(strings.TrimSpace(ref.Name)) == trailingTPRatchetRegimeCloseName {
			regimeVariant = true
			break
		}
	}
	hasRegimeBlock := sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero()
	if regimeVariant {
		if !hasRegimeBlock {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime requires trailing_stop_atr_mult_regime (the per-regime opening trail / SL owner)", prefix))
		}
		if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime cannot combine with scalar trailing_stop_atr_mult (the trailing_stop_atr_mult_regime block owns the trail)", prefix))
		}
	} else {
		if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet requires trailing_stop_atr_mult > 0 (initial trail distance)", prefix))
		}
		if hasRegimeBlock {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet cannot combine with trailing_stop_atr_mult_regime (use trailing_tp_ratchet_regime)", prefix))
		}
	}
	if sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with trailing_stop_pct", prefix))
	}
	if sc.StopLossPct != nil && *sc.StopLossPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_pct", prefix))
	}
	if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_margin_pct", prefix))
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_atr_mult", prefix))
	}
	if sc.StopLossATRMultRegime.IsConfigured() {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_atr_mult_regime", prefix))
	}
	scalarInitialTrail := 0.0
	if !regimeVariant && sc.TrailingStopATRMult != nil {
		scalarInitialTrail = *sc.TrailingStopATRMult
	}
	regimeKeyOpen := func(key string) float64 {
		if sc.TrailingStopATRMultRegime == nil {
			return 0
		}
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRMultRegime, key); ok {
			return v
		}
		return 0
	}
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		sub := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		isRegime := name == trailingTPRatchetRegimeCloseName
		for k := range ref.Params {
			switch k {
			case "tp_tiers", "use_defaults":
			case "tiers":
				errs = append(errs, fmt.Sprintf("%s: legacy param %q is not supported — use tp_tiers (#841)", sub, k))
			default:
				errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: tp_tiers, use_defaults)", sub, k))
			}
		}
		if isRegime && !regimeEnabled {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime requires top-level regime.enabled=true", sub))
		}
		raw, hasTiers := closeTierListParam(ref.Params)
		if !hasTiers {
			if isRegime {
				for _, key := range labels {
					def := defaultTrailingRatchetTiersForRegime(key)
					errs = append(errs, validateTrailingRatchetInitialTrail(def, regimeKeyOpen(key), sub+".tp_tiers(default)."+key)...)
				}
			} else {
				def := defaultTrailingRatchetTiers()
				errs = append(errs, validateTrailingRatchetInitialTrail(def, scalarInitialTrail, sub+".tp_tiers(default)")...)
			}
			continue
		}
		if isRegime {
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
			bareDirectional := table[regimeDirectionalBare] != nil
			for _, key := range labels {
				block, ok := table[key]
				if !ok {
					if regimeLabelFamilyCovered(key, bareDirectional) {
						continue
					}
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: missing required regime key %q", sub, key))
					continue
				}
				tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers."+key)
				errs = append(errs, subErrs...)
				errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers."+key)...)
				errs = append(errs, validateTrailingRatchetInitialTrail(tiers, regimeKeyOpen(key), sub+".tp_tiers."+key)...)
			}
			continue
		}
		if table, ok := raw.(map[string]interface{}); ok {
			block := table["default"]
			if block == nil {
				block = table["ranging"]
			}
			if block == nil {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: object form requires a \"default\" or \"ranging\" key", sub))
				continue
			}
			tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers")
			errs = append(errs, subErrs...)
			errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
			errs = append(errs, validateTrailingRatchetInitialTrail(tiers, scalarInitialTrail, sub+".tp_tiers")...)
			continue
		}
		tiers, subErrs := parseTrailingRatchetTierList(raw, sub+".tp_tiers")
		errs = append(errs, subErrs...)
		errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
		errs = append(errs, validateTrailingRatchetInitialTrail(tiers, scalarInitialTrail, sub+".tp_tiers")...)
	}
	return errs
}

func validateTrailingRatchetInitialTrail(tiers []trailingRatchetTier, initialTrail float64, ctxLabel string) []string {
	if len(tiers) == 0 || initialTrail <= 0 {
		return nil
	}
	if tiers[0].TrailingMultAfter > initialTrail+1e-12 {
		return []string{fmt.Sprintf(
			"%s[0].trailing distance %.4g×ATR must be <= initial trailing_stop_atr_mult (%.4g×ATR) — the first ratchet rung can only tighten",
			ctxLabel, tiers[0].TrailingMultAfter, initialTrail,
		)}
	}
	return nil
}

func validateTrailingRatchetTierMonotonicity(tiers []trailingRatchetTier, ctxLabel string) []string {
	if len(tiers) < 2 {
		return nil
	}
	var errs []string
	prevTrail := tiers[0].TrailingMultAfter
	prevFrac := tiers[0].CloseFraction
	for i := 1; i < len(tiers); i++ {
		curTrail := tiers[i].TrailingMultAfter
		if curTrail > prevTrail+1e-12 {
			errs = append(errs, fmt.Sprintf(
				"%s[%d].trailing distance %.4g×ATR must be <= tier[%d] (%.4g×ATR) — ratchet tiers tighten monotonically",
				ctxLabel, i, curTrail, i-1, prevTrail,
			))
		}
		curFrac := tiers[i].CloseFraction
		if curFrac+1e-12 < prevFrac {
			errs = append(errs, fmt.Sprintf(
				"%s[%d].close_fraction %.4g must be >= tier[%d] close_fraction %.4g — close fractions are cumulative",
				ctxLabel, i, curFrac, i-1, prevFrac,
			))
		}
		prevTrail = curTrail
		prevFrac = curFrac
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
	if sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero() && pos != nil {
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRMultRegime, protectionATRRegimeLabel(pos, sc)); ok {
			return v
		}
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

func applyTrailingTPRatchet(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	mu *sync.RWMutex,
	logger *StrategyLogger,
) *RatchetTriggerAlert {
	if !strategyUsesTrailingTPRatchetClose(sc) || stratState == nil || symbol == "" || mark <= 0 {
		return nil
	}
	mu.Lock()
	var alert *RatchetTriggerAlert
	pos, ok := stratState.Positions[symbol]
	if ok {
		_, alert = applyTrailingTPRatchetToPosition(sc, pos, symbol, mark, logger)
	}
	mu.Unlock()
	return alert
}

func applyTrailingTPRatchetToPosition(sc StrategyConfig, pos *Position, symbol string, mark float64, logger *StrategyLogger) (bool, *RatchetTriggerAlert) {
	if !strategyUsesTrailingTPRatchetClose(sc) || pos == nil || symbol == "" || mark <= 0 {
		return false, nil
	}
	if pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		return false, nil
	}
	side := strings.ToLower(strings.TrimSpace(pos.Side))
	if side != "long" && side != "short" {
		return false, nil
	}
	regime := protectionATRRegimeLabel(pos, sc)
	tiers := trailingRatchetTiersForRegime(sc, regime)
	if len(tiers) == 0 {
		return false, nil
	}
	anchor := pos.riskAnchorPrice()
	profitDistance := mark - anchor
	if side == "short" {
		profitDistance = anchor - mark
	}
	atrProfit := profitDistance / pos.EntryATR
	clearedIdx, clearedOK := findHighestMarkClearedRatchetTier(tiers, atrProfit, pos.SLAdjustedTiersProcessed)
	if !clearedOK {
		return false, nil
	}
	newMult := tiers[clearedIdx].TrailingMultAfter
	current := effectiveTrailingRatchetMult(pos, sc)
	if newMult >= current-1e-12 {
		if pos.SLAdjustedTiersProcessed <= clearedIdx {
			pos.SLAdjustedTiersProcessed = clearedIdx + 1
		}
		return false, nil
	}
	mult := newMult
	pos.PostTPTrailingATRMult = &mult
	pos.SLAdjustedTiersProcessed = clearedIdx + 1
	if logger != nil {
		logger.Info("trailing_tp_ratchet: %s tier %d cleared — trail tightened to %.4g×ATR (from %.4g×ATR)",
			symbol, clearedIdx, newMult, current)
	}
	alert := buildRatchetTriggerAlert(sc, pos, symbol, side, regime, mark, anchor, atrProfit, tiers, clearedIdx, current, newMult)
	return true, alert
}

func buildRatchetTriggerAlert(sc StrategyConfig, pos *Position, symbol, side, regime string, mark, anchor, atrProfit float64, tiers []trailingRatchetTier, clearedIdx int, oldMult, newMult float64) *RatchetTriggerAlert {
	entryATR := pos.EntryATR
	contractMult := 1.0
	if pos.Multiplier > 0 {
		contractMult = pos.Multiplier
	}
	profitDistance := mark - anchor
	if side == "short" {
		profitDistance = anchor - mark
	}
	hwm := pos.StopLossHighWaterPx
	if side == "long" {
		if hwm <= 0 || mark > hwm {
			hwm = mark
		}
	} else {
		if hwm <= 0 || mark < hwm {
			hwm = mark
		}
	}
	intendedSL := 0.0
	if entryATR > 0 && hwm > 0 && newMult > 0 {
		if side == "long" {
			intendedSL = hwm - newMult*entryATR
		} else {
			intendedSL = hwm + newMult*entryATR
		}
		if intendedSL <= 0 {
			intendedSL = 0
		}
	}
	a := &RatchetTriggerAlert{
		StrategyID:           sc.ID,
		Symbol:               symbol,
		Side:                 side,
		TierIdx:              clearedIdx,
		TotalTiers:           len(tiers),
		TierATRMultiple:      tiers[clearedIdx].ATRMultiple,
		TierTriggerPx:        atrTierTriggerPx(side, anchor, entryATR, tiers[clearedIdx].ATRMultiple),
		MarkPrice:            mark,
		AnchorPrice:          anchor,
		EntryATR:             entryATR,
		ProfitATR:            atrProfit,
		ProfitUSD:            profitDistance * pos.Quantity * contractMult,
		OldTrailMult:         oldMult,
		NewTrailMult:         newMult,
		HighWaterMark:        hwm,
		IntendedSLTriggerPx:  intendedSL,
		RegimeLabel:          regime,
		PositionRegimeAtOpen: pos.Regime,
	}
	if clearedIdx+1 < len(tiers) {
		nt := tiers[clearedIdx+1]
		a.HasNextTier = true
		a.NextTierATRMultiple = nt.ATRMultiple
		a.NextTierTrailAfter = nt.TrailingMultAfter
		a.NextTierTriggerPx = atrTierTriggerPx(side, anchor, entryATR, nt.ATRMultiple)
	}
	return a
}

var manualCloseEvaluatorDriftWarned sync.Map

func manualCloseEvaluatorDriftedFromTPs(sc StrategyConfig, pos *Position) bool {
	return pos != nil && len(pos.TPOIDs) > 0 && strategyUsesTrailingTPRatchetClose(sc)
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
