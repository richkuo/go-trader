package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Correlated hedge legs (#1159, phase 1). A hedge-enabled HL perps strategy
// auto-manages an inverse position on a DIFFERENT (collision-validated) coin.
// The whole mechanism is a per-cycle, STATE-DERIVED reconciler: runHedgeSync
// compares the current primary position against a persisted quantity watermark
// (Position.HedgePrimaryQtyBasis on the hedge leg) and converges the hedge to
// its target every dispatch cycle. Because it is state-derived, every primary
// lifecycle event — fresh open, scale-in add, evaluator partial/full close,
// on-chain SL/TP fill booked by reconcile, kill-switch/CB force-close, external
// close, or a crash between legs — automatically produces the matching hedge
// action within the same or next cycle, without touching each close path.
//
// Invariants:
//   - Qty-event mirroring, not price mirroring: the hedge re-trades only when the
//     primary QUANTITY changes past the watermark. Mark drift never re-trades it.
//   - Fill-confirmed state mutation only: live hedge state mutates solely from a
//     confirmed fill (mirrors runHyperliquidExecuteOrder's ok2=false contract).
//   - Fail-closed open: a primary fill confirmed + hedge open failed on that
//     fresh-open cycle immediately reduce-only unwinds the primary fill (cancels
//     its just-armed SL/TP OIDs) + CRITICAL owner DM. Later reconcile-drift
//     cycles alert and retry instead of unwinding an aged position.
//   - Sole ownership by construction: validateHedgeConfigs guarantees a hedge
//     coin is never any strategy's configured coin and never shared between
//     hedgers, so every shared-coin mechanism keyed on hyperliquidConfiguredCoin
//     stays correct without seeing hedge coins.

// hedgeMinOrderNotionalUSD is the ~min HL order notional. A reduce/add below it
// is deferred (WARN) so the watermark accumulates instead of spamming
// unfillable dust orders. A full close and a fresh open always execute.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeQtyEpsilon guards the primary-qty watermark comparison against float
// rounding so an unchanged primary never re-trades the hedge.
const hedgeQtyEpsilon = 1e-9

type hedgeActionKind int

const (
	hedgeNone hedgeActionKind = iota
	hedgeOpen
	hedgeAdd
	hedgeReduce
	hedgeCloseFull
)

// hedgeSnapshot is the primary+hedge state captured under the Phase-1 RLock.
type hedgeSnapshot struct {
	PrimaryQty     float64
	PrimarySide    string
	PrimaryAvgCost float64
	HedgeQty       float64
	HedgeSide      string
	HedgeAvgCost   float64
	HedgeBasis     float64 // HedgePrimaryQtyBasis on the hedge leg
}

// hedgeAction is the pure decision core's verdict for one cycle.
type hedgeAction struct {
	Kind hedgeActionKind
	// Side is the hedge POSITION side for open ("long"/"short").
	Side string
	// RequestedQty is the hedge-coin quantity to open/add/reduce/close.
	RequestedQty float64
	// PrimaryQtyTarget is the primary quantity this action syncs the hedge to;
	// the applied watermark is interpolated from BasisBefore toward this by the
	// actual fill fraction (handles partial fills).
	PrimaryQtyTarget float64
	BasisBefore      float64
	Reason           string
	// FailClosed is set when the decision WANTED to open/add but a required
	// price was unusable — the caller escalates (fresh-open → primary unwind;
	// manage cycle → alert + retry).
	FailClosed bool
}

// formatHedgeConfig renders a hedge block for operator surfaces (reload
// changelog, inspect). "none" for a nil/disabled block.
func formatHedgeConfig(h *HedgeConfig) string {
	if h == nil || !h.Enabled {
		return "none"
	}
	sc := StrategyConfig{Hedge: h}
	side := h.Side
	if side == "" {
		side = "inverse"
	}
	return fmt.Sprintf("%s×%g(%s,%s,%gx)", hedgeCoin(sc), hedgeRatio(sc), side, hedgeMarginMode(sc), hedgeLeverage(sc))
}

// hedgeExecuteSide maps a hedge position side to the exchange order side used to
// OPEN/increase it: a long position opens with a buy, a short with a sell.
func hedgeExecuteSide(hedgeSide string) string {
	if hedgeSide == "short" {
		return "sell"
	}
	return "buy"
}

// hedgeOpenQty converts a primary notional to a hedge-coin quantity:
// qty = primaryQty × primaryPx × ratio / hedgePx. ok=false when a price is
// unusable or the result rounds to ≤0.
func hedgeOpenQty(primaryQty, primaryPx, ratio, hedgePx float64) (float64, bool) {
	if primaryQty <= 0 || primaryPx <= 0 || hedgePx <= 0 || ratio <= 0 {
		return 0, false
	}
	qty := primaryQty * primaryPx * ratio / hedgePx
	if qty <= 0 {
		return 0, false
	}
	return qty, true
}

// hedgeReduceQty computes the hedge-coin quantity to reduce when the primary
// shrank from basis to primaryQty: fraction (basis-primaryQty)/basis of the
// current hedge qty, capped at the full hedge qty. Returns the full hedge qty
// when the primary is (near) flat.
func hedgeReduceQty(hedgeQty, basis, primaryQty float64) float64 {
	if hedgeQty <= 0 || basis <= 0 {
		return 0
	}
	if primaryQty <= 0 {
		return hedgeQty
	}
	frac := (basis - primaryQty) / basis
	if frac <= 0 {
		return 0
	}
	if frac >= 1 {
		return hedgeQty
	}
	q := frac * hedgeQty
	if q > hedgeQty {
		q = hedgeQty
	}
	return q
}

// hedgeTargetDecision is the pure decision core. Given the strategy's hedge
// config, the captured snapshot, and the current primary/hedge prices, it
// returns the single action that converges the hedge to its target for this
// cycle. Pure and Python-free (repo testing rule).
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeNone}
	}
	ratio := hedgeRatio(sc)

	// Primary flat.
	if snap.PrimaryQty <= 0 {
		if snap.HedgeQty > 0 {
			return hedgeAction{Kind: hedgeCloseFull, RequestedQty: snap.HedgeQty, PrimaryQtyTarget: 0, BasisBefore: snap.HedgeBasis, Reason: "primary_flat"}
		}
		return hedgeAction{Kind: hedgeNone}
	}

	desiredSide := hedgeSideForPrimary(snap.PrimarySide)
	if desiredSide == "" {
		// Unknown primary side — fail safe, take no hedge action.
		return hedgeAction{Kind: hedgeNone, Reason: "unknown_primary_side"}
	}

	// Primary held, hedge flat → open.
	if snap.HedgeQty <= 0 {
		qty, ok := hedgeOpenQty(snap.PrimaryQty, primaryPx, ratio, hedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeNone, FailClosed: true, Reason: "unusable_price_open"}
		}
		return hedgeAction{Kind: hedgeOpen, Side: desiredSide, RequestedQty: qty, PrimaryQtyTarget: snap.PrimaryQty, BasisBefore: 0, Reason: "primary_open"}
	}

	// Hedge held with the wrong side (defense-in-depth; unreachable with
	// direction=both rejected) → flatten, re-open inverse next cycle.
	if snap.HedgeSide != desiredSide {
		return hedgeAction{Kind: hedgeCloseFull, RequestedQty: snap.HedgeQty, PrimaryQtyTarget: snap.PrimaryQty, BasisBefore: snap.HedgeBasis, Reason: "wrong_side"}
	}

	basis := snap.HedgeBasis
	if basis <= 0 {
		// Legacy/corrupt watermark — can't diff safely; leave as-is.
		return hedgeAction{Kind: hedgeNone, Reason: "no_basis"}
	}

	// Primary grew → add.
	if snap.PrimaryQty > basis+hedgeQtyEpsilon {
		deltaPrimary := snap.PrimaryQty - basis
		addQty, ok := hedgeOpenQty(deltaPrimary, primaryPx, ratio, hedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeNone, FailClosed: true, Reason: "unusable_price_add"}
		}
		if addQty*hedgePx < hedgeMinOrderNotionalUSD {
			// Dust add — defer WITHOUT advancing the basis so it accumulates.
			return hedgeAction{Kind: hedgeNone, Reason: "add_dust_deferred"}
		}
		return hedgeAction{Kind: hedgeAdd, Side: desiredSide, RequestedQty: addQty, PrimaryQtyTarget: snap.PrimaryQty, BasisBefore: basis, Reason: "primary_add"}
	}

	// Primary shrank → reduce.
	if snap.PrimaryQty < basis-hedgeQtyEpsilon {
		reduceQty := hedgeReduceQty(snap.HedgeQty, basis, snap.PrimaryQty)
		if reduceQty <= 0 {
			return hedgeAction{Kind: hedgeNone}
		}
		// Dust reduce — defer WITHOUT advancing the basis so it retries and
		// eventually clears in one order. A full close is exempt (handled by the
		// primary-flat branch above).
		refPx := hedgePx
		if refPx <= 0 {
			refPx = snap.HedgeAvgCost
		}
		if refPx > 0 && reduceQty*refPx < hedgeMinOrderNotionalUSD && reduceQty < snap.HedgeQty {
			return hedgeAction{Kind: hedgeNone, Reason: "reduce_dust_deferred"}
		}
		return hedgeAction{Kind: hedgeReduce, RequestedQty: reduceQty, PrimaryQtyTarget: snap.PrimaryQty, BasisBefore: basis, Reason: "primary_reduce"}
	}

	return hedgeAction{Kind: hedgeNone}
}

