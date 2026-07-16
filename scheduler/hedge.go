package main

// Per-strategy correlated hedge legs, phase 1 (#1159).
//
// A strategy may opt into an automatically managed hedge leg on a second HL
// perps coin (e.g. primary long ETH → hedge short BTC). Hedge management is a
// per-cycle, state-derived reconciler ("hedge sync"), not scattered per-event
// mirror hooks: hedgeTargetDecision computes the hedge action from the current
// primary position vs. the persisted Position.HedgePrimaryQtyBasis watermark,
// and runHedgeSync converges the hedge leg to it on every HL dispatch cycle.
// Qty-event mirroring only — mark-price drift never re-trades the hedge.
//
// Phase-1 invariants:
//   - HL perps only, side "inverse" only, direction "both" rejected.
//   - Collision rejection (validateHedgeConfigs) guarantees a hedge coin is
//     never any strategy's configured coin and never shared between hedgers,
//     so every hyperliquidConfiguredCoin-keyed shared-coin mechanism stays
//     untouched; hedge visibility is added via explicit extensions (marks,
//     kill-switch roster, wallet books, reconcile).
//   - Fail-closed open: primary fill confirmed + hedge open failed on the same
//     cycle → reduce-only unwind of the primary + CRITICAL owner DM
//     (unwindPrimaryAfterHedgeOpenFailure). Never run unhedged silently.
//   - Fill-confirmed state mutation only (live-exec guard): hedge virtual
//     state mutates only from confirmed fills; skip-reason mirror
//     (hedgeOrderSkipReason) runs immediately before spawning.
//   - Hedge legs book PnL/fees to the owning strategy's ledger but are not
//     independent alpha: RecordHedgeTradeResult never touches the CB loss
//     streak, trade_diagnostics skips them, and lifetime W/L excludes them.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HedgeConfig declares an automatically managed correlated hedge leg (#1159).
// Phase 1: Hyperliquid perps only, side "inverse" only, strictly coupled to
// the primary position lifecycle (no hedge SL/TP or close evaluator).
type HedgeConfig struct {
	Enabled    bool    `json:"enabled"`
	Symbol     string  `json:"symbol"`      // hedge coin: "BTC" or ccxt "BTC/USDC:USDC" (normalized via hedgeCoin)
	Side       string  `json:"side"`        // "inverse" (only value in phase 1; empty defaults to it)
	Ratio      float64 `json:"ratio"`       // notional multiplier vs primary notional; 0 → 1.0; bounds (0, 10]
	Platform   string  `json:"platform"`    // must be "" or "hyperliquid" in phase 1
	Type       string  `json:"type"`        // must be "" or "perps" in phase 1
	MarginMode string  `json:"margin_mode"` // hedge leg's OWN on-chain margin mode: "isolated" (default) or "cross"
	Leverage   float64 `json:"leverage"`    // hedge leg's OWN exchange leverage; 0 → 1
}

// HedgeEnabled reports whether sc declares an enabled hedge block. Read this
// accessor, never sc.Hedge fields directly.
func HedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

