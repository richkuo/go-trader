package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)


const hlLiquidationStopBufferPct = 0.5

func stopPastLiquidation(side string, triggerPx, liqPx float64) bool {
	if triggerPx <= 0 || liqPx <= 0 {
		return false
	}
	switch side {
	case "long":
		return triggerPx <= liqPx
	case "short":
		return triggerPx >= liqPx
	}
	return false
}

func clampStopInsideLiquidation(side string, triggerPx, liqPx float64) (float64, bool) {
	if !stopPastLiquidation(side, triggerPx, liqPx) {
		return triggerPx, false
	}
	var clamped float64
	switch side {
	case "long":
		clamped = liqPx * (1.0 + hlLiquidationStopBufferPct/100.0)
	case "short":
		clamped = liqPx * (1.0 - hlLiquidationStopBufferPct/100.0)
	default:
		return triggerPx, false
	}
	if clamped <= 0 || math.IsNaN(clamped) || math.IsInf(clamped, 0) {
		return triggerPx, false
	}
	return clamped, true
}

func hlLiquidationPxForSide(liqPxByCoin map[string]float64, netSideByCoin map[string]string, coin, side string) float64 {
	px := liqPxByCoin[coin]
	if px <= 0 {
		return 0
	}
	if netSideByCoin[coin] != side || (side != "long" && side != "short") {
		return 0
	}
	return px
}


func hlBankruptcyStopBoundPct(leverage float64) float64 {
	if leverage < 1 {
		leverage = 1
	}
	return 100.0 / leverage
}

func validateHLStopWithinBankruptcyBound(sc StrategyConfig) []string {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return nil
	}
	if !isLiveArgs(sc.Args) {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(sc.MarginMode), "cross") {
		return nil
	}
	lev := EffectiveExchangeLeverage(sc)
	bound := hlBankruptcyStopBoundPct(lev)
	if lev < 1 {
		lev = 1
	}
	var errs []string
	report := func(field string, pct float64) {
		if pct <= 0 || pct < bound {
			return
		}
		errs = append(errs, fmt.Sprintf("%s = %g%% is at or beyond the isolated-margin bankruptcy distance (100 / leverage = %g%% at leverage %g), so Hyperliquid would force-close the position before the stop could ever fill; lower the stop distance or lower the leverage", field, pct, bound, lev))
	}
	if !strategyUsesUnifiedRegimeClose(sc) {
		if sc.StopLossPct != nil {
			report("stop_loss_pct", *sc.StopLossPct)
		}
		if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
			report("derived stop-loss price %% (stop_loss_margin_pct / leverage)", *sc.StopLossMarginPct/lev)
		}
	}
	if hlStopLossResolvesFromMaxDrawdownFallback(sc) {
		fallback := sc.MaxDrawdownPct
		if fallback > MaxAutoStopLossPct {
			fallback = MaxAutoStopLossPct
		}
		report("max_drawdown_pct", fallback)
	}
	return errs
}

func hlStopLossResolvesFromMaxDrawdownFallback(sc StrategyConfig) bool {
	if strategyUsesUnifiedRegimeClose(sc) {
		return false
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return false
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return false
	}
	if sc.StopLossATRRegime.IsConfigured() {
		return false
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		return false
	}
	return sc.TrailingStopPct == nil &&
		sc.StopLossPct == nil &&
		sc.StopLossMarginPct == nil &&
		sc.MaxDrawdownPct > 0
}


type hlLiquidationAlertState struct {
	Notified       bool
	LastNotifiedAt time.Time
	LastAction hlLiquidationAlertAction
}

var hlLiquidationAlerts sync.Map

func hlLiquidationAlertKey(strategyID, symbol string) string {
	return strategyID + ":" + symbol
}

func hlLiquidationShouldNotify(prev hlLiquidationAlertState, action hlLiquidationAlertAction, now time.Time) (bool, hlLiquidationAlertState) {
	notify := false
	switch {
	case !prev.Notified:
		notify = true
	case action != prev.LastAction:
		notify = true
	case now.Sub(prev.LastNotifiedAt) >= effectiveAlertThrottleInterval():
		notify = true
	}
	if !notify {
		return false, prev
	}
	return true, hlLiquidationAlertState{
		Notified:       true,
		LastNotifiedAt: now,
		LastAction:     action,
	}
}

func hlLiquidationAlertDue(strategyID, symbol string, action hlLiquidationAlertAction, now time.Time) bool {
	key := hlLiquidationAlertKey(strategyID, symbol)
	var prev hlLiquidationAlertState
	if v, ok := hlLiquidationAlerts.Load(key); ok {
		if s, ok2 := v.(hlLiquidationAlertState); ok2 {
			prev = s
		}
	}
	send, next := hlLiquidationShouldNotify(prev, action, now)
	if send {
		hlLiquidationAlerts.Store(key, next)
	}
	return send
}

func clearHLLiquidationAlert(strategyID, symbol string) {
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey(strategyID, symbol))
}

func clearHLLiquidationAlertOnHLPerpsClose(s *StrategyState, symbol string) {
	if s == nil || s.Platform != "hyperliquid" {
		return
	}
	if s.Type != "perps" && s.Type != "manual" {
		return
	}
	clearHLLiquidationAlert(s.ID, symbol)
}

