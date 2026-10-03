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

const feedBudgetWindow = 60 * time.Second

var errFeedBudgetExhausted = errors.New("request budget exhausted")

type feedBudgetConfig struct {
	PerMinute int `json:"per_minute"`
	Startup   int `json:"startup"`
}

type feedLedgerCtxKey struct{}
type feedReasonCtxKey struct{}
type feedKeepCtxKey struct{}

type feedRequestLedger struct {
	mu          sync.Mutex
	clock       func() time.Time
	enforce     bool
	perMinute   int
	startup     int
	startupLeft int
	window      []time.Time
	totals      feedBudgetTotals
}

type feedBudgetTotals struct {
	Total           int            `json:"total"`
	Refused         int            `json:"refused"`
	ByReason        map[string]int `json:"by_reason"`
	ByType          map[string]int `json:"by_type"`
	RefusedByReason map[string]int `json:"refused_by_reason"`
}

type feedBudgetStatus struct {
	Enforced    bool             `json:"enforced"`
	PerMinute   int              `json:"per_minute"`
	Startup     int              `json:"startup"`
	StartupLeft int              `json:"startup_left"`
	WindowUsed  int              `json:"window_used"`
	Totals      feedBudgetTotals `json:"totals"`
}

func newFeedBudgetTotals() feedBudgetTotals {
	return feedBudgetTotals{
		ByReason:        map[string]int{},
		ByType:          map[string]int{},
		RefusedByReason: map[string]int{},
	}
}

func newFeedRequestLedger(cfg *feedBudgetConfig, enforce bool, clock func() time.Time) *feedRequestLedger {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	l := &feedRequestLedger{clock: clock, enforce: enforce, totals: newFeedBudgetTotals()}
	l.setLimits(cfg)
	return l
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
}

func (l *feedRequestLedger) openStartup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.startupLeft = l.startup
}

func (l *feedRequestLedger) pruneLocked(now time.Time) {
	cut := 0
	for cut < len(l.window) && now.Sub(l.window[cut]) >= feedBudgetWindow {
		cut++
	}
	if cut > 0 {
		l.window = append([]time.Time(nil), l.window[cut:]...)
	}
}

func (l *feedRequestLedger) acquire(reason, reqType string, keep int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UTC()
	l.pruneLocked(now)
	if reason == string(feedRestBootstrap) && l.startupLeft > 0 {
		l.startupLeft--
		l.recordLocked(reason, reqType)
		return nil
	}
	if l.enforce && len(l.window)+keep >= l.perMinute {
		l.totals.Refused++
		l.totals.RefusedByReason[reason]++
		return fmt.Errorf("%w: %d of %d requests used in the last %s (reason %s, %s)",
			errFeedBudgetExhausted, len(l.window), l.perMinute, feedBudgetWindow, reason, reqType)
	}
	l.window = append(l.window, now)
	l.recordLocked(reason, reqType)
	return nil
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

func (l *feedRequestLedger) status() feedBudgetStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.clock().UTC())
	return feedBudgetStatus{
		Enforced:    l.enforce,
		PerMinute:   l.perMinute,
		Startup:     l.startup,
		StartupLeft: l.startupLeft,
		WindowUsed:  len(l.window),
		Totals:      copyFeedBudgetTotals(l.totals),
	}
}

func copyFeedBudgetTotals(t feedBudgetTotals) feedBudgetTotals {
	out := newFeedBudgetTotals()
	out.Total, out.Refused = t.Total, t.Refused
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

func feedLedgerFrom(ctx context.Context) *feedRequestLedger {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(feedLedgerCtxKey{}).(*feedRequestLedger)
	return l
}

func feedBudgetAcquire(ctx context.Context, reqType string) error {
	l := feedLedgerFrom(ctx)
	if l == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reason, _ := ctx.Value(feedReasonCtxKey{}).(string)
	if reason == "" {
		reason = "other"
	}
	keep, _ := ctx.Value(feedKeepCtxKey{}).(int)
	return l.acquire(reason, reqType, keep)
}

func feedBudgetConfigErrors(field string, cfg *feedBudgetConfig, required bool) []string {
	if cfg == nil {
		if required {
			return []string{fmt.Sprintf("%s is required for feed.source %q: set per_minute and startup from the measured request baseline", field, feedSourceREST)}
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
	return errs
}
