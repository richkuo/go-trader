package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	hlRESTWeightCeiling      = 1200
	hlWeightRowUnit          = 1
	hlWeightCheckedOn        = "2026-11-16"
	hlWeightSourceRateLimits = "https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/rate-limits-and-user-limits"
	hlWeightSourceInfo       = "https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint"
	hlWeightSourcePerps      = "https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals"
	hlCandleRowCap           = 5000
	hlFundingRowCap          = 500
	hlFundingRecordStepMs    = int64(3_600_000)
	hlCandleMonthBoundMs     = int64(28 * 86_400_000)
)

type hlInfoWeightRule struct {
	Type    string
	Base    int
	RowStep int
	RowCap  int
	Basis   string
}

var hlInfoWeightRules = map[string]hlInfoWeightRule{
	"candleSnapshot": {Type: "candleSnapshot", Base: 20, RowStep: 60, RowCap: hlCandleRowCap,
		Basis: "documented: all other documented info requests have weight 20; candleSnapshot has an additional rate limit weight per 60 items returned; only the most recent 5000 candles are available; local: the 5000-row cap is a request-safety cap enforced by limiting the request window"},
	"fundingHistory": {Type: "fundingHistory", Base: 20, RowStep: 20, RowCap: hlFundingRowCap,
		Basis: "documented: all other documented info requests have weight 20; fundingHistory has an additional rate limit weight per 20 items returned; responses that take a time range only return 500 elements"},
	"allMids": {Type: "allMids", Base: 2,
		Basis: "documented: allMids is in the weight-2 info list"},
	"clearinghouseState": {Type: "clearinghouseState", Base: 2,
		Basis: "documented: clearinghouseState is in the weight-2 info list"},
	"metaAndAssetCtxs": {Type: "metaAndAssetCtxs", Base: 20,
		Basis: "documented: all other documented info requests have weight 20"},
	"meta": {Type: "meta", Base: 20,
		Basis: "documented: all other documented info requests have weight 20"},
}

var hlWeightAssumptions = []string{
	"A1: one extra weight unit per started row step (rounded up); the venue does not state the size of the extra unit",
	"A2: a sent request is charged its full reserved cost whatever its outcome; the venue does not state how failed requests are charged",
	"A3: at most one funding record per coin per hour",
	"A4: a candle response holds at most floor(span/interval)+2 bars (the bar that contains the start and the forming bar)",
	"A5: a candle request is sent only when its A4 bound is at most 5000 rows, so the candle cap is enforced by limiting the request window; the 500-element statement caps one fundingHistory response",
	"A6: a 1M bar lasts at least 28 days, so the 1M row bound uses 28 days",
}

func hlInfoWeightRuleFor(reqType string) (hlInfoWeightRule, bool) {
	r, ok := hlInfoWeightRules[reqType]
	return r, ok
}

func hlInfoWeightRaw(rule hlInfoWeightRule, rows int) int {
	if rule.RowStep <= 0 || rows <= 0 {
		return rule.Base
	}
	steps := (rows + rule.RowStep - 1) / rule.RowStep
	return rule.Base + steps*hlWeightRowUnit
}

func hlInfoWeight(reqType string, rows int) (int, error) {
	rule, ok := hlInfoWeightRuleFor(reqType)
	if !ok {
		return 0, fmt.Errorf("info request type %q has no weight rule; it is refused because its cost has no justified bound", reqType)
	}
	if rows < 0 {
		return 0, fmt.Errorf("info request %s row bound %d is negative", reqType, rows)
	}
	if rule.RowStep > 0 && rows > rule.RowCap {
		return 0, fmt.Errorf("info request %s row bound %d is above the %d-row response cap", reqType, rows, rule.RowCap)
	}
	return hlInfoWeightRaw(rule, rows), nil
}

func hlInfoWeightAtCap(reqType string) (int, error) {
	rule, ok := hlInfoWeightRuleFor(reqType)
	if !ok {
		return hlInfoWeight(reqType, 0)
	}
	return hlInfoWeightRaw(rule, rule.RowCap), nil
}

func hlInfoRowCap(reqType string) int {
	rule, ok := hlInfoWeightRuleFor(reqType)
	if !ok || rule.RowStep <= 0 {
		return 0
	}
	return rule.RowCap
}

func hlCandleBoundIntervalMs(interval string) (int64, error) {
	if interval == "1M" {
		return hlCandleMonthBoundMs, nil
	}
	ms, ok := hlCandleIntervalMs(interval)
	if !ok || ms <= 0 {
		return 0, fmt.Errorf("unsupported Hyperliquid interval %q has no row bound", interval)
	}
	return ms, nil
}

