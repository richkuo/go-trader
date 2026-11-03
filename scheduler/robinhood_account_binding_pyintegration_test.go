//go:build pyintegration

package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const rhOtherVenue = "acct-other"

func (e *rhLiveEnv) setBoundPosition(id, coin string, qty float64, key, venue string) {
	e.t.Helper()
	e.setPosition(id, coin, qty, 90)
	e.mu.Lock()
	pos := e.state.Strategies[id].Positions[coin]
	pos.AccountKey, pos.VenueAccountID = key, venue
	pos.AccountBindingSource = ""
	if key != "" || venue != "" {
		pos.AccountBindingSource = robinhoodBindingSourceBackfill
	}
	pos.OpenedAt = time.Now().Add(-time.Hour)
	err := e.store.SaveStrategyBook(e.state.Strategies[id])
	e.mu.Unlock()
	if err != nil {
		e.t.Fatalf("seed binding: %v", err)
	}
}

func (e *rhLiveEnv) position(id, coin string) *Position {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if pos := e.state.Strategies[id].Positions[coin]; pos != nil {
		cp := *pos
		return &cp
	}
	return nil
}

func (e *rhLiveEnv) seedSettlingAttempt(id string, a *robinhoodOrderAttempt) {
	e.t.Helper()
	db, err := e.store.dbForStrategy(id)
	if err != nil {
		e.t.Fatal(err)
	}
	sid, err := db.robinhoodAttemptOwner(id)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := upsertRobinhoodAttemptRow(db.db, sid, a); err != nil {
		e.t.Fatalf("seed attempt: %v", err)
	}
	e.reopen()
}

func rhSignalSellRequest(pid string, qty float64) string {
	return marshalRobinhoodJSON(robinhoodOrderRequest{Version: robinhoodSizedSellRequestVersion, Origin: robinhoodOriginSignal, Side: "sell", SignalStr: "SELL",
		Quantity: qty, Price: 99, PrevPositionID: pid, PrevPosSide: "long", PrevPosQty: qty, CloseKind: robinhoodCloseKindWhole, SoleOwner: true,
		Result: []byte(`{"symbol":"BTC","signal":-1}`), BudgetMs: 50000, Script: "shared_scripts/check_robinhood.py"})
}

func rhEmergencySellRequest(qty float64, peers ...string) string {
	return marshalRobinhoodJSON(robinhoodOrderRequest{Version: robinhoodEmergencySellRequestVersion, Origin: robinhoodOriginKillSwitch, Side: "sell",
		Quantity: qty, CloseKind: robinhoodCloseKindEmergency, Peers: peers, BudgetMs: 50000, Script: robinhoodLiveCloseScript})
}

func (e *rhLiveEnv) resolveDeps(fetcher RobinhoodPositionsFetcher, mark func(string) (float64, error)) robinhoodAccountResolveDeps {
	return robinhoodAccountResolveDeps{
		store: e.store, state: e.state, mu: &e.mu,
		lookup: func(id string) (StrategyConfig, bool) {
			for _, sc := range e.cfg.Strategies {
				if sc.ID == id {
					return sc, true
				}
			}
			return StrategyConfig{}, false
		},
		cfgs:    func() []StrategyConfig { return e.cfg.Strategies },
		fetcher: fetcher,
		mark:    mark,
	}
}

func rhMark(px float64) func(string) (float64, error) {
	return func(string) (float64, error) { return px, nil }
}

func (e *rhLiveEnv) holdingsFetcher(rows ...interface{}) RobinhoodPositionsFetcher {
	return e.phasedFetcher(rhHoldingsPhase(rows...))
}

func (e *rhLiveEnv) dbBinding(id, coin string) rhBindRow {
	e.t.Helper()
	var r rhBindRow
	err := e.store.primary().db.QueryRow(`SELECT account_key, venue_account_id, account_binding_source FROM positions WHERE strategy_id = ? AND symbol = ?`, id, coin).Scan(&r.Key, &r.Venue, &r.Source)
	if err != nil {
		e.t.Fatalf("binding row %s/%s: %v", id, coin, err)
	}
	return r
}

