package main

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hedgeTestConfig(id, symbol, hedgeSymbol string) StrategyConfig {
	return StrategyConfig{
		ID:             id,
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"rsi", symbol, "--mode=live"},
		Capital:        1000,
		MaxDrawdownPct: 20,
		Leverage:       3,
		Hedge: &HedgeConfig{
			Enabled:    true,
			Symbol:     hedgeSymbol,
			Side:       HedgeSideInverse,
			Ratio:      0.5,
			Platform:   "hyperliquid",
			Type:       "perps",
			MarginMode: "cross",
			Leverage:   2,
		},
	}
}

func TestHedgeTargetInverseNotionalSizing(t *testing.T) {
	sc := hedgeTestConfig("hl-eth", "ETH", "BTC")
	target, err := hedgeTargetForPrimary(sc, "long", 4, 2500, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if target.Side != "short" || target.Quantity != 0.1 {
		t.Fatalf("target = %+v, want short 0.1 BTC", target)
	}
	target, err = hedgeTargetForPrimary(sc, "short", 2, 2500, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if target.Side != "long" || target.Quantity != 0.05 {
		t.Fatalf("target = %+v, want long 0.05 BTC", target)
	}
}

func TestPlanHedgeTransitionLifecycle(t *testing.T) {
	tests := []struct {
		name string
		from *Position
		to   hedgeTarget
		want []hedgeOrder
	}{
		{"open", nil, hedgeTarget{Side: "short", Quantity: 2}, []hedgeOrder{{Side: "sell", Quantity: 2}}},
		{"scale", &Position{Side: "short", Quantity: 2, IsHedge: true}, hedgeTarget{Side: "short", Quantity: 3}, []hedgeOrder{{Side: "sell", Quantity: 1}}},
		{"partial", &Position{Side: "short", Quantity: 3, IsHedge: true}, hedgeTarget{Side: "short", Quantity: 1}, []hedgeOrder{{Close: true, Quantity: 2}}},
		{"full", &Position{Side: "short", Quantity: 3, IsHedge: true}, hedgeTarget{}, []hedgeOrder{{Close: true, Quantity: 3, FullClose: true}}},
		{"flip", &Position{Side: "short", Quantity: 3, IsHedge: true}, hedgeTarget{Side: "long", Quantity: 2}, []hedgeOrder{{Close: true, Quantity: 3, FullClose: true}, {Side: "buy", Quantity: 2}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planHedgeTransition(tc.from, tc.to)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("orders = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("orders[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPlanHedgeTransitionDeadband(t *testing.T) {
	current := &Position{Side: "short", Quantity: 1, IsHedge: true}
	if got, err := planHedgeTransitionWithPolicy(current, hedgeTarget{Side: "short", Quantity: 1.004}, false, 0.5); err != nil || len(got) != 0 {
		t.Fatalf("sub-threshold drift orders=%+v err=%v, want hold", got, err)
	}
	got, err := planHedgeTransitionWithPolicy(current, hedgeTarget{Side: "short", Quantity: 1.006}, false, 0.5)
	if err != nil || len(got) != 1 || math.Abs(got[0].Quantity-0.006) > 1e-12 {
		t.Fatalf("accumulated drift orders=%+v err=%v, want 0.006 add", got, err)
	}
	got, err = planHedgeTransitionWithPolicy(current, hedgeTarget{Side: "short", Quantity: 1.001}, true, 0.5)
	if err != nil || len(got) != 1 || math.Abs(got[0].Quantity-0.001) > 1e-12 {
		t.Fatalf("forced lifecycle rebalance orders=%+v err=%v, want 0.001 add", got, err)
	}
}

func TestValidateHedgeConfigRejectsCollisions(t *testing.T) {
	tests := []struct {
		name       string
		strategies []StrategyConfig
		want       string
	}{
		{"own coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "ETH")}, "matches its primary coin"},
		{"strategy coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "BTC"), hedgeTestConfig("b", "BTC", "SOL")}, "matches configured strategy"},
		{"shared hedge", []StrategyConfig{hedgeTestConfig("a", "ETH", "BTC"), hedgeTestConfig("b", "SOL", "BTC")}, "shared by hedge-enabled strategies"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(&Config{Strategies: tc.strategies}, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateHedgeConfigRejectsUnsupportedShape(t *testing.T) {
	tests := []struct {
		name string
		edit func(*StrategyConfig)
		want string
	}{
		{"paper", func(sc *StrategyConfig) { sc.Args[2] = "--mode=paper" }, "live Hyperliquid perps"},
		{"side", func(sc *StrategyConfig) { sc.Hedge.Side = "same" }, "side must be"},
		{"ratio", func(sc *StrategyConfig) { sc.Hedge.Ratio = 0 }, "ratio must be > 0"},
		{"margin", func(sc *StrategyConfig) { sc.Hedge.MarginMode = "" }, "margin_mode"},
		{"leverage", func(sc *StrategyConfig) { sc.Hedge.Leverage = 0 }, "leverage must"},
		{"rebalance threshold", func(sc *StrategyConfig) { sc.Hedge.RebalanceMinMovePct = 101 }, "rebalance_min_move_pct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestConfig("a", "ETH", "BTC")
			tc.edit(&sc)
			err := validateConfig(&Config{Strategies: []StrategyConfig{sc}}, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestHedgeRebalanceMinMovePctDefaultAndOverride(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	if got := hedgeRebalanceMinMovePct(sc); got != DefaultHedgeRebalanceMinMovePct {
		t.Fatalf("default threshold=%g", got)
	}
	sc.Hedge.RebalanceMinMovePct = 1.25
	if got := hedgeRebalanceMinMovePct(sc); got != 1.25 {
		t.Fatalf("override threshold=%g", got)
	}
}

func TestHedgeHotReloadBlockedWhileOpen(t *testing.T) {
	old := hedgeTestConfig("a", "ETH", "BTC")
	next := old
	clone := *old.Hedge
	clone.Ratio = 0.75
	next.Hedge = &clone
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}},
	}}}
	err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, state)
	if err == nil || !strings.Contains(err.Error(), "hedge changed with an open hedge leg") {
		t.Fatalf("error = %v", err)
	}
}

func TestStrategyHasOpenHedgeLegIgnoresPrimaryOnly(t *testing.T) {
	s := &StrategyState{Positions: map[string]*Position{
		"ETH": {Quantity: 1, Side: "long"},
	}}
	if strategyHasOpenHedgeLeg(s) {
		t.Fatal("primary-only position must not count as hedge")
	}
}

func TestApplyHedgeOpenStampsOwnershipMetadata(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	s := NewStrategyState(sc)
	primary := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	trade := applyHedgeOpen(s, sc, primary, "short", 0.02, 50000, 0.25, "42", nil)
	if trade == nil {
		t.Fatal("expected hedge trade")
	}
	pos := s.Positions["BTC"]
	if pos == nil || !pos.IsHedge || pos.OwnerStrategyID != sc.ID || pos.HedgePrimarySymbol != "ETH" || pos.HedgePrimaryPositionID == "" || pos.TradePositionID != pos.HedgePrimaryPositionID+":hedge" || pos.Multiplier != 1 || pos.Leverage != sc.Hedge.Leverage {
		t.Fatalf("hedge position metadata = %+v", pos)
	}
	if !strings.Contains(trade.Details, "[hedge]") || trade.StrategyID != sc.ID {
		t.Fatalf("hedge trade = %+v", trade)
	}
}

func TestApplyHedgePartialAndFullCloseClearsResidual(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.03, InitialQuantity: 0.03, AvgCost: 50000, Side: "short", IsHedge: true, OwnerStrategyID: sc.ID}
	if !bookPerpsPartialCloseWithFillFee(s, "BTC", 0.01, 49000, 0, false, "", "hedge_partial", "[hedge]", "[hedge]", nil) {
		t.Fatal("partial hedge close failed")
	}
	if got := s.Positions["BTC"].Quantity; got < 0.019999 || got > 0.020001 {
		t.Fatalf("remaining hedge quantity = %g", got)
	}
	if !bookPerpsCloseWithFillFee(s, "BTC", 49000, 0, false, "", "hedge_full", "[hedge]", "[hedge]", nil) {
		t.Fatal("full hedge close failed")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("full hedge close left residual position")
	}
}

func TestSyncStrategyHedge_ScaleInMirrorsNotional(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, AvgCost: 2500, Side: "long"}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.04, InitialQuantity: 0.04, AvgCost: 50000, Side: "short", IsHedge: true}
	var calls []float64
	exec := func(_ string, symbol, side string, size float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
		calls = append(calls, size)
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Symbol: symbol, Action: side, Fill: &HyperliquidFill{AvgPx: 50000, TotalSz: size}}}, "", nil
	}
	_, _, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 3000, "BTC": 50000}, nil, exec, nil, nil, &sync.RWMutex{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] < 0.019999 || calls[0] > 0.020001 {
		t.Fatalf("scale calls = %v, want 0.02", calls)
	}
	if got := s.Positions["BTC"].Quantity; got < 0.059999 || got > 0.060001 {
		t.Fatalf("hedge quantity = %g, want 0.06", got)
	}
}

func TestSyncStrategyHedge_FailedOpenUnwindsPrimary(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	oldCloser := hedgeLiveCloser
	t.Cleanup(func() { hedgeLiveCloser = oldCloser })
	hedgeLiveCloser = func(symbol string, partial *float64, _ []int64) (*HyperliquidCloseResult, error) {
		if symbol != "ETH" || partial != nil {
			t.Fatalf("unexpected rollback %s partial=%v", symbol, partial)
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 1990, TotalSz: 1, OID: 9}}}, nil
	}
	exec := func(_ string, symbol, side string, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
		if symbol == "BTC" {
			return nil, "", fmt.Errorf("hedge unavailable")
		}
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 1}}}, "", nil
	}
	_, unwound, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 2000, "BTC": 50000}, nil, exec, nil, nil, &sync.RWMutex{}, true)
	if err == nil || !unwound {
		t.Fatalf("err=%v unwound=%v", err, unwound)
	}
	if len(s.Positions) != 0 {
		t.Fatalf("positions after unwind = %+v", s.Positions)
	}
}

