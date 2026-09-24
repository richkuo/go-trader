package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func hlSignedView(coin string, signed float64) hlOnChainCoinView {
	v := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}
	if signed > 0 {
		v.AbsQty[coin] = signed
		v.NetSide[coin] = "long"
	} else if signed < 0 {
		v.AbsQty[coin] = -signed
		v.NetSide[coin] = "short"
	}
	return v
}

func TestPlanHLCloseOrder(t *testing.T) {
	unknown := hlOnChainCoinView{}
	sideless := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 3}, NetSide: map[string]string{}}
	cases := []struct {
		name       string
		side       string
		posQty     float64
		closeQty   float64
		same, opp  float64
		view       hlOnChainCoinView
		wantAction hlCloseAction
		wantMode   hlCloseMode
		wantSize   float64
		wantCapped bool
	}{
		{"sole owner book above chain caps reduce-only", "long", 10, 10, 0, 0, hlSignedView("ETH", 7), hlCloseSend, hlCloseModeReduceOnly, 7, true},
		{"sole owner chain flat sends nothing", "long", 10, 10, 0, 0, hlSignedView("ETH", 0), hlCloseSkip, hlCloseModeNone, 0, false},
		{"sole owner chain opposite sends nothing", "long", 10, 10, 0, 0, hlSignedView("ETH", -2), hlCloseSkip, hlCloseModeNone, 0, false},
		{"sole owner partial within chain", "long", 10, 4, 0, 0, hlSignedView("ETH", 10), hlCloseSend, hlCloseModeReduceOnly, 4, false},
		{"sole owner partial with book above chain caps", "long", 10, 5, 0, 0, hlSignedView("ETH", 7), hlCloseSend, hlCloseModeReduceOnly, 2, true},
		{"same-side peer keeps its share", "long", 10, 10, 5, 0, hlSignedView("ETH", 12), hlCloseSend, hlCloseModeReduceOnly, 7, true},
		{"opposite-side peer crosses to its book", "long", 10, 10, 0, 4, hlSignedView("ETH", 6), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side net already short crosses", "long", 10, 10, 0, 14, hlSignedView("ETH", -4), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side chain flat crosses only to the peer book", "long", 10, 10, 0, 4, hlSignedView("ETH", 0), hlCloseSend, hlCloseModeCross, 4, true},
		{"opposite-side peer stop unbooked caps at own book", "long", 10, 10, 0, 4, hlSignedView("ETH", 10), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side partial stays reduce-only", "long", 10, 3, 0, 4, hlSignedView("ETH", 6), hlCloseSend, hlCloseModeReduceOnly, 3, false},
		{"short strategy with long peer buys cross", "short", 10, 10, 0, 4, hlSignedView("ETH", -6), hlCloseSend, hlCloseModeCross, 10, false},
		{"short sole owner book above chain caps", "short", 10, 10, 0, 0, hlSignedView("ETH", -3), hlCloseSend, hlCloseModeReduceOnly, 3, true},
		{"unknown chain without opposite peer sends reduce-only book size", "long", 10, 10, 5, 0, unknown, hlCloseSend, hlCloseModeReduceOnly, 10, false},
		{"unknown chain with opposite peer defers", "long", 10, 10, 0, 4, unknown, hlCloseDefer, hlCloseModeNone, 0, false},
		{"unreadable net side defers", "long", 10, 10, 0, 0, sideless, hlCloseDefer, hlCloseModeNone, 0, false},
		{"close above book defers", "long", 10, 11, 0, 0, hlSignedView("ETH", 10), hlCloseDefer, hlCloseModeNone, 0, false},
		{"unknown side defers", "", 10, 10, 0, 0, hlSignedView("ETH", 10), hlCloseDefer, hlCloseModeNone, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planHLCloseOrder("ETH", tc.side, tc.posQty, tc.closeQty, tc.same, tc.opp, tc.view)
			if got.Action != tc.wantAction || got.Mode != tc.wantMode || math.Abs(got.Size-tc.wantSize) > 1e-9 || got.Capped != tc.wantCapped {
				t.Fatalf("plan = %+v, want action %v mode %v size %g capped %t", got, tc.wantAction, tc.wantMode, tc.wantSize, tc.wantCapped)
			}
			wantSigned, readable := hlOnChainSignedQty(tc.view, "ETH")
			wantPreSend := tc.view.Known && readable && (tc.side == "long" || tc.side == "short") && tc.closeQty <= tc.posQty
			if got.PreSend.Known != wantPreSend || (wantPreSend && math.Abs(got.PreSend.Signed-wantSigned) > 1e-9) {
				t.Fatalf("pre-send view = %+v, want known %t signed %g", got.PreSend, wantPreSend, wantSigned)
			}
			if got.Action != hlCloseSend {
				return
			}
			if got.Size > tc.closeQty+1e-9 {
				t.Fatalf("size %g exceeds the book close %g", got.Size, tc.closeQty)
			}
			if !tc.view.Known {
				return
			}
			net, _ := hlOnChainSignedQty(tc.view, "ETH")
			s := hlSideSign(tc.side)
			target := s*(tc.posQty-tc.closeQty) + s*tc.same - s*tc.opp
			after := net - s*got.Size
			lo, hi := math.Min(net, target), math.Max(net, target)
			if after < lo-1e-9 || after > hi+1e-9 {
				t.Fatalf("chain after close %g leaves the range [%g, %g] between the net and the books", after, lo, hi)
			}
			if got.Mode == hlCloseModeReduceOnly && got.Size > math.Abs(net)+1e-9 {
				t.Fatalf("reduce-only size %g exceeds the on-chain %g", got.Size, math.Abs(net))
			}
		})
	}
}

