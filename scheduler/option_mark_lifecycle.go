package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	optionLegOpMark    = "mark"
	optionLegOpSettle  = "settle"
	optionLegOpResolve = "resolve"

	optionMarkAgeFactor           = 1.5
	optionMarkFailureKeySuffix    = "#option_marks"
	optionSettlementPendingPrefix = "settlement pending: "
	optionCutoffUnresolvedPrefix  = "expiry cutoff unresolved: "

	optionSettlementBasisDelivery = "delivery_price"
	optionSettlementBasisRawClose = "raw_close"
)

type optionLegRequest struct {
	Key             string
	TradePositionID string
	Underlying      string
	OptionType      string
	Strike          float64
	Expiry          string
	Action          string
	Quantity        float64
	Multiplier      float64
	DTE             float64
	Op              string
	KnownCutoff     time.Time
	KnownReady      time.Time
	GoSchedule      bool
	PreError        string
}

type optionLegMark struct {
	PerUnitUSD float64
	Spot       float64
	Greeks     OptGreeks
	Source     string
}

type optionLegSettlement struct {
	Price  float64
	Basis  string
	Date   string
	Source string
}

type optionLegReply struct {
	Key        string
	CutoffAt   time.Time
	ReadyAt    time.Time
	Mark       *optionLegMark
	Settlement *optionLegSettlement
	Pending    string
	Err        string
}

type optionHarvestClock struct {
	Now        time.Time
	MaxMarkAge time.Duration
}

func newOptionHarvestClock(sc StrategyConfig, globalIntervalSeconds int, now time.Time) optionHarvestClock {
	interval := configuredStrategyIntervalSeconds(sc, globalIntervalSeconds)
	if interval < 0 {
		interval = 0
	}
	return optionHarvestClock{Now: now, MaxMarkAge: time.Duration(float64(interval) * optionMarkAgeFactor * float64(time.Second))}
}

func optionHarvestEligibility(pos *OptionPosition, clock optionHarvestClock) (float64, string) {
	if pos.MarkError != "" {
		return 0, "last mark refresh failed: " + pos.MarkError
	}
	if pos.markVerifiedAt.IsZero() {
		return 0, "no successful mark refresh in this process yet (entry value, unknown mark, or restart)"
	}
	if clock.MaxMarkAge <= 0 {
		return 0, "no mark age limit: the strategy has no evaluation interval"
	}
	age := clock.Now.Sub(pos.markVerifiedAt)
	if age < 0 || age > clock.MaxMarkAge {
		return 0, fmt.Sprintf("the last successful mark is older than the %s limit (%.1f x the strategy interval)", clock.MaxMarkAge, optionMarkAgeFactor)
	}
	if pos.ExpiryCutoffAt.IsZero() {
		return 0, "expiry cutoff unknown"
	}
	if !clock.Now.Before(pos.ExpiryCutoffAt) {
		return 0, "at or past the expiry cutoff; the leg waits for settlement"
	}
	return pos.ExpiryCutoffAt.Sub(clock.Now).Hours() / 24, ""
}

func optionGoExpirySchedule(platform, expiry string) (time.Time, time.Time, bool, error) {
	day, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("invalid expiry %q: %w", expiry, err)
	}
	switch platform {
	case "deribit", "okx":
		cutoff := day.Add(8 * time.Hour)
		return cutoff, cutoff.Add(time.Hour), true, nil
	}
	spec, ok := optionContractSpecs[platform]
	if !ok || spec.Cutoff == nil {
		return time.Time{}, time.Time{}, false, nil
	}
	loc, err := time.LoadLocation(spec.Cutoff.Zone)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("%s expiry cutoff zone %q: %w", platform, spec.Cutoff.Zone, err)
	}
	cutoff := time.Date(day.Year(), day.Month(), day.Day(), spec.Cutoff.Hour, spec.Cutoff.Minute, 0, 0, loc).UTC()
	return cutoff, cutoff.Add(spec.Cutoff.ReadDelay), true, nil
}

func optionSettlementNeedsReadBoundary(platform string) bool {
	spec, ok := optionContractSpecs[platform]
	return ok && spec.Cutoff != nil
}