// hedgeCoin normalizes the hedge block's symbol to a bare upper-case coin
// ticker ("BTC/USDC:USDC" → "BTC"), matching hyperliquidConfiguredCoin's
// normalization so collision checks compare like with like. "" when no hedge
// block or no symbol.
func hedgeCoin(sc StrategyConfig) string {
	if sc.Hedge == nil {
		return ""
	}
	raw := strings.ToUpper(strings.TrimSpace(sc.Hedge.Symbol))
	if i := strings.IndexAny(raw, "/:"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// HedgeRatio returns the notional multiplier (default 1.0).
func HedgeRatio(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Ratio <= 0 {
		return 1.0
	}
	return sc.Hedge.Ratio
}

// hedgeLeverage returns the hedge leg's exchange leverage (default 1).
func hedgeLeverage(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Leverage <= 0 {
		return 1
	}
	return sc.Hedge.Leverage
}

// hedgeMarginMode returns the hedge leg's margin mode (default "isolated").
func hedgeMarginMode(sc StrategyConfig) string {
	if sc.Hedge == nil || sc.Hedge.MarginMode == "" {
		return "isolated"
	}
	return sc.Hedge.MarginMode
}

// positionCloseTradeType returns the trade_type label for a position's close
// leg: "hedge" for hedge legs (excluded from lifetime W/L, #1159), else
// "perps" (the historical label at these call sites).
func positionCloseTradeType(pos *Position) string {
	if pos != nil && pos.HedgeFor != "" {
		return "hedge"
	}
	return "perps"
}

// hedgeConfigEqual reports whether two hedge blocks are identical for
// hot-reload purposes (nil-vs-nil pointer semantics like scaleInConfigEqual).
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// validateHedgeConfigs enforces the phase-1 hedge constraint matrix (#1159).
// Called from validateConfig and re-run by validateHotReloadCompatible.
func validateHedgeConfigs(cfg *Config) []string {
	var errs []string
	// hedge coin → strategy id, for hedge-vs-hedge collisions.
	hedgeOwners := make(map[string]string)
	// every configured coin (perps AND manual, all modes) → strategy id.
	configuredCoins := make(map[string]string)
	for _, sc := range cfg.Strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			configuredCoins[coin] = sc.ID
		}
	}
	for _, sc := range cfg.Strategies {
		if sc.Hedge == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge legs are Hyperliquid perps only in phase 1 (strategy is %s/%s)", prefix, sc.Platform, sc.Type))
		}
		if h.Platform != "" && h.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s.platform: must be \"hyperliquid\" (or omitted), got %q", prefix, h.Platform))
		}
		if h.Type != "" && h.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s.type: must be \"perps\" (or omitted), got %q", prefix, h.Type))
		}
		if h.Side != "" && h.Side != "inverse" {
			errs = append(errs, fmt.Sprintf("%s.side: only \"inverse\" is supported in phase 1, got %q", prefix, h.Side))
		}
		if h.Ratio < 0 || h.Ratio > 10 {
			errs = append(errs, fmt.Sprintf("%s.ratio: must be in (0, 10] (or omitted for 1.0), got %g", prefix, h.Ratio))
		}
		if h.Leverage < 0 {
			errs = append(errs, fmt.Sprintf("%s.leverage: must be > 0 (or omitted for 1), got %g", prefix, h.Leverage))
		}
		if h.MarginMode != "" && h.MarginMode != "isolated" && h.MarginMode != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode: must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}
		coin := hedgeCoin(sc)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s.symbol: required (hedge coin ticker, e.g. \"BTC\")", prefix))
		} else {
			if coin == hyperliquidConfiguredCoin(sc) {
				errs = append(errs, fmt.Sprintf("%s.symbol: %q equals the strategy's own coin — a same-coin hedge just nets the position on-chain", prefix, coin))
			} else if owner, taken := configuredCoins[coin]; taken {
				errs = append(errs, fmt.Sprintf("%s.symbol: %q collides with strategy %q's configured coin — HL aggregates positions per coin per account (phase-1 constraint 2)", prefix, coin, owner))
			}
			if !h.Enabled {
				// A disabled block changes nothing live; skip the
				// hedge-vs-hedge claim so a parked block can't collide.
			} else if other, dup := hedgeOwners[coin]; dup {
				errs = append(errs, fmt.Sprintf("%s.symbol: %q is already strategy %q's hedge coin — two hedge legs on one coin would share the on-chain position", prefix, coin, other))
			} else {
				hedgeOwners[coin] = sc.ID
			}
		}
		if h.Enabled && EffectiveDirection(sc) == "both" {
			errs = append(errs, fmt.Sprintf("%s: direction \"both\" is not supported with a hedge leg in phase 1 (flips change hedge side mid-flight)", prefix))
		}
	}
	return errs
}

// ---------------------------------------------------------------------------
// Pure decision core

type hedgeActionKind int

const (
	hedgeActionNone hedgeActionKind = iota
	hedgeActionOpen
	hedgeActionAdd
	hedgeActionReduce
	hedgeActionCloseFull
)

// hedgeMinOrderNotionalUSD defers open/add/reduce legs whose notional is below
// HL's minimum order size (~$10). The basis watermark is deliberately NOT
// advanced on a deferral so the delta accumulates and retries. closeFull
// always executes.
const hedgeMinOrderNotionalUSD = 10.0

