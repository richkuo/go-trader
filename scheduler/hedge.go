package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// #1159 — per-strategy correlated hedge legs (phase 1, HL perps).
//
// The hedge is managed as a per-cycle, STATE-DERIVED reconciler ("hedge sync"),
// not scattered per-event mirror hooks. hedgeTargetDecision computes the single
// hedge action that converges the hedge leg to the CURRENT primary position vs.
// a persisted quantity watermark (Position.HedgePrimaryQtyBasis). runHedgeSync
// runs that decision every HL dispatch cycle, so every primary lifecycle event —
// fresh open, scale-in add, partial/full close, on-chain SL/TP fill detected by
// reconcile, kill-switch/CB force close — automatically produces the matching
// hedge action within the same or next cycle without touching each close path.
//
// Invariants:
//   - Qty-event mirroring, not price mirroring: the target keys on the primary
//     qty watermark, so mark drift never re-trades the hedge; only qty/side
//     changes do.
//   - Fill-confirmed state mutation only: hedge virtual state mutates solely
//     from a confirmed fill (mirrors runHyperliquidExecuteOrder's ok=false → no
//     state update contract).
//   - Fail-closed open: a confirmed primary fresh open whose hedge open fails in
//     the same cycle immediately unwinds the primary fill (never run unhedged).
//   - Sole ownership by construction: collision validation guarantees the hedge
//     coin is never any strategy's configured coin and never shared between
//     hedgers, so on-chain hedge operations are always sole-owner operations.

// hedgeMinOrderNotionalUSD is the floor below which a hedge REDUCE or ADD is
// deferred as dust (HL rejects sub-~$10 orders). A full close always executes.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeQtyEpsilon guards float comparisons of primary qty vs the hedge basis.
const hedgeQtyEpsilon = 1e-9

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
		return "close"
	default:
		return "none"
	}
}

// hedgeSnapshot captures the primary + hedge position state under the Phase-1
// RLock so hedgeTargetDecision is a pure function of one consistent view.
type hedgeSnapshot struct {
	PrimaryQty  float64
	PrimarySide string // "long" | "short" | "" (flat)
	HedgeQty    float64
	HedgeSide   string  // "long" | "short" | "" (flat)
	HedgeBasis  float64 // primary qty the hedge was last sized against (watermark)
}

// hedgeAction is the pure decision output. Qty is a positive magnitude. Side is
// the ORDER side ("buy"/"sell"). FailClosed signals an unusable mark: the caller
// escalates (fresh open → unwind primary; manage cycle → alert + retry).
// TargetBasis is the primary qty the hedge would be sized against after the
// order fully fills; RequestedQty is the order qty asked for. The apply path
// advances the persisted basis proportionally to the ACTUAL fill so a partial
// fill leaves a consistent watermark and the next cycle catches up.
type hedgeAction struct {
	Kind        hedgeActionKind
	Qty         float64
	Side        string
	Reason      string
	FailClosed  bool
	TargetBasis float64
}

// hedgeInverseOrderSide maps a primary position side to the hedge OPEN order
// side for an inverse hedge: primary long → short hedge → "sell"; primary short
// → long hedge → "buy".
func hedgeInverseOrderSide(primarySide string) string {
	if primarySide == "long" {
		return "sell"
	}
	return "buy"
}

// hedgeInversePositionSide maps a primary side to the hedge POSITION side.
func hedgeInversePositionSide(primarySide string) string {
	if primarySide == "long" {
		return "short"
	}
	return "long"
}

// hedgeReduceOrderSide returns the reduce-only order side that shrinks/closes an
// existing hedge leg: closing a short buys back; closing a long sells.
func hedgeReduceOrderSide(hedgeSide string) string {
	if hedgeSide == "short" {
		return "buy"
	}
	return "sell"
}

