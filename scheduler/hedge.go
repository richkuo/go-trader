package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// #1159 — per-strategy correlated hedge legs (phase 1, HL perps).
//
// Hedge management is a per-cycle, state-derived reconciler ("hedge sync").
// hedgeTargetDecision computes the single action that converges the hedge leg
// to the CURRENT primary position versus Position.HedgePrimaryQtyBasis.
// runHedgeSync runs that decision every HL perps dispatch cycle so every
// primary lifecycle event produces the matching hedge action without
// per-event mirror hooks.
//
// Invariants: qty-event mirroring (never mark re-trades); fill-confirmed state
// mutation only; fail-closed fresh open (unwind primary if hedge open fails);
// sole ownership by collision validation.

const hedgeMinOrderNotionalUSD = 10.0
const hedgeQtyEpsilon = 1e-9

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
	default:
		return "none"
	}
}

type hedgeSnapshot struct {
	PrimaryQty  float64
	PrimarySide string
	HedgeQty    float64
	HedgeSide   string
	HedgeBasis  float64
}

type hedgeAction struct {
	Kind        hedgeActionKind
	Qty         float64
	Side        string
	Reason      string
	FailClosed  bool
	TargetBasis float64
}

func hedgeSide(sc StrategyConfig) string {
	if sc.Hedge == nil || strings.TrimSpace(sc.Hedge.Side) == "" {
		return "inverse"
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.Side))
}

func hedgeInverseOrderSide(primarySide string) string {
	if primarySide == "long" {
		return "sell"
	}
	return "buy"
}

func hedgeInversePositionSide(primarySide string) string {
	if primarySide == "long" {
		return "short"
	}
	return "long"
}

func hedgeReduceOrderSide(hedgeSide string) string {
	if hedgeSide == "short" {
		return "buy"
	}
	return "sell"
}

func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeActionNone}
	}
	primaryFlat := snap.PrimaryQty <= hedgeQtyEpsilon
	hedgeHeld := snap.HedgeQty > hedgeQtyEpsilon

	if primaryFlat {
		if hedgeHeld {
			return hedgeAction{
				Kind:   hedgeActionCloseFull,
				Qty:    snap.HedgeQty,
				Side:   hedgeReduceOrderSide(snap.HedgeSide),
				Reason: "primary flat",
			}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}

	ratio := hedgeRatio(sc)
	wantSide := hedgeInversePositionSide(snap.PrimarySide)
	if hedgeHeld && snap.HedgeSide != "" && snap.HedgeSide != wantSide {
		return hedgeAction{
			Kind:   hedgeActionCloseFull,
			Qty:    snap.HedgeQty,
			Side:   hedgeReduceOrderSide(snap.HedgeSide),
			Reason: fmt.Sprintf("hedge side %q opposes required %q", snap.HedgeSide, wantSide),
		}
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, FailClosed: true, Reason: "hedge/primary mark unavailable"}
	}
	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= 0 {
			return hedgeAction{Kind: hedgeActionNone, FailClosed: true, Reason: "computed hedge open qty <= 0"}
		}
		return hedgeAction{
			Kind:        hedgeActionOpen,
			Qty:         qty,
			Side:        hedgeInverseOrderSide(snap.PrimarySide),
			Reason:      "primary held, hedge flat",
			TargetBasis: snap.PrimaryQty,
		}
	}
	if snap.PrimaryQty > snap.HedgeBasis+hedgeQtyEpsilon {
		delta := snap.PrimaryQty - snap.HedgeBasis
		addQty := delta * primaryPx * ratio / hedgePx
		if addQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge add below min notional — deferring"}
		}
		return hedgeAction{
			Kind:        hedgeActionAdd,
			Qty:         addQty,
			Side:        hedgeInverseOrderSide(snap.PrimarySide),
			Reason:      "primary increased",
			TargetBasis: snap.PrimaryQty,
		}
	}
	if snap.HedgeBasis > 0 && snap.PrimaryQty < snap.HedgeBasis-hedgeQtyEpsilon {
		frac := (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if frac > 1 {
			frac = 1
		}
		reduceQty := snap.HedgeQty * frac
		if reduceQty >= snap.HedgeQty-hedgeQtyEpsilon {
			return hedgeAction{
				Kind:        hedgeActionCloseFull,
				Qty:         snap.HedgeQty,
				Side:        hedgeReduceOrderSide(snap.HedgeSide),
				Reason:      "primary reduced — full hedge close",
				TargetBasis: snap.PrimaryQty,
			}
		}
		if reduceQty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge reduce below min notional — deferring"}
		}
		return hedgeAction{
			Kind:        hedgeActionReduce,
			Qty:         reduceQty,
			Side:        hedgeReduceOrderSide(snap.HedgeSide),
			Reason:      "primary reduced",
			TargetBasis: snap.PrimaryQty,
		}
	}
	return hedgeAction{Kind: hedgeActionNone, Reason: "hedge already aligned"}
}

