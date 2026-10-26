//go:build pyintegration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

const replayFixtureBaseMs = int64(1756684800000)

type replayFixture struct {
	Candles map[string][][]float64 `json:"candles"`
	Funding map[string][][]float64 `json:"funding"`
}

func loadReplayFixture(t *testing.T) replayFixture {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "basket_replay_recorded_frames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx replayFixture
	if err := json.Unmarshal(blob, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

type replayScenario struct {
	RunID         string
	FirstBar      int
	Steps         int
	Weights       map[string]float64
	Rebalance     int
	Net           float64
	MemberConfig  map[string]interface{}
	PortfolioRisk *PortfolioRiskConfig
	CostPolicy    *basketReplayCostPolicy
	TargetInput   basketReplayTargetInput
	Lots          map[string]int
	Extra         map[int]basketReplayStep
	DropMarks     map[int][]string
	DupSteps      []int
	Params        map[string]interface{}
	Horizon       int64
	Ranker        string
	CoinCap       float64
	SkipSteps     []int
}

func defaultReplayMemberConfig() map[string]interface{} {
	return map[string]interface{}{
		"capital": 2000, "leverage": 1, "max_drawdown_pct": 90, "stop_loss_pct": 3,
		"close_strategy": map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 2.0, "close_fraction": 1.0}}}},
	}
}

func (sc replayScenario) spec(t *testing.T, fx replayFixture, stateDir, mode string) *basketReplaySpec {
	t.Helper()
	const hour = int64(3_600_000)
	first := replayFixtureBaseMs + int64(sc.FirstBar)*hour
	opens := map[string]map[int64]float64{}
	for key, rows := range fx.Candles {
		coin := key[:len(key)-3]
		opens[coin] = map[int64]float64{}
		for _, r := range rows {
			opens[coin][int64(r[0])] = r[1]
		}
	}
	coins := []string{"BTC", "ETH", "SOL"}
	var steps []basketReplayStep
	for i := 0; i < sc.Steps; i++ {
		at := first + int64(i)*hour
		st := basketReplayStep{At: time.UnixMilli(at).UTC().Format(time.RFC3339), Marks: map[string]float64{}}
		for _, c := range coins {
			if px, ok := opens[c][at]; ok {
				st.Marks[c] = px
			}
		}
		for _, c := range sc.DropMarks[i] {
			delete(st.Marks, c)
		}
		if ex, ok := sc.Extra[i]; ok {
			st.Venue, st.Queries, st.SaveFailure = ex.Venue, ex.Queries, ex.SaveFailure
		}
		steps = append(steps, st)
	}
	if len(sc.SkipSteps) > 0 {
		skip := map[int]bool{}
		for _, i := range sc.SkipSteps {
			skip[i] = true
		}
		kept := steps[:0:0]
		for i, st := range steps {
			if !skip[i] {
				kept = append(kept, st)
			}
		}
		steps = kept
	}
	for k := len(sc.DupSteps) - 1; k >= 0; k-- {
		i := sc.DupSteps[k]
		dup := steps[i]
		dup.Venue, dup.Queries, dup.SaveFailure = nil, nil, false
		steps = append(steps[:i+1], append([]basketReplayStep{dup}, steps[i+1:]...)...)
	}
	mcfg := sc.MemberConfig
	if mcfg == nil {
		mcfg = defaultReplayMemberConfig()
	}
	var members []basketReplayMemberSpec
	var ids []string
	listings := map[string]BasketListingFact{}
	for _, c := range coins {
		cfg := map[string]interface{}{}
		blob, _ := json.Marshal(mcfg)
		_ = json.Unmarshal(blob, &cfg)
		id := "rp-" + string(bytes.ToLower([]byte(c)))
		members = append(members, basketReplayMemberSpec{ID: id, Coin: c, Timeframe: "1h", Config: cfg})
		ids = append(ids, id)
		listings[c] = BasketListingFact{ListedAt: "2020-01-01T00:00:00Z", KnownAt: "2020-01-01T00:00:00Z"}
	}
	weights := sc.Weights
	if weights == nil {
		weights = map[string]float64{"BTC": 0.35, "ETH": 0.25, "SOL": -0.4}
	}
	params := sc.Params
	if params == nil {
		w := map[string]interface{}{}
		for k, v := range weights {
			w[k] = v
		}
		params = map[string]interface{}{"weights": w}
	}
	rebalance := sc.Rebalance
	if rebalance == 0 {
		rebalance = 14400
	}
	net := sc.Net
	if net == 0 {
		net = 900
	}
	lots := sc.Lots
	if lots == nil {
		lots = map[string]int{"BTC": 5, "ETH": 4, "SOL": 2}
	}
	target := sc.TargetInput
	if target.Mode == "" {
		target.Mode = basketReplayTargetsRanker
	}
	funding := map[string]basketReplayFunding{}
	for coin, recs := range fx.Funding {
		funding[coin] = basketReplayFunding{FromMs: replayFixtureBaseMs, ToMs: int64(recs[len(recs)-1][0]), Records: recs}
	}
	runID := sc.RunID
	if runID == "" {
		runID = "replay-test"
	}
	ranker := sc.Ranker
	if ranker == "" {
		ranker = "basket_scripted_fixture"
	}
	coinCap := sc.CoinCap
	if coinCap == 0 {
		coinCap = 1200
	}
	return &basketReplaySpec{
		Protocol: basketReplayProtocolName, Version: basketReplayProtocolVersion, Mode: mode,
		Run: basketReplayRunSpec{RunID: runID, StateDir: stateDir, Source: map[string]string{"frames": "scheduler/testdata/basket_replay_recorded_frames.json"}},
		Basket: BasketConfig{
			ID: "xs-replay", Ranker: ranker, Params: params, Members: ids,
			RebalanceIntervalSeconds: rebalance, EvaluationWindowSeconds: 3600, RankerTimeoutSeconds: 60, LookbackBars: 48,
			GrossBudgetUSD: 3000, MaxTransitionNetUSD: net, MaxCoinNotionalUSD: coinCap, MinEligibleMembers: 2, HoldingHorizonSeconds: sc.Horizon,
			Eligibility: &BasketEligibility{Source: "fixture listing ledger", Version: "2026-10-14", MinHistoryBars: 24, Listings: listings},
		},
		Members: members, PortfolioRisk: sc.PortfolioRisk, TargetInput: target, CostPolicy: sc.CostPolicy,
		LotMetadata: lots, Candles: fx.Candles, Funding: funding, Steps: steps,
	}
}

