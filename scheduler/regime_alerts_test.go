package main

import (
	"strings"
	"testing"
)

// ─── Trade.Regime field ───────────────────────────────────────────────────────

func TestTradeRegimeFieldExists(t *testing.T) {
	trade := Trade{Regime: "trending_up"}
	if trade.Regime != "trending_up" {
		t.Errorf("expected trending_up, got %q", trade.Regime)
	}
}

func TestTradeRegimeDefaultEmpty(t *testing.T) {
	trade := Trade{}
	if trade.Regime != "" {
		t.Errorf("expected empty Regime by default, got %q", trade.Regime)
	}
}

// ─── FormatTradeDM includes regime ───────────────────────────────────────────

func TestFormatTradeDM_IncludesRegime(t *testing.T) {
	sc := StrategyConfig{ID: "hl-btc-1", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    60000,
		Value:    600,
		Details:  "Open long",
		Regime:   "trending_up",
	}
	msg := FormatTradeDM(sc, trade, "paper")
	if !strings.Contains(msg, "Regime: trending_up") {
		t.Errorf("expected 'Regime: trending_up' in DM, got:\n%s", msg)
	}
}

func TestFormatTradeDM_RegimeBeforeMode(t *testing.T) {
	sc := StrategyConfig{ID: "hl-btc-1", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    60000,
		Value:    600,
		Details:  "Open long",
		Regime:   "ranging",
	}
	msg := FormatTradeDM(sc, trade, "paper")
	regimeIdx := strings.Index(msg, "Regime:")
	modeIdx := strings.Index(msg, "Mode:")
	if regimeIdx == -1 || modeIdx == -1 {
		t.Fatalf("missing Regime or Mode in DM: %s", msg)
	}
	if regimeIdx >= modeIdx {
		t.Errorf("Regime should appear before Mode; got:\n%s", msg)
	}
}

func TestFormatTradeDM_EmptyRegimeOmitted(t *testing.T) {
	sc := StrategyConfig{ID: "hl-btc-1", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    60000,
		Value:    600,
		Details:  "Open long",
		Regime:   "",
	}
	msg := FormatTradeDM(sc, trade, "paper")
	if strings.Contains(msg, "Regime:") {
		t.Errorf("empty Regime should be omitted from DM, got:\n%s", msg)
	}
}

// ─── FormatTradeDMPlain includes regime ──────────────────────────────────────

func TestFormatTradeDMPlain_IncludesRegime(t *testing.T) {
	sc := StrategyConfig{ID: "hl-btc-1", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    60000,
		Value:    600,
		Details:  "Open long",
		Regime:   "trending_down",
	}
	msg := FormatTradeDMPlain(sc, trade, "live")
	if !strings.Contains(msg, "Regime: trending_down") {
		t.Errorf("expected 'Regime: trending_down' in plain DM, got:\n%s", msg)
	}
}

func TestFormatTradeDMPlain_EmptyRegimeOmitted(t *testing.T) {
	sc := StrategyConfig{ID: "hl-btc-1", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    60000,
		Value:    600,
		Details:  "Open long",
	}
	msg := FormatTradeDMPlain(sc, trade, "paper")
	if strings.Contains(msg, "Regime:") {
		t.Errorf("empty Regime should be omitted from plain DM, got:\n%s", msg)
	}
}

// ─── InsertTrade persists Regime ─────────────────────────────────────────────

func TestInsertTrade_RegimePersisted(t *testing.T) {
	db := mustOpenTestDB(t)
	defer db.Close()

	trade := Trade{
		Symbol:    "BTC",
		Side:      "buy",
		Quantity:  0.01,
		Price:     60000,
		Value:     600,
		TradeType: "perps",
		Regime:    "trending_up",
	}
	if err := db.InsertTrade("test-strat", trade); err != nil {
		t.Fatalf("InsertTrade failed: %v", err)
	}

	rows, err := db.db.Query("SELECT regime FROM trades WHERE strategy_id = 'test-strat'")
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
	if regime != "trending_up" {
		t.Errorf("expected trending_up, got %q", regime)
	}
}

func TestInsertTrade_EmptyRegimeStored(t *testing.T) {
	db := mustOpenTestDB(t)
	defer db.Close()

	trade := Trade{Symbol: "ETH", Side: "buy", Quantity: 0.1, Price: 3000, Value: 300, TradeType: "spot"}
	if err := db.InsertTrade("test-strat2", trade); err != nil {
		t.Fatalf("InsertTrade failed: %v", err)
	}

	rows, err := db.db.Query("SELECT regime FROM trades WHERE strategy_id = 'test-strat2'")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows")
	}
	var regime string
	if err := rows.Scan(&regime); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if regime != "" {
		t.Errorf("expected empty regime, got %q", regime)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func mustOpenTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	return db
}