func hedgeOrderSkipReason(sc StrategyConfig, snap hedgeSnapshot, action hedgeAction, primaryPx, hedgePx float64) string {
	if !sc.HedgeEnabled() {
		return "hedge not enabled"
	}
	if action.Kind == hedgeActionNone {
		return "no hedge action"
	}
	if action.Qty <= 0 {
		return "hedge order qty <= 0"
	}
	if hedgeCoin(sc) == "" {
		return "hedge coin empty"
	}
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		if primaryPx <= 0 || hedgePx <= 0 {
			return "hedge marks unavailable"
		}
		if snap.PrimaryQty <= hedgeQtyEpsilon {
			return "primary flat — refuse hedge open/add"
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if snap.HedgeQty <= hedgeQtyEpsilon {
			return "hedge already flat"
		}
	}
	return ""
}

func hedgeSnapshotForStrategy(s *StrategyState, sc StrategyConfig) hedgeSnapshot {
	var snap hedgeSnapshot
	if s == nil {
		return snap
	}
	primary := hyperliquidConfiguredCoin(sc)
	if primary == "" {
		primary = hyperliquidSymbol(sc.Args)
	}
	if pos := s.Positions[primary]; pos != nil && pos.Quantity > 0 {
		snap.PrimaryQty = pos.Quantity
		snap.PrimarySide = pos.Side
	}
	if hc := hedgeCoin(sc); hc != "" {
		if hpos := s.Positions[hc]; hpos != nil && hpos.HedgeFor != "" && hpos.Quantity > 0 {
			snap.HedgeQty = hpos.Quantity
			snap.HedgeSide = hpos.Side
			snap.HedgeBasis = hpos.HedgePrimaryQtyBasis
		}
	}
	return snap
}

// runHedgeSync is the single mirror choke point. Lock discipline mirrors
// runHyperliquidProtectionSync: snapshot under RLock, spawn unlocked, apply under Lock.
func runHedgeSync(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	prices map[string]float64,
	hlSnap hlExecuteSnapshot,
	freshPrimaryOpen bool,
	primaryOpenFillQty float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	if s == nil || mu == nil || !sc.HedgeEnabled() {
		return 0
	}
	primaryCoin := hyperliquidConfiguredCoin(sc)
	if primaryCoin == "" {
		primaryCoin = hyperliquidSymbol(sc.Args)
	}
	hc := hedgeCoin(sc)
	if primaryCoin == "" || hc == "" {
		return 0
	}
	primaryPx := prices[primaryCoin]
	hedgePx := prices[hc]

	mu.RLock()
	snap := hedgeSnapshotForStrategy(s, sc)
	mu.RUnlock()

	decision := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if decision.Kind == hedgeActionNone {
		if decision.FailClosed && snap.PrimaryQty > 0 && snap.HedgeQty <= hedgeQtyEpsilon {
			if freshPrimaryOpen && primaryOpenFillQty > 0 {
				return unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryOpenFillQty, prices, decision.Reason, notifier, logger)
			}
			hedgeAlert(notifier, logger, "hedge %s for %s: %s — retry next cycle", hc, sc.ID, decision.Reason)
		} else if decision.Reason != "" && logger != nil {
			logger.Info("hedge %s: %s", hc, decision.Reason)
		}
		return 0
	}
	if skip := hedgeOrderSkipReason(sc, snap, decision, primaryPx, hedgePx); skip != "" {
		if logger != nil {
			logger.Info("hedge sync skip (%s): %s", hc, skip)
		}
		return 0
	}

	live := hyperliquidIsLive(sc.Args)
	switch decision.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		return runHedgeOpenOrAdd(sc, s, mu, decision, snap, hc, primaryCoin, hedgePx, prices, hlSnap, live, freshPrimaryOpen, primaryOpenFillQty, notifier, logger)
	case hedgeActionReduce, hedgeActionCloseFull:
		return runHedgeReduceOrClose(sc, s, mu, decision, snap, hc, live, notifier, logger)
	}
	return 0
}