func writeReplaySpec(t *testing.T, spec *basketReplaySpec) string {
	t.Helper()
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp("", "basket-replay-spec-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(blob); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

var (
	pyintegrationBinOnce sync.Once
	pyintegrationBinPath string
	pyintegrationBinErr  error
)

func pyintegrationBinary(t *testing.T) string {
	t.Helper()
	pyintegrationBinOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			pyintegrationBinErr = fmt.Errorf("runtime.Caller failed")
			return
		}
		dir, err := os.MkdirTemp("", "pyintegration-bin-")
		if err != nil {
			pyintegrationBinErr = err
			return
		}
		pyintegrationBinPath = filepath.Join(dir, "go-trader")
		cmd := exec.Command("go", "build", "-tags", "pyintegration", "-o", pyintegrationBinPath, ".")
		cmd.Dir = filepath.Dir(thisFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			pyintegrationBinErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if pyintegrationBinErr != nil {
		t.Fatal(pyintegrationBinErr)
	}
	return pyintegrationBinPath
}

type replayProcess struct {
	Out    basketReplayOutput
	Raw    []byte
	Code   int
	Stderr string
}

func runReplayAdapter(t *testing.T, root string, spec *basketReplaySpec, args ...string) replayProcess {
	t.Helper()
	bin := pyintegrationBinary(t)
	path := writeReplaySpec(t, spec)
	cmd := exec.Command(bin, append([]string{"basket-replay", "--spec", path}, args...)...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	p := replayProcess{Raw: stdout.Bytes(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		p.Code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("adapter did not run: %v", err)
	}
	if p.Code != basketReplayExitFault {
		dec := json.NewDecoder(bytes.NewReader(p.Raw))
		if err := dec.Decode(&p.Out); err != nil {
			t.Fatalf("adapter stdout is not one protocol JSON document (exit %d): %v\nstdout=%s\nstderr tail=%s", p.Code, err, p.Raw, tail(p.Stderr, 4000))
		}
		if dec.More() {
			t.Fatalf("adapter stdout carries more than one JSON document")
		}
	}
	return p
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

type replayReference struct {
	t       *testing.T
	spec    *basketReplaySpec
	cfg     *Config
	store   *StateStore
	state   *AppState
	mu      *sync.RWMutex
	bc      *basketCoordinator
	venue   *scriptedBasketVenue
	clock   *simulatedClock
	feedReq feedRequirements
	need    map[marketFeedKey]int
	members []StrategyConfig
	records []basketReplayStepRecord
	lots    *hlLotMetadataCache
	marks   map[string]float64
	bounds  map[string]basketReplayBoundRecord
	calls   []replayVenueCall
}

type replayVenueCall struct {
	Kind    string
	AtMs    int64
	Action  basketAction
	Outcome basketVenueOutcome
	Err     error
}

type refBoundsVenue struct {
	*scriptedBasketVenue
	ref *replayReference
}

func (v refBoundsVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	if b := basketReplayActionBounds(v.ref.mu, v.ref.bc, v.ref.state, v.ref.marks, a); b != nil {
		v.ref.bounds[a.ActionID] = *b
	}
	out, err := v.scriptedBasketVenue.Submit(a)
	v.ref.calls = append(v.ref.calls, replayVenueCall{Kind: "submit", AtMs: tradingNow().UnixMilli(), Action: *a, Outcome: out, Err: err})
	return out, err
}

func (v refBoundsVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	out, err := v.scriptedBasketVenue.Query(a)
	v.ref.calls = append(v.ref.calls, replayVenueCall{Kind: "query", AtMs: tradingNow().UnixMilli(), Action: *a, Outcome: out, Err: err})
	return out, err
}

func newReplayReference(t *testing.T, spec *basketReplaySpec, cost fillCostPolicy) *replayReference {
	t.Helper()
	if err := validateBasketReplaySpec(spec); err != nil {
		t.Fatalf("reference spec: %v", err)
	}
	dir := t.TempDir()
	cfgJSON, err := spec.configDocument(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigData("", cfgJSON, true, true)
	if err != nil {
		t.Fatalf("reference config: %v", err)
	}
	ref := &replayReference{t: t, spec: spec, cfg: cfg, mu: &sync.RWMutex{}, clock: &simulatedClock{}, venue: newScriptedBasketVenue(false), bounds: map[string]basketReplayBoundRecord{}}
	if err := ref.clock.Set(spec.parsedSteps[0]); err != nil {
		t.Fatal(err)
	}
	if err := bindTradingClock(basketReplayClockOwner, ref.clock.Now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock(basketReplayClockOwner) })
	if err := bindFillCostPolicy("replay-reference", cost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindFillCostPolicy("replay-reference") })
	ref.lots = newFrozenHLLotMetadataCache(spec.LotMetadata, t.Logf)
	prevLots := hlLotMetadata
	hlLotMetadata = ref.lots
	t.Cleanup(func() { hlLotMetadata = prevLots })
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prevRecorder })
	ref.store = openSourceStore(t, cfg)
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	ReconcileConfigInitialCapital(cfg, state, ref.store)
	syncPaperFundingEligibility(state, cfg.Strategies, ref.clock.Now())
	ref.state = state
	ref.members = append([]StrategyConfig(nil), cfg.Strategies...)
	sort.Slice(ref.members, func(i, j int) bool { return ref.members[i].ID < ref.members[j].ID })
	ref.feedReq, err = deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ref.need = map[marketFeedKey]int{}
	for _, e := range ref.feedReq.Strategies {
		if e.SignalLookback > ref.need[e.Signal] {
			ref.need[e.Signal] = e.SignalLookback
		}
	}
	for _, sc := range cfg.Strategies {
		coin, tf := strategyArgSymbolTimeframe(sc.Args)
		if l := basketFrameLookback(cfg.Baskets[0].LookbackBars); l > ref.need[feedKeyFor(coin, tf)] {
			ref.need[feedKeyFor(coin, tf)] = l
		}
	}
	ref.open(false)
	return ref
}

func (ref *replayReference) open(persisted bool) {
	t := ref.t
	known := map[string]bool{}
	if persisted {
		for id := range ref.state.Strategies {
			known[id] = true
		}
	}
	if err := ref.store.LoadEntryGuards(ref.state, ref.cfg.Strategies, known); err != nil {
		t.Fatal(err)
	}
	runtimes, err := ref.store.LoadBaskets(ref.state, ref.cfg)
	if err != nil {
		t.Fatal(err)
	}
	bc := newBasketCoordinator(ref.cfg, ref.state, ref.store, ref.mu, nil, nil, runtimes)
	bc.parent = context.Background()
	bc.rankClock = basketRankClockSimulated
	bc.lots = ref.lots
	bc.venueFor = func(sc StrategyConfig) (basketVenue, error) {
		if isLiveArgs(sc.Args) {
			return nil, errors.New("live refused")
		}
		return refBoundsVenue{scriptedBasketVenue: ref.venue, ref: ref}, nil
	}
	if ref.spec.TargetInput.Mode == basketReplayTargetsBaseline {
		bc.rankRun = basketBaselineRankRunner(ref.spec.baseline)
		bc.targetPolicy = basketTargetsOfflineBaseline
	} else {
		bc.rankRun = runBasketRankerProcess
	}
	if err := bc.startupCheck(); err != nil {
		t.Fatal(err)
	}
	bc.recoverInterrupted(ref.clock.Now())
	ref.bc = bc
}

func (ref *replayReference) restart() {
	t := ref.t
	ref.store.Close()
	ref.store = openSourceStore(t, ref.cfg)
	loaded, _, err := LoadStateWithStore(ref.cfg, ref.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range ref.cfg.Strategies {
		if loaded.Strategies[sc.ID] == nil {
			loaded.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	ref.state = loaded
	ref.open(true)
}

func (ref *replayReference) snapshot(at, cutoff, deadline time.Time, marks map[string]float64, id string) *marketSnapshot {
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: id, ConfigGeneration: 1, SealedAt: at, Deadline: deadline,
		keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{}, observations: map[feedObservationKey]*marketSnapshotObservation{}}
	for key, bars := range ref.spec.parsedCandles {
		ivl, _ := hlCandleIntervalMs(key.Timeframe)
		var keep []feedBar
		for _, b := range bars {
			switch {
			case b.OpenMs+ivl <= cutoff.UnixMilli():
				keep = append(keep, b)
			case b.OpenMs <= cutoff.UnixMilli() && b.OpenMs <= at.UnixMilli():
				keep = append(keep, feedBar{OpenMs: b.OpenMs, CloseMs: b.CloseMs, Open: b.Open, High: b.Open, Low: b.Open, Close: b.Open, Source: b.Source, RecvAt: at})
			}
		}
		need := ref.need[key]
		ready := len(keep) >= need && need > 0
		entry := &marketSnapshotKey{Key: key, IntervalMs: ivl, Bars: keep, Readiness: feedKeyReadiness{Key: key, Bars: len(keep), Required: need, Ready: ready, Status: feedStatusReady, Source: "replay", LastRecvAt: at}}
		snap.keys[key] = entry
	}
	for coin, px := range marks {
		snap.mids[coin] = feedMid{Px: px, RecvAt: at, Source: "replay"}
	}
	return snap
}

func (ref *replayReference) fundingInputs(need map[string]int64, at time.Time) paperFundingInputs {
	in := paperFundingInputs{Source: "replay", Coverage: map[string]feedFundingCoverage{}, Detail: map[string]string{}, NotServed: map[string]bool{}}
	for coin := range need {
		cov, ok := ref.spec.funding[coin]
		if !ok || cov.FromMs > at.UnixMilli() {
			continue
		}
		out := feedFundingCoverage{Coin: coin, FromMs: cov.FromMs}
		for _, rec := range cov.Records {
			if rec.TimeMs <= at.UnixMilli() {
				out.Records = append(out.Records, rec)
				out.ToMs = rec.TimeMs
			}
		}
		in.Coverage[coin] = out
	}
	return in
}

func (ref *replayReference) scriptVenue(step basketReplayStep) {
	v := ref.venue
	v.mu.Lock()
	defer v.mu.Unlock()
	v.outcomes, v.submitErr = nil, nil
	for _, o := range step.Venue {
		switch o.Outcome {
		case basketReplayOutcomeTransport:
			v.submitErr = append(v.submitErr, errors.New(scriptedDetail(o, "scripted transport failure")))
		case basketReplayOutcomePartial:
			v.submitErr = append(v.submitErr, nil)
			v.outcomes = append(v.outcomes, basketVenueOutcome{Outcome: basketVenuePartial, FilledQty: -o.FillFraction})
		case basketReplayOutcomeRejected:
			v.submitErr = append(v.submitErr, nil)
			v.outcomes = append(v.outcomes, basketVenueOutcome{Outcome: basketVenueRejected, Detail: scriptedDetail(o, "the scripted venue rejected the order")})
		case basketReplayOutcomeUnknown:
			v.submitErr = append(v.submitErr, nil)
			v.outcomes = append(v.outcomes, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: scriptedDetail(o, "the scripted venue did not report a status")})
		default:
			v.submitErr = append(v.submitErr, nil)
			v.outcomes = append(v.outcomes, basketVenueOutcome{Outcome: basketVenueFilled})
		}
	}
	for _, rt := range ref.bc.runtimes {
		a := rt.pendingAction()
		if a == nil || len(step.Queries) == 0 {
			continue
		}
		var q []basketVenueOutcome
		for _, o := range step.Queries {
			switch o.Outcome {
			case basketReplayOutcomeFilled:
				q = append(q, basketVenueOutcome{Outcome: basketVenueFilled, FilledQty: a.RequestedQty})
			case basketReplayOutcomePartial:
				q = append(q, basketVenueOutcome{Outcome: basketVenuePartial, FilledQty: hlFloorLotSize(a.RequestedQty*o.FillFraction, basketTestLots[a.Symbol])})
			case basketReplayOutcomeRejected, basketReplayOutcomeTransport:
				q = append(q, basketVenueOutcome{Outcome: basketVenueRejected, Detail: scriptedDetail(o, "the scripted venue reports the order rejected")})
			default:
				q = append(q, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: scriptedDetail(o, "the scripted venue still reports no status")})
			}
		}
		v.queries[a.ClientOrderID] = q
	}
}

func (ref *replayReference) step(i int) {
	t := ref.t
	at := ref.spec.parsedSteps[i]
	step := ref.spec.Steps[i]
	if err := ref.clock.Set(at); err != nil {
		t.Fatal(err)
	}
	if step.SaveFailure {
		prev := storeCommitHook
		storeCommitHook = func(storageRole) error { return errors.New("scripted state save failure") }
		defer func() { storeCommitHook = prev }()
	}
	cfg, state, store, mu := ref.cfg, ref.state, ref.store, ref.mu
	snap := ref.snapshot(at, at, time.Time{}, step.Marks, fmt.Sprintf("ref/%d", i))
	feedCtx := &marketFeedContext{Enabled: true, Requirements: ref.feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
	prices := snap.freshMids()
	ref.scriptVenue(step)
	ref.marks = step.Marks
	ref.bc.beginCycle()
	rec := basketReplayStepRecord{Index: i, At: at.Format(time.RFC3339Nano)}
	mu.RLock()
	need := planPaperFunding(state, cfg.Strategies)
	mu.RUnlock()
	mu.Lock()
	fres := runPaperFundingAccounting(state, cfg.Strategies, store, ref.fundingInputs(need, at), prices, func(p RiskPartition) bool { return partitionSaveBlocked(store, p) }, at)
	mu.Unlock()
	rec.FundingBooked = fres.Booked
	parts := activePartitions(cfg.Strategies)
	scopeRisk := map[RiskPartition]*scopeCycleRisk{}
	for _, p := range parts {
		scopeRisk[p] = &scopeCycleRisk{Partition: p, Config: partitionRiskConfig(cfg, p)}
	}
	snapshotFor := func(rt *basketRuntime, d time.Time) *marketSnapshot {
		return ref.snapshot(at, d, d, step.Marks, fmt.Sprintf("ref/%d/%d", i, d.UnixMilli()))
	}
	if allPartitionsSaveBlocked(store, cfg) {
		rec.SaveBlocked = true
		ref.bc.step(basketCycleInputs{Now: at, Snapshot: snap, SnapshotFor: snapshotFor, SaveBlocked: true, Once: true})
	} else {
		mu.RLock()
		for _, p := range parts {
			scopeRisk[p] = measureScopeCycleRisk(p, scopeRisk[p].Config, cfg.Strategies, state, prices, nil, nil, true, false, at)
		}
		mu.RUnlock()
		mu.Lock()
		for _, p := range parts {
			applyScopeCycleRisk(scopeRisk[p], state.partitionRisk(p))
			if !p.IsLive() && scopeRisk[p].KillSwitchFired {
				applyPaperKillSwitchCycle(state, cfg, prices, scopeRisk[p], true)
			}
		}
		mu.Unlock()
		for _, sc := range dueStrategiesPersistable(store, dueStrategiesNotLatched(ref.members, scopeRisk)) {
			s := state.Strategies[sc.ID]
			stratDB, err := store.dbForStrategy(sc.ID)
			if err != nil {
				t.Fatal(err)
			}
			sr := scopeRisk[partitionFor(sc)]
			logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
			mu.RLock()
			pv := PortfolioValue(s, prices)
			pos, hasPos := readHLPerpsPosition(sc, s, hyperliquidSymbol(sc.Args), cfg.Regime)
			cash := s.Cash
			mu.RUnlock()
			mu.Lock()
			allowed, reason := CheckRisk(&sc, s, pv, prices, logger, nil)
			mu.Unlock()
			cbManageOnly := false
			if !allowed {
				if !circuitBreakerPermitsManagement(reason, sc.Platform, sc.Type, pos.Ctx.Quantity) {
					continue
				}
				cbManageOnly = true
			}
			if notionalCapSkipsStrategyCycle(sr.NotionalBlocked) {
				continue
			}
			x := &hlPerpsStep{sc: sc, mu: mu, logger: logger, stratState: s, notifier: NewMultiNotifier(), cfg: cfg, stratDB: stratDB, store: store, state: state,
				sr: sr, prices: prices, feedCtx: feedCtx, basketCoord: ref.bc, intervals: effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(cfg)),
				stratScope: partitionFor(sc).Scope, persistenceHold: store.persistenceHoldsPartition(partitionFor(sc)), cbManageOnly: cbManageOnly, hlScaleInCash: cash}
			if hasPos {
				x.hlPosCtx, x.hlPosSide, x.hlPosQty, x.hlAvgCost = pos.Ctx, pos.Ctx.Side, pos.Ctx.Quantity, pos.Ctx.AvgCost
				x.hlPosLeverage, x.hlEntryATR, x.hlStopLossOID, x.hlTPOIDs = pos.Leverage, pos.EntryATR, pos.StopLossOID, pos.TPOIDs
				x.hlStopLossTriggerPx, x.hlStopLossHighWaterPx, x.hlPosSnapshot = pos.StopLossTriggerPx, pos.StopLossHighWaterPx, pos.Snapshot
				x.hlScaleInCount, x.hlLastAddPrice, x.hlAddedNotionalUSD = pos.ScaleInCount, pos.LastAddPrice, pos.AddedNotionalUSD
				x.hlScaleInResizePending, x.hlSharedCloseHoldUSD, x.hlSharedCloseHoldReason = pos.ScaleInResizePending, pos.SharedCloseHoldUSD, pos.SharedCloseHoldReason
			}
			runHyperliquidPerpsStep(x)
		}
		ref.bc.step(basketCycleInputs{Now: at, Snapshot: snap, SnapshotFor: snapshotFor, ScopeRisk: scopeRisk, Once: true})
	}
	mu.Lock()
	state.CycleCount++
	state.LastCycle = at
	outcomes := store.SaveAll(state)
	mu.Unlock()
	for _, out := range sortedPartitionErrors(outcomes) {
		if out.err != nil {
			rec.SaveErrors = append(rec.SaveErrors, fmt.Sprintf("%s: %v", partitionLabel(out.part), out.err))
		}
	}
	db, err := store.dbForStrategy(ref.members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	buildBasketReplayStepRecord(&rec, at, basketReplayRecordInput{DB: db, BC: ref.bc, State: state, Mu: mu, Bounds: ref.bounds, Partition: partitionFor(ref.members[0]), ScopeRisk: scopeRisk, Prices: prices, Marks: step.Marks})
	ref.records = append(ref.records, rec)
}

func (ref *replayReference) trades() []basketReplayTradeRecord {
	var out []basketReplayTradeRecord
	for _, sc := range ref.members {
		rows, _, err := ref.store.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 1_000_000, 0)
		if err != nil {
			ref.t.Fatal(err)
		}
		for i := len(rows) - 1; i >= 0; i-- {
			tr := rows[i]
			out = append(out, basketReplayTradeRecord{StrategyID: sc.ID, Timestamp: tr.Timestamp.UTC().Format(time.RFC3339Nano), Symbol: tr.Symbol, PositionID: tr.PositionID, Side: tr.Side,
				Quantity: tr.Quantity, Price: tr.Price, Value: tr.Value, TradeType: tr.TradeType, ExchangeOrderID: tr.ExchangeOrderID, ExchangeFee: tr.ExchangeFee,
				IsClose: tr.IsClose, RealizedPnL: tr.RealizedPnL, CostModelVersion: tr.CostModelVersion, CostPolicyID: tr.CostPolicyID})
		}
	}
	return out
}

type replayActionView struct {
	ActionID, StrategyID, Direction, OrderSide, Status, HoldReason, Purpose string
	Seq, Attempt                                                            int
	ExpiryMs                                                                int64
	Requested, Confirmed, Applied, Fee, Mark                                float64
	Bounds                                                                  *basketReplayBoundRecord
}

type replayMemberView struct {
	ID, DailyDate, CBUntil                       string
	Cash, Qty, AvgCost, StopPx, Expected, Daily  float64
	CL                                           int
	CB                                           bool
	Consumed, Observed, FundingThrough, OpenedAt int64
	LifeEntry, LifeExpiry, Suppress              int64
	LifeState                                    string
	LifeAttempt, LifeFailures                    int
	LifeRetry                                    int64
	LifeAlerts                                   []string
}

type replayStepView struct {
	At            string
	SaveBlocked   bool
	SaveErrors    int
	Funding       int
	Rebalances    []basketReplayRebalanceRecord
	Actions       []replayActionView
	Members       []replayMemberView
	KillSwitch    bool
	KillSwitchAt  string
	DailyLossHeld bool
	PendingAction string
	NeedsDurable  bool
}

func replayView(rec basketReplayStepRecord) replayStepView {
	v := replayStepView{At: rec.At, SaveBlocked: rec.SaveBlocked, SaveErrors: len(rec.SaveErrors), Funding: rec.FundingBooked, KillSwitch: rec.Partition.KillSwitchActive,
		KillSwitchAt: rec.Partition.KillSwitchAt, DailyLossHeld: rec.Partition.DailyLossHeld, PendingAction: rec.PendingAction, NeedsDurable: rec.NeedsDurable}
	for _, rb := range rec.Rebalances {
		rb.InputFP = ""
		v.Rebalances = append(v.Rebalances, rb)
	}
	for _, a := range rec.Actions {
		id := a.ActionID
		if a.Purpose == basketPurposeExpiry {
			id = ""
		}
		v.Actions = append(v.Actions, replayActionView{ActionID: id, StrategyID: a.StrategyID, Direction: a.Direction, OrderSide: a.OrderSide, Status: a.Status,
			HoldReason: a.HoldReason, Purpose: a.Purpose, Seq: a.Seq, Attempt: a.Attempt, ExpiryMs: a.ExpiryMs, Requested: a.RequestedQty, Confirmed: a.ConfirmedQty,
			Applied: a.AppliedQty, Fee: a.AppliedFee, Mark: a.MarkPx, Bounds: a.Bounds})
	}
	sort.SliceStable(v.Actions, func(i, j int) bool {
		x, y := v.Actions[i], v.Actions[j]
		if x.Purpose != y.Purpose {
			return x.Purpose < y.Purpose
		}
		if x.StrategyID != y.StrategyID {
			return x.StrategyID < y.StrategyID
		}
		if x.ActionID != y.ActionID {
			return x.ActionID < y.ActionID
		}
		if x.Attempt != y.Attempt {
			return x.Attempt < y.Attempt
		}
		return x.Seq < y.Seq
	})
	for _, m := range rec.Members {
		v.Members = append(v.Members, replayMemberView{ID: m.StrategyID, DailyDate: m.DailyPnLDate, CBUntil: m.CircuitBreakerUntil, Cash: m.Cash, Qty: m.Qty, AvgCost: m.AvgCost,
			StopPx: m.StopLossTriggerPx, Expected: m.ExpectedQty, Daily: m.DailyPnL, CL: m.ConsecutiveLosses, CB: m.CircuitBreaker, Consumed: m.ConsumedThroughMs,
			Observed: m.LatestObservedMs, FundingThrough: m.FundingSettledMs, OpenedAt: m.OpenedAtMs, LifeEntry: m.LifecycleEntryMs, LifeExpiry: m.LifecycleExpiryMs,
			Suppress: m.SuppressThroughMs, LifeState: m.LifecycleState, LifeAttempt: m.LifecycleAttempt, LifeFailures: m.LifecycleFailures, LifeRetry: m.LifecycleRetryAtMs,
			LifeAlerts: replayPositionNonceFree(m.LifecycleAlerts)})
	}
	if pa := v.PendingAction; pa != "" {
		v.PendingAction = pa
	}
	return v
}

var replayExpiryActionID = regexp.MustCompile(`be-[0-9a-f]{24}`)

var replayPositionNonce = regexp.MustCompile(`([A-Za-z0-9_-]+:[A-Za-z0-9]+:[0-9]+):[0-9]+`)

func replayPositionNonceFree(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, replayPositionNonce.ReplaceAllString(s, "$1"))
	}
	return out
}

