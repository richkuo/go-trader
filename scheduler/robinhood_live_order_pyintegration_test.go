//go:build pyintegration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rhTestPairs    = "https://nummus.robinhood.com/currency_pairs/"
	rhTestAccounts = "https://nummus.robinhood.com/accounts/"
	rhTestQuote    = "https://api.robinhood.com/marketdata/forex/quotes/pair-btc/"
	rhTestOrders   = "https://nummus.robinhood.com/orders/"
	rhTestHoldings = "https://nummus.robinhood.com/holdings/"
	rhTestOrderID  = "ord-1"
	rhTestAccount  = "acct-1"
)

type rhRoute map[string]interface{}

type rhRoutes map[string][]rhRoute

type rhLoggedRequest struct {
	Method string          `json:"method"`
	URL    string          `json:"url"`
	Body   json.RawMessage `json:"body"`
}

type rhLiveEnv struct {
	t        *testing.T
	root     string
	dir      string
	routes   string
	log      string
	cfg      *Config
	store    *StateStore
	state    *AppState
	mu       sync.RWMutex
	mock     *mockNotifier
	notifier *MultiNotifier
	logger   *StrategyLogger
}

func rhLiveSpot(id, coin string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"momentum_pro", coin, "1h", "--mode=live"}, Capital: capital, MaxDrawdownPct: 50}
}

func newRHLiveEnv(t *testing.T, strategies ...StrategyConfig) *rhLiveEnv {
	t.Helper()
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	dir := t.TempDir()
	e := &rhLiveEnv{
		t:      t,
		root:   root,
		dir:    dir,
		routes: filepath.Join(dir, "routes.json"),
		log:    filepath.Join(dir, "requests.jsonl"),
		mock:   &mockNotifier{},
		logger: &StrategyLogger{stratID: "rh-test", writer: io.Discard},
	}
	e.notifier = NewMultiNotifier(notifierBackend{notifier: e.mock, ownerID: "owner", channels: map[string]string{"spot": "chan-1"}})
	t.Setenv("HOME", dir)
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "robinhood_offline"))
	t.Setenv("RH_OFFLINE_STUB_MODE", "routes")
	t.Setenv("RH_OFFLINE_STUB_ROUTES", e.routes)
	t.Setenv("RH_OFFLINE_STUB_LOG", e.log)
	t.Setenv("RH_OFFLINE_STUB_STDOUT_NOISE", "")
	t.Setenv("ROBINHOOD_USERNAME", "offline-user")
	t.Setenv("ROBINHOOD_PASSWORD", "offline-pass")
	t.Setenv("ROBINHOOD_TOTP_SECRET", "JBSWY3DPEHPK3PXP")
	origTimeout := robinhoodOrderScriptTimeout
	robinhoodOrderScriptTimeout = 16 * time.Second
	t.Cleanup(func() { robinhoodOrderScriptTimeout = origTimeout })
	e.cfg = &Config{DBFile: filepath.Join(dir, "state.db"), IntervalSeconds: 600, Strategies: strategies}
	e.setRoutes(rhRoutes{})
	e.open()
	t.Cleanup(func() {
		if e.store != nil {
			e.store.Close()
		}
	})
	return e
}

