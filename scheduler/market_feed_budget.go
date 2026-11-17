package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	feedBudgetWindow         = 60 * time.Second
	feedWeightWaitTick       = 250 * time.Millisecond
	feedWeightParticipating  = "enabled"
	feedWeightNotEnrolled    = "disabled"
	feedWeightCauseExhausted = "weight_exhausted"
	feedWeightCauseUnfit     = "allocation_unfit"
	feedWeightCauseUnpriced  = "unpriced_request"
	feedWeightCauseBound     = "bound_blocked"
	feedWeightEventStart     = "weight_refused"
	feedWeightEventRecover   = "weight_recovered"
	feedWeightEventViolation = "bound_violation"
)

var errFeedBudgetExhausted = errors.New("request budget exhausted")

var errFeedWeightRefused = errors.New("request weight refused")

type feedBudgetConfig struct {
	PerMinute       int  `json:"per_minute"`
	Startup         int  `json:"startup"`
	WeightPerMinute *int `json:"weight_per_minute,omitempty"`
	WeightStartup   *int `json:"weight_startup,omitempty"`
	WeightReserve   *int `json:"weight_reserve,omitempty"`
}

type feedWeightBudgetConfig struct {
	WeightPerMinute *int `json:"weight_per_minute"`
	WeightStartup   *int `json:"weight_startup"`
	WeightReserve   *int `json:"weight_reserve"`
}

type feedWeightPolicy struct {
	PerMinute int
	Startup   int
	Reserve   int
}

func (c *feedBudgetConfig) weightPolicy() *feedWeightPolicy {
	if c == nil || c.WeightPerMinute == nil || c.WeightStartup == nil || c.WeightReserve == nil {
		return nil
	}
	return &feedWeightPolicy{PerMinute: *c.WeightPerMinute, Startup: *c.WeightStartup, Reserve: *c.WeightReserve}
}

func (c *feedWeightBudgetConfig) budget() *feedBudgetConfig {
	if c == nil {
		return nil
	}
	return &feedBudgetConfig{WeightPerMinute: c.WeightPerMinute, WeightStartup: c.WeightStartup, WeightReserve: c.WeightReserve}
}

func feedIntPtr(v int) *int { return &v }

type feedLedgerCtxKey struct{}
type feedReasonCtxKey struct{}
type feedKeepCtxKey struct{}
type feedKeepWeightCtxKey struct{}
type feedInputCtxKey struct{}

type feedWeightCharge struct {
	at      time.Time
	weight  int
	startup int
	debt    bool
	host    string
}

type feedWeightBound struct {
	atCap    bool
	blocked  bool
	epoch    int
	observed int
	bound    int
	at       time.Time
	alerted  bool
}

type feedWeightEpisode struct {
	open         bool
	openedAt     time.Time
	inputs       map[string]bool
	refused      int
	startSent    bool
	successAfter bool
}

type feedRequestLedger struct {
	mu             sync.Mutex
	clock          func() time.Time
	logf           func(string, ...any)
	enforce        bool
	perMinute      int
	startup        int
	startupLeft    int
	startupOpened  bool
	window         []time.Time
	totals         feedBudgetTotals
	hosts          map[string]*feedHostPause
	closeOnSuccess bool

	weight       *feedWeightPolicy
	wStartupLeft int
	wPending     int
	wInFlight    int
	wOpenSteady  int
	wCharges     []feedWeightCharge
	wTotals      feedWeightTotals
	wBounds      map[string]*feedWeightBound
	wLastRefusal string
	wEpisode     feedWeightEpisode
	wChanged     chan struct{}

	authority         *feedWeightAuthority
	coverage          string
	seededStartup     bool
	seededStartupLeft int

	demand  []feedDemandMark
	wDemand []feedDemandMark
}

type feedDemandMark struct {
	at    time.Time
	value int
}

func pushFeedDemand(marks []feedDemandMark, now time.Time, value int) []feedDemandMark {
	for len(marks) > 0 && marks[len(marks)-1].value <= value {
		marks = marks[:len(marks)-1]
	}
	return append(marks, feedDemandMark{at: now, value: value})
}

func pruneFeedDemand(marks []feedDemandMark, now time.Time) []feedDemandMark {
	cut := 0
	for cut < len(marks) && now.Sub(marks[cut].at) >= feedBudgetWindow {
		cut++
	}
	if cut == 0 {
		return marks
	}
	return append([]feedDemandMark(nil), marks[cut:]...)
}

