package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pairVirtualRecordVersion    = 1
	pairVirtualClientPrefix     = "vp-"
	pairVirtualMarketTimeout    = 30 * time.Second
	pairVirtualBoundEvidence    = "decision_evidence"
	pairVirtualBoundGrid        = "venue_grid"
	pairVirtualMaxLatencyMs     = int64(24 * 60 * 60 * 1000)
	pairVirtualAdverseBandLimit = 10000.0
)

type pairVirtualVenueConfig struct {
	Partition         RiskPartition
	RunTag            string
	MemberID          string
	Provider          string
	AccountKey        string
	Instrument        string
	Market            pairVirtualMarket
	AdverseBandBps    float64
	SubmitLatencyMs   int64
	ResponseLatencyMs int64
}

type pairVirtualAttachment struct {
	Store   *StateStore
	Runtime *basketRuntime
	Config  StrategyConfig
	Clock   func() time.Time
}

type pairVirtualBindingBody struct {
	V                   int         `json:"v"`
	RunTag              string      `json:"run_tag"`
	PairID              string      `json:"pair_id"`
	ContractFingerprint string      `json:"contract_fingerprint"`
	Provider            string      `json:"provider"`
	AccountKey          string      `json:"account_key"`
	Instrument          string      `json:"instrument"`
	SourceContract      string      `json:"source_contract"`
	TimeSemantics       string      `json:"time_semantics"`
	IdentitySemantics   string      `json:"identity_semantics"`
	ExecutionModel      string      `json:"execution_model"`
	CostPolicyID        string      `json:"cost_policy_id"`
	CostModelVersion    int         `json:"cost_model_version"`
	TakerFeePct         pairDecimal `json:"taker_fee_pct"`
	SlippagePct         pairDecimal `json:"slippage_pct"`
	AdverseBandBps      pairDecimal `json:"adverse_band_bps"`
	AdverseRate         pairDecimal `json:"adverse_rate"`
	SubmitLatencyMs     int64       `json:"submit_latency_ms"`
	ResponseLatencyMs   int64       `json:"response_latency_ms"`
	ContractSize        pairDecimal `json:"contract_size"`
	LotStep             pairDecimal `json:"lot_step"`
	PriceCurrency       string      `json:"price_currency"`
	BookDepth           int         `json:"book_depth"`
}

type pairVirtualSubmitBody struct {
	V                int         `json:"v"`
	ClientOrderID    string      `json:"client_order_id"`
	Request          string      `json:"request"`
	RequestHash      string      `json:"request_hash"`
	Purpose          string      `json:"purpose"`
	OrderSide        string      `json:"order_side"`
	ReduceOnly       bool        `json:"reduce_only"`
	NativeQty        pairDecimal `json:"native_qty"`
	BaseQty          pairDecimal `json:"base_qty"`
	PriceBound       pairDecimal `json:"price_bound"`
	BoundSource      string      `json:"bound_source"`
	AcceptedAtMs     int64       `json:"accepted_at_ms"`
	AcceptDeadlineMs int64       `json:"accept_deadline_ms"`
	InventoryBase    pairDecimal `json:"inventory_base,omitempty"`
}

type pairVirtualUnsentBody struct {
	V             int    `json:"v"`
	ClientOrderID string `json:"client_order_id"`
	RequestHash   string `json:"request_hash,omitempty"`
	Reason        string `json:"reason"`
	DeadlineMs    int64  `json:"deadline_ms,omitempty"`
	AtMs          int64  `json:"at_ms"`
}

type pairVirtualFillRecord struct {
	FillID         string      `json:"fill_id"`
	LevelIndex     int         `json:"level_index"`
	NativePx       string      `json:"native_px"`
	AdversePx      pairDecimal `json:"adverse_px"`
	NativeQty      pairDecimal `json:"native_qty"`
	BaseQty        pairDecimal `json:"base_qty"`
	NativeNotional pairDecimal `json:"native_notional"`
}

type pairVirtualMatchBody struct {
	V             int                     `json:"v"`
	ClientOrderID string                  `json:"client_order_id"`
	DecidedAtMs   int64                   `json:"decided_at_ms"`
	Status        string                  `json:"status"`
	Side          string                  `json:"side"`
	AcceptedAtMs  int64                   `json:"accepted_at_ms"`
	VisibleMs     int64                   `json:"visible_ms"`
	AdverseRate   pairDecimal             `json:"adverse_rate"`
	RequestedQty  pairDecimal             `json:"requested_native_qty"`
	ContainedQty  pairDecimal             `json:"contained_native_qty"`
	InventoryBase pairDecimal             `json:"inventory_base"`
	CumNativeQty  pairDecimal             `json:"cum_native_qty"`
	CumBaseQty    pairDecimal             `json:"cum_base_qty"`
	CumNotional   pairDecimal             `json:"cum_native_notional"`
	Book          pairVirtualBook         `json:"book"`
	Fills         []pairVirtualFillRecord `json:"fills"`
	Digest        string                  `json:"digest"`
	Detail        string                  `json:"detail"`
}

type pairVirtualConsumeBody struct {
	V             int         `json:"v"`
	ClientOrderID string      `json:"client_order_id"`
	LevelPx       string      `json:"level_px"`
	LevelQty      string      `json:"level_qty"`
	AdversePx     pairDecimal `json:"adverse_px"`
}

type pairVirtualQueryBody struct {
	V             int    `json:"v"`
	ClientOrderID string `json:"client_order_id"`
	ThroughMs     int64  `json:"through_ms"`
	Digest        string `json:"digest"`
	Books         int    `json:"books"`
}

type pairVirtualAbsenceBody struct {
	V             int    `json:"v"`
	ClientOrderID string `json:"client_order_id"`
	FromMs        int64  `json:"coverage_from_ms"`
	ThroughMs     int64  `json:"coverage_through_ms"`
	DecidedAtMs   int64  `json:"decided_at_ms"`
	Digest        string `json:"digest"`
}

type pairVirtualVisibleBody struct {
	V             int    `json:"v"`
	ClientOrderID string `json:"client_order_id"`
	VisibleMs     int64  `json:"visible_ms"`
	PublishedAtMs int64  `json:"published_at_ms"`
}

func pairVirtualDecodeBody(raw string, v interface{}) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("pair virtual record body is unreadable: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("pair virtual record body has trailing data")
	}
	return nil
}

