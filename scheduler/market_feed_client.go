package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	feedClientDialTimeout = 2 * time.Second
	feedClientIOTimeout   = 10 * time.Second
	feedClientSlack       = 5 * time.Second

	feedFetchSealed   = "sealed"
	feedFetchDegraded = "degraded"
)

type sharedFeedEndpoint struct {
	Name   string
	Socket string
}

type feedEndpointError struct {
	Kind   string
	Detail string
}

func (e *feedEndpointError) Error() string {
	return e.Kind + ": " + e.Detail
}

const (
	feedErrTransport    = "unreachable"
	feedErrIncompatible = "incompatible"
	feedErrUnavailable  = "unavailable"
	feedErrPendingLimit = "pending past its give-up time"
	feedErrServer       = "feed error"
	feedErrMalformed    = "malformed"
)

type feedEndpointState struct {
	Name         string `json:"name"`
	Socket       string `json:"socket"`
	LastStatus   string `json:"last_status,omitempty"`
	LastDetail   string `json:"last_detail,omitempty"`
	Incompatible bool   `json:"incompatible,omitempty"`
	Instance     string `json:"instance,omitempty"`
	LastKey      int64  `json:"last_key,omitempty"`
}

type sharedFeedFetchReport struct {
	Key        int64    `json:"key"`
	Status     string   `json:"status"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Source     string   `json:"source,omitempty"`
	Instance   string   `json:"instance,omitempty"`
	Generation uint64   `json:"generation,omitempty"`
	Hash       string   `json:"hash,omitempty"`
	Bytes      int      `json:"bytes,omitempty"`
	SealedAtMs int64    `json:"sealed_at_ms,omitempty"`
	KeysTotal  int      `json:"keys_total"`
	KeysReady  int      `json:"keys_ready"`
	Gaps       []string `json:"gaps,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Attempts   []string `json:"attempts,omitempty"`
	WaitedMs   int64    `json:"waited_ms"`
	AtUnix     int64    `json:"at"`
	Alerts     []string `json:"-"`
}

type sharedFeedClient struct {
	endpoints []sharedFeedEndpoint
	dial      func(ctx context.Context, path string) (net.Conn, error)
	clock     func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error

	settle  time.Duration
	prepare time.Duration
	grace   time.Duration

	mu       sync.Mutex
	states   map[string]*feedEndpointState
	outage   bool
	lastGaps string
	last     *sharedFeedFetchReport
}

func newSharedFeedClient(cfg *Config) *sharedFeedClient {
	primary, backup := cfg.sharedMarketFeedSockets()
	eps := []sharedFeedEndpoint{{Name: "primary", Socket: primary}}
	if backup != "" {
		eps = append(eps, sharedFeedEndpoint{Name: "backup", Socket: backup})
	}
	return newSharedFeedClientWithEndpoints(eps)
}

func newSharedFeedClientWithEndpoints(eps []sharedFeedEndpoint) *sharedFeedClient {
	c := &sharedFeedClient{
		endpoints: eps,
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			d := net.Dialer{Timeout: feedClientDialTimeout}
			return d.DialContext(ctx, "unix", path)
		},
		clock:   func() time.Time { return time.Now().UTC() },
		sleep:   sleepCtx,
		settle:  feedSealSettleDelay,
		prepare: feedSealPrepareBudget,
		grace:   feedSealPublishGrace,
		states:  make(map[string]*feedEndpointState, len(eps)),
	}
	for _, ep := range eps {
		c.states[ep.Name] = &feedEndpointState{Name: ep.Name, Socket: ep.Socket}
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *sharedFeedClient) giveUpWindow() time.Duration {
	return c.settle + c.prepare + c.grace + feedClientSlack
}

func (c *sharedFeedClient) giveUpAt(key int64) time.Time {
	return time.Unix(key, 0).UTC().Add(c.giveUpWindow())
}

func sharedFeedScheduleFor(cfg *Config, client *sharedFeedClient) *sharedFeedSchedule {
	if client == nil || !cfg.marketFeedSharedEnabled() {
		return nil
	}
	return &sharedFeedSchedule{Cadences: feedConsumerCadences(cfg), Window: client.giveUpWindow()}
}