// hedgeSnapshot is the cycle-local state hedgeTargetDecision reads, captured
// under the Phase-1 RLock.
type hedgeSnapshot struct {
	PrimaryQty    float64
	PrimarySide   string
	HedgeQty      float64
	HedgeSide     string
	HedgeBasis    float64 // Position.HedgePrimaryQtyBasis on the hedge leg
	HedgeForeign  bool    // a position sits at the hedge coin WITHOUT the HedgeFor stamp
	HedgeStranded bool    // hedge leg exists but its HedgeFor doesn't match the primary symbol
}

type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64 // coin quantity on the HEDGE symbol
	Side   string  // "long"/"short" position side for open/add
	Reason string  // human-readable driver (logs) or fail-closed explanation
}

// inverseSide maps the primary side to the phase-1 "inverse" hedge side.
func inverseSide(side string) string {
	if side == "long" {
		return "short"
	}
	return "long"
}

// hedgeTargetDecision computes the converging hedge action from the current
// primary/hedge state. Pure — no locks, no I/O. Unusable prices fail closed
// to None with a Reason for the caller to escalate.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.HedgeForeign {
		return hedgeAction{Kind: hedgeActionNone, Reason: "foreign position on hedge coin — not touching (fail-closed)"}
	}
	const eps = 1e-9
	primaryHeld := snap.PrimaryQty > eps
	hedgeHeld := snap.HedgeQty > eps
	if !primaryHeld && !hedgeHeld {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if !primaryHeld && hedgeHeld {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary flat"}
	}
	// primary held from here.
	wantSide := inverseSide(snap.PrimarySide)
	if hedgeHeld && (snap.HedgeSide != wantSide || snap.HedgeStranded) {
		// Defense in depth — unreachable with direction "both" rejected.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: fmt.Sprintf("hedge side/ownership mismatch (have %s, want %s)", snap.HedgeSide, wantSide)}
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("unusable price (primary=%.4f hedge=%.4f) — holding (fail-closed)", primaryPx, hedgePx)}
	}
	ratio := HedgeRatio(sc)
	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= eps {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge qty is zero"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: wantSide, Reason: "primary held, hedge flat"}
	}
	// Hedge held with the right side — diff qty against the watermark.
	basis := snap.HedgeBasis
	if snap.PrimaryQty > basis+eps {
		deltaQty := (snap.PrimaryQty - basis) * primaryPx * ratio / hedgePx
		if deltaQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("add delta $%.2f below min order notional — deferring (basis not advanced)", deltaQty*hedgePx)}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: deltaQty, Side: wantSide, Reason: fmt.Sprintf("primary grew %.6f → %.6f", basis, snap.PrimaryQty)}
	}
	if snap.PrimaryQty < basis-eps && basis > 0 {
		reduceQty := snap.HedgeQty * (basis - snap.PrimaryQty) / basis
		if reduceQty > snap.HedgeQty {
			reduceQty = snap.HedgeQty
		}
		if reduceQty*hedgePx < hedgeMinOrderNotionalUSD && snap.HedgeQty-reduceQty > eps {
			return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("reduce delta $%.2f below min order notional — deferring (basis not advanced)", reduceQty*hedgePx)}
		}
		if snap.HedgeQty-reduceQty <= eps {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary reduced to ~0 of basis"}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: reduceQty, Reason: fmt.Sprintf("primary shrank %.6f → %.6f", basis, snap.PrimaryQty)}
	}
	return hedgeAction{Kind: hedgeActionNone}
}

