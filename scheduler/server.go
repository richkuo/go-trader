package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type StatusServer struct {
	state          *AppState
	mu             *sync.RWMutex
	statusToken    string
	priceSymbols   []string
	futuresSymbols []string
	hlPerpsCoins   []string
	okxPerpsCoins  []string
	stateDB        *StateDB
	candleFetcher  UICandleFetcher
	candleCache    *UICandleCache
	tuning         *tuningRunManager

	strategiesMu  sync.RWMutex
	strategies    []StrategyConfig
	configPath    string
	regime        *RegimeConfig
	configWriteMu sync.Mutex

	intervalSeconds   int
	userCloseDefaults CloseDefaultsMap

	globalNotifyRatchet *bool
	reloadConfig        func() error

	uiCfg         *Config
	uiNotifier    *MultiNotifier
	confirmMu     sync.Mutex
	confirmNonces map[string]confirmNonceEntry
	tradeActionMu sync.Mutex
	tradeDepsHook func(*manualCoreDeps)
	restartFn     func() error

	perpsErrMu              sync.Mutex
	lastFuturesErrLoggedAt  time.Time
	lastFuturesModeLoggedAt time.Time
	lastHLPerpsErrLoggedAt  time.Time
	lastOKXPerpsErrLoggedAt time.Time
}

const perpsErrLogInterval = 5 * time.Minute

const DefaultStatusPort = 8099

const statusPortMaxAttempts = 5

func NewStatusServer(state *AppState, mu *sync.RWMutex, statusToken string, strategies []StrategyConfig, stateDB *StateDB) *StatusServer {
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
		candleFetcher:  FetchUICandles,
		candleCache:    NewUICandleCache(30 * time.Second),
		reloadConfig:   requestSIGHUPReload,
	}
}

func (ss *StatusServer) UpdateStrategies(strategies []StrategyConfig) {
	if ss == nil {
		return
	}
	ss.strategiesMu.Lock()
	defer ss.strategiesMu.Unlock()
	ss.strategies = append([]StrategyConfig(nil), strategies...)
}

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

func resolveStatusPort(cliFlag, cfgPort int) int {
	if cliFlag > 0 {
		return cliFlag
	}
	if cfgPort > 0 {
		return cfgPort
	}
	return DefaultStatusPort
}

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
	mux.HandleFunc("/dashboard", ss.handleDashboard)
	mux.HandleFunc("/dashboard/", ss.handleDashboard)
	mux.HandleFunc("/tuning", ss.handleTuning)
	mux.HandleFunc("/tuning/", ss.handleTuning)
	mux.HandleFunc("/reports", ss.handleReports)
	mux.HandleFunc("/reports/", ss.handleReports)
	mux.HandleFunc("/api/strategies", ss.handleAPIStrategies)
	mux.HandleFunc("/api/strategies/overview", ss.handleAPIStrategiesOverview)
	mux.HandleFunc("/api/regime", ss.handleAPIRegime)
	mux.HandleFunc("/api/regime/transitions", ss.handleAPIRegimeTransitions)
	mux.HandleFunc("/api/leaderboard", ss.handleAPILeaderboard)
	mux.HandleFunc("/api/diagnostics", ss.handleAPIDiagnostics)
	mux.HandleFunc("/api/cashflow", ss.handleAPICashflow)
	mux.HandleFunc("/api/strategies/dead", ss.handleAPIDeadStrategies)
	mux.HandleFunc("/api/closing-strategies", ss.handleAPIClosingStrategies)
	mux.HandleFunc("/api/correlation", ss.handleAPICorrelation)
	mux.HandleFunc("/api/tuning/runs", ss.handleAPITuningRuns)
	mux.HandleFunc("/api/tuning/runs/", ss.handleAPITuningRun)
	mux.HandleFunc("/api/tuning/apply", ss.handleAPITuningApply)
	mux.HandleFunc("/api/config/notifications", ss.handleAPIConfigNotifications)
	mux.HandleFunc("/api/confirm", ss.handleAPIConfirm)
	mux.HandleFunc("/api/config/add-strategy", ss.handleAPIAddStrategy)
	mux.HandleFunc("/api/strategies/", ss.handleAPIStrategy)

	listener, boundPort, err := bindWithFallback(port, statusPortMaxAttempts)
	if err != nil {
		fmt.Printf("[server] WARNING: %v. Status endpoint unavailable.\n", err)
		return
	}
	if boundPort != port {
		fmt.Printf("[server] WARNING: requested port %d was in use, bound to %d instead — another go-trader may already be running on %d; compare /health pid across ports\n", port, boundPort, port)
	}
	fmt.Printf("[server] Status endpoint at http://localhost:%d/status\n", boundPort)
	fmt.Printf("[server] Dashboard at http://localhost:%d/dashboard\n", boundPort)
	fmt.Printf("[server] Tuning at http://localhost:%d/tuning\n", boundPort)
	if ss.statusToken != "" {
		fmt.Printf("[server] Dashboard API requires the configured status token\n")
	} else {
		fmt.Printf("[server] NOTE: status_token unset — dashboard mutations are open to any local (loopback) client; set status_token if other users can reach this host\n")
	}
	go func() {
		if err := http.Serve(listener, mux); err != nil {
			fmt.Printf("[server] HTTP server error: %v\n", err)
		}
	}()
}

