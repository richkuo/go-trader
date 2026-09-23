package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

const (
	hlVenueMinOrderNotionalUSD    = 10.0
	hlVenueMinOrderNotionalMargin = 0.03
	hlSharedCloseStrandedGate     = "shared_coin_stranded_remainder"
	hlSharedCloseDeferredGate     = "shared_coin_on_chain_unknown"
	hlSharedCloseQtyTolerance     = 1e-6
	hlSharedCloseHoldPeerBusy     = "peer_busy"
	hlSharedCloseHoldVenueReject  = "venue_rejected"
	hlSharedCloseHoldEscalateFail = "escalate_failed"
	hlSharedCloseHoldOperatorRef  = "operator_refused"
)

type hlSharedCloseFloorOutcome int

const (
	hlSharedCloseFloorNone hlSharedCloseFloorOutcome = iota
	hlSharedCloseFloorEscalate
	hlSharedCloseFloorHold
	hlSharedCloseFloorHeld
	hlSharedCloseFloorDefer
)

func (o hlSharedCloseFloorOutcome) String() string {
	switch o {
	case hlSharedCloseFloorEscalate:
		return "escalate"
	case hlSharedCloseFloorHold:
		return "hold"
	case hlSharedCloseFloorHeld:
		return "held"
	case hlSharedCloseFloorDefer:
		return "defer"
	default:
		return "none"
	}
}

type hlOnChainCoinView struct {
	Known   bool
	AbsQty  map[string]float64
	NetSide map[string]string
}

func hlPeerVirtualQtyOnCoin(snapshot hlVirtualQuantitySnapshot, coin, selfID string) float64 {
	target := strings.ToUpper(strings.TrimSpace(coin))
	total := 0.0
	for rawCoin, byID := range snapshot {
		if strings.ToUpper(strings.TrimSpace(rawCoin)) != target {
			continue
		}
		for id, qty := range byID {
			if id != selfID && qty > 0 {
				total += qty
			}
		}
	}
	return total
}

func hlVenueCloseGateThresholdUSD() float64 {
	return hlVenueMinOrderNotionalUSD * (1.0 + hlVenueMinOrderNotionalMargin)
}

func hlOnChainCoinViewFromPositions(positions []HLPosition) hlOnChainCoinView {
	absQty, _, netSide := buildHLLiquidationMaps(positions)
	return hlOnChainCoinView{Known: true, AbsQty: absQty, NetSide: netSide}
}

func hlPeerFlatOnCoin(symbol, posSide string, posQty float64, onChain hlOnChainCoinView) bool {
	coin := strings.TrimSpace(symbol)
	onChainQty := onChain.AbsQty[coin]
	if onChainQty <= 1e-9 {
		return true
	}
	if side, ok := onChain.NetSide[coin]; ok && side != posSide {
		return false
	}
	return onChainQty <= posQty+hlSharedCloseQtyTolerance
}

func evaluateSharedCoinFullCloseFloor(closeFraction float64, symbol string, hlLiveAll []StrategyConfig, posQty float64, posSide string, price float64, onChain hlOnChainCoinView, peerVirtualQty float64, heldReason string) (hlSharedCloseFloorOutcome, float64) {
	if closeFraction != 1.0 || posQty <= 0 || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return hlSharedCloseFloorNone, 0
	}
	if len(hlLiveStrategiesForCoin(symbol, hlLiveAll)) <= 1 {
		return hlSharedCloseFloorNone, 0
	}
	remainderUSD := posQty * price
	if remainderUSD >= hlVenueCloseGateThresholdUSD() {
		return hlSharedCloseFloorNone, remainderUSD
	}
	if heldReason == hlSharedCloseHoldVenueReject {
		return hlSharedCloseFloorHeld, remainderUSD
	}
	if !onChain.Known {
		return hlSharedCloseFloorDefer, remainderUSD
	}
	if peerVirtualQty <= 1e-9 && hlPeerFlatOnCoin(symbol, posSide, posQty, onChain) {
		return hlSharedCloseFloorEscalate, remainderUSD
	}
	if heldReason == hlSharedCloseHoldPeerBusy {
		return hlSharedCloseFloorHeld, remainderUSD
	}
	return hlSharedCloseFloorHold, remainderUSD
}

type sharedCloseFloorResolution struct {
	Outcome        hlSharedCloseFloorOutcome
	RemainderUSD   float64
	OnChainQty     float64
	SoughtEscalate bool
	RefetchMissing bool
	RefetchFailed  bool
	RefetchErr     error
}

