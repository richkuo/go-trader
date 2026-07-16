package main

import (
	"fmt"
	"sync"
	"time"
)

// hedgeTradeType tags a hedge leg's Trade rows distinctly from ordinary
// "perps" trades so lifetime stats can exclude the open leg from the #T
// round-trip count (db.go) and trade_diagnostics can skip capture (#1159).
// StrategyID on these rows is always the OWNING strategy's own ID, so
// trade_pnl.go's ledger sums (which never filter by trade_type) attribute
// hedge PnL/fees to that strategy automatically — no separate plumbing.
const hedgeTradeType = "hedge"

// hedgeQtyEpsilon guards float comparisons on quantities throughout the
// hedge decision core (mirrors the 1e-9 tolerance used elsewhere in
// portfolio.go for "effectively zero/unchanged" quantity checks).
const hedgeQtyEpsilon = 1e-9

// hedgeMinOrderNotionalUSD is the dust floor below which a hedge REDUCE is
// deferred rather than submitted (HL's practical minimum order size). The
// basis watermark is deliberately NOT advanced on a deferred reduce, so the
// deferred amount accumulates across cycles until it clears the floor or the
// primary changes further. closeFull always executes regardless of size —
// there's no "defer forever" outcome for fully unwinding the hedge.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeActionKind enumerates the mutually exclusive outcomes of
// hedgeTargetDecision.
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
	default:
		return "none"
	}
}

// hedgeSnapshot is the qty-event-mirroring state hedgeTargetDecision diffs
// against. Captured under the caller's lock (RLock is sufficient — the
// decision core never mutates) from the SAME StrategyState the primary
// position lives in: the hedge leg is a second entry in s.Positions, keyed
// by the hedge coin, distinguished from an ordinary position by a non-empty
// HedgeFor field.
type hedgeSnapshot struct {
	PrimaryQty     float64
	PrimarySide    string
	PrimaryAvgCost float64
	HedgeQty       float64
	HedgeSide      string
	HedgeAvgCost   float64
	// HedgeBasis is the primary qty this hedge leg was last sized against
	// (Position.HedgePrimaryQtyBasis) — the watermark, not the current
	// primary qty.
	HedgeBasis float64
}

// hedgeSnapshotFor builds a hedgeSnapshot from live strategy state. Callers
// hold at least mu.RLock() (the decision core never mutates). The hedge leg
// is only recognized when HedgeFor matches primarySym — defense-in-depth
// against ever reading an unrelated position under the hedge coin key
// (unreachable in practice: hedge coin collisions are rejected at config
// validation, so no other position should ever occupy that key).
func hedgeSnapshotFor(s *StrategyState, primarySym, hedgeCoinKey string) hedgeSnapshot {
	var snap hedgeSnapshot
	if s == nil {
		return snap
	}
	if pos, ok := s.Positions[primarySym]; ok && pos != nil {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
		snap.PrimaryAvgCost = pos.AvgCost
	}
	if pos, ok := s.Positions[hedgeCoinKey]; ok && pos != nil && pos.HedgeFor == primarySym {
		snap.HedgeQty = pos.Quantity
		snap.HedgeSide = pos.Side
		snap.HedgeAvgCost = pos.AvgCost
		snap.HedgeBasis = pos.HedgePrimaryQtyBasis
	}
	return snap
}

// hedgeAction is the mirror decision for one cycle: what to do to converge
// the hedge leg to the current primary state. Side is the resulting/target
// hedge POSITION side ("long"/"short") — for open/add it's the side to
// establish or add to; for reduce/closeFull it's the EXISTING hedge side
// being reduced (informational; the close RPC itself is symbol-scoped, not
// side-scoped).
type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string
	Reason string
}

// inverseHedgeSide returns the hedge's target side given the primary's
// current side — the only side mapping phase 1 supports (HedgeSideInverse).
func inverseHedgeSide(primarySide string) string {
	if primarySide == "short" {
		return "long"
	}
	return "short"
}

// hedgeOrderSideForPositionSide maps a target/resulting POSITION side to the
// HL order side needed to establish or grow it (open/add only — reduce/close
// orders are symbol-scoped RunHyperliquidClose calls with no side param).
func hedgeOrderSideForPositionSide(side string) string {
	if side == "long" {
		return "buy"
	}
	return "sell"
}

