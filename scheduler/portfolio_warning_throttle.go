package main

import "time"

// #1449 review — throttle for the portfolio drawdown warning.
//
// Before #1448 a margin drawdown above max_drawdown_pct ended the warn band
// within one cycle by latching the kill switch. Since #1448 the latch belongs
// to equity while equity can measure, so that same reading is a REACHABLE warn
// state that can persist indefinitely. The warning is a multi-section block
// sent to every channel plus an owner DM, and it also runs a RecentTrades
// query, so an unthrottled per-cycle cadence means roughly 144 identical
// messages per channel per day at the default 600s interval — alert fatigue on
// the same channel that carries kill-switch and CRITICAL notices.
//
// The throttle deliberately keeps four escape hatches so a real escalation is
// never delayed:
//
//   - the first cycle of a band always notifies;
//   - a signal that was NOT in band at the last notification and is now in
//     band always notifies, so equity crossing in while margin is already
//     throttled is immediate;
//   - a rise of portfolioWarningEscalationPct or more on either lens since the
//     last notification always notifies;
//   - otherwise the alert_throttle_interval floor applies (default 6h), the
//     same knob missingMarkAlerts and script_failure_alerts use.
//
// Suppressed cycles return the PREVIOUS state unchanged, so the escalation
// baseline stays anchored to the last reading actually sent: a slow creep of
// 0.2pp per cycle still notifies once it accumulates past the threshold, and a
// signal flapping in and out of band between notifications does not re-arm the
// newly-in-band hatch.
//
// The band-cleared reset is the caller's job (portfolioWarningAlertsReset),
// which is what makes an exited-and-re-entered band notify immediately.

// portfolioWarningEscalationPct is the rise on either drawdown lens, in
// percentage points since the last DM, that overrides the interval floor.
const portfolioWarningEscalationPct = 1.0

type portfolioWarningAlertState struct {
	Notified        bool
	LastNotifiedAt  time.Time
	LastEquityDDPct float64
	LastMarginDDPct float64
	EquityInBand    bool
	MarginInBand    bool
}

// portfolioWarningAlerts is the live throttle state. Written only from the
// main trading loop (single goroutine), matching exposureCapAlerts.
var portfolioWarningAlerts portfolioWarningAlertState

// portfolioWarningShouldNotify decides whether this in-band cycle sends the
// warning, and returns the state to carry forward. Pure — the caller owns the
// state variable and does the sending.
func portfolioWarningShouldNotify(prev portfolioWarningAlertState, equityInBand, marginInBand bool, equityDD, marginDD float64, now time.Time) (bool, portfolioWarningAlertState) {
	notify := false
	switch {
	case !prev.Notified:
		notify = true // first cycle in this band
	case equityInBand && !prev.EquityInBand:
		notify = true // equity newly joined the band
	case marginInBand && !prev.MarginInBand:
		notify = true // margin newly joined the band
	case equityDD-prev.LastEquityDDPct >= portfolioWarningEscalationPct:
		notify = true // material worsening on equity
	case marginDD-prev.LastMarginDDPct >= portfolioWarningEscalationPct:
		notify = true // material worsening on margin
	case now.Sub(prev.LastNotifiedAt) >= effectiveAlertThrottleInterval():
		notify = true // periodic reminder while the band persists
	}
	if !notify {
		return false, prev
	}
	return true, portfolioWarningAlertState{
		Notified:        true,
		LastNotifiedAt:  now,
		LastEquityDDPct: equityDD,
		LastMarginDDPct: marginDD,
		EquityInBand:    equityInBand,
		MarginInBand:    marginInBand,
	}
}

// portfolioWarningAlertsReset clears the throttle when the warn band ends
// (recovered, or the kill switch latched and took the band with it), so the
// next entry into the band notifies on its first cycle.
func portfolioWarningAlertsReset() {
	portfolioWarningAlerts = portfolioWarningAlertState{}
}