var errHLCandleWindowTooWide = errors.New("candle request window is wider than one response may hold, so it is not sent")

func hlCandleRowBound(interval string, startMs, endMs int64) (int, error) {
	ms, err := hlCandleBoundIntervalMs(interval)
	if err != nil {
		return 0, err
	}
	if endMs < startMs {
		return 0, fmt.Errorf("candle request end %d is before its start %d", endMs, startMs)
	}
	span := endMs - startMs
	bars := span/ms + 2
	if span < 0 || bars < 0 || bars > hlCandleRowCap {
		return 0, fmt.Errorf("%s candle window %d to %d is above the %d-row cap: %w", interval, startMs, endMs, hlCandleRowCap, errHLCandleWindowTooWide)
	}
	return int(bars), nil
}

func hlCandleWindowFits(interval string, startMs, endMs int64) bool {
	_, err := hlCandleRowBound(interval, startMs, endMs)
	return !errors.Is(err, errHLCandleWindowTooWide)
}

func hlCandleMaxSpanBars(interval string) (int64, error) {
	intervalMs, ok := hlCandleIntervalMs(interval)
	if !ok || intervalMs <= 0 {
		return 0, fmt.Errorf("unsupported Hyperliquid interval %q", interval)
	}
	boundMs, err := hlCandleBoundIntervalMs(interval)
	if err != nil {
		return 0, err
	}
	return boundMs * (hlCandleRowCap - 2) / intervalMs, nil
}

func hlFundingRowBound(startMs, endMs int64) (int, error) {
	if endMs < startMs {
		return 0, fmt.Errorf("funding request end %d is before its start %d", endMs, startMs)
	}
	rows := (endMs-startMs)/hlFundingRecordStepMs + 1
	if rows > hlFundingRowCap || rows < 0 {
		return hlFundingRowCap, nil
	}
	return int(rows), nil
}

func hlInfoWeightUncapped(reqType string, rows int) int {
	rule, ok := hlInfoWeightRuleFor(reqType)
	if !ok {
		return 0
	}
	if rows > math.MaxInt32 {
		rows = math.MaxInt32
	}
	return hlInfoWeightRaw(rule, rows)
}

func hlWeightTableLine() string {
	types := make([]string, 0, len(hlInfoWeightRules))
	for t := range hlInfoWeightRules {
		types = append(types, t)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		r := hlInfoWeightRules[t]
		if r.RowStep > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d+%d/%d rows(cap %d rows, %d max)", t, r.Base, hlWeightRowUnit, r.RowStep, r.RowCap, hlInfoWeightRaw(r, r.RowCap)))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%d", t, r.Base))
		}
	}
	return fmt.Sprintf("weight table: ceiling=%d/min per IP (documented) %s; checked %s from %s, %s, %s; assumptions A1-A6 (%s)",
		hlRESTWeightCeiling, strings.Join(parts, " "), hlWeightCheckedOn, hlWeightSourceRateLimits, hlWeightSourceInfo, hlWeightSourcePerps,
		strings.Join(hlWeightAssumptions, "; "))
}

func (l *feedRequestLedger) revalidateLocked(p *feedRequestPermit, now time.Time) *feedWeightRefusal {
	if l.weight == nil || p.wState != feedPermitPending {
		return nil
	}
	if b := l.wBounds[p.reqType]; b != nil && b.epoch != p.epoch {
		return &feedWeightRefusal{Cause: feedWeightCauseBound, Reason: p.reason, ReqType: p.reqType, Input: p.input,
			Detail: fmt.Sprintf("the %s row bound changed after this read was priced; it must be priced again", p.reqType)}
	}
	l.pruneLocked(now)
	used := l.steadyUsedLocked()
	if used > l.weight.PerMinute {
		return &feedWeightRefusal{Cause: feedWeightCauseExhausted, Reason: p.reason, ReqType: p.reqType, Input: p.input, Cost: p.cost,
			SteadyUsed: used - (p.cost - p.startupPart), PerMinute: l.weight.PerMinute, StartupLeft: l.wStartupLeft}
	}
	return nil
}

func (l *feedRequestLedger) releaseLocked(p *feedRequestPermit) {
	if p.wState != feedPermitPending {
		return
	}
	l.wPending -= p.cost
	l.wOpenSteady -= p.cost - p.startupPart
	l.restoreStartupLocked(p.startupPart)
	l.wTotals.Released += p.cost
	p.wState = feedPermitClosed
	l.signalLocked()
}

