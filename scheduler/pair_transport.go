package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	pairOrderFlag         = "--pair-order-json"
	pairQueryFlag         = "--pair-query-json"
	pairPositionFlag      = "--pair-position-json"
	pairOrderScriptTimout = 30 * time.Second
	pairQueryFillMarginMs = 60000
	pairOutcomeVersion    = 1
	pairPositionReqV      = 1
)

type pairAccountReader interface {
	Account(ctx context.Context, m pairContractMember, refMs int64) (pairAccountEvidence, error)
}

type pairLiveShapedEvidenceSource struct {
	frozen   *pairFrozenEvidenceSource
	accounts pairAccountReader
}

func (s *pairLiveShapedEvidenceSource) Capability(ctx context.Context, m pairContractMember, refMs int64) (pairCapabilityEvidence, error) {
	return s.frozen.Capability(ctx, m, refMs)
}

func (s *pairLiveShapedEvidenceSource) Conversions(ctx context.Context, m pairContractMember, refMs int64) ([]pairConversionEvidence, error) {
	return s.frozen.Conversions(ctx, m, refMs)
}

func (s *pairLiveShapedEvidenceSource) Account(ctx context.Context, m pairContractMember, refMs int64) (pairAccountEvidence, error) {
	key, err := s.frozen.key(ctx, m)
	if err != nil {
		return pairAccountEvidence{}, err
	}
	a, err := s.accounts.Account(ctx, m, refMs)
	if err != nil {
		return pairAccountEvidence{}, fmt.Errorf("the controlled account reader: %w", err)
	}
	if a.Key != key {
		return pairAccountEvidence{}, fmt.Errorf("the controlled account record names %s, not %s", a.Key, key)
	}
	if err := pairAccountMatches(m, a); err != nil {
		return pairAccountEvidence{}, err
	}
	if !pairEligibleAt(a.pairEvidenceStamp, refMs) {
		return pairAccountEvidence{}, fmt.Errorf("the controlled account record %s is not eligible at %d (observed %d, received %d, valid %d..%d)", a.EvidenceID, refMs, a.ObservedMs, a.FirstReceiptMs, a.ValidFromMs, a.ValidThroughMs)
	}
	return a, nil
}

type pairLiveTransport struct {
	Script         string
	PositionScript string
	Partition      RiskPartition
	Clock          func() time.Time
	Substituted    bool
}

type pairLiveBase struct {
	provider string
	account  string
	m        pairContractMember
	t        pairLiveTransport
}

type hyperliquidPairVenue struct{ pairLiveBase }

type okxPairVenue struct{ pairLiveBase }

func newPairLiveBase(adm *pairResearchAdmission, provider string, m pairContractMember, t pairLiveTransport) (pairLiveBase, error) {
	account := m.AccountKey
	if m.Provider != provider {
		return pairLiveBase{}, fmt.Errorf("member %s is a %s member, not %s", m.MemberID, m.Provider, provider)
	}
	if !adm.valid() || adm.kind != pairAdmissionLiveShapedFixture {
		return pairLiveBase{}, fmt.Errorf("a live-shaped %s pair transport is constructed only by the controlled live-shaped fixture; production pair dispatch is refused", provider)
	}
	if !t.Substituted || t.Clock == nil || strings.TrimSpace(t.Script) == "" || strings.TrimSpace(t.PositionScript) == "" {
		return pairLiveBase{}, fmt.Errorf("the live-shaped %s pair transport needs every substituted dependency (driver scripts and clock) before construction", provider)
	}
	for _, prod := range []string{hyperliquidCheckScript, okxCheckScriptPath, okxFetchPositionsScript} {
		if filepath.Clean(t.Script) == prod || filepath.Clean(t.PositionScript) == prod {
			return pairLiveBase{}, fmt.Errorf("the live-shaped %s pair transport refuses the production script %s; the fixture runs a substituted driver", provider, prod)
		}
	}
	if !t.Partition.IsLive() {
		return pairLiveBase{}, fmt.Errorf("the live-shaped %s pair transport binds the live partition", provider)
	}
	if why := pairAccountKeyError(provider, account, true); why != "" || strings.HasPrefix(account, pairSyntheticAccountPrefix) {
		return pairLiveBase{}, fmt.Errorf("the live-shaped %s pair transport needs the venue account format: %s", provider, why)
	}
	return pairLiveBase{provider: provider, account: account, m: m, t: t}, nil
}