func TestHedgeClosesDoNotClassifyPrimaryRoundTripOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		primaryPx  float64
		hedgePx    float64
		wantLosses int
	}{
		{name: "winning primary is not inverted by losing hedge", primaryPx: 110, hedgePx: 110, wantLosses: 0},
		{name: "losing primary is not reset by winning hedge", primaryPx: 90, hedgePx: 90, wantLosses: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestConfig("risk-"+strings.ReplaceAll(tc.name, " ", "-"), "ETH", "BTC")
			s := NewStrategyState(sc)
			s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 100, Side: "long"}
			s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
			if !bookPerpsCloseWithFillFee(s, "ETH", tc.primaryPx, 0, true, "1", "test", "primary", "primary", nil) {
				t.Fatal("primary close failed")
			}
			if !bookPerpsCloseWithFillFee(s, "BTC", tc.hedgePx, 0, true, "2", "test", "hedge", "hedge", nil) {
				t.Fatal("hedge close failed")
			}
			if s.RiskState.ConsecutiveLosses != tc.wantLosses {
				t.Fatalf("ConsecutiveLosses=%d want %d", s.RiskState.ConsecutiveLosses, tc.wantLosses)
			}
			if math.Abs(s.RiskState.DailyPnL) > 1e-9 {
				t.Fatalf("DailyPnL=%g, want both realized legs represented and net zero", s.RiskState.DailyPnL)
			}
		})
	}
}

