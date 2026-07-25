package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// hedge.go — correlated hedge legs (#1159, phases C+D): the pure decision
// core that computes the hedge target from the current primary position vs.
// the persisted qty watermark, and the execution/booking helpers that
// converge the hedge leg to it. Main-loop wiring is phase E; this file is
// deliberately free of any main.go dependency so every branch is unit-tested
// without spawning Python.
//
// Invariants enforced here:
//   - Qty-event mirroring, not price mirroring: decisions diff
//     Position.HedgePrimaryQtyBasis against the live primary qty, so
//     mark-price drift never re-trades the hedge.
//   - Fill-confirmed state mutation only: applyHedgeFill is the single
//     mutation choke point and is only ever called with a confirmed fill
//     (live) or a synthesized paper fill — mirroring the run*ExecuteOrder
//     ok2=false → no-state-update contract.
//   - Sole ownership by construction (phase-A collision matrix): hedge
//     closes are sized reduce-only via RunHyperliquidClose and never carry
//     cancel OIDs (the hedge leg has no SL/TP of its own).

// hedgeTradeType labels hedge open/close legs in the trades table (#1159) —
// the marker the LifetimeTradeStats* queries use to keep coupled
// risk-management legs out of #T and W/L (phase B).
const hedgeTradeType = "hedge"

// hedgeMinOrderNotionalUSD approximates Hyperliquid's minimum order notional.
// A reduce/add below this would be rejected by the exchange, so the decision
// defers it (basis intentionally NOT advanced) until the accumulated delta
// clears the floor. closeFull is exempt: flattening always executes.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeQtyEpsilon is the float guard for qty comparisons (plan: ε at the
// 1e-9 scale). Quantities below it are treated as flat / unchanged.
const hedgeQtyEpsilon = 1e-9

// bookTradeTypeForPosition returns the trades.trade_type label for a booked
// leg: hedge legs are "hedge" regardless of which close-booking helper ran,
// so the stats exclusion can't be bypassed by a close path that forgot to
// relabel.
func bookTradeTypeForPosition(pos *Position, defaultType string) string {
	if pos != nil && pos.HedgeFor != "" {
		return hedgeTradeType
	}
	return defaultType
}

// heldHedgeCoinsForKillSwitch returns the set of hedge coins the scheduler
// currently holds a virtual leg for, across the live HL roster (#1159 phase
// H). The portfolio kill switch extends its close roster with exactly this
// set: gating on the HELD leg (persisted HedgeFor stamp + positive qty),
// never the config block alone, means a genuinely foreign position on a
// declared-but-flat hedge coin is left for the operator instead of being
// liquidated. Call under the state lock.
func heldHedgeCoinsForKillSwitch(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig) map[string]bool {
	var out map[string]bool
	for _, sc := range hlLiveAll {
		hc := hedgeCoin(sc)
		if hc == "" || !HedgeEnabled(sc) {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		if pos := ss.Positions[hc]; pos != nil && pos.HedgeFor != "" && pos.Quantity > 0 {
			if out == nil {
				out = make(map[string]bool)
			}
			out[hc] = true
		}
	}
	return out
}

// HedgeStatus is the /status + dashboard serialization of a strategy's
// correlated hedge leg (#1159 phase J): the configured geometry plus the
// held leg's qty/side/basis/coupling when open. Position rows separately
// carry hedge_for (Phase B) so the UI can badge the leg itself.
type HedgeStatus struct {
	Symbol          string  `json:"symbol"`
	Side            string  `json:"side"`                        // configured orientation ("inverse" in phase 1)
	Ratio           float64 `json:"ratio"`                       // configured hedge-to-primary notional ratio
	HeldQty         float64 `json:"held_qty,omitempty"`          // open hedge leg quantity; 0 when flat
	HeldSide        string  `json:"held_side,omitempty"`         // open hedge leg side ("long"/"short")
	PrimaryQtyBasis float64 `json:"primary_qty_basis,omitempty"` // primary qty the held leg is hedged against
	HedgeFor        string  `json:"hedge_for,omitempty"`         // primary coin the held leg is coupled to
}

// hedgeStatusForStrategy builds the /status + dashboard serialization of a
// strategy's correlated hedge leg (#1159 phase J): the configured geometry,
// plus the held leg's qty/side/basis/coupling when open. Nil when the
// strategy has no enabled hedge block (or a blank symbol).
func hedgeStatusForStrategy(sc StrategyConfig, s *StrategyState) *HedgeStatus {
	if !HedgeEnabled(sc) {
		return nil
	}
	hc := hedgeCoin(sc)
	if hc == "" {
		return nil
	}
	out := &HedgeStatus{Symbol: hc, Side: hedgeSide(sc), Ratio: HedgeRatio(sc)}
	if s != nil {
		if pos := s.Positions[hc]; pos != nil && pos.HedgeFor != "" && pos.Quantity > 0 {
			out.HeldQty = pos.Quantity
			out.HeldSide = pos.Side
			out.PrimaryQtyBasis = pos.HedgePrimaryQtyBasis
			out.HedgeFor = pos.HedgeFor
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Phase C — pure decision core
// ---------------------------------------------------------------------------

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
		return "closeFull"
	default:
		return "none"
	}
}

// hedgeSnapshot is the primary/hedge state the decision core diffs, captured
// under the cycle's Phase-1 RLock (phase E wires the capture site).
type hedgeSnapshot struct {
	PrimaryQty     float64
	PrimaryAvgCost float64
	PrimarySide    string // "long" | "short" | "" when flat
	HedgeQty       float64
	HedgeAvgCost   float64
	HedgeBasis     float64 // Position.HedgePrimaryQtyBasis watermark
	HedgeSide      string  // "long" | "short" | "" when flat
}

// captureHedgeSnapshot extracts the snapshot from strategy state. Pure read;
// the caller holds the appropriate lock.
func captureHedgeSnapshot(s *StrategyState, sc StrategyConfig) hedgeSnapshot {
	var snap hedgeSnapshot
	if s == nil {
		return snap
	}
	if p := s.Positions[hyperliquidConfiguredCoin(sc)]; p != nil {
		snap.PrimaryQty = p.Quantity
		snap.PrimaryAvgCost = p.AvgCost
		snap.PrimarySide = p.Side
	}
	if h := s.Positions[hedgeCoin(sc)]; h != nil {
		snap.HedgeQty = h.Quantity
		snap.HedgeAvgCost = h.AvgCost
		snap.HedgeBasis = h.HedgePrimaryQtyBasis
		snap.HedgeSide = h.Side
	}
	return snap
}

// hedgeAction is one converging step toward the hedge target. Qty is the
// hedge-coin quantity for open/add/reduce; closeFull always flattens the
// whole leg (Qty ignored). Side is the order side ("buy"/"sell") for
// open/add. Reason is operator-facing context (and the escalation payload
// for fail-closed none decisions).
type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string
	Reason string
}

// hedgePositionSideForPrimarySide maps the live primary side to the inverse
// hedge-leg side ("" for an unusable primary side).
func hedgePositionSideForPrimarySide(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	}
	return ""
}

