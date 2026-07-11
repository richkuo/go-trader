package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hedgeLiveExecFn func(script, symbol, side string, size float64, marginMode string, leverage float64, closeFull bool, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error)

var hedgeLiveCloser HyperliquidLiveCloser = defaultHyperliquidLiveCloser
var hedgeCloseFailureAlerts sync.Map
var hedgeExposureFailureAlerts sync.Map
var lookupHedgeOpenFill = lookupHyperliquidHedgeOpenFill
var fetchHedgeAccountState = fetchHyperliquidState

func defaultHedgeLiveExec(script, symbol, side string, size float64, marginMode string, leverage float64, closeFull bool, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
	return RunHyperliquidExecute(script, symbol, side, size, 0, 0, 0, marginMode, leverage, closeFull, snapshot)
}

func hedgeCloseFailureAlertKey(sc StrategyConfig, coin string) string {
	return sc.ID + "\x00" + strings.ToUpper(strings.TrimSpace(coin))
}

func notifyHedgeCloseFailure(sc StrategyConfig, coin string, fullClose bool, qty float64, primaryOpen bool, cause error, notifier *MultiNotifier) {
	if notifier == nil {
		return
	}
	key := hedgeCloseFailureAlertKey(sc, coin)
	if _, loaded := hedgeCloseFailureAlerts.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	action := "reduce"
	exposure := "oversized while the primary remains open"
	if fullClose {
		action = "full-close"
	}
	if !primaryOpen {
		exposure = "NAKED because the primary is flat"
	}
	notifier.SendOwnerDM(fmt.Sprintf("CRITICAL [%s] hedge %s %s failed qty=%.6f — leg is %s: %v", sc.ID, coin, action, qty, exposure, cause))
}

func clearHedgeCloseFailureAlert(sc StrategyConfig, coin string) {
	hedgeCloseFailureAlerts.Delete(hedgeCloseFailureAlertKey(sc, coin))
}

func notifyHedgeExposureFailure(sc StrategyConfig, coin, exposure string, cause error, notifier *MultiNotifier) {
	if notifier == nil {
		return
	}
	key := hedgeCloseFailureAlertKey(sc, coin)
	if _, loaded := hedgeExposureFailureAlerts.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("CRITICAL [%s] hedge %s exposure increase failed — %s: %v", sc.ID, coin, exposure, cause))
}

func clearHedgeExposureFailureAlert(sc StrategyConfig, coin string) {
	hedgeExposureFailureAlerts.Delete(hedgeCloseFailureAlertKey(sc, coin))
}

func hedgeFill(result *HyperliquidExecuteResult, fallback float64) (px, qty, fee float64, oid string, ok bool) {
	if result != nil && result.Execution != nil && result.Execution.Fill != nil {
		f := result.Execution.Fill
		if f.AvgPx > 0 && f.TotalSz > 0 {
			if f.OID > 0 {
				oid = strconv.FormatInt(f.OID, 10)
			}
			return f.AvgPx, f.TotalSz, f.Fee, oid, true
		}
	}
	return fallback, 0, 0, "", fallback > 0
}

func unwindPrimaryAfterHedgeFailure(sc StrategyConfig, s *StrategyState, primarySym string, prices map[string]float64, closer HyperliquidLiveCloser, notifier *MultiNotifier, logger *StrategyLogger, mu *sync.RWMutex) bool {
	if s == nil {
		return false
	}
	if closer == nil {
		closer = hedgeLiveCloser
	}
	if mu != nil {
		mu.RLock()
	}
	primary := clonePosition(findPrimaryPosition(s, primarySym))
	if mu != nil {
		mu.RUnlock()
	}
	if primary == nil {
		return false
	}
	res, err := closer(primarySym, nil, hyperliquidProtectionCancelOIDs(primary))
	if err != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
		if logger != nil {
			logger.Error("[hedge] primary reduce-only unwind failed for %s: %v", primarySym, err)
		}
		return false
	}
	f := res.Close.Fill
	oid := ""
	if f.OID > 0 {
		oid = strconv.FormatInt(f.OID, 10)
	}
	px := f.AvgPx
	if px <= 0 {
		px = prices[primarySym]
	}
	if mu != nil {
		mu.Lock()
	}
	ok := bookPerpsCloseWithFillFee(s, primarySym, px, f.Fee, true, oid, "hedge_primary_unwind", "[hedge] primary unwind", "[hedge] primary unwind", logger)
	if mu != nil {
		mu.Unlock()
	}
	if !ok {
		return false
	}
	// Owner DM must run outside mu — Discord/Telegram HTTP under Lock deadlocks SIGHUP.
	if notifier != nil {
		notifier.SendOwnerDM(fmt.Sprintf("[hedge] strategy %s unwound primary %s after hedge order failure", sc.ID, primarySym))
	}
	return true
}

