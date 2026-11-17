package main

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	feedRateLimitPause         = feedBudgetWindow
	feedRetryAfterMaxBytes     = 64
	feedAlertSenderDepth       = 64
	feedAlertSenderDrainBound  = 10 * time.Second
	feedRateLimitStatePaused   = "paused"
	feedRateLimitStateRecovery = "recovering"
	feedRateLimitStateClear    = "clear"
	feedRateLimitEventPaused   = "paused"
	feedRateLimitEventRecover  = "recovered"
)

var errFeedHostPaused = errors.New("venue host paused after a rate limit")

type feedHostPausedError struct {
	Host  string
	Until time.Time
}

func (e *feedHostPausedError) Error() string {
	return fmt.Sprintf("%v: %s until %s", errFeedHostPaused, e.Host, e.Until.UTC().Format(time.RFC3339))
}

func (e *feedHostPausedError) Unwrap() error { return errFeedHostPaused }

func feedRateLimitErr(err error) bool {
	return errors.Is(err, errHLInfoRateLimited) || errors.Is(err, errFeedHostPaused)
}

func feedHostOf(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

type feedHostPause struct {
	host           string
	until          time.Time
	inflight       int
	lastRetryAfter string
	lastRateLimit  time.Time
	episode        bool
	openedAt       time.Time
	extensions     int
	refused        int
	refusedInputs  map[string]bool
	pauseAlerted   bool
	successAfter   bool
}

type feedRateLimitHostStatus struct {
	Host           string   `json:"host"`
	State          string   `json:"state"`
	PausedUntil    string   `json:"paused_until,omitempty"`
	OpenedAt       string   `json:"opened_at,omitempty"`
	Extensions     int      `json:"extensions"`
	Refused        int      `json:"refused"`
	RefusedInputs  []string `json:"refused_inputs"`
	InFlight       int      `json:"in_flight"`
	LastRetryAfter string   `json:"last_retry_after,omitempty"`
	LastRateLimit  string   `json:"last_rate_limit,omitempty"`
}

type feedRateLimitEvent struct {
	Kind       string
	Host       string
	Until      time.Time
	OpenedAt   time.Time
	Inputs     []string
	Refused    int
	Extensions int
	RetryAfter string
}

const (
	feedPermitPending = iota + 1
	feedPermitInFlight
	feedPermitClosed
)

type feedRequestPermit struct {
	l           *feedRequestLedger
	host        string
	reason      string
	reqType     string
	input       string
	began       time.Time
	sending     bool
	done        bool
	cost        int
	startupPart int
	epoch       int
	wState      int
	atCap       bool
}

func (p *feedRequestPermit) rowBound(callBound int) int {
	if p == nil || !p.atCap {
		return callBound
	}
	if c := hlInfoRowCap(p.reqType); c > 0 {
		return c
	}
	return callBound
}

func (l *feedRequestLedger) hostLocked(host string) *feedHostPause {
	p := l.hosts[host]
	if p == nil {
		p = &feedHostPause{host: host, refusedInputs: map[string]bool{}}
		l.hosts[host] = p
	}
	return p
}

func (l *feedRequestLedger) pauseRefusalLocked(host, reason, input string, now time.Time) error {
	p := l.hosts[host]
	if p == nil || !now.Before(p.until) {
		return nil
	}
	p.refused++
	if input != "" {
		p.refusedInputs[input] = true
	}
	l.totals.Paused++
	l.totals.PausedByReason[reason]++
	return &feedHostPausedError{Host: host, Until: p.until}
}

func (p *feedRequestPermit) beginSend() error {
	if p == nil || p.l == nil {
		return nil
	}
	l := p.l
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UTC()
	if err := l.pauseRefusalLocked(p.host, p.reason, p.input, now); err != nil {
		p.done = true
		l.releaseLocked(p)
		return err
	}
	if refusal := l.revalidateLocked(p, now); refusal != nil {
		p.done = true
		l.releaseLocked(p)
		spec := feedAcquireSpec{host: p.host, reason: p.reason, reqType: p.reqType, input: p.input}
		l.countWeightRefusalLocked(spec, refusal.Cause, now)
		return refusal
	}
	if p.wState == feedPermitPending {
		l.wPending -= p.cost
		l.wInFlight += p.cost
		p.wState = feedPermitInFlight
	}
	l.hostLocked(p.host).inflight++
	p.began = now
	p.sending = true
	return nil
}

func (p *feedRequestPermit) finish(status int, retryAfter string) {
	if p == nil || p.l == nil || !p.sending || p.done {
		return
	}
	p.done = true
	l := p.l
	l.mu.Lock()
	now := l.clock().UTC()
	h := l.hostLocked(p.host)
	if h.inflight > 0 {
		h.inflight--
	}
	if status != 200 {
		l.chargeLocked(p, now, p.cost)
	}
	var line string
	switch {
	case status == 429:
		l.totals.RateLimited++
		line = l.rateLimitedLocked(h, now, p, retryAfter)
	case status == 200 && h.episode && !now.Before(h.until) && !p.began.Before(h.until):
		h.successAfter = true
		if l.closeOnSuccess {
			line = fmt.Sprintf("[feed-ratelimit] host=%s status=recovered opened_at=%s extensions=%d refused=%d refused_inputs=%s (no alert route for this ledger)",
				h.host, h.openedAt.Format(time.RFC3339), h.extensions, h.refused, joinFeedNames(sortedFeedSet(h.refusedInputs)))
			l.closeEpisodeLocked(h)
		}
	}
	logf := l.logf
	l.mu.Unlock()
	if line != "" && logf != nil {
		logf("%s", line)
	}
}

func (l *feedRequestLedger) rateLimitedLocked(h *feedHostPause, now time.Time, p *feedRequestPermit, retryAfter string) string {
	next := now.Add(feedRateLimitPause)
	if next.After(h.until) {
		h.until = next
	}
	h.lastRateLimit = now
	h.lastRetryAfter = quoteFeedRetryAfter(retryAfter)
	status := "paused"
	if !h.episode {
		h.episode = true
		h.openedAt = now
		h.extensions = 0
		h.refused = 0
		h.refusedInputs = map[string]bool{}
		h.pauseAlerted = false
	} else {
		h.extensions++
		status = "extended"
	}
	h.successAfter = false
	if p.input != "" {
		h.refusedInputs[p.input] = true
	}
	return fmt.Sprintf("[feed-ratelimit] host=%s status=%s until=%s reason=%s type=%s input=%s extensions=%d in_flight=%d retry_after=%s",
		h.host, status, h.until.Format(time.RFC3339), p.reason, p.reqType, p.input, h.extensions, h.inflight, h.lastRetryAfter)
}

func (l *feedRequestLedger) closeEpisodeLocked(h *feedHostPause) {
	h.episode = false
	h.pauseAlerted = false
	h.successAfter = false
	h.extensions = 0
	h.refused = 0
	h.refusedInputs = map[string]bool{}
	h.openedAt = time.Time{}
}

func quoteFeedRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) > feedRetryAfterMaxBytes {
		v = v[:feedRetryAfterMaxBytes]
	}
	return strconv.Quote(v)
}

