package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type basketTestMember struct {
	ID     string
	Coin   string
	Mark   float64
	Source string
}

var basketTestMembers = []basketTestMember{
	{ID: "hl-bsk-avax", Coin: "AVAX", Mark: 30},
	{ID: "hl-bsk-btc", Coin: "BTC", Mark: 60000},
	{ID: "hl-bsk-eth", Coin: "ETH", Mark: 3000},
	{ID: "hl-bsk-sol", Coin: "SOL", Mark: 150},
}

var basketTestLots = map[string]int{"AVAX": 2, "BTC": 5, "ETH": 4, "SOL": 2, "DOGE": 0}

func basketTestConfigJSON(t *testing.T, dir string, mutate func(root map[string]interface{})) string {
	t.Helper()
	var strategies []interface{}
	var memberIDs []string
	listings := map[string]interface{}{}
	for _, m := range basketTestMembers {
		memberIDs = append(memberIDs, m.ID)
		listings[m.Coin] = map[string]interface{}{"listed_at": "2020-01-01T00:00:00Z", "known_at": "2020-01-01T00:00:00Z"}
		strategies = append(strategies, map[string]interface{}{
			"id":                   m.ID,
			"basket_id":            "xs-test",
			"type":                 "perps",
			"platform":             "hyperliquid",
			"script":               "shared_scripts/check_hyperliquid.py",
			"args":                 []interface{}{"basket_member", m.Coin, "1h", "--mode=paper"},
			"close_strategy":       map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.5, "close_fraction": 1.0}}}},
			"closed_bar_decisions": true,
			"direction":            "both",
			"capital":              5000,
			"leverage":             1,
			"max_drawdown_pct":     90,
		})
	}
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 3600,
		"db_file":          filepath.Join(dir, "state.db"),
		"log_dir":          filepath.Join(dir, "logs"),
		"market_feed":      "websocket",
		"strategies":       strategies,
		"baskets": []interface{}{map[string]interface{}{
			"id":                         "xs-test",
			"ranker":                     "basket_scripted_fixture",
			"params":                     map[string]interface{}{"weights": map[string]interface{}{"BTC": 0.25, "ETH": 0.25, "SOL": -0.25, "AVAX": -0.25}},
			"members":                    memberIDs,
			"rebalance_interval_seconds": 86400,
			"evaluation_window_seconds":  1800,
			"ranker_timeout_seconds":     60,
			"lookback_bars":              48,
			"gross_budget_usd":           4000,
			"max_transition_net_usd":     400,
			"max_coin_notional_usd":      1500,
			"min_eligible_members":       3,
			"eligibility": map[string]interface{}{
				"source":           "operator listing ledger",
				"version":          "2026-10-01",
				"min_history_bars": 24,
				"listings":         listings,
			},
		}},
	}
	if mutate != nil {
		mutate(root)
	}
	blob, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func basketTestStrategy(root map[string]interface{}, id string) map[string]interface{} {
	for _, raw := range root["strategies"].([]interface{}) {
		sc := raw.(map[string]interface{})
		if sc["id"] == id {
			return sc
		}
	}
	return nil
}

func basketTestBasket(root map[string]interface{}) map[string]interface{} {
	return root["baskets"].([]interface{})[0].(map[string]interface{})
}

func loadBasketTestConfig(t *testing.T, mutate func(root map[string]interface{})) *Config {
	t.Helper()
	dir := t.TempDir()
	path := basketTestConfigJSON(t, dir, mutate)
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatalf("LoadConfigReadOnly: %v", err)
	}
	return cfg
}

