package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file implements the #1159 phase-1 correlated hedge leg engine: a
// scheduler-managed second HL perps position that mirrors a primary
// strategy's lifecycle (open/scale-in/close) on an inverse side, on a
// different coin. Pure sizing/gate helpers and the HedgeConfig schema live in
// config.go; this file holds the I/O wrappers (subprocess order placement),
// the state-mutating apply functions (must be called under Lock), the
// higher-level open/scale-in/close mirror orchestration (must be called from
// Phase 3 — no lock held, since they issue live orders), the per-cycle
// coherence safety net, and cross-strategy config validation.

// ============================================================================
// Pre-open gate
// ============================================================================

// hedgePreOpenGate decides whether it's safe to open a NEW hedge leg
// alongside a fresh primary open, evaluated BEFORE the primary order is
// spawned — mirrors the "skip-reason before spawning" discipline the rest of
// the perps dispatch uses (PerpsOrderSkipReason) so a doomed hedge never lets
// an unhedgeable primary fill land first. Refuses when the hedge coin/mark
// can't be resolved, when a virtual hedge position already exists (should
// only happen if coherence hasn't caught up — defensive), or when the hedge
// coin carries a foreign/ambiguous on-chain position with no matching
// virtual row (HL aggregates per coin — there is no safe way to
// disambiguate "ours" from "foreign" on a shared coin).
func hedgePreOpenGate(sc StrategyConfig, hedgeCoin string, hedgeMark float64, hlPositions []HLPosition, existingHedgePos *Position) (bool, string) {
	if hedgeCoin == "" {
		return false, "hedge coin unresolved"
	}
	if hedgeMark <= 0 {
		return false, fmt.Sprintf("no mark price available for hedge coin %s", hedgeCoin)
	}
	if existingHedgePos != nil && existingHedgePos.Quantity > 0 {
		return false, fmt.Sprintf("a hedge leg already exists on %s", hedgeCoin)
	}
	for _, p := range hlPositions {
		if p.Coin == hedgeCoin && p.Size != 0 {
			return false, fmt.Sprintf("hedge coin %s has a foreign/ambiguous on-chain position (size=%.6f) with no matching virtual hedge — refusing to open alongside it", hedgeCoin, p.Size)
		}
	}
	return true, ""
}

// ============================================================================
// Subprocess I/O wrappers (Phase 3 — no lock held)
// ============================================================================

// runHedgeOpenOrder submits a market order for the hedge leg via the same
// live-order primitive normal perps opens use (RunHyperliquidExecute) — the
// Python adapter is symbol-agnostic, so no dedicated hedge script path is
// needed. No SL/TP is requested (phase 1: the hedge carries no independent
// protection — stopLossPct=0). Margin/leverage args are sent only when
// hedgeFlat is true (mirrors runHyperliquidExecuteOrder's open-from-flat-only
// guard, main.go ~3762 — HL rejects update_leverage on an open position, and
// an add must inherit whatever the first hedge open already set on-chain).
func runHedgeOpenOrder(sc StrategyConfig, hedgeCoin, side string, qty float64, hedgeFlat bool, snapshot hlExecuteSnapshot, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	marginMode := ""
	leverage := 0.0
	if hedgeFlat {
		marginMode = sc.Hedge.MarginMode
		leverage = sc.Hedge.Leverage
		if leverage <= 0 {
			leverage = 1
		}
	}
	execResult, stderr, err := RunHyperliquidExecute(sc.Script, hedgeCoin, side, qty, 0, 0, 0, marginMode, leverage, false, snapshot)
	if stderr != "" && logger != nil {
		logger.Info("hedge execute stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Hedge open failed for %s: %v", hedgeCoin, err)
		}
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeCoin, err.Error())
		return execResult, false
	}
	if execResult.Error != "" {
		if logger != nil {
			logger.Error("Hedge open returned error for %s: %s", hedgeCoin, execResult.Error)
		}
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeCoin, execResult.Error)
		return execResult, false
	}
	return execResult, true
}

// runReduceOnlyClose wraps RunHyperliquidClose for a reduce-only close/reduce
// of a single coin. sz=nil closes the ENTIRE on-chain position for that coin
// (market_close, dust-free) — safe to use for a hedge coin because
// hyperliquidHedgeConfigErrors guarantees hedge coins are always sole-owned
// (no peer/collision), and safe for a primary coin unwind because it's used
// only when this strategy is the confirmed owner of the fill that just
// landed. sz!=nil submits a partial reduce for that quantity. Shared by the
// hedge open/scale-in/close mirrors, syncHedgeCoherence, and
// unwindPrimaryAfterHedgeOpenFailure's primary-side unwind.
func runReduceOnlyClose(script, symbol string, sz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, bool) {
	result, _, err := RunHyperliquidClose(script, symbol, sz, cancelOIDs)
	if err != nil || result == nil || result.Error != "" {
		return result, false
	}
	return result, true
}