// hedgeTargetDecision computes the single hedge action that converges the hedge
// leg to the primary position, keyed on the qty watermark (never on mark drift).
// primaryPx / hedgePx are the current marks. Pure; no I/O.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeActionNone}
	}
	primaryFlat := snap.PrimaryQty <= hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	// Primary flat: close any hedge; nothing to do if none.
	if primaryFlat {
		if hedgeHeld {
			return hedgeAction{
				Kind:   hedgeActionCloseFull,
				Qty:    snap.HedgeQty,
				Side:   hedgeReduceOrderSide(snap.HedgeSide),
				Reason: "primary flat",
			}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}

	// Primary held.
	ratio := hedgeRatio(sc)
	wantSide := hedgeInversePositionSide(snap.PrimarySide)

	// Defense-in-depth: a held hedge on the WRONG side (only reachable if a flip
	// slipped past the direction="both" reject) is flattened, never blended into.
	if hedgeHeld && snap.HedgeSide != wantSide {
		return hedgeAction{
			Kind:   hedgeActionCloseFull,
			Qty:    snap.HedgeQty,
			Side:   hedgeReduceOrderSide(snap.HedgeSide),
			Reason: fmt.Sprintf("hedge side %q opposes required %q — flattening", snap.HedgeSide, wantSide),
		}
	}

	// Any hedge order below needs usable marks.
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, FailClosed: true, Reason: "hedge/primary mark unavailable"}
	}

	// Primary held, no hedge → open sized to the full primary notional.
	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, FailClosed: true, Reason: "computed hedge open qty <= 0"}
		}
		return hedgeAction{
			Kind:        hedgeActionOpen,
			Qty:         qty,
			Side:        hedgeInverseOrderSide(snap.PrimarySide),
			Reason:      "primary held, hedge flat",
			TargetBasis: snap.PrimaryQty,
		}
	}

	// Both held, correct side → converge on the qty watermark.
	if snap.PrimaryQty > snap.HedgeBasis+hedgeQtyEpsilon {
		delta := snap.PrimaryQty - snap.HedgeBasis
		addQty := delta * primaryPx * ratio / hedgePx
		if addQty*hedgePx < hedgeMinOrderNotionalUSD {
			// Dust: defer without advancing the basis so it accumulates.
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge add below min notional — deferring"}
		}
		return hedgeAction{
			Kind:        hedgeActionAdd,
			Qty:         addQty,
			Side:        hedgeInverseOrderSide(snap.PrimarySide),
			Reason:      "primary increased",
			TargetBasis: snap.PrimaryQty,
		}
	}
	if snap.HedgeBasis > 0 && snap.PrimaryQty < snap.HedgeBasis-hedgeQtyEpsilon {
		frac := (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if frac > 1 {
			frac = 1
		}
		reduceQty := snap.HedgeQty * frac
		if reduceQty >= snap.HedgeQty-hedgeQtyEpsilon {
			// Reduce covers ~the whole leg → full close.
			return hedgeAction{
				Kind:   hedgeActionCloseFull,
				Qty:    snap.HedgeQty,
				Side:   hedgeReduceOrderSide(snap.HedgeSide),
				Reason: "primary reduced to ~flat",
			}
		}
		if reduceQty*hedgePx < hedgeMinOrderNotionalUSD {
			// Dust: defer without advancing the basis.
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge reduce below min notional — deferring"}
		}
		return hedgeAction{
			Kind:        hedgeActionReduce,
			Qty:         reduceQty,
			Side:        hedgeReduceOrderSide(snap.HedgeSide),
			Reason:      "primary reduced",
			TargetBasis: snap.PrimaryQty,
		}
	}
	return hedgeAction{Kind: hedgeActionNone}
}

// hedgeBasisAfterFill advances the persisted qty watermark proportionally to the
// ACTUAL fill: full fill lands exactly on TargetBasis; a partial fill lands
// between the old basis and the target so the next cycle converges the rest.
// Never overshoots the target regardless of fill/round noise.
func hedgeBasisAfterFill(oldBasis, targetBasis, requestedQty, filledQty float64) float64 {
	if requestedQty <= 0 {
		return targetBasis
	}
	frac := filledQty / requestedQty
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	next := oldBasis + (targetBasis-oldBasis)*frac
	// Clamp to [min,max] of old/target to defend against float noise.
	lo, hi := oldBasis, targetBasis
	if lo > hi {
		lo, hi = hi, lo
	}
	if next < lo {
		next = lo
	}
	if next > hi {
		next = hi
	}
	return next
}

