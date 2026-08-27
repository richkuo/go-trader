package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	if len(positions) != 2 {
		t.Fatalf("positions count = %d, want 2", len(positions))
	}
	if positions[0].Coin != "BTC" || positions[0].Size != 0.334 || positions[0].EntryPrice != 42000.50 {
		t.Errorf("BTC position = %+v", positions[0])
	}
	if positions[1].Coin != "ETH" || positions[1].Size != -2.5 || positions[1].EntryPrice != 3100.00 {
		t.Errorf("ETH position = %+v", positions[1])
	}
}

func TestFetchHyperliquidStateLiquidationPx(t *testing.T) {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{
			"accountValue": "50000.00",
		},
		"assetPositions": []map[string]interface{}{
			{
				"position": map[string]interface{}{
					"coin":          "ETH",
					"szi":           "2.5",
					"entryPx":       "2400.00",
					"liquidationPx": "2340.5",
				},
			},
			{
				"position": map[string]interface{}{
					"coin":    "BTC",
					"szi":     "0.5",
					"entryPx": "42000.00",
				},
			},
			{
				"position": map[string]interface{}{
					"coin":          "SOL",
					"szi":           "-10",
					"entryPx":       "150.00",
					"liquidationPx": nil,
				},
			},
			{
				"position": map[string]interface{}{
					"coin":          "DOGE",
					"szi":           "1000",
					"entryPx":       "0.35",
					"liquidationPx": "abc",
				},
			},
			{
				"position": map[string]interface{}{
					"coin":          "AVAX",
					"szi":           "5",
					"entryPx":       "30.00",
					"liquidationPx": "0",
				},
			},
			{
				"position": map[string]interface{}{
					"coin":          "LINK",
					"szi":           "50",
					"entryPx":       "20.00",
					"liquidationPx": 17.25,
				},
			},
			{
				"position": map[string]interface{}{
					"coin":          "ARB",
					"szi":           "200",
					"entryPx":       "1.50",
					"liquidationPx": "1.2",
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

	_, positions, err := fetchHyperliquidState("0xabc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 7 {
		t.Fatalf("positions count = %d, want 7", len(positions))
	}
	want := map[string]float64{
		"ETH":  2340.5,
		"BTC":  0,
		"SOL":  0,
		"DOGE": 0,
		"AVAX": 0,
		"LINK": 17.25,
		"ARB":  1.2,
	}
	for _, p := range positions {
		w, ok := want[p.Coin]
		if !ok {
			t.Fatalf("unexpected coin %s", p.Coin)
		}
		if p.LiquidationPx != w {
			t.Errorf("%s LiquidationPx = %g, want %g", p.Coin, p.LiquidationPx, w)
		}
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

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if !changed {
		t.Error("expected changed=true")
	}
	if s.Positions["BTC"].Quantity != 0.334 {
		t.Errorf("quantity = %g, want 0.334", s.Positions["BTC"].Quantity)
	}
	if s.Positions["BTC"].AvgCost != 42000 {
		t.Errorf("avg_cost = %g, want 42000", s.Positions["BTC"].AvgCost)
	}
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
	positions := []HLPosition{}

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if !changed {
		t.Error("expected changed=true")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("BTC position should have been removed")
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].RealizedPnL != 0 || !s.TradeHistory[0].PnLGross {
		t.Fatalf("want one zero-gross-PnL trade row, got %+v", s.TradeHistory)
	}
	if got, want := s.Cash, 5000-s.TradeHistory[0].ExchangeFee; math.Abs(got-want) > 1e-9 {
		t.Errorf("cash = %g, want %g (modeled fee only)", got, want)
	}
}

func TestReconcileNoChange(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-btc",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: "hl-btc"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000, Leverage: 2}}

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if changed {
		t.Error("expected changed=false when state matches on-chain")
	}
}

func TestReconcileSkipsUnownedOnChainPosition(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-btc",
		Cash:      5000,
		Positions: make(map[string]*Position),
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 40000}}

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if changed {
		t.Error("expected changed=false — should not adopt unowned position")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("BTC position should NOT be added to a strategy that doesn't own it")
	}
}

func TestReconcilePreservesConfiguredLeverage(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-eth",
		Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long",
				Multiplier: 1, Leverage: 2, OwnerStrategyID: "hl-eth"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "ETH", Size: 1, EntryPrice: 3000, Leverage: 20}}

	reconcileHyperliquidPositionsWithResolver(s, "ETH", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if s.Positions["ETH"].Leverage != 2 {
		t.Errorf("Leverage = %v; want 2 (configured value must be preserved against on-chain 20)", s.Positions["ETH"].Leverage)
	}
}

func TestReconcileSeedsZeroLeverageFromOnChain(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-eth",
		Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long",
				Multiplier: 1, OwnerStrategyID: "hl-eth"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "ETH", Size: 1, EntryPrice: 3000, Leverage: 10}}

	reconcileHyperliquidPositionsWithResolver(s, "ETH", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if s.Positions["ETH"].Leverage != 10 {
		t.Errorf("Leverage = %v; want 10 (zero-value position seeded from on-chain)", s.Positions["ETH"].Leverage)
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

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	if changed {
		t.Error("expected changed=false when no position on either side")
	}
}

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

	ethPos := state.Strategies["hl-amd-eth"].Positions["ETH"]
	if ethPos == nil {
		t.Fatal("hl-amd-eth should have ETH position")
	}
	if ethPos.Quantity != 2.0 {
		t.Errorf("ETH quantity = %g, want 2.0", ethPos.Quantity)
	}

	if _, ok := state.Strategies["hl-momentum-btc"].Positions["ETH"]; ok {
		t.Error("hl-momentum-btc should NOT have ETH position")
	}
	if _, ok := state.Strategies["hl-amd-eth"].Positions["BTC"]; ok {
		t.Error("hl-amd-eth should NOT have BTC position")
	}

	if state.Strategies["hl-momentum-btc"].Cash != 10000 {
		t.Errorf("hl-momentum-btc cash = %g, want 10000", state.Strategies["hl-momentum-btc"].Cash)
	}
	if state.Strategies["hl-amd-eth"].Cash != 8000 {
		t.Errorf("hl-amd-eth cash = %g, want 8000", state.Strategies["hl-amd-eth"].Cash)
	}
}

func TestAccountSyncUnownedPositionNotAssigned(t *testing.T) {
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

	ValidateState(state, nil)

	pos := state.Strategies["hl-btc"].Positions["BTC"]
	if pos.OwnerStrategyID != "hl-btc" {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, "hl-btc")
	}
}

func TestReconcileMigratesLegacyMultiplierAndSyncsLeverage(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-eth",
		Cash: 27.15,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.279, AvgCost: 2210.71, Side: "long", OwnerStrategyID: "hl-eth"},
		},
	}
	logger := newTestLogger(t)
	positions := []HLPosition{{Coin: "ETH", Size: 0.279, EntryPrice: 2210.71, Leverage: 20}}

	changed := reconcileHyperliquidPositionsWithResolver(s, "ETH", positions, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

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

func TestReconcileLegacyPositionPortfolioValueAfterMigration(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-eth",
		Cash:            27.15,
		OptionPositions: make(map[string]*OptionPosition),
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.279, AvgCost: 2210.71, Side: "long", OwnerStrategyID: "hl-eth"},
		},
	}
	preFix := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	if preFix < 600 || preFix > 700 {
		t.Logf("pre-migration value = %v (spot branch)", preFix)
	}

	logger := newTestLogger(t)
	reconcileHyperliquidPositionsWithResolver(s, "ETH",
		[]HLPosition{{Coin: "ETH", Size: 0.279, EntryPrice: 2210.71, Leverage: 20}}, noFillFeeResolver, logger, nil, nil, StrategyConfig{})

	postFix := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	expected := 27.15 + 0.279*(2201.10-2210.71)
	if postFix-expected > 0.01 || expected-postFix > 0.01 {
		t.Errorf("post-migration value = %v, want %v (cash + PnL)", postFix, expected)
	}
	if postFix >= preFix-1 {
		t.Errorf("post-migration value (%v) should be much lower than pre-fix (%v)", postFix, preFix)
	}
}

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

	if state.Strategies["hl-rmc-eth-live"].Cash != 27.15 {
		t.Errorf("rmc cash = %g, want 27.15", state.Strategies["hl-rmc-eth-live"].Cash)
	}
	if state.Strategies["hl-tema-eth-live"].Cash != 27.79 {
		t.Errorf("tema cash = %g, want 27.79", state.Strategies["hl-tema-eth-live"].Cash)
	}

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
	if len(gap.Strategies) != 2 {
		t.Errorf("gap Strategies = %v, want 2 entries", gap.Strategies)
	}
	if gap.UpdatedAt.IsZero() {
		t.Error("gap UpdatedAt should be set")
	}
}