func clonePosition(pos *Position) *Position {
	if pos == nil {
		return nil
	}
	clone := *pos
	return &clone
}

func bookHedgeExposureFill(sc StrategyConfig, s *StrategyState, primarySym, side string, fillQty, fillPx, fee float64, oid, orderSide string, logger *StrategyLogger, mu *sync.RWMutex) (string, *Position, bool) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	primary := findPrimaryPosition(s, primarySym)
	if primary == nil {
		return "", nil, false
	}
	current := findHedgePosition(s, sc)
	coin := hedgeCoin(sc)
	if current == nil {
		if applyHedgeOpen(s, sc, primary, side, fillQty, fillPx, fee, oid, logger) == nil {
			return "", nil, false
		}
		return fmt.Sprintf("open %.6f %s", fillQty, coin), findHedgePosition(s, sc), true
	}
	applyHedgeScale(current, fillQty, fillPx)
	trade := Trade{Timestamp: timeNowUTC(), StrategyID: sc.ID, Symbol: coin, PositionID: current.TradePositionID, Side: orderSide, Quantity: fillQty, Price: fillPx, Value: fillQty * fillPx, TradeType: "perps", Details: "[hedge] scale", ExchangeOrderID: oid, ExchangeFee: fee, FeeSource: FeeSourceUserFills}
	RecordTrade(s, trade)
	s.Cash -= fee
	return fmt.Sprintf("scale %.6f %s", fillQty, coin), current, true
}

