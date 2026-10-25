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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pairReplayStageScenario           = "scenario"
	pairReplayStagePolicy             = "policy"
	pairReplayStageManagement         = "management"
	pairReplayKindProcess             = "process_failure"
	pairReplayKindNativeCheck         = "native_check_failure"
	pairReplayScenarioManagementCause = "scenario management_check_failure: "
	pairReplayScenarioPolicyCause     = "scenario policy_process_failure: "
	pairReplayMaxSameTime             = 3
	pairReplayManagementBars          = 500
)

type pairReplayManifest struct {
	Owner                string                   `json:"owner"`
	Version              int                      `json:"version"`
	RunID                string                   `json:"run_id"`
	Fingerprints         basketReplayFingerprints `json:"fingerprints"`
	EvidenceDigest       string                   `json:"evidence_digest"`
	FundingDigest        string                   `json:"funding_digest"`
	FundingBinding       json.RawMessage          `json:"funding_binding"`
	CostEvidence         json.RawMessage          `json:"cost_evidence"`
	MemberCosts          json.RawMessage          `json:"member_costs"`
	Objects              []pairReplayObjectRef    `json:"objects"`
	Lifetime             pairReplayLifetime       `json:"lifetime"`
	Scenario             pairReplayScenario       `json:"scenario"`
	EventOrder           string                   `json:"event_order"`
	AdministrativeFields []string                 `json:"administrative_fields"`
	UID                  int                      `json:"uid"`
	CreatedAt            string                   `json:"created_at"`
}

type pairReplayRuntime struct {
	spec      *pairReplaySpec
	lt        pairReplayLifetime
	opts      basketReplayOptions
	inputs    *pairReplayInputs
	fp        basketReplayFingerprints
	manifest  pairReplayManifest
	cfg       *Config
	cfgJSON   []byte
	adm       *pairResearchAdmission
	layout    storageLayout
	store     *StateStore
	file      *StateDB
	state     *AppState
	mu        *sync.RWMutex
	bc        *basketCoordinator
	rt        *basketRuntime
	clock     *simulatedClock
	notifier  *MultiNotifier
	quotes    *pairReplayQuoteSource
	market    *pairReplayMarket
	venues    map[string]*pairVirtualVenue
	evidence  *pairFrozenEvidenceSource
	funding   *pairFrozenFundingSource
	lots      *hlLotMetadataCache
	feedReq   feedRequirements
	intervals map[string]int
	members   []StrategyConfig
	partition RiskPartition
	interval  int64
	costs     map[string]fillCostPolicy

	cursor       pairReplayCursor
	nextAt       int64
	sameTime     int
	nextManageMs int64
	pending      pairReplayPendingRecord
	resumeStep   bool
	resumePhase  string
	resumedAt    int
	resumed      bool
	firstPrepare bool
	processSteps int
	finished     bool
	stopReason   string

	commitMu    sync.Mutex
	coordCommit *pairReplayPendingRecord
	finalCommit *pairReplayFinalCommit

	failMu   sync.Mutex
	failures []pairReplayFailure

	faultCount map[string]int
	equality   []string
	wakeTimes  []int64
	cleanups   []func()
}

func (r *pairReplayRuntime) Close() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}

type pairReplayResearchMember struct {
	StrategyID string `json:"strategy_id"`
	AccountKey string `json:"account_key"`
	Instrument string `json:"instrument"`
	Source     string `json:"source"`
	Meta       struct {
		SzDecimals int `json:"sz_decimals"`
	} `json:"meta"`
}

type pairReplayResearchShape struct {
	IntervalSeconds int `json:"interval_seconds"`
	Strategies      []struct {
		ID          string   `json:"id"`
		Platform    string   `json:"platform"`
		Args        []string `json:"args"`
		PaperSource string   `json:"paper_source"`
	} `json:"strategies"`
	Pairs []struct {
		Params  map[string]json.RawMessage `json:"params"`
		Members []pairReplayResearchMember `json:"members"`
	} `json:"pairs"`
}

func pairReplayConfigDocument(raw json.RawMessage, stateDir string) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var root map[string]interface{}
	if err := dec.Decode(&root); err != nil {
		return nil, refuseReplay("spec_invalid", "research_config: "+err.Error())
	}
	root["db_file"] = filepath.Join(stateDir, basketReplayDBName)
	root["log_dir"] = filepath.Join(stateDir, basketReplayLogDirName)
	root["market_feed"] = marketFeedWebsocket
	return json.MarshalIndent(root, "", "  ")
}

func pairReplayFaultMessage(kind string, n int) {
	fmt.Fprintf(os.Stderr, "fault %s:%d\n", kind, n)
}

func (r *pairReplayRuntime) fault(kind string) {
	if r.opts.Fault.Kind != kind {
		return
	}
	r.faultCount[kind]++
	if r.faultCount[kind] == r.opts.Fault.N {
		pairReplayFaultMessage(kind, r.opts.Fault.N)
		os.Exit(basketReplayExitFault)
	}
}

