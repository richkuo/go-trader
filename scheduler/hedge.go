package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Correlated hedge legs (#1159, phase 1).
//
// A hedge-enabled strategy carries a SECOND Hyperliquid perps position on a
// different, correlated coin, opened opposite the primary so the pair runs with
// reduced market beta while the strategy's relative thesis stays intact.
//
// The central design decision is that hedge management is a per-cycle,
// STATE-DERIVED RECONCILER, not a set of per-event mirror hooks. One pure
// function (hedgeTargetDecision) computes the hedge target from the CURRENT
// primary position versus a persisted quantity watermark
// (Position.HedgePrimaryQtyBasis), and one orchestrator (runHedgeSync)
// converges the hedge leg to that target on every HL dispatch cycle.
//
// Why that matters: a primary position changes size through at least eight
// different code paths — fresh open, scale-in add, close-evaluator partial,
// close-evaluator full, on-chain SL fill detected by reconcile, on-chain TP
// fill detected by reconcile, ratchet close, external/manual close on the
// exchange. Hooking each one would mean eight places to keep correct forever,
// and any path added later would silently skip the hedge. Deriving the target
// from state instead means every one of those paths mirrors automatically
// within the same or the next cycle, and a restart mid-sequence resumes from
// the persisted watermark with no recovery logic of its own.
//
// The events that DON'T flow through the per-strategy dispatch — portfolio
// kill switch, per-strategy circuit-breaker force-close, manual force-close —
// bypass this reconciler entirely and therefore get explicit extensions in
// their own files.
//
// Phase-1 scope: HL perps only (live and paper). No hedge stop-loss, no hedge
// take-profit, no close evaluator, no check script. The hedge coin never
// enters runHyperliquidProtectionSync, the trailing-stop walker, or the regime
// store.

// hedgeQtyEpsilon is the quantity tolerance below which a primary/hedge
// difference is float noise rather than a real position event. Sized well
// above float64 round-trip error on realistic crypto quantities and well below
// any fillable order size.
const hedgeQtyEpsilon = 1e-9

// hedgeMinOrderNotionalUSD is the notional floor below which a hedge REDUCE is
// deferred rather than submitted. Hyperliquid rejects orders under ~$10
// notional; spamming an unfillable reduce every cycle would burn subprocess
// budget and generate a failure alert per cycle for a position that is
// materially already hedged. The basis is deliberately NOT advanced on a
// deferral, so the shortfall accumulates and clears itself as soon as a later
// primary reduction pushes the combined delta over the floor.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeActionKind enumerates the convergence actions the reconciler can take.
type hedgeActionKind int

const (
	// hedgeActionNone: the hedge already matches the target (or there is
	// nothing to hedge). No order is placed.
	hedgeActionNone hedgeActionKind = iota
	// hedgeActionOpen: primary is held, hedge is flat — open the full hedge.
	hedgeActionOpen
	// hedgeActionAdd: primary grew past the watermark — add the delta.
	hedgeActionAdd
	// hedgeActionReduce: primary shrank below the watermark — reduce the
	// hedge proportionally.
	hedgeActionReduce
	// hedgeActionCloseFull: primary is flat (or the hedge is on the wrong
	// side) — flatten the hedge.
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

// isOrder reports whether the action requires submitting an exchange order.
func (k hedgeActionKind) isOrder() bool {
	return k != hedgeActionNone
}

// hedgeSnapshot is the immutable view of primary + hedge state captured under
// the Phase-1 read lock. Every decision and every pre-spawn skip-reason check
// reads this copy, never live state, so the lock-free subprocess phase can
// never observe a torn read.
type hedgeSnapshot struct {
	PrimarySymbol  string
	PrimaryQty     float64
	PrimarySide    string
	PrimaryAvgCost float64

	HedgeSymbol string
	HedgeQty    float64
	HedgeSide   string
	// HedgeBasis is Position.HedgePrimaryQtyBasis on the held hedge leg: the
	// primary quantity the hedge was last sized against. Zero when no hedge
	// leg is held.
	HedgeBasis float64
	// HedgeHeld distinguishes "no hedge leg" from "hedge leg with zero
	// quantity" (a corrupt row) — the latter must be flattened, not opened.
	HedgeHeld bool
}

// hedgeAction is the reconciler's decision for one cycle.
type hedgeAction struct {
	Kind hedgeActionKind
	// Qty is the hedge-coin quantity to trade. For open/add it is the size to
	// submit; for reduce it is the size to close; for closeFull it is the
	// whole held hedge quantity.
	Qty float64
	// Side is the exchange side of the ORDER ("buy"/"sell") for open/add.
	// Empty for reduce/close (reduce-only closes derive their own side).
	Side string
	// HedgeSide is the resulting position side ("long"/"short") for an open.
	HedgeSide string
	// NewBasis is the primary quantity the hedge will have been sized against
	// once this action fills completely. Applied proportionally to the actual
	// fill by applyHedgeFill.
	NewBasis float64
	// Reason is operator-facing context, always populated.
	Reason string
	// Blocked marks a decision the reconciler REFUSED to make because an
	// input was unusable (no mark price, unknown side). The caller must not
	// treat this as "nothing to do" — it escalates.
	Blocked bool
}

// hedgeTargetDecision is the pure decision core: given a strategy's hedge
// config, a state snapshot, and current marks, what should the hedge leg do?
//
// Sizing is notional-matched against the PRIMARY's quantity delta:
//
//	hedge_qty = (primary_qty_delta × primary_px × ratio) / hedge_px
//
// The delta (not the total) is what gets sized, and the delta is measured
// against the persisted watermark rather than against the hedge's own implied
// notional. That is deliberate: sizing off implied notional would re-trade the
// hedge every time either mark moved, converting a position-mirroring leg into
// an unwanted continuous rebalancer that pays taker fees on noise. Keying on
// the primary QUANTITY means only a real primary event moves the hedge.
//
// primaryPx should be the current primary mark; hedgePx the current hedge mark.
// Both must be positive — an unusable price fails CLOSED (Blocked) rather than
// falling back to AvgCost, because a stale basis would mis-size a live order.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) || snap.HedgeSymbol == "" {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge not enabled"}
	}

	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeHeld && snap.HedgeQty > hedgeQtyEpsilon

	// Primary flat: the hedge has nothing to hedge. Flatten it. This is the
	// single convergence point for EVERY primary exit path — evaluator close,
	// SL fill, TP flatten, orphan close, external close — none of which need
	// to know the hedge exists.
	if !primaryHeld {
		if snap.HedgeHeld {
			return hedgeAction{
				Kind:     hedgeActionCloseFull,
				Qty:      snap.HedgeQty,
				NewBasis: 0,
				Reason:   "primary flat — flattening hedge leg",
			}
		}
		return hedgeAction{Kind: hedgeActionNone, Reason: "primary flat, no hedge leg"}
	}

	wantSide := HedgeSideForPrimary(snap.PrimarySide)
	if wantSide == "" {
		// An unknown primary side must never be guessed into a directional
		// order — a wrong guess doubles exposure instead of hedging it.
		return hedgeAction{
			Kind:    hedgeActionNone,
			Blocked: true,
			Reason:  fmt.Sprintf("primary side %q is not long/short — refusing to derive a hedge side", snap.PrimarySide),
		}
	}

	// A hedge leg on the wrong side is not a hedge; it doubles down. This is
	// unreachable while direction="both" is rejected at load, but it is the
	// cheapest possible defense against a future path (or a hand-edited state
	// DB) producing one, so flatten rather than trust the invariant.
	if hedgeHeld && snap.HedgeSide != wantSide {
		return hedgeAction{
			Kind:     hedgeActionCloseFull,
			Qty:      snap.HedgeQty,
			NewBasis: 0,
			Reason:   fmt.Sprintf("hedge leg side %q opposes the required hedge side %q for a %s primary — flattening (this doubles exposure instead of hedging it)", snap.HedgeSide, wantSide, snap.PrimarySide),
		}
	}

	// A held row with a non-positive quantity is structurally corrupt (the
	// same class #1009 handles for primaries). Flatten it so booking never
	// computes PnL off it.
	if snap.HedgeHeld && !hedgeHeld {
		return hedgeAction{
			Kind:     hedgeActionCloseFull,
			Qty:      absQty(snap.HedgeQty),
			NewBasis: 0,
			Reason:   fmt.Sprintf("hedge leg has non-positive quantity %.8f — clearing corrupt leg", snap.HedgeQty),
		}
	}

	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{
			Kind:    hedgeActionNone,
			Blocked: true,
			Reason:  fmt.Sprintf("unusable marks (primary=%.6f hedge=%.6f) — refusing to size a hedge order", primaryPx, hedgePx),
		}
	}

	ratio := hedgeRatio(sc)

	// Hedge flat, primary held: open the full hedge against the primary's
	// entire current quantity.
	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge size is zero"}
		}
		return hedgeAction{
			Kind:      hedgeActionOpen,
			Qty:       qty,
			Side:      hedgeOrderSideForPositionSide(wantSide),
			HedgeSide: wantSide,
			NewBasis:  snap.PrimaryQty,
			Reason:    fmt.Sprintf("opening %s hedge %.8f %s against %s primary %.8f %s (ratio %.4g)", wantSide, qty, snap.HedgeSymbol, snap.PrimarySide, snap.PrimaryQty, snap.PrimarySymbol, ratio),
		}
	}

	// A held hedge with a zero/negative basis predates the watermark or was
	// corrupted. Re-anchor to the current primary quantity rather than
	// computing a delta against garbage — the alternative would size an
	// "add" for the entire primary on top of an already-hedged leg.
	if snap.HedgeBasis <= hedgeQtyEpsilon {
		return hedgeAction{
			Kind:     hedgeActionNone,
			NewBasis: snap.PrimaryQty,
			Reason:   fmt.Sprintf("hedge leg has no quantity basis — re-anchoring watermark to primary %.8f without trading", snap.PrimaryQty),
		}
	}

	delta := snap.PrimaryQty - snap.HedgeBasis

	if delta > hedgeQtyEpsilon {
		// Primary grew (scale-in add, or a reconcile-detected upsize). Hedge
		// the DELTA at the delta's own current notional — matching the
		// scale-in convention that each leg is sized at its own entry, not
		// re-based to a blended average.
		addQty := delta * primaryPx * ratio / hedgePx
		if addQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{
				Kind:   hedgeActionNone,
				Reason: fmt.Sprintf("hedge add of $%.2f is below the $%.2f minimum order notional — deferring (basis held at %.8f so the shortfall accumulates)", addQty*hedgePx, hedgeMinOrderNotionalUSD, snap.HedgeBasis),
			}
		}
		return hedgeAction{
			Kind:      hedgeActionAdd,
			Qty:       addQty,
			Side:      hedgeOrderSideForPositionSide(wantSide),
			HedgeSide: wantSide,
			NewBasis:  snap.PrimaryQty,
			Reason:    fmt.Sprintf("primary grew %.8f → %.8f — adding %.8f %s to the hedge", snap.HedgeBasis, snap.PrimaryQty, addQty, snap.HedgeSymbol),
		}
	}

	if delta < -hedgeQtyEpsilon {
		// Primary shrank (partial close, partial TP fill, partial external
		// close). Reduce the hedge by the SAME FRACTION the primary lost,
		// applied to the hedge's actual held quantity. Fraction-of-held (not
		// a re-derived notional) is what keeps the two legs proportional
		// across an arbitrary sequence of partials without accumulating
		// rounding drift, and it is immune to mark movement between the open
		// and the reduce.
		fraction := (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if fraction > 1 {
			fraction = 1
		}
		reduceQty := snap.HedgeQty * fraction
		if reduceQty > snap.HedgeQty {
			reduceQty = snap.HedgeQty
		}
		if reduceQty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge reduction is zero"}
		}
		// Reducing all-but-dust is a full close: leaving a sub-minimum
		// residue on-chain would be unclosable by any later reduce.
		if snap.HedgeQty-reduceQty <= hedgeQtyEpsilon || (snap.HedgeQty-reduceQty)*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{
				Kind:     hedgeActionCloseFull,
				Qty:      snap.HedgeQty,
				NewBasis: snap.PrimaryQty,
				Reason:   fmt.Sprintf("primary shrank %.8f → %.8f — the residual hedge would be below the $%.2f minimum, closing the leg in full", snap.HedgeBasis, snap.PrimaryQty, hedgeMinOrderNotionalUSD),
			}
		}
		if reduceQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{
				Kind:   hedgeActionNone,
				Reason: fmt.Sprintf("hedge reduction of $%.2f is below the $%.2f minimum order notional — deferring (basis held at %.8f so the shortfall accumulates)", reduceQty*hedgePx, hedgeMinOrderNotionalUSD, snap.HedgeBasis),
			}
		}
		return hedgeAction{
			Kind:     hedgeActionReduce,
			Qty:      reduceQty,
			NewBasis: snap.PrimaryQty,
			Reason:   fmt.Sprintf("primary shrank %.8f → %.8f — reducing hedge by %.8f %s (%.2f%%)", snap.HedgeBasis, snap.PrimaryQty, reduceQty, snap.HedgeSymbol, fraction*100),
		}
	}

	return hedgeAction{Kind: hedgeActionNone, Reason: "hedge in sync with primary"}
}

