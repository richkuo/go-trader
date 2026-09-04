package main

import (
	"strings"
	"testing"
	"time"
)

func expiredCBLatch(peak, maxDD float64, losses int) RiskState {
	return RiskState{
		PeakValue:           peak,
		MaxDrawdownPct:      maxDD,
		ConsecutiveLosses:   losses,
		CircuitBreaker:      true,
		CircuitBreakerUntil: time.Now().UTC().Add(-time.Minute),
		DailyPnLDate:        todayUTC(),
	}
}

func TestCheckRisk_CooldownExpired(t *testing.T) {
	cases := []struct {
		name               string
		stratType          string
		cash               float64
		leverage           float64
		latch              RiskState
		maxDrawdownPct     float64
		wantAllowed        bool
		wantReasonPrefix   string
		wantReasonContains string
		wantReasonNot      string
		wantRelatch        bool
		wantCBCleared      bool
		wantLossesZero     bool
		wantDDMin          float64
		wantDDMax          float64
	}{
		{
			name: "drawdown_still_breached_relatches", stratType: "spot", cash: 7000,
			latch: expiredCBLatch(10000, 20, 0), maxDrawdownPct: 20,
			wantAllowed: false, wantReasonPrefix: RiskReasonMaxDrawdownExceeded,
			wantReasonNot: RiskReasonCircuitBreakerActive, wantRelatch: true,
		},
		{
			name: "drawdown_recovered_allows", stratType: "spot", cash: 9500,
			latch: expiredCBLatch(10000, 20, 0), maxDrawdownPct: 20,
			wantAllowed: true, wantCBCleared: true, wantLossesZero: true,
		},
		{
			name: "loss_streak_resets_and_allows", stratType: "spot", cash: 10000,
			latch: expiredCBLatch(10000, 50, 5), maxDrawdownPct: 50,
			wantAllowed: true, wantCBCleared: true, wantLossesZero: true,
		},
		{
			name: "perps_flat_peak_relative_under_threshold_allows", stratType: "perps", cash: 900, leverage: 20,
			latch: expiredCBLatch(1000, 25, 0), maxDrawdownPct: 25,
			wantAllowed: true, wantCBCleared: true, wantDDMin: 9, wantDDMax: 11,
		},
		{
			name: "perps_flat_peak_relative_still_breached_relatches", stratType: "perps", cash: 700, leverage: 20,
			latch: expiredCBLatch(1000, 25, 0), maxDrawdownPct: 25,
			wantAllowed: false, wantReasonPrefix: RiskReasonMaxDrawdownExceeded,
			wantReasonContains: "denom=peak=", wantRelatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StrategyState{
				ID:              "cb-expiry-" + tc.name,
				Type:            tc.stratType,
				Cash:            tc.cash,
				RiskState:       tc.latch,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			}
			sc := &StrategyConfig{ID: s.ID, Type: tc.stratType, Leverage: tc.leverage, MaxDrawdownPct: tc.maxDrawdownPct}

			before := time.Now().UTC()
			allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v (reason=%q)", allowed, tc.wantAllowed, reason)
			}
			if tc.wantReasonPrefix != "" && !strings.HasPrefix(reason, tc.wantReasonPrefix) {
				t.Fatalf("reason = %q, want %q prefix", reason, tc.wantReasonPrefix)
			}
			if tc.wantReasonContains != "" && !strings.Contains(reason, tc.wantReasonContains) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.wantReasonContains)
			}
			if tc.wantReasonNot != "" && reason == tc.wantReasonNot {
				t.Fatalf("reason = %q must not be the latched reason after cooldown expiry", reason)
			}
			if tc.wantRelatch {
				assertCBLatchDuration(t, s, before, 24*time.Hour)
			}
			if tc.wantCBCleared && s.RiskState.CircuitBreaker {
				t.Fatal("CircuitBreaker must be cleared after a healthy expiry-cycle check")
			}
			if tc.wantLossesZero && s.RiskState.ConsecutiveLosses != 0 {
				t.Fatalf("ConsecutiveLosses = %d, want 0 after expiry clear", s.RiskState.ConsecutiveLosses)
			}
			if tc.wantDDMax > 0 {
				if s.RiskState.CurrentDrawdownPct < tc.wantDDMin || s.RiskState.CurrentDrawdownPct > tc.wantDDMax {
					t.Fatalf("CurrentDrawdownPct = %.2f, want between %.2f and %.2f (peak-relative, not margin-based)",
						s.RiskState.CurrentDrawdownPct, tc.wantDDMin, tc.wantDDMax)
				}
			}
		})
	}
}
