package main

import (
	"context"
	"fmt"
	"os"
	"sort"
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
		if _, err := closer(p.Coin); err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}
