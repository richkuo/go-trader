package main

import (
	"fmt"
	"sync"
	"time"
)

// scaleInResizeTrailingSLNow eagerly grows a TRAILING stop-loss on the SAME cycle
// as the add, so the increased position is covered immediately instead of waiting
// for the next Signal==0 trailing-walker cycle (#882 review: with a long strategy
// interval, deferring would leave the added size under-covered for up to a full
// cycle — seriously dangerous on a fast adverse move). It is a no-op for
// non-trailing SL owners (the post-trade protection sync already grew their SL on
// this cycle) and for non-trailing-walker positions.
//
// Correctness over a single steady-state path: it reuses the exact walker
// primitive (runHyperliquidTrailingStopUpdate with forceResize) and result
// handler (applyTrailingStopUpdateResult) rather than a second SL-placement path,
// so there is only one stop-placement implementation that can't drift. The only
// added input is the #621 size cap's on-chain qty, which must reflect the GROWN
// position: Go's per-cycle reconcile snapshot is pre-add, so we add the confirmed
// add fill (filledAddQty) to it. If even the corrected qty is still capped (e.g.
// shared-coin reconcile lag), it leaves ScaleInResizePending set so the next
// walker cycle still resizes — same-cycle coverage is best-effort, never worse
// than the deferred path.
func scaleInResizeTrailingSLNow(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	preAddOnChainAbsQty map[string]float64,
	filledAddQty float64,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" || mark <= 0 {
		return 0, ""
	}
	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || !pos.ScaleInResizePending || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	triggerPx := pos.StopLossTriggerPx
	slOID := pos.StopLossOID
	posSnap := *pos
	mu.RUnlock()

	// #621 size cap with the on-chain qty corrected for the just-confirmed add.
	grownOnChain := map[string]float64{symbol: preAddOnChainAbsQty[symbol] + filledAddQty}
	slEffectiveQty, capped := hlSLEffectiveQty(symbol, posSnap.Quantity, grownOnChain)
	if capped {
		// Reconcile hasn't caught up enough to size the SL to the grown total;
		// leave the flag for the next walker cycle rather than place an
		// under-sized stop now.
		logger.Warn("scale-in eager SL resize: %s still capped (virtual %.6f > on-chain %.6f); deferring to next walker cycle", symbol, posSnap.Quantity, slEffectiveQty)
		return 0, ""
	}
	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, slOID, true, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	trades := 0
	detail := ""
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, slOID, newHighWater, updateConfirmed, slUpdate, logger); immediateFill {
		trades = 1
		detail = fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed {
		if p, ok := stratState.Positions[symbol]; ok && p != nil {
			p.ScaleInResizePending = false
			logger.Info("Scale-in trailing SL re-sized same-cycle (qty=%.6f)", slEffectiveQty)
		}
	}
	return trades, detail
}

// scaleInTradeType marks the open-side leg of a scale-in so lifetime stats can
// exclude it from the round-trip open count (#T) — an add is a second open-side
// leg on the SAME position id, not a new position. W/L grouping is unaffected
// (it keys on close legs, which a scale-in never is).
const scaleInTradeType = "scale_in"

// Scale-in / pyramiding core (#873).
//
// A scale-in increases an open position's size on a same-direction signal
// instead of skipping it (the default). It BLENDS price and size for PnL —
//
//	AvgCost  = (oldQty·oldAvg + addQty·addPrice) / (oldQty + addQty)
//	Quantity += addQty
//
// — and FREEZES the original risk plan: EntryATR, the regime label, and the
// take-profit tier geometry stay pinned to the first entry. Only on-chain
// protection SIZING is re-based (handled by the HL protection sync). The
// cleared-tier watermark (SLAdjustedTiersProcessed / TPArmedTiers) is never
// reset by an add. InitialQuantity grows with the add so the
// `Quantity < InitialQuantity` "partially closed" test stays correct.
//
// This file holds the two pure, subprocess-free helpers: applyScaleIn (the
// blend) and perpsScaleInDecision (the gate). They are unit-tested without
// spawning Python. The dispatch wiring (live order + paper) lives in main.go;
// the manual CLI path is in manual.go.