func basketTestSnapshot(deadline time.Time, bars int, marks map[string]float64, generation uint64) *marketSnapshot {
	sealedAt := deadline.Add(30 * time.Second)
	snap := &marketSnapshot{
		Version:          marketSnapshotVersion,
		EvaluationID:     fmt.Sprintf("test/%d", deadline.Unix()),
		ConfigGeneration: generation,
		SealedAt:         sealedAt,
		Deadline:         deadline,
		keys:             make(map[marketFeedKey]*marketSnapshotKey),
		mids:             make(map[string]feedMid),
		funding:          make(map[string]feedFunding),
		observations:     make(map[feedObservationKey]*marketSnapshotObservation),
	}
	const interval = int64(3_600_000)
	coins := make([]string, 0, len(marks))
	for c := range marks {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for ci, coin := range coins {
		mark := marks[coin]
		key := feedKeyFor(coin, "1h")
		entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
		start := deadline.UnixMilli() - int64(bars)*interval
		for i := 0; i <= bars; i++ {
			open := start + int64(i)*interval
			px := mark * (1 + 0.002*math.Sin(float64(i+ci)/3))
			b := feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: i < bars, Open: px, High: px * 1.004, Low: px * 0.996, Close: px * 1.001, Volume: 100 + float64(i)}
			if i == bars {
				b.Close = mark
			}
			entry.Bars = append(entry.Bars, b)
		}
		entry.Readiness = feedKeyReadiness{Key: key, Bars: len(entry.Bars), Required: len(entry.Bars), Ready: true, Status: feedStatusReady, Source: "test"}
		snap.keys[key] = entry
		snap.mids[coin] = feedMid{Px: mark, RecvAt: sealedAt, Source: "test"}
	}
	return snap
}

func basketTestMarks() map[string]float64 {
	out := make(map[string]float64, len(basketTestMembers))
	for _, m := range basketTestMembers {
		out[m.Coin] = m.Mark
	}
	return out
}

type basketTestEnv struct {
	t     *testing.T
	cfg   *Config
	store *StateStore
	state *AppState
	mu    *sync.RWMutex
	bc    *basketCoordinator
	now   time.Time
}

func newBasketTestEnv(t *testing.T, cfg *Config, rank basketRankRunner, venue func(StrategyConfig) (basketVenue, error)) *basketTestEnv {
	t.Helper()
	useHLLotMetadataForTest(t, basketTestLots)
	store := openSourceStore(t, cfg)
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	env := &basketTestEnv{t: t, cfg: cfg, store: store, state: state, mu: &sync.RWMutex{}}
	env.reopen(rank, venue, false)
	return env
}

func (e *basketTestEnv) reopen(rank basketRankRunner, venue func(StrategyConfig) (basketVenue, error), persisted bool) {
	e.t.Helper()
	known := make(map[string]bool)
	if persisted {
		for id := range e.state.Strategies {
			known[id] = true
		}
	}
	if err := e.store.LoadEntryGuards(e.state, e.cfg.Strategies, known); err != nil {
		e.t.Fatalf("LoadEntryGuards: %v", err)
	}
	runtimes, err := e.store.LoadBaskets(e.state, e.cfg)
	if err != nil {
		e.t.Fatalf("LoadBaskets: %v", err)
	}
	e.bc = newBasketCoordinator(e.cfg, e.state, e.store, e.mu, nil, nil, runtimes)
	e.bc.parent = context.Background()
	if rank != nil {
		e.bc.rankRun = rank
	}
	if venue != nil {
		e.bc.venueFor = venue
	}
	if err := e.bc.startupCheck(); err != nil {
		e.t.Fatalf("startupCheck: %v", err)
	}
}