func replayTradeView(trades []basketReplayTradeRecord) []basketReplayTradeRecord {
	out := make([]basketReplayTradeRecord, 0, len(trades))
	for _, tr := range trades {
		tr.PositionID, tr.Details = replayPositionNonce.ReplaceAllString(tr.PositionID, "$1"), ""
		out = append(out, tr)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StrategyID != out[j].StrategyID {
			return out[i].StrategyID < out[j].StrategyID
		}
		return out[i].Timestamp < out[j].Timestamp
	})
	return out
}

func requireReplayParity(t *testing.T, label string, a, b []basketReplayStepRecord, ta, tb []basketReplayTradeRecord) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d steps against %d", label, len(a), len(b))
	}
	ma, mb := &replayIdentityMap{tag: "expiry-action-"}, &replayIdentityMap{tag: "expiry-action-"}
	for i := range a {
		va, vb := replayView(a[i]), replayView(b[i])
		ja, _ := json.MarshalIndent(va, "", " ")
		jb, _ := json.MarshalIndent(vb, "", " ")
		ja = replayExpiryActionID.ReplaceAllFunc(ja, func(id []byte) []byte { return []byte(ma.of(string(id))) })
		jb = replayExpiryActionID.ReplaceAllFunc(jb, func(id []byte) []byte { return []byte(mb.of(string(id))) })
		if !bytes.Equal(ja, jb) {
			t.Fatalf("%s: step %d differs\nfirst:\n%s\nsecond:\n%s", label, i, ja, jb)
		}
	}
	xa, _ := json.MarshalIndent(replayTradeView(ta), "", " ")
	xb, _ := json.MarshalIndent(replayTradeView(tb), "", " ")
	if !bytes.Equal(xa, xb) {
		t.Fatalf("%s: booked trades differ\nfirst:\n%s\nsecond:\n%s", label, xa, xb)
	}
}