// hedgeOrderSideForPositionSide maps a desired POSITION side to the exchange
// ORDER side that establishes it.
func hedgeOrderSideForPositionSide(positionSide string) string {
	if positionSide == "short" {
		return "sell"
	}
	return "buy"
}

// hedgeOrderSkipReason re-checks a decision's preconditions against a snapshot
// taken immediately before the order is spawned, and returns a non-empty
// reason when the order MUST NOT be submitted.
//
// This is the repo's skip-reason mirror rule (see the {Perps,Spot,Futures}
// OrderSkipReason family): the conditions that authorized an order are
// re-evaluated at the spawn site, because an on-chain fill that lands without
// a matching Trade record is unrecoverable bookkeeping damage. Here the risk is
// concrete — between the Phase-1 snapshot and the spawn, the same cycle's
// execute path may have opened, closed, or resized the primary.
func hedgeOrderSkipReason(sc StrategyConfig, action hedgeAction, snap hedgeSnapshot) string {
	if !HedgeEnabled(sc) {
		return "hedge disabled"
	}
	if !action.Kind.isOrder() {
		return "no hedge order to place"
	}
	if action.Qty <= hedgeQtyEpsilon {
		return fmt.Sprintf("hedge order quantity %.10f is not positive", action.Qty)
	}
	if snap.HedgeSymbol == "" {
		return "hedge symbol unresolved"
	}
	switch action.Kind {
	case hedgeActionOpen:
		if snap.HedgeHeld {
			return "hedge leg already exists — refusing to open a second one"
		}
		if snap.PrimaryQty <= hedgeQtyEpsilon {
			return "primary position is flat — refusing to open a hedge"
		}
		if HedgeSideForPrimary(snap.PrimarySide) != action.HedgeSide {
			return fmt.Sprintf("primary side changed to %q — hedge side %q no longer correct", snap.PrimarySide, action.HedgeSide)
		}
	case hedgeActionAdd:
		if !snap.HedgeHeld {
			return "hedge leg vanished — an add would open an unsized leg"
		}
		if snap.PrimaryQty <= hedgeQtyEpsilon {
			return "primary position is flat — refusing to add to a hedge"
		}
		if HedgeSideForPrimary(snap.PrimarySide) != action.HedgeSide {
			return fmt.Sprintf("primary side changed to %q — hedge side %q no longer correct", snap.PrimarySide, action.HedgeSide)
		}
		if snap.PrimaryQty <= snap.HedgeBasis+hedgeQtyEpsilon {
			return "primary no longer exceeds the hedge basis — add is stale"
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if !snap.HedgeHeld {
			return "hedge leg already flat"
		}
	}
	return ""
}

// hedgeSnapshotFromState builds a hedgeSnapshot for a strategy. MUST be called
// under at least a read lock on the state mutex.
func hedgeSnapshotFromState(sc StrategyConfig, s *StrategyState) hedgeSnapshot {
	snap := hedgeSnapshot{
		PrimarySymbol: hyperliquidSymbol(sc.Args),
		HedgeSymbol:   hedgeCoin(sc),
	}
	if s == nil {
		return snap
	}
	if snap.PrimarySymbol != "" {
		if pos, ok := s.Positions[snap.PrimarySymbol]; ok && pos != nil {
			snap.PrimaryQty = pos.Quantity
			snap.PrimarySide = pos.Side
			snap.PrimaryAvgCost = pos.AvgCost
		}
	}
	if snap.HedgeSymbol != "" {
		if pos, ok := s.Positions[snap.HedgeSymbol]; ok && pos != nil {
			// Only a leg explicitly stamped as OUR hedge counts. A position on
			// the hedge coin that is not stamped is not adopted — ownership
			// comes from persisted metadata, never from coin→config inference
			// (issue constraint 5).
			if pos.HedgeFor == snap.PrimarySymbol {
				snap.HedgeHeld = true
				snap.HedgeQty = pos.Quantity
				snap.HedgeSide = pos.Side
				snap.HedgeBasis = pos.HedgePrimaryQtyBasis
			}
		}
	}
	return snap
}

// hedgeExecutor is the exchange-side interface the reconciler needs. Injected
// so every execution path is unit-testable without spawning Python.
type hedgeExecutor struct {
	// Open submits a market order that establishes/adds hedge exposure.
	Open func(sc StrategyConfig, coin, side string, qty float64, setMargin bool) (*HyperliquidExecuteResult, error)
	// Reduce submits a reduce-only close for qty (nil qty = full close).
	Reduce func(sc StrategyConfig, coin string, qty *float64) (*HyperliquidCloseResult, error)
	// UnwindPrimary submits a reduce-only close of the primary leg for qty,
	// cancelling the supplied resting protection OIDs first.
	UnwindPrimary func(sc StrategyConfig, coin string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error)
}

// defaultHedgeExecutor is the production executor: it shells out through the
// same side-effecting wrappers every other live HL order uses.
func defaultHedgeExecutor() hedgeExecutor {
	return hedgeExecutor{
		Open: func(sc StrategyConfig, coin, side string, qty float64, setMargin bool) (*HyperliquidExecuteResult, error) {
			marginMode := ""
			leverage := 0.0
			if setMargin {
				// HL rejects update_leverage on an open position, so the
				// hedge's own margin mode / leverage is asserted only on a
				// FRESH hedge open — the same rule the primary open follows.
				marginMode = hedgeMarginMode(sc)
				leverage = hedgeLeverage(sc)
			}
			// No stop-loss, no TP tiers, no cancel OIDs, no close-full flag:
			// the hedge carries no protection orders in phase 1.
			res, stderr, err := RunHyperliquidExecute(sc.Script, coin, side, qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
			if stderr != "" {
				fmt.Printf("[hedge] %s execute stderr: %s\n", coin, stderr)
			}
			return res, err
		},
		Reduce: func(sc StrategyConfig, coin string, qty *float64) (*HyperliquidCloseResult, error) {
			res, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, coin, qty, nil)
			if stderr != "" {
				fmt.Printf("[hedge] %s close stderr: %s\n", coin, stderr)
			}
			return res, err
		},
		UnwindPrimary: func(sc StrategyConfig, coin string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
			// SIZED close, never a full market_close: the primary coin may
			// have shared-coin peers whose exposure must not be liquidated by
			// this strategy's unwind.
			sz := qty
			res, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, coin, &sz, cancelOIDs)
			if stderr != "" {
				fmt.Printf("[hedge] %s unwind stderr: %s\n", coin, stderr)
			}
			return res, err
		},
	}
}