// hedgeTargetDecision computes the single hedge action, if any, that
// converges the hedge leg to the current primary state (#1159). Pure and
// side-effect free — every branch is independently unit-testable without a
// subprocess or lock. Fails closed (returns hedgeActionNone with a Reason)
// whenever a price needed to size an order is unusable, rather than ever
// guessing a notional.
//
// Decision table:
//   - primary flat, hedge flat            -> none
//   - primary flat, hedge held            -> closeFull (unconditional, no dust guard)
//   - primary held, hedge flat            -> open (side = inverse of primary; notional = primaryQty*primaryPx*ratio)
//   - primary held, hedge wrong side      -> closeFull (defense-in-depth; unreachable while direction="both" is rejected together with hedge)
//   - primary held, hedge held, qty grew  -> add (delta-notional sizing)
//   - primary held, hedge held, qty shrank -> reduce (proportional; dust-deferred below hedgeMinOrderNotionalUSD, basis NOT advanced on defer)
//   - primary held, hedge held, qty same  -> none
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge not enabled"}
	}

	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	if !primaryHeld {
		if hedgeHeld {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "primary flat"}
		}
		return hedgeAction{Kind: hedgeActionNone, Reason: "both flat"}
	}

	expectedSide := inverseHedgeSide(snap.PrimarySide)

	if !hedgeHeld {
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "unusable price for hedge open"}
		}
		notional := snap.PrimaryQty * primaryPx * HedgeRatio(sc)
		qty := notional / hedgePx
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge open qty <= 0"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: expectedSide, Reason: "primary opened"}
	}

	if snap.HedgeSide != expectedSide {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide,
			Reason: fmt.Sprintf("hedge side %q does not match expected inverse %q of primary side %q — closing for re-open next cycle", snap.HedgeSide, expectedSide, snap.PrimarySide)}
	}

	basis := snap.HedgeBasis
	if basis <= 0 {
		// No watermark yet (should not happen in normal operation — every hedge
		// open stamps HedgePrimaryQtyBasis immediately). Fail closed: take no
		// action rather than guess a basis, and let the next cycle's watermark
		// (once persisted) resume normal diffing.
		return hedgeAction{Kind: hedgeActionNone, Reason: "no hedge basis watermark yet"}
	}

	delta := snap.PrimaryQty - basis
	switch {
	case delta > hedgeQtyEpsilon:
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "unusable price for hedge add"}
		}
		deltaNotional := delta * primaryPx * HedgeRatio(sc)
		qty := deltaNotional / hedgePx
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge add qty <= 0"}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: expectedSide, Reason: "primary added"}
	case delta < -hedgeQtyEpsilon:
		fraction := (basis - snap.PrimaryQty) / basis
		if fraction > 1 {
			fraction = 1
		}
		qty := fraction * snap.HedgeQty
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge reduce qty <= 0"}
		}
		if hedgePx > 0 && qty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge reduce notional below HL minimum order size — deferring (basis not advanced)"}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty, Side: expectedSide, Reason: "primary reduced"}
	default:
		return hedgeAction{Kind: hedgeActionNone, Reason: "no qty change since last hedge sync"}
	}
}

// hedgeOrderSkipReason re-derives the hedge decision from a freshly captured
// snapshot immediately before the hedge order subprocess is spawned,
// mirroring the {Perps,Spot,Futures}OrderSkipReason convention (CLAUDE.md):
// the outer dispatch decided to act off a snapshot that may now be stale (a
// concurrent async writer — funding ingestion, diagnostics — mutated the
// SAME strategy's positions between the RLock snapshot and the no-lock
// subprocess call), so re-check right before the on-chain call rather than
// ever place an order the caller no longer expects a Trade record for.
// Returns "" when the fresh snapshot still supports the planned action.
func hedgeOrderSkipReason(planned hedgeAction, sc StrategyConfig, freshSnap hedgeSnapshot, primaryPx, hedgePx float64) string {
	fresh := hedgeTargetDecision(sc, freshSnap, primaryPx, hedgePx)
	if fresh.Kind != planned.Kind {
		return fmt.Sprintf("hedge state changed since decision (planned %s, now %s: %s)", planned.Kind, fresh.Kind, fresh.Reason)
	}
	return ""
}

// hedgeFillResult normalizes the two live-order RPC result shapes
// (HyperliquidExecuteResult / HyperliquidCloseResult) into one struct so
// runHedgeSync's post-processing doesn't need to know which RPC produced it.
type hedgeFillResult struct {
	Qty         float64
	Px          float64
	Fee         float64
	OID         int64
	AlreadyFlat bool
}