func openPairReplay(spec *pairReplaySpec, raw []byte, lt pairReplayLifetime, opts basketReplayOptions) (*pairReplayRuntime, error) {
	r := &pairReplayRuntime{spec: spec, lt: lt, opts: opts, mu: &sync.RWMutex{}, notifier: NewMultiNotifier(), clock: &simulatedClock{}, faultCount: map[string]int{}}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	stateDir := spec.Run.StateDir
	if err := basketReplayPathAllowed(stateDir); err != nil {
		return nil, refuseReplay("state_dir_refused", err.Error())
	}
	if _, err := basketReplayRealParent(stateDir); err != nil {
		return nil, refuseReplay("state_dir_refused", err.Error())
	}
	if spec.Mode == basketReplayModeNew {
		if _, err := os.Lstat(stateDir); err == nil {
			return nil, refuseReplay("state_exists", fmt.Sprintf("new-run mode refuses the existing path %q; a new run claims its own directory", stateDir))
		}
	} else if err := checkPairReplayResumeDir(stateDir); err != nil {
		return nil, err
	}
	inputFP, err := pairReplayInputFingerprint(raw)
	if err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	in, err := loadPairReplayInputs(spec)
	if err != nil {
		return nil, err
	}
	r.inputs = in
	cfgJSON, err := pairReplayConfigDocument(spec.ResearchConfig, stateDir)
	if err != nil {
		return nil, err
	}
	portable, err := pairReplayConfigDocument(spec.ResearchConfig, "/basket-replay-state")
	if err != nil {
		return nil, err
	}
	r.cfgJSON = cfgJSON
	var shape pairReplayResearchShape
	if err := json.Unmarshal(spec.ResearchConfig, &shape); err != nil || len(shape.Pairs) != 1 {
		return nil, refuseReplay("config_refused", "research_config needs exactly one pair")
	}
	if shape.IntervalSeconds <= 0 {
		return nil, refuseReplay("config_refused", "research_config needs a positive interval_seconds: it is the member management cadence")
	}
	r.interval = int64(shape.IntervalSeconds) * 1000
	r.fp = basketReplayFingerprints{Protocol: fmt.Sprintf("%s/%d", basketReplayProtocolName, pairReplayProtocolVersion), Source: basketReplaySourceFingerprint(spec.Run.Source), Input: inputFP, Config: sha256Hex(portable)}
	runBlob, _ := json.Marshal([]string{r.fp.Protocol, spec.Run.RunID, r.fp.Source, r.fp.Input, r.fp.Config})
	r.fp.Run = sha256Hex(runBlob)

	evidence, err := newPairFrozenEvidenceSource(in.evidence, sha256Hex(in.evidence))
	if err != nil {
		return nil, refuseReplay("input_refused", "evidence: "+err.Error())
	}
	funding, err := newPairFrozenFundingSource(in.funding, sha256Hex(in.funding))
	if err != nil {
		return nil, refuseReplay("input_refused", "funding: "+err.Error())
	}
	r.evidence, r.funding = evidence, funding
	sources := map[string]string{}
	pair := shape.Pairs[0]
	szDecimals := map[string]int{}
	for _, m := range pair.Members {
		sources[m.StrategyID] = m.Source
	}
	r.quotes = newPairReplayQuoteSource(spec.Run.RunID, sources, in)
	r.market = &pairReplayMarket{src: r.quotes, clock: r.clock.Now}
	band := 0.0
	if b, ok := pair.Params["adverse_band_bps"]; ok {
		if err := json.Unmarshal(b, &band); err != nil {
			return nil, refuseReplay("config_refused", "pairs[0].params.adverse_band_bps is not a number")
		}
	}
	r.venues = map[string]*pairVirtualVenue{}
	for _, m := range pair.Members {
		part := defaultPaperPartition
		provider := ""
		for _, sc := range shape.Strategies {
			if sc.ID != m.StrategyID {
				continue
			}
			provider = pairProviderPlatforms[sc.Platform]
			if sc.PaperSource != "" {
				part = RiskPartition{Scope: ScopePaper, Source: sc.PaperSource}
			}
			for _, a := range sc.Args {
				if a == "--mode=live" {
					part = RiskPartition{Scope: ScopeLive}
				}
			}
			if provider == entryProviderHyperliquid {
				szDecimals[m.Instrument] = m.Meta.SzDecimals
			}
		}
		v, err := newPairVirtualVenue(pairVirtualVenueConfig{Partition: part, RunTag: spec.Run.RunID, MemberID: m.StrategyID, Provider: provider, AccountKey: m.AccountKey,
			Instrument: m.Instrument, Market: r.market, AdverseBandBps: band, SubmitLatencyMs: spec.Scenario.SubmitLatencyMs, ResponseLatencyMs: spec.Scenario.ResponseLatencyMs})
		if err != nil {
			return nil, refuseReplay("config_refused", fmt.Sprintf("member %s virtual venue: %v", m.StrategyID, err))
		}
		r.venues[m.StrategyID] = v
	}
	venueFor := func(sc StrategyConfig) (basketVenue, error) {
		if v := r.venues[sc.ID]; v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("no virtual venue for %s", sc.ID)
	}
	r.manifest = pairReplayManifest{Owner: basketReplayOwnerMarker, Version: pairReplayManifestVersion, RunID: spec.Run.RunID, Fingerprints: r.fp,
		EvidenceDigest: evidence.Digest(), FundingDigest: sha256Hex(in.funding), Objects: pairReplayObjectsWithTotals(in), Lifetime: lt, Scenario: spec.Scenario,
		EventOrder: pairReplayEventOrder, AdministrativeFields: pairReplayAdministrativeFields, UID: os.Getuid()}
	var saved pairReplayManifest
	if spec.Mode == basketReplayModeResume {
		saved, err = readPairReplayManifest(stateDir)
		if err != nil {
			return nil, err
		}
		if err := comparePairReplayManifest(saved, r.manifest, false); err != nil {
			return nil, err
		}
	}
	start := lt.StartMs
	if err := r.clock.Set(time.UnixMilli(start).UTC()); err != nil {
		return nil, err
	}
	if err := bindTradingClock(basketReplayClockOwner, r.clock.Now); err != nil {
		return nil, err
	}
	r.cleanups = append(r.cleanups, func() { unbindTradingClock(basketReplayClockOwner) })
	r.lots = newFrozenHLLotMetadataCache(szDecimals, func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) })
	prevLots := hlLotMetadata
	hlLotMetadata = r.lots
	r.cleanups = append(r.cleanups, func() { hlLotMetadata = prevLots })
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	r.cleanups = append(r.cleanups, func() { tradeRecorder = prevRecorder })
	prevFundingClock := paperFundingClock
	paperFundingClock = func() time.Time { return tradingNow().UTC() }
	r.cleanups = append(r.cleanups, func() { paperFundingClock = prevFundingClock })

	adm, cfg, err := newPairResearchAdmission(cfgJSON, spec.Run.RunID, stateDir, spec.Mode == basketReplayModeResume, pairResearchDeps{VenueFor: venueFor, Evidence: evidence, Quotes: r.quotes, Funding: funding})
	if err != nil {
		return nil, refuseReplay("admission_refused", err.Error())
	}
	r.cleanups = append(r.cleanups, adm.release)
	r.adm, r.cfg = adm, cfg
	r.fp.Contract = adm.contract
	r.manifest.Fingerprints.Contract = adm.contract
	r.manifest.FundingBinding = json.RawMessage(adm.fundingBinding)
	if spec.Mode == basketReplayModeResume {
		if err := comparePairReplayManifest(saved, r.manifest, true); err != nil {
			return nil, err
		}
		r.manifest.CostEvidence, r.manifest.MemberCosts, r.manifest.CreatedAt = saved.CostEvidence, saved.MemberCosts, saved.CreatedAt
		cur, err := verifyPairReplayDBReadOnly(filepath.Join(stateDir, basketReplayDBName), r.manifest)
		if err != nil {
			return nil, err
		}
		r.cursor = cur
	} else if err := os.Mkdir(filepath.Join(stateDir, basketReplayLogDirName), 0o700); err != nil {
		return nil, err
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
	r.cleanups = append(r.cleanups, func() { owned.Release() })
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		return nil, err
	}
	r.store, r.layout = store, layout
	r.cleanups = append(r.cleanups, func() { store.Close() })
	for _, sc := range cfg.Strategies {
		if isPairMember(sc) {
			r.members = append(r.members, sc)
		}
	}
	sort.Slice(r.members, func(i, j int) bool { return r.members[i].ID < r.members[j].ID })
	if len(r.members) != 2 {
		return nil, refuseReplay("config_refused", "the replay runs exactly two pair members")
	}
	r.partition = partitionFor(r.members[0])
	r.file = store.file(layout.roleForPartition(r.partition))
	if r.file == nil {
		return nil, fmt.Errorf("no state file owns the %s partition", partitionLabel(r.partition))
	}
	if err := r.file.ensureBasketReplaySchema(); err != nil {
		return nil, err
	}
	if spec.Mode == basketReplayModeNew {
		if err := r.file.insertPairReplayRun(r.manifest, r.manifest.Objects, in.recordTotal); err != nil {
			return nil, err
		}
	} else {
		cur, err := r.file.readPairReplayCursor(spec.Run.RunID)
		if err != nil {
			return nil, err
		}
		if cur != r.cursor {
			return nil, refuseReplay("resume_refused", "the run cursor changed while the adapter opened the state file")
		}
	}
	if err := r.loadState(); err != nil {
		return nil, err
	}
	if spec.Mode == basketReplayModeNew {
		if err := r.startupCommit(); err != nil {
			return nil, err
		}
	} else if err := r.resumeState(); err != nil {
		return nil, err
	}
	store.saveExtra = r.saveExtra
	ok = true
	return r, nil
}

func pairReplayObjectsWithTotals(in *pairReplayInputs) []pairReplayObjectRef {
	return append([]pairReplayObjectRef(nil), in.objects...)
}

var pairReplayResumeEntries = map[string]bool{
	pairResearchManifestName: true,
}

func checkPairReplayResumeDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return refuseReplay("state_missing", fmt.Sprintf("resume mode needs the replay-created state directory %q: %v", dir, err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return refuseReplay("state_foreign", fmt.Sprintf("%q is not a plain directory", dir))
	}
	if !basketReplayOwnedByMe(info) {
		return refuseReplay("state_foreign", fmt.Sprintf("%q is owned by another user", dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	var extra []string
	haveManifest := false
	for _, e := range entries {
		if !basketReplayAllowedEntries[e.Name()] && !pairReplayResumeEntries[e.Name()] {
			extra = append(extra, e.Name())
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			extra = append(extra, e.Name()+" (symbolic link)")
		}
		if e.Name() == basketReplayManifestName {
			haveManifest = true
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return refuseReplay("state_foreign", fmt.Sprintf("%q holds entries the replay never creates: %s", dir, strings.Join(extra, ", ")))
	}
	if !haveManifest {
		return refuseReplay("resume_refused", fmt.Sprintf("%q has no replay manifest: its initialization never finished (the manifest is written only after the startup bindings commit), so it is not a resumable run", dir))
	}
	return nil
}

func readPairReplayManifest(dir string) (pairReplayManifest, error) {
	var m pairReplayManifest
	path := filepath.Join(dir, basketReplayManifestName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !basketReplayOwnedByMe(info) {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the replay manifest %q is not a regular file this user owns", path))
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return m, refuseReplay("state_foreign", err.Error())
	}
	if err := pairReplayStrictDecode(blob, &m); err != nil {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the replay manifest is not a version-2 pair manifest: %v", err))
	}
	if m.Owner != basketReplayOwnerMarker || m.Version != pairReplayManifestVersion {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the replay manifest owner %q version %d is not this adapter's version-2 pair manifest", m.Owner, m.Version))
	}
	return m, nil
}

func comparePairReplayManifest(saved, want pairReplayManifest, withContract bool) error {
	var diffs []string
	check := func(name string, a, b string) {
		if a != b {
			diffs = append(diffs, name+" changed")
		}
	}
	check("run id", saved.RunID, want.RunID)
	check("protocol", saved.Fingerprints.Protocol, want.Fingerprints.Protocol)
	check("source (adapter version, source commit and provenance)", saved.Fingerprints.Source, want.Fingerprints.Source)
	check("input", saved.Fingerprints.Input, want.Fingerprints.Input)
	check("config", saved.Fingerprints.Config, want.Fingerprints.Config)
	check("run", saved.Fingerprints.Run, want.Fingerprints.Run)
	check("evidence digest", saved.EvidenceDigest, want.EvidenceDigest)
	check("funding digest", saved.FundingDigest, want.FundingDigest)
	check("input objects", pairReplayObjectSet(saved.Objects), pairReplayObjectSet(want.Objects))
	check("lifetime", saved.Lifetime.String(), want.Lifetime.String())
	sa, _ := json.Marshal(saved.Scenario)
	wa, _ := json.Marshal(want.Scenario)
	check("scenario", string(sa), string(wa))
	check("event order", saved.EventOrder, want.EventOrder)
	check("administrative field list", strings.Join(saved.AdministrativeFields, "\n"), strings.Join(want.AdministrativeFields, "\n"))
	if saved.UID != want.UID {
		diffs = append(diffs, "the run was created by another user")
	}
	if withContract {
		check("contract", saved.Fingerprints.Contract, want.Fingerprints.Contract)
		if err := pairFundingCompareBinding(want.FundingBinding, saved.FundingBinding); err != nil {
			diffs = append(diffs, "funding binding: "+err.Error())
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		return refuseReplay("resume_refused", diffs...)
	}
	return nil
}

func (r *pairReplayRuntime) loadState() error {
	cfg, store := r.cfg, r.store
	state, orphans, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return err
	}
	if len(orphans) > 0 {
		return refuseReplay("state_foreign", fmt.Sprintf("the run state file holds %d strategy rows the run does not configure", len(orphans)))
	}
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
	if err := store.LoadEntryGuards(state, cfg.Strategies, persisted); err != nil {
		return err
	}
	runtimes, err := store.LoadBaskets(state, cfg)
	if err != nil {
		return refuseReplay("state_refused", err.Error())
	}
	ValidateStateWithPairOwner(state, cfg.Strategies, r.adm)
	r.state = state
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		return err
	}
	r.feedReq = feedReq
	r.intervals = effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(cfg))
	bc := newBasketCoordinator(cfg, state, store, r.mu, r.notifier, nil, runtimes)
	bc.parent = context.Background()
	bc.lots = r.lots
	bc.venueFor = func(sc StrategyConfig) (basketVenue, error) {
		if v := r.venues[sc.ID]; v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("strategy %s has no virtual pair venue", sc.ID)
	}
	bc.pairPolicyRun = r.policyRunner
	bc.pairClock = r.clock.Now
	bc.pairReplay = &pairReplayHooks{collected: r.onCollected, frozen: r.onFrozen, beforeSubmit: r.beforeSubmit, submitted: r.onSubmitted}
	ids := bc.sortedPairIDs()
	if len(ids) != 1 {
		return refuseReplay("state_refused", fmt.Sprintf("the replay needs exactly one pair runtime, got %d", len(ids)))
	}
	r.bc, r.rt = bc, bc.runtimes[ids[0]]
	if err := bc.attachPairVirtualVenues(); err != nil {
		return refuseReplay("state_refused", err.Error())
	}
	if err := bc.startupCheck(); err != nil {
		return refuseReplay("state_refused", err.Error())
	}
	failures, err := r.file.readPairReplayFailures(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	r.failures = failures
	return nil
}

func (r *pairReplayRuntime) checkedCosts() (map[string]fillCostPolicy, string, error) {
	costs := pairCostBindings(r.adm)
	if len(costs) != len(r.rt.Contract.pairMembersSorted()) {
		return nil, "", refuseReplay("cost_refused", fmt.Sprintf("the durable member cost bindings are absent or incomplete (%d of %d members)", len(costs), len(r.rt.Contract.pairMembersSorted())))
	}
	for _, m := range r.rt.Contract.pairMembersSorted() {
		p, ok := costs[m.MemberID]
		want := m.CostPolicy.rates()
		switch {
		case !ok:
			return nil, "", refuseReplay("cost_refused", fmt.Sprintf("member %s has no durable cost binding", m.MemberID))
		case p.ModelVersion != FillCostModelReplayCustomVersion || !strings.HasPrefix(p.PolicyID, pairCostPolicyIDPrefix) || len(p.PolicyID) != len(pairCostPolicyIDPrefix)+64:
			return nil, "", refuseReplay("cost_refused", fmt.Sprintf("member %s binding %q version %d is not a full pair cost binding of model version %d", m.MemberID, p.PolicyID, p.ModelVersion, FillCostModelReplayCustomVersion))
		case p.TakerFeePct != want.TakerFeePct || p.SlippagePct != want.SlippagePct || p.FillRole != want.FillRole || p.Spread != want.Spread:
			return nil, "", refuseReplay("cost_refused", fmt.Sprintf("member %s binding rates differ from the contract member cost policy", m.MemberID))
		}
	}
	blob, err := json.Marshal(costs)
	if err != nil {
		return nil, "", err
	}
	return costs, string(blob), nil
}

func (r *pairReplayRuntime) startupCommit() error {
	r.mu.Lock()
	err := r.store.SavePartition(r.state, r.partition)
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("the startup commit of books, contract, cost evidence and funding binding failed: %w", err)
	}
	costs, blob, err := r.checkedCosts()
	if err != nil {
		return err
	}
	r.costs = costs
	if err := r.file.setPairReplayMemberCosts(r.spec.Run.RunID, blob); err != nil {
		return err
	}
	r.mu.RLock()
	r.manifest.CostEvidence = append(json.RawMessage(nil), r.rt.Pair.Progress.CostEvidence...)
	r.mu.RUnlock()
	r.manifest.MemberCosts = json.RawMessage(blob)
	r.manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	mb, err := json.MarshalIndent(r.manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := pairWriteClaimFile(r.spec.Run.StateDir, filepath.Join(r.spec.Run.StateDir, basketReplayManifestName), append(mb, '\n')); err != nil {
		return err
	}
	cur, err := r.file.readPairReplayCursor(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	r.cursor = cur
	r.nextAt, r.nextManageMs = r.lt.StartMs, r.lt.StartMs
	return nil
}

func (r *pairReplayRuntime) resumeState() error {
	costs, blob, err := r.checkedCosts()
	if err != nil {
		return err
	}
	if blob != pairReplayCompactJSON(r.manifest.MemberCosts) {
		return refuseReplay("resume_refused", "the loaded member cost bindings differ from the frozen replay manifest")
	}
	r.costs = costs
	r.mu.RLock()
	saved := string(r.rt.Pair.Progress.CostEvidence)
	r.mu.RUnlock()
	if pairReplayCompactJSON([]byte(saved)) != pairReplayCompactJSON(r.manifest.CostEvidence) {
		return refuseReplay("resume_refused", "the saved pair cost evidence differs from the frozen replay manifest")
	}
	if why := r.verifyTradeStamps(); why != "" {
		return refuseReplay("resume_refused", why)
	}
	if n, err := r.virtualEventCount(); err != nil {
		return err
	} else if n < r.cursor.BindingSeq {
		return refuseReplay("resume_refused", fmt.Sprintf("the virtual venue ledger holds %d records, fewer than the %d the cursor committed", n, r.cursor.BindingSeq))
	}
	steps, err := r.file.readPairReplaySteps(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	r.resumed, r.resumedAt, r.firstPrepare = true, r.cursor.Applied, true
	var last *pairReplayStepRecord
	var pending *pairReplayStoredStep
	for i := range steps {
		s := steps[i]
		switch {
		case s.Status == pairReplayStatusComplete && s.Ordinal < r.cursor.Applied:
			var rec pairReplayStepRecord
			if err := pairReplayStrictDecode(s.Record, &rec); err != nil {
				return refuseReplay("state_foreign", fmt.Sprintf("the stored record of step %d is unreadable: %v", s.Ordinal, err))
			}
			if sha256Hex(s.Record) != s.ResultHash {
				return refuseReplay("state_foreign", fmt.Sprintf("the stored record of step %d does not match its result hash", s.Ordinal))
			}
			last = &rec
		case s.Status == pairReplayStatusPending && s.Ordinal == r.cursor.Applied && s.StepID == r.cursor.PendingID:
			pending = &steps[i]
		default:
			return refuseReplay("state_foreign", fmt.Sprintf("stored step %d (%s, %s) does not fit the cursor at %d", s.Ordinal, s.Status, s.StepID, r.cursor.Applied))
		}
	}
	if r.cursor.Applied > 0 && last == nil {
		return refuseReplay("state_foreign", "the cursor has applied steps but no stored step record")
	}
	if r.cursor.PendingID != "" && pending == nil {
		return refuseReplay("state_foreign", fmt.Sprintf("the cursor names pending step %s, which has no stored record", r.cursor.PendingID))
	}
	if last != nil {
		r.restoreHost(last.Host)
		r.nextAt, r.nextManageMs, r.sameTime = last.NextWakeMs, last.Host.NextManageMs, last.SameTime
	} else {
		r.nextAt, r.nextManageMs = r.lt.StartMs, r.lt.StartMs
	}
	nowMs := r.nextAt
	if pending != nil {
		var rec pairReplayPendingRecord
		if err := pairReplayStrictDecode(pending.Record, &rec); err != nil {
			return refuseReplay("state_foreign", fmt.Sprintf("the pending record of step %d is unreadable: %v", pending.Ordinal, err))
		}
		if rec.PrefixHash != r.prefixHash(rec.AtMs) || rec.PrefixHash != r.cursor.PendingHash || rec.AtMs != r.cursor.PendingMs {
			return refuseReplay("resume_refused", fmt.Sprintf("the pending step %d visible-prefix binding does not match the frozen inputs", pending.Ordinal))
		}
		r.pending, r.resumeStep, r.resumePhase = rec, true, pending.Phase
		r.nextAt, nowMs = rec.AtMs, rec.AtMs
		if err := r.clock.Set(time.UnixMilli(nowMs).UTC()); err != nil {
			return err
		}
		if rec.Collection != nil {
			if err := r.completeCollection(*rec.Collection); err != nil {
				return err
			}
		}
	} else if r.cursor.CursorMs > 0 {
		if err := r.clock.Set(time.UnixMilli(r.cursor.CursorMs).UTC()); err != nil {
			return err
		}
	}
	r.bc.recoverInterrupted(time.UnixMilli(nowMs).UTC())
	return nil
}

func (r *pairReplayRuntime) completeCollection(c pairReplayCollection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt := r.rt
	rb := rt.Rebalances[c.SlotMs]
	if c.RunID != r.spec.Run.RunID || c.PairID != rt.Contract.BasketID || c.ContractFP != rt.Contract.Fingerprint() {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d belongs to run %s pair %s contract %s", c.SlotMs, c.RunID, c.PairID, shortFingerprint(c.ContractFP)))
	}
	if strings.Join(c.ObjectHashes, ",") != strings.Join(r.objectHashes(), ",") {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d was read from other input objects", c.SlotMs))
	}
	if rb == nil {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection names slot %d, which the state file never reserved", c.SlotMs))
	}
	if rb.Status != basketRebalanceReserved {
		if rb.InputFP == c.InputFP && rb.EvidenceJSON == c.EvidenceJSON {
			return nil
		}
		return refuseReplay("resume_refused", fmt.Sprintf("slot %d is %s with other evidence than the saved collection; frozen evidence is never replaced", c.SlotMs, rb.Status))
	}
	limits := rt.Pair.Limits
	if deadline, ok := pairAddMs(c.StartMs, limits.CollectionTimeoutMs); !ok || deadline != c.DeadlineMs || c.SlotMs != c.StartMs-c.StartMs%limits.DecisionCadenceMs || c.CutoffMs < c.StartMs || !pairBeforeDeadline(c.CutoffMs, c.DeadlineMs) {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d has inconsistent original times (start %d, cutoff %d, deadline %d)", c.SlotMs, c.StartMs, c.CutoffMs, c.DeadlineMs))
	}
	probe := *rb
	probe.CutoffMs, probe.InputFP, probe.EvidenceJSON, probe.SnapshotID, probe.Generation = c.CutoffMs, c.InputFP, c.EvidenceJSON, c.SnapshotID, c.Generation
	env, why := verifyPairEnvelope(rt, &probe)
	if why != "" {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d fails envelope verification: %s", c.SlotMs, why))
	}
	if env.StartMs != c.StartMs || env.DeadlineMs != c.DeadlineMs || env.CutoffMs != c.CutoffMs || env.SnapshotID != c.SnapshotID {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d does not carry its own original times and seal", c.SlotMs))
	}
	if err := r.bc.recheckPairSlot(rt, c.SlotMs, c.ContractFP); err != nil {
		return refuseReplay("resume_refused", fmt.Sprintf("the reserved slot %d failed its recheck: %v", c.SlotMs, err))
	}
	if err := r.bc.freezePairEnvelope(rt, c.SlotMs, pairEnvelope{CutoffMs: c.CutoffMs, InputFP: c.InputFP, EvidenceJSON: c.EvidenceJSON, SnapshotID: c.SnapshotID, Generation: c.Generation}, r.clock.Now()); err != nil {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved collection of slot %d could not be frozen: %v", c.SlotMs, err))
	}
	r.bc.pairLogf(rt, "slot %s: the saved checked collection completed the reserved slot after a restart (input %s)", formatClosedBarMs(c.SlotMs), shortFingerprint(c.InputFP))
	return nil
}

func (r *pairReplayRuntime) objectHashes() []string {
	out := make([]string, 0, len(r.inputs.objects))
	for _, o := range r.inputs.objects {
		out = append(out, o.SHA256)
	}
	sort.Strings(out)
	return out
}

func (r *pairReplayRuntime) prefixHash(atMs int64) string {
	var parts []string
	parts = append(parts, r.fp.Input, strconv.FormatInt(atMs, 10))
	for _, o := range r.inputs.objects {
		n := 0
		switch o.Kind {
		case pairReplayKindQuotes:
			for _, rec := range r.inputs.quotes[o.MemberID].Records {
				if rec.ReceiptMs <= atMs {
					n++
				}
			}
		case pairReplayKindBooks:
			for _, rec := range r.inputs.books[o.MemberID].Records {
				if rec.RecvMs <= atMs {
					n++
				}
			}
		case pairReplayKindCandles:
			for _, b := range r.inputs.candles[o.MemberID] {
				if b.OpenMs <= atMs {
					n++
				}
			}
		default:
			n = -1
		}
		parts = append(parts, fmt.Sprintf("%s:%d", o.SHA256, n))
	}
	return sha256Hex([]byte(strings.Join(parts, "|")))
}

func (r *pairReplayRuntime) hostStateLocked() pairReplayHostState {
	rt := r.rt
	h := pairReplayHostState{Cycle: r.bc.cycle, NextManageMs: r.nextManageMs, LastStepMs: rt.pairLastStepMs, ViewGen: rt.pairViewGen, PrepSeq: rt.pairPrepSeq,
		Observations: map[string]pairReplayObservation{}, EchoFailures: map[string]string{}}
	for id, o := range rt.observations {
		h.Observations[id] = pairReplayObservation{Cycle: o.Cycle, AtMs: o.AtMs, Fields: o.Fields, Indicators: o.Indicators, Price: o.Price}
	}
	for id, why := range rt.pairEchoFailures {
		h.EchoFailures[id] = why
	}
	return h
}

func (r *pairReplayRuntime) hostState() pairReplayHostState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := r.hostStateLocked()
	blob, _ := json.Marshal(h)
	var out pairReplayHostState
	_ = json.Unmarshal(blob, &out)
	return out
}

func (r *pairReplayRuntime) restoreHost(h pairReplayHostState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt := r.rt
	r.bc.cycle = h.Cycle
	r.nextManageMs = h.NextManageMs
	rt.pairLastStepMs = h.LastStepMs
	rt.pairViewGen, rt.pairPrepSeq = h.ViewGen, h.PrepSeq
	rt.observations = map[string]basketMemberObservation{}
	for id, o := range h.Observations {
		rt.observations[id] = basketMemberObservation{Cycle: o.Cycle, AtMs: o.AtMs, Fields: o.Fields, Indicators: o.Indicators, Price: o.Price}
	}
	rt.pairEchoFailures = map[string]string{}
	for id, why := range h.EchoFailures {
		rt.pairEchoFailures[id] = why
	}
}

func (r *pairReplayRuntime) saveExtra(tx *sql.Tx, role storageRole) error {
	if role != r.file.role {
		return nil
	}
	r.commitMu.Lock()
	coord, final := r.coordCommit, r.finalCommit
	r.commitMu.Unlock()
	switch {
	case final != nil:
		return writePairReplayFinalTx(tx, r.spec.Run.RunID, *final)
	case coord != nil:
		return writePairReplayCoordinatorTx(tx, r.spec.Run.RunID, *coord)
	}
	return nil
}

func (r *pairReplayRuntime) recordFailure(f pairReplayFailure) {
	r.failMu.Lock()
	defer r.failMu.Unlock()
	f.Ordinal, f.StepID = r.pending.Ordinal, r.pending.StepID
	if f.AtMs == 0 {
		f.AtMs = r.clock.Now().UnixMilli()
	}
	if err := r.file.insertPairReplayFailure(r.spec.Run.RunID, f); err != nil {
		fmt.Fprintf(os.Stderr, "[basket-replay] CRITICAL: could not save the %s failure evidence (%s): %v; the run stops incomplete\n", f.Stage, f.Cause, err)
		r.stopReason = "failure evidence could not be saved: " + err.Error()
	}
	r.failures = append(r.failures, f)
}

func (r *pairReplayRuntime) hasFailure(stage, kind string, match func(f pairReplayFailure) bool) bool {
	r.failMu.Lock()
	defer r.failMu.Unlock()
	for _, f := range r.failures {
		if f.Stage == stage && f.Kind == kind && match(f) {
			return true
		}
	}
	return false
}

func (r *pairReplayRuntime) scenario(kind string, match func(f pairReplayFailureScript) bool) bool {
	for _, f := range r.spec.Scenario.VenueFailures {
		if f.Kind == kind && match(f) {
			return true
		}
	}
	return false
}

func pairReplayScenarioSubmitCause(purpose string, nth int) string {
	return fmt.Sprintf("scenario transport failure of %s #%d before the venue received action ", purpose, nth)
}

func (r *pairReplayRuntime) scenarioApplied(s pairReplayFailureScript) bool {
	switch s.Kind {
	case pairReplayFailSubmit:
		prefix := pairReplayScenarioSubmitCause(s.Purpose, s.Nth)
		return r.hasFailure(pairReplayStageScenario, pairReplayFailSubmit, func(f pairReplayFailure) bool { return strings.HasPrefix(f.Cause, prefix) })
	case pairReplayFailPolicy:
		return r.hasFailure(pairReplayStagePolicy, pairReplayKindProcess, func(f pairReplayFailure) bool {
			return f.AtMs == s.atMs && strings.HasPrefix(f.Cause, pairReplayScenarioPolicyCause)
		})
	case pairReplayFailManagement:
		return r.hasFailure(pairReplayStageManagement, pairReplayKindNativeCheck, func(f pairReplayFailure) bool {
			return f.MemberID == s.MemberID && f.AtMs == s.atMs && strings.HasPrefix(f.Cause, pairReplayScenarioManagementCause)
		})
	case pairReplayFailSave:
		return r.hasFailure(pairReplayStageScenario, pairReplayFailSave, func(f pairReplayFailure) bool { return f.AtMs == s.atMs })
	}
	return false
}

func (r *pairReplayRuntime) policyRunner(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	var head struct {
		SlotMs int64 `json:"slot_ms"`
	}
	_ = json.Unmarshal(stdin, &head)
	nowMs := r.clock.Now().UnixMilli()
	if r.scenario(pairReplayFailPolicy, func(f pairReplayFailureScript) bool { return f.atMs == nowMs }) &&
		!r.hasFailure(pairReplayStagePolicy, pairReplayKindProcess, func(f pairReplayFailure) bool { return f.SlotMs == head.SlotMs }) {
		err := fmt.Errorf("%sthe policy process was not launched", pairReplayScenarioPolicyCause)
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, AtMs: nowMs, Cause: err.Error()})
		return nil, nil, err
	}
	out, errb, err := runPairPolicyProcess(ctx, stdin, timeout)
	switch {
	case err != nil:
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the canonical policy runner failed: " + err.Error()})
	case ctx.Err() != nil:
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the policy context ended: " + ctx.Err().Error()})
	default:
		if _, derr := pairDecodePolicyResult(out); derr != nil {
			r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the policy process output is unreadable: " + derr.Error()})
		}
	}
	return out, errb, err
}

func (r *pairReplayRuntime) onCollected(rt *basketRuntime, c pairCollectedSlot) error {
	rec := r.pending
	rec.Collection = &pairReplayCollection{RunID: r.spec.Run.RunID, PairID: rt.Contract.BasketID, ContractFP: rt.Contract.Fingerprint(), SlotMs: c.SlotMs, StartMs: c.StartMs,
		DeadlineMs: c.DeadlineMs, CutoffMs: c.Envelope.CutoffMs, InputFP: c.Envelope.InputFP, EvidenceJSON: c.Envelope.EvidenceJSON, SnapshotID: c.Envelope.SnapshotID,
		Generation: c.Envelope.Generation, ObjectHashes: r.objectHashes()}
	if err := r.file.updatePairReplayPending(r.spec.Run.RunID, rec); err != nil {
		return err
	}
	r.pending = rec
	r.fault("exit-after-collection")
	return nil
}

func (r *pairReplayRuntime) onFrozen(rt *basketRuntime, slotMs int64) {
	r.fault("exit-after-envelope-freeze")
}

func (r *pairReplayRuntime) beforeSubmit(a basketAction) error {
	if len(r.spec.Scenario.VenueFailures) == 0 {
		return nil
	}
	r.mu.RLock()
	nth := 0
	for _, b := range r.rt.Actions {
		if b.Purpose == a.Purpose && (b.CreatedAtMs < a.CreatedAtMs || (b.CreatedAtMs == a.CreatedAtMs && b.ActionID <= a.ActionID)) {
			nth++
		}
	}
	r.mu.RUnlock()
	if !r.scenario(pairReplayFailSubmit, func(f pairReplayFailureScript) bool { return f.Purpose == a.Purpose && f.Nth == nth }) {
		return nil
	}
	if r.hasFailure(pairReplayStageScenario, pairReplayFailSubmit, func(f pairReplayFailure) bool { return strings.HasSuffix(f.Cause, " "+a.ActionID) }) {
		return nil
	}
	r.recordFailure(pairReplayFailure{Stage: pairReplayStageScenario, Kind: pairReplayFailSubmit, MemberID: a.StrategyID, Cause: pairReplayScenarioSubmitCause(a.Purpose, nth) + a.ActionID})
	return fmt.Errorf("scenario submit_transport_error: the request never reached the virtual venue")
}

func (r *pairReplayRuntime) onSubmitted(a basketAction, out basketVenueOutcome, err error) {
	if err == nil && out.AcceptedAtMs > 0 {
		r.fault("exit-after-acceptance")
	}
}

func (r *pairReplayRuntime) virtualEventCount() (int64, error) {
	var n int64
	ok, err := tableExists(r.file, "pair_virtual_events")
	if err != nil || !ok {
		return 0, err
	}
	err = r.file.db.QueryRow(`SELECT COUNT(*) FROM pair_virtual_events`).Scan(&n)
	return n, err
}

func (r *pairReplayRuntime) verifyTradeStamps() string {
	process := activeFillCostPolicy()
	var bad []string
	for _, sc := range r.members {
		want := r.costs[sc.ID]
		rows, err := r.memberTrades(sc.ID)
		if err != nil {
			return err.Error()
		}
		for _, t := range rows {
			if t.TradeType == TradeTypeFunding {
				if t.CostModelVersion != process.ModelVersion || t.CostPolicyID != process.PolicyID {
					bad = append(bad, fmt.Sprintf("%s funding row %s carries stamp %d/%q, not the process stamp", sc.ID, t.ExchangeOrderID, t.CostModelVersion, t.CostPolicyID))
				}
				continue
			}
			if t.CostModelVersion != want.ModelVersion || t.CostPolicyID != want.PolicyID {
				bad = append(bad, fmt.Sprintf("%s trade at %s carries stamp %d/%q, not its member binding", sc.ID, t.Timestamp.UTC().Format(time.RFC3339Nano), t.CostModelVersion, t.CostPolicyID))
			}
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, "; ")
}

func (r *pairReplayRuntime) memberTrades(id string) ([]Trade, error) {
	var rows []Trade
	for {
		page, total, err := r.store.QueryTradeHistory(id, "", time.Time{}, time.Time{}, basketReplayTradePage, len(rows))
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		if len(rows) >= total || len(page) == 0 {
			if len(rows) != total {
				return nil, fmt.Errorf("strategy %s: read %d of %d stored trade rows", id, len(rows), total)
			}
			break
		}
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

func (r *pairReplayRuntime) Run() error {
	for {
		if r.stopReason != "" {
			return nil
		}
		if r.nextAt > r.lt.EndMs {
			r.finished = true
			return nil
		}
		if r.cursor.Applied >= r.lt.MaxSteps {
			r.stopReason = fmt.Sprintf("the step budget of %d steps ran out before the declared end", r.lt.MaxSteps)
			return nil
		}
		if r.opts.MaxSteps > 0 && r.processSteps >= r.opts.MaxSteps {
			r.stopReason = fmt.Sprintf("--max-steps %d stopped the run; it stays resumable", r.opts.MaxSteps)
			return nil
		}
		if err := r.runStep(r.nextAt); err != nil {
			return err
		}
		r.processSteps++
	}
}

func (r *pairReplayRuntime) cycleInputs(at time.Time) basketCycleInputs {
	return basketCycleInputs{Now: at, Once: true, PairSnapshotFor: r.quotes.seal, PairGeneration: r.quotes.generation}
}

func (r *pairReplayRuntime) checkResumedView(at time.Time) {
	if !r.firstPrepare {
		return
	}
	r.firstPrepare = false
	views := r.bc.pairViews(at)
	var bad []string
	for _, id := range r.rt.memberIDs() {
		if v, ok := views[id]; !ok || !v.MarkValid {
			bad = append(bad, id)
		}
	}
	if len(bad) > 0 {
		r.equality = append(r.equality, fmt.Sprintf("the first valuation view after the resume at %d had no valid mark for %s, so the restarted risk inputs cannot equal an uninterrupted run", at.UnixMilli(), strings.Join(bad, ", ")))
	}
}

func (r *pairReplayRuntime) runStep(atMs int64) error {
	at := time.UnixMilli(atMs).UTC()
	if err := r.clock.Set(at); err != nil {
		return refuseReplay("step_before_cursor", err.Error())
	}
	ordinal := r.cursor.Applied
	stepID := fmt.Sprintf("s%d@%d", ordinal, atMs)
	resumePhase := ""
	if r.resumeStep {
		if r.pending.StepID != stepID {
			return refuseReplay("resume_refused", fmt.Sprintf("the pending step %s is not the next step %s", r.pending.StepID, stepID))
		}
		resumePhase = r.resumePhase
		r.restoreHost(r.pending.HostBefore)
		r.resumeStep = false
	} else {
		rec := pairReplayPendingRecord{V: 1, StepID: stepID, AtMs: atMs, Ordinal: ordinal, PrefixHash: r.prefixHash(atMs), HostBefore: r.hostState()}
		saved, err := r.file.beginPairReplayStep(r.spec.Run.RunID, rec)
		if err != nil {
			return err
		}
		r.pending = saved
		r.cursor.PendingID, r.cursor.PendingMs, r.cursor.PendingHash = stepID, atMs, rec.PrefixHash
	}
	cfg, state, store, mu := r.cfg, r.state, r.store, r.mu
	in := r.cycleInputs(at)
	trace := []string{"evidence"}
	var risk map[RiskPartition]*scopeCycleRisk
	managed, saveBlocked := false, false
	var riskRecs []pairReplayRiskRecord
	if resumePhase == pairReplayPhaseCoordinator && r.pending.Coordinator != nil {
		r.bc.preparePairRisk(in)
		r.checkResumedView(at)
		cp := r.pending.Coordinator
		r.restoreHost(cp.HostAfter)
		managed, riskRecs = cp.Managed, cp.Risk
		risk = map[RiskPartition]*scopeCycleRisk{}
		for _, s := range cp.Risk {
			p, err := parseRiskPartition(s.Partition)
			if err != nil {
				return err
			}
			risk[p] = &scopeCycleRisk{Partition: p, Config: partitionRiskConfig(cfg, p), KillSwitchFired: s.KillSwitchFired, DailyLossEntriesHeld: s.DailyLossEntriesHeld, NotionalBlocked: s.NotionalBlocked}
		}
		trace = append(trace, "prepare_risk", "risk_restored")
	} else {
		r.bc.stepPairFundingDue(in)
		trace = append(trace, "funding")
		r.bc.preparePairRisk(in)
		r.checkResumedView(at)
		trace = append(trace, "prepare_risk")
		if allPartitionsSaveBlocked(store, cfg) {
			saveBlocked = true
			trace = append(trace, "save_blocked")
		} else {
			parts := activePartitions(cfg.Strategies)
			risk = make(map[RiskPartition]*scopeCycleRisk, len(parts))
			prices := map[string]float64{}
			mu.RLock()
			pm := r.bc.pairMarksLocked()
			for _, p := range parts {
				risk[p] = measureScopeCycleRiskMarked(p, partitionRiskConfig(cfg, p), cfg.Strategies, state, prices, nil, nil, true, false, at, pm)
			}
			mu.RUnlock()
			mu.Lock()
			for _, p := range parts {
				applyScopeCycleRisk(risk[p], state.partitionRisk(p))
				if !p.IsLive() && risk[p].KillSwitchFired {
					out := applyPaperKillSwitchCycle(state, cfg, prices, risk[p], true)
					if out.CloseApplied {
						fmt.Fprintf(os.Stderr, "[CRITICAL] Portfolio kill switch [%s]: the paper kill switch closed %d book(s); pair members stage their closes for the coordinator\n", partitionLabel(p), len(out.Closed))
					}
				}
			}
			mu.Unlock()
			trace = append(trace, "risk")
			if atMs >= r.nextManageMs {
				managed = true
				r.bc.beginCycle()
				due := dueStrategiesPersistable(store, dueStrategiesNotLatched(r.members, risk))
				for _, sc := range due {
					r.manageMember(sc, risk, at)
				}
				for r.nextManageMs <= atMs {
					r.nextManageMs += r.interval
				}
				trace = append(trace, "manage")
				r.fault("exit-after-management")
			}
		}
		riskRecs = pairReplayRiskRecords(risk)
		cp := r.pending
		cp.Coordinator = &pairReplayCoordinatorCheckpoint{HostAfter: r.hostState(), Managed: managed, Risk: riskRecs}
		r.commitMu.Lock()
		r.coordCommit = &cp
		r.commitMu.Unlock()
		mu.Lock()
		outcomes := store.SaveAll(state)
		mu.Unlock()
		r.commitMu.Lock()
		r.coordCommit = nil
		r.commitMu.Unlock()
		if err := outcomes[r.partition]; err == nil {
			r.pending = cp
		} else {
			fmt.Fprintf(os.Stderr, "[basket-replay] the coordinator checkpoint of step %s did not save: %v; a restart reruns the whole step\n", stepID, err)
		}
	}
	in.ScopeRisk, in.SaveBlocked = risk, saveBlocked
	trace = append(trace, "step")
	r.bc.step(in)
	fresh := r.bc.pairNow(basketCycleInputs{Now: at})
	next := r.nextWake(fresh.UnixMilli())
	sameTime := 0
	if next == atMs {
		sameTime = r.sameTime + 1
		if sameTime >= pairReplayMaxSameTime {
			next = r.nextWakeAfter(atMs)
			sameTime = 0
		}
	}
	rec := r.stepRecord(ordinal, stepID, at, managed, saveBlocked, trace, next, sameTime, riskRecs)
	recBlob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	binding, err := r.virtualEventCount()
	if err != nil {
		return err
	}
	commit := pairReplayFinalCommit{Ordinal: ordinal, StepID: stepID, AtMs: atMs, Record: recBlob, ResultHash: sha256Hex(recBlob), BindingSeq: binding}
	r.commitMu.Lock()
	r.finalCommit = &commit
	r.commitMu.Unlock()
	prevHook := storeCommitHook
	injectSave := r.scenario(pairReplayFailSave, func(f pairReplayFailureScript) bool { return f.atMs == atMs }) &&
		!r.hasFailure(pairReplayStageScenario, pairReplayFailSave, func(f pairReplayFailure) bool { return f.AtMs == atMs })
	storeCommitHook = func(role storageRole) error {
		if role == r.file.role {
			if injectSave {
				return fmt.Errorf("scenario save_failure at %d", atMs)
			}
			r.fault("exit-after-book-write")
		}
		if prevHook != nil {
			return prevHook(role)
		}
		return nil
	}
	if injectSave {
		r.recordFailure(pairReplayFailure{Stage: pairReplayStageScenario, Kind: pairReplayFailSave, AtMs: atMs, Cause: "scenario failure of the final save of step " + stepID})
	}
	mu.Lock()
	state.CycleCount++
	state.LastCycle = at
	outcomes := store.SaveAll(state)
	mu.Unlock()
	storeCommitHook = prevHook
	r.commitMu.Lock()
	r.finalCommit = nil
	r.commitMu.Unlock()
	if err := outcomes[r.partition]; err != nil {
		mu.Lock()
		state.CycleCount--
		mu.Unlock()
		r.stopReason = fmt.Sprintf("the required state save of step %s failed (%v); nothing advanced, and a resume reruns the step", stepID, err)
		return nil
	}
	r.fault("exit-after-book-commit")
	r.cursor.Applied, r.cursor.CursorMs, r.cursor.CursorStepID, r.cursor.CursorPhase, r.cursor.BindingSeq = ordinal+1, atMs, stepID, pairReplayPhaseComplete, binding
	r.cursor.PendingID, r.cursor.PendingMs, r.cursor.PendingHash = "", 0, ""
	r.nextAt, r.sameTime = next, sameTime
	return nil
}

func pairReplayRiskPartitions(m map[RiskPartition]*scopeCycleRisk) []RiskPartition {
	out := make([]RiskPartition, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (r *pairReplayRuntime) nextWake(nowMs int64) int64 {
	best := r.nextWakeAfter(nowMs)
	if d := r.bc.wakeDelay(time.UnixMilli(nowMs).UTC()); d >= 0 {
		if t := nowMs + d.Milliseconds(); t < best {
			best = t
		}
	}
	return best
}

func (r *pairReplayRuntime) nextWakeAfter(nowMs int64) int64 {
	best := int64(1<<62 - 1)
	consider := func(t int64) {
		if t > nowMs && t < best {
			best = t
		}
	}
	consider(r.nextManageMs)
	if r.wakeTimes == nil {
		r.wakeTimes = r.frozenWakeTimes()
	}
	if i := sort.Search(len(r.wakeTimes), func(i int) bool { return r.wakeTimes[i] > nowMs }); i < len(r.wakeTimes) {
		consider(r.wakeTimes[i])
	}
	for _, m := range r.rt.Contract.pairMembersSorted() {
		if t, ok := r.funding.NextVisibleMs(r.rt.Contract.pairMemberKey(m).evidenceKey(), nowMs); ok {
			consider(t)
		}
	}
	for _, v := range r.venues {
		if t, ok := v.NextVisibleMs(); ok {
			consider(t)
		}
	}
	return best
}

func (r *pairReplayRuntime) frozenWakeTimes() []int64 {
	set := map[int64]bool{}
	for _, t := range r.quotes.deliveries() {
		set[t] = true
	}
	if stamps, err := pairDecodeEvidenceDocumentBytes(r.inputs.evidence); err == nil {
		for _, st := range stamps {
			set[st.FirstReceiptMs], set[st.ValidFromMs] = true, true
			if st.ValidThroughMs < 1<<61 {
				set[st.ValidThroughMs+1] = true
			}
		}
	}
	for _, bars := range r.inputs.candles {
		for _, b := range bars {
			set[b.CloseMs+1] = true
		}
	}
	out := make([]int64, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func pairDecodeEvidenceDocumentBytes(blob []byte) ([]pairEvidenceStamp, error) {
	var d pairEvidenceDocument
	if err := json.Unmarshal(blob, &d); err != nil {
		return nil, err
	}
	var out []pairEvidenceStamp
	for _, c := range d.Capabilities {
		out = append(out, c.pairEvidenceStamp)
	}
	for _, a := range d.Accounts {
		out = append(out, a.pairEvidenceStamp)
	}
	for _, c := range d.Conversions {
		out = append(out, c.pairEvidenceStamp)
	}
	return out, nil
}

func (r *pairReplayRuntime) manageMember(sc StrategyConfig, risk map[RiskPartition]*scopeCycleRisk, at time.Time) {
	atMs := at.UnixMilli()
	mark, ok := r.quotes.nativeMid(sc.ID, atMs)
	if !ok {
		why := fmt.Sprintf("no recorded quote of %s is visible at %d for its management mark", sc.ID, atMs)
		r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: sc.ID, Cause: why})
		return
	}
	inject := r.scenario(pairReplayFailManagement, func(f pairReplayFailureScript) bool { return f.MemberID == sc.ID && f.atMs == atMs }) &&
		!r.hasFailure(pairReplayStageManagement, pairReplayKindNativeCheck, func(f pairReplayFailure) bool {
			return f.MemberID == sc.ID && f.AtMs == atMs && strings.HasPrefix(f.Cause, pairReplayScenarioManagementCause)
		})
	if sc.Platform == "okx" {
		r.manageOKX(sc, risk, at, mark, inject)
	} else {
		r.manageHL(sc, risk, at, mark, inject)
	}
	r.mu.RLock()
	echo := r.rt.pairEchoFailures[sc.ID]
	r.mu.RUnlock()
	if echo == "" {
		return
	}
	cause := echo
	if inject {
		cause = pairReplayScenarioManagementCause + echo
	}
	r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: sc.ID, AtMs: atMs, Cause: cause})
}

func (r *pairReplayRuntime) memberRisk(sc StrategyConfig, s *StrategyState, prices map[string]float64, logger *StrategyLogger) (bool, string, float64) {
	r.mu.RLock()
	pv, _ := portfolioValueMarked(s, prices, r.state.pairMarksLocked())
	r.mu.RUnlock()
	r.mu.Lock()
	allowed, reason := checkStrategyRiskWithView(&sc, s, pv, prices, r.state.pairMarksLocked(), logger, nil)
	r.mu.Unlock()
	return allowed, reason, pv
}

func (r *pairReplayRuntime) manageHL(sc StrategyConfig, risk map[RiskPartition]*scopeCycleRisk, at time.Time, mark float64, inject bool) {
	cfg, state, store, mu := r.cfg, r.state, r.store, r.mu
	s := state.Strategies[sc.ID]
	stratDB, err := store.dbForStrategy(sc.ID)
	if s == nil || err != nil {
		return
	}
	part := partitionFor(sc)
	sr := risk[part]
	if sr == nil {
		sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(cfg, part)}
	}
	sym := hyperliquidSymbol(sc.Args)
	prices := map[string]float64{sym: mark}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
	mu.RLock()
	pos, hasPos := readHLPerpsPosition(sc, s, sym, cfg.Regime)
	scaleInCash := s.Cash
	mu.RUnlock()
	allowed, reason, pv := r.memberRisk(sc, s, prices, logger)
	cbManageOnly := false
	if !allowed {
		logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
		if !circuitBreakerPermitsManagement(reason, sc.Platform, sc.Type, pos.Ctx.Quantity) {
			return
		}
		cbManageOnly = true
	}
	if notionalCapSkipsStrategyCycle(sr.NotionalBlocked) {
		return
	}
	snap := r.hlSnapshot(sc, at, prices)
	feedCtx := &marketFeedContext{Enabled: true, Requirements: r.feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
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
	if inject {
		prev := runHyperliquidCheckWithStdinFn
		runHyperliquidCheckWithStdinFn = func(script string, args []string, stdin []byte) (*HyperliquidResult, string, error) {
			return nil, "", errors.New("the scenario stopped the management check process")
		}
		defer func() { runHyperliquidCheckWithStdinFn = prev }()
	}
	runHyperliquidPerpsStep(x)
}

func (r *pairReplayRuntime) hlSnapshot(sc StrategyConfig, at time.Time, prices map[string]float64) *marketSnapshot {
	atMs := at.UnixMilli()
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: fmt.Sprintf("replay/%s/manage/%d", r.spec.Run.RunID, atMs), ConfigGeneration: 1, SealedAt: at,
		keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{}, observations: map[feedObservationKey]*marketSnapshotObservation{}}
	bars := r.inputs.candles[sc.ID]
	tf := r.inputs.timeframes[sc.ID]
	ivl, _ := hlCandleIntervalMs(tf)
	coin := hyperliquidSymbol(sc.Args)
	req := r.feedReq.Strategies[sc.ID]
	need := req.SignalLookback
	key := feedKeyFor(coin, tf)
	frame := pairReplayFrame(bars, ivl, atMs, need+2, true)
	entry := &marketSnapshotKey{Key: key, IntervalMs: ivl, Bars: frame}
	ready := need > 0 && len(frame) >= need
	entry.Readiness = feedKeyReadiness{Key: key, Bars: len(frame), Required: need, Ready: ready, Status: feedStatusReady, Source: "replay", LastRecvAt: at}
	if len(frame) > 0 {
		entry.Readiness.FirstOpenMs, entry.Readiness.LastOpenMs, entry.Readiness.LastCloseMs = frame[0].OpenMs, frame[len(frame)-1].OpenMs, frame[len(frame)-1].CloseMs
	}
	if !ready {
		entry.Readiness.Status, entry.Readiness.CoverageShort = feedStatusShort, true
		entry.Readiness.Detail = fmt.Sprintf("%d frozen bars at %s, below the %d the replay needs", len(frame), at.Format(time.RFC3339), need)
	}
	snap.keys[key] = entry
	for c, px := range prices {
		snap.mids[c] = feedMid{Px: px, RecvAt: at, Source: "replay"}
	}
	return snap
}

func (r *pairReplayRuntime) manageOKX(sc StrategyConfig, risk map[RiskPartition]*scopeCycleRisk, at time.Time, mark float64, inject bool) {
	cfg, state, store, mu := r.cfg, r.state, r.store, r.mu
	s := state.Strategies[sc.ID]
	stratDB, err := store.dbForStrategy(sc.ID)
	if s == nil || err != nil {
		return
	}
	part := partitionFor(sc)
	sr := risk[part]
	if sr == nil {
		sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(cfg, part)}
	}
	sym := okxSymbol(sc.Args)
	prices := map[string]float64{sym: mark}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
	allowed, reason, pv := r.memberRisk(sc, s, prices, logger)
	if !allowed {
		logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
		return
	}
	if notionalCapSkipsStrategyCycle(sr.NotionalBlocked) {
		return
	}
	c := okxStepContext{Cfg: cfg, State: state, StratState: s, Store: store, StratDB: stratDB, Mu: mu, Notifier: r.notifier, Logger: logger, Prices: prices,
		Risk: sr, Scope: part.Scope, PersistenceHold: store.persistenceHoldsPartition(part), Coord: r.bc}
	mu.RLock()
	if pos, ok := s.Positions[sym]; ok && pos != nil {
		ctx := positionCtxForCheck(sc, pos, cfg.Regime)
		c.Pos = okxStepPosition{Ctx: ctx, Qty: ctx.Quantity, Side: ctx.Side, AvgCost: ctx.AvgCost, Leverage: pos.Leverage}
	}
	mu.RUnlock()
	tf := r.inputs.timeframes[sc.ID]
	ivl, _ := hlCandleIntervalMs(tf)
	input := pairManagementInput{MarkPrice: mark}
	for _, b := range pairReplayFrame(r.inputs.candles[sc.ID], ivl, at.UnixMilli(), pairReplayManagementBars, false) {
		input.Candles = append(input.Candles, [6]float64{float64(b.OpenMs), b.Open, b.High, b.Low, b.Close, b.Volume})
	}
	check := okxPairManagementCheck(input, r.notifier)
	if inject {
		check = func(StrategyConfig, PositionCtx, *StrategyLogger) (*OKXResult, string, float64, bool) {
			logger.Error("the scenario stopped the management check process")
			return nil, "", 0, false
		}
	}
	runOKXStep(c, sc, check)
}
