package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type RobinhoodPosition struct {
	Coin     string
	Size     float64
	AvgPrice float64
}

var robinhoodLiveCloseScript = "shared_scripts/close_robinhood_position.py"

var robinhoodFetchPositionsScript = "shared_scripts/fetch_robinhood_positions.py"

type RobinhoodLiveCloser func(symbol string) (*RobinhoodCloseResult, error)

var robinhoodCloseFn = RunRobinhoodClose

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
	Fills        map[string]RobinhoodCloseFill
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

const robinhoodHoldingsLagWindow = 10 * time.Minute

func robinhoodFreshLongsLocked(state *AppState, cfgs []StrategyConfig, now time.Time) map[string]float64 {
	out := map[string]float64{}
	if state == nil {
		return out
	}
	for _, sc := range cfgs {
		if !isRobinhoodLiveSpot(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		coin := robinhoodSymbol(sc.Args)
		pos := s.Positions[coin]
		if pos == nil || pos.Side != "long" || !(pos.Quantity > 0) || pos.OpenedAt.IsZero() {
			continue
		}
		if now.Sub(pos.OpenedAt) < robinhoodHoldingsLagWindow {
			out[coin] += pos.Quantity
		}
	}
	return out
}

func robinhoodFlatReadLags(coin string, readQty float64, freshLongs map[string]float64) error {
	fresh := freshLongs[coin]
	if fresh > 0 && readQty < fresh*(1-robinhoodQtyTolerance) {
		return fmt.Errorf("the holdings read shows %.10g, below the %.10g bought within the last %s; the read may lag the fill, so flat is not confirmed", readQty, fresh, robinhoodHoldingsLagWindow)
	}
	return nil
}

func forceCloseRobinhoodLive(ctx context.Context, positions []RobinhoodPosition, rhLiveCrypto []StrategyConfig, closer RobinhoodLiveCloser, fetcher RobinhoodPositionsFetcher, unresolved map[string]error, freshLongs map[string]float64) RobinhoodLiveCloseReport {
	report := RobinhoodLiveCloseReport{Errors: make(map[string]error), Fills: make(map[string]RobinhoodCloseFill)}

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
	held := make(map[string]float64)
	coinSet := make(map[string]bool)
	for coin := range tradedCoins {
		coinSet[coin] = true
	}
	for _, p := range positions {
		coinSet[p.Coin] = true
		if p.Size > held[p.Coin] {
			held[p.Coin] = p.Size
		}
	}
	coins := make([]string, 0, len(coinSet))
	for c := range coinSet {
		coins = append(coins, c)
	}
	sort.Strings(coins)

	var closedCandidates, flatCandidates []string
	for _, coin := range coins {
		size := held[coin]
		if !tradedCoins[coin] {
			if size > 0 {
				report.Unconfigured = append(report.Unconfigured, RobinhoodPosition{Coin: coin, Size: size})
			}
			continue
		}
		if err, ok := unresolved[coin]; ok {
			report.Errors[coin] = fmt.Errorf("an unresolved Robinhood order holds this coin: %v", err)
			continue
		}
		if size <= 0 {
			if err := robinhoodFlatReadLags(coin, size, freshLongs); err != nil {
				report.Errors[coin] = err
				continue
			}
			flatCandidates = append(flatCandidates, coin)
			continue
		}
		if err := ctx.Err(); err != nil {
			report.Errors[coin] = fmt.Errorf("close budget exhausted before submit: %w", err)
			continue
		}
		if closer == nil {
			report.Errors[coin] = fmt.Errorf("no Robinhood closer wired")
			continue
		}
		result, err := closer(coin)
		if result != nil && result.Close != nil && result.Close.Fill != nil && result.Close.Fill.TotalSz > 0 && result.Close.Fill.AvgPx > 0 {
			report.Fills[coin] = *result.Close.Fill
		}
		if err != nil {
			report.Errors[coin] = err
			continue
		}
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			if err := robinhoodFlatReadLags(coin, 0, freshLongs); err != nil {
				report.Errors[coin] = err
				continue
			}
			flatCandidates = append(flatCandidates, coin)
			continue
		}
		closedCandidates = append(closedCandidates, coin)
	}

	if len(closedCandidates)+len(flatCandidates) == 0 {
		return report
	}
	var post []RobinhoodPosition
	var verifyErr error
	if fetcher == nil {
		verifyErr = fmt.Errorf("no strict holdings fetcher is wired, so flat cannot be verified")
	} else {
		post, verifyErr = fetcher()
	}
	stillHeld := make(map[string]float64)
	for _, p := range post {
		if p.Size > stillHeld[p.Coin] {
			stillHeld[p.Coin] = p.Size
		}
	}
	verify := func(coin string, into *[]string) {
		if verifyErr != nil {
			report.Errors[coin] = fmt.Errorf("post-close holdings read failed: %v", verifyErr)
			return
		}
		if q := stillHeld[coin]; q > 0 {
			report.Errors[coin] = fmt.Errorf("still held %.10g after close", q)
			return
		}
		*into = append(*into, coin)
	}
	for _, coin := range closedCandidates {
		verify(coin, &report.ClosedCoins)
	}
	for _, coin := range flatCandidates {
		verify(coin, &report.AlreadyFlat)
	}
	return report
}

type robinhoodEmergencyDeps struct {
	store    *StateStore
	state    *AppState
	mu       *sync.RWMutex
	cfgs     []StrategyConfig
	notifier *MultiNotifier
}