// hedgeSnapshotForStrategy builds a hedgeSnapshot from live state. Caller holds
// at least an RLock on mu.
func hedgeSnapshotForStrategy(s *StrategyState, sc StrategyConfig) hedgeSnapshot {
	var snap hedgeSnapshot
	primaryCoin := hyperliquidSymbol(sc.Args)
	if pos := s.Positions[primaryCoin]; pos != nil && pos.Quantity > 0 {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	if hpos := s.Positions[hedgeCoin(sc)]; hpos != nil && hpos.HedgeFor != "" && hpos.Quantity > 0 {
		snap.HedgeQty = hpos.Quantity
		snap.HedgeSide = hpos.Side
		snap.HedgeBasis = hpos.HedgePrimaryQtyBasis
	}
	return snap
}

// runHedgeSync converges a hedge-enabled strategy's hedge leg to its primary
// position for one cycle. It is the single mirror choke point for every primary
// lifecycle event. Lock discipline mirrors runHyperliquidProtectionSync:
// snapshot under RLock → spawn the order unlocked → apply under Lock with a
// re-read.
//
// freshPrimaryOpen indicates the primary went flat→held THIS cycle (a genuine
// fresh open, not an add/manage). Only then does a hedge-OPEN failure escalate
// to unwinding the primary (constraint 4 "never run unhedged"). On any other
// cycle a failure alerts + retries next cycle (the state-derived design
// self-heals). primaryOpenFillQty is the primary fill qty to unwind on
// escalation. Returns the number of hedge trades booked this cycle.
func runHedgeSync(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, prices map[string]float64, hlPositions []HLPosition, freshPrimaryOpen bool, primaryOpenFillQty float64, notifier *MultiNotifier, logger *StrategyLogger) int {
	if s == nil || !sc.HedgeEnabled() {
		return 0
	}
	primaryCoin := hyperliquidSymbol(sc.Args)
	hc := hedgeCoin(sc)
	if hc == "" || primaryCoin == "" {
		return 0
	}
	primaryPx := prices[primaryCoin]
	hedgePx := prices[hc]

	mu.RLock()
	snap := hedgeSnapshotForStrategy(s, sc)
	mu.RUnlock()

	decision := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if decision.Kind == hedgeActionNone {
		if decision.FailClosed && snap.PrimaryQty > 0 && snap.HedgeQty <= hedgeQtyEpsilon {
			// Primary held but we cannot even price the hedge to open it.
			if freshPrimaryOpen && hyperliquidIsLive(sc.Args) {
				return unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryOpenFillQty, prices, "hedge mark unavailable at open", notifier, logger)
			}
			hedgeAlert(notifier, logger, "hedge %s for %s: %s — will retry next cycle", hc, sc.ID, decision.Reason)
		} else if decision.Reason != "" {
			logger.Info("hedge %s: %s", hc, decision.Reason)
		}
		return 0
	}

	live := hyperliquidIsLive(sc.Args)

	switch decision.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		return runHedgeOpenOrAdd(sc, s, mu, decision, snap, hc, primaryCoin, hedgePx, prices, hlPositions, live, freshPrimaryOpen, primaryOpenFillQty, notifier, logger)
	case hedgeActionReduce, hedgeActionCloseFull:
		return runHedgeReduceOrClose(sc, s, mu, decision, snap, hc, primaryCoin, hedgePx, live, notifier, logger)
	}
	return 0
}