func (l *feedRequestLedger) restoreStartupLocked(w int) {
	if w <= 0 || l.weight == nil {
		return
	}
	l.wStartupLeft += w
	if l.wStartupLeft > l.weight.Startup {
		l.wStartupLeft = l.weight.Startup
	}
}

func (l *feedRequestLedger) chargeLocked(p *feedRequestPermit, now time.Time, charge int) (int, int, bool) {
	return l.closeChargeLocked(p, now, charge, false)
}

func (l *feedRequestLedger) closeChargeLocked(p *feedRequestPermit, now time.Time, charge int, settled bool) (int, int, bool) {
	switch p.wState {
	case feedPermitPending:
		l.wPending -= p.cost
	case feedPermitInFlight:
		l.wInFlight -= p.cost
	default:
		return 0, 0, false
	}
	if charge > p.cost || charge < 0 {
		charge = p.cost
	}
	l.wOpenSteady -= p.cost - p.startupPart
	refund := p.cost - charge
	startupRefund := refund
	if startupRefund > p.startupPart {
		startupRefund = p.startupPart
	}
	l.restoreStartupLocked(startupRefund)
	if charge > 0 {
		l.wCharges = append(l.wCharges, feedWeightCharge{at: now, weight: charge, startup: p.startupPart - startupRefund, host: p.host})
	}
	if settled {
		l.wTotals.Settled += charge
		l.wTotals.Refunded += refund
	} else {
		l.wTotals.Retained += charge
	}
	l.wTotals.ChargedByType[p.reqType] += charge
	p.wState = feedPermitClosed
	l.signalLocked()
	return charge, p.startupPart - startupRefund, true
}

func (p *feedRequestPermit) settle(rows int) {
	if p == nil || p.l == nil {
		return
	}
	l := p.l
	l.mu.Lock()
	if p.wState == feedPermitClosed {
		l.mu.Unlock()
		return
	}
	actual, err := hlInfoWeight(p.reqType, rows)
	if err != nil {
		actual = p.cost
	}
	charge, startup, closed := l.closeChargeLocked(p, l.clock().UTC(), actual, true)
	if l.wEpisode.open && feedWeightRequiredReason(p.reason) {
		l.wEpisode.successAfter = true
	}
	l.mu.Unlock()
	if closed {
		feedAuthorityHookAt("settle:before_commit")
		p.sharedClose(feedAuthClose{charge: charge, startup: startup})
	}
}

func (p *feedRequestPermit) retain() {
	if p == nil || p.l == nil {
		return
	}
	l := p.l
	l.mu.Lock()
	charge, startup, closed := l.chargeLocked(p, l.clock().UTC(), p.cost)
	l.mu.Unlock()
	if closed {
		p.sharedClose(feedAuthClose{charge: charge, startup: startup})
	}
}

func (p *feedRequestPermit) release() {
	if p == nil || p.l == nil {
		return
	}
	l := p.l
	l.mu.Lock()
	open := p.wState == feedPermitPending
	l.releaseLocked(p)
	l.mu.Unlock()
	if open {
		p.sharedRelease()
	}
}

func (p *feedRequestPermit) closeOpen() {
	if p == nil || p.l == nil {
		return
	}
	l := p.l
	l.mu.Lock()
	switch p.wState {
	case feedPermitPending:
		l.releaseLocked(p)
		l.mu.Unlock()
		p.sharedRelease()
		return
	case feedPermitInFlight:
		charge, startup, closed := l.chargeLocked(p, l.clock().UTC(), p.cost)
		l.mu.Unlock()
		if closed {
			p.sharedClose(feedAuthClose{charge: charge, startup: startup})
		}
		return
	}
	l.mu.Unlock()
}