func (e *rhLiveEnv) dbPositionCount(id string) int {
	e.t.Helper()
	var n int
	if err := e.store.primary().db.QueryRow(`SELECT COUNT(*) FROM positions WHERE strategy_id = ?`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestRobinhoodAccountBindingSignals(t *testing.T) {
	t.Run("confirmed_buy_binds_the_new_book", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"POST " + rhTestOrders:             {rhAccepted("buy")},
			"GET " + rhOrderURL(rhTestOrderID): {rhOrder("filled", "buy", "9.5", "100.25")},
		}))
		if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 1 {
			t.Fatalf("trades = %d, want the live buy booked", trades)
		}
		pos := e.position("rh-btc", "BTC")
		if pos.AccountKey != robinhoodAccountKey() || pos.VenueAccountID != rhTestAccount || pos.AccountBindingSource != robinhoodBindingSourceSettlement {
			t.Fatalf("book = %+v, want bound by settlement", pos)
		}
		if got := e.dbBinding("rh-btc", "BTC"); got != (rhBindRow{robinhoodAccountKey(), rhTestAccount, robinhoodBindingSourceSettlement}) {
			t.Fatalf("db binding = %+v", got)
		}
	})
	for _, tc := range []struct {
		name, key, venue string
	}{
		{"unbound", "", ""},
		{"key_only", "current", ""},
		{"foreign_key", "robinhood:user-0000000000000000", rhTestAccount},
	} {
		t.Run("held_sell_reserves_nothing_"+tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			key := tc.key
			if key == "current" {
				key = robinhoodAccountKey()
			}
			e.setBoundPosition("rh-btc", "BTC", 0.5, key, tc.venue)
			e.setRoutes(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.5")))
			if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
				t.Fatalf("trades = %d, want none", trades)
			}
			if rows := e.attemptRows(); len(rows) != 0 {
				t.Fatalf("attempts = %+v, want none reserved", rows)
			}
			if reqs := e.requests(); len(reqs) != 0 {
				t.Fatalf("requests = %+v, want no venue call", reqs)
			}
			if pos := e.position("rh-btc", "BTC"); pos == nil || pos.Quantity != 0.5 {
				t.Fatalf("book = %+v, want intact", pos)
			}
		})
	}
	t.Run("bound_sell_refuses_another_venue_account_before_sending", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, robinhoodAccountKey(), rhTestAccount)
		e.setRoutes(rhWith(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.5")), rhRoutes{"GET " + rhTestAccounts: {rhAccountRoute(rhOtherVenue)}}))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
			t.Fatalf("trades = %d, want none", trades)
		}
		if posts := e.orderPosts(); len(posts) != 0 {
			t.Fatalf("order posts = %+v, want none from another account", posts)
		}
		row := e.onlyAttempt()
		if row.Status != robinhoodAttemptNoFill {
			t.Fatalf("attempt = %+v, want released as not sent", row)
		}
		if pos := e.position("rh-btc", "BTC"); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("book = %+v, want intact", pos)
		}
	})
	t.Run("bound_sell_settles_and_keeps_the_binding_on_a_partial_fill", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, robinhoodAccountKey(), rhTestAccount)
		e.setRoutes(rhWith(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.5")), rhRoutes{
			"POST " + rhTestOrders:             {rhAccepted("sell")},
			"GET " + rhOrderURL(rhTestOrderID): {rhOrder("canceled", "sell", "0.2", "99")},
		}))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 1 {
			t.Fatalf("trades = %d, want the partial sale booked", trades)
		}
		pos := e.position("rh-btc", "BTC")
		if pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 || pos.AccountKey != robinhoodAccountKey() || pos.VenueAccountID != rhTestAccount {
			t.Fatalf("book = %+v, want 0.3 left with the binding kept", pos)
		}
	})
}

func TestRobinhoodAccountBindingSettlementHolds(t *testing.T) {
	t.Run("settling_sell_from_another_configured_account_waits_until_it_returns", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		origKey := robinhoodAccountKey()
		e.setBoundPosition("rh-btc", "BTC", 0.5, origKey, rhTestAccount)
		pid := e.position("rh-btc", "BTC").TradePositionID
		e.seedSettlingAttempt("rh-btc", &robinhoodOrderAttempt{AttemptID: "rh-000000000000000000000001", AccountKey: origKey, Symbol: "BTC", Origin: robinhoodOriginSignal, Side: "sell",
			RefID: "11111111-1111-4111-8111-111111111111", VenueOrderID: "ord-s", VenueAccountID: rhTestAccount, Status: robinhoodAttemptSettling,
			RequestJSON: rhSignalSellRequest(pid, 0.5), ConfirmedQty: 0.5, ConfirmedAvgPx: 99, CreatedAtMs: 1, UpdatedAtMs: 1})
		t.Setenv("ROBINHOOD_USERNAME", "another-user")
		e.setRoutes(rhRoutes{})
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		row := e.onlyAttempt()
		if row.Status != robinhoodAttemptSettling || len(e.dbTrades("rh-btc")) != 0 || e.position("rh-btc", "BTC") == nil {
			t.Fatalf("attempt = %+v trades = %+v, want held with the book intact", row, e.dbTrades("rh-btc"))
		}
		if reqs := e.requests(); len(reqs) != 0 {
			t.Fatalf("requests = %+v, want no venue call", reqs)
		}
		e.mu.RLock()
		hold := e.state.Strategies["rh-btc"].RobinhoodOrders.Attempts["rh-000000000000000000000001"].HoldReason
		e.mu.RUnlock()
		if !strings.Contains(hold, "configured account") {
			t.Fatalf("hold = %q, want an account hold", hold)
		}
		t.Setenv("ROBINHOOD_USERNAME", "offline-user")
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled {
			t.Fatalf("attempt = %+v, want settled once the original account returns", row)
		}
		if e.position("rh-btc", "BTC") != nil || len(e.dbTrades("rh-btc")) != 1 {
			t.Fatalf("want the sale booked into the bound book")
		}
	})
	t.Run("emergency_sale_skips_a_peer_bound_to_another_venue_account", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		key := robinhoodAccountKey()
		e.setBoundPosition("rh-a", "BTC", 0.3, key, rhTestAccount)
		e.setBoundPosition("rh-b", "BTC", 0.2, key, rhOtherVenue)
		e.seedSettlingAttempt("rh-a", &robinhoodOrderAttempt{AttemptID: "rh-000000000000000000000002", AccountKey: key, Symbol: "BTC", Origin: robinhoodOriginKillSwitch, Side: "sell",
			RefID: "22222222-2222-4222-8222-222222222222", VenueOrderID: "ord-e", VenueAccountID: rhTestAccount, Status: robinhoodAttemptSettling,
			RequestJSON: rhEmergencySellRequest(0.3, "rh-a", "rh-b"), ConfirmedQty: 0.3, ConfirmedAvgPx: 98, CreatedAtMs: 1, UpdatedAtMs: 1})
		settleRobinhoodAttempts(e.deps(), e.cfg, nil)
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled || math.Abs(row.AppliedQty-0.3) > 1e-12 {
			t.Fatalf("attempt = %+v, want 0.3 applied", row)
		}
		if e.position("rh-a", "BTC") != nil || e.position("rh-b", "BTC").Quantity != 0.2 || len(e.dbTrades("rh-b")) != 0 {
			t.Fatalf("want rh-a closed and the other-venue rh-b book untouched")
		}
	})
}

