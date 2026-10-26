package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pairPaperExportDirName = "export"
	pairPaperSealName      = "seal.json"
	pairPaperSourcePrefix  = "paper:"
	pairPaperWaitSlice     = 250 * time.Millisecond
)

var pairPaperFaultKinds = map[string]bool{
	"exit-after-call-start":   true,
	"exit-after-input-commit": true,
	"fail-record-append":      true,
}

type pairPaperOptions struct {
	basket basketReplayOptions
	seal   bool
	rec    pairRecFaults
}

type pairPaperManifest struct {
	Owner                string                       `json:"owner"`
	Version              int                          `json:"version"`
	RunID                string                       `json:"run_id"`
	Fingerprints         basketReplayFingerprints     `json:"fingerprints"`
	PaperIdentity        string                       `json:"paper_identity"`
	Partition            string                       `json:"partition"`
	EvidenceIdentity     pairRecordedEvidenceIdentity `json:"evidence_identity"`
	EvidenceDigest       string                       `json:"evidence_digest"`
	FundingBinding       json.RawMessage              `json:"funding_binding"`
	CostEvidence         json.RawMessage              `json:"cost_evidence"`
	MemberCosts          json.RawMessage              `json:"member_costs"`
	Objects              []pairReplayObjectRef        `json:"objects"`
	Lifetime             pairReplayLifetime           `json:"lifetime"`
	Collection           pairPaperCollection          `json:"collection"`
	Scenario             pairPaperScenario            `json:"scenario"`
	EventOrder           string                       `json:"event_order"`
	AdministrativeFields []string                     `json:"administrative_fields"`
	UID                  int                          `json:"uid"`
	CreatedAt            string                       `json:"created_at"`
}

const pairPaperEventOrder = "one host cycle per recorded wake; a wake's logical time is the larger of the previous logical time plus 1 ms and the wall clock; the cycle first commits its due match polls (member id order), capability refreshes and funding reads at its anchored grids, then runs coordinator funding, preparePairRisk, partition risk with the paper kill switch, native member management on a management cycle (Hyperliquid then OKX by member id, each with a recorded candle read, a recorded own mark and a recorded member check), the coordinator step, one full save that commits the step record and the cursor, then the next wake from fresh time; every outside read and process outcome commits before use and advances logical time to the larger of the previous logical time plus 1 ms and its raw completion"

type pairPaperRun struct {
	playback   bool
	spec       *pairPaperSpec
	collection pairPaperCollection
	scenario   pairPaperScenario
	egress     *pairResearchEgress
	seedBlob   []byte
	seedRef    pairReplayObjectRef
	identity   string
	evidence   *pairRecordedEvidenceSource
	funding    *pairRecordedFundingSource
	quotes     *recordingQuoteSource
	market     *recordingVirtualMarket
	manifest   pairPaperManifest
	seal       bool
	sealResult *pairPaperSealResult
	sealedLog  *pairRecSeal
	recording  *pairRecordingManifest
	barrierLog bool
	timeframes map[string]string
	preLog     *pairRecLog
}

func parsePairPaperFault(raw string) (basketReplayFault, pairRecFaults, error) {
	var f basketReplayFault
	if raw == "" {
		return f, pairRecFaults{}, nil
	}
	name, val, _ := strings.Cut(raw, ":")
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		return f, pairRecFaults{}, fmt.Errorf("--fault %q needs a positive occurrence count", raw)
	}
	if pairPaperFaultKinds[name] {
		return f, pairRecFaults{kind: name, n: n}, nil
	}
	if basketReplayFaultKinds[name] == pairReplayProtocolVersion {
		f.Kind, f.N = name, n
		return f, pairRecFaults{}, nil
	}
	if _, known := basketReplayFaultKinds[name]; known {
		return f, pairRecFaults{}, fmt.Errorf("--fault %s is a version-%d replay fault; basket-replay-paper takes the version-2 kinds and exit-after-call-start, exit-after-input-commit or fail-record-append", name, basketReplayFaultKinds[name])
	}
	return f, pairRecFaults{}, fmt.Errorf("--fault %q must be <kind>:<n> with a version-2 replay kind or exit-after-call-start, exit-after-input-commit or fail-record-append", raw)
}

func runBasketReplayPaper(args []string) int {
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	scrubBasketReplayEnvironment()
	return runBasketReplayPaperWith(args, os.Stdin, protocolOut, researchEgressAdmission)
}

func runBasketReplayPaperWith(args []string, stdin io.Reader, out io.Writer, egress *pairResearchEgress) int {
	fs := flag.NewFlagSet("basket-replay-paper", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	specPath := fs.String("spec", "", "path to the version-2 paper specification (JSON), or - for standard input")
	configPath := fs.String("config", "", "absolute path of the research egress file (checked before the egress gate)")
	maxSteps := fs.Int("max-steps", 0, "apply at most this many further host cycles, then stop with the run resumable (0 = all)")
	faultRaw := fs.String("fault", "", "controlled fault for restart tests: a version-2 replay kind or exit-after-call-start|exit-after-input-commit|fail-record-append:<n>")
	seal := fs.Bool("seal", false, "seal the finished run and export it for offline version-2 reproduction")
	if err := fs.Parse(args); err != nil {
		return basketReplayExitUsage
	}
	if fs.NArg() > 0 || *specPath == "" || *maxSteps < 0 || *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go-trader basket-replay-paper --spec <v2.json|-> --config <absolute research egress file> [--max-steps N] [--fault kind:N] [--seal]")
		return basketReplayExitUsage
	}
	f, rf, err := parsePairPaperFault(*faultRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return basketReplayExitUsage
	}
	emit := func(v pairReplayOutput, code int) int {
		enc := json.NewEncoder(out)
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "[basket-replay-paper] could not write the result: %v\n", err)
			return basketReplayExitError
		}
		return code
	}
	fail := func(err error) int {
		o, code := pairReplayErrorOutput(err)
		return emit(o, code)
	}
	if egress == nil || egress.Path != *configPath {
		return fail(refuseReplay("config_refused", "the research egress file was not admitted by the egress gate for this --config; isolated paper reads public venues only behind that gate"))
	}
	var src io.Reader = stdin
	if *specPath != "-" {
		fh, err := os.Open(*specPath)
		if err != nil {
			return fail(refuseReplay("spec_unreadable", err.Error()))
		}
		defer fh.Close()
		src = fh
	}
	raw, err := io.ReadAll(io.LimitReader(src, basketReplayMaxSpecBytes+1))
	if err != nil {
		return fail(refuseReplay("spec_unreadable", err.Error()))
	}
	if len(raw) > basketReplayMaxSpecBytes {
		return fail(refuseReplay("spec_too_large", fmt.Sprintf("the specification exceeds %d bytes", basketReplayMaxSpecBytes)))
	}
	spec, err := decodePairPaperSpec(raw)
	if err != nil {
		return fail(err)
	}
	lt, err := validatePairPaperSpec(spec)
	if err != nil {
		return fail(err)
	}
	if err := pairPaperRuntimeFiles(spec); err != nil {
		return fail(err)
	}
	rt, err := openPairPaper(spec, raw, lt, pairPaperOptions{basket: basketReplayOptions{MaxSteps: *maxSteps, Fault: f}, seal: *seal, rec: rf}, egress)
	if err != nil {
		return fail(err)
	}
	defer rt.Close()
	if err := rt.Run(); err != nil {
		return fail(err)
	}
	if rt.paper.seal {
		rt.sealPaper()
	}
	res, err := rt.output()
	if err != nil {
		return fail(err)
	}
	if res.MechanicallyComplete && (!rt.paper.seal || rt.paper.sealResult != nil && rt.paper.sealResult.Sealed) {
		return emit(res, basketReplayExitOK)
	}
	return emit(res, basketReplayExitError)
}

