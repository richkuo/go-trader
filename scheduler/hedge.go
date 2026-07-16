package main

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Correlated hedge legs (#1159 phase 1, HL perps only).
//
// Design: hedge management is a per-cycle, state-derived reconciler ("hedge
// sync"), not scattered per-event mirror hooks. hedgeTargetDecision computes
// the hedge action from the CURRENT primary position vs. the persisted
// Position.HedgePrimaryQtyBasis watermark, and runHedgeSync converges the
// hedge leg to it once per HL dispatch cycle. Every primary lifecycle event —
// fresh open, scale-in add, evaluator partial close, on-chain SL/TP fill
// booked by reconcile, external close — therefore produces the matching hedge
// action within the same or next cycle without touching each close path
// individually. Events that bypass the dispatch loop (portfolio kill switch,
// per-strategy CB) have explicit extensions in hyperliquid_balance.go/risk.go.
//
// Invariants:
//   - Qty-event mirroring, not price mirroring: the watermark keys the target
//     to primary QUANTITY, so mark drift never re-trades the hedge.
//   - Fill-confirmed state mutation only (live-exec guard): hedge virtual
//     state mutates only from confirmed fills.
//   - Fail-closed open: primary fill confirmed + hedge open failed on the
//     opening cycle → reduce-only close of the primary + CRITICAL owner DM —
//     never run unhedged silently (constraint 4).
//   - Sole ownership by construction: config validation guarantees a hedge
//     coin is never any strategy's configured coin and never shared between
//     hedgers (validateHedgeConfigs).

// hedgeQtyEpsilon is the float guard for primary-qty watermark comparisons.
const hedgeQtyEpsilon = 1e-9

// hedgeMinOrderNotionalUSD defers hedge REDUCE orders whose notional is below
// HL's minimum order size (~$10): the basis is deliberately not advanced so
// the reduce retries/accumulates on later cycles. Full closes always execute.
const hedgeMinOrderNotionalUSD = 10.0

type hedgeActionKind int

const (
	hedgeActionNone hedgeActionKind = iota
	hedgeActionOpen
	hedgeActionAdd
	hedgeActionReduce
	hedgeActionCloseFull
)

func (k hedgeActionKind) String() string {
	switch k {
	case hedgeActionOpen:
		return "open"
	case hedgeActionAdd:
		return "add"
	case hedgeActionReduce:
		return "reduce"
	case hedgeActionCloseFull:
		return "close_full"
	}
	return "none"
}

// hedgeSnapshot is the Phase-1 (RLock) view hedge decisions are computed from.
type hedgeSnapshot struct {
	PrimaryQty     float64
	PrimarySide    string
	HedgeQty       float64
	HedgeSide      string
	HedgeBasis     float64 // Position.HedgePrimaryQtyBasis of the held hedge leg
	HedgeExists    bool
	HedgeCancelOID int64 // hedge legs carry no SL/TP by design; kept for defense-in-depth
}

// hedgeAction is one converging step toward the hedge target.
type hedgeAction struct {
	Kind hedgeActionKind
	// Qty is the hedge-coin quantity to trade (order size for open/add,
	// reduce-only size for reduce/close_full).
	Qty float64
	// PosSide is the hedge POSITION side for open ("long"/"short").
	PosSide string
	// PrimaryDelta is the primary-qty change this action mirrors (open/add:
	// qty above basis; reduce: qty below basis) — used to advance the
	// watermark proportionally to what actually fills.
	PrimaryDelta float64
	Reason       string
}

// hedgePositionSideForPrimary returns the inverse hedge side for a primary
// position side ("" when the primary side is unknown).
func hedgePositionSideForPrimary(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	}
	return ""
}

// hedgeOrderSideForPositionSide maps a hedge position side to the order side
// that opens/adds to it.
func hedgeOrderSideForPositionSide(posSide string) string {
	if posSide == "short" {
		return "sell"
	}
	return "buy"
}

