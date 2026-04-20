package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// TopStepPosition represents a live TopStep futures position. Size is
// signed (positive = long, negative = short) and integer-valued (futures
// contracts have no fractional sizing). Mirrors HLPosition / OKXPosition /
// RobinhoodPosition so the kill-switch plan builder can treat all
// platforms symmetrically.
type TopStepPosition struct {
	Coin     string
	Size     int
	AvgPrice float64
	Side     string // "long" or "short"
}

// topstepLiveCloseScript is the path to the Python close helper. Exposed
// as a var so tests can substitute — same pattern as hyperliquidLiveCloseScript /
// okxLiveCloseScript / robinhoodLiveCloseScript.
var topstepLiveCloseScript = "shared_scripts/close_topstep_position.py"

// topstepFetchPositionsScript is the path to the Python position fetcher.
var topstepFetchPositionsScript = "shared_scripts/fetch_topstep_positions.py"

// TopStepLiveCloser submits a market-flatten for the full on-account
// contract count of a single TopStep futures symbol and returns the parsed
// result. Exposed as a function variable so tests can inject a fake
// without spawning Python. Production implementation is
// defaultTopStepLiveCloser, which shells out to close_topstep_position.py
// via RunTopStepClose.
type TopStepLiveCloser func(symbol string) (*TopStepCloseResult, error)

// defaultTopStepLiveCloser is the production close implementation. Matches
// the HL/OKX/Robinhood shape: stderr goes to os.Stderr (kill switch is a
// system-level event, not strategy-scoped) and any non-nil err means the
// close was NOT confirmed by the adapter so the kill switch must stay
// latched.
func defaultTopStepLiveCloser(symbol string) (*TopStepCloseResult, error) {
	result, stderr, err := RunTopStepClose(topstepLiveCloseScript, symbol)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[ts-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

// TopStepPositionsFetcher fetches every open TopStep futures position on
// the configured account. Exposed as a function type so tests can stub —
// mirrors HLStateFetcher / OKXPositionsFetcher / RobinhoodPositionsFetcher.
type TopStepPositionsFetcher func() ([]TopStepPosition, error)

// defaultTopStepPositionsFetcher wraps fetch_topstep_positions.py for
// production use. Python-side filters zero-size entries; forceCloseTopStepLive
// also has a size==0 defense-in-depth layer.
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

// TopStepLiveCloseReport summarizes a forceCloseTopStepLive run. Mirrors
// HyperliquidLiveCloseReport / OKXLiveCloseReport / RobinhoodLiveCloseReport
// so the kill-switch plan can treat all platforms symmetrically. Errors is
// the load-bearing correctness signal — ConfirmedFlat() returns true only
// when it's empty.
//
// Unconfigured lists live positions for symbols no configured live TopStep
// futures strategy trades. Computed here (rather than in the plan builder)
// so the "which symbols is this scheduler authorized to touch" partition
// has a single source of truth. The plan builder consumes Unconfigured
// separately from ConfirmedFlat to decide whether to latch the kill
// switch for manual intervention.
type TopStepLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []TopStepPosition
	Errors       map[string]error
}

// ConfirmedFlat reports whether every configured live TopStep futures
// symbol reached a terminal closed/flat state without errors. Mirrors
// HL/OKX/Robinhood shape.
func (r TopStepLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

// SortedErrorCoins returns Errors keys in deterministic order for stable
// log/Discord output. Go map iteration is randomized, so identical
// kill-switch fires would otherwise produce different messages (caught in
// #342 review for the HL report).
func (r TopStepLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

// tsLiveStrategiesForContract returns every configured live TopStep futures
// strategy that trades the given contract symbol. Used by the per-strategy
// circuit-breaker sizing code to decide whether the firing strategy is the
// sole peer (full flatten safe) or one of several peers sharing the contract
// (partial flatten required but not supported by market_close).
func tsLiveStrategiesForContract(symbol string, tsLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range tsLiveAll {
		if topstepSymbol(sc.Args) == symbol {
			out = append(out, sc)
		}
	}
	return out
}

// computeTopStepCircuitCloseQty returns the unsigned contract count to flatten
// when strategyID's per-strategy circuit breaker fires (#362). Futures are
// whole-contract only, so qty is an integer magnitude.
//
// Sole-peer (one live strategy configured for the contract): returns the full
// on-account absolute contract count so market_close flattens the position.
//
// Multi-peer (two+ live strategies sharing the contract): returns (0, false).
// TopStepX's market_close has no partial-size variant; issuing a full flatten
// on behalf of one strategy would close the peer's share too. A proportional
// partial close would require a regular market order in the opposite
// direction, which is a different code path and outside phase 4's scope. The
// virtual force-close in CheckRisk still runs, and the operator is warned to
// intervene manually for the shared contract.
//
// ok is false when no non-zero on-account position exists for the symbol.
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
	// Multi-peer: skip. Logged by the caller when the skip is visible (#362
	// review). Enqueuing nothing means virtual force-close still mutates local
	// state and the kill switch for the firing strategy is latched, but the
	// live contracts are left for operator review.
	fmt.Printf("[WARN] ts-circuit-close: strategy %s shares contract %s with %d peers; skipping enqueue (market_close has no partial-size variant — manual intervention required)\n",
		strategyID, symbol, len(peers))
	return 0, false
}

// runPendingTopStepCircuitCloses drains the topstep entry of
// RiskState.PendingCircuitCloses for every strategy, submitting market-flatten
// orders outside the state mutex. Retries next scheduler cycle on failure,
// which handles CME outside-RTH rejections naturally: the close errors, the
// pending stays queued, and the drain re-attempts on the next tick (#362).
//
// Also recovers "stuck CB" strategies: if a per-strategy circuit breaker fired
// on a cycle where the TopStep positions fetch failed,
// setTopStepCircuitBreakerPending bails on the nil assist and the pending
// close is never set. Subsequent CheckRisk calls early-return with "circuit
// breaker active" without re-enqueuing. This drain detects the case (live TS
// futures strategy with CircuitBreaker=true but no pending TS entry AND a
// matching non-zero on-account position) and reconstructs the pending so the
// market_close eventually fires once TS is reachable again.
//
// Mirrors runPendingHyperliquidCircuitCloses (#356) exactly; differences are:
//   - closer signature has no partial-size argument (TopStep has none)
//   - stuck-CB recovery only reconstructs for sole-peer contracts (the
//     computeTopStepCircuitCloseQty guard)
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
		for _, c := range j.pending.Symbols {
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}
			// Defense-in-depth: skip legs whose on-account position already
			// went flat between enqueue and drain (e.g. operator manual close,
			// eventual consistency after a prior successful submit). Without
			// this guard, calling market_close on a flat position would error
			// and keep the pending latched forever.
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
				// Outside-RTH rejections, transient TopStepX API errors, and
				// any other close failure land here — we log and latch. The
				// next cycle re-enters this drain and retries; virtual state
				// stays untouched (CheckRisk already force-closed locally).
				fmt.Printf("[CRITICAL] ts-circuit-close: strategy %s contract %s sz=%d failed: %v (will retry next cycle)\n",
					j.stratID, c.Symbol, absOC, err)
				allOK = false
				break
			}
			fmt.Printf("[INFO] ts-circuit-close: strategy %s contract %s submitted market_close sz=%d\n",
				j.stratID, c.Symbol, absOC)
		}

		if allOK {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseTopStep)
			}
			mu.Unlock()
		}
	}
}

