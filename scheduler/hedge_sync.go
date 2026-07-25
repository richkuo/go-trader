package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Injection seams for the hedge order path. Package-level vars (mirroring the
// defaultHyperliquid*Closer pattern) so Go tests can exercise every live
// success/failure branch WITHOUT spawning Python — the repo's CI rule. Never
// reassigned in production code.
var (
	hedgeExecuteFn             = RunHyperliquidExecute
	hedgeCloseFn               = RunHyperliquidClose
	hedgeCloseCancelAfterFilFn = RunHyperliquidCloseCancelAfterFill
)

// runHedgeSync is the ONE orchestrator that converges a strategy's correlated
// hedge leg to the target derived from its primary position (#1159 phase 1).
//
// It follows the repo's 6-phase lock discipline exactly, like
// runHyperliquidProtectionSync: snapshot under RLock, run the subprocess with
// NO lock held, re-snapshot + apply under Lock. It must be called without
// holding mu.
//
// Call sites (both needed, and deliberately idempotent so overlap is free):
//
//   - inline at the tail of the HL perps dispatch, so a fresh open is hedged in
//     the SAME cycle and a hedge-open failure unwinds the primary immediately
//     rather than after a full interval of naked exposure;
//   - once per cycle over every hedge-enabled strategy that the dispatch did
//     not reach, so asynchronous primary changes (on-chain SL/TP fills booked
//     by reconcile, kill switch, circuit breaker, manual force-close, external
//     closes, a crash between the two legs) still converge.
//
// Deliberately NOT gated by pause (#1150), the daily-loss hold (#1269), the
// notional cap (#1344), or the exposure cap (#1270). Those gates hold SIGNALS —
// discretionary new risk. A hedge order is not a signal; it is the risk-reducing
// other half of a position those gates have already allowed to exist. Under any
// of those states the primary cannot grow, so hedge sync can only reduce or
// close, which is precisely what should keep running.
func runHedgeSync(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	prices map[string]float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) {
	if s == nil || mu == nil {
		return
	}
	// Phase 1: snapshot under RLock.
	mu.RLock()
	snap := hedgeSnapshotFor(sc, s, prices)
	mu.RUnlock()

	// A strategy with no hedge config and no hedge leg is the overwhelmingly
	// common case — bail before doing any work.
	if !HedgeEnabled(sc) && snap.HedgeQty <= 0 && snap.HedgeStaleReason == "" {
		return
	}

	action := hedgeTargetDecision(HedgeEnabled(sc), hedgeRatio(sc), snap)
	if action.Kind == hedgeActionNone {
		if action.Blocked {
			hedgeHandleBlockedAction(sc, s, mu, snap, action, prices, notifier, logger)
		}
		return
	}
	if logger != nil {
		logger.Info("hedge-sync: %s %s %.6f — %s", action.Kind, snap.HedgeSymbol, action.Qty, action.Reason)
	}

	// Phase 3: execute with NO lock held. Re-check the decision against a fresh
	// snapshot immediately before spawning (CLAUDE.md skip-reason mirror) so an
	// on-chain fill can never land without a matching Trade row.
	mu.RLock()
	fresh := hedgeSnapshotFor(sc, s, prices)
	mu.RUnlock()
	if reason := hedgeOrderSkipReason(action, fresh); reason != "" {
		if logger != nil {
			logger.Info("hedge-sync: skipping %s on %s — %s", action.Kind, snap.HedgeSymbol, reason)
		}
		return
	}

	fill, ok := runHedgeOrder(sc, snap, action, notifier, logger)
	if !ok {
		hedgeHandleOrderFailure(sc, s, mu, snap, action, prices, notifier, logger)
		return
	}
	if action.Kind.hedgeIncreasesExposure() {
		globalHedgeFailures.clear(sc.ID)
	}

	// Phase 4: apply under Lock. Only confirmed fills mutate state.
	mu.Lock()
	applyHedgeFill(sc, s, snap, action, fill, logger)
	mu.Unlock()
}

// hedgeStrategyHoldsLeg reports whether the strategy currently holds a hedge
// leg, taking the read lock itself. Used at dispatch sites to decide whether a
// strategy with no (longer) enabled hedge config still needs a sync pass to
// unwind a stale leg. Must be called WITHOUT holding mu.
func hedgeStrategyHoldsLeg(s *StrategyState, mu *sync.RWMutex) bool {
	if s == nil || mu == nil {
		return false
	}
	mu.RLock()
	defer mu.RUnlock()
	pos, _ := hedgePositionOf(s)
	return pos != nil
}

// hedgeSnapshotFor captures primary + hedge state for one strategy. Caller must
// hold at least an RLock.
//
// Ownership of the hedge leg comes from the persisted HedgeFor stamp, never
// from the configured hedge symbol (phase-1 constraint 5): a foreign on-chain
// position that happens to sit on a declared hedge coin must never be adopted,
// and a hedge leg whose config was edited away must still be found so it can be
// unwound rather than stranded.
func hedgeSnapshotFor(sc StrategyConfig, s *StrategyState, prices map[string]float64) hedgeSnapshot {
	snap := hedgeSnapshot{PrimarySymbol: hyperliquidConfiguredCoin(sc)}
	if s == nil {
		return snap
	}
	if pos := s.Positions[snap.PrimarySymbol]; pos != nil && pos.Quantity > 0 {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	snap.PrimaryPx = hedgeMarkFor(prices, snap.PrimarySymbol, s, snap.PrimarySymbol)

	heldPos, heldCoin := hedgePositionOf(s)
	configured := hedgeCoin(sc)
	switch {
	case heldPos != nil:
		snap.HedgeSymbol = heldCoin
		snap.HedgeQty = heldPos.Quantity
		snap.HedgeSide = heldPos.Side
		snap.HedgeBasis = heldPos.HedgePrimaryQtyBasis
		// A config edit plus a process restart bypasses the SIGHUP guard that
		// normally blocks hedge-block changes while open. Detect the drift here
		// and let the decision unwind the now-unauthorized leg deterministically
		// instead of leaving live exposure nobody manages.
		switch {
		case !HedgeEnabled(sc):
			snap.HedgeStaleReason = fmt.Sprintf(
				"held hedge leg on %s is no longer authorized by config (hedge block removed or disabled) — unwinding", heldCoin)
		case configured != heldCoin:
			snap.HedgeStaleReason = fmt.Sprintf(
				"held hedge leg on %s no longer matches configured hedge symbol %s — unwinding the stale leg first", heldCoin, configured)
		}
	case HedgeEnabled(sc):
		snap.HedgeSymbol = configured
	}
	if snap.HedgeSymbol != "" {
		snap.HedgePx = hedgeMarkFor(prices, snap.HedgeSymbol, s, snap.HedgeSymbol)
	}
	return snap
}

// hedgeMarkFor resolves a mark for sizing. Prefers this cycle's fetched mark —
// collectPerpsMarkSymbols includes hedge coins, so it is normally present.
//
// When the mark is missing (a price-feed hiccup) it falls back to the virtual
// position's AvgCost, which only exists once a leg is HELD. That bounds the
// fallback to the cases where it is the lesser evil: a proportional reduce or
// close (unaffected — the fraction comes from quantities), or an add sized at
// the position's cost basis rather than the current mark. Sizing an add
// slightly stale beats fail-closing an established, correctly-hedged position
// out of the market over a transient feed outage.
//
// A fresh OPEN has no position to fall back to, so it returns 0 and the
// decision fails closed — never a guessed size for exposure that does not yet
// exist.
func hedgeMarkFor(prices map[string]float64, symbol string, s *StrategyState, posKey string) float64 {
	if symbol == "" {
		return 0
	}
	if px, ok := prices[symbol]; ok && px > 0 {
		return px
	}
	if s != nil {
		if pos := s.Positions[posKey]; pos != nil && pos.AvgCost > 0 {
			return pos.AvgCost
		}
	}
	return 0
}

// runHedgeOrder submits the hedge order. Live mode goes through the same
// side-effecting wrappers every other HL order uses; paper mode synthesizes a
// fill at the hedge mark (there is no exchange to fail, which is exactly why
// paper hedging is a cheap end-to-end regression surface).
//
// Returns (fill, ok). ok=false means NO state may be mutated.
func runHedgeOrder(sc StrategyConfig, snap hedgeSnapshot, action hedgeAction, notifier *MultiNotifier, logger *StrategyLogger) (hedgeFillResult, bool) {
	live := hyperliquidIsLive(sc.Args)
	if !live {
		if snap.HedgePx <= 0 {
			return hedgeFillResult{}, false
		}
		qty := action.Qty
		return hedgeFillResult{
			Qty:         qty,
			Px:          snap.HedgePx,
			Fee:         CalculatePlatformSpotFee(sc.Platform, qty*snap.HedgePx),
			UseFillFee:  false,
			RequestedSz: action.Qty,
		}, true
	}

	direction := directionOpen
	if !action.Kind.hedgeIncreasesExposure() {
		direction = directionClose
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		// Margin mode + leverage are sent only on a FRESH hedge open: HL rejects
		// update_leverage on an open position, so an add must inherit whatever
		// the open pinned (mirrors runHyperliquidExecuteOrder's posQty==0 gate).
		marginMode := ""
		leverage := 0.0
		if action.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			leverage = hedgeExchangeLeverage(sc)
		}
		side := hedgeOrderSideForPositionSide(action.PositionSide)
		// No stop-loss, no TPs, no cancel OIDs: a hedge leg carries no on-chain
		// protection of its own by design (phase-1 constraint 1).
		res, stderr, err := hedgeExecuteFn(sc.Script, snap.HedgeSymbol, side, action.Qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
		if stderr != "" && logger != nil {
			logger.Info("hedge execute stderr: %s", stderr)
		}
		if err != nil {
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, err.Error(), notifier, logger)
			return hedgeFillResult{}, false
		}
		if res == nil || res.Error != "" {
			msg := "hedge execute returned no result"
			if res != nil {
				msg = res.Error
			}
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, msg, notifier, logger)
			return hedgeFillResult{}, false
		}
		if res.Execution == nil || res.Execution.Fill == nil || res.Execution.Fill.TotalSz <= 0 || res.Execution.Fill.AvgPx <= 0 {
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, "hedge order returned no confirmed fill", notifier, logger)
			return hedgeFillResult{}, false
		}
		clearLiveExecThrottle(sc, direction, snap.HedgeSymbol)
		f := res.Execution.Fill
		return hedgeFillResult{
			Qty:         f.TotalSz,
			Px:          f.AvgPx,
			Fee:         f.Fee,
			OID:         f.OID,
			UseFillFee:  true,
			RequestedSz: action.Qty,
		}, true

	case hedgeActionReduce, hedgeActionCloseFull:
		// ALWAYS a sized reduce-only close, never market_close(sz=None). Config
		// validation makes the hedge coin sole-owned among CONFIGURED strategies,
		// but it cannot rule out a position an operator opened by hand on the
		// exchange. Sizing to our own virtual quantity keeps a foreign position
		// untouched (fail closed) instead of liquidating it.
		sz := action.Qty
		if sz > snap.HedgeQty {
			sz = snap.HedgeQty
		}
		if sz <= 0 {
			return hedgeFillResult{}, false
		}
		res, stderr, err := hedgeCloseFn(sc.Script, snap.HedgeSymbol, &sz, nil)
		if stderr != "" && logger != nil {
			logger.Info("hedge close stderr: %s", stderr)
		}
		if err != nil {
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, err.Error(), notifier, logger)
			return hedgeFillResult{}, false
		}
		if res == nil || res.Close == nil {
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, "hedge close returned no close block", notifier, logger)
			return hedgeFillResult{}, false
		}
		clearLiveExecThrottle(sc, direction, snap.HedgeSymbol)
		if res.Close.AlreadyFlat {
			// The exchange says there is nothing left to close. Book the virtual
			// leg out at the mark so state stops diverging from the chain; with
			// no fill there is no real price to use.
			if logger != nil {
				logger.Warn("hedge-sync: %s already flat on-chain — clearing the virtual hedge leg at mark", snap.HedgeSymbol)
			}
			return hedgeFillResult{Qty: snap.HedgeQty, Px: snap.HedgePx, AlreadyFlat: true, RequestedSz: action.Qty}, snap.HedgePx > 0
		}
		if res.Close.Fill == nil || res.Close.Fill.TotalSz <= 0 || res.Close.Fill.AvgPx <= 0 {
			hedgeReportExecFailure(sc, direction, snap.HedgeSymbol, "hedge close returned no confirmed fill", notifier, logger)
			return hedgeFillResult{}, false
		}
		f := res.Close.Fill
		return hedgeFillResult{
			Qty:         f.TotalSz,
			Px:          f.AvgPx,
			Fee:         f.Fee,
			OID:         f.OID,
			UseFillFee:  true,
			RequestedSz: action.Qty,
		}, true
	}
	return hedgeFillResult{}, false
}

