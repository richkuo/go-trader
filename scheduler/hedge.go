package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func validatePersistedHedgeDeclarations(state *AppState, cfg *Config) error {
	if state == nil || cfg == nil {
		return nil
	}
	byID := strategyConfigByID(cfg.Strategies)
	var errs []string
	for id, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		sc, configured := byID[id]
		if configured && sc.HedgeEnabled() {
			if pos := ss.Positions[hedgeCoin(sc)]; pos != nil && !pos.IsHedge {
				errs = append(errs, fmt.Sprintf("strategy %s declared hedge coin %s contains a persisted position without hedge ownership metadata", id, hedgeCoin(sc)))
			}
		}
		for sym, pos := range ss.Positions {
			if pos == nil || !pos.IsHedge {
				continue
			}
			if !configured || !sc.HedgeEnabled() {
				errs = append(errs, fmt.Sprintf("strategy %s has persisted hedge %s but no enabled hedge declaration", id, sym))
				continue
			}
			primary := ss.Positions[hyperliquidPrimaryCoin(sc)]
			if sym != hedgeCoin(sc) || pos.HedgeForSymbol != hyperliquidPrimaryCoin(sc) || pos.HedgeForPositionID == "" || (primary != nil && pos.HedgeForPositionID != primary.TradePositionID) {
				errs = append(errs, fmt.Sprintf("strategy %s persisted hedge %s has ownership metadata inconsistent with config", id, sym))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("persisted hedge ownership validation failed:\n  %s", strings.Join(errs, "\n  "))
}

// checkHedgeStateDriftAtStartup surfaces persisted pair metadata that no
// longer matches the loaded declaration. The first coherence sweep will
// converge it fail-closed; this read-only pass makes the reason visible before
// any side effect and forwards cleanly to the owner once notifiers are wired.
func checkHedgeStateDriftAtStartup(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	var warnings []string
	for _, sc := range cfg.Strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		primaryCoin := hyperliquidPrimaryCoin(sc)
		primary := ss.Positions[primaryCoin]
		if !sc.HedgeEnabled() {
			continue
		}
		coin := hedgeCoin(sc)
		h := ss.Positions[coin]
		switch {
		case primary != nil && h == nil:
			warnings = append(warnings, fmt.Sprintf("hedge state drift: strategy %s primary %s is open but declared hedge %s is missing; the uncovered primary will be closed fail-closed", sc.ID, primaryCoin, coin))
		case primary == nil && h != nil:
			warnings = append(warnings, fmt.Sprintf("hedge state drift: strategy %s hedge %s is open without primary %s; the orphan hedge will be closed", sc.ID, coin, primaryCoin))
		case primary != nil && h != nil:
			if primary.HedgeSymbol != "" && primary.HedgeSymbol != coin {
				warnings = append(warnings, fmt.Sprintf("hedge state drift: strategy %s primary %s points to hedge %s, but config declares %s", sc.ID, primaryCoin, primary.HedgeSymbol, coin))
			}
		}
	}
	sort.Strings(warnings)
	for _, msg := range warnings {
		fmt.Printf("[WARN] %s\n", msg)
	}
	return warnings
}

type hedgeActionKind string

const (
	hedgeActionNone    hedgeActionKind = "none"
	hedgeActionOpen    hedgeActionKind = "open"
	hedgeActionAdd     hedgeActionKind = "add"
	hedgeActionReduce  hedgeActionKind = "reduce"
	hedgeActionClose   hedgeActionKind = "close"
	hedgeActionBlocked hedgeActionKind = "blocked"
)

// hedgeSnapshot contains only immutable decision inputs captured while the
// scheduler state mutex is held. The decision core below performs no I/O.
type hedgeSnapshot struct {
	PrimaryQty         float64
	PrimaryAvgCost     float64
	PrimarySide        string
	PrimaryPositionID  string
	HedgeQty           float64
	HedgeAvgCost       float64
	HedgeSide          string
	HedgeCoveredQty    float64
	HedgeForPositionID string
}

type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string // position side (long/short), not order side
	Reason string
}