func sharedFeedSkipLines(stale map[int64][]string, logged map[int64]bool, now time.Time, cadences []int) []string {
	for k := range logged {
		if k < now.Unix()-2*86400 {
			delete(logged, k)
		}
	}
	keys := make([]int64, 0, len(stale))
	for k := range stale {
		if !logged[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		logged[k] = true
		out = append(out, fmt.Sprintf("[feed-audit] key=%d status=skipped strategies=%d ids=%s cadences=%s at=%d reason=%q",
			k, len(stale[k]), strings.Join(stale[k], ","), joinInts(cadences), now.Unix(),
			"the key passed its give-up time before this consumer evaluated it; its strategies run on the next fresh key and never on this seal"))
	}
	return out
}

func (c *sharedFeedClient) roundTrip(ctx context.Context, socket string, req feedWireRequest) (feedWireHeader, []byte, error) {
	var h feedWireHeader
	conn, err := c.dial(ctx, socket)
	if err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrTransport, Detail: err.Error()}
	}
	defer conn.Close()
	ioDeadline := c.clock().Add(feedClientIOTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(ioDeadline) {
		ioDeadline = cd
	}
	_ = conn.SetDeadline(ioDeadline)
	if err := writeFeedJSONFrame(conn, req); err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrTransport, Detail: fmt.Sprintf("send request: %v", err)}
	}
	blob, err := readFeedFrame(conn, feedWireMaxHeaderBytes)
	if err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrTransport, Detail: fmt.Sprintf("read reply: %v", err)}
	}
	var head struct {
		V              int `json:"v"`
		SealVersion    int `json:"seal_version"`
		PayloadVersion int `json:"payload_version"`
	}
	if err := json.Unmarshal(blob, &head); err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("reply header: %v", err)}
	}
	if head.V != feedWireVersion || head.SealVersion != feedSealVersion || head.PayloadVersion != marketSnapshotVersion {
		return h, nil, &feedEndpointError{Kind: feedErrIncompatible, Detail: fmt.Sprintf(
			"feed speaks wire v%d, seal v%d, payload v%d; this consumer needs wire v%d, seal v%d, payload v%d",
			head.V, head.SealVersion, head.PayloadVersion, feedWireVersion, feedSealVersion, marketSnapshotVersion)}
	}
	if err := decodeFeedStrict(blob, &h); err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("reply header: %v", err)}
	}
	if h.Status != feedWireStatusSealed {
		return h, nil, nil
	}
	if h.Bytes <= 0 || h.Bytes > feedSealMaxBytes {
		return h, nil, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("sealed reply announces %d bytes", h.Bytes)}
	}
	body, err := readFeedFrame(conn, feedSealMaxBytes)
	if err != nil {
		return h, nil, &feedEndpointError{Kind: feedErrTransport, Detail: fmt.Sprintf("read seal: %v", err)}
	}
	if len(body) != h.Bytes {
		return h, nil, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("seal is %d bytes, header announced %d", len(body), h.Bytes)}
	}
	return h, body, nil
}