// hedgeSyncInputs carries everything runHedgeSync needs that isn't derivable
// from the strategy config and state.
type hedgeSyncInputs struct {
	// PrimaryPx / HedgePx are the current marks. HedgePx must come from the
	// perps mark snapshot (collectPerpsMarkSymbols includes hedge coins), not
	// from AvgCost.
	PrimaryPx float64
	HedgePx   float64
	// FreshExposureQty is the primary quantity THIS cycle added — a fresh open
	// from flat, or a scale-in add — that the hedge does not yet cover. It is
	// non-zero only on a cycle where the execute path confirmed an
	// opening-side fill on the primary.
	//
	// Only on such a cycle does a failed hedge escalate to unwinding the
	// primary (constraint 4): the operator asked for a hedged entry and did
	// not get one, so exactly the increment that arrived unhedged is undone.
	// On any later cycle the position is already aged and was hedged when it
	// opened; unwinding it would be a surprise liquidation of a running
	// trade, so the reconciler alerts and retries instead.
	//
	// Unwinding the INCREMENT rather than the whole position matters for
	// scale-in: when an add's hedge fails, the pre-add position is still
	// correctly hedged. Closing all of it would destroy a healthy hedged
	// trade and then strand an oversized hedge leg until the next cycle.
	FreshExposureQty float64
	// PrimaryCancelOIDs are the resting protection OIDs on the primary,
	// captured so a fail-closed FULL unwind cancels them with the close
	// rather than orphaning triggers against HL's per-account order cap. They
	// are deliberately NOT sent for a partial unwind — the position survives
	// and must keep its protection.
	PrimaryCancelOIDs []int64
	// Live distinguishes a live wallet from paper. Paper books the same
	// decisions at the mark with a modeled fee and places no orders.
	Live bool
}

