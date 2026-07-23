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

// StatusServer provides an HTTP endpoint for portfolio status.
type StatusServer struct {
	state          *AppState
	mu             *sync.RWMutex
	statusToken    string   // if non-empty, /status requires Authorization: Bearer <token>
	priceSymbols   []string // BinanceUS spot symbols to always fetch prices for
	futuresSymbols []string // CME futures contracts that need TopStep marks (#261)
	hlPerpsCoins   []string // HL perps coins that need venue-native marks (#263)
	okxPerpsCoins  []string // OKX perps coins that need venue-native marks (#263)
	stateDB        *StateDB // SQLite DB for /history queries (may be nil)
	candleFetcher  UICandleFetcher
	candleCache    *UICandleCache
	tuning         *tuningRunManager // #1339 persistent dedicated research lane

	// strategiesMu protects `strategies` independently of `mu`. SIGHUP holds
	// the global state `mu.Lock()` across the reload (see config_reload.go);
	// UpdateStrategies is invoked from that path, so reusing `mu` here would
	// deadlock. Readers on the /api/strategies path also benefit: they no
	// longer contend with the scheduler's state writes during dashboard polls.
	strategiesMu  sync.RWMutex
	strategies    []StrategyConfig // strategy configs for initial capital lookup
	configPath    string           // live config file for tuner Apply (#811)
	regime        *RegimeConfig    // global regime settings for simulate preview
	configWriteMu sync.Mutex       // serializes dashboard config Apply writes

	// #1231 config-derived context for the read-only ops endpoints, refreshed
	// via SetConfigContext on startup and SIGHUP. Guarded by strategiesMu
	// (SetConfigContext runs from the reload path which already holds mu).
	intervalSeconds   int              // global check interval for leaderboard entries
	userCloseDefaults CloseDefaultsMap // user_defaults.close for /api/closing-strategies override marking

	// #1256 mutation surface. globalNotifyRatchet mirrors the top-level
	// notify_ratchet_triggers (#1110) for GET /api/config/notifications
	// (guarded by strategiesMu, refreshed via SetConfigContext).
	// reloadConfig signals the process to hot-reload after a UI config write
	// (requestSIGHUPReload in production; injectable for tests).
	globalNotifyRatchet *bool
	reloadConfig        func() error

	// #1257 trade-action surface (ui_confirm.go / ui_trade_actions.go).
	// uiCfg is the live *Config snapshot (guarded by strategiesMu, refreshed
	// via SetConfigContext); uiNotifier is the daemon notifier (SetNotifier).
	// confirmNonces holds the short-lived single-use confirm nonces.
	uiCfg         *Config
	uiNotifier    *MultiNotifier
	confirmMu     sync.Mutex
	confirmNonces map[string]confirmNonceEntry
	// tradeActionMu serializes dashboard trade-action submits so the
	// double-fire guard's check-then-submit is atomic (#1260 review).
	tradeActionMu sync.Mutex
	// tradeDepsHook lets tests stub the on-chain exec seams of the manual
	// cores (nil in production).
	tradeDepsHook func(*manualCoreDeps)
	// #1258 structural mutations (ui_structural.go): restartFn is the restart
	// trigger fired when a confirmed structural write asked for
	// apply-via-restart (nil → restartSelf; injectable for tests).
	restartFn func() error

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
		candleFetcher:  FetchUICandles,
		candleCache:    NewUICandleCache(30 * time.Second),
		reloadConfig:   requestSIGHUPReload,
	}
}

