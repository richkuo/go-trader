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
