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

	ss := NewStatusServer(state, &mu, "", nil)

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
}

func TestHandleHealthStale(t *testing.T) {
	state := NewAppState()
	state.LastCycle = time.Now().Add(-60 * time.Minute) // stale
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ss.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleHealthZeroTime(t *testing.T) {
	state := NewAppState()
	// LastCycle is zero (never run) — should be healthy
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", nil)

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

	ss := NewStatusServer(state, &mu, "secret-token", nil)

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

	ss := NewStatusServer(state, &mu, "secret-token", nil)

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

	ss := NewStatusServer(state, &mu, "", nil)

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

	ss := NewStatusServer(state, &mu, "my-token", nil)

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
		{Type: "options", Args: []string{"vol", "BTC"}}, // not spot, skipped
	}
	state := NewAppState()
	var mu sync.RWMutex

	ss := NewStatusServer(state, &mu, "", strategies)

	// Check that priceSymbols were extracted
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
	if len(ss.priceSymbols) != 2 {
		t.Errorf("priceSymbols len = %d, want 2", len(ss.priceSymbols))
	}
}
