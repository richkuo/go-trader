package main

import (
	"io"
	"testing"
)

func rhLiveSpot(id, coin string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"momentum_pro", coin, "1h", "--mode=live"}, Capital: capital, MaxDrawdownPct: 50}
}

func TestRobinhoodLiveOrderBookingGuardRefusesLiveResultWithoutFill(t *testing.T) {
	sc := rhLiveSpot("rh-btc", "BTC", 1000)
	e := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	if err := e.store.LoadRobinhoodOrderGuards(e.state, e.cfg.Strategies); err != nil {
		t.Fatalf("load robinhood guards: %v", err)
	}
	logger := &StrategyLogger{stratID: "rh-test", writer: io.Discard}
	s := e.state.Strategies["rh-btc"]
	db, _ := e.store.dbForStrategy("rh-btc")
	for _, er := range []*RobinhoodExecuteResult{nil, {}, {Execution: &RobinhoodExecution{Fill: &RobinhoodFill{AvgPx: 100}}}} {
		e.mu.Lock()
		trades, _, _ := executeRobinhoodResult(sc, s, db, &RobinhoodResult{Symbol: "BTC", Signal: 1}, er, "BUY", 100, nil, e.cfg, HurstGateDecision{}, logger)
		e.mu.Unlock()
		if trades != 0 || s.Cash != 1000 || len(s.Positions) != 0 {
			t.Fatalf("live booking without a confirmed fill must refuse: trades=%d cash=%v", trades, s.Cash)
		}
	}
}

func TestRobinhoodPaperBookingStillBooksTheModeledTrade(t *testing.T) {
	sc := StrategyConfig{ID: "rh-paper", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py", Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}, Capital: 1000}
	e := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	if err := e.store.LoadRobinhoodOrderGuards(e.state, e.cfg.Strategies); err != nil {
		t.Fatalf("load robinhood guards: %v", err)
	}
	logger := &StrategyLogger{stratID: "rh-test", writer: io.Discard}
	s := e.state.Strategies["rh-paper"]
	db, _ := e.store.dbForStrategy("rh-paper")
	e.mu.Lock()
	trades, _, _ := executeRobinhoodResult(sc, s, db, &RobinhoodResult{Symbol: "BTC", Signal: 1}, nil, "BUY", 100, nil, e.cfg, HurstGateDecision{}, logger)
	e.mu.Unlock()
	if trades != 1 || s.Positions["BTC"] == nil {
		t.Fatalf("paper buy trades=%d positions=%v, want the modeled buy", trades, s.Positions)
	}
	var attempts int
	if err := e.store.primary().db.QueryRow(`SELECT COUNT(*) FROM robinhood_order_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("a paper booking must make no order attempt, got %d rows", attempts)
	}
}
