package main

import (
	"context"
	"fmt"
	"sync"
)

const pairEvidenceSourceRecorded = "recorded"

type pairAdmittedEvidence interface {
	pairEvidenceSource
	Digest() string
	bind(c basketContract) error
	SettlementConversion(ctx context.Context, m pairContractMember, currency string, fillAtMs, settlementAtMs int64) (pairConversionEvidence, error)
	FundingConversion(m pairContractMember, currency string, eventMs, cutoffMs int64) (pairConversionEvidence, string, error)
	hasConversion(c pairConversionEvidence) bool
}

type pairRecordedEvidenceIdentity struct {
	V          int    `json:"v"`
	SourceKind string `json:"source_kind"`
	SourceID   string `json:"source_id"`
	AnchorMs   int64  `json:"anchor_ms"`
	SeedDigest string `json:"seed_digest"`
}

type pairRecordedEvidenceSource struct {
	mu       sync.RWMutex
	identity pairRecordedEvidenceIdentity
	digest   string
	seed     *pairFrozenEvidenceSource
	seedDoc  pairEvidenceDocument
	doc      pairEvidenceDocument
	commit   map[string]int64
	active   bool
	bound    bool
	boundFP  string
	part     string
	members  map[string]pairContractMember
}

func newPairRecordedEvidenceSource(seed []byte, seedDigest, sourceID string, anchorMs int64) (*pairRecordedEvidenceSource, error) {
	frozen, err := newPairFrozenEvidenceSource(seed, seedDigest)
	if err != nil {
		return nil, fmt.Errorf("the recorded evidence seed is refused: %w", err)
	}
	if !pairPrintableASCII(sourceID, pairEvidenceMaxIDLen) {
		return nil, fmt.Errorf("recorded evidence source id must be 1..%d printable ASCII characters", pairEvidenceMaxIDLen)
	}
	if anchorMs <= 0 {
		return nil, fmt.Errorf("recorded evidence needs a positive anchor time, got %d", anchorMs)
	}
	id := pairRecordedEvidenceIdentity{V: 1, SourceKind: pairEvidenceSourceRecorded, SourceID: sourceID, AnchorMs: anchorMs, SeedDigest: seedDigest}
	raw, err := pairCanonicalJSON(id)
	if err != nil {
		return nil, err
	}
	s := &pairRecordedEvidenceSource{identity: id, digest: pairSHA256(raw), seed: frozen, seedDoc: frozen.doc, commit: map[string]int64{},
		doc: pairEvidenceDocument{V: pairEvidenceVersion, Capabilities: []pairCapabilityEvidence{}, Accounts: []pairAccountEvidence{}, Conversions: []pairConversionEvidence{}}}
	return s, nil
}

func (s *pairRecordedEvidenceSource) Digest() string { return s.digest }

func (s *pairRecordedEvidenceSource) Identity() pairRecordedEvidenceIdentity { return s.identity }

func (s *pairRecordedEvidenceSource) bind(c basketContract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bound {
		if s.boundFP == c.Fingerprint() {
			return nil
		}
		return fmt.Errorf("recorded pair evidence is already bound to contract %s", shortFingerprint(s.boundFP))
	}
	if err := s.seed.bind(c); err != nil {
		return fmt.Errorf("the recorded evidence seed: %w", err)
	}
	s.bound, s.boundFP, s.part, s.members = true, c.Fingerprint(), c.Partition, s.seed.members
	return nil
}

func (s *pairRecordedEvidenceSource) checkRecordLocked(k pairEvidenceKey, stamp pairEvidenceStamp, hash, what string) error {
	if !s.bound {
		return fmt.Errorf("recorded pair evidence is not bound to a contract")
	}
	if k.Partition != s.part {
		return fmt.Errorf("%s evidence %s key %s is outside the pair partition %s", what, stamp.EvidenceID, k, s.part)
	}
	m, ok := s.members[k.MemberID]
	if !ok {
		return fmt.Errorf("%s evidence %s names no pair member", what, stamp.EvidenceID)
	}
	if err := pairCheckKeyForMember(k, m); err != nil {
		return err
	}
	return pairValidateStamp(stamp, hash, what)
}

func (s *pairRecordedEvidenceSource) admitCapability(c pairCapabilityEvidence, logicalMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := c.computeHash()
	if err != nil {
		return err
	}
	if err := s.checkRecordLocked(c.Key, c.pairEvidenceStamp, h, "capability"); err != nil {
		return err
	}
	if err := pairValidateWireMeta(c.Key.Provider, c.Meta); err != nil {
		return err
	}
	if _, dup := s.commit[c.EvidenceHash]; dup {
		return nil
	}
	cc := c
	cc.Meta = c.Meta.clone()
	s.doc.Capabilities = append(s.doc.Capabilities, cc)
	pairSortCapabilities(s.doc.Capabilities)
	s.commit[c.EvidenceHash] = logicalMs
	return nil
}

func (s *pairRecordedEvidenceSource) admitAccount(a pairAccountEvidence, logicalMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := a.computeHash()
	if err != nil {
		return err
	}
	if err := s.checkRecordLocked(a.Key, a.pairEvidenceStamp, h, "account"); err != nil {
		return err
	}
	if _, dup := s.commit[a.EvidenceHash]; dup {
		return nil
	}
	s.doc.Accounts = append(s.doc.Accounts, a)
	pairSortAccounts(s.doc.Accounts)
	s.commit[a.EvidenceHash] = logicalMs
	return nil
}

