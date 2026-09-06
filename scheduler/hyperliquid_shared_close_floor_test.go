package main

import (
	"errors"
	"strings"
	"testing"
)

func sharedCloseFloorPeers() []StrategyConfig {
	return []StrategyConfig{
		confirmationTestStrategy(DirectionLong),
		{ID: "hl-peer", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
}

func TestEvaluateSharedCoinFullCloseFloor(t *testing.T) {
	peers := sharedCloseFloorPeers()
	single := peers[:1]
	kPeers := []StrategyConfig{
		{ID: "hl-kpepe-a", Type: "perps", Platform: "hyperliquid", Symbol: "kPEPE", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "kPEPE", "1h", "--mode=live"}},
		{ID: "hl-kpepe-b", Type: "perps", Platform: "hyperliquid", Symbol: "kPEPE", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "kPEPE", "1h", "--mode=live"}},
	}
	cases := []struct {
		name        string
		fraction    float64
		peers       []StrategyConfig
		posQty      float64
		price       float64
		symbol      string
		onChain     hlOnChainCoinView
		peerVirtual float64
		heldReason  string
		want        hlSharedCloseFloorOutcome
	}{
		{name: "peer flat escalates", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorEscalate},
		{name: "on-chain below virtual escalates", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.001}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorEscalate},
		{name: "peer holds quantity holds once", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorHold},
		{name: "peer net opposite side holds", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.001}, NetSide: map[string]string{"ETH": "short"}}, want: hlSharedCloseFloorHold},
		{name: "opposite-side peer netting to a small same-side residual holds", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.0005}, NetSide: map[string]string{"ETH": "long"}}, peerVirtual: 0.0015, want: hlSharedCloseFloorHold},
		{name: "same-side peer hidden under virtual-over-on-chain drift holds", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.001}, NetSide: map[string]string{"ETH": "long"}}, peerVirtual: 0.001, want: hlSharedCloseFloorHold},
		{name: "peer book non-zero while wallet net is under virtual holds", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.0}, NetSide: map[string]string{}}, peerVirtual: 0.003, want: hlSharedCloseFloorHold},
		{name: "already held on busy peer stays silent", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldPeerBusy, want: hlSharedCloseFloorHeld},
		{name: "busy-peer hold escalates once the peer goes flat", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldPeerBusy, want: hlSharedCloseFloorEscalate},
		{name: "venue-rejected hold never re-escalates on a flat peer", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldVenueReject, want: hlSharedCloseFloorHeld},
		{name: "venue-rejected hold clears above the gate", fraction: 1, peers: peers, posQty: 0.01, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.01}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldVenueReject, want: hlSharedCloseFloorNone},
		{name: "mixed-case coin busy peer holds", fraction: 1, peers: kPeers, symbol: "kPEPE", posQty: 500, price: 0.00001, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"kPEPE": 900000}, NetSide: map[string]string{"kPEPE": "long"}}, want: hlSharedCloseFloorHold},
		{name: "mixed-case coin opposite-side peer holds", fraction: 1, peers: kPeers, symbol: "kPEPE", posQty: 500, price: 0.00001, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"kPEPE": 100}, NetSide: map[string]string{"kPEPE": "short"}}, want: hlSharedCloseFloorHold},
		{name: "mixed-case coin with whitespace symbol and flat peer escalates", fraction: 1, peers: kPeers, symbol: " kPEPE ", posQty: 500, price: 0.00001, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"kPEPE": 500}, NetSide: map[string]string{"kPEPE": "long"}}, want: hlSharedCloseFloorEscalate},
		{name: "on-chain unknown defers", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{}, want: hlSharedCloseFloorDefer},
		{name: "remainder at gate keeps sized close", fraction: 1, peers: peers, posQty: 0.00515, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "remainder above minimum keeps sized close", fraction: 1, peers: peers, posQty: 0.01, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "single owner unchanged", fraction: 1, peers: single, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "partial close unchanged", fraction: 0.5, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			symbol := tc.symbol
			if symbol == "" {
				symbol = "ETH"
			}
			got, _ := evaluateSharedCoinFullCloseFloor(tc.fraction, symbol, tc.peers, tc.posQty, "long", tc.price, tc.onChain, tc.peerVirtual, tc.heldReason)
			if got != tc.want {
				t.Fatalf("outcome = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSharedCoinFullCloseFloorExecutePath(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})
	peers := sharedCloseFloorPeers()
	sc := peers[0]
	flat := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}
	busy := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}

	cases := []struct {
		name          string
		onChain       hlOnChainCoinView
		refetched     *hlOnChainCoinView
		refetchErr    error
		peerVirtual   float64
		heldReason    string
		signal        int
		execErr       error
		wantFullClose int
		wantSized     int
		wantAlerts    int
		wantThrottled int
		wantStranded  bool
		wantOK        bool
	}{
		{name: "flat peer escalates to whole-position close", onChain: flat, wantFullClose: 1, wantOK: true},
		{name: "busy peer alerts once and sends no order", onChain: busy, wantAlerts: 1},
		{name: "busy peer already held sends nothing", onChain: busy, heldReason: hlSharedCloseHoldPeerBusy},
		{name: "escalated close rejected below minimum strands once", onChain: flat, execErr: errors.New("Order must have minimum value of $10."), wantFullClose: 1, wantAlerts: 1, wantStranded: true},
		{name: "venue-rejected hold sends no order and no alert next cycle", onChain: flat, heldReason: hlSharedCloseHoldVenueReject},
		{name: "peer opened since the cycle snapshot: refetch holds instead of escalating", onChain: flat, refetched: &busy, wantAlerts: 1},
		{name: "refetch failure defers without an order or alert", onChain: flat, refetchErr: errors.New("clearinghouseState timeout")},
		{name: "peer book holds quantity though the wallet net looks flat: one alert, no order", onChain: flat, peerVirtual: 0.001, wantAlerts: 1},
		{name: "zero signal is a noop for the floor", onChain: busy, signal: 0},
		{name: "escalated close other failure keeps the throttled failure alert", onChain: flat, execErr: errors.New("insufficient margin"), wantFullClose: 1, wantAlerts: 1, wantThrottled: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fullCloses, sized := 0, 0
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				if closeFullPosition {
					fullCloses++
				} else {
					sized++
				}
				if tc.execErr != nil {
					return nil, "", tc.execErr
				}
				return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.002}}}, "", nil
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			signal := -1
			if tc.signal != 0 {
				signal = tc.signal
			}
			if tc.name == "zero signal is a noop for the floor" {
				signal = 0
			}
			result := &HyperliquidResult{Symbol: "ETH", Signal: signal, Price: 2000}
			result.CloseFraction = 1
			refetches := 0
			refetch := func() (hlOnChainCoinView, error) {
				refetches++
				if tc.refetchErr != nil {
					return hlOnChainCoinView{}, tc.refetchErr
				}
				if tc.refetched != nil {
					return *tc.refetched, nil
				}
				return tc.onChain, nil
			}
			outcome, _ := applySharedCoinFullCloseFloor(sc, result, 0.002, "long", 2000, peers, tc.onChain, tc.peerVirtual, tc.heldReason, refetch, notifier, silentStrategyLogger(sc.ID))
			if outcome == hlSharedCloseFloorEscalate && refetches != 1 {
				t.Fatalf("escalated with %d refetches, want exactly 1", refetches)
			}
			if tc.wantFullClose == 0 && result.ForceFullClose {
				t.Fatalf("ForceFullClose set on outcome %s", outcome)
			}
			if signal == 0 {
				if outcome != hlSharedCloseFloorNone || result.CloseGate != "" {
					t.Fatalf("zero-signal floor outcome = %s gate %q, want none", outcome, result.CloseGate)
				}
				return
			}
			_, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 0.002, "long", 1900, 2, 111, nil, peers, hlExecuteSnapshot{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			if fullCloses != tc.wantFullClose || sized != tc.wantSized {
				t.Fatalf("orders = full %d sized %d, want full %d sized %d", fullCloses, sized, tc.wantFullClose, tc.wantSized)
			}
			backend.mu.Lock()
			messages, dms := len(backend.messages), len(backend.dms)
			backend.mu.Unlock()
			if messages != tc.wantAlerts || dms != tc.wantAlerts {
				t.Fatalf("alerts = channels %d DMs %d, want %d each", messages, dms, tc.wantAlerts)
			}
			liveExecThrottle.mu.Lock()
			throttled := len(liveExecThrottle.entries)
			liveExecThrottle.mu.Unlock()
			if throttled != tc.wantThrottled {
				t.Fatalf("throttle entries = %d, want %d", throttled, tc.wantThrottled)
			}
			if (result.SharedCloseStrandedUSD > 0) != tc.wantStranded {
				t.Fatalf("stranded = %g, want stranded %t", result.SharedCloseStrandedUSD, tc.wantStranded)
			}
			if tc.wantStranded {
				backend.mu.Lock()
				msg := backend.dms[0].content
				backend.mu.Unlock()
				if strings.Contains(msg, "every peer is flat on-chain and in its own book, or") || !strings.Contains(msg, "a peer going flat does not resend it") {
					t.Fatalf("venue-rejected alert promises a peer-flat resend: %s", msg)
				}
			}
		})
	}
}