// hedgeAppliedBasis interpolates the watermark from basisBefore toward the
// target by the actual fill fraction, so a partial fill advances the basis only
// by what actually hedged (never assume the requested size filled).
func hedgeAppliedBasis(basisBefore, target, requestedQty, filledQty float64) float64 {
	if requestedQty <= 0 {
		return target
	}
	frac := filledQty / requestedQty
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	return basisBefore + (target-basisBefore)*frac
}

// runHedgeSync is the per-cycle hedge orchestrator. It snapshots the primary +
// hedge state under RLock, computes the target action, spawns the order
// unlocked (live) or books directly (paper), and applies the confirmed fill
// under Lock — the same 6-phase discipline as runHyperliquidProtectionSync.
//
// freshOpen is true only on a flat→open cycle for the primary; a hedge-open
// failure on such a cycle escalates to unwindPrimaryAfterHedgeOpenFailure.
func runHedgeSync(sc StrategyConfig, stratState *StrategyState, primarySym string, mu *sync.RWMutex, prices map[string]float64, freshOpen bool, notifier *MultiNotifier, logger *StrategyLogger) {
	if !sc.HedgeEnabled() || stratState == nil {
		return
	}
	hCoin := hedgeCoin(sc)
	if hCoin == "" || hCoin == primarySym {
		return
	}

	// Phase 1: snapshot under RLock.
	mu.RLock()
	var snap hedgeSnapshot
	if p := stratState.Positions[primarySym]; p != nil && p.Quantity > 0 {
		snap.PrimaryQty = p.Quantity
		snap.PrimarySide = p.Side
		snap.PrimaryAvgCost = p.AvgCost
	}
	if h := stratState.Positions[hCoin]; h != nil && h.HedgeFor != "" && h.Quantity > 0 {
		snap.HedgeQty = h.Quantity
		snap.HedgeSide = h.Side
		snap.HedgeAvgCost = h.AvgCost
		snap.HedgeBasis = h.HedgePrimaryQtyBasis
	}
	primaryPx := prices[primarySym]
	if primaryPx <= 0 {
		primaryPx = snap.PrimaryAvgCost
	}
	hedgePx := prices[hCoin]
	mu.RUnlock()

	decision := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if decision.Kind == hedgeNone {
		if decision.FailClosed {
			if freshOpen {
				unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, snap.PrimaryQty, mu, notifier, logger, decision.Reason)
			} else {
				hedgeAlert(notifier, logger, fmt.Sprintf("[%s] hedge %s sync could not size the hedge (%s) — will retry next cycle", sc.ID, hCoin, decision.Reason))
			}
		} else if decision.Reason == "add_dust_deferred" || decision.Reason == "reduce_dust_deferred" {
			if logger != nil {
				logger.Warn("hedge %s: %s (below $%.0f min order) — deferring, watermark not advanced", hCoin, decision.Reason, hedgeMinOrderNotionalUSD)
			}
		}
		return
	}

	live := hyperliquidIsLive(sc.Args)

	// Paper: no order spawn, no failure path — book directly at the hedge mark.
	if !live {
		if hedgePx <= 0 {
			if logger != nil {
				logger.Warn("hedge %s: no paper mark available — deferring %s to next cycle", hCoin, decision.Reason)
			}
			return
		}
		mu.Lock()
		applyHedgeFillLocked(sc, stratState, primarySym, hCoin, decision, hedgePx, 0, 0, true, logger)
		mu.Unlock()
		return
	}

	// Live: spawn the order unlocked.
	execFill, ok := runHedgeOrder(sc, hCoin, decision, notifier, logger)
	if !ok || execFill == nil || execFill.TotalSz <= 0 {
		// Fill not confirmed → NO state mutation (live-exec guard).
		if decision.Kind == hedgeOpen {
			if freshOpen {
				unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, snap.PrimaryQty, mu, notifier, logger, "hedge_open_failed")
			} else {
				hedgeAlert(notifier, logger, fmt.Sprintf("[%s] hedge %s open FAILED on a reconcile-drift cycle — retrying next cycle (primary stays hedged-pending)", sc.ID, hCoin))
			}
		} else if decision.Kind == hedgeReduce || decision.Kind == hedgeCloseFull {
			// Primary already changed; the hedge close/reduce failed. State is
			// untouched (no mutation), so the next cycle's sync retries.
			hedgeAlert(notifier, logger, fmt.Sprintf("[%s] hedge %s %s FAILED — hedge leg still open; sync retries next cycle", sc.ID, hCoin, decision.Reason))
		} else {
			hedgeAlert(notifier, logger, fmt.Sprintf("[%s] hedge %s %s FAILED — retrying next cycle", sc.ID, hCoin, decision.Reason))
		}
		return
	}

	mu.Lock()
	applyHedgeFillLocked(sc, stratState, primarySym, hCoin, decision, execFill.AvgPx, execFill.Fee, execFill.OID, false, logger)
	mu.Unlock()
}

