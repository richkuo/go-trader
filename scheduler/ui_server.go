package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed static/ui/*
var uiAssets embed.FS

type UIStrategy struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Platform  string `json:"platform"`
	Strategy  string `json:"strategy"`
	Symbol    string `json:"symbol"`
	Timeframe string `json:"timeframe"`
	Direction string `json:"direction,omitempty"`
}

type UIStrategyOverview struct {
	ID             string  `json:"id"`
	Platform       string  `json:"platform"`
	Symbol         string  `json:"symbol"`
	PnLPct         float64 `json:"pnl_pct"`
	WinRate        float64 `json:"win_rate,omitempty"`
	Sharpe         float64 `json:"sharpe,omitempty"`
	Regime         string  `json:"regime,omitempty"`
	Direction      string  `json:"direction,omitempty"`
	PnL            float64 `json:"pnl"`
	PortfolioValue float64 `json:"portfolio_value"`
	InitialCapital float64 `json:"initial_capital"`
}

type UIStrategyStatus struct {
	ID              string                     `json:"id"`
	Type            string                     `json:"type"`
	Platform        string                     `json:"platform"`
	Symbol          string                     `json:"symbol"`
	Timeframe       string                     `json:"timeframe"`
	Direction       string                     `json:"direction,omitempty"`
	Cash            float64                    `json:"cash"`
	InitialCapital  float64                    `json:"initial_capital"`
	PortfolioValue  float64                    `json:"portfolio_value"`
	PnL             float64                    `json:"pnl"`
	PnLPct          float64                    `json:"pnl_pct"`
	TradeCount      int                        `json:"trade_count"`
	WinRate         float64                    `json:"win_rate,omitempty"`
	LifetimeStats   LifetimeTradeStats         `json:"lifetime_stats"`
	Sharpe          float64                    `json:"sharpe,omitempty"`
	Regime          string                     `json:"regime,omitempty"`
	RiskState       RiskState                  `json:"risk_state"`
	Positions       map[string]*Position       `json:"positions"`
	OptionPositions map[string]*OptionPosition `json:"option_positions"`
	Leverage        float64                    `json:"leverage,omitempty"`
	SizingLeverage  float64                    `json:"sizing_leverage,omitempty"`
	MarginMode      string                     `json:"margin_mode,omitempty"`
}

type UIEquityPoint struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// uiEquityLookbackLimit caps closed-position rows for dashboard equity curves (#805).
// Independent of sharpeLookbackLimit so Sharpe tuning does not shrink sparklines.
const uiEquityLookbackLimit = 500

type UITradeMarker struct {
	Time        int64   `json:"time"`
	Position    string  `json:"position"`
	Color       string  `json:"color"`
	Shape       string  `json:"shape"`
	Text        string  `json:"text"`
	Side        string  `json:"side"`
	IsClose     bool    `json:"is_close"`
	Price       float64 `json:"price"`
	Quantity    float64 `json:"quantity"`
	RealizedPnL float64 `json:"realized_pnl,omitempty"`
	Details     string  `json:"details,omitempty"`
	Regime      string  `json:"regime,omitempty"`
}

func (ss *StatusServer) rejectIfDraining(w http.ResponseWriter) bool {
	if !isDraining() {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"status":"draining"}`))
	return true
}

func (ss *StatusServer) requireAPIAuth(w http.ResponseWriter, r *http.Request) bool {
	if ss.statusToken == "" {
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+ss.statusToken {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"unauthorized"}`))
	return false
}

func (ss *StatusServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sub, err := fs.Sub(uiAssets, "static/ui")
	if err != nil {
		http.Error(w, "ui assets unavailable", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/dashboard" || r.URL.Path == "/dashboard/" {
		http.ServeFileFS(w, r, sub, "index.html")
		return
	}
	http.StripPrefix("/dashboard/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func (ss *StatusServer) handleAPIStrategies(w http.ResponseWriter, r *http.Request) {
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
	if r.URL.Path != "/api/strategies" && r.URL.Path != "/api/strategies/" {
		http.NotFound(w, r)
		return
	}
	strategies := ss.uiStrategies()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]UIStrategy{"strategies": strategies})
}

func (ss *StatusServer) handleAPIStrategiesOverview(w http.ResponseWriter, r *http.Request) {
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
	if r.URL.Path != "/api/strategies/overview" && r.URL.Path != "/api/strategies/overview/" {
		http.NotFound(w, r)
		return
	}

	configs := ss.uiStrategies()
	out := make([]UIStrategyOverview, 0, len(configs))
	for _, item := range configs {
		overview, _, ok := ss.uiStrategyOverview(item.ID)
		if !ok {
			continue
		}
		out = append(out, overview)
	}
	writeJSON(w, map[string][]UIStrategyOverview{"strategies": out})
}

func (ss *StatusServer) handleAPIStrategy(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	id, resource, ok := parseStrategyAPIPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch resource {
	case "candles":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ss.handleAPIStrategyCandles(w, r, id)
	case "trades":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ss.handleAPIStrategyTrades(w, r, id)
	case "status":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ss.handleAPIStrategyStatus(w, r, id)
	case "equity":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ss.handleAPIStrategyEquity(w, r, id)
	case "config":
		switch r.Method {
		case http.MethodGet:
			ss.handleAPIStrategyConfig(w, r, id)
		case http.MethodPost:
			ss.handleAPIStrategyApplyConfig(w, r, id)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case "simulate":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ss.handleAPIStrategySimulate(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func parseStrategyAPIPath(p string) (id, resource string, ok bool) {
	rest := strings.TrimPrefix(p, "/api/strategies/")
	if rest == p || rest == "" {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	decoded, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	return decoded, parts[1], true
}

func (ss *StatusServer) uiStrategies() []UIStrategy {
	ss.strategiesMu.RLock()
	configs := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()

	out := make([]UIStrategy, 0, len(configs))
	for _, sc := range configs {
		out = append(out, uiStrategyFromConfig(sc))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func uiStrategyFromConfig(sc StrategyConfig) UIStrategy {
	return UIStrategy{
		ID:        sc.ID,
		Type:      sc.Type,
		Platform:  sc.Platform,
		Strategy:  strategyDisplayName(sc),
		Symbol:    strategyDisplaySymbol(sc),
		Timeframe: strategyDisplayTimeframe(sc),
		Direction: strategyDisplayDirection(sc),
	}
}

func strategyDisplayName(sc StrategyConfig) string {
	if sc.OpenStrategy.Name != "" {
		return sc.OpenStrategy.Name
	}
	if len(sc.Args) > 0 {
		return sc.Args[0]
	}
	return ""
}

func strategyDisplaySymbol(sc StrategyConfig) string {
	if sc.Symbol != "" {
		return sc.Symbol
	}
	if len(sc.Args) > 1 {
		return sc.Args[1]
	}
	return ""
}

func strategyDisplayTimeframe(sc StrategyConfig) string {
	if sc.Timeframe != "" {
		return sc.Timeframe
	}
	if len(sc.Args) > 2 {
		return sc.Args[2]
	}
	return ""
}

func strategyDisplayDirection(sc StrategyConfig) string {
	if sc.Type != "perps" && sc.Type != "manual" {
		return ""
	}
	return EffectiveDirection(sc)
}

func (ss *StatusServer) strategyConfig(id string) (StrategyConfig, bool) {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	for _, sc := range ss.strategies {
		if sc.ID == id {
			return sc, true
		}
	}
	return StrategyConfig{}, false
}

func (ss *StatusServer) handleAPIStrategyCandles(w http.ResponseWriter, r *http.Request, id string) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	from, to, limit := parseUITimeQuery(r)
	req := UICandleRequest{Strategy: sc, From: from, To: to, Limit: limit}
	cacheKey := req.CacheKey()
	if ss.candleCache != nil {
		if cached, ok := ss.candleCache.Get(cacheKey); ok {
			writeJSON(w, map[string]interface{}{
				"strategy_id": id,
				"source":      cached.Source + ":cached",
				"candles":     cached.Candles,
			})
			return
		}
	}

	var candles []UICandle
	source := ""
	var fetchErr error
	if ss.candleFetcher != nil {
		candles, source, fetchErr = ss.candleFetcher(req)
	}
	if fetchErr != nil || len(candles) == 0 {
		if ss.stateDB == nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("candle fetch failed: %v", fetchErr))
			return
		}
		trades, _, err := ss.stateDB.QueryTradeHistory(id, "", from, to, limit, 0)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		candles = tradeCandles(trades, strategyDisplayTimeframe(sc))
		source = "trades"
		if len(candles) == 0 && fetchErr != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("candle fetch failed: %v", fetchErr))
			return
		}
	}
	if ss.candleCache != nil {
		ss.candleCache.Set(cacheKey, UICandleResponse{Candles: candles, Source: source})
	}
	writeJSON(w, map[string]interface{}{
		"strategy_id": id,
		"source":      source,
		"candles":     candles,
	})
}

func (ss *StatusServer) handleAPIStrategyTrades(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := ss.strategyConfig(id); !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}
	from, to, limit := parseUITimeQuery(r)
	trades, total, err := ss.stateDB.QueryTradeHistory(id, "", from, to, limit, 0)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	markers := tradeMarkers(trades)
	writeJSON(w, map[string]interface{}{
		"strategy_id": id,
		"markers":     markers,
		"trades":      tradeMarkersForTable(markers),
		"total":       total,
	})
}

func (ss *StatusServer) uiStrategyOverview(id string) (UIStrategyOverview, LifetimeTradeStats, bool) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		return UIStrategyOverview{}, LifetimeTradeStats{}, false
	}

	ss.mu.RLock()
	strat := ss.state.Strategies[id]
	var snapshot StrategyState
	if strat != nil {
		snapshot = *strat
	}
	ss.mu.RUnlock()
	if strat == nil {
		return UIStrategyOverview{}, LifetimeTradeStats{}, false
	}

	prices := ss.fetchLiveMarkPrices()
	pv := PortfolioValue(&snapshot, prices)
	initCap := EffectiveInitialCapital(sc, &snapshot)
	pnl := pv - initCap
	pnlPct := 0.0
	if initCap > 0 {
		pnlPct = pnl / initCap * 100
	}

	lifetime := LifetimeTradeStats{}
	sharpe := 0.0
	if ss.stateDB != nil {
		if stats, err := ss.stateDB.LifetimeTradeStatsForStrategy(id); err == nil {
			lifetime = stats
		}
		if closed, _, err := ss.stateDB.QueryClosedPositions(id, "", time.Time{}, time.Time{}, sharpeLookbackLimit, 0); err == nil {
			sharpe = ComputeSharpeRatio(closed, initCap, DefaultAnnualRiskFreeRate)
		}
	}
	winRate := 0.0
	if lifetime.Wins+lifetime.Losses > 0 {
		winRate = float64(lifetime.Wins) / float64(lifetime.Wins+lifetime.Losses) * 100
	}

	return UIStrategyOverview{
		ID:             id,
		Platform:       sc.Platform,
		Symbol:         strategyDisplaySymbol(sc),
		PnLPct:         pnlPct,
		WinRate:        winRate,
		Sharpe:         sharpe,
		Regime:         snapshot.Regime,
		Direction:      strategyDisplayDirection(sc),
		PnL:            pnl,
		PortfolioValue: pv,
		InitialCapital: initCap,
	}, lifetime, true
}

func (ss *StatusServer) handleAPIStrategyStatus(w http.ResponseWriter, r *http.Request, id string) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	overview, lifetime, ok := ss.uiStrategyOverview(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy state not found")
		return
	}

	ss.mu.RLock()
	strat := ss.state.Strategies[id]
	var snapshot StrategyState
	if strat != nil {
		snapshot = *strat
		snapshot.Positions = cloneUIPositions(strat.Positions)
		snapshot.OptionPositions = cloneUIOptionPositions(strat.OptionPositions)
		snapshot.TradeHistory = append([]Trade(nil), strat.TradeHistory...)
		snapshot.RiskState = cloneUIRiskState(strat.RiskState)
	}
	ss.mu.RUnlock()
	if strat == nil {
		writeJSONError(w, http.StatusNotFound, "strategy state not found")
		return
	}

	resp := UIStrategyStatus{
		ID:              overview.ID,
		Type:            sc.Type,
		Platform:        overview.Platform,
		Symbol:          overview.Symbol,
		Timeframe:       strategyDisplayTimeframe(sc),
		Direction:       overview.Direction,
		Cash:            snapshot.Cash,
		InitialCapital:  overview.InitialCapital,
		PortfolioValue:  overview.PortfolioValue,
		PnL:             overview.PnL,
		PnLPct:          overview.PnLPct,
		TradeCount:      len(snapshot.TradeHistory),
		WinRate:         overview.WinRate,
		LifetimeStats:   lifetime,
		Sharpe:          overview.Sharpe,
		Regime:          overview.Regime,
		RiskState:       snapshot.RiskState,
		Positions:       snapshot.Positions,
		OptionPositions: snapshot.OptionPositions,
		Leverage:        EffectiveExchangeLeverage(sc),
		SizingLeverage:  EffectiveSizingLeverage(sc),
		MarginMode:      sc.MarginMode,
	}
	writeJSON(w, resp)
}

func (ss *StatusServer) handleAPIStrategyEquity(w http.ResponseWriter, r *http.Request, id string) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}

	ss.mu.RLock()
	strat := ss.state.Strategies[id]
	var snapshot StrategyState
	if strat != nil {
		snapshot = *strat
		snapshot.Positions = cloneUIPositions(strat.Positions)
		snapshot.OptionPositions = cloneUIOptionPositions(strat.OptionPositions)
	}
	ss.mu.RUnlock()
	if strat == nil {
		writeJSONError(w, http.StatusNotFound, "strategy state not found")
		return
	}

	initCap := EffectiveInitialCapital(sc, &snapshot)
	// Cost-basis terminal point only — avoids N× external mark fetches when
	// loadSparklines polls one equity URL per visible strategy (#813).
	pv := PortfolioValue(&snapshot, map[string]float64{})

	var closed []ClosedPosition
	if ss.stateDB != nil {
		rows, _, err := ss.stateDB.QueryClosedPositions(id, "", time.Time{}, time.Time{}, uiEquityLookbackLimit, 0)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		closed = rows
	}

	limit := parseUIEquityLimit(r)
	points := buildEquityCurvePoints(initCap, closed, pv, limit)
	writeJSON(w, map[string]interface{}{
		"strategy_id": id,
		"points":      points,
	})
}

// buildEquityCurvePoints builds a mini equity curve from initial capital, realized
// PnL at each closed position (ASC), and the current portfolio value.
func buildEquityCurvePoints(initCap float64, closed []ClosedPosition, currentPV float64, limit int) []UIEquityPoint {
	if limit <= 0 {
		limit = 40
	}
	if limit > 500 {
		limit = 500
	}

	sorted := append([]ClosedPosition(nil), closed...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ClosedAt.Equal(sorted[j].ClosedAt) {
			return sorted[i].OpenedAt.Before(sorted[j].OpenedAt)
		}
		return sorted[i].ClosedAt.Before(sorted[j].ClosedAt)
	})

	points := make([]UIEquityPoint, 0, len(sorted)+2)
	equity := initCap

	var startT int64
	if len(sorted) > 0 {
		cp := sorted[0]
		if !cp.OpenedAt.IsZero() {
			startT = cp.OpenedAt.UTC().Unix()
		} else if !cp.ClosedAt.IsZero() {
			startT = cp.ClosedAt.UTC().Unix()
		}
	}
	if startT == 0 {
		startT = time.Now().UTC().Unix()
	}
	points = append(points, UIEquityPoint{T: startT, V: initCap})

	for _, cp := range sorted {
		if cp.ClosedAt.IsZero() {
			continue
		}
		equity += cp.RealizedPnL
		points = append(points, UIEquityPoint{
			T: cp.ClosedAt.UTC().Unix(),
			V: equity,
		})
	}

	now := time.Now().UTC().Unix()
	last := points[len(points)-1]
	if last.V != currentPV || last.T != now {
		points = append(points, UIEquityPoint{T: now, V: currentPV})
	}

	if len(points) > limit {
		points = points[len(points)-limit:]
	}
	return points
}

func parseUIEquityLimit(r *http.Request) int {
	limit := 40
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	return limit
}

func parseUITimeQuery(r *http.Request) (from, to time.Time, limit int) {
	q := r.URL.Query()
	from = parseUITime(q.Get("from"))
	to = parseUITime(q.Get("to"))
	limit = 300
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	return from, to, limit
}

func parseUITime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(ts, 0).UTC()
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func tradeMarkers(trades []Trade) []UITradeMarker {
	out := make([]UITradeMarker, 0, len(trades))
	for i := len(trades) - 1; i >= 0; i-- {
		tr := trades[i]
		if tr.Timestamp.IsZero() {
			continue
		}
		m := UITradeMarker{
			Time:        tr.Timestamp.UTC().Unix(),
			Side:        tr.Side,
			IsClose:     tr.IsClose,
			Price:       tr.Price,
			Quantity:    tr.Quantity,
			RealizedPnL: tr.RealizedPnL,
			Details:     tr.Details,
			Regime:      tr.Regime,
		}
		if tr.IsClose {
			m.Position = "aboveBar"
			m.Color = "#2563eb"
			m.Shape = "circle"
			m.Text = "CLOSE"
		} else if strings.EqualFold(tr.Side, "buy") {
			m.Position = "belowBar"
			m.Color = "#059669"
			m.Shape = "arrowUp"
			m.Text = "BUY"
		} else {
			m.Position = "aboveBar"
			m.Color = "#dc2626"
			m.Shape = "arrowDown"
			m.Text = "SELL"
		}
		out = append(out, m)
	}
	return out
}

// tradeMarkersForTable returns a copy of markers (already oldest-first from tradeMarkers) for the trades JSON key (#808).
func tradeMarkersForTable(markers []UITradeMarker) []UITradeMarker {
	if len(markers) == 0 {
		return []UITradeMarker{}
	}
	out := make([]UITradeMarker, len(markers))
	copy(out, markers)
	return out
}

func tradeCandles(trades []Trade, timeframe string) []UICandle {
	if len(trades) == 0 {
		return []UICandle{}
	}
	bucketSeconds := timeframeSeconds(timeframe)
	byBucket := make(map[int64]*UICandle)
	var keys []int64
	for i := len(trades) - 1; i >= 0; i-- {
		tr := trades[i]
		if tr.Timestamp.IsZero() || tr.Price <= 0 {
			continue
		}
		bucket := tr.Timestamp.UTC().Unix()
		if bucketSeconds > 0 {
			bucket = bucket / bucketSeconds * bucketSeconds
		}
		c, ok := byBucket[bucket]
		if !ok {
			byBucket[bucket] = &UICandle{
				Time:  bucket,
				Open:  tr.Price,
				High:  tr.Price,
				Low:   tr.Price,
				Close: tr.Price,
			}
			keys = append(keys, bucket)
			continue
		}
		c.High = math.Max(c.High, tr.Price)
		c.Low = math.Min(c.Low, tr.Price)
		c.Close = tr.Price
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]UICandle, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byBucket[k])
	}
	return out
}

func timeframeSeconds(tf string) int64 {
	if tf == "" {
		return 0
	}
	unit := tf[len(tf)-1]
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 {
		return 0
	}
	switch unit {
	case 'm':
		return int64(n * 60)
	case 'h':
		return int64(n * 3600)
	case 'd':
		return int64(n * 86400)
	case 'w':
		return int64(n * 604800)
	default:
		return 0
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func cloneUIPositions(in map[string]*Position) map[string]*Position {
	if in == nil {
		return nil
	}
	out := make(map[string]*Position, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		cp := *v
		cp.TPOIDs = append([]int64(nil), v.TPOIDs...)
		cp.TPArmedTiers = append([]bool(nil), v.TPArmedTiers...)
		if v.StopLossATRMult != nil {
			x := *v.StopLossATRMult
			cp.StopLossATRMult = &x
		}
		if v.PostTPTrailingATRMult != nil {
			x := *v.PostTPTrailingATRMult
			cp.PostTPTrailingATRMult = &x
		}
		out[k] = &cp
	}
	return out
}

func cloneUIOptionPositions(in map[string]*OptionPosition) map[string]*OptionPosition {
	if in == nil {
		return nil
	}
	out := make(map[string]*OptionPosition, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		cp := *v
		out[k] = &cp
	}
	return out
}

func cloneUIRiskState(in RiskState) RiskState {
	if in.PendingCircuitCloses == nil {
		return in
	}
	out := in
	out.PendingCircuitCloses = make(map[string]*PendingCircuitClose, len(in.PendingCircuitCloses))
	for k, v := range in.PendingCircuitCloses {
		if v == nil {
			continue
		}
		cp := *v
		cp.Symbols = append([]PendingCircuitCloseSymbol(nil), v.Symbols...)
		out.PendingCircuitCloses[k] = &cp
	}
	return out
}
