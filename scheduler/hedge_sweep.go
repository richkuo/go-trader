package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// hedgeSweepAction identifies what runHedgeCoherenceSweep should do for one
// hedgeCoherenceJob.
type hedgeSweepAction string

const (
	// hedgeSweepClosePrimary fail-closes an unhedged primary (P present,
	// H absent) — constraint 4's containment for every async path that can
	// produce this state (crash between legs, retried unwind, externally
	// closed hedge).
	hedgeSweepClosePrimary hedgeSweepAction = "close_primary"
	// hedgeSweepCloseHedge closes an orphaned hedge (H present, P absent) —
	// the primary closed via SL/TP/manual/CB/kill-switch while the hedge
	// close failed, was pending, or hadn't run yet.
	hedgeSweepCloseHedge hedgeSweepAction = "close_hedge"
	// hedgeSweepReduceHedge shrinks an over-sized hedge back toward the
	// configured ratio (primary was reduced asynchronously — SL/TP fill,
	// reconcile-detected partial close — outside the synchronous dispatch
	// mirror).
	hedgeSweepReduceHedge hedgeSweepAction = "reduce_hedge"
)

// hedgeSweepDustTolerance bounds the "both open" oversized-hedge check so sz-
// decimal rounding and mark-price noise between primary/hedge marks don't
// cause reduce-then-immediately-need-to-grow-again churn every cycle.
const hedgeSweepDustTolerance = 0.02 // 2%

// hedgeCoherenceJob is one row of primary/hedge divergence detected by
// snapshotHedgeCoherenceJobs, repaired by runHedgeCoherenceSweep.
type hedgeCoherenceJob struct {
	StrategyID    string
	PrimaryCoin   string
	HedgeCoin     string
	Action        hedgeSweepAction
	Qty           float64 // close_primary: primary qty to unwind. close_hedge: informational (full close ignores it). reduce_hedge: hedge qty to shed.
	PrimaryShared bool    // primary coin has live peers — close_primary must be sized, never sz=None.
}

// snapshotHedgeCoherenceJobs compares each hedge-enabled live HL perps
// strategy's primary position against its hedge position and returns the
// repair jobs per the #1159 coherence-sweep table. Must be called under
// mu.RLock — this is the SINGLE repair-detection engine for every
// asynchronous path that can desync the pair: on-chain SL fills booked by
// reconcile, kill-switch/CB residue, manual force-close, externally-closed
// legs, and restart recovery from persisted state. Because a failed hedge
// order never mutates state (existing live-exec guardrail), re-detecting the
// same divergence next cycle IS the retry mechanism — no separate pending-
// action table is needed.
func snapshotHedgeCoherenceJobs(state *AppState, strategies []StrategyConfig, hlPeerAll []StrategyConfig) []hedgeCoherenceJob {
	var jobs []hedgeCoherenceJob
	for _, sc := range strategies {
		if !sc.HedgeEnabled() || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
			continue
		}
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		primaryCoin := hyperliquidConfiguredCoin(sc)
		hedgeSym := hedgeCoin(sc)
		if primaryCoin == "" || hedgeSym == "" {
			continue
		}
		var primary, hedge *Position
		if p, ok := ss.Positions[primaryCoin]; ok {
			primary = p
		}
		if h, ok := ss.Positions[hedgeSym]; ok {
			hedge = h
		}
		primaryOpen := primary != nil && primary.Quantity > 0
		hedgeOpen := hedge != nil && hedge.Quantity > 0

		switch {
		case !primaryOpen && !hedgeOpen:
			continue
		case primaryOpen && !hedgeOpen:
			shared := len(hlLiveStrategiesForCoin(primaryCoin, hlPeerAll)) > 1
			jobs = append(jobs, hedgeCoherenceJob{
				StrategyID: sc.ID, PrimaryCoin: primaryCoin, HedgeCoin: hedgeSym,
				Action: hedgeSweepClosePrimary, Qty: primary.Quantity, PrimaryShared: shared,
			})
		case !primaryOpen && hedgeOpen:
			jobs = append(jobs, hedgeCoherenceJob{
				StrategyID: sc.ID, PrimaryCoin: primaryCoin, HedgeCoin: hedgeSym,
				Action: hedgeSweepCloseHedge, Qty: hedge.Quantity,
			})
		default:
			// Both open: converge an over-sized hedge back toward the
			// configured ratio. Never auto-upsizes an under-sized hedge
			// outside a primary entry event (an up-sizing order outside
			// that event risks compounding a booking error) — alert-only,
			// no job.
			//
			// #1337 review: the target MUST be computed from each leg's own
			// entry-price accounting (AvgCost), never live marks. AvgCost is
			// a trade-derived value that only changes on an actual fill
			// (open/scale-in add) — unlike a live mid, it does not drift
			// with ordinary relative price movement between the primary and
			// hedge coins. Using live marks here previously fired a
			// reduce_hedge job on price noise alone (no quantity change on
			// either leg), permanently ratcheting the hedge smaller on every
			// adverse relative move with no path to re-grow it. Recomputing
			// from AvgCost reproduces the EXACT original sizing at rest
			// (target == hedge.Quantity when nothing has actually traded)
			// and only diverges when primary.Quantity itself changes — the
			// real event this branch exists to react to (an async
			// reconcile-booked partial close that bypassed the synchronous
			// dispatch mirror).
			if primary.AvgCost <= 0 || hedge.AvgCost <= 0 {
				continue
			}
			targetHedgeQty, ok := hedgeOpenQty(primary.Quantity, primary.AvgCost, hedgeRatio(sc), hedge.AvgCost)
			if !ok {
				continue
			}
			if hedge.Quantity > targetHedgeQty*(1+hedgeSweepDustTolerance) {
				jobs = append(jobs, hedgeCoherenceJob{
					StrategyID: sc.ID, PrimaryCoin: primaryCoin, HedgeCoin: hedgeSym,
					Action: hedgeSweepReduceHedge, Qty: hedge.Quantity - targetHedgeQty,
				})
			}
		}
	}
	return jobs
}