// hedgeOrderSkipReason is the skip-reason mirror (CLAUDE.md rule): re-checks
// the decision preconditions against the snapshot immediately before spawning
// a live order, so an on-chain fill can never land without a Trade record.
// "" = proceed.
func hedgeOrderSkipReason(sc StrategyConfig, action hedgeAction) string {
	if action.Kind == hedgeActionNone {
		return "no hedge action"
	}
	if !HedgeEnabled(sc) {
		return "hedge not enabled"
	}
	if hedgeCoin(sc) == "" {
		return "no hedge coin"
	}
	if action.Qty <= 0 {
		return fmt.Sprintf("non-positive hedge qty %.8f", action.Qty)
	}
	if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && action.Side != "long" && action.Side != "short" {
		return fmt.Sprintf("invalid hedge side %q", action.Side)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Runtime sync

// hedgeSyncSnapshot captures the decision inputs under an RLock.
func hedgeSyncSnapshot(stratState *StrategyState, primarySym, hCoin string) hedgeSnapshot {
	var snap hedgeSnapshot
	if pos := stratState.Positions[primarySym]; pos != nil && pos.Quantity > 0 {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	if hpos := stratState.Positions[hCoin]; hpos != nil && hpos.Quantity > 0 {
		if hpos.HedgeFor == "" {
			snap.HedgeForeign = true
		} else {
			snap.HedgeQty = hpos.Quantity
			snap.HedgeSide = hpos.Side
			snap.HedgeBasis = hpos.HedgePrimaryQtyBasis
			snap.HedgeStranded = hpos.HedgeFor != primarySym
		}
	}
	return snap
}

// runHedgeSync converges the hedge leg toward the target implied by the
// current primary position. Called once per HL perps dispatch cycle, after
// the execute/apply block — unconditionally on manage-only cycles too, so
// paused / latched-CB / daily-loss states still reduce and close the hedge
// (they can never grow the primary, so hedge sync can only shrink under
// them). Lock discipline: RLock snapshot → unlocked subprocess → Lock apply
// (the 6-phase pattern). freshPrimaryOpen escalates a hedge-open failure to a
// reduce-only unwind of the just-filled primary (phase-1 constraint 4).
func runHedgeSync(sc StrategyConfig, stratState *StrategyState, primarySym string, primaryPx float64, prices map[string]float64, freshPrimaryOpen bool, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	if !HedgeEnabled(sc) || primarySym == "" {
		return
	}
	hCoin := hedgeCoin(sc)
	if hCoin == "" {
		return
	}

	mu.RLock()
	snap := hedgeSyncSnapshot(stratState, primarySym, hCoin)
	mu.RUnlock()

	hedgePx := prices[hCoin]
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if action.Kind == hedgeActionNone {
		if action.Reason != "" {
			logger.Warn("hedge(%s): holding — %s", hCoin, action.Reason)
			if snap.HedgeForeign {
				notifier.SendOwnerDM(fmt.Sprintf("⚠️ [%s] foreign position on declared hedge coin %s — hedge sync is NOT adopting or trading it. Reconcile manually.", sc.ID, hCoin))
			}
			// A fresh primary open that cannot even compute a hedge target is
			// the constraint-4 case: never run unhedged silently.
			if freshPrimaryOpen && snap.HedgeQty <= 0 {
				unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, primaryPx, action.Reason, mu, notifier, logger)
			}
		}
		return
	}
	if skip := hedgeOrderSkipReason(sc, action); skip != "" {
		logger.Warn("hedge(%s): skipping order — %s", hCoin, skip)
		return
	}
	logger.Info("hedge(%s): %s qty=%.6f (%s)", hCoin, hedgeActionLabel(action.Kind), action.Qty, action.Reason)

	if !hyperliquidIsLive(sc.Args) {
		// Paper: book directly at the hedge mark with a modeled fee.
		mu.Lock()
		applyHedgeFill(sc, stratState, primarySym, hCoin, action, snap, action.Qty, hedgePx, 0, false, "", logger)
		mu.Unlock()
		return
	}

	// Live: spawn the side-effecting subprocess without holding any lock.
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		orderSide := "buy"
		if action.Side == "short" {
			orderSide = "sell"
		}
		// margin_mode/leverage are sent only on a fresh hedge open from flat —
		// HL rejects update_leverage on an open position (#486 mirror).
		marginMode := ""
		var leverage float64
		if action.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeLeverage(sc)
		}
		er, stderr, err := RunHyperliquidExecute(sc.Script, hCoin, orderSide, action.Qty, 0, 0, snap.HedgeQty, marginMode, leverage, false, hlExecuteSnapshot{})
		if err != nil || er == nil || er.Error != "" || er.Execution == nil || er.Execution.Fill == nil || er.Execution.Fill.TotalSz <= 0 {
			errMsg := hedgeExecErrString(er, stderr, err)
			logger.Error("hedge(%s): %s order failed: %s", hCoin, hedgeActionLabel(action.Kind), errMsg)
			notifyLiveExecFailure(notifier, sc, "hedge-"+hedgeActionLabel(action.Kind), hCoin, errMsg)
			if freshPrimaryOpen && action.Kind == hedgeActionOpen {
				unwindPrimaryAfterHedgeOpenFailure(sc, stratState, primarySym, primaryPx, errMsg, mu, notifier, logger)
			}
			return
		}
		clearLiveExecThrottle(sc, "hedge-"+hedgeActionLabel(action.Kind), hCoin)
		fill := er.Execution.Fill
		oidStr := ""
		if fill.OID != 0 {
			oidStr = strconv.FormatInt(fill.OID, 10)
		}
		mu.Lock()
		applyHedgeFill(sc, stratState, primarySym, hCoin, action, snap, fill.TotalSz, fill.AvgPx, fill.Fee, true, oidStr, logger)
		mu.Unlock()
	case hedgeActionReduce, hedgeActionCloseFull:
		sz := action.Qty
		cr, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, hCoin, &sz, nil)
		if err != nil {
			logger.Error("hedge(%s): %s close failed: %v (stderr: %s)", hCoin, hedgeActionLabel(action.Kind), err, stderr)
			notifyLiveExecFailure(notifier, sc, "hedge-"+hedgeActionLabel(action.Kind), hCoin, err.Error())
			return
		}
		clearLiveExecThrottle(sc, "hedge-"+hedgeActionLabel(action.Kind), hCoin)
		if cr != nil && cr.Close != nil && cr.Close.AlreadyFlat {
			// On-chain already flat — the next reconcile books the external
			// close; nothing to mutate here (fill-confirmed only).
			logger.Warn("hedge(%s): close reports already flat on-chain — deferring to reconcile", hCoin)
			return
		}
		if cr == nil || cr.Close == nil || cr.Close.Fill == nil || cr.Close.Fill.TotalSz <= 0 || cr.Close.Fill.AvgPx <= 0 {
			logger.Error("hedge(%s): close returned no usable fill — retrying next cycle", hCoin)
			return
		}
		fill := cr.Close.Fill
		oidStr := ""
		if fill.OID != 0 {
			oidStr = strconv.FormatInt(fill.OID, 10)
		}
		mu.Lock()
		applyHedgeFill(sc, stratState, primarySym, hCoin, action, snap, fill.TotalSz, fill.AvgPx, fill.Fee, true, oidStr, logger)
		mu.Unlock()
	}
}