func TestHLPeerBooksOnCoin(t *testing.T) {
	live := []string{"hold", "ETH", "1h", "--mode=live"}
	cfgs := []StrategyConfig{
		{ID: "self", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "peer-long", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "peer-short", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "hedger", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Hedge: &HedgeConfig{Enabled: true, Symbol: "eth"}},
		{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "eth", Args: []string{"hold", "eth", "1h", "--mode=live"}},
		{ID: "other-coin", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "SOL", "1h", "--mode=live"}},
	}
	states := map[string]*StrategyState{
		"self":       {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 10}}},
		"peer-long":  {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 5}}},
		"peer-short": {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "short", Quantity: 3}}},
		"hedger": {Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 1},
			"ETH": {Symbol: "ETH", Side: "short", Quantity: 2, HedgeFor: "BTC"},
		}},
		"manual-eth": {Positions: map[string]*Position{"eth": {Symbol: "eth", Side: "long", Quantity: 1}}},
		"other-coin": {Positions: map[string]*Position{"SOL": {Symbol: "SOL", Side: "short", Quantity: 9}}},
	}
	cases := []struct {
		name     string
		selfID   string
		selfSide string
		wantSame float64
		wantOpp  float64
	}{
		{"long self counts long peers as same side", "self", "long", 6, 5},
		{"short self counts long peers as opposite side", "self", "short", 5, 6},
		{"a peer excludes only itself", "peer-long", "long", 11, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			same, opp := hlPeerBooksOnCoin(states, cfgs, "Eth", tc.selfID, tc.selfSide)
			if math.Abs(same-tc.wantSame) > 1e-9 || math.Abs(opp-tc.wantOpp) > 1e-9 {
				t.Fatalf("same=%g opp=%g, want same=%g opp=%g", same, opp, tc.wantSame, tc.wantOpp)
			}
		})
	}
}

func TestManualCloseFillAttribution(t *testing.T) {
	cases := []struct {
		name       string
		posQty     float64
		fill       *HyperliquidFill
		wantQty    float64
		wantFee    float64
		wantFullBk bool
	}{
		{"fill above book books only the book", 1.0, &HyperliquidFill{TotalSz: 1.25, Fee: 1.0}, 1.0, 0.8, true},
		{"capped fill below request books the fill", 1.0, &HyperliquidFill{TotalSz: 0.7, Fee: 0.35}, 0.7, 0.35, false},
		{"exact fill closes the book", 1.0, &HyperliquidFill{TotalSz: 1.0, Fee: 0.5}, 1.0, 0.5, true},
		{"dust under threshold counts as full", 1.0, &HyperliquidFill{TotalSz: 0.99995, Fee: 0.5}, 0.99995, 0.5, true},
		{"missing fill books nothing", 1.0, nil, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qty, fee, full := manualCloseFillAttribution(tc.posQty, tc.fill)
			if math.Abs(qty-tc.wantQty) > 1e-12 || math.Abs(fee-tc.wantFee) > 1e-12 || full != tc.wantFullBk {
				t.Fatalf("got qty=%g fee=%g full=%t, want qty=%g fee=%g full=%t", qty, fee, full, tc.wantQty, tc.wantFee, tc.wantFullBk)
			}
		})
	}
}