// runHedgeCoherenceSweep executes the jobs snapshotHedgeCoherenceJobs
// detected: snapshot under RLock (by the caller, via the jobs param),
// on-chain close orders outside any lock, apply under Lock — the same shape
// as runRegimeDirectionOrphanCloses. Runs every cycle after reconcile and
// drainPendingManualActions, before per-strategy dispatch.
//
// hlPositions is the cycle's already-fetched on-chain snapshot, threaded
// through so runHedgeSweepClosePrimary can check for a crash-between-legs
// orphan hedge before giving up and fail-closing the primary (#1337 review).
func runHedgeCoherenceSweep(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	jobs []hedgeCoherenceJob,
	hlPositions []HLPosition,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logMgr *LogManager,
) {
	if ctx == nil || state == nil || len(jobs) == 0 {
		return
	}
	ctxOverall, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	strategyByID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		strategyByID[sc.ID] = sc
	}

	for _, job := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] hedge-sweep: budget exhausted: %v\n", err)
			return
		}
		sc, ok := strategyByID[job.StrategyID]
		if !ok {
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			logger = nil
		}

		switch job.Action {
		case hedgeSweepClosePrimary:
			runHedgeSweepClosePrimary(sc, state, mu, job, hlPositions, notifier, logger)
		case hedgeSweepCloseHedge:
			runHedgeSweepCloseHedge(sc, state, mu, job, notifier, logger)
		case hedgeSweepReduceHedge:
			runHedgeSweepReduceHedge(sc, state, mu, job, notifier, logger)
		}
	}
}

// onChainCoinQty returns the unsigned on-chain size for coin from the
// cycle's HL positions snapshot, or (0, false) if the coin has no non-zero
// entry.
func onChainCoinQty(hlPositions []HLPosition, coin string) (float64, bool) {
	for i := range hlPositions {
		if hlPositions[i].Coin != coin {
			continue
		}
		sz := hlPositions[i].Size
		if sz < 0 {
			sz = -sz
		}
		if sz <= 1e-15 {
			return 0, false
		}
		return sz, true
	}
	return 0, false
}