func newHyperliquidPairVenue(adm *pairResearchAdmission, m pairContractMember, t pairLiveTransport) (*hyperliquidPairVenue, error) {
	b, err := newPairLiveBase(adm, entryProviderHyperliquid, m, t)
	if err != nil {
		return nil, err
	}
	return &hyperliquidPairVenue{b}, nil
}

func newOKXPairVenue(adm *pairResearchAdmission, m pairContractMember, t pairLiveTransport) (*okxPairVenue, error) {
	b, err := newPairLiveBase(adm, entryProviderOKX, m, t)
	if err != nil {
		return nil, err
	}
	return &okxPairVenue{b}, nil
}

func (v *pairLiveBase) Name() string                    { return v.provider + "-pair" }
func (v *pairLiveBase) LiveShaped() bool                { return true }
func (v *pairLiveBase) Virtual() bool                   { return false }
func (v *pairLiveBase) ExecutionModel() string          { return pairExecutionVenueConfirmed }
func (v *pairLiveBase) Provider() string                { return v.provider }
func (v *pairLiveBase) AccountKey(RiskPartition) string { return v.account }

func (v *hyperliquidPairVenue) NewClientOrderID() (string, error) { return newQuoteClientOrderID() }

func (v *okxPairVenue) NewClientOrderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (v *pairLiveBase) nowMs() int64 { return v.t.Clock().UnixMilli() }

type pairScriptFill struct {
	FillID         string `json:"fill_id"`
	VenueTimeMs    int64  `json:"venue_time_ms"`
	NativeQty      string `json:"native_qty"`
	NativePx       string `json:"native_px"`
	NativeNotional string `json:"native_notional"`
	FeeKnown       bool   `json:"fee_known"`
	FeeAmount      string `json:"fee_amount"`
	FeeCurrency    string `json:"fee_currency"`
	OrderID        string `json:"order_id"`
	Side           string `json:"side"`
}

type pairScriptVenue struct {
	AccountKey    string `json:"account_key"`
	Instrument    string `json:"instrument"`
	ClientOrderID string `json:"client_order_id"`
	VenueOrderID  string `json:"venue_order_id"`
	Side          string `json:"side"`
	NativeQty     string `json:"native_qty"`
	Price         string `json:"price"`
	Tif           string `json:"tif"`
	Status        string `json:"status"`
	ReduceOnly    *bool  `json:"reduce_only"`
	ContractSize  string `json:"contract_size"`
	PriceCurrency string `json:"price_currency"`
	NativeUnit    string `json:"native_unit"`
}

type pairScriptRequestEcho struct {
	ClientOrderID    string `json:"client_order_id"`
	AccountKey       string `json:"account_key"`
	Instrument       string `json:"instrument"`
	OrderSide        string `json:"order_side"`
	Purpose          string `json:"purpose"`
	ReduceOnly       bool   `json:"reduce_only"`
	NativeQty        string `json:"native_qty"`
	PriceBound       string `json:"price_bound"`
	AcceptDeadlineMs int64  `json:"accept_deadline_ms"`
}

