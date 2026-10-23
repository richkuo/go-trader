package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	quoteRoleBid   = "bid"
	quoteRoleAsk   = "ask"
	quoteRoleStop  = "stop"
	quoteRoleClose = "close"

	quoteOutcomeUnknown   = "unknown"
	quoteOutcomeAbsent    = "absent"
	quoteOutcomeResting   = "resting"
	quoteOutcomeRejected  = "rejected"
	quoteOutcomeTriggered = "triggered"
	quoteOutcomeFilled    = "filled"
	quoteOutcomeCancelled = "cancelled"

	quoteVenueScriptTimeout = 30 * time.Second
	quoteVenueScript        = hyperliquidCheckScript
)

type quoteOrderRequest struct {
	Coin          string `json:"coin"`
	Role          string `json:"role"`
	Side          string `json:"side"`
	Px            string `json:"px,omitempty"`
	Size          string `json:"size"`
	ReduceOnly    bool   `json:"reduce_only,omitempty"`
	PostOnly      bool   `json:"post_only,omitempty"`
	TIF           string `json:"tif,omitempty"`
	TriggerPx     string `json:"trigger_px,omitempty"`
	ExpiryMs      int64  `json:"expiry_ms"`
	ClientOrderID string `json:"client_order_id"`
}

func (r quoteOrderRequest) canonicalJSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

func parseQuoteOrderRequest(raw string) (quoteOrderRequest, error) {
	var r quoteOrderRequest
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("frozen quote request is unreadable: %w", err)
	}
	return r, nil
}

