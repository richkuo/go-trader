package main

import (
	"strings"
	"testing"
	"time"
)

// #1431 — paper mirror apply path. These tests mutate the package-level
// replayMirrorProgress high-water map; no t.Parallel().

func replayMirrorTestSetup(t *testing.T, id string) (StrategyConfig, *StrategyState, *StrategyLogger) {
	t.Helper()
	sc := StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid",
		Args: []string{"--mode", "paper"}, ReplaySharing: ReplaySharingLiveMirror,
	}
	s := replayTestStrategyState(id)
	logger := silentStrategyLogger(id)
	replayMirrorProgress.Lock()
	replayMirrorProgress.last = map[string]int64{}
	replayMirrorProgress.Unlock()
	t.Cleanup(func() {
		replayMirrorProgress.Lock()
		replayMirrorProgress.last = map[string]int64{}
		replayMirrorProgress.Unlock()
	})
	return sc, s, logger
}

func replayTestResult() *HyperliquidResult {
	return &HyperliquidResult{Symbol: "ETH", Price: 1900, Indicators: map[string]interface{}{}}
}

func TestMirrorReplayOpenBooksLiveFill(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	cfg := &Config{}
	decidedAt := time.Date(2026, 8, 10, 12, 53, 0, 0, time.UTC)
	pending := []ReplayDecision{
		{DecisionID: 7, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: decidedAt, Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908.25},
	}

	applied, trades, details := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), cfg, logger)
	if trades != 1 || len(applied) != 1 || applied[0] != 7 {
		t.Fatalf("trades=%d applied=%v, want 1 trade, [7]", trades, applied)
	}
	pos := s.Positions["ETH"]
	if pos == nil {
		t.Fatal("no position booked")
	}
	// Live's ACTUAL filled qty + VWAP — not paper sizing, not paper's mark.
	if pos.Quantity != 0.5 || pos.AvgCost != 1908.25 || pos.Side != "long" {
		t.Errorf("position mismatch: %+v", pos)
	}
	// Live's decision timestamp is mirrored for hold-duration analytics.
	if !pos.OpenedAt.Equal(decidedAt) {
		t.Errorf("OpenedAt = %v, want %v", pos.OpenedAt, decidedAt)
	}
	if len(details) != 1 || !strings.Contains(details[0], "REPLAY OPEN") {
		t.Errorf("details = %v", details)
	}
	// The open trade carries live's timestamp and the mirror tag.
	if len(s.TradeHistory) != 1 || !s.TradeHistory[0].Timestamp.Equal(decidedAt) {
		t.Fatalf("trade history = %+v", s.TradeHistory)
	}
	if !strings.Contains(s.TradeHistory[0].Details, "replay_live_mirror") {
		t.Errorf("open trade missing mirror tag: %q", s.TradeHistory[0].Details)
	}
}

func TestMirrorReplayShortOpen(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "short", Quantity: 0.25, ReferencePrice: 1900},
	}
	_, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Side != "short" || pos.Quantity != 0.25 {
		t.Fatalf("short position mismatch: %+v", pos)
	}
}

func TestMirrorReplayFullCloseBooksMirrorReason(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1908.25, Side: "long", Multiplier: 1}
	pending := []ReplayDecision{
		{DecisionID: 3, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900.5, CloseReason: "hl_sync_stop_loss"},
	}
	applied, trades, details := applyReplayedLiveDecisions(sc, s, pending, 1902.0, replayTestResult(), &Config{}, logger)
	if trades != 1 || len(applied) != 1 {
		t.Fatalf("trades=%d applied=%v", trades, applied)
	}
	if _, stillOpen := s.Positions["ETH"]; stillOpen {
		t.Fatal("position still open after replayed full close")
	}
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("closed positions = %d, want 1", len(s.ClosedPositions))
	}
	cp := s.ClosedPositions[0]
	// Paper books at its own current mark (1902), not live's fill (1900.5) —
	// the sanctioned replay-slippage drift.
	if cp.CloseReason != "replay_live_mirror" || cp.ClosePrice != 1902.0 {
		t.Errorf("closed position = %+v, want reason replay_live_mirror @ 1902", cp)
	}
	if len(details) != 1 || !strings.Contains(details[0], "hl_sync_stop_loss") {
		t.Errorf("details = %v — want the live close reason surfaced", details)
	}
}