func optionLegAwaitingSettlement(platform string, pos *OptionPosition, now time.Time) bool {
	if cutoff, _, ok, err := optionGoExpirySchedule(platform, pos.Expiry); err == nil && ok {
		return !now.Before(cutoff)
	}
	if !pos.ExpiryCutoffAt.IsZero() {
		return !now.Before(pos.ExpiryCutoffAt)
	}
	day, err := time.Parse("2006-01-02", pos.Expiry)
	return err == nil && !now.Before(day)
}

func collectOptionLegRequests(s *StrategyState, platform string, refreshAt time.Time) []optionLegRequest {
	if s == nil {
		return nil
	}
	var reqs []optionLegRequest
	for _, key := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[key]
		if pos == nil {
			continue
		}
		req := optionLegRequest{
			Key:             key,
			TradePositionID: pos.TradePositionID,
			Underlying:      pos.Underlying,
			OptionType:      pos.OptionType,
			Strike:          pos.Strike,
			Expiry:          pos.Expiry,
			Action:          pos.Action,
			Quantity:        pos.Quantity,
			DTE:             pos.DTE,
		}
		var errs []string
		if err := optionPricingPolicyError(platform, pos.Underlying); err != nil {
			errs = append(errs, err.Error())
		}
		if m, err := strategyOptionMultiplier(s, key, pos); err != nil {
			errs = append(errs, err.Error())
		} else {
			req.Multiplier = m
		}
		if pos.OptionType != "call" && pos.OptionType != "put" {
			errs = append(errs, fmt.Sprintf("option type %q is not call or put", pos.OptionType))
		}
		if pos.Action != "buy" && pos.Action != "sell" {
			errs = append(errs, fmt.Sprintf("action %q is not buy or sell", pos.Action))
		}
		cutoff, ready, resolved, err := optionGoExpirySchedule(platform, pos.Expiry)
		req.GoSchedule = err == nil && resolved
		if err != nil {
			errs = append(errs, err.Error())
		} else if !resolved && !pos.ExpiryCutoffAt.IsZero() {
			cutoff, ready = pos.ExpiryCutoffAt, pos.SettlementReadyAt
			if ready.IsZero() {
				ready = cutoff
			}
		}
		req.KnownCutoff, req.KnownReady = cutoff, ready
		switch {
		case cutoff.IsZero():
			req.Op = optionLegOpResolve
		case !refreshAt.Before(cutoff):
			req.Op = optionLegOpSettle
		default:
			req.Op = optionLegOpMark
		}
		req.PreError = strings.Join(errs, "; ")
		reqs = append(reqs, req)
	}
	return reqs
}

func refreshOptionLegs(pricer OptionPricer, reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	byKey := make(map[string]optionLegReply, len(reqs))
	var live []optionLegRequest
	for _, req := range reqs {
		if req.PreError != "" {
			byKey[req.Key] = optionLegReply{Key: req.Key, Err: req.PreError}
			continue
		}
		live = append(live, req)
	}
	if len(live) > 0 {
		if pricer == nil {
			for _, req := range live {
				byKey[req.Key] = optionLegReply{Key: req.Key, Err: "no option pricer for this platform"}
			}
		} else {
			want := make(map[string]bool, len(live))
			for _, req := range live {
				want[req.Key] = true
			}
			seen := make(map[string]bool, len(live))
			for _, r := range pricer.Refresh(live, refreshAt) {
				if !want[r.Key] {
					continue
				}
				if seen[r.Key] {
					byKey[r.Key] = optionLegReply{Key: r.Key, Err: pricer.Name() + " pricer returned two results for this leg"}
					continue
				}
				seen[r.Key] = true
				byKey[r.Key] = r
			}
			for _, req := range live {
				if !seen[req.Key] {
					byKey[req.Key] = optionLegReply{Key: req.Key, Err: pricer.Name() + " pricer returned no result for this leg"}
				}
			}
		}
	}
	out := make([]optionLegReply, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, byKey[req.Key])
	}
	return out
}

type optionSettlementAlert struct {
	Key             string
	TradePositionID string
	Reason          string
}

type optionLegApplyResult struct {
	Alerts   []optionSettlementAlert
	Failures []string
	Marked   int
	Settled  int
	Waiting  int
	Legs     int
}

func (r optionLegApplyResult) allSucceeded() bool {
	return len(r.Failures) == 0 && r.Waiting == 0 && r.Marked+r.Settled == r.Legs
}

