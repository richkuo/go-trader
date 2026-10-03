package main

import (
	"fmt"
	"sort"
	"strings"
)

func hyperliquidTieredTPStrategy(sc StrategyConfig) bool {
	return sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual")
}

func parseTPTierLadderStrict(raw interface{}, ctxLabel string) ([]hlProtectionTier, []string) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, []string{fmt.Sprintf("%s: must be a list, got %T", ctxLabel, raw)}
	}
	var errs []string
	tiers := make([]hlProtectionTier, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s[%d]: must be an object, got %T", ctxLabel, i, item))
			continue
		}
		multiple, mErr := floatFromAnyChecked(m["atr_multiple"])
		if mErr != nil || multiple <= 0 {
			errs = append(errs, fmt.Sprintf("%s[%d].atr_multiple: must be > 0", ctxLabel, i))
		}
		fraction, fErr := floatFromAnyChecked(m["close_fraction"])
		if fErr != nil || fraction <= 0 || fraction > 1 {
			errs = append(errs, fmt.Sprintf("%s[%d].close_fraction: must be in (0, 1]", ctxLabel, i))
		}
		tiers = append(tiers, hlProtectionTier{Multiple: multiple, Fraction: fraction})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].Multiple < tiers[j].Multiple })
	return tiers, nil
}

func validateTPTierLadder(tiers []hlProtectionTier, ctxLabel string) []string {
	if len(tiers) < 2 {
		return nil
	}
	var errs []string
	for i := 1; i < len(tiers); i++ {
		if tiers[i].Multiple < tiers[i-1].Multiple {
			errs = append(errs, fmt.Sprintf("%s: tier %d atr_multiple %g is below tier %d atr_multiple %g; list tiers in increasing atr_multiple order (the on-chain take-profit ladder keeps this order)", ctxLabel, i, tiers[i].Multiple, i-1, tiers[i-1].Multiple))
			continue
		}
		if tiers[i].Fraction <= tiers[i-1].Fraction {
			errs = append(errs, fmt.Sprintf("%s: tier %d close_fraction %g must be greater than tier %d close_fraction %g; Hyperliquid take-profit fractions are cumulative and must strictly increase, or no on-chain tier can be placed", ctxLabel, i, tiers[i].Fraction, i-1, tiers[i-1].Fraction))
		}
	}
	return errs
}

func validateTPTierLadders(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	regimeEnabled := cfg.Regime != nil && cfg.Regime.Enabled
	var errs []string
	for i := range cfg.Strategies {
		sc := cfg.Strategies[i]
		if !hyperliquidTieredTPStrategy(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		if sc.ID == "" {
			prefix = fmt.Sprintf("strategy[%d]", i)
		}
		labels := canonicalTrendRegimeLabels
		if regimeEnabled {
			labels = regimeLabelsForStrategyWindow(sc, cfg.Regime, "atr")
		}
		for _, ref := range sc.closeRefs() {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if !isTieredTPATRCloseName(name) {
				continue
			}
			ctx := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
			regimeAware := name == "tiered_tp_atr_regime" || name == "tiered_tp_atr_live_regime" || name == dynamicCloseStrategyName
			if regimeAware && closeParamsAreUnifiedRegime(ref.Params) {
				errs = append(errs, validateUnifiedTPTierLadders(ref.Params, ctx)...)
				continue
			}
			raw, has := closeTierListParam(ref.Params)
			if !has {
				continue
			}
			if regimeAware {
				for _, label := range labels {
					tiers := resolveRegimeTPTiers(raw, label)
					errs = append(errs, validateTPTierLadder(tiers, fmt.Sprintf("%s.tp_tiers[regime %s]", ctx, label))...)
				}
				continue
			}
			tiers, parseErrs := parseTPTierLadderStrict(raw, ctx+".tp_tiers")
			if len(parseErrs) > 0 {
				errs = append(errs, parseErrs...)
				continue
			}
			errs = append(errs, validateTPTierLadder(tiers, ctx+".tp_tiers")...)
		}
	}
	return errs
}

func validateUnifiedTPTierLadders(params map[string]interface{}, ctxLabel string) []string {
	trend, ok := params[regimeClassifierKey].(map[string]interface{})
	if !ok {
		return nil
	}
	labels := make([]string, 0, len(trend))
	for label := range trend {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var errs []string
	for _, label := range labels {
		block, ok := trend[label].(map[string]interface{})
		if !ok {
			continue
		}
		raw, ok := block["tp_tiers"]
		if !ok {
			continue
		}
		tiers, parseErrs := parseTPTierLadderStrict(raw, fmt.Sprintf("%s.%s.%s.tp_tiers", ctxLabel, regimeClassifierKey, label))
		if len(parseErrs) > 0 {
			continue
		}
		errs = append(errs, validateTPTierLadder(tiers, fmt.Sprintf("%s.%s.%s.tp_tiers", ctxLabel, regimeClassifierKey, label))...)
	}
	return errs
}

func warnHyperliquidTieredATRSourceLive(cfg *Config) {
	if cfg == nil {
		return
	}
	for _, sc := range cfg.Strategies {
		if !hyperliquidTieredTPStrategy(sc) {
			continue
		}
		for _, ref := range sc.closeRefs() {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if name != "tiered_tp_atr_live" && name != "tiered_tp_atr_live_regime" && name != dynamicCloseStrategyName {
				continue
			}
			source, ok := ref.Params["atr_source"].(string)
			if !ok || strings.ToLower(strings.TrimSpace(source)) != "live" {
				continue
			}
			fmt.Printf("[WARN] %s: close_strategy %s sets atr_source \"live\", which no longer moves Hyperliquid take-profit tiers each cycle. Paper, live and the backtester price the tiers from the entry ATR, the risk anchor and the position regime, as the on-chain orders do; the live ATR prices a tier only for a position with no entry ATR (#1576).\n", sc.ID, ref.Name)
		}
	}
}
