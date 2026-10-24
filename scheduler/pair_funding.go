package main

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	pairFundingDocumentVersion = 1
	pairFundingRecordVersion   = 1
	pairFundingBatchVersion    = 1
	pairFundingEventVersion    = 2
	pairFundingCoverageVersion = 1
	pairFundingBindingVersion  = 1

	pairFundingRuleHyperliquid = "hyperliquid_hourly_oracle_v1"
	pairFundingRuleOKX         = "okx_swap_realized_mark_v1"

	pairFundingPriceOracle = "oracle"
	pairFundingPriceMark   = "mark"

	pairFundingSourceFrozen   = "frozen"
	pairFundingSourceRecorded = "recorded"

	pairFundingMaxRecords   = 1 << 16
	pairFundingMaxIntervals = 1 << 12
	pairFundingMaxBatches   = 1 << 16
)

const (
	pairFundingReasonCoverageGap       = "coverage_gap"
	pairFundingReasonRuleUnsupported   = "rule_unsupported"
	pairFundingReasonRuleProvider      = "rule_provider_mismatch"
	pairFundingReasonPriceMissing      = "price_missing"
	pairFundingReasonConversionAtEvent = "conversion_invalid_at_event"
	pairFundingReasonConversionCutoff  = "conversion_unavailable_by_cutoff"
	pairFundingReasonConversionError   = "conversion_unreadable"
	pairFundingReasonInventory         = "inventory_incomplete"
	pairFundingReasonFillTime          = "fill_time_unknown"
	pairFundingReasonAssessment        = "assessment_window_fill"
	pairFundingReasonConflict          = "conflicting_evidence"
	pairFundingReasonArithmetic        = "amount_not_finite_decimal"
	pairFundingReasonCurrency          = "currency_mismatch"
)

type pairFundingRule struct {
	RuleVersion        string   `json:"rule_version"`
	Provider           string   `json:"provider"`
	Currency           string   `json:"currency"`
	RateField          string   `json:"rate_field"`
	PriceKind          string   `json:"price_kind"`
	Formula            string   `json:"formula"`
	SameTime           string   `json:"same_time"`
	AssessmentWindowMs int64    `json:"assessment_window_ms"`
	Schedule           string   `json:"schedule"`
	Rounding           string   `json:"rounding"`
	Evidence           []string `json:"evidence"`
}

var pairFundingRules = map[string]pairFundingRule{
	pairFundingRuleHyperliquid: {
		RuleVersion:        pairFundingRuleHyperliquid,
		Provider:           entryProviderHyperliquid,
		Currency:           pairHyperliquidCurrency,
		RateField:          "fundingHistory.fundingRate",
		PriceKind:          pairFundingPriceOracle,
		Formula:            "native_amount = -(signed_base_qty * native_mark * rate)",
		SameTime:           "inventory strictly before event_ms; a fill at event_ms is excluded",
		AssessmentWindowMs: 0,
		Schedule:           "each venue settlement record is one event; no fixed interval is assumed",
		Rounding:           "none: no venue rounding statement was found, so the exact decimal product is kept",
		Evidence: []string{
			"https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding: \"The funding rate on Hyperliquid is paid every hour.\"",
			"https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding: \"the funding payment at the end of the interval is position_size * oracle_price * funding_rate. In particular, the spot oracle price is used to convert the position size to notional value, not the mark price.\"",
			"https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding: \"the funding rate will be positive, and the long position will pay the short position.\"",
			"https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals: fundingHistory takes startTime and endTime in milliseconds, both inclusive, and returns coin, fundingRate, premium and time",
		},
	},
	pairFundingRuleOKX: {
		RuleVersion:        pairFundingRuleOKX,
		Provider:           entryProviderOKX,
		Currency:           pairOKXCurrency,
		RateField:          "funding-rate-history.realizedRate",
		PriceKind:          pairFundingPriceMark,
		Formula:            "native_amount = -(signed_base_qty * native_mark * rate); signed_base_qty = contracts * contract_size",
		SameTime:           "inventory strictly before event_ms; a fill whose possible time lies in [event_ms, event_ms + assessment_window_ms) holds the event as incomplete",
		AssessmentWindowMs: 60000,
		Schedule:           "each venue settlement record is one event; OKX settles every 8 hours by default and may use 1, 2 or 4 hours, so no fixed interval is assumed",
		Rounding:           "none: no venue rounding statement was found, so the exact decimal product is kept",
		Evidence: []string{
			"https://www.okx.com/help/iv-introduction-to-perpetual-swap-funding-fee: \"Funding fee = Position value x Funding rate\" and \"Position value = Number of contracts x Contract size x Contract multiplier x Mark price\" (USDT-margined)",
			"https://www.okx.com/help/iv-introduction-to-perpetual-swap-funding-fee: \"When the funding rate is positive, traders with long positions pay a funding fee to traders with short positions.\"",
			"https://www.okx.com/help/iv-introduction-to-perpetual-swap-funding-fee: \"every 8 hours (00:00, 08:00, and 16:00 UTC) by default, unless specified otherwise (i.e. every 1, 2 or 4 hours)\" and \"The actual fee assessment may take up to a minute.\"",
			"https://www.okx.com/docs-v5/en/#public-data-rest-api-get-funding-rate-history: realizedRate is \"Actual funding rate\", fundingRate is \"Predicted funding rate\", fundingTime is \"Settlement time\"",
		},
	},
}

var pairFundingRulesMu sync.RWMutex

func pairFundingRuleFor(version string) (pairFundingRule, bool) {
	pairFundingRulesMu.RLock()
	defer pairFundingRulesMu.RUnlock()
	r, ok := pairFundingRules[version]
	return r, ok
}