type pairScriptOutcome struct {
	V                 int                    `json:"v"`
	Mode              string                 `json:"mode"`
	Status            string                 `json:"status"`
	Detail            string                 `json:"detail"`
	ObservedAtMs      int64                  `json:"observed_at_ms"`
	Terminal          bool                   `json:"terminal"`
	AcceptedAtMs      int64                  `json:"accepted_at_ms"`
	CumNativeQty      string                 `json:"cum_native_qty"`
	CumNativeNotional string                 `json:"cum_native_notional"`
	Fills             []pairScriptFill       `json:"fills"`
	FillCoverage      quoteFillCoverage      `json:"fill_coverage"`
	Venue             pairScriptVenue        `json:"venue"`
	Request           *pairScriptRequestEcho `json:"request"`
	ContraryEvidence  bool                   `json:"contrary_evidence"`
}

type pairScriptOutput struct {
	Platform string             `json:"platform"`
	Account  string             `json:"account"`
	Error    string             `json:"error"`
	Outcome  *pairScriptOutcome `json:"pair_outcome"`
	Position *pairScriptPos     `json:"pair_position"`

	NativeHistory *pairNativeHistoryOutput `json:"native_history"`
}

type pairScriptPos struct {
	V               int    `json:"v"`
	AccountKey      string `json:"account_key"`
	Instrument      string `json:"instrument"`
	NativeUnit      string `json:"native_unit"`
	ContractSize    string `json:"contract_size"`
	SignedNativeQty string `json:"signed_native_qty"`
	PositionMode    string `json:"position_mode"`
	MarginMode      string `json:"margin_mode"`
	Found           bool   `json:"found"`
	ObservedMs      int64  `json:"observed_ms"`
}

func pairParseScriptOutput(stdout, stderr []byte, runErr error) (pairScriptOutput, error) {
	var out pairScriptOutput
	parseErr := json.Unmarshal(stdout, &out)
	var timeoutErr *pythonScriptTimeoutError
	if errors.As(runErr, &timeoutErr) {
		return out, fmt.Errorf("the pair venue script timed out: %w", runErr)
	}
	if parseErr != nil {
		return out, fmt.Errorf("the pair venue script output is unreadable: %v (stderr: %s)", parseErr, strings.TrimSpace(string(stderr)))
	}
	return out, nil
}

func pairNativeUnitFor(provider string) string {
	if provider == entryProviderOKX {
		return "contract"
	}
	return "base"
}

func pairDecimalEqualsFloat(text string, f float64) bool {
	a, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok || strings.TrimSpace(text) == "" {
		return false
	}
	b, err := quoteRatFromFloatDecimal(f)
	return err == nil && a.Cmp(b) == 0
}

func pairActionRequestOf(a *basketAction) (pairActionRequest, error) {
	var req pairActionRequest
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err != nil {
		return req, fmt.Errorf("the saved request is unreadable: %w", err)
	}
	return req, nil
}

