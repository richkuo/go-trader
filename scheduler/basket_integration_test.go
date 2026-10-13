package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBasketConfigLoadAcceptsValidAndRefusesNamedCases(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	if len(cfg.Baskets) != 1 || len(basketMemberConfigs(cfg, cfg.Baskets[0])) != 4 {
		t.Fatalf("valid basket did not load: %+v", cfg.Baskets)
	}
	member := func(id string, edit func(sc map[string]interface{})) func(root map[string]interface{}) {
		return func(root map[string]interface{}) { edit(basketTestStrategy(root, id)) }
	}
	basket := func(edit func(b map[string]interface{})) func(root map[string]interface{}) {
		return func(root map[string]interface{}) { edit(basketTestBasket(root)) }
	}
	addStrategy := func(sc map[string]interface{}) func(root map[string]interface{}) {
		return func(root map[string]interface{}) {
			root["strategies"] = append(root["strategies"].([]interface{}), sc)
		}
	}
	plain := func(id, positional, coin string) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{positional, coin, "1h", "--mode=paper"}, "capital": 1000,
		}
	}
	cases := []struct {
		name   string
		mutate func(root map[string]interface{})
		want   string
	}{
		{"duplicate coin", member("hl-bsk-eth", func(sc map[string]interface{}) {
			sc["args"] = []interface{}{"basket_member", "BTC", "1h", "--mode=paper"}
		}), "duplicate coin BTC"},
		{"non-member holder", addStrategy(plain("hl-other-btc", "rsi", "BTC")), "which basket \"xs-test\" owns exclusively"},
		{"mixed paper source", func(root map[string]interface{}) {
			root["paper_sources"] = []interface{}{map[string]interface{}{"id": "alt", "db_file": filepath.Join(t.TempDir(), "alt.db")}}
			basketTestStrategy(root, "hl-bsk-sol")["paper_source"] = "alt"
		}, "members span risk partitions"},
		{"rest feed", func(root map[string]interface{}) { root["market_feed"] = "rest" }, "the rest feed has no sealed deadline snapshot"},
		{"unknown basket key", basket(func(b map[string]interface{}) { b["rebalance_every"] = 1 }), "unknown field \"rebalance_every\""},
		{"unknown eligibility key", basket(func(b map[string]interface{}) {
			b["eligibility"].(map[string]interface{})["sources"] = "x"
		}), "unknown field \"sources\" in eligibility block"},
		{"unknown listing key", basket(func(b map[string]interface{}) {
			b["eligibility"].(map[string]interface{})["listings"].(map[string]interface{})["BTC"].(map[string]interface{})["listed"] = "x"
		}), "unknown field \"listed\" in eligibility.listings[BTC]"},
		{"unknown ranker param", basket(func(b map[string]interface{}) { b["params"].(map[string]interface{})["lookback"] = 3 }), "unknown field \"lookback\" in params"},
		{"ranker as open strategy", addStrategy(func() map[string]interface{} {
			sc := plain("hl-open-doge", "rsi", "DOGE")
			sc["open_strategy"] = map[string]interface{}{"name": "basket_scripted_fixture"}
			return sc
		}()), "open strategy \"basket_scripted_fixture\" is a basket evaluator"},
		{"ranker as positional", addStrategy(plain("hl-pos-doge", "basket_scripted_fixture", "DOGE")), "positional strategy \"basket_scripted_fixture\" is a basket evaluator"},
		{"ranker as close fallback", addStrategy(func() map[string]interface{} {
			sc := plain("hl-close-doge", "rsi", "DOGE")
			sc["close_strategy"] = map[string]interface{}{"name": "basket_scripted_fixture"}
			return sc
		}()), "close-fallback strategy \"basket_scripted_fixture\" is a basket evaluator"},
		{"no closed-bar identity", member("hl-bsk-btc", func(sc map[string]interface{}) { delete(sc, "closed_bar_decisions") }), "requires \"closed_bar_decisions\": true"},
		{"gross budget", basket(func(b map[string]interface{}) { b["gross_budget_usd"] = 0 }), "gross_budget_usd must be a finite positive number"},
		{"net above gross", basket(func(b map[string]interface{}) { b["max_transition_net_usd"] = 5000 }), "max_transition_net_usd 5000 exceeds gross_budget_usd"},
		{"coin cap above gross", basket(func(b map[string]interface{}) { b["max_coin_notional_usd"] = 4001 }), "max_coin_notional_usd 4001 exceeds gross_budget_usd"},
		{"window", basket(func(b map[string]interface{}) { b["evaluation_window_seconds"] = 86400 }), "evaluation_window_seconds must be positive and shorter"},
		{"ranker timeout", basket(func(b map[string]interface{}) { b["ranker_timeout_seconds"] = 3600 }), "ranker_timeout_seconds must be in"},
		{"schedule grid", basket(func(b map[string]interface{}) { b["rebalance_interval_seconds"] = 5400 }), "is not a multiple of member"},
		{"min population", basket(func(b map[string]interface{}) { b["min_eligible_members"] = 9 }), "min_eligible_members must be in"},
		{"scale-in route", member("hl-bsk-btc", func(sc map[string]interface{}) { sc["allow_scale_in"] = true }), "must not set allow_scale_in"},
		{"own open strategy", member("hl-bsk-btc", func(sc map[string]interface{}) {
			sc["open_strategy"] = map[string]interface{}{"name": "rsi"}
		}), "must not set open_strategy"},
		{"inversion route", member("hl-bsk-btc", func(sc map[string]interface{}) { sc["invert_signal"] = true }), "must not set invert_signal"},
		{"open fallback close", member("hl-bsk-btc", func(sc map[string]interface{}) {
			sc["close_strategy"] = map[string]interface{}{"name": "rsi"}
		}), "is an open-strategy close fallback"},
		{"live member", member("hl-bsk-btc", func(sc map[string]interface{}) {
			sc["args"] = []interface{}{"basket_member", "BTC", "1h", "--mode=live"}
		}), "live basket dispatch is refused"},
		{"live member with acknowledgement", member("hl-bsk-btc", func(sc map[string]interface{}) {
			sc["args"] = []interface{}{"basket_member", "BTC", "1h", "--mode=live"}
			sc["allow_no_edge"] = true
		}), "allow_no_edge: true does not lift this refusal"},
		{"reserved member token", addStrategy(plain("hl-token-doge", "basket_member", "DOGE")), "is reserved for basket members"},
		{"member not listed", func(root map[string]interface{}) {
			b := basketTestBasket(root)
			b["members"] = b["members"].([]string)[1:]
		}, "but that basket does not list it as a member"},
		{"missing listing fact", basket(func(b map[string]interface{}) {
			delete(b["eligibility"].(map[string]interface{})["listings"].(map[string]interface{}), "SOL")
		}), "has no listing fact for member coin SOL"},
		{"unknown ranker", basket(func(b map[string]interface{}) { b["ranker"] = "momentum" }), "is not a registered basket evaluator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := basketTestConfigJSON(t, dir, tc.mutate)
			_, err := LoadConfigReadOnly(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestBasketReloadRefusesEveryBasketChange(t *testing.T) {
	cur := loadBasketTestConfig(t, nil)
	for name, mutate := range map[string]func(root map[string]interface{}){
		"budget":   func(root map[string]interface{}) { basketTestBasket(root)["gross_budget_usd"] = 3000 },
		"window":   func(root map[string]interface{}) { basketTestBasket(root)["evaluation_window_seconds"] = 900 },
		"timeout":  func(root map[string]interface{}) { basketTestBasket(root)["ranker_timeout_seconds"] = 30 },
		"schedule": func(root map[string]interface{}) { basketTestBasket(root)["rebalance_interval_seconds"] = 43200 },
		"ranker": func(root map[string]interface{}) {
			basketTestBasket(root)["params"] = map[string]interface{}{"hold": "x"}
		},
		"listings": func(root map[string]interface{}) {
			basketTestBasket(root)["eligibility"].(map[string]interface{})["version"] = "v2"
		},
		"remove id": func(root map[string]interface{}) {
			delete(basketTestStrategy(root, "hl-bsk-sol"), "basket_id")
			basketTestStrategy(root, "hl-bsk-sol")["args"] = []interface{}{"rsi", "SOL", "1h", "--mode=paper"}
			delete(basketTestBasket(root)["eligibility"].(map[string]interface{})["listings"].(map[string]interface{}), "SOL")
			basketTestBasket(root)["members"] = []string{"hl-bsk-avax", "hl-bsk-btc", "hl-bsk-eth"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			next := loadBasketTestConfig(t, mutate)
			next.DBFile, next.LogDir = cur.DBFile, cur.LogDir
			err := validateHotReloadCompatible(cur, next)
			if err == nil || !strings.Contains(err.Error(), "restart") {
				t.Fatalf("want a restart-required refusal, got %v", err)
			}
			if !strings.Contains(err.Error(), "baskets changed") && !strings.Contains(err.Error(), "basket_id changed") {
				t.Fatalf("refusal does not name the basket change: %v", err)
			}
		})
	}
	same := loadBasketTestConfig(t, nil)
	same.DBFile, same.LogDir = cur.DBFile, cur.LogDir
	if err := validateHotReloadCompatible(cur, same); err != nil {
		t.Fatalf("unchanged baskets must reload: %v", err)
	}
}

func TestBasketPaperRebalanceEndToEndAndRestart(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 7)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)

	rt := env.rt()
	rb := rt.Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("rebalance did not complete: %+v", rb)
	}
	targets, err := rb.Targets()
	if err != nil || targets == nil {
		t.Fatalf("targets: %v", err)
	}
	for _, m := range basketTestMembers {
		mt := targets.Members[m.ID]
		got := env.signedQty(m.ID, m.Coin)
		if !basketQtyEqual(got, mt.TargetQty) || mt.TargetQty == 0 {
			t.Fatalf("%s holds %.10g, target %.10g", m.ID, got, mt.TargetQty)
		}
		if math.Abs(math.Abs(mt.TargetNotionalUSD)-1000) > 1e-9 {
			t.Fatalf("%s target notional %.4f, want 1000", m.ID, mt.TargetNotionalUSD)
		}
		if !basketQtyEqual(rt.Expected[m.ID], got) {
			t.Fatalf("%s expected %.10g, book %.10g", m.ID, rt.Expected[m.ID], got)
		}
		s := env.state.Strategies[m.ID]
		opens := 0
		for _, tr := range s.TradeHistory {
			if !tr.IsClose && tr.Symbol == m.Coin {
				opens++
			}
		}
		if opens != 1 {
			t.Fatalf("%s booked %d opens, want exactly one", m.ID, opens)
		}
		if s.Positions[m.Coin].StopLossTriggerPx <= 0 || s.Positions[m.Coin].EntryATR <= 0 {
			t.Fatalf("%s position has no armed paper stop or entry ATR: %+v", m.ID, s.Positions[m.Coin])
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE status = 'committed'`); n != 4 {
		t.Fatalf("committed actions = %d, want 4", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances WHERE status = 'completed' AND targets_json != ''`); n != 1 {
		t.Fatalf("completed rebalance rows = %d, want 1", n)
	}
	if n := env.dbCount(`SELECT COUNT(DISTINCT client_order_id) FROM basket_member_actions`); n != 4 {
		t.Fatalf("distinct client order ids = %d, want 4", n)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("persisted open trades = %d, want 4", n)
	}

	reranks := 0
	countingRanker := func(inner basketRankRunner) basketRankRunner {
		return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
			reranks++
			return inner(ctx, stdin, timeout)
		}
	}
	env.restartFromDisk(countingRanker(basketTestRanker(basketTestWeights, "")), nil)
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if reranks != 0 {
		t.Fatalf("a recorded deadline was reranked %d time(s) after restart", reranks)
	}
	for _, m := range basketTestMembers {
		if got := env.signedQty(m.ID, m.Coin); !basketQtyEqual(got, targets.Members[m.ID].TargetQty) {
			t.Fatalf("after restart %s holds %.10g", m.ID, got)
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("restart duplicated bookings: %d open trades", n)
	}
}

func TestBasketStorageSeparatesFilesAndRefusesWrongOwnership(t *testing.T) {
	dir := t.TempDir()
	path := basketTestConfigJSON(t, dir, func(root map[string]interface{}) {
		root["paper_db_file"] = filepath.Join(dir, "paper.db")
		root["paper_sources"] = []interface{}{
			map[string]interface{}{"id": "alpha", "db_file": filepath.Join(dir, "alpha.db")},
			map[string]interface{}{"id": "beta", "db_file": filepath.Join(dir, "beta.db")},
		}
		for _, m := range basketTestMembers {
			basketTestStrategy(root, m.ID)["paper_source"] = "alpha"
		}
		root["strategies"] = append(root["strategies"].([]interface{}), map[string]interface{}{
			"id": "hl-beta-doge", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{"rsi", "DOGE", "1h", "--mode=paper"}, "capital": 1000, "paper_source": "beta",
		}, map[string]interface{}{
			"id": "hl-default-doge", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{"rsi", "DOGE", "1h", "--mode=paper"}, "capital": 1000,
		})
	})
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 3)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if rb := env.rt().latest(); rb == nil || rb.Status == basketRebalanceHeld {
		t.Fatalf("rebalance did not run in the alpha partition: %+v", rb)
	}
	for _, part := range activePartitions(cfg.Strategies) {
		if err := env.store.SavePartition(env.state, part); err != nil {
			t.Fatalf("SavePartition(%s): %v", partitionLabel(part), err)
		}
	}
	countIn := func(p string, table string) int {
		db, err := openStateDBReadOnly(p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ok, err := tableExists(db, table)
		if err != nil || !ok {
			return 0
		}
		var n int
		if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countIn(filepath.Join(dir, "alpha.db"), "basket_rebalances"); n != 1 {
		t.Fatalf("alpha file holds %d basket rows, want 1", n)
	}
	for _, other := range []string{"paper.db", "beta.db"} {
		if n := countIn(filepath.Join(dir, other), "basket_rebalances"); n != 0 {
			t.Fatalf("%s holds %d basket rows, want 0", other, n)
		}
		if n := countIn(filepath.Join(dir, other), "basket_member_actions"); n != 0 {
			t.Fatalf("%s holds %d basket actions, want 0", other, n)
		}
	}
	rt := env.rt()
	betaDB := env.store.file(env.store.layout.roleForPartition(paperSourcePartition("beta")))
	if err := betaDB.persistBasketProgress(rt); err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Fatalf("a basket write into the beta file must fail on ownership before mutation, got %v", err)
	}
	if n := countIn(filepath.Join(dir, "beta.db"), "basket_contracts"); n != 0 {
		t.Fatalf("refused write left %d contract rows in the beta file", n)
	}

	before := basketSchemaSnapshot(t, betaDB)
	if err := betaDB.migrateBasketSchema(); err != nil {
		t.Fatal(err)
	}
	if err := betaDB.migrateSchema(); err != nil {
		t.Fatal(err)
	}
	if after := basketSchemaSnapshot(t, betaDB); !reflect.DeepEqual(before, after) {
		t.Fatalf("repeated migration changed the schema:\n%v\n%v", before, after)
	}
}

func basketSchemaSnapshot(t *testing.T, db *StateDB) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT type || ':' || name || ':' || COALESCE(sql, '') FROM sqlite_master ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestBasketStorageInspectionReportsPendingRecovery(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	venue := newScriptedBasketVenue(false)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueUnknown, Detail: "timeout"}}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	a := env.rt().pendingAction()
	if a == nil || a.Status != basketActionUnresolved {
		t.Fatalf("want one unresolved action, got %+v", a)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	si, err := inspectStorageOwnership(layout, ident, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	text := formatStorageInspection(si)
	if !strings.Contains(text, "PENDING BASKET RECOVERY xs-test action "+a.ActionID) || !strings.Contains(text, "unresolved") {
		t.Fatalf("storage inspection does not report the pending basket action:\n%s", text)
	}
	if !si.OK() {
		t.Fatalf("a pending action in its own file is not an ownership rejection: %v", si.Rejections)
	}
}

func TestBasketTransitionBoundsHoldEveryReachableState(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	bounds := basketBoundsFor(env.rt().Contract)
	marks := basketTestMarks()
	venue.onSubmit = func(a *basketAction) {
		exp := basketExposure{Qty: map[string]float64{}, Mark: marks}
		for _, m := range basketTestMembers {
			exp.Qty[m.Coin] = basketPositionSigned(env.state.Strategies[m.ID], m.Coin)
		}
		basketReachableWithin(t, "submission", exp, *a, bounds)
	}
	deadline := basketTestDeadline()
	for h := 0; h < 6; h++ {
		candle := deadline.Add(time.Duration(h) * time.Hour)
		snap := basketTestSnapshot(candle, 60, marks, 1)
		env.cycle(candle.Add(time.Minute), candle, snap, true)
	}
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("rebalance did not complete within six candles under the $400 net bound: %+v", rb)
	}
	if len(venue.submits) < 5 {
		t.Fatalf("the $400 net bound should split the $1000 legs into tranches, saw %d submissions", len(venue.submits))
	}
	perCandle := map[string]map[int64]int{}
	for _, a := range venue.submits {
		if a.Direction != basketDirectionIncrease {
			continue
		}
		if perCandle[a.StrategyID] == nil {
			perCandle[a.StrategyID] = map[int64]int{}
		}
		perCandle[a.StrategyID][a.DecisionBarOpenMs]++
		if perCandle[a.StrategyID][a.DecisionBarOpenMs] > 1 {
			t.Fatalf("%s submitted two increases from the candle opened %d", a.StrategyID, a.DecisionBarOpenMs)
		}
	}
}