func TestBookManualCycleClose(t *testing.T) {
	sc := StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}}
	cases := []struct {
		name        string
		intentFull  bool
		closeQty    float64
		fillSz      float64
		requested   []int64
		succeeded   []int64
		failed      []int64
		wantQty     float64
		wantFull    bool
		wantShort   bool
		wantSLOID   int64
		wantTPOIDs  []int64
		wantTPArmed []bool
		wantPnL     float64
	}{
		{name: "capped fill with the stop and every take-profit cancelled clears all ids", intentFull: true, closeQty: 7, fillSz: 7, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 7, wantShort: true, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 699},
		{name: "partial IOC fill of a full close clears the cancelled ids", intentFull: true, closeQty: 10, fillSz: 4, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 4, wantShort: true, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 399},
		{name: "a stop cancel the venue reported as failed keeps its id", intentFull: true, closeQty: 10, fillSz: 4, requested: []int64{111, 201, 202}, succeeded: []int64{201, 202}, failed: []int64{111}, wantQty: 4, wantShort: true, wantSLOID: 111, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 399},
		{name: "a full fill closes the book and leaves the ids to the book delete", intentFull: true, closeQty: 10, fillSz: 10, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 10, wantFull: true, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantTPArmed: []bool{true, true}, wantPnL: 999},
		{name: "a partial-intent close requested no cancel and keeps every id", closeQty: 5, fillSz: 5, requested: []int64{0}, wantQty: 5, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantTPArmed: []bool{true, true}, wantPnL: 499},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := &Position{Symbol: "ETH", Quantity: 10, AvgCost: 2000, Side: "long", OwnerStrategyID: sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{201, 202}, TPArmedTiers: []bool{true, true}}
			exec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: tc.fillSz, Fee: 1, OID: 9}}, CancelStopLossSucceededOIDs: tc.succeeded, CancelStopLossFailedOIDs: tc.failed}
			if len(tc.failed) > 0 {
				exec.CancelStopLossError = "cancel rejected"
			}
			booking := bookManualCycleClose(sc, pos, "sell", tc.closeQty, tc.intentFull, exec, tc.requested, time.Unix(0, 0).UTC())
			clearHyperliquidProtectionOIDsMatching(pos, booking.ClearOIDs)
			a := booking.Action
			if math.Abs(a.Quantity-tc.wantQty) > 1e-9 || a.IsFullClose != tc.wantFull || booking.ShortOfIntent != tc.wantShort || math.Abs(a.RealizedPnL-tc.wantPnL) > 1e-9 || a.ExchangeOrderID != "9" {
				t.Fatalf("action qty=%g full=%t short=%t pnl=%g oid=%q, want qty=%g full=%t short=%t pnl=%g", a.Quantity, a.IsFullClose, booking.ShortOfIntent, a.RealizedPnL, a.ExchangeOrderID, tc.wantQty, tc.wantFull, tc.wantShort, tc.wantPnL)
			}
			if pos.StopLossOID != tc.wantSLOID || fmt.Sprint(pos.TPOIDs) != fmt.Sprint(tc.wantTPOIDs) || fmt.Sprint(pos.TPArmedTiers) != fmt.Sprint(tc.wantTPArmed) {
				t.Fatalf("book sl=%d tps=%v armed=%v, want sl=%d tps=%v armed=%v", pos.StopLossOID, pos.TPOIDs, pos.TPArmedTiers, tc.wantSLOID, tc.wantTPOIDs, tc.wantTPArmed)
			}
		})
	}
}

