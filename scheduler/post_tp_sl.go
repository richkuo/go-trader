package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SLAfterRule describes how to adjust the stop-loss trigger after a tiered TP
// fills. Configured per-tier (with optional strategy-level default) on
// tiered_tp_atr / tiered_tp_atr_live close evaluators. See #708.
type SLAfterRule struct {
	// Kind is "" (no rule, default behavior preserved), "breakeven",
	// "atr_offset", or "trail_from_here".
	Kind string
	// ATRMult is the signed multiplier for "atr_offset"; positive values move
	// the SL toward profit (long: above AvgCost, short: below). Zero is
	// equivalent to "breakeven" but legal.
	ATRMult float64
	// TrailATRMult is the trail distance in ATR units for "trail_from_here".
	// Must be > 0.
	TrailATRMult float64
}

// IsEmpty reports whether the rule is a no-op.
func (r SLAfterRule) IsEmpty() bool { return r.Kind == "" }

// computePostTPStopLossTrigger returns the proposed new SL trigger price after
// a TP tier fires. ok=false signals insufficient inputs (rule kind requires
// ATR but EntryATR is missing, unknown side, etc.). The caller is responsible
// for the "never worse than current SL" clamp; this helper returns the rule's
// natural target.
//
// For trail_from_here the returned price is the initial trailing trigger
// seeded at currentMark; subsequent walking is handled by the trailing-stop
// walker. Pass currentMark=0 for non-trailing rules.
func computePostTPStopLossTrigger(
	rule SLAfterRule, side string, avgCost, entryATR, currentMark float64,
) (triggerPx float64, mode string, ok bool) {
	sideLower := strings.ToLower(strings.TrimSpace(side))
	if sideLower != "long" && sideLower != "short" {
		return 0, "", false
	}
	if avgCost <= 0 {
		return 0, "", false
	}
	switch rule.Kind {
	case "":
		return 0, "", false
	case "breakeven":
		return avgCost, "breakeven", true
	case "atr_offset":
		if entryATR <= 0 {
			return 0, "", false
		}
		var px float64
		if sideLower == "long" {
			px = avgCost + rule.ATRMult*entryATR
		} else {
			px = avgCost - rule.ATRMult*entryATR
		}
		if px <= 0 {
			return 0, "", false
		}
		return px, formatATROffsetMode(rule.ATRMult), true
	case "trail_from_here":
		if entryATR <= 0 || currentMark <= 0 || rule.TrailATRMult <= 0 {
			return 0, "", false
		}
		var px float64
		if sideLower == "long" {
			px = currentMark - rule.TrailATRMult*entryATR
		} else {
			px = currentMark + rule.TrailATRMult*entryATR
		}
		if px <= 0 {
			return 0, "", false
		}
		return px, fmt.Sprintf("trail %g×ATR", rule.TrailATRMult), true
	}
	return 0, "", false
}

// formatATROffsetMode preserves the operator's original kind in the audit
// trail: an explicit {atr_mult: 0} renders "atr+0" rather than collapsing to
// "breakeven", so DM/log readers can reconcile against config without
// guessing which form was written. The "breakeven" string is reserved for the
// explicit Kind=="breakeven" rule.
func formatATROffsetMode(m float64) string {
	sign := "+"
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("atr%s%g", sign, m)
}

// validateSLAfterRule sanity-checks a rule's fields. Returns nil for the empty
// rule. Use from config parsing.
func validateSLAfterRule(rule SLAfterRule) error {
	switch rule.Kind {
	case "", "breakeven", "atr_offset":
		return nil
	case "trail_from_here":
		if rule.TrailATRMult <= 0 {
			return errors.New("sl_after trail_from_here requires atr_mult > 0")
		}
		return nil
	default:
		return fmt.Errorf("sl_after kind %q is not recognized (expected breakeven|atr_offset|trail_from_here)", rule.Kind)
	}
}

