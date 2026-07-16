package main

import (
	"fmt"
	"sync"
	"time"
)

// #1159 phase 1: per-strategy correlated hedge legs (Hyperliquid perps only).
//
// Design: hedge management is a per-cycle, STATE-DERIVED reconciler
// (runHedgeSync), not scattered per-event mirror hooks. hedgeTargetDecision
// (pure) diffs the CURRENT primary position against a persisted quantity
// watermark (Position.HedgePrimaryQtyBasis, stamped on the hedge leg) — mark
// price drift never re-trades the hedge, only a real primary quantity/side
// change does. Because it is state-derived, every primary lifecycle event
// (fresh open, scale-in add, evaluator partial/full close, an on-chain
// SL/TP fill booked by reconcile, external close) converges the hedge
// within the same or the next cycle, without touching each primary close
// path individually.
//
// Ownership: hedge coins are guaranteed sole-owned by validateHedgeConfigs'
// collision matrix (own coin / any configured strategy coin / another
// strategy's hedge coin all rejected at load), so every operation below is
// safely "the sole owner of this coin" — a full close can use sz=None-style
// semantics without touching a peer's exposure. The persisted
// Position.HedgeFor field (never coin→config inference) is the sole
// ownership source for reconcile/restart recovery.

const (
	// hedgeQtyEpsilon absorbs float rounding noise when comparing perps
	// quantities (crypto sizes routinely carry many decimal places).
	hedgeQtyEpsilon = 1e-9
	// hedgeMinOrderNotionalUSD approximates Hyperliquid's practical minimum
	// order size. A reduce below this notional is deferred (not forced
	// through) rather than spamming an unfillable dust order; the basis is
	// intentionally left unadvanced so the deficit accumulates and retries.
	hedgeMinOrderNotionalUSD = 10.0
)

type hedgeActionKind int

const (
	hedgeActionNone hedgeActionKind = iota
	hedgeActionOpen
	hedgeActionAdd
	hedgeActionReduce
	hedgeActionCloseFull
	// hedgeActionOpenUnavailable is a distinct no-op: the primary is held,
	// no hedge is held yet, but the hedge coin's price is unusable so no
	// open order can even be sized. Unlike every other hedgeActionNone
	// cause (both flat, basis already matches, an existing hedge's reduce
	// deferred as dust), this one leaves a HELD primary with NO hedge at
	// all — on a fresh-open cycle that is exactly the fail-open constraint
	// 4 exists to prevent, so runHedgeSync must treat it like an open-order
	// failure, not a benign no-op (review finding).
	hedgeActionOpenUnavailable
)

// hedgeSnapshot is the primary+hedge state hedgeTargetDecision needs,
// captured under the caller's RLock so the decision is computed from one
// consistent read.
type hedgeSnapshot struct {
	PrimaryQty  float64
	PrimarySide string // "long"/"short"; "" when primary is flat
	HedgeQty    float64
	HedgeSide   string // "long"/"short"; "" when hedge is flat
	HedgeBasis  float64
}

// hedgeAction is the target hedgeTargetDecision computes for the current
// cycle. Kind == hedgeActionNone means no order should be placed this cycle
// (Reason explains why, for logging — not always an error).
type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string // Position.Side for open/add ("long"/"short"); unused for reduce/close
	Reason string
}

// hedgeOpenNotionalQty sizes a hedge open/add leg by notional exposure:
// hedge_notional = qty * px * ratio; hedge_qty = hedge_notional / hedgePx.
// Returns ok=false when any input makes the result meaningless (fail-closed
// — the caller must never fall back to a guessed size).
func hedgeOpenNotionalQty(qty, px, ratio, hedgePx float64) (float64, bool) {
	if qty <= 0 || px <= 0 || hedgePx <= 0 || ratio <= 0 {
		return 0, false
	}
	hedgeQty := (qty * px * ratio) / hedgePx
	if hedgeQty <= 0 {
		return 0, false
	}
	return hedgeQty, true
}

