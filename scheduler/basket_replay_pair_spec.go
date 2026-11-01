package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pairReplayProtocolVersion = 2
	pairReplayManifestVersion = 2
	pairReplayMaxObjectBytes  = 1 << 30
	pairReplayMaxSteps        = 2000000
	pairReplayObjectVersion   = 1
	pairReplayReceiptModel    = "recorded_first_receipt"
	pairReplayKindEvidence    = "evidence"
	pairReplayKindFunding     = "funding"
	pairReplayKindQuotes      = "quotes"
	pairReplayKindBooks       = "books"
	pairReplayKindCandles     = "candles"
	pairReplayFailSubmit      = "submit_transport_error"
	pairReplayFailPolicy      = "policy_process_failure"
	pairReplayFailManagement  = "management_check_failure"
	pairReplayFailSave        = "save_failure"
	pairReplayEventOrder      = "one host cycle per wake time; at one time every frozen record with first receipt (quotes), receive time (books), visibility (funding) or eligibility start (evidence) at or before the cycle time is visible together, in recorded order per object; objects are ordered by kind (evidence, funding, books, quotes, candles), then member id; the cycle runs evidence delivery, coordinator funding, preparePairRisk, partition risk with the paper kill switch, native member management on a management cycle (Hyperliquid then OKX by member id), the coordinator step, one full save that commits the step record and the cursor, then the next wake from fresh time"
	pairReplayClockNote       = "one simulated clock drives every trading, valuation, venue, funding, risk, decision, action and management time; collection, valuation and policy budgets are wall-clock limits that never advance it; the storage audit columns keep the system clock and no decision reads them"
	pairTargetModePolicy      = "policy"
	pairTargetModeNoTrade     = "no_trade"
	pairTargetModeFrozen      = "frozen_schedule"
	pairTargetHoldNoTrade     = "target_input no_trade: the slot holds and management continues"
	pairTargetHoldMissing     = "target_input frozen_schedule: the slot has no entry and the candidate policy is not a fallback"
	pairTargetHoldSatisfied   = "target_input: the scheduled targets are already satisfied"
	pairTargetHoldConflict    = "target_input: the lifecycle conflicts with the scheduled targets and the schedule does not force a transition"
	pairTargetHoldGeometry    = "target_input: nonzero targets need opposite signs and equal absolute base quantity"
)

var pairReplayAdministrativeFields = []string{
	"entry intent ids (intent_id, every foreign reference): first-appearance ordinal per kind",
	"trade position ids (position_id, closed_position_id, inventory position identity, staged paper close position_id): first-appearance ordinal per kind",
	"trade row ids (trade_row_id): first-appearance ordinal per kind",
	"pair progress seq (progress_json seq): excluded",
	"storage audit columns (entry intent updated_at, basket_replay_runs.created_at_ms, run paths): excluded",
	"replay step ids and run state paths: excluded",
}

var pairReplaySHAPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var pairReplayObjectPathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

var pairReplayResearchRefusedKeys = []string{"db_file", "log_dir", "paper_db_file", "paper_sources", "market_feed", "egress", "baskets"}

type pairReplaySpec struct {
	Protocol       string                  `json:"protocol"`
	Version        int                     `json:"version"`
	Mode           string                  `json:"mode"`
	Run            basketReplayRunSpec     `json:"run"`
	ResearchConfig json.RawMessage         `json:"research_config"`
	InputManifest  pairReplayInputManifest `json:"input_manifest"`
	Scenario       pairReplayScenario      `json:"scenario"`
	Bounds         pairReplayBounds        `json:"bounds"`
	Recording      *pairReplayRecordingRef `json:"recording,omitempty"`
	TargetInput    *pairReplayTargetInput  `json:"target_input,omitempty"`
}

type pairReplayTargetInput struct {
	Mode     string          `json:"mode"`
	Schedule json.RawMessage `json:"schedule,omitempty"`
	slots    map[int64]map[string]pairPolicyMemberTarget
}

type pairReplayInputManifest struct {
	Root    string                `json:"root"`
	Objects []pairReplayObjectRef `json:"objects"`
}

