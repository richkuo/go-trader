package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	paperFundingSampleEvery     = 5 * time.Minute
	paperFundingMinSpacing      = 30 * time.Minute
	paperFundingGapSpacing      = 90 * time.Minute
	paperFundingLagAlertAfter   = 2 * time.Hour
	paperFundingRESTInterval    = 5 * time.Minute
	paperFundingRESTMaxPasses   = 8
	paperFundingOrderIDPrefix   = "paper_funding:"
	paperFundingHeldUnknown     = "unknown_window"
	paperFundingHeldNonFinite   = "non_finite_payment"
	paperFundingSourceSealed    = "sealed"
	paperFundingSourceREST      = "rest"
	paperFundingBacklogRemedy   = "run the consumer once with market_feed \"rest\" (restart required) to settle it through bounded REST pages"
	paperFundingConditionPrefix = "**PAPER FUNDING**"
	paperFundingCriticalPrefix  = "**PAPER FUNDING CRITICAL**"
)

type paperFundingTradeLookup interface {
	HasTradeWithExchangeOrderID(strategyID, exchangeOrderID string) (bool, error)
}

type paperFundingInputs struct {
	Source    string
	Coverage  map[string]feedFundingCoverage
	Detail    map[string]string
	NotServed map[string]bool
}

type paperFundingRunResult struct {
	Alerts []string
	Logs   []string
	Booked int
}

var paperFundingEpisodes = make(map[string]bool)

func paperFundingOrderID(coin string, tMs int64) string {
	return fmt.Sprintf("%s%s:%d", paperFundingOrderIDPrefix, coin, tMs)
}

func (c *paperFundingCoin) exposureNonzero() bool {
	return c.Anchor.SignedQty != 0 || len(c.Segments) > 0
}

func (c *paperFundingCoin) openGaps() []paperFundingGap {
	var out []paperFundingGap
	for _, g := range c.Gaps {
		if !g.Expired {
			out = append(out, g)
		}
	}
	return out
}

func (c *paperFundingCoin) needsCoverage() bool {
	if c.exposureNonzero() || len(c.openGaps()) > 0 {
		return true
	}
	for _, u := range c.Unknown {
		if u.ToMs > c.SettledThroughMs {
			return true
		}
	}
	return false
}

func (c *paperFundingCoin) coverageSince() int64 {
	since := c.SettledThroughMs + 1
	for _, g := range c.openGaps() {
		if g.AfterMs+1 < since {
			since = g.AfterMs + 1
		}
	}
	return since
}

func paperFundingOverlaps(aFrom, aTo, bFrom, bTo int64) bool {
	return aFrom < bTo && bFrom < aTo
}

func (c *paperFundingCoin) exposureNonzeroIn(fromMs, toMs int64) bool {
	if toMs <= fromMs {
		return false
	}
	for _, seg := range c.Segments {
		if paperFundingOverlaps(seg.FromMs, seg.ToMs, fromMs, toMs) {
			return true
		}
	}
	for _, u := range c.Unknown {
		if paperFundingOverlaps(u.FromMs, u.ToMs, fromMs, toMs) {
			return true
		}
	}
	return c.Anchor.SignedQty != 0 && toMs > c.Anchor.SinceMs
}

type paperFundingExposure int

const (
	paperFundingExposureKnown paperFundingExposure = iota
	paperFundingExposureUnknown
	paperFundingExposureUnverified
)

func (c *paperFundingCoin) exposureAt(tMs int64) (float64, string, paperFundingExposure) {
	for _, u := range c.Unknown {
		if u.FromMs < tMs && tMs <= u.ToMs {
			return 0, "", paperFundingExposureUnknown
		}
	}
	for _, seg := range c.Segments {
		if seg.FromMs < tMs && tMs <= seg.ToMs {
			return seg.SignedQty, seg.PositionID, paperFundingExposureKnown
		}
	}
	a := c.Anchor
	if tMs > a.VerifiedMs {
		return 0, "", paperFundingExposureUnverified
	}
	if a.SinceMs < tMs {
		return a.SignedQty, a.PositionID, paperFundingExposureKnown
	}
	return 0, "", paperFundingExposureKnown
}