// hedgeProportionalBasis returns the HedgePrimaryQtyBasis watermark after an
// action that intended to move the basis from oldBasis toward
// targetPrimaryQty by executing intendedQty (hedge-coin units) but actually
// achieved achievedQty (the real fill size — paper always achieves the full
// intendedQty; live can partially fill). A full/over fill converges exactly
// to targetPrimaryQty (identical to always stamping the target, the
// pre-existing behavior); a partial fill advances the basis by only the
// achieved fraction of the gap, so the next cycle's delta captures the
// unfilled shortfall/residual instead of freezing a partial-fill error as
// fully converged (review finding — this is the fill-level counterpart of
// the cycle-level basis-never-advances bug fixed earlier in this PR).
func hedgeProportionalBasis(oldBasis, targetPrimaryQty, intendedQty, achievedQty float64) float64 {
	if intendedQty <= 0 || achievedQty >= intendedQty {
		return targetPrimaryQty
	}
	frac := achievedQty / intendedQty
	if frac < 0 {
		frac = 0
	}
	return oldBasis + (targetPrimaryQty-oldBasis)*frac
}

// hedgeReduceQty returns the hedge quantity to reduce by by, proportional to
// how far the primary quantity has fallen below the basis it was last
// hedged against. newPrimaryQty <= 0 (primary flat) returns the full hedge
// quantity (a full close, handled by the caller as closeFull).
func hedgeReduceQty(hedgeQty, basisQty, newPrimaryQty float64) float64 {
	if hedgeQty <= 0 || basisQty <= 0 {
		return 0
	}
	if newPrimaryQty <= hedgeQtyEpsilon {
		return hedgeQty
	}
	frac := (basisQty - newPrimaryQty) / basisQty
	if frac <= 0 {
		return 0
	}
	if frac >= 1 {
		return hedgeQty
	}
	return hedgeQty * frac
}

// hedgeTargetDecision computes the hedge action for the current cycle from a
// snapshot of primary+hedge state and the current marks. Pure and
// Python-free (repo testing rule) — every branch is unit-testable without a
// subprocess or lock.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeActionNone}
	}
	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	if !primaryHeld {
		if hedgeHeld {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary flat"}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}

	desiredSide := hedgeSideForPrimary(snap.PrimarySide)

	if hedgeHeld && snap.HedgeSide != desiredSide {
		// Defense-in-depth: unreachable in phase 1 (direction="both" is
		// rejected at config load, so the primary side can never flip
		// mid-flight), but a hedge must never sit on the wrong side.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "hedge side mismatch (defense-in-depth)"}
	}

	if !hedgeHeld {
		qty, ok := hedgeOpenNotionalQty(snap.PrimaryQty, primaryPx, hedgeRatio(sc), hedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeActionOpenUnavailable, Reason: "unusable price for hedge open"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: desiredSide}
	}

	delta := snap.PrimaryQty - snap.HedgeBasis
	switch {
	case delta > hedgeQtyEpsilon:
		qty, ok := hedgeOpenNotionalQty(delta, primaryPx, hedgeRatio(sc), hedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeActionNone, Reason: "unusable price for hedge add"}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: desiredSide}
	case delta < -hedgeQtyEpsilon:
		if hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "unusable price for hedge reduce"}
		}
		reduceQty := hedgeReduceQty(snap.HedgeQty, snap.HedgeBasis, snap.PrimaryQty)
		if reduceQty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone}
		}
		if reduceQty >= snap.HedgeQty-hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary reduced to ~0 relative to hedge basis"}
		}
		if reduceQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: "reduce below min order notional, deferred (basis not advanced)"}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: reduceQty}
	default:
		return hedgeAction{Kind: hedgeActionNone}
	}
}

