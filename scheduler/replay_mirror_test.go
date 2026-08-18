package main

import (
	"path/filepath"
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

// ─── review-round-1 regression tests (atomicity, stamps, index) ─────────────

func TestMirrorReplayPersistedWatermarkSurvivesRestart(t *testing.T) {
	// Review finding 1, must-survive case (2)+(3): the process crashed AFTER
	// SaveState persisted the open (row 1) + scale-in (row 2) and the
	// watermark, but BEFORE MarkDecisionsApplied flipped the shared rows. On
	// restart the in-memory high-water is empty; rows 1-2 come back pending.
	// The persisted watermark must re-mark them WITHOUT re-applying — a
	// repeated scale_in would double the book — while the genuinely new row 3
	// still applies.
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.0, InitialQuantity: 1.0, AvgCost: 1905, Side: "long", Multiplier: 1}
	s.ReplayMirrorWatermark = 2
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
		{DecisionID: 3, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1920},
	}
	applied, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1920.0, replayTestResult(), &Config{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (only row 3 re-applied)", trades)
	}
	if len(applied) != 3 {
		t.Fatalf("applied = %v, want all three rows (re-)marked", applied)
	}
	if pos := s.Positions["ETH"]; pos.Quantity != 1.5 {
		t.Fatalf("position qty = %.4f, want 1.5 (no double add of rows 1-2)", pos.Quantity)
	}
	if s.ReplayMirrorWatermark != 3 {
		t.Fatalf("watermark = %d, want 3", s.ReplayMirrorWatermark)
	}
}

func TestMirrorReplayWatermarkAdvancesOnApply(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 7, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if s.ReplayMirrorWatermark != 7 {
		t.Fatalf("watermark = %d, want 7", s.ReplayMirrorWatermark)
	}
}

func TestMirrorReplayOpenSeedsLiveStamps(t *testing.T) {
	// Review optional 1: live's open-time EntryATR/regime ride the row, and the
	// mirror seeds them even when paper's own payload disagrees on the bar.
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	result := replayTestResult()
	result.Indicators["atr"] = 99.0 // paper disagrees with live's 42.5
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900, EntryATR: 42.5, Regime: "trending_up"},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, result, &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := s.Positions["ETH"]
	if pos.EntryATR != 42.5 {
		t.Errorf("EntryATR = %v, want live's 42.5 (not paper's 99)", pos.EntryATR)
	}
	if pos.Regime != "trending_up" {
		t.Errorf("Regime = %q, want live's trending_up", pos.Regime)
	}
}

func TestMirrorReplayOpenFallsBackToPaperStamps(t *testing.T) {
	// Rows written before the entry_atr/regime columns existed (0/"") keep the
	// paper-payload stamps.
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	result := replayTestResult()
	result.Indicators["atr"] = 33.0
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, result, &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if pos := s.Positions["ETH"]; pos.EntryATR != 33.0 {
		t.Errorf("EntryATR = %v, want paper's 33 (no live stamp on the row)", pos.EntryATR)
	}
}

func TestMirrorSaveBeforeMarkWiring(t *testing.T) {
	// Structural lock for review finding 1's invariant: inside the HL perps
	// replay block the state save (book + watermark, one transaction) must run
	// BEFORE the shared log's mark-applied.
	src := string(mustReadFile(t, "main.go"))
	applyIdx := strings.Index(src, "applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)")
	if applyIdx < 0 {
		t.Fatal("replay application call not found")
	}
	saveIdx := strings.Index(src[applyIdx:], "SaveStateWithDB(state, cfg, stateDB)")
	markIdx := strings.Index(src[applyIdx:], "decisionLog.MarkDecisionsApplied(appliedIDs)")
	if saveIdx < 0 || markIdx < 0 {
		t.Fatalf("replay block missing save (%d) or mark (%d) after apply", saveIdx, markIdx)
	}
	if saveIdx > markIdx {
		t.Fatal("MarkDecisionsApplied runs BEFORE SaveStateWithDB — a kill in the gap drops a mirrored trade")
	}
}