func (p *feedRequestPermit) violation(bound, observed int) {
	if p == nil || p.l == nil {
		return
	}
	l := p.l
	l.mu.Lock()
	now := l.clock().UTC()
	charge, startup, closed := l.chargeLocked(p, now, p.cost)
	debt := hlInfoWeightUncapped(p.reqType, observed) - p.cost
	if !closed {
		debt = 0
	}
	if debt > feedAuthorityMaxDebt {
		debt = feedAuthorityMaxDebt
	}
	if debt > 0 {
		l.wCharges = append(l.wCharges, feedWeightCharge{at: now, weight: debt, debt: true, host: p.host})
		l.wTotals.Debt += debt
		l.wTotals.ChargedByType[p.reqType] += debt
	}
	b := l.wBounds[p.reqType]
	if b == nil {
		b = &feedWeightBound{}
		l.wBounds[p.reqType] = b
	}
	prevAtCap, prevBlocked := b.atCap, b.blocked
	state := "repriced_at_cap"
	if observed > hlInfoRowCap(p.reqType) {
		b.blocked = true
		state = "blocked"
	} else {
		b.atCap = true
	}
	if b.atCap != prevAtCap || b.blocked != prevBlocked {
		b.epoch++
		b.bound, b.observed, b.at, b.alerted = bound, observed, now, false
	}
	l.signalLocked()
	logf := l.logf
	bound2 := &feedAuthBound{AtCap: b.atCap, Blocked: b.blocked, Bound: b.bound, Observed: b.observed}
	l.mu.Unlock()
	if closed {
		p.sharedClose(feedAuthClose{charge: charge, startup: startup, debt: max(debt, 0), bound: bound2, boundType: p.reqType})
	}
	if logf != nil {
		logf("[feed-weight] bound_violation type=%s input=%s bound=%d observed=%d debt=%d state=%s", p.reqType, p.input, bound, observed, debt, state)
	}
}

type feedWeightEvent struct {
	Kind     string
	OpenedAt time.Time
	Inputs   []string
	Refused  int
	Type     string
	Bound    int
	Observed int
	State    string
	Enforced bool
}

