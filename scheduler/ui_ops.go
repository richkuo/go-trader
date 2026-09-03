package main

import (
	"net/http"
	"strconv"
	"time"
)

const uiOpsMaxLimit = 500

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

type uiCloseEvaluator struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Platforms     []string               `json:"platforms"`
	DefaultParams map[string]interface{} `json:"default_params"`
	UserOverrides map[string]interface{} `json:"user_overrides,omitempty"`
}

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
	ss.strategiesMu.RLock()
	cfgStrategies := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()
	byScope := make(map[string]*CorrelationSnapshot)
	for _, scope := range activeScopes(cfgStrategies) {
		if snap := ss.state.scopeCorrelation(scope); snap != nil {
			byScope[string(scope)] = snap
		}
	}
	legacy := ss.state.scopeCorrelation(statusLegacyScope(activeScopes(cfgStrategies)))
	ss.mu.RUnlock()
	writeJSON(w, map[string]any{"correlation": legacy, "correlation_by_scope": byScope})
}