func TestAccountSyncSharedCoinClosedWhenOnChainGone(t *testing.T) {
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
				Positions: map[string]*Position{},
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

	temaPos := state.Strategies["hl-tema-eth-live"].Positions["ETH"]
	if temaPos != nil {
		t.Errorf("hl-tema-eth-live ETH position should be nil after external close reconcile, got %+v", temaPos)
	}
	if len(state.Strategies["hl-tema-eth-live"].ClosedPositions) != 1 {
		t.Errorf("tema ClosedPositions = %d, want 1", len(state.Strategies["hl-tema-eth-live"].ClosedPositions))
	} else if state.Strategies["hl-tema-eth-live"].ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Errorf("CloseReason = %q, want hl_sync_external", state.Strategies["hl-tema-eth-live"].ClosedPositions[0].CloseReason)
	}

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap entry for ETH")
	}
	if gap.OnChainQty != 0 {
		t.Errorf("gap OnChainQty = %g, want 0", gap.OnChainQty)
	}
	if math.Abs(gap.VirtualQty) > 1e-6 {
		t.Errorf("gap VirtualQty = %g, want ~0 after close", gap.VirtualQty)
	}
	if math.Abs(gap.DeltaQty) > 1e-6 {
		t.Errorf("gap DeltaQty = %g, want ~0 after close", gap.DeltaQty)
	}
	if gap.UpdatedAt.IsZero() {
		t.Error("gap UpdatedAt should be set")
	}
}

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
		t.Error("expected changed=true (multiplier migration + zero-leverage init)")
	}

	posA := state.Strategies["hl-a-eth"].Positions["ETH"]
	if posA.Multiplier != 1 {
		t.Errorf("hl-a-eth ETH multiplier = %v, want 1 (migrated)", posA.Multiplier)
	}
	if posA.Leverage != 10 {
		t.Errorf("hl-a-eth ETH leverage = %v, want 10 (zero-value init from on-chain)", posA.Leverage)
	}

	posB := state.Strategies["hl-b-eth"].Positions["ETH"]
	if posB.Leverage != 5 {
		t.Errorf("hl-b-eth ETH leverage = %v, want 5 (configured leverage preserved; on-chain overwrite blocked by #418 RC3 write-path guard)", posB.Leverage)
	}

	if posA.Quantity != 0.3 {
		t.Errorf("hl-a-eth ETH quantity = %g, want 0.3 (unchanged)", posA.Quantity)
	}
	if posB.Quantity != 0.2 {
		t.Errorf("hl-b-eth ETH quantity = %g, want 0.2 (unchanged)", posB.Quantity)
	}
}

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

	btcPos := state.Strategies["hl-btc"].Positions["BTC"]
	if btcPos == nil {
		t.Fatal("hl-btc should have BTC position")
	}
	if btcPos.Quantity != 0.5 {
		t.Errorf("BTC quantity = %g, want 0.5 (reconciled)", btcPos.Quantity)
	}

	rmcETH := state.Strategies["hl-rmc-eth"].Positions["ETH"]
	if rmcETH == nil || rmcETH.Quantity != 0.46 {
		t.Errorf("rmc ETH = %+v, want quantity 0.46 (not reconciled)", rmcETH)
	}
	temaETH := state.Strategies["hl-tema-eth"].Positions["ETH"]
	if temaETH == nil || temaETH.Quantity != 0.212 {
		t.Errorf("tema ETH = %+v, want quantity 0.212 (not reconciled)", temaETH)
	}

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
		ReconciliationGaps: map[string]*ReconciliationGap{
			"ETH": {Coin: "ETH", OnChainQty: 0.5, VirtualQty: 0.7, DeltaQty: 0.2, Strategies: []string{"hl-eth", "hl-old"}},
		},
	}

	strategies := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	syncHyperliquidAccountPositions(strategies, state, &mu, logMgr)

	ethPos := state.Strategies["hl-eth"].Positions["ETH"]
	if ethPos == nil {
		t.Fatal("hl-eth should have ETH position")
	}
	if ethPos.Quantity != 0.3 {
		t.Errorf("ETH quantity = %g, want 0.3 (reconciled to on-chain)", ethPos.Quantity)
	}

	if _, ok := state.ReconciliationGaps["ETH"]; ok {
		t.Error("ETH reconciliation gap should be removed (no longer shared)")
	}
}

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
	dueStrategies := allStrategies[:1]

	positions := []HLPosition{
		{Coin: "ETH", Size: 0.4, EntryPrice: 2100, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	_, _, _ = reconcileHyperliquidAccountPositions(dueStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

	rmcPos := state.Strategies["hl-rmc-eth"].Positions["ETH"]
	if rmcPos == nil {
		t.Fatal("hl-rmc-eth should still have ETH position")
	}
	if rmcPos.Quantity != 0.5 {
		t.Errorf("rmc ETH quantity = %g, want 0.5 (shared coin, not reconciled)", rmcPos.Quantity)
	}

	temaPos := state.Strategies["hl-tema-eth"].Positions["ETH"]
	if temaPos == nil || temaPos.Quantity != 0.3 {
		t.Errorf("tema ETH = %+v, want quantity 0.3 (not due, not reconciled)", temaPos)
	}
	smaPos := state.Strategies["hl-sma-eth"].Positions["ETH"]
	if smaPos == nil || smaPos.Quantity != 0.2 {
		t.Errorf("sma ETH = %+v, want quantity 0.2 (not due, not reconciled)", smaPos)
	}

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

	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5, EntryPrice: 2150, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

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
	expectedVirtual := 0.5
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g (long 0.8 - short 0.3)", gap.VirtualQty, expectedVirtual)
	}
	if math.Abs(gap.DeltaQty) > 0.000001 {
		t.Errorf("gap DeltaQty = %g, want ~0 (virtual matches on-chain)", gap.DeltaQty)
	}
	if gap.OnChainQty != 0.5 {
		t.Errorf("gap OnChainQty = %g, want 0.5", gap.OnChainQty)
	}
}

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

	positions := []HLPosition{
		{Coin: "ETH", Size: -1.0, EntryPrice: 2150, Leverage: 20},
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected reconciliation gap for ETH")
	}
	expectedVirtual := -1.0
	if math.Abs(gap.VirtualQty-expectedVirtual) > 0.000001 {
		t.Errorf("gap VirtualQty = %g, want %g (both short)", gap.VirtualQty, expectedVirtual)
	}
	if gap.OnChainQty != -1.0 {
		t.Errorf("gap OnChainQty = %g, want -1.0", gap.OnChainQty)
	}
	if math.Abs(gap.DeltaQty) > 0.000001 {
		t.Errorf("gap DeltaQty = %g, want ~0", gap.DeltaQty)
	}
}

func TestReconcileSharedCoin_OwnerStopLossFired_ClosesOwnerOnly(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 42, StopLossTriggerPx: 2900},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Leverage: 10}}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 42 {
			return HLFillLookup{Fee: 0.05, FilledQty: 1.0, Px: 2900, Count: 1, OID: 42}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "0xtest", nil, false)

	if state.Strategies["hl-owner-eth"].Positions["ETH"] != nil {
		t.Error("owner ETH position should be nil after SL reconciliation")
	}
	if len(state.Strategies["hl-owner-eth"].ClosedPositions) != 1 {
		t.Errorf("owner ClosedPositions = %d, want 1", len(state.Strategies["hl-owner-eth"].ClosedPositions))
	} else {
		cp := state.Strategies["hl-owner-eth"].ClosedPositions[0]
		if cp.CloseReason != "hl_sync_stop_loss" {
			t.Errorf("CloseReason = %q, want hl_sync_stop_loss", cp.CloseReason)
		}
		if math.Abs(cp.ClosePrice-2900) > 0.01 {
			t.Errorf("ClosePrice = %g, want 2900", cp.ClosePrice)
		}
	}

	peerPos := state.Strategies["hl-peer-eth"].Positions["ETH"]
	if peerPos == nil || math.Abs(peerPos.Quantity-0.5) > 1e-6 {
		t.Errorf("peer ETH = %+v, want 0.5 long (unchanged)", peerPos)
	}

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.DeltaQty) > 1e-6 {
		t.Errorf("gap DeltaQty = %g after SL reconcile, want ~0", gap.DeltaQty)
	}
}

func TestReconcileSharedCoin_MultipleStopLossOwnersConfirmed_ClosesOwners(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {
				ID: "hl-a-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-a-eth",
						StopLossOID: 42, StopLossTriggerPx: 2900},
				},
			},
			"hl-b-eth": {
				ID: "hl-b-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.25, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-b-eth",
						StopLossOID: 43, StopLossTriggerPx: 2910},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"a", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"b", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"peer", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Leverage: 10}}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, _ float64) (HLFillLookup, bool) {
		switch oid {
		case 42:
			return HLFillLookup{Fee: 0.05, FilledQty: 1.0, Px: 2900, Count: 1, OID: 42}, true
		case 43:
			return HLFillLookup{Fee: 0.02, FilledQty: 0.25, Px: 2910, Count: 1, OID: 43}, true
		default:
			return HLFillLookup{}, false
		}
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "0xtest", nil, false)

	for _, id := range []string{"hl-a-eth", "hl-b-eth"} {
		if state.Strategies[id].Positions["ETH"] != nil {
			t.Fatalf("%s position should be closed", id)
		}
		if len(state.Strategies[id].ClosedPositions) != 1 || state.Strategies[id].ClosedPositions[0].CloseReason != "hl_sync_stop_loss" {
			t.Fatalf("%s closed positions = %+v, want one SL close", id, state.Strategies[id].ClosedPositions)
		}
	}
	peerPos := state.Strategies["hl-peer-eth"].Positions["ETH"]
	if peerPos == nil || math.Abs(peerPos.Quantity-0.5) > 1e-9 {
		t.Fatalf("peer position = %+v, want unchanged 0.5", peerPos)
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil || math.Abs(gap.DeltaQty) > 1e-9 {
		t.Fatalf("gap = %+v, want reconciled delta 0", gap)
	}
}

func TestReconcileSharedCoin_MultipleStopLossOwnersUnconfirmed_LeavesGap(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {
				ID: "hl-a-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-a-eth",
						StopLossOID: 42, StopLossTriggerPx: 2900},
				},
			},
			"hl-b-eth": {
				ID: "hl-b-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.25, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-b-eth",
						StopLossOID: 43, StopLossTriggerPx: 2910},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"a", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"b", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"peer", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Leverage: 10}}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, _ int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "0xtest", nil, false)

	for _, id := range []string{"hl-a-eth", "hl-b-eth", "hl-peer-eth"} {
		if state.Strategies[id].Positions["ETH"] == nil {
			t.Fatalf("%s position should remain open", id)
		}
		if len(state.Strategies[id].ClosedPositions) != 0 {
			t.Fatalf("%s closed positions = %+v, want none", id, state.Strategies[id].ClosedPositions)
		}
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil || math.Abs(gap.DeltaQty-1.25) > 1e-9 {
		t.Fatalf("gap = %+v, want unresolved delta 1.25", gap)
	}
}

func TestReconcileSharedCoin_OwnerStopLossFired_Short(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.8, AvgCost: 3000, Side: "short",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 99, StopLossTriggerPx: 3100},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.3, AvgCost: 3000, Side: "short",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: -0.3, EntryPrice: 3000, Leverage: 10}}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 99 {
			return HLFillLookup{Fee: 0.04, FilledQty: 0.8, Px: 3100, Count: 1, OID: 99}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "0xtest", nil, false)

	if state.Strategies["hl-owner-eth"].Positions["ETH"] != nil {
		t.Error("owner short ETH position should be nil after SL reconciliation")
	}
	peerPos := state.Strategies["hl-peer-eth"].Positions["ETH"]
	if peerPos == nil || math.Abs(peerPos.Quantity-0.3) > 1e-6 || peerPos.Side != "short" {
		t.Errorf("peer ETH = %+v, want 0.3 short (unchanged)", peerPos)
	}
}

func TestReconcileSharedCoin_AllPositionsClosedExternally(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 7, StopLossTriggerPx: 2800},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 7 {
			return HLFillLookup{Fee: 0.02, FilledQty: 1.0, Px: 2800, Count: 1, OID: 7}, true
		}
		_ = qty
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "0xtest", nil, false)

	if state.Strategies["hl-owner-eth"].Positions["ETH"] != nil {
		t.Error("owner ETH position should be nil")
	}
	if len(state.Strategies["hl-owner-eth"].ClosedPositions) != 1 {
		t.Errorf("owner ClosedPositions = %d, want 1", len(state.Strategies["hl-owner-eth"].ClosedPositions))
	} else if state.Strategies["hl-owner-eth"].ClosedPositions[0].CloseReason != "hl_sync_stop_loss" {
		t.Errorf("owner CloseReason = %q, want hl_sync_stop_loss", state.Strategies["hl-owner-eth"].ClosedPositions[0].CloseReason)
	}

	if state.Strategies["hl-peer-eth"].Positions["ETH"] != nil {
		t.Error("peer ETH position should be nil")
	}
	if len(state.Strategies["hl-peer-eth"].ClosedPositions) != 1 {
		t.Errorf("peer ClosedPositions = %d, want 1", len(state.Strategies["hl-peer-eth"].ClosedPositions))
	} else if state.Strategies["hl-peer-eth"].ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Errorf("peer CloseReason = %q, want hl_sync_external", state.Strategies["hl-peer-eth"].ClosedPositions[0].CloseReason)
	}
}