func pairFundingRuleVersionFor(provider string) (string, bool) {
	pairFundingRulesMu.RLock()
	defer pairFundingRulesMu.RUnlock()
	var found []string
	for v, r := range pairFundingRules {
		if r.Provider == provider {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

func pairFundingRuleTable() ([]pairFundingRule, error) {
	pairFundingRulesMu.RLock()
	defer pairFundingRulesMu.RUnlock()
	out := make([]pairFundingRule, 0, len(pairFundingRules))
	providers := map[string]string{}
	for v, r := range pairFundingRules {
		if r.RuleVersion != v {
			return nil, fmt.Errorf("funding rule table entry %s names rule %s", v, r.RuleVersion)
		}
		if prev, ok := providers[r.Provider]; ok {
			return nil, fmt.Errorf("funding rules %s and %s both serve provider %s; one rule per venue", prev, v, r.Provider)
		}
		providers[r.Provider] = v
		r.Evidence = append([]string{}, r.Evidence...)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleVersion < out[j].RuleVersion })
	return out, nil
}

func pairFundingRulesDigest() (string, []string, error) {
	table, err := pairFundingRuleTable()
	if err != nil {
		return "", nil, err
	}
	b, err := pairCanonicalJSON(table)
	if err != nil {
		return "", nil, err
	}
	versions := make([]string, 0, len(table))
	for _, r := range table {
		versions = append(versions, r.RuleVersion)
	}
	return pairSHA256(b), versions, nil
}

func pairFundingEventID(provider, instrument string, eventMs int64) string {
	return provider + ":" + instrument + ":" + strconv.FormatInt(eventMs, 10)
}

type pairFundingRate struct {
	V           int             `json:"v"`
	Key         pairEvidenceKey `json:"key"`
	EventID     string          `json:"event_id"`
	RuleVersion string          `json:"rule_version"`
	EventMs     int64           `json:"event_ms"`
	VisibleMs   int64           `json:"visible_ms"`
	Rate        pairDecimal     `json:"rate"`
	Provenance  string          `json:"provenance"`
	RecordHash  string          `json:"record_hash"`
}

type pairFundingPrice struct {
	V          int             `json:"v"`
	Key        pairEvidenceKey `json:"key"`
	EvidenceID string          `json:"evidence_id"`
	PriceKind  string          `json:"price_kind"`
	AtMs       int64           `json:"at_ms"`
	VisibleMs  int64           `json:"visible_ms"`
	Price      pairDecimal     `json:"price"`
	Provenance string          `json:"provenance"`
	RecordHash string          `json:"record_hash"`
}

type pairFundingInterval struct {
	FromMs    int64 `json:"from_ms"`
	ThroughMs int64 `json:"through_ms"`
}

type pairFundingCoverageRecord struct {
	V                  int                   `json:"v"`
	Key                pairEvidenceKey       `json:"key"`
	EvidenceID         string                `json:"evidence_id"`
	RequestedFromMs    int64                 `json:"requested_from_ms"`
	RequestedThroughMs int64                 `json:"requested_through_ms"`
	Proven             []pairFundingInterval `json:"proven"`
	Gaps               []pairCoverageGap     `json:"gaps"`
	VisibleMs          int64                 `json:"visible_ms"`
	Provenance         string                `json:"provenance"`
	RecordHash         string                `json:"record_hash"`
}

type pairFundingDocument struct {
	V        int                         `json:"v"`
	SourceID string                      `json:"source_id"`
	Rates    []pairFundingRate           `json:"rates"`
	Prices   []pairFundingPrice          `json:"prices"`
	Coverage []pairFundingCoverageRecord `json:"coverage"`
}

type pairFundingBatch struct {
	V         int                         `json:"v"`
	SourceID  string                      `json:"source_id"`
	Seq       int64                       `json:"seq"`
	PrevHash  string                      `json:"prev_hash"`
	Rates     []pairFundingRate           `json:"rates"`
	Prices    []pairFundingPrice          `json:"prices"`
	Coverage  []pairFundingCoverageRecord `json:"coverage"`
	BatchHash string                      `json:"batch_hash"`
}

type pairFundingCoverage struct {
	V                  int                   `json:"v"`
	Key                pairEvidenceKey       `json:"key"`
	RequestedFromMs    int64                 `json:"requested_from_ms"`
	RequestedThroughMs int64                 `json:"requested_through_ms"`
	Proven             []pairFundingInterval `json:"proven"`
	Gaps               []pairCoverageGap     `json:"gaps"`
	InventoryThroughMs int64                 `json:"inventory_through_ms"`
	SourceKind         string                `json:"source_kind"`
	SourceID           string                `json:"source_id"`
	CutoffMs           int64                 `json:"cutoff_ms"`
}

type pairFundingEvent struct {
	V             int                    `json:"v"`
	Key           pairEvidenceKey        `json:"key"`
	EventID       string                 `json:"event_id"`
	RuleVersion   string                 `json:"rule_version"`
	EventMs       int64                  `json:"event_ms"`
	VisibleMs     int64                  `json:"visible_ms"`
	SignedBaseQty pairDecimal            `json:"signed_base_qty"`
	NativeMark    pairDecimal            `json:"native_mark"`
	Rate          pairDecimal            `json:"rate"`
	Currency      string                 `json:"currency"`
	NativeAmount  pairDecimal            `json:"native_amount"`
	USDAmount     pairDecimal            `json:"usd_amount"`
	Conversion    pairConversionEvidence `json:"conversion"`
	Coverage      pairFundingCoverage    `json:"coverage"`
	SourceHash    string                 `json:"source_hash"`
	PriceHash     string                 `json:"price_hash"`
	EvidenceHash  string                 `json:"evidence_hash"`
}

type pairFundingBinding struct {
	V                   int               `json:"v"`
	ContractFingerprint string            `json:"contract_fingerprint"`
	Members             []pairEvidenceKey `json:"members"`
	SourceKind          string            `json:"source_kind"`
	SourceID            string            `json:"source_id"`
	Digest              string            `json:"digest"`
	AnchorMs            int64             `json:"anchor_ms"`
	RulesDigest         string            `json:"rules_digest"`
	Rules               []string          `json:"rules"`
}

func (r pairFundingRate) computeHash() (string, error) {
	r.RecordHash = ""
	return pairRecordHashOf(r)
}

func (p pairFundingPrice) computeHash() (string, error) {
	p.RecordHash = ""
	return pairRecordHashOf(p)
}

func (c pairFundingCoverageRecord) computeHash() (string, error) {
	c.RecordHash = ""
	c.Proven = pairFundingIntervalsOrEmpty(c.Proven)
	c.Gaps = pairFundingGapsOrEmpty(c.Gaps)
	return pairRecordHashOf(c)
}

func (r pairFundingRate) sealed() (pairFundingRate, error) {
	h, err := r.computeHash()
	r.RecordHash = h
	return r, err
}

func (p pairFundingPrice) sealed() (pairFundingPrice, error) {
	h, err := p.computeHash()
	p.RecordHash = h
	return p, err
}

func (c pairFundingCoverageRecord) sealed() (pairFundingCoverageRecord, error) {
	c.Proven = pairFundingIntervalsOrEmpty(c.Proven)
	c.Gaps = pairFundingGapsOrEmpty(c.Gaps)
	h, err := c.computeHash()
	c.RecordHash = h
	return c, err
}

func pairFundingSourceHash(r pairFundingRate) (string, error) {
	body := struct {
		Key         pairEvidenceKey `json:"key"`
		EventID     string          `json:"event_id"`
		RuleVersion string          `json:"rule_version"`
		EventMs     int64           `json:"event_ms"`
		Rate        pairDecimal     `json:"rate"`
	}{r.Key, r.EventID, r.RuleVersion, r.EventMs, r.Rate}
	return pairRecordHashOf(body)
}

func (e pairFundingEvent) computeHash() (string, error) {
	e.EvidenceHash = ""
	e.Coverage.Proven = pairFundingIntervalsOrEmpty(e.Coverage.Proven)
	e.Coverage.Gaps = pairFundingGapsOrEmpty(e.Coverage.Gaps)
	return pairRecordHashOf(e)
}

func (e pairFundingEvent) sealed() (pairFundingEvent, error) {
	e.Coverage.Proven = pairFundingIntervalsOrEmpty(e.Coverage.Proven)
	e.Coverage.Gaps = pairFundingGapsOrEmpty(e.Coverage.Gaps)
	h, err := e.computeHash()
	e.EvidenceHash = h
	return e, err
}

func pairFundingIntervalsOrEmpty(in []pairFundingInterval) []pairFundingInterval {
	if in == nil {
		return []pairFundingInterval{}
	}
	return in
}

func pairFundingGapsOrEmpty(in []pairCoverageGap) []pairCoverageGap {
	if in == nil {
		return []pairCoverageGap{}
	}
	return in
}

func pairFundingCheckKey(k pairEvidenceKey, what string) error {
	if _, err := k.memberKey(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func pairFundingValidateRate(r pairFundingRate) error {
	what := "funding rate " + r.EventID
	if r.V != pairFundingRecordVersion {
		return fmt.Errorf("%s version %d is not supported (want %d)", what, r.V, pairFundingRecordVersion)
	}
	if err := pairFundingCheckKey(r.Key, what); err != nil {
		return err
	}
	if r.EventMs <= 0 || r.VisibleMs < r.EventMs {
		return fmt.Errorf("%s needs event_ms above zero and visible_ms at or after it (event %d, visible %d)", what, r.EventMs, r.VisibleMs)
	}
	if want := pairFundingEventID(r.Key.Provider, r.Key.Instrument, r.EventMs); r.EventID != want {
		return fmt.Errorf("%s event_id must be the venue identity %s", what, want)
	}
	if !pairPrintableASCII(r.RuleVersion, pairEvidenceMaxIDLen) {
		return fmt.Errorf("%s rule_version must be 1..%d printable ASCII characters", what, pairEvidenceMaxIDLen)
	}
	if _, err := pairDecimalRat("rate", r.Rate); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !pairPrintableASCII(r.Provenance, pairEvidenceMaxProvenanceLen) {
		return fmt.Errorf("%s provenance must be 1..%d printable ASCII characters", what, pairEvidenceMaxProvenanceLen)
	}
	h, err := r.computeHash()
	if err != nil {
		return err
	}
	if !pairHexDigestPattern.MatchString(r.RecordHash) || r.RecordHash != h {
		return fmt.Errorf("%s record_hash does not match its canonical bytes", what)
	}
	return nil
}

func pairFundingValidatePrice(p pairFundingPrice) error {
	what := "funding price " + p.EvidenceID
	if p.V != pairFundingRecordVersion {
		return fmt.Errorf("%s version %d is not supported (want %d)", what, p.V, pairFundingRecordVersion)
	}
	if err := pairFundingCheckKey(p.Key, what); err != nil {
		return err
	}
	if !pairPrintableASCII(p.EvidenceID, pairEvidenceMaxIDLen) {
		return fmt.Errorf("funding price evidence_id %q must be 1..%d printable ASCII characters", p.EvidenceID, pairEvidenceMaxIDLen)
	}
	if p.PriceKind != pairFundingPriceMark && p.PriceKind != pairFundingPriceOracle {
		return fmt.Errorf("%s price_kind %q is not %s or %s", what, p.PriceKind, pairFundingPriceMark, pairFundingPriceOracle)
	}
	if p.AtMs <= 0 || p.VisibleMs < p.AtMs {
		return fmt.Errorf("%s needs at_ms above zero and visible_ms at or after it (at %d, visible %d)", what, p.AtMs, p.VisibleMs)
	}
	if _, err := pairDecimalPositive("price", p.Price); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !pairPrintableASCII(p.Provenance, pairEvidenceMaxProvenanceLen) {
		return fmt.Errorf("%s provenance must be 1..%d printable ASCII characters", what, pairEvidenceMaxProvenanceLen)
	}
	h, err := p.computeHash()
	if err != nil {
		return err
	}
	if !pairHexDigestPattern.MatchString(p.RecordHash) || p.RecordHash != h {
		return fmt.Errorf("%s record_hash does not match its canonical bytes", what)
	}
	return nil
}

func pairFundingIntervalsValid(in []pairFundingInterval, from, through int64) error {
	if len(in) > pairFundingMaxIntervals {
		return fmt.Errorf("%d intervals exceed the %d bound", len(in), pairFundingMaxIntervals)
	}
	prev := from
	for i, iv := range in {
		if iv.FromMs >= iv.ThroughMs {
			return fmt.Errorf("interval %d (%d, %d] is empty", i, iv.FromMs, iv.ThroughMs)
		}
		if iv.FromMs < prev || iv.ThroughMs > through {
			return fmt.Errorf("interval %d (%d, %d] is out of order or outside (%d, %d]", i, iv.FromMs, iv.ThroughMs, from, through)
		}
		prev = iv.ThroughMs
	}
	return nil
}

func pairFundingValidateCoverage(c pairFundingCoverageRecord) error {
	what := "funding coverage " + c.EvidenceID
	if c.V != pairFundingRecordVersion {
		return fmt.Errorf("%s version %d is not supported (want %d)", what, c.V, pairFundingRecordVersion)
	}
	if err := pairFundingCheckKey(c.Key, what); err != nil {
		return err
	}
	if !pairPrintableASCII(c.EvidenceID, pairEvidenceMaxIDLen) {
		return fmt.Errorf("funding coverage evidence_id %q must be 1..%d printable ASCII characters", c.EvidenceID, pairEvidenceMaxIDLen)
	}
	if c.RequestedFromMs < 0 || c.RequestedFromMs >= c.RequestedThroughMs {
		return fmt.Errorf("%s requested window (%d, %d] is empty or negative", what, c.RequestedFromMs, c.RequestedThroughMs)
	}
	if c.VisibleMs < 0 {
		return fmt.Errorf("%s visible_ms is negative", what)
	}
	if err := pairFundingIntervalsValid(c.Proven, c.RequestedFromMs, c.RequestedThroughMs); err != nil {
		return fmt.Errorf("%s proven: %v", what, err)
	}
	gaps := make([]pairFundingInterval, 0, len(c.Gaps))
	for _, g := range c.Gaps {
		if !pairPrintableASCII(g.Reason, pairEvidenceMaxIDLen) {
			return fmt.Errorf("%s gap reason must be 1..%d printable ASCII characters", what, pairEvidenceMaxIDLen)
		}
		gaps = append(gaps, pairFundingInterval{FromMs: g.FromMs, ThroughMs: g.ThroughMs})
	}
	if err := pairFundingIntervalsValid(gaps, c.RequestedFromMs, c.RequestedThroughMs); err != nil {
		return fmt.Errorf("%s gaps: %v", what, err)
	}
	if len(pairFundingIntersect(pairFundingNormalize(c.Proven), pairFundingNormalize(gaps))) > 0 {
		return fmt.Errorf("%s declares an interval both proven and a gap", what)
	}
	if !pairPrintableASCII(c.Provenance, pairEvidenceMaxProvenanceLen) {
		return fmt.Errorf("%s provenance must be 1..%d printable ASCII characters", what, pairEvidenceMaxProvenanceLen)
	}
	h, err := c.computeHash()
	if err != nil {
		return err
	}
	if !pairHexDigestPattern.MatchString(c.RecordHash) || c.RecordHash != h {
		return fmt.Errorf("%s record_hash does not match its canonical bytes", what)
	}
	return nil
}

func pairFundingSortRates(rs []pairFundingRate) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		if a.EventMs != b.EventMs {
			return a.EventMs < b.EventMs
		}
		if a.RuleVersion != b.RuleVersion {
			return a.RuleVersion < b.RuleVersion
		}
		return a.VisibleMs < b.VisibleMs
	})
}

func pairFundingSortPrices(ps []pairFundingPrice) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		if a.PriceKind != b.PriceKind {
			return a.PriceKind < b.PriceKind
		}
		if a.VisibleMs != b.VisibleMs {
			return a.VisibleMs < b.VisibleMs
		}
		return a.EvidenceID < b.EvidenceID
	})
}

