package main

import (
	"errors"
	"math"
	"strings"
	"sync"
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
		{name: "escalate-failed marker retries the escalation while every peer stays flat", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldEscalateFail, want: hlSharedCloseFloorEscalate},
		{name: "escalate-failed marker still alerts when a peer becomes busy", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}, heldReason: hlSharedCloseHoldEscalateFail, want: hlSharedCloseFloorHold},
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
		name               string
		onChain            hlOnChainCoinView
		refetched          *hlOnChainCoinView
		refetchErr         error
		peerVirtual        float64
		heldReason         string
		signal             int
		execErr            error
		wantFullClose      int
		wantSized          int
		wantAlerts         int
		wantThrottled      int
		wantStranded       bool
		wantEscalateFailed bool
		wantOK             bool
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
		{name: "escalated close other failure keeps the throttled failure alert and marks the escalation failed", onChain: flat, execErr: errors.New("insufficient margin"), wantFullClose: 1, wantAlerts: 1, wantThrottled: 1, wantEscalateFailed: true},
		{name: "escalated close refused in unrecognised wording marks the escalation failed instead of stranding", onChain: flat, execErr: errors.New("order value below minimum"), wantFullClose: 1, wantAlerts: 1, wantThrottled: 1, wantEscalateFailed: true},
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
			if (result.SharedCloseEscalateFailedUSD > 0) != tc.wantEscalateFailed {
				t.Fatalf("escalate-failed = %g, want marked %t", result.SharedCloseEscalateFailedUSD, tc.wantEscalateFailed)
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

func TestRearmTrailingStopAfterFailedCloseKeepsRatchetAndClamp(t *testing.T) {
	oldUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = oldUpdate })
	trail := 2.0
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs, TrailingStopATRMult: &trail}
	cases := []struct {
		name          string
		bookOID       int64
		bookTrigger   float64
		bookHighWater float64
		prevOID       int64
		prevTrigger   float64
		prevHighWater float64
		onChainQty    float64
		liqPx         float64
		wantCancel    int64
		wantSize      float64
		wantTrigger   float64
		wantHighWater float64
	}{
		{name: "readable cancel cleared the book: old oid is the cancel oid and the ratcheted high-water anchors the trigger", bookHighWater: 2200, prevOID: 444, prevTrigger: 2090, prevHighWater: 2200, onChainQty: 0.002, wantCancel: 444, wantSize: 0.002, wantTrigger: 2090, wantHighWater: 2200},
		{name: "unreadable result keeps the book oid and hands it to the update script as the cancel oid", bookOID: 444, bookTrigger: 2090, bookHighWater: 2200, prevOID: 444, prevTrigger: 2090, prevHighWater: 2200, onChainQty: 0.002, wantCancel: 444, wantSize: 0.002, wantTrigger: 2090, wantHighWater: 2200},
		{name: "virtual above on-chain arms at the capped size instead of skipping", bookHighWater: 2200, prevOID: 444, prevHighWater: 2200, onChainQty: 0.001, wantCancel: 444, wantSize: 0.001, wantTrigger: 2090, wantHighWater: 2200},
		{name: "trigger past the liquidation price is clamped inside it", bookHighWater: 2200, prevOID: 444, prevHighWater: 2200, onChainQty: 0.002, liqPx: 2095, wantCancel: 444, wantSize: 0.002, wantTrigger: 2095 * (1 + hlLiquidationStopBufferPct/100), wantHighWater: 2200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCancel int64
			var gotSize, gotTrigger float64
			placed := 0
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				placed++
				gotCancel, gotSize, gotTrigger = cancelStopLossOID, size, triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelStopLossOID > 0}, "", nil
			}
			st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 0.002, InitialQuantity: 0.002, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, StopLossOID: tc.bookOID, StopLossTriggerPx: tc.bookTrigger, StopLossHighWaterPx: tc.bookHighWater},
			}}
			var liq map[string]float64
			var net map[string]string
			if tc.liqPx > 0 {
				liq = map[string]float64{"ETH": tc.liqPx}
				net = map[string]string{"ETH": "long"}
			}
			var mu sync.RWMutex
			rearmProtectionAfterFailedClose(sc, st, nil, "ETH", 2100, tc.prevOID, tc.prevTrigger, tc.prevHighWater, map[string]float64{"ETH": tc.onChainQty}, nil, liq, net, &mu, nil, newTestLogger(t))
			if placed != 1 {
				t.Fatalf("stop placements = %d, want 1", placed)
			}
			if gotCancel != tc.wantCancel || math.Abs(gotSize-tc.wantSize) > 1e-9 || math.Abs(gotTrigger-tc.wantTrigger) > 1e-6 {
				t.Fatalf("placed cancel=%d size=%g trigger=%g, want cancel=%d size=%g trigger=%g", gotCancel, gotSize, gotTrigger, tc.wantCancel, tc.wantSize, tc.wantTrigger)
			}
			pos := st.Positions["ETH"]
			if pos.StopLossOID != 999 || math.Abs(pos.StopLossTriggerPx-tc.wantTrigger) > 1e-6 || math.Abs(pos.StopLossHighWaterPx-tc.wantHighWater) > 1e-9 {
				t.Fatalf("book after re-arm oid=%d trigger=%g high_water=%g, want oid 999 trigger %g high_water %g", pos.StopLossOID, pos.StopLossTriggerPx, pos.StopLossHighWaterPx, tc.wantTrigger, tc.wantHighWater)
			}
		})
	}
}