// parseSLAfterRule converts the raw JSON value found at params["sl_after"] (or
// inside a tier object) into a typed SLAfterRule. Accepted shapes:
//
//	"breakeven"                                          string shorthand
//	{"atr_mult": 0.25}                                    → atr_offset
//	{"trail_from_here": {"atr_mult": 1.0}}                → trail_from_here
//	{"kind": "atr_offset", "atr_mult": 0.25}              → explicit kind
//	{"kind": "trail_from_here", "atr_mult": 1.0}          → explicit kind
//
// nil input returns an empty rule with no error (field omitted).
func parseSLAfterRule(raw interface{}) (SLAfterRule, error) {
	if raw == nil {
		return SLAfterRule{}, nil
	}
	switch v := raw.(type) {
	case string:
		kind := strings.ToLower(strings.TrimSpace(v))
		switch kind {
		case "":
			return SLAfterRule{}, nil
		case "breakeven":
			return SLAfterRule{Kind: "breakeven"}, nil
		default:
			return SLAfterRule{}, fmt.Errorf("sl_after string %q is not recognized (expected \"breakeven\")", v)
		}
	case map[string]interface{}:
		// Explicit kind takes precedence.
		if kindRaw, ok := v["kind"]; ok {
			kindStr, isStr := kindRaw.(string)
			if !isStr {
				return SLAfterRule{}, fmt.Errorf("sl_after.kind must be a string, got %T", kindRaw)
			}
			kind := strings.ToLower(strings.TrimSpace(kindStr))
			switch kind {
			case "breakeven":
				return SLAfterRule{Kind: "breakeven"}, nil
			case "atr_offset":
				mult, err := floatFromAnyChecked(firstPresent(v, "atr_mult", "atr_offset"))
				if err != nil {
					return SLAfterRule{}, fmt.Errorf("sl_after kind=atr_offset: %w", err)
				}
				return SLAfterRule{Kind: "atr_offset", ATRMult: mult}, nil
			case "trail_from_here":
				mult, err := floatFromAnyChecked(firstPresent(v, "atr_mult", "trail_atr_mult"))
				if err != nil {
					return SLAfterRule{}, fmt.Errorf("sl_after kind=trail_from_here: %w", err)
				}
				rule := SLAfterRule{Kind: "trail_from_here", TrailATRMult: mult}
				return rule, validateSLAfterRule(rule)
			default:
				return SLAfterRule{}, fmt.Errorf("sl_after kind %q is not recognized", kind)
			}
		}
		// Implicit discrimination: trail_from_here nested object.
		if trailRaw, ok := v["trail_from_here"]; ok {
			trailMap, isMap := trailRaw.(map[string]interface{})
			if !isMap {
				return SLAfterRule{}, fmt.Errorf("sl_after.trail_from_here must be an object, got %T", trailRaw)
			}
			mult, err := floatFromAnyChecked(firstPresent(trailMap, "atr_mult", "trail_atr_mult"))
			if err != nil {
				return SLAfterRule{}, fmt.Errorf("sl_after.trail_from_here: %w", err)
			}
			rule := SLAfterRule{Kind: "trail_from_here", TrailATRMult: mult}
			return rule, validateSLAfterRule(rule)
		}
		// Implicit discrimination: atr_mult at top level → atr_offset.
		if _, ok := firstNonNil(v, "atr_mult", "atr_offset"); ok {
			mult, err := floatFromAnyChecked(firstPresent(v, "atr_mult", "atr_offset"))
			if err != nil {
				return SLAfterRule{}, fmt.Errorf("sl_after atr_mult: %w", err)
			}
			return SLAfterRule{Kind: "atr_offset", ATRMult: mult}, nil
		}
		return SLAfterRule{}, fmt.Errorf("sl_after object must contain \"kind\", \"atr_mult\", or \"trail_from_here\"")
	default:
		return SLAfterRule{}, fmt.Errorf("sl_after must be a string or object, got %T", raw)
	}
}