func hedgeActionLabel(k hedgeActionKind) string {
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

func hedgeExecErrString(er *HyperliquidExecuteResult, stderr string, err error) string {
	switch {
	case er != nil && er.Error != "":
		return er.Error
	case err != nil:
		return err.Error()
	case er == nil || er.Execution == nil || er.Execution.Fill == nil || er.Execution.Fill.TotalSz <= 0:
		return "no fill returned"
	}
	_ = stderr
	return "unknown error"
}

// applyHedgeFill mutates virtual hedge state from a CONFIRMED fill (or books
// the paper equivalent when useFillFee=false and fee=0). Must be called under
// mu.Lock. The basis watermark advances to the primary quantity observed in
// the pre-spawn snapshot; a state change between snapshot and apply is booked
// anyway (never drop an on-chain fill) with a WARN.
func applyHedgeFill(sc StrategyConfig, s *StrategyState, primarySym, hCoin string, action hedgeAction, snap hedgeSnapshot, fillQty, fillPx, fillFee float64, useFillFee bool, oidStr string, logger *StrategyLogger) {
	if fillQty <= 0 || fillPx <= 0 {
		return
	}
	// Re-check-under-lock: WARN on drift, but book regardless.
	if cur := s.Positions[primarySym]; cur != nil && cur.Quantity != snap.PrimaryQty && logger != nil {
		logger.Warn("hedge(%s): primary qty moved %.6f → %.6f between snapshot and apply — booking fill against current state", hCoin, snap.PrimaryQty, cur.Quantity)
	}
	newBasis := snap.PrimaryQty

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		feePlatform := s.Platform
		fee := CalculatePlatformSpotFee(feePlatform, fillQty*fillPx)
		feeSource := FeeSourceModeled
		if useFillFee {
			fee = fillFee
			feeSource = FeeSourceUserFills
		}
		s.Cash -= fee // perps open: only the fee leaves cash, notional stays virtual
		now := time.Now().UTC()
		pos := s.Positions[hCoin]
		if pos == nil || pos.Quantity <= 0 {
			positionID := newTradePositionID(s.ID, hCoin, now)
			pos = &Position{
				Symbol:               hCoin,
				Quantity:             fillQty,
				InitialQuantity:      fillQty,
				AvgCost:              fillPx,
				Side:                 action.Side,
				Multiplier:           1,
				Leverage:             hedgeLeverage(sc),
				OwnerStrategyID:      s.ID,
				OpenedAt:             now,
				TradePositionID:      positionID,
				HedgeFor:             primarySym,
				HedgePrimaryQtyBasis: newBasis,
			}
			s.Positions[hCoin] = pos
		} else {
			// Blend the add leg (mirrors scale-in blend semantics).
			totalQty := pos.Quantity + fillQty
			pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*fillQty) / totalQty
			pos.Quantity = totalQty
			pos.HedgePrimaryQtyBasis = newBasis
		}
		tradeSide := "buy"
		if action.Side == "short" {
			tradeSide = "sell"
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          hCoin,
			PositionID:      ensurePositionTradeID(s.ID, hCoin, pos),
			Side:            tradeSide,
			Quantity:        fillQty,
			Price:           fillPx,
			Value:           fillQty * fillPx,
			TradeType:       "hedge",
			Details:         fmt.Sprintf("hedge(%s) %s %s %.6f @ $%.2f (fee $%.4f)", primarySym, hedgeActionLabel(action.Kind), action.Side, fillQty, fillPx, fee),
			ExchangeOrderID: oidStr,
			ExchangeFee:     fee,
			FeeSource:       feeSource,
			PnLGross:        true,
			Regime:          s.Regime,
		}
		RecordTrade(s, trade)
		if logger != nil {
			logger.Info("hedge(%s): %s %s %.6f @ $%.4f (fee $%.4f, basis=%.6f)", hCoin, hedgeActionLabel(action.Kind), action.Side, fillQty, fillPx, fee, newBasis)
		}
	case hedgeActionReduce:
		if bookPerpsPartialCloseWithFillFee(s, hCoin, fillQty, fillPx, fillFee, useFillFee, oidStr, "hedge_reduce", fmt.Sprintf("hedge(%s) reduce", primarySym), fmt.Sprintf("hedge(%s) reduce", primarySym), logger) {
			if pos := s.Positions[hCoin]; pos != nil {
				pos.HedgePrimaryQtyBasis = newBasis
			}
		}
	case hedgeActionCloseFull:
		bookPerpsCloseWithFillFee(s, hCoin, fillPx, fillFee, useFillFee, oidStr, "hedge_close", fmt.Sprintf("hedge(%s) close", primarySym), fmt.Sprintf("hedge(%s) close", primarySym), logger)
	}
}