func TestBasketNoLegalSequenceHoldsAndInitialBreachHoldsIncreases(t *testing.T) {
	start := basketExposure{Qty: map[string]float64{"BTC": 0, "SOL": 0}, Mark: map[string]float64{"BTC": 60000, "SOL": 150}}
	b := basketBounds{GrossUSD: 1000, NetUSD: 5, PerCoinUSD: 1000}
	floor := func(coin string, qty float64) float64 {
		q := hlFloorLotSize(qty, basketTestLots[coin])
		if q*start.Mark[coin] < hlVenueCloseGateThresholdUSD() {
			return 0
		}
		return q
	}
	c := basketCandidate{StrategyID: "hl-bsk-btc", Coin: "BTC", Direction: basketDirectionIncrease, PositionSide: entrySideLong, OrderSide: "buy", FullQty: 0.01}
	if tr := basketLegalTranche(start, c, b, true, floor); tr.Qty > 0 {
		t.Fatalf("a $5 net bound admits no lot-sized BTC tranche, got %v", tr)
	}
	breached := basketExposure{Qty: map[string]float64{"BTC": 0.02, "SOL": 0}, Mark: start.Mark}
	inc := basketCandidate{StrategyID: "hl-bsk-sol", Coin: "SOL", Direction: basketDirectionIncrease, PositionSide: entrySideShort, OrderSide: "sell", FullQty: 1}
	if tr := basketLegalTranche(breached, inc, basketBounds{GrossUSD: 1000, NetUSD: 2000, PerCoinUSD: 1000}, true, floor); tr.Qty > 0 {
		t.Fatalf("an increase from an exposure already over the gross budget must hold, got %v", tr)
	}
	red := basketCandidate{StrategyID: "hl-bsk-btc", Coin: "BTC", Direction: basketDirectionReduce, PositionSide: entrySideLong, OrderSide: "sell", FullQty: 0.02, CurrentQty: 0.02}
	if tr := basketLegalTranche(breached, red, basketBounds{GrossUSD: 1000, NetUSD: 2000, PerCoinUSD: 1000}, false, floor); !(tr.Qty > 0) {
		t.Fatalf("a reduction that does not worsen the breach must stay available, got %v", tr)
	}
}

