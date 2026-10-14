package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	basketReplayClockOwner  = "basket-replay"
	basketReplayCapitalNote = "per-member paper cash: each member books its own positive capital and its own margin gate; the basket shares one gross budget, one net bound and one per-coin cap, and reports one aggregate equity line; there is no pooled cash or margin"
	basketReplayClockNote   = "every trade, position, funding, risk, decision, action, holding-horizon expiry, lifecycle, suppression and basket row time is the simulated step time; the storage audit columns entry intent updated_at and basket_replay_runs.created_at_ms keep the system clock and no trading or funding decision reads them"
	basketReplayEventOrder  = "per step: funding accounting, partition risk measurement and kill switch, per-member circuit breaker check and management (stops, native closes, trailing stops, ratchet, protection) on sealed closed bars, then the basket coordinator (reconciliation, exceptions, ranking, frozen-target execution), then one state save; bar fixtures resolve intrabar stop and take-profit races with ohlc_walk before they become steps"
)

type basketReplayOptions struct {
	MaxSteps int
	Fault    basketReplayFault
}

type basketReplayFault struct {
	ExitAfterSubmit int
}

type basketReplayRuntime struct {
	spec      *basketReplaySpec
	fp        basketReplayFingerprints
	manifest  basketReplayManifest
	opts      basketReplayOptions
	cfg       *Config
	cfgJSON   []byte
	layout    storageLayout
	store     *StateStore
	owner     *stateOwnership
	file      *StateDB
	state     *AppState
	mu        *sync.RWMutex
	bc        *basketCoordinator
	clock     *simulatedClock
	lots      *hlLotMetadataCache
	cost      fillCostPolicy
	feedReq   feedRequirements
	intervals map[string]int
	need      map[marketFeedKey]int
	notifier  *MultiNotifier
	venue     *basketReplayVenue
	progress  basketReplayProgress
	resumedAt int
	records   []basketReplayStepRecord
	stored    []basketReplayStoredStep
	pending   []basketReplayStoredStep
	lotMark   int
	cleanups  []func()
	members   []StrategyConfig
	partition RiskPartition
}

func (r *basketReplayRuntime) Close() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}

func openBasketReplay(spec *basketReplaySpec, raw []byte, opts basketReplayOptions) (*basketReplayRuntime, error) {
	r := &basketReplayRuntime{spec: spec, opts: opts, mu: &sync.RWMutex{}, notifier: NewMultiNotifier(), clock: &simulatedClock{}}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	inputFP, err := basketReplayInputFingerprint(raw)
	if err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	stateDir := spec.Run.StateDir
	if err := checkBasketReplayStateDir(spec.Mode, stateDir); err != nil {
		return nil, err
	}
	cfgJSON, err := spec.configDocument(stateDir)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfigData("", append([]byte(nil), cfgJSON...), true, true)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	if err := basketReplayConfigRefusal(cfg, stateDir); err != nil {
		return nil, err
	}
	contract, err := buildBasketContract(cfg, cfg.Baskets[0])
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	r.cfg, r.cfgJSON = cfg, cfgJSON
	r.fp = basketReplayFingerprints{
		Protocol: basketReplayProtocolFingerprint(),
		Source:   basketReplaySourceFingerprint(spec.Run.Source),
		Contract: contract.Fingerprint(),
		Input:    inputFP,
	}
	portable, err := spec.configDocument("/basket-replay-state")
	if err != nil {
		return nil, err
	}
	r.fp.Config = sha256Hex(portable)
	runBlob, _ := json.Marshal([]string{r.fp.Protocol, spec.Run.RunID, r.fp.Source, r.fp.Contract, r.fp.Input, r.fp.Config})
	r.fp.Run = sha256Hex(runBlob)
	requested := defaultFillCostPolicy()
	if spec.CostPolicy != nil {
		requested = spec.CostPolicy.policy()
	}
	cost, err := resolveFillCostPolicy(requested, r.fp.Run)
	if err != nil {
		return nil, refuseReplay("spec_invalid", err.Error())
	}
	r.cost = cost
	r.manifest = basketReplayManifest{Owner: basketReplayOwnerMarker, RunID: spec.Run.RunID, Fingerprints: r.fp, CostPolicyID: cost.PolicyID, CostVersion: cost.ModelVersion, StepsTotal: len(spec.Steps), UID: os.Getuid()}
	start := spec.parsedSteps[0]
	if spec.Mode == basketReplayModeResume {
		saved, err := readBasketReplayManifest(stateDir)
		if err != nil {
			return nil, err
		}
		if err := compareBasketReplayManifest(saved, r.manifest); err != nil {
			return nil, err
		}
		progress, err := readBasketReplayProgressReadOnly(filepath.Join(stateDir, basketReplayDBName), r.manifest)
		if err != nil {
			return nil, err
		}
		if progress.StepsApplied > len(spec.Steps) {
			return nil, refuseReplay("resume_refused", fmt.Sprintf("the saved cursor is at step %d of a %d-step script", progress.StepsApplied, len(spec.Steps)))
		}
		if progress.StepsApplied > 0 {
			start = time.UnixMilli(progress.CursorMs).UTC()
			if want := spec.parsedSteps[progress.StepsApplied-1]; !want.Equal(start) {
				return nil, refuseReplay("resume_refused", fmt.Sprintf("the saved cursor %s is not the time of step %d (%s)", start.Format(time.RFC3339Nano), progress.StepsApplied-1, want.Format(time.RFC3339Nano)))
			}
		}
		r.progress = progress
		r.resumedAt = progress.StepsApplied
	}
	if err := r.clock.Set(start); err != nil {
		return nil, err
	}
	if err := bindTradingClock(basketReplayClockOwner, r.clock.Now); err != nil {
		return nil, err
	}
	r.cleanups = append(r.cleanups, func() { unbindTradingClock(basketReplayClockOwner) })
	if err := bindFillCostPolicy(basketReplayClockOwner, cost); err != nil {
		return nil, err
	}
	r.cleanups = append(r.cleanups, func() { unbindFillCostPolicy(basketReplayClockOwner) })
	r.lots = newFrozenHLLotMetadataCache(spec.LotMetadata, func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) })
	prevLots := hlLotMetadata
	hlLotMetadata = r.lots
	r.cleanups = append(r.cleanups, func() { hlLotMetadata = prevLots })
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	r.cleanups = append(r.cleanups, func() { tradeRecorder = prevRecorder })
	prevFundingClock := paperFundingClock
	paperFundingClock = func() time.Time { return tradingNow().UTC() }
	r.cleanups = append(r.cleanups, func() { paperFundingClock = prevFundingClock })

	if spec.Mode == basketReplayModeNew {
		r.manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := createBasketReplayStateDir(stateDir, r.manifest); err != nil {
			return nil, err
		}
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	for _, f := range layout.Files {
		if f.InMemory || filepath.Dir(f.Path) != stateDir {
			return nil, refuseReplay("state_dir_refused", fmt.Sprintf("state file %q is outside the run state directory", f.Path))
		}
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		return nil, refuseReplay("state_busy", err.Error())
	}
	r.owner = owned
	r.cleanups = append(r.cleanups, func() { owned.Release() })
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		return nil, err
	}
	r.store, r.layout = store, layout
	r.cleanups = append(r.cleanups, func() { store.Close() })
	r.partition = partitionFor(cfg.Strategies[0])
	r.file = store.file(layout.roleForPartition(r.partition))
	if r.file == nil {
		return nil, fmt.Errorf("no state file owns the %s partition", partitionLabel(r.partition))
	}
	if err := r.file.ensureBasketReplaySchema(); err != nil {
		return nil, err
	}
	if spec.Mode == basketReplayModeNew {
		if err := r.file.insertBasketReplayRun(r.manifest, cost); err != nil {
			return nil, err
		}
	} else {
		if err := r.file.verifyBasketReplayPolicy(cost, r.fp.Run); err != nil {
			return nil, err
		}
		p, err := r.file.readBasketReplayProgress(spec.Run.RunID)
		if err != nil {
			return nil, err
		}
		if p != r.progress {
			return nil, refuseReplay("resume_refused", "the run progress changed while the adapter opened the state file")
		}
		stored, err := r.file.readBasketReplaySteps(spec.Run.RunID, p.StepsApplied)
		if err != nil {
			return nil, err
		}
		r.stored = stored
	}
	if err := r.loadState(); err != nil {
		return nil, err
	}
	store.saveExtra = func(tx *sql.Tx, role storageRole) error {
		if role != r.file.role {
			return nil
		}
		return writeBasketReplayProgressTx(tx, spec.Run.RunID, r.progress)
	}
	ok = true
	return r, nil
}

