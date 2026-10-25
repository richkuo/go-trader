//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func rhPairRules(minSize, increment string) rhRoutes {
	return rhRoutes{"GET " + rhTestPairs: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
		map[string]interface{}{"id": "pair-btc", "asset_currency": map[string]interface{}{"code": "BTC"}, "quote_currency": map[string]interface{}{"code": "USD"},
			"min_order_size": minSize, "min_order_quantity_increment": increment, "max_order_size": "20"},
	}}}}}
}

func rhSizedSellRoutes(holding rhRoutes, filled string, extra ...rhRoutes) rhRoutes {
	routes := rhWith(rhWith(rhPrepRoutes(), holding), rhRoutes{
		"POST " + rhTestOrders:             {rhAccepted("sell")},
		"GET " + rhOrderURL(rhTestOrderID): {rhOrder("filled", "sell", filled, "99")},
	})
	for _, x := range extra {
		routes = rhWith(routes, x)
	}
	return routes
}

func (e *rhLiveEnv) postQuantities() []float64 {
	e.t.Helper()
	var out []float64
	for _, p := range e.orderPosts() {
		var body struct {
			Quantity json.Number `json:"quantity"`
		}
		if err := json.Unmarshal(p.Body, &body); err != nil {
			e.t.Fatalf("order body %s: %v", p.Body, err)
		}
		q, err := body.Quantity.Float64()
		if err != nil {
			e.t.Fatalf("order quantity %q: %v", body.Quantity, err)
		}
		out = append(out, q)
	}
	return out
}

func (e *rhLiveEnv) dmCount(marker string) int {
	e.mock.mu.Lock()
	defer e.mock.mu.Unlock()
	n := 0
	for _, d := range e.mock.dms {
		if strings.Contains(d.content, marker) {
			n++
		}
	}
	return n
}

func (e *rhLiveEnv) evidence(attemptID string) robinhoodOrderEvidenceRecord {
	e.t.Helper()
	var raw string
	if err := e.store.primary().db.QueryRow(`SELECT evidence_json FROM robinhood_order_attempts WHERE attempt_id = ?`, attemptID).Scan(&raw); err != nil {
		e.t.Fatal(err)
	}
	var ev robinhoodOrderEvidenceRecord
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			e.t.Fatalf("evidence %s: %v", raw, err)
		}
	}
	return ev
}

func rhSamePosts(t *testing.T, got []float64, want ...float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order POST quantities = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order POST quantities = %v, want %v", got, want)
		}
	}
}

func TestRobinhoodLiveOrderSellCappedByTheAccountBooksTheFillAndKeepsTheRemainder(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	var logBuf bytes.Buffer
	e.logger = &StrategyLogger{stratID: "rh-btc", writer: &logBuf}
	e.setPosition("rh-btc", "BTC", 0.5, 90)
	e.setRoutes(rhSizedSellRoutes(rhSellHolding("0.5", "0.3"), "0.3"))
	if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	rhSamePosts(t, e.postQuantities(), 0.3)
	s := e.state.Strategies["rh-btc"]
	pos := s.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.2) > 1e-12 || pos.InitialQuantity != 0.5 {
		t.Fatalf("position = %+v, want the 0.2 remainder kept", pos)
	}
	dbt := e.dbTrades("rh-btc")
	if len(dbt) != 1 || math.Abs(dbt[0].Quantity-0.3) > 1e-12 || dbt[0].Price != 99 || !dbt[0].IsClose {
		t.Fatalf("db trades = %+v, want one 0.3 close at 99", dbt)
	}
	row := e.onlyAttempt()
	if row.Status != robinhoodAttemptSettled || row.SettlementSeq != 1 || math.Abs(row.AppliedQty-0.3) > 1e-12 {
		t.Fatalf("attempt = %+v, want settled once with 0.3 applied", row)
	}
	ev := e.evidence(row.ID)
	if ev.Sizing == nil || ev.Sizing.SentQuantity != 0.3 || ev.Sizing.AskedQuantity != 0.5 || !ev.Sizing.Capped || !ev.Sizing.AccountShort || ev.RequestedQuantity != 0.3 {
		t.Fatalf("evidence = %+v sizing %+v, want the sent 0.3 saved apart from the asked 0.5", ev, ev.Sizing)
	}
	if !strings.Contains(logBuf.String(), "capped: requested 0.5, available 0.3, sent 0.3") {
		t.Fatalf("log %q lacks the capped WARN line", logBuf.String())
	}
	if n := e.dmCount("ROBINHOOD BOOK SHORTFALL"); n != 1 {
		t.Fatalf("shortfall DMs = %d, want 1", n)
	}

	clearLiveExecThrottle(e.sc("rh-btc"), directionClose, "BTC")
	e.setRoutes(rhSizedSellRoutes(rhSellHolding("0.2", "0"), "0.2"))
	if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
		t.Fatalf("a sell with nothing available booked %d trade(s)", trades)
	}
	if posts := e.orderPosts(); len(posts) != 0 {
		t.Fatalf("a sell with nothing available sent %d order(s)", len(posts))
	}
	rows := e.attemptRows()
	if len(rows) != 2 || rows[1].Status != robinhoodAttemptNoFill {
		t.Fatalf("attempts = %+v, want the second ended no_fill", rows)
	}
	if e.failedAlerts() == 0 {
		t.Fatal("a refused sell must send the live-order-failed alert")
	}
	if pos := s.Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.2) > 1e-12 || len(e.dbTrades("rh-btc")) != 1 {
		t.Fatalf("position = %+v, want the book unchanged by the refused sell", pos)
	}
	if n := e.dmCount("ROBINHOOD BOOK SHORTFALL"); n != 1 {
		t.Fatalf("shortfall DMs = %d, want still 1", n)
	}
}