func pairVirtualEncodeBody(v interface{}) (string, error) {
	b, err := pairCanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type pairVirtualOrder struct {
	ClientOrderID string
	AcceptSeq     int64
	Submit        pairVirtualSubmitBody
	Match         *pairVirtualMatchBody
	Absence       *pairVirtualAbsenceBody
	LastQuery     *pairVirtualQueryBody
	Published     bool
}

type pairVirtualState struct {
	orders   map[string]*pairVirtualOrder
	accepted []string
	unsent   map[string]pairVirtualUnsentBody
	consumed map[string]*big.Rat
	maxMs    int64
}

func newPairVirtualState() *pairVirtualState {
	return &pairVirtualState{orders: map[string]*pairVirtualOrder{}, unsent: map[string]pairVirtualUnsentBody{}, consumed: map[string]*big.Rat{}}
}

func pairVirtualCapKey(contract, obs, side string, level int) string {
	return fmt.Sprintf("%s|%s|%s|%d", contract, obs, side, level)
}

func (s *pairVirtualState) apply(e pairVirtualEvent) error {
	if e.CreatedAtMs > s.maxMs {
		s.maxMs = e.CreatedAtMs
	}
	switch e.Kind {
	case pairVirtualKindBinding:
	case pairVirtualKindSubmit:
		var b pairVirtualSubmitBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		if b.ClientOrderID != e.ClientOrderID || s.orders[b.ClientOrderID] != nil {
			return fmt.Errorf("pair virtual acceptance %d for %s is repeated or mislabelled", e.Seq, e.ClientOrderID)
		}
		if _, gone := s.unsent[b.ClientOrderID]; gone {
			return fmt.Errorf("pair virtual acceptance %d for %s follows a never-sent record", e.Seq, e.ClientOrderID)
		}
		s.orders[b.ClientOrderID] = &pairVirtualOrder{ClientOrderID: b.ClientOrderID, AcceptSeq: e.Seq, Submit: b}
		s.accepted = append(s.accepted, b.ClientOrderID)
	case pairVirtualKindRefusal, pairVirtualKindDeadline:
		var b pairVirtualUnsentBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		if b.ClientOrderID != e.ClientOrderID || s.orders[b.ClientOrderID] != nil {
			return fmt.Errorf("pair virtual never-sent record %d for %s contradicts an acceptance", e.Seq, e.ClientOrderID)
		}
		s.unsent[b.ClientOrderID] = b
	case pairVirtualKindMatch:
		var b pairVirtualMatchBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		o := s.orders[b.ClientOrderID]
		if o == nil || o.Match != nil || o.Absence != nil || b.ClientOrderID != e.ClientOrderID {
			return fmt.Errorf("pair virtual match %d for %s has no open acceptance", e.Seq, e.ClientOrderID)
		}
		o.Match = &b
	case pairVirtualKindConsume:
		var b pairVirtualConsumeBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		if s.orders[e.ClientOrderID] == nil {
			return fmt.Errorf("pair virtual consumption %d names %s, which has no acceptance", e.Seq, e.ClientOrderID)
		}
		q, err := e.ConsumedNative.rat()
		if err != nil {
			return err
		}
		k := pairVirtualCapKey(e.SourceContract, e.ObservationID, e.BookSide, e.LevelIndex)
		if s.consumed[k] == nil {
			s.consumed[k] = new(big.Rat)
		}
		s.consumed[k].Add(s.consumed[k], q)
	case pairVirtualKindQuery:
		var b pairVirtualQueryBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		o := s.orders[e.ClientOrderID]
		if o == nil {
			return fmt.Errorf("pair virtual query %d names %s, which has no acceptance", e.Seq, e.ClientOrderID)
		}
		o.LastQuery = &b
	case pairVirtualKindAbsence:
		var b pairVirtualAbsenceBody
		if err := pairVirtualDecodeBody(e.Body, &b); err != nil {
			return err
		}
		o := s.orders[e.ClientOrderID]
		if o == nil || o.Match != nil || o.Absence != nil {
			return fmt.Errorf("pair virtual absence %d for %s has no open acceptance", e.Seq, e.ClientOrderID)
		}
		o.Absence = &b
	case pairVirtualKindVisible:
		o := s.orders[e.ClientOrderID]
		if o == nil || o.Match == nil {
			return fmt.Errorf("pair virtual visibility %d for %s has no saved match", e.Seq, e.ClientOrderID)
		}
		o.Published = true
	default:
		return fmt.Errorf("pair virtual record %d has unknown kind %q", e.Seq, e.Kind)
	}
	return nil
}

type pairVirtualVenue struct {
	cfg pairVirtualVenueConfig

	mu       sync.Mutex
	attached bool
	st       *StateStore
	rt       *basketRuntime
	member   pairContractMember
	need     pairBookNeed
	id       pairVirtualIdentity
	binding  string
	rate     *big.Rat
	cs       *big.Rat
	lot      *big.Rat
	clock    func() time.Time
	state    *pairVirtualState
	native   []pairInventoryRow
	nativeOK bool

	posMu    sync.Mutex
	posCalls int

	visMu    sync.Mutex
	visClock func() time.Time
	visible  map[string]int64
}

func newPairVirtualVenue(c pairVirtualVenueConfig) (*pairVirtualVenue, error) {
	switch {
	case !basketIDPattern.MatchString(c.RunTag):
		return nil, fmt.Errorf("pair virtual venue run tag %q must match %s", c.RunTag, basketIDPattern.String())
	case strings.TrimSpace(c.MemberID) == "":
		return nil, fmt.Errorf("pair virtual venue needs its member id")
	case c.Provider != entryProviderHyperliquid && c.Provider != entryProviderOKX:
		return nil, fmt.Errorf("pair virtual venue provider %q is neither %s nor %s", c.Provider, entryProviderHyperliquid, entryProviderOKX)
	case !strings.HasPrefix(c.AccountKey, pairSyntheticAccountPrefix) || len(c.AccountKey) <= len(pairSyntheticAccountPrefix):
		return nil, fmt.Errorf("pair virtual venue account %q must be a synthetic research account (%s<name>)", c.AccountKey, pairSyntheticAccountPrefix)
	case strings.TrimSpace(c.Instrument) == "":
		return nil, fmt.Errorf("pair virtual venue needs its instrument")
	case c.Market == nil:
		return nil, fmt.Errorf("pair virtual venue needs its recorded market")
	case math.IsNaN(c.AdverseBandBps) || math.IsInf(c.AdverseBandBps, 0) || c.AdverseBandBps < 0 || c.AdverseBandBps >= pairVirtualAdverseBandLimit:
		return nil, fmt.Errorf("pair virtual venue adverse band %v bps is outside [0, %v)", c.AdverseBandBps, pairVirtualAdverseBandLimit)
	case c.SubmitLatencyMs < 0 || c.ResponseLatencyMs < 0 || c.SubmitLatencyMs > pairVirtualMaxLatencyMs || c.ResponseLatencyMs > pairVirtualMaxLatencyMs:
		return nil, fmt.Errorf("pair virtual venue latencies (submit %d, response %d ms) must lie in [0, %d]", c.SubmitLatencyMs, c.ResponseLatencyMs, pairVirtualMaxLatencyMs)
	}
	if err := validEntryPartition(c.Partition); err != nil {
		return nil, fmt.Errorf("pair virtual venue partition: %w", err)
	}
	if c.Partition.IsLive() {
		return nil, fmt.Errorf("pair virtual venue binds a research partition, never live")
	}
	return &pairVirtualVenue{cfg: c, visible: map[string]int64{}}, nil
}

func (v *pairVirtualVenue) Name() string                    { return "virtual-" + v.cfg.Provider }
func (v *pairVirtualVenue) LiveShaped() bool                { return false }
func (v *pairVirtualVenue) Virtual() bool                   { return true }
func (v *pairVirtualVenue) ExecutionModel() string          { return pairExecutionVirtualIOCAdverseMax }
func (v *pairVirtualVenue) Provider() string                { return v.cfg.Provider }
func (v *pairVirtualVenue) AccountKey(RiskPartition) string { return v.cfg.AccountKey }

func (v *pairVirtualVenue) Position(ctx context.Context, m pairContractMember, nowMs int64) (pairVenuePosition, error) {
	v.posMu.Lock()
	v.posCalls++
	v.posMu.Unlock()
	return pairVenuePosition{}, fmt.Errorf("a virtual research venue makes no private position read")
}

func (v *pairVirtualVenue) positionCalls() int {
	v.posMu.Lock()
	defer v.posMu.Unlock()
	return v.posCalls
}

func (v *pairVirtualVenue) isAttached() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attached
}