func TestRobinhoodAccountBindingDeploymentTransition(t *testing.T) {
	for _, origin := range []string{robinhoodOriginSignal, robinhoodOriginKillSwitch} {
		t.Run(origin, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			key := robinhoodAccountKey()
			e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
			pid := e.position("rh-btc", "BTC").TradePositionID
			req := rhSignalSellRequest(pid, 0.5)
			if origin != robinhoodOriginSignal {
				req = rhEmergencySellRequest(0.5, "rh-btc")
			}
			e.seedSettlingAttempt("rh-btc", &robinhoodOrderAttempt{AttemptID: "rh-000000000000000000000003", AccountKey: key, Symbol: "BTC", Origin: origin, Side: "sell",
				RefID: "33333333-3333-4333-8333-333333333333", VenueOrderID: "ord-t", VenueAccountID: rhTestAccount, Status: robinhoodAttemptSettling,
				RequestJSON: req, ConfirmedQty: 0.2, ConfirmedAvgPx: 98, CreatedAtMs: 1, UpdatedAtMs: 1})
			settleRobinhoodAttempts(e.deps(), e.cfg, nil)
			e.mu.RLock()
			hold := e.state.Strategies["rh-btc"].RobinhoodOrders.Attempts["rh-000000000000000000000003"].HoldReason
			e.mu.RUnlock()
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettling || !strings.Contains(hold, "#1921") {
				t.Fatalf("attempt = %+v hold = %q, want held for the account binding", row, hold)
			}
			if n := e.dmCount("SALE ABOVE THE BOOK"); n != 0 {
				t.Fatalf("excess alerts = %d, want none while a held book owns the coin", n)
			}
			if pos := e.position("rh-btc", "BTC"); pos == nil || pos.Quantity != 0.5 {
				t.Fatalf("book = %+v, want intact", pos)
			}
			d := e.resolveDeps(e.holdingsFetcher(rhHeld("BTC", "0.3")), nil)
			res, prompt := prepareRobinhoodAccountResolution(d, "rh-btc", robinhoodAccountActionBind)
			if res == nil || res.AttemptID != "rh-000000000000000000000003" {
				t.Fatalf("prepare = %q, want the settling sale allowed", prompt)
			}
			if msg := applyRobinhoodAccountResolution(d, "owner", *res, time.Now()); !strings.HasPrefix(msg, "Bound") {
				t.Fatalf("bind = %q", msg)
			}
			settleRobinhoodAttempts(e.deps(), e.cfg, nil)
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled || math.Abs(row.AppliedQty-0.2) > 1e-12 {
				t.Fatalf("attempt = %+v, want the 0.2 sale booked into the bound book", row)
			}
			if pos := e.position("rh-btc", "BTC"); pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 {
				t.Fatalf("book = %+v, want 0.3 left", pos)
			}
			if n := e.dmCount("SALE ABOVE THE BOOK"); n != 0 {
				t.Fatalf("excess alerts = %d, want none", n)
			}
		})
	}
}