func recoverAmbiguousHedgeExposureFill(sc StrategyConfig, s *StrategyState, primarySym string, order hedgeOrder, target hedgeTarget, startedAt time.Time, logger *StrategyLogger, mu *sync.RWMutex) (string, *Position, bool) {
	account := strings.TrimSpace(os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS"))
	lookup, ok := lookupHedgeOpenFill(account, hedgeCoin(sc), target.Side, order.Quantity, startedAt.Add(-2*time.Second).UnixMilli())
	if !ok || lookup.FilledQty <= 0 || lookup.Px <= 0 {
		return "", nil, false
	}
	oid := ""
	if lookup.OID > 0 {
		oid = strconv.FormatInt(lookup.OID, 10)
	}
	action, current, booked := bookHedgeExposureFill(sc, s, primarySym, target.Side, lookup.FilledQty, lookup.Px, lookup.Fee, oid, order.Side, logger, mu)
	if booked && logger != nil {
		logger.Warn("[hedge] recovered ambiguous %s fill from exact userFills oid=%s qty=%.6f px=%.6f", hedgeCoin(sc), oid, lookup.FilledQty, lookup.Px)
	}
	return action, current, booked
}

func hedgeExposureOrderDefinitelyAbsent(coin string, currentQty float64) bool {
	account := strings.TrimSpace(os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS"))
	if account == "" || currentQty <= 0 {
		return false
	}
	_, positions, err := fetchHedgeAccountState(account)
	if err != nil {
		return false
	}
	for _, p := range positions {
		if strings.EqualFold(strings.TrimSpace(p.Coin), strings.TrimSpace(coin)) {
			return math.Abs(math.Abs(p.Size)-currentQty) <= hlReconcileFillSizeTolerance
		}
	}
	return false
}

func rollbackPrimaryToHedgeCoverage(sc StrategyConfig, s *StrategyState, primarySym string, prices map[string]float64, notifier *MultiNotifier, logger *StrategyLogger, mu *sync.RWMutex) bool {
	if mu != nil {
		mu.RLock()
	}
	primary := clonePosition(findPrimaryPosition(s, primarySym))
	hedge := clonePosition(findHedgePosition(s, sc))
	if mu != nil {
		mu.RUnlock()
	}
	primaryPx, hedgePx := prices[primarySym], prices[hedgeCoin(sc)]
	if primary == nil || hedge == nil || primaryPx <= 0 || hedgePx <= 0 || sc.Hedge.Ratio <= 0 {
		return false
	}
	coveredPrimaryQty := hedge.Quantity * hedgePx / (primaryPx * sc.Hedge.Ratio)
	rollbackQty := primary.Quantity - coveredPrimaryQty
	if rollbackQty <= hedgeQtyEpsilon {
		return true
	}
	if rollbackQty >= primary.Quantity-hedgeQtyEpsilon {
		return false
	}
	partial := rollbackQty
	res, err := hedgeLiveCloser(primarySym, &partial, nil)
	if err != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
		return false
	}
	f := res.Close.Fill
	if f.AvgPx <= 0 || f.TotalSz <= 0 {
		return false
	}
	oid := ""
	if f.OID > 0 {
		oid = strconv.FormatInt(f.OID, 10)
	}
	if mu != nil {
		mu.Lock()
	}
	booked := bookPerpsPartialCloseWithFillFee(s, primarySym, math.Min(f.TotalSz, rollbackQty), f.AvgPx, f.Fee, true, oid, "hedge_primary_rollback", "[hedge] primary rollback", "[hedge] primary rollback", logger)
	if mu != nil {
		mu.Unlock()
	}
	if booked && notifier != nil {
		notifier.SendOwnerDM(fmt.Sprintf("[hedge] strategy %s rolled back %.6f %s after a forced hedge top-up failure; prior hedge coverage restored", sc.ID, math.Min(f.TotalSz, rollbackQty), primarySym))
	}
	return booked
}

func flattenHedgeAfterFailure(sc StrategyConfig, s *StrategyState, logger *StrategyLogger, mu *sync.RWMutex) bool {
	if mu != nil {
		mu.RLock()
	}
	hedge := clonePosition(findHedgePosition(s, sc))
	if mu != nil {
		mu.RUnlock()
	}
	if hedge == nil {
		return true
	}
	res, err := hedgeLiveCloser(hedge.Symbol, nil, nil)
	if err != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
		return false
	}
	f := res.Close.Fill
	oid := ""
	if f.OID > 0 {
		oid = strconv.FormatInt(f.OID, 10)
	}
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return bookPerpsCloseWithFillFee(s, hedge.Symbol, f.AvgPx, f.Fee, true, oid, "hedge_failure_flatten", "[hedge] failure flatten", "[hedge] failure flatten", logger)
}

