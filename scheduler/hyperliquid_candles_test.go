package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
