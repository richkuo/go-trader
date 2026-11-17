package main

import (
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
		Basis: "documented: all other documented info requests have weight 20; candleSnapshot has an additional rate limit weight per 60 items returned; only the most recent 5000 candles are available"},
	"fundingHistory": {Type: "fundingHistory", Base: 20, RowStep: 20, RowCap: hlFundingRowCap,
		Basis: "documented: all other documented info requests have weight 20; fundingHistory has an additional rate limit weight per 20 items returned; responses that take a time range only return 500 elements"},
	"allMids": {Type: "allMids", Base: 2,
		Basis: "documented: allMids is in the weight-2 info list"},
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
	"A5: the 5000-candle and 500-element statements cap one response",
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

func hlCandleRowBound(interval string, startMs, endMs int64) (int, error) {
	ms, err := hlCandleBoundIntervalMs(interval)
	if err != nil {
		return 0, err
	}
	if endMs < startMs {
		return 0, fmt.Errorf("candle request end %d is before its start %d", endMs, startMs)
	}
	bars := (endMs-startMs)/ms + 2
	if bars > hlCandleRowCap || bars < 0 {
		return hlCandleRowCap, nil
	}
	return int(bars), nil
}

func hlCandleWindowFits(interval string, startMs, endMs int64) bool {
	ms, err := hlCandleBoundIntervalMs(interval)
	if err != nil || endMs < startMs {
		return true
	}
	return (endMs-startMs)/ms+2 <= hlCandleRowCap
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

func (l *feedRequestLedger) weightReport(outstanding []string) []feedWeightEvent {
	if l == nil {
		return nil
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
	if !ep.successAfter || len(outstanding) > 0 {
		return out
	}
	out = append(out, feedWeightEvent{Kind: feedWeightEventRecover, OpenedAt: ep.openedAt, Inputs: sortedFeedSet(ep.inputs), Refused: ep.refused})
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
		return fmt.Sprintf("**MARKET FEED WEIGHT RECOVERED** [%s] a required read succeeded and no weight-refused input is outstanding (refused inputs %s; %d refused request(s); instance %s).",
			source, joinFeedNames(ev.Inputs), ev.Refused, instance)
	}
	return fmt.Sprintf("**MARKET FEED WEIGHT REFUSED** [%s] this owner's venue weight allocation refused required reads. Refused inputs: %s. Consumers hold entries on those inputs; protection continues on verified inputs (instance %s).",
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
	scalar, records := 0, 0
	ctxW, _ := hlInfoWeight("metaAndAssetCtxs", 0)
	avgRows, _ := hlFundingRowBound(0, int64(hlFundingAvgWindowDays)*86_400_000)
	avgW, _ := hlInfoWeight("fundingHistory", avgRows)
	pageW, _ := hlInfoWeightAtCap("fundingHistory")
	for _, need := range union.Funding {
		if need.Scalar {
			scalar += ctxW + avgW
			largest = max(largest, ctxW, avgW)
		}
		if need.Records {
			records++
			largest = max(largest, pageW)
		}
	}
	return fmt.Sprintf("weight estimate (informational, first passes only): bootstrap=%d for %d keys; refresh=%d per deadline for candles plus mids=%d; funding scalar=%d per refresh; funding records for %d coin(s) and accounting funding at up to %d per page; largest_read=%d",
		bootstrap, len(union.Order), refresh, mids, scalar, records, pageW, largest)
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
