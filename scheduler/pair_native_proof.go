package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
)

const (
	pairNativeFundingProofVersion    = 1
	pairNativeFundingProofTimeSource = "host_local_clock"
)

var errPairNativeBound = errors.New("a saved native funding bound is immutable")

type pairNativeFundingProof struct {
	V              int64             `json:"v"`
	Partition      string            `json:"partition"`
	PairID         string            `json:"pair_id"`
	MemberID       string            `json:"member_id"`
	Kind           string            `json:"kind"`
	ObligationID   string            `json:"obligation_id"`
	ResolutionID   string            `json:"resolution_id"`
	HoldFromMs     int64             `json:"hold_from_ms"`
	HoldAtMs       int64             `json:"hold_at_ms"`
	ContractFP     string            `json:"contract_fingerprint"`
	ContractJSON   string            `json:"contract_json"`
	Position       pairVenuePosition `json:"position"`
	VenueBaseQty   string            `json:"venue_base_qty"`
	BookBaseQty    string            `json:"book_base_qty"`
	RequestStartMs int64             `json:"request_start_ms"`
	AcceptedAtMs   int64             `json:"accepted_at_ms"`
	TimeSource     string            `json:"time_source"`
	ProofHash      string            `json:"proof_hash"`
}

func (h pairNativeFundingHold) bounded() bool { return h.ThroughMs > 0 }

func (h pairNativeFundingHold) identity() string {
	return strings.Join([]string{h.MemberID, h.Kind, h.ObligationID, h.ResolutionID}, "|")
}

func pairBuildNativeFundingProof(proof pairNativeFundingProof) (pairNativeFundingProof, json.RawMessage, error) {
	proof.V = pairNativeFundingProofVersion
	proof.TimeSource = pairNativeFundingProofTimeSource
	proof.ProofHash = ""
	h, err := pairRecordHashOf(proof)
	if err != nil {
		return proof, nil, err
	}
	proof.ProofHash = h
	raw, err := pairCanonicalJSON(proof)
	if err != nil {
		return proof, nil, err
	}
	return proof, json.RawMessage(raw), nil
}

func pairParseNativeFundingProof(raw json.RawMessage) (pairNativeFundingProof, error) {
	var proof pairNativeFundingProof
	if len(bytes.TrimSpace(raw)) == 0 {
		return proof, fmt.Errorf("the native funding proof is missing")
	}
	tree, err := pairStrictParse(raw)
	if err != nil {
		return proof, fmt.Errorf("the native funding proof is unreadable: %w", err)
	}
	if err := pairStrictDecodeTree(tree, raw, &proof, "native funding proof"); err != nil {
		return proof, err
	}
	canon, err := pairCanonicalJSON(proof)
	if err != nil {
		return proof, err
	}
	if !bytes.Equal(canon, []byte(raw)) {
		return proof, fmt.Errorf("the native funding proof is not in canonical form")
	}
	return proof, nil
}

func pairCheckVenuePositionFor(m pairContractMember, p pairVenuePosition, nowMs, quoteAgeMs int64) error {
	if p.V != pairVenuePositionVersion {
		return fmt.Errorf("position record version %d, want %d", p.V, pairVenuePositionVersion)
	}
	wantCS, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		return err
	}
	if p.ContractSize != string(wantCS) {
		return fmt.Errorf("position contract size %s differs from the contract size %s", p.ContractSize, wantCS)
	}
	if p.PositionMode != m.Meta.PositionMode {
		return fmt.Errorf("position mode %q differs from the contract position mode %q", p.PositionMode, m.Meta.PositionMode)
	}
	zero := p.SignedNativeQty == "0"
	if !(zero && p.MarginMode == "") && p.MarginMode != m.Meta.MarginMode {
		return fmt.Errorf("position margin mode %q differs from the contract margin mode %q", p.MarginMode, m.Meta.MarginMode)
	}
	if p.ObservedMs <= 0 || p.ObservedMs > p.FirstReceiptMs || p.FirstReceiptMs > nowMs {
		return fmt.Errorf("position times observed %d, received %d and coordinator %d are not ordered", p.ObservedMs, p.FirstReceiptMs, nowMs)
	}
	if !pairAgeWithin(nowMs, p.ObservedMs, quoteAgeMs) || !pairAgeWithin(nowMs, p.FirstReceiptMs, quoteAgeMs) {
		return fmt.Errorf("position observed %d and received %d are not within quote_age_ms %d of %d", p.ObservedMs, p.FirstReceiptMs, quoteAgeMs, nowMs)
	}
	return nil
}