func TestHedgeRebalanceReduceUpdatesDailyPnLWithoutChangingLossStreak(t *testing.T) {
	sc := hedgeTestConfig("risk-rebalance", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.RiskState.DailyPnL = 5
	s.RiskState.DailyPnLDate = todayUTC()
	s.RiskState.ConsecutiveLosses = 3
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 2, AvgCost: 100, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
	if !bookPerpsPartialCloseWithFillFee(s, "BTC", 1, 90, 0, true, "3", "test", "hedge", "hedge", nil) {
		t.Fatal("hedge rebalance reduce failed")
	}
	if s.RiskState.DailyPnL != 15 || s.RiskState.ConsecutiveLosses != 3 {
		t.Fatalf("risk state daily=%g losses=%d, want daily=15 losses=3", s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses)
	}
}

func TestSyncStrategyHedge_HoldTopUpFailureKeepsCoveredPair(t *testing.T) {
	sc := hedgeTestConfig("topup-hold", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, AvgCost: 50000, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
	oldLookup := lookupHedgeOpenFill
	oldCloser := hedgeLiveCloser
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	t.Cleanup(func() {
		lookupHedgeOpenFill = oldLookup
		hedgeLiveCloser = oldCloser
		clearHedgeExposureFailureAlert(sc, "BTC")
	})
	lookupHedgeOpenFill = func(string, string, string, float64, int64) (HLFillLookup, bool) { return HLFillLookup{}, false }
	hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		t.Fatal("hold-cycle top-up failure must not close either leg")
		return nil, nil
	}
	exec := func(string, string, string, float64, string, float64, bool, hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
		return nil, "timeout", fmt.Errorf("timeout")
	}
	for i := 0; i < 2; i++ {
		_, unwound, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 3000, "BTC": 50000}, nil, exec, notifier, nil, &sync.RWMutex{}, false)
		if err == nil || unwound || len(s.Positions) != 2 {
			t.Fatalf("attempt=%d err=%v unwound=%v positions=%+v, want covered HOLD", i, err, unwound, s.Positions)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "existing hedge retained") {
		t.Fatalf("top-up failure DMs=%+v", mock.dms)
	}
}

