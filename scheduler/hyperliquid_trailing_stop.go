package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultTrailingStopMinMovePct = 0.5

var runHyperliquidUpdateStopLossFunc = RunHyperliquidUpdateStopLoss

var (
	hlTrailingUpdateLocksMu sync.Mutex
	hlTrailingUpdateLocks   = make(map[string]*sync.Mutex)
)

// hlUnreadableStopPlace blocks another place for an OID whose last replacement
// could not be read and whose old stop was left resting.
var hlUnreadableStopPlace sync.Map

func hlStopPlaceUnreadKey(symbol string, oid int64) string {
	return symbol + "|" + strconv.FormatInt(oid, 10)
}

func hlStopPlaceUnread(symbol string, oid int64) bool {
	if oid <= 0 {
		return false
	}
	_, ok := hlUnreadableStopPlace.Load(hlStopPlaceUnreadKey(symbol, oid))
	return ok
}

func hlRememberUnreadableStop(symbol string, oid int64, before []int64) {
	if oid <= 0 {
		return
	}
	key := hlStopPlaceUnreadKey(symbol, oid)
	if len(before) == 0 {
		if _, exists := hlUnreadableStopPlace.Load(key); exists {
			return
		}
	}
	hlUnreadableStopPlace.Store(key, append([]int64(nil), before...))
}

func hlListedStopMatches(order hlListedOpenOrder, side string, qty, trigger float64) bool {
	if !order.ReduceOnly || !order.IsTrigger || order.TriggerPx <= 0 || order.Sz <= 0 || qty <= 0 || trigger <= 0 {
		return false
	}
	kind := strings.ToLower(order.OrderType)
	if !strings.Contains(kind, "stop") || strings.Contains(kind, "take") {
		return false
	}
	wantSide := "A"
	if side == "short" {
		wantSide = "B"
	}
	if order.Side != wantSide {
		return false
	}
	if math.Abs(order.Sz-qty) > 1e-6 && math.Abs(order.Sz-qty)/qty > 1e-4 {
		return false
	}
	if math.Abs(order.TriggerPx-trigger) > 0.01 && math.Abs(order.TriggerPx-trigger)/trigger > 1e-3 {
		return false
	}
	return true
}

// hlReleaseUnreadableStop reads the open orders once. A failed read keeps the
// hold and places nothing. A readable book clears the hold. The only order that
// can be adopted is one new reduce-only stop for this side, size and trigger.
func hlReleaseUnreadableStop(script, symbol, side string, oid int64, qty, trigger float64) (released bool, adopted *HyperliquidStopLossUpdateResult, alert string) {
	raw, ok := hlUnreadableStopPlace.Load(hlStopPlaceUnreadKey(symbol, oid))
	if !ok {
		return true, nil, ""
	}
	before, _ := raw.([]int64)
	orders, readErr, err := runHyperliquidListOpenOrderOIDsFunc(script, symbol)
	if err != nil || readErr != "" {
		return false, nil, ""
	}
	key := hlStopPlaceUnreadKey(symbol, oid)
	seen := map[int64]struct{}{oid: {}}
	for _, id := range before {
		seen[id] = struct{}{}
	}
	var matches []hlListedOpenOrder
	fresh := 0
	for _, order := range orders {
		if _, had := seen[order.OID]; had {
			continue
		}
		fresh++
		if hlListedStopMatches(order, side, qty, trigger) {
			matches = append(matches, order)
		}
	}
	hlUnreadableStopPlace.Delete(key)
	if len(matches) == 1 {
		return true, &HyperliquidStopLossUpdateResult{
			StopLossOID:         matches[0].OID,
			StopLossTriggerPx:   matches[0].TriggerPx,
			CancelStopLossError: fmt.Sprintf("old stop OID %d still open", oid),
		}, ""
	}
	if fresh > 0 {
		return true, nil, fmt.Sprintf("**HL STOP OUTCOME UNKNOWN** %s: the unreadable replacement for OID %d could not be matched to one reduce-only stop. The hold ended and the old stop stays until the next replace.", symbol, oid)
	}
	return true, nil, ""
}

var hlStopReplaceAlertOnce sync.Map