func pairCheckScriptEcho(provider string, a *basketAction, req pairActionRequest, o pairScriptOutcome, m pairContractMember) string {
	var bad []string
	if o.Venue.AccountKey != a.AccountKey {
		bad = append(bad, fmt.Sprintf("account %q", o.Venue.AccountKey))
	}
	if o.Request == nil || o.Request.ClientOrderID != a.ClientOrderID || o.Request.AccountKey != a.AccountKey || o.Request.Instrument != a.Instrument ||
		o.Request.OrderSide != a.OrderSide || o.Request.Purpose != a.Purpose || o.Request.ReduceOnly != req.ReduceOnly || o.Request.AcceptDeadlineMs != a.AcceptDeadlineMs ||
		!pairDecimalEqualsFloat(o.Request.NativeQty, req.NativeQty) || !pairDecimalEqualsFloat(o.Request.PriceBound, req.PriceBound) {
		bad = append(bad, "request echo")
	}
	cs, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil || o.Venue.ContractSize != string(cs) {
		bad = append(bad, fmt.Sprintf("contract size %q", o.Venue.ContractSize))
	}
	if o.Venue.PriceCurrency != m.Meta.PriceCurrency {
		bad = append(bad, fmt.Sprintf("price currency %q", o.Venue.PriceCurrency))
	}
	if o.Venue.NativeUnit != pairNativeUnitFor(provider) {
		bad = append(bad, fmt.Sprintf("native unit %q", o.Venue.NativeUnit))
	}
	recorded := o.Venue.ClientOrderID != "" || o.Venue.VenueOrderID != ""
	if !recorded {
		if o.Status != "rejected" || o.Terminal != true || o.CumNativeQty != "0" || len(o.Fills) > 0 {
			bad = append(bad, "an outcome with fills or acceptance has no venue order record")
		}
	} else {
		wantTif := "Ioc"
		if provider == entryProviderOKX {
			wantTif = "ioc"
		}
		if o.Venue.Instrument != a.Instrument {
			bad = append(bad, fmt.Sprintf("instrument %q", o.Venue.Instrument))
		}
		if o.Venue.ClientOrderID != a.ClientOrderID {
			bad = append(bad, fmt.Sprintf("client order id %q", o.Venue.ClientOrderID))
		}
		if o.Venue.Side != a.OrderSide {
			bad = append(bad, fmt.Sprintf("side %q", o.Venue.Side))
		}
		if !pairDecimalEqualsFloat(o.Venue.NativeQty, req.NativeQty) {
			bad = append(bad, fmt.Sprintf("native size %q", o.Venue.NativeQty))
		}
		if !pairDecimalEqualsFloat(o.Venue.Price, req.PriceBound) {
			bad = append(bad, fmt.Sprintf("price %q", o.Venue.Price))
		}
		if o.Venue.ReduceOnly == nil || *o.Venue.ReduceOnly != req.ReduceOnly {
			bad = append(bad, "reduce-only flag")
		}
		if o.Venue.Tif != wantTif {
			bad = append(bad, fmt.Sprintf("time in force %q", o.Venue.Tif))
		}
		if a.VenueOrderID != "" && o.Venue.VenueOrderID != a.VenueOrderID {
			bad = append(bad, fmt.Sprintf("venue order %q", o.Venue.VenueOrderID))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Strings(bad)
	return "the venue evidence does not match the saved request (" + strings.Join(bad, ", ") + ")"
}

func pairScaleDecimal(text string, cs *big.Rat, what string, positive bool) (*big.Rat, string, error) {
	r, err := pairCoverageRat(what, text, positive)
	if err != nil {
		return nil, "", err
	}
	scaled := new(big.Rat).Mul(r, cs)
	s, err := pairRatDecimal(scaled)
	if err != nil {
		return nil, "", fmt.Errorf("%s %s times the contract size is not a finite decimal", what, text)
	}
	return scaled, s, nil
}

func pairMapScriptOutcome(provider string, a *basketAction, m pairContractMember, o pairScriptOutcome) basketVenueOutcome {
	unknown := func(detail string) basketVenueOutcome {
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: detail, ObservedAtMs: o.ObservedAtMs}
	}
	if o.V != pairOutcomeVersion {
		return unknown(fmt.Sprintf("the pair outcome version %d is not %d", o.V, pairOutcomeVersion))
	}
	req, err := pairActionRequestOf(a)
	if err != nil {
		return unknown(err.Error())
	}
	switch o.Status {
	case pairVenueNeverSent:
		if o.CumNativeQty != "0" || len(o.Fills) > 0 {
			return unknown("a never-sent answer carries fill evidence")
		}
		return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: o.Detail, ObservedAtMs: o.ObservedAtMs}
	case basketVenueUnknown:
		return unknown(o.Detail)
	case pairVenueAbsent:
		if o.Venue.AccountKey != a.AccountKey || o.CumNativeQty != "0" || len(o.Fills) > 0 {
			return unknown("an absent answer names another account or carries fill evidence")
		}
		return basketVenueOutcome{Outcome: pairVenueAbsent, Detail: o.Detail, ObservedAtMs: o.ObservedAtMs, CoverageComplete: o.FillCoverage.Complete,
			CoverageThroughMs: o.FillCoverage.ThroughMs, ContraryEvidence: o.ContraryEvidence, AccountKey: a.AccountKey}
	case basketVenueFilled, basketVenuePartial, basketVenueRejected, pairVenueOpen:
	default:
		return unknown(fmt.Sprintf("the pair outcome status %q is not known", o.Status))
	}
	if why := pairCheckScriptEcho(provider, a, req, o, m); why != "" {
		return unknown(why)
	}
	csDec, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		return unknown(err.Error())
	}
	cs, _ := new(big.Rat).SetString(string(csDec))
	cumQty, _, err := pairScaleDecimal(o.CumNativeQty, cs, "cumulative native quantity", false)
	if err != nil {
		return unknown(err.Error())
	}
	cumNotional, _, err := pairScaleDecimal(o.CumNativeNotional, cs, "cumulative native notional", false)
	if err != nil {
		return unknown(err.Error())
	}
	out := basketVenueOutcome{
		Outcome: o.Status, Detail: o.Detail, ObservedAtMs: o.ObservedAtMs, ClientOrderID: a.ClientOrderID, AccountKey: o.Venue.AccountKey,
		Instrument: a.Instrument, Side: a.OrderSide, VenueOrderID: o.Venue.VenueOrderID, AcceptedAtMs: o.AcceptedAtMs, Terminal: o.Terminal,
		CumQty: pairRatFloat(cumQty), CumNotional: pairRatFloat(cumNotional), CoverageComplete: o.FillCoverage.Complete, CoverageThroughMs: o.FillCoverage.ThroughMs,
	}
	if out.Outcome == basketVenueRejected && out.CumQty == 0 {
		out.AcceptedAtMs = 0
	}
	sumQty, sumNotional := new(big.Rat), new(big.Rat)
	allFees := len(o.Fills) > 0
	seen := map[string]bool{}
	for _, f := range o.Fills {
		if seen[f.FillID] {
			return unknown(fmt.Sprintf("fill %s appears twice", f.FillID))
		}
		seen[f.FillID] = true
		q, qs, err := pairScaleDecimal(f.NativeQty, cs, "fill native quantity", true)
		if err != nil {
			return unknown(err.Error())
		}
		n, ns, err := pairScaleDecimal(f.NativeNotional, cs, "fill native notional", true)
		if err != nil {
			return unknown(err.Error())
		}
		sumQty.Add(sumQty, q)
		sumNotional.Add(sumNotional, n)
		rec, err := pairConfirmedFill{FillID: f.FillID, VenueTimeMs: f.VenueTimeMs, BaseQty: qs, NativeNotional: ns}.sealed()
		if err != nil {
			return unknown(err.Error())
		}
		out.FillCoverage = append(out.FillCoverage, rec)
		if !f.FeeKnown {
			allFees = false
			continue
		}
		amount, err := pairNormalizeDecimal(f.FeeAmount)
		if err != nil {
			return unknown(fmt.Sprintf("fill %s fee %q: %v", f.FillID, f.FeeAmount, err))
		}
		fee, err := pairFeeFill{FillID: f.FillID, VenueTimeMs: f.VenueTimeMs, BaseQty: qs, NativeNotional: ns, FeeCurrency: strings.ToUpper(strings.TrimSpace(f.FeeCurrency)), FeeAmount: amount}.sealed()
		if err != nil {
			return unknown(err.Error())
		}
		out.FeeCoverage = append(out.FeeCoverage, fee)
	}
	if sumQty.Cmp(cumQty) != 0 || sumNotional.Cmp(cumNotional) != 0 {
		return unknown("the fill records do not sum to the cumulative quantity and notional")
	}
	if len(out.FeeCoverage) > 0 {
		out.FeePresent = true
		out.FeeEvidenceVersion = int64(len(out.FeeCoverage))
		out.FeeEvidenceID = pairFeeEvidenceID(a)
	}
	out.FeeFinal = out.Terminal && out.CoverageComplete && allFees && sumQty.Sign() > 0
	return out
}