func TestSyncStrategyHedge_ForcedTopUpFailureRollsBackPrimaryIncrement(t *testing.T) {
	sc := hedgeTestConfig("topup-rollback", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.5, AvgCost: 2000, Side: "long"}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, AvgCost: 50000, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	oldLookup := lookupHedgeOpenFill
	oldFetch := fetchHedgeAccountState
	oldCloser := hedgeLiveCloser
	t.Cleanup(func() {
		lookupHedgeOpenFill = oldLookup
		fetchHedgeAccountState = oldFetch
		hedgeLiveCloser = oldCloser
		clearHedgeExposureFailureAlert(sc, "BTC")
	})
	lookupHedgeOpenFill = func(string, string, string, float64, int64) (HLFillLookup, bool) { return HLFillLookup{}, false }
	fetchHedgeAccountState = func(string) (float64, []HLPosition, error) {
		return 0, []HLPosition{{Coin: "BTC", Size: -0.02}}, nil
	}
	hedgeLiveCloser = func(symbol string, partial *float64, _ []int64) (*HyperliquidCloseResult, error) {
		if symbol != "ETH" || partial == nil || math.Abs(*partial-0.5) > 1e-9 {
			t.Fatalf("rollback close symbol=%s partial=%v", symbol, partial)
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: 0.5, OID: 44}}}, nil
	}
	exec := func(string, string, string, float64, string, float64, bool, hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
		return nil, "rejected", fmt.Errorf("rejected")
	}
	_, unwound, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 2000, "BTC": 50000}, nil, exec, nil, nil, &sync.RWMutex{}, true)
	if err == nil || unwound {
		t.Fatalf("err=%v unwound=%v", err, unwound)
	}
	if got := s.Positions["ETH"].Quantity; math.Abs(got-1) > 1e-9 {
		t.Fatalf("primary qty=%g want 1 after rollback", got)
	}
	if got := s.Positions["BTC"].Quantity; math.Abs(got-0.02) > 1e-9 {
		t.Fatalf("hedge qty=%g want 0.02", got)
	}
}