// applyScaleIn blends an add leg of addQty @ addPrice into pos. The caller must
// already have decided the add is allowed (via perpsScaleInDecision for the
// strategy-flag path, or operator intent for manual-add). pos must be a live
// same-direction position. EntryATR, Regime, RegimeWindows, and the tier
// watermarks are intentionally left untouched (frozen entry).
func applyScaleIn(pos *Position, addQty, addPrice float64) {
	if pos == nil || addQty <= 0 || addPrice <= 0 {
		return
	}
	// Capture the frozen risk anchor before the first blend overwrites AvgCost.
	// On the first add, AvgCost still equals the original entry, so this pins the
	// SL/TP trigger geometry to the first entry for the life of the position.
	if pos.RiskAnchorPrice <= 0 {
		pos.RiskAnchorPrice = pos.AvgCost
	}
	oldQty := pos.Quantity
	newQty := oldQty + addQty
	if newQty > 0 {
		pos.AvgCost = (oldQty*pos.AvgCost + addQty*addPrice) / newQty
	}
	pos.Quantity = newQty
	// Grow the high-water size so Quantity < InitialQuantity remains the
	// canonical "partially closed" test and the tier-split baseline covers the
	// new total.
	pos.InitialQuantity += addQty
	pos.ScaleInCount++
	pos.LastAddPrice = addPrice
	pos.AddedNotionalUSD += addQty * addPrice
	// Force the next HL-live protection sync to cancel+replace SL + un-cleared
	// TP tiers at the grown size (at the frozen trigger geometry). Transient;
	// cleared after the sync. No-op in paper mode.
	pos.ScaleInResizePending = true
}

// scaleInLiveProtectionResizable reports whether sc's HL stop-loss owner can be
// grown after an add (#873, guard adopted from #875). The two resize paths are:
//   - the protection sync's force-replace, which fires only for an ATR/regime
//     fixed SL or a close that arms an ATR SL (EffectiveStopLossPct defers to 0
//     for those), and re-sizes un-cleared TP tiers too; and
//   - the trailing walker's forceResize, for any trailing SL.
//
// A static scalar SL — stop_loss_pct, stop_loss_margin_pct, or the implicit
// max_drawdown fallback — is placed once at open with no resize path, so an add
// would leave it under-covering the grown position. Those make
// EffectiveStopLossPct return a positive pct while no trailing owner is present.
func scaleInLiveProtectionResizable(sc StrategyConfig) bool {
	trailing := (sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0) ||
		(sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0) ||
		(sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero()) ||
		strategyUsesTrailingTPRatchetClose(sc)
	if trailing {
		return true
	}
	// A positive effective pct here (trailing already excluded) means the active
	// owner is a static scalar SL with no resize path.
	if EffectiveStopLossPct(sc) > 0 {
		return false
	}
	// EffectiveStopLossPct == 0 with no trailing owner means an ATR/regime fixed
	// or close-owned ATR SL (deferred arming) — the sync force-replaces it — or
	// genuinely no SL, in which case there is nothing to under-cover.
	return true
}

// scaleInSnapshot is the read-only position state perpsScaleInDecision needs.
// Captured under RLock in Phase 1 so the decision can be computed once and
// consumed by both the skip-reason gate (Phase 3) and the state-apply (Phase 4)
// — a single source of truth that keeps the on-chain order and the Trade record
// consistent (the #298 fill-without-Trade gap).
type scaleInSnapshot struct {
	Side             string
	Quantity         float64
	AvgCost          float64
	EntryATR         float64
	ScaleInCount     int
	AddedNotionalUSD float64
	LastAddPrice     float64
}