func clearHLPerpsPositionAlertThrottles(s *StrategyState, symbol string) {
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	clearHLLiquidationAlertOnHLPerpsClose(s, symbol)
}

type hlLiquidationAlertAction string

const (
	hlLiquidationActionClamped hlLiquidationAlertAction = "clamped"
	hlLiquidationActionReplaceDeferred hlLiquidationAlertAction = "replace deferred"
	hlLiquidationActionProtectionLost hlLiquidationAlertAction = "protection lost"
	hlLiquidationActionRearmed hlLiquidationAlertAction = "re-armed"
	hlLiquidationActionRearmFailed hlLiquidationAlertAction = "re-arm failed"
	hlLiquidationActionUnreconciled hlLiquidationAlertAction = "not reconciled"
	hlLiquidationActionExited hlLiquidationAlertAction = "exited"
	hlLiquidationActionFilledOnChain hlLiquidationAlertAction = "SL filled"
	hlLiquidationActionOutcomeUnknown hlLiquidationAlertAction = "outcome unknown"
	hlLiquidationActionPlacementUnknown hlLiquidationAlertAction = "placement unknown"
)

func hlLiquidationActionUnprotected(a hlLiquidationAlertAction) bool {
	return a == hlLiquidationActionProtectionLost || a == hlLiquidationActionRearmFailed
}

func hlLiquidationUnprotectedRecovery(sc StrategyConfig) string {
	if !scaleInLiveProtectionResizable(sc) {
		return "The scheduler re-arms it on the next cycle"
	}
	if sc.IntervalSeconds > 0 {
		return fmt.Sprintf("This strategy's own stop management re-arms it on its next due manage-only cycle (interval %s)", (time.Duration(sc.IntervalSeconds) * time.Second).String())
	}
	return "This strategy's own stop management re-arms it on its next due manage-only cycle"
}

func notifyHLStopPastLiquidation(sc StrategyConfig, symbol, side string, triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction, notifier *MultiNotifier, logger *StrategyLogger, now time.Time) {
	recovery := hlLiquidationUnprotectedRecovery(sc)
	_, detail, unprotected := hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx, action, recovery)
	due := hlLiquidationAlertDue(sc.ID, symbol, action, now)
	if logger != nil && (due || unprotected) {
		if unprotected {
			logger.Error("CRITICAL: %s (%s) has no exchange-side stop: %s", symbol, side, detail)
		} else {
			logger.Warn("SL past liquidation for %s (%s): %s", symbol, side, detail)
		}
	}
	if !due {
		return
	}
	if notifier != nil && notifier.HasBackends() {
		msg := hlLiquidationAlertFullMessage(sc, symbol, side, triggerPx, clampedPx, liqPx, action)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

func hlLiquidationAlertFullMessage(sc StrategyConfig, symbol, side string, triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction) string {
	headline, detail, _ := hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx, action, hlLiquidationUnprotectedRecovery(sc))
	msg := fmt.Sprintf("%s [%s] %s %s — %s.", headline, sc.ID, symbol, side, detail)
	if triggerPx > 0 && liqPx > 0 &&
		action != hlLiquidationActionRearmed &&
		action != hlLiquidationActionExited &&
		action != hlLiquidationActionFilledOnChain {
		msg += " A stop past liquidation can never fill: Hyperliquid force-closes first, at liquidation-engine pricing. Lower the leverage or tighten the stop distance so the configured geometry is reachable."
	}
	return msg
}

func hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction, recovery string) (headline, detail string, unprotected bool) {
	headline = "**HL STOP PAST LIQUIDATION**"
	geometryKnown := triggerPx > 0 && liqPx > 0
	preamble := "the position has NO exchange-side stop"
	if geometryKnown {
		preamble = fmt.Sprintf("configured trigger $%.4f is past the exchange liquidation price $%.4f", triggerPx, liqPx)
	}
	unprotected = hlLiquidationActionUnprotected(action)
	switch action {
	case hlLiquidationActionRearmed:
		headline = "**HL STOP RE-ARMED**"
		if liqPx > 0 {
			detail = fmt.Sprintf("the position had NO exchange-side stop; re-armed at $%.4f (liquidation $%.4f)", clampedPx, liqPx)
		} else {
			detail = fmt.Sprintf("the position had NO exchange-side stop; re-armed at $%.4f", clampedPx)
		}
	case hlLiquidationActionRearmFailed:
		headline = "**HL POSITION UNPROTECTED**"
		if geometryKnown {
			detail = fmt.Sprintf("%s — the clamped stop at $%.4f did NOT rest: the position has no exchange-side stop right now. %s.", preamble, clampedPx, recovery)
		} else {
			detail = fmt.Sprintf("the position has NO exchange-side stop and the re-arm at $%.4f did not rest; %s", clampedPx, strings.ToLower(recovery[:1])+recovery[1:])
		}
	case hlLiquidationActionClamped:
		detail = preamble + fmt.Sprintf(" — tightened to $%.4f (%.2f%% inside liquidation)", clampedPx, hlLiquidationStopBufferPct)
	case hlLiquidationActionReplaceDeferred:
		detail = preamble + fmt.Sprintf(" — could not re-place at $%.4f; the ORIGINAL stop is still resting and the scheduler will retry next cycle", clampedPx)
	case hlLiquidationActionProtectionLost:
		headline = "**HL POSITION UNPROTECTED**"
		detail = preamble + fmt.Sprintf(" — the old trigger was CANCELLED but the replacement at $%.4f did NOT rest: the position has no exchange-side stop right now. %s.", clampedPx, recovery)
	case hlLiquidationActionUnreconciled:
		if !geometryKnown {
			headline = "**HL POSITION UNPROTECTED**"
			unprotected = true
		}
		detail = preamble + " — the audit did NOT touch the order: this coin's recorded size does not match the on-chain snapshot, so moving a reduce-only trigger could close a peer strategy's real position. Reconcile the coin, then the audit heals it."
	case hlLiquidationActionExited:
		headline = "**HL STOP FILLED — POSITION FLAT**"
		detail = fmt.Sprintf("the replacement trigger at $%.4f FILLED at submit — the position exited immediately, at a price inside the old liquidation price. No stop is needed; nothing is unprotected.", clampedPx)
	case hlLiquidationActionFilledOnChain:
		headline = "**HL STOP ALREADY FILLED**"
		detail = "the ORIGINAL stop already fired on-chain before it could be replaced — nothing was cancelled and there is no order left to replace; the reconciler books the close."
	case hlLiquidationActionOutcomeUnknown:
		headline = "**HL STOP OUTCOME UNKNOWN**"
		detail = preamble + fmt.Sprintf(" — the old trigger was CANCELLED and the replacement at $%.4f returned an outcome that could NOT be read: it may be resting untracked. Recorded state is KEPT and no re-place is attempted; verify the order book on Hyperliquid.", clampedPx)
	case hlLiquidationActionPlacementUnknown:
		detail = fmt.Sprintf("a fresh stop placement at $%.4f returned an outcome that could NOT be read: it may be resting untracked. Nothing was cancelled and nothing will be re-placed automatically; verify the order book on Hyperliquid.", clampedPx)
	default:
		detail = preamble
	}
	return headline, detail, unprotected
}

func hlLiquidationArmClampAction(result *HyperliquidStopLossUpdateResult, armOK bool) hlLiquidationAlertAction {
	if armOK && result != nil {
		if result.StopLossOID > 0 {
			return hlLiquidationActionClamped
		}
		if result.StopLossFilledImmediately && result.StopLossTriggerPx > 0 {
			return hlLiquidationActionExited
		}
		if result.StopLossOutcomeUnknown {
			return hlLiquidationActionPlacementUnknown
		}
	}
	return hlLiquidationActionRearmFailed
}

const hlTriggerRoundingTolerancePct = 0.05

func hlTriggerStrictlyTighter(side string, candidate, resting float64) bool {
	if candidate <= 0 || resting <= 0 {
		return false
	}
	slack := resting * (hlTriggerRoundingTolerancePct / 100.0)
	switch side {
	case "long":
		return candidate > resting+slack
	case "short":
		return candidate < resting-slack
	}
	return false
}

func hlProtectionSLTriggerPx(side string, anchor, entryATR, slMult float64) float64 {
	return atrStopLossTriggerPx(side, anchor, entryATR, slMult)
}

func hlClampProtectionSLMult(side string, anchor, entryATR, slMult, liqPx float64) (float64, bool) {
	if liqPx <= 0 || slMult <= 0 || entryATR <= 0 || anchor <= 0 {
		return slMult, false
	}
	if side != "long" && side != "short" {
		return slMult, false
	}
	wouldBe := hlProtectionSLTriggerPx(side, anchor, entryATR, slMult)
	clamped, ok := clampStopInsideLiquidation(side, wouldBe, liqPx)
	if !ok {
		if side == "long" && wouldBe <= 0 {
			clamped = liqPx * (1.0 + hlLiquidationStopBufferPct/100.0)
		} else {
			return slMult, false
		}
	}
	var delta float64
	switch side {
	case "long":
		delta = anchor - clamped
	case "short":
		delta = clamped - anchor
	}
	newMult := delta / entryATR
	if newMult <= 0 || math.IsNaN(newMult) || math.IsInf(newMult, 0) {
		return slMult, false
	}
	if newMult > slMult {
		return slMult, false
	}
	return newMult, true
}

func hlProtectionSLTriggerReachable(side string, anchor, entryATR, slMult, liqPx float64) bool {
	if slMult <= 0 || entryATR <= 0 || anchor <= 0 {
		return false
	}
	if liqPx <= 0 {
		return true
	}
	wouldBe := hlProtectionSLTriggerPx(side, anchor, entryATR, slMult)
	if wouldBe <= 0 {
		return false
	}
	return !stopPastLiquidation(side, wouldBe, liqPx)
}


type hlLiquidationAuditCandidate struct {
	StrategyID string
	Script     string
	Symbol     string
	Side       string
	Qty               float64
	VirtualQty        float64
	QtyCapped         bool
	StopLossOID       int64
	StopLossTriggerPx float64
	LiquidationPx     float64
	StaticScalarOwner bool
	RearmTriggerPx float64
	Unprotected bool
	UnresolvedPlacement bool
	BookConsistent bool
}