func hlStopReplaceNotifyOnce(key string, notifier *MultiNotifier, msg string) {
	if _, loaded := hlStopReplaceAlertOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func hyperliquidProtectionPositionSnapshot(pos *Position) *Position {
	if pos == nil {
		return nil
	}
	snap := &Position{
		AvgCost:                         pos.AvgCost,
		EntryATR:                        pos.EntryATR,
		RiskAnchorPrice:                 pos.RiskAnchorPrice,
		Regime:                          pos.Regime,
		RegimeWindows:                   cloneStringMap(pos.RegimeWindows),
		RegimeAppliedLabel:              pos.RegimeAppliedLabel,
		RegimePendingLabel:              pos.RegimePendingLabel,
		RegimePendingCount:              pos.RegimePendingCount,
		SLAdjustedTiersProcessed:        pos.SLAdjustedTiersProcessed,
		RatchetFallbackNormalizePending: pos.RatchetFallbackNormalizePending,
	}
	if pos.PostTPTrailingATRMult != nil {
		v := *pos.PostTPTrailingATRMult
		snap.PostTPTrailingATRMult = &v
	}
	return snap
}

func lockHyperliquidTrailingUpdate(symbol string) func() {
	hlTrailingUpdateLocksMu.Lock()
	m := hlTrailingUpdateLocks[symbol]
	if m == nil {
		m = &sync.Mutex{}
		hlTrailingUpdateLocks[symbol] = m
	}
	hlTrailingUpdateLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}

func hlSLEffectiveQty(symbol string, virtualQty float64, onChainQtyMap map[string]float64) (float64, bool) {
	if onChainQty, ok := onChainQtyMap[symbol]; ok && onChainQty > 1e-9 && onChainQty < virtualQty-1e-9 {
		return onChainQty, true
	}
	return virtualQty, false
}

func effectiveTrailingStopPct(sc StrategyConfig, pos *Position) float64 {
	if sc.Platform != "hyperliquid" {
		return 0
	}
	switch sc.Type {
	case "perps":
	case "manual":
		if !strategyUsesTrailingTPRatchetClose(sc) {
			return 0
		}
	default:
		return 0
	}
	if pos != nil && pos.PostTPTrailingATRMult != nil && *pos.PostTPTrailingATRMult > 0 {
		if pos.EntryATR <= 0 || pos.AvgCost <= 0 {
			return 0
		}
		pct := *pos.PostTPTrailingATRMult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return *sc.TrailingStopPct
		}
		return 0
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 {
			return 0
		}
		pct := *sc.TrailingStopATRMult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	if sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero() {
		if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 || positionATRRegimeLabel(pos, sc) == "" {
			return 0
		}
		mult, ok := resolveRegimeATR(*sc.TrailingStopATRMultRegime, positionATRRegimeLabel(pos, sc))
		if !ok {
			return 0
		}
		pct := mult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	return 0
}

func atrMultMissingEntryATR(sc StrategyConfig, pos *Position) bool {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return false
	}
	wantsTrailing := sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0
	wantsFixed := sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0
	wantsRegimeFixed := sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero()
	wantsRegimeTrailing := sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero()
	if !wantsTrailing && !wantsFixed && !wantsRegimeFixed && !wantsRegimeTrailing {
		return false
	}
	if sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0 {
		return false
	}
	if pos == nil {
		return false
	}
	return pos.EntryATR <= 0 || pos.AvgCost <= 0
}

func effectiveFixedStopLossATRPct(sc StrategyConfig, pos *Position) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
	}
	mult := 0.0
	if v, ok := unifiedCloseStopLossATR(sc, protectionATRRegimeLabel(pos, sc)); ok {
		mult = v
	} else if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		mult = *sc.StopLossATRMult
	} else if sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero() {
		if pos == nil || positionATRRegimeLabel(pos, sc) == "" {
			return 0
		}
		v, ok := resolveRegimeATR(*sc.StopLossATRMultRegime, positionATRRegimeLabel(pos, sc))
		if !ok {
			return 0
		}
		mult = v
	}
	if mult <= 0 {
		return 0
	}
	if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 {
		return 0
	}
	pct := mult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
	if pct > MaxAutoStopLossPct {
		pct = MaxAutoStopLossPct
	}
	return pct
}

func fixedStopLossATRTriggerPx(sc StrategyConfig, side string, pos *Position) float64 {
	pct := effectiveFixedStopLossATRPct(sc, pos)
	if pct <= 0 || pos == nil || pos.AvgCost <= 0 {
		return 0
	}
	anchor := pos.riskAnchorPrice()
	switch side {
	case "long":
		return anchor * (1.0 - pct/100.0)
	case "short":
		return anchor * (1.0 + pct/100.0)
	}
	return 0
}

