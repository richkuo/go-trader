package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	quoteRecoveryOverlapMs  = 60_000
	quoteRecoveryMaxRetries = 3
	quoteFeeAssetUSDC       = "USDC"
	quoteFeeAdjustmentType  = "fee_adjustment"
	quoteCloseReasonFill    = "quote_fill"
	quoteCloseReasonStop    = "quote_stop"
	quoteCloseReasonExit    = "quote_exit"
)

var quoteSupportedFeeAssets = map[string]bool{quoteFeeAssetUSDC: true}

type quoteOwnerState struct {
	Partition   RiskPartition
	Coin        string
	Provider    string
	AccountKey  string
	Namespace   string
	Revision    int64
	Contract    *quoteContractRow
	Orders      []*quoteOrderRow
	Generations []*quoteGeneration
	Unapplied   []*quoteFillRow
	Funding     []*quoteFundingEvidenceRow
	Settlements int

	LastHolds       []string
	SettlementHolds []string
	Envelope        *quoteEnvelope
}

func (q *quoteOwnerState) clone() *quoteOwnerState {
	if q == nil {
		return nil
	}
	c := *q
	if q.Contract != nil {
		cc := *q.Contract
		c.Contract = &cc
	}
	c.Orders = make([]*quoteOrderRow, len(q.Orders))
	for i, o := range q.Orders {
		oc := *o
		c.Orders[i] = &oc
	}
	c.Generations = make([]*quoteGeneration, len(q.Generations))
	for i, g := range q.Generations {
		gc := *g
		c.Generations[i] = &gc
	}
	c.Unapplied = append([]*quoteFillRow(nil), q.Unapplied...)
	c.Funding = append([]*quoteFundingEvidenceRow(nil), q.Funding...)
	c.LastHolds = append([]string(nil), q.LastHolds...)
	c.SettlementHolds = append([]string(nil), q.SettlementHolds...)
	return &c
}

func (q *quoteOwnerState) busy() bool {
	if q == nil {
		return false
	}
	if q.Contract != nil && mustQuoteRat(q.Contract.Book.Qty).Sign() != 0 {
		return true
	}
	for _, o := range q.Orders {
		if !o.resolved() {
			return true
		}
	}
	for _, g := range q.Generations {
		if g.Status != quoteGenResolved {
			return true
		}
	}
	return len(q.Unapplied) > 0 || len(q.Funding) > 0
}

func (q *quoteOwnerState) order(rowID int64) *quoteOrderRow {
	for _, o := range q.Orders {
		if o.RowID == rowID {
			return o
		}
	}
	return nil
}

func (q *quoteOwnerState) generation(g int64) *quoteGeneration {
	for _, gen := range q.Generations {
		if gen.Generation == g {
			return gen
		}
	}
	return nil
}

func (q *quoteOwnerState) generationForDeadline(deadlineMs int64) *quoteGeneration {
	for _, gen := range q.Generations {
		if gen.DeadlineMs == deadlineMs {
			return gen
		}
	}
	return nil
}

func (q *quoteOwnerState) sidesOf(generation int64) []*quoteOrderRow {
	var out []*quoteOrderRow
	for _, o := range q.Orders {
		if (o.Role == quoteRoleBid || o.Role == quoteRoleAsk) && o.Generation == generation {
			out = append(out, o)
		}
	}
	return out
}

func (q *quoteOwnerState) currentStop() *quoteOrderRow {
	var best *quoteOrderRow
	for _, o := range q.Orders {
		if o.Role != quoteRoleStop {
			continue
		}
		if best == nil || o.Generation > best.Generation {
			best = o
		}
	}
	return best
}

func (q *quoteOwnerState) openStops() []*quoteOrderRow {
	var out []*quoteOrderRow
	for _, o := range q.Orders {
		if o.Role == quoteRoleStop && !o.terminal() {
			out = append(out, o)
		}
	}
	return out
}

func cloneQuoteStrategy(s *StrategyState) *StrategyState {
	c := *s
	c.Positions = make(map[string]*Position, len(s.Positions))
	for k, p := range s.Positions {
		pc := *p
		pc.TPOIDs = append([]int64(nil), p.TPOIDs...)
		pc.TPArmedTiers = append([]bool(nil), p.TPArmedTiers...)
		pc.TPConsumptions = append([]TPConsumption(nil), p.TPConsumptions...)
		c.Positions[k] = &pc
	}
	c.TradeHistory = append([]Trade(nil), s.TradeHistory...)
	c.ClosedPositions = append([]ClosedPosition(nil), s.ClosedPositions...)
	c.pendingTradeDiagnostics = append([]TradeDiagnosticsRow(nil), s.pendingTradeDiagnostics...)
	if s.RiskState.PendingCircuitCloses != nil {
		c.RiskState.PendingCircuitCloses = make(map[string]*PendingCircuitClose, len(s.RiskState.PendingCircuitCloses))
		for k, v := range s.RiskState.PendingCircuitCloses {
			c.RiskState.PendingCircuitCloses[k] = v
		}
	}
	c.PaperFunding = clonePaperFundingState(s.PaperFunding)
	c.Quote = s.Quote.clone()
	return &c
}

func clonePaperFundingState(st *paperFundingState) *paperFundingState {
	if st == nil {
		return nil
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return nil
	}
	var out paperFundingState
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil
	}
	if out.Coins == nil {
		out.Coins = make(map[string]*paperFundingCoin)
	}
	return &out
}