func pairFundingSortCoverage(cs []pairFundingCoverageRecord) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		if a.VisibleMs != b.VisibleMs {
			return a.VisibleMs < b.VisibleMs
		}
		return a.EvidenceID < b.EvidenceID
	})
}

func pairFundingNormalizeLists(rates []pairFundingRate, prices []pairFundingPrice, cov []pairFundingCoverageRecord) ([]pairFundingRate, []pairFundingPrice, []pairFundingCoverageRecord) {
	r := append([]pairFundingRate{}, rates...)
	p := append([]pairFundingPrice{}, prices...)
	c := append([]pairFundingCoverageRecord{}, cov...)
	for i := range c {
		c[i].Proven = pairFundingIntervalsOrEmpty(append([]pairFundingInterval(nil), c[i].Proven...))
		c[i].Gaps = pairFundingGapsOrEmpty(append([]pairCoverageGap(nil), c[i].Gaps...))
	}
	pairFundingSortRates(r)
	pairFundingSortPrices(p)
	pairFundingSortCoverage(c)
	return r, p, c
}

func pairFundingDocumentBytes(d pairFundingDocument) ([]byte, error) {
	out := d
	out.Rates, out.Prices, out.Coverage = pairFundingNormalizeLists(d.Rates, d.Prices, d.Coverage)
	return pairCanonicalJSON(out)
}

