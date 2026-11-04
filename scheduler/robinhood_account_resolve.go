package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type robinhoodAccountResolveDeps struct {
	store   *StateStore
	state   *AppState
	mu      *sync.RWMutex
	lookup  func(id string) (StrategyConfig, bool)
	cfgs    func() []StrategyConfig
	fetcher RobinhoodPositionsFetcher
	mark    func(coin string) (float64, error)
}

type robinhoodAccountResolution struct {
	StrategyID string
	Coin       string
	Action     string
	CurrentKey string
	Entry      robinhoodBookEntry
	Source     string
	AttemptID  string
}

func defaultRobinhoodReleaseMark(coin string) (float64, error) {
	pair := coin + "/USD"
	prices, err := FetchPrices([]string{pair})
	if err != nil {
		return 0, err
	}
	px, ok := prices[pair]
	if !ok {
		return 0, fmt.Errorf("no mark price for %s", pair)
	}
	return px, nil
}

func robinhoodAccountResolutionBlock(state *AppState, coin, strategyID, positionID, currentKey, action string) (*robinhoodOrderAttempt, string) {
	var allowed *robinhoodOrderAttempt
	for _, a := range robinhoodUnresolvedAttemptsLocked(state) {
		if a.Symbol != coin {
			continue
		}
		if action == robinhoodAccountActionBind && allowed == nil && robinhoodSettlingSellTargets(a, strategyID, positionID, currentKey) {
			allowed = a
			continue
		}
		return nil, fmt.Sprintf("refused: Robinhood attempt %s (%s %s %s, status %s) is unresolved for %s; resolve it first with /%s%s", a.AttemptID, a.Origin, a.Side, a.Symbol, a.Status, coin, commandPrefix, robinhoodOperatorCommandName)
	}
	return allowed, ""
}

func robinhoodSettlingSellTargets(a *robinhoodOrderAttempt, strategyID, positionID, currentKey string) bool {
	if a == nil || a.Status != robinhoodAttemptSettling || a.Side != "sell" || currentKey == "" || a.AccountKey != currentKey || a.VenueAccountID == "" {
		return false
	}
	req, err := parseRobinhoodOrderRequest(a)
	if err != nil {
		return false
	}
	if a.Origin == robinhoodOriginSignal {
		return a.StrategyID == strategyID && req.PrevPositionID != "" && req.PrevPositionID == positionID
	}
	for _, peer := range req.Peers {
		if peer == strategyID {
			return true
		}
	}
	return false
}

func robinhoodHeldBookFor(d robinhoodAccountResolveDeps, strategyID string) (StrategyConfig, *Position, string) {
	sc, ok := d.lookup(strategyID)
	if !ok {
		return sc, nil, fmt.Sprintf("refused: strategy %q is not configured", strategyID)
	}
	if !isRobinhoodLiveSpot(sc) {
		return sc, nil, fmt.Sprintf("refused: strategy %q is not a live Robinhood spot strategy", strategyID)
	}
	s := d.state.Strategies[strategyID]
	if s == nil {
		return sc, nil, fmt.Sprintf("refused: strategy %s has no state", strategyID)
	}
	coin := robinhoodSymbol(sc.Args)
	pos := s.Positions[coin]
	if !robinhoodPositionHeld(pos) || pos.Side != "long" {
		return sc, nil, fmt.Sprintf("refused: strategy %s holds no open %s book", strategyID, coin)
	}
	return sc, pos, ""
}