func mutatePaperPerpsBookAt(s *StrategyState, symbol string, atMs int64, mutate func()) error {
	if s == nil || s.PaperFunding == nil {
		mutate()
		return nil
	}
	st := s.PaperFunding
	if atMs < st.StartedAtMs || atMs < st.ObservedThroughMs {
		return fmt.Errorf("a %s inventory change at %d is earlier than the paper funding anchor (started %d, verified through %d)", symbol, atMs, st.StartedAtMs, st.ObservedThroughMs)
	}
	qty, pid, bad := paperFundingBookQty(s, symbol)
	if bad != "" {
		return fmt.Errorf("%s: %s", symbol, bad)
	}
	c := st.Coins[symbol]
	if c == nil {
		if qty != 0 {
			return fmt.Errorf("%s holds %.10g with no paper funding anchor", symbol, qty)
		}
		c = st.coin(symbol, atMs)
	}
	if atMs < c.Anchor.SinceMs || atMs < c.Anchor.VerifiedMs {
		return fmt.Errorf("a %s inventory change at %d is earlier than the inventory anchor (since %d, verified %d)", symbol, atMs, c.Anchor.SinceMs, c.Anchor.VerifiedMs)
	}
	if !c.anchorMatches(qty, pid) {
		return fmt.Errorf("%s book qty %.10g (position %s) does not match the inventory anchor %.10g (position %s)", symbol, qty, pid, c.Anchor.SignedQty, c.Anchor.PositionID)
	}
	c.Anchor.VerifiedMs = atMs
	if st.active == nil {
		st.active = make(map[string]bool)
	}
	st.active[symbol] = true
	mutate()
	delete(st.active, symbol)
	qty2, pid2, bad2 := paperFundingBookQty(s, symbol)
	if bad2 != "" {
		return fmt.Errorf("%s: %s", symbol, bad2)
	}
	c.moveTo(qty2, pid2, atMs)
	return nil
}

type quoteEvidence struct {
	Fill     quoteRawFill
	Row      *quoteFillRow
	Order    *quoteOrderRow
	Role     string
	position string
}

type quoteRecovery struct {
	Fills          []quoteRawFill
	Position       quotePositionRead
	ApplyThroughMs int64
	BoundaryMs     int64
	Holds          []string
	Critical       []string
}

func (a *quoteAdmission) readVenue(ctx context.Context, owner *quoteOwnerState) quoteRecovery {
	out := quoteRecovery{}
	coin := owner.Coin
	since := owner.Contract.StartMs
	if owner.Contract.AppliedBoundaryMs > since {
		since = owner.Contract.AppliedBoundaryMs
	}
	since -= quoteRecoveryOverlapMs
	if since < 1 {
		since = 1
	}
	now := a.Now().UnixMilli()
	for attempt := 0; attempt < quoteRecoveryMaxRetries; attempt++ {
		first, err := a.venue.RecoverFills(ctx, coin, since, now)
		if err != nil {
			out.Holds = []string{fmt.Sprintf("fill history read failed: %v", err)}
			return out
		}
		if !first.Coverage.Complete {
			out.Holds = []string{fmt.Sprintf("fill history coverage is incomplete: %s", first.Coverage.Reason)}
			return out
		}
		if len(first.Conflicts) > 0 {
			out.Holds = append([]string{"the venue returned conflicting copies of a fill"}, first.Conflicts...)
			return out
		}
		pos, err := a.venue.Position(ctx, coin)
		if err != nil {
			out.Holds = []string{fmt.Sprintf("strict position read failed: %v", err)}
			return out
		}
		recheckFrom := first.Coverage.ThroughMs - quoteRecoveryOverlapMs
		if recheckFrom < since {
			recheckFrom = since
		}
		second, err := a.venue.RecoverFills(ctx, coin, recheckFrom, pos.ReadAtMs)
		if err != nil {
			out.Holds = []string{fmt.Sprintf("fill history recheck failed: %v", err)}
			return out
		}
		if !second.Coverage.Complete || second.Coverage.ThroughMs < pos.ReadAtMs {
			out.Holds = []string{fmt.Sprintf("fill history recheck does not cover the position read at %d (%s)", pos.ReadAtMs, second.Coverage.Reason)}
			return out
		}
		if len(second.Conflicts) > 0 {
			out.Holds = append([]string{"the venue returned conflicting copies of a fill on recheck"}, second.Conflicts...)
			return out
		}
		seen := make(map[string]string, len(first.Fills))
		for _, f := range first.Fills {
			seen[f.key()] = f.digest()
		}
		stable := true
		var merged []quoteRawFill
		merged = append(merged, first.Fills...)
		for _, f := range second.Fills {
			d, ok := seen[f.key()]
			if ok {
				if d != f.digest() {
					out.Holds = []string{fmt.Sprintf("fill %s changed between two history reads", f.key())}
					return out
				}
				continue
			}
			if f.TimeMs <= pos.ReadAtMs {
				stable = false
			}
			merged = append(merged, f)
		}
		if !stable {
			now = a.Now().UnixMilli()
			continue
		}
		out.Fills = merged
		out.Position = pos
		out.ApplyThroughMs = pos.ReadAtMs
		out.BoundaryMs = pos.ReadAtMs - 1
		return out
	}
	out.Holds = []string{fmt.Sprintf("no stable fill prefix after %d attempts: fills kept arriving across the position read", quoteRecoveryMaxRetries)}
	return out
}

func (a *quoteAdmission) attributeFill(owner *quoteOwnerState, f quoteRawFill) (*quoteOrderRow, string) {
	for _, o := range owner.Orders {
		if f.ClientOrderID != "" && strings.EqualFold(o.ClientOrderID, f.ClientOrderID) {
			return o, ""
		}
	}
	for _, o := range owner.Orders {
		if o.VenueOrderID != "" && o.VenueOrderID == f.OrderID {
			return o, ""
		}
	}
	return nil, fmt.Sprintf("fill %s (order %s, client %q) is not attributable to a saved quote, stop or close identity", f.key(), f.OrderID, f.ClientOrderID)
}