func (b pairFundingBatch) computeHash() (string, error) {
	b.BatchHash = ""
	b.Rates, b.Prices, b.Coverage = pairFundingNormalizeLists(b.Rates, b.Prices, b.Coverage)
	return pairRecordHashOf(b)
}

func (b pairFundingBatch) sealed() (pairFundingBatch, error) {
	b.Rates, b.Prices, b.Coverage = pairFundingNormalizeLists(b.Rates, b.Prices, b.Coverage)
	h, err := b.computeHash()
	b.BatchHash = h
	return b, err
}

func pairFundingBatchBytes(b pairFundingBatch) ([]byte, error) {
	b.Rates, b.Prices, b.Coverage = pairFundingNormalizeLists(b.Rates, b.Prices, b.Coverage)
	return pairCanonicalJSON(b)
}

type pairFundingRecordSet struct {
	Rates    []pairFundingRate
	Prices   []pairFundingPrice
	Coverage []pairFundingCoverageRecord
}

func pairFundingIdentity(k pairEvidenceKey, eventID, rule string) string {
	return k.String() + "|" + eventID + "|" + rule
}

func pairFundingCheckRecords(set pairFundingRecordSet, prior map[string]string) error {
	if len(set.Rates) > pairFundingMaxRecords || len(set.Prices) > pairFundingMaxRecords || len(set.Coverage) > pairFundingMaxRecords {
		return fmt.Errorf("funding input holds more than %d records of one kind", pairFundingMaxRecords)
	}
	seen := map[string]string{}
	for k, v := range prior {
		seen[k] = v
	}
	fresh := map[string]bool{}
	for _, r := range set.Rates {
		if err := pairFundingValidateRate(r); err != nil {
			return err
		}
		sh, err := pairFundingSourceHash(r)
		if err != nil {
			return err
		}
		id := pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)
		if fresh[id] {
			return fmt.Errorf("funding rate %s for %s appears twice in one input", r.EventID, r.Key)
		}
		fresh[id] = true
		if prev, ok := seen[id]; ok && prev != sh {
			return fmt.Errorf("funding rate %s for %s conflicts with earlier evidence for the same event identity (source hash %s, earlier %s)", r.EventID, r.Key, shortFingerprint(sh), shortFingerprint(prev))
		}
		seen[id] = sh
	}
	priceSeen := map[string]bool{}
	for _, p := range set.Prices {
		if err := pairFundingValidatePrice(p); err != nil {
			return err
		}
		id := p.Key.String() + "|" + p.EvidenceID
		if priceSeen[id] {
			return fmt.Errorf("funding price %s for %s appears twice", p.EvidenceID, p.Key)
		}
		priceSeen[id] = true
	}
	covSeen := map[string]bool{}
	for _, c := range set.Coverage {
		if err := pairFundingValidateCoverage(c); err != nil {
			return err
		}
		id := c.Key.String() + "|" + c.EvidenceID
		if covSeen[id] {
			return fmt.Errorf("funding coverage %s for %s appears twice", c.EvidenceID, c.Key)
		}
		covSeen[id] = true
		for _, r := range set.Rates {
			if r.Key != c.Key || r.VisibleMs <= c.VisibleMs {
				continue
			}
			for _, iv := range c.Proven {
				if iv.FromMs < r.EventMs && r.EventMs <= iv.ThroughMs {
					return fmt.Errorf("funding coverage %s proves (%d, %d] complete at %d, but rate %s inside it is first visible at %d", c.EvidenceID, iv.FromMs, iv.ThroughMs, c.VisibleMs, r.EventID, r.VisibleMs)
				}
			}
		}
	}
	return nil
}

func pairFundingStrictDecode(b []byte, out interface{}, what string, maxBytes int) error {
	if len(b) > maxBytes {
		return fmt.Errorf("%s is %d bytes, above the %d byte limit", what, len(b), maxBytes)
	}
	tree, err := pairStrictParse(b)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return pairStrictDecodeTree(tree, b, out, what)
}

func pairDecodeFundingDocument(b []byte) (pairFundingDocument, error) {
	var d pairFundingDocument
	if err := pairFundingStrictDecode(b, &d, "funding document", pairEvidenceMaxDocumentBytes); err != nil {
		return d, err
	}
	if d.V != pairFundingDocumentVersion {
		return d, fmt.Errorf("funding document version %d is not supported (want %d)", d.V, pairFundingDocumentVersion)
	}
	if !pairPrintableASCII(d.SourceID, pairEvidenceMaxIDLen) {
		return d, fmt.Errorf("funding document source_id must be 1..%d printable ASCII characters", pairEvidenceMaxIDLen)
	}
	canon, err := pairFundingDocumentBytes(d)
	if err != nil {
		return d, err
	}
	if !bytes.Equal(canon, b) {
		return d, fmt.Errorf("funding document bytes are not canonical (compact sorted keys, canonical decimals, records sorted by key and time)")
	}
	if err := pairFundingCheckRecords(pairFundingRecordSet{Rates: d.Rates, Prices: d.Prices, Coverage: d.Coverage}, nil); err != nil {
		return d, err
	}
	return d, nil
}

func pairDecodeFundingBatch(b []byte) (pairFundingBatch, error) {
	var x pairFundingBatch
	if err := pairFundingStrictDecode(b, &x, "funding batch", pairEvidenceMaxDocumentBytes); err != nil {
		return x, err
	}
	if x.V != pairFundingBatchVersion {
		return x, fmt.Errorf("funding batch version %d is not supported (want %d)", x.V, pairFundingBatchVersion)
	}
	if !pairPrintableASCII(x.SourceID, pairEvidenceMaxIDLen) {
		return x, fmt.Errorf("funding batch source_id must be 1..%d printable ASCII characters", pairEvidenceMaxIDLen)
	}
	if x.Seq < 1 {
		return x, fmt.Errorf("funding batch seq %d must be at least 1", x.Seq)
	}
	if (x.Seq == 1) != (x.PrevHash == "") || (x.PrevHash != "" && !pairHexDigestPattern.MatchString(x.PrevHash)) {
		return x, fmt.Errorf("funding batch %d prev_hash must be empty for the first batch and a lowercase SHA-256 otherwise", x.Seq)
	}
	canon, err := pairFundingBatchBytes(x)
	if err != nil {
		return x, err
	}
	if !bytes.Equal(canon, b) {
		return x, fmt.Errorf("funding batch %d bytes are not canonical", x.Seq)
	}
	h, err := x.computeHash()
	if err != nil {
		return x, err
	}
	if !pairHexDigestPattern.MatchString(x.BatchHash) || x.BatchHash != h {
		return x, fmt.Errorf("funding batch %d batch_hash does not match its canonical bytes", x.Seq)
	}
	return x, nil
}

