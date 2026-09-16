package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func partitionTestStrategies() []StrategyConfig {
	return []StrategyConfig{
		{ID: "live-1", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "BTC", "1h", "--mode=live"}, InitialCapital: 100},
		{ID: "paper-1", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, InitialCapital: 100},
		{ID: "btc-1", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "SOL", "1h", "--mode=paper"}, PaperSource: "btc", InitialCapital: 100},
	}
}

func partitionTestState() *AppState {
	state := NewAppState()
	for _, id := range []string{"live-1", "paper-1", "btc-1"} {
		state.Strategies[id] = &StrategyState{ID: id, Type: "perps", Cash: 120, InitialCapital: 100,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	}
	return state
}

func TestUIPartitionFilterSelectsOnePartitionPerEndpoint(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)

	cases := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{"all partitions", "", []string{"btc-1", "live-1", "paper-1"}},
		{"live only", "?partition=live", []string{"live-1"}},
		{"default paper only", "?partition=paper", []string{"paper-1"}},
		{"folded source only", "?partition=paper:btc", []string{"btc-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := opsGet(ss, ss.handleAPIStrategies, "/api/strategies"+tc.query)
			if w.Code != http.StatusOK {
				t.Fatalf("strategies code = %d, want 200", w.Code)
			}
			var list struct {
				Strategies []UIStrategy `json:"strategies"`
			}
			if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
				t.Fatalf("decode strategies: %v", err)
			}
			got := make([]string, 0, len(list.Strategies))
			for _, s := range list.Strategies {
				got = append(got, s.ID)
			}
			assertIDSet(t, "strategies", got, tc.wantIDs)

			w = opsGet(ss, ss.handleAPILeaderboard, "/api/leaderboard"+tc.query)
			if w.Code != http.StatusOK {
				t.Fatalf("leaderboard code = %d, want 200", w.Code)
			}
			var board struct {
				Entries []LeaderboardEntry `json:"entries"`
			}
			if err := json.NewDecoder(w.Body).Decode(&board); err != nil {
				t.Fatalf("decode leaderboard: %v", err)
			}
			got = got[:0]
			for _, e := range board.Entries {
				got = append(got, e.ID)
			}
			assertIDSet(t, "leaderboard", got, tc.wantIDs)

			w = opsGet(ss, ss.handleAPIDeadStrategies, "/api/strategies/dead"+tc.query)
			if w.Code != http.StatusOK {
				t.Fatalf("dead code = %d, want 200", w.Code)
			}
			var dead struct {
				Dead  []string `json:"dead"`
				Total int      `json:"total"`
			}
			if err := json.NewDecoder(w.Body).Decode(&dead); err != nil {
				t.Fatalf("decode dead: %v", err)
			}
			assertIDSet(t, "dead", dead.Dead, tc.wantIDs)
			if dead.Total != len(tc.wantIDs) {
				t.Errorf("dead total = %d, want %d", dead.Total, len(tc.wantIDs))
			}
		})
	}
}

func assertIDSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s ids = %v, want %v", label, got, want)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("%s ids = %v, want %v", label, got, want)
		}
	}
}

func TestUIPartitionRefusesUnparseableAndUnownedKeys(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"unparseable text", "?partition=nonsense", http.StatusBadRequest},
		{"live with a source", "?partition=live:btc", http.StatusBadRequest},
		{"paper with an empty source", "?partition=paper:", http.StatusBadRequest},
		{"owned by no strategy", "?partition=paper:eth", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := opsGet(ss, ss.handleAPILeaderboard, "/api/leaderboard"+tc.query); w.Code != tc.want {
				t.Errorf("leaderboard%s = %d, want %d", tc.query, w.Code, tc.want)
			}
			if w := opsGet(ss, ss.handleAPIStrategies, "/api/strategies"+tc.query); w.Code != tc.want {
				t.Errorf("strategies%s = %d, want %d", tc.query, w.Code, tc.want)
			}
		})
	}
}

