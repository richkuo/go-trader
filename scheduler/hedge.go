package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const hedgeQtyEpsilon = 1e-9

const hedgeMinOrderNotionalUSD = 10.0

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

func (k hedgeActionKind) isOrder() bool {
	return k != hedgeActionNone
}

type hedgeSnapshot struct {
	PrimarySymbol  string
	PrimaryQty     float64
	PrimarySide    string
	PrimaryAvgCost float64

	HedgeSymbol string
	HedgeQty    float64
	HedgeSide   string
	HedgeBasis  float64
	HedgeHeld   bool
}

type hedgeAction struct {
	Kind      hedgeActionKind
	Qty       float64
	Side      string
	HedgeSide string
	NewBasis  float64
	Reason    string
	Blocked   bool
}

func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) || snap.HedgeSymbol == "" {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge not enabled"}
	}

	primaryHeld := snap.PrimaryQty > hedgeQtyEpsilon
	hedgeHeld := snap.HedgeHeld && snap.HedgeQty > hedgeQtyEpsilon

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
		return hedgeAction{
			Kind:    hedgeActionNone,
			Blocked: true,
			Reason:  fmt.Sprintf("primary side %q is not long/short — refusing to derive a hedge side", snap.PrimarySide),
		}
	}

	if hedgeHeld && snap.HedgeSide != wantSide {
		return hedgeAction{
			Kind:     hedgeActionCloseFull,
			Qty:      snap.HedgeQty,
			NewBasis: 0,
			Reason:   fmt.Sprintf("hedge leg side %q opposes the required hedge side %q for a %s primary — flattening (this doubles exposure instead of hedging it)", snap.HedgeSide, wantSide, snap.PrimarySide),
		}
	}

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

	if !hedgeHeld {
		qty := snap.PrimaryQty * primaryPx * ratio / hedgePx
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Reason: "computed hedge size is zero"}
		}
		if qty*hedgePx < hedgeMinOrderNotionalUSD {
			return hedgeAction{
				Kind:   hedgeActionNone,
				Reason: fmt.Sprintf("hedge open of $%.2f is below the $%.2f minimum order notional — deferring (primary runs un-hedged until it grows above the floor)", qty*hedgePx, hedgeMinOrderNotionalUSD),
			}
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

	if snap.HedgeBasis <= hedgeQtyEpsilon {
		return hedgeAction{
			Kind:     hedgeActionNone,
			NewBasis: snap.PrimaryQty,
			Reason:   fmt.Sprintf("hedge leg has no quantity basis — re-anchoring watermark to primary %.8f without trading", snap.PrimaryQty),
		}
	}

	delta := snap.PrimaryQty - snap.HedgeBasis

	if delta > hedgeQtyEpsilon {
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

func hedgeOrderSideForPositionSide(positionSide string) string {
	if positionSide == "short" {
		return "sell"
	}
	return "buy"
}

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

type hedgeExecutor struct {
	Open          func(sc StrategyConfig, coin, side string, qty float64, setMargin bool) (*HyperliquidExecuteResult, error)
	Reduce        func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error)
	UnwindPrimary func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error)
	Refetch       func() (hlOnChainCoinView, error)
}

func defaultHedgeExecutor() hedgeExecutor {
	return hedgeExecutor{
		Open: func(sc StrategyConfig, coin, side string, qty float64, setMargin bool) (*HyperliquidExecuteResult, error) {
			marginMode := ""
			leverage := 0.0
			if setMargin {
				marginMode = hedgeMarginMode(sc)
				leverage = hedgeLeverage(sc)
			}
			res, stderr, err := runHyperliquidExecuteFn(sc.Script, coin, side, qty, 0, 0, 0, marginMode, leverage, hlCloseModeNone, hlExecuteSnapshot{})
			if stderr != "" {
				fmt.Printf("[hedge] %s execute stderr: %s\n", coin, stderr)
			}
			return res, err
		},
		Reduce: func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
			res, stderr, err := RunHyperliquidSizedClose(hyperliquidLiveCloseScript, req)
			if stderr != "" {
				fmt.Printf("[hedge] %s close stderr: %s\n", req.Symbol, stderr)
			}
			return res, err
		},
		UnwindPrimary: func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
			res, stderr, err := RunHyperliquidSizedClose(hyperliquidLiveCloseScript, req)
			if stderr != "" {
				fmt.Printf("[hedge] %s unwind stderr: %s\n", req.Symbol, stderr)
			}
			return res, err
		},
		Refetch: func() (hlOnChainCoinView, error) {
			addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
			if addr == "" {
				return hlOnChainCoinView{}, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS is not set")
			}
			return hlOnChainRefetcher(addr)()
		},
	}
}