func TestRobinhoodLiveOrderSoleOwnerWholeCloseSweepsSubMinimumDust(t *testing.T) {
	t.Run("sole_owner", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhSizedSellRoutes(rhSellHolding("0.5004", "0.5004"), "0.5004", rhPairRules("0.001", "0.00000001")))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		rhSamePosts(t, e.postQuantities(), 0.5004)
		s := e.state.Strategies["rh-btc"]
		if len(s.Positions) != 0 {
			t.Fatalf("positions = %+v, want closed", s.Positions)
		}
		if len(s.TradeHistory) != 1 {
			t.Fatalf("trades = %+v, want one close", s.TradeHistory)
		}
		tr := s.TradeHistory[0]
		if tr.Quantity != 0.5004 || tr.Price != 99 {
			t.Fatalf("trade = %+v, want 0.5004 at 99", tr)
		}
		want := 1000 + 0.5004*99 - tr.ExchangeFee
		if math.Abs(s.Cash-want) > 1e-9 {
			t.Fatalf("cash = %v, want %v", s.Cash, want)
		}
		wantPnL := 0.5004*99 - 0.5*90
		if math.Abs(tr.RealizedPnL-wantPnL) > 1e-9 {
			t.Fatalf("gross realized PnL = %v, want %v", tr.RealizedPnL, wantPnL)
		}
	})
	t.Run("peer_on_the_coin", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000), rhLiveSpot("rh-btc-2", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhSizedSellRoutes(rhSellHolding("0.5004", "0.5004"), "0.5", rhPairRules("0.001", "0.00000001")))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		rhSamePosts(t, e.postQuantities(), 0.5)
		if s := e.state.Strategies["rh-btc"]; len(s.Positions) != 0 || s.TradeHistory[0].Quantity != 0.5 {
			t.Fatalf("positions = %+v trades = %+v, want a 0.5 whole close", s.Positions, s.TradeHistory)
		}
	})
}

func TestRobinhoodLiveOrderWholeCloseClosesASubIncrementBookResidue(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	e.setPosition("rh-btc", "BTC", 0.123456789, 90)
	e.setRoutes(rhSizedSellRoutes(rhSellHolding("1", "1"), "0.1234", rhPairRules("0.0001", "0.0001")))
	if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	rhSamePosts(t, e.postQuantities(), 0.1234)
	s := e.state.Strategies["rh-btc"]
	if len(s.Positions) != 0 {
		t.Fatalf("positions = %+v, want the sub-increment residue closed", s.Positions)
	}
	tr := s.TradeHistory[0]
	if tr.Quantity != 0.1234 {
		t.Fatalf("trade quantity = %v, want 0.1234", tr.Quantity)
	}
	wantGross := 0.1234*99 - 0.123456789*90
	if math.Abs(tr.RealizedPnL-wantGross) > 1e-9 {
		t.Fatalf("gross realized PnL = %v, want %v (proceeds on 0.1234, cost on the whole book)", tr.RealizedPnL, wantGross)
	}
}