type hlLiquidationAuditActionKind int

const (
	hlAuditTighten hlLiquidationAuditActionKind = iota
	hlAuditRearm
	hlAuditRefuse
	hlAuditReport
)

type hlLiquidationAuditAction struct {
	Candidate hlLiquidationAuditCandidate
	Kind      hlLiquidationAuditActionKind
	ClampedTriggerPx float64
}

func planHyperliquidLiquidationAudit(candidates []hlLiquidationAuditCandidate) []hlLiquidationAuditAction {
	var actions []hlLiquidationAuditAction
	for _, c := range candidates {
		if c.Qty <= 0 {
			continue
		}
		switch {
		case c.UnresolvedPlacement:
			actions = append(actions, hlLiquidationAuditAction{
				Candidate: c, Kind: hlAuditReport, ClampedTriggerPx: c.StopLossTriggerPx,
			})
		case c.Unprotected:
			if !c.StaticScalarOwner || c.RearmTriggerPx <= 0 {
				continue
			}
			kind := hlAuditRearm
			if !c.BookConsistent {
				kind = hlAuditRefuse
			}
			actions = append(actions, hlLiquidationAuditAction{
				Candidate: c, Kind: kind, ClampedTriggerPx: c.RearmTriggerPx,
			})
		default:
			if c.StopLossTriggerPx <= 0 || c.LiquidationPx <= 0 {
				continue
			}
			clamped, ok := clampStopInsideLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx)
			if !ok {
				continue
			}
			kind := hlAuditTighten
			if !c.BookConsistent {
				kind = hlAuditRefuse
			}
			actions = append(actions, hlLiquidationAuditAction{
				Candidate: c, Kind: kind, ClampedTriggerPx: clamped,
			})
		}
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Candidate.StrategyID != actions[j].Candidate.StrategyID {
			return actions[i].Candidate.StrategyID < actions[j].Candidate.StrategyID
		}
		return actions[i].Candidate.Symbol < actions[j].Candidate.Symbol
	})
	return actions
}

func hlLiquidationScalarRearmTriggerPx(sc StrategyConfig, side string, anchor, liqPx float64) float64 {
	if anchor <= 0 {
		return 0
	}
	pct := EffectiveStopLossPct(sc)
	if pct <= 0 {
		return 0
	}
	var trigger float64
	switch side {
	case "long":
		trigger = anchor * (1.0 - pct/100.0)
	case "short":
		trigger = anchor * (1.0 + pct/100.0)
	default:
		return 0
	}
	if trigger <= 0 {
		return 0
	}
	if clamped, ok := clampStopInsideLiquidation(side, trigger, liqPx); ok {
		return clamped
	}
	return trigger
}

func hlLiquidationCoinBookConsistent(netVirtualByCoin map[string]float64, ownersByCoin map[string]int, onChainAbsQty map[string]float64) map[string]bool {
	out := make(map[string]bool, len(netVirtualByCoin))
	for coin, netVirtual := range netVirtualByCoin {
		onChain, ok := onChainAbsQty[coin]
		if !ok || onChain <= 1e-9 {
			out[coin] = false
			continue
		}
		out[coin] = math.Abs(netVirtual) <= onChain+1e-9 || ownersByCoin[coin] <= 1
	}
	return out
}

func collectHLLiquidationAuditCandidates(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	hlNetSideByCoin map[string]string,
	hlOnChainAbsQty map[string]float64,
	mu *sync.RWMutex,
) []hlLiquidationAuditCandidate {
	if state == nil {
		return nil
	}
	var out []hlLiquidationAuditCandidate
	virtualByCoin := make(map[string]float64)
	ownersByCoin := make(map[string]int)
	mu.RLock()
	for _, sc := range strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Type != "perps" && sc.Type != "manual" {
			continue
		}
		if !hyperliquidIsLive(sc.Args) {
			continue
		}
		ss, ok := state.Strategies[sc.ID]
		if !ok || ss == nil {
			continue
		}
		for symbol, pos := range ss.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			if pos.Side != "long" && pos.Side != "short" {
				continue
			}
			if pos.HedgeFor != "" {
				continue
			}
			if pos.Side == "short" {
				virtualByCoin[symbol] -= pos.Quantity
			} else {
				virtualByCoin[symbol] += pos.Quantity
			}
			ownersByCoin[symbol]++

			staticScalar := !scaleInLiveProtectionResizable(sc)
			armed := pos.StopLossTriggerPx > 0
			unprotected := pos.StopLossOID == 0 && pos.StopLossTriggerPx <= 0
			unresolvedPlacement := pos.StopLossOID == 0 && armed
			if !armed && !unprotected {
				continue
			}
			if unprotected && !staticScalar {
				continue
			}
			if unresolvedPlacement {
				slQty, capped := hlSLEffectiveQty(symbol, pos.Quantity, hlOnChainAbsQty)
				out = append(out, hlLiquidationAuditCandidate{
					StrategyID:          sc.ID,
					Script:              sc.Script,
					Symbol:              symbol,
					Side:                pos.Side,
					Qty:                 slQty,
					VirtualQty:          pos.Quantity,
					QtyCapped:           capped,
					StopLossOID:         0,
					StopLossTriggerPx:   pos.StopLossTriggerPx,
					StaticScalarOwner:   staticScalar,
					UnresolvedPlacement: true,
				})
				continue
			}
			liqPx := hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, symbol, pos.Side)
			if armed && liqPx <= 0 {
				continue
			}
			sideConfirmed := hlNetSideByCoin[symbol] == pos.Side
			if unprotected && !sideConfirmed {
				continue
			}
			slQty, capped := hlSLEffectiveQty(symbol, pos.Quantity, hlOnChainAbsQty)
			rearmPx := 0.0
			if unprotected {
				rearmPx = hlLiquidationScalarRearmTriggerPx(sc, pos.Side, pos.riskAnchorPrice(), liqPx)
			}
			out = append(out, hlLiquidationAuditCandidate{
				StrategyID:        sc.ID,
				Script:            sc.Script,
				Symbol:            symbol,
				Side:              pos.Side,
				Qty:               slQty,
				VirtualQty:        pos.Quantity,
				QtyCapped:         capped,
				StopLossOID:       pos.StopLossOID,
				StopLossTriggerPx: pos.StopLossTriggerPx,
				LiquidationPx:     liqPx,
				StaticScalarOwner: staticScalar,
				RearmTriggerPx:    rearmPx,
				Unprotected:       unprotected,
			})
		}
	}
	mu.RUnlock()

	consistent := hlLiquidationCoinBookConsistent(virtualByCoin, ownersByCoin, hlOnChainAbsQty)
	for i := range out {
		out[i].BookConsistent = consistent[out[i].Symbol]
	}
	return out
}