func optionMarkValueError(mk *optionLegMark) error {
	if mk == nil {
		return fmt.Errorf("no mark in the reply")
	}
	if math.IsNaN(mk.PerUnitUSD) || math.IsInf(mk.PerUnitUSD, 0) || mk.PerUnitUSD < 0 {
		return fmt.Errorf("mark value %v is not a finite non-negative amount", mk.PerUnitUSD)
	}
	if !finitePositive(mk.Spot) {
		return fmt.Errorf("mark spot %v is not positive", mk.Spot)
	}
	for _, g := range []float64{mk.Greeks.Delta, mk.Greeks.Gamma, mk.Greeks.Theta, mk.Greeks.Vega} {
		if math.IsNaN(g) || math.IsInf(g, 0) {
			return fmt.Errorf("mark greeks are not finite")
		}
	}
	if strings.TrimSpace(mk.Source) == "" {
		return fmt.Errorf("mark has no source label")
	}
	return nil
}

func optionSettlementError(platform string, pos *OptionPosition, st *optionLegSettlement) error {
	if st == nil {
		return fmt.Errorf("no settlement price in the reply")
	}
	if !finitePositive(st.Price) {
		return fmt.Errorf("settlement price %v is not positive", st.Price)
	}
	if strings.TrimSpace(st.Source) == "" {
		return fmt.Errorf("settlement has no source label")
	}
	day, err := time.Parse("2006-01-02", st.Date)
	if err != nil {
		return fmt.Errorf("settlement date %q is invalid", st.Date)
	}
	expiry, err := time.Parse("2006-01-02", pos.Expiry)
	if err != nil {
		return fmt.Errorf("leg expiry %q is invalid", pos.Expiry)
	}
	switch platform {
	case "deribit", "okx":
		if st.Basis != optionSettlementBasisDelivery || st.Date != pos.Expiry {
			return fmt.Errorf("settlement basis %s on %s does not match the %s delivery price for expiry %s", st.Basis, st.Date, platform, pos.Expiry)
		}
	case "robinhood":
		if st.Basis != optionSettlementBasisRawClose || day.After(expiry) || expiry.Sub(day) > 7*24*time.Hour {
			return fmt.Errorf("settlement basis %s on %s is not a raw close of the expiry session for %s", st.Basis, st.Date, pos.Expiry)
		}
	case "ibkr":
		spec := optionContractSpecs[platform]
		if st.Basis != spec.SettlementBasis || st.Source != spec.SettlementSource || st.Date != pos.Expiry {
			return fmt.Errorf("settlement %s %s on %s is not the %s %s for expiry %s", st.Source, st.Basis, st.Date, spec.SettlementSource, spec.SettlementBasis, pos.Expiry)
		}
	default:
		return fmt.Errorf("no settlement basis for platform %q", platform)
	}
	return nil
}

