package main

import (
	"context"
	"fmt"
	"os"
	"sort"
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
type OKXLiveCloser func(symbol string) (*OKXCloseResult, error)

// defaultOKXLiveCloser is the production close implementation. Matches the
// HL shape: stderr goes to os.Stderr (kill switch is a system-level event,
// not strategy-scoped) and any non-nil err means the close was NOT confirmed
// by the adapter so the kill switch must stay latched.
func defaultOKXLiveCloser(symbol string) (*OKXCloseResult, error) {
	result, stderr, err := RunOKXClose(okxLiveCloseScript, symbol)
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
		if _, err := closer(p.Coin); err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}