type hlLiquidationReplaceOutcome int

const (
	hlReplaceDeferred hlLiquidationReplaceOutcome = iota
	hlReplacePlaced
	hlReplaceFilled
	hlReplaceFilledExternally
	hlReplaceProtectionLost
	hlReplaceOutcomeUnknown
)

func hlLiquidationClampReplace(candidate hlLiquidationAuditCandidate, clampedTriggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if clampedTriggerPx <= 0 || candidate.Qty <= 0 {
		return nil, hlReplaceDeferred
	}
	unlock := lockHyperliquidTrailingUpdate(candidate.Symbol)
	defer unlock()

	result, stderr, err := runHyperliquidUpdateStopLossFunc(candidate.Script, candidate.Symbol, candidate.Side, candidate.Qty, clampedTriggerPx, candidate.StopLossOID)
	if stderr != "" && logger != nil {
		logger.Info("liquidation-clamp SL replace stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace failed for %s: %v", candidate.Symbol, err)
		}
		return result, hlReplaceDeferred
	}
	if result == nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned no result for %s", candidate.Symbol)
		}
		return nil, hlReplaceDeferred
	}
	if result.Error != "" {
		if result.CancelStopLossSucceeded {
			if result.StopLossOutcomeUnknown {
				if logger != nil {
					logger.Error("CRITICAL: liquidation-clamp cancelled SL OID=%d for %s and the replacement's outcome could NOT be read — it may be resting untracked: %s", candidate.StopLossOID, candidate.Symbol, result.Error)
				}
				return result, hlReplaceOutcomeUnknown
			}
			if logger != nil {
				logger.Error("CRITICAL: liquidation-clamp cancelled SL OID=%d for %s and the subprocess failed before placing — the position has NO exchange-side stop: %s", candidate.StopLossOID, candidate.Symbol, result.Error)
			}
			return result, hlReplaceProtectionLost
		}
		if result.StopLossOutcomeUnknown {
			if logger != nil {
				logger.Error("CRITICAL: liquidation-clamp placement for %s returned an error and its outcome could NOT be read — it may be resting untracked: %s", candidate.Symbol, result.Error)
			}
			return result, hlReplaceOutcomeUnknown
		}
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned error for %s: %s", candidate.Symbol, result.Error)
		}
		return result, hlReplaceDeferred
	}
	if result.OpenOrderCheckError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL replace deferred for %s (open-order lookup failed): %s", candidate.Symbol, result.OpenOrderCheckError)
		}
		return result, hlReplaceDeferred
	}
	if result.StopLossFilledExternally {
		if logger != nil {
			logger.Warn("Liquidation-clamp: SL OID=%d for %s already filled on-chain — reconciler will book the close", candidate.StopLossOID, candidate.Symbol)
		}
		return result, hlReplaceFilledExternally
	}
	if result.CancelStopLossError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL cancel failed for %s; original stop still resting: %s", candidate.Symbol, result.CancelStopLossError)
		}
		return result, hlReplaceDeferred
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected the liquidation-clamp SL replace for %s: %s", candidate.Symbol, result.StopLossError)
			}
		} else if logger != nil {
			logger.Warn("Liquidation-clamp SL placement failed (non-fatal) for %s: %s", candidate.Symbol, result.StopLossError)
		}
	}
	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		if logger != nil {
			logger.Warn("Liquidation-clamp SL filled at submit for %s — position exited inside the liquidation price", candidate.Symbol)
		}
		return result, hlReplaceFilled
	case result.StopLossOID > 0:
		return result, hlReplacePlaced
	case result.StopLossOutcomeUnknown:
		if logger != nil {
			logger.Error("CRITICAL: liquidation-clamp SL for %s (old OID=%d) could not read the placement's outcome — it may be resting untracked; recorded state kept", candidate.Symbol, candidate.StopLossOID)
		}
		return result, hlReplaceOutcomeUnknown
	case result.CancelStopLossSucceeded:
		if logger != nil {
			logger.Error("CRITICAL: liquidation-clamp cancelled SL OID=%d for %s but the replacement did not rest — the position has NO exchange-side stop", candidate.StopLossOID, candidate.Symbol)
		}
		return result, hlReplaceProtectionLost
	}
	return result, hlReplaceDeferred
}

