package main

import (
	"strings"
	"testing"
	"time"
)

// #1449 review round 3 — the UPWARD direction of an untrusted equity reading.
//
// Round 2 closed the fail-open direction: an untrusted total that OVERSTATES
// equity can no longer mask a loss, because the measured drawdown is floored at
// the last recorded reading. The opposite direction stayed open. An untrusted
// total that UNDERSTATES equity inflates the drawdown, and nothing stopped that
// inflated number from latching the portfolio and flattening the whole book —
// manual and spot included — off a total the same cycle already flagged as
// substituted or aged.
//
// The fix is a DEFERRAL with an escalation deadline, not a veto. These tests
// pin both halves, because each one alone is a hole: a veto disarms the only
// full-book protection for as long as a balance endpoint stays down, and no
// deferral at all is the spurious flatten the finding reported.

// TestUntrustedEquity_OverLimitLatchIsDeferredNotVetoed covers the reviewer's
// three must-survive cases.
func TestUntrustedEquity_OverLimitLatchIsDeferredNotVetoed(t *testing.T) {
	cfg := review1449Config() // 25% limit, warn band at 15%

	// (a) Untrusted cycle, real equity healthy, but the substituted total
	// reads a 40% drawdown. The book must stay open.
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
	// The reading itself is the real measurement, not a clamped one: the
	// operator must see the 40%, and the deferral is recorded separately.
	if prs.CurrentDrawdownPct != 40 {
		t.Errorf("a deferred cycle must persist its own measurement; got %.1f want 40", prs.CurrentDrawdownPct)
	}
	// Entering the deferral is an auditable transition.
	if n := countKillSwitchEvents(prs, "latch_deferred"); n != 1 {
		t.Errorf("expected exactly one latch_deferred event on entry; got %d", n)
	}

	// A second untrusted cycle inside the window must not re-stamp the start
	// (that would push the deadline out forever) and must not add a second
	// event.
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

	// (b) A genuine TRUSTED measurement above the limit latches immediately —
	// the deferral must never delay a real one.
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

	// (c) A sustained run of untrusted over-limit cycles must reach protection
	// rather than defer forever. Age the window past the deadline.
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

// TestUntrustedEquity_DeferralWindowClearsOnEveryNonQualifyingCycle pins the
// inverse of the reported scenario. The window must measure an UNBROKEN run:
// if any intervening cycle is trusted, or reads at or below the limit, or has
// the equity guard unarmed, the clock restarts. Otherwise a book that dips
// over the limit for one cycle a day would accumulate toward an escalation it
// never earned.
func TestUntrustedEquity_DeferralWindowClearsOnEveryNonQualifyingCycle(t *testing.T) {
	cfg := review1449Config()

	openWindow := func() *PortfolioRiskState {
		prs := &PortfolioRiskState{PeakValue: 10000}
		if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 6000, 0, 0, 0, true, false); prs.UntrustedOverLimitSince.IsZero() {
			t.Fatal("setup: expected a deferral window")
		}
		return prs
	}

	// A trusted cycle that reads healthy clears the window.
	prs := openWindow()
	if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, true); !prs.UntrustedOverLimitSince.IsZero() {
		t.Error("a trusted cycle must clear the deferral window")
	}

	// An untrusted cycle back at or below the limit clears it too. The floor
	// holds the reading at the clamped 25%, which is NOT above the limit, so
	// the strict > that governs the latch also governs the window.
	prs = openWindow()
	if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false); !prs.UntrustedOverLimitSince.IsZero() {
		t.Errorf("an untrusted cycle at the clamped limit must clear the window (reading %.1f)", prs.CurrentDrawdownPct)
	}

	// Equity guard unarmed: the equity arm is not the one that can latch, so
	// it cannot be accumulating toward an escalation either.
	prs = openWindow()
	if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 6000, 0, 0, 0, false, false); !prs.UntrustedOverLimitSince.IsZero() {
		t.Error("an unarmed equity guard must clear the deferral window")
	}
}