// unwindPrimaryAfterHedgeOpenFailure implements phase-1 constraint 4: the
// primary fill confirmed but the hedge leg could not be opened on the same
// cycle → immediately reduce-only close the primary (sized, never
// market_close(sz=None) — the primary coin may have shared-coin peers) and
// CRITICAL-DM the owner. If the unwind itself fails, state is unchanged and
// the state-derived hedge sync retries the hedge open next cycle — no silent
// unhedged running, no new latch state.
func unwindPrimaryAfterHedgeOpenFailure(sc StrategyConfig, stratState *StrategyState, primarySym string, primaryPx float64, why string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	mu.RLock()
	pos := stratState.Positions[primarySym]
	var unwindQty float64
	var cancelOIDs []int64
	if pos != nil && pos.Quantity > 0 {
		unwindQty = pos.Quantity
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()
	if unwindQty <= 0 {
		return
	}
	logger.Error("hedge open failed after primary fill (%s) — unwinding primary %s %.6f (fail-closed, #1159 constraint 4)", why, primarySym, unwindQty)

	if !hyperliquidIsLive(sc.Args) {
		// Paper: book the virtual unwind at the current mark directly.
		mu.Lock()
		bookPerpsCloseWithFillFee(stratState, primarySym, primaryPx, 0, false, "", "hedge_open_failed_unwind", "hedge open failed — primary unwound", "hedge open failed — primary unwound", logger)
		mu.Unlock()
		notifier.SendOwnerDM(fmt.Sprintf("🚨 [%s] hedge open FAILED after primary open (%s) — primary %s position unwound (paper). Cause: %s", sc.ID, hedgeCoin(sc), primarySym, why))
		return
	}

	sz := unwindQty
	cr, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, primarySym, &sz, cancelOIDs)
	if err != nil || cr == nil || cr.Close == nil || cr.Close.Fill == nil || cr.Close.Fill.TotalSz <= 0 || cr.Close.Fill.AvgPx <= 0 {
		detail := "no usable fill"
		if err != nil {
			detail = err.Error()
		}
		logger.Error("hedge-open-failure unwind of %s FAILED: %s (stderr: %s) — position remains open and UNHEDGED; hedge sync retries next cycle", primarySym, detail, stderr)
		notifier.SendOwnerDM(fmt.Sprintf("🚨🚨 [%s] hedge open failed AND the primary unwind failed (%s) — %s is running UNHEDGED. Hedge sync retries next cycle. Cause: %s / unwind: %s", sc.ID, hedgeCoin(sc), primarySym, why, detail))
		return
	}
	fill := cr.Close.Fill
	oidStr := ""
	if fill.OID != 0 {
		oidStr = strconv.FormatInt(fill.OID, 10)
	}
	mu.Lock()
	bookPerpsCloseWithFillFee(stratState, primarySym, fill.AvgPx, fill.Fee, true, oidStr, "hedge_open_failed_unwind", "hedge open failed — primary unwound", "hedge open failed — primary unwound", logger)
	mu.Unlock()
	notifier.SendOwnerDM(fmt.Sprintf("🚨 [%s] hedge open FAILED (%s) after the primary fill — primary %s reduce-only unwound @ $%.4f (fail-closed, #1159). Cause: %s", sc.ID, hedgeCoin(sc), primarySym, fill.AvgPx, why))
}