// hedgeExecFill is the confirmed-fill data the live path hands to the apply.
type hedgeExecFill struct {
	AvgPx   float64
	TotalSz float64
	Fee     float64
	OID     int64
}

// runHedgeOrder places the live hedge order (unlocked) and returns the confirmed
// fill. open/add place a market order with the hedge leg's OWN margin_mode +
// leverage (passed only on a fresh open from flat, mirroring the primary path);
// reduce/closeFull place a reduce-only close. Returns (nil, false) on any
// failure so the caller performs no state mutation.
func runHedgeOrder(sc StrategyConfig, hCoin string, action hedgeAction, notifier *MultiNotifier, logger *StrategyLogger) (*hedgeExecFill, bool) {
	switch action.Kind {
	case hedgeOpen, hedgeAdd:
		side := hedgeExecuteSide(action.Side)
		marginMode := ""
		leverage := 0.0
		if action.Kind == hedgeOpen {
			// Fresh hedge open from flat: set the leg's own margin + leverage.
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeLeverage(sc)
		}
		execResult, stderr, err := RunHyperliquidExecute(sc.Script, hCoin, side, action.RequestedQty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
		if stderr != "" && logger != nil {
			logger.Info("hedge execute stderr: %s", stderr)
		}
		if err != nil {
			notifyLiveExecFailure(notifier, sc, "hedge_open", hCoin, err.Error())
			return nil, false
		}
		if execResult == nil || execResult.Error != "" {
			msg := "nil result"
			if execResult != nil {
				msg = execResult.Error
			}
			notifyLiveExecFailure(notifier, sc, "hedge_open", hCoin, msg)
			return nil, false
		}
		clearLiveExecThrottle(sc, "hedge_open", hCoin)
		if execResult.Execution == nil || execResult.Execution.Fill == nil || execResult.Execution.Fill.TotalSz <= 0 {
			return nil, false
		}
		f := execResult.Execution.Fill
		return &hedgeExecFill{AvgPx: f.AvgPx, TotalSz: f.TotalSz, Fee: f.Fee, OID: f.OID}, true
	case hedgeReduce, hedgeCloseFull:
		var partialSz *float64
		if action.Kind == hedgeReduce {
			sz := action.RequestedQty
			partialSz = &sz
		}
		// Full close: sole-owner hedge coin (collision-validated) → market_close
		// with sz=nil is safe. Reduce: sized reduce-only. Hedge carries no SL/TP
		// OIDs to cancel.
		closeResult, stderr, err := RunHyperliquidClose(sc.Script, hCoin, partialSz, nil)
		if stderr != "" && logger != nil {
			logger.Info("hedge close stderr: %s", stderr)
		}
		if err != nil {
			notifyLiveExecFailure(notifier, sc, "hedge_close", hCoin, err.Error())
			return nil, false
		}
		if closeResult == nil || closeResult.Close == nil {
			return nil, false
		}
		if closeResult.Close.AlreadyFlat {
			// Nothing to close on-chain; treat as a benign no-op success so the
			// apply flattens/decrements virtual state to match.
			clearLiveExecThrottle(sc, "hedge_close", hCoin)
			sz := action.RequestedQty
			return &hedgeExecFill{AvgPx: 0, TotalSz: sz, Fee: 0, OID: 0}, true
		}
		if closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
			return nil, false
		}
		clearLiveExecThrottle(sc, "hedge_close", hCoin)
		f := closeResult.Close.Fill
		return &hedgeExecFill{AvgPx: f.AvgPx, TotalSz: f.TotalSz, Fee: f.Fee, OID: f.OID}, true
	}
	return nil, false
}

