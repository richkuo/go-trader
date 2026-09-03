package main

import "time"

const portfolioWarningEscalationPct = 1.0

type portfolioWarningAlertState struct {
	Notified        bool
	LastNotifiedAt  time.Time
	LastEquityDDPct float64
	LastMarginDDPct float64
	EquityInBand    bool
	MarginInBand    bool
}

var portfolioWarningAlerts = map[PortfolioScope]portfolioWarningAlertState{}

func portfolioWarningShouldNotify(prev portfolioWarningAlertState, equityInBand, marginInBand bool, equityDD, marginDD float64, now time.Time) (bool, portfolioWarningAlertState) {
	notify := false
	switch {
	case !prev.Notified:
		notify = true
	case equityInBand && !prev.EquityInBand:
		notify = true
	case marginInBand && !prev.MarginInBand:
		notify = true
	case equityDD-prev.LastEquityDDPct >= portfolioWarningEscalationPct:
		notify = true
	case marginDD-prev.LastMarginDDPct >= portfolioWarningEscalationPct:
		notify = true
	case now.Sub(prev.LastNotifiedAt) >= effectiveAlertThrottleInterval():
		notify = true
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

func portfolioWarningAlertsReset(scope PortfolioScope) {
	delete(portfolioWarningAlerts, scope)
}
