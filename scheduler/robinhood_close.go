package main

import (
	"context"
	"errors"
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

type RobinhoodCloseRequest struct {
	Symbol   string
	Quantity float64
}

type RobinhoodLiveCloser func(req RobinhoodCloseRequest) (*RobinhoodCloseResult, error)

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
	BookFlat     []string
	Unconfigured []RobinhoodPosition
	Residue      []RobinhoodPosition
	Errors       map[string]error
	Fills        map[string]RobinhoodCloseFill
}

func (r RobinhoodLiveCloseReport) VerifiedFlatCoins() map[string]bool {
	out := map[string]bool{}
	for _, list := range [][]string{r.ClosedCoins, r.AlreadyFlat, r.BookFlat} {
		for _, c := range list {
			out[c] = true
		}
	}
	return out
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

type robinhoodBookSnapshot struct {
	Targets map[string]float64
	Held    map[string]bool
}

func robinhoodBookSnapshotLocked(state *AppState, cfgs []StrategyConfig) robinhoodBookSnapshot {
	out := robinhoodBookSnapshot{Targets: map[string]float64{}, Held: map[string]bool{}}
	if state == nil {
		return out
	}
	configured := make(map[string]StrategyConfig, len(cfgs))
	for _, sc := range cfgs {
		configured[sc.ID] = sc
	}
	markHeld := func(s *StrategyState) {
		for sym, pos := range s.Positions {
			if pos != nil && pos.Quantity > 0 {
				out.Held[sym] = true
			}
		}
	}
	for id, s := range state.Strategies {
		if s == nil {
			continue
		}
		sc, ok := configured[id]
		switch {
		case ok && isRobinhoodLiveSpot(sc):
			markHeld(s)
			coin := robinhoodSymbol(sc.Args)
			if pos := s.Positions[coin]; pos != nil && pos.Side == "long" && pos.Quantity > 0 && robinhoodFinite(pos.Quantity) {
				out.Targets[coin] += pos.Quantity
			}
		case !ok && s.Platform == "robinhood" && s.Type == "spot":
			markHeld(s)
		}
	}
	return out
}

func robinhoodBookSnapshotReader(d robinhoodEmergencyDeps) func() robinhoodBookSnapshot {
	return func() robinhoodBookSnapshot {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return robinhoodBookSnapshotLocked(d.state, d.cfgs)
	}
}

type robinhoodKillSwitchCloseDeps struct {
	Closer     RobinhoodLiveCloser
	Fetcher    RobinhoodPositionsFetcher
	Unresolved map[string]error
	FreshLongs map[string]float64
	Book       robinhoodBookSnapshot
	BookAfter  func() robinhoodBookSnapshot
	Settle     func() robinhoodSettleOutcome
}

func robinhoodUnsoldReason(coin string, remaining, held float64, res *RobinhoodCloseResult) string {
	tol := robinhoodQtyTolerance
	why := "the remaining book is unsold"
	switch {
	case res != nil && res.Close != nil && res.Close.HeldQty > 0 && res.Close.AvailableQty < res.Close.HeldQty*(1-tol):
		why = fmt.Sprintf("reserved holdings: only %.10g of the %.10g held were available to sell", res.Close.AvailableQty, res.Close.HeldQty)
	case res != nil && res.Sizing != nil && res.Sizing.AccountShort:
		why = fmt.Sprintf("account shortfall: the account could sell %.10g of the %.10g asked", res.Sizing.AccountAvailable, res.Sizing.AskedQuantity)
	case res != nil && res.Sizing != nil && res.Close != nil && res.Close.Fill != nil && res.Close.Fill.TotalSz < res.Sizing.SentQuantity*(1-tol):
		why = fmt.Sprintf("partial fill: %.10g of the %.10g sent filled", res.Close.Fill.TotalSz, res.Sizing.SentQuantity)
	case res != nil && res.Sizing != nil && (remaining < res.Sizing.MinOrderSize || remaining < res.Sizing.QuantityIncrement):
		why = fmt.Sprintf("the remaining book is below the venue minimum %.10g or one increment %.10g", res.Sizing.MinOrderSize, res.Sizing.QuantityIncrement)
	}
	return fmt.Sprintf("the go-trader book keeps %.10g %s unsold while the account holds %.10g (%s); it retries next cycle and never sells above the book", remaining, coin, held, why)
}

func forceCloseRobinhoodLive(ctx context.Context, positions []RobinhoodPosition, rhLiveCrypto []StrategyConfig, deps robinhoodKillSwitchCloseDeps) RobinhoodLiveCloseReport {
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

	type candidate struct {
		coin string
		sold bool
		res  *RobinhoodCloseResult
	}
	var candidates []candidate
	for _, coin := range coins {
		size := held[coin]
		if !tradedCoins[coin] {
			if size > 0 {
				p := RobinhoodPosition{Coin: coin, Size: size}
				if _, pending := deps.Unresolved[coin]; pending || deps.Book.Held[coin] {
					report.Unconfigured = append(report.Unconfigured, p)
				} else {
					report.Residue = append(report.Residue, p)
				}
			}
			continue
		}
		if err, ok := deps.Unresolved[coin]; ok {
			report.Errors[coin] = fmt.Errorf("an unresolved Robinhood order holds this coin: %v", err)
			continue
		}
		book := deps.Book.Targets[coin]
		if robinhoodBookFlat(book) {
			report.BookFlat = append(report.BookFlat, coin)
			if size > 0 {
				report.Residue = append(report.Residue, RobinhoodPosition{Coin: coin, Size: size})
			}
			continue
		}
		if size <= 0 {
			if err := robinhoodFlatReadLags(coin, size, deps.FreshLongs); err != nil {
				report.Errors[coin] = err
				continue
			}
			candidates = append(candidates, candidate{coin: coin})
			continue
		}
		if err := ctx.Err(); err != nil {
			report.Errors[coin] = fmt.Errorf("close budget exhausted before submit: %w", err)
			continue
		}
		if deps.Closer == nil {
			report.Errors[coin] = fmt.Errorf("no Robinhood closer wired")
			continue
		}
		result, err := deps.Closer(RobinhoodCloseRequest{Symbol: coin, Quantity: book})
		if result != nil && result.Close != nil && result.Close.Fill != nil && result.Close.Fill.TotalSz > 0 && result.Close.Fill.AvgPx > 0 {
			report.Fills[coin] = *result.Close.Fill
		}
		if err != nil {
			report.Errors[coin] = fmt.Errorf("%v (book %.10g, held %.10g)", err, book, size)
			continue
		}
		if result != nil && result.NoBook {
			report.BookFlat = append(report.BookFlat, coin)
			report.Residue = append(report.Residue, RobinhoodPosition{Coin: coin, Size: size})
			continue
		}
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			if err := robinhoodFlatReadLags(coin, 0, deps.FreshLongs); err != nil {
				report.Errors[coin] = err
				continue
			}
			candidates = append(candidates, candidate{coin: coin, res: result})
			continue
		}
		candidates = append(candidates, candidate{coin: coin, sold: true, res: result})
	}

	if len(candidates) == 0 {
		return report
	}
	var post []RobinhoodPosition
	var verifyErr error
	if deps.Fetcher == nil {
		verifyErr = fmt.Errorf("no strict holdings fetcher is wired, so flat cannot be verified")
	} else {
		post, verifyErr = deps.Fetcher()
	}
	outcome := robinhoodSettleOutcome{SaveErr: fmt.Errorf("no durable settlement is wired, so the remaining book cannot be proven")}
	if deps.Settle != nil {
		outcome = deps.Settle()
	}
	var after robinhoodBookSnapshot
	if deps.BookAfter != nil {
		after = deps.BookAfter()
	} else if outcome.SaveErr == nil {
		outcome.SaveErr = fmt.Errorf("no book snapshot is wired, so the remaining book cannot be proven")
	}
	stillHeld := make(map[string]float64)
	for _, p := range post {
		if p.Size > stillHeld[p.Coin] {
			stillHeld[p.Coin] = p.Size
		}
	}
	for _, c := range candidates {
		remaining, q := after.Targets[c.coin], stillHeld[c.coin]
		switch {
		case verifyErr != nil:
			report.Errors[c.coin] = fmt.Errorf("post-close holdings read failed: %v", verifyErr)
			continue
		case outcome.SaveErr != nil:
			report.Errors[c.coin] = fmt.Errorf("settlement is not saved: %v", outcome.SaveErr)
			continue
		case outcome.Unresolved[c.coin] != nil:
			report.Errors[c.coin] = fmt.Errorf("an order for this coin is still unresolved after settlement: %v", outcome.Unresolved[c.coin])
			continue
		case robinhoodBookFlat(remaining):
		case q <= 0:
			if err := robinhoodFlatReadLags(c.coin, q, deps.FreshLongs); err != nil {
				report.Errors[c.coin] = err
				continue
			}
		default:
			report.Errors[c.coin] = fmt.Errorf("%s", robinhoodUnsoldReason(c.coin, remaining, q, c.res))
			continue
		}
		if c.sold {
			report.ClosedCoins = append(report.ClosedCoins, c.coin)
		} else {
			report.AlreadyFlat = append(report.AlreadyFlat, c.coin)
		}
		if q > 0 {
			report.Residue = append(report.Residue, RobinhoodPosition{Coin: c.coin, Size: q})
		}
	}
	sort.Slice(report.Residue, func(i, j int) bool { return report.Residue[i].Coin < report.Residue[j].Coin })
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
	return func(cr RobinhoodCloseRequest) (*RobinhoodCloseResult, error) {
		symbol := cr.Symbol
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
		a, req, err := reserveRobinhoodEmergencyAttempt(d.store, d.state, owners, d.mu, symbol, origin, cr.Quantity, time.Now())
		if errors.Is(err, errRobinhoodNoBookToSell) {
			return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: symbol}, NoBook: true}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the close attempt could not be saved before sending, so nothing was sent: %w", err)
		}
		d.mu.RLock()
		s := d.state.Strategies[a.StrategyID]
		d.mu.RUnlock()
		res, stderr, err := robinhoodCloseFn(robinhoodLiveCloseScript, symbol, a.RefID, req.Quantity)
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "[rh-close] %s stderr: %s\n", symbol, stderr)
		}
		if res != nil {
			res.AskedQty = req.Quantity
		}
		rawErr := err
		if res != nil {
			rawErr = res.RunErr
		}
		if res != nil && res.OrderOutcome == robinhoodOutcomeAlreadyFlat && res.Close != nil && res.Close.AlreadyFlat && rawErr == nil {
			d.mu.Lock()
			recordErr := recordRobinhoodOrderOutcomeLocked(d.store, s, a, &RobinhoodExecuteResult{OrderOutcome: robinhoodOutcomeAlreadyFlat}, robinhoodOrderNoFill, "already flat; no order was sent", nil, time.Now())
			d.mu.Unlock()
			if recordErr != nil {
				notifyRobinhoodOrderHold(d.notifier, s.ID, a, fmt.Sprintf("the already-flat result could not be saved (%v)", recordErr), time.Now())
				return res, fmt.Errorf("the already-flat result could not be saved: %v", recordErr)
			}
			return res, nil
		}
		orderRes := robinhoodCloseAsOrderResult(res)
		class, detail := classifyRobinhoodOrderOutcome(orderRes, rawErr, a, 0)
		if res == nil && err != nil {
			detail = err.Error()
		}
		recordRes := orderRes
		var sizing *RobinhoodSellSizing
		sizeErr := fmt.Errorf("the close script gave no result")
		if res != nil {
			sizing, sizeErr = checkRobinhoodSellSizing(res.Sizing, req, orderRes)
		}
		if sizeErr != nil {
			sizing = nil
			if class == robinhoodOrderConfirmed {
				class, detail = robinhoodOrderUnknown, "the close result has no valid sent_quantity: "+sizeErr.Error()
			}
			if orderRes != nil && orderRes.RequestedQuantity != 0 {
				cp := *orderRes
				cp.RequestedQuantity = 0
				recordRes = &cp
			}
		}
		d.mu.Lock()
		recordErr := recordRobinhoodOrderOutcomeLocked(d.store, s, a, recordRes, class, detail, sizing, time.Now())
		d.mu.Unlock()
		if sizing != nil && sizing.Capped {
			fmt.Printf("[WARN] rh-close: %s sell capped: asked %.10g, held %.10g, available %.10g, sent %.10g (attempt %s) (#1825)\n", symbol, sizing.AskedQuantity, sizing.AccountQuantity, sizing.AccountAvailable, sizing.SentQuantity, a.AttemptID)
		}
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
