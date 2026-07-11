package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// #1159 correlated hedge legs — phase 1 (Hyperliquid perps only).
//
// Hedge management is a per-cycle, STATE-DERIVED reconciler ("hedge sync"), not
// a set of per-event mirror hooks. A single pure function (hedgeTargetDecision)
// computes the hedge target from the CURRENT primary position versus a persisted
// quantity watermark (Position.HedgePrimaryQtyBasis on the hedge leg), and one
// orchestrator (runHedgeSync) converges the hedge leg to it every HL dispatch
// cycle. This makes every primary lifecycle event — fresh open, scale-in add,
// evaluator partial close, on-chain SL/TP fill detected by reconcile, ratchet
// close, external close — automatically produce the matching hedge action
// within the same or next cycle, without touching each close path individually.
//
// Key invariants:
//   - Qty-event mirroring, not price mirroring: the target keys on the primary
//     QUANTITY vs. the basis watermark, so mark-price drift never re-trades the
//     hedge; only a real primary quantity or side change does.
//   - Fill-confirmed state mutation only: the hedge virtual position mutates
//     only from confirmed fills (applyHedgeFill), mirroring the live-exec guard.
//   - Fail-closed: an unusable price on a cycle that WANTS to open/add returns a
//     no-op with a Reason so the caller can escalate (a fresh-open cycle unwinds
//     the primary; a manage cycle alerts + retries next cycle).

// hedgeMinOrderNotionalUSD is the floor below which a hedge REDUCE is deferred
// (Hyperliquid rejects sub-$10 orders). A closeFull is never dust-deferred — it
// clears the whole leg. The basis is intentionally NOT advanced on a deferred
// reduce so the shortfall accumulates and retries.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeQtyEpsilon guards float comparisons of primary quantity against the
// basis watermark so a rounding-scale delta doesn't trigger a phantom add/reduce.
const hedgeQtyEpsilon = 1e-9

type hedgeActionKind int

const (
	hedgeActionNone      hedgeActionKind = iota // nothing to do (in sync, or fail-closed no-op)
	hedgeActionOpen                             // hedge flat, primary held → open inverse leg
	hedgeActionAdd                              // primary grew past basis → add to the hedge leg
	hedgeActionReduce                           // primary shrank below basis → partial-close the hedge leg
	hedgeActionCloseFull                        // primary flat (or hedge on the wrong side) → flatten the hedge leg
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
	default:
		return "none"
	}
}

// hedgeSnapshot captures the primary + hedge state needed to decide a hedge
// action. Populated under the Phase-1 RLock so the decision runs against a
// consistent view.
type hedgeSnapshot struct {
	PrimarySymbol string  // the strategy's primary coin
	PrimaryQty    float64 // unsigned primary position quantity (0 = flat)
	PrimarySide   string  // "long"/"short"/"" when flat
	HedgeSymbol   string  // the configured hedge coin
	HedgeQty      float64 // unsigned hedge position quantity (0 = flat)
	HedgeSide     string  // "long"/"short"/"" when flat
	HedgeBasis    float64 // Position.HedgePrimaryQtyBasis on the hedge leg (0 when hedge flat)
}

// hedgeAction is the decision output. Qty is the unsigned hedge quantity to
// trade; Side is the target side for an open/add. TargetPrimaryBasis is the
// primary quantity the hedge leg should record as its watermark after a FULL
// fill (applyHedgeFill scales it for partial fills). Reason carries a
// fail-closed / dust-defer explanation for none actions the caller may escalate
// or alert on.
type hedgeAction struct {
	Kind               hedgeActionKind
	Qty                float64
	Side               string
	TargetPrimaryBasis float64
	Reason             string
}

