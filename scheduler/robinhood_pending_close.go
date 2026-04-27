package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RobinhoodPendingCloseOwnerDM is the callback the drain uses to notify the
// operator when a per-strategy CB cannot be closed because the coin is shared
// by multiple live configured Robinhood crypto strategies on the same account.
// The function is expected to post a DM and return; nil-safe (drain falls back
// to log-only when no DM sender is wired — matches test usage).
type RobinhoodPendingCloseOwnerDM func(message string)

// runPendingRobinhoodCircuitCloses drains the robinhood entry of
// RiskState.PendingCircuitCloses for every strategy, submitting full-account
// market_sell closes outside the state mutex. Retries next scheduler cycle on
// failure.
//
// Two correctness gates beyond the HL/OKX drain analogs:
//
//  1. Sole-ownership gate: Robinhood crypto has no reduce-only primitive;
//     a market_sell of BTC consumes the entire on-account BTC balance. When
//     two live configured RH crypto strategies trade the same coin on the
//     same account, no strategy-local CB can safely close that coin without
//     blasting the other strategy's exposure. On detection: emit a CRITICAL
//     log, DM the owner once per drain cycle, and clear the pending. The
//     stuck-CB recovery path reapplies the same gate so DMs fire on every
//     cycle the strategy remains in the shared-and-latched state — operator
//     intervention is the only resolution.
//
//  2. Stuck-CB recovery: mirrors the HL drain. If a CB fires on a cycle
//     where the RH positions fetch failed, setRobinhoodCircuitBreakerPending
//     bails (no RHPositions in the assist) and the pending is never set.
//     Subsequent CheckRisk calls early-return with "circuit breaker active"
//     without re-enqueuing. The drain detects latched-CB strategies (live RH
//     crypto, CircuitBreaker=true, no pending, non-zero on-account position)
//     and reconstructs the pending — gated on sole-ownership.
//
// When sendOwnerDM is nil the drain logs the skip and does not DM (tests pass
// nil to keep the pure-function contract).
func runPendingRobinhoodCircuitCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	positions []RobinhoodPosition,
	positionsFetched bool,
	fetcher RobinhoodPositionsFetcher,
	closer RobinhoodLiveCloser,
	sendOwnerDM RobinhoodPendingCloseOwnerDM,
	totalBudget time.Duration,
	mu *sync.RWMutex,
	notifier operatorRequiredNotifier,
) {
	if closer == nil || state == nil {
		return
	}

	// Roster of live RH crypto strategies from cfg — used for both stuck-CB
	// recovery and the sole-ownership check.
	var rhLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "robinhood" && sc.Type == "spot" && robinhoodIsLive(sc.Args) {
			rhLiveAll = append(rhLiveAll, sc)
		}
	}
	if len(rhLiveAll) == 0 {
		return
	}

	// Phase 1: snapshot — detect pending jobs AND stuck-CB strategies needing
	// pending reconstruction.
	mu.RLock()
	hasPending := false
	hasStuckCB := false
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
			hasPending = true
		}
	}
	for _, sc := range rhLiveAll {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) == nil && ss.RiskState.CircuitBreaker {
			hasStuckCB = true
			break
		}
	}
	mu.RUnlock()

	if !hasPending && !hasStuckCB {
		return
	}

	ctxOverall, cancelOverall := context.WithTimeout(ctx, totalBudget)
	defer cancelOverall()

	// Lazy fetch if we weren't handed positions this cycle — mirrors HL.
	if !positionsFetched && fetcher != nil {
		pos, err := fetcher()
		if err != nil {
			fmt.Printf("[CRITICAL] rh-circuit-close: cannot fetch RH positions: %v — will retry next cycle\n", err)
			return
		}
		positions = pos
		positionsFetched = true
	}
	if !positionsFetched {
		fmt.Printf("[CRITICAL] rh-circuit-close: no RH positions snapshot available — will retry next cycle\n")
		return
	}

	// Phase 2: reconstruct pending for stuck-CB strategies, applying the
	// sole-ownership gate BEFORE enqueueing so shared-coin strategies never
	// latch pending state (avoids silent loop churn).
	if hasStuckCB {
		recoverOrder := make([]StrategyConfig, len(rhLiveAll))
		copy(recoverOrder, rhLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		mu.Lock()
		for _, sc := range recoverOrder {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
				continue
			}
			if !ss.RiskState.CircuitBreaker {
				continue
			}
			coin := robinhoodSymbol(sc.Args)
			if coin == "" {
				continue
			}
			qty := robinhoodOnAccountSize(coin, positions)
			if qty <= 0 {
				continue
			}
			peers := rhLiveStrategiesForCoin(coin, rhLiveAll)
			if len(peers) > 1 {
				// Shared-owner — don't enqueue; DM from the submit phase below
				// already-pending strategies. For stuck-CB strategies we also
				// surface the skip here so the operator hears about it even if
				// no pending ever made it into state.
				msg := formatRobinhoodSharedOwnerDM(sc.ID, coin, peers)
				fmt.Printf("[CRITICAL] rh-circuit-close: %s\n", msg)
				if sendOwnerDM != nil {
					sendOwnerDM(msg)
				}
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{
				Symbols: []PendingCircuitCloseSymbol{{Symbol: coin, Size: qty}},
			})
			fmt.Printf("[CRITICAL] rh-circuit-close: recovered pending for strategy %s coin %s size=%.8f (CB latched, RH fetch had failed at fire time)\n",
				sc.ID, coin, qty)
		}
		mu.Unlock()
	}

	// Phase 3: re-snapshot jobs (may now include recovered entries).
	type job struct {
		stratID string
		pending PendingCircuitClose
	}
	var jobs []job
	mu.RLock()
	for id, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
		if p == nil || len(p.Symbols) == 0 {
			continue
		}
		jobs = append(jobs, job{id, *p})
	}
	mu.RUnlock()

	if len(jobs) == 0 {
		return
	}

	// Deterministic drain order for operator-facing logs (#356 review finding 2).
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].stratID < jobs[j].stratID })

	for _, j := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] rh-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "robinhood" || sc.Type != "spot" || !robinhoodIsLive(sc.Args) {
			// Strategy was removed from config (or flipped to paper / non-RH)
			// between enqueue and drain — clear the orphaned pending leg so
			// the map does not leak stale entries forever. Same bail-out as
			// the HL / OKX drains.
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		for _, c := range j.pending.Symbols {
			// Defense in depth: re-check the on-account balance right before
			// submit (it may have drained since enqueue via stuck-CB recovery
			// or manual intervention). Zero → already flat; skip silently.
			onAccount := robinhoodOnAccountSize(c.Symbol, positions)
			if onAccount <= 0 {
				continue
			}

			// Sole-ownership gate — checked BEFORE ctxOverall so the DM fires
			// on the same cycle the drain reached this leg even if the
			// remaining budget has been exhausted. DM formatting is purely
			// local work (no RPC) so honoring it under an expired budget is
			// safe. Shared-coin strategies DM the owner and clear the pending
			// for this coin; the stuck-CB recovery path will re-surface the
			// same DM next cycle while the CB stays latched.
			peers := rhLiveStrategiesForCoin(c.Symbol, rhLiveAll)
			if len(peers) > 1 {
				msg := formatRobinhoodSharedOwnerDM(j.stratID, c.Symbol, peers)
				fmt.Printf("[CRITICAL] rh-circuit-close: %s\n", msg)
				if sendOwnerDM != nil {
					sendOwnerDM(msg)
				}
				// Drop this leg but keep the overall success flag honest:
				// we did NOT close the position, so don't report success.
				allOK = false
				continue
			}

			// Submit gate: Robinhood TOTP login + market_sell are the only
			// RPCs in this loop, so the overall-budget guard sits here (not
			// at the top of the iteration).
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}

			result, err := closer(c.Symbol)
			if err != nil {
				errMsg := err.Error()
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s failed: %v\n", j.stratID, c.Symbol, err)
				allOK = false
				now := time.Now().UTC()
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood); p != nil {
						p.FailureCount++
						if shouldNotifyDrainFailure(p.FailureCount, p.LastNotifiedAt, now) {
							p.LastNotifiedAt = now
							mu.Unlock()
							if notifier != nil && notifier.HasBackends() {
								msg := formatDrainFailureAlert("robinhood", j.stratID, c.Symbol, c.Size, errMsg, p.FailureCount)
								notifier.SendToAllChannels(msg)
								notifier.SendOwnerDM(msg)
							}
							mu.Lock()
						}
					}
				}
				mu.Unlock()
				break
			}
			if result != nil && result.Close != nil && result.Close.AlreadyFlat {
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s already flat on-account (no-op)\n", j.stratID, c.Symbol)
				continue
			}
			fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s submitted market_sell size=%.8f\n",
				j.stratID, c.Symbol, onAccount)
		}

		if allOK {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
			}
			mu.Unlock()
			continue
		}

		// Shared-ownership skip: clear the pending so CheckRisk's stuck-CB
		// recovery controls whether to re-enqueue next cycle. If the shared
		// configuration persists, recovery's sole-owner gate will again skip
		// + DM, giving the operator a steady audit trail until they fix it.
		// For genuine submit errors we preserve pending so the next cycle's
		// drain retries (same semantics as HL).
		mu.Lock()
		if ss := state.Strategies[j.stratID]; ss != nil {
			sharedOnly := true
			for _, c := range j.pending.Symbols {
				peers := rhLiveStrategiesForCoin(c.Symbol, rhLiveAll)
				if len(peers) <= 1 {
					sharedOnly = false
					break
				}
			}
			if sharedOnly {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
			}
		}
		mu.Unlock()
	}
}

// rhLiveStrategiesForCoin returns the subset of live configured RH crypto
// strategies trading the given coin. Used by the drain to detect shared
// ownership of an on-account balance (see runPendingRobinhoodCircuitCloses).
func rhLiveStrategiesForCoin(coin string, rhLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range rhLiveAll {
		if robinhoodSymbol(sc.Args) == coin {
			out = append(out, sc)
		}
	}
	return out
}

// formatRobinhoodSharedOwnerDM formats the DM sent when a per-strategy CB
// cannot safely close a shared-ownership RH crypto coin. Exported shape in
// tests: asserts strategy / coin / peer list ordering is deterministic.
func formatRobinhoodSharedOwnerDM(firingStrategyID, coin string, peers []StrategyConfig) string {
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)
	return fmt.Sprintf(
		"Robinhood CB close skipped: strategy %s tripped on coin %s, but %d live strategies share that coin (%v). Robinhood crypto has no reduce-only primitive, so CB close cannot run safely. Manual intervention required.",
		firingStrategyID, coin, len(peers), ids,
	)
}