func TestReconcileSharedCoin_AllPositionsClosedExternally_CreditsPeerCash(t *testing.T) {
	const peerStartCash = 500.0
	const peerQty = 0.5
	const peerAvgCost = 3000.0
	const mark = 3200.0

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 7, StopLossTriggerPx: 2800},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: peerStartCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: peerQty, AvgCost: peerAvgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{}
	prices := map[string]float64{"ETH": mark}
	wantPeerFee := peerQty * mark * HyperliquidTakerFeePct

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 7 && coin == "ETH" {
			return HLFillLookup{Fee: 0.02, FilledQty: 1.0, Px: 2800, Count: 1, OID: 7}, true
		}
		if oid == 0 && coin == "ETH" && math.Abs(qty-peerQty) < 1e-9 {
			return HLFillLookup{Fee: wantPeerFee, Count: 1}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "0xtest", nil, false)

	peer := state.Strategies["hl-peer-eth"]
	if peer.Positions["ETH"] != nil {
		t.Error("peer ETH position should be nil after external close")
	}
	if len(peer.ClosedPositions) != 1 {
		t.Fatalf("peer ClosedPositions = %d, want 1", len(peer.ClosedPositions))
	}
	cp := peer.ClosedPositions[0]
	if cp.CloseReason != "hl_sync_external" {
		t.Errorf("CloseReason = %q, want hl_sync_external", cp.CloseReason)
	}
	if cp.ClosePrice != mark {
		t.Errorf("ClosePrice = %v, want %v", cp.ClosePrice, mark)
	}
	wantFee := peerQty * mark * HyperliquidTakerFeePct
	wantPnL := peerQty*(mark-peerAvgCost) - wantFee
	wantCash := peerStartCash + wantPnL
	if math.Abs(cp.RealizedPnL-wantPnL) > 1e-6 {
		t.Errorf("RealizedPnL = %v, want %v", cp.RealizedPnL, wantPnL)
	}
	if math.Abs(peer.Cash-wantCash) > 1e-6 {
		t.Errorf("peer Cash = %v, want %v (started %v + PnL %v)", peer.Cash, wantCash, peerStartCash, wantPnL)
	}
	var closeTrades []Trade
	for _, tr := range peer.TradeHistory {
		if tr.IsClose {
			closeTrades = append(closeTrades, tr)
		}
	}
	if len(closeTrades) != 1 {
		t.Fatalf("peer close trades = %d, want 1 (history=%+v)", len(closeTrades), peer.TradeHistory)
	}
	ct := closeTrades[0]
	if !ct.PnLGross || math.Abs(tradeNetPnL(ct)-wantPnL) > 1e-6 {
		t.Errorf("trade net PnL = %v (gross=%v), want %v", tradeNetPnL(ct), ct.PnLGross, wantPnL)
	}
	if ct.Price != mark {
		t.Errorf("trade Price = %v, want %v", ct.Price, mark)
	}
	if ct.Side != "sell" {
		t.Errorf("trade Side = %q, want %q (long close)", ct.Side, "sell")
	}
	if ct.TradeType != "perps" {
		t.Errorf("trade TradeType = %q, want perps", ct.TradeType)
	}
	owner := state.Strategies["hl-owner-eth"]
	if owner.Positions["ETH"] != nil {
		t.Error("owner ETH position should be nil")
	}
	if len(owner.ClosedPositions) != 1 || owner.ClosedPositions[0].CloseReason != "hl_sync_stop_loss" {
		t.Errorf("owner ClosedPositions wrong: %+v", owner.ClosedPositions)
	}
}

func TestReconcileSharedCoin_Detector1SplitsAggregateFillAcrossPeers(t *testing.T) {
	const (
		fillPx    = 3200.0
		aggFee    = 4.0
		aggOID    = int64(98765)
		ownerQty  = 1.5
		peerQty   = 0.5
		avgCost   = 3000.0
		ownerCash = 1000.0
		peerCash  = 500.0
	)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: ownerCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: ownerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth"},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: peerCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: peerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 0 && coin == "ETH" && math.Abs(qty-(ownerQty+peerQty)) < 1e-9 {
			return HLFillLookup{
				Fee:            aggFee,
				ClosedPnLGross: (ownerQty + peerQty) * (fillPx - avgCost),
				FilledQty:      ownerQty + peerQty,
				Px:             fillPx,
				Count:          1,
				OID:            aggOID,
			}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, nil, map[string]float64{"ETH": 3100}, "0xtest", nil, false)

	assertClose := func(id string, startCash, qty, wantFee float64) {
		t.Helper()
		ss := state.Strategies[id]
		if ss.Positions["ETH"] != nil {
			t.Fatalf("%s ETH position should be nil", id)
		}
		if len(ss.ClosedPositions) != 1 {
			t.Fatalf("%s ClosedPositions = %d, want 1", id, len(ss.ClosedPositions))
		}
		cp := ss.ClosedPositions[0]
		if cp.CloseReason != "hl_sync_external" {
			t.Errorf("%s CloseReason = %q, want hl_sync_external", id, cp.CloseReason)
		}
		if cp.ClosePrice != fillPx {
			t.Errorf("%s ClosePrice = %v, want fill %v", id, cp.ClosePrice, fillPx)
		}
		wantNetPnL := qty*(fillPx-avgCost) - wantFee
		if math.Abs(cp.RealizedPnL-wantNetPnL) > 1e-9 {
			t.Errorf("%s ClosedPosition net PnL = %v, want %v", id, cp.RealizedPnL, wantNetPnL)
		}
		if math.Abs(ss.Cash-(startCash+wantNetPnL)) > 1e-9 {
			t.Errorf("%s Cash = %v, want %v", id, ss.Cash, startCash+wantNetPnL)
		}
		var closeTrades []Trade
		for _, tr := range ss.TradeHistory {
			if tr.IsClose {
				closeTrades = append(closeTrades, tr)
			}
		}
		if len(closeTrades) != 1 {
			t.Fatalf("%s close trades = %d, want 1 (history=%+v)", id, len(closeTrades), ss.TradeHistory)
		}
		tr := closeTrades[0]
		if tr.ExchangeOrderID != "98765" {
			t.Errorf("%s ExchangeOrderID = %q, want 98765", id, tr.ExchangeOrderID)
		}
		if tr.FeeSource != FeeSourceUserFills {
			t.Errorf("%s FeeSource = %q, want %q", id, tr.FeeSource, FeeSourceUserFills)
		}
		if math.Abs(tr.ExchangeFee-wantFee) > 1e-9 {
			t.Errorf("%s ExchangeFee = %v, want %v", id, tr.ExchangeFee, wantFee)
		}
		if tr.Price != fillPx {
			t.Errorf("%s trade Price = %v, want %v", id, tr.Price, fillPx)
		}
		if math.Abs(tr.RealizedPnL-qty*(fillPx-avgCost)) > 1e-9 {
			t.Errorf("%s trade gross PnL = %v, want %v", id, tr.RealizedPnL, qty*(fillPx-avgCost))
		}
		if math.Abs(tradeNetPnL(tr)-wantNetPnL) > 1e-9 {
			t.Errorf("%s trade net PnL = %v, want %v", id, tradeNetPnL(tr), wantNetPnL)
		}
	}

	assertClose("hl-owner-eth", ownerCash, ownerQty, aggFee*(ownerQty/(ownerQty+peerQty)))
	assertClose("hl-peer-eth", peerCash, peerQty, aggFee*(peerQty/(ownerQty+peerQty)))
}

func TestReconcileSharedCoin_Detector1BidirectionalAggregateSplitWinsOverPeerQtyMatch(t *testing.T) {
	const (
		fillPx    = 3200.0
		aggFee    = 6.0
		aggOID    = int64(87654)
		longQty   = 1.0
		shortQty  = 0.5
		avgCost   = 3000.0
		longCash  = 1000.0
		shortCash = 600.0
	)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-long-eth": {
				ID: "hl-long-eth", Cash: longCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: longQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-long-eth"},
				},
			},
			"hl-short-eth": {
				ID: "hl-short-eth", Cash: shortCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: shortQty, AvgCost: avgCost, Side: "short",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-short-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-long-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"longer", "ETH", "1h", "--mode=live"}},
		{ID: "hl-short-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"shorter", "ETH", "1h", "--mode=live"}},
	}

	aggregateQty := math.Abs(longQty - shortQty)
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 0 && coin == "ETH" && math.Abs(qty-aggregateQty) < 1e-9 {
			return HLFillLookup{
				Fee:            aggFee,
				ClosedPnLGross: aggregateQty * (fillPx - avgCost),
				FilledQty:      aggregateQty,
				Px:             fillPx,
				Count:          1,
				OID:            aggOID,
			}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, nil, map[string]float64{"ETH": 3100}, "0xtest", nil, false)

	denom := longQty + shortQty
	assertClose := func(id, wantTradeSide string, startCash, qty, wantGrossPnL, wantFee float64) {
		t.Helper()
		ss := state.Strategies[id]
		if ss.Positions["ETH"] != nil {
			t.Fatalf("%s ETH position should be nil", id)
		}
		if len(ss.ClosedPositions) != 1 {
			t.Fatalf("%s ClosedPositions = %d, want 1", id, len(ss.ClosedPositions))
		}
		wantNetPnL := wantGrossPnL - wantFee
		if math.Abs(ss.ClosedPositions[0].RealizedPnL-wantNetPnL) > 1e-9 {
			t.Errorf("%s ClosedPosition net PnL = %v, want %v", id, ss.ClosedPositions[0].RealizedPnL, wantNetPnL)
		}
		if math.Abs(ss.Cash-(startCash+wantNetPnL)) > 1e-9 {
			t.Errorf("%s Cash = %v, want %v", id, ss.Cash, startCash+wantNetPnL)
		}
		if len(ss.TradeHistory) != 1 {
			t.Fatalf("%s TradeHistory = %d, want 1 (%+v)", id, len(ss.TradeHistory), ss.TradeHistory)
		}
		tr := ss.TradeHistory[0]
		if tr.ExchangeOrderID != "87654" {
			t.Errorf("%s ExchangeOrderID = %q, want 87654", id, tr.ExchangeOrderID)
		}
		if tr.Side != wantTradeSide {
			t.Errorf("%s Side = %q, want %q", id, tr.Side, wantTradeSide)
		}
		if math.Abs(tr.ExchangeFee-wantFee) > 1e-9 {
			t.Errorf("%s ExchangeFee = %v, want %v", id, tr.ExchangeFee, wantFee)
		}
		if tr.Price != fillPx {
			t.Errorf("%s Price = %v, want %v", id, tr.Price, fillPx)
		}
		if math.Abs(tr.RealizedPnL-wantGrossPnL) > 1e-9 {
			t.Errorf("%s gross PnL = %v, want %v", id, tr.RealizedPnL, wantGrossPnL)
		}
	}

	longFee := aggFee * (longQty / denom)
	shortFee := aggFee * (shortQty / denom)
	assertClose("hl-long-eth", "sell", longCash, longQty, longQty*(fillPx-avgCost), longFee)
	assertClose("hl-short-eth", "buy", shortCash, shortQty, shortQty*(avgCost-fillPx), shortFee)

	totalFee := state.Strategies["hl-long-eth"].TradeHistory[0].ExchangeFee + state.Strategies["hl-short-eth"].TradeHistory[0].ExchangeFee
	if math.Abs(totalFee-aggFee) > 1e-9 {
		t.Errorf("total split fee = %v, want aggregate fee %v", totalFee, aggFee)
	}
}

func TestHlReconcileExternalClosePx(t *testing.T) {
	const mark = 63564.5
	const fillPx = 63597.0
	lookup := HLFillLookup{Px: fillPx, Fee: 1.2, Count: 1}

	if got := hlReconcileExternalClosePx(mark, lookup, true); got != fillPx {
		t.Errorf("with fill Px = %v, got %v", fillPx, got)
	}
	if got := hlReconcileExternalClosePx(mark, HLFillLookup{Fee: 1.2, Count: 1}, true); got != mark {
		t.Errorf("missing Px should fall back to mark %v, got %v", mark, got)
	}
	if got := hlReconcileExternalClosePx(mark, lookup, false); got != mark {
		t.Errorf("useFillFee=false should fall back to mark %v, got %v", mark, got)
	}
}