func TestSharedCloseHoldPersistsAcrossReload(t *testing.T) {
	sdb := openTestDB(t)
	state := makeTestState()
	ss := state.Strategies["hl-momentum-btc"]
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.002, AvgCost: 2000, Side: "long", SharedCloseHoldUSD: 4, SharedCloseHoldReason: hlSharedCloseHoldVenueReject}
	if err := sdb.SaveState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Strategies["hl-momentum-btc"].Positions["ETH"]
	if got.SharedCloseHoldUSD != 4 || got.SharedCloseHoldReason != hlSharedCloseHoldVenueReject {
		t.Fatalf("hold after reload = %g/%q, want 4/%q", got.SharedCloseHoldUSD, got.SharedCloseHoldReason, hlSharedCloseHoldVenueReject)
	}
	if outcome, _ := evaluateSharedCoinFullCloseFloor(1, "ETH", sharedCloseFloorPeers(), 0.002, "long", 2000, hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, 0, got.SharedCloseHoldReason); outcome != hlSharedCloseFloorHeld {
		t.Fatalf("reloaded venue-rejected hold outcome = %s, want held", outcome)
	}
	clearSharedCloseHold(loaded.Strategies["hl-momentum-btc"], "ETH")
	if err := sdb.SaveState(loaded); err != nil {
		t.Fatal(err)
	}
	again, err := sdb.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Strategies["hl-momentum-btc"].Positions["ETH"]; v.SharedCloseHoldUSD != 0 || v.SharedCloseHoldReason != "" {
		t.Fatalf("hold after clear = %g/%q, want 0/empty", v.SharedCloseHoldUSD, v.SharedCloseHoldReason)
	}
}
