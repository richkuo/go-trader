package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openSourceStore(t *testing.T, cfg *Config) *StateStore {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sourceTestState(cfg *Config) *AppState {
	state := NewAppState()
	for i, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = &StrategyState{
			ID: sc.ID, Type: sc.Type, Platform: sc.Platform,
			Cash: float64(1000 * (i + 1)), InitialCapital: float64(1000 * (i + 1)),
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
		}
	}
	return state
}

func TestStateStoreThreeSourceRoundTrip(t *testing.T) {
	cfg := threeSourceConfig(t)
	store := openSourceStore(t, cfg)
	state := sourceTestState(cfg)
	ts := time.Unix(1700000000, 0).UTC()
	for i, sc := range cfg.Strategies {
		s := state.Strategies[sc.ID]
		s.Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: float64(i + 1), AvgCost: 2000, OwnerStrategyID: sc.ID}
		RecordTrade(s, Trade{StrategyID: sc.ID, Symbol: "ETH", Side: "buy", Quantity: float64(i + 1), Price: 2000, Timestamp: ts})
	}
	for i, part := range activePartitions(cfg.Strategies) {
		state.partitionRisk(part).PeakValue = float64(1000 * (i + 1))
	}
	for part, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}

	reloaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("LoadStateWithStore: %v", err)
	}
	for i, sc := range cfg.Strategies {
		s := reloaded.Strategies[sc.ID]
		if s == nil {
			t.Fatalf("%s lost its book across restart", sc.ID)
		}
		pos := s.Positions["ETH"]
		if pos == nil || pos.Quantity != float64(i+1) {
			t.Fatalf("%s book crossed files: %+v", sc.ID, pos)
		}
		if len(s.TradeHistory) != 1 || s.TradeHistory[0].Quantity != float64(i+1) {
			t.Fatalf("%s trade history crossed files: %+v", sc.ID, s.TradeHistory)
		}
	}
	for i, part := range activePartitions(cfg.Strategies) {
		prs := reloaded.partitionRiskIfPresent(part)
		if prs == nil || prs.PeakValue != float64(1000*(i+1)) {
			t.Fatalf("%s peak did not survive restart: %+v", partitionLabel(part), prs)
		}
	}
}

func TestPartitionRiskIsolation(t *testing.T) {
	cfg := threeSourceConfig(t)
	store := openSourceStore(t, cfg)
	state := sourceTestState(cfg)
	btc := paperSourcePartition("btc")
	eth := paperSourcePartition("eth")

	btcPrs := state.partitionRisk(btc)
	btcPrs.PeakValue = 5000
	btcPrs.KillSwitchActive = true
	btcPrs.CurrentDrawdownPct = 45
	ethPrs := state.partitionRisk(eth)
	ethPrs.PeakValue = 7000
	livePrs := state.partitionRisk(livePartition)
	livePrs.PeakValue = 9000

	if got := state.latchedPartitions(); !reflect.DeepEqual(got, []RiskPartition{btc}) {
		t.Fatalf("latchedPartitions = %v, want only paper:btc", got)
	}
	if state.partitionLatched(eth) || state.partitionLatched(livePartition) {
		t.Fatal("one source's latch must not latch another partition")
	}

	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, OwnerStrategyID: sc.ID}
	}
	closed := forceClosePaperScopePositions(state, cfg, btc, map[string]float64{"ETH": 1000})
	if !reflect.DeepEqual(closed, []string{"hl-btc"}) {
		t.Fatalf("force-closed = %v, want only hl-btc", closed)
	}
	if len(state.Strategies["hl-eth"].Positions) != 1 || len(state.Strategies["hl-live"].Positions) != 1 {
		t.Fatal("a source latch must never close another partition's books")
	}

	ResetPortfolioKillSwitchManual(state.partitionRisk(btc))
	if state.partitionLatched(btc) {
		t.Error("the reset must clear the named source")
	}
	if err := store.SavePartition(state, eth); err != nil {
		t.Fatalf("SavePartition(paper:eth): %v", err)
	}
	reloaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("LoadStateWithStore: %v", err)
	}
	if prs := reloaded.partitionRiskIfPresent(eth); prs == nil || prs.PeakValue != 7000 {
		t.Fatalf("paper:eth peak = %+v, want 7000 from its own file", prs)
	}
	if prs := reloaded.partitionRiskIfPresent(btc); prs != nil && prs.PeakValue != 0 {
		t.Fatalf("saving paper:eth must not write paper:btc's file: %+v", prs)
	}
}

