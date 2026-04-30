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

// OKXPosition represents an on-chain OKX perpetual swap position. Size is
// signed (positive = long, negative = short) to mirror HLPosition so the
// kill-switch plan builder can treat both platforms symmetrically.
type OKXPosition struct {
	Coin       string
	Size       float64
	EntryPrice float64
	Side       string // "long" or "short"; empty when szi==0 (filtered)
}

// okxLiveCloseScript is the path to the Python close helper. Exposed as a
// var so tests can substitute — same pattern as hyperliquidLiveCloseScript.
var okxLiveCloseScript = "shared_scripts/close_okx_position.py"

// okxFetchPositionsScript is the path to the Python position fetcher.
var okxFetchPositionsScript = "shared_scripts/fetch_okx_positions.py"

// OKXLiveCloser submits a reduce-only market close for a single OKX swap
// coin and returns the parsed result. Exposed as a function variable so
// tests can inject a fake without spawning Python. Production implementation
// is defaultOKXLiveCloser, which shells out to close_okx_position.py via
// RunOKXClose.
// When partialSz is nil, the full on-chain position is closed (portfolio
// kill switch and sole-owner circuit breakers). When non-nil, submits a
// reduce-only partial close for that coin quantity — used by per-strategy
// circuit breakers on shared OKX wallets (#360 / phase 2 of #357).
type OKXLiveCloser func(symbol string, partialSz *float64) (*OKXCloseResult, error)

// defaultOKXLiveCloser is the production close implementation. Matches the
// HL shape: stderr goes to os.Stderr (kill switch is a system-level event,
// not strategy-scoped) and any non-nil err means the close was NOT confirmed
// by the adapter so the kill switch must stay latched.
func defaultOKXLiveCloser(symbol string, partialSz *float64) (*OKXCloseResult, error) {
	result, stderr, err := RunOKXClose(okxLiveCloseScript, symbol, partialSz)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[okx-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

// OKXPositionsFetcher fetches every open OKX swap position on the account.
// Exposed as a function type so tests can stub — mirrors HLStateFetcher.
type OKXPositionsFetcher func() ([]OKXPosition, error)

// defaultOKXPositionsFetcher wraps fetch_okx_positions.py for production
// use. Returns a typed slice so callers don't have to decode the JSON
// envelope themselves. Python-side filters stale zero-size entries
// upstream (fetch_okx_positions.py), and forceCloseOKXLive has its own
// size==0 defense-in-depth — no need for a third filtering layer here.
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
			Coin:       p.Coin,
			Size:       p.Size,
			EntryPrice: p.EntryPrice,
			Side:       p.Side,
		})
	}
	return positions, nil
}

// OKXLiveCloseReport summarizes a forceCloseOKXLive run. Mirrors
// HyperliquidLiveCloseReport so the kill-switch plan can treat both
// platforms symmetrically. Errors is the load-bearing correctness signal —
// ConfirmedFlat() returns true only when it's empty. See
// HyperliquidLiveCloseReport doc for the rationale.
//
// Unconfigured lists on-chain positions for coins no configured live perps
// strategy trades. Computed here (rather than in the plan builder) so the
// "which coins is this scheduler authorized to touch" partition has a
// single source of truth — if the perps-vs-spot partition rule ever
// changes, only this function needs to be updated. The plan builder
// consumes Unconfigured separately from ConfirmedFlat to decide whether
// to latch the kill switch for manual intervention.
type OKXLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []OKXPosition
	Errors       map[string]error
}