func (c *sharedFeedClient) fetchFrom(ctx context.Context, ep sharedFeedEndpoint, key int64, giveUp time.Time) (*feedSealDoc, feedWireHeader, error) {
	req := feedWireRequest{V: feedWireVersion, Op: feedWireOpSnapshot, Key: key}
	for {
		h, body, err := c.roundTrip(ctx, ep.Socket, req)
		if err != nil {
			return nil, h, err
		}
		switch h.Status {
		case feedWireStatusSealed:
			if h.Key != key {
				return nil, h, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("reply for key %d, requested %d", h.Key, key)}
			}
			if got := feedSealHash(body); got != h.Hash {
				return nil, h, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("seal hash %s does not match the announced %s", got, h.Hash)}
			}
			doc, derr := decodeFeedSeal(body, key)
			if derr != nil {
				return nil, h, &feedEndpointError{Kind: feedErrMalformed, Detail: derr.Error()}
			}
			if doc.Instance != h.Instance || doc.Generation != h.Generation || doc.Source != h.Source || doc.SealedAtMs != h.SealedAtMs {
				return nil, h, &feedEndpointError{Kind: feedErrMalformed, Detail: "seal metadata does not match its reply header"}
			}
			return doc, h, nil
		case feedWireStatusPending:
			wait := time.Duration(h.RetryAfterMs) * time.Millisecond
			if wait < feedPendingRetryFloor {
				wait = feedPendingRetryFloor
			}
			if wait > feedPendingRetryCap {
				wait = feedPendingRetryCap
			}
			limit := giveUp
			if h.GiveUpAtMs > 0 {
				if feedGive := time.UnixMilli(h.GiveUpAtMs).UTC(); feedGive.Before(limit) {
					limit = feedGive
				}
			}
			if !c.clock().Add(wait).Before(limit) {
				return nil, h, &feedEndpointError{Kind: feedErrPendingLimit, Detail: fmt.Sprintf("key %d still pending at %s", key, limit.Format(time.RFC3339))}
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, h, &feedEndpointError{Kind: feedErrTransport, Detail: err.Error()}
			}
		case feedWireStatusUnavailable:
			return nil, h, &feedEndpointError{Kind: feedErrUnavailable, Detail: h.Detail}
		case feedWireStatusError:
			return nil, h, &feedEndpointError{Kind: feedErrServer, Detail: h.Detail}
		default:
			return nil, h, &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("unknown reply status %q", h.Status)}
		}
	}
}

func (c *sharedFeedClient) Fetch(ctx context.Context, key int64, reqs cycleMarketRequirements) (*marketSnapshot, sharedFeedFetchReport) {
	started := c.clock()
	giveUp := c.giveUpAt(key)
	report := sharedFeedFetchReport{Key: key}
	var snap *marketSnapshot
	for _, ep := range c.endpoints {
		doc, h, err := c.fetchFrom(ctx, ep, key, giveUp)
		c.recordEndpoint(ep, key, h, err, &report)
		if err != nil {
			report.Attempts = append(report.Attempts, fmt.Sprintf("%s=%v", ep.Name, err))
			continue
		}
		report.Attempts = append(report.Attempts, ep.Name+"=sealed")
		snap = doc.snapshot()
		report.Status = feedFetchSealed
		report.Endpoint = ep.Name
		report.Source = doc.Source
		report.Instance = doc.Instance
		report.Generation = doc.Generation
		report.Hash = h.Hash
		report.Bytes = h.Bytes
		report.SealedAtMs = doc.SealedAtMs
		report.KeysTotal = len(doc.Keys)
		for _, k := range doc.Keys {
			if k.Readiness.Ready {
				report.KeysReady++
			}
		}
		break
	}
	stopping := false
	if snap == nil {
		snap = degradedSharedSnapshot(key, c.clock())
		report.Status = feedFetchDegraded
		report.Reason = "no endpoint served a compatible seal: " + strings.Join(report.Attempts, "; ")
		if ctx.Err() != nil {
			stopping = true
			report.Reason = "the consumer is stopping; " + report.Reason
		}
	} else {
		report.Gaps = applySealCoverage(snap, reqs)
	}
	report.WaitedMs = c.clock().Sub(started).Milliseconds()
	report.AtUnix = started.Unix()

	c.mu.Lock()
	switch {
	case stopping:
	case report.Status == feedFetchDegraded && !c.outage:
		c.outage = true
		report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED OUTAGE** key %d: %s. Entries are held; closes, stops, ratchet and protection continue on verified inputs.", key, strings.Join(report.Attempts, "; ")))
	case report.Status == feedFetchSealed && c.outage:
		c.outage = false
		report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED RECOVERED** key %d served by %s (source %s, instance %s, generation %d).", key, report.Endpoint, report.Source, report.Instance, report.Generation))
	}
	if report.Status == feedFetchSealed {
		gaps := strings.Join(report.Gaps, "; ")
		if gaps != c.lastGaps {
			if gaps != "" {
				report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED COVERAGE GAP** key %d: %s. Affected strategies hold entries; reload the feed with this consumer's config first.", key, gaps))
			} else {
				report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED COVERAGE RESTORED** key %d: the seal covers every key this consumer needs.", key))
			}
			c.lastGaps = gaps
		}
	}
	cp := report
	cp.Alerts = nil
	c.last = &cp
	c.mu.Unlock()
	return snap, report
}

