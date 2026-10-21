package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type RobinhoodPendingCloseOwnerDM func(message string)

type robinhoodCircuitCloseDeps struct {
	store   *StateStore
	prices  map[string]float64
	resolve func() map[string]error
}

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
	deps robinhoodCircuitCloseDeps,
) {
	if closer == nil || state == nil {
		return
	}

	var rhLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "robinhood" && sc.Type == "spot" && robinhoodIsLive(sc.Args) {
			rhLiveAll = append(rhLiveAll, sc)
		}
	}
	if len(rhLiveAll) == 0 {
		return
	}

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

	unresolved := map[string]error{}
	if deps.resolve != nil {
		unresolved = deps.resolve()
	}
	mu.RLock()
	freshLongs := robinhoodFreshLongsLocked(state, strategies, time.Now())
	mu.RUnlock()

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

	if hasStuckCB {
		recoverOrder := make([]StrategyConfig, len(rhLiveAll))
		copy(recoverOrder, rhLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		var flatCleaned []*StrategyState
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
				if lag := robinhoodFlatReadLags(coin, qty, freshLongs); lag != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: %v — book kept\n", sc.ID, coin, lag)
					continue
				}
				if _, held := unresolved[coin]; !held {
					if pos := ss.Positions[coin]; pos != nil && len(rhLiveStrategiesForCoin(coin, rhLiveAll)) == 1 {
						fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s is flat on a fresh holdings read with no unresolved order; closing the book's %.8f as verified flat\n", sc.ID, coin, pos.Quantity)
						closeRobinhoodBookVerifiedFlat(ss, sc, deps.prices, map[string]bool{coin: true}, robinhoodVerifiedFlatCircuitBreaker, nil)
						flatCleaned = append(flatCleaned, ss)
					}
				}
				continue
			}
			peers := rhLiveStrategiesForCoin(coin, rhLiveAll)
			if len(peers) > 1 {
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
		if deps.store != nil {
			for _, ss := range flatCleaned {
				if err := deps.store.SaveStrategyBook(ss); err != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: saving the verified-flat close for %s failed: %v — it retries with the next save\n", ss.ID, err)
				}
			}
		}
		mu.Unlock()
	}

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

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].stratID < jobs[j].stratID })

	for _, j := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] rh-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "robinhood" || sc.Type != "spot" || !robinhoodIsLive(sc.Args) {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		var failedCoin string
		var failedSize float64
		var failedErr error
		var candidates []string
		for _, c := range j.pending.Symbols {
			if held, ok := unresolved[c.Symbol]; ok {
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s waits for an unresolved order: %v\n", j.stratID, c.Symbol, held)
				allOK = false
				continue
			}
			onAccount := robinhoodOnAccountSize(c.Symbol, positions)
			if onAccount <= 0 {
				if lag := robinhoodFlatReadLags(c.Symbol, onAccount, freshLongs); lag != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: %v — pending close kept\n", j.stratID, c.Symbol, lag)
					allOK = false
					continue
				}
				candidates = append(candidates, c.Symbol)
				continue
			}

			peers := rhLiveStrategiesForCoin(c.Symbol, rhLiveAll)
			if len(peers) > 1 {
				msg := formatRobinhoodSharedOwnerDM(j.stratID, c.Symbol, peers)
				fmt.Printf("[CRITICAL] rh-circuit-close: %s\n", msg)
				if sendOwnerDM != nil {
					sendOwnerDM(msg)
				}
				allOK = false
				continue
			}

			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}

			result, err := closer(c.Symbol)
			if err != nil {
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s failed: %v\n", j.stratID, c.Symbol, err)
				allOK = false
				failedCoin = c.Symbol
				failedSize = c.Size
				failedErr = err
				break
			}
			if result != nil && result.Close != nil && result.Close.AlreadyFlat {
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s already flat on-account (no-op)\n", j.stratID, c.Symbol)
			} else {
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s confirmed venue sell of size=%.8f\n",
					j.stratID, c.Symbol, onAccount)
			}
			candidates = append(candidates, c.Symbol)
		}

		if allOK && len(candidates) > 0 {
			var post []RobinhoodPosition
			var verifyErr error
			if fetcher == nil {
				verifyErr = fmt.Errorf("no strict holdings fetcher is wired")
			} else {
				post, verifyErr = fetcher()
			}
			for _, coin := range candidates {
				switch {
				case verifyErr != nil:
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: post-close holdings read failed: %v — pending close kept\n", j.stratID, coin, verifyErr)
					allOK = false
					failedCoin, failedErr = coin, fmt.Errorf("post-close holdings read failed: %v", verifyErr)
				case robinhoodOnAccountSize(coin, post) > 0:
					q := robinhoodOnAccountSize(coin, post)
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s still held %.8f after close — pending close kept\n", j.stratID, coin, q)
					allOK = false
					failedCoin, failedSize, failedErr = coin, q, fmt.Errorf("still held %.8f after close", q)
				}
			}
		}

		if deps.store != nil {
			settleRobinhoodAttempts(robinhoodEmergencyDeps{store: deps.store, state: state, mu: mu, cfgs: strategies}, nil, nil)
		}

		if allOK {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				stillUnresolved := false
				for _, coin := range candidates {
					if robinhoodUnresolvedForCoinLocked(state, "", coin) != nil {
						stillUnresolved = true
					}
				}
				if stillUnresolved {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s pending close kept: a confirmed fill still waits for settlement\n", j.stratID)
				} else {
					coins := map[string]bool{}
					for _, coin := range candidates {
						coins[coin] = true
					}
					closeRobinhoodBookVerifiedFlat(ss, *sc, deps.prices, coins, robinhoodVerifiedFlatCircuitBreaker, nil)
					ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
					if deps.store != nil {
						if err := deps.store.SaveStrategyBook(ss); err != nil {
							fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s pending close cleared in memory but the save failed: %v — it retries with the next save\n", j.stratID, err)
						}
					}
				}
			}
			mu.Unlock()
			continue
		}

		var failCount int
		var shouldAlert bool
		now := time.Now().UTC()
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
			} else if failedErr != nil {
				if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood); p != nil {
					p.ConsecutiveFailures++
					failCount = p.ConsecutiveFailures
					if shouldNotifyDrainFailure(p.ConsecutiveFailures, p.LastNotifiedAt, now) {
						p.LastNotifiedAt = now
						shouldAlert = true
					}
				}
			}
		}
		mu.Unlock()

		if shouldAlert && sendOwnerDM != nil {
			sendOwnerDM(formatDrainFailureAlert("robinhood", j.stratID, failedCoin, failedSize, failedErr.Error(), failCount))
		}
	}
}

func rhLiveStrategiesForCoin(coin string, rhLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range rhLiveAll {
		if robinhoodSymbol(sc.Args) == coin {
			out = append(out, sc)
		}
	}
	return out
}

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
