package main

import (
	"context"
	"testing"
	"time"
)

func feedOwnerWithHistory(t *testing.T, key marketFeedKey, required, bars int, now time.Time) *marketFeedOwner {
	t.Helper()
	owner := newMarketFeedOwner(func() time.Time { return now }, nil)
	st := newFeedKeyState(key, testFeedIntervalMs, required)
	base := now.UnixMilli() - int64(bars)*testFeedIntervalMs
	mergeRestRows(st, testRawSeries(base, bars), now.Add(-time.Second))
	st.Status = feedStatusReady
	st.LastRecvAt = now
	owner.keys[key] = st
	owner.published[key] = true
	owner.midCoins[key.Symbol] = true
	owner.mids[key.Symbol] = feedMid{Px: 101, RecvAt: now, Source: "ws"}
	owner.gen = 3
	return owner
}

func feedCycleReqs(key marketFeedKey, required int) cycleMarketRequirements {
	return cycleMarketRequirements{
		Keys:    []cycleMarketRequirement{{Key: key, Required: required}},
		Coins:   []string{key.Symbol},
		Funding: map[string]feedFundingNeed{},
	}
}

func TestSealedSnapshotRepairsAReconnectingKeyBeforeServingIt(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 40, 60, now)
	owner.SetConnected(true)
	owner.SetConnected(false)
	if r, _ := owner.readinessFor(key); r.Ready || r.Status != feedStatusRepairing {
		t.Fatalf("a key awaiting reconnect repair must not report ready: %+v", r)
	}

	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		return nil, context.DeadlineExceeded
	})
	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 40), "300s/1", now)
	if !snap.keyFailed(key) {
		t.Fatalf("a repairing key whose recovery failed must not serve a frame with a hole in it: %+v", snap.keys[key].Readiness)
	}
	if owner.Metrics().RecoveryCalls != 1 {
		t.Fatalf("the cycle must try exactly one recovery: %+v", owner.Metrics())
	}

	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		return testRawSeries(now.UnixMilli()-40*testFeedIntervalMs, 40), nil
	})
	snap = sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 40), "300s/2", now)
	if snap.keyFailed(key) {
		t.Fatalf("a successful recovery must serve the key in the same cycle: %+v", snap.keys[key].Readiness)
	}
	if r, _ := owner.readinessFor(key); !r.Ready || r.Status != feedStatusReady {
		t.Fatalf("the repaired key must be ready again: %+v", r)
	}

	owner.SetConnected(true)
	owner.SetConnected(false)
	owner.SetConnected(true)
	owner.repairAfterConnect(context.Background())
	if r, _ := owner.readinessFor(key); !r.Ready {
		t.Fatalf("a reconnect repair that succeeds serves the key with no degraded cycle: %+v", r)
	}
}
