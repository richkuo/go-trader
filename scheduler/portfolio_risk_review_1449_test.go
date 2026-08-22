package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func review1449Config() *PortfolioRiskConfig {
	// Kill switch at 25%, warn band opens at 15%.
	return &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60}
}

// TestUntrustedEquity_FreezesPeakAndFloorsDrawdown covers the #1449 review
// finding that pooledEquityComplete only says the equity total EXISTS. A
// substituted (sum-of-member-PV) or one-generation-stale total leaves it true,
// and before this the latch ran solely off that number with the peak rolled
// back only afterwards by the caller.
func TestUntrustedEquity_FreezesPeakAndFloorsDrawdown(t *testing.T) {
	// Prior trusted cycle measured a 10% drawdown against a $10,000 peak.
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 10}
	cfg := review1449Config()

	// Untrusted cycle whose substitute OVERSTATES equity back to the peak.
	// Raw equityDD would be 0 and would erase the measured loss.
	allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false)
	if !allowed {
		t.Fatal("floored drawdown must never latch on its own")
	}
	if prs.CurrentDrawdownPct != 10 {
		t.Errorf("untrusted total must not lower the measured drawdown; got %.1f want 10", prs.CurrentDrawdownPct)
	}

	// Untrusted substitute ABOVE the peak: the ratchet must not move the
	// high-water mark, and the floor still holds.
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

	// A trusted cycle overwrites the floor with the real measurement.
	if _, _, _, _ = checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, true); prs.CurrentDrawdownPct != 0 {
		t.Errorf("trusted cycle must overwrite the floor; got %.1f want 0", prs.CurrentDrawdownPct)
	}
	if prs.PeakValue != 10000 {
		t.Errorf("trusted equal-to-peak total moved the peak; got %.2f", prs.PeakValue)
	}
}

// TestUntrustedEquity_FloorAloneCannotLatch pins the safety property that makes
// the floor sound: every stored reading came from a cycle that did NOT latch,
// so it is at or below the limit and can never fire the kill switch by itself.
func TestUntrustedEquity_FloorAloneCannotLatch(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 24.9} // just under the 25% limit
	cfg := review1449Config()

	allowed, _, warning, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false)
	if !allowed || prs.KillSwitchActive {
		t.Fatal("a floor at 24.9% must not latch against a 25% limit")
	}
	if !warning {
		t.Error("expected the floored reading to stay in the warn band")
	}

	// A genuine loss measured on the same untrusted cycle still latches.
	allowed, _, _, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 7000, 0, 0, 0, true, false)
	if allowed {
		t.Fatal("a real 30% drawdown must latch even on an untrusted cycle")
	}
	if !strings.Contains(reason, "portfolio drawdown") || strings.Contains(reason, "margin") {
		t.Errorf("expected an equity-sourced latch reason; got %q", reason)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "equity" {
		t.Errorf("expected one triggered event with Source=equity; got %+v", prs.Events)
	}
}

// TestUntrustedEquity_LatchStaysWithEquity is the deliberate NON-change: an
// untrusted total does not hand the whole book back to the margin signal. That
// handover over a transient balance-fetch blip is the #1448 failure mode.
func TestUntrustedEquity_LatchStaysWithEquity(t *testing.T) {
	cfg := review1449Config()
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 9.8}

	// The live incident's shape — 65% margin drawdown on tiny deployed margin —
	// but on an UNTRUSTED cycle this time.
	allowed, _, warning, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 9020, 0, 31.6, 48.42, true, false)
	if !allowed || prs.KillSwitchActive {
		t.Fatalf("margin must not latch while equity can measure, trusted or not; reason=%q", reason)
	}
	if !warning {
		t.Error("an over-limit margin reading must still raise the warn band")
	}

	// Inverse: with equity genuinely UNAVAILABLE, margin is the only signal
	// left and must latch.
	prs2 := &PortfolioRiskState{PeakValue: 10000}
	allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(prs2, cfg, 0, 0, 31.6, 48.42, false, false)
	if allowed {
		t.Fatal("margin must latch when the equity guard cannot measure at all")
	}
	if len(prs2.Events) != 1 || prs2.Events[0].Source != "margin" {
		t.Errorf("expected one triggered event with Source=margin; got %+v", prs2.Events)
	}
}

