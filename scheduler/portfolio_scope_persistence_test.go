package main

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func portfolioRiskTableColumns(t *testing.T, db *StateDB, table string) map[string]bool {
	t.Helper()
	rows, err := db.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = pk == 1
	}
	return cols
}

func TestMigrateSchema_PortfolioRiskScope_Idempotent(t *testing.T) {
	db := openTestDB(t)

	cols := portfolioRiskTableColumns(t, db, "portfolio_risk")
	if _, ok := cols["scope"]; !ok {
		t.Fatal("portfolio_risk must carry a scope column after migration")
	}
	if !cols["scope"] {
		t.Error("scope must be the portfolio_risk primary key")
	}
	if _, ok := cols["id"]; ok {
		t.Error("the legacy id column must be gone after migration")
	}
	if _, ok := portfolioRiskTableColumns(t, db, "kill_switch_events")["scope"]; !ok {
		t.Error("kill_switch_events must carry a scope column")
	}
	corr := portfolioRiskTableColumns(t, db, "correlation_snapshot")
	if !corr["scope"] {
		t.Error("scope must be the correlation_snapshot primary key")
	}

	state := NewAppState()
	prs := state.partitionRisk(livePartition)
	prs.PeakValue = 4321
	prs.KillSwitchActive = true
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.migrateSchema(); err != nil {
			t.Fatalf("re-run migrateSchema (%d): %v", i, err)
		}
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.partitionRisk(livePartition); got.PeakValue != 4321 || !got.KillSwitchActive {
		t.Errorf("a repeated migration must be a no-op; got %+v", got)
	}
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM portfolio_risk").Scan(&count); err != nil {
		t.Fatalf("count portfolio_risk: %v", err)
	}
	if count != 1 {
		t.Errorf("portfolio_risk rows = %d, want 1", count)
	}
}

func TestMigrateSchema_PortfolioRiskScope_LegacyRowBecomesUnassigned(t *testing.T) {
	db := openTestDB(t)
	if err := db.SaveState(NewAppState()); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}

	if _, err := db.db.Exec("DROP TABLE portfolio_risk"); err != nil {
		t.Fatalf("drop scoped table: %v", err)
	}
	if _, err := db.db.Exec(`CREATE TABLE portfolio_risk (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		peak_value REAL NOT NULL DEFAULT 0,
		current_drawdown_pct REAL NOT NULL DEFAULT 0,
		current_margin_drawdown_pct REAL NOT NULL DEFAULT 0,
		kill_switch_active INTEGER NOT NULL DEFAULT 0,
		kill_switch_at TEXT NOT NULL DEFAULT '',
		warning_sent INTEGER NOT NULL DEFAULT 0,
		warn_band_entered_at TEXT NOT NULL DEFAULT '',
		last_warning_equity_dd_pct REAL NOT NULL DEFAULT 0,
		last_warning_margin_dd_pct REAL NOT NULL DEFAULT 0,
		warning_equity_delta_pct REAL NOT NULL DEFAULT 0,
		warning_margin_delta_pct REAL NOT NULL DEFAULT 0,
		manual_mark_basis_rebaselined INTEGER NOT NULL DEFAULT 0,
		drawdown_reading_substituted INTEGER NOT NULL DEFAULT 0,
		untrusted_over_limit_since TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("recreate legacy table: %v", err)
	}
	if _, err := db.db.Exec(`INSERT INTO portfolio_risk (id, peak_value, current_drawdown_pct, kill_switch_active, kill_switch_at)
		VALUES (1, 9876, 41.5, 1, '2026-04-01T12:00:00Z')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	legacy := loaded.PortfolioRisk[unassignedPartition]
	if legacy == nil {
		t.Fatal("the legacy row must load under the unassigned sentinel")
	}
	if legacy.PeakValue != 9876 || !legacy.KillSwitchActive {
		t.Errorf("the legacy latch and peak must survive migration: %+v", legacy)
	}
}