func TestBasketInconsistentVenueFillStaysUnresolved(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	venue := newScriptedBasketVenue(false)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenuePartial, FilledQty: 0}}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if len(venue.submits) != 1 {
		t.Fatalf("want exactly one submission before the inconsistent outcome holds, got %d", len(venue.submits))
	}
	a := env.rt().pendingAction()
	if a == nil || a.Status != basketActionUnresolved || a.AppliedQty != 0 {
		t.Fatalf("a partial outcome with no quantity must stay unresolved and unbooked: %+v", a)
	}
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s booked %.10g from an inconsistent outcome", m.ID, q)
		}
	}
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if len(venue.submits) != 1 {
		t.Fatalf("no new elective order may be sent while an action is unresolved; submissions %d", len(venue.submits))
	}
}

func TestBasketPartialUnknownRejectedAndRestartRecovery(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x"+strings.Repeat("a", 40))
	venue := newScriptedBasketVenue(true)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	rt := env.rt()

	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenuePartial, FilledQty: -0.5}, {Outcome: basketVenueUnknown, Detail: "disconnect after send"}}
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if len(venue.submits) != 2 {
		t.Fatalf("want the partial leg then one unknown leg, saw %d submissions", len(venue.submits))
	}
	first := venue.submits[0]
	half := hlFloorLotSize(first.RequestedQty*0.5, basketTestLots[first.Symbol])
	if got := env.signedQty(first.StrategyID, first.Symbol); !basketQtyEqual(math.Abs(got), half) {
		t.Fatalf("partial fill booked %.10g, want %.10g", got, half)
	}
	pending := rt.pendingAction()
	if pending == nil || pending.Status != basketActionUnresolved || pending.ActionID == first.ActionID {
		t.Fatalf("want the second leg unresolved, got %+v", pending)
	}
	in := env.state.Strategies[pending.StrategyID].EntryGuard.Intents[pending.EntryIntentID]
	if in == nil || in.Status != entryIntentSubmitting || in.ClientOrderID != pending.ClientOrderID {
		t.Fatalf("the unresolved increase must keep its submitting entry intent with the same client id: %+v", in)
	}

	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	rt = env.rt()
	again := rt.pendingAction()
	if again == nil || again.ActionID != pending.ActionID || again.Status != basketActionUnresolved {
		t.Fatalf("restart lost the unresolved action: %+v", again)
	}
	submitsBefore := len(venue.submits)
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if len(venue.submits) != submitsBefore {
		t.Fatalf("an unresolved order must reconcile before any other elective order; %d new submissions", len(venue.submits)-submitsBefore)
	}

	venue.queries[pending.ClientOrderID] = []basketVenueOutcome{{Outcome: basketVenueFilled, FilledQty: again.RequestedQty, VenueOrderID: "777"}}
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueRejected, Detail: "insufficient margin"}}
	next := deadline.Add(time.Hour)
	nextSnap := basketTestSnapshot(next, 60, basketTestMarks(), 1)
	env.cycle(next.Add(time.Minute), next, nextSnap, true)
	resolved := rt.Actions[pending.ActionID]
	if resolved.Status != basketActionCommitted || resolved.VenueOrderID != "777" {
		t.Fatalf("the race-resolved fill was not booked once: %+v", resolved)
	}
	if got := env.signedQty(resolved.StrategyID, resolved.Symbol); !basketQtyEqual(math.Abs(got), resolved.RequestedQty) {
		t.Fatalf("%s holds %.10g after reconciliation, want %.10g", resolved.StrategyID, got, resolved.RequestedQty)
	}
	if in := env.state.Strategies[resolved.StrategyID].EntryGuard.Intents[resolved.EntryIntentID]; in != nil && in.Status != entryIntentCommitted {
		t.Fatalf("the reconciled entry intent must commit with the fill, got %s", in.Status)
	}
	rb := rt.Rebalances[deadline.UnixMilli()]
	if !rb.IncreasesFrozen || !strings.Contains(rb.Outcome.FreezeReason, "rejected") {
		t.Fatalf("a rejected leg must freeze increases: frozen=%v reason=%q", rb.IncreasesFrozen, rb.Outcome.FreezeReason)
	}
	submits := len(venue.submits)
	later := next.Add(time.Hour)
	env.cycle(later.Add(time.Minute), later, basketTestSnapshot(later, 60, basketTestMarks(), 1), true)
	for _, a := range venue.submits[submits:] {
		if a.Direction == basketDirectionIncrease {
			t.Fatalf("an increase was sent after the freeze: %+v", a)
		}
	}
	seen := map[string]bool{}
	for _, a := range venue.submits {
		if seen[a.ClientOrderID] {
			t.Fatalf("client order id %s was submitted twice", a.ClientOrderID)
		}
		seen[a.ClientOrderID] = true
	}
	found := false
	for _, d := range rb.Outcome.Deviations {
		if d.Kind == "partial_fill" && strings.Contains(d.Detail, first.StrategyID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the underfill of %s must be recorded as a deviation: %+v", first.StrategyID, rb.Outcome.Deviations)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM entry_order_intents WHERE status = 'committed'`); n < 1 {
		t.Fatalf("live-shaped increases must commit their entry intents in the owning file, got %d", n)
	}
}
func TestBasketPartialIncreaseConsumesTheDecisionCandle(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	venue := newScriptedBasketVenue(false)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenuePartial, FilledQty: -0.5}}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if len(venue.submits) == 0 {
		t.Fatal("no submission")
	}
	pid := venue.submits[0].StrategyID
	count := func() int {
		n := 0
		for _, a := range venue.submits {
			if a.StrategyID == pid {
				n++
			}
		}
		return n
	}
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if n := count(); n != 1 {
		t.Fatalf("a top-up must not reuse the consumed decision candle; %s submissions %d", pid, n)
	}
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb.Status != basketRebalanceExecuting {
		t.Fatalf("the underfilled rebalance must stay executing, got %s", rb.Status)
	}
	next := deadline.Add(time.Hour)
	env.cycle(next.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 1), true)
	if n := count(); n != 2 {
		t.Fatalf("the underfill should be retried from the next eligible candle; %s submissions %d", pid, n)
	}
	var coin string
	for _, m := range basketTestMembers {
		if m.ID == pid {
			coin = m.Coin
		}
	}
	if got := env.signedQty(pid, coin); !basketQtyEqual(got, rb.targets.Members[pid].TargetQty) {
		t.Fatalf("%s holds %.10g after the second tranche, target %.10g", pid, got, rb.targets.Members[pid].TargetQty)
	}
	if rb.Status != basketRebalanceCompleted || rb.Outcome.Result != "completed_with_deviations" {
		t.Fatalf("the rebalance should complete with the recorded underfill deviation: %s %s", rb.Status, rb.Outcome.Result)
	}
}
func TestBasketProtectiveExitRecordsExceptionAndHoldsIncreases(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	rt := env.rt()
	rb := rt.Rebalances[deadline.UnixMilli()]
	if rb.Status != basketRebalanceCompleted {
		t.Fatalf("setup rebalance did not complete: %s", rb.Status)
	}
	env.mu.Lock()
	for _, id := range []string{"hl-bsk-sol", "hl-bsk-avax"} {
		coin := strings.ToUpper(strings.TrimPrefix(id, "hl-bsk-"))
		if !recordPerpsStopLossClose(env.state.Strategies[id], coin, basketTestMarks()[coin]*1.05, "stop_loss", &StrategyLogger{stratID: id, writer: os.Stdout}) {
			env.mu.Unlock()
			t.Fatalf("could not record the protective stop close for %s", id)
		}
	}
	env.mu.Unlock()
	submits := len(venue.submits)
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if len(rb.Outcome.Exceptions) != 2 {
		t.Fatalf("both protective exits must be recorded: %+v", rb.Outcome.Exceptions)
	}
	if rb.Outcome.Result != "protective_exception" {
		t.Fatalf("a protective exit must never report a bounded success, result %q", rb.Outcome.Result)
	}
	if !strings.Contains(rb.Outcome.Exceptions[0].Breach, "net") {
		t.Fatalf("closing both short legs breaches the $1100 net bound; breach %q", rb.Outcome.Exceptions[0].Breach)
	}
	for _, id := range []string{"hl-bsk-sol", "hl-bsk-avax"} {
		if rt.Expected[id] != 0 {
			t.Fatalf("the exception must acknowledge the residual book for %s, expected %.10g", id, rt.Expected[id])
		}
	}
	for _, a := range venue.submits[submits:] {
		if a.Direction != basketDirectionReduce {
			t.Fatalf("after a protective exit only reductions may run, got %+v", a)
		}
	}
	exp := basketExposure{Qty: map[string]float64{}, Mark: basketTestMarks()}
	for _, m := range basketTestMembers {
		exp.Qty[m.Coin] = env.signedQty(m.ID, m.Coin)
	}
	if v := exp.reading().violations(basketBoundsFor(rt.Contract)); len(v) > 0 {
		t.Fatalf("reduce-only recovery left a breach: %v", v)
	}
	if rb.Outcome.RecoveryOpen {
		t.Fatal("recovery should close once the net bound holds")
	}
	env.cycle(deadline.Add(3*time.Minute), deadline, snap, true)
	if len(rb.Outcome.Exceptions) != 2 {
		t.Fatalf("each exit must be recorded once, got %d", len(rb.Outcome.Exceptions))
	}
}
func TestBasketExceptionDuringExecutionStartsReduceOnlyRecovery(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	rt := env.rt()
	rb := rt.Rebalances[deadline.UnixMilli()]
	if rb.Status != basketRebalanceExecuting {
		t.Fatalf("a $400 net bound should leave the first candle executing, got %s", rb.Status)
	}
	var shortID, shortCoin string
	for _, m := range basketTestMembers {
		if env.signedQty(m.ID, m.Coin) < 0 {
			shortID, shortCoin = m.ID, m.Coin
		}
	}
	if shortID == "" {
		t.Fatal("no short leg was opened in the first candle")
	}
	env.mu.Lock()
	recordPerpsStopLossClose(env.state.Strategies[shortID], shortCoin, 1e9, "stop_loss", &StrategyLogger{stratID: shortID, writer: os.Stdout})
	env.mu.Unlock()
	submits := len(venue.submits)
	next := deadline.Add(time.Hour)
	env.cycle(next.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 1), true)
	if !rb.IncreasesFrozen {
		t.Fatal("an exit during execution must freeze increases")
	}
	reductions := 0
	for _, a := range venue.submits[submits:] {
		if a.Direction == basketDirectionIncrease {
			t.Fatalf("an increase was sent during reduce-only recovery: %+v", a)
		}
		reductions++
	}
	exp := basketExposure{Qty: map[string]float64{}, Mark: basketTestMarks()}
	for _, m := range basketTestMembers {
		exp.Qty[m.Coin] = env.signedQty(m.ID, m.Coin)
	}
	if v := exp.reading().violations(basketBoundsFor(rt.Contract)); len(v) > 0 {
		t.Fatalf("reduce-only recovery left a breach: %v (reductions %d)", v, reductions)
	}
}