func TestSyncStrategyHedge_AmbiguousOpenRecoversExactUserFill(t *testing.T) {
	sc := hedgeTestConfig("ambiguous-open", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	oldLookup := lookupHedgeOpenFill
	oldCloser := hedgeLiveCloser
	t.Cleanup(func() {
		lookupHedgeOpenFill = oldLookup
		hedgeLiveCloser = oldCloser
	})
	lookupHedgeOpenFill = func(account, coin, side string, qty float64, _ int64) (HLFillLookup, bool) {
		if account != "0xtest" || coin != "BTC" || side != "short" || math.Abs(qty-0.02) > 1e-9 {
			t.Fatalf("lookup args account=%s coin=%s side=%s qty=%g", account, coin, side, qty)
		}
		return HLFillLookup{OID: 77, FilledQty: 0.02, Px: 50000, Fee: 0.25, Direction: "Open Short"}, true
	}
	hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		t.Fatal("recovered hedge fill must not unwind the primary")
		return nil, nil
	}
	exec := func(string, string, string, float64, string, float64, bool, hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Symbol: "BTC", Action: "sell", Size: 0.02}}, "", nil
	}
	detail, unwound, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 2000, "BTC": 50000}, nil, exec, nil, nil, &sync.RWMutex{}, true)
	if err != nil || unwound || !strings.Contains(detail, "open") {
		t.Fatalf("detail=%q unwound=%v err=%v", detail, unwound, err)
	}
	if hedge := s.Positions["BTC"]; hedge == nil || !hedge.IsHedge || hedge.Quantity != 0.02 {
		t.Fatalf("recovered hedge=%+v", hedge)
	}
}

func TestReconcileHyperliquidHedgePositionRecoversCrashOrphan(t *testing.T) {
	sc := hedgeTestConfig("crash-recover", "ETH", "BTC")
	s := NewStrategyState(sc)
	opened := time.Now().UTC().Add(-time.Minute)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long", OpenedAt: opened}
	resolver := func(coin string, oid int64, qty float64) (HLFillLookup, bool) {
		return HLFillLookup{OID: 88, FilledQty: qty, Px: 50000, Fee: 0.2, Direction: "Open Short", TimeMs: opened.Add(time.Second).UnixMilli()}, true
	}
	if !reconcileHyperliquidHedgePosition(sc, s, "BTC", []HLPosition{{Coin: "BTC", Size: -0.02, EntryPrice: 50000}}, resolver, nil) {
		t.Fatal("expected exact crash-orphan recovery")
	}
	if hedge := s.Positions["BTC"]; hedge == nil || !hedge.IsHedge || hedge.HedgePrimarySymbol != "ETH" {
		t.Fatalf("recovered hedge=%+v", hedge)
	}
}

func TestReconcileHyperliquidAccountPositionsRecoversCrashOrphanEndToEnd(t *testing.T) {
	sc := hedgeTestConfig("crash-recover-e2e", "ETH", "BTC")
	opened := time.Now().UTC().Add(-time.Minute)
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long", OpenedAt: opened}
	state := &AppState{Strategies: map[string]*StrategyState{sc.ID: s}}
	oldFetch := fetchHyperliquidUserFillsByTime
	t.Cleanup(func() { fetchHyperliquidUserFillsByTime = oldFetch })
	fetchHyperliquidUserFillsByTime = func(account string, startTimeMs int64) ([]hlFillRecord, error) {
		if account != "0xtest" || startTimeMs > opened.Add(time.Second).UnixMilli() {
			t.Fatalf("account=%s start=%d", account, startTimeMs)
		}
		return []hlFillRecord{{Coin: "BTC", Sz: "0.02", Px: "50000", Fee: "0.2", OID: "99", Dir: "Open Short", Time: opened.Add(time.Second).UnixMilli()}}, nil
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1, EntryPrice: 2000}, {Coin: "BTC", Size: -0.02, EntryPrice: 50000}}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	changed, _, _ := reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr, positions, map[string]float64{"ETH": 2000, "BTC": 50000}, "0xtest", nil, false)
	if !changed {
		t.Fatal("expected crash-orphan reconciliation change")
	}
	if hedge := s.Positions["BTC"]; hedge == nil || !hedge.IsHedge || hedge.Quantity != 0.02 {
		t.Fatalf("recovered hedge=%+v", hedge)
	}
}

func TestHedgeMetadataPersistsAcrossRestart(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		ID: "a", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", TradePositionID: "primary-1:hedge", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, OwnerStrategyID: "a", IsHedge: true, HedgePrimarySymbol: "ETH", HedgePrimaryPositionID: "primary-1"},
		}, OptionPositions: map[string]*OptionPosition{},
	}}}
	if err := db.SaveState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	h := loaded.Strategies["a"].Positions["BTC"]
	if h == nil || !h.IsHedge || h.HedgePrimarySymbol != "ETH" || h.HedgePrimaryPositionID != "primary-1" {
		t.Fatalf("loaded hedge = %+v", h)
	}
}