func pairCheckNativeFundingProof(hold pairNativeFundingHold, proof pairNativeFundingProof) error {
	if hold.ThroughMs <= 0 || len(hold.Proof) == 0 {
		return fmt.Errorf("native funding hold %s has a bound without a proof", hold.ResolutionID)
	}
	if proof.V != pairNativeFundingProofVersion {
		return fmt.Errorf("native funding proof version %d, want %d", proof.V, pairNativeFundingProofVersion)
	}
	if proof.TimeSource != pairNativeFundingProofTimeSource {
		return fmt.Errorf("native funding proof time source %q, want %q", proof.TimeSource, pairNativeFundingProofTimeSource)
	}
	digest := proof
	digest.ProofHash = ""
	h, err := pairRecordHashOf(digest)
	if err != nil {
		return err
	}
	if h != proof.ProofHash {
		return fmt.Errorf("native funding proof hash does not match its content")
	}
	if proof.ContractFP == "" || pairSHA256([]byte(proof.ContractJSON)) != proof.ContractFP {
		return fmt.Errorf("native funding proof contract bytes do not match its fingerprint")
	}
	saved, err := parseBasketContract(proof.ContractJSON)
	if err != nil {
		return fmt.Errorf("native funding proof contract is unreadable: %w", err)
	}
	if !saved.isPair() {
		return fmt.Errorf("native funding proof contract is not a pair contract")
	}
	if saved.BasketID != proof.PairID || saved.Partition != proof.Partition {
		return fmt.Errorf("native funding proof names pair %s in %s, its contract names %s in %s", proof.PairID, proof.Partition, saved.BasketID, saved.Partition)
	}
	m, ok := saved.pairMember(proof.MemberID)
	if !ok {
		return fmt.Errorf("native funding proof member %s is not in its contract", proof.MemberID)
	}
	wantKey := saved.pairMemberKey(m).evidenceKey()
	if proof.Position.Key != wantKey {
		return fmt.Errorf("native funding proof position key %s does not name member %s", proof.Position.Key, wantKey)
	}
	ph, err := proof.Position.computeHash()
	if err != nil {
		return err
	}
	if ph != proof.Position.EvidenceHash {
		return fmt.Errorf("native funding proof position hash does not match its content")
	}
	if !pairPrintableASCII(proof.Position.EvidenceID, pairEvidenceMaxIDLen) {
		return fmt.Errorf("native funding proof position has no printable evidence id")
	}
	if !pairDecimalCanonical(proof.Position.SignedNativeQty) {
		return fmt.Errorf("native funding proof position quantity %q is not a canonical decimal", proof.Position.SignedNativeQty)
	}
	if err := pairCheckVenuePositionFor(m, proof.Position, proof.AcceptedAtMs, saved.Pair.Limits.QuoteAgeMs); err != nil {
		return err
	}
	base, err := pairPositionBase(proof.Position)
	if err != nil {
		return err
	}
	venueQty, err := pairRatDecimal(base)
	if err != nil {
		return err
	}
	if proof.VenueBaseQty != venueQty || proof.BookBaseQty != venueQty {
		return fmt.Errorf("native funding proof quantities venue %s book %s, want %s", proof.VenueBaseQty, proof.BookBaseQty, venueQty)
	}
	if hold.MemberID != proof.MemberID || hold.Kind != proof.Kind || hold.ObligationID != proof.ObligationID || hold.ResolutionID != proof.ResolutionID || hold.FromMs != proof.HoldFromMs || hold.AtMs != proof.HoldAtMs {
		return fmt.Errorf("native funding proof does not name hold %s", hold.identity())
	}
	if hold.ThroughMs != proof.Position.FirstReceiptMs {
		return fmt.Errorf("native funding hold bound %d differs from proof receipt %d", hold.ThroughMs, proof.Position.FirstReceiptMs)
	}
	if !(proof.HoldAtMs < proof.RequestStartMs && proof.RequestStartMs <= proof.Position.ObservedMs && proof.Position.ObservedMs <= proof.Position.FirstReceiptMs && proof.Position.FirstReceiptMs <= proof.AcceptedAtMs) {
		return fmt.Errorf("native funding proof times hold %d request %d observed %d receipt %d accepted %d are not ordered", proof.HoldAtMs, proof.RequestStartMs, proof.Position.ObservedMs, proof.Position.FirstReceiptMs, proof.AcceptedAtMs)
	}
	return nil
}