func (ss *StatusServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pid := os.Getpid()

	if isDraining() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"status": "draining", "pid": pid})
		return
	}

	ss.mu.RLock()
	lastCycle := ss.state.LastCycle
	ss.mu.RUnlock()

	resp := map[string]any{
		"status":  "ok",
		"version": Version,
		"pid":     pid,
	}
	if !lastCycle.IsZero() && time.Since(lastCycle) > 30*time.Minute {
		resp["status"] = "unhealthy"
		resp["reason"] = "main loop stale"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(resp)
}

func (ss *StatusServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if ss.statusToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+ss.statusToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
	}

	prices := ss.fetchLiveMarkPrices()

	ss.mu.RLock()
	defer ss.mu.RUnlock()

	type StratStatus struct {
		ID                             string                     `json:"id"`
		Type                           string                     `json:"type"`
		Cash                           float64                    `json:"cash"`
		InitialCapital                 float64                    `json:"initial_capital"`
		Positions                      map[string]*Position       `json:"positions"`
		OptionPositions                map[string]*OptionPosition `json:"option_positions"`
		TradeCount                     int                        `json:"trade_count"`
		PortfolioValue                 float64                    `json:"portfolio_value"`
		PnL                            float64                    `json:"pnl"`
		PnLPct                         float64                    `json:"pnl_pct"`
		PoolBudget                     bool                       `json:"pool_budget,omitempty"`
		RiskState                      RiskState                  `json:"risk_state"`
		Regime                         string                     `json:"regime,omitempty"`
		RegimeGateFailClosed           bool                       `json:"regime_gate_fail_closed,omitempty"`
		BaseDirection                  string                     `json:"base_direction,omitempty"`
		BaseInvertSignal               bool                       `json:"base_invert_signal,omitempty"`
		EffectiveDirection             string                     `json:"effective_direction,omitempty"`
		EffectiveInvertSignal          bool                       `json:"effective_invert_signal,omitempty"`
		RegimeDirectionalPolicy        bool                       `json:"regime_directional_policy,omitempty"`
		EffectivePolicyRegime          string                     `json:"effective_policy_regime,omitempty"`
		DirectionalCertificationStatus string                     `json:"directional_certification_status,omitempty"`
		DirectionalCertificationCell   string                     `json:"directional_certification_cell,omitempty"`
		RegimeDivergence               *RegimeDivergenceState     `json:"regime_divergence,omitempty"`
		RegimeProfile                  *RegimeProfileState        `json:"regime_profile,omitempty"`
		Paused                         bool                       `json:"paused,omitempty"`
		Hedge                          *HedgeStatus               `json:"hedge,omitempty"`
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

	totalValue := latestDisplayTotal(ss.state, prices)
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

	ss.strategiesMu.RLock()
	cfgByID := make(map[string]StrategyConfig, len(ss.strategies))
	for _, sc := range ss.strategies {
		cfgByID[sc.ID] = sc
	}
	ss.strategiesMu.RUnlock()

	for id, s := range ss.state.Strategies {
		pv := displayStrategyValue(s, prices)
		sc := cfgByID[id]
		initCap := EffectiveInitialCapital(sc, s)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		dirView := directionalStatusForStrategy(sc, s, ss.regime, time.Now().UTC())

		resp.Strategies[id] = StratStatus{
			ID:                             s.ID,
			Type:                           s.Type,
			Cash:                           s.Cash,
			InitialCapital:                 initCap,
			Positions:                      s.Positions,
			OptionPositions:                s.OptionPositions,
			TradeCount:                     len(s.TradeHistory),
			PortfolioValue:                 pv,
			PnL:                            pnl,
			PnLPct:                         pnlPct,
			PoolBudget:                     usesSharedWalletPoolBudget(sc),
			RiskState:                      s.RiskState,
			Regime:                         strategyDisplayRegimeLabel(s, sc, ss.regime),
			RegimeGateFailClosed:           regimeGateFailClosedActive(sc, s, ss.regime),
			BaseDirection:                  dirView.BaseDirection,
			BaseInvertSignal:               dirView.BaseInvertSignal,
			EffectiveDirection:             dirView.EffectiveDirection,
			EffectiveInvertSignal:          dirView.EffectiveInvertSignal,
			RegimeDirectionalPolicy:        dirView.PolicyConfigured,
			EffectivePolicyRegime:          dirView.EffectivePolicyRegime,
			DirectionalCertificationStatus: dirView.CertStatus,
			DirectionalCertificationCell:   dirView.CertCell,
			RegimeDivergence:               s.RegimeDivergence,
			RegimeProfile:                  s.RegimeProfile,
			Paused:                         sc.Paused,
			Hedge:                          buildHedgeStatus(sc, s),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (ss *StatusServer) fetchLiveMarkPrices() map[string]float64 {
	symbolSet := make(map[string]bool)
	for _, sym := range ss.priceSymbols {
		symbolSet[sym] = true
	}
	ss.mu.RLock()
	for _, s := range ss.state.Strategies {
		for sym := range s.Positions {
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

	prices := make(map[string]float64)
	if len(symbols) > 0 {
		if p, err := FetchPrices(symbols); err == nil {
			prices = p
		}
	}
	if len(ss.hlPerpsCoins) > 0 {
		if hlMarks, err := fetchHyperliquidMids(ss.hlPerpsCoins); err == nil {
			mergePerpsMarks(prices, hlMarks)
		} else {
			ss.logHLPerpsErrThrottled(err)
		}
	}
	if len(ss.okxPerpsCoins) > 0 {
		if okxMarks, err := fetchOKXPerpsMids(ss.okxPerpsCoins); err == nil {
			mergePerpsMarks(prices, okxMarks)
		} else {
			ss.logOKXPerpsErrThrottled(err)
		}
	}
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
	return prices
}

type directionalStatusView struct {
	BaseDirection         string
	BaseInvertSignal      bool
	EffectiveDirection    string
	EffectiveInvertSignal bool
	PolicyConfigured      bool
	EffectivePolicyRegime string
	CertStatus            string
	CertCell              string
}

func directionalStatusForStrategy(sc StrategyConfig, s *StrategyState, rc *RegimeConfig, now time.Time) directionalStatusView {
	view := directionalStatusView{
		BaseDirection:    EffectiveDirection(sc),
		BaseInvertSignal: sc.InvertSignal,
	}
	view.EffectiveDirection = view.BaseDirection
	view.EffectiveInvertSignal = view.BaseInvertSignal
	view.PolicyConfigured = sc.RegimeDirectionalPolicy.IsConfigured()
	if !view.PolicyConfigured {
		return view
	}
	posQty := 0.0
	posRegime := ""
	var certStates map[string]string
	for _, p := range s.Positions {
		if p != nil && p.Quantity > 0 {
			posQty = p.Quantity
			posRegime = positionDirectionalRegimeLabel(p, sc)
			certStates = p.DirectionCertifiedStatesAtOpen
			break
		}
	}
	currentDirRegime := strategyCurrentDirectionalRegime(s, sc)
	view.EffectivePolicyRegime = effectiveRegimeForPolicy(currentDirRegime, posRegime, posQty)
	if posQty <= 0 {
		certStates, _ = strategyDirectionalCertified(sc, rc, now)
	}
	view.EffectiveDirection = EffectiveDirectionForPositionGated(sc, currentDirRegime, posRegime, posQty, certStates)
	view.EffectiveInvertSignal = EffectiveInvertSignalForPositionGated(sc, currentDirRegime, posRegime, posQty, certStates)
	view.CertStatus = strategyDirectionalCertStatus(sc, rc, now).String()
	_, view.CertCell = directionalCertInspectStatus(sc, &Config{Regime: rc})
	return view
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
