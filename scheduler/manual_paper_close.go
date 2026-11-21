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
			out = append(out, fmt.Sprintf("paper manual position %s/%s (%s %.6f, %s partition): before this release a paper manual strategy could send real Hyperliquid orders, so earlier opens, adds, closes, stops or limit orders for it may have left real exposure or protection on the account. Verify %s on Hyperliquid before you rely on this paper book; its trade and order history is kept unchanged.",
				sc.ID, coin, pos.Side, pos.Quantity, partitionLabel(partitionFor(sc)), coin))
		}
	}
	return out
}
