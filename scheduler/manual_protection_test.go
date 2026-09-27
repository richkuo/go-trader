package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func hlCoinView(coin string, signed float64) hlOnChainCoinView {
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

func stubCleanupClose(t *testing.T, res *HyperliquidCloseResult, err error) *[]hlSizedCloseRequest {
	t.Helper()
	orig := manualOpenCleanupCloseFn
	t.Cleanup(func() { manualOpenCleanupCloseFn = orig })
	var calls []hlSizedCloseRequest
	manualOpenCleanupCloseFn = func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
		calls = append(calls, req)
		return res, err
	}
	return &calls
}

func filledClose(sz float64, cancelled ...int64) *HyperliquidCloseResult {
	return &HyperliquidCloseResult{
		OrderOutcome:                "filled",
		Close:                       &HyperliquidClose{Symbol: "ETH", Fill: &HyperliquidCloseFill{AvgPx: 3000, TotalSz: sz, OID: 9}},
		CancelStopLossSucceeded:     len(cancelled) > 0,
		CancelStopLossSucceededOIDs: cancelled,
	}
}

func cleanupInput(side string, fill float64, view manualStateView, chain hlOnChainCoinView) manualOpenCleanupInput {
	return manualOpenCleanupInput{
		StrategyID: "m1", Symbol: "ETH", Side: side, FillQty: fill,
		StopLossOID: 12345, TPOIDs: []int64{67890},
		View: view, ViewKnown: true,
		Refetch: func() (hlOnChainCoinView, error) { return chain, nil },
	}
}

func TestAttemptManualOpenCleanup_CloseFails(t *testing.T) {
	stubCleanupClose(t, nil, fmt.Errorf("rpc timeout"))
	cleanedUp, msg := attemptManualOpenCleanup(cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.8)))
	if cleanedUp {
		t.Fatalf("expected cleanedUp=false, got msg=%q", msg)
	}
	if !strings.Contains(msg, "rpc timeout") || !strings.Contains(msg, "UNKNOWN") {
		t.Errorf("msg should report the unknown outcome and its cause; got %q", msg)
	}
}

func TestAttemptManualOpenCleanupSizesAgainstPeersAndTheFreshChain(t *testing.T) {
	cases := []struct {
		name       string
		side       string
		fill       float64
		view       manualStateView
		chain      float64
		wantSide   string
		wantMode   hlCloseMode
		wantSize   float64
		wantCancel bool
	}{
		{"sole owner long", "long", 1, manualStateView{}, 1, "sell", hlCloseModeReduceOnly, 1, true},
		{"sole owner short", "short", 1, manualStateView{}, -1, "buy", hlCloseModeReduceOnly, 1, true},
		{"same-side peer keeps its units", "long", 1, manualStateView{PeerLongQty: 2}, 3, "sell", hlCloseModeReduceOnly, 1, true},
		{"opposite-side peer long: sell, not a buy from the net", "long", 1, manualStateView{PeerShortQty: 3}, -2, "sell", hlCloseModeCross, 1, true},
		{"opposite-side peer short: buy crosses", "short", 1, manualStateView{PeerLongQty: 3}, 2, "buy", hlCloseModeCross, 1, true},
		{"chain below the book caps the close", "long", 1, manualStateView{PeerLongQty: 2}, 2.4, "sell", hlCloseModeReduceOnly, 0.4, false},
		{"own queued book counts once", "long", 1, manualStateView{Pos: &Position{Side: "long", Quantity: 2}}, 3, "sell", hlCloseModeReduceOnly, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubCleanupClose(t, filledClose(tc.wantSize, 12345, 67890), nil)
			ok, msg := attemptManualOpenCleanup(cleanupInput(tc.side, tc.fill, tc.view, hlCoinView("ETH", tc.chain)))
			if len(*calls) != 1 {
				t.Fatalf("close calls = %d, want 1 (%s)", len(*calls), msg)
			}
			req := (*calls)[0]
			if req.Side != tc.wantSide || req.Mode != tc.wantMode || math.Abs(req.Size-tc.wantSize) > 1e-9 {
				t.Fatalf("req = %+v, want side=%s mode=%s size=%v", req, tc.wantSide, tc.wantMode, tc.wantSize)
			}
			if got := len(req.CancelOIDs) > 0; got != tc.wantCancel {
				t.Fatalf("cancel OIDs = %v, want cancel=%v", req.CancelOIDs, tc.wantCancel)
			}
			if tc.wantCancel && math.Abs(req.CancelMinFill-(tc.fill-0.0001)) > 1e-12 {
				t.Fatalf("cancel threshold = %v, want the full fill less the close tolerance", req.CancelMinFill)
			}
			wantOK := tc.wantSize >= tc.fill-1e-9
			if ok != wantOK {
				t.Fatalf("cleanedUp = %v, want %v (%s)", ok, wantOK, msg)
			}
			if !ok && !strings.Contains(msg, "NOT proven closed") {
				t.Fatalf("a capped cleanup must name the unresolved exposure; got %q", msg)
			}
		})
	}
}