func TestReconcileSharedCoin_ExternalCloseUsesFillPriceWhenAvailable(t *testing.T) {
	const peerStartCash = 500.0
	const peerQty = 0.5
	const peerAvgCost = 63000.0
	const mark = 63564.5
	const fillPx = 63597.0

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-btc": {
				ID: "hl-owner-btc", Cash: 10000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 1.0, AvgCost: 63000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-btc",
						StopLossOID: 7, StopLossTriggerPx: 62000},
				},
			},
			"hl-peer-btc": {
				ID: "hl-peer-btc", Cash: peerStartCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: peerQty, AvgCost: peerAvgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-btc"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "BTC", "1h", "--mode=live"}},
		{ID: "hl-peer-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{}
	prices := map[string]float64{"BTC": mark}
	wantPeerFee := 0.42

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 7 && coin == "BTC" {
			return HLFillLookup{Fee: 0.02, FilledQty: 1.0, Px: 62000, Count: 1, OID: 7}, true
		}
		if oid == 0 && coin == "BTC" && math.Abs(qty-peerQty) < 1e-9 {
			return HLFillLookup{Fee: wantPeerFee, Px: fillPx, Count: 1, OID: 4242}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "0xtest", nil, false)

	peer := state.Strategies["hl-peer-btc"]
	if len(peer.ClosedPositions) != 1 {
		t.Fatalf("peer ClosedPositions = %d, want 1", len(peer.ClosedPositions))
	}
	cp := peer.ClosedPositions[0]
	if cp.ClosePrice != fillPx {
		t.Errorf("ClosePrice = %v, want fill %v (not mark %v)", cp.ClosePrice, fillPx, mark)
	}
	wantPnL := peerQty*(fillPx-peerAvgCost) - wantPeerFee
	if math.Abs(cp.RealizedPnL-wantPnL) > 1e-6 {
		t.Errorf("RealizedPnL = %v, want %v", cp.RealizedPnL, wantPnL)
	}
	if math.Abs(peer.Cash-(peerStartCash+wantPnL)) > 1e-6 {
		t.Errorf("peer Cash = %v, want %v", peer.Cash, peerStartCash+wantPnL)
	}
}

func TestReconcileSharedCoin_Detector1_ExternalFallbackUsesFillPrice(t *testing.T) {
	const mark = 61000.0
	const fillPx = 60800.0
	const ownerQty = 0.2
	const ownerAvgCost = 60000.0
	const wantFee = 0.85

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-btc": {
				ID: "hl-owner-btc", Cash: 10000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: ownerQty, AvgCost: ownerAvgCost, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-owner-btc",
						StopLossOID: 5005, StopLossTriggerPx: 58000},
				},
			},
			"hl-peer-btc": {
				ID: "hl-peer-btc", Cash: 10000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60500, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-peer-btc"},
				},
			},
		},
	}
	scs := []StrategyConfig{
		{ID: "hl-owner-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
		{ID: "hl-peer-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
	}
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 5005 {
			return HLFillLookup{Fee: 1.0, FilledQty: ownerQty, Count: 1, OID: 9999}, true
		}
		if oid == 0 && coin == "BTC" && math.Abs(qty-ownerQty) < 1e-9 {
			return HLFillLookup{Fee: wantFee, Px: fillPx, Count: 1, OID: 8888}, true
		}
		return HLFillLookup{}, false
	}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	prices := map[string]float64{"BTC": mark}
	_, _, _ = reconcileHyperliquidAccountPositions(scs, scs, state, &mu, logMgr, nil, prices, "0xtest", nil, false)

	owner := state.Strategies["hl-owner-btc"]
	if len(owner.ClosedPositions) != 1 {
		t.Fatalf("owner ClosedPositions = %d, want 1", len(owner.ClosedPositions))
	}
	cp := owner.ClosedPositions[0]
	if cp.ClosePrice != fillPx {
		t.Errorf("owner ClosePrice = %v, want fill %v (not mark %v)", cp.ClosePrice, fillPx, mark)
	}
	wantPnL := ownerQty*(fillPx-ownerAvgCost) - wantFee
	if math.Abs(cp.RealizedPnL-wantPnL) > 1e-6 {
		t.Errorf("owner RealizedPnL = %v, want %v", cp.RealizedPnL, wantPnL)
	}
}

func TestReconcileSharedCoin_Detector2_UnconfirmedFillLeavesGap(t *testing.T) {
	const mark = 3020.0
	const fillPx = 3010.0
	const ownerQty = 1.0
	const ownerAvgCost = 3000.0
	const wantFee = 1.05

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: ownerQty, AvgCost: ownerAvgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 4242, StopLossTriggerPx: 2900},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	scs := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Leverage: 10}}
	prices := map[string]float64{"ETH": mark}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 4242 {
			return HLFillLookup{Fee: 0.1, FilledQty: ownerQty, Count: 1, OID: 9999}, true
		}
		if oid == 0 && coin == "ETH" && math.Abs(qty-ownerQty) < 1e-9 {
			return HLFillLookup{Fee: wantFee, Px: fillPx, Count: 1, OID: 7777}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(scs, scs, state, &mu, logMgr, positions, prices, "0xtest", nil, false)

	owner := state.Strategies["hl-owner-eth"]
	if len(owner.ClosedPositions) != 0 {
		t.Fatalf("owner ClosedPositions = %d, want 0 for unconfirmed SL fill", len(owner.ClosedPositions))
	}
	if pos := owner.Positions["ETH"]; pos == nil || math.Abs(pos.Quantity-ownerQty) > 1e-9 {
		t.Fatalf("owner position = %+v, want unchanged qty %v", pos, ownerQty)
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil || math.Abs(gap.DeltaQty-ownerQty) > 1e-9 {
		t.Fatalf("gap = %+v, want unresolved delta %v", gap, ownerQty)
	}
}

func TestReconcileSharedCoin_Detector3_PartialUsesFillPrice(t *testing.T) {
	const ownerStartCash = 1000.0
	const ownerQty = 0.5
	const peerQty = 0.5
	const avgCost = 3000.0
	const mark = 3200.0
	const fillPx = 3185.0
	const closeQty = 0.25
	const wantFee = 0.28

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: ownerStartCash, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: ownerQty, InitialQuantity: ownerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						EntryATR: 100, StopLossOID: 77, StopLossTriggerPx: 2900,
						TPOIDs: []int64{0, 222, 333}},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: peerQty, InitialQuantity: peerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.75, EntryPrice: avgCost, Leverage: 10}}
	prices := map[string]float64{"ETH": mark}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 0 && coin == "ETH" && math.Abs(qty-closeQty) < 1e-9 {
			return HLFillLookup{Fee: wantFee, Px: fillPx, Count: 1, OID: 5555}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	dm := &countingDMSender{}
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "0xtest", dm, true)

	owner := state.Strategies["hl-owner-eth"]
	if len(owner.TradeHistory) != 1 {
		t.Fatalf("owner close trades = %d, want 1", len(owner.TradeHistory))
	}
	tr := owner.TradeHistory[0]
	if !tr.IsClose || math.Abs(tr.Quantity-closeQty) > 1e-9 || tr.Price != fillPx {
		t.Errorf("close trade = %+v, want close %.2f @ fill %v", tr, closeQty, fillPx)
	}
	wantPnL := closeQty*(fillPx-avgCost) - wantFee
	if !tr.PnLGross || math.Abs(tradeNetPnL(tr)-wantPnL) > 1e-6 {
		t.Errorf("trade net PnL = %v (gross=%v), want %v", tradeNetPnL(tr), tr.PnLGross, wantPnL)
	}
	if math.Abs(owner.Cash-(ownerStartCash+wantPnL)) > 1e-6 {
		t.Errorf("owner Cash = %v, want %v", owner.Cash, ownerStartCash+wantPnL)
	}
	if dm.count != 1 {
		t.Fatalf("protection fill DM = %d, want 1", dm.count)
	}
	wantPriceInDM := fmt.Sprintf("@ $%.4f", fillPx)
	if !strings.Contains(dm.last, wantPriceInDM) {
		t.Errorf("DM should report fill price %s, got:\n%s", wantPriceInDM, dm.last)
	}
	if strings.Contains(dm.last, fmt.Sprintf("@ $%.4f", mark)) {
		t.Errorf("DM should not report mark %v, got:\n%s", mark, dm.last)
	}
}

func TestReconcileSharedCoin_Detector1_WrongOIDInUserfillsBooksExternal(t *testing.T) {
	const mark = 61000.0
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-btc": {
				ID: "hl-owner-btc", Cash: 10000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.2, AvgCost: 60000, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-owner-btc",
						StopLossOID: 5005, StopLossTriggerPx: 58000},
				},
			},
			"hl-peer-btc": {
				ID: "hl-peer-btc", Cash: 10000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60500, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-peer-btc"},
				},
			},
		},
	}
	scs := []StrategyConfig{
		{ID: "hl-owner-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
		{ID: "hl-peer-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
	}
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 5005 {
			return HLFillLookup{Fee: 1.0, FilledQty: 0.2, Count: 1, OID: 9999}, true
		}
		if oid == 0 && coin == "BTC" && math.Abs(qty-0.1) < 1e-9 {
			return HLFillLookup{Fee: 0.05, Count: 1}, true
		}
		return HLFillLookup{}, false
	}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	prices := map[string]float64{"BTC": mark}
	_, _, _ = reconcileHyperliquidAccountPositions(scs, scs, state, &mu, logMgr, nil, prices, "0xtest", nil, false)

	owner := state.Strategies["hl-owner-btc"]
	if len(owner.ClosedPositions) != 1 {
		t.Fatalf("owner ClosedPositions = %d, want 1", len(owner.ClosedPositions))
	}
	if owner.ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Errorf("owner CloseReason = %q, want hl_sync_external (wrong userFills OID)", owner.ClosedPositions[0].CloseReason)
	}
	if owner.ClosedPositions[0].ClosePrice != mark {
		t.Errorf("owner ClosePrice = %v, want mark %v", owner.ClosedPositions[0].ClosePrice, mark)
	}
}

func TestReconcileSharedCoin_Detector2_WrongOIDInUserfillsLeavesGap(t *testing.T) {
	const mark = 3020.0
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 4242, StopLossTriggerPx: 2900},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}
	scs := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Leverage: 10}}
	prices := map[string]float64{"ETH": mark}

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 4242 {
			return HLFillLookup{Fee: 0.1, FilledQty: 1.0, Count: 1, OID: 9999}, true
		}
		return HLFillLookup{}, false
	}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(scs, scs, state, &mu, logMgr, positions, prices, "0xtest", nil, false)

	owner := state.Strategies["hl-owner-eth"]
	if len(owner.ClosedPositions) != 0 {
		t.Fatalf("owner ClosedPositions = %d, want 0 for unconfirmed SL fill", len(owner.ClosedPositions))
	}
	ownerPos := owner.Positions["ETH"]
	if ownerPos == nil || math.Abs(ownerPos.Quantity-1.0) > 1e-9 {
		t.Fatalf("owner position = %+v, want unchanged 1.0 long", ownerPos)
	}

	peer := state.Strategies["hl-peer-eth"]
	p := peer.Positions["ETH"]
	if p == nil || math.Abs(p.Quantity-0.5) > 1e-9 || p.Side != "long" {
		t.Errorf("peer ETH = %+v, want 0.5 long unchanged", p)
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil || math.Abs(gap.DeltaQty-1.0) > 1e-9 {
		t.Fatalf("gap = %+v, want unresolved delta 1.0", gap)
	}
}

