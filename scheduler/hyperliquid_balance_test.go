package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
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
			// #254: Multiplier=1 + Leverage=2 so reconcile sees a fully
			// up-to-date perps position and doesn't flip any fields.
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: "hl-btc"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000, Leverage: 2}}

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
				pos := map[string]interface{}{
					"coin":    p.Coin,
					"szi":     fmt.Sprintf("%.6f", p.Size),
					"entryPx": fmt.Sprintf("%.2f", p.EntryPrice),
				}
				if p.Leverage > 0 {
					pos["leverage"] = map[string]interface{}{
						"type":  "cross",
						"value": p.Leverage,
					}
				}
				out = append(out, map[string]interface{}{"position": pos})
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

// #254: reconcile migrates a legacy position stored with Multiplier=0 up to
// Multiplier=1 so PortfolioValue uses the perps PnL branch. It also copies
// the on-chain leverage into the Position.
func TestReconcileMigratesLegacyMultiplierAndSyncsLeverage(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-eth",
		Cash: 27.15,
		Positions: map[string]*Position{
			// Legacy perps position as stored before #254: Multiplier=0.
			"ETH": {Symbol: "ETH", Quantity: 0.279, AvgCost: 2210.71, Side: "long", OwnerStrategyID: "hl-eth"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "ETH", Size: 0.279, EntryPrice: 2210.71, Leverage: 20}}

	changed := reconcileHyperliquidPositions(s, "ETH", positions, logger)

	if !changed {
		t.Fatal("expected changed=true (migration)")
	}
	pos := s.Positions["ETH"]
	if pos.Multiplier != 1 {
		t.Errorf("Multiplier = %v, want 1 after migration", pos.Multiplier)
	}
	if pos.Leverage != 20 {
		t.Errorf("Leverage = %v, want 20 (from on-chain)", pos.Leverage)
	}
	if pos.Quantity != 0.279 || pos.AvgCost != 2210.71 {
		t.Errorf("qty/avgCost changed unexpectedly: %v @ %v", pos.Quantity, pos.AvgCost)
	}
}

// #254: after migration, PortfolioValue reflects margin + PnL, not inflated
// notional. This is the direct regression for the issue.
func TestReconcileLegacyPositionPortfolioValueAfterMigration(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-eth",
		Cash:            27.15,
		OptionPositions: make(map[string]*OptionPosition),
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.279, AvgCost: 2210.71, Side: "long", OwnerStrategyID: "hl-eth"},
		},
	}
	// Pre-fix value (spot branch): 27.15 + 0.279 * 2201.10 = 641.23 — inflated.
	preFix := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	if preFix < 600 || preFix > 700 {
		t.Logf("pre-migration value = %v (spot branch)", preFix)
	}

	logger := newTestLogger(t)
	reconcileHyperliquidPositions(s, "ETH",
		[]HLPosition{{Coin: "ETH", Size: 0.279, EntryPrice: 2210.71, Leverage: 20}}, logger)

	// Post-migration value: cash + qty*(price-entry) = 27.15 + 0.279*(2201.10-2210.71) = ~24.47
	postFix := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	expected := 27.15 + 0.279*(2201.10-2210.71)
	if postFix-expected > 0.01 || expected-postFix > 0.01 {
		t.Errorf("post-migration value = %v, want %v (cash + PnL)", postFix, expected)
	}
	if postFix >= preFix-1 {
		t.Errorf("post-migration value (%v) should be much lower than pre-fix (%v)", postFix, preFix)
	}
}