func pairExactBookRat(s *StrategyState, coin string) (*big.Rat, error) {
	if s == nil {
		return new(big.Rat), nil
	}
	pos := s.Positions[coin]
	if pos == nil || pos.Quantity == 0 {
		return new(big.Rat), nil
	}
	if math.IsNaN(pos.Quantity) || math.IsInf(pos.Quantity, 0) || pos.Quantity < 0 {
		return nil, fmt.Errorf("book quantity %v for %s is not a finite non-negative quantity", pos.Quantity, coin)
	}
	switch pos.Side {
	case "", "long":
		return quoteRatFromFloatDecimal(pos.Quantity)
	case "short":
		r, err := quoteRatFromFloatDecimal(pos.Quantity)
		if err != nil {
			return nil, err
		}
		return r.Neg(r), nil
	default:
		return nil, fmt.Errorf("book position for %s has unknown side %q", coin, pos.Side)
	}
}

var pairCoinEpochState = struct {
	sync.RWMutex
	m map[string]uint64
}{m: map[string]uint64{}}

func pairCoinEpochKey(coin string) string {
	return strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(coin)), "/USDT")
}

func pairCoinEpoch(coin string) uint64 {
	pairCoinEpochState.RLock()
	defer pairCoinEpochState.RUnlock()
	return pairCoinEpochState.m[pairCoinEpochKey(coin)]
}

func bumpPairCoinEpoch(coins ...string) {
	pairCoinEpochState.Lock()
	defer pairCoinEpochState.Unlock()
	for _, c := range coins {
		if k := pairCoinEpochKey(c); k != "" {
			pairCoinEpochState.m[k]++
		}
	}
}

func (rt *basketRuntime) bumpPairMutationEpoch() {
	if rt != nil && rt.Pair != nil {
		rt.Pair.mutationEpoch++
	}
}