type quoteOrderOutcome struct {
	Status       string `json:"status"`
	VenueOrderID string `json:"venue_order_id,omitempty"`
	VenueTimeMs  int64  `json:"venue_time_ms"`
	AcceptedAtMs int64  `json:"accepted_at_ms,omitempty"`
	FilledQty    string `json:"filled_qty,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

func (o quoteOrderOutcome) terminal() bool {
	switch o.Status {
	case quoteOutcomeRejected, quoteOutcomeFilled, quoteOutcomeCancelled, quoteOutcomeAbsent:
		return true
	}
	return false
}

type quoteRawFill struct {
	Account       string `json:"account"`
	Namespace     string `json:"namespace"`
	Coin          string `json:"coin"`
	Side          string `json:"side"`
	Px            string `json:"px"`
	Size          string `json:"size"`
	Fee           string `json:"fee"`
	FeeAsset      string `json:"fee_asset"`
	StartPosition string `json:"start_position,omitempty"`
	TimeMs        int64  `json:"time_ms"`
	TradeID       string `json:"trade_id"`
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Kind          string `json:"kind,omitempty"`
	CorrectsTrade string `json:"corrects_trade_id,omitempty"`
	Raw           string `json:"raw"`
}

func (f quoteRawFill) kind() string {
	if f.Kind == "" {
		return "fill"
	}
	return f.Kind
}

func (f quoteRawFill) key() string {
	return strings.Join([]string{f.Account, f.Namespace, f.Coin, f.OrderID, strconv.FormatInt(f.TimeMs, 10), f.TradeID, f.kind()}, "|")
}

func (f quoteRawFill) digest() string {
	body, _ := json.Marshal(struct {
		Side, Px, Size, Fee, FeeAsset, StartPosition, ClientOrderID, Kind, CorrectsTrade, Raw string
	}{f.Side, f.Px, f.Size, f.Fee, f.FeeAsset, f.StartPosition, f.ClientOrderID, f.kind(), f.CorrectsTrade, f.Raw})
	return sha256Hex(body)
}

func (f quoteRawFill) validate() error {
	if strings.TrimSpace(f.Account) == "" || strings.TrimSpace(f.Coin) == "" || strings.TrimSpace(f.OrderID) == "" || strings.TrimSpace(f.TradeID) == "" {
		return fmt.Errorf("fill %s lacks account, coin, order or trade identity", f.key())
	}
	if f.Side != "buy" && f.Side != "sell" {
		return fmt.Errorf("fill %s has side %q", f.key(), f.Side)
	}
	if f.TimeMs <= 0 {
		return fmt.Errorf("fill %s has no event time", f.key())
	}
	px, err := parseQuoteDecimal(f.Px)
	if err != nil || px.Sign() <= 0 {
		return fmt.Errorf("fill %s has price %q", f.key(), f.Px)
	}
	sz, err := parseQuoteDecimal(f.Size)
	if err != nil || sz.Sign() <= 0 {
		return fmt.Errorf("fill %s has size %q", f.key(), f.Size)
	}
	if _, err := parseQuoteDecimal(f.Fee); err != nil {
		return fmt.Errorf("fill %s has fee %q", f.key(), f.Fee)
	}
	if strings.TrimSpace(f.FeeAsset) == "" {
		return fmt.Errorf("fill %s names no fee asset", f.key())
	}
	if f.StartPosition != "" {
		if _, err := parseQuoteDecimal(f.StartPosition); err != nil {
			return fmt.Errorf("fill %s has start position %q", f.key(), f.StartPosition)
		}
	}
	switch f.kind() {
	case "fill":
		if f.CorrectsTrade != "" {
			return fmt.Errorf("fill %s names a corrected trade but is not a correction", f.key())
		}
	case "fee_correction":
		if strings.TrimSpace(f.CorrectsTrade) == "" {
			return fmt.Errorf("fee correction %s names no corrected trade", f.key())
		}
	default:
		return fmt.Errorf("fill %s has unsupported kind %q", f.key(), f.Kind)
	}
	return nil
}

type quoteFillCoverage struct {
	Complete  bool   `json:"complete"`
	FromMs    int64  `json:"from_ms"`
	ThroughMs int64  `json:"through_ms"`
	Reason    string `json:"reason,omitempty"`
	Pages     int    `json:"pages"`
}

type quoteFillRecovery struct {
	Fills     []quoteRawFill    `json:"fills"`
	Coverage  quoteFillCoverage `json:"coverage"`
	Conflicts []string          `json:"conflicts,omitempty"`
}

type quotePositionRead struct {
	Account       string  `json:"account"`
	Coin          string  `json:"coin"`
	Size          string  `json:"size"`
	LiquidationPx float64 `json:"liquidation_px"`
	FreeMargin    string  `json:"free_margin,omitempty"`
	ReadAtMs      int64   `json:"read_at_ms"`
}

type quoteVenue interface {
	Name() string
	LiveShaped() bool
	Provider() string
	AccountKey(p RiskPartition) string
	Namespace() string
	NewClientOrderID() (string, error)
	Place(ctx context.Context, req quoteOrderRequest) (quoteOrderOutcome, error)
	Cancel(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error)
	Status(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error)
	RecoverFills(ctx context.Context, coin string, sinceMs, untilMs int64) (quoteFillRecovery, error)
	Position(ctx context.Context, coin string) (quotePositionRead, error)
}

func newQuoteClientOrderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}

type quotePriceRule struct {
	lot hlLotLookup
}

func newQuotePriceRule(lot hlLotLookup) quotePriceRule {
	return quotePriceRule{lot: lot}
}

func (r quotePriceRule) known() bool {
	return r.lot.Known && r.lot.SzDecimals >= 0
}

func quoteRatLog10Floor(px *big.Rat) int {
	k := 0
	one := big.NewRat(1, 1)
	if px.Cmp(one) >= 0 {
		for pow10Rat(k+1).Cmp(px) <= 0 {
			k++
		}
		return k
	}
	for pow10Rat(k).Cmp(px) > 0 {
		k--
	}
	return k
}

func (r quotePriceRule) pxDecimals(px *big.Rat) int {
	pxDec := 6 - r.lot.SzDecimals
	if pxDec < 0 {
		pxDec = 0
	}
	sig := 4 - quoteRatLog10Floor(px)
	if sig < 0 {
		sig = 0
	}
	if sig < pxDec {
		return sig
	}
	return pxDec
}

func (r quotePriceRule) directed(px *big.Rat, up bool) (*big.Rat, error) {
	if !r.known() {
		return nil, fmt.Errorf("lot metadata for %s is unknown (%s)", r.lot.Coin, r.lot.Reason)
	}
	if px == nil || px.Sign() <= 0 {
		return nil, fmt.Errorf("price must be > 0")
	}
	d := r.pxDecimals(px)
	var out *big.Rat
	if up {
		out = quoteRatCeilTo(px, d)
	} else {
		out = quoteRatFloorTo(px, d)
	}
	if out.Sign() <= 0 {
		return nil, fmt.Errorf("price rounds to zero")
	}
	if d2 := r.pxDecimals(out); d2 < d {
		if up {
			out = quoteRatCeilTo(out, d2)
		} else {
			out = quoteRatFloorTo(out, d2)
		}
	}
	return out, nil
}

func (r quotePriceRule) roundQuotePx(side string, px *big.Rat) (*big.Rat, error) {
	switch side {
	case "buy":
		return r.directed(px, false)
	case "sell":
		return r.directed(px, true)
	}
	return nil, fmt.Errorf("quote side %q is neither buy nor sell", side)
}

func (r quotePriceRule) roundTriggerPx(stopSide string, px *big.Rat) (*big.Rat, error) {
	switch stopSide {
	case "sell":
		return r.directed(px, true)
	case "buy":
		return r.directed(px, false)
	}
	return nil, fmt.Errorf("stop side %q is neither buy nor sell", stopSide)
}

func (r quotePriceRule) floorQuoteSize(sz *big.Rat) (*big.Rat, error) {
	if !r.known() {
		return nil, fmt.Errorf("lot metadata for %s is unknown (%s)", r.lot.Coin, r.lot.Reason)
	}
	if sz == nil || sz.Sign() <= 0 {
		return quoteRatZero(), nil
	}
	return quoteRatFloorTo(sz, r.lot.SzDecimals), nil
}

func (r quotePriceRule) check(req quoteOrderRequest) error {
	sz, err := parseQuoteDecimal(req.Size)
	if err != nil || sz.Sign() <= 0 {
		return fmt.Errorf("request size %q is not a positive decimal", req.Size)
	}
	floored, err := r.floorQuoteSize(sz)
	if err != nil {
		return err
	}
	if floored.Cmp(sz) != 0 {
		return fmt.Errorf("request size %s is not on the %d-decimal lot grid", req.Size, r.lot.SzDecimals)
	}
	if req.Role == quoteRoleStop {
		tp, err := parseQuoteDecimal(req.TriggerPx)
		if err != nil || tp.Sign() <= 0 {
			return fmt.Errorf("stop trigger %q is not a positive decimal", req.TriggerPx)
		}
		rounded, err := r.roundTriggerPx(req.Side, tp)
		if err != nil {
			return err
		}
		if rounded.Cmp(tp) != 0 {
			return fmt.Errorf("stop trigger %s would round to %s", req.TriggerPx, rounded.FloatString(8))
		}
		return nil
	}
	px, err := parseQuoteDecimal(req.Px)
	if err != nil || px.Sign() <= 0 {
		return fmt.Errorf("request price %q is not a positive decimal", req.Px)
	}
	rounded, err := r.roundQuotePx(req.Side, px)
	if err != nil {
		return err
	}
	if rounded.Cmp(px) != 0 {
		return fmt.Errorf("request price %s would round to %s", req.Px, rounded.FloatString(8))
	}
	return nil
}

func quoteDecimalText(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	for d := 1; d <= 18; d++ {
		s := r.FloatString(d)
		back, ok := new(big.Rat).SetString(s)
		if ok && back.Cmp(r) == 0 {
			return strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
	}
	return r.FloatString(18)
}

type quoteLiveVenue struct {
	account   string
	namespace string
	script    string
}

func newQuoteLiveVenue(account, namespace string) *quoteLiveVenue {
	return &quoteLiveVenue{account: account, namespace: namespace, script: quoteVenueScript}
}

func (v *quoteLiveVenue) Name() string                      { return "hyperliquid" }
func (v *quoteLiveVenue) LiveShaped() bool                  { return true }
func (v *quoteLiveVenue) Provider() string                  { return "hyperliquid" }
func (v *quoteLiveVenue) AccountKey(p RiskPartition) string { return v.account }
func (v *quoteLiveVenue) Namespace() string                 { return v.namespace }
func (v *quoteLiveVenue) NewClientOrderID() (string, error) {
	return newQuoteClientOrderID()
}

type quoteScriptOutput struct {
	Error     string             `json:"error,omitempty"`
	Account   string             `json:"account,omitempty"`
	Outcome   *quoteOrderOutcome `json:"outcome,omitempty"`
	Recovery  *quoteFillRecovery `json:"recovery,omitempty"`
	Position  *quotePositionRead `json:"position,omitempty"`
	CheckedAt int64              `json:"checked_at_ms,omitempty"`
}

func quoteRequestArgs(mode string, req quoteOrderRequest) []string {
	args := []string{mode, "--symbol=" + req.Coin, "--side=" + req.Side, "--size=" + req.Size, "--cloid=" + req.ClientOrderID,
		"--role=" + req.Role, fmt.Sprintf("--expires-at-ms=%d", req.ExpiryMs), "--mode=live"}
	if req.Role == quoteRoleStop {
		args = append(args, "--trigger-px="+req.TriggerPx)
	} else {
		args = append(args, "--limit-price="+req.Px)
		tif := req.TIF
		if tif == "" {
			tif = "Alo"
		}
		args = append(args, "--tif="+tif)
	}
	if req.ReduceOnly {
		args = append(args, "--reduce-only")
	}
	return args
}

func parseQuoteScriptOutput(stdout, stderr []byte, runErr error) (quoteScriptOutput, error) {
	var out quoteScriptOutput
	parseErr := json.Unmarshal(stdout, &out)
	var timeoutErr *pythonScriptTimeoutError
	if errors.As(runErr, &timeoutErr) {
		return out, fmt.Errorf("quote venue script timed out: %w", runErr)
	}
	if parseErr != nil {
		return out, fmt.Errorf("quote venue script output is unreadable: %v (stderr: %s)", parseErr, strings.TrimSpace(string(stderr)))
	}
	if out.Error != "" {
		return out, fmt.Errorf("quote venue script: %s", out.Error)
	}
	if runErr != nil {
		return out, fmt.Errorf("quote venue script failed: %w", runErr)
	}
	return out, nil
}

func (v *quoteLiveVenue) sideEffect(args []string) (quoteOrderOutcome, error) {
	stdout, stderr, err := runPythonSideEffectWithTimeout(v.script, args, quoteVenueScriptTimeout)
	out, perr := parseQuoteScriptOutput(stdout, stderr, err)
	if out.Outcome != nil && perr != nil && out.Outcome.Status == quoteOutcomeRejected {
		return *out.Outcome, nil
	}
	if perr != nil {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: perr.Error()}, nil
	}
	if out.Outcome == nil {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "the quote venue script returned no outcome"}, nil
	}
	return *out.Outcome, nil
}

func (v *quoteLiveVenue) read(ctx context.Context, args []string) (quoteScriptOutput, error) {
	stdout, stderr, err := runPythonWithTimeout(ctx, v.script, args, nil, quoteVenueScriptTimeout)
	return parseQuoteScriptOutput(stdout, stderr, err)
}

func (v *quoteLiveVenue) Place(ctx context.Context, req quoteOrderRequest) (quoteOrderOutcome, error) {
	return v.sideEffect(quoteRequestArgs("--quote-place", req))
}

func (v *quoteLiveVenue) Cancel(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	args := []string{"--quote-cancel", "--symbol=" + req.Coin, "--cloid=" + req.ClientOrderID, "--mode=live"}
	if venueOrderID != "" {
		args = append(args, "--oid="+venueOrderID)
	}
	return v.sideEffect(args)
}

func (v *quoteLiveVenue) Status(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	args := []string{"--quote-status", "--symbol=" + req.Coin, "--cloid=" + req.ClientOrderID, "--mode=live"}
	out, err := v.read(ctx, args)
	if err != nil {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}, nil
	}
	if out.Outcome == nil {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: "the quote status read returned no outcome"}, nil
	}
	return *out.Outcome, nil
}

func (v *quoteLiveVenue) RecoverFills(ctx context.Context, coin string, sinceMs, untilMs int64) (quoteFillRecovery, error) {
	args := []string{"--quote-recover", "--symbol=" + coin, fmt.Sprintf("--since-ms=%d", sinceMs), fmt.Sprintf("--until-ms=%d", untilMs), "--mode=live"}
	out, err := v.read(ctx, args)
	if err != nil {
		return quoteFillRecovery{}, err
	}
	if out.Recovery == nil {
		return quoteFillRecovery{}, fmt.Errorf("the quote recovery read returned no evidence")
	}
	if out.Account != "" && out.Account != v.account {
		return quoteFillRecovery{}, fmt.Errorf("the quote recovery read answered for account %q, not %q", out.Account, v.account)
	}
	return *out.Recovery, nil
}

func (v *quoteLiveVenue) Position(ctx context.Context, coin string) (quotePositionRead, error) {
	out, err := v.read(ctx, []string{"--quote-position", "--symbol=" + coin, "--mode=live"})
	if err != nil {
		return quotePositionRead{}, err
	}
	if out.Position == nil {
		return quotePositionRead{}, fmt.Errorf("the quote position read returned no position")
	}
	if out.Position.Account != v.account {
		return quotePositionRead{}, fmt.Errorf("the quote position read answered for account %q, not %q", out.Position.Account, v.account)
	}
	return *out.Position, nil
}

func sortQuoteFills(fills []quoteRawFill) {
	sort.SliceStable(fills, func(i, j int) bool {
		if fills[i].TimeMs != fills[j].TimeMs {
			return fills[i].TimeMs < fills[j].TimeMs
		}
		return fills[i].key() < fills[j].key()
	})
}