// inverseSide returns the hedge side opposite the given primary side.
func inverseSide(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

// hedgeTargetDecision computes the hedge action to converge the hedge leg to
// the primary position. Pure — no I/O, no locks. primaryPx / hedgePx are the
// current marks (used only for open/add notional sizing; close/reduce are
// quantity-derived and don't need a price).
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone}
	}
	ratio := HedgeRatio(sc)
	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	// Primary flat: the hedge must be flat too.
	if !primaryHeld {
		if hedgeHeld {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary flat"}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}

	wantSide := inverseSide(snap.PrimarySide)
	if wantSide == "" {
		// Primary held but side unknown — cannot safely mirror. Fail closed.
		return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("primary %s has unknown side", snap.PrimarySymbol)}
	}

	// Hedge held on the WRONG side (should be unreachable with direction="both"
	// rejected, but defense-in-depth): flatten and let the next cycle re-open
	// on the correct side.
	if hedgeHeld && snap.HedgeSide != wantSide {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: fmt.Sprintf("hedge side %q != required %q", snap.HedgeSide, wantSide)}
	}

	// Fresh open: hedge flat, primary held.
	if !hedgeHeld {
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("unusable price for hedge open (primary=%.6f hedge=%.6f)", primaryPx, hedgePx)}
		}
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge open qty <= 0"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: wantSide, TargetPrimaryBasis: snap.PrimaryQty}
	}

	// Hedge held on the correct side — converge on quantity events vs. basis.
	delta := snap.PrimaryQty - snap.HedgeBasis
	if delta > hedgeQtyEpsilon {
		// Primary grew (scale-in add) → add to the hedge on the delta notional.
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("unusable price for hedge add (primary=%.6f hedge=%.6f)", primaryPx, hedgePx)}
		}
		addQty := delta * primaryPx * ratio / hedgePx
		if addQty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge add qty <= 0"}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: addQty, Side: wantSide, TargetPrimaryBasis: snap.PrimaryQty}
	}
	if -delta > hedgeQtyEpsilon {
		// Primary shrank (partial close) → reduce the hedge proportionally.
		if snap.HedgeBasis <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge basis <= 0, cannot compute reduce fraction"}
		}
		fraction := (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if fraction > 1 {
			fraction = 1
		}
		reduceQty := fraction * snap.HedgeQty
		if reduceQty >= snap.HedgeQty-hedgeQtyEpsilon {
			// Primary fully closed via partial legs down to ~0 — flatten.
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary reduced to ~0", TargetPrimaryBasis: snap.PrimaryQty}
		}
		// Dust guard: a sub-min-notional reduce is deferred (basis unchanged so
		// it accumulates and retries next cycle). Uses the hedge mark when
		// available; when the price is unusable we cannot size the notional, so
		// conservatively defer as well.
		if hedgePx <= 0 || reduceQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("hedge reduce %.6f below min notional $%.2f — deferred", reduceQty, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: reduceQty, TargetPrimaryBasis: snap.PrimaryQty}
	}

	// In sync.
	return hedgeAction{Kind: hedgeActionNone}
}

// hedgeOrderSkipReason re-checks the decision preconditions against a snapshot
// immediately before spawning a hedge order, so an on-chain fill can never land
// without a matching Trade record (mirrors the {Perps,Spot,Futures}OrderSkipReason
// guards). Returns a non-empty reason when the action should be SKIPPED.
func hedgeOrderSkipReason(sc StrategyConfig, action hedgeAction, snap hedgeSnapshot, primaryPx, hedgePx float64) string {
	if !HedgeEnabled(sc) {
		return "hedge not enabled"
	}
	if action.Kind == hedgeActionNone {
		return "no hedge action"
	}
	if action.Qty <= 0 {
		return "hedge action qty <= 0"
	}
	// Recompute the decision and require the same kind — a state change between
	// snapshot and spawn (concurrent reconcile, external fill) must abort.
	fresh := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if fresh.Kind != action.Kind {
		return fmt.Sprintf("hedge decision changed %s -> %s before spawn", action.Kind, fresh.Kind)
	}
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		if primaryPx <= 0 || hedgePx <= 0 {
			return "unusable price at spawn"
		}
		if action.Side == "" {
			return "empty hedge side"
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if snap.HedgeQty <= 0 {
			return "no hedge position to reduce/close"
		}
	}
	return ""
}