// #254: parse leverage out of clearinghouseState JSON.
func TestFetchHyperliquidStateParsesLeverage(t *testing.T) {
	body := `{
		"marginSummary": {"accountValue": "1000.0"},
		"assetPositions": [
			{"position": {"coin": "ETH", "szi": "0.5", "entryPx": "2000.0", "leverage": {"type": "cross", "value": 20}}}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	savedURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = savedURL }()

	_, positions, err := fetchHyperliquidState("0xdeadbeef")
	if err != nil {
		t.Fatalf("fetchHyperliquidState: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].Leverage != 20 {
		t.Errorf("Leverage = %v, want 20", positions[0].Leverage)
	}
}

// --- #258: shared-coin reconciliation tests ---

// TestAccountSyncSharedCoinSkipsReconciliation verifies that when two strategies
// trade the same coin on a shared wallet, per-strategy reconciliation is skipped
// and positions are NOT modified to match on-chain.
func TestAccountSyncSharedCoinSkipsReconciliation(t *testing.T) {
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "ETH", Size: 0.315, EntryPrice: 2200},
	})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth-live": {
				ID: "hl-rmc-eth-live", Cash: 27.15,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.460, AvgCost: 2100, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-rmc-eth-live"},
				},
			},
			"hl-tema-eth-live": {
				ID: "hl-tema-eth-live", Cash: 27.79,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.212, AvgCost: 2150, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-tema-eth-live"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
		{ID: "hl-tema-eth-live", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	// Both virtual positions should be unchanged.
	rmcPos := state.Strategies["hl-rmc-eth-live"].Positions["ETH"]
	if rmcPos == nil {
		t.Fatal("hl-rmc-eth-live should still have ETH position")
	}
	if rmcPos.Quantity != 0.460 {
		t.Errorf("rmc ETH quantity = %g, want 0.460 (should not be reconciled)", rmcPos.Quantity)
	}

	temaPos := state.Strategies["hl-tema-eth-live"].Positions["ETH"]
	if temaPos == nil {
		t.Fatal("hl-tema-eth-live should still have ETH position")
	}
	if temaPos.Quantity != 0.212 {
		t.Errorf("tema ETH quantity = %g, want 0.212 (should not be reconciled)", temaPos.Quantity)
	}

	// Cash should not change.
	if state.Strategies["hl-rmc-eth-live"].Cash != 27.15 {
		t.Errorf("rmc cash = %g, want 27.15", state.Strategies["hl-rmc-eth-live"].Cash)
	}
	if state.Strategies["hl-tema-eth-live"].Cash != 27.79 {
		t.Errorf("tema cash = %g, want 27.79", state.Strategies["hl-tema-eth-live"].Cash)
	}

	// Reconciliation gap should be recorded.
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	if gap.OnChainQty != 0.315 {
		t.Errorf("gap OnChainQty = %g, want 0.315", gap.OnChainQty)
	}
	expectedVirtual := 0.460 + 0.212
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g", gap.VirtualQty, expectedVirtual)
	}
	expectedDelta := expectedVirtual - 0.315
	if math.Abs(gap.DeltaQty-expectedDelta) > 0.000001 {
		t.Errorf("gap DeltaQty = %g, want %g", gap.DeltaQty, expectedDelta)
	}
	// Strategies field should list both strategy IDs.
	if len(gap.Strategies) != 2 {
		t.Errorf("gap Strategies = %v, want 2 entries", gap.Strategies)
	}
	if gap.UpdatedAt.IsZero() {
		t.Error("gap UpdatedAt should be set")
	}
}

// TestAccountSyncSharedCoinNotRemovedWhenOnChainGone verifies the phantom
// circuit breaker fix (#258): when one strategy sells the shared position,
// the other strategy's virtual position is NOT removed by sync.
func TestAccountSyncSharedCoinNotRemovedWhenOnChainGone(t *testing.T) {
	// On-chain ETH position is gone (sold by rmc).
	ts := setupHLTestServer(1336, []HLPosition{})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth-live": {
				ID: "hl-rmc-eth-live", Cash: 1336,
				Positions: map[string]*Position{}, // rmc already sold via ExecutePerpsSignal
			},
			"hl-tema-eth-live": {
				ID: "hl-tema-eth-live", Cash: 27.79,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.212, AvgCost: 2150, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-tema-eth-live"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
		{ID: "hl-tema-eth-live", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	// tema's position should NOT be removed (phantom circuit breaker fix).
	temaPos := state.Strategies["hl-tema-eth-live"].Positions["ETH"]
	if temaPos == nil {
		t.Fatal("hl-tema-eth-live should still have ETH position (shared coin — not removed by sync)")
	}
	if temaPos.Quantity != 0.212 {
		t.Errorf("tema ETH quantity = %g, want 0.212", temaPos.Quantity)
	}

	// Reconciliation gap should show the drift.
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	if gap.OnChainQty != 0 {
		t.Errorf("gap OnChainQty = %g, want 0", gap.OnChainQty)
	}
	if gap.VirtualQty != 0.212 {
		t.Errorf("gap VirtualQty = %g, want 0.212", gap.VirtualQty)
	}
	if len(gap.Strategies) != 2 {
		t.Errorf("gap Strategies = %v, want 2 entries", gap.Strategies)
	}
	if gap.UpdatedAt.IsZero() {
		t.Error("gap UpdatedAt should be set")
	}
}

// TestAccountSyncSharedCoinMultiplierMigration verifies that non-destructive
// updates (multiplier migration, leverage sync) still happen for shared coins.
func TestAccountSyncSharedCoinMultiplierMigration(t *testing.T) {
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "ETH", Size: 0.5, EntryPrice: 2000, Leverage: 10},
	})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {
				ID: "hl-a-eth", Cash: 100,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.3, AvgCost: 2000, Side: "long", Multiplier: 0, OwnerStrategyID: "hl-a-eth"},
				},
			},
			"hl-b-eth": {
				ID: "hl-b-eth", Cash: 100,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.2, AvgCost: 2100, Side: "long", Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-b-eth"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	changed := syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)
	if !changed {
		t.Error("expected changed=true (multiplier migration + leverage sync)")
	}

	posA := state.Strategies["hl-a-eth"].Positions["ETH"]
	if posA.Multiplier != 1 {
		t.Errorf("hl-a-eth ETH multiplier = %v, want 1 (migrated)", posA.Multiplier)
	}
	if posA.Leverage != 10 {
		t.Errorf("hl-a-eth ETH leverage = %v, want 10 (from on-chain)", posA.Leverage)
	}

	posB := state.Strategies["hl-b-eth"].Positions["ETH"]
	if posB.Leverage != 10 {
		t.Errorf("hl-b-eth ETH leverage = %v, want 10 (synced from on-chain)", posB.Leverage)
	}

	// Quantities must NOT change.
	if posA.Quantity != 0.3 {
		t.Errorf("hl-a-eth ETH quantity = %g, want 0.3 (unchanged)", posA.Quantity)
	}
	if posB.Quantity != 0.2 {
		t.Errorf("hl-b-eth ETH quantity = %g, want 0.2 (unchanged)", posB.Quantity)
	}
}

// TestAccountSyncMixedSharedAndNonShared verifies that shared and non-shared
// coins are handled independently: BTC (sole owner) is reconciled normally,
// while ETH (shared by 2 strategies) skips reconciliation.
func TestAccountSyncMixedSharedAndNonShared(t *testing.T) {
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "BTC", Size: 0.5, EntryPrice: 42000, Leverage: 5},
		{Coin: "ETH", Size: 0.315, EntryPrice: 2200, Leverage: 20},
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
					"BTC": {Symbol: "BTC", Quantity: 0.3, AvgCost: 40000, Side: "long", Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-btc"},
				},
			},
			"hl-rmc-eth": {
				ID: "hl-rmc-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.46, AvgCost: 2100, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-rmc-eth"},
				},
			},
			"hl-tema-eth": {
				ID: "hl-tema-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.212, AvgCost: 2150, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-tema-eth"},
				},
			},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-rmc-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
		{ID: "hl-tema-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	// BTC should be reconciled (non-shared): 0.3 → 0.5.
	btcPos := state.Strategies["hl-btc"].Positions["BTC"]
	if btcPos == nil {
		t.Fatal("hl-btc should have BTC position")
	}
	if btcPos.Quantity != 0.5 {
		t.Errorf("BTC quantity = %g, want 0.5 (reconciled)", btcPos.Quantity)
	}

	// ETH positions should be unchanged (shared).
	rmcETH := state.Strategies["hl-rmc-eth"].Positions["ETH"]
	if rmcETH == nil || rmcETH.Quantity != 0.46 {
		t.Errorf("rmc ETH = %+v, want quantity 0.46 (not reconciled)", rmcETH)
	}
	temaETH := state.Strategies["hl-tema-eth"].Positions["ETH"]
	if temaETH == nil || temaETH.Quantity != 0.212 {
		t.Errorf("tema ETH = %+v, want quantity 0.212 (not reconciled)", temaETH)
	}

	// Only ETH should have a reconciliation gap.
	if _, ok := state.ReconciliationGaps["BTC"]; ok {
		t.Error("BTC should not have a reconciliation gap (non-shared)")
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("ETH should have a reconciliation gap")
	}
	if gap.OnChainQty != 0.315 {
		t.Errorf("ETH gap OnChainQty = %g, want 0.315", gap.OnChainQty)
	}
	if len(gap.Strategies) != 2 {
		t.Errorf("ETH gap Strategies = %v, want 2 entries", gap.Strategies)
	}
}

// TestAccountSyncSharedCoinGapClearedWhenNoLongerShared verifies that
// reconciliation gaps are cleaned up when a coin is no longer shared.
func TestAccountSyncSharedCoinGapClearedWhenNoLongerShared(t *testing.T) {
	ts := setupHLTestServer(50000, []HLPosition{
		{Coin: "ETH", Size: 0.3, EntryPrice: 2000, Leverage: 10},
	})
	defer ts.Close()

	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 100,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.25, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-eth"},
				},
			},
		},
		// Stale gap from when ETH was shared.
		ReconciliationGaps: map[string]*ReconciliationGap{
			"ETH": {Coin: "ETH", OnChainQty: 0.5, VirtualQty: 0.7, DeltaQty: 0.2, Strategies: []string{"hl-eth", "hl-old"}},
		},
	}

	// Only one strategy trades ETH now (no longer shared).
	strategies := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	// ETH should be reconciled normally (non-shared).
	ethPos := state.Strategies["hl-eth"].Positions["ETH"]
	if ethPos == nil {
		t.Fatal("hl-eth should have ETH position")
	}
	if ethPos.Quantity != 0.3 {
		t.Errorf("ETH quantity = %g, want 0.3 (reconciled to on-chain)", ethPos.Quantity)
	}

	// Stale gap should be cleaned up.
	if _, ok := state.ReconciliationGaps["ETH"]; ok {
		t.Error("ETH reconciliation gap should be removed (no longer shared)")
	}
}

// TestReconcileDueSubsetOfAllDetectsSharedCoins calls reconcileHyperliquidAccountPositions
// directly with dueStrategies as a strict subset of allStrategies. This is the production
// call pattern from main.go where not all strategies are due every cycle.
func TestReconcileDueSubsetOfAllDetectsSharedCoins(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth": {
				ID: "hl-rmc-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2100, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-rmc-eth"},
				},
			},
			"hl-tema-eth": {
				ID: "hl-tema-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.3, AvgCost: 2200, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-tema-eth"},
				},
			},
			"hl-sma-eth": {
				ID: "hl-sma-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.2, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-sma-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-rmc-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
		{ID: "hl-tema-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-sma-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	// Only rmc is due this cycle.
	dueStrategies := allStrategies[:1]

	positions := []HLPosition{
		{Coin: "ETH", Size: 0.4, EntryPrice: 2100, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	reconcileHyperliquidAccountPositions(dueStrategies, allStrategies, state, &mu, logMgr, positions)

	// Even though only rmc is due, allStrategies reveals ETH is shared by 3
	// strategies, so rmc's position must NOT be reconciled to on-chain.
	rmcPos := state.Strategies["hl-rmc-eth"].Positions["ETH"]
	if rmcPos == nil {
		t.Fatal("hl-rmc-eth should still have ETH position")
	}
	if rmcPos.Quantity != 0.5 {
		t.Errorf("rmc ETH quantity = %g, want 0.5 (shared coin, not reconciled)", rmcPos.Quantity)
	}

	// Non-due strategies should also be untouched.
	temaPos := state.Strategies["hl-tema-eth"].Positions["ETH"]
	if temaPos == nil || temaPos.Quantity != 0.3 {
		t.Errorf("tema ETH = %+v, want quantity 0.3 (not due, not reconciled)", temaPos)
	}
	smaPos := state.Strategies["hl-sma-eth"].Positions["ETH"]
	if smaPos == nil || smaPos.Quantity != 0.2 {
		t.Errorf("sma ETH = %+v, want quantity 0.2 (not due, not reconciled)", smaPos)
	}

	// Gap should list all 3 strategies.
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	if len(gap.Strategies) != 3 {
		t.Errorf("gap Strategies = %v, want 3 entries", gap.Strategies)
	}
	expectedVirtual := 0.5 + 0.3 + 0.2
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g", gap.VirtualQty, expectedVirtual)
	}
}

// TestReconcileSharedCoinShortAndMixedPositions verifies the signed virtual qty
// computation for shared coins with short and mixed long/short positions.
func TestReconcileSharedCoinShortAndMixedPositions(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-long-eth": {
				ID: "hl-long-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.8, AvgCost: 2100, Side: "long", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-long-eth"},
				},
			},
			"hl-short-eth": {
				ID: "hl-short-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.3, AvgCost: 2200, Side: "short", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-short-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-long-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-short-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}},
	}

	// On-chain: net long 0.5 (= 0.8 long - 0.3 short).
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5, EntryPrice: 2150, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions)

	// Positions should be unchanged.
	longPos := state.Strategies["hl-long-eth"].Positions["ETH"]
	if longPos == nil || longPos.Quantity != 0.8 || longPos.Side != "long" {
		t.Errorf("long ETH = %+v, want 0.8 long (unchanged)", longPos)
	}
	shortPos := state.Strategies["hl-short-eth"].Positions["ETH"]
	if shortPos == nil || shortPos.Quantity != 0.3 || shortPos.Side != "short" {
		t.Errorf("short ETH = %+v, want 0.3 short (unchanged)", shortPos)
	}

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	// Virtual: +0.8 (long) - 0.3 (short) = 0.5.
	expectedVirtual := 0.5
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g (long 0.8 - short 0.3)", gap.VirtualQty, expectedVirtual)
	}
	// On-chain is also 0.5, so delta should be ~0.
	if math.Abs(gap.DeltaQty) > 0.000001 {
		t.Errorf("gap DeltaQty = %g, want ~0 (virtual matches on-chain)", gap.DeltaQty)
	}
	if gap.OnChainQty != 0.5 {
		t.Errorf("gap OnChainQty = %g, want 0.5", gap.OnChainQty)
	}
}

// TestReconcileSharedCoinBothShort verifies virtual qty computation when both
// strategies are short on a shared coin.
func TestReconcileSharedCoinBothShort(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {
				ID: "hl-a-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 2100, Side: "short", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-a-eth"},
				},
			},
			"hl-b-eth": {
				ID: "hl-b-eth", Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.6, AvgCost: 2200, Side: "short", Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-b-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}},
	}

	// On-chain: short 1.0 (negative size).
	positions := []HLPosition{
		{Coin: "ETH", Size: -1.0, EntryPrice: 2150, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions)

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	// Virtual: -0.4 + -0.6 = -1.0.
	expectedVirtual := -1.0
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g (both short)", gap.VirtualQty, expectedVirtual)
	}
	// On-chain is -1.0, so delta should be ~0.
	if gap.OnChainQty != -1.0 {
		t.Errorf("gap OnChainQty = %g, want -1.0", gap.OnChainQty)
	}
	if math.Abs(gap.DeltaQty) > 0.000001 {
		t.Errorf("gap DeltaQty = %g, want ~0", gap.DeltaQty)
	}
}

// TestReconciliationGapJSONRoundTrip verifies that AppState with ReconciliationGaps
// survives JSON marshal/unmarshal (catches struct tag typos or type mismatches).
func TestReconciliationGapJSONRoundTrip(t *testing.T) {
	original := &AppState{
		CycleCount: 42,
		Strategies: map[string]*StrategyState{},
		ReconciliationGaps: map[string]*ReconciliationGap{
			"ETH": {
				Coin:       "ETH",
				OnChainQty: 0.5,
				VirtualQty: 0.8,
				DeltaQty:   0.3,
				Strategies: []string{"hl-a", "hl-b"},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored AppState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	gap := restored.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("ETH gap missing after round-trip")
	}
	if gap.Coin != "ETH" {
		t.Errorf("Coin = %q, want ETH", gap.Coin)
	}
	if gap.OnChainQty != 0.5 {
		t.Errorf("OnChainQty = %g, want 0.5", gap.OnChainQty)
	}
	if gap.VirtualQty != 0.8 {
		t.Errorf("VirtualQty = %g, want 0.8", gap.VirtualQty)
	}
	if gap.DeltaQty != 0.3 {
		t.Errorf("DeltaQty = %g, want 0.3", gap.DeltaQty)
	}
	if len(gap.Strategies) != 2 {
		t.Errorf("Strategies = %v, want 2 entries", gap.Strategies)
	}
}

// TestReconciliationGapOmittedWhenEmpty verifies that an empty ReconciliationGaps
// map is omitted from JSON (omitempty behavior).
func TestReconciliationGapOmittedWhenEmpty(t *testing.T) {
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["reconciliation_gaps"]; ok {
		t.Error("reconciliation_gaps should be omitted when nil/empty")
	}
}

// --- forceCloseHyperliquidLive tests (#341) ---
//
// These verify the kill-switch live close helper that was missing pre-#341.
// The helper closes on-chain positions directly via the HL SDK's market_close
// (reduce-only by construction), regardless of which strategy "owns" them, so
// shared coins where reconciliation deliberately does not overwrite virtual
// (#258) are still liquidated when the portfolio kill switch fires.

// fakeCloser builds a HyperliquidLiveCloser test double that records every
// invocation and returns either a canned success or an error per coin.
func fakeCloser(errs map[string]error) (HyperliquidLiveCloser, *[]string) {
	var calls []string
	closer := func(symbol string, partialSz *float64) (*HyperliquidCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", symbol, *partialSz))
		} else {
			calls = append(calls, symbol)
		}
		if err, ok := errs[symbol]; ok {
			return nil, err
		}
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform: "hyperliquid",
		}, nil
	}
	return closer, &calls
}

// Non-shared coin: a single live HL strategy for ETH with an on-chain position
// → close is submitted, no errors. Verifies the basic happy path that didn't
// exist before #341 (the kill switch never called any exchange API).
func TestForceCloseHyperliquidLive_NonSharedCoin(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-ema-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.517, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors, got %v", report.Errors)
	}
	if got, want := report.ClosedCoins, []string{"ETH"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("ClosedCoins = %v, want %v", got, want)
	}
	if got, want := *calls, []string{"ETH"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("closer calls = %v, want %v", got, want)
	}
}

// Shared coin with empty virtual state: two strategies both trade ETH on the
// same wallet. Per-strategy reconciliation skips shared coins (#258), so
// virtual state is empty — but on-chain has 0.517 ETH long. The kill switch
// must still close it. Critical regression test for #341 root cause.
func TestForceCloseHyperliquidLive_SharedCoinEmptyVirtual(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-tema-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"rsi_macd_combo", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.517, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors, got %v", report.Errors)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	// Crucially: closer is invoked exactly once for ETH, not per-strategy.
	// The HL SDK's market_close acts on the net on-chain position so a single
	// reduce-only order liquidates the shared exposure.
	if len(*calls) != 1 || (*calls)[0] != "ETH" {
		t.Errorf("expected exactly 1 closer call for ETH, got %v", *calls)
	}
}

// Net-zero szi: when bidirectional strategies on the same wallet hold equal-
// and-opposite virtual positions that net to zero on-chain, kill switch must
// treat the coin as already flat. Submitting a zero-size order would have the
// HL API reject it and would inflate Errors with a meaningless failure that
// keeps the kill switch latched forever.
func TestForceCloseHyperliquidLive_NetZeroSziAlreadyFlat(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-bidir-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema_bidir", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors for net-zero coin, got %v", report.Errors)
	}
	if len(report.ClosedCoins) != 0 {
		t.Errorf("ClosedCoins should be empty for already-flat coin, got %v", report.ClosedCoins)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "ETH" {
		t.Errorf("AlreadyFlat = %v, want [ETH]", report.AlreadyFlat)
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked for zero-szi coin, got calls=%v", *calls)
	}
}

// Short positions are closed identically to longs because the HL SDK's
// market_close infers direction from the current position sign. The Go layer
// only needs to detect non-zero szi and submit one close per coin. This test
// guards the implicit assumption that we don't need separate buy/sell branches
// here — and that overshooting cannot flip the position because market_close
// is reduce-only by SDK construction (reduce_only=True is hard-coded in
// hyperliquid.exchange.Exchange.market_close inside the SDK).
func TestForceCloseHyperliquidLive_ShortPosition(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-bidir-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema_bidir", "ETH", "1h", "--mode=live"}, AllowShorts: true},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: -1.234, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors, got %v", report.Errors)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	if len(*calls) != 1 || (*calls)[0] != "ETH" {
		t.Errorf("closer calls = %v, want [ETH]", *calls)
	}
}

// Close failure: when the SDK call errors (network, exchange downtime, rate
// limit), the coin lands in Errors so the caller keeps the kill switch latched
// and retries next cycle. Without this, virtual state would be cleared while
// on-chain still has exposure and no future cycle could detect the leak (the
// original #341 failure mode, just with the close attempt added).
func TestForceCloseHyperliquidLive_ClosePartialFailure(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5, EntryPrice: 3000},
		{Coin: "BTC", Size: 0.01, EntryPrice: 60000},
	}
	closeErr := fmt.Errorf("hl rate limited")
	closer, _ := fakeCloser(map[string]error{"BTC": closeErr})

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	if got, ok := report.Errors["BTC"]; !ok || got == nil {
		t.Errorf("expected BTC in Errors, got %v", report.Errors)
	}
	if _, ok := report.Errors["ETH"]; ok {
		t.Errorf("ETH should not be in Errors")
	}
}

// Unowned on-chain coin: if some other system has opened a position on this
// wallet for a coin no live HL strategy in our config trades, the kill switch
// must NOT touch it. Liquidating positions we don't own is unsafe — the
// operator may be holding manual hedges. Such positions are surfaced as
// warnings by reconcileHyperliquidAccountPositions, not auto-closed.
func TestForceCloseHyperliquidLive_UnownedPositionIgnored(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5},
		{Coin: "DOGE", Size: 1000}, // not configured — manual / external
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	for _, c := range *calls {
		if c == "DOGE" {
			t.Errorf("closer must not be invoked for unowned coin DOGE")
		}
	}
}

// Empty inputs: with no live HL strategies configured (e.g. an all-spot deploy
// that nonetheless somehow tripped the kill switch), the helper is a clean
// no-op. The caller's onChainConfirmedFlat check then proceeds straight to
// virtual state mutation, matching pre-#341 behavior for non-HL deployments.
func TestForceCloseHyperliquidLive_EmptyInputs(t *testing.T) {
	report := forceCloseHyperliquidLive(context.Background(), nil, nil, func(string, *float64) (*HyperliquidCloseResult, error) {
		t.Fatalf("closer should not be called with empty inputs")
		return nil, nil
	})
	if len(report.ClosedCoins) != 0 || len(report.AlreadyFlat) != 0 || len(report.Errors) != 0 {
		t.Errorf("expected empty report, got %+v", report)
	}
}

// Adapter-side AlreadyFlat: closer returns success with already_flat=true
// (eventual-consistency window — Go-side fetch saw non-zero szi, but by
// the time the SDK submitted, the position was already flat). The coin
// must land in AlreadyFlat, NOT ClosedCoins, so operator messaging
// distinguishes "we sent a close order" from "nothing to close" (#350).
func TestForceCloseHyperliquidLive_AdapterAlreadyFlatRoutedCorrectly(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}

	var calls []string
	closer := func(symbol string, partialSz *float64) (*HyperliquidCloseResult, error) {
		calls = append(calls, symbol)
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors, got %v", report.Errors)
	}
	if len(report.ClosedCoins) != 0 {
		t.Errorf("ClosedCoins should be empty when adapter reports already_flat, got %v", report.ClosedCoins)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "ETH" {
		t.Errorf("AlreadyFlat = %v, want [ETH]", report.AlreadyFlat)
	}
	if len(calls) != 1 || calls[0] != "ETH" {
		t.Errorf("closer should still be called once (Go side saw non-zero szi), got %v", calls)
	}
}

func TestComputeHyperliquidCircuitCloseQty_SoleOwnerFullSzi(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: -0.4, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-eth", pos, hlLive)
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(q-0.4) > 1e-9 {
		t.Errorf("qty=%.6f want 0.4 (full abs szi for sole owner)", q)
	}
}

func TestComputeHyperliquidCircuitCloseQty_Shared50_50(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-a", pos, hlLive)
	if !ok {
		t.Fatal("expected ok")
	}
	want := 0.517 * 0.5
	if math.Abs(q-want) > 1e-9 {
		t.Errorf("qty=%.6f want %.6f", q, want)
	}
}

// Mixed-units weight normalization (#356 review finding 3): when peers on a
// shared coin declare weights in different fields (fractional CapitalPct vs
// absolute Capital), their sum is nonsensical. Detect the mismatch and fall
// back to equal weights so the firing strategy still gets a meaningful share.
func TestComputeHyperliquidCircuitCloseQty_MixedUnitsFallsBackToEqualWeights(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-a", pos, hlLive)
	if !ok {
		t.Fatal("expected ok")
	}
	// With equal 1.0/1.0 fallback, hl-a gets half of |szi| = 0.25. Without the
	// fallback, the old logic would compute 0.5/(0.5+1000) ≈ 0.00025 — a no-op.
	want := 0.25
	if math.Abs(q-want) > 1e-9 {
		t.Errorf("qty=%.6f want %.6f (equal-weight fallback on mixed units)", q, want)
	}
}

// Recovery after HL-fetch-fail at CB fire time (#356 review finding 1).
// When the clearinghouse fetch fails on the cycle a CB first fires, the
// pending close is never enqueued (setHyperliquidCircuitBreakerPending bails
// on nil hlAssist). Subsequent cycles must detect the stuck state (CB active,
// pending nil, live HL perps, on-chain position still open) and reconstruct
// the pending so the reduce-only close eventually fires.
func TestRunPendingHyperliquidCircuitCloses_RecoversStuckCB(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					// CB was fired on a prior cycle, but pending was never set
					// because the HL fetch had failed at that time.
					CircuitBreaker:                 true,
					CircuitBreakerUntil:            time.Now().Add(24 * time.Hour),
					PendingHyperliquidCircuitClose: nil,
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*HyperliquidCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", sym, *partialSz))
		} else {
			calls = append(calls, sym)
		}
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.4, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.4, EntryPrice: 1}},
		true, // hl state already fetched this cycle
		nil,
		closer,
		30*time.Second,
		&mu,
	)
	if len(calls) != 1 || calls[0] != "ETH:0.4" {
		t.Errorf("closer calls=%v want [ETH:0.4] (recovered pending should drain full szi as sole owner)", calls)
	}
	if state.Strategies["hl-a"].RiskState.PendingHyperliquidCircuitClose != nil {
		t.Error("expected pending cleared after successful recovery close")
	}
}

// If the stuck-CB strategy has no on-chain position (e.g. operator already
// closed it manually), recovery must be a no-op rather than submitting a
// zero-size order.
func TestRunPendingHyperliquidCircuitCloses_StuckCBNoOnChainPositionIsNoOp(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: sym}, Platform: "hyperliquid"}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		nil, // no on-chain positions
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
	)
	if len(calls) != 0 {
		t.Errorf("expected no closer calls when no on-chain position, got %v", calls)
	}
	if state.Strategies["hl-a"].RiskState.PendingHyperliquidCircuitClose != nil {
		t.Error("pending should remain nil when recovery has no on-chain position to close")
	}
}

func TestRunPendingHyperliquidCircuitCloses_ClearsOnSuccess(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingHyperliquidCircuitClose: &HyperliquidCircuitClosePending{
						Coins: []HyperliquidCircuitCloseCoin{{Coin: "ETH", Sz: 0.1}},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*HyperliquidCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", sym, *partialSz))
		} else {
			calls = append(calls, sym)
		}
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.1, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 1}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
	)
	if state.Strategies["hl-a"].RiskState.PendingHyperliquidCircuitClose != nil {
		t.Error("expected pending cleared after successful close")
	}
	if len(calls) != 1 || calls[0] != "ETH:0.1" {
		t.Errorf("closer calls=%v want [ETH:0.1]", calls)
	}
}