// hedgeTargetDecision computes the next converging hedge action from a state
// snapshot. Pure — no I/O, no locks, no time. primaryPx/hedgePx are the
// current marks; an unusable price (≤ 0) when an order WOULD be needed fails
// closed with Kind=none and a non-empty Reason for the caller to escalate.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{}
	}
	primaryHeld := snap.PrimaryQty > 0
	hedgeHeld := snap.HedgeExists && snap.HedgeQty > 0

	if !primaryHeld {
		if hedgeHeld {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary flat — closing hedge leg"}
		}
		return hedgeAction{}
	}

	wantSide := hedgePositionSideForPrimary(snap.PrimarySide)
	if wantSide == "" {
		return hedgeAction{Reason: fmt.Sprintf("primary side %q unresolvable — holding hedge action", snap.PrimarySide)}
	}

	if hedgeHeld && snap.HedgeSide != wantSide {
		// Defense-in-depth: unreachable while direction "both" is rejected,
		// but a wrong-side hedge must flatten before anything else.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: fmt.Sprintf("hedge side %s conflicts with target %s — closing", snap.HedgeSide, wantSide)}
	}

	if !hedgeHeld {
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Reason: fmt.Sprintf("unusable marks (primary $%.4f hedge $%.4f) — hedge open refused (fail-closed)", primaryPx, hedgePx)}
		}
		qty := snap.PrimaryQty * primaryPx * hedgeRatio(sc) / hedgePx
		if qty <= 0 {
			return hedgeAction{Reason: "hedge open size resolved to zero — refused (fail-closed)"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, PosSide: wantSide, PrimaryDelta: snap.PrimaryQty}
	}

	basis := snap.HedgeBasis
	if basis <= 0 {
		// Legacy/unstamped watermark: adopt the current primary qty rather than
		// guessing a delta trade. The apply path always stamps the basis.
		return hedgeAction{Reason: "hedge basis unstamped — adopting current primary qty as watermark"}
	}

	switch {
	case snap.PrimaryQty > basis+hedgeQtyEpsilon:
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Reason: fmt.Sprintf("unusable marks (primary $%.4f hedge $%.4f) — hedge add deferred", primaryPx, hedgePx)}
		}
		delta := snap.PrimaryQty - basis
		qty := delta * primaryPx * hedgeRatio(sc) / hedgePx
		if qty <= 0 {
			return hedgeAction{Reason: "hedge add size resolved to zero — deferred"}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, PosSide: wantSide, PrimaryDelta: delta}
	case snap.PrimaryQty < basis-hedgeQtyEpsilon:
		fraction := (basis - snap.PrimaryQty) / basis
		if fraction >= 1-hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, PrimaryDelta: basis}
		}
		qty := snap.HedgeQty * fraction
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		if qty <= 0 {
			return hedgeAction{Reason: "hedge reduce size resolved to zero — deferred"}
		}
		if hedgePx > 0 && qty*hedgePx < hedgeMinOrderNotionalUSD {
			// Dust deferral: basis intentionally NOT advanced so the reduce
			// accumulates and retries once it clears HL's min order size.
			return hedgeAction{Reason: fmt.Sprintf("hedge reduce notional $%.2f below $%.2f min — deferred (accumulates)", qty*hedgePx, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty, PrimaryDelta: basis - snap.PrimaryQty}
	}
	return hedgeAction{}
}

