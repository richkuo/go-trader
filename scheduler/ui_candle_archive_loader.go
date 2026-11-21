package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	candleArchiveStateDisabled = "disabled"
	candleArchiveStateLoading  = "loading"
	candleArchiveStateReady    = "ready"
	candleArchiveStateStale    = "stale"
	candleArchiveStateRefused  = "refused"

	candleArchiveDefaultRefresh = 300
	candleArchiveMinRefresh     = 30
	candleArchiveMaxRefresh     = 86400
	candleArchiveLoadDeadline   = 5 * time.Minute
)

type candleArchiveLoader struct {
	dir     string
	refresh time.Duration
	limits  candleArchiveLimits
	now     func() time.Time

	current atomic.Pointer[candleArchiveGeneration]
	epoch   atomic.Uint64
	running atomic.Bool

	mu          sync.Mutex
	state       string
	lastErr     string
	lastErrCode string
	lastErrAt   time.Time
	lastCheckAt time.Time
	attempted   string
	swaps       int
	onSwap      func()
}

type candleArchiveStatus struct {
	State           string `json:"state"`
	Dir             string `json:"archive_dir,omitempty"`
	Generation      string `json:"generation,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	Series          int    `json:"series"`
	Publisher       string `json:"publisher,omitempty"`
	LastRefreshErr  string `json:"last_refresh_error,omitempty"`
	LastRefreshErrA string `json:"last_refresh_error_at,omitempty"`
	LastCheckAt     string `json:"last_check_at,omitempty"`
	Swaps           int    `json:"generation_swaps"`
}

func newCandleArchiveLoader(cfg *DashboardCandlesConfig) *candleArchiveLoader {
	l := &candleArchiveLoader{now: time.Now, state: candleArchiveStateDisabled}
	if cfg == nil || cfg.ArchiveDir == "" {
		return l
	}
	l.dir = cfg.ArchiveDir
	l.refresh = time.Duration(cfg.refreshSeconds()) * time.Second
	l.limits = candleArchiveLimits{MaxBytes: cfg.maxGenerationBytes(), Deadline: candleArchiveLoadDeadline}
	l.state = candleArchiveStateLoading
	return l
}

func (l *candleArchiveLoader) enabled() bool { return l != nil && l.dir != "" }

func (l *candleArchiveLoader) generation() *candleArchiveGeneration {
	if l == nil {
		return nil
	}
	return l.current.Load()
}

func (l *candleArchiveLoader) pin() *candleArchiveGeneration {
	for i := 0; i < 4; i++ {
		g := l.generation()
		if g == nil {
			return nil
		}
		if g.acquire() {
			return g
		}
	}
	return nil
}

func (l *candleArchiveLoader) run(ctx context.Context) {
	if !l.enabled() {
		return
	}
	l.check(ctx)
	t := time.NewTicker(l.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.check(ctx)
		}
	}
}

func (l *candleArchiveLoader) check(ctx context.Context) {
	if !l.running.CompareAndSwap(false, true) {
		return
	}
	defer l.running.Store(false)
	now := l.now().UTC()
	l.mu.Lock()
	l.lastCheckAt = now
	l.mu.Unlock()
	id, err := readCandleArchiveCurrent(l.dir)
	if err != nil {
		code := candleAvailSourceIncompatible
		if errors.Is(err, os.ErrNotExist) {
			code = candleAvailSourceMissing
		}
		l.fail(code, fmt.Sprintf("read %s: %v", candleArchiveCurrentName, err), now)
		return
	}
	if cur := l.generation(); cur != nil && cur.ID == id {
		l.settle()
		return
	}
	l.mu.Lock()
	attempted := l.attempted
	l.mu.Unlock()
	if attempted == id {
		return
	}
	gen, err := loadCandleArchiveGeneration(ctx, l.dir, id, l.limits, now)
	if !errors.Is(err, errCandleArchiveInterrupted) {
		l.mu.Lock()
		l.attempted = id
		l.mu.Unlock()
	}
	if err != nil {
		l.fail(candleAvailSourceIncompatible, fmt.Sprintf("generation %s refused: %v", id, err), now)
		fmt.Printf("[candle-archive] generation %s refused: %v\n", id, err)
		return
	}
	prev := l.current.Swap(gen)
	l.epoch.Add(1)
	if prev != nil {
		prev.retire()
	}
	l.mu.Lock()
	l.swaps++
	l.lastErr = ""
	l.lastErrCode = ""
	l.lastErrAt = time.Time{}
	onSwap := l.onSwap
	l.mu.Unlock()
	l.settle()
	fmt.Printf("[candle-archive] admitted generation %s (%d series, %d bytes, publisher %s, expires %s)\n",
		gen.ID, len(gen.series), gen.Bytes, gen.Publisher.ID, gen.ExpiresAt.Format(time.RFC3339))
	if onSwap != nil {
		onSwap()
	}
}

func (l *candleArchiveLoader) fail(code, msg string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr = msg
	l.lastErrCode = code
	l.lastErrAt = now
	if l.current.Load() == nil {
		l.state = candleArchiveStateRefused
	}
}

func (l *candleArchiveLoader) settle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if g := l.current.Load(); g != nil {
		l.state = candleArchiveStateReady
	}
}

func (l *candleArchiveLoader) status() candleArchiveStatus {
	if l == nil {
		return candleArchiveStatus{State: candleArchiveStateDisabled}
	}
	l.mu.Lock()
	st := candleArchiveStatus{State: l.state, Dir: l.dir, LastRefreshErr: l.lastErr, Swaps: l.swaps}
	if !l.lastErrAt.IsZero() {
		st.LastRefreshErrA = l.lastErrAt.Format(time.RFC3339)
	}
	if !l.lastCheckAt.IsZero() {
		st.LastCheckAt = l.lastCheckAt.Format(time.RFC3339)
	}
	l.mu.Unlock()
	if g := l.generation(); g != nil {
		st.Generation = g.ID
		st.PublishedAt = g.PublishedAt.Format(time.RFC3339)
		st.ExpiresAt = g.ExpiresAt.Format(time.RFC3339)
		st.Series = len(g.series)
		st.Publisher = g.Publisher.ID
		if g.expired(l.now()) {
			st.State = candleArchiveStateStale
		}
	}
	return st
}

func (l *candleArchiveLoader) availability() (string, string) {
	if !l.enabled() {
		return candleAvailSourceMissing, "no dashboard_candles.archive_dir is configured"
	}
	st := l.status()
	switch st.State {
	case candleArchiveStateLoading:
		return candleAvailSourceLoading, "the archive generation is still being validated"
	case candleArchiveStateRefused:
		l.mu.Lock()
		code := l.lastErrCode
		l.mu.Unlock()
		if code == "" {
			code = candleAvailSourceIncompatible
		}
		return code, st.LastRefreshErr
	case candleArchiveStateStale:
		return candleAvailSourceStale, "archive generation " + st.Generation + " expired at " + st.ExpiresAt
	}
	return "", ""
}
