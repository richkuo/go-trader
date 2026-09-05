package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	replayDriftAlerts.reset()
	t.Cleanup(func() {
		replayMirrorProgress.Lock()
		replayMirrorProgress.last = map[string]int64{}
		replayMirrorProgress.Unlock()
		replayDriftAlerts.reset()
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

	applied, trades, details, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), cfg, logger)
	if trades != 1 || len(applied) != 1 || applied[0] != 7 {
		t.Fatalf("trades=%d applied=%v, want 1 trade, [7]", trades, applied)
	}
	pos := s.Positions["ETH"]
	if pos == nil {
		t.Fatal("no position booked")
	}
	if pos.Quantity != 0.5 || pos.AvgCost != 1908.25 || pos.Side != "long" {
		t.Errorf("position mismatch: %+v", pos)
	}
	if !pos.OpenedAt.Equal(decidedAt) {
		t.Errorf("OpenedAt = %v, want %v", pos.OpenedAt, decidedAt)
	}
	if len(details) != 1 || !strings.Contains(details[0], "REPLAY OPEN") {
		t.Errorf("details = %v", details)
	}
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
	_, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger)
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
	applied, trades, details, _ := applyReplayedLiveDecisions(sc, s, pending, 1902.0, replayTestResult(), &Config{}, logger)
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
	if cp.CloseReason != "replay_live_mirror" || cp.ClosePrice != 1902.0 {
		t.Errorf("closed position = %+v, want reason replay_live_mirror @ 1902", cp)
	}
	if len(details) != 1 || !strings.Contains(details[0], "hl_sync_stop_loss") {
		t.Errorf("details = %v — want the live close reason surfaced", details)
	}
}

func TestMirrorReplayFullCloseWhenAlreadyFlat(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 9, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900, CloseReason: "signal"},
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger)
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
	_, trades, details, _ := applyReplayedLiveDecisions(sc, s, pending, 1920.0, replayTestResult(), &Config{}, logger)
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
	_, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1911.0, replayTestResult(), &Config{}, logger)
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
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1905, CloseReason: "signal"},
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1905.0, replayTestResult(), &Config{}, logger)
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
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 2 || len(applied) != 2 {
		t.Fatalf("first pass trades=%d applied=%v, want 2/2", trades, applied)
	}
	applied, trades, _, _ = applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
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
	src := string(mustReadFile(t, "main.go"))
	if !strings.Contains(src, "replayMirrorPaperActive(sc) && pausedBlocksSignal(") {
		t.Fatal("HL perps dispatch missing the #1431 replay-mirror suppression arm paired with pausedBlocksSignal")
	}
	if !strings.Contains(src, "applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)") {
		t.Fatal("HL perps dispatch missing the #1431 replay application call")
	}
	if !strings.Contains(src, "detail = mergeTradeDetails(detail, replayDetails...)") {
		t.Fatal("replay block overwrites detail instead of merging — a same-cycle paper SL close would vanish from the digest")
	}
	if strings.Contains(src, "detail = strings.Join(replayDetails, \"; \")") {
		t.Fatal("replay block still last-wins overwrites detail with joined replay text")
	}
}

func TestMergeTradeDetails(t *testing.T) {
	const nativeSL = "[hl-paper-eth] PAPER TRAILING SL ETH @ $1900.00"
	const nativeATR = "[hl-paper-eth] PAPER FIXED ATR SL ETH @ $1890.00"

	t.Run("preserves earlier native action", func(t *testing.T) {
		replay := []string{"[hl-paper-eth] REPLAY OPEN long ETH 0.500000 @ $1908.25"}
		got := mergeTradeDetails(nativeSL, replay...)
		if want := nativeSL + "; " + replay[0]; got != want {
			t.Fatalf("merge = %q, want %q", got, want)
		}
	})

	t.Run("joins multiple replay rows without dropping native", func(t *testing.T) {
		got := mergeTradeDetails(nativeATR,
			"[hl-paper-eth] REPLAY SCALE-IN ETH +0.500000 @ $1910.00",
			"[hl-paper-eth] REPLAY CLOSE ETH @ $1905.00 (live reason: signal)",
		)
		if !strings.HasPrefix(got, nativeATR+"; ") {
			t.Fatalf("native detail was eclipsed: %q", got)
		}
		if !strings.Contains(got, "REPLAY SCALE-IN") || !strings.Contains(got, "REPLAY CLOSE") {
			t.Fatalf("missing a replay fragment: %q", got)
		}
	})

	t.Run("empty and blank replay parts keep native", func(t *testing.T) {
		if got := mergeTradeDetails(nativeSL); got != nativeSL {
			t.Fatalf("empty parts dropped native: %q", got)
		}
		if got := mergeTradeDetails(nativeSL, "", ""); got != nativeSL {
			t.Fatalf("blank parts dropped native: %q", got)
		}
	})

	t.Run("empty existing detail does not leave a leading separator", func(t *testing.T) {
		got := mergeTradeDetails("", "[hl-paper-eth] REPLAY OPEN long ETH 0.5 @ $1900")
		if !strings.Contains(got, "REPLAY OPEN") || strings.HasPrefix(got, "; ") {
			t.Fatalf("empty existing produced %q", got)
		}
	})
}