func (e *basketTestEnv) restartFromDisk(rank basketRankRunner, venue func(StrategyConfig) (basketVenue, error)) {
	e.t.Helper()
	e.store.Close()
	e.store = openSourceStore(e.t, e.cfg)
	loaded, _, err := LoadStateWithStore(e.cfg, e.store)
	if err != nil {
		e.t.Fatalf("LoadStateWithStore: %v", err)
	}
	for _, sc := range e.cfg.Strategies {
		if loaded.Strategies[sc.ID] == nil {
			loaded.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	e.state = loaded
	e.reopen(rank, venue, true)
	e.bc.recoverInterrupted(e.now)
}

func (e *basketTestEnv) rt() *basketRuntime {
	return e.bc.runtimes["xs-test"]
}

func basketTestObservation(deadline time.Time, atr float64) *HyperliquidResult {
	cutoff := deadline.Add(30 * time.Second)
	return &HyperliquidResult{
		StrategyDecisionFields: StrategyDecisionFields{ClosedBar: &ClosedBarDecision{
			Enabled:            true,
			CutoffMs:           cutoff.UnixMilli(),
			DecisionBoundaryMs: deadline.UnixMilli(),
			BarOpenMs:          deadline.Add(-time.Hour).UnixMilli(),
			IntervalMs:         3_600_000,
		}},
		Indicators:   map[string]interface{}{"atr": atr},
		BasketMember: true,
	}
}

func (e *basketTestEnv) cycle(now, candleClose time.Time, snap *marketSnapshot, observe bool) {
	e.t.Helper()
	e.now = now
	e.bc.beginCycle()
	if observe {
		for _, m := range basketTestMembers {
			sc := e.bc.members[m.ID]
			res := basketTestObservation(candleClose, m.Mark*0.01)
			e.mu.Lock()
			if s := e.state.Strategies[m.ID]; s != nil {
				admitClosedBarEntry(sc, s, nil, m.Coin, res.StrategyDecisionFields, 0, 0, 0, "", now)
			}
			e.mu.Unlock()
			e.bc.observe(sc, res, m.Mark)
		}
	}
	e.bc.step(basketCycleInputs{
		Now:         now,
		Snapshot:    snap,
		SnapshotFor: func(*basketRuntime, time.Time) *marketSnapshot { return snap },
		Once:        true,
	})
}

func basketTestRanker(weights map[string]float64, hold string) basketRankRunner {
	return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		var req basketRankRequest
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, nil, err
		}
		res := basketRankResult{
			V: basketRankRequestVersion, BasketID: req.BasketID, DeadlineMs: req.DeadlineMs,
			SnapshotID: req.Expected.SnapshotID, Generation: req.Expected.Generation, CutoffMs: req.Expected.CutoffMs,
			Eligibility: map[string]basketRankEligibility{},
			Scores:      map[string]float64{}, Weights: map[string]float64{}, Exclusions: map[string]string{},
			InputFingerprint: "fp-test",
		}
		for _, m := range req.Members {
			px, _ := basketDeadlineClose(req.Market, basketContractMember{Coin: m.Symbol, Timeframe: m.Timeframe, IntervalMs: 3_600_000}, req.DeadlineMs)
			res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "eligible", ClosedBars: req.AllocationContext.LookbackBars, Mark: px}
			res.Weights[m.Symbol] = weights[m.Symbol]
			res.Scores[m.Symbol] = weights[m.Symbol]
		}
		res.Status = basketRankStatusTargets
		if hold != "" {
			res.Status, res.HoldReason = basketRankStatusHold, hold
			res.Weights, res.Scores = nil, nil
		}
		out, err := json.Marshal(res)
		return out, nil, err
	}
}

var basketTestWeights = map[string]float64{"BTC": 0.25, "ETH": 0.25, "SOL": -0.25, "AVAX": -0.25}

func basketTestDeadline() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

func (e *basketTestEnv) signedQty(id, coin string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return basketPositionSigned(e.state.Strategies[id], coin)
}

func (e *basketTestEnv) dbCount(query string, args ...interface{}) int {
	e.t.Helper()
	var n int
	if err := e.store.primary().db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

type scriptedBasketVenue struct {
	mu        sync.Mutex
	live      bool
	submits   []basketAction
	outcomes  []basketVenueOutcome
	queries   map[string][]basketVenueOutcome
	submitErr []error
	onSubmit  func(a *basketAction)
	cloids    map[string]bool
	seq       int
}

func newScriptedBasketVenue(live bool) *scriptedBasketVenue {
	return &scriptedBasketVenue{live: live, queries: map[string][]basketVenueOutcome{}, cloids: map[string]bool{}}
}

func (v *scriptedBasketVenue) Name() string     { return "scripted" }
func (v *scriptedBasketVenue) LiveShaped() bool { return v.live }
func (v *scriptedBasketVenue) Provider() string {
	if v.live {
		return entryProviderHyperliquid
	}
	return "paper"
}
func (v *scriptedBasketVenue) AccountKey(p RiskPartition) string {
	if v.live {
		return "hyperliquid:mainnet:0x" + strings.Repeat("a", 40)
	}
	return "paper:" + p.String()
}
func (v *scriptedBasketVenue) NewClientOrderID() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seq++
	return fmt.Sprintf("0x%032x", v.seq), nil
}
func (v *scriptedBasketVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cloids[a.ClientOrderID] {
		return basketVenueOutcome{}, fmt.Errorf("duplicate client order id %s", a.ClientOrderID)
	}
	v.cloids[a.ClientOrderID] = true
	v.submits = append(v.submits, *a)
	if v.onSubmit != nil {
		v.onSubmit(a)
	}
	if len(v.submitErr) > 0 {
		err := v.submitErr[0]
		v.submitErr = v.submitErr[1:]
		if err != nil {
			return basketVenueOutcome{}, err
		}
	}
	if len(v.outcomes) > 0 {
		out := v.outcomes[0]
		v.outcomes = v.outcomes[1:]
		if out.Outcome == basketVenueFilled && out.FilledQty == 0 {
			out.FilledQty = a.RequestedQty
		}
		if out.Outcome == basketVenuePartial && out.FilledQty < 0 {
			out.FilledQty = hlFloorLotSize(a.RequestedQty*-out.FilledQty, basketTestLots[a.Symbol])
		}
		return out, nil
	}
	return basketVenueOutcome{Outcome: basketVenueFilled, FilledQty: a.RequestedQty}, nil
}
func (v *scriptedBasketVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	q := v.queries[a.ClientOrderID]
	if len(q) == 0 {
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "still unknown"}, nil
	}
	out := q[0]
	v.queries[a.ClientOrderID] = q[1:]
	return out, nil
}