func feedDemandPeak(marks []feedDemandMark) int {
	if len(marks) == 0 {
		return 0
	}
	return marks[0].value
}

type feedBudgetTotals struct {
	Total           int            `json:"total"`
	Refused         int            `json:"refused"`
	ByReason        map[string]int `json:"by_reason"`
	ByType          map[string]int `json:"by_type"`
	RefusedByReason map[string]int `json:"refused_by_reason"`
	RateLimited     int            `json:"rate_limited"`
	Paused          int            `json:"paused"`
	PausedByReason  map[string]int `json:"paused_by_reason"`
}

type feedWeightTotals struct {
	Reserved         int            `json:"reserved"`
	Settled          int            `json:"settled"`
	Retained         int            `json:"retained"`
	Refunded         int            `json:"refunded"`
	Released         int            `json:"released"`
	Debt             int            `json:"debt"`
	Refused          int            `json:"refused"`
	Unpriced         int            `json:"unpriced"`
	ReservedByReason map[string]int `json:"reserved_by_reason"`
	ChargedByType    map[string]int `json:"charged_by_type"`
	RefusedByCause   map[string]int `json:"refused_by_cause"`
	RefusedByReason  map[string]int `json:"refused_by_reason"`
	RefusedByType    map[string]int `json:"refused_by_type"`
}

type feedWeightViolation struct {
	Type     string `json:"type"`
	Bound    int    `json:"bound"`
	Observed int    `json:"observed"`
	State    string `json:"state"`
	At       string `json:"at"`
}

type feedWeightEpisodeStatus struct {
	Open     bool     `json:"open"`
	OpenedAt string   `json:"opened_at,omitempty"`
	Refused  int      `json:"refused"`
	Inputs   []string `json:"inputs"`
}

type feedWeightStatus struct {
	Participation string                  `json:"participation"`
	Ceiling       int                     `json:"ceiling"`
	PerMinute     int                     `json:"weight_per_minute"`
	Startup       int                     `json:"weight_startup"`
	StartupLeft   int                     `json:"weight_startup_left"`
	Reserve       int                     `json:"weight_reserve"`
	Allocation    int                     `json:"allocation"`
	DemandPeak    int                     `json:"demand_peak"`
	Pending       int                     `json:"pending"`
	InFlight      int                     `json:"in_flight"`
	Charged       int                     `json:"charged"`
	SteadyUsed    int                     `json:"steady_used"`
	Totals        feedWeightTotals        `json:"totals"`
	LastRefusal   string                  `json:"last_refusal,omitempty"`
	Violations    []feedWeightViolation   `json:"violations"`
	Episode       feedWeightEpisodeStatus `json:"episode"`
	Shared        feedSharedStatus        `json:"shared"`
}

type feedBudgetStatus struct {
	Enforced    bool                      `json:"enforced"`
	PerMinute   int                       `json:"per_minute"`
	Startup     int                       `json:"startup"`
	StartupLeft int                       `json:"startup_left"`
	WindowUsed  int                       `json:"window_used"`
	DemandPeak  int                       `json:"demand_peak"`
	Totals      feedBudgetTotals          `json:"totals"`
	RateLimit   []feedRateLimitHostStatus `json:"rate_limit"`
	Weight      feedWeightStatus          `json:"weight"`
}

type feedWeightRefusal struct {
	Cause       string
	Reason      string
	ReqType     string
	Input       string
	Cost        int
	SteadyUsed  int
	PerMinute   int
	Keep        int
	StartupLeft int
	Detail      string
}

func (e *feedWeightRefusal) Error() string {
	switch e.Cause {
	case feedWeightCauseSharedExhausted, feedWeightCauseSharedHold, feedWeightCauseSharedStorage,
		feedWeightCauseSharedClock, feedWeightCauseSharedVersion, feedWeightCauseSharedOwner:
		return fmt.Sprintf("%v (%s): %s (reason %s, %s, input %s)", errFeedWeightRefused, e.Cause, e.Detail, e.Reason, e.ReqType, e.Input)
	case feedWeightCauseUnpriced, feedWeightCauseBound:
		return fmt.Sprintf("%v (%s): %s (reason %s, %s, input %s)", errFeedWeightRefused, e.Cause, e.Detail, e.Reason, e.ReqType, e.Input)
	}
	return fmt.Sprintf("%v (%s): cost %d with %d kept for later reads does not fit %d of %d weight used in the last %s, startup left %d (reason %s, %s, input %s)",
		errFeedWeightRefused, e.Cause, e.Cost, e.Keep, e.SteadyUsed, e.PerMinute, feedBudgetWindow, e.StartupLeft, e.Reason, e.ReqType, e.Input)
}

