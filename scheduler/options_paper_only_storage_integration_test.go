package main

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	optionsMigrationOldID     = "rh-ccall-spy"
	optionsMigrationFreshID   = "rh-ccall-spy-paper"
	optionsMigrationLiveHLID  = "hl-live-eth"
	optionsMigrationLivePeak  = 4000.0
	optionsMigrationHLPeak    = 2600.0
	optionsMigrationRHCash    = 830.0
	optionsMigrationOptValueA = 150.0
	optionsMigrationOptValueB = 120.0
)

type optionsMigrationFixture struct {
	livePath  string
	paperPath string
	split     bool
}

func optionsMigrationOptionsStrategy(id string, mode string) StrategyConfig {
	return StrategyConfig{
		ID: id, Type: "options", Platform: "robinhood", Script: "shared_scripts/check_options.py",
		Args: []string{"covered_calls", "SPY", "--platform=robinhood", mode}, Capital: 1000, MaxDrawdownPct: 40,
	}
}

func optionsMigrationHLStrategy() StrategyConfig {
	return StrategyConfig{
		ID: optionsMigrationLiveHLID, Type: "perps", Platform: "hyperliquid", Symbol: "ETH",
		Args: []string{"momentum", "ETH", "1h", "--mode=live"}, Capital: 500, MaxDrawdownPct: 40,
	}
}

func (f optionsMigrationFixture) config(strategies ...StrategyConfig) *Config {
	cfg := &Config{DBFile: f.livePath, Strategies: strategies}
	if f.split {
		cfg.PaperDBFile = f.paperPath
	}
	return cfg
}

func newOptionsMigrationFixture(t *testing.T, split bool) optionsMigrationFixture {
	t.Helper()
	dir := t.TempDir()
	f := optionsMigrationFixture{livePath: filepath.Join(dir, "live.db"), paperPath: filepath.Join(dir, "paper.db"), split: split}
	before := f.config(optionsMigrationOptionsStrategy(optionsMigrationOldID, "--mode=live"), optionsMigrationHLStrategy())
	store := openSplitStore(t, before)

	state := NewAppState()
	rh := &StrategyState{
		ID: optionsMigrationOldID, Type: "options", Platform: "robinhood", Cash: optionsMigrationRHCash, InitialCapital: 1000,
		Positions: map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{
			"SPY-call-600-20261218": {ID: "SPY-call-600-20261218", Underlying: "SPY", OptionType: "call", Strike: 600, Expiry: "2026-12-18", Action: "sell", Quantity: 1, EntryPremium: 1.5, EntryPremiumUSD: 150, CurrentValueUSD: optionsMigrationOptValueA, OpenedAt: time.Unix(10, 0).UTC()},
			"SPY-put-560-20261218":  {ID: "SPY-put-560-20261218", Underlying: "SPY", OptionType: "put", Strike: 560, Expiry: "2026-12-18", Action: "buy", Quantity: 1, EntryPremium: 1.2, EntryPremiumUSD: 120, CurrentValueUSD: optionsMigrationOptValueB, OpenedAt: time.Unix(11, 0).UTC()},
		},
	}
	rh.RiskState.PeakValue = 1100
	rh.RiskState.PendingCircuitCloses = map[string]*PendingCircuitClose{
		PlatformPendingCloseRobinhoodOptions: {
			OperatorRequired: true,
			Symbols: []PendingCircuitCloseSymbol{
				{Symbol: "SPY-call-600-20261218", Size: 1},
				{Symbol: "SPY-put-560-20261218", Size: 1},
			},
		},
	}
	RecordTrade(rh, Trade{StrategyID: optionsMigrationOldID, Symbol: "SPY-call-600-20261218", Side: "sell", Quantity: 1, Price: 150, Timestamp: time.Unix(10, 0).UTC()})
	state.Strategies[optionsMigrationOldID] = rh

	hl := &StrategyState{
		ID: optionsMigrationLiveHLID, Type: "perps", Platform: "hyperliquid", Cash: 500, InitialCapital: 500,
		Positions:       map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000}},
		OptionPositions: map[string]*OptionPosition{},
	}
	hl.RiskState.PeakValue = optionsMigrationHLPeak
	state.Strategies[optionsMigrationLiveHLID] = hl
	state.partitionRisk(livePartition).PeakValue = optionsMigrationLivePeak

	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("save pre-change fixture: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close pre-change store: %v", err)
	}
	if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", optionsMigrationOldID); n != 2 {
		t.Fatalf("fixture primary option_positions for %s = %d, want 2", optionsMigrationOldID, n)
	}
	return f
}

