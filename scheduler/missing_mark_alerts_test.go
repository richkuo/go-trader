package main

import (
	"strings"
	"testing"
	"time"
)

func TestMissingMarkTracker_ThrottlesPerStrategySymbol(t *testing.T) {
	prev := effectiveAlertThrottleInterval()
	applyAlertThrottleInterval(6 * time.Hour)
	defer applyAlertThrottleInterval(prev)

	tr := &missingMarkTracker{}
	btc := missingMarkPosition{StrategyID: "hl-live", Symbol: "BTC", Live: true}
	sol := missingMarkPosition{StrategyID: "hl-live", Symbol: "SOL", Live: true}
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

	if !tr.Record(btc, base) {
		t.Fatal("first BTC miss should notify")
	}
	if !tr.Record(sol, base) {
		t.Fatal("first SOL miss should notify — slots are per symbol")
	}
	for i := 1; i <= 20; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if tr.Record(btc, at) {
			t.Fatalf("BTC re-notified %v into the throttle window", at.Sub(base))
		}
		if tr.Record(sol, at) {
			t.Fatalf("SOL re-notified %v into the throttle window", at.Sub(base))
		}
	}
	if !tr.Record(btc, base.Add(6*time.Hour)) {
		t.Error("BTC should re-notify once the throttle window elapses")
	}
}

func TestMissingMarkTracker_RetainRearmsAfterMarkReturns(t *testing.T) {
	prev := effectiveAlertThrottleInterval()
	applyAlertThrottleInterval(6 * time.Hour)
	defer applyAlertThrottleInterval(prev)

	tr := &missingMarkTracker{}
	miss := missingMarkPosition{StrategyID: "hl-live", Symbol: "BTC", Live: true}
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

	tr.Retain([]missingMarkPosition{miss})
	if !tr.Record(miss, base) {
		t.Fatal("first miss should notify")
	}
	if tr.Record(miss, base.Add(time.Minute)) {
		t.Fatal("second miss inside the window should stay silent")
	}
	tr.Retain(nil)
	tr.Retain([]missingMarkPosition{miss})
	if !tr.Record(miss, base.Add(10*time.Minute)) {
		t.Error("a NEW outage after a recovery must notify immediately")
	}
}

func TestMissingMarkTracker_RetainKeepsOtherSlots(t *testing.T) {
	prev := effectiveAlertThrottleInterval()
	applyAlertThrottleInterval(6 * time.Hour)
	defer applyAlertThrottleInterval(prev)

	tr := &missingMarkTracker{}
	btc := missingMarkPosition{StrategyID: "hl-live", Symbol: "BTC", Live: true}
	sol := missingMarkPosition{StrategyID: "hl-live", Symbol: "SOL", Live: true}
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

	tr.Record(btc, base)
	tr.Record(sol, base)
	tr.Retain([]missingMarkPosition{btc})
	if tr.Record(btc, base.Add(time.Minute)) {
		t.Error("BTC kept missing: its throttle window must survive Retain")
	}
	tr.Retain([]missingMarkPosition{btc, sol})
	if !tr.Record(sol, base.Add(2*time.Minute)) {
		t.Error("SOL recovered then re-missed: must notify again")
	}
}

func TestFormatMissingMarkDM_NamesOnlyProtectionsThatRun(t *testing.T) {
	hlManagers := []string{"Trailing stop-loss walker", "Take-profit ratchet"}
	cases := []struct {
		name       string
		sc         StrategyConfig
		id         string
		sym        string
		wantHLMgrs bool
	}{
		{"hl perps", StrategyConfig{Type: "perps", Platform: "hyperliquid"}, "hl-live", "BTC", true},
		{"hl manual", StrategyConfig{Type: "manual", Platform: "hyperliquid"}, "manual-hl", "HYPE", true},
		{"binanceus spot", StrategyConfig{Type: "spot", Platform: "binanceus"}, "sma-eth", "ETH/USDT", false},
		{"okx perps", StrategyConfig{Type: "perps", Platform: "okx"}, "okx-sol", "SOL", false},
		{"topstep futures", StrategyConfig{Type: "futures", Platform: "topstep"}, "ts-es", "ES", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			miss := missingMarkPosition{
				StrategyID: tc.id, Symbol: tc.sym, Live: true,
				Platform: tc.sc.Platform, Type: tc.sc.Type,
				DisabledManagers: markGatedManagers(tc.sc),
			}
			got := formatMissingMarkDM(miss)
			for _, mgr := range hlManagers {
				if has := strings.Contains(got, mgr); has != tc.wantHLMgrs {
					t.Errorf("%s: manager %q named=%v, want %v:\n%s", tc.sc.Platform, mgr, has, tc.wantHLMgrs, got)
				}
			}
			for _, want := range []string{tc.id, tc.sym} {
				if !strings.Contains(got, want) {
					t.Errorf("DM missing %q:\n%s", want, got)
				}
			}
		})
	}
}