func pairVirtualPolicyBand(rt *basketRuntime) (float64, bool) {
	if rt.Contract.Pair == nil {
		return 0, false
	}
	has := false
	for _, k := range pairPolicyParamKeys[rt.Contract.Pair.Policy] {
		if k == "adverse_band_bps" {
			has = true
		}
	}
	if !has {
		return 0, false
	}
	switch x := rt.Contract.Pair.Params["adverse_band_bps"].(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f, true
		}
	}
	return math.NaN(), true
}

func (v *pairVirtualVenue) attach(a pairVirtualAttachment) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.attached {
		return fmt.Errorf("pair virtual venue for %s is already attached; attachment is one-time", v.cfg.MemberID)
	}
	rt := a.Runtime
	if a.Store == nil || rt == nil || !rt.isPair() || a.Clock == nil {
		return fmt.Errorf("pair virtual venue attachment needs the state store, the loaded pair runtime and the coordinator clock")
	}
	adm := rt.Pair.admission
	if !adm.valid() || adm.kind != pairAdmissionResearch {
		return fmt.Errorf("pair virtual venue attaches only to a valid research admission")
	}
	if adm.runID != v.cfg.RunTag {
		return fmt.Errorf("pair virtual venue run tag %s is not the admitted research run %s", v.cfg.RunTag, adm.runID)
	}
	if rt.Partition != v.cfg.Partition {
		return fmt.Errorf("pair virtual venue partition %s is not the pair partition %s", partitionLabel(v.cfg.Partition), partitionLabel(rt.Partition))
	}
	m, ok := rt.Contract.pairMember(v.cfg.MemberID)
	if !ok {
		return fmt.Errorf("pair virtual venue member %s is not a member of pair %s", v.cfg.MemberID, rt.Contract.BasketID)
	}
	if m.Provider != v.cfg.Provider || m.AccountKey != v.cfg.AccountKey || m.Instrument != v.cfg.Instrument {
		return fmt.Errorf("pair virtual venue %s names %s %s %s, the frozen member is %s %s %s", v.cfg.MemberID, v.cfg.Provider, v.cfg.AccountKey, v.cfg.Instrument, m.Provider, m.AccountKey, m.Instrument)
	}
	if a.Config.ID != m.MemberID || partitionFor(a.Config) != rt.Partition {
		return fmt.Errorf("pair virtual venue %s attaches with strategy %s in %s", v.cfg.MemberID, a.Config.ID, partitionLabel(partitionFor(a.Config)))
	}
	if err := adm.checkTransport(a.Config, v); err != nil {
		return err
	}
	mc, err := adm.memberCost(m.MemberID)
	if err != nil {
		return err
	}
	if mc.model != pairExecutionVirtualIOCAdverseMax || mc.policy.ModelVersion != FillCostModelReplayCustomVersion {
		return fmt.Errorf("pair virtual venue %s cost binding has model %s version %d", m.MemberID, mc.model, mc.policy.ModelVersion)
	}
	if band, ok := pairVirtualPolicyBand(rt); ok && band != v.cfg.AdverseBandBps {
		return fmt.Errorf("pair virtual venue adverse band %v bps is not the frozen policy adverse_band_bps %v", v.cfg.AdverseBandBps, band)
	}
	spec, ok := pairSourceSpecFor(m.Provider, m.Source)
	if !ok {
		return fmt.Errorf("pair virtual venue member %s source %q has no source contract", m.MemberID, m.Source)
	}
	meta, err := pairExpectedWireMeta(m)
	if err != nil {
		return err
	}
	cs, err := meta.ContractSize.rat()
	if err != nil || cs.Sign() <= 0 {
		return fmt.Errorf("pair virtual venue member %s contract size %s is not positive", m.MemberID, meta.ContractSize)
	}
	lot, err := meta.LotStep.rat()
	if meta.LotStepText != "" {
		lot, err = pairParseDecimalInput(meta.LotStepText)
	}
	if err != nil || lot.Sign() <= 0 {
		return fmt.Errorf("pair virtual venue member %s lot step is not positive", m.MemberID)
	}
	band, err := pairDecimalFromFloat(v.cfg.AdverseBandBps)
	if err != nil {
		return err
	}
	bandRat, err := band.rat()
	if err != nil {
		return err
	}
	rate := new(big.Rat).Quo(bandRat, big.NewRat(10000, 1))
	slip, err := pairDecimalFromFloat(m.CostPolicy.SlippagePct)
	if err != nil {
		return err
	}
	slipRat, err := slip.rat()
	if err != nil {
		return err
	}
	if slipRat.Cmp(rate) > 0 {
		rate = slipRat
	}
	if rate.Sign() < 0 || rate.Cmp(big.NewRat(1, 1)) >= 0 {
		return fmt.Errorf("pair virtual venue adverse rate %s is outside [0, 1)", rate.RatString())
	}
	rateDec, err := pairRatDecimal(rate)
	if err != nil {
		return err
	}
	fee, err := pairDecimalFromFloat(m.CostPolicy.TakerFeePct)
	if err != nil {
		return err
	}
	body := pairVirtualBindingBody{V: pairVirtualRecordVersion, RunTag: v.cfg.RunTag, PairID: rt.Contract.BasketID, ContractFingerprint: rt.Contract.Fingerprint(),
		Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
		ExecutionModel: mc.model, CostPolicyID: mc.policy.PolicyID, CostModelVersion: mc.policy.ModelVersion, TakerFeePct: fee, SlippagePct: slip,
		AdverseBandBps: band, AdverseRate: pairDecimal(rateDec), SubmitLatencyMs: v.cfg.SubmitLatencyMs, ResponseLatencyMs: v.cfg.ResponseLatencyMs,
		ContractSize: meta.ContractSize, LotStep: pairDecimalOfRat(lot), PriceCurrency: m.Meta.PriceCurrency, BookDepth: rt.Contract.Pair.BookNeed.Depth}
	binding, err := pairVirtualEncodeBody(body)
	if err != nil {
		return err
	}
	id := pairVirtualIdentity{RunTag: v.cfg.RunTag, Partition: rt.Partition, PairID: rt.Contract.BasketID, MemberID: m.MemberID, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument}
	ledger, err := a.Store.LoadPairVirtualLedger(rt, id)
	if err != nil {
		return fmt.Errorf("pair virtual venue %s saved history is refused: %w", m.MemberID, err)
	}
	if len(ledger.Events) > 0 && ledger.Events[0].Body != binding {
		return fmt.Errorf("%w: pair virtual venue %s saved binding differs from this attachment (contract, cost, source, adverse rate or latency); the saved history is kept unchanged", errPairVirtualBinding, m.MemberID)
	}
	state := newPairVirtualState()
	for _, e := range ledger.Events {
		if err := state.apply(e); err != nil {
			return fmt.Errorf("pair virtual venue %s saved history is refused: %w", m.MemberID, err)
		}
	}
	v.st, v.rt, v.member, v.need, v.id, v.binding, v.rate, v.cs, v.lot, v.clock, v.state = a.Store, rt, m, rt.Contract.Pair.BookNeed, id, binding, rate, cs, lot, a.Clock, state
	v.attached = true
	v.refreshVisibleLocked()
	return nil
}