func optionsMigrationCount(t *testing.T, path, query string, args ...interface{}) int {
	t.Helper()
	raw, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatalf("openStateDBReadOnly(%s): %v", path, err)
	}
	defer raw.Close()
	var n int
	if err := raw.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q on %s: %v", query, path, err)
	}
	return n
}

func optionsMigrationLiveValue(t *testing.T, cfg *Config, state *AppState) float64 {
	t.Helper()
	v, fallback := computeSubsetPortfolioValue(strategiesInPartition(cfg.Strategies, livePartition), state, map[string]float64{}, nil, nil)
	if fallback {
		t.Fatalf("live value used a shared-wallet fallback")
	}
	return v
}

func TestOptionsLiveModeMigrationSingleFileSameIdentity(t *testing.T) {
	f := newOptionsMigrationFixture(t, false)
	before := f.config(optionsMigrationOptionsStrategy(optionsMigrationOldID, "--mode=live"), optionsMigrationHLStrategy())
	beforeStore := openSplitStore(t, before)
	beforeState, _, err := LoadStateWithStore(before, beforeStore)
	if err != nil {
		t.Fatalf("load with the pre-change config: %v", err)
	}
	liveBefore := optionsMigrationLiveValue(t, before, beforeState)
	if err := beforeStore.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	after := f.config(optionsMigrationOptionsStrategy(optionsMigrationOldID, "--mode=paper"), optionsMigrationHLStrategy())
	si, err := inspectStorageLayoutForConfig(after, false)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !si.OK() {
		t.Fatalf("single-file same-identity inspection rejected: %v", si.Rejections)
	}
	store := openSplitStore(t, after)
	state, orphans, err := LoadStateWithStore(after, store)
	if err != nil {
		t.Fatalf("load with the migrated config: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("orphans = %+v, want none", orphans)
	}
	rh := state.Strategies[optionsMigrationOldID]
	if rh == nil {
		t.Fatalf("strategy %s did not reload; roster = %v", optionsMigrationOldID, state.Strategies)
	}
	if rh.Cash != optionsMigrationRHCash {
		t.Errorf("cash = %.2f, want the old simulated cash %.2f", rh.Cash, optionsMigrationRHCash)
	}
	if len(rh.OptionPositions) != 2 {
		t.Errorf("option positions = %d, want the 2 old simulated positions", len(rh.OptionPositions))
	}
	pending := rh.RiskState.PendingCircuitCloses[PlatformPendingCloseRobinhoodOptions]
	if pending == nil || !pending.OperatorRequired || len(pending.Symbols) != 2 {
		t.Errorf("pending close = %+v, want the saved operator-required robinhood_options close", pending)
	}
	if !planOperatorRequiredWarning(state, tradingNow().UTC()).HasEntries() {
		t.Error("the saved operator-required close no longer produces a warning")
	}
	if p := partitionFor(after.Strategies[0]); p != defaultPaperPartition {
		t.Errorf("partition = %+v, want the default paper partition", p)
	}
	if got := state.partitionRisk(livePartition).PeakValue; got != optionsMigrationLivePeak {
		t.Errorf("live peak = %.2f, want the saved %.2f (no prune, no rebaseline)", got, optionsMigrationLivePeak)
	}
	moved := optionsMigrationRHCash + optionsMigrationOptValueA + optionsMigrationOptValueB
	liveAfter := optionsMigrationLiveValue(t, after, state)
	if math.Abs((liveBefore-liveAfter)-moved) > 1e-9 {
		t.Errorf("live value fell by %.2f, want the moved book value %.2f (before %.2f, after %.2f)", liveBefore-liveAfter, moved, liveBefore, liveAfter)
	}
}

func TestOptionsLiveModeMigrationSplitSameIdentity(t *testing.T) {
	f := newOptionsMigrationFixture(t, true)
	after := f.config(optionsMigrationOptionsStrategy(optionsMigrationOldID, "--mode=paper"), optionsMigrationHLStrategy())
	si, err := inspectStorageLayoutForConfig(after, false)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if si.OK() {
		t.Fatal("split same-identity inspection passed, want an ownership rejection")
	}
	want := `primary state file: stored strategy "rh-ccall-spy" is mapped to the paper state file; move or re-map it by hand`
	found := false
	for _, r := range si.Rejections {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("rejections = %v, want %q", si.Rejections, want)
	}
	if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", optionsMigrationOldID); n != 2 {
		t.Errorf("primary option_positions after inspection = %d, want 2 (inspection is read-only)", n)
	}
}