// hedgeOrderSideForPrimarySide maps the live primary side to the hedge
// opening ORDER side (inverse: long primary → sell/short the hedge coin).
func hedgeOrderSideForPrimarySide(primarySide string) string {
	switch hedgePositionSideForPrimarySide(primarySide) {
	case "short":
		return "sell"
	case "long":
		return "buy"
	}
	return ""
}

// hedgeTargetDecision computes the next hedge action from the snapshot. All
// sizing is notional: hedge notional delta = primary qty delta × primary
// price × ratio; hedge qty = notional / hedge mark. Fail-closed throughout:
// unusable prices or sides produce none + Reason (the caller escalates — a
// fresh-open cycle unwinds the primary, a manage cycle alerts + retries).
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge not enabled"}
	}
	primaryFlat := snap.PrimaryQty <= hedgeQtyEpsilon
	hedgeFlat := snap.HedgeQty <= hedgeQtyEpsilon

	switch {
	case primaryFlat && hedgeFlat:
		return hedgeAction{Kind: hedgeActionNone}
	case primaryFlat:
		return hedgeAction{Kind: hedgeActionCloseFull,
			Reason: "primary flat — flattening residual hedge leg"}
	}

	// Primary held from here on.
	wantHedgeSide := hedgePositionSideForPrimarySide(snap.PrimarySide)
	if wantHedgeSide == "" {
		return hedgeAction{Kind: hedgeActionNone,
			Reason: fmt.Sprintf("primary side %q unusable — refusing hedge order (fail-closed)", snap.PrimarySide)}
	}
	if !hedgeFlat && snap.HedgeSide != wantHedgeSide {
		// Defense-in-depth: direction="both" is rejected at load, so a flip
		// can't reach here via config; if state ever disagrees, flatten the
		// wrong-side leg and let the next cycle re-open on the right side.
		return hedgeAction{Kind: hedgeActionCloseFull,
			Reason: fmt.Sprintf("hedge side %q is not the inverse of primary side %q — flattening before re-open (alert)", snap.HedgeSide, snap.PrimarySide)}
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone,
			Reason: fmt.Sprintf("unusable price (primary=%g hedge=%g) — fail-closed, no hedge order", primaryPx, hedgePx)}
	}

	ratio := HedgeRatio(sc)
	if hedgeFlat {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone,
				Reason: "computed hedge qty ~0 — deferring open"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty,
			Side:   hedgeOrderSideForPrimarySide(snap.PrimarySide),
			Reason: "primary held, hedge flat — opening inverse hedge leg"}
	}

	delta := snap.PrimaryQty - snap.HedgeBasis
	switch {
	case delta > hedgeQtyEpsilon:
		qty := delta * primaryPx * ratio / hedgePx
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone,
				Reason: "computed hedge add qty ~0 — deferring"}
		}
		// Dust guard (mirrors the reduce floor): a sub-minimum add would be
		// rejected on-chain, so defer WITHOUT advancing the basis — the
		// delta accumulates until it clears the floor.
		if qty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone,
				Reason: fmt.Sprintf("hedge add notional $%.2f below min $%.2f — deferring (basis not advanced)", qty*hedgePx, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty,
			Side:   hedgeOrderSideForPrimarySide(snap.PrimarySide),
			Reason: fmt.Sprintf("primary grew %g over hedge basis — adding to hedge leg", delta)}
	case delta < -hedgeQtyEpsilon:
		frac := 1.0
		if snap.HedgeBasis > hedgeQtyEpsilon {
			frac = (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		}
		qty := snap.HedgeQty * frac
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		// A reduction whose REMAINDER would be untradeable dust flattens
		// instead, so no sub-minimum residue is left behind; closeFull is
		// exempt from the notional floor.
		if remaining := snap.HedgeQty - qty; remaining <= hedgeQtyEpsilon || remaining*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionCloseFull,
				Reason: "primary reduction leaves only dust on the hedge leg — flattening (no dust residue)"}
		}
		if qty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone,
				Reason: fmt.Sprintf("hedge reduce notional $%.2f below min $%.2f — deferring (basis not advanced)", qty*hedgePx, hedgeMinOrderNotionalUSD)}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty,
			Reason: fmt.Sprintf("primary shrank %g under hedge basis — reducing hedge leg proportionally", -delta)}
	default:
		return hedgeAction{Kind: hedgeActionNone}
	}
}

