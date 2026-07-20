package main

// #1392 — circuit-breaker cooldown-expiry invariants.
//
// Spun out of #1345 (closed as not-a-bug): after CircuitBreakerUntil expires,
// the same CheckRisk call clears the latch then re-evaluates both firing arms
// before any allowed return. These tests pin that "clear then immediately
// re-check" contract so a future early-return between the clear and the
// drawdown arm cannot silently re-enable trading while drawdown is still
// breached.

import (
	"strings"
	"testing"
	"time"
)

// expiredCBLatch returns a RiskState whose circuit breaker is latched but whose
// cooldown window already ended — the setup every expiry-cycle case needs.
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

// #1392: cooldown expired + drawdown still over MaxDrawdownPct → same-call
// re-latch with the max-drawdown reason (not the latched "circuit breaker
// active" reason), CircuitBreaker true again, CircuitBreakerUntil re-armed.
func TestCheckRisk_CooldownExpired_DrawdownStillBreached_Relatches(t *testing.T) {
	s := &StrategyState{
		ID:              "cb-expiry-dd-breach",
		Type:            "spot",
		Cash:            7000,
		RiskState:       expiredCBLatch(10000, 20, 0),
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{ID: s.ID, Type: "spot", MaxDrawdownPct: 20}

	// Peak $10k, portfolio $7k → 30% > 20% — must re-fire on this call.
	before := time.Now().UTC()
	allowed, reason := CheckRisk(sc, s, 7000, nil, nil, nil)
	if allowed {
		t.Fatal("expired CB + still-breached drawdown must block on the same CheckRisk call")
	}
	if !strings.HasPrefix(reason, RiskReasonMaxDrawdownExceeded) {
		t.Fatalf("reason = %q, want %q prefix (fresh re-fire, not latched %q)",
			reason, RiskReasonMaxDrawdownExceeded, RiskReasonCircuitBreakerActive)
	}
	if reason == RiskReasonCircuitBreakerActive {
		t.Fatal("must not return the latched reason after cooldown expiry — drawdown arm must re-evaluate")
	}
	assertCBLatchDuration(t, s, before, 24*time.Hour)
}

// #1392: cooldown expired + drawdown recovered → trading allowed and breaker
// fully cleared (no sticky latch).
func TestCheckRisk_CooldownExpired_DrawdownRecovered_Allows(t *testing.T) {
	s := &StrategyState{
		ID:              "cb-expiry-dd-ok",
		Type:            "spot",
		Cash:            9500,
		RiskState:       expiredCBLatch(10000, 20, 0),
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{ID: s.ID, Type: "spot", MaxDrawdownPct: 20}

	// Peak $10k, portfolio $9.5k → 5% < 20% — clear must stick.
	allowed, reason := CheckRisk(sc, s, 9500, nil, nil, nil)
	if !allowed {
		t.Fatalf("expired CB + recovered drawdown must allow trading, got reason=%q", reason)
	}
	if s.RiskState.CircuitBreaker {
		t.Fatal("CircuitBreaker must be cleared after a healthy expiry-cycle check")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("ConsecutiveLosses = %d, want 0 after expiry clear", s.RiskState.ConsecutiveLosses)
	}
}

// #1392: cooldown expired after a loss-streak fire → ConsecutiveLosses resets
// to 0 and trading is allowed. The streak arm is intentionally time-only: the
// clear at risk.go:1431 zeroes the counter so the streak cannot re-latch
// forever (the only other reset is a winning trade).
func TestCheckRisk_CooldownExpired_LossStreak_ResetsAndAllows(t *testing.T) {
	s := &StrategyState{
		ID:              "cb-expiry-streak",
		Type:            "spot",
		Cash:            10000,
		RiskState:       expiredCBLatch(10000, 50, 5), // streak that originally fired
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{ID: s.ID, Type: "spot", MaxDrawdownPct: 50}

	allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
	if !allowed {
		t.Fatalf("expired loss-streak CB must allow after ConsecutiveLosses reset, got reason=%q", reason)
	}
	if s.RiskState.CircuitBreaker {
		t.Fatal("CircuitBreaker must be cleared")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("ConsecutiveLosses = %d, want 0 — expiry clear is the intentional time-based streak reset",
			s.RiskState.ConsecutiveLosses)
	}
}

// #1392: perps with no open positions after a CB force-close fall back to
// peak-relative drawdown on the expiry cycle (#292). When peak-relative is
// under the threshold, the clear sticks — the closed positions *are* the
// recovery; margin-based drawdown no longer applies with margin=0.
func TestCheckRisk_CooldownExpired_PerpsFlat_PeakRelativeUnderThreshold_Allows(t *testing.T) {
	s := &StrategyState{
		ID:              "cb-expiry-perps-flat",
		Type:            "perps",
		Cash:            900, // realized losses from the force-close
		RiskState:       expiredCBLatch(1000, 25, 0),
		Positions:       map[string]*Position{}, // flat — force-close already drained
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{ID: s.ID, Type: "perps", Leverage: 20, MaxDrawdownPct: 25}

	// Peak $1000, cash-only portfolio $900 → peak-relative 10% < 25%.
	// If the expiry path incorrectly kept a margin-based numerator with
	// denom=0, drawdown semantics would be wrong; peak-relative must win.
	pv := PortfolioValue(s, nil)
	allowed, reason := CheckRisk(sc, s, pv, nil, nil, nil)
	if !allowed {
		t.Fatalf("flat perps + peak-relative under threshold must allow after CB expiry, got reason=%q", reason)
	}
	if s.RiskState.CircuitBreaker {
		t.Fatal("CircuitBreaker must be cleared when peak-relative drawdown is under the limit")
	}
	if s.RiskState.CurrentDrawdownPct < 9 || s.RiskState.CurrentDrawdownPct > 11 {
		t.Fatalf("CurrentDrawdownPct = %.2f, want ≈10%% peak-relative (not margin-based)",
			s.RiskState.CurrentDrawdownPct)
	}
}

// Companion to the flat-under-threshold case: after positions are closed, a
// still-breached peak-relative drawdown must re-latch on the expiry cycle
// (same clear-then-recheck invariant, peak-relative denominator).
func TestCheckRisk_CooldownExpired_PerpsFlat_PeakRelativeStillBreached_Relatches(t *testing.T) {
	s := &StrategyState{
		ID:              "cb-expiry-perps-flat-breach",
		Type:            "perps",
		Cash:            700,
		RiskState:       expiredCBLatch(1000, 25, 0),
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{ID: s.ID, Type: "perps", Leverage: 20, MaxDrawdownPct: 25}

	// Peak $1000, cash $700 → 30% > 25% peak-relative → re-fire.
	before := time.Now().UTC()
	allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
	if allowed {
		t.Fatal("flat perps + peak-relative still over limit must re-latch on expiry cycle")
	}
	if !strings.HasPrefix(reason, RiskReasonMaxDrawdownExceeded) {
		t.Fatalf("reason = %q, want %q prefix", reason, RiskReasonMaxDrawdownExceeded)
	}
	if !strings.Contains(reason, "denom=peak=") {
		t.Fatalf("reason %q should name peak denominator (no margin deployed)", reason)
	}
	assertCBLatchDuration(t, s, before, 24*time.Hour)
}