func resolveSharedCloseFloorEscalation(closeFraction float64, symbol string, hlLiveAll []StrategyConfig, posQty float64, posSide string, price float64, onChain hlOnChainCoinView, peerVirtualQty float64, heldReason string, refetch func() (hlOnChainCoinView, error)) sharedCloseFloorResolution {
	coin := strings.TrimSpace(symbol)
	r := sharedCloseFloorResolution{OnChainQty: onChain.AbsQty[coin]}
	r.Outcome, r.RemainderUSD = evaluateSharedCoinFullCloseFloor(closeFraction, symbol, hlLiveAll, posQty, posSide, price, onChain, peerVirtualQty, heldReason)
	if r.Outcome != hlSharedCloseFloorEscalate {
		return r
	}
	r.SoughtEscalate = true
	if refetch == nil {
		r.RefetchMissing = true
		r.Outcome = hlSharedCloseFloorDefer
		return r
	}
	fresh, err := refetch()
	if err != nil || !fresh.Known {
		r.RefetchFailed = true
		r.RefetchErr = err
		r.Outcome = hlSharedCloseFloorDefer
		return r
	}
	r.OnChainQty = fresh.AbsQty[coin]
	r.Outcome, r.RemainderUSD = evaluateSharedCoinFullCloseFloor(closeFraction, symbol, hlLiveAll, posQty, posSide, price, fresh, peerVirtualQty, heldReason)
	return r
}

type operatorSharedCloseDecision struct {
	Escalate       bool
	Refuse         bool
	MarkUnreadable bool
	RemainderUSD   float64
	Reason         string
}

func operatorSharedCloseUnprovableReason(symbol string, remainderUSD float64, peers int, err error) string {
	detail := "the on-chain account positions are not readable"
	if err != nil {
		detail = fmt.Sprintf("the on-chain account read failed: %v", err)
	}
	return fmt.Sprintf("cannot prove every peer is flat on %s (%s); the closing value $%.2f is under the $%.2f venue minimum gate, so the venue rejects a sized close and a whole-position close could take the exposure of one of the %d live strategies sharing this coin — no close order sent",
		symbol, detail, remainderUSD, hlVenueCloseGateThresholdUSD(), peers)
}

func decideOperatorSharedCloseFloor(symbol, posSide string, posQty, price float64, hlLiveAll []StrategyConfig, peerVirtualQty float64, fetchOnChain func() (hlOnChainCoinView, error)) operatorSharedCloseDecision {
	var d operatorSharedCloseDecision
	peers := len(hlLiveStrategiesForCoin(symbol, hlLiveAll))
	if posQty <= 0 || peers <= 1 {
		return d
	}
	gate := hlVenueCloseGateThresholdUSD()
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		d.MarkUnreadable = true
		d.Reason = fmt.Sprintf("no usable mark price for %s, so the closing value cannot be measured against the $%.2f venue minimum gate; %d live strategies share this coin, so the escalation to a whole-position close is withheld and the sized close is sent unchanged (manual-close sends it reduce-only and capped at the on-chain quantity, or as the netted cross order when an opposite-side peer shares the coin; force-close sends it reduce-only) — the venue rejects it if the value is under the gate",
			symbol, gate, peers)
		return d
	}
	d.RemainderUSD = posQty * price
	if d.RemainderUSD >= gate {
		return d
	}
	if fetchOnChain == nil {
		d.Refuse = true
		d.Reason = operatorSharedCloseUnprovableReason(symbol, d.RemainderUSD, peers, fmt.Errorf("no Hyperliquid account reader is configured"))
		return d
	}
	onChain, err := fetchOnChain()
	if err != nil || !onChain.Known {
		d.Refuse = true
		d.Reason = operatorSharedCloseUnprovableReason(symbol, d.RemainderUSD, peers, err)
		return d
	}
	r := resolveSharedCloseFloorEscalation(1.0, symbol, hlLiveAll, posQty, posSide, price, onChain, peerVirtualQty, "", fetchOnChain)
	d.RemainderUSD = r.RemainderUSD
	switch r.Outcome {
	case hlSharedCloseFloorEscalate:
		d.Escalate = true
	case hlSharedCloseFloorDefer:
		d.Refuse = true
		d.Reason = operatorSharedCloseUnprovableReason(symbol, r.RemainderUSD, peers, r.RefetchErr)
	default:
		d.Refuse = true
		d.Reason = fmt.Sprintf("a peer strategy still holds quantity on %s (peers hold %.6f in their own books and the account holds %.6f on-chain against this position's %.6f), so a whole-position close would take its exposure; the closing value $%.2f is under the $%.2f venue minimum gate, so the venue rejects a sized close — no close order sent",
			symbol, peerVirtualQty, r.OnChainQty, posQty, r.RemainderUSD, gate)
	}
	return d
}

func operatorRefusalWithCancelledRestingLimits(reason string, cancelledOIDs []int64) string {
	if reason == "" || len(cancelledOIDs) == 0 {
		return reason
	}
	oids := make([]string, 0, len(cancelledOIDs))
	for _, oid := range cancelledOIDs {
		oids = append(oids, fmt.Sprintf("%d", oid))
	}
	return fmt.Sprintf("%s; this strategy's resting limit order(s) oid=%s were already cancelled on the venue before the refusal and are not restored — re-place them by hand if you still want them",
		reason, strings.Join(oids, ","))
}

