package main

// Correlated hedge legs (#1159, phase 1 — Hyperliquid perps).
//
// DESIGN: hedge management is a per-cycle, state-derived RECONCILER, not a set
// of mirror hooks bolted onto each primary lifecycle event. One pure function
// (hedgeTargetDecision) computes the hedge target from the CURRENT primary
// position versus a persisted quantity watermark (Position.HedgePrimaryQtyBasis),
// and one orchestrator (runHedgeSync) converges the hedge leg to that target on
// every HL perps dispatch cycle — including the Signal==0 manage path.
//
// The consequence is that every primary quantity event automatically produces
// the matching hedge action without touching the individual close paths: fresh
// open, scale-in add, evaluator partial close, on-chain SL/TP fill detected by
// reconcile, trailing-ratchet close, #822 orphan close, and external close all
// converge within the same or the next cycle. Only events that bypass the
// per-strategy dispatch loop entirely — the portfolio kill switch and the
// per-strategy circuit-breaker drain — get explicit extensions elsewhere.
//
// INVARIANTS
//
//   - Quantity-event mirroring, never price mirroring. The target is keyed to
//     the primary QUANTITY watermark, so mark drift can never re-trade the hedge.
//   - Fill-confirmed state mutation only. Virtual hedge state moves solely from
//     a confirmed fill, mirroring runHyperliquidExecuteOrder's ok2=false → no
//     state update contract.
//   - Fail-closed open. Primary fill confirmed + hedge open failed on the same
//     cycle → immediately reduce-only close the primary fill (cancelling its
//     just-armed protection OIDs) and raise a CRITICAL owner alert. If the
//     unwind itself fails, no latch is needed: the state-derived sync retries
//     from the persisted state next cycle.
//   - Sole ownership by construction. validateHedgeConfigs guarantees a hedge
//     coin is never any strategy's configured coin and never shared between
//     hedgers, so hyperliquidConfiguredCoin-derived shared-coin machinery stays
//     correct while remaining blind to hedge coins.
//   - No hedge protection. The hedge symbol never enters the protection sync,
//     the trailing walker, the regime store, the LLM analysis queue, or any
//     check script.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// HedgeTradeType labels every Trade row belonging to a hedge leg. Ledger
	// sums ignore trade_type by design (trade_pnl.go), which is exactly what
	// books hedge PnL and fees into the owning strategy's ledger; the
	// round-trip stats queries exclude it so a hedge never counts as alpha.
	HedgeTradeType = "hedge"
	// hedgeMinOrderNotionalUSD is Hyperliquid's practical minimum order value.
	// A reduce smaller than this is deferred (and the basis deliberately NOT
	// advanced) so the shortfall accumulates into a fillable order instead of
	// being silently dropped.
	hedgeMinOrderNotionalUSD = 10.0
	// hedgeQtyEpsilon absorbs float noise when diffing quantities.
	hedgeQtyEpsilon = 1e-9
)

// perpsTradeTypeForPosition returns the trade_type label a perps close leg
// should carry: "hedge" for an auto-managed hedge leg, "perps" otherwise.
func perpsTradeTypeForPosition(pos *Position) string {
	if pos.isHedgeLeg() {
		return HedgeTradeType
	}
	return "perps"
}

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
	}
	return "none"
}

// hedgeSnapshot is the Phase-1 RLock capture the lock-free decision runs on.
type hedgeSnapshot struct {
	PrimarySymbol string
	PrimaryQty    float64
	PrimarySide   string
	HedgeCoin     string
	HedgeQty      float64
	HedgeSide     string
	// HedgeBasis is the persisted primary-quantity watermark the current hedge
	// size was derived from. 0 with a hedge held means a legacy/unstamped leg;
	// the decision adopts the live primary quantity rather than guessing a diff.
	HedgeBasis float64
	// HedgeCancelOIDs / PrimaryCancelOIDs carry resting protection OIDs. Only
	// the primary ever has any (a hedge leg is never given SL/TP), but the
	// unwind path needs the primary's so a fail-closed unwind doesn't strand
	// triggers against HL's per-account cap.
	PrimaryCancelOIDs []int64
}