func TestRobinhoodLiveOrderPartialCloseSendsTheFlooredShare(t *testing.T) {
	cases := []struct {
		name      string
		available string
		sent      string
		post      float64
		left      float64
	}{
		{"account_covers_the_share", "1", "0.25", 0.25, 0.25},
		{"account_short_of_the_share", "0.2", "0.2", 0.2, 0.3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setPosition("rh-btc", "BTC", 0.5, 90)
			e.setRoutes(rhSizedSellRoutes(rhSellHolding(tc.available, tc.available), tc.sent))
			if trades, _ := e.signal("rh-btc", -1, 0.5, 99); trades != 1 {
				t.Fatalf("trades = %d, want 1", trades)
			}
			rhSamePosts(t, e.postQuantities(), tc.post)
			pos := e.state.Strategies["rh-btc"].Positions["BTC"]
			if pos == nil || math.Abs(pos.Quantity-tc.left) > 1e-12 {
				t.Fatalf("position = %+v, want %v left", pos, tc.left)
			}
		})
	}
}

func TestRobinhoodLiveOrderSellRefusalsSendNothingAndBookNothing(t *testing.T) {
	cases := []struct {
		name   string
		routes rhRoutes
	}{
		{"holdings_http_500", rhSizedSellRoutes(rhRoutes{"GET " + rhTestHoldings: {{"status": 500, "json": map[string]interface{}{"detail": "down"}}}}, "0.5")},
		{"coin_absent", rhSizedSellRoutes(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhAvailable("ETH", "1", "1"))}}, "0.5")},
		{"below_minimum", rhSizedSellRoutes(rhSellHolding("0.0005", "0.0005"), "0.0005", rhPairRules("0.001", "0.00000001"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			clearLiveExecThrottle(e.sc("rh-btc"), directionClose, "BTC")
			e.setPosition("rh-btc", "BTC", 0.5, 90)
			e.setRoutes(tc.routes)
			if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
				t.Fatalf("trades = %d, want 0", trades)
			}
			if posts := e.orderPosts(); len(posts) != 0 {
				t.Fatalf("a refused sell sent %d order(s)", len(posts))
			}
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptNoFill {
				t.Fatalf("attempt = %+v, want no_fill", row)
			}
			if e.failedAlerts() == 0 {
				t.Fatal("a refused sell must send the live-order-failed alert")
			}
			s := e.state.Strategies["rh-btc"]
			if pos := s.Positions["BTC"]; pos == nil || pos.Quantity != 0.5 || s.Cash != 1000 || len(e.dbTrades("rh-btc")) != 0 {
				t.Fatalf("book changed: position=%+v cash=%v", pos, s.Cash)
			}
		})
	}
}

func rhSizingJSON(asked, available, sent float64, swept bool) string {
	b, _ := json.Marshal(RobinhoodSellSizing{AskedQuantity: asked, AccountAvailable: available, SentQuantity: sent, MinOrderSize: 0.000001, QuantityIncrement: 0.00000001, CloseKind: robinhoodCloseKindWhole, SoleOwner: true, Swept: swept})
	return string(b)
}

func rhFixtureSell(fill, requested float64, sizing string) string {
	out := `{"execution":{"action":"sell","symbol":"BTC","fill":{"avg_px":99,"quantity":` + jsonNum(fill) + `,"oid":"ord-1"}},"order_outcome":"filled","order":{"id":"ord-1"},"requested_quantity":` + jsonNum(requested)
	if sizing != "" {
		out += `,"sizing":` + sizing
	}
	return out + `}`
}