func (v *pairVirtualVenue) refreshVisibleLocked() {
	vis := map[string]int64{}
	for id, o := range v.state.orders {
		if o.Match != nil {
			vis[id] = o.Match.VisibleMs
		}
	}
	v.visMu.Lock()
	v.visible, v.visClock = vis, v.clock
	v.visMu.Unlock()
}

func (v *pairVirtualVenue) NextVisibleMs() (int64, bool) {
	v.visMu.Lock()
	vis := make([]int64, 0, len(v.visible))
	for _, ms := range v.visible {
		vis = append(vis, ms)
	}
	clock := v.visClock
	v.visMu.Unlock()
	if clock == nil {
		return 0, false
	}
	now := clock().UnixMilli()
	best, ok := int64(0), false
	for _, ms := range vis {
		if ms > now && (!ok || ms < best) {
			best, ok = ms, true
		}
	}
	return best, ok
}

func (v *pairVirtualVenue) noteNativeInventory(rows []pairInventoryRow) {
	keep := make([]pairInventoryRow, 0, len(rows))
	for _, r := range rows {
		if r.Origin == pairInventoryOriginCoordinator {
			continue
		}
		r.qty = nil
		keep = append(keep, r)
	}
	v.mu.Lock()
	v.native, v.nativeOK = keep, true
	v.mu.Unlock()
}

func (v *pairVirtualVenue) costGate() string {
	costs := pairCostBindings(v.rt.Pair.admission)
	if _, ok := costs[v.cfg.MemberID]; !ok {
		return "the member cost binding is not durable yet, so the virtual venue executes nothing"
	}
	return ""
}

func (v *pairVirtualVenue) memberHash() string {
	k := pairMemberKey{Partition: v.cfg.Partition, Provider: v.cfg.Provider, AccountKey: v.cfg.AccountKey, Instrument: v.cfg.Instrument, MemberID: v.cfg.MemberID}
	return pairSHA256([]byte(k.String()))[:8]
}

func (v *pairVirtualVenue) NewClientOrderID() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.attached {
		return "", fmt.Errorf("the pair virtual venue for %s is not attached to its state store; no identifier is allocated", v.cfg.MemberID)
	}
	if why := v.costGate(); why != "" {
		return "", fmt.Errorf("%s; no identifier is allocated", why)
	}
	prefix := pairVirtualClientPrefix + v.cfg.RunTag + "-" + v.memberHash() + "-"
	return v.st.AllocatePairVirtualID(v.rt, v.id, v.binding, v.clock().UnixMilli(), func(seq int64) string { return fmt.Sprintf("%s%d", prefix, seq) })
}

func (v *pairVirtualVenue) commit(events []pairVirtualEvent, caps []pairVirtualCapacity, nowMs int64) error {
	saved, err := v.st.AppendPairVirtualEvents(v.rt, v.id, v.binding, events, caps, nowMs)
	if err != nil {
		if rerr := v.reloadLocked(); rerr != nil {
			return fmt.Errorf("%v; reloading the saved history also failed: %w", err, rerr)
		}
		return err
	}
	for _, e := range saved {
		if err := v.state.apply(e); err != nil {
			if rerr := v.reloadLocked(); rerr != nil {
				return fmt.Errorf("%v; reloading the saved history also failed: %w", err, rerr)
			}
			return err
		}
	}
	v.refreshVisibleLocked()
	return nil
}

func (v *pairVirtualVenue) reloadLocked() error {
	ledger, err := v.st.LoadPairVirtualLedger(v.rt, v.id)
	if err != nil {
		return err
	}
	state := newPairVirtualState()
	for _, e := range ledger.Events {
		if err := state.apply(e); err != nil {
			return err
		}
	}
	v.state = state
	v.refreshVisibleLocked()
	return nil
}

func (v *pairVirtualVenue) unknown(detail string, nowMs int64) basketVenueOutcome {
	return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: detail, ObservedAtMs: nowMs}
}

func (v *pairVirtualVenue) neverSent(detail string, nowMs int64) basketVenueOutcome {
	return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: detail, ObservedAtMs: nowMs}
}

func (v *pairVirtualVenue) actionIdentity(a *basketAction) string {
	switch {
	case a == nil:
		return "the action is missing"
	case a.StrategyID != v.cfg.MemberID || a.Provider != v.cfg.Provider || a.AccountKey != v.cfg.AccountKey || a.Instrument != v.cfg.Instrument:
		return fmt.Sprintf("action %s names %s %s %s %s, not this venue's member %s", a.ActionID, a.StrategyID, a.Provider, a.AccountKey, a.Instrument, v.cfg.MemberID)
	case a.Partition != v.cfg.Partition:
		return fmt.Sprintf("action %s is in %s, the venue binds %s", a.ActionID, partitionLabel(a.Partition), partitionLabel(v.cfg.Partition))
	case !strings.HasPrefix(a.ClientOrderID, pairVirtualClientPrefix+v.cfg.RunTag+"-"):
		return fmt.Sprintf("client %s was not allocated by this venue's run %s", a.ClientOrderID, v.cfg.RunTag)
	}
	return ""
}

func (v *pairVirtualVenue) nowLocked() (int64, string) {
	now := v.clock().UnixMilli()
	if now < v.state.maxMs {
		return now, fmt.Sprintf("the clock %d is before the latest saved venue record at %d; a clock regression never exposes saved execution earlier", now, v.state.maxMs)
	}
	return now, ""
}

func pairVirtualRequestHash(raw string) string {
	return pairSHA256([]byte(raw))
}

func (v *pairVirtualVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.attached {
		return basketVenueOutcome{}, fmt.Errorf("the pair virtual venue for %s is not attached; nothing is accepted", v.cfg.MemberID)
	}
	now, why := v.nowLocked()
	if why != "" {
		return v.unknown(why, now), nil
	}
	if why := v.actionIdentity(a); why != "" {
		return v.neverSent(why+"; nothing was accepted", now), nil
	}
	if why := v.costGate(); why != "" {
		return v.unknown(why, now), nil
	}
	if o := v.state.orders[a.ClientOrderID]; o != nil {
		if o.Submit.RequestHash != pairVirtualRequestHash(a.RequestJSON) {
			return v.unknown(fmt.Sprintf("client %s was accepted with another request; a changed request never replaces it", a.ClientOrderID), now), nil
		}
		return v.evaluateLocked(o, now), nil
	}
	if u, gone := v.state.unsent[a.ClientOrderID]; gone {
		return v.neverSent("the venue saved this client as never sent: "+u.Reason, now), nil
	}
	if a.AcceptDeadlineMs <= 0 || !pairBeforeDeadline(now, a.AcceptDeadlineMs) {
		return v.recordUnsent(a, pairVirtualKindDeadline, fmt.Sprintf("the submission at %d is at or after the acceptance deadline %d", now, a.AcceptDeadlineMs), now), nil
	}
	body, why, transient := v.validateLocked(a, now)
	if transient != "" {
		return v.unknown(transient, now), nil
	}
	if why != "" {
		return v.recordUnsent(a, pairVirtualKindRefusal, why, now), nil
	}
	raw, err := pairVirtualEncodeBody(body)
	if err != nil {
		return v.unknown(err.Error(), now), nil
	}
	e := pairVirtualEvent{Kind: pairVirtualKindSubmit, SemanticKey: "submit|" + a.ClientOrderID, ClientOrderID: a.ClientOrderID, RequestHash: body.RequestHash, Body: raw, CreatedAtMs: now}
	if err := v.commit([]pairVirtualEvent{e}, nil, now); err != nil {
		if o := v.state.orders[a.ClientOrderID]; o != nil && o.Submit.RequestHash == body.RequestHash {
			return v.evaluateLocked(o, now), nil
		}
		return v.unknown("the acceptance could not be saved: "+err.Error(), now), nil
	}
	return v.evaluateLocked(v.state.orders[a.ClientOrderID], now), nil
}

