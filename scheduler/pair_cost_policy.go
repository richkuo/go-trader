package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	pairCostBindingVersion  = 2
	pairCostEvidenceVersion = 1
	pairCostPolicyIDPrefix  = "pair-cost-"

	pairExecutionVenueConfirmed       = "venue_confirmed_v1"
	pairExecutionVirtualIOCAdverseMax = "virtual_ioc_adverse_max_v1"

	pairObligationCostProvenance = "cost_provenance"
)

var (
	errPairCostOwnership     = errors.New("pair member cost ownership is refused")
	errPairFeeFixProvenance  = errors.New("pair fee correction target row provenance is refused")
	errPairCostEvidenceGuard = errors.New("pair member cost evidence is frozen")
)

type pairCostPolicyRates struct {
	TakerFeePct pairDecimal `json:"taker_fee_pct"`
	SlippagePct pairDecimal `json:"slippage_pct"`
	FillRole    string      `json:"fill_role"`
	Spread      string      `json:"spread"`
}

type pairCostBinding struct {
	V                   int                 `json:"v"`
	ContractFingerprint string              `json:"contract_fingerprint"`
	MemberKey           pairEvidenceKey     `json:"member_key"`
	CostPolicyRates     pairCostPolicyRates `json:"cost_policy_rates"`
	ExecutionModel      string              `json:"execution_model"`
}

type pairCostEvidenceMember struct {
	MemberID     string          `json:"member_id"`
	Binding      pairCostBinding `json:"binding"`
	ModelVersion int             `json:"model_version"`
	PolicyID     string          `json:"policy_id"`
}

type pairCostEvidence struct {
	V                   int                      `json:"v"`
	ContractFingerprint string                   `json:"contract_fingerprint"`
	Members             []pairCostEvidenceMember `json:"members"`
}

type pairMemberCost struct {
	key       pairEvidenceKey
	model     string
	binding   pairCostBinding
	canonical []byte
	policy    fillCostPolicy
}

type pairAdmissionCosts struct {
	contract string
	members  map[string]pairMemberCost
	evidence []byte
	durable  bool
}

func pairExecutionModelKnown(model string) bool {
	return model == pairExecutionVenueConfirmed || model == pairExecutionVirtualIOCAdverseMax
}

func pairCostRatesOf(p fillCostPolicy) (pairCostPolicyRates, error) {
	if err := validateFillCostRates(p); err != nil {
		return pairCostPolicyRates{}, err
	}
	fee, err := pairDecimalFromFloat(p.TakerFeePct)
	if err != nil {
		return pairCostPolicyRates{}, fmt.Errorf("cost policy taker_fee_pct: %w", err)
	}
	slip, err := pairDecimalFromFloat(p.SlippagePct)
	if err != nil {
		return pairCostPolicyRates{}, fmt.Errorf("cost policy slippage_pct: %w", err)
	}
	return pairCostPolicyRates{TakerFeePct: fee, SlippagePct: slip, FillRole: p.FillRole, Spread: p.Spread}, nil
}

func resolvePairMemberCost(contractFP string, key pairEvidenceKey, rates fillCostPolicy, model string) (pairMemberCost, error) {
	if !pairHexDigestPattern.MatchString(contractFP) {
		return pairMemberCost{}, fmt.Errorf("pair cost binding needs the full contract fingerprint, got %q", contractFP)
	}
	if !pairExecutionModelKnown(model) {
		return pairMemberCost{}, fmt.Errorf("execution model %q is not %s or %s", model, pairExecutionVenueConfirmed, pairExecutionVirtualIOCAdverseMax)
	}
	if _, err := key.memberKey(); err != nil {
		return pairMemberCost{}, err
	}
	r, err := pairCostRatesOf(rates)
	if err != nil {
		return pairMemberCost{}, err
	}
	b := pairCostBinding{V: pairCostBindingVersion, ContractFingerprint: contractFP, MemberKey: key, CostPolicyRates: r, ExecutionModel: model}
	canonical, err := pairCanonicalJSON(b)
	if err != nil {
		return pairMemberCost{}, err
	}
	policy := rates.rates()
	policy.ModelVersion = FillCostModelReplayCustomVersion
	policy.PolicyID = pairCostPolicyIDPrefix + pairSHA256(canonical)
	return pairMemberCost{key: key, model: model, binding: b, canonical: canonical, policy: policy}, nil
}