// hedgeOpenTradeSide maps a hedge position side to the order/trade side used to
// OPEN or ADD to it (long → buy, short → sell).
func hedgeOpenTradeSide(side string) string {
	if side == "short" {
		return "sell"
	}
	return "buy"
}

// applyHedgeOpenOrAddFill books a CONFIRMED hedge open/add fill into virtual
// state under the caller's mu.Lock. Only a positive fillQty reaches here (the
// live-exec guard: fill-confirmed state mutation only). It creates the hedge
// Position on a fresh open, or blends price+size on an add, advances the primary
// quantity watermark proportionally to what actually filled (partial-fill safe),
// deducts the fee from cash (perps: only the fee leaves cash, notional stays
// virtual), and records a "hedge" open Trade attributed to the owning strategy.
func applyHedgeOpenOrAddFill(s *StrategyState, sc StrategyConfig, action hedgeAction, hedgeSymbol, primarySymbol string, fillPx, fillQty, fillFee float64, fillOID string, useFillFee bool, logger *StrategyLogger) {
	if s == nil || fillPx <= 0 || fillQty <= 0 {
		return
	}
	notional := fillQty * fillPx
	feePlatform := s.Platform
	fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
	s.Cash -= fee
	now := time.Now().UTC()

	pos, exists := s.Positions[hedgeSymbol]
	requested := action.Qty
	fillFraction := 1.0
	if requested > 0 && fillQty < requested {
		fillFraction = fillQty / requested
	}
	if !exists || pos == nil || pos.Quantity <= 0 {
		// Fresh open. Basis is the primary quantity actually hedged: scale the
		// target by the fill fraction so a partial fill records a proportional
		// watermark (the shortfall re-hedges next cycle).
		positionID := newTradePositionID(s.ID, hedgeSymbol, now)
		basis := action.TargetPrimaryBasis * fillFraction
		s.Positions[hedgeSymbol] = &Position{
			Symbol:               hedgeSymbol,
			Quantity:             fillQty,
			InitialQuantity:      fillQty,
			AvgCost:              fillPx,
			Side:                 action.Side,
			Multiplier:           1,
			Leverage:             hedgeLeverage(sc),
			OwnerStrategyID:      sc.ID,
			OpenedAt:             now,
			TradePositionID:      positionID,
			HedgeFor:             primarySymbol,
			HedgePrimaryQtyBasis: basis,
		}
		pos = s.Positions[hedgeSymbol]
	} else {
		// Add: blend price+size, advance the basis by the primary delta actually
		// hedged (proportional to the fill).
		oldBasis := pos.HedgePrimaryQtyBasis
		newQty := pos.Quantity + fillQty
		if newQty > 0 {
			pos.AvgCost = (pos.Quantity*pos.AvgCost + fillQty*fillPx) / newQty
		}
		pos.Quantity = newQty
		pos.InitialQuantity += fillQty
		pos.HedgePrimaryQtyBasis = oldBasis + (action.TargetPrimaryBasis-oldBasis)*fillFraction
		// Keep the coupling metadata coherent.
		pos.HedgeFor = primarySymbol
		pos.Side = action.Side
	}

	openOID := ""
	if useFillFee {
		openOID = fillOID
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeSymbol,
		PositionID:      pos.TradePositionID,
		Side:            hedgeOpenTradeSide(action.Side),
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           notional,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.2f (fee $%.2f)", primarySymbol, action.Kind, action.Side, fillQty, fillPx, fee),
		ExchangeOrderID: openOID,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fee, useFillFee),
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("HEDGE %s %s %.6f @ $%.4f (for %s, basis %.6f, fee $%.2f)", action.Kind, action.Side, fillQty, fillPx, primarySymbol, pos.HedgePrimaryQtyBasis, fee)
	}
}