func (v *pairVirtualVenue) recordUnsent(a *basketAction, kind, reason string, now int64) basketVenueOutcome {
	if v.state.orders[a.ClientOrderID] != nil {
		return v.unknown("client "+a.ClientOrderID+" already holds an acceptance", now)
	}
	body := pairVirtualUnsentBody{V: pairVirtualRecordVersion, ClientOrderID: a.ClientOrderID, Reason: reason, AtMs: now}
	if kind == pairVirtualKindDeadline {
		body.DeadlineMs = a.AcceptDeadlineMs
	} else {
		body.RequestHash = pairVirtualRequestHash(a.RequestJSON)
	}
	raw, err := pairVirtualEncodeBody(body)
	if err != nil {
		return v.unknown(err.Error(), now)
	}
	e := pairVirtualEvent{Kind: kind, SemanticKey: "unsent|" + a.ClientOrderID, ClientOrderID: a.ClientOrderID, RequestHash: body.RequestHash, Body: raw, CreatedAtMs: now}
	if err := v.commit([]pairVirtualEvent{e}, nil, now); err != nil {
		if u, gone := v.state.unsent[a.ClientOrderID]; gone {
			return v.neverSent(u.Reason, now)
		}
		return v.unknown("the never-sent record could not be saved: "+err.Error(), now)
	}
	return v.neverSent(reason+"; nothing was accepted", now)
}