func pairTransportModel(kind string, v basketVenue) (string, error) {
	pv, ok := v.(pairVenue)
	if !ok || pv == nil {
		return "", fmt.Errorf("the transport has no pair venue seam")
	}
	model := pv.ExecutionModel()
	switch model {
	case pairExecutionVirtualIOCAdverseMax:
		if kind != pairAdmissionResearch || !pv.Virtual() || pv.LiveShaped() {
			return "", fmt.Errorf("the transport declares %s, which only the virtual research transport of a research admission may declare", model)
		}
	case pairExecutionVenueConfirmed:
		if kind != pairAdmissionLiveShapedFixture || !pv.LiveShaped() || pv.Virtual() {
			return "", fmt.Errorf("the transport declares %s, which only the controlled live-shaped transport may declare", model)
		}
	default:
		return "", fmt.Errorf("the transport declares the unknown execution model %q", model)
	}
	return model, nil
}

func (a *pairResearchAdmission) bindCosts(cfg *Config, c basketContract) error {
	if a.deps.VenueFor == nil {
		return fmt.Errorf("pair cost binding needs the admitted transport")
	}
	part, err := parseRiskPartition(c.Partition)
	if err != nil {
		return err
	}
	byID := strategyConfigByID(cfg.Strategies)
	fp := c.Fingerprint()
	costs := &pairAdmissionCosts{contract: fp, members: map[string]pairMemberCost{}}
	ev := pairCostEvidence{V: pairCostEvidenceVersion, ContractFingerprint: fp}
	for _, m := range c.pairMembersSorted() {
		sc, ok := byID[m.MemberID]
		if !ok || sc.ID != m.MemberID {
			return fmt.Errorf("pair member %s has no configured strategy", m.MemberID)
		}
		if got := partitionFor(sc); got != part {
			return fmt.Errorf("pair member %s is in the %s partition, the contract in %s", m.MemberID, partitionLabel(got), partitionLabel(part))
		}
		v, verr := a.deps.VenueFor(sc)
		if verr != nil {
			return fmt.Errorf("pair member %s has no admitted transport: %w", m.MemberID, verr)
		}
		model, merr := pairTransportModel(a.kind, v)
		if merr != nil {
			return fmt.Errorf("pair member %s: %w", m.MemberID, merr)
		}
		if v.Provider() != m.Provider {
			return fmt.Errorf("pair member %s transport provider %q is not the contract provider %q", m.MemberID, v.Provider(), m.Provider)
		}
		if got := v.AccountKey(part); got != m.AccountKey {
			return fmt.Errorf("pair member %s transport account %q is not the contract account %q", m.MemberID, got, m.AccountKey)
		}
		mc, rerr := resolvePairMemberCost(fp, c.pairMemberKey(m).evidenceKey(), m.CostPolicy, model)
		if rerr != nil {
			return fmt.Errorf("pair member %s cost binding: %w", m.MemberID, rerr)
		}
		costs.members[m.MemberID] = mc
		ev.Members = append(ev.Members, pairCostEvidenceMember{MemberID: m.MemberID, Binding: mc.binding, ModelVersion: mc.policy.ModelVersion, PolicyID: mc.policy.PolicyID})
	}
	if c.Pair == nil || len(costs.members) == 0 || len(costs.members) != len(c.Pair.Members) {
		return fmt.Errorf("pair cost binding needs exactly one binding per contract member")
	}
	if costs.evidence, err = pairCanonicalJSON(ev); err != nil {
		return err
	}
	a.costMu.Lock()
	defer a.costMu.Unlock()
	if a.costs != nil {
		if !bytes.Equal(a.costs.evidence, costs.evidence) {
			return fmt.Errorf("%w: the admission already holds member cost bindings for contract %s; a changed pair identity, rate or execution model needs a new admission", errPairCostEvidenceGuard, shortFingerprint(a.costs.contract))
		}
		return nil
	}
	a.costs = costs
	return nil
}

func (a *pairResearchAdmission) memberCost(memberID string) (pairMemberCost, error) {
	if a == nil || !a.valid() {
		return pairMemberCost{}, fmt.Errorf("%w: the pair admission is released or was never registered", errPairCostOwnership)
	}
	a.costMu.RLock()
	defer a.costMu.RUnlock()
	c := a.costs
	if c == nil || c.contract == "" || c.contract != a.contract {
		return pairMemberCost{}, fmt.Errorf("%w: the pair admission has no member cost bindings for its frozen contract", errPairCostOwnership)
	}
	mc, ok := c.members[memberID]
	if !ok {
		return pairMemberCost{}, fmt.Errorf("%w: the pair admission has no member %s", errPairCostOwnership, memberID)
	}
	return mc, nil
}