// hedgeFillResult is a venue-agnostic confirmed fill, so paper and live share
// one booking path.
type hedgeFillResult struct {
	Qty         float64
	Px          float64
	Fee         float64
	OID         int64
	UseFillFee  bool
	AlreadyFlat bool
	// RequestedSz is what we asked for; a shortfall advances the qty watermark
	// only proportionally so the unfilled remainder is retried next cycle
	// rather than assumed hedged.
	RequestedSz float64
}

func hedgeReportExecFailure(sc StrategyConfig, direction, symbol, msg string, notifier *MultiNotifier, logger *StrategyLogger) {
	if logger != nil {
		logger.Error("hedge %s failed for %s: %s", direction, symbol, msg)
	}
	notifyLiveExecFailure(notifier, sc, direction, symbol, msg)
}

// applyHedgeFill books a confirmed hedge fill. Caller must hold mu.Lock.
//
// The hedge Position is re-read here rather than trusted from the pre-spawn
// snapshot: the fill is real and must be booked against whatever state actually
// exists now, never dropped because state moved.
func applyHedgeFill(sc StrategyConfig, s *StrategyState, snap hedgeSnapshot, action hedgeAction, fill hedgeFillResult, logger *StrategyLogger) {
	if s == nil || fill.Qty <= 0 || snap.HedgeSymbol == "" {
		return
	}
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		applyHedgeEntryFill(sc, s, snap, action, fill, logger)
	case hedgeActionReduce, hedgeActionCloseFull:
		applyHedgeExitFill(s, snap, action, fill, logger)
	}
}