// hedgeAction is the converged target for this cycle.
type hedgeAction struct {
	Kind hedgeActionKind
	// Qty is the hedge-coin quantity to trade (always positive).
	Qty float64
	// Side is the hedge POSITION side an open/add establishes.
	Side string
	// NewBasis is the primary quantity to stamp on the hedge leg once the fill
	// confirms. Only meaningful for open/add/reduce.
	NewBasis float64
	// AdoptBasis, when > 0 on a Kind==none decision, means the hedge leg is
	// correctly sized but its watermark is missing (legacy row): stamp it
	// without trading.
	AdoptBasis float64
	// Blocked marks a fail-closed outcome: the decision could not be computed
	// safely (unusable price, unmappable side). The caller must alert, and on a
	// fresh-open cycle escalate to unwinding the primary rather than running
	// unhedged.
	Blocked bool
	Reason  string
}

// hedgeTargetDecision is the pure decision core: given the strategy config, a
// state snapshot, and the two marks, it returns the single hedge action that
// converges the hedge leg to its target. No I/O, no locks, no clock.
//
// primaryPx and hedgePx are marks (not fill prices) — they size the order only;
// the resulting virtual state is always booked from the CONFIRMED fill.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge not enabled"}
	}
	if snap.HedgeCoin == "" {
		return hedgeAction{Kind: hedgeActionNone, Blocked: true, Reason: "hedge coin unresolved"}
	}

	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	// Primary flat: the hedge has nothing to hedge. Close it in full, whatever
	// its basis says — this is the terminal convergence that makes every
	// primary close path (evaluator, SL, TP, orphan, external) self-mirroring.
	if !primaryHeld {
		if !hedgeHeld {
			return hedgeAction{Kind: hedgeActionNone, Reason: "primary and hedge both flat"}
		}
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "primary flat"}
	}

	want := inverseSide(snap.PrimarySide)
	if want == "" {
		// Unmappable primary side — never guess a direction to trade.
		return hedgeAction{Kind: hedgeActionNone, Blocked: true, Reason: fmt.Sprintf("primary side %q is not long/short", snap.PrimarySide)}
	}

	// Defense in depth: a hedge on the wrong side is worse than no hedge (it
	// doubles the primary's beta). Unwind it; the next cycle re-opens correctly.
	// Unreachable while direction "both" is rejected at load.
	if hedgeHeld && snap.HedgeSide != want {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide,
			Reason: fmt.Sprintf("hedge side %q does not mirror primary %q — unwinding", snap.HedgeSide, snap.PrimarySide)}
	}

	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, Blocked: true,
			Reason: fmt.Sprintf("unusable marks (primary=%.6f hedge=%.6f) — cannot size the hedge", primaryPx, hedgePx)}
	}
	ratio := HedgeRatio(sc)

	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Blocked: true, Reason: "computed hedge quantity is zero"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: want, NewBasis: snap.PrimaryQty,
			Reason: fmt.Sprintf("primary %s %.6f @ $%.4f × ratio %.2f", snap.PrimarySide, snap.PrimaryQty, primaryPx, ratio)}
	}

	// Hedge held on the right side — diff against the watermark.
	if snap.HedgeBasis <= 0 {
		// Legacy / never-stamped leg. Adopting the live primary quantity is the
		// only non-destructive choice: trading off an unknown basis could add
		// or reduce arbitrarily.
		return hedgeAction{Kind: hedgeActionNone, AdoptBasis: snap.PrimaryQty,
			Reason: "hedge basis missing — adopting current primary quantity as the watermark"}
	}

	delta := snap.PrimaryQty - snap.HedgeBasis
	if delta > hedgeQtyEpsilon {
		qty := delta * primaryPx * ratio / hedgePx
		if qty*hedgePx < hedgeMinOrderNotionalUSD {
			// Basis deliberately NOT advanced: the shortfall accumulates until
			// it clears the exchange minimum.
			return hedgeAction{Kind: hedgeActionNone,
				Reason: fmt.Sprintf("hedge add of $%.2f is below the $%.2f exchange minimum — deferring", qty*hedgePx, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: want, NewBasis: snap.PrimaryQty,
			Reason: fmt.Sprintf("primary grew %.6f → %.6f", snap.HedgeBasis, snap.PrimaryQty)}
	}
	if delta < -hedgeQtyEpsilon {
		// Proportional reduce: mirror the FRACTION of the primary that closed,
		// applied to the live hedge quantity. Working off the fraction (rather
		// than re-deriving a notional) keeps the hedge proportional even when
		// its own mark has moved far from the open.
		frac := (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if frac > 1 {
			frac = 1
		}
		qty := snap.HedgeQty * frac
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge reduce is zero"}
		}
		if qty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone,
				Reason: fmt.Sprintf("hedge reduce of $%.2f is below the $%.2f exchange minimum — deferring", qty*hedgePx, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty, Side: snap.HedgeSide, NewBasis: snap.PrimaryQty,
			Reason: fmt.Sprintf("primary shrank %.6f → %.6f (%.1f%%)", snap.HedgeBasis, snap.PrimaryQty, frac*100)}
	}

	return hedgeAction{Kind: hedgeActionNone, Reason: "hedge in sync"}
}

// captureHedgeSnapshot reads the primary + hedge legs out of strategy state.
// Caller must hold at least a read lock.
func captureHedgeSnapshot(sc StrategyConfig, s *StrategyState, primarySym string) hedgeSnapshot {
	snap := hedgeSnapshot{PrimarySymbol: primarySym, HedgeCoin: hedgeCoin(sc)}
	if s == nil {
		return snap
	}
	if pos := s.Positions[primarySym]; pos != nil && pos.Quantity > 0 {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
		snap.PrimaryCancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	if snap.HedgeCoin != "" {
		if pos := s.Positions[snap.HedgeCoin]; pos != nil && pos.Quantity > 0 && pos.isHedgeLeg() {
			snap.HedgeQty = pos.Quantity
			snap.HedgeSide = pos.Side
			snap.HedgeBasis = pos.HedgePrimaryQtyBasis
		}
	}
	return snap
}

// hedgeOrderSkipReason re-checks the decision's preconditions against a FRESH
// snapshot immediately before spawning the order (the CLAUDE.md skip-reason
// mirror rule). A non-empty return means the world moved between the Phase-1
// capture and the spawn, so the order must not be sent — otherwise an on-chain
// fill could land with no matching Trade record.
func hedgeOrderSkipReason(action hedgeAction, fresh hedgeSnapshot) string {
	switch action.Kind {
	case hedgeActionNone:
		return "no hedge action"
	case hedgeActionOpen:
		if fresh.PrimaryQty <= hedgeQtyEpsilon {
			return "primary went flat before the hedge open"
		}
		if inverseSide(fresh.PrimarySide) != action.Side {
			return fmt.Sprintf("primary side changed to %q before the hedge open", fresh.PrimarySide)
		}
		if fresh.HedgeQty > hedgeQtyEpsilon {
			return "hedge leg already exists"
		}
	case hedgeActionAdd:
		if fresh.PrimaryQty <= hedgeQtyEpsilon {
			return "primary went flat before the hedge add"
		}
		if fresh.HedgeQty <= hedgeQtyEpsilon {
			return "hedge leg disappeared before the add"
		}
		if fresh.HedgeSide != action.Side {
			return fmt.Sprintf("hedge side changed to %q before the add", fresh.HedgeSide)
		}
		if fresh.PrimaryQty < action.NewBasis-hedgeQtyEpsilon {
			return "primary shrank before the hedge add"
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if fresh.HedgeQty <= hedgeQtyEpsilon {
			return "hedge leg already flat"
		}
	}
	return ""
}

// hedgeExecutor abstracts the three on-chain calls hedge sync can make so the
// orchestrator is testable without spawning Python.
type hedgeExecutor interface {
	// OpenHedge submits a market order on the hedge coin. applyMargin is true
	// only on a FRESH open (HL rejects update_leverage on an open position).
	OpenHedge(sc StrategyConfig, coin, orderSide string, qty float64, applyMargin bool) (*HyperliquidExecuteResult, error)
	// CloseHedge submits a sized reduce-only close on the hedge coin.
	CloseHedge(sc StrategyConfig, coin string, qty float64) (*HyperliquidCloseResult, error)
	// ClosePrimary submits a SIZED reduce-only close of the primary leg for the
	// fail-closed unwind. Always sized, never a full market_close: the primary
	// coin may have shared-coin peers.
	ClosePrimary(sc StrategyConfig, symbol string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error)
}

// liveHedgeExecutor is the production executor. Every call routes through the
// existing side-effecting wrappers, so shutdown draining and subprocess
// timeouts apply unchanged.
type liveHedgeExecutor struct{}

func (liveHedgeExecutor) OpenHedge(sc StrategyConfig, coin, orderSide string, qty float64, applyMargin bool) (*HyperliquidExecuteResult, error) {
	marginMode := ""
	leverage := 0.0
	if applyMargin {
		marginMode = hedgeMarginMode(sc)
		leverage = hedgeLeverage(sc)
	}
	// stopLossPct=0 and no cancel OIDs: a hedge leg carries no protection by
	// design (#1159 constraint 1).
	res, _, err := RunHyperliquidExecute(sc.Script, coin, orderSide, qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
	return res, err
}

func (liveHedgeExecutor) CloseHedge(sc StrategyConfig, coin string, qty float64) (*HyperliquidCloseResult, error) {
	sz := qty
	res, _, err := RunHyperliquidClose(sc.Script, coin, &sz, nil)
	return res, err
}

func (liveHedgeExecutor) ClosePrimary(sc StrategyConfig, symbol string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
	sz := qty
	res, _, err := RunHyperliquidClose(sc.Script, symbol, &sz, cancelOIDs)
	return res, err
}

// hedgeFill is the normalized outcome of a hedge order: quantity, price, fee,
// and provenance. Paper mode synthesizes one at the mark with a modeled fee.
type hedgeFill struct {
	Qty       float64
	Px        float64
	Fee       float64
	OID       int64
	FromChain bool
}

// executeHedgeOrder submits the action and normalizes the outcome. Returns a
// nil fill (with a non-nil error, or an error-free "no fill") when nothing
// executed — the caller must not mutate state in that case.
func executeHedgeOrder(sc StrategyConfig, action hedgeAction, coin string, hedgePx float64, live bool, exec hedgeExecutor) (*hedgeFill, error) {
	if !live {
		// Paper: the hedge is virtual-only. Book at the mark with the modeled
		// taker fee, exactly like every other paper perps leg.
		if hedgePx <= 0 {
			return nil, fmt.Errorf("paper hedge needs a positive mark, got %.6f", hedgePx)
		}
		return &hedgeFill{Qty: action.Qty, Px: hedgePx, Fee: CalculatePlatformSpotFee("hyperliquid", action.Qty*hedgePx)}, nil
	}
	if exec == nil {
		return nil, fmt.Errorf("hedge executor unavailable")
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		res, err := exec.OpenHedge(sc, coin, hedgeOrderSide(action.Side), action.Qty, action.Kind == hedgeActionOpen)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("hedge %s returned no result", action.Kind)
		}
		if res.Error != "" {
			return nil, fmt.Errorf("hedge %s: %s", action.Kind, res.Error)
		}
		if res.Execution == nil || res.Execution.Fill == nil || res.Execution.Fill.TotalSz <= 0 || res.Execution.Fill.AvgPx <= 0 {
			return nil, fmt.Errorf("hedge %s reported no confirmed fill", action.Kind)
		}
		f := res.Execution.Fill
		return &hedgeFill{Qty: f.TotalSz, Px: f.AvgPx, Fee: f.Fee, OID: f.OID, FromChain: true}, nil

	case hedgeActionReduce, hedgeActionCloseFull:
		res, err := exec.CloseHedge(sc, coin, action.Qty)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("hedge close returned no result")
		}
		if res.Error != "" {
			return nil, fmt.Errorf("hedge close: %s", res.Error)
		}
		if res.Close != nil && res.Close.AlreadyFlat {
			// Nothing on-chain to close. Reported as a fill-less success so the
			// caller clears the virtual leg without inventing PnL.
			return nil, nil
		}
		if res.Close == nil || res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 || res.Close.Fill.AvgPx <= 0 {
			return nil, fmt.Errorf("hedge close reported no confirmed fill")
		}
		f := res.Close.Fill
		return &hedgeFill{Qty: f.TotalSz, Px: f.AvgPx, Fee: f.Fee, OID: f.OID, FromChain: true}, nil
	}
	return nil, fmt.Errorf("unsupported hedge action %s", action.Kind)
}

// applyHedgeFill mutates virtual state from a CONFIRMED hedge fill. Caller must
// hold the write lock.
//
// Opens/adds create or blend the hedge Position and record an open-side Trade;
// reduces/closes reuse the existing perps booking helpers, which already give
// the #954 duplicate-OID guard, the #1009 corrupt-position zero-PnL handling,
// and (via perpsTradeTypeForPosition / recordTradeResultForPosition) the
// hedge-aware trade_type and loss-streak exemption.
func applyHedgeFill(s *StrategyState, sc StrategyConfig, action hedgeAction, coin, primarySym string, fill hedgeFill, logger *StrategyLogger) {
	if s == nil || fill.Qty <= 0 || fill.Px <= 0 {
		return
	}
	oidStr := ""
	if fill.OID > 0 {
		oidStr = fmt.Sprintf("%d", fill.OID)
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		now := time.Now().UTC()
		pos := s.Positions[coin]
		if pos == nil || pos.Quantity <= 0 {
			pos = &Position{
				Symbol:          coin,
				Quantity:        fill.Qty,
				InitialQuantity: fill.Qty,
				AvgCost:         fill.Px,
				Side:            action.Side,
				Multiplier:      1,
				Leverage:        hedgeLeverage(sc),
				OwnerStrategyID: sc.ID,
				OpenedAt:        now,
				HedgeFor:        primarySym,
			}
			s.Positions[coin] = pos
		} else {
			// Blend price + size, mirroring the scale-in convention.
			total := pos.Quantity + fill.Qty
			pos.AvgCost = (pos.AvgCost*pos.Quantity + fill.Px*fill.Qty) / total
			pos.Quantity = total
			pos.HedgeFor = primarySym
		}
		pos.HedgePrimaryQtyBasis = action.NewBasis

		fee := fill.Fee
		feeSource := FeeSourceUserFills
		if !fill.FromChain {
			feeSource = FeeSourceModeled
		}
		// Opens debit the fee from virtual cash exactly like the primary's
		// perps open path; PnL is realized on the close leg.
		s.Cash -= fee
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          coin,
			PositionID:      ensurePositionTradeID(s.ID, coin, pos),
			Side:            hedgeOrderSide(action.Side),
			Quantity:        fill.Qty,
			Price:           fill.Px,
			Value:           fill.Qty * fill.Px,
			TradeType:       HedgeTradeType,
			Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.4f — %s", primarySym, action.Kind, action.Side, fill.Qty, fill.Px, action.Reason),
			ExchangeOrderID: oidStr,
			ExchangeFee:     fee,
			PnLGross:        true,
			FeeSource:       feeSource,
			Regime:          s.Regime,
		}
		RecordTrade(s, trade)
		if logger != nil {
			logger.Info("hedge(%s): %s %s %.6f %s @ $%.4f (fee $%.4f) — %s", primarySym, action.Kind, action.Side, fill.Qty, coin, fill.Px, fee, action.Reason)
		}

	case hedgeActionReduce:
		detail := fmt.Sprintf("hedge(%s) reduce", primarySym)
		if bookPerpsPartialCloseWithFillFee(s, coin, fill.Qty, fill.Px, fill.Fee, fill.FromChain, oidStr, "hedge_reduce", detail, "hedge reduce", logger) {
			if pos := s.Positions[coin]; pos != nil {
				pos.HedgePrimaryQtyBasis = action.NewBasis
			}
		}

	case hedgeActionCloseFull:
		detail := fmt.Sprintf("hedge(%s) close", primarySym)
		bookPerpsCloseWithFillFee(s, coin, fill.Px, fill.Fee, fill.FromChain, oidStr, "hedge_close", detail, "hedge close", logger)
	}
}

// hedgeSyncInputs bundles what runHedgeSync needs from the dispatch site.
type hedgeSyncInputs struct {
	PrimarySymbol string
	PrimaryPx     float64
	HedgePx       float64
	// FreshOpen is true when THIS cycle produced a confirmed primary open (or
	// scale-in add). It selects the fail-closed escalation: a hedge failure on
	// the opening cycle unwinds the primary rather than leaving it unhedged.
	FreshOpen bool
	Live      bool
}

// runHedgeSync converges the hedge leg to its target for one strategy. Follows
// the repo's 6-phase lock discipline: snapshot under RLock, decide and spawn
// with no lock held, apply under Lock. Notifications are sent after every lock
// is released.
//
// Deliberately NOT gated by pause (#1150), the daily-loss hold (#1269), the
// exposure cap (#1270), or the regime gate: a hedge order is a coupled
// risk-management leg, not a signal. Those states can only hold the PRIMARY
// from growing, so hedge sync under them can only reduce or close — which is
// exactly what they want.
func runHedgeSync(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, in hedgeSyncInputs, exec hedgeExecutor, notifier *MultiNotifier, logger *StrategyLogger) {
	if !HedgeEnabled(sc) || s == nil || mu == nil {
		return
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return
	}

	mu.RLock()
	snap := captureHedgeSnapshot(sc, s, in.PrimarySymbol)
	mu.RUnlock()

	action := hedgeTargetDecision(sc, snap, in.PrimaryPx, in.HedgePx)

	if action.Blocked {
		msg := fmt.Sprintf("hedge(%s) on %s: %s", in.PrimarySymbol, coin, action.Reason)
		if logger != nil {
			logger.Error("%s", msg)
		}
		if in.FreshOpen && snap.PrimaryQty > hedgeQtyEpsilon {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, snap, action.Reason, in.Live, exec, notifier, logger)
			return
		}
		notifyLiveExecFailure(notifier, sc, "hedge", coin, action.Reason)
		return
	}

	if action.Kind == hedgeActionNone {
		if action.AdoptBasis > 0 {
			mu.Lock()
			if pos := s.Positions[coin]; pos != nil && pos.isHedgeLeg() {
				pos.HedgePrimaryQtyBasis = action.AdoptBasis
			}
			mu.Unlock()
			if logger != nil {
				logger.Warn("hedge(%s) on %s: %s", in.PrimarySymbol, coin, action.Reason)
			}
		} else if logger != nil && action.Reason != "hedge in sync" && action.Reason != "primary and hedge both flat" {
			logger.Info("hedge(%s) on %s: %s", in.PrimarySymbol, coin, action.Reason)
		}
		return
	}

	// Skip-reason mirror: re-read state immediately before spawning so an
	// on-chain fill can never land without a matching Trade record.
	mu.RLock()
	fresh := captureHedgeSnapshot(sc, s, in.PrimarySymbol)
	mu.RUnlock()
	if why := hedgeOrderSkipReason(action, fresh); why != "" {
		if logger != nil {
			logger.Info("hedge(%s) on %s: skipping %s — %s", in.PrimarySymbol, coin, action.Kind, why)
		}
		return
	}

	fill, err := executeHedgeOrder(sc, action, coin, in.HedgePx, in.Live, exec)
	if err != nil {
		errMsg := err.Error()
		if logger != nil {
			logger.Error("hedge(%s) on %s: %s failed — %s", in.PrimarySymbol, coin, action.Kind, errMsg)
		}
		// Fail-closed (#1159 constraint 4): a confirmed primary fill this cycle
		// with no hedge behind it must not run unhedged. Later cycles' failures
		// alert and retry instead — the primary was already hedged, or is being
		// converged down.
		if in.FreshOpen && (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, fresh, errMsg, in.Live, exec, notifier, logger)
			return
		}
		notifyLiveExecFailure(notifier, sc, "hedge", coin, errMsg)
		return
	}
	clearLiveExecThrottle(sc, "hedge", coin)

	if fill == nil {
		// already_flat on a close: clear the virtual leg without booking a fill.
		if action.Kind == hedgeActionCloseFull || action.Kind == hedgeActionReduce {
			mu.Lock()
			if pos := s.Positions[coin]; pos != nil && pos.isHedgeLeg() {
				recordClosedPosition(s, pos, pos.AvgCost, 0, "hedge_close_already_flat", time.Now().UTC())
				delete(s.Positions, coin)
			}
			mu.Unlock()
			if logger != nil {
				logger.Warn("hedge(%s) on %s: already flat on-chain — cleared the virtual leg with zero PnL", in.PrimarySymbol, coin)
			}
		}
		return
	}

	mu.Lock()
	applyHedgeFill(s, sc, action, coin, in.PrimarySymbol, *fill, logger)
	mu.Unlock()
}