func TestResolveHLCloseRemainderStop(t *testing.T) {
	readErr := errors.New("clearinghouseState timeout")
	num := func(v float64) *float64 { return &v }
	filled := func(q float64) hlCloseFillOutcome { return hlCloseFillOutcome{Filled: q, Known: true} }
	rejected := hlCloseFillOutcome{Known: true}
	unknown := hlCloseFillOutcome{}
	cases := []struct {
		name         string
		side         string
		book         float64
		fill         hlCloseFillOutcome
		same, opp    float64
		preSend      *float64
		read         *float64
		readErr      error
		wantQty      float64
		wantBasis    hlCloseRemainderBasis
		wantUnbacked bool
		wantReads    int
	}{
		{name: "a fill that covers the book leaves nothing to re-arm and reads nothing", side: "long", book: 10, fill: filled(10), wantQty: 0, wantBasis: hlRemainderBasisNone},
		{name: "a sole short fill arms the remainder the post-close read backs", side: "long", book: 10, fill: filled(4), read: num(6), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a sole capped close that empties the chain places no stop", side: "long", book: 10, fill: filled(7), read: num(0), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "a short fill beside a same-side peer arms the whole remainder", side: "long", book: 10, fill: filled(4), same: 5, read: num(11), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a capped shared close whose chain holds only the peer units places no stop", side: "long", book: 10, fill: filled(7), same: 5, read: num(5), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "a cross short fill beside an opposite-side peer caps at the own-side chain units", side: "long", book: 10, fill: filled(3), opp: 4, read: num(3), wantQty: 3, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a chain net on the other side backs nothing even with an opposite-side peer", side: "long", book: 4, fill: filled(2), opp: 10, read: num(-8), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "a short strategy nets its same-side peer out", side: "short", book: 10, fill: filled(4), same: 2, read: num(-8), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a short strategy with a long net backs nothing", side: "short", book: 10, fill: filled(4), opp: 3, read: num(2), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "a post-fill read nets the same-side peers out", side: "long", book: 10, fill: filled(4), same: 5, read: num(9), wantQty: 4, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a post-fill read caps at the own-side units and never adds opposite-side peers back", side: "long", book: 10, fill: filled(4), opp: 2, read: num(4), wantQty: 4, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a failed read after a short fill derives the chain from the pre-send reading", side: "long", book: 10, fill: filled(4), same: 5, preSend: num(15), readErr: readErr, wantQty: 6, wantBasis: hlRemainderBasisDerived, wantReads: 1},
		{name: "a failed read after a capped shared close derives no backing", side: "long", book: 10, fill: filled(7), same: 5, preSend: num(12), readErr: readErr, wantQty: 0, wantBasis: hlRemainderBasisDerived, wantUnbacked: true, wantReads: 1},
		{name: "a failed read derives a pre-send share below the book as a partial stop", side: "long", book: 10, fill: filled(4), preSend: num(9), readErr: readErr, wantQty: 5, wantBasis: hlRemainderBasisDerived, wantReads: 1},
		{name: "a failed read on a short strategy derives from the short pre-send reading", side: "short", book: 10, fill: filled(4), same: 2, preSend: num(-12), readErr: readErr, wantQty: 6, wantBasis: hlRemainderBasisDerived, wantReads: 1},
		{name: "a failed read with no pre-send reading arms the remainder on no reading", side: "long", book: 10, fill: filled(7), readErr: readErr, wantQty: 3, wantBasis: hlRemainderBasisNone, wantReads: 1},
		{name: "a known rejection on a sole chain arms the whole book", side: "long", book: 10, fill: rejected, read: num(10), wantQty: 10, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a known rejection beside a same-side peer arms the whole book", side: "long", book: 10, fill: rejected, same: 5, read: num(15), wantQty: 10, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a known rejection beside an opposite-side peer caps at the own-side chain units", side: "long", book: 10, fill: rejected, opp: 4, read: num(6), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a known rejection with the chain net on the other side places no stop", side: "long", book: 10, fill: rejected, opp: 14, read: num(-4), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "a known rejection on a short strategy beside a long peer caps at the own-side units", side: "short", book: 10, fill: rejected, opp: 4, read: num(-6), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a known rejection with no pre-send share reads the account once", side: "long", book: 10, fill: rejected, read: num(3), wantQty: 3, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "a known rejection of a capped plan reads once and derives the pre-send share", side: "long", book: 10, fill: rejected, preSend: num(7), readErr: readErr, wantQty: 7, wantBasis: hlRemainderBasisDerived, wantReads: 1},
		{name: "a known rejection with a failed read derives from the opposite-side pre-send reading", side: "long", book: 10, fill: rejected, opp: 4, preSend: num(6), readErr: readErr, wantQty: 6, wantBasis: hlRemainderBasisDerived, wantReads: 1},
		{name: "a known rejection with no reading at all arms the book", side: "long", book: 10, fill: rejected, readErr: readErr, wantQty: 10, wantBasis: hlRemainderBasisNone, wantReads: 1},
		{name: "an unknown outcome that filled after a lost reply leaves only peer units", side: "long", book: 3, fill: unknown, same: 5, read: num(5), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "an unknown outcome that partly filled after a lost reply caps at the backed units", side: "long", book: 10, fill: unknown, same: 5, read: num(11), wantQty: 6, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "an unknown outcome that did not fill arms the whole book", side: "long", book: 10, fill: unknown, read: num(10), wantQty: 10, wantBasis: hlRemainderBasisFresh, wantReads: 1},
		{name: "an unknown outcome reads the account even with a pre-send reading", side: "long", book: 10, fill: unknown, same: 5, preSend: num(15), read: num(5), wantQty: 0, wantBasis: hlRemainderBasisFresh, wantUnbacked: true, wantReads: 1},
		{name: "an unknown outcome with a failed read keeps the stale pre-send reading", side: "long", book: 10, fill: unknown, same: 5, preSend: num(15), readErr: readErr, wantQty: 10, wantBasis: hlRemainderBasisStale, wantReads: 1},
		{name: "an unknown outcome with no reading at all arms the book", side: "long", book: 10, fill: unknown, readErr: readErr, wantQty: 10, wantBasis: hlRemainderBasisNone, wantReads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			b := hlCloseBacking{PeerSameQty: tc.same, PeerOppQty: tc.opp}
			if tc.preSend != nil {
				b.PreSend = hlCloseView{Signed: *tc.preSend, Known: true, Source: hlCloseViewPreSendRefetch}
			}
			b.Refetch = func() (hlOnChainCoinView, error) {
				reads++
				if tc.readErr != nil {
					return hlOnChainCoinView{}, tc.readErr
				}
				if tc.read == nil {
					return hlOnChainCoinView{}, fmt.Errorf("unexpected read")
				}
				return hlSignedView("ETH", *tc.read), nil
			}
			got := resolveHLCloseRemainderStop("ETH", tc.side, tc.book, tc.fill, b)
			if math.Abs(got.Qty-tc.wantQty) > 1e-9 || got.Basis != tc.wantBasis || got.Unbacked != tc.wantUnbacked || reads != tc.wantReads {
				t.Fatalf("stop qty=%g basis=%d unbacked=%t reads=%d, want qty=%g basis=%d unbacked=%t reads=%d", got.Qty, got.Basis, got.Unbacked, reads, tc.wantQty, tc.wantBasis, tc.wantUnbacked, tc.wantReads)
			}
			wantRemainder := tc.book
			if tc.fill.Known {
				wantRemainder = tc.book - tc.fill.Filled
			}
			if math.Abs(got.Remainder-wantRemainder) > 1e-9 || got.AfterFill != (tc.fill.Known && tc.fill.Filled > 0) || got.Qty > got.Remainder+1e-9 {
				t.Fatalf("remainder=%g after_fill=%t qty=%g, want remainder %g after_fill %t and qty within it", got.Remainder, got.AfterFill, got.Qty, wantRemainder, tc.fill.Known && tc.fill.Filled > 0)
			}
			if tc.readErr != nil && got.Remainder > 0 && !strings.Contains(got.Detail, readErr.Error()) {
				t.Fatalf("detail = %q, want the failed read named", got.Detail)
			}
		})
	}
}
