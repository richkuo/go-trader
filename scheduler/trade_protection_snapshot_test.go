package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTradeOpenStopLossATRMult_FixedATR(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{StopLossATRMult: &mult}
	got := tradeOpenStopLossATRMult(sc)
	if got == nil || *got != 1.5 {
		t.Fatalf("StopLossATRMult=1.5: got %v, want *1.5", got)
	}
}

func TestTradeOpenStopLossATRMult_TrailingATR(t *testing.T) {
	mult := 2.0
	sc := StrategyConfig{TrailingStopATRMult: &mult}
	got := tradeOpenStopLossATRMult(sc)
	if got == nil || *got != 2.0 {
		t.Fatalf("TrailingStopATRMult=2.0: got %v, want *2.0", got)
	}
}

func TestTradeOpenStopLossATRMult_NilForPctArmedAndZero(t *testing.T) {
	pct := 5.0
	zero := 0.0
	cases := []struct {
		name string
		sc   StrategyConfig
	}{
		{"pct-armed (StopLossPct)", StrategyConfig{StopLossPct: &pct}},
		{"pct-armed (TrailingStopPct)", StrategyConfig{TrailingStopPct: &pct}},
		{"pct-armed (StopLossMarginPct)", StrategyConfig{StopLossMarginPct: &pct}},
		{"no SL fields", StrategyConfig{}},
		{"explicit zero StopLossATRMult", StrategyConfig{StopLossATRMult: &zero}},
		{"explicit zero TrailingStopATRMult", StrategyConfig{TrailingStopATRMult: &zero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tradeOpenStopLossATRMult(tc.sc); got != nil {
				t.Fatalf("expected nil (non-ATR-armed), got %v", *got)
			}
		})
	}
}

func TestTradeOpenTPTiersJSON_TieredCloseStrategy(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{
			{Name: "tiered_tp_atr", Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				},
			}},
		},
	}
	got := tradeOpenTPTiersJSON(sc)
	if got == "" {
		t.Fatal("expected non-empty TPTiersJSON for tiered_tp_atr close strategy")
	}
	var parsed []map[string]float64
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal TPTiersJSON: %v (raw=%s)", err, got)
	}
	if len(parsed) != 2 {
		t.Fatalf("want 2 tiers, got %d (raw=%s)", len(parsed), got)
	}
	if parsed[0]["atr_multiple"] != 1.0 || parsed[0]["close_fraction"] != 0.5 {
		t.Fatalf("tier[0] = %v, want {atr_multiple:1, close_fraction:0.5}", parsed[0])
	}
	if parsed[1]["atr_multiple"] != 2.0 || parsed[1]["close_fraction"] != 1.0 {
		t.Fatalf("tier[1] = %v, want {atr_multiple:2, close_fraction:1}", parsed[1])
	}
}

func TestTradeOpenTPTiersJSON_NoTieredCloseReturnsEmpty(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{
			{Name: "tp_at_pct", Params: map[string]interface{}{"pct": 0.05}},
		},
	}
	if got := tradeOpenTPTiersJSON(sc); got != "" {
		t.Fatalf("expected empty (no tiered_tp_atr*), got %q", got)
	}
}

func TestStampPositionProtectionSnapshot_NilPos(t *testing.T) {
	stampPositionProtectionSnapshot(nil, StrategyConfig{}) // must not panic
}

func TestStampPositionProtectionSnapshot_Idempotent(t *testing.T) {
	mult := 1.5
	pos := &Position{}
	sc := StrategyConfig{StopLossATRMult: &mult}

	stampPositionProtectionSnapshot(pos, sc)
	if pos.StopLossATRMult == nil || *pos.StopLossATRMult != 1.5 {
		t.Fatalf("first stamp: got %v", pos.StopLossATRMult)
	}

	// Idempotent: re-stamp with a different config does not overwrite.
	otherMult := 9.9
	stampPositionProtectionSnapshot(pos, StrategyConfig{StopLossATRMult: &otherMult})
	if *pos.StopLossATRMult != 1.5 {
		t.Fatalf("second stamp overwrote: got %v, want 1.5", *pos.StopLossATRMult)
	}
}

// TestStampOpenTradeFromPositionBackfillsProtectionSnapshot verifies that the
// SL ATR mult + TP tier snapshot stamped on Position post-fill flows onto the
// most-recent open Trade row (#669).
func TestStampOpenTradeFromPositionBackfillsProtectionSnapshot(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &StrategyState{ID: "stamp-test", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false, Timestamp: ts},
	}}
	if err := db.InsertTrade(s.ID, s.TradeHistory[0]); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	mult := 1.5
	pos := &Position{
		StopLossATRMult: &mult,
		TPTiersJSON:     `[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1}]`,
	}
	stampOpenTradeFromPosition(s, db, "ETH", pos)

	if s.TradeHistory[0].StopLossATRMult == nil || *s.TradeHistory[0].StopLossATRMult != 1.5 {
		t.Fatalf("in-memory StopLossATRMult: got %v, want *1.5", s.TradeHistory[0].StopLossATRMult)
	}
	if s.TradeHistory[0].TPTiersJSON != pos.TPTiersJSON {
		t.Fatalf("in-memory TPTiersJSON: got %q, want %q", s.TradeHistory[0].TPTiersJSON, pos.TPTiersJSON)
	}

	// Verify SQLite row was updated.
	var slMult float64
	var tpTiersJSON string
	if err := db.db.QueryRow(
		`SELECT stop_loss_atr_mult, tp_tiers_json FROM trades WHERE strategy_id = ? AND timestamp = ?`,
		s.ID, formatTime(ts),
	).Scan(&slMult, &tpTiersJSON); err != nil {
		t.Fatalf("query stamped trade: %v", err)
	}
	if slMult != 1.5 || tpTiersJSON != pos.TPTiersJSON {
		t.Fatalf("persisted snapshot = (%v, %q), want (1.5, %q)", slMult, tpTiersJSON, pos.TPTiersJSON)
	}
}

// TestInsertTradePreservesNullStopLossATRMult verifies the nullness gate: a
// pct-armed open trade lands as SQL NULL so analytics can distinguish ATR-armed
// from pct-armed without back-computing.
func TestInsertTradePreservesNullStopLossATRMult(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()

	ts := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	pctArmed := Trade{Symbol: "BTC", Timestamp: ts, StrategyID: "s1"}
	if err := db.InsertTrade(pctArmed.StrategyID, pctArmed); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	var slMult interface{}
	if err := db.db.QueryRow(
		`SELECT stop_loss_atr_mult FROM trades WHERE strategy_id = ? AND timestamp = ?`,
		pctArmed.StrategyID, formatTime(ts),
	).Scan(&slMult); err != nil {
		t.Fatalf("query: %v", err)
	}
	if slMult != nil {
		t.Fatalf("expected NULL for pct-armed trade, got %v", slMult)
	}

	// ATR-armed → non-NULL value preserved exactly.
	mult := 1.25
	atrArmed := Trade{Symbol: "ETH", Timestamp: ts.Add(time.Second), StrategyID: "s1", StopLossATRMult: &mult, TPTiersJSON: `[{"atr_multiple":1,"close_fraction":1}]`}
	if err := db.InsertTrade(atrArmed.StrategyID, atrArmed); err != nil {
		t.Fatalf("InsertTrade ATR: %v", err)
	}
	var got float64
	var tiers string
	if err := db.db.QueryRow(
		`SELECT stop_loss_atr_mult, tp_tiers_json FROM trades WHERE strategy_id = ? AND timestamp = ?`,
		atrArmed.StrategyID, formatTime(atrArmed.Timestamp),
	).Scan(&got, &tiers); err != nil {
		t.Fatalf("query ATR: %v", err)
	}
	if got != 1.25 || tiers != atrArmed.TPTiersJSON {
		t.Fatalf("ATR row: got (%v, %q), want (1.25, %q)", got, tiers, atrArmed.TPTiersJSON)
	}
}

// TestApplyHyperliquidCircuitCloseFill_StampsProtectionSnapshot verifies that
// the kill-switch on-chain close path copies StopLossATRMult and TPTiersJSON
// from the position onto the Trade — without this the round-trip analytics
// promise from #669 breaks for kill-switch closes (PR #671 review).
func TestApplyHyperliquidCircuitCloseFill_StampsProtectionSnapshot(t *testing.T) {
	mult := 1.5
	tiersJSON := `[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1}]`
	s := &StrategyState{
		ID:   "hl-snapshot",
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: 1.0, AvgCost: 50000, Side: "long",
				Multiplier: 1, Leverage: 5,
				StopLossATRMult: &mult,
				TPTiersJSON:     tiersJSON,
			},
		},
	}
	applyHyperliquidCircuitCloseFill(s, "BTC", 0.3, 49000, 1.5, 1.0)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if tr.StopLossATRMult == nil || *tr.StopLossATRMult != mult {
		t.Errorf("Trade.StopLossATRMult = %v, want *%v", tr.StopLossATRMult, mult)
	}
	if tr.TPTiersJSON != tiersJSON {
		t.Errorf("Trade.TPTiersJSON = %q, want %q", tr.TPTiersJSON, tiersJSON)
	}
}

// TestForceCloseAllPositions_StampsProtectionSnapshot verifies the same
// round-trip on the circuit-breaker force-close path (risk.go).
func TestForceCloseAllPositions_StampsProtectionSnapshot(t *testing.T) {
	mult := 2.0
	tiersJSON := `[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1}]`
	s := &StrategyState{
		ID:   "perps-snapshot",
		Cash: 10000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long",
				Multiplier: 1, Leverage: 5,
				StopLossATRMult: &mult,
				TPTiersJSON:     tiersJSON,
			},
		},
		TradeHistory: []Trade{},
		RiskState:    RiskState{},
	}

	forceCloseAllPositions(s, map[string]float64{"BTC": 51000}, nil)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if !tr.IsClose {
		t.Errorf("Trade.IsClose = false, want true")
	}
	if tr.StopLossATRMult == nil || *tr.StopLossATRMult != mult {
		t.Errorf("Trade.StopLossATRMult = %v, want *%v", tr.StopLossATRMult, mult)
	}
	if tr.TPTiersJSON != tiersJSON {
		t.Errorf("Trade.TPTiersJSON = %q, want %q", tr.TPTiersJSON, tiersJSON)
	}
}