func TestMirrorReplayPersistedWatermarkSurvivesRestart(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.0, InitialQuantity: 1.0, AvgCost: 1905, Side: "long", Multiplier: 1}
	s.ReplayMirrorWatermark = 2
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
		{DecisionID: 3, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1920},
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1920.0, replayTestResult(), &Config{}, logger)
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
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if s.ReplayMirrorWatermark != 7 {
		t.Fatalf("watermark = %d, want 7", s.ReplayMirrorWatermark)
	}
}

func TestMirrorReplayOpenSeedsLiveStamps(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	result := replayTestResult()
	result.Indicators["atr"] = 99.0
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900, EntryATR: 42.5, Regime: "trending_up"},
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, result, &Config{}, logger); trades != 1 {
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
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	result := replayTestResult()
	result.Indicators["atr"] = 33.0
	pending := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, result, &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if pos := s.Positions["ETH"]; pos.EntryATR != 33.0 {
		t.Errorf("EntryATR = %v, want paper's 33 (no live stamp on the row)", pos.EntryATR)
	}
}

func TestMirrorSaveBeforeMarkWiring(t *testing.T) {
	src := string(mustReadFile(t, "main.go"))
	applyIdx := strings.Index(src, "applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)")
	if applyIdx < 0 {
		t.Fatal("replay application call not found")
	}
	saveIdx := strings.Index(src[applyIdx:], "SaveStrategyBookWithDB(stratState, stratDB)")
	markIdx := strings.Index(src[applyIdx:], "decisionLog.MarkDecisionsApplied(appliedIDs)")
	if saveIdx < 0 || markIdx < 0 {
		t.Fatalf("replay block missing save (%d) or mark (%d) after apply", saveIdx, markIdx)
	}
	if saveIdx > markIdx {
		t.Fatal("MarkDecisionsApplied runs BEFORE SaveStrategyBookWithDB — a kill in the gap drops a mirrored trade")
	}
	if strings.Contains(src[applyIdx:applyIdx+markIdx+len("decisionLog.MarkDecisionsApplied(appliedIDs)")], "SaveStateWithStore(state, store)") {
		t.Fatal("replay block still calls full-fleet SaveStateWithDB — persist cost would grow with unrelated strategies")
	}
}

func TestMirrorReplaySuspendsEagerTradePersist(t *testing.T) {
	src := string(mustReadFile(t, "replay_mirror.go"))
	if !strings.Contains(src, "defer suspendEagerTradePersist()()") {
		t.Fatal("applyReplayedLiveDecisions missing suspendEagerTradePersist — replayed trades would eager-insert before the watermark save")
	}
	if !strings.Contains(src, "defer suspendEagerDiagnosticsPersist()()") {
		t.Fatal("applyReplayedLiveDecisions missing suspendEagerDiagnosticsPersist — replayed full-closes would eager-insert diagnostics before the watermark save")
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
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger); trades != 2 {
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
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger); trades != 2 {
		t.Fatalf("first apply trades = %d, want 2", trades)
	}
	_, n, err := sdb.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 100, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory after unsaved apply: %v", err)
	}
	if n != 0 {
		t.Fatalf("trades in DB after unsaved apply = %d, want 0 (eager persist would have written them)", n)
	}

	sc2, s2, logger2 := replayMirrorTestSetup(t, "hl-paper-eth")
	if _, trades, _, _ := applyReplayedLiveDecisions(sc2, s2, pending, 1910.0, replayTestResult(), &Config{}, logger2); trades != 2 {
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
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
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
	src := string(mustReadFile(t, "../systemd/go-trader@.service"))
	if !strings.Contains(src, "StateDirectory=go-trader/%i go-trader/shared") {
		t.Fatal("template unit StateDirectory missing go-trader/shared — template instances cannot write the shared replay log path")
	}
	if !strings.Contains(src, "ProtectSystem=strict") {
		t.Fatal("template unit lost ProtectSystem=strict")
	}
}

func TestSaveStrategyBookDoesNotRewriteUnrelatedStrategies(t *testing.T) {
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	unrelated := NewStrategyState(StrategyConfig{ID: "hl-other", Type: "perps", Platform: "hyperliquid", Capital: 50000})
	unrelated.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, InitialQuantity: 1, AvgCost: 60000, Side: "long", Multiplier: 1}
	for i := 0; i < 50; i++ {
		unrelated.TradeHistory = append(unrelated.TradeHistory, Trade{
			StrategyID: unrelated.ID, Symbol: "BTC", Quantity: 0.01, Price: 60000, persisted: true,
		})
	}
	fleet := NewAppState()
	fleet.Strategies[unrelated.ID] = unrelated
	if err := sdb.SaveState(fleet); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	pending := []ReplayDecision{
		{DecisionID: 7, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s); err != nil {
		t.Fatalf("SaveStrategyBook: %v", err)
	}

	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	other := loaded.Strategies[unrelated.ID]
	if other == nil {
		t.Fatal("SaveStrategyBook deleted the unrelated strategy — it used a fleet rewrite")
	}
	if other.Cash != 50000 {
		t.Fatalf("unrelated cash = %v, want 50000", other.Cash)
	}
	if pos := other.Positions["BTC"]; pos == nil || pos.Quantity != 1 {
		t.Fatalf("unrelated position = %+v, want BTC qty 1", pos)
	}
	got := loaded.Strategies[sc.ID]
	if got == nil || got.ReplayMirrorWatermark != 7 {
		t.Fatalf("mirrored strategy = %+v, want watermark 7", got)
	}
	if pos := got.Positions["ETH"]; pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("mirrored position = %+v, want ETH qty 0.5", pos)
	}
}

func TestMirrorReplayFullCloseDefersDiagnosticsUntilSave(t *testing.T) {
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	var inserts int
	prevRec := tradeDiagnosticsRecorder
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		inserts++
		return sdb.InsertTradeDiagnostics(row)
	}
	t.Cleanup(func() { tradeDiagnosticsRecorder = prevRec })

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1,
		TradePositionID: "pos-replay-eth", OpenedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	pending := []ReplayDecision{
		{DecisionID: 3, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900.5, CloseReason: "signal"},
	}

	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1902.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("first apply trades = %d, want 1", trades)
	}
	if inserts != 0 {
		t.Fatalf("eager diagnostics insert ran %d time(s) during apply, want 0", inserts)
	}
	if len(s.pendingTradeDiagnostics) != 1 {
		t.Fatalf("pending diagnostics = %d, want 1 buffered for the save tx", len(s.pendingTradeDiagnostics))
	}
	if s.pendingTradeDiagnostics[0].PositionID != "pos-replay-eth" {
		t.Fatalf("pending position_id = %q, want pos-replay-eth (identity reused on retry)", s.pendingTradeDiagnostics[0].PositionID)
	}

	rows, err := sdb.TradeDiagnosticsRows(sc.ID)
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows after unsaved apply: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("diagnostics in DB after unsaved apply = %d, want 0", len(rows))
	}

	sc2, s2, logger2 := replayMirrorTestSetup(t, "hl-paper-eth")
	s2.Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1,
		TradePositionID: "pos-replay-eth", OpenedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc2, s2, pending, 1902.0, replayTestResult(), &Config{}, logger2); trades != 1 {
		t.Fatalf("second apply trades = %d, want 1", trades)
	}
	rows, err = sdb.TradeDiagnosticsRows(sc.ID)
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows after second unsaved apply: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("diagnostics in DB after second unsaved apply = %d, want 0", len(rows))
	}

	if err := sdb.SaveStrategyBook(s2); err != nil {
		t.Fatalf("SaveStrategyBook: %v", err)
	}
	rows, err = sdb.TradeDiagnosticsRows(sc.ID)
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows after save: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("diagnostics after save = %d, want 1", len(rows))
	}
	if rows[0].PositionID != "pos-replay-eth" || rows[0].CloseReason != "replay_live_mirror" {
		t.Fatalf("diagnostics row = %+v, want pos-replay-eth / replay_live_mirror", rows[0])
	}
	if len(s2.pendingTradeDiagnostics) != 0 {
		t.Fatalf("pending diagnostics after commit = %d, want 0", len(s2.pendingTradeDiagnostics))
	}

	sc3, s3, logger3 := replayMirrorTestSetup(t, "hl-paper-eth")
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadState returned nil after SaveStrategyBook — app_state seed missing")
	}
	s3 = loaded.Strategies[sc.ID]
	if s3 == nil {
		t.Fatal("mirrored strategy missing after save")
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc3, s3, pending, 1902.0, replayTestResult(), &Config{}, logger3); trades != 0 {
		t.Fatalf("watermark skip re-applied %d trades", trades)
	}
	if err := sdb.SaveStrategyBook(s3); err != nil {
		t.Fatalf("second SaveStrategyBook: %v", err)
	}
	rows, err = sdb.TradeDiagnosticsRows(sc.ID)
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows after watermark skip: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("diagnostics after watermark skip = %d, want 1 (not a duplicate)", len(rows))
	}
}

