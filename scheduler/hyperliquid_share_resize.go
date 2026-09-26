package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

func hlFloorToLot(qty float64, decimals int) float64 {
	if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
		return 0
	}
	if decimals < 0 {
		decimals = 0
	}
	factor := math.Pow(10, float64(decimals))
	return math.Floor(qty*factor+1e-9) / factor
}

func hlLotStep(decimals int) float64 {
	if decimals < 0 {
		decimals = 0
	}
	return math.Pow(10, -float64(decimals))
}

var pendingManualActionOnSymbolFn = pendingManualActionOnSymbol

func pendingManualActionOnSymbol(store *StateStore, symbol string) (bool, error) {
	if store == nil || strings.TrimSpace(symbol) == "" {
		return false, nil
	}
	db, err := store.liveFile()
	if err != nil {
		return false, err
	}
	unlock, err := acquireManualActionFileLockWithWait(db.path, 0)
	if err != nil {
		return false, err
	}
	defer unlock()
	actions, err := singleFileStore(db).LoadPendingManualActions()
	if err != nil {
		return false, err
	}
	for _, a := range actions {
		if !strings.EqualFold(a.Symbol, symbol) {
			continue
		}
		switch a.Action {
		case "open", "add", "close", "update-sl", "cancel-sl", "restore-tp":
			return true, nil
		}
	}
	return false, nil
}

func hlShareResizeDue(sc StrategyConfig) bool {
	if sc.Platform != "hyperliquid" || !hyperliquidIsLive(sc.Args) {
		return false
	}
	return sc.Type == "perps" || sc.Type == "manual"
}

// runHyperliquidShareResize replaces a resting stop that is above the chain
// share, or more than one lot below it, and shrinks resting take-profit tiers.
// A not-fresh view, a failed listing, a queued manual row, or an unreadable
// stop skips the coin.
func runHyperliquidShareResize(
	strategies []StrategyConfig,
	state *AppState,
	share *hlCycleShare,
	listed hlAllOpenOrders,
	listFailed bool,
	store *StateStore,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
) {
	if listFailed {
		n, alert := recordHLOpenOrderListFailure()
		if alert {
			hlSendShareCritical(notifier, fmt.Sprintf("CRITICAL: the Hyperliquid open-order listing has failed for %d consecutive cycles. Resting stops and take-profits were not resized to the chain share.", n))
		}
		return
	}
	clearHLOpenOrderListFailure()
	hlShareClearForceTP()
	if share == nil || state == nil {
		return
	}
	byOID := map[string]map[int64]hlListedOpenOrder{}
	for _, order := range listed.Orders {
		coin := hlCoinKey(order.Coin)
		if coin == "" || order.OID <= 0 {
			continue
		}
		if byOID[coin] == nil {
			byOID[coin] = map[int64]hlListedOpenOrder{}
		}
		byOID[coin][order.OID] = order
	}
	ids := make([]string, 0, len(strategies))
	byID := map[string]StrategyConfig{}
	for _, sc := range strategies {
		if !hlShareResizeDue(sc) {
			continue
		}
		ids = append(ids, sc.ID)
		byID[sc.ID] = sc
	}
	sort.Strings(ids)
	for _, id := range ids {
		sc := byID[id]
		symbol := hyperliquidSymbol(sc.Args)
		if sc.Type == "manual" && sc.Symbol != "" {
			symbol = sc.Symbol
		}
		if symbol == "" {
			continue
		}
		mu.RLock()
		ss := state.Strategies[sc.ID]
		var pos *Position
		if ss != nil {
			pos = ss.Positions[symbol]
		}
		if pos == nil || pos.Quantity <= 0 || pos.isHedgeLeg() || (pos.Side != "long" && pos.Side != "short") {
			mu.RUnlock()
			continue
		}
		book := pos.Quantity
		side := pos.Side
		armed := hlBookArmed(pos)
		slOID := pos.StopLossOID
		tpOIDs := cloneInt64s(pos.TPOIDs)
		peers, opp := share.peers(symbol, sc.ID, side)
		mu.RUnlock()

		q := share.StopQty(sc, symbol, side, book, armed, peers, opp)
		if !q.Fresh || !q.Known {
			continue
		}
		if pending, err := pendingManualActionOnSymbolFn(store, symbol); err != nil || pending {
			continue
		}
		if slOID > 0 && hlStopPlaceUnread(symbol, slOID) {
			continue
		}
		coin := hlCoinKey(symbol)
		decimals, haveLot := listed.Decimals[coin]
		floorQ := q.Qty
		oneLot := 0.0
		if haveLot {
			floorQ = hlFloorToLot(q.Qty, decimals)
			oneLot = hlLotStep(decimals)
		}
		orders := byOID[coin]
		if slOID > 0 {
			if order, ok := orders[slOID]; ok {
				hlShareResizeStop(sc, state, symbol, side, slOID, floorQ, oneLot, haveLot, order, mu, notifier)
			}
		}
		hlShareResizeTiers(sc, state, symbol, floorQ, tpOIDs, orders, mu, notifier)
	}
}