// applyHedgeFillLocked applies a confirmed hedge fill under mu.Lock. It
// creates/blends/reduces/deletes the hedge Position and books a trade_type
// "hedge" leg (which routes PnL/fees into the owning strategy's ledger via
// tradeLedgerDeltaSQL while staying out of #T/W-L). paper=true books at the mark
// with a modeled fee.
func applyHedgeFillLocked(sc StrategyConfig, s *StrategyState, primarySym, hCoin string, action hedgeAction, fillPx, fillFee float64, fillOID int64, paper bool, logger *StrategyLogger) {
	switch action.Kind {
	case hedgeOpen:
		applyHedgeOpenLocked(sc, s, primarySym, hCoin, action, fillPx, action.RequestedQty, fillFee, fillOID, paper, logger)
	case hedgeAdd:
		applyHedgeAddLocked(sc, s, primarySym, hCoin, action, fillPx, action.RequestedQty, fillFee, fillOID, paper, logger)
	case hedgeReduce:
		applyHedgeReduceLocked(s, hCoin, action, fillPx, action.RequestedQty, fillFee, fillOID, paper, logger)
	case hedgeCloseFull:
		applyHedgeCloseLocked(s, hCoin, action, fillPx, fillFee, fillOID, paper, logger)
	}
}

func hedgeModeledFee(s *StrategyState, notional float64) float64 {
	return CalculatePlatformSpotFee(s.Platform, notional)
}