func (e *rhLiveEnv) open() {
	e.t.Helper()
	layout, err := resolveStorageLayout(e.cfg)
	if err != nil {
		e.t.Fatalf("layout: %v", err)
	}
	ident, err := buildStorageIdentityMap(e.cfg, layout)
	if err != nil {
		e.t.Fatalf("identity: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		e.t.Fatalf("open store: %v", err)
	}
	state, _, err := LoadStateWithStore(e.cfg, store)
	if err != nil {
		e.t.Fatalf("load state: %v", err)
	}
	for _, sc := range e.cfg.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadRobinhoodOrderGuards(state, e.cfg.Strategies); err != nil {
		e.t.Fatalf("load robinhood guards: %v", err)
	}
	e.store, e.state = store, state
}

func (e *rhLiveEnv) reopen() {
	e.t.Helper()
	if err := e.store.Close(); err != nil {
		e.t.Fatalf("close store: %v", err)
	}
	e.open()
}

func (e *rhLiveEnv) setRoutes(routes rhRoutes) {
	e.t.Helper()
	b, err := json.Marshal(routes)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.routes, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.log, nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *rhLiveEnv) requests() []rhLoggedRequest {
	e.t.Helper()
	f, err := os.Open(e.log)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	var out []rhLoggedRequest
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var r rhLoggedRequest
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			e.t.Fatalf("request log line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func (e *rhLiveEnv) orderPosts() []rhLoggedRequest {
	var out []rhLoggedRequest
	for _, r := range e.requests() {
		if r.Method == "POST" && r.URL == rhTestOrders {
			out = append(out, r)
		}
	}
	return out
}

func (e *rhLiveEnv) sc(id string) StrategyConfig {
	for _, sc := range e.cfg.Strategies {
		if sc.ID == id {
			return sc
		}
	}
	e.t.Fatalf("no strategy %s", id)
	return StrategyConfig{}
}

func (e *rhLiveEnv) deps() robinhoodEmergencyDeps {
	return robinhoodEmergencyDeps{store: e.store, state: e.state, mu: &e.mu, cfgs: e.cfg.Strategies, notifier: e.notifier}
}

func (e *rhLiveEnv) setPosition(id, coin string, qty, avgCost float64) {
	e.t.Helper()
	e.mu.Lock()
	s := e.state.Strategies[id]
	s.Positions[coin] = &Position{Symbol: coin, Quantity: qty, InitialQuantity: qty, AvgCost: avgCost, Side: "long", OwnerStrategyID: id, TradePositionID: id + ":" + coin + ":seed", OpenedAt: time.Now().UTC()}
	err := e.store.SaveStrategyBook(s)
	e.mu.Unlock()
	if err != nil {
		e.t.Fatalf("seed position: %v", err)
	}
}

func (e *rhLiveEnv) signal(id string, signal int, closeFraction, price float64) (int, string) {
	e.t.Helper()
	sc := e.sc(id)
	s := e.state.Strategies[id]
	db, err := e.store.dbForStrategy(id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.mu.RLock()
	cash := s.Cash
	reconcile := s.CashReconcileRequired
	var qty float64
	var side string
	if pos := s.Positions[robinhoodSymbol(sc.Args)]; pos != nil {
		qty, side = pos.Quantity, pos.Side
	}
	e.mu.RUnlock()
	result := &RobinhoodResult{Symbol: robinhoodSymbol(sc.Args), Signal: signal, Price: price, Mode: "live", Platform: "robinhood"}
	result.CloseFraction = closeFraction
	signalStr := "BUY"
	if signal < 0 {
		signalStr = "SELL"
	}
	return executeRobinhoodLiveSignal(e.store, e.state, &e.mu, sc, s, db, result, signalStr, price, cash, reconcile, qty, side, HurstGateDecision{}, e.cfg, e.notifier, e.logger)
}

type rhAttemptRow struct {
	ID, StrategyID, Symbol, Origin, Side, RefID, VenueOrderID, Status, HoldReason string
	ConfirmedQty, AppliedQty                                                      float64
	SettlementSeq, CreatedAtMs                                                    int64
}

func (e *rhLiveEnv) attemptRows() []rhAttemptRow {
	e.t.Helper()
	rows, err := e.store.primary().db.Query(`SELECT attempt_id, strategy_id, symbol, origin, side, ref_id, venue_order_id, status, hold_reason, confirmed_qty, applied_qty, settlement_seq, created_at_ms FROM robinhood_order_attempts ORDER BY created_at_ms, attempt_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []rhAttemptRow
	for rows.Next() {
		var r rhAttemptRow
		if err := rows.Scan(&r.ID, &r.StrategyID, &r.Symbol, &r.Origin, &r.Side, &r.RefID, &r.VenueOrderID, &r.Status, &r.HoldReason, &r.ConfirmedQty, &r.AppliedQty, &r.SettlementSeq, &r.CreatedAtMs); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *rhLiveEnv) onlyAttempt() rhAttemptRow {
	e.t.Helper()
	rows := e.attemptRows()
	if len(rows) != 1 {
		e.t.Fatalf("want one attempt row, got %+v", rows)
	}
	return rows[0]
}

func (e *rhLiveEnv) dbTrades(id string) []Trade {
	e.t.Helper()
	rows, err := e.store.primary().db.Query(`SELECT side, quantity, price, exchange_order_id, is_close, details FROM trades WHERE strategy_id = ? ORDER BY rowid`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var tr Trade
		var isClose int
		if err := rows.Scan(&tr.Side, &tr.Quantity, &tr.Price, &tr.ExchangeOrderID, &isClose, &tr.Details); err != nil {
			e.t.Fatal(err)
		}
		tr.IsClose = isClose != 0
		out = append(out, tr)
	}
	return out
}

func (e *rhLiveEnv) heldAlerts() int {
	e.mock.mu.Lock()
	defer e.mock.mu.Unlock()
	n := 0
	for _, d := range e.mock.dms {
		if strings.Contains(d.content, "ROBINHOOD ORDER UNRESOLVED") {
			n++
		}
	}
	return n
}

func (e *rhLiveEnv) failedAlerts() int {
	e.mock.mu.Lock()
	defer e.mock.mu.Unlock()
	n := 0
	for _, d := range e.mock.dms {
		if strings.Contains(d.content, "LIVE ORDER FAILED") {
			n++
		}
	}
	return n
}

func rhPrepRoutes() rhRoutes {
	return rhRoutes{
		"GET " + rhTestPairs: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
			map[string]interface{}{"id": "pair-btc", "asset_currency": map[string]interface{}{"code": "BTC"}, "quote_currency": map[string]interface{}{"code": "USD"}},
		}}}},
		"GET " + rhTestAccounts: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{map[string]interface{}{"id": rhTestAccount}}}}},
		"GET " + rhTestQuote:    {{"status": 200, "json": map[string]interface{}{"ask_price": "100.00", "bid_price": "99.00"}}},
	}
}

func rhOrderBody(id, state, side, cumulative string, average interface{}, refID string) map[string]interface{} {
	body := map[string]interface{}{"id": id, "state": state, "side": side, "currency_pair_id": "pair-btc", "cumulative_quantity": cumulative, "average_price": average}
	if refID != "" {
		body["ref_id"] = refID
	}
	return body
}

func rhOrder(state, side, cumulative string, average interface{}) rhRoute {
	return rhRoute{"status": 200, "json": rhOrderBody(rhTestOrderID, state, side, cumulative, average, "")}
}

func rhAccepted(side string) rhRoute {
	return rhRoute{"status": 201, "json": rhOrderBody(rhTestOrderID, "unconfirmed", side, "0", nil, "")}
}

func rhHoldings(rows ...interface{}) rhRoute {
	if rows == nil {
		rows = []interface{}{}
	}
	return rhRoute{"status": 200, "json": map[string]interface{}{"next": nil, "results": rows}}
}

func rhHeld(coin, qty string) map[string]interface{} {
	return map[string]interface{}{"currency": map[string]interface{}{"code": coin}, "quantity": qty}
}

func rhWith(base rhRoutes, extra rhRoutes) rhRoutes {
	out := rhRoutes{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func rhOrderURL(id string) string { return rhTestOrders + id + "/" }

func TestRobinhoodLiveOrderBuyBooksTheConfirmedVenueFill(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
		"POST " + rhTestOrders:                          {rhAccepted("buy")},
		"GET " + rhOrderURL(rhTestOrderID):              {rhOrder("queued", "buy", "0", nil), rhOrder("filled", "buy", "9.5", "100.25")},
		"POST " + rhOrderURL(rhTestOrderID) + "cancel/": {{"status": 200, "json": map[string]interface{}{}}},
	}))
	trades, detail := e.signal("rh-btc", 1, 0, 100)
	if trades != 1 || !strings.Contains(detail, "LIVE BUY") {
		t.Fatalf("trades=%d detail=%q, want one live buy", trades, detail)
	}
	s := e.state.Strategies["rh-btc"]
	pos := s.Positions["BTC"]
	if pos == nil || pos.Quantity != 9.5 || pos.AvgCost != 100.25 || pos.InitialQuantity != 9.5 {
		t.Fatalf("position = %+v, want 9.5 @ 100.25", pos)
	}
	wantCash := 1000 - 9.5*100.25
	if math.Abs(s.Cash-wantCash) > 1e-6 {
		t.Fatalf("cash = %v, want %v", s.Cash, wantCash)
	}
	row := e.onlyAttempt()
	if row.Status != robinhoodAttemptSettled || row.SettlementSeq != 1 || row.AppliedQty != 9.5 || row.VenueOrderID != rhTestOrderID {
		t.Fatalf("attempt row = %+v, want settled once", row)
	}
	posts := e.orderPosts()
	if len(posts) != 1 || !strings.Contains(string(posts[0].Body), row.RefID) {
		t.Fatalf("posts = %+v, want one POST carrying ref_id %s", posts, row.RefID)
	}
	dbt := e.dbTrades("rh-btc")
	if len(dbt) != 1 || dbt[0].Price != 100.25 || dbt[0].Quantity != 9.5 || dbt[0].ExchangeOrderID != rhTestOrderID {
		t.Fatalf("db trades = %+v, want one venue-priced buy", dbt)
	}
	before := len(s.TradeHistory)
	db, _ := e.store.dbForStrategy("rh-btc")
	a := &robinhoodOrderAttempt{AttemptID: row.ID, StrategyID: "rh-btc", Origin: robinhoodOriginSignal, Status: robinhoodAttemptSettling, VenueOrderID: rhTestOrderID, ConfirmedQty: 9.5, ConfirmedAvgPx: 100.25}
	e.mu.Lock()
	again := settleRobinhoodSignalAttemptLocked(e.store, e.sc("rh-btc"), s, db, a, true, e.cfg, e.logger)
	e.mu.Unlock()
	if again.Settled || len(s.TradeHistory) != before {
		t.Fatalf("a second settlement of the same venue order must book nothing, got %+v", again)
	}
}

func TestRobinhoodLiveOrderFullCloseSellWithPartialFillKeepsTheRemainder(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	e.setPosition("rh-btc", "BTC", 0.5, 90)
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
		"POST " + rhTestOrders:             {rhAccepted("sell")},
		"GET " + rhOrderURL(rhTestOrderID): {rhOrder("confirmed", "sell", "0", nil), rhOrder("canceled", "sell", "0.2", "99")},
	}))
	trades, _ := e.signal("rh-btc", -1, 0, 99)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := e.state.Strategies["rh-btc"].Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 || pos.InitialQuantity != 0.5 {
		t.Fatalf("position = %+v, want 0.3 remaining with InitialQuantity 0.5", pos)
	}
	dbt := e.dbTrades("rh-btc")
	if len(dbt) != 1 || dbt[0].Quantity != 0.2 || dbt[0].Price != 99 || !dbt[0].IsClose {
		t.Fatalf("db trades = %+v, want one 0.2 close at 99", dbt)
	}
	if posts := e.orderPosts(); len(posts) != 1 || !strings.Contains(string(posts[0].Body), `"quantity": 0.5`) {
		t.Fatalf("posts = %+v, want one sell of the book quantity", posts)
	}
}

func TestRobinhoodLiveOrderRequestedPartialCloseBooksTheFilledHalf(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	e.setPosition("rh-btc", "BTC", 0.5, 90)
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
		"POST " + rhTestOrders:             {rhAccepted("sell")},
		"GET " + rhOrderURL(rhTestOrderID): {rhOrder("filled", "sell", "0.25", "99")},
	}))
	if trades, _ := e.signal("rh-btc", -1, 0.5, 99); trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := e.state.Strategies["rh-btc"].Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.25) > 1e-12 || pos.InitialQuantity != 0.5 {
		t.Fatalf("position = %+v, want half left", pos)
	}
}

func TestRobinhoodLiveOrderDefinitiveNoFillBooksNothingAndReleases(t *testing.T) {
	cases := []struct {
		name   string
		routes rhRoutes
	}{
		{"rejected", rhRoutes{"POST " + rhTestOrders: {{"status": 400, "json": map[string]interface{}{"detail": "insufficient buying power"}}}}},
		{"not_sent", rhRoutes{"POST " + rhTestOrders: {{"raise": "new_connection"}}}},
		{"zero_fill_cancel", rhRoutes{"POST " + rhTestOrders: {rhAccepted("buy")}, "GET " + rhOrderURL(rhTestOrderID): {rhOrder("canceled", "buy", "0", nil)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setRoutes(rhWith(rhPrepRoutes(), tc.routes))
			if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
				t.Fatalf("trades = %d, want 0", trades)
			}
			s := e.state.Strategies["rh-btc"]
			if s.Cash != 1000 || len(s.Positions) != 0 || len(s.TradeHistory) != 0 || len(e.dbTrades("rh-btc")) != 0 {
				t.Fatalf("book changed: cash=%v positions=%v trades=%v", s.Cash, s.Positions, s.TradeHistory)
			}
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptNoFill {
				t.Fatalf("attempt = %+v, want no_fill", row)
			}
			if e.failedAlerts() == 0 {
				t.Fatal("a definitive no-fill must send the live-order-failed alert")
			}
		})
	}
}

func TestRobinhoodLiveOrderUnresolvedOutcomeHoldsTheCoin(t *testing.T) {
	cases := []struct {
		name   string
		routes rhRoutes
		status string
	}{
		{"http_503", rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}, robinhoodAttemptUnknown},
		{"open_after_cancel", rhRoutes{
			"POST " + rhTestOrders:                          {rhAccepted("buy")},
			"GET " + rhOrderURL(rhTestOrderID):              {rhOrder("confirmed", "buy", "0", nil)},
			"POST " + rhOrderURL(rhTestOrderID) + "cancel/": {{"status": 200, "json": map[string]interface{}{}}},
		}, robinhoodAttemptOpen},
		{"excess_fill", rhRoutes{
			"POST " + rhTestOrders:             {rhAccepted("buy")},
			"GET " + rhOrderURL(rhTestOrderID): {rhOrder("filled", "buy", "50", "100")},
		}, robinhoodAttemptUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
			e.setRoutes(rhWith(rhPrepRoutes(), tc.routes))
			if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
				t.Fatalf("trades = %d, want 0", trades)
			}
			s := e.state.Strategies["rh-btc"]
			if s.Cash != 1000 || len(s.Positions) != 0 || len(e.dbTrades("rh-btc")) != 0 {
				t.Fatalf("book changed: cash=%v positions=%v", s.Cash, s.Positions)
			}
			row := e.onlyAttempt()
			if row.Status != tc.status || row.HoldReason == "" {
				t.Fatalf("attempt = %+v, want %s with a hold reason", row, tc.status)
			}
			if e.heldAlerts() == 0 {
				t.Fatal("an unresolved order must send the CRITICAL hold alert")
			}
			e.setRoutes(rhWith(rhPrepRoutes(), tc.routes))
			if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
				t.Fatalf("second signal trades = %d, want 0", trades)
			}
			if posts := e.orderPosts(); len(posts) != 0 {
				t.Fatalf("a second signal on a held coin sent %d order(s)", len(posts))
			}
		})
	}
}

func TestRobinhoodLiveOrderGoGuardRefusesMalformedResults(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		exit   string
	}{
		{"empty_fill", `{"execution":{"action":"buy","symbol":"BTC","fill":{}},"order_outcome":"filled","order":{"id":"ord-1"}}`, "0"},
		{"zero_price", `{"execution":{"action":"buy","symbol":"BTC","fill":{"avg_px":0,"quantity":1,"oid":"ord-1"}},"order_outcome":"filled","order":{"id":"ord-1"}}`, "0"},
		{"zero_quantity", `{"execution":{"action":"buy","symbol":"BTC","fill":{"avg_px":100,"quantity":0,"oid":"ord-1"}},"order_outcome":"filled","order":{"id":"ord-1"}}`, "0"},
		{"overflow_price", `{"execution":{"action":"buy","symbol":"BTC","fill":{"avg_px":1e400,"quantity":1,"oid":"ord-1"}},"order_outcome":"filled","order":{"id":"ord-1"}}`, "0"},
		{"missing_outcome", `{"execution":{"action":"buy","symbol":"BTC","fill":{"avg_px":100,"quantity":1,"oid":"ord-1"}},"order":{"id":"ord-1"}}`, "0"},
		{"no_output", ``, "1"},
		{"no_fill_with_exit_zero", `{"execution":{"action":"buy","symbol":"BTC"},"order_outcome":"rejected","error":"refused"}`, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := rhLiveSpot("rh-btc", "BTC", 1000)
			sc.Script = "shared_scripts/testdata/robinhood_offline/print_result.py"
			e := newRHLiveEnv(t, sc)
			t.Setenv("RH_FIXTURE_STDOUT", tc.stdout)
			t.Setenv("RH_FIXTURE_EXIT", tc.exit)
			if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
				t.Fatalf("trades = %d, want 0", trades)
			}
			s := e.state.Strategies["rh-btc"]
			if s.Cash != 1000 || len(s.Positions) != 0 || len(s.TradeHistory) != 0 {
				t.Fatalf("book changed: cash=%v positions=%v trades=%v", s.Cash, s.Positions, s.TradeHistory)
			}
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptUnknown {
				t.Fatalf("attempt = %+v, want unknown", row)
			}
			if e.heldAlerts() == 0 {
				t.Fatal("a malformed result must send the CRITICAL hold alert")
			}
		})
	}
}

func TestRobinhoodLiveOrderPaperStrategyStillBooksTheModeledTrade(t *testing.T) {
	sc := StrategyConfig{ID: "rh-paper", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py", Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}, Capital: 1000}
	e := newRHLiveEnv(t, sc)
	s := e.state.Strategies["rh-paper"]
	db, _ := e.store.dbForStrategy("rh-paper")
	e.mu.Lock()
	trades, _, _ := executeRobinhoodResult(sc, s, db, &RobinhoodResult{Symbol: "BTC", Signal: 1}, nil, "BUY", 100, nil, e.cfg, HurstGateDecision{}, e.logger)
	e.mu.Unlock()
	if trades != 1 || s.Positions["BTC"] == nil {
		t.Fatalf("paper buy trades=%d positions=%v, want the modeled buy", trades, s.Positions)
	}
	if len(e.attemptRows()) != 0 || len(e.requests()) != 0 {
		t.Fatal("a paper booking must make no attempt and no venue request")
	}
}

func TestRobinhoodLiveOrderBookingGuardRefusesLiveResultWithoutFill(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	s := e.state.Strategies["rh-btc"]
	db, _ := e.store.dbForStrategy("rh-btc")
	for _, er := range []*RobinhoodExecuteResult{nil, {}, {Execution: &RobinhoodExecution{Fill: &RobinhoodFill{AvgPx: 100}}}} {
		e.mu.Lock()
		trades, _, _ := executeRobinhoodResult(e.sc("rh-btc"), s, db, &RobinhoodResult{Symbol: "BTC", Signal: 1}, er, "BUY", 100, nil, e.cfg, HurstGateDecision{}, e.logger)
		e.mu.Unlock()
		if trades != 0 || s.Cash != 1000 || len(s.Positions) != 0 {
			t.Fatalf("live booking without a confirmed fill must refuse: trades=%d cash=%v", trades, s.Cash)
		}
	}
}

func TestRobinhoodLiveOrderForcedTerminationHoldsAcrossRestartAndRecoverySettlesOnce(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	robinhoodOrderScriptTimeout = 1500 * time.Millisecond
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
		"POST " + rhTestOrders:             {rhAccepted("buy")},
		"GET " + rhOrderURL(rhTestOrderID): {{"sleep_s": 60, "status": 200, "json": map[string]interface{}{}}},
	}))
	if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
		t.Fatalf("trades = %d, want 0", trades)
	}
	row := e.onlyAttempt()
	if row.Status != robinhoodAttemptUnknown {
		t.Fatalf("a killed order script must leave the attempt unknown, got %+v", row)
	}
	if s := e.state.Strategies["rh-btc"]; s.Cash != 1000 || len(s.Positions) != 0 {
		t.Fatalf("book changed after a killed order script")
	}
	robinhoodOrderScriptTimeout = 16 * time.Second

	e.reopen()
	held := robinhoodOrderStartupLines(e.state)
	if len(held) != 1 || !strings.Contains(held[0], row.ID) {
		t.Fatalf("restart must keep the attempt held, got %v", held)
	}

	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
		"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
			map[string]interface{}{"id": "ord-9", "ref_id": row.RefID, "state": "filled", "side": "buy", "currency_pair_id": "pair-btc", "quantity": "0.1", "created_at": time.Now().UTC().Format(time.RFC3339)},
		}}}},
		"GET " + rhOrderURL("ord-9"): {{"status": 200, "json": rhOrderBody("ord-9", "filled", "buy", "0.1", "101", row.RefID)}},
	}))
	recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
	after := e.onlyAttempt()
	if after.Status != robinhoodAttemptSettled || after.VenueOrderID != "ord-9" || after.SettlementSeq != 1 {
		t.Fatalf("recovery by ref_id must settle once, got %+v", after)
	}
	if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Price != 101 || dbt[0].Quantity != 0.1 {
		t.Fatalf("db trades = %+v, want one recovered buy", dbt)
	}
	recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
	if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 {
		t.Fatalf("a second recovery must add no trade, got %+v", dbt)
	}
	if found, err := e.store.HasTradeWithExchangeOrderID("rh-btc", "ord-9"); err != nil || !found {
		t.Fatalf("HasTradeWithExchangeOrderID = %v, %v", found, err)
	}
}

func (e *rhLiveEnv) seedHeldBuy() rhAttemptRow {
	e.t.Helper()
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}))
	if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
		e.t.Fatalf("seed trades = %d", trades)
	}
	row := e.onlyAttempt()
	if row.Status != robinhoodAttemptUnknown {
		e.t.Fatalf("seed attempt = %+v", row)
	}
	return row
}

func TestRobinhoodLiveOrderRecoveryOutcomes(t *testing.T) {
	t.Run("terminal_zero_fill_releases", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		row := e.seedHeldBuy()
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhTestOrders:        {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{map[string]interface{}{"id": "ord-5", "ref_id": row.RefID, "created_at": time.Now().UTC().Format(time.RFC3339)}}}}},
			"GET " + rhOrderURL("ord-5"): {{"status": 200, "json": rhOrderBody("ord-5", "canceled", "buy", "0", nil, row.RefID)}},
		}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if got := e.onlyAttempt(); got.Status != robinhoodAttemptNoFill {
			t.Fatalf("attempt = %+v, want no_fill", got)
		}
	})
	t.Run("not_found_after_settle_window_releases", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		row := e.seedHeldBuy()
		old := time.Now().Add(-40 * time.Minute).UnixMilli()
		if _, err := e.store.primary().db.Exec(`UPDATE robinhood_order_attempts SET created_at_ms = ? WHERE attempt_id = ?`, old, row.ID); err != nil {
			t.Fatal(err)
		}
		e.reopen()
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{}}}}}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if got := e.onlyAttempt(); got.Status != robinhoodAttemptNoFill {
			t.Fatalf("attempt = %+v, want no_fill after the settle window", got)
		}
	})
	t.Run("not_found_inside_settle_window_holds", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.seedHeldBuy()
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{}}}}}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if got := e.onlyAttempt(); got.Status != robinhoodAttemptUnknown {
			t.Fatalf("attempt = %+v, want still unknown", got)
		}
	})
	t.Run("failed_resolution_read_holds", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.seedHeldBuy()
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhTestOrders: {{"status": 500, "json": map[string]interface{}{"detail": "down"}}}}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if got := e.onlyAttempt(); got.Status != robinhoodAttemptUnknown {
			t.Fatalf("attempt = %+v, want still unknown", got)
		}
	})
	t.Run("account_change_holds_without_venue_call", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.seedHeldBuy()
		t.Setenv("ROBINHOOD_USERNAME", "another-user")
		e.setRoutes(rhPrepRoutes())
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if got := e.onlyAttempt(); got.Status != robinhoodAttemptUnknown {
			t.Fatalf("attempt = %+v, want still unknown", got)
		}
		if reqs := e.requests(); len(reqs) != 0 {
			t.Fatalf("an account change must make no venue call, got %+v", reqs)
		}
	})
}

func (e *rhLiveEnv) killSwitchInputs(phases ...rhRoutes) KillSwitchCloseInputs {
	var rhLive []StrategyConfig
	for _, sc := range e.cfg.Strategies {
		if isRobinhoodLiveSpot(sc) {
			rhLive = append(rhLive, sc)
		}
	}
	calls := 0
	fetcher := func() ([]RobinhoodPosition, error) {
		if calls < len(phases) {
			e.setRoutes(phases[calls])
		}
		calls++
		return defaultRobinhoodPositionsFetcher()
	}
	return KillSwitchCloseInputs{
		RHLiveCrypto:        rhLive,
		RHCloser:            newRobinhoodEmergencyCloser(e.deps(), robinhoodOriginKillSwitch),
		RHFetcher:           fetcher,
		RHResolveUnresolved: robinhoodEmergencyResolver(e.deps(), e.cfg, nil),
		RHFreshLongs:        robinhoodFreshLongsReader(e.deps()),
		PortfolioReason:     "drawdown",
		CloseTimeout:        2 * time.Minute,
	}
}

func (e *rhLiveEnv) applyKillSwitch(plan KillSwitchClosePlan, prices map[string]float64) {
	settleRobinhoodAttempts(e.deps(), e.cfg, nil)
	e.mu.Lock()
	defer e.mu.Unlock()
	if plan.OnChainConfirmedFlat {
		for _, sc := range strategiesInScope(e.cfg.Strategies, ScopeLive) {
			if s := e.state.Strategies[sc.ID]; s != nil {
				forceCloseKillSwitchPositions(s, sc, prices, nil, nil, hlVirtualQuantitySnapshot{}, nil)
			}
		}
	} else {
		applyKillSwitchSettledLegsWhileLatched(e.state.Strategies, strategiesInScope(e.cfg.Strategies, ScopeLive), &plan, nil, hlVirtualQuantitySnapshot{}, prices, nil)
	}
	if err := SaveStateWithStore(e.state, e.store); err != nil {
		e.t.Fatalf("save: %v", err)
	}
}

func rhSellFilled(cumulative, average string) rhRoutes {
	return rhWith(rhPrepRoutes(), rhRoutes{
		"POST " + rhTestOrders:             {rhAccepted("sell")},
		"GET " + rhOrderURL(rhTestOrderID): {rhOrder("filled", "sell", cumulative, average)},
	})
}

func TestRobinhoodLiveOrderKillSwitchConfirmsFlatWithAFreshRead(t *testing.T) {
	t.Run("filled_and_verified_flat", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		closePhase := rhWith(rhSellFilled("0.5", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		if !plan.OnChainConfirmedFlat || len(plan.RHCloseReport.ClosedCoins) != 1 {
			t.Fatalf("plan = %+v, want BTC closed and confirmed flat", plan.RHCloseReport)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		dbt := e.dbTrades("rh-btc")
		if len(dbt) != 1 || dbt[0].Price != 98 || dbt[0].Quantity != 0.5 || dbt[0].ExchangeOrderID != rhTestOrderID {
			t.Fatalf("db trades = %+v, want one venue-priced sell and no mark row", dbt)
		}
		if len(e.state.Strategies["rh-btc"].Positions) != 0 {
			t.Fatal("book must be flat")
		}
		plan2 := planKillSwitchClose(e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}}))
		e.applyKillSwitch(plan2, map[string]float64{"BTC": 50})
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 {
			t.Fatalf("a second plan run must add no settlement row, got %+v", dbt)
		}
	})
	t.Run("filled_but_still_held", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		closePhase := rhWith(rhSellFilled("0.2", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.3"))}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		if plan.OnChainConfirmedFlat || plan.RHCloseReport.Errors["BTC"] == nil || len(plan.RHCloseReport.ClosedCoins) != 0 || len(plan.RHCloseReport.AlreadyFlat) != 0 {
			t.Fatalf("report = %+v, want BTC in Errors only", plan.RHCloseReport)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		pos := e.state.Strategies["rh-btc"].Positions["BTC"]
		if pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 {
			t.Fatalf("position = %+v, want the 0.3 remainder kept", pos)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.2 || dbt[0].Price != 98 {
			t.Fatalf("db trades = %+v, want the 0.2 fill applied once", dbt)
		}
	})
	t.Run("post_close_read_fails", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		closePhase := rhWith(rhSellFilled("0.5", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}, rhRoutes{"GET " + rhTestHoldings: {{"status": 500, "json": map[string]interface{}{"detail": "down"}}}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		if plan.OnChainConfirmedFlat || plan.RHCloseReport.Errors["BTC"] == nil || len(plan.RHCloseReport.ClosedCoins) != 0 {
			t.Fatalf("report = %+v, want BTC in Errors", plan.RHCloseReport)
		}
	})
	t.Run("open_buy_attempt_blocks_flat", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"POST " + rhTestOrders:                          {rhAccepted("buy")},
			"GET " + rhOrderURL(rhTestOrderID):              {rhOrder("confirmed", "buy", "0", nil)},
			"POST " + rhOrderURL(rhTestOrderID) + "cancel/": {{"status": 200, "json": map[string]interface{}{}}},
		}))
		e.signal("rh-btc", 1, 0, 100)
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptOpen {
			t.Fatalf("seed = %+v, want open", row)
		}
		stillOpen := rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhOrderURL(rhTestOrderID):              {rhOrder("confirmed", "buy", "0", nil)},
			"POST " + rhOrderURL(rhTestOrderID) + "cancel/": {{"status": 200, "json": map[string]interface{}{}}},
			"GET " + rhTestHoldings:                         {rhHoldings()},
		})
		e.setRoutes(stillOpen)
		in := e.killSwitchInputs(stillOpen, stillOpen)
		plan := planKillSwitchClose(in)
		if plan.OnChainConfirmedFlat || plan.RHCloseReport.Errors["BTC"] == nil || len(plan.RHCloseReport.AlreadyFlat) != 0 {
			t.Fatalf("report = %+v, want BTC held by the open buy", plan.RHCloseReport)
		}
	})
	t.Run("fill_below_book_then_verified_flat", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		closePhase := rhWith(rhSellFilled("0.3", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.3"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.3"))}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		if !plan.OnChainConfirmedFlat {
			t.Fatalf("report = %+v, want confirmed flat", plan.RHCloseReport)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		dbt := e.dbTrades("rh-btc")
		if len(dbt) != 2 || dbt[0].Quantity != 0.3 || dbt[0].Price != 98 || !strings.Contains(dbt[1].Details, robinhoodVerifiedFlatKillSwitch) {
			t.Fatalf("db trades = %+v, want a venue row and a verified-flat row", dbt)
		}
	})
	t.Run("shared_coin_splits_by_book", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setPosition("rh-a", "BTC", 0.1, 90)
		e.setPosition("rh-b", "BTC", 0.3, 90)
		closePhase := rhWith(rhSellFilled("0.4", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.4"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.4"))}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		if !plan.OnChainConfirmedFlat {
			t.Fatalf("report = %+v, want confirmed flat", plan.RHCloseReport)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		a, b := e.dbTrades("rh-a"), e.dbTrades("rh-b")
		if len(a) != 1 || len(b) != 1 || math.Abs(a[0].Quantity-0.1) > 1e-12 || math.Abs(b[0].Quantity-0.3) > 1e-12 || a[0].Price != 98 || b[0].Price != 98 {
			t.Fatalf("trades a=%+v b=%+v, want a proportional venue split", a, b)
		}
	})
	t.Run("unconfigured_coin_blocks_flat", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		phase := rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("DOGE", "100"))}}
		plan := planKillSwitchClose(e.killSwitchInputs(phase, phase))
		if plan.OnChainConfirmedFlat || len(plan.RHUnconfigured) != 1 || plan.RHUnconfigured[0].Coin != "DOGE" {
			t.Fatalf("plan = %+v, want the unconfigured DOGE to block flat", plan.RHCloseReport)
		}
	})
}

func (e *rhLiveEnv) phaseCloser(routes rhRoutes, inner RobinhoodLiveCloser) RobinhoodLiveCloser {
	return func(symbol string) (*RobinhoodCloseResult, error) {
		e.setRoutes(routes)
		return inner(symbol)
	}
}

func (e *rhLiveEnv) latchCB(id string) {
	e.mu.Lock()
	s := e.state.Strategies[id]
	s.RiskState.CircuitBreaker = true
	s.RiskState.CircuitBreakerUntil = time.Now().Add(time.Hour)
	e.mu.Unlock()
}

func (e *rhLiveEnv) runCB(fetcher RobinhoodPositionsFetcher) {
	runPendingRobinhoodCircuitCloses(context.Background(), e.state, e.cfg.Strategies, nil, false, fetcher,
		newRobinhoodEmergencyCloser(e.deps(), robinhoodOriginCircuitBreaker), nil, 2*time.Minute, &e.mu,
		robinhoodCircuitCloseDeps{store: e.store, prices: map[string]float64{"BTC": 50}, resolve: robinhoodEmergencyResolver(e.deps(), e.cfg, nil)})
}

func (e *rhLiveEnv) phasedFetcher(phases ...rhRoutes) RobinhoodPositionsFetcher {
	calls := 0
	return func() ([]RobinhoodPosition, error) {
		if calls < len(phases) {
			e.setRoutes(phases[calls])
		}
		calls++
		return defaultRobinhoodPositionsFetcher()
	}
}

func TestRobinhoodLiveOrderCircuitBreakerKeepsExposureUntilVerifiedFlat(t *testing.T) {
	t.Run("activation_keeps_the_book", func(t *testing.T) {
		sc := rhLiveSpot("rh-btc", "BTC", 1000)
		s := NewStrategyState(sc)
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, InitialQuantity: 1, AvgCost: 1000, Side: "long"}
		s.RiskState.PeakValue = 2000
		s.RiskState.MaxDrawdownPct = 10
		allowed, _ := CheckRisk(&sc, s, 500, map[string]float64{"BTC": 500}, nil, nil)
		if allowed || !s.RiskState.CircuitBreaker {
			t.Fatal("expected the circuit breaker to fire")
		}
		if pos := s.Positions["BTC"]; pos == nil || pos.Quantity != 1 {
			t.Fatalf("CB activation must keep the Robinhood live book, got %+v", s.Positions)
		}
	})
	t.Run("filled_and_flat_clears_pending", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.latchCB("rh-btc")
		held := rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}
		closePhase := rhWith(rhSellFilled("0.5", "97"), held)
		fetcher := e.phasedFetcher(held, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}})
		runPendingRobinhoodCircuitClosesWithCloser(e, fetcher, closePhase)
		if s := e.state.Strategies["rh-btc"]; s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil || len(s.Positions) != 0 {
			t.Fatalf("pending=%v positions=%v, want cleared and flat", s.RiskState.PendingCircuitCloses, s.Positions)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Price != 97 {
			t.Fatalf("db trades = %+v, want one venue-priced sell", dbt)
		}
	})
	t.Run("filled_but_still_held_keeps_pending", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.latchCB("rh-btc")
		held := rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}
		closePhase := rhWith(rhSellFilled("0.2", "97"), held)
		fetcher := e.phasedFetcher(held, rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.3"))}})
		runPendingRobinhoodCircuitClosesWithCloser(e, fetcher, closePhase)
		s := e.state.Strategies["rh-btc"]
		if s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) == nil {
			t.Fatal("pending close must be kept while the coin is still held")
		}
		if pos := s.Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 {
			t.Fatalf("position = %+v, want 0.3 remaining", pos)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.2 {
			t.Fatalf("db trades = %+v, want the fill applied once", dbt)
		}
	})
	t.Run("refetch_error_keeps_pending", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.latchCB("rh-btc")
		held := rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}
		fetcher := e.phasedFetcher(held, rhRoutes{"GET " + rhTestHoldings: {{"status": 500, "json": map[string]interface{}{"detail": "down"}}}})
		runPendingRobinhoodCircuitClosesWithCloser(e, fetcher, rhWith(rhSellFilled("0.5", "97"), held))
		if e.state.Strategies["rh-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) == nil {
			t.Fatal("pending close must be kept when the post-close read fails")
		}
	})
	t.Run("nil_fetcher_keeps_pending", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.latchCB("rh-btc")
		e.mu.Lock()
		e.state.Strategies["rh-btc"].RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.5}}})
		e.mu.Unlock()
		closePhase := rhWith(rhSellFilled("0.5", "97"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}})
		closer := e.phaseCloser(closePhase, newRobinhoodEmergencyCloser(e.deps(), robinhoodOriginCircuitBreaker))
		runPendingRobinhoodCircuitCloses(context.Background(), e.state, e.cfg.Strategies, []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, true, nil,
			closer, nil, 2*time.Minute, &e.mu, robinhoodCircuitCloseDeps{store: e.store, prices: map[string]float64{"BTC": 50}})
		if e.state.Strategies["rh-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) == nil {
			t.Fatal("pending close must be kept with no post-close fetcher")
		}
	})
	t.Run("cooldown_and_reset_do_not_release_a_hold", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}))
		e.signal("rh-btc", -1, 0, 99)
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptUnknown {
			t.Fatalf("seed = %+v", row)
		}
		e.mu.Lock()
		s := e.state.Strategies["rh-btc"]
		s.RiskState.CircuitBreaker = true
		s.RiskState.CircuitBreakerUntil = time.Now().Add(-time.Minute)
		sc := e.sc("rh-btc")
		CheckRisk(&sc, s, s.Cash+45, map[string]float64{"BTC": 90}, nil, nil)
		ResetPortfolioKillSwitchManual(e.state.partitionRisk(livePartition))
		e.mu.Unlock()
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {rhAccepted("sell")}}))
		e.signal("rh-btc", -1, 0, 99)
		if posts := e.orderPosts(); len(posts) != 0 {
			t.Fatalf("cooldown and reset must not release the hold, sent %d order(s)", len(posts))
		}
	})
}

func runPendingRobinhoodCircuitClosesWithCloser(e *rhLiveEnv, fetcher RobinhoodPositionsFetcher, closePhase rhRoutes) {
	closer := e.phaseCloser(closePhase, newRobinhoodEmergencyCloser(e.deps(), robinhoodOriginCircuitBreaker))
	runPendingRobinhoodCircuitCloses(context.Background(), e.state, e.cfg.Strategies, nil, false, fetcher,
		closer, nil, 2*time.Minute, &e.mu,
		robinhoodCircuitCloseDeps{store: e.store, prices: map[string]float64{"BTC": 50}, resolve: robinhoodEmergencyResolver(e.deps(), e.cfg, nil)})
}

func TestRobinhoodLiveOrderOperatorResolutionNeedsVenueEvidence(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	row := e.seedHeldBuy()
	lookup := func(id string) (StrategyConfig, bool) {
		for _, sc := range e.cfg.Strategies {
			if sc.ID == id {
				return sc, true
			}
		}
		return StrategyConfig{}, false
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	mismatch := rhOrderBody("ord-x", "filled", "buy", "5", "100", "")
	mismatch["quantity"] = "5"
	mismatch["created_at"] = stamp
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhOrderURL("ord-x"): {{"status": 200, "json": mismatch}}}))
	msg := resolveRobinhoodAttemptByOperator(e.deps(), lookup, "rh-btc", row.ID, "ord-x", time.Now())
	if got := e.onlyAttempt(); got.Status != robinhoodAttemptUnknown || !strings.Contains(msg, "still held") {
		t.Fatalf("a mismatched order must keep the hold: msg=%q row=%+v", msg, got)
	}
	if msg := resolveRobinhoodAttemptByOperator(e.deps(), lookup, "rh-btc", "rh-000000000000000000000000", "", time.Now()); !strings.Contains(msg, "refused") {
		t.Fatalf("an unknown attempt must be refused, got %q", msg)
	}
	match := rhOrderBody("ord-7", "filled", "buy", "9.5", "100.5", row.RefID)
	e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"GET " + rhOrderURL("ord-7"): {{"status": 200, "json": match}}}))
	msg = resolveRobinhoodAttemptByOperator(e.deps(), lookup, "rh-btc", row.ID, "ord-7", time.Now())
	if got := e.onlyAttempt(); got.Status != robinhoodAttemptSettling || !strings.Contains(msg, "confirmed") {
		t.Fatalf("a matching order must confirm: msg=%q row=%+v", msg, got)
	}
	recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
	if got := e.onlyAttempt(); got.Status != robinhoodAttemptSettled {
		t.Fatalf("the confirmed attempt must settle with the next cycle, got %+v", got)
	}
	if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Price != 100.5 {
		t.Fatalf("db trades = %+v", dbt)
	}
}

func TestRobinhoodLiveOrderStorageInspectionRejectsOrphanAttempts(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	row := e.seedHeldBuy()
	e.reopen()
	layout, _ := resolveStorageLayout(e.cfg)
	ident, _ := buildStorageIdentityMap(e.cfg, layout)
	si, err := inspectStorageOwnership(layout, ident, e.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !si.OK() || len(si.Files) == 0 || len(si.Files[0].RobinhoodHeld) != 1 || !strings.Contains(si.Files[0].RobinhoodHeld[0], row.ID) {
		t.Fatalf("inspection = %+v, want the held attempt listed and accepted", si)
	}
	if _, err := json.Marshal(si); err != nil {
		t.Fatal(err)
	}
	other := &Config{DBFile: e.cfg.DBFile, Strategies: []StrategyConfig{{ID: "other", Type: "spot", Platform: "robinhood", Args: []string{"momentum_pro", "ETH", "1h", "--mode=live"}}}}
	layout2, _ := resolveStorageLayout(other)
	ident2, _ := buildStorageIdentityMap(other, layout2)
	si2, err := inspectStorageOwnership(layout2, ident2, other, false)
	if err != nil {
		t.Fatal(err)
	}
	if si2.OK() {
		t.Fatal("an unresolved attempt for an unconfigured strategy must reject the layout")
	}
	joined := strings.Join(si2.Rejections, "\n")
	if !strings.Contains(joined, row.ID) {
		t.Fatalf("rejections %v must name the attempt", si2.Rejections)
	}
	e.store.Close()
	e.store = nil
	e.cfg = other
	layout3, _ := resolveStorageLayout(other)
	ident3, _ := buildStorageIdentityMap(other, layout3)
	store, err := OpenStateStore(layout3, ident3)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, _, err := LoadStateWithStore(other, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range other.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadRobinhoodOrderGuards(state, other.Strategies); err == nil {
		t.Fatal("loading an orphan unresolved attempt must fail so startup exits with the storage code")
	}
}

func TestRobinhoodLiveOrderProbeArgvsParse(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	for _, c := range []struct {
		script string
		argv   []string
	}{
		{robinhoodResolveScript, robinhoodExecuteProbeArgv},
		{robinhoodResolveScript, robinhoodResolveProbeArgv},
		{robinhoodLiveCloseScript, robinhoodCloseProbeArgv},
	} {
		if err := probeOneCheckScript(c.script, c.argv); err != nil {
			t.Errorf("%s %v: %v", c.script, c.argv, err)
		}
	}
	if err := probeOneCheckScript(robinhoodLiveCloseScript, []string{"--symbol=BTC", "--unknown-flag", "--probe-only"}); err == nil {
		t.Error("an unknown close argv must fail the probe")
	}
}

func (e *rhLiveEnv) seedSharedSettlingClose() rhAttemptRow {
	e.t.Helper()
	e.setPosition("rh-a", "BTC", 0.1, 90)
	e.setPosition("rh-b", "BTC", 0.3, 90)
	e.setRoutes(rhWith(rhSellFilled("0.4", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.4"))}}))
	if _, err := newRobinhoodEmergencyCloser(e.deps(), robinhoodOriginKillSwitch)("BTC"); err != nil {
		e.t.Fatalf("seed close: %v", err)
	}
	row := e.onlyAttempt()
	if row.Status != robinhoodAttemptSettling || row.StrategyID != "rh-a" {
		e.t.Fatalf("seed attempt = %+v, want settling and bound to rh-a", row)
	}
	return row
}

func TestRobinhoodLiveOrderRemovedPeerDoesNotBlockSettlement(t *testing.T) {
	for _, layout := range []string{"split", "single"} {
		t.Run(layout, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
			if layout == "split" {
				e.store.Close()
				e.cfg.PaperDBFile = filepath.Join(e.dir, "paper.db")
				e.cfg.Strategies = append(e.cfg.Strategies, StrategyConfig{ID: "rh-paper", Type: "spot", Platform: "robinhood", Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}, Capital: 100})
				e.open()
			}
			e.seedSharedSettlingClose()
			e.store.Close()
			var kept []StrategyConfig
			for _, sc := range e.cfg.Strategies {
				if sc.ID != "rh-b" {
					kept = append(kept, sc)
				}
			}
			e.cfg.Strategies = kept
			e.open()
			settleRobinhoodAttempts(e.deps(), e.cfg, nil)
			row := e.onlyAttempt()
			if row.Status != robinhoodAttemptSettled || row.SettlementSeq != 1 || math.Abs(row.AppliedQty-0.1) > 1e-12 {
				t.Fatalf("attempt = %+v, want settled with rh-a's 0.1 share", row)
			}
			if dbt := e.dbTrades("rh-a"); len(dbt) != 1 || math.Abs(dbt[0].Quantity-0.1) > 1e-12 || dbt[0].Price != 98 {
				t.Fatalf("rh-a trades = %+v, want one 0.1 sell at the venue price", dbt)
			}
		})
	}
}

func TestRobinhoodLiveOrderUnreadableDuplicateCheckRetriesNextCycle(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
	e.seedSharedSettlingClose()
	db := e.store.primary().db
	if _, err := db.Exec(`ALTER TABLE trades RENAME TO trades_hidden`); err != nil {
		t.Fatal(err)
	}
	settleRobinhoodAttempts(e.deps(), e.cfg, nil)
	if _, err := db.Exec(`ALTER TABLE trades_hidden RENAME TO trades`); err != nil {
		t.Fatal(err)
	}
	if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettling || row.HoldReason != "" {
		t.Fatalf("attempt = %+v, want still settling with no booked-duplicate hold", row)
	}
	for _, id := range []string{"rh-a", "rh-b"} {
		if len(e.state.Strategies[id].TradeHistory) != 0 || len(e.dbTrades(id)) != 0 {
			t.Fatalf("%s booked during an unreadable duplicate check", id)
		}
	}
	settleRobinhoodAttempts(e.deps(), e.cfg, nil)
	if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled {
		t.Fatalf("attempt = %+v, want settled on the next pass", row)
	}
	if a, b := e.dbTrades("rh-a"), e.dbTrades("rh-b"); len(a) != 1 || len(b) != 1 {
		t.Fatalf("trades a=%+v b=%+v, want one each", a, b)
	}
}

func TestRobinhoodLiveOrderSharedSettlementCommitsOnlyWithItsSettledMark(t *testing.T) {
	for _, bookSaved := range []string{"rh-b", "rh-a"} {
		t.Run(bookSaved, func(t *testing.T) {
			e := newRHLiveEnv(t, rhLiveSpot("rh-a", "BTC", 1000), rhLiveSpot("rh-b", "BTC", 1000))
			e.seedSharedSettlingClose()
			storeCommitHook = func(storageRole) error { return fmt.Errorf("injected partition save failure") }
			settleRobinhoodAttempts(e.deps(), e.cfg, nil)
			storeCommitHook = nil
			e.mu.Lock()
			err := e.store.SaveStrategyBook(e.state.Strategies[bookSaved])
			e.mu.Unlock()
			if err == nil {
				t.Fatalf("a per-book save of %s must not commit a shared settlement without its settled mark", bookSaved)
			}
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettling {
				t.Fatalf("durable attempt = %+v, want settling", row)
			}
			if a, b := e.dbTrades("rh-a"), e.dbTrades("rh-b"); len(a) != 0 || len(b) != 0 {
				t.Fatalf("trades committed without the settled mark: a=%+v b=%+v", a, b)
			}
			e.reopen()
			settleRobinhoodAttempts(e.deps(), e.cfg, nil)
			if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled || row.HoldReason != "" {
				t.Fatalf("attempt = %+v, want settled after the restart", row)
			}
			a, b := e.dbTrades("rh-a"), e.dbTrades("rh-b")
			if len(a) != 1 || len(b) != 1 || math.Abs(a[0].Quantity-0.1) > 1e-12 || math.Abs(b[0].Quantity-0.3) > 1e-12 {
				t.Fatalf("trades a=%+v b=%+v, want each share booked once", a, b)
			}
			e.mu.Lock()
			err = e.store.SaveStrategyBook(e.state.Strategies[bookSaved])
			e.mu.Unlock()
			if err != nil {
				t.Fatalf("per-book saves must work again after the partition save: %v", err)
			}
		})
	}
}

func TestRobinhoodLiveOrderFlatReadAfterAFreshBuyDoesNotConfirmFlat(t *testing.T) {
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
	e.setPosition("rh-btc", "BTC", 0.5, 90)
	flat := rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}}
	plan := planKillSwitchClose(e.killSwitchInputs(flat, flat))
	if plan.OnChainConfirmedFlat || plan.RHCloseReport.Errors["BTC"] == nil || len(plan.RHCloseReport.AlreadyFlat) != 0 {
		t.Fatalf("report = %+v, want BTC unconfirmed while the read lags a fresh buy", plan.RHCloseReport)
	}
	e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
	if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("position = %+v, want the fresh long kept", pos)
	}
	e.mu.Lock()
	e.state.Strategies["rh-btc"].Positions["BTC"].OpenedAt = time.Now().Add(-time.Hour)
	e.mu.Unlock()
	plan = planKillSwitchClose(e.killSwitchInputs(flat, flat))
	if !plan.OnChainConfirmedFlat || len(plan.RHCloseReport.AlreadyFlat) != 1 {
		t.Fatalf("report = %+v, want an old long on a flat account confirmed flat", plan.RHCloseReport)
	}
}

type blockingNotifier struct {
	release chan struct{}
	mu      sync.Mutex
	dms     []string
}

func (b *blockingNotifier) SendMessage(channelID, content string) error { return nil }
func (b *blockingNotifier) SendDM(userID, content string) error {
	<-b.release
	b.mu.Lock()
	b.dms = append(b.dms, content)
	b.mu.Unlock()
	return nil
}
func (b *blockingNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	return "", nil
}
func (b *blockingNotifier) Close() {}

func (e *rhLiveEnv) seedHeldSettlingBuy(id string) {
	e.t.Helper()
	s := e.state.Strategies[id]
	sc := e.sc(id)
	a, err := reserveRobinhoodAttempt(e.store, e.state, s, &e.mu, robinhoodSymbol(sc.Args), robinhoodOriginSignal, "buy", robinhoodOrderRequest{Signal: 1, Result: json.RawMessage(`{"symbol":"` + robinhoodSymbol(sc.Args) + `","signal":1}`)}, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	res := &RobinhoodExecuteResult{OrderOutcome: "filled", RefID: a.RefID, Order: RobinhoodOrderEvidence{ID: "ord-" + id}, Execution: &RobinhoodExecution{Fill: &RobinhoodFill{AvgPx: 100, Quantity: 1, OID: "ord-" + id}}}
	e.mu.Lock()
	if err := recordRobinhoodOrderOutcomeLocked(e.store, s, a, res, robinhoodOrderConfirmed, "", time.Now()); err != nil {
		e.t.Fatal(err)
	}
	e.mu.Unlock()
	e.setPosition(id, robinhoodSymbol(sc.Args), 2, 90)
}

func TestRobinhoodLiveOrderHoldAlertsSendOutsideTheStateLock(t *testing.T) {
	eth := rhLiveSpot("rh-eth", "ETH", 1000)
	e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000), eth)
	e.seedHeldSettlingBuy("rh-btc")
	e.seedHeldSettlingBuy("rh-eth")
	bn := &blockingNotifier{release: make(chan struct{})}
	e.notifier = NewMultiNotifier(notifierBackend{notifier: bn, ownerID: "owner"})
	done := make(chan struct{})
	go func() {
		settleRobinhoodAttempts(e.deps(), e.cfg, nil)
		close(done)
	}()
	time.Sleep(500 * time.Millisecond)
	locked := make(chan struct{})
	go func() {
		e.mu.RLock()
		e.mu.RUnlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked notifier must not hold the state lock")
	}
	close(bn.release)
	<-done
	settleRobinhoodAttempts(e.deps(), e.cfg, nil)
	bn.mu.Lock()
	defer bn.mu.Unlock()
	count := map[string]int{}
	for _, d := range bn.dms {
		for _, coin := range []string{" buy BTC ", " buy ETH "} {
			if strings.Contains(d, "ROBINHOOD ORDER UNRESOLVED") && strings.Contains(d, coin) {
				count[coin]++
			}
		}
	}
	if count[" buy BTC "] != 1 || count[" buy ETH "] != 1 {
		t.Fatalf("hold DMs = %v (%q), want one per attempt across two passes", count, bn.dms)
	}
}