func runHedgeOpenOrAdd(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	decision hedgeAction,
	snap hedgeSnapshot,
	hc, primaryCoin string,
	hedgePx float64,
	prices map[string]float64,
	hlSnap hlExecuteSnapshot,
	live, freshPrimaryOpen bool,
	primaryOpenFillQty float64,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	dir := "hedge:" + decision.Kind.String()
	var fillPx, filledQty, fillFee float64
	var fillOID int64
	useFillFee := false

	if live {
		marginMode := ""
		lev := 0.0
		execSnap := hlSnap
		if decision.Kind == hedgeActionOpen {
			marginMode = hedgeMarginMode(sc)
			lev = hedgeLeverage(sc)
			execSnap = hlExecuteSnapshot{} // fresh hedge coin
		}
		exec, _, err := RunHyperliquidExecute(sc.Script, hc, decision.Side, decision.Qty, 0, 0, 0, marginMode, lev, false, execSnap)
		if err != nil || exec == nil || exec.Error != "" || exec.Execution == nil || exec.Execution.Fill == nil || exec.Execution.Fill.TotalSz <= 0 {
			msg := "hedge execute failed"
			if err != nil {
				msg = err.Error()
			} else if exec != nil && exec.Error != "" {
				msg = exec.Error
			} else {
				msg = "no fill"
			}
			notifyLiveExecFailure(notifier, sc, dir, hc, msg)
			if freshPrimaryOpen && decision.Kind == hedgeActionOpen && primaryOpenFillQty > 0 {
				return unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryOpenFillQty, prices, msg, notifier, logger)
			}
			hedgeAlert(notifier, logger, "CRITICAL: hedge %s %s failed for %s: %s", hc, decision.Kind.String(), sc.ID, msg)
			return 0
		}
		clearLiveExecThrottle(sc, dir, hc)
		f := exec.Execution.Fill
		fillPx, filledQty, fillFee, fillOID = f.AvgPx, f.TotalSz, f.Fee, f.OID
		useFillFee = true
	} else {
		if hedgePx <= 0 {
			if freshPrimaryOpen && decision.Kind == hedgeActionOpen && primaryOpenFillQty > 0 {
				return unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, primaryCoin, primaryOpenFillQty, prices, "paper hedge mark unavailable", notifier, logger)
			}
			return 0
		}
		fillPx = hedgePx
		filledQty = decision.Qty
		fillFee = CalculatePlatformSpotFee("hyperliquid", filledQty*fillPx)
	}

	mu.Lock()
	defer mu.Unlock()
	if !applyHedgeOpenOrAddFill(sc, s, decision, snap, hc, primaryCoin, fillPx, filledQty, fillFee, fillOID, useFillFee, logger) {
		return 0
	}
	return 1
}

func runHedgeReduceOrClose(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	decision hedgeAction,
	snap hedgeSnapshot,
	hc string,
	live bool,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	dir := "hedge:" + decision.Kind.String()
	var fillPx, filledQty, fillFee float64
	var fillOID int64
	useFillFee := false

	if live {
		var partial *float64
		if decision.Kind == hedgeActionReduce {
			q := decision.Qty
			partial = &q
		}
		closeOut, _, err := RunHyperliquidClose(sc.Script, hc, partial, nil)
		if err != nil || closeOut == nil || closeOut.Error != "" || closeOut.Close == nil || closeOut.Close.Fill == nil || closeOut.Close.Fill.TotalSz <= 0 {
			msg := "hedge close failed"
			if err != nil {
				msg = err.Error()
			} else if closeOut != nil && closeOut.Error != "" {
				msg = closeOut.Error
			} else {
				msg = "no fill"
			}
			notifyLiveExecFailure(notifier, sc, dir, hc, msg)
			hedgeAlert(notifier, logger, "CRITICAL: hedge %s %s failed for %s: %s", hc, decision.Kind.String(), sc.ID, msg)
			return 0
		}
		clearLiveExecThrottle(sc, dir, hc)
		f := closeOut.Close.Fill
		fillPx, filledQty, fillFee, fillOID = f.AvgPx, f.TotalSz, f.Fee, f.OID
		useFillFee = true
	} else {
		mu.RLock()
		hpos := s.Positions[hc]
		mark := 0.0
		if hpos != nil {
			mark = hpos.AvgCost
		}
		mu.RUnlock()
		if mark <= 0 {
			return 0
		}
		fillPx = mark
		filledQty = decision.Qty
		if decision.Kind == hedgeActionCloseFull {
			filledQty = snap.HedgeQty
		}
		fillFee = CalculatePlatformSpotFee("hyperliquid", filledQty*fillPx)
	}

	mu.Lock()
	defer mu.Unlock()
	oidStr := ""
	if fillOID > 0 {
		oidStr = fmt.Sprintf("%d", fillOID)
	}
	prefix := fmt.Sprintf("hedge(%s)", hyperliquidConfiguredCoin(sc))
	ok := false
	if decision.Kind == hedgeActionCloseFull || filledQty >= snap.HedgeQty-hedgeQtyEpsilon {
		ok = bookPerpsCloseWithFillFee(s, hc, fillPx, fillFee, useFillFee, oidStr, "hedge_close", prefix, "hedge", logger)
	} else {
		ok = bookPerpsPartialCloseWithFillFee(s, hc, filledQty, fillPx, fillFee, useFillFee, oidStr, "hedge_reduce", prefix, "hedge", logger)
		if ok {
			if hpos := s.Positions[hc]; hpos != nil {
				if decision.Qty > 0 && filledQty < decision.Qty-hedgeQtyEpsilon {
					delta := decision.TargetBasis - hpos.HedgePrimaryQtyBasis
					hpos.HedgePrimaryQtyBasis += delta * (filledQty / decision.Qty)
				} else {
					hpos.HedgePrimaryQtyBasis = decision.TargetBasis
				}
			}
		}
	}
	if ok {
		return 1
	}
	return 0
}