func jsonNum(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRobinhoodLiveOrderSellResultWithoutSentQuantityHolds(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		keptSent float64
	}{
		{"no_sizing", rhFixtureSell(0.5, 0.5, ""), 0},
		{"asked_differs_from_the_book", rhFixtureSell(0.4, 0.4, rhSizingJSON(0.4, 0.4, 0.4, false)), 0},
		{"sent_above_the_ask_without_a_sweep", rhFixtureSell(0.5, 0.6, rhSizingJSON(0.5, 0.6, 0.6, false)), 0},
		{"fill_above_the_sent", rhFixtureSell(0.5, 0.4, rhSizingJSON(0.5, 0.4, 0.4, false)), 0.4},
		{"sent_differs_from_requested_quantity", rhFixtureSell(0.4, 0.4, rhSizingJSON(0.5, 0.5, 0.5, false)), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := rhLiveSpot("rh-btc", "BTC", 1000)
			sc.Script = "shared_scripts/testdata/robinhood_offline/print_result.py"
			e := newRHLiveEnv(t, sc)
			e.setPosition("rh-btc", "BTC", 0.5, 90)
			t.Setenv("RH_FIXTURE_STDOUT", tc.stdout)
			t.Setenv("RH_FIXTURE_EXIT", "0")
			if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
				t.Fatalf("trades = %d, want 0", trades)
			}
			s := e.state.Strategies["rh-btc"]
			if pos := s.Positions["BTC"]; pos == nil || pos.Quantity != 0.5 || s.Cash != 1000 || len(s.TradeHistory) != 0 {
				t.Fatalf("book changed: position=%+v cash=%v trades=%v", pos, s.Cash, s.TradeHistory)
			}
			row := e.onlyAttempt()
			if row.Status != robinhoodAttemptUnknown || row.ConfirmedQty != 0 {
				t.Fatalf("attempt = %+v, want unknown with no confirmed fill", row)
			}
			ev := e.evidence(row.ID)
			switch {
			case tc.keptSent == 0 && (ev.Sizing != nil || ev.RequestedQuantity != 0):
				t.Fatalf("evidence = %+v, want no trusted sent quantity saved", ev)
			case tc.keptSent > 0 && (ev.Sizing == nil || ev.Sizing.SentQuantity != tc.keptSent || ev.RequestedQuantity != tc.keptSent):
				t.Fatalf("evidence = %+v, want the valid sent quantity %v kept for recovery", ev, tc.keptSent)
			}
			if e.heldAlerts() == 0 {
				t.Fatal("a sell result without a valid sent quantity must send the hold alert")
			}
		})
	}
}