func checkSavedPairNativeBoundsTx(tx *sql.Tx, rt *basketRuntime, next pairProgress) error {
	var raw string
	err := tx.QueryRow(`SELECT progress_json FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		for _, h := range next.NativeFundingHolds {
			if err := pairValidateBoundHold(h); err != nil {
				return fmt.Errorf("%w: %v", errPairNativeBound, err)
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pair %s saved progress: %w", rt.Contract.BasketID, err)
	}
	saved, err := parsePairProgress(raw)
	if err != nil {
		return fmt.Errorf("%w: pair %s saved progress is unreadable: %v", errPairNativeBound, rt.Contract.BasketID, err)
	}
	byID := map[string]pairNativeFundingHold{}
	for _, h := range next.NativeFundingHolds {
		byID[h.identity()] = h
		if err := pairValidateBoundHold(h); err != nil {
			return fmt.Errorf("%w: %v", errPairNativeBound, err)
		}
	}
	for _, sh := range saved.NativeFundingHolds {
		nh, ok := byID[sh.identity()]
		if !ok {
			return fmt.Errorf("%w: pair %s progress would remove saved native funding hold %s", errPairNativeBound, rt.Contract.BasketID, sh.identity())
		}
		if sh.FromMs != nh.FromMs || sh.AtMs != nh.AtMs {
			return fmt.Errorf("%w: pair %s progress would move saved native funding hold %s", errPairNativeBound, rt.Contract.BasketID, sh.identity())
		}
		if sh.bounded() {
			if nh.ThroughMs != sh.ThroughMs || !bytes.Equal(nh.Proof, sh.Proof) {
				return fmt.Errorf("%w: pair %s progress would remove or replace the saved bound of native funding hold %s", errPairNativeBound, rt.Contract.BasketID, sh.identity())
			}
		}
	}
	return nil
}

func pairValidateBoundHold(h pairNativeFundingHold) error {
	if h.ThroughMs == 0 && len(h.Proof) == 0 {
		return nil
	}
	if h.ThroughMs <= 0 || len(h.Proof) == 0 {
		return fmt.Errorf("native funding hold %s has a bound without its proof or a proof without its bound", h.ResolutionID)
	}
	proof, err := pairParseNativeFundingProof(h.Proof)
	if err != nil {
		return err
	}
	return pairCheckNativeFundingProof(h, proof)
}

func (st *StateStore) SavedPairProgress(rt *basketRuntime) (string, error) {
	db, err := st.basketFile(rt)
	if err != nil {
		return "", err
	}
	var raw string
	err = db.db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&raw)
	if err != nil {
		return "", err
	}
	return raw, nil
}

func pairNativeOpenHoldFromMs(rt *basketRuntime, memberID string) int64 {
	var from int64
	for _, h := range rt.Pair.Progress.NativeFundingHolds {
		if h.MemberID == memberID && !h.bounded() && h.FromMs > 0 && (from == 0 || h.FromMs < from) {
			from = h.FromMs
		}
	}
	return from
}

func pairNativeSortedHolds(rt *basketRuntime, memberID string) []pairNativeFundingHold {
	var out []pairNativeFundingHold
	if rt == nil || rt.Pair == nil {
		return out
	}
	for _, h := range rt.Pair.Progress.NativeFundingHolds {
		if h.MemberID == memberID {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromMs != out[j].FromMs {
			return out[i].FromMs < out[j].FromMs
		}
		return out[i].ResolutionID < out[j].ResolutionID
	})
	return out
}

func pairNativeFundingHoldsHit(rt *basketRuntime, memberID string, t, need int64) (pairNativeFundingHold, bool) {
	for _, h := range pairNativeSortedHolds(rt, memberID) {
		if h.FromMs <= 0 || need < h.FromMs {
			continue
		}
		if !h.bounded() {
			return h, true
		}
		if t <= h.ThroughMs {
			return h, true
		}
	}
	return pairNativeFundingHold{}, false
}

func pairNativeHoldGapIntervals(rt *basketRuntime, memberID string) []pairFundingInterval {
	var out []pairFundingInterval
	for _, h := range pairNativeSortedHolds(rt, memberID) {
		if h.FromMs <= 0 {
			continue
		}
		from := h.FromMs - 1
		if h.bounded() {
			out = append(out, pairFundingInterval{FromMs: from, ThroughMs: h.ThroughMs})
		} else {
			out = append(out, pairFundingInterval{FromMs: from, ThroughMs: math.MaxInt64})
		}
	}
	return out
}

func pairNativeProofHashPrefix(raw json.RawMessage) string {
	proof, err := pairParseNativeFundingProof(raw)
	if err != nil || proof.ProofHash == "" {
		return "none"
	}
	if len(proof.ProofHash) > 12 {
		return proof.ProofHash[:12]
	}
	return proof.ProofHash
}

func validatePairNativeFundingHolds(rt *basketRuntime) error {
	if rt == nil || rt.Pair == nil {
		return nil
	}
	for _, h := range rt.Pair.Progress.NativeFundingHolds {
		if !h.bounded() && len(h.Proof) == 0 {
			continue
		}
		if err := pairValidateBoundHold(h); err != nil {
			return fmt.Errorf("pair %s native funding hold %s: %w", rt.Contract.BasketID, h.identity(), err)
		}
		proof, _ := pairParseNativeFundingProof(h.Proof)
		var matches []pairNativeResolution
		for _, r := range rt.Pair.resolutions {
			if r.ID == h.ResolutionID && r.MemberID == h.MemberID && r.Kind == h.Kind && r.ObligationID == h.ObligationID && r.FundingHoldFromMs == h.FromMs && (r.Mode == pairNativeModeWriteOff || r.Mode == pairNativeModeAcknowledge) {
				matches = append(matches, r)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("pair %s native funding hold %s has %d linked resolutions, want exactly 1", rt.Contract.BasketID, h.identity(), len(matches))
		}
		r := matches[0]
		if r.Attempt != nil && r.Attempt.DeadlineMs > 0 {
			limit, ok := pairAddMs(r.Attempt.DeadlineMs, pairNativeAcceptSkewMs)
			if !ok || proof.RequestStartMs <= limit {
				return fmt.Errorf("pair %s native funding hold %s proof request %d is not after the attempt deadline %d plus skew", rt.Contract.BasketID, h.identity(), proof.RequestStartMs, r.Attempt.DeadlineMs)
			}
		}
		if f := rt.Pair.funding; f != nil {
			m := f.members[h.MemberID]
			if m == nil {
				return fmt.Errorf("pair %s native funding hold %s has no funding journal member", rt.Contract.BasketID, h.identity())
			}
			end, ok := pairAddMs(h.ThroughMs, 1)
			if !ok {
				return fmt.Errorf("pair %s native funding hold %s bound overflows", rt.Contract.BasketID, h.identity())
			}
			q, amb := m.qtyBefore(end)
			if amb {
				return fmt.Errorf("pair %s native funding hold %s journal is ambiguous at its bound", rt.Contract.BasketID, h.identity())
			}
			want, err := pairDecimal(proof.BookBaseQty).rat()
			if err != nil || q.Cmp(want) != 0 {
				return fmt.Errorf("pair %s native funding hold %s journal holds %s at its bound, proof holds %s", rt.Contract.BasketID, h.identity(), pairDecimalOfRat(q), proof.BookBaseQty)
			}
		}
	}
	return nil
}

type pairNativeProofToken struct {
	member     pairContractMember
	holds      []pairNativeFundingHold
	epoch      uint64
	coinEpoch  uint64
	nativeGen  uint64
	contractFP string
}

type pairNativeAcceptedProof struct {
	hold  pairNativeFundingHold
	proof pairNativeFundingProof
	raw   json.RawMessage
}

func (bc *basketCoordinator) pairNativeProofPeers(rt *basketRuntime, m pairContractMember) []string {
	if bc == nil || bc.cfg == nil || rt.Contract.Pair == nil {
		return nil
	}
	members := map[string]bool{}
	for _, id := range rt.memberIDs() {
		members[id] = true
	}
	underlying := rt.Contract.Pair.Underlying
	var out []string
	for _, sc := range bc.cfg.Strategies {
		if members[sc.ID] || partitionFor(sc) != rt.Partition || sc.Platform != m.Platform {
			continue
		}
		holds := basketPositionSigned(bc.state.Strategies[sc.ID], m.Symbol) != 0
		sym, _ := strategyArgSymbolTimeframe(sc.Args)
		configured := ((sc.Platform == "hyperliquid" || sc.Platform == "okx") && strings.EqualFold(sym, underlying)) || (HedgeEnabled(sc) && strings.EqualFold(hedgeCoin(sc), underlying))
		if holds || configured {
			out = append(out, sc.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (bc *basketCoordinator) pairNativeProofBlock(rt *basketRuntime, m pairContractMember, saveBlocked bool) string {
	if saveBlocked {
		return "a state save is blocked"
	}
	if rt.needsDurable {
		return "pair settlement is not durable"
	}
	if bc.store != nil && bc.store.persistenceHoldsPartition(rt.Partition) {
		return "the partition has an unacknowledged state save failure"
	}
	if why := bc.pairNativeBusy(rt, m.MemberID); why != "" {
		return why
	}
	if rt.Pair.nativeReads[m.MemberID] != nil {
		return "a native history read is in flight"
	}
	p := &rt.Pair.Progress
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if a.StrategyID != m.MemberID {
			continue
		}
		if a.ConfirmedQty-a.AppliedQty > 1e-12 {
			return "a confirmed fill is not applied"
		}
		if basketActionPending(a.Status) {
			return "a pair action is pending"
		}
	}
	for _, o := range p.Obligations {
		if o.MemberID == m.MemberID && (pairNativeObligationKind(o.Kind) || pairSettlementObligationKind(o.Kind)) {
			return "a native or settlement obligation is open"
		}
	}
	for _, r := range p.NativeReceipts {
		if r.MemberID == m.MemberID {
			return "a native receipt is not attached"
		}
	}
	if p.stagedPaperClose(m.MemberID) != nil {
		return "a paper close is staged"
	}
	for _, r := range p.NativeRequests {
		if r.MemberID == m.MemberID {
			return "an operator decision is staged"
		}
	}
	for _, a := range p.NativeAttempts {
		if a.MemberID == m.MemberID && a.Pending != nil {
			return "a recovered booking is pending"
		}
	}
	if f := rt.Pair.funding; f != nil {
		if fm := f.members[m.MemberID]; fm != nil && fm.durableSeq != int64(len(fm.rows)) {
			return "the funding journal is not durable"
		}
	}
	if peers := bc.pairNativeProofPeers(rt, m); len(peers) > 0 {
		return fmt.Sprintf("peer strategies %s share the coin", strings.Join(peers, ", "))
	}
	return ""
}

func (bc *basketCoordinator) pairNativeProofHoldEligible(rt *basketRuntime, h pairNativeFundingHold, startMs int64) bool {
	if h.bounded() || len(h.Proof) > 0 || h.AtMs >= startMs {
		return false
	}
	if _, bad := rt.Pair.proofUnknown[h.ResolutionID]; bad {
		return false
	}
	var found *pairNativeResolution
	n := 0
	for i := range rt.Pair.resolutions {
		r := &rt.Pair.resolutions[i]
		if r.ID == h.ResolutionID && r.MemberID == h.MemberID && r.Kind == h.Kind && r.ObligationID == h.ObligationID && r.FundingHoldFromMs == h.FromMs && (r.Mode == pairNativeModeWriteOff || r.Mode == pairNativeModeAcknowledge) {
			found = r
			n++
		}
	}
	if n != 1 {
		return false
	}
	if found.Attempt != nil && found.Attempt.DeadlineMs > 0 {
		limit, ok := pairAddMs(found.Attempt.DeadlineMs, pairNativeAcceptSkewMs)
		if !ok || startMs <= limit {
			return false
		}
	}
	return true
}

func (bc *basketCoordinator) pairNativeProofTokens(rt *basketRuntime, ms []pairContractMember, startMs int64, saveBlocked bool) map[string]*pairNativeProofToken {
	out := map[string]*pairNativeProofToken{}
	for _, m := range ms {
		var holds []pairNativeFundingHold
		for _, h := range rt.Pair.Progress.NativeFundingHolds {
			if h.MemberID == m.MemberID && bc.pairNativeProofHoldEligible(rt, h, startMs) {
				holds = append(holds, h)
			}
		}
		if len(holds) == 0 {
			continue
		}
		if bc.pairNativeProofBlock(rt, m, saveBlocked) != "" {
			continue
		}
		sort.Slice(holds, func(i, j int) bool { return holds[i].ResolutionID < holds[j].ResolutionID })
		out[m.MemberID] = &pairNativeProofToken{member: m, holds: holds, epoch: rt.Pair.mutationEpoch, coinEpoch: pairCoinEpoch(m.Symbol), nativeGen: bc.pairNativeGenOf(m.MemberID), contractFP: rt.Contract.Fingerprint()}
	}
	return out
}

func (bc *basketCoordinator) recoverPairNativeProofUnknown(rt *basketRuntime) {
	if rt == nil || rt.Pair == nil || len(rt.Pair.proofUnknown) == 0 || bc.store == nil {
		return
	}
	raw, err := bc.store.SavedPairProgress(rt)
	if err != nil {
		return
	}
	saved, err := parsePairProgress(raw)
	if err != nil {
		return
	}
	for resID, praw := range rt.Pair.proofUnknown {
		for _, sh := range saved.NativeFundingHolds {
			if sh.ResolutionID != resID {
				continue
			}
			if sh.bounded() && bytes.Equal(sh.Proof, praw) {
				for i := range rt.Pair.Progress.NativeFundingHolds {
					h := &rt.Pair.Progress.NativeFundingHolds[i]
					if h.identity() == sh.identity() && !h.bounded() {
						h.ThroughMs, h.Proof = sh.ThroughMs, append(json.RawMessage(nil), sh.Proof...)
					}
				}
				if saved.Seq > rt.Pair.Progress.Seq {
					rt.Pair.Progress.Seq = saved.Seq
				}
			}
			delete(rt.Pair.proofUnknown, resID)
		}
	}
}

func (bc *basketCoordinator) commitPairNativeProofs(rt *basketRuntime, accepted []pairNativeAcceptedProof) {
	snap := pairCloneProgress(rt.Pair.Progress)
	for _, acc := range accepted {
		for i := range rt.Pair.Progress.NativeFundingHolds {
			h := &rt.Pair.Progress.NativeFundingHolds[i]
			if h.identity() == acc.hold.identity() {
				h.ThroughMs, h.Proof = acc.hold.ThroughMs, acc.raw
			}
		}
	}
	rt.touchPairProgress()
	landed := false
	if err := bc.store.PersistBasketProgress(rt); err != nil {
		rt.Pair.Progress = snap
		rawSaved, rerr := bc.store.SavedPairProgress(rt)
		if rerr != nil {
			if rt.Pair.proofUnknown == nil {
				rt.Pair.proofUnknown = map[string]json.RawMessage{}
			}
			for _, acc := range accepted {
				rt.Pair.proofUnknown[acc.hold.ResolutionID] = acc.raw
			}
			bc.pairLogf(rt, "a native funding proof commit failed and its read-back failed: %v / %v; holds stay open and no new proof is tried for them until the read-back succeeds", err, rerr)
			return
		}
		saved, perr := parsePairProgress(rawSaved)
		if perr != nil {
			if rt.Pair.proofUnknown == nil {
				rt.Pair.proofUnknown = map[string]json.RawMessage{}
			}
			for _, acc := range accepted {
				rt.Pair.proofUnknown[acc.hold.ResolutionID] = acc.raw
			}
			bc.pairLogf(rt, "a native funding proof commit failed and its saved progress is unreadable: %v / %v; holds stay open", err, perr)
			return
		}
		landed = true
		for _, acc := range accepted {
			found := false
			for _, sh := range saved.NativeFundingHolds {
				if sh.identity() == acc.hold.identity() && sh.ThroughMs == acc.hold.ThroughMs && bytes.Equal(sh.Proof, acc.raw) {
					found = true
				}
			}
			if !found {
				landed = false
			}
		}
		if !landed {
			bc.pairLogf(rt, "a native funding proof commit did not save: %v; holds stay open in memory and storage", err)
			return
		}
		for _, acc := range accepted {
			for i := range rt.Pair.Progress.NativeFundingHolds {
				h := &rt.Pair.Progress.NativeFundingHolds[i]
				if h.identity() == acc.hold.identity() {
					h.ThroughMs, h.Proof = acc.hold.ThroughMs, acc.raw
				}
			}
		}
		if saved.Seq > rt.Pair.Progress.Seq {
			rt.Pair.Progress.Seq = saved.Seq
		}
	}
	for _, acc := range accepted {
		bc.pairLogf(rt, "member %s native funding hold %s is bounded through %d by an exact venue position proof", acc.hold.MemberID, acc.hold.ResolutionID, acc.hold.ThroughMs)
		bc.pairAlert(rt, "native-hold-bounded|"+acc.hold.ResolutionID, fmt.Sprintf("pair %s member %s native funding hold from %d is bounded through %d by a saved exact venue position proof; the historical gap stays incomplete evidence", rt.Contract.BasketID, acc.hold.MemberID, acc.hold.FromMs, acc.hold.ThroughMs), false)
	}
}

func (bc *basketCoordinator) pairEvaluateNativeProofs(rt *basketRuntime, ms []pairContractMember, got []pairVenuePosition, errs []error, answered []bool, tokens map[string]*pairNativeProofToken, startMs, nowMs int64) {
	if len(tokens) == 0 {
		return
	}
	var accepted []pairNativeAcceptedProof
	for i, m := range ms {
		tok := tokens[m.MemberID]
		if tok == nil || !answered[i] || errs[i] != nil {
			continue
		}
		if err := pairCheckVenuePosition(rt, m, got[i], nowMs); err != nil {
			continue
		}
		if rt.Contract.Fingerprint() != tok.contractFP || rt.Pair.mutationEpoch != tok.epoch || pairCoinEpoch(m.Symbol) != tok.coinEpoch || bc.pairNativeGenOf(m.MemberID) != tok.nativeGen {
			continue
		}
		if bc.pairNativeProofBlock(rt, m, false) != "" {
			continue
		}
		still := true
		for _, th := range tok.holds {
			found := false
			for _, h := range rt.Pair.Progress.NativeFundingHolds {
				if h.identity() == th.identity() && !h.bounded() && len(h.Proof) == 0 && h.FromMs == th.FromMs && h.AtMs == th.AtMs {
					found = true
				}
			}
			if !found {
				still = false
			}
		}
		if !still {
			continue
		}
		bookRat, err := pairExactBookRat(bc.state.Strategies[m.MemberID], m.Symbol)
		if err != nil {
			continue
		}
		baseRat, err := pairPositionBase(got[i])
		if err != nil || bookRat.Cmp(baseRat) != 0 {
			continue
		}
		venueQty, err := pairRatDecimal(baseRat)
		if err != nil {
			continue
		}
		if f := rt.Pair.funding; f != nil {
			fm := f.members[m.MemberID]
			if fm == nil {
				continue
			}
			end, ok := pairAddMs(got[i].FirstReceiptMs, 1)
			if !ok {
				continue
			}
			q, amb := fm.qtyBefore(end)
			if amb || q.Cmp(bookRat) != 0 {
				continue
			}
		}
		for _, th := range tok.holds {
			cand := th
			cand.ThroughMs = got[i].FirstReceiptMs
			proof := pairNativeFundingProof{Partition: rt.Partition.String(), PairID: rt.Contract.BasketID, MemberID: m.MemberID, Kind: th.Kind, ObligationID: th.ObligationID, ResolutionID: th.ResolutionID,
				HoldFromMs: th.FromMs, HoldAtMs: th.AtMs, ContractFP: rt.Contract.Fingerprint(), ContractJSON: rt.Contract.JSON(), Position: got[i], VenueBaseQty: venueQty, BookBaseQty: venueQty,
				RequestStartMs: startMs, AcceptedAtMs: nowMs}
			built, raw, err := pairBuildNativeFundingProof(proof)
			if err != nil {
				continue
			}
			cand.Proof = raw
			if err := pairCheckNativeFundingProof(cand, built); err != nil {
				continue
			}
			accepted = append(accepted, pairNativeAcceptedProof{hold: cand, proof: built, raw: raw})
		}
	}
	if len(accepted) > 0 {
		bc.commitPairNativeProofs(rt, accepted)
	}
}
