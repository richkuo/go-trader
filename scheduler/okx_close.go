package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

type OKXPosition struct {
	Coin          string
	Size          float64
	EntryPrice    float64
	Side          string
	UnrealizedPnL float64
}

var okxLiveCloseScript = "shared_scripts/close_okx_position.py"

var okxFetchPositionsScript = "shared_scripts/fetch_okx_positions.py"

type OKXLiveCloser func(symbol string, partialSz *float64) (*OKXCloseResult, error)

func defaultOKXLiveCloser(symbol string, partialSz *float64) (*OKXCloseResult, error) {
	result, stderr, err := RunOKXClose(okxLiveCloseScript, symbol, partialSz)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[okx-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

type OKXPositionsFetcher func() ([]OKXPosition, error)

func defaultOKXPositionsFetcher() ([]OKXPosition, error) {
	result, stderr, err := RunOKXFetchPositions(okxFetchPositionsScript)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[okx-close] fetch_positions stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, err
	}
	positions := make([]OKXPosition, 0, len(result.Positions))
	for _, p := range result.Positions {
		positions = append(positions, OKXPosition{
			Coin:          p.Coin,
			Size:          p.Size,
			EntryPrice:    p.EntryPrice,
			Side:          p.Side,
			UnrealizedPnL: p.UnrealizedPnL,
		})
	}
	return positions, nil
}

type OKXLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []OKXPosition
	Errors       map[string]error
}

func (r OKXLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

func (r OKXLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

func forceCloseOKXLive(ctx context.Context, positions []OKXPosition, okxLiveAll []StrategyConfig, closer OKXLiveCloser) OKXLiveCloseReport {
	report := OKXLiveCloseReport{Errors: make(map[string]error)}

	tradedCoins := make(map[string]bool)
	for _, sc := range okxLiveAll {
		if sc.Type != "perps" {
			continue
		}
		sym := okxSymbol(sc.Args)
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
		result, err := closer(p.Coin, nil)
		if err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}

func okxLiveStrategiesForCoin(coin string, okxLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range okxLiveAll {
		if sc.Platform != "okx" || sc.Type != "perps" {
			continue
		}
		if okxSymbol(sc.Args) == coin {
			out = append(out, sc)
		}
	}
	return out
}

func okxStrategyCapitalWeight(sc StrategyConfig) float64 {
	if sc.CapitalPct > 0 {
		return sc.CapitalPct
	}
	if sc.Capital > 0 {
		return sc.Capital
	}
	return 1.0
}

func okxStrategyCapitalWeights(peers []StrategyConfig) []float64 {
	hasPct := false
	hasAbs := false
	for _, p := range peers {
		switch {
		case p.CapitalPct > 0:
			hasPct = true
		case p.Capital > 0:
			hasAbs = true
		}
	}
	mixed := hasPct && hasAbs
	out := make([]float64, len(peers))
	for i, p := range peers {
		if mixed {
			out[i] = 1.0
			continue
		}
		out[i] = okxStrategyCapitalWeight(p)
	}
	return out
}

func computeOKXCircuitCloseQty(coin, strategyID string, okxPositions []OKXPosition, okxLiveAll []StrategyConfig) (qty float64, ok bool) {
	var onChain float64
	found := false
	for i := range okxPositions {
		if okxPositions[i].Coin == coin {
			onChain = okxPositions[i].Size
			found = true
			break
		}
	}
	if !found || onChain == 0 {
		return 0, false
	}
	absSzi := math.Abs(onChain)
	peers := okxLiveStrategiesForCoin(coin, okxLiveAll)
	if len(peers) <= 1 {
		return absSzi, true
	}
	weights := okxStrategyCapitalWeights(peers)
	sumW := 0.0
	var wFiring float64
	foundFiring := false
	for i, p := range peers {
		sumW += weights[i]
		if p.ID == strategyID {
			wFiring = weights[i]
			foundFiring = true
		}
	}
	if !foundFiring || sumW <= 0 {
		return absSzi, true
	}
	q := absSzi * (wFiring / sumW)
	if q > absSzi {
		q = absSzi
	}
	if q < 1e-12 {
		return 0, false
	}
	return q, true
}

func runPendingOKXCircuitCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	okxHasCreds bool,
	okxPositions []OKXPosition,
	okxStateFetched bool,
	okxFetcher OKXPositionsFetcher,
	closer OKXLiveCloser,
	totalBudget time.Duration,
	mu *sync.RWMutex,
	ownerDM func(string),
) {
	if !okxHasCreds || closer == nil || state == nil {
		return
	}

	var okxLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "okx" && sc.Type == "perps" && okxIsLive(sc.Args) {
			okxLiveAll = append(okxLiveAll, sc)
		}
	}

	mu.RLock()
	hasPending := false
	hasStuckCB := false
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
			hasPending = true
		}
	}
	for _, sc := range okxLiveAll {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) == nil && ss.RiskState.CircuitBreaker {
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

	positions := okxPositions
	if !okxStateFetched && okxFetcher != nil {
		pos, err := okxFetcher()
		if err != nil {
			fmt.Printf("[CRITICAL] okx-circuit-close: cannot fetch OKX positions: %v — will retry next cycle\n", err)
			return
		}
		positions = pos
	}

	if hasStuckCB {
		recoverOrder := make([]StrategyConfig, len(okxLiveAll))
		copy(recoverOrder, okxLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		mu.Lock()
		for _, sc := range recoverOrder {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
				continue
			}
			if !ss.RiskState.CircuitBreaker {
				continue
			}
			sym := okxSymbol(sc.Args)
			if sym == "" {
				continue
			}
			qty, ok := computeOKXCircuitCloseQty(sym, sc.ID, positions, okxLiveAll)
			if !ok || qty <= 0 {
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &PendingCircuitClose{
				Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}},
			})
			fmt.Printf("[CRITICAL] okx-circuit-close: recovered pending for strategy %s coin %s sz=%.6f (CB latched, OKX fetch had failed at fire time)\n",
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
		p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX)
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
			fmt.Printf("[CRITICAL] okx-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "okx" || sc.Type != "perps" || !okxIsLive(sc.Args) {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		var failedSym string
		var failedSz float64
		var failedErr error
		for _, c := range j.pending.Symbols {
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}
			sz := c.Size
			for _, p := range positions {
				if p.Coin != c.Symbol {
					continue
				}
				absOC := math.Abs(p.Size)
				if absOC <= 1e-15 {
					sz = 0
					break
				}
				if sz > absOC {
					sz = absOC
				}
				break
			}
			if sz <= 1e-15 {
				continue
			}
			partial := sz
			_, err := closer(c.Symbol, &partial)
			if err != nil {
				fmt.Printf("[CRITICAL] okx-circuit-close: strategy %s coin %s sz=%.6f failed: %v\n", j.stratID, c.Symbol, sz, err)
				allOK = false
				failedSym = c.Symbol
				failedSz = sz
				failedErr = err
				break
			}
			fmt.Printf("[INFO] okx-circuit-close: strategy %s coin %s submitted reduce-only close sz=%.6f\n", j.stratID, c.Symbol, sz)
		}

		var failCount int
		var shouldAlert bool
		now := time.Now().UTC()
		mu.Lock()
		if ss := state.Strategies[j.stratID]; ss != nil {
			if allOK {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
			} else if failedErr != nil {
				if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX); p != nil {
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
			ownerDM(formatDrainFailureAlert("okx", j.stratID, failedSym, failedSz, failedErr.Error(), failCount))
		}
	}
}
