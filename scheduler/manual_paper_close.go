package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type paperManualCycleCloseInput struct {
	SC     StrategyConfig
	State  *StrategyState
	Store  *StateStore
	Pinned *Position
	Result *HyperliquidResult
	Price  float64
	Mu     *sync.RWMutex
	Logger *StrategyLogger
	Now    time.Time
}

type paperManualCycleCloseOutcome struct {
	Queued bool
	Hold   string
	Action PendingManualAction
}

var paperManualCloseLotLookup = func(coin string) hlLotLookup {
	if hlLotMetadata == nil {
		return hlLotLookup{Coin: coin, Reason: "no venue metadata source"}
	}
	return hlLotMetadata.Lookup(coin)
}

func paperManualCloseRefusal(in paperManualCycleCloseInput) (closeSide string, closeQty float64, full bool, hold string) {
	sc := in.SC
	if portfolioScopeFor(sc) == ScopeLive {
		return "", 0, false, "the strategy is live"
	}
	pos := in.Pinned
	if pos == nil || !finitePositive(pos.Quantity) || (pos.Side != "long" && pos.Side != "short") {
		return "", 0, false, "no valid paper position was evaluated"
	}
	if !manualPositionOwnedByStrategy(pos, sc.ID) {
		return "", 0, false, fmt.Sprintf("the position is owned by %q", pos.OwnerStrategyID)
	}
	res := in.Result
	if res == nil {
		return "", 0, false, "the close evaluation returned no result"
	}
	if strings.TrimSpace(res.Symbol) != "" && hlWriteCoinKey(res.Symbol) != hlWriteCoinKey(sc.Symbol) {
		return "", 0, false, fmt.Sprintf("the evaluation coin %q does not match the strategy coin %q", res.Symbol, sc.Symbol)
	}
	frac := res.CloseFraction
	if math.IsNaN(frac) || math.IsInf(frac, 0) || frac <= 0 || frac > 1 {
		return "", 0, false, fmt.Sprintf("close fraction %v is outside (0, 1]", frac)
	}
	wantSignal := -1
	closeSide = "sell"
	if pos.Side == "short" {
		wantSignal = 1
		closeSide = "buy"
	}
	if res.Signal != wantSignal {
		return "", 0, false, fmt.Sprintf("signal %d does not close the held %s side", res.Signal, pos.Side)
	}
	if !finitePositive(in.Price) {
		return "", 0, false, fmt.Sprintf("evaluation price %v is not a finite positive number", in.Price)
	}
	full = frac >= 1
	closeQty = pos.Quantity
	if !full {
		closeQty = pos.Quantity * frac
	}
	return closeSide, closeQty, full, ""
}

func queuePaperManualCycleClose(in paperManualCycleCloseInput) paperManualCycleCloseOutcome {
	sc := in.SC
	logHold := func(why string) paperManualCycleCloseOutcome {
		if in.Logger != nil {
			in.Logger.Info("Paper manual close for %s held: %s; the paper book is unchanged and no venue order is sent", sc.Symbol, why)
		}
		return paperManualCycleCloseOutcome{Hold: why}
	}
	closeSide, closeQty, full, hold := paperManualCloseRefusal(in)
	if hold != "" {
		return logHold(hold)
	}
	fillPx := paperTierFillPrice(sc, in.Result, nil)
	if fillPx <= 0 {
		fillPx = ApplyAdverseSlippage(in.Price, closeSide == "buy")
	}
	if !finitePositive(fillPx) {
		return logHold(fmt.Sprintf("simulated fill price %v is not a finite positive number", fillPx))
	}
	if !full {
		d := hlVenuePartialCloseDecision(closeQty, in.Price, paperManualCloseLotLookup(sc.Symbol))
		if d.Hold != "" {
			return logHold(fmt.Sprintf("%s: %s", d.Hold, d.Detail))
		}
		closeQty = d.Qty
	}
	if in.Store == nil || in.State == nil || in.Mu == nil {
		return logHold("the owning state store is unavailable")
	}
	release, err := in.Store.manualActionLock(sc.ID)
	if err != nil {
		return logHold(fmt.Sprintf("could not take the manual-action lock: %v", err))
	}
	defer release()
	pending, err := pendingManualActionExists(in.Store, sc.ID, sc.Symbol, "open", "add", "close")
	if err != nil {
		return logHold(fmt.Sprintf("queued position actions are unreadable: %v", err))
	}
	if pending {
		return logHold("a position action for this coin is already queued")
	}
	in.Mu.RLock()
	cur := in.State.Positions[sc.Symbol]
	var snap Position
	same := cur != nil && manualSamePositionIdentity(in.Pinned, cur, sc.ID) && math.Abs(cur.Quantity-in.Pinned.Quantity) <= 1e-9
	if same {
		snap = *cur
	}
	in.Mu.RUnlock()
	if !same {
		return logHold("the position changed identity, owner, side or quantity after the evaluation")
	}
	if !full && closeQty >= snap.Quantity-1e-9 {
		full = true
		closeQty = snap.Quantity
	}
	fee := CalculatePlatformSpotFee(sc.Platform, closeQty*fillPx)
	synthetic := &HyperliquidExecuteResult{Platform: sc.Platform, Execution: &HyperliquidExecution{Action: closeSide, Symbol: sc.Symbol, Size: closeQty, Fill: &HyperliquidFill{AvgPx: fillPx, TotalSz: closeQty, Fee: fee}}}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	booking := bookManualCycleClose(sc, &snap, closeSide, closeQty, full, synthetic, nil, now)
	if err := in.Store.InsertPendingManualAction(booking.Action); err != nil {
		return logHold(fmt.Sprintf("the simulated close could not be queued: %v", err))
	}
	if in.Logger != nil {
		in.Logger.Info("Paper manual close queued: %s %.6f %s @ $%.4f (simulated, fee $%.4f) | PnL=$%.2f; no venue order is sent", closeSide, booking.Action.Quantity, sc.Symbol, fillPx, fee, booking.Action.RealizedPnL)
	}
	return paperManualCycleCloseOutcome{Queued: true, Action: booking.Action}
}