// runHedgeOrder submits the live order for action and normalizes the result.
// No lock is held; the caller MUST re-validate under mu.Lock() before
// mutating state (a fill on the wire is not yet booked). Margin mode /
// leverage are only sent on a fresh hedge OPEN — HL rejects update_leverage
// on an already-open position, mirroring the primary's own #486 fresh-open-only rule.
func runHedgeOrder(sc StrategyConfig, coin string, action hedgeAction, walletSnapshot hlExecuteSnapshot) (*hedgeFillResult, string, error) {
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		side := hedgeOrderSideForPositionSide(action.Side)
		marginMode := ""
		leverage := 0.0
		if action.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeLeverage(sc)
		}
		execResult, stderr, err := RunHyperliquidExecute(sc.Script, coin, side, action.Qty, 0, 0, 0, marginMode, leverage, false, walletSnapshot)
		if err != nil {
			return nil, stderr, err
		}
		if execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
			errMsg := ""
			if execResult != nil {
				errMsg = execResult.Error
			}
			return nil, stderr, fmt.Errorf("hedge %s %s: no fill (%s)", action.Kind, coin, errMsg)
		}
		fill := execResult.Execution.Fill
		if fill.TotalSz <= 0 || fill.AvgPx <= 0 {
			return nil, stderr, fmt.Errorf("hedge %s %s: empty fill", action.Kind, coin)
		}
		return &hedgeFillResult{Qty: fill.TotalSz, Px: fill.AvgPx, Fee: fill.Fee, OID: fill.OID}, stderr, nil
	case hedgeActionReduce, hedgeActionCloseFull:
		sz := action.Qty
		closeResult, stderr, err := RunHyperliquidClose(sc.Script, coin, &sz, nil)
		if err != nil {
			return nil, stderr, err
		}
		if closeResult == nil || closeResult.Close == nil {
			errMsg := ""
			if closeResult != nil {
				errMsg = closeResult.Error
			}
			return nil, stderr, fmt.Errorf("hedge close %s: no result (%s)", coin, errMsg)
		}
		if closeResult.Close.AlreadyFlat {
			return &hedgeFillResult{AlreadyFlat: true}, stderr, nil
		}
		fill := closeResult.Close.Fill
		if fill == nil || fill.TotalSz <= 0 || fill.AvgPx <= 0 {
			return nil, stderr, fmt.Errorf("hedge close %s: empty fill", coin)
		}
		return &hedgeFillResult{Qty: fill.TotalSz, Px: fill.AvgPx, Fee: fill.Fee, OID: fill.OID}, stderr, nil
	default:
		return nil, "", fmt.Errorf("runHedgeOrder: unexpected action kind %v", action.Kind)
	}
}

// hedgeBasisAfterFill returns the updated HedgePrimaryQtyBasis watermark
// after a hedge fill, prorating convergence toward primaryQtyAtEvent by the
// fraction of the intended order (requestedQty, i.e. action.Qty) that
// actually filled (filledQty). A thin book or slippage cap can fill an HL
// market order short of what was requested; stamping the basis as if the
// full requested qty had filled would make next cycle's delta computation
// see zero change (primary qty unchanged since the snapshot) and never
// retry the shortfall, silently leaving the leg under- (open/add) or
// over- (reduce) hedged with no operator signal (#1159 review). Prorating
// leaves the unfilled remainder as a live delta the next cycle picks up.
func hedgeBasisAfterFill(oldBasis, primaryQtyAtEvent, requestedQty, filledQty float64) float64 {
	if requestedQty <= hedgeQtyEpsilon {
		return primaryQtyAtEvent
	}
	fraction := filledQty / requestedQty
	if fraction > 1 {
		fraction = 1
	} else if fraction < 0 {
		fraction = 0
	}
	return oldBasis + fraction*(primaryQtyAtEvent-oldBasis)
}