func firstNonNil(m map[string]interface{}, keys ...string) (interface{}, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// tierSLAfterRules carries the strategy-level default plus per-tier overrides
// (aligned with strategyTPTiers output by ascending atr_multiple).
type tierSLAfterRules struct {
	Default SLAfterRule
	PerTier []SLAfterRule
}

// ForTier returns the rule to apply when tier index idx fires: tier-level
// override when set, otherwise the strategy-level default, otherwise the empty
// rule (no adjustment).
func (r tierSLAfterRules) ForTier(idx int) SLAfterRule {
	if idx >= 0 && idx < len(r.PerTier) && !r.PerTier[idx].IsEmpty() {
		return r.PerTier[idx]
	}
	return r.Default
}

// HasAny reports whether the strategy configures any sl_after rule (default or
// per-tier). Cheap check before walking tiers.
func (r tierSLAfterRules) HasAny() bool {
	if !r.Default.IsEmpty() {
		return true
	}
	for _, t := range r.PerTier {
		if !t.IsEmpty() {
			return true
		}
	}
	return false
}

// parseStrategyTPSLAfterRules walks a strategy's tiered_tp_atr / tiered_tp_atr_live
// close ref and extracts the strategy-level default and per-tier sl_after rules.
// errs is non-nil when individual fields are malformed; callers may surface
// them at config-load time but the parser still returns whatever it could.
func parseStrategyTPSLAfterRules(sc StrategyConfig) (rules tierSLAfterRules, errs []string) {
	if !strategyUsesTieredTPATRClose(sc) {
		return rules, nil
	}
	var defaultRaw interface{}
	var tiersRaw interface{}
	for _, ref := range sc.CloseStrategies {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n != "tiered_tp_atr" && n != "tiered_tp_atr_live" {
			continue
		}
		if v, ok := ref.Params["sl_after"]; ok {
			defaultRaw = v
		}
		if v, ok := ref.Params["tiers"]; ok {
			tiersRaw = v
		}
		break
	}
	if defaultRaw != nil {
		r, err := parseSLAfterRule(defaultRaw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("sl_after (strategy-level): %v", err))
		} else if err := validateSLAfterRule(r); err != nil {
			errs = append(errs, fmt.Sprintf("sl_after (strategy-level): %v", err))
		} else {
			rules.Default = r
		}
	}
	items, ok := tiersRaw.([]interface{})
	if !ok {
		return rules, errs
	}
	type pair struct {
		multiple float64
		rule     SLAfterRule
	}
	pairs := make([]pair, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		mult, err := floatFromAnyChecked(firstPresent(m, "atr_multiple", "multiple"))
		if err != nil || mult <= 0 {
			continue
		}
		var r SLAfterRule
		if raw, ok := m["sl_after"]; ok && raw != nil {
			parsed, perr := parseSLAfterRule(raw)
			if perr != nil {
				errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, perr))
			} else if verr := validateSLAfterRule(parsed); verr != nil {
				errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, verr))
			} else {
				r = parsed
			}
		}
		pairs = append(pairs, pair{multiple: mult, rule: r})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].multiple < pairs[j].multiple })
	rules.PerTier = make([]SLAfterRule, len(pairs))
	for i, p := range pairs {
		rules.PerTier[i] = p.rule
	}
	return rules, errs
}