// hedgeOrderSkipReason is the pre-spawn skip-reason mirror (repo rule: re-check
// the decision preconditions immediately before spawning, so an on-chain fill
// can never land without a Trade record). Returns "" when the order may spawn.
// onChain is the signed on-chain size of the hedge coin (0 = flat/unknown);
// onChainKnown reports whether the positions snapshot covered the coin.
func hedgeOrderSkipReason(action hedgeAction, snap hedgeSnapshot, onChain float64, onChainKnown bool) string {
	switch action.Kind {
	case hedgeActionNone:
		return "no hedge action"
	case hedgeActionOpen:
		if action.Qty <= 0 {
			return "hedge open qty <= 0"
		}
		// Fail closed on a foreign position: an on-chain position on the
		// declared hedge coin with NO virtual hedge leg means something else
		// (manual trade, another system) owns exposure there — opening into it
		// would entangle the strategy with a position it can never account for.
		if onChainKnown && math.Abs(onChain) > 1e-9 && !snap.HedgeExists {
			return fmt.Sprintf("foreign on-chain position (%.6f) on declared hedge coin with no virtual hedge leg — not trading (fail-closed)", onChain)
		}
	case hedgeActionAdd:
		if action.Qty <= 0 {
			return "hedge add qty <= 0"
		}
		if !snap.HedgeExists {
			return "hedge add without a held hedge leg"
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if action.Qty <= 0 {
			return "hedge close qty <= 0"
		}
		if !snap.HedgeExists {
			return "hedge close without a held hedge leg"
		}
	}
	return ""
}

// hedgeAdvancedBasis returns the new watermark after `filled` of `requested`
// hedge qty executed for an action that mirrors primaryDelta. Partial fills
// advance the basis proportionally so the next cycle converges the remainder.
func hedgeAdvancedBasis(prevBasis, primaryDelta, requested, filled float64, kind hedgeActionKind) float64 {
	if requested <= 0 {
		return prevBasis
	}
	frac := filled / requested
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	switch kind {
	case hedgeActionOpen, hedgeActionAdd:
		return prevBasis + primaryDelta*frac
	case hedgeActionReduce:
		b := prevBasis - primaryDelta*frac
		if b < 0 {
			b = 0
		}
		return b
	}
	return prevBasis
}

// applyHedgeOpenFill books a hedge open/add fill under mu.Lock. For a fresh
// open it creates the hedge Position (stamping HedgeFor + the qty watermark);
// for an add it blends price/size and advances the watermark. useFillFee=false
// (paper) books the modeled taker fee. Returns the number of trades booked.
func applyHedgeOpenFill(s *StrategyState, sc StrategyConfig, primarySym string, action hedgeAction, fillQty, fillPx, fillFee float64, useFillFee bool, oid string, logger *StrategyLogger) int {
	hc := hedgeCoin(sc)
	if hc == "" || fillQty <= 0 || fillPx <= 0 {
		return 0
	}
	fee := fillFee
	feeSource := FeeSourceUserFills
	if !useFillFee {
		fee = CalculatePlatformSpotFee(s.Platform, fillQty*fillPx)
		feeSource = FeeSourceModeled
	}
	now := time.Now().UTC()
	pos, held := s.Positions[hc]
	var positionID string
	side := "long"
	if held && pos != nil {
		// Add leg: blend price/size like a scale-in; the hedge has no frozen
		// geometry to preserve.
		totalQty := pos.Quantity + fillQty
		pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*fillQty) / totalQty
		pos.Quantity = totalQty
		pos.HedgePrimaryQtyBasis = hedgeAdvancedBasis(pos.HedgePrimaryQtyBasis, action.PrimaryDelta, action.Qty, fillQty, hedgeActionAdd)
		positionID = ensurePositionTradeID(s.ID, hc, pos)
		side = pos.Side
	} else {
		side = action.PosSide
		positionID = newTradePositionID(s.ID, hc, now)
		s.Positions[hc] = &Position{
			Symbol:               hc,
			Quantity:             fillQty,
			InitialQuantity:      fillQty,
			AvgCost:              fillPx,
			Side:                 side,
			Multiplier:           1, // perps PnL-branch convention
			Leverage:             hedgeExchangeLeverage(sc),
			OwnerStrategyID:      s.ID,
			OpenedAt:             now,
			TradePositionID:      positionID,
			HedgeFor:             primarySym,
			HedgePrimaryQtyBasis: hedgeAdvancedBasis(0, action.PrimaryDelta, action.Qty, fillQty, hedgeActionOpen),
		}
	}
	s.Cash -= fee // margin stays virtual; only the fee leaves cash (perps convention)
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hc,
		PositionID:      positionID,
		Side:            hedgeOrderSideForPositionSide(side),
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.4f (ratio %.2f, fee $%.2f)", primarySym, action.Kind, side, fillQty, fillPx, hedgeRatio(sc), fee),
		ExchangeOrderID: oid,
		ExchangeFee:     fee,
		FeeSource:       feeSource,
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("Hedge %s: %s %s %.6f @ $%.4f (for %s, fee $%.2f)", action.Kind, side, hc, fillQty, fillPx, primarySym, fee)
	}
	return 1
}