func TestSaveStrategyBookReplacesOpenPositionAcrossCycles(t *testing.T) {
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	open := []ReplayDecision{
		{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900},
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, open, 1900.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("open trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s); err != nil {
		t.Fatalf("SaveStrategyBook after open: %v", err)
	}

	add := []ReplayDecision{
		{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionScaleIn, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1910},
	}
	if _, trades, _, _ := applyReplayedLiveDecisions(sc, s, add, 1910.0, replayTestResult(), &Config{}, logger); trades != 1 {
		t.Fatalf("scale-in trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s); err != nil {
		t.Fatalf("SaveStrategyBook after scale-in: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil || loaded == nil {
		t.Fatalf("LoadState after scale-in: loaded=%v err=%v", loaded, err)
	}
	got := loaded.Strategies[sc.ID]
	if got == nil {
		t.Fatal("strategy missing after scale-in save")
	}
	if pos := got.Positions["ETH"]; pos == nil || pos.Quantity != 1.0 {
		t.Fatalf("after scale-in position = %+v, want ETH qty 1.0", pos)
	}

	partial := []ReplayDecision{
		{DecisionID: 3, StrategyID: sc.ID, DecisionType: ReplayDecisionPartialClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.25, ReferencePrice: 1912},
	}
	sc2, s2, logger2 := replayMirrorTestSetup(t, "hl-paper-eth")
	s2 = got
	if _, trades, _, _ := applyReplayedLiveDecisions(sc2, s2, partial, 1911.0, replayTestResult(), &Config{}, logger2); trades != 1 {
		t.Fatalf("partial-close trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s2); err != nil {
		t.Fatalf("SaveStrategyBook after partial-close: %v", err)
	}
	loaded, err = sdb.LoadState()
	if err != nil || loaded == nil {
		t.Fatalf("LoadState after partial-close: loaded=%v err=%v", loaded, err)
	}
	got = loaded.Strategies[sc.ID]
	if pos := got.Positions["ETH"]; pos == nil || pos.Quantity != 0.75 {
		t.Fatalf("after partial-close position = %+v, want ETH qty 0.75", pos)
	}

	full := []ReplayDecision{
		{DecisionID: 4, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.75, ReferencePrice: 1905, CloseReason: "signal"},
	}
	sc3, s3, logger3 := replayMirrorTestSetup(t, "hl-paper-eth")
	s3 = got
	if _, trades, _, _ := applyReplayedLiveDecisions(sc3, s3, full, 1905.0, replayTestResult(), &Config{}, logger3); trades != 1 {
		t.Fatalf("full-close trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s3); err != nil {
		t.Fatalf("SaveStrategyBook after full-close: %v", err)
	}
	loaded, err = sdb.LoadState()
	if err != nil || loaded == nil {
		t.Fatalf("LoadState after full-close: loaded=%v err=%v", loaded, err)
	}
	got = loaded.Strategies[sc.ID]
	if pos := got.Positions["ETH"]; pos != nil && pos.Quantity > 0 {
		t.Fatalf("after full-close resurrected position = %+v, want flat", pos)
	}
	var posRows int
	if err := sdb.db.QueryRow(`SELECT COUNT(*) FROM positions WHERE strategy_id = ?`, sc.ID).Scan(&posRows); err != nil {
		t.Fatalf("count positions after full-close: %v", err)
	}
	if posRows != 0 {
		t.Fatalf("positions rows after full-close = %d, want 0 (stale row would resurrect on LoadState)", posRows)
	}

	reopen := []ReplayDecision{
		{DecisionID: 5, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: 0.2, ReferencePrice: 1920},
	}
	sc4, s4, logger4 := replayMirrorTestSetup(t, "hl-paper-eth")
	s4 = got
	if _, trades, _, _ := applyReplayedLiveDecisions(sc4, s4, reopen, 1920.0, replayTestResult(), &Config{}, logger4); trades != 1 {
		t.Fatalf("reopen trades = %d, want 1", trades)
	}
	if err := sdb.SaveStrategyBook(s4); err != nil {
		t.Fatalf("SaveStrategyBook after reopen: %v", err)
	}
	loaded, err = sdb.LoadState()
	if err != nil || loaded == nil {
		t.Fatalf("LoadState after reopen: loaded=%v err=%v", loaded, err)
	}
	got = loaded.Strategies[sc.ID]
	if pos := got.Positions["ETH"]; pos == nil || pos.Quantity != 0.2 {
		t.Fatalf("after reopen position = %+v, want ETH qty 0.2", pos)
	}
}