func (a *quoteAdmission) persistRecoveredFills(owner *quoteOwnerState, rec *quoteRecovery) ([]string, []string) {
	var holds, critical []string
	var inserts []quoteFillInsert
	acct := owner.AccountKey
	for _, f := range rec.Fills {
		if !strings.EqualFold(f.Coin, owner.Coin) || f.TimeMs < owner.Contract.StartMs {
			continue
		}
		if err := f.validate(); err != nil {
			holds = append(holds, err.Error())
			continue
		}
		if f.Account != acct || f.Namespace != owner.Namespace {
			holds = append(holds, fmt.Sprintf("fill %s names account %q namespace %q, not %q %q", f.key(), f.Account, f.Namespace, acct, owner.Namespace))
			continue
		}
		o, why := a.attributeFill(owner, f)
		if o == nil {
			holds = append(holds, why)
			continue
		}
		inserts = append(inserts, quoteFillInsert{Fill: f, Role: o.Role, OrderRowID: o.RowID})
	}
	if len(holds) > 0 {
		return holds, critical
	}
	key, err := a.ownerKey()
	if err != nil {
		return []string{err.Error()}, nil
	}
	_, conflicts, err := a.db.recordQuoteFills(key, a.venue.Provider(), inserts, a.Now().UnixMilli())
	if err != nil {
		return []string{fmt.Sprintf("fill evidence could not be saved: %v", err)}, nil
	}
	if len(conflicts) > 0 {
		critical = append(critical, conflicts...)
		holds = append(holds, conflicts...)
	}
	return holds, critical
}

type quoteStaged struct {
	Kind          string
	BaseRevision  int64
	BaseCash      float64
	BaseTrades    int
	BaseFunding   string
	Staged        *StrategyState
	Book          quoteExactBook
	FillIDs       []int64
	FillKeys      []string
	FundingIDs    []string
	FundingJSON   string
	OrderProgress []quoteOrderProgress
	Generations   []*quoteGeneration
	BoundaryMs    int64
	TradeResults  []float64
	FeeAdjust     []float64
	Holds         []string
	Critical      []string
	NewTrades     []Trade
	Position      quotePositionRead
	PositionKnown bool
}

type quoteLedger struct {
	qty      *big.Rat
	cost     *big.Rat
	cash     *big.Rat
	realized *big.Rat
	fees     *big.Rat
	funding  *big.Rat
	anchor   *big.Rat
	initial  *big.Rat
	posID    string
	firstMs  int64
}

func newQuoteLedger(b quoteExactBook) *quoteLedger {
	return &quoteLedger{
		qty: mustQuoteRat(b.Qty), cost: mustQuoteRat(b.Cost), cash: mustQuoteRat(b.Cash),
		realized: mustQuoteRat(b.RealizedGross), fees: mustQuoteRat(b.Fees), funding: mustQuoteRat(b.Funding),
		anchor: mustQuoteRat(b.RiskAnchorPx), initial: mustQuoteRat(b.InitialQty), posID: b.PositionID, firstMs: b.FirstOpenMs,
	}
}

func (l *quoteLedger) book() quoteExactBook {
	b := quoteExactBook{
		Qty: quoteRatString(l.qty), Cost: quoteRatString(l.cost), Cash: quoteRatString(l.cash),
		RealizedGross: quoteRatString(l.realized), Fees: quoteRatString(l.fees), Funding: quoteRatString(l.funding),
		PositionID: l.posID, FirstOpenMs: l.firstMs,
	}
	if l.qty.Sign() != 0 {
		b.RiskAnchorPx = quoteRatString(l.anchor)
		b.InitialQty = quoteRatString(l.initial)
	}
	return b
}

func quoteSideSign(side string) int {
	if side == "sell" {
		return -1
	}
	return 1
}

func (a *quoteAdmission) projectQuoteBook(s *StrategyState, l *quoteLedger) error {
	coin := a.sc.Symbol
	if coin == "" {
		coin = quoteCoinOf(a.sc)
	}
	s.Cash = quoteRatFloat(l.cash)
	if !quoteProjectionOK(l.cash, s.Cash) {
		return fmt.Errorf("cash projection %v does not match the exact cash %s", s.Cash, quoteRatString(l.cash))
	}
	if l.qty.Sign() == 0 {
		delete(s.Positions, coin)
		return nil
	}
	abs := quoteRatAbs(l.qty)
	avg := quoteRatQuo(l.cost, abs)
	side := "long"
	if l.qty.Sign() < 0 {
		side = "short"
	}
	pos := s.Positions[coin]
	if pos == nil || pos.TradePositionID != l.posID {
		pos = &Position{Symbol: coin, Multiplier: 1, Leverage: EffectiveExchangeLeverage(a.sc), OwnerStrategyID: s.ID}
		s.Positions[coin] = pos
	}
	pos.Quantity = quoteRatFloat(abs)
	pos.AvgCost = quoteRatFloat(avg)
	pos.Side = side
	pos.TradePositionID = l.posID
	pos.InitialQuantity = quoteRatFloat(l.initial)
	pos.RiskAnchorPrice = quoteRatFloat(l.anchor)
	pos.OpenedAt = time.UnixMilli(l.firstMs).UTC()
	if !quoteProjectionOK(abs, pos.Quantity) || !quoteProjectionOK(avg, pos.AvgCost) {
		return fmt.Errorf("position projection does not match the exact book")
	}
	return nil
}

func quoteFeeRat(f quoteRawFill) (*big.Rat, error) {
	fee, err := parseQuoteDecimal(f.Fee)
	if err != nil {
		return nil, err
	}
	if !quoteSupportedFeeAssets[strings.ToUpper(strings.TrimSpace(f.FeeAsset))] {
		return nil, fmt.Errorf("fill %s charges its fee in the unsupported asset %q", f.key(), f.FeeAsset)
	}
	return fee, nil
}

func quoteCloseReasonFor(role string) string {
	switch role {
	case quoteRoleStop:
		return quoteCloseReasonStop
	case quoteRoleClose:
		return quoteCloseReasonExit
	}
	return quoteCloseReasonFill
}