func inverseHedgeSide(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

func hedgeQtyNearlyEqual(a, b float64) bool {
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= 1e-9*scale
}

// hedgeTargetDecision mirrors primary QUANTITY events against a persisted
// covered-quantity watermark. The hedge mark is used to size new notional only;
// it never causes an order when the primary quantity is unchanged.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, hedgeMark float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.PrimaryQty <= 1e-9 {
		if snap.HedgeQty > 1e-9 {
			return hedgeAction{Kind: hedgeActionClose, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "primary is flat"}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}
	expectedSide := inverseHedgeSide(snap.PrimarySide)
	if expectedSide == "" {
		return hedgeAction{Kind: hedgeActionBlocked, Reason: fmt.Sprintf("invalid primary side %q", snap.PrimarySide)}
	}
	if snap.PrimaryPositionID == "" {
		return hedgeAction{Kind: hedgeActionBlocked, Reason: "primary ownership metadata is missing"}
	}
	if snap.HedgeQty <= 1e-9 {
		if snap.PrimaryAvgCost <= 0 || hedgeMark <= 0 {
			return hedgeAction{Kind: hedgeActionBlocked, Reason: "primary or hedge price unavailable"}
		}
		qty := snap.PrimaryQty * snap.PrimaryAvgCost * hedgeRatio(sc) / hedgeMark
		if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
			return hedgeAction{Kind: hedgeActionBlocked, Reason: "hedge open quantity is unusable"}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: expectedSide}
	}
	if snap.HedgeForPositionID == "" || snap.HedgeForPositionID != snap.PrimaryPositionID {
		return hedgeAction{Kind: hedgeActionBlocked, Reason: "hedge ownership does not match the primary position"}
	}
	if snap.HedgeSide != expectedSide {
		return hedgeAction{Kind: hedgeActionClose, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "hedge side no longer matches primary"}
	}
	if snap.HedgeCoveredQty <= 1e-9 {
		return hedgeAction{Kind: hedgeActionBlocked, Reason: "hedge ownership quantity watermark is missing"}
	}
	if hedgeQtyNearlyEqual(snap.PrimaryQty, snap.HedgeCoveredQty) {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.PrimaryQty > snap.HedgeCoveredQty {
		if snap.PrimaryAvgCost <= 0 || hedgeMark <= 0 {
			return hedgeAction{Kind: hedgeActionBlocked, Reason: "primary or hedge price unavailable for hedge add"}
		}
		delta := snap.PrimaryQty - snap.HedgeCoveredQty
		qty := delta * snap.PrimaryAvgCost * hedgeRatio(sc) / hedgeMark
		if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
			return hedgeAction{Kind: hedgeActionBlocked, Reason: "hedge add quantity is unusable"}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: expectedSide}
	}
	fraction := (snap.HedgeCoveredQty - snap.PrimaryQty) / snap.HedgeCoveredQty
	qty := snap.HedgeQty * fraction
	if qty > snap.HedgeQty || snap.PrimaryQty <= 1e-9 {
		qty = snap.HedgeQty
	}
	if qty <= 1e-9 {
		return hedgeAction{Kind: hedgeActionNone}
	}
	return hedgeAction{Kind: hedgeActionReduce, Qty: qty, Side: snap.HedgeSide}
}

type hedgeFill struct {
	Price float64
	Qty   float64
	Fee   float64
	OID   string
}

type hedgeOrderExecutor interface {
	open(sc StrategyConfig, coin, positionSide string, qty float64, fresh bool, positions []HLPosition) (hedgeFill, error)
	close(sc StrategyConfig, coin string, qty float64) (hedgeFill, error)
	unwindPrimary(sc StrategyConfig, coin string, qty float64, cancelOIDs []int64) (hedgeFill, error)
}

type liveHedgeOrderExecutor struct{}