// hedgeCloseFillPx resolves the close price for a hedge reduce/close leg: the
// on-chain fill price when present, else the current hedge mark (paper) — never 0
// (which would make the booking helpers no-op).
func hedgeCloseFillPx(fillPx, fallbackPx float64) float64 {
	if fillPx > 0 {
		return fillPx
	}
	return fallbackPx
}

func applyHedgeOpenLocked(sc StrategyConfig, s *StrategyState, primarySym, hCoin string, action hedgeAction, fillPx, requestedQty, fillFee float64, fillOID int64, paper bool, logger *StrategyLogger) {
	qty := requestedQty
	px := fillPx
	if paper {
		px = fillPx // hedge mark
	}
	if px <= 0 || qty <= 0 {
		return
	}
	now := time.Now().UTC()
	fee := fillFee
	feeSource := FeeSourceUserFills
	if paper {
		fee = hedgeModeledFee(s, qty*px)
		feeSource = FeeSourceModeled
	}
	basis := hedgeAppliedBasis(0, action.PrimaryQtyTarget, requestedQty, qty)
	pos := &Position{
		Symbol:               hCoin,
		Quantity:             qty,
		InitialQuantity:      qty,
		AvgCost:              px,
		Side:                 action.Side,
		Multiplier:           1, // perps PnL branch
		Leverage:             hedgeLeverage(sc),
		OwnerStrategyID:      s.ID,
		OpenedAt:             now,
		HedgeFor:             primarySym,
		HedgePrimaryQtyBasis: basis,
	}
	pos.TradePositionID = newTradePositionID(s.ID, hCoin, now)
	s.Positions[hCoin] = pos

	var oidStr string
	if fillOID > 0 {
		oidStr = strconv.FormatInt(fillOID, 10)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hCoin,
		PositionID:      pos.TradePositionID,
		Side:            openTradeSide(action.Side),
		Quantity:        qty,
		Price:           px,
		Value:           qty * px,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) open %s %s @ $%.4f", primarySym, action.Side, hCoin, px),
		ExchangeOrderID: oidStr,
		ExchangeFee:     fee,
		FeeSource:       feeSource,
		PnLGross:        true,
		Regime:          s.Regime,
	}
	RecordTrade(s, trade)
	s.Cash -= fee // perps open: only the fee moves cash; notional stays virtual
	if logger != nil {
		logger.Warn("hedge(%s) OPENED %s %s %.6f @ $%.4f (basis primary=%.6f)", primarySym, action.Side, hCoin, qty, px, basis)
	}
}