// validateHedgeStateConsistency surfaces persisted hedge legs whose config no
// longer matches (config edited + restart bypasses the hot-reload guard).
// Non-destructive fail-closed: the position is left frozen; the operator is
// warned. Returns human-readable warnings (mirrors ValidatePerpsDirectionConfig).
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	var warnings []string
	for _, sc := range cfg.Strategies {
		s, ok := state.Strategies[sc.ID]
		if !ok || s == nil {
			continue
		}
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || pos.HedgeFor == "" || pos.Quantity <= 0 {
				continue
			}
			switch {
			case !HedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds a persisted hedge leg %s (for %s) but its config no longer declares an enabled hedge block — leg left frozen; close manually or restore the hedge config (#1159)", sc.ID, sym, pos.HedgeFor))
			case hedgeCoin(sc) != sym:
				warnings = append(warnings, fmt.Sprintf("hedge state gap: strategy %s holds hedge leg %s but config declares hedge.symbol %s — leg left frozen; reconcile manually (#1159)", sc.ID, sym, hedgeCoin(sc)))
			}
		}
	}
	return warnings
}

// hedgeCoinsWithHeldLegs returns hedge coins whose strategies currently hold
// a virtual hedge leg (kill-switch roster extension). Gating on the HELD leg,
// not config alone, prevents the kill switch from liquidating a genuinely
// foreign on-chain position sitting on a declared-but-flat hedge coin. Caller
// must hold at least an RLock on the state mutex.
func hedgeCoinsWithHeldLegs(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig) map[string]bool {
	out := make(map[string]bool)
	for _, sc := range hlLiveAll {
		if !HedgeEnabled(sc) {
			continue
		}
		coin := hedgeCoin(sc)
		if coin == "" {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		if pos := ss.Positions[coin]; pos != nil && pos.HedgeFor != "" && pos.Quantity > 0 {
			out[coin] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