func TestRobinhoodAccountBindingKillSwitch(t *testing.T) {
	for _, held := range []string{"0", "0.5"} {
		t.Run("held_only_coin_account_holds_"+held, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
			var rows []interface{}
			if held != "0" {
				rows = append(rows, rhHeld("BTC", held))
			}
			in := e.killSwitchInputs(rhHoldingsPhase(rows...), rhHoldingsPhase(rows...))
			plan := planKillSwitchClose(in)
			rep := plan.RHCloseReport
			if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() || rep.Errors["BTC"] == nil || !strings.Contains(rep.Errors["BTC"].Error(), "account hold") {
				t.Fatalf("report = %+v, want BTC held in Errors", rep)
			}
			if len(rep.ClosedCoins)+len(rep.AlreadyFlat)+len(rep.BookFlat)+len(rep.VerifiedFlatCoins()) != 0 {
				t.Fatalf("report = %+v, want BTC in no flat list", rep)
			}
			if posts := e.orderPosts(); len(posts) != 0 || len(e.attemptRows()) != 0 {
				t.Fatalf("posts = %+v, want no sale", posts)
			}
			e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
			if pos := e.position("rh-btc", "BTC"); pos == nil || pos.Quantity != 0.5 || len(e.dbTrades("rh-btc")) != 0 {
				t.Fatalf("book = %+v, want intact", pos)
			}
		})
	}
	t.Run("mixed_coin_sells_only_the_bound_books", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000), rhLiveSpot("rh-c", "BTC", 1000))
		key := robinhoodAccountKey()
		e.setBoundPosition("rh-a", "BTC", 0.3, key, rhTestAccount)
		e.setBoundPosition("rh-b", "BTC", 0.2, key, rhOtherVenue)
		e.setBoundPosition("rh-c", "BTC", 0.1, "", "")
		in := e.killSwitchInputs(rhHoldingsPhase(rhHeld("BTC", "0.6")), rhHoldingsPhase(rhHeld("BTC", "0.3")))
		in.RHCloser = e.phaseCloser(rhClosePhase(rhSellFilled("0.3", "98"), "0.6", "0.6"), in.RHCloser)
		plan := planKillSwitchClose(in)
		rhSamePosts(t, e.allPostQuantities(), 0.3)
		rep := plan.RHCloseReport
		if plan.OnChainConfirmedFlat || rep.Errors["BTC"] == nil || !strings.Contains(rep.Errors["BTC"].Error(), "account hold") || len(rep.ClosedCoins) != 0 {
			t.Fatalf("report = %+v, want the coin unconfirmed while held books remain", rep)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		if e.position("rh-a", "BTC") != nil {
			t.Fatalf("rh-a = %+v, want closed by the venue fill", e.position("rh-a", "BTC"))
		}
		if e.position("rh-b", "BTC").Quantity != 0.2 || e.position("rh-c", "BTC").Quantity != 0.1 || len(e.dbTrades("rh-b"))+len(e.dbTrades("rh-c")) != 0 {
			t.Fatalf("held peers changed")
		}
	})
	t.Run("post_close_read_from_another_account_proves_nothing", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, robinhoodAccountKey(), rhTestAccount)
		other := rhWith(rhHoldingsPhase(), rhRoutes{"GET " + rhTestAccounts: {rhAccountRoute(rhOtherVenue)}})
		in := e.killSwitchInputs(rhHoldingsPhase(), other)
		plan := planKillSwitchClose(in)
		if plan.OnChainConfirmedFlat || plan.RHCloseReport.Errors["BTC"] == nil {
			t.Fatalf("report = %+v, want BTC unconfirmed", plan.RHCloseReport)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		if pos := e.position("rh-btc", "BTC"); pos == nil {
			t.Fatal("book closed on a read from another account")
		}
	})
	t.Run("a_replaced_position_is_not_closed_by_an_older_proof", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, robinhoodAccountKey(), rhTestAccount)
		in := e.killSwitchInputs(rhHoldingsPhase(), rhHoldingsPhase())
		plan := planKillSwitchClose(in)
		if !plan.OnChainConfirmedFlat {
			t.Fatalf("report = %+v, want confirmed flat", plan.RHCloseReport)
		}
		e.mu.Lock()
		e.state.Strategies["rh-btc"].Positions["BTC"].TradePositionID = "replaced"
		kept := forceCloseKillSwitchPositions(e.state.Strategies["rh-btc"], e.sc("rh-btc"), map[string]float64{"BTC": 50}, nil, nil, hlVirtualQuantitySnapshot{}, plan.RHCloseReport.VerifiedFlatCoins(), nil)
		e.mu.Unlock()
		if len(kept) != 1 || e.position("rh-btc", "BTC") == nil {
			t.Fatalf("kept = %v, want the replaced book kept", kept)
		}
	})
}