func TestAttemptManualOpenCleanupCappedNearTheFillDoesNotClaimTriggersCancelled(t *testing.T) {
	calls := stubCleanupClose(t, filledClose(0.99995), nil)
	in := cleanupInput("long", 1, manualStateView{PeerLongQty: 2}, hlCoinView("ETH", 2.99995))
	ok, msg := attemptManualOpenCleanup(in)
	if len(*calls) != 1 || len((*calls)[0].CancelOIDs) != 0 {
		t.Fatalf("calls=%v, want one capped close with no cancel request", *calls)
	}
	if ok || !strings.Contains(msg, "NOT proven closed") || !strings.Contains(msg, "12345") || !strings.Contains(msg, "67890") {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
	if strings.Contains(msg, "orphan triggers cancelled") {
		t.Fatalf("a capped cleanup must not claim the triggers were cancelled: %q", msg)
	}
}

func TestAttemptManualOpenCleanupNeverTreatsAFlatNetAsResolved(t *testing.T) {
	calls := stubCleanupClose(t, filledClose(1), nil)
	ok, msg := attemptManualOpenCleanup(cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 0)))
	if len(*calls) != 0 {
		t.Fatalf("a skipped plan must send no order, got %v", *calls)
	}
	if ok || !strings.Contains(msg, "NOT proven closed") {
		t.Fatalf("a flat shared net must not report success; got ok=%v msg=%q", ok, msg)
	}
}

func TestAttemptManualOpenCleanupReportsPartialAndUnconfirmedOutcomes(t *testing.T) {
	t.Run("partial fill", func(t *testing.T) {
		stubCleanupClose(t, filledClose(0.4), nil)
		ok, msg := attemptManualOpenCleanup(cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 1)))
		if ok || !strings.Contains(msg, "0.600000 of the 1.000000") {
			t.Fatalf("partial fill must report the 0.6 remainder; got ok=%v msg=%q", ok, msg)
		}
	})
	t.Run("zero fill is a rejection, not a close", func(t *testing.T) {
		stubCleanupClose(t, &HyperliquidCloseResult{OrderOutcome: "rejected", Error: "could not match"}, fmt.Errorf("close failed: could not match"))
		ok, msg := attemptManualOpenCleanup(cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 1)))
		if ok || !strings.Contains(msg, "rejected") {
			t.Fatalf("got ok=%v msg=%q", ok, msg)
		}
	})
	t.Run("filled without a usable size", func(t *testing.T) {
		stubCleanupClose(t, &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 3000, TotalSz: math.NaN()}}}, nil)
		ok, msg := attemptManualOpenCleanup(cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 1)))
		if ok || !strings.Contains(msg, "UNKNOWN") {
			t.Fatalf("a non-finite fill must be unknown, got ok=%v msg=%q", ok, msg)
		}
	})
	t.Run("trigger cancel not confirmed", func(t *testing.T) {
		stubCleanupClose(t, filledClose(1, 12345), nil)
		ok, msg := attemptManualOpenCleanup(cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 1)))
		if ok || !strings.Contains(msg, "not confirmed") {
			t.Fatalf("got ok=%v msg=%q", ok, msg)
		}
	})
	t.Run("unreadable books with no config send nothing", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(1, 12345, 67890), nil)
		in := cleanupInput("long", 1, manualStateView{}, hlCoinView("ETH", 1))
		in.ViewKnown = false
		ok, _ := attemptManualOpenCleanup(in)
		if ok || len(*calls) != 0 {
			t.Fatalf("got ok=%v calls=%v", ok, *calls)
		}
	})
}

func livePerps(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID: id, Platform: "hyperliquid", Type: "perps",
		Args: []string{"check_hyperliquid.py", coin, "1h", "--mode", "live"},
	}
}

