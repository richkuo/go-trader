package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// threeSourceConfig builds one live partition plus three folded paper sources
// whose strategies all store under the SAME identifier, so a collision between
// two source files would be visible immediately.
func threeSourceConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		DBFile: filepath.Join(dir, "live.db"),
		PaperSources: []PaperSourceConfig{
			{ID: "btc", Label: "Paper BTC", DBFile: filepath.Join(dir, "btc.db")},
			{ID: "eth", DBFile: filepath.Join(dir, "eth.db")},
			{ID: "sol", DBFile: filepath.Join(dir, "sol.db")},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
		Strategies: []StrategyConfig{
			{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}, StorageStrategyID: "hl"},
		},
	}
	for _, id := range []string{"btc", "eth", "sol"} {
		cfg.Strategies = append(cfg.Strategies, StrategyConfig{
			ID: "hl-" + id, Type: "perps", Platform: "hyperliquid", Symbol: "ETH",
			Args: []string{"--mode=paper"}, PaperSource: id, StorageStrategyID: "hl",
		})
	}
	return cfg
}

func TestRiskPartitionText(t *testing.T) {
	cases := []struct {
		text    string
		want    RiskPartition
		wantErr bool
	}{
		{"live", livePartition, false},
		{"paper", defaultPaperPartition, false},
		{"paper:btc", paperSourcePartition("btc"), false},
		{"", unassignedPartition, false},
		{"live:btc", RiskPartition{}, true},
		{"paper:", RiskPartition{}, true},
		{"nonsense", RiskPartition{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, err := parseRiskPartition(tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRiskPartition(%q) = %v, want an error", tc.text, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseRiskPartition(%q) = %v, %v; want %v", tc.text, got, err, tc.want)
			}
			round, err := parseRiskPartition(got.String())
			if err != nil || round != tc.want {
				t.Fatalf("round trip of %v = %v, %v", tc.want, round, err)
			}
		})
	}
}

func TestActivePartitionsOrder(t *testing.T) {
	cfg := threeSourceConfig(t)
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{
		ID: "hl-unsourced", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=paper"},
	})
	want := []RiskPartition{
		livePartition,
		defaultPaperPartition,
		paperSourcePartition("btc"),
		paperSourcePartition("eth"),
		paperSourcePartition("sol"),
	}
	if got := activePartitions(cfg.Strategies); !reflect.DeepEqual(got, want) {
		t.Fatalf("activePartitions = %v, want %v", got, want)
	}
}

func TestValidatePaperSourcesConfig(t *testing.T) {
	valid := func() *Config { return threeSourceConfig(t) }
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"three sources valid", func(*Config) {}, ""},
		{"blank id", func(c *Config) { c.PaperSources[0].ID = "" }, "id is empty"},
		{"reserved id live", func(c *Config) {
			c.PaperSources[0].ID = "live"
			c.Strategies[1].PaperSource = "live"
		}, "is reserved"},
		{"reserved id primary", func(c *Config) {
			c.PaperSources[0].ID = "primary"
			c.Strategies[1].PaperSource = "primary"
		}, "is reserved"},
		{"bad id shape", func(c *Config) {
			c.PaperSources[0].ID = "BTC Spot"
			c.Strategies[1].PaperSource = "BTC Spot"
		}, "must match"},
		{"duplicate id", func(c *Config) { c.PaperSources[1].ID = "btc" }, "duplicate id"},
		{"blank db_file", func(c *Config) { c.PaperSources[0].DBFile = "" }, "db_file is empty"},
		{"unknown reference", func(c *Config) { c.Strategies[1].PaperSource = "doge" }, "not declared in paper_sources"},
		{"source on a live strategy", func(c *Config) { c.Strategies[0].PaperSource = "btc" }, "set on a live strategy"},
		{"nested paper override", func(c *Config) {
			c.PaperSources[0].PortfolioRisk = &PortfolioRiskConfig{Paper: &PortfolioRiskConfig{}}
		}, "cannot nest another override"},
		{"out-of-range source override", func(c *Config) {
			c.PaperSources[0].PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 140}
		}, "max_drawdown_pct must be in [0, 100]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			errs := strings.Join(validatePaperSourcesConfig(cfg), "\n")
			if tc.wantErr == "" {
				if errs != "" {
					t.Fatalf("valid config rejected: %s", errs)
				}
				return
			}
			if !strings.Contains(errs, tc.wantErr) {
				t.Fatalf("errors = %q, want one containing %q", errs, tc.wantErr)
			}
		})
	}
}