func TestHLPeerVirtualQtyOnCoinMatchesPeerSetKey(t *testing.T) {
	live := []string{"x.py", "kPEPE", "1h", "--mode=live"}
	self := StrategyConfig{ID: "self", Type: "perps", Platform: "hyperliquid", Args: live}
	cases := []struct {
		name string
		peer StrategyConfig
		want float64
	}{
		{name: "peer coin padded with whitespace", peer: StrategyConfig{ID: "peer", Type: "perps", Platform: "hyperliquid", Args: []string{"x.py", " kPEPE ", "1h", "--mode=live"}}, want: 5},
		{name: "peer coin differs only by case", peer: StrategyConfig{ID: "peer", Type: "perps", Platform: "hyperliquid", Args: []string{"x.py", "KPEPE", "1h", "--mode=live"}}, want: 5},
		{name: "manual peer keyed by its symbol", peer: StrategyConfig{ID: "peer", Type: "manual", Platform: "hyperliquid", Symbol: "kpepe", Args: live}, want: 5},
		{name: "peer on another coin is not a peer", peer: StrategyConfig{ID: "peer", Type: "perps", Platform: "hyperliquid", Args: []string{"x.py", "DOGE", "1h", "--mode=live"}}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peerCoin := hyperliquidRawCoin(tc.peer)
			strategies := map[string]*StrategyState{
				"self": {ID: "self", Positions: map[string]*Position{"kPEPE": {Symbol: "kPEPE", Side: "long", Quantity: 1, AvgCost: 0.01}}},
				"peer": {ID: "peer", Positions: map[string]*Position{peerCoin: {Symbol: peerCoin, Side: "long", Quantity: 5, AvgCost: 0.01}}},
			}
			roster := []StrategyConfig{self, tc.peer}
			sharedPeers := len(hlLiveStrategiesForCoin("kPEPE", roster)) - 1
			got := hlPeerVirtualQtyOnCoin(snapshotHyperliquidVirtualQuantities(strategies, roster), "kPEPE", "self")
			if (sharedPeers > 0) != (got > 0) || got != tc.want {
				t.Fatalf("shared peers=%d but peer virtual qty=%g, want %g", sharedPeers, got, tc.want)
			}
		})
	}
}