func TestAssignLegacyPortfolioScope(t *testing.T) {
	cases := []struct {
		name  string
		cfgs  []StrategyConfig
		want  RiskPartition
		moved bool
	}{
		{"live only lands in live", []StrategyConfig{scopeCfg("l", true)}, livePartition, true},
		{"paper only lands in paper", []StrategyConfig{scopeCfg("p", false)}, defaultPaperPartition, true},
		{"empty roster lands in paper", nil, defaultPaperPartition, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := NewAppState()
			state.PortfolioRisk[unassignedPartition] = &PortfolioRiskState{PeakValue: 5000, KillSwitchActive: true}
			state.CorrelationSnapshot[unassignedPartition] = &CorrelationSnapshot{PortfolioGrossUSD: 111}
			got, moved := assignLegacyPortfolioScope(state, &Config{Strategies: tc.cfgs})
			if moved != tc.moved || got != tc.want {
				t.Fatalf("assignLegacyPortfolioScope = %q,%v; want %q,%v", got, moved, tc.want, tc.moved)
			}
			if _, still := state.PortfolioRisk[unassignedPartition]; still {
				t.Error("the sentinel row must be removed")
			}
			prs := state.partitionRisk(tc.want)
			if prs.PeakValue != 5000 || !prs.KillSwitchActive {
				t.Errorf("the latch and peak must move intact: %+v", prs)
			}
			if snap := state.partitionCorrelation(tc.want); snap == nil || snap.PortfolioGrossUSD != 111 {
				t.Errorf("the correlation snapshot must move too: %+v", snap)
			}
			if tc.want == defaultPaperPartition && !prs.ManualMarkBasisRebaselined {
				t.Error("a paper row must never run the live manual-mark basis migration")
			}
			if _, again := assignLegacyPortfolioScope(state, &Config{Strategies: tc.cfgs}); again {
				t.Error("a second placement pass must find nothing to move")
			}
		})
	}

	t.Run("already assigned untouched", func(t *testing.T) {
		state := NewAppState()
		state.partitionRisk(livePartition).PeakValue = 7777
		if _, moved := assignLegacyPortfolioScope(state, &Config{Strategies: []StrategyConfig{scopeCfg("l", true)}}); moved {
			t.Error("a scoped-only state must report nothing moved")
		}
		if state.partitionRisk(livePartition).PeakValue != 7777 {
			t.Error("an existing scoped row must be untouched")
		}
	})

	t.Run("conflict ORs the latch", func(t *testing.T) {
		state := NewAppState()
		kept := state.partitionRisk(livePartition)
		kept.PeakValue = 1000
		state.PortfolioRisk[unassignedPartition] = &PortfolioRiskState{PeakValue: 5000, KillSwitchActive: true}
		got, moved := assignLegacyPortfolioScope(state, &Config{Strategies: []StrategyConfig{scopeCfg("l", true)}})
		if !moved || got != livePartition {
			t.Fatalf("conflict placement = %q,%v", got, moved)
		}
		if kept.PeakValue != 1000 {
			t.Errorf("the existing scoped row must be kept: %+v", kept)
		}
		if !kept.KillSwitchActive {
			t.Error("a latched legacy row must OR its latch into the kept row")
		}
		if len(kept.Events) != 1 || kept.Events[0].Type != "migration_conflict" {
			t.Errorf("a conflict must append a migration_conflict event; got %+v", kept.Events)
		}
	})
}

func TestLoadStateWithDB_PlacesLegacyRowAndPersists(t *testing.T) {
	db := openTestDB(t)
	state := NewAppState()
	state.PortfolioRisk[unassignedPartition] = &PortfolioRiskState{PeakValue: 3210, KillSwitchActive: true}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true)}}
	loaded, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("LoadStateWithDB: %v", err)
	}
	if loaded.partitionRisk(livePartition).PeakValue != 3210 || !loaded.partitionLatched(livePartition) {
		t.Fatalf("legacy row must land in live: %+v", loaded.partitionRisk(livePartition))
	}

	again, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("second LoadStateWithDB: %v", err)
	}
	if _, still := again.PortfolioRisk[unassignedPartition]; still {
		t.Error("the placement must be durable, so a second boot finds no unassigned row")
	}
	var scope string
	if err := db.db.QueryRow("SELECT scope FROM portfolio_risk").Scan(&scope); err != nil {
		t.Fatalf("read scope: %v", err)
	}
	if scope != string(ScopeLive) {
		t.Errorf("persisted scope = %q, want live", scope)
	}
}