// syncStrategyHedge runs after confirmed primary fills and during hold cycles.
// It converges the hedge to the primary's current notional. Every reduction
// uses the reduce-only closer; a failed hedge increase immediately unwinds the
// primary, preserving the fail-closed invariant.
func syncStrategyHedge(sc StrategyConfig, s *StrategyState, primarySym string, prices map[string]float64, hlPositions []HLPosition, exec hedgeLiveExecFn, notifier *MultiNotifier, logger *StrategyLogger, mu *sync.RWMutex, forceRebalance bool) (detail string, primaryUnwound bool, err error) {
	if !hedgeEnabled(sc) || s == nil {
		return "", false, nil
	}
	if exec == nil {
		exec = defaultHedgeLiveExec
	}
	primarySym = hyperliquidCoinFromSymbol(primarySym)
	if primarySym == "" {
		primarySym = hyperliquidConfiguredCoin(sc)
	}
	read := func() (*Position, *Position) {
		if mu != nil {
			mu.RLock()
			defer mu.RUnlock()
		}
		return clonePosition(findPrimaryPosition(s, primarySym)), clonePosition(findHedgePosition(s, sc))
	}
	primary, current := read()
	failClosed := func(reason error) (string, bool, error) {
		// Primary is open but the required hedge cannot be established/sized —
		// never leave an unhedged primary (#1159 constraint 4).
		hedgeFlat := flattenHedgeAfterFailure(sc, s, logger, mu)
		unwound := unwindPrimaryAfterHedgeFailure(sc, s, primarySym, prices, nil, notifier, logger, mu)
		msg := fmt.Sprintf("CRITICAL [%s] hedge sync fail-closed (%v); primary_unwound=%t hedge_flat=%t", sc.ID, reason, unwound, hedgeFlat)
		if notifier != nil {
			notifier.SendOwnerDM(msg)
		}
		return "", unwound, fmt.Errorf("%s", msg)
	}
	target := hedgeTarget{}
	if primary != nil {
		primaryPrice := prices[primarySym]
		hedgePrice := prices[hedgeCoin(sc)]
		if primaryPrice <= 0 || hedgePrice <= 0 {
			markErr := fmt.Errorf("hedge sizing requires positive primary and hedge prices (primary=%g hedge=%g)", primaryPrice, hedgePrice)
			if current != nil {
				// Existing coverage is safer than a fleet-wide liquidation on a
				// transient/partial mids response. No order was attempted, so hold
				// both legs and retry on the next cycle. A fresh unhedged primary
				// still takes failClosed below.
				if logger != nil {
					logger.Warn("[hedge] marks unavailable for %s/%s; holding existing hedge without rebalance: %v", primarySym, hedgeCoin(sc), markErr)
				}
				return "", false, nil
			}
			return failClosed(markErr)
		}
		target, err = hedgeTargetForPrimary(sc, primary.Side, primary.Quantity, primaryPrice, hedgePrice)
		if err != nil {
			return failClosed(err)
		}
	}
	orders, err := planHedgeTransitionWithPolicy(current, target, forceRebalance, hedgeRebalanceMinMovePct(sc))
	if err != nil {
		if primary != nil {
			return failClosed(err)
		}
		return "", false, err
	}
	var actions []string
	hasCloseOrder := false
	hasExposureOrder := false
	for _, order := range orders {
		if order.Close {
			hasCloseOrder = true
		} else {
			hasExposureOrder = true
		}
	}
	if !hasCloseOrder {
		clearHedgeCloseFailureAlert(sc, hedgeCoin(sc))
	}
	if !hasExposureOrder {
		clearHedgeExposureFailureAlert(sc, hedgeCoin(sc))
	}
	for _, order := range orders {
		coin := hedgeCoin(sc)
		if order.Close {
			var partial *float64
			if !order.FullClose {
				q := order.Quantity
				partial = &q
			}
			res, callErr := hedgeLiveCloser(coin, partial, nil)
			if callErr != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
				if callErr == nil {
					callErr = fmt.Errorf("no confirmed hedge close fill")
				}
				notifyHedgeCloseFailure(sc, coin, order.FullClose, order.Quantity, primary != nil, callErr, notifier)
				return strings.Join(actions, "; "), false, fmt.Errorf("hedge reduce-only close failed: %w", callErr)
			}
			f := res.Close.Fill
			if f.AvgPx <= 0 || f.TotalSz <= 0 {
				fillErr := fmt.Errorf("hedge reduce-only close for %s returned no usable fill (avg_px=%.6f total_sz=%.6f)", coin, f.AvgPx, f.TotalSz)
				notifyHedgeCloseFailure(sc, coin, order.FullClose, order.Quantity, primary != nil, fillErr, notifier)
				return strings.Join(actions, "; "), false, fillErr
			}
			oid := ""
			if f.OID > 0 {
				oid = strconv.FormatInt(f.OID, 10)
			}
			if mu != nil {
				mu.Lock()
			}
			booked := false
			if order.FullClose {
				booked = bookPerpsCloseWithFillFee(s, coin, f.AvgPx, f.Fee, true, oid, "hedge_close", "[hedge] close", "[hedge] close", logger)
			} else {
				booked = bookPerpsPartialCloseWithFillFee(s, coin, math.Min(f.TotalSz, order.Quantity), f.AvgPx, f.Fee, true, oid, "hedge_partial_close", "[hedge] partial close", "[hedge] partial close", logger)
			}
			if mu != nil {
				mu.Unlock()
			}
			if !booked {
				bookingErr := fmt.Errorf("hedge reduce-only close for %s filled on-chain but virtual booking failed", coin)
				notifyHedgeCloseFailure(sc, coin, order.FullClose, order.Quantity, primary != nil, bookingErr, notifier)
				return strings.Join(actions, "; "), false, bookingErr
			}
			clearHedgeCloseFailureAlert(sc, coin)
			actions = append(actions, fmt.Sprintf("close %.6f %s", f.TotalSz, coin))
			if mu != nil {
				mu.RLock()
			}
			current = clonePosition(findHedgePosition(s, sc))
			if mu != nil {
				mu.RUnlock()
			}
			continue
		}
		marginMode, leverage := "", float64(0)
		if current == nil {
			marginMode, leverage = sc.Hedge.MarginMode, sc.Hedge.Leverage
		}
		startedAt := timeNowUTC()
		result, stderr, callErr := exec(sc.Script, coin, order.Side, order.Quantity, marginMode, leverage, false, hlExecuteSnapshotForCoin(hlPositions, coin))
		handleExposureFailure := func(cause error) (string, bool, error) {
			if current != nil {
				exposure := "existing hedge retained; retrying the incremental top-up next cycle"
				if forceRebalance && hedgeExposureOrderDefinitelyAbsent(coin, current.Quantity) {
					if rollbackPrimaryToHedgeCoverage(sc, s, primarySym, prices, notifier, logger, mu) {
						exposure = "forced primary increment rolled back to prior hedge coverage"
					} else {
						exposure = "primary remains under-hedged because proportional rollback also failed"
					}
				} else if forceRebalance {
					exposure = "fill status remains ambiguous; existing hedge and primary retained for exact reconciliation"
				}
				notifyHedgeExposureFailure(sc, coin, exposure, cause, notifier)
				return strings.Join(actions, "; "), false, fmt.Errorf("hedge incremental exposure order failed (%s): %w", exposure, cause)
			}
			hedgeFlat := flattenHedgeAfterFailure(sc, s, logger, mu)
			unwound := unwindPrimaryAfterHedgeFailure(sc, s, primarySym, prices, nil, notifier, logger, mu)
			msg := fmt.Sprintf("CRITICAL [%s] initial hedge exposure order failed; primary_unwound=%t hedge_flat=%t: %v", sc.ID, unwound, hedgeFlat, cause)
			if notifier != nil {
				notifier.SendOwnerDM(msg)
			}
			return strings.Join(actions, "; "), unwound, fmt.Errorf("%s", msg)
		}
		if callErr != nil {
			if recoveredAction, recoveredCurrent, recovered := recoverAmbiguousHedgeExposureFill(sc, s, primarySym, order, target, startedAt, logger, mu); recovered {
				actions = append(actions, recoveredAction)
				current = recoveredCurrent
				clearHedgeExposureFailureAlert(sc, coin)
				continue
			}
			return handleExposureFailure(callErr)
		}
		fillPx, fillQty, fee, oid, fillOK := hedgeFill(result, prices[coin])
		if !fillOK || fillQty <= 0 || fillPx <= 0 {
			if recoveredAction, recoveredCurrent, recovered := recoverAmbiguousHedgeExposureFill(sc, s, primarySym, order, target, startedAt, logger, mu); recovered {
				actions = append(actions, recoveredAction)
				current = recoveredCurrent
				clearHedgeExposureFailureAlert(sc, coin)
				continue
			}
			cause := fmt.Errorf("no confirmed fill (%s)", strings.TrimSpace(stderr))
			return handleExposureFailure(cause)
		}
		action, updatedCurrent, booked := bookHedgeExposureFill(sc, s, primarySym, target.Side, fillQty, fillPx, fee, oid, order.Side, logger, mu)
		if !booked {
			return handleExposureFailure(fmt.Errorf("confirmed hedge fill could not be booked"))
		}
		actions = append(actions, action)
		current = updatedCurrent
		clearHedgeExposureFailureAlert(sc, coin)
	}
	return strings.Join(actions, "; "), false, nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }
