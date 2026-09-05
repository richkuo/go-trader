package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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

func TestSealedSnapshotIsImmutableAndShared(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 60, 80, now)
	reqs := feedCycleReqs(key, 60)

	snap := sealCycleMarketSnapshot(context.Background(), owner, reqs, "300s/1700003600", now)
	if snap == nil {
		t.Fatalf("seal returned no snapshot")
	}
	frame, ok := snap.frameFor(key, 60)
	if !ok {
		t.Fatalf("frame missing from the sealed snapshot")
	}
	if len(frame.Rows) != 60 {
		t.Fatalf("frame rows: got %d want 60", len(frame.Rows))
	}
	if !frame.FormingBarIncluded {
		t.Fatalf("the forming bar must be represented like candleSnapshot does today")
	}
	lastRow := frame.Rows[len(frame.Rows)-1]
	if lastRow.TimestampMs != frame.LastCloseMs {
		t.Fatalf("rows must carry the exchange close timestamp: %d vs %d", lastRow.TimestampMs, frame.LastCloseMs)
	}

	newOpen := owner.keys[key].Bars[len(owner.keys[key].Bars)-1].OpenMs + testFeedIntervalMs
	owner.IngestSocketCandle(key, testRawBar(newOpen, 9_999), now.Add(time.Second))

	again, _ := snap.frameFor(key, 60)
	if len(again.Rows) != len(frame.Rows) || again.LastCloseMs != frame.LastCloseMs {
		t.Fatalf("a later socket update mutated the sealed snapshot")
	}
	for i := range frame.Rows {
		if frame.Rows[i] != again.Rows[i] {
			t.Fatalf("sealed row %d changed after a socket update", i)
		}
	}

	shorter, ok := snap.frameFor(key, 30)
	if !ok || len(shorter.Rows) != 30 {
		t.Fatalf("a consumer with a smaller lookback must get its own slice: ok=%v rows=%d", ok, len(shorter.Rows))
	}
	if shorter.Rows[len(shorter.Rows)-1] != frame.Rows[len(frame.Rows)-1] {
		t.Fatalf("both slices must end on the same newest bar")
	}
}

func TestSealedSnapshotRecoversStaleKeysOncePerCycle(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := newMarketFeedOwner(func() time.Time { return now }, nil)
	st := newFeedKeyState(key, testFeedIntervalMs, 40)
	base := now.UnixMilli() - 200*testFeedIntervalMs
	mergeRestRows(st, testRawSeries(base, 40), now.Add(-time.Hour))
	st.Status = feedStatusReady
	owner.keys[key] = st
	owner.published[key] = true

	var calls int
	var mu sync.Mutex
	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return testRawSeries(now.UnixMilli()-40*testFeedIntervalMs, 40), nil
	})

	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 40), "300s/1", now)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("a stale key gets exactly one shared recovery per cycle, got %d", got)
	}
	if owner.Metrics().RecoveryCalls != 1 {
		t.Fatalf("recovery calls must be counted separately: %+v", owner.Metrics())
	}
	if owner.Metrics().SteadyCandleCalls != 0 {
		t.Fatalf("steady-state candle REST calls must stay 0: %+v", owner.Metrics())
	}
	if snap.keyFailed(key) {
		t.Fatalf("the recovered key must serve every consumer: %+v", snap.keys[key].Readiness)
	}
}

func TestSealedSnapshotReportsAnUnrecoverableKeyAsFailed(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := newMarketFeedOwner(func() time.Time { return now }, nil)
	owner.keys[key] = newFeedKeyState(key, testFeedIntervalMs, 40)
	owner.published[key] = true
	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		return nil, context.DeadlineExceeded
	})

	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 40), "300s/1", now)
	if !snap.keyFailed(key) {
		t.Fatalf("an unrecoverable key must report failed, never a fabricated frame")
	}
	if _, ok := snap.frameFor(key, 40); ok {
		t.Fatalf("a failed key must yield no frame")
	}
	if _, err := marketPayloadFor(snap, []marketPayloadFrameSpec{{Key: key, Required: 40}}, nil); err == nil {
		t.Fatalf("building a payload from a failed key must be an explicit error")
	}
	if len(owner.DrainAlerts()) == 0 {
		t.Fatalf("a failed recovery must raise a feed alert")
	}
}

func TestMarketPayloadCapAndShape(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 60, 80, now)
	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 60), "300s/1700003600", now)

	payload, err := marketPayloadFor(snap, []marketPayloadFrameSpec{{Key: key, Required: 60}}, []string{"BTC"})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	blob, err := marketPayloadJSON(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(blob) > marketPayloadMaxBytes {
		t.Fatalf("payload %d bytes exceeds the cap", len(blob))
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("payload must be valid JSON: %v", err)
	}
	if decoded["snapshot_id"] != "300s/1700003600" {
		t.Fatalf("snapshot id: %v", decoded["snapshot_id"])
	}
	frames, _ := decoded["frames"].(map[string]any)
	if _, ok := frames["BTC|1h"]; !ok {
		t.Fatalf("frames must be keyed <symbol>|<timeframe>: %v", frames)
	}
	mids, _ := decoded["mids"].(map[string]any)
	if _, ok := mids["BTC"]; !ok {
		t.Fatalf("the payload must carry the mid with its provenance: %v", mids)
	}

	oversize := &marketPayload{Version: 1, SnapshotID: "x", Frames: map[string]marketFrame{}}
	big := make([]hlCandleRow, marketPayloadMaxBytes/8)
	for i := range big {
		big[i] = hlCandleRow{TimestampMs: int64(i), Open: 1.123456, High: 2.123456, Low: 0.123456, Close: 1.5, Volume: 9.87654}
	}
	oversize.Frames["BTC|1h"] = marketFrame{Rows: big}
	if _, err := marketPayloadJSON(oversize); err == nil {
		t.Fatalf("an oversized payload must be refused, never truncated")
	}
}

