package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StatusServer provides an HTTP endpoint for portfolio status.
type StatusServer struct {
	state          *AppState
	mu             *sync.RWMutex
	statusToken    string           // if non-empty, /status requires Authorization: Bearer <token>
	priceSymbols   []string         // BinanceUS spot symbols to always fetch prices for
	futuresSymbols []string         // CME futures contracts that need TopStep marks (#261)
	hlPerpsCoins   []string         // HL perps coins that need venue-native marks (#263)
	okxPerpsCoins  []string         // OKX perps coins that need venue-native marks (#263)
	strategies     []StrategyConfig // strategy configs for initial capital lookup
	stateDB        *StateDB         // SQLite DB for /history queries (may be nil)

	// Throttled logging for repeated mark-fetch failures on the /status
	// rail. /status can be polled frequently (oncall dashboard, monitoring),
	// so we don't want to spam logs on every hit — but silently discarding
	// errors leaves operators blind to a broken price rail. Emit the first
	// occurrence immediately, then at most once per perpsErrLogInterval.
	perpsErrMu              sync.Mutex
	lastFuturesErrLoggedAt  time.Time
	lastFuturesModeLoggedAt time.Time
	lastHLPerpsErrLoggedAt  time.Time
	lastOKXPerpsErrLoggedAt time.Time
}

// perpsErrLogInterval caps how often /status logs repeated mark-fetch
// failures. 5m produces a reasonable audit trail without drowning the log
// on sustained outages during frequent dashboard polling.
const perpsErrLogInterval = 5 * time.Minute

// DefaultStatusPort is the default TCP port for the status HTTP server.
const DefaultStatusPort = 8099

// statusPortMaxAttempts bounds the auto-fallback sweep. On collision we try
// port, port+1, ..., port+statusPortMaxAttempts-1 before giving up.
const statusPortMaxAttempts = 5

func NewStatusServer(state *AppState, mu *sync.RWMutex, statusToken string, strategies []StrategyConfig, stateDB *StateDB) *StatusServer {
	// Spot symbols fetched via BinanceUS; perps marks now sourced from the
	// venue the position lives on (#263); futures on the TopStep rail (#261).
	symbols := collectPriceSymbols(strategies)
	futuresSymbols := collectFuturesMarkSymbols(strategies)
	hlCoins, okxCoins := collectPerpsMarkSymbols(strategies)
	return &StatusServer{
		state:          state,
		mu:             mu,
		statusToken:    statusToken,
		priceSymbols:   symbols,
		futuresSymbols: futuresSymbols,
		hlPerpsCoins:   hlCoins,
		okxPerpsCoins:  okxCoins,
		strategies:     strategies,
		stateDB:        stateDB,
	}
}

// logFuturesErrThrottled emits a [WARN] line for a fetch_futures_marks
// failure on the /status path, skipping emission if we have already
// logged within perpsErrLogInterval. Thread-safe — /status handlers
// run concurrently across requests.
func (ss *StatusServer) logFuturesErrThrottled(err error) {
	ss.perpsErrMu.Lock()
	defer ss.perpsErrMu.Unlock()
	now := time.Now()
	if !ss.lastFuturesErrLoggedAt.IsZero() && now.Sub(ss.lastFuturesErrLoggedAt) < perpsErrLogInterval {
		return
	}
	ss.lastFuturesErrLoggedAt = now
	fmt.Printf("[WARN] /status futures marks fetch failed for %v: %v — PortfolioNotional/Value will fall back to entry cost (throttled, next log in %s)\n",
		ss.futuresSymbols, err, perpsErrLogInterval)
}

// logFuturesModeThrottled emits a [WARN] line when fetch_futures_marks
// silently downgraded from live to paper mode on the /status path.
func (ss *StatusServer) logFuturesModeThrottled() {
	ss.perpsErrMu.Lock()
	defer ss.perpsErrMu.Unlock()
	now := time.Now()
	if !ss.lastFuturesModeLoggedAt.IsZero() && now.Sub(ss.lastFuturesModeLoggedAt) < perpsErrLogInterval {
		return
	}
	ss.lastFuturesModeLoggedAt = now
	fmt.Printf("[WARN] /status fetch_futures_marks: live mode init failed, degraded to paper (yfinance) — check TopStepX creds and network (throttled, next log in %s)\n",
		perpsErrLogInterval)
}

// logHLPerpsErrThrottled emits a [WARN] line for an HL perps marks fetch
// failure on the /status path, throttled to once per perpsErrLogInterval.
func (ss *StatusServer) logHLPerpsErrThrottled(err error) {
	ss.perpsErrMu.Lock()
	defer ss.perpsErrMu.Unlock()
	now := time.Now()
	if !ss.lastHLPerpsErrLoggedAt.IsZero() && now.Sub(ss.lastHLPerpsErrLoggedAt) < perpsErrLogInterval {
		return
	}
	ss.lastHLPerpsErrLoggedAt = now
	fmt.Printf("[WARN] /status HL perps marks fetch failed for %v: %v — PortfolioNotional/Value will fall back to entry cost (throttled, next log in %s)\n",
		ss.hlPerpsCoins, err, perpsErrLogInterval)
}

// logOKXPerpsErrThrottled emits a [WARN] line for an OKX perps marks fetch
// failure on the /status path, throttled to once per perpsErrLogInterval.
func (ss *StatusServer) logOKXPerpsErrThrottled(err error) {
	ss.perpsErrMu.Lock()
	defer ss.perpsErrMu.Unlock()
	now := time.Now()
	if !ss.lastOKXPerpsErrLoggedAt.IsZero() && now.Sub(ss.lastOKXPerpsErrLoggedAt) < perpsErrLogInterval {
		return
	}
	ss.lastOKXPerpsErrLoggedAt = now
	fmt.Printf("[WARN] /status OKX perps marks fetch failed for %v: %v — PortfolioNotional/Value will fall back to entry cost (throttled, next log in %s)\n",
		ss.okxPerpsCoins, err, perpsErrLogInterval)
}