func applyOptionLegReplies(s *StrategyState, platform string, reqs []optionLegRequest, replies []optionLegReply, refreshAt time.Time, logger *StrategyLogger) optionLegApplyResult {
	var res optionLegApplyResult
	byKey := make(map[string]optionLegReply, len(replies))
	for _, r := range replies {
		byKey[r.Key] = r
	}
	for _, req := range reqs {
		pos := s.OptionPositions[req.Key]
		if pos == nil || pos.TradePositionID != req.TradePositionID {
			continue
		}
		res.Legs++
		r, ok := byKey[req.Key]
		if !ok {
			r = optionLegReply{Key: req.Key, Err: "no result for this leg"}
		}
		cutoff, ready := req.KnownCutoff, req.KnownReady
		if req.GoSchedule {
			if (!r.CutoffAt.IsZero() && !r.CutoffAt.Equal(cutoff)) || (!r.ReadyAt.IsZero() && !r.ReadyAt.Equal(ready)) {
				r = optionLegReply{Key: r.Key, Err: fmt.Sprintf("the pricer changed the expiry schedule (cutoff %s, read boundary %s; expected %s, %s)",
					r.CutoffAt.UTC().Format(time.RFC3339), r.ReadyAt.UTC().Format(time.RFC3339), cutoff.Format(time.RFC3339), ready.Format(time.RFC3339))}
			}
		} else if !r.CutoffAt.IsZero() {
			cutoff, ready = r.CutoffAt, r.ReadyAt
		}
		if !cutoff.IsZero() && (ready.IsZero() || ready.Before(cutoff)) {
			ready = cutoff
		}
		if !cutoff.IsZero() {
			pos.ExpiryCutoffAt = cutoff
			pos.SettlementReadyAt = ready
			pos.DTE = math.Max(0, cutoff.Sub(refreshAt).Hours()/24)
		}
		fail := func(reason string) {
			pos.MarkError = reason
			pos.markVerifiedAt = time.Time{}
			res.Failures = append(res.Failures, req.Key+": "+reason)
			logger.Warn("Option mark %s: %s (value $%.2f kept, DTE=%.2f)", req.Key, reason, pos.CurrentValueUSD, pos.DTE)
		}
		if cutoff.IsZero() {
			reason := optionCutoffUnresolvedPrefix + firstNonEmpty(r.Err, r.Pending, "no cutoff in the reply")
			fail(reason)
			if day, err := time.Parse("2006-01-02", pos.Expiry); err != nil || !refreshAt.Before(day.Add(24*time.Hour)) {
				res.Alerts = append(res.Alerts, optionSettlementAlert{Key: req.Key, TradePositionID: req.TradePositionID, Reason: reason})
			}
			continue
		}
		if !refreshAt.Before(cutoff) {
			if r.Mark != nil && r.Settlement == nil && r.Err == "" && r.Pending == "" {
				r.Err = "the pricer returned a mark for a leg at or past its expiry cutoff"
			}
			if r.Settlement != nil && refreshAt.Before(ready) && optionSettlementNeedsReadBoundary(platform) {
				r = optionLegReply{Key: r.Key, Pending: "the pricer returned a settlement before the read boundary; it is not booked"}
			}
			if r.Err == "" && r.Pending == "" && r.Settlement != nil {
				if err := optionSettlementError(platform, pos, r.Settlement); err != nil {
					r.Err = err.Error()
				} else if err := settleExpiredOptionLeg(s, platform, req.Key, pos, req.Multiplier, *r.Settlement, refreshAt, logger); err != nil {
					r.Err = err.Error()
				} else {
					res.Settled++
					continue
				}
			}
			reason := optionSettlementPendingPrefix + firstNonEmpty(r.Err, r.Pending, "no settlement price in the reply")
			pos.MarkError = reason
			pos.markVerifiedAt = time.Time{}
			pos.DTE = 0
			if refreshAt.Before(ready) {
				res.Waiting++
				logger.InfoOnChange("option-settlement:"+req.Key, reason, "Option %s expired at %s; %s (settlement read boundary %s)", req.Key, cutoff.Format(time.RFC3339), reason, ready.Format(time.RFC3339))
				continue
			}
			res.Failures = append(res.Failures, req.Key+": "+reason)
			res.Alerts = append(res.Alerts, optionSettlementAlert{Key: req.Key, TradePositionID: req.TradePositionID, Reason: reason})
			logger.Error("Option %s held open: %s. No close, assignment or cash change is booked; it retries each refresh", req.Key, reason)
			continue
		}
		if r.Err != "" || r.Pending != "" || r.Mark == nil {
			fail(firstNonEmpty(r.Err, r.Pending, "no mark in the reply"))
			continue
		}
		if r.Settlement != nil {
			fail("the pricer returned a settlement for a leg before its expiry cutoff")
			continue
		}
		if err := optionMarkValueError(r.Mark); err != nil {
			fail(err.Error())
			continue
		}
		v, err := optionContractValueUSD(r.Mark.PerUnitUSD, req.Multiplier, pos.Quantity)
		if err != nil {
			fail(err.Error())
			continue
		}
		if pos.Action == "sell" {
			v = -v
		}
		pos.CurrentValueUSD = v
		pos.Greeks = r.Mark.Greeks
		pos.MarkUpdatedAt = refreshAt
		pos.MarkSource = r.Mark.Source
		pos.MarkError = ""
		pos.markVerifiedAt = refreshAt
		res.Marked++
	}
	return res
}

type optionPhysicalTransfer struct {
	Symbol      string
	Side        string
	Units       float64
	Price       float64
	Value       float64
	Existing    *Position
	NewQuantity float64
	NewAvgCost  float64
	PnL         float64
}