func (e *feedWeightRefusal) Is(target error) bool {
	return target == errFeedWeightRefused || target == errFeedBudgetExhausted
}

func feedWeightRefusalCause(err error) string {
	var r *feedWeightRefusal
	if errors.As(err, &r) {
		return r.Cause
	}
	return ""
}

func newFeedBudgetTotals() feedBudgetTotals {
	return feedBudgetTotals{
		ByReason:        map[string]int{},
		ByType:          map[string]int{},
		RefusedByReason: map[string]int{},
		PausedByReason:  map[string]int{},
	}
}

func newFeedWeightTotals() feedWeightTotals {
	return feedWeightTotals{
		ReservedByReason: map[string]int{},
		ChargedByType:    map[string]int{},
		RefusedByCause:   map[string]int{},
		RefusedByReason:  map[string]int{},
		RefusedByType:    map[string]int{},
	}
}

func newFeedRequestLedger(cfg *feedBudgetConfig, enforce bool, clock func() time.Time) *feedRequestLedger {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	l := &feedRequestLedger{
		clock:    clock,
		logf:     func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
		enforce:  enforce,
		totals:   newFeedBudgetTotals(),
		hosts:    make(map[string]*feedHostPause),
		wTotals:  newFeedWeightTotals(),
		wBounds:  make(map[string]*feedWeightBound),
		wChanged: make(chan struct{}),
	}
	l.setLimits(cfg)
	return l
}

func (l *feedRequestLedger) signalLocked() {
	close(l.wChanged)
	l.wChanged = make(chan struct{})
}

func (l *feedRequestLedger) setLimits(cfg *feedBudgetConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cfg == nil {
		l.perMinute, l.startup = 0, 0
	} else {
		l.perMinute, l.startup = cfg.PerMinute, cfg.Startup
	}
	if l.startupLeft > l.startup {
		l.startupLeft = l.startup
	}
	l.weight = cfg.weightPolicy()
	if l.weight == nil {
		l.wStartupLeft = 0
	} else if l.wStartupLeft > l.weight.Startup {
		l.wStartupLeft = l.weight.Startup
	}
	l.signalLocked()
}

func (l *feedRequestLedger) openStartup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startupOpened {
		return
	}
	l.startupOpened = true
	l.startupLeft = l.startup
	if l.weight != nil {
		l.wStartupLeft = l.weight.Startup
		if l.seededStartup && l.seededStartupLeft < l.wStartupLeft {
			l.wStartupLeft = l.seededStartupLeft
		}
	}
	l.signalLocked()
}

func (l *feedRequestLedger) pruneLocked(now time.Time) {
	cut := 0
	for cut < len(l.window) && now.Sub(l.window[cut]) >= feedBudgetWindow {
		cut++
	}
	if cut > 0 {
		l.window = append([]time.Time(nil), l.window[cut:]...)
	}
	kept := l.wCharges[:0]
	for _, c := range l.wCharges {
		if now.Sub(c.at) < feedBudgetWindow {
			kept = append(kept, c)
		}
	}
	l.wCharges = kept
	l.demand = pruneFeedDemand(l.demand, now)
	l.wDemand = pruneFeedDemand(l.wDemand, now)
}

func (l *feedRequestLedger) steadyUsedLocked() int {
	used := l.wOpenSteady
	for _, c := range l.wCharges {
		used += c.weight - c.startup
	}
	return used
}

func (l *feedRequestLedger) chargedLocked() int {
	total := 0
	for _, c := range l.wCharges {
		total += c.weight
	}
	return total
}

type feedAcquireSpec struct {
	host       string
	reason     string
	reqType    string
	input      string
	keep       int
	keepWeight int
	rows       int
}

func (l *feedRequestLedger) acquire(host, reason, reqType, input string, keep int) (*feedRequestPermit, error) {
	return l.acquireSpec(feedAcquireSpec{host: host, reason: reason, reqType: reqType, input: input, keep: keep, rows: -1}, false)
}