func applyHedgeOpenOrAddFill(
	sc StrategyConfig,
	s *StrategyState,
	decision hedgeAction,
	snap hedgeSnapshot,
	hc, primaryCoin string,
	fillPx, filledQty, fillFee float64,
	fillOID int64,
	useFillFee bool,
	logger *StrategyLogger,
) bool {
	if filledQty <= 0 || fillPx <= 0 {
		return false
	}
	now := time.Now().UTC()
	basis := decision.TargetBasis
	if decision.Qty > 0 && filledQty < decision.Qty-hedgeQtyEpsilon {
		delta := decision.TargetBasis - snap.HedgeBasis
		if decision.Kind == hedgeActionOpen {
			basis = decision.TargetBasis * (filledQty / decision.Qty)
		} else {
			basis = snap.HedgeBasis + delta*(filledQty/decision.Qty)
		}
	}
	pos := s.Positions[hc]
	if pos == nil || pos.HedgeFor == "" {
		pos = &Position{
			Symbol:               hc,
			Quantity:             filledQty,
			InitialQuantity:      filledQty,
			AvgCost:              fillPx,
			Side:                 hedgeInversePositionSide(snap.PrimarySide),
			Multiplier:           1,
			Leverage:             hedgeLeverage(sc),
			OwnerStrategyID:      sc.ID,
			OpenedAt:             now,
			HedgeFor:             primaryCoin,
			HedgePrimaryQtyBasis: basis,
			TradePositionID:      newTradePositionID(sc.ID, hc, now),
		}
		s.Positions[hc] = pos
	} else {
		total := pos.Quantity + filledQty
		if total > 0 {
			pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*filledQty) / total
		}
		pos.Quantity = total
		pos.HedgePrimaryQtyBasis = basis
	}
	oidStr := ""
	if fillOID > 0 {
		oidStr = fmt.Sprintf("%d", fillOID)
	}
	fee := fillFee
	if !useFillFee {
		fee = CalculatePlatformSpotFee("hyperliquid", filledQty*fillPx)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      sc.ID,
		Symbol:          hc,
		Side:            decision.Side,
		Quantity:        filledQty,
		Price:           fillPx,
		Value:           filledQty * fillPx,
		TradeType:       "hedge",
		Details:         fmt.Sprintf("hedge(%s) %s", primaryCoin, decision.Kind.String()),
		PositionID:      pos.TradePositionID,
		ExchangeOrderID: oidStr,
		ExchangeFee:     fee,
		FeeSource:       "modeled",
	}
	if useFillFee {
		trade.FeeSource = "userfills"
	}
	s.Cash -= fee
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("hedge %s booked %s %.6f @ %.4f (basis=%.6f)", decision.Kind.String(), hc, filledQty, fillPx, pos.HedgePrimaryQtyBasis)
	}
	return true
}