func (a *pairResearchAdmission) ExecutionModel(memberID string) (string, bool) {
	mc, err := a.memberCost(memberID)
	if err != nil {
		return "", false
	}
	return mc.model, true
}

func (a *pairResearchAdmission) checkTransport(sc StrategyConfig, v basketVenue) error {
	mc, err := a.memberCost(sc.ID)
	if err != nil {
		return err
	}
	model, err := pairTransportModel(a.kind, v)
	if err != nil {
		return fmt.Errorf("pair member %s transport identity changed: %w", sc.ID, err)
	}
	part := partitionFor(sc)
	switch {
	case model != mc.model:
		return fmt.Errorf("pair member %s transport identity changed: execution model %s, frozen %s", sc.ID, model, mc.model)
	case v.Provider() != mc.key.Provider:
		return fmt.Errorf("pair member %s transport identity changed: provider %s, frozen %s", sc.ID, v.Provider(), mc.key.Provider)
	case part.String() != mc.key.Partition:
		return fmt.Errorf("pair member %s transport identity changed: partition %s, frozen %s", sc.ID, part.String(), mc.key.Partition)
	case v.AccountKey(part) != mc.key.AccountKey:
		return fmt.Errorf("pair member %s transport identity changed: account %s, frozen %s", sc.ID, v.AccountKey(part), mc.key.AccountKey)
	}
	return nil
}

func (a *pairResearchAdmission) costEvidence() ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("pair cost evidence needs an admission")
	}
	a.costMu.RLock()
	defer a.costMu.RUnlock()
	if a.costs == nil || len(a.costs.evidence) == 0 {
		return nil, fmt.Errorf("the pair admission has no member cost bindings")
	}
	return append([]byte(nil), a.costs.evidence...), nil
}

func pairCostEvidenceCanonical(raw []byte) ([]byte, pairCostEvidence, error) {
	var ev pairCostEvidence
	tree, err := pairStrictParse(raw)
	if err != nil {
		return nil, ev, err
	}
	if err := pairStrictDecodeTree(tree, raw, &ev, "cost_evidence"); err != nil {
		return nil, ev, err
	}
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, tree); err != nil {
		return nil, ev, err
	}
	return buf.Bytes(), ev, nil
}

func pairCostEvidenceDiff(want, got pairCostEvidence) string {
	if got.V != want.V {
		return fmt.Sprintf("evidence version %d, want %d", got.V, want.V)
	}
	if got.ContractFingerprint != want.ContractFingerprint {
		return fmt.Sprintf("contract %s, the run has %s", shortFingerprint(got.ContractFingerprint), shortFingerprint(want.ContractFingerprint))
	}
	if len(got.Members) != len(want.Members) {
		return fmt.Sprintf("%d member bindings, the run has %d", len(got.Members), len(want.Members))
	}
	for i := range want.Members {
		w, g := want.Members[i], got.Members[i]
		switch {
		case g.MemberID != w.MemberID:
			return fmt.Sprintf("member %d is %s, the run has %s", i, g.MemberID, w.MemberID)
		case g.ModelVersion != w.ModelVersion:
			return fmt.Sprintf("member %s model version %d, the run has %d", w.MemberID, g.ModelVersion, w.ModelVersion)
		case g.Binding.ExecutionModel != w.Binding.ExecutionModel:
			return fmt.Sprintf("member %s execution model %s, the run has %s", w.MemberID, g.Binding.ExecutionModel, w.Binding.ExecutionModel)
		case g.Binding.MemberKey != w.Binding.MemberKey:
			return fmt.Sprintf("member %s key %s, the run has %s", w.MemberID, g.Binding.MemberKey, w.Binding.MemberKey)
		case g.Binding.CostPolicyRates != w.Binding.CostPolicyRates:
			return fmt.Sprintf("member %s rates %+v, the run has %+v", w.MemberID, g.Binding.CostPolicyRates, w.Binding.CostPolicyRates)
		case g.PolicyID != w.PolicyID:
			return fmt.Sprintf("member %s policy id %s, the run has %s", w.MemberID, g.PolicyID, w.PolicyID)
		}
	}
	return "the canonical bytes differ"
}