// validatePostTPStopLossRules returns config-load errors for a single
// strategy's sl_after configuration. Conditions enforced:
//   - shape/field-level errors (kind, missing atr_mult, …)
//   - reject when the strategy already uses a trailing stop (TrailingStopATRMult
//     or TrailingStopPct > 0) — trailing walks the SL continuously and
//     sl_after would race the walker
//   - reject when the strategy has no fixed SL to cancel+replace
//   - reject sl_after keys placed under non-tiered_tp_atr* close refs — the
//     runtime only honors them on tiered TP refs, so an operator who writes
//     sl_after under e.g. tp_at_pct would silently get no SL bumps. Better to
//     fail loud at load than to swallow the intent.
func validatePostTPStopLossRules(sc StrategyConfig) []string {
	rules, errs := parseStrategyTPSLAfterRules(sc)
	out := append([]string(nil), errs...)
	for _, ref := range sc.CloseStrategies {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n == "tiered_tp_atr" || n == "tiered_tp_atr_live" {
			continue
		}
		if _, ok := ref.Params["sl_after"]; ok {
			out = append(out, fmt.Sprintf("sl_after is only honored on tiered_tp_atr / tiered_tp_atr_live close refs; found on %q", ref.Name))
		}
		if tiersRaw, ok := ref.Params["tiers"]; ok {
			if items, ok := tiersRaw.([]interface{}); ok {
				for i, item := range items {
					if m, ok := item.(map[string]interface{}); ok {
						if _, ok := m["sl_after"]; ok {
							out = append(out, fmt.Sprintf("sl_after on tier[%d] of %q has no effect; only honored on tiered_tp_atr* close refs", i, ref.Name))
						}
					}
				}
			}
		}
	}
	if !rules.HasAny() {
		return out
	}
	if (sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0) ||
		(sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0) {
		out = append(out, "sl_after cannot be combined with trailing_stop_atr_mult or trailing_stop_pct — trailing already walks the SL continuously")
	}
	hasFixedSL := (sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0) ||
		(sc.StopLossPct != nil && *sc.StopLossPct > 0) ||
		(sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0)
	if !hasFixedSL {
		out = append(out, "sl_after requires a fixed stop-loss to adjust (set stop_loss_atr_mult, stop_loss_pct, or stop_loss_margin_pct)")
	}
	// trail_from_here drives the trailing-stop walker, which currently only
	// runs for perps strategies. Reject it on manual strategies in v1.
	if sc.Type == "manual" {
		if rules.Default.Kind == "trail_from_here" {
			out = append(out, "sl_after: trail_from_here is not supported on manual strategies (perps only in v1) — use breakeven or atr_mult instead")
		}
		for i, r := range rules.PerTier {
			if r.Kind == "trail_from_here" {
				out = append(out, fmt.Sprintf("sl_after (tier[%d]): trail_from_here is not supported on manual strategies (perps only in v1) — use breakeven or atr_mult instead", i))
			}
		}
	}
	return out
}

// SLAdjustmentAlert describes a post-TP SL bump for the owner DM (#708).
type SLAdjustmentAlert struct {
	StrategyID           string
	Symbol               string
	Side                 string  // "long" / "short"
	TierIdx              int     // 0-based tier whose fill triggered the bump
	OldTriggerPx         float64 // 0 = unknown
	NewTriggerPx         float64
	Mode                 string // human label: "breakeven", "atr+0.25", "trail 1.00×ATR"
	TransitionToTrailing bool
}

// formatSLAdjustmentAlert produces the DM body. Pure helper for testing.
func formatSLAdjustmentAlert(a SLAdjustmentAlert) string {
	headline := fmt.Sprintf("SL adjusted post-%s", tpTierLabel(a.TierIdx))
	if a.TransitionToTrailing {
		headline += " → trailing"
	}
	headline += fmt.Sprintf(" — %s", a.StrategyID)
	side := "LONG"
	if a.Side == "short" {
		side = "SHORT"
	}
	priceLine := fmt.Sprintf("%s %s", a.Symbol, side)
	var slLine string
	if a.OldTriggerPx > 0 {
		slLine = fmt.Sprintf("SL: $%.4f → $%.4f (%s)", a.OldTriggerPx, a.NewTriggerPx, a.Mode)
	} else {
		slLine = fmt.Sprintf("SL: $%.4f (%s)", a.NewTriggerPx, a.Mode)
	}
	return fmt.Sprintf("%s\n%s\n%s", headline, priceLine, slLine)
}