func TestReconcileSharedCoin_TPPartialFill_DecrementsOwnerAndBooksPnL(t *testing.T) {
	const ownerStartCash = 1000.0
	const ownerQty = 0.5
	const peerQty = 0.5
	const avgCost = 3000.0
	const mark = 3200.0

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: ownerStartCash, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: ownerQty, InitialQuantity: ownerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						EntryATR: 100, StopLossOID: 77, StopLossTriggerPx: 2900,
						TPOIDs: []int64{0, 222, 333}},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: peerQty, InitialQuantity: peerQty, AvgCost: avgCost, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.75, EntryPrice: avgCost, Leverage: 10}}
	prices := map[string]float64{"ETH": mark}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "", nil, false)

	owner := state.Strategies["hl-owner-eth"]
	ownerPos := owner.Positions["ETH"]
	if ownerPos == nil {
		t.Fatal("owner ETH position should remain after TP1 partial fill")
	}
	if math.Abs(ownerPos.Quantity-0.25) > 1e-9 {
		t.Errorf("owner quantity = %g, want 0.25", ownerPos.Quantity)
	}
	if ownerPos.InitialQuantity != ownerQty {
		t.Errorf("InitialQuantity = %g, want %g", ownerPos.InitialQuantity, ownerQty)
	}
	if ownerPos.AvgCost != avgCost {
		t.Errorf("AvgCost = %g, want %g", ownerPos.AvgCost, avgCost)
	}
	if ownerPos.StopLossOID != 77 {
		t.Errorf("StopLossOID = %d, want preserved 77", ownerPos.StopLossOID)
	}

	wantFee := 0.25 * mark * HyperliquidTakerFeePct
	wantPnL := 0.25*(mark-avgCost) - wantFee
	if math.Abs(owner.Cash-(ownerStartCash+wantPnL)) > 1e-6 {
		t.Errorf("owner Cash = %v, want %v", owner.Cash, ownerStartCash+wantPnL)
	}
	if len(owner.TradeHistory) != 1 {
		t.Fatalf("owner close trades = %d, want 1", len(owner.TradeHistory))
	}
	tr := owner.TradeHistory[0]
	if !tr.IsClose || tr.Side != "sell" || math.Abs(tr.Quantity-0.25) > 1e-9 || tr.Price != mark {
		t.Errorf("close trade = %+v, want sell close 0.25 @ %v", tr, mark)
	}
	if !tr.PnLGross || math.Abs(tradeNetPnL(tr)-wantPnL) > 1e-6 {
		t.Errorf("trade net PnL = %v (gross=%v), want %v", tradeNetPnL(tr), tr.PnLGross, wantPnL)
	}
	if tr.EntryATR != 100 || tr.StopLossTriggerPx != 2900 {
		t.Errorf("trade context EntryATR/SL = %v/%v, want 100/2900", tr.EntryATR, tr.StopLossTriggerPx)
	}
	if len(owner.ClosedPositions) != 0 {
		t.Errorf("ClosedPositions = %d, want 0 for partial close", len(owner.ClosedPositions))
	}

	peerPos := state.Strategies["hl-peer-eth"].Positions["ETH"]
	if peerPos == nil || math.Abs(peerPos.Quantity-peerQty) > 1e-9 {
		t.Errorf("peer ETH = %+v, want unchanged %.2f", peerPos, peerQty)
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.VirtualQty-0.75) > 1e-9 || math.Abs(gap.OnChainQty-0.75) > 1e-9 || math.Abs(gap.DeltaQty) > 1e-9 {
		t.Errorf("gap = %+v, want virtual/on-chain 0.75 with zero delta", gap)
	}
}

func TestReconcileSharedCoin_TPPartialFill_Short(t *testing.T) {
	const ownerStartCash = 1000.0
	const ownerQty = 0.5
	const peerQty = 0.5
	const avgCost = 3000.0
	const mark = 2800.0

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: ownerStartCash, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: ownerQty, InitialQuantity: ownerQty, AvgCost: avgCost, Side: "short",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						TPOIDs: []int64{0, 222, 333}},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: peerQty, InitialQuantity: peerQty, AvgCost: avgCost, Side: "short",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: -0.75, EntryPrice: avgCost, Leverage: 10}}
	prices := map[string]float64{"ETH": mark}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "", nil, false)

	owner := state.Strategies["hl-owner-eth"]
	ownerPos := owner.Positions["ETH"]
	if ownerPos == nil {
		t.Fatal("owner ETH short should remain after TP partial fill")
	}
	if math.Abs(ownerPos.Quantity-0.25) > 1e-9 {
		t.Errorf("owner short quantity = %g, want 0.25", ownerPos.Quantity)
	}
	wantFee := 0.25 * mark * HyperliquidTakerFeePct
	wantPnL := 0.25*(avgCost-mark) - wantFee
	if math.Abs(owner.Cash-(ownerStartCash+wantPnL)) > 1e-6 {
		t.Errorf("owner Cash = %v, want %v", owner.Cash, ownerStartCash+wantPnL)
	}
	if len(owner.TradeHistory) != 1 {
		t.Fatalf("owner close trades = %d, want 1", len(owner.TradeHistory))
	}
	tr := owner.TradeHistory[0]
	if !tr.IsClose || tr.Side != "buy" || math.Abs(tr.Quantity-0.25) > 1e-9 || tr.Price != mark {
		t.Errorf("close trade = %+v, want buy close 0.25 @ %v", tr, mark)
	}
	if !tr.PnLGross || math.Abs(tradeNetPnL(tr)-wantPnL) > 1e-6 {
		t.Errorf("trade net PnL = %v (gross=%v), want %v", tradeNetPnL(tr), tr.PnLGross, wantPnL)
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.VirtualQty-(-0.75)) > 1e-9 || math.Abs(gap.OnChainQty-(-0.75)) > 1e-9 || math.Abs(gap.DeltaQty) > 1e-9 {
		t.Errorf("gap = %+v, want virtual/on-chain -0.75 with zero delta", gap)
	}
}

func TestReconcileSharedCoin_TPPartialFill_PaddedNeverPlacedTierDoesNotAttribute(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						EntryATR: 100, TPOIDs: []int64{111}},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.75, EntryPrice: 3000, Leverage: 10}}
	prices := map[string]float64{"ETH": 3200}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "", nil, false)

	owner := state.Strategies["hl-owner-eth"]
	ownerPos := owner.Positions["ETH"]
	if ownerPos == nil {
		t.Fatal("owner ETH position should remain")
	}
	if math.Abs(ownerPos.Quantity-0.5) > 1e-9 {
		t.Errorf("owner quantity = %g, want unchanged 0.5", ownerPos.Quantity)
	}
	if len(owner.TradeHistory) != 0 {
		t.Fatalf("owner close trades = %d, want 0 because missing tier slot was never placed", len(owner.TradeHistory))
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.VirtualQty-1.0) > 1e-9 || math.Abs(gap.OnChainQty-0.75) > 1e-9 || math.Abs(gap.DeltaQty-0.25) > 1e-9 {
		t.Errorf("gap = %+v, want unresolved virtual=1.0 on-chain=0.75 delta=0.25", gap)
	}
}

func TestReconcileSharedCoin_TPPartialFill_MultipleCandidatesDoesNotAttribute(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-a-eth": {
				ID: "hl-owner-a-eth", Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-a-eth",
						EntryATR: 100, TPOIDs: []int64{0, 222}},
				},
			},
			"hl-owner-b-eth": {
				ID: "hl-owner-b-eth", Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-b-eth",
						EntryATR: 100, TPOIDs: []int64{0, 333}},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-a-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-owner-b-eth", Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}, Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.75, EntryPrice: 3000, Leverage: 10}}
	prices := map[string]float64{"ETH": 3200}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, prices, "", nil, false)

	for id, ss := range state.Strategies {
		pos := ss.Positions["ETH"]
		if pos == nil {
			t.Fatalf("%s ETH position should remain", id)
		}
		if math.Abs(pos.Quantity-0.5) > 1e-9 {
			t.Errorf("%s quantity = %g, want unchanged 0.5", id, pos.Quantity)
		}
		if len(ss.TradeHistory) != 0 {
			t.Fatalf("%s close trades = %d, want 0 because multiple TP candidates are ambiguous", id, len(ss.TradeHistory))
		}
	}
	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.VirtualQty-1.0) > 1e-9 || math.Abs(gap.OnChainQty-0.75) > 1e-9 || math.Abs(gap.DeltaQty-0.25) > 1e-9 {
		t.Errorf("gap = %+v, want unresolved virtual=1.0 on-chain=0.75 delta=0.25", gap)
	}
}

func TestReconcileSharedCoin_AllPositionsClosedExternally_NoMarkPrice_FallsBack(t *testing.T) {
	const peerStartCash = 500.0
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: peerStartCash, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
			"hl-other-eth": {
				ID: "hl-other-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-other-eth"},
				},
			},
		},
	}
	allStrategies := []StrategyConfig{
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-other-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

	peer := state.Strategies["hl-peer-eth"]
	if peer.Positions["ETH"] != nil {
		t.Error("peer ETH position should be nil")
	}
	if len(peer.TradeHistory) != 1 || peer.TradeHistory[0].RealizedPnL != 0 || !peer.TradeHistory[0].PnLGross {
		t.Fatalf("want one zero-gross-PnL trade row, got %+v", peer.TradeHistory)
	}
	fee := peer.TradeHistory[0].ExchangeFee
	if len(peer.ClosedPositions) != 1 || math.Abs(peer.ClosedPositions[0].RealizedPnL-(-fee)) > 1e-9 {
		t.Errorf("expected fee-only close PnL, got %+v", peer.ClosedPositions)
	}
	if math.Abs(peer.Cash-(peerStartCash-fee)) > 1e-9 {
		t.Errorf("peer Cash = %v, want %v (no mark price → modeled fee only, no PnL credit)", peer.Cash, peerStartCash-fee)
	}
}

func TestReconcileSharedCoin_GapWithoutSLOwner_LeavesPositionsAlone(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {
				ID: "hl-a-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.6, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-a-eth"},
				},
			},
			"hl-b-eth": {
				ID: "hl-b-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-b-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.7, EntryPrice: 3000, Leverage: 10}}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

	posA := state.Strategies["hl-a-eth"].Positions["ETH"]
	if posA == nil || math.Abs(posA.Quantity-0.6) > 1e-6 {
		t.Errorf("hl-a-eth ETH = %+v, want 0.6 (unchanged)", posA)
	}
	posB := state.Strategies["hl-b-eth"].Positions["ETH"]
	if posB == nil || math.Abs(posB.Quantity-0.4) > 1e-6 {
		t.Errorf("hl-b-eth ETH = %+v, want 0.4 (unchanged)", posB)
	}

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.DeltaQty-0.3) > 1e-6 {
		t.Errorf("gap DeltaQty = %g, want 0.3", gap.DeltaQty)
	}
}

func TestReconcileSharedCoin_ResidualMismatch_LeavesPositionsAlone(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner-eth": {
				ID: "hl-owner-eth", Cash: 1000, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-owner-eth",
						StopLossOID: 55, StopLossTriggerPx: 2900},
				},
			},
			"hl-peer-eth": {
				ID: "hl-peer-eth", Cash: 500, Platform: "hyperliquid",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-peer-eth"},
				},
			},
		},
	}

	allStrategies := []StrategyConfig{
		{ID: "hl-owner-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-peer-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 3000, Leverage: 10}}

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, nil, "", nil, false)

	ownerPos := state.Strategies["hl-owner-eth"].Positions["ETH"]
	if ownerPos == nil || math.Abs(ownerPos.Quantity-1.0) > 1e-6 {
		t.Errorf("owner ETH = %+v, want 1.0 (unchanged)", ownerPos)
	}
	peerPos := state.Strategies["hl-peer-eth"].Positions["ETH"]
	if peerPos == nil || math.Abs(peerPos.Quantity-0.5) > 1e-6 {
		t.Errorf("peer ETH = %+v, want 0.5 (unchanged)", peerPos)
	}

	gap := state.ReconciliationGaps["ETH"]
	if gap == nil {
		t.Fatal("expected gap entry for ETH")
	}
	if math.Abs(gap.DeltaQty-1.3) > 1e-6 {
		t.Errorf("gap DeltaQty = %g, want 1.3", gap.DeltaQty)
	}
}

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

func fakeCloser(errs map[string]error) (HyperliquidLiveCloser, *[]string) {
	var calls []string
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", symbol, *partialSz))
		} else {
			calls = append(calls, symbol)
		}
		if err, ok := errs[symbol]; ok {
			return nil, err
		}
		return &HyperliquidCloseResult{
			Close:                   &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform:                "hyperliquid",
			CancelStopLossSucceeded: firstPositiveStopLossOID(cancelStopLossOIDs) > 0,
		}, nil
	}
	return closer, &calls
}

func TestForceCloseHyperliquidLive_NonSharedCoin(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-ema-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.517, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

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
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

	if len(report.Errors) != 0 {
		t.Errorf("expected no errors, got %v", report.Errors)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	if len(*calls) != 1 || (*calls)[0] != "ETH" {
		t.Errorf("expected exactly 1 closer call for ETH, got %v", *calls)
	}
}

func TestForceCloseHyperliquidLive_NetZeroSziAlreadyFlat(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-bidir-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema_bidir", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

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

func TestForceCloseHyperliquidLive_ShortPosition(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-bidir-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema_bidir", "ETH", "1h", "--mode=live"}, AllowShorts: true},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: -1.234, EntryPrice: 3000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

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

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

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

func TestForceCloseHyperliquidLive_UnownedPositionIgnored(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5},
		{Coin: "DOGE", Size: 1000},
	}

	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", report.ClosedCoins)
	}
	for _, c := range *calls {
		if c == "DOGE" {
			t.Errorf("closer must not be invoked for unowned coin DOGE")
		}
	}
}

func TestForceCloseHyperliquidLive_EmptyInputs(t *testing.T) {
	report := forceCloseHyperliquidLive(context.Background(), nil, nil, nil, func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		t.Fatalf("closer should not be called with empty inputs")
		return nil, nil
	}, nil)
	if len(report.ClosedCoins) != 0 || len(report.AlreadyFlat) != 0 || len(report.Errors) != 0 {
		t.Errorf("expected empty report, got %+v", report)
	}
}

func TestForceCloseHyperliquidLive_AdapterAlreadyFlatRoutedCorrectly(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}

	var calls []string
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, symbol)
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)

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

func TestComputeHyperliquidCircuitCloseQty_SharedCoinSkipped(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-a", pos, hlLive)
	if ok || q != 0 {
		t.Fatalf("shared Hyperliquid coin must not enqueue a per-strategy close; qty=%.6f ok=%v", q, ok)
	}
}

func TestComputeHyperliquidCircuitCloseQty_ManualPeerSharedCoinSkipped(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-a", pos, hlLive)
	if ok || q != 0 {
		t.Fatalf("manual peer on shared Hyperliquid coin must not enqueue a per-strategy close; qty=%.6f ok=%v", q, ok)
	}
}

func TestComputeHyperliquidCircuitCloseQty_MixedUnitsSharedCoinSkipped(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}
	q, ok := computeHyperliquidCircuitCloseQty("ETH", "hl-a", pos, hlLive)
	if ok || q != 0 {
		t.Fatalf("shared Hyperliquid coin must not enqueue a per-strategy close; qty=%.6f ok=%v", q, ok)
	}
}

func TestRunPendingHyperliquidCircuitCloses_RecoversStuckCB(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					CircuitBreaker:       true,
					CircuitBreakerUntil:  time.Now().Add(24 * time.Hour),
					PendingCircuitCloses: nil,
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
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
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
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 1 || calls[0] != "ETH:0.4" {
		t.Errorf("closer calls=%v want [ETH:0.4] (recovered pending should drain full szi as sole owner)", calls)
	}
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Error("expected pending cleared after successful recovery close")
	}
}

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
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: sym}, Platform: "hyperliquid"}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		nil,
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 0 {
		t.Errorf("expected no closer calls when no on-chain position, got %v", calls)
	}
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Error("pending should remain nil when recovery has no on-chain position to close")
	}
}

func TestRunPendingHyperliquidCircuitCloses_ClearsOnSuccess(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
						},
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
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
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
		nil,
	)
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Error("expected pending cleared after successful close")
	}
	if len(calls) != 1 || calls[0] != "ETH:0.1" {
		t.Errorf("closer calls=%v want [ETH:0.1]", calls)
	}
}

func TestRunPendingHyperliquidCircuitCloses_SharedCoinClearsPendingWithoutClose(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
						},
					},
				},
			},
			"hl-b": {ID: "hl-b"},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
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
		nil,
	)

	if len(calls) != 0 {
		t.Fatalf("expected no closer calls for shared Hyperliquid coin; got %v", calls)
	}
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Fatal("expected stale shared-coin pending close to be cleared")
	}
}

func TestRunPendingHyperliquidCircuitCloses_ManualPeerClearsPendingWithoutClose(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
						},
					},
				},
			},
			"hl-manual-eth": {ID: "hl-manual-eth"},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
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
		nil,
	)

	if len(calls) != 0 {
		t.Fatalf("expected no closer calls when perps CB has a manual peer; got %v", calls)
	}
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Fatal("expected stale manual-peer pending close to be cleared")
	}
}

func TestRunPendingHyperliquidCircuitCloses_PartialFillKeepsPendingAndDecrements(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID:   "hl-a",
				Type: "perps",
				Cash: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 5},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1.0}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.5, AvgPx: 3000, Fee: 0.75}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 3000}}, true,
		nil, closer, 30*time.Second, &mu,
		nil,
	)

	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) == nil {
		t.Error("expected pending preserved after partial fill (allOK=false), got nil")
	}
	pos, ok := state.Strategies["hl-a"].Positions["ETH"]
	if !ok || pos == nil {
		t.Fatal("expected ETH position to remain (partial fill, residual 0.5)")
	}
	if math.Abs(pos.Quantity-0.5) > 1e-9 {
		t.Errorf("Quantity = %.6f; want 0.5 (1.0 - 0.5 partial fill)", pos.Quantity)
	}
	if pos.AvgCost != 3000 {
		t.Errorf("AvgCost = %.2f; want 3000 (must not change on partial close)", pos.AvgCost)
	}
	if len(state.Strategies["hl-a"].TradeHistory) != 1 {
		t.Errorf("expected 1 close trade recorded, got %d", len(state.Strategies["hl-a"].TradeHistory))
	}
	if len(state.Strategies["hl-a"].TradeHistory) > 0 {
		tr := state.Strategies["hl-a"].TradeHistory[0]
		if tr.Side != "sell" || tr.Quantity != 0.5 || tr.Price != 3000 {
			t.Errorf("close trade = %+v; want sell 0.5 @ 3000", tr)
		}
	}
}

func TestRunPendingHyperliquidCircuitCloses_FullFillDecrementsAndClears(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID:   "hl-a",
				Type: "perps",
				Cash: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 5},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.5, AvgPx: 2900, Fee: 0.5}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}, true,
		nil, closer, 30*time.Second, &mu,
		nil,
	)

	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Error("expected pending cleared after full close")
	}
	if _, ok := state.Strategies["hl-a"].Positions["ETH"]; ok {
		t.Error("expected ETH position removed after full close")
	}
	wantCash := 949.5
	if math.Abs(state.Strategies["hl-a"].Cash-wantCash) > 1e-6 {
		t.Errorf("Cash = %.4f; want %.4f (PnL -$50 - $0.50 fee)", state.Strategies["hl-a"].Cash, wantCash)
	}
	if len(state.Strategies["hl-a"].TradeHistory) != 1 {
		t.Fatalf("expected 1 close trade, got %d", len(state.Strategies["hl-a"].TradeHistory))
	}
	if len(state.Strategies["hl-a"].ClosedPositions) != 1 {
		t.Fatalf("expected 1 closed-position row, got %d", len(state.Strategies["hl-a"].ClosedPositions))
	}
	cp := state.Strategies["hl-a"].ClosedPositions[0]
	if cp.CloseReason != "circuit_breaker" || cp.ClosePrice != 2900 {
		t.Errorf("closed position = %+v; want circuit_breaker @ 2900", cp)
	}
}

func TestRunPendingHyperliquidCircuitCloses_SharedCoinLeavesVirtualPosition(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-tema": {
				ID:   "hl-tema",
				Type: "perps",
				Cash: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 10},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-tema", Platform: "hyperliquid", Type: "perps", Leverage: 10,
			Capital: 500, CapitalPct: 0.5,
			Args: []string{"triple_ema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-rmc", Platform: "hyperliquid", Type: "perps", Leverage: 10,
			Capital: 500, CapitalPct: 0.5,
			Args: []string{"rsi_macd", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls int
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls++
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.5, AvgPx: 3000, Fee: 0.5}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 3000}}, true,
		nil, closer, 30*time.Second, &mu,
		nil,
	)

	if calls != 0 {
		t.Fatalf("expected no closer calls for shared Hyperliquid coin; got %d", calls)
	}
	if pos := state.Strategies["hl-tema"].Positions["ETH"]; pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("expected firing strategy virtual position to remain unchanged; got %+v", pos)
	}
	if p := state.Strategies["hl-tema"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
		t.Fatalf("expected shared-coin pending close cleared; got %+v", p)
	}
}

