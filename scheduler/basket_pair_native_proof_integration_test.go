package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

func pairProofHold(e *pairTestEnv, member string) *pairNativeFundingHold {
	for i := range e.rt().Pair.Progress.NativeFundingHolds {
		h := &e.rt().Pair.Progress.NativeFundingHolds[i]
		if h.MemberID == member {
			return h
		}
	}
	return nil
}

func pairProofInexactPosition(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
	cs, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		return pairVenuePosition{}, err
	}
	p := pairVenuePosition{V: pairVenuePositionVersion, Key: pairEvidenceKey{Partition: "live", Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID},
		SignedNativeQty: "0.000000000001", ContractSize: string(cs), PositionMode: m.Meta.PositionMode, MarginMode: m.Meta.MarginMode,
		ObservedMs: nowMs, FirstReceiptMs: nowMs, EvidenceID: "fixture-position-inexact"}
	return p.sealed()
}

func pairProofSetupOpenHold(t *testing.T, inexact bool) *pairNativeRig {
	t.Helper()
	r := newPairNativeRig(t)
	e := r.e
	r.f.fire()
	from := e.now.UnixMilli() - 1000
	id := "pnr-00112233445566778899aabbccddeeff"
	e.mu.Lock()
	e.rt().Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: pairTestHL, ActionID: id, Detail: "saved before attempts", AtMs: e.now.UnixMilli(), FromMs: from})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	r.step(time.Second)
	e.venues.of(pairTestHL).setVenueBase(0)
	e.venues.of(pairTestOKX).setVenueBase(0)
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	if inexact {
		e.venues.of(pairTestHL).position = pairProofInexactPosition
	}
	d, _ := r.discord()
	if msg := r.decide(d, pairTestHL, pairObligationNativeUnknown, id, pairNativeDecisionWriteOff, "venue history purged; the owner accepts the gap"); !strings.Contains(msg, "Staged") {
		t.Fatalf("the write-off must stage: %q", msg)
	}
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("setup: the flat reset must finish, lifecycle %s", e.lifecycle())
	}
	h := pairProofHold(e, pairTestHL)
	if h == nil {
		t.Fatal("setup: the write-off must leave a funding hold")
	}
	if inexact && h.bounded() {
		t.Fatal("setup: an inexact read must leave the hold open")
	}
	return r
}

func pairProofRead(t *testing.T, e *pairTestEnv) {
	t.Helper()
	e.now = e.now.Add(time.Minute)
	e.bc.beginCycle()
	e.observe("")
	e.mu.Lock()
	_, _ = e.bc.pairReadPositions(e.rt(), e.inputs(e.now), "", true)
	e.mu.Unlock()
}

func TestBasketPairNativeFundingProofBoundsAHoldAndReleasesEntries(t *testing.T) {
	r := pairProofSetupOpenHold(t, false)
	e := r.e
	h := pairProofHold(e, pairTestHL)
	if h == nil || !h.bounded() || len(h.Proof) == 0 {
		t.Fatalf("an exact flat read after a write-off must bound the hold: %+v", h)
	}
	proof, err := pairParseNativeFundingProof(h.Proof)
	if err != nil || proof.Position.FirstReceiptMs != h.ThroughMs || proof.VenueBaseQty != "0" || proof.BookBaseQty != "0" {
		t.Fatalf("the saved proof must name the bound as its receipt time with zero quantities: %+v %v", proof, err)
	}
	if got := pairNativeUnknownFromMs(e.rt(), pairTestHL); got != 0 {
		t.Fatalf("a bounded hold alone must not cap the unresolved inventory horizon, got %d", got)
	}
	if o, ok := pairIncompleteEvidence(e.rt()); !ok || !strings.Contains(o.Detail, "bounded through") {
		t.Fatalf("the historical gap must stay incomplete evidence after the bound: %+v %v", o, ok)
	}
	if res := r.savedResolutions(pairTestHL); len(res) != 1 || res[0].FundingHoldFromMs != h.FromMs {
		t.Fatalf("the original resolution must be unchanged: %+v", res)
	}
	rr := &pairReplayRuntime{rt: e.rt(), state: e.state, cfg: e.cfg, bc: e.bc, mu: &sync.RWMutex{}, partition: e.rt().Partition, members: []StrategyConfig{e.bc.members[pairTestHL], e.bc.members[pairTestOKX]}}
	_, _, pr := rr.stateReadingsLocked(e.now)
	if len(pr.NativeFundingHolds) != 1 || !strings.Contains(pr.NativeFundingHolds[0], pairTestHL) || !strings.Contains(pr.NativeFundingHolds[0], fmt.Sprintf("%d", h.ThroughMs)) {
		t.Fatalf("replay must list the bounded hold in stable form: %+v", pr.NativeFundingHolds)
	}
	savedProof := append(json.RawMessage(nil), h.Proof...)
	savedThrough := h.ThroughMs
	r.restart()
	h = pairProofHold(e, pairTestHL)
	if h == nil || h.ThroughMs != savedThrough || string(h.Proof) != string(savedProof) {
		t.Fatalf("the bound and proof must survive a restart byte-equal: %+v", h)
	}
	r.step(time.Minute)
	e.mu.Lock()
	hold := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
	e.mu.Unlock()
	if strings.Contains(hold, "native funding hold") {
		t.Fatalf("a bounded hold alone must not block an entry: %q", hold)
	}
	entries := len(e.actionsByPurpose(pairPurposeEntry))
	at := e.now.Truncate(time.Minute).Add(time.Minute)
	e.slot(at, "after-proof")
	e.step(at.Add(time.Second))
	e.step(at.Add(30 * time.Second))
	if got := len(e.actionsByPurpose(pairPurposeEntry)); got <= entries {
		t.Fatalf("an otherwise valid entry after the bound must send orders: entries %d -> %d", entries, got)
	}
}