func (l *feedRequestLedger) priceLocked(reqType string, rows int) (cost, epoch int, cause, detail string) {
	b := l.wBounds[reqType]
	if b != nil {
		epoch = b.epoch
		if b.blocked {
			return 0, epoch, feedWeightCauseBound, fmt.Sprintf("a %s response returned %d rows, above the %d-row cap, so this read has no finite cost bound until restart", reqType, b.observed, hlInfoRowCap(reqType))
		}
		if b.atCap {
			rows = -1
		}
	}
	var err error
	if rows < 0 {
		cost, err = hlInfoWeightAtCap(reqType)
	} else {
		cost, err = hlInfoWeight(reqType, rows)
	}
	if err != nil {
		return 0, epoch, feedWeightCauseUnpriced, err.Error()
	}
	return cost, epoch, "", ""
}

func feedWeightRequiredReason(reason string) bool {
	switch feedRestReason(reason) {
	case feedRestCorrection, feedRestCorrectionRetry, feedRestLotMeta:
		return false
	}
	return reason != "other"
}

func (l *feedRequestLedger) countWeightRefusalLocked(spec feedAcquireSpec, cause string, now time.Time) {
	l.wTotals.Refused++
	l.wTotals.RefusedByCause[cause]++
	l.wTotals.RefusedByReason[spec.reason]++
	l.wTotals.RefusedByType[spec.reqType]++
	l.wLastRefusal = cause
	if !feedWeightRequiredReason(spec.reason) || feedSharedCause(cause) {
		return
	}
	ep := &l.wEpisode
	if !ep.open {
		ep.open = true
		ep.openedAt = now
		ep.inputs = map[string]bool{}
		ep.refused = 0
		ep.startSent = false
	}
	ep.successAfter = false
	ep.refused++
	if spec.input != "" {
		ep.inputs[spec.input] = true
	}
}

func (l *feedRequestLedger) countWeightRefusal(spec feedAcquireSpec, cause string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.countWeightRefusalLocked(spec, cause, l.clock().UTC())
}

func (l *feedRequestLedger) weightDecisionLocked(spec feedAcquireSpec, cost int, now time.Time) (startupPart int, refusal *feedWeightRefusal) {
	if l.weight == nil {
		return 0, nil
	}
	bootstrap := spec.reason == string(feedRestBootstrap)
	if bootstrap {
		startupPart = cost
		if startupPart > l.wStartupLeft {
			startupPart = l.wStartupLeft
		}
	}
	steady := cost - startupPart
	used := l.steadyUsedLocked()
	l.wDemand = pushFeedDemand(l.wDemand, now, used+steady+spec.keepWeight)
	if used+steady+spec.keepWeight <= l.weight.PerMinute {
		return startupPart, nil
	}
	cause := feedWeightCauseExhausted
	ceiling := l.weight.PerMinute
	if bootstrap {
		ceiling += l.wStartupLeft
	}
	if cost+spec.keepWeight > ceiling {
		cause = feedWeightCauseUnfit
	}
	return 0, &feedWeightRefusal{Cause: cause, Reason: spec.reason, ReqType: spec.reqType, Input: spec.input, Cost: cost,
		SteadyUsed: used, PerMinute: l.weight.PerMinute, Keep: spec.keepWeight, StartupLeft: l.wStartupLeft}
}