func pairDecodeFundingEvent(b []byte) (pairFundingEvent, error) {
	var e pairFundingEvent
	if err := pairFundingStrictDecode(b, &e, "funding event", pairEvidenceMaxDocumentBytes); err != nil {
		return e, err
	}
	canon, err := pairCanonicalJSON(e)
	if err != nil {
		return e, err
	}
	if !bytes.Equal(canon, b) {
		return e, fmt.Errorf("funding event bytes are not canonical")
	}
	if err := pairFundingValidateEvent(e); err != nil {
		return e, err
	}
	return e, nil
}

func pairFundingValidateEvent(e pairFundingEvent) error {
	what := "funding event " + e.EventID
	if e.V != pairFundingEventVersion {
		return fmt.Errorf("%s version %d is not supported (want %d)", what, e.V, pairFundingEventVersion)
	}
	if err := pairFundingCheckKey(e.Key, what); err != nil {
		return err
	}
	if want := pairFundingEventID(e.Key.Provider, e.Key.Instrument, e.EventMs); e.EventID != want || e.EventMs <= 0 {
		return fmt.Errorf("%s event_id must be the venue identity %s", what, want)
	}
	if e.VisibleMs < e.EventMs {
		return fmt.Errorf("%s visible_ms %d is before event_ms %d", what, e.VisibleMs, e.EventMs)
	}
	rule, ok := pairFundingRuleFor(e.RuleVersion)
	if !ok || rule.Provider != e.Key.Provider {
		return fmt.Errorf("%s rule %q is not the supported rule for %s", what, e.RuleVersion, e.Key.Provider)
	}
	if e.Currency != rule.Currency || e.Conversion.Currency != e.Currency {
		return fmt.Errorf("%s currency %s and conversion %s must both be the rule currency %s", what, e.Currency, e.Conversion.Currency, rule.Currency)
	}
	qty, err := pairDecimalRat("signed_base_qty", e.SignedBaseQty)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if qty.Sign() == 0 {
		return fmt.Errorf("%s signed_base_qty is zero; a flat member owes no funding event", what)
	}
	px, err := pairDecimalPositive("native_mark", e.NativeMark)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	rate, err := pairDecimalRat("rate", e.Rate)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	conv, err := pairDecimalPositive("conversion.rate_usd", e.Conversion.RateUSD)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	native, usd, err := pairFundingAmounts(qty, px, rate, conv)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if string(e.NativeAmount) != native || string(e.USDAmount) != usd {
		return fmt.Errorf("%s amounts %s/%s are not the rule product %s/%s", what, e.NativeAmount, e.USDAmount, native, usd)
	}
	ch, err := e.Conversion.computeHash()
	if err != nil {
		return err
	}
	if err := pairValidateStamp(e.Conversion.pairEvidenceStamp, ch, "funding event conversion"); err != nil {
		return err
	}
	if e.Conversion.Key != e.Key {
		return fmt.Errorf("%s conversion key %s is not the member key", what, e.Conversion.Key)
	}
	if !(e.Conversion.ValidFromMs <= e.EventMs && e.EventMs <= e.Conversion.ValidThroughMs) {
		return fmt.Errorf("%s conversion %s is not valid at event time %d", what, e.Conversion.EvidenceID, e.EventMs)
	}
	cov := e.Coverage
	if cov.V != pairFundingCoverageVersion || cov.Key != e.Key || cov.RequestedThroughMs != e.EventMs || cov.RequestedFromMs >= cov.RequestedThroughMs {
		return fmt.Errorf("%s coverage must be version %d for the member key with a requested window ending at the event", what, pairFundingCoverageVersion)
	}
	if len(cov.Gaps) != 0 {
		return fmt.Errorf("%s coverage still holds gaps; an applied event needs complete coverage", what)
	}
	want := []pairFundingInterval{{FromMs: cov.RequestedFromMs, ThroughMs: cov.RequestedThroughMs}}
	if len(pairFundingSubtract(want, pairFundingNormalize(cov.Proven))) != 0 {
		return fmt.Errorf("%s proven coverage does not span the requested window", what)
	}
	if cov.InventoryThroughMs < e.EventMs+rule.AssessmentWindowMs-1 || cov.InventoryThroughMs < e.EventMs {
		return fmt.Errorf("%s inventory is complete only through %d, short of the event and its assessment window", what, cov.InventoryThroughMs)
	}
	if cov.CutoffMs < e.VisibleMs {
		return fmt.Errorf("%s cutoff %d is before its inputs were visible at %d", what, cov.CutoffMs, e.VisibleMs)
	}
	if !pairHexDigestPattern.MatchString(e.SourceHash) || !pairHexDigestPattern.MatchString(e.PriceHash) {
		return fmt.Errorf("%s needs its source and price record hashes", what)
	}
	h, err := e.computeHash()
	if err != nil {
		return err
	}
	if !pairHexDigestPattern.MatchString(e.EvidenceHash) || e.EvidenceHash != h {
		return fmt.Errorf("%s evidence_hash does not match its canonical bytes", what)
	}
	return nil
}

func pairFundingAmounts(qty, px, rate, conv *big.Rat) (string, string, error) {
	native := new(big.Rat).Mul(qty, px)
	native.Mul(native, rate)
	native.Neg(native)
	usd := new(big.Rat).Mul(native, conv)
	n, err := pairRatDecimal(native)
	if err != nil {
		return "", "", err
	}
	u, err := pairRatDecimal(usd)
	if err != nil {
		return "", "", err
	}
	return n, u, nil
}