func TestApplyHyperliquidCircuitCloseFill_PartialPreservesAvgCost(t *testing.T) {
	s := &StrategyState{
		ID:   "hl-x",
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1.0, AvgCost: 50000, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	applyHyperliquidCircuitCloseFill(s, "BTC", 0.3, 49000, 1.5, 1.0, 0, "")

	pos, ok := s.Positions["BTC"]
	if !ok {
		t.Fatal("BTC position must remain after partial close")
	}
	if math.Abs(pos.Quantity-0.7) > 1e-9 {
		t.Errorf("Quantity = %.6f; want 0.7 (1.0 - 0.3)", pos.Quantity)
	}
	if pos.AvgCost != 50000 {
		t.Errorf("AvgCost = %.2f; want 50000 (must not change on partial close — #418 review gap 3)", pos.AvgCost)
	}
	wantCash := 1000 + (-301.5)
	if math.Abs(s.Cash-wantCash) > 1e-6 {
		t.Errorf("Cash = %.4f; want %.4f", s.Cash, wantCash)
	}
}

func TestRunPendingHyperliquidCircuitCloses_ZeroFillKeepsPending(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID:   "hl-a",
				Type: "perps",
				Cash: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 5},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1.0}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: nil},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 3000}}, true,
		nil, closer, 30*time.Second, &mu,
		nil,
	)

	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) == nil {
		t.Error("pending must be preserved on zero-fill (#418 review observation 1)")
	}
	pos := state.Strategies["hl-a"].Positions["ETH"]
	if pos == nil || math.Abs(pos.Quantity-1.0) > 1e-9 {
		t.Errorf("Quantity should remain 1.0 on zero-fill, got %v", pos)
	}
	if len(state.Strategies["hl-a"].TradeHistory) != 0 {
		t.Errorf("expected no trade on zero-fill, got %d", len(state.Strategies["hl-a"].TradeHistory))
	}
}

func TestRunPendingHyperliquidCircuitCloses_PartialThenFullPreservesAvgCost(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID:   "hl-a",
				Type: "perps",
				Cash: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 5},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1.0}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex

	cycle1 := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.4, AvgPx: 2950, Fee: 0.4}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 3000}}, true,
		nil, cycle1, 30*time.Second, &mu,
		nil,
	)

	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) == nil {
		t.Fatal("cycle 1: pending must be preserved after partial fill")
	}
	pos := state.Strategies["hl-a"].Positions["ETH"]
	if pos == nil || math.Abs(pos.Quantity-0.6) > 1e-9 {
		t.Fatalf("cycle 1: Quantity = %v; want 0.6", pos)
	}
	if pos.AvgCost != 3000 {
		t.Errorf("cycle 1: AvgCost = %.2f; want 3000 (preserved on partial)", pos.AvgCost)
	}

	cycle2 := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		got := *partialSz
		if math.Abs(got-0.6) > 1e-6 {
			t.Errorf("cycle 2 closer expected sz=0.6 (residual cap), got %v", got)
		}
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 0.6, AvgPx: 2900, Fee: 0.6}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.6, EntryPrice: 3000}}, true,
		nil, cycle2, 30*time.Second, &mu,
		nil,
	)

	if _, ok := state.Strategies["hl-a"].Positions["ETH"]; ok {
		t.Error("cycle 2: position must be removed after full close")
	}
	if state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Error("cycle 2: pending must be cleared after full close")
	}
	closed := state.Strategies["hl-a"].ClosedPositions
	if len(closed) != 1 {
		t.Fatalf("expected 1 ClosedPosition, got %d", len(closed))
	}
	if math.Abs(closed[0].Quantity-0.6) > 1e-9 {
		t.Errorf("ClosedPosition.Quantity = %v; want 0.6 (residual at final close, not original 1.0)", closed[0].Quantity)
	}
	if closed[0].CloseReason != "circuit_breaker" {
		t.Errorf("ClosedPosition.CloseReason = %q; want circuit_breaker", closed[0].CloseReason)
	}
}

func TestApplyHyperliquidCircuitCloseFill_NoPositionShortCloseRecordsBuy(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-x",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 0.5, 3000, 0.5, -0.5, 0, "")

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 defensive trade, got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].Side != "buy" {
		t.Errorf("Side = %q; want buy (closing a short, #418 review observation 4)", s.TradeHistory[0].Side)
	}
}

func TestApplyHyperliquidCircuitCloseFill_NoPositionLongCloseRecordsSell(t *testing.T) {
	s := &StrategyState{
		ID:        "hl-x",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 0.5, 3000, 0.5, 0.5, 0, "")

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 defensive trade, got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].Side != "sell" {
		t.Errorf("Side = %q; want sell (closing a long)", s.TradeHistory[0].Side)
	}
}

func TestRunPendingHyperliquidCircuitCloses_FailureIncrementsCountAndNotifies(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.25}},
						},
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
	closer := func(symbol string, sz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("float_to_wire causes rounding")
	}
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.25}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		ownerDM,
	)
	p := state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("pending should be preserved on close failure")
	}
	if p.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures: got %d, want 1", p.ConsecutiveFailures)
	}
	if len(dmMsgs) != 1 {
		t.Errorf("expected 1 DM on first failure, got %d", len(dmMsgs))
	}
}

func TestRunPendingHyperliquidCircuitCloses_RepeatedFailureThrottlesNotifier(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.25}},
							ConsecutiveFailures: 1,
							LastNotifiedAt:      time.Now(),
						},
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
	closer := func(symbol string, sz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("float_to_wire causes rounding")
	}
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.25}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		ownerDM,
	)
	if len(dmMsgs) != 0 {
		t.Errorf("expected 0 DMs on failure #2 (suppressed), got %d", len(dmMsgs))
	}
}

func TestRunPendingHyperliquidCircuitCloses_TenthFailureNotifies(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.25}},
							ConsecutiveFailures: 9,
							LastNotifiedAt:      time.Now(),
						},
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
	closer := func(symbol string, sz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("float_to_wire causes rounding")
	}
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }
	runPendingHyperliquidCircuitCloses(
		context.Background(),
		state,
		cfg,
		"0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.25}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		ownerDM,
	)
	p := state.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || p.ConsecutiveFailures != 10 {
		t.Fatalf("expected ConsecutiveFailures=10, got %v", p)
	}
	if len(dmMsgs) != 1 {
		t.Errorf("expected 1 DM on failure #10 (every-10th cadence), got %d", len(dmMsgs))
	}
}

func TestReconcileManualPositionExternalClose(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"manual-eth": {
				ID: "manual-eth", Cash: 100,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "manual-eth"},
				},
			},
		},
	}
	sc := StrategyConfig{
		ID: "manual-eth", Platform: "hyperliquid", Type: "manual",
		Symbol: "ETH", Timeframe: "1h", Leverage: 5,
		Args: []string{"hold", "ETH", "1h", "--mode=live"},
	}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	_, _, _ = reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr, nil, nil, "", nil, false)

	ss := state.Strategies["manual-eth"]
	if _, ok := ss.Positions["ETH"]; ok {
		t.Error("ETH position should have been removed after external close")
	}
	if len(ss.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
	}
	if ss.ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Errorf("CloseReason = %q, want hl_sync_external", ss.ClosedPositions[0].CloseReason)
	}
}

func tieredTPATRSC() StrategyConfig {
	return StrategyConfig{
		ID: "hl-tp",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}},
	}
}

func TestHyperliquidHasClearedTPTier_NoTPOIDs(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{Quantity: 0.422, TPOIDs: nil}
	if hyperliquidHasClearedTPTier(sc, pos, 0.211) {
		t.Error("expected false when pos.TPOIDs is nil")
	}
	pos.TPOIDs = []int64{}
	if hyperliquidHasClearedTPTier(sc, pos, 0.211) {
		t.Error("expected false when pos.TPOIDs is empty")
	}
}

func TestHyperliquidHasClearedTPTier_AllActive(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{Quantity: 0.422, TPOIDs: []int64{111, 222}}
	if hyperliquidHasClearedTPTier(sc, pos, 0.211) {
		t.Error("expected false when all TP OIDs are active (non-zero)")
	}
}

func TestHyperliquidHasClearedTPTier_OneClearedOneActive(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{Quantity: 0.422, TPOIDs: []int64{0, 222}}
	if !hyperliquidHasClearedTPTier(sc, pos, 0.211) {
		t.Error("expected true when one tier is cleared and one is still active")
	}
}

func TestHyperliquidHasClearedTPTier_AllZeroFullClose(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}}
	if hyperliquidHasClearedTPTier(sc, pos, 0.211) {
		t.Error("expected false when all OIDs zero but closeQty != pos.Quantity (ambiguous gap)")
	}
	if !hyperliquidHasClearedTPTier(sc, pos, 0.422) {
		t.Error("expected true when all OIDs zero and closeQty == pos.Quantity (sole-peer final close)")
	}
	if hyperliquidAllTiersArmedAndCleared(sc, &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{true, true}}) {
		if hyperliquidHasClearedTPTier(sc, pos, 0.211) {
			t.Error("hyperliquidHasClearedTPTier must stay false for dust; use hlAttemptCloseFromArmedTPClears (#777)")
		}
	}
}

func TestReconcilePositionSLClose_UsesFilledQtyFromLookup(t *testing.T) {
	const (
		virtualQty  = 0.422
		filledQty   = 0.211
		slTriggerPx = 1800.0
		avgCost     = 2000.0
	)
	ss := &StrategyState{
		ID:   "hl-eth",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: virtualQty, AvgCost: avgCost, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 42, StopLossTriggerPx: slTriggerPx,
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.08, FilledQty: filledQty, Count: 1, OID: oid}, true
	})
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})
	if !changed {
		t.Fatal("expected changed=true")
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("ETH position should be closed after SL reconciliation")
	}
	if len(ss.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
	}
	cp := ss.ClosedPositions[0]
	if cp.Quantity < filledQty-1e-9 || cp.Quantity > filledQty+1e-9 {
		t.Errorf("ClosedPosition.Quantity = %g, want %g (actual fill qty, not virtual)", cp.Quantity, filledQty)
	}
	wantGross := filledQty * (slTriggerPx - avgCost)
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
	}
	tr := ss.TradeHistory[0]
	if !tr.PnLGross || math.Abs(tr.RealizedPnL-wantGross) > 0.01 {
		t.Errorf("RealizedPnL = %g (gross=%v), want gross %g (based on actual fill qty)", tr.RealizedPnL, tr.PnLGross, wantGross)
	}
	if math.Abs(tradeNetPnL(tr)-(wantGross-0.08)) > 0.01 {
		t.Errorf("tradeNetPnL = %g, want %g", tradeNetPnL(tr), wantGross-0.08)
	}
}

func TestReconcilePositionSLClose_NoFillFallsThroughToExternal(t *testing.T) {
	const virtualQty = 0.422
	ss := &StrategyState{
		ID:   "hl-eth",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: virtualQty, AvgCost: 2000, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 42, StopLossTriggerPx: 1800,
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, _ int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.15, FilledQty: 0, Count: 1}, true
	})
	logger := newTestLogger(t)
	startCash := ss.Cash
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})

	if len(ss.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
	}
	cp := ss.ClosedPositions[0]
	if cp.CloseReason != "hl_sync_external" {
		t.Errorf("CloseReason = %q, want hl_sync_external (SL not confirmed filled)", cp.CloseReason)
	}
	if cp.ClosePrice != 2000 {
		t.Errorf("ClosePrice = %g, want 2000 (AvgCost — zero-PnL booking, not the SL trigger)", cp.ClosePrice)
	}
	if len(ss.TradeHistory) != 1 || ss.TradeHistory[0].RealizedPnL != 0 || !ss.TradeHistory[0].PnLGross {
		t.Fatalf("want one zero-gross-PnL trade row, got %+v", ss.TradeHistory)
	}
	if math.Abs(ss.Cash-(startCash-0.15)) > 1e-9 {
		t.Errorf("cash = %g, want %g (real fee only) on zero-PnL fallback", ss.Cash, startCash-0.15)
	}
}

