package main

import (
	"context"
	"fmt"
	"os"
	"sort"
)

type RobinhoodPosition struct {
	Coin     string
	Size     float64
	AvgPrice float64
}

var robinhoodLiveCloseScript = "shared_scripts/close_robinhood_position.py"

var robinhoodFetchPositionsScript = "shared_scripts/fetch_robinhood_positions.py"

type RobinhoodLiveCloser func(symbol string) (*RobinhoodCloseResult, error)

func defaultRobinhoodLiveCloser(symbol string) (*RobinhoodCloseResult, error) {
	result, stderr, err := RunRobinhoodClose(robinhoodLiveCloseScript, symbol)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[rh-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

type RobinhoodPositionsFetcher func() ([]RobinhoodPosition, error)

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

type RobinhoodLiveCloseReport struct {
	ClosedCoins  []string
	AlreadyFlat  []string
	Unconfigured []RobinhoodPosition
	Errors       map[string]error
}

func (r RobinhoodLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

func (r RobinhoodLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

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
		result, err := closer(p.Coin)
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