func (a *pairResearchAdmission) verifyCostEvidence(saved []byte) error {
	want, err := a.costEvidence()
	if err != nil {
		return err
	}
	got, gotEv, err := pairCostEvidenceCanonical(saved)
	if err != nil {
		return fmt.Errorf("the saved member cost evidence is malformed: %v", err)
	}
	if bytes.Equal(got, want) {
		return nil
	}
	_, wantEv, err := pairCostEvidenceCanonical(want)
	if err != nil {
		return err
	}
	return fmt.Errorf("the saved member cost evidence does not match the rebuilt bindings: %s", pairCostEvidenceDiff(wantEv, gotEv))
}

func (a *pairResearchAdmission) markCostDurable(committed []byte) {
	if a == nil || len(committed) == 0 || a.verifyCostEvidence(committed) != nil {
		return
	}
	a.costMu.Lock()
	defer a.costMu.Unlock()
	if a.costs != nil {
		a.costs.durable = true
	}
}

func (a *pairResearchAdmission) costDurable() bool {
	if a == nil {
		return false
	}
	a.costMu.RLock()
	defer a.costMu.RUnlock()
	return a.costs != nil && a.costs.durable
}

func fillCostPolicyForStrategy(owner *pairResearchAdmission, strategyID string) (fillCostPolicy, error) {
	if owner == nil {
		return activeFillCostPolicy(), nil
	}
	mc, err := owner.memberCost(strategyID)
	if err != nil {
		return fillCostPolicy{}, err
	}
	if !owner.costDurable() {
		return fillCostPolicy{}, fmt.Errorf("%w: the member cost bindings are not saved yet, so booking holds until the run commits them", errPairCostOwnership)
	}
	return mc.policy, nil
}

func pairCostBindings(owner *pairResearchAdmission) map[string]fillCostPolicy {
	if owner == nil || !owner.valid() {
		return nil
	}
	owner.costMu.RLock()
	defer owner.costMu.RUnlock()
	c := owner.costs
	if c == nil || !c.durable || c.contract == "" || c.contract != owner.contract || len(c.members) == 0 {
		return nil
	}
	out := make(map[string]fillCostPolicy, len(c.members))
	for id, mc := range c.members {
		out[id] = mc.policy
	}
	return out
}