func hlLiquidationMayRetryReplace(result *HyperliquidStopLossUpdateResult) bool {
	return result != nil && !result.StopLossOutcomeUnknown
}

func hlLiquidationRetryPlace(candidate hlLiquidationAuditCandidate, clampedTriggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if clampedTriggerPx <= 0 || candidate.Qty <= 0 {
		return nil, hlReplaceDeferred
	}
	unlock := lockHyperliquidTrailingUpdate(candidate.Symbol)
	defer unlock()
	return hlLiquidationPlaceFresh(candidate.Script, candidate.Symbol, candidate.Side, candidate.Qty, clampedTriggerPx, logger)
}

func hlLiquidationPlaceFresh(script, symbol, side string, qty, triggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if triggerPx <= 0 || qty <= 0 {
		return nil, hlReplaceDeferred
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(script, symbol, side, qty, triggerPx, 0)
	if stderr != "" && logger != nil {
		logger.Info("liquidation-clamp SL retry stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL retry failed for %s: %v", symbol, err)
		}
		return result, hlReplaceDeferred
	}
	if result == nil {
		return nil, hlReplaceDeferred
	}
	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		return result, hlReplaceFilled
	case result.StopLossOID > 0:
		if logger != nil {
			logger.Warn("Liquidation-clamp SL retry rested for %s at $%.4f after the first placement was rejected", symbol, result.StopLossTriggerPx)
		}
		return result, hlReplacePlaced
	case result.StopLossOutcomeUnknown:
		if logger != nil {
			logger.Error("CRITICAL: liquidation-clamp SL retry's outcome could NOT be read for %s — it may be resting untracked", symbol)
		}
		return result, hlReplaceOutcomeUnknown
	case result.Error != "", result.OpenOrderCheckError != "", result.StopLossError != "":
		if logger != nil && result.StopLossError != "" {
			logger.Error("CRITICAL: liquidation-clamp SL retry did not rest for %s: %s — the position has NO exchange-side stop", symbol, result.StopLossError)
		}
		return result, hlReplaceDeferred
	}
	return result, hlReplaceDeferred
}

type hlLiquidationPendingAlert struct {
	sc        StrategyConfig
	symbol    string
	side      string
	triggerPx float64
	clampedPx float64
	liqPx     float64
	action    hlLiquidationAlertAction
	logger    *StrategyLogger
}

type hlLiquidationAuditResult struct {
	ImmediateFills int
	CloseDetails []hlLiquidationCloseDetail
	StateMutations int
}

type hlLiquidationCloseDetail struct {
	SC     StrategyConfig
	Symbol string
	FillPx float64
	Detail string
}

func sendAuditCloseAlerts(details []hlLiquidationCloseDetail, state map[string]*StrategyState, mu *sync.RWMutex, notifier *MultiNotifier) {
	counts := make(map[string]int, len(details))
	scByID := make(map[string]StrategyConfig, len(details))
	order := make([]string, 0, len(details))
	for _, cd := range details {
		if _, seen := counts[cd.SC.ID]; !seen {
			order = append(order, cd.SC.ID)
			scByID[cd.SC.ID] = cd.SC
		}
		counts[cd.SC.ID]++
	}
	for _, id := range order {
		sendTradeAlerts(scByID[id], state[id], counts[id], mu, notifier)
	}
}

func applyAuditStopUpdate(res *hlLiquidationAuditResult, ss *StrategyState, symbol, side string, prevSLOID int64, placedQty float64, result *HyperliquidStopLossUpdateResult, logger *StrategyLogger) (bool, float64) {
	var beforeOID int64
	var beforeTriggerPx float64
	var beforeNormalizePending bool
	if ss != nil {
		if pos, ok := ss.Positions[symbol]; ok && pos != nil {
			beforeOID = pos.StopLossOID
			beforeTriggerPx = pos.StopLossTriggerPx
			beforeNormalizePending = pos.RatchetFallbackNormalizePending
		}
	}
	immediateFill, fillPx := applyTrailingStopUpdateResult(ss, symbol, side, prevSLOID, 0, true, result, "liquidation_clamp_sl_immediate", logger, placedQty)
	if immediateFill {
		res.StateMutations++
		return true, fillPx
	}
	if ss != nil {
		if pos, ok := ss.Positions[symbol]; ok && pos != nil {
			if pos.StopLossOID != beforeOID || pos.StopLossTriggerPx != beforeTriggerPx || pos.RatchetFallbackNormalizePending != beforeNormalizePending {
				res.StateMutations++
			}
		}
	}
	return false, fillPx
}