// runHedgeSweepClosePrimary handles a P-without-H divergence. Before
// abandoning the hedge and fail-closing the primary, it checks the cycle's
// on-chain snapshot for a REAL position on the hedge coin (#1337 review,
// Requires Human Review: the crash-between-legs window where
// mirrorHedgeOpen's on-chain order confirmed but applyHedgeOpenFill's state
// write never committed). Collision rejection guarantees the hedge coin is
// exclusively reserved for this strategy — no other strategy can ever be
// configured to trade it — so a non-zero position found there is
// unambiguously this strategy's, not a guess or coin->config inference
// (constraint 5 is about not attributing ownership from static config
// matching; this is a positive on-chain observation on a coin only this
// strategy could have placed an order on). When found, the orphan hedge is
// recovered (closed, booked defensively with no persisted-position basis —
// same primitive the CB/kill-switch paths already use for exactly this
// shape) and the primary is LEFT ALONE, since real hedge coverage existed.
// Only when no on-chain position exists on the hedge coin does this fall
// through to the original fail-closed primary unwind — sized (never
// sz=None), since the primary coin is not guaranteed sole-owned like the
// hedge coin is.
func runHedgeSweepClosePrimary(sc StrategyConfig, state *AppState, mu *sync.RWMutex, job hedgeCoherenceJob, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger) {
	if onChainQty, found := onChainCoinQty(hlPositions, job.HedgeCoin); found {
		runHedgeSweepRecoverCrashOrphanHedge(sc, state, mu, job, onChainQty, notifier, logger)
		return
	}

	qty := job.Qty
	closeResult, _, err := RunHyperliquidClose(sc.Script, job.PrimaryCoin, &qty, nil)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "sweep: fail-closed primary unwind failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-sweep-close-primary", job.PrimaryCoin, errMsg)
		if logger != nil {
			logger.Error("hedge-sweep: strategy %s is running an UNHEDGED primary on %s (hedge %s absent, no on-chain hedge position found) and the fail-closed unwind failed (%s) — will retry next cycle", sc.ID, job.PrimaryCoin, job.HedgeCoin, errMsg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss != nil {
		bookPerpsCloseWithFillFee(ss, job.PrimaryCoin, fill.AvgPx, fill.Fee, true, closeOID, "hedge_sweep_unhedged_primary_close", "HEDGE-SWEEP unhedged-primary close", "HEDGE-SWEEP unhedged-primary close", logger)
	}
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-sweep-close-primary", job.PrimaryCoin)
	msg := fmt.Sprintf("strategy %s: hedge-sweep closed an unhedged primary position on %s (hedge %s was absent, and no on-chain hedge position was found either — crash between legs, a retried unwind, or an externally-closed hedge leg).", sc.ID, job.PrimaryCoin, job.HedgeCoin)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}

// runHedgeSweepRecoverCrashOrphanHedge closes a REAL on-chain hedge position
// that has no persisted IsHedge Position row — the crash-between-legs
// window described on runHedgeSweepClosePrimary. Full close (sz=None) is
// safe: collision rejection guarantees sole ownership of the hedge coin.
// Booked via applyHyperliquidCircuitCloseFill, the same primitive the CB/
// kill-switch paths use, which already handles "real fill, no persisted
// position to decrement" by recording a defensive Trade with no PnL
// accounting (no AvgCost basis exists to compute PnL against) rather than
// inventing one.
func runHedgeSweepRecoverCrashOrphanHedge(sc StrategyConfig, state *AppState, mu *sync.RWMutex, job hedgeCoherenceJob, onChainQty float64, notifier *MultiNotifier, logger *StrategyLogger) {
	closeResult, _, err := runHyperliquidHedgeCloseOrder(sc, nil)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "sweep: crash-orphan hedge recovery close failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-sweep-recover-orphan", job.HedgeCoin, errMsg)
		if logger != nil {
			logger.Error("hedge-sweep: strategy %s has a real on-chain hedge position on %s (qty=%.6f) with NO persisted pairing state (crash between legs) — the recovery close failed (%s); primary on %s left untouched since real hedge coverage exists — will retry next cycle", sc.ID, job.HedgeCoin, onChainQty, errMsg, job.PrimaryCoin)
		}
		msg := fmt.Sprintf("strategy %s: found an untracked on-chain hedge position on %s (qty=%.6f, no persisted state — crash between legs) but the recovery close FAILED — manual reconciliation required on Hyperliquid.", sc.ID, job.HedgeCoin, onChainQty)
		if notifier != nil {
			notifier.SendOwnerDM(msg)
			notifier.SendToAllChannels(msg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID int64
	if fill.OID != 0 {
		closeOID = fill.OID
	}
	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss != nil {
		applyHyperliquidCircuitCloseFill(ss, job.HedgeCoin, fill.TotalSz, fill.AvgPx, fill.Fee, onChainQty, closeOID, true, "hedge_crash_recovery_orphan_close")
	}
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-sweep-recover-orphan", job.HedgeCoin)
	msg := fmt.Sprintf("strategy %s: recovered and closed an untracked on-chain hedge position on %s (qty=%.6f, no persisted pairing state — crash between the hedge-open order confirming and its state write committing). Primary on %s left untouched since real hedge coverage existed. No PnL basis was available for this leg (no persisted entry) — review trade_diagnostics for the defensive close row.", sc.ID, job.HedgeCoin, onChainQty, job.PrimaryCoin)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
		notifier.SendToAllChannels(msg)
	}
}

// runHedgeSweepCloseHedge closes an orphaned hedge leg. Safe to use sz=None
// (full close) — the hedge coin is sole-owned by construction.
func runHedgeSweepCloseHedge(sc StrategyConfig, state *AppState, mu *sync.RWMutex, job hedgeCoherenceJob, notifier *MultiNotifier, logger *StrategyLogger) {
	closeResult, _, err := runHyperliquidHedgeCloseOrder(sc, nil)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "sweep: orphan hedge close failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-sweep-close-hedge", job.HedgeCoin, errMsg)
		if logger != nil {
			logger.Error("hedge-sweep: orphaned hedge leg on %s (primary %s already closed) close failed (%s) — will retry next cycle", job.HedgeCoin, job.PrimaryCoin, errMsg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss != nil {
		applyHedgeCloseFill(ss, job.PrimaryCoin, job.HedgeCoin, fill.AvgPx, fill.Fee, true, closeOID, "hedge_orphan_close", logger)
	}
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-sweep-close-hedge", job.HedgeCoin)
	msg := fmt.Sprintf("strategy %s: hedge-sweep closed an orphaned hedge leg on %s (primary %s had already closed).", sc.ID, job.HedgeCoin, job.PrimaryCoin)
	if notifier != nil {
		notifier.SendOwnerDM(msg)
	}
}

// runHedgeSweepReduceHedge shrinks an over-sized hedge leg toward the
// configured ratio, sized (not sz=None, since this is a partial reduce).
func runHedgeSweepReduceHedge(sc StrategyConfig, state *AppState, mu *sync.RWMutex, job hedgeCoherenceJob, notifier *MultiNotifier, logger *StrategyLogger) {
	qty := job.Qty
	closeResult, _, err := runHyperliquidHedgeCloseOrder(sc, &qty)
	if err != nil || closeResult == nil || closeResult.Close == nil || closeResult.Close.Fill == nil {
		errMsg := "sweep: hedge reduce failed"
		if err != nil {
			errMsg = err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			errMsg = closeResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge-sweep-reduce", job.HedgeCoin, errMsg)
		if logger != nil {
			logger.Warn("hedge-sweep: over-sized hedge leg on %s reduce failed (%s) — will retry next cycle", job.HedgeCoin, errMsg)
		}
		return
	}
	fill := closeResult.Close.Fill
	var closeOID string
	if fill.OID != 0 {
		closeOID = fmt.Sprintf("%d", fill.OID)
	}
	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss != nil {
		applyHedgeReduceFill(ss, job.HedgeCoin, fill.TotalSz, fill.AvgPx, fill.Fee, true, closeOID, "hedge_sweep_reduce", logger)
	}
	mu.Unlock()
	clearLiveExecThrottle(sc, "hedge-sweep-reduce", job.HedgeCoin)
}