func paperManualLimitRowStartupWarnings(cfg *Config, loader func() ([]PendingLimitOrder, error)) []string {
	if cfg == nil || loader == nil {
		return nil
	}
	owners := make(map[string]StrategyConfig)
	for _, sc := range cfg.Strategies {
		if paperManualLimitRowOwner(sc, true) {
			owners[sc.ID] = sc
		}
	}
	if len(owners) == 0 {
		return nil
	}
	orders, err := loader()
	if err != nil {
		return []string{fmt.Sprintf("paper manual limit orders: the resting limit order queue could not be read (%v), so the scheduler cannot tell whether a paper manual strategy still owns a real Hyperliquid limit order from a release before the paper manual order guard. Check the open orders of each paper manual coin on Hyperliquid.", err)}
	}
	sort.SliceStable(orders, func(i, j int) bool {
		if orders[i].StrategyID != orders[j].StrategyID {
			return orders[i].StrategyID < orders[j].StrategyID
		}
		return orders[i].OrderOID < orders[j].OrderOID
	})
	var out []string
	for _, o := range orders {
		if _, ok := owners[o.StrategyID]; !ok {
			continue
		}
		out = append(out, fmt.Sprintf("paper manual limit order %s/%s oid=%d (%s %.6f @ $%.4f): a release before the paper manual order guard placed this order on Hyperliquid for a paper manual strategy, and it can still rest and fill as real exposure. The scheduler cancels it on its next cycle through a live Hyperliquid strategy's script. With no live Hyperliquid strategy in this config it raises a CRITICAL alert: then cancel oid=%d on the Hyperliquid UI, close any position it opened and run `go-trader manual-clear-limit-row %d --flattened`.",
			o.StrategyID, o.Symbol, o.OrderOID, o.Side, o.OrderSize, o.LimitPrice, o.OrderOID, o.OrderOID))
	}
	return out
}

func sendPaperManualStartupWarnings(notifier *MultiNotifier, warnings []string) {
	if len(warnings) == 0 || notifier == nil || !notifier.HasOwner() {
		return
	}
	for _, msg := range warnings {
		notifier.SendOwnerDM("[state] WARNING: " + msg)
	}
}

func paperManualPositionStartupWarnings(cfg *Config, state *AppState) []string {
	if cfg == nil || state == nil {
		return nil
	}
	var out []string
	for _, sc := range cfg.Strategies {
		if sc.Type != "manual" || sc.Platform != "hyperliquid" || portfolioScopeFor(sc) == ScopeLive {
			continue
		}
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		keys := make([]string, 0, len(ss.Positions))
		for k, pos := range ss.Positions {
			if pos != nil && pos.Quantity > 0 {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, coin := range keys {
			pos := ss.Positions[coin]
			out = append(out, fmt.Sprintf("paper manual position %s/%s (%s %.6f, %s partition): a release before the paper manual order guard could send real Hyperliquid orders for a paper manual strategy. If this position, or any open, add, close, stop or limit order for it, dates from such a release, real exposure or protection can remain on the account: verify %s on Hyperliquid before you rely on this paper book. A position booked only with --record-only under the guard sent no order. This warning repeats on each start while the position is open; its trade and order history is kept unchanged.",
				sc.ID, coin, pos.Side, pos.Quantity, partitionLabel(partitionFor(sc)), coin))
		}
	}
	return out
}
