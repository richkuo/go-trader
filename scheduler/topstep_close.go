package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type TopStepPosition struct {
	Coin     string
	Size     int
	AvgPrice float64
	Side     string
}

var topstepLiveCloseScript = "shared_scripts/close_topstep_position.py"

var topstepFetchPositionsScript = "shared_scripts/fetch_topstep_positions.py"

type TopStepLiveCloser func(symbol string) (*TopStepCloseResult, error)

func defaultTopStepLiveCloser(symbol string) (*TopStepCloseResult, error) {
	result, stderr, err := RunTopStepClose(topstepLiveCloseScript, symbol)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[ts-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

type TopStepPositionsFetcher func() ([]TopStepPosition, error)

func defaultTopStepPositionsFetcher() ([]TopStepPosition, error) {
	result, stderr, err := RunTopStepFetchPositions(topstepFetchPositionsScript)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[ts-close] fetch_positions stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, err
	}
	positions := make([]TopStepPosition, 0, len(result.Positions))
	for _, p := range result.Positions {
		positions = append(positions, TopStepPosition{
			Coin:     p.Coin,
			Size:     p.Size,
			AvgPrice: p.AvgPrice,
			Side:     p.Side,
		})
	}
	return positions, nil
}

type TopStepLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []TopStepPosition
	Errors       map[string]error
}

func (r TopStepLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

func (r TopStepLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

func tsLiveStrategiesForContract(symbol string, tsLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range tsLiveAll {
		if topstepSymbol(sc.Args) == symbol {
			out = append(out, sc)
		}
	}
	return out
}

func computeTopStepCircuitCloseQty(symbol, strategyID string, tsPositions []TopStepPosition, tsLiveAll []StrategyConfig) (qty int, ok bool) {
	var onAccount int
	found := false
	for _, p := range tsPositions {
		if p.Coin == symbol {
			onAccount = p.Size
			found = true
			break
		}
	}
	if !found || onAccount == 0 {
		return 0, false
	}
	abs := onAccount
	if abs < 0 {
		abs = -abs
	}
	peers := tsLiveStrategiesForContract(symbol, tsLiveAll)
	if len(peers) <= 1 {
		return abs, true
	}
	fmt.Printf("[WARN] ts-circuit-close: strategy %s shares contract %s with %d peers; skipping enqueue (market_close has no partial-size variant — manual intervention required)\n",
		strategyID, symbol, len(peers))
	return 0, false
}

func runPendingTopStepCircuitCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	tsPositions []TopStepPosition,
	tsStateFetched bool,
	tsFetcher TopStepPositionsFetcher,
	closer TopStepLiveCloser,
	totalBudget time.Duration,
	mu *sync.RWMutex,
	ownerDM func(string),
) {
	if closer == nil || state == nil {
		return
	}

	var tsLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "topstep" && sc.Type == "futures" && topstepIsLive(sc.Args) {
			tsLiveAll = append(tsLiveAll, sc)
		}
	}
	if len(tsLiveAll) == 0 {
		return
	}

	mu.RLock()
	hasPending := false
	hasStuckCB := false
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
			hasPending = true
		}
	}
	for _, sc := range tsLiveAll {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) == nil && ss.RiskState.CircuitBreaker {
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

	positions := tsPositions
	if !tsStateFetched && tsFetcher != nil {
		pos, err := tsFetcher()
		if err != nil {
			fmt.Printf("[CRITICAL] ts-circuit-close: cannot fetch TopStep positions: %v — will retry next cycle\n", err)
			return
		}
		positions = pos
	}

	if hasStuckCB {
		recoverOrder := make([]StrategyConfig, len(tsLiveAll))
		copy(recoverOrder, tsLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		mu.Lock()
		for _, sc := range recoverOrder {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
				continue
			}
			if !ss.RiskState.CircuitBreaker {
				continue
			}
			sym := topstepSymbol(sc.Args)
			if sym == "" {
				continue
			}
			qty, ok := computeTopStepCircuitCloseQty(sym, sc.ID, positions, tsLiveAll)
			if !ok || qty <= 0 {
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseTopStep, &PendingCircuitClose{
				Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: float64(qty)}},
			})
			fmt.Printf("[CRITICAL] ts-circuit-close: recovered pending for strategy %s contract %s sz=%d (CB latched, TS fetch had failed at fire time)\n",
				sc.ID, sym, qty)
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
		p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
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
			fmt.Printf("[CRITICAL] ts-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "topstep" || sc.Type != "futures" || !topstepIsLive(sc.Args) {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseTopStep)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		var failedSym string
		var failedAbsOC int
		var failedErr error
		for _, c := range j.pending.Symbols {
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}
			var absOC int
			stillOpen := false
			for _, p := range positions {
				if p.Coin == c.Symbol {
					absOC = p.Size
					if absOC < 0 {
						absOC = -absOC
					}
					if absOC > 0 {
						stillOpen = true
					}
					break
				}
			}
			if !stillOpen {
				fmt.Printf("[INFO] ts-circuit-close: strategy %s contract %s already flat on-account; clearing pending without submit\n",
					j.stratID, c.Symbol)
				continue
			}
			if _, err := closer(c.Symbol); err != nil {
				fmt.Printf("[CRITICAL] ts-circuit-close: strategy %s contract %s sz=%d failed: %v (will retry next cycle)\n",
					j.stratID, c.Symbol, absOC, err)
				allOK = false
				failedSym = c.Symbol
				failedAbsOC = absOC
				failedErr = err
				break
			}
			fmt.Printf("[INFO] ts-circuit-close: strategy %s contract %s submitted market_close sz=%d\n",
				j.stratID, c.Symbol, absOC)
		}

		var failCount int
		var shouldAlert bool
		now := time.Now().UTC()
		mu.Lock()
		if ss := state.Strategies[j.stratID]; ss != nil {
			if allOK {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseTopStep)
			} else if failedErr != nil {
				if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep); p != nil {
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

		if shouldAlert && ownerDM != nil && failedErr != nil {
			ownerDM(formatDrainFailureAlert("topstep", j.stratID, failedSym, float64(failedAbsOC), failedErr.Error(), failCount))
		}
	}
}

func forceCloseTopStepLive(ctx context.Context, positions []TopStepPosition, tsLiveAll []StrategyConfig, closer TopStepLiveCloser) TopStepLiveCloseReport {
	report := TopStepLiveCloseReport{Errors: make(map[string]error)}

	tradedCoins := make(map[string]bool)
	for _, sc := range tsLiveAll {
		if sc.Type != "futures" {
			continue
		}
		sym := topstepSymbol(sc.Args)
		if sym != "" {
			tradedCoins[sym] = true
		}
	}

	for _, p := range positions {
		if !tradedCoins[p.Coin] {
			if p.Size != 0 {
				report.Unconfigured = append(report.Unconfigured, p)
			}
			continue
		}
		if p.Size == 0 {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		if err := ctx.Err(); err != nil {
			report.Errors[p.Coin] = fmt.Errorf("close budget exhausted before submit: %w", err)
			continue
		}
		if _, err := closer(p.Coin); err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}
