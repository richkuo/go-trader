package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHandleHealth(t *testing.T) {
	state := NewAppState()
	state.LastCycle = time.Now() // recent cycle
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ss.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
	// #682: /health must report the build version so update.sh can verify
	// the post-restart process matches the just-built binary.
	if resp["version"] != Version {
		t.Errorf("version = %q, want %q", resp["version"], Version)
	}
}

func TestHandleHealthStale(t *testing.T) {
	state := NewAppState()
	state.LastCycle = time.Now().Add(-60 * time.Minute) // stale
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ss.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	// Even when stale, the version field should be present so a rolling
	// update can still distinguish old from new during the brief window
	// between restart and the first completed cycle.
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["version"] != Version {
		t.Errorf("version = %q, want %q", resp["version"], Version)
	}
}

func TestHandleHealthZeroTime(t *testing.T) {
	state := NewAppState()
	// LastCycle is zero (never run) — should be healthy
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ss.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (zero time = healthy)", w.Code, http.StatusOK)
	}
}

func TestHandleStatusUnauthorized(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "secret-token", nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	ss.handleStatus(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleStatusUnauthorizedWrongToken(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "secret-token", nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	ss.handleStatus(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleStatusNoAuth(t *testing.T) {
	state := NewAppState()
	state.CycleCount = 5
	state.Strategies["test"] = &StrategyState{
		ID:              "test",
		Type:            "spot",
		Cash:            900,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{{StrategyID: "test"}},
	}
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	ss.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["cycle_count"].(float64) != 5 {
		t.Errorf("cycle_count = %v, want 5", resp["cycle_count"])
	}

	strategies := resp["strategies"].(map[string]interface{})
	testStrat := strategies["test"].(map[string]interface{})
	if testStrat["id"] != "test" {
		t.Errorf("strategy id = %v, want %q", testStrat["id"], "test")
	}
	if testStrat["cash"].(float64) != 900 {
		t.Errorf("cash = %v, want 900", testStrat["cash"])
	}
	if testStrat["trade_count"].(float64) != 1 {
		t.Errorf("trade_count = %v, want 1", testStrat["trade_count"])
	}
}

func TestHandleStatusWithBearerToken(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "my-token", nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer my-token")
	w := httptest.NewRecorder()
	ss.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestNewStatusServerExtractsSymbols(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
		{Type: "spot", Args: []string{"rsi", "ETH/USDT", "1h"}},
		{Type: "options", Args: []string{"vol", "BTC"}}, // options skipped
		// #263: HL perps must populate hlPerpsCoins (venue-native mark),
		// NOT priceSymbols (BinanceUS spot). The old #245 "/USDT" normalisation
		// and priceMirror path have been removed — perps are now sourced from
		// the exchange they live on.
		{Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "SOL", "1h"}},
		{Type: "perps", Platform: "okx", Args: []string{"ema", "BTC", "1h"}},
	}
	state := NewAppState()
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", strategies, nil)

	// Spot symbols must be in priceSymbols.
	symbolSet := make(map[string]bool)
	for _, s := range ss.priceSymbols {
		symbolSet[s] = true
	}
	if !symbolSet["BTC/USDT"] {
		t.Error("BTC/USDT should be in priceSymbols")
	}
	if !symbolSet["ETH/USDT"] {
		t.Error("ETH/USDT should be in priceSymbols")
	}
	// Perps must NOT be in priceSymbols — they live in hlPerpsCoins/okxPerpsCoins.
	if symbolSet["SOL/USDT"] {
		t.Error("SOL/USDT must not be in priceSymbols (HL perps now venue-native — #263)")
	}
	if len(ss.priceSymbols) != 2 {
		t.Errorf("priceSymbols len = %d, want 2 (spot only)", len(ss.priceSymbols))
	}

	// HL perps coin must appear in hlPerpsCoins.
	hlSet := make(map[string]bool)
	for _, c := range ss.hlPerpsCoins {
		hlSet[c] = true
	}
	if !hlSet["SOL"] {
		t.Errorf("hlPerpsCoins missing SOL; got %v", ss.hlPerpsCoins)
	}

	// OKX perps coin must appear in okxPerpsCoins.
	okxSet := make(map[string]bool)
	for _, c := range ss.okxPerpsCoins {
		okxSet[c] = true
	}
	if !okxSet["BTC"] {
		t.Errorf("okxPerpsCoins missing BTC; got %v", ss.okxPerpsCoins)
	}
}

func TestHandleHistory_NilDB(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/history", nil)
	w := httptest.NewRecorder()
	ss.handleHistory(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleHistory_Unauthorized(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "secret", nil, nil)

	req := httptest.NewRequest("GET", "/history", nil)
	w := httptest.NewRecorder()
	ss.handleHistory(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleHistory_NoAuth(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var mu sync.RWMutex
	ss := NewStatusServer(NewAppState(), &mu, "", nil, db)

	req := httptest.NewRequest("GET", "/history", nil)
	w := httptest.NewRecorder()
	ss.handleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Trades []Trade `json:"trades"`
		Total  int     `json:"total"`
		Limit  int     `json:"limit"`
		Offset int     `json:"offset"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.Limit != 50 {
		t.Errorf("limit = %d, want 50", resp.Limit)
	}
}

func TestHandleHistory_QueryParams(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var mu sync.RWMutex
	ss := NewStatusServer(NewAppState(), &mu, "", nil, db)

	// Filter by strategy.
	req := httptest.NewRequest("GET", "/history?strategy=hl-momentum-btc&limit=1", nil)
	w := httptest.NewRecorder()
	ss.handleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Trades []Trade `json:"trades"`
		Total  int     `json:"total"`
		Limit  int     `json:"limit"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if len(resp.Trades) != 1 {
		t.Errorf("trades len = %d, want 1 (limited)", len(resp.Trades))
	}
	if resp.Limit != 1 {
		t.Errorf("limit = %d, want 1", resp.Limit)
	}
}

func TestHandleAPIStrategies(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex
	strategies := []StrategyConfig{
		{ID: "okx-eth", Platform: "okx", Type: "perps", Args: []string{"ema", "ETH", "4h"}, Direction: DirectionBoth},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
	}
	ss := NewStatusServer(state, &mu, "", strategies, nil)

	req := httptest.NewRequest("GET", "/api/strategies", nil)
	w := httptest.NewRecorder()
	ss.handleAPIStrategies(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		Strategies []UIStrategy `json:"strategies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Strategies) != 2 {
		t.Fatalf("strategies len = %d, want 2", len(resp.Strategies))
	}
	if resp.Strategies[0].ID != "spot-btc" || resp.Strategies[0].Symbol != "BTC/USDT" || resp.Strategies[0].Timeframe != "1h" {
		t.Errorf("first strategy = %+v, want spot-btc BTC/USDT 1h", resp.Strategies[0])
	}
	if resp.Strategies[0].Direction != "" {
		t.Errorf("spot direction = %q, want empty", resp.Strategies[0].Direction)
	}
	if resp.Strategies[1].Direction != DirectionBoth {
		t.Errorf("direction = %q, want %q", resp.Strategies[1].Direction, DirectionBoth)
	}
}

func TestHandleAPIStrategiesOverview(t *testing.T) {
	state := NewAppState()
	state.Strategies["spot-btc"] = &StrategyState{
		ID:              "spot-btc",
		Type:            "spot",
		Cash:            1100,
		InitialCapital:  1000,
		Regime:          "trending",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	state.Strategies["okx-eth"] = &StrategyState{
		ID:              "okx-eth",
		Type:            "perps",
		Cash:            800,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	var mu sync.RWMutex
	strategies := []StrategyConfig{
		{ID: "okx-eth", Platform: "okx", Type: "perps", Args: []string{"ema", "ETH", "4h"}, Direction: DirectionBoth},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
	}
	ss := NewStatusServer(state, &mu, "", strategies, nil)

	req := httptest.NewRequest("GET", "/api/strategies/overview", nil)
	w := httptest.NewRecorder()
	ss.handleAPIStrategiesOverview(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		Strategies []UIStrategyOverview `json:"strategies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Strategies) != 2 {
		t.Fatalf("strategies len = %d, want 2", len(resp.Strategies))
	}
	byID := make(map[string]UIStrategyOverview, len(resp.Strategies))
	for _, row := range resp.Strategies {
		byID[row.ID] = row
	}
	spot := byID["spot-btc"]
	if spot.Platform != "binanceus" || spot.Symbol != "BTC/USDT" {
		t.Errorf("spot-btc row = %+v, want binanceus BTC/USDT", spot)
	}
	if spot.PnL != 100 || spot.PnLPct != 10 {
		t.Errorf("spot-btc pnl = %v/%v, want 100/10", spot.PnL, spot.PnLPct)
	}
	if spot.Regime != "trending" {
		t.Errorf("spot-btc regime = %q, want trending", spot.Regime)
	}
	if spot.Direction != "" {
		t.Errorf("spot-btc direction = %q, want empty", spot.Direction)
	}
	okx := byID["okx-eth"]
	if okx.Direction != DirectionBoth {
		t.Errorf("okx-eth direction = %q, want %q", okx.Direction, DirectionBoth)
	}
	if okx.PnL != -200 || okx.PnLPct != -20 {
		t.Errorf("okx-eth pnl = %v/%v, want -200/-20", okx.PnL, okx.PnLPct)
	}
}

func TestHandleAPIStrategyCandles_UsesFetcherAndCache(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
	}, nil)
	calls := 0
	ss.candleFetcher = func(req UICandleRequest) ([]UICandle, string, error) {
		calls++
		return []UICandle{{Time: 100, Open: 1, High: 2, Low: 1, Close: 2}}, "test", nil
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/api/strategies/spot-btc/candles?limit=10", nil)
		w := httptest.NewRecorder()
		ss.handleAPIStrategy(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, w.Code, http.StatusOK)
		}
		var resp struct {
			Source  string     `json:"source"`
			Candles []UICandle `json:"candles"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Candles) != 1 {
			t.Fatalf("candles len = %d, want 1", len(resp.Candles))
		}
	}
	if calls != 1 {
		t.Errorf("fetcher calls = %d, want 1 due to cache", calls)
	}
}

func TestHandleAPIStrategyTradesMarkers(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if err := db.InsertTrade("spot-btc", Trade{
		Timestamp: now, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 100, Regime: "trending",
	}); err != nil {
		t.Fatalf("InsertTrade open: %v", err)
	}
	if err := db.InsertTrade("spot-btc", Trade{
		Timestamp: now.Add(time.Hour), Symbol: "BTC/USDT", Side: "sell", Quantity: 1, Price: 110,
		IsClose: true, RealizedPnL: 10, Regime: "ranging",
	}); err != nil {
		t.Fatalf("InsertTrade close: %v", err)
	}

	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
	}, db)

	req := httptest.NewRequest("GET", "/api/strategies/spot-btc/trades", nil)
	w := httptest.NewRecorder()
	ss.handleAPIStrategy(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		Markers []UITradeMarker `json:"markers"`
		Trades  []UITradeMarker `json:"trades"`
		Total   int             `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || len(resp.Markers) != 2 || len(resp.Trades) != 2 {
		t.Fatalf("total/markers/trades = %d/%d/%d, want 2/2/2", resp.Total, len(resp.Markers), len(resp.Trades))
	}
	if resp.Markers[0].Text != "BUY" || resp.Markers[1].Text != "CLOSE" {
		t.Errorf("marker texts = %q/%q, want BUY/CLOSE", resp.Markers[0].Text, resp.Markers[1].Text)
	}
	if resp.Trades[0].Text != "BUY" || resp.Trades[1].Text != "CLOSE" {
		t.Errorf("trade texts = %q/%q, want BUY/CLOSE", resp.Trades[0].Text, resp.Trades[1].Text)
	}
	if resp.Markers[0].Regime != "trending" || resp.Markers[1].Regime != "ranging" {
		t.Errorf("marker regimes = %q/%q, want trending/ranging", resp.Markers[0].Regime, resp.Markers[1].Regime)
	}
	if resp.Trades[0].Regime != "trending" || resp.Trades[1].Regime != "ranging" {
		t.Errorf("trade regimes = %q/%q, want trending/ranging", resp.Trades[0].Regime, resp.Trades[1].Regime)
	}
}

func TestBuildEquityCurvePoints(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	t2 := t0.Add(48 * time.Hour)
	closed := []ClosedPosition{
		{OpenedAt: t0, ClosedAt: t1, RealizedPnL: 50},
		{OpenedAt: t1, ClosedAt: t2, RealizedPnL: -20},
	}
	points := buildEquityCurvePoints(1000, closed, 1030, 10)
	if len(points) != 4 {
		t.Fatalf("len = %d, want 4 (start + 2 closes + current)", len(points))
	}
	if points[0].T != t0.Unix() || points[0].V != 1000 {
		t.Errorf("start = %+v, want t=%d v=1000", points[0], t0.Unix())
	}
	if points[1].T != t1.Unix() || points[1].V != 1050 {
		t.Errorf("after first close = %+v, want v=1050", points[1])
	}
	if points[2].T != t2.Unix() || points[2].V != 1030 {
		t.Errorf("after second close = %+v, want v=1030", points[2])
	}
	if points[3].V != 1030 {
		t.Errorf("final value = %v, want 1030", points[3].V)
	}

	trimmed := buildEquityCurvePoints(1000, closed, 1030, 2)
	if len(trimmed) != 2 {
		t.Fatalf("trimmed len = %d, want 2", len(trimmed))
	}
	if trimmed[0].V != 1030 || trimmed[1].V != 1030 {
		t.Errorf("trimmed keeps most recent points, got %+v", trimmed)
	}
}

func TestHandleAPIStrategyEquity(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"spot-btc": {
				ID:             "spot-btc",
				Type:           "spot",
				Cash:           1010,
				InitialCapital: 1000,
				Positions:      map[string]*Position{},
				ClosedPositions: []ClosedPosition{
					{
						StrategyID: "spot-btc", Symbol: "BTC/USDT", Quantity: 1, AvgCost: 100,
						Side: "long", OpenedAt: now.Add(-2 * time.Hour), ClosedAt: now.Add(-time.Hour),
						ClosePrice: 110, RealizedPnL: 10, CloseReason: "signal",
					},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 1000, Args: []string{"sma", "BTC/USDT", "1h"}},
	}, db)

	req := httptest.NewRequest("GET", "/api/strategies/spot-btc/equity?limit=40", nil)
	w := httptest.NewRecorder()
	ss.handleAPIStrategy(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		StrategyID string          `json:"strategy_id"`
		Points     []UIEquityPoint `json:"points"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StrategyID != "spot-btc" {
		t.Errorf("strategy_id = %q, want spot-btc", resp.StrategyID)
	}
	if len(resp.Points) < 2 {
		t.Fatalf("points len = %d, want at least 2", len(resp.Points))
	}
	if resp.Points[0].V != 1000 {
		t.Errorf("first point value = %v, want 1000", resp.Points[0].V)
	}
	foundClose := false
	for _, p := range resp.Points {
		if p.T == now.Add(-time.Hour).Unix() && p.V == 1010 {
			foundClose = true
		}
	}
	if !foundClose {
		t.Errorf("missing close point at %+v", resp.Points)
	}
}

func TestHandleAPIReturnsDraining(t *testing.T) {
	shutdownDraining.Store(false)
	beginDrain()
	defer shutdownDraining.Store(false)

	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", nil, nil)

	req := httptest.NewRequest("GET", "/api/strategies", nil)
	w := httptest.NewRecorder()
	ss.handleAPIStrategies(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// Regression: SIGHUP holds the global state mu.Lock() across the reload (see
// reloadConfig in main.go), and applyHotReloadConfig calls
// server.UpdateStrategies while still holding it. A previous version of
// UpdateStrategies took the same non-reentrant mutex and deadlocked the
// daemon on every reload. Exercise the path with a real *sync.RWMutex held
// by the caller — a deadlocked implementation hangs here until the timeout.
func TestUpdateStrategiesDoesNotDeadlockUnderStateLock(t *testing.T) {
	state := NewAppState()
	var mu sync.RWMutex
	ss := NewStatusServer(state, &mu, "", nil, nil)

	mu.Lock()
	defer mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ss.UpdateStrategies([]StrategyConfig{
			{ID: "alpha", Platform: "binanceus"},
			{ID: "beta", Platform: "hyperliquid"},
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateStrategies blocked while caller holds state mu.Lock() — SIGHUP deadlock regression")
	}

	got := ss.uiStrategies()
	if len(got) != 2 {
		t.Fatalf("uiStrategies len = %d, want 2 (%+v)", len(got), got)
	}
}