// hedgeReduceProportionalQty returns the hedge quantity to close when the
// primary is closed by closeFraction (0,1] — used by the manual force-close
// partial path where the hedge must mirror a proportional primary reduction
// deterministically rather than waiting a cycle. Clamped to the held qty.
func hedgeReduceProportionalQty(hedgeQty, closeFraction float64) float64 {
	if hedgeQty <= 0 || closeFraction <= 0 {
		return 0
	}
	q := hedgeQty * math.Min(closeFraction, 1)
	if q > hedgeQty {
		q = hedgeQty
	}
	return q
}

// snapshotHedgeState captures the primary + hedge position state for a strategy
// under the caller's read lock. Returns the snapshot and whether the strategy
// carries an enabled hedge with a resolvable coin.
func snapshotHedgeState(s *StrategyState, sc StrategyConfig) (hedgeSnapshot, bool) {
	if s == nil || !HedgeEnabled(sc) {
		return hedgeSnapshot{}, false
	}
	primarySym := hyperliquidConfiguredCoin(sc)
	hedgeSym := hedgeCoin(sc)
	if primarySym == "" || hedgeSym == "" {
		return hedgeSnapshot{}, false
	}
	snap := hedgeSnapshot{PrimarySymbol: primarySym, HedgeSymbol: hedgeSym}
	if p := s.Positions[primarySym]; p != nil && p.Quantity > 0 {
		snap.PrimaryQty = p.Quantity
		snap.PrimarySide = p.Side
	}
	if h := s.Positions[hedgeSym]; h != nil && h.Quantity > 0 {
		snap.HedgeQty = h.Quantity
		snap.HedgeSide = h.Side
		snap.HedgeBasis = h.HedgePrimaryQtyBasis
	}
	return snap, true
}

// runHedgeSync is the per-cycle hedge orchestrator (#1159). It converges the
// hedge leg to the primary position: snapshot under RLock → decide → spawn the
// order unlocked (live) or synthesize a paper fill → apply the confirmed fill
// under Lock. On a FRESH primary open whose hedge OPEN fails, it escalates to a
// fail-closed unwind of the primary leg (never runs unhedged silently). Returns
// the number of hedge trades booked.
//
// Lock discipline mirrors runHyperliquidProtectionSync: the exclusive lock is
// held only for the snapshot and the apply, never across the subprocess spawn.
func runHedgeSync(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, primaryPx, hedgePx float64, freshOpenPrimary bool) int {
	if s == nil || mu == nil || !HedgeEnabled(sc) {
		return 0
	}
	isLive := hyperliquidIsLive(sc.Args)

	mu.RLock()
	snap, ok := snapshotHedgeState(s, sc)
	mu.RUnlock()
	if !ok {
		return 0
	}

	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if action.Kind == hedgeActionNone {
		// A fail-closed no-op on a fresh-open cycle (unusable price) must not run
		// the primary unhedged: escalate to unwind. Other none reasons (dust
		// deferral, in-sync) just retry next cycle.
		if freshOpenPrimary && action.Reason != "" && snap.PrimaryQty > 0 && snap.HedgeQty == 0 {
			if logger != nil {
				logger.Warn("HEDGE could not open on fresh primary open (%s) — unwinding primary (fail-closed)", action.Reason)
			}
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, notifier, logger, snap.PrimarySymbol, action.Reason)
		}
		return 0
	}
	if skip := hedgeOrderSkipReason(sc, action, snap, primaryPx, hedgePx); skip != "" {
		if logger != nil {
			logger.Info("HEDGE skip %s: %s", action.Kind, skip)
		}
		return 0
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		return runHedgeOpenOrAdd(sc, s, mu, notifier, logger, action, snap, primaryPx, hedgePx, isLive, freshOpenPrimary)
	case hedgeActionReduce, hedgeActionCloseFull:
		return runHedgeReduceOrClose(sc, s, mu, logger, action, snap, hedgePx, isLive)
	}
	return 0
}