// runHedgeOpenOrAdd places (live) or books (paper) a hedge open/add and applies
// the confirmed fill under Lock. A live open failure on a fresh-open cycle
// escalates to unwinding the primary.
func runHedgeOpenOrAdd(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, decision hedgeAction, snap hedgeSnapshot, hc, primaryCoin string, hedgePx float64, prices map[string]float64, hlPositions []HLPosition, live, freshPrimaryOpen bool, primaryOpenFillQty float64, notifier *MultiNotifier, logger *StrategyLogger) int {
	var fillPx, filledQty, fillFee float64
	var fillOID int64
	useFillFee := false

	if live {
		// margin_mode + leverage only on a FRESH hedge open (hedge coin flat on
		// chain); HL rejects update_leverage on an open position, so an add
		// passes empty/0 to leave the on-chain assignment untouched.
		marginMode := ""
		leverage := 0.0
		if decision.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeLeverage(sc)
		}
		wallet := hlExecuteSnapshotForCoin(hlPositions, hc)
		execResult, _, err := RunHyperliquidExecute(sc.Script, hc, decision.Side, decision.Qty, 0 /*no SL*/, 0, 0, marginMode, leverage, false, wallet)
		if err != nil || execResult == nil || execResult.Error != "" || execResult.Execution == nil || execResult.Execution.Fill == nil || execResult.Execution.Fill.TotalSz <= 0 {
			reason := "no fill"
			if err != nil {
				reason = err.Error()
			} else if execResult != nil && execResult.Error != "" {
				reason = execResult.Error
			}
			notifyLiveExecFailure(notifier, sc, "hedge-"+decision.Side, hc, reason)
			if decision.Kind == hedgeActionOpen && freshPrimaryOpen {
				// FAIL CLOSED: the primary is confirmed filled but has no hedge.
				return unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryOpenFillQty, prices, fmt.Sprintf("hedge open failed: %s", reason), notifier, logger)
			}
			hedgeAlert(notifier, logger, "hedge %s %s for %s failed (%s) — retrying next cycle", decision.Kind, hc, sc.ID, reason)
			return 0
		}
		clearLiveExecThrottle(sc, "hedge-"+decision.Side, hc)
		fill := execResult.Execution.Fill
		fillPx = fill.AvgPx
		filledQty = fill.TotalSz
		fillFee = fill.Fee
		fillOID = fill.OID
		useFillFee = true
		if fillPx <= 0 {
			fillPx = hedgePx
		}
	} else {
		// Paper: book virtually at the mark; no failure path exists.
		fillPx = hedgePx
		filledQty = decision.Qty
	}

	mu.Lock()
	trades := applyHedgeOpenOrAddFill(sc, s, hc, primaryCoin, decision, snap, filledQty, fillPx, fillFee, useFillFee, fillOID, live, logger)
	mu.Unlock()
	return trades
}

// runHedgeReduceOrClose places (live) or books (paper) a reduce-only hedge
// close/reduce and applies the confirmed fill under Lock. Hedge closes never
// unwind the primary (the primary is already at its target); a live failure
// alerts and the next cycle retries.
func runHedgeReduceOrClose(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, decision hedgeAction, snap hedgeSnapshot, hc, primaryCoin string, hedgePx float64, live bool, notifier *MultiNotifier, logger *StrategyLogger) int {
	fullClose := decision.Kind == hedgeActionCloseFull
	var fillPx, filledQty, fillFee float64
	var fillOID int64
	useFillFee := false

	if live {
		var partialSz *float64
		if !fullClose {
			q := decision.Qty
			partialSz = &q
		}
		closeResult, _, err := RunHyperliquidClose(sc.Script, hc, partialSz, nil)
		if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
			if closeResult != nil && closeResult.Close != nil && closeResult.Close.AlreadyFlat {
				// On-chain already flat (externally closed in the window between
				// the reconcile pre-phase and this dispatch). Book the virtual leg
				// out at mark so its realized PnL credits the owner's ledger
				// instead of being dropped, then it is deleted by the booker.
				booked := 0
				mu.Lock()
				if hp := s.Positions[hc]; hp != nil && hp.HedgeFor != "" {
					if hedgePx > 0 && bookPerpsCloseWithFillFee(s, hc, hedgePx, 0, false, "", "hedge_close_already_flat", fmt.Sprintf("hedge(%s) close (already flat on-chain)", primaryCoin), "hedge close already-flat", logger) {
						booked = 1
					} else {
						delete(s.Positions, hc)
					}
				}
				mu.Unlock()
				hedgeAlert(notifier, logger, "hedge %s for %s already flat on-chain — booked virtual leg out at mark", hc, sc.ID)
				return booked
			}
			reason := "no fill"
			if err != nil {
				reason = err.Error()
			} else if closeResult != nil && closeResult.Error != "" {
				reason = closeResult.Error
			}
			notifyLiveExecFailure(notifier, sc, "hedge-close", hc, reason)
			hedgeAlert(notifier, logger, "hedge %s %s for %s failed (%s) — primary already at target; retrying next cycle", decision.Kind, hc, sc.ID, reason)
			return 0
		}
		clearLiveExecThrottle(sc, "hedge-close", hc)
		fill := closeResult.Close.Fill
		fillPx = fill.AvgPx
		filledQty = fill.TotalSz
		fillFee = fill.Fee
		fillOID = fill.OID
		useFillFee = true
		if fillPx <= 0 {
			fillPx = hedgePx
		}
	} else {
		fillPx = hedgePx
		if fullClose {
			filledQty = snap.HedgeQty
		} else {
			filledQty = decision.Qty
		}
	}

	mu.Lock()
	trades := applyHedgeReduceOrCloseFill(sc, s, hc, primaryCoin, decision, snap, filledQty, fillPx, fillFee, useFillFee, fillOID, fullClose, logger)
	mu.Unlock()
	return trades
}