func (l *feedRequestLedger) acquireSpec(spec feedAcquireSpec, silent bool) (*feedRequestPermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UTC()
	if err := l.pauseRefusalLocked(spec.host, spec.reason, spec.input, now); err != nil {
		return nil, err
	}
	l.pruneLocked(now)
	cost, epoch, priceCause, priceDetail := l.priceLocked(spec.reqType, spec.rows)
	useStartup := spec.reason == string(feedRestBootstrap) && l.startupLeft > 0
	if !useStartup {
		l.demand = pushFeedDemand(l.demand, now, len(l.window)+spec.keep+1)
	}
	if !useStartup && l.enforce && len(l.window)+spec.keep >= l.perMinute {
		l.totals.Refused++
		l.totals.RefusedByReason[spec.reason]++
		return nil, fmt.Errorf("%w: %d of %d requests used in the last %s (reason %s, %s)",
			errFeedBudgetExhausted, len(l.window), l.perMinute, feedBudgetWindow, spec.reason, spec.reqType)
	}
	var startupPart int
	if priceCause != "" {
		if l.weight != nil {
			refusal := &feedWeightRefusal{Cause: priceCause, Reason: spec.reason, ReqType: spec.reqType, Input: spec.input, Detail: priceDetail}
			if !silent {
				l.countWeightRefusalLocked(spec, priceCause, now)
			}
			return nil, refusal
		}
		if priceCause == feedWeightCauseUnpriced {
			l.wTotals.Unpriced++
		}
		cost = 0
	} else {
		var refusal *feedWeightRefusal
		startupPart, refusal = l.weightDecisionLocked(spec, cost, now)
		if refusal != nil {
			if !silent {
				l.countWeightRefusalLocked(spec, refusal.Cause, now)
			}
			return nil, refusal
		}
	}
	if useStartup {
		l.startupLeft--
	} else {
		l.window = append(l.window, now)
	}
	durable := l.authority != nil && l.weight != nil
	l.recordLocked(spec.reason, spec.reqType)
	l.wStartupLeft -= startupPart
	l.wPending += cost
	l.wOpenSteady += cost - startupPart
	l.wTotals.Reserved += cost
	l.wTotals.ReservedByReason[spec.reason] += cost
	atCap := false
	if b := l.wBounds[spec.reqType]; b != nil && b.atCap && !b.blocked {
		atCap = true
	}
	return &feedRequestPermit{l: l, host: spec.host, reason: spec.reason, reqType: spec.reqType, input: spec.input,
		cost: cost, startupPart: startupPart, epoch: epoch, wState: feedPermitPending, atCap: atCap,
		durable: durable, usedReqStartup: useStartup, reqAt: now}, nil
}

func (l *feedRequestLedger) unreserve(p *feedRequestPermit, spec feedAcquireSpec, refusal error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.wState != feedPermitPending {
		return
	}
	if p.usedReqStartup {
		l.startupLeft++
	} else {
		for i := len(l.window) - 1; i >= 0; i-- {
			if l.window[i].Equal(p.reqAt) {
				l.window = append(l.window[:i], l.window[i+1:]...)
				break
			}
		}
	}
	l.totals.Total--
	l.totals.ByReason[p.reason]--
	if l.totals.ByReason[p.reason] <= 0 {
		delete(l.totals.ByReason, p.reason)
	}
	l.totals.ByType[p.reqType]--
	if l.totals.ByType[p.reqType] <= 0 {
		delete(l.totals.ByType, p.reqType)
	}
	l.wPending -= p.cost
	l.wOpenSteady -= p.cost - p.startupPart
	l.restoreStartupLocked(p.startupPart)
	l.wTotals.Reserved -= p.cost
	l.wTotals.ReservedByReason[p.reason] -= p.cost
	if l.wTotals.ReservedByReason[p.reason] <= 0 {
		delete(l.wTotals.ReservedByReason, p.reason)
	}
	p.wState = feedPermitClosed
	p.done = true
	var paused *feedHostPausedError
	if errors.As(refusal, &paused) {
		l.totals.Paused++
		l.totals.PausedByReason[spec.reason]++
		if h := l.hosts[spec.host]; h != nil {
			h.refused++
			if spec.input != "" {
				h.refusedInputs[spec.input] = true
			}
		}
	} else if cause := feedWeightRefusalCause(refusal); cause != "" {
		l.countWeightRefusalLocked(spec, cause, l.clock().UTC())
	}
	l.signalLocked()
}

func (l *feedRequestLedger) sharedAuthority() *feedWeightAuthority {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authority
}

func (l *feedRequestLedger) recordLocked(reason, reqType string) {
	l.totals.Total++
	l.totals.ByReason[reason]++
	l.totals.ByType[reqType]++
}

func (l *feedRequestLedger) snapshot() feedBudgetTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copyFeedBudgetTotals(l.totals)
}

func (l *feedRequestLedger) weightSnapshot() feedWeightTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copyFeedWeightTotals(l.wTotals)
}

func (l *feedRequestLedger) status() feedBudgetStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UTC()
	l.pruneLocked(now)
	return feedBudgetStatus{
		Enforced:    l.enforce,
		PerMinute:   l.perMinute,
		Startup:     l.startup,
		StartupLeft: l.startupLeft,
		WindowUsed:  len(l.window),
		DemandPeak:  feedDemandPeak(l.demand),
		Totals:      copyFeedBudgetTotals(l.totals),
		RateLimit:   l.rateLimitStatusLocked(now),
		Weight:      l.weightStatusLocked(),
	}
}

func (l *feedRequestLedger) weightStatus() feedWeightStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.clock().UTC())
	return l.weightStatusLocked()
}