func TestSnapshotMidsExpire(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 60, 80, now)
	owner.mids["BTC"] = feedMid{Px: 101, RecvAt: now.Add(-feedMidStaleAfter - time.Second), Source: "ws"}
	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 60), "300s/1", now)
	if _, ok := snap.midFor("BTC"); ok {
		t.Fatalf("a mid older than %s must not be treated as verified", feedMidStaleAfter)
	}
	if len(snap.freshMids()) != 0 {
		t.Fatalf("stale mids must not reach the cycle price map")
	}
	owner.mids["BTC"] = feedMid{Px: 101, RecvAt: now, Source: "ws"}
	fresh := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 60), "300s/2", now)
	if px, ok := fresh.midFor("BTC"); !ok || px != 101 {
		t.Fatalf("a fresh mid must reach the cycle price map: %v %v", px, ok)
	}
}

func TestSnapshotAgeGatesLateDecisions(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 60, 80, now)
	snap := sealCycleMarketSnapshot(context.Background(), owner, feedCycleReqs(key, 60), "300s/1", now)
	feed := &marketFeedContext{Enabled: true, Snapshot: snap, Interval: 300}
	if tooOld, _ := feed.decisionTooOld(now.Add(time.Minute), 300); tooOld {
		t.Fatalf("a one-minute-old snapshot is inside the 150s limit for a 300s cadence")
	}
	tooOld, why := feed.decisionTooOld(now.Add(4*time.Minute), 300)
	if !tooOld {
		t.Fatalf("a four-minute-old snapshot must be refused for a 300s cadence")
	}
	if !strings.Contains(why, "decision-age limit") {
		t.Fatalf("the refusal must name the limit: %s", why)
	}
}

func TestSealedPayloadCarriesFundingIndependentlyOfMids(t *testing.T) {
	key := testFeedKey()
	now := time.Unix(1_700_003_600, 0).UTC()
	owner := feedOwnerWithHistory(t, key, 60, 80, now)
	delete(owner.mids, "BTC")
	owner.funding["BTC"] = &feedFunding{Current: 0.0001, Avg7d: 0.0002, HasScalar: true, HasRecords: true,
		Records: []feedFundingRecord{{Rate: 0.0001, TimeMs: now.UnixMilli()}}, FetchedAt: now, Source: "rest"}
	reqs := feedCycleReqs(key, 60)
	reqs.Funding["BTC"] = feedFundingNeed{Scalar: true, Records: true}

	snap := sealCycleMarketSnapshot(context.Background(), owner, reqs, "300s/1", now)
	payload, err := marketPayloadFor(snap, []marketPayloadFrameSpec{{Key: key, Required: 60}}, []string{"BTC"})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, hasMid := payload.Mids["BTC"]; hasMid {
		t.Fatalf("no mid has arrived, so the payload must not invent one")
	}
	f, ok := payload.Funding["BTC"]
	if !ok || !f.HasScalar || !f.HasRecords || f.Current != 0.0001 {
		t.Fatalf("funding must travel without a mid: %+v", payload.Funding)
	}

	tests := []struct {
		name     string
		funding  *feedFunding
		scalar   bool
		records  bool
		wantHold bool
	}{
		{name: "healthy scalar and records", funding: owner.funding["BTC"], scalar: true, records: true, wantHold: false},
		{name: "strategy without funding needs never holds", funding: nil, scalar: false, records: false, wantHold: false},
		{name: "missing funding holds", funding: nil, scalar: true, records: false, wantHold: true},
		{name: "fetch error holds", funding: &feedFunding{Err: "boom", FetchedAt: now}, scalar: true, records: false, wantHold: true},
		{name: "scalar need without a rate holds", funding: &feedFunding{HasRecords: true, FetchedAt: now}, scalar: true, records: false, wantHold: true},
		{name: "records need without history holds", funding: &feedFunding{HasScalar: true, FetchedAt: now}, scalar: false, records: true, wantHold: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &marketSnapshot{funding: map[string]feedFunding{}}
			if tc.funding != nil {
				s.funding["BTC"] = *tc.funding
			}
			held, why := s.fundingHold("BTC", tc.scalar, tc.records)
			if held != tc.wantHold || (held && why == "") {
				t.Fatalf("hold: got %v %q want %v", held, why, tc.wantHold)
			}
		})
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