func (a *quoteAdmission) bookQuoteFill(s *StrategyState, l *quoteLedger, ev *quoteEvidence, results *[]float64) error {
	f := ev.Fill
	sz := mustQuoteRat(f.Size)
	px := mustQuoteRat(f.Px)
	fee, err := quoteFeeRat(f)
	if err != nil {
		return err
	}
	coin := a.sc.Symbol
	if coin == "" {
		coin = quoteCoinOf(a.sc)
	}
	t := time.UnixMilli(f.TimeMs).UTC()
	dir := quoteSideSign(f.Side)
	sign := l.qty.Sign()
	oid := "quote:" + f.OrderID + ":" + f.TradeID
	open := func(qty, openFee *big.Rat, add bool) error {
		if !add {
			l.posID = newTradePositionID(s.ID, coin, t)
			l.firstMs = f.TimeMs
			l.anchor = new(big.Rat).Set(px)
			l.initial = new(big.Rat).Set(qty)
			l.cost = quoteRatMul(qty, px)
			if dir > 0 {
				l.qty = new(big.Rat).Set(qty)
			} else {
				l.qty = quoteRatNeg(qty)
			}
		} else {
			l.cost = quoteRatAdd(l.cost, quoteRatMul(qty, px))
			l.initial = quoteRatAdd(l.initial, qty)
			if dir > 0 {
				l.qty = quoteRatAdd(l.qty, qty)
			} else {
				l.qty = quoteRatSub(l.qty, qty)
			}
		}
		l.cash = quoteRatSub(l.cash, openFee)
		l.fees = quoteRatAdd(l.fees, openFee)
		if err := a.projectQuoteBook(s, l); err != nil {
			return err
		}
		tradeType := "perps"
		label := "Quote open"
		if add {
			tradeType = scaleInTradeType
			label = "Quote add"
		}
		withStrategyBookClock(s, t, func() {
			RecordTrade(s, Trade{
				Timestamp:       t,
				StrategyID:      s.ID,
				Symbol:          coin,
				PositionID:      l.posID,
				Side:            f.Side,
				Quantity:        quoteRatFloat(qty),
				Price:           quoteRatFloat(px),
				Value:           quoteRatFloat(quoteRatMul(qty, px)),
				TradeType:       tradeType,
				Details:         fmt.Sprintf("%s %s %s @ %s (%s, fee %s)", label, f.Side, quoteDecimalText(qty), f.Px, ev.Role, quoteDecimalText(openFee)),
				ExchangeOrderID: oid,
				ExchangeFee:     quoteRatFloat(openFee),
				FeeSource:       FeeSourceUserFills,
				PnLGross:        true,
			})
		})
		return nil
	}
	if sign == 0 || sign == dir {
		return open(sz, fee, sign != 0)
	}
	abs := quoteRatAbs(l.qty)
	reduce := quoteRatMin(sz, abs)
	residual := quoteRatSub(sz, reduce)
	closeFee := quoteRatQuo(quoteRatMul(fee, reduce), sz)
	openFee := quoteRatSub(fee, closeFee)
	avg := quoteRatQuo(l.cost, abs)
	gross := quoteRatMul(reduce, quoteRatSub(px, avg))
	if sign < 0 {
		gross = quoteRatNeg(gross)
	}
	posSide := "long"
	if sign < 0 {
		posSide = "short"
	}
	pre := s.Positions[coin]
	var preCopy Position
	if pre != nil {
		preCopy = *pre
	}
	l.cost = quoteRatSub(l.cost, quoteRatMul(avg, reduce))
	if dir > 0 {
		l.qty = quoteRatAdd(l.qty, reduce)
	} else {
		l.qty = quoteRatSub(l.qty, reduce)
	}
	l.cash = quoteRatAdd(l.cash, quoteRatSub(gross, closeFee))
	l.realized = quoteRatAdd(l.realized, gross)
	l.fees = quoteRatAdd(l.fees, closeFee)
	net := quoteRatFloat(quoteRatSub(gross, closeFee))
	closedID := l.posID
	full := l.qty.Sign() == 0
	if full {
		l.cost = quoteRatZero()
		l.posID = ""
		l.firstMs = 0
		l.anchor = quoteRatZero()
		l.initial = quoteRatZero()
	}
	if err := a.projectQuoteBook(s, l); err != nil {
		return err
	}
	withStrategyBookClock(s, t, func() {
		RecordTrade(s, Trade{
			Timestamp:       t,
			StrategyID:      s.ID,
			Symbol:          coin,
			PositionID:      closedID,
			Side:            f.Side,
			Quantity:        quoteRatFloat(reduce),
			Price:           quoteRatFloat(px),
			Value:           quoteRatFloat(quoteRatMul(reduce, px)),
			TradeType:       "perps",
			Details:         fmt.Sprintf("Quote reduce %s %s %s @ %s (%s), PnL: $%.4f (fee %s)", posSide, f.Side, quoteDecimalText(reduce), f.Px, ev.Role, net, quoteDecimalText(closeFee)),
			IsClose:         true,
			RealizedPnL:     quoteRatFloat(gross),
			PnLGross:        true,
			ExchangeOrderID: oid,
			ExchangeFee:     quoteRatFloat(closeFee),
			FeeSource:       FeeSourceUserFills,
		})
	})
	*results = append(*results, net)
	RecordTradeResult(&s.RiskState, net)
	if full && pre != nil {
		withStrategyBookClock(s, t, func() {
			recordClosedPosition(s, &preCopy, quoteRatFloat(px), net, quoteCloseReasonFor(ev.Role), t)
		})
	}
	if residual.Sign() > 0 {
		return open(residual, openFee, false)
	}
	if openFee.Sign() != 0 {
		return fmt.Errorf("fill %s left a fee share %s with no residual", f.key(), quoteRatString(openFee))
	}
	return nil
}

