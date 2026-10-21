package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

type RobinhoodPendingCloseOwnerDM func(message string)

type robinhoodCircuitCloseDeps struct {
	store   *StateStore
	prices  map[string]float64
	resolve func() map[string]error
	settle  func() robinhoodSettleOutcome
}

func (d robinhoodCircuitCloseDeps) settleNow(state *AppState, strategies []StrategyConfig, mu *sync.RWMutex) robinhoodSettleOutcome {
	if d.settle != nil {
		return d.settle()
	}
	if d.store != nil {
		_, out := settleRobinhoodAttemptsOutcome(robinhoodEmergencyDeps{store: d.store, state: state, mu: mu, cfgs: strategies}, nil, nil)
		return out
	}
	return robinhoodSettleOutcome{SaveErr: fmt.Errorf("no durable settlement is wired, so the remaining book cannot be proven")}
}

func robinhoodOwnLongBook(ss *StrategyState, coin string) float64 {
	if ss == nil {
		return 0
	}
	pos := ss.Positions[coin]
	if pos == nil || pos.Side != "long" || !(pos.Quantity > 0) || !robinhoodFinite(pos.Quantity) {
		return 0
	}
	return pos.Quantity
}

func formatRobinhoodCBResidueDM(strategyID, coin string, held float64) string {
	return fmt.Sprintf("Robinhood CB close [%s]: the go-trader book for %s is closed, and the account still holds %.10g %s above every go-trader book. go-trader never sells coin outside its book; that coin is the operator's.", strategyID, coin, held, coin)
}