func (l *feedRequestLedger) weightReport(outstanding []string, required func(string) bool) []feedWeightEvent {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	refused := sortedFeedSet(l.wEpisode.inputs)
	l.mu.Unlock()
	retiredInputs := make(map[string]bool)
	for _, input := range refused {
		if required != nil && !required(input) {
			retiredInputs[input] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []feedWeightEvent
	types := make([]string, 0, len(l.wBounds))
	for t := range l.wBounds {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		b := l.wBounds[t]
		if b.alerted {
			continue
		}
		b.alerted = true
		state := "repriced_at_cap"
		if b.blocked {
			state = "blocked"
		}
		out = append(out, feedWeightEvent{Kind: feedWeightEventViolation, Type: t, Bound: b.bound, Observed: b.observed, State: state, Enforced: l.weight != nil})
	}
	ep := &l.wEpisode
	if !ep.open {
		return out
	}
	if !ep.startSent {
		ep.startSent = true
		inputs := make(map[string]bool, len(ep.inputs)+len(outstanding))
		for k := range ep.inputs {
			inputs[k] = true
		}
		for _, k := range outstanding {
			inputs[k] = true
		}
		out = append(out, feedWeightEvent{Kind: feedWeightEventStart, OpenedAt: ep.openedAt, Inputs: sortedFeedSet(inputs), Refused: ep.refused})
		return out
	}
	recovered, retired := map[string]bool{}, map[string]bool{}
	for input := range ep.inputs {
		switch {
		case retiredInputs[input]:
			retired[input] = true
		case ep.valid[input]:
			recovered[input] = true
		default:
			return out
		}
	}
	if len(outstanding) > 0 {
		return out
	}
	if len(recovered) == 0 {
		out = append(out, feedWeightEvent{Kind: feedWeightEventRetired, OpenedAt: ep.openedAt, Inputs: sortedFeedSet(retired), Refused: ep.refused})
		*ep = feedWeightEpisode{}
		return out
	}
	if !ep.successAfter {
		return out
	}
	out = append(out, feedWeightEvent{Kind: feedWeightEventRecover, OpenedAt: ep.openedAt, Inputs: sortedFeedSet(recovered), Refused: ep.refused})
	*ep = feedWeightEpisode{}
	return out
}

func (ev feedWeightEvent) logLine() string {
	if ev.Kind == feedWeightEventViolation {
		return fmt.Sprintf("status=%s type=%s bound=%d observed=%d state=%s", ev.Kind, ev.Type, ev.Bound, ev.Observed, ev.State)
	}
	return fmt.Sprintf("status=%s opened_at=%s refused=%d inputs=%s", ev.Kind, ev.OpenedAt.UTC().Format(time.RFC3339), ev.Refused, joinFeedNames(ev.Inputs))
}

func formatFeedWeightAlert(ev feedWeightEvent, source, instance string) string {
	switch ev.Kind {
	case feedWeightEventViolation:
		after := map[string]string{"blocked": "refused until restart (a shared weight participant restarted within 60s keeps the block)", "repriced_at_cap": "priced at the response cap"}[ev.State]
		if !ev.Enforced {
			after = map[string]string{"blocked": "still sent, because this owner has no weight policy; their weight is recorded but not enforced", "repriced_at_cap": "recorded at the response cap, but not refused, because this owner has no weight policy"}[ev.State]
		}
		return fmt.Sprintf("**MARKET FEED WEIGHT BOUND VIOLATED** [%s] a %s response returned %d rows over its %d-row bound; the excess is charged as debt and later %s reads are %s (instance %s).",
			source, ev.Type, ev.Observed, ev.Bound, ev.Type, after, instance)
	case feedWeightEventRecover:
		return fmt.Sprintf("**MARKET FEED WEIGHT RECOVERED** [%s] every weight-refused input that is still required passed validation again (recovered inputs %s; %d refused request(s); instance %s).",
			source, joinFeedNames(ev.Inputs), ev.Refused, instance)
	case feedWeightEventRetired:
		return ""
	}
	return fmt.Sprintf("**MARKET FEED WEIGHT REFUSED** [%s] this owner's venue weight allocation refused required reads. Refused inputs: %s. Consumers can hold entries when a required input is unavailable from the selected feed; protection continues on verified inputs (instance %s).",
		source, joinFeedNames(ev.Inputs), instance)
}

func estimateFeedUnionWeight(union feedRequirements) string {
	bootstrap, refresh, largest := 0, 0, 0
	for _, key := range union.Order {
		rows := union.Keys[key] + hlCandleGapMargin + 2
		if rows > hlCandleRowCap {
			rows = hlCandleRowCap
		}
		if w, err := hlInfoWeight("candleSnapshot", rows); err == nil {
			bootstrap += w
			largest = max(largest, w)
		}
		if w, err := hlInfoWeight("candleSnapshot", 3); err == nil {
			refresh += w
		}
	}
	mids := 0
	if len(union.MidCoins) > 0 {
		mids, _ = hlInfoWeight("allMids", 0)
	}
	funding := estimateFeedFunding(union)
	pageW, _ := hlInfoWeightAtCap("fundingHistory")
	if funding.ScalarCoins > 0 {
		largest = max(largest, funding.ContextWeight, funding.AverageWeight)
	}
	if funding.RecordCoins > 0 {
		largest = max(largest, pageW)
	}
	return fmt.Sprintf("weight estimate (informational, first passes only): bootstrap=%d for %d keys; refresh=%d per deadline for candles plus mids=%d; funding scalar=%d per refresh; funding records for %d coin(s) and accounting funding at up to %d per page; largest_read=%d",
		bootstrap, len(union.Order), refresh, mids, funding.scalarWeight(), funding.RecordCoins, pageW, largest)
}

func formatMarketFeedWeightBudget(b *feedWeightBudgetConfig) string {
	if b == nil {
		return "none"
	}
	f := func(v *int) string {
		if v == nil {
			return "unset"
		}
		return fmt.Sprintf("%d", *v)
	}
	return fmt.Sprintf("weight_per_minute=%s weight_startup=%s weight_reserve=%s", f(b.WeightPerMinute), f(b.WeightStartup), f(b.WeightReserve))
}

const feedDemandHorizonSeconds = int64(86_400)

type feedDemandClass struct {
	Label       string
	Deadlines   int
	DueMax      int
	ReusedMax   int
	CandleMax   int
	CandleTotal int
}

func feedDeadlineClassLabel(cadences []int, deadline int64) string {
	var parts []string
	for _, c := range cadences {
		if c > 0 && deadline%int64(c) == 0 {
			parts = append(parts, fmt.Sprintf("%ds", c))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

type feedDemandWindow struct {
	SpanSeconds int64
	Weight      int
	Reads       int
	Keys        int
	Deadlines   int
	At          int64
}

type feedDemandModel struct {
	Cadences   []int
	Problem    string
	Classes    []*feedDemandClass
	MinSpacing int64
	Nominal    feedDemandWindow
	Completion feedDemandWindow
	MidsWeight int
	Funding    feedFundingDemand
}

func modelFeedDemand(union feedRequirements) feedDemandModel {
	m := feedDemandModel{Funding: estimateFeedFunding(union)}
	cadenceSet := make(map[int]bool)
	for _, key := range union.Order {
		for _, c := range union.KeyCadences[key] {
			if c > 0 {
				cadenceSet[c] = true
			}
		}
	}
	for c := range cadenceSet {
		m.Cadences = append(m.Cadences, c)
	}
	sort.Ints(m.Cadences)
	candleW, _ := hlInfoWeight("candleSnapshot", feedIncrementalRows)
	midsReads := 0
	if len(union.MidCoins) > 0 {
		m.MidsWeight, _ = hlInfoWeight("allMids", 0)
		midsReads = 1
	}
	if len(m.Cadences) == 0 {
		m.Problem = "no key has a cadence, so no deadline reads are estimated"
		return m
	}
	horizon := feedDemandHorizonSeconds
	for _, c := range m.Cadences {
		if horizon%int64(c) != 0 {
			m.Problem = fmt.Sprintf("cadences %s do not divide one day, so the deadline cycle is not simulated", formatCadences(m.Cadences))
			m.MinSpacing = feedCadenceSpacing(m.Cadences)
			return m
		}
	}
	deadlineSet := make(map[int64]bool)
	for _, c := range m.Cadences {
		for d := int64(0); d < horizon; d += int64(c) {
			deadlineSet[d] = true
		}
	}
	deadlines := make([]int64, 0, len(deadlineSet))
	for d := range deadlineSet {
		deadlines = append(deadlines, d)
	}
	sort.Slice(deadlines, func(i, j int) bool { return deadlines[i] < deadlines[j] })
	lastFetchBar := make(map[marketFeedKey]int64)
	weightAt, readsAt, keysAt := make([]int, len(deadlines)), make([]int, len(deadlines)), make([]int, len(deadlines))
	classes := make(map[string]*feedDemandClass)
	for pass := 0; pass < 2; pass++ {
		for i, d := range deadlines {
			due, reused, w := 0, 0, 0
			for _, key := range union.Order {
				if !feedKeyDueAt(union.KeyCadences[key], d) {
					continue
				}
				due++
				if union.closedOnlyReuse(key) {
					intervalS, _ := hlCandleIntervalMs(key.Timeframe)
					intervalS /= 1000
					bar := d / intervalS
					if prev, ok := lastFetchBar[key]; ok && prev == bar {
						reused++
						continue
					}
					lastFetchBar[key] = bar
				}
				w += candleW
			}
			if pass == 0 {
				continue
			}
			weightAt[i], readsAt[i], keysAt[i] = w+m.MidsWeight, due-reused+midsReads, due
			label := feedDeadlineClassLabel(m.Cadences, d)
			cl := classes[label]
			if cl == nil {
				cl = &feedDemandClass{Label: label}
				classes[label] = cl
				m.Classes = append(m.Classes, cl)
			}
			cl.Deadlines++
			cl.DueMax = max(cl.DueMax, due)
			cl.ReusedMax = max(cl.ReusedMax, reused)
			cl.CandleMax = max(cl.CandleMax, w)
			cl.CandleTotal += w
		}
	}
	sort.Slice(m.Classes, func(i, j int) bool { return m.Classes[i].Label < m.Classes[j].Label })
	n := len(deadlines)
	m.MinSpacing = horizon
	for i := range deadlines {
		next := deadlines[(i+1)%n]
		if i == n-1 {
			next += horizon
		}
		m.MinSpacing = min(m.MinSpacing, next-deadlines[i])
	}
	window := func(span int64) feedDemandWindow {
		best := feedDemandWindow{SpanSeconds: span}
		for i := 0; i < n; i++ {
			cur := feedDemandWindow{SpanSeconds: span, At: deadlines[i]}
			for k := 0; k < n; k++ {
				j := (i - k + n) % n
				t := deadlines[j]
				if j > i {
					t -= horizon
				}
				if deadlines[i]-t >= span {
					break
				}
				cur.Weight += weightAt[j]
				cur.Reads += readsAt[j]
				cur.Keys += keysAt[j]
				cur.Deadlines++
			}
			if cur.Weight > best.Weight {
				best = cur
			}
		}
		return best
	}
	m.Nominal = window(int64(feedBudgetWindow / time.Second))
	m.Completion = window(int64((feedBudgetWindow + feedSealPrepareBudget) / time.Second))
	return m
}

func feedCadenceSpacing(cadences []int) int64 {
	gcd := func(a, b int64) int64 {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	spacing := int64(0)
	for i, a := range cadences {
		if spacing == 0 || int64(a) < spacing {
			spacing = int64(a)
		}
		for _, b := range cadences[i+1:] {
			if g := gcd(int64(a), int64(b)); g < spacing {
				spacing = g
			}
		}
	}
	return spacing
}

func estimateFeedDemandLines(union feedRequirements, source string) []string {
	if source != feedSourceREST {
		return []string{fmt.Sprintf("weight demand source=%s: candles arrive on the venue socket, so no per-deadline candle reads are estimated; funding and accounting reads follow the lines below", source)}
	}
	m := modelFeedDemand(union)
	if m.Problem != "" {
		return append([]string{"weight demand source=rest: " + m.Problem}, m.Funding.lines()...)
	}
	var out []string
	for _, cl := range m.Classes {
		out = append(out, fmt.Sprintf("weight demand source=rest class=%s deadlines_per_day=%d keys_due_max=%d reused_max=%d candle_weight_max=%d candle_weight_avg=%d mids=%d",
			cl.Label, cl.Deadlines, cl.DueMax, cl.ReusedMax, cl.CandleMax, cl.CandleTotal/max(cl.Deadlines, 1), m.MidsWeight))
	}
	out = append(out, fmt.Sprintf("weight demand source=rest rolling_60s_peak candles_and_mids=%d at day offset %ds (%d deadline(s), class %s) with_funding_expiry=%d",
		m.Nominal.Weight, m.Nominal.At, m.Nominal.Deadlines, feedDeadlineClassLabel(m.Cadences, m.Nominal.At), m.Nominal.Weight+m.Funding.steadyWeight()))
	out = append(out, fmt.Sprintf("weight demand overlap nominal: min_deadline_spacing=%ds window=%ds adjacent_preparations_share_a_window=%t keys_due=%d weight_peak=%d",
		m.MinSpacing, m.Nominal.SpanSeconds, m.MinSpacing < m.Nominal.SpanSeconds, m.Nominal.Keys, m.Nominal.Weight))
	out = append(out, fmt.Sprintf("weight demand overlap completion_based: window=%ds (a read that settles at the end of its %ds preparation budget stays charged for %ds) adjacent_preparations_can_share_a_window=%t keys_due=%d weight_peak=%d; a preparation that settles within %ds of its start shares none",
		m.Completion.SpanSeconds, int64(feedSealPrepareBudget/time.Second), int64(feedBudgetWindow/time.Second), m.MinSpacing < m.Completion.SpanSeconds, m.Completion.Keys, m.Completion.Weight, max(m.MinSpacing-m.Nominal.SpanSeconds, 0)))
	out = append(out, fmt.Sprintf("request demand source=rest rolling_60s_peak reads=%d (candles and mids) steady_funding_reads=%d required_per_minute=%d (peak, steady funding and one read kept for mids); completion_based reads=%d",
		m.Nominal.Reads, m.Funding.steadyReads(), m.requiredRequests(), m.Completion.Reads))
	out = append(out, m.Funding.lines()...)
	out = append(out, "weight demand assumptions: warm history (one incremental candle read of at most 3 rows per due key); closed_only keys are read once at the first due deadline after each bar close and reused until the next close; a nominal read is charged from its deadline for 60s; with_funding_expiry assumes that every steady funding read coincides with the peak deadline, and the preparation spreads them; non-participating owners on the host are not included")
	return out
}

func (m feedDemandModel) requiredRequests() int {
	return m.Nominal.Reads + m.Funding.steadyReads() + 1
}

func feedDemandWarnings(union feedRequirements, source string, budget *feedBudgetConfig) []string {
	if source != feedSourceREST {
		return nil
	}
	m := modelFeedDemand(union)
	var out []string
	window := int64(feedBudgetWindow / time.Second)
	if m.MinSpacing > 0 && m.MinSpacing < window {
		out = append(out, fmt.Sprintf("cadences %s put adjacent preparations %ds apart, inside one %ds budget window, so their reads share one allocation", formatCadences(m.Cadences), m.MinSpacing, window))
	}
	if m.Problem != "" || budget == nil {
		return out
	}
	if policy := budget.weightPolicy(); policy != nil {
		if m.Nominal.Weight > policy.PerMinute {
			out = append(out, fmt.Sprintf("estimated candle and mids demand %d per %ds is above the steady allocation weight_per_minute=%d", m.Nominal.Weight, window, policy.PerMinute))
		} else if total := m.Nominal.Weight + m.Funding.steadyWeight(); total > policy.PerMinute {
			out = append(out, fmt.Sprintf("estimated candle, mids and steady funding demand %d per %ds is above the steady allocation weight_per_minute=%d", total, window, policy.PerMinute))
		}
		if limit := (hlRESTWeightCeiling - policy.Reserve) * 2 / 3; m.Nominal.Weight > limit {
			out = append(out, fmt.Sprintf("estimated candle and mids demand %d is above %d, the largest measured peak that derived calibration accepts at weight_reserve=%d", m.Nominal.Weight, limit, policy.Reserve))
		}
	}
	if need := m.requiredRequests(); need > budget.PerMinute {
		out = append(out, fmt.Sprintf("estimated request demand %d per %ds is above per_minute=%d", need, window, budget.PerMinute))
	}
	return out
}

type feedFundingLoadBound struct {
	Coin    string
	Hours   int64
	Pages   int64
	Weight  int
	Readers string
}

type feedFundingDemand struct {
	ScalarCoins      int
	DirectAverages   int
	RecordCoins      int
	AccountingCoins  int
	ContextWeight    int
	AverageWeight    int
	TailWeight       int
	AccountingWeight int
	Loads            []feedFundingLoadBound
}

func estimateFeedFunding(union feedRequirements) feedFundingDemand {
	var d feedFundingDemand
	d.ContextWeight, _ = hlInfoWeight("metaAndAssetCtxs", 0)
	avgRows, _ := hlFundingRowBound(0, int64(hlFundingAvgWindowDays)*86_400_000)
	d.AverageWeight, _ = hlInfoWeight("fundingHistory", avgRows)
	tailRows, _ := hlFundingRowBound(0, hlFundingRecordStepMs)
	d.TailWeight, _ = hlInfoWeight("fundingHistory", tailRows)
	overlapRows, _ := hlFundingRowBound(0, feedAccountingOverlap.Milliseconds()+2*feedAccountingCadence.Milliseconds())
	overlapW, _ := hlInfoWeight("fundingHistory", overlapRows)
	d.AccountingWeight = overlapW + d.TailWeight
	d.AccountingCoins = len(union.AccountingCoins)
	for _, coin := range sortedFundingCoins(union.Funding) {
		need := union.Funding[coin]
		if need.Scalar {
			d.ScalarCoins++
			if !need.Records || need.rangeHours() <= int64(hlFundingAvgWindowDays)*24 {
				d.DirectAverages++
			}
		}
		if !need.Records {
			continue
		}
		d.RecordCoins++
		b := feedFundingLoadBound{Coin: coin, Hours: need.rangeHours(), Readers: need.readerList()}
		rows := b.Hours + 1
		b.Pages = (rows + hlFundingRowCap - 1) / hlFundingRowCap
		for left := rows; left > 0; left -= hlFundingRowCap {
			w, _ := hlInfoWeight("fundingHistory", int(min(left, hlFundingRowCap)))
			b.Weight += w
		}
		d.Loads = append(d.Loads, b)
	}
	return d
}

func (d feedFundingDemand) scalarWeight() int {
	if d.ScalarCoins == 0 {
		return 0
	}
	return d.ContextWeight + d.DirectAverages*d.AverageWeight
}

func (d feedFundingDemand) steadyWeight() int {
	return d.scalarWeight() + d.RecordCoins*d.TailWeight
}

func (d feedFundingDemand) steadyReads() int {
	reads := d.DirectAverages + d.RecordCoins
	if d.ScalarCoins > 0 {
		reads++
	}
	return reads
}

func (d feedFundingDemand) lines() []string {
	ttl := int(feedFundingTTL / time.Second)
	out := []string{fmt.Sprintf("weight demand funding: scalar=%d per %ds for %d coin(s) (one shared metaAndAssetCtxs %d, %d direct 7-day fundingHistory read(s) of %d where the record range does not prove the window); strategy_records_tail=%d per %ds and after each hour for %d coin(s) (one bounded page of %d each); accounting=%d per hour for %d coin(s) (up to %d each), spread over %ds; cold pages use only the residual request and weight allowance",
		d.scalarWeight(), ttl, d.ScalarCoins, d.ContextWeight, d.DirectAverages, d.AverageWeight, d.RecordCoins*d.TailWeight, ttl, d.RecordCoins, d.TailWeight,
		d.AccountingCoins*d.AccountingWeight, d.AccountingCoins, d.AccountingWeight, int(feedAccountingSlack/time.Second))}
	for _, b := range d.Loads {
		out = append(out, fmt.Sprintf("weight demand funding full load coin=%s range_hours=%d pages_max=%d weight_max=%d readers=%s", b.Coin, b.Hours, b.Pages, b.Weight, b.Readers))
	}
	return out
}