// ============================================================================
// State-mutating apply functions (must be called under Lock)
// ============================================================================

// applyHedgeOpen books a confirmed hedge fill as a new hedge Position + Trade,
// and stamps HedgeSymbol on the primary so restart/reconcile can recover
// ownership from persisted metadata alone (never coin->symbol inference,
// constraint 5). Must be called under Lock.
func applyHedgeOpen(s *StrategyState, sc StrategyConfig, primarySymbol, hedgeCoin, hedgeSide string, fill *HyperliquidFill) *Trade {
	if fill == nil || fill.TotalSz <= 0 || fill.AvgPx <= 0 {
		return nil
	}
	now := time.Now().UTC()
	s.Cash -= fill.Fee
	positionID := newTradePositionID(s.ID, hedgeCoin, now)
	s.Positions[hedgeCoin] = &Position{
		Symbol:          hedgeCoin,
		Quantity:        fill.TotalSz,
		InitialQuantity: fill.TotalSz,
		AvgCost:         fill.AvgPx,
		Side:            hedgeSide,
		Multiplier:      1,
		Leverage:        sc.Hedge.Leverage,
		OwnerStrategyID: s.ID,
		OpenedAt:        now,
		TradePositionID: positionID,
		IsHedge:         true,
		HedgeFor:        primarySymbol,
	}
	if primaryPos, ok := s.Positions[primarySymbol]; ok && primaryPos != nil {
		primaryPos.HedgeSymbol = hedgeCoin
		if primaryPos.Quantity > 0 {
			primaryPos.HedgeQtyRatio = fill.TotalSz / primaryPos.Quantity
		}
	}
	side := "buy"
	if hedgeSide == "short" {
		side = "sell"
	}
	var oid string
	if fill.OID != 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeCoin,
		PositionID:      positionID,
		Side:            side,
		Quantity:        fill.TotalSz,
		Price:           fill.AvgPx,
		Value:           fill.TotalSz * fill.AvgPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("HEDGE open %s %s %.6f @ $%.2f (for %s, ratio %.2f, fee $%.2f)", hedgeCoin, hedgeSide, fill.TotalSz, fill.AvgPx, primarySymbol, sc.Hedge.Ratio, fill.Fee),
		ExchangeOrderID: oid,
		ExchangeFee:     fill.Fee,
		FeeSource:       FeeSourceUserFills,
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	return &trade
}

// applyHedgeScaleIn blends an additional hedge fill into the existing hedge
// position via the same blend primitive scale-in adds use (applyScaleIn,
// scale_in.go), so hedge/primary InitialQuantity growth stays in lockstep
// when both legs add successfully. Also refreshes the primary's
// HedgeQtyRatio to the post-add quantities: an add's hedge sizing uses the
// price AT ADD TIME (hedgeOpenQty), which generally differs from the
// open-time price, so the correct qty ratio shifts slightly on every add —
// re-anchoring here (rather than recomputing from live marks) keeps
// syncHedgeCoherence's price-independent comparison exact instead of
// misreading a legitimate, already-mirrored add as desync. Must be called
// under Lock.
func applyHedgeScaleIn(s *StrategyState, primarySymbol, hedgeCoin string, fill *HyperliquidFill) *Trade {
	pos, ok := s.Positions[hedgeCoin]
	if !ok || pos == nil || fill == nil || fill.TotalSz <= 0 || fill.AvgPx <= 0 {
		return nil
	}
	s.Cash -= fill.Fee
	applyScaleIn(pos, fill.TotalSz, fill.AvgPx)
	if primaryPos, ok := s.Positions[primarySymbol]; ok && primaryPos != nil && primaryPos.Quantity > 0 {
		primaryPos.HedgeQtyRatio = pos.Quantity / primaryPos.Quantity
	}
	now := time.Now().UTC()
	side := "buy"
	if pos.Side == "short" {
		side = "sell"
	}
	var oid string
	if fill.OID != 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeCoin,
		PositionID:      ensurePositionTradeID(s.ID, hedgeCoin, pos),
		Side:            side,
		Quantity:        fill.TotalSz,
		Price:           fill.AvgPx,
		Value:           fill.TotalSz * fill.AvgPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("HEDGE add %s %.6f @ $%.2f (new qty %.6f, fee $%.2f)", hedgeCoin, fill.TotalSz, fill.AvgPx, pos.Quantity, fill.Fee),
		ExchangeOrderID: oid,
		ExchangeFee:     fill.Fee,
		FeeSource:       FeeSourceUserFills,
		PnLGross:        true,
	}
	trade.Regime = pos.Regime
	RecordTrade(s, trade)
	return &trade
}