func applyHedgeAddLocked(sc StrategyConfig, s *StrategyState, primarySym, hCoin string, action hedgeAction, fillPx, requestedQty, fillFee float64, fillOID int64, paper bool, logger *StrategyLogger) {
	pos, ok := s.Positions[hCoin]
	if !ok || pos == nil || pos.HedgeFor == "" {
		// Hedge leg vanished between snapshot and apply — treat the add as an
		// open so we never drop a confirmed fill on the floor.
		applyHedgeOpenLocked(sc, s, primarySym, hCoin, hedgeAction{Kind: hedgeOpen, Side: action.Side, RequestedQty: requestedQty, PrimaryQtyTarget: action.PrimaryQtyTarget, BasisBefore: 0}, fillPx, requestedQty, fillFee, fillOID, paper, logger)
		return
	}
	qty := requestedQty
	px := fillPx
	if px <= 0 || qty <= 0 {
		return
	}
	now := time.Now().UTC()
	fee := fillFee
	feeSource := FeeSourceUserFills
	if paper {
		fee = hedgeModeledFee(s, qty*px)
		feeSource = FeeSourceModeled
	}
	// Blend price + size (plain qty/AvgCost blend; a hedge has no frozen geometry).
	newQty := pos.Quantity + qty
	if newQty > 0 {
		pos.AvgCost = (pos.AvgCost*pos.Quantity + px*qty) / newQty
	}
	pos.Quantity = newQty
	pos.HedgePrimaryQtyBasis = hedgeAppliedBasis(action.BasisBefore, action.PrimaryQtyTarget, requestedQty, qty)

	var oidStr string
	if fillOID > 0 {
		oidStr = strconv.FormatInt(fillOID, 10)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hCoin,
		PositionID:      pos.TradePositionID,
		Side:            openTradeSide(pos.Side),
		Quantity:        qty,
		Price:           px,
		Value:           qty * px,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) add %s %.6f @ $%.4f", primarySym, hCoin, qty, px),
		ExchangeOrderID: oidStr,
		ExchangeFee:     fee,
		FeeSource:       feeSource,
		PnLGross:        true,
		Regime:          s.Regime,
	}
	RecordTrade(s, trade)
	s.Cash -= fee
	if logger != nil {
		logger.Warn("hedge(%s) ADDED %s %.6f @ $%.4f (total %.6f, basis primary=%.6f)", primarySym, hCoin, qty, px, pos.Quantity, pos.HedgePrimaryQtyBasis)
	}
}

func applyHedgeReduceLocked(s *StrategyState, hCoin string, action hedgeAction, fillPx, requestedQty, fillFee float64, fillOID int64, paper bool, logger *StrategyLogger) {
	pos, ok := s.Positions[hCoin]
	if !ok || pos == nil || pos.HedgeFor == "" {
		return
	}
	px := hedgeCloseFillPx(fillPx, pos.AvgCost)
	if px <= 0 {
		return
	}
	var oidStr string
	if fillOID > 0 {
		oidStr = strconv.FormatInt(fillOID, 10)
	}
	useFillFee := !paper
	// bookPerpsPartialCloseWithFillFee routes trade_type="hedge" +
	// RecordHedgeTradeResult (pos.HedgeFor != "") for free.
	if bookPerpsPartialCloseWithFillFee(s, hCoin, requestedQty, px, fillFee, useFillFee, oidStr, "hedge_reduce", fmt.Sprintf("hedge(%s) reduce", pos.HedgeFor), fmt.Sprintf("hedge(%s) reduce", pos.HedgeFor), logger) {
		if p := s.Positions[hCoin]; p != nil {
			p.HedgePrimaryQtyBasis = action.PrimaryQtyTarget
		}
	}
}