func TestReconcileHedgeNeverBooksGuessedClose(t *testing.T) {
	s := &StrategyState{ID: "a", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", TradePositionID: "p:hedge", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH"},
	}}
	changed := reconcileHyperliquidHedgePosition(hedgeTestConfig("a", "ETH", "BTC"), s, "BTC", nil, noFillFeeResolver, nil)
	if changed || s.Positions["BTC"] == nil || len(s.TradeHistory) != 0 {
		t.Fatalf("guessed reconciliation mutated state: changed=%t positions=%+v trades=%+v", changed, s.Positions, s.TradeHistory)
	}
}

func TestReconcileHedgeBooksExactFillToOwnerLedger(t *testing.T) {
	s := &StrategyState{ID: "a", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", TradePositionID: "p:hedge", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH"},
	}}
	resolver := func(coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if coin != "BTC" || oid != 0 || qty != 0.1 {
			t.Fatalf("lookup = %s oid=%d qty=%g", coin, oid, qty)
		}
		return HLFillLookup{OID: 77, FilledQty: 0.1, Px: 49000, Fee: 1.25}, true
	}
	if !reconcileHyperliquidHedgePosition(hedgeTestConfig("a", "ETH", "BTC"), s, "BTC", nil, resolver, nil) {
		t.Fatal("expected exact hedge close reconciliation")
	}
	if s.Positions["BTC"] != nil || len(s.TradeHistory) != 1 {
		t.Fatalf("state = %+v trades=%+v", s.Positions, s.TradeHistory)
	}
	trade := s.TradeHistory[0]
	if trade.StrategyID != "a" || trade.ExchangeOrderID != "77" || trade.ExchangeFee != 1.25 || !trade.IsClose {
		t.Fatalf("ledger trade = %+v", trade)
	}
}

func TestSyncStrategyHedge_MissingMarkUnwindsPrimary(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	oldCloser := hedgeLiveCloser
	t.Cleanup(func() { hedgeLiveCloser = oldCloser })
	hedgeLiveCloser = func(symbol string, partial *float64, _ []int64) (*HyperliquidCloseResult, error) {
		if symbol != "ETH" {
			t.Fatalf("unexpected closer call for %s", symbol)
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 1995, TotalSz: 1, OID: 11}}}, nil
	}
	_, unwound, err := syncStrategyHedge(sc, s, "ETH", map[string]float64{"ETH": 2000}, nil, nil, nil, nil, &sync.RWMutex{}, true)
	if err == nil || !unwound {
		t.Fatalf("err=%v unwound=%v, want fail-closed unwind on missing hedge mark", err, unwound)
	}
	if len(s.Positions) != 0 {
		t.Fatalf("positions after missing-mark unwind = %+v", s.Positions)
	}
}

func TestSyncStrategyHedge_MissingMarksHoldExistingCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prices map[string]float64
	}{
		{name: "total mids failure", prices: map[string]float64{}},
		{name: "hedge coin omitted", prices: map[string]float64{"ETH": 2000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestConfig("a-"+strings.ReplaceAll(tc.name, " ", "-"), "ETH", "BTC")
			s := NewStrategyState(sc)
			s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
			s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, AvgCost: 50000, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
			oldCloser := hedgeLiveCloser
			t.Cleanup(func() { hedgeLiveCloser = oldCloser })
			hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
				t.Fatal("missing marks must not close either existing leg")
				return nil, nil
			}
			exec := func(string, string, string, float64, string, float64, bool, hlExecuteSnapshot) (*HyperliquidExecuteResult, string, error) {
				t.Fatal("missing marks must not place a rebalance order")
				return nil, "", nil
			}
			_, unwound, err := syncStrategyHedge(sc, s, "ETH", tc.prices, nil, exec, nil, nil, &sync.RWMutex{}, false)
			if err != nil || unwound {
				t.Fatalf("err=%v unwound=%v, want hold", err, unwound)
			}
			if len(s.Positions) != 2 {
				t.Fatalf("positions changed on mark gap: %+v", s.Positions)
			}
		})
	}
}