func runHyperliquidLiquidationAudit(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	hlNetSideByCoin map[string]string,
	hlOnChainAbsQty map[string]float64,
	snapshotFetched bool,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	now time.Time,
) hlLiquidationAuditResult {
	var res hlLiquidationAuditResult
	if !snapshotFetched {
		return res
	}
	candidates := collectHLLiquidationAuditCandidates(strategies, state, hlLiquidationPx, hlNetSideByCoin, hlOnChainAbsQty, mu)
	for _, c := range candidates {
		if !c.Unprotected && !c.UnresolvedPlacement && !stopPastLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx) {
			clearHLLiquidationAlert(c.StrategyID, c.Symbol)
		}
	}
	actions := planHyperliquidLiquidationAudit(candidates)
	if len(actions) == 0 {
		return res
	}
	for _, c := range candidates {
		if c.QtyCapped {
			(&StrategyLogger{stratID: c.StrategyID, writer: os.Stderr}).Warn("Liquidation-clamp SL re-arm: %s capped by on-chain size (virtual %.6f > on-chain %.6f)", c.Symbol, c.VirtualQty, c.Qty)
		}
	}
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	var pending []hlLiquidationPendingAlert
	for _, act := range actions {
		c := act.Candidate
		sc, ok := byID[c.StrategyID]
		if !ok {
			continue
		}
		logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
		if act.Kind == hlAuditRefuse {
			logger.Error("CRITICAL: #1450 audit refused to touch %s — recorded size across live strategies exceeds the on-chain snapshot", c.Symbol)
			pending = append(pending, hlLiquidationPendingAlert{
				sc: sc, symbol: c.Symbol, side: c.Side,
				triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
				action: hlLiquidationActionUnreconciled, logger: logger,
			})
			continue
		}
		if act.Kind == hlAuditReport {
			pending = append(pending, hlLiquidationPendingAlert{
				sc: sc, symbol: c.Symbol, side: c.Side,
				triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
				action: hlLiquidationActionPlacementUnknown, logger: logger,
			})
			continue
		}
		result, outcome := hlLiquidationClampReplace(c, act.ClampedTriggerPx, logger)
		action := hlLiquidationActionClamped
		if act.Kind == hlAuditRearm {
			action = hlLiquidationActionRearmed
		}
		switch outcome {
		case hlReplaceDeferred:
			action = hlLiquidationActionReplaceDeferred
			if act.Kind == hlAuditRearm {
				action = hlLiquidationActionRearmFailed
			}
		case hlReplaceFilledExternally:
			action = hlLiquidationActionFilledOnChain
		case hlReplaceOutcomeUnknown:
			action = hlLiquidationActionOutcomeUnknown
			if c.StopLossOID == 0 {
				action = hlLiquidationActionPlacementUnknown
			}
			mu.Lock()
			if immediateFill, fillPx := applyAuditStopUpdate(&res, state.Strategies[c.StrategyID], c.Symbol, c.Side, c.StopLossOID, c.Qty, result, logger); immediateFill {
				res.ImmediateFills++
				action = hlLiquidationActionExited
				logger.Warn("Liquidation-clamp SL booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
				res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
					SC: sc, Symbol: c.Symbol, FillPx: fillPx,
					Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
				})
			}
			mu.Unlock()
		case hlReplaceProtectionLost:
			action = hlLiquidationActionProtectionLost
			clearDeadState := true
			if hlLiquidationMayRetryReplace(result) {
				retryResult, retryOutcome := hlLiquidationRetryPlace(c, act.ClampedTriggerPx, logger)
				switch retryOutcome {
				case hlReplacePlaced, hlReplaceFilled:
					if retryOutcome == hlReplaceFilled {
						action = hlLiquidationActionExited
					} else {
						action = hlLiquidationActionClamped
					}
					clearDeadState = false
					mu.Lock()
					immediateFill, fillPx := applyAuditStopUpdate(&res, state.Strategies[c.StrategyID], c.Symbol, c.Side, c.StopLossOID, c.Qty, retryResult, logger)
					mu.Unlock()
					if immediateFill {
						res.ImmediateFills++
						logger.Warn("Liquidation-clamp SL retry booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
						res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
							SC: sc, Symbol: c.Symbol, FillPx: fillPx,
							Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
						})
					}
				case hlReplaceOutcomeUnknown:
					action = hlLiquidationActionOutcomeUnknown
					clearDeadState = false
				default:
				}
			}
			if clearDeadState {
				mu.Lock()
				if immediateFill, fillPx := applyAuditStopUpdate(&res, state.Strategies[c.StrategyID], c.Symbol, c.Side, c.StopLossOID, c.Qty, result, logger); immediateFill {
					res.ImmediateFills++
					logger.Warn("Liquidation-clamp SL booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
					res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
						SC: sc, Symbol: c.Symbol, FillPx: fillPx,
						Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
					})
				}
				mu.Unlock()
			}
		case hlReplacePlaced, hlReplaceFilled:
			if outcome == hlReplaceFilled {
				action = hlLiquidationActionExited
			}
			mu.Lock()
			ss := state.Strategies[c.StrategyID]
			immediateFill, fillPx := applyAuditStopUpdate(&res, ss, c.Symbol, c.Side, c.StopLossOID, c.Qty, result, logger)
			mu.Unlock()
			if immediateFill {
				res.ImmediateFills++
				logger.Warn("Liquidation-clamp SL booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
				res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
					SC: sc, Symbol: c.Symbol, FillPx: fillPx,
					Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
				})
			}
		}
		pending = append(pending, hlLiquidationPendingAlert{
			sc: sc, symbol: c.Symbol, side: c.Side,
			triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
			action: action, logger: logger,
		})
	}

	for _, p := range pending {
		notifyHLStopPastLiquidation(p.sc, p.symbol, p.side, p.triggerPx, p.clampedPx, p.liqPx, p.action, notifier, p.logger, now)
	}
	return res
}