func newRobinhoodEmergencyCloser(d robinhoodEmergencyDeps, origin string) RobinhoodLiveCloser {
	return func(symbol string) (*RobinhoodCloseResult, error) {
		owners := robinhoodLiveOwnersForCoin(symbol, d.cfgs)
		if len(owners) == 0 {
			return nil, fmt.Errorf("no configured live Robinhood strategy owns %s", symbol)
		}
		accountKey := robinhoodAccountKey()
		if accountKey == "" {
			return nil, fmt.Errorf("ROBINHOOD_USERNAME is not set; no order was sent")
		}
		release, ok := tryLockRobinhoodCoin(accountKey, symbol)
		if !ok {
			return nil, fmt.Errorf("another Robinhood order for %s is being resolved; no order was sent", symbol)
		}
		defer release()
		now := time.Now()
		d.mu.RLock()
		s := d.state.Strategies[owners[0].ID]
		prev := robinhoodUnresolvedForCoinLocked(d.state, accountKey, symbol)
		d.mu.RUnlock()
		if prev != nil {
			return nil, fmt.Errorf("Robinhood attempt %s for %s is unresolved; no order was sent", prev.AttemptID, symbol)
		}
		if s == nil {
			return nil, fmt.Errorf("strategy %s has no state", owners[0].ID)
		}
		peers := make([]string, 0, len(owners))
		for _, o := range owners {
			peers = append(peers, o.ID)
		}
		req := robinhoodOrderRequest{Peers: peers, Script: robinhoodLiveCloseScript}
		a, err := reserveRobinhoodAttempt(d.store, d.state, s, d.mu, symbol, origin, "sell", req, now)
		if err != nil {
			return nil, fmt.Errorf("the close attempt could not be saved before sending, so nothing was sent: %w", err)
		}
		res, stderr, err := robinhoodCloseFn(robinhoodLiveCloseScript, symbol, a.RefID)
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "[rh-close] %s stderr: %s\n", symbol, stderr)
		}
		rawErr := err
		if res != nil {
			rawErr = res.RunErr
		}
		if res != nil && res.OrderOutcome == robinhoodOutcomeAlreadyFlat && res.Close != nil && res.Close.AlreadyFlat && rawErr == nil {
			d.mu.Lock()
			recordErr := recordRobinhoodOrderOutcomeLocked(d.store, s, a, &RobinhoodExecuteResult{OrderOutcome: robinhoodOutcomeAlreadyFlat}, robinhoodOrderNoFill, "already flat; no order was sent", time.Now())
			d.mu.Unlock()
			if recordErr != nil {
				notifyRobinhoodOrderHold(d.notifier, s.ID, a, fmt.Sprintf("the already-flat result could not be saved (%v)", recordErr), time.Now())
				return res, fmt.Errorf("the already-flat result could not be saved: %v", recordErr)
			}
			return res, nil
		}
		orderRes := robinhoodCloseAsOrderResult(res)
		requested := 0.0
		if res != nil && res.Close != nil {
			requested = res.Close.HeldQty
		}
		class, detail := classifyRobinhoodOrderOutcome(orderRes, rawErr, a, requested)
		if res == nil && err != nil {
			detail = err.Error()
		}
		d.mu.Lock()
		recordErr := recordRobinhoodOrderOutcomeLocked(d.store, s, a, orderRes, class, detail, time.Now())
		d.mu.Unlock()
		switch class {
		case robinhoodOrderConfirmed:
			if recordErr != nil {
				notifyRobinhoodOrderHold(d.notifier, s.ID, a, fmt.Sprintf("the confirmed close fill could not be saved (%v)", recordErr), time.Now())
				return res, fmt.Errorf("the confirmed close fill could not be saved: %v", recordErr)
			}
			return res, nil
		case robinhoodOrderNoFill:
			if recordErr != nil {
				notifyRobinhoodOrderHold(d.notifier, s.ID, a, fmt.Sprintf("the no-fill result could not be saved (%v)", recordErr), time.Now())
			}
			return res, fmt.Errorf("close ended with no fill: %s", detail)
		}
		notifyRobinhoodOrderHold(d.notifier, s.ID, a, detail, time.Now())
		return res, fmt.Errorf("close unresolved (%s): %s", class, detail)
	}
}

func demoteRobinhoodUnresolvedCoins(plan *KillSwitchClosePlan, state *AppState) []string {
	if plan == nil {
		return nil
	}
	held := map[string]*robinhoodOrderAttempt{}
	for _, a := range robinhoodUnresolvedAttemptsLocked(state) {
		if held[a.Symbol] == nil {
			held[a.Symbol] = a
		}
	}
	if len(held) == 0 {
		return nil
	}
	report := &plan.RHCloseReport
	if report.Errors == nil {
		report.Errors = make(map[string]error)
	}
	var demoted []string
	keep := func(coins []string) []string {
		var out []string
		for _, coin := range coins {
			if a := held[coin]; a != nil {
				report.Errors[coin] = fmt.Errorf("Robinhood attempt %s is still unresolved (%s)", a.AttemptID, a.Status)
				demoted = append(demoted, coin)
				continue
			}
			out = append(out, coin)
		}
		return out
	}
	report.ClosedCoins = keep(report.ClosedCoins)
	report.AlreadyFlat = keep(report.AlreadyFlat)
	if len(demoted) > 0 {
		plan.OnChainConfirmedFlat = false
	}
	sort.Strings(demoted)
	return demoted
}