// unwindPrimaryAfterHedgeOpenFailure implements the phase-1 fail-closed policy
// (#1159 constraint 4): the primary filled, the hedge did not, so the primary
// is immediately closed reduce-only and the operator is alerted CRITICAL.
//
// The close is always SIZED — never a bare market_close — because the primary
// coin may have shared-coin peers whose exposure must not be touched. Resting
// protection OIDs armed at open are cancelled with it so the unwind doesn't
// strand triggers against Hyperliquid's per-account cap.
//
// If the unwind itself fails, nothing is latched: the next cycle's state-derived
// sync sees the same primary-held/hedge-flat state and either opens the hedge
// (now hedged) or unwinds again. That degraded loop is deliberate — it is
// restart-safe and needs no new persisted flag.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, snap hedgeSnapshot, why string, live bool, exec hedgeExecutor, notifier *MultiNotifier, logger *StrategyLogger) {
	sym := snap.PrimarySymbol
	qty := snap.PrimaryQty
	if qty <= hedgeQtyEpsilon {
		return
	}

	alert := fmt.Sprintf("🚨 CRITICAL: %s — hedge leg on %s could not be opened (%s). Closing the primary %s position (%.6f %s) reduce-only rather than running unhedged (#1159).",
		sc.ID, hedgeCoin(sc), why, sym, qty, snap.PrimarySide)
	if logger != nil {
		logger.Error("%s", alert)
	}

	var fillPx, fillFee float64
	var fillQty float64
	var oidStr string
	fromChain := false

	if live {
		if exec == nil {
			if notifier != nil {
				notifier.SendOwnerDM(alert + " UNWIND FAILED: no executor available — the primary is STILL OPEN AND UNHEDGED. Intervene now.")
			}
			return
		}
		res, err := exec.ClosePrimary(sc, sym, qty, snap.PrimaryCancelOIDs)
		if err != nil || res == nil || res.Error != "" {
			detail := "no result"
			if err != nil {
				detail = err.Error()
			} else if res != nil && res.Error != "" {
				detail = res.Error
			}
			failMsg := alert + fmt.Sprintf(" UNWIND FAILED (%s) — the primary is STILL OPEN AND UNHEDGED; the next cycle retries the hedge or the unwind. Intervene now.", detail)
			if logger != nil {
				logger.Error("%s", failMsg)
			}
			if notifier != nil {
				notifier.SendOwnerDM(failMsg)
				notifier.SendToAllChannels(failMsg)
			}
			return
		}
		if res.Close != nil && res.Close.AlreadyFlat {
			if logger != nil {
				logger.Warn("hedge unwind: %s already flat on-chain — nothing to close", sym)
			}
			if notifier != nil {
				notifier.SendOwnerDM(alert + " (primary was already flat on-chain)")
			}
			return
		}
		if res.Close == nil || res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 || res.Close.Fill.AvgPx <= 0 {
			failMsg := alert + " UNWIND FAILED (no confirmed fill) — the primary is STILL OPEN AND UNHEDGED. Intervene now."
			if logger != nil {
				logger.Error("%s", failMsg)
			}
			if notifier != nil {
				notifier.SendOwnerDM(failMsg)
				notifier.SendToAllChannels(failMsg)
			}
			return
		}
		f := res.Close.Fill
		fillQty, fillPx, fillFee, fromChain = f.TotalSz, f.AvgPx, f.Fee, true
		if f.OID > 0 {
			oidStr = fmt.Sprintf("%d", f.OID)
		}
	} else {
		// Paper: unwind at the primary mark with a modeled fee.
		mu.RLock()
		var avg float64
		if pos := s.Positions[sym]; pos != nil {
			avg = pos.AvgCost
		}
		mu.RUnlock()
		if avg <= 0 {
			return
		}
		fillQty, fillPx = qty, avg
		fillFee = CalculatePlatformSpotFee("hyperliquid", fillQty*fillPx)
	}

	mu.Lock()
	if math.Abs(fillQty-qty) <= hedgeQtyEpsilon {
		bookPerpsCloseWithFillFee(s, sym, fillPx, fillFee, fromChain, oidStr, "hedge_open_failed_unwind", "Hedge-open failure unwind", "hedge unwind close", logger)
	} else {
		bookPerpsPartialCloseWithFillFee(s, sym, fillQty, fillPx, fillFee, fromChain, oidStr, "hedge_open_failed_unwind", "Hedge-open failure unwind", "hedge unwind close", logger)
	}
	mu.Unlock()

	if notifier != nil {
		done := alert + fmt.Sprintf(" Primary closed at $%.4f.", fillPx)
		notifier.SendOwnerDM(done)
		notifier.SendToAllChannels(done)
	}
}