func pairPaperRuntimeFiles(spec *pairPaperSpec) error {
	var shape struct {
		Strategies []struct {
			Script string `json:"script"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(spec.ResearchConfig, &shape); err != nil {
		return refuseReplay("config_refused", err.Error())
	}
	required := []string{".venv/bin/python3", pairPolicyScript, pairQuoteReaderScript, pairEvidenceReaderScript, pairHistoryReaderScript}
	for _, sc := range shape.Strategies {
		required = append(required, sc.Script)
	}
	for _, p := range required {
		if _, err := os.Stat(p); err != nil {
			return refuseReplay("runtime_missing", fmt.Sprintf("the paper recorder runs the real readers, member checks and the canonical policy runner from the repository root; %s is missing: %v", p, err))
		}
	}
	return nil
}

func (r *pairReplayRuntime) paperProbes() [][]string {
	var probes [][]string
	for _, sc := range r.members {
		switch sc.Platform {
		case "hyperliquid":
			probes = append(probes, append([]string{sc.Script}, hyperliquidMarketCheckProbeArgv...),
				append([]string{pairQuoteReaderScript}, pairQuoteHLPublicProbeArgv...),
				append([]string{pairEvidenceReaderScript}, pairEvidenceHyperliquidProbeArgv...),
				append([]string{pairHistoryReaderScript}, pairHistoryHLCandlesProbeArgv...),
				append([]string{pairHistoryReaderScript}, pairHistoryHLFundingProbeArgv...))
		case "okx":
			probes = append(probes, append([]string{sc.Script}, okxPairManagementProbeArgv...),
				append([]string{pairQuoteReaderScript}, pairQuoteOKXPublicProbeArgv...),
				append([]string{pairEvidenceReaderScript}, pairEvidenceOKXProbeArgv...),
				append([]string{pairHistoryReaderScript}, pairHistoryOKXCandlesProbeArgv...),
				append([]string{pairHistoryReaderScript}, pairHistoryOKXFundingProbeArgv...))
		}
	}
	return append(probes, append([]string{pairPolicyScript}, pairPolicyProbeArgv...))
}

func (r *pairReplayRuntime) runRecordedProbes() error {
	for _, p := range r.paperProbes() {
		req, _ := json.Marshal(struct {
			Script string   `json:"script"`
			Argv   []string `json:"argv"`
		}{p[0], p[1:]})
		res, _, err := r.rec.call(context.Background(), pairRecCall{Kind: pairRecKindNativeCheck, Phase: pairRecPhaseBootstrap, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
			if perr := probeOneCheckScriptFn(p[0], p[1:]); perr != nil {
				return pairRecResult{Class: pairRecClassInfra, Cause: processEgress.redact(perr.Error())}
			}
			return pairRecResult{Class: pairRecClassResult, Result: []byte("probe ok")}
		})
		if err != nil {
			return refuseReplay("probe_failed", fmt.Sprintf("%s %s: the probe call could not be recorded: %v", p[0], strings.Join(p[1:], " "), err))
		}
		if res.Class != pairRecClassResult {
			return refuseReplay("probe_failed", fmt.Sprintf("%s %s: %s", p[0], strings.Join(p[1:], " "), res.Cause))
		}
	}
	return nil
}

var pairPaperResumeEntries = map[string]bool{
	pairResearchManifestName: true,
	pairPaperExportDirName:   true,
	pairPaperSealName:        true,
}

func checkPairPaperResumeDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return refuseReplay("state_missing", fmt.Sprintf("resume mode needs the paper-created state directory %q: %v", dir, err))
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
		if !basketReplayAllowedEntries[e.Name()] && !pairPaperResumeEntries[e.Name()] {
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
		return refuseReplay("state_foreign", fmt.Sprintf("%q holds entries the paper recorder never creates: %s", dir, strings.Join(extra, ", ")))
	}
	if !haveManifest {
		return refuseReplay("resume_refused", fmt.Sprintf("%q has no paper manifest: its initialization never finished (the manifest is written only after the startup bindings and the recording bootstrap commit), so it is not a resumable run", dir))
	}
	return nil
}

func readPairPaperManifest(dir string) (pairPaperManifest, error) {
	var m pairPaperManifest
	path := filepath.Join(dir, basketReplayManifestName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !basketReplayOwnedByMe(info) {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the paper manifest %q is not a regular file this user owns", path))
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return m, refuseReplay("state_foreign", err.Error())
	}
	if err := pairReplayStrictDecode(blob, &m); err != nil {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the manifest is not a paper recording manifest: %v", err))
	}
	if m.Owner != pairPaperManifestOwner || m.Version != pairPaperManifestVer {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the manifest owner %q version %d is not this recorder's", m.Owner, m.Version))
	}
	return m, nil
}

func comparePairPaperManifest(saved, want pairPaperManifest, withContract bool) error {
	var diffs []string
	check := func(name string, a, b string) {
		if a != b {
			diffs = append(diffs, name+" changed")
		}
	}
	check("run id", saved.RunID, want.RunID)
	check("protocol", saved.Fingerprints.Protocol, want.Fingerprints.Protocol)
	check("source (adapter version, source commit and provenance)", saved.Fingerprints.Source, want.Fingerprints.Source)
	check("paper identity", saved.PaperIdentity, want.PaperIdentity)
	check("config", saved.Fingerprints.Config, want.Fingerprints.Config)
	check("run", saved.Fingerprints.Run, want.Fingerprints.Run)
	check("evidence identity", saved.EvidenceDigest, want.EvidenceDigest)
	check("seed object", pairReplayObjectSet(saved.Objects), pairReplayObjectSet(want.Objects))
	check("lifetime", saved.Lifetime.String(), want.Lifetime.String())
	sa, _ := json.Marshal(saved.Collection)
	wa, _ := json.Marshal(want.Collection)
	check("collection bounds", string(sa), string(wa))
	sa, _ = json.Marshal(saved.Scenario)
	wa, _ = json.Marshal(want.Scenario)
	check("scenario", string(sa), string(wa))
	check("event order", saved.EventOrder, want.EventOrder)
	check("administrative field list", strings.Join(saved.AdministrativeFields, "\n"), strings.Join(want.AdministrativeFields, "\n"))
	if saved.UID != want.UID {
		diffs = append(diffs, "the run was created by another user")
	}
	if withContract {
		check("contract", saved.Fingerprints.Contract, want.Fingerprints.Contract)
		check("partition", saved.Partition, want.Partition)
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

func pairPaperShape(raw json.RawMessage) (pairReplayResearchShape, error) {
	var shape pairReplayResearchShape
	if err := json.Unmarshal(raw, &shape); err != nil || len(shape.Pairs) != 1 {
		return shape, refuseReplay("config_refused", "research_config needs exactly one pair")
	}
	if shape.IntervalSeconds <= 0 {
		return shape, refuseReplay("config_refused", "research_config needs a positive interval_seconds: it is the member management cadence")
	}
	return shape, nil
}

func pairPaperMemberPartition(shape pairReplayResearchShape, id string) (RiskPartition, string) {
	part := defaultPaperPartition
	provider := ""
	for _, sc := range shape.Strategies {
		if sc.ID != id {
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
	}
	return part, provider
}

type pairRecordedSetup struct {
	runID      string
	stateDir   string
	resume     bool
	shape      pairReplayResearchShape
	seed       []byte
	seedDigest string
	scenario   pairPaperScenario
	collection pairPaperCollection
	egress     *EgressConfig
	recMode    pairRecMode
	faults     pairRecFaults
}

func (r *pairReplayRuntime) buildRecordedSources(o pairRecordedSetup) (map[string]*pairVirtualVenue, error) {
	evidence, err := newPairRecordedEvidenceSource(o.seed, o.seedDigest, pairPaperSourcePrefix+o.runID, o.collection.FromMs)
	if err != nil {
		return nil, refuseReplay("input_refused", err.Error())
	}
	funding, err := newPairRecordedFundingSource(pairPaperSourcePrefix+o.runID, o.collection.FromMs)
	if err != nil {
		return nil, refuseReplay("input_refused", "funding: "+err.Error())
	}
	r.rec = newPairRecorder(o.recMode, o.runID, "", "", nil, r.clock, o.faults)
	sources := map[string]string{}
	pair := o.shape.Pairs[0]
	szDecimals := map[string]int{}
	for _, m := range pair.Members {
		sources[m.StrategyID] = m.Source
	}
	quotes := newRecordingQuoteSource(r.rec, sources, r.lt.MaxSteps, o.egress, r.clock.Now)
	market := &recordingVirtualMarket{src: quotes}
	r.paper.evidence, r.paper.funding, r.paper.quotes, r.paper.market = evidence, funding, quotes, market
	band := 0.0
	if b, ok := pair.Params["adverse_band_bps"]; ok {
		if err := json.Unmarshal(b, &band); err != nil {
			return nil, refuseReplay("config_refused", "pairs[0].params.adverse_band_bps is not a number")
		}
	}
	venues := map[string]*pairVirtualVenue{}
	for _, m := range pair.Members {
		part, provider := pairPaperMemberPartition(o.shape, m.StrategyID)
		if provider == entryProviderHyperliquid {
			szDecimals[m.Instrument] = m.Meta.SzDecimals
		}
		v, err := newPairVirtualVenue(pairVirtualVenueConfig{Partition: part, RunTag: o.runID, MemberID: m.StrategyID, Provider: provider, AccountKey: m.AccountKey,
			Instrument: m.Instrument, Market: market, AdverseBandBps: band, SubmitLatencyMs: o.scenario.SubmitLatencyMs, ResponseLatencyMs: o.scenario.ResponseLatencyMs})
		if err != nil {
			return nil, refuseReplay("config_refused", fmt.Sprintf("member %s virtual venue: %v", m.StrategyID, err))
		}
		venues[m.StrategyID] = v
	}
	r.lots = newFrozenHLLotMetadataCache(szDecimals, func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) })
	return venues, nil
}

func (r *pairReplayRuntime) bindRecordedProcess() error {
	if err := r.clock.Set(time.UnixMilli(r.lt.StartMs).UTC()); err != nil {
		return err
	}
	if err := bindTradingClock(basketReplayClockOwner, r.clock.Now); err != nil {
		return err
	}
	r.cleanups = append(r.cleanups, func() { unbindTradingClock(basketReplayClockOwner) })
	prevLots := hlLotMetadata
	hlLotMetadata = r.lots
	r.cleanups = append(r.cleanups, func() { hlLotMetadata = prevLots })
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	r.cleanups = append(r.cleanups, func() { tradeRecorder = prevRecorder })
	prevFundingClock := paperFundingClock
	paperFundingClock = func() time.Time { return tradingNow().UTC() }
	r.cleanups = append(r.cleanups, func() { paperFundingClock = prevFundingClock })
	return nil
}

func openPairPaper(spec *pairPaperSpec, raw []byte, lt pairReplayLifetime, opts pairPaperOptions, egress *pairResearchEgress) (*pairReplayRuntime, error) {
	r := &pairReplayRuntime{spec: spec.replaySpec(), lt: lt, opts: opts.basket, mu: &sync.RWMutex{}, notifier: NewMultiNotifier(), clock: &simulatedClock{}, faultCount: map[string]int{}}
	r.paper = &pairPaperRun{spec: spec, collection: spec.Collection, scenario: spec.Scenario, egress: egress, seal: opts.seal, timeframes: map[string]string{}}
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
	resume := spec.Mode == basketReplayModeResume
	if !resume {
		if _, err := os.Lstat(stateDir); err == nil {
			return nil, refuseReplay("state_exists", fmt.Sprintf("new-run mode refuses the existing path %q; a new run claims its own directory", stateDir))
		}
	} else if err := checkPairPaperResumeDir(stateDir); err != nil {
		return nil, err
	}
	identity, err := pairPaperIdentity(raw)
	if err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	r.paper.identity = identity
	seedRef := spec.InputManifest.Objects[0]
	seed, err := pairReplayReadObject(spec.InputManifest.Root, seedRef)
	if err != nil {
		return nil, refuseReplay("input_refused", "seed evidence: "+err.Error())
	}
	r.paper.seedBlob, r.paper.seedRef = seed, seedRef
	r.inputs = &pairReplayInputs{evidence: seed, objects: []pairReplayObjectRef{seedRef}, recordTotal: map[string]int{}, quotes: map[string]pairReplayQuoteDoc{},
		books: map[string]pairReplayBookDoc{}, candles: map[string][]feedBar{}, timeframes: map[string]string{}}
	if d, derr := pairDecodeEvidenceDocument(seed); derr == nil {
		r.inputs.recordTotal[pairReplayKindEvidence+"|"] = len(d.Capabilities) + len(d.Accounts) + len(d.Conversions)
	}
	cfgJSON, err := pairReplayConfigDocument(spec.ResearchConfig, stateDir)
	if err != nil {
		return nil, err
	}
	portable, err := pairReplayConfigDocument(spec.ResearchConfig, "/basket-replay-state")
	if err != nil {
		return nil, err
	}
	r.cfgJSON = cfgJSON
	shape, err := pairPaperShape(spec.ResearchConfig)
	if err != nil {
		return nil, err
	}
	r.interval = int64(shape.IntervalSeconds) * 1000
	r.fp = basketReplayFingerprints{Protocol: fmt.Sprintf("%s/%d", pairPaperProtocolLabel, pairReplayProtocolVersion), Source: basketReplaySourceFingerprint(spec.Run.Source), Input: identity, Config: sha256Hex(portable)}
	runBlob, _ := json.Marshal([]string{r.fp.Protocol, spec.Run.RunID, r.fp.Source, r.fp.Input, r.fp.Config})
	r.fp.Run = sha256Hex(runBlob)
	var egressCfg *EgressConfig
	if egress != nil {
		egressCfg = egress.Egress
	}
	mode := pairRecLive
	if resume {
		mode = pairRecResume
	}
	venues, err := r.buildRecordedSources(pairRecordedSetup{runID: spec.Run.RunID, stateDir: stateDir, resume: resume, shape: shape, seed: seed, seedDigest: seedRef.SHA256,
		scenario: spec.Scenario, collection: spec.Collection, egress: egressCfg, recMode: mode, faults: opts.rec})
	if err != nil {
		return nil, err
	}
	r.venues = venues
	part0, _ := pairPaperMemberPartition(shape, shape.Pairs[0].Members[0].StrategyID)
	r.paper.manifest = pairPaperManifest{Owner: pairPaperManifestOwner, Version: pairPaperManifestVer, RunID: spec.Run.RunID, Fingerprints: r.fp, PaperIdentity: identity,
		Partition: part0.String(), EvidenceIdentity: r.paper.evidence.Identity(), EvidenceDigest: r.paper.evidence.Digest(), Objects: []pairReplayObjectRef{seedRef}, Lifetime: lt,
		Collection: spec.Collection, Scenario: spec.Scenario, EventOrder: pairPaperEventOrder, AdministrativeFields: pairReplayAdministrativeFields, UID: os.Getuid()}
	r.manifest = pairReplayManifest{Owner: basketReplayOwnerMarker, Version: pairReplayManifestVersion, RunID: spec.Run.RunID, Fingerprints: r.fp, EvidenceDigest: r.paper.evidence.Digest(),
		Objects: []pairReplayObjectRef{seedRef}, Lifetime: lt, Scenario: spec.Scenario.replay(), EventOrder: pairPaperEventOrder, AdministrativeFields: pairReplayAdministrativeFields, UID: os.Getuid()}
	var saved pairPaperManifest
	if resume {
		saved, err = readPairPaperManifest(stateDir)
		if err != nil {
			return nil, err
		}
		if err := comparePairPaperManifest(saved, r.paper.manifest, false); err != nil {
			return nil, err
		}
		if err := r.preAdmissionRestore(stateDir, saved); err != nil {
			return nil, err
		}
	}
	if err := r.bindRecordedProcess(); err != nil {
		return nil, err
	}
	venueFor := func(sc StrategyConfig) (basketVenue, error) {
		if v := r.venues[sc.ID]; v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("no virtual venue for %s", sc.ID)
	}
	adm, cfg, err := newPairResearchAdmission(cfgJSON, spec.Run.RunID, stateDir, resume, pairResearchDeps{VenueFor: venueFor, Evidence: r.paper.evidence, Quotes: r.paper.quotes, Funding: r.paper.funding})
	if err != nil {
		return nil, refuseReplay("admission_refused", err.Error())
	}
	r.cleanups = append(r.cleanups, adm.release)
	r.adm, r.cfg = adm, cfg
	r.fp.Contract = adm.contract
	r.manifest.Fingerprints.Contract, r.paper.manifest.Fingerprints.Contract = adm.contract, adm.contract
	r.manifest.FundingBinding, r.paper.manifest.FundingBinding = json.RawMessage(adm.fundingBinding), json.RawMessage(adm.fundingBinding)
	if resume {
		if err := comparePairPaperManifest(saved, r.paper.manifest, true); err != nil {
			return nil, err
		}
		r.manifest.CostEvidence, r.manifest.MemberCosts, r.manifest.CreatedAt = saved.CostEvidence, saved.MemberCosts, saved.CreatedAt
		r.paper.manifest.CostEvidence, r.paper.manifest.MemberCosts, r.paper.manifest.CreatedAt = saved.CostEvidence, saved.MemberCosts, saved.CreatedAt
		cur, err := verifyPairReplayDBReadOnly(filepath.Join(stateDir, basketReplayDBName), r.manifest)
		if err != nil {
			return nil, err
		}
		r.cursor = cur
	} else if err := os.Mkdir(filepath.Join(stateDir, basketReplayLogDirName), 0o700); err != nil {
		return nil, err
	}
	if err := r.openRecordedStore(); err != nil {
		return nil, err
	}
	r.rec.partition, r.rec.contractFP, r.rec.file = r.partition.String(), adm.contract, r.file
	if err := r.file.ensurePairRecordingSchema(); err != nil {
		return nil, err
	}
	if !resume {
		if err := r.file.insertPairReplayRun(r.manifest, r.manifest.Objects, r.inputs.recordTotal); err != nil {
			return nil, err
		}
	} else {
		cur, err := r.file.readPairReplayCursor(spec.Run.RunID)
		if err != nil {
			return nil, err
		}
		if cur != r.cursor {
			return nil, refuseReplay("resume_refused", "the run cursor changed while the paper recorder opened the state file")
		}
		if err := r.recheckRecordingFrontier(); err != nil {
			return nil, err
		}
	}
	if err := r.loadState(); err != nil {
		return nil, err
	}
	if !resume {
		if err := r.paperStartupCommit(); err != nil {
			return nil, err
		}
	} else if err := r.paperResumeState(); err != nil {
		return nil, err
	}
	r.store.saveExtra = r.saveExtra
	ok = true
	return r, nil
}

func (r *pairReplayRuntime) openRecordedStore() error {
	cfg := r.cfg
	stateDir := r.spec.Run.StateDir
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return refuseReplay("config_refused", err.Error())
	}
	for _, f := range layout.Files {
		if f.InMemory || filepath.Dir(f.Path) != stateDir {
			return refuseReplay("state_dir_refused", fmt.Sprintf("state file %q is outside the run state directory", f.Path))
		}
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return refuseReplay("config_refused", err.Error())
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		return refuseReplay("state_busy", err.Error())
	}
	r.cleanups = append(r.cleanups, func() { owned.Release() })
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		return err
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
		return refuseReplay("config_refused", "the paper recorder runs exactly two pair members")
	}
	r.partition = partitionFor(r.members[0])
	r.file = store.file(layout.roleForPartition(r.partition))
	if r.file == nil {
		return fmt.Errorf("no state file owns the %s partition", partitionLabel(r.partition))
	}
	if err := r.file.ensureBasketReplaySchema(); err != nil {
		return err
	}
	for _, sc := range r.members {
		tf := ""
		if len(sc.Args) >= 3 {
			tf = sc.Args[2]
		}
		if _, ok := hlCandleIntervalMs(tf); !ok {
			return refuseReplay("config_refused", fmt.Sprintf("member %s timeframe %q is not a candle interval the management reads can record", sc.ID, tf))
		}
		r.paper.timeframes[sc.ID] = tf
	}
	return nil
}

func (r *pairReplayRuntime) preAdmissionRestore(stateDir string, saved pairPaperManifest) error {
	db, err := openPairFundingSnapshot(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	defer db.Close()
	l, err := readVerifiedPairRecLog(db, saved.RunID, saved.Partition, saved.Fingerprints.Contract)
	if err != nil {
		return err
	}
	r.paper.preLog = l
	if l.bind == nil {
		return refuseReplay("resume_refused", "the recording has no binding event; its initialization never finished")
	}
	if l.bind.PaperIdentity != r.paper.identity || l.bind.SeedSHA256 != r.paper.seedRef.SHA256 || l.bind.EvidenceIdentity != r.paper.evidence.Digest() {
		return refuseReplay("resume_refused", "the recording binds another paper identity, seed or evidence identity")
	}
	if l.sealed != nil {
		pairPaperSealedFromLog(stateDir, l)
		return refuseReplay("resume_refused", fmt.Sprintf("the run is sealed at input %d; a sealed recording is immutable and is reproduced with basket-replay --spec version 2", l.sealed.FinalSeq))
	}
	return r.restoreFundingFrom(l)
}

func readVerifiedPairRecLog(db *StateDB, runID, partition, contractFP string) (*pairRecLog, error) {
	ok, err := tableExists(db, "basket_replay_inputs")
	if err != nil {
		return nil, refuseReplay("state_foreign", err.Error())
	}
	if !ok {
		return nil, refuseReplay("resume_refused", "the state file holds no recording; its initialization never finished")
	}
	tx, err := db.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := readPairRecRows(tx, runID)
	if err != nil {
		return nil, refuseReplay("state_foreign", err.Error())
	}
	calls, err := readPairRecCallRows(tx, runID)
	if err != nil {
		return nil, refuseReplay("state_foreign", err.Error())
	}
	slots, err := readPairRecSlotRows(tx, runID)
	if err != nil {
		return nil, refuseReplay("state_foreign", err.Error())
	}
	l, err := verifyPairRecLog(rows, runID, partition, contractFP)
	if err != nil {
		return nil, refuseReplay("resume_refused", "the committed recording is refused before any trading state opens: "+err.Error())
	}
	if err := l.checkCalls(calls); err != nil {
		return nil, refuseReplay("resume_refused", "the call index is refused: "+err.Error())
	}
	if err := l.checkSlots(slots); err != nil {
		return nil, refuseReplay("resume_refused", "the slot index is refused: "+err.Error())
	}
	return l, nil
}

func (r *pairReplayRuntime) restoreFundingFrom(l *pairRecLog) error {
	src := r.paper.funding
	for _, row := range l.rows {
		if row.Kind != pairRecEventEnd || row.Event.End.Class != pairRecClassResult {
			continue
		}
		si := l.starts[row.CallID]
		if l.rows[si].Event.Start.Kind != pairRecKindFunding {
			continue
		}
		b, err := pairDecodeFundingBatch(row.Event.End.Result)
		if err != nil {
			return refuseReplay("resume_refused", fmt.Sprintf("the recorded funding batch of call %s is unreadable: %v", row.CallID, err))
		}
		if b.Seq <= int64(len(src.chain())) {
			continue
		}
		_ = src.Append(row.Event.End.Result)
	}
	return nil
}

func (r *pairReplayRuntime) restoreSourcesFrom(l *pairRecLog, upTo int) error {
	for i, row := range l.rows {
		if upTo >= 0 && i >= upTo {
			break
		}
		if row.Kind != pairRecEventEnd || row.Event.End.Class != pairRecClassResult {
			continue
		}
		s := l.rows[l.starts[row.CallID]].Event.Start
		if err := r.deliverRecorded(s, row.CallID, row.Event.End); err != nil {
			return refuseReplay("resume_refused", fmt.Sprintf("the recorded %s result of call %s cannot be restored: %v", s.Kind, row.CallID, err))
		}
	}
	return nil
}

func (r *pairReplayRuntime) deliverRecorded(s *pairRecStart, callID string, end *pairRecEnd) error {
	switch s.Kind {
	case pairRecKindAccount, pairRecKindConversions, pairRecKindCapability:
		if s.Phase == pairRecPhaseBootstrap {
			return r.admitSeedRecords(s.Kind, end.Result, end.LogicalMs)
		}
		var c pairCapabilityEvidence
		if err := pairReplayStrictDecode(end.Result, &c); err != nil {
			return err
		}
		return r.paper.evidence.admitCapability(c, end.LogicalMs)
	case pairRecKindMatchPoll:
		m, ok := r.paper.quotes.member(s.MemberID)
		if !ok {
			return fmt.Errorf("member %s is not bound", s.MemberID)
		}
		rd, err := pairPaperDecodeRead(end.Result)
		if err != nil {
			return err
		}
		o, err := r.paper.quotes.derive(m, rd, "")
		if err != nil {
			return err
		}
		r.paper.quotes.addPoll(s.MemberID, o, end.LogicalMs, callID)
	case pairRecKindFunding:
		b, err := pairDecodeFundingBatch(end.Result)
		if err != nil {
			return err
		}
		if b.Seq > int64(len(r.paper.funding.chain())) {
			_ = r.paper.funding.Append(end.Result)
		}
	}
	return nil
}

type pairSeedRecords struct {
	Capabilities []pairCapabilityEvidence `json:"capabilities,omitempty"`
	Accounts     []pairAccountEvidence    `json:"accounts,omitempty"`
	Conversions  []pairConversionEvidence `json:"conversions,omitempty"`
}

func (r *pairReplayRuntime) admitSeedRecords(kind string, blob []byte, logical int64) error {
	var recs pairSeedRecords
	if err := pairReplayStrictDecode(blob, &recs); err != nil {
		return err
	}
	ev := r.paper.evidence
	for _, c := range recs.Capabilities {
		if err := ev.admitCapability(c, logical); err != nil {
			return err
		}
	}
	for _, a := range recs.Accounts {
		if err := ev.admitAccount(a, logical); err != nil {
			return err
		}
	}
	for _, c := range recs.Conversions {
		if err := ev.admitConversion(c, logical); err != nil {
			return err
		}
	}
	return nil
}

func (r *pairReplayRuntime) recheckRecordingFrontier() error {
	l, err := readVerifiedPairRecLog(r.file, r.spec.Run.RunID, r.partition.String(), r.adm.contract)
	if err != nil {
		return err
	}
	pre := r.paper.preLog
	if pre == nil || l.head() != pre.head() {
		return refuseReplay("resume_refused", "the committed recording changed between the read-only check and the writable open")
	}
	r.paper.preLog = l
	return nil
}

func (r *pairReplayRuntime) paperStartupCommit() error {
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
	r.rec.epoch = 1
	bind := pairRecBind{RunID: r.spec.Run.RunID, PaperIdentity: r.paper.identity, ContractFP: r.adm.contract, Partition: r.partition.String(), SeedSHA256: r.paper.seedRef.SHA256,
		EvidenceIdentity: r.paper.evidence.Digest(), FundingBinding: string(r.adm.fundingBinding), AnchorMs: r.paper.collection.FromMs}
	if err := r.rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: time.Now().UnixMilli(), Mode: "new"}}); err != nil {
		return fmt.Errorf("the recording bootstrap could not commit: %w", err)
	}
	if err := r.bootstrapSeed(); err != nil {
		return err
	}
	if err := r.runRecordedProbes(); err != nil {
		return err
	}
	r.mu.RLock()
	r.paper.manifest.CostEvidence = append(json.RawMessage(nil), r.rt.Pair.Progress.CostEvidence...)
	r.mu.RUnlock()
	r.manifest.CostEvidence = r.paper.manifest.CostEvidence
	r.paper.manifest.MemberCosts, r.manifest.MemberCosts = json.RawMessage(blob), json.RawMessage(blob)
	r.paper.manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	r.manifest.CreatedAt = r.paper.manifest.CreatedAt
	mb, err := json.MarshalIndent(r.paper.manifest, "", "  ")
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
	r.initPaperGrids()
	return nil
}

func (r *pairReplayRuntime) bootstrapSeed() error {
	ev := r.paper.evidence
	for _, sc := range r.members {
		caps, accts, convs := ev.seedRecords(sc.ID)
		for _, kind := range []string{pairRecKindCapability, pairRecKindAccount, pairRecKindConversions} {
			var recs pairSeedRecords
			switch kind {
			case pairRecKindCapability:
				recs.Capabilities = caps
			case pairRecKindAccount:
				recs.Accounts = accts
			default:
				recs.Conversions = convs
			}
			blob, err := json.Marshal(recs)
			if err != nil {
				return err
			}
			req, _ := json.Marshal(struct {
				MemberID string `json:"member_id"`
				Source   string `json:"source"`
				Seed     string `json:"seed_sha256"`
			}{sc.ID, "seed evidence object", r.paper.seedRef.SHA256})
			res, saved, err := r.rec.call(context.Background(), pairRecCall{Kind: kind, Phase: pairRecPhaseBootstrap, MemberID: sc.ID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
				return pairRecResult{Class: pairRecClassResult, Result: blob}
			})
			if err != nil {
				return fmt.Errorf("the %s seed record of %s could not be committed: %w", kind, sc.ID, err)
			}
			if err := r.admitSeedRecords(kind, res.Result, saved.End.LogicalMs); err != nil {
				return refuseReplay("input_refused", fmt.Sprintf("the %s seed record of %s is refused: %v", kind, sc.ID, err))
			}
		}
	}
	ev.activate()
	return nil
}

func (r *pairReplayRuntime) initPaperGrids() {
	c, sc := r.paper.collection, r.paper.scenario
	r.pollNextMs = c.FromMs
	r.fundingNextMs = c.FromMs + sc.PaperFundingPollMs
	r.capabilityNextMs = 0
	if sc.PaperCapabilityRefreshMs > 0 {
		r.capabilityNextMs = c.FromMs + sc.PaperCapabilityRefreshMs
	}
}

func (r *pairReplayRuntime) paperResumeState() error {
	l := r.paper.preLog
	if err := r.restoreSourcesFrom(l, -1); err != nil {
		return err
	}
	r.paper.evidence.activate()
	r.rec.log = l
	r.rec.head = l.head()
	r.rec.epoch = l.epochs + 1
	for _, row := range l.rows {
		switch {
		case row.Kind == pairRecEventEnd:
			r.rec.noteRaw(row.Event.End.RawCompletionMs)
		case row.Kind == pairRecEventWake:
			r.rec.noteRaw(row.Event.Wake.WallMs)
		}
	}
	if err := r.rec.appendDirect(pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: r.rec.epoch, WallMs: time.Now().UnixMilli(), Mode: "resume"}}); err != nil {
		return fmt.Errorf("the resume marker could not be recorded: %w", err)
	}
	var orphans []pairRecEvent
	for _, id := range l.unterminated() {
		orphans = append(orphans, pairRecEvent{Kind: pairRecEventRecovery, CallID: id, Recovery: &pairRecRecovery{Reason: "the process stopped after this call's start committed and before its outcome committed; the result is unknown and the call is never repeated", Epoch: r.rec.epoch}})
	}
	if len(orphans) > 0 {
		if err := r.rec.appendDirect(orphans...); err != nil {
			return fmt.Errorf("the interrupted child calls could not be marked unknown: %w", err)
		}
		for _, ev := range orphans {
			l.ends[ev.CallID] = -1
		}
	}
	if err := r.resumeState(); err != nil {
		return err
	}
	if err := r.runRecordedProbes(); err != nil {
		return err
	}
	r.mu.RLock()
	blocked := r.rt.Pair.Progress.RecordingBlocked
	r.mu.RUnlock()
	if blocked != nil {
		r.paper.barrierLog = true
		ev := pairRecEvent{Kind: pairRecEventClear, Clear: &pairRecClear{VerifiedThroughSeq: r.rec.head.Seq, FailedSeq: blocked.FailedSeq, Reason: "restart verification of the full committed recording before any increase: " + blocked.Reason, Epoch: r.rec.epoch}}
		if r.rec.mode != pairRecLive {
			r.rec.refuse("the recording barrier from input %d was set before a restart; the refused calls of that process were not recorded, so this run makes no equality claim", blocked.FailedSeq)
		}
		if err := r.rec.appendDirect(ev); err != nil {
			return fmt.Errorf("the barrier release after the restart verification could not be recorded: %w", err)
		}
		r.mu.Lock()
		r.rt.Pair.Progress.RecordingBlocked = nil
		r.rt.touchPairProgress()
		err := r.store.SavePartition(r.state, r.partition)
		r.mu.Unlock()
		if err != nil {
			return fmt.Errorf("the cleared recording barrier could not be saved: %w", err)
		}
	}
	return nil
}

func (r *pairReplayRuntime) paperHooks() *pairReplayHooks {
	c := r.paper.collection
	return &pairReplayHooks{collected: r.onCollected, frozen: r.onFrozen, beforeSubmit: r.beforeSubmit, submitted: r.onSubmitted,
		collectHold: func(rt *basketRuntime, startMs int64) string {
			switch {
			case startMs < c.FromMs:
				return fmt.Sprintf("the paper collection starts at %d", c.FromMs)
			case startMs >= c.ThroughMs:
				return fmt.Sprintf("the paper collection ended at %d; the drain through %d runs saved actions, funding, protection and emergency work only", c.ThroughMs, c.DrainThroughMs)
			}
			return r.rec.increaseHold()
		},
		decideHold: func(rt *basketRuntime, nowMs int64) string {
			if slot := pairNewestUndecidedSlot(rt); slot > 0 && r.rec.catchUpCollect(slot) {
				nowMs = r.clock.Now().UnixMilli()
			}
			if nowMs >= c.ThroughMs {
				return fmt.Sprintf("no new decision is made after the collection end %d", c.ThroughMs)
			}
			return r.rec.increaseHold()
		},
		increaseHold: func(rt *basketRuntime) string { return r.rec.increaseHold() },
		budget:       r.rec.budget,
	}
}

func pairNewestUndecidedSlot(rt *basketRuntime) int64 {
	var newest int64
	for _, d := range rt.sortedDeadlines() {
		if rb := rt.Rebalances[d]; rb.Status == basketRebalanceFrozen && rb.TargetsJSON == "" {
			newest = d
		}
	}
	return newest
}

func (r *pairReplayRuntime) syncBarrier() {
	blocked, reason, failed := r.rec.barrier()
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &r.rt.Pair.Progress
	switch {
	case blocked && p.RecordingBlocked == nil:
		p.RecordingBlocked = &pairRecBarrier{Reason: reason, FailedSeq: failed, AtMs: r.clock.Now().UnixMilli()}
		r.rt.touchPairProgress()
	case !blocked && p.RecordingBlocked != nil:
		p.RecordingBlocked = nil
		r.rt.touchPairProgress()
	}
}

func (r *pairReplayRuntime) verifyRecordingForClear() (int64, error) {
	l, err := readVerifiedPairRecLog(r.file, r.spec.Run.RunID, r.partition.String(), r.adm.contract)
	if err != nil {
		return 0, err
	}
	if l.head() != r.rec.head {
		return 0, fmt.Errorf("the committed recording head (seq %d) is not the recorder head (seq %d)", l.head().Seq, r.rec.head.Seq)
	}
	return l.head().Seq, nil
}

func (r *pairReplayRuntime) deliver(at time.Time) []string {
	atMs := at.UnixMilli()
	var trace []string
	c, sc := r.paper.collection, r.paper.scenario
	ctx := context.Background()
	if r.pollNextMs > 0 && atMs >= r.pollNextMs && r.pollNextMs <= c.DrainThroughMs {
		for _, m := range r.members {
			if err := r.paper.quotes.poll(ctx, m.ID); err != nil {
				fmt.Fprintf(os.Stderr, "[paper] match poll of %s: %v\n", m.ID, err)
			}
		}
		for r.pollNextMs <= atMs {
			r.pollNextMs += sc.PaperPollIntervalMs
		}
		trace = append(trace, "match_poll")
	}
	if r.capabilityNextMs > 0 && atMs >= r.capabilityNextMs && r.capabilityNextMs <= c.DrainThroughMs {
		for _, m := range r.rt.Contract.pairMembersSorted() {
			key := r.rt.Contract.pairMemberKey(m).evidenceKey()
			prev, perr := r.paper.evidence.Capability(ctx, m, r.clock.Now().UnixMilli())
			var pp *pairCapabilityEvidence
			if perr == nil {
				pp = &prev
			}
			capEv, logical, err := recordingEvidenceSource{rec: r.rec, src: r.paper.evidence, egress: r.paperEgress()}.refresh(ctx, key, pp, 2*sc.PaperCapabilityRefreshMs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[paper] capability refresh of %s: %v\n", m.MemberID, err)
				continue
			}
			if err := r.paper.evidence.admitCapability(capEv, logical); err != nil {
				fmt.Fprintf(os.Stderr, "[paper] the recorded capability of %s is refused: %v\n", m.MemberID, err)
			}
		}
		for r.capabilityNextMs <= atMs {
			r.capabilityNextMs += sc.PaperCapabilityRefreshMs
		}
		trace = append(trace, "capability")
	}
	if r.fundingNextMs > 0 && atMs >= r.fundingNextMs && r.fundingNextMs <= c.DrainThroughMs {
		for _, m := range r.rt.Contract.pairMembersSorted() {
			key := r.rt.Contract.pairMemberKey(m).evidenceKey()
			chain := r.paper.funding.chain()
			seq, prev := int64(len(chain)+1), ""
			if len(chain) > 0 {
				prev = chain[len(chain)-1].BatchHash
			}
			from := pairPaperFundingFrom(chain, key, c.FromMs)
			through := r.clock.Now().UnixMilli()
			if through <= from {
				continue
			}
			batch, err := recordingFundingSource{rec: r.rec, src: r.paper.funding, egress: r.paperEgress()}.read(ctx, key, seq, prev, from, through)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[paper] funding read of %s: %v\n", m.MemberID, err)
				continue
			}
			if err := r.paper.funding.Append(batch); err != nil {
				fmt.Fprintf(os.Stderr, "[paper] the recorded funding batch %d of %s is refused and stays only in the recording: %v\n", seq, m.MemberID, err)
			}
		}
		for r.fundingNextMs <= atMs {
			r.fundingNextMs += sc.PaperFundingPollMs
		}
		trace = append(trace, "funding_read")
	}
	return trace
}

func pairPaperFundingFrom(chain []pairFundingBatch, key pairEvidenceKey, anchorMs int64) int64 {
	from := anchorMs - 1
	for _, b := range chain {
		for _, c := range b.Coverage {
			if c.Key != key {
				continue
			}
			for _, iv := range c.Proven {
				if iv.FromMs <= from && iv.ThroughMs > from {
					from = iv.ThroughMs
				}
			}
		}
	}
	return from
}

func (r *pairReplayRuntime) paperEgress() *EgressConfig {
	if r.paper.egress == nil {
		return nil
	}
	return r.paper.egress.Egress
}

func (r *pairReplayRuntime) paperNextWakeAfter(nowMs int64) int64 {
	best := int64(1<<62 - 1)
	consider := func(t int64) {
		if t > nowMs && t < best {
			best = t
		}
	}
	c := r.paper.collection
	consider(r.nextManageMs)
	for _, t := range []int64{r.pollNextMs, r.fundingNextMs, r.capabilityNextMs} {
		if t > 0 && t <= c.DrainThroughMs {
			consider(t)
		}
	}
	consider(c.DrainThroughMs + 1)
	for _, m := range r.rt.Contract.pairMembersSorted() {
		if t, ok := r.paper.funding.NextVisibleMs(r.rt.Contract.pairMemberKey(m).evidenceKey(), nowMs); ok {
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

func (r *pairReplayRuntime) runPaper() error {
	for {
		if r.stopReason != "" {
			return nil
		}
		if r.resumeStep {
			if err := r.runStep(r.nextAt); err != nil {
				return err
			}
			r.processSteps++
			if why := r.rec.divergedReason(); why != "" {
				r.stopReason = why
				return nil
			}
			continue
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
		if r.paper.playback {
			if _, _, ok := r.rec.peekWakeAfterClear(); !ok {
				r.stopReason = fmt.Sprintf("the recording ends before the planned wake at %d", r.nextAt)
				return nil
			}
		} else if !r.rec.consuming() {
			for {
				wait := time.Until(time.UnixMilli(r.nextAt))
				if wait <= 0 {
					break
				}
				if wait > pairPaperWaitSlice {
					wait = pairPaperWaitSlice
				}
				time.Sleep(wait)
			}
		}
		if _, err := r.rec.clearAtBoundary(r.verifyRecordingForClear); err != nil {
			fmt.Fprintf(os.Stderr, "[paper] %v\n", err)
		}
		at, err := r.rec.wake("", r.cursor.Applied)
		if err != nil {
			r.stopReason = err.Error()
			return nil
		}
		if at > r.lt.EndMs {
			r.finished = true
			return nil
		}
		if err := r.runStep(at); err != nil {
			return err
		}
		r.processSteps++
		if why := r.rec.divergedReason(); why != "" {
			r.stopReason = why
			return nil
		}
	}
}

func (r *pairReplayRuntime) paperFrame(sc StrategyConfig, at time.Time) (pairPaperHistoryCandles, error) {
	m, ok := r.rt.Contract.pairMember(sc.ID)
	if !ok {
		return pairPaperHistoryCandles{}, fmt.Errorf("member %s is not in the contract", sc.ID)
	}
	return pairPaperCandles(context.Background(), r.rec, r.paperEgress(), m, r.paper.timeframes[sc.ID])
}

func (r *pairReplayRuntime) paperSnapshot(sc StrategyConfig, at time.Time, prices map[string]float64, frame pairPaperHistoryCandles) *marketSnapshot {
	atMs := at.UnixMilli()
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: fmt.Sprintf("paper/%s/manage/%d", r.spec.Run.RunID, atMs), ConfigGeneration: 1, SealedAt: at,
		keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{}, observations: map[feedObservationKey]*marketSnapshotObservation{}}
	tf := r.paper.timeframes[sc.ID]
	ivl, _ := hlCandleIntervalMs(tf)
	coin := hyperliquidSymbol(sc.Args)
	req := r.feedReq.Strategies[sc.ID]
	need := req.SignalLookback
	key := feedKeyFor(coin, tf)
	var bars []feedBar
	for _, b := range frame.Bars {
		open := int64(b[0])
		bars = append(bars, feedBar{OpenMs: open, CloseMs: open + ivl - 1, HasClose: true, Open: b[1], High: b[2], Low: b[3], Close: b[4], Volume: b[5], Source: "paper", RecvAt: time.UnixMilli(open + ivl).UTC()})
	}
	if len(frame.Forming) == 2 {
		open := int64(frame.Forming[0])
		px := frame.Forming[1]
		bars = append(bars, feedBar{OpenMs: open, CloseMs: open + ivl - 1, HasClose: false, Open: px, High: px, Low: px, Close: px, Source: "paper", RecvAt: at})
	}
	entry := &marketSnapshotKey{Key: key, IntervalMs: ivl, Bars: bars}
	closed := len(frame.Bars)
	ready := need > 0 && closed >= need
	entry.Readiness = feedKeyReadiness{Key: key, Bars: len(bars), Required: need, Ready: ready, Status: feedStatusReady, Source: "paper", LastRecvAt: at}
	if len(bars) > 0 {
		entry.Readiness.FirstOpenMs, entry.Readiness.LastOpenMs, entry.Readiness.LastCloseMs = bars[0].OpenMs, bars[len(bars)-1].OpenMs, bars[len(bars)-1].CloseMs
	}
	if !ready {
		entry.Readiness.Status, entry.Readiness.CoverageShort = feedStatusShort, true
		entry.Readiness.Detail = fmt.Sprintf("%d recorded closed bars at %s, below the %d the member check needs", closed, at.Format(time.RFC3339), need)
	}
	snap.keys[key] = entry
	for c, px := range prices {
		snap.mids[c] = feedMid{Px: px, RecvAt: at, Source: "paper"}
	}
	return snap
}

func (r *pairReplayRuntime) onCollectedPaper(rt *basketRuntime, c pairCollectedSlot) error {
	rec := r.pending
	rec.Collection = &pairReplayCollection{RunID: r.spec.Run.RunID, PairID: rt.Contract.BasketID, ContractFP: rt.Contract.Fingerprint(), SlotMs: c.SlotMs, StartMs: c.StartMs,
		DeadlineMs: c.DeadlineMs, CutoffMs: c.Envelope.CutoffMs, InputFP: c.Envelope.InputFP, EvidenceJSON: c.Envelope.EvidenceJSON, SnapshotID: c.Envelope.SnapshotID,
		Generation: c.Envelope.Generation, ObjectHashes: r.objectHashes()}
	if r.paper.playback {
		if err := r.file.updatePairReplayPending(r.spec.Run.RunID, rec); err != nil {
			return err
		}
		r.pending = rec
		r.fault("exit-after-collection")
		return nil
	}
	callID := r.paper.quotes.collectCall(c.SlotMs)
	if callID == "" {
		return fmt.Errorf("slot %d has no recorded collection call to bind", c.SlotMs)
	}
	head := r.rec.headNow()
	slot := pairRecSlotRow{PairID: rt.Contract.BasketID, SlotMs: c.SlotMs, CollectCallID: callID, InputFP: c.Envelope.InputFP, EnvelopeSHA: sha256Hex([]byte(c.Envelope.EvidenceJSON)),
		StartMs: c.StartMs, CutoffMs: c.Envelope.CutoffMs, DeadlineMs: c.DeadlineMs, PrefixSeq: head.Seq, PrefixHash: head.Prefix}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tx, err := r.file.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE basket_replay_steps SET record_json = ? WHERE run_id = ? AND step_index = ? AND status = ?`, string(b), r.spec.Run.RunID, rec.Ordinal, pairReplayStatusPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("replay step %d is not pending", rec.Ordinal)
	}
	if err := r.file.insertPairRecSlotTx(tx, r.spec.Run.RunID, slot); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.pending = rec
	r.fault("exit-after-collection")
	return nil
}

func pairPaperSlotCheck(file *StateDB, runID string, rt *basketRuntime) error {
	rows, err := readPairRecSlotRows(file.db, runID)
	if err != nil {
		return err
	}
	for _, s := range rows {
		rb := rt.Rebalances[s.SlotMs]
		if rb == nil || rb.EvidenceJSON == "" {
			continue
		}
		if rb.InputFP != s.InputFP || sha256Hex([]byte(rb.EvidenceJSON)) != s.EnvelopeSHA || rb.CutoffMs != s.CutoffMs {
			return fmt.Errorf("slot %d froze other evidence than its recorded collection %s", s.SlotMs, s.CollectCallID)
		}
	}
	return nil
}

func (r *pairReplayRuntime) paperPrefix(atMs, seq int64) string {
	return sha256Hex([]byte(strings.Join([]string{r.fp.Input, strconv.FormatInt(atMs, 10), strconv.FormatInt(seq, 10), r.rec.prefixAt(seq)}, "|")))
}

func (r *pairReplayRuntime) managePaperMember(sc StrategyConfig, risk map[RiskPartition]*scopeCycleRisk, at time.Time) {
	atMs := at.UnixMilli()
	mark, err := r.paper.quotes.mark(context.Background(), sc.ID, atMs)
	if err != nil {
		r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: sc.ID, AtMs: atMs, Cause: "no recorded own mark for the management check: " + err.Error()})
		return
	}
	frame, err := r.paperFrame(sc, at)
	if err != nil {
		r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: sc.ID, AtMs: atMs, Cause: "the management candle read: " + err.Error()})
		return
	}
	r.paperFrame0, r.lastNativeCall = frame, ""
	if sc.Platform == "okx" {
		r.manageOKX(sc, risk, at, mark, false)
	} else {
		r.manageHL(sc, risk, at, mark, false)
	}
	r.mu.RLock()
	echo := r.rt.pairEchoFailures[sc.ID]
	r.mu.RUnlock()
	if echo == "" {
		return
	}
	r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: sc.ID, AtMs: atMs, Cause: echo, CallID: r.lastNativeCall})
}

func (r *pairReplayRuntime) durablePairCounts() (int, int) {
	var slots, actions int
	part := r.rt.Partition
	_ = r.file.db.QueryRow(`SELECT COUNT(*) FROM basket_rebalances WHERE scope = ? AND source = ? AND basket_id = ?`, string(part.Scope), part.Source, r.rt.Contract.BasketID).Scan(&slots)
	_ = r.file.db.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE scope = ? AND source = ? AND basket_id = ?`, string(part.Scope), part.Source, r.rt.Contract.BasketID).Scan(&actions)
	return slots, actions
}