// ConfirmedFlat reports whether every configured live OKX coin reached a
// terminal closed/flat state without errors. Mirrors HL shape
// (Errors-only); Unconfigured is a separate signal the plan builder
// consumes to decide whether to latch the switch for manual intervention.
func (r OKXLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

// SortedErrorCoins returns Errors keys in deterministic order for stable
// log/Discord output. Same reason as HyperliquidLiveCloseReport — map
// iteration is randomized in Go, so identical kill-switch fires would
// otherwise produce different messages (caught in #342 review).
func (r OKXLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

// forceCloseOKXLive submits reduce-only market closes for every non-zero
// on-chain OKX swap position belonging to a coin a configured live OKX
// perps strategy trades on this account. Mirrors forceCloseHyperliquidLive:
// closes the on-chain quantity directly, regardless of which strategy
// "owns" it — required because account-level positions can diverge from
// per-strategy virtual state (#341/#345). The OKX adapter's market_close
// passes reduceOnly=True, so overshooting cannot flip the position.
//
// Pure / no state mutation. Caller mutates virtual state only when
// report.ConfirmedFlat() is true.
//
// The ctx argument bounds the OVERALL close loop. Each individual closer
// call has its own subprocess timeout (see RunPythonScript). Once ctx
// expires, remaining unprocessed coins are added to Errors so the kill
// switch stays latched and retries next cycle.
//
// Note: only perps strategies are considered. OKX spot strategies (sc.Type
// == "spot") are deliberately NOT closed here — there is no reduce-only
// spot close, and a blind sell-all would trash unrelated holdings. The
// plan surfaces spot strategies via SpotUnclosable so the operator sees
// the gap in the Discord message.
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
			// Unowned position — kill switch only acts on coins this
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
		result, err := closer(p.Coin, nil)
		if err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		// Adapter may report already_flat when its own pre-submit position
		// check finds nothing to close (eventual-consistency window). Route
		// through AlreadyFlat so operator messaging distinguishes "we sent a
		// close order" from "nothing to close" (#350).
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}

// okxLiveStrategiesForCoin returns every live OKX perps strategy configured to
// trade the given coin. Mirrors hlLiveStrategiesForCoin.
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

// okxStrategyCapitalWeight returns a single strategy's proportional weight for
// OKX shared-coin close sizing.
func okxStrategyCapitalWeight(sc StrategyConfig) float64 {
	if sc.CapitalPct > 0 {
		return sc.CapitalPct
	}
	if sc.Capital > 0 {
		return sc.Capital
	}
	return 1.0
}

// okxStrategyCapitalWeights returns per-peer weights for proportional close
// sizing on a shared coin. When peers declare CapitalPct (fractional) alongside
// raw Capital (dollars), their sum is nonsensical and the CapitalPct-only
// peer's share collapses to near-zero, producing a no-op close. Detect the
// mismatch and fall back to equal weights so the firing strategy still gets a
// meaningful share.
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

// computeOKXCircuitCloseQty returns the unsigned contract quantity for a
// reduce-only market_close when strategyID's per-strategy circuit breaker
// fires on OKX perps. For a coin traded by multiple live OKX strategies on the
// same wallet, the close size is proportional to capital_pct (or capital)
// weights. For a sole configured trader of that coin, the full on-chain
// absolute size is used. ok is false when there is no non-zero on-chain
// position for the coin.
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

// runPendingOKXCircuitCloses drains the "okx" entry of
// RiskState.PendingCircuitCloses for every strategy, submitting reduce-only
// OKX swap closes outside the state mutex. Retries next scheduler cycle on
// failure. Mirrors runPendingHyperliquidCircuitCloses (#360).
//
// Also recovers "stuck CB" strategies: if a per-strategy circuit breaker fires
// on a cycle where the OKX position fetch failed, setOKXCircuitBreakerPending
// bails on the nil assist and the pending close is never set. Subsequent
// CheckRisk calls early-return with "circuit breaker active" without
// re-enqueueing. This drain detects the case (live OKX perps strategy with
// CircuitBreaker=true but no pending OKX entry AND a matching non-zero
// on-chain position) and reconstructs the pending so the reduce-only close
// eventually fires once OKX is reachable again.
//
// okxHasCreds is used to decide whether fetching is worth attempting on a
// cycle where the main loop did not already pre-fetch; it is the same gate as
// the env-var check in defaultOKXPositionsFetcher. When false, this function
// is a no-op.
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

	// Build the live OKX perps roster from strategies — needed for both the
	// stuck-CB recovery path and the shared-coin weight computation.
	var okxLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "okx" && sc.Type == "perps" && okxIsLive(sc.Args) {
			okxLiveAll = append(okxLiveAll, sc)
		}
	}

	// Phase 1: snapshot — detect pending jobs AND stuck-CB strategies.
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

	// Phase 2: reconstruct pending for stuck-CB strategies.
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
				// Only count as a drain failure when a closer() actually errored.
				// allOK can also be set false by a mid-loop ctxOverall expiry
				// where failedErr stays nil — counting that would inflate the
				// counter and dereferencing failedErr.Error() below would panic.
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