func applyHedgeCloseLocked(s *StrategyState, hCoin string, action hedgeAction, fillPx, fillFee float64, fillOID int64, paper bool, logger *StrategyLogger) {
	pos, ok := s.Positions[hCoin]
	if !ok || pos == nil || pos.HedgeFor == "" {
		return
	}
	px := hedgeCloseFillPx(fillPx, pos.AvgCost)
	if px <= 0 {
		return
	}
	var oidStr string
	if fillOID > 0 {
		oidStr = strconv.FormatInt(fillOID, 10)
	}
	useFillFee := !paper
	reason := "hedge_close"
	if action.Reason == "wrong_side" {
		reason = "hedge_close_wrong_side"
	}
	bookPerpsCloseWithFillFee(s, hCoin, px, fillFee, useFillFee, oidStr, reason, fmt.Sprintf("hedge(%s) close", pos.HedgeFor), fmt.Sprintf("hedge(%s) close", pos.HedgeFor), logger)
}

// unwindPrimaryAfterHedgeOpenFailure is the fail-closed disposition: a primary
// fill confirmed on a fresh-open cycle but the hedge open failed, so the primary
// must not run unhedged. It submits a sized reduce-only close of the full
// primary virtual qty (cancelling its just-armed SL/TP OIDs) and books it, with
// a CRITICAL owner DM. If the unwind itself fails, state is left unchanged and
// the next cycle's state-derived sync retries (documented degraded loop) — no
// new latch state, restart-safe.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, primarySym string, primaryQty float64, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, reason string) {
	if s == nil {
		return
	}
	live := hyperliquidIsLive(sc.Args)

	// Snapshot the cancel-OIDs + current qty under RLock.
	mu.RLock()
	pos := s.Positions[primarySym]
	var cancelOIDs []int64
	var qty float64
	if pos != nil {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
		qty = pos.Quantity
	}
	mu.RUnlock()
	if pos == nil || qty <= 0 {
		return
	}

	crit := fmt.Sprintf("**HEDGE OPEN FAILED — UNWINDING PRIMARY** [%s] %s: hedge leg could not be opened (%s); closing the primary %s %.6f reduce-only so it never runs UNHEDGED (#1159)", sc.ID, sc.ID, reason, primarySym, qty)

	if !live {
		// Paper: book the primary close at its avg cost (no exchange fill path).
		mu.Lock()
		bookPerpsCloseWithFillFee(s, primarySym, pos.AvgCost, 0, false, "", "hedge_open_failed_unwind", "hedge open failed — primary unwound", "hedge open failed — primary unwound", logger)
		mu.Unlock()
		hedgeAlert(notifier, logger, crit)
		return
	}

	// Live: sized reduce-only close, cancelling the primary's protection OIDs.
	closeResult, stderr, err := RunHyperliquidClose(sc.Script, primarySym, &qty, cancelOIDs)
	if stderr != "" && logger != nil {
		logger.Info("hedge-unwind close stderr: %s", stderr)
	}
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil || closeResult.Close.Fill.TotalSz <= 0 {
		// Unwind failed — state unchanged; the next cycle's sync retries the
		// hedge open first (now hedged) or unwinds again. CRITICAL DM.
		hedgeAlert(notifier, logger, crit+" — UNWIND ORDER FAILED; primary is UNHEDGED and still OPEN, retrying next cycle")
		return
	}
	f := closeResult.Close.Fill
	mu.Lock()
	bookPerpsCloseWithFillFee(s, primarySym, f.AvgPx, f.Fee, true, hlOIDString(f.OID), "hedge_open_failed_unwind", "hedge open failed — primary unwound", "hedge open failed — primary unwound", logger)
	mu.Unlock()
	hedgeAlert(notifier, logger, crit)
}

// hlOIDString formats an OID for a Trade.ExchangeOrderID ("" for 0).
func hlOIDString(oid int64) string {
	if oid <= 0 {
		return ""
	}
	return strconv.FormatInt(oid, 10)
}

// hedgeAlert logs a CRITICAL line and DMs the owner + broadcasts to channels
// (all sends outside mu, #880 convention).
func hedgeAlert(notifier *MultiNotifier, logger *StrategyLogger, msg string) {
	if logger != nil {
		logger.Error("CRITICAL: %s", msg)
	} else {
		fmt.Printf("[CRITICAL] %s\n", msg)
	}
	if notifier != nil && notifier.HasBackends() {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}
