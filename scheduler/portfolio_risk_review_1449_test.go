package main

import (
	"strings"
	"testing"
	"time"
)

func review1449Config() *PortfolioRiskConfig {
	return &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60}
}

func TestUntrustedEquityReview1449(t *testing.T) {
	t.Run("FreezesPeakAndFloorsDrawdown", func(t *testing.T) {
		prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 10}
		cfg := review1449Config()

		allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false)
		if !allowed {
			t.Fatal("floored drawdown must never latch on its own")
		}
		if prs.CurrentDrawdownPct != 10 {
			t.Errorf("untrusted total must not lower the measured drawdown; got %.1f want 10", prs.CurrentDrawdownPct)
		}

		allowed, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 12000, 0, 0, 0, true, false)
		if !allowed {
			t.Fatal("unexpected latch on an untrusted over-peak substitute")
		}
		if prs.PeakValue != 10000 {
			t.Errorf("untrusted total must not ratchet the peak; got %.2f want 10000", prs.PeakValue)
		}
		if prs.CurrentDrawdownPct != 10 {
			t.Errorf("floor lost after an over-peak substitute; got %.1f want 10", prs.CurrentDrawdownPct)
		}

		if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, true); prs.CurrentDrawdownPct != 0 {
			t.Errorf("trusted cycle must overwrite the floor; got %.1f want 0", prs.CurrentDrawdownPct)
		}
		if prs.PeakValue != 10000 {
			t.Errorf("trusted equal-to-peak total moved the peak; got %.2f", prs.PeakValue)
		}
	})
	t.Run("FloorAloneCannotLatch", func(t *testing.T) {
		prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 24.9}
		cfg := review1449Config()

		allowed, _, warning, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false)
		if !allowed || prs.KillSwitchActive {
			t.Fatal("a floor at 24.9% must not latch against a 25% limit")
		}
		if !warning {
			t.Error("expected the floored reading to stay in the warn band")
		}

		allowed, _, _, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 7000, 0, 0, 0, true, false)
		if !allowed || prs.KillSwitchActive {
			t.Fatalf("an untrusted 30%% reading must defer, not latch; reason=%q", reason)
		}
		if prs.CurrentDrawdownPct != 30 {
			t.Errorf("the clamp must not swallow a real measurement; got %.1f want 30", prs.CurrentDrawdownPct)
		}
		if prs.UntrustedOverLimitSince.IsZero() {
			t.Error("an untrusted over-limit reading must open the deferral window")
		}

		trusted := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 24.9}
		allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(trusted, cfg, 7000, 0, 0, 0, true, true)
		if allowed {
			t.Fatal("a real 30% drawdown must latch on a trusted cycle")
		}
		if !strings.Contains(reason, "portfolio drawdown") || strings.Contains(reason, "margin") {
			t.Errorf("expected an equity-sourced latch reason; got %q", reason)
		}
		if len(trusted.Events) != 1 || trusted.Events[0].Source != "equity" {
			t.Errorf("expected one triggered event with Source=equity; got %+v", trusted.Events)
		}
	})
	t.Run("LatchStaysWithEquity", func(t *testing.T) {
		cfg := review1449Config()
		prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 9.8}

		allowed, _, warning, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 9020, 0, 31.6, 48.42, true, false)
		if !allowed || prs.KillSwitchActive {
			t.Fatalf("margin must not latch while equity can measure, trusted or not; reason=%q", reason)
		}
		if !warning {
			t.Error("an over-limit margin reading must still raise the warn band")
		}

		prs2 := &PortfolioRiskState{PeakValue: 10000}
		allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(prs2, cfg, 0, 0, 31.6, 48.42, false, false)
		if allowed {
			t.Fatal("margin must latch when the equity guard cannot measure at all")
		}
		if len(prs2.Events) != 1 || prs2.Events[0].Source != "margin" {
			t.Errorf("expected one triggered event with Source=margin; got %+v", prs2.Events)
		}
	})
	t.Run("OverLimitLatchIsDeferredNotVetoed", func(t *testing.T) {
		cfg := review1449Config()

		prs := &PortfolioRiskState{PeakValue: 10000}
		allowed, _, warning, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 6000, 0, 0, 0, true, false)
		if !allowed || prs.KillSwitchActive {
			t.Fatalf("an untrusted over-limit total must not latch on the cycle it appears; reason=%q", reason)
		}
		if !warning {
			t.Error("a deferred latch must still raise the portfolio warning")
		}
		if prs.UntrustedOverLimitSince.IsZero() {
			t.Fatal("the deferral window must be stamped on the first qualifying cycle")
		}
		if !strings.Contains(reason, "DEFERRED") {
			t.Errorf("the warn reason must name the deferral; got %q", reason)
		}
		if prs.CurrentDrawdownPct != 40 {
			t.Errorf("a deferred cycle must persist its own measurement; got %.1f want 40", prs.CurrentDrawdownPct)
		}
		if n := countKillSwitchEvents(prs, "latch_deferred"); n != 1 {
			t.Errorf("expected exactly one latch_deferred event on entry; got %d", n)
		}

		firstSince := prs.UntrustedOverLimitSince
		if allowed, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 6000, 0, 0, 0, true, false); !allowed {
			t.Fatal("second untrusted cycle inside the window must still be deferred")
		}
		if !prs.UntrustedOverLimitSince.Equal(firstSince) {
			t.Error("the deferral window must not restart on a repeat untrusted cycle")
		}
		if n := countKillSwitchEvents(prs, "latch_deferred"); n != 1 {
			t.Errorf("the deferral event must fire once per window, not per cycle; got %d", n)
		}

		trusted := &PortfolioRiskState{PeakValue: 10000}
		allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(trusted, cfg, 6000, 0, 0, 0, true, true)
		if allowed || !trusted.KillSwitchActive {
			t.Fatal("a trusted over-limit measurement must latch on the same cycle")
		}
		if !trusted.UntrustedOverLimitSince.IsZero() {
			t.Error("a trusted cycle must never open a deferral window")
		}
		if strings.Contains(reason, "UNTRUSTED") {
			t.Errorf("a trusted latch reason must not claim an untrusted basis; got %q", reason)
		}

		aged := &PortfolioRiskState{
			PeakValue:               10000,
			UntrustedOverLimitSince: time.Now().UTC().Add(-untrustedEquityLatchDeferral - time.Minute),
		}
		allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(aged, cfg, 6000, 0, 0, 0, true, false)
		if allowed || !aged.KillSwitchActive {
			t.Fatal("the deferral must escalate to a latch once the window elapses")
		}
		if !strings.Contains(reason, "UNTRUSTED") || !strings.Contains(reason, "escalated") {
			t.Errorf("an escalated latch must name the untrusted basis; got %q", reason)
		}
		if !aged.UntrustedOverLimitSince.IsZero() {
			t.Error("the window must be cleared once the latch fires")
		}
	})
	t.Run("DegenerateLimitKeepsExistingMeaning", func(t *testing.T) {
		cfg := &PortfolioRiskConfig{MaxDrawdownPct: 0, WarnThresholdPct: 60}
		prs := &PortfolioRiskState{PeakValue: 10000}

		allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 9000, 0, 0, 0, true, false)
		if allowed || !prs.KillSwitchActive {
			t.Fatal("a non-positive limit must keep latching on any drawdown, untrusted or not")
		}
		if !prs.UntrustedOverLimitSince.IsZero() {
			t.Error("a non-positive limit must not open a deferral window")
		}
	})
	t.Run("ManualKillSwitchReset_ClearsStaleReadings", func(t *testing.T) {
		prs := &PortfolioRiskState{
			PeakValue:                  10000,
			CurrentDrawdownPct:         40,
			CurrentMarginDrawdownPct:   65,
			DrawdownReadingSubstituted: true,
			KillSwitchActive:           true,
			KillSwitchAt:               time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC),
		}

		prior := ResetPortfolioKillSwitchManual(prs)
		if prior != 40 {
			t.Errorf("reset must return the pre-reset reading for the audit event; got %.1f want 40", prior)
		}
		if prs.KillSwitchActive || !prs.KillSwitchAt.IsZero() {
			t.Error("reset must clear the latch")
		}
		if prs.CurrentDrawdownPct != 0 || prs.CurrentMarginDrawdownPct != 0 {
			t.Errorf("reset must clear both drawdown readings; got equity=%.1f margin=%.1f",
				prs.CurrentDrawdownPct, prs.CurrentMarginDrawdownPct)
		}
		if prs.DrawdownReadingSubstituted {
			t.Error("reset must clear the substituted marker")
		}
		if prs.PeakValue != 10000 {
			t.Errorf("manual reset must retain the real high-water mark; got %.2f", prs.PeakValue)
		}
		cfg := review1449Config()
		if allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false); !allowed {
			t.Fatal("a freshly reset portfolio must not re-latch on the next untrusted cycle")
		}
		if ResetPortfolioKillSwitchManual(nil) != 0 {
			t.Error("nil state must be a no-op")
		}
	})
}