func (v *scriptedBasketVenue) factory() func(StrategyConfig) (basketVenue, error) {
	return func(sc StrategyConfig) (basketVenue, error) {
		if isLiveArgs(sc.Args) {
			return nil, fmt.Errorf("live refused")
		}
		return v, nil
	}
}

func basketReachableWithin(t *testing.T, label string, before basketExposure, a basketAction, b basketBounds) {
	t.Helper()
	after := before.clone()
	if a.OrderSide == "buy" {
		after.Qty[a.Symbol] += a.RequestedQty
	} else {
		after.Qty[a.Symbol] -= a.RequestedQty
	}
	startR, endR := before.reading(), after.reading()
	increase := a.Direction == basketDirectionIncrease
	if ok, why := basketEndpointLegal(startR, endR, b, increase); !ok {
		t.Fatalf("%s: action %s %s %s %.10g leaves the bounds: %s", label, a.StrategyID, a.Direction, a.OrderSide, a.RequestedQty, why)
	}
	if increase {
		for _, r := range []basketBoundReading{startR, endR} {
			if v := r.violations(b); len(v) > 0 {
				t.Fatalf("%s: reachable state violates %v", label, v)
			}
		}
	}
}

func basketListingTestRanker(weights map[string]float64) basketRankRunner {
	return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		var req basketRankRequest
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, nil, err
		}
		res := basketRankResult{
			V: basketRankRequestVersion, BasketID: req.BasketID, DeadlineMs: req.DeadlineMs,
			SnapshotID: req.Expected.SnapshotID, Generation: req.Expected.Generation, CutoffMs: req.Expected.CutoffMs,
			Status:      basketRankStatusTargets,
			Eligibility: map[string]basketRankEligibility{},
			Scores:      map[string]float64{}, Weights: map[string]float64{}, Exclusions: map[string]string{},
			InputFingerprint: "fp-listing-test",
		}
		contract := basketContract{Listings: req.Listings, MinHistoryBars: req.AllocationContext.MinHistoryBars}
		for _, m := range req.Members {
			member := basketContractMember{Coin: m.Symbol, Timeframe: m.Timeframe, IntervalMs: 3_600_000}
			if why := basketListingExclusion(contract, member, req.DeadlineMs); why != "" {
				res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "excluded", Reason: why}
				continue
			}
			px, why := basketDeadlineClose(req.Market, member, req.DeadlineMs)
			if why != "" {
				res.Status, res.HoldReason = basketRankStatusHold, why
				res.Weights, res.Scores = nil, nil
				break
			}
			res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "eligible", ClosedBars: req.AllocationContext.LookbackBars, Mark: px}
			res.Weights[m.Symbol] = weights[m.Symbol]
			res.Scores[m.Symbol] = weights[m.Symbol]
		}
		out, err := json.Marshal(res)
		return out, nil, err
	}
}

func basketMarksWithout(coin string) map[string]float64 {
	out := basketTestMarks()
	delete(out, coin)
	return out
}