func applyHedgeEntryFill(sc StrategyConfig, s *StrategyState, snap hedgeSnapshot, action hedgeAction, fill hedgeFillResult, logger *StrategyLogger) {
	now := time.Now().UTC()
	sym := snap.HedgeSymbol
	pos := s.Positions[sym]

	// Advance the watermark only in proportion to what actually filled. A
	// partial hedge fill therefore leaves a residual delta that the next cycle
	// re-derives and retries, instead of recording the primary as fully hedged.
	filledFrac := 1.0
	if fill.RequestedSz > 0 && fill.Qty < fill.RequestedSz {
		filledFrac = fill.Qty / fill.RequestedSz
	}
	prevBasis := 0.0
	if pos != nil {
		prevBasis = pos.HedgePrimaryQtyBasis
	}
	if action.Kind == hedgeActionOpen {
		prevBasis = 0
	}
	newBasis := prevBasis + (action.NewBasis-prevBasis)*filledFrac

	fee := fill.Fee
	if !fill.UseFillFee {
		fee = CalculatePlatformSpotFee(sc.Platform, fill.Qty*fill.Px)
	}
	// An entry costs only its fee in this virtual-cash model (perps PnL is
	// booked at close), matching the primary perps convention.
	s.Cash -= fee

	if pos == nil {
		pos = &Position{
			Symbol:               sym,
			Quantity:             fill.Qty,
			InitialQuantity:      fill.Qty,
			AvgCost:              fill.Px,
			Side:                 action.PositionSide,
			Multiplier:           1,
			Leverage:             hedgeExchangeLeverage(sc),
			OwnerStrategyID:      sc.ID,
			OpenedAt:             now,
			HedgeFor:             snap.PrimarySymbol,
			HedgePrimaryQtyBasis: newBasis,
		}
		s.Positions[sym] = pos
	} else {
		// Blend price and size. A hedge has no frozen risk geometry to preserve
		// (no SL, no TP tiers, no EntryATR), so a plain notional blend is exactly
		// right — unlike the #873 primary scale-in, which must freeze its anchor.
		total := pos.Quantity + fill.Qty
		if total > 0 {
			pos.AvgCost = (pos.AvgCost*pos.Quantity + fill.Px*fill.Qty) / total
		}
		pos.Quantity = total
		pos.Side = action.PositionSide
		pos.Multiplier = 1
		pos.HedgeFor = snap.PrimarySymbol
		pos.HedgePrimaryQtyBasis = newBasis
		if pos.OwnerStrategyID == "" {
			pos.OwnerStrategyID = sc.ID
		}
	}

	positionID := ensurePositionTradeID(s.ID, sym, pos)
	side := "buy"
	if action.PositionSide == "short" {
		side = "sell"
	}
	feeSource := FeeSourceModeled
	if fill.UseFillFee {
		feeSource = FeeSourceUserFills
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          sym,
		PositionID:      positionID,
		Side:            side,
		Quantity:        fill.Qty,
		Price:           fill.Px,
		Value:           fill.Qty * fill.Px,
		TradeType:       TradeTypeHedge,
		Details:         fmt.Sprintf("HEDGE(%s) %s %s %.6f @ $%.4f (ratio %.2fx, fee $%.2f)", snap.PrimarySymbol, action.Kind, action.PositionSide, fill.Qty, fill.Px, hedgeRatio(sc), fee),
		ExchangeOrderID: exchangeOrderIDForTrade(fmt.Sprintf("%d", fill.OID), fill.OID > 0),
		ExchangeFee:     fee,
		PnLGross:        true,
		FeeSource:       feeSource,
		Regime:          s.Regime,
	}
	RecordTrade(s, trade)
	if logger != nil {
		logger.Warn("hedge %s: %s %s %.6f @ $%.4f (basis %.6f %s, fee $%.2f)",
			action.Kind, action.PositionSide, sym, fill.Qty, fill.Px, newBasis, snap.PrimarySymbol, fee)
	}
}