type replayCallView struct {
	Kind, At, Member, Coin, Side, Purpose, Direction, Outcome, Action, Client string
	Requested, Confirmed                                                      float64
	Attempt                                                                   int
}

type replayIdentityMap struct {
	ids  map[string]string
	next int
	tag  string
}

func (m *replayIdentityMap) of(id string) string {
	if id == "" {
		return ""
	}
	if m.ids == nil {
		m.ids = map[string]string{}
	}
	if v, ok := m.ids[id]; ok {
		return v
	}
	m.next++
	v := fmt.Sprintf("%s%d", m.tag, m.next)
	m.ids[id] = v
	return v
}

func replayActionsByID(out basketReplayOutput) map[string]basketReplayActionRecord {
	byID := map[string]basketReplayActionRecord{}
	for _, st := range out.Steps {
		for _, a := range st.Actions {
			byID[a.ActionID] = a
		}
	}
	return byID
}

func replayAdapterCalls(t *testing.T, out basketReplayOutput) []replayCallView {
	t.Helper()
	byID := replayActionsByID(out)
	actions, clients := &replayIdentityMap{tag: "a"}, &replayIdentityMap{tag: "c"}
	var views []replayCallView
	for _, ev := range out.VenueEvents {
		a, ok := byID[ev.ActionID]
		if !ok {
			t.Fatalf("venue event %d names action %s, which no step record carries", ev.EventID, ev.ActionID)
		}
		outcome := ev.Outcome.Outcome
		if ev.Kind == "submit" && ev.Outcome.Detail == "the request never reached the scripted venue" {
			outcome = "transport_error"
		}
		views = append(views, replayCallView{Kind: ev.Kind, At: ev.At, Member: a.StrategyID, Coin: a.Symbol, Side: a.OrderSide, Purpose: a.Purpose, Direction: a.Direction,
			Outcome: outcome, Action: actions.of(ev.ActionID), Client: clients.of(ev.ClientOrderID), Requested: a.RequestedQty, Confirmed: ev.Outcome.FilledQty, Attempt: a.Attempt})
	}
	return views
}