func prepareRobinhoodAccountResolution(d robinhoodAccountResolveDeps, strategyID, action string) (*robinhoodAccountResolution, string) {
	if d.store == nil || d.state == nil || d.mu == nil || d.lookup == nil || d.cfgs == nil {
		return nil, "status server not ready"
	}
	if action != robinhoodAccountActionBind && action != robinhoodAccountActionRelease {
		return nil, fmt.Sprintf("refused: action %q is not %s or %s", action, robinhoodAccountActionBind, robinhoodAccountActionRelease)
	}
	currentKey := robinhoodAccountKey()
	if currentKey == "" {
		return nil, "refused: ROBINHOOD_USERNAME is not set, so no fresh holdings read of the configured account is possible"
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	sc, pos, refusal := robinhoodHeldBookFor(d, strategyID)
	if refusal != "" {
		return nil, refusal
	}
	coin := robinhoodSymbol(sc.Args)
	state := robinhoodBookAccountState(pos, currentKey, "")
	reason := robinhoodAccountHoldReason(state, pos.AccountKey, pos.VenueAccountID, currentKey, "")
	if state == robinhoodAccountCurrent {
		reason = "the stored binding names the configured account; the fresh holdings read decides, and the action is refused if that read names the bound venue account"
	}
	allowed, refusal := robinhoodAccountResolutionBlock(d.state, coin, strategyID, pos.TradePositionID, currentKey, action)
	if refusal != "" {
		return nil, refusal
	}
	res := &robinhoodAccountResolution{StrategyID: strategyID, Coin: coin, Action: action, CurrentKey: currentKey, Entry: robinhoodBookEntryOf(strategyID, pos), Source: pos.AccountBindingSource}
	if allowed != nil {
		res.AttemptID = allowed.AttemptID
	}
	source := pos.AccountBindingSource
	if source == "" {
		source = "none"
	}
	head := fmt.Sprintf("Robinhood book %s %s: position %s, quantity %.10g, state %s (%s).\nBinding: account %s, venue account %s, source %s. Configured account: %s.\n",
		strategyID, coin, pos.TradePositionID, pos.Quantity, state, reason,
		robinhoodKeyFingerprint(pos.AccountKey), robinhoodVenueFingerprint(pos.VenueAccountID), source, robinhoodKeyFingerprint(currentKey))
	switch action {
	case robinhoodAccountActionBind:
		prompt := head + fmt.Sprintf("Action: %s. go-trader reads the configured account's holdings now. It binds the book to that account only if the account holds at least the bound peer books plus this book, and the venue account matches any stored venue account. After the bind, go-trader can SELL these %s from the configured account.", robinhoodAccountActionBind, coin)
		if allowed != nil {
			prompt += fmt.Sprintf(" Settling sale %s (%.10g %s) targets this book; it books into the bound book with the next settlement.", allowed.AttemptID, allowed.ConfirmedQty, coin)
		}
		return res, prompt + "\nConfirm only if these coins are on the configured Robinhood account."
	}
	prompt := head + fmt.Sprintf("Action: %s. Confirm only if these %s are outside EVERY account this daemon manages (for example on a former account you no longer use with go-trader). go-trader reads the configured account's holdings now and refuses if that account holds more %s than its bound books. It then closes the book at the current mark as a model-only close (reason robinhood_account_released), records your attestation and the prior binding, and sends NO order. It does not claim the former account is flat.", robinhoodAccountActionRelease, coin, coin)
	return res, prompt
}

func robinhoodCurrentPeerQtyLocked(state *AppState, cfgs []StrategyConfig, coin, exclude, currentKey, readID string) float64 {
	total := 0.0
	for _, sc := range robinhoodLiveOwnersForCoin(coin, cfgs) {
		if sc.ID == exclude {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		pos := s.Positions[coin]
		if pos == nil || pos.Side != "long" || !robinhoodPositionHeld(pos) {
			continue
		}
		if robinhoodBookAccountState(pos, currentKey, readID) == robinhoodAccountCurrent {
			total += pos.Quantity
		}
	}
	return total
}

func (d robinhoodAccountResolveDeps) revalidateLocked(r *robinhoodAccountResolution, readID string) (*StrategyState, *Position, *robinhoodOrderAttempt, string) {
	if robinhoodAccountKey() != r.CurrentKey {
		return nil, nil, nil, "refused: the configured Robinhood account changed while you were confirming; nothing was changed"
	}
	_, pos, refusal := robinhoodHeldBookFor(d, r.StrategyID)
	if refusal != "" {
		return nil, nil, nil, refusal + "; nothing was changed"
	}
	if !r.Entry.matches(pos) || pos.AccountBindingSource != r.Source {
		return nil, nil, nil, fmt.Sprintf("refused: the %s book of %s changed while you were confirming; nothing was changed. Run the command again.", r.Coin, r.StrategyID)
	}
	if readID != "" && robinhoodBookAccountState(pos, r.CurrentKey, readID) == robinhoodAccountCurrent {
		return nil, nil, nil, fmt.Sprintf("refused: the book is already bound to the configured account, and the fresh holdings read names its bound venue account %s; nothing was changed", robinhoodVenueFingerprint(readID))
	}
	allowed, refusal := robinhoodAccountResolutionBlock(d.state, r.Coin, r.StrategyID, pos.TradePositionID, r.CurrentKey, r.Action)
	if refusal != "" {
		return nil, nil, nil, refusal + "; nothing was changed"
	}
	if (allowed == nil) != (r.AttemptID == "") || (allowed != nil && allowed.AttemptID != r.AttemptID) {
		return nil, nil, nil, "refused: the unresolved Robinhood attempts for this coin changed while you were confirming; nothing was changed"
	}
	s := d.state.Strategies[r.StrategyID]
	if s.robinhoodPartitionSavePending {
		return nil, nil, nil, fmt.Sprintf("refused: strategy %s holds a shared Robinhood settlement that commits only with the next full live save; try again after the next cycle", r.StrategyID)
	}
	return s, pos, allowed, ""
}

func stageRobinhoodResolutionBook(s *StrategyState) *StrategyState {
	c := *s
	c.Positions = make(map[string]*Position, len(s.Positions))
	for k, pos := range s.Positions {
		if pos == nil {
			continue
		}
		pc := *pos
		pc.TPOIDs = append([]int64(nil), pos.TPOIDs...)
		pc.TPArmedTiers = append([]bool(nil), pos.TPArmedTiers...)
		pc.TPConsumptions = append([]TPConsumption(nil), pos.TPConsumptions...)
		pc.RegimeWindows = cloneStringMap(pos.RegimeWindows)
		pc.DirectionCertifiedStatesAtOpen = cloneStringMap(pos.DirectionCertifiedStatesAtOpen)
		if pos.RegimeSnapshots != nil {
			pc.RegimeSnapshots = make(map[string]RegimeSnapshot, len(pos.RegimeSnapshots))
			for rk, rv := range pos.RegimeSnapshots {
				pc.RegimeSnapshots[rk] = rv
			}
		}
		c.Positions[k] = &pc
	}
	c.OptionPositions = make(map[string]*OptionPosition, len(s.OptionPositions))
	for k, op := range s.OptionPositions {
		if op == nil {
			continue
		}
		oc := *op
		c.OptionPositions[k] = &oc
	}
	c.TradeHistory = append([]Trade(nil), s.TradeHistory...)
	c.ClosedPositions = append([]ClosedPosition(nil), s.ClosedPositions...)
	c.ClosedOptionPositions = append([]ClosedOptionPosition(nil), s.ClosedOptionPositions...)
	c.pendingTradeDiagnostics = append([]TradeDiagnosticsRow(nil), s.pendingTradeDiagnostics...)
	if s.RiskState.PendingCircuitCloses != nil {
		c.RiskState.PendingCircuitCloses = make(map[string]*PendingCircuitClose, len(s.RiskState.PendingCircuitCloses))
		for k, v := range s.RiskState.PendingCircuitCloses {
			if v == nil {
				continue
			}
			vc := *v
			vc.Symbols = append([]PendingCircuitCloseSymbol(nil), v.Symbols...)
			c.RiskState.PendingCircuitCloses[k] = &vc
		}
	}
	return &c
}

func (d robinhoodAccountResolveDeps) commitStagedLocked(s, staged *StrategyState) error {
	db, err := d.store.dbForStrategy(s.ID)
	if err != nil {
		return err
	}
	scope, _ := d.store.scopeForStrategy(s.ID)
	if err := db.saveStrategyBookWithAcks(staged, scope, nil, nil); err != nil {
		return err
	}
	staged.deferTradePersist = s.deferTradePersist
	*s = *staged
	return nil
}

func applyRobinhoodAccountResolution(d robinhoodAccountResolveDeps, operatorID string, r robinhoodAccountResolution, now time.Time) string {
	if d.store == nil || d.state == nil || d.mu == nil || d.lookup == nil || d.cfgs == nil || d.fetcher == nil {
		return "status server not ready"
	}
	if strings.TrimSpace(operatorID) == "" {
		return "refused: the operator is unknown; the book stays held"
	}
	release, ok := tryLockRobinhoodCoin(r.CurrentKey, r.Coin)
	if !ok {
		return fmt.Sprintf("refused: another Robinhood order or resolution for %s is in progress; the book stays held. Try again shortly.", r.Coin)
	}
	defer release()

	d.mu.RLock()
	_, _, _, refusal := d.revalidateLocked(&r, "")
	d.mu.RUnlock()
	if refusal != "" {
		return refusal
	}

	read, err := d.fetcher()
	if err != nil {
		return fmt.Sprintf("refused: the fresh holdings read of the configured account failed: %v. The book stays held.", err)
	}
	if strings.TrimSpace(read.AccountID) == "" {
		return "refused: the fresh holdings read names no crypto account; the book stays held"
	}
	noteRobinhoodVenueRead(r.CurrentKey, read.AccountID)
	held := robinhoodOnAccountSize(r.Coin, read.Positions)
	mark := 0.0
	if r.Action == robinhoodAccountActionRelease {
		if d.mark == nil {
			return "refused: no mark price source is wired; the book stays held"
		}
		mark, err = d.mark(r.Coin)
		if err != nil || !(mark > 0) || !robinhoodFinite(mark) {
			return fmt.Sprintf("refused: no finite positive current mark for %s (%v); the book stays held", r.Coin, err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	s, pos, allowed, refusal := d.revalidateLocked(&r, read.AccountID)
	if refusal != "" {
		return refusal
	}
	peers := robinhoodCurrentPeerQtyLocked(d.state, d.cfgs(), r.Coin, r.StrategyID, r.CurrentKey, read.AccountID)
	tol := robinhoodQtyTolerance
	staged := stageRobinhoodResolutionBook(s)
	sp := staged.Positions[r.Coin]
	switch r.Action {
	case robinhoodAccountActionBind:
		if pos.VenueAccountID != "" && pos.VenueAccountID != read.AccountID {
			return fmt.Sprintf("refused: the book is bound to venue account %s, and the configured login reads venue account %s; the book stays held", robinhoodVenueFingerprint(pos.VenueAccountID), robinhoodVenueFingerprint(read.AccountID))
		}
		sold := 0.0
		if allowed != nil {
			if allowed.VenueAccountID != read.AccountID {
				return fmt.Sprintf("refused: settling sale %s was sent from venue account %s, and the configured login reads %s; the book stays held", allowed.AttemptID, robinhoodVenueFingerprint(allowed.VenueAccountID), robinhoodVenueFingerprint(read.AccountID))
			}
			sold = allowed.ConfirmedQty
		}
		need := peers + pos.Quantity - sold
		if held < need*(1-tol) {
			return fmt.Sprintf("refused: the configured account holds %.10g %s, below the %.10g needed for its bound books (%.10g) plus this book (%.10g) net of a settling sale (%.10g); the book stays held", held, r.Coin, need, peers, pos.Quantity, sold)
		}
		sp.AccountKey = r.CurrentKey
		sp.VenueAccountID = read.AccountID
		sp.AccountBindingSource = robinhoodBindingSourceOperator + operatorID
		if err := d.commitStagedLocked(s, staged); err != nil {
			return fmt.Sprintf("refused: saving the binding failed: %v. Nothing was changed; the book stays held and a retry is safe.", err)
		}
		robinhoodHeldBookAlerts.Range(func(k, _ interface{}) bool {
			if ks, _ := k.(string); strings.HasPrefix(ks, r.StrategyID+"|"+r.Coin+"|") {
				robinhoodHeldBookAlerts.Delete(k)
			}
			return true
		})
		fmt.Printf("[CRITICAL] rh-account: operator %s bound the %s book of %s (%.10g, position %s) to account %s venue %s; the account held %.10g against %.10g needed (#1921)\n", operatorID, r.Coin, r.StrategyID, pos.Quantity, pos.TradePositionID, robinhoodKeyFingerprint(r.CurrentKey), robinhoodVenueFingerprint(read.AccountID), held, need)
		return fmt.Sprintf("Bound the %s book of %s (%.10g) to the configured Robinhood account (venue account %s). Sales and flat checks for this book resume next cycle.", r.Coin, r.StrategyID, pos.Quantity, robinhoodVenueFingerprint(read.AccountID))
	}

	if held > peers*(1+tol)+tol {
		return fmt.Sprintf("refused: the configured account holds %.10g %s, above the %.10g in its bound books, so this book's coins may be on the configured account; run %s instead if they are, or move the extra coins first. The book stays held.", held, r.Coin, peers, robinhoodAccountActionBind)
	}
	if !pos.OpenedAt.IsZero() && now.Sub(pos.OpenedAt) < robinhoodHoldingsLagWindow {
		return fmt.Sprintf("refused: the book opened within %s, so the holdings read may lag the opening buy; try again later. The book stays held.", robinhoodHoldingsLagWindow)
	}
	staged.deferTradePersist = true
	bookRobinhoodAccountRelease(staged, r, operatorID, sp, mark, held, peers, read.AccountID, now)
	if err := d.commitStagedLocked(s, staged); err != nil {
		return fmt.Sprintf("refused: saving the release failed: %v. Nothing was changed; the book stays held and a retry is safe.", err)
	}
	fmt.Printf("[CRITICAL] rh-account: operator %s released the %s book of %s (%.10g, position %s, prior key %s venue %s) at mark $%.6f on attestation; the configured account held %.10g against %.10g in bound books; no order was sent (#1921)\n", operatorID, r.Coin, r.StrategyID, r.Entry.Quantity, r.Entry.PositionID, robinhoodKeyFingerprint(r.Entry.AccountKey), robinhoodVenueFingerprint(r.Entry.VenueAccountID), mark, held, peers)
	return fmt.Sprintf("Released the %s book of %s (%.10g) at mark $%.6f on your attestation. No order was sent. The close is recorded as robinhood_account_released with your id and the prior binding; it does not claim the former account is flat.", r.Coin, r.StrategyID, r.Entry.Quantity, mark)
}

func bookRobinhoodAccountRelease(s *StrategyState, r robinhoodAccountResolution, operatorID string, pos *Position, mark, held, peers float64, readID string, now time.Time) {
	const reason = "robinhood_account_released"
	proceeds := pos.Quantity * mark
	pnl := proceeds - pos.Quantity*pos.AvgCost
	if closePositionIsCorrupt(pos) {
		proceeds, pnl = 0, 0
	} else {
		s.Cash += proceeds
	}
	source := pos.AccountBindingSource
	if source == "" {
		source = "none"
	}
	details := fmt.Sprintf("Released on owner attestation (%s) by operator %s: the coins are outside every account this daemon manages; prior binding key=%s venue=%s source=%s; a fresh holdings read of the configured account (venue %s) showed %.10g against %.10g in its bound books; no order was sent and the former account is not verified flat, PnL: $%.2f (model-only adjustment), pre-streak=%d",
		reason, operatorID, robinhoodKeyFingerprint(pos.AccountKey), robinhoodVenueFingerprint(pos.VenueAccountID), source, robinhoodVenueFingerprint(readID), held, peers, pnl, s.RiskState.ConsecutiveLosses)
	trade := Trade{
		Timestamp:         now.UTC(),
		StrategyID:        s.ID,
		Symbol:            r.Coin,
		PositionID:        ensurePositionTradeID(s.ID, r.Coin, pos),
		Side:              closeTradeSide(pos.Side),
		Quantity:          math.Abs(pos.Quantity),
		Price:             mark,
		Value:             proceeds,
		TradeType:         "spot",
		Details:           details,
		IsClose:           true,
		RealizedPnL:       pnl,
		PnLGross:          true,
		FeeSource:         FeeSourceReconcileAdjustment,
		Regime:            s.Regime,
		EntryATR:          pos.EntryATR,
		StopLossTriggerPx: pos.StopLossTriggerPx,
		StopLossATRMult:   pos.StopLossATRMult,
		TPTiersJSON:       pos.TPTiersJSON,
	}
	RecordTrade(s, trade)
	recordPositionTradeResult(s, pos, pnl)
	recordClosedPosition(s, pos, mark, pnl, reason, now.UTC())
	delete(s.Positions, r.Coin)
}