func TestPartitionRiskConfigLayering(t *testing.T) {
	cfg := threeSourceConfig(t)
	cfg.PortfolioRisk = &PortfolioRiskConfig{
		MaxDrawdownPct: 25, WarnThresholdPct: 60, DailyMaxLossUSD: 500,
		Paper: &PortfolioRiskConfig{MaxDrawdownPct: 40},
	}
	cfg.PaperSources[0].PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 15}

	live := partitionRiskConfig(cfg, livePartition)
	if live.MaxDrawdownPct != 25 {
		t.Errorf("live must read the root limit, got %v", live.MaxDrawdownPct)
	}
	paper := partitionRiskConfig(cfg, defaultPaperPartition)
	if paper.MaxDrawdownPct != 40 || paper.DailyMaxLossUSD != 500 {
		t.Errorf("default paper must layer the paper override over the root, got %+v", paper)
	}
	btc := partitionRiskConfig(cfg, paperSourcePartition("btc"))
	if btc.MaxDrawdownPct != 15 {
		t.Errorf("a source override must win, got %v", btc.MaxDrawdownPct)
	}
	if btc.DailyMaxLossUSD != 500 || btc.WarnThresholdPct != 60 {
		t.Errorf("a zero in a source override must inherit the layer above, got %+v", btc)
	}
	eth := partitionRiskConfig(cfg, paperSourcePartition("eth"))
	if eth.MaxDrawdownPct != 40 {
		t.Errorf("a source with no override must read the default paper layer, got %v", eth.MaxDrawdownPct)
	}
}

func TestPartitionRiskConfigIncludePausedLayering(t *testing.T) {
	cfg := threeSourceConfig(t)
	cfg.PortfolioRisk = &PortfolioRiskConfig{
		IncludePausedInWarning: true,
		Paper:                  &PortfolioRiskConfig{MaxDrawdownPct: 40},
	}
	cfg.PaperSources[0].PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 15}

	if got := partitionRiskConfig(cfg, defaultPaperPartition); !got.IncludePausedInWarning {
		t.Errorf("paper with an unset flag must inherit root true, got %+v", got)
	}
	if got := partitionRiskConfig(cfg, paperSourcePartition("btc")); !got.IncludePausedInWarning {
		t.Errorf("source with an unset flag must inherit root true, got %+v", got)
	}

	cfg.PortfolioRisk.IncludePausedInWarning = false
	cfg.PortfolioRisk.Paper.IncludePausedInWarning = true
	if got := partitionRiskConfig(cfg, defaultPaperPartition); !got.IncludePausedInWarning {
		t.Errorf("paper true override must enable paused contributors, got %+v", got)
	}
	if got := partitionRiskConfig(cfg, paperSourcePartition("btc")); !got.IncludePausedInWarning {
		t.Errorf("source with an unset flag must inherit paper true, got %+v", got)
	}
}

func TestResolveStorageLayoutPaperSources(t *testing.T) {
	cfg := threeSourceConfig(t)
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	if !layout.Split {
		t.Error("a layout with paper sources is split")
	}
	wantRoles := []storageRole{storageRolePrimary, paperSourceRole("btc"), paperSourceRole("eth"), paperSourceRole("sol")}
	if got := layout.roles(); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("lock order = %v, want %v", got, wantRoles)
	}
	// With no paper_db_file the primary still owns the unnamed paper partition.
	if got := layout.partitionsForRole(storageRolePrimary); !reflect.DeepEqual(got, []RiskPartition{livePartition, defaultPaperPartition}) {
		t.Errorf("primary partitions = %v", got)
	}
	if got := layout.roleForPartition(paperSourcePartition("eth")); got != paperSourceRole("eth") {
		t.Errorf("roleForPartition(paper:eth) = %v", got)
	}
	if got, ok := layout.partitionForRoleScope(paperSourceRole("sol"), ScopePaper); !ok || got != paperSourcePartition("sol") {
		t.Errorf("partitionForRoleScope(paper:sol, paper) = %v,%v", got, ok)
	}
	if _, ok := layout.partitionForRoleScope(paperSourceRole("sol"), ScopeLive); ok {
		t.Error("a source file must not own the live scope")
	}

	t.Run("source aliasing the primary refused", func(t *testing.T) {
		dup := threeSourceConfig(t)
		dup.PaperSources[1].DBFile = dup.DBFile
		if _, err := resolveStorageLayout(dup); err == nil || !strings.Contains(err.Error(), "same physical file") {
			t.Fatalf("err = %v, want a same-file refusal", err)
		}
	})
	t.Run("two sources on one file refused", func(t *testing.T) {
		dup := threeSourceConfig(t)
		dup.PaperSources[2].DBFile = dup.PaperSources[0].DBFile
		if _, err := resolveStorageLayout(dup); err == nil || !strings.Contains(err.Error(), "same physical file") {
			t.Fatalf("err = %v, want a same-file refusal", err)
		}
	})
	t.Run("source aliasing paper_db_file refused", func(t *testing.T) {
		dup := threeSourceConfig(t)
		dup.PaperDBFile = dup.PaperSources[0].DBFile
		if _, err := resolveStorageLayout(dup); err == nil || !strings.Contains(err.Error(), "same physical file") {
			t.Fatalf("err = %v, want a same-file refusal", err)
		}
	})
}