// hedgeFullCloseSymbol fully closes `symbol` reduce-only (market_close,
// sz=nil) and books the real fill. Manages its own lock phases — call from
// Phase 3. Used for hedge full-closes (signal close, coherence) AND, via
// unwindPrimaryAfterHedgeOpenFailure, the primary-side unwind on an open
// failure — reason/detail strings passed by the caller distinguish them in
// the trade ledger.
func hedgeFullCloseSymbol(sc StrategyConfig, stratState *StrategyState, symbol, reason string, mu *sync.RWMutex, logger *StrategyLogger) bool {
	closeResult, ok := runReduceOnlyClose(sc.Script, symbol, nil, nil)
	if !ok || closeResult == nil || closeResult.Close == nil {
		errMsg := "unknown error"
		if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		if logger != nil {
			logger.Error("hedge: reduce-only close of %s failed (%s) — retrying next cycle", symbol, errMsg)
		}
		return false
	}
	if closeResult.Close.AlreadyFlat || closeResult.Close.Fill == nil {
		// Already flat on-chain (race with an earlier fill this same
		// window) — clear any stale virtual row rather than leaving it
		// dangling with no matching exchange position.
		mu.Lock()
		delete(stratState.Positions, symbol)
		mu.Unlock()
		return true
	}
	fill := closeResult.Close.Fill
	mu.Lock()
	ok2 := bookPerpsCloseWithFillFee(stratState, symbol, fill.AvgPx, fill.Fee, true, fmt.Sprintf("%d", fill.OID), reason, "HEDGE close", "HEDGE close", logger)
	mu.Unlock()
	return ok2
}

// hedgeReduceSymbolBy reduces `symbol` reduce-only by `qty` (clamped to a
// full close when qty >= currentQty) and books the real fill. No-ops when
// qty <= 0. Manages its own lock phases — call from Phase 3.
func hedgeReduceSymbolBy(sc StrategyConfig, stratState *StrategyState, symbol string, qty, currentQty float64, reason string, mu *sync.RWMutex, logger *StrategyLogger) {
	if qty <= 0 {
		return
	}
	if qty >= currentQty {
		hedgeFullCloseSymbol(sc, stratState, symbol, reason, mu, logger)
		return
	}
	sz := qty
	closeResult, ok := runReduceOnlyClose(sc.Script, symbol, &sz, nil)
	if !ok || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "unknown error"
		if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		if logger != nil {
			logger.Error("hedge: reduce-only partial of %s (qty=%.6f) failed (%s) — retrying next cycle", symbol, qty, errMsg)
		}
		return
	}
	fill := closeResult.Close.Fill
	mu.Lock()
	bookPerpsPartialCloseWithFillFee(stratState, symbol, fill.TotalSz, fill.AvgPx, fill.Fee, true, fmt.Sprintf("%d", fill.OID), reason, "HEDGE reduce", "HEDGE reduce", logger)
	mu.Unlock()
}

// ============================================================================
// Notification helper
// ============================================================================

// notifyHedgeCritical sends a CRITICAL owner-DM + channel alert for a hedge
// safety event (open failure, coherence under-hedge correction, config/state
// mismatch freeze). Every fail-closed hedge path routes through this so an
// operator is never left inferring from logs alone that a position is
// running unhedged.
func notifyHedgeCritical(notifier *MultiNotifier, logger *StrategyLogger, sc StrategyConfig, msg string) {
	full := fmt.Sprintf("CRITICAL [hedge] [%s]: %s", sc.ID, msg)
	if logger != nil {
		logger.Error("%s", full)
	}
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(full)
		notifier.SendOwnerDM(full)
	}
}

// ============================================================================
// Open mirror + fail-closed unwind (constraint 4)
// ============================================================================

// runHedgeOpenMirror attempts to open the hedge leg for a CONFIRMED fresh
// primary fill. Must be called from Phase 3 (no lock — this issues a live
// order). Returns ok=true with the confirmed hedge fill when the hedge opened
// successfully; the caller books it via applyHedgeOpen under the SAME lock
// scope as the primary open, so both legs land atomically from state's
// perspective. ok=false means the hedge could not be opened at all (gate
// refusal, sub-minimum notional, or exec failure) — the caller MUST unwind
// the primary (unwindPrimaryAfterHedgeOpenFailure), never book it as a naked
// open.
func runHedgeOpenMirror(sc StrategyConfig, primarySide string, fill *HyperliquidFill, hlPositions []HLPosition, existingHedgePos *Position, hedgeMark float64, notifier *MultiNotifier, logger *StrategyLogger) (hedgeCoin, hedgeSide string, hedgeFill *HyperliquidFill, ok bool, reason string) {
	hedgeCoin = hedgeCoinForStrategy(sc)
	hedgeSide = hedgeSideForPrimary(primarySide)
	if hedgeSide == "" {
		return hedgeCoin, hedgeSide, nil, false, fmt.Sprintf("cannot derive hedge side from primary side %q", primarySide)
	}
	if gateOK, gateReason := hedgePreOpenGate(sc, hedgeCoin, hedgeMark, hlPositions, existingHedgePos); !gateOK {
		return hedgeCoin, hedgeSide, nil, false, gateReason
	}
	if fill == nil || fill.TotalSz <= 0 || fill.AvgPx <= 0 {
		return hedgeCoin, hedgeSide, nil, false, "primary fill missing/zero — nothing to size the hedge from"
	}
	qty := hedgeOpenQty(fill.TotalSz, fill.AvgPx, sc.Hedge.Ratio, hedgeMark)
	if qty <= 0 || qty*hedgeMark < hedgeMinNotionalUSD {
		return hedgeCoin, hedgeSide, nil, false, fmt.Sprintf("hedge notional $%.2f below minimum $%.2f", qty*hedgeMark, hedgeMinNotionalUSD)
	}
	side := "buy"
	if hedgeSide == "short" {
		side = "sell"
	}
	snapshot := hlExecuteSnapshotForCoin(hlPositions, hedgeCoin)
	execResult, execOK := runHedgeOpenOrder(sc, hedgeCoin, side, qty, true, snapshot, notifier, logger)
	if !execOK || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
		return hedgeCoin, hedgeSide, nil, false, "hedge order execution failed"
	}
	return hedgeCoin, hedgeSide, execResult.Execution.Fill, true, ""
}

