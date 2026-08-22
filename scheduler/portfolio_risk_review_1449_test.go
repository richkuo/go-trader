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
// the floor sound: a carried reading can raise the reported drawdown but can
// never fire the kill switch by itself, because the floor is clamped to
// MaxDrawdownPct while the latch test is a strict >.
//
// The clamp is what makes that true. An earlier revision claimed instead that
// every stored reading came from a non-latching cycle and was therefore already
// below the limit; TestUntrustedEquity_StoredOverLimitFloorCannotLatch below is
// the counterexample that disproved it.
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

	// A genuine 30% loss measured on the same untrusted cycle is not swallowed
	// by the clamp — but #1449 review round 3 DEFERS the full-book latch there
	// rather than firing it, because an untrusted total that reads too low
	// would otherwise flatten a healthy book. The measurement survives intact
	// and the deferral window opens; the escalation and the trusted-cycle
	// immediacy are pinned in the round-3 tests.
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

	// The same measurement on a TRUSTED cycle latches immediately, on equity.
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

// TestUntrustedEquity_StoredOverLimitFloorCannotLatch is the #1449 review
// counterexample to the floor's original soundness argument.
//
// CurrentDrawdownPct is written BEFORE the latch check, so the latching cycle
// persists an OVER-LIMIT reading, and SQLite keeps it. Every reset path now
// clears it, but the reading also survives a plain process restart while the
// latch is already clear. Unclamped, the first untrusted cycle after that would
// floor a healthy measurement back up to the stale over-limit value and flatten
// the entire book off a number nothing measured this cycle.
func TestUntrustedEquity_StoredOverLimitFloorCannotLatch(t *testing.T) {
	cfg := review1449Config() // 25% limit

	// (a) Stored reading exactly AT the limit: the strict > must not fire.
	atLimit := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 25}
	allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(atLimit, cfg, 10000, 0, 0, 0, true, false)
	if !allowed || atLimit.KillSwitchActive {
		t.Fatal("a stored reading equal to the limit must not latch on its own")
	}

	// (c) Restart shape: a reading ABOVE the limit reloaded from SQLite while
	// KillSwitchActive is already false, first cycle untrusted.
	overLimit := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 40}
	allowed, _, _, reason := checkPortfolioRiskWithEquityAvailability(overLimit, cfg, 10000, 0, 0, 0, true, false)
	if !allowed || overLimit.KillSwitchActive {
		t.Fatalf("a stale over-limit reading must never re-latch by itself; reason=%q", reason)
	}
	if overLimit.CurrentDrawdownPct != 25 {
		t.Errorf("floor must be clamped to the limit; got %.1f want 25", overLimit.CurrentDrawdownPct)
	}
	if !overLimit.DrawdownReadingSubstituted {
		t.Error("a floored reading must be marked as substituted")
	}

	// (b) The clamp must not become a fail-open: a genuine this-cycle
	// measurement above the limit still latches, and is not clamped.
	//
	// #1449 review round 3 moved WHICH cycle this is asserted on. It used to
	// be asserted on an untrusted cycle, because the only question then was
	// whether the clamp could swallow a real measurement. The round-3 finding
	// added a second question the clamp says nothing about — whether an
	// untrusted total that reads too LOW should be allowed to flatten the book
	// at all — and the answer is that it is deferred, not suppressed. So the
	// clamp's non-fail-open property is pinned here on a trusted cycle, where
	// it is the only mechanism in play, and the untrusted case with its
	// deferral and escalation is pinned in
	// TestUntrustedEquity_OverLimitLatchIsDeferredNotVetoed.
	real := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 40}
	allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(real, cfg, 6000, 0, 0, 0, true, true)
	if allowed || !real.KillSwitchActive {
		t.Fatal("a real 40% drawdown must latch even with the floor clamped")
	}
	if real.CurrentDrawdownPct != 40 {
		t.Errorf("a this-cycle measurement above the limit must not be clamped; got %.1f want 40", real.CurrentDrawdownPct)
	}
	if !strings.Contains(reason, "portfolio drawdown") {
		t.Errorf("expected an equity-sourced latch reason; got %q", reason)
	}
	// The latch fired on this cycle's own 40% measurement, not on the floor, so
	// the reading is a direct measurement and must not be marked substituted.
	if real.DrawdownReadingSubstituted {
		t.Error("a latch driven by a this-cycle measurement must not be marked substituted")
	}
}

// TestManualKillSwitchReset_ClearsStaleReadings covers the other half of the
// #1449 review finding: the owner-DM reset cleared only the latch flags, so the
// over-limit reading the latching cycle persisted survived the reset. Both
// auto-reset paths already cleared it.
func TestManualKillSwitchReset_ClearsStaleReadings(t *testing.T) {
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
	// A manual reset makes no claim that the book is flat or the total
	// verified, so unlike the auto-reset paths it must NOT move the peak.
	if prs.PeakValue != 10000 {
		t.Errorf("manual reset must retain the real high-water mark; got %.2f", prs.PeakValue)
	}
	// The cleared state cannot re-latch on the next untrusted cycle.
	cfg := review1449Config()
	if allowed, _, _, _ := checkPortfolioRiskWithEquityAvailability(prs, cfg, 10000, 0, 0, 0, true, false); !allowed {
		t.Fatal("a freshly reset portfolio must not re-latch on the next untrusted cycle")
	}
	if ResetPortfolioKillSwitchManual(nil) != 0 {
		t.Error("nil state must be a no-op")
	}
}