func buildHLLiquidationMaps(hlPositions []HLPosition) (onChainAbsQty map[string]float64, liquidationPx map[string]float64, netSideByCoin map[string]string) {
	onChainAbsQty = make(map[string]float64, len(hlPositions))
	liquidationPx = make(map[string]float64, len(hlPositions))
	netSideByCoin = make(map[string]string, len(hlPositions))
	for _, p := range hlPositions {
		sz := p.Size
		if sz < 0 {
			sz = -sz
		}
		if sz > 1e-9 {
			onChainAbsQty[p.Coin] = sz
			if p.Size < 0 {
				netSideByCoin[p.Coin] = "short"
			} else {
				netSideByCoin[p.Coin] = "long"
			}
			if p.LiquidationPx > 0 {
				liquidationPx[p.Coin] = p.LiquidationPx
			}
		}
	}
	return onChainAbsQty, liquidationPx, netSideByCoin
}

const liquidationAuditMinIntervalSeconds = 60

func liquidationAuditIntervalSeconds(strategies []StrategyConfig, intervals map[string]int) int {
	best := 0
	for _, sc := range strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Type != "perps" && sc.Type != "manual" {
			continue
		}
		if !hyperliquidIsLive(sc.Args) {
			continue
		}
		if iv := intervals[sc.ID]; iv > 0 && (best == 0 || iv < best) {
			best = iv
		}
	}
	if best == 0 {
		return 0
	}
	if half := best / 2; half > liquidationAuditMinIntervalSeconds {
		return half
	}
	return liquidationAuditMinIntervalSeconds
}

func flushOffCycleLiquidationAuditState(state *AppState, cfg *Config, stateDB *StateDB, mu *sync.RWMutex, mutations int, dirty bool, saveFailures int, force bool) (bool, int) {
	if mutations <= 0 && !dirty && !force {
		return dirty, saveFailures
	}
	mu.Lock()
	err := SaveStateWithDB(state, cfg, stateDB)
	mu.Unlock()
	if err != nil {
		saveFailures++
		fmt.Printf("[CRITICAL] Save state failed after off-cycle liquidation audit (%d/3): %v\n", saveFailures, err)
		return true, saveFailures
	}
	return false, 0
}

func runOffCycleLiquidationAudit(strategies []StrategyConfig, state *AppState, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) int {
	hlAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if hlAddr == "" {
		return 0
	}
	_, hlPositions, err := fetchHyperliquidState(hlAddr)
	if err != nil {
		fmt.Printf("[WARN] #1450 off-cycle liquidation audit: clearinghouseState fetch failed: %v\n", err)
		return 0
	}
	hlOnChainAbsQty, hlLiquidationPx, hlNetSideByCoin := buildHLLiquidationMaps(hlPositions)
	auditRes := runHyperliquidLiquidationAudit(strategies, state, hlLiquidationPx, hlNetSideByCoin, hlOnChainAbsQty, true, mu, notifier, time.Now().UTC())
	if auditRes.ImmediateFills == 0 {
		return auditRes.StateMutations
	}
	fmt.Printf("[WARN] #1450 liquidation audit: %d position(s) exited on a clamped stop this cycle\n", auditRes.ImmediateFills)
	priceCoins := make(map[string]bool)
	for _, cd := range auditRes.CloseDetails {
		if chKey := notifier.resolveChannelKey(cd.SC.Platform, cd.SC.Type); chKey != "" {
			notifier.SendToChannel(cd.SC.Platform, cd.SC.Type, fmt.Sprintf("**#1450 off-cycle liquidation audit**\n%s", cd.Detail))
		}
		priceCoins[cd.Symbol] = true
		if HedgeEnabled(cd.SC) && hedgeCoin(cd.SC) != "" {
			priceCoins[hedgeCoin(cd.SC)] = true
		}
	}
	sendAuditCloseAlerts(auditRes.CloseDetails, state.Strategies, mu, notifier)
	prices := make(map[string]float64)
	coins := make([]string, 0, len(priceCoins))
	for c := range priceCoins {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	if marks, err := fetchHyperliquidMids(coins); err != nil {
		fmt.Printf("[WARN] #1450 off-cycle liquidation audit: mark fetch failed for %v — hedge sync will use fallback pricing\n", coins)
	} else {
		prices = marks
	}
	if n := convergeHedgesAfterAuditClose(auditRes.CloseDetails, state.Strategies, mu, prices, notifier, logMgr.GetStrategyLogger); n > 0 {
		fmt.Printf("[WARN] #1450 liquidation audit: converged %d hedge leg(s) post-close\n", n)
	}
	return auditRes.StateMutations
}