type pairReplayObjectRef struct {
	Kind      string `json:"kind"`
	MemberID  string `json:"member_id,omitempty"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type pairReplayScenario struct {
	SubmitLatencyMs   int64                     `json:"submit_latency_ms"`
	ResponseLatencyMs int64                     `json:"response_latency_ms"`
	ReceiptModel      string                    `json:"receipt_model"`
	VenueFailures     []pairReplayFailureScript `json:"venue_failures"`
}

type pairReplayFailureScript struct {
	Kind     string `json:"kind"`
	Purpose  string `json:"purpose,omitempty"`
	Nth      int    `json:"nth,omitempty"`
	MemberID string `json:"member_id,omitempty"`
	At       string `json:"at,omitempty"`
	atMs     int64
}

type pairReplayBounds struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	MaxSteps int    `json:"max_steps"`
}

type pairReplayLifetime struct {
	StartMs  int64 `json:"start_ms"`
	EndMs    int64 `json:"end_ms"`
	MaxSteps int   `json:"max_steps"`
}

func (l pairReplayLifetime) String() string {
	return fmt.Sprintf("%d..%d/%d", l.StartMs, l.EndMs, l.MaxSteps)
}

type pairReplayQuoteDoc struct {
	V        int                     `json:"v"`
	MemberID string                  `json:"member_id"`
	Records  []pairReplayQuoteRecord `json:"records"`
}

type pairReplayQuoteRecord struct {
	ReceiptMs int64  `json:"receipt_ms"`
	Payload   []byte `json:"payload"`
}

type pairReplayBookDoc struct {
	V                 int                      `json:"v"`
	MemberID          string                   `json:"member_id"`
	Instance          string                   `json:"instance"`
	CoverageFromMs    int64                    `json:"coverage_from_ms"`
	CoverageThroughMs int64                    `json:"coverage_through_ms"`
	Records           []pairReplayBookRecord   `json:"records"`
	Markers           []pairReplayBookMarkerIn `json:"markers"`
}

type pairReplayBookRecord struct {
	EventMs int64           `json:"event_ms"`
	RecvMs  int64           `json:"recv_ms"`
	Bids    []feedBookLevel `json:"bids"`
	Asks    []feedBookLevel `json:"asks"`
}

type pairReplayBookMarkerIn struct {
	AtMs   int64  `json:"at_ms"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type pairReplayCandleDoc struct {
	V         int         `json:"v"`
	MemberID  string      `json:"member_id"`
	Timeframe string      `json:"timeframe"`
	Rows      [][]float64 `json:"rows"`
}

type pairReplayInputs struct {
	evidence    []byte
	funding     []byte
	quotes      map[string]pairReplayQuoteDoc
	books       map[string]pairReplayBookDoc
	candles     map[string][]feedBar
	timeframes  map[string]string
	objects     []pairReplayObjectRef
	recordTotal map[string]int
}

func pairReplayPeekVersion(raw []byte) (string, int, error) {
	var head struct {
		Protocol string `json:"protocol"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", 0, refuseReplay("spec_malformed", err.Error())
	}
	return head.Protocol, head.Version, nil
}

func decodePairReplaySpec(raw []byte) (*pairReplaySpec, error) {
	var probe struct {
		TargetInput json.RawMessage `json:"target_input"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	if bytes.Equal(bytes.TrimSpace(probe.TargetInput), []byte("null")) {
		return nil, refuseReplay("spec_invalid", "target_input must be an object when present; null is refused")
	}
	if len(bytes.TrimSpace(probe.TargetInput)) > 0 {
		if _, err := pairStrictParse(raw); err != nil {
			return nil, refuseReplay("spec_malformed", "a specification with target_input must be one strict JSON object (duplicate keys, invalid text and trailing data are refused): "+err.Error())
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec pairReplaySpec
	if err := dec.Decode(&spec); err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	if dec.More() {
		return nil, refuseReplay("spec_malformed", "trailing data after the replay specification")
	}
	return &spec, nil
}

func pairReplayParseTime(label, s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not an RFC 3339 time", label, s)
	}
	return t.UTC().UnixMilli(), nil
}

func validatePairReplaySpec(spec *pairReplaySpec) (pairReplayLifetime, error) {
	var lt pairReplayLifetime
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if spec.Protocol != basketReplayProtocolName || spec.Version != pairReplayProtocolVersion {
		return lt, refuseReplay("protocol_unsupported", fmt.Sprintf("the specification names protocol %q version %d; the pair replay reads %q version %d", spec.Protocol, spec.Version, basketReplayProtocolName, pairReplayProtocolVersion))
	}
	if spec.Mode != basketReplayModeNew && spec.Mode != basketReplayModeResume {
		add("mode %q must be %q or %q", spec.Mode, basketReplayModeNew, basketReplayModeResume)
	}
	if !basketReplayRunIDPattern.MatchString(spec.Run.RunID) {
		add("run.run_id %q must match %s", spec.Run.RunID, basketReplayRunIDPattern.String())
	}
	if strings.TrimSpace(spec.Run.StateDir) == "" || !filepath.IsAbs(spec.Run.StateDir) || filepath.Clean(spec.Run.StateDir) != spec.Run.StateDir {
		add("run.state_dir %q must be an absolute, clean path", spec.Run.StateDir)
	}
	if len(spec.Run.Source) == 0 {
		add("run.source must record the frozen data provenance")
	}
	if len(bytes.TrimSpace(spec.ResearchConfig)) == 0 {
		add("research_config is required")
	} else {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(spec.ResearchConfig, &root); err != nil {
			add("research_config must be one JSON object: %v", err)
		} else {
			for _, k := range pairReplayResearchRefusedKeys {
				if _, ok := root[k]; ok {
					add("research_config sets %q; the replay owns storage, logs, routing, feeds and egress", k)
				}
			}
		}
	}
	m := spec.InputManifest
	if strings.TrimSpace(m.Root) == "" || !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root {
		add("input_manifest.root %q must be an absolute, clean path", m.Root)
	} else if rel, err := filepath.Rel(spec.Run.StateDir, m.Root); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		add("input_manifest.root %q must be outside the run state directory", m.Root)
	} else if rel, err := filepath.Rel(m.Root, spec.Run.StateDir); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		add("the run state directory must not be inside input_manifest.root")
	}
	kinds := map[string]int{}
	seen := map[string]bool{}
	for i, o := range m.Objects {
		label := fmt.Sprintf("input_manifest.objects[%d]", i)
		switch o.Kind {
		case pairReplayKindEvidence, pairReplayKindFunding:
			if o.MemberID != "" {
				add("%s kind %s is pair-wide and names no member", label, o.Kind)
			}
		case pairReplayKindQuotes, pairReplayKindBooks, pairReplayKindCandles:
			if strings.TrimSpace(o.MemberID) == "" {
				add("%s kind %s needs a member_id", label, o.Kind)
			}
		default:
			add("%s kind %q must be evidence, funding, quotes, books or candles", label, o.Kind)
		}
		key := o.Kind + "|" + o.MemberID
		if seen[key] {
			add("%s repeats kind %s for member %q", label, o.Kind, o.MemberID)
		}
		seen[key] = true
		kinds[o.Kind]++
		if !pairReplayObjectPathPattern.MatchString(o.Path) || strings.Contains(o.Path, "..") {
			add("%s path %q must be a clean relative path inside the input root", label, o.Path)
		}
		if !pairReplaySHAPattern.MatchString(o.SHA256) {
			add("%s sha256 %q must be 64 lowercase hex characters", label, o.SHA256)
		}
		if o.SizeBytes <= 0 || o.SizeBytes > pairReplayMaxObjectBytes {
			add("%s size_bytes %d must be in [1, %d]", label, o.SizeBytes, pairReplayMaxObjectBytes)
		}
	}
	if kinds[pairReplayKindEvidence] != 1 || kinds[pairReplayKindFunding] != 1 {
		add("input_manifest needs exactly one evidence and one funding object")
	}
	sc := spec.Scenario
	if sc.SubmitLatencyMs < 0 || sc.SubmitLatencyMs > pairVirtualMaxLatencyMs || sc.ResponseLatencyMs < 0 || sc.ResponseLatencyMs > pairVirtualMaxLatencyMs {
		add("scenario latencies must be in [0, %d] ms", pairVirtualMaxLatencyMs)
	}
	if sc.ReceiptModel != pairReplayReceiptModel {
		add("scenario.receipt_model %q must be %q: every record keeps its recorded first receipt", sc.ReceiptModel, pairReplayReceiptModel)
	}
	if sc.VenueFailures == nil {
		add("scenario.venue_failures must be present (an empty list declares none)")
	}
	for i := range sc.VenueFailures {
		f := &spec.Scenario.VenueFailures[i]
		label := fmt.Sprintf("scenario.venue_failures[%d]", i)
		switch f.Kind {
		case pairReplayFailSubmit:
			if (f.Purpose != pairPurposeEntry && f.Purpose != pairPurposeExit && f.Purpose != pairPurposeProtective) || f.Nth <= 0 || f.MemberID != "" || f.At != "" {
				add("%s %s needs purpose pair_entry, pair_exit or pair_protective and a positive nth only", label, f.Kind)
			}
		case pairReplayFailPolicy, pairReplayFailSave, pairReplayFailManagement:
			ms, err := pairReplayParseTime(label+".at", f.At)
			if err != nil {
				add("%v", err)
			}
			f.atMs = ms
			if f.Purpose != "" || f.Nth != 0 {
				add("%s %s takes at%s only", label, f.Kind, map[bool]string{true: " and member_id", false: ""}[f.Kind == pairReplayFailManagement])
			}
			if (f.Kind == pairReplayFailManagement) != (f.MemberID != "") {
				add("%s member_id is required for %s and refused otherwise", label, pairReplayFailManagement)
			}
		default:
			add("%s kind %q must be %s, %s, %s or %s", label, f.Kind, pairReplayFailSubmit, pairReplayFailPolicy, pairReplayFailManagement, pairReplayFailSave)
		}
	}
	start, err := pairReplayParseTime("bounds.start", spec.Bounds.Start)
	if err != nil {
		add("%v", err)
	}
	end, err2 := pairReplayParseTime("bounds.end", spec.Bounds.End)
	if err2 != nil {
		add("%v", err2)
	}
	if err == nil && err2 == nil && (start <= 0 || end < start) {
		add("bounds.start must be positive and bounds.end must not be before it")
	}
	if spec.Bounds.MaxSteps <= 0 || spec.Bounds.MaxSteps > pairReplayMaxSteps {
		add("bounds.max_steps %d must be in [1, %d]", spec.Bounds.MaxSteps, pairReplayMaxSteps)
	}
	boundsOK := err == nil && err2 == nil && start > 0 && end >= start
	if spec.Recording != nil {
		errs = append(errs, pairValidateRecordingRef(spec.Recording)...)
		if len(sc.VenueFailures) != 0 {
			add("scenario.venue_failures must be empty with a recording: the recorded outcomes are the only failures a playback reproduces")
		}
	}
	if spec.TargetInput != nil {
		errs = append(errs, pairValidateTargetInput(spec, start, end, boundsOK)...)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return lt, refuseReplay("spec_invalid", errs...)
	}
	return pairReplayLifetime{StartMs: start, EndMs: end, MaxSteps: spec.Bounds.MaxSteps}, nil
}

func pairReplayInputFingerprint(raw []byte) (string, error) {
	var generic map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return "", err
	}
	delete(generic, "mode")
	if run, ok := generic["run"].(map[string]interface{}); ok {
		delete(run, "state_dir")
	}
	if man, ok := generic["input_manifest"].(map[string]interface{}); ok {
		delete(man, "root")
	}
	canon, err := json.Marshal(generic)
	if err != nil {
		return "", err
	}
	return sha256Hex(canon), nil
}