func replayReferenceCalls(calls []replayVenueCall) []replayCallView {
	actions, clients := &replayIdentityMap{tag: "a"}, &replayIdentityMap{tag: "c"}
	var views []replayCallView
	for _, c := range calls {
		outcome := c.Outcome.Outcome
		if c.Err != nil {
			outcome = "transport_error"
		}
		a := c.Action
		views = append(views, replayCallView{Kind: c.Kind, At: time.UnixMilli(c.AtMs).UTC().Format(time.RFC3339Nano), Member: a.StrategyID, Coin: a.Symbol, Side: a.OrderSide,
			Purpose: a.purpose(), Direction: a.Direction, Outcome: outcome, Action: actions.of(a.ActionID), Client: clients.of(a.ClientOrderID), Requested: a.RequestedQty,
			Confirmed: c.Outcome.FilledQty, Attempt: a.LifecycleAttempt})
	}
	return views
}

func compareReplayCalls(ref, got []replayCallView) error {
	n := len(ref)
	if len(got) > n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		var a, b replayCallView
		if i < len(ref) {
			a = ref[i]
		}
		if i < len(got) {
			b = got[i]
		}
		if a != b {
			return fmt.Errorf("venue call %d differs (%d reference calls, %d adapter calls)\nreference: %+v\nadapter:   %+v", i, len(ref), len(got), a, b)
		}
	}
	return nil
}