func (v *pairLiveBase) submit(a *basketAction, m pairContractMember) (basketVenueOutcome, error) {
	if !pairBeforeDeadline(v.nowMs(), a.AcceptDeadlineMs) {
		return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: "the acceptance deadline passed before the order script launched; nothing was sent", ObservedAtMs: v.nowMs()}, nil
	}
	if why := pairReductionAgeRefusal(a.RequestJSON, v.nowMs()); why != "" {
		return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: why + "; the order script was not launched and nothing was sent", ObservedAtMs: v.nowMs()}, nil
	}
	wire, err := pairTransportRequestJSON(a.RequestJSON)
	if err != nil {
		return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: err.Error() + "; nothing was sent", ObservedAtMs: v.nowMs()}, nil
	}
	args := []string{pairOrderFlag + "=" + wire, "--mode=live"}
	stdout, stderr, runErr := runPythonSideEffectWithTimeout(v.t.Script, args, pairOrderScriptTimout)
	out, perr := pairParseScriptOutput(stdout, stderr, runErr)
	if perr != nil || out.Outcome == nil {
		why := "the order script returned no pair outcome"
		if perr != nil {
			why = perr.Error()
		}
		if out.Error != "" {
			why += ": " + out.Error
		}
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: why, ObservedAtMs: v.nowMs()}, nil
	}
	if out.Outcome.Mode != "order" {
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "the order script answered in mode " + out.Outcome.Mode, ObservedAtMs: v.nowMs()}, nil
	}
	return pairMapScriptOutcome(v.provider, a, m, *out.Outcome), nil
}