func readBasketReplayProgressReadOnly(path string, want basketReplayManifest) (basketReplayProgress, error) {
	if err := verifyBasketReplayDBReadOnly(path, want); err != nil {
		return basketReplayProgress{}, err
	}
	db, err := openStateDBReadOnly(path)
	if err != nil {
		return basketReplayProgress{}, refuseReplay("state_foreign", err.Error())
	}
	defer db.Close()
	p, err := db.readBasketReplayProgress(want.RunID)
	if err != nil {
		return basketReplayProgress{}, refuseReplay("state_foreign", err.Error())
	}
	return p, nil
}

func basketReplayConfigRefusal(cfg *Config, stateDir string) error {
	var errs []string
	if len(cfg.Baskets) != 1 {
		errs = append(errs, fmt.Sprintf("the replay runs exactly one basket, got %d", len(cfg.Baskets)))
	}
	if len(cfg.PaperSources) > 0 || strings.TrimSpace(cfg.PaperDBFile) != "" {
		errs = append(errs, "the replay owns one state file; paper sources and a paper db file are refused")
	}
	if filepath.Dir(cfg.DBFile) != stateDir {
		errs = append(errs, fmt.Sprintf("db_file %q is outside the run state directory", cfg.DBFile))
	}
	for _, sc := range cfg.Strategies {
		switch {
		case !isBasketMember(sc):
			errs = append(errs, fmt.Sprintf("strategy %s is not a basket member", sc.ID))
		case sc.Platform != "hyperliquid" || sc.Type != "perps":
			errs = append(errs, fmt.Sprintf("strategy %s is %s %s; the replay runs Hyperliquid perps paper members only", sc.ID, sc.Platform, sc.Type))
		case isLiveArgs(sc.Args) || basketLiveRefusal(sc) != "" || partitionFor(sc).IsLive():
			errs = append(errs, fmt.Sprintf("strategy %s is live-shaped; live members are refused", sc.ID))
		case strategyUsesUnifiedRegimeClose(sc) || sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured():
			errs = append(errs, fmt.Sprintf("strategy %s reads regime labels, which the replay does not freeze", sc.ID))
		}
	}
	if req, err := deriveFeedRequirements(cfg); err != nil {
		errs = append(errs, err.Error())
	} else {
		kinds := map[string]bool{}
		for k := range req.Observations {
			kinds[k.Kind] = true
		}
		for kind := range kinds {
			errs = append(errs, fmt.Sprintf("the configuration needs %s observations, which the replay does not freeze (it builds no sealed feed snapshot of any version)", kind))
		}
		for _, id := range sortedStrategyRequirementIDs(req) {
			if req.Strategies[id].MacroEvents {
				errs = append(errs, fmt.Sprintf("strategy %s needs macro calendar and quote records, which the replay does not freeze", id))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return refuseReplay("config_refused", errs...)
	}
	return nil
}

func (r *basketReplayRuntime) loadState() error {
	cfg, store := r.cfg, r.store
	state, orphans, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return err
	}
	if len(orphans) > 0 {
		return refuseReplay("state_foreign", fmt.Sprintf("the run state file holds %d strategy rows the run does not configure", len(orphans)))
	}
	ValidateState(state, cfg.Strategies)
	persisted := make(map[string]bool, len(state.Strategies))
	for id := range state.Strategies {
		persisted[id] = true
	}
	if r.spec.Mode == basketReplayModeNew && len(persisted) > 0 {
		return refuseReplay("state_exists", "a new run state file already holds strategy books")
	}
	for i := range cfg.Strategies {
		sc := cfg.Strategies[i]
		if _, ok := state.Strategies[sc.ID]; !ok {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if _, errs := ReconcileConfigInitialCapital(cfg, state, store); len(errs) > 0 {
		return fmt.Errorf("initial capital: %s", strings.Join(errs, "; "))
	}
	syncPaperFundingEligibility(state, cfg.Strategies, r.clock.Now())
	if err := store.LoadEntryGuards(state, cfg.Strategies, persisted); err != nil {
		return err
	}
	runtimes, err := store.LoadBaskets(state, cfg)
	if err != nil {
		return err
	}
	r.state = state
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		return err
	}
	r.feedReq = feedReq
	r.intervals = effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(cfg))
	r.need = make(map[marketFeedKey]int)
	for _, entry := range feedReq.Strategies {
		if entry.SignalLookback > r.need[entry.Signal] {
			r.need[entry.Signal] = entry.SignalLookback
		}
	}
	for _, b := range cfg.Baskets {
		for _, sc := range basketMemberConfigs(cfg, b) {
			coin, tf := strategyArgSymbolTimeframe(sc.Args)
			k := feedKeyFor(coin, tf)
			if l := basketFrameLookback(b.LookbackBars); l > r.need[k] {
				r.need[k] = l
			}
		}
	}
	r.members = append([]StrategyConfig(nil), cfg.Strategies...)
	sort.Slice(r.members, func(i, j int) bool { return r.members[i].ID < r.members[j].ID })
	actions, err := r.file.basketReplayActionCount()
	if err != nil {
		return err
	}
	bounds, err := r.file.basketReplaySubmitBounds(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	r.venue = &basketReplayVenue{r: r, tag: shortFingerprint(r.fp.Run)[:8], seq: actions, bounds: bounds}
	bc := newBasketCoordinator(cfg, state, store, r.mu, nil, nil, runtimes)
	bc.parent = context.Background()
	bc.rankClock = basketRankClockSimulated
	bc.lots = r.lots
	bc.venueFor = func(sc StrategyConfig) (basketVenue, error) {
		if msg := basketLiveRefusal(sc); msg != "" || isLiveArgs(sc.Args) {
			return nil, fmt.Errorf("strategy[%s]: live basket dispatch is refused", sc.ID)
		}
		return r.venue, nil
	}
	if r.spec.TargetInput.Mode == basketReplayTargetsBaseline {
		bc.rankRun = basketBaselineRankRunner(r.spec.baseline)
		bc.targetPolicy = basketTargetsOfflineBaseline
	} else {
		bc.rankRun = runBasketRankerProcess
	}
	if err := bc.startupCheck(); err != nil {
		return refuseReplay("state_refused", err.Error())
	}
	bc.recoverInterrupted(r.clock.Now())
	r.bc = bc
	return nil
}

func (r *basketReplayRuntime) frameBars(key marketFeedKey, cutoff, at time.Time) ([]feedBar, int, bool) {
	bars := r.spec.parsedCandles[key]
	ivl, _ := hlCandleIntervalMs(key.Timeframe)
	cutMs, atMs := cutoff.UnixMilli(), at.UnixMilli()
	k := sort.Search(len(bars), func(i int) bool { return bars[i].OpenMs+ivl > cutMs })
	need := r.need[key]
	keep := need + 2
	from := k - keep
	if from < 0 {
		from = 0
	}
	out := make([]feedBar, 0, k-from+1)
	for _, b := range bars[from:k] {
		b.RecvAt = time.UnixMilli(b.CloseMs + 1).UTC()
		out = append(out, b)
	}
	if k < len(bars) && bars[k].OpenMs <= atMs && bars[k].OpenMs <= cutMs {
		f := bars[k]
		out = append(out, feedBar{OpenMs: f.OpenMs, CloseMs: f.CloseMs, HasClose: false, Open: f.Open, High: f.Open, Low: f.Open, Close: f.Open, Source: f.Source, RecvAt: at})
	}
	return out, need, len(out) >= need && need > 0
}

func (r *basketReplayRuntime) snapshot(at, cutoff time.Time, deadline time.Time, marks map[string]float64, id string) *marketSnapshot {
	snap := &marketSnapshot{
		Version:          marketSnapshotVersion,
		EvaluationID:     id,
		ConfigGeneration: 1,
		SealedAt:         at,
		Deadline:         deadline,
		keys:             make(map[marketFeedKey]*marketSnapshotKey),
		mids:             make(map[string]feedMid),
		funding:          make(map[string]feedFunding),
		observations:     make(map[feedObservationKey]*marketSnapshotObservation),
	}
	keys := make([]marketFeedKey, 0, len(r.spec.parsedCandles))
	for k := range r.spec.parsedCandles {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return marketFeedKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		ivl, _ := hlCandleIntervalMs(key.Timeframe)
		bars, need, ready := r.frameBars(key, cutoff, at)
		entry := &marketSnapshotKey{Key: key, IntervalMs: ivl, Bars: bars}
		entry.Readiness = feedKeyReadiness{Key: key, Bars: len(bars), Required: need, Ready: ready, Status: feedStatusReady, Source: "replay", LastRecvAt: at}
		if len(bars) > 0 {
			entry.Readiness.FirstOpenMs, entry.Readiness.LastOpenMs, entry.Readiness.LastCloseMs = bars[0].OpenMs, bars[len(bars)-1].OpenMs, bars[len(bars)-1].CloseMs
		}
		if !ready {
			entry.Readiness.Status = feedStatusShort
			entry.Readiness.CoverageShort = true
			entry.Readiness.Detail = fmt.Sprintf("%d frozen bars at %s, below the %d the replay needs", len(bars), cutoff.Format(time.RFC3339), need)
		}
		snap.keys[key] = entry
	}
	for coin, px := range marks {
		snap.mids[coin] = feedMid{Px: px, RecvAt: at, Source: "replay"}
	}
	atMs := at.UnixMilli()
	for coin, cov := range r.spec.funding {
		var recs []feedFundingRecord
		for _, rec := range cov.Records {
			if rec.TimeMs > atMs {
				break
			}
			recs = append(recs, rec)
		}
		if len(recs) == 0 {
			continue
		}
		f := feedFunding{Current: recs[len(recs)-1].Rate, HasScalar: true, Records: recs, HasRecords: true, FetchedAt: at, Source: "replay"}
		var sum float64
		var n int
		for _, rec := range recs {
			if rec.TimeMs >= atMs-7*24*time.Hour.Milliseconds() {
				sum += rec.Rate
				n++
			}
		}
		if n > 0 {
			f.Avg7d = sum / float64(n)
		}
		snap.funding[coin] = f
	}
	return snap
}

func (r *basketReplayRuntime) fundingInputs(need map[string]int64, at time.Time) paperFundingInputs {
	in := paperFundingInputs{Source: "replay", Coverage: make(map[string]feedFundingCoverage), Detail: make(map[string]string), NotServed: make(map[string]bool)}
	atMs := at.UnixMilli()
	coins := make([]string, 0, len(need))
	for c := range need {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		cov, ok := r.spec.funding[coin]
		if !ok || cov.FromMs > atMs {
			in.Detail[coin] = "the step script has no frozen funding coverage for this coin at this time"
			continue
		}
		out := feedFundingCoverage{Coin: coin, FromMs: cov.FromMs}
		for _, rec := range cov.Records {
			if rec.TimeMs > atMs {
				break
			}
			out.Records = append(out.Records, rec)
		}
		if len(out.Records) > 0 {
			out.ToMs = out.Records[len(out.Records)-1].TimeMs
		}
		in.Coverage[coin] = out
	}
	return in
}

func (r *basketReplayRuntime) Run() error {
	steps := r.spec.parsedSteps
	limit := len(steps)
	if r.opts.MaxSteps > 0 && r.progress.StepsApplied+r.opts.MaxSteps < limit {
		limit = r.progress.StepsApplied + r.opts.MaxSteps
	}
	for i := r.progress.StepsApplied; i < limit; i++ {
		at := steps[i]
		if r.progress.StepsApplied > 0 && at.UnixMilli() < r.progress.CursorMs {
			return refuseReplay("step_before_cursor", fmt.Sprintf("step %d at %s is earlier than the saved cursor %s; nothing was changed", i, at.Format(time.RFC3339Nano), time.UnixMilli(r.progress.CursorMs).UTC().Format(time.RFC3339Nano)))
		}
		if err := r.clock.Set(at); err != nil {
			return refuseReplay("step_before_cursor", err.Error())
		}
		rec, saved, err := r.runStep(i, at, r.spec.Steps[i])
		if err != nil {
			return err
		}
		r.records = append(r.records, rec)
		lots := r.lots.fetchRefusals()
		r.pending = append(r.pending, basketReplayStoredStep{Record: rec, LotRefusals: lots - r.lotMark})
		r.lotMark = lots
		if saved {
			r.flushStepRecords()
		}
	}
	return nil
}

func (r *basketReplayRuntime) flushStepRecords() {
	if len(r.pending) == 0 {
		return
	}
	if err := r.file.writeBasketReplaySteps(r.spec.Run.RunID, r.pending); err != nil {
		fmt.Fprintf(os.Stderr, "[basket-replay] could not record %d step record(s) after a durable save: %v\n", len(r.pending), err)
		return
	}
	r.pending = nil
}

func (r *basketReplayRuntime) runStep(i int, at time.Time, step basketReplayStep) (basketReplayStepRecord, bool, error) {
	cfg, state, store, mu := r.cfg, r.state, r.store, r.mu
	if step.SaveFailure {
		prev := storeCommitHook
		storeCommitHook = func(role storageRole) error { return fmt.Errorf("scripted state save failure at step %d", i) }
		defer func() { storeCommitHook = prev }()
	}
	snap := r.snapshot(at, at, time.Time{}, step.Marks, fmt.Sprintf("replay/%s/%d", r.spec.Run.RunID, i))
	feedCtx := &marketFeedContext{Enabled: true, Requirements: r.feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
	prices := snap.freshMids()
	r.venue.beginStep(i, step)
	r.bc.beginCycle()
	rec := basketReplayStepRecord{Index: i, At: at.Format(time.RFC3339Nano)}

	mu.RLock()
	need := planPaperFunding(state, cfg.Strategies)
	mu.RUnlock()
	inputs := r.fundingInputs(need, at)
	mu.Lock()
	fres := runPaperFundingAccounting(state, cfg.Strategies, store, inputs, prices, func(p RiskPartition) bool { return partitionSaveBlocked(store, p) }, at)
	mu.Unlock()
	rec.FundingBooked = fres.Booked
	for _, line := range fres.Logs {
		fmt.Fprintln(os.Stderr, line)
	}

	parts := activePartitions(cfg.Strategies)
	scopeRisk := make(map[RiskPartition]*scopeCycleRisk, len(parts))
	for _, p := range parts {
		scopeRisk[p] = &scopeCycleRisk{Partition: p, Config: partitionRiskConfig(cfg, p)}
	}
	deadlineSnaps := make(map[int64]*marketSnapshot)
	snapshotFor := func(rt *basketRuntime, d time.Time) *marketSnapshot {
		key := d.UnixMilli()
		if s, ok := deadlineSnaps[key]; ok {
			return s
		}
		s := r.snapshot(at, d, d, step.Marks, fmt.Sprintf("replay/%s/%d/deadline/%d", r.spec.Run.RunID, i, key))
		deadlineSnaps[key] = s
		return s
	}
	if allPartitionsSaveBlocked(store, cfg) {
		rec.SaveBlocked = true
		r.bc.step(basketCycleInputs{Now: at, Snapshot: snap, SnapshotFor: snapshotFor, SaveBlocked: true, Once: true})
	} else {
		mu.RLock()
		for _, p := range parts {
			scopeRisk[p] = measureScopeCycleRisk(p, scopeRisk[p].Config, cfg.Strategies, state, prices, nil, nil, true, false, at)
		}
		mu.RUnlock()
		mu.Lock()
		for _, p := range parts {
			sr := scopeRisk[p]
			applyScopeCycleRisk(sr, state.partitionRisk(p))
			if !p.IsLive() && sr.KillSwitchFired {
				out := applyPaperKillSwitchCycle(state, cfg, prices, sr, true)
				if out.CloseApplied {
					fmt.Fprintf(os.Stderr, "[CRITICAL] Portfolio kill switch [%s]: force-closed %d paper strategy book(s) at mark\n", partitionLabel(p), len(out.Closed))
				}
			}
		}
		mu.Unlock()
		due := dueStrategiesPersistable(store, dueStrategiesNotLatched(r.members, scopeRisk))
		for _, sc := range due {
			r.manageMember(sc, scopeRisk, prices, feedCtx)
		}
		r.bc.step(basketCycleInputs{Now: at, Snapshot: snap, SnapshotFor: snapshotFor, ScopeRisk: scopeRisk, Once: true})
	}
	r.progress = basketReplayProgress{StepsApplied: i + 1, CursorMs: at.UnixMilli()}
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
	saveErr, attempted := outcomes[r.partition]
	r.fillRecord(&rec, at, step, scopeRisk, prices)
	return rec, attempted && saveErr == nil, nil
}

func (r *basketReplayRuntime) manageMember(sc StrategyConfig, scopeRisk map[RiskPartition]*scopeCycleRisk, prices map[string]float64, feedCtx *marketFeedContext) {
	cfg, state, store, mu := r.cfg, r.state, r.store, r.mu
	s := state.Strategies[sc.ID]
	if s == nil {
		return
	}
	stratDB, err := store.dbForStrategy(sc.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[storage] CRITICAL: %v — skipping %s this step\n", err, sc.ID)
		return
	}
	part := partitionFor(sc)
	sr := scopeRisk[part]
	if sr == nil {
		sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(cfg, part)}
	}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
	sym := hyperliquidSymbol(sc.Args)
	mu.RLock()
	pv := PortfolioValue(s, prices)
	pos, hasPos := readHLPerpsPosition(sc, s, sym, cfg.Regime)
	scaleInCash := s.Cash
	mu.RUnlock()
	mu.Lock()
	allowed, reason := CheckRisk(&sc, s, pv, prices, logger, nil)
	mu.Unlock()
	cbManageOnly := false
	if !allowed {
		logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
		if circuitBreakerPermitsManagement(reason, sc.Platform, sc.Type, pos.Ctx.Quantity) {
			cbManageOnly = true
		} else {
			return
		}
	}
	if notionalCapSkipsStrategyCycle(sr.NotionalBlocked) {
		logger.Warn("Notional cap exceeded — skipping strategy cycle")
		return
	}
	x := &hlPerpsStep{
		sc: sc, mu: mu, logger: logger, stratState: s, notifier: r.notifier, cfg: cfg, stratDB: stratDB, store: store, state: state,
		sr: sr, prices: prices, feedCtx: feedCtx, basketCoord: r.bc, intervals: r.intervals, stratScope: part.Scope,
		persistenceHold: store.persistenceHoldsPartition(part), cbManageOnly: cbManageOnly, hlScaleInCash: scaleInCash,
	}
	if hasPos {
		x.hlPosCtx, x.hlPosSide, x.hlPosQty, x.hlAvgCost = pos.Ctx, pos.Ctx.Side, pos.Ctx.Quantity, pos.Ctx.AvgCost
		x.hlPosLeverage, x.hlEntryATR = pos.Leverage, pos.EntryATR
		x.hlStopLossOID, x.hlTPOIDs = pos.StopLossOID, pos.TPOIDs
		x.hlStopLossTriggerPx, x.hlStopLossHighWaterPx = pos.StopLossTriggerPx, pos.StopLossHighWaterPx
		x.hlPosSnapshot = pos.Snapshot
		x.hlScaleInCount, x.hlLastAddPrice, x.hlAddedNotionalUSD = pos.ScaleInCount, pos.LastAddPrice, pos.AddedNotionalUSD
		x.hlScaleInResizePending = pos.ScaleInResizePending
		x.hlSharedCloseHoldUSD, x.hlSharedCloseHoldReason = pos.SharedCloseHoldUSD, pos.SharedCloseHoldReason
	}
	runHyperliquidPerpsStep(x)
}

type basketReplayVenue struct {
	r       *basketReplayRuntime
	tag     string
	seq     int
	step    int
	script  basketReplayStep
	submits int
	bounds  map[string]basketReplayBoundRecord
}

func (v *basketReplayVenue) beginStep(i int, step basketReplayStep) {
	v.step, v.script = i, step
}

func (v *basketReplayVenue) Name() string                      { return "replay" }
func (v *basketReplayVenue) LiveShaped() bool                  { return false }
func (v *basketReplayVenue) Provider() string                  { return "paper" }
func (v *basketReplayVenue) AccountKey(p RiskPartition) string { return "paper:" + p.String() }

func (v *basketReplayVenue) NewClientOrderID() (string, error) {
	v.seq++
	return fmt.Sprintf("rp-%s-%06d", v.tag, v.seq), nil
}

func basketReplayActionBounds(mu *sync.RWMutex, bc *basketCoordinator, state *AppState, marks map[string]float64, a *basketAction) *basketReplayBoundRecord {
	mu.RLock()
	defer mu.RUnlock()
	rt := bc.runtimes[a.BasketID]
	if rt == nil {
		return nil
	}
	px := make(map[string]float64, len(rt.Contract.Members))
	before := basketExposure{Qty: make(map[string]float64), Mark: px}
	for _, m := range rt.Contract.Members {
		before.Qty[m.Coin] = basketPositionSigned(state.Strategies[m.StrategyID], m.Coin)
		if p, ok := marks[m.Coin]; ok {
			px[m.Coin] = p
		}
	}
	px[a.Symbol] = a.MarkPx
	after := before.clone()
	if a.OrderSide == "buy" {
		after.Qty[a.Symbol] += a.RequestedQty
	} else {
		after.Qty[a.Symbol] -= a.RequestedQty
	}
	start, end := before.reading(), after.reading()
	legal, why := basketEndpointLegal(start, end, basketBoundsFor(rt.Contract), a.Direction == basketDirectionIncrease)
	return &basketReplayBoundRecord{Start: basketReplayReading(start), End: basketReplayReading(end), Legal: legal, Detail: why}
}

func (v *basketReplayVenue) captureBounds(a *basketAction) *basketReplayBoundRecord {
	b := basketReplayActionBounds(v.r.mu, v.r.bc, v.r.state, v.script.Marks, a)
	if b == nil {
		return nil
	}
	if v.bounds == nil {
		v.bounds = make(map[string]basketReplayBoundRecord)
	}
	v.bounds[a.ActionID] = *b
	return b
}

func (v *basketReplayVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	r := v.r
	idx, err := r.file.countBasketReplayVenueEvents(r.spec.Run.RunID, v.step, "submit")
	if err != nil {
		return basketVenueOutcome{}, fmt.Errorf("the replay venue could not read its evidence: %w", err)
	}
	scripted := basketReplayScriptedOutcome{Outcome: basketReplayOutcomeFilled}
	if idx < len(v.script.Venue) {
		scripted = v.script.Venue[idx]
	}
	bounds := v.captureBounds(a)
	nowMs := tradingNow().UnixMilli()
	out := basketVenueOutcome{ObservedAtMs: nowMs, VenueOrderID: fmt.Sprintf("replay-%s", a.ClientOrderID)}
	var transport error
	switch scripted.Outcome {
	case basketReplayOutcomeFilled:
		out.Outcome, out.FilledQty = basketVenueFilled, a.RequestedQty
	case basketReplayOutcomePartial:
		lot := r.lots.Peek(a.Symbol)
		qty := a.RequestedQty * scripted.FillFraction
		if lot.Known {
			qty = hlFloorLotSize(qty, lot.SzDecimals)
		}
		if qty > 0 {
			out.Outcome, out.FilledQty = basketVenuePartial, qty
		} else {
			out.Outcome, out.Detail = basketVenueRejected, "the scripted partial fill is below one lot, so nothing filled"
		}
	case basketReplayOutcomeRejected:
		out.Outcome, out.Detail = basketVenueRejected, scriptedDetail(scripted, "the scripted venue rejected the order")
	case basketReplayOutcomeUnknown:
		out.Outcome, out.Detail = basketVenueUnknown, scriptedDetail(scripted, "the scripted venue did not report a status")
	case basketReplayOutcomeTransport:
		out = basketVenueOutcome{Outcome: basketVenueRejected, Detail: "the request never reached the scripted venue", ObservedAtMs: nowMs}
		transport = fmt.Errorf("%s", scriptedDetail(scripted, "scripted transport failure"))
	}
	if err := r.file.insertBasketReplayVenueEvent(r.spec.Run.RunID, basketReplayVenueEvent{ClientOrderID: a.ClientOrderID, ActionID: a.ActionID, Kind: "submit", StepIndex: v.step, ScriptIndex: idx, Outcome: out, Bounds: bounds}, nowMs); err != nil {
		return basketVenueOutcome{}, fmt.Errorf("the replay venue could not record the order: %w", err)
	}
	v.submits++
	if v.r.opts.Fault.ExitAfterSubmit > 0 && v.submits == v.r.opts.Fault.ExitAfterSubmit {
		fmt.Fprintf(os.Stderr, "[basket-replay] fault injection: exiting after submit %d (client %s)\n", v.submits, a.ClientOrderID)
		os.Exit(basketReplayExitFault)
	}
	if transport != nil {
		return basketVenueOutcome{}, transport
	}
	return out, nil
}

func scriptedDetail(o basketReplayScriptedOutcome, fallback string) string {
	if strings.TrimSpace(o.Detail) != "" {
		return o.Detail
	}
	return fallback
}

func (v *basketReplayVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	r := v.r
	events, err := r.file.basketReplayVenueEvents(r.spec.Run.RunID, a.ClientOrderID)
	if err != nil {
		return basketVenueOutcome{}, err
	}
	nowMs := tradingNow().UnixMilli()
	if len(events) == 0 {
		return basketVenueOutcome{Outcome: basketVenueRejected, Detail: "the scripted venue never received this client order id, so nothing filled", ObservedAtMs: nowMs}, nil
	}
	last := events[len(events)-1].Outcome
	if last.Outcome != basketVenueUnknown {
		return last, nil
	}
	idx, err := r.file.countBasketReplayVenueEvents(r.spec.Run.RunID, v.step, "query")
	if err != nil {
		return basketVenueOutcome{}, err
	}
	if idx >= len(v.script.Queries) {
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "the scripted venue has not resolved the order yet", ObservedAtMs: nowMs}, nil
	}
	scripted := v.script.Queries[idx]
	out := basketVenueOutcome{ObservedAtMs: nowMs, VenueOrderID: last.VenueOrderID}
	switch scripted.Outcome {
	case basketReplayOutcomeFilled:
		out.Outcome, out.FilledQty = basketVenueFilled, a.RequestedQty
	case basketReplayOutcomePartial:
		lot := r.lots.Peek(a.Symbol)
		qty := a.RequestedQty * scripted.FillFraction
		if lot.Known {
			qty = hlFloorLotSize(qty, lot.SzDecimals)
		}
		out.Outcome, out.FilledQty = basketVenuePartial, qty
		if qty <= 0 {
			out.Outcome, out.FilledQty, out.Detail = basketVenueRejected, 0, "the scripted partial fill is below one lot, so nothing filled"
		}
	case basketReplayOutcomeRejected, basketReplayOutcomeTransport:
		out.Outcome, out.Detail = basketVenueRejected, scriptedDetail(scripted, "the scripted venue reports the order rejected")
	default:
		out.Outcome, out.Detail = basketVenueUnknown, scriptedDetail(scripted, "the scripted venue still reports no status")
	}
	if err := r.file.insertBasketReplayVenueEvent(r.spec.Run.RunID, basketReplayVenueEvent{ClientOrderID: a.ClientOrderID, ActionID: a.ActionID, Kind: "query", StepIndex: v.step, ScriptIndex: idx, Outcome: out}, nowMs); err != nil {
		return basketVenueOutcome{}, err
	}
	return out, nil
}

type basketReplayReadingRecord struct {
	Gross   float64            `json:"gross_usd"`
	Net     float64            `json:"abs_net_usd"`
	PerCoin map[string]float64 `json:"per_coin_usd"`
}

func basketReplayReading(b basketBoundReading) basketReplayReadingRecord {
	return basketReplayReadingRecord{Gross: b.Gross, Net: b.Net, PerCoin: b.PerCoin}
}

type basketReplayBoundRecord struct {
	Start  basketReplayReadingRecord `json:"start"`
	End    basketReplayReadingRecord `json:"end"`
	Legal  bool                      `json:"legal"`
	Detail string                    `json:"detail,omitempty"`
}

type basketReplayRebalanceRecord struct {
	DeadlineMs      int64                         `json:"deadline_ms"`
	Status          string                        `json:"status"`
	HoldReason      string                        `json:"hold_reason,omitempty"`
	Result          string                        `json:"result,omitempty"`
	IncreasesFrozen bool                          `json:"increases_frozen"`
	RecoveryOpen    bool                          `json:"recovery_open"`
	FreezeReason    string                        `json:"freeze_reason,omitempty"`
	Targets         map[string]basketMemberTarget `json:"targets,omitempty"`
	Exceptions      []basketException             `json:"exceptions,omitempty"`
	Deviations      []basketDeviation             `json:"deviations,omitempty"`
	InputFP         string                        `json:"input_fingerprint,omitempty"`
}

type basketReplayActionRecord struct {
	ActionID       string                   `json:"action_id"`
	DeadlineMs     int64                    `json:"deadline_ms"`
	StrategyID     string                   `json:"strategy_id"`
	Symbol         string                   `json:"symbol"`
	Direction      string                   `json:"direction"`
	Seq            int                      `json:"seq"`
	OrderSide      string                   `json:"order_side"`
	Status         string                   `json:"status"`
	RequestedQty   float64                  `json:"requested_qty"`
	ConfirmedQty   float64                  `json:"confirmed_qty"`
	AppliedQty     float64                  `json:"applied_qty"`
	AppliedFee     float64                  `json:"applied_fee"`
	MarkPx         float64                  `json:"mark_px"`
	ClientOrderID  string                   `json:"client_order_id"`
	HoldReason     string                   `json:"hold_reason,omitempty"`
	Bounds         *basketReplayBoundRecord `json:"bounds,omitempty"`
	CreatedAtMs    int64                    `json:"created_at_ms"`
	UpdatedAtMs    int64                    `json:"updated_at_ms"`
	PreQty         float64                  `json:"pre_qty"`
	PreNotionalUSD float64                  `json:"pre_notional_usd"`
	Purpose        string                   `json:"purpose,omitempty"`
	ExpiryMs       int64                    `json:"expiry_ms,omitempty"`
	Attempt        int                      `json:"lifecycle_attempt,omitempty"`
}

type basketReplayMemberRecord struct {
	StrategyID          string  `json:"strategy_id"`
	Coin                string  `json:"coin"`
	Cash                float64 `json:"cash"`
	InitialCapital      float64 `json:"initial_capital"`
	Value               float64 `json:"value"`
	Qty                 float64 `json:"signed_qty"`
	AvgCost             float64 `json:"avg_cost,omitempty"`
	EntryATR            float64 `json:"entry_atr,omitempty"`
	StopLossTriggerPx   float64 `json:"stop_loss_trigger_px,omitempty"`
	StopLossHighWaterPx float64 `json:"stop_loss_high_water_px,omitempty"`
	OpenedAtMs          int64   `json:"opened_at_ms,omitempty"`
	ExpectedQty         float64 `json:"expected_qty"`
	DailyPnL            float64 `json:"daily_pnl"`
	DailyPnLDate        string  `json:"daily_pnl_date,omitempty"`
	ConsecutiveLosses   int     `json:"consecutive_losses"`
	CircuitBreaker      bool    `json:"circuit_breaker"`
	CircuitBreakerUntil string  `json:"circuit_breaker_until,omitempty"`
	PeakValue           float64 `json:"peak_value"`
	DrawdownPct         float64 `json:"drawdown_pct"`
	ConsumedThroughMs   int64   `json:"entry_consumed_through_ms,omitempty"`
	LatestObservedMs    int64   `json:"entry_latest_observed_ms,omitempty"`
	FundingSettledMs    int64   `json:"funding_settled_through_ms,omitempty"`
	FundingHeld         int     `json:"funding_held,omitempty"`
	FundingGaps         int     `json:"funding_open_gaps,omitempty"`
	Trades              int     `json:"trade_rows"`
	LifecycleEntryMs    int64   `json:"lifecycle_entry_ms,omitempty"`
	LifecycleExpiryMs   int64   `json:"lifecycle_expiry_ms,omitempty"`
	LifecycleState      string  `json:"lifecycle_state,omitempty"`
	LifecycleAttempt    int     `json:"lifecycle_attempt,omitempty"`
	SuppressThroughMs   int64   `json:"reopen_suppressed_through_ms,omitempty"`
}

type basketReplayPartitionRecord struct {
	Partition        string  `json:"partition"`
	KillSwitchActive bool    `json:"kill_switch_active"`
	KillSwitchAt     string  `json:"kill_switch_at,omitempty"`
	KillSwitchFired  bool    `json:"kill_switch_fired"`
	PeakValue        float64 `json:"peak_value"`
	DrawdownPct      float64 `json:"drawdown_pct"`
	DailyLossHeld    bool    `json:"daily_loss_entries_held"`
	DailyLossUSD     float64 `json:"daily_loss_usd"`
	NotionalBlocked  bool    `json:"notional_blocked"`
	Events           int     `json:"kill_switch_events"`
	LastEvent        string  `json:"last_event,omitempty"`
	LastEventAt      string  `json:"last_event_at,omitempty"`
}

type basketReplayExposureRecord struct {
	GrossUSD   float64            `json:"gross_usd"`
	NetUSD     float64            `json:"net_usd"`
	PerCoinUSD map[string]float64 `json:"per_coin_usd"`
	EquityUSD  float64            `json:"basket_equity_usd"`
}

type basketReplayStepRecord struct {
	Index         int                           `json:"index"`
	At            string                        `json:"at"`
	SaveBlocked   bool                          `json:"save_blocked,omitempty"`
	SaveErrors    []string                      `json:"save_errors,omitempty"`
	FundingBooked int                           `json:"funding_rows_booked"`
	Rebalances    []basketReplayRebalanceRecord `json:"rebalances,omitempty"`
	Actions       []basketReplayActionRecord    `json:"actions,omitempty"`
	Members       []basketReplayMemberRecord    `json:"members"`
	Partition     basketReplayPartitionRecord   `json:"partition"`
	Exposure      basketReplayExposureRecord    `json:"exposure"`
	PendingAction string                        `json:"pending_action,omitempty"`
	NeedsDurable  bool                          `json:"settlement_not_durable,omitempty"`
	Coverage      []string                      `json:"coverage_gaps,omitempty"`
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

type basketReplayRecordInput struct {
	BC        *basketCoordinator
	State     *AppState
	Mu        *sync.RWMutex
	Bounds    map[string]basketReplayBoundRecord
	Partition RiskPartition
	ScopeRisk map[RiskPartition]*scopeCycleRisk
	Prices    map[string]float64
	Marks     map[string]float64
}

func (r *basketReplayRuntime) fillRecord(rec *basketReplayStepRecord, at time.Time, step basketReplayStep, scopeRisk map[RiskPartition]*scopeCycleRisk, prices map[string]float64) {
	lines := buildBasketReplayStepRecord(rec, at, basketReplayRecordInput{BC: r.bc, State: r.state, Mu: r.mu, Bounds: r.venue.bounds, Partition: r.partition, ScopeRisk: scopeRisk, Prices: prices, Marks: step.Marks})
	r.mu.RLock()
	for key, ready := range r.lastSnapshotKeys(at) {
		if !ready {
			lines = append(lines, fmt.Sprintf("step %d: the frozen %s frame is short at %s", rec.Index, key.PayloadID(), at.Format(time.RFC3339)))
		}
	}
	r.mu.RUnlock()
	rec.Coverage = append(rec.Coverage, lines...)
	sort.Strings(rec.Coverage)
}

func buildBasketReplayStepRecord(rec *basketReplayStepRecord, at time.Time, in basketReplayRecordInput) []string {
	in.Mu.RLock()
	defer in.Mu.RUnlock()
	var coverage []string
	atMs := at.UnixMilli()
	ids := in.BC.sortedIDs()
	marks := make(map[string]float64)
	for k, v := range in.Prices {
		marks[k] = v
	}
	var equity float64
	for _, id := range ids {
		rt := in.BC.runtimes[id]
		for _, d := range rt.sortedDeadlines() {
			rb := rt.Rebalances[d]
			if rb.UpdatedAtMs != atMs && rb.CreatedAtMs != atMs {
				continue
			}
			rr := basketReplayRebalanceRecord{DeadlineMs: rb.DeadlineMs, Status: rb.Status, HoldReason: rb.HoldReason, Result: rb.Outcome.Result, IncreasesFrozen: rb.IncreasesFrozen,
				RecoveryOpen: rb.Outcome.RecoveryOpen, FreezeReason: rb.Outcome.FreezeReason, Exceptions: rb.Outcome.Exceptions, Deviations: rb.Outcome.Deviations, InputFP: rb.InputFP}
			if t, err := rb.Targets(); err == nil && t != nil {
				rr.Targets = t.Members
			}
			rec.Rebalances = append(rec.Rebalances, rr)
		}
		for _, aid := range rt.sortedActionIDs() {
			a := rt.Actions[aid]
			if a.UpdatedAtMs != atMs && a.CreatedAtMs != atMs {
				continue
			}
			ar := basketReplayActionRecord{ActionID: a.ActionID, DeadlineMs: a.DeadlineMs, StrategyID: a.StrategyID, Symbol: a.Symbol, Direction: a.Direction, Seq: a.Seq, OrderSide: a.OrderSide,
				Status: a.Status, RequestedQty: a.RequestedQty, ConfirmedQty: a.ConfirmedQty, AppliedQty: a.AppliedQty, AppliedFee: a.AppliedFee, MarkPx: a.MarkPx, ClientOrderID: a.ClientOrderID,
				HoldReason: a.HoldReason, CreatedAtMs: a.CreatedAtMs, UpdatedAtMs: a.UpdatedAtMs, PreQty: a.PreQty, PreNotionalUSD: a.PreNotionalUSD,
				Purpose: a.purpose(), ExpiryMs: a.ExpiryMs, Attempt: a.LifecycleAttempt}
			if b, ok := in.Bounds[a.ActionID]; ok {
				bb := b
				ar.Bounds = &bb
			}
			rec.Actions = append(rec.Actions, ar)
		}
		if a := rt.pendingAction(); a != nil {
			rec.PendingAction = a.ActionID + " " + a.Status
		}
		rec.NeedsDurable = rec.NeedsDurable || rt.needsDurable
		e := basketExposure{Qty: make(map[string]float64), Mark: marks}
		for _, m := range rt.Contract.Members {
			s := in.State.Strategies[m.StrategyID]
			q := basketPositionSigned(s, m.Coin)
			e.Qty[m.Coin] = q
			mr := basketReplayMemberRecord{StrategyID: m.StrategyID, Coin: m.Coin, ExpectedQty: rt.Expected[m.StrategyID], Qty: q}
			if l, ok := rt.Lifecycle[m.StrategyID]; ok {
				mr.LifecycleEntryMs, mr.LifecycleExpiryMs, mr.LifecycleState, mr.LifecycleAttempt, mr.SuppressThroughMs = l.EntryMs, l.ExpiryMs, l.State, l.Attempt, l.SuppressThroughMs
			}
			if s != nil {
				mr.Cash, mr.InitialCapital = s.Cash, s.InitialCapital
				mr.Value = PortfolioValue(s, marks)
				equity += mr.Value
				if pos := s.Positions[m.Coin]; pos != nil {
					mr.AvgCost, mr.EntryATR, mr.StopLossTriggerPx, mr.StopLossHighWaterPx = pos.AvgCost, pos.EntryATR, pos.StopLossTriggerPx, pos.StopLossHighWaterPx
					if !pos.OpenedAt.IsZero() {
						mr.OpenedAtMs = pos.OpenedAt.UnixMilli()
					}
				}
				rs := s.RiskState
				mr.DailyPnL, mr.DailyPnLDate, mr.ConsecutiveLosses, mr.CircuitBreaker = rs.DailyPnL, rs.DailyPnLDate, rs.ConsecutiveLosses, rs.CircuitBreaker
				mr.CircuitBreakerUntil = formatOptionalTime(rs.CircuitBreakerUntil)
				mr.PeakValue, mr.DrawdownPct = rs.PeakValue, rs.CurrentDrawdownPct
				if g := s.EntryGuard; g != nil {
					for key, d := range g.Decisions {
						if key.Symbol == m.Coin && d != nil {
							if d.ConsumedThroughBarMs > mr.ConsumedThroughMs {
								mr.ConsumedThroughMs = d.ConsumedThroughBarMs
							}
							if d.LatestObservedBarMs > mr.LatestObservedMs {
								mr.LatestObservedMs = d.LatestObservedBarMs
							}
						}
					}
				}
				if pf := s.PaperFunding; pf != nil {
					if c := pf.Coins[m.Coin]; c != nil {
						mr.FundingSettledMs, mr.FundingHeld, mr.FundingGaps = c.SettledThroughMs, len(c.Held), len(c.openGaps())
					}
				}
				mr.Trades = len(s.TradeHistory)
			}
			rec.Members = append(rec.Members, mr)
			targeted := false
			if rb := rt.latest(); rb != nil && rb.targets != nil {
				targeted = rb.targets.Members[m.StrategyID].TargetQty != 0
			}
			if _, ok := in.Marks[m.Coin]; !ok && (q != 0 || targeted) {
				line := fmt.Sprintf("step %d: no frozen mark for %s while it is held or targeted", rec.Index, m.Coin)
				coverage = append(coverage, line)
			}
		}
		r1 := e.reading()
		rec.Exposure = basketReplayExposureRecord{GrossUSD: r1.Gross, NetUSD: e.net(), PerCoinUSD: r1.PerCoin, EquityUSD: equity}
	}
	prs := in.State.partitionRiskIfPresent(in.Partition)
	pr := basketReplayPartitionRecord{Partition: in.Partition.String()}
	if prs != nil {
		pr.KillSwitchActive, pr.KillSwitchAt = prs.KillSwitchActive, formatOptionalTime(prs.KillSwitchAt)
		pr.PeakValue, pr.DrawdownPct, pr.Events = prs.PeakValue, prs.CurrentDrawdownPct, len(prs.Events)
		if n := len(prs.Events); n > 0 {
			pr.LastEvent, pr.LastEventAt = prs.Events[n-1].Type, formatOptionalTime(prs.Events[n-1].Timestamp)
		}
	}
	if sr := in.ScopeRisk[in.Partition]; sr != nil {
		pr.KillSwitchFired, pr.DailyLossHeld, pr.DailyLossUSD, pr.NotionalBlocked = sr.KillSwitchFired, sr.DailyLossEntriesHeld, sr.DailyLossStatus.LossUSD, sr.NotionalBlocked
	}
	rec.Partition = pr
	return coverage
}

func (r *basketReplayRuntime) lastSnapshotKeys(at time.Time) map[marketFeedKey]bool {
	out := make(map[marketFeedKey]bool)
	for _, sc := range r.members {
		coin, tf := strategyArgSymbolTimeframe(sc.Args)
		key := feedKeyFor(coin, tf)
		_, _, ready := r.frameBars(key, at, at)
		out[key] = ready
	}
	return out
}

type basketReplayTradeRecord struct {
	StrategyID       string  `json:"strategy_id"`
	Timestamp        string  `json:"timestamp"`
	Symbol           string  `json:"symbol"`
	PositionID       string  `json:"position_id,omitempty"`
	Side             string  `json:"side"`
	Quantity         float64 `json:"quantity"`
	Price            float64 `json:"price"`
	Value            float64 `json:"value"`
	TradeType        string  `json:"trade_type"`
	ExchangeOrderID  string  `json:"exchange_order_id,omitempty"`
	ExchangeFee      float64 `json:"exchange_fee"`
	IsClose          bool    `json:"is_close"`
	RealizedPnL      float64 `json:"realized_pnl"`
	CostModelVersion int     `json:"cost_model_version"`
	CostPolicyID     string  `json:"cost_policy_id,omitempty"`
	Details          string  `json:"details,omitempty"`
}

func (r *basketReplayRuntime) storedTrades() ([]basketReplayTradeRecord, error) {
	var out []basketReplayTradeRecord
	for _, sc := range r.members {
		rows, _, err := r.store.QueryTradeHistory(sc.ID, "", time.Time{}, time.Time{}, 1_000_000, 0)
		if err != nil {
			return nil, err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			t := rows[i]
			out = append(out, basketReplayTradeRecord{StrategyID: sc.ID, Timestamp: t.Timestamp.UTC().Format(time.RFC3339Nano), Symbol: t.Symbol, PositionID: t.PositionID, Side: t.Side,
				Quantity: t.Quantity, Price: t.Price, Value: t.Value, TradeType: t.TradeType, ExchangeOrderID: t.ExchangeOrderID, ExchangeFee: t.ExchangeFee, IsClose: t.IsClose,
				RealizedPnL: t.RealizedPnL, CostModelVersion: t.CostModelVersion, CostPolicyID: t.CostPolicyID, Details: t.Details})
		}
	}
	return out, nil
}

func (r *basketReplayRuntime) fundingCoverageGaps() []string {
	var out []string
	r.mu.RLock()
	defer r.mu.RUnlock()
	lastMs := r.progress.CursorMs
	for _, sc := range r.members {
		s := r.state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		if s.paperFundingCorrupt {
			out = append(out, fmt.Sprintf("%s: the paper funding state is unreadable", sc.ID))
			continue
		}
		pf := s.PaperFunding
		if pf == nil {
			continue
		}
		syms := make([]string, 0, len(pf.Coins))
		for sym := range pf.Coins {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			c := pf.Coins[sym]
			if len(c.Held) > 0 {
				out = append(out, fmt.Sprintf("%s %s: %d funding record(s) are held unresolved", sc.ID, sym, len(c.Held)))
			}
			if g := c.openGaps(); len(g) > 0 {
				out = append(out, fmt.Sprintf("%s %s: %d funding gap(s) while a position was held", sc.ID, sym, len(g)))
			}
			if len(c.Unknown) > 0 {
				out = append(out, fmt.Sprintf("%s %s: %d unknown inventory window(s)", sc.ID, sym, len(c.Unknown)))
			}
			if c.exposureNonzero() {
				limit := lastMs - paperFundingGapSpacing.Milliseconds()
				if c.SettledThroughMs < limit {
					out = append(out, fmt.Sprintf("%s %s: funding is settled through %s, short of the last step %s", sc.ID, sym, formatClosedBarMs(c.SettledThroughMs), formatClosedBarMs(lastMs)))
				}
			}
		}
	}
	return out
}

type basketReplayRunInfo struct {
	RunID        string                   `json:"run_id"`
	Mode         string                   `json:"mode"`
	StateDir     string                   `json:"state_dir"`
	Fingerprints basketReplayFingerprints `json:"fingerprints"`
	StepsTotal   int                      `json:"steps_total"`
	StepsApplied int                      `json:"steps_applied"`
	ResumedAt    int                      `json:"resumed_at_step"`
	CursorMs     int64                    `json:"cursor_ms"`
	Cursor       string                   `json:"cursor,omitempty"`
}

type basketReplayCostInfo struct {
	Applied       fillCostPolicy `json:"applied"`
	DefaultParity fillCostPolicy `json:"default_parity"`
	SpreadModel   string         `json:"spread_model"`
	Path          string         `json:"path"`
}

type basketReplayCoverage struct {
	Complete bool     `json:"complete"`
	Gaps     []string `json:"gaps,omitempty"`
}

type basketReplayOutput struct {
	Protocol     string                    `json:"protocol"`
	Version      int                       `json:"version"`
	Status       string                    `json:"status"`
	Refusal      *basketReplayRefusal      `json:"refusal,omitempty"`
	Error        string                    `json:"error,omitempty"`
	Run          *basketReplayRunInfo      `json:"run,omitempty"`
	CapitalModel string                    `json:"capital_model,omitempty"`
	ClockModel   string                    `json:"clock_model,omitempty"`
	EventOrder   string                    `json:"event_order,omitempty"`
	TargetInput  string                    `json:"target_input,omitempty"`
	Cost         *basketReplayCostInfo     `json:"cost,omitempty"`
	Steps        []basketReplayStepRecord  `json:"steps,omitempty"`
	Trades       []basketReplayTradeRecord `json:"trades,omitempty"`
	Coverage     *basketReplayCoverage     `json:"coverage,omitempty"`
	LotFetches   int                       `json:"lot_metadata_fetch_refusals"`
}

const (
	basketReplayStatusComplete   = "complete"
	basketReplayStatusIncomplete = "incomplete"
	basketReplayStatusStopped    = "stopped"
	basketReplayStatusRefused    = "refused"
	basketReplayStatusError      = "error"
)

func (r *basketReplayRuntime) output() (basketReplayOutput, error) {
	out := basketReplayOutput{Protocol: basketReplayProtocolName, Version: basketReplayProtocolVersion, CapitalModel: basketReplayCapitalNote, ClockModel: basketReplayClockNote, EventOrder: basketReplayEventOrder, TargetInput: r.spec.TargetInput.Mode}
	out.Run = &basketReplayRunInfo{RunID: r.spec.Run.RunID, Mode: r.spec.Mode, StateDir: r.spec.Run.StateDir, Fingerprints: r.fp, StepsTotal: len(r.spec.Steps), StepsApplied: r.progress.StepsApplied, ResumedAt: r.resumedAt, CursorMs: r.progress.CursorMs}
	if r.progress.CursorMs > 0 {
		out.Run.Cursor = time.UnixMilli(r.progress.CursorMs).UTC().Format(time.RFC3339Nano)
	}
	out.Cost = &basketReplayCostInfo{Applied: r.cost, DefaultParity: defaultFillCostPolicy(), SpreadModel: fillCostSpreadInSlippage, Path: "replay cost input passed into the shared paper open, add, partial close, full close, stop and kill-switch booking owners and the prospective margin gate; one booking and one cost application per confirmed fill"}
	storedLots := 0
	have := make(map[int]bool, len(r.stored))
	steps := make([]basketReplayStepRecord, 0, len(r.stored)+len(r.records))
	for _, st := range r.stored {
		steps = append(steps, st.Record)
		storedLots += st.LotRefusals
		have[st.Record.Index] = true
	}
	out.Steps = append(steps, r.records...)
	trades, err := r.storedTrades()
	if err != nil {
		return out, err
	}
	out.Trades = trades
	out.LotFetches = storedLots + r.lots.fetchRefusals()
	seen := map[string]bool{}
	var gaps []string
	for _, st := range out.Steps {
		for _, g := range st.Coverage {
			if !seen[g] {
				seen[g] = true
				gaps = append(gaps, g)
			}
		}
	}
	for i := 0; i < r.resumedAt; i++ {
		if !have[i] {
			gaps = append(gaps, fmt.Sprintf("step %d has no stored step record, so its coverage cannot be shown", i))
		}
	}
	for _, p := range r.pending {
		if len(p.Record.SaveErrors) > 0 {
			gaps = append(gaps, fmt.Sprintf("step %d: the state save failed and no later save made the step durable", p.Record.Index))
		} else {
			gaps = append(gaps, fmt.Sprintf("step %d: the step record was not written to the run state file", p.Record.Index))
		}
	}
	durable, err := r.file.readBasketReplayProgress(r.spec.Run.RunID)
	if err != nil {
		return out, err
	}
	if durable != r.progress {
		gaps = append(gaps, fmt.Sprintf("the run state file records %d applied steps, behind the %d steps this run applied", durable.StepsApplied, r.progress.StepsApplied))
	}
	gaps = append(gaps, r.fundingCoverageGaps()...)
	r.mu.RLock()
	for _, id := range r.bc.sortedIDs() {
		rt := r.bc.runtimes[id]
		if a := rt.pendingAction(); a != nil {
			gaps = append(gaps, fmt.Sprintf("basket %s action %s is %s at the end of the run", id, a.ActionID, a.Status))
		}
		if rt.needsDurable {
			gaps = append(gaps, fmt.Sprintf("basket %s settlement is not durable at the end of the run", id))
		}
	}
	r.mu.RUnlock()
	if out.LotFetches > 0 {
		gaps = append(gaps, fmt.Sprintf("%d lot metadata lookups named a coin absent from the frozen metadata", out.LotFetches))
	}
	sort.Strings(gaps)
	out.Coverage = &basketReplayCoverage{Complete: len(gaps) == 0, Gaps: gaps}
	switch {
	case r.progress.StepsApplied < len(r.spec.Steps):
		out.Status = basketReplayStatusStopped
	case len(gaps) > 0:
		out.Status = basketReplayStatusIncomplete
	default:
		out.Status = basketReplayStatusComplete
	}
	return out, nil
}

func basketReplayErrorOutput(err error) (basketReplayOutput, int) {
	out := basketReplayOutput{Protocol: basketReplayProtocolName, Version: basketReplayProtocolVersion}
	var refusal *basketReplayRefusal
	if errors.As(err, &refusal) {
		out.Status = basketReplayStatusRefused
		out.Refusal = refusal
		return out, basketReplayExitRefused
	}
	out.Status = basketReplayStatusError
	out.Error = err.Error()
	return out, basketReplayExitError
}