func TestMirrorReplaySuspendsEagerTradePersist(t *testing.T) {
	// Review optional (PR #1435 round 2): apply must not InsertTrade before
	// SaveState, so a kill during the save cannot leave a trade row while
	// rolling back the watermark.
	src := string(mustReadFile(t, "replay_mirror.go"))
	if !strings.Contains(src, "defer suspendEagerTradePersist()()") {
		t.Fatal("applyReplayedLiveDecisions missing suspendEagerTradePersist — replayed trades would eager-insert before the watermark save")
	}

	var inserts int
	prev := tradeRecorder
	tradeRecorder = func(string, Trade) error {
		inserts++
		return nil
	}
	t.Cleanup(func() { tradeRecorder = prev })

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger); trades != 2 {
		t.Fatalf("trades = %d, want 2", trades)
	}
	if inserts != 0 {
		t.Fatalf("eager InsertTrade ran %d time(s) during apply, want 0", inserts)
	}
	if len(s.TradeHistory) != 2 {
		t.Fatalf("TradeHistory = %d, want 2 in-memory rows", len(s.TradeHistory))
	}
	for i, tr := range s.TradeHistory {
		if tr.persisted {
			t.Fatalf("TradeHistory[%d].persisted = true, want false so SaveState flushes the row in the watermark tx", i)
		}
	}
}

func TestMirrorReplayKillDuringSaveDoesNotDuplicateTrades(t *testing.T) {
	// Must-survive (1)+(2): production eager hook is armed, apply books
	// open+scale-in, then the process is killed BEFORE SaveState. Restart
	// re-applies from pending (watermark was never persisted). Trades must
	// appear once after the post-restart save — not twice from the first
	// apply's eager insert plus the re-apply.
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()
	prev := tradeRecorder
	tradeRecorder = sdb.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger); trades != 2 {
		t.Fatalf("first apply trades = %d, want 2", trades)
	}
	_, n, err := sdb.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 100, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory after unsaved apply: %v", err)
	}
	if n != 0 {
		t.Fatalf("trades in DB after unsaved apply = %d, want 0 (eager persist would have written them)", n)
	}

	// Kill: in-memory high-water and watermark are gone; pending rows return.
	sc2, s2, logger2 := replayMirrorTestSetup(t, "hl-paper-eth")
	if _, trades, _ := applyReplayedLiveDecisions(sc2, s2, pending, 1910.0, replayTestResult(), &Config{}, logger2); trades != 2 {
		t.Fatalf("restart re-apply trades = %d, want 2", trades)
	}
	state := NewAppState()
	state.Strategies[s2.ID] = s2
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	_, n, err = sdb.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 100, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory after restart save: %v", err)
	}
	if n != 2 {
		t.Fatalf("trades after restart re-apply+save = %d, want 2 (not a duplicate 4)", n)
	}
}

func TestMirrorReplayTradesFlushWithWatermarkSave(t *testing.T) {
	// Must-survive (3): with eager persist off, SaveState's unpersisted-trade
	// flush is the only persist, and it shares the watermark transaction.
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()
	prev := tradeRecorder
	tradeRecorder = sdb.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 7, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	state := NewAppState()
	state.Strategies[s.ID] = s
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := loaded.Strategies[sc.ID]
	if got == nil {
		t.Fatal("strategy missing after reload")
	}
	if got.ReplayMirrorWatermark != 7 {
		t.Fatalf("watermark = %d, want 7", got.ReplayMirrorWatermark)
	}
	if len(got.TradeHistory) != 1 || got.TradeHistory[0].Quantity != 0.5 {
		t.Fatalf("loaded trades = %+v, want 1 open of 0.5", got.TradeHistory)
	}
	_, n, err := sdb.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 100, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if n != 1 {
		t.Fatalf("DB trades = %d, want 1", n)
	}
}

func TestSystemdTemplateGrantsSharedReplayDir(t *testing.T) {
	// Review finding 2: two template instances must both be able to write the
	// shared replay_log_path under ProtectSystem=strict.
	src := string(mustReadFile(t, "../systemd/go-trader@.service"))
	if !strings.Contains(src, "StateDirectory=go-trader/%i go-trader/shared") {
		t.Fatal("template unit StateDirectory missing go-trader/shared — template instances cannot write the shared replay log path")
	}
	if !strings.Contains(src, "ProtectSystem=strict") {
		t.Fatal("template unit lost ProtectSystem=strict")
	}
}