func unwindPrimaryAfterHedgeOpenFailure(
	sc StrategyConfig,
	s *StrategyState,
	mu *sync.RWMutex,
	primaryCoin string,
	fillQty float64,
	prices map[string]float64,
	reason string,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) int {
	if fillQty <= 0 || s == nil || mu == nil || primaryCoin == "" {
		return 0
	}
	live := hyperliquidIsLive(sc.Args)
	var cancelOIDs []int64
	mu.RLock()
	if pos := s.Positions[primaryCoin]; pos != nil {
		cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
	}
	mu.RUnlock()

	var closePx, closeQty, closeFee float64
	var oid int64
	ok := false
	if live {
		partial := fillQty
		closeOut, _, err := RunHyperliquidClose(sc.Script, primaryCoin, &partial, cancelOIDs)
		if err == nil && closeOut != nil && closeOut.Error == "" && closeOut.Close != nil && closeOut.Close.Fill != nil && closeOut.Close.Fill.TotalSz > 0 {
			f := closeOut.Close.Fill
			closePx, closeQty, closeFee, oid, ok = f.AvgPx, f.TotalSz, f.Fee, f.OID, true
		}
	} else {
		px := prices[primaryCoin]
		if px <= 0 {
			mu.RLock()
			if pos := s.Positions[primaryCoin]; pos != nil {
				px = pos.AvgCost
			}
			mu.RUnlock()
		}
		if px > 0 {
			closePx, closeQty, closeFee, ok = px, fillQty, CalculatePlatformSpotFee("hyperliquid", fillQty*px), true
		}
	}

	msg := fmt.Sprintf("CRITICAL: hedge open failed for strategy %s (%s) — unwound primary %s qty=%.6f", sc.ID, reason, primaryCoin, fillQty)
	if !ok {
		msg = fmt.Sprintf("CRITICAL: hedge open failed for strategy %s (%s) AND primary unwind failed — %s may be unhedged; hedge sync will retry", sc.ID, reason, primaryCoin)
		hedgeAlert(notifier, logger, "%s", msg)
		return 0
	}

	mu.Lock()
	oidStr := ""
	if oid > 0 {
		oidStr = fmt.Sprintf("%d", oid)
	}
	booked := false
	if pos := s.Positions[primaryCoin]; pos != nil && pos.Quantity > closeQty+hedgeQtyEpsilon {
		booked = bookPerpsPartialCloseWithFillFee(s, primaryCoin, closeQty, closePx, closeFee, live, oidStr, "hedge_open_failed_unwind", "hedge_open_failed_unwind", "hedge", logger)
	} else {
		booked = bookPerpsCloseWithFillFee(s, primaryCoin, closePx, closeFee, live, oidStr, "hedge_open_failed_unwind", "hedge_open_failed_unwind", "hedge", logger)
	}
	mu.Unlock()
	hedgeAlert(notifier, logger, "%s (booked=%v)", msg, booked)
	if booked {
		return 1
	}
	return 0
}

func hedgeAlert(notifier *MultiNotifier, logger *StrategyLogger, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if logger != nil {
		logger.Error("%s", msg)
	}
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}

func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := strategyConfigByID(cfg.Strategies)
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
		sc, ok := byID[id]
		syms := make([]string, 0, len(ss.Positions))
		for sym := range ss.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := ss.Positions[sym]
			if pos == nil || pos.HedgeFor == "" {
				continue
			}
			if !ok || !sc.HedgeEnabled() {
				warnings = append(warnings, fmt.Sprintf(
					"strategy[%s] holds hedge leg on %s (for %s) but hedge is disabled/absent — flatten or restore the hedge block",
					id, sym, pos.HedgeFor))
				continue
			}
			want := hedgeCoin(sc)
			if want != "" && !strings.EqualFold(want, sym) {
				warnings = append(warnings, fmt.Sprintf(
					"strategy[%s] hedge coin drift: persisted %s vs configured %s",
					id, sym, want))
			}
		}
	}
	return warnings
}

func hedgeStatusNote(sc StrategyConfig, ss *StrategyState) string {
	if !sc.HedgeEnabled() {
		return ""
	}
	hc := hedgeCoin(sc)
	if hc == "" {
		return ""
	}
	held := "flat"
	if ss != nil {
		if hp := ss.Positions[hc]; hp != nil && hp.HedgeFor != "" && hp.Quantity > 0 {
			held = fmt.Sprintf("%s %.4f (basis %.4f)", hp.Side, hp.Quantity, hp.HedgePrimaryQtyBasis)
		}
	}
	return fmt.Sprintf("hedge=%s×%g(%s,%s,%.0fx) [%s]",
		hc, hedgeRatio(sc), hedgeSide(sc), hedgeMarginMode(sc), hedgeLeverage(sc), held)
}

func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func formatHedgeConfig(h *HedgeConfig) string {
	if h == nil {
		return "<nil>"
	}
	if !h.Enabled {
		return "disabled"
	}
	return fmt.Sprintf("enabled symbol=%s side=%s ratio=%g", h.Symbol, h.Side, h.Ratio)
}