func (l *feedRequestLedger) weightStatusLocked() feedWeightStatus {
	st := feedWeightStatus{
		Participation: feedWeightNotEnrolled,
		Ceiling:       hlRESTWeightCeiling,
		StartupLeft:   l.wStartupLeft,
		Pending:       l.wPending,
		InFlight:      l.wInFlight,
		Charged:       l.chargedLocked(),
		SteadyUsed:    l.steadyUsedLocked(),
		Totals:        copyFeedWeightTotals(l.wTotals),
		LastRefusal:   l.wLastRefusal,
		Violations:    []feedWeightViolation{},
		Episode:       feedWeightEpisodeStatus{Open: l.wEpisode.open, Refused: l.wEpisode.refused, Inputs: sortedFeedSet(l.wEpisode.inputs)},
	}
	if l.wEpisode.open {
		st.Episode.OpenedAt = l.wEpisode.openedAt.Format(time.RFC3339)
	}
	if l.weight != nil {
		st.Participation = feedWeightParticipating
		st.PerMinute, st.Startup, st.Reserve = l.weight.PerMinute, l.weight.Startup, l.weight.Reserve
		st.Allocation = l.weight.PerMinute + l.weight.Startup
		st.DemandPeak = feedDemandPeak(l.wDemand)
	}
	switch {
	case l.authority != nil:
		st.Shared = l.authority.statusView()
		if l.weight == nil {
			st.Shared.Coverage = feedSharedCoverageNoPolicy
		}
	case l.coverage != "":
		st.Shared = feedSharedStatus{Coverage: l.coverage, Hosts: []feedSharedHostStatus{}, Owners: []feedSharedOwnerStatus{}}
	default:
		st.Shared = feedSharedStatus{Coverage: feedSharedCoverageNoPolicy, Hosts: []feedSharedHostStatus{}, Owners: []feedSharedOwnerStatus{}}
	}
	types := make([]string, 0, len(l.wBounds))
	for t := range l.wBounds {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		b := l.wBounds[t]
		state := "repriced_at_cap"
		if b.blocked {
			state = "blocked"
		}
		st.Violations = append(st.Violations, feedWeightViolation{Type: t, Bound: b.bound, Observed: b.observed, State: state, At: b.at.Format(time.RFC3339)})
	}
	return st
}

func copyFeedWeightTotals(t feedWeightTotals) feedWeightTotals {
	out := newFeedWeightTotals()
	out.Reserved, out.Settled, out.Retained, out.Refunded = t.Reserved, t.Settled, t.Retained, t.Refunded
	out.Released, out.Debt, out.Refused, out.Unpriced = t.Released, t.Debt, t.Refused, t.Unpriced
	for src, dst := range map[*map[string]int]*map[string]int{
		&t.ReservedByReason: &out.ReservedByReason,
		&t.ChargedByType:    &out.ChargedByType,
		&t.RefusedByCause:   &out.RefusedByCause,
		&t.RefusedByReason:  &out.RefusedByReason,
		&t.RefusedByType:    &out.RefusedByType,
	} {
		for k, v := range *src {
			(*dst)[k] = v
		}
	}
	return out
}

func (t feedWeightTotals) since(prev feedWeightTotals) feedWeightTotals {
	out := newFeedWeightTotals()
	out.Reserved = t.Reserved - prev.Reserved
	out.Settled = t.Settled - prev.Settled
	out.Retained = t.Retained - prev.Retained
	out.Refunded = t.Refunded - prev.Refunded
	out.Released = t.Released - prev.Released
	out.Debt = t.Debt - prev.Debt
	out.Refused = t.Refused - prev.Refused
	out.Unpriced = t.Unpriced - prev.Unpriced
	for k, v := range t.RefusedByCause {
		if d := v - prev.RefusedByCause[k]; d != 0 {
			out.RefusedByCause[k] = d
		}
	}
	for k, v := range t.ChargedByType {
		if d := v - prev.ChargedByType[k]; d != 0 {
			out.ChargedByType[k] = d
		}
	}
	return out
}

func copyFeedBudgetTotals(t feedBudgetTotals) feedBudgetTotals {
	out := newFeedBudgetTotals()
	out.Total, out.Refused = t.Total, t.Refused
	out.RateLimited, out.Paused = t.RateLimited, t.Paused
	for k, v := range t.PausedByReason {
		out.PausedByReason[k] = v
	}
	for k, v := range t.ByReason {
		out.ByReason[k] = v
	}
	for k, v := range t.ByType {
		out.ByType[k] = v
	}
	for k, v := range t.RefusedByReason {
		out.RefusedByReason[k] = v
	}
	return out
}