func pairReplayStrictDecode(blob []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after the object")
	}
	return nil
}

func pairReplayReadObject(root string, o pairReplayObjectRef) ([]byte, error) {
	path := filepath.Join(root, filepath.FromSlash(o.Path))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() != o.SizeBytes {
		return nil, fmt.Errorf("%s is %d bytes, the manifest freezes %d", path, info.Size(), o.SizeBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blob, err := io.ReadAll(io.LimitReader(f, pairReplayMaxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(blob)) != o.SizeBytes {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	if got := sha256Hex(blob); got != o.SHA256 {
		return nil, fmt.Errorf("%s has sha256 %s, the manifest freezes %s", path, got, o.SHA256)
	}
	return blob, nil
}

func loadPairReplayInputs(spec *pairReplaySpec) (*pairReplayInputs, error) {
	in := &pairReplayInputs{quotes: map[string]pairReplayQuoteDoc{}, books: map[string]pairReplayBookDoc{}, candles: map[string][]feedBar{}, timeframes: map[string]string{}, recordTotal: map[string]int{}}
	var errs []string
	objs := append([]pairReplayObjectRef(nil), spec.InputManifest.Objects...)
	rank := map[string]int{pairReplayKindEvidence: 0, pairReplayKindFunding: 1, pairReplayKindBooks: 2, pairReplayKindQuotes: 3, pairReplayKindCandles: 4}
	sort.Slice(objs, func(i, j int) bool {
		if rank[objs[i].Kind] != rank[objs[j].Kind] {
			return rank[objs[i].Kind] < rank[objs[j].Kind]
		}
		return objs[i].MemberID < objs[j].MemberID
	})
	in.objects = objs
	recordedBooks := spec.Recording != nil && spec.Recording.Version == pairRecordingRefBooksVersion
	for _, o := range objs {
		label := fmt.Sprintf("%s object %s", o.Kind, o.Path)
		if spec.Recording != nil && spec.Recording.Version == pairRecordingRefBooksVersion && o.Kind == pairReplayKindBooks && o.SizeBytes > pairPaperBookObjectLimit {
			errs = append(errs, label+": family-4 book object exceeds its byte limit")
			continue
		}
		blob, err := pairReplayReadObject(spec.InputManifest.Root, o)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		key := o.Kind + "|" + o.MemberID
		switch o.Kind {
		case pairReplayKindEvidence:
			in.evidence = blob
			var d pairEvidenceDocument
			if json.Unmarshal(blob, &d) == nil {
				in.recordTotal[key] = len(d.Capabilities) + len(d.Accounts) + len(d.Conversions)
			}
		case pairReplayKindFunding:
			in.funding = blob
			d, err := pairDecodeFundingDocument(blob)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			in.recordTotal[key] = len(d.Rates) + len(d.Prices) + len(d.Coverage)
			if d.Research != nil {
				in.recordTotal[key] += len(d.Research.Observations)
			}
		case pairReplayKindQuotes:
			var d pairReplayQuoteDoc
			if err := pairReplayStrictDecode(blob, &d); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			if why := d.structural(o.MemberID); why != "" && !(recordedBooks && len(d.Records) == 0 && d.V == pairReplayObjectVersion && d.MemberID == o.MemberID) {
				errs = append(errs, fmt.Sprintf("%s: %s", label, why))
				continue
			}
			in.quotes[o.MemberID] = d
			in.recordTotal[key] = len(d.Records)
		case pairReplayKindBooks:
			var d pairReplayBookDoc
			if err := pairReplayStrictDecode(blob, &d); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			if why := d.structural(o.MemberID); why != "" {
				errs = append(errs, fmt.Sprintf("%s: %s", label, why))
				continue
			}
			in.books[o.MemberID] = d
			in.recordTotal[key] = len(d.Records) + len(d.Markers)
		case pairReplayKindCandles:
			var d pairReplayCandleDoc
			if err := pairReplayStrictDecode(blob, &d); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			ivl, ok := hlCandleIntervalMs(d.Timeframe)
			if d.V != pairReplayObjectVersion || d.MemberID != o.MemberID || !ok {
				errs = append(errs, fmt.Sprintf("%s: version must be %d, member_id must be %s and timeframe must be a candle interval", label, pairReplayObjectVersion, o.MemberID))
				continue
			}
			if recordedBooks && len(d.Rows) == 0 {
				in.candles[o.MemberID] = []feedBar{}
				in.timeframes[o.MemberID] = d.Timeframe
				in.recordTotal[key] = 0
				continue
			}
			bars, why := parseBasketReplayCandles(d.Rows, ivl)
			if why != "" {
				errs = append(errs, fmt.Sprintf("%s: %s", label, why))
				continue
			}
			in.candles[o.MemberID] = bars
			in.timeframes[o.MemberID] = d.Timeframe
			in.recordTotal[key] = len(bars)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, refuseReplay("input_refused", errs...)
	}
	return in, nil
}

func (d pairReplayQuoteDoc) structural(member string) string {
	if d.V != pairReplayObjectVersion || d.MemberID != member {
		return fmt.Sprintf("version must be %d and member_id must be %s", pairReplayObjectVersion, member)
	}
	if len(d.Records) == 0 {
		return "no recorded quotes"
	}
	for i, r := range d.Records {
		if r.ReceiptMs <= 0 || len(r.Payload) == 0 {
			return fmt.Sprintf("record %d needs a positive receipt time and a payload", i)
		}
		if i > 0 && r.ReceiptMs < d.Records[i-1].ReceiptMs {
			return fmt.Sprintf("record %d receipt %d is before the previous record's %d; recorded order is receipt order", i, r.ReceiptMs, d.Records[i-1].ReceiptMs)
		}
	}
	return ""
}

func (d pairReplayBookDoc) structural(member string) string {
	if d.V != pairReplayObjectVersion || d.MemberID != member {
		return fmt.Sprintf("version must be %d and member_id must be %s", pairReplayObjectVersion, member)
	}
	if !pairPrintableASCII(d.Instance, 128) || d.Instance == "" {
		return "instance must be printable ASCII of at most 128 bytes"
	}
	if len(d.Records) == 0 || d.CoverageFromMs <= 0 || d.CoverageThroughMs < d.CoverageFromMs {
		return "a book stream needs records and a coverage interval"
	}
	for i, r := range d.Records {
		if r.RecvMs < d.CoverageFromMs || r.RecvMs > d.CoverageThroughMs || r.EventMs <= 0 {
			return fmt.Sprintf("record %d receive time %d lies outside the declared coverage", i, r.RecvMs)
		}
		if i > 0 && r.RecvMs <= d.Records[i-1].RecvMs {
			return fmt.Sprintf("record %d receive time %d is not after the previous record's", i, r.RecvMs)
		}
		if len(r.Bids) == 0 || len(r.Asks) == 0 {
			return fmt.Sprintf("record %d needs both sides", i)
		}
		for _, lv := range append(append([]feedBookLevel(nil), r.Bids...), r.Asks...) {
			if !pairDecimalCanonical(lv.Px) || !pairDecimalCanonical(lv.Sz) {
				return fmt.Sprintf("record %d level %+v is not canonical", i, lv)
			}
		}
	}
	for i, mk := range d.Markers {
		if mk.AtMs < d.CoverageFromMs || mk.AtMs > d.CoverageThroughMs || strings.TrimSpace(mk.Reason) == "" {
			return fmt.Sprintf("marker %d must name a reason inside the coverage", i)
		}
	}
	return ""
}

func pairReplayFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func pairTargetInputSHA(ti *pairReplayTargetInput) string {
	if ti == nil {
		return ""
	}
	blob, err := json.Marshal(struct {
		Mode     string          `json:"mode"`
		Schedule json.RawMessage `json:"schedule,omitempty"`
	}{Mode: ti.Mode, Schedule: ti.Schedule})
	if err != nil {
		return ""
	}
	return sha256Hex(blob)
}

func pairValidateTargetInput(spec *pairReplaySpec, start, end int64, boundsOK bool) []string {
	ti := spec.TargetInput
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if spec.Recording != nil {
		add("target_input is refused with a recording: sealed playback reproduces the recorded policy and takes no target override")
	}
	switch ti.Mode {
	case pairTargetModePolicy, pairTargetModeNoTrade:
		if len(bytes.TrimSpace(ti.Schedule)) != 0 {
			add("target_input mode %s carries no schedule", ti.Mode)
		}
	case pairTargetModeFrozen:
		if len(bytes.TrimSpace(ti.Schedule)) == 0 || bytes.Equal(bytes.TrimSpace(ti.Schedule), []byte("null")) {
			add("target_input mode frozen_schedule needs a schedule object")
			break
		}
		members, cadence, why := pairTargetConfiguredMembers(spec.ResearchConfig)
		if why != "" {
			add("%s", why)
			break
		}
		slots, slotErrs := pairParseTargetSchedule(ti.Schedule, members, cadence, start, end, boundsOK)
		errs = append(errs, slotErrs...)
		if len(slotErrs) == 0 {
			ti.slots = slots
		}
	default:
		add("target_input.mode %q must be policy, no_trade or frozen_schedule", ti.Mode)
	}
	return errs
}

func pairTargetConfiguredMembers(raw json.RawMessage) (ids []string, cadence int64, why string) {
	var root struct {
		Pairs []struct {
			Members []struct {
				StrategyID string `json:"strategy_id"`
			} `json:"members"`
			Limits struct {
				DecisionCadenceMs json.RawMessage `json:"decision_cadence_ms"`
			} `json:"limits"`
		} `json:"pairs"`
	}
	if json.Unmarshal(raw, &root) != nil || len(root.Pairs) != 1 {
		return nil, 0, "target_input needs research_config to name exactly one pair"
	}
	seen := map[string]bool{}
	for _, m := range root.Pairs[0].Members {
		if m.StrategyID == "" || seen[m.StrategyID] {
			return nil, 0, "target_input needs two distinct configured member ids"
		}
		seen[m.StrategyID] = true
		ids = append(ids, m.StrategyID)
	}
	sort.Strings(ids)
	if len(ids) != 2 {
		return nil, 0, "target_input needs exactly the two configured members"
	}
	cadence, ok := pairRawInteger(root.Pairs[0].Limits.DecisionCadenceMs)
	if !ok || cadence <= 0 {
		return nil, 0, "target_input needs limits.decision_cadence_ms as a positive integer"
	}
	return ids, cadence, ""
}

func pairCanonicalSlotMillis(s string) (int64, bool) {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

func pairSlotOverlapsLifetime(slot, cadence, start, end int64) bool {
	if cadence <= 0 || slot <= 0 || slot%cadence != 0 || end < start {
		return false
	}
	next, ok := pairAddMs(slot, cadence)
	if !ok {
		return false
	}
	return slot < end && next > start
}

func pairTargetNumber(v interface{}) (float64, bool) {
	n, ok := v.(pairJSONNumber)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || !pairReplayFinite(f) {
		return 0, false
	}
	return f, true
}

func pairParseTargetSchedule(raw json.RawMessage, members []string, cadence, start, end int64, boundsOK bool) (map[int64]map[string]pairPolicyMemberTarget, []string) {
	tree, err := pairStrictParse(raw)
	if err != nil {
		return nil, []string{fmt.Sprintf("target_input.schedule is not strict JSON: %v", err)}
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return nil, []string{"target_input.schedule must be an object"}
	}
	want := map[string]bool{}
	for _, id := range members {
		want[id] = true
	}
	var errs []string
	slots := map[int64]map[string]pairPolicyMemberTarget{}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	memberList := strings.Join(members, ", ")
	for _, key := range keys {
		slot, canon := pairCanonicalSlotMillis(key)
		if !canon {
			errs = append(errs, fmt.Sprintf("target_input.schedule key %q is not a canonical positive decimal millisecond time", key))
			continue
		}
		if !boundsOK || !pairSlotOverlapsLifetime(slot, cadence, start, end) {
			errs = append(errs, fmt.Sprintf("target_input.schedule slot %s is not a %d ms cadence slot inside the replay lifetime", key, cadence))
		}
		leg, isObj := obj[key].(map[string]interface{})
		if !isObj || len(leg) != len(want) {
			errs = append(errs, fmt.Sprintf("target_input.schedule[%s] must name exactly the configured members %s", key, memberList))
			continue
		}
		targets := map[string]pairPolicyMemberTarget{}
		ids := make([]string, 0, len(leg))
		for id := range leg {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !want[id] {
				errs = append(errs, fmt.Sprintf("target_input.schedule[%s] names unknown member %q", key, id))
				continue
			}
			body, bodyOK := leg[id].(map[string]interface{})
			if !bodyOK || len(body) != 2 {
				errs = append(errs, fmt.Sprintf("target_input.schedule[%s].%s must contain exactly target_base_qty and price_bound", key, id))
				continue
			}
			qty, qok := pairTargetNumber(body["target_base_qty"])
			px, pok := pairTargetNumber(body["price_bound"])
			if _, has := body["target_base_qty"]; !has || !qok {
				errs = append(errs, fmt.Sprintf("target_input.schedule[%s].%s target_base_qty must be a finite JSON number", key, id))
			}
			if _, has := body["price_bound"]; !has || !pok || !(px > 0) {
				errs = append(errs, fmt.Sprintf("target_input.schedule[%s].%s price_bound must be a finite JSON number above zero", key, id))
			}
			for field := range body {
				if field != "target_base_qty" && field != "price_bound" {
					errs = append(errs, fmt.Sprintf("target_input.schedule[%s].%s has unknown field %q", key, id, field))
				}
			}
			if qok && pok && px > 0 {
				targets[id] = pairPolicyMemberTarget{TargetBaseQty: qty, PriceBound: px}
			}
		}
		if len(targets) == len(want) {
			slots[slot] = targets
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return slots, nil
}