func (a *quoteAdmission) orderQuoteFills(evs []*quoteEvidence, start *big.Rat) ([]*quoteEvidence, string) {
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].Fill.TimeMs != evs[j].Fill.TimeMs {
			return evs[i].Fill.TimeMs < evs[j].Fill.TimeMs
		}
		return evs[i].Fill.key() < evs[j].Fill.key()
	})
	var out []*quoteEvidence
	running := new(big.Rat).Set(start)
	for i := 0; i < len(evs); {
		j := i
		for j < len(evs) && evs[j].Fill.TimeMs == evs[i].Fill.TimeMs {
			j++
		}
		group := evs[i:j]
		if len(group) == 1 {
			if sp := group[0].Fill.StartPosition; sp != "" {
				if v := mustQuoteRat(sp); v.Cmp(running) != 0 {
					return nil, fmt.Sprintf("fill %s starts from position %s, the applied chain is at %s", group[0].Fill.key(), sp, quoteRatString(running))
				}
			}
			out = append(out, group[0])
			running = quoteRatAdd(running, signedQuoteFillQty(group[0].Fill))
			i = j
			continue
		}
		chain, why := uniqueQuoteFillChain(group, running)
		if why != "" {
			return nil, why
		}
		for _, ev := range chain {
			running = quoteRatAdd(running, signedQuoteFillQty(ev.Fill))
		}
		out = append(out, chain...)
		i = j
	}
	return out, ""
}

func signedQuoteFillQty(f quoteRawFill) *big.Rat {
	sz := mustQuoteRat(f.Size)
	if f.Side == "sell" {
		return quoteRatNeg(sz)
	}
	return sz
}

func uniqueQuoteFillChain(group []*quoteEvidence, start *big.Rat) ([]*quoteEvidence, string) {
	for _, ev := range group {
		if ev.Fill.StartPosition == "" {
			return nil, fmt.Sprintf("%d fills share event time %d and fill %s carries no starting-position evidence, so their order is ambiguous", len(group), ev.Fill.TimeMs, ev.Fill.key())
		}
	}
	var solutions [][]*quoteEvidence
	used := make([]bool, len(group))
	var walk func(cur *big.Rat, chain []*quoteEvidence)
	walk = func(cur *big.Rat, chain []*quoteEvidence) {
		if len(solutions) > 1 {
			return
		}
		if len(chain) == len(group) {
			solutions = append(solutions, append([]*quoteEvidence(nil), chain...))
			return
		}
		for i, ev := range group {
			if used[i] {
				continue
			}
			if mustQuoteRat(ev.Fill.StartPosition).Cmp(cur) != 0 {
				continue
			}
			used[i] = true
			walk(quoteRatAdd(cur, signedQuoteFillQty(ev.Fill)), append(chain, ev))
			used[i] = false
		}
	}
	if len(group) > 8 {
		return nil, fmt.Sprintf("%d fills share event time %d; the chain is too large to prove unique", len(group), group[0].Fill.TimeMs)
	}
	walk(start, nil)
	switch len(solutions) {
	case 0:
		return nil, fmt.Sprintf("%d fills at event time %d form no chain from the applied position %s", len(group), group[0].Fill.TimeMs, quoteRatString(start))
	case 1:
		return solutions[0], ""
	}
	if quoteChainsEquivalent(solutions[0], solutions[1]) {
		return solutions[0], ""
	}
	return nil, fmt.Sprintf("%d fills at event time %d form more than one chain from position %s", len(group), group[0].Fill.TimeMs, quoteRatString(start))
}

func quoteChainsEquivalent(a, b []*quoteEvidence) bool {
	for i := range a {
		x, y := a[i].Fill, b[i].Fill
		if x.Side != y.Side || mustQuoteRat(x.Size).Cmp(mustQuoteRat(y.Size)) != 0 || mustQuoteRat(x.Px).Cmp(mustQuoteRat(y.Px)) != 0 ||
			mustQuoteRat(x.Fee).Cmp(mustQuoteRat(y.Fee)) != 0 || a[i].Role != b[i].Role {
			return false
		}
	}
	return true
}

