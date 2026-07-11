package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hedgeLiveExecFn func(script, symbol, side string, size float64, marginMode string, leverage float64, closeFull bool, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error)

var hedgeLiveCloser HyperliquidLiveCloser = defaultHyperliquidLiveCloser

func defaultHedgeLiveExec(script, symbol, side string, size float64, marginMode string, leverage float64, closeFull bool, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
	return RunHyperliquidExecute(script, symbol, side, size, 0, 0, 0, marginMode, leverage, closeFull, snapshot)
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

// syncStrategyHedge runs only after a confirmed primary fill has been applied
// to virtual state. It converges the hedge to the primary's current notional.
// Every reduction uses the reduce-only closer; a failed hedge increase
// immediately unwinds the primary, preserving the fail-closed invariant.
func syncStrategyHedge(sc StrategyConfig, s *StrategyState, primarySym string, prices map[string]float64, hlPositions []HLPosition, exec hedgeLiveExecFn, notifier *MultiNotifier, logger *StrategyLogger, mu *sync.RWMutex) (detail string, primaryUnwound bool, err error) {
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
		return findPrimaryPosition(s, primarySym), findHedgePosition(s, sc)
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
		target, err = hedgeTargetForPrimary(sc, primary.Side, primary.Quantity, prices[primarySym], prices[hedgeCoin(sc)])
		if err != nil {
			return failClosed(err)
		}
	}
	orders, err := planHedgeTransition(current, target)
	if err != nil {
		if primary != nil {
			return failClosed(err)
		}
		return "", false, err
	}
	var actions []string
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
				return strings.Join(actions, "; "), false, fmt.Errorf("hedge reduce-only close failed: %w", callErr)
			}
			f := res.Close.Fill
			if f.AvgPx <= 0 || f.TotalSz <= 0 {
				return strings.Join(actions, "; "), false, fmt.Errorf("hedge reduce-only close for %s returned no usable fill (avg_px=%.6f total_sz=%.6f)", coin, f.AvgPx, f.TotalSz)
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
				return strings.Join(actions, "; "), false, fmt.Errorf("hedge reduce-only close for %s filled on-chain but virtual booking failed", coin)
			}
			actions = append(actions, fmt.Sprintf("close %.6f %s", f.TotalSz, coin))
			if mu != nil {
				mu.RLock()
			}
			current = findHedgePosition(s, sc)
			if mu != nil {
				mu.RUnlock()
			}
			continue
		}
		marginMode, leverage := "", float64(0)
		if current == nil {
			marginMode, leverage = sc.Hedge.MarginMode, sc.Hedge.Leverage
		}
		result, stderr, callErr := exec(sc.Script, coin, order.Side, order.Quantity, marginMode, leverage, false, hlExecuteSnapshotForCoin(hlPositions, coin))
		if callErr != nil {
			hedgeFlat := flattenHedgeAfterFailure(sc, s, logger, mu)
			unwound := unwindPrimaryAfterHedgeFailure(sc, s, primarySym, prices, nil, notifier, logger, mu)
			msg := fmt.Sprintf("CRITICAL [%s] hedge exposure order failed; primary_unwound=%t hedge_flat=%t: %v", sc.ID, unwound, hedgeFlat, callErr)
			if notifier != nil {
				notifier.SendOwnerDM(msg)
			}
			return "", unwound, fmt.Errorf("%s", msg)
		}
		fillPx, fillQty, fee, oid, fillOK := hedgeFill(result, prices[coin])
		if !fillOK || fillQty <= 0 || fillPx <= 0 {
			hedgeFlat := flattenHedgeAfterFailure(sc, s, logger, mu)
			unwound := unwindPrimaryAfterHedgeFailure(sc, s, primarySym, prices, nil, notifier, logger, mu)
			return strings.Join(actions, "; "), unwound, fmt.Errorf("CRITICAL [%s] hedge order for %s returned no confirmed fill; primary_unwound=%t hedge_flat=%t (%s)", sc.ID, coin, unwound, hedgeFlat, strings.TrimSpace(stderr))
		}
		if mu != nil {
			mu.Lock()
		}
		if current == nil {
			applyHedgeOpen(s, sc, primary, target.Side, fillQty, fillPx, fee, oid, logger)
			actions = append(actions, fmt.Sprintf("open %.6f %s", fillQty, coin))
			current = s.Positions[coin]
		} else {
			applyHedgeScale(current, fillQty, fillPx)
			trade := Trade{Timestamp: timeNowUTC(), StrategyID: sc.ID, Symbol: coin, PositionID: current.TradePositionID, Side: order.Side, Quantity: fillQty, Price: fillPx, Value: fillQty * fillPx, TradeType: "perps", Details: "[hedge] scale", ExchangeOrderID: oid, ExchangeFee: fee, FeeSource: FeeSourceUserFills}
			RecordTrade(s, trade)
			s.Cash -= fee
			actions = append(actions, fmt.Sprintf("scale %.6f %s", fillQty, coin))
		}
		if mu != nil {
			mu.Unlock()
		}
	}
	return strings.Join(actions, "; "), false, nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }
