package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type paperFundingTestClock struct{ ms int64 }

func (c *paperFundingTestClock) set(ms int64)   { c.ms = ms }
func (c *paperFundingTestClock) now() time.Time { return time.UnixMilli(c.ms).UTC() }

func withPaperFundingClock(t *testing.T, startMs int64) *paperFundingTestClock {
	t.Helper()
	c := &paperFundingTestClock{ms: startMs}
	prev := paperFundingClock
	paperFundingClock = c.now
	t.Cleanup(func() { paperFundingClock = prev })
	return c
}

func paperFundingSplitConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		DBFile:       filepath.Join(dir, "live.db"),
		PaperDBFile:  filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "btc", DBFile: filepath.Join(dir, "btc.db")}},
		Strategies: []StrategyConfig{
			{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=live"}, Capital: 1000},
			{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 1000},
			{ID: "hl-src", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 1000, PaperSource: "btc"},
		},
	}
}

func paperFundingFreshState(cfg *Config) *AppState {
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	return state
}

func paperFundingStateJSON(t *testing.T, s *StrategyState) string {
	t.Helper()
	raw, err := marshalPaperFundingState(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func paperFundingColumn(t *testing.T, sdb *StateDB, storageID string) (string, bool) {
	t.Helper()
	var raw string
	err := sdb.db.QueryRow("SELECT paper_funding_state FROM strategies WHERE id = ?", storageID).Scan(&raw)
	if err != nil {
		return "", false
	}
	return raw, true
}

func TestPaperFundingStatePersistsInOwningFileAcrossSaveAndBookSave(t *testing.T) {
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	cfg := paperFundingSplitConfig(t)
	store := openSplitStore(t, cfg)
	state := paperFundingFreshState(cfg)
	state.Strategies["hl-paper"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 2, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: "hl-paper", TradePositionID: "pos-eth-1"}
	state.Strategies["hl-src"].Positions["BTC"] = &Position{Symbol: "BTC", Side: "short", Quantity: 0.5, AvgCost: 60000, Multiplier: 1, OwnerStrategyID: "hl-src", TradePositionID: "pos-btc-1"}
	state.Strategies["hl-live"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: "hl-live"}
	syncPaperFundingEligibility(state, cfg.Strategies, clock.now())

	if state.Strategies["hl-live"].PaperFunding != nil {
		t.Fatal("a live strategy must never get paper funding state")
	}
	paper := state.Strategies["hl-paper"]
	if c := paper.PaperFunding.Coins["ETH"]; c == nil || c.Anchor.SignedQty != 2 || c.Anchor.PositionID != "pos-eth-1" {
		t.Fatalf("an open paper position must be anchored at sync: %+v", paper.PaperFunding.Coins["ETH"])
	}
	if c := state.Strategies["hl-src"].PaperFunding.Coins["BTC"]; c == nil || c.Anchor.SignedQty != -0.5 {
		t.Fatalf("a short paper position must anchor a negative signed quantity: %+v", c)
	}

	clock.set(1_800_000_600_000)
	if !bookPerpsPartialCloseWithFillFee(paper, "ETH", 0.5, 2100, 0, false, "", "test", "test", "test", nil) {
		t.Fatal("partial close did not book")
	}
	c := paper.PaperFunding.Coins["ETH"]
	if len(c.Segments) != 1 || c.Segments[0].SignedQty != 2 || c.Segments[0].FromMs != 1_800_000_000_000 || c.Segments[0].ToMs != 1_800_000_600_000 || c.Anchor.SignedQty != 1.5 || c.Anchor.SinceMs != 1_800_000_600_000 {
		t.Fatalf("a partial close must close the 2.0 segment at the change time and re-anchor at 1.5: %+v", c)
	}

	for part, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}
	if raw, ok := paperFundingColumn(t, store.file(storageRolePrimary), "hl-live"); !ok || raw != "" {
		t.Fatalf("the live row must carry no paper funding state: %q %t", raw, ok)
	}
	if _, ok := paperFundingColumn(t, store.file(storageRolePrimary), "hl-paper"); ok {
		t.Fatal("a paper strategy row must not land in the primary file when paper_db_file is set")
	}
	if raw, ok := paperFundingColumn(t, store.file(storageRolePaper), "hl-paper"); !ok || raw != paperFundingStateJSON(t, paper) {
		t.Fatalf("the paper file must hold hl-paper's state: %q", raw)
	}
	if _, ok := paperFundingColumn(t, store.file(storageRolePaper), "hl-src"); ok {
		t.Fatal("a paper source strategy must not land in the default paper file")
	}
	if raw, ok := paperFundingColumn(t, store.file(paperSourceRole("btc")), "hl-src"); !ok || raw != paperFundingStateJSON(t, state.Strategies["hl-src"]) {
		t.Fatalf("the source file must hold hl-src's state: %q", raw)
	}

	reloaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paperFundingStateJSON(t, reloaded.Strategies["hl-paper"]), paperFundingStateJSON(t, paper); got != want {
		t.Fatalf("partition save round trip:\n got %s\nwant %s", got, want)
	}
	if reloaded.Strategies["hl-live"].PaperFunding != nil {
		t.Fatal("a reloaded live strategy must have no paper funding state")
	}

	clock.set(1_800_001_200_000)
	rp := reloaded.Strategies["hl-paper"]
	if !bookPerpsCloseWithFillFee(rp, "ETH", 2200, 0, false, "", "test", "test", "test", nil) {
		t.Fatal("close did not book")
	}
	if err := store.SaveStrategyBook(rp); err != nil {
		t.Fatalf("SaveStrategyBook: %v", err)
	}
	again, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	got := again.Strategies["hl-paper"].PaperFunding.Coins["ETH"]
	if got == nil || got.Anchor.SignedQty != 0 || len(got.Segments) != 2 || got.Segments[1].SignedQty != 1.5 || got.Segments[1].ToMs != 1_800_001_200_000 {
		t.Fatalf("book save round trip must keep the closed 1.5 segment and a flat anchor: %+v", got)
	}

	sdb, err := OpenStateDB(cfg.PaperDBFile)
	if err != nil {
		t.Fatalf("re-running migrations on an existing file: %v", err)
	}
	sdb.Close()
	sdb, err = OpenStateDB(cfg.PaperDBFile)
	if err != nil {
		t.Fatalf("re-running migrations twice: %v", err)
	}
	defer sdb.Close()
	if _, err := sdb.db.Exec(`INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('legacy-row', 'perps', 'hyperliquid', 5, 5)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if raw, ok := paperFundingColumn(t, sdb, "legacy-row"); !ok || raw != "" {
		t.Fatalf("a row written without the column must read back empty: %q %t", raw, ok)
	}
}

func TestPaperFundingNegativeCashAndEligibilityChangesSurviveRestart(t *testing.T) {
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	cfg := paperFundingSplitConfig(t)
	store := openSplitStore(t, cfg)
	state := paperFundingFreshState(cfg)
	syncPaperFundingEligibility(state, cfg.Strategies, clock.now())
	state.Strategies["hl-paper"].Cash = -3.25
	state.Strategies["hl-live"].Cash = -4
	for part, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}
	reloaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	ValidateState(reloaded, cfg.Strategies)
	if got := reloaded.Strategies["hl-paper"].Cash; got != -3.25 {
		t.Fatalf("an eligible paper strategy must keep negative cash across restart, got %v", got)
	}
	if got := reloaded.Strategies["hl-live"].Cash; got != 0 {
		t.Fatalf("a live strategy keeps the existing negative-cash clamp, got %v", got)
	}
	if reloaded.Strategies["hl-paper"].PaperFunding == nil {
		t.Fatal("paper funding state must reload")
	}

	switched := *cfg
	switched.Strategies = append([]StrategyConfig(nil), cfg.Strategies...)
	switched.Strategies[1].Args = []string{"sma", "ETH", "1h", "--mode=live"}
	lines := syncPaperFundingEligibility(reloaded, switched.Strategies, clock.now())
	if reloaded.Strategies["hl-paper"].PaperFunding != nil {
		t.Fatal("a strategy that became live must drop its paper funding state")
	}
	dropped := 0
	for _, l := range lines {
		if strings.Contains(l, "hl-paper") {
			dropped++
		}
	}
	if dropped != 1 {
		t.Fatalf("the drop must log exactly one line for the strategy, got %v", lines)
	}

	corrupt := reloaded.Strategies["hl-src"]
	unmarshalPaperFundingState(corrupt, `{"v":1,"started_at_ms":0}`)
	if !corrupt.paperFundingCorrupt || corrupt.PaperFunding != nil {
		t.Fatal("invalid stored state must be marked corrupt")
	}
	syncPaperFundingEligibility(reloaded, cfg.Strategies, clock.now())
	if corrupt.PaperFunding != nil {
		t.Fatal("sync must never reset corrupt state")
	}
	if raw := paperFundingStateJSON(t, corrupt); raw != `{"v":1,"started_at_ms":0}` {
		t.Fatalf("a corrupt state must save its raw text unchanged, got %q", raw)
	}
}

func TestPaperFundingHelperTracksEveryBookChangeAndFlagsBypass(t *testing.T) {
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	sc := StrategyConfig{ID: "hl-p", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 10000,
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	state := NewAppState()
	s := NewStrategyState(sc)
	state.Strategies[sc.ID] = s
	syncPaperFundingEligibility(state, []StrategyConfig{sc}, clock.now())
	logger := silentStrategyLogger(sc.ID)

	clock.set(1_800_000_100_000)
	if n, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "", 0, DirectionBoth, 0, logger); err != nil || n != 1 {
		t.Fatalf("open: n=%d err=%v", n, err)
	}
	pid := s.Positions["ETH"].TradePositionID
	clock.set(1_800_000_200_000)
	if n, _ := applyPerpsScaleIn(s, sc, "ETH", 2010, 0.25, 0, "", false, logger); n != 1 {
		t.Fatal("scale-in did not book")
	}
	clock.set(1_800_000_300_000)
	applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionOpen, Side: "sell", HedgeSide: "short", Qty: 0.01, NewBasis: 0.75}, 0.01, 60000, 0, false, "", logger)
	clock.set(1_800_000_400_000)
	if n, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2020, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 0, logger); err != nil || n == 0 {
		t.Fatalf("flip: n=%d err=%v", n, err)
	}
	eth := s.PaperFunding.Coins["ETH"]
	if len(eth.Segments) != 2 || eth.Segments[0].SignedQty != 0.5 || eth.Segments[0].ToMs != 1_800_000_200_000 ||
		eth.Segments[1].SignedQty != 0.75 || eth.Segments[1].PositionID != pid || eth.Segments[1].ToMs != 1_800_000_400_000 {
		t.Fatalf("ETH segments must record 0.5 then 0.75 on the first position: %+v", eth.Segments)
	}
	if eth.Anchor.SignedQty >= 0 || eth.Anchor.PositionID == pid || eth.Anchor.SinceMs != 1_800_000_400_000 {
		t.Fatalf("a flip must anchor the new short position at the flip time: %+v", eth.Anchor)
	}
	if btc := s.PaperFunding.Coins["BTC"]; btc == nil || btc.Anchor.SignedQty != -0.01 || btc.Anchor.SinceMs != 1_800_000_300_000 {
		t.Fatalf("the hedge leg must be anchored on its own coin: %+v", btc)
	}
	if alerts := s.PaperFunding.drainAlerts(); len(alerts) != 0 {
		t.Fatalf("hooked changes must raise no alert: %+v", alerts)
	}

	clock.set(1_800_000_500_000)
	s.Positions["ETH"].Quantity *= 2
	clock.set(1_800_000_600_000)
	observePaperFunding(s, clock.ms)
	if len(eth.Unknown) != 1 || eth.Unknown[0].FromMs != 1_800_000_400_000 || eth.Unknown[0].ToMs != 1_800_000_600_000 {
		t.Fatalf("a direct book write must become an explicit unknown window from the last check: %+v", eth.Unknown)
	}
	alerts := s.PaperFunding.drainAlerts()
	if len(alerts) != 1 || !alerts[0].Critical || alerts[0].Kind != "helper_bypass" {
		t.Fatalf("a bypass must raise one CRITICAL alert: %+v", alerts)
	}

	live := hlLivePerps("hl-l", "ETH", 1000)
	ls := NewStrategyState(live)
	state.Strategies[live.ID] = ls
	syncPaperFundingEligibility(state, []StrategyConfig{sc, live}, clock.now())
	if _, err := ExecutePerpsSignalWithLeverage(ls, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.1, "", 0, DirectionBoth, 0, silentStrategyLogger(live.ID)); err != nil {
		t.Fatal(err)
	}
	if ls.PaperFunding != nil {
		t.Fatal("a live strategy must never gain paper funding state through the helper")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(paperFundingStateJSON(t, s)), &probe); err != nil {
		t.Fatalf("state must marshal to valid JSON: %v", err)
	}
}
