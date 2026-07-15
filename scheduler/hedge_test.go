package main

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeHedgeCoin(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"ticker", " btc ", "BTC"},
		{"perp symbol", "BTC/USDC:USDC", "BTC"},
		{"settle symbol", " eth/usdc:usdc ", "ETH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeHedgeCoin(tc.in); got != tc.want {
				t.Fatalf("normalizeHedgeCoin(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHedgeTargetQuantityUsesNotionalRatio(t *testing.T) {
	got, err := hedgeTargetQuantity(2, 100, 50, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("target qty = %v, want %v", got, want)
	}
}

func TestDecideHedgeOpenAndPartialReduction(t *testing.T) {
	primary := hedgePositionSnapshot{Quantity: 2, Side: "long"}
	decision := decideHedge(primary, hedgePositionSnapshot{}, 100, 50, 0.25, true)
	if decision.Action != hedgeActionOpen || decision.Side != "short" || decision.Quantity != 1 {
		t.Fatalf("open decision = %+v", decision)
	}

	hedge := hedgePositionSnapshot{Quantity: 1, Side: "short", HedgePrimaryQtyBasis: 2}
	decision = decideHedge(hedgePositionSnapshot{Quantity: 1, Side: "long"}, hedge, 100, 50, 0.25, false)
	if decision.Action != hedgeActionReduce || decision.Quantity != 0.5 || decision.PrimaryQtyBasis != 1 {
		t.Fatalf("partial reduction decision = %+v", decision)
	}
}

func TestDecideHedgeDoesNotChurnOnPriceOnlyChanges(t *testing.T) {
	primary := hedgePositionSnapshot{Quantity: 2, Side: "long"}
	hedge := hedgePositionSnapshot{Quantity: 1, Side: "short", HedgePrimaryQtyBasis: 2}
	decision := decideHedge(primary, hedge, 125, 80, 0.25, false)
	if decision.Action != hedgeActionNone {
		t.Fatalf("price-only decision = %+v, want none", decision)
	}
}

func TestDecideHedgeFailsClosedWhenPrimaryOpenedWithoutHedge(t *testing.T) {
	decision := decideHedge(hedgePositionSnapshot{Quantity: 1, Side: "long"}, hedgePositionSnapshot{}, 100, 50, 1, false)
	if decision.Action != hedgeActionPrimaryFailure {
		t.Fatalf("decision = %+v, want primary failure", decision)
	}
}

func TestDecideHedgeHoldsWhenEitherMarkIsMissing(t *testing.T) {
	primary := hedgePositionSnapshot{Quantity: 1, Side: "long"}
	hedge := hedgePositionSnapshot{Quantity: 2, Side: "short", HedgePrimaryQtyBasis: 1}
	for name, prices := range map[string][2]float64{
		"missing primary": {0, 100},
		"missing hedge":   {100, 0},
	} {
		t.Run(name, func(t *testing.T) {
			decision := decideHedge(primary, hedge, prices[0], prices[1], 1, false)
			if decision.Action != hedgeActionHold {
				t.Fatalf("decision = %+v, want hold", decision)
			}
		})
	}
}

func TestSyncHedgeDoesNotMutateOnMissingMark(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
		Args:  []string{"sma", "ETH", "1h"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1, Platform: "hyperliquid", Type: "perps", MarginMode: "isolated", Leverage: 1},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 100, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 2, AvgCost: 50, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
		}},
	}}
	var mu sync.RWMutex
	syncHedgeForStrategy(sc, state, &mu, map[string]float64{"ETH": 100}, nil, false, nil, nil)
	if got := state.Strategies[sc.ID].Positions["ETH"].Quantity; got != 1 {
		t.Fatalf("primary quantity = %v, want unchanged 1", got)
	}
	if got := state.Strategies[sc.ID].Positions["BTC"].Quantity; got != 2 {
		t.Fatalf("hedge quantity = %v, want unchanged 2", got)
	}
}

func TestHedgeBasisAfterReductionTracksConfirmedFill(t *testing.T) {
	cases := []struct {
		name                                       string
		previous, current, requested, filled, want float64
	}{
		{"full fill", 2, 1, 0.5, 0.5, 1},
		{"partial fill", 2, 1, 0.5, 0.25, 1.5},
		{"no confirmed fill", 2, 1, 0.5, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hedgeBasisAfterReduction(tc.previous, tc.current, tc.requested, tc.filled)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("basis = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHedgeConfigValidationRejectsCollisions(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "primary", Type: "perps", Platform: "hyperliquid", Args: []string{"open", "BTC", "1h"}, Direction: DirectionLong,
			Hedge: &HedgeConfig{Enabled: true, Symbol: "ETH", Side: "inverse", Ratio: 1, Platform: "hyperliquid", Type: "perps", MarginMode: "isolated", Leverage: 1}},
		{ID: "eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"hold", "ETH", "1h"}},
	}}
	errs := validateHedgeConfigs(cfg)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "collides") {
		t.Fatalf("validateHedgeConfigs = %v, want hedge/primary collision", errs)
	}
}

func TestHedgePositionPersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"primary": {
			ID: "primary", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 2000, Side: "short", Multiplier: 1, OwnerStrategyID: "primary"},
				"BTC": {Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 50000, Side: "long", Multiplier: 1, OwnerStrategyID: "primary", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
			}, OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["primary"].Positions["BTC"]
	if pos == nil || pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 1 {
		t.Fatalf("loaded hedge position = %+v", pos)
	}
}

func TestHedgeTradeStatsExcludedButLedgerRowsRemain(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	rows := []Trade{
		{StrategyID: "s", Timestamp: now, Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Value: 2000, TradeType: "perps", PositionID: "p"},
		{StrategyID: "s", Timestamp: now.Add(time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: TradeTypeHedge, PositionID: "h", ExchangeFee: 1},
		{StrategyID: "s", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 49000, Value: 4900, TradeType: TradeTypeHedge, PositionID: "h", IsClose: true, RealizedPnL: 100, ExchangeFee: 1},
		{StrategyID: "s", Timestamp: now.Add(3 * time.Second), Symbol: "ETH", Side: "sell", Quantity: 1, Price: 2100, Value: 2100, TradeType: "perps", PositionID: "p", IsClose: true, RealizedPnL: 100},
	}
	for _, row := range rows {
		if err := db.InsertTrade("s", row); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	stats, err := db.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats["s"]; got.PositionsOpened != 1 || got.Wins != 1 || got.Losses != 0 {
		t.Fatalf("hedge-inclusive stats = %+v, want one primary round-trip only", got)
	}
	var ledger float64
	if err := db.db.QueryRow("SELECT SUM" + tradeLedgerDeltaSQL + " FROM trades WHERE strategy_id='s'").Scan(&ledger); err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	if ledger == 0 {
		t.Fatal("hedge rows did not remain in the cash ledger")
	}
}

func TestRecordHedgeTradeResultDoesNotAdvanceLossStreak(t *testing.T) {
	r := &RiskState{DailyPnLDate: time.Now().UTC().Format("2006-01-02"), ConsecutiveLosses: 3}
	RecordHedgeTradeResult(r, -10)
	if r.ConsecutiveLosses != 3 || r.DailyPnL != -10 {
		t.Fatalf("risk after hedge pnl = %+v, want streak 3 and daily -10", r)
	}
}