func TestOptionsLiveModeMigrationFreshIdentity(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, keepAsFresh := range []bool{true, false} {
			name := "single-file"
			if split {
				name = "split"
			}
			if keepAsFresh {
				name += "/fresh-id"
			} else {
				name += "/removed"
			}
			t.Run(name, func(t *testing.T) {
				f := newOptionsMigrationFixture(t, split)
				strategies := []StrategyConfig{optionsMigrationHLStrategy()}
				if keepAsFresh {
					strategies = append(strategies, optionsMigrationOptionsStrategy(optionsMigrationFreshID, "--mode=paper"))
				}
				after := f.config(strategies...)
				si, err := inspectStorageLayoutForConfig(after, false)
				if err != nil {
					t.Fatalf("inspect: %v", err)
				}
				if !si.OK() {
					t.Fatalf("inspection rejected: %v", si.Rejections)
				}
				var inspected []storageStrategyRow
				for _, fi := range si.Files {
					if fi.Role == storageRolePrimary {
						inspected = fi.Orphans
					}
				}
				if len(inspected) != 1 || inspected[0].StorageID != optionsMigrationOldID || inspected[0].PositionCount != 2 {
					t.Errorf("storage-inspect primary orphans = %+v, want only %s with a position count of 2", inspected, optionsMigrationOldID)
				}
				if report := formatStorageInspection(si); !strings.Contains(report, "    ORPHAN "+optionsMigrationOldID+" (2 position(s))  ** holds positions **\n") {
					t.Errorf("storage-inspect report lacks the orphan line and position marker:\n%s", report)
				}
				store := openSplitStore(t, after)
				state, orphans, err := LoadStateWithStore(after, store)
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if len(orphans) != 1 {
					t.Fatalf("orphans = %+v, want exactly one", orphans)
				}
				if orphans[0].StorageID != optionsMigrationOldID || orphans[0].Role != storageRolePrimary || orphans[0].PositionCount != 2 {
					t.Errorf("orphan = %+v, want %s in the primary file with a position count of 2", orphans[0], optionsMigrationOldID)
				}
				if _, adopted := state.Strategies[optionsMigrationOldID]; adopted {
					t.Fatal("the old options book was keyed into the roster")
				}
				if keepAsFresh {
					if _, exists := state.Strategies[optionsMigrationFreshID]; exists {
						t.Fatalf("fresh id %s loaded a stored book", optionsMigrationFreshID)
					}
				}
				if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", optionsMigrationOldID); n != 2 {
					t.Errorf("option_positions before the first save = %d, want 2", n)
				}

				if got := state.partitionRisk(livePartition).PeakValue; got != optionsMigrationLivePeak {
					t.Fatalf("loaded live peak = %.2f, want %.2f", got, optionsMigrationLivePeak)
				}
				if got := rebaselinePortfolioPeakAfterPruneForPartition(state, after, livePartition, nil); got != optionsMigrationHLPeak {
					t.Errorf("live rebaseline = %.2f, want the remaining live strategy peak %.2f", got, optionsMigrationHLPeak)
				}

				if err := SaveStateWithStore(state, store); err != nil {
					t.Fatalf("first save: %v", err)
				}
				if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM strategies WHERE id = ?", optionsMigrationOldID); n != 0 {
					t.Errorf("primary strategies rows for %s after save = %d, want 0", optionsMigrationOldID, n)
				}
				if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", optionsMigrationOldID); n != 0 {
					t.Errorf("primary option_positions for %s after save = %d, want 0", optionsMigrationOldID, n)
				}
				if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM trades WHERE strategy_id = ?", optionsMigrationOldID); n != 1 {
					t.Errorf("primary trades for %s after save = %d, want the 1 old row kept", optionsMigrationOldID, n)
				}
				if n := optionsMigrationCount(t, f.livePath, "SELECT COUNT(*) FROM strategies WHERE id = ?", optionsMigrationLiveHLID); n != 1 {
					t.Errorf("primary strategies rows for %s after save = %d, want 1", optionsMigrationLiveHLID, n)
				}
			})
		}
	}
}