func (a *quoteAdmission) stageSettlement(ctx context.Context, deadlineMs int64, funding *paperFundingInputs, marks []quoteDeadlineMark) (*quoteStaged, error) {
	a.mu.RLock()
	live := a.state.Strategies[a.sc.ID]
	if live == nil || live.Quote == nil || live.Quote.Contract == nil {
		a.mu.RUnlock()
		return nil, fmt.Errorf("quote owner %s has no durable contract", a.sc.ID)
	}
	staged := cloneQuoteStrategy(live)
	baseFunding, _ := marshalPaperFundingState(live)
	st := &quoteStaged{Kind: quoteSettleNone, BaseRevision: live.Quote.Revision, BaseCash: live.Cash, BaseTrades: len(live.TradeHistory), BaseFunding: baseFunding, Staged: staged}
	a.mu.RUnlock()
	owner := staged.Quote
	st.Book = owner.Contract.Book
	st.BoundaryMs = owner.Contract.AppliedBoundaryMs
	staged.deferTradePersist = true

	rec := a.readVenue(ctx, owner)
	if len(rec.Holds) == 0 {
		st.Position, st.PositionKnown = rec.Position, true
		holds, critical := a.persistRecoveredFills(owner, &rec)
		rec.Holds = append(rec.Holds, holds...)
		rec.Critical = append(rec.Critical, critical...)
	}
	key, err := a.ownerKey()
	if err != nil {
		return nil, err
	}
	durable, err := a.db.loadQuoteDurableOwner(key)
	if err != nil {
		return nil, err
	}
	owner.Unapplied = durable.Unapplied
	owner.Funding = durable.Funding
	owner.Orders = durable.Orders
	st.Holds = append(st.Holds, rec.Holds...)
	st.Critical = append(st.Critical, rec.Critical...)

	ledger := newQuoteLedger(owner.Contract.Book)
	startQty := new(big.Rat).Set(ledger.qty)
	oldBoundary := owner.Contract.AppliedBoundaryMs
	var apply []*quoteEvidence
	var corrections []*quoteFillRow
	if len(st.Holds) == 0 {
		newBoundary := rec.ApplyThroughMs
		for _, row := range owner.Unapplied {
			if row.Kind == "fee_correction" {
				if row.Fill.TimeMs <= newBoundary {
					corrections = append(corrections, row)
				}
				continue
			}
			if row.Fill.TimeMs <= oldBoundary {
				msg := fmt.Sprintf("fill %s at %d is earlier than the applied coverage boundary %d; the venue history contradicts the settled prefix", row.Key, row.Fill.TimeMs, oldBoundary)
				st.Holds = append(st.Holds, msg)
				st.Critical = append(st.Critical, msg)
				continue
			}
			if row.Fill.TimeMs > newBoundary {
				continue
			}
			o := owner.order(row.OrderRowID)
			if o == nil {
				st.Holds = append(st.Holds, fmt.Sprintf("fill %s names order row %d, which the owner does not hold", row.Key, row.OrderRowID))
				continue
			}
			apply = append(apply, &quoteEvidence{Fill: row.Fill, Row: row, Order: o, Role: row.Role})
		}
		if len(st.Holds) == 0 {
			ordered, why := a.orderQuoteFills(apply, startQty)
			if why != "" {
				st.Holds = append(st.Holds, why)
			} else {
				apply = ordered
			}
		}
	}
	coin := quoteCoinOf(a.sc)
	if len(st.Holds) == 0 {
		var results []float64
		for _, ev := range apply {
			ev := ev
			var bookErr error
			mutErr := mutatePaperPerpsBookAt(staged, coin, ev.Fill.TimeMs, func() {
				bookErr = a.bookQuoteFill(staged, ledger, ev, &results)
			})
			if mutErr != nil {
				msg := fmt.Sprintf("fill %s: %v", ev.Fill.key(), mutErr)
				st.Holds = append(st.Holds, msg)
				st.Critical = append(st.Critical, msg)
				break
			}
			if bookErr != nil {
				st.Holds = append(st.Holds, fmt.Sprintf("fill %s could not be booked: %v", ev.Fill.key(), bookErr))
				break
			}
		}
		if len(st.Holds) == 0 {
			for _, row := range corrections {
				delta, err := a.applyFeeCorrection(staged, ledger, row, owner)
				if errors.Is(err, errQuoteCorrectionPending) {
					continue
				}
				if err != nil {
					st.Holds = append(st.Holds, err.Error())
					break
				}
				st.FeeAdjust = append(st.FeeAdjust, delta)
				st.FillIDs = append(st.FillIDs, row.ID)
				st.FillKeys = append(st.FillKeys, row.Key)
			}
		}
		if len(st.Holds) == 0 {
			want, err := parseQuoteDecimal(rec.Position.Size)
			if err != nil {
				st.Holds = append(st.Holds, fmt.Sprintf("the strict position read has size %q", rec.Position.Size))
			} else if want.Cmp(ledger.qty) != 0 {
				st.Holds = append(st.Holds, fmt.Sprintf("the settled inventory %s does not match the fresh venue position %s at %d", quoteRatString(ledger.qty), rec.Position.Size, rec.Position.ReadAtMs))
			}
		}
		if len(st.Holds) == 0 {
			st.TradeResults = results
			for _, ev := range apply {
				st.FillIDs = append(st.FillIDs, ev.Row.ID)
				st.FillKeys = append(st.FillKeys, ev.Row.Key)
			}
			if rec.BoundaryMs > st.BoundaryMs {
				st.BoundaryMs = rec.BoundaryMs
			}
			if staged.PaperFunding != nil {
				if c := staged.PaperFunding.Coins[coin]; c != nil {
					qty, pid, _ := paperFundingBookQty(staged, coin)
					if c.anchorMatches(qty, pid) && rec.BoundaryMs > c.Anchor.VerifiedMs {
						c.Anchor.VerifiedMs = rec.BoundaryMs
					}
				}
				if rec.BoundaryMs > staged.PaperFunding.ObservedThroughMs {
					staged.PaperFunding.ObservedThroughMs = rec.BoundaryMs
				}
			}
			applied := make(map[int64]*big.Rat)
			for _, ev := range apply {
				cur := applied[ev.Order.RowID]
				if cur == nil {
					cur = mustQuoteRat(ev.Order.AppliedQty)
				}
				applied[ev.Order.RowID] = quoteRatAdd(cur, mustQuoteRat(ev.Fill.Size))
			}
			ids := make([]int64, 0, len(applied))
			for id := range applied {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				o := owner.order(id)
				if mustQuoteRat(o.ConfirmedQty).Cmp(applied[id]) < 0 {
					o.ConfirmedQty = quoteRatString(applied[id])
				}
				o.AppliedQty = quoteRatString(applied[id])
				st.OrderProgress = append(st.OrderProgress, quoteOrderProgress{RowID: id, AppliedQty: o.AppliedQty})
			}
		}
	}
	if len(st.Holds) > 0 {
		staged = cloneQuoteStrategy(stagedBaseline(a, staged))
		st.Staged = staged
		staged.deferTradePersist = true
		ledger = newQuoteLedger(owner.Contract.Book)
		st.FillIDs, st.FillKeys, st.OrderProgress, st.TradeResults, st.FeeAdjust = nil, nil, nil, nil, nil
		st.BoundaryMs = oldBoundary
	}
	fundingBooked, fundingIDs, fundingDetail := a.settleQuoteFunding(staged, ledger, deadlineMs, st.BoundaryMs, funding, marks, owner)
	st.FundingIDs = fundingIDs
	st.FundingJSON = fundingDetail
	st.Book = ledger.book()
	if err := a.projectQuoteBook(staged, ledger); err != nil {
		return nil, err
	}
	switch {
	case len(st.FillIDs) > 0 && fundingBooked:
		st.Kind = quoteSettleBoth
	case len(st.FillIDs) > 0:
		st.Kind = quoteSettleFills
	case fundingBooked:
		st.Kind = quoteSettleFunding
	case len(st.Holds) == 0 && st.BoundaryMs > oldBoundary:
		st.Kind = quoteSettleFunding
	}
	st.NewTrades = append([]Trade(nil), staged.TradeHistory[st.BaseTrades:]...)
	st.Generations = a.resolvedGenerations(owner)
	return st, nil
}

func stagedBaseline(a *quoteAdmission, staged *StrategyState) *StrategyState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	live := a.state.Strategies[a.sc.ID]
	c := cloneQuoteStrategy(live)
	c.Quote = staged.Quote
	return c
}