func pairFundingNormalize(in []pairFundingInterval) []pairFundingInterval {
	var xs []pairFundingInterval
	for _, iv := range in {
		if iv.FromMs < iv.ThroughMs {
			xs = append(xs, iv)
		}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].FromMs < xs[j].FromMs })
	var out []pairFundingInterval
	for _, iv := range xs {
		if n := len(out); n > 0 && iv.FromMs <= out[n-1].ThroughMs {
			if iv.ThroughMs > out[n-1].ThroughMs {
				out[n-1].ThroughMs = iv.ThroughMs
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func pairFundingIntersect(a, b []pairFundingInterval) []pairFundingInterval {
	var out []pairFundingInterval
	for _, x := range a {
		for _, y := range b {
			lo, hi := x.FromMs, x.ThroughMs
			if y.FromMs > lo {
				lo = y.FromMs
			}
			if y.ThroughMs < hi {
				hi = y.ThroughMs
			}
			if lo < hi {
				out = append(out, pairFundingInterval{FromMs: lo, ThroughMs: hi})
			}
		}
	}
	return pairFundingNormalize(out)
}

func pairFundingSubtract(a, b []pairFundingInterval) []pairFundingInterval {
	cur := pairFundingNormalize(a)
	for _, y := range pairFundingNormalize(b) {
		var next []pairFundingInterval
		for _, x := range cur {
			if y.ThroughMs <= x.FromMs || y.FromMs >= x.ThroughMs {
				next = append(next, x)
				continue
			}
			if y.FromMs > x.FromMs {
				next = append(next, pairFundingInterval{FromMs: x.FromMs, ThroughMs: y.FromMs})
			}
			if y.ThroughMs < x.ThroughMs {
				next = append(next, pairFundingInterval{FromMs: y.ThroughMs, ThroughMs: x.ThroughMs})
			}
		}
		cur = next
	}
	return cur
}

type pairFundingRequest struct {
	Key       pairEvidenceKey
	FromMs    int64
	ThroughMs int64
	CutoffMs  int64
}

type pairFundingCollected struct {
	Request    pairFundingRequest
	SourceKind string
	SourceID   string
	Rates      []pairFundingRate
	Prices     []pairFundingPrice
	Coverage   []pairFundingCoverageRecord
}

type pairFundingSource interface {
	Kind() string
	SourceID() string
	Collect(ctx context.Context, req pairFundingRequest) (pairFundingCollected, error)
	NextVisibleMs(key pairEvidenceKey, afterMs int64) (int64, bool)
}

func pairFundingFilter(set pairFundingRecordSet, req pairFundingRequest) pairFundingCollected {
	out := pairFundingCollected{Request: req, Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Coverage: []pairFundingCoverageRecord{}}
	for _, r := range set.Rates {
		if r.Key == req.Key && r.VisibleMs <= req.CutoffMs && r.EventMs > req.FromMs && r.EventMs <= req.ThroughMs {
			out.Rates = append(out.Rates, r)
		}
	}
	for _, p := range set.Prices {
		if p.Key == req.Key && p.VisibleMs <= req.CutoffMs && p.AtMs > req.FromMs && p.AtMs <= req.ThroughMs {
			out.Prices = append(out.Prices, p)
		}
	}
	for _, c := range set.Coverage {
		if c.Key != req.Key || c.VisibleMs > req.CutoffMs || c.RequestedThroughMs <= req.FromMs || c.RequestedFromMs >= req.ThroughMs {
			continue
		}
		cc := c
		cc.Proven = append([]pairFundingInterval{}, c.Proven...)
		cc.Gaps = append([]pairCoverageGap{}, c.Gaps...)
		out.Coverage = append(out.Coverage, cc)
	}
	pairFundingSortRates(out.Rates)
	pairFundingSortPrices(out.Prices)
	pairFundingSortCoverage(out.Coverage)
	return out
}

func pairFundingNextVisible(set pairFundingRecordSet, key pairEvidenceKey, afterMs int64) (int64, bool) {
	best, found := int64(0), false
	consider := func(k pairEvidenceKey, v int64) {
		if k == key && v > afterMs && (!found || v < best) {
			best, found = v, true
		}
	}
	for _, r := range set.Rates {
		consider(r.Key, r.VisibleMs)
	}
	for _, p := range set.Prices {
		consider(p.Key, p.VisibleMs)
	}
	for _, c := range set.Coverage {
		consider(c.Key, c.VisibleMs)
	}
	return best, found
}

func pairFundingRequestCheck(ctx context.Context, req pairFundingRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := req.Key.memberKey(); err != nil {
		return err
	}
	if req.FromMs < 0 || req.FromMs >= req.ThroughMs || req.CutoffMs <= 0 {
		return fmt.Errorf("funding request window (%d, %d] at cutoff %d is invalid", req.FromMs, req.ThroughMs, req.CutoffMs)
	}
	return nil
}

type pairFrozenFundingSource struct {
	digest  string
	doc     pairFundingDocument
	set     pairFundingRecordSet
	bound   bool
	boundFP string
}

func newPairFrozenFundingSource(doc []byte, wantDigest string) (*pairFrozenFundingSource, error) {
	if !pairHexDigestPattern.MatchString(wantDigest) {
		return nil, fmt.Errorf("pair funding needs an expected lowercase SHA-256 document digest, got %q", wantDigest)
	}
	if got := pairSHA256(doc); got != wantDigest {
		return nil, fmt.Errorf("pair funding document digest %s is not the expected %s", shortFingerprint(got), shortFingerprint(wantDigest))
	}
	d, err := pairDecodeFundingDocument(doc)
	if err != nil {
		return nil, err
	}
	return &pairFrozenFundingSource{digest: wantDigest, doc: d, set: pairFundingRecordSet{Rates: d.Rates, Prices: d.Prices, Coverage: d.Coverage}}, nil
}

func (s *pairFrozenFundingSource) Kind() string     { return pairFundingSourceFrozen }
func (s *pairFrozenFundingSource) SourceID() string { return s.doc.SourceID }
func (s *pairFrozenFundingSource) Digest() string   { return s.digest }

func (s *pairFrozenFundingSource) Collect(ctx context.Context, req pairFundingRequest) (pairFundingCollected, error) {
	if err := pairFundingRequestCheck(ctx, req); err != nil {
		return pairFundingCollected{}, err
	}
	out := pairFundingFilter(s.set, req)
	out.SourceKind, out.SourceID = pairFundingSourceFrozen, s.doc.SourceID
	return out, nil
}

func (s *pairFrozenFundingSource) NextVisibleMs(key pairEvidenceKey, afterMs int64) (int64, bool) {
	return pairFundingNextVisible(s.set, key, afterMs)
}

type pairRecordedFundingSource struct {
	mu       sync.RWMutex
	sourceID string
	anchorMs int64
	batches  []pairFundingBatch
	raw      [][]byte
	set      pairFundingRecordSet
	ids      map[string]string
}

func newPairRecordedFundingSource(sourceID string, anchorMs int64) (*pairRecordedFundingSource, error) {
	if !pairPrintableASCII(sourceID, pairEvidenceMaxIDLen) {
		return nil, fmt.Errorf("recorded funding source id must be 1..%d printable ASCII characters", pairEvidenceMaxIDLen)
	}
	if anchorMs <= 0 {
		return nil, fmt.Errorf("recorded funding source needs a positive anchor time, got %d", anchorMs)
	}
	return &pairRecordedFundingSource{sourceID: sourceID, anchorMs: anchorMs, ids: map[string]string{}}, nil
}

func (s *pairRecordedFundingSource) Kind() string     { return pairFundingSourceRecorded }
func (s *pairRecordedFundingSource) SourceID() string { return s.sourceID }
func (s *pairRecordedFundingSource) AnchorMs() int64  { return s.anchorMs }

func (s *pairRecordedFundingSource) Append(raw []byte) error {
	b, err := pairDecodeFundingBatch(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.SourceID != s.sourceID {
		return fmt.Errorf("funding batch %d belongs to source %s, not %s", b.Seq, b.SourceID, s.sourceID)
	}
	if len(s.batches) >= pairFundingMaxBatches {
		return fmt.Errorf("recorded funding source %s already holds %d batches", s.sourceID, len(s.batches))
	}
	want, prev := int64(len(s.batches)+1), ""
	if n := len(s.batches); n > 0 {
		prev = s.batches[n-1].BatchHash
	}
	if b.Seq != want || b.PrevHash != prev {
		return fmt.Errorf("funding batch %d (prev %s) does not extend the chain at seq %d (head %s); prior evidence is immutable and only an appended batch is accepted", b.Seq, shortFingerprint(b.PrevHash), want, shortFingerprint(prev))
	}
	for _, c := range b.Coverage {
		for _, iv := range c.Proven {
			if iv.FromMs < s.anchorMs-1 {
				return fmt.Errorf("funding batch %d coverage %s proves (%d, %d], before the source anchor %d", b.Seq, c.EvidenceID, iv.FromMs, iv.ThroughMs, s.anchorMs)
			}
		}
	}
	if err := pairFundingCheckRecords(pairFundingRecordSet{Rates: b.Rates, Prices: b.Prices, Coverage: b.Coverage}, s.ids); err != nil {
		return fmt.Errorf("funding batch %d is refused: %w", b.Seq, err)
	}
	all := pairFundingRecordSet{
		Rates:    append(append([]pairFundingRate{}, s.set.Rates...), b.Rates...),
		Prices:   append(append([]pairFundingPrice{}, s.set.Prices...), b.Prices...),
		Coverage: append(append([]pairFundingCoverageRecord{}, s.set.Coverage...), b.Coverage...),
	}
	for _, p := range b.Prices {
		for _, q := range s.set.Prices {
			if p.Key == q.Key && p.EvidenceID == q.EvidenceID {
				return fmt.Errorf("funding batch %d price %s repeats an earlier evidence id", b.Seq, p.EvidenceID)
			}
		}
	}
	for _, c := range b.Coverage {
		for _, q := range s.set.Coverage {
			if c.Key == q.Key && c.EvidenceID == q.EvidenceID {
				return fmt.Errorf("funding batch %d coverage %s repeats an earlier evidence id", b.Seq, c.EvidenceID)
			}
		}
	}
	for _, c := range b.Coverage {
		for _, r := range all.Rates {
			if r.Key != c.Key || r.VisibleMs <= c.VisibleMs {
				continue
			}
			for _, iv := range c.Proven {
				if iv.FromMs < r.EventMs && r.EventMs <= iv.ThroughMs {
					return fmt.Errorf("funding batch %d coverage %s proves (%d, %d] at %d, but rate %s inside it is first visible at %d", b.Seq, c.EvidenceID, iv.FromMs, iv.ThroughMs, c.VisibleMs, r.EventID, r.VisibleMs)
				}
			}
		}
	}
	for _, c := range s.set.Coverage {
		for _, r := range b.Rates {
			if r.Key != c.Key || r.VisibleMs <= c.VisibleMs {
				continue
			}
			for _, iv := range c.Proven {
				if iv.FromMs < r.EventMs && r.EventMs <= iv.ThroughMs {
					return fmt.Errorf("funding batch %d adds rate %s inside (%d, %d], which earlier coverage %s already proved complete; prior evidence is immutable", b.Seq, r.EventID, iv.FromMs, iv.ThroughMs, c.EvidenceID)
				}
			}
		}
	}
	for _, r := range b.Rates {
		sh, _ := pairFundingSourceHash(r)
		s.ids[pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)] = sh
	}
	s.set = all
	s.batches = append(s.batches, b)
	s.raw = append(s.raw, append([]byte(nil), raw...))
	return nil
}

func (s *pairRecordedFundingSource) chain() []pairFundingBatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]pairFundingBatch(nil), s.batches...)
}

