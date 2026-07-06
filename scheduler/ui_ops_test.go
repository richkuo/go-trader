package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newOpsTestServer wires a StatusServer with real state and an optional
// SQLite-backed StateDB for the #1231 ops-endpoint tests.
func newOpsTestServer(t *testing.T, strategies []StrategyConfig, state *AppState, withDB bool) *StatusServer {
	t.Helper()
	var sdb *StateDB
	if withDB {
		sdb = openTestDB(t)
	}
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", strategies, sdb)
	ss.SetConfigContext("", &Config{IntervalSeconds: 3600})
	return ss
}

func opsGet(ss *StatusServer, handler func(http.ResponseWriter, *http.Request), path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", path, nil))
	return w
}

// All six ops endpoints must 503 while draining — before touching state or DB.
func TestOpsEndpointsRejectWhileDraining(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), false)
	shutdownDraining.Store(true)
	defer shutdownDraining.Store(false)
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"/api/leaderboard":        ss.handleAPILeaderboard,
		"/api/diagnostics":        ss.handleAPIDiagnostics,
		"/api/cashflow":           ss.handleAPICashflow,
		"/api/strategies/dead":    ss.handleAPIDeadStrategies,
		"/api/closing-strategies": ss.handleAPIClosingStrategies,
		"/api/correlation":        ss.handleAPICorrelation,
	}
	for path, h := range handlers {
		if w := opsGet(ss, h, path); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s while draining = %d, want 503", path, w.Code)
		}
	}
}

func TestOpsEndpointsRejectNonGet(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), false)
	w := httptest.NewRecorder()
	ss.handleAPILeaderboard(w, httptest.NewRequest("POST", "/api/leaderboard", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/leaderboard = %d, want 405", w.Code)
	}
}

func TestAPILeaderboardRanksByPnLPct(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "winner", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}, InitialCapital: 100},
		{ID: "loser", Type: "spot", Args: []string{"sma", "ETH/USDT", "1h"}, InitialCapital: 100},
	}
	state := NewAppState()
	state.Strategies["winner"] = &StrategyState{ID: "winner", Type: "spot", Cash: 150,
		InitialCapital: 100, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	state.Strategies["loser"] = &StrategyState{ID: "loser", Type: "spot", Cash: 80,
		InitialCapital: 100, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	ss := newOpsTestServer(t, strategies, state, false)

	w := opsGet(ss, ss.handleAPILeaderboard, "/api/leaderboard")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Entries []LeaderboardEntry `json:"entries"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].ID != "winner" || resp.Entries[1].ID != "loser" {
		t.Errorf("rank order = [%s, %s], want [winner, loser]", resp.Entries[0].ID, resp.Entries[1].ID)
	}
	if resp.Entries[0].PnLPct != 50 {
		t.Errorf("winner pnl_pct = %v, want 50", resp.Entries[0].PnLPct)
	}
}

func TestAPIDiagnosticsNilDB503(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), false)
	if w := opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil-DB diagnostics = %d, want 503", w.Code)
	}
}

func TestAPIDiagnosticsEmptyDB(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)
	w := opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Rows  []json.RawMessage `json:"rows"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || len(resp.Rows) != 0 {
		t.Errorf("empty DB: total=%d rows=%d, want 0/0", resp.Total, len(resp.Rows))
	}
}

// Net PnL must come from the trades join (tradeNetPnLSQL: gross-convention
// rows net the fee), never from the diagnostics row's own pre-fee
// RealizedPnL. Pending rows keep null metric columns.
func TestAPIDiagnosticsNetPnLAndPendingMetrics(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)
	sdb := ss.stateDB

	row := &TradeDiagnosticsRow{
		StrategyID: "hl-btc", PositionID: "pos-1", Symbol: "BTC", Side: "long",
		EntryPrice: 100, ExitPrice: 110, Quantity: 1,
		RealizedPnL:   10, // PRE-FEE
		OpenedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ClosedAt:      time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		MetricsStatus: diagMetricsPending,
	}
	if err := sdb.InsertTradeDiagnostics(row); err != nil {
		t.Fatalf("insert diagnostics: %v", err)
	}
	// Gross-convention close leg: realized_pnl=10 gross, fee=1.5 → net 8.5.
	if err := sdb.InsertTrade("hl-btc", Trade{
		Timestamp: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), StrategyID: "hl-btc",
		Symbol: "BTC", Side: "sell", Quantity: 1, Price: 110, Value: 110,
		TradeType: "perps", PositionID: "pos-1",
		IsClose: true, RealizedPnL: 10, ExchangeFee: 1.5, PnLGross: true,
	}); err != nil {
		t.Fatalf("insert trade: %v", err)
	}

	w := opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics?strategy=hl-btc")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Rows []struct {
			StrategyID    string   `json:"strategy_id"`
			NetPnL        float64  `json:"net_pnl"`
			CaptureRatio  *float64 `json:"capture_ratio"`
			MetricsStatus string   `json:"metrics_status"`
		} `json:"rows"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 1/1", resp.Total, len(resp.Rows))
	}
	got := resp.Rows[0]
	if got.NetPnL != 8.5 {
		t.Errorf("net_pnl = %v, want 8.5 (gross 10 − fee 1.5 via tradeNetPnLSQL)", got.NetPnL)
	}
	if got.MetricsStatus != diagMetricsPending {
		t.Errorf("metrics_status = %q, want %q", got.MetricsStatus, diagMetricsPending)
	}
	if got.CaptureRatio != nil {
		t.Errorf("capture_ratio = %v, want null while pending", *got.CaptureRatio)
	}
}

func TestAPIDiagnosticsPagingNewestFirst(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)
	for i := 0; i < 3; i++ {
		row := &TradeDiagnosticsRow{
			StrategyID: "s", PositionID: "p", Symbol: "BTC", Side: "long",
			EntryPrice: 100, ExitPrice: 100, Quantity: 1,
			OpenedAt:      time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC),
			ClosedAt:      time.Date(2026, 1, 2+i, 0, 0, 0, 0, time.UTC),
			MetricsStatus: diagMetricsPending,
		}
		if err := ss.stateDB.InsertTradeDiagnostics(row); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	w := opsGet(ss, ss.handleAPIDiagnostics, "/api/diagnostics?limit=1&offset=1")
	var resp struct {
		Rows []struct {
			ClosedAt time.Time `json:"closed_at"`
		} `json:"rows"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 || len(resp.Rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 3/1", resp.Total, len(resp.Rows))
	}
	// Newest-first: offset 1 skips the Jan 4 close and returns Jan 3.
	if want := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC); !resp.Rows[0].ClosedAt.Equal(want) {
		t.Errorf("closed_at = %v, want %v", resp.Rows[0].ClosedAt, want)
	}
}