func (v *pairVirtualVenue) validateLocked(a *basketAction, now int64) (pairVirtualSubmitBody, string, string) {
	var body pairVirtualSubmitBody
	var req pairActionRequest
	dec := json.NewDecoder(strings.NewReader(a.RequestJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return body, fmt.Sprintf("the saved request is unreadable: %v", err), ""
	}
	m, rt := v.member, v.rt
	switch {
	case req.V != pairActionRequestVersion:
		return body, fmt.Sprintf("request version %d is not %d", req.V, pairActionRequestVersion), ""
	case req.PairID != rt.Contract.BasketID || req.Partition != v.cfg.Partition.String() || req.MemberID != m.MemberID || req.Provider != m.Provider ||
		req.AccountKey != m.AccountKey || req.Instrument != m.Instrument || req.ClientOrderID != a.ClientOrderID || req.Purpose != a.Purpose ||
		req.OrderSide != a.OrderSide || req.PositionSide != a.PositionSide || req.AcceptDeadlineMs != a.AcceptDeadlineMs:
		return body, "the request identity does not match the action and the attached member", ""
	case req.ContractFingerprint != rt.Contract.Fingerprint():
		return body, fmt.Sprintf("the request names contract %s, the attached contract is %s", shortFingerprint(req.ContractFingerprint), shortFingerprint(rt.Contract.Fingerprint())), ""
	case req.NativeBooked || a.recoveryNote().NativeBooked:
		return body, "a native-booked action stays with its native owner and never enters virtual submission", ""
	}
	increase := a.Direction == basketDirectionIncrease
	switch a.Purpose {
	case pairPurposeEntry:
		if !increase || req.ReduceOnly {
			return body, "an entry must be a non-reduce-only increase", ""
		}
	case pairPurposeExit, pairPurposeProtective:
		if a.Direction != basketDirectionReduce || !req.ReduceOnly {
			return body, "a coordinator reduction must be reduce-only", ""
		}
	default:
		return body, fmt.Sprintf("purpose %q never enters virtual submission", a.Purpose), ""
	}
	sideOK := (req.OrderSide == "buy" && req.PositionSide == entrySideLong) || (req.OrderSide == "sell" && req.PositionSide == entrySideShort)
	if !increase {
		sideOK = (req.OrderSide == "sell" && req.PositionSide == entrySideLong) || (req.OrderSide == "buy" && req.PositionSide == entrySideShort)
	}
	if !sideOK {
		return body, fmt.Sprintf("order side %s does not fit position side %s for a %s", req.OrderSide, req.PositionSide, a.Direction), ""
	}
	if req.BaseQty != a.RequestedQty || req.NativeQty != a.NativeQty || req.ContractSize != m.Meta.ContractSize || req.PriceBound != a.PriceBound {
		return body, "the request quantities, contract size or bound differ from the reserved action", ""
	}
	nat, err := quoteRatFromFloatDecimal(req.NativeQty)
	if err != nil || nat.Sign() <= 0 {
		return body, fmt.Sprintf("native quantity %v is not finite and positive", req.NativeQty), ""
	}
	base, err := quoteRatFromFloatDecimal(req.BaseQty)
	if err != nil || base.Sign() <= 0 {
		return body, fmt.Sprintf("base quantity %v is not finite and positive", req.BaseQty), ""
	}
	if f := pairRatFloat(new(big.Rat).Mul(nat, v.cs)); math.Abs(f-req.BaseQty) > 1e-12*math.Max(1, req.BaseQty) {
		return body, fmt.Sprintf("native %v times contract size %s is %v, not the base quantity %v", req.NativeQty, v.cs.RatString(), f, req.BaseQty), ""
	}
	var invText pairDecimal
	if err := pairCheckExactRequestQty(m, req); err != nil {
		if increase {
			return body, "the increase quantity is not on the venue lot grid: " + err.Error(), ""
		}
		lo, hi, ok := v.inventoryLocked(now, "")
		if !ok {
			return body, "", "the member inventory is not known to the venue, so an off-lot reduction cannot be checked"
		}
		if lo.Cmp(hi) != 0 || new(big.Rat).Abs(lo).Cmp(base) != 0 || (lo.Sign() > 0) != (req.OrderSide == "sell") {
			return body, fmt.Sprintf("the off-lot reduction %v is not the member's full known exposure (%s to %s base)", req.BaseQty, lo.RatString(), hi.RatString()), ""
		}
		invText = pairDecimalOfRat(lo)
	}
	if !basketFinitePositive(req.PriceBound) {
		return body, "the request has no finite positive price bound; the virtual venue never fills an uncapped request", ""
	}
	if err := pairBoundOnGrid(m, req.OrderSide, req.PriceBound); err != nil {
		return body, "the request bound is not a legal venue price: " + err.Error(), ""
	}
	source := pairVirtualBoundGrid
	raw, found, err := v.st.PairVirtualSlotTargets(rt, req.OriginSlotMs)
	if err != nil {
		return body, "", "the origin slot decision is unreadable: " + err.Error()
	}
	var t *pairTargets
	if found {
		if t, err = parsePairTargets(raw); err != nil {
			return body, "the origin slot decision is unreadable: " + err.Error(), ""
		}
	}
	matches := t != nil && ((a.Purpose == pairPurposeEntry && t.Kind == pairPolicyStatusEnter) || (a.Purpose == pairPurposeExit && t.Kind == pairPolicyStatusExit))
	switch {
	case matches && t.legacy():
		return body, fmt.Sprintf("slot %d holds a version-1 decision record, which authorizes no order", req.OriginSlotMs), ""
	case matches:
		ev, ok := t.BoundEvidence[m.MemberID]
		tg, okT := t.Members[m.MemberID]
		if !ok || !okT || ev.RoundedBound != req.PriceBound || tg.PriceBound != req.PriceBound || ev.Side != req.OrderSide || !pairHexDigestPattern.MatchString(ev.CapabilityHash) {
			return body, fmt.Sprintf("the %s bound %v is not the saved rounded bound evidence of slot %d", req.OrderSide, req.PriceBound, req.OriginSlotMs), ""
		}
		source = pairVirtualBoundEvidence
	case a.Purpose == pairPurposeEntry:
		return body, fmt.Sprintf("the entry names slot %d, which holds no saved entry decision", req.OriginSlotMs), ""
	}
	row, err := v.st.PairVirtualActionRow(rt, v.id, a.ClientOrderID)
	if err != nil {
		return body, err.Error(), ""
	}
	if !row.Found || row.RequestJSON != a.RequestJSON || !basketActionPending(row.Status) || row.Status == basketActionReserved {
		return body, fmt.Sprintf("client %s has no durable submitting action with this request (found %v, status %q)", a.ClientOrderID, row.Found, row.Status), ""
	}
	ledger, err := v.st.LoadPairVirtualLedger(rt, v.id)
	if err != nil {
		return body, "", "the allocation history is unreadable: " + err.Error()
	}
	allocated := false
	for _, al := range ledger.Allocations {
		if al.ClientOrderID == a.ClientOrderID {
			allocated = true
		}
	}
	if !allocated {
		return body, fmt.Sprintf("client %s was not allocated by this venue for run %s", a.ClientOrderID, v.cfg.RunTag), ""
	}
	nd, err := pairRatDecimal(nat)
	if err != nil {
		return body, err.Error(), ""
	}
	bd, err := pairRatDecimal(base)
	if err != nil {
		return body, err.Error(), ""
	}
	pb, err := pairDecimalFromFloat(req.PriceBound)
	if err != nil {
		return body, err.Error(), ""
	}
	body = pairVirtualSubmitBody{V: pairVirtualRecordVersion, ClientOrderID: a.ClientOrderID, Request: a.RequestJSON, RequestHash: pairVirtualRequestHash(a.RequestJSON),
		Purpose: a.Purpose, OrderSide: req.OrderSide, ReduceOnly: req.ReduceOnly, NativeQty: pairDecimal(nd), BaseQty: pairDecimal(bd), PriceBound: pb, BoundSource: source,
		AcceptedAtMs: now, AcceptDeadlineMs: req.AcceptDeadlineMs, InventoryBase: invText}
	return body, "", ""
}

func (v *pairVirtualVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.attached {
		return basketVenueOutcome{}, fmt.Errorf("the pair virtual venue for %s is not attached", v.cfg.MemberID)
	}
	now, why := v.nowLocked()
	if why != "" {
		return v.unknown(why, now), nil
	}
	if why := v.actionIdentity(a); why != "" {
		return basketVenueOutcome{}, fmt.Errorf("%s", why)
	}
	if o := v.state.orders[a.ClientOrderID]; o != nil {
		if o.Submit.RequestHash != pairVirtualRequestHash(a.RequestJSON) {
			return v.unknown(fmt.Sprintf("client %s was accepted with another request; the saved acceptance stays authoritative", a.ClientOrderID), now), nil
		}
		return v.evaluateLocked(o, now), nil
	}
	if u, gone := v.state.unsent[a.ClientOrderID]; gone {
		return v.neverSent("the venue saved this client as never sent: "+u.Reason, now), nil
	}
	if a.AcceptDeadlineMs > 0 && pairBeforeDeadline(now, a.AcceptDeadlineMs) {
		return v.unknown("the venue holds no acceptance for this client yet; before the acceptance deadline a submission could still arrive", now), nil
	}
	return v.recordUnsent(a, pairVirtualKindDeadline, fmt.Sprintf("the venue holds no acceptance and the acceptance deadline %d has passed at %d", a.AcceptDeadlineMs, now), now), nil
}

func (v *pairVirtualVenue) evaluateLocked(o *pairVirtualOrder, now int64) basketVenueOutcome {
	if o.Match == nil && o.Absence == nil {
		for _, id := range v.state.accepted {
			p := v.state.orders[id]
			if p == o {
				break
			}
			if p.Match == nil && p.Absence == nil {
				if why := v.decideLocked(p, now); why != "" {
					return v.unknown("an earlier accepted order for this member is undecidable: "+why, now)
				}
			}
		}
		if why := v.decideLocked(o, now); why != "" {
			return v.unknown(why, now)
		}
		o = v.state.orders[o.ClientOrderID]
	}
	return v.publishLocked(o, now)
}

func (v *pairVirtualVenue) publishLocked(o *pairVirtualOrder, now int64) basketVenueOutcome {
	out := basketVenueOutcome{ClientOrderID: o.ClientOrderID, AccountKey: v.cfg.AccountKey, Instrument: v.cfg.Instrument, Side: o.Submit.OrderSide,
		AcceptedAtMs: o.Submit.AcceptedAtMs, ObservedAtMs: now}
	switch {
	case o.Absence != nil:
		out.Outcome, out.AcceptedAtMs, out.Side, out.Instrument, out.ClientOrderID = pairVenueAbsent, 0, "", "", ""
		out.CoverageComplete, out.CoverageThroughMs = true, o.Absence.ThroughMs
		out.Detail = fmt.Sprintf("no eligible recorded book before the deadline; continuous matching history covers [%d, %d]", o.Absence.FromMs, o.Absence.ThroughMs)
		return out
	case o.Match == nil || now < o.Match.VisibleMs:
		out.Outcome = pairVenueOpen
		out.Detail = "accepted; no execution result is visible yet"
		return out
	}
	mb := o.Match
	out.Outcome, out.Terminal, out.Detail = mb.Status, true, mb.Detail
	cq, _ := mb.CumBaseQty.rat()
	cn, _ := mb.CumNotional.rat()
	out.CumQty, out.CumNotional = pairRatFloat(cq), pairRatFloat(cn)
	for _, f := range mb.Fills {
		bq, _ := f.BaseQty.rat()
		out.Fills = append(out.Fills, pairVenueFill{FillID: f.FillID, BaseQty: pairRatFloat(bq), EventMs: mb.Book.EventMs, VisibleMs: mb.VisibleMs})
	}
	if !o.Published {
		raw, err := pairVirtualEncodeBody(pairVirtualVisibleBody{V: pairVirtualRecordVersion, ClientOrderID: o.ClientOrderID, VisibleMs: mb.VisibleMs, PublishedAtMs: now})
		if err == nil {
			_ = v.commit([]pairVirtualEvent{{Kind: pairVirtualKindVisible, SemanticKey: "visible|" + o.ClientOrderID, ClientOrderID: o.ClientOrderID, VisibleMs: mb.VisibleMs, Body: raw, CreatedAtMs: now}}, nil, now)
		}
	}
	return out
}

func (v *pairVirtualVenue) inventoryLocked(t int64, exclude string) (*big.Rat, *big.Rat, bool) {
	if !v.nativeOK {
		return nil, nil, false
	}
	base := new(big.Rat)
	excludeSeq := int64(math.MaxInt64)
	if o := v.state.orders[exclude]; o != nil {
		excludeSeq = o.AcceptSeq
	}
	for _, id := range v.state.accepted {
		o := v.state.orders[id]
		if o == nil || o.Match == nil || id == exclude {
			continue
		}
		ev := o.Match.Book.EventMs
		if ev > t || (ev == t && o.AcceptSeq > excludeSeq) {
			continue
		}
		q, err := o.Match.CumBaseQty.rat()
		if err != nil {
			return nil, nil, false
		}
		if o.Submit.OrderSide == "sell" {
			q.Neg(q)
		}
		base.Add(base, q)
	}
	lo, hi := new(big.Rat).Set(base), new(big.Rat).Set(base)
	for _, r := range v.native {
		d, err := r.Delta.rat()
		if err != nil {
			return nil, nil, false
		}
		switch pairRowStateAt(r, t) {
		case pairInventoryIncluded:
			lo.Add(lo, d)
			hi.Add(hi, d)
		case pairInventoryAmbiguous:
			hi.Add(hi, d)
		}
	}
	return lo, hi, true
}

func (v *pairVirtualVenue) reduceCap(o *pairVirtualOrder, t int64) (*big.Rat, *big.Rat, string) {
	lo, hi, ok := v.inventoryLocked(t, o.ClientOrderID)
	if !ok {
		return nil, nil, "the member inventory is not known to the venue, so reduce-only containment cannot be checked"
	}
	allowed := func(inv *big.Rat) *big.Rat {
		x := new(big.Rat).Set(inv)
		if o.Submit.OrderSide == "buy" {
			x.Neg(x)
		}
		if x.Sign() < 0 {
			return new(big.Rat)
		}
		return x
	}
	a, b := allowed(lo), allowed(hi)
	if b.Cmp(a) < 0 {
		a = b
	}
	return new(big.Rat).Quo(a, v.cs), lo, ""
}

func (v *pairVirtualVenue) adverse(px *big.Rat, side string) *big.Rat {
	f := new(big.Rat).Add(big.NewRat(1, 1), v.rate)
	if side == "sell" {
		f = new(big.Rat).Sub(big.NewRat(1, 1), v.rate)
	}
	return new(big.Rat).Mul(px, f)
}

func pairVirtualDigest(books []pairVirtualBook, throughMs int64) (string, int) {
	var b strings.Builder
	n := 0
	for _, k := range books {
		if k.AvailableMs <= throughMs {
			fmt.Fprintf(&b, "%s|%s|%d|%d\n", k.SourceContract, k.SourceID, k.EventMs, k.AvailableMs)
			n++
		}
	}
	fmt.Fprintf(&b, "n=%d", n)
	return pairSHA256([]byte(b.String())), n
}

func (v *pairVirtualVenue) decideLocked(o *pairVirtualOrder, now int64) string {
	s := o.Submit.AcceptedAtMs
	deadline := o.Submit.AcceptDeadlineMs
	earliest, ok := pairAddMs(s, v.cfg.SubmitLatencyMs)
	if !ok {
		return "the submission latency boundary overflows"
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairVirtualMarketTimeout)
	defer cancel()
	key := pairMemberKey{Partition: v.cfg.Partition, Provider: v.member.Provider, AccountKey: v.member.AccountKey, Instrument: v.member.Instrument, MemberID: v.member.MemberID}
	w, err := v.cfg.Market.Events(ctx, key, s, now)
	if err != nil {
		return "the recorded market read failed and supplies no fill or absence proof: " + err.Error()
	}
	books, cont, err := pairVirtualBooks(v.member, key.evidenceKey(), v.need, o.Submit.OrderSide, w, s, now)
	if err != nil {
		return "the recorded market window is refused and supplies no fill or absence proof: " + err.Error()
	}
	if q := o.LastQuery; q != nil {
		if q.ThroughMs > now {
			return fmt.Sprintf("the saved market prefix through %d is after the clock %d", q.ThroughMs, now)
		}
		if got, _ := pairVirtualDigest(books, q.ThroughMs); got != q.Digest {
			return fmt.Sprintf("the recorded market changed its committed prefix through %d; no new fill or absence proof is taken from it", q.ThroughMs)
		}
	}
	var cand []pairVirtualBook
	for _, b := range books {
		if b.EventMs > s && b.EventMs >= earliest && b.EventMs < deadline && b.AvailableMs <= now && b.complete(o.Submit.OrderSide, v.need) {
			cand = append(cand, b)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].AvailableMs != cand[j].AvailableMs {
			return cand[i].AvailableMs < cand[j].AvailableMs
		}
		if cand[i].EventMs != cand[j].EventMs {
			return cand[i].EventMs < cand[j].EventMs
		}
		return cand[i].order < cand[j].order
	})
	digest, n := pairVirtualDigest(books, now)
	if len(cand) == 0 {
		return v.noDecisionLocked(o, now, deadline, cont, digest, n)
	}
	return v.matchLocked(o, cand[0], now, digest)
}