// applyHedgeCloseFill books a hedge reduce/close fill under mu.Lock via the
// shared perps close bookers (dup-OID guard + corrupt-position handling for
// free; trade_type "hedge" via perpsTradeTypeForPosition). For a reduce, the
// watermark advances proportionally to the filled fraction.
func applyHedgeCloseFill(s *StrategyState, sc StrategyConfig, action hedgeAction, fillQty, fillPx, fillFee float64, useFillFee bool, oid string, logger *StrategyLogger) int {
	hc := hedgeCoin(sc)
	pos := s.Positions[hc]
	if hc == "" || pos == nil || fillQty <= 0 || fillPx <= 0 {
		return 0
	}
	primarySym := pos.HedgeFor
	if action.Kind == hedgeActionReduce && fillQty < pos.Quantity-hedgeQtyEpsilon {
		newBasis := hedgeAdvancedBasis(pos.HedgePrimaryQtyBasis, action.PrimaryDelta, action.Qty, fillQty, hedgeActionReduce)
		if bookPerpsPartialCloseWithFillFee(s, hc, fillQty, fillPx, fillFee, useFillFee, oid, "hedge_reduce",
			fmt.Sprintf("hedge(%s) reduce", primarySym), "Hedge reduce", logger) {
			if p := s.Positions[hc]; p != nil {
				p.HedgePrimaryQtyBasis = newBasis
			}
			return 1
		}
		return 0
	}
	if bookPerpsCloseWithFillFee(s, hc, fillPx, fillFee, useFillFee, oid, "hedge_close",
		fmt.Sprintf("hedge(%s) close", primarySym), "Hedge close", logger) {
		return 1
	}
	return 0
}