type hedgeSyncInputs struct {
	PrimaryPx         float64
	HedgePx           float64
	FreshExposureQty  float64
	PrimaryCancelOIDs []int64
	PrimaryPeers      hedgePrimaryPeers
	Live              bool
}

type hedgePrimaryPeers struct {
	Known   bool
	Side    string
	SameQty float64
	OppQty  float64
}

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
		logger.Warn("hedge: %s", action.Reason)
		if in.FreshExposureQty > hedgeQtyEpsilon {
			unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, exec, snap, action.Reason, in, notifier, logger)
			return hedgeActionNone
		}
		notifyHedgeProblem(notifier, sc, snap.HedgeSymbol,
			fmt.Sprintf("hedge sync could not evaluate: %s — the primary may be running under-hedged; retrying next cycle", action.Reason))
		return hedgeActionNone
	}

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

	mu.RLock()
	spawnSnap := hedgeSnapshotFromState(sc, s)
	mu.RUnlock()
	if reason := hedgeOrderSkipReason(sc, action, spawnSnap); reason != "" {
		logger.Info("hedge: skipping %s on %s — %s", action.Kind, snap.HedgeSymbol, reason)
		return hedgeActionNone
	}

	logger.Info("hedge: %s", action.Reason)

	if !in.Live {
		mu.Lock()
		applyHedgeFill(sc, s, snap.PrimarySymbol, action, action.Qty, in.HedgePx, 0, false, "", logger)
		mu.Unlock()
		return action.Kind
	}

	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		res, err := exec.Open(sc, snap.HedgeSymbol, action.Side, action.Qty, action.Kind == hedgeActionOpen)
		res, err = confirmHyperliquidExecuteFill(res, err)
		if err != nil {
			logger.Error("hedge %s failed on %s: %s", action.Kind, snap.HedgeSymbol, err)
			notifyLiveExecFailure(notifier, sc, directionOpen, snap.HedgeSymbol, err.Error())
			if in.FreshExposureQty > hedgeQtyEpsilon {
				unwindPrimaryAfterHedgeOpenFailure(sc, s, mu, exec, snap, err.Error(), in, notifier, logger)
			} else {
				notifyHedgeProblem(notifier, sc, snap.HedgeSymbol,
					fmt.Sprintf("hedge %s failed: %s — the primary %s position is now under-hedged. The scheduler will retry every cycle; no position was unwound because the primary was not opened this cycle.", action.Kind, err, snap.PrimarySymbol))
			}
			return hedgeActionNone
		}
		clearLiveExecThrottle(sc, directionOpen, snap.HedgeSymbol)
		fill := res.Execution.Fill
		mu.Lock()
		applyHedgeFill(sc, s, snap.PrimarySymbol, action, fill.TotalSz, fill.AvgPx, fill.Fee, true, formatHedgeOID(fill.OID), logger)
		mu.Unlock()
		return action.Kind

	case hedgeActionReduce, hedgeActionCloseFull:
		return runHedgeLegClose(sc, s, mu, exec, snap, spawnSnap, action, in, notifier, logger)
	}
	return hedgeActionNone
}