// runHedgeOpenOrAdd spawns and books a hedge open/add. On live-open failure of a
// fresh primary open it escalates to the primary unwind (fail-closed).
func runHedgeOpenOrAdd(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, action hedgeAction, snap hedgeSnapshot, primaryPx, hedgePx float64, isLive, freshOpenPrimary bool) int {
	hedgeSym := snap.HedgeSymbol
	primarySym := snap.PrimarySymbol
	side := hedgeOpenTradeSide(action.Side)

	var fillPx, fillQty, fillFee float64
	var fillOID string
	var useFillFee bool

	if isLive {
		// Margin mode + leverage only on a fresh open (HL rejects update_leverage
		// on an open position); an add inherits the open-time assignment.
		marginMode := ""
		leverage := 0.0
		if action.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeLeverage(sc)
		}
		execResult, _, err := RunHyperliquidExecute(sc.Script, hedgeSym, side, action.Qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
		if err != nil || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil || execResult.Execution.Fill.TotalSz <= 0 {
			// Fail closed. On a fresh primary open, unwind the primary.
			reason := "hedge open order failed"
			if err != nil {
				reason = fmt.Sprintf("hedge open order failed: %v", err)
			}
			if logger != nil {
				logger.Error("HEDGE %s %s failed: %s", action.Kind, hedgeSym, reason)
			}
			notifyLiveExecFailure(notifier, sc, "hedge", hedgeSym, reason)
			if action.Kind == hedgeActionOpen && freshOpenPrimary {
				unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, notifier, logger, primarySym, reason)
			}
			return 0
		}
		clearLiveExecThrottle(sc, "hedge", hedgeSym)
		fill := execResult.Execution.Fill
		fillPx = fill.AvgPx
		fillQty = fill.TotalSz
		fillFee = fill.Fee
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		useFillFee = true
	} else {
		// Paper: synthesize a fill at the slipped hedge mark, modeled fee.
		fillPx = ApplySlippage(hedgePx)
		if fillPx <= 0 {
			return 0
		}
		fillQty = action.Qty
	}

	mu.Lock()
	defer mu.Unlock()
	// Re-check under the exclusive lock. For a live confirmed fill we must ALWAYS
	// book (never drop an on-chain fill) — warn if the state drifted. For paper
	// the fill is synthetic, so honor a changed decision by skipping.
	fresh, freshOK := snapshotHedgeState(s, sc)
	if freshOK {
		reDecision := hedgeTargetDecision(sc, fresh, primaryPx, hedgePx)
		if reDecision.Kind != action.Kind {
			if !isLive {
				if logger != nil {
					logger.Info("HEDGE %s aborted before apply: decision changed to %s", action.Kind, reDecision.Kind)
				}
				return 0
			}
			if logger != nil {
				logger.Warn("HEDGE %s: state drifted before apply (now %s) — booking confirmed fill anyway", action.Kind, reDecision.Kind)
			}
		}
	}
	applyHedgeOpenOrAddFill(s, sc, action, hedgeSym, primarySym, fillPx, fillQty, fillFee, fillOID, useFillFee, logger)
	return 1
}

