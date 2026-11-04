package main

import (
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
