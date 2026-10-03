package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplayMirrorAppliesDecisionsFromNamedSource(t *testing.T) {
	db, err := OpenDecisionLogDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatalf("OpenDecisionLogDB: %v", err)
	}
	defer db.Close()

	sc, s, logger := replayMirrorTestSetup(t, "hl-x-paper")
	sc.ReplaySourceID = "hl-x-live"
	decidedAt := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	if err := db.InsertDecision(ReplayDecision{
		StrategyID: "hl-x-live", DecisionType: ReplayDecisionOpen, DecidedAt: decidedAt,
		Symbol: "ETH", Side: "long", Quantity: 0.4, ReferencePrice: 1900,
	}); err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}

	if rows, err := db.PendingDecisions(sc.ID); err != nil || len(rows) != 0 {
		t.Fatalf("pending under the paper id = %v (err %v), want none — the live source owns the rows", rows, err)
	}

	sourceID := replayMirrorSourceID(sc)
	if sourceID != "hl-x-live" {
		t.Fatalf("source id = %q, want hl-x-live", sourceID)
	}
	syncReplayMirrorWatermarkSource(sc, s, sourceID, logger)
	pending, err := db.PendingDecisions(sourceID)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1901.0, replayTestResult(), &Config{}, logger)
	if trades != 1 || len(applied) != 1 {
		t.Fatalf("trades=%d applied=%v, want the live decision booked into the paper book", trades, applied)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 || pos.AvgCost != 1900 {
		t.Fatalf("position mismatch: %+v", pos)
	}
	if err := db.MarkDecisionsApplied(applied); err != nil {
		t.Fatalf("MarkDecisionsApplied: %v", err)
	}
	rest, err := db.PendingDecisions(sourceID)
	if err != nil {
		t.Fatalf("PendingDecisions after mark: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("pending after mark = %d, want 0", len(rest))
	}
}

func TestSyncReplayMirrorWatermarkSourceResetsOnChange(t *testing.T) {
	sc, s, _ := replayMirrorTestSetup(t, "hl-x-paper")
	var buf bytes.Buffer
	logger := &StrategyLogger{stratID: sc.ID, writer: &buf}

	s.ReplayMirrorWatermark = 41
	replayMirrorSetLastApplied(sc.ID, 41)
	if reset := syncReplayMirrorWatermarkSource(sc, s, sc.ID, logger); reset {
		t.Fatal("a legacy row with no recorded source must keep its watermark under the same id")
	}
	if s.ReplayMirrorWatermark != 41 || s.ReplayMirrorWatermarkSource != sc.ID {
		t.Fatalf("watermark=%d source=%q, want 41 / %q", s.ReplayMirrorWatermark, s.ReplayMirrorWatermarkSource, sc.ID)
	}

	if reset := syncReplayMirrorWatermarkSource(sc, s, "hl-x-live", logger); !reset {
		t.Fatal("source change must reset the watermark")
	}
	if s.ReplayMirrorWatermark != 0 {
		t.Fatalf("watermark = %d after source change, want 0", s.ReplayMirrorWatermark)
	}
	if s.ReplayMirrorWatermarkSource != "hl-x-live" {
		t.Fatalf("recorded source = %q, want hl-x-live", s.ReplayMirrorWatermarkSource)
	}
	if got := replayMirrorLastApplied(sc.ID); got != 0 {
		t.Fatalf("in-memory progress = %d after source change, want 0", got)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "source changed") || !strings.Contains(out, "hl-x-live") {
		t.Fatalf("missing source-change WARN, got: %q", out)
	}

	buf.Reset()
	if reset := syncReplayMirrorWatermarkSource(sc, s, "hl-x-live", logger); reset {
		t.Fatal("an unchanged source must not reset the watermark")
	}
	if buf.Len() != 0 {
		t.Fatalf("unchanged source logged %q", buf.String())
	}
}

func TestReplayMirrorWatermarkSourceStateRoundTrip(t *testing.T) {
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	state := NewAppState()
	s := replayTestStrategyState("hl-x-paper")
	s.ReplayMirrorWatermark = 12
	s.ReplayMirrorWatermarkSource = "hl-x-live"
	state.Strategies[s.ID] = s
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := loaded.Strategies["hl-x-paper"]
	if got == nil {
		t.Fatal("strategy missing after reload")
	}
	if got.ReplayMirrorWatermarkSource != "hl-x-live" || got.ReplayMirrorWatermark != 12 {
		t.Fatalf("reloaded watermark=%d source=%q, want 12 / hl-x-live", got.ReplayMirrorWatermark, got.ReplayMirrorWatermarkSource)
	}
	sc := StrategyConfig{ID: "hl-x-paper", Type: "perps", Platform: "hyperliquid",
		Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, ReplaySharing: ReplaySharingLiveMirror, ReplaySourceID: "hl-x-live"}
	if reset := syncReplayMirrorWatermarkSource(sc, got, replayMirrorSourceID(sc), silentStrategyLogger(sc.ID)); reset {
		t.Fatal("a restart must not reset the watermark of an unchanged source")
	}
	if got.ReplayMirrorWatermark != 12 {
		t.Fatalf("watermark = %d after restart, want 12 — replayed rows would double-book", got.ReplayMirrorWatermark)
	}
}