func TestRobinhoodAccountBindingCircuitBreaker(t *testing.T) {
	t.Run("held_book_keeps_its_pending_close", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
		e.latchCB("rh-btc")
		e.mu.Lock()
		s := e.state.Strategies["rh-btc"]
		setRobinhoodCircuitBreakerPending(ptrStrategyConfig(e.sc("rh-btc")), s)
		e.mu.Unlock()
		for _, held := range []string{"", "0.5"} {
			var rows []interface{}
			if held != "" {
				rows = append(rows, rhHeld("BTC", held))
			}
			e.runCBPhases(rhClosePhase(rhSellFilled("0.5", "97"), "0.5", "0.5"), rhHoldingsPhase(rows...), rhHoldingsPhase(rows...))
			if p := e.pendingRH("rh-btc"); p == nil {
				t.Fatalf("holdings %q: pending cleared, want kept", held)
			}
			if pos := e.position("rh-btc", "BTC"); pos == nil || pos.Quantity != 0.5 {
				t.Fatalf("holdings %q: book = %+v, want intact", held, pos)
			}
		}
		if len(e.allPostQuantities()) != 0 {
			t.Fatal("a held book was sold")
		}
	})
	t.Run("shared_coin_failure_finalizer_keeps_an_account_held_pending_close", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setBoundPosition("rh-a", "BTC", 0.5, "", "")
		e.setBoundPosition("rh-b", "BTC", 0.2, robinhoodAccountKey(), rhTestAccount)
		e.latchCB("rh-a")
		e.mu.Lock()
		setRobinhoodCircuitBreakerPending(ptrStrategyConfig(e.sc("rh-a")), e.state.Strategies["rh-a"])
		e.mu.Unlock()
		e.runCBPhases(rhRoutes{}, rhHoldingsPhase(rhHeld("BTC", "0.7")))
		if p := e.pendingRH("rh-a"); p == nil {
			t.Fatal("the shared-coin finalizer cleared a pending close held for account identity")
		}
	})
	t.Run("stuck_breaker_never_reconciles_a_held_flat_book", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, robinhoodAccountKey(), rhOtherVenue)
		e.latchCB("rh-btc")
		e.runCBPhases(rhRoutes{}, rhHoldingsPhase())
		if pos := e.position("rh-btc", "BTC"); pos == nil || len(e.dbTrades("rh-btc")) != 0 {
			t.Fatalf("book = %+v, want kept", pos)
		}
	})
}

func ptrStrategyConfig(sc StrategyConfig) *StrategyConfig { return &sc }

func TestRobinhoodAccountBindingScripts(t *testing.T) {
	t.Run("empty_holdings_keep_the_account_id", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhHoldingsPhase())
		h, err := defaultRobinhoodPositionsFetcher()
		if err != nil || h.AccountID != rhTestAccount || len(h.Positions) != 0 {
			t.Fatalf("holdings = %+v err = %v, want the account id with no positions", h, err)
		}
	})
	for name, route := range map[string]rhRoute{
		"no_id":    {"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{map[string]interface{}{"id": ""}}}},
		"two_rows": {"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{map[string]interface{}{"id": "a"}, map[string]interface{}{"id": "b"}}}},
		"failed":   {"status": 500, "json": map[string]interface{}{"detail": "down"}},
	} {
		t.Run("account_read_"+name+"_fails_the_whole_read", func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setRoutes(rhWith(rhHoldingsPhase(rhHeld("BTC", "0.5")), rhRoutes{"GET " + rhTestAccounts: {route}}))
			if h, err := defaultRobinhoodPositionsFetcher(); err == nil {
				t.Fatalf("holdings = %+v, want an error", h)
			}
		})
	}
	t.Run("emergency_close_refuses_another_account_before_already_flat", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhHoldingsPhase())
		res, _, err := RunRobinhoodClose(robinhoodLiveCloseScript, "BTC", "44444444-4444-4444-8444-444444444444", 0.5, rhOtherVenue)
		if err == nil || res == nil || res.OrderOutcome != robinhoodOutcomeNotSent || (res.Close != nil && res.Close.AlreadyFlat) {
			t.Fatalf("result = %+v err = %v, want not_sent and never already_flat", res, err)
		}
		for _, r := range e.requests() {
			if r.URL == rhTestHoldings || r.Method == "POST" && r.URL == rhTestOrders {
				t.Fatalf("request %s %s sent after an account mismatch", r.Method, r.URL)
			}
		}
	})
	t.Run("execute_sell_without_an_expected_account_sends_nothing", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.5")))
		res, _, err := RunRobinhoodExecute("shared_scripts/check_robinhood.py", "BTC", "sell", 0, 0.5, "55555555-5555-4555-8555-555555555555", RobinhoodSellArgs{CloseKind: robinhoodCloseKindWhole})
		if err == nil || res == nil || res.OrderOutcome != robinhoodOutcomeNotSent {
			t.Fatalf("result = %+v err = %v, want not_sent", res, err)
		}
		if len(e.requests()) != 0 {
			t.Fatalf("requests = %+v, want none", e.requests())
		}
	})
}