func TestRearmProtectionAfterFailedCloseCoversPercentageStopOwners(t *testing.T) {
	oldUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = oldUpdate })
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	pct := 5.0
	marginPct := 20.0
	trailPct := 3.0
	base := func(mut func(*StrategyConfig)) StrategyConfig {
		sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs}
		mut(&sc)
		return sc
	}
	cases := []struct {
		name        string
		sc          StrategyConfig
		bookOID     int64
		bookTrigger float64
		prevOID     int64
		liqPx       float64
		wantPlaced  int
		wantCancel  int64
		wantTrigger float64
	}{
		{name: "stop_loss_pct owner whose cancel landed is re-armed at the anchor-scaled trigger", sc: base(func(sc *StrategyConfig) { sc.StopLossPct = &pct }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "stop_loss_margin_pct owner with an unreadable result hands the still-recorded oid to the update script", sc: base(func(sc *StrategyConfig) { sc.StopLossMarginPct = &marginPct; sc.Leverage = 4 }), bookOID: 444, bookTrigger: 1900, prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "max_drawdown_pct fallback owner is re-armed", sc: base(func(sc *StrategyConfig) { sc.MaxDrawdownPct = 5 }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "percentage trigger past the liquidation price is clamped inside it", sc: base(func(sc *StrategyConfig) { sc.StopLossPct = &pct }), prevOID: 444, liqPx: 1950, wantPlaced: 1, wantCancel: 444, wantTrigger: 1950 * (1 + hlLiquidationStopBufferPct/100)},
		{name: "trailing_stop_pct owner is armed once by the trailing arm and never double-armed", sc: base(func(sc *StrategyConfig) { sc.TrailingStopPct = &trailPct }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 2000 * (1 - trailPct/100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCancel int64
			var gotTrigger float64
			placed := 0
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				placed++
				gotCancel, gotTrigger = cancelStopLossOID, triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelStopLossOID > 0}, "", nil
			}
			st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 0.002, InitialQuantity: 0.002, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, StopLossOID: tc.bookOID, StopLossTriggerPx: tc.bookTrigger, StopLossHighWaterPx: 2000},
			}}
			var liq map[string]float64
			var net map[string]string
			if tc.liqPx > 0 {
				liq = map[string]float64{"ETH": tc.liqPx}
				net = map[string]string{"ETH": "long"}
			}
			var mu sync.RWMutex
			rearmProtectionAfterFailedClose(tc.sc, st, nil, "ETH", 2000, tc.prevOID, tc.bookTrigger, 2000, map[string]float64{"ETH": 0.002}, nil, liq, net, &mu, nil, newTestLogger(t))
			if placed != tc.wantPlaced {
				t.Fatalf("stop placements = %d, want %d", placed, tc.wantPlaced)
			}
			if gotCancel != tc.wantCancel || math.Abs(gotTrigger-tc.wantTrigger) > 1e-6 {
				t.Fatalf("placed cancel=%d trigger=%g, want cancel=%d trigger=%g", gotCancel, gotTrigger, tc.wantCancel, tc.wantTrigger)
			}
			if pos := st.Positions["ETH"]; pos.StopLossOID != 999 || math.Abs(pos.StopLossTriggerPx-tc.wantTrigger) > 1e-6 {
				t.Fatalf("book after re-arm oid=%d trigger=%g, want oid 999 trigger %g", pos.StopLossOID, pos.StopLossTriggerPx, tc.wantTrigger)
			}
		})
	}
}