// runHedgeReduceOrClose spawns and books a hedge reduce/close via the shared
// perps close-booking path (already hedge-routed for trade_type + risk result).
func runHedgeReduceOrClose(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, logger *StrategyLogger, action hedgeAction, snap hedgeSnapshot, hedgePx float64, isLive bool) int {
	hedgeSym := snap.HedgeSymbol
	full := action.Kind == hedgeActionCloseFull

	var closePx, closeFee float64
	var closeOID string
	var useFillFee bool

	if isLive {
		var partialSz *float64
		if !full {
			q := action.Qty
			partialSz = &q
		}
		closeResult, _, err := RunHyperliquidClose(sc.Script, hedgeSym, partialSz, nil)
		if err != nil || closeResult == nil || closeResult.Close == nil {
			if logger != nil {
				logger.Error("HEDGE %s %s failed: %v", action.Kind, hedgeSym, err)
			}
			return 0
		}
		if closeResult.Close.AlreadyFlat {
			// Nothing on-chain to close — the next reconcile/sync converges state.
			return 0
		}
		if closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
			return 0
		}
		fill := closeResult.Close.Fill
		closePx = fill.AvgPx
		closeFee = fill.Fee
		if fill.OID != 0 {
			closeOID = fmt.Sprintf("%d", fill.OID)
		}
		useFillFee = true
	} else {
		closePx = ApplySlippage(hedgePx)
		if closePx <= 0 {
			return 0
		}
	}

	mu.Lock()
	defer mu.Unlock()
	detail := fmt.Sprintf("hedge(%s) %s", snap.PrimarySymbol, action.Kind)
	if full {
		if bookPerpsCloseWithFillFee(s, hedgeSym, closePx, closeFee, useFillFee, closeOID, "hedge_close", detail, detail, logger) {
			return 1
		}
		return 0
	}
	if bookPerpsPartialCloseWithFillFee(s, hedgeSym, action.Qty, closePx, closeFee, useFillFee, closeOID, "hedge_reduce", detail, detail, logger) {
		// Keep the basis watermark aligned with the primary after a partial
		// hedge reduce so the next cycle diffs against the new target.
		if h := s.Positions[hedgeSym]; h != nil {
			h.HedgePrimaryQtyBasis = action.TargetPrimaryBasis
		}
		return 1
	}
	return 0
}

// unwindPrimaryAfterHedgeOpenFailure is the fail-closed disposition when a
// primary fill confirmed but the hedge open failed (#1159 constraint 4): close
// the primary leg reduce-only (cancelling its just-armed SL/TP OIDs) and alert
// the operator CRITICALLY. Runs under mu.Lock taken here. If the unwind order
// itself fails, state is left unchanged and the next-cycle hedge sync retries —
// no silent unhedged running, restart-safe with no new latch state.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, primarySymbol, reason string) {
	if s == nil || mu == nil {
		return
	}
	isLive := hyperliquidIsLive(sc.Args)

	mu.RLock()
	pos := s.Positions[primarySymbol]
	var qty float64
	var cancelOIDs []int64
	if pos != nil {
		qty = pos.Quantity
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()
	if pos == nil || qty <= 0 {
		return
	}

	var closePx, closeFee float64
	var closeOID string
	var useFillFee bool
	if isLive {
		sz := qty
		closeResult, _, err := RunHyperliquidClose(sc.Script, primarySymbol, &sz, cancelOIDs)
		if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
			msg := fmt.Sprintf("⚠️ CRITICAL: strategy %s primary %s could NOT be unwound after a hedge-open failure (%s); the primary may be running UNHEDGED. Intervene now. Close error: %v", sc.ID, primarySymbol, reason, err)
			if logger != nil {
				logger.Error("%s", msg)
			}
			if notifier != nil {
				notifier.SendOwnerDM(msg)
				notifier.SendToAllChannels(msg)
			}
			return
		}
		fill := closeResult.Close.Fill
		closePx = fill.AvgPx
		closeFee = fill.Fee
		if fill.OID != 0 {
			closeOID = fmt.Sprintf("%d", fill.OID)
		}
		useFillFee = true
	} else {
		mu.RLock()
		closePx = pos.AvgCost
		mu.RUnlock()
	}

	mu.Lock()
	booked := bookPerpsCloseWithFillFee(s, primarySymbol, closePx, closeFee, useFillFee, closeOID, "hedge_open_failed_unwind", "Unwind after hedge-open failure", "Unwind after hedge-open failure", logger)
	mu.Unlock()

	msg := fmt.Sprintf("⚠️ CRITICAL: strategy %s hedge OPEN failed (%s) — primary %s was unwound reduce-only to avoid running unhedged (#1159).", sc.ID, reason, primarySymbol)
	if !booked {
		msg = fmt.Sprintf("⚠️ CRITICAL: strategy %s hedge OPEN failed (%s); primary %s unwind submitted but virtual close did not book — verify on-chain state (#1159).", sc.ID, reason, primarySymbol)
	}
	if logger != nil {
		logger.Warn("%s", msg)
	}
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}
