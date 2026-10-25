package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	pairObservationVersion = 2
	pairEnvelopeVersion    = 2

	pairContractHLSealed  = feedBookSource
	pairContractHLArchive = "hyperliquid_archive_l2_v1"
	pairContractHLPublic  = "hyperliquid_public_l2_snapshot_v1"
	pairContractOKXPublic = "okx_public_books_snapshot_v1"

	pairTimeSemanticsHLPublic   = "hyperliquid_l2_time_ms+reader_receipt_ms/1871-v1"
	pairTimeSemanticsOKXPublic  = "okx_books_ts_ms+reader_receipt_ms/1871-v1"
	pairTimeSemanticsArchive    = "archiver_receipt_ms+modeled_delivery_ms/1871-v1"
	pairIdentitySemanticsPublic = "sha256(canonical_payload)/1871-v1"
	pairArchivePayloadTimeBasis = "archiver_receipt_ns_floor_ms/1635-v1"

	pairHLPublicHost  = "https://api.hyperliquid.xyz"
	pairOKXPublicHost = "https://www.okx.com"

	pairCoverageComplete   = "complete"
	pairCoverageIncomplete = "incomplete"
	pairGapSilence         = "silence"

	pairSnapshotIDMaxLen = 256
	pairFeedStatusMaxLen = 64
)

var pairDecimalTextPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
var pairOrderCountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})$`)
var pairVenueMsPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

type pairSourceSpec struct {
	Provider          string
	Source            string
	Contract          string
	TimeSemantics     string
	IdentitySemantics string
	Point             bool
}

var pairSourceSpecs = []pairSourceSpec{
	{Provider: entryProviderHyperliquid, Source: pairSourceSealedFeed, Contract: pairContractHLSealed, TimeSemantics: feedBookTimeSemantics, IdentitySemantics: feedBookIdentitySemantics},
	{Provider: entryProviderHyperliquid, Source: pairSourceArchive, Contract: pairContractHLArchive, TimeSemantics: pairTimeSemanticsArchive, IdentitySemantics: feedBookIdentitySemantics},
	{Provider: entryProviderHyperliquid, Source: pairSourcePublicSnapshot, Contract: pairContractHLPublic, TimeSemantics: pairTimeSemanticsHLPublic, IdentitySemantics: pairIdentitySemanticsPublic, Point: true},
	{Provider: entryProviderOKX, Source: pairSourcePublicSnapshot, Contract: pairContractOKXPublic, TimeSemantics: pairTimeSemanticsOKXPublic, IdentitySemantics: pairIdentitySemanticsPublic, Point: true},
}

func pairSourceSpecFor(provider, source string) (pairSourceSpec, bool) {
	for _, s := range pairSourceSpecs {
		if s.Provider == provider && s.Source == source {
			return s, true
		}
	}
	return pairSourceSpec{}, false
}

func pairSourceContractFor(provider, source string) string {
	s, _ := pairSourceSpecFor(provider, source)
	return s.Contract
}

func pairSourceContractMap(members []pairContractMember) map[string]string {
	out := make(map[string]string, len(members))
	for _, m := range members {
		out[m.MemberID] = pairSourceContractFor(m.Provider, m.Source)
	}
	return out
}

func (n pairBookNeed) validate() error {
	switch {
	case n.WindowMs <= 0 || n.WindowMs > feedBookMaxWindowMs:
		return fmt.Errorf("book window %dms is outside (0, %d]", n.WindowMs, feedBookMaxWindowMs)
	case n.Capacity <= 0 || n.Capacity > feedBookMaxObservations:
		return fmt.Errorf("book capacity %d observations is outside [1, %d]", n.Capacity, feedBookMaxObservations)
	case n.Depth <= 0 || n.Depth > feedBookMaxLevels:
		return fmt.Errorf("book depth %d levels is outside [1, %d]", n.Depth, feedBookMaxLevels)
	}
	return nil
}

func pairFeedBookNeeds(rt *basketRuntime) map[feedObservationKey]feedBookNeed {
	out := map[feedObservationKey]feedBookNeed{}
	if !rt.isPair() {
		return out
	}
	need := rt.Contract.Pair.BookNeed
	for _, m := range rt.Contract.pairMembersSorted() {
		if m.Provider == entryProviderHyperliquid && m.Source == pairSourceSealedFeed {
			addBookNeed(out, l2BookKeyFor(m.Instrument), contractBookNeed(need.WindowMs, need.Capacity, need.Depth), nil)
		}
	}
	return out
}

type pairNativeLevel struct {
	Px  string `json:"px"`
	Qty string `json:"qty"`
}

type pairBaseLevel struct {
	Px      string `json:"px"`
	BaseQty string `json:"base_qty"`
}

type pairCoverageGap struct {
	FromMs    int64  `json:"from_ms"`
	ThroughMs int64  `json:"through_ms"`
	Reason    string `json:"reason"`
}

type pairCoverage struct {
	SourceContract    string            `json:"source_contract"`
	TimeSemantics     string            `json:"time_semantics"`
	IdentitySemantics string            `json:"identity_semantics"`
	FromMs            int64             `json:"from_ms"`
	ThroughMs         int64             `json:"through_ms"`
	RequiredDepth     int               `json:"required_depth"`
	RequiredWindowMs  int64             `json:"required_window_ms"`
	RequiredCapacity  int               `json:"required_capacity"`
	PrefixComplete    bool              `json:"prefix_complete"`
	Status            string            `json:"status"`
	Gaps              []pairCoverageGap `json:"gaps"`
}

type pairObservation struct {
	V                 int               `json:"v"`
	Key               pairEvidenceKey   `json:"key"`
	Source            string            `json:"source"`
	Provenance        string            `json:"provenance"`
	VenueTimeMs       *int64            `json:"venue_time_ms"`
	FirstReceiptMs    int64             `json:"first_receipt_ms"`
	ModeledDeliveryMs int64             `json:"modeled_delivery_ms"`
	SourceCutoffMs    int64             `json:"source_cutoff_ms"`
	SnapshotID        string            `json:"snapshot_id"`
	Generation        uint64            `json:"generation"`
	Coverage          pairCoverage      `json:"coverage"`
	NativeBids        []pairNativeLevel `json:"native_bids"`
	NativeAsks        []pairNativeLevel `json:"native_asks"`
	Bids              []pairBaseLevel   `json:"bids"`
	Asks              []pairBaseLevel   `json:"asks"`
	NativeUnit        string            `json:"native_unit"`
	ContractSize      string            `json:"contract_size"`
	PriceCurrency     string            `json:"price_currency"`
	NormalizationID   string            `json:"normalization_id"`
	Payload           []byte            `json:"payload"`
	PayloadHash       string            `json:"payload_hash"`
}

type pairEnvelopeMember struct {
	Key         pairEvidenceKey          `json:"key"`
	Observation pairObservation          `json:"observation"`
	Capability  pairCapabilityEvidence   `json:"capability"`
	Conversions []pairConversionEvidence `json:"conversions"`
	Account     pairAccountEvidence      `json:"account"`
}

type pairQuoteEnvelope struct {
	V                   int                  `json:"v"`
	Partition           string               `json:"partition"`
	PairID              string               `json:"pair_id"`
	ContractFingerprint string               `json:"contract_fingerprint"`
	SlotMs              int64                `json:"slot_ms"`
	StartMs             int64                `json:"start_ms"`
	DeadlineMs          int64                `json:"deadline_ms"`
	CutoffMs            int64                `json:"cutoff_ms"`
	SnapshotID          string               `json:"snapshot_id"`
	Generation          uint64               `json:"generation"`
	Members             []pairEnvelopeMember `json:"members"`
}

type pairEnvelopeInputs struct {
	Partition           RiskPartition
	PairID              string
	ContractFingerprint string
	SlotMs              int64
	StartMs             int64
	DeadlineMs          int64
	CutoffMs            int64
	Members             []pairContractMember
	BookNeed            pairBookNeed
	SourceContracts     map[string]string
	Observations        []pairObservation
	Capabilities        []pairCapabilityEvidence
	Conversions         []pairConversionEvidence
	Accounts            []pairAccountEvidence
	SnapshotID          string
	Generation          uint64
	Limits              pairOperationalLimits
}

type pairCollectRequest struct {
	Partition           RiskPartition
	PairID              string
	ContractFingerprint string
	SlotMs              int64
	StartMs             int64
	DeadlineMs          int64
	Members             []pairContractMember
	BookNeed            pairBookNeed
	SourceContracts     map[string]string
	Snapshot            *marketSnapshot
	SnapshotID          string
	Generation          uint64
}

type pairReferenceRequest struct {
	Partition           RiskPartition
	PairID              string
	ContractFingerprint string
	Member              pairContractMember
	Side                string
	BookNeed            pairBookNeed
	SourceContract      string
	StartMs             int64
	DeadlineMs          int64
	Snapshot            *marketSnapshot
	SnapshotID          string
	Generation          uint64
}

type pairQuoteSource interface {
	Sources() map[string]string
	Recorded() bool
	Collect(ctx context.Context, req pairCollectRequest) ([]pairObservation, error)
	Reference(ctx context.Context, req pairReferenceRequest) (pairObservation, error)
}

func pairQuoteSourceErrors(c basketContract, src pairQuoteSource) error {
	if src == nil || (reflect.ValueOf(src).Kind() == reflect.Ptr && reflect.ValueOf(src).IsNil()) {
		return fmt.Errorf("pair research needs a quote source (pairResearchDeps.Quotes)")
	}
	got := map[string]string{}
	for k, v := range src.Sources() {
		got[k] = v
	}
	if !c.isPair() {
		return fmt.Errorf("a quote source binds only a pair contract")
	}
	var errs []string
	for _, m := range c.pairMembersSorted() {
		label, ok := got[m.MemberID]
		if !ok {
			errs = append(errs, fmt.Sprintf("quote source has no reader for member %s", m.MemberID))
			continue
		}
		delete(got, m.MemberID)
		if label != m.Source {
			errs = append(errs, fmt.Sprintf("quote source reads member %s as %q, the contract freezes %q", m.MemberID, label, m.Source))
			continue
		}
		if _, ok := pairSourceSpecFor(m.Provider, label); !ok {
			errs = append(errs, fmt.Sprintf("quote source %q is not a %s source", label, m.Provider))
		}
	}
	extra := make([]string, 0, len(got))
	for k := range got {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		errs = append(errs, fmt.Sprintf("quote source names %s, which is not a pair member", k))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

type pairLevelRat struct {
	px, qty *big.Rat
	level   pairNativeLevel
	orders  uint64
}

type pairDerived struct {
	Provenance     string
	VenueTimeMs    int64
	FirstReceiptMs int64
	SourceCutoffMs int64
	SnapshotID     string
	Coverage       pairCoverage
	Bids, Asks     []pairNativeLevel
	Records        []pairWindowRecord
}

type pairWindowRecord struct {
	Seq       int64
	EventMs   int64
	ReceiptMs int64
	Identity  string
	BidDepth  int
	AskDepth  int
	Bids      []pairLevelRat
	Asks      []pairLevelRat
}

func pairTreeObject(node interface{}, path string, required, optional []string) (map[string]interface{}, error) {
	obj, ok := node.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON object, got %s", path, pairNodeKind(node))
	}
	allowed := map[string]bool{}
	for _, k := range required {
		allowed[k] = true
		if _, ok := obj[k]; !ok {
			return nil, fmt.Errorf("%s.%s is missing", path, k)
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	var extra []string
	for k := range obj {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return nil, fmt.Errorf("%s has unknown field %q", path, extra[0])
	}
	return obj, nil
}

func pairTreeString(node interface{}, path string) (string, error) {
	s, ok := node.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a JSON string, got %s", path, pairNodeKind(node))
	}
	return s, nil
}

func pairTreeInt(node interface{}, path string, lo, hi int64) (int64, error) {
	n, ok := node.(pairJSONNumber)
	if !ok || !pairIntegerPattern.MatchString(string(n)) {
		return 0, fmt.Errorf("%s must be a JSON integer, got %s", path, pairNodeKind(node))
	}
	v, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%s %s is outside [%d, %d]", path, n, lo, hi)
	}
	return v, nil
}

func pairTreeBool(node interface{}, path string) (bool, error) {
	b, ok := node.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a JSON boolean, got %s", path, pairNodeKind(node))
	}
	return b, nil
}

func pairTreeArray(node interface{}, path string, max int) ([]interface{}, error) {
	arr, ok := node.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON array, got %s", path, pairNodeKind(node))
	}
	if len(arr) > max {
		return nil, fmt.Errorf("%s holds %d entries, above %d", path, len(arr), max)
	}
	return arr, nil
}

func pairBookDecimalText(raw, path string) (string, *big.Rat, error) {
	s, err := canonicalBookDecimal(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %v", path, err)
	}
	return s, bookDecimalRat(s), nil
}

func pairCanonicalBookDecimal(raw, path string) (string, *big.Rat, error) {
	s, r, err := pairBookDecimalText(raw, path)
	if err != nil {
		return "", nil, err
	}
	if s != raw {
		return "", nil, fmt.Errorf("%s decimal %q is not canonical", path, raw)
	}
	return s, r, nil
}

func pairSideDepths(depth int, side string) [2]int {
	switch side {
	case "buy":
		return [2]int{1, depth}
	case "sell":
		return [2]int{depth, 1}
	}
	return [2]int{depth, depth}
}

func pairCheckSides(bids, asks []pairLevelRat, depths [2]int, depth int, path string) ([]pairNativeLevel, []pairNativeLevel, error) {
	if len(bids) < depths[0] || len(asks) < depths[1] {
		return nil, nil, fmt.Errorf("%s holds %d bids and %d asks, below the required depth %d", path, len(bids), len(asks), depth)
	}
	for i := 1; i < len(bids); i++ {
		if bids[i].px.Cmp(bids[i-1].px) >= 0 {
			return nil, nil, fmt.Errorf("%s bids are not strictly descending at %d", path, i)
		}
	}
	for i := 1; i < len(asks); i++ {
		if asks[i].px.Cmp(asks[i-1].px) <= 0 {
			return nil, nil, fmt.Errorf("%s asks are not strictly ascending at %d", path, i)
		}
	}
	if bids[0].px.Cmp(asks[0].px) >= 0 {
		return nil, nil, fmt.Errorf("%s is crossed or locked (best bid %s, best ask %s)", path, bids[0].level.Px, asks[0].level.Px)
	}
	b, a := []pairNativeLevel{}, []pairNativeLevel{}
	for i := 0; i < depth && i < len(bids); i++ {
		b = append(b, bids[i].level)
	}
	for i := 0; i < depth && i < len(asks); i++ {
		a = append(a, asks[i].level)
	}
	return b, a, nil
}

func pairPointCoverage(spec pairSourceSpec, need pairBookNeed, receiptMs int64) pairCoverage {
	return pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
		FromMs: receiptMs, ThroughMs: receiptMs, RequiredDepth: need.Depth, RequiredWindowMs: 0, RequiredCapacity: 1,
		PrefixComplete: true, Status: pairCoverageComplete, Gaps: []pairCoverageGap{}}
}

func pairCheckVenueTime(venueMs, receiptMs int64, path string) error {
	if venueMs-receiptMs > feedBookMaxFutureSkewMs {
		return fmt.Errorf("%s venue time %d is more than %dms after its receipt %d", path, venueMs, feedBookMaxFutureSkewMs, receiptMs)
	}
	if receiptMs-venueMs > feedBookMaxLagMs {
		return fmt.Errorf("%s venue time %d lags its receipt %d by more than %dms", path, venueMs, receiptMs, feedBookMaxLagMs)
	}
	return nil
}

func pairDerive(spec pairSourceSpec, m pairContractMember, need pairBookNeed, payload []byte, readerReceiptMs int64) (pairDerived, error) {
	return pairDeriveSide(spec, m, need, payload, readerReceiptMs, "")
}

func pairDeriveSide(spec pairSourceSpec, m pairContractMember, need pairBookNeed, payload []byte, readerReceiptMs int64, side string) (pairDerived, error) {
	depths := pairSideDepths(need.Depth, side)
	tree, err := pairStrictParse(payload)
	if err != nil {
		return pairDerived{}, fmt.Errorf("payload: %w", err)
	}
	canon := &bytes.Buffer{}
	if err := pairWriteCanonical(canon, tree); err != nil {
		return pairDerived{}, fmt.Errorf("payload: %w", err)
	}
	if !bytes.Equal(canon.Bytes(), payload) {
		return pairDerived{}, fmt.Errorf("payload bytes are not canonical")
	}
	switch spec.Contract {
	case pairContractHLPublic:
		return pairDeriveHLPublic(spec, m, need, tree, readerReceiptMs, depths)
	case pairContractOKXPublic:
		return pairDeriveOKXPublic(spec, m, need, tree, readerReceiptMs, depths)
	case pairContractHLSealed, pairContractHLArchive:
		return pairDeriveWindow(spec, m, need, tree, depths)
	}
	return pairDerived{}, fmt.Errorf("source contract %q has no derivation", spec.Contract)
}

func pairDeriveHLPublic(spec pairSourceSpec, m pairContractMember, need pairBookNeed, tree interface{}, receiptMs int64, depths [2]int) (pairDerived, error) {
	root, err := pairTreeObject(tree, "payload", []string{"request", "response"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	req, err := pairTreeObject(root["request"], "payload.request", []string{"coin", "type"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	if c, _ := req["coin"].(string); c != m.Instrument {
		return pairDerived{}, fmt.Errorf("payload.request.coin %v is not the member instrument %s", req["coin"], m.Instrument)
	}
	if t, _ := req["type"].(string); t != "l2Book" {
		return pairDerived{}, fmt.Errorf("payload.request.type %v is not l2Book", req["type"])
	}
	resp, err := pairTreeObject(root["response"], "payload.response", []string{"coin", "levels", "time"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	coin, err := pairTreeString(resp["coin"], "payload.response.coin")
	if err != nil {
		return pairDerived{}, err
	}
	if coin != m.Instrument {
		return pairDerived{}, fmt.Errorf("payload.response.coin %q is not the member instrument %s", coin, m.Instrument)
	}
	venue, err := pairTreeInt(resp["time"], "payload.response.time", 1, math.MaxInt64)
	if err != nil {
		return pairDerived{}, err
	}
	sides, err := pairTreeArray(resp["levels"], "payload.response.levels", 2)
	if err != nil {
		return pairDerived{}, err
	}
	if len(sides) != 2 {
		return pairDerived{}, fmt.Errorf("payload.response.levels holds %d sides, want 2", len(sides))
	}
	var parsed [2][]pairLevelRat
	for s, side := range sides {
		path := fmt.Sprintf("payload.response.levels[%d]", s)
		levels, err := pairTreeArray(side, path, feedBookMaxLevels)
		if err != nil {
			return pairDerived{}, err
		}
		for i, lv := range levels {
			lp := fmt.Sprintf("%s[%d]", path, i)
			o, err := pairTreeObject(lv, lp, []string{"n", "px", "sz"}, nil)
			if err != nil {
				return pairDerived{}, err
			}
			if _, err := pairTreeInt(o["n"], lp+".n", 1, int64(feedBookMaxOrders)); err != nil {
				return pairDerived{}, err
			}
			pxRaw, err := pairTreeString(o["px"], lp+".px")
			if err != nil {
				return pairDerived{}, err
			}
			szRaw, err := pairTreeString(o["sz"], lp+".sz")
			if err != nil {
				return pairDerived{}, err
			}
			px, pxR, err := pairBookDecimalText(pxRaw, lp+".px")
			if err != nil {
				return pairDerived{}, err
			}
			sz, szR, err := pairBookDecimalText(szRaw, lp+".sz")
			if err != nil {
				return pairDerived{}, err
			}
			parsed[s] = append(parsed[s], pairLevelRat{px: pxR, qty: szR, level: pairNativeLevel{Px: px, Qty: sz}})
		}
	}
	bids, asks, err := pairCheckSides(parsed[0], parsed[1], depths, need.Depth, "payload book")
	if err != nil {
		return pairDerived{}, err
	}
	if err := pairCheckVenueTime(venue, receiptMs, "payload"); err != nil {
		return pairDerived{}, err
	}
	return pairDerived{Provenance: spec.Contract + "@" + pairHLPublicHost, VenueTimeMs: venue, FirstReceiptMs: receiptMs, SourceCutoffMs: receiptMs,
		Coverage: pairPointCoverage(spec, need, receiptMs), Bids: bids, Asks: asks}, nil
}

func pairDeriveOKXPublic(spec pairSourceSpec, m pairContractMember, need pairBookNeed, tree interface{}, receiptMs int64, depths [2]int) (pairDerived, error) {
	root, err := pairTreeObject(tree, "payload", []string{"request", "response"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	req, err := pairTreeObject(root["request"], "payload.request", []string{"instId", "sz"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	if id, _ := req["instId"].(string); id != m.Instrument {
		return pairDerived{}, fmt.Errorf("payload.request.instId %v is not the member instrument %s", req["instId"], m.Instrument)
	}
	if sz, _ := req["sz"].(string); sz != strconv.Itoa(need.Depth) {
		return pairDerived{}, fmt.Errorf("payload.request.sz %v is not the required depth %d", req["sz"], need.Depth)
	}
	resp, err := pairTreeObject(root["response"], "payload.response", []string{"code", "data", "msg"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	if code, _ := resp["code"].(string); code != "0" {
		return pairDerived{}, fmt.Errorf("payload.response.code %v is not \"0\"", resp["code"])
	}
	if _, err := pairTreeString(resp["msg"], "payload.response.msg"); err != nil {
		return pairDerived{}, err
	}
	data, err := pairTreeArray(resp["data"], "payload.response.data", 1)
	if err != nil {
		return pairDerived{}, err
	}
	if len(data) != 1 {
		return pairDerived{}, fmt.Errorf("payload.response.data holds %d books, want 1", len(data))
	}
	book, err := pairTreeObject(data[0], "payload.response.data[0]", []string{"asks", "bids", "ts"}, nil)
	if err != nil {
		return pairDerived{}, err
	}
	tsRaw, err := pairTreeString(book["ts"], "payload.response.data[0].ts")
	if err != nil {
		return pairDerived{}, err
	}
	if !pairVenueMsPattern.MatchString(tsRaw) {
		return pairDerived{}, fmt.Errorf("payload.response.data[0].ts %q is not a positive millisecond integer", tsRaw)
	}
	venue, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return pairDerived{}, fmt.Errorf("payload.response.data[0].ts %q is out of range", tsRaw)
	}
	var parsed [2][]pairLevelRat
	for s, name := range []string{"bids", "asks"} {
		path := "payload.response.data[0]." + name
		levels, err := pairTreeArray(book[name], path, feedBookMaxLevels)
		if err != nil {
			return pairDerived{}, err
		}
		for i, lv := range levels {
			lp := fmt.Sprintf("%s[%d]", path, i)
			parts, err := pairTreeArray(lv, lp, 4)
			if err != nil {
				return pairDerived{}, err
			}
			if len(parts) != 4 {
				return pairDerived{}, fmt.Errorf("%s holds %d fields, want [px, sz, liquidated, orders]", lp, len(parts))
			}
			var text [4]string
			for j := range parts {
				if text[j], err = pairTreeString(parts[j], fmt.Sprintf("%s[%d]", lp, j)); err != nil {
					return pairDerived{}, err
				}
			}
			if len(text[2]) > feedBookDecimalMaxLen || !pairDecimalTextPattern.MatchString(text[2]) {
				return pairDerived{}, fmt.Errorf("%s liquidated field %q is not a plain decimal", lp, text[2])
			}
			if !pairOrderCountPattern.MatchString(text[3]) {
				return pairDerived{}, fmt.Errorf("%s order count %q is not a plain integer", lp, text[3])
			}
			px, pxR, err := pairBookDecimalText(text[0], lp+" px")
			if err != nil {
				return pairDerived{}, err
			}
			sz, szR, err := pairBookDecimalText(text[1], lp+" sz")
			if err != nil {
				return pairDerived{}, err
			}
			parsed[s] = append(parsed[s], pairLevelRat{px: pxR, qty: szR, level: pairNativeLevel{Px: px, Qty: sz}})
		}
	}
	bids, asks, err := pairCheckSides(parsed[0], parsed[1], depths, need.Depth, "payload book")
	if err != nil {
		return pairDerived{}, err
	}
	if err := pairCheckVenueTime(venue, receiptMs, "payload"); err != nil {
		return pairDerived{}, err
	}
	return pairDerived{Provenance: spec.Contract + "@" + pairOKXPublicHost, VenueTimeMs: venue, FirstReceiptMs: receiptMs, SourceCutoffMs: receiptMs,
		Coverage: pairPointCoverage(spec, need, receiptMs), Bids: bids, Asks: asks}, nil
}

var pairWindowPayloadFields = []string{"available", "capacity", "coin", "contract", "cutoff_ms", "depth", "identity_semantics", "kind", "markers",
	"max_future_skew_ms", "max_gap_ms", "max_lag_ms", "observations", "owner_instance", "prefix_complete", "retained_from_ms", "snapshot_id",
	"source", "time_semantics", "window_ms"}

func pairShift(v, delay int64, path string) (int64, error) {
	out, ok := pairAddMs(v, delay)
	if !ok {
		return 0, fmt.Errorf("%s %d plus the modeled delivery %dms overflows", path, v, delay)
	}
	return out, nil
}

func pairDeriveWindow(spec pairSourceSpec, m pairContractMember, need pairBookNeed, tree interface{}, depths [2]int) (pairDerived, error) {
	root, err := pairTreeObject(tree, "payload", pairWindowPayloadFields, []string{"feed_status", "reason"})
	if err != nil {
		return pairDerived{}, err
	}
	str := func(k string) string { s, _ := root[k].(string); return s }
	archive := spec.Contract == pairContractHLArchive
	wantTime := feedBookTimeSemantics
	if archive {
		wantTime = pairArchivePayloadTimeBasis
	}
	for _, f := range []struct{ k, want string }{{"kind", feedObservationL2Book}, {"coin", m.Instrument}, {"source", feedBookSource}, {"contract", feedBookContract},
		{"time_semantics", wantTime}, {"identity_semantics", feedBookIdentitySemantics}} {
		if _, err := pairTreeString(root[f.k], "payload."+f.k); err != nil {
			return pairDerived{}, err
		}
		if str(f.k) != f.want {
			return pairDerived{}, fmt.Errorf("payload.%s %q is not %q", f.k, str(f.k), f.want)
		}
	}
	for _, f := range []struct {
		k    string
		want int64
	}{{"max_future_skew_ms", feedBookMaxFutureSkewMs}, {"max_lag_ms", feedBookMaxLagMs}, {"max_gap_ms", feedBookMaxGapMs},
		{"window_ms", need.WindowMs}, {"capacity", int64(need.Capacity)}, {"depth", int64(need.Depth)}} {
		v, err := pairTreeInt(root[f.k], "payload."+f.k, math.MinInt64, math.MaxInt64)
		if err != nil {
			return pairDerived{}, err
		}
		if v != f.want {
			return pairDerived{}, fmt.Errorf("payload.%s %d is not the required %d", f.k, v, f.want)
		}
	}
	for _, k := range []string{"owner_instance", "snapshot_id"} {
		if _, err := pairTreeString(root[k], "payload."+k); err != nil {
			return pairDerived{}, err
		}
	}
	if v, ok := root["feed_status"]; ok {
		s, err := pairTreeString(v, "payload.feed_status")
		if err != nil {
			return pairDerived{}, err
		}
		if len(s) > pairFeedStatusMaxLen {
			return pairDerived{}, fmt.Errorf("payload.feed_status is longer than %d bytes", pairFeedStatusMaxLen)
		}
	}
	available, err := pairTreeBool(root["available"], "payload.available")
	if err != nil {
		return pairDerived{}, err
	}
	if v, ok := root["reason"]; ok {
		reason, err := pairTreeString(v, "payload.reason")
		if err != nil {
			return pairDerived{}, err
		}
		if !available {
			return pairDerived{}, fmt.Errorf("the book section is unavailable: %s", reason)
		}
		return pairDerived{}, fmt.Errorf("an available book section carries a refusal reason %q", reason)
	}
	if !available {
		return pairDerived{}, fmt.Errorf("the book section is unavailable")
	}
	snapshotID, owner := str("snapshot_id"), str("owner_instance")
	if !validBookText(owner, feedBookMaxInstanceLen) || owner == "" {
		return pairDerived{}, fmt.Errorf("payload.owner_instance %q is not a bounded owner identity", owner)
	}
	if archive {
		if snapshotID != "" {
			return pairDerived{}, fmt.Errorf("an archive section carries snapshot_id %q", snapshotID)
		}
	} else if snapshotID == "" || len(snapshotID) > pairSnapshotIDMaxLen || !pairPrintableASCII(snapshotID, pairSnapshotIDMaxLen) {
		return pairDerived{}, fmt.Errorf("a sealed section needs a printable snapshot_id of at most %d bytes", pairSnapshotIDMaxLen)
	}
	cutoff, err := pairTreeInt(root["cutoff_ms"], "payload.cutoff_ms", 1, math.MaxInt64)
	if err != nil {
		return pairDerived{}, err
	}
	retained, err := pairTreeInt(root["retained_from_ms"], "payload.retained_from_ms", 0, math.MaxInt64)
	if err != nil {
		return pairDerived{}, err
	}
	prefix, err := pairTreeBool(root["prefix_complete"], "payload.prefix_complete")
	if err != nil {
		return pairDerived{}, err
	}
	start := cutoff - need.WindowMs
	rawObs, err := pairTreeArray(root["observations"], "payload.observations", need.Capacity)
	if err != nil {
		return pairDerived{}, err
	}
	if len(rawObs) == 0 {
		return pairDerived{}, fmt.Errorf("the book section holds no observation in its window")
	}
	type winObs struct {
		seq, event, recv int64
		bids, asks       []pairLevelRat
		identity         string
		depths           [2]int64
	}
	var obs []winObs
	ids := map[string]bool{}
	for i, raw := range rawObs {
		p := fmt.Sprintf("payload.observations[%d]", i)
		o, err := pairTreeObject(raw, p, []string{"ask_depth", "asks", "bid_depth", "bids", "event_ms", "identity", "recv_ms", "seq", "session"}, nil)
		if err != nil {
			return pairDerived{}, err
		}
		var w winObs
		if w.seq, err = pairTreeInt(o["seq"], p+".seq", 1, math.MaxInt64); err != nil {
			return pairDerived{}, err
		}
		if w.event, err = pairTreeInt(o["event_ms"], p+".event_ms", 1, math.MaxInt64); err != nil {
			return pairDerived{}, err
		}
		if w.recv, err = pairTreeInt(o["recv_ms"], p+".recv_ms", 1, math.MaxInt64); err != nil {
			return pairDerived{}, err
		}
		if _, err = pairTreeInt(o["session"], p+".session", 0, math.MaxInt64); err != nil {
			return pairDerived{}, err
		}
		identity, err := pairTreeString(o["identity"], p+".identity")
		if err != nil {
			return pairDerived{}, err
		}
		if !pairHexDigestPattern.MatchString(identity) || ids[identity] {
			return pairDerived{}, fmt.Errorf("%s.identity is not a unique 64-character lowercase hex digest", p)
		}
		ids[identity] = true
		w.identity = identity
		var depths [2]int64
		for s, name := range []string{"bid_depth", "ask_depth"} {
			if depths[s], err = pairTreeInt(o[name], p+"."+name, 1, feedBookMaxLevels); err != nil {
				return pairDerived{}, err
			}
		}
		w.depths = depths
		for s, name := range []string{"bids", "asks"} {
			levels, err := pairTreeArray(o[name], p+"."+name, need.Depth)
			if err != nil {
				return pairDerived{}, err
			}
			if int64(len(levels)) > depths[s] || len(levels) == 0 {
				return pairDerived{}, fmt.Errorf("%s.%s holds %d levels, outside [1, its declared depth %d]", p, name, len(levels), depths[s])
			}
			for j, lv := range levels {
				lp := fmt.Sprintf("%s.%s[%d]", p, name, j)
				parts, err := pairTreeArray(lv, lp, 3)
				if err != nil {
					return pairDerived{}, err
				}
				if len(parts) != 3 {
					return pairDerived{}, fmt.Errorf("%s is not [px, sz, n]", lp)
				}
				pxRaw, err := pairTreeString(parts[0], lp+" px")
				if err != nil {
					return pairDerived{}, err
				}
				szRaw, err := pairTreeString(parts[1], lp+" sz")
				if err != nil {
					return pairDerived{}, err
				}
				orders, err := pairTreeInt(parts[2], lp+" n", 1, int64(feedBookMaxOrders))
				if err != nil {
					return pairDerived{}, err
				}
				px, pxR, err := pairCanonicalBookDecimal(pxRaw, lp+" px")
				if err != nil {
					return pairDerived{}, err
				}
				sz, szR, err := pairCanonicalBookDecimal(szRaw, lp+" sz")
				if err != nil {
					return pairDerived{}, err
				}
				lr := pairLevelRat{px: pxR, qty: szR, level: pairNativeLevel{Px: px, Qty: sz}, orders: uint64(orders)}
				if s == 0 {
					w.bids = append(w.bids, lr)
				} else {
					w.asks = append(w.asks, lr)
				}
			}
		}
		if w.recv < start || w.recv > cutoff {
			return pairDerived{}, fmt.Errorf("%s receipt %d lies outside the window [%d, %d]", p, w.recv, start, cutoff)
		}
		if err := pairCheckVenueTime(w.event, w.recv, p); err != nil {
			return pairDerived{}, err
		}
		if n := len(obs); n > 0 && !(w.seq > obs[n-1].seq && w.event > obs[n-1].event && w.recv >= obs[n-1].recv) {
			return pairDerived{}, fmt.Errorf("%s is out of sequence, event or receipt order", p)
		}
		obs = append(obs, w)
	}
	rawMarks, err := pairTreeArray(root["markers"], "payload.markers", feedBookMaxMarkers)
	if err != nil {
		return pairDerived{}, err
	}
	var gaps []pairCoverageGap
	var lastSeq int64
	for i, raw := range rawMarks {
		p := fmt.Sprintf("payload.markers[%d]", i)
		mk, err := pairTreeObject(raw, p, []string{"at_ms", "reason", "seq", "session"}, []string{"detail"})
		if err != nil {
			return pairDerived{}, err
		}
		seq, err := pairTreeInt(mk["seq"], p+".seq", 1, math.MaxInt64)
		if err != nil {
			return pairDerived{}, err
		}
		if i > 0 && seq <= lastSeq {
			return pairDerived{}, fmt.Errorf("%s is out of sequence", p)
		}
		lastSeq = seq
		at, err := pairTreeInt(mk["at_ms"], p+".at_ms", 1, math.MaxInt64)
		if err != nil {
			return pairDerived{}, err
		}
		if at < start || at > cutoff {
			return pairDerived{}, fmt.Errorf("%s lies outside the window", p)
		}
		if _, err := pairTreeInt(mk["session"], p+".session", 0, math.MaxInt64); err != nil {
			return pairDerived{}, err
		}
		reason, err := pairTreeString(mk["reason"], p+".reason")
		if err != nil {
			return pairDerived{}, err
		}
		if !feedBookMarkerReasons[reason] {
			return pairDerived{}, fmt.Errorf("%s reason %q is not a contract marker", p, reason)
		}
		if d, ok := mk["detail"]; ok {
			detail, err := pairTreeString(d, p+".detail")
			if err != nil {
				return pairDerived{}, err
			}
			if detail == "" || len(detail) > feedBookMaxReasonLen {
				return pairDerived{}, fmt.Errorf("%s detail is empty or longer than %d bytes", p, feedBookMaxReasonLen)
			}
		}
		gaps = append(gaps, pairCoverageGap{FromMs: at, ThroughMs: at, Reason: reason})
	}
	prev := start
	for _, o := range obs {
		if o.recv-prev > feedBookMaxGapMs {
			gaps = append(gaps, pairCoverageGap{FromMs: prev, ThroughMs: o.recv, Reason: pairGapSilence})
		}
		prev = o.recv
	}
	if cutoff-prev > feedBookMaxGapMs {
		gaps = append(gaps, pairCoverageGap{FromMs: prev, ThroughMs: cutoff, Reason: pairGapSilence})
	}
	sort.SliceStable(gaps, func(i, j int) bool {
		if gaps[i].FromMs != gaps[j].FromMs {
			return gaps[i].FromMs < gaps[j].FromMs
		}
		if gaps[i].ThroughMs != gaps[j].ThroughMs {
			return gaps[i].ThroughMs < gaps[j].ThroughMs
		}
		return gaps[i].Reason < gaps[j].Reason
	})
	last := obs[len(obs)-1]
	bids, asks, err := pairCheckSides(last.bids, last.asks, depths, need.Depth, "payload latest observation")
	if err != nil {
		return pairDerived{}, err
	}
	delay := int64(0)
	if archive {
		delay = m.ModeledDeliveryMs
	}
	receipt, err := pairShift(last.recv, delay, "receipt")
	if err != nil {
		return pairDerived{}, err
	}
	from, err := pairShift(retained, delay, "coverage start")
	if err != nil {
		return pairDerived{}, err
	}
	through, err := pairShift(cutoff, delay, "coverage end")
	if err != nil {
		return pairDerived{}, err
	}
	out := make([]pairCoverageGap, 0, len(gaps))
	for _, g := range gaps {
		f, err := pairShift(g.FromMs, delay, "gap start")
		if err != nil {
			return pairDerived{}, err
		}
		t, err := pairShift(g.ThroughMs, delay, "gap end")
		if err != nil {
			return pairDerived{}, err
		}
		out = append(out, pairCoverageGap{FromMs: f, ThroughMs: t, Reason: g.Reason})
	}
	status := pairCoverageComplete
	if !prefix || len(out) > 0 {
		status = pairCoverageIncomplete
	}
	records := make([]pairWindowRecord, 0, len(obs))
	for _, o := range obs {
		r, err := pairShift(o.recv, delay, "record receipt")
		if err != nil {
			return pairDerived{}, err
		}
		records = append(records, pairWindowRecord{Seq: o.seq, EventMs: o.event, ReceiptMs: r, Identity: o.identity, BidDepth: int(o.depths[0]), AskDepth: int(o.depths[1]), Bids: o.bids, Asks: o.asks})
	}
	cov := pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
		FromMs: from, ThroughMs: through, RequiredDepth: need.Depth, RequiredWindowMs: need.WindowMs, RequiredCapacity: need.Capacity,
		PrefixComplete: prefix, Status: status, Gaps: out}
	return pairDerived{Provenance: spec.Contract + "@" + owner, VenueTimeMs: last.event, FirstReceiptMs: receipt, SourceCutoffMs: cutoff,
		SnapshotID: snapshotID, Coverage: cov, Bids: bids, Asks: asks, Records: records}, nil
}

func pairObservationFromPayload(m pairContractMember, partition string, need pairBookNeed, payload []byte, readerReceiptMs int64, generation uint64) (pairObservation, error) {
	return pairObservationFromPayloadSide(m, partition, need, payload, readerReceiptMs, generation, "")
}

func pairObservationFromPayloadSide(m pairContractMember, partition string, need pairBookNeed, payload []byte, readerReceiptMs int64, generation uint64, side string) (pairObservation, error) {
	spec, ok := pairSourceSpecFor(m.Provider, m.Source)
	if !ok {
		return pairObservation{}, fmt.Errorf("member %s source %q has no source contract", m.MemberID, m.Source)
	}
	if max := pairPayloadMaxBytes(spec, m, need); len(payload) == 0 || len(payload) > max {
		return pairObservation{}, fmt.Errorf("member %s payload is %d bytes, outside [1, %d]", m.MemberID, len(payload), max)
	}
	d, err := pairDeriveSide(spec, m, need, payload, readerReceiptMs, side)
	if err != nil {
		return pairObservation{}, fmt.Errorf("member %s %s: %w", m.MemberID, spec.Contract, err)
	}
	if spec.Source != pairSourceSealedFeed {
		generation = 0
	}
	venue := d.VenueTimeMs
	return pairObservation{V: pairObservationVersion, Key: pairEvidenceKey{Partition: partition, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID},
		Source: m.Source, Provenance: d.Provenance, VenueTimeMs: &venue, FirstReceiptMs: d.FirstReceiptMs, ModeledDeliveryMs: m.ModeledDeliveryMs,
		SourceCutoffMs: d.SourceCutoffMs, SnapshotID: d.SnapshotID, Generation: generation, Coverage: d.Coverage,
		NativeBids: d.Bids, NativeAsks: d.Asks, Bids: []pairBaseLevel{}, Asks: []pairBaseLevel{},
		Payload: append([]byte(nil), payload...), PayloadHash: pairSHA256(payload)}, nil
}

func pairEscapedLen(s string) int {
	return 6*len(s) + 2
}

const (
	pairPublicLevelBytes   = 160
	pairWindowLevelBytes   = 96
	pairWindowRecordBytes  = 256
	pairWindowMarkerBytes  = 1792
	pairWindowHeaderBytes  = 2048
	pairPublicHeaderBytes  = 512
	pairObservationFixed   = 2048
	pairNativeLevelBytes   = 96
	pairBaseLevelBytes     = 112
	pairCoverageGapBytes   = 128
	pairStampBytes         = 3072
	pairCapabilityBodySize = 2048
	pairAccountBodySize    = 512
	pairConversionBodySize = 256
	pairEnvelopeFixedBytes = 2048
)

func pairPayloadMaxBytes(spec pairSourceSpec, m pairContractMember, need pairBookNeed) int {
	inst := pairEscapedLen(m.Instrument)
	if spec.Point {
		return pairPublicHeaderBytes + 2*inst + 2*need.Depth*pairPublicLevelBytes
	}
	record := pairWindowRecordBytes + 2*need.Depth*pairWindowLevelBytes
	return pairWindowHeaderBytes + inst + pairEscapedLen(strings.Repeat("x", pairSnapshotIDMaxLen)) + pairEscapedLen(strings.Repeat("x", feedBookMaxInstanceLen)) +
		need.Capacity*record + feedBookMaxMarkers*pairWindowMarkerBytes
}

func pairEnvelopeWorstCaseBytes(partition string, pairID, contractFP string, members []pairContractMember, need pairBookNeed) int {
	total := pairEnvelopeFixedBytes + pairEscapedLen(partition) + pairEscapedLen(pairID) + pairEscapedLen(contractFP)
	for _, m := range members {
		spec, ok := pairSourceSpecFor(m.Provider, m.Source)
		if !ok {
			continue
		}
		key := pairEscapedLen(partition) + pairEscapedLen(m.Provider) + pairEscapedLen(m.AccountKey) + pairEscapedLen(m.Instrument) + pairEscapedLen(m.MemberID) + 128
		payload := pairPayloadMaxBytes(spec, m, need)
		gaps := 0
		if !spec.Point {
			gaps = feedBookMaxMarkers + need.Capacity + 1
		}
		obs := pairObservationFixed + key + base64.StdEncoding.EncodedLen(payload) + 2*need.Depth*(pairNativeLevelBytes+pairBaseLevelBytes) +
			gaps*pairCoverageGapBytes + pairEscapedLen(strings.Repeat("x", feedBookMaxInstanceLen+len(spec.Contract)+1)) + pairEscapedLen(strings.Repeat("x", pairSnapshotIDMaxLen))
		fees := 0
		for _, c := range m.Meta.FeeCurrencies {
			fees += pairEscapedLen(c) + 1
		}
		conversions := len(pairRequiredConversionCurrencies(m)) * (pairStampBytes + key + pairConversionBodySize)
		total += key + obs + (pairStampBytes + key + pairCapabilityBodySize + fees + pairEscapedLen(m.Meta.BaseAsset)) + (pairStampBytes + key + pairAccountBodySize) + conversions
	}
	return total
}

type pairEnvelopeExpect struct {
	Partition           string
	PairID              string
	ContractFingerprint string
	Members             []pairContractMember
	BookNeed            pairBookNeed
	Limits              pairOperationalLimits
}

func pairEnvelopeExpectFor(rt *basketRuntime) pairEnvelopeExpect {
	return pairEnvelopeExpect{Partition: rt.Partition.String(), PairID: rt.Contract.BasketID, ContractFingerprint: rt.Contract.Fingerprint(),
		Members: rt.Contract.pairMembersSorted(), BookNeed: rt.Contract.Pair.BookNeed, Limits: rt.Pair.Limits}
}

func (e pairEnvelopeExpect) memberKey(m pairContractMember) pairEvidenceKey {
	return pairEvidenceKey{Partition: e.Partition, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID}
}

func (e pairEnvelopeExpect) sortedMembers() []pairContractMember {
	out := append([]pairContractMember(nil), e.Members...)
	sort.SliceStable(out, func(i, j int) bool { return e.memberKey(out[i]).less(e.memberKey(out[j])) < 0 })
	return out
}

func pairNormalizedLevels(levels []pairNativeLevel, contractSize *big.Rat) ([]pairBaseLevel, error) {
	out := make([]pairBaseLevel, 0, len(levels))
	for _, l := range levels {
		q, ok := new(big.Rat).SetString(l.Qty)
		if !ok || !pairDecimalCanonical(l.Qty) || q.Sign() <= 0 {
			return nil, fmt.Errorf("native quantity %q is not a positive canonical decimal", l.Qty)
		}
		if p, ok := new(big.Rat).SetString(l.Px); !ok || !pairDecimalCanonical(l.Px) || p.Sign() <= 0 {
			return nil, fmt.Errorf("native price %q is not a positive canonical decimal", l.Px)
		}
		base, err := pairRatDecimal(new(big.Rat).Mul(q, contractSize))
		if err != nil {
			return nil, err
		}
		out = append(out, pairBaseLevel{Px: l.Px, BaseQty: base})
	}
	return out, nil
}

func pairNormalizationID(c pairCapabilityEvidence) string {
	return c.Meta.NativeUnit + "*" + string(c.Meta.ContractSize) + "@" + c.EvidenceID
}

func pairNormalizeObservation(obs pairObservation, c pairCapabilityEvidence) (pairObservation, error) {
	size, err := pairDecimalPositive("contract_size", c.Meta.ContractSize)
	if err != nil {
		return obs, err
	}
	bids, err := pairNormalizedLevels(obs.NativeBids, size)
	if err != nil {
		return obs, err
	}
	asks, err := pairNormalizedLevels(obs.NativeAsks, size)
	if err != nil {
		return obs, err
	}
	obs.Bids, obs.Asks = bids, asks
	obs.NativeUnit, obs.ContractSize, obs.PriceCurrency = c.Meta.NativeUnit, string(c.Meta.ContractSize), c.Meta.PriceCurrency
	obs.NormalizationID = pairNormalizationID(c)
	return obs, nil
}

func pairCloneObservation(o pairObservation) pairObservation {
	o.NativeBids = append([]pairNativeLevel{}, o.NativeBids...)
	o.NativeAsks = append([]pairNativeLevel{}, o.NativeAsks...)
	o.Bids = append([]pairBaseLevel{}, o.Bids...)
	o.Asks = append([]pairBaseLevel{}, o.Asks...)
	o.Coverage.Gaps = append([]pairCoverageGap{}, o.Coverage.Gaps...)
	o.Payload = append([]byte{}, o.Payload...)
	if o.VenueTimeMs != nil {
		v := *o.VenueTimeMs
		o.VenueTimeMs = &v
	}
	return o
}

func pairCheckObservationSource(o pairObservation, m pairContractMember, key pairEvidenceKey, need pairBookNeed) (pairSourceSpec, error) {
	return pairCheckObservationSourceSide(o, m, key, need, "")
}

func pairCheckObservationSourceSide(o pairObservation, m pairContractMember, key pairEvidenceKey, need pairBookNeed, side string) (pairSourceSpec, error) {
	spec, _, err := pairCheckObservationEvent(o, m, key, need, side)
	if err != nil {
		return spec, err
	}
	if o.Coverage.Status != pairCoverageComplete {
		return spec, fmt.Errorf("%s coverage is %s (prefix complete %v, %d gaps); a failed or partial book is never repaired", spec.Contract, o.Coverage.Status, o.Coverage.PrefixComplete, len(o.Coverage.Gaps))
	}
	return spec, nil
}

func pairCheckObservationEvent(o pairObservation, m pairContractMember, key pairEvidenceKey, need pairBookNeed, side string) (pairSourceSpec, pairDerived, error) {
	spec, ok := pairSourceSpecFor(m.Provider, m.Source)
	if !ok {
		return spec, pairDerived{}, fmt.Errorf("member %s source %q has no source contract", m.MemberID, m.Source)
	}
	if o.V != pairObservationVersion {
		return spec, pairDerived{}, fmt.Errorf("observation version %d is not supported (want %d)", o.V, pairObservationVersion)
	}
	if o.Key != key {
		return spec, pairDerived{}, fmt.Errorf("observation key %s is not the member key %s", o.Key, key)
	}
	if o.Source != m.Source {
		return spec, pairDerived{}, fmt.Errorf("observation source %q is not the frozen source %q", o.Source, m.Source)
	}
	if o.ModeledDeliveryMs != m.ModeledDeliveryMs {
		return spec, pairDerived{}, fmt.Errorf("observation modeled delivery %dms is not the frozen %dms", o.ModeledDeliveryMs, m.ModeledDeliveryMs)
	}
	if o.VenueTimeMs == nil {
		return spec, pairDerived{}, fmt.Errorf("source contract %s supplies a venue time, but the observation venue time is null", spec.Contract)
	}
	if max := pairPayloadMaxBytes(spec, m, need); len(o.Payload) == 0 || len(o.Payload) > max {
		return spec, pairDerived{}, fmt.Errorf("observation payload is %d bytes, outside [1, %d]", len(o.Payload), max)
	}
	if o.PayloadHash != pairSHA256(o.Payload) {
		return spec, pairDerived{}, fmt.Errorf("observation payload_hash does not match its payload bytes")
	}
	d, err := pairDeriveSide(spec, m, need, o.Payload, o.FirstReceiptMs, side)
	if err != nil {
		return spec, pairDerived{}, fmt.Errorf("%s payload: %w", spec.Contract, err)
	}
	if d.Provenance != o.Provenance {
		return spec, pairDerived{}, fmt.Errorf("observation provenance %q is not the payload provenance %q", o.Provenance, d.Provenance)
	}
	if *o.VenueTimeMs != d.VenueTimeMs || o.FirstReceiptMs != d.FirstReceiptMs || o.SourceCutoffMs != d.SourceCutoffMs || o.SnapshotID != d.SnapshotID {
		return spec, pairDerived{}, fmt.Errorf("observation times or snapshot (venue %d, receipt %d, source cutoff %d, snapshot %q) disagree with the payload (%d, %d, %d, %q)",
			*o.VenueTimeMs, o.FirstReceiptMs, o.SourceCutoffMs, o.SnapshotID, d.VenueTimeMs, d.FirstReceiptMs, d.SourceCutoffMs, d.SnapshotID)
	}
	if !reflect.DeepEqual(o.Coverage, d.Coverage) {
		return spec, pairDerived{}, fmt.Errorf("observation coverage %+v disagrees with the payload coverage %+v", o.Coverage, d.Coverage)
	}
	if !reflect.DeepEqual(o.NativeBids, d.Bids) || !reflect.DeepEqual(o.NativeAsks, d.Asks) {
		return spec, pairDerived{}, fmt.Errorf("observation native levels disagree with the payload levels")
	}
	if spec.Source != pairSourceSealedFeed && o.Generation != 0 {
		return spec, pairDerived{}, fmt.Errorf("a %s observation carries snapshot generation %d", spec.Source, o.Generation)
	}
	return spec, d, nil
}

func pairDecodeQuoteEnvelope(raw []byte, limit int) (pairQuoteEnvelope, error) {
	var env pairQuoteEnvelope
	if limit <= 0 || limit > pairMaxEnvelopeBytes {
		limit = pairMaxEnvelopeBytes
	}
	if len(raw) > limit {
		return env, fmt.Errorf("quote envelope is %d bytes, above the %d byte limit", len(raw), limit)
	}
	tree, err := pairStrictParse(raw)
	if err != nil {
		return env, fmt.Errorf("quote envelope: %w", err)
	}
	if obj, ok := tree.(map[string]interface{}); ok {
		if v, ok := obj["v"].(pairJSONNumber); ok && string(v) != strconv.Itoa(pairEnvelopeVersion) {
			return env, fmt.Errorf("quote envelope version %s is not supported (want %d)", v, pairEnvelopeVersion)
		}
	}
	if err := pairStrictDecodeTree(tree, raw, &env, "quote envelope"); err != nil {
		return env, err
	}
	canon, err := pairCanonicalJSON(env)
	if err != nil {
		return env, fmt.Errorf("quote envelope: %w", err)
	}
	if !bytes.Equal(canon, raw) {
		return env, fmt.Errorf("quote envelope bytes are not canonical")
	}
	if env.V != pairEnvelopeVersion {
		return env, fmt.Errorf("quote envelope version %d is not supported (want %d)", env.V, pairEnvelopeVersion)
	}
	return env, nil
}

func pairCheckEnvelope(exp pairEnvelopeExpect, raw []byte) (pairQuoteEnvelope, error) {
	env, err := pairDecodeQuoteEnvelope(raw, exp.Limits.EnvelopeMaxBytes)
	if err != nil {
		return env, err
	}
	if env.Partition != exp.Partition || env.PairID != exp.PairID || env.ContractFingerprint != exp.ContractFingerprint {
		return env, fmt.Errorf("envelope identity (partition %s, pair %s, contract %s) is not the pair (partition %s, pair %s, contract %s)",
			env.Partition, env.PairID, shortFingerprint(env.ContractFingerprint), exp.Partition, exp.PairID, shortFingerprint(exp.ContractFingerprint))
	}
	if err := exp.BookNeed.validate(); err != nil {
		return env, fmt.Errorf("book need: %w", err)
	}
	wantDeadline, ok := pairAddMs(env.StartMs, exp.Limits.CollectionTimeoutMs)
	switch {
	case env.SlotMs <= 0 || env.StartMs < env.SlotMs:
		return env, fmt.Errorf("envelope slot %d is not positive or is after the collection start %d", env.SlotMs, env.StartMs)
	case !ok || env.DeadlineMs != wantDeadline:
		return env, fmt.Errorf("envelope deadline %d is not the collection start %d plus the %dms collection timeout", env.DeadlineMs, env.StartMs, exp.Limits.CollectionTimeoutMs)
	case env.CutoffMs < env.StartMs || !pairBeforeDeadline(env.CutoffMs, env.DeadlineMs):
		return env, fmt.Errorf("envelope cutoff %d is not in [start %d, deadline %d)", env.CutoffMs, env.StartMs, env.DeadlineMs)
	}
	members := exp.sortedMembers()
	if len(env.Members) != len(members) || len(members) == 0 {
		return env, fmt.Errorf("envelope holds %d members, the pair has %d", len(env.Members), len(members))
	}
	sealed := false
	receipts := make([]int64, 0, len(members))
	for i, m := range members {
		em := env.Members[i]
		key := exp.memberKey(m)
		if em.Key != key {
			return env, fmt.Errorf("envelope member %d key %s is not %s (members must be complete and sorted by full key)", i, em.Key, key)
		}
		spec, err := pairCheckObservationSource(em.Observation, m, key, exp.BookNeed)
		if err != nil {
			return env, fmt.Errorf("member %s: %w", m.MemberID, err)
		}
		obs := em.Observation
		if spec.Source == pairSourceSealedFeed {
			sealed = true
			if obs.SnapshotID == "" || obs.SnapshotID != env.SnapshotID || obs.Generation != env.Generation {
				return env, fmt.Errorf("member %s sealed observation snapshot %q generation %d is not the captured snapshot %q generation %d", m.MemberID, obs.SnapshotID, obs.Generation, env.SnapshotID, env.Generation)
			}
		} else if obs.SnapshotID != "" {
			return env, fmt.Errorf("member %s %s observation carries snapshot_id %q", m.MemberID, spec.Source, obs.SnapshotID)
		}
		if !pairAgeWithin(env.CutoffMs, obs.FirstReceiptMs, exp.Limits.QuoteAgeMs) {
			return env, fmt.Errorf("member %s receipt %d is after the cutoff %d or older than quote_age_ms %d", m.MemberID, obs.FirstReceiptMs, env.CutoffMs, exp.Limits.QuoteAgeMs)
		}
		receipts = append(receipts, obs.FirstReceiptMs)
		c := em.Capability
		if err := pairCapabilityMatches(m, c); err != nil {
			return env, fmt.Errorf("member %s: %w", m.MemberID, err)
		}
		if c.Key != key || !pairEligibleAt(c.pairEvidenceStamp, env.CutoffMs) {
			return env, fmt.Errorf("member %s capability %s is not bound to %s and eligible at the cutoff %d", m.MemberID, c.EvidenceID, key, env.CutoffMs)
		}
		norm, err := pairNormalizeObservation(pairCloneObservation(obs), c)
		if err != nil {
			return env, fmt.Errorf("member %s normalization: %w", m.MemberID, err)
		}
		if obs.NativeUnit != norm.NativeUnit || obs.ContractSize != norm.ContractSize || obs.PriceCurrency != norm.PriceCurrency || obs.NormalizationID != norm.NormalizationID {
			return env, fmt.Errorf("member %s normalization %q (%s × %s, %s) is not the capability normalization %q", m.MemberID, obs.NormalizationID, obs.NativeUnit, obs.ContractSize, obs.PriceCurrency, norm.NormalizationID)
		}
		if !reflect.DeepEqual(obs.Bids, norm.Bids) || !reflect.DeepEqual(obs.Asks, norm.Asks) {
			return env, fmt.Errorf("member %s base levels disagree with the native levels times the verified contract size", m.MemberID)
		}
		a := em.Account
		if err := pairAccountMatches(m, a); err != nil {
			return env, fmt.Errorf("member %s: %w", m.MemberID, err)
		}
		if a.Key != key || !pairEligibleAt(a.pairEvidenceStamp, env.CutoffMs) {
			return env, fmt.Errorf("member %s account %s is not bound to %s and eligible at the cutoff %d", m.MemberID, a.EvidenceID, key, env.CutoffMs)
		}
		required := pairRequiredConversionCurrencies(m)
		if len(em.Conversions) != len(required) {
			return env, fmt.Errorf("member %s carries %d conversions, it needs exactly %v", m.MemberID, len(em.Conversions), required)
		}
		for j, cv := range em.Conversions {
			if cv.Currency != required[j] {
				return env, fmt.Errorf("member %s conversion %d is %s, want %s (sorted by currency, then evidence id)", m.MemberID, j, cv.Currency, required[j])
			}
			if err := pairConversionMatches(m, cv); err != nil {
				return env, fmt.Errorf("member %s: %w", m.MemberID, err)
			}
			if cv.Key != key || !pairEligibleAt(cv.pairEvidenceStamp, env.CutoffMs) {
				return env, fmt.Errorf("member %s conversion %s is not bound to %s and eligible at the cutoff %d", m.MemberID, cv.EvidenceID, key, env.CutoffMs)
			}
		}
	}
	if !sealed && (env.SnapshotID != "" || env.Generation != 0) {
		return env, fmt.Errorf("an envelope with no sealed member carries snapshot %q generation %d", env.SnapshotID, env.Generation)
	}
	for i := 1; i < len(receipts); i++ {
		if !pairSkewWithin(receipts[0], receipts[i], exp.Limits.ReceiveSkewMs) {
			return env, fmt.Errorf("member receipts %d and %d differ by more than receive_skew_ms %d", receipts[0], receipts[i], exp.Limits.ReceiveSkewMs)
		}
	}
	return env, nil
}

func buildPairEnvelope(in pairEnvelopeInputs) (pairEnvelope, string) {
	exp := pairEnvelopeExpect{Partition: in.Partition.String(), PairID: in.PairID, ContractFingerprint: in.ContractFingerprint,
		Members: in.Members, BookNeed: in.BookNeed, Limits: in.Limits}
	members := exp.sortedMembers()
	if len(members) == 0 {
		return pairEnvelope{}, "the envelope has no members"
	}
	want := pairSourceContractMap(members)
	if len(in.SourceContracts) != len(want) {
		return pairEnvelope{}, fmt.Sprintf("the source-contract map names %d members, the pair has %d", len(in.SourceContracts), len(want))
	}
	for id, c := range want {
		if c == "" || in.SourceContracts[id] != c {
			return pairEnvelope{}, fmt.Sprintf("member %s source contract %q is not the compatibility table's %q", id, in.SourceContracts[id], c)
		}
	}
	byMember := func(n int, keyOf func(int) pairEvidenceKey, what string) (map[string]int, string) {
		out := map[string]int{}
		for i := 0; i < n; i++ {
			k := keyOf(i)
			if _, dup := out[k.MemberID]; dup {
				return nil, fmt.Sprintf("member %s has more than one %s", k.MemberID, what)
			}
			out[k.MemberID] = i
		}
		return out, ""
	}
	obsIdx, why := byMember(len(in.Observations), func(i int) pairEvidenceKey { return in.Observations[i].Key }, "observation")
	if why != "" {
		return pairEnvelope{}, why
	}
	capIdx, why := byMember(len(in.Capabilities), func(i int) pairEvidenceKey { return in.Capabilities[i].Key }, "capability record")
	if why != "" {
		return pairEnvelope{}, why
	}
	acctIdx, why := byMember(len(in.Accounts), func(i int) pairEvidenceKey { return in.Accounts[i].Key }, "account record")
	if why != "" {
		return pairEnvelope{}, why
	}
	if len(in.Observations) != len(members) || len(in.Capabilities) != len(members) || len(in.Accounts) != len(members) {
		return pairEnvelope{}, fmt.Sprintf("the envelope needs exactly one observation, capability and account per member (%d, %d, %d for %d members)", len(in.Observations), len(in.Capabilities), len(in.Accounts), len(members))
	}
	convs := map[string][]pairConversionEvidence{}
	for _, c := range in.Conversions {
		convs[c.Key.MemberID] = append(convs[c.Key.MemberID], c)
	}
	env := pairQuoteEnvelope{V: pairEnvelopeVersion, Partition: exp.Partition, PairID: in.PairID, ContractFingerprint: in.ContractFingerprint,
		SlotMs: in.SlotMs, StartMs: in.StartMs, DeadlineMs: in.DeadlineMs, CutoffMs: in.CutoffMs, SnapshotID: in.SnapshotID, Generation: in.Generation,
		Members: []pairEnvelopeMember{}}
	for _, m := range members {
		oi, ok1 := obsIdx[m.MemberID]
		ci, ok2 := capIdx[m.MemberID]
		ai, ok3 := acctIdx[m.MemberID]
		if !ok1 || !ok2 || !ok3 {
			return pairEnvelope{}, fmt.Sprintf("member %s is missing its observation, capability or account", m.MemberID)
		}
		c := in.Capabilities[ci]
		c.Meta = c.Meta.clone()
		obs, err := pairNormalizeObservation(pairCloneObservation(in.Observations[oi]), c)
		if err != nil {
			return pairEnvelope{}, fmt.Sprintf("member %s normalization: %v", m.MemberID, err)
		}
		mc := append([]pairConversionEvidence{}, convs[m.MemberID]...)
		delete(convs, m.MemberID)
		sort.SliceStable(mc, func(i, j int) bool {
			if mc[i].Currency != mc[j].Currency {
				return mc[i].Currency < mc[j].Currency
			}
			return mc[i].EvidenceID < mc[j].EvidenceID
		})
		env.Members = append(env.Members, pairEnvelopeMember{Key: exp.memberKey(m), Observation: obs, Capability: c, Conversions: mc, Account: in.Accounts[ai]})
	}
	if len(convs) > 0 {
		return pairEnvelope{}, "a conversion record names no pair member"
	}
	raw, err := pairCanonicalJSON(env)
	if err != nil {
		return pairEnvelope{}, "the envelope could not be encoded: " + err.Error()
	}
	if _, err := pairCheckEnvelope(exp, raw); err != nil {
		return pairEnvelope{}, err.Error()
	}
	return pairEnvelope{CutoffMs: in.CutoffMs, InputFP: pairSHA256(raw), EvidenceJSON: string(raw), SnapshotID: in.SnapshotID, Generation: in.Generation}, ""
}

func verifyPairEnvelope(rt *basketRuntime, rb *basketRebalance) (pairQuoteEnvelope, string) {
	if !rt.isPair() {
		return pairQuoteEnvelope{}, "envelope verification needs a pair runtime"
	}
	if rb == nil {
		return pairQuoteEnvelope{}, "the slot is missing"
	}
	if rb.CutoffMs <= 0 || rb.InputFP == "" || rb.EvidenceJSON == "" {
		return pairQuoteEnvelope{}, "the slot holds no frozen quote envelope"
	}
	if rb.ContractFP != rt.Contract.Fingerprint() {
		return pairQuoteEnvelope{}, fmt.Sprintf("the slot contract %s is not the running contract %s", shortFingerprint(rb.ContractFP), shortFingerprint(rt.Contract.Fingerprint()))
	}
	raw := []byte(rb.EvidenceJSON)
	if pairSHA256(raw) != rb.InputFP {
		return pairQuoteEnvelope{}, "the saved envelope bytes do not match their input fingerprint"
	}
	env, err := pairCheckEnvelope(pairEnvelopeExpectFor(rt), raw)
	if err != nil {
		return env, err.Error()
	}
	if env.SlotMs != rb.DeadlineMs || env.CutoffMs != rb.CutoffMs || env.SnapshotID != rb.SnapshotID || env.Generation != rb.Generation {
		return env, fmt.Sprintf("the envelope slot %d, cutoff %d, snapshot %q and generation %d disagree with the saved slot (%d, %d, %q, %d)",
			env.SlotMs, env.CutoffMs, env.SnapshotID, env.Generation, rb.DeadlineMs, rb.CutoffMs, rb.SnapshotID, rb.Generation)
	}
	return env, ""
}

func (e pairQuoteEnvelope) member(memberID string) (pairEnvelopeMember, bool) {
	for _, m := range e.Members {
		if m.Key.MemberID == memberID {
			return m, true
		}
	}
	return pairEnvelopeMember{}, false
}

func pairBoundCrosses(env pairQuoteEnvelope, memberID, side string, bound float64) bool {
	m, ok := env.member(memberID)
	if !ok || math.IsNaN(bound) || math.IsInf(bound, 0) || bound <= 0 {
		return false
	}
	b, err := quoteRatFromFloatDecimal(bound)
	if err != nil {
		return false
	}
	o := m.Observation
	switch side {
	case "buy":
		if len(o.NativeAsks) == 0 {
			return false
		}
		ask, ok := new(big.Rat).SetString(o.NativeAsks[0].Px)
		return ok && b.Cmp(ask) >= 0
	case "sell":
		if len(o.NativeBids) == 0 {
			return false
		}
		bid, ok := new(big.Rat).SetString(o.NativeBids[0].Px)
		return ok && b.Cmp(bid) <= 0
	}
	return false
}

func pairCheckReference(rt *basketRuntime, m pairContractMember, ref *pairObservation, side string, nowMs int64) error {
	if side != "buy" && side != "sell" {
		return fmt.Errorf("a reference needs the order side whose depth it proves, got %q", side)
	}
	key := pairEvidenceKey{Partition: rt.Partition.String(), Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID}
	if _, err := pairCheckObservationSourceSide(*ref, m, key, rt.Contract.Pair.BookNeed, side); err != nil {
		return err
	}
	if ref.NormalizationID != "" || len(ref.Bids) > 0 || len(ref.Asks) > 0 || ref.NativeUnit != "" || ref.ContractSize != "" || ref.PriceCurrency != "" {
		return fmt.Errorf("a reference carries native levels only; normalization belongs to the envelope builder")
	}
	if !pairAgeWithin(nowMs, ref.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs) {
		return fmt.Errorf("the reference receipt %d is after %d or older than quote_age_ms %d", ref.FirstReceiptMs, nowMs, rt.Pair.Limits.QuoteAgeMs)
	}
	return nil
}

func pairRequestKind(purpose string) string {
	switch purpose {
	case pairPurposeEntry:
		return "increase"
	case pairPurposeExit, pairPurposeProtective:
		return "coordinator_reduction"
	case pairPurposeKillSwitch, pairPurposeOperator:
		return "native_booked"
	}
	return ""
}

func pairValidateActionRequest(rt *basketRuntime, a *basketAction, use *pairQuoteEventUse, nowMs int64) (pairActionRequest, error) {
	var req pairActionRequest
	dec := json.NewDecoder(strings.NewReader(a.RequestJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return req, fmt.Errorf("pair action %s request is unreadable: %w", a.ActionID, err)
	}
	if req.V != pairActionRequestVersion {
		return req, fmt.Errorf("pair action %s request version %d is not the current %d; a saved older request is never reserved or resent as new authority", a.ActionID, req.V, pairActionRequestVersion)
	}
	m, ok := rt.Contract.pairMember(a.StrategyID)
	if !ok {
		return req, fmt.Errorf("pair action %s names %s, which the pair contract does not list", a.ActionID, a.StrategyID)
	}
	if req.PairID != rt.Contract.BasketID || req.Partition != rt.Partition.String() || req.MemberID != a.StrategyID || req.Provider != a.Provider ||
		req.AccountKey != a.AccountKey || req.Instrument != a.Instrument || req.ClientOrderID != a.ClientOrderID || req.Purpose != a.Purpose ||
		req.OrderSide != a.OrderSide || req.PositionSide != a.PositionSide || req.AcceptDeadlineMs != a.AcceptDeadlineMs || req.QuoteEventID != a.QuoteEventID ||
		req.Provider != m.Provider || req.Instrument != m.Instrument || req.AccountKey != m.AccountKey {
		return req, fmt.Errorf("pair action %s request identity does not match the action it reserves", a.ActionID)
	}
	if req.ContractFingerprint == "" || req.ContractFingerprint != rt.Contract.Fingerprint() {
		return req, fmt.Errorf("pair action %s request names contract %s, the running contract is %s", a.ActionID, shortFingerprint(req.ContractFingerprint), shortFingerprint(rt.Contract.Fingerprint()))
	}
	evidence := len(req.Conversions) > 0 || req.CapabilityHash != "" || req.EvidenceHash != ""
	switch pairRequestKind(a.Purpose) {
	case "increase":
		if a.Direction != basketDirectionIncrease || req.ReduceOnly || req.NativeBooked || a.recoveryNote().NativeBooked {
			return req, fmt.Errorf("pair increase %s must be a non-native, non-reduce-only increase", a.ActionID)
		}
		if use == nil || req.QuoteEventID == "" || req.QuoteEventID != use.EventID || use.EventID != use.Event.ID() || use.StrategyID != a.StrategyID {
			return req, fmt.Errorf("pair increase %s request must name its selected quote event", a.ActionID)
		}
		if len(req.Conversions) == 0 || !pairHexDigestPattern.MatchString(req.CapabilityHash) || req.EvidenceHash != use.Event.InputFingerprint || !pairHexDigestPattern.MatchString(req.EvidenceHash) {
			return req, fmt.Errorf("pair increase %s request needs conversions, a capability hash and the selected envelope's evidence hash", a.ActionID)
		}
		if req.Reference != nil {
			return req, fmt.Errorf("pair increase %s binds its quote event, not a reference", a.ActionID)
		}
		if err := pairCheckLiveRequest(rt, m, a, req); err != nil {
			return req, err
		}
	case "coordinator_reduction":
		if a.Direction != basketDirectionReduce || !req.ReduceOnly || req.NativeBooked || a.recoveryNote().NativeBooked || req.QuoteEventID != "" {
			return req, fmt.Errorf("pair reduction %s must be a reduce-only coordinator request with no quote event", a.ActionID)
		}
		if evidence {
			return req, fmt.Errorf("pair reduction %s carries quote-event evidence fields, which only an increase binds", a.ActionID)
		}
		if req.Reference != nil {
			if err := pairCheckReference(rt, m, req.Reference, req.OrderSide, nowMs); err != nil {
				return req, fmt.Errorf("pair reduction %s reference: %w", a.ActionID, err)
			}
		}
		if err := pairCheckLiveRequest(rt, m, a, req); err != nil {
			return req, err
		}
	case "native_booked":
		if a.Direction != basketDirectionReduce || !req.ReduceOnly || !req.NativeBooked || !a.recoveryNote().NativeBooked {
			return req, fmt.Errorf("pair native reduction %s needs native_booked with a matching recovery note and reduce_only", a.ActionID)
		}
		if evidence || req.QuoteEventID != "" || req.Reference != nil || req.PriceBound != 0 {
			return req, fmt.Errorf("pair native reduction %s carries quote evidence or a price bound; the native owner keeps booking authority", a.ActionID)
		}
	default:
		return req, fmt.Errorf("pair action %s purpose %q has no request rule", a.ActionID, a.Purpose)
	}
	if a.Direction == basketDirectionReduce && req.NativeBooked != a.recoveryNote().NativeBooked {
		return req, fmt.Errorf("pair action %s request native_booked disagrees with its recovery note", a.ActionID)
	}
	return req, nil
}
