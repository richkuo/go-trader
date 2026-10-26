package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	pairNativeHistoryFlag          = "--pair-native-history-json"
	pairNativeHistoryVersion       = 1
	pairNativeHistoryOutputVersion = 1
	pairNativeHistoryMode          = "native_history"
	pairNativeHistoryComplete      = "complete"
	pairNativeHistoryMaxOrders     = 8
	pairNativeHistoryTimeout       = 20 * time.Second
	pairNativeAcceptSkewMs         = 10000
)

type pairNativeHistoryRequest struct {
	V             int      `json:"v"`
	PairID        string   `json:"pair_id"`
	Partition     string   `json:"partition"`
	MemberID      string   `json:"member_id"`
	Provider      string   `json:"provider"`
	AccountKey    string   `json:"account_key"`
	Instrument    string   `json:"instrument"`
	AttemptID     string   `json:"attempt_id"`
	OrderSide     string   `json:"order_side"`
	VenueOrderIDs []string `json:"venue_order_ids"`
	FromMs        int64    `json:"from_ms"`
	ThroughMs     int64    `json:"through_ms"`
	Revision      string   `json:"revision"`
}

type pairNativeHistoryOrder struct {
	OrderID           string `json:"order_id"`
	State             string `json:"state"`
	Terminal          bool   `json:"terminal"`
	Side              string `json:"side"`
	Instrument        string `json:"instrument"`
	ExecutedNativeQty string `json:"executed_native_qty"`
	AcceptedAtMs      int64  `json:"accepted_at_ms"`
}

type pairNativeHistoryCoverage struct {
	Complete  bool   `json:"complete"`
	Reason    string `json:"reason"`
	FromMs    int64  `json:"from_ms"`
	ThroughMs int64  `json:"through_ms"`
	Pages     int    `json:"pages"`
}

type pairNativeHistoryOutput struct {
	V             int                       `json:"v"`
	Mode          string                    `json:"mode"`
	Status        string                    `json:"status"`
	Detail        string                    `json:"detail"`
	ObservedAtMs  int64                     `json:"observed_at_ms"`
	AttemptID     string                    `json:"attempt_id"`
	AccountKey    string                    `json:"account_key"`
	Instrument    string                    `json:"instrument"`
	OrderSide     string                    `json:"order_side"`
	VenueOrderIDs []string                  `json:"venue_order_ids"`
	Orders        []pairNativeHistoryOrder  `json:"orders"`
	Coverage      pairNativeHistoryCoverage `json:"coverage"`
	Evidence      *nativeCloseEvidence      `json:"evidence"`
}

type pairNativeHistoryResult struct {
	Complete     bool
	Detail       string
	ObservedAtMs int64
	Orders       []pairNativeHistoryOrder
	Evidence     nativeCloseEvidence
}

type pairNativeHistoryReader func(ctx context.Context, m pairContractMember, req pairNativeHistoryRequest) (pairNativeHistoryResult, error)

type pairNativeHistorySource interface {
	NativeHistory(ctx context.Context, m pairContractMember, req pairNativeHistoryRequest) (pairNativeHistoryResult, error)
}

func pairSameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func pairCheckNativeHistory(req pairNativeHistoryRequest, h *pairNativeHistoryOutput) (pairNativeHistoryResult, error) {
	if h == nil {
		return pairNativeHistoryResult{}, fmt.Errorf("the native history script returned no native_history record")
	}
	if h.V != pairNativeHistoryOutputVersion || h.Mode != pairNativeHistoryMode {
		return pairNativeHistoryResult{}, fmt.Errorf("the native history record version %d mode %q is not %d %q", h.V, h.Mode, pairNativeHistoryOutputVersion, pairNativeHistoryMode)
	}
	if h.AttemptID != req.AttemptID || h.AccountKey != req.AccountKey || h.Instrument != req.Instrument || h.OrderSide != req.OrderSide || !pairSameIDSet(h.VenueOrderIDs, req.VenueOrderIDs) {
		return pairNativeHistoryResult{}, fmt.Errorf("the native history record echoes attempt %q account %q instrument %q side %q orders %v, not the request", h.AttemptID, h.AccountKey, h.Instrument, h.OrderSide, h.VenueOrderIDs)
	}
	res := pairNativeHistoryResult{Detail: strings.TrimSpace(h.Detail), ObservedAtMs: h.ObservedAtMs, Orders: h.Orders}
	if h.Status != pairNativeHistoryComplete {
		if res.Detail == "" {
			res.Detail = "the venue history is incomplete"
		}
		return res, nil
	}
	incomplete := func(format string, a ...interface{}) (pairNativeHistoryResult, error) {
		res.Detail = fmt.Sprintf(format, a...)
		return res, nil
	}
	if !h.Coverage.Complete {
		return incomplete("the fill history coverage is not complete: %s", h.Coverage.Reason)
	}
	if h.Evidence == nil || !h.Evidence.Complete || h.Evidence.V != nativeCloseEvidenceVersion {
		return incomplete("a complete answer carries no complete version %d fill evidence", nativeCloseEvidenceVersion)
	}
	if !pairSameIDSet(h.Evidence.VenueOrderIDs, req.VenueOrderIDs) {
		return incomplete("the fill evidence names orders %v, not the requested %v", h.Evidence.VenueOrderIDs, req.VenueOrderIDs)
	}
	want := map[string]bool{}
	for _, id := range req.VenueOrderIDs {
		want[id] = true
	}
	seen := map[string]bool{}
	executed := new(big.Rat)
	for _, o := range h.Orders {
		if !want[o.OrderID] || seen[o.OrderID] {
			return incomplete("order record %q is not one requested order", o.OrderID)
		}
		seen[o.OrderID] = true
		if !o.Terminal {
			return incomplete("order %s is %q, not terminal", o.OrderID, o.State)
		}
		if o.Side != req.OrderSide || o.Instrument != req.Instrument {
			return incomplete("order %s is a %s order on %s, not a %s close on %s", o.OrderID, o.Side, o.Instrument, req.OrderSide, req.Instrument)
		}
		if o.AcceptedAtMs < req.FromMs || o.AcceptedAtMs > req.ThroughMs {
			return incomplete("order %s was accepted at %d, outside the attempt window %d..%d", o.OrderID, o.AcceptedAtMs, req.FromMs, req.ThroughMs)
		}
		q, err := pairCoverageRat("order "+o.OrderID+" executed quantity", o.ExecutedNativeQty, false)
		if err != nil {
			return incomplete("%v", err)
		}
		executed.Add(executed, q)
	}
	if len(seen) != len(want) {
		return incomplete("the venue answered for %d of %d requested orders", len(seen), len(want))
	}
	evExec, err := pairCoverageRat("evidence executed quantity", h.Evidence.ExecutedNativeQty, false)
	if err != nil {
		return incomplete("%v", err)
	}
	if evExec.Cmp(executed) != 0 {
		return incomplete("the evidence reports %s executed, the order records %s", h.Evidence.ExecutedNativeQty, executed.FloatString(12))
	}
	for _, f := range h.Evidence.Fills {
		if !want[f.OrderID] {
			return incomplete("fill %s names order %q, which the attempt did not send", f.FillID, f.OrderID)
		}
		if f.Side != req.OrderSide {
			return incomplete("fill %s is a %q fill, not a %s close fill", f.FillID, f.Side, req.OrderSide)
		}
		if f.VenueTimeMs < req.FromMs {
			return incomplete("fill %s at %d precedes the attempt window from %d", f.FillID, f.VenueTimeMs, req.FromMs)
		}
	}
	res.Complete = true
	res.Evidence = *h.Evidence
	return res, nil
}

func (v *pairLiveBase) NativeHistory(ctx context.Context, m pairContractMember, req pairNativeHistoryRequest) (pairNativeHistoryResult, error) {
	if m.MemberID != v.m.MemberID || m.Provider != v.provider || req.Provider != v.provider || req.AccountKey != v.account || req.Instrument != v.m.Instrument || req.MemberID != v.m.MemberID {
		return pairNativeHistoryResult{}, fmt.Errorf("the native history request for %s %s %s is not bound to this %s transport member %s", req.MemberID, req.AccountKey, req.Instrument, v.provider, v.m.MemberID)
	}
	wire, err := json.Marshal(req)
	if err != nil {
		return pairNativeHistoryResult{}, err
	}
	out, err := v.read(ctx, v.t.Script, []string{pairNativeHistoryFlag + "=" + string(wire), "--mode=live"})
	if err != nil {
		return pairNativeHistoryResult{}, err
	}
	return pairCheckNativeHistory(req, out.NativeHistory)
}

var (
	_ pairNativeHistorySource = (*hyperliquidPairVenue)(nil)
	_ pairNativeHistorySource = (*okxPairVenue)(nil)
)