func TestBasketPairNativeFundingProofNonUnitOKXExact(t *testing.T) {
	r := newPairNativeRig(t)
	e := r.e
	from := e.now.UnixMilli() - 1000
	id := "pnr-00112233445566778899aabbccddee00"
	e.mu.Lock()
	e.rt().Pair.Progress.addObligation(pairObligation{Kind: pairObligationNativeUnknown, MemberID: pairTestOKX, ActionID: id, Detail: "saved before attempts", AtMs: e.now.UnixMilli(), FromMs: from})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	d, _ := r.discord()
	if msg := r.decide(d, pairTestOKX, pairObligationNativeUnknown, id, pairNativeDecisionWriteOff, "venue history purged"); !strings.Contains(msg, "Staged") {
		t.Fatalf("the write-off must stage: %q", msg)
	}
	for i := 0; i < 3; i++ {
		r.step(time.Minute)
	}
	h := pairProofHold(e, pairTestOKX)
	if h == nil || !h.bounded() {
		t.Fatalf("an exact OKX read must bound its hold: %+v", h)
	}
	proof, err := pairParseNativeFundingProof(h.Proof)
	if err != nil || proof.BookBaseQty != "-0.06" || proof.VenueBaseQty != "-0.06" || proof.Position.SignedNativeQty != "-6" || proof.Position.ContractSize != "0.01" {
		t.Fatalf("a non-unit OKX contract size must convert exactly to base: %+v %v", proof, err)
	}
}

func TestBasketPairNativeFundingProofRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *pairNativeRig)
	}{
		{"reachable but inexact", func(r *pairNativeRig) {}},
		{"float drift book", func(r *pairNativeRig) {
			e := r.e
			e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				return pairTestVenuePosition(m, 0.3, nowMs, nowMs)
			}
			e.mu.Lock()
			s := e.state.Strategies[pairTestHL]
			driftA, driftB := 0.1, 0.2
			s.Positions["BTC"] = &Position{Symbol: "BTC", Side: entrySideLong, Quantity: driftA + driftB, AvgCost: 60000, Multiplier: 1}
			e.mu.Unlock()
		}},
		{"wrong identity", func(r *pairNativeRig) {
			r.e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				p, err := pairTestVenuePosition(m, 0, nowMs, nowMs)
				if err != nil {
					return p, err
				}
				p.Key.MemberID = "pair-other"
				p.EvidenceHash = ""
				return p.sealed()
			}
		}},
		{"bad hash", func(r *pairNativeRig) {
			r.e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				p, err := pairTestVenuePosition(m, 0, nowMs, nowMs)
				p.SignedNativeQty = "1"
				return p, err
			}
		}},
		{"stale read", func(r *pairNativeRig) {
			r.e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				return pairTestVenuePosition(m, 0, nowMs-10000, nowMs-10000)
			}
		}},
		{"failed read", func(r *pairNativeRig) {
			r.e.venues.of(pairTestHL).position = func(pairContractMember, int64) (pairVenuePosition, error) {
				return pairVenuePosition{}, fmt.Errorf("venue unreachable")
			}
		}},
		{"pending action", func(r *pairNativeRig) {
			e := r.e
			e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				return pairTestVenuePosition(m, 0, nowMs, nowMs)
			}
			e.mu.Lock()
			e.rt().Actions["proof-pending"] = &basketAction{ActionID: "proof-pending", StrategyID: pairTestHL, Status: basketActionReserved}
			e.mu.Unlock()
		}},
		{"staged operator decision", func(r *pairNativeRig) {
			e := r.e
			e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				return pairTestVenuePosition(m, 0, nowMs, nowMs)
			}
			e.mu.Lock()
			e.rt().Pair.Progress.NativeRequests = append(e.rt().Pair.Progress.NativeRequests, pairNativeOperatorRequest{ID: "pnq-proof", MemberID: pairTestHL})
			e.mu.Unlock()
		}},
		{"peer holds the coin", func(r *pairNativeRig) {
			e := r.e
			e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				return pairTestVenuePosition(m, 0, nowMs, nowMs)
			}
			e.mu.Lock()
			peer := e.bc.members[pairTestHL]
			peer.ID, peer.PairID = "pair-peer-hl", ""
			e.cfg.Strategies = append(e.cfg.Strategies, peer)
			s := NewStrategyState(peer)
			s.Positions["BTC"] = &Position{Symbol: "BTC", Side: entrySideLong, Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
			e.state.Strategies[peer.ID] = s
			e.mu.Unlock()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pairProofSetupOpenHold(t, true)
			e := r.e
			tc.setup(r)
			before := r.savedProgressValue()
			pairProofRead(t, e)
			h := pairProofHold(e, pairTestHL)
			if h == nil || h.bounded() {
				t.Fatalf("a refused proof must leave the hold open: %+v", h)
			}
			if got := r.savedProgressValue(); len(got.NativeFundingHolds) != len(before.NativeFundingHolds) || got.NativeFundingHolds[0].ThroughMs != 0 || len(got.NativeFundingHolds[0].Proof) != 0 {
				t.Fatalf("a refused proof must leave the saved hold open: %+v", got.NativeFundingHolds)
			}
			e.mu.Lock()
			hold := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
			e.mu.Unlock()
			if !strings.Contains(hold, "native funding hold") {
				t.Fatalf("a refused proof must keep increases held: %q", hold)
			}
		})
	}
}

