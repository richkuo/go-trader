package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// StatusServer provides an HTTP endpoint for portfolio status.
type StatusServer struct {
	state          *AppState
	mu             *sync.RWMutex
	statusToken    string            // if non-empty, /status requires Authorization: Bearer <token>
	priceSymbols   []string          // symbols to always fetch prices for
	priceMirror    map[string]string // perps position-key → fetch-key aliases (#245)
	futuresSymbols []string          // CME futures contracts that need TopStep marks (#261)
	strategies     []StrategyConfig  // strategy configs for initial capital lookup
	stateDB        *StateDB          // SQLite DB for /history queries (may be nil)

	// Throttled logging for repeated fetch_futures_marks.py failures and
	// paper_fallback mode on the /status rail. /status can be polled
	// frequently (oncall dashboard, monitoring), so we don't want to spam
	// logs on every hit — but silently discarding the error (or the
	// silent live→paper downgrade) leaves operators blind to a broken
	// TopStep rail. For each signal (err / paper_fallback), emit the
	// first occurrence immediately, then at most once per
	// futuresErrLogInterval while the condition persists.
	futuresErrMu            sync.Mutex
	lastFuturesErrLoggedAt  time.Time
	lastFuturesModeLoggedAt time.Time
}

// futuresErrLogInterval caps how often /status logs repeated
// fetch_futures_marks failures. Kept at 5m so a sustained outage still
// produces a reasonable trail (every cycle summary) without drowning
// the log on every dashboard poll.
const futuresErrLogInterval = 5 * time.Minute

func NewStatusServer(state *AppState, mu *sync.RWMutex, statusToken string, strategies []StrategyConfig, stateDB *StateDB) *StatusServer {
	// Extract all traded symbols from strategy configs so prices are always
	// fetched, even when no positions are open. Perps strategies key their
	// positions under the base asset (e.g. "BTC"); collectPriceSymbols
	// normalizes the fetch key to "BTC/USDT" and returns a mirror map so
	// the handler can back-fill the base-asset alias after FetchPrices.
	// Futures positions (TopStep CME) are on a separate price rail — #261.
	symbols, mirror := collectPriceSymbols(strategies)
	futuresSymbols := collectFuturesMarkSymbols(strategies)
	return &StatusServer{
		state:          state,
		mu:             mu,
		statusToken:    statusToken,
		priceSymbols:   symbols,
		priceMirror:    mirror,
		futuresSymbols: futuresSymbols,
		strategies:     strategies,
		stateDB:        stateDB,
	}
}

// logFuturesErrThrottled emits a [WARN] line for a fetch_futures_marks
// failure on the /status path, skipping emission if we have already
// logged within futuresErrLogInterval. Thread-safe — /status handlers
// run concurrently across requests.
func (ss *StatusServer) logFuturesErrThrottled(err error) {
	ss.futuresErrMu.Lock()
	defer ss.futuresErrMu.Unlock()
	now := time.Now()
	if !ss.lastFuturesErrLoggedAt.IsZero() && now.Sub(ss.lastFuturesErrLoggedAt) < futuresErrLogInterval {
		return
	}
	ss.lastFuturesErrLoggedAt = now
	fmt.Printf("[WARN] /status futures marks fetch failed for %v: %v — PortfolioNotional/Value will fall back to entry cost (throttled, next log in %s)\n",
		ss.futuresSymbols, err, futuresErrLogInterval)
}

// logFuturesModeThrottled emits a [WARN] line when fetch_futures_marks
// silently downgraded from live to paper mode on the /status path. Uses
// the same 5m window as logFuturesErrThrottled so a sustained outage
// still produces a reasonable trail without drowning the log on every
// dashboard poll. Thread-safe. Shares futuresErrMu with the error
// throttle since the state is a handful of timestamps and contention
// is negligible.
func (ss *StatusServer) logFuturesModeThrottled() {
	ss.futuresErrMu.Lock()
	defer ss.futuresErrMu.Unlock()
	now := time.Now()
	if !ss.lastFuturesModeLoggedAt.IsZero() && now.Sub(ss.lastFuturesModeLoggedAt) < futuresErrLogInterval {
		return
	}
	ss.lastFuturesModeLoggedAt = now
	fmt.Printf("[WARN] /status fetch_futures_marks: live mode init failed, degraded to paper (yfinance) — check TopStepX creds and network (throttled, next log in %s)\n",
		futuresErrLogInterval)
}

func (ss *StatusServer) Start(port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", ss.handleStatus)
	mux.HandleFunc("/health", ss.handleHealth)
	mux.HandleFunc("/history", ss.handleHistory)

	addr := fmt.Sprintf("localhost:%d", port)
	go func() {
		fmt.Printf("[server] Status endpoint at http://%s/status\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("[server] HTTP server error: %v\n", err)
		}
	}()
}