func hedgeOrderSide(positionSide string) string {
	if positionSide == "long" {
		return "buy"
	}
	return "sell"
}

func hyperliquidExecuteHedgeFill(result *HyperliquidExecuteResult) (hedgeFill, error) {
	if result == nil {
		return hedgeFill{}, fmt.Errorf("empty execute result")
	}
	if result.Error != "" {
		return hedgeFill{}, fmt.Errorf("execute reported: %s", result.Error)
	}
	if result.Execution == nil || result.Execution.Fill == nil {
		return hedgeFill{}, fmt.Errorf("execute returned no confirmed fill")
	}
	f := result.Execution.Fill
	if f.AvgPx <= 0 || f.TotalSz <= 0 {
		return hedgeFill{}, fmt.Errorf("execute returned unusable fill price=%g qty=%g", f.AvgPx, f.TotalSz)
	}
	return hedgeFill{Price: f.AvgPx, Qty: f.TotalSz, Fee: f.Fee, OID: strconv.FormatInt(f.OID, 10)}, nil
}

func hyperliquidCloseHedgeFill(result *HyperliquidCloseResult) (hedgeFill, error) {
	if result == nil || result.Close == nil {
		return hedgeFill{}, fmt.Errorf("empty close result")
	}
	if result.Error != "" {
		return hedgeFill{}, fmt.Errorf("close reported: %s", result.Error)
	}
	if result.Close.Fill == nil {
		if result.Close.AlreadyFlat {
			return hedgeFill{}, fmt.Errorf("exchange already flat without a fill; reconcile must confirm ownership")
		}
		return hedgeFill{}, fmt.Errorf("close returned no confirmed fill")
	}
	f := result.Close.Fill
	if f.AvgPx <= 0 || f.TotalSz <= 0 {
		return hedgeFill{}, fmt.Errorf("close returned unusable fill price=%g qty=%g", f.AvgPx, f.TotalSz)
	}
	return hedgeFill{Price: f.AvgPx, Qty: f.TotalSz, Fee: f.Fee, OID: strconv.FormatInt(f.OID, 10)}, nil
}

func (liveHedgeOrderExecutor) open(sc StrategyConfig, coin, positionSide string, qty float64, fresh bool, positions []HLPosition) (hedgeFill, error) {
	marginMode := ""
	leverage := 0.0
	if fresh {
		marginMode = hedgeMarginMode(sc)
		leverage = hedgeExchangeLeverage(sc)
	}
	result, _, err := RunHyperliquidExecute(sc.Script, coin, hedgeOrderSide(positionSide), qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshotForCoin(positions, coin))
	if err != nil {
		return hedgeFill{}, err
	}
	return hyperliquidExecuteHedgeFill(result)
}

func (liveHedgeOrderExecutor) close(sc StrategyConfig, coin string, qty float64) (hedgeFill, error) {
	result, _, err := RunHyperliquidClose(sc.Script, coin, &qty, nil)
	if err != nil {
		return hedgeFill{}, err
	}
	return hyperliquidCloseHedgeFill(result)
}

func (liveHedgeOrderExecutor) unwindPrimary(sc StrategyConfig, coin string, qty float64, cancelOIDs []int64) (hedgeFill, error) {
	result, _, err := RunHyperliquidCloseCancelAfterFill(sc.Script, coin, &qty, cancelOIDs)
	if err != nil {
		return hedgeFill{}, err
	}
	return hyperliquidCloseHedgeFill(result)
}

func hedgeSnapshotFromState(sc StrategyConfig, s *StrategyState) hedgeSnapshot {
	if s == nil {
		return hedgeSnapshot{}
	}
	var out hedgeSnapshot
	if p := s.Positions[hyperliquidPrimaryCoin(sc)]; p != nil && !p.IsHedge {
		out.PrimaryQty = p.Quantity
		out.PrimaryAvgCost = p.AvgCost
		out.PrimarySide = p.Side
		out.PrimaryPositionID = p.TradePositionID
	}
	if h := s.Positions[hedgeCoin(sc)]; h != nil {
		out.HedgeQty = h.Quantity
		out.HedgeAvgCost = h.AvgCost
		out.HedgeSide = h.Side
		if h.IsHedge {
			out.HedgeCoveredQty = h.HedgeCoveredPrimaryQty
			out.HedgeForPositionID = h.HedgeForPositionID
		}
	}
	return out
}

