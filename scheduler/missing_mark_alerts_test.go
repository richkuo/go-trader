package main

import (
	"strings"
	"testing"
	"time"
)

// TestMissingMarkTracker_ThrottlesPerStrategySymbol pins the review's first
// must-survive case: a multi-cycle mark outage must not send one DM per
// position per cycle. Each (strategy, symbol) slot fires once per window, and
// two positions inside the same outage stay independent.
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
	// Same outage, later cycles inside the window: silent.
	for i := 1; i <= 20; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if tr.Record(btc, at) {
			t.Fatalf("BTC re-notified %v into the throttle window", at.Sub(base))
		}
		if tr.Record(sol, at) {
			t.Fatalf("SOL re-notified %v into the throttle window", at.Sub(base))
		}
	}
	// Window elapsed: re-alert so a still-broken protection is not forgotten.
	if !tr.Record(btc, base.Add(6*time.Hour)) {
		t.Error("BTC should re-notify once the throttle window elapses")
	}
}

// TestMissingMarkTracker_RetainRearmsAfterMarkReturns pins the review's third
// must-survive case: once the mark returns the alert stops, and a LATER outage
// notifies immediately — no restart, and no hiding behind the first outage's
// window.
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
	// Mark returns: this cycle reports no misses at all.
	tr.Retain(nil)
	// Mark drops again, still well inside the original 6h window.
	tr.Retain([]missingMarkPosition{miss})
	if !tr.Record(miss, base.Add(10*time.Minute)) {
		t.Error("a NEW outage after a recovery must notify immediately")
	}
}

// TestMissingMarkTracker_RetainKeepsOtherSlots verifies Retain prunes only the
// positions that recovered, so an ongoing outage elsewhere keeps its window.
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
	// SOL recovers, BTC still missing.
	tr.Retain([]missingMarkPosition{btc})
	if tr.Record(btc, base.Add(time.Minute)) {
		t.Error("BTC kept missing: its throttle window must survive Retain")
	}
	tr.Retain([]missingMarkPosition{btc, sol})
	if !tr.Record(sol, base.Add(2*time.Minute)) {
		t.Error("SOL recovered then re-missed: must notify again")
	}
}

// TestFormatMissingMarkDM_NamesDisabledProtections keeps the operator-facing
// text actionable: it must say which auto-protective mechanisms stopped, not
// merely that a price is missing.
//
// #1445 review must-survive (a): a live HL perps/manual miss still names the
// walker and the ratchet.
func TestFormatMissingMarkDM_NamesDisabledProtections(t *testing.T) {
	miss := missingMarkPosition{
		StrategyID: "hl-live", Symbol: "BTC", Live: true,
		Platform: "hyperliquid", Type: "perps",
		DisabledManagers: markGatedManagers(StrategyConfig{Type: "perps", Platform: "hyperliquid"}),
	}
	got := formatMissingMarkDM(miss)
	for _, want := range []string{"hl-live", "BTC", "Trailing stop-loss walker", "Take-profit ratchet"} {
		if !strings.Contains(got, want) {
			t.Errorf("DM missing %q:\n%s", want, got)
		}
	}
}

// TestFormatMissingMarkDM_ManualNamesDisabledProtections is the manual half of
// must-survive (a) — manual runs the same two HL mechanisms.
func TestFormatMissingMarkDM_ManualNamesDisabledProtections(t *testing.T) {
	miss := missingMarkPosition{
		StrategyID: "manual-hl", Symbol: "HYPE", Live: true,
		Platform: "hyperliquid", Type: "manual",
		DisabledManagers: markGatedManagers(StrategyConfig{Type: "manual", Platform: "hyperliquid"}),
	}
	got := formatMissingMarkDM(miss)
	for _, want := range []string{"manual-hl", "HYPE", "Trailing stop-loss walker", "Take-profit ratchet"} {
		if !strings.Contains(got, want) {
			t.Errorf("DM missing %q:\n%s", want, got)
		}
	}
}

// TestFormatMissingMarkDM_NonHLVenuesClaimNoHLMechanism is must-survive (b)
// and (c): a live BinanceUS spot or OKX-perps miss must not tell the operator
// that a Hyperliquid walker or ratchet stopped — those managers do not exist
// on those venues, so the claim would be false. The DM must still carry the
// one consequence that IS true everywhere: the portfolio kill switch reads a
// stale valuation for this position.
func TestFormatMissingMarkDM_NonHLVenuesClaimNoHLMechanism(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
		id   string
		sym  string
	}{
		{"binanceus spot", StrategyConfig{Type: "spot", Platform: "binanceus"}, "sma-eth", "ETH/USDT"},
		{"okx perps", StrategyConfig{Type: "perps", Platform: "okx"}, "okx-sol", "SOL"},
		{"topstep futures", StrategyConfig{Type: "futures", Platform: "topstep"}, "ts-es", "ES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			miss := missingMarkPosition{
				StrategyID: tc.id, Symbol: tc.sym, Live: true,
				Platform: tc.sc.Platform, Type: tc.sc.Type,
				DisabledManagers: markGatedManagers(tc.sc),
			}
			got := formatMissingMarkDM(miss)
			for _, banned := range []string{"Trailing stop-loss walker", "Take-profit ratchet"} {
				if strings.Contains(got, banned) {
					t.Errorf("DM claims %q on %s, which never runs it:\n%s", banned, tc.sc.Platform, got)
				}
			}
			for _, want := range []string{tc.id, tc.sym, tc.sc.Platform, "falls back to entry cost"} {
				if !strings.Contains(got, want) {
					t.Errorf("DM missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestFormatManualMarkBasisRebaselineDM_StatesDrawdownNotReset pins the
// operator-facing claim that matters: only the units moved. A DM that read as
// "drawdown cleared" would hide an armed kill switch.
func TestFormatManualMarkBasisRebaselineDM_StatesDrawdownNotReset(t *testing.T) {
	got := formatManualMarkBasisRebaselineDM(60000, 56000, 56000, 60000)
	for _, want := range []string{"60000.00", "56000.00", "NOT reset", "-4000.00"} {
		if !strings.Contains(got, want) {
			t.Errorf("DM missing %q:\n%s", want, got)
		}
	}
}
