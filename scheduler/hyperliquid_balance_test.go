package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// Tests in this file mutate package-level hlMainnetURL and must NOT use t.Parallel().

func TestSyncHyperliquidLiveCapitalSkipsNonHL(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "spot-btc",
		Platform: "binanceus",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change for non-hyperliquid, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalSkipsPaper(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change for paper mode, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalSkipsNoMode(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change without --mode=live, got %g", sc.Capital)
	}
}

func TestSyncHyperliquidLiveCapitalNoAddress(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	// Should fall back to config capital when no address
	if sc.Capital != original {
		t.Errorf("capital should not change without account address, got %g", sc.Capital)
	}
}

// --- fetchHyperliquidState tests ---

func TestFetchHyperliquidState(t *testing.T) {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{
			"accountValue": "50000.00",
		},
		"assetPositions": []map[string]interface{}{
			{
				"position": map[string]string{
					"coin":    "BTC",
					"szi":     "0.334",
					"entryPx": "42000.50",
				},
			},
			{
				"position": map[string]string{
					"coin":    "ETH",
					"szi":     "-2.5",
					"entryPx": "3100.00",
				},
			},
			{
				"position": map[string]string{
					"coin":    "SOL",
					"szi":     "0",
					"entryPx": "150.00",
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()

	balance, positions, err := fetchHyperliquidState("0xabc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if balance != 50000.00 {
		t.Errorf("balance = %g, want 50000", balance)
	}
	// Should have 2 positions (SOL has szi=0, filtered out)
	if len(positions) != 2 {
		t.Fatalf("positions count = %d, want 2", len(positions))
	}
	// BTC long
	if positions[0].Coin != "BTC" || positions[0].Size != 0.334 || positions[0].EntryPrice != 42000.50 {
		t.Errorf("BTC position = %+v", positions[0])
	}
	// ETH short (negative size)
	if positions[1].Coin != "ETH" || positions[1].Size != -2.5 || positions[1].EntryPrice != 3100.00 {
		t.Errorf("ETH position = %+v", positions[1])
	}
}

func TestFetchHyperliquidStateNoPositions(t *testing.T) {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{
			"accountValue": "10000.00",
		},
		"assetPositions": []interface{}{},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()

	balance, positions, err := fetchHyperliquidState("0xabc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if balance != 10000.00 {
		t.Errorf("balance = %g, want 10000", balance)
	}
	if len(positions) != 0 {
		t.Errorf("positions count = %d, want 0", len(positions))
	}
}

// --- reconcileHyperliquidPositions tests ---

func newTestLogger(t *testing.T) *StrategyLogger {
	t.Helper()
	return &StrategyLogger{stratID: "test", writer: os.Stdout}
}

func TestReconcileDiscoverNewPosition(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000}}

	changed := reconcileHyperliquidPositions(s, "BTC", 8000, positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	pos, ok := s.Positions["BTC"]
	if !ok {
		t.Fatal("BTC position should exist in state")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("quantity = %g, want 0.5", pos.Quantity)
	}
	if pos.Side != "long" {
		t.Errorf("side = %s, want long", pos.Side)
	}
	if pos.AvgCost != 40000 {
		t.Errorf("avg_cost = %g, want 40000", pos.AvgCost)
	}
	if s.Cash != 8000 {
		t.Errorf("cash = %g, want 8000", s.Cash)
	}
}

func TestReconcileDiscoverShortPosition(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-eth",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "ETH", Size: -2.0, EntryPrice: 3000}}

	changed := reconcileHyperliquidPositions(s, "ETH", 5000, positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	pos := s.Positions["ETH"]
	if pos == nil {
		t.Fatal("ETH position should exist")
	}
	if pos.Quantity != 2.0 {
		t.Errorf("quantity = %g, want 2.0", pos.Quantity)
	}
	if pos.Side != "short" {
		t.Errorf("side = %s, want short", pos.Side)
	}
}

func TestReconcileUpdateDriftedPosition(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.229, AvgCost: 41000, Side: "long"},
		},
	}
	logger := newTestLogger(t)
	// On-chain shows larger position than state (e.g., manual trade added size)
	positions := []HLPosition{{Coin: "BTC", Size: 0.334, EntryPrice: 42000}}

	changed := reconcileHyperliquidPositions(s, "BTC", 5000, positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	if s.Positions["BTC"].Quantity != 0.334 {
		t.Errorf("quantity = %g, want 0.334", s.Positions["BTC"].Quantity)
	}
	if s.Positions["BTC"].AvgCost != 42000 {
		t.Errorf("avg_cost = %g, want 42000", s.Positions["BTC"].AvgCost)
	}
}

func TestReconcileRemoveClosedPosition(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long"},
		},
	}
	logger := newTestLogger(t)
	// No on-chain position for BTC (closed externally)
	positions := []HLPosition{}

	changed := reconcileHyperliquidPositions(s, "BTC", 10000, positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("BTC position should have been removed")
	}
	if s.Cash != 10000 {
		t.Errorf("cash = %g, want 10000", s.Cash)
	}
}

func TestReconcileNoChange(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long"},
		},
	}
	logger := newTestLogger(t)
	// On-chain matches state exactly
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000}}

	changed := reconcileHyperliquidPositions(s, "BTC", 5000, positions, logger)

	if changed {
		t.Error("expected changed=false when state matches on-chain")
	}
}

func TestReconcileNoPositionBothSides(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	// No on-chain position, no state position
	positions := []HLPosition{}

	changed := reconcileHyperliquidPositions(s, "BTC", 5000, positions, logger)

	if changed {
		t.Error("expected changed=false when no position on either side")
	}
}

func TestSyncHyperliquidPositionsSkipsNoAddress(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	sc := StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	var mu sync.RWMutex
	logger := newTestLogger(t)

	syncHyperliquidPositions(sc, s, &mu, logger)

	// Should be a no-op
	if len(s.Positions) != 0 {
		t.Error("should not add positions without account address")
	}
}

func TestSyncHyperliquidPositionsSkipsNoSymbol(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xabc")
	sc := StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Args:     []string{"sma"}, // no symbol arg
	}
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	var mu sync.RWMutex
	logger := newTestLogger(t)

	syncHyperliquidPositions(sc, s, &mu, logger)

	if len(s.Positions) != 0 {
		t.Error("should not add positions without symbol")
	}
}