// perpsScaleInDecision decides whether a same-direction perps signal should ADD
// to the existing position (scale-in) rather than be skipped. It is pure and
// gates on: opt-in (AllowScaleIn), direction-match (an add never flips), the
// max-adds and max-added-notional caps, and the signed ATR spacing.
//
// defaultOpenNotionalUSD is the strategy's standard open notional (the same
// sizing a fresh open leg uses); it is the per-add notional unless
// ScaleIn.AddNotionalUSD overrides it. Returns (addQty, ok, reason): when ok is
// false, reason is a log-ready string and the caller falls through to the
// normal skip-on-same-direction behavior.
func perpsScaleInDecision(sc StrategyConfig, snap scaleInSnapshot, signal int, price, defaultOpenNotionalUSD float64) (addQty float64, ok bool, reason string) {
	if !sc.AllowScaleIn {
		return 0, false, "scale-in not enabled"
	}
	if price <= 0 {
		return 0, false, "no price for scale-in"
	}
	// Direction match: a buy adds to a long, a sell adds to a short. Anything
	// else (opposite signal, or flat) is a close/cover/flip/fresh-open and is
	// handled by the existing paths — never a scale-in.
	switch {
	case signal == 1 && snap.Side == "long" && snap.Quantity > 0:
	case signal == -1 && snap.Side == "short" && snap.Quantity > 0:
	default:
		return 0, false, "not a same-direction add"
	}

	var cfg ScaleInConfig
	if sc.ScaleIn != nil {
		cfg = *sc.ScaleIn
	}

	if cfg.MaxAdds > 0 && snap.ScaleInCount >= cfg.MaxAdds {
		return 0, false, "scale-in max_adds reached"
	}

	addNotional := defaultOpenNotionalUSD
	if cfg.AddNotionalUSD > 0 {
		addNotional = cfg.AddNotionalUSD
	}
	if addNotional <= 0 {
		return 0, false, "scale-in add notional resolves to zero"
	}
	// The cap compares the cumulative ACTUAL added notional (snap.AddedNotionalUSD,
	// accumulated from fills) plus this add's REQUESTED notional. Live fills can
	// slip slightly from the requested sizing, so the cap is an approximate
	// guardrail, not an exact ceiling — acceptable for a soft limit (#873 review).
	if cfg.MaxAddedNotionalUSD > 0 && snap.AddedNotionalUSD+addNotional > cfg.MaxAddedNotionalUSD+1e-9 {
		return 0, false, "scale-in max_added_notional_usd reached"
	}

	if cfg.AddSpacingATR != 0 {
		if snap.EntryATR <= 0 {
			return 0, false, "scale-in spacing requires a positive EntryATR"
		}
		lastAdd := snap.LastAddPrice
		if lastAdd <= 0 {
			lastAdd = snap.AvgCost
		}
		dir := 1.0
		if snap.Side == "short" {
			dir = -1.0
		}
		// favorableMove > 0 when price has moved in the position's favor since
		// the last entry leg (up for a long, down for a short).
		favorableMove := (price - lastAdd) * dir
		needed := cfg.AddSpacingATR * snap.EntryATR
		if cfg.AddSpacingATR > 0 {
			// add-to-winners: require an in-favor move of at least `needed`.
			if favorableMove+1e-9 < needed {
				return 0, false, "scale-in spacing (add-to-winners) not reached"
			}
		} else {
			// average-down: require an adverse move of at least |needed|.
			if -favorableMove+1e-9 < -needed {
				return 0, false, "scale-in spacing (average-down) not reached"
			}
		}
	}

	return addNotional / price, true, ""
}

// applyPerpsScaleIn blends an add leg into the existing same-direction perps
// position and builds (but does not record) the scale_in open-side Trade leg.
// It mirrors the open-leg cash math of executePerpsSignalWithLeverage:
// margin-based, so only the fee leaves cash and the notional stays virtual.
// Returns (1, &trade) on success, (0, nil) when there's nothing to add. The
// caller records the trade — deferred until after the protection sync for live,
// immediately for paper.
func applyPerpsScaleIn(s *StrategyState, sc StrategyConfig, symbol string, addPrice, addQty, fillFee float64, fillOID string, useFillFee bool, logger *StrategyLogger) (int, *Trade) {
	if addQty <= 0 || addPrice <= 0 {
		return 0, nil
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		// Defensive (#873 review): the add decision is computed from the Phase-1
		// snapshot and the position can only vanish here if some other path
		// flattened it between the fill and this apply — not reachable in the
		// current single-threaded per-strategy dispatch. Surface it loudly rather
		// than silently dropping a booked fill (the #298 gap class).
		if useFillFee {
			logger.Error("scale-in fill (oid=%s qty=%.6f @ $%.2f) has no position to apply to for %s — fill booked on-chain with NO Trade record", fillOID, addQty, addPrice, symbol)
		}
		return 0, nil
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	notional := addQty * addPrice
	fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
	s.Cash -= fee // margin-based: only the fee leaves cash, notional stays virtual
	side := "buy"
	if pos.Side == "short" {
		side = "sell"
	}
	// Blend price+size; freeze EntryATR/regime/TP geometry; grow InitialQuantity;
	// flag the next protection sync to re-size SL + un-cleared TPs.
	applyScaleIn(pos, addQty, addPrice)
	now := time.Now().UTC()
	var oid string
	if useFillFee {
		oid = fillOID
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          symbol,
		PositionID:      ensurePositionTradeID(s.ID, symbol, pos),
		Side:            side,
		Quantity:        addQty,
		Price:           addPrice,
		Value:           notional,
		TradeType:       scaleInTradeType,
		Details:         fmt.Sprintf("Scale-in %s %.6f @ $%.2f (add #%d, new qty %.6f, avg $%.2f, fee $%.2f)", pos.Side, addQty, addPrice, pos.ScaleInCount, pos.Quantity, pos.AvgCost, fee),
		ExchangeOrderID: oid,
		ExchangeFee:     exchangeFeeForTrade(fillFee, useFillFee),
		IsClose:         false,
	}
	trade.Regime = pos.Regime
	trade.EntryATR = pos.EntryATR
	logger.Info("SCALE-IN %s: +%.6f @ $%.2f (new qty %.6f, avg $%.2f, add #%d, fee $%.2f)", symbol, addQty, addPrice, pos.Quantity, pos.AvgCost, pos.ScaleInCount, fee)
	return 1, &trade
}

// scaleInProtectionForceReplace forces the HL protection sync to cancel+replace
// the SL and any already-placed (un-cleared) TP tiers after a scale-in. The
// trigger PRICES are frozen, but the SIZE grew, so the existing trigger orders
// under-cover the new total and must be resized. Tiers with no resting order
// (OID 0) are placed fresh by the sync at the new size; cleared/filled tiers are
// left untouched (the watermark is never reset by an add).
func scaleInProtectionForceReplace(pos *Position, plan hlProtectionPlan) (forceSL bool, forceTP []bool) {
	forceSL = plan.StopLossATRMult > 0
	if len(plan.Tiers) == 0 {
		return forceSL, nil
	}
	forceTP = make([]bool, len(plan.Tiers))
	for i := range plan.Tiers {
		if i < len(pos.TPOIDs) && pos.TPOIDs[i] > 0 {
			forceTP[i] = true
		}
	}
	return forceSL, forceTP
}

// orForceReplace returns the element-wise OR of two force-replace slices,
// tolerating differing lengths (the result is as long as the longer input).
func orForceReplace(a, b []bool) []bool {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	if n == 0 {
		return nil
	}
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		ai := i < len(a) && a[i]
		bi := i < len(b) && b[i]
		out[i] = ai || bi
	}
	return out
}