// runHedgeSync converges a strategy's hedge leg to the target derived from its
// primary position. Safe to call on every HL perps cycle; a no-op for
// strategies without an enabled hedge.
//
// Lock discipline mirrors runHyperliquidProtectionSync and the repo's 6-phase
// loop: snapshot under RLock, spawn the subprocess with NO lock held, apply the
// confirmed fill under Lock.
//
// Returns the action actually taken (hedgeActionNone when nothing happened).
func runHedgeSync(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	exec hedgeExecutor,
	in hedgeSyncInputs,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) hedgeActionKind {
	if !HedgeEnabled(sc) || s == nil || mu == nil {
		return hedgeActionNone
	}

	mu.RLock()
	snap := hedgeSnapshotFromState(sc, s)
	mu.RUnlock()

	if snap.HedgeSymbol == "" || snap.PrimarySymbol == "" {
		return hedgeActionNone
	}

	action := hedgeTargetDecision(sc, snap, in.PrimaryPx, in.HedgePx)
	if action.Blocked {
		// A blocked decision means an input the reconciler refuses to guess
		// at. On a fresh-open cycle that is a hedge failure (the primary is
		// running unhedged and we cannot even size the hedge), so it escalates
		// exactly like a rejected order.
		logger.Warn("hedge: %s", action.Reason)
		if in.FreshExposureQty > hedgeQtyEpsilon {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, exec, snap, action.Reason, in, notifier, logger)
			return hedgeActionNone
		}
		notifyHedgeProblem(notifier, sc, snap.HedgeSymbol,
			fmt.Sprintf("hedge sync could not evaluate: %s — the primary may be running under-hedged; retrying next cycle", action.Reason))
		return hedgeActionNone
	}

	// A no-trade decision may still need to persist a re-anchored watermark
	// (the "no basis" recovery path).
	if !action.Kind.isOrder() {
		if action.NewBasis > 0 {
			mu.Lock()
			if pos, ok := s.Positions[snap.HedgeSymbol]; ok && pos != nil && pos.HedgeFor == snap.PrimarySymbol {
				pos.HedgePrimaryQtyBasis = action.NewBasis
			}
			mu.Unlock()
			logger.Info("hedge: %s", action.Reason)
		} else if action.Reason != "" && action.Reason != "hedge in sync with primary" && action.Reason != "hedge not enabled" && action.Reason != "primary flat, no hedge leg" {
			logger.Info("hedge: %s", action.Reason)
		}
		return hedgeActionNone
	}

	// Re-check under a fresh snapshot immediately before spawning.
	mu.RLock()
	spawnSnap := hedgeSnapshotFromState(sc, s)
	mu.RUnlock()
	if reason := hedgeOrderSkipReason(sc, action, spawnSnap); reason != "" {
		logger.Info("hedge: skipping %s on %s — %s", action.Kind, snap.HedgeSymbol, reason)
		return hedgeActionNone
	}

	logger.Info("hedge: %s", action.Reason)

	if !in.Live {
		// Paper: book the same decision at the mark with a modeled fee. This
		// keeps the entire decision surface exercisable end-to-end without
		// live keys, which is the only practical way to regression-test the
		// lifecycle coupling.
		mu.Lock()
		applyHedgeFill(sc, s, snap.PrimarySymbol, action, action.Qty, in.HedgePx, 0, false, "", logger)
		mu.Unlock()
		return action.Kind
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		// setMargin only on a FRESH open: HL rejects update_leverage while a
		// position is open, so an add must not resend it.
		res, err := exec.Open(sc, snap.HedgeSymbol, action.Side, action.Qty, action.Kind == hedgeActionOpen)
		if ok, why := hedgeExecuteConfirmed(res, err); !ok {
			logger.Error("hedge %s failed on %s: %s", action.Kind, snap.HedgeSymbol, why)
			notifyLiveExecFailure(notifier, sc, directionOpen, snap.HedgeSymbol, why)
			if in.FreshExposureQty > hedgeQtyEpsilon {
				unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, exec, snap, why, in, notifier, logger)
			} else {
				notifyHedgeProblem(notifier, sc, snap.HedgeSymbol,
					fmt.Sprintf("hedge %s failed: %s — the primary %s position is now under-hedged. The scheduler will retry every cycle; no position was unwound because the primary was not opened this cycle.", action.Kind, why, snap.PrimarySymbol))
			}
			return hedgeActionNone
		}
		clearLiveExecThrottle(sc, directionOpen, snap.HedgeSymbol)
		fill := res.Execution.Fill
		mu.Lock()
		// Book the size the exchange ACTUALLY filled, never the size requested.
		applyHedgeFill(sc, s, snap.PrimarySymbol, action, fill.TotalSz, fill.AvgPx, fill.Fee, true, formatHedgeOID(fill.OID), logger)
		mu.Unlock()
		return action.Kind

	case hedgeActionReduce, hedgeActionCloseFull:
		// Always a SIZED reduce-only close, never a size-less market_close —
		// including for a full close. The hedge coin is sole-owned by
		// construction, but a sized close still cannot liquidate an on-chain
		// surplus the scheduler never booked, which a market_close would.
		sz := action.Qty
		res, err := exec.Reduce(sc, snap.HedgeSymbol, &sz)
		if ok, why := hedgeCloseConfirmed(res, err); !ok {
			logger.Error("hedge %s failed on %s: %s", action.Kind, snap.HedgeSymbol, why)
			notifyLiveExecFailure(notifier, sc, directionClose, snap.HedgeSymbol, why)
			notifyHedgeProblem(notifier, sc, snap.HedgeSymbol,
				fmt.Sprintf("hedge %s failed: %s — an oversized hedge leg remains against %s. Retrying next cycle.", action.Kind, why, snap.PrimarySymbol))
			return hedgeActionNone
		}
		clearLiveExecThrottle(sc, directionClose, snap.HedgeSymbol)
		if res.Close != nil && res.Close.AlreadyFlat {
			// Nothing on-chain to close. Clear the virtual leg so the next
			// cycle doesn't retry forever against a position that is gone.
			mu.Lock()
			clearHedgeLegAfterExternalFlat(s, snap.HedgeSymbol, in.HedgePx, logger)
			mu.Unlock()
			return hedgeActionCloseFull
		}
		fill := res.Close.Fill
		mu.Lock()
		// Book the size the exchange ACTUALLY filled, never the size requested.
		applyHedgeFill(sc, s, snap.PrimarySymbol, action, fill.TotalSz, fill.AvgPx, fill.Fee, true, formatHedgeOID(fill.OID), logger)
		mu.Unlock()
		return action.Kind
	}
	return hedgeActionNone
}

// hedgeExecuteConfirmed applies the repo's live-exec guard to a hedge open:
// virtual state may only change when the exchange confirmed a real fill.
func hedgeExecuteConfirmed(res *HyperliquidExecuteResult, err error) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if res == nil {
		return false, "no execute result returned"
	}
	if res.Error != "" {
		return false, res.Error
	}
	if res.Execution == nil || res.Execution.Fill == nil {
		return false, "execute returned no fill block"
	}
	if res.Execution.Fill.TotalSz <= 0 || res.Execution.Fill.AvgPx <= 0 {
		return false, fmt.Sprintf("execute returned an empty fill (sz=%.8f px=%.8f)", res.Execution.Fill.TotalSz, res.Execution.Fill.AvgPx)
	}
	return true, ""
}

// hedgeCloseConfirmed applies the same guard to a hedge reduce/close. An
// already_flat result is a confirmed no-op, not a failure.
func hedgeCloseConfirmed(res *HyperliquidCloseResult, err error) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	if res == nil {
		return false, "no close result returned"
	}
	if res.Error != "" {
		return false, res.Error
	}
	if res.Close == nil {
		return false, "close returned no close block"
	}
	if res.Close.AlreadyFlat {
		return true, ""
	}
	if res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 || res.Close.Fill.AvgPx <= 0 {
		return false, "close returned no fill"
	}
	return true, ""
}

func formatHedgeOID(oid int64) string {
	if oid <= 0 {
		return ""
	}
	return strconv.FormatInt(oid, 10)
}

// hedgeReducedBasis advances the quantity watermark for a hedge REDUCE (or a
// short-filled close) in proportion to what actually filled.
//
// The reduce path is the one place a partial fill can silently strand
// exposure. `bookPerpsPartialCloseWithFillFee` shrinks the leg by `filledQty`,
// but stamping the basis at the full post-reduce target would claim the leg is
// already sized for that target. `hedgeTargetDecision` would then compute
// `delta == 0` and report the pair in sync while it is still over-hedged —
// leaving a net directional bias past neutral for the remaining life of the
// position, with no event that ever revisits it (mark drift deliberately does
// not re-trade the hedge).
//
// Interpolating between the OLD basis and the target by the fill ratio keeps
// the residual delta proportional to the residual surplus, so the next cycle
// trims exactly what is left. It composes across consecutive partials: each
// call re-bases off the true current basis rather than the original target, so
// two half-fills converge instead of compounding an error.
//
// A non-positive old basis means the leg was never anchored (legacy or
// corrupted row); fall through to the target and let the "no basis" re-anchor
// path in hedgeTargetDecision take it from there.
func hedgeReducedBasis(oldBasis, targetBasis, filledQty, requestedQty float64) float64 {
	if oldBasis <= hedgeQtyEpsilon || requestedQty <= hedgeQtyEpsilon {
		return targetBasis
	}
	ratio := filledQty / requestedQty
	if ratio >= 1 {
		return targetBasis
	}
	if ratio <= 0 {
		return oldBasis
	}
	return oldBasis - (oldBasis-targetBasis)*ratio
}

// hedgeIsInverseOfPrimaryOnChain reports whether the on-chain position on
// hedgeCoin is opposite in direction to the one on primaryCoin.
//
// It is the discriminator the stuck-CB reconstruction uses when the virtual
// hedge leg has already been deleted and `Position.HedgeFor` is therefore
// unavailable. An auto-managed hedge is ALWAYS inverse to its primary
// (HedgeSideForPrimary), so a same-side position on the declared hedge coin
// provably was not opened by this mechanism and must never be liquidated as
// one. It does not prove the inverse case IS ours — no evidence surviving the
// delete can — but it rules out the whole class of same-side foreign
// positions, and the caller logs exactly what it closes either way.
//
// Returns false when either coin has no non-zero on-chain position, so a
// missing side can never be read as "inverse".
func hedgeIsInverseOfPrimaryOnChain(primaryCoin, hedgeCoin string, positions []HLPosition) bool {
	var primarySize, hedgeSize float64
	for i := range positions {
		switch positions[i].Coin {
		case primaryCoin:
			primarySize = positions[i].Size
		case hedgeCoin:
			hedgeSize = positions[i].Size
		}
	}
	if primarySize == 0 || hedgeSize == 0 {
		return false
	}
	return (primarySize > 0) != (hedgeSize > 0)
}