func (c *paperFundingCoin) sampleAtOrAfter(tMs int64) (paperFundingSample, bool) {
	i := sort.Search(len(c.Samples), func(i int) bool { return c.Samples[i].AtMs >= tMs })
	if i >= len(c.Samples) {
		return paperFundingSample{}, false
	}
	return c.Samples[i], true
}

func (c *paperFundingCoin) maybeSample(px float64, nowMs int64) {
	if !(px > 0) || math.IsInf(px, 0) {
		return
	}
	if n := len(c.Samples); n > 0 {
		last := c.Samples[n-1]
		hourStart := nowMs - nowMs%time.Hour.Milliseconds()
		if last.AtMs >= nowMs || (last.AtMs >= hourStart && nowMs-last.AtMs < paperFundingSampleEvery.Milliseconds()) {
			return
		}
	}
	c.Samples = append(c.Samples, paperFundingSample{AtMs: nowMs, Px: px})
}

func (c *paperFundingCoin) prune(nowMs int64) []string {
	var logs []string
	for i := range c.Gaps {
		if !c.Gaps[i].Expired && nowMs-c.Gaps[i].BeforeMs > feedAccountingWindow.Milliseconds() {
			c.Gaps[i].Expired = true
			logs = append(logs, fmt.Sprintf("gap (%d, %d) is older than the accounting window and stays recorded with no booking", c.Gaps[i].AfterMs, c.Gaps[i].BeforeMs))
		}
	}
	cutoff := c.SettledThroughMs
	for _, g := range c.openGaps() {
		if g.AfterMs < cutoff {
			cutoff = g.AfterMs
		}
	}
	keptSeg := c.Segments[:0]
	for _, seg := range c.Segments {
		if seg.ToMs > cutoff {
			keptSeg = append(keptSeg, seg)
		}
	}
	c.Segments = keptSeg
	keptUnknown := c.Unknown[:0]
	for _, u := range c.Unknown {
		if u.ToMs > cutoff {
			keptUnknown = append(keptUnknown, u)
		}
	}
	c.Unknown = keptUnknown
	keptSamples := c.Samples[:0]
	for _, smp := range c.Samples {
		if smp.AtMs > cutoff {
			keptSamples = append(keptSamples, smp)
		}
	}
	c.Samples = keptSamples
	if len(c.Segments) == 0 {
		c.Segments = nil
	}
	if len(c.Unknown) == 0 {
		c.Unknown = nil
	}
	if len(c.Samples) == 0 {
		c.Samples = nil
	}
	return logs
}