func formatRobinhoodLegacyPendingDM(strategyID, coin string, legacySize, held float64) string {
	return fmt.Sprintf("Robinhood CB close [%s]: a pending close of %.10g %s from an earlier version was sized from the account, and the go-trader book for %s is flat. No order was sent and the pending close is cleared. The account holds %.10g %s; reconcile it by hand.", strategyID, legacySize, coin, coin, held, coin)
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

	ownerDM := func(msg string) {
		fmt.Printf("[CRITICAL] rh-circuit-close: %s\n", msg)
		if sendOwnerDM != nil {
			sendOwnerDM(msg)
		}
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
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) == nil && ss.RiskState.CircuitBreaker && !robinhoodBookFlat(robinhoodOwnLongBook(ss, robinhoodSymbol(sc.Args))) {
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
		var dms []string
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
			book := robinhoodOwnLongBook(ss, coin)
			if robinhoodBookFlat(book) {
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
				dms = append(dms, formatRobinhoodSharedOwnerDM(sc.ID, coin, peers))
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{
				Symbols:   []PendingCircuitCloseSymbol{{Symbol: coin, Size: book}},
				SizeBasis: robinhoodPendingSizeBasisBook,
			})
			fmt.Printf("[CRITICAL] rh-circuit-close: recovered pending for strategy %s coin %s size=%.10g from the go-trader book (CB latched with no pending close; the account holds %.10g) (#1825)\n",
				sc.ID, coin, book, qty)
		}
		if deps.store != nil {
			for _, ss := range flatCleaned {
				if err := deps.store.SaveStrategyBook(ss); err != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: saving the verified-flat close for %s failed: %v — it retries with the next save\n", ss.ID, err)
				}
			}
		}
		mu.Unlock()
		for _, msg := range dms {
			ownerDM(msg)
		}
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
		cp := *p
		cp.Symbols = append([]PendingCircuitCloseSymbol(nil), p.Symbols...)
		jobs = append(jobs, job{id, cp})
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

		legacy := j.pending.SizeBasis != robinhoodPendingSizeBasisBook
		allOK := true
		var failedCoin string
		var failedSize float64
		var failedErr error
		var dms []string
		type candidate struct {
			coin string
			res  *RobinhoodCloseResult
		}
		var candidates []candidate
		closerCalled := false
		for _, c := range j.pending.Symbols {
			if held, ok := unresolved[c.Symbol]; ok {
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s waits for an unresolved order: %v\n", j.stratID, c.Symbol, held)
				allOK = false
				continue
			}
			onAccount := robinhoodOnAccountSize(c.Symbol, positions)
			mu.RLock()
			book := robinhoodOwnLongBook(state.Strategies[j.stratID], c.Symbol)
			mu.RUnlock()
			target := c.Size
			if legacy {
				if robinhoodBookFlat(book) {
					dms = append(dms, formatRobinhoodLegacyPendingDM(j.stratID, c.Symbol, c.Size, onAccount))
					continue
				}
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood); p != nil {
						p.SizeBasis = robinhoodPendingSizeBasisBook
						for i := range p.Symbols {
							if p.Symbols[i].Symbol == c.Symbol {
								p.Symbols[i].Size = book
							}
						}
					}
					if deps.store != nil {
						if err := deps.store.SaveStrategyBook(ss); err != nil {
							fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s: saving the book-sized pending close failed: %v — the book still caps the sale\n", j.stratID, err)
						}
					}
				}
				mu.Unlock()
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: the earlier account-sized pending close of %.10g now targets the go-trader book %.10g (#1825)\n", j.stratID, c.Symbol, c.Size, book)
				target = book
			}
			if robinhoodBookFlat(book) {
				if onAccount > 0 {
					dms = append(dms, formatRobinhoodCBResidueDM(j.stratID, c.Symbol, onAccount))
				}
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s has no go-trader book left to sell; no order sent\n", j.stratID, c.Symbol)
				continue
			}
			if onAccount <= 0 {
				if lag := robinhoodFlatReadLags(c.Symbol, onAccount, freshLongs); lag != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: %v — pending close kept\n", j.stratID, c.Symbol, lag)
					allOK = false
					continue
				}
				candidates = append(candidates, candidate{coin: c.Symbol})
				continue
			}

			peers := rhLiveStrategiesForCoin(c.Symbol, rhLiveAll)
			if len(peers) > 1 {
				dms = append(dms, formatRobinhoodSharedOwnerDM(j.stratID, c.Symbol, peers))
				allOK = false
				continue
			}

			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}

			ask := math.Min(target, book)
			closerCalled = true
			result, err := closer(RobinhoodCloseRequest{Symbol: c.Symbol, Quantity: ask})
			if err != nil {
				fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s asking %.10g failed: %v\n", j.stratID, c.Symbol, ask, err)
				allOK = false
				failedCoin = c.Symbol
				failedSize = ask
				failedErr = err
				break
			}
			switch {
			case result != nil && result.NoBook:
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s has no go-trader book left to sell; no order sent\n", j.stratID, c.Symbol)
				if onAccount > 0 {
					dms = append(dms, formatRobinhoodCBResidueDM(j.stratID, c.Symbol, onAccount))
				}
				continue
			case result != nil && result.Close != nil && result.Close.AlreadyFlat:
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s already flat on-account (no-op)\n", j.stratID, c.Symbol)
			default:
				sent := 0.0
				if result != nil {
					sent = result.RequestedQuantity
				}
				fmt.Printf("[INFO] rh-circuit-close: strategy %s coin %s confirmed venue sell: asked %.10g, sent %.10g, account held %.10g\n",
					j.stratID, c.Symbol, ask, sent, onAccount)
			}
			candidates = append(candidates, candidate{coin: c.Symbol, res: result})
		}

		var verifiedFlat map[string]bool
		remainders := map[string]float64{}
		verify := allOK && len(candidates) > 0
		var post []RobinhoodPosition
		var verifyErr error
		if verify {
			if fetcher == nil {
				verifyErr = fmt.Errorf("no strict holdings fetcher is wired")
			} else {
				post, verifyErr = fetcher()
			}
		}
		var outcome robinhoodSettleOutcome
		if closerCalled || len(candidates) > 0 {
			outcome = deps.settleNow(state, strategies, mu)
		}
		if verify {
			verifiedFlat = map[string]bool{}
			mu.RLock()
			ss := state.Strategies[j.stratID]
			for _, cand := range candidates {
				coin := cand.coin
				remaining := robinhoodOwnLongBook(ss, coin)
				q := robinhoodOnAccountSize(coin, post)
				var reason error
				switch {
				case verifyErr != nil:
					reason = fmt.Errorf("post-close holdings read failed: %v", verifyErr)
				case outcome.SaveErr != nil:
					reason = fmt.Errorf("settlement is not saved: %v", outcome.SaveErr)
				case outcome.Unresolved[coin] != nil:
					reason = fmt.Errorf("an order for this coin is still unresolved after settlement: %v", outcome.Unresolved[coin])
				case robinhoodBookFlat(remaining):
					if q > 0 {
						dms = append(dms, formatRobinhoodCBResidueDM(j.stratID, coin, q))
					}
				case q <= 0:
					if lag := robinhoodFlatReadLags(coin, q, freshLongs); lag != nil {
						reason = lag
					} else {
						verifiedFlat[coin] = true
					}
				default:
					reason = fmt.Errorf("%s", robinhoodUnsoldReason(coin, remaining, q, cand.res))
				}
				if reason != nil {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s coin %s: %v — pending close kept\n", j.stratID, coin, reason)
					allOK = false
					failedCoin, failedSize, failedErr = coin, remaining, reason
					remainders[coin] = remaining
				}
			}
			mu.RUnlock()
		}

		if allOK {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				if len(verifiedFlat) > 0 {
					fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s: the account is flat on a fresh read with no unresolved order; closing the unsold book for %v as verified flat\n", j.stratID, sortedBoolKeys(verifiedFlat))
					closeRobinhoodBookVerifiedFlat(ss, *sc, deps.prices, verifiedFlat, robinhoodVerifiedFlatCircuitBreaker, nil)
				}
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
				if deps.store != nil {
					if err := deps.store.SaveStrategyBook(ss); err != nil {
						fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s pending close cleared in memory but the save failed: %v — it retries with the next save\n", j.stratID, err)
					}
				}
			}
			mu.Unlock()
			for _, msg := range dms {
				ownerDM(msg)
			}
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
			} else if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood); p != nil {
				for i := range p.Symbols {
					if r, ok := remainders[p.Symbols[i].Symbol]; ok && !robinhoodBookFlat(r) {
						p.Symbols[i].Size = r
					}
				}
				if failedErr != nil {
					p.ConsecutiveFailures++
					failCount = p.ConsecutiveFailures
					if shouldNotifyDrainFailure(p.ConsecutiveFailures, p.LastNotifiedAt, now) {
						p.LastNotifiedAt = now
						shouldAlert = true
					}
				}
				if len(remainders) > 0 && deps.store != nil {
					if err := deps.store.SaveStrategyBook(ss); err != nil {
						fmt.Printf("[CRITICAL] rh-circuit-close: strategy %s: saving the pending close remainder failed: %v — the book still caps the next sale\n", j.stratID, err)
					}
				}
			}
		}
		mu.Unlock()

		for _, msg := range dms {
			ownerDM(msg)
		}
		if shouldAlert && sendOwnerDM != nil {
			sendOwnerDM(formatDrainFailureAlert("robinhood", j.stratID, failedCoin, failedSize, failedErr.Error(), failCount))
		}
	}
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