func TestStorageIdentityEqualStoredIDsAcrossSources(t *testing.T) {
	cfg := threeSourceConfig(t)
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	if errs := validateStorageIdentityConfig(cfg); len(errs) != 0 {
		t.Fatalf("four strategies storing as %q in four distinct files must validate: %v", "hl", errs)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}
	for _, tc := range []struct {
		procID string
		role   storageRole
		part   RiskPartition
	}{
		{"hl-live", storageRolePrimary, livePartition},
		{"hl-btc", paperSourceRole("btc"), paperSourcePartition("btc")},
		{"hl-eth", paperSourceRole("eth"), paperSourcePartition("eth")},
		{"hl-sol", paperSourceRole("sol"), paperSourcePartition("sol")},
	} {
		got, ok := ident.storageFor(tc.procID)
		if !ok || got.Role != tc.role || got.Partition != tc.part || got.StorageID != "hl" {
			t.Fatalf("storageFor(%q) = %+v,%v; want role %s partition %v id hl", tc.procID, got, ok, tc.role, tc.part)
		}
		if back, ok := ident.processFor(tc.role, "hl"); !ok || back != tc.procID {
			t.Fatalf("processFor(%s, hl) = %q,%v; want %q", tc.role, back, ok, tc.procID)
		}
	}
}

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

	// A latch in one source force-closes only that source's books.
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

	// A manual reset clears exactly one partition and persists to its own file.
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

func TestSaveFailuresPerPartition(t *testing.T) {
	cfg := threeSourceConfig(t)
	store := openSourceStore(t, cfg)
	state := sourceTestState(cfg)
	btc := paperSourcePartition("btc")

	prev := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		if role == paperSourceRole("btc") {
			return errors.New("injected btc commit failure")
		}
		return nil
	}
	outcomes := store.SaveAll(state)
	storeCommitHook = prev

	if outcomes[btc] == nil {
		t.Fatal("the injected failure must be reported for paper:btc")
	}
	if outcomes[livePartition] != nil || outcomes[paperSourcePartition("eth")] != nil {
		t.Fatal("one source's failure must not fail another partition's save")
	}
	if !store.persistenceHoldsPartition(btc) {
		t.Error("paper:btc must be held after a failed save")
	}
	if store.persistenceHoldsPartition(paperSourcePartition("eth")) || store.persistenceHoldsPartition(livePartition) {
		t.Error("only the failing partition may be held")
	}
	for i := 0; i < 2; i++ {
		store.recordSaveOutcome(btc, errors.New("again"))
	}
	if !partitionSaveBlocked(store, btc) {
		t.Error("three failures must block the partition")
	}
	if allPartitionsSaveBlocked(store, cfg) {
		t.Error("one blocked partition must not skip the whole cycle")
	}
	due := dueStrategiesPersistable(store, cfg.Strategies)
	for _, sc := range due {
		if sc.ID == "hl-btc" {
			t.Fatal("a blocked partition's strategies must be dropped from the due set")
		}
	}
	if len(due) != len(cfg.Strategies)-1 {
		t.Fatalf("due = %d strategies, want every partition but paper:btc", len(due))
	}
}

func TestDiagnosticsUpdateRoutedByRole(t *testing.T) {
	cfg := threeSourceConfig(t)
	store := openSourceStore(t, cfg)
	row := &TradeDiagnosticsRow{
		StrategyID: "hl-eth", Symbol: "ETH", Side: "long", EntryPrice: 2000, ExitPrice: 2100,
		OpenedAt: time.Unix(1700000000, 0).UTC(), ClosedAt: time.Unix(1700003600, 0).UTC(),
		MetricsStatus: diagMetricsPending,
	}
	if err := store.InsertTradeDiagnostics(row); err != nil {
		t.Fatalf("InsertTradeDiagnostics: %v", err)
	}
	if row.SourceRole != paperSourceRole("eth") {
		t.Fatalf("row.SourceRole = %q, want the owning source file", row.SourceRole)
	}
	if err := store.UpdateTradeDiagnosticsMetrics(row.SourceRole, row.RowID, "1h", nil, diagMetricsNoCandles); err != nil {
		t.Fatalf("UpdateTradeDiagnosticsMetrics: %v", err)
	}
	rows, err := store.TradeDiagnosticsRows("hl-eth")
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows: %v", err)
	}
	if len(rows) != 1 || rows[0].MetricsStatus != diagMetricsNoCandles {
		t.Fatalf("rows = %+v, want the update applied in the eth file", rows)
	}
	if err := store.UpdateTradeDiagnosticsMetrics("", row.RowID, "1h", nil, diagMetricsNoCandles); err == nil {
		t.Error("a row with no source file must be refused, never guessed")
	}
}