func (ss *StatusServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	ss.mu.RLock()
	lastCycle := ss.state.LastCycle
	ss.mu.RUnlock()

	// Stale if main loop hasn't completed a cycle in the last 30 minutes
	if !lastCycle.IsZero() && time.Since(lastCycle) > 30*time.Minute {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"unhealthy","reason":"main loop stale"}`))
		return
	}
	w.Write([]byte(`{"status":"ok"}`))
}

func (ss *StatusServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	// #38: Optional bearer token auth for /status.
	if ss.statusToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+ss.statusToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
	}

	// Always fetch prices for all configured symbols + any with open positions.
	symbolSet := make(map[string]bool)
	for _, sym := range ss.priceSymbols {
		symbolSet[sym] = true
	}
	// Also include symbols from open positions (in case config changed).
	ss.mu.RLock()
	for _, s := range ss.state.Strategies {
		for sym := range s.Positions {
			symbolSet[sym] = true
		}
	}
	ss.mu.RUnlock()

	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}

	// Fetch live prices WITHOUT holding the lock
	prices := make(map[string]float64)
	if len(symbols) > 0 {
		p, err := FetchPrices(symbols)
		if err == nil {
			prices = p
		}
	}
	// Back-fill perps position-key aliases (#245).
	mirrorPerpsPrices(prices, ss.priceMirror)
	// Fetch CME futures marks on their separate rail (#261). Best-effort:
	// on error, open futures positions fall back to pos.AvgCost — same
	// degradation behavior as the main cycle loop. Errors are logged
	// through a throttle so repeated /status polls don't spam the log
	// on a sustained outage, but the first failure (and periodic
	// reminders) are still visible.
	if len(ss.futuresSymbols) > 0 {
		if marks, mode, err := FetchFuturesMarks(ss.futuresSymbols); err == nil {
			if mode == FuturesMarkModePaperFallback {
				ss.logFuturesModeThrottled()
			}
			mergeFuturesMarks(prices, marks)
		} else {
			ss.logFuturesErrThrottled(err)
		}
	}

	// Re-acquire read lock to build the response
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	type StratStatus struct {
		ID              string                     `json:"id"`
		Type            string                     `json:"type"`
		Cash            float64                    `json:"cash"`
		InitialCapital  float64                    `json:"initial_capital"`
		Positions       map[string]*Position       `json:"positions"`
		OptionPositions map[string]*OptionPosition `json:"option_positions"`
		TradeCount      int                        `json:"trade_count"`
		PortfolioValue  float64                    `json:"portfolio_value"`
		PnL             float64                    `json:"pnl"`
		PnLPct          float64                    `json:"pnl_pct"`
		RiskState       RiskState                  `json:"risk_state"`
	}

	type StatusResp struct {
		CycleCount    int                    `json:"cycle_count"`
		Prices        map[string]float64     `json:"prices"`
		Strategies    map[string]StratStatus `json:"strategies"`
		PortfolioRisk PortfolioRiskState     `json:"portfolio_risk"`
		TotalValue    float64                `json:"total_value"`
		TotalNotional float64                `json:"total_notional"`
		Correlation   *CorrelationSnapshot   `json:"correlation,omitempty"`
	}

	totalValue := 0.0
	for _, s := range ss.state.Strategies {
		totalValue += PortfolioValue(s, prices)
	}
	totalNotional := PortfolioNotional(ss.state.Strategies, prices)

	resp := StatusResp{
		CycleCount:    ss.state.CycleCount,
		Prices:        prices,
		Strategies:    make(map[string]StratStatus),
		PortfolioRisk: ss.state.PortfolioRisk,
		TotalValue:    totalValue,
		TotalNotional: totalNotional,
		Correlation:   ss.state.CorrelationSnapshot,
	}

	// Build config lookup for EffectiveInitialCapital.
	cfgByID := make(map[string]StrategyConfig, len(ss.strategies))
	for _, sc := range ss.strategies {
		cfgByID[sc.ID] = sc
	}

	for id, s := range ss.state.Strategies {
		pv := PortfolioValue(s, prices)
		sc := cfgByID[id]
		initCap := EffectiveInitialCapital(sc, s)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		resp.Strategies[id] = StratStatus{
			ID:              s.ID,
			Type:            s.Type,
			Cash:            s.Cash,
			InitialCapital:  initCap,
			Positions:       s.Positions,
			OptionPositions: s.OptionPositions,
			TradeCount:      len(s.TradeHistory),
			PortfolioValue:  pv,
			PnL:             pnl,
			PnLPct:          pnlPct,
			RiskState:       s.RiskState,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (ss *StatusServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	if ss.statusToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+ss.statusToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
	}

	if ss.stateDB == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"database not available"}`))
		return
	}

	q := r.URL.Query()
	strategyID := q.Get("strategy")
	symbol := q.Get("symbol")

	limit := 50
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	var since, until time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	if u := q.Get("until"); u != "" {
		until, _ = time.Parse(time.RFC3339, u)
	}

	trades, total, err := ss.stateDB.QueryTradeHistory(strategyID, symbol, since, until, limit, offset)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	type HistoryResp struct {
		Trades []Trade `json:"trades"`
		Total  int     `json:"total"`
		Limit  int     `json:"limit"`
		Offset int     `json:"offset"`
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HistoryResp{
		Trades: trades,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}
