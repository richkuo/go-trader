package main

import (
	"strings"
	"testing"
	"time"
)

func TestUntrustedEquity_OverLimitLatchIsDeferredNotVetoed(t *testing.T) {
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
}

func TestUntrustedEquity_DegenerateLimitKeepsExistingMeaning(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 0, WarnThresholdPct: 60}
	prs := &PortfolioRiskState{PeakValue: 10000}

	allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 9000, 0, 0, 0, true, false)
	if allowed || !prs.KillSwitchActive {
		t.Fatal("a non-positive limit must keep latching on any drawdown, untrusted or not")
	}
	if !prs.UntrustedOverLimitSince.IsZero() {
		t.Error("a non-positive limit must not open a deferral window")
	}
}

func countKillSwitchEvents(prs *PortfolioRiskState, eventType string) int {
	n := 0
	for _, e := range prs.Events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}
