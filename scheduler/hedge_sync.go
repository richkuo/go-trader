package main

// #1159 phase 1: the per-cycle hedge convergence engine. Runs once per main
// loop AFTER the dispatch loop (and never while the portfolio kill switch is
// latched — the kill-switch close path owns flattening then), so any primary
// lifecycle event that landed this cycle — fresh open, scale-in add, partial
// close, full close, on-chain SL fill booked by reconcile, circuit-breaker
// force-close — is mirrored onto the hedge leg within the same cycle.
//
// One mechanism covers every case: read the primary and hedge legs from
// virtual state, plan the delta (planHedgeConvergence), place reduce-only
// closes / market opens outside mu, and book confirmed fills under mu. This
// is deliberately NOT wired into the signal dispatch: closes that bypass
// dispatch (SL fills, CB) must converge through the same path.
//
// Fail-closed policy (issue constraint 4): if a hedge OPEN/ADD cannot be
// placed or confirmed — order error, size rounded to zero, missing marks,
// unverifiable or foreign on-chain state on the hedge coin — the engine
// immediately closes the UNCOVERED primary quantity reduce-only (the full
// leg for a fresh open/flip, the added delta for a scale-in) and alerts the
// operator. The primary is never left unhedged silently. Reduce/close
// failures alert and retry next cycle (an over-hedged residual reduces net
// exposure — safe direction — unlike a naked primary).

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// hedgeExecuteFn places a market open/add order for the hedge leg and returns
// the parsed execute result. Injectable so the engine is unit-testable
// without subprocesses (repo pattern: HyperliquidLiveCloser).
type hedgeExecuteFn func(sc StrategyConfig, coin, side string, size float64, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, error)

type hedgeSyncDeps struct {
	execute hedgeExecuteFn
	closer  HyperliquidLiveCloser
	ownerDM func(string)
}

func defaultHedgeSyncDeps(notifier *MultiNotifier) hedgeSyncDeps {
	return hedgeSyncDeps{
		execute: defaultHedgeExecute,
		closer:  defaultHyperliquidLiveCloser,
		ownerDM: notifier.SendOwnerDM,
	}
}

// defaultHedgeExecute shells to check_hyperliquid.py --execute for the hedge
// coin. No --stop-loss-pct (phase 1: the hedge carries no protection orders
// of its own), no SL-cancel OIDs; margin_mode/leverage come from the hedge
// block — the hedge coin needs its own on-chain margin assignment, never the
// primary's (issue constraint 3).
func defaultHedgeExecute(sc StrategyConfig, coin, side string, size float64, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, error) {
	execResult, _, err := RunHyperliquidExecute(sc.Script, coin, side, size, 0, 0, 0, sc.Hedge.MarginMode, sc.Hedge.Leverage, false, snapshot)
	if err != nil {
		return execResult, err
	}
	if execResult != nil && execResult.Error != "" {
		return execResult, fmt.Errorf("hedge execute error: %s", execResult.Error)
	}
	return execResult, nil
}

// hyperliquidOnChainSignedSize returns the signed on-chain size for coin (0
// when absent).
func hyperliquidOnChainSignedSize(positions []HLPosition, coin string) float64 {
	for i := range positions {
		if positions[i].Coin == coin {
			return positions[i].Size
		}
	}
	return 0
}

// runHedgeLegSync converges every hedge-enabled live HL perps strategy's
// hedge leg with its primary position. Must be called WITHOUT holding mu;
// spawns subprocesses. Returns the number of hedge trades booked.
func runHedgeLegSync(ctx context.Context, strategies []StrategyConfig, state *AppState, mu *sync.RWMutex, prices map[string]float64, hlPositions []HLPosition, hlStateFetched bool, deps hedgeSyncDeps, logMgr *LogManager) int {
	if state == nil || deps.closer == nil || deps.execute == nil {
		return 0
	}
	var hedged []StrategyConfig
	for _, sc := range strategies {
		if hedgeEnabled(sc) && sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
			hedged = append(hedged, sc)
		}
	}
	if len(hedged) == 0 {
		return 0
	}
	sort.Slice(hedged, func(i, j int) bool { return hedged[i].ID < hedged[j].ID })

	trades := 0
	for _, sc := range hedged {
		if err := ctx.Err(); err != nil {
			fmt.Printf("[CRITICAL] hedge-sync: budget exhausted before strategy %s: %v\n", sc.ID, err)
			return trades
		}
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		var logger *StrategyLogger
		if logMgr != nil {
			if lg, err := logMgr.GetStrategyLogger(sc.ID); err == nil {
				logger = lg
			}
		}
		trades += syncHedgeLegForStrategy(sc, ss, mu, prices, hlPositions, hlStateFetched, deps, logger)
		if logger != nil {
			logger.Close()
		}
	}
	return trades
}