func TestRobinhoodAccountBindingHeldBookAlerts(t *testing.T) {
	reset := func() {
		robinhoodHeldBookAlerts.Range(func(k, _ interface{}) bool { robinhoodHeldBookAlerts.Delete(k); return true })
	}
	t.Run("startup_and_first_cycle_send_one_owner_alert", func(t *testing.T) {
		reset()
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
		now := time.Now()
		notifyRobinhoodHeldBooks(e.state, e.cfg.Strategies, &sync.RWMutex{}, e.notifier, now)
		notifyRobinhoodHeldBooks(e.state, e.cfg.Strategies, &e.mu, e.notifier, now.Add(time.Minute))
		if n := e.dmCount("ROBINHOOD BOOK HELD"); n != 1 {
			t.Fatalf("held-book DMs = %d, want one", n)
		}
		e.mock.mu.Lock()
		msg := e.mock.dms[len(e.mock.dms)-1].content
		e.mock.mu.Unlock()
		for _, want := range []string{"[rh-btc]", "0.5 BTC", "unbound", commandPrefix + robinhoodAccountCommandName, robinhoodAccountActionBind, robinhoodAccountActionRelease} {
			if !strings.Contains(msg, want) {
				t.Fatalf("alert %q misses %q", msg, want)
			}
		}
		if strings.Contains(msg, "offline-user") {
			t.Fatal("alert prints the login name")
		}
	})
	t.Run("no_owner_sends_no_dm", func(t *testing.T) {
		reset()
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
		noOwner := NewMultiNotifier(notifierBackend{notifier: e.mock, channels: map[string]string{"spot": "chan-1"}})
		msgs := collectRobinhoodHeldBookAlerts(e.state, e.cfg.Strategies, &e.mu, time.Now())
		reset()
		notifyRobinhoodHeldBooks(e.state, e.cfg.Strategies, &e.mu, noOwner, time.Now())
		if len(msgs) != 1 || e.dmCount("ROBINHOOD BOOK HELD") != 0 {
			t.Fatalf("msgs = %d DMs = %d, want the CRITICAL line only", len(msgs), e.dmCount("ROBINHOOD BOOK HELD"))
		}
	})
}

func rhAccountOpts(strategy, action string) []*discordgo.ApplicationCommandInteractionDataOption {
	return []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "strategy", Type: discordgo.ApplicationCommandOptionString, Value: strategy},
		{Name: "action", Type: discordgo.ApplicationCommandOptionString, Value: action},
	}
}