// forceCloseTopStepLive submits market-flatten orders for every non-zero
// live TopStep futures position belonging to a symbol a configured live
// TopStep strategy trades on this account. Mirrors forceCloseHyperliquidLive /
// forceCloseOKXLive. TopStep's market_close endpoint flattens the full
// contract count for the symbol — futures are whole-contract only, so no
// client-side rounding is required.
//
// Pure / no state mutation. Caller mutates virtual state only when
// report.ConfirmedFlat() is true.
//
// The ctx argument bounds the OVERALL close loop. Each individual closer
// call has its own subprocess timeout (see RunPythonScript). Once ctx
// expires, remaining unprocessed symbols are added to Errors so the kill
// switch stays latched and retries next cycle.
//
// CME trading-hour restriction: fires outside RTH may fail with a venue
// error (market closed). The latch-until-flat semantic handles this
// naturally — the kill switch stays latched, logs the error, and the
// next cycle retries. The operator sees the restriction in the Discord
// message so there is no false-reassurance window.
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
			// Unowned position — kill switch only acts on symbols this
			// scheduler is configured to trade. Non-zero sizes are surfaced
			// to the caller via Unconfigured so the plan can latch the
			// switch and prompt manual intervention.
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
		// No adapter-side already_flat branch (unlike HL/OKX/RH in #350):
		// TopStepX's market_close endpoint rejects close-on-flat with an
		// error rather than returning a no-op envelope, so any close after
		// an eventual-consistency flat surfaces here as report.Errors and
		// keeps the kill switch latched until the next cycle re-fetches.
		if _, err := closer(p.Coin); err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}
