package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	store, err := openStateStoreWith(layout, ident, openStateDBFromTemplate)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

var stateDBTemplate struct {
	once  sync.Once
	bytes []byte
	err   error
}

func migratedStateDBTemplate() ([]byte, error) {
	stateDBTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "go-trader-statedb-template-")
		if err != nil {
			stateDBTemplate.err = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "template.db")
		db, err := OpenStateDB(path)
		if err != nil {
			stateDBTemplate.err = err
			return
		}
		if err := db.Close(); err != nil {
			stateDBTemplate.err = err
			return
		}
		for _, side := range []string{path + "-wal", path + "-shm", path + "-journal"} {
			if _, err := os.Stat(side); err == nil {
				stateDBTemplate.err = fmt.Errorf("template state file left %s after close", filepath.Base(side))
				return
			}
		}
		stateDBTemplate.bytes, stateDBTemplate.err = os.ReadFile(path)
	})
	return stateDBTemplate.bytes, stateDBTemplate.err
}

func openStateDBFromTemplate(path string) (*StateDB, error) {
	if isInMemoryDBPath(path) {
		return OpenStateDB(path)
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		tmpl, err := migratedStateDBTemplate()
		if err != nil {
			return nil, fmt.Errorf("state file template: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, tmpl, 0644); err != nil {
			return nil, err
		}
		return openStateDB(path, false)
	} else if err != nil {
		return nil, err
	}
	return OpenStateDB(path)
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

func TestPaperSourceStorageAndRisk(t *testing.T) {
	t.Run("StateStoreThreeSourceRoundTrip", func(t *testing.T) {
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
	})
	t.Run("PartitionRiskIsolation", func(t *testing.T) {
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
	})
	t.Run("PaperSourceRelocationRefusedBeforeAnyWrite", func(t *testing.T) {
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
	})
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

func rebaselineTestCycle(t *testing.T, cfg *Config, state *AppState, store *StateStore, part RiskPartition, prices map[string]float64, sources cyclePriceSources, trusted bool) *scopeCycleRisk {
	t.Helper()
	inputsFrom := tradingNow().UTC()
	sr := measureScopeCycleRisk(part, partitionRiskConfig(cfg, part), cfg.Strategies, state, prices, nil, nil, true, !trusted, time.Now().UTC())
	prs := state.partitionRisk(part)
	measureScopeRebaselineValuation(sr, prs, cfg.Strategies, state, prices, sources, nil, nil, cfg.IntervalSeconds, inputsFrom, tradingNow().UTC())
	applyScopeCycleRiskCommitted(sr, prs, func(p RiskPartition) error { return store.SavePartition(state, p) })
	return sr
}

func countEvents(prs *PortfolioRiskState, typ string) int {
	n := 0
	for _, e := range prs.Events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func latchForRebaselineTest(state *AppState, part RiskPartition, peak float64) killSwitchLatchInstance {
	prs := state.partitionRisk(part)
	prs.PeakValue = peak
	prs.KillSwitchActive = true
	prs.KillSwitchAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	prs.CurrentDrawdownPct = 30
	return killSwitchLatchInstance{Partition: part, LatchedAt: prs.KillSwitchAt}
}

func TestKillSwitchResetPeakRebaseline(t *testing.T) {
	btc := paperSourcePartition("btc")
	eth := paperSourcePartition("eth")

	t.Run("OwnerResetRebaselinesFirstEligibleReadingInOnePartition", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		inst := latchForRebaselineTest(state, btc, 10000)
		state.Strategies["hl-btc"].Cash = 7400
		state.partitionRisk(eth).PeakValue = 7000
		state.partitionRisk(livePartition).PeakValue = 9000
		for part, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
			}
		}

		control := clonePortfolioRiskState(state.partitionRisk(btc))
		ResetPortfolioKillSwitchManual(&control)
		control.cancelPeakRebaseline()
		if allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(&control, partitionRiskConfig(cfg, btc), 7400, 0, 0, 0, true, true); allowed {
			t.Fatal("control: the kept peak of 10000 must re-latch a 7400 total at a 25% limit")
		}

		code := newKillSwitchResetCode()
		res := applyKillSwitchResetReply(state, killSwitchResetRequest{Code: code, Prompted: []killSwitchLatchInstance{inst}, Reply: "reset " + code},
			func(p RiskPartition) error { return store.SavePartition(state, p) })
		if res.Err != nil {
			t.Fatalf("reset: %v", res.Err)
		}
		if res.Target != btc || res.PeakValue != 10000 {
			t.Fatalf("reset result = %+v, want paper:btc with the kept peak 10000", res)
		}
		if !strings.Contains(formatKillSwitchResetAck(res), "verify open positions") {
			t.Fatalf("ack must keep the position-protection warning: %s", formatKillSwitchResetAck(res))
		}

		restarted, _, err := LoadStateWithStore(cfg, store)
		if err != nil {
			t.Fatalf("LoadStateWithStore: %v", err)
		}
		prs := restarted.partitionRisk(btc)
		if prs.KillSwitchActive || !prs.PeakRebaselinePending || prs.PeakValue != 10000 || countEvents(prs, "reset") != 1 {
			t.Fatalf("after restart: latched=%v pending=%v peak=%.2f resets=%d; want unlatched, pending, peak 10000, one reset event",
				prs.KillSwitchActive, prs.PeakRebaselinePending, prs.PeakValue, countEvents(prs, "reset"))
		}
		for _, other := range []RiskPartition{livePartition, eth, paperSourcePartition("sol")} {
			if restarted.partitionRisk(other).PeakRebaselinePending {
				t.Fatalf("the reset leaked a pending flag into %s", partitionLabel(other))
			}
		}

		restarted.partitionRisk(eth).armPeakRebaseline()
		view := formatCircuitBreakersResponse(restarted, time.Now())
		if strings.Count(view, "rebaseline WAITING") != 2 || !strings.Contains(view, "WAITING [paper:btc]") || !strings.Contains(view, "WAITING [paper:eth]") {
			t.Fatalf("the circuit-breaker view needs one waiting line per pending partition:\n%s", view)
		}
		restarted.partitionRisk(eth).cancelPeakRebaseline()

		sr := rebaselineTestCycle(t, cfg, restarted, store, btc, nil, cyclePriceSources{}, true)
		if sr.KillSwitchFired {
			t.Fatalf("the first eligible reading must not latch on equity: %s", sr.Reason)
		}
		if !sr.Rebaselined || sr.RebaselineOldPeak != 10000 || sr.RebaselineNewPeak != 7400 || sr.RebaselineSaveErr != nil {
			t.Fatalf("rebaseline = %v old=%.2f new=%.2f err=%v; want 10000 -> 7400 committed", sr.Rebaselined, sr.RebaselineOldPeak, sr.RebaselineNewPeak, sr.RebaselineSaveErr)
		}
		if view := formatCircuitBreakersResponse(restarted, time.Now()); strings.Count(view, "rebaseline WAITING") != 0 {
			t.Fatalf("the committed partition must drop its waiting line:\n%s", view)
		}
		if prs := restarted.partitionRisk(btc); prs.PeakValue != 7400 || prs.PeakRebaselinePending || prs.CurrentDrawdownPct != 0 {
			t.Fatalf("in memory: peak=%.2f pending=%v dd=%.2f", prs.PeakValue, prs.PeakRebaselinePending, prs.CurrentDrawdownPct)
		}

		again, _, err := LoadStateWithStore(cfg, store)
		if err != nil {
			t.Fatalf("second LoadStateWithStore: %v", err)
		}
		got := again.partitionRisk(btc)
		if got.PeakValue != 7400 || got.PeakRebaselinePending || countEvents(got, "reset_rebaseline") != 1 {
			t.Fatalf("committed rebaseline: peak=%.2f pending=%v events=%d", got.PeakValue, got.PeakRebaselinePending, countEvents(got, "reset_rebaseline"))
		}
		for _, e := range got.Events {
			if e.Type == "reset_rebaseline" && (!strings.Contains(e.Details, "$10000.00 -> $7400.00") || e.PeakValue != 7400) {
				t.Fatalf("rebaseline event must record both peaks: %+v", e)
			}
		}
		if again.partitionRisk(eth).PeakValue != 7000 || again.partitionRisk(livePartition).PeakValue != 9000 {
			t.Fatal("the rebaseline changed another partition's peak")
		}

		next := rebaselineTestCycle(t, cfg, again, store, btc, nil, cyclePriceSources{}, true)
		if next.Rebaselined || countEvents(again.partitionRisk(btc), "reset_rebaseline") != 1 {
			t.Fatal("a later cycle must not rebaseline again")
		}
	})

	t.Run("StaleAmbiguousOrReplacedRepliesChangeNothing", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		instBTC := latchForRebaselineTest(state, btc, 10000)
		instETH := latchForRebaselineTest(state, eth, 8000)
		saves := 0
		save := func(p RiskPartition) error { saves++; return store.SavePartition(state, p) }
		code := newKillSwitchResetCode()
		both := []killSwitchLatchInstance{instBTC, instETH}
		for _, tc := range []struct {
			name  string
			req   killSwitchResetRequest
			setup func()
		}{
			{"old prompt code", killSwitchResetRequest{Code: code, Prompted: both, Reply: "reset paper:btc " + code + "x"}, nil},
			{"no code", killSwitchResetRequest{Code: code, Prompted: both, Reply: "reset paper:btc"}, nil},
			{"bare reset with two latched", killSwitchResetRequest{Code: code, Prompted: both, Reply: "reset " + code}, nil},
			{"bare reset when the current view differs", killSwitchResetRequest{Code: code, Prompted: []killSwitchLatchInstance{instBTC}, Reply: "reset " + code}, nil},
			{"replaced latch", killSwitchResetRequest{Code: code, Prompted: both, Reply: "reset paper:btc " + code}, func() {
				state.partitionRisk(btc).KillSwitchAt = instBTC.LatchedAt.Add(time.Hour)
			}},
		} {
			if tc.setup != nil {
				tc.setup()
			}
			before := clonePortfolioRiskState(state.partitionRisk(btc))
			res := applyKillSwitchResetReply(state, tc.req, save)
			if res.Err == nil {
				t.Fatalf("%s: the reply must be refused", tc.name)
			}
			after := state.partitionRisk(btc)
			if !after.KillSwitchActive || after.PeakRebaselinePending || len(after.Events) != len(before.Events) {
				t.Fatalf("%s: a refused reply changed state", tc.name)
			}
		}
		if saves != 0 {
			t.Fatalf("a refused reply must not write state; saves=%d", saves)
		}
		if !state.partitionLatched(eth) {
			t.Fatal("paper:eth latch changed")
		}
	})

	t.Run("FailedResetSaveKeepsLatch", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		inst := latchForRebaselineTest(state, btc, 10000)
		for part, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
			}
		}
		store.saveExtra = func(tx *sql.Tx, role storageRole) error { return fmt.Errorf("injected rollback") }
		code := newKillSwitchResetCode()
		res := applyKillSwitchResetReply(state, killSwitchResetRequest{Code: code, Prompted: []killSwitchLatchInstance{inst}, Reply: "reset paper:btc " + code},
			func(p RiskPartition) error { return store.SavePartition(state, p) })
		if res.SaveErr == nil || res.Err == nil || !strings.Contains(res.Err.Error(), "NOT applied") {
			t.Fatalf("a failed save must be reported: %+v", res)
		}
		prs := state.partitionRisk(btc)
		if !prs.KillSwitchActive || prs.PeakRebaselinePending || countEvents(prs, "reset") != 0 {
			t.Fatal("a failed reset save must leave the latched state intact in memory")
		}
		store.saveExtra = nil
		reloaded, _, err := LoadStateWithStore(cfg, store)
		if err != nil {
			t.Fatalf("LoadStateWithStore: %v", err)
		}
		if !reloaded.partitionLatched(btc) || reloaded.partitionRisk(btc).PeakRebaselinePending {
			t.Fatal("the rolled-back transaction wrote the reset")
		}
	})

	t.Run("FailedRebaselineSaveKeepsOldPeakAndRetriesOnce", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		prs := state.partitionRisk(btc)
		prs.PeakValue = 10000
		prs.armPeakRebaseline()
		state.Strategies["hl-btc"].Cash = 7400
		for part, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
			}
		}
		store.saveExtra = func(tx *sql.Tx, role storageRole) error { return fmt.Errorf("injected rollback") }
		sr := rebaselineTestCycle(t, cfg, state, store, btc, nil, cyclePriceSources{}, true)
		if sr.Rebaselined || sr.RebaselineSaveErr == nil {
			t.Fatalf("a failed save must not report a rebaseline: rebaselined=%v err=%v", sr.Rebaselined, sr.RebaselineSaveErr)
		}
		if !sr.KillSwitchFired || state.partitionRisk(btc).PeakRebaselinePending {
			t.Fatal("with the old peak the 26% drawdown latches, and the new latch cancels the pending change")
		}
		if countEvents(state.partitionRisk(btc), "reset_rebaseline") != 0 {
			t.Fatal("a failed save left a rebaseline event")
		}
		if !store.persistenceHoldsPartition(btc) {
			t.Fatal("a failed rebaseline save must engage the persistence hold")
		}

		store.saveExtra = nil
		state2 := sourceTestState(cfg)
		prs2 := state2.partitionRisk(btc)
		prs2.PeakValue = 10000
		prs2.armPeakRebaseline()
		state2.Strategies["hl-btc"].Cash = 9000
		store.saveExtra = func(tx *sql.Tx, role storageRole) error { return fmt.Errorf("injected rollback") }
		sr2 := rebaselineTestCycle(t, cfg, state2, store, btc, nil, cyclePriceSources{}, true)
		if sr2.Rebaselined || sr2.KillSwitchFired || prs2.PeakValue != 10000 || !prs2.PeakRebaselinePending {
			t.Fatalf("an in-limit failed save keeps the old peak and the flag: peak=%.2f pending=%v fired=%v", prs2.PeakValue, prs2.PeakRebaselinePending, sr2.KillSwitchFired)
		}
		store.saveExtra = nil
		sr3 := rebaselineTestCycle(t, cfg, state2, store, btc, nil, cyclePriceSources{}, true)
		if !sr3.Rebaselined || prs2.PeakValue != 9000 || countEvents(prs2, "reset_rebaseline") != 1 || store.persistenceHoldsPartition(btc) {
			t.Fatalf("the retry must commit once: rebaselined=%v peak=%.2f events=%d", sr3.Rebaselined, prs2.PeakValue, countEvents(prs2, "reset_rebaseline"))
		}
	})

	t.Run("IneligibleReadingsKeepPeak", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		prs := state.partitionRisk(btc)
		prs.PeakValue = 10000
		prs.armPeakRebaseline()
		book := state.Strategies["hl-btc"]
		book.Cash = 9000
		book.Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: "hl-btc"}

		for _, tc := range []struct {
			name    string
			prices  map[string]float64
			sources cyclePriceSources
		}{
			{"missing mark (entry-cost substitution)", nil, cyclePriceSources{}},
			{"mark from another source", map[string]float64{"ETH": 2100}, cyclePriceSources{"ETH": priceSourceSpot}},
			{"mark with no source", map[string]float64{"ETH": 2100}, cyclePriceSources{}},
		} {
			sr := rebaselineTestCycle(t, cfg, state, store, btc, tc.prices, tc.sources, true)
			if sr.Rebaselined || prs.PeakValue != 10000 || !prs.PeakRebaselinePending || sr.ValuationComplete {
				t.Fatalf("%s: peak=%.2f pending=%v complete=%v", tc.name, prs.PeakValue, prs.PeakRebaselinePending, sr.ValuationComplete)
			}
			if wait := scopeRebaselineWaitReason(sr, prs); !strings.Contains(wait, "valuation is incomplete") {
				t.Fatalf("%s: wait reason %q", tc.name, wait)
			}
		}

		prs.PeakValue = 1000
		sr := rebaselineTestCycle(t, cfg, state, store, btc, nil, cyclePriceSources{}, true)
		if prs.PeakValue != 1000 || !prs.PeakRebaselinePending {
			t.Fatalf("an ineligible higher reading must not raise the pending peak: peak=%.2f", prs.PeakValue)
		}
		if sr.Rebaselined {
			t.Fatal("ineligible reading rebaselined")
		}
		prs.PeakValue = 10000

		sr = rebaselineTestCycle(t, cfg, state, store, btc, map[string]float64{"ETH": 2100}, cyclePriceSources{"ETH": priceSourceHyperliquid}, true)
		if !sr.Rebaselined || prs.PeakValue != 9100 {
			t.Fatalf("residual exposure with a complete valuation must rebaseline to 9100: rebaselined=%v peak=%.2f gap=%q", sr.Rebaselined, prs.PeakValue, sr.ValuationGap)
		}
	})

	t.Run("ReadingsBeforeTheResetCannotQualify", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		inst := latchForRebaselineTest(state, btc, 10000)
		state.Strategies["hl-btc"].Cash = 7400
		inputsFrom := tradingNow().UTC()
		sr := measureScopeCycleRisk(btc, partitionRiskConfig(cfg, btc), cfg.Strategies, state, nil, nil, nil, true, false, time.Now().UTC())
		time.Sleep(2 * time.Millisecond)
		code := newKillSwitchResetCode()
		if res := applyKillSwitchResetReply(state, killSwitchResetRequest{Code: code, Prompted: []killSwitchLatchInstance{inst}, Reply: "reset " + code},
			func(p RiskPartition) error { return store.SavePartition(state, p) }); res.Err != nil {
			t.Fatalf("reset: %v", res.Err)
		}
		prs := state.partitionRisk(btc)
		measureScopeRebaselineValuation(sr, prs, cfg.Strategies, state, nil, cyclePriceSources{}, nil, nil, cfg.IntervalSeconds, inputsFrom, tradingNow().UTC())
		applyScopeCycleRiskCommitted(sr, prs, func(p RiskPartition) error { return store.SavePartition(state, p) })
		if sr.Rebaselined {
			t.Fatal("inputs acquired before the reset authorized the new peak")
		}
		if !sr.KillSwitchFired || prs.PeakRebaselinePending {
			t.Fatal("the stale cycle measures against the kept peak; its new latch cancels the pending change")
		}
	})

	t.Run("UntrustedAndUnavailableEquityKeepExistingGuards", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		prs := state.partitionRisk(livePartition)
		prs.PeakValue = 10000
		prs.armPeakRebaseline()
		state.Strategies["hl-live"].Cash = 7400
		time.Sleep(time.Millisecond)

		sr := rebaselineTestCycle(t, cfg, state, store, livePartition, nil, cyclePriceSources{}, false)
		if sr.Rebaselined || sr.KillSwitchFired || prs.PeakValue != 10000 || !prs.PeakRebaselinePending {
			t.Fatalf("untrusted over-limit: rebaselined=%v fired=%v peak=%.2f pending=%v", sr.Rebaselined, sr.KillSwitchFired, prs.PeakValue, prs.PeakRebaselinePending)
		}
		if prs.UntrustedOverLimitSince.IsZero() {
			t.Fatal("an untrusted over-limit reading must open the existing deferral")
		}

		inputsFrom := tradingNow().UTC()
		srU := measureScopeCycleRisk(livePartition, partitionRiskConfig(cfg, livePartition), cfg.Strategies, state, nil, nil, nil, false, false, time.Now().UTC())
		srU.PerpsLoss, srU.PerpsMargin = 300, 1000
		measureScopeRebaselineValuation(srU, prs, cfg.Strategies, state, nil, cyclePriceSources{}, nil, nil, cfg.IntervalSeconds, inputsFrom, tradingNow().UTC())
		applyScopeCycleRiskCommitted(srU, prs, func(p RiskPartition) error { return store.SavePartition(state, p) })
		if srU.Rebaselined || !srU.KillSwitchFired || !strings.Contains(srU.Reason, "margin") {
			t.Fatalf("unavailable equity must keep the margin backstop: rebaselined=%v fired=%v reason=%q", srU.Rebaselined, srU.KillSwitchFired, srU.Reason)
		}
		if prs.PeakRebaselinePending || prs.PeakValue != 10000 {
			t.Fatal("the new latch cancels the pending change and keeps the peak")
		}
	})

	t.Run("PaperAutoResetWaitsForTheNextReading", func(t *testing.T) {
		cfg := threeSourceConfig(t)
		store := openSourceStore(t, cfg)
		state := sourceTestState(cfg)
		latchForRebaselineTest(state, btc, 10000)
		book := state.Strategies["hl-btc"]
		book.Cash = 7000
		book.Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: "hl-btc"}
		prices := map[string]float64{"ETH": 2200}
		sr := measureScopeCycleRisk(btc, partitionRiskConfig(cfg, btc), cfg.Strategies, state, prices, nil, nil, true, false, time.Now().UTC())
		applyScopeCycleRisk(sr, state.partitionRisk(btc))
		preCloseTotal := sr.TotalPV

		store.saveExtra = func(tx *sql.Tx, role storageRole) error { return fmt.Errorf("injected rollback") }
		out := applyPaperKillSwitchCycle(state, cfg, map[string]float64{"ETH": 1500}, sr, false)
		if !out.AutoReset {
			t.Fatal("the ownerless paper latch must auto-reset after its close pass")
		}
		if err := commitPaperKillSwitchAutoReset(state, btc, &out, func(p RiskPartition) error { return store.SavePartition(state, p) }); err == nil {
			t.Fatal("the injected rollback must fail the commit")
		}
		prs := state.partitionRisk(btc)
		if out.AutoReset || !prs.KillSwitchActive || !prs.KillSwitchCloseApplied || prs.PeakRebaselinePending || strings.Contains(out.Message, "auto-reset") {
			t.Fatalf("a failed commit restores the latch and keeps the close-pass evidence: auto=%v latched=%v closeApplied=%v", out.AutoReset, prs.KillSwitchActive, prs.KillSwitchCloseApplied)
		}
		if len(book.Positions) != 0 {
			t.Fatal("the booked close must stay booked")
		}

		store.saveExtra = nil
		sr = measureScopeCycleRisk(btc, partitionRiskConfig(cfg, btc), cfg.Strategies, state, prices, nil, nil, true, false, time.Now().UTC())
		applyScopeCycleRisk(sr, prs)
		out = applyPaperKillSwitchCycle(state, cfg, prices, sr, false)
		if err := commitPaperKillSwitchAutoReset(state, btc, &out, func(p RiskPartition) error { return store.SavePartition(state, p) }); err != nil || !out.AutoReset {
			t.Fatalf("retry commit: err=%v auto=%v", err, out.AutoReset)
		}
		if prs.PeakValue != 10000 || !prs.PeakRebaselinePending {
			t.Fatalf("the pre-close total must not become the peak: peak=%.2f pending=%v (pre-close %.2f)", prs.PeakValue, prs.PeakRebaselinePending, preCloseTotal)
		}

		time.Sleep(time.Millisecond)
		next := rebaselineTestCycle(t, cfg, state, store, btc, nil, cyclePriceSources{}, true)
		if !next.Rebaselined || prs.PeakValue != book.Cash || prs.PeakValue == preCloseTotal {
			t.Fatalf("the first eligible reading after the close sets the peak: rebaselined=%v peak=%.2f cash=%.2f pre-close=%.2f", next.Rebaselined, prs.PeakValue, book.Cash, preCloseTotal)
		}
	})
}