func percentStopLossTriggerPx(sc StrategyConfig, side string, anchor float64) float64 {
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
	return trigger
}

func paperStopFillPx(side string, mark, trigger float64) float64 {
	switch side {
	case "long":
		if mark > 0 && mark < trigger {
			return mark
		}
	case "short":
		if mark > trigger {
			return mark
		}
	}
	return trigger
}

func runPaperFixedStopLoss(side string, mark, currentTrigger, armTrigger float64) (newTrigger float64, breach bool, breachPx float64) {
	if mark <= 0 {
		return 0, false, 0
	}
	trigger := currentTrigger
	if trigger <= 0 {
		if armTrigger <= 0 {
			return 0, false, 0
		}
		trigger = armTrigger
		newTrigger = armTrigger
	}
	if trailingStopBreached(side, mark, trigger) {
		return newTrigger, true, paperStopFillPx(side, mark, trigger)
	}
	return newTrigger, false, 0
}

func runHyperliquidFixedATRStopLossPaper(sc StrategyConfig, side string, pos *Position, mark, currentTrigger float64) (newTrigger float64, breach bool, breachPx float64) {
	if effectiveFixedStopLossATRPct(sc, pos) <= 0 {
		return 0, false, 0
	}
	return runPaperFixedStopLoss(side, mark, currentTrigger, fixedStopLossATRTriggerPx(sc, side, pos))
}

func runHyperliquidPercentStopLossPaper(sc StrategyConfig, side string, pos *Position, mark, currentTrigger float64) (newTrigger float64, breach bool, breachPx float64) {
	if pos == nil || pos.AvgCost <= 0 || effectiveTrailingStopPct(sc, pos) > 0 || EffectiveStopLossPct(sc) <= 0 {
		return 0, false, 0
	}
	return runPaperFixedStopLoss(side, mark, currentTrigger, percentStopLossTriggerPx(sc, side, pos.riskAnchorPrice()))
}

const (
	paperStopReasonTrailing = "trailing_stop_loss_paper"
	paperStopReasonATR      = "stop_loss_atr_paper"
	paperStopReasonPct      = "stop_loss_pct_paper"
)

func paperStopLossCloseReason(sc StrategyConfig, pos *Position) string {
	switch {
	case effectiveTrailingStopPct(sc, pos) > 0:
		return paperStopReasonTrailing
	case effectiveFixedStopLossATRPct(sc, pos) > 0:
		return paperStopReasonATR
	}
	return paperStopReasonPct
}

func paperStopLossDetailLabel(reason string) string {
	switch reason {
	case paperStopReasonTrailing:
		return "PAPER TRAILING SL"
	case paperStopReasonATR:
		return "PAPER FIXED ATR SL"
	}
	return "PAPER PERCENTAGE SL"
}

func runHyperliquidFixedStopLossPaper(sc StrategyConfig, side string, pos *Position, mark, currentTrigger float64) (newTrigger float64, breach bool, breachPx float64, reason string) {
	if effectiveFixedStopLossATRPct(sc, pos) > 0 {
		newTrigger, breach, breachPx = runHyperliquidFixedATRStopLossPaper(sc, side, pos, mark, currentTrigger)
		return newTrigger, breach, breachPx, paperStopReasonATR
	}
	newTrigger, breach, breachPx = runHyperliquidPercentStopLossPaper(sc, side, pos, mark, currentTrigger)
	return newTrigger, breach, breachPx, paperStopReasonPct
}

func applyPaperStopLossBreach(sc StrategyConfig, s *StrategyState, symbol, side string, mark float64, mu *sync.RWMutex, logger *StrategyLogger) (int, string) {
	if hyperliquidIsLive(sc.Args) || s == nil || mu == nil {
		return 0, ""
	}
	mu.Lock()
	defer mu.Unlock()
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != side || !trailingStopBreached(side, mark, pos.StopLossTriggerPx) {
		return 0, ""
	}
	fillPx := paperStopFillPx(side, mark, pos.StopLossTriggerPx)
	reason := paperStopLossCloseReason(sc, hyperliquidProtectionPositionSnapshot(pos))
	if !recordPerpsStopLossClose(s, symbol, fillPx, reason, logger) {
		return 0, ""
	}
	return 1, fmt.Sprintf("[%s] %s %s @ $%.2f", sc.ID, paperStopLossDetailLabel(reason), symbol, fillPx)
}

