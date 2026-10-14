package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFillCostPolicyIdentityRoundTripsThroughStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("ALTER TABLE trades DROP COLUMN cost_policy_id"); err != nil {
		t.Fatalf("simulate a pre-replay state file: %v", err)
	}
	if _, err := db.db.Exec("INSERT INTO app_state (id, cycle_count) VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('s1', 'perps', 'hyperliquid', 1000, 1000)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, cost_model_version)
		VALUES ('s1', ?, 'BTC', 'buy', 0.1, 50000, 5000, 'perps', 'legacy open', 1)`, formatTime(time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		db, err = OpenStateDB(path)
		if err != nil {
			t.Fatalf("migration pass %d: %v", i, err)
		}
		if i == 0 {
			db.Close()
		}
	}
	defer db.Close()
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	s := loaded.Strategies["s1"]
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].CostModelVersion != FillCostModelVersion || s.TradeHistory[0].CostPolicyID != "" {
		t.Fatalf("a migrated default fill must keep version %d and no policy id: %+v", FillCostModelVersion, s.TradeHistory)
	}
	policy, err := resolveFillCostPolicy(fillCostPolicy{TakerFeePct: 0.001, SlippagePct: 0.002, FillRole: fillCostRoleTakerAll, Spread: fillCostSpreadInSlippage}, "run-fp")
	if err != nil {
		t.Fatal(err)
	}
	if policy.ModelVersion != FillCostModelReplayCustomVersion || policy.PolicyID == "" {
		t.Fatalf("a non-default policy must carry the replay model identity: %+v", policy)
	}
	same, _ := resolveFillCostPolicy(defaultFillCostPolicy().rates(), "run-fp")
	if same != defaultFillCostPolicy() {
		t.Fatalf("a policy equal to the default paper costs must stay the frozen default model: %+v", same)
	}
	if err := bindFillCostPolicy("storage-test", policy); err != nil {
		t.Fatal(err)
	}
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	func() {
		defer unbindFillCostPolicy("storage-test")
		defer func() { tradeRecorder = prevRecorder }()
		if err := bindFillCostPolicy("other", policy); err == nil {
			t.Fatal("a second cost policy binding must be refused")
		}
		RecordTrade(s, Trade{Timestamp: time.Date(2025, 9, 2, 0, 0, 0, 0, time.UTC), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50100, Value: 5010, TradeType: "perps", Details: "replay open"})
		if got := ApplyAdverseSlippage(100, true); got != 100*(1+policy.SlippagePct) {
			t.Fatalf("the bound slippage %.10g was not applied once: %.10g", policy.SlippagePct, got)
		}
		if got := CalculatePlatformSpotFee("hyperliquid", 1000); got != 1000*policy.TakerFeePct {
			t.Fatalf("the bound taker rate was not applied: %.10g", got)
		}
	}()
	if err := db.SaveState(loaded); err != nil {
		t.Fatal(err)
	}
	if got := ApplyAdverseSlippage(100, true); got != 100*(1+SlippagePct) {
		t.Fatalf("unbinding must restore the default slippage, got %.10g", got)
	}
	reloaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	hist := reloaded.Strategies["s1"].TradeHistory
	if len(hist) != 2 {
		t.Fatalf("want two stored trades, got %d", len(hist))
	}
	byDetail := map[string]Trade{}
	for _, tr := range hist {
		byDetail[tr.Details] = tr
	}
	if tr := byDetail["replay open"]; tr.CostModelVersion != FillCostModelReplayCustomVersion || tr.CostPolicyID != policy.PolicyID {
		t.Fatalf("the replay fill lost its cost identity after save and load: %+v", tr)
	}
	if tr := byDetail["legacy open"]; tr.CostModelVersion != FillCostModelVersion || tr.CostPolicyID != "" {
		t.Fatalf("the default fill changed identity: %+v", tr)
	}
	rows, _, err := singleFileStore(db).QueryTradeHistory("s1", "", time.Time{}, time.Time{}, 10, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("paged read: %v %d", err, len(rows))
	}
	for _, tr := range rows {
		if tr.Details == "replay open" && tr.CostPolicyID != policy.PolicyID {
			t.Fatalf("the paged trade read dropped the policy link: %+v", tr)
		}
	}
	read, err := db.readLedgerSnapshot("s1", 10)
	if err != nil {
		t.Fatalf("ledger snapshot read: %v", err)
	}
	versions := map[string]int64{}
	for _, row := range read.Trades {
		d, _ := row.Values["details"].(string)
		switch v := row.Values["cost_model_version"].(type) {
		case int64:
			versions[d] = v
		}
	}
	if versions["replay open"] != FillCostModelReplayCustomVersion || versions["legacy open"] != FillCostModelVersion {
		t.Fatalf("the ledger export read must keep the replay cost identity distinct: %v", versions)
	}
}

func TestBasketBaselineTargetsRefuseOutsideTheReplayAdapter(t *testing.T) {
	run := basketBaselineRankRunner(map[int64]map[string]float64{1: {"BTC": 0.5}})
	if _, _, err := run(context.Background(), []byte(`{}`), time.Second); err == nil {
		t.Fatal("the baseline target path must refuse to run without the replay adapter's clock binding")
	}
	if err := bindTradingClock("someone-else", time.Now); err != nil {
		t.Fatal(err)
	}
	defer unbindTradingClock("someone-else")
	if _, _, err := run(context.Background(), []byte(`{}`), time.Second); err == nil {
		t.Fatal("another clock owner must not unlock the baseline target path")
	}
	if basketOfflineBaselineAllowed() {
		t.Fatal("the baseline path reported allowed under a foreign clock owner")
	}
}

func TestStorageInspectionRefusesOfflineReplayState(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	store := openSourceStore(t, cfg)
	file := store.primary()
	if err := file.ensureBasketReplaySchema(); err != nil {
		t.Fatal(err)
	}
	if err := file.ensureBasketReplaySchema(); err != nil {
		t.Fatalf("the replay schema must be idempotent: %v", err)
	}
	policy, _ := resolveFillCostPolicy(fillCostPolicy{TakerFeePct: 0.001, SlippagePct: 0.001, FillRole: fillCostRoleTakerAll, Spread: fillCostSpreadInSlippage}, "fp")
	m := basketReplayManifest{Owner: basketReplayOwnerMarker, RunID: "inspect", Fingerprints: basketReplayFingerprints{Protocol: basketReplayProtocolFingerprint(), Run: "fp"}, CostPolicyID: policy.PolicyID, CostVersion: policy.ModelVersion, StepsTotal: 3}
	if err := file.insertBasketReplayRun(m, policy); err != nil {
		t.Fatal(err)
	}
	if err := file.insertBasketReplayRun(m, policy); err == nil {
		t.Fatal("a second run record in one replay state file must be refused")
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
	if si.OK() {
		t.Fatal("the scheduler's storage inspection must refuse a file that holds offline replay state")
	}
	var fi storageFileInspection
	for _, f := range si.Files {
		if f.Role == storageRolePrimary {
			fi = f
		}
	}
	if len(fi.ReplayRuns) != 1 || len(fi.FillCostPolicies) != 1 {
		t.Fatalf("storage inspection must report the replay run and its cost policy evidence: runs=%v policies=%v", fi.ReplayRuns, fi.FillCostPolicies)
	}
	blob, _ := json.Marshal(fi)
	var back storageFileInspection
	if err := json.Unmarshal(blob, &back); err != nil || len(back.FillCostPolicies) != 1 {
		t.Fatalf("the inspection JSON must carry the policy evidence: %s", blob)
	}
}

func basketOfflineRankEnv(t *testing.T, run basketRankRunner) (*basketTestEnv, time.Time) {
	t.Helper()
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		basketTestBasket(root)["max_transition_net_usd"] = 1100
	})
	env := newBasketTestEnv(t, cfg, run, nil)
	env.bc.rankClock = basketRankClockSimulated
	return env, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
}

func (e *basketTestEnv) offlineStep(now, deadline time.Time, snap *marketSnapshot, once bool) {
	e.t.Helper()
	e.now = now
	e.bc.beginCycle()
	e.bc.step(basketCycleInputs{Now: now, Snapshot: snap, SnapshotFor: func(*basketRuntime, time.Time) *marketSnapshot { return snap }, Once: once})
}

func TestBasketOfflineRankClockRunsHistoricalWindows(t *testing.T) {
	calls := int32(0)
	counting := func(inner basketRankRunner) basketRankRunner {
		return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
			atomic.AddInt32(&calls, 1)
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= 0 {
				return nil, nil, context.DeadlineExceeded
			}
			return inner(ctx, stdin, timeout)
		}
	}
	env, deadline := basketOfflineRankEnv(t, counting(basketTestRanker(basketTestWeights, "")))
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("a historical deadline must rank on the simulated window instead of expiring on the system clock: %+v", rb)
	}

	runtimeEnv, _ := basketOfflineRankEnv(t, counting(basketTestRanker(basketTestWeights, "")))
	runtimeEnv.bc.rankClock = basketRankClockRuntime
	runtimeEnv.cycle(deadline.Add(time.Minute), deadline, snap, true)
	if rb := runtimeEnv.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status == basketRebalanceCompleted {
		t.Fatalf("the runtime clock policy keeps its system-clock window: a historical deadline must not complete, got %+v", rb)
	}

	atEnd, _ := basketOfflineRankEnv(t, counting(basketTestRanker(basketTestWeights, "")))
	before := atomic.LoadInt32(&calls)
	windowEnd := deadline.Add(time.Duration(atEnd.rt().Contract.EvaluationWindowSeconds) * time.Second)
	atEnd.offlineStep(windowEnd, deadline, snap, true)
	if rb := atEnd.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld {
		t.Fatalf("a step at the simulated window end must hold the deadline: %+v", rb)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Fatal("a step at the simulated window end started a ranker job")
	}
}

func TestBasketOfflineRankClockHoldsLateCancelledAndTimedOutResults(t *testing.T) {
	release := make(chan struct{})
	late, deadline := basketOfflineRankEnv(t, func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		<-release
		return basketTestRanker(basketTestWeights, "")(ctx, stdin, timeout)
	})
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	late.offlineStep(deadline.Add(time.Minute), deadline, snap, false)
	close(release)
	job := late.rt().rank
	if job == nil {
		t.Fatal("the ranker job did not start")
	}
	<-job.done
	windowEnd := deadline.Add(time.Duration(late.rt().Contract.EvaluationWindowSeconds) * time.Second)
	late.offlineStep(windowEnd, deadline, snap, false)
	if rb := late.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
		t.Fatalf("a result collected at the simulated window end must hold without targets: %+v", rb)
	}

	cancelled, _ := basketOfflineRankEnv(t, func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		<-ctx.Done()
		return basketTestRanker(basketTestWeights, "")(context.Background(), stdin, timeout)
	})
	nearEnd := windowEnd.Add(-1500 * time.Millisecond)
	cancelled.offlineStep(nearEnd, deadline, snap, true)
	if rb := cancelled.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
		t.Fatalf("valid JSON from a cancelled run must never freeze targets: %+v", rb)
	}

	timedOut, _ := basketOfflineRankEnv(t, func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		out, _, err := basketTestRanker(basketTestWeights, "")(ctx, stdin, timeout)
		if err != nil {
			return nil, nil, err
		}
		return out, nil, &pythonScriptTimeoutError{d: timeout}
	})
	timedOut.offlineStep(deadline.Add(time.Minute), deadline, snap, true)
	rb := timedOut.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
		t.Fatalf("valid JSON from a timed-out run must never freeze targets: %+v", rb)
	}
	for _, m := range basketTestMembers {
		for _, env := range []*basketTestEnv{late, cancelled, timedOut} {
			if q := env.signedQty(m.ID, m.Coin); q != 0 {
				t.Fatalf("%s traded %.10g after a refused ranker result", m.ID, q)
			}
		}
	}
}

func TestBasketReplayTradeExportReadsEveryStoredRow(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('m1', 'perps', 'hyperliquid', 1000, 1000)"); err != nil {
		t.Fatal(err)
	}
	const rows = 3*basketReplayTradePage + 7
	start := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < rows; i++ {
		if _, err := db.db.Exec(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, realized_pnl, cost_model_version)
			VALUES ('m1', ?, 'BTC', 'funding', 0, 0, 0, ?, ?, ?, ?, 1)`, formatTime(start.Add(time.Duration(i)*time.Hour)), TradeTypeFunding, fmt.Sprintf("row %d", i), fmt.Sprintf("f-%d", i), float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	r := &basketReplayRuntime{store: singleFileStore(db), members: []StrategyConfig{{ID: "m1"}}}
	out, err := r.storedTrades()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != rows {
		t.Fatalf("the replay output must carry every booked trade, got %d of %d", len(out), rows)
	}
	for i, tr := range out {
		if tr.Details != fmt.Sprintf("row %d", i) {
			t.Fatalf("trade %d is %q: the export must keep booking order", i, tr.Details)
		}
	}
}