// TestColdStartWithPositiveEquity_MarginCannotFire pins the corrected comment
// from the #1449 review: a first cycle carrying any positive total arms the
// equity guard through the ratchet, so margin cannot fire the latch. Only a
// total that has been non-positive on every cycle keeps the margin arm live —
// which is what TestCheckPortfolioRisk_PeakZero_MarginCanStillFire covers.
func TestColdStartWithPositiveEquity_MarginCannotFire(t *testing.T) {
	cfg := review1449Config()
	prs := &PortfolioRiskState{PeakValue: 0} // never valued before

	// Bar 1: the account holds cash and immediately blows up margin.
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 1000, 0, 500, 1000)
	if !allowed || prs.KillSwitchActive {
		t.Fatalf("bar-1 positive equity arms the guard, so margin must not latch; reason=%q", reason)
	}
	if prs.PeakValue != 1000 {
		t.Errorf("expected the first positive total to arm the peak; got %.2f", prs.PeakValue)
	}
}

// TestPortfolioWarningThrottle covers the #1449 review finding that the warn
// band can now persist indefinitely, so a fixed per-cycle DM cadence would bury
// the channel that also carries kill-switch notices.
func TestPortfolioWarningThrottle(t *testing.T) {
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	prev := portfolioWarningAlertState{}

	// (a) First cycle of the band always notifies.
	notify, st := portfolioWarningShouldNotify(prev, false, true, 0, 30, base)
	if !notify {
		t.Fatal("first cycle in the band must notify")
	}

	// Held over the limit for 24h with no change: every 600s cycle in between
	// is suppressed, and only the alert_throttle_interval floor lets one
	// through.
	sent := 0
	cur := st
	for i := 1; i <= 144; i++ {
		at := base.Add(time.Duration(i) * 600 * time.Second)
		var n bool
		n, cur = portfolioWarningShouldNotify(cur, false, true, 0, 30, at)
		if n {
			sent++
		}
	}
	if want := int((24 * time.Hour) / effectiveAlertThrottleInterval()); sent != want {
		t.Errorf("expected %d throttled reminders over 24h; got %d", want, sent)
	}

	// (b) A cleared band re-arms: the reset makes the next entry notify at once.
	portfolioWarningAlerts = cur
	portfolioWarningAlertsReset()
	if portfolioWarningAlerts.Notified {
		t.Error("reset must clear the throttle so a re-entered band notifies immediately")
	}
	if n, _ := portfolioWarningShouldNotify(portfolioWarningAlerts, false, true, 0, 30, base.Add(25*time.Hour)); !n {
		t.Error("re-entered band must notify on its first cycle")
	}

	// (c) Equity newly crossing into the band while margin is already throttled
	// must notify on that cycle, not wait for the interval.
	throttled := portfolioWarningAlertState{
		Notified: true, LastNotifiedAt: base, LastMarginDDPct: 30, MarginInBand: true,
	}
	if n, _ := portfolioWarningShouldNotify(throttled, true, true, 16, 30, base.Add(10*time.Minute)); !n {
		t.Error("a signal newly entering the band must notify immediately")
	}

	// A material worsening also overrides the interval; a trivial one does not.
	if n, _ := portfolioWarningShouldNotify(throttled, false, true, 0, 31, base.Add(10*time.Minute)); !n {
		t.Error("a 1.0pp rise must notify")
	}
	if n, _ := portfolioWarningShouldNotify(throttled, false, true, 0, 30.3, base.Add(10*time.Minute)); n {
		t.Error("a 0.3pp rise must stay throttled")
	}

	// A suppressed cycle keeps the previous baseline, so a slow creep still
	// accumulates past the escalation threshold instead of resetting each cycle.
	creep := throttled
	for i := 1; i <= 4; i++ {
		var n bool
		n, creep = portfolioWarningShouldNotify(creep, false, true, 0, 30+0.3*float64(i), base.Add(time.Duration(i)*10*time.Minute))
		if n && i < 4 {
			t.Errorf("creep notified too early at step %d", i)
		}
		if n && i == 4 {
			return // 1.2pp accumulated — notified as intended
		}
	}
	t.Error("a slow creep past the escalation threshold must eventually notify")
}