func runHedgeLegClose(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, exec hedgeExecutor, snap, spawnSnap hedgeSnapshot, action hedgeAction, in hedgeSyncInputs, notifier *MultiNotifier, logger *StrategyLogger) hedgeActionKind {
	coin := snap.HedgeSymbol
	closeQty := math.Min(action.Qty, spawnSnap.HedgeQty)
	fail := func(why string) hedgeActionKind {
		logger.Error("hedge %s failed on %s: %s", action.Kind, coin, why)
		notifyLiveExecFailure(notifier, sc, directionClose, coin, why)
		notifyHedgeProblem(notifier, sc, coin,
			fmt.Sprintf("hedge %s failed: %s — an oversized hedge leg remains against %s. Retrying next cycle.", action.Kind, why, snap.PrimarySymbol))
		return hedgeActionNone
	}
	plan := resolveHLCloseOrder(coin, spawnSnap.HedgeSide, spawnSnap.HedgeQty, closeQty, hlCloseContext{Refetch: exec.Refetch})
	switch plan.Action {
	case hlCloseDefer:
		return fail("close not sent: " + plan.Reason)
	case hlCloseSkip:
		if plan.PreSend.Known && math.Abs(plan.PreSend.Signed) <= hlSharedCloseQtyTolerance {
			mu.Lock()
			clearHedgeLegAfterExternalFlat(s, coin, in.HedgePx, logger)
			mu.Unlock()
			return hedgeActionCloseFull
		}
		logger.Warn("hedge: %s on %s not sent: %s", action.Kind, coin, plan.Reason)
		notifyHedgeProblem(notifier, sc, coin,
			fmt.Sprintf("hedge %s not sent: %s. The %s hedge book does not match the on-chain position; the reconciler owns the book.", action.Kind, plan.Reason, coin))
		return hedgeActionNone
	}
	res, err := exec.Reduce(sc, hlSizedCloseRequest{Symbol: coin, Side: closeTradeSide(spawnSnap.HedgeSide), Mode: plan.Mode, Size: plan.Size})
	outcome := classifyHLSizedClose(res, err)
	switch {
	case outcome.NotSent:
		return fail("close not sent: " + outcome.Detail)
	case !outcome.Known:
		return fail(fmt.Sprintf("close outcome UNKNOWN (%s); nothing is booked and no second order is sent this cycle — the reconciler books any fill by order id", outcome.Detail))
	case outcome.Filled <= 0:
		return fail(outcome.Detail)
	}
	clearLiveExecThrottle(sc, directionClose, coin)
	if plan.Capped {
		notifyHedgeProblem(notifier, sc, coin,
			fmt.Sprintf("hedge %s was capped from %.8f to %.8f by the on-chain position (%s). The hedge book still overstates the chain; the reconciler owns the book.", action.Kind, closeQty, plan.Size, plan.Reason))
	}
	mu.Lock()
	applyHedgeFill(sc, s, snap.PrimarySymbol, action, outcome.Filled, outcome.AvgPx, outcome.Fee, true, formatHedgeOID(outcome.OID), logger)
	mu.Unlock()
	return action.Kind
}

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

