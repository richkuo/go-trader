package main

import (
	"testing"
	"time"
)

func assertCBLatchDuration(t *testing.T, s *StrategyState, before time.Time, want time.Duration) {
	t.Helper()
	if !s.RiskState.CircuitBreaker {
		t.Fatal("expected the circuit breaker latched")
	}
	got := s.RiskState.CircuitBreakerUntil.Sub(before)
	if got < want-time.Second || got > want+30*time.Second {
		t.Fatalf("latch duration = %v, want ~%v", got, want)
	}
}
