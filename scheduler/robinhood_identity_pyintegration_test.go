//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func rhIdentityLiveMap(id, coin string) map[string]interface{} {
	return map[string]interface{}{"id": id, "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
		"args": []string{"momentum_pro", coin, "1h", "--mode=live"}, "capital": 1000.0, "max_drawdown_pct": 50.0}
}

func (e *rhLiveEnv) loadMigrated(strategies ...map[string]interface{}) []string {
	e.t.Helper()
	list := make([]interface{}, 0, len(strategies))
	for _, m := range strategies {
		list = append(list, m)
	}
	data, err := json.Marshal(map[string]interface{}{"config_version": CurrentConfigVersion, "interval_seconds": 600,
		"db_file": e.cfg.DBFile, "strategies": list})
	if err != nil {
		e.t.Fatal(err)
	}
	path := filepath.Join(e.dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		e.t.Fatalf("LoadConfig: %v", err)
	}
	if err := e.store.Close(); err != nil {
		e.t.Fatalf("close store: %v", err)
	}
	e.cfg = cfg
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		e.t.Fatal(err)
	}
	state, _, lines, err := prepareStartupState(cfg, store, true)
	if err != nil {
		e.t.Fatalf("startup identity migration: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadRobinhoodOrderGuards(state, cfg.Strategies); err != nil {
		e.t.Fatalf("load robinhood guards: %v", err)
	}
	e.store, e.state = store, state
	return lines
}

func rhIdentityRequireMigrated(t *testing.T, lines []string, ids ...string) {
	t.Helper()
	if len(lines) != len(ids) {
		t.Fatalf("migration lines = %q, want one per book %v", lines, ids)
	}
	for i, id := range ids {
		if !strings.Contains(lines[i], "Robinhood coin identity migrated for "+id+" ") || !strings.Contains(lines[i], " to BTC: positions=1") {
			t.Fatalf("migration line %q, want %s converted to BTC", lines[i], id)
		}
	}
}