func planPaperFunding(state *AppState, cfgs []StrategyConfig) map[string]int64 {
	need := make(map[string]int64)
	if state == nil {
		return need
	}
	for _, sc := range cfgs {
		if !paperFundingEligible(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil || s.PaperFunding == nil {
			continue
		}
		symbols := make(map[string]bool)
		for sym, c := range s.PaperFunding.Coins {
			if c != nil && c.needsCoverage() {
				symbols[sym] = true
			}
		}
		for sym, pos := range s.Positions {
			if pos != nil && pos.Quantity > 0 {
				symbols[sym] = true
			}
		}
		for sym := range symbols {
			since := s.PaperFunding.ObservedThroughMs + 1
			if c := s.PaperFunding.Coins[sym]; c != nil {
				since = c.coverageSince()
			}
			if cur, ok := need[sym]; !ok || since < cur {
				need[sym] = since
			}
		}
	}
	return need
}

type paperFundingRESTEntry struct {
	cov       feedFundingCoverage
	fetchedAt time.Time
	err       string
}

var (
	paperFundingRESTMu    sync.Mutex
	paperFundingRESTCache = make(map[string]*paperFundingRESTEntry)
)

func gatherPaperFundingInputs(ctx context.Context, feedMode bool, snap *marketSnapshot, need map[string]int64, now time.Time) paperFundingInputs {
	in := paperFundingInputs{Coverage: make(map[string]feedFundingCoverage), Detail: make(map[string]string), NotServed: make(map[string]bool)}
	coins := make([]string, 0, len(need))
	for c := range need {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	if feedMode {
		in.Source = paperFundingSourceSealed
		for _, coin := range coins {
			cov, detail, ok := snap.accountingCoverage(coin)
			if ok {
				in.Coverage[coin] = cov
			} else if snap != nil && snap.accountingFunding != nil {
				in.NotServed[coin] = true
			}
			if detail != "" {
				in.Detail[coin] = detail
			}
		}
		return in
	}
	in.Source = paperFundingSourceREST
	for _, coin := range coins {
		since := need[coin]
		paperFundingRESTMu.Lock()
		cached := paperFundingRESTCache[coin]
		paperFundingRESTMu.Unlock()
		if cached != nil && cached.err == "" && now.Sub(cached.fetchedAt) < paperFundingRESTInterval && cached.cov.FromMs <= since {
			in.Coverage[coin] = cached.cov
			continue
		}
		cov, err := hlFundingRecordsCoverage(ctx, coin, since, now, paperFundingRESTMaxPasses)
		entry := &paperFundingRESTEntry{cov: cov, fetchedAt: now}
		if err != nil {
			entry.err = err.Error()
			in.Detail[coin] = fmt.Sprintf("funding history fetch failed: %v", err)
			if cached != nil && cached.err == "" && cached.cov.FromMs <= since {
				in.Coverage[coin] = cached.cov
				entry.cov = cached.cov
			}
		} else {
			in.Coverage[coin] = cov
		}
		paperFundingRESTMu.Lock()
		paperFundingRESTCache[coin] = entry
		paperFundingRESTMu.Unlock()
	}
	return in
}

type paperFundingRun struct {
	sc      StrategyConfig
	s       *StrategyState
	st      *paperFundingState
	part    RiskPartition
	store   paperFundingTradeLookup
	nowMs   int64
	res     *paperFundingRunResult
	current map[string]bool
}

func (r *paperFundingRun) condition(coin, kind string, critical bool, format string, args ...any) {
	key := r.sc.ID + "|" + coin + "|" + kind
	r.current[key] = true
	if paperFundingEpisodes[key] {
		return
	}
	paperFundingEpisodes[key] = true
	r.event(coin, kind, critical, format, args...)
}

func (r *paperFundingRun) event(coin, kind string, critical bool, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	prefix := paperFundingConditionPrefix
	level := "WARN"
	if critical {
		prefix = paperFundingCriticalPrefix
		level = "CRITICAL"
	}
	r.res.Logs = append(r.res.Logs, fmt.Sprintf("[%s] [paper-funding] %s %s %s: %s", level, r.sc.ID, coin, kind, detail))
	r.res.Alerts = append(r.res.Alerts, partitionPrefixedDM(r.part, fmt.Sprintf("%s [%s] %s %s: %s", prefix, r.sc.ID, coin, kind, detail)))
}

func (r *paperFundingRun) lagging(c *paperFundingCoin) bool {
	lagStart := r.nowMs - paperFundingLagAlertAfter.Milliseconds()
	return lagStart > c.SettledThroughMs && c.exposureNonzeroIn(c.SettledThroughMs, lagStart)
}

type paperFundingOutcome int

const (
	paperFundingSettled paperFundingOutcome = iota
	paperFundingStop
)

func (r *paperFundingRun) bookRecord(coin string, c *paperFundingCoin, rec feedFundingRecord) paperFundingOutcome {
	qty, pid, kind := c.exposureAt(rec.TimeMs)
	switch kind {
	case paperFundingExposureUnverified:
		return paperFundingStop
	case paperFundingExposureUnknown:
		c.Held = append(c.Held, paperFundingHeld{TimeMs: rec.TimeMs, Rate: rec.Rate, Reason: paperFundingHeldUnknown})
		r.event(coin, "held_unknown_window", true, "funding record at %d (rate %.10g) falls inside an unknown inventory window; it is held and never guessed", rec.TimeMs, rec.Rate)
		return paperFundingSettled
	}
	if qty == 0 || rec.Rate == 0 {
		return paperFundingSettled
	}
	smp, ok := c.sampleAtOrAfter(rec.TimeMs)
	if !ok {
		if r.nowMs-rec.TimeMs > paperFundingLagAlertAfter.Milliseconds() {
			r.condition(coin, "mark_missing", false, "no valuation mark at or after funding record %d; settlement waits for a mark", rec.TimeMs)
		}
		return paperFundingStop
	}
	payment := -qty * smp.Px * rec.Rate
	if !paperFundingFinite(payment) {
		c.Held = append(c.Held, paperFundingHeld{TimeMs: rec.TimeMs, Rate: rec.Rate, Reason: paperFundingHeldNonFinite, SignedQty: qty, PositionID: pid})
		r.event(coin, "held_non_finite", true, "funding record at %d produced a non-finite payment (qty %.10g, mark %.10g, rate %.10g); it is held", rec.TimeMs, qty, smp.Px, rec.Rate)
		return paperFundingSettled
	}
	key := paperFundingOrderID(coin, rec.TimeMs)
	for i := len(r.s.TradeHistory) - 1; i >= 0; i-- {
		if r.s.TradeHistory[i].ExchangeOrderID == key {
			r.event(coin, "dedup_inconsistency", true, "funding row %s is already in memory although progress had not passed it; treated as settled with no cash change", key)
			return paperFundingSettled
		}
	}
	if r.store != nil {
		exists, err := r.store.HasTradeWithExchangeOrderID(r.s.ID, key)
		if err != nil {
			r.condition(coin, "storage_error", false, "dedup lookup for %s failed: %v; settlement stops with no progress", key, err)
			return paperFundingStop
		}
		if exists {
			r.event(coin, "dedup_inconsistency", true, "funding row %s is already stored although progress had not passed it; treated as settled with no cash change", key)
			return paperFundingSettled
		}
	}
	lag := smp.AtMs - rec.TimeMs
	r.s.Cash += payment
	RecordTrade(r.s, Trade{
		Timestamp:       time.UnixMilli(rec.TimeMs).UTC(),
		StrategyID:      r.s.ID,
		Symbol:          coin,
		PositionID:      pid,
		Side:            "funding",
		TradeType:       TradeTypeFunding,
		Details:         fmt.Sprintf("Paper funding payment $%+.6f on %s: qty %.10g x mark %.6f (sampled at %d, lag %dms) x rate %.10g at %d", payment, coin, qty, smp.Px, smp.AtMs, lag, rec.Rate, rec.TimeMs),
		ExchangeOrderID: key,
		RealizedPnL:     payment,
		PnLGross:        true,
	})
	r.res.Booked++
	return paperFundingSettled
}

func (r *paperFundingRun) resolveGaps(coin string, c *paperFundingCoin, cov feedFundingCoverage) {
	if len(c.openGaps()) == 0 || len(cov.Records) == 0 {
		return
	}
	for _, rec := range cov.Records {
		if rec.TimeMs > c.SettledThroughMs {
			break
		}
		inGap := false
		for _, g := range c.openGaps() {
			if g.AfterMs < rec.TimeMs && rec.TimeMs < g.BeforeMs && cov.FromMs <= rec.TimeMs {
				inGap = true
				break
			}
		}
		if !inGap {
			continue
		}
		if r.bookRecord(coin, c, rec) == paperFundingStop {
			return
		}
		r.res.Logs = append(r.res.Logs, fmt.Sprintf("[paper-funding] %s %s: late funding record at %d inside a recorded gap settled once", r.sc.ID, coin, rec.TimeMs))
		kept := c.Gaps[:0]
		for _, g := range c.Gaps {
			if !g.Expired && g.AfterMs < rec.TimeMs && rec.TimeMs < g.BeforeMs {
				if g.AfterMs < rec.TimeMs && rec.TimeMs-g.AfterMs > paperFundingGapSpacing.Milliseconds() {
					kept = append(kept, paperFundingGap{AfterMs: g.AfterMs, BeforeMs: rec.TimeMs, DetectedMs: g.DetectedMs})
				}
				if g.BeforeMs-rec.TimeMs > paperFundingGapSpacing.Milliseconds() {
					kept = append(kept, paperFundingGap{AfterMs: rec.TimeMs, BeforeMs: g.BeforeMs, DetectedMs: g.DetectedMs})
				}
				continue
			}
			kept = append(kept, g)
		}
		c.Gaps = kept
	}
}

func (c *paperFundingCoin) firstNonzeroAfter(fromMs, toMs int64) (int64, bool) {
	best, found := int64(0), false
	consider := func(start, end int64) {
		if !paperFundingOverlaps(start, end, fromMs, toMs) {
			return
		}
		if start < fromMs {
			start = fromMs
		}
		if !found || start < best {
			best, found = start, true
		}
	}
	for _, seg := range c.Segments {
		consider(seg.FromMs, seg.ToMs)
	}
	for _, u := range c.Unknown {
		consider(u.FromMs, u.ToMs)
	}
	if c.Anchor.SignedQty != 0 {
		consider(c.Anchor.SinceMs, math.MaxInt64)
	}
	return best, found
}

func (r *paperFundingRun) settleCoin(coin string, c *paperFundingCoin, cov feedFundingCoverage, covOK bool, detail string, notServed bool) {
	if !c.needsCoverage() {
		if c.Anchor.VerifiedMs > c.SettledThroughMs {
			c.SettledThroughMs = c.Anchor.VerifiedMs
		}
		return
	}
	if !covOK {
		if notServed {
			r.condition(coin, "not_served", false, "the shared market feed does not serve accounting funding for this coin (%s); settlement waits; reload the feed with this consumer's config", detail)
		} else if r.lagging(c) {
			r.condition(coin, "records_unavailable", false, "funding records are unavailable (%s); settlement is pending and retries every cycle", detail)
		}
		return
	}
	if err := validateFundingCoverageRecords(coin, cov.FromMs, cov.ToMs, cov.Records); err != nil {
		r.condition(coin, "anomalous_records", true, "funding coverage refused: %v; settlement stops with no progress", err)
		return
	}
	r.resolveGaps(coin, c, cov)
	if cov.FromMs > c.SettledThroughMs+1 {
		if c.exposureNonzeroIn(c.SettledThroughMs, cov.FromMs-1) {
			r.condition(coin, "backlog", false, "unsettled exposure since %d is older than the funding coverage start %d; it stays pending; %s", c.SettledThroughMs, cov.FromMs, paperFundingBacklogRemedy)
			return
		}
		c.SettledThroughMs = cov.FromMs - 1
	}
	limit := cov.ToMs
	if c.Anchor.VerifiedMs < limit {
		limit = c.Anchor.VerifiedMs
	}
	stopped := false
	for _, rec := range cov.Records {
		if rec.TimeMs <= c.SettledThroughMs {
			continue
		}
		if rec.TimeMs > limit {
			break
		}
		prev := c.LastRecordMs
		if prev > 0 && rec.TimeMs > prev && rec.TimeMs-prev < paperFundingMinSpacing.Milliseconds() {
			r.condition(coin, "anomalous_records", true, "funding records at %d and %d are less than %s apart; settlement stops with no progress", prev, rec.TimeMs, paperFundingMinSpacing)
			stopped = true
			break
		}
		ref := prev
		if ref <= 0 {
			ref = c.SettledThroughMs
		}
		if start, held := c.firstNonzeroAfter(ref, rec.TimeMs-1); held && rec.TimeMs-start > paperFundingGapSpacing.Milliseconds() {
			c.Gaps = append(c.Gaps, paperFundingGap{AfterMs: start, BeforeMs: rec.TimeMs, DetectedMs: r.nowMs})
			r.event(coin, "gap", false, "no funding record between %d and %d while a position was held; the gap is recorded, never booked as zero, and a late record inside it books once", start, rec.TimeMs)
		}
		if r.bookRecord(coin, c, rec) == paperFundingStop {
			stopped = true
			break
		}
		c.SettledThroughMs = rec.TimeMs
		if rec.TimeMs > c.LastRecordMs {
			c.LastRecordMs = rec.TimeMs
		}
	}
	if !stopped && limit > c.SettledThroughMs {
		c.SettledThroughMs = limit
	}
	if detail != "" && r.lagging(c) {
		r.condition(coin, "records_unavailable", false, "funding coverage refresh reported: %s; settlement is pending", detail)
	} else if r.lagging(c) && !stopped {
		r.condition(coin, "records_unavailable", false, "no funding record has arrived for unsettled exposure older than %s; settlement is pending", paperFundingLagAlertAfter)
	}
}

func runPaperFundingAccounting(state *AppState, cfgs []StrategyConfig, store paperFundingTradeLookup, inputs paperFundingInputs, marks map[string]float64, blocked func(RiskPartition) bool, now time.Time) paperFundingRunResult {
	res := paperFundingRunResult{}
	if state == nil {
		return res
	}
	defer suspendEagerTradePersist()()
	nowMs := now.UTC().UnixMilli()
	ordered := append([]StrategyConfig(nil), cfgs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	current := make(map[string]bool)
	processed := make(map[string]bool)
	for _, sc := range ordered {
		if !paperFundingEligible(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		part := partitionFor(sc)
		run := &paperFundingRun{sc: sc, s: s, st: s.PaperFunding, part: part, store: store, nowMs: nowMs, res: &res, current: current}
		if s.paperFundingCorrupt {
			processed[sc.ID] = true
			run.condition("*", "corrupt_state", true, "stored paper funding state is unreadable; accounting is held and the stored text is kept unchanged")
			continue
		}
		st := s.PaperFunding
		if st == nil {
			continue
		}
		observePaperFunding(s, nowMs)
		syms := make([]string, 0, len(st.Coins))
		for sym := range st.Coins {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			c := st.Coins[sym]
			if c.exposureNonzero() || len(c.openGaps()) > 0 {
				c.maybeSample(marks[sym], nowMs)
			}
		}
		if blocked != nil && blocked(part) {
			for _, a := range st.drainAlerts() {
				run.event(a.Coin, a.Kind, a.Critical, "%s", a.Detail)
			}
			continue
		}
		processed[sc.ID] = true
		for _, sym := range syms {
			c := st.Coins[sym]
			cov, ok := inputs.Coverage[sym]
			run.settleCoin(sym, c, cov, ok, inputs.Detail[sym], inputs.NotServed[sym])
			for _, line := range c.prune(nowMs) {
				res.Logs = append(res.Logs, fmt.Sprintf("[paper-funding] %s %s: %s", sc.ID, sym, line))
			}
			_, hasPos := s.Positions[sym]
			if !hasPos && c.Anchor.SignedQty == 0 && len(c.Segments) == 0 && len(c.Unknown) == 0 && len(c.Held) == 0 && len(c.Gaps) == 0 {
				delete(st.Coins, sym)
			}
		}
		for _, a := range st.drainAlerts() {
			run.event(a.Coin, a.Kind, a.Critical, "%s", a.Detail)
		}
	}
	for key := range paperFundingEpisodes {
		id := strings.SplitN(key, "|", 2)[0]
		if processed[id] && !current[key] {
			delete(paperFundingEpisodes, key)
			res.Logs = append(res.Logs, fmt.Sprintf("[paper-funding] %s cleared", strings.ReplaceAll(key, "|", " ")))
		}
	}
	sort.Strings(res.Logs)
	return res
}

func runPaperFundingCycle(ctx context.Context, state *AppState, cfg *Config, store *StateStore, mu *sync.RWMutex, feedMode bool, snap *marketSnapshot, marks map[string]float64, notifier *MultiNotifier) {
	if cfg == nil || state == nil {
		return
	}
	mu.RLock()
	need := planPaperFunding(state, cfg.Strategies)
	mu.RUnlock()
	now := paperFundingClock()
	inputs := gatherPaperFundingInputs(ctx, feedMode, snap, need, now)
	mu.Lock()
	res := runPaperFundingAccounting(state, cfg.Strategies, store, inputs, marks, func(p RiskPartition) bool {
		return partitionSaveBlocked(store, p)
	}, paperFundingClock())
	mu.Unlock()
	for _, line := range res.Logs {
		fmt.Println(line)
	}
	if res.Booked > 0 {
		fmt.Printf("[paper-funding] booked %d funding row(s) from %s coverage\n", res.Booked, inputs.Source)
	}
	if notifier == nil || !notifier.HasOwner() {
		return
	}
	for _, msg := range res.Alerts {
		notifier.SendOwnerDM(msg)
	}
}