// runHedgeSync converges a hedge-enabled strategy's hedge leg to the target
// implied by its current primary position. Called once per HL perps dispatch
// cycle (after the execute/apply block) — deliberately NOT gated by pause /
// daily-loss / exposure-cap holds: hedge orders are coupled risk-management
// legs, not signals, and under those holds the primary cannot increase, so
// the sync can only reduce/close anyway. Lock discipline mirrors
// runHyperliquidProtectionSync: snapshot under RLock, spawn unlocked, apply
// under Lock. freshOpenCycle marks a cycle whose primary open/add just filled:
// a hedge-open failure on that cycle escalates to the fail-closed primary
// unwind (constraint 4); on later reconcile-drift cycles it alerts + retries.
// Returns trades booked.
func runHedgeSync(
	sc StrategyConfig,
	stratState *StrategyState,
	primarySym string,
	primaryPx float64,
	prices map[string]float64,
	hlPositions []HLPosition,
	freshOpenCycle bool,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	if !HedgeEnabled(sc) || sc.Type != "perps" || sc.Platform != "hyperliquid" || stratState == nil || primarySym == "" {
		return 0
	}
	hc := hedgeCoin(sc)
	if hc == "" {
		return 0
	}
	live := hyperliquidIsLive(sc.Args)

	// Phase 1: snapshot under RLock.
	mu.RLock()
	var snap hedgeSnapshot
	if pos, ok := stratState.Positions[primarySym]; ok && pos != nil {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	if hpos, ok := stratState.Positions[hc]; ok && hpos != nil {
		snap.HedgeExists = true
		snap.HedgeQty = hpos.Quantity
		snap.HedgeSide = hpos.Side
		snap.HedgeBasis = hpos.HedgePrimaryQtyBasis
	}
	mu.RUnlock()

	hedgePx := prices[hc]
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)

	if action.Kind == hedgeActionNone {
		if action.Reason == "" {
			return 0
		}
		// A refused OPEN on the opening cycle is the fail-closed trigger:
		// never let the primary run unhedged because the hedge mark was
		// unavailable (constraint 4).
		if freshOpenCycle && snap.PrimaryQty > 0 && !snap.HedgeExists {
			logger.Warn("Hedge sync: %s — unwinding primary %s (fail-closed, #1159)", action.Reason, primarySym)
			return unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, primaryPx, action.Reason, mu, notifier, logger)
		}
		logger.Warn("Hedge sync: %s (retrying next cycle)", action.Reason)
		return 0
	}

	// Adopt-watermark repair path (no order): stamp the basis when unstamped.
	// Handled by decision Reason above; nothing else to do here.

	var onChain float64
	var onChainKnown bool
	for i := range hlPositions {
		if hlPositions[i].Coin == hc {
			onChain = hlPositions[i].Size
			onChainKnown = true
			break
		}
	}

	if live {
		if reason := hedgeOrderSkipReason(action, snap, onChain, onChainKnown); reason != "" {
			logger.Warn("Hedge sync: order skipped for %s: %s", hc, reason)
			if action.Kind == hedgeActionOpen {
				msg := fmt.Sprintf("🛑 [%s] hedge open on %s refused: %s", sc.ID, hc, reason)
				notifier.SendOwnerDM(msg)
				if freshOpenCycle && snap.PrimaryQty > 0 {
					return unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, primaryPx, reason, mu, notifier, logger)
				}
			}
			return 0
		}
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		if !live {
			mu.Lock()
			trades := applyHedgeOpenFill(stratState, sc, primarySym, action, action.Qty, hedgePx, 0, false, "", logger)
			mu.Unlock()
			return trades
		}
		orderSide := hedgeOrderSideForPositionSide(action.PosSide)
		// #486 mirror: margin mode + leverage only on a fresh hedge open (HL
		// rejects update_leverage on an open position), from the hedge block —
		// never the primary's (constraint 3).
		marginMode := ""
		leverage := 0.0
		if action.Kind == hedgeActionOpen && sc.Hedge.MarginMode != "" {
			marginMode = sc.Hedge.MarginMode
			leverage = hedgeExchangeLeverage(sc)
		}
		execResult, stderr, err := RunHyperliquidExecute(sc.Script, hc, orderSide, action.Qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshotForCoin(hlPositions, hc))
		if stderr != "" {
			logger.Info("hedge execute stderr: %s", stderr)
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else if execResult != nil && execResult.Error != "" {
			errMsg = execResult.Error
		} else if execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil || execResult.Execution.Fill.TotalSz <= 0 {
			errMsg = "hedge order returned no fill"
		}
		if errMsg != "" {
			logger.Error("Hedge %s failed for %s: %s", action.Kind, hc, errMsg)
			notifyLiveExecFailure(notifier, sc, "hedge-"+action.Kind.String(), hc, errMsg)
			// Only a failed fresh OPEN (position fully unhedged) escalates to
			// the primary unwind. A failed ADD leaves an existing, merely
			// undersized hedge — bounded exposure; alert + retry next cycle.
			if freshOpenCycle && action.Kind == hedgeActionOpen {
				return unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, primaryPx, errMsg, mu, notifier, logger)
			}
			return 0
		}
		clearLiveExecThrottle(sc, "hedge-"+action.Kind.String(), hc)
		fill := execResult.Execution.Fill
		oid := ""
		if fill.OID > 0 {
			oid = fmt.Sprintf("%d", fill.OID)
		}
		mu.Lock()
		trades := applyHedgeOpenFill(stratState, sc, primarySym, action, fill.TotalSz, fill.AvgPx, fill.Fee, true, oid, logger)
		mu.Unlock()
		return trades

	case hedgeActionReduce, hedgeActionCloseFull:
		if !live {
			mu.Lock()
			trades := applyHedgeCloseFill(stratState, sc, action, action.Qty, hedgePx, 0, false, "", logger)
			mu.Unlock()
			return trades
		}
		// Always a SIZED reduce-only close. Collision validation makes the
		// hedge coin sole-owned, but sized closes stay correct even if that
		// invariant is ever violated. Cap to the on-chain size when known so a
		// virtual>on-chain drift can't over-close into a fresh opposite position.
		sz := action.Qty
		if onChainKnown {
			if absOC := math.Abs(onChain); absOC > 0 && sz > absOC {
				sz = absOC
			}
		}
		if sz <= 1e-12 {
			logger.Warn("Hedge sync: %s close size resolved to 0 (on-chain flat?) — reconcile will resync the virtual leg", hc)
			return 0
		}
		closeResult, stderr, err := RunHyperliquidClose(sc.Script, hc, &sz, nil)
		if stderr != "" {
			logger.Info("hedge close stderr: %s", stderr)
		}
		if err != nil {
			logger.Error("Hedge %s failed for %s: %v", action.Kind, hc, err)
			notifyLiveExecFailure(notifier, sc, "hedge-"+action.Kind.String(), hc, err.Error())
			return 0
		}
		clearLiveExecThrottle(sc, "hedge-"+action.Kind.String(), hc)
		if closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
			if closeResult != nil && closeResult.Close != nil && closeResult.Close.AlreadyFlat {
				logger.Warn("Hedge sync: %s already flat on-chain — reconcile will resync the virtual leg", hc)
			} else {
				logger.Warn("Hedge sync: %s close returned no fill — retrying next cycle", hc)
			}
			return 0
		}
		fill := closeResult.Close.Fill
		oid := ""
		if fill.OID > 0 {
			oid = fmt.Sprintf("%d", fill.OID)
		}
		mu.Lock()
		trades := applyHedgeCloseFill(stratState, sc, action, fill.TotalSz, fill.AvgPx, fill.Fee, true, oid, logger)
		mu.Unlock()
		return trades
	}
	return 0
}