// unwindPrimaryAfterHedgeOpenFailure implements constraint 4 (fail-closed):
// when a primary fill confirms but the hedge open cannot be completed, the
// primary must never be left open unhedged. Books the primary open (so trade
// history matches the real on-chain fill), then submits a reduce-only close
// of the primary (cancelling any SL OID the open just placed) and books the
// close — both legs land on the trades ledger so history matches the chain.
// Alerts CRITICAL throughout. Manages its own Lock/Unlock phases — call from
// Phase 3 (no lock held). Returns (trades booked, detail string) for the
// caller's cycle-summary counters.
//
// hedgeCoin is the intended hedge coin (the one that failed to open) — if the
// reduce-only close itself ALSO fails, it gets stamped onto the primary's
// HedgeSymbol so syncHedgeCoherence's "primary exists, hedge gone" branch
// (which is gated on HedgeSymbol != "") picks the primary up and keeps
// retrying the reduce-only close every cycle until it succeeds. Without this,
// a primary left open after a failed close silently degrades to an ordinary,
// permanently-unhedged position after a single alert — coherence would never
// touch it, because as far as it could tell no hedge was ever intended.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, stratState *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, cfg *Config, hedgeCoin, hedgeReason string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	mu.Lock()
	trades, detail, openTrade, ratchetAlert := executeHyperliquidResultDeferredOpen(sc, stratState, result, execResult, signalStr, price, cfg.Regime, cfg, logger)
	var pos *Position
	if p, ok := stratState.Positions[result.Symbol]; ok {
		pos = p
	}
	if openTrade != nil {
		recordPositionOpen(stratState, sc, openTrade, pos)
	}
	var slOID int64
	if pos != nil {
		slOID = pos.StopLossOID
	}
	mu.Unlock()
	notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)

	notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
		"hedge open failed for %s: %s — unwinding primary (reduce-only close, never running unhedged)", result.Symbol, hedgeReason))

	var cancelOIDs []int64
	if slOID > 0 {
		cancelOIDs = append(cancelOIDs, slOID)
	}
	closeResult, ok := runReduceOnlyClose(sc.Script, result.Symbol, nil, cancelOIDs)
	if !ok || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "unknown error"
		if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		mu.Lock()
		if p, ok3 := stratState.Positions[result.Symbol]; ok3 && p != nil {
			p.HedgeSymbol = hedgeCoin
		}
		mu.Unlock()
		notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
			"hedge-unwind close of primary %s FAILED (%s) — position left open UNHEDGED, retrying reduce-only every cycle via coherence sync", result.Symbol, errMsg))
		return trades, detail
	}
	fill := closeResult.Close.Fill
	mu.Lock()
	closeOK := bookPerpsCloseWithFillFee(stratState, result.Symbol, fill.AvgPx, fill.Fee, true, fmt.Sprintf("%d", fill.OID), "hedge_unwind_primary", "HEDGE UNWIND primary close", "HEDGE UNWIND primary close", logger)
	mu.Unlock()
	if closeOK {
		trades++
		detail = fmt.Sprintf("[%s] HEDGE UNWIND closed primary %s @ $%.2f", sc.ID, result.Symbol, fill.AvgPx)
	}
	return trades, detail
}

// ============================================================================
// Scale-in add mirror + add-specific unwind
// ============================================================================