func hedgeOrderSkipReason(sc StrategyConfig, planned hedgeAction, s *StrategyState, hedgeMark float64) string {
	current := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), hedgeMark)
	if current.Kind != planned.Kind || current.Side != planned.Side {
		return fmt.Sprintf("state changed before submit (planned %s/%s, now %s/%s)", planned.Kind, planned.Side, current.Kind, current.Side)
	}
	if planned.Qty > 0 && !hedgeQtyNearlyEqual(current.Qty, planned.Qty) {
		return fmt.Sprintf("quantity changed before submit (planned %.8f, now %.8f)", planned.Qty, current.Qty)
	}
	return ""
}

func hedgeOnChainConflict(action hedgeAction, coin string, positions []HLPosition) string {
	for _, p := range positions {
		if p.Coin != coin || math.Abs(p.Size) <= 1e-9 {
			continue
		}
		if action.Kind == hedgeActionOpen {
			return fmt.Sprintf("foreign on-chain position already exists on declared hedge coin %s", coin)
		}
		if action.Kind == hedgeActionAdd {
			onChainSide := "long"
			if p.Size < 0 {
				onChainSide = "short"
			}
			if onChainSide != action.Side {
				return fmt.Sprintf("on-chain hedge side %s disagrees with managed side %s", onChainSide, action.Side)
			}
		}
		break
	}
	return ""
}

func applyHedgeIncrease(s *StrategyState, sc StrategyConfig, action hedgeAction, fill hedgeFill, useFillFee bool) error {
	if s == nil || fill.Price <= 0 || fill.Qty <= 0 {
		return fmt.Errorf("unusable hedge fill")
	}
	primaryCoin := hyperliquidPrimaryCoin(sc)
	coin := hedgeCoin(sc)
	primary := s.Positions[primaryCoin]
	if primary == nil || primary.IsHedge || primary.Quantity <= 0 || primary.TradePositionID == "" {
		return fmt.Errorf("primary position changed before hedge fill apply")
	}
	hedgePos := s.Positions[coin]
	coveredBefore := 0.0
	if action.Kind == hedgeActionOpen {
		if hedgePos != nil {
			return fmt.Errorf("hedge coin %s acquired a virtual position before fill apply", coin)
		}
	} else if hedgePos == nil || !hedgePos.IsHedge || hedgePos.HedgeForPositionID != primary.TradePositionID || hedgePos.Side != action.Side {
		return fmt.Errorf("hedge ownership changed before add fill apply")
	}
	notional := fill.Price * fill.Qty
	fee := executionFee(CalculatePlatformSpotFee("hyperliquid", notional), fill.Fee, useFillFee)
	s.Cash -= fee
	now := time.Now().UTC()
	if action.Kind == hedgeActionOpen {
		hedgePos = &Position{
			Symbol:             coin,
			Quantity:           fill.Qty,
			InitialQuantity:    fill.Qty,
			AvgCost:            fill.Price,
			Side:               action.Side,
			Multiplier:         1,
			Leverage:           hedgeExchangeLeverage(sc),
			OwnerStrategyID:    s.ID,
			OpenedAt:           now,
			TradePositionID:    newTradePositionID(s.ID, coin, now),
			IsHedge:            true,
			HedgeForSymbol:     primaryCoin,
			HedgeForPositionID: primary.TradePositionID,
		}
		s.Positions[coin] = hedgePos
	} else {
		coveredBefore = hedgePos.HedgeCoveredPrimaryQty
		oldQty := hedgePos.Quantity
		newQty := oldQty + fill.Qty
		hedgePos.AvgCost = (oldQty*hedgePos.AvgCost + fill.Qty*fill.Price) / newQty
		hedgePos.Quantity = newQty
		hedgePos.InitialQuantity += fill.Qty
	}
	coverageFraction := fill.Qty / action.Qty
	if coverageFraction > 1 {
		coverageFraction = 1
	}
	if action.Kind == hedgeActionOpen {
		hedgePos.HedgeCoveredPrimaryQty = primary.Quantity * coverageFraction
	} else {
		hedgePos.HedgeCoveredPrimaryQty = coveredBefore + (primary.Quantity-coveredBefore)*coverageFraction
	}
	primary.HedgeSymbol = coin
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          coin,
		PositionID:      hedgePos.TradePositionID,
		Side:            hedgeOrderSide(action.Side),
		Quantity:        fill.Qty,
		Price:           fill.Price,
		Value:           notional,
		TradeType:       "perps",
		Details:         fmt.Sprintf("HEDGE %s for %s %.6f @ $%.4f (covered primary qty %.6f, fee $%.2f)", action.Kind, primaryCoin, fill.Qty, fill.Price, hedgePos.HedgeCoveredPrimaryQty, fee),
		ExchangeOrderID: fill.OID,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fill.Fee, useFillFee),
		PnLGross:        true,
		IsHedge:         true,
	}
	RecordTrade(s, trade)
	return nil
}

func applyHedgeClose(s *StrategyState, sc StrategyConfig, action hedgeAction, fill hedgeFill, useFillFee bool, logger *StrategyLogger) error {
	coin := hedgeCoin(sc)
	h := s.Positions[coin]
	if h == nil || !h.IsHedge {
		return fmt.Errorf("managed hedge disappeared before close fill apply")
	}
	closeQty := math.Min(fill.Qty, h.Quantity)
	if closeQty <= 0 {
		return fmt.Errorf("hedge close fill has no applicable quantity")
	}
	oldCovered := h.HedgeCoveredPrimaryQty
	full := closeQty >= h.Quantity-1e-9
	var ok bool
	if full {
		ok = bookPerpsCloseWithFillFee(s, coin, fill.Price, fill.Fee, useFillFee, fill.OID, "hedge_close", "HEDGE close", "Hedge close", logger)
	} else {
		ok = bookPerpsPartialCloseWithFillFee(s, coin, closeQty, fill.Price, fill.Fee, useFillFee, fill.OID, "hedge_reduce", "HEDGE reduce", "Hedge reduce", logger)
	}
	if !ok {
		return fmt.Errorf("hedge close booking refused")
	}
	primary := s.Positions[hyperliquidPrimaryCoin(sc)]
	if remaining := s.Positions[coin]; remaining != nil {
		fraction := closeQty / action.Qty
		if fraction > 1 {
			fraction = 1
		}
		targetCovered := 0.0
		if primary != nil {
			targetCovered = primary.Quantity
		}
		remaining.HedgeCoveredPrimaryQty = oldCovered - (oldCovered-targetCovered)*fraction
	} else if primary != nil && primary.HedgeSymbol == coin {
		primary.HedgeSymbol = ""
	}
	return nil
}

func notifyHedgeCritical(notifier *MultiNotifier, sc StrategyConfig, msg string) {
	full := fmt.Sprintf("**HEDGE CRITICAL** [%s] %s", sc.ID, msg)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(full)
		notifier.SendOwnerDM(full)
	}
}

func primaryUnwindQty(action hedgeAction, snap hedgeSnapshot) float64 {
	if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && snap.PrimaryQty > snap.HedgeCoveredQty {
		return snap.PrimaryQty - snap.HedgeCoveredQty
	}
	return snap.PrimaryQty
}

func unwindPrimaryPaperAfterHedgeFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, prices map[string]float64, qty float64, notifier *MultiNotifier, logger *StrategyLogger) (int, error) {
	coin := hyperliquidPrimaryCoin(sc)
	mark := prices[coin]
	if mark <= 0 {
		return 0, fmt.Errorf("paper primary mark unavailable for fail-closed unwind of %s", coin)
	}
	mu.Lock()
	defer mu.Unlock()
	p := s.Positions[coin]
	if p == nil || p.IsHedge || p.Quantity <= 0 {
		return 0, nil
	}
	if qty <= 0 || qty > p.Quantity {
		qty = p.Quantity
	}
	var booked bool
	if qty >= p.Quantity-1e-9 {
		booked = bookPerpsCloseWithFillFee(s, coin, mark, 0, false, "", "hedge_open_failed_unwind", "HEDGE FAILURE primary unwind", "Hedge failure unwind", logger)
	} else {
		booked = bookPerpsPartialCloseWithFillFee(s, coin, qty, mark, 0, false, "", "hedge_open_failed_unwind", "HEDGE FAILURE primary unwind", "Hedge failure unwind", logger)
	}
	if !booked {
		return 0, fmt.Errorf("paper primary unwind could not be booked")
	}
	notifyHedgeCritical(notifier, sc, fmt.Sprintf("paper hedge could not be confirmed; primary %s was reduced by %.6f", coin, qty))
	return 1, nil
}

func failClosedPrimary(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, prices map[string]float64, executor hedgeOrderExecutor, qty float64, notifier *MultiNotifier, logger *StrategyLogger) (int, error) {
	if hyperliquidIsLive(sc.Args) {
		return unwindPrimaryAfterHedgeFailure(sc, s, mu, prices, executor, qty, notifier, logger)
	}
	return unwindPrimaryPaperAfterHedgeFailure(sc, s, mu, prices, qty, notifier, logger)
}

func applyHedgeFillToPositions(positions []HLPosition, coin, side string, qty float64, increasing bool) []HLPosition {
	next := append([]HLPosition(nil), positions...)
	signed := qty
	if side == "short" {
		signed = -qty
	}
	for i := range next {
		if next[i].Coin != coin {
			continue
		}
		if increasing {
			next[i].Size += signed
		} else if next[i].Size < 0 {
			next[i].Size += qty
		} else {
			next[i].Size -= qty
		}
		if math.Abs(next[i].Size) <= 1e-9 {
			return append(next[:i], next[i+1:]...)
		}
		return next
	}
	if increasing {
		next = append(next, HLPosition{Coin: coin, Size: signed})
	}
	return next
}

func unwindPrimaryAfterHedgeFailure(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, prices map[string]float64, executor hedgeOrderExecutor, qty float64, notifier *MultiNotifier, logger *StrategyLogger) (int, error) {
	primaryCoin := hyperliquidPrimaryCoin(sc)
	mu.RLock()
	p := s.Positions[primaryCoin]
	if p == nil || p.IsHedge || p.Quantity <= 0 {
		mu.RUnlock()
		return 0, nil
	}
	if qty <= 0 || qty > p.Quantity {
		qty = p.Quantity
	}
	cancelOIDs := hyperliquidProtectionCancelOIDs(p)
	positionID := p.TradePositionID
	positionSide := p.Side
	mu.RUnlock()
	// Repeat the no-op/ownership guards immediately before the side-effecting
	// subprocess. A concurrent operator action between planning and submit must
	// never let a stale fail-closed plan hit a replacement position.
	mu.RLock()
	current := s.Positions[primaryCoin]
	unchanged := current != nil && !current.IsHedge && current.TradePositionID == positionID && current.Side == positionSide && current.Quantity+1e-9 >= qty
	mu.RUnlock()
	if !unchanged {
		return 0, fmt.Errorf("primary changed before fail-closed unwind submit")
	}

	fill, err := executor.unwindPrimary(sc, primaryCoin, qty, cancelOIDs)
	if err != nil {
		notifyHedgeCritical(notifier, sc, fmt.Sprintf("failed to unwind primary %s after hedge failure: %v", primaryCoin, err))
		return 0, err
	}
	mu.Lock()
	defer mu.Unlock()
	p = s.Positions[primaryCoin]
	if p == nil || p.IsHedge {
		return 0, fmt.Errorf("primary disappeared before unwind fill apply")
	}
	closeQty := math.Min(fill.Qty, p.Quantity)
	var booked bool
	if closeQty >= p.Quantity-1e-9 {
		booked = bookPerpsCloseWithFillFee(s, primaryCoin, fill.Price, fill.Fee, true, fill.OID, "hedge_open_failed_unwind", "HEDGE FAILURE primary unwind", "Hedge failure unwind", logger)
	} else {
		booked = bookPerpsPartialCloseWithFillFee(s, primaryCoin, closeQty, fill.Price, fill.Fee, true, fill.OID, "hedge_open_failed_unwind", "HEDGE FAILURE primary unwind", "Hedge failure unwind", logger)
	}
	if !booked {
		return 0, fmt.Errorf("primary unwind fill could not be booked")
	}
	_ = prices // kept in the seam for paper/future fallback parity
	notifyHedgeCritical(notifier, sc, fmt.Sprintf("hedge could not be confirmed; primary %s was reduced by %.6f", primaryCoin, closeQty))
	return 1, nil
}

