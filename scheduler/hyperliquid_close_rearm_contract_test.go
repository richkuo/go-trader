package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestHLCloseRearmContract(t *testing.T) {
	t.Run("SizedClosePartialFillBooksTheFill", func(t *testing.T) {
		s := &StrategyState{
			ID:       "hl-eth",
			Platform: "hyperliquid",
			Type:     "perps",
			Cash:     10000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, AvgCost: 2000},
			},
		}
		result := &HyperliquidResult{Signal: -1, Symbol: "ETH", SizedCloseBookFraction: 0.4}
		result.CloseFraction = 1
		exec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: 4, OID: 9}}}
		executeHyperliquidResultDeferredOpen(StrategyConfig{ID: s.ID, Platform: "hyperliquid", Type: "perps", Direction: DirectionLong}, s, result, exec, "SELL", 2100, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(s.ID))
		pos := s.Positions["ETH"]
		if pos == nil || pos.Quantity < 5.9 || pos.Quantity > 6.1 {
			t.Fatalf("book = %+v, want 6 of 10 kept after a fill of 4", pos)
		}
	})
	t.Run("RearmQZeroRemoveUnconfirmedStop", func(t *testing.T) {
		orig := runHyperliquidUpdateStopLossFunc
		t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = orig })
		for _, tc := range []struct {
			name        string
			result      *HyperliquidStopLossUpdateResult
			wantOID     int64
			wantResting bool
		}{
			{name: "a rejected cancel is removed", result: &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossSucceeded: true}, wantOID: 0},
			{name: "a rejected removal leaves the stop and says it is still resting", result: &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossError: "busy"}, wantOID: 111, wantResting: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var calls int
				var gotSize float64
				var gotOID int64
				runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
					calls++
					gotSize, gotOID = size, cancelOID
					return tc.result, "", nil
				}
				state := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, StopLossOID: 111, StopLossTriggerPx: 1900},
				}}
				var mu sync.RWMutex
				sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid"}
				_, _, res, _ := rearmProtectionForCloseRemainder(sc, state, nil, "ETH", 2000, 111, 1900, 0, nil, nil, nil, hlCloseUnconfirmed{StopOID: 111}, hlCloseRemainderStop{}, &mu, nil, nil)
				if calls != 1 || gotSize != 0 || gotOID != 111 {
					t.Fatalf("removal calls=%d size=%g oid=%d, want one size-0 call for 111", calls, gotSize, gotOID)
				}
				if state.Positions["ETH"].StopLossOID != tc.wantOID {
					t.Fatalf("book stop = %d, want %d", state.Positions["ETH"].StopLossOID, tc.wantOID)
				}
				report := formatCloseRemovalReport(sc, "ETH", res.Removal)
				if tc.wantResting != strings.Contains(report, "STILL RESTING") {
					t.Fatalf("report = %q, resting %t", report, tc.wantResting)
				}
			})
		}
	})
	t.Run("DecideManualCloseRearmUnknownAndNotSent", func(t *testing.T) {
		snap := manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4, StopLossOID: 5150}
		for _, tc := range []struct {
			name   string
			result *HyperliquidExecuteResult
			want   manualCloseRearmDecision
		}{
			{name: "not_sent", result: &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "update_leverage failed"}, want: manualCloseRearmNoCancelRequested},
			{name: "unknown", result: &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"}, want: manualCloseRearmOutcomeUnknown},
			{name: "missing field", result: &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150}}, want: manualCloseRearmOutcomeUnknown},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := decideManualCloseRearm(tc.result, []int64{5150}, snap); got != tc.want {
					t.Fatalf("decision = %d, want %d", got, tc.want)
				}
			})
		}
	})
	t.Run("ResolveHLCloseRemainderStop", func(t *testing.T) {
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
	})
}