func (c *sharedFeedClient) recordEndpoint(ep sharedFeedEndpoint, key int64, h feedWireHeader, err error, report *sharedFeedFetchReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.states[ep.Name]
	if st == nil {
		st = &feedEndpointState{Name: ep.Name, Socket: ep.Socket}
		c.states[ep.Name] = st
	}
	st.LastKey = key
	if h.Instance != "" {
		st.Instance = h.Instance
	}
	incompatible := st.Incompatible
	if err != nil {
		var fe *feedEndpointError
		if errors.As(err, &fe) {
			st.LastStatus = fe.Kind
			st.LastDetail = fe.Detail
			switch fe.Kind {
			case feedErrIncompatible:
				incompatible = true
			case feedErrUnavailable, feedErrPendingLimit, feedErrServer:
				incompatible = false
			}
		} else {
			st.LastStatus = "error"
			st.LastDetail = err.Error()
		}
	} else {
		st.LastStatus = feedFetchSealed
		st.LastDetail = ""
		incompatible = false
	}
	if incompatible != st.Incompatible {
		st.Incompatible = incompatible
		if incompatible {
			report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED INCOMPATIBLE** %s endpoint %s is excluded: %s", ep.Name, ep.Socket, st.LastDetail))
		} else {
			report.Alerts = append(report.Alerts, fmt.Sprintf("**SHARED MARKET FEED COMPATIBLE AGAIN** %s endpoint %s serves this consumer again.", ep.Name, ep.Socket))
		}
	}
}

func degradedSharedSnapshot(key int64, now time.Time) *marketSnapshot {
	return &marketSnapshot{
		Version:      marketSnapshotVersion,
		EvaluationID: feedSealEvaluationID(key),
		SealedAt:     now.UTC(),
		Deadline:     time.Unix(key, 0).UTC(),
		keys:         map[marketFeedKey]*marketSnapshotKey{},
		mids:         map[string]feedMid{},
		funding:      map[string]feedFunding{},
	}
}

func applySealCoverage(snap *marketSnapshot, reqs cycleMarketRequirements) []string {
	var gaps []string
	for _, cr := range reqs.Keys {
		entry, ok := snap.keys[cr.Key]
		if !ok || entry == nil {
			gaps = append(gaps, fmt.Sprintf("%s is not in the seal", cr.Key.PayloadID()))
			continue
		}
		if entry.Readiness.Required < cr.Required {
			gaps = append(gaps, fmt.Sprintf("%s lookback %d is below the %d this consumer needs", cr.Key.PayloadID(), entry.Readiness.Required, cr.Required))
			delete(snap.keys, cr.Key)
		}
	}
	coins := make([]string, 0, len(reqs.Funding))
	for coin := range reqs.Funding {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		need := reqs.Funding[coin]
		if !need.Scalar && !need.Records {
			continue
		}
		if _, ok := snap.funding[coin]; !ok {
			gaps = append(gaps, fmt.Sprintf("funding for %s is not in the seal", coin))
		}
	}
	return gaps
}

type feedEndpointDescription struct {
	Endpoint sharedFeedEndpoint
	Describe *feedDescribe
	Header   feedWireHeader
	Err      error
}

func (c *sharedFeedClient) Describe(ctx context.Context) []feedEndpointDescription {
	out := make([]feedEndpointDescription, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		h, _, err := c.roundTrip(ctx, ep.Socket, feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe})
		d := feedEndpointDescription{Endpoint: ep, Header: h, Err: err}
		if err == nil {
			if h.Status != feedWireStatusDescribe || h.Describe == nil {
				d.Err = &feedEndpointError{Kind: feedErrMalformed, Detail: fmt.Sprintf("describe returned status %q", h.Status)}
			} else {
				d.Describe = h.Describe
			}
		}
		out = append(out, d)
	}
	return out
}

