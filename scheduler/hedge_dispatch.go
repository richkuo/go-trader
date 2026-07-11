package main

import (
	"fmt"
	"sync"
)

// mirrorHedgeAfterPrimaryFill is the single entry point the HL perps dispatch
// site (main.go) calls after applying a primary-coin fill, when the strategy
// is hedge-enabled (#1159 phase 1). It is delta-based: it compares the
// primary's pre-cycle snapshot (preQty/preSide, captured before this cycle's
// signal ran) against its post-apply state (postQty/postSide) and mirrors
// whatever actually happened — open, scale-in add, partial close, full
// close, or flip — onto the hedge leg. This is deliberately independent of
// which of main.go's several execution branches produced the fill: the
// observed delta IS the event.
//
// Must be called OUTSIDE mu (it takes the lock itself for each state read/
// apply, matching the no-lock Phase-3 convention live order placement runs
// under). No-op when preQty==postQty (Signal==0 manage cycles, skipped
// signals, or a same-cycle open+immediate-SL-fill that round-tripped back to
// flat — correctly not hedged).
func mirrorHedgeAfterPrimaryFill(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, preQty float64, preSide string, price float64, execResult *HyperliquidExecuteResult, hedgeMid float64, notifier *MultiNotifier, logger *StrategyLogger) {
	if !sc.HedgeEnabled() {
		return
	}
	mu.RLock()
	var postQty float64
	postSide := ""
	primaryPositionID := ""
	if p, ok := s.Positions[primaryCoin]; ok && p != nil {
		postQty = p.Quantity
		postSide = p.Side
		primaryPositionID = p.TradePositionID
	}
	mu.RUnlock()

	if preQty == postQty && preSide == postSide {
		return
	}

	fillPrice := price
	var fillOID string
	var fillFee float64
	useFillFee := false
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		fill := execResult.Execution.Fill
		if fill.AvgPx > 0 {
			fillPrice = fill.AvgPx
		}
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		fillFee = fill.Fee
		useFillFee = true
	}
	// #1337 review: live/paper booking mode must track the STRATEGY's mode,
	// never a per-cycle proxy. execResult is nil not only for genuine paper
	// strategies but also for a LIVE strategy's Signal==0 manage-cycle
	// branches that can close the primary in-state without ever populating
	// execResult (e.g. the immediate fixed-ATR-SL fill at main.go's
	// hyperliquidArmFixedATRStopLossLive branch, recordPerpsStopLossClose) —
	// execResult != nil was previously read as "live", which silently routed
	// a real on-chain-close event through the no-order paper booking path
	// and stranded the actual on-chain hedge leg.
	live := hyperliquidIsLive(sc.Args)

	switch {
	case preQty <= 0 && postQty > 0:
		// Fresh open (or the open leg of a flip that started flat).
		mirrorHedgeOpen(sc, s, mu, primaryCoin, primaryPositionID, postQty, postSide, fillPrice, fillOID, fillFee, useFillFee, live, hedgeMid, notifier, logger)

	case preQty > 0 && postQty > preQty && postSide == preSide:
		// Scale-in add.
		addQty := postQty - preQty
		mirrorHedgeAdd(sc, s, mu, primaryCoin, addQty, postSide, fillPrice, fillOID, fillFee, useFillFee, live, hedgeMid, notifier, logger)

	case preQty > 0 && postQty > 0 && postQty < preQty && postSide == preSide:
		// Partial close.
		closedQty := preQty - postQty
		mirrorHedgeReduce(sc, s, mu, primaryCoin, preQty, closedQty, fillOID, fillFee, useFillFee, live, notifier, logger)

	case preQty > 0 && postQty == 0:
		// Full close (terminal — no reopen).
		mirrorHedgeFullClose(sc, s, mu, primaryCoin, fillOID, fillFee, useFillFee, live, notifier, logger)

	case preQty > 0 && postQty > 0 && postSide != preSide:
		// Flip: close the old-side hedge, then open a fresh hedge on the new
		// inverse side sized to the new primary qty. A reopen failure fails
		// closed on the NEW primary side (the old side is already closed
		// on-chain and cannot be un-closed).
		//
		// #1337 review: the reopen must NOT run if the reduce failed — a
		// failed close leaves the OLD-side hedge Position intact, and
		// applyHedgeOpenFill's blend branch would otherwise sum the new
		// fill into it while keeping the stale Side, corrupting the
		// Position (wrong side, wrong qty, no on-chain counterpart). On a
		// failed reduce we leave the (still-open, wrong-side-for-the-new-
		// primary) hedge for the coherence sweep/operator rather than risk
		// a blended, un-recoverable Position.
		if mirrorHedgeReduce(sc, s, mu, primaryCoin, preQty, preQty, fillOID, fillFee, useFillFee, live, notifier, logger) {
			mirrorHedgeOpen(sc, s, mu, primaryCoin, primaryPositionID, postQty, postSide, fillPrice, fillOID, fillFee, useFillFee, live, hedgeMid, notifier, logger)
		} else if logger != nil {
			logger.Error("hedge flip: old-side hedge close failed for primary %s — skipping the new-side hedge reopen to avoid blending opposite sides into one Position; the old hedge leg is left in place for manual review/the coherence sweep", primaryCoin)
		}
	}
}

// mirrorHedgeOpen opens (or grows, on a flip's reopen leg) the hedge to match
// a fresh primary open. Fails closed (constraint 4): a live hedge-open
// failure immediately submits a sized reduce-only unwind of the JUST-OPENED
// primary quantity and alerts CRITICAL. If the unwind itself fails, the
// primary open is still booked (the fill is real; state must never diverge
// from chain) and the coherence sweep retries the unwind every cycle.
func mirrorHedgeOpen(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin, primaryPositionID string, primaryQty float64, primarySide string, primaryFillPx float64, primaryFillOID string, primaryFillFee float64, useFillFee, live bool, hedgeMid float64, notifier *MultiNotifier, logger *StrategyLogger) {
	side := hedgeSideForPrimary(sideForPositionSide(primarySide))
	qty, ok := hedgeOpenQty(primaryQty, primaryFillPx, hedgeRatio(sc), hedgeMid)
	if !ok {
		if reason := hedgeOrderSkipReason(qty, hedgeMid); reason != "" && logger != nil {
			logger.Warn("hedge open skipped for %s: %s", primaryCoin, reason)
		}
		if live {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryQty, "hedge order size unresolvable", notifier, logger)
		}
		return
	}

	if !live {
		// Paper: book directly at the hedge mid, no order, no failure path.
		mu.Lock()
		pos := applyHedgeOpenFill(s, sc, primaryCoin, primaryPositionID, side, qty, hedgeMid, CalculatePlatformSpotFee(s.Platform, qty*hedgeMid), 0, "", false)
		mu.Unlock()
		if pos == nil {
			alertHedgeOpenRefusedOppositeSide(sc, primaryCoin, hedgeCoin(sc), qty, hedgeMid, false, notifier, logger)
			return
		}
		if logger != nil {
			logger.Info("PAPER hedge open %s %.6f @ $%.2f (for primary %s)", side, qty, hedgeMid, primaryCoin)
		}
		return
	}

	execResult, _, err := runHyperliquidHedgeOpenOrder(sc, qty, side, 0)
	if err != nil || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
		errMsg := "hedge open order failed"
		if err != nil {
			errMsg = err.Error()
		} else if execResult != nil && execResult.Error != "" {
			errMsg = execResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-open", hedgeCoin(sc), errMsg)
		unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryQty, errMsg, notifier, logger)
		return
	}

	fill := execResult.Execution.Fill
	var hFillOID string
	if fill.OID != 0 {
		hFillOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	pos := applyHedgeOpenFill(s, sc, primaryCoin, primaryPositionID, side, fill.TotalSz, fill.AvgPx, CalculatePlatformSpotFee(s.Platform, fill.TotalSz*fill.AvgPx), fill.Fee, hFillOID, true)
	mu.Unlock()
	if pos == nil {
		alertHedgeOpenRefusedOppositeSide(sc, primaryCoin, hedgeCoin(sc), fill.TotalSz, fill.AvgPx, true, notifier, logger)
		return
	}
	clearLiveExecThrottle(sc, "hedge-open", hedgeCoin(sc))
	if logger != nil {
		logger.Info("LIVE hedge open %s %.6f @ $%.2f oid=%d (for primary %s)", side, fill.TotalSz, fill.AvgPx, fill.OID, primaryCoin)
	}
}

// alertHedgeOpenRefusedOppositeSide reports the CRITICAL condition where a
// hedge open/add fill occurred (live: a real confirmed on-chain fill; paper:
// a virtual fill) but applyHedgeOpenFill refused to record it because an
// opposite-side hedge Position was still present (its guard against
// corrupting a blend across sides, #1337 review). Live is the severe case: a
// real order filled on the exchange and is now UNTRACKED by state until an
// operator manually reconciles it — this must never be silent.
func alertHedgeOpenRefusedOppositeSide(sc StrategyConfig, primaryCoin, hedgeSym string, qty, px float64, live bool, notifier *MultiNotifier, logger *StrategyLogger) {
	mode := "PAPER"
	tail := "no real order was placed — this is a state inconsistency to review, not untracked exposure."
	if live {
		mode = "LIVE"
		tail = "a REAL on-chain fill occurred and is now UNTRACKED by state — manually verify and reconcile the position on Hyperliquid."
	}
	msg := fmt.Sprintf("strategy %s: %s hedge open/add fill (%.6f %s @ $%.2f, for primary %s) was REFUSED because an opposite-side hedge Position was already present (a prior hedge close on this coin likely failed) — %s",
		sc.ID, mode, qty, hedgeSym, px, primaryCoin, tail)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
	if logger != nil {
		logger.Error("%s", msg)
	}
}

// unwindPrimaryAfterHedgeOpenFailure implements constraint 4's fail-closed
// unwind: a sized (never sz=None — the primary coin may be shared with
// peers) reduce-only close of the just-opened primary quantity. Booking the
// unwind fill (success or failure to unwind) always leaves the primary open
// booked, since the fill was real — state must never diverge from the chain.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, primaryQty float64, hedgeFailureReason string, notifier *MultiNotifier, logger *StrategyLogger) {
	if logger != nil {
		logger.Warn("HEDGE OPEN FAILED for primary %s (%s) — unwinding %.6f primary qty (fail-closed, #1159 constraint 4)", primaryCoin, hedgeFailureReason, primaryQty)
	}
	closeResult, _, err := RunHyperliquidClose(sc.Script, primaryCoin, &primaryQty, nil)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		// Unwind failed too — the primary open stays booked (fill was real);
		// the coherence sweep (hedge_sweep.go) retries the reduce-only unwind
		// every cycle until flat. CRITICAL: this strategy is running an
		// unhedged position the operator did not intend.
		msg := fmt.Sprintf("strategy %s: hedge open failed for primary %s AND the fail-closed unwind ALSO failed — position is open and UNHEDGED; the coherence sweep will keep retrying the unwind every cycle. Hedge error: %s", sc.ID, primaryCoin, hedgeFailureReason)
		if err != nil {
			msg += fmt.Sprintf("; unwind error: %v", err)
		}
		if notifier != nil {
			notifier.SendOwnerDM(msg)
			notifier.SendToAllChannels(msg)
		}
		if logger != nil {
			logger.Error("%s", msg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	bookPerpsCloseWithFillFee(s, primaryCoin, fill.AvgPx, fill.Fee, true, closeOID, "hedge_open_failed_unwind", "HEDGE-OPEN-FAILED unwind", "HEDGE-OPEN-FAILED unwind", logger)
	mu.Unlock()
	msg := fmt.Sprintf("strategy %s: hedge open failed for primary %s (%s) — the primary was immediately unwound (fail-closed, #1159 constraint 4). No unhedged exposure remains.", sc.ID, primaryCoin, hedgeFailureReason)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
	if logger != nil {
		logger.Warn("%s", msg)
	}
}

// mirrorHedgeAdd mirrors a scale-in add onto the hedge leg. Fails closed on
// JUST the add leg (not the whole primary) — the pre-existing primary+hedge
// pair stays intact; only the just-added primary quantity is unwound.
func mirrorHedgeAdd(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, addQty float64, primarySide string, fillPx float64, fillOID string, fillFee float64, useFillFee, live bool, hedgeMid float64, notifier *MultiNotifier, logger *StrategyLogger) {
	side := hedgeSideForPrimary(sideForPositionSide(primarySide))
	qty, ok := hedgeOpenQty(addQty, fillPx, hedgeRatio(sc), hedgeMid)
	if !ok {
		if live {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, addQty, "hedge add order size unresolvable", notifier, logger)
		}
		return
	}
	mu.RLock()
	var prevHedgeQty float64
	if p, ok3 := s.Positions[hedgeCoin(sc)]; ok3 && p != nil {
		prevHedgeQty = p.Quantity
	}
	var primaryPositionID string
	if p, ok3 := s.Positions[primaryCoin]; ok3 && p != nil {
		primaryPositionID = p.TradePositionID
	}
	mu.RUnlock()

	if !live {
		mu.Lock()
		pos := applyHedgeOpenFill(s, sc, primaryCoin, primaryPositionID, side, qty, hedgeMid, CalculatePlatformSpotFee(s.Platform, qty*hedgeMid), 0, "", false)
		mu.Unlock()
		if pos == nil {
			alertHedgeOpenRefusedOppositeSide(sc, primaryCoin, hedgeCoin(sc), qty, hedgeMid, false, notifier, logger)
		}
		return
	}

	execResult, _, err := runHyperliquidHedgeOpenOrder(sc, qty, side, prevHedgeQty)
	if err != nil || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
		errMsg := "hedge add order failed"
		if err != nil {
			errMsg = err.Error()
		} else if execResult != nil && execResult.Error != "" {
			errMsg = execResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-add", hedgeCoin(sc), errMsg)
		unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, addQty, errMsg, notifier, logger)
		return
	}
	fill := execResult.Execution.Fill
	var hFillOID string
	if fill.OID != 0 {
		hFillOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	pos := applyHedgeOpenFill(s, sc, primaryCoin, primaryPositionID, side, fill.TotalSz, fill.AvgPx, CalculatePlatformSpotFee(s.Platform, fill.TotalSz*fill.AvgPx), fill.Fee, hFillOID, true)
	mu.Unlock()
	if pos == nil {
		alertHedgeOpenRefusedOppositeSide(sc, primaryCoin, hedgeCoin(sc), fill.TotalSz, fill.AvgPx, true, notifier, logger)
		return
	}
	clearLiveExecThrottle(sc, "hedge-add", hedgeCoin(sc))
}

// mirrorHedgeReduce mirrors a primary partial close (or the close half of a
// flip) onto the hedge leg with a proportional-by-quantity reduce-only
// close. No unwind on failure — the primary leg is already closed on-chain
// and cannot be un-closed; the hedge Position is left untouched and the
// coherence sweep retries the reduce-only close every cycle.
//
// Returns true when the hedge ends this call with no MORE than the intended
// residual — i.e. there is nothing left blocking a subsequent hedge open on
// a NEW side (no hedge existed, nothing needed reducing, or the reduce/close
// order confirmed). Returns false only when a live order was required and
// failed, leaving the pre-existing hedge Position (wrong side, for a flip
// caller) still in place — callers (the flip path) must not open a new-side
// hedge when this is false, or applyHedgeOpenFill's blend branch will merge
// opposite sides into one corrupt Position (#1337 review).
func mirrorHedgeReduce(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, primaryQtyBefore, primaryQtyClosed float64, fillOID string, fillFee float64, useFillFee, live bool, notifier *MultiNotifier, logger *StrategyLogger) bool {
	hedgeSym := hedgeCoin(sc)
	mu.RLock()
	hedgePos, exists := s.Positions[hedgeSym]
	var hedgeQty float64
	if exists && hedgePos != nil {
		hedgeQty = hedgePos.Quantity
	}
	mu.RUnlock()
	if !exists || hedgeQty <= 0 {
		return true
	}
	reduceQty := hedgeReduceQty(hedgeQty, primaryQtyBefore, primaryQtyClosed)
	if reduceQty <= 0 {
		return true
	}
	full := reduceQty >= hedgeQty-1e-9

	if !live {
		mu.Lock()
		if full {
			applyHedgeCloseFill(s, primaryCoin, hedgeSym, hedgePos.AvgCost, 0, false, "", "hedge_partial_close_paper", logger)
		} else {
			applyHedgeReduceFill(s, hedgeSym, reduceQty, hedgePos.AvgCost, 0, false, "", "hedge_partial_close_paper", logger)
		}
		mu.Unlock()
		return true
	}

	var partialSz *float64
	if !full {
		partialSz = &reduceQty
	}
	closeResult, _, err := runHyperliquidHedgeCloseOrder(sc, partialSz)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "hedge reduce order failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-reduce", hedgeSym, errMsg)
		if logger != nil {
			logger.Error("hedge reduce failed for %s (%s) — hedge leg left as-is; the coherence sweep will retry", hedgeSym, errMsg)
		}
		return false
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	if full {
		applyHedgeCloseFill(s, primaryCoin, hedgeSym, fill.AvgPx, fill.Fee, true, closeOID, "hedge_partial_close", logger)
	} else {
		applyHedgeReduceFill(s, hedgeSym, fill.TotalSz, fill.AvgPx, fill.Fee, true, closeOID, "hedge_partial_close", logger)
	}
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-reduce", hedgeSym)
	return true
}

// mirrorHedgeFullClose mirrors a terminal primary full close onto the hedge
// leg with a full (sz=None — safe, sole-owner by construction) reduce-only
// close. No unwind on failure, same rationale as mirrorHedgeReduce.
func mirrorHedgeFullClose(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, fillOID string, fillFee float64, useFillFee, live bool, notifier *MultiNotifier, logger *StrategyLogger) {
	hedgeSym := hedgeCoin(sc)
	mu.RLock()
	hedgePos, exists := s.Positions[hedgeSym]
	mu.RUnlock()
	if !exists || hedgePos == nil || hedgePos.Quantity <= 0 {
		return
	}

	if !live {
		mu.Lock()
		applyHedgeCloseFill(s, primaryCoin, hedgeSym, hedgePos.AvgCost, 0, false, "", "hedge_close_paper", logger)
		mu.Unlock()
		return
	}

	closeResult, _, err := runHyperliquidHedgeCloseOrder(sc, nil)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "hedge close order failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-close", hedgeSym, errMsg)
		if logger != nil {
			logger.Error("hedge close failed for %s (%s) — primary is already closed; hedge leg left open, the coherence sweep will retry (hedge leg surviving primary close containment)", hedgeSym, errMsg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	applyHedgeCloseFill(s, primaryCoin, hedgeSym, fill.AvgPx, fill.Fee, true, closeOID, "hedge_close", logger)
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-close", hedgeSym)
}

// sideForPositionSide maps a Position.Side ("long"/"short") to the order
// side that WOULD have opened it ("buy"/"sell") — the input hedgeSideForPrimary
// expects. Distinct from closeTradeSide (portfolio.go), which maps a position
// side to the side that CLOSES it.
func sideForPositionSide(positionSide string) string {
	if positionSide == "short" {
		return "sell"
	}
	return "buy"
}