// applyHedgeOpenOrAddFill creates or blends the hedge Position from a confirmed
// fill and records the hedge open Trade. Caller holds mu.Lock. Booking always
// happens against whatever hedge position actually exists now (never drops a
// fill): a nil position → create; an existing one → blend qty/AvgCost.
func applyHedgeOpenOrAddFill(sc StrategyConfig, s *StrategyState, hc, primaryCoin string, decision hedgeAction, snap hedgeSnapshot, filledQty, fillPx, fillFee float64, useFillFee bool, fillOID int64, live bool, logger *StrategyLogger) int {
	if filledQty <= 0 || fillPx <= 0 {
		return 0
	}
	now := time.Now().UTC()
	notional := filledQty * fillPx
	feePlatform := s.Platform
	fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
	feeSource := executionFeeSource(fillFee, useFillFee)
	s.Cash -= fee // margin-based: only the fee leaves cash, notional stays virtual

	positionSide := hedgeInversePositionSide(snap.PrimarySide)
	pos := s.Positions[hc]
	if pos == nil || pos.HedgeFor == "" || pos.Quantity <= 0 {
		// Create a fresh hedge leg.
		basis := hedgeBasisAfterFill(0, decision.TargetBasis, decision.Qty, filledQty)
		s.Positions[hc] = &Position{
			Symbol:               hc,
			Quantity:             filledQty,
			InitialQuantity:      filledQty,
			AvgCost:              fillPx,
			Side:                 positionSide,
			Multiplier:           1,
			Leverage:             hedgeLeverage(sc),
			OwnerStrategyID:      s.ID,
			OpenedAt:             now,
			TradePositionID:      newTradePositionID(s.ID, hc, now),
			HedgeFor:             primaryCoin,
			HedgePrimaryQtyBasis: basis,
		}
	} else {
		// Blend into the existing hedge leg (scale-in add).
		oldQty := pos.Quantity
		newQty := oldQty + filledQty
		pos.AvgCost = (oldQty*pos.AvgCost + filledQty*fillPx) / newQty
		pos.Quantity = newQty
		pos.HedgePrimaryQtyBasis = hedgeBasisAfterFill(snap.HedgeBasis, decision.TargetBasis, decision.Qty, filledQty)
	}

	pos = s.Positions[hc]
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hc,
		PositionID:      pos.TradePositionID,
		Side:            decision.Side,
		Quantity:        filledQty,
		Price:           fillPx,
		Value:           notional,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.4f (fee $%.4f)", primaryCoin, decision.Kind, positionSide, filledQty, fillPx, fee),
		ExchangeFee:     fee,
		FeeSource:       feeSource,
		PnLGross:        true,
		ExchangeOrderID: exchangeOrderIDForTrade(fmt.Sprintf("%d", fillOID), useFillFee && fillOID != 0),
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("hedge %s(%s): %s %.6f @ $%.4f basis=%.6f (fee $%.4f)", hc, primaryCoin, decision.Kind, filledQty, fillPx, pos.HedgePrimaryQtyBasis, fee)
	}
	return 1
}