// runHedgeScaleInMirror mirrors a confirmed primary scale-in ADD onto the
// hedge leg. Must be called from Phase 3 (no lock). hedgeSide is the
// EXISTING hedge position's side (adds never change side — direction="both"
// is rejected at validation for hedge-enabled strategies, so the primary
// never flips while open).
func runHedgeScaleInMirror(sc StrategyConfig, hedgeCoin, hedgeSide string, addFillQty, addFillPx, hedgeMark float64, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidFill, bool, string) {
	if hedgeCoin == "" || hedgeSide == "" {
		return nil, false, "hedge coin/side unresolved"
	}
	if hedgeMark <= 0 {
		return nil, false, fmt.Sprintf("no mark price available for hedge coin %s", hedgeCoin)
	}
	qty := hedgeOpenQty(addFillQty, addFillPx, sc.Hedge.Ratio, hedgeMark)
	if qty <= 0 || qty*hedgeMark < hedgeMinNotionalUSD {
		return nil, false, fmt.Sprintf("hedge add notional $%.2f below minimum $%.2f", qty*hedgeMark, hedgeMinNotionalUSD)
	}
	side := "buy"
	if hedgeSide == "short" {
		side = "sell"
	}
	snapshot := hlExecuteSnapshotForCoin(hlPositions, hedgeCoin)
	execResult, ok := runHedgeOpenOrder(sc, hedgeCoin, side, qty, false, snapshot, notifier, logger)
	if !ok || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
		return nil, false, "hedge add order execution failed"
	}
	return execResult.Execution.Fill, true, ""
}

// unwindPrimaryAddAfterHedgeAddFailure handles a hedge ADD failure (distinct
// from a fresh-open failure): rather than unwinding the whole primary
// position, it reduces the primary back off by just the add quantity that
// couldn't be hedged, restoring the pre-add primary/hedge relationship.
// Manages its own lock phases — call from Phase 3. Returns (trades booked,
// detail string).
func unwindPrimaryAddAfterHedgeAddFailure(sc StrategyConfig, stratState *StrategyState, primarySymbol string, addFillQty float64, hedgeReason string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
		"hedge add failed for %s: %s — reducing primary add back off (restoring pre-add coverage)", primarySymbol, hedgeReason))
	sz := addFillQty
	closeResult, ok := runReduceOnlyClose(sc.Script, primarySymbol, &sz, nil)
	if !ok || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "unknown error"
		if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
			"hedge-add unwind reduce of %s FAILED (%s) — primary left over-added versus hedge; coherence sync will keep retrying reduce-only", primarySymbol, errMsg))
		return 0, ""
	}
	fill := closeResult.Close.Fill
	mu.Lock()
	ok2 := bookPerpsPartialCloseWithFillFee(stratState, primarySymbol, fill.TotalSz, fill.AvgPx, fill.Fee, true, fmt.Sprintf("%d", fill.OID), "hedge_unwind_add", "HEDGE UNWIND primary add", "HEDGE UNWIND primary add", logger)
	mu.Unlock()
	if ok2 {
		return 1, fmt.Sprintf("[%s] HEDGE UNWIND reduced primary add %s by %.6f", sc.ID, primarySymbol, fill.TotalSz)
	}
	return 0, ""
}

// ============================================================================
// Signal-close mirror
// ============================================================================

// runHedgeCloseMirror mirrors a confirmed signal-based primary close/partial
// close onto the hedge leg: a full primary close (closeFraction>=1.0) fully
// closes the hedge; a fractional close reduces the hedge by the same
// fraction. A hedge-side failure here is NOT fail-closed the way an open
// failure is — the residual hedge is risk-REDUCING relative to the (now
// smaller or flat) primary, so it's left for syncHedgeCoherence to retry
// rather than blocking or reversing the primary's close. Must be called from
// Phase 3 (no lock).
func runHedgeCloseMirror(sc StrategyConfig, stratState *StrategyState, primarySymbol string, closeFraction float64, mu *sync.RWMutex, logger *StrategyLogger) {
	if !strategyHedgeEnabled(sc) || closeFraction <= 0 {
		return
	}
	mu.RLock()
	var hedgeCoin string
	var hedgeQty float64
	if p, ok := stratState.Positions[primarySymbol]; ok && p != nil {
		hedgeCoin = p.HedgeSymbol
	}
	if hedgeCoin != "" {
		if h, ok := stratState.Positions[hedgeCoin]; ok && h != nil && h.Quantity > 0 {
			hedgeQty = h.Quantity
		} else {
			hedgeCoin = ""
		}
	}
	mu.RUnlock()
	if hedgeCoin == "" || hedgeQty <= 0 {
		return
	}
	if closeFraction >= 1.0 {
		hedgeFullCloseSymbol(sc, stratState, hedgeCoin, "hedge_close", mu, logger)
		return
	}
	hedgeReduceSymbolBy(sc, stratState, hedgeCoin, hedgeQty*closeFraction, hedgeQty, "hedge_reduce", mu, logger)
}

// ============================================================================
// Per-cycle coherence safety net
// ============================================================================