// hedgeCoinsForStrategies returns the set of hedge coins declared by enabled
// hedge blocks, deduplicated. Consumers that must SEE hedge coins (mark
// fetching, the fill resolver's coin universe) use this; consumers that must
// not (peer detection, margin matching) keep using hyperliquidConfiguredCoin.
func hedgeCoinsForStrategies(strategies []StrategyConfig) map[string]bool {
	out := make(map[string]bool)
	for _, sc := range strategies {
		if coin := hedgeCoin(sc); coin != "" {
			out[coin] = true
		}
	}
	return out
}

// strategyHeldHedgeCoin returns the hedge coin a strategy CURRENTLY holds a
// virtual hedge leg on, or "" when it holds none. Gating on the held leg rather
// than on config alone is what stops the kill switch and the circuit-breaker
// drain from liquidating a genuinely foreign position that happens to sit on a
// declared-but-flat hedge coin. Caller must hold at least a read lock.
func strategyHeldHedgeCoin(sc StrategyConfig, s *StrategyState) string {
	coin := hedgeCoin(sc)
	if coin == "" || s == nil {
		return ""
	}
	if pos := s.Positions[coin]; pos != nil && pos.Quantity > 0 && pos.isHedgeLeg() {
		return coin
	}
	return ""
}

// validateHedgeStateConsistency detects persisted hedge legs that no longer
// match the live config (#1159 acceptance 3). A config edit plus a process
// RESTART bypasses the SIGHUP hot-reload guard entirely — there is no "old"
// value to diff against — so a strategy can boot holding a hedge leg its
// config no longer declares, or one sitting on a different coin.
//
// Deliberately NON-destructive: the position is left frozen and surfaced to the
// operator, mirroring the shared-coin ambiguity convention. Auto-closing on a
// warning would let a typo'd config edit liquidate live exposure at boot.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	var warnings []string
	if state == nil || cfg == nil {
		return nil
	}
	for _, sc := range cfg.Strategies {
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		configured := hedgeCoin(sc)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || pos.Quantity <= 0 || !pos.isHedgeLeg() {
				continue
			}
			switch {
			case configured == "":
				warnings = append(warnings, fmt.Sprintf(
					"strategy[%s] holds a hedge leg on %s (%.6f %s, hedging %s) but its config no longer enables a hedge — the leg is FROZEN (not auto-managed, not auto-closed). Re-enable the hedge block or close the leg manually (#1159)",
					sc.ID, sym, pos.Quantity, pos.Side, pos.HedgeFor))
			case sym != configured:
				warnings = append(warnings, fmt.Sprintf(
					"strategy[%s] holds a hedge leg on %s but hedge.symbol now resolves to %s — the %s leg is FROZEN (not auto-managed, not auto-closed). Close it manually before trading the new hedge coin (#1159)",
					sc.ID, sym, configured, sym))
			}
		}
	}
	return warnings
}