// runHyperliquidScaleInOrder places a live same-side market order of addSize to
// scale into an open HL perps position (Phase 3, no lock). Unlike
// runHyperliquidExecuteOrder it does NOT cancel the resting SL/TP and does NOT
// re-send update_leverage (HL rejects leverage changes on an open position) —
// the post-add protection sync re-sizes SL + un-cleared TPs at the frozen
// triggers. Returns (execResult, ok); ok=false means the caller must not apply
// state mutations.
func runHyperliquidScaleInOrder(sc StrategyConfig, result *HyperliquidResult, addSize float64, walletSnapshot hlExecuteSnapshot, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	side := "buy"
	if result.Signal == -1 {
		side = "sell"
	}
	logger.Info("Placing live scale-in %s %s size=%.6f", side, result.Symbol, addSize)
	execResult, stderr, err := RunHyperliquidExecute(sc.Script, result.Symbol, side, addSize, 0, 0, 0, "", 0, false, walletSnapshot)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live scale-in failed: %v", err)
		notifyLiveExecFailure(notifier, sc, directionOpen, result.Symbol, err.Error())
		return execResult, false
	}
	if execResult.Error != "" {
		logger.Error("Live scale-in returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, directionOpen, result.Symbol, execResult.Error)
		return execResult, false
	}
	clearLiveExecThrottle(sc, directionOpen, result.Symbol)
	return execResult, true
}

// executeHyperliquidScaleInDeferredOpen applies a scale-in to virtual state.
// Must be called under Lock. execResult is non-nil for a live fill (use the
// fill price/qty/fee), nil for paper (use the modeled addQty at the mid). For
// live, the scale_in trade is returned so the caller can run same-cycle
// protection re-size before the single INSERT; for paper it is recorded here.
func executeHyperliquidScaleInDeferredOpen(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price, addQty float64, logger *StrategyLogger) (int, string, *Trade) {
	fillPrice := price
	fillAddQty := addQty
	var fillOID string
	var fillFee float64
	useFillFee := false
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		fill := execResult.Execution.Fill
		if fill.AvgPx > 0 {
			fillPrice = fill.AvgPx
		}
		if fill.TotalSz > 0 {
			fillAddQty = fill.TotalSz // settle against the actual filled size
		}
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		fillFee = fill.Fee
		useFillFee = true
		logger.Info("Live scale-in fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillAddQty, price)
	}
	trades, openTrade := applyPerpsScaleIn(s, sc, result.Symbol, fillPrice, fillAddQty, fillFee, fillOID, useFillFee, logger)
	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %sSCALE-IN %s @ $%.2f", sc.ID, prefix, result.Symbol, fillPrice)
	}
	if execResult == nil {
		// Paper has no post-unlock protection sync; record now and nil the
		// deferred trade so the caller cannot double-insert it.
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
	}
	return trades, detail, openTrade
}