// resolveStatusPort applies the precedence CLI flag > config > DefaultStatusPort.
// Non-positive values on either input are treated as "unset" and fall through
// to the next layer. Returns DefaultStatusPort if neither is set.
func resolveStatusPort(cliFlag, cfgPort int) int {
	if cliFlag > 0 {
		return cliFlag
	}
	if cfgPort > 0 {
		return cfgPort
	}
	return DefaultStatusPort
}

// bindWithFallback tries to bind localhost:port, then port+1, ..., up to
// maxAttempts consecutive ports. Returns the bound listener and the port
// that actually succeeded, or an error if all attempts failed. Each failed
// attempt is logged with the real net.Listen error (not a speculative
// "busy" message), so permission-denied and parse errors aren't masked
// as port collisions.
func bindWithFallback(port, maxAttempts int) (net.Listener, int, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tryPort := port + attempt
		addr := fmt.Sprintf("localhost:%d", tryPort)
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			return listener, tryPort, nil
		}
		lastErr = err
		fmt.Printf("[server] bind %s failed: %v\n", addr, err)
	}
	return nil, 0, fmt.Errorf("could not bind after %d attempts starting from %d: %w", maxAttempts, port, lastErr)
}

func (ss *StatusServer) Start(port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", ss.handleStatus)
	mux.HandleFunc("/health", ss.handleHealth)
	mux.HandleFunc("/history", ss.handleHistory)

	listener, boundPort, err := bindWithFallback(port, statusPortMaxAttempts)
	if err != nil {
		fmt.Printf("[server] WARNING: %v. Status endpoint unavailable.\n", err)
		return
	}
	if boundPort != port {
		// Prominent fallback notice: operators running `--once` next to a
		// live instance used to get a hard port-collision error; now the
		// bind silently advances, so make the advance itself visible.
		fmt.Printf("[server] NOTICE: requested port %d was in use, bound to %d instead\n", port, boundPort)
	}
	fmt.Printf("[server] Status endpoint at http://localhost:%d/status\n", boundPort)
	go func() {
		if err := http.Serve(listener, mux); err != nil {
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

	// Always fetch prices for all configured spot symbols + any with open
	// positions (in case config changed). Perps marks come from venue-native
	// fetchers below (#263), not BinanceUS, so we only pull spot symbols here.
	symbolSet := make(map[string]bool)
	for _, sym := range ss.priceSymbols {
		symbolSet[sym] = true
	}
	ss.mu.RLock()
	for _, s := range ss.state.Strategies {
		for sym := range s.Positions {
			// Include only spot-style keys (contain "/") to avoid routing
			// HL/OKX perps position keys through BinanceUS (#263).
			if strings.Contains(sym, "/") {
				symbolSet[sym] = true
			}
		}
	}
	ss.mu.RUnlock()

	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}

	// Fetch live prices WITHOUT holding the lock.
	prices := make(map[string]float64)
	if len(symbols) > 0 {
		p, err := FetchPrices(symbols)
		if err == nil {
			prices = p
		}
	}
	// HL perps marks — venue-native oracle (#263). Best-effort.
	if len(ss.hlPerpsCoins) > 0 {
		if hlMarks, err := fetchHyperliquidMids(ss.hlPerpsCoins); err == nil {
			mergePerpsMarks(prices, hlMarks)
		} else {
			ss.logHLPerpsErrThrottled(err)
		}
	}
	// OKX perps marks — venue-native oracle (#263). Best-effort.
	if len(ss.okxPerpsCoins) > 0 {
		if okxMarks, err := fetchOKXPerpsMids(ss.okxPerpsCoins); err == nil {
			mergePerpsMarks(prices, okxMarks)
		} else {
			ss.logOKXPerpsErrThrottled(err)
		}
	}
	// Fetch CME futures marks on their separate rail (#261). Best-effort:
	// on error, open futures positions fall back to pos.AvgCost. Errors are
	// throttle-logged so repeated /status polls don't spam on a sustained
	// outage, but the first failure (and periodic reminders) remain visible.
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
		CycleCount         int                           `json:"cycle_count"`
		Prices             map[string]float64            `json:"prices"`
		Strategies         map[string]StratStatus        `json:"strategies"`
		PortfolioRisk      PortfolioRiskState            `json:"portfolio_risk"`
		TotalValue         float64                       `json:"total_value"`
		TotalNotional      float64                       `json:"total_notional"`
		Correlation        *CorrelationSnapshot          `json:"correlation,omitempty"`
		ReconciliationGaps map[string]*ReconciliationGap `json:"reconciliation_gaps,omitempty"`
	}

	totalValue := 0.0
	for _, s := range ss.state.Strategies {
		totalValue += PortfolioValue(s, prices)
	}
	totalNotional := PortfolioNotional(ss.state.Strategies, prices)

	resp := StatusResp{
		CycleCount:         ss.state.CycleCount,
		Prices:             prices,
		Strategies:         make(map[string]StratStatus),
		PortfolioRisk:      ss.state.PortfolioRisk,
		TotalValue:         totalValue,
		TotalNotional:      totalNotional,
		Correlation:        ss.state.CorrelationSnapshot,
		ReconciliationGaps: ss.state.ReconciliationGaps,
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