// HedgeStatus is the operator-facing view of a strategy's correlated hedge leg
// (#1159 requirement 7) — surfaced on /status, the dashboard, and Discord so a
// hedge position is never mistaken for an unmanaged one.
type HedgeStatus struct {
	Symbol     string  `json:"symbol"`              // hedge coin
	PrimaryFor string  `json:"primary_for"`         // the primary symbol this hedge is coupled to
	Side       string  `json:"side"`                // configured side policy ("inverse")
	Ratio      float64 `json:"ratio"`               // notional multiplier
	MarginMode string  `json:"margin_mode"`         //
	Leverage   float64 `json:"leverage"`            //
	Held       bool    `json:"held"`                // a hedge leg is currently open
	Quantity   float64 `json:"quantity,omitempty"`  //
	HeldSide   string  `json:"held_side,omitempty"` // "long"/"short" of the live leg
	AvgCost    float64 `json:"avg_cost,omitempty"`  //
	QtyBasis   float64 `json:"qty_basis,omitempty"` // primary quantity the leg was last sized against
}

// buildHedgeStatus renders a strategy's hedge state. Returns nil when the
// strategy has no enabled hedge block. Caller must hold at least a read lock.
func buildHedgeStatus(sc StrategyConfig, s *StrategyState) *HedgeStatus {
	coin := hedgeCoin(sc)
	if coin == "" {
		return nil
	}
	out := &HedgeStatus{
		Symbol:     coin,
		Side:       hedgeSide(sc),
		Ratio:      HedgeRatio(sc),
		MarginMode: hedgeMarginMode(sc),
		Leverage:   hedgeLeverage(sc),
	}
	if s != nil {
		if pos := s.Positions[coin]; pos != nil && pos.Quantity > 0 && pos.isHedgeLeg() {
			out.Held = true
			out.Quantity = pos.Quantity
			out.HeldSide = pos.Side
			out.AvgCost = pos.AvgCost
			out.QtyBasis = pos.HedgePrimaryQtyBasis
			out.PrimaryFor = pos.HedgeFor
		}
	}
	if out.PrimaryFor == "" {
		out.PrimaryFor = hyperliquidConfiguredCoin(sc)
	}
	return out
}

// hedgeStatusNote renders a one-line Discord /status footnote listing every
// strategy that currently HOLDS a hedge leg. Empty when none do. Sorted for
// deterministic output (Go map iteration is randomized).
func hedgeStatusNote(strategies []StrategyConfig, state *AppState) string {
	if state == nil {
		return ""
	}
	var lines []string
	for _, sc := range strategies {
		st := buildHedgeStatus(sc, state.Strategies[sc.ID])
		if st == nil || !st.Held {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: hedge %s %.6f %s (hedging %s, ratio %.2f)", sc.ID, st.HeldSide, st.Quantity, st.Symbol, st.PrimaryFor, st.Ratio))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return "🔗 Correlated hedge legs (auto-managed, not independent positions):\n" + strings.Join(lines, "\n")
}