func (v *pairVirtualVenue) noDecisionLocked(o *pairVirtualOrder, now, deadline int64, cont pairVirtualContinuity, digest string, n int) string {
	limit, ok := pairAddMs(deadline, entryRecoveryGrace.Milliseconds())
	if ok && cont.Continuous && cont.FromMs <= o.Submit.AcceptedAtMs && cont.ThroughMs >= limit && pairAbsenceReleasable(now, deadline, cont.ThroughMs) {
		raw, err := pairVirtualEncodeBody(pairVirtualAbsenceBody{V: pairVirtualRecordVersion, ClientOrderID: o.ClientOrderID, FromMs: cont.FromMs, ThroughMs: cont.ThroughMs, DecidedAtMs: now, Digest: digest})
		if err != nil {
			return err.Error()
		}
		e := pairVirtualEvent{Kind: pairVirtualKindAbsence, SemanticKey: "decision|" + o.ClientOrderID, ClientOrderID: o.ClientOrderID, Body: raw, CreatedAtMs: now}
		if err := v.commit([]pairVirtualEvent{e}, nil, now); err != nil {
			if cur := v.state.orders[o.ClientOrderID]; cur == nil || (cur.Absence == nil && cur.Match == nil) {
				return "the absence proof could not be saved: " + err.Error()
			}
		}
		return ""
	}
	if (o.LastQuery != nil && o.LastQuery.Digest == digest) || (n == 0 && o.LastQuery == nil) {
		return ""
	}
	raw, err := pairVirtualEncodeBody(pairVirtualQueryBody{V: pairVirtualRecordVersion, ClientOrderID: o.ClientOrderID, ThroughMs: now, Digest: digest, Books: n})
	if err != nil {
		return err.Error()
	}
	e := pairVirtualEvent{Kind: pairVirtualKindQuery, SemanticKey: fmt.Sprintf("query|%s|%d", o.ClientOrderID, now), ClientOrderID: o.ClientOrderID, Body: raw, CreatedAtMs: now}
	_ = v.commit([]pairVirtualEvent{e}, nil, now)
	return ""
}

type pairVirtualTake struct {
	idx      int
	adv      *big.Rat
	qty      *big.Rat
	levelQty *big.Rat
	level    pairNativeLevel
}