// TestDrawdownReadingSubstitutedMarker pins the #1449 review finding that a
// floored reading is persisted into CurrentDrawdownPct and so stops matching
// the persisted peak and total. It stays persisted — that keeps the stored
// number equal to the one the latch and warn band decided on — but it must
// always be labeled.
func TestDrawdownReadingSubstitutedMarker(t *testing.T) {
	cfg := review1449Config()

	// (a) Untrusted substitute ABOVE the peak: raw is 0%, the floor carries
	// 10%. The stored reading must be marked so no surface implies a 10% loss
	// at peak value.
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 10}
	checkPortfolioRiskWithEquityAvailability(prs, cfg, 12000, 0, 0, 0, true, false)
	if prs.CurrentDrawdownPct != 10 || !prs.DrawdownReadingSubstituted {
		t.Errorf("expected a marked 10%% floored reading; got %.1f marked=%v",
			prs.CurrentDrawdownPct, prs.DrawdownReadingSubstituted)
	}

	// (c) The inverse: a trusted cycle writes the exact measurement and clears
	// the marker, so the arithmetic invariant holds again.
	checkPortfolioRiskWithEquityAvailability(prs, cfg, 9500, 0, 0, 0, true, true)
	if prs.DrawdownReadingSubstituted {
		t.Error("a trusted cycle must clear the substituted marker")
	}
	if want := (10000 - 9500.0) / 10000 * 100; prs.CurrentDrawdownPct != want {
		t.Errorf("trusted reading must reconstruct from peak and total; got %.2f want %.2f",
			prs.CurrentDrawdownPct, want)
	}

	// An untrusted cycle whose own measurement EXCEEDS the floor is a real
	// measurement, so it must not be marked either.
	checkPortfolioRiskWithEquityAvailability(prs, cfg, 9000, 0, 0, 0, true, false)
	if prs.DrawdownReadingSubstituted {
		t.Error("an untrusted cycle that measured above the floor is not substituted")
	}
	if prs.CurrentDrawdownPct != 10 {
		t.Errorf("expected the measured 10%%; got %.2f", prs.CurrentDrawdownPct)
	}
}

// TestPortfolioWarningLabels_FollowTheArmedGuard covers the #1449 review
// finding that the warning message always attached "distance to kill switch" to
// equity, even on the path where the margin arm is the one that flattens the
// book. The two arms are mutually exclusive, so the labels must follow whichever
// one can latch this cycle.
func TestPortfolioWarningLabels_FollowTheArmedGuard(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60}
	base := func(prs PortfolioRiskState) *AppState {
		return &AppState{PortfolioRisk: prs, Strategies: map[string]*StrategyState{}}
	}

	// (a) Pooled wallet with no trustworthy balance: margin in band, equity
	// guard unarmed. Margin owns the latch, so it owns the label — and the
	// stale equity reading must not be presented as current.
	unarmed := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:           cfg,
		State:            base(PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 18, CurrentMarginDrawdownPct: 20}),
		PerpsLoss:        400,
		PerpsMargin:      2000,
		EquityGuardArmed: false,
	})
	if !strings.Contains(unarmed, "Distance to kill switch: 5.0% perps margin") {
		t.Errorf("unarmed guard must label margin as the distance to the kill switch:\n%s", unarmed)
	}
	if strings.Contains(unarmed, "from limit") {
		t.Errorf("margin must not be demoted to distance-from-limit when it owns the latch:\n%s", unarmed)
	}
	if strings.Contains(unarmed, "equity=18.0%") {
		t.Errorf("a stale equity reading must not be shown as current when the guard is unarmed:\n%s", unarmed)
	}

	// (b) PeakValue == 0 cold start with a leftover non-zero reading: same
	// unarmed treatment, and the leftover number must not surface.
	coldStart := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:           cfg,
		State:            base(PortfolioRiskState{PeakValue: 0, CurrentDrawdownPct: 22, CurrentMarginDrawdownPct: 19}),
		PerpsLoss:        380,
		PerpsMargin:      2000,
		EquityGuardArmed: false,
	})
	if !strings.Contains(coldStart, "Distance to kill switch: 6.0% perps margin") {
		t.Errorf("cold start must point the kill-switch label at margin:\n%s", coldStart)
	}
	if strings.Contains(coldStart, "22.0%") {
		t.Errorf("leftover cold-start equity reading leaked into the message:\n%s", coldStart)
	}
	if !strings.Contains(coldStart, "equity dd n/a") {
		t.Errorf("trend line must not report a delta for a signal never measured:\n%s", coldStart)
	}

	// (c) The inverse — armed guard keeps the #1448 labeling exactly as it was.
	armed := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:           cfg,
		State:            base(PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 18, CurrentMarginDrawdownPct: 20}),
		TotalValue:       8200,
		PerpsLoss:        400,
		PerpsMargin:      2000,
		EquityGuardArmed: true,
	})
	if !strings.Contains(armed, "Distance to kill switch: 7.0% equity | perps margin 5.0% from limit") {
		t.Errorf("armed guard must keep equity on the kill-switch label:\n%s", armed)
	}

	// A substituted reading is labeled on the armed path so the percentage and
	// the dollar figures beside it are not read as corrupt data.
	substituted := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config: cfg,
		State: base(PortfolioRiskState{
			PeakValue: 10000, CurrentDrawdownPct: 18, CurrentMarginDrawdownPct: 20,
			DrawdownReadingSubstituted: true,
		}),
		TotalValue:       10200,
		PerpsLoss:        400,
		PerpsMargin:      2000,
		EquityGuardArmed: true,
	})
	if !strings.Contains(substituted, "balance substituted this cycle") {
		t.Errorf("a floored reading must be labeled on the operator surface:\n%s", substituted)
	}
}