func (t feedBudgetTotals) since(prev feedBudgetTotals) feedBudgetTotals {
	out := newFeedBudgetTotals()
	out.Total = t.Total - prev.Total
	out.Refused = t.Refused - prev.Refused
	out.RateLimited = t.RateLimited - prev.RateLimited
	out.Paused = t.Paused - prev.Paused
	for k, v := range t.PausedByReason {
		if d := v - prev.PausedByReason[k]; d != 0 {
			out.PausedByReason[k] = d
		}
	}
	for k, v := range t.ByReason {
		if d := v - prev.ByReason[k]; d != 0 {
			out.ByReason[k] = d
		}
	}
	for k, v := range t.ByType {
		if d := v - prev.ByType[k]; d != 0 {
			out.ByType[k] = d
		}
	}
	for k, v := range t.RefusedByReason {
		if d := v - prev.RefusedByReason[k]; d != 0 {
			out.RefusedByReason[k] = d
		}
	}
	return out
}

func formatFeedCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}
	return strings.Join(parts, ",")
}

func withFeedLedger(ctx context.Context, l *feedRequestLedger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, feedLedgerCtxKey{}, l)
}

func withFeedReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, feedReasonCtxKey{}, reason)
}

func withFeedKeep(ctx context.Context, keep int) context.Context {
	return context.WithValue(ctx, feedKeepCtxKey{}, keep)
}

func withFeedKeepWeight(ctx context.Context, weight int) context.Context {
	return context.WithValue(ctx, feedKeepWeightCtxKey{}, weight)
}

func withFeedInput(ctx context.Context, input string) context.Context {
	return context.WithValue(ctx, feedInputCtxKey{}, input)
}

func feedLedgerFrom(ctx context.Context) *feedRequestLedger {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(feedLedgerCtxKey{}).(*feedRequestLedger)
	return l
}

func feedAcquireSpecFrom(ctx context.Context, baseURL, reqType string, rows int) feedAcquireSpec {
	reason, _ := ctx.Value(feedReasonCtxKey{}).(string)
	if reason == "" {
		reason = "other"
	}
	input, _ := ctx.Value(feedInputCtxKey{}).(string)
	if input == "" {
		input = reason
	}
	keep, _ := ctx.Value(feedKeepCtxKey{}).(int)
	keepWeight, _ := ctx.Value(feedKeepWeightCtxKey{}).(int)
	return feedAcquireSpec{host: feedHostOf(baseURL), reason: reason, reqType: reqType, input: input, keep: keep, keepWeight: keepWeight, rows: rows}
}

func feedBudgetAcquire(ctx context.Context, baseURL, reqType string) (*feedRequestPermit, error) {
	return feedBudgetAcquireBounded(ctx, baseURL, reqType, -1)
}