func TestSyncStrategyHedge_CloseFailureAlertsAreEdgeTriggered(t *testing.T) {
	sc := hedgeTestConfig("close-alert", "ETH", "BTC")
	s := NewStrategyState(sc)
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, AvgCost: 50000, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	oldCloser := hedgeLiveCloser
	t.Cleanup(func() {
		hedgeLiveCloser = oldCloser
		clearHedgeCloseFailureAlert(sc, "BTC")
	})
	hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("rate limited")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := syncStrategyHedge(sc, s, "ETH", nil, nil, nil, notifier, nil, &sync.RWMutex{}, false); err == nil {
			t.Fatal("expected hedge close failure")
		}
	}
	mock.mu.Lock()
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "NAKED because the primary is flat") {
		t.Fatalf("first failure episode DMs=%+v", mock.dms)
	}
	mock.mu.Unlock()

	hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 50000, TotalSz: 0.02, OID: 91}}}, nil
	}
	if _, _, err := syncStrategyHedge(sc, s, "ETH", nil, nil, nil, notifier, nil, &sync.RWMutex{}, false); err != nil {
		t.Fatal(err)
	}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, AvgCost: 50000, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"}
	hedgeLiveCloser = func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("rate limited again")
	}
	if _, _, err := syncStrategyHedge(sc, s, "ETH", nil, nil, nil, notifier, nil, &sync.RWMutex{}, false); err == nil {
		t.Fatal("expected second failure episode")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.dms) != 2 {
		t.Fatalf("re-armed failure episode DMs=%+v", mock.dms)
	}
}

func TestNotifyHedgeCloseFailureDistinguishesOversizedLeg(t *testing.T) {
	sc := hedgeTestConfig("oversized-alert", "ETH", "BTC")
	clearHedgeCloseFailureAlert(sc, "BTC")
	t.Cleanup(func() { clearHedgeCloseFailureAlert(sc, "BTC") })
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	notifyHedgeCloseFailure(sc, "BTC", false, 0.01, true, fmt.Errorf("rejected"), notifier)
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "oversized while the primary remains open") || !strings.Contains(mock.dms[0].content, "reduce") {
		t.Fatalf("DMs=%+v", mock.dms)
	}
}

func TestHedgeHotReloadAllowedWhenFlat(t *testing.T) {
	old := hedgeTestConfig("a", "ETH", "BTC")
	next := old
	clone := *old.Hedge
	clone.Ratio = 0.75
	next.Hedge = &clone
	state := &AppState{Strategies: map[string]*StrategyState{"a": {Positions: map[string]*Position{}}}}
	if err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, state); err != nil {
		t.Fatalf("flat hedge reload should be allowed: %v", err)
	}
}

func TestCollectPerpsMarkSymbolsIncludesHedgeCoin(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	hl, okx := collectPerpsMarkSymbols([]StrategyConfig{sc})
	if len(okx) != 0 {
		t.Fatalf("okx coins = %v", okx)
	}
	want := map[string]bool{"ETH": true, "BTC": true}
	for _, c := range hl {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Fatalf("missing hedge/primary marks: %v (got %v)", want, hl)
	}
}

func TestForceCloseHyperliquidLiveIncludesConfiguredAndOrphanHedgeCoins(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	positions := []HLPosition{
		{Coin: "ETH", Size: 1},
		{Coin: "BTC", Size: -0.1},
		{Coin: "SOL", Size: -2}, // orphan IsHedge claim only
	}
	closed := map[string]bool{}
	closer := func(coin string, _ *float64, _ []int64) (*HyperliquidCloseResult, error) {
		closed[coin] = true
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 1, TotalSz: 1}}}, nil
	}
	report := forceCloseHyperliquidLive(context.Background(), positions, []StrategyConfig{sc}, closer, nil, map[string]bool{"SOL": true})
	for _, coin := range []string{"ETH", "BTC", "SOL"} {
		if !closed[coin] {
			t.Fatalf("expected kill-switch to close %s; closed=%v report=%+v", coin, closed, report)
		}
	}
}