func TestRobinhoodLiveOrderCoinIdentity(t *testing.T) {
	t.Run("KillSwitchSellsTheMigratedCanonicalBookOnce", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "btc", 1000))
		e.setPosition("rh-btc", "btc", 0.5, 90)
		rhIdentityRequireMigrated(t, e.loadMigrated(rhIdentityLiveMap("rh-btc", "btc")), "rh-btc")
		if got := robinhoodSymbol(e.sc("rh-btc").Args); got != "BTC" {
			t.Fatalf("configured coin = %q, want BTC", got)
		}
		closePhase := rhWith(rhSellFilled("0.5", "98"), rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhAvailable("BTC", "0.5", "0.5"))}})
		in := e.killSwitchInputs(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("BTC", "0.5"))}}, rhRoutes{"GET " + rhTestHoldings: {rhHoldings()}})
		in.RHCloser = e.phaseCloser(closePhase, in.RHCloser)
		plan := planKillSwitchClose(in)
		r := plan.RHCloseReport
		if !plan.OnChainConfirmedFlat || !slices.Equal(r.ClosedCoins, []string{"BTC"}) || len(r.AlreadyFlat) != 0 || len(r.Residue) != 0 || len(r.Unconfigured) != 0 {
			t.Fatalf("report = %+v, want BTC closed by one order and no residue", r)
		}
		if q := e.allPostQuantities(); !slices.Equal(q, []float64{0.5}) {
			t.Fatalf("order quantities = %v, want one 0.5 BTC sell", q)
		}
		e.applyKillSwitch(plan, map[string]float64{"BTC": 50})
		if n := len(e.state.Strategies["rh-btc"].Positions); n != 0 {
			t.Fatalf("book must be flat after the confirmed sale, got %d positions", n)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.5 || dbt[0].Price != 98 {
			t.Fatalf("db trades = %+v, want one venue-priced sell", dbt)
		}
	})

	t.Run("PendingCloseDrainSellsTheMigratedCanonicalCoin", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "btc", 1000))
		e.setPosition("rh-btc", "btc", 0.5, 90)
		e.tripCB("rh-btc")
		if p := e.pendingRH("rh-btc"); p == nil || p.Symbols[0].Symbol != "btc" {
			t.Fatalf("seed pending close = %+v, want the lowercase spelling", p)
		}
		rhIdentityRequireMigrated(t, e.loadMigrated(rhIdentityLiveMap("rh-btc", "btc")), "rh-btc")
		if p := e.pendingRH("rh-btc"); p == nil || len(p.Symbols) != 1 || p.Symbols[0].Symbol != "BTC" || p.Symbols[0].Size != 0.5 {
			t.Fatalf("migrated pending close = %+v, want BTC 0.5", p)
		}
		e.resetSent()
		e.runCBPhases(rhClosePhase(rhSellFilled("0.5", "96"), "0.5", "0.5"), rhHoldingsPhase(rhHeld("BTC", "0.5")), rhHoldingsPhase())
		if p := e.pendingRH("rh-btc"); p != nil || e.bookQty("rh-btc", "BTC") != 0 {
			t.Fatalf("pending=%+v book=%g, want the drain to sell and clear", p, e.bookQty("rh-btc", "BTC"))
		}
		if q := e.allPostQuantities(); !slices.Equal(q, []float64{0.5}) {
			t.Fatalf("order quantities = %v, want one 0.5 BTC sell", q)
		}
	})

	t.Run("MixedCasePeersShareOneCoinIdentity", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-a", "btc", 1000), rhLiveSpot("rh-b", "BTC", 1000))
		e.setPosition("rh-a", "btc", 0.5, 90)
		rhIdentityRequireMigrated(t, e.loadMigrated(rhIdentityLiveMap("rh-a", "btc"), rhIdentityLiveMap("rh-b", "BTC")), "rh-a")
		a, b := robinhoodSymbol(e.sc("rh-a").Args), robinhoodSymbol(e.sc("rh-b").Args)
		if a != "BTC" || b != "BTC" {
			t.Fatalf("configured coins = %q, %q, want one BTC identity", a, b)
		}
		if owners := robinhoodLiveOwnersForCoin("BTC", e.cfg.Strategies); len(owners) != 2 {
			t.Fatalf("owners = %v, want both peers, so neither gets sole-owner sizing", owners)
		}
		unlock, ok := tryLockRobinhoodCoin("acct", a)
		if !ok {
			t.Fatal("first coin lock must succeed")
		}
		if _, again := tryLockRobinhoodCoin("acct", b); again {
			t.Fatal("the peer must share the coin lock")
		}
		unlock()

		e.latchCB("rh-a")
		e.runCBSharedCycle(rhHoldingsPhase(rhHeld("BTC", "0.5")))
		if n := e.sharedCloseDMs("rh-a"); n != 1 {
			t.Fatalf("shared-coin refusal DMs = %d, want 1", n)
		}
		if got := e.bookQty("rh-a", "BTC"); got != 0.5 {
			t.Fatalf("book = %g, want the shared coin held", got)
		}

		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}))
		if trades, _ := e.signal("rh-b", 1, 0, 100); trades != 0 {
			t.Fatalf("seed trades = %d", trades)
		}
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptUnknown || row.Symbol != "BTC" {
			t.Fatalf("seed attempt = %+v", row)
		}
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {rhAccepted("buy")}}))
		e.signal("rh-a", 1, 0, 100)
		if posts := e.orderPosts(); len(posts) != 0 {
			t.Fatalf("the peer sent %d order(s) while the coin had an unresolved attempt", len(posts))
		}
		if rows := e.attemptRows(); len(rows) != 1 {
			t.Fatalf("attempts = %+v, want the one unresolved attempt only", rows)
		}
	})

	t.Run("MigratedUnresolvedOrderRecoversOnce", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "btc", 1000))
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}))
		if trades, _ := e.signal("rh-btc", 1, 0, 100); trades != 0 {
			t.Fatalf("seed trades = %d", trades)
		}
		seed := e.onlyAttempt()
		if seed.Status != robinhoodAttemptUnknown || seed.Symbol != "btc" {
			t.Fatalf("seed attempt = %+v, want an unknown lowercase attempt", seed)
		}
		var reqBefore, evBefore string
		if err := e.store.primary().db.QueryRow(`SELECT request_json, evidence_json FROM robinhood_order_attempts WHERE attempt_id = ?`, seed.ID).Scan(&reqBefore, &evBefore); err != nil {
			t.Fatal(err)
		}
		lines := e.loadMigrated(rhIdentityLiveMap("rh-btc", "btc"))
		if len(lines) != 1 || !strings.Contains(lines[0], "unresolved_attempts=1") {
			t.Fatalf("migration lines = %q, want the attempt renamed", lines)
		}
		var reqAfter, evAfter string
		if err := e.store.primary().db.QueryRow(`SELECT request_json, evidence_json FROM robinhood_order_attempts WHERE attempt_id = ?`, seed.ID).Scan(&reqAfter, &evAfter); err != nil {
			t.Fatal(err)
		}
		if reqAfter != reqBefore || evAfter != evBefore {
			t.Fatalf("frozen payloads changed: request %q -> %q, evidence %q -> %q", reqBefore, reqAfter, evBefore, evAfter)
		}
		if row := e.onlyAttempt(); row.Symbol != "BTC" || row.Status != robinhoodAttemptUnknown || row.RefID != seed.RefID {
			t.Fatalf("migrated attempt = %+v", row)
		}
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"GET " + rhTestOrders: {{"status": 200, "json": map[string]interface{}{"next": nil, "results": []interface{}{
				map[string]interface{}{"id": "ord-9", "ref_id": seed.RefID, "state": "filled", "side": "buy", "currency_pair_id": "pair-btc", "quantity": "0.1", "created_at": time.Now().UTC().Format(time.RFC3339)},
			}}}},
			"GET " + rhOrderURL("ord-9"): {{"status": 200, "json": rhOrderBody("ord-9", "filled", "buy", "0.1", "101", seed.RefID)}},
		}))
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if after := e.onlyAttempt(); after.Status != robinhoodAttemptSettled || after.SettlementSeq != 1 || after.Symbol != "BTC" {
			t.Fatalf("recovery must settle the renamed attempt once, got %+v", after)
		}
		if got := e.bookQty("rh-btc", "BTC"); math.Abs(got-0.1) > 1e-12 {
			t.Fatalf("book = %g, want the recovered 0.1 under BTC", got)
		}
		recoverRobinhoodOrders(e.cfg, e.state, e.store, &e.mu, e.notifier, nil, time.Now())
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.1 || dbt[0].Price != 101 {
			t.Fatalf("db trades = %+v, want one recovered buy and no duplicate", dbt)
		}
	})

	t.Run("WrongSignalSymbolIsAScriptErrorAndReservesNothing", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		prevTracker := scriptFailureTracker
		t.Cleanup(func() { scriptFailureTracker = prevTracker })
		stub := filepath.Join(t.TempDir(), "rh_identity_stub.py")
		if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
			t.Fatal(err)
		}
		sc := StrategyConfig{ID: "rh-btc", Type: "spot", Platform: "robinhood", Script: stub, Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}}
		payload := map[string]interface{}{"strategy": "momentum_pro", "timeframe": "1h", "signal": 1, "price": 100,
			"indicators": map[string]interface{}{}, "mode": "paper", "platform": "robinhood", "close_fraction": 0}
		for _, symbol := range []string{"btc"} {
			scriptFailureTracker = &ScriptFailureTracker{}
			e.mock.mu.Lock()
			e.mock.messages = nil
			e.mock.mu.Unlock()
			payload["symbol"] = symbol
			blob, _ := json.Marshal(payload)
			t.Setenv("HTF_IT_STUB_JSON", string(blob))
			t.Setenv("HTF_IT_STUB_EXIT", "0")
			for i := 0; i < scriptFailureAlertThreshold; i++ {
				if r, _, _, ok := runRobinhoodCheck(sc, map[string]float64{"BTC": 100}, PositionCtx{}, nil, "simple", e.notifier, e.logger); ok || r != nil {
					t.Fatalf("symbol %q: check passed, want a script error", symbol)
				}
			}
			e.mock.mu.Lock()
			alerted := false
			for _, m := range e.mock.messages {
				if strings.Contains(m.content, "robinhood check returned symbol") {
					alerted = true
				}
			}
			e.mock.mu.Unlock()
			if !alerted {
				t.Fatalf("symbol %q: no script-error alert", symbol)
			}
		}
		payload["symbol"] = "BTC"
		blob, _ := json.Marshal(payload)
		t.Setenv("HTF_IT_STUB_JSON", string(blob))
		if _, _, _, ok := runRobinhoodCheck(sc, map[string]float64{"BTC": 100}, PositionCtx{}, nil, "simple", e.notifier, e.logger); !ok {
			t.Fatal("the canonical echo must pass")
		}
		if rows := e.attemptRows(); len(rows) != 0 {
			t.Fatalf("attempts = %+v, want none", rows)
		}
	})

	t.Run("AmbiguousHoldingsFailTheWholeRead", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhRoutes{"GET " + rhTestHoldings: {rhHoldings(rhHeld("btc", "0.1"), rhHeld("BTC", "0.4"))}})
		if _, err := defaultRobinhoodPositionsFetcher(); err == nil || !strings.Contains(err.Error(), "duplicate holding for BTC") {
			t.Fatalf("venue duplicate: err = %v, want the strict read refused", err)
		}
		stub := filepath.Join(t.TempDir(), "rh_positions_stub.py")
		if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
			t.Fatal(err)
		}
		prev := robinhoodFetchPositionsScript
		robinhoodFetchPositionsScript = stub
		t.Cleanup(func() { robinhoodFetchPositionsScript = prev })
		t.Setenv("HTF_IT_STUB_EXIT", "0")
		for name, rows := range map[string][]interface{}{
			"duplicate": {map[string]interface{}{"coin": "BTC", "size": 0.25, "avg_price": 1}, map[string]interface{}{"coin": " btc", "size": 0.25, "avg_price": 1}},
		} {
			blob, _ := json.Marshal(map[string]interface{}{"positions": rows, "platform": "robinhood"})
			t.Setenv("HTF_IT_STUB_JSON", string(blob))
			if _, err := defaultRobinhoodPositionsFetcher(); err == nil || !strings.Contains(err.Error(), "refusing the whole read") {
				t.Fatalf("%s: err = %v, want the whole read refused", name, err)
			}
			e.resetSent()
			in := e.killSwitchInputs()
			in.RHFetcher = defaultRobinhoodPositionsFetcher
			plan := planKillSwitchClose(in)
			if plan.OnChainConfirmedFlat || len(plan.RHCloseReport.ClosedCoins) != 0 || len(plan.RHCloseReport.AlreadyFlat) != 0 {
				t.Fatalf("%s: plan = %+v, want no flat proof", name, plan.RHCloseReport)
			}
			if posts := e.orderPosts(); len(posts) != 0 {
				t.Fatalf("%s: sent %d order(s) on an ambiguous read", name, len(posts))
			}
		}
	})
}