func armPaperStopLossAtOpen(sc StrategyConfig, s *StrategyState, symbol string, mark float64, logger *StrategyLogger) (breach bool, fillPx float64, reason string) {
	if hyperliquidIsLive(sc.Args) || s == nil || mark <= 0 {
		return false, 0, ""
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.StopLossTriggerPx > 0 {
		return false, 0, ""
	}
	snap := hyperliquidProtectionPositionSnapshot(pos)
	var trigger float64
	if effectiveTrailingStopPct(sc, snap) > 0 {
		var highWater float64
		highWater, trigger, breach, fillPx = runHyperliquidTrailingStopPaper(sc, pos.Side, snap, mark, pos.riskAnchorPrice(), 0, trailingReplacePolicy{})
		if highWater > 0 {
			pos.StopLossHighWaterPx = highWater
		}
		reason = paperStopReasonTrailing
	} else {
		trigger, breach, fillPx, reason = runHyperliquidFixedStopLossPaper(sc, pos.Side, snap, mark, 0)
	}
	if trigger <= 0 {
		return false, 0, ""
	}
	pos.StopLossTriggerPx = trigger
	if logger != nil {
		logger.Info("Paper SL armed at open @ $%.4f (%s, anchor $%.4f, mark $%.4f)", trigger, reason, pos.riskAnchorPrice(), mark)
	}
	return breach, fillPx, reason
}

func hyperliquidArmFixedATRStopLossLive(sc StrategyConfig, symbol, side string, qty float64, triggerPx float64, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, bool) {
	if triggerPx <= 0 || qty <= 0 {
		return nil, true
	}
	if logger != nil {
		logger.Info("Arming fixed ATR SL for %s: side=%s qty=%.6f trigger=$%.4f", symbol, side, qty, triggerPx)
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, triggerPx, 0)
	if stderr != "" && logger != nil {
		logger.Info("arm fixed SL stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Fixed ATR SL arm failed: %v", err)
		}
		notifyLiveExecFailure(notifier, sc, "fixed-atr-sl-arm", symbol, err.Error())
		return result, false
	}
	if result.Error != "" {
		if logger != nil {
			logger.Error("Fixed ATR SL arm returned error: %s", result.Error)
		}
		notifyLiveExecFailure(notifier, sc, "fixed-atr-sl-arm", symbol, result.Error)
		return result, false
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected fixed ATR SL arm for %s - position is unprotected: %s",
					symbol, result.StopLossError)
			}
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s fixed ATR SL arm rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else if logger != nil {
			logger.Warn("Fixed ATR SL arm placement failed (non-fatal): %s", result.StopLossError)
		}
	}
	if result.StopLossFilledImmediately && logger != nil {
		logger.Warn("Fixed ATR SL trigger filled at submit for %s — position is flat on-chain", symbol)
	}
	return result, true
}

var atrMultMissingEntryATRWarned sync.Map

func atrMultMissingEntryATRKey(strategyID, symbol string) string {
	return strategyID + ":" + symbol
}

func notifyATRMultMissingEntryATROnce(sc StrategyConfig, symbol string, notifier *MultiNotifier, logger *StrategyLogger) {
	key := atrMultMissingEntryATRKey(sc.ID, symbol)
	if _, loaded := atrMultMissingEntryATRWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger != nil {
		logger.Warn("trailing_stop_atr_mult set but Position.EntryATR is 0 for %s — entry strategy must emit an 'atr' indicator on the open candle, so no ATR-derived trigger has been armed for this strategy.", symbol)
	}
	if notifier != nil && notifier.HasBackends() {
		msg := fmt.Sprintf("**HL TRAILING ATR-MULT MISSING ENTRY ATR** [%s] %s — strategy is configured with trailing_stop_atr_mult but the open candle did not produce an ATR indicator, so no ATR-derived trigger has been armed for this strategy. Verify the entry strategy emits `atr`, or switch to a fixed `trailing_stop_pct`. (If a peer strategy on the same coin owns the trigger, this strategy is still covered by the shared exchange-side stop.)",
			sc.ID, symbol)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

func clearATRMultMissingEntryATRWarning(strategyID, symbol string) {
	atrMultMissingEntryATRWarned.Delete(atrMultMissingEntryATRKey(strategyID, symbol))
}

func clearATRMultMissingEntryATRWarningOnHLPerpsClose(s *StrategyState, symbol string) {
	if s == nil || s.Platform != "hyperliquid" || s.Type != "perps" {
		return
	}
	clearATRMultMissingEntryATRWarning(s.ID, symbol)
}

func clearATRMultMissingEntryATRWarningsForStrategy(strategyID string) {
	prefix := strategyID + ":"
	atrMultMissingEntryATRWarned.Range(func(k, _ any) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			atrMultMissingEntryATRWarned.Delete(k)
		}
		return true
	})
}