func TestDecideOperatorSharedCloseFloor(t *testing.T) {
	peers := sharedCloseFloorPeers()
	single := peers[:1]
	flat := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}
	busy := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}
	fetchErr := errors.New("clearinghouseState timeout")

	cases := []struct {
		name         string
		peers        []StrategyConfig
		posQty       float64
		price        float64
		peerVirtual  float64
		reads        []hlOnChainCoinView
		readErrs     []error
		wantEscalate bool
		wantRefuse   bool
		wantUnread   bool
		wantReads    int
		wantInReason []string
	}{
		{name: "every peer flat escalates", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{flat, flat}, wantEscalate: true, wantReads: 2},
		{name: "peer holds quantity in its own book refuses", peers: peers, posQty: 0.002, price: 2000,
			peerVirtual: 0.003, reads: []hlOnChainCoinView{flat}, wantRefuse: true, wantReads: 1,
			wantInReason: []string{"0.003000", "no order sent"}},
		{name: "peer holds quantity on-chain refuses", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{busy}, wantRefuse: true, wantReads: 1,
			wantInReason: []string{"0.500000", "no order sent"}},
		{name: "peer goes busy between the read and the refetch refuses", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{flat, busy}, wantRefuse: true, wantReads: 2,
			wantInReason: []string{"0.500000", "no order sent"}},
		{name: "refetch failure refuses", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{flat, {}}, readErrs: []error{nil, fetchErr}, wantRefuse: true, wantReads: 2,
			wantInReason: []string{"clearinghouseState timeout", "no order sent"}},
		{name: "first read failure refuses", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{{}}, readErrs: []error{fetchErr}, wantRefuse: true, wantReads: 1,
			wantInReason: []string{"clearinghouseState timeout"}},
		{name: "unreadable account refuses", peers: peers, posQty: 0.002, price: 2000,
			reads: []hlOnChainCoinView{{}}, wantRefuse: true, wantReads: 1,
			wantInReason: []string{"not readable"}},
		{name: "unreadable mark withholds only the escalation and leaves the sized order unchanged", peers: peers, posQty: 0.002, price: 0,
			wantUnread: true, wantReads: 0,
			wantInReason: []string{"no usable mark price", "escalation to a whole-position close is withheld", "sized reduce-only close is sent unchanged"}},
		{name: "unreadable mark on a large position leaves the sized order unchanged", peers: peers, posQty: 5, price: math.NaN(),
			wantUnread: true, wantReads: 0, wantInReason: []string{"no usable mark price"}},
		{name: "single owner leaves the order unchanged", peers: single, posQty: 0.002, price: 2000, wantReads: 0},
		{name: "value at or above the gate leaves the order unchanged", peers: peers, posQty: 0.01, price: 2000, wantReads: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			fetch := func() (hlOnChainCoinView, error) {
				idx := reads
				reads++
				if idx >= len(tc.reads) {
					t.Fatalf("unexpected on-chain read #%d", reads)
				}
				var err error
				if idx < len(tc.readErrs) {
					err = tc.readErrs[idx]
				}
				return tc.reads[idx], err
			}
			got := decideOperatorSharedCloseFloor("ETH", "long", tc.posQty, tc.price, tc.peers, tc.peerVirtual, fetch)
			if got.Escalate != tc.wantEscalate || got.Refuse != tc.wantRefuse || got.MarkUnreadable != tc.wantUnread {
				t.Fatalf("escalate=%v refuse=%v markUnreadable=%v, want escalate=%v refuse=%v markUnreadable=%v (reason %q)",
					got.Escalate, got.Refuse, got.MarkUnreadable, tc.wantEscalate, tc.wantRefuse, tc.wantUnread, got.Reason)
			}
			if reads != tc.wantReads {
				t.Fatalf("on-chain reads = %d, want %d", reads, tc.wantReads)
			}
			if tc.wantRefuse && got.Reason == "" {
				t.Fatal("refusal carries no reason")
			}
			for _, want := range tc.wantInReason {
				if !strings.Contains(got.Reason, want) {
					t.Fatalf("reason %q missing %q", got.Reason, want)
				}
			}
		})
	}

	t.Run("no account reader refuses", func(t *testing.T) {
		got := decideOperatorSharedCloseFloor("ETH", "long", 0.002, 2000, peers, 0, nil)
		if !got.Refuse || got.Escalate {
			t.Fatalf("escalate=%v refuse=%v, want a refusal", got.Escalate, got.Refuse)
		}
	})
}

func TestSharedCloseStrandedAlertRecoveryTextDropsForceCloseClaim(t *testing.T) {
	for _, holdReason := range []string{hlSharedCloseHoldPeerBusy, hlSharedCloseHoldVenueReject, hlSharedCloseHoldOperatorRef} {
		msg := formatSharedCloseStrandedAlert("hl-eth", "ETH", 4.12, "peer busy", holdReason)
		if strings.Contains(msg, "sends the same sized order") || strings.Contains(msg, "issue 1534") {
			t.Fatalf("hold %q recovery text still promises the old force-close behavior: %s", holdReason, msg)
		}
		if !strings.Contains(msg, "worth $4.12") {
			t.Fatalf("hold %q alert dropped the measured remainder: %s", holdReason, msg)
		}
		unmeasured := formatSharedCloseStrandedAlert("hl-eth", "ETH", 0, "peer busy", holdReason)
		if strings.Contains(unmeasured, "worth $0.00") {
			t.Fatalf("hold %q alert states an unmeasured value as fact: %s", holdReason, unmeasured)
		}
		if !strings.Contains(unmeasured, "could not be measured") {
			t.Fatalf("hold %q alert does not say the value is unmeasured: %s", holdReason, unmeasured)
		}
	}
}