// runHedgeSyncWithExecutor is the single lifecycle convergence point. Set
// allowIncrease only immediately after a confirmed primary execution event;
// the per-cycle repair sweep is close-only and fail-closes uncovered primary
// quantity instead of guessing a missing hedge open/add.
func runHedgeSyncWithExecutor(sc StrategyConfig, s *StrategyState, prices map[string]float64, positions []HLPosition, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, allowIncrease bool, executor hedgeOrderExecutor) (int, string) {
	if !sc.HedgeEnabled() || s == nil || mu == nil || executor == nil {
		return 0, ""
	}
	coin := hedgeCoin(sc)
	hedgeMark := prices[coin]
	live := hyperliquidIsLive(sc.Args)
	totalTrades := 0
	lastDetail := ""

	for attempts := 0; attempts < 4; attempts++ {
		mu.RLock()
		snap := hedgeSnapshotFromState(sc, s)
		action := hedgeTargetDecision(sc, snap, hedgeMark)
		mu.RUnlock()
		if action.Kind == hedgeActionNone {
			return totalTrades, lastDetail
		}
		if action.Kind == hedgeActionBlocked || ((action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && !allowIncrease) {
			reason := action.Reason
			if reason == "" {
				reason = fmt.Sprintf("repair sweep refuses position-increasing hedge action %s", action.Kind)
			}
			if logger != nil {
				logger.Error("Hedge fail-closed: %s", reason)
			}
			notifyHedgeCritical(notifier, sc, reason)
			if snap.PrimaryQty > 0 {
				trades, err := failClosedPrimary(sc, s, mu, prices, executor, primaryUnwindQty(action, snap), notifier, logger)
				totalTrades += trades
				if err == nil && trades > 0 {
					lastDetail = fmt.Sprintf("[%s] LIVE HEDGE FAIL-CLOSED %s", sc.ID, hyperliquidPrimaryCoin(sc))
					continue
				}
			}
			return totalTrades, lastDetail
		}
		if live {
			if conflict := hedgeOnChainConflict(action, coin, positions); conflict != "" {
				notifyHedgeCritical(notifier, sc, conflict)
				if snap.PrimaryQty > 0 {
					trades, _ := failClosedPrimary(sc, s, mu, prices, executor, primaryUnwindQty(action, snap), notifier, logger)
					totalTrades += trades
				}
				return totalTrades, lastDetail
			}
			mu.RLock()
			skip := hedgeOrderSkipReason(sc, action, s, hedgeMark)
			mu.RUnlock()
			if skip != "" {
				if logger != nil {
					logger.Warn("Skipping hedge order: %s", skip)
				}
				return totalTrades, lastDetail
			}
		}

		var fill hedgeFill
		var err error
		useFillFee := live
		if live {
			switch action.Kind {
			case hedgeActionOpen, hedgeActionAdd:
				fill, err = executor.open(sc, coin, action.Side, action.Qty, action.Kind == hedgeActionOpen, positions)
			case hedgeActionReduce, hedgeActionClose:
				fill, err = executor.close(sc, coin, action.Qty)
			}
		} else {
			if hedgeMark <= 0 {
				err = fmt.Errorf("paper hedge mark unavailable for %s", coin)
			} else {
				fill = hedgeFill{Price: hedgeMark, Qty: action.Qty}
			}
		}
		if err != nil {
			if logger != nil {
				logger.Error("Hedge %s failed for %s: %v", action.Kind, coin, err)
			}
			if live {
				notifyLiveExecFailure(notifier, sc, directionOpen, coin, err.Error())
			}
			notifyHedgeCritical(notifier, sc, fmt.Sprintf("%s %s failed: %v", action.Kind, coin, err))
			if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && snap.PrimaryQty > 0 {
				trades, _ := failClosedPrimary(sc, s, mu, prices, executor, primaryUnwindQty(action, snap), notifier, logger)
				totalTrades += trades
			}
			return totalTrades, lastDetail
		}

		mu.Lock()
		if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
			err = applyHedgeIncrease(s, sc, action, fill, useFillFee)
		} else {
			err = applyHedgeClose(s, sc, action, fill, useFillFee, logger)
		}
		mu.Unlock()
		if err != nil {
			notifyHedgeCritical(notifier, sc, fmt.Sprintf("fill landed but state apply failed for %s: %v", coin, err))
			if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
				trades, _ := failClosedPrimary(sc, s, mu, prices, executor, primaryUnwindQty(action, snap), notifier, logger)
				totalTrades += trades
			}
			return totalTrades, lastDetail
		}
		if live {
			positions = applyHedgeFillToPositions(positions, coin, action.Side, fill.Qty, action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd)
		}
		totalTrades++
		lastDetail = fmt.Sprintf("[%s] %sHEDGE %s %s %.6f @ $%.4f", sc.ID, map[bool]string{true: "LIVE "}[live], action.Kind, coin, fill.Qty, fill.Price)
		if logger != nil {
			logger.Info("HEDGE %s %s qty=%.6f @ $%.4f", action.Kind, coin, fill.Qty, fill.Price)
		}
		if live {
			clearLiveExecThrottle(sc, directionOpen, coin)
		}

		// A partial hedge increase is tracked precisely, then the uncovered
		// primary delta is closed immediately. Never advance the watermark as if
		// the full requested hedge filled.
		if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && fill.Qty < action.Qty-1e-9 {
			mu.RLock()
			post := hedgeSnapshotFromState(sc, s)
			mu.RUnlock()
			trades, _ := failClosedPrimary(sc, s, mu, prices, executor, primaryUnwindQty(action, post), notifier, logger)
			totalTrades += trades
		}
	}
	return totalTrades, lastDetail
}

func runHedgeSync(sc StrategyConfig, s *StrategyState, prices map[string]float64, positions []HLPosition, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, allowIncrease bool) (int, string) {
	return runHedgeSyncWithExecutor(sc, s, prices, positions, mu, notifier, logger, allowIncrease, liveHedgeOrderExecutor{})
}

func runHedgeCoherenceSweep(strategies []StrategyConfig, state *AppState, prices map[string]float64, positions []HLPosition, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) {
	if state == nil || mu == nil || logMgr == nil {
		return
	}
	for _, sc := range strategies {
		if !sc.HedgeEnabled() || state.Strategies[sc.ID] == nil {
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hedge sweep logger for %s: %v\n", sc.ID, err)
			continue
		}
		runHedgeSync(sc, state.Strategies[sc.ID], prices, positions, mu, notifier, logger, false)
	}
}