func applyHedgeExitFill(s *StrategyState, snap hedgeSnapshot, action hedgeAction, fill hedgeFillResult, logger *StrategyLogger) {
	sym := snap.HedgeSymbol
	pos := s.Positions[sym]
	if pos == nil {
		// The leg vanished between spawn and apply (e.g. reconcile booked an
		// external close in the same cycle). The fill is already reflected;
		// nothing to book — booking again would double-count.
		if logger != nil {
			logger.Info("hedge-sync: hedge leg on %s already gone at apply time — nothing to book", sym)
		}
		return
	}
	oid := ""
	if fill.OID > 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	reason := "hedge_reduce"
	if action.Kind == hedgeActionCloseFull {
		reason = "hedge_close"
	}
	if fill.AlreadyFlat {
		reason += "_already_flat"
	}
	qty := fill.Qty
	if qty >= pos.Quantity || action.Kind == hedgeActionCloseFull {
		if bookPerpsCloseWithFillFee(s, sym, fill.Px, fill.Fee, fill.UseFillFee, oid, reason, "Hedge close", "Hedge close", logger) {
			return
		}
		return
	}
	// Partial reduce: the residual leg keeps hedging the residual primary, so
	// re-point its watermark at the primary quantity this reduction targeted.
	if bookPerpsPartialCloseWithFillFee(s, sym, qty, fill.Px, fill.Fee, fill.UseFillFee, oid, reason, "Hedge reduce", "Hedge reduce", logger) {
		if remaining := s.Positions[sym]; remaining != nil {
			// Only credit the fraction that actually filled (see applyHedgeEntryFill).
			filledFrac := 1.0
			if fill.RequestedSz > 0 && fill.Qty < fill.RequestedSz {
				filledFrac = fill.Qty / fill.RequestedSz
			}
			prev := snap.HedgeBasis
			if prev <= 0 {
				prev = snap.PrimaryQty
			}
			remaining.HedgePrimaryQtyBasis = prev + (action.NewBasis-prev)*filledFrac
		}
	}
}