// syncHedgeCoherence is the per-cycle safety net for hedge-enabled strategies.
// The open/scale-in/close call sites above mirror the common case
// synchronously, but on-chain SL/TP/trailing fills on the primary (booked by
// the reconciler, not this dispatch), crashes mid-mirror, and external
// hedge-coin closes/liquidations can all desync primary and hedge without
// ever hitting one of those call sites. This pass runs every cycle
// (unconditionally, for every hedge-configured HL-live strategy — cheap when
// nothing is open) and converges REDUCE-ONLY, never adding exposure to
// either leg:
//
//   - hedge exists, primary gone           -> full-close hedge
//   - primary exists, hedge gone           -> full-close primary + CRITICAL
//     ("never run unhedged silently" made deterministic, not just logged)
//   - hedge qty > expected (over-hedged)   -> reduce hedge down to expected
//   - hedge qty < expected (under-hedged)  -> reduce PRIMARY down to match
//     the hedge's actual size + CRITICAL (never adds hedge exposure to
//     converge)
//   - persisted hedge coin != current configured coin, or a hedge row is
//     alive while hedging is now disabled -> freeze (no automatic action) +
//     CRITICAL; an edit+restart can bypass the SIGHUP state-compat guard, so
//     this is the backstop that catches it
//
// "expected" is derived from Position.HedgeQtyRatio (hedge qty per unit of
// primary qty, fixed at the last confirmed open/add event) times the
// CURRENT primary quantity — deliberately quantity-only, never live marks.
// Two different coins never move identically, so recomputing "expected" from
// live prices every cycle would misread ordinary price drift between the two
// coins as desync and reduce-only ratchet a winning primary toward zero on
// every up-move. See hedgeCoherenceDecision (config.go) for the comparison
// math and the InitialQuantity-coverage-fraction alternative it also rejects.
//
// Must be called from Phase 3 (no lock held) — it manages its own lock
// phases because reduce/close orders are subprocess calls.
func syncHedgeCoherence(sc StrategyConfig, stratState *StrategyState, primarySymbol string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	if !hyperliquidIsLive(sc.Args) {
		return
	}
	mu.RLock()
	var primary, hedge *Position
	if p, ok := stratState.Positions[primarySymbol]; ok && p != nil {
		cp := *p
		primary = &cp
	}
	hedgeCoin := ""
	if primary != nil && primary.HedgeSymbol != "" {
		hedgeCoin = primary.HedgeSymbol
	} else if strategyHedgeEnabled(sc) {
		hedgeCoin = hedgeCoinForStrategy(sc)
	}
	if hedgeCoin != "" {
		if h, ok := stratState.Positions[hedgeCoin]; ok && h != nil {
			ch := *h
			hedge = &ch
		}
	}
	mu.RUnlock()

	if primary == nil && hedge == nil {
		return
	}

	// Config/state mismatch: a persisted hedge coin that no longer matches
	// the CURRENT config (an edit+restart can bypass the SIGHUP
	// validateHotReloadStateCompatible guard, which only fires on a live
	// reload), or a hedge row alive while hedging is now disabled outright.
	// Freeze rather than guess which side is stale — resolve manually.
	if hedge != nil {
		configuredCoin := hedgeCoinForStrategy(sc)
		if !strategyHedgeEnabled(sc) || (configuredCoin != "" && configuredCoin != hedge.Symbol) {
			notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
				"hedge state/config mismatch for %s: persisted hedge coin %s, current config coin %q (hedge enabled=%v) — freezing automatic hedge management, resolve manually",
				primarySymbol, hedge.Symbol, configuredCoin, strategyHedgeEnabled(sc)))
			return
		}
	}

	switch {
	case hedge != nil && primary == nil:
		hedgeFullCloseSymbol(sc, stratState, hedge.Symbol, "hedge_coherence_primary_closed", mu, logger)

	case primary != nil && hedge == nil && strategyHedgeEnabled(sc) && primary.HedgeSymbol != "":
		notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
			"hedge for %s (%s) is gone but the primary is still open — closing primary reduce-only to avoid running unhedged", primarySymbol, primary.HedgeSymbol))
		hedgeFullCloseSymbol(sc, stratState, primarySymbol, "hedge_coherence_hedge_lost", mu, logger)

	case primary != nil && hedge != nil && strategyHedgeEnabled(sc):
		if sc.Hedge == nil {
			return
		}
		needsBootstrap, reduceSymbol, reduceQty, expected := hedgeCoherenceDecision(primary.Quantity, hedge.Quantity, primary.HedgeQtyRatio, primarySymbol, hedge.Symbol)
		if needsBootstrap {
			// No basis established yet (brand-new position this cycle, or a
			// position that predates HedgeQtyRatio) — record the CURRENT
			// actual quantities as the new basis rather than guessing a
			// target from an absent one. Never triggers a reduction on this
			// pass; comparisons start from the next cycle onward.
			mu.Lock()
			if p, ok := stratState.Positions[primarySymbol]; ok && p != nil && p.Quantity > 0 {
				p.HedgeQtyRatio = hedge.Quantity / p.Quantity
			}
			mu.Unlock()
			return
		}
		switch reduceSymbol {
		case hedge.Symbol:
			hedgeReduceSymbolBy(sc, stratState, hedge.Symbol, reduceQty, hedge.Quantity, "hedge_coherence_over_hedged", mu, logger)
		case primarySymbol:
			notifyHedgeCritical(notifier, logger, sc, fmt.Sprintf(
				"hedge under-covers primary for %s: hedge qty %.6f < expected %.6f (qty-ratio %.6f) — reducing primary to match (never adding exposure to converge)",
				primarySymbol, hedge.Quantity, expected, primary.HedgeQtyRatio))
			hedgeReduceSymbolBy(sc, stratState, primarySymbol, reduceQty, primary.Quantity, "hedge_coherence_under_hedged", mu, logger)
		}
	}
}