func TestPaperSourceRelocationRefusedBeforeAnyWrite(t *testing.T) {
	live := StrategyConfig{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}, StorageStrategyID: "hl"}
	paper := StrategyConfig{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}, StorageStrategyID: "hlp"}
	sourced := paper
	sourced.PaperSource = "btc"

	cases := []struct {
		name       string
		seed       func(dir string) []*Config
		after      func(dir string) *Config
		wantRefuse bool
	}{
		{
			name: "books still in the primary when the source is added",
			seed: func(dir string) []*Config {
				return []*Config{{DBFile: filepath.Join(dir, "live.db"), Strategies: []StrategyConfig{live, paper}}}
			},
			after: func(dir string) *Config {
				return &Config{
					DBFile:       filepath.Join(dir, "live.db"),
					PaperSources: []PaperSourceConfig{{ID: "btc", DBFile: filepath.Join(dir, "btc.db")}},
					Strategies:   []StrategyConfig{live, sourced},
				}
			},
			wantRefuse: true,
		},
		{
			name: "books still in the paper file when the source is added",
			seed: func(dir string) []*Config {
				return []*Config{{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"), Strategies: []StrategyConfig{live, paper}}}
			},
			after: func(dir string) *Config {
				return &Config{
					DBFile:       filepath.Join(dir, "live.db"),
					PaperDBFile:  filepath.Join(dir, "paper.db"),
					PaperSources: []PaperSourceConfig{{ID: "btc", DBFile: filepath.Join(dir, "btc.db")}},
					Strategies:   []StrategyConfig{live, sourced},
				}
			},
			wantRefuse: true,
		},
		{
			name: "the documented fold, where the source file already holds the books",
			seed: func(dir string) []*Config {
				return []*Config{
					{DBFile: filepath.Join(dir, "live.db"), Strategies: []StrategyConfig{live}},
					{DBFile: filepath.Join(dir, "btc.db"), Strategies: []StrategyConfig{paper}},
				}
			},
			after: func(dir string) *Config {
				return &Config{
					DBFile:       filepath.Join(dir, "live.db"),
					PaperSources: []PaperSourceConfig{{ID: "btc", DBFile: filepath.Join(dir, "btc.db")}},
					Strategies:   []StrategyConfig{live, sourced},
				}
			},
		},
		{
			name: "a retired strategy still prunes instead of refusing",
			seed: func(dir string) []*Config {
				return []*Config{{DBFile: filepath.Join(dir, "live.db"), Strategies: []StrategyConfig{live, paper}}}
			},
			after: func(dir string) *Config {
				return &Config{DBFile: filepath.Join(dir, "live.db"), Strategies: []StrategyConfig{live}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, seed := range tc.seed(dir) {
				savePaperSourceSeed(t, seed)
			}
			after := tc.after(dir)
			layout, err := resolveStorageLayout(after)
			if err != nil {
				t.Fatalf("resolveStorageLayout: %v", err)
			}
			ident, err := buildStorageIdentityMap(after, layout)
			if err != nil {
				t.Fatalf("buildStorageIdentityMap: %v", err)
			}
			si, err := inspectStorageOwnership(layout, ident, after, false)
			if err != nil {
				t.Fatalf("inspectStorageOwnership: %v", err)
			}
			if tc.wantRefuse {
				if si.OK() {
					t.Fatalf("a relocated book must stop startup before any save; rejections = %v", si.Rejections)
				}
				joined := strings.Join(si.Rejections, " ")
				if !strings.Contains(joined, "hlp") || !strings.Contains(joined, string(paperSourceRole("btc"))) {
					t.Errorf("the refusal must name the moved book and the file that now owns it: %v", si.Rejections)
				}
				return
			}
			if !si.OK() {
				t.Fatalf("startup must not refuse here: %v", si.Rejections)
			}
		})
	}
}

func savePaperSourceSeed(t *testing.T, cfg *Config) {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	defer store.Close()
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = &StrategyState{
			ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 2222, InitialCapital: 2000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
		}
	}
	for part, saveErr := range store.SaveAll(state) {
		if saveErr != nil {
			t.Fatalf("SaveAll(%s): %v", part, saveErr)
		}
	}
}