func checkSavedPairCostEvidenceTx(tx *sql.Tx, rt *basketRuntime) error {
	var raw string
	err := tx.QueryRow(`SELECT progress_json FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if len(rt.Pair.Progress.CostEvidence) == 0 {
			return fmt.Errorf("%w: pair %s has no member cost evidence to save", errPairCostEvidenceGuard, rt.Contract.BasketID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pair %s saved progress: %w", rt.Contract.BasketID, err)
	}
	saved, err := parsePairProgress(raw)
	if err != nil {
		return fmt.Errorf("%w: pair %s: %v", errPairCostEvidenceGuard, rt.Contract.BasketID, err)
	}
	if len(saved.CostEvidence) == 0 {
		return fmt.Errorf("%w: pair %s was saved without member cost evidence; a write never adds bindings to a pre-binding run", errPairCostEvidenceGuard, rt.Contract.BasketID)
	}
	if len(rt.Pair.Progress.CostEvidence) == 0 {
		return fmt.Errorf("%w: pair %s progress would remove its saved member cost evidence", errPairCostEvidenceGuard, rt.Contract.BasketID)
	}
	savedBytes, _, serr := pairCostEvidenceCanonical(saved.CostEvidence)
	nextBytes, _, nerr := pairCostEvidenceCanonical(rt.Pair.Progress.CostEvidence)
	if serr != nil || nerr != nil || !bytes.Equal(savedBytes, nextBytes) {
		return fmt.Errorf("%w: pair %s progress would replace its saved member cost evidence", errPairCostEvidenceGuard, rt.Contract.BasketID)
	}
	return nil
}

func strategyPairCostRequired(s *StrategyState) bool {
	return s != nil && (s.pairCostPair != "" || (s.Basket != nil && s.Basket.Pair != nil))
}

func strategyPairCostOwner(s *StrategyState) (*pairResearchAdmission, error) {
	rt := s.Basket
	if rt == nil || rt.Pair == nil || !rt.isPair() {
		return nil, fmt.Errorf("%w: pair member %s has no pair runtime link, and a pair member never uses the legacy cost policy", errPairCostOwnership, s.ID)
	}
	if s.pairCostPair != "" && rt.Contract.BasketID != s.pairCostPair {
		return nil, fmt.Errorf("%w: pair member %s belongs to pair %s, but its runtime link names pair %s", errPairCostOwnership, s.ID, s.pairCostPair, rt.Contract.BasketID)
	}
	if _, ok := rt.Contract.pairMember(s.ID); !ok {
		return nil, fmt.Errorf("%w: pair %s does not list member %s", errPairCostOwnership, rt.Contract.BasketID, s.ID)
	}
	if rt.Contract.Partition != rt.Partition.String() {
		return nil, fmt.Errorf("%w: pair %s contract partition %s is not its runtime partition %s", errPairCostOwnership, rt.Contract.BasketID, rt.Contract.Partition, rt.Partition.String())
	}
	adm := rt.Pair.admission
	if adm == nil || adm.contract != rt.Contract.Fingerprint() {
		return nil, fmt.Errorf("%w: pair %s runtime contract is not the admitted contract", errPairCostOwnership, rt.Contract.BasketID)
	}
	mc, err := adm.memberCost(s.ID)
	if err != nil {
		return nil, err
	}
	if mc.key.Partition != rt.Partition.String() || mc.key.MemberID != s.ID {
		return nil, fmt.Errorf("%w: pair member %s binding key %s is outside the runtime partition %s", errPairCostOwnership, s.ID, mc.key, rt.Partition.String())
	}
	return adm, nil
}

func strategyFillCostPolicy(s *StrategyState) (fillCostPolicy, error) {
	if !strategyPairCostRequired(s) {
		return activeFillCostPolicy(), nil
	}
	if s.costScope != nil {
		return *s.costScope, nil
	}
	adm, err := strategyPairCostOwner(s)
	if err != nil {
		return fillCostPolicy{}, err
	}
	return fillCostPolicyForStrategy(adm, s.ID)
}

func pairDispatchCostPolicy(sc StrategyConfig, s *StrategyState) (fillCostPolicy, error) {
	if isPairMember(sc) {
		switch {
		case s == nil:
			return fillCostPolicy{}, fmt.Errorf("%w: pair member %s has no book", errPairCostOwnership, sc.ID)
		case s.ID != sc.ID:
			return fillCostPolicy{}, fmt.Errorf("%w: pair member %s dispatch reached the book of %s", errPairCostOwnership, sc.ID, s.ID)
		case !strategyPairCostRequired(s):
			return fillCostPolicy{}, fmt.Errorf("%w: strategy %s names pair %s, but its book has no pair cost owner", errPairCostOwnership, sc.ID, strings.TrimSpace(sc.PairID))
		case s.Basket != nil && s.Basket.Contract.BasketID != strings.TrimSpace(sc.PairID):
			return fillCostPolicy{}, fmt.Errorf("%w: strategy %s names pair %s, but its book is linked to %s", errPairCostOwnership, sc.ID, strings.TrimSpace(sc.PairID), s.Basket.Contract.BasketID)
		}
	}
	return strategyFillCostPolicy(s)
}

func withStrategyCostScope(s *StrategyState, fn func()) error {
	if !strategyPairCostRequired(s) || s.costScope != nil {
		fn()
		return nil
	}
	p, err := strategyFillCostPolicy(s)
	if err != nil {
		noteStrategyCostFault(s, err.Error())
		return err
	}
	s.costScope = &p
	defer func() { s.costScope = nil }()
	fn()
	return nil
}

func withPairDispatchCostScope(sc StrategyConfig, s *StrategyState, fn func()) error {
	if isPairMember(sc) || strategyPairCostRequired(s) {
		if _, err := pairDispatchCostPolicy(sc, s); err != nil {
			noteStrategyCostFault(s, err.Error())
			return err
		}
	}
	return withStrategyCostScope(s, fn)
}

func strategyPlatformFee(s *StrategyState, platform string, value float64) float64 {
	if s != nil && s.costScope != nil {
		return value * s.costScope.TakerFeePct
	}
	return CalculatePlatformSpotFee(platform, value)
}

func strategyAdverseSlippage(s *StrategyState, price float64, isBuy bool) float64 {
	if s != nil && s.costScope != nil {
		return applySlippageRate(price, isBuy, s.costScope.SlippagePct)
	}
	return ApplyAdverseSlippage(price, isBuy)
}

func noteStrategyCostFault(s *StrategyState, msg string) {
	id := ""
	if s != nil {
		id = s.ID
	}
	fmt.Fprintf(os.Stderr, "[CRITICAL] [pair-cost] %s: %s\n", id, msg)
	if s == nil {
		return
	}
	for _, m := range s.pairCostFaults {
		if m == msg {
			return
		}
	}
	s.pairCostFaults = append(s.pairCostFaults, msg)
}

func stampTradeCostProvenance(s *StrategyState, trade *Trade) {
	pair := strategyPairCostRequired(s) && trade.TradeType != TradeTypeFunding
	if trade.CostModelVersion == 0 {
		switch {
		case pair && s.costScope != nil:
			trade.CostModelVersion, trade.CostPolicyID = s.costScope.ModelVersion, s.costScope.PolicyID
		case !pair:
			policy := activeFillCostPolicy()
			trade.CostModelVersion, trade.CostPolicyID = policy.ModelVersion, policy.PolicyID
		}
	}
	if !pair {
		return
	}
	want, err := strategyFillCostPolicy(s)
	switch {
	case err != nil:
		noteStrategyCostFault(s, fmt.Sprintf("a %s row for %s at %s was recorded unchanged without a checked member cost binding (%v)", trade.TradeType, trade.Symbol, formatTime(trade.Timestamp), err))
	case trade.CostModelVersion != want.ModelVersion || trade.CostPolicyID != want.PolicyID:
		noteStrategyCostFault(s, fmt.Sprintf("a %s row for %s at %s carries cost stamp %d/%q, but the member binding is %d/%q; the row was recorded unchanged", trade.TradeType, trade.Symbol, formatTime(trade.Timestamp), trade.CostModelVersion, trade.CostPolicyID, want.ModelVersion, want.PolicyID))
	}
}

func (bc *basketCoordinator) drainPairCostFaults(rt *basketRuntime, now time.Time) {
	changed := false
	for _, id := range rt.memberIDs() {
		s := bc.state.Strategies[id]
		if s == nil || len(s.pairCostFaults) == 0 {
			continue
		}
		faults := s.pairCostFaults
		s.pairCostFaults = nil
		for _, msg := range faults {
			rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationCostProvenance, MemberID: id, Detail: msg, AtMs: now.UnixMilli()})
			bc.pairOpenRecovery(rt, fmt.Sprintf("member %s cost provenance fault: %s", id, msg), now)
			bc.pairAlert(rt, "cost|"+id+"|"+msg, fmt.Sprintf("CRITICAL pair %s member %s cost provenance fault: %s; increases are frozen and the accounting holds for an operator", rt.Contract.BasketID, id, msg), true)
		}
		changed = true
	}
	if changed {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
}

func (bc *basketCoordinator) ensurePairCostEvidence(rt *basketRuntime) bool {
	adm := rt.Pair.admission
	if adm.costDurable() {
		return true
	}
	if len(rt.Pair.Progress.CostEvidence) == 0 {
		bc.pairAlert(rt, "cost-evidence|missing", fmt.Sprintf("pair %s has no member cost evidence; booking and dispatch hold", rt.Contract.BasketID), true)
		return false
	}
	rt.touchPairProgress()
	if err := bc.store.PersistBasketProgress(rt); err != nil {
		bc.pairLogf(rt, "the member cost bindings could not be saved (%v); every booking and dispatch holds until they are durable", err)
		bc.emit(rt, fmt.Sprintf("pair %s member cost bindings could not be saved: %v; booking and dispatch hold", rt.Contract.BasketID, err), true)
		return false
	}
	return adm.costDurable()
}

func logStrategyCostRefusal(logger *StrategyLogger, what, symbol string, err error) {
	if logger != nil {
		logger.Error("%s %s refused before any book change: %v", what, symbol, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[CRITICAL] [pair-cost] %s %s refused before any book change: %v\n", what, symbol, err)
}

type pairFeeFixUndo struct {
	fix                 pairFeeFix
	delta               float64
	dailyApplied        bool
	dailyDate           string
	tradeFound          bool
	prevFeeSource       string
	intentFound         bool
	prevIntentApplied   float64
	prevIntentConfirmed float64
	prevApplied         float64
	prevAppliedVersion  int64
	prevBooked          string
	prevStatus          string
	prevHold            string
	prevUpdatedAtMs     int64
}

func pairRetainedTrade(s *StrategyState, fix *pairFeeFix) *Trade {
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := &s.TradeHistory[i]
		sameTs := t.Timestamp.UTC().UnixMilli() == fix.TradeTsMs
		if fix.TradeTs != "" {
			sameTs = formatTime(t.Timestamp) == fix.TradeTs
		}
		if t.Symbol == fix.Symbol && sameTs && t.ExchangeOrderID == fix.OrderID {
			return t
		}
	}
	return nil
}

func (bc *basketCoordinator) pairFeeFixTarget(rt *basketRuntime, s *StrategyState, a *basketAction, fix *pairFeeFix, retained *Trade) (pairTradeStamp, error) {
	want, err := strategyFillCostPolicy(s)
	if err != nil {
		return pairTradeStamp{}, err
	}
	stamp, err := bc.store.PairTradeStamp(rt, a.StrategyID, fix.tradeTs(), fix.Symbol, fix.OrderID)
	if err != nil {
		return pairTradeStamp{}, err
	}
	if stamp.ModelVersion != want.ModelVersion || stamp.PolicyID != want.PolicyID {
		return pairTradeStamp{}, fmt.Errorf("%w: the saved row carries cost stamp %d/%q, but the member binding is %d/%q", errPairFeeFixProvenance, stamp.ModelVersion, stamp.PolicyID, want.ModelVersion, want.PolicyID)
	}
	if retained != nil && (retained.CostModelVersion != want.ModelVersion || retained.CostPolicyID != want.PolicyID) {
		return pairTradeStamp{}, fmt.Errorf("%w: the retained row carries cost stamp %d/%q, but the member binding is %d/%q", errPairFeeFixProvenance, retained.CostModelVersion, retained.CostPolicyID, want.ModelVersion, want.PolicyID)
	}
	return stamp, nil
}

func (bc *basketCoordinator) holdPairFeeFixProvenance(rt *basketRuntime, a *basketAction, detail string, now time.Time) {
	rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationCostProvenance, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: detail, AtMs: now.UnixMilli()})
	bc.holdPairAction(rt, a, detail, true, now)
}

func (bc *basketCoordinator) rollbackPairFeeFixes(rt *basketRuntime, err error, now time.Time) bool {
	var fe *pairFeeFixError
	if !errors.As(err, &fe) || !errors.Is(err, errPairFeeFixProvenance) {
		return false
	}
	var refused *basketAction
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if a.feeFixUndo == nil {
			a.pendingFee = nil
			continue
		}
		bc.undoPairFeeFix(a)
		if a.ActionID == fe.ActionID {
			refused = a
		}
	}
	if refused == nil {
		bc.pairLogf(rt, "a fee correction was refused in the save transaction (%v), but no staged correction names action %s", err, fe.ActionID)
		return true
	}
	bc.holdPairFeeFixProvenance(rt, refused, fmt.Sprintf("the fee correction was refused inside the save transaction: %v; the book, the saved row, the intent and the action are unchanged", fe.Err), now)
	return true
}

func (bc *basketCoordinator) undoPairFeeFix(a *basketAction) {
	u := a.feeFixUndo
	s := bc.state.Strategies[a.StrategyID]
	if s != nil {
		s.Cash += u.delta
		if u.dailyApplied && s.RiskState.DailyPnLDate == u.dailyDate {
			s.RiskState.DailyPnL += u.delta
		}
		if u.tradeFound {
			if t := pairRetainedTrade(s, &u.fix); t != nil {
				t.ExchangeFee -= u.delta
				t.FeeSource = u.prevFeeSource
			}
		}
		if in := bc.intentFor(s, a); in != nil && u.intentFound {
			in.AppliedFee, in.ConfirmedFee = u.prevIntentApplied, u.prevIntentConfirmed
		}
	}
	a.AppliedFee, a.AppliedFeeVersion, a.BookedJSON = u.prevApplied, u.prevAppliedVersion, u.prevBooked
	a.Status, a.HoldReason, a.UpdatedAtMs = u.prevStatus, u.prevHold, u.prevUpdatedAtMs
	a.pendingFee, a.feeFixUndo = nil, nil
}