func TestMirrorReplayFullCloseWhenAlreadyFlat(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	// Paper's own trailing SL beat the mirror (the trailing_stop_loss_paper
	// carve-out): the full_close row is consumed as a no-op, not an error.
	pending := []ReplayDecision{
		{DecisionID: 9, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900, CloseReason: "signal"},
	}
	applied, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || applied[0] != 9 {
		t.Fatalf("trades=%d applied=%v, want 0 trades and the row consumed", trades, applied)
	}
}

func TestMirrorReplayScaleInBlends(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	pending := []ReplayDecision{
		{DecisionID: 4, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1920},
	}
	_, trades, details := applyReplayedLiveDecisions(sc, s, pending, 1920.0, replayTestResult(), &Config{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := s.Positions["ETH"]
	if pos.Quantity != 1.0 || pos.AvgCost != 1910 {
		t.Errorf("blended position qty=%.4f avg=%.4f, want 1.0 @ 1910", pos.Quantity, pos.AvgCost)
	}
	if len(details) != 1 || !strings.Contains(details[0], "REPLAY SCALE-IN") {
		t.Errorf("details = %v", details)
	}
}

func TestMirrorReplayPartialCloseReduces(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.0, InitialQuantity: 1.0, AvgCost: 1900, Side: "long", Multiplier: 1}
	pending := []ReplayDecision{
		{DecisionID: 5, StrategyID: sc.ID, DecisionType: ReplayDecisionPartialClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.4, ReferencePrice: 1912, CloseReason: "tiered_tp"},
	}
	_, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1911.0, replayTestResult(), &Config{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Quantity != 0.6 {
		t.Fatalf("remaining = %+v, want 0.6", pos)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].Quantity != 0.4 || s.TradeHistory[0].Price != 1911.0 {
		t.Errorf("partial close trade = %+v, want 0.4 @ paper mark 1911", s.TradeHistory)
	}
}

func TestMirrorReplayDriftSkipsWithoutWedging(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	// Paper already holds a position when live's open arrives (drift) — the
	// row is consumed with a WARN, and LATER rows still apply.
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1905, CloseReason: "signal"},
	}
	applied, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1905.0, replayTestResult(), &Config{}, logger)
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want both rows consumed", applied)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (only the close applied)", trades)
	}
	if _, stillOpen := s.Positions["ETH"]; stillOpen {
		t.Fatal("drift close did not flatten the paper book")
	}
}

func TestMirrorReplayHighWaterPreventsDoubleApply(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
	}
	applied, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 2 || len(applied) != 2 {
		t.Fatalf("first pass trades=%d applied=%v, want 2/2", trades, applied)
	}
	// Simulate a MarkDecisionsApplied failure: the same rows come back next
	// cycle. The in-memory high-water must re-mark WITHOUT re-applying — a
	// repeated scale_in would double the book.
	applied, trades, _ = applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 0 {
		t.Fatalf("second pass re-applied %d trades — double-apply protection failed", trades)
	}
	if len(applied) != 2 {
		t.Fatalf("second pass applied=%v, want both re-marked", applied)
	}
	if pos := s.Positions["ETH"]; pos.Quantity != 1.0 {
		t.Fatalf("position qty = %.4f, want 1.0 (no double add)", pos.Quantity)
	}
}

func TestMirrorSuppressionWiring(t *testing.T) {
	// Structural lock (mirrors hurst_gate_wiring_test.go): the HL perps
	// dispatch must hold the mirror's open suppression through
	// pausedBlocksSignal so closes/reductions pass and only position-
	// increasing signals are held.
	src := string(mustReadFile(t, "main.go"))
	if !strings.Contains(src, "replayMirrorPaperActive(sc) && pausedBlocksSignal(") {
		t.Fatal("HL perps dispatch missing the #1431 replay-mirror suppression arm paired with pausedBlocksSignal")
	}
	if !strings.Contains(src, "applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)") {
		t.Fatal("HL perps dispatch missing the #1431 replay application call")
	}
}
