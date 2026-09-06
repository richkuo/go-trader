package main

import (
	"errors"
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
	cases := []struct {
		name        string
		fraction    float64
		peers       []StrategyConfig
		posQty      float64
		price       float64
		onChain     hlOnChainCoinView
		alreadyHeld bool
		want        hlSharedCloseFloorOutcome
	}{
		{name: "peer flat escalates", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.002}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorEscalate},
		{name: "on-chain below virtual escalates", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.001}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorEscalate},
		{name: "peer holds quantity holds once", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}, want: hlSharedCloseFloorHold},
		{name: "peer net opposite side holds", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.001}, NetSide: map[string]string{"ETH": "short"}}, want: hlSharedCloseFloorHold},
		{name: "already held stays silent", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}, NetSide: map[string]string{"ETH": "long"}}, alreadyHeld: true, want: hlSharedCloseFloorHeld},
		{name: "on-chain unknown defers", fraction: 1, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{}, want: hlSharedCloseFloorDefer},
		{name: "remainder at gate keeps sized close", fraction: 1, peers: peers, posQty: 0.00515, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "remainder above minimum keeps sized close", fraction: 1, peers: peers, posQty: 0.01, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "single owner unchanged", fraction: 1, peers: single, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
		{name: "partial close unchanged", fraction: 0.5, peers: peers, posQty: 0.002, price: 2000, onChain: hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 0.5}}, want: hlSharedCloseFloorNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := evaluateSharedCoinFullCloseFloor(tc.fraction, "ETH", tc.peers, tc.posQty, "long", tc.price, tc.onChain, tc.alreadyHeld)
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
		alreadyHeld   bool
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
		{name: "busy peer already held sends nothing", onChain: busy, alreadyHeld: true},
		{name: "escalated close rejected below minimum strands once", onChain: flat, execErr: errors.New("Order must have minimum value of $10."), wantFullClose: 1, wantAlerts: 1, wantStranded: true},
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
			result := &HyperliquidResult{Symbol: "ETH", Signal: -1, Price: 2000}
			result.CloseFraction = 1
			applySharedCoinFullCloseFloor(sc, result, 0.002, "long", 2000, peers, tc.onChain, tc.alreadyHeld, notifier, silentStrategyLogger(sc.ID))
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
		})
	}
}

func TestSharedCloseHoldPersistsAcrossReload(t *testing.T) {
	sdb := openTestDB(t)
	state := makeTestState()
	ss := state.Strategies["hl-momentum-btc"]
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.002, AvgCost: 2000, Side: "long", SharedCloseHoldUSD: 4}
	if err := sdb.SaveState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Strategies["hl-momentum-btc"].Positions["ETH"].SharedCloseHoldUSD
	if got != 4 {
		t.Fatalf("shared_close_hold_usd after reload = %g, want 4", got)
	}
	clearSharedCloseHold(loaded.Strategies["hl-momentum-btc"], "ETH")
	if err := sdb.SaveState(loaded); err != nil {
		t.Fatal(err)
	}
	again, err := sdb.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Strategies["hl-momentum-btc"].Positions["ETH"].SharedCloseHoldUSD; v != 0 {
		t.Fatalf("shared_close_hold_usd after clear = %g, want 0", v)
	}
}
