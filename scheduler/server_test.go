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
		Timestamp: now, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 100,
	}); err != nil {
		t.Fatalf("InsertTrade open: %v", err)
	}
	if err := db.InsertTrade("spot-btc", Trade{
		Timestamp: now.Add(time.Hour), Symbol: "BTC/USDT", Side: "sell", Quantity: 1, Price: 110, IsClose: true, RealizedPnL: 10,
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
		Total   int             `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || len(resp.Markers) != 2 {
		t.Fatalf("total/markers = %d/%d, want 2/2", resp.Total, len(resp.Markers))
	}
	if resp.Markers[0].Text != "BUY" || resp.Markers[1].Text != "CLOSE" {
		t.Errorf("marker texts = %q/%q, want BUY/CLOSE", resp.Markers[0].Text, resp.Markers[1].Text)
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