func (v *pairLiveBase) query(ctx context.Context, a *basketAction, m pairContractMember) (basketVenueOutcome, error) {
	since := a.CreatedAtMs - pairQueryFillMarginMs
	if a.SubmittedAtMs > 0 && a.SubmittedAtMs-pairQueryFillMarginMs < since {
		since = a.SubmittedAtMs - pairQueryFillMarginMs
	}
	if since <= 0 {
		since = 1
	}
	wire, err := pairTransportRequestJSON(a.RequestJSON)
	if err != nil {
		return basketVenueOutcome{}, err
	}
	args := []string{pairQueryFlag + "=" + wire, fmt.Sprintf("--since-ms=%d", since), "--mode=live"}
	out, err := v.read(ctx, v.t.Script, args)
	if err != nil {
		return basketVenueOutcome{}, err
	}
	if out.Outcome == nil || out.Outcome.Mode != "query" {
		return basketVenueOutcome{}, fmt.Errorf("the query script returned no query outcome: %s", out.Error)
	}
	return pairMapScriptOutcome(v.provider, a, m, *out.Outcome), nil
}

func (v *pairLiveBase) read(ctx context.Context, script string, args []string) (pairScriptOutput, error) {
	release, err := acquirePythonSlot(ctx)
	if err != nil {
		return pairScriptOutput{}, fmt.Errorf("waiting for a script slot: %w", err)
	}
	defer release()
	timeout := pairOrderScriptTimout
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
		if timeout <= 0 {
			return pairScriptOutput{}, context.DeadlineExceeded
		}
	}
	stdout, stderr, runErr := spawnPythonProcess(ctx, script, args, nil, timeout)
	out, perr := pairParseScriptOutput(stdout, stderr, runErr)
	if perr != nil {
		return out, perr
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s", out.Error)
	}
	if runErr != nil {
		return out, fmt.Errorf("the pair venue script failed: %w", runErr)
	}
	return out, nil
}