func formatSharedCloseStrandedAlert(strategyID, symbol string, remainderUSD float64, reason, holdReason string) string {
	recovery := "The scheduler holds this close and will not resend it until the value rises above the gate or every peer is flat on-chain and in its own book; a hand close (manual-close or force-close) escalates to a whole-position close only under that same peer-flat proof and otherwise refuses, so to end it sooner close the remainder directly on the venue or add to it above the gate."
	switch holdReason {
	case hlSharedCloseHoldVenueReject:
		recovery = "The scheduler holds this close and will not resend it until the value rises above the gate; a peer going flat does not resend it, and a hand close escalates to a whole-position close only under the same peer-flat proof and otherwise refuses, so close the remainder directly on the venue or add to it above the gate."
	case hlSharedCloseHoldOperatorRef:
		recovery = "The hand close was refused and no close order was sent; the scheduler's own hold on this position is unchanged. A hand close escalates to a whole-position close only when every peer is flat on-chain and in its own book, so close the remainder directly on the venue or add to it above the gate."
	}
	value := fmt.Sprintf("the final full close is worth $%.2f, under the $%.2f gate", remainderUSD, hlVenueCloseGateThresholdUSD())
	if remainderUSD <= 0 {
		value = fmt.Sprintf("the closing value of the final full close could not be measured against the $%.2f gate", hlVenueCloseGateThresholdUSD())
	}
	return fmt.Sprintf("**CRITICAL — stranded remainder below venue minimum gate** [%s] %s: %s (the $%.2f venue minimum plus the %.0f%% safety margin). %s %s",
		strategyID, symbol, value, hlVenueMinOrderNotionalUSD, hlVenueMinOrderNotionalMargin*100, reason, recovery)
}

