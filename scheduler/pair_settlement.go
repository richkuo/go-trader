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
	"time"
)

const (
	pairObligationFillCoverage          = "fill_coverage"
	pairObligationFeeUnconfirmed        = "fee_unconfirmed"
	pairObligationSettlementConversion  = "settlement_conversion"
	pairObligationConversionUnavailable = "conversion_unavailable"
	pairObligationFeeIncomplete         = "fee_incomplete"
	pairObligationFeeConflict           = "fee_conflict"
	pairObligationFeeInvalid            = "fee_invalid"
	pairObligationLegacySettlement      = "legacy_settlement"

	pairFillTimeVenue    = "venue_time"
	pairFillTimeAccepted = "accepted_at"

	pairFeeModeQuantity   = "with_quantity"
	pairFeeModeCorrection = "correction"

	pairLedgerVersion = 1
)

var pairQuantityHoldKinds = []string{pairObligationFillCoverage, pairObligationFeeUnconfirmed, pairObligationSettlementConversion, pairObligationConversionUnavailable, pairObligationLegacySettlement}

func pairSettlementObligationKind(kind string) bool {
	switch kind {
	case pairObligationFillCoverage, pairObligationFeeUnconfirmed, pairObligationSettlementConversion, pairObligationConversionUnavailable,
		pairObligationFeeIncomplete, pairObligationFeeConflict, pairObligationFeeInvalid, pairObligationLegacySettlement:
		return true
	}
	return false
}

type pairConfirmedFill struct {
	FillID         string `json:"fill_id"`
	VenueTimeMs    int64  `json:"venue_time_ms"`
	BaseQty        string `json:"base_qty"`
	NativeNotional string `json:"native_notional"`
	EvidenceHash   string `json:"evidence_hash"`
}

type pairFeeFill struct {
	FillID         string `json:"fill_id"`
	VenueTimeMs    int64  `json:"venue_time_ms"`
	BaseQty        string `json:"base_qty"`
	NativeNotional string `json:"native_notional"`
	FeeCurrency    string `json:"fee_currency"`
	FeeAmount      string `json:"fee_amount"`
	EvidenceHash   string `json:"evidence_hash"`
}

func (f pairConfirmedFill) computeHash() (string, error) {
	f.EvidenceHash = ""
	return pairRecordHashOf(f)
}

func (f pairConfirmedFill) sealed() (pairConfirmedFill, error) {
	h, err := f.computeHash()
	f.EvidenceHash = h
	return f, err
}

func (f pairFeeFill) computeHash() (string, error) {
	f.EvidenceHash = ""
	return pairRecordHashOf(f)
}

func (f pairFeeFill) sealed() (pairFeeFill, error) {
	h, err := f.computeHash()
	f.EvidenceHash = h
	return f, err
}

func (f pairFeeFill) sameExecution(e pairConfirmedFill) bool {
	return f.FillID == e.FillID && f.VenueTimeMs == e.VenueTimeMs && f.BaseQty == e.BaseQty && f.NativeNotional == e.NativeNotional
}

type pairConfirmedFee struct {
	Present         bool
	Currency        string
	Amount          float64
	Conversion      pairConversionEvidence
	EvidenceID      string
	EvidenceVersion int64
	DeltaUSD        float64
	Parts           []pairLedgerFee
}

type pairLedgerRow struct {
	TradeTsMs        int64  `json:"trade_ts_ms"`
	TradeTs          string `json:"trade_ts,omitempty"`
	OrderID          string `json:"order_id"`
	IsClose          bool   `json:"is_close,omitempty"`
	ClosedPositionID string `json:"closed_position_id,omitempty"`
}

type pairLedgerFill struct {
	ScopedFillID   string                  `json:"scoped_fill_id"`
	FillID         string                  `json:"fill_id"`
	EvidenceHash   string                  `json:"evidence_hash"`
	SelectedTimeMs int64                   `json:"selected_time_ms"`
	TimeSource     string                  `json:"time_source"`
	SettlementAtMs int64                   `json:"settlement_at_ms"`
	BaseQty        string                  `json:"base_qty"`
	NativeNotional string                  `json:"native_notional"`
	PriceCurrency  string                  `json:"price_currency"`
	Conversion     *pairConversionEvidence `json:"conversion,omitempty"`
	RateUSD        string                  `json:"rate_usd"`
	NotionalUSD    float64                 `json:"notional_usd"`
	Booking        pairLedgerRow           `json:"booking"`
}

type pairLedgerFee struct {
	ScopedFillID   string                  `json:"scoped_fill_id"`
	FillID         string                  `json:"fill_id"`
	EvidenceHash   string                  `json:"evidence_hash"`
	SelectedTimeMs int64                   `json:"selected_time_ms"`
	SettlementAtMs int64                   `json:"settlement_at_ms"`
	FeeCurrency    string                  `json:"fee_currency"`
	FeeAmount      string                  `json:"fee_amount"`
	Conversion     *pairConversionEvidence `json:"conversion,omitempty"`
	RateUSD        string                  `json:"rate_usd"`
	FeeUSD         float64                 `json:"fee_usd"`
	Mode           string                  `json:"mode"`
	Target         pairLedgerRow           `json:"target"`
}

type pairLedger struct {
	V     int              `json:"v"`
	Fills []pairLedgerFill `json:"fills"`
	Fees  []pairLedgerFee  `json:"fees"`
}

func pairOrderScope(a *basketAction) string {
	return strings.Join([]string{a.Partition.String(), a.StrategyID, a.Provider, a.AccountKey, a.Instrument, a.ClientOrderID}, "|")
}

func pairScopedFillID(a *basketAction, fillID string) string {
	return pairOrderScope(a) + "|" + fillID
}

func pairFeeEvidenceID(a *basketAction) string {
	return "pfe-" + pairSHA256([]byte(pairOrderScope(a)))[:32]
}

func pairParseFills(raw string) ([]pairConfirmedFill, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []pairConfirmedFill
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("the saved execution records are unreadable: %w", err)
	}
	return out, nil
}

func pairParseFees(raw string) ([]pairFeeFill, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []pairFeeFill
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("the saved fee records are unreadable: %w", err)
	}
	return out, nil
}

func pairParseLedger(raw string) (pairLedger, error) {
	l := pairLedger{V: pairLedgerVersion}
	if strings.TrimSpace(raw) == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return pairLedger{}, fmt.Errorf("the saved settlement ledger is unreadable: %w", err)
	}
	if l.V != pairLedgerVersion {
		return pairLedger{}, fmt.Errorf("the saved settlement ledger version %d is not supported", l.V)
	}
	return l, nil
}