// TestPortfolioWarnBandSignals_SharedDefinition guards the single definition of
// warn-band membership used by both the risk check and the throttle.
func TestPortfolioWarnBandSignals_SharedDefinition(t *testing.T) {
	cfg := review1449Config() // warn band opens at 15%
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 16, CurrentMarginDrawdownPct: 4}

	if eq, mg := portfolioWarnBandSignals(cfg, prs, true); !eq || mg {
		t.Errorf("expected equity in band and margin out; got eq=%v mg=%v", eq, mg)
	}
	if eq, _ := portfolioWarnBandSignals(cfg, prs, false); eq {
		t.Error("equity must never be in band when the total is unavailable")
	}
	// A stale non-zero reading with no peak must not read as in-band — the
	// drawdown computation leaves CurrentDrawdownPct untouched there.
	noPeak := &PortfolioRiskState{PeakValue: 0, CurrentDrawdownPct: 16}
	if eq, _ := portfolioWarnBandSignals(cfg, noPeak, true); eq {
		t.Error("PeakValue==0 must not report an equity warn band")
	}
	if eq, mg := portfolioWarnBandSignals(nil, prs, true); eq || mg {
		t.Error("nil config must report no bands")
	}
}

// TestCircuitBreakerSuppression_QueuesOwnerDM covers the #1449 review finding
// that circuit_breaker:false leaves a strategy with no automatic protection at
// any level once the portfolio latch belongs to equity — so the crossing has to
// reach the owner, not only the strategy log.
func TestCircuitBreakerSuppression_QueuesOwnerDM(t *testing.T) {
	off := false
	id := "hl-cb-suppress-dm"
	circuitBreakerSuppressedWarned.Delete(id)
	defer circuitBreakerSuppressedWarned.Delete(id)
	drainCircuitBreakerSuppressionAlerts() // isolate from other tests

	newState := func() *StrategyState {
		return &StrategyState{
			ID: id, Type: "perps", Cash: 7700,
			RiskState: RiskState{
				PeakValue: 10000, MaxDrawdownPct: 20, ConsecutiveLosses: 5,
				DailyPnLDate: todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}
	sc := &StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "ETH", "1h", "--mode=live"}, MaxDrawdownPct: 20, CircuitBreaker: &off,
	}
	run := func() {
		var buf bytes.Buffer
		s := newState()
		CheckRisk(sc, s, PortfolioValue(s, nil), nil, &StrategyLogger{stratID: id, writer: &buf}, nil)
	}

	run()
	queued := drainCircuitBreakerSuppressionAlerts()
	if len(queued) != 1 {
		t.Fatalf("expected one queued suppression alert; got %d", len(queued))
	}
	if queued[0].StrategyID != id {
		t.Errorf("wrong strategy id: %q", queued[0].StrategyID)
	}
	dm := formatCircuitBreakerSuppressionDM(queued[0])
	for _, want := range []string{id, "circuit_breaker", "Nothing was closed", "drawdown", "consecutive losses"} {
		if !strings.Contains(dm, want) {
			t.Errorf("owner DM missing %q:\n%s", want, dm)
		}
	}

	// Same episode: throttled by the existing once-per-episode latch, so no
	// second DM.
	run()
	if extra := drainCircuitBreakerSuppressionAlerts(); len(extra) != 0 {
		t.Errorf("expected no repeat DM within one suppression episode; got %d", len(extra))
	}

	// Re-enabling clears the latch, so a later re-disable alerts afresh.
	on := true
	enabled := *sc
	enabled.CircuitBreaker = &on
	var buf bytes.Buffer
	s := newState()
	CheckRisk(&enabled, s, PortfolioValue(s, nil), nil, &StrategyLogger{stratID: id, writer: &buf}, nil)
	drainCircuitBreakerSuppressionAlerts()
	run()
	if again := drainCircuitBreakerSuppressionAlerts(); len(again) != 1 {
		t.Errorf("expected a fresh DM after re-enable then re-disable; got %d", len(again))
	}
}