func TestAPICashflowEmptyAndPopulated(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)

	// Empty DB: no wallets, alarm on by default.
	w := opsGet(ss, ss.handleAPICashflow, "/api/cashflow")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Wallets      []CashflowJournalWalletStatus `json:"wallets"`
		Drift        []SharedWalletDriftView       `json:"drift"`
		AlarmEnabled bool                          `json:"alarm_enabled"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Wallets) != 0 {
		t.Errorf("empty DB wallets = %d, want 0", len(resp.Wallets))
	}
	if !resp.AlarmEnabled {
		t.Error("alarm_enabled should default to true")
	}

	// Populate: an HL live-basis wallet and a shadow-only OKX wallet. The
	// basis registry is a package-level singleton other tests may have
	// written to — clear this wallet's slot so "unknown" is deterministic.
	cashflowJournalBases.mu.Lock()
	delete(cashflowJournalBases.bases, "hyperliquid/0xabc")
	cashflowJournalBases.mu.Unlock()
	sdb := ss.stateDB
	if err := sdb.UpsertCashflowJournalState("hyperliquid", "0xabc", CashflowJournalState{BaselineSet: true, BaselineAccountValue: 1000}); err != nil {
		t.Fatalf("upsert hl: %v", err)
	}
	if err := sdb.UpsertCashflowJournalState("okx", "acct1", CashflowJournalState{BaselineSet: true, Incomplete: true}); err != nil {
		t.Fatalf("upsert okx: %v", err)
	}
	if err := sdb.InsertCashflowJournalEntry("hyperliquid", "0xabc", 1700000000000, "fill", 12.5, "BTC", 13, 0.5, "d1"); err != nil {
		t.Fatalf("insert entry: %v", err)
	}
	if err := sdb.InsertCashflowJournalEntry("hyperliquid", "0xabc", 1700000001000, "funding", -2.5, "BTC", 0, 0, "d2"); err != nil {
		t.Fatalf("insert entry: %v", err)
	}

	w = opsGet(ss, ss.handleAPICashflow, "/api/cashflow")
	resp.Wallets = nil
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Wallets) != 2 {
		t.Fatalf("wallets = %d, want 2", len(resp.Wallets))
	}
	hl, okx := resp.Wallets[0], resp.Wallets[1]
	if hl.Platform != "hyperliquid" || okx.Platform != "okx" {
		t.Fatalf("wallet order = [%s, %s], want [hyperliquid, okx]", hl.Platform, okx.Platform)
	}
	if hl.ShadowOnly || !hl.LiveBasisEligible {
		t.Errorf("HL wallet shadow_only=%v live_basis_eligible=%v, want false/true", hl.ShadowOnly, hl.LiveBasisEligible)
	}
	if hl.Basis != cashflowBasisUnknown {
		t.Errorf("HL wallet basis = %q, want %q before any reconcile cycle (eligibility must not overclaim)", hl.Basis, cashflowBasisUnknown)
	}
	if hl.SettledSum != 10 || hl.EntryCount != 2 || hl.LastEventMs != 1700000001000 {
		t.Errorf("HL aggregates = sum %v count %d last %d, want 10/2/1700000001000", hl.SettledSum, hl.EntryCount, hl.LastEventMs)
	}
	if !okx.ShadowOnly || okx.LiveBasisEligible {
		t.Errorf("OKX wallet shadow_only=%v live_basis_eligible=%v, want true/false (#1100 shadow phases)", okx.ShadowOnly, okx.LiveBasisEligible)
	}
	if okx.Basis != "" {
		t.Errorf("OKX wallet basis = %q, want empty (shadow-only wallets carry no runtime basis)", okx.Basis)
	}
}

// The runtime basis recorded by applyCashflowJournalDriftBasis must overlay
// structural eligibility: a transient fetch miss (Usable=false, within the
// suppression streak) reads "pending", never "journal", even though the
// persisted state stays live-basis-eligible.
func TestAPICashflowRuntimeBasisOverridesEligibility(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xbasis"}
	label := sharedWalletKeyLabel(key)
	if err := ss.stateDB.UpsertCashflowJournalState(key.Platform, key.Account, CashflowJournalState{BaselineSet: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	defer cashflowJournalPendingStreaks.reset(label)
	defer func() {
		cashflowJournalBases.mu.Lock()
		delete(cashflowJournalBases.bases, label)
		cashflowJournalBases.mu.Unlock()
	}()

	// Transient miss: journal governs but produced no usable reading this cycle.
	applyCashflowJournalDriftBasis(nil, key, &cashflowJournalReconcile{Key: key, Usable: false, Incomplete: false}, true)

	w := opsGet(ss, ss.handleAPICashflow, "/api/cashflow")
	var resp struct {
		Wallets []CashflowJournalWalletStatus `json:"wallets"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Wallets) != 1 {
		t.Fatalf("wallets = %d, want 1", len(resp.Wallets))
	}
	got := resp.Wallets[0]
	if !got.LiveBasisEligible {
		t.Errorf("live_basis_eligible = false, want true (structurally eligible)")
	}
	if got.Basis != cashflowBasisPending {
		t.Errorf("basis = %q, want %q (transient miss must not read as the live basis)", got.Basis, cashflowBasisPending)
	}

	// Recovery: a usable reconcile flips the recorded basis to journal.
	applyCashflowJournalDriftBasis(nil, key, &cashflowJournalReconcile{Key: key, Usable: true}, true)
	w = opsGet(ss, ss.handleAPICashflow, "/api/cashflow")
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Wallets[0].Basis != cashflowBasisJournal {
		t.Errorf("basis after usable cycle = %q, want %q", resp.Wallets[0].Basis, cashflowBasisJournal)
	}
}