func formatPendingCircuitCloseSymbols(symbols []PendingCircuitCloseSymbol) string {
	parts := make([]string, 0, len(symbols))
	for _, s := range symbols {
		parts = append(parts, fmt.Sprintf("%s sz=%.6f", s.Symbol, s.Size))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

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

func applyHedgeFill(sc StrategyConfig, s *StrategyState, primarySymbol string, action hedgeAction, filledQty, fillPx, fillFee float64, useFillFee bool, oid string, logger *StrategyLogger) {
	if s == nil || fillPx <= 0 || filledQty <= hedgeQtyEpsilon {
		return
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return
	}
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
		basis := action.NewBasis
		if action.Kind == hedgeActionAdd && action.Qty > 0 && exists && pos != nil && pos.HedgePrimaryQtyBasis > 0 {
			deltaBasis := action.NewBasis - pos.HedgePrimaryQtyBasis
			basis = pos.HedgePrimaryQtyBasis + deltaBasis*(filledQty/action.Qty)
		}
		if !exists || pos == nil {
			if action.Qty > 0 && filledQty < action.Qty {
				basis = action.NewBasis * (filledQty / action.Qty)
			}
			pos = &Position{
				Symbol:               coin,
				Quantity:             filledQty,
				InitialQuantity:      filledQty,
				AvgCost:              fillPx,
				Side:                 action.HedgeSide,
				Multiplier:           1,
				Leverage:             hedgeLeverage(sc),
				OwnerStrategyID:      s.ID,
				OpenedAt:             time.Now().UTC(),
				HedgeFor:             primarySymbol,
				HedgePrimaryQtyBasis: basis,
			}
			s.Positions[coin] = pos
		} else {
			totalQty := pos.Quantity + filledQty
			if totalQty > 0 {
				pos.AvgCost = (pos.AvgCost*pos.Quantity + fillPx*filledQty) / totalQty
			}
			pos.Quantity = totalQty
			pos.HedgePrimaryQtyBasis = basis
			pos.HedgeFor = primarySymbol
		}
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
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(fillFee, useFillFee),
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
		reduceBasis := hedgeReducedBasis(pre.HedgePrimaryQtyBasis, action.NewBasis, filledQty, action.Qty)
		if bookPerpsPartialCloseWithFillFee(s, coin, filledQty, fillPx, fillFee, useFillFee, oid, hedgeReduceCloseReason, detailsPrefix+" reduce", "hedge", logger) {
			if p, ok := s.Positions[coin]; ok && p != nil {
				p.HedgePrimaryQtyBasis = reduceBasis
			}
		}

	case hedgeActionCloseFull:
		pos := s.Positions[coin]
		if pos == nil {
			return
		}
		if filledQty+hedgeQtyEpsilon < pos.Quantity {
			if logger != nil {
				logger.Warn("hedge: %s close filled only %.8f of %.8f — booking a partial close; the remainder is re-closed next cycle", coin, filledQty, pos.Quantity)
			}
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

const (
	hedgeTradeType         = "hedge"
	hedgeReduceCloseReason = "hedge_reduce"
	hedgeCloseCloseReason  = "hedge_close"
	hedgeUnwindCloseReason = "hedge_open_failed_unwind"
)

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

	unresolved := func(why string, remaining float64) {
		logger.Error("hedge: FAIL-CLOSED UNWIND INCOMPLETE for %s: %s — %.8f of the %.8f increment is still open and UNHEDGED", snap.PrimarySymbol, why, remaining, unwindQty)
		notifyHedgeCritical(notifier, sc, fmt.Sprintf(
			"**CRITICAL — unhedged position running**\nStrategy `%s` opened %s %s but the %s hedge failed (%s), and the fail-closed unwind of the %.8f increment did not complete (%s).\n\n%.8f of the increment is still open and UNHEDGED. The scheduler retries the hedge every cycle; read the position on Hyperliquid and intervene if that does not clear.",
			sc.ID, snap.PrimarySide, snap.PrimarySymbol, snap.HedgeSymbol, hedgeErr, unwindQty, why, remaining))
	}

	ctx := hlCloseContext{Refetch: exec.Refetch}
	switch {
	case !in.PrimaryPeers.Known:
		unresolved("the peer books on the primary coin were not captured, so the close cannot be sized against them; no order was sent", unwindQty)
		return
	case in.PrimaryPeers.Side != snap.PrimarySide:
		unresolved(fmt.Sprintf("the peer books were captured for a %q primary, but the primary is now %q; no order was sent", in.PrimaryPeers.Side, snap.PrimarySide), unwindQty)
		return
	}
	ctx.PeerSameQty = in.PrimaryPeers.SameQty
	ctx.PeerOppQty = in.PrimaryPeers.OppQty
	plan := resolveHLCloseOrder(snap.PrimarySymbol, snap.PrimarySide, snap.PrimaryQty, unwindQty, ctx)
	if plan.Action != hlCloseSend {
		unresolved(fmt.Sprintf("no order was sent: %s", plan.Reason), unwindQty)
		return
	}
	req := hlSizedCloseRequest{Symbol: snap.PrimarySymbol, Side: closeTradeSide(snap.PrimarySide), Mode: plan.Mode, Size: plan.Size}
	if fullUnwind && !plan.Capped {
		req.CancelOIDs = positiveInt64s(in.PrimaryCancelOIDs)
		if len(req.CancelOIDs) > 0 {
			req.CancelMinFill = unwindQty - 0.0001
		}
	}
	res, err := exec.UnwindPrimary(sc, req)
	outcome := classifyHLSizedClose(res, err)
	switch {
	case outcome.NotSent:
		unresolved(fmt.Sprintf("the close was not sent: %s", outcome.Detail), unwindQty)
		return
	case !outcome.Known:
		unresolved(fmt.Sprintf("the close outcome is UNKNOWN (%s); nothing is booked and no second order is sent — the reconciler books any fill by order id", outcome.Detail), unwindQty)
		return
	case outcome.Filled <= 0:
		unresolved(fmt.Sprintf("the venue rejected the close: %s", outcome.Detail), unwindQty)
		return
	}

	filled := math.Min(outcome.Filled, unwindQty)
	if filled < unwindQty-hedgeQtyEpsilon {
		fullUnwind = false
	}
	unwindQty, requested := filled, unwindQty
	mu.Lock()
	bookUnwind(outcome.AvgPx, outcome.Fee, true, formatHedgeOID(outcome.OID))
	mu.Unlock()

	if filled < requested-hedgeQtyEpsilon {
		why := fmt.Sprintf("the %s close filled %.8f of the %.8f increment", operatorSizedCloseLabel(plan.Mode), filled, requested)
		if plan.Capped {
			why += fmt.Sprintf(" after it was capped to %.8f by the on-chain position (%s)", plan.Size, plan.Reason)
		}
		why += "; the pre-close protection is kept and the next protection sync resizes it to the book"
		unwindQty = requested
		unresolved(why, requested-filled)
		return
	}
	if res.CancelStopLossError != "" || len(hyperliquidSucceededCancelOIDs(res, req.CancelOIDs)) < len(req.CancelOIDs) {
		notifyHedgeCritical(notifier, sc, fmt.Sprintf(
			"**CRITICAL — hedge unwind left protection unconfirmed**\nStrategy `%s`: the fail-closed unwind closed %s on %s %s, but the cancel of the pre-close protection %v was not confirmed (%s). Read the open orders on Hyperliquid and cancel them by hand.",
			sc.ID, scope, snap.PrimarySide, snap.PrimarySymbol, req.CancelOIDs, res.CancelStopLossError))
	}

	notifyHedgeCritical(notifier, sc, fmt.Sprintf(
		"**CRITICAL — hedge open failed, primary unwound**\nStrategy `%s`: the %s hedge leg could not be opened (%s), so %s on %s %s was closed %s on the same cycle. No unhedged exposure was left running.\n\nCheck the hedge coin's margin availability and order limits before the next signal.",
		sc.ID, snap.HedgeSymbol, hedgeErr, scope, snap.PrimarySide, snap.PrimarySymbol, operatorSizedCloseLabel(plan.Mode)))
}

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
		if pendingHedgeAlerts != nil {
			*pendingHedgeAlerts = append(*pendingHedgeAlerts, fmt.Sprintf(
				"⚠️ **Hedge leg closed externally** — `%s` / %s\nThe %s hedge leg (%.8f, hedging %s) is gone from the exchange and has been booked as an external close. Because the primary is still open, hedge sync will RE-OPEN the hedge on the next cycle. Disable the hedge block if that is not what you want.",
				sc.ID, coin, coin, hadQty, primary))
		}
	case hadLeg && stillHeld && after != nil:
		if after.HedgePrimaryQtyBasis == 0 && hadBasis > 0 {
			after.HedgePrimaryQtyBasis = hadBasis
		}
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

func perpsPositionTradeType(pos *Position) string {
	if pos.isHedgeLeg() {
		return hedgeTradeType
	}
	return "perps"
}

func manualCloseTradeType(pos *Position) string {
	return perpsPositionTradeType(pos)
}

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

func hedgePrimaryPeersFor(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig, s *StrategyState, mu *sync.RWMutex, selfID, primarySymbol string) hedgePrimaryPeers {
	if s == nil || mu == nil || primarySymbol == "" {
		return hedgePrimaryPeers{}
	}
	mu.RLock()
	defer mu.RUnlock()
	pos := s.Positions[primarySymbol]
	if pos == nil || (pos.Side != "long" && pos.Side != "short") {
		return hedgePrimaryPeers{}
	}
	same, opp := hlPeerBooksOnCoin(strategies, hlLiveAll, primarySymbol, selfID, pos.Side)
	return hedgePrimaryPeers{Known: true, Side: pos.Side, SameQty: same, OppQty: opp}
}

func notifyHedgeProblem(notifier *MultiNotifier, sc StrategyConfig, coin, msg string) {
	if notifier == nil {
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("⚠️ **Hedge leg issue** — `%s` / %s\n%s", sc.ID, coin, msg))
}

func notifyHedgeCritical(notifier *MultiNotifier, sc StrategyConfig, msg string) {
	if notifier == nil {
		return
	}
	notifier.SendOwnerDM(msg)
	notifier.SendToAllChannels(msg)
}

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

type HedgeStatus struct {
	Enabled    bool    `json:"enabled"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Ratio      float64 `json:"ratio"`
	MarginMode string  `json:"margin_mode"`
	Leverage   float64 `json:"leverage"`
	CoupledTo  string  `json:"coupled_to,omitempty"`
	Held       bool    `json:"held"`
	Quantity   float64 `json:"quantity,omitempty"`
	PosSide    string  `json:"pos_side,omitempty"`
	AvgCost    float64 `json:"avg_cost,omitempty"`
	QtyBasis   float64 `json:"qty_basis,omitempty"`
}

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

func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

var postAuditHedgeSyncFn = runHedgeSync

func convergeHedgesAfterAuditClose(
	closeDetails []hlLiquidationCloseDetail,
	states map[string]*StrategyState,
	mu *sync.RWMutex,
	prices map[string]float64,
	notifier *MultiNotifier,
	getLogger func(string) (*StrategyLogger, error),
) int {
	converged := 0
	for _, cd := range closeDetails {
		sc := cd.SC
		if !HedgeEnabled(sc) {
			continue
		}
		s := states[sc.ID]
		if s == nil || mu == nil {
			continue
		}
		logger, err := getLogger(sc.ID)
		if err != nil || logger == nil {
			logger = &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
		}
		postAuditHedgeSyncFn(sc, s, mu, defaultHedgeExecutor(), hedgeSyncInputs{
			PrimaryPx: prices[cd.Symbol],
			HedgePx:   prices[hedgeCoin(sc)],
			Live:      hyperliquidIsLive(sc.Args),
		}, notifier, logger)
		converged++
	}
	return converged
}