func TestUIPartitionDiagnosticsCountsOnlyTheSelectedPartition(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	rows := []struct {
		id  string
		day int
	}{
		{"live-1", 1}, {"live-1", 2}, {"paper-1", 3}, {"btc-1", 4}, {"btc-1", 5}, {"btc-1", 6},
	}
	for _, r := range rows {
		row := &TradeDiagnosticsRow{
			StrategyID: r.id, PositionID: "p", Symbol: "BTC", Side: "long",
			EntryPrice: 100, ExitPrice: 100, Quantity: 1,
			OpenedAt:      time.Date(2026, 1, r.day, 0, 0, 0, 0, time.UTC),
			ClosedAt:      time.Date(2026, 1, r.day+1, 0, 0, 0, 0, time.UTC),
			MetricsStatus: diagMetricsPending,
		}
		if err := ss.stateDB.InsertTradeDiagnostics(row); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// The total must count the partition's rows, not every row in the file,
	// or the operator pages past the end of what the filter can show.
	w := opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics?partition=paper:btc&limit=2&offset=0")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var page struct {
		Rows []struct {
			StrategyID string `json:"strategy_id"`
		} `json:"rows"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("total = %d, want 3 (only paper:btc rows)", page.Total)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(page.Rows))
	}
	for _, r := range page.Rows {
		if r.StrategyID != "btc-1" {
			t.Errorf("row strategy = %q, want btc-1", r.StrategyID)
		}
	}

	// A strategy the selected partition does not own reports no rows rather
	// than a neighbour partition's history under the selected label.
	w = opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics?partition=paper:btc&strategy=live-1")
	if w.Code != http.StatusOK {
		t.Fatalf("cross-partition strategy code = %d, want 200", w.Code)
	}
	page.Total = -1
	page.Rows = nil
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode cross-partition: %v", err)
	}
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Errorf("cross-partition page total=%d rows=%d, want 0/0", page.Total, len(page.Rows))
	}
}

func TestUIPartitionCashflowStaysLiveOwned(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	var resp struct {
		Wallets   []CashflowJournalWalletStatus `json:"wallets"`
		LiveOwned bool                          `json:"live_owned"`
		Available *bool                         `json:"available"`
	}

	w := opsGet(ss, ss.handleAPICashflow, "/api/cashflow?partition=paper:btc")
	if w.Code != http.StatusOK {
		t.Fatalf("paper cashflow code = %d, want 200", w.Code)
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode paper: %v", err)
	}
	if resp.Available == nil || *resp.Available {
		t.Error("a paper partition must report cash flow as unavailable")
	}
	if !resp.LiveOwned {
		t.Error("cash flow must stay marked live-owned")
	}
	if len(resp.Wallets) != 0 {
		t.Errorf("paper wallets = %d, want 0", len(resp.Wallets))
	}

	w = opsGet(ss, ss.handleAPICashflow, "/api/cashflow?partition=live")
	if w.Code != http.StatusOK {
		t.Fatalf("live cashflow code = %d, want 200", w.Code)
	}
	resp.Available = nil
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode live: %v", err)
	}
	if resp.Available == nil || !*resp.Available {
		t.Error("the live partition must still read the journal")
	}
}

func TestUIStrategyCarriesItsPartition(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	w := opsGet(ss, ss.handleAPIStrategies, "/api/strategies")
	var list struct {
		Strategies []UIStrategy `json:"strategies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"live-1": "live", "paper-1": "paper", "btc-1": "paper:btc"}
	wantSource := map[string]string{"live-1": "", "paper-1": "", "btc-1": "btc"}
	for _, s := range list.Strategies {
		if s.Partition != want[s.ID] {
			t.Errorf("%s partition = %q, want %q", s.ID, s.Partition, want[s.ID])
		}
		if s.PaperSource != wantSource[s.ID] {
			t.Errorf("%s paper_source = %q, want %q", s.ID, s.PaperSource, wantSource[s.ID])
		}
	}
}

func TestStatusListsEveryOwnedPartition(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	ss.UpdatePaperSources([]PaperSourceConfig{{ID: "btc", Label: "Paper BTC"}})

	w := opsGet(ss, ss.handleStatus, "/status")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Partitions []struct {
			Partition string `json:"partition"`
			Label     string `json:"label"`
			Scope     string `json:"scope"`
			Source    string `json:"source"`
		} `json:"partitions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantOrder := []string{"live", "paper", "paper:btc"}
	if len(resp.Partitions) != len(wantOrder) {
		t.Fatalf("partitions = %d, want %d", len(resp.Partitions), len(wantOrder))
	}
	for i, want := range wantOrder {
		if resp.Partitions[i].Partition != want {
			t.Errorf("partition[%d] = %q, want %q", i, resp.Partitions[i].Partition, want)
		}
	}
	if resp.Partitions[2].Label != "Paper BTC" {
		t.Errorf("source label = %q, want %q", resp.Partitions[2].Label, "Paper BTC")
	}
	if resp.Partitions[2].Source != "btc" || resp.Partitions[2].Scope != "paper" {
		t.Errorf("source partition = %+v, want scope paper source btc", resp.Partitions[2])
	}
}

func TestStatusPartitionLabelFallsBackToPartitionText(t *testing.T) {
	ss := newOpsTestServer(t, partitionTestStrategies(), partitionTestState(), true)
	w := opsGet(ss, ss.handleStatus, "/status")
	var resp struct {
		Partitions []struct {
			Partition string `json:"partition"`
			Label     string `json:"label"`
		} `json:"partitions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// An unlabelled source must not read as the bare id, which the selector
	// could not tell from the default paper partition.
	if resp.Partitions[2].Label != "paper:btc" {
		t.Errorf("unlabelled source label = %q, want %q", resp.Partitions[2].Label, "paper:btc")
	}
}

func TestStatusNamesNoPartitionForAnUnconfiguredStateRow(t *testing.T) {
	state := partitionTestState()
	state.Strategies["orphan-1"] = &StrategyState{ID: "orphan-1", Type: "perps", Cash: 120, InitialCapital: 100,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	ss := newOpsTestServer(t, partitionTestStrategies(), state, true)

	w := opsGet(ss, ss.handleStatus, "/status")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Strategies map[string]struct {
			Partition string `json:"partition"`
		} `json:"strategies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"live-1": "live", "paper-1": "paper", "btc-1": "paper:btc", "orphan-1": ""}
	for id, wantPartition := range want {
		got, ok := resp.Strategies[id]
		if !ok {
			t.Fatalf("status omits strategy %q", id)
		}
		if got.Partition != wantPartition {
			t.Errorf("%s partition = %q, want %q", id, got.Partition, wantPartition)
		}
	}
}