// hedgeBasisAfterPartialReduce re-anchors the quantity watermark from the
// hedge leg's own before/after sizes, for callers that know how much the leg
// actually shrank but not what size was originally requested.
//
// Hedge size is proportional to the basis at a fixed ratio and entry price, so
// the primary quantity the REMAINING leg corresponds to scales by the same
// fraction the leg did: `newBasis = oldBasis × remaining / preReduce`. This is
// the same answer hedgeReducedBasis produces from the order's fill ratio — it
// is simply derived from held quantities instead, which is what the manual
// pending-action drain and the reconciler have available.
//
// Deriving from held sizes also makes it composable: each call re-bases off the
// true current basis and size, so consecutive partial reduces converge rather
// than compounding an error.
//
// A fully drained leg returns 0 (nothing left to hedge with); a non-positive
// pre-reduce size or basis returns the old basis untouched, leaving the "no
// basis" re-anchor path in hedgeTargetDecision to handle it.
func hedgeBasisAfterPartialReduce(oldBasis, preReduceQty, remainingQty float64) float64 {
	if oldBasis <= hedgeQtyEpsilon || preReduceQty <= hedgeQtyEpsilon {
		return oldBasis
	}
	if remainingQty <= hedgeQtyEpsilon {
		return 0
	}
	if remainingQty >= preReduceQty {
		return oldBasis
	}
	return oldBasis * (remainingQty / preReduceQty)
}

// applyHedgeFill mutates virtual state for a CONFIRMED hedge fill. MUST be
// called under mu.Lock.
//
// fillPx/fillFee/useFillFee follow the repo convention: a live fill supplies
// the exchange's real price and fee; paper passes the mark with useFillFee
// false so the modeled-fee path runs.
//
// The basis advance is proportional to what ACTUALLY filled, never to what was
// requested — a partial fill that advanced the watermark to the requested size
// would permanently under-hedge the remainder with no path back.
func applyHedgeFill(sc StrategyConfig, s *StrategyState, primarySymbol string, action hedgeAction, filledQty, fillPx, fillFee float64, useFillFee bool, oid string, logger *StrategyLogger) {
	if s == nil || fillPx <= 0 || filledQty <= hedgeQtyEpsilon {
		return
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return
	}
	// Never book more than the exchange reported. A short fill that booked the
	// REQUESTED size would leave virtual state overstating the leg with no path
	// back — the reconciler would then read a hedge that does not exist.
	if action.Qty > 0 && filledQty > action.Qty {
		filledQty = action.Qty
	}
	detailsPrefix := fmt.Sprintf("hedge(%s)", primarySymbol)

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		fee := fillFee
		if !useFillFee {
			fee = CalculatePlatformSpotFee(s.Platform, filledQty*fillPx)
		}
		pos, exists := s.Positions[coin]
		// Advance the watermark PROPORTIONALLY to what actually filled. A
		// partial add that advanced the basis to the full requested target
		// would permanently under-hedge the unfilled remainder: the next cycle
		// would see basis == primary qty and conclude there is nothing to do.
		basis := action.NewBasis
		if action.Kind == hedgeActionAdd && action.Qty > 0 && exists && pos != nil && pos.HedgePrimaryQtyBasis > 0 {
			deltaBasis := action.NewBasis - pos.HedgePrimaryQtyBasis
			basis = pos.HedgePrimaryQtyBasis + deltaBasis*(filledQty/action.Qty)
		}
		if !exists || pos == nil {
			// A partial OPEN anchors the basis to the primary share it actually
			// covers, so the next cycle sizes the shortfall as an add.
			if action.Qty > 0 && filledQty < action.Qty {
				basis = action.NewBasis * (filledQty / action.Qty)
			}
			pos = &Position{
				Symbol:               coin,
				Quantity:             filledQty,
				InitialQuantity:      filledQty,
				AvgCost:              fillPx,
				Side:                 action.HedgeSide,
				Multiplier:           1, // canonical perps value — never leverage
				Leverage:             hedgeLeverage(sc),
				OwnerStrategyID:      s.ID,
				OpenedAt:             time.Now().UTC(),
				HedgeFor:             primarySymbol,
				HedgePrimaryQtyBasis: basis,
			}
			s.Positions[coin] = pos
		} else {
			// Blend the add into the existing leg (same math as a perps
			// scale-in: weighted average cost, summed quantity).
			totalQty := pos.Quantity + filledQty
			if totalQty > 0 {
				pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*filledQty) / totalQty
			}
			pos.Quantity = totalQty
			pos.HedgePrimaryQtyBasis = basis
			pos.HedgeFor = primarySymbol
		}
		// A hedge open transfers no realized PnL, so cash moves by the fee
		// alone — the same treatment a perps open gets.
		s.Cash -= fee
		positionID := ensurePositionTradeID(s.ID, coin, pos)
		trade := Trade{
			Timestamp:       time.Now().UTC(),
			StrategyID:      s.ID,
			Symbol:          coin,
			PositionID:      positionID,
			Side:            action.Side,
			Quantity:        filledQty,
			Price:           fillPx,
			Value:           filledQty * fillPx,
			TradeType:       hedgeTradeType,
			Details:         fmt.Sprintf("%s %s %s %.8f @ $%.4f (fee $%.4f)", detailsPrefix, action.Kind, coin, filledQty, fillPx, fee),
			ExchangeOrderID: oid,
			Regime:          s.Regime,
			// #954 gross convention: ExchangeFee always carries the fee that
			// was deducted from cash — real or modeled — and FeeSource says
			// which. tradeLedgerDeltaSQL / tradeNetPnLSQL deliberately ignore
			// trade_type, so this is exactly what books the hedge's fees into
			// the OWNING strategy's ledger (issue requirement 6).
			ExchangeFee: fee,
			FeeSource:   executionFeeSource(fillFee, useFillFee),
		}
		RecordTrade(s, trade)
		if logger != nil {
			logger.Info("hedge: booked %s %.8f %s @ $%.4f (basis %.8f)", action.Kind, filledQty, coin, fillPx, basis)
		}

	case hedgeActionReduce:
		pre := s.Positions[coin]
		if pre == nil {
			return
		}
		// Capture the pre-reduce basis BEFORE booking: the booker mutates (or
		// on a full drain, deletes) the position.
		reduceBasis := hedgeReducedBasis(pre.HedgePrimaryQtyBasis, action.NewBasis, filledQty, action.Qty)
		if bookPerpsPartialCloseWithFillFee(s, coin, filledQty, fillPx, fillFee, useFillFee, oid, hedgeReduceCloseReason, detailsPrefix+" reduce", "hedge", logger) {
			// bookPerpsPartialCloseWithFillFee deletes the position when it
			// fully drains; re-read before stamping the basis.
			if p, ok := s.Positions[coin]; ok && p != nil {
				p.HedgePrimaryQtyBasis = reduceBasis
			}
		}

	case hedgeActionCloseFull:
		pos := s.Positions[coin]
		if pos == nil {
			return
		}
		// A SHORT fill on a full close is a partial close, not a full one.
		// Booking it as full would mark the leg flat virtually while real
		// exposure still sits on the exchange — the reconciler would then have
		// nothing to converge and the residue would run unmanaged.
		if filledQty+hedgeQtyEpsilon < pos.Quantity {
			if logger != nil {
				logger.Warn("hedge: %s close filled only %.8f of %.8f — booking a partial close; the remainder is re-closed next cycle", coin, filledQty, pos.Quantity)
			}
			// Same proportional rule as a partial reduce: a short-filled close
			// must leave a basis that still describes the surplus, or the next
			// cycle sees delta==0 and abandons it.
			closeBasis := hedgeReducedBasis(pos.HedgePrimaryQtyBasis, action.NewBasis, filledQty, action.Qty)
			if bookPerpsPartialCloseWithFillFee(s, coin, filledQty, fillPx, fillFee, useFillFee, oid, hedgeReduceCloseReason, detailsPrefix+" close (partial fill)", "hedge", logger) {
				if p, ok := s.Positions[coin]; ok && p != nil {
					p.HedgePrimaryQtyBasis = closeBasis
				}
			}
			return
		}
		if bookPerpsCloseWithFillFee(s, coin, fillPx, fillFee, useFillFee, oid, hedgeCloseCloseReason, detailsPrefix+" close", "hedge", logger) && logger != nil {
			logger.Info("hedge: closed %s leg", coin)
		}
	}
}