func notifySharedCloseStranded(notifier *MultiNotifier, sc StrategyConfig, symbol string, remainderUSD float64, reason, holdReason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatSharedCloseStrandedAlert(sc.ID, symbol, remainderUSD, reason, holdReason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func applySharedCoinFullCloseFloor(sc StrategyConfig, result *HyperliquidResult, posQty float64, posSide string, price float64, hlLiveAll []StrategyConfig, onChain hlOnChainCoinView, peerVirtualQty float64, heldReason string, refetch func() (hlOnChainCoinView, error), notifier *MultiNotifier, logger *StrategyLogger) (hlSharedCloseFloorOutcome, float64) {
	if result == nil || result.Signal == 0 {
		return hlSharedCloseFloorNone, 0
	}
	r := resolveSharedCloseFloorEscalation(result.CloseFraction, result.Symbol, hlLiveAll, posQty, posSide, price, onChain, peerVirtualQty, heldReason, refetch)
	outcome, remainderUSD := r.Outcome, r.RemainderUSD
	if r.RefetchFailed {
		logger.Warn("Final full close %s: pre-escalation account refetch failed (%v) — deferring to the next cycle", result.Symbol, r.RefetchErr)
	} else if r.SoughtEscalate && !r.RefetchMissing && outcome != hlSharedCloseFloorEscalate {
		logger.Warn("Final full close %s: the refetched account state no longer shows every peer flat — outcome %s", result.Symbol, outcome)
	}
	switch outcome {
	case hlSharedCloseFloorEscalate:
		result.ForceFullClose = true
		logger.Info("Final full close %s worth $%.2f is below the venue minimum gate and every peer is flat on the refetched account state and in its own book — escalating to market_close(sz=None)", result.Symbol, remainderUSD)
	case hlSharedCloseFloorHold:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Error("Final full close %s worth $%.2f is below the venue minimum gate while a peer holds quantity (peer virtual %.6f) — holding the close and alerting once; stop-loss management continues", result.Symbol, remainderUSD, peerVirtualQty)
		notifySharedCloseStranded(notifier, sc, result.Symbol, remainderUSD, "A peer strategy still holds quantity on this coin (on-chain or in its own book), so a whole-position close would take its exposure and a sized close is rejected by the venue.", hlSharedCloseHoldPeerBusy)
	case hlSharedCloseFloorHeld:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Info("Final full close %s still stranded below the venue minimum ($%.2f, hold reason %s) — hold persists, no order sent", result.Symbol, remainderUSD, heldReason)
	case hlSharedCloseFloorDefer:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseDeferredGate
		logger.Warn("Final full close %s worth $%.2f is below the venue minimum but on-chain positions are not known for this cycle — deferring to the next cycle", result.Symbol, remainderUSD)
	}
	return outcome, remainderUSD
}

func isHLMinOrderValueRejection(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "minimum value") || strings.Contains(lower, "min value") || strings.Contains(lower, "minimum order value")
}

func stampSharedCloseHold(s *StrategyState, symbol string, remainderUSD float64, reason string) {
	if s == nil {
		return
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil {
		pos.SharedCloseHoldUSD = remainderUSD
		pos.SharedCloseHoldReason = reason
	}
}

func clearSharedCloseHold(s *StrategyState, symbol string) bool {
	if s == nil {
		return false
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil && (pos.SharedCloseHoldUSD != 0 || pos.SharedCloseHoldReason != "") {
		pos.SharedCloseHoldUSD = 0
		pos.SharedCloseHoldReason = ""
		return true
	}
	return false
}

type hlStopRearmStatus int

const (
	hlStopRearmUnclaimed hlStopRearmStatus = iota
	hlStopRearmPlaced
	hlStopRearmClosed
	hlStopRearmUnbacked
	hlStopRearmNoTrigger
	hlStopRearmNoArm
	hlStopRearmReadFailed
	hlStopRearmPreCloseStopResting
	hlStopRearmPlacementFailed
	hlStopRearmProtectionLost
	hlStopRearmOutcomeUnknown
	hlStopRearmRemoved
)

const (
	hlRearmOwnerATR      = "ATR stop"
	hlRearmOwnerTrailing = "trailing stop"
	hlRearmOwnerPercent  = "percentage stop"
	hlRearmOwnerRecorded = "recorded manual stop"
)

type hlStopRearmResult struct {
	Status    hlStopRearmStatus
	Owner     string
	Qty       float64
	TriggerPx float64
	OID       int64
	Detail    string
	Removal   hlCloseRemovalReport
}

func (r hlStopRearmResult) failed() bool {
	switch r.Status {
	case hlStopRearmUnclaimed, hlStopRearmPlaced, hlStopRearmClosed, hlStopRearmRemoved:
		return false
	}
	return true
}

const hlStopRearmNoResultDetail = "the update returned no result; a cancel or a placement may have happened"

type hlCloseRemovalReport struct {
	StopOID    int64
	Removed    []int64
	Filled     []int64
	Resting    []int64
	Unverified []int64
	Detail     string
}

func (r *hlCloseRemovalReport) addDetail(detail string) {
	if strings.TrimSpace(detail) == "" {
		return
	}
	if r.Detail == "" {
		r.Detail = detail
		return
	}
	r.Detail += "; " + detail
}

func classifyStopRearmRemoval(result *HyperliquidStopLossUpdateResult) hlStopRearmResult {
	out := hlStopRearmResult{Owner: "pre-close stop"}
	switch {
	case result == nil:
		out.Status = hlStopRearmOutcomeUnknown
		out.Detail = hlStopRearmNoResultDetail
	case !result.CancelOnly:
		out.Status = hlStopRearmOutcomeUnknown
		out.Detail = firstNonEmptyText(result.Error, "the update did not run as a cancel-only removal")
	case result.CancelStopLossSucceeded, result.StopLossNotOpen:
		out.Status = hlStopRearmRemoved
	case result.StopLossFilledExternally:
		out.Status = hlStopRearmClosed
		out.Detail = "the pre-close stop had already filled on-chain"
	case result.OpenOrderCheckError != "":
		out.Status = hlStopRearmReadFailed
		out.Detail = result.OpenOrderCheckError
	case result.CancelStopLossError != "":
		out.Status = hlStopRearmPreCloseStopResting
		out.Detail = result.CancelStopLossError
	default:
		out.Status = hlStopRearmOutcomeUnknown
		out.Detail = firstNonEmptyText(result.Error, "the cancel-only removal reported no outcome")
	}
	return out
}

type hlTPRearmStatus int

const (
	hlTPRearmNone hlTPRearmStatus = iota
	hlTPRearmPlaced
	hlTPRearmKept
	hlTPRearmRemoved
	hlTPRearmFailed
	hlTPRearmUnknown
)

type hlTPRearmResult struct {
	Status  hlTPRearmStatus
	Detail  string
	Unknown string
}

func (r hlTPRearmResult) critical() bool {
	return r.Status == hlTPRearmFailed || r.Status == hlTPRearmUnknown
}

func classifyProtectionSyncTPRearm(plan hlProtectionPlan, result *HyperliquidProtectionSyncResult) hlTPRearmResult {
	if len(plan.Tiers) == 0 && len(plan.CancelTPOIDs) == 0 {
		return hlTPRearmResult{}
	}
	if result == nil {
		return hlTPRearmResult{Status: hlTPRearmUnknown, Detail: "the protection sync returned no result; a take-profit cancel or placement may have happened"}
	}
	unknown := ""
	if tiers := unknownTPPlacementTiers(result); len(tiers) > 0 {
		unknown = fmt.Sprintf("the placement of tier(s) %v could not be resolved, so a reduce-only order may rest untracked", tiers)
	}
	if result.Error != "" {
		return hlTPRearmResult{Status: hlTPRearmFailed, Detail: result.Error, Unknown: unknown}
	}
	var failures []string
	for i, e := range result.TPErrors {
		if e == "" || (i < len(result.TPOutcomeUnknown) && result.TPOutcomeUnknown[i]) {
			continue
		}
		failures = append(failures, fmt.Sprintf("tier %d: %s", i+1, e))
	}
	for i, force := range plan.ForceTPReplace {
		if !force || i >= len(plan.TPOIDs) || plan.TPOIDs[i] <= 0 {
			continue
		}
		if i < len(result.TPErrors) && result.TPErrors[i] != "" {
			continue
		}
		if i < len(result.TPFilledExternally) && result.TPFilledExternally[i] {
			continue
		}
		if i < len(result.TPOutcomeUnknown) && result.TPOutcomeUnknown[i] {
			continue
		}
		after := plan.TPOIDs[i]
		if i < len(result.TPOIDs) {
			after = result.TPOIDs[i]
		}
		if after == plan.TPOIDs[i] {
			failures = append(failures, fmt.Sprintf("tier %d (OID=%d) still rests at its pre-close size", i+1, plan.TPOIDs[i]))
		}
	}
	if len(result.TPCancelFailedOIDs) > 0 {
		reason := "the cancel was rejected"
		if result.OpenOrderCheckError != "" {
			reason = fmt.Sprintf("the open orders could not be read (%s)", result.OpenOrderCheckError)
		}
		failures = append(failures, fmt.Sprintf("take-profit OIDs %v were not removed: %s", result.TPCancelFailedOIDs, reason))
	}
	if len(failures) > 0 {
		return hlTPRearmResult{Status: hlTPRearmFailed, Detail: strings.Join(failures, "; "), Unknown: unknown}
	}
	if unknown != "" {
		return hlTPRearmResult{Status: hlTPRearmUnknown, Detail: unknown}
	}
	for i, oid := range result.TPOIDs {
		if oid <= 0 {
			continue
		}
		if i >= len(plan.TPOIDs) || plan.TPOIDs[i] != oid {
			return hlTPRearmResult{Status: hlTPRearmPlaced}
		}
	}
	if len(plan.CancelTPOIDs) > 0 {
		return hlTPRearmResult{Status: hlTPRearmRemoved, Detail: fmt.Sprintf("take-profit OIDs %v", plan.CancelTPOIDs)}
	}
	return hlTPRearmResult{Status: hlTPRearmKept}
}

func firstNonEmptyText(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func classifyStopRearmUpdate(owner string, qty, requestedTriggerPx float64, result *HyperliquidStopLossUpdateResult) hlStopRearmResult {
	out := hlStopRearmResult{Owner: owner, Qty: qty, TriggerPx: requestedTriggerPx}
	if result == nil {
		out.Status = hlStopRearmOutcomeUnknown
		out.Detail = hlStopRearmNoResultDetail
		return out
	}
	if result.StopLossTriggerPx > 0 {
		out.TriggerPx = result.StopLossTriggerPx
	}
	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		out.Status = hlStopRearmClosed
		out.Detail = "the re-armed stop filled at submit"
	case result.StopLossOID > 0:
		out.Status = hlStopRearmPlaced
		out.OID = result.StopLossOID
	case result.StopLossFilledExternally:
		out.Status = hlStopRearmClosed
		out.Detail = "the pre-close stop had already filled on-chain"
	case result.StopLossOutcomeUnknown:
		out.Status = hlStopRearmOutcomeUnknown
		out.Detail = firstNonEmptyText(result.StopLossError, result.Error, "the open-order diff was inconclusive")
	case result.OpenOrderCheckError != "":
		out.Status = hlStopRearmReadFailed
		out.Detail = result.OpenOrderCheckError
	case result.CancelStopLossError != "":
		out.Status = hlStopRearmPreCloseStopResting
		out.Detail = result.CancelStopLossError
	case result.CancelStopLossSucceeded:
		out.Status = hlStopRearmProtectionLost
		out.Detail = firstNonEmptyText(result.StopLossError, result.Error, "the replacement did not rest")
	default:
		out.Status = hlStopRearmPlacementFailed
		out.Detail = firstNonEmptyText(result.StopLossError, result.Error, "the replacement did not rest")
	}
	return out
}

func classifyProtectionSyncStopRearm(qty float64, protection *HyperliquidProtectionSyncResult) hlStopRearmResult {
	if protection == nil {
		return classifyStopRearmUpdate(hlRearmOwnerATR, qty, 0, nil)
	}
	return classifyStopRearmUpdate(hlRearmOwnerATR, qty, 0, &HyperliquidStopLossUpdateResult{
		Error:                     protection.Error,
		StopLossOID:               protection.StopLossOID,
		StopLossTriggerPx:         protection.StopLossTriggerPx,
		CancelStopLossSucceeded:   protection.CancelStopLossSucceeded,
		CancelStopLossError:       protection.CancelStopLossError,
		StopLossError:             protection.StopLossError,
		StopLossFilledImmediately: protection.StopLossFilledImmediately,
		StopLossFilledExternally:  protection.StopLossFilledExternally,
		StopLossOutcomeUnknown:    protection.StopLossOutcomeUnknown,
		OpenOrderCheckError:       protection.OpenOrderCheckError,
	})
}

func rearmProtectionForCloseRemainder(sc StrategyConfig, stratState *StrategyState, db *StateDB, symbol string, price float64, prevStopOID int64, prevTriggerPx, prevHighWater float64, reconcileFillHintsJSON []byte, liqPxByCoin map[string]float64, netSideByCoin map[string]string, u hlCloseUnconfirmed, stop hlCloseRemainderStop, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string, hlStopRearmResult, hlTPRearmResult) {
	if stratState == nil || symbol == "" {
		return 0, "", hlStopRearmResult{}, hlTPRearmResult{}
	}
	if stop.Unbacked || stop.Qty <= 0 {
		res := hlStopRearmResult{Status: hlStopRearmUnbacked, Owner: "stop"}
		res.Removal = removeHLCloseUnconfirmedOrders(sc, stratState, symbol, u, reconcileFillHintsJSON, mu, logger)
		return 0, "", res, hlTPRearmResult{}
	}
	trades := 0
	detail := ""
	var claimed []hlStopRearmResult
	_, fillPx, syncRes, tpRes := runHyperliquidProtectionSyncForRemainder(sc, stratState, db, symbol, mu, notifier, logger, "HL protection re-armed after close", reconcileFillHintsJSON, liqPxByCoin, netSideByCoin, hlProtectionGuardStopLegAfterFailedClose, stop.Qty, stop.AfterFill, prevStopOID, u)
	if fillPx > 0 {
		trades++
		detail = fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	claimed = append(claimed, syncRes)
	extraTrades, slDetail, trailRes := rearmTrailingStopAfterFailedClose(sc, stratState, symbol, price, prevStopOID, prevTriggerPx, prevHighWater, liqPxByCoin, netSideByCoin, stop, mu, logger)
	if extraTrades > 0 {
		trades += extraTrades
		detail = slDetail
	}
	claimed = append(claimed, trailRes)
	extraTrades, slDetail, scalarRes := rearmScalarStopAfterFailedClose(sc, stratState, symbol, prevStopOID, prevTriggerPx, liqPxByCoin, netSideByCoin, stop, mu, logger)
	if extraTrades > 0 {
		trades += extraTrades
		detail = slDetail
	}
	claimed = append(claimed, scalarRes)
	return trades, detail, pickStopRearmResult(claimed, prevStopOID), tpRes
}

func removeHLStopByOID(script, symbol, side string, oid int64, logger *StrategyLogger) hlStopRearmResult {
	unlock := lockHyperliquidTrailingUpdate(symbol)
	result, stderr, err := runHyperliquidUpdateStopLossFunc(script, symbol, side, 0, 0, oid)
	unlock()
	if stderr != "" && logger != nil {
		logger.Info("pre-close stop removal stderr: %s", stderr)
	}
	if err != nil && logger != nil {
		logger.Error("pre-close stop OID=%d removal for %s failed: %v", oid, symbol, err)
	}
	out := classifyStopRearmRemoval(result)
	out.OID = oid
	return out
}

func removeHLTakeProfitsByOID(sc StrategyConfig, symbol, side string, avgCost, entryATR float64, oids []int64, reconcileFillHintsJSON []byte, logger *StrategyLogger) hlCloseRemovalReport {
	var rep hlCloseRemovalReport
	if len(oids) == 0 {
		return rep
	}
	plan := hlProtectionPlan{Symbol: symbol, Side: side, AvgCost: avgCost, EntryATR: entryATR, CancelTPOIDs: cloneInt64s(oids)}
	unlock := lockHyperliquidProtectionSync(symbol)
	result, ok := syncHyperliquidProtection(sc, plan, nil, logger, reconcileFillHintsJSON)
	unlock()
	var err error
	if !ok && result != nil && result.Error == "" {
		err = fmt.Errorf("the take-profit removal exited with an error; see the strategy log")
	}
	return classifyHLTakeProfitRemoval(oids, result, err)
}

func classifyHLTakeProfitRemoval(oids []int64, result *HyperliquidProtectionSyncResult, err error) hlCloseRemovalReport {
	var rep hlCloseRemovalReport
	switch {
	case result == nil || (err != nil && result.Error == ""):
		rep.Unverified = cloneInt64s(oids)
		detail := hlStopRearmNoResultDetail
		if err != nil {
			detail = err.Error()
		}
		rep.addDetail("take-profit removal: " + detail)
		return rep
	case result.Error != "":
		rep.Unverified = cloneInt64s(oids)
		rep.addDetail("take-profit removal: " + result.Error)
		return rep
	}
	for _, oid := range oids {
		switch {
		case containsInt64(result.TPCancelFilledOIDs, oid):
			rep.Filled = append(rep.Filled, oid)
		case containsInt64(result.TPCancelFailedOIDs, oid) && result.OpenOrderCheckError != "":
			rep.Unverified = append(rep.Unverified, oid)
		case containsInt64(result.TPCancelFailedOIDs, oid):
			rep.Resting = append(rep.Resting, oid)
		default:
			rep.Removed = append(rep.Removed, oid)
		}
	}
	if result.OpenOrderCheckError != "" && len(rep.Unverified) > 0 {
		rep.addDetail("take-profit removal: the open orders could not be read (" + result.OpenOrderCheckError + ")")
	}
	return rep
}

func removeHLCloseUnconfirmedOrders(sc StrategyConfig, stratState *StrategyState, symbol string, u hlCloseUnconfirmed, reconcileFillHintsJSON []byte, mu *sync.RWMutex, logger *StrategyLogger) hlCloseRemovalReport {
	rep := hlCloseRemovalReport{StopOID: u.StopOID}
	if u.StopOID <= 0 && len(u.TPOIDs) == 0 {
		return rep
	}
	var side string
	var avgCost, entryATR float64
	mu.RLock()
	if pos := stratState.Positions[symbol]; pos != nil {
		side, avgCost, entryATR = pos.Side, pos.AvgCost, pos.EntryATR
	}
	mu.RUnlock()
	if side == "" {
		if u.StopOID > 0 {
			rep.Unverified = append(rep.Unverified, u.StopOID)
		}
		rep.Unverified = append(rep.Unverified, u.TPOIDs...)
		rep.addDetail("the position is no longer in the book, so no pre-close order was verified")
		return rep
	}
	if u.StopOID > 0 {
		stopRes := removeHLStopByOID(sc.Script, symbol, side, u.StopOID, logger)
		switch stopRes.Status {
		case hlStopRearmRemoved:
			rep.Removed = append(rep.Removed, u.StopOID)
			mu.Lock()
			if pos := stratState.Positions[symbol]; pos != nil && pos.StopLossOID == u.StopOID {
				pos.StopLossOID = 0
				pos.StopLossTriggerPx = 0
			}
			mu.Unlock()
		case hlStopRearmClosed:
			rep.Filled = append(rep.Filled, u.StopOID)
		case hlStopRearmPreCloseStopResting:
			rep.Resting = append(rep.Resting, u.StopOID)
			rep.addDetail(fmt.Sprintf("stop OID=%d cancel: %s", u.StopOID, stopRes.Detail))
		default:
			rep.Unverified = append(rep.Unverified, u.StopOID)
			rep.addDetail(fmt.Sprintf("stop OID=%d: %s", u.StopOID, stopRes.Detail))
		}
	}
	if len(u.TPOIDs) > 0 {
		tp := removeHLTakeProfitsByOID(sc, symbol, side, avgCost, entryATR, u.TPOIDs, reconcileFillHintsJSON, logger)
		rep.Removed = append(rep.Removed, tp.Removed...)
		rep.Filled = append(rep.Filled, tp.Filled...)
		rep.Resting = append(rep.Resting, tp.Resting...)
		rep.Unverified = append(rep.Unverified, tp.Unverified...)
		rep.addDetail(tp.Detail)
		mu.Lock()
		if pos := stratState.Positions[symbol]; pos != nil {
			clearHyperliquidProtectionOIDsMatching(pos, tp.Removed)
			for idx, oid := range pos.TPOIDs {
				if oid > 0 && containsInt64(tp.Filled, oid) {
					pos.TPOIDs[idx] = 0
					if idx < len(pos.TPArmedTiers) {
						pos.TPArmedTiers[idx] = true
					}
				}
			}
		}
		mu.Unlock()
	}
	return rep
}

func pickStopRearmResult(results []hlStopRearmResult, prevStopOID int64) hlStopRearmResult {
	var first *hlStopRearmResult
	for i := range results {
		r := results[i]
		if r.Status == hlStopRearmUnclaimed {
			continue
		}
		if r.failed() {
			return r
		}
		if first == nil {
			first = &results[i]
		}
	}
	if first != nil {
		return *first
	}
	if prevStopOID > 0 {
		return hlStopRearmResult{Status: hlStopRearmNoArm, Owner: "stop"}
	}
	return hlStopRearmResult{Status: hlStopRearmUnclaimed}
}

func manualRecordedStopOwner(sc StrategyConfig, pos *Position) bool {
	if sc.Type != "manual" || pos == nil || effectiveTrailingStopPct(sc, pos) > 0 {
		return false
	}
	probe := *pos
	probe.StopLossOID = 0
	probe.StopLossTriggerPx = 0
	plan, ok := buildHyperliquidProtectionPlan(sc, &probe, 0)
	return !ok || plan.StopLossATRMult <= 0
}

func rearmScalarStopAfterFailedClose(sc StrategyConfig, stratState *StrategyState, symbol string, prevStopOID int64, prevTriggerPx float64, liqPxByCoin map[string]float64, netSideByCoin map[string]string, stop hlCloseRemainderStop, mu *sync.RWMutex, logger *StrategyLogger) (int, string, hlStopRearmResult) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" {
		return 0, "", hlStopRearmResult{}
	}
	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) > 0 {
		mu.RUnlock()
		return 0, "", hlStopRearmResult{}
	}
	pctOwner := EffectiveStopLossPct(sc) > 0
	recordedOwner := !pctOwner && manualRecordedStopOwner(sc, pos)
	side := pos.Side
	anchor := pos.riskAnchorPrice()
	slEffectiveQty := stop.Qty
	capped := stop.Qty < stop.Remainder-hlSharedCloseQtyTolerance
	cancelOID := pos.StopLossOID
	if cancelOID <= 0 {
		cancelOID = prevStopOID
	}
	recordedTrigger := pos.StopLossTriggerPx
	if recordedTrigger <= 0 {
		recordedTrigger = prevTriggerPx
	}
	mu.RUnlock()
	if !pctOwner && !recordedOwner {
		return 0, "", hlStopRearmResult{}
	}

	liqPx := hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, symbol, side)
	var triggerPx float64
	ownerLabel := hlRearmOwnerPercent
	if pctOwner {
		triggerPx = hlLiquidationScalarRearmTriggerPx(sc, side, anchor, liqPx)
	} else {
		ownerLabel = hlRearmOwnerRecorded
		if recordedTrigger <= 0 {
			if cancelOID > 0 {
				logger.Error("CRITICAL: close %s cancelled the %s (oid=%d) and the book holds no trigger to restore — the position has NO exchange-side stop", symbol, ownerLabel, cancelOID)
				return 0, "", hlStopRearmResult{Status: hlStopRearmNoTrigger, Owner: ownerLabel, Qty: slEffectiveQty}
			}
			return 0, "", hlStopRearmResult{}
		}
		triggerPx = recordedTrigger
		if clamped, ok := clampStopInsideLiquidation(side, triggerPx, liqPx); ok {
			triggerPx = clamped
		}
	}
	if triggerPx <= 0 {
		logger.Error("CRITICAL: close %s cancelled the %s and no re-arm trigger could be resolved (side=%q anchor=$%.4f) — the position has NO exchange-side stop", symbol, ownerLabel, side, anchor)
		return 0, "", hlStopRearmResult{Status: hlStopRearmNoTrigger, Owner: ownerLabel, Qty: slEffectiveQty}
	}
	if capped {
		logger.Warn("close-remainder %s re-arm for %s: %.6f of the %.6f remainder is backed on-chain; the stop is sized to the backed units", ownerLabel, symbol, slEffectiveQty, stop.Remainder)
	}
	logger.Warn("Close %s cancelled (or may have cancelled) its on-chain %s (oid=%d); re-arming %.6f at $%.4f with the old oid verified on-chain before any cancel", symbol, ownerLabel, cancelOID, slEffectiveQty, triggerPx)
	candidate := hlLiquidationAuditCandidate{
		StrategyID:  sc.ID,
		Script:      sc.Script,
		Symbol:      symbol,
		Side:        side,
		Qty:         slEffectiveQty,
		VirtualQty:  stop.Remainder,
		QtyCapped:   capped,
		StopLossOID: cancelOID,
	}
	result, _ := hlLiquidationClampReplace(candidate, triggerPx, logger)
	outcome := classifyStopRearmUpdate(ownerLabel, slEffectiveQty, triggerPx, result)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, cancelOID, 0, true, result, "stop_loss_pct_immediate", logger, slEffectiveQty); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE PERCENTAGE SL %s @ $%.2f", sc.ID, symbol, fillPx), outcome
	}
	if outcome.Status == hlStopRearmPlaced {
		logger.Info("%s re-armed after close for %s (qty=%.6f trigger=$%.4f)", ownerLabel, symbol, slEffectiveQty, result.StopLossTriggerPx)
	}
	return 0, "", outcome
}