func TestSaveLoadState_PerScope(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	state := NewAppState()
	live := state.partitionRisk(livePartition)
	live.PeakValue = 20000
	live.CurrentDrawdownPct = 12
	live.KillSwitchActive = true
	live.KillSwitchAt = now
	live.KillSwitchCloseApplied = true
	live.Events = []KillSwitchEvent{{Timestamp: now, Type: "triggered", Source: "equity", PeakValue: 20000}}

	paper := state.partitionRisk(defaultPaperPartition)
	paper.PeakValue = 5000
	paper.CurrentDrawdownPct = 3
	paper.Events = []KillSwitchEvent{{Timestamp: now, Type: "warning", Source: "equity", PeakValue: 5000}}

	state.setPartitionCorrelation(livePartition, &CorrelationSnapshot{PortfolioGrossUSD: 1000})
	state.setPartitionCorrelation(defaultPaperPartition, &CorrelationSnapshot{PortfolioGrossUSD: 2000})

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	gotLive := loaded.partitionRisk(livePartition)
	gotPaper := loaded.partitionRisk(defaultPaperPartition)
	if gotLive.PeakValue != 20000 || !gotLive.KillSwitchActive || !gotLive.KillSwitchCloseApplied {
		t.Errorf("live row did not round-trip: %+v", gotLive)
	}
	if gotPaper.PeakValue != 5000 || gotPaper.KillSwitchActive {
		t.Errorf("paper row did not round-trip: %+v", gotPaper)
	}
	if len(gotLive.Events) != 1 || gotLive.Events[0].Type != "triggered" {
		t.Errorf("live events = %+v, want one triggered event", gotLive.Events)
	}
	if len(gotPaper.Events) != 1 || gotPaper.Events[0].Type != "warning" {
		t.Errorf("paper events = %+v, want one warning event", gotPaper.Events)
	}
	if gotLive.Events[0].Partition != livePartition || gotPaper.Events[0].Partition != defaultPaperPartition {
		t.Error("events must round-trip with their scope")
	}
	if loaded.partitionCorrelation(livePartition).PortfolioGrossUSD != 1000 ||
		loaded.partitionCorrelation(defaultPaperPartition).PortfolioGrossUSD != 2000 {
		t.Error("correlation snapshots must round-trip per scope")
	}
}

func TestFormatCircuitBreakersResponse_PerScope(t *testing.T) {
	state := NewAppState()
	state.Strategies["live-a"] = scopeState("live-a", 1000)
	state.Strategies["paper-a"] = scopeState("paper-a", 1000)
	live := state.partitionRisk(livePartition)
	live.KillSwitchActive = true
	live.CurrentDrawdownPct = 41.5
	paper := state.partitionRisk(defaultPaperPartition)
	paper.CurrentDrawdownPct = 30
	paper.UntrustedOverLimitSince = time.Now().UTC()

	got := formatCircuitBreakersResponse(state, time.Now().UTC())
	if !strings.Contains(got, "ACTIVE [live]") {
		t.Errorf("the live latch must be labeled:\n%s", got)
	}
	if !strings.Contains(got, "DEFERRED [paper]") {
		t.Errorf("a deferral must name its scope:\n%s", got)
	}
	if strings.Contains(got, "ACTIVE [paper]") {
		t.Errorf("paper must not be reported as latched:\n%s", got)
	}
}

func statusRespForScopes(t *testing.T, cfgs []StrategyConfig, state *AppState) map[string]any {
	t.Helper()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", cfgs, nil)
	w := httptest.NewRecorder()
	ss.handleStatus(w, httptest.NewRequest("GET", "/status", nil))
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return resp
}