func syncHedgeLegForStrategy(sc StrategyConfig, ss *StrategyState, mu *sync.RWMutex, prices map[string]float64, hlPositions []HLPosition, hlStateFetched bool, deps hedgeSyncDeps, logger *StrategyLogger) int {
	hcoin := hedgeCoin(sc)
	psym := hyperliquidSymbol(sc.Args)
	if hcoin == "" || psym == "" {
		return 0
	}

	mu.RLock()
	var prim *hedgePrimarySnapshot
	if pos, ok := ss.Positions[psym]; ok && pos != nil && !pos.IsHedge && pos.Quantity > hedgeQtyEpsilon {
		prim = &hedgePrimarySnapshot{Symbol: psym, Side: pos.Side, Quantity: pos.Quantity}
	}
	var hedge *hedgeLegSnapshot
	if hp := findHedgePosition(ss, sc); hp != nil {
		hedge = &hedgeLegSnapshot{Symbol: hp.Symbol, Side: hp.Side, Quantity: hp.Quantity, Covered: hp.HedgeCoveredPrimaryQty}
	}
	mu.RUnlock()

	// A state-only hedge under a different coin (config edited across a
	// restart) is converged on ITS coin, not the configured one — never
	// leave a live leg unmanaged.
	if hedge != nil && hedge.Symbol != hcoin {
		hcoin = hedge.Symbol
	}

	plan, err := planHedgeConvergence(sc.Hedge.Ratio, prim, hedge, prices[psym], prices[hcoin])
	if err != nil {
		if prim != nil && hedge == nil {
			// Could not size/plan the opening leg — constraint 4.
			return hedgeFailClosePrimary(sc, ss, mu, prim, 0, deps, logger,
				fmt.Sprintf("hedge open for %s could not be planned: %v", hcoin, err))
		}
		hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: cannot converge hedge leg %s: %v — manual intervention may be required", sc.ID, hcoin, err))
		return 0
	}

	if plan.StampCovered != nil {
		mu.Lock()
		if hp := findHedgePosition(ss, sc); hp != nil {
			if hp.HedgeCoveredPrimaryQty != *plan.StampCovered && logger != nil {
				logger.Info("[hedge] adopting covered watermark %.6f %s for %s leg", *plan.StampCovered, psym, hp.Symbol)
			}
			hp.HedgeCoveredPrimaryQty = *plan.StampCovered
		}
		mu.Unlock()
		return 0
	}
	if len(plan.Orders) == 0 {
		return 0
	}

	// Pre-flight for opening orders: the hedge coin's on-chain state must be
	// verifiable and unambiguous. Structurally (validateHedgeConfigs) no
	// configured strategy trades this coin, so an on-chain position with no
	// state-side hedge leg is foreign (operator/manual or a lost-state fill)
	// — fail closed per issue requirement 4: never place an order that would
	// blend into an unattributable position.
	needsOpen := false
	for _, o := range plan.Orders {
		if o.Side != "" {
			needsOpen = true
			break
		}
	}
	if needsOpen {
		if !hlStateFetched {
			return hedgeFailClosePrimary(sc, ss, mu, prim, hedgeUncoveredQty(prim, hedge), deps, logger,
				fmt.Sprintf("hedge open for %s refused: HL clearinghouse state unavailable this cycle (cannot verify the hedge coin is unowned)", hcoin))
		}
		onChain := hyperliquidOnChainSignedSize(hlPositions, hcoin)
		if hedge == nil && (onChain > hedgeQtyEpsilon || onChain < -hedgeQtyEpsilon) {
			return hedgeFailClosePrimary(sc, ss, mu, prim, hedgeUncoveredQty(prim, hedge), deps, logger,
				fmt.Sprintf("hedge open for %s refused: foreign on-chain position (size %.6f) already exists on the hedge coin — resolve it manually", hcoin, onChain))
		}
	}

	trades := 0
	hedgeOpenLegLive := hedge != nil
	for _, order := range plan.Orders {
		if order.Close {
			var partial *float64
			if !order.FullClose {
				v := order.Quantity
				partial = &v
			}
			result, err := deps.closer(hcoin, partial, nil)
			if err != nil {
				// The "retry next cycle" stance is safe ONLY while the
				// residual hedge is INVERSE to the primary (a true over-
				// hedge, which reduces net exposure). After a primary FLIP
				// the stale hedge is the SAME side as the flipped primary —
				// a failed close there holds 2× directional exposure with no
				// stop on the hedge leg, so de-risk the primary reduce-only
				// instead of passively retrying (review on #1333). Exposure
				// cannot compound across cycles: once the primary is closed,
				// later cycles only retry the stale-hedge close.
				if prim != nil && hedge != nil && hedge.Side == prim.Side {
					return trades + hedgeFailClosePrimary(sc, ss, mu, prim, 0, deps, logger,
						fmt.Sprintf("stale hedge leg %s could not be closed after a primary flip (%v) — the residual hedge is the SAME side as the flipped primary (2x directional exposure, no hedge stop)", hcoin, err))
				}
				hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: reduce-only close of hedge leg %s (qty %.6f, full=%t) failed: %v — residual hedge stays inverse to the primary (over-hedged, net exposure reduced) until the next cycle retry", sc.ID, hcoin, order.Quantity, order.FullClose, err))
				return trades
			}
			if result == nil || result.Close == nil || result.Close.AlreadyFlat || result.Close.Fill == nil {
				// State says the leg is open but the exchange has nothing to
				// close: reconcile owns gap booking (hl_sync_external with the
				// userFills VWAP) — never book a guessed fill here.
				if logger != nil {
					logger.Warn("[hedge] close of %s returned no fill (already flat?) — deferring to reconcile for gap booking", hcoin)
				}
				return trades
			}
			fill := result.Close.Fill
			mu.Lock()
			applyHyperliquidCircuitCloseFill(ss, hcoin, fill.TotalSz, fill.AvgPx, fill.Fee, 0, fill.OID, "hedge_sync")
			if !order.FullClose {
				if hp := findHedgePosition(ss, sc); hp != nil {
					hp.HedgeCoveredPrimaryQty = order.CoveredAfter
				}
			}
			mu.Unlock()
			trades++
			if !order.FullClose && fill.TotalSz < order.Quantity*0.99 {
				hedgeAlert(deps, logger, fmt.Sprintf("[WARN] hedge-sync %s: hedge reduce on %s under-filled %.6f/%.6f — residual over-hedge remains until the leg closes", sc.ID, hcoin, fill.TotalSz, order.Quantity))
			}
			if order.FullClose {
				hedgeOpenLegLive = false
			}
			continue
		}

		// Opening/adding market order.
		snapshot := hlExecuteSnapshotForCoin(hlPositions, hcoin)
		execResult, err := deps.execute(sc, hcoin, order.Side, order.Quantity, snapshot)
		var fill *HyperliquidFill
		if err == nil && execResult != nil && execResult.Execution != nil {
			fill = execResult.Execution.Fill
		}
		if err != nil || fill == nil || fill.TotalSz <= hedgeQtyEpsilon || fill.AvgPx <= 0 {
			why := "no fill returned"
			if err != nil {
				why = err.Error()
			}
			closeQty := hedgeUncoveredQty(prim, hedge)
			if !hedgeOpenLegLive {
				// Fresh open or the reopen leg of a flip whose close already
				// filled: the whole primary is unhedged.
				closeQty = 0
			}
			return trades + hedgeFailClosePrimary(sc, ss, mu, prim, closeQty, deps, logger,
				fmt.Sprintf("hedge %s order on %s (qty %.6f) failed: %s", order.Side, hcoin, order.Quantity, why))
		}
		// A partial fill covers proportionally less of the primary than the
		// order intended: stamping the full CoveredAfter would record coverage
		// the leg doesn't have, so the next cycle sees primary==covered and
		// never re-adds the shortfall — a silent, permanent under-hedge that
		// reconcile cannot repair (state qty == on-chain qty, no drift).
		// Scale the watermark by the actual fill ratio so the shortfall
		// re-triggers an add next cycle, and alert: under-hedged is the
		// unsafe direction (review on #1333).
		coveredAfter := order.CoveredAfter
		if fill.TotalSz < order.Quantity*(1-hedgeCoveredRelEpsilon) {
			coveredBefore := 0.0
			if hedgeOpenLegLive && hedge != nil {
				coveredBefore = hedge.Covered
			}
			coveredAfter = coveredBefore + (order.CoveredAfter-coveredBefore)*fill.TotalSz/order.Quantity
			hedgeAlert(deps, logger, fmt.Sprintf("[WARN] hedge-sync %s: hedge %s on %s under-filled %.6f/%.6f — covered watermark scaled to %.6f of %.6f; the primary is under-hedged until the next cycle re-adds the shortfall", sc.ID, order.Side, hcoin, fill.TotalSz, order.Quantity, coveredAfter, order.CoveredAfter))
		}
		mu.Lock()
		if hedgeOpenLegLive {
			if hp := findHedgePosition(ss, sc); hp != nil {
				applyHedgeAddFill(ss, hp, fill.TotalSz, fill.AvgPx, fill.Fee, fill.OID, coveredAfter, logger)
			}
		} else {
			applyHedgeOpenFill(ss, sc, psym, inverseSide(prim.Side), fill.TotalSz, fill.AvgPx, fill.Fee, fill.OID, coveredAfter, logger)
			hedgeOpenLegLive = true
		}
		mu.Unlock()
		trades++
	}
	return trades
}