// applyHedgeReduceOrCloseFill books a confirmed hedge reduce/close via the
// shared perps close-booking helpers (which route hedge legs to
// RecordHedgeTradeResult + trade_type=hedge and skip diagnostics), then advances
// the surviving leg's watermark. Caller holds mu.Lock.
func applyHedgeReduceOrCloseFill(sc StrategyConfig, s *StrategyState, hc, primaryCoin string, decision hedgeAction, snap hedgeSnapshot, filledQty, fillPx, fillFee float64, useFillFee bool, fillOID int64, fullClose bool, logger *StrategyLogger) int {
	if filledQty <= 0 || fillPx <= 0 {
		return 0
	}
	pos := s.Positions[hc]
	if pos == nil || pos.HedgeFor == "" {
		return 0
	}
	oid := ""
	if fillOID != 0 {
		oid = fmt.Sprintf("%d", fillOID)
	}
	detailsPrefix := fmt.Sprintf("hedge(%s) %s", primaryCoin, decision.Kind)
	if fullClose || filledQty >= pos.Quantity-hedgeQtyEpsilon {
		if bookPerpsCloseWithFillFee(s, hc, fillPx, fillFee, useFillFee, oid, "hedge_close", detailsPrefix, "hedge close", logger) {
			return 1
		}
		return 0
	}
	if bookPerpsPartialCloseWithFillFee(s, hc, filledQty, fillPx, fillFee, useFillFee, oid, "hedge_reduce", detailsPrefix, "hedge reduce", logger) {
		// Advance the surviving leg's watermark proportionally to the fill.
		if hp := s.Positions[hc]; hp != nil && hp.HedgeFor != "" {
			hp.HedgePrimaryQtyBasis = hedgeBasisAfterFill(snap.HedgeBasis, decision.TargetBasis, decision.Qty, filledQty)
		}
		return 1
	}
	return 0
}

// unwindPrimaryAfterHedgeOpenFailure closes the just-opened primary fill
// reduce-only (constraint 4 fail-closed) when the hedge open failed on the
// fresh-open cycle, cancels the primary's just-armed SL/TP OIDs, books the
// close, and CRITICAL-alerts the owner. A sized close (never a full
// market_close) is used because the primary coin may have shared-coin peers.
// Unwind failure leaves state unchanged — the next-cycle hedge sync retries the
// hedge open (documented degraded loop). Returns 0 (no HEDGE trade booked).
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, primaryFillQty float64, prices map[string]float64, cause string, notifier *MultiNotifier, logger *StrategyLogger) int {
	if primaryFillQty <= 0 {
		hedgeAlert(notifier, logger, "hedge open failed for %s (%s) but primary fill qty unknown — cannot auto-unwind; MANUAL INTERVENTION REQUIRED", sc.ID, cause)
		hedgeCritical(notifier, "🚨 hedge open failed for %s (%s); primary %s may be running UNHEDGED — manual review required", sc.ID, cause, primaryCoin)
		return 0
	}
	// Snapshot the primary's cancel OIDs under RLock.
	mu.RLock()
	var cancelOIDs []int64
	if pos := s.Positions[primaryCoin]; pos != nil {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()

	sz := primaryFillQty
	closeResult, _, err := RunHyperliquidClose(sc.Script, primaryCoin, &sz, cancelOIDs)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
		reason := "no fill"
		if err != nil {
			reason = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			reason = closeResult.Error
		}
		hedgeCritical(notifier, "🚨 %s: hedge open failed (%s) AND the primary %s unwind ALSO failed (%s) — position may be running UNHEDGED; next cycle retries the hedge open. MANUAL REVIEW.", sc.ID, cause, primaryCoin, reason)
		if logger != nil {
			logger.Error("hedge-open-failed unwind of %s failed: %s", primaryCoin, reason)
		}
		return 0
	}
	fill := closeResult.Close.Fill
	fillPx := fill.AvgPx
	if fillPx <= 0 {
		fillPx = prices[primaryCoin]
	}
	mu.Lock()
	bookPerpsPartialCloseWithFillFee(s, primaryCoin, fill.TotalSz, fillPx, fill.Fee, true, fmt.Sprintf("%d", fill.OID), "hedge_open_failed_unwind", "hedge-open-failed unwind", "hedge-open-failed unwind", logger)
	mu.Unlock()
	hedgeCritical(notifier, "🚨 %s: hedge open failed (%s) — unwound the primary %s fill (%.6f @ $%.4f) reduce-only to avoid running unhedged.", sc.ID, cause, primaryCoin, fill.TotalSz, fillPx)
	return 0
}

// validateHedgeStateConsistency warns (non-destructively) about persisted hedge
// legs that no longer match config — a config edit + restart bypasses the
// SIGHUP hot-reload guard (which only sees in-process edits). A hedge position
// whose strategy no longer enables a matching hedge block, or whose coin differs
// from the configured hedge symbol, is left FROZEN (never auto-closed) and
// surfaced to the operator, mirroring the shared-coin ambiguity convention.
// Returns warning strings (also printed to stderr).
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var warnings []string
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
		for _, sym := range syms {
			pos := ss.Positions[sym]
			if pos == nil || pos.HedgeFor == "" {
				continue
			}
			sc, ok := byID[id]
			if !ok || !sc.HedgeEnabled() {
				msg := fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg %s (qty=%.6f) but its config no longer enables a hedge — leaving it frozen; flatten it or restore the hedge block", id, sym, pos.Quantity)
				fmt.Printf("[WARN] %s\n", msg)
				warnings = append(warnings, msg)
				continue
			}
			if hc := hedgeCoin(sc); hc != sym {
				msg := fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg on %s but config now hedges %s — leaving the old leg frozen; flatten it or restore the previous hedge symbol", id, sym, hc)
				fmt.Printf("[WARN] %s\n", msg)
				warnings = append(warnings, msg)
			}
		}
	}
	return warnings
}

// hedgeSummaryTag renders the one-line operator audit tag for an active hedge
// block, e.g. "hedge=BTC×1.0(inverse,cross,3x)". Empty when hedging is off.
func hedgeSummaryTag(sc StrategyConfig) string {
	if !sc.HedgeEnabled() {
		return ""
	}
	return fmt.Sprintf("hedge=%s×%g(%s,%s,%gx)", hedgeCoin(sc), hedgeRatio(sc), hedgeSide(sc), hedgeMarginMode(sc), hedgeLeverage(sc))
}

// hedgeStatusNote lists hedge-enabled strategies for the Discord /status output
// (#1159). Empty when none configure a hedge. IDs sorted for stable output.
func hedgeStatusNote(strategies []StrategyConfig) string {
	var lines []string
	for _, sc := range strategies {
		if sc.HedgeEnabled() {
			lines = append(lines, fmt.Sprintf("%s→%s×%g", sc.ID, hedgeCoin(sc), hedgeRatio(sc)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return "\n🛡️ hedged: " + joinComma(lines)
}

// joinComma joins with ", " (tiny local helper to avoid importing strings here).
func joinComma(items []string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}

// hedgeAlert logs a hedge WARN and DMs the owner (non-fatal drift / retries).
func hedgeAlert(notifier *MultiNotifier, logger *StrategyLogger, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if logger != nil {
		logger.Warn("%s", msg)
	}
	if notifier != nil {
		notifier.SendOwnerDM("⚠️ " + msg)
	}
}

// hedgeCritical broadcasts a CRITICAL hedge failure to the owner DM AND all
// channels (fail-closed unwind / potential unhedged running).
func hedgeCritical(notifier *MultiNotifier, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[CRITICAL] %s\n", msg)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}