func (v *pairLiveBase) position(ctx context.Context, m pairContractMember, script string) (pairVenuePosition, error) {
	if m.Provider != v.provider || m.AccountKey != v.account {
		return pairVenuePosition{}, fmt.Errorf("member %s is not bound to this %s transport account", m.MemberID, v.provider)
	}
	req, err := json.Marshal(map[string]interface{}{"v": pairPositionReqV, "provider": m.Provider, "account_key": m.AccountKey, "instrument": m.Instrument, "member_id": m.MemberID, "partition": v.t.Partition.String()})
	if err != nil {
		return pairVenuePosition{}, err
	}
	out, err := v.read(ctx, script, []string{pairPositionFlag + "=" + string(req), "--mode=live"})
	if err != nil {
		return pairVenuePosition{}, err
	}
	receipt := v.nowMs()
	p := out.Position
	if p == nil {
		return pairVenuePosition{}, fmt.Errorf("the position script returned no strict position")
	}
	if p.V != 1 || p.AccountKey != m.AccountKey || p.Instrument != m.Instrument || p.NativeUnit != pairNativeUnitFor(m.Provider) {
		return pairVenuePosition{}, fmt.Errorf("the position script answered for %s %s in %s units, not %s %s", p.AccountKey, p.Instrument, p.NativeUnit, m.AccountKey, m.Instrument)
	}
	qty, err := pairNormalizeDecimal(p.SignedNativeQty)
	if err != nil {
		return pairVenuePosition{}, fmt.Errorf("the position quantity %q: %w", p.SignedNativeQty, err)
	}
	cs, err := pairNormalizeDecimal(p.ContractSize)
	if err != nil {
		return pairVenuePosition{}, fmt.Errorf("the position contract size %q: %w", p.ContractSize, err)
	}
	if !p.Found && qty != "0" {
		return pairVenuePosition{}, fmt.Errorf("an absent position cannot carry quantity %s", qty)
	}
	rec := pairVenuePosition{V: pairVenuePositionVersion, Key: pairEvidenceKey{Partition: v.t.Partition.String(), Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID},
		SignedNativeQty: qty, ContractSize: cs, PositionMode: p.PositionMode, MarginMode: p.MarginMode, ObservedMs: p.ObservedMs, FirstReceiptMs: receipt}
	body, err := pairCanonicalJSON(rec)
	if err != nil {
		return pairVenuePosition{}, err
	}
	rec.EvidenceID = fmt.Sprintf("%s-position:%d:%s", m.Provider, receipt, pairSHA256(body)[:16])
	return rec.sealed()
}

func (v *pairLiveBase) actionMember(a *basketAction) (pairContractMember, error) {
	if a.StrategyID != v.m.MemberID || a.AccountKey != v.m.AccountKey || a.Instrument != v.m.Instrument || a.Provider != v.provider {
		return pairContractMember{}, fmt.Errorf("action %s names %s %s %s, not this transport's member %s", a.ActionID, a.StrategyID, a.AccountKey, a.Instrument, v.m.MemberID)
	}
	return v.m, nil
}

func (v *pairLiveBase) Submit(a *basketAction) (basketVenueOutcome, error) {
	m, err := v.actionMember(a)
	if err != nil {
		return basketVenueOutcome{Outcome: pairVenueNeverSent, Detail: err.Error() + "; nothing was sent", ObservedAtMs: v.nowMs()}, nil
	}
	return v.submit(a, m)
}

func (v *pairLiveBase) Query(a *basketAction) (basketVenueOutcome, error) {
	m, err := v.actionMember(a)
	if err != nil {
		return basketVenueOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairOrderScriptTimout)
	defer cancel()
	return v.query(ctx, a, m)
}

func (v *pairLiveBase) Position(ctx context.Context, m pairContractMember, nowMs int64) (pairVenuePosition, error) {
	if m.MemberID != v.m.MemberID {
		return pairVenuePosition{}, fmt.Errorf("member %s is not this transport's member %s", m.MemberID, v.m.MemberID)
	}
	return v.position(ctx, m, v.t.PositionScript)
}

var (
	_ pairVenue = (*hyperliquidPairVenue)(nil)
	_ pairVenue = (*okxPairVenue)(nil)
)