func TestReconcileManualPositionSLFired(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"manual-eth": {
				ID: "manual-eth", Cash: 100,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "manual-eth",
						StopLossOID: 77, StopLossTriggerPx: 1800},
				},
			},
		},
	}
	sc := StrategyConfig{
		ID: "manual-eth", Platform: "hyperliquid", Type: "manual",
		Symbol: "ETH", Timeframe: "1h", Leverage: 5,
		Args: []string{"hold", "ETH", "1h", "--mode=live"},
	}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(_, _ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 77 {
			return HLFillLookup{Fee: 0.05, FilledQty: 0.5, Px: 1800, Count: 1, OID: 77}, true
		}
		return HLFillLookup{}, false
	}

	_, _, _ = reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr, nil, nil, "0xtest", nil, false)

	ss := state.Strategies["manual-eth"]
	if _, ok := ss.Positions["ETH"]; ok {
		t.Error("ETH position should have been removed after SL fire")
	}
	if len(ss.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
	}
	if ss.ClosedPositions[0].CloseReason != "stop_loss" {
		t.Errorf("CloseReason = %q, want stop_loss", ss.ClosedPositions[0].CloseReason)
	}
	if ss.ClosedPositions[0].ClosePrice != 1800 {
		t.Errorf("ClosePrice = %g, want 1800", ss.ClosedPositions[0].ClosePrice)
	}
}

func TestReconcilePosition_TPFillsAttributedNotSL(t *testing.T) {
	const (
		entryPx     = 2315.70
		tp1Px       = 2325.19
		tp2Px       = 2329.93
		slTriggerPx = 2308.60
		qtyTotal    = 0.432
		qtyTier     = 0.216
		feeTP1      = 0.10
		feeTP2      = 0.11
	)
	ss := &StrategyState{
		ID:   "hl-tema-eth-live",
		Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: qtyTotal, InitialQuantity: qtyTotal,
				AvgCost: entryPx, Side: "long",
				Multiplier: 1, Leverage: 20, OwnerStrategyID: "hl-tema-eth-live",
				StopLossOID: 999, StopLossTriggerPx: slTriggerPx,
				TPOIDs: []int64{111, 222},
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		switch oid {
		case 111:
			return HLFillLookup{Fee: feeTP1, FilledQty: qtyTier, Px: tp1Px, Count: 1, OID: 111}, true
		case 222:
			return HLFillLookup{Fee: feeTP2, FilledQty: qtyTier, Px: tp2Px, Count: 1, OID: 222}, true
		default:
			return HLFillLookup{}, false
		}
	})
	logger := newTestLogger(t)

	var alerts []ProtectionFillAlert
	startCash := ss.Cash
	changed := reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, &alerts, nil, StrategyConfig{})
	if !changed {
		t.Fatal("expected changed=true")
	}
	if len(alerts) != 2 {
		t.Fatalf("pendingAlerts = %d, want 2 (TP1+TP2 fill DMs)", len(alerts))
	}
	if alerts[0].FillType != "TP1" || alerts[1].FillType != "TP2" {
		t.Errorf("FillType = %q, %q, want TP1, TP2", alerts[0].FillType, alerts[1].FillType)
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("ETH position should be removed after TP-fill attribution")
	}
	if len(ss.TradeHistory) != 2 {
		t.Fatalf("TradeHistory = %d, want 2 (one per TP tier)", len(ss.TradeHistory))
	}
	for i, trade := range ss.TradeHistory {
		if !trade.IsClose {
			t.Errorf("trade[%d].IsClose = false, want true", i)
		}
		if trade.RealizedPnL <= 0 {
			t.Errorf("trade[%d].RealizedPnL = %g, want positive (TP profit)", i, trade.RealizedPnL)
		}
	}
	wantPnL := qtyTier*(tp1Px-entryPx) - feeTP1 + qtyTier*(tp2Px-entryPx) - feeTP2
	gotPnL := ss.Cash - startCash
	if math.Abs(gotPnL-wantPnL) > 0.01 {
		t.Errorf("cash delta = %g, want %g (sum of TP fill PnLs minus fees)", gotPnL, wantPnL)
	}
	if gotPnL <= 0 {
		t.Errorf("cash delta = %g, want positive — pre-fix this booked at SL trigger and was negative", gotPnL)
	}
}

func TestReconcilePosition_SLFillStillTakesSLPath(t *testing.T) {
	const slTriggerPx = 1800.0
	ss := &StrategyState{
		ID: "hl-eth", Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.4, AvgCost: 2000, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 42, StopLossTriggerPx: slTriggerPx,
				TPOIDs: []int64{111, 222},
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 42 {
			return HLFillLookup{Fee: 0.05, FilledQty: 0.4, Px: slTriggerPx, Count: 1, OID: 42}, true
		}
		return HLFillLookup{}, false
	})
	logger := newTestLogger(t)
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})

	if len(ss.ClosedPositions) != 1 || ss.ClosedPositions[0].CloseReason != "stop_loss" {
		t.Fatalf("expected one ClosedPosition with reason=stop_loss, got %+v", ss.ClosedPositions)
	}
	if ss.ClosedPositions[0].ClosePrice != slTriggerPx {
		t.Errorf("ClosePrice = %g, want %g (SL trigger)", ss.ClosedPositions[0].ClosePrice, slTriggerPx)
	}
}

func TestReconcilePosition_PartialTPFillResidualZeroPnL(t *testing.T) {
	const (
		entryPx  = 2000.0
		tp1Px    = 2050.0
		qtyTotal = 0.4
		qtyTier1 = 0.2
		feeTP1   = 0.05
	)
	ss := &StrategyState{
		ID: "hl-eth", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: qtyTotal, InitialQuantity: qtyTotal,
				AvgCost: entryPx, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 999, StopLossTriggerPx: 1900,
				TPOIDs: []int64{111, 222},
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		if oid == 111 {
			return HLFillLookup{Fee: feeTP1, FilledQty: qtyTier1, Px: tp1Px, Count: 1, OID: 111}, true
		}
		return HLFillLookup{}, false
	})
	logger := newTestLogger(t)
	startCash := ss.Cash
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})

	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("ETH should be removed even when TP fills under-shoot")
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1 (TP1 only — TP2 had no fill)", len(ss.TradeHistory))
	}
	if len(ss.ClosedPositions) != 1 || ss.ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Fatalf("expected residual ClosedPosition with reason=hl_sync_external, got %+v", ss.ClosedPositions)
	}
	if ss.ClosedPositions[0].Quantity < qtyTier1-1e-9 || ss.ClosedPositions[0].Quantity > qtyTier1+1e-9 {
		t.Errorf("residual ClosedPosition.Quantity = %g, want %g", ss.ClosedPositions[0].Quantity, qtyTier1)
	}
	wantPnL := qtyTier1*(tp1Px-entryPx) - feeTP1
	gotPnL := ss.Cash - startCash
	if math.Abs(gotPnL-wantPnL) > 0.01 {
		t.Errorf("cash delta = %g, want %g (TP1 only — residual contributes 0)", gotPnL, wantPnL)
	}
}

func TestReconcilePosition_NoFillsFallsBackToZeroPnL(t *testing.T) {
	ss := &StrategyState{
		ID: "hl-eth", Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.4, AvgCost: 2000, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				TPOIDs: []int64{111, 222},
			},
		},
	}
	resolver := noFillFeeResolver
	logger := newTestLogger(t)
	startCash := ss.Cash
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})

	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("ETH should be removed even when no fills found")
	}
	if len(ss.ClosedPositions) != 1 || ss.ClosedPositions[0].CloseReason != "hl_sync_external" {
		t.Fatalf("expected ClosedPosition with reason=hl_sync_external, got %+v", ss.ClosedPositions)
	}
	if len(ss.TradeHistory) != 1 || ss.TradeHistory[0].RealizedPnL != 0 || ss.TradeHistory[0].FeeSource != FeeSourceModeled {
		t.Fatalf("want one zero-gross-PnL modeled-fee trade row, got %+v", ss.TradeHistory)
	}
	if math.Abs(ss.Cash-(startCash-ss.TradeHistory[0].ExchangeFee)) > 1e-9 {
		t.Errorf("cash = %g, want %g (modeled fee only) on zero-PnL fallback", ss.Cash, startCash-ss.TradeHistory[0].ExchangeFee)
	}
}

func TestReconcilePosition_AllTPOIDsZeroedSLNotFilled(t *testing.T) {
	const slTriggerPx = 1800.0
	ss := &StrategyState{
		ID: "hl-eth", Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.215, AvgCost: 2329.8, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 999, StopLossTriggerPx: slTriggerPx,
				TPOIDs: []int64{0, 0},
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, _ int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	logger := newTestLogger(t)
	startCash := ss.Cash
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})

	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("ETH position should be removed after reconcile")
	}
	if len(ss.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
	}
	cp := ss.ClosedPositions[0]
	if cp.CloseReason != "hl_sync_external" {
		t.Errorf("CloseReason = %q, want hl_sync_external (SL not confirmed filled)", cp.CloseReason)
	}
	if cp.ClosePrice == slTriggerPx {
		t.Errorf("ClosePrice = %g matches stale SL trigger — #685 regression", cp.ClosePrice)
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1 (no-mark-price close must book a row, #954)", len(ss.TradeHistory))
	}
	tr := ss.TradeHistory[0]
	if !tr.PnLGross || tr.RealizedPnL != 0 || tr.Price != 2329.8 {
		t.Errorf("zero-info close row = pnl %g px %g (gross=%v), want gross 0 @ AvgCost", tr.RealizedPnL, tr.Price, tr.PnLGross)
	}
	if tr.FeeSource != FeeSourceModeled {
		t.Errorf("FeeSource = %q, want modeled (no userFills match)", tr.FeeSource)
	}
	if math.Abs(ss.Cash-(startCash-tr.ExchangeFee)) > 1e-9 {
		t.Errorf("cash = %g, want %g — only the modeled fee may move cash, never fictitious SL PnL", ss.Cash, startCash-tr.ExchangeFee)
	}
}

func TestAttemptCloseFromTPFills_CoinSizeSLFallbackDoesNotStarveTP(t *testing.T) {
	const (
		entryPx = 2000.0
		tpPx    = 2100.0
		qty     = 0.2
	)
	ss := &StrategyState{
		ID: "hl-eth", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: qty, InitialQuantity: qty,
				AvgCost: entryPx, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-eth",
				StopLossOID: 42, StopLossTriggerPx: 1900,
				TPOIDs: []int64{111},
			},
		},
	}
	resolver := hlReconcileFillResolver(func(_ string, _ int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.04, FilledQty: qty, Px: tpPx, Count: 1, OID: 111}, true
	})
	logger := newTestLogger(t)

	var tpAlerts []ProtectionFillAlert
	if !hlAttemptCloseFromTPFills(ss, "ETH", ss.Positions["ETH"], resolver, logger, &tpAlerts) {
		t.Fatal("expected TP attribution to proceed (SL gate must reject non-OID-match)")
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Error("ETH position should be removed after TP attribution")
	}
	if len(ss.TradeHistory) != 1 || !ss.TradeHistory[0].IsClose {
		t.Fatalf("expected one close trade, got %+v", ss.TradeHistory)
	}
	if math.Abs(ss.TradeHistory[0].Price-tpPx) > 1e-9 {
		t.Errorf("Trade.Price = %g, want %g (TP fill price)", ss.TradeHistory[0].Price, tpPx)
	}
	if len(tpAlerts) != 1 || tpAlerts[0].FillType != "TP1" {
		t.Errorf("tpAlerts = %+v, want one TP1 protection alert", tpAlerts)
	}
}