func (s *pairRecordedFundingSource) chainRaw(seq int64) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if seq < 1 || seq > int64(len(s.raw)) {
		return nil
	}
	return append([]byte(nil), s.raw[seq-1]...)
}

func (s *pairRecordedFundingSource) Collect(ctx context.Context, req pairFundingRequest) (pairFundingCollected, error) {
	if err := pairFundingRequestCheck(ctx, req); err != nil {
		return pairFundingCollected{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := pairFundingFilter(s.set, req)
	out.SourceKind, out.SourceID = pairFundingSourceRecorded, s.sourceID
	return out, nil
}

func (s *pairRecordedFundingSource) NextVisibleMs(key pairEvidenceKey, afterMs int64) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return pairFundingNextVisible(s.set, key, afterMs)
}

func pairFundingCheckAgainstContract(set pairFundingRecordSet, c basketContract) error {
	members := map[string]pairContractMember{}
	for _, m := range c.Pair.Members {
		members[m.MemberID] = m
	}
	check := func(k pairEvidenceKey, kind, id string) error {
		m, ok := members[k.MemberID]
		if !ok || k.Partition != c.Partition {
			return fmt.Errorf("funding %s %s key %s names no pair member in partition %s", kind, id, k, c.Partition)
		}
		return pairCheckKeyForMember(k, m)
	}
	for _, r := range set.Rates {
		if err := check(r.Key, "rate", r.EventID); err != nil {
			return err
		}
	}
	for _, p := range set.Prices {
		if err := check(p.Key, "price", p.EvidenceID); err != nil {
			return err
		}
	}
	for _, cv := range set.Coverage {
		if err := check(cv.Key, "coverage", cv.EvidenceID); err != nil {
			return err
		}
	}
	return nil
}

func pairFundingBindingFor(c basketContract, src pairFundingSource) (pairFundingBinding, []byte, error) {
	rulesDigest, rules, err := pairFundingRulesDigest()
	if err != nil {
		return pairFundingBinding{}, nil, err
	}
	b := pairFundingBinding{V: pairFundingBindingVersion, ContractFingerprint: c.Fingerprint(), SourceKind: src.Kind(), SourceID: src.SourceID(), RulesDigest: rulesDigest, Rules: rules}
	for _, m := range c.pairMembersSorted() {
		b.Members = append(b.Members, c.pairMemberKey(m).evidenceKey())
	}
	switch s := src.(type) {
	case *pairFrozenFundingSource:
		if err := pairFundingCheckAgainstContract(s.set, c); err != nil {
			return pairFundingBinding{}, nil, err
		}
		b.Digest = s.Digest()
	case *pairRecordedFundingSource:
		s.mu.RLock()
		err := pairFundingCheckAgainstContract(s.set, c)
		s.mu.RUnlock()
		if err != nil {
			return pairFundingBinding{}, nil, err
		}
		b.AnchorMs = s.AnchorMs()
	default:
		return pairFundingBinding{}, nil, fmt.Errorf("pair research needs the immutable frozen funding source or the append-only recorded funding source")
	}
	for _, m := range c.pairMembersSorted() {
		rule, ok := pairFundingRuleVersionFor(m.Provider)
		if !ok {
			return pairFundingBinding{}, nil, fmt.Errorf("no single funding rule serves provider %s", m.Provider)
		}
		r, _ := pairFundingRuleFor(rule)
		if r.Currency != m.Meta.SettlementCurrency {
			return pairFundingBinding{}, nil, fmt.Errorf("funding rule %s settles in %s, member %s settles in %s", rule, r.Currency, m.MemberID, m.Meta.SettlementCurrency)
		}
	}
	raw, err := pairCanonicalJSON(b)
	if err != nil {
		return pairFundingBinding{}, nil, err
	}
	return b, raw, nil
}

func pairFundingBindingCanonical(raw []byte) ([]byte, pairFundingBinding, error) {
	var b pairFundingBinding
	if err := pairFundingStrictDecode(raw, &b, "funding binding", 1<<20); err != nil {
		return nil, b, err
	}
	canon, err := pairCanonicalJSON(b)
	if err != nil {
		return nil, b, err
	}
	return canon, b, nil
}

func pairFundingBindingDiff(want, got pairFundingBinding) string {
	switch {
	case got.V != want.V:
		return fmt.Sprintf("binding version %d, want %d", got.V, want.V)
	case got.ContractFingerprint != want.ContractFingerprint:
		return fmt.Sprintf("contract %s, the run has %s", shortFingerprint(got.ContractFingerprint), shortFingerprint(want.ContractFingerprint))
	case got.SourceKind != want.SourceKind:
		return fmt.Sprintf("funding source kind %s, the run has %s", got.SourceKind, want.SourceKind)
	case got.SourceID != want.SourceID:
		return fmt.Sprintf("funding source id %s, the run has %s", got.SourceID, want.SourceID)
	case got.Digest != want.Digest:
		return fmt.Sprintf("frozen funding digest %s, the run has %s", shortFingerprint(got.Digest), shortFingerprint(want.Digest))
	case got.AnchorMs != want.AnchorMs:
		return fmt.Sprintf("recorded funding anchor %d, the run has %d", got.AnchorMs, want.AnchorMs)
	case got.RulesDigest != want.RulesDigest:
		return fmt.Sprintf("venue funding rules %s (%s), the run has %s (%s)", shortFingerprint(got.RulesDigest), strings.Join(got.Rules, ","), shortFingerprint(want.RulesDigest), strings.Join(want.Rules, ","))
	case len(got.Members) != len(want.Members):
		return fmt.Sprintf("%d members, the run has %d", len(got.Members), len(want.Members))
	}
	for i := range want.Members {
		if got.Members[i] != want.Members[i] {
			return fmt.Sprintf("member %s, the run has %s", got.Members[i], want.Members[i])
		}
	}
	return "the canonical bytes differ"
}

func pairFundingCompareBinding(wantRaw, gotRaw []byte) error {
	want, wantB, err := pairFundingBindingCanonical(wantRaw)
	if err != nil {
		return err
	}
	got, gotB, err := pairFundingBindingCanonical(gotRaw)
	if err != nil {
		return fmt.Errorf("the saved funding binding is malformed: %v", err)
	}
	if bytes.Equal(want, got) {
		return nil
	}
	return fmt.Errorf("the saved funding binding does not match this run: %s", pairFundingBindingDiff(wantB, gotB))
}

func (s *pairFrozenEvidenceSource) FundingConversion(m pairContractMember, currency string, eventMs, cutoffMs int64) (pairConversionEvidence, string, error) {
	if s == nil || !s.bound {
		return pairConversionEvidence{}, pairFundingReasonConversionError, fmt.Errorf("pair evidence source is not bound to a contract")
	}
	k, err := s.key(context.Background(), m)
	if err != nil {
		return pairConversionEvidence{}, pairFundingReasonConversionError, err
	}
	validAtEvent := false
	var best *pairConversionEvidence
	for i := range s.doc.Conversions {
		r := &s.doc.Conversions[i]
		if r.Key != k || r.Currency != currency {
			continue
		}
		if !(r.ValidFromMs <= eventMs && eventMs <= r.ValidThroughMs) {
			continue
		}
		validAtEvent = true
		if r.ObservedMs > cutoffMs || r.FirstReceiptMs > cutoffMs {
			continue
		}
		if best == nil {
			best = r
			continue
		}
		switch pairNewer(r.pairEvidenceStamp, best.pairEvidenceStamp) {
		case 1:
			best = r
		case 0:
			if r.RateUSD != best.RateUSD {
				return pairConversionEvidence{}, pairFundingReasonConversionError, fmt.Errorf("conversion evidence %s and %s for %s %s tie at receipt %d with different rates", best.EvidenceID, r.EvidenceID, k, currency, r.FirstReceiptMs)
			}
			if r.EvidenceID < best.EvidenceID {
				best = r
			}
		}
	}
	switch {
	case best != nil:
		return *best, "", nil
	case validAtEvent:
		return pairConversionEvidence{}, pairFundingReasonConversionCutoff, fmt.Errorf("a %s-to-USD conversion valid at %d exists for %s, but none was received by the processing cutoff %d", currency, eventMs, k, cutoffMs)
	}
	return pairConversionEvidence{}, pairFundingReasonConversionAtEvent, fmt.Errorf("no %s-to-USD conversion for %s is valid at event time %d", currency, k, eventMs)
}

func (s *pairFrozenEvidenceSource) hasConversion(c pairConversionEvidence) bool {
	if s == nil {
		return false
	}
	for _, r := range s.doc.Conversions {
		if r.EvidenceHash == c.EvidenceHash && r.Key == c.Key && r.Currency == c.Currency {
			return true
		}
	}
	return false
}

func pairFundingFromHyperliquidCoverage(key pairEvidenceKey, cov feedFundingCoverage, requestedThroughMs, visibleMs int64, provenance, evidenceID string) ([]pairFundingRate, pairFundingCoverageRecord, error) {
	if key.Provider != entryProviderHyperliquid || key.Instrument != cov.Coin {
		return nil, pairFundingCoverageRecord{}, fmt.Errorf("hyperliquid coverage for %s cannot prove funding for %s", cov.Coin, key)
	}
	from := cov.FromMs - 1
	if from < 0 || requestedThroughMs <= from {
		return nil, pairFundingCoverageRecord{}, fmt.Errorf("hyperliquid coverage window (%d, %d] is empty or negative", from, requestedThroughMs)
	}
	if err := validateFundingCoverageRecords(cov.Coin, cov.FromMs, cov.ToMs, cov.Records); err != nil {
		return nil, pairFundingCoverageRecord{}, err
	}
	rates := []pairFundingRate{}
	for _, r := range cov.Records {
		if r.TimeMs > visibleMs {
			return nil, pairFundingCoverageRecord{}, fmt.Errorf("hyperliquid record at %d is after its visibility %d", r.TimeMs, visibleMs)
		}
		d, err := pairDecimalFromFloat(r.Rate)
		if err != nil {
			return nil, pairFundingCoverageRecord{}, err
		}
		rec, err := pairFundingRate{V: pairFundingRecordVersion, Key: key, EventID: pairFundingEventID(key.Provider, key.Instrument, r.TimeMs), RuleVersion: pairFundingRuleHyperliquid,
			EventMs: r.TimeMs, VisibleMs: visibleMs, Rate: d, Provenance: provenance}.sealed()
		if err != nil {
			return nil, pairFundingCoverageRecord{}, err
		}
		rates = append(rates, rec)
	}
	c := pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: key, EvidenceID: evidenceID, RequestedFromMs: from, RequestedThroughMs: requestedThroughMs,
		Proven: []pairFundingInterval{}, Gaps: []pairCoverageGap{}, VisibleMs: visibleMs, Provenance: provenance}
	through := from
	if len(cov.Records) > 0 {
		through = cov.ToMs
		if through > requestedThroughMs {
			through = requestedThroughMs
		}
		c.Proven = append(c.Proven, pairFundingInterval{FromMs: from, ThroughMs: through})
	}
	if through < requestedThroughMs {
		c.Gaps = append(c.Gaps, pairCoverageGap{FromMs: through, ThroughMs: requestedThroughMs, Reason: "after_last_record"})
	}
	sealed, err := c.sealed()
	return rates, sealed, err
}
