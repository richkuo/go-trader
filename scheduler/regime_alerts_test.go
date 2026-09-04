package main

import (
	"testing"
	"time"
)

func TestInsertTrade_RegimePersisted(t *testing.T) {
	cases := []struct {
		name       string
		strategyID string
		trade      Trade
		want       string
	}{
		{
			name:       "regime stored",
			strategyID: "test-strat",
			trade:      Trade{Symbol: "BTC", Side: "buy", Quantity: 0.01, Price: 60000, Value: 600, TradeType: "perps", Regime: "trending_up"},
			want:       "trending_up",
		},
		{
			name:       "empty regime stored",
			strategyID: "test-strat2",
			trade:      Trade{Symbol: "ETH", Side: "buy", Quantity: 0.1, Price: 3000, Value: 300, TradeType: "spot"},
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := mustOpenTestDB(t)
			defer db.Close()

			if err := db.InsertTrade(tc.strategyID, tc.trade); err != nil {
				t.Fatalf("InsertTrade failed: %v", err)
			}
			rows, err := db.db.Query("SELECT regime FROM trades WHERE strategy_id = ?", tc.strategyID)
			if err != nil {
				t.Fatalf("query failed: %v", err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatal("no rows returned")
			}
			var regime string
			if err := rows.Scan(&regime); err != nil {
				t.Fatalf("scan failed: %v", err)
			}
			if regime != tc.want {
				t.Errorf("regime = %q, want %q", regime, tc.want)
			}
		})
	}
}

func TestRegime_LoadStateAndQueryTradeHistoryRoundTrip(t *testing.T) {
	db := mustOpenTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec("INSERT INTO app_state (id, cycle_count) VALUES (1, 1)"); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}
	if _, err := db.db.Exec("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES (?, ?, ?, ?, ?)",
		"s1", "perps", "hyperliquid", 1000.0, 1000.0); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	trade1 := Trade{Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Regime: "trending_up", Timestamp: now}
	trade2 := Trade{Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Regime: "", Timestamp: now.Add(time.Second)}
	if err := db.InsertTrade("s1", trade1); err != nil {
		t.Fatalf("InsertTrade trade1: %v", err)
	}
	if err := db.InsertTrade("s1", trade2); err != nil {
		t.Fatalf("InsertTrade trade2: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	trades := loaded.Strategies["s1"].TradeHistory
	if len(trades) != 2 {
		t.Fatalf("LoadState trade count = %d; want 2", len(trades))
	}
	if got := trades[0].Regime; got != "trending_up" {
		t.Errorf("LoadState trades[0].Regime = %q; want trending_up", got)
	}
	if got := trades[1].Regime; got != "" {
		t.Errorf("LoadState trades[1].Regime = %q; want empty", got)
	}

	history, total, err := db.QueryTradeHistory("s1", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 2 || len(history) != 2 {
		t.Fatalf("QueryTradeHistory total=%d len=%d; want 2/2", total, len(history))
	}
	if got := history[0].Regime; got != "" {
		t.Errorf("QueryTradeHistory history[0].Regime = %q; want empty (newest, no regime)", got)
	}
	if got := history[1].Regime; got != "trending_up" {
		t.Errorf("QueryTradeHistory history[1].Regime = %q; want trending_up (oldest)", got)
	}
}

func TestRegime_StampedAtProductionCallSites(t *testing.T) {
	newState := func(platform string) *StrategyState {
		return &StrategyState{
			ID: "test-strat", Cash: 100000, Platform: platform,
			Positions:       make(map[string]*Position),
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{}, RiskState: RiskState{},
			Regime: "trending_up",
		}
	}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	const want = "trending_up"
	lastRegime := func(s *StrategyState) string {
		if len(s.TradeHistory) == 0 {
			return "<no trade recorded>"
		}
		return s.TradeHistory[len(s.TradeHistory)-1].Regime
	}

	t.Run("portfolio/ExecuteSpotSignalWithFillFee", func(t *testing.T) {
		s := newState("binanceus")
		ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000, 0, 0, "", 0, logger)
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("portfolio/recordPerpsStopLossClose", func(t *testing.T) {
		s := newState("hyperliquid")
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", Leverage: 5}
		ok := recordPerpsStopLossClose(s, "BTC", 49000, "stop_loss", logger)
		if !ok {
			t.Fatal("recordPerpsStopLossClose returned false")
		}
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("risk/forceCloseAllPositions", func(t *testing.T) {
		s := newState("hyperliquid")
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long"}
		forceCloseAllPositions(s, nil, map[string]float64{"BTC": 51000}, logger)
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("hyperliquid_balance/applyHyperliquidCircuitCloseFill_normal", func(t *testing.T) {
		s := newState("hyperliquid")
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, AvgCost: 50000, Side: "long", Multiplier: 1, Leverage: 5}
		applyHyperliquidCircuitCloseFill(s, "BTC", 1.0, 49000, 1.5, 1.0, 0, "")
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("hyperliquid_balance/applyHyperliquidCircuitCloseFill_noPosition", func(t *testing.T) {
		s := newState("hyperliquid")
		applyHyperliquidCircuitCloseFill(s, "BTC", 1.0, 49000, 1.5, 1.0, 0, "")
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("options/executeOptionBuy", func(t *testing.T) {
		s := newState("ibkr")
		result := &OptionsResult{Underlying: "BTC", SpotPrice: 60000}
		action := &OptionsAction{Action: "buy", OptionType: "call", Strike: 60000, Expiry: "2026-12-26", Quantity: 1, PremiumUSD: 100}
		executeOptionBuy(s, result, action, logger)
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("options/executeOptionSell", func(t *testing.T) {
		s := newState("ibkr")
		result := &OptionsResult{Underlying: "BTC", SpotPrice: 60000}
		action := &OptionsAction{Action: "sell", OptionType: "call", Strike: 60000, Expiry: "2026-12-26", Quantity: 1, PremiumUSD: 100}
		executeOptionSell(s, result, action, logger)
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})

	t.Run("options/executeOptionClose", func(t *testing.T) {
		s := newState("ibkr")
		result := &OptionsResult{Underlying: "BTC", SpotPrice: 60000}
		posID := "BTC-call-buy-60000-2026-12-26"
		s.OptionPositions[posID] = &OptionPosition{
			ID: posID, Underlying: "BTC", OptionType: "call", Strike: 60000,
			Expiry: "2026-12-26", Action: "buy", Quantity: 1,
			EntryPremiumUSD: 100, CurrentValueUSD: 150,
		}
		action := &OptionsAction{Action: "close", OptionType: "call", Strike: 60000, Expiry: "2026-12-26", PremiumUSD: 150}
		executeOptionClose(s, result, action, logger)
		if got := lastRegime(s); got != want {
			t.Errorf("Regime = %q; want %q", got, want)
		}
	})
}

func mustOpenTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	return db
}