// TestUntrustedEquity_DeferralDoesNotDisarmTheMarginArm pins that the deferral
// is scoped to the equity arm. On the equityAvailable == false path margin owns
// the latch (#1448) and nothing about an untrusted equity reading may weaken
// it — that path is the standing backstop the deferral leans on.
func TestUntrustedEquity_DeferralDoesNotDisarmTheMarginArm(t *testing.T) {
	cfg := review1449Config()
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity unavailable (pooled wallet with no trustworthy balance), margin
	// drawdown 40% on $100 of deployed margin.
	allowed, _, _, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 0, 0, 40, 100, false, false)
	if allowed || !prs.KillSwitchActive {
		t.Fatal("the margin arm must still latch when the equity guard is unarmed")
	}
	if !strings.Contains(reason, "margin drawdown") {
		t.Errorf("expected a margin-sourced latch reason; got %q", reason)
	}
	if !prs.UntrustedOverLimitSince.IsZero() {
		t.Error("the margin arm must not open an equity deferral window")
	}
}

// TestUntrustedEquity_DeferralWindowSurvivesReset pins that every reset path
// clears the window. A window left set across a reset would let the next
// untrusted over-limit cycle escalate straight to a latch, skipping the whole
// deferral the operator was just told about.
func TestUntrustedEquity_DeferralWindowSurvivesReset(t *testing.T) {
	since := time.Now().UTC().Add(-time.Minute)

	manual := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 40, KillSwitchActive: true, UntrustedOverLimitSince: since}
	if got := ResetPortfolioKillSwitchManual(manual); got != 40 {
		t.Errorf("manual reset must return the pre-reset reading; got %.1f", got)
	}
	if !manual.UntrustedOverLimitSince.IsZero() {
		t.Error("the manual DM reset must clear the deferral window")
	}

	auto := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 40, KillSwitchActive: true, UntrustedOverLimitSince: since}
	if !AutoResetConfirmedFlatKillSwitch(auto, 9000, true, "confirmed flat") {
		t.Fatal("auto reset must run on a latched state")
	}
	if !auto.UntrustedOverLimitSince.IsZero() {
		t.Error("the confirmed-flat auto reset must clear the deferral window")
	}
}

// TestUntrustedEquity_DegenerateLimitKeepsExistingMeaning pins that a
// non-positive MaxDrawdownPct is untouched. It already means "latch on any
// drawdown"; deferring that would silently redefine the degenerate config
// instead of leaving it alone, exactly as the priorEquityDD clamp does.
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

// TestPortfolioWarningMessage_NamesTheDeferredLatch pins the operator surface.
// With the deferral active the equity distance is 0.0%, which on every other
// path means "a flatten is imminent". Printing the bare number there would
// send an operator to intervene against a close that is not coming.
func TestPortfolioWarningMessage_NamesTheDeferredLatch(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: PortfolioRiskState{
			PeakValue:               10000,
			CurrentDrawdownPct:      40,
			UntrustedOverLimitSince: time.Now().UTC().Add(-time.Minute),
		},
	}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Reason:           "portfolio equity drawdown 40.0% exceeds limit 25.0%",
		Config:           review1449Config(),
		State:            state,
		TotalValue:       6000,
		Now:              time.Now().UTC(),
		EquityGuardArmed: true,
	})
	if !strings.Contains(msg, "DEFERRED") {
		t.Errorf("the warning message must name the deferred latch; got:\n%s", msg)
	}
	if strings.Contains(msg, "Distance to kill switch: 0.0% equity") {
		t.Errorf("the deferred path must not print a bare 0.0%% distance; got:\n%s", msg)
	}
	if !strings.Contains(msg, "#292") {
		t.Errorf("the message must name what is protecting the book meanwhile; got:\n%s", msg)
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