// UpdateStrategies refreshes config-derived status metadata after a hot reload.
// Uses the dedicated strategiesMu — not the global state mu — because the SIGHUP
// reload path already holds mu.Lock() when it calls this through
// applyHotReloadConfig (config_reload.go), and the global mu is not reentrant.
func (ss *StatusServer) UpdateStrategies(strategies []StrategyConfig) {
	if ss == nil {
		return
	}
	ss.strategiesMu.Lock()
	defer ss.strategiesMu.Unlock()
	ss.strategies = append([]StrategyConfig(nil), strategies...)
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
	// #1231 read-only ops endpoints (ui_ops.go). "/api/strategies/dead" is an
	// exact pattern, so it wins over the "/api/strategies/" prefix handler.
	mux.HandleFunc("/api/leaderboard", ss.handleAPILeaderboard)
	mux.HandleFunc("/api/diagnostics", ss.handleAPIDiagnostics)
	mux.HandleFunc("/api/cashflow", ss.handleAPICashflow)
	mux.HandleFunc("/api/strategies/dead", ss.handleAPIDeadStrategies)
	mux.HandleFunc("/api/closing-strategies", ss.handleAPIClosingStrategies)
	mux.HandleFunc("/api/correlation", ss.handleAPICorrelation)
	// #1339 persistent strategy-tuning jobs. The exact collection route
	// handles GET/POST; the longer prefix serves one stable run id.
	// #1341 operator-explicit promotion (exact /apply sibling; never under /runs/).
	mux.HandleFunc("/api/tuning/runs", ss.handleAPITuningRuns)
	mux.HandleFunc("/api/tuning/runs/", ss.handleAPITuningRun)
	mux.HandleFunc("/api/tuning/apply", ss.handleAPITuningApply)
	// #1256 low-risk mutation surface (ui_mutations.go): global notification
	// toggle; per-strategy pause + notification toggles route through the
	// "/api/strategies/" prefix handler below.
	mux.HandleFunc("/api/config/notifications", ss.handleAPIConfigNotifications)
	// #1257 trade-action confirm nonce; the trade-action endpoints route
	// through the "/api/strategies/" prefix handler below.
	mux.HandleFunc("/api/confirm", ss.handleAPIConfirm)
	mux.HandleFunc("/api/config/add-strategy", ss.handleAPIAddStrategy)
	mux.HandleFunc("/api/strategies/", ss.handleAPIStrategy)

	listener, boundPort, err := bindWithFallback(port, statusPortMaxAttempts)
	if err != nil {
		fmt.Printf("[server] WARNING: %v. Status endpoint unavailable.\n", err)
		return
	}
	if boundPort != port {
		// Prominent fallback notice: operators running `--once` next to a
		// live instance used to get a hard port-collision error; now the
		// bind silently advances, so make the advance itself visible. A
		// fallback on the daemon path can also mean a duplicate instance is
		// already bound to the configured port — the #849 state-DB lock is the
		// authoritative guard, but flag it loudly here too and point at the
		// /health pid as the external detection signal.
		fmt.Printf("[server] WARNING: requested port %d was in use, bound to %d instead — another go-trader may already be running on %d; compare /health pid across ports\n", port, boundPort, port)
	}
	fmt.Printf("[server] Status endpoint at http://localhost:%d/status\n", boundPort)
	fmt.Printf("[server] Dashboard at http://localhost:%d/dashboard\n", boundPort)
	fmt.Printf("[server] Tuning at http://localhost:%d/tuning\n", boundPort)
	if ss.statusToken != "" {
		fmt.Printf("[server] Dashboard API requires the configured status token\n")
	} else {
		// #1229/#1256: mutations (incl. leverage/direction/stop-loss via the
		// tuner) are open to any loopback client when no token is set. Fine on
		// a single-user host; on a shared host, set status_token.
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

	// `pid` (#849) lets external monitoring detect a duplicate from the
	// outside: alert when health.pid != systemd MainPID, or when two ports
	// (8099/8100) both report healthy with different pids. It complements the
	// state-DB flock — a cheap detection aid, not a replacement. Included on
	// every branch (incl. draining) so the signal is always available.
	pid := os.Getpid()

	// 503 once SIGTERM has fired so any future load-balancer-style probe
	// stops sending traffic immediately. Returns before the staleness check
	// since the daemon is intentionally winding down.
	if isDraining() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"status": "draining", "pid": pid})
		return
	}

	ss.mu.RLock()
	lastCycle := ss.state.LastCycle
	ss.mu.RUnlock()

	// `version` is the build-stamped Version (#682) so scripts/update.sh can
	// confirm the post-restart process matches the just-built binary before
	// declaring the update successful (and rolling back otherwise). update.sh
	// matches the `"version":"<ver>"` substring, which the sorted-key JSON
	// encoding preserves regardless of the added pid field.
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
	// #38: Optional bearer token auth for /status.
	if ss.statusToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+ss.statusToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
	}

	prices := ss.fetchLiveMarkPrices()

	// Re-acquire read lock to build the response
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
		RiskState                      RiskState                  `json:"risk_state"`
		Regime                         string                     `json:"regime,omitempty"`
		RegimeGateFailClosed           bool                       `json:"regime_gate_fail_closed,omitempty"`          // #1278: entry gate is actively failing closed — allowed_regimes configured, policy "closed", strategy flat, and the cycle store has no gate label; fresh opens are held
		BaseDirection                  string                     `json:"base_direction,omitempty"`                   // #779: base direction from config (pre-policy resolution)
		BaseInvertSignal               bool                       `json:"base_invert_signal,omitempty"`               // #779: base invert from config (pre-policy resolution)
		EffectiveDirection             string                     `json:"effective_direction,omitempty"`              // #779: resolved direction for the active regime (policy override or base)
		EffectiveInvertSignal          bool                       `json:"effective_invert_signal,omitempty"`          // #779: resolved invert for the active regime
		RegimeDirectionalPolicy        bool                       `json:"regime_directional_policy,omitempty"`        // #779: true when strategy has a policy block configured
		EffectivePolicyRegime          string                     `json:"effective_policy_regime,omitempty"`          // #779: regime key the resolver used (pos.Regime while open, current regime when flat); shown only when policy is configured
		DirectionalCertificationStatus string                     `json:"directional_certification_status,omitempty"` // #1157: certified|expired|uncertified for the strategy's (asset,tf,classifier) cell
		DirectionalCertificationCell   string                     `json:"directional_certification_cell,omitempty"`   // #1157: (asset,timeframe,classifier) certification key
		RegimeDivergence               *RegimeDivergenceState     `json:"regime_divergence,omitempty"`                // #907: active window-divergence state; nil when none
		RegimeProfile                  *RegimeProfileState        `json:"regime_profile,omitempty"`                   // #998: active regime-profile allocation switch state; nil when none
		Paused                         bool                       `json:"paused,omitempty"`                           // #1150: strategy is paused — position-increasing signals held; closes and SL/TP management still run
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
		totalValue += displayStrategyValue(s, prices)
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

	// Build config lookup for EffectiveInitialCapital. strategies has its own
	// mutex now — see the strategiesMu doc on StatusServer.
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
		// #779: surface base + effective directional policy so operators can
		// verify why the bot is in long vs. short mode. Effective values are
		// what the next signal will be evaluated under — pulled by replaying
		// the resolver against the strategy's first open position (or flat).
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
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// fetchLiveMarkPrices returns best-effort mark prices for /status and dashboard
// API handlers. Call without holding ss.mu.
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

// directionalStatusView bundles the #779/#1157 directional-policy display
// fields shared by /status and the dashboard per-strategy status endpoint.
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

// directionalStatusForStrategy replays the directional-policy resolver
// (cert-gated per #1085/#1157) against the strategy's first open position, or
// the live regime when flat. Read-only; caller supplies a consistent snapshot
// of the strategy state (call under ss.mu.RLock, or on a cloned snapshot).
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
		// #1159: skip hedge legs — map iteration could pick the (inverse)
		// hedge first and misreport the directional view.
		if p != nil && p.Quantity > 0 && !p.IsHedge {
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