// ============================================================================
// Startup / reconcile lane
// ============================================================================

// reconcileHedgeLegsForStrategy detects and books EXTERNAL changes to a
// hedge-enabled strategy's hedge leg (operator manual close on the HL UI,
// liquidation) — the hedge-coin counterpart to
// reconcileHyperliquidPositionsForStrategy, simplified because a hedge leg
// carries no SL/TP/regime state to reconcile. Must be called under Lock
// (mirrors the primary reconciler's contract) — this is the mechanism that
// makes constraint 5 real: without it, an externally-closed hedge sits stale
// in virtual state forever and syncHedgeCoherence's "hedge gone" branch can
// never fire, leaving the operator believing a position is hedged when it
// isn't. Returns true if state changed.
//
// An on-chain position on the hedge coin with NO matching virtual hedge row
// is never adopted here — hyperliquidHedgeConfigErrors guarantees the coin is
// exclusively this strategy's hedge, so a foreign position on it can only mean
// a real-world ownership problem (stale config, manual intervention) that the
// caller's gap-tracking/alerting surfaces for the operator, never silently
// inferred here.
func reconcileHedgeLegsForStrategy(sc StrategyConfig, ss *StrategyState, positions []HLPosition, resolveFee hlReconcileFillResolver, logger *StrategyLogger) bool {
	if !strategyHedgeEnabled(sc) || ss == nil {
		return false
	}
	hedgeCoin := hedgeCoinForStrategy(sc)
	if hedgeCoin == "" {
		return false
	}
	pos := ss.Positions[hedgeCoin]
	if pos == nil || pos.Quantity <= 0 {
		return false
	}
	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == hedgeCoin {
			onChainPos = &positions[i]
			break
		}
	}
	primarySymbol := pos.HedgeFor

	if onChainPos == nil {
		// Fully closed externally.
		lookup, useFillFee := resolveFee(hedgeCoin, 0, pos.Quantity)
		closePx := lookup.Px
		if closePx <= 0 {
			closePx = pos.AvgCost // no fill data at all — avoid a zero-price booking
		}
		var oidStr string
		if useFillFee && lookup.OID != 0 {
			oidStr = fmt.Sprintf("%d", lookup.OID)
		}
		if logger != nil {
			logger.Warn("hl-sync: hedge %s externally closed (no matching on-chain position) — booking close", hedgeCoin)
		}
		ok := bookPerpsCloseWithFillFee(ss, hedgeCoin, closePx, lookup.Fee, useFillFee, oidStr, "hl_sync_external_hedge", "HEDGE external close", "HEDGE external close", logger)
		if ok {
			if primaryPos, exists := ss.Positions[primarySymbol]; exists && primaryPos != nil {
				primaryPos.HedgeSymbol = ""
			}
		}
		return ok
	}

	onChainAbs := math.Abs(onChainPos.Size)
	sameDirection := (onChainPos.Size > 0 && pos.Side == "long") || (onChainPos.Size < 0 && pos.Side == "short")
	if !sameDirection {
		// Direction flipped on-chain — should never happen for a coin
		// hyperliquidHedgeConfigErrors guarantees is sole-owned and that only
		// runHedgeOpenOrder/runReduceOnlyClose ever trade, but fail safe
		// rather than book an economically meaningless "close" against a
		// foreign-direction fill: clear the stale virtual row so
		// syncHedgeCoherence's "hedge gone" branch closes the primary next
		// cycle (never left silently believing it's still hedged).
		delete(ss.Positions, hedgeCoin)
		if primaryPos, exists := ss.Positions[primarySymbol]; exists && primaryPos != nil {
			primaryPos.HedgeSymbol = ""
		}
		if logger != nil {
			logger.Warn("hl-sync: hedge %s on-chain direction no longer matches virtual (side=%s, on-chain size=%.6f) — clearing stale virtual row", hedgeCoin, pos.Side, onChainPos.Size)
		}
		return true
	}
	if onChainAbs+1e-9 >= pos.Quantity {
		return false // no drift, or grew (our own open/scale-in mirrors already booked that)
	}
	closeQty := pos.Quantity - onChainAbs
	lookup, useFillFee := resolveFee(hedgeCoin, 0, closeQty)
	closePx := lookup.Px
	if closePx <= 0 {
		closePx = pos.AvgCost
	}
	var oidStr string
	if useFillFee && lookup.OID != 0 {
		oidStr = fmt.Sprintf("%d", lookup.OID)
	}
	if logger != nil {
		logger.Warn("hl-sync: hedge %s externally reduced by %.6f — booking partial close", hedgeCoin, closeQty)
	}
	return bookPerpsPartialCloseWithFillFee(ss, hedgeCoin, closeQty, closePx, lookup.Fee, useFillFee, oidStr, "hl_sync_external_hedge", "HEDGE external reduce", "HEDGE external reduce", logger)
}