// hedgeUncoveredQty returns the primary quantity NOT yet covered by the hedge
// leg — the fail-closed close size for a failed ADD (0 = close the full leg,
// used when there is no live hedge at all).
func hedgeUncoveredQty(prim *hedgePrimarySnapshot, hedge *hedgeLegSnapshot) float64 {
	if prim == nil {
		return 0
	}
	if hedge == nil || hedge.Covered <= hedgeQtyEpsilon {
		return 0 // full close
	}
	delta := prim.Quantity - hedge.Covered
	if delta <= hedgeQtyEpsilon {
		return 0
	}
	return delta
}

// hedgeFailClosePrimary implements issue constraint 4: reduce-only close of
// the uncovered primary quantity (closeQty <= 0 → the full virtual leg) after
// a hedge open/add failure, booking whatever filled, then alerting the
// operator. The close is ALWAYS sized (partialSz set, never nil) so a shared
// primary coin's peer exposure is never flattened. Returns booked trades.
func hedgeFailClosePrimary(sc StrategyConfig, ss *StrategyState, mu *sync.RWMutex, prim *hedgePrimarySnapshot, closeQty float64, deps hedgeSyncDeps, logger *StrategyLogger, why string) int {
	if prim == nil {
		hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: %s", sc.ID, why))
		return 0
	}
	mu.RLock()
	pos := ss.Positions[prim.Symbol]
	var qty float64
	var cancelOIDs []int64
	if pos != nil && !pos.IsHedge && pos.Quantity > hedgeQtyEpsilon {
		qty = pos.Quantity
		if closeQty > hedgeQtyEpsilon && closeQty < qty {
			qty = closeQty
		} else {
			// Full-leg close: cancel this strategy's resting protection so the
			// flatten doesn't orphan trigger orders (#421/#479 convention).
			cancelOIDs = hyperliquidProtectionCancelOIDs(pos)
		}
	}
	mu.RUnlock()
	if qty <= hedgeQtyEpsilon {
		hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: %s (primary already flat — nothing to de-risk)", sc.ID, why))
		return 0
	}

	partial := qty
	result, err := deps.closer(prim.Symbol, &partial, cancelOIDs)
	if err != nil {
		hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: %s — AND the fail-closed primary close of %.6f %s FAILED (%v). The position is RUNNING UNHEDGED; the engine retries next cycle", sc.ID, why, qty, prim.Symbol, err))
		return 0
	}
	trades := 0
	if result != nil && result.Close != nil && !result.Close.AlreadyFlat && result.Close.Fill != nil {
		fill := result.Close.Fill
		mu.Lock()
		applyHyperliquidCircuitCloseFill(ss, prim.Symbol, fill.TotalSz, fill.AvgPx, fill.Fee, 0, fill.OID, "hedge_open_failed")
		if result.CancelStopLossSucceeded {
			if p, ok := ss.Positions[prim.Symbol]; ok && p != nil {
				p.StopLossOID = 0
				p.TPOIDs = nil
			}
		}
		mu.Unlock()
		trades++
	}
	hedgeAlert(deps, logger, fmt.Sprintf("[CRITICAL] hedge-sync %s: %s — closed %.6f %s reduce-only (fail-closed per #1159 constraint 4: never run unhedged silently)", sc.ID, why, qty, prim.Symbol))
	return trades
}

func hedgeAlert(deps hedgeSyncDeps, logger *StrategyLogger, msg string) {
	if logger != nil {
		logger.Error("%s", msg)
	}
	fmt.Println(msg)
	if deps.ownerDM != nil {
		deps.ownerDM(msg)
	}
}