// applyHedgeOpenOrAddFill books a confirmed (live) or modeled (paper) hedge
// open/add fill into virtual state. Must be called under the strategy's
// owning mu.Lock(). Mirrors the perps fresh-open cash/fee handling in
// executePerpsSignalWithLeverage: only the fee leaves cash, notional stays
// virtual (margin-based accounting). newBasis is the primary quantity this
// hedge leg is now sized against — the watermark the next cycle's decision
// diffs against.
func applyHedgeOpenOrAddFill(s *StrategyState, sc StrategyConfig, hCoin, side string, fillQty, fillPx, fillFee float64, fillOID string, useFillFee bool, newBasis float64) {
	if fillQty <= 0 || fillPx <= 0 {
		return
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fee := CalculatePlatformSpotFee(feePlatform, fillQty*fillPx)
	feeSource := FeeSourceModeled
	if useFillFee {
		fee = fillFee
		feeSource = FeeSourceUserFills
	}
	s.Cash -= fee
	now := time.Now().UTC()
	primaryCoin := hyperliquidConfiguredCoin(sc)

	pos, exists := s.Positions[hCoin]
	if !exists || pos == nil {
		positionID := newTradePositionID(s.ID, hCoin, now)
		pos = &Position{
			Symbol:               hCoin,
			Quantity:             fillQty,
			InitialQuantity:      fillQty,
			AvgCost:              fillPx,
			Side:                 side,
			Multiplier:           1, // perps 1:1 contract size — PnL-branch in PortfolioValue
			Leverage:             hedgeExchangeLeverage(sc),
			OwnerStrategyID:      s.ID,
			OpenedAt:             now,
			TradePositionID:      positionID,
			HedgeFor:             primaryCoin,
			HedgePrimaryQtyBasis: newBasis,
		}
		s.Positions[hCoin] = pos
	} else {
		oldQty := pos.Quantity
		newQty := oldQty + fillQty
		if newQty > 0 {
			pos.AvgCost = (oldQty*pos.AvgCost + fillQty*fillPx) / newQty
		}
		pos.Quantity = newQty
		pos.InitialQuantity += fillQty
		pos.HedgePrimaryQtyBasis = newBasis
	}

	positionID := ensurePositionTradeID(s.ID, hCoin, pos)
	tradeSide := "buy"
	if side == "short" {
		tradeSide = "sell"
	}
	var openOID string
	if useFillFee {
		openOID = fillOID
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hCoin,
		PositionID:      positionID,
		Side:            tradeSide,
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("Hedge %s for %s (ratio=%.2f)", tradeSide, primaryCoin, hedgeRatio(sc)),
		ExchangeOrderID: openOID,
		ExchangeFee:     fee,
		FeeSource:       feeSource,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
}

// applyHedgeReduceFill books a hedge partial reduce via the generalized
// perps partial-close booker — pos.HedgeFor (stamped at open) routes
// TradeType/RiskState/diagnostics correctly with no hedge-specific logic
// needed there. Must be called under Lock. newBasis is the primary quantity
// this reduce brought the hedge's watermark down to; it MUST be advanced
// here (mirroring applyHedgeOpenOrAddFill) — otherwise the next cycle's
// hedgeTargetDecision still sees the stale (larger) basis, computes another
// positive reduce fraction against the now-smaller hedge, and the leg decays
// geometrically every cycle instead of converging after one reduce.
func applyHedgeReduceFill(s *StrategyState, hCoin string, closeQty, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, newBasis float64, logger *StrategyLogger) bool {
	ok := bookPerpsPartialCloseWithFillFee(s, hCoin, closeQty, closePx, fillFee, useFillFee, exchangeOrderID, reason, "Hedge reduce", "Hedge reduce", logger)
	if ok {
		if pos, exists := s.Positions[hCoin]; exists && pos != nil {
			pos.HedgePrimaryQtyBasis = newBasis
		}
	}
	return ok
}

// applyHedgeCloseFill books a full hedge close via the generalized perps
// close booker. Must be called under Lock.
func applyHedgeCloseFill(s *StrategyState, hCoin string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, hCoin, closePx, fillFee, useFillFee, exchangeOrderID, reason, "Hedge close", "Hedge close", logger)
}

// runHyperliquidHedgeOpenOrAdd submits the live HL order for a hedge
// open/add. Margin mode + leverage are passed only on a genuine fresh open
// (isFreshOpen) — HL rejects update_leverage on an already-open position,
// and collision validation guarantees this strategy is the hedge coin's
// sole owner so there is never a peer's margin setting to preserve.
func runHyperliquidHedgeOpenOrAdd(sc StrategyConfig, hCoin, side string, qty float64, isFreshOpen bool) (*HyperliquidExecuteResult, error) {
	execSide := "buy"
	if side == "short" {
		execSide = "sell"
	}
	var marginMode string
	var leverage float64
	if isFreshOpen {
		marginMode = hedgeMarginMode(sc)
		leverage = hedgeExchangeLeverage(sc)
	}
	result, _, err := RunHyperliquidExecute(sc.Script, hCoin, execSide, qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
	return result, err
}

// unwindPrimaryAfterHedgeOpenFailure implements the phase-1 fail-closed
// policy (issue constraint 4): a confirmed primary fill on a fresh-open
// cycle whose hedge open failed (a live order failure, OR the hedge open
// couldn't even be sized — e.g. an unusable/missing hedge-coin price) must
// never leave the strategy running unhedged silently. On live it submits a
// SIZED (never a full/sz=None) reduce-only close of the primary fill —
// sized because the primary coin may be shared with peer strategies, unlike
// the hedge coin — cancelling its just-armed SL/TP OIDs. On paper there is
// no live order to fail or unwind, so it books a direct virtual close at
// primaryPx instead (mirrors how a paper hedge fill is applied directly,
// with no subprocess call). Either way it DMs the owner CRITICAL. If the
// unwind itself also fails (paper: primaryPx unusable too; live: the close
// order fails), virtual state is left unchanged (the primary fill was real)
// and the NEXT cycle's hedge sync retries the hedge open against the
// still-open primary — no new latch state, restart-safe degraded loop.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primaryCoin string, fillQty, primaryPx float64, live bool, notifier *MultiNotifier, logger *StrategyLogger) {
	if !live {
		mu.Lock()
		ok := bookPerpsCloseWithFillFee(s, primaryCoin, primaryPx, 0, false, "", "hedge_open_failed_unwind", "Hedge-open-failed primary unwind (paper)", "Hedge-open-failed primary unwind (paper)", logger)
		mu.Unlock()
		msg := fmt.Sprintf("[CRITICAL] %s: hedge open FAILED (unusable hedge price) with primary %s open this cycle — unwinding primary at mark (paper, phase-1 fail-closed policy, #1159)", sc.ID, primaryCoin)
		if !ok {
			msg += "; UNWIND ALSO FAILED (no usable primary mark to book a virtual close) — primary remains open and UNHEDGED; the next cycle's hedge sync will retry the hedge open"
		} else {
			msg += fmt.Sprintf("; primary unwound @ $%.4f (paper)", primaryPx)
		}
		if logger != nil {
			logger.Error("%s", msg)
		}
		if notifier != nil {
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
		return
	}

	mu.RLock()
	var cancelOIDs []int64
	if pos, ok := s.Positions[primaryCoin]; ok && pos != nil {
		if pos.StopLossOID > 0 {
			cancelOIDs = append(cancelOIDs, pos.StopLossOID)
		}
		cancelOIDs = append(cancelOIDs, pos.TPOIDs...)
		if fillQty <= 0 || fillQty > pos.Quantity {
			fillQty = pos.Quantity
		}
	}
	mu.RUnlock()
	if fillQty <= 0 {
		return
	}

	partialSz := fillQty
	result, _, err := RunHyperliquidClose(sc.Script, primaryCoin, &partialSz, cancelOIDs)
	msg := fmt.Sprintf("[CRITICAL] %s: hedge open FAILED after primary %s fill confirmed — unwinding primary reduce-only (phase-1 fail-closed policy, #1159)", sc.ID, primaryCoin)
	if err != nil || result == nil || result.Close == nil || result.Close.Fill == nil || result.Close.Fill.TotalSz <= 0 {
		errMsg := "no fill"
		switch {
		case err != nil:
			errMsg = err.Error()
		case result != nil && result.Error != "":
			errMsg = result.Error
		}
		msg += fmt.Sprintf("; UNWIND ALSO FAILED (%s) — primary remains open and UNHEDGED; the next cycle's hedge sync will retry the hedge open", errMsg)
		if logger != nil {
			logger.Error("%s", msg)
		}
		if notifier != nil {
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
		return
	}
	fill := result.Close.Fill
	var fillOID string
	if fill.OID != 0 {
		fillOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	bookPerpsCloseWithFillFee(s, primaryCoin, fill.AvgPx, fill.Fee, true, fillOID, "hedge_open_failed_unwind", "Hedge-open-failed primary unwind", "Hedge-open-failed primary unwind", logger)
	mu.Unlock()
	msg += fmt.Sprintf("; primary unwound @ $%.4f qty=%.6f", fill.AvgPx, fill.TotalSz)
	if logger != nil {
		logger.Error("%s", msg)
	}
	if notifier != nil {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

// runHedgeSync is the per-cycle state-derived hedge reconciler for one HL
// perps strategy (#1159). Snapshots primary+hedge state under RLock,
// computes the target action via hedgeTargetDecision, executes unlocked
// (live) or applies directly at mark (paper), then applies the result under
// Lock. freshOpenThisCycle marks whether the primary transitioned flat→open
// on THIS cycle (the Phase-1 snapshot read flat, and a trade executed) — a
// failed hedge OPEN on that cycle escalates to the fail-closed primary
// unwind; a failed hedge open on any other cycle (config just enabled,
// restart recovery) or a failed ADD only alerts and retries, since the
// state-derived design makes the retry automatic.
//
// Deliberately NOT gated by pause/daily-loss-hold/exposure-cap: those are
// signal-level entry gates, and a hedge order is a coupled risk-management
// leg, not a signal. A paused/held primary can only ever be flat or
// shrinking under those states, so hedge sync naturally only reduces/closes
// while they're active — never opens/adds.
func runHedgeSync(sc StrategyConfig, s *StrategyState, prices map[string]float64, mu *sync.RWMutex, freshOpenThisCycle bool, notifier *MultiNotifier, logger *StrategyLogger) {
	if !sc.HedgeEnabled() {
		return
	}
	primaryCoin := hyperliquidConfiguredCoin(sc)
	hCoin := hedgeCoin(sc)
	if primaryCoin == "" || hCoin == "" {
		return
	}

	mu.RLock()
	var snap hedgeSnapshot
	if pos, ok := s.Positions[primaryCoin]; ok && pos != nil {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	if pos, ok := s.Positions[hCoin]; ok && pos != nil {
		snap.HedgeQty = pos.Quantity
		snap.HedgeSide = pos.Side
		snap.HedgeBasis = pos.HedgePrimaryQtyBasis
	}
	mu.RUnlock()

	primaryPx := prices[primaryCoin]
	hedgePx := prices[hCoin]
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	live := hyperliquidIsLive(sc.Args)
	if action.Kind == hedgeActionNone {
		if action.Reason != "" && logger != nil {
			logger.Info("Hedge sync %s: no action (%s)", hCoin, action.Reason)
		}
		return
	}
	if action.Kind == hedgeActionOpenUnavailable {
		// The primary is held with NO hedge at all and no order could even be
		// sized (unusable/missing hedge-coin price) — on a fresh open this is
		// the same fail-open constraint 4 exists to prevent, so it must
		// escalate exactly like a failed hedge-open order, not log-and-return
		// like every other no-op (review finding).
		if freshOpenThisCycle {
			if logger != nil {
				logger.Warn("Hedge sync %s: %s on a fresh-open cycle — escalating to fail-closed primary unwind", hCoin, action.Reason)
			}
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, snap.PrimaryQty, primaryPx, live, notifier, logger)
		} else if logger != nil {
			logger.Warn("Hedge open unavailable for %s: %s — hedge sync will retry next cycle", hCoin, action.Reason)
		}
		return
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		isFreshOpen := action.Kind == hedgeActionOpen
		oldBasis := snap.HedgeBasis
		if isFreshOpen {
			oldBasis = 0 // no primary was hedged before this cycle
		}
		if !live {
			// Paper always "fills" the full intended qty — proportional basis
			// reduces to the target exactly like the pre-existing behavior.
			mu.Lock()
			applyHedgeOpenOrAddFill(s, sc, hCoin, action.Side, action.Qty, hedgePx, 0, "", false, snap.PrimaryQty)
			mu.Unlock()
			return
		}
		result, err := runHyperliquidHedgeOpenOrAdd(sc, hCoin, action.Side, action.Qty, isFreshOpen)
		if err != nil || result == nil || result.Error != "" || result.Execution == nil || result.Execution.Fill == nil || result.Execution.Fill.TotalSz <= 0 {
			errMsg := "no fill"
			switch {
			case err != nil:
				errMsg = err.Error()
			case result != nil && result.Error != "":
				errMsg = result.Error
			}
			notifyLiveExecFailure(notifier, sc, "hedge-open", hCoin, errMsg)
			if isFreshOpen && freshOpenThisCycle {
				unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, snap.PrimaryQty, primaryPx, live, notifier, logger)
			} else if logger != nil {
				action := "add"
				if isFreshOpen {
					action = "open"
				}
				logger.Warn("Hedge %s failed for %s: %s — hedge sync will retry next cycle", action, hCoin, errMsg)
			}
			return
		}
		clearLiveExecThrottle(sc, "hedge-open", hCoin)
		fill := result.Execution.Fill
		var fillOID string
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		newBasis := hedgeProportionalBasis(oldBasis, snap.PrimaryQty, action.Qty, fill.TotalSz)
		mu.Lock()
		applyHedgeOpenOrAddFill(s, sc, hCoin, action.Side, fill.TotalSz, fill.AvgPx, fill.Fee, fillOID, true, newBasis)
		mu.Unlock()

	case hedgeActionReduce, hedgeActionCloseFull:
		full := action.Kind == hedgeActionCloseFull
		if !live {
			mu.Lock()
			if full {
				applyHedgeCloseFill(s, hCoin, hedgePx, 0, false, "", "hedge_close", logger)
			} else {
				applyHedgeReduceFill(s, hCoin, action.Qty, hedgePx, 0, false, "", "hedge_reduce", snap.PrimaryQty, logger)
			}
			mu.Unlock()
			return
		}
		var partialSz *float64
		if !full {
			q := action.Qty
			partialSz = &q
		}
		result, _, err := RunHyperliquidClose(sc.Script, hCoin, partialSz, nil)
		if err != nil || result == nil || result.Close == nil || result.Close.Fill == nil || result.Close.Fill.TotalSz <= 0 {
			if result != nil && result.Close != nil && result.Close.AlreadyFlat {
				// Already flat on-chain (e.g. externally closed) — clear
				// whatever virtual hedge state remains at the last mark so
				// the sync doesn't retry a close against a coin with no
				// position.
				mu.Lock()
				applyHedgeCloseFill(s, hCoin, hedgePx, 0, false, "", "hedge_already_flat", logger)
				mu.Unlock()
				return
			}
			errMsg := "no fill"
			switch {
			case err != nil:
				errMsg = err.Error()
			case result != nil && result.Error != "":
				errMsg = result.Error
			}
			notifyLiveExecFailure(notifier, sc, "hedge-close", hCoin, errMsg)
			return
		}
		clearLiveExecThrottle(sc, "hedge-close", hCoin)
		fill := result.Close.Fill
		var fillOID string
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		reason := "hedge_reduce"
		if full {
			reason = "hedge_close"
		}
		mu.Lock()
		if full {
			applyHedgeCloseFill(s, hCoin, fill.AvgPx, fill.Fee, true, fillOID, reason, logger)
		} else {
			newBasis := hedgeProportionalBasis(snap.HedgeBasis, snap.PrimaryQty, action.Qty, fill.TotalSz)
			applyHedgeReduceFill(s, hCoin, fill.TotalSz, fill.AvgPx, fill.Fee, true, fillOID, reason, newBasis, logger)
		}
		mu.Unlock()
	}
}