func finiteAmount(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func optionHeldLongCover(pos *Position) bool {
	return pos != nil && pos.Side == "long" && finitePositive(pos.Quantity) && finitePositive(pos.AvgCost)
}

func planOptionPhysicalTransfer(s *StrategyState, key string, pos *OptionPosition, units, settlePx float64) (*optionPhysicalTransfer, []string, error) {
	symbol := strings.ToUpper(pos.Underlying)
	existing := s.Positions[symbol]
	switch pos.OptionType {
	case "put":
		if existing != nil && !optionHeldLongCover(existing) {
			return nil, []string{fmt.Sprintf("PUT ASSIGNMENT NOT DELIVERED: sold put %s expired in the money, but %s already holds a %s position qty=%g avg_cost=%g that is not a usable long; the position is kept, no units are delivered and the option settles in cash at its intrinsic only",
				key, symbol, existing.Side, existing.Quantity, existing.AvgCost)}, nil
		}
		cost := settlePx * units
		if !finitePositive(cost) {
			return nil, nil, fmt.Errorf("put delivery cost %v x %v is not a finite positive amount", settlePx, units)
		}
		t := &optionPhysicalTransfer{Symbol: symbol, Side: "buy", Units: units, Price: settlePx, Value: cost, Existing: existing, NewQuantity: units, NewAvgCost: settlePx}
		if existing != nil {
			t.NewQuantity = existing.Quantity + units
			t.NewAvgCost = (existing.AvgCost*existing.Quantity + cost) / t.NewQuantity
			if !finitePositive(t.NewQuantity) || !finitePositive(t.NewAvgCost) {
				return nil, nil, fmt.Errorf("put delivery into %s overflows (qty %v + %v)", symbol, existing.Quantity, units)
			}
		}
		return t, nil, nil
	case "call":
		var warns []string
		held := 0.0
		if optionHeldLongCover(existing) {
			held = existing.Quantity
		} else if existing != nil && existing.Side == "long" {
			warns = append(warns, fmt.Sprintf("CORRUPT CALL COVER: sold call %s expired in the money and %s long qty=%g avg_cost=%g is corrupt, so it is not cover; the row is kept and the option settles in cash at its intrinsic",
				key, symbol, existing.Quantity, existing.AvgCost))
		}
		covered := math.Min(units, held)
		if uncovered := units - covered; uncovered > 0 {
			warns = append(warns, fmt.Sprintf("UNCOVERED CALL ASSIGNMENT: sold call %s expired in the money (settlement $%s) with %g of %g %s units held; %g uncovered units get no delivery and no strike proceeds, and their intrinsic is in the option settlement",
				key, formatSignalPrice(settlePx), covered, units, symbol, uncovered))
		}
		if covered <= 0 {
			return nil, warns, nil
		}
		proceeds := settlePx * covered
		pnl := (settlePx - existing.AvgCost) * covered
		if !finitePositive(proceeds) || !finiteAmount(pnl) {
			return nil, nil, fmt.Errorf("call delivery of %v %s at %v overflows", covered, symbol, settlePx)
		}
		return &optionPhysicalTransfer{Symbol: symbol, Side: "sell", Units: covered, Price: settlePx, Value: proceeds, Existing: existing,
			NewQuantity: existing.Quantity - covered, NewAvgCost: existing.AvgCost, PnL: pnl}, warns, nil
	}
	return nil, nil, fmt.Errorf("option type %q is not call or put", pos.OptionType)
}

func settleExpiredOptionLeg(s *StrategyState, platform, key string, pos *OptionPosition, multiplier float64, st optionLegSettlement, refreshAt time.Time, logger *StrategyLogger) error {
	style, err := optionSettlementStyle(platform)
	if err != nil {
		return err
	}
	if !finitePositive(st.Price) {
		return fmt.Errorf("settlement price %v is not positive", st.Price)
	}
	if !finitePositive(pos.Strike) {
		return fmt.Errorf("strike %v is not positive", pos.Strike)
	}
	if !finiteAmount(pos.EntryPremiumUSD) {
		return fmt.Errorf("entry premium %v is not finite", pos.EntryPremiumUSD)
	}
	if pos.Action != "buy" && pos.Action != "sell" {
		return fmt.Errorf("action %q is not buy or sell", pos.Action)
	}
	var unit float64
	switch pos.OptionType {
	case "call":
		unit = math.Max(st.Price-pos.Strike, 0)
	case "put":
		unit = math.Max(pos.Strike-st.Price, 0)
	default:
		return fmt.Errorf("option type %q is not call or put", pos.OptionType)
	}
	intrinsic, err := optionContractValueUSD(unit, multiplier, pos.Quantity)
	if err != nil {
		return fmt.Errorf("settlement value: %w", err)
	}
	units, err := optionContractValueUSD(1, multiplier, pos.Quantity)
	if err != nil {
		return fmt.Errorf("settlement units: %w", err)
	}
	cashDelta, pnl := -intrinsic, pos.EntryPremiumUSD-intrinsic
	if pos.Action == "buy" {
		cashDelta, pnl = intrinsic, intrinsic-pos.EntryPremiumUSD
	}
	var transfer *optionPhysicalTransfer
	var warns []string
	if pos.Action == "sell" && unit > 0 && style == optionSettlementPhysical {
		if transfer, warns, err = planOptionPhysicalTransfer(s, key, pos, units, st.Price); err != nil {
			return err
		}
	}
	finalCash := s.Cash + cashDelta
	if transfer != nil {
		if transfer.Side == "buy" {
			finalCash -= transfer.Value
		} else {
			finalCash += transfer.Value
		}
	}
	if !finiteAmount(pnl) || !finiteAmount(finalCash) {
		return fmt.Errorf("settlement cash or PnL overflows (cash %v, intrinsic %v, premium %v)", s.Cash, intrinsic, pos.EntryPremiumUSD)
	}

	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	defer func() { s.deferTradePersist = prevDefer }()

	reason := "expired_worthless"
	if intrinsic > 0 {
		reason = "expired_itm"
	}
	label := fmt.Sprintf("%s %s %s $%s", st.Source, st.Basis, st.Date, formatSignalPrice(st.Price))
	positionID := ensureOptionTradeID(s.ID, pos)
	s.Cash += cashDelta
	pos.CurrentValueUSD = cashDelta
	pos.DTE = 0
	RecordTrade(s, Trade{
		Timestamp:   refreshAt,
		StrategyID:  s.ID,
		Symbol:      pos.ID,
		PositionID:  positionID,
		Side:        optionCloseTradeSide(pos.Action),
		Quantity:    pos.Quantity,
		Price:       intrinsic,
		Value:       intrinsic,
		TradeType:   "options",
		Details:     fmt.Sprintf("Expiry settlement %s %s (%s settlement) intrinsic=$%.2f PnL=$%.2f [settlement %s]", pos.ID, reason, style, intrinsic, pnl, label),
		IsClose:     true,
		RealizedPnL: pnl,
		PnLGross:    true,
		Regime:      s.Regime,
	})
	RecordTradeResult(&s.RiskState, pnl)
	recordClosedOptionPosition(s, pos, intrinsic, pnl, reason, refreshAt)
	delete(s.OptionPositions, key)
	logger.Info("Position %s expired (%s), settlement=%s, %s settlement, intrinsic=$%.2f, PnL=$%.2f", key, reason, label, style, intrinsic, pnl)
	for _, w := range warns {
		logger.Warn("%s", w)
	}
	if transfer != nil {
		bookOptionPhysicalTransfer(s, key, pos, transfer, label, refreshAt, logger)
	}
	return nil
}

func bookOptionPhysicalTransfer(s *StrategyState, key string, pos *OptionPosition, t *optionPhysicalTransfer, label string, at time.Time, logger *StrategyLogger) {
	suffix := " [settlement " + label + "]"
	if t.Side == "buy" {
		s.Cash -= t.Value
		var positionID string
		if t.Existing != nil {
			t.Existing.Quantity = t.NewQuantity
			t.Existing.AvgCost = t.NewAvgCost
			positionID = ensurePositionTradeID(s.ID, t.Symbol, t.Existing)
		} else {
			positionID = newTradePositionID(s.ID, t.Symbol, at)
			s.Positions[t.Symbol] = &Position{
				Symbol:          t.Symbol,
				TradePositionID: positionID,
				Quantity:        t.Units,
				InitialQuantity: t.Units,
				AvgCost:         t.Price,
				Side:            "long",
				OwnerStrategyID: s.ID,
				OpenedAt:        at,
			}
		}
		RecordTrade(s, Trade{
			Timestamp:  at,
			StrategyID: s.ID,
			Symbol:     t.Symbol,
			PositionID: positionID,
			Side:       "buy",
			Quantity:   t.Units,
			Price:      t.Price,
			Value:      t.Value,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel assignment: sold put %s (strike $%.2f) expired in the money, bought %g %s @ settlement $%.2f",
				key, pos.Strike, t.Units, t.Symbol, t.Price) + suffix,
			Regime: s.Regime,
		})
		logger.Info("ASSIGNMENT: sold put %s expired in the money, bought %g %s @ settlement $%.2f (cash debit=$%.2f)", key, t.Units, t.Symbol, t.Price, t.Value)
		return
	}
	existing := t.Existing
	s.Cash += t.Value
	positionID := ensurePositionTradeID(s.ID, t.Symbol, existing)
	entryATR, stopPx := existing.EntryATR, existing.StopLossTriggerPx
	if t.NewQuantity <= 0 {
		recordClosedPosition(s, existing, t.Price, t.PnL, "assignment", at)
		delete(s.Positions, t.Symbol)
	} else {
		existing.Quantity = t.NewQuantity
	}
	RecordTradeResult(&s.RiskState, t.PnL)
	RecordTrade(s, Trade{
		Timestamp:  at,
		StrategyID: s.ID,
		Symbol:     t.Symbol,
		PositionID: positionID,
		Side:       "sell",
		Quantity:   t.Units,
		Price:      t.Price,
		Value:      t.Value,
		TradeType:  "assignment",
		Details: fmt.Sprintf("Wheel call-away: sold call %s (strike $%.2f) expired in the money, sold %g %s @ settlement $%.2f PnL=$%.2f",
			key, pos.Strike, t.Units, t.Symbol, t.Price, t.PnL) + suffix,
		IsClose:           true,
		RealizedPnL:       t.PnL,
		PnLGross:          true,
		Regime:            s.Regime,
		EntryATR:          entryATR,
		StopLossTriggerPx: stopPx,
	})
	logger.Info("CALL-AWAY: sold call %s expired in the money, sold %g %s @ settlement $%.2f (proceeds=$%.2f, PnL=$%.2f)", key, t.Units, t.Symbol, t.Price, t.Value, t.PnL)
}

type optionSettlementAlertThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newOptionSettlementAlertThrottle() *optionSettlementAlertThrottle {
	return &optionSettlementAlertThrottle{last: make(map[string]time.Time)}
}

func optionSettlementAlertKey(part RiskPartition, strategyID, legKey, tradePositionID string) string {
	return partitionLabel(part) + "\x00" + strategyID + "\x00" + legKey + "\x00" + tradePositionID
}

func (t *optionSettlementAlertThrottle) due(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !alertThrottleDue(t.last[key], now) {
		return false
	}
	t.last[key] = now
	return true
}

func (t *optionSettlementAlertThrottle) prune(part RiskPartition, strategyID string, live map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prefix := partitionLabel(part) + "\x00" + strategyID + "\x00"
	for k := range t.last {
		if strings.HasPrefix(k, prefix) && !live[k] {
			delete(t.last, k)
		}
	}
}

func formatOptionSettlementAlert(sc StrategyConfig, a optionSettlementAlert) string {
	return fmt.Sprintf("**CRITICAL: OPTION SETTLEMENT PENDING** [%s] %s leg %s (position %s): %s. The leg stays open with no close, assignment or cash change booked, and it retries every refresh.",
		sc.ID, sc.Platform, a.Key, a.TradePositionID, a.Reason)
}

