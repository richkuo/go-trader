package main

import (
	"math"
	"testing"
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
