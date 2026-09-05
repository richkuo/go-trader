package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMarketFeedNeverNestsInsideSchedulerLock(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := newMarketFeedOwner(func() time.Time { return time.Now().UTC() }, func(string, ...any) {})
	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		return testRawSeries(now.UnixMilli()-60*testFeedIntervalMs, 60), nil
	})

	var mu sync.RWMutex
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		mu.Lock()
		close(held)
		<-release
		mu.Unlock()
	}()
	<-held
	defer close(release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := feedTestRequirements(key, 60, "BTC")
		<-owner.ApplyGeneration(context.Background(), req)
		owner.SetConnected(true)
		owner.IngestSocketCandle(key, testRawBar(now.UnixMilli(), 123), time.Now().UTC())
		owner.IngestMids(map[string]float64{"BTC": 101}, time.Now().UTC(), "ws")
		snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 60), "300s/1", time.Now().UTC())
		if snap == nil {
			t.Errorf("seal must complete while the scheduler lock is held elsewhere")
		}
		owner.Health("300s/1")
		owner.Readiness()
		owner.DrainAlerts()
		owner.Metrics()
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("the feed blocked on the scheduler state lock")
	}
}

func TestMarketFeedSourceNeverTouchesTheSchedulerLock(t *testing.T) {
	matches, err := filepath.Glob("market_feed*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no market_feed source files found")
	}
	banned := []string{"mu.Lock", "mu.RLock", "mu.Unlock", "mu.RUnlock", "*sync.RWMutex"}
	checked := 0
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(blob)
		body = strings.ReplaceAll(body, "feedMu.", "")
		body = strings.ReplaceAll(body, "statusMu.", "")
		body = strings.ReplaceAll(body, "writeMu.", "")
		for _, bad := range banned {
			if strings.Contains(body, bad) {
				t.Fatalf("%s references %q: the feed owner must never take the scheduler state lock", path, bad)
			}
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("expected the whole market_feed source set to be checked, got %d files", checked)
	}
}