// ---------------------------------------------------------------------------
// Fail-closed handling
// ---------------------------------------------------------------------------

// hedgeHandleOrderFailure applies phase-1 constraint 4: a primary that the
// hedge could not cover must not keep running.
//
// The unwind is scoped to the UNHEDGED SLICE, which makes one rule cover both
// escalation shapes precisely:
//
//	open failed  → basis is 0, so the whole primary is unwound;
//	add  failed  → basis is the already-hedged quantity, so only the add leg is
//	               unwound and the original hedged position rides on untouched.
//
// A failed REDUCE or CLOSE is not escalated: the hedge is over-sized, not
// missing, which is risk-reducing relative to the primary. State is unchanged,
// so the next cycle re-derives the same reduce and retries — bounded, because
// the order is reduce-only.
func hedgeHandleOrderFailure(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	snap hedgeSnapshot,
	action hedgeAction,
	prices map[string]float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) {
	if !action.Kind.hedgeIncreasesExposure() {
		if logger != nil {
			logger.Error("CRITICAL: hedge %s on %s failed — hedge remains over-sized vs the primary; retrying next cycle (reduce-only, bounded)",
				action.Kind, snap.HedgeSymbol)
		}
		hedgeNotifyCritical(notifier, fmt.Sprintf(
			"**HEDGE %s FAILED** [%s] %s — the hedge leg is larger than the primary it covers. It is reduce-only and will retry every cycle.",
			action.Kind.String(), sc.ID, snap.HedgeSymbol))
		return
	}
	count, firstHold := globalHedgeFailures.recordFailure(sc.ID)
	if logger != nil {
		logger.Error("CRITICAL: hedge %s on %s failed (consecutive failures: %d) — unwinding the unhedged primary slice (#1159 fail-closed)",
			action.Kind, snap.HedgeSymbol, count)
	}
	unwindUnhedgedPrimary(sc, s, mu, snap, prices, notifier, logger)
	if firstHold {
		hedgeNotifyCritical(notifier, fmt.Sprintf(
			"**HEDGE ENTRY HOLD** [%s] %d consecutive hedge-leg failures on %s — new entries are held until a hedge opens successfully. Existing positions keep managing and closing normally.",
			sc.ID, count, snap.HedgeSymbol))
	}
}