func TestAttemptManualOpenCleanupUnreadableBooksUsesConfig(t *testing.T) {
	sole := &Config{Strategies: []StrategyConfig{livePerps("m1", "ETH")}}
	peer := &Config{Strategies: []StrategyConfig{livePerps("m1", "ETH"), livePerps("peer", "ETH")}}
	hedger := livePerps("hedger", "BTC")
	hedger.Hedge = &HedgeConfig{Enabled: true, Symbol: "eth"}
	hedged := &Config{Strategies: []StrategyConfig{livePerps("m1", "ETH"), hedger}}
	paper := StrategyConfig{
		ID: "paper", Platform: "hyperliquid", Type: "perps",
		Args: []string{"check_hyperliquid.py", "ETH", "1h", "--mode", "paper"},
	}
	paperOnly := &Config{Strategies: []StrategyConfig{livePerps("m1", "ETH"), paper}}

	t.Run("sole coin flattens the fill", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.8, 12345, 67890), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.8))
		in.ViewKnown = false
		in.Cfg = sole
		ok, msg := attemptManualOpenCleanup(in)
		if !ok || len(*calls) != 1 {
			t.Fatalf("ok=%v calls=%d msg=%q", ok, len(*calls), msg)
		}
		req := (*calls)[0]
		if req.Side != "sell" || req.Mode != hlCloseModeReduceOnly || math.Abs(req.Size-0.8) > 1e-9 {
			t.Fatalf("req=%+v", req)
		}
		if len(req.CancelOIDs) != 2 || math.Abs(req.CancelMinFill-(0.8-0.0001)) > 1e-12 {
			t.Fatalf("cancel=%v threshold=%v", req.CancelOIDs, req.CancelMinFill)
		}
	})
	t.Run("sole coin closes only the fill when the chain is larger", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.8, 12345, 67890), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 1.3))
		in.ViewKnown = false
		in.Cfg = sole
		ok, msg := attemptManualOpenCleanup(in)
		if !ok || len(*calls) != 1 {
			t.Fatalf("ok=%v calls=%d msg=%q", ok, len(*calls), msg)
		}
		req := (*calls)[0]
		if req.Side != "sell" || req.Mode != hlCloseModeReduceOnly || math.Abs(req.Size-0.8) > 1e-9 || len(req.CancelOIDs) != 2 {
			t.Fatalf("req=%+v", req)
		}
	})
	t.Run("configured peer sends nothing", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.8, 12345, 67890), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.8))
		in.ViewKnown = false
		in.Cfg = peer
		ok, msg := attemptManualOpenCleanup(in)
		if ok || len(*calls) != 0 || !strings.Contains(msg, "could not be read") {
			t.Fatalf("ok=%v calls=%d msg=%q", ok, len(*calls), msg)
		}
	})
	t.Run("hedge peer sends nothing", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.8, 12345, 67890), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.8))
		in.ViewKnown = false
		in.Cfg = hedged
		ok, msg := attemptManualOpenCleanup(in)
		if ok || len(*calls) != 0 || !strings.Contains(msg, "could not be read") {
			t.Fatalf("ok=%v calls=%d msg=%q", ok, len(*calls), msg)
		}
	})
	t.Run("paper strategy on the coin does not block", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.8, 12345, 67890), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.8))
		in.ViewKnown = false
		in.Cfg = paperOnly
		ok, msg := attemptManualOpenCleanup(in)
		if !ok || len(*calls) != 1 || math.Abs((*calls)[0].Size-0.8) > 1e-9 {
			t.Fatalf("ok=%v calls=%v msg=%q", ok, *calls, msg)
		}
	})
	t.Run("unread opposite book is capped and not treated as closed", func(t *testing.T) {
		calls := stubCleanupClose(t, filledClose(0.3), nil)
		in := cleanupInput("long", 0.8, manualStateView{}, hlCoinView("ETH", 0.3))
		in.ViewKnown = false
		in.Cfg = sole
		ok, msg := attemptManualOpenCleanup(in)
		if ok || len(*calls) != 1 {
			t.Fatalf("ok=%v calls=%d msg=%q", ok, len(*calls), msg)
		}
		req := (*calls)[0]
		if req.Mode != hlCloseModeReduceOnly || math.Abs(req.Size-0.3) > 1e-9 || len(req.CancelOIDs) != 0 {
			t.Fatalf("req=%+v", req)
		}
		if !strings.Contains(msg, "NOT proven closed") {
			t.Fatalf("msg=%q", msg)
		}
	})
}