// Trade type and close reasons for hedge legs. Kept as named constants because
// the trade_type value is load-bearing: tradeStatsExcludedTypesSQL keys on it
// to keep hedge round trips out of lifetime #T / W-L stats.
const (
	hedgeTradeType         = "hedge"
	hedgeReduceCloseReason = "hedge_reduce"
	hedgeCloseCloseReason  = "hedge_close"
	hedgeUnwindCloseReason = "hedge_open_failed_unwind"
)

// clearHedgeLegAfterExternalFlat books a zero-PnL clear of a hedge leg the
// exchange reports as already flat. MUST be called under mu.Lock.
func clearHedgeLegAfterExternalFlat(s *StrategyState, coin string, markPx float64, logger *StrategyLogger) {
	pos, ok := s.Positions[coin]
	if !ok || pos == nil {
		return
	}
	px := markPx
	if px <= 0 {
		px = pos.AvgCost
	}
	if logger != nil {
		logger.Warn("hedge: %s reported already-flat on-chain — clearing the virtual hedge leg at $%.4f", coin, px)
	}
	bookPerpsCloseWithFillFee(s, coin, px, 0, false, "", "hedge_already_flat", "hedge close (already flat on-chain)", "hedge", logger)
}

// unwindPrimaryAfterHedgeOpenFailure implements the fail-closed policy from
// issue constraint 4: when the primary fill confirmed but the hedge could not
// be opened on the SAME cycle, the unhedged increment is immediately closed
// reduce-only and the operator is alerted. The strategy never silently runs
// the naked directional exposure the operator explicitly asked to hedge.
//
// The unwind is scoped to in.FreshExposureQty — the quantity THIS cycle added.
// For a fresh open from flat that is the whole position; for a scale-in add it
// is only the add leg, because the pre-add position is still correctly hedged
// and closing it would destroy a healthy trade while stranding an oversized
// hedge leg. The close is always SIZED, never a full market_close: the primary
// coin may have shared-coin peers whose positions must not be liquidated here.
//
// Resting protection OIDs are cancelled only on a FULL unwind. A partial
// unwind leaves the position open, so its stop-loss must survive; the next
// protection sync re-sizes it to the reduced quantity.
//
// If the unwind itself fails there is no new latch state to maintain — the
// state-derived reconciler self-heals. Next cycle it sees primary-held +
// hedge-flat and retries the hedge open (which either succeeds, leaving a
// correctly hedged position, or fails again and, because that cycle is no
// longer a fresh open, alerts and retries rather than unwinding an aged
// position). Either outcome is visible to the operator and safe on restart.
func unwindPrimaryAfterHedgeOpenFailure(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	exec hedgeExecutor,
	snap hedgeSnapshot,
	hedgeErr string,
	in hedgeSyncInputs,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) {
	if s == nil || snap.PrimarySymbol == "" || snap.PrimaryQty <= hedgeQtyEpsilon {
		return
	}
	unwindQty := in.FreshExposureQty
	if unwindQty <= hedgeQtyEpsilon {
		return
	}
	if unwindQty > snap.PrimaryQty {
		unwindQty = snap.PrimaryQty
	}
	// "Full" means the unwind leaves nothing behind — the fresh open was the
	// entire position. Anything less is a partial that must preserve
	// protection.
	fullUnwind := snap.PrimaryQty-unwindQty <= hedgeQtyEpsilon

	scope := fmt.Sprintf("the %.8f increment opened this cycle", unwindQty)
	if fullUnwind {
		scope = fmt.Sprintf("the whole %.8f position opened this cycle", unwindQty)
	}
	logger.Error("hedge: FAIL-CLOSED — hedge open failed (%s); unwinding %s on %s %s (#1159 constraint 4)",
		hedgeErr, scope, snap.PrimarySide, snap.PrimarySymbol)

	details := fmt.Sprintf("hedge open failed (%s) — primary unwound", hedgeErr)

	bookUnwind := func(px, fee float64, useFillFee bool, oid string) {
		if px <= 0 {
			px = snap.PrimaryAvgCost
		}
		if fullUnwind {
			bookPerpsCloseWithFillFee(s, snap.PrimarySymbol, px, fee, useFillFee, oid, hedgeUnwindCloseReason, details, "hedge-unwind", logger)
			return
		}
		bookPerpsPartialCloseWithFillFee(s, snap.PrimarySymbol, unwindQty, px, fee, useFillFee, oid, hedgeUnwindCloseReason, details, "hedge-unwind", logger)
	}

	if !in.Live {
		mu.Lock()
		bookUnwind(in.PrimaryPx, 0, false, "")
		mu.Unlock()
		notifyHedgeCritical(notifier, sc, fmt.Sprintf(
			"**CRITICAL — hedge open failed, primary unwound (paper)**\nStrategy `%s`: the %s hedge leg could not be opened (%s), so %s on %s %s was closed immediately. No unhedged exposure was left running.",
			sc.ID, snap.HedgeSymbol, hedgeErr, scope, snap.PrimarySide, snap.PrimarySymbol))
		return
	}

	var cancelOIDs []int64
	if fullUnwind {
		cancelOIDs = in.PrimaryCancelOIDs
	}
	res, err := exec.UnwindPrimary(sc, snap.PrimarySymbol, unwindQty, cancelOIDs)
	if ok, why := hedgeCloseConfirmed(res, err); !ok {
		logger.Error("hedge: FAIL-CLOSED UNWIND FAILED for %s: %s — the primary is running UNHEDGED", snap.PrimarySymbol, why)
		notifyHedgeCritical(notifier, sc, fmt.Sprintf(
			"**CRITICAL — unhedged position running**\nStrategy `%s` opened %s %s but the %s hedge failed (%s) AND the fail-closed unwind of the primary also failed (%s).\n\nThe position is live and UNHEDGED. The scheduler will retry the hedge every cycle, but intervene manually if that does not clear.",
			sc.ID, snap.PrimarySide, snap.PrimarySymbol, snap.HedgeSymbol, hedgeErr, why))
		return
	}

	if res.Close != nil && res.Close.AlreadyFlat {
		logger.Warn("hedge: primary %s already flat on-chain during fail-closed unwind", snap.PrimarySymbol)
	}

	mu.Lock()
	px := in.PrimaryPx
	fee := 0.0
	useFillFee := false
	oid := ""
	if res.Close != nil && res.Close.Fill != nil {
		if res.Close.Fill.AvgPx > 0 {
			px = res.Close.Fill.AvgPx
		}
		// Book the quantity the exchange actually closed, not the quantity we
		// asked it to close — an assumed size would desync virtual state from
		// the wallet exactly when the operator is least able to notice.
		if res.Close.Fill.TotalSz > 0 && res.Close.Fill.TotalSz < unwindQty {
			unwindQty = res.Close.Fill.TotalSz
			fullUnwind = snap.PrimaryQty-unwindQty <= hedgeQtyEpsilon
		}
		fee = res.Close.Fill.Fee
		useFillFee = true
		oid = formatHedgeOID(res.Close.Fill.OID)
	}
	bookUnwind(px, fee, useFillFee, oid)
	mu.Unlock()

	notifyHedgeCritical(notifier, sc, fmt.Sprintf(
		"**CRITICAL — hedge open failed, primary unwound**\nStrategy `%s`: the %s hedge leg could not be opened (%s), so %s on %s %s was closed reduce-only on the same cycle. No unhedged exposure was left running.\n\nCheck the hedge coin's margin availability and order limits before the next signal.",
		sc.ID, snap.HedgeSymbol, hedgeErr, scope, snap.PrimarySide, snap.PrimarySymbol))
}