func requireReplayCallParity(t *testing.T, label string, ref *replayReference, out basketReplayOutput) []replayCallView {
	t.Helper()
	got := replayAdapterCalls(t, out)
	if len(got) == 0 {
		t.Fatalf("%s: the adapter recorded no venue call", label)
	}
	if err := compareReplayCalls(replayReferenceCalls(ref.calls), got); err != nil {
		t.Fatalf("%s: chronological venue calls differ: %v", label, err)
	}
	return got
}

func requireOneBookingPerFill(t *testing.T, out basketReplayOutput) {
	t.Helper()
	final := map[string]basketReplayActionRecord{}
	for _, st := range out.Steps {
		for _, a := range st.Actions {
			final[a.ActionID] = a
		}
	}
	type bookingKey struct {
		Strategy, At, Side string
		Close              bool
	}
	want := map[bookingKey]float64{}
	for _, a := range final {
		if a.AppliedQty > a.ConfirmedQty*(1+1e-9)+1e-12 {
			t.Fatalf("action %s applied %.10g of %.10g confirmed", a.ActionID, a.AppliedQty, a.ConfirmedQty)
		}
		if a.Status == basketActionCommitted && math.Abs(a.AppliedQty-a.ConfirmedQty) > 1e-12 {
			t.Fatalf("committed action %s applied %.10g but confirmed %.10g", a.ActionID, a.AppliedQty, a.ConfirmedQty)
		}
		if a.Status != basketActionCommitted && a.AppliedQty != 0 {
			t.Fatalf("action %s is %s but applied %.10g", a.ActionID, a.Status, a.AppliedQty)
		}
		if a.Bounds != nil && a.Status == basketActionCommitted && !a.Bounds.Legal {
			t.Fatalf("committed action %s had an illegal endpoint: %s", a.ActionID, a.Bounds.Detail)
		}
		if a.AppliedQty > 0 {
			k := bookingKey{a.StrategyID, time.UnixMilli(a.UpdatedAtMs).UTC().Format(time.RFC3339Nano), a.OrderSide, a.Direction == basketDirectionReduce}
			want[k] += a.AppliedQty
		}
	}
	got := map[bookingKey]float64{}
	for _, tr := range out.Trades {
		if tr.TradeType == TradeTypeFunding {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, tr.Timestamp)
		if err != nil {
			t.Fatal(err)
		}
		k := bookingKey{tr.StrategyID, ts.UTC().Format(time.RFC3339Nano), tr.Side, tr.IsClose}
		if _, ok := want[k]; ok {
			got[k] += tr.Quantity
		}
	}
	for k, q := range want {
		if math.Abs(got[k]-q) > 1e-9 {
			t.Fatalf("%s %s %s close=%v: actions applied %.10g but the books hold %.10g of trades", k.Strategy, k.At, k.Side, k.Close, q, got[k])
		}
	}
}
