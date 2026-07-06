package main

import (
	"net/http"
	"strconv"
	"time"
)

// ui_ops.go — #1231 read-only operator API endpoints (Phase 2 of the #1229
// dashboard-parity plan). Six GET routes give the dashboard read parity with
// every Discord read-only command and the diagnostics CLI:
//
//	/api/leaderboard        — per-strategy PnL ranking (Discord `leaderboard`)
//	/api/diagnostics        — #1147 trade_diagnostics rows, paged (CLI `diagnostics`)
//	/api/cashflow           — #1100 journal wallet status + wallet-drift tracker
//	/api/strategies/dead    — Discord `dead-strategies`
//	/api/closing-strategies — #1203 close-evaluator registry dump
//	/api/correlation        — Discord `correlation` / the /status snapshot
//
// Locking contract: every handler is read-only, drain-aware
// (rejectIfDraining) and token-guarded (requireAPIAuth). SQLite reads run
// BEFORE taking ss.mu (never across it) per the #879/#1224 convention — a
// slow DB read must never stall the trading loop.

// uiOpsMaxLimit caps the diagnostics page size so a single dashboard poll
// can't marshal an unbounded row set.
const uiOpsMaxLimit = 500

// handleAPILeaderboard serves all leaderboard entries ranked by PnL%
// descending — the same data layer as the Discord `leaderboard` command
// (buildLeaderboardEntries), without the top-N truncation (presentation
// belongs to the client). Sharpe is omitted (0) exactly like the Discord
// command; the overview endpoint already carries per-strategy Sharpe.
func (ss *StatusServer) handleAPILeaderboard(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// DB + price fetches before the state lock.
	var lifetime map[string]LifetimeTradeStats
	if ss.stateDB != nil {
		lifetime, _ = ss.stateDB.LifetimeTradeStatsAll()
	}
	prices := ss.fetchLiveMarkPrices()

	ss.strategiesMu.RLock()
	configs := append([]StrategyConfig(nil), ss.strategies...)
	intervalSeconds := ss.intervalSeconds
	ss.strategiesMu.RUnlock()

	ss.mu.RLock()
	entries := buildLeaderboardEntries(configs, ss.state, prices, nil, lifetime, intervalSeconds)
	ss.mu.RUnlock()

	sortLeaderboardEntriesByPnLPct(entries)
	if entries == nil {
		entries = []LeaderboardEntry{}
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// uiDiagnosticsRow is the JSON projection of one #1147 trade_diagnostics row.
// NetPnL is the convention-aware net-of-fees sum over ALL close legs of the
// position (trades join via tradeNetPnLSQL); the row's own RealizedPnL is
// pre-fee final-leg only and deliberately not exposed. Nullable metric
// pointers serialize as null while metrics_status != "ok" ("pending" etc.),
// matching the CLI report semantics.
type uiDiagnosticsRow struct {
	StrategyID    string    `json:"strategy_id"`
	PositionID    string    `json:"position_id,omitempty"`
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"`
	Timeframe     string    `json:"timeframe,omitempty"`
	RegimeAtOpen  string    `json:"regime_at_open,omitempty"`
	CloseReason   string    `json:"close_reason,omitempty"`
	EntryPrice    float64   `json:"entry_price"`
	ExitPrice     float64   `json:"exit_price"`
	Quantity      float64   `json:"quantity"`
	NetPnL        float64   `json:"net_pnl"`
	EntryATR      float64   `json:"entry_atr,omitempty"`
	OpenedAt      time.Time `json:"opened_at"`
	ClosedAt      time.Time `json:"closed_at"`
	MFEPrice      *float64  `json:"mfe_price"`
	MAEPrice      *float64  `json:"mae_price"`
	FavorablePct  *float64  `json:"favorable_pct"`
	AdversePct    *float64  `json:"adverse_pct"`
	CaptureRatio  *float64  `json:"capture_ratio"`
	MetricsStatus string    `json:"metrics_status"`
	LLMVerdict    *string   `json:"llm_verdict"`
}

// handleAPIDiagnostics serves paged #1147 trade-diagnostics rows, newest
// close first, optionally filtered by ?strategy=. Query params: strategy,
// limit (default 50, max uiOpsMaxLimit), offset. Pure DB read — no state lock.
func (ss *StatusServer) handleAPIDiagnostics(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}

	q := r.URL.Query()
	strategyID := q.Get("strategy")
	limit := 50
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > uiOpsMaxLimit {
		limit = uiOpsMaxLimit
	}
	offset := 0
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	// Bounded queries only — this endpoint is polled on the dashboard refresh
	// interval, so per-call cost must track the page size, not the lifetime
	// row count: SQL-side LIMIT/OFFSET for the page, and the trades net-PnL
	// join scoped to just the page's position IDs.
	rows, total, err := ss.stateDB.TradeDiagnosticsRowsPage(strategyID, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	positionIDs := make([]string, 0, len(rows))
	for _, rrow := range rows {
		if rrow.PositionID != "" {
			positionIDs = append(positionIDs, rrow.PositionID)
		}
	}
	netByPos, err := ss.stateDB.NetPnLForPositions(strategyID, positionIDs)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]uiDiagnosticsRow, 0, len(rows))
	for _, rrow := range rows {
		out = append(out, uiDiagnosticsRow{
			StrategyID:    rrow.StrategyID,
			PositionID:    rrow.PositionID,
			Symbol:        rrow.Symbol,
			Side:          rrow.Side,
			Timeframe:     rrow.Timeframe,
			RegimeAtOpen:  rrow.RegimeAtOpen,
			CloseReason:   rrow.CloseReason,
			EntryPrice:    rrow.EntryPrice,
			ExitPrice:     rrow.ExitPrice,
			Quantity:      rrow.Quantity,
			NetPnL:        diagRowNetPnL(rrow, netByPos),
			EntryATR:      rrow.EntryATR,
			OpenedAt:      rrow.OpenedAt,
			ClosedAt:      rrow.ClosedAt,
			MFEPrice:      rrow.MFEPrice,
			MAEPrice:      rrow.MAEPrice,
			FavorablePct:  rrow.FavorablePct,
			AdversePct:    rrow.AdversePct,
			CaptureRatio:  rrow.CaptureRatio,
			MetricsStatus: rrow.MetricsStatus,
			LLMVerdict:    rrow.LLMVerdict,
		})
	}
	writeJSON(w, map[string]any{
		"rows":   out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// handleAPICashflow serves the #1100–#1106 cashflow-journal wallet statuses
// (persisted state + journal aggregates, explicit shadow-only flags for
// OKX/TopStep) and the in-memory shared-wallet drift-tracker snapshot
// (#918/#954). Reads only persisted journal state — it never re-runs an
// exchange reconcile on the polling path. alarm_enabled reflects the
// GO_TRADER_CASHFLOW_JOURNAL_ALARM operator kill switch.
func (ss *StatusServer) handleAPICashflow(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Money-path display fidelity: a failed journal read must fail open to the
	// panel's "-" fallback, never render as a clean empty journal — mirror the
	// /api/diagnostics error contract (nil DB → 503, query error → 500).
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}
	wallets, err := ss.stateDB.ListCashflowJournalWallets()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wallets == nil {
		wallets = []CashflowJournalWalletStatus{}
	}
	writeJSON(w, map[string]any{
		"wallets":       wallets,
		"drift":         sharedWalletDriftTracker.Snapshot(),
		"alarm_enabled": cashflowJournalAlarmEnabled(),
	})
}

// handleAPIDeadStrategies lists strategies that have never opened a position
// (lifetime is_close=0 count == 0) — same predicate as the Discord
// `dead-strategies` command. Lifetime stats come from SQLite before the state
// lock; the ID walk holds ss.mu.RLock only.
func (ss *StatusServer) handleAPIDeadStrategies(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var lifetime map[string]LifetimeTradeStats
	if ss.stateDB != nil {
		lifetime, _ = ss.stateDB.LifetimeTradeStatsAll()
	}

	ss.mu.RLock()
	ids := sortedAppStateIDs(ss.state)
	ss.mu.RUnlock()

	dead := []string{}
	for _, id := range ids {
		if lifetime[id].PositionsOpened == 0 {
			dead = append(dead, id)
		}
	}
	writeJSON(w, map[string]any{"dead": dead, "total": len(ids)})
}

// uiCloseEvaluator is one close-registry entry plus any effective
// user_defaults.close overrides (#866/#1135) — the values that actually run
// in place of the registry defaults, mirroring the Discord
// /closing-strategies override marking.
type uiCloseEvaluator struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Platforms     []string               `json:"platforms"`
	DefaultParams map[string]interface{} `json:"default_params"`
	UserOverrides map[string]interface{} `json:"user_overrides,omitempty"`
}

// handleAPIClosingStrategies serves the #1203 read-only close-evaluator
// catalog. First call after startup spawns the (cached) close-registry
// subprocess via fetchCloseRegistryCatalog — outside any lock; subsequent
// calls hit the in-process cache.
func (ss *StatusServer) handleAPIClosingStrategies(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	entries, err := fetchCloseRegistryCatalog()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	ss.strategiesMu.RLock()
	userClose := ss.userCloseDefaults
	ss.strategiesMu.RUnlock()

	out := make([]uiCloseEvaluator, 0, len(entries))
	for _, e := range entries {
		ev := uiCloseEvaluator{
			Name:          e.Name,
			Description:   e.Description,
			Platforms:     append([]string(nil), e.Platforms...),
			DefaultParams: e.DefaultParams,
		}
		if userEntry, ok := closeDefaultsEntry(userClose, e.Name); ok {
			ev.UserOverrides = userEntry
		}
		out = append(out, ev)
	}
	writeJSON(w, map[string]any{"evaluators": out})
}

// handleAPICorrelation serves the latest correlation/concentration snapshot
// computed during the trading cycle — the same struct the Discord
// `correlation` command formats and /status embeds. null until the first
// cycle computes one.
func (ss *StatusServer) handleAPICorrelation(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ss.mu.RLock()
	snap := ss.state.CorrelationSnapshot
	ss.mu.RUnlock()
	writeJSON(w, map[string]any{"correlation": snap})
}