func TestRobinhoodAccountBindingOwnerActions(t *testing.T) {
	t.Run("command_refusals", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
		ss := &StatusServer{state: e.state, mu: &e.mu, stateDB: e.store, strategies: e.cfg.Strategies}
		d := &DiscordNotifier{ownerID: "owner", ss: ss}
		opts := rhAccountOpts("rh-btc", robinhoodAccountActionRelease)
		for name, msg := range map[string]string{
			"intruder":   d.resolveRobinhoodAccountCommand("intruder", "", opts),
			"guild":      d.resolveRobinhoodAccountCommand("owner", "guild-1", opts),
			"no_owner":   (&DiscordNotifier{ss: ss}).resolveRobinhoodAccountCommand("owner", "", opts),
			"no_storage": (&DiscordNotifier{ownerID: "owner", ss: &StatusServer{state: e.state, mu: &e.mu, strategies: e.cfg.Strategies}}).resolveRobinhoodAccountCommand("owner", "", opts),
			"bad_action": d.resolveRobinhoodAccountCommand("owner", "", rhAccountOpts("rh-btc", "sell-all")),
			"unknown":    d.resolveRobinhoodAccountCommand("owner", "", rhAccountOpts("rh-none", robinhoodAccountActionBind)),
		} {
			if !strings.Contains(msg, "owner") && !strings.Contains(msg, "DM") && !strings.Contains(msg, "not ready") && !strings.Contains(msg, "refused") {
				t.Errorf("%s: %q, want a refusal", name, msg)
			}
		}
		if pos := e.position("rh-btc", "BTC"); pos == nil || pos.AccountKey != "" {
			t.Fatalf("book changed: %+v", pos)
		}
		if len(commandPrefix+robinhoodAccountCommandName) > 32 {
			t.Fatalf("command name %q is over the 32-character Discord limit", commandPrefix+robinhoodAccountCommandName)
		}
	})
	t.Run("prepare_refuses_a_current_book_and_an_unrelated_unresolved_attempt", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setBoundPosition("rh-a", "BTC", 0.5, robinhoodAccountKey(), rhTestAccount)
		e.setBoundPosition("rh-b", "BTC", 0.2, "", "")
		d := e.resolveDeps(e.holdingsFetcher(), rhMark(50))
		if res, msg := prepareRobinhoodAccountResolution(d, "rh-a", robinhoodAccountActionBind); res != nil || !strings.Contains(msg, "already bound") {
			t.Fatalf("current book: %q", msg)
		}
		e.seedSettlingAttempt("rh-a", &robinhoodOrderAttempt{AttemptID: "rh-000000000000000000000004", AccountKey: robinhoodAccountKey(), Symbol: "BTC", Origin: robinhoodOriginSignal, Side: "buy",
			RefID: "66666666-6666-4666-8666-666666666666", Status: robinhoodAttemptUnknown, RequestJSON: `{"version":1}`, CreatedAtMs: 1, UpdatedAtMs: 1})
		d = e.resolveDeps(e.holdingsFetcher(), rhMark(50))
		for _, action := range []string{robinhoodAccountActionBind, robinhoodAccountActionRelease} {
			if res, msg := prepareRobinhoodAccountResolution(d, "rh-b", action); res != nil || !strings.Contains(msg, "is unresolved") {
				t.Fatalf("%s with an unresolved attempt: %q", action, msg)
			}
		}
	})

	type outcome struct {
		name   string
		book   [2]string
		rows   []interface{}
		action string
		mark   float64
		setup  func(e *rhLiveEnv, r *robinhoodAccountResolution) func()
		want   string
	}
	cases := []outcome{
		{name: "bind_ok", rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionBind, want: "Bound"},
		{name: "bind_key_only_ok", book: [2]string{"current", ""}, rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionBind, want: "Bound"},
		{name: "bind_insufficient", rows: []interface{}{rhHeld("BTC", "0.4")}, action: robinhoodAccountActionBind, want: "below the"},
		{name: "bind_stored_venue_mismatch", book: [2]string{"", rhOtherVenue}, rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionBind, want: "is bound to venue account"},
		{name: "bind_failed_read", rows: nil, action: robinhoodAccountActionBind, want: "read of the configured account failed",
			setup: func(e *rhLiveEnv, _ *robinhoodAccountResolution) func() { return nil }},
		{name: "busy_lock", rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionBind, want: "in progress",
			setup: func(e *rhLiveEnv, r *robinhoodAccountResolution) func() {
				release, _ := tryLockRobinhoodCoin(r.CurrentKey, r.Coin)
				return release
			}},
		{name: "changed_snapshot", rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionBind, want: "changed while you were confirming",
			setup: func(e *rhLiveEnv, _ *robinhoodAccountResolution) func() {
				e.mu.Lock()
				e.state.Strategies["rh-btc"].Positions["BTC"].Quantity = 0.45
				e.mu.Unlock()
				return nil
			}},
		{name: "release_ok", rows: nil, action: robinhoodAccountActionRelease, mark: 50, want: "Released"},
		{name: "release_holdings_on_account", rows: []interface{}{rhHeld("BTC", "0.5")}, action: robinhoodAccountActionRelease, mark: 50, want: "above the"},
		{name: "release_zero_mark", rows: nil, action: robinhoodAccountActionRelease, mark: 0, want: "no finite positive current mark"},
		{name: "release_nan_mark", rows: nil, action: robinhoodAccountActionRelease, mark: math.NaN(), want: "no finite positive current mark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			key := tc.book[0]
			if key == "current" {
				key = robinhoodAccountKey()
			}
			e.setBoundPosition("rh-btc", "BTC", 0.5, key, tc.book[1])
			fetcher := e.holdingsFetcher(tc.rows...)
			if tc.name == "bind_failed_read" {
				fetcher = e.phasedFetcher(rhRoutes{"GET " + rhTestHoldings: {{"status": 500, "json": map[string]interface{}{"detail": "down"}}}})
			}
			d := e.resolveDeps(fetcher, rhMark(tc.mark))
			res, prompt := prepareRobinhoodAccountResolution(d, "rh-btc", tc.action)
			if res == nil {
				t.Fatalf("prepare refused: %q", prompt)
			}
			if tc.setup != nil {
				if release := tc.setup(e, res); release != nil {
					defer release()
				}
			}
			before := e.position("rh-btc", "BTC")
			cash := e.state.Strategies["rh-btc"].Cash
			msg := applyRobinhoodAccountResolution(d, "owner-1", *res, time.Now())
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("apply = %q, want %q", msg, tc.want)
			}
			if posts := e.orderPosts(); len(posts) != 0 {
				t.Fatalf("an owner action sent orders: %+v", posts)
			}
			e.reopen()
			switch tc.want {
			case "Bound":
				if got := e.dbBinding("rh-btc", "BTC"); got.Key != robinhoodAccountKey() || got.Venue != rhTestAccount || got.Source != "operator:owner-1" {
					t.Fatalf("db binding = %+v", got)
				}
			case "Released":
				if e.dbPositionCount("rh-btc") != 0 || e.position("rh-btc", "BTC") != nil {
					t.Fatal("released book still stored")
				}
				dbt := e.dbTrades("rh-btc")
				if len(dbt) != 1 || !dbt[0].IsClose || dbt[0].Price != 50 || !strings.Contains(dbt[0].Details, "robinhood_account_released") || !strings.Contains(dbt[0].Details, "owner-1") || strings.Contains(dbt[0].Details, "Verified flat") {
					t.Fatalf("release trades = %+v", dbt)
				}
				if got := e.state.Strategies["rh-btc"].Cash; math.Abs(got-(cash+0.5*50)) > 1e-9 {
					t.Fatalf("cash = %v, want %v", got, cash+25)
				}
				for _, a := range drainModelOnlyCloseAlerts() {
					if a.StrategyID == "rh-btc" {
						t.Fatalf("release queued a model-only close alert %+v", a)
					}
				}
			default:
				after := e.position("rh-btc", "BTC")
				if after == nil || after.AccountKey != before.AccountKey || after.VenueAccountID != before.VenueAccountID || len(e.dbTrades("rh-btc")) != 0 {
					t.Fatalf("refused action changed the book: %+v", after)
				}
			}
		})
	}

	for _, action := range []string{robinhoodAccountActionBind, robinhoodAccountActionRelease} {
		t.Run("save_failure_leaves_everything_unchanged_"+action, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setBoundPosition("rh-btc", "BTC", 0.5, "", "")
			rows := []interface{}{rhHeld("BTC", "0.5")}
			if action == robinhoodAccountActionRelease {
				rows = nil
			}
			d := e.resolveDeps(e.holdingsFetcher(rows...), rhMark(50))
			res, prompt := prepareRobinhoodAccountResolution(d, "rh-btc", action)
			if res == nil {
				t.Fatalf("prepare: %q", prompt)
			}
			s := e.state.Strategies["rh-btc"]
			before := *e.position("rh-btc", "BTC")
			cash, trades, closed, risk := s.Cash, len(s.TradeHistory), len(s.ClosedPositions), s.RiskState.ConsecutiveLosses
			storeCommitHook = func(storageRole) error { return fmt.Errorf("injected save failure") }
			msg := applyRobinhoodAccountResolution(d, "owner-1", *res, time.Now())
			storeCommitHook = nil
			if !strings.Contains(msg, "Nothing was changed") {
				t.Fatalf("apply = %q", msg)
			}
			after := e.position("rh-btc", "BTC")
			if after == nil || after.AccountKey != before.AccountKey || after.Quantity != before.Quantity || s.Cash != cash || len(s.TradeHistory) != trades || len(s.ClosedPositions) != closed || s.RiskState.ConsecutiveLosses != risk {
				t.Fatalf("memory changed after a failed save: %+v", after)
			}
			for _, a := range drainModelOnlyCloseAlerts() {
				if a.StrategyID == "rh-btc" {
					t.Fatalf("alert queued before commit: %+v", a)
				}
			}
			e.reopen()
			if got := e.dbBinding("rh-btc", "BTC"); got != (rhBindRow{"", "", robinhoodBindingSourceBackfill}) && got != (rhBindRow{}) {
				t.Fatalf("db binding = %+v after a failed save", got)
			}
			if len(e.dbTrades("rh-btc")) != 0 {
				t.Fatal("trades written by a failed save")
			}
		})
	}

	t.Run("final_validation_uses_the_peer_total_changed_during_the_read", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setBoundPosition("rh-a", "BTC", 0.5, "", "")
		e.setBoundPosition("rh-b", "BTC", 0.2, robinhoodAccountKey(), rhTestAccount)
		inner := e.holdingsFetcher(rhHeld("BTC", "0.7"))
		fetcher := func() (RobinhoodHoldings, error) {
			e.mu.Lock()
			e.state.Strategies["rh-b"].Positions["BTC"].Quantity = 0.3
			e.mu.Unlock()
			return inner()
		}
		d := e.resolveDeps(fetcher, nil)
		res, prompt := prepareRobinhoodAccountResolution(d, "rh-a", robinhoodAccountActionBind)
		if res == nil {
			t.Fatalf("prepare: %q", prompt)
		}
		if msg := applyRobinhoodAccountResolution(d, "owner-1", *res, time.Now()); !strings.Contains(msg, "below the 0.8") {
			t.Fatalf("apply = %q, want the new peer total used", msg)
		}
	})
	t.Run("a_bind_in_progress_blocks_order_dispatch_for_the_coin", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setBoundPosition("rh-a", "BTC", 0.5, "", "")
		e.setBoundPosition("rh-b", "BTC", 0.2, robinhoodAccountKey(), rhTestAccount)
		entered := make(chan struct{})
		proceed := make(chan struct{})
		inner := e.holdingsFetcher(rhHeld("BTC", "0.7"))
		fetcher := func() (RobinhoodHoldings, error) {
			close(entered)
			<-proceed
			return inner()
		}
		d := e.resolveDeps(fetcher, nil)
		res, prompt := prepareRobinhoodAccountResolution(d, "rh-a", robinhoodAccountActionBind)
		if res == nil {
			t.Fatalf("prepare: %q", prompt)
		}
		done := make(chan string, 1)
		go func() { done <- applyRobinhoodAccountResolution(d, "owner-1", *res, time.Now()) }()
		<-entered
		if trades, _ := e.signal("rh-b", -1, 0, 99); trades != 0 || len(e.attemptRows()) != 0 {
			t.Fatal("a sell was reserved while the coin lock was held by a bind")
		}
		second := make(chan string, 1)
		go func() { second <- applyRobinhoodAccountResolution(d, "owner-1", *res, time.Now()) }()
		if msg := <-second; !strings.Contains(msg, "in progress") {
			t.Fatalf("second bind = %q, want busy", msg)
		}
		close(proceed)
		if msg := <-done; !strings.HasPrefix(msg, "Bound") {
			t.Fatalf("bind = %q", msg)
		}
	})
}