var errQuoteCorrectionPending = errors.New("the corrected fill is not applied yet")

func (a *quoteAdmission) applyFeeCorrection(s *StrategyState, l *quoteLedger, row *quoteFillRow, owner *quoteOwnerState) (float64, error) {
	key, err := a.ownerKey()
	if err != nil {
		return 0, err
	}
	all, err := a.db.loadQuoteFillRows(a.db.db, key, false)
	if err != nil {
		return 0, err
	}
	var orig *quoteFillRow
	prior := quoteRatZero()
	for _, f := range all {
		if f.ID == row.CorrectsID && f.Kind == "fill" {
			orig = f
		}
	}
	if orig == nil {
		return 0, fmt.Errorf("fee correction %s names fill %d, which is not a fill of this owner", row.Key, row.CorrectsID)
	}
	if orig.SettlementID == 0 {
		return 0, errQuoteCorrectionPending
	}
	if !strings.EqualFold(orig.Fill.FeeAsset, row.Fill.FeeAsset) {
		return 0, fmt.Errorf("fee correction %s changes the fee asset from %s to %s", row.Key, orig.Fill.FeeAsset, row.Fill.FeeAsset)
	}
	prior = mustQuoteRat(orig.Fill.Fee)
	for _, f := range all {
		if f.Kind == "fee_correction" && f.CorrectsID == orig.ID && f.SettlementID != 0 && f.ID != row.ID {
			if f.ID > row.ID {
				return 0, fmt.Errorf("fee correction %s is older than an applied correction of the same fill", row.Key)
			}
			prior = mustQuoteRat(f.Fill.Fee)
		}
	}
	newFee, err := quoteFeeRat(row.Fill)
	if err != nil {
		return 0, err
	}
	delta := quoteRatSub(newFee, prior)
	l.cash = quoteRatSub(l.cash, delta)
	l.fees = quoteRatAdd(l.fees, delta)
	coin := quoteCoinOf(a.sc)
	t := time.UnixMilli(row.Fill.TimeMs).UTC()
	d := quoteRatFloat(delta)
	withStrategyBookClock(s, t, func() {
		RecordTrade(s, Trade{
			Timestamp:       t,
			StrategyID:      s.ID,
			Symbol:          coin,
			PositionID:      "quote_fee_correction",
			Side:            quoteFeeAdjustmentType,
			TradeType:       quoteFeeAdjustmentType,
			Details:         fmt.Sprintf("Quote fee correction for fill %s: %s -> %s", orig.Key, quoteDecimalText(prior), quoteDecimalText(newFee)),
			ExchangeOrderID: "quote_fee_correction:" + row.Fill.TradeID,
			ExchangeFee:     d,
			FeeSource:       FeeSourceUserFills,
			PnLGross:        true,
		})
	})
	rolloverDailyPnL(&s.RiskState)
	s.RiskState.DailyPnL -= d
	return -d, nil
}

func (a *quoteAdmission) resolvedGenerations(owner *quoteOwnerState) []*quoteGeneration {
	var out []*quoteGeneration
	for _, g := range owner.Generations {
		if g.Status == quoteGenResolved {
			continue
		}
		sides := owner.sidesOf(g.Generation)
		if len(sides) == 0 && g.Status == quoteGenReserved {
			continue
		}
		done := true
		for _, o := range sides {
			if !o.resolved() {
				done = false
			}
		}
		if done {
			gc := *g
			gc.Status = quoteGenResolved
			proof, _ := json.Marshal(map[string]interface{}{"sides": len(sides), "resolved": true})
			gc.TerminalJSON = string(proof)
			out = append(out, &gc)
		}
	}
	return out
}

func (a *quoteAdmission) settleQuoteFunding(s *StrategyState, l *quoteLedger, deadlineMs, boundaryMs int64, inputs *paperFundingInputs, marks []quoteDeadlineMark, owner *quoteOwnerState) (bool, []string, string) {
	coin := quoteCoinOf(a.sc)
	before := len(s.TradeHistory)
	var ids []string
	if a.partition.Scope == ScopePaper {
		if s.PaperFunding == nil || inputs == nil {
			return false, nil, ""
		}
		var mark float64
		for _, m := range marks {
			if strings.EqualFold(m.Coin, coin) && quoteMarkFresh(m, deadlineMs) {
				mark = m.Price
			}
		}
		res := paperFundingRunResult{}
		run := &paperFundingRun{sc: a.sc, s: s, st: s.PaperFunding, part: a.partition, store: a.store, nowMs: deadlineMs, res: &res, current: map[string]bool{}}
		syms := paperFundingSampleCoins(run, map[string]float64{coin: mark}, false)
		paperFundingSettleCoins(run, syms, *inputs)
		for _, line := range res.Logs {
			fmt.Println(line)
		}
		for _, msg := range res.Alerts {
			a.queueAlert(msg, strings.Contains(msg, paperFundingCriticalPrefix))
		}
	} else {
		for _, ev := range owner.Funding {
			if ev.EventMs > boundaryMs {
				continue
			}
			var raw hlLedgerEvent
			if err := json.Unmarshal([]byte(ev.RawJSON), &raw); err != nil {
				a.queueAlert(fmt.Sprintf("quote owner %s: funding evidence %s is unreadable: %v", a.sc.ID, ev.DedupID, err), true)
				continue
			}
			amount := parseHLFloat(raw.Delta.USDC)
			withStrategyBookClock(s, time.UnixMilli(ev.EventMs).UTC(), func() {
				RecordTrade(s, Trade{
					Timestamp:       time.UnixMilli(ev.EventMs).UTC(),
					StrategyID:      s.ID,
					Symbol:          coin,
					PositionID:      l.posID,
					Side:            "funding",
					TradeType:       TradeTypeFunding,
					Details:         fmt.Sprintf("Funding payment $%+.4f on %s (quote owner)", amount, coin),
					ExchangeOrderID: ev.DedupID,
					RealizedPnL:     amount,
					PnLGross:        true,
				})
			})
			ids = append(ids, ev.DedupID)
		}
	}
	booked := false
	var details []string
	for _, t := range s.TradeHistory[before:] {
		if t.TradeType != TradeTypeFunding {
			continue
		}
		booked = true
		pay, err := quoteRatFromFloatDecimal(t.RealizedPnL)
		if err != nil {
			continue
		}
		l.funding = quoteRatAdd(l.funding, pay)
		if a.partition.Scope == ScopePaper {
			l.cash = quoteRatAdd(l.cash, pay)
		}
		details = append(details, t.ExchangeOrderID)
	}
	blob, _ := json.Marshal(details)
	if !booked {
		return false, ids, ""
	}
	return true, ids, string(blob)
}