func (v *pairVirtualVenue) matchLocked(o *pairVirtualOrder, b pairVirtualBook, now int64, digest string) string {
	side := o.Submit.OrderSide
	levels, _ := b.levels(side)
	bound, err := o.Submit.PriceBound.rat()
	if err != nil {
		return err.Error()
	}
	requested, err := o.Submit.NativeQty.rat()
	if err != nil {
		return err.Error()
	}
	target := new(big.Rat).Set(requested)
	contained := new(big.Rat).Set(requested)
	inventory := pairDecimal("0")
	if o.Submit.ReduceOnly {
		capNative, inv, why := v.reduceCap(o, b.EventMs)
		if why != "" {
			return why
		}
		inventory, contained = pairDecimalOfRat(inv), capNative
		if capNative.Cmp(target) < 0 {
			target = capNative
		}
	}
	var avail []pairVirtualTake
	total := new(big.Rat)
	for i, l := range levels {
		adv := v.adverse(l.px, side)
		if (side == "buy" && adv.Cmp(bound) > 0) || (side == "sell" && adv.Cmp(bound) < 0) {
			break
		}
		rem := new(big.Rat).Set(l.qty)
		if used := v.state.consumed[pairVirtualCapKey(b.SourceContract, b.SourceID, side, i)]; used != nil {
			rem.Sub(rem, used)
		}
		if rem.Sign() <= 0 {
			continue
		}
		avail = append(avail, pairVirtualTake{idx: i, adv: adv, qty: rem, levelQty: l.qty, level: l.level})
		total.Add(total, rem)
	}
	fill := new(big.Rat).Set(target)
	if total.Cmp(fill) < 0 {
		fill.Set(total)
	}
	if fill.Cmp(requested) != 0 {
		n := new(big.Rat).Quo(fill, v.lot)
		whole := new(big.Int).Quo(n.Num(), n.Denom())
		fill = new(big.Rat).Mul(new(big.Rat).SetInt(whole), v.lot)
	}
	var fills []pairVirtualFillRecord
	var events []pairVirtualEvent
	var caps []pairVirtualCapacity
	left := new(big.Rat).Set(fill)
	cumBase, cumNotional := new(big.Rat), new(big.Rat)
	for _, t := range avail {
		if left.Sign() <= 0 {
			break
		}
		q := new(big.Rat).Set(t.qty)
		if q.Cmp(left) > 0 {
			q.Set(left)
		}
		left.Sub(left, q)
		baseQ := new(big.Rat).Mul(q, v.cs)
		notional := new(big.Rat).Mul(baseQ, t.adv)
		cumBase.Add(cumBase, baseQ)
		cumNotional.Add(cumNotional, notional)
		advDec, err1 := pairRatDecimal(t.adv)
		qDec, err2 := pairRatDecimal(q)
		bDec, err3 := pairRatDecimal(baseQ)
		nDec, err4 := pairRatDecimal(notional)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return "the execution amounts have no finite decimal form: " + err.Error()
		}
		fid := fmt.Sprintf("%s-f%d", o.ClientOrderID, len(fills)+1)
		fills = append(fills, pairVirtualFillRecord{FillID: fid, LevelIndex: t.idx, NativePx: t.level.Px, AdversePx: pairDecimal(advDec), NativeQty: pairDecimal(qDec), BaseQty: pairDecimal(bDec), NativeNotional: pairDecimal(nDec)})
		cb, err := pairVirtualEncodeBody(pairVirtualConsumeBody{V: pairVirtualRecordVersion, ClientOrderID: o.ClientOrderID, LevelPx: t.level.Px, LevelQty: t.level.Qty, AdversePx: pairDecimal(advDec)})
		if err != nil {
			return err.Error()
		}
		delta := pairDecimal(bDec)
		if side == "sell" {
			delta = pairDecimal("-" + bDec)
		}
		events = append(events, pairVirtualEvent{Kind: pairVirtualKindConsume, SemanticKey: fmt.Sprintf("consume|%s|%s|%s|%d", o.ClientOrderID, b.SourceID, side, t.idx), ClientOrderID: o.ClientOrderID,
			ObservationID: b.SourceID, SourceContract: b.SourceContract, BookSide: side, LevelIndex: t.idx, ConsumedNative: pairDecimal(qDec), ConsumedBase: pairDecimal(bDec), NativeDelta: delta,
			VenueTimeMs: b.EventMs, ReceiptMs: b.ReceiptMs, Body: cb, CreatedAtMs: now})
		caps = append(caps, pairVirtualCapacity{SourceContract: b.SourceContract, ObservationID: b.SourceID, BookSide: side, LevelIndex: t.idx, LevelPx: t.level.Px, LevelQty: t.levelQty, Consume: q})
	}
	status, detail := basketVenueFilled, "the first eligible recorded book filled the full request"
	switch {
	case fill.Sign() == 0:
		status, detail = basketVenueRejected, "the first eligible recorded book held no admissible depth within the saved bound after the adverse rate; the IOC ended with no fill"
	case fill.Cmp(requested) < 0:
		status, detail = basketVenuePartial, "the first eligible recorded book filled part of the request; the IOC remainder is cancelled"
	}
	visible, ok := pairAddMs(b.AvailableMs, v.cfg.ResponseLatencyMs)
	if !ok {
		return "the response visibility time overflows"
	}
	rateDec, err := pairRatDecimal(v.rate)
	if err != nil {
		return err.Error()
	}
	bookRec := b
	bookRec.Bids, bookRec.Asks = append([]pairNativeLevel{}, b.Bids...), append([]pairNativeLevel{}, b.Asks...)
	if fills == nil {
		fills = []pairVirtualFillRecord{}
	}
	mb := pairVirtualMatchBody{V: pairVirtualRecordVersion, ClientOrderID: o.ClientOrderID, DecidedAtMs: now, Status: status, Side: side, AcceptedAtMs: o.Submit.AcceptedAtMs, VisibleMs: visible,
		AdverseRate: pairDecimal(rateDec), RequestedQty: o.Submit.NativeQty, ContainedQty: pairDecimalOfRat(contained), InventoryBase: inventory,
		CumNativeQty: pairDecimalOfRat(fill), CumBaseQty: pairDecimalOfRat(cumBase), CumNotional: pairDecimalOfRat(cumNotional), Book: bookRec, Fills: fills, Digest: digest, Detail: detail}
	raw, err := pairVirtualEncodeBody(mb)
	if err != nil {
		return err.Error()
	}
	match := pairVirtualEvent{Kind: pairVirtualKindMatch, SemanticKey: "decision|" + o.ClientOrderID, ClientOrderID: o.ClientOrderID, ObservationID: b.SourceID, SourceContract: b.SourceContract,
		BookSide: side, VenueTimeMs: b.EventMs, ReceiptMs: b.ReceiptMs, VisibleMs: visible, RequestHash: o.Submit.RequestHash, Body: raw, CreatedAtMs: now}
	if err := v.commit(append([]pairVirtualEvent{match}, events...), caps, now); err != nil {
		if cur := v.state.orders[o.ClientOrderID]; cur != nil && cur.Match != nil {
			return ""
		}
		return "the execution could not be saved, so nothing is exposed: " + err.Error()
	}
	return ""
}

func (v *pairVirtualVenue) executedBase() (map[string]*big.Rat, *big.Rat, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.attached {
		return nil, nil, false
	}
	per := map[string]*big.Rat{}
	total := new(big.Rat)
	for id, o := range v.state.orders {
		q := new(big.Rat)
		if o.Match != nil {
			r, err := o.Match.CumBaseQty.rat()
			if err != nil {
				return nil, nil, false
			}
			q = r
		}
		if o.Submit.OrderSide == "sell" {
			q.Neg(q)
		}
		per[id] = q
		total.Add(total, q)
	}
	return per, total, true
}

var _ pairVenue = (*pairVirtualVenue)(nil)