func TestHandleStatus_PerScopeFields(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	state := NewAppState()
	state.Strategies["live-a"] = scopeState("live-a", 1000)
	state.Strategies["paper-a"] = scopeState("paper-a", 2000)
	state.partitionRisk(livePartition).PeakValue = 11000
	state.partitionRisk(defaultPaperPartition).PeakValue = 22000

	resp := statusRespForScopes(t, cfgs, state)
	byScope, ok := resp["portfolio_risk_by_scope"].(map[string]any)
	if !ok || len(byScope) != 2 || byScope["live"] == nil || byScope["paper"] == nil {
		t.Fatalf("portfolio_risk_by_scope = %v, want live and paper keys", resp["portfolio_risk_by_scope"])
	}
	legacy, ok := resp["portfolio_risk"].(map[string]any)
	if !ok || legacy["peak_value"] != float64(11000) {
		t.Errorf("the legacy portfolio_risk field must mirror the live scope; got %v", resp["portfolio_risk"])
	}
	valueByScope, _ := resp["total_value_by_scope"].(map[string]any)
	if resp["total_value"] != valueByScope["live"] || resp["total_value"] == valueByScope["paper"] {
		t.Errorf("legacy total_value must mirror the live scope: total=%v by_scope=%v", resp["total_value"], valueByScope)
	}
	notionalByScope, _ := resp["total_notional_by_scope"].(map[string]any)
	if resp["total_notional"] != notionalByScope["live"] {
		t.Errorf("legacy total_notional must mirror the live scope: total=%v by_scope=%v", resp["total_notional"], notionalByScope)
	}

	paperOnly := NewAppState()
	paperOnly.Strategies["paper-a"] = scopeState("paper-a", 2000)
	paperOnly.partitionRisk(defaultPaperPartition).PeakValue = 22000
	resp = statusRespForScopes(t, []StrategyConfig{scopeCfg("paper-a", false)}, paperOnly)
	byScope, _ = resp["portfolio_risk_by_scope"].(map[string]any)
	if len(byScope) != 1 || byScope["paper"] == nil {
		t.Fatalf("a paper-only roster must expose exactly one scope key; got %v", byScope)
	}
	legacy, _ = resp["portfolio_risk"].(map[string]any)
	if legacy["peak_value"] != float64(22000) {
		t.Errorf("with no live scope the legacy field must mirror paper; got %v", legacy)
	}
	valueByScope, _ = resp["total_value_by_scope"].(map[string]any)
	if resp["total_value"] != valueByScope["paper"] {
		t.Errorf("with no live scope legacy total_value must mirror paper: total=%v by_scope=%v", resp["total_value"], valueByScope)
	}
}

func TestResolveChannelKey_PaperPrefersPaperKey(t *testing.T) {
	mn := NewMultiNotifier(notifierBackend{
		notifier: &mockNotifier{},
		channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"},
	})
	if key := mn.resolveChannelKey("hyperliquid", "perps", false, ""); key != "hyperliquid-paper" {
		t.Errorf("paper key = %q, want hyperliquid-paper", key)
	}
	if key := mn.resolveChannelKey("hyperliquid", "perps", true, ""); key != "hyperliquid" {
		t.Errorf("live key = %q, want hyperliquid", key)
	}
}

func TestResolveChannelKey_NoPaperKeyFallsBack(t *testing.T) {
	mn := NewMultiNotifier(notifierBackend{
		notifier: &mockNotifier{},
		channels: map[string]string{"hyperliquid": "one-ch", "spot": "spot-ch"},
	})
	if key := mn.resolveChannelKey("hyperliquid", "perps", false, ""); key != "hyperliquid" {
		t.Errorf("without a paper key the grouping must stay merged; got %q", key)
	}
	if key := mn.resolveChannelKey("binanceus", "spot", false, ""); key != "spot" {
		t.Errorf("type fallback = %q, want spot", key)
	}

	empty := NewMultiNotifier(notifierBackend{
		notifier: &mockNotifier{},
		channels: map[string]string{"hyperliquid": "one-ch", "hyperliquid-paper": ""},
	})
	if key := empty.resolveChannelKey("hyperliquid", "perps", false, ""); key != "hyperliquid" {
		t.Errorf("an empty paper channel id must not capture the grouping; got %q", key)
	}
}

func TestSendToPartitionChannels(t *testing.T) {
	paperHL := StrategyConfig{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=paper"}}
	liveOKX := StrategyConfig{ID: "okx-live", Type: "perps", Platform: "okx", Args: []string{"--mode=live"}}

	mockMerged := &mockNotifier{}
	mnMerged := NewMultiNotifier(notifierBackend{
		notifier: mockMerged,
		channels: map[string]string{"hyperliquid": "C", "okx": "C-okx-live"},
	})
	mnMerged.ReloadConfig(&Config{
		Discord:    DiscordConfig{Channels: map[string]string{"hyperliquid": "C", "okx": "C-okx-live"}},
		Strategies: []StrategyConfig{paperHL, liveOKX},
	})
	mnMerged.SendToPartitionChannels(defaultPaperPartition, "paper message")
	if len(mockMerged.messages) != 1 || mockMerged.messages[0].channelID != "C" {
		t.Fatalf("merged live+paper map paper broadcast = %+v, want only C", mockMerged.messages)
	}

	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"},
	})
	mn.ReloadConfig(&Config{
		Discord:    DiscordConfig{Channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"}},
		Strategies: []StrategyConfig{paperHL},
	})
	mn.SendToPartitionChannels(defaultPaperPartition, "paper message")
	if len(mock.messages) != 1 || mock.messages[0].channelID != "paper-ch" {
		t.Fatalf("paper broadcast = %+v, want only paper-ch", mock.messages)
	}

	mockUnused := &mockNotifier{}
	mnUnused := NewMultiNotifier(notifierBackend{
		notifier: mockUnused,
		channels: map[string]string{"hyperliquid-paper": "P", "okx-paper": "O"},
	})
	mnUnused.ReloadConfig(&Config{
		Discord:    DiscordConfig{Channels: map[string]string{"hyperliquid-paper": "P", "okx-paper": "O"}},
		Strategies: []StrategyConfig{paperHL},
	})
	mnUnused.SendToPartitionChannels(defaultPaperPartition, "paper message")
	if len(mockUnused.messages) != 1 || mockUnused.messages[0].channelID != "P" {
		t.Fatalf("unused -paper key must not receive the paper broadcast; got %+v", mockUnused.messages)
	}

	mock2 := &mockNotifier{}
	mn2 := NewMultiNotifier(notifierBackend{
		notifier: mock2,
		channels: map[string]string{"hyperliquid": "only-ch"},
	})
	mn2.SendToPartitionChannels(defaultPaperPartition, "paper message")
	if len(mock2.messages) != 1 || mock2.messages[0].channelID != "only-ch" {
		t.Fatalf("with no paper channel the paper broadcast must fall back to all channels; got %+v", mock2.messages)
	}

	mockEmpty := &mockNotifier{}
	mnEmpty := NewMultiNotifier(notifierBackend{
		notifier: mockEmpty,
		channels: map[string]string{"okx": "C-okx-live"},
	})
	mnEmpty.ReloadConfig(&Config{
		Discord:    DiscordConfig{Channels: map[string]string{"okx": "C-okx-live"}},
		Strategies: []StrategyConfig{paperHL},
	})
	mnEmpty.SendToPartitionChannels(defaultPaperPartition, "paper message")
	if len(mockEmpty.messages) != 1 || mockEmpty.messages[0].channelID != "C-okx-live" {
		t.Fatalf("unresolved paper strategies must fall through to every channel; got %+v", mockEmpty.messages)
	}

	mock3 := &mockNotifier{}
	mn3 := NewMultiNotifier(notifierBackend{
		notifier: mock3,
		channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"},
	})
	mn3.SendToPartitionChannels(livePartition, "live message")
	if len(mock3.messages) != 2 {
		t.Fatalf("a live broadcast must keep reaching every channel; got %+v", mock3.messages)
	}
}