// hedgeOrderSkipReason re-checks the decision preconditions against a fresh
// snapshot immediately before spawning the order (the skip-reason mirror
// rule): an on-chain fill must never land without the state conditions that
// produced it still holding. "" means proceed.
func hedgeOrderSkipReason(sc StrategyConfig, action hedgeAction, snap hedgeSnapshot, hedgePx float64) string {
	if action.Kind == hedgeActionNone {
		return "no hedge action"
	}
	if !HedgeEnabled(sc) {
		return "hedge not enabled"
	}
	hedgeFlat := snap.HedgeQty <= hedgeQtyEpsilon
	switch action.Kind {
	case hedgeActionOpen:
		if !hedgeFlat {
			return "hedge leg already open — open would double the leg (wanted add)"
		}
		if action.Qty <= hedgeQtyEpsilon {
			return "zero/negative hedge qty"
		}
		if hedgePx <= 0 {
			return "no hedge mark price"
		}
		if action.Side != "buy" && action.Side != "sell" {
			return fmt.Sprintf("bad hedge order side %q", action.Side)
		}
	case hedgeActionAdd:
		if hedgeFlat {
			return "hedge leg flat — add without an existing leg (wanted open)"
		}
		if want := hedgePositionSideForPrimarySide(snap.PrimarySide); want != "" && snap.HedgeSide != want {
			return "hedge side drifted from inverse-of-primary"
		}
		if action.Qty <= hedgeQtyEpsilon {
			return "zero/negative hedge add qty"
		}
		if hedgePx <= 0 {
			return "no hedge mark price"
		}
	case hedgeActionReduce:
		if hedgeFlat {
			return "no hedge leg to reduce"
		}
		if action.Qty <= hedgeQtyEpsilon {
			return "zero/negative hedge reduce qty"
		}
	case hedgeActionCloseFull:
		if hedgeFlat {
			return "no hedge leg to close"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Phase D — execution and booking
// ---------------------------------------------------------------------------

// Injectable exec seams (repo testing rule: Go tests stub these, never spawn
// Python). Same pattern as runHyperliquidUpdateStopLossFunc et al.
var (
	runHyperliquidHedgeExecuteFn = RunHyperliquidExecute
	runHyperliquidHedgeCloseFn   = RunHyperliquidClose
	runHyperliquidUnwindCloseFn  = RunHyperliquidCloseCancelAfterFill
)

// hedgeOrderResult is the uniform confirmed-fill view over the execute
// (open/add) and close (reduce/closeFull/unwind) scripts.
type hedgeOrderResult struct {
	OK          bool
	FillPx      float64
	FillQty     float64
	FillFee     float64
	FillOID     int64
	AlreadyFlat bool // close only: exchange reported no position to close
	ErrMsg      string
}

// runHedgeOrder executes one hedge action live. Phase-3 (no lock) only —
// the caller snapshots under RLock, spawns here unlocked, and applies the
// result under Lock via applyHedgeFill. Failures route through
// notifyLiveExecFailure with a hedge-tagged direction label; successes clear
// the throttle.
func runHedgeOrder(sc StrategyConfig, action hedgeAction, snap hedgeSnapshot, notifier *MultiNotifier, logger *StrategyLogger) hedgeOrderResult {
	coin := hedgeCoin(sc)
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		label := "hedge-open"
		if action.Kind == hedgeActionAdd {
			label = "hedge-add"
		}
		// Margin mode + leverage are the hedge leg's OWN values (constraint:
		// never inherited from the primary) and are passed only when the
		// hedge leg is flat — HL rejects update_leverage on an open position
		// (mirror of the primary open path).
		marginMode, leverage := "", 0.0
		if snap.HedgeQty <= hedgeQtyEpsilon {
			marginMode, leverage = hedgeMarginMode(sc), hedgeLeverage(sc)
		}
		// No SL (0), no cancel OIDs, no TPs: the hedge leg carries no
		// protection of its own by design.
		res, _, err := runHyperliquidHedgeExecuteFn(sc.Script, coin, action.Side, action.Qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
		if err != nil {
			notifyLiveExecFailure(notifier, sc, label, coin, err.Error())
			return hedgeOrderResult{ErrMsg: err.Error()}
		}
		if res == nil || res.Execution == nil || res.Execution.Fill == nil || res.Execution.Fill.TotalSz <= 0 {
			msg := "hedge order submitted but no fill reported"
			notifyLiveExecFailure(notifier, sc, label, coin, msg)
			return hedgeOrderResult{ErrMsg: msg}
		}
		clearLiveExecThrottle(sc, label, coin)
		f := res.Execution.Fill
		return hedgeOrderResult{OK: true, FillPx: f.AvgPx, FillQty: f.TotalSz, FillFee: f.Fee, FillOID: f.OID}

	case hedgeActionReduce, hedgeActionCloseFull:
		label := "hedge-close"
		qty := action.Qty
		if action.Kind == hedgeActionCloseFull || qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		if qty <= hedgeQtyEpsilon {
			return hedgeOrderResult{ErrMsg: "nothing to close"}
		}
		// Sized reduce-only close; no cancel OIDs — the hedge leg has no
		// resting SL/TP triggers of its own.
		res, _, err := runHyperliquidHedgeCloseFn(sc.Script, coin, &qty, nil)
		if err != nil {
			notifyLiveExecFailure(notifier, sc, label, coin, err.Error())
			return hedgeOrderResult{ErrMsg: err.Error()}
		}
		if res == nil || res.Close == nil {
			msg := "hedge close returned no close block"
			notifyLiveExecFailure(notifier, sc, label, coin, msg)
			return hedgeOrderResult{ErrMsg: msg}
		}
		if res.Close.AlreadyFlat {
			clearLiveExecThrottle(sc, label, coin)
			return hedgeOrderResult{OK: true, AlreadyFlat: true}
		}
		if res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 {
			msg := "hedge close submitted but no fill reported"
			notifyLiveExecFailure(notifier, sc, label, coin, msg)
			return hedgeOrderResult{ErrMsg: msg}
		}
		clearLiveExecThrottle(sc, label, coin)
		f := res.Close.Fill
		return hedgeOrderResult{OK: true, FillPx: f.AvgPx, FillQty: f.TotalSz, FillFee: f.Fee, FillOID: f.OID}
	}
	return hedgeOrderResult{ErrMsg: "no hedge action"}
}

// paperHedgeOrderResult synthesizes the fill a paper hedge books at: the
// cycle's hedge mark with a modeled taker fee (the perps paper convention —
// the same parity norm as the primary leg booking at mid).
func paperHedgeOrderResult(action hedgeAction, snap hedgeSnapshot, hedgeMark float64) hedgeOrderResult {
	qty := action.Qty
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		// qty as computed by the decision
	case hedgeActionReduce:
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
	case hedgeActionCloseFull:
		qty = snap.HedgeQty
	default:
		return hedgeOrderResult{ErrMsg: "no hedge action"}
	}
	if qty <= hedgeQtyEpsilon || hedgeMark <= 0 {
		return hedgeOrderResult{ErrMsg: fmt.Sprintf("unusable paper hedge fill (qty=%g mark=%g)", qty, hedgeMark)}
	}
	return hedgeOrderResult{OK: true, FillPx: hedgeMark, FillQty: qty}
}

// hedgeStateMatchesSnapshot is the re-check-under-lock guard: state may have
// moved between the pre-spawn snapshot and the apply (an earlier same-cycle
// booking). A mismatch never drops a confirmed fill — applyHedgeFill books
// against whatever exists and warns.
func hedgeStateMatchesSnapshot(s *StrategyState, sc StrategyConfig, snap hedgeSnapshot) bool {
	if s == nil {
		return false
	}
	eq := func(a, b float64) bool {
		d := a - b
		return d <= hedgeQtyEpsilon && d >= -hedgeQtyEpsilon
	}
	var pQty, hQty, hBasis float64
	var pSide, hSide string
	if p := s.Positions[hyperliquidConfiguredCoin(sc)]; p != nil {
		pQty, pSide = p.Quantity, p.Side
	}
	if h := s.Positions[hedgeCoin(sc)]; h != nil {
		hQty, hSide, hBasis = h.Quantity, h.Side, h.HedgePrimaryQtyBasis
	}
	return eq(pQty, snap.PrimaryQty) && pSide == snap.PrimarySide &&
		eq(hQty, snap.HedgeQty) && hSide == snap.HedgeSide && eq(hBasis, snap.HedgeBasis)
}

// applyHedgeFill books a confirmed hedge order into virtual state. MUST be
// called under mu.Lock, and only with a confirmed fill (live) or a
// synthesized paper fill — hedge virtual state mutates exclusively here.
// paper=true books at the mark with a modeled fee; paper=false trusts the
// fill's real fee/OID.
func applyHedgeFill(s *StrategyState, sc StrategyConfig, action hedgeAction, snap hedgeSnapshot, res hedgeOrderResult, paper bool, logger *StrategyLogger) {
	if !res.OK && !res.AlreadyFlat {
		return
	}
	if !hedgeStateMatchesSnapshot(s, sc, snap) && logger != nil {
		logger.Warn("hedge state moved between snapshot and fill apply (%s %s) — booking the confirmed fill against current state", hedgeCoin(sc), action.Kind)
	}
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		applyHedgeOpenFill(s, sc, action, snap, res, paper, logger)
	case hedgeActionReduce, hedgeActionCloseFull:
		applyHedgeCloseFill(s, sc, action, res, paper, logger)
	}
}

// applyHedgeOpenFill creates or blends the hedge Position and records the
// open-side Trade (trade_type "hedge", Details prefixed hedge(<primary>)).
// Partial fills book the actual FillQty and advance the basis
// proportionally to what actually hedged — never the requested size.
func applyHedgeOpenFill(s *StrategyState, sc StrategyConfig, action hedgeAction, snap hedgeSnapshot, res hedgeOrderResult, paper bool, logger *StrategyLogger) {
	coin := hedgeCoin(sc)
	primarySym := hyperliquidConfiguredCoin(sc)
	fillQty, fillPx := res.FillQty, res.FillPx
	if fillQty <= hedgeQtyEpsilon || fillPx <= 0 {
		if logger != nil {
			logger.Warn("refusing to book hedge %s with unusable fill (qty=%g px=%g)", action.Kind, fillQty, fillPx)
		}
		return
	}
	notional := fillQty * fillPx
	useFillFee := !paper
	fee := executionFee(CalculatePlatformSpotFee("hyperliquid", notional), res.FillFee, useFillFee)
	s.Cash -= fee // margin-based: only the fee leaves cash, notional stays virtual

	// Basis advancement is proportional to the fill fraction:
	//   open: basis = PrimaryQty × filled/requested
	//   add:  basis += (PrimaryQty − basis) × filled/requested
	fillFrac := 1.0
	if action.Qty > 0 {
		fillFrac = fillQty / action.Qty
	}
	newBasis := snap.PrimaryQty * fillFrac
	if action.Kind == hedgeActionAdd {
		newBasis = snap.HedgeBasis + (snap.PrimaryQty-snap.HedgeBasis)*fillFrac
	}

	posSide := hedgePositionSideForPrimarySide(snap.PrimarySide)
	if posSide == "" { // re-check-mismatch fallback: derive from the order side
		if action.Side == "buy" {
			posSide = "long"
		} else {
			posSide = "short"
		}
	}

	now := time.Now().UTC()
	var positionID string
	if existing := s.Positions[coin]; existing != nil {
		if existing.HedgeFor == "" && logger != nil {
			logger.Warn("hedge coin %s already held WITHOUT hedge metadata (qty=%.6f) — adopting into the hedge leg for %s; verify manually", coin, existing.Quantity, primarySym)
		}
		if existing.HedgeFor != "" && existing.HedgeFor != primarySym && logger != nil {
			logger.Warn("hedge coin %s stamped hedge_for=%s but booking for primary %s — keeping the persisted stamp; verify manually", coin, existing.HedgeFor, primarySym)
		}
		totalQty := existing.Quantity + fillQty
		existing.AvgCost = (existing.Quantity*existing.AvgCost + notional) / totalQty
		existing.Quantity = totalQty
		existing.InitialQuantity += fillQty // mirror scale-in: Quantity < InitialQuantity stays the "partially closed" test
		if existing.HedgeFor == "" {
			existing.HedgeFor = primarySym
		}
		existing.HedgePrimaryQtyBasis = newBasis
		positionID = ensurePositionTradeID(s.ID, coin, existing)
	} else {
		positionID = newTradePositionID(s.ID, coin, now)
		s.Positions[coin] = &Position{
			Symbol:               coin,
			Quantity:             fillQty,
			InitialQuantity:      fillQty,
			AvgCost:              fillPx,
			Side:                 posSide,
			Multiplier:           1, // perps 1:1 contract size
			Leverage:             hedgeLeverage(sc),
			OwnerStrategyID:      sc.ID,
			OpenedAt:             now,
			TradePositionID:      positionID,
			HedgeFor:             primarySym,
			HedgePrimaryQtyBasis: newBasis,
		}
	}

	var oid string
	if useFillFee && res.FillOID != 0 {
		oid = strconv.FormatInt(res.FillOID, 10)
	}
	verb := "open"
	if action.Kind == hedgeActionAdd {
		verb = "add"
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          coin,
		PositionID:      positionID,
		Side:            action.Side,
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           notional,
		TradeType:       hedgeTradeType,
		Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.2f (fee $%.2f)", primarySym, verb, posSide, fillQty, fillPx, fee),
		ExchangeOrderID: oid,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(res.FillFee, useFillFee),
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("hedge(%s) %s %s: %.6f @ $%.2f (notional $%.2f, fee $%.2f, basis %.6f)", primarySym, verb, coin, fillQty, fillPx, notional, fee, newBasis)
	}
}

// applyHedgeCloseFill books a hedge reduce/flatten through the standard
// perps close-booking helpers (which carry the #954 dup-OID guard, the
// #1009 corrupt-position zero-PnL handling, and the phase-B hedge routing
// for risk stats). After a partial reduce the basis watermark scales down by
// the filled fraction of the leg.
func applyHedgeCloseFill(s *StrategyState, sc StrategyConfig, action hedgeAction, res hedgeOrderResult, paper bool, logger *StrategyLogger) {
	coin := hedgeCoin(sc)
	primarySym := hyperliquidConfiguredCoin(sc)
	pos := s.Positions[coin]
	if pos == nil {
		if logger != nil {
			logger.Warn("hedge close fill for %s but no virtual hedge leg — nothing to book (fill already on-chain)", coin)
		}
		return
	}
	if res.AlreadyFlat {
		// The exchange reports nothing to close but virtual state holds a
		// leg: someone/something closed it externally. Leave the virtual leg
		// untouched — the reconcile pass (phase G) books the external close
		// from userFills rather than guessing a price here.
		if logger != nil {
			logger.Warn("hedge close for %s reported already-flat on-chain while virtual qty=%.6f — leaving virtual leg for reconcile to resolve", coin, pos.Quantity)
		}
		return
	}
	useFillFee := !paper
	oid := ""
	if useFillFee && res.FillOID != 0 {
		oid = strconv.FormatInt(res.FillOID, 10)
	}
	if action.Kind == hedgeActionCloseFull || res.FillQty >= pos.Quantity-hedgeQtyEpsilon {
		bookPerpsCloseWithFillFee(s, coin, res.FillPx, res.FillFee, useFillFee, oid,
			"hedge_close", fmt.Sprintf("hedge(%s) close", primarySym), "Hedge close", logger)
		return
	}
	before := pos.Quantity
	if bookPerpsPartialCloseWithFillFee(s, coin, res.FillQty, res.FillPx, res.FillFee, useFillFee, oid,
		"hedge_reduce", fmt.Sprintf("hedge(%s) reduce", primarySym), "Hedge reduce", logger) {
		if p := s.Positions[coin]; p != nil && before > 0 {
			p.HedgePrimaryQtyBasis = p.HedgePrimaryQtyBasis * (1 - res.FillQty/before)
		}
	}
}

// ---------------------------------------------------------------------------
// Fail-closed primary unwind (hedge open failed on the fresh-open cycle)
// ---------------------------------------------------------------------------

// runPrimaryUnwindOrder submits the sized reduce-only unwind of a primary
// fill whose hedge leg failed to open. Phase-3 (no lock). Sized (never
// sz=None) because the primary coin may legitimately have shared-coin peers
// — only the hedge coin is guaranteed sole-owned. cancelOIDs are the just-
// armed SL/TP triggers (hyperliquidProtectionCancelOIDs(pos) captured under
// the caller's lock).
func runPrimaryUnwindOrder(sc StrategyConfig, primarySym string, unwindQty float64, cancelOIDs []int64) hedgeOrderResult {
	if unwindQty <= hedgeQtyEpsilon {
		return hedgeOrderResult{ErrMsg: "nothing to unwind"}
	}
	res, _, err := runHyperliquidUnwindCloseFn(sc.Script, primarySym, &unwindQty, cancelOIDs)
	if err != nil {
		return hedgeOrderResult{ErrMsg: err.Error()}
	}
	if res == nil || res.Close == nil {
		return hedgeOrderResult{ErrMsg: "unwind returned no close block"}
	}
	if res.Close.AlreadyFlat {
		return hedgeOrderResult{OK: true, AlreadyFlat: true}
	}
	if res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 {
		return hedgeOrderResult{ErrMsg: "unwind submitted but no fill reported"}
	}
	f := res.Close.Fill
	return hedgeOrderResult{OK: true, FillPx: f.AvgPx, FillQty: f.TotalSz, FillFee: f.Fee, FillOID: f.OID}
}

// applyPrimaryUnwindFill books the unwind close leg under mu.Lock with
// reason "hedge_open_failed_unwind". The unwind is a PRIMARY leg (not a
// hedge), so PnL flows through the normal risk accumulators.
func applyPrimaryUnwindFill(s *StrategyState, primarySym string, res hedgeOrderResult, logger *StrategyLogger) bool {
	pos := s.Positions[primarySym]
	if pos == nil {
		if logger != nil {
			logger.Warn("hedge-open-failure unwind fill for %s but no virtual primary position — nothing to book", primarySym)
		}
		return false
	}
	if res.AlreadyFlat {
		// Chain says the primary is already flat (e.g. liquidated in the
		// unwind window). Clear the virtual leg at AvgCost with zero PnL
		// rather than guessing a price — reconcile owns the real booking.
		return bookPerpsCloseWithFillFee(s, primarySym, pos.AvgCost, 0, false, "",
			"hedge_open_failed_unwind_already_flat", "hedge unwind (already flat on-chain)", "Hedge unwind", logger)
	}
	oid := ""
	if res.FillOID != 0 {
		oid = strconv.FormatInt(res.FillOID, 10)
	}
	if res.FillQty >= pos.Quantity-hedgeQtyEpsilon {
		return bookPerpsCloseWithFillFee(s, primarySym, res.FillPx, res.FillFee, true, oid,
			"hedge_open_failed_unwind", "hedge open failed — primary unwind", "Hedge unwind", logger)
	}
	return bookPerpsPartialCloseWithFillFee(s, primarySym, res.FillQty, res.FillPx, res.FillFee, true, oid,
		"hedge_open_failed_unwind", "hedge open failed — primary unwind", "Hedge unwind", logger)
}

// formatHedgeOpenFailureCritical is the owner-facing CRITICAL when a hedge
// open fails on the fresh-primary-open cycle (sent before the unwind).
func formatHedgeOpenFailureCritical(sc StrategyConfig, hedgeErr string) string {
	return fmt.Sprintf("CRITICAL: %s primary %s filled but the hedge open on %s FAILED (%s) — unwinding the primary fill now; the strategy is NOT hedged (#1159)",
		sc.ID, hyperliquidConfiguredCoin(sc), hedgeCoin(sc), hedgeErr)
}

// formatHedgeUnwindSuccessCritical is the owner-facing CRITICAL after the
// primary unwind was booked — the fail-closed path completed.
func formatHedgeUnwindSuccessCritical(sc StrategyConfig, unwindQty float64) string {
	return fmt.Sprintf("CRITICAL: %s unwound %.6g %s after the hedge open failed (booked as hedge_open_failed_unwind) — position flattened, investigate the hedge venue before the next signal (#1159)",
		sc.ID, unwindQty, hyperliquidConfiguredCoin(sc))
}

// formatHedgeUnwindFailureCritical is the owner-facing CRITICAL when the
// unwind itself failed: the primary stays open and unhedged, and the
// state-derived hedge sync retries every cycle (documented degraded loop).
func formatHedgeUnwindFailureCritical(sc StrategyConfig, unwindQty float64, unwindErr string) string {
	return fmt.Sprintf("CRITICAL: %s hedge-open-failure unwind of %.6g %s FAILED (%s) — the primary is OPEN AND UNHEDGED; hedge sync retries every cycle, close manually if it persists (#1159)",
		sc.ID, unwindQty, hyperliquidConfiguredCoin(sc), unwindErr)
}

// ---------------------------------------------------------------------------
// Phase E — per-cycle orchestrator (called from the HL perps dispatch tail)
// ---------------------------------------------------------------------------

// hedgeMarkForSyncFn resolves the hedge coin's mark for sizing. Seam for
// tests. The default prefers the cycle's prices map — phase F added hedge
// coins to collectPerpsMarkSymbols, so fetchHyperliquidMids covers them
// every cycle — and falls back to a one-shot fetch when the map lacks the
// coin (e.g. a caller that never fetched marks).
var hedgeMarkForSyncFn = hedgeMarkForSync

func hedgeMarkForSync(sc StrategyConfig, prices map[string]float64) float64 {
	coin := hedgeCoin(sc)
	if coin == "" {
		return 0
	}
	if prices != nil {
		if px := prices[coin]; px > 0 {
			return px
		}
	}
	return hedgeMarkFetchFn(coin)
}

// hedgeMarkFetchFn is the one-shot fetch fallback (seamed for tests — the
// real one hits the HL API).
var hedgeMarkFetchFn = func(coin string) float64 {
	mids, err := fetchHyperliquidMids([]string{coin})
	if err != nil {
		return 0
	}
	return mids[coin]
}

// sendHedgeCritical delivers a CRITICAL hedge alert to all channels + owner
// DM (mirroring the notifyLiveExecFailure fan-out). Nil-safe.
func sendHedgeCritical(notifier *MultiNotifier, msg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

// runHedgeSync converges the hedge leg to the primary's qty events once per
// HL perps dispatch cycle (#1159 phase E). Called from the dispatch tail with
// NO lock held; lock discipline mirrors runHyperliquidProtectionSync
// (snapshot under RLock → spawn unlocked → apply under Lock).
//
// Runs unconditionally on every successful-check cycle — fresh open,
// scale-in add, evaluator partial/full close, Signal==0 manage, paused
// (#1150), latched-CB manage-only (#1046), daily-loss hold (#1269): hedge
// orders are coupled risk-management legs, not signals, so none of the
// entry gates apply (a held primary can't increase, so under those states
// hedge sync can only reduce/close anyway).
//
// freshOpenQty is this cycle's confirmed primary-increasing fill qty (fresh
// open or scale-in add; 0 otherwise / paper). A hedge open/add failure on
// that cycle escalates to the fail-closed primary unwind; failures on any
// other cycle alert (inside runHedgeOrder) and retry next cycle.
//
// NOTE: the snapshot is captured HERE, after the primary apply block, not in
// Phase 1 — the decision MUST see the just-booked primary fill (a Phase-1
// snapshot would read the primary as flat on the open cycle).
//
// prices is the cycle's mark map (used for the hedge mid via
// hedgeMarkForSyncFn; a one-shot fetch covers callers that never fetched).
func runHedgeSync(sc StrategyConfig, stratState *StrategyState, primaryPx float64, prices map[string]float64, live bool, freshOpenQty float64, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	if !HedgeEnabled(sc) {
		return
	}
	mu.RLock()
	snap := captureHedgeSnapshot(stratState, sc)
	mu.RUnlock()

	hedgePx := hedgeMarkForSyncFn(sc, prices)
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	escalate := live && freshOpenQty > 0

	if action.Kind == hedgeActionNone {
		if action.Reason == "" {
			return
		}
		// Fail-closed decision (unusable price/side/dust). Escalate only
		// when the primary is held and the hedge never opened.
		if escalate && snap.PrimaryQty > hedgeQtyEpsilon && snap.HedgeQty <= hedgeQtyEpsilon {
			unwindPrimaryAfterHedgeOpenFailure(sc, stratState, freshOpenQty, action.Reason, mu, notifier, logger)
			return
		}
		if logger != nil {
			logger.Warn("hedge sync: no action for %s (%s) — will retry next cycle", hedgeCoin(sc), action.Reason)
		}
		return
	}
	if reason := hedgeOrderSkipReason(sc, action, snap, hedgePx); reason != "" {
		if escalate && (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) {
			unwindPrimaryAfterHedgeOpenFailure(sc, stratState, freshOpenQty, "pre-order guard: "+reason, mu, notifier, logger)
			return
		}
		if logger != nil {
			logger.Warn("hedge sync: %s %s skipped: %s", action.Kind, hedgeCoin(sc), reason)
		}
		return
	}

	var res hedgeOrderResult
	if live {
		res = runHedgeOrder(sc, action, snap, notifier, logger)
		if !res.OK {
			// Alerts already sent by runHedgeOrder. Fresh-open/add cycles
			// escalate to the primary unwind; every other cycle relies on
			// the state-derived retry (failed orders never mutate state).
			if escalate && (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) {
				unwindPrimaryAfterHedgeOpenFailure(sc, stratState, freshOpenQty, res.ErrMsg, mu, notifier, logger)
			}
			return
		}
	} else {
		res = paperHedgeOrderResult(action, snap, hedgePx)
		if !res.OK {
			if logger != nil {
				logger.Warn("hedge sync: paper hedge %s refused: %s", action.Kind, res.ErrMsg)
			}
			return
		}
	}
	mu.Lock()
	applyHedgeFill(stratState, sc, action, snap, res, !live, logger)
	mu.Unlock()
}

// unwindPrimaryAfterHedgeOpenFailure executes the fail-closed path for a
// fresh primary fill whose hedge leg failed to open: CRITICAL alert, sized
// reduce-only unwind of the fill (cancelling its just-armed SL/TP OIDs),
// booking via applyPrimaryUnwindFill, CRITICAL confirmation. If the unwind
// itself fails, state is left unchanged and a second CRITICAL goes out —
// the state-derived hedge sync retries every cycle (restart-safe, no new
// latch state).
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, stratState *StrategyState, unwindQty float64, hedgeErr string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	primarySym := hyperliquidConfiguredCoin(sc)
	if primarySym == "" || unwindQty <= hedgeQtyEpsilon {
		return
	}
	sendHedgeCritical(notifier, formatHedgeOpenFailureCritical(sc, hedgeErr))
	mu.RLock()
	var cancelOIDs []int64
	if pos := stratState.Positions[primarySym]; pos != nil {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()
	res := runPrimaryUnwindOrder(sc, primarySym, unwindQty, cancelOIDs)
	if !res.OK {
		if logger != nil {
			logger.Error("hedge-open-failure unwind of %.6g %s failed: %s — primary left open and unhedged; hedge sync retries every cycle", unwindQty, primarySym, res.ErrMsg)
		}
		sendHedgeCritical(notifier, formatHedgeUnwindFailureCritical(sc, unwindQty, res.ErrMsg))
		return
	}
	mu.Lock()
	applyPrimaryUnwindFill(stratState, primarySym, res, logger)
	mu.Unlock()
	sendHedgeCritical(notifier, formatHedgeUnwindSuccessCritical(sc, unwindQty))
}

// ---------------------------------------------------------------------------
// Phase G — foreign position on a declared hedge coin (alerting only)
// ---------------------------------------------------------------------------

// hedgeForeignCoinWarn is one queued foreign-hedge-coin warning (collected
// under mu by the reconcile pass, DM'd after unlock when the throttle
// allows).
type hedgeForeignCoinWarn struct {
	strategyID string
	coin       string
	msg        string
}

// hedgeForeignCoinAlerts throttles the "foreign position on declared hedge
// coin" owner DM to the fleet alert cadence (alert_throttle_interval). The
// per-cycle WARN log line is unconditional; only the DM is throttled.
var hedgeForeignCoinAlerts = struct {
	sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time)}

// hedgeForeignCoinAlertDue reports whether the DM for this strategy/coin
// pair is due now (and stamps the send time when it is). Keyed per
// strategy+coin so two hedgers never throttle each other.
func hedgeForeignCoinAlertDue(strategyID, coin string, now time.Time) bool {
	hedgeForeignCoinAlerts.Lock()
	defer hedgeForeignCoinAlerts.Unlock()
	key := strategyID + "/" + coin
	if last, ok := hedgeForeignCoinAlerts.last[key]; ok && now.Sub(last) < effectiveAlertThrottleInterval() {
		return false
	}
	hedgeForeignCoinAlerts.last[key] = now
	return true
}

// formatHedgeForeignCoinAlert is the operator-facing warning when the
// exchange reports a position on a strategy's declared hedge coin but no
// virtual hedge leg exists. The reconcile pass NEVER adopts it — ownership
// comes from the persisted Position.HedgeFor stamp, not coin inference.
func formatHedgeForeignCoinAlert(sc StrategyConfig, coin string, onChainSize float64) string {
	return fmt.Sprintf("WARN: %s has a FOREIGN on-chain position on its declared hedge coin %s (size %.6g) with no matching virtual hedge leg — NOT adopting it. If intentional, move the position or change hedge.symbol; if it is a stale hedge leg, the state DB lost the row — restore or close manually (#1159)",
		sc.ID, coin, onChainSize)
}