func (a *quoteAdmission) commitStaged(st *quoteStaged, deadlineMs int64, progress quoteProgressRecord) (*quoteCommitResult, error) {
	a.mu.Lock()
	live := a.state.Strategies[a.sc.ID]
	if live == nil || live.Quote == nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("quote owner %s left memory during the settlement", a.sc.ID)
	}
	liveFunding, _ := marshalPaperFundingState(live)
	if live.Quote.Revision != st.BaseRevision || live.Cash != st.BaseCash || len(live.TradeHistory) != st.BaseTrades || liveFunding != st.BaseFunding {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: the in-memory quote book of %s changed while the settlement was staged", errQuoteRevisionStale, a.sc.ID)
	}
	staged := st.Staged
	merged := live.RiskState
	for _, pnl := range st.TradeResults {
		RecordTradeResult(&merged, pnl)
	}
	for _, d := range st.FeeAdjust {
		rolloverDailyPnL(&merged)
		merged.DailyPnL += d
	}
	staged.RiskState = merged
	key, err := a.ownerKey()
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	c := &quoteSettlementCommit{
		Key: key, Scope: a.partition.Scope, ExpectRevision: st.BaseRevision, Kind: st.Kind, DeadlineMs: deadlineMs, BoundaryMs: st.BoundaryMs,
		Book: st.Book, Staged: staged, FillIDs: st.FillIDs, FundingIDs: st.FundingIDs, FundingJSON: st.FundingJSON, OrderProgress: st.OrderProgress,
		Generations: st.Generations, Progress: progress, CommittedAtMs: a.Now().UnixMilli(), AppliedFillKeys: st.FillKeys,
	}
	if st.Kind == quoteSettleNone && progress == nil {
		a.mu.Unlock()
		return &quoteCommitResult{Revision: st.BaseRevision, Summary: quoteCommitSummary{SettlementKind: quoteSettleNone, Revision: st.BaseRevision, AppliedBoundaryMs: st.BoundaryMs, DeadlineMs: deadlineMs}}, nil
	}
	res, err := a.db.commitQuoteSettlement(c)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if st.Kind != quoteSettleNone {
		for _, idx := range res.Flushed {
			staged.TradeHistory[idx].persisted = true
		}
		staged.ClosedPositions = nil
		staged.pendingTradeDiagnostics = nil
		staged.deferTradePersist = live.deferTradePersist
		staged.Quote.Revision = res.Revision
		staged.Quote.Contract.Revision = res.Revision
		staged.Quote.Contract.Book = st.Book
		if st.BoundaryMs > staged.Quote.Contract.AppliedBoundaryMs {
			staged.Quote.Contract.AppliedBoundaryMs = st.BoundaryMs
		}
		for _, g := range st.Generations {
			if cur := staged.Quote.generation(g.Generation); cur != nil {
				cur.Status, cur.TerminalJSON = g.Status, g.TerminalJSON
			}
		}
		applied := map[int64]bool{}
		for _, id := range st.FillIDs {
			applied[id] = true
		}
		var rest []*quoteFillRow
		for _, f := range staged.Quote.Unapplied {
			if !applied[f.ID] {
				rest = append(rest, f)
			}
		}
		staged.Quote.Unapplied = rest
		fundingApplied := map[string]bool{}
		for _, id := range st.FundingIDs {
			fundingApplied[id] = true
		}
		var restFunding []*quoteFundingEvidenceRow
		for _, f := range staged.Quote.Funding {
			if !fundingApplied[f.DedupID] {
				restFunding = append(restFunding, f)
			}
		}
		staged.Quote.Funding = restFunding
		staged.Quote.Settlements++
	} else {
		staged = live
	}
	if deadlineMs > staged.Quote.Contract.LastDeadlineMs {
		staged.Quote.Contract.LastDeadlineMs = deadlineMs
	}
	staged.Quote.SettlementHolds = append([]string(nil), st.Holds...)
	if staged != live {
		*live = *staged
	}
	a.mu.Unlock()
	enqueueFlushedDiagnostics(res.Diags)
	return res, nil
}

func (a *quoteAdmission) reloadOwner() error {
	fresh, _, err := LoadStateWithStore(a.cfg, a.store)
	if err != nil {
		return fmt.Errorf("durable reload of %s failed: %w", a.sc.ID, err)
	}
	fs := fresh.Strategies[a.sc.ID]
	if fs == nil {
		return fmt.Errorf("durable reload found no book for %s", a.sc.ID)
	}
	q, err := a.loadOwnerState()
	if err != nil {
		return err
	}
	fs.Quote = q
	a.mu.Lock()
	if live := a.state.Strategies[a.sc.ID]; live != nil {
		*live = *fs
	} else {
		a.state.Strategies[a.sc.ID] = fs
	}
	a.mu.Unlock()
	return nil
}

type quoteReloadError struct {
	DeadlineMs int64
	Cause      error
}

func (e *quoteReloadError) Error() string {
	return fmt.Sprintf("quote deadline %d needs a resume after a durable reload: %v", e.DeadlineMs, e.Cause)
}

func (e *quoteReloadError) Unwrap() error { return e.Cause }

func isQuoteReloadError(err error) bool {
	var re *quoteReloadError
	return errors.As(err, &re)
}