func rearmTrailingStopAfterFailedClose(sc StrategyConfig, stratState *StrategyState, symbol string, mark float64, prevStopOID int64, prevTriggerPx, prevHighWater float64, liqPxByCoin map[string]float64, netSideByCoin map[string]string, stop hlCloseRemainderStop, mu *sync.RWMutex, logger *StrategyLogger) (int, string, hlStopRearmResult) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" {
		return 0, "", hlStopRearmResult{}
	}
	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, "", hlStopRearmResult{}
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	if highWater <= 0 {
		highWater = prevHighWater
	}
	triggerPx := pos.StopLossTriggerPx
	if triggerPx <= 0 {
		triggerPx = prevTriggerPx
	}
	cancelOID := pos.StopLossOID
	if cancelOID <= 0 {
		cancelOID = prevStopOID
	}
	posSnap := *pos
	mu.RUnlock()

	slEffectiveQty := stop.Qty
	if mark <= 0 {
		logger.Error("CRITICAL: close %s left the trailing stop to re-arm but no mark price is known, so no trigger can be set — the position has NO exchange-side stop", symbol)
		return 0, "", hlStopRearmResult{Status: hlStopRearmNoTrigger, Owner: hlRearmOwnerTrailing, Qty: slEffectiveQty, TriggerPx: triggerPx}
	}
	if slEffectiveQty < stop.Remainder-hlSharedCloseQtyTolerance {
		logger.Warn("failed-close trailing SL re-arm for %s: %.6f of the %.6f remainder is backed on-chain; the stop is sized to the backed units", symbol, slEffectiveQty, stop.Remainder)
	}
	placedQty := slEffectiveQty
	logger.Warn("Failed close %s cancelled its on-chain stop (oid=%d); re-arming the trailing SL from high-water $%.4f with the old oid verified on-chain before any cancel", symbol, cancelOID, highWater)
	policy := trailingReplacePolicy{forceResize: true, liquidationPx: hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, symbol, side)}
	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, cancelOID, policy, nil, logger)
	outcome := classifyStopRearmUpdate(hlRearmOwnerTrailing, slEffectiveQty, triggerPx, slUpdate)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, cancelOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, placedQty); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx), outcome
	}
	if updateConfirmed && slUpdate != nil && slUpdate.StopLossOID > 0 {
		logger.Info("Trailing SL re-armed after failed close for %s (qty=%.6f high_water=$%.4f trigger=$%.4f)", symbol, slEffectiveQty, newHighWater, slUpdate.StopLossTriggerPx)
	}
	return 0, "", outcome
}