func TestBasketPairNativeFundingProofDiscardsAChangeDuringTheRead(t *testing.T) {
	cases := []struct {
		name   string
		during func(e *pairTestEnv)
	}{
		{"native send begins and ends", func(e *pairTestEnv) {
			e.bc.pairNativeSendBegan(pairTestHL)
			e.bc.pairNativeSendEnded(pairTestHL)
		}},
		{"book changes and returns through the wrapper", func(e *pairTestEnv) {
			s := e.state.Strategies[pairTestHL]
			mutatePaperPerpsBook(s, "BTC", func() {
				s.Positions["BTC"] = &Position{Symbol: "BTC", Side: entrySideLong, Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
			})
			mutatePaperPerpsBook(s, "BTC", func() { delete(s.Positions, "BTC") })
		}},
		{"progress touches and the action set returns", func(e *pairTestEnv) {
			rt := e.rt()
			rt.Actions["proof-temp"] = &basketAction{ActionID: "proof-temp", StrategyID: pairTestHL, Status: basketActionCommitted}
			rt.bumpPairMutationEpoch()
			delete(rt.Actions, "proof-temp")
			rt.bumpPairMutationEpoch()
		}},
		{"peer opens and closes", func(e *pairTestEnv) {
			peer := e.bc.members[pairTestHL]
			peer.ID, peer.PairID = "pair-peer-race", ""
			e.cfg.Strategies = append(e.cfg.Strategies, peer)
			s := NewStrategyState(peer)
			e.state.Strategies[peer.ID] = s
			mutatePaperPerpsBook(s, "BTC", func() {
				s.Positions["BTC"] = &Position{Symbol: "BTC", Side: entrySideLong, Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
			})
			mutatePaperPerpsBook(s, "BTC", func() { delete(s.Positions, "BTC") })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pairProofSetupOpenHold(t, true)
			e := r.e
			during := tc.during
			e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
				e.mu.Lock()
				during(e)
				e.mu.Unlock()
				return pairTestVenuePosition(m, 0, nowMs, nowMs)
			}
			pairProofRead(t, e)
			if h := pairProofHold(e, pairTestHL); h == nil || h.bounded() {
				t.Fatalf("a change during the read must discard the proof: %+v", h)
			}
			e.venues.of(pairTestHL).position = nil
			if tc.name == "peer opens and closes" {
				e.mu.Lock()
				kept := e.cfg.Strategies[:0]
				for _, sc := range e.cfg.Strategies {
					if sc.ID != "pair-peer-race" {
						kept = append(kept, sc)
					}
				}
				e.cfg.Strategies = kept
				delete(e.state.Strategies, "pair-peer-race")
				e.mu.Unlock()
			}
			pairProofRead(t, e)
			if h := pairProofHold(e, pairTestHL); h == nil || !h.bounded() {
				t.Fatalf("a later clean read must bound the hold: %+v", h)
			}
		})
	}
}

func TestBasketPairNativeFundingProofCommitFailureKeepsTheHold(t *testing.T) {
	r := pairProofSetupOpenHold(t, true)
	e := r.e
	e.venues.of(pairTestHL).position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
		return pairTestVenuePosition(m, 0, nowMs, nowMs)
	}
	prev := storeCommitHook
	storeCommitHook = func(storageRole) error { return fmt.Errorf("disk full") }
	t.Cleanup(func() { storeCommitHook = prev })
	pairProofRead(t, e)
	storeCommitHook = prev
	if h := pairProofHold(e, pairTestHL); h == nil || h.bounded() {
		t.Fatalf("a failed commit must leave the hold open in memory: %+v", h)
	}
	if got := r.savedProgressValue(); len(got.NativeFundingHolds) == 0 || got.NativeFundingHolds[0].ThroughMs != 0 {
		t.Fatalf("a failed commit must leave the saved hold open: %+v", got.NativeFundingHolds)
	}
	e.mu.Lock()
	e.rt().needsDurable = false
	_ = e.bc.persistPair(e.rt())
	e.mu.Unlock()
	pairProofRead(t, e)
	h := pairProofHold(e, pairTestHL)
	if h == nil || !h.bounded() {
		t.Fatalf("a retry after a failed commit must bound the hold once: %+v", h)
	}
	proof := append(json.RawMessage(nil), h.Proof...)
	through := h.ThroughMs
	pairProofRead(t, e)
	if h = pairProofHold(e, pairTestHL); h.ThroughMs != through || string(h.Proof) != string(proof) {
		t.Fatalf("a later read must not move or duplicate the bound: %+v", h)
	}
	e.mu.Lock()
	for i := range e.rt().Pair.Progress.NativeFundingHolds {
		e.rt().Pair.Progress.NativeFundingHolds[i].ThroughMs, e.rt().Pair.Progress.NativeFundingHolds[i].Proof = 0, nil
	}
	if e.rt().Pair.proofUnknown == nil {
		e.rt().Pair.proofUnknown = map[string]json.RawMessage{}
	}
	e.rt().Pair.proofUnknown[h.ResolutionID] = proof
	e.mu.Unlock()
	pairProofRead(t, e)
	if h = pairProofHold(e, pairTestHL); h == nil || h.ThroughMs != through || string(h.Proof) != string(proof) {
		t.Fatalf("the read-back must adopt the landed bound once: %+v", h)
	}
}

func TestBasketPairNativeFundingProofSavedBoundIsImmutable(t *testing.T) {
	r := pairProofSetupOpenHold(t, false)
	e := r.e
	before := r.savedProgressValue()
	e.mu.Lock()
	for i := range e.rt().Pair.Progress.NativeFundingHolds {
		e.rt().Pair.Progress.NativeFundingHolds[i].ThroughMs, e.rt().Pair.Progress.NativeFundingHolds[i].Proof = 0, nil
	}
	ok := e.bc.persistPair(e.rt())
	e.mu.Unlock()
	if ok {
		t.Fatal("a progress write that removes a saved bound must be refused")
	}
	after := r.savedProgressValue()
	if len(after.NativeFundingHolds) != len(before.NativeFundingHolds) || after.NativeFundingHolds[0].ThroughMs != before.NativeFundingHolds[0].ThroughMs || string(after.NativeFundingHolds[0].Proof) != string(before.NativeFundingHolds[0].Proof) {
		t.Fatalf("the saved bound must be unchanged after a refused write: %+v", after.NativeFundingHolds)
	}
}