// reconcileHyperliquidHedgeLeg brings a strategy's virtual hedge leg back in
// line with the exchange. MUST be called under mu.Lock (the caller
// reconcileHyperliquidAccountPositions holds it).
//
// It deliberately reuses reconcileHyperliquidPositionsWithResolver rather than
// re-implementing drift handling, which buys three safety properties for free:
// quantity/side resync, external-close booking at the userFills VWAP
// (hl_sync_external, so the close never vanishes from the ledger), and — the
// important one — NON-ADOPTION of a foreign on-chain position. The resolver's
// "on-chain exists but not in state → skip" branch is what stops the scheduler
// from claiming someone else's position on a coin this strategy merely
// declares.
//
// pendingOrphanCloses is passed as nil on purpose: the #822 regime/direction
// orphan check compares a position's side against the strategy's effective
// direction, and a hedge leg is by definition the opposite side. Feeding it
// through would queue an auto-close of a perfectly healthy hedge on every
// cycle.
//
// Returns true when virtual state changed.
func reconcileHyperliquidHedgeLeg(
	sc StrategyConfig,
	ss *StrategyState,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
	pendingHedgeAlerts *[]string,
) bool {
	coin := hedgeCoin(sc)
	if coin == "" || ss == nil {
		return false
	}
	primary := hyperliquidSymbol(sc.Args)

	before, hadLeg := ss.Positions[coin]
	if hadLeg && before != nil && !before.isHedgeLeg() {
		// A position on the hedge coin that is not stamped as our hedge is not
		// ours to manage. Ownership comes from persisted metadata only
		// (issue constraint 5) — never from "this coin matches the config".
		if pendingHedgeAlerts != nil {
			*pendingHedgeAlerts = append(*pendingHedgeAlerts, fmt.Sprintf(
				"⚠️ **Hedge coin conflict** — `%s`\nThe virtual position on %s is not stamped as a hedge leg (hedge_for is empty), so hedge sync will not manage it. Resolve it manually before relying on the hedge.",
				sc.ID, coin))
		}
		return false
	}
	hadQty := 0.0
	hadBasis := 0.0
	if hadLeg && before != nil {
		hadQty = before.Quantity
		hadBasis = before.HedgePrimaryQtyBasis
	}

	changed := reconcileHyperliquidPositionsWithResolver(ss, coin, positions, resolveFee, logger, pendingAlerts, nil, sc)

	after, stillHeld := ss.Positions[coin]
	switch {
	case hadLeg && !stillHeld:
		// The exchange no longer shows the leg — liquidated, manually closed,
		// or filled by something outside the scheduler. The close has been
		// booked; hedge sync re-opens next cycle because the primary is still
		// held. Tell the operator: silently re-opening a leg someone closed on
		// purpose would look like the scheduler fighting them.
		if pendingHedgeAlerts != nil {
			*pendingHedgeAlerts = append(*pendingHedgeAlerts, fmt.Sprintf(
				"⚠️ **Hedge leg closed externally** — `%s` / %s\nThe %s hedge leg (%.8f, hedging %s) is gone from the exchange and has been booked as an external close. Because the primary is still open, hedge sync will RE-OPEN the hedge on the next cycle. Disable the hedge block if that is not what you want.",
				sc.ID, coin, coin, hadQty, primary))
		}
	case hadLeg && stillHeld && after != nil:
		if after.HedgePrimaryQtyBasis == 0 && hadBasis > 0 {
			after.HedgePrimaryQtyBasis = hadBasis
		}
		// #1159 (review round 2): a DOWNWARD resync means the exchange took
		// hedge size away from us — a partial liquidation, or an operator
		// closing part of the leg by hand. Shrink the basis by the same
		// fraction the leg shrank, so hedgeTargetDecision sees a real delta
		// and RE-GROWS the hedge back to the primary.
		//
		// Without this the shortfall is invisible: the decision core keys on
		// `PrimaryQty - HedgeBasis`, never on the hedge's own size, so an
		// unchanged primary yields delta==0 and the pair silently runs
		// under-hedged for the rest of the position's life. That was also
		// incoherent with the FULL external-close path, which already
		// re-opens the leg from scratch on the next cycle — closing 100% of
		// the hedge got it rebuilt while closing 99% did not.
		//
		// Re-growing via the basis (rather than comparing the held size to a
		// target recomputed at the current mark) is what keeps the qty-event
		// invariant intact: the basis moves only when the exchange actually
		// takes size, never when a price moves, so this cannot degrade the
		// hedge into a continuous rebalancer paying taker fees on noise.
		//
		// Deliberately one-directional. An UPWARD resync means unowned size
		// appeared on our coin; trading it away would act on a position this
		// scheduler never opened, so the basis is left alone and the operator
		// gets the alert below.
		if after.Quantity+1e-9 < hadQty {
			after.HedgePrimaryQtyBasis = hedgeBasisAfterPartialReduce(after.HedgePrimaryQtyBasis, hadQty, after.Quantity)
		}
		if math.Abs(after.Quantity-hadQty) > 1e-9 && pendingHedgeAlerts != nil {
			detail := "The leg has been resynced to the exchange and the quantity watermark shrank with it, so hedge sync will RE-GROW the hedge back to the primary on the next cycle. Disable the hedge block if you closed part of this leg deliberately."
			if after.Quantity > hadQty {
				detail = "On-chain size EXCEEDS the scheduler's record. The surplus was not opened by this scheduler, so it is left alone — hedge sync will not trade it away. Reconcile it manually."
			}
			*pendingHedgeAlerts = append(*pendingHedgeAlerts, fmt.Sprintf(
				"⚠️ **Hedge leg resynced** — `%s` / %s\nOn-chain quantity differed from the scheduler's record (%.8f → %.8f). %s",
				sc.ID, coin, hadQty, after.Quantity, detail))
		}
	case !hadLeg:
		// No virtual leg. If something IS on-chain for this coin, the
		// reconciler correctly refused to adopt it — surface that rather than
		// leaving an unexplained position sitting on a declared hedge coin.
		for i := range positions {
			if positions[i].Coin == coin && positions[i].Size != 0 {
				if pendingHedgeAlerts != nil {
					*pendingHedgeAlerts = append(*pendingHedgeAlerts, fmt.Sprintf(
						"⚠️ **Foreign position on hedge coin** — `%s` / %s\nThe exchange shows %.8f %s but the scheduler holds no hedge leg for it, so it was NOT adopted. Hedge sync will open its own leg on top of it when the primary opens, which will net against this position on-chain. Close or reassign the foreign position.",
						sc.ID, coin, positions[i].Size, coin))
				}
				break
			}
		}
	}
	return changed
}

// perpsPositionTradeType labels a perps trade row by the position it belongs
// to. A #1159 hedge leg must carry the "hedge" trade_type on EVERY leg — open,
// reduce, close, force-close, kill-switch, circuit-breaker — because
// tradeStatsExcludedTypesSQL filters lifetime #T / W-L on both the open-count
// AND the close-side round-trip aggregation. Labelling only the open would
// leave the hedge's mirror-image close counting as a win or loss, which is the
// exact distortion the exclusion exists to prevent.
//
// The label is display/stats only: tradeLedgerDeltaSQL and tradeNetPnLSQL
// deliberately ignore trade_type, so the hedge's PnL and fees still book to the
// owning strategy's ledger either way (issue requirement 6).
func perpsPositionTradeType(pos *Position) string {
	if pos.isHedgeLeg() {
		return hedgeTradeType
	}
	return "perps"
}

// manualCloseTradeType is the manual/operator-close spelling of
// perpsPositionTradeType, kept as a named seam for the pending-action drain.
func manualCloseTradeType(pos *Position) string {
	return perpsPositionTradeType(pos)
}

// hedgeUnwindCancelOIDs snapshots the resting protection OIDs on a primary
// position so a fail-closed FULL unwind cancels them with the close instead of
// leaving orphaned triggers burning Hyperliquid's per-account order cap.
// Returns nil when no position or no resting orders exist.
func hedgeUnwindCancelOIDs(s *StrategyState, mu *sync.RWMutex, primarySymbol string) []int64 {
	if s == nil || mu == nil || primarySymbol == "" {
		return nil
	}
	mu.RLock()
	defer mu.RUnlock()
	pos, ok := s.Positions[primarySymbol]
	if !ok || pos == nil {
		return nil
	}
	return hyperliquidProtectionCancelOIDs(pos)
}