// hedgeHandleBlockedAction alerts on a decision that wanted to act but could
// not. An increase that cannot even be SIZED (no usable mark) leaves the
// primary just as unhedged as a rejected order, so it escalates identically.
func hedgeHandleBlockedAction(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	snap hedgeSnapshot,
	action hedgeAction,
	prices map[string]float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) {
	unhedged := snap.PrimaryQty > 0 && (snap.HedgeQty <= 0 || snap.PrimaryQty > snap.HedgeBasis)
	if !unhedged {
		if logger != nil {
			logger.Warn("hedge-sync: %s", action.Reason)
		}
		return
	}
	if logger != nil {
		logger.Error("CRITICAL: hedge cannot be sized for %s — %s; unwinding the unhedged primary slice (#1159 fail-closed)", snap.PrimarySymbol, action.Reason)
	}
	globalHedgeFailures.recordFailure(sc.ID)
	unwindUnhedgedPrimary(sc, s, mu, snap, prices, notifier, logger)
}

// unwindUnhedgedPrimary closes, reduce-only, the slice of the primary position
// that no hedge covers.
//
// Always a SIZED close, never market_close(sz=None): unlike the hedge coin, the
// PRIMARY coin may legitimately be shared with peer strategies on the same
// wallet, and a full close would flatten their exposure too.
//
// When the whole position is being unwound, the protection OIDs are cancelled
// AFTER the close fills (RunHyperliquidCloseCancelAfterFill) so a failed close
// can never leave the position naked. A partial unwind deliberately leaves the
// resting reduce-only SL in place — it still protects the remainder, exactly
// like the partial-close convention in runHyperliquidExecuteOrder.
func unwindUnhedgedPrimary(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	snap hedgeSnapshot,
	prices map[string]float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) {
	sym := snap.PrimarySymbol
	if sym == "" || snap.PrimaryQty <= 0 {
		return
	}
	hedgedQty := 0.0
	if snap.HedgeQty > 0 && snap.HedgeBasis > 0 {
		hedgedQty = snap.HedgeBasis
	}
	unwindQty := snap.PrimaryQty - hedgedQty
	if unwindQty <= 0 {
		return
	}
	full := hedgedQty <= 0

	mu.RLock()
	var cancelOIDs []int64
	if pos := s.Positions[sym]; pos != nil && full {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()

	var fillPx, fillFee float64
	useFillFee := false
	oid := ""

	if hyperliquidIsLive(sc.Args) {
		sz := unwindQty
		var res *HyperliquidCloseResult
		var err error
		var stderr string
		if full {
			res, stderr, err = hedgeCloseCancelAfterFilFn(sc.Script, sym, &sz, cancelOIDs)
		} else {
			res, stderr, err = hedgeCloseFn(sc.Script, sym, &sz, nil)
		}
		if stderr != "" && logger != nil {
			logger.Info("hedge unwind stderr: %s", stderr)
		}
		if err != nil || res == nil || res.Close == nil {
			msg := "unwind returned no close block"
			if err != nil {
				msg = err.Error()
			}
			if logger != nil {
				logger.Error("CRITICAL: unhedged-primary unwind of %.6f %s FAILED: %s — position is running UNHEDGED; hedge sync retries next cycle", unwindQty, sym, msg)
			}
			hedgeNotifyCritical(notifier, fmt.Sprintf(
				"**UNHEDGED PRIMARY — UNWIND FAILED** [%s] could not close %.6f %s after the hedge leg failed: %s. The position is running WITHOUT its hedge. The scheduler retries every cycle; intervene if this persists.",
				sc.ID, unwindQty, sym, msg))
			return
		}
		if res.Close.AlreadyFlat {
			if logger != nil {
				logger.Warn("hedge unwind: %s already flat on-chain", sym)
			}
		} else if res.Close.Fill != nil && res.Close.Fill.TotalSz > 0 && res.Close.Fill.AvgPx > 0 {
			fillPx = res.Close.Fill.AvgPx
			fillFee = res.Close.Fill.Fee
			useFillFee = true
			if res.Close.Fill.OID > 0 {
				oid = fmt.Sprintf("%d", res.Close.Fill.OID)
			}
			if res.Close.Fill.TotalSz < unwindQty {
				unwindQty = res.Close.Fill.TotalSz
			}
		}
	}
	if fillPx <= 0 {
		fillPx = hedgeMarkFor(prices, sym, s, sym)
	}
	if fillPx <= 0 {
		if logger != nil {
			logger.Error("CRITICAL: unhedged-primary unwind of %s has no usable close price — virtual state left untouched, retrying next cycle", sym)
		}
		hedgeNotifyCritical(notifier, fmt.Sprintf(
			"**UNHEDGED PRIMARY — CANNOT BOOK UNWIND** [%s] no usable mark for %s. Virtual state is unchanged and will be retried.", sc.ID, sym))
		return
	}

	mu.Lock()
	booked := false
	if pos := s.Positions[sym]; pos != nil {
		if full || unwindQty >= pos.Quantity {
			booked = bookPerpsCloseWithFillFee(s, sym, fillPx, fillFee, useFillFee, oid,
				"hedge_unwind_unhedged", "Unhedged-primary unwind (hedge leg unavailable)", "Unhedged-primary unwind", logger)
		} else {
			booked = bookPerpsPartialCloseWithFillFee(s, sym, unwindQty, fillPx, fillFee, useFillFee, oid,
				"hedge_unwind_unhedged", "Unhedged-primary unwind (hedge add unavailable)", "Unhedged-primary unwind", logger)
		}
	}
	mu.Unlock()

	if logger != nil {
		logger.Warn("hedge fail-closed: unwound %.6f %s @ $%.4f (booked=%t) because the hedge leg could not be established", unwindQty, sym, fillPx, booked)
	}
	hedgeNotifyCritical(notifier, fmt.Sprintf(
		"**HEDGE FAILED — PRIMARY UNWOUND** [%s] the correlated hedge on %s could not be opened, so %.6f %s was closed reduce-only @ $%.4f to avoid running unhedged (#1159 fail-closed).",
		sc.ID, snap.HedgeSymbol, unwindQty, sym, fillPx))
}

// hedgeNotifyCritical sends an operator DM plus a channel post. Must be called
// with NO lock held (notifier calls do blocking HTTP).
func hedgeNotifyCritical(notifier *MultiNotifier, msg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendOwnerDM(msg)
	notifier.SendToAllChannels(msg)
}

// ---------------------------------------------------------------------------
// Cycle sweep
// ---------------------------------------------------------------------------

// runHedgeSweep converges every hedge-enabled strategy — and every strategy
// still holding a hedge leg, whether or not its config still declares one —
// that the per-strategy dispatch did not already handle this cycle.
//
// This is what makes hedge coupling hold across the paths the dispatch never
// sees: an on-chain SL or TP fill booked by reconcile, a portfolio kill switch,
// a circuit-breaker force-close, a manual force-close, an externally closed
// leg, or a crash between the two legs. `handled` carries the strategy IDs the
// dispatch already synced, so a fresh open whose hedge just failed (and whose
// primary was therefore just unwound) is not immediately re-attempted in the
// same cycle.
//
// Must be called WITHOUT holding mu.
func runHedgeSweep(
	strategies []StrategyConfig,
	state *AppState,
	mu *sync.RWMutex,
	logMgr *LogManager,
	prices map[string]float64,
	handled map[string]bool,
	notifier *MultiNotifier,
) {
	if state == nil || mu == nil {
		return
	}
	// Deterministic order: hedge orders are operator-visible side effects.
	ordered := make([]StrategyConfig, 0, len(strategies))
	for _, sc := range strategies {
		if handled[sc.ID] {
			continue
		}
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			continue
		}
		mu.RLock()
		ss := state.Strategies[sc.ID]
		holdsHedge := false
		if ss != nil {
			if pos, _ := hedgePositionOf(ss); pos != nil {
				holdsHedge = true
			}
		}
		mu.RUnlock()
		if !HedgeEnabled(sc) && !holdsHedge {
			continue
		}
		ordered = append(ordered, sc)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	for _, sc := range ordered {
		mu.RLock()
		ss := state.Strategies[sc.ID]
		mu.RUnlock()
		if ss == nil {
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hedge-sweep: logger for %s: %v\n", sc.ID, err)
			continue
		}
		runHedgeSync(sc, ss, mu, prices, notifier, logger)
	}
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

// reconcileHedgeCoins resyncs every held correlated hedge leg against on-chain
// state, and reports foreign positions sitting on a declared-but-unheld hedge
// coin. Caller must hold mu.Lock (it is invoked from inside
// reconcileHyperliquidAccountPositions' critical section).
//
// Two distinct cases, deliberately handled differently:
//
//   - a hedge leg IS held → run the standard reconciler on the hedge coin, so
//     drift resyncs and an external close books at the real userFills VWAP.
//     The hedge reconciler then re-opens or unwinds on the next cycle.
//   - a hedge coin is declared but NO hedge leg is held while an on-chain
//     position exists there → do nothing but alert. Adopting it would book a
//     guessed fill for a position we never opened (acceptance criterion 3
//     forbids exactly that), and closing it would liquidate an operator's
//     manual trade.
func reconcileHedgeCoins(
	allStrategies []StrategyConfig,
	state *AppState,
	logMgr *LogManager,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	pendingAlerts *[]ProtectionFillAlert,
	changed *bool,
) {
	if state == nil {
		return
	}
	ordered := make([]StrategyConfig, 0, len(allStrategies))
	for _, sc := range allStrategies {
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			continue
		}
		ordered = append(ordered, sc)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	for _, sc := range ordered {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		heldPos, heldCoin := hedgePositionOf(ss)
		if heldPos == nil {
			if HedgeEnabled(sc) {
				reportForeignHedgeCoinPosition(sc, positions, logMgr)
			}
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", sc.ID, err)
			continue
		}
		if reconcileHyperliquidPositionsWithResolver(ss, heldCoin, positions, resolveFee, logger, pendingAlerts, nil, StrategyConfig{}) {
			if changed != nil {
				*changed = true
			}
		}
	}
}

// reportForeignHedgeCoinPosition warns (throttled by the shared live-exec alert
// throttle) when a position exists on a strategy's declared hedge coin but no
// hedge leg is recorded in state. Alerting only — never books, guesses, or
// mutates. Mirrors the hl_reconcile_gap_alerts.go stance.
func reportForeignHedgeCoinPosition(sc StrategyConfig, positions []HLPosition, logMgr *LogManager) {
	coin := hedgeCoin(sc)
	if coin == "" {
		return
	}
	for i := range positions {
		if positions[i].Coin != coin || positions[i].Size == 0 {
			continue
		}
		if logger, err := logMgr.GetStrategyLogger(sc.ID); err == nil && logger != nil {
			logger.Warn("hl-sync: on-chain position of %.6f on declared hedge coin %s with NO recorded hedge leg — NOT adopting (ownership comes from persisted metadata only, #1159). Reconcile it manually or flatten it.",
				positions[i].Size, coin)
		}
		return
	}
}

// ---------------------------------------------------------------------------
// Startup consistency
// ---------------------------------------------------------------------------

// ValidateHedgeStateConsistency reports persisted hedge legs that the current
// config no longer authorizes (#1159). A config edit plus a process restart
// bypasses the SIGHUP guard that blocks hedge-block changes while open, so this
// runs once at boot beside ValidatePerpsDirectionConfig.
//
// It WARNS rather than refusing to start, on purpose. Refusing to boot would
// leave live positions completely unmanaged — no stop-loss walker, no
// protection sync, no reconcile — which is strictly worse than the drift it
// would be protecting against. The hedge reconciler converges the stale leg
// deterministically (it unwinds it) on the first cycle, and this warning tells
// the operator that is about to happen.
func ValidateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var warnings []string
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		pos, coin := hedgePositionOf(ss)
		sc, configured := byID[id]
		if pos == nil {
			// A hedge is configured and enabled but nothing is held: normal when
			// the primary is flat, and the reconciler opens one otherwise. Only
			// flag the genuinely surprising case.
			if configured && HedgeEnabled(sc) {
				if primary := ss.Positions[hyperliquidConfiguredCoin(sc)]; primary != nil && primary.Quantity > 0 {
					warnings = append(warnings, fmt.Sprintf(
						"strategy %s holds an open %s position with NO hedge leg despite hedge.enabled — the hedge reconciler will open the %s hedge on the first cycle, or close the primary reduce-only if it cannot (#1159 fail-closed)",
						id, hyperliquidConfiguredCoin(sc), hedgeCoin(sc)))
				}
			}
			continue
		}
		switch {
		case !configured:
			warnings = append(warnings, fmt.Sprintf(
				"strategy %s is no longer in the config but still holds a %s hedge leg (%.6f %s) — it cannot be managed; close it manually",
				id, coin, pos.Quantity, pos.Side))
		case !HedgeEnabled(sc):
			warnings = append(warnings, fmt.Sprintf(
				"strategy %s holds a %s hedge leg (%.6f %s) but its hedge block is now absent/disabled — the reconciler will UNWIND the hedge on the first cycle, leaving the primary unhedged as configured (#1159)",
				id, coin, pos.Quantity, pos.Side))
		case hedgeCoin(sc) != coin:
			warnings = append(warnings, fmt.Sprintf(
				"strategy %s holds a %s hedge leg (%.6f %s) but hedge.symbol is now %s — the reconciler will unwind the stale leg, then open the configured one (#1159)",
				id, coin, pos.Quantity, pos.Side, hedgeCoin(sc)))
		case pos.HedgeFor != hyperliquidConfiguredCoin(sc):
			warnings = append(warnings, fmt.Sprintf(
				"strategy %s hedge leg on %s is stamped for primary %q but the strategy now trades %q — hedge sizing will re-base on the first cycle (#1159)",
				id, coin, pos.HedgeFor, hyperliquidConfiguredCoin(sc)))
		}
	}
	return warnings
}