func tieredTPATRMissingEntryATR(sc StrategyConfig, pos *Position) bool {
	hasTieredTP := false
	for _, ref := range sc.closeRefs() {
		if isTieredTPATRCloseName(ref.Name) {
			hasTieredTP = true
			break
		}
	}
	if !hasTieredTP {
		return false
	}
	if pos == nil {
		return false
	}
	return pos.EntryATR <= 0 && pos.AvgCost > 0
}

func notifyTieredTPATRMissingEntryATROnce(sc StrategyConfig, symbol string, notifier *MultiNotifier, logger *StrategyLogger) {
	key := atrMultMissingEntryATRKey(sc.ID, symbol)
	if _, loaded := atrMultMissingEntryATRWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger != nil {
		logger.Warn("tiered_tp_atr configured but Position.EntryATR is 0 for %s — the open strategy must emit an 'atr' indicator on the open candle; take-profit tiers will noop until EntryATR is stamped.", symbol)
	}
	if notifier != nil && notifier.HasBackends() {
		msg := fmt.Sprintf("**MISSING ENTRY ATR** [%s] %s — close strategy `tiered_tp_atr` is configured but the open candle did not produce an ATR indicator, so take-profit tiers are disabled until EntryATR is stamped. Ensure the entry strategy emits `atr` in its indicator output.",
			sc.ID, symbol)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

func effectiveTrailingStopMinMovePct(sc StrategyConfig) float64 {
	if sc.TrailingStopMinMovePct != nil && *sc.TrailingStopMinMovePct >= 0 {
		return *sc.TrailingStopMinMovePct
	}
	return defaultTrailingStopMinMovePct
}

type trailingReplacePolicy struct {
	forceResize bool

	ratchetTightened bool

	liquidationPx float64
}

func computeTrailingStopUpdate(side string, mark, highWater, trailingPct, minMovePct, currentTrigger float64) (float64, float64, bool) {
	return computeTrailingStopUpdateInternal(side, mark, highWater, trailingPct, minMovePct, currentTrigger, false, false)
}

func computeTrailingStopUpdateInternal(side string, mark, highWater, trailingPct, minMovePct, currentTrigger float64, allowOneShotWiden, bypassMinMove bool) (float64, float64, bool) {
	if mark <= 0 || trailingPct <= 0 {
		return highWater, 0, false
	}
	if highWater <= 0 {
		highWater = mark
	}

	candidateHighWater := highWater
	switch side {
	case "long":
		if mark > candidateHighWater {
			candidateHighWater = mark
		}
	case "short":
		if mark < candidateHighWater {
			candidateHighWater = mark
		}
	default:
		return highWater, 0, false
	}
	if candidateHighWater <= 0 {
		return highWater, 0, false
	}

	var candidateTrigger float64
	switch side {
	case "long":
		candidateTrigger = candidateHighWater * (1.0 - trailingPct/100.0)
	case "short":
		candidateTrigger = candidateHighWater * (1.0 + trailingPct/100.0)
	}
	if candidateTrigger <= 0 {
		return candidateHighWater, 0, false
	}
	if currentTrigger <= 0 {
		return candidateHighWater, candidateTrigger, true
	}

	favorable := (side == "long" && candidateTrigger > currentTrigger) ||
		(side == "short" && candidateTrigger < currentTrigger)
	if !favorable {
		if allowOneShotWiden && math.Abs(candidateTrigger-currentTrigger) > 1e-9 {
			return candidateHighWater, candidateTrigger, true
		}
		return candidateHighWater, 0, false
	}
	if bypassMinMove && math.Abs(candidateTrigger-currentTrigger) > 1e-9 {
		return candidateHighWater, candidateTrigger, true
	}
	movePct := math.Abs(candidateTrigger-currentTrigger) / currentTrigger * 100.0
	if movePct >= minMovePct {
		return candidateHighWater, candidateTrigger, true
	}
	return candidateHighWater, 0, false
}

func trailingStopBreached(side string, mark, currentTrigger float64) bool {
	if mark <= 0 || currentTrigger <= 0 {
		return false
	}
	switch side {
	case "long":
		return mark <= currentTrigger
	case "short":
		return mark >= currentTrigger
	}
	return false
}

func runHyperliquidTrailingStopPaper(sc StrategyConfig, side string, pos *Position, mark, highWater, currentTrigger float64, policy trailingReplacePolicy) (newHighWater, newTrigger float64, breach bool, breachPx float64) {
	trailingPct := effectiveTrailingStopPct(sc, pos)
	if trailingPct <= 0 || mark <= 0 {
		return highWater, 0, false, 0
	}
	if trailingStopBreached(side, mark, currentTrigger) {
		return highWater, 0, true, paperStopFillPx(side, mark, currentTrigger)
	}
	avgCost := 0.0
	if pos != nil {
		avgCost = pos.riskAnchorPrice()
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	allowOneShotWiden := pos != nil && pos.RatchetFallbackNormalizePending
	nhw, nt, replace := computeTrailingStopUpdateInternal(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger, allowOneShotWiden, policy.ratchetTightened)
	if replace {
		if trailingStopBreached(side, mark, nt) {
			return nhw, nt, true, paperStopFillPx(side, mark, nt)
		}
		return nhw, nt, false, 0
	}
	return nhw, 0, false, 0
}

func applyTrailingStopUpdateResult(s *StrategyState, symbol, expectedSide string, prevSLOID int64, newHighWater float64, updateConfirmed bool, slUpdate *HyperliquidStopLossUpdateResult, closeReason string, logger *StrategyLogger, placedQty float64) (immediateFill bool, fillPx float64) {
	if s == nil {
		return false, 0
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 {
		return false, 0
	}
	if expectedSide != "" && pos.Side != expectedSide {
		return false, 0
	}
	if newHighWater > 0 && updateConfirmed {
		pos.StopLossHighWaterPx = newHighWater
	}
	if slUpdate == nil {
		return false, 0
	}
	if closeReason == "" {
		closeReason = "trailing_stop_loss_immediate"
	}
	switch {
	case slUpdate.StopLossFilledImmediately && slUpdate.StopLossTriggerPx > 0:
		pos.RatchetFallbackNormalizePending = false
		if recordPerpsStopLossCloseQty(s, symbol, placedQty, slUpdate.StopLossTriggerPx, closeReason, logger) {
			if residue, ok := s.Positions[symbol]; ok && residue != nil && residue.Quantity > 0 {
				residue.StopLossOID = 0
				residue.StopLossTriggerPx = 0
				residue.RatchetFallbackNormalizePending = false
			}
			return true, slUpdate.StopLossTriggerPx
		}
	case slUpdate.StopLossOID > 0:
		pos.StopLossOID = slUpdate.StopLossOID
		pos.StopLossTriggerPx = slUpdate.StopLossTriggerPx
		pos.RatchetFallbackNormalizePending = false
		if logger != nil {
			logger.Info("Trailing SL trigger updated oid=%d @ $%.4f", slUpdate.StopLossOID, slUpdate.StopLossTriggerPx)
		}
	case slUpdate.StopLossOutcomeUnknown && slUpdate.StopLossOldStillOpen && !slUpdate.CancelStopLossSucceeded:
		if prevSLOID > 0 {
			hlRememberUnreadableStop(symbol, prevSLOID, slUpdate.PrePlaceOpenOIDs)
		}
		if logger != nil {
			logger.Warn("Trailing SL replacement for %s OID=%d could not be read; the old stop stays until the order book can be read", symbol, prevSLOID)
		}
	case slUpdate.StopLossOutcomeUnknown:
		if pos.StopLossOID == prevSLOID {
			pos.StopLossOID = 0
		}
		if slUpdate.StopLossTriggerPx > 0 {
			pos.StopLossTriggerPx = slUpdate.StopLossTriggerPx
			if logger != nil {
				logger.Info("Unreadable placement outcome for %s: requested trigger $%.4f recorded (oid unknown)", symbol, slUpdate.StopLossTriggerPx)
			}
		}
		if logger != nil && prevSLOID > 0 {
			logger.Warn("Trailing SL old OID=%d was cancelled and the replacement's outcome could NOT be read — recorded trigger kept, oid unknown; no re-place is licensed until a readable attempt", prevSLOID)
		}
	case slUpdate.CancelStopLossSucceeded && prevSLOID > 0 && pos.StopLossOID == prevSLOID:
		pos.StopLossOID = 0
		pos.StopLossTriggerPx = 0
		if logger != nil {
			logger.Warn("Trailing SL old OID=%d was cancelled but replacement did not rest", prevSLOID)
		}
	}
	return false, 0
}

func runHyperliquidTrailingStopUpdate(sc StrategyConfig, symbol, side string, qty float64, pos *Position, mark, highWater, currentTrigger float64, currentOID int64, policy trailingReplacePolicy, notifier *MultiNotifier, logger *StrategyLogger) (float64, *HyperliquidStopLossUpdateResult, bool) {
	trailingPct := effectiveTrailingStopPct(sc, pos)
	if trailingPct <= 0 || qty <= 0 || mark <= 0 {
		return highWater, nil, true
	}
	avgCost := 0.0
	if pos != nil {
		avgCost = pos.riskAnchorPrice()
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	allowOneShotWiden := pos != nil && pos.RatchetFallbackNormalizePending
	newHighWater, newTrigger, replace := computeTrailingStopUpdateInternal(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger, allowOneShotWiden, policy.ratchetTightened)
	if policy.forceResize && !replace {
		replace = true
		if currentTrigger > 0 {
			newTrigger = currentTrigger
			if _, tighter, ok := computeTrailingStopUpdateInternal(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger, allowOneShotWiden, true); ok && tighter > 0 {
				newTrigger = tighter
			}
		}
	}
	clampOutcome := hlLiquidationActionReplaceDeferred
	clampTriggered := false
	if policy.liquidationPx > 0 {
		offending := newTrigger
		if !replace {
			offending = currentTrigger
		}
		if clamped, clampedOK := clampStopInsideLiquidation(side, offending, policy.liquidationPx); clampedOK {
			newTrigger = clamped
			replace = true
			clampTriggered = true
			defer func() {
				notifyHLStopPastLiquidation(sc, symbol, side, offending, clamped, policy.liquidationPx, clampOutcome, notifier, logger, time.Now().UTC())
			}()
		}
	}

	if !replace {
		return newHighWater, nil, true
	}
	if hlStopPlaceUnread(symbol, currentOID) {
		released, adopted, alert := hlReleaseUnreadableStop(sc.Script, symbol, side, currentOID, qty, newTrigger)
		if !released {
			logger.Info("Trailing SL replace for %s OID=%d held: the order book could not be read, so the old stop stays", symbol, currentOID)
			return highWater, nil, false
		}
		if alert != "" {
			hlStopReplaceNotifyOnce(sc.ID+"|unread-end|"+symbol+"|"+strconv.FormatInt(currentOID, 10), notifier, alert)
		}
		if adopted != nil {
			msg := fmt.Sprintf("**HL TRAILING SL OUTCOME UNKNOWN** [%s] %s: the earlier replacement could not be read. Open order %d is now recorded and old OID %d may still be resting.",
				sc.ID, symbol, adopted.StopLossOID, currentOID)
			hlStopReplaceNotifyOnce(sc.ID+"|unread|"+symbol+"|"+strconv.FormatInt(currentOID, 10), notifier, msg)
			return newHighWater, adopted, true
		}
	}

	logger.Info("Updating trailing SL for %s: side=%s mark=$%.4f high_water=$%.4f trigger=$%.4f cancel_oid=%d",
		symbol, side, mark, newHighWater, newTrigger, currentOID)
	unlock := lockHyperliquidTrailingUpdate(symbol)
	defer unlock()
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, newTrigger, currentOID)
	if stderr != "" {
		logger.Info("update stop-loss stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Trailing SL update failed: %v", err)
		return highWater, result, false
	}
	if result == nil {
		logger.Error("Trailing SL update returned no result")
		return highWater, nil, false
	}
	if result.Error != "" && !result.CancelStopLossSucceeded {
		logger.Error("Trailing SL update returned error: %s", result.Error)
		return highWater, result, false
	}
	if result.Error != "" {
		logger.Error("Trailing SL update returned error AFTER the old trigger was cancelled (%s) — treating as cancel-landed", result.Error)
	}
	if result.OpenOrderCheckError != "" {
		logger.Warn("Trailing SL open-order check failed; replacement deferred: %s", result.OpenOrderCheckError)
		if currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL TRAILING SL REPLACEMENT DEFERRED** [%s] %s old trigger OID %d was not replaced because open-order lookup failed. The scheduler will retry next cycle. Error: %s",
				sc.ID, symbol, currentOID, result.OpenOrderCheckError)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
		return highWater, result, false
	}
	if result.StopLossFilledExternally {
		logger.Warn("Trailing SL OID=%d already filled on-chain for %s — reconciler will book the close", currentOID, symbol)
		clampOutcome = hlLiquidationActionFilledOnChain
		return highWater, result, false
	}
	newStopResting := result.StopLossOID > 0 || (result.StopLossFilledImmediately && result.StopLossTriggerPx > 0)
	if result.CancelStopLossError != "" && !newStopResting {
		logger.Warn("Trailing SL cancel failed; replacement deferred: %s", result.CancelStopLossError)
		if currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL TRAILING SL CANCEL FAILED** [%s] %s old trigger OID %d was not replaced. The scheduler will retry next cycle. Error: %s",
				sc.ID, symbol, currentOID, result.CancelStopLossError)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
		return highWater, result, false
	}
	if result.CancelStopLossError != "" && result.StopLossOID > 0 {
		logger.Warn("Trailing SL cancel failed after the replacement was placed: %s", result.CancelStopLossError)
		if currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL TRAILING SL CANCEL FAILED** [%s] %s old trigger OID %d may still be resting while new trigger OID %d was placed. Error: %s",
				sc.ID, symbol, currentOID, result.StopLossOID, result.CancelStopLossError)
			hlStopReplaceNotifyOnce(sc.ID+"|cancel|"+symbol+"|"+strconv.FormatInt(currentOID, 10), notifier, msg)
		}
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			logger.Error("CRITICAL: HL open-order-cap rejected trailing SL update for %s - position may be under-protected: %s",
				symbol, result.StopLossError)
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s trailing SL update rejected: %s",
					sc.ID, symbol, result.StopLossError)
				hlStopReplaceNotifyOnce(sc.ID+"|cap|"+symbol, notifier, msg)
			}
		} else {
			logger.Warn("Trailing SL placement failed (non-fatal): %s", result.StopLossError)
		}
	}
	if result.StopLossFilledImmediately {
		logger.Warn("Trailing SL trigger filled at submit for %s — position is flat on-chain", symbol)
	}
	restingConfirmed := (result.StopLossOID > 0) ||
		(result.StopLossFilledImmediately && result.StopLossTriggerPx > 0)
	filledAtSubmit := result.StopLossFilledImmediately && result.StopLossTriggerPx > 0
	updateConfirmed := restingConfirmed || result.CancelStopLossSucceeded
	if result.StopLossOutcomeUnknown && result.StopLossOldStillOpen && !result.CancelStopLossSucceeded {
		msg := fmt.Sprintf("**HL TRAILING SL OUTCOME UNKNOWN** [%s] %s: the replacement could NOT be read and old OID %d was left resting. No further stop is placed until the order book can be read.",
			sc.ID, symbol, currentOID)
		hlStopReplaceNotifyOnce(sc.ID+"|unread|"+symbol+"|"+strconv.FormatInt(currentOID, 10), notifier, msg)
	}
	if !updateConfirmed {
		return highWater, result, false
	}
	if restingConfirmed {
		hlStopReplaceAlertOnce.Delete(sc.ID + "|cap|" + symbol)
	}
	retryOutcomeUnknown := false
	if result.CancelStopLossSucceeded && !restingConfirmed && clampTriggered && !result.StopLossOutcomeUnknown {
		retryResult, retryOutcome := hlLiquidationPlaceFresh(sc.Script, symbol, side, qty, newTrigger, logger)
		switch retryOutcome {
		case hlReplacePlaced, hlReplaceFilled:
			result = retryResult
			filledAtSubmit = result.StopLossFilledImmediately && result.StopLossTriggerPx > 0
			restingConfirmed = !filledAtSubmit && result.StopLossOID > 0
		case hlReplaceOutcomeUnknown:
			result = retryResult
			retryOutcomeUnknown = true
		}
	}
	switch {
	case filledAtSubmit:
		clampOutcome = hlLiquidationActionExited
	case restingConfirmed:
		clampOutcome = hlLiquidationActionClamped
	case retryOutcomeUnknown:
		clampOutcome = hlLiquidationActionPlacementUnknown
	case result.CancelStopLossSucceeded && result.StopLossOutcomeUnknown:
		clampOutcome = hlLiquidationActionOutcomeUnknown
	case result.CancelStopLossSucceeded:
		clampOutcome = hlLiquidationActionProtectionLost
	}
	return newHighWater, result, true
}