// notifyHedgeProblem sends a non-fatal hedge alert to the owner. All sends
// happen outside mu (#880 convention) — every caller here is already unlocked.
func notifyHedgeProblem(notifier *MultiNotifier, sc StrategyConfig, coin, msg string) {
	if notifier == nil {
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("⚠️ **Hedge leg issue** — `%s` / %s\n%s", sc.ID, coin, msg))
}

// notifyHedgeCritical escalates a fail-closed hedge event to both the owner DM
// and every configured channel: an unhedged or unwound live position is a
// system-level event, not a per-strategy footnote.
func notifyHedgeCritical(notifier *MultiNotifier, sc StrategyConfig, msg string) {
	if notifier == nil {
		return
	}
	notifier.SendOwnerDM(msg)
	notifier.SendToAllChannels(msg)
}

// hedgeCoinsForStrategies returns the sorted set of hedge coins declared by
// enabled hedge blocks. Used to make hedge coins visible to the mark fetcher
// and the fill resolver, both of which otherwise only see configured primaries.
func hedgeCoinsForStrategies(strategies []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range strategies {
		if coin := hedgeCoin(sc); coin != "" {
			set[coin] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// heldHedgeCoin returns the hedge coin for a strategy ONLY when a virtual
// hedge leg is currently held for it, else "".
//
// The distinction is load-bearing for the kill switch and the circuit breaker:
// acting on a merely CONFIGURED hedge coin would let those paths liquidate a
// genuinely foreign on-chain position that happens to sit on a coin this
// strategy declares but does not currently hedge with. Acting on a HELD leg
// only keeps the "never touch what we didn't open" invariant intact.
//
// MUST be called under at least a read lock.
func heldHedgeCoin(sc StrategyConfig, s *StrategyState) string {
	coin := hedgeCoin(sc)
	if coin == "" || s == nil {
		return ""
	}
	pos, ok := s.Positions[coin]
	if !ok || pos == nil || !pos.isHedgeLeg() || pos.Quantity <= hedgeQtyEpsilon {
		return ""
	}
	return coin
}

// validateHedgeStateConsistency reports persisted hedge legs that no longer
// match the loaded config. Called once at startup next to
// ValidatePerpsDirectionConfig.
//
// The SIGHUP hot-reload guard blocks a hedge-block change while a hedge leg is
// open, but a config edit followed by a process RESTART bypasses it entirely —
// there is no "previous" config to diff against at boot. Without this check a
// live on-chain hedge leg could be silently orphaned (hedge disabled) or
// silently re-pointed (hedge symbol changed), in both cases leaving real
// exposure that nothing manages.
//
// Non-destructive by design: it warns and leaves the position frozen, matching
// the shared-coin ambiguity convention. Guessing a close here would liquidate
// real money on the strength of a config diff.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var warnings []string
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil {
			continue
		}
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || !pos.isHedgeLeg() || pos.Quantity <= hedgeQtyEpsilon {
				continue
			}
			sc, known := byID[id]
			switch {
			case !known:
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg %s %.8f %s (hedging %s) but the strategy is no longer in the config. The leg is frozen — nothing will manage or close it. Close it manually or restore the strategy.", id, pos.Side, pos.Quantity, sym, pos.HedgeFor))
			case !HedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg %s %.8f %s (hedging %s) but its hedge block is now absent/disabled. The leg is frozen — hedge sync will not manage or close it. Re-enable the hedge block or close the leg manually before the next signal.", id, pos.Side, pos.Quantity, sym, pos.HedgeFor))
			case hedgeCoin(sc) != sym:
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg on %s but its config now declares hedge.symbol=%s. The %s leg is frozen — hedge sync manages only the configured coin. Close the stale leg manually or revert hedge.symbol.", id, sym, hedgeCoin(sc), sym))
			case pos.HedgeFor != hyperliquidSymbol(sc.Args):
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds a hedge leg on %s stamped for primary %s, but the strategy now trades %s. The leg is frozen — close it manually.", id, sym, pos.HedgeFor, hyperliquidSymbol(sc.Args)))
			default:
				continue
			}
			fmt.Printf("[WARN] %s\n", warnings[len(warnings)-1])
		}
	}
	return warnings
}

// hedgeStatusLine renders a one-line operator summary of a strategy's hedge
// configuration and current leg. Shared by the startup summary, inspect, and
// the Discord/HTTP status surfaces so every operator view describes the hedge
// identically. Returns "" when no hedge is configured.
//
// MUST be called under at least a read lock when s is non-nil.
func hedgeStatusLine(sc StrategyConfig, s *StrategyState) string {
	if !HedgeEnabled(sc) {
		return ""
	}
	coin := hedgeCoin(sc)
	base := fmt.Sprintf("hedge=%s×%.4g(inverse,%s,%gx)", coin, hedgeRatio(sc), hedgeMarginMode(sc), hedgeLeverage(sc))
	if s == nil {
		return base
	}
	pos, ok := s.Positions[coin]
	if !ok || pos == nil || !pos.isHedgeLeg() || pos.Quantity <= hedgeQtyEpsilon {
		return base + " flat"
	}
	return fmt.Sprintf("%s %s %.8g @ $%.4f (coupled to %s, basis %.8g)", base, pos.Side, pos.Quantity, pos.AvgCost, pos.HedgeFor, pos.HedgePrimaryQtyBasis)
}

// HedgeStatus is the operator-facing view of a strategy's #1159 hedge: what is
// configured and what is currently held. Shared by the HTTP status API, the
// dashboard, and inspect --json so every surface describes the hedge the same
// way and a hedge leg is never mistaken for an unmanaged position.
type HedgeStatus struct {
	Enabled    bool    `json:"enabled"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`                 // phase 1: always "inverse"
	Ratio      float64 `json:"ratio"`                // resolved (0/unset → 1.0)
	MarginMode string  `json:"margin_mode"`          // resolved (empty → "isolated")
	Leverage   float64 `json:"leverage"`             // resolved (0/unset → 1)
	CoupledTo  string  `json:"coupled_to,omitempty"` // the primary symbol this leg mirrors
	Held       bool    `json:"held"`                 // a hedge leg is currently open
	Quantity   float64 `json:"quantity,omitempty"`   // held leg size
	PosSide    string  `json:"pos_side,omitempty"`   // "long"/"short" of the held leg
	AvgCost    float64 `json:"avg_cost,omitempty"`   // held leg entry
	QtyBasis   float64 `json:"qty_basis,omitempty"`  // primary quantity the leg was last sized against
}

// buildHedgeStatus renders a strategy's hedge view, or nil when no hedge block
// is configured. MUST be called under at least a read lock when s is non-nil.
func buildHedgeStatus(sc StrategyConfig, s *StrategyState) *HedgeStatus {
	if sc.Hedge == nil {
		return nil
	}
	out := &HedgeStatus{
		Enabled:    sc.Hedge.Enabled,
		Symbol:     normalizeHedgeCoin(sc.Hedge.Symbol),
		Side:       "inverse",
		Ratio:      hedgeRatio(sc),
		MarginMode: hedgeMarginMode(sc),
		Leverage:   hedgeLeverage(sc),
	}
	if s == nil || out.Symbol == "" {
		return out
	}
	pos, ok := s.Positions[out.Symbol]
	if !ok || pos == nil || !pos.isHedgeLeg() || pos.Quantity <= hedgeQtyEpsilon {
		return out
	}
	out.Held = true
	out.Quantity = pos.Quantity
	out.PosSide = pos.Side
	out.AvgCost = pos.AvgCost
	out.QtyBasis = pos.HedgePrimaryQtyBasis
	out.CoupledTo = pos.HedgeFor
	return out
}

// hedgeConfigEqual reports whether two hedge blocks are identical for
// hot-reload purposes. Nil and non-nil are distinct (adding or removing the
// block is a change), matching scaleInConfigEqual's pointer semantics.
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
