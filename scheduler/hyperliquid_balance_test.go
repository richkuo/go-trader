package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// Tests in this file mutate package-level hlMainnetURL and must NOT use t.Parallel().

func TestSyncHyperliquidLiveCapitalIsNoOp(t *testing.T) {
	sc := &StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Capital:  1000,
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	original := sc.Capital
	syncHyperliquidLiveCapital(sc)
	if sc.Capital != original {
		t.Errorf("capital should not change (no-op), got %g", sc.Capital)
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

func TestReconcileUpdatesExistingOwnedPosition(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.229, AvgCost: 41000, Side: "long", OwnerStrategyID: "hl-btc"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.334, EntryPrice: 42000}}

	changed := reconcileHyperliquidPositions(s, "BTC", positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	if s.Positions["BTC"].Quantity != 0.334 {
		t.Errorf("quantity = %g, want 0.334", s.Positions["BTC"].Quantity)
	}
	if s.Positions["BTC"].AvgCost != 42000 {
		t.Errorf("avg_cost = %g, want 42000", s.Positions["BTC"].AvgCost)
	}
	// Cash should NOT be synced from on-chain.
	if s.Cash != 5000 {
		t.Errorf("cash = %g, want 5000 (should not change)", s.Cash)
	}
}

func TestReconcileRemoveClosedPosition(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long", OwnerStrategyID: "hl-btc"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{} // No on-chain position

	changed := reconcileHyperliquidPositions(s, "BTC", positions, logger)

	if !changed {
		t.Error("expected changed=true")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("BTC position should have been removed")
	}
	// Cash should not change.
	if s.Cash != 5000 {
		t.Errorf("cash = %g, want 5000", s.Cash)
	}
}

func TestReconcileNoChange(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long", OwnerStrategyID: "hl-btc"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000}}

	changed := reconcileHyperliquidPositions(s, "BTC", positions, logger)

	if changed {
		t.Error("expected changed=false when state matches on-chain")
	}
}

func TestReconcileSkipsUnownedOnChainPosition(t *testing.T) {
	// Strategy has no position in state; on-chain position exists.
	// The new behavior should NOT add it (unlike the old behavior).
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000}}

	changed := reconcileHyperliquidPositions(s, "BTC", positions, logger)

	if changed {
		t.Error("expected changed=false — should not adopt unowned position")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("BTC position should NOT be added to a strategy that doesn't own it")
	}
}

func TestReconcileNoPositionBothSides(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	positions := []HLPosition{}

	changed := reconcileHyperliquidPositions(s, "BTC", positions, logger)

	if changed {
		t.Error("expected changed=false when no position on either side")
	}
}

// --- syncHyperliquidAccountPositions tests ---

func setupHLTestServer(balance float64, positions []HLPosition) *httptest.Server {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{
			"accountValue": fmt.Sprintf("%.2f", balance),
		},
		"assetPositions": func() []interface{} {
			var out []interface{}
			for _, p := range positions {
				out = append(out, map[string]interface{}{
					"position": map[string]string{
						"coin":    p.Coin,
						"szi":     fmt.Sprintf("%.6f", p.Size),
						"entryPx": fmt.Sprintf("%.2f", p.EntryPrice),
					},
				})
			}
			return out
		}(),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestAccountSyncTwoStrategiesDifferentCoins(t *testing.T) {
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "BTC", Size: 0.5, EntryPrice: 40000},
		{Coin: "ETH", Size: 2.0, EntryPrice: 3000},
	})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-momentum-btc": {
				ID: "hl-momentum-btc", Cash: 10000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.3, AvgCost: 39000, Side: "long", OwnerStrategyID: "hl-momentum-btc"},
				},
			},
			"hl-amd-eth": {
				ID: "hl-amd-eth", Cash: 8000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.5, AvgCost: 2800, Side: "long", OwnerStrategyID: "hl-amd-eth"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-momentum-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"momentum", "BTC", "1h", "--mode=live"}},
		{ID: "hl-amd-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"amd", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	changed := syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)
	if !changed {
		t.Error("expected changed=true (quantities differ)")
	}

	// BTC should be reconciled to on-chain values, owned by hl-momentum-btc.
	btcPos := state.Strategies["hl-momentum-btc"].Positions["BTC"]
	if btcPos == nil {
		t.Fatal("hl-momentum-btc should have BTC position")
	}
	if btcPos.Quantity != 0.5 {
		t.Errorf("BTC quantity = %g, want 0.5", btcPos.Quantity)
	}
	if btcPos.OwnerStrategyID != "hl-momentum-btc" {
		t.Errorf("BTC owner = %s, want hl-momentum-btc", btcPos.OwnerStrategyID)
	}

	// ETH should be reconciled, owned by hl-amd-eth.
	ethPos := state.Strategies["hl-amd-eth"].Positions["ETH"]
	if ethPos == nil {
		t.Fatal("hl-amd-eth should have ETH position")
	}
	if ethPos.Quantity != 2.0 {
		t.Errorf("ETH quantity = %g, want 2.0", ethPos.Quantity)
	}

	// Neither strategy should have the OTHER coin's position.
	if _, ok := state.Strategies["hl-momentum-btc"].Positions["ETH"]; ok {
		t.Error("hl-momentum-btc should NOT have ETH position")
	}
	if _, ok := state.Strategies["hl-amd-eth"].Positions["BTC"]; ok {
		t.Error("hl-amd-eth should NOT have BTC position")
	}

	// Cash should NOT be synced from on-chain.
	if state.Strategies["hl-momentum-btc"].Cash != 10000 {
		t.Errorf("hl-momentum-btc cash = %g, want 10000", state.Strategies["hl-momentum-btc"].Cash)
	}
	if state.Strategies["hl-amd-eth"].Cash != 8000 {
		t.Errorf("hl-amd-eth cash = %g, want 8000", state.Strategies["hl-amd-eth"].Cash)
	}
}

func TestAccountSyncUnownedPositionNotAssigned(t *testing.T) {
	// On-chain has SOL position, but no strategy trades SOL.
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "BTC", Size: 0.5, EntryPrice: 40000},
		{Coin: "SOL", Size: 10.0, EntryPrice: 150},
	})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {
				ID: "hl-btc", Cash: 10000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long", OwnerStrategyID: "hl-btc"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	// SOL should NOT appear in any strategy.
	for id, ss := range state.Strategies {
		if _, ok := ss.Positions["SOL"]; ok {
			t.Errorf("strategy %s should NOT have SOL position", id)
		}
	}
}

func TestAccountSyncSkipsNoAddress(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 5000, Positions: make(map[string]*Position)},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	changed := syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)
	if changed {
		t.Error("should return false without account address")
	}
}

func TestValidateStateMigratesOwnership(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {
				ID: "hl-btc", Cash: 5000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long"},
				},
			},
		},
	}

	ValidateState(state)

	pos := state.Strategies["hl-btc"].Positions["BTC"]
	if pos.OwnerStrategyID != "hl-btc" {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, "hl-btc")
	}
}