func (s *pairRecordedEvidenceSource) admitConversion(c pairConversionEvidence, logicalMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := c.computeHash()
	if err != nil {
		return err
	}
	if err := s.checkRecordLocked(c.Key, c.pairEvidenceStamp, h, "conversion"); err != nil {
		return err
	}
	if _, dup := s.commit[c.EvidenceHash]; dup {
		return nil
	}
	s.doc.Conversions = append(s.doc.Conversions, c)
	pairSortConversions(s.doc.Conversions)
	s.commit[c.EvidenceHash] = logicalMs
	return nil
}

func (s *pairRecordedEvidenceSource) activate() {
	s.mu.Lock()
	s.active = true
	s.mu.Unlock()
}

func (s *pairRecordedEvidenceSource) seedRecords(memberID string) ([]pairCapabilityEvidence, []pairAccountEvidence, []pairConversionEvidence) {
	var caps []pairCapabilityEvidence
	var accts []pairAccountEvidence
	var convs []pairConversionEvidence
	for _, r := range s.seedDoc.Capabilities {
		if r.Key.MemberID == memberID {
			r.Meta = r.Meta.clone()
			caps = append(caps, r)
		}
	}
	for _, r := range s.seedDoc.Accounts {
		if r.Key.MemberID == memberID {
			accts = append(accts, r)
		}
	}
	for _, r := range s.seedDoc.Conversions {
		if r.Key.MemberID == memberID {
			convs = append(convs, r)
		}
	}
	return caps, accts, convs
}

func (s *pairRecordedEvidenceSource) view(visibleMs int64) (*pairFrozenEvidenceSource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.bound {
		return nil, fmt.Errorf("recorded pair evidence is not bound to a contract")
	}
	if !s.active {
		return nil, fmt.Errorf("recorded pair evidence has no committed bootstrap record yet; nothing is read before the recorder commits it")
	}
	d := pairEvidenceDocument{V: pairEvidenceVersion}
	for _, r := range s.doc.Capabilities {
		if s.commit[r.EvidenceHash] <= visibleMs {
			r.Meta = r.Meta.clone()
			d.Capabilities = append(d.Capabilities, r)
		}
	}
	for _, r := range s.doc.Accounts {
		if s.commit[r.EvidenceHash] <= visibleMs {
			d.Accounts = append(d.Accounts, r)
		}
	}
	for _, r := range s.doc.Conversions {
		if s.commit[r.EvidenceHash] <= visibleMs {
			d.Conversions = append(d.Conversions, r)
		}
	}
	return &pairFrozenEvidenceSource{digest: s.digest, doc: d, bound: true, boundFP: s.boundFP, partition: s.part, members: s.members}, nil
}

func (s *pairRecordedEvidenceSource) Capability(ctx context.Context, m pairContractMember, refMs int64) (pairCapabilityEvidence, error) {
	v, err := s.view(refMs)
	if err != nil {
		return pairCapabilityEvidence{}, err
	}
	return v.Capability(ctx, m, refMs)
}

func (s *pairRecordedEvidenceSource) Account(ctx context.Context, m pairContractMember, refMs int64) (pairAccountEvidence, error) {
	v, err := s.view(refMs)
	if err != nil {
		return pairAccountEvidence{}, err
	}
	return v.Account(ctx, m, refMs)
}

func (s *pairRecordedEvidenceSource) Conversions(ctx context.Context, m pairContractMember, refMs int64) ([]pairConversionEvidence, error) {
	v, err := s.view(refMs)
	if err != nil {
		return nil, err
	}
	return v.Conversions(ctx, m, refMs)
}

func (s *pairRecordedEvidenceSource) SettlementConversion(ctx context.Context, m pairContractMember, currency string, fillAtMs, settlementAtMs int64) (pairConversionEvidence, error) {
	v, err := s.view(settlementAtMs)
	if err != nil {
		return pairConversionEvidence{}, err
	}
	return v.SettlementConversion(ctx, m, currency, fillAtMs, settlementAtMs)
}

func (s *pairRecordedEvidenceSource) FundingConversion(m pairContractMember, currency string, eventMs, cutoffMs int64) (pairConversionEvidence, string, error) {
	v, err := s.view(cutoffMs)
	if err != nil {
		return pairConversionEvidence{}, pairFundingReasonConversionError, err
	}
	return v.FundingConversion(m, currency, eventMs, cutoffMs)
}

func (s *pairRecordedEvidenceSource) hasConversion(c pairConversionEvidence) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.doc.Conversions {
		if r.EvidenceHash == c.EvidenceHash && r.Key == c.Key && r.Currency == c.Currency {
			return true
		}
	}
	return false
}

func (s *pairRecordedEvidenceSource) document() pairEvidenceDocument {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := pairEvidenceDocument{V: pairEvidenceVersion, Capabilities: []pairCapabilityEvidence{}, Accounts: []pairAccountEvidence{}, Conversions: []pairConversionEvidence{}}
	for _, r := range s.doc.Capabilities {
		r.Meta = r.Meta.clone()
		d.Capabilities = append(d.Capabilities, r)
	}
	d.Accounts = append(d.Accounts, s.doc.Accounts...)
	d.Conversions = append(d.Conversions, s.doc.Conversions...)
	return d
}