// unwindPrimaryAfterHedgeOpenFailure fail-closes a primary position whose
// hedge leg could not be opened on the opening cycle (constraint 4): a SIZED
// reduce-only close (never market_close(sz=None) — the primary coin may have
// shared-coin peers), cancelling the just-armed protection OIDs, booked with
// the real fill and reason "hedge_open_failed_unwind", plus a CRITICAL owner
// DM. If the unwind itself fails, state is left unchanged and the
// state-derived hedge sync self-heals next cycle (retry hedge open first, or
// unwind again) — restart-safe with no new latch state. Returns trades booked.
func unwindPrimaryAfterHedgeOpenFailure(
	sc StrategyConfig,
	stratState *StrategyState,
	primarySym string,
	primaryPx float64,
	why string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	mu.RLock()
	pos := stratState.Positions[primarySym]
	var qty float64
	var cancelOIDs []int64
	if pos != nil {
		qty = pos.Quantity
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()
	if qty <= 0 {
		return 0
	}
	alert := fmt.Sprintf("🛑 CRITICAL [%s] hedge open failed (%s) — unwinding primary %s %.6f (fail-closed, #1159 constraint 4)", sc.ID, why, primarySym, qty)
	notifier.SendOwnerDM(alert)
	notifier.SendToAllChannels(alert)

	if !hyperliquidIsLive(sc.Args) {
		mu.Lock()
		booked := bookPerpsClose(stratState, primarySym, primaryPx, "hedge_open_failed_unwind",
			"Hedge-open-failure unwind (paper)", "Hedge-open-failure unwind (paper)", logger)
		mu.Unlock()
		if booked {
			return 1
		}
		return 0
	}

	closeResult, stderr, err := RunHyperliquidCloseCancelAfterFill(sc.Script, primarySym, &qty, cancelOIDs)
	if stderr != "" {
		logger.Info("hedge unwind stderr: %s", stderr)
	}
	if err != nil {
		msg := fmt.Sprintf("🛑 CRITICAL [%s] hedge-open-failure UNWIND ALSO FAILED for %s: %v — position is running UNHEDGED; hedge sync retries next cycle", sc.ID, primarySym, err)
		logger.Error("%s", msg)
		notifier.SendOwnerDM(msg)
		return 0
	}
	if closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
		logger.Warn("Hedge unwind for %s returned no fill — reconcile/hedge sync will converge next cycle", primarySym)
		return 0
	}
	fill := closeResult.Close.Fill
	oid := ""
	if fill.OID > 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	booked := bookPerpsCloseWithFillFee(stratState, primarySym, fill.AvgPx, fill.Fee, true, oid,
		"hedge_open_failed_unwind", "Hedge-open-failure unwind", "Hedge-open-failure unwind", logger)
	mu.Unlock()
	if booked {
		return 1
	}
	return 0
}

// validateHedgeStateConsistency surfaces hedge state↔config drift at startup —
// the gap a config edit + restart opens past the SIGHUP hot-reload guard.
// Non-destructive fail-closed (mirrors the shared-coin ambiguity convention):
// the persisted hedge leg is left frozen and the operator warned; nothing is
// auto-closed or adopted. Returns sorted warning strings.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	var warnings []string
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		syms := make([]string, 0, len(ss.Positions))
		for sym := range ss.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		sc, hasCfg := byID[id]
		for _, sym := range syms {
			pos := ss.Positions[sym]
			if pos == nil || pos.HedgeFor == "" {
				continue
			}
			switch {
			case !hasCfg || !HedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("strategy[%s]: persisted hedge leg %s (for %s) but the config no longer enables a hedge — leg left frozen; re-enable the hedge block or flatten manually (#1159)", id, sym, pos.HedgeFor))
			case hedgeCoin(sc) != sym:
				warnings = append(warnings, fmt.Sprintf("strategy[%s]: persisted hedge leg %s (for %s) but hedge.symbol now resolves to %s — leg left frozen; restore the previous hedge symbol or flatten manually (#1159)", id, sym, pos.HedgeFor, hedgeCoin(sc)))
			}
		}
		if hasCfg && HedgeEnabled(sc) {
			hc := hedgeCoin(sc)
			if hpos := ss.Positions[hc]; hpos != nil && hpos.HedgeFor == "" && hpos.Quantity > 0 {
				warnings = append(warnings, fmt.Sprintf("strategy[%s]: position on configured hedge coin %s carries no hedge ownership stamp — not managing it as a hedge leg (fail-closed, #1159)", id, hc))
			}
		}
	}
	return warnings
}