func sharedFeedCompatibilityLines(prefix string, descs []feedEndpointDescription, req feedRequirements, cadences []int) (lines []string, problems []string) {
	for _, d := range descs {
		if d.Err != nil {
			msg := fmt.Sprintf("%s endpoint %s (%s): %v", d.Endpoint.Name, d.Endpoint.Socket, prefix, d.Err)
			lines = append(lines, "[feed] "+msg)
			problems = append(problems, msg)
			continue
		}
		desc := d.Describe
		lines = append(lines, fmt.Sprintf("[feed] %s endpoint %s: instance=%s source=%s generation=%d serving=%t first_deadline=%d keys=%d cadences=%s",
			d.Endpoint.Name, d.Endpoint.Socket, d.Header.Instance, d.Header.Source, d.Header.Generation, desc.Serving, desc.FirstDeadline, len(desc.Keys), formatDescribeCadences(desc.Cadences)))
		served := make(map[marketFeedKey]int, len(desc.Keys))
		for _, k := range desc.Keys {
			served[marketFeedKey{Host: k.Host, Namespace: k.Namespace, Symbol: k.Symbol, Timeframe: k.Timeframe}] = k.Required
		}
		var gaps []string
		for _, key := range req.Order {
			need := req.Keys[key]
			got, ok := served[key]
			switch {
			case !ok:
				gaps = append(gaps, key.PayloadID()+" not served")
			case got < need:
				gaps = append(gaps, fmt.Sprintf("%s lookback %d < %d", key.PayloadID(), got, need))
			}
		}
		fundingServed := make(map[string]feedDescribeFunding, len(desc.Funding))
		for _, f := range desc.Funding {
			fundingServed[f.Coin] = f
		}
		fundingCoins := make([]string, 0, len(req.Funding))
		for coin := range req.Funding {
			fundingCoins = append(fundingCoins, coin)
		}
		sort.Strings(fundingCoins)
		for _, coin := range fundingCoins {
			need := req.Funding[coin]
			got := fundingServed[coin]
			if (need.Scalar && !got.Scalar) || (need.Records && !got.Records) {
				gaps = append(gaps, "funding "+coin+" not served")
			}
		}
		cadenceSet := make(map[int]bool, len(desc.Cadences))
		for _, cd := range desc.Cadences {
			cadenceSet[cd.Seconds] = true
		}
		for _, cd := range cadences {
			if !cadenceSet[cd] {
				gaps = append(gaps, fmt.Sprintf("cadence %ds not scheduled", cd))
			}
		}
		if len(gaps) > 0 {
			msg := fmt.Sprintf("%s endpoint %s (%s) does not cover this consumer: %s; reload the feed with this consumer's config", d.Endpoint.Name, d.Endpoint.Socket, prefix, strings.Join(gaps, ", "))
			lines = append(lines, "[feed] "+msg)
			problems = append(problems, msg)
		} else {
			lines = append(lines, fmt.Sprintf("[feed] %s endpoint covers every key, funding need and cadence of this consumer", d.Endpoint.Name))
		}
	}
	return lines, problems
}

func formatDescribeCadences(cads []feedDescribeCadence) string {
	vals := make([]int, 0, len(cads))
	for _, c := range cads {
		vals = append(vals, c.Seconds)
	}
	return formatCadences(vals)
}

func (c *sharedFeedClient) status() *sharedFeedStatus {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := &sharedFeedStatus{Outage: c.outage}
	names := make([]string, 0, len(c.states))
	for n := range c.states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Endpoints = append(out.Endpoints, *c.states[n])
	}
	if c.last != nil {
		cp := *c.last
		out.Last = &cp
	}
	return out
}

type sharedFeedStatus struct {
	Outage    bool                   `json:"outage"`
	Endpoints []feedEndpointState    `json:"endpoints"`
	Last      *sharedFeedFetchReport `json:"last,omitempty"`
}