func TestBasketHoldsWholeTargetSetOnDegradedInputs(t *testing.T) {
	deadline := basketTestDeadline()
	cases := []struct {
		name   string
		snap   func() *marketSnapshot
		ranker basketRankRunner
		want   string
	}{
		{"missing member frame", func() *marketSnapshot {
			s := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
			delete(s.keys, feedKeyFor("SOL", "1h"))
			return s
		}, basketTestRanker(basketTestWeights, ""), "cannot supply every member frame"},
		{"no snapshot", func() *marketSnapshot { return nil }, basketTestRanker(basketTestWeights, ""), "no sealed market snapshot"},
		{"ranker failure", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, func(context.Context, []byte, time.Duration) ([]byte, []byte, error) {
			return nil, nil, errors.New("exit status 1")
		}, "the ranker failed"},
		{"ranker timeout", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, func(context.Context, []byte, time.Duration) ([]byte, []byte, error) {
			return nil, nil, &pythonScriptTimeoutError{d: time.Second}
		}, "the ranker timed out"},
		{"ranker hold", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, basketTestRanker(nil, "scripted hold"), "scripted hold"},
		{"invalid json", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, func(context.Context, []byte, time.Duration) ([]byte, []byte, error) {
			return []byte("{not json"), nil, nil
		}, "unreadable JSON"},
		{"wrong generation", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, func(ctx context.Context, in []byte, d time.Duration) ([]byte, []byte, error) {
			out, _, err := basketTestRanker(basketTestWeights, "")(ctx, in, d)
			return []byte(strings.Replace(string(out), `"generation":1`, `"generation":2`, 1)), nil, err
		}, "belongs to snapshot"},
		{"cap infeasible", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, basketTestRanker(map[string]float64{"BTC": 0.5, "ETH": 0.0, "SOL": -0.25, "AVAX": -0.25}, ""), "per-coin cap"},
		{"empty short side", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, basketTestRanker(map[string]float64{"BTC": 0.25, "ETH": 0.25}, ""), "short side is empty"},
		{"weights above one", func() *marketSnapshot { return basketTestSnapshot(deadline, 60, basketTestMarks(), 1) }, basketTestRanker(map[string]float64{"BTC": 0.3, "ETH": 0.3, "SOL": -0.3, "AVAX": -0.3}, ""), "above 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadBasketTestConfig(t, nil)
			env := newBasketTestEnv(t, cfg, tc.ranker, nil)
			env.cycle(deadline.Add(time.Minute), deadline, tc.snap(), true)
			rb := env.rt().Rebalances[deadline.UnixMilli()]
			if rb == nil || rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, tc.want) {
				t.Fatalf("want a held deadline naming %q, got %+v", tc.want, rb)
			}
			for _, m := range basketTestMembers {
				if q := env.signedQty(m.ID, m.Coin); q != 0 {
					t.Fatalf("%s traded %.10g during a whole-set hold", m.ID, q)
				}
			}
			if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances WHERE status = 'held'`); n != 1 {
				t.Fatalf("held rows %d, want 1", n)
			}
		})
	}
}

func TestBasketSaveFailureHoldsTheNextElectiveAction(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	saves := 0
	prev := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		saves++
		if len(venue.submits) == 1 && saves > 0 {
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if len(venue.submits) != 1 {
		t.Fatalf("a failed settlement save must stop further elective actions; submissions %d", len(venue.submits))
	}
	rt := env.rt()
	if !rt.needsDurable {
		t.Fatal("the coordinator must remember that the settlement is not durable")
	}
	storeCommitHook = prev
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if len(venue.submits) != 1 {
		t.Fatalf("no elective action may run before the settlement saves; submissions %d", len(venue.submits))
	}
	for part, err := range env.store.SaveAll(env.state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}
	if rt.needsDurable {
		t.Fatal("a successful full save must make the settlement durable")
	}
	env.cycle(deadline.Add(3*time.Minute), deadline, snap, true)
	if len(venue.submits) != 4 {
		t.Fatalf("after the durable save the rebalance should finish; submissions %d", len(venue.submits))
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("persisted opens %d, want 4 (one per fill, none duplicated)", n)
	}
}

func TestBasketCrashBeforeCommitLeavesNoBookingAndRecovers(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketTestDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	prev := storeCommitHook
	fail := false
	storeCommitHook = func(role storageRole) error {
		if fail {
			return errors.New("crash")
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	origVenue := env.bc.venueFor
	env.bc.venueFor = func(sc StrategyConfig) (basketVenue, error) {
		v, err := origVenue(sc)
		if err != nil {
			return nil, err
		}
		return crashAfterSubmitVenue{basketVenue: v, arm: func() { fail = true }}, nil
	}
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	fail = false
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), nil)
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s holds %.10g after a crash before the settlement commit", m.ID, q)
		}
	}
	a := env.rt().pendingAction()
	if a == nil || a.Status != basketActionSubmitting {
		t.Fatalf("want the submitting action from before the crash, got %+v", a)
	}
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if got := env.rt().Actions[a.ActionID]; got.Status != basketActionNoFill {
		t.Fatalf("a paper action that never committed must resolve as no fill, got %s", got.Status)
	}
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb.Status == basketRebalanceCompleted {
		t.Fatal("the candle first observed after a restart with no durable entry record is a bootstrap hold; it must not fund increases")
	}
	next := deadline.Add(time.Hour)
	env.cycle(next.Add(time.Minute), next, basketTestSnapshot(next, 60, basketTestMarks(), 1), true)
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb.Status != basketRebalanceCompleted {
		t.Fatalf("recovery should finish the frozen targets on the next candle, got %s", rb.Status)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("persisted opens %d, want 4", n)
	}
}

type crashAfterSubmitVenue struct {
	basketVenue
	arm func()
}

func (v crashAfterSubmitVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	out, err := v.basketVenue.Submit(a)
	v.arm()
	return out, err
}

func TestBasketLiveRefusalHoldsAtDispatchAndOperatorRoutes(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), func(sc StrategyConfig) (basketVenue, error) { return venue, nil })
	for _, ack := range []bool{false, true} {
		for i := range cfg.Strategies {
			if cfg.Strategies[i].ID == "hl-bsk-btc" {
				cfg.Strategies[i].Args = []string{"basket_member", "BTC", "1h", "--mode=live"}
				v := ack
				cfg.Strategies[i].AllowNoEdge = &v
			}
		}
		env.bc.reload(cfg)
		deadline := basketTestDeadline()
		env.cycle(deadline.Add(time.Minute), deadline, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true)
		if len(venue.submits) != 0 || env.rt().latest() != nil {
			t.Fatalf("allow_no_edge=%v: a live member must refuse before any reservation or order", ack)
		}
		if _, err := basketVenueFor(env.bc.members["hl-bsk-btc"]); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("allow_no_edge=%v: the production venue factory must refuse a live member, got %v", ack, err)
		}
	}
	if _, _, err := buildAddStrategyEntry("basket_scripted_fixture", "hyperliquid", "BTC"); err == nil {
		t.Fatal("the operator add route must refuse a basket evaluator")
	}
	ss := NewStatusServer(env.state, env.mu, "", cfg.Strategies, env.store)
	if reason := ss.paperToLiveBlockedReason("hl-bsk-eth", false); !strings.Contains(reason, "live basket dispatch is refused") {
		t.Fatalf("paper-to-live must refuse a basket member, got %q", reason)
	}
	out := resolveEntryAttemptByOperator(entryOperatorDeps{state: env.state, mu: env.mu, store: env.store, strategy: func(id string) (StrategyConfig, bool) {
		for _, sc := range cfg.Strategies {
			if sc.ID == id {
				return sc, true
			}
		}
		return StrategyConfig{}, false
	}}, "owner", "hl-bsk-eth", "ei-"+strings.Repeat("0", 24))
	if !strings.Contains(out.Message, "basket coordinator") {
		t.Fatalf("resolve-entry must refuse a basket member, got %q", out.Message)
	}
}

func basketDelistedSOLConfig(t *testing.T, deadline time.Time) *Config {
	t.Helper()
	delisted := deadline.Add(-time.Hour).UTC().Format(time.RFC3339)
	return loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["max_transition_net_usd"] = 1100
		b["eligibility"].(map[string]interface{})["listings"].(map[string]interface{})["SOL"] = map[string]interface{}{
			"listed_at": "2020-01-01T00:00:00Z", "known_at": delisted, "delisted_at": delisted,
		}
	})
}

var basketNoSOLWeights = map[string]float64{"BTC": 0.25, "ETH": 0.25, "AVAX": -0.25}

func TestBasketListingExcludedMemberNeedsNoFrameOrMark(t *testing.T) {
	deadline := basketTestDeadline()
	cfg := basketDelistedSOLConfig(t, deadline)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketListingTestRanker(basketNoSOLWeights), venue.factory())
	rt := env.rt()
	sol, _ := rt.Contract.memberByCoin("SOL")
	if why := basketListingExclusion(rt.Contract, sol, deadline.UnixMilli()); why != "delisted at or before the deadline" {
		t.Fatalf("the declared delisting must exclude SOL at the deadline, got %q", why)
	}
	reqs := env.bc.cycleRequirements(map[string]time.Time{"xs-test": deadline}, feedRequirements{})
	for _, k := range reqs.Keys {
		if k.Key.Symbol == "SOL" {
			t.Fatalf("the cycle requirements still request the excluded SOL frame: %+v", reqs.Keys)
		}
	}
	snap := basketTestSnapshot(deadline, 60, basketMarksWithout("SOL"), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	rb := rt.Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("a delisted, flat SOL with no frame and no mark must not block the rebalance: %+v", rb)
	}
	targets, err := rb.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if got := targets.Members["hl-bsk-sol"].Excluded; got != "delisted at or before the deadline" {
		t.Fatalf("the SOL exclusion reason is %q", got)
	}
	for _, m := range basketTestMembers {
		got := env.signedQty(m.ID, m.Coin)
		if m.Coin == "SOL" {
			if got != 0 {
				t.Fatalf("the excluded SOL member traded %.10g", got)
			}
			continue
		}
		if got == 0 || !basketQtyEqual(got, targets.Members[m.ID].TargetQty) {
			t.Fatalf("%s holds %.10g, target %.10g", m.ID, got, targets.Members[m.ID].TargetQty)
		}
	}

	env.mu.Lock()
	if !recordPerpsStopLossClose(env.state.Strategies["hl-bsk-avax"], "AVAX", 31.5, "stop_loss", &StrategyLogger{stratID: "hl-bsk-avax", writer: os.Stdout}) {
		env.mu.Unlock()
		t.Fatal("could not record the protective AVAX stop close")
	}
	env.mu.Unlock()
	submits := len(venue.submits)
	env.cycle(deadline.Add(2*time.Minute), deadline, snap, true)
	if len(rb.Outcome.Exceptions) != 1 || !strings.Contains(rb.Outcome.Exceptions[0].Breach, "net") {
		t.Fatalf("closing the AVAX short breaches the $1100 net bound, and the breach must be known without a SOL mark: %+v", rb.Outcome.Exceptions)
	}
	reductions := 0
	for _, a := range venue.submits[submits:] {
		if a.Direction != basketDirectionReduce || a.Symbol == "SOL" {
			t.Fatalf("reduce-only recovery may only reduce the priced members, got %+v", a)
		}
		reductions++
	}
	if reductions == 0 {
		t.Fatal("reduce-only recovery sent no reduction while SOL was flat and unpriced")
	}
	exp := basketExposure{Qty: map[string]float64{}, Mark: basketMarksWithout("SOL")}
	for _, m := range basketTestMembers {
		exp.Qty[m.Coin] = env.signedQty(m.ID, m.Coin)
	}
	if v := exp.reading().violations(basketBoundsFor(rt.Contract)); len(v) > 0 {
		t.Fatalf("reduce-only recovery left a breach: %v", v)
	}
	if rb.Outcome.RecoveryOpen {
		t.Fatal("recovery should close once the net bound holds")
	}
}

func TestBasketUnpricedHeldMemberHoldsAndAlerts(t *testing.T) {
	deadline := basketTestDeadline()
	cfg := basketDelistedSOLConfig(t, deadline)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketListingTestRanker(basketNoSOLWeights), venue.factory())
	env.mu.Lock()
	env.state.Strategies["hl-bsk-sol"].Positions["SOL"] = &Position{Symbol: "SOL", Quantity: 2, AvgCost: 150, Side: "long", Multiplier: 1}
	env.mu.Unlock()
	env.rt().Expected["hl-bsk-sol"] = 2
	env.cycle(deadline.Add(time.Minute), deadline, basketTestSnapshot(deadline, 60, basketMarksWithout("SOL"), 1), true)
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceFrozen {
		t.Fatalf("the deadline must still rank and freeze, got %+v", rb)
	}
	if len(venue.submits) != 0 {
		t.Fatalf("no elective order may run while a held member has no mark: %+v", venue.submits)
	}
	if !rb.Outcome.alerted("unpriced-held|hl-bsk-sol") {
		t.Fatal("a held position in a delisted coin with no mark must raise the CRITICAL alert")
	}
}

func TestBasketRankerEligibilityMustMatchTheDeclaredListing(t *testing.T) {
	deadline := basketTestDeadline()
	cfg := basketDelistedSOLConfig(t, deadline)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	env.cycle(deadline.Add(time.Minute), deadline, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true)
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "declared listing evidence") {
		t.Fatalf("a ranker that reports a declared-delisted member as eligible must hold the deadline, got %+v", rb)
	}
}

func TestBasketDeadlineWaitsForASealAfterTheBoundary(t *testing.T) {
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), newScriptedBasketVenue(false).factory())
	deadline := basketTestDeadline()
	early := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	early.SealedAt, early.Deadline = deadline.Add(-2*time.Second), time.Time{}
	env.cycle(deadline.Add(3*time.Second), deadline, early, false)
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb != nil {
		t.Fatalf("a snapshot sealed before the deadline must reserve nothing, got %+v", rb)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_rebalances`); n != 0 {
		t.Fatalf("rows reserved %d, want 0", n)
	}
	env.cycle(deadline.Add(time.Minute), deadline, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true)
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the next cycle inside the window must rank the deadline, got %+v", rb)
	}

	cfg2 := loadBasketTestConfig(t, nil)
	env2 := newBasketTestEnv(t, cfg2, basketTestRanker(basketTestWeights, ""), nil)
	onTime := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	onTime.SealedAt, onTime.Deadline = deadline.Add(time.Second), time.Time{}
	env2.cycle(deadline.Add(time.Second), deadline, onTime, false)
	if rb := env2.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status == basketRebalanceHeld || rb.TargetsJSON == "" {
		t.Fatalf("a cycle that starts after the boundary must rank the deadline in that cycle, got %+v", rb)
	}
}
