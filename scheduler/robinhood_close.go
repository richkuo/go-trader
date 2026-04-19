package main

import (
	"context"
	"fmt"
	"os"
	"sort"
)

// RobinhoodPosition represents a live Robinhood crypto position. Size is
// unsigned — Robinhood crypto is spot, so no short exposure exists.
// Mirrors HLPosition / OKXPosition so the kill-switch plan builder can
// treat all platforms symmetrically.
type RobinhoodPosition struct {
	Coin     string
	Size     float64
	AvgPrice float64
}

// robinhoodLiveCloseScript is the path to the Python close helper. Exposed as
// a var so tests can substitute — same pattern as hyperliquidLiveCloseScript /
// okxLiveCloseScript.
var robinhoodLiveCloseScript = "shared_scripts/close_robinhood_position.py"

// robinhoodFetchPositionsScript is the path to the Python position fetcher.
var robinhoodFetchPositionsScript = "shared_scripts/fetch_robinhood_positions.py"

// RobinhoodLiveCloser submits a market sell for the full on-account quantity
// of a single Robinhood crypto coin and returns the parsed result. Exposed
// as a function variable so tests can inject a fake without spawning Python.
// Production implementation is defaultRobinhoodLiveCloser, which shells out
// to close_robinhood_position.py via RunRobinhoodClose.
type RobinhoodLiveCloser func(symbol string) (*RobinhoodCloseResult, error)

// defaultRobinhoodLiveCloser is the production close implementation. Matches
// the HL/OKX shape: stderr goes to os.Stderr (kill switch is a system-level
// event, not strategy-scoped) and any non-nil err means the close was NOT
// confirmed by the adapter so the kill switch must stay latched.
func defaultRobinhoodLiveCloser(symbol string) (*RobinhoodCloseResult, error) {
	result, stderr, err := RunRobinhoodClose(robinhoodLiveCloseScript, symbol)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[rh-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

// RobinhoodPositionsFetcher fetches every open Robinhood crypto position on
// the account. Exposed as a function type so tests can stub — mirrors
// HLStateFetcher / OKXPositionsFetcher.
type RobinhoodPositionsFetcher func() ([]RobinhoodPosition, error)

// defaultRobinhoodPositionsFetcher wraps fetch_robinhood_positions.py for
// production use. Python-side filters stale zero-size entries; forceCloseRobinhoodLive
// also has a size==0 defense-in-depth layer.
func defaultRobinhoodPositionsFetcher() ([]RobinhoodPosition, error) {
	result, stderr, err := RunRobinhoodFetchPositions(robinhoodFetchPositionsScript)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[rh-close] fetch_positions stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, err
	}
	positions := make([]RobinhoodPosition, 0, len(result.Positions))
	for _, p := range result.Positions {
		positions = append(positions, RobinhoodPosition{
			Coin:     p.Coin,
			Size:     p.Size,
			AvgPrice: p.AvgPrice,
		})
	}
	return positions, nil
}

// RobinhoodLiveCloseReport summarizes a forceCloseRobinhoodLive run. Mirrors
// HyperliquidLiveCloseReport / OKXLiveCloseReport so the kill-switch plan
// can treat all platforms symmetrically. Errors is the load-bearing
// correctness signal — ConfirmedFlat() returns true only when it's empty.
//
// Unconfigured lists on-chain positions for coins no configured live
// Robinhood crypto strategy trades. Computed here (rather than in the plan
// builder) so the "which coins is this scheduler authorized to touch"
// partition has a single source of truth. The plan builder consumes
// Unconfigured separately from ConfirmedFlat to decide whether to latch
// the kill switch for manual intervention.
type RobinhoodLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []RobinhoodPosition
	Errors       map[string]error
}

// ConfirmedFlat reports whether every configured live Robinhood crypto coin
// reached a terminal closed/flat state without errors. Mirrors HL/OKX shape.
func (r RobinhoodLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

// SortedErrorCoins returns Errors keys in deterministic order for stable
// log/Discord output. Go map iteration is randomized, so identical
// kill-switch fires would otherwise produce different messages (caught in
// #342 review for the HL report).
func (r RobinhoodLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

// forceCloseRobinhoodLive submits market closes for every non-zero live
// Robinhood crypto position belonging to a coin a configured live Robinhood
// crypto strategy trades on this account. Mirrors forceCloseHyperliquidLive /
// forceCloseOKXLive. Robinhood crypto is spot with no reduce-only primitive,
// so this sells the full on-account balance for each configured coin —
// acceptable because kill switch only acts on coins the scheduler is
// authorized to trade (the operator's configuration opts in).
//
// Pure / no state mutation. Caller mutates virtual state only when
// report.ConfirmedFlat() is true.
//
// The ctx argument bounds the OVERALL close loop. Each individual closer
// call has its own subprocess timeout (see RunPythonScript). Once ctx
// expires, remaining unprocessed coins are added to Errors so the kill
// switch stays latched and retries next cycle.
//
// Only crypto (Type == "spot") strategies are considered. Robinhood stock
// options (Type == "options") are NOT closed here — their close semantics
// (sell-to-close vs buy-to-close per leg) and multi-leg instrument IDs
// require separate handling. The plan surfaces options strategies as a
// known gap in the Discord message (#346 follow-up).
func forceCloseRobinhoodLive(ctx context.Context, positions []RobinhoodPosition, rhLiveCrypto []StrategyConfig, closer RobinhoodLiveCloser) RobinhoodLiveCloseReport {
	report := RobinhoodLiveCloseReport{Errors: make(map[string]error)}

	tradedCoins := make(map[string]bool)
	for _, sc := range rhLiveCrypto {
		if sc.Type != "spot" {
			continue
		}
		sym := robinhoodSymbol(sc.Args)
		if sym != "" {
			tradedCoins[sym] = true
		}
	}

	for _, p := range positions {
		if !tradedCoins[p.Coin] {
			// Unowned position — kill switch only acts on coins this
			// scheduler is configured to trade. Non-zero sizes are surfaced
			// to the caller via Unconfigured so the plan can latch the
			// switch and prompt manual intervention. Robinhood crypto is
			// spot-only so Size should never be < 0; guard with > 0 so a
			// future change that mistakenly populates a negative balance
			// (e.g. lent / staked) doesn't trigger a market sell for |size|.
			if p.Size > 0 {
				report.Unconfigured = append(report.Unconfigured, p)
			}
			continue
		}
		if p.Size <= 0 {
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