// ============================================================================
// Config validation
// ============================================================================

// hyperliquidHedgeConfigErrors validates every hedge-enabled strategy's
// HedgeConfig, both in isolation and for cross-strategy collisions (phase-1
// constraint 2: the hedge coin must always be sole-owned by construction so
// every shared-coin mechanism — peer detection, margin compatibility, CB
// drain, kill-switch share, reconcile owner mapping — stays correct without
// needing to know hedge legs exist).
func hyperliquidHedgeConfigErrors(strategies []StrategyConfig) []string {
	var errs []string
	configuredCoins := make(map[string]bool) // every configured HL strategy's own coin (incl. manual)
	hedgeCoins := make(map[string][]string)  // hedge coin -> strategy IDs using it as their hedge
	for _, sc := range strategies {
		if c := hyperliquidConfiguredCoin(sc); c != "" {
			configuredCoins[c] = true
		}
	}
	for _, sc := range strategies {
		if !strategyHedgeEnabled(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
		}
		if !hyperliquidIsLive(sc.Args) {
			errs = append(errs, fmt.Sprintf("%s: hedge requires --mode=live (phase 1 has no paper/backtest hedge model)", prefix))
		}
		if EffectiveDirection(sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s: hedge is not supported with direction=\"both\" (phase 1: flips are ambiguous to hedge)", prefix))
		}
		hc := hedgeCoinForStrategy(sc)
		if hc == "" {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol is required", prefix))
		}
		if sc.Hedge.Side != HedgeSideInverse {
			errs = append(errs, fmt.Sprintf("%s: hedge.side must be %q (the only phase-1-supported value), got %q", prefix, HedgeSideInverse, sc.Hedge.Side))
		}
		if sc.Hedge.Ratio <= 0 || sc.Hedge.Ratio > hedgeMaxRatio {
			errs = append(errs, fmt.Sprintf("%s: hedge.ratio must be in (0, %g], got %g", prefix, hedgeMaxRatio, sc.Hedge.Ratio))
		}
		if sc.Hedge.Platform != "" && sc.Hedge.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge.platform must be empty or \"hyperliquid\", got %q", prefix, sc.Hedge.Platform))
		}
		if sc.Hedge.Type != "" && sc.Hedge.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge.type must be empty or \"perps\", got %q", prefix, sc.Hedge.Type))
		}
		if sc.Hedge.MarginMode != "isolated" && sc.Hedge.MarginMode != "cross" {
			errs = append(errs, fmt.Sprintf("%s: hedge.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, sc.Hedge.MarginMode))
		}
		if sc.Hedge.Leverage <= 0 || sc.Hedge.Leverage > 100 {
			errs = append(errs, fmt.Sprintf("%s: hedge.leverage must be in (0, 100], got %g", prefix, sc.Hedge.Leverage))
		}
		if hc != "" {
			primaryCoin := hyperliquidConfiguredCoin(sc)
			if hc == primaryCoin {
				errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q must differ from the strategy's own primary coin", prefix, hc))
			} else if configuredCoins[hc] {
				errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q collides with another configured strategy's primary coin — HL aggregates per coin per account, hedge coins must be sole-owned", prefix, hc))
			}
			hedgeCoins[hc] = append(hedgeCoins[hc], sc.ID)
		}
	}
	coins := make([]string, 0, len(hedgeCoins))
	for c := range hedgeCoins {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, c := range coins {
		ids := hedgeCoins[c]
		if len(ids) > 1 {
			sortedIDs := append([]string(nil), ids...)
			sort.Strings(sortedIDs)
			errs = append(errs, fmt.Sprintf("hedge.symbol %q is used by multiple strategies (%s) as their hedge coin — hedge-vs-hedge collision: HL aggregates per coin per account", c, strings.Join(sortedIDs, ", ")))
		}
	}
	return errs
}

// hedgeConfigEqual reports whether two HedgeConfig values are identical
// (including both nil). Used by the hot-reload state-compat guard — every
// field change is state-shifting while a position is open, so this is a
// simple equality check, not a semantic diff. HedgeConfig has only
// comparable fields (bool/string/float64), so a plain struct compare is
// exact.
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