// applyHedgeOpenOrAddFill books a confirmed hedge open/add fill under
// mu.Lock(). Bespoke (not a reuse of the primary's ExecutePerpsSignalWithLeverage
// path) because a hedge leg never flips, carries no SL/TP, and doesn't
// participate in scale-in caps — the blend math is the plain weighted-average
// every position open/add uses. Returns 1 on a successful book (mirrors the
// repo's "trades executed" convention), 0 otherwise.
func applyHedgeOpenOrAddFill(s *StrategyState, sc StrategyConfig, coin string, action hedgeAction, fillQty, fillPx, fillFee float64, fillOID int64, primaryQtyAtEvent float64) int {
	if s == nil || fillQty <= 0 || fillPx <= 0 {
		return 0
	}
	primarySym := hyperliquidSymbol(sc.Args)
	pos, exists := s.Positions[coin]
	if !exists || pos == nil {
		pos = &Position{
			Symbol:          coin,
			Side:            action.Side,
			OwnerStrategyID: sc.ID,
			OpenedAt:        time.Now().UTC(),
			Multiplier:      1,
			Leverage:        hedgeLeverage(sc),
			HedgeFor:        primarySym,
		}
		s.Positions[coin] = pos
	}
	oldBasis := pos.HedgePrimaryQtyBasis
	newQty := pos.Quantity + fillQty
	if pos.Quantity <= 0 {
		pos.AvgCost = fillPx
		pos.InitialQuantity = fillQty
		pos.Side = action.Side
		pos.HedgeFor = primarySym
		pos.OwnerStrategyID = sc.ID
		pos.OpenedAt = time.Now().UTC()
	} else {
		pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*fillQty) / newQty
	}
	pos.Quantity = newQty
	pos.HedgePrimaryQtyBasis = hedgeBasisAfterFill(oldBasis, primaryQtyAtEvent, action.Qty, fillQty)

	positionID := ensurePositionTradeID(sc.ID, coin, pos)
	var exchangeOID string
	if fillOID != 0 {
		exchangeOID = fmt.Sprintf("%d", fillOID)
	}
	RecordTrade(s, Trade{
		Timestamp:       time.Now().UTC(),
		StrategyID:      sc.ID,
		Symbol:          coin,
		PositionID:      positionID,
		Side:            hedgeOrderSideForPositionSide(action.Side),
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       hedgeTradeType,
		Details:         fmt.Sprintf("hedge(%s) %s", primarySym, action.Reason),
		ExchangeOrderID: exchangeOID,
		ExchangeFee:     fillFee,
	})
	return 1
}

// applyHedgeReduceOrCloseFill books a confirmed hedge reduce/close fill under
// mu.Lock() by delegating to bookPerpsPartialCloseWithFillFee — it already
// clamps the close qty to the live position, deletes the position and calls
// recordClosedPosition when the remainder is ~0 (so it's safe to use even
// when a "reduce" fill turns out to fully flatten the leg), gives the #954
// dup-OID guard for free, and (via its #1159 hedge-routing addition) tags
// trade_type="hedge" and books PnL through RecordHedgeTradeResult rather than
// RecordTradeResult because pos.HedgeFor is non-empty. Advances the basis
// watermark toward primaryQtyAtEvent (prorated by fill fraction via
// hedgeBasisAfterFill, #1159 review) when the leg survives as a partial
// reduce — a short fill leaves the excess hedge as a live delta the next
// cycle retries, rather than being stamped as fully converged.
func applyHedgeReduceOrCloseFill(s *StrategyState, sc StrategyConfig, coin string, action hedgeAction, fillQty, fillPx, fillFee float64, fillOID int64, primaryQtyAtEvent float64, logger *StrategyLogger) bool {
	primarySym := hyperliquidSymbol(sc.Args)
	var exchangeOID string
	if fillOID != 0 {
		exchangeOID = fmt.Sprintf("%d", fillOID)
	}
	reason := "hedge_reduce"
	if action.Kind == hedgeActionCloseFull {
		reason = "hedge_close"
	}
	detailsPrefix := fmt.Sprintf("hedge(%s) %s", primarySym, action.Reason)
	ok := bookPerpsPartialCloseWithFillFee(s, coin, fillQty, fillPx, fillFee, true, exchangeOID, reason, detailsPrefix, "Hedge close", logger)
	if ok && action.Kind == hedgeActionReduce {
		if pos, stillOpen := s.Positions[coin]; stillOpen && pos != nil {
			pos.HedgePrimaryQtyBasis = hedgeBasisAfterFill(pos.HedgePrimaryQtyBasis, primaryQtyAtEvent, action.Qty, fillQty)
		}
	}
	return ok
}

// unwindPrimaryAfterHedgeOpenFailure implements the phase-1 fail-closed
// policy (#1159 constraint 4): the primary fill confirmed this cycle but the
// hedge open failed — immediately reduce-only close the primary fill
// (cancelling its just-armed SL/TP OIDs) and alert the operator, rather than
// run unhedged silently. The close is SIZED to fillQty, never a full
// market_close(sz=None): the primary coin may have shared-coin peers, so
// only this fill's quantity may be closed (mirrors shouldCloseFullPosition's
// peer-awareness elsewhere). If the unwind itself fails, state is left
// unchanged and the CRITICAL DM below is the only signal — the position
// still exists so the next cycle's hedge sync tries to open the hedge again
// against the same (unchanged) primary qty.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, stratState *StrategyState, mu *sync.RWMutex, primarySym string, fillQty float64, notifier *MultiNotifier, logger *StrategyLogger) {
	if fillQty <= 0 {
		return
	}
	mu.RLock()
	pos := stratState.Positions[primarySym]
	var cancelOIDs []int64
	if pos != nil {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()
	if pos == nil {
		return
	}

	sz := fillQty
	closeResult, stderr, err := RunHyperliquidClose(sc.Script, primarySym, &sz, cancelOIDs)
	msgPrefix := fmt.Sprintf("CRITICAL: hedge open failed for strategy %s after primary %s filled %.6f — attempting fail-closed unwind", sc.ID, primarySym, fillQty)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil {
			errMsg = closeResult.Error
		}
		msg := fmt.Sprintf("%s — UNWIND ALSO FAILED (error=%q stderr=%q). Strategy %s is running an UNHEDGED position on %s. Manual intervention required.",
			msgPrefix, errMsg, stderr, sc.ID, primarySym)
		if logger != nil {
			logger.Error("%s", msg)
		}
		if notifier != nil {
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
		return
	}

	fill := closeResult.Close.Fill
	var exchangeOID string
	if fill.OID != 0 {
		exchangeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	bookPerpsCloseWithFillFee(stratState, primarySym, fill.AvgPx, fill.Fee, true, exchangeOID, "hedge_open_failed_unwind", "Hedge-open-failed unwind close", "Hedge-open-failed unwind", logger)
	mu.Unlock()

	msg := fmt.Sprintf("%s — primary position closed (unwound) at $%.4f. No hedge was ever opened; this is a coupled risk-management event, not a strategy failure.", msgPrefix, fill.AvgPx)
	if logger != nil {
		logger.Warn("%s", msg)
	}
	if notifier != nil {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

// runHedgeSync is the per-cycle hedge reconciler: it converges the hedge leg
// to the primary's current state by mirroring runHyperliquidProtectionSync's
// locking shape — snapshot under RLock, decide with no lock held, spawn the
// live order subprocess with no lock held, re-validate and apply under
// Lock() — rather than scattering per-event mirror hooks across every
// primary lifecycle path (#1159). freshOpenThisCycle must be true only when
// THIS cycle produced a brand-new primary open (never for an add/reduce/manage
// cycle) — it gates the fail-closed unwind-on-hedge-open-failure policy.
// Returns true when a hedge action was applied this cycle (informational).
func runHedgeSync(sc StrategyConfig, stratState *StrategyState, mu *sync.RWMutex, primaryPx float64, prices map[string]float64, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger, freshOpenThisCycle bool) bool {
	if !HedgeEnabled(sc) || stratState == nil || mu == nil {
		return false
	}
	primarySym := hyperliquidSymbol(sc.Args)
	coin := hedgeCoin(sc)
	if primarySym == "" || coin == "" {
		return false
	}

	mu.RLock()
	snap := hedgeSnapshotFor(stratState, primarySym, coin)
	mu.RUnlock()

	hedgePx := prices[coin]
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if action.Kind == hedgeActionNone {
		return false
	}

	mu.RLock()
	freshSnap := hedgeSnapshotFor(stratState, primarySym, coin)
	mu.RUnlock()
	if reason := hedgeOrderSkipReason(action, sc, freshSnap, primaryPx, hedgePx); reason != "" {
		if logger != nil {
			logger.Info("hedge sync %s: %s", coin, reason)
		}
		return false
	}

	directionTag := "hedge_" + action.Kind.String()
	walletSnapshot := hlExecuteSnapshotForCoin(hlPositions, coin)
	result, stderr, err := runHedgeOrder(sc, coin, action, walletSnapshot)
	if stderr != "" && logger != nil {
		logger.Info("hedge %s %s stderr: %s", action.Kind, coin, stderr)
	}
	if err != nil || result == nil {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		notifyLiveExecFailure(notifier, sc, directionTag, coin, errMsg)
		if action.Kind == hedgeActionOpen && freshOpenThisCycle {
			unwindPrimaryAfterHedgeOpenFailure(sc, stratState, mu, primarySym, snap.PrimaryQty, notifier, logger)
		}
		return false
	}
	if result.AlreadyFlat {
		// Reconcile clears the stale virtual leg next cycle; nothing to book.
		return false
	}
	clearLiveExecThrottle(sc, directionTag, coin)

	mu.Lock()
	defer mu.Unlock()
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		return applyHedgeOpenOrAddFill(stratState, sc, coin, action, result.Qty, result.Px, result.Fee, result.OID, snap.PrimaryQty) > 0
	case hedgeActionReduce, hedgeActionCloseFull:
		return applyHedgeReduceOrCloseFill(stratState, sc, coin, action, result.Qty, result.Px, result.Fee, result.OID, snap.PrimaryQty, logger)
	default:
		return false
	}
}