func TestTradeAlertRoutesSourceKeys(t *testing.T) {
	cases := []struct {
		name       string
		channels   map[string]string
		alerts     map[string]string
		dms        map[string]string
		source     string
		isLive     bool
		wantChan   string
		wantDMDest string
	}{
		{
			name:     "source channel key wins",
			channels: map[string]string{"hyperliquid": "live", "hyperliquid-paper": "paper", "hyperliquid-paper:btc": "btc"},
			source:   "btc",
			wantChan: "btc",
		},
		{
			name:     "source falls back to the paper key",
			channels: map[string]string{"hyperliquid": "live", "hyperliquid-paper": "paper"},
			source:   "btc",
			wantChan: "paper",
		},
		{
			name:     "live is unaffected by a source key",
			channels: map[string]string{"hyperliquid": "live", "hyperliquid-paper:btc": "btc"},
			isLive:   true,
			wantChan: "live",
		},
		{
			name:     "trade alert override honours the source key",
			channels: map[string]string{"hyperliquid": "live"},
			alerts:   map[string]string{"hyperliquid-paper": "alert-paper", "hyperliquid-paper:btc": "alert-btc"},
			source:   "btc",
			wantChan: "alert-btc",
		},
		{
			name:       "DM key is an exact match with no fallback",
			channels:   map[string]string{"hyperliquid": "live"},
			dms:        map[string]string{"hyperliquid-paper": "dm-paper"},
			source:     "btc",
			wantChan:   "live",
			wantDMDest: "",
		},
		{
			name:       "DM reaches the source's own key",
			channels:   map[string]string{"hyperliquid": "live"},
			dms:        map[string]string{"hyperliquid-paper:btc": "dm-btc"},
			source:     "btc",
			wantChan:   "live",
			wantDMDest: "dm-btc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mn := NewMultiNotifier(notifierBackend{
				notifier:           &mockNotifier{},
				channels:           tc.channels,
				tradeAlertChannels: tc.alerts,
				dmChannels:         tc.dms,
			})
			routes := mn.tradeAlertRoutes("hyperliquid", "perps", tc.isLive, tc.source)
			if len(routes) != 1 {
				t.Fatalf("routes = %+v, want exactly one", routes)
			}
			if routes[0].channel != tc.wantChan {
				t.Errorf("channel = %q, want %q", routes[0].channel, tc.wantChan)
			}
			if routes[0].dmDest != tc.wantDMDest {
				t.Errorf("dm destination = %q, want %q", routes[0].dmDest, tc.wantDMDest)
			}
		})
	}
}

// TestPaperKillSwitchMessageNamesItsPartition pins the operator instruction a
// paper kill-switch broadcast carries: the reply it prints must be the reply
// that clears the partition the broadcast announces, and no other. A fixed
// "reset paper" is refused when only a named source is latched and clears the
// default paper partition when both are latched.
func TestPaperKillSwitchMessageNamesItsPartition(t *testing.T) {
	cases := []struct {
		name      string
		part      RiskPartition
		wantReply string
	}{
		{"default paper", defaultPaperPartition, "reset paper"},
		{"named source", paperSourcePartition("btc"), "reset paper:btc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := formatPaperKillSwitchMessage(tc.part, "paper drawdown 50.0% exceeds limit 25.0%", []string{"hl-btc"})
			if !strings.Contains(msg, "'"+tc.wantReply+"'") {
				t.Fatalf("message must instruct %q:\n%s", tc.wantReply, msg)
			}
			if !strings.Contains(msg, strings.ToUpper(partitionLabel(tc.part))) {
				t.Errorf("message must name the partition that fired:\n%s", msg)
			}
			latched := []RiskPartition{defaultPaperPartition, paperSourcePartition("btc")}
			got, err := parseKillSwitchResetReply(tc.wantReply, latched)
			if err != nil {
				t.Fatalf("the instructed reply must parse while both partitions are latched: %v", err)
			}
			if got != tc.part {
				t.Fatalf("the instructed reply cleared %s, want %s", got, tc.part)
			}
			auto := formatPaperKillSwitchAutoResetMessage(tc.part, msg)
			if strings.Contains(auto, tc.wantReply) || !strings.Contains(auto, paperKillSwitchAutoResetLine) {
				t.Errorf("auto-reset must replace this partition's manual line:\n%s", auto)
			}
		})
	}
}