func TestBasketPairNativeFundingProofLoadRefusesACorruptOrUnlinkedProof(t *testing.T) {
	r := pairProofSetupOpenHold(t, false)
	e := r.e
	saved := r.savedProgressValue()
	h := saved.NativeFundingHolds[0]
	proof, err := pairParseNativeFundingProof(h.Proof)
	if err != nil {
		t.Fatal(err)
	}
	proof.BookBaseQty, proof.VenueBaseQty = "1", "1"
	_, raw, err := pairBuildNativeFundingProof(proof)
	if err != nil {
		t.Fatal(err)
	}
	saved.NativeFundingHolds[0].Proof = raw
	if _, err := e.db().db.Exec(`UPDATE basket_contracts SET progress_json = ? WHERE basket_id = ?`, saved.JSON(), pairTestID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.LoadBaskets(e.state, e.cfg); err == nil || !strings.Contains(err.Error(), "native funding hold") {
		t.Fatalf("a corrupt bounded proof must refuse the load before mutation: %v", err)
	}
	saved.NativeFundingHolds[0].ThroughMs, saved.NativeFundingHolds[0].Proof = 0, nil
	if _, err := e.db().db.Exec(`UPDATE basket_contracts SET progress_json = ? WHERE basket_id = ?`, saved.JSON(), pairTestID); err != nil {
		t.Fatal(err)
	}
	runtimes, err := e.store.LoadBaskets(e.state, e.cfg)
	if err != nil {
		t.Fatalf("a downgraded progress with an open hold must load: %v", err)
	}
	rt := runtimes[pairTestID]
	if rt == nil || len(rt.Pair.Progress.NativeFundingHolds) != 1 || rt.Pair.Progress.NativeFundingHolds[0].ThroughMs != 0 {
		t.Fatalf("a downgraded hold must load open: %+v", rt)
	}
}

func pairProofBoundedResearchHold(t *testing.T, e *pairTestEnv, member string, fromMs, throughMs int64, baseQty string) pairNativeResolution {
	t.Helper()
	rt := e.rt()
	m, ok := rt.Contract.pairMember(member)
	if !ok {
		t.Fatalf("member %s is not in the contract", member)
	}
	cs, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		t.Fatal(err)
	}
	qtyRat, err := pairDecimal(baseQty).rat()
	if err != nil {
		t.Fatal(err)
	}
	native := new(big.Rat).Quo(qtyRat, pairRatOf(cs))
	nativeStr, err := pairRatDecimal(native)
	if err != nil {
		t.Fatal(err)
	}
	margin := m.Meta.MarginMode
	if baseQty == "0" {
		margin = ""
	}
	pos := pairVenuePosition{V: pairVenuePositionVersion, Key: rt.Contract.pairMemberKey(m).evidenceKey(), SignedNativeQty: nativeStr, ContractSize: string(cs),
		PositionMode: m.Meta.PositionMode, MarginMode: margin, ObservedMs: throughMs, FirstReceiptMs: throughMs, EvidenceID: "fixture-proof-" + member}
	pos, err = pos.sealed()
	if err != nil {
		t.Fatal(err)
	}
	resID := "pnz-proof-" + member
	hold := pairNativeFundingHold{MemberID: member, Kind: pairObligationNativeUnknown, ObligationID: "pna-proof-" + member, ResolutionID: resID,
		FromMs: fromMs, AtMs: fromMs, Detail: "the operator write_off left the inventory unproven", ThroughMs: throughMs}
	proof := pairNativeFundingProof{Partition: rt.Partition.String(), PairID: rt.Contract.BasketID, MemberID: member, Kind: hold.Kind, ObligationID: hold.ObligationID, ResolutionID: resID,
		HoldFromMs: fromMs, HoldAtMs: fromMs, ContractFP: rt.Contract.Fingerprint(), ContractJSON: rt.Contract.JSON(), Position: pos, VenueBaseQty: baseQty, BookBaseQty: baseQty,
		RequestStartMs: throughMs - 100, AcceptedAtMs: throughMs + 100}
	_, raw, err := pairBuildNativeFundingProof(proof)
	if err != nil {
		t.Fatal(err)
	}
	hold.Proof = raw
	res := pairNativeResolution{ID: resID, Partition: rt.Partition.String(), PairID: rt.Contract.BasketID, MemberID: member, Kind: hold.Kind, ObligationID: hold.ObligationID,
		Mode: pairNativeModeWriteOff, DecidedBy: pairNativeDecidedByOperator, Operator: "owner", DecidedAtMs: fromMs, CommittedAtMs: fromMs, FundingHoldFromMs: fromMs}
	e.mu.Lock()
	rt.Pair.Progress.NativeFundingHolds = append(rt.Pair.Progress.NativeFundingHolds, hold)
	rt.touchPairProgress()
	err = e.bc.store.CommitPairNative(rt, []pairNativeResolution{res}, nil, nil)
	e.mu.Unlock()
	if err != nil {
		t.Fatalf("save the bounded hold and its resolution: %v", err)
	}
	return res
}