func (l *feedRequestLedger) pausedUntil(host string) time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.hosts[host]; p != nil {
		return p.until
	}
	return time.Time{}
}

func (l *feedRequestLedger) rateLimitStatusLocked(now time.Time) []feedRateLimitHostStatus {
	hosts := make([]string, 0, len(l.hosts))
	for h := range l.hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	out := make([]feedRateLimitHostStatus, 0, len(hosts))
	for _, host := range hosts {
		p := l.hosts[host]
		st := feedRateLimitHostStatus{
			Host:           host,
			State:          feedRateLimitStateClear,
			Extensions:     p.extensions,
			Refused:        p.refused,
			RefusedInputs:  sortedFeedSet(p.refusedInputs),
			InFlight:       p.inflight,
			LastRetryAfter: p.lastRetryAfter,
		}
		if !p.lastRateLimit.IsZero() {
			st.LastRateLimit = p.lastRateLimit.Format(time.RFC3339)
		}
		if p.episode {
			st.State = feedRateLimitStateRecovery
			st.OpenedAt = p.openedAt.Format(time.RFC3339)
			st.PausedUntil = p.until.Format(time.RFC3339)
		}
		if now.Before(p.until) {
			st.State = feedRateLimitStatePaused
			st.PausedUntil = p.until.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}

func (l *feedRequestLedger) rateLimitStatus() []feedRateLimitHostStatus {
	if l == nil {
		return []feedRateLimitHostStatus{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rateLimitStatusLocked(l.clock().UTC())
}

func (l *feedRequestLedger) rateLimitReport(outstanding map[string][]string) []feedRateLimitEvent {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UTC()
	hosts := make([]string, 0, len(l.hosts))
	for h := range l.hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	var out []feedRateLimitEvent
	for _, host := range hosts {
		p := l.hosts[host]
		if !p.episode {
			continue
		}
		if !p.pauseAlerted {
			p.pauseAlerted = true
			inputs := make(map[string]bool, len(p.refusedInputs))
			for k := range p.refusedInputs {
				inputs[k] = true
			}
			for _, k := range outstanding[host] {
				inputs[k] = true
			}
			out = append(out, feedRateLimitEvent{
				Kind: feedRateLimitEventPaused, Host: host, Until: p.until, OpenedAt: p.openedAt,
				Inputs: sortedFeedSet(inputs), Refused: p.refused, Extensions: p.extensions, RetryAfter: p.lastRetryAfter,
			})
		}
		if now.Before(p.until) || !p.successAfter || len(outstanding[host]) > 0 {
			continue
		}
		out = append(out, feedRateLimitEvent{
			Kind: feedRateLimitEventRecover, Host: host, Until: p.until, OpenedAt: p.openedAt,
			Inputs: sortedFeedSet(p.refusedInputs), Refused: p.refused, Extensions: p.extensions, RetryAfter: p.lastRetryAfter,
		})
		l.closeEpisodeLocked(p)
	}
	return out
}

func sortedFeedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatFeedRateLimitStatus(hosts []feedRateLimitHostStatus) string {
	if len(hosts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		part := h.Host + ":" + h.State
		if h.PausedUntil != "" {
			part += ":until=" + h.PausedUntil
		}
		if h.State != feedRateLimitStateClear {
			part += fmt.Sprintf(":refused=%d:inputs=%s", h.Refused, joinFeedNames(h.RefusedInputs))
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func (ev feedRateLimitEvent) logLine() string {
	return fmt.Sprintf("host=%s status=%s until=%s opened_at=%s extensions=%d refused=%d refused_inputs=%s retry_after=%s",
		ev.Host, ev.Kind, ev.Until.UTC().Format(time.RFC3339), ev.OpenedAt.UTC().Format(time.RFC3339), ev.Extensions, ev.Refused,
		joinFeedNames(ev.Inputs), ev.RetryAfter)
}

func formatFeedRateLimitAlert(ev feedRateLimitEvent, source, instance string) string {
	if ev.Kind == feedRateLimitEventRecover {
		return fmt.Sprintf("**MARKET FEED RATE LIMIT RECOVERED** [%s] host %s: the pause ended at %s, a read succeeded after it, and every refused input that is still required has recovered (refused inputs %s; %d refused request(s), %d extension(s); instance %s).",
			source, ev.Host, ev.Until.UTC().Format(time.RFC3339), joinFeedNames(ev.Inputs), ev.Refused, ev.Extensions, instance)
	}
	return fmt.Sprintf("**MARKET FEED RATE LIMITED** [%s] host %s answered HTTP 429; every feed read to this host waits until %s. Refused inputs: %s. Consumers hold entries on those inputs; protection continues on verified inputs (instance %s).",
		source, ev.Host, ev.Until.UTC().Format(time.RFC3339), joinFeedNames(ev.Inputs), instance)
}

type feedAlertSender struct {
	ch      chan string
	pending atomic.Int64
	logf    func(string, ...any)
}

func newFeedAlertSender(send func(string), logf func(string, ...any)) *feedAlertSender {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &feedAlertSender{ch: make(chan string, feedAlertSenderDepth), logf: logf}
	go func() {
		for msg := range s.ch {
			if send != nil {
				send(msg)
			}
			s.pending.Add(-1)
		}
	}()
	return s
}

func (s *feedAlertSender) enqueue(msg string) {
	if s == nil {
		return
	}
	s.pending.Add(1)
	select {
	case s.ch <- msg:
	default:
		s.pending.Add(-1)
		s.logf("[feed-ratelimit] alert queue full (%d); dropped: %s", feedAlertSenderDepth, msg)
	}
}

func (s *feedAlertSender) drain(bound time.Duration) bool {
	if s == nil {
		return true
	}
	deadline := time.Now().Add(bound)
	for s.pending.Load() > 0 {
		if time.Now().After(deadline) {
			s.logf("[feed-ratelimit] %d alert(s) still unsent after %s", s.pending.Load(), bound)
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func (o *marketFeedOwner) beginKeyFetch() uint64 {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.fetchSeq++
	return o.fetchSeq
}

func (o *marketFeedOwner) markKeyRateLimited(key marketFeedKey, detail string) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if st := o.keys[key]; st != nil {
		st.Status = feedStatusRateLimited
		st.StatusDetail = detail
		if o.fetchSeq == 0 {
			o.fetchSeq = 1
		}
		st.rlSeq = o.fetchSeq
	}
}

func (o *marketFeedOwner) setMidsRateLimited(limited bool) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.midsRateLimited = limited
}

func (o *marketFeedOwner) rateLimitedInputs() []string {
	if o == nil {
		return nil
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	var out []string
	for key, st := range o.keys {
		if st.rlSeq != 0 {
			out = append(out, key.PayloadID())
		}
	}
	for coin, f := range o.funding {
		if f != nil && f.rateLimited {
			if need, ok := o.fundingNeeds[coin]; ok && (need.Scalar || need.Records) {
				out = append(out, "funding:"+coin)
			}
		}
	}
	for coin, f := range o.accountingFunding {
		if f != nil && f.rateLimited && o.accountingCoins[coin] {
			out = append(out, "accounting_funding:"+coin)
		}
	}
	if o.midsRateLimited && len(o.midCoins) > 0 {
		out = append(out, "mids")
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) setMidsWeightHeld(held bool) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.midsWeightHeld = held
}

func (o *marketFeedOwner) weightRefusedInputs() []string {
	if o == nil {
		return nil
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	var out []string
	for key, st := range o.keys {
		if st.weightHeld {
			out = append(out, key.PayloadID())
		}
	}
	for coin, f := range o.funding {
		if f != nil && f.weightHeld {
			if need, ok := o.fundingNeeds[coin]; ok && (need.Scalar || need.Records) {
				out = append(out, "funding:"+coin)
			}
		}
	}
	for coin, f := range o.accountingFunding {
		if f != nil && f.weightHeld && o.accountingCoins[coin] {
			out = append(out, "accounting_funding:"+coin)
		}
	}
	if o.midsWeightHeld && len(o.midCoins) > 0 {
		out = append(out, "mids")
	}
	sort.Strings(out)
	return out
}

func feedRateLimitOutstanding(o *marketFeedOwner) map[string][]string {
	return map[string][]string{feedHostOf(hlMainnetURL): o.rateLimitedInputs()}
}

func (o *marketFeedOwner) ledger() *feedRequestLedger {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.requestLedger
}

type feedRateLimitLedgerView struct {
	Name      string                    `json:"name"`
	Enforced  bool                      `json:"enforced"`
	RateLimit []feedRateLimitHostStatus `json:"rate_limit"`
	Weight    *feedWeightStatus         `json:"weight,omitempty"`
}

func schedulerFeedRateLimitView() []feedRateLimitLedgerView {
	var out []feedRateLimitLedgerView
	var feedLedger *feedRequestLedger
	if owner, _ := globalMarketFeedStatus.read(); owner != nil {
		feedLedger = owner.ledger()
	}
	if feedLedger != nil {
		st := feedLedger.status()
		out = append(out, feedRateLimitLedgerView{Name: "feed", Enforced: st.Enforced, RateLimit: st.RateLimit, Weight: &st.Weight})
	}
	if lot := hlLotMetadata.requestLedger(); lot != nil && lot != feedLedger {
		st := lot.status()
		out = append(out, feedRateLimitLedgerView{Name: "lot_metadata", Enforced: st.Enforced, RateLimit: st.RateLimit, Weight: &st.Weight})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []feedRateLimitLedgerView{}
	}
	return out
}