func TestPaperScopeChannelValues(t *testing.T) {
	paperHL := StrategyConfig{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=paper"}}
	paperOKX := StrategyConfig{ID: "okx-paper", Type: "perps", Platform: "okx", Args: []string{"--mode=paper"}}
	liveOKX := StrategyConfig{ID: "okx-live", Type: "perps", Platform: "okx", Args: []string{"--mode=live"}}
	sourcedHL := StrategyConfig{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=paper"}, PaperSource: "btc"}
	cases := []struct {
		name     string
		channels map[string]string
		strats   []StrategyConfig
		part     RiskPartition
		want     []string
	}{
		{
			name:     "bare platform plus live-only",
			channels: map[string]string{"hyperliquid": "C", "okx": "C-okx-live"},
			strats:   []StrategyConfig{paperHL, liveOKX},
			want:     []string{"C"},
		},
		{
			name:     "prefers platform-paper",
			channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"},
			strats:   []StrategyConfig{paperHL},
			want:     []string{"paper-ch"},
		},
		{
			name:     "unused paper key omitted",
			channels: map[string]string{"hyperliquid-paper": "P", "okx-paper": "O"},
			strats:   []StrategyConfig{paperHL},
			want:     []string{"P"},
		},
		{
			name:     "no paper strategy",
			channels: map[string]string{"hyperliquid": "C", "okx": "C-okx-live"},
			strats:   []StrategyConfig{liveOKX},
		},
		{
			name:     "paper strategy with no matching channel",
			channels: map[string]string{"okx": "C-okx-live"},
			strats:   []StrategyConfig{paperHL},
		},
		{
			name:     "dedupes shared value",
			channels: map[string]string{"hyperliquid": "C", "okx": "C"},
			strats:   []StrategyConfig{paperHL, paperOKX},
			want:     []string{"C"},
		},
		{
			name:     "type key fallback",
			channels: map[string]string{"perps": "type-ch"},
			strats:   []StrategyConfig{paperHL},
			want:     []string{"type-ch"},
		},
		{
			name:     "source key wins over the bare paper key",
			channels: map[string]string{"hyperliquid-paper": "paper-ch", "hyperliquid-paper:btc": "btc-ch"},
			strats:   []StrategyConfig{sourcedHL},
			part:     paperSourcePartition("btc"),
			want:     []string{"btc-ch"},
		},
		{
			name:     "source falls back to the bare paper key",
			channels: map[string]string{"hyperliquid-paper": "paper-ch"},
			strats:   []StrategyConfig{sourcedHL},
			part:     paperSourcePartition("btc"),
			want:     []string{"paper-ch"},
		},
		{
			name:     "one source never reaches another source's channel",
			channels: map[string]string{"hyperliquid-paper:eth": "eth-ch"},
			strats:   []StrategyConfig{sourcedHL},
			part:     paperSourcePartition("btc"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			part := tc.part
			if part == (RiskPartition{}) {
				part = defaultPaperPartition
			}
			got := partitionChannelValues(tc.channels, tc.strats)[part]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func paperOverrideReloadConfig(paper *PortfolioRiskConfig) *Config {
	return &Config{
		IntervalSeconds: 60,
		DBFile:          "state.db",
		LogDir:          "logs",
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct: 25, WarnThresholdPct: 60, DailyMaxLossUSD: 500,
			Paper: paper,
		},
		Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)},
	}
}

func TestHotReload_PortfolioRiskPaperOverride(t *testing.T) {
	cfg := paperOverrideReloadConfig(nil)
	next := paperOverrideReloadConfig(&PortfolioRiskConfig{MaxDrawdownPct: 60, DailyMaxLossUSD: 2000})
	state := NewAppState()

	changes, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err != nil {
		t.Fatalf("adding a paper override must hot-reload: %v", err)
	}
	joined := strings.Join(changes, "\n")
	for _, want := range []string{"portfolio_risk.paper.max_drawdown_pct", "portfolio_risk.paper.daily_max_loss_usd"} {
		if !strings.Contains(joined, want) {
			t.Errorf("change list missing %q:\n%s", want, joined)
		}
	}
	if got := partitionRiskConfig(cfg, defaultPaperPartition); got.MaxDrawdownPct != 60 || got.DailyMaxLossUSD != 2000 {
		t.Errorf("the override must be live after reload: %+v", got)
	}
	if got := partitionRiskConfig(cfg, livePartition); got.MaxDrawdownPct != 25 || got.DailyMaxLossUSD != 500 {
		t.Errorf("the live scope must keep the parent values: %+v", got)
	}
}

func TestHotReload_PaperMaxNotionalRestartRequired(t *testing.T) {
	cfg := paperOverrideReloadConfig(&PortfolioRiskConfig{MaxNotionalUSD: 10000})
	next := paperOverrideReloadConfig(&PortfolioRiskConfig{MaxNotionalUSD: 20000})
	if err := validateHotReloadCompatible(cfg, next); err == nil ||
		!strings.Contains(err.Error(), "portfolio_risk.paper.max_notional_usd") {
		t.Fatalf("paper max_notional_usd must be restart-required; got %v", err)
	}
}

func TestHotReload_WhileLatched_KeepsLatch(t *testing.T) {
	cfg := paperOverrideReloadConfig(nil)
	next := paperOverrideReloadConfig(&PortfolioRiskConfig{MaxDrawdownPct: 60})
	next.PortfolioRisk.MaxDrawdownPct = 40

	state := NewAppState()
	latchedAt := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for _, scope := range []RiskPartition{livePartition, defaultPaperPartition} {
		prs := state.partitionRisk(scope)
		prs.KillSwitchActive = true
		prs.KillSwitchAt = latchedAt
		prs.PeakValue = 10000
		prs.CurrentDrawdownPct = 50
	}

	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("a limit change while latched must hot-reload: %v", err)
	}
	for _, scope := range []RiskPartition{livePartition, defaultPaperPartition} {
		prs := state.partitionRisk(scope)
		if !prs.KillSwitchActive || !prs.KillSwitchAt.Equal(latchedAt) || prs.PeakValue != 10000 || prs.CurrentDrawdownPct != 50 {
			t.Errorf("%s latch state must survive a reload untouched: %+v", partitionLabel(scope), prs)
		}
	}
}

func TestConfigExample_PaperOverrideLoads(t *testing.T) {
	cfg, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatalf("LoadConfig(config.example.json): %v", err)
	}
	if cfg.PortfolioRisk == nil || cfg.PortfolioRisk.Paper == nil {
		t.Fatal("config.example.json must document the portfolio_risk.paper override")
	}
	paper := partitionRiskConfig(cfg, defaultPaperPartition)
	if paper.MaxDrawdownPct != 50 {
		t.Errorf("paper max_drawdown_pct = %v, want the override value 50", paper.MaxDrawdownPct)
	}
	if paper.WarnThresholdPct != cfg.PortfolioRisk.WarnThresholdPct {
		t.Errorf("warn_threshold_pct must inherit; paper=%v parent=%v", paper.WarnThresholdPct, cfg.PortfolioRisk.WarnThresholdPct)
	}
	if partitionRiskConfig(cfg, livePartition).MaxDrawdownPct != cfg.PortfolioRisk.MaxDrawdownPct {
		t.Error("the live scope must keep the parent drawdown limit")
	}
}

func TestAssignLegacyPortfolioScope_MixedRosterRebasesLivePeak(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("live-b", true), scopeCfg("paper-a", false)}}
	state := NewAppState()
	state.Strategies["live-a"] = scopeState("live-a", 8000)
	state.Strategies["live-a"].RiskState.PeakValue = 8000
	state.Strategies["live-b"] = scopeState("live-b", 4000)
	state.Strategies["live-b"].RiskState.PeakValue = 4000
	state.Strategies["paper-a"] = scopeState("paper-a", 9000)
	state.Strategies["paper-a"].RiskState.PeakValue = 9000
	latchedAt := time.Now().UTC().Add(-time.Hour)
	state.PortfolioRisk[unassignedPartition] = &PortfolioRiskState{PeakValue: 21000, CurrentDrawdownPct: 5, KillSwitchActive: true, KillSwitchAt: latchedAt}

	target, moved := assignLegacyPortfolioScope(state, cfg)
	if !moved || target != livePartition {
		t.Fatalf("placement = %q,%v; want live,true", target, moved)
	}
	prs := state.partitionRisk(livePartition)
	if prs.PeakValue != 12000 {
		t.Fatalf("live peak = %v, want the live-only basis 12000 (sum of live per-strategy peaks, with no configured-capital floor)", prs.PeakValue)
	}
	if !prs.KillSwitchActive || !prs.KillSwitchAt.Equal(latchedAt) {
		t.Error("the legacy latch must survive the re-base untouched")
	}
	if n := len(prs.Events); n == 0 || prs.Events[n-1].Type != "scope_basis_rebaseline" || prs.Events[n-1].Partition != livePartition {
		t.Errorf("the re-base must be recorded as a live-scope event; got %+v", prs.Events)
	}
	if state.partitionRiskIfPresent(defaultPaperPartition) != nil {
		t.Error("placement must not create the paper scope; its peak seeds from the paper book on the first cycle")
	}

	cfgWithRisk := &Config{PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60}, Strategies: cfg.Strategies}
	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	res := runScopeCycle(t, cfgWithRisk, state, nil)
	if res[livePartition].KillSwitchFired {
		t.Errorf("live at its own peaks must not latch after the upgrade; reason=%q", res[livePartition].Reason)
	}
	state.Strategies["live-a"].Cash = 2000
	res = runScopeCycle(t, cfgWithRisk, state, nil)
	if !res[livePartition].KillSwitchFired {
		t.Errorf("a genuine live drawdown past the limit must still latch on the re-based peak; reason=%q", res[livePartition].Reason)
	}
}
