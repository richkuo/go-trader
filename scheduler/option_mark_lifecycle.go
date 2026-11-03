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
	optionSettlementBasisIBKR     = "cycle_price_legacy_model"
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

type optionAssignment struct {
	AssignUnderlying string
	AssignOptionType string
	AssignStrike     float64
	AssignSpotPrice  float64
	AssignQuantity   float64
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
	case "ibkr":
		return day, day, true, nil
	}
	return time.Time{}, time.Time{}, false, nil
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
		if st.Basis != optionSettlementBasisIBKR {
			return fmt.Errorf("settlement basis %s is not the ibkr legacy model", st.Basis)
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
		if !r.CutoffAt.IsZero() {
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
			if r.Err == "" && r.Pending == "" && r.Settlement != nil {
				if err := optionSettlementError(platform, pos, r.Settlement); err != nil {
					r.Err = err.Error()
				} else if err := settleExpiredOptionLeg(s, req.Key, pos, req.Multiplier, *r.Settlement, refreshAt, logger); err != nil {
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

func settleExpiredOptionLeg(s *StrategyState, key string, pos *OptionPosition, multiplier float64, st optionLegSettlement, refreshAt time.Time, logger *StrategyLogger) error {
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
	assigned := pos.Action == "sell" && unit > 0

	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	defer func() { s.deferTradePersist = prevDefer }()

	var pnl float64
	if pos.Action == "buy" {
		pnl = intrinsic - pos.EntryPremiumUSD
		pos.CurrentValueUSD = intrinsic
	} else {
		pnl = pos.EntryPremiumUSD - intrinsic
		pos.CurrentValueUSD = -intrinsic
	}
	pos.DTE = 0
	reason := "expired_worthless"
	if assigned {
		reason = "expired_itm"
	}
	label := fmt.Sprintf("%s %s %s $%s", st.Source, st.Basis, st.Date, formatSignalPrice(st.Price))
	recordClosedOptionPosition(s, pos, intrinsic, pnl, reason, refreshAt)
	delete(s.OptionPositions, key)
	assignedStr := ""
	if assigned {
		assignedStr = " [ASSIGNED]"
	}
	logger.Info("Position %s expired, settlement=%s, intrinsic=$%.2f%s", key, label, intrinsic, assignedStr)
	if !assigned {
		logger.Info("Removed expired position %s (%s)", key, reason)
		return nil
	}
	from := len(s.TradeHistory)
	applyAssignment(s, optionAssignment{
		AssignUnderlying: pos.Underlying,
		AssignOptionType: pos.OptionType,
		AssignStrike:     pos.Strike,
		AssignSpotPrice:  st.Price,
		AssignQuantity:   units,
	}, logger)
	for i := from; i < len(s.TradeHistory); i++ {
		if s.TradeHistory[i].TradeType == "assignment" {
			s.TradeHistory[i].Details += " [settlement " + label + "]"
		}
	}
	return nil
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
}

func newOptionPricers() optionPricers {
	return optionPricers{Deribit: NewDeribitPricer(), Robinhood: NewRobinhoodOptionPricer()}
}

func optionPricerFor(platform string, prices map[string]float64, shared optionPricers) OptionPricer {
	switch platform {
	case "robinhood":
		if shared.Robinhood != nil {
			return shared.Robinhood
		}
	case "ibkr":
		return NewIBKRPricer(prices)
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
