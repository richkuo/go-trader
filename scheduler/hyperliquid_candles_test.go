package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func hlCandleFixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "platforms", "hyperliquid", "testdata", name)
}

func TestCandleSnapshotConversionParity(t *testing.T) {
	rawBlob, err := os.ReadFile(hlCandleFixturePath(t, "candle_snapshot_fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	expectedBlob, err := os.ReadFile(hlCandleFixturePath(t, "candle_snapshot_expected_rows.json"))
	if err != nil {
		t.Fatalf("read expected rows: %v", err)
	}
	var expected []hlCandleRow
	if err := json.Unmarshal(expectedBlob, &expected); err != nil {
		t.Fatalf("parse expected rows: %v", err)
	}

	raws, err := parseHyperliquidCandleRaws(rawBlob)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := hlCandleRowsFromRaws(raws)
	if len(got) != len(expected) {
		t.Fatalf("row count: got %d want %d", len(got), len(expected))
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("row %d: got %+v want %+v", i, got[i], expected[i])
		}
	}
	if raws[3].HasClose {
		t.Fatalf("fixture row 3 must have no close timestamp so the open-time fallback is exercised")
	}
	if got[3].TimestampMs != raws[3].OpenMs {
		t.Fatalf("row without T must fall back to the open timestamp: got %d want %d", got[3].TimestampMs, raws[3].OpenMs)
	}
}

func TestCandleIntervalTableCoversSupportedIntervals(t *testing.T) {
	for _, tf := range []string{"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "8h", "12h", "1d", "3d", "1w", "1M"} {
		if _, ok := hlCandleIntervalMs(tf); !ok {
			t.Fatalf("interval %q missing from the Hyperliquid interval table", tf)
		}
	}
	if _, ok := hlCandleIntervalMs("7m"); ok {
		t.Fatalf("unsupported interval 7m must not resolve")
	}
	intervals := hlSupportedCandleIntervals()
	if len(intervals) != len(hlCandleIntervalMsTable) {
		t.Fatalf("supported interval list length: got %d want %d", len(intervals), len(hlCandleIntervalMsTable))
	}
	for i := 1; i < len(intervals); i++ {
		prev, _ := hlCandleIntervalMs(intervals[i-1])
		cur, _ := hlCandleIntervalMs(intervals[i])
		if prev >= cur {
			t.Fatalf("supported intervals must be ordered by duration: %q then %q", intervals[i-1], intervals[i])
		}
	}
}

func TestFetchCandleHistoryWidensLikeThePythonAdapter(t *testing.T) {
	makeRaws := func(n int, startOpen, intervalMs int64) []hlCandleRaw {
		out := make([]hlCandleRaw, 0, n)
		for i := 0; i < n; i++ {
			open := startOpen + int64(i)*intervalMs
			out = append(out, hlCandleRaw{
				OpenMs: open, CloseMs: open + intervalMs - 1, HasClose: true,
				Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 10,
			})
		}
		return out
	}

	tests := []struct {
		name       string
		limit      int
		available  int
		wantCalls  int
		wantRows   int
		wantShort  bool
		wantErr    bool
		fetchError error
	}{
		{name: "first pass satisfies the limit", limit: 5, available: 200, wantCalls: 1, wantRows: 5},
		{name: "sparse market stops after two stale widenings", limit: 100, available: 4, wantCalls: 3, wantRows: 4, wantShort: true},
		{name: "empty history stops immediately", limit: 100, available: 0, wantCalls: 1, wantRows: 0, wantShort: true},
		{name: "fetch error propagates", limit: 10, available: 10, wantErr: true, fetchError: errors.New("boom")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			orig := fetchHyperliquidCandleSnapshotFn
			fetchHyperliquidCandleSnapshotFn = func(_ context.Context, _, _ string, _, _ int64) ([]hlCandleRaw, error) {
				calls++
				if tc.fetchError != nil {
					return nil, tc.fetchError
				}
				return makeRaws(tc.available, 1_700_000_000_000, 3_600_000), nil
			}
			defer func() { fetchHyperliquidCandleSnapshotFn = orig }()

			hist, err := hlFetchCandleHistory(context.Background(), "BTC", "1h", tc.limit, time.Unix(1_700_100_000, 0))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("snapshot calls: got %d want %d", calls, tc.wantCalls)
			}
			if len(hist.Raws) != tc.wantRows {
				t.Fatalf("rows: got %d want %d", len(hist.Raws), tc.wantRows)
			}
			if hist.Short != tc.wantShort {
				t.Fatalf("short: got %v want %v", hist.Short, tc.wantShort)
			}
		})
	}
}

func TestFetchCandleHistoryRejectsUnsupportedInterval(t *testing.T) {
	if _, err := hlFetchCandleHistory(context.Background(), "BTC", "7m", 10, time.Now()); err == nil {
		t.Fatalf("expected an unsupported-interval error")
	}
}