func sharedFeedAuditLine(r sharedFeedFetchReport, strategies int, cadences []int) string {
	gaps := "none"
	if len(r.Gaps) > 0 {
		gaps = strings.Join(r.Gaps, "|")
	}
	if r.Status == feedFetchSealed {
		return fmt.Sprintf("[feed-audit] key=%d status=sealed endpoint=%s source=%s instance=%s generation=%d hash=%s bytes=%d keys=%d ready=%d strategies=%d cadences=%s at=%d waited_ms=%d gaps=%q",
			r.Key, r.Endpoint, r.Source, r.Instance, r.Generation, r.Hash, r.Bytes, r.KeysTotal, r.KeysReady, strategies, joinInts(cadences), r.AtUnix, r.WaitedMs, gaps)
	}
	return fmt.Sprintf("[feed-audit] key=%d status=degraded strategies=%d cadences=%s at=%d waited_ms=%d reason=%q",
		r.Key, strategies, joinInts(cadences), r.AtUnix, r.WaitedMs, r.Reason)
}

func joinInts(vals []int) string {
	if len(vals) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, fmt.Sprintf("%d", v))
	}
	return strings.Join(parts, ",")
}

func splitDueByDeadline(due []StrategyConfig, marks map[string]feedEvaluationMark) ([]StrategyConfig, map[string]feedEvaluationMark, time.Time, int) {
	if len(marks) == 0 {
		return due, marks, time.Time{}, 0
	}
	var chosen time.Time
	for _, m := range marks {
		if chosen.IsZero() || m.Deadline.Before(chosen) {
			chosen = m.Deadline
		}
	}
	kept := make([]StrategyConfig, 0, len(due))
	keptMarks := make(map[string]feedEvaluationMark, len(marks))
	deferred := 0
	for _, sc := range due {
		m, isFeed := marks[sc.ID]
		if !isFeed {
			kept = append(kept, sc)
			continue
		}
		if m.Deadline.Equal(chosen) {
			kept = append(kept, sc)
			keptMarks[sc.ID] = m
			continue
		}
		deferred++
	}
	return kept, keptMarks, chosen, deferred
}

func cycleFeedCadences(marks map[string]feedEvaluationMark) []int {
	set := make(map[int]bool)
	for _, m := range marks {
		set[m.IntervalSeconds] = true
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func checkSharedFeedCoverage(client *sharedFeedClient, phase string, req feedRequirements, cadences []int, notifier *MultiNotifier) {
	ctx, cancel := context.WithTimeout(shutdownReadOnlyCtx, 2*feedClientIOTimeout)
	defer cancel()
	lines, problems := sharedFeedCompatibilityLines(phase, client.Describe(ctx), req, cadences)
	for _, line := range lines {
		fmt.Println(line)
	}
	if len(problems) == 0 || notifier == nil || !notifier.HasOwner() {
		return
	}
	notifier.SendOwnerDM("**Shared market feed** (" + phase + "): " + strings.Join(problems, "; ") + ". Entries hold for uncovered strategies; this consumer keeps running and retries every cycle.")
}

func feedEffectiveCadences(cfg *Config, intervals map[string]int) []int {
	set := make(map[int]bool)
	for _, sc := range cfg.Strategies {
		if !feedScopedStrategy(sc) || shouldSkipZeroCapital(sc) {
			continue
		}
		set[feedEvaluationInterval(sc, intervals, cfg.IntervalSeconds)] = true
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func sharedFeedMarkFallbackLine(key int64, missing []string, prices map[string]float64) string {
	var supplied, absent []string
	for _, coin := range missing {
		if _, ok := prices[coin]; ok {
			supplied = append(supplied, coin)
		} else {
			absent = append(absent, coin)
		}
	}
	sort.Strings(supplied)
	sort.Strings(absent)
	return fmt.Sprintf("[feed-audit] key=%d mark_fallback=%s absent=%s: REST marks outside the seal feed check prices, degraded results and valuation this cycle",
		key, strings.Join(supplied, ","), strings.Join(absent, ","))
}