func formatOptionMarkFailureAlert(sc StrategyConfig, errMsg string, count int, transient bool) string {
	kind := ""
	if transient {
		kind = " (sustained upstream error)"
	}
	return fmt.Sprintf("**OPTION MARKS FAILING%s** [%s] %s (pid=%d, %d consecutive failed refreshes): %s",
		kind, sc.ID, sc.Platform, os.Getpid(), count, errMsg)
}

func notifyOptionMarkFailure(notifier *MultiNotifier, sc StrategyConfig, errMsg string, now time.Time) {
	key := sc.ID + optionMarkFailureKeySuffix
	tracker, threshold, maxDuration := scriptFailureTracker, scriptFailureAlertThreshold, time.Duration(0)
	transient := scriptFailureErrorIsTransient(errMsg)
	if transient {
		tracker, threshold, maxDuration = scriptFailureTransientTracker, scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration
	}
	shouldNotify, count := recordScriptFailureAtThreshold(tracker, key, errMsg, now, threshold, maxDuration)
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatOptionMarkFailureAlert(sc, errMsg, count, transient)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func clearOptionMarkFailure(notifier *MultiNotifier, sc StrategyConfig) {
	key := sc.ID + optionMarkFailureKeySuffix
	recovered, prior := scriptFailureTracker.Clear(key)
	transientRecovered, transientPrior := scriptFailureTransientTracker.Clear(key)
	if !recovered && !transientRecovered {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	if transientPrior > prior {
		prior = transientPrior
	}
	msg := fmt.Sprintf("**OPTION MARKS RECOVERED** [%s] %s (pid=%d): every leg refreshed after %d failed refreshes", sc.ID, sc.Platform, os.Getpid(), prior)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

type optionPricers struct {
	Deribit   *DeribitPricer
	Robinhood *RobinhoodOptionPricer
	IBKR      *IBKROptionPricer
}

func newOptionPricers() optionPricers {
	return optionPricers{Deribit: NewDeribitPricer(), Robinhood: NewRobinhoodOptionPricer(), IBKR: NewIBKROptionPricer()}
}

func optionPricerFor(platform string, shared optionPricers) OptionPricer {
	switch platform {
	case "robinhood":
		if shared.Robinhood != nil {
			return shared.Robinhood
		}
	case "ibkr":
		if shared.IBKR != nil {
			return shared.IBKR
		}
	case "deribit":
		if shared.Deribit != nil {
			return shared.Deribit
		}
	case "okx":
		if shared.Deribit != nil {
			return shared.Deribit.OKXProxy()
		}
	}
	return nil
}

type optionMarkRefresh struct {
	SC        StrategyConfig
	State     *StrategyState
	Pricer    OptionPricer
	Mu        *sync.RWMutex
	Notifier  *MultiNotifier
	Throttle  *optionSettlementAlertThrottle
	Logger    *StrategyLogger
	RefreshAt time.Time
}

func runOptionMarkRefresh(d optionMarkRefresh) optionLegApplyResult {
	part := partitionFor(d.SC)
	d.Mu.RLock()
	reqs := collectOptionLegRequests(d.State, d.SC.Platform, d.RefreshAt)
	d.Mu.RUnlock()
	if len(reqs) == 0 {
		if d.Throttle != nil {
			d.Throttle.prune(part, d.SC.ID, nil)
		}
		if d.SC.Type == "options" {
			clearOptionMarkFailure(d.Notifier, d.SC)
		}
		return optionLegApplyResult{}
	}
	replies := refreshOptionLegs(d.Pricer, reqs, d.RefreshAt)
	d.Mu.Lock()
	res := applyOptionLegReplies(d.State, d.SC.Platform, reqs, replies, d.RefreshAt, d.Logger)
	live := make(map[string]bool, len(d.State.OptionPositions))
	for _, key := range sortedOptionLegKeys(d.State.OptionPositions) {
		if pos := d.State.OptionPositions[key]; pos != nil {
			live[optionSettlementAlertKey(part, d.SC.ID, key, pos.TradePositionID)] = true
		}
	}
	d.Mu.Unlock()

	if len(res.Failures) > 0 {
		failures := append([]string(nil), res.Failures...)
		sort.Strings(failures)
		notifyOptionMarkFailure(d.Notifier, d.SC, fmt.Sprintf("%d of %d option legs failed: %s", len(failures), res.Legs, strings.Join(failures, "; ")), d.RefreshAt)
	} else if res.allSucceeded() {
		clearOptionMarkFailure(d.Notifier, d.SC)
	}
	if d.Throttle != nil {
		for _, a := range res.Alerts {
			if !d.Throttle.due(optionSettlementAlertKey(part, d.SC.ID, a.Key, a.TradePositionID), d.RefreshAt) {
				continue
			}
			if d.Notifier == nil || !d.Notifier.HasBackends() {
				continue
			}
			msg := formatOptionSettlementAlert(d.SC, a)
			d.Notifier.SendOwnerDM(msg)
			d.Notifier.SendToPartitionChannels(part, msg)
		}
		d.Throttle.prune(part, d.SC.ID, live)
	}
	return res
}

func optionMarkAgeText(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h >= 48:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func optionMarkStatusText(opt *OptionPosition, now time.Time) string {
	mark := "mark unknown"
	if !opt.MarkUpdatedAt.IsZero() {
		age := now.Sub(opt.MarkUpdatedAt)
		if age < 0 {
			age = 0
		}
		mark = fmt.Sprintf("mark %s ago (%s)", optionMarkAgeText(age), firstNonEmpty(opt.MarkSource, "unknown source"))
	}
	if opt.MarkError != "" {
		mark += " | " + opt.MarkError
	}
	return mark
}