// notifySLAdjustment emits an owner DM for a post-TP SL bump. Gated on the
// same `notify_tp_sl_fills` toggle as the protection-fill alert; no-ops when
// sender is unavailable.
func notifySLAdjustment(sender ownerDMSender, enabled bool, alert SLAdjustmentAlert) {
	if !enabled || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatSLAdjustmentAlert(alert))
}

// findHighestClearedTier returns the index of the highest-numbered tier in
// tpOIDs that has been cleared (OID==0) at or above fromIdx. found=false when
// no cleared tier exists in that range.
func findHighestClearedTier(tpOIDs []int64, fromIdx int) (int, bool) {
	if fromIdx < 0 {
		fromIdx = 0
	}
	highest := -1
	for i := fromIdx; i < len(tpOIDs); i++ {
		if tpOIDs[i] == 0 {
			highest = i
		}
	}
	if highest >= 0 {
		return highest, true
	}
	return 0, false
}

// runPostTPStopLossAdjustment is the locking + plan + subprocess + apply
// pipeline for the #708 sl_after machinery. Called by the per-cycle perps /
// manual loops after runHyperliquidProtectionSync; idempotent via
// pos.SLAdjustedTiersProcessed.
//
// mark is the current price snapshot; required by the trail_from_here rule and
// ignored by breakeven / atr_offset. When mark is unavailable (0) and the
// resolved rule needs it, the function defers to the next cycle.
//
// Returns true when the SL OID was successfully cancel+replaced. false covers
// every short-circuit: no rules configured, no cleared tier above the
// watermark, missing inputs, subprocess failure.
func runPostTPStopLossAdjustment(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	cfg *Config,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) bool {
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		return false
	}
	if stratState == nil || symbol == "" {
		return false
	}
	rules, _ := parseStrategyTPSLAfterRules(sc)
	if !rules.HasAny() {
		return false
	}

	// Phase 1: RLock — snapshot the inputs needed for the subprocess call.
	mu.RLock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.InitialQuantity <= 0 {
		mu.RUnlock()
		return false
	}
	// Gate on a partial close having occurred — a fresh position with all
	// tiers at OID=0 simply hasn't been armed yet, and the watermark would
	// race the protection-sync's initial OID placement. The epsilon is for
	// float-roundoff; the gate is "any partial close occurred" (TP OR
	// close-eval), not "any TP fired" specifically — close-evals on the same
	// position can also satisfy it, which is fine because findHighestClearedTier
	// further narrows to tiers whose OID is actually 0.
	if pos.Quantity >= pos.InitialQuantity-1e-9 {
		mu.RUnlock()
		return false
	}
	clearedIdx, clearedOK := findHighestClearedTier(pos.TPOIDs, pos.SLAdjustedTiersProcessed)
	if !clearedOK {
		mu.RUnlock()
		return false
	}
	rule := rules.ForTier(clearedIdx)
	side := pos.Side
	avgCost := pos.AvgCost
	entryATR := pos.EntryATR
	qty := pos.Quantity
	currentOID := pos.StopLossOID
	mu.RUnlock()

	// If the matched tier has no rule, advance the watermark so we stop
	// re-evaluating it each cycle. No subprocess work.
	if rule.IsEmpty() {
		mu.Lock()
		if p, ok := stratState.Positions[symbol]; ok && p != nil && p.SLAdjustedTiersProcessed <= clearedIdx {
			p.SLAdjustedTiersProcessed = clearedIdx + 1
		}
		mu.Unlock()
		return false
	}

	// Defer when SL isn't armed yet — short-circuits before compute so we
	// don't burn cycles on a trail_from_here rule whose trigger we'd then
	// throw away.
	if currentOID == 0 {
		return false
	}
	triggerPx, mode, computeOK := computePostTPStopLossTrigger(rule, side, avgCost, entryATR, mark)
	if !computeOK {
		// trail_from_here without a mark (or other malformed input) — defer.
		// We do NOT advance the watermark; next cycle (with a price) retries.
		return false
	}

	// Phase 2: no-lock subprocess — cancel+replace SL OID. RunHyperliquidUpdateStopLoss
	// is the trailing-stop primitive but it just cancels an existing OID and
	// places a fresh reduce-only trigger, which is what every sl_after mode
	// needs (breakeven / atr_offset / trail_from_here all). Intentionally
	// reused for sc.Type=="manual" too — the validator blocks trail_from_here
	// there, so manual paths only cancel+replace a fixed SL OID.
	if logger != nil {
		logger.Info("post-TP SL adjustment for %s: tier %d cleared, mode=%s new_trigger=$%.4f (cancel oid=%d)",
			symbol, clearedIdx, mode, triggerPx, currentOID)
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, triggerPx, currentOID)
	if stderr != "" && logger != nil {
		logger.Info("post-TP SL stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("post-TP SL update failed: %v", err)
		}
		return false
	}
	if result == nil || result.Error != "" {
		if logger != nil && result != nil && result.Error != "" {
			logger.Error("post-TP SL update returned error: %s", result.Error)
		}
		return false
	}
	if result.CancelStopLossError != "" && logger != nil {
		logger.Warn("post-TP SL cancel failed (non-fatal): %s", result.CancelStopLossError)
		if result.StopLossOID > 0 && currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL POST-TP SL CANCEL FAILED** [%s] %s old trigger OID %d may still be resting while new trigger OID %d was placed. Check HL open triggers before they accumulate toward the account cap. Error: %s",
				sc.ID, symbol, currentOID, result.StopLossOID, result.CancelStopLossError)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected post-TP SL update for %s — position may be under-protected: %s",
					symbol, result.StopLossError)
			}
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s post-TP SL update rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else if logger != nil {
			logger.Warn("post-TP SL placement failed (non-fatal): %s", result.StopLossError)
		}
	}

	// Phase 3: Lock — apply the update.
	mu.Lock()
	p, ok := stratState.Positions[symbol]
	if !ok || p == nil || p.Quantity <= 0 || p.Side != side {
		mu.Unlock()
		return false
	}
	oldTrigger := p.StopLossTriggerPx
	if result.StopLossOID > 0 {
		p.StopLossOID = result.StopLossOID
	}
	if result.StopLossTriggerPx > 0 {
		p.StopLossTriggerPx = result.StopLossTriggerPx
	} else {
		p.StopLossTriggerPx = triggerPx
	}
	p.SLAdjustedTiersProcessed = clearedIdx + 1
	transitionedToTrailing := false
	if rule.Kind == "trail_from_here" && rule.TrailATRMult > 0 {
		mult := rule.TrailATRMult
		p.PostTPTrailingATRMult = &mult
		if mark > 0 {
			p.StopLossHighWaterPx = mark
		}
		transitionedToTrailing = true
	}
	newTrigger := p.StopLossTriggerPx
	if logger != nil {
		logger.Info("post-TP SL adjusted: oid=%d trigger=$%.4f→$%.4f (mode=%s tier=%d)",
			p.StopLossOID, oldTrigger, newTrigger, mode, clearedIdx)
	}
	mu.Unlock()

	// Owner DM (held outside the lock — Discord HTTP must not block RLocks).
	if cfg != nil {
		notifySLAdjustment(notifier, cfg.NotifyTPSLFillsEnabled(), SLAdjustmentAlert{
			StrategyID:           sc.ID,
			Symbol:               symbol,
			Side:                 side,
			TierIdx:              clearedIdx,
			OldTriggerPx:         oldTrigger,
			NewTriggerPx:         newTrigger,
			Mode:                 mode,
			TransitionToTrailing: transitionedToTrailing,
		})
	}
	return true
}