func feedBudgetAcquireBounded(ctx context.Context, baseURL, reqType string, rows int) (*feedRequestPermit, error) {
	l := feedLedgerFrom(ctx)
	if l == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	spec := feedAcquireSpecFrom(ctx, baseURL, reqType, rows)
	if spec.reason != string(feedRestBootstrap) {
		return l.acquireShared(ctx, spec, false)
	}
	for {
		changed := l.changedCh()
		permit, err := l.acquireShared(ctx, spec, true)
		cause := feedWeightRefusalCause(err)
		waitable := cause == feedWeightCauseExhausted || cause == feedWeightCauseSharedExhausted || cause == feedWeightCauseSharedHold
		if err == nil || !waitable {
			if cause != "" && !feedSharedCause(cause) {
				l.countWeightRefusal(spec, cause)
			}
			return permit, err
		}
		tick := feedWeightWaitTick
		wake := changed
		if feedSharedCause(cause) {
			wake = nil
			if a := l.sharedAuthority(); a != nil {
				tick = a.waitHint(l.clock().UTC())
			}
		}
		timer := time.NewTimer(tick)
		select {
		case <-ctx.Done():
			timer.Stop()
			if !feedSharedCause(cause) {
				l.countWeightRefusal(spec, cause)
			}
			return nil, fmt.Errorf("bootstrap waited for weight capacity until its key deadline ended (%v): %w", ctx.Err(), err)
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (l *feedRequestLedger) acquireShared(ctx context.Context, spec feedAcquireSpec, silent bool) (*feedRequestPermit, error) {
	permit, err := l.acquireSpec(spec, silent)
	if err != nil || permit == nil || !permit.durable {
		return permit, err
	}
	a := l.sharedAuthority()
	if a == nil {
		return permit, nil
	}
	if derr := a.reserve(ctx, permit); derr != nil {
		l.unreserve(permit, spec, derr)
		var r *feedWeightRefusal
		if errors.As(derr, &r) {
			cp := *r
			cp.Reason, cp.ReqType, cp.Input, cp.Cost = spec.reason, spec.reqType, spec.input, permit.cost
			a.noteRefusal(&cp)
			return nil, &cp
		}
		return nil, derr
	}
	if err := feedAuthorityHookAt("reserve:after_commit"); err != nil {
		a.releasePermit(ctx, permit)
		l.unreserve(permit, spec, feedSharedRefusal(feedWeightCauseSharedStorage, err.Error()))
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, err.Error())
	}
	return permit, nil
}

func (l *feedRequestLedger) changedCh() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wChanged
}

func feedBudgetConfigErrors(field string, cfg *feedBudgetConfig, required bool) []string {
	if cfg == nil {
		if required {
			return []string{fmt.Sprintf("%s is required for feed.source %q: set per_minute and startup from the measured request baseline, and weight_per_minute, weight_startup and weight_reserve (weight_per_minute + weight_startup <= %d - weight_reserve)", field, feedSourceREST, hlRESTWeightCeiling)}
		}
		return nil
	}
	var errs []string
	if cfg.PerMinute <= 0 {
		errs = append(errs, fmt.Sprintf("%s.per_minute must be a positive request count, got %d", field, cfg.PerMinute))
	}
	if cfg.Startup <= 0 {
		errs = append(errs, fmt.Sprintf("%s.startup must be a positive request count, got %d", field, cfg.Startup))
	}
	errs = append(errs, feedWeightPolicyErrors(field, cfg.WeightPerMinute, cfg.WeightStartup, cfg.WeightReserve, required)...)
	return errs
}

func feedWeightPolicyErrors(field string, perMinute, startup, reserve *int, required bool) []string {
	names := []string{"weight_per_minute", "weight_startup", "weight_reserve"}
	vals := []*int{perMinute, startup, reserve}
	var missing, present []string
	for i, v := range vals {
		if v == nil {
			missing = append(missing, field+"."+names[i])
		} else {
			present = append(present, field+"."+names[i])
		}
	}
	if len(present) == 0 {
		if required {
			return []string{fmt.Sprintf("%s.weight_per_minute, %s.weight_startup and %s.weight_reserve are required for feed.source %q: a REST feed must enforce a venue weight allocation (weight_per_minute + weight_startup <= %d - weight_reserve; request limits are never read as weights)",
				field, field, field, feedSourceREST, hlRESTWeightCeiling)}
		}
		return nil
	}
	if len(missing) > 0 {
		return []string{fmt.Sprintf("%s is a partial weight policy: %s set but %s missing; set all three weight fields or none",
			field, strings.Join(present, ", "), strings.Join(missing, ", "))}
	}
	var errs []string
	for i, v := range vals {
		if *v < 0 || *v > hlRESTWeightCeiling {
			errs = append(errs, fmt.Sprintf("%s.%s must be between 0 and the %d venue weight ceiling, got %d", field, names[i], hlRESTWeightCeiling, *v))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if *perMinute > hlRESTWeightCeiling-*reserve-*startup {
		errs = append(errs, fmt.Sprintf("%s weight allocation %d (weight_per_minute %d + weight_startup %d) exceeds the %d venue weight ceiling less weight_reserve %d (at most %d)",
			field, *perMinute+*startup, *perMinute, *startup, hlRESTWeightCeiling, *reserve, hlRESTWeightCeiling-*reserve))
	}
	return errs
}

func formatFeedWeightPolicy(p *feedWeightPolicy) string {
	if p == nil {
		return fmt.Sprintf("weight: participation=%s (no weight policy; request behavior only, no venue weight guarantee; ceiling %d)", feedWeightNotEnrolled, hlRESTWeightCeiling)
	}
	return fmt.Sprintf("weight: participation=%s weight_per_minute=%d weight_startup=%d weight_reserve=%d allocation=%d ceiling=%d",
		feedWeightParticipating, p.PerMinute, p.Startup, p.Reserve, p.PerMinute+p.Startup, hlRESTWeightCeiling)
}