func pairMarshalRecords(v interface{}, n int) string {
	if n == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (l pairLedger) json() string {
	if len(l.Fills) == 0 && len(l.Fees) == 0 {
		return ""
	}
	l.V = pairLedgerVersion
	b, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	return string(b)
}

func pairCoverageRat(name, s string, positive bool) (*big.Rat, error) {
	if !pairDecimalCanonical(s) {
		return nil, fmt.Errorf("%s %q is not a canonical decimal string", name, s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%s %q is unreadable", name, s)
	}
	if positive && r.Sign() <= 0 {
		return nil, fmt.Errorf("%s %s must be above zero", name, s)
	}
	return r, nil
}

func pairRatFloat(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}

func pairCheckExecutionFields(id string, venueMs int64, qty, notional string) error {
	if !pairPrintableASCII(id, pairEvidenceMaxIDLen) {
		return fmt.Errorf("fill id %q must be 1..%d printable ASCII characters", id, pairEvidenceMaxIDLen)
	}
	if venueMs < 0 {
		return fmt.Errorf("fill %s has a negative venue time", id)
	}
	if _, err := pairCoverageRat("fill "+id+" base_qty", qty, true); err != nil {
		return err
	}
	if _, err := pairCoverageRat("fill "+id+" native_notional", notional, true); err != nil {
		return err
	}
	return nil
}

func pairCheckConfirmedFill(f pairConfirmedFill) error {
	if err := pairCheckExecutionFields(f.FillID, f.VenueTimeMs, f.BaseQty, f.NativeNotional); err != nil {
		return err
	}
	h, err := f.computeHash()
	if err != nil {
		return err
	}
	if f.EvidenceHash != h {
		return fmt.Errorf("fill %s evidence_hash does not match its canonical bytes", f.FillID)
	}
	return nil
}

func pairCheckFeeFill(f pairFeeFill, m pairContractMember) error {
	if err := pairCheckExecutionFields(f.FillID, f.VenueTimeMs, f.BaseQty, f.NativeNotional); err != nil {
		return err
	}
	allowed := false
	for _, c := range m.Meta.FeeCurrencies {
		if c == f.FeeCurrency && f.FeeCurrency != "" {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("fill %s fee currency %q is not a fee currency of the frozen member contract %v", f.FillID, f.FeeCurrency, m.Meta.FeeCurrencies)
	}
	if _, err := pairCoverageRat("fill "+f.FillID+" fee_amount", f.FeeAmount, false); err != nil {
		return err
	}
	h, err := f.computeHash()
	if err != nil {
		return err
	}
	if f.EvidenceHash != h {
		return fmt.Errorf("fee record %s evidence_hash does not match its canonical bytes", f.FillID)
	}
	return nil
}

func pairFillSums(fills []pairConfirmedFill) (float64, float64) {
	q, n := new(big.Rat), new(big.Rat)
	for _, f := range fills {
		if r, err := pairCoverageRat("base_qty", f.BaseQty, true); err == nil {
			q.Add(q, r)
		}
		if r, err := pairCoverageRat("native_notional", f.NativeNotional, true); err == nil {
			n.Add(n, r)
		}
	}
	return pairRatFloat(q), pairRatFloat(n)
}

func pairNotionalEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func pairExecutionComplete(fills []pairConfirmedFill, cumQty, cumNotional float64) bool {
	if len(fills) == 0 || !(cumQty > 0) {
		return false
	}
	q, n := pairFillSums(fills)
	return basketQtyEqual(q, cumQty) && pairNotionalEqual(n, cumNotional)
}

func pairFeeCoversAll(fees []pairFeeFill, exec []pairConfirmedFill) bool {
	if len(exec) == 0 {
		return false
	}
	covered := map[string]bool{}
	for _, f := range fees {
		covered[f.FillID] = true
	}
	for _, e := range exec {
		if !covered[e.FillID] {
			return false
		}
	}
	return true
}

func pairFeeTotals(fees []pairFeeFill) (string, float64) {
	cur := ""
	sum := new(big.Rat)
	for i, f := range fees {
		if i == 0 {
			cur = f.FeeCurrency
		} else if f.FeeCurrency != cur {
			return "", 0
		}
		if r, err := pairCoverageRat("fee_amount", f.FeeAmount, false); err == nil {
			sum.Add(sum, r)
		}
	}
	return cur, pairRatFloat(sum)
}

type pairExecEval struct {
	fills []pairConfirmedFill
	raw   string
	grew  bool
}

func pairEvaluateExecution(a *basketAction, out basketVenueOutcome) (pairExecEval, string) {
	prev, err := pairParseFills(a.FillCoverageJSON)
	if err != nil {
		return pairExecEval{}, err.Error()
	}
	next := prev
	if len(out.FillCoverage) > 0 {
		next = append([]pairConfirmedFill(nil), out.FillCoverage...)
		sort.Slice(next, func(i, j int) bool {
			return pairScopedFillID(a, next[i].FillID) < pairScopedFillID(a, next[j].FillID)
		})
		byID := map[string]pairConfirmedFill{}
		for i, f := range next {
			if err := pairCheckConfirmedFill(f); err != nil {
				return pairExecEval{}, "execution record: " + err.Error()
			}
			if i > 0 && next[i-1].FillID == f.FillID {
				return pairExecEval{}, fmt.Sprintf("execution record %s appears twice", f.FillID)
			}
			byID[f.FillID] = f
		}
		for _, p := range prev {
			if n, ok := byID[p.FillID]; !ok || n != p {
				return pairExecEval{}, fmt.Sprintf("the accepted execution record %s changed or disappeared", p.FillID)
			}
		}
	}
	if len(next) > 0 {
		q, n := pairFillSums(next)
		if q > out.CumQty*(1+1e-9)+1e-12 || n > out.CumNotional*(1+1e-9)+1e-9 {
			return pairExecEval{}, fmt.Sprintf("the execution records sum to %v base and %v native notional, above the cumulative %v and %v", q, n, out.CumQty, out.CumNotional)
		}
	}
	return pairExecEval{fills: next, raw: pairMarshalRecords(next, len(next)), grew: len(next) > len(prev)}, ""
}

type pairFeeVerdict int

const (
	pairFeeNone pairFeeVerdict = iota
	pairFeeAccept
	pairFeeSame
	pairFeeStale
	pairFeeConflict
	pairFeeInvalid
)

func pairEvaluateFees(a *basketAction, m pairContractMember, out basketVenueOutcome, exec []pairConfirmedFill) (pairFeeVerdict, []pairFeeFill, string) {
	if !out.FeePresent && out.FeeEvidenceVersion == 0 && len(out.FeeCoverage) == 0 {
		return pairFeeNone, nil, ""
	}
	version := int64(len(out.FeeCoverage))
	switch {
	case version == 0:
		return pairFeeInvalid, nil, "the fee answer carries no fee-covered fill record"
	case out.FeeEvidenceVersion != version:
		return pairFeeInvalid, nil, fmt.Sprintf("fee version %d is not the number of fee-covered fills %d", out.FeeEvidenceVersion, version)
	case !out.FeePresent:
		return pairFeeInvalid, nil, "fee records arrived without fee presence"
	case out.FeeEvidenceID != pairFeeEvidenceID(a):
		return pairFeeConflict, nil, fmt.Sprintf("fee identity %q is not the saved order identity %q", out.FeeEvidenceID, pairFeeEvidenceID(a))
	}
	execByID := map[string]pairConfirmedFill{}
	for _, e := range exec {
		execByID[e.FillID] = e
	}
	next := append([]pairFeeFill(nil), out.FeeCoverage...)
	sort.Slice(next, func(i, j int) bool {
		return pairScopedFillID(a, next[i].FillID) < pairScopedFillID(a, next[j].FillID)
	})
	nextByID := map[string]pairFeeFill{}
	for i, f := range next {
		if err := pairCheckFeeFill(f, m); err != nil {
			return pairFeeInvalid, nil, "fee record: " + err.Error()
		}
		if i > 0 && next[i-1].FillID == f.FillID {
			return pairFeeInvalid, nil, fmt.Sprintf("fee record %s appears twice", f.FillID)
		}
		e, ok := execByID[f.FillID]
		if !ok || !f.sameExecution(e) {
			return pairFeeInvalid, nil, fmt.Sprintf("fee record %s does not match an accepted execution record", f.FillID)
		}
		nextByID[f.FillID] = f
	}
	if out.FeeCurrency != "" {
		cur, sum := pairFeeTotals(next)
		if strings.ToUpper(strings.TrimSpace(out.FeeCurrency)) != cur || math.Abs(sum-out.FeeAmount) > 1e-9*math.Max(1, math.Abs(sum)) {
			return pairFeeInvalid, nil, fmt.Sprintf("the summary fee %v %q disagrees with the fee records", out.FeeAmount, out.FeeCurrency)
		}
	}
	prev, err := pairParseFees(a.FeeCoverageJSON)
	if err != nil {
		return pairFeeConflict, nil, err.Error()
	}
	prevByID := map[string]pairFeeFill{}
	for _, p := range prev {
		prevByID[p.FillID] = p
	}
	prevVersion := a.FeeEvidenceVersion
	switch {
	case version > prevVersion:
		for _, p := range prev {
			n, ok := nextByID[p.FillID]
			if !ok {
				return pairFeeConflict, nil, fmt.Sprintf("fee version %d drops the accepted fee record %s, so it is not a superset of version %d", version, p.FillID, prevVersion)
			}
			if n != p {
				return pairFeeConflict, nil, fmt.Sprintf("fee version %d changes the accepted fee record %s", version, p.FillID)
			}
		}
		return pairFeeAccept, next, ""
	case version == prevVersion:
		for _, n := range next {
			if p, ok := prevByID[n.FillID]; !ok || p != n {
				return pairFeeConflict, nil, fmt.Sprintf("fee version %d carries fee records that differ from the accepted set", version)
			}
		}
		return pairFeeSame, next, ""
	}
	for _, n := range next {
		if p, ok := prevByID[n.FillID]; !ok || p != n {
			return pairFeeConflict, nil, fmt.Sprintf("the older fee version %d carries fee record %s that differs from the accepted version %d", version, n.FillID, prevVersion)
		}
	}
	return pairFeeStale, nil, ""
}

func pairLiveLegacyFee(a *basketAction) bool {
	return a.FeeEvidenceVersion > 0 && strings.TrimSpace(a.FeeCoverageJSON) == ""
}

func pairLiveLegacyReason(a *basketAction) string {
	switch {
	case pairLiveLegacyFee(a):
		return fmt.Sprintf("the action holds fee version %d from before fee-covered fill records were saved; its coverage and finality are unknown until authoritative reconciliation", a.FeeEvidenceVersion)
	case a.AppliedQty > 1e-12 && strings.TrimSpace(a.SettlementJSON) == "":
		return fmt.Sprintf("the action applied %v before the settlement ledger was saved; its fill conversions are unknown until authoritative reconciliation", a.AppliedQty)
	}
	return ""
}

type pairFillTime struct {
	AtMs   int64
	Source string
}

func pairSelectFillTime(a *basketAction, venueMs int64) pairFillTime {
	if venueMs > 0 {
		return pairFillTime{AtMs: venueMs, Source: pairFillTimeVenue}
	}
	if a.AcceptedAtMs > 0 {
		return pairFillTime{AtMs: a.AcceptedAtMs, Source: pairFillTimeAccepted}
	}
	return pairFillTime{}
}

func pairSettlementEligible(s pairEvidenceStamp, fillAtMs, settlementAtMs int64) bool {
	return s.ObservedMs <= fillAtMs && s.FirstReceiptMs <= settlementAtMs && s.ValidFromMs <= fillAtMs && fillAtMs <= s.ValidThroughMs
}

func (s *pairFrozenEvidenceSource) SettlementConversion(ctx context.Context, m pairContractMember, currency string, fillAtMs, settlementAtMs int64) (pairConversionEvidence, error) {
	k, err := s.key(ctx, m)
	if err != nil {
		return pairConversionEvidence{}, err
	}
	if fillAtMs <= 0 || settlementAtMs <= 0 {
		return pairConversionEvidence{}, fmt.Errorf("settlement conversion needs a positive fill time and settlement time, got %d and %d", fillAtMs, settlementAtMs)
	}
	required := false
	for _, c := range pairRequiredConversionCurrencies(s.members[m.MemberID]) {
		if c == currency {
			required = true
		}
	}
	if !required {
		return pairConversionEvidence{}, fmt.Errorf("currency %q is not a settlement, price or fee currency of member %s that needs a conversion record", currency, m.MemberID)
	}
	var best *pairConversionEvidence
	for i := range s.doc.Conversions {
		r := &s.doc.Conversions[i]
		if r.Key != k || r.Currency != currency || !pairSettlementEligible(r.pairEvidenceStamp, fillAtMs, settlementAtMs) {
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
				return pairConversionEvidence{}, fmt.Errorf("conversion evidence %s and %s for %s %s tie at receipt %d with different rates", best.EvidenceID, r.EvidenceID, k, currency, r.FirstReceiptMs)
			}
			if r.EvidenceID < best.EvidenceID {
				best = r
			}
		}
	}
	if best == nil {
		return pairConversionEvidence{}, fmt.Errorf("no %s-to-USD conversion evidence for %s is observed by the fill at %d, received by settlement at %d and valid at the fill", currency, k, fillAtMs, settlementAtMs)
	}
	return *best, nil
}

type pairConvNeed struct {
	Currency string
	AtMs     int64
}

type pairResolvedConv struct {
	Rate     *big.Rat
	RateText string
	Evidence *pairConversionEvidence
}

func pairCheckSettlementConversion(rt *basketRuntime, m pairContractMember, ev pairConversionEvidence, need pairConvNeed, settlementAtMs int64) (pairResolvedConv, error) {
	if ev.Currency != need.Currency {
		return pairResolvedConv{}, fmt.Errorf("conversion evidence %s is for %s, not %s", ev.EvidenceID, ev.Currency, need.Currency)
	}
	if ev.Key.Partition != rt.Partition.String() {
		return pairResolvedConv{}, fmt.Errorf("conversion evidence %s key partition %s is not the pair partition %s", ev.EvidenceID, ev.Key.Partition, rt.Partition.String())
	}
	if err := pairConversionMatches(m, ev); err != nil {
		return pairResolvedConv{}, err
	}
	if !pairSettlementEligible(ev.pairEvidenceStamp, need.AtMs, settlementAtMs) {
		return pairResolvedConv{}, fmt.Errorf("conversion evidence %s is not observed by the fill at %d, received by settlement at %d and valid at the fill", ev.EvidenceID, need.AtMs, settlementAtMs)
	}
	r, err := ev.RateUSD.rat()
	if err != nil {
		return pairResolvedConv{}, err
	}
	c := ev
	return pairResolvedConv{Rate: r, RateText: string(ev.RateUSD), Evidence: &c}, nil
}

func pairRequestConversions(a *basketAction) []pairConversionEvidence {
	var req pairActionRequest
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err != nil || req.V != pairActionRequestVersion {
		return nil
	}
	return req.Conversions
}

func pairSelectRequestConversion(rt *basketRuntime, m pairContractMember, convs []pairConversionEvidence, need pairConvNeed, settlementAtMs int64) (pairResolvedConv, bool) {
	var best *pairResolvedConv
	var bestStamp pairEvidenceStamp
	for _, c := range convs {
		if c.Currency != need.Currency {
			continue
		}
		got, err := pairCheckSettlementConversion(rt, m, c, need, settlementAtMs)
		if err != nil {
			continue
		}
		if best == nil {
			best, bestStamp = &got, c.pairEvidenceStamp
			continue
		}
		switch pairNewer(c.pairEvidenceStamp, bestStamp) {
		case 1:
			best, bestStamp = &got, c.pairEvidenceStamp
		case 0:
			if got.RateText != best.RateText {
				return pairResolvedConv{}, false
			}
		}
	}
	if best == nil {
		return pairResolvedConv{}, false
	}
	return *best, true
}

type pairSettleRev struct {
	ptr      *basketAction
	status   string
	cq, cn   float64
	aq, an   float64
	af       float64
	fills    string
	fees     string
	ledger   string
	booked   string
	final    bool
	book     float64
	nTrades  int
	obligKey int
}

func (bc *basketCoordinator) pairCaptureSettleRev(rt *basketRuntime, a *basketAction) pairSettleRev {
	s := bc.state.Strategies[a.StrategyID]
	r := pairSettleRev{ptr: a, status: a.Status, cq: a.ConfirmedQty, cn: a.ConfirmedNotional, aq: a.AppliedQty, an: a.AppliedNotional, af: a.AppliedFee,
		fills: a.FillCoverageJSON, fees: a.FeeCoverageJSON, ledger: a.SettlementJSON, booked: a.BookedJSON, final: a.FeeFinal}
	if s != nil {
		r.book = basketPositionSigned(s, a.Symbol)
		r.nTrades = len(s.TradeHistory)
	}
	if rt.Pair.Progress.obligation(pairObligationFeeConflict, a.StrategyID, a.ActionID) != nil {
		r.obligKey = 1
	}
	return r
}

func (bc *basketCoordinator) pairResolveConversions(rt *basketRuntime, a *basketAction, m pairContractMember, needs []pairConvNeed, in basketCycleInputs) (map[pairConvNeed]pairResolvedConv, map[pairConvNeed]string, int64, bool) {
	resolved := map[pairConvNeed]pairResolvedConv{}
	missing := map[pairConvNeed]string{}
	settleAt := in.Now.UnixMilli()
	reqConvs := pairRequestConversions(a)
	var pending []pairConvNeed
	seen := map[pairConvNeed]bool{}
	for _, n := range needs {
		if seen[n] {
			continue
		}
		seen[n] = true
		if n.Currency == pairUSD {
			resolved[n] = pairResolvedConv{Rate: big.NewRat(1, 1), RateText: "1"}
			continue
		}
		if c, ok := pairSelectRequestConversion(rt, m, reqConvs, n, settleAt); ok {
			resolved[n] = c
			continue
		}
		pending = append(pending, n)
	}
	if len(pending) == 0 {
		return resolved, missing, settleAt, true
	}
	var src *pairFrozenEvidenceSource
	if rt.Pair != nil && rt.Pair.admission != nil {
		src = rt.Pair.admission.evidence
	}
	lookup := bc.pairConversionLookup
	if lookup == nil && src == nil {
		for _, n := range pending {
			missing[n] = "the pair has no frozen evidence source for settlement conversions"
		}
		return resolved, missing, settleAt, true
	}
	if lookup == nil {
		lookup = src.SettlementConversion
	}
	rev := bc.pairCaptureSettleRev(rt, a)
	parent, clock := bc.parent, bc.pairClock
	if parent == nil {
		parent = context.Background()
	}
	if clock == nil {
		clock = tradingNow
	}
	budget := time.Duration(rt.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	if budget <= 0 {
		budget = 2 * time.Second
	}
	got := make([]pairConversionEvidence, len(pending))
	errs := make([]error, len(pending))
	bc.unlocked(func() {
		if fresh := clock().UnixMilli(); fresh > settleAt {
			settleAt = fresh
		}
		ctx, cancel := context.WithTimeout(parent, budget)
		defer cancel()
		for i, n := range pending {
			got[i], errs[i] = lookup(ctx, m, n.Currency, n.AtMs, settleAt)
		}
	})
	if bc.pairCaptureSettleRev(rt, a) != rev || rt.Actions[a.ActionID] != a {
		bc.pairLogf(rt, "action %s changed while its settlement conversions were looked up; the stale result is discarded and the next cycle settles again", a.ActionID)
		return nil, nil, 0, false
	}
	for i, n := range pending {
		if errs[i] != nil {
			missing[n] = errs[i].Error()
			continue
		}
		c, err := pairCheckSettlementConversion(rt, m, got[i], n, settleAt)
		if err != nil {
			missing[n] = "the returned conversion evidence is refused: " + err.Error()
			continue
		}
		resolved[n] = c
	}
	return resolved, missing, settleAt, true
}

type pairSettlePlan struct {
	exec      []pairConfirmedFill
	fees      []pairFeeFill
	ledger    pairLedger
	booked    map[string]pairLedgerFill
	qtyDue    bool
	qtyKind   string
	qtyWhy    string
	remainder bool
	dueFills  []pairConfirmedFill
	dueTimes  map[string]pairFillTime
	dueFees   []pairFeeFill
	fixFees   []pairFeeFill
}

func pairBuildSettlePlan(a *basketAction) (pairSettlePlan, error) {
	exec, err := pairParseFills(a.FillCoverageJSON)
	if err != nil {
		return pairSettlePlan{}, err
	}
	fees, err := pairParseFees(a.FeeCoverageJSON)
	if err != nil {
		return pairSettlePlan{}, err
	}
	ledger, err := pairParseLedger(a.SettlementJSON)
	if err != nil {
		return pairSettlePlan{}, err
	}
	pl := pairSettlePlan{exec: exec, fees: fees, ledger: ledger, booked: map[string]pairLedgerFill{}, dueTimes: map[string]pairFillTime{}}
	for _, f := range ledger.Fills {
		pl.booked[f.FillID] = f
	}
	applied := map[string]bool{}
	for _, f := range ledger.Fees {
		applied[f.FillID] = true
	}
	remaining := a.ConfirmedQty - a.AppliedQty
	pl.qtyDue = remaining > 1e-12
	if pl.qtyDue {
		switch {
		case !pairExecutionComplete(exec, a.ConfirmedQty, a.ConfirmedNotional):
			pl.qtyKind = pairObligationFillCoverage
			pl.qtyWhy = fmt.Sprintf("the %d execution fill records do not cover the confirmed %v base and %v native notional; quantity settlement holds", len(exec), a.ConfirmedQty, a.ConfirmedNotional)
		case len(fees) == 0:
			pl.qtyKind = pairObligationFeeUnconfirmed
			pl.qtyWhy = "no fee-covered fill record is accepted yet; quantity settlement holds and no modeled fee replaces confirmed evidence"
		default:
			for _, f := range exec {
				if _, ok := pl.booked[f.FillID]; !ok {
					pl.dueFills = append(pl.dueFills, f)
				}
			}
			if len(pl.dueFills) == 0 {
				pl.remainder = true
				break
			}
			q, _ := pairFillSums(pl.dueFills)
			if !basketQtyEqual(q, remaining) {
				pl.qtyKind = pairObligationLegacySettlement
				pl.qtyWhy = fmt.Sprintf("the unbooked fill records sum to %v base, but %v confirmed base is unapplied; the settlement waits for authoritative reconciliation", q, remaining)
				break
			}
			due := map[string]bool{}
			for _, f := range pl.dueFills {
				t := pairSelectFillTime(a, f.VenueTimeMs)
				if t.AtMs <= 0 {
					pl.qtyKind = pairObligationConversionUnavailable
					pl.qtyWhy = fmt.Sprintf("conversion_unavailable: fill %s has no venue time and the action has no accepted time", f.FillID)
					break
				}
				pl.dueTimes[f.FillID] = t
				due[f.FillID] = true
			}
			for _, f := range fees {
				if due[f.FillID] {
					pl.dueFees = append(pl.dueFees, f)
				}
			}
		}
	}
	for _, f := range fees {
		if _, ok := pl.booked[f.FillID]; ok && !applied[f.FillID] {
			pl.fixFees = append(pl.fixFees, f)
		}
	}
	return pl, nil
}

func (pl pairSettlePlan) quantityNeeds(m pairContractMember) []pairConvNeed {
	if pl.qtyKind != "" || !pl.qtyDue || pl.remainder {
		return nil
	}
	var out []pairConvNeed
	for _, f := range pl.dueFills {
		out = append(out, pairConvNeed{Currency: m.Meta.PriceCurrency, AtMs: pl.dueTimes[f.FillID].AtMs})
	}
	for _, f := range pl.dueFees {
		out = append(out, pairConvNeed{Currency: f.FeeCurrency, AtMs: pl.dueTimes[f.FillID].AtMs})
	}
	return out
}

func (pl pairSettlePlan) correctionNeeds() []pairConvNeed {
	var out []pairConvNeed
	for _, f := range pl.fixFees {
		out = append(out, pairConvNeed{Currency: f.FeeCurrency, AtMs: pl.booked[f.FillID].SelectedTimeMs})
	}
	return out
}

func pairMissingDetail(needs []pairConvNeed, missing map[pairConvNeed]string) string {
	var parts []string
	for _, n := range needs {
		if why, ok := missing[n]; ok {
			parts = append(parts, fmt.Sprintf("%s at %d: %s", n.Currency, n.AtMs, why))
		}
	}
	sort.Strings(parts)
	return strings.Join(dedupeSortedStrings(parts), "; ")
}

func (bc *basketCoordinator) notePairSettlementHold(rt *basketRuntime, a *basketAction, kind, detail string, now time.Time) bool {
	p := &rt.Pair.Progress
	changed := false
	for _, k := range pairQuantityHoldKinds {
		if k != kind && p.clearObligation(k, a.StrategyID, a.ActionID) {
			changed = true
		}
	}
	if p.addObligation(pairObligation{Kind: kind, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: detail, AtMs: now.UnixMilli()}) {
		changed = true
		bc.pairAlert(rt, "settle|"+kind+"|"+a.ActionID, fmt.Sprintf("%s pair action %s (client %s): %s", a.StrategyID, a.ActionID, a.ClientOrderID, detail), false)
	}
	if changed {
		rt.touchPairProgress()
	}
	return changed
}

func (bc *basketCoordinator) clearPairQuantityHolds(rt *basketRuntime, a *basketAction) bool {
	changed := false
	for _, k := range pairQuantityHoldKinds {
		if rt.Pair.Progress.clearObligation(k, a.StrategyID, a.ActionID) {
			changed = true
		}
	}
	return changed
}

func (bc *basketCoordinator) refreshPairFeeIncomplete(rt *basketRuntime, a *basketAction, now time.Time) bool {
	p := &rt.Pair.Progress
	exec, err1 := pairParseFills(a.FillCoverageJSON)
	fees, err2 := pairParseFees(a.FeeCoverageJSON)
	if err1 != nil || err2 != nil || a.AppliedQty <= 1e-12 || a.recoveryNote().NativeBooked {
		return false
	}
	if pairExecutionComplete(exec, a.ConfirmedQty, a.ConfirmedNotional) && pairFeeCoversAll(fees, exec) {
		return p.clearObligation(pairObligationFeeIncomplete, a.StrategyID, a.ActionID)
	}
	return p.addObligation(pairObligation{Kind: pairObligationFeeIncomplete, MemberID: a.StrategyID, ActionID: a.ActionID,
		Detail: fmt.Sprintf("fee records cover %d of %d confirmed execution fills; the action stays unresolved until terminal complete coverage is final", len(fees), len(exec)), AtMs: now.UnixMilli()})
}

func (bc *basketCoordinator) notePairFeeConflict(rt *basketRuntime, a *basketAction, reason string, now time.Time) {
	p := &rt.Pair.Progress
	p.addObligation(pairObligation{Kind: pairObligationFeeConflict, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: reason, AtMs: now.UnixMilli()})
	p.IncreasesFrozen = true
	if p.FreezeReason == "" {
		p.FreezeReason = fmt.Sprintf("%s action %s fee conflict: %s", a.StrategyID, a.ActionID, reason)
	}
	rt.touchPairProgress()
	bc.pairAlert(rt, "fee_conflict|"+a.ActionID, fmt.Sprintf("**PAIR FEE CONFLICT** %s action %s (client %s): %s; the accepted fee evidence is kept, nothing is charged, increases are frozen and authoritative reconciliation is required before release",
		a.StrategyID, a.ActionID, a.ClientOrderID, reason), true)
}

func (bc *basketCoordinator) acceptPairLiveEvidence(rt *basketRuntime, a *basketAction, out basketVenueOutcome, terminal bool, now time.Time) bool {
	m, ok := rt.Contract.pairMember(a.StrategyID)
	if !ok {
		bc.holdPairAction(rt, a, "the action's member is not in the pair contract", true, now)
		return false
	}
	ev, why := pairEvaluateExecution(a, out)
	if why != "" {
		bc.holdPairAction(rt, a, "malformed or inconsistent execution evidence: "+why, true, now)
		return false
	}
	verdict, fees, reason := pairFeeNone, []pairFeeFill(nil), ""
	if !pairLiveLegacyFee(a) {
		verdict, fees, reason = pairEvaluateFees(a, m, out, ev.fills)
	}
	if why := pairLiveLegacyReason(a); why != "" {
		bc.notePairSettlementHold(rt, a, pairObligationLegacySettlement, why, now)
	}
	if a.FeeFinal && ev.grew {
		verdict, reason = pairFeeConflict, "new execution records arrived after the fee set was final"
	}
	a.FillCoverageJSON = ev.raw
	if a.AcceptedAtMs == 0 && out.AcceptedAtMs > 0 {
		a.AcceptedAtMs = out.AcceptedAtMs
	}
	switch verdict {
	case pairFeeAccept:
		cur, amt := pairFeeTotals(fees)
		a.FeeCoverageJSON = pairMarshalRecords(fees, len(fees))
		a.FeePresent, a.FeeCurrency, a.FeeAmount, a.ConfirmedFee = true, cur, amt, amt
		a.FeeEvidenceID, a.FeeEvidenceVersion = out.FeeEvidenceID, int64(len(fees))
	case pairFeeConflict:
		bc.notePairFeeConflict(rt, a, reason, now)
	case pairFeeInvalid:
		detail := "the fee answer was refused and changed nothing: " + reason
		if rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationFeeInvalid, MemberID: a.StrategyID, ActionID: a.ActionID, Detail: detail, AtMs: now.UnixMilli()}) {
			rt.touchPairProgress()
			bc.pairAlert(rt, "fee_invalid|"+a.ActionID+"|"+reason, fmt.Sprintf("%s pair action %s (client %s): %s", a.StrategyID, a.ActionID, a.ClientOrderID, detail), false)
		}
	case pairFeeStale:
		bc.pairLogf(rt, "action %s: fee version %d is older than the accepted version %d and a subset of it; the stale fee answer changes nothing", a.ActionID, out.FeeEvidenceVersion, a.FeeEvidenceVersion)
	}
	if (verdict == pairFeeAccept || verdict == pairFeeSame) && rt.Pair.Progress.clearObligation(pairObligationFeeInvalid, a.StrategyID, a.ActionID) {
		rt.touchPairProgress()
	}
	if out.FeeFinal && !a.FeeFinal && (verdict == pairFeeAccept || verdict == pairFeeSame) && terminal {
		accepted, _ := pairParseFees(a.FeeCoverageJSON)
		if pairExecutionComplete(ev.fills, out.CumQty, out.CumNotional) && pairFeeCoversAll(accepted, ev.fills) {
			a.FeeFinal = true
		} else {
			bc.pairLogf(rt, "action %s: the venue marks the fee set final, but it does not cover every confirmed execution fill; finality does not advance", a.ActionID)
		}
	}
	return true
}

func (bc *basketCoordinator) pairLiveFeeDue(a *basketAction) bool {
	if pairLiveLegacyReason(a) != "" {
		return false
	}
	pl, err := pairBuildSettlePlan(a)
	return err == nil && len(pl.fixFees) > 0
}

func (bc *basketCoordinator) pairLiveComplete(a *basketAction) bool {
	if !a.FeeFinal || pairLiveLegacyReason(a) != "" {
		return false
	}
	if rt := bc.byMember[a.StrategyID]; rt != nil && rt.isPair() && rt.Pair.Progress.obligation(pairObligationFeeConflict, a.StrategyID, a.ActionID) != nil {
		return false
	}
	pl, err := pairBuildSettlePlan(a)
	if err != nil || !pairExecutionComplete(pl.exec, a.ConfirmedQty, a.ConfirmedNotional) || !pairFeeCoversAll(pl.fees, pl.exec) {
		return false
	}
	applied := map[string]bool{}
	for _, f := range pl.ledger.Fees {
		applied[f.FillID] = true
	}
	for _, e := range pl.exec {
		if _, ok := pl.booked[e.FillID]; !ok || !applied[e.FillID] {
			return false
		}
	}
	return true
}

func pairLedgerRowFor(s *StrategyState, t Trade, symbol string) pairLedgerRow {
	row := pairLedgerRow{TradeTsMs: t.Timestamp.UTC().UnixMilli(), TradeTs: formatTime(t.Timestamp), OrderID: t.ExchangeOrderID, IsClose: t.IsClose}
	if t.IsClose && s.Positions[symbol] == nil {
		row.ClosedPositionID = t.PositionID
	}
	return row
}

func pairConfirmedFeeOf(a *basketAction, parts []pairLedgerFee) pairConfirmedFee {
	f := pairConfirmedFee{Present: true, EvidenceID: a.FeeEvidenceID, EvidenceVersion: a.FeeEvidenceVersion, Parts: parts}
	sum := new(big.Rat)
	usd := 0.0
	cur := ""
	uniform := true
	for i, p := range parts {
		usd += p.FeeUSD
		if r, err := pairCoverageRat("fee_amount", p.FeeAmount, false); err == nil {
			sum.Add(sum, r)
		}
		if i == 0 {
			cur = p.FeeCurrency
		} else if p.FeeCurrency != cur {
			uniform = false
		}
	}
	f.DeltaUSD = usd
	if uniform {
		f.Currency, f.Amount = cur, pairRatFloat(sum)
	}
	if len(parts) == 1 && parts[0].Conversion != nil {
		f.Conversion = *parts[0].Conversion
	}
	return f
}

func withPairConfirmedFeeScope(s *StrategyState, fee pairConfirmedFee, fn func()) error {
	if s == nil {
		return fmt.Errorf("the confirmed fee scope needs a book")
	}
	if fee.Present && !pairFinite(fee.DeltaUSD) {
		return fmt.Errorf("the confirmed fee delta %v is not finite", fee.DeltaUSD)
	}
	if !fee.Present {
		fn()
		return nil
	}
	prev := s.confirmedFee
	f := fee
	s.confirmedFee = &f
	defer func() { s.confirmedFee = prev }()
	fn()
	return nil
}

func strategyExecutionFee(s *StrategyState, modeledFee, fillFee float64, useFillFee bool) float64 {
	if useFillFee && s != nil && s.confirmedFee != nil && s.confirmedFee.Present {
		return s.confirmedFee.DeltaUSD
	}
	return executionFee(modeledFee, fillFee, useFillFee)
}

func strategyExecutionFeeSource(s *StrategyState, fillFee float64, useFillFee bool) string {
	if useFillFee && s != nil && s.confirmedFee != nil && s.confirmedFee.Present {
		return FeeSourceUserFills
	}
	return executionFeeSource(fillFee, useFillFee)
}

func (bc *basketCoordinator) settlePairActionLive(rt *basketRuntime, a *basketAction, in basketCycleInputs) bool {
	now := in.Now
	p := &rt.Pair.Progress
	s := bc.state.Strategies[a.StrategyID]
	m, ok := rt.Contract.pairMember(a.StrategyID)
	if !ok || s == nil {
		bc.holdPairAction(rt, a, "the action's member is not in the pair contract or has no book", true, now)
		return false
	}
	if p.obligation(pairObligationFeeConflict, a.StrategyID, a.ActionID) != nil {
		return false
	}
	if why := pairLiveLegacyReason(a); why != "" {
		if bc.notePairSettlementHold(rt, a, pairObligationLegacySettlement, why, now) {
			bc.persistPair(rt)
		}
		return false
	}
	pl, err := pairBuildSettlePlan(a)
	if err != nil {
		bc.holdPairAction(rt, a, "the saved settlement evidence is unreadable: "+err.Error(), true, now)
		return false
	}
	if !pl.qtyDue && len(pl.fixFees) == 0 {
		bc.clearPairQuantityHolds(rt, a)
		bc.refreshPairFeeIncomplete(rt, a, now)
		if bc.pairActionComplete(a) {
			a.Status = basketActionCommitted
		} else {
			a.Status = basketActionUnresolved
		}
		a.UpdatedAtMs = now.UnixMilli()
		rt.touchPairProgress()
		return bc.persistPair(rt)
	}
	qNeeds := pl.quantityNeeds(m)
	fNeeds := pl.correctionNeeds()
	resolved, missing, settleAt, fresh := bc.pairResolveConversions(rt, a, m, append(append([]pairConvNeed(nil), qNeeds...), fNeeds...), in)
	if !fresh {
		return false
	}
	if pl.qtyDue && pl.qtyKind == "" {
		if detail := pairMissingDetail(qNeeds, missing); detail != "" {
			pl.qtyKind, pl.qtyWhy = pairObligationSettlementConversion, "settlement holds: "+detail
		}
	}
	if pl.qtyDue && pl.qtyKind == "" {
		return bc.bookPairLiveQuantity(rt, a, s, m, pl, resolved, settleAt, in)
	}
	if pl.qtyDue {
		bc.notePairSettlementHold(rt, a, pl.qtyKind, pl.qtyWhy, now)
	}
	if len(pl.fixFees) > 0 {
		if detail := pairMissingDetail(fNeeds, missing); detail != "" {
			if !pl.qtyDue || pl.qtyKind == pairObligationSettlementConversion {
				bc.notePairSettlementHold(rt, a, pairObligationSettlementConversion, "fee correction holds: "+detail, now)
			}
		} else {
			return bc.correctPairLiveFees(rt, a, s, pl, resolved, settleAt, in)
		}
	}
	rt.touchPairProgress()
	bc.persistPair(rt)
	return false
}

func (bc *basketCoordinator) bookPairLiveQuantity(rt *basketRuntime, a *basketAction, s *StrategyState, m pairContractMember, pl pairSettlePlan, resolved map[pairConvNeed]pairResolvedConv, settleAt int64, in basketCycleInputs) bool {
	now := in.Now
	p := &rt.Pair.Progress
	sc := bc.members[a.StrategyID]
	remaining := a.ConfirmedQty - a.AppliedQty
	dn := a.ConfirmedNotional - a.AppliedNotional
	var usd, refQty float64
	var newFills []pairLedgerFill
	var parts []pairLedgerFee
	if pl.remainder {
		usdTotal, nativeTotal := 0.0, 0.0
		for _, f := range pl.ledger.Fills {
			usdTotal += f.NotionalUSD
			if r, err := pairCoverageRat("native_notional", f.NativeNotional, true); err == nil {
				nativeTotal += pairRatFloat(r)
			}
		}
		if !(nativeTotal > 0) || !(dn > 0) {
			bc.holdPairAction(rt, a, "the unbooked remainder has no saved fill conversion to price it", true, now)
			return false
		}
		usd, refQty = dn*usdTotal/nativeTotal, remaining
	} else {
		native, usdR, qty := new(big.Rat), new(big.Rat), new(big.Rat)
		for _, f := range pl.dueFills {
			t := pl.dueTimes[f.FillID]
			n, err := pairCoverageRat("native_notional", f.NativeNotional, true)
			if err != nil {
				bc.holdPairAction(rt, a, "a saved execution record is unreadable: "+err.Error(), true, now)
				return false
			}
			q, err := pairCoverageRat("base_qty", f.BaseQty, true)
			if err != nil {
				bc.holdPairAction(rt, a, "a saved execution record is unreadable: "+err.Error(), true, now)
				return false
			}
			c := resolved[pairConvNeed{Currency: m.Meta.PriceCurrency, AtMs: t.AtMs}]
			v := new(big.Rat).Mul(n, c.Rate)
			usdR.Add(usdR, v)
			native.Add(native, n)
			qty.Add(qty, q)
			newFills = append(newFills, pairLedgerFill{ScopedFillID: pairScopedFillID(a, f.FillID), FillID: f.FillID, EvidenceHash: f.EvidenceHash, SelectedTimeMs: t.AtMs,
				TimeSource: t.Source, SettlementAtMs: settleAt, BaseQty: f.BaseQty, NativeNotional: f.NativeNotional, PriceCurrency: m.Meta.PriceCurrency,
				Conversion: c.Evidence, RateUSD: c.RateText, NotionalUSD: pairRatFloat(v)})
		}
		if a.PriceBound > 0 {
			npx := pairRatFloat(new(big.Rat).Quo(native, qty))
			if (a.OrderSide == "buy" && npx > a.PriceBound*(1+1e-9)) || (a.OrderSide == "sell" && npx < a.PriceBound*(1-1e-9)) {
				bc.holdPairAction(rt, a, fmt.Sprintf("the incremental native fill price %v crosses the saved %s bound %v", npx, a.OrderSide, a.PriceBound), true, now)
				return false
			}
		}
		usd, refQty = pairRatFloat(usdR), pairRatFloat(qty)
	}
	px := usd / refQty
	if !basketFinitePositive(px) {
		bc.holdPairAction(rt, a, fmt.Sprintf("the incremental fill price %v is not finite and positive", px), true, now)
		return false
	}
	for _, f := range pl.dueFees {
		t := pl.dueTimes[f.FillID]
		amt, err := pairCoverageRat("fee_amount", f.FeeAmount, false)
		if err != nil {
			bc.holdPairAction(rt, a, "a saved fee record is unreadable: "+err.Error(), true, now)
			return false
		}
		c := resolved[pairConvNeed{Currency: f.FeeCurrency, AtMs: t.AtMs}]
		parts = append(parts, pairLedgerFee{ScopedFillID: pairScopedFillID(a, f.FillID), FillID: f.FillID, EvidenceHash: f.EvidenceHash, SelectedTimeMs: t.AtMs,
			SettlementAtMs: settleAt, FeeCurrency: f.FeeCurrency, FeeAmount: f.FeeAmount, Conversion: c.Evidence, RateUSD: c.RateText,
			FeeUSD: pairRatFloat(new(big.Rat).Mul(amt, c.Rate)), Mode: pairFeeModeQuantity})
	}
	fee := pairConfirmedFeeOf(a, parts)
	logger := bc.memberLogger(a.StrategyID)
	defer logger.Close()
	obs := bc.pairObservation(rt, a.StrategyID, a)
	from := len(s.TradeHistory)
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	var booked basketBooking
	withStrategyBookClock(s, now, func() {
		booked = bookPairFill(sc, s, bc.cfg, a, remaining, px, fee, obs, logger)
	})
	s.deferTradePersist = prevDefer
	if !(booked.Qty > 0) {
		reason := booked.Hold
		if reason == "" {
			reason = "the booking owner recorded no fill"
		}
		bc.holdPairAction(rt, a, "a confirmed fill could not be booked: "+reason, true, now)
		return false
	}
	trades := append([]Trade(nil), s.TradeHistory[from:]...)
	bookedRows := a.bookedTrades()
	var link pairLedgerRow
	for _, t := range trades {
		if t.Symbol != a.Symbol || t.TradeType == TradeTypeFunding {
			continue
		}
		row := pairBookedTrade{TradeTsMs: t.Timestamp.UTC().UnixMilli(), TradeTs: formatTime(t.Timestamp), Qty: t.Quantity, Px: t.Price, FeeUSD: t.ExchangeFee, OrderID: t.ExchangeOrderID, IsClose: t.IsClose}
		if t.IsClose && s.Positions[a.Symbol] == nil {
			row.ClosedPositionID = t.PositionID
		}
		bookedRows = append(bookedRows, row)
		link = pairLedgerRowFor(s, t, a.Symbol)
	}
	for i := range newFills {
		newFills[i].Booking = link
	}
	for i := range parts {
		parts[i].Target = link
	}
	ledger := pl.ledger
	ledger.Fills = append(append([]pairLedgerFill(nil), ledger.Fills...), newFills...)
	ledger.Fees = append(append([]pairLedgerFee(nil), ledger.Fees...), parts...)
	a.SettlementJSON = ledger.json()
	a.AppliedQty += booked.Qty
	a.AppliedNotional += dn * booked.Qty / remaining
	a.AppliedFee += booked.Fee
	a.AppliedFeeVersion = int64(len(ledger.Fees))
	if b, err := json.Marshal(bookedRows); err == nil {
		a.BookedJSON = string(b)
	}
	rt.Expected[a.StrategyID] += a.signedDelta() * booked.Qty
	if math.Abs(rt.Expected[a.StrategyID]) < 1e-12 {
		rt.Expected[a.StrategyID] = 0
	}
	if a.Direction == basketDirectionIncrease && p.FirstFillMs == 0 {
		p.FirstFillMs = now.UnixMilli()
	}
	for _, u := range rt.Pair.Uses {
		if u.ActionID == a.ActionID && u.Status == pairUseReserved {
			u.Status = pairUseConsumed
			u.UpdatedAtMs = now.UnixMilli()
		}
	}
	if in := bc.intentFor(s, a); in != nil {
		in.ConfirmedQty, in.ConfirmedNotional, in.ConfirmedFee = a.ConfirmedQty, a.ConfirmedNotional, a.ConfirmedFee
		in.AppliedQty, in.AppliedNotional, in.AppliedFee = a.AppliedQty, a.AppliedNotional, a.AppliedFee
		in.SettlementSeq++
		in.VenueOrderID = a.VenueOrderID
		in.EvidenceJSON = a.EvidenceJSON
		in.HoldReason = ""
		in.UpdatedAtMs = now.UnixMilli()
		if a.Terminal && basketQtyEqual(a.AppliedQty, a.ConfirmedQty) {
			in.Status = entryIntentCommitted
		} else {
			in.Status = entryIntentSettling
		}
	}
	bc.clearPairQuantityHolds(rt, a)
	bc.pairLogf(rt, "action %s booked %s %.10g %s @ $%.6f with %d confirmed fee records ($%.6f) and %d saved fill conversions", a.ActionID, a.OrderSide, booked.Qty, a.Symbol, booked.Px, len(parts), booked.Fee, len(newFills))
	if bc.notifier != nil && len(trades) > 0 {
		notifier, regime := bc.notifier, bc.cfg.Regime
		bc.outbox = append(bc.outbox, func() { sendTradeAlertRows(sc, trades, notifier, regime) })
	}
	return bc.savePairLiveSettlement(rt, a, now)
}

type pairFeeFixGroup struct {
	row   pairLedgerRow
	delta float64
	fix   *pairFeeFix
	trade *Trade
}

func (bc *basketCoordinator) correctPairLiveFees(rt *basketRuntime, a *basketAction, s *StrategyState, pl pairSettlePlan, resolved map[pairConvNeed]pairResolvedConv, settleAt int64, in basketCycleInputs) bool {
	now := in.Now
	var order []string
	groups := map[string]*pairFeeFixGroup{}
	var parts []pairLedgerFee
	total := 0.0
	for _, f := range pl.fixFees {
		lf := pl.booked[f.FillID]
		amt, err := pairCoverageRat("fee_amount", f.FeeAmount, false)
		if err != nil {
			bc.holdPairAction(rt, a, "a saved fee record is unreadable: "+err.Error(), true, now)
			return false
		}
		c := resolved[pairConvNeed{Currency: f.FeeCurrency, AtMs: lf.SelectedTimeMs}]
		v := pairRatFloat(new(big.Rat).Mul(amt, c.Rate))
		parts = append(parts, pairLedgerFee{ScopedFillID: pairScopedFillID(a, f.FillID), FillID: f.FillID, EvidenceHash: f.EvidenceHash, SelectedTimeMs: lf.SelectedTimeMs,
			SettlementAtMs: settleAt, FeeCurrency: f.FeeCurrency, FeeAmount: f.FeeAmount, Conversion: c.Evidence, RateUSD: c.RateText, FeeUSD: v,
			Mode: pairFeeModeCorrection, Target: lf.Booking})
		key := fmt.Sprintf("%d|%s|%s", lf.Booking.TradeTsMs, lf.Booking.TradeTs, lf.Booking.OrderID)
		g := groups[key]
		if g == nil {
			g = &pairFeeFixGroup{row: lf.Booking}
			groups[key] = g
			order = append(order, key)
		}
		g.delta += v
		total += v
	}
	newApplied := a.AppliedFee + total
	for _, key := range order {
		g := groups[key]
		fix := &pairFeeFix{TradeTsMs: g.row.TradeTsMs, TradeTs: g.row.TradeTs, Symbol: a.Symbol, OrderID: g.row.OrderID, DeltaUSD: g.delta, Version: a.FeeEvidenceVersion,
			ClosedPositionID: g.row.ClosedPositionID, IsClose: g.row.IsClose, IntentID: a.EntryIntentID, ConfirmedFee: a.ConfirmedFee, AppliedFeeUSD: newApplied}
		retained := pairRetainedTrade(s, fix)
		if retained != nil && !retained.persisted {
			bc.pairLogf(rt, "action %s fee correction waits until its booked trade row is durable", a.ActionID)
			return false
		}
		stamp, err := bc.pairFeeFixTarget(rt, s, a, fix, retained)
		if err != nil {
			if !errors.Is(err, errPairFeeFixProvenance) && !errors.Is(err, errPairCostOwnership) {
				bc.pairLogf(rt, "action %s fee correction waits: its saved target row could not be read (%v)", a.ActionID, err)
				return false
			}
			bc.holdPairFeeFixProvenance(rt, a, fmt.Sprintf("the fee correction target row is refused before any change: %v", err), now)
			return false
		}
		fix.TradeRowID, fix.CostModelVersion, fix.CostPolicyID = stamp.RowID, stamp.ModelVersion, stamp.PolicyID
		g.fix, g.trade = fix, retained
	}
	undo := &pairFeeFixUndo{prevApplied: a.AppliedFee, prevAppliedVersion: a.AppliedFeeVersion, prevBooked: a.BookedJSON, prevLedger: a.SettlementJSON,
		prevStatus: a.Status, prevHold: a.HoldReason, prevUpdatedAtMs: a.UpdatedAtMs}
	rows := a.bookedTrades()
	for _, key := range order {
		g := groups[key]
		part := pairFeeFixPart{fix: *g.fix, delta: g.delta}
		if g.trade != nil {
			part.tradeFound, part.prevFeeSource = true, g.trade.FeeSource
			g.trade.ExchangeFee += g.delta
			g.trade.FeeSource = FeeSourceUserFills
		}
		s.Cash -= g.delta
		undo.cash += g.delta
		if g.row.IsClose {
			rolloverDailyPnL(&s.RiskState)
			s.RiskState.DailyPnL -= g.delta
			undo.daily += g.delta
			undo.dailyApplied, undo.dailyDate = true, s.RiskState.DailyPnLDate
		}
		for i := range rows {
			if rows[i].TradeTsMs == g.row.TradeTsMs && rows[i].OrderID == g.row.OrderID && (g.row.TradeTs == "" || rows[i].TradeTs == "" || rows[i].TradeTs == g.row.TradeTs) {
				rows[i].FeeUSD += g.delta
				break
			}
		}
		undo.parts = append(undo.parts, part)
		a.pendingFee = append(a.pendingFee, g.fix)
	}
	if in := bc.intentFor(s, a); in != nil {
		undo.intentFound, undo.prevIntentApplied, undo.prevIntentConfirmed = true, in.AppliedFee, in.ConfirmedFee
		in.AppliedFee, in.ConfirmedFee = newApplied, a.ConfirmedFee
	}
	if b, err := json.Marshal(rows); err == nil {
		a.BookedJSON = string(b)
	}
	ledger := pl.ledger
	ledger.Fees = append(append([]pairLedgerFee(nil), ledger.Fees...), parts...)
	a.SettlementJSON = ledger.json()
	a.AppliedFee = newApplied
	a.AppliedFeeVersion = int64(len(ledger.Fees))
	a.feeFixUndo = undo
	if !pl.qtyDue {
		bc.clearPairQuantityHolds(rt, a)
	}
	bc.pairLogf(rt, "action %s fee evidence %s version %d applied %d newly covered fee records as a signed delta of $%.6f across %d booked rows once", a.ActionID, a.FeeEvidenceID, a.FeeEvidenceVersion, len(parts), total, len(order))
	return bc.savePairLiveSettlement(rt, a, now)
}

func (bc *basketCoordinator) savePairLiveSettlement(rt *basketRuntime, a *basketAction, now time.Time) bool {
	switch {
	case a.ConfirmedQty-a.AppliedQty > 1e-12:
		a.Status = basketActionSettling
	case bc.pairActionComplete(a):
		a.Status = basketActionCommitted
	default:
		a.Status = basketActionUnresolved
	}
	if a.Status == basketActionCommitted {
		rt.Pair.Progress.clearObligation(pairObligationFeeIncomplete, a.StrategyID, a.ActionID)
	} else {
		bc.refreshPairFeeIncomplete(rt, a, now)
	}
	a.UpdatedAtMs = now.UnixMilli()
	if u := a.feeFixUndo; u != nil {
		u.stagedStatus, u.stagedHold, u.stagedUpdatedAtMs = a.Status, a.HoldReason, a.UpdatedAtMs
	}
	rt.touchPairProgress()
	if err := bc.store.SavePartition(bc.state, rt.Partition); err != nil {
		if bc.rollbackPairFeeFixes(rt, err, now) {
			if err2 := bc.store.SavePartition(bc.state, rt.Partition); err2 != nil {
				rt.needsDurable = true
				bc.pairLogf(rt, "the refused fee correction was undone, but the %s state file still did not save: %v; no elective order is sent until it does", partitionLabel(rt.Partition), err2)
			}
			return false
		}
		rt.needsDurable = true
		bc.pairLogf(rt, "action %s settled in memory, but the %s state file did not save: %v; no elective order is sent until it does, and a restart settles the confirmed evidence once from the saved records", a.ActionID, partitionLabel(rt.Partition), err)
		return false
	}
	return true
}