func TestRobinhoodLiveOrderSizedSellRecoversAcrossRestartWithTheSentQuantity(t *testing.T) {
	t.Run("sent_quantity_survives_restart_and_drives_the_field_match", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhWith(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.3")), rhRoutes{"POST " + rhTestOrders: {{"raise": "read_timeout"}}}))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
			t.Fatalf("trades = %d, want 0", trades)
		}
		rhSamePosts(t, e.postQuantities(), 0.3)
		row := e.onlyAttempt()
		if row.Status != robinhoodAttemptUnknown || row.VenueOrderID != "" {
			t.Fatalf("attempt = %+v, want unknown with no venue order id", row)
		}
		e.reopen()
		if ev := e.evidence(row.ID); ev.Sizing == nil || ev.Sizing.SentQuantity != 0.3 || ev.Sizing.AskedQuantity != 0.5 {
			t.Fatalf("evidence after restart = %+v, want the sent 0.3 kept", ev)
		}
		stamp := time.Now().UTC().Format(time.RFC3339)
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
				map[string]interface{}{"id": "ord-9", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.3", "created_at": stamp},
			}}}},
			"GET " + rhOrderURL("ord-9"): {{"status": 200, "json": map[string]interface{}{"id": "ord-9", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.3", "cumulative_quantity": "0.3", "average_price": "99", "created_at": stamp}}},
		}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		after := e.onlyAttempt()
		if after.Status != robinhoodAttemptSettled || after.VenueOrderID != "ord-9" || after.SettlementSeq != 1 {
			t.Fatalf("recovery must settle once by the sent quantity, got %+v", after)
		}
		if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.2) > 1e-12 {
			t.Fatalf("position = %+v, want the 0.2 remainder kept", pos)
		}
		if ev := e.evidence(row.ID); ev.Sizing == nil || ev.Sizing.SentQuantity != 0.3 {
			t.Fatalf("evidence after recovery = %+v, want the write-once sizing carried", ev)
		}
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || math.Abs(dbt[0].Quantity-0.3) > 1e-12 {
			t.Fatalf("db trades = %+v, want one 0.3 close", dbt)
		}
	})
	t.Run("unknown_sent_quantity_is_never_released", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		robinhoodOrderScriptTimeout = 1500 * time.Millisecond
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhTestHoldings: {{"sleep_s": 60, "status": 200, "json": map[string]interface{}{}}}}))
		if trades, _ := e.signal("rh-btc", -1, 0, 99); trades != 0 {
			t.Fatalf("trades = %d, want 0", trades)
		}
		robinhoodOrderScriptTimeout = 14 * time.Second
		row := e.onlyAttempt()
		if row.Status != robinhoodAttemptUnknown {
			t.Fatalf("a killed order script must leave the attempt unknown, got %+v", row)
		}
		old := time.Now().Add(-40 * time.Minute)
		if _, err := e.store.primary().db.Exec(`UPDATE robinhood_order_attempts SET created_at_ms = ? WHERE attempt_id = ?`, old.UnixMilli(), row.ID); err != nil {
			t.Fatal(err)
		}
		e.reopen()
		stamp := old.Add(time.Minute).UTC().Format(time.RFC3339)
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
				map[string]interface{}{"id": "ord-x", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.5", "created_at": stamp},
			}}}},
			"GET " + rhOrderURL("ord-x"): {{"status": 200, "json": map[string]interface{}{"id": "ord-x", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.5", "cumulative_quantity": "0.5", "average_price": "99", "created_at": stamp}}},
		}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		after := e.onlyAttempt()
		if after.Status != robinhoodAttemptUnknown {
			t.Fatalf("an attempt whose sent quantity is unknown must stay held while a same-pair sell without ref_id is in the window, got %+v", after)
		}
		if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos == nil || pos.Quantity != 0.5 || len(e.dbTrades("rh-btc")) != 0 {
			t.Fatalf("position = %+v, want the book unchanged", pos)
		}
		for _, r := range e.requests() {
			if r.URL == rhOrderURL("ord-x") {
				t.Fatalf("recovery must not adopt a same-pair order by the frozen ask, but it read %s", r.URL)
			}
		}
	})
	t.Run("version_1_sell_attempt_keeps_the_frozen_quantity_rule", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		s := e.state.Strategies["rh-btc"]
		a, err := reserveRobinhoodAttempt(e.store, e.state, s, &e.mu, "BTC", robinhoodOriginSignal, "sell", robinhoodOrderRequest{
			Signal: -1, SignalStr: "SELL", Quantity: 0.5, Price: 99, PrevPosSide: "long", PrevPosQty: 0.5,
			CloseKind: robinhoodCloseKindWhole, SoleOwner: true,
			Result: json.RawMessage(`{"symbol":"BTC","signal":-1}`),
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		err = recordRobinhoodOrderOutcomeLocked(e.store, s, a, nil, robinhoodOrderUnknown, "seed", nil, time.Now())
		e.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		var reqJSON string
		if err := e.store.primary().db.QueryRow(`SELECT request_json FROM robinhood_order_attempts WHERE attempt_id = ?`, a.AttemptID).Scan(&reqJSON); err != nil {
			t.Fatal(err)
		}
		var frozen map[string]interface{}
		if err := json.Unmarshal([]byte(reqJSON), &frozen); err != nil {
			t.Fatal(err)
		}
		if frozen["version"] != float64(robinhoodSizedSellRequestVersion) {
			t.Fatalf("a signal sell must freeze request version 2, got %v", frozen["version"])
		}
		frozen["version"] = float64(robinhoodOrderRequestVersion)
		delete(frozen, "close_kind")
		delete(frozen, "sole_owner")
		v1, _ := json.Marshal(frozen)
		if _, err := e.store.primary().db.Exec(`UPDATE robinhood_order_attempts SET request_json = ? WHERE attempt_id = ?`, string(v1), a.AttemptID); err != nil {
			t.Fatal(err)
		}
		e.reopen()
		stamp := time.Now().UTC().Format(time.RFC3339)
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
				map[string]interface{}{"id": "ord-9", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.5", "created_at": stamp},
			}}}},
			"GET " + rhOrderURL("ord-9"): {{"status": 200, "json": map[string]interface{}{"id": "ord-9", "state": "filled", "side": "sell", "currency_pair_id": "pair-btc", "quantity": "0.5", "cumulative_quantity": "0.5", "average_price": "99", "created_at": stamp}}},
		}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		row := e.onlyAttempt()
		if row.Status != robinhoodAttemptSettled || row.VenueOrderID != "ord-9" {
			t.Fatalf("a version 1 sell must resolve by its frozen quantity and settle, got %+v", row)
		}
		if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos != nil {
			t.Fatalf("position = %+v, want closed by the full fill", pos)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.5 || dbt[0].Price != 99 {
			t.Fatalf("db trades = %+v, want one 0.5 close at 99", dbt)
		}
	})
}