func TestBasketPairFundingBoundedNativeHoldSettlesOnlyAfterTheBound(t *testing.T) {
	t0 := pairTestStart()
	from := pairSlotAt(t0, 2).UnixMilli()
	through := pairSlotAt(t0, 3).UnixMilli()
	inside := pairSlotAt(t0, 2).Add(30 * time.Second).UnixMilli()
	after := pairSlotAt(t0, 4).UnixMilli()
	insideID := pairFundingEventID("hyperliquid", "BTC", inside)
	afterID := pairFundingEventID("hyperliquid", "BTC", after)
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, inside, "0.0001"), pfRate(k[pairTestHL], pairFundingRuleHyperliquid, after, "0.0001")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, inside, "60000"), pfPrice(k[pairTestHL], pairFundingPriceOracle, after, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	e.openMatched(0.05)
	pairProofBoundedResearchHold(t, e, pairTestHL, from, through, "0.05")
	e.restart()
	e.step(pairSlotAt(t0, 6))
	if status, reason, _ := e.fundingRow(pairTestHL, insideID); status != pairFundingStatusUnresolvable || reason != pairFundingReasonNativeDecision {
		t.Fatalf("an event inside the bounded interval must stay unresolvable, got %q %q", status, reason)
	}
	if status, _, _ := e.fundingRow(pairTestHL, afterID); status != pairFundingStatusApplied {
		t.Fatalf("a nonzero event wholly after the bound must settle, got %q", status)
	}
	if n := len(e.fundingTrades(pairTestHL)); n != 1 {
		t.Fatalf("only the after-bound event must charge once, got %d funding trades", n)
	}
	cash := e.cash(pairTestHL)
	e.restart()
	e.step(pairSlotAt(t0, 7))
	if n := len(e.fundingTrades(pairTestHL)); n != 1 || e.cash(pairTestHL) != cash {
		t.Fatalf("a restart must not charge the after-bound event again: trades %d cash %v want %v", n, e.cash(pairTestHL), cash)
	}
	if status, _, _ := e.fundingRow(pairTestHL, insideID); status != pairFundingStatusUnresolvable {
		t.Fatalf("the earlier unresolvable row must stay saved and unchanged, got %q", status)
	}
	if o, ok := pairIncompleteEvidence(e.rt()); !ok || o.Kind != pairObligationNativeFundingUncertain || !strings.Contains(o.Detail, "bounded through") {
		t.Fatalf("the historical gap must stay incomplete after the bound: %+v %v", o, ok)
	}
}

func TestBasketPairFundingMultipleNativeHoldsEachKeepTheirInterval(t *testing.T) {
	t0 := pairTestStart()
	from := pairSlotAt(t0, 2).UnixMilli()
	through := pairSlotAt(t0, 3).UnixMilli()
	after := pairSlotAt(t0, 4).UnixMilli()
	afterID := pairFundingEventID("hyperliquid", "BTC", after)
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, after, "0.0001")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, after, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	e.openMatched(0.05)
	pairProofBoundedResearchHold(t, e, pairTestHL, from, through, "0.05")
	e.mu.Lock()
	e.rt().Pair.Progress.NativeFundingHolds = append(e.rt().Pair.Progress.NativeFundingHolds, pairNativeFundingHold{MemberID: pairTestHL, Kind: pairObligationNativeUnknown,
		ObligationID: "pna-proof-later", ResolutionID: "pnz-proof-later", FromMs: through + 30000, AtMs: through + 30000, Detail: "a later operator decision left the inventory unproven"})
	e.rt().touchPairProgress()
	e.bc.persistPair(e.rt())
	e.mu.Unlock()
	e.restart()
	e.step(pairSlotAt(t0, 6))
	if status, reason, _ := e.fundingRow(pairTestHL, afterID); status != pairFundingStatusUnresolvable || reason != pairFundingReasonNativeDecision {
		t.Fatalf("a later open hold must keep its own interval even after an earlier bound, got %q %q", status, reason)
	}
	if n := len(e.fundingTrades(pairTestHL)); n != 0 {
		t.Fatalf("an event inside the later open hold must charge nothing, got %d", n)
	}
}