func TestAPICashflowNilDB503(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), false)
	if w := opsGet(ss, ss.handleAPICashflow, "/api/cashflow"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil-DB cashflow = %d, want 503 (a failed money-path read must not render as a clean empty journal)", w.Code)
	}
}

func TestAPICashflowDriftSnapshot(t *testing.T) {
	ss := newOpsTestServer(t, nil, NewAppState(), true)
	key := "test-drift-wallet-1231"
	now := time.Now()
	sharedWalletDriftTracker.Record(key, 1.23, []string{"BTC"}, now)
	sharedWalletDriftTracker.Record(key, 1.23, []string{"BTC"}, now.Add(time.Minute))
	defer sharedWalletDriftTracker.Clear(key)

	w := opsGet(ss, ss.handleAPICashflow, "/api/cashflow")
	var resp struct {
		Drift []SharedWalletDriftView `json:"drift"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *SharedWalletDriftView
	for i := range resp.Drift {
		if resp.Drift[i].Wallet == key {
			found = &resp.Drift[i]
		}
	}
	if found == nil {
		t.Fatalf("drift snapshot missing wallet %q: %+v", key, resp.Drift)
	}
	if found.Cycles != 2 || !found.Alerted {
		t.Errorf("drift view cycles=%d alerted=%v, want 2/true", found.Cycles, found.Alerted)
	}
	if len(found.OrphanCoins) != 1 || found.OrphanCoins[0] != "BTC" {
		t.Errorf("orphan_coins = %v, want [BTC]", found.OrphanCoins)
	}
}

func TestAPIDeadStrategies(t *testing.T) {
	state := NewAppState()
	state.Strategies["alive"] = &StrategyState{ID: "alive", Type: "spot", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	state.Strategies["dead"] = &StrategyState{ID: "dead", Type: "spot", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	ss := newOpsTestServer(t, nil, state, true)
	if err := ss.stateDB.InsertTrade("alive", Trade{
		Timestamp: time.Now(), StrategyID: "alive", Symbol: "BTC/USDT", Side: "buy",
		Quantity: 1, Price: 100, Value: 100, TradeType: "spot", PositionID: "p1",
	}); err != nil {
		t.Fatalf("insert trade: %v", err)
	}

	w := opsGet(ss, ss.handleAPIDeadStrategies, "/api/strategies/dead")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Dead  []string `json:"dead"`
		Total int      `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if len(resp.Dead) != 1 || resp.Dead[0] != "dead" {
		t.Errorf("dead = %v, want [dead]", resp.Dead)
	}
}

// The close-registry catalog is normally fetched from a Python subprocess;
// seed the package cache so the handler is testable hermetically.
func TestAPIClosingStrategiesFromCacheWithOverrides(t *testing.T) {
	closeRegistryCatalogMu.Lock()
	prev := closeRegistryCatalog
	closeRegistryCatalog = []closeRegistryEntry{{
		Name:          "tiered_tp_atr_live",
		Description:   "test evaluator",
		DefaultParams: map[string]interface{}{"atr_source": "live"},
		Platforms:     []string{"hyperliquid"},
	}}
	closeRegistryCatalogMu.Unlock()
	defer func() {
		closeRegistryCatalogMu.Lock()
		closeRegistryCatalog = prev
		closeRegistryCatalogMu.Unlock()
	}()

	ss := newOpsTestServer(t, nil, NewAppState(), false)
	ss.strategiesMu.Lock()
	ss.userCloseDefaults = CloseDefaultsMap{
		"tiered_tp_atr_live": {"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "pct": 50.0}}},
	}
	ss.strategiesMu.Unlock()

	w := opsGet(ss, ss.handleAPIClosingStrategies, "/api/closing-strategies")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Evaluators []struct {
			Name          string                 `json:"name"`
			Platforms     []string               `json:"platforms"`
			DefaultParams map[string]interface{} `json:"default_params"`
			UserOverrides map[string]interface{} `json:"user_overrides"`
		} `json:"evaluators"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Evaluators) != 1 {
		t.Fatalf("evaluators = %d, want 1", len(resp.Evaluators))
	}
	ev := resp.Evaluators[0]
	if ev.Name != "tiered_tp_atr_live" || ev.DefaultParams["atr_source"] != "live" {
		t.Errorf("unexpected evaluator: %+v", ev)
	}
	if ev.UserOverrides == nil || ev.UserOverrides["tp_tiers"] == nil {
		t.Errorf("user_overrides missing tp_tiers: %+v", ev.UserOverrides)
	}
}

func TestAPICorrelation(t *testing.T) {
	state := NewAppState()
	ss := newOpsTestServer(t, nil, state, false)

	// No snapshot yet → null.
	w := opsGet(ss, ss.handleAPICorrelation, "/api/correlation")
	var resp struct {
		Correlation *CorrelationSnapshot `json:"correlation"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Correlation != nil {
		t.Errorf("expected null correlation before first cycle, got %+v", resp.Correlation)
	}

	state.CorrelationSnapshot = &CorrelationSnapshot{
		Timestamp:         time.Now(),
		PortfolioGrossUSD: 1234,
		Warnings:          []string{"BTC concentration 90%"},
		Assets: map[string]*AssetExposure{
			"BTC": {Asset: "BTC", NetDeltaUSD: 1000, GrossDeltaUSD: 1100, ConcentrationPct: 90},
		},
	}
	w = opsGet(ss, ss.handleAPICorrelation, "/api/correlation")
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Correlation == nil || resp.Correlation.PortfolioGrossUSD != 1234 {
		t.Fatalf("correlation snapshot not round-tripped: %+v", resp.Correlation)
	}
	if len(resp.Correlation.Warnings) != 1 {
		t.Errorf("warnings = %v, want 1 warning", resp.Correlation.Warnings)
	}
}