func hlShareStopNeedsReplace(listed, floorQ, oneLot float64, haveLot bool) bool {
	tol := hlSharedCloseQtyTolerance
	if listed > floorQ+tol {
		return true
	}
	if !haveLot {
		return false
	}
	return floorQ-listed > oneLot+tol
}

func hlShareResizeStop(sc StrategyConfig, state *AppState, symbol, side string, oid int64, floorQ, oneLot float64, haveLot bool, order hlListedOpenOrder, mu *sync.RWMutex, notifier *MultiNotifier) {
	if floorQ <= hlSharedCloseQtyTolerance {
		hlShareCancelRestingStop(sc, state, symbol, oid, mu, notifier)
		return
	}
	if !hlShareStopNeedsReplace(order.Sz, floorQ, oneLot, haveLot) {
		return
	}
	trigger := order.TriggerPx
	unlock := lockHyperliquidTrailingUpdate(symbol)
	result, _, _ := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, floorQ, trigger, oid)
	unlock()
	mu.Lock()
	defer mu.Unlock()
	ss := state.Strategies[sc.ID]
	if ss == nil {
		return
	}
	applyTrailingStopUpdateResult(ss, symbol, side, oid, 0, true, result, "trailing_stop_loss_immediate", nil, floorQ)
}

func hlShareCancelRestingStop(sc StrategyConfig, state *AppState, symbol string, oid int64, mu *sync.RWMutex, notifier *MultiNotifier) {
	unlock := lockHyperliquidTrailingUpdate(symbol)
	result, _, err := runHyperliquidCancelOrderFn(sc.Script, symbol, oid)
	unlock()
	if err == nil && result != nil && result.Cancelled {
		mu.Lock()
		if ss := state.Strategies[sc.ID]; ss != nil {
			if pos := ss.Positions[symbol]; pos != nil && pos.StopLossOID == oid {
				pos.StopLossOID = 0
				pos.StopLossTriggerPx = 0
			}
		}
		mu.Unlock()
		return
	}
	detail := "the cancel was refused"
	if err != nil {
		detail = err.Error()
	} else if result != nil && result.CancelError != "" {
		detail = result.CancelError
	} else if result != nil && result.Error != "" {
		detail = result.Error
	}
	hlSendShareCritical(notifier, fmt.Sprintf("CRITICAL: [%s] %s: stop OID %d is still resting (%s). Cancel it with `go-trader manual-cancel-sl %s --symbol %s`.",
		sc.ID, symbol, oid, detail, sc.ID, symbol))
}

func hlShareResizeTiers(sc StrategyConfig, state *AppState, symbol string, floorQ float64, tpOIDs []int64, orders map[int64]hlListedOpenOrder, mu *sync.RWMutex, notifier *MultiNotifier) {
	if len(tpOIDs) == 0 || orders == nil {
		return
	}
	var sum float64
	var resting []int64
	for _, oid := range tpOIDs {
		order, ok := orders[oid]
		if !ok || oid <= 0 {
			continue
		}
		sum += order.Sz
		resting = append(resting, oid)
	}
	if len(resting) == 0 {
		return
	}
	if floorQ <= hlSharedCloseQtyTolerance {
		cancelled := map[int64]bool{}
		for _, oid := range resting {
			result, _, err := runHyperliquidCancelOrderFn(sc.Script, symbol, oid)
			if err == nil && result != nil && result.Cancelled {
				cancelled[oid] = true
				continue
			}
			detail := "the cancel was refused"
			if err != nil {
				detail = err.Error()
			} else if result != nil && result.CancelError != "" {
				detail = result.CancelError
			}
			hlSendShareCritical(notifier, fmt.Sprintf("CRITICAL: [%s] %s: take-profit OID %d is still resting (%s). Cancel it on Hyperliquid.",
				sc.ID, symbol, oid, detail))
		}
		if len(cancelled) == 0 {
			return
		}
		mu.Lock()
		if ss := state.Strategies[sc.ID]; ss != nil {
			if pos := ss.Positions[symbol]; pos != nil {
				for i, oid := range pos.TPOIDs {
					if !cancelled[oid] {
						continue
					}
					pos.TPOIDs[i] = 0
					if i < len(pos.TPArmedTiers) {
						pos.TPArmedTiers[i] = false
					}
				}
			}
		}
		mu.Unlock()
		return
	}
	if sum > floorQ+hlSharedCloseQtyTolerance {
		hlShareMarkForceTP(sc.ID, symbol)
	}
}