// captureStdout collects what a validation warning prints, so a test can
// assert the operator sees the exact key it must add.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = orig
	out := <-done
	r.Close()
	return out
}

// TestPaperSourceDMGapWarning pins the load-time warning for the one config
// that silently disables a routing destination the send path used before: a
// declared source keeps its plain "-paper" DM key, which tradeAlertRoutes
// stops reading the moment the strategy carries a source.
func TestPaperSourceDMGapWarning(t *testing.T) {
	sourced := StrategyConfig{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}, PaperSource: "btc"}
	cases := []struct {
		name string
		dms  map[string]string
		want string
	}{
		{
			name: "plain paper key alone warns and names the key to add",
			dms:  map[string]string{"hyperliquid-paper": "dm-paper"},
			want: `no dm_channels["hyperliquid-paper:btc"]`,
		},
		{
			name: "the source's own key warns nothing",
			dms:  map[string]string{"hyperliquid-paper": "dm-paper", "hyperliquid-paper:btc": "dm-btc"},
		},
		{
			name: "no dm_channels at all warns nothing",
		},
		{
			name: "an unrelated platform key warns nothing",
			dms:  map[string]string{"okx-paper": "dm-okx"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				PaperSources: []PaperSourceConfig{{ID: "btc", DBFile: "btc.db"}},
				Strategies:   []StrategyConfig{sourced},
				Discord:      DiscordConfig{DMChannels: tc.dms},
			}
			got := captureStdout(t, func() { warnPaperSourceDMGaps(cfg, cfg.Discord.DMChannels, "discord") })
			if tc.want == "" {
				if strings.Contains(got, "[WARN]") {
					t.Fatalf("expected no warning, got: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning must name the missing key %q, got: %s", tc.want, got)
			}
			if !strings.Contains(got, `dm_channels["hyperliquid-paper"]`) {
				t.Errorf("warning must name the key that no longer routes, got: %s", got)
			}
		})
	}
}

// TestPaperSourceRelocationRefusedBeforeAnyWrite pins the data-integrity
// contract that makes `paper_source` safe to add by hand: a strategy whose
// books still sit in another owned state file is refused at startup, before
// LoadStateWithStore and before any save, because the first full save of the
// old file deletes the row it no longer owns. The refusal comes from
// inspectStorageOwnership, which scheduler/main.go runs ahead of the load.
func TestPaperSourceRelocationRefusedBeforeAnyWrite(t *testing.T) {
	live := StrategyConfig{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}, StorageStrategyID: "hl"}
	paper := StrategyConfig{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}, StorageStrategyID: "hlp"}
	sourced := paper
	sourced.PaperSource = "btc"

	cases := []struct {
		name string
		// seed builds the deployments that already hold books on disk.
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

// savePaperSourceSeed writes one deployment's books so a later config can be
// inspected against state that already exists on disk.
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

func TestPaperSourceReloadErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unchanged reloads", func(*Config) {}, ""},
		{"source added", func(c *Config) {
			c.PaperSources = append(c.PaperSources, PaperSourceConfig{ID: "doge", DBFile: "doge.db"})
		}, "paper_sources[doge] added"},
		{"source removed", func(c *Config) { c.PaperSources = c.PaperSources[:2] }, "paper_sources[sol] removed"},
		{"db_file changed", func(c *Config) { c.PaperSources[0].DBFile = "moved.db" }, "paper_sources[btc].db_file changed"},
		{"strategy moved between sources", func(c *Config) { c.Strategies[1].PaperSource = "eth" }, "paper_source changed"},
		{"source notional cap changed", func(c *Config) {
			c.PaperSources[0].PortfolioRisk = &PortfolioRiskConfig{MaxNotionalUSD: 900}
		}, "paper_sources[btc].portfolio_risk.max_notional_usd changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := threeSourceConfig(t)
			next := threeSourceConfig(t)
			next.DBFile = cfg.DBFile
			for i := range next.PaperSources {
				next.PaperSources[i].DBFile = cfg.PaperSources[i].DBFile
			}
			tc.mutate(next)
			err := validateHotReloadCompatible(cfg, next)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("an unchanged source set must hot-reload: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
