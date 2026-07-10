package main

// #1273 — per-strategy circuit-breaker timing/threshold overrides. These tests
// pin the acceptance criteria: omitted fields reproduce the historical
// hardcoded behavior bit-for-bit, overrides move the firing threshold and both
// latch durations, the #1048 suppression warning resolves the same threshold
// as the firing arm, and reason-string classification survives non-default
// thresholds.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// #1273: omitting all three cb_* override fields reproduces the historical
// hardcoded behavior exactly — the loss-streak arm fires at 5 losses and
// latches 1h, the drawdown arm latches 24h.
func TestCheckRisk_CBTimingDefaultsPreserved(t *testing.T) {
	newState := func(losses int) *StrategyState {
		return &StrategyState{
			ID:   "cb-timing-defaults",
			Cash: 10000,
			RiskState: RiskState{
				PeakValue:         10000,
				MaxDrawdownPct:    50,
				ConsecutiveLosses: losses,
				DailyPnLDate:      todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}
	sc := &StrategyConfig{ID: "cb-timing-defaults", Type: "spot"}

	// 4 losses: below the default threshold — no fire.
	s := newState(4)
	if allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil); !allowed {
		t.Fatalf("4 losses should not fire the default 5-loss breaker (reason=%q)", reason)
	}

	// 5 losses: fires with the RiskReasonConsecutiveLosses prefix and a 1h latch.
	s = newState(5)
	before := time.Now().UTC()
	allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
	if allowed {
		t.Fatal("5 losses should fire the default breaker")
	}
	if !strings.HasPrefix(reason, RiskReasonConsecutiveLosses) {
		t.Fatalf("reason %q should start with prefix %q", reason, RiskReasonConsecutiveLosses)
	}
	assertCBLatchDuration(t, s, before, time.Hour)

	// Drawdown arm: 60% > 50% fires and latches 24h.
	s = newState(0)
	before = time.Now().UTC()
	if allowed, _ := CheckRisk(sc, s, 4000, nil, nil, nil); allowed {
		t.Fatal("60% drawdown should fire the default breaker")
	}
	assertCBLatchDuration(t, s, before, 24*time.Hour)
}

// #1273: per-strategy overrides move the loss-streak threshold and both latch
// durations, and the fired reason names the actual count and threshold while
// keeping the RiskReasonConsecutiveLosses prefix for classification.
func TestCheckRisk_CBOverridesHonored(t *testing.T) {
	intp := func(v int) *int { return &v }
	newState := func(losses int) *StrategyState {
		return &StrategyState{
			ID:   "cb-overrides",
			Cash: 10000,
			RiskState: RiskState{
				PeakValue:         10000,
				MaxDrawdownPct:    50,
				ConsecutiveLosses: losses,
				DailyPnLDate:      todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}

	// Lower threshold + shorter loss cooldown: fires at 3 with a 30m latch.
	sc := &StrategyConfig{
		ID: "cb-overrides", Type: "spot",
		CBLossStreakThreshold:       intp(3),
		CBLossStreakCooldownMinutes: intp(30),
	}
	s := newState(2)
	if allowed, _ := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil); !allowed {
		t.Fatal("2 losses should not fire a threshold-3 breaker")
	}
	s = newState(3)
	before := time.Now().UTC()
	allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
	if allowed {
		t.Fatal("3 losses should fire a threshold-3 breaker")
	}
	if !strings.HasPrefix(reason, RiskReasonConsecutiveLosses) || !strings.Contains(reason, "3 in a row, threshold 3") {
		t.Fatalf("reason %q should carry the prefix plus the actual count/threshold", reason)
	}
	assertCBLatchDuration(t, s, before, 30*time.Minute)

	// Higher threshold: the historical default streak of 5 does NOT fire.
	sc = &StrategyConfig{ID: "cb-overrides", Type: "spot", CBLossStreakThreshold: intp(8)}
	s = newState(5)
	if allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil); !allowed {
		t.Fatalf("5 losses should not fire a threshold-8 breaker (reason=%q)", reason)
	}

	// Drawdown cooldown override: latches 12h instead of 24h.
	sc = &StrategyConfig{ID: "cb-overrides", Type: "spot", CBDrawdownCooldownMinutes: intp(720)}
	s = newState(0)
	before = time.Now().UTC()
	if allowed, _ := CheckRisk(sc, s, 4000, nil, nil, nil); allowed {
		t.Fatal("60% drawdown should fire")
	}
	assertCBLatchDuration(t, s, before, 12*time.Hour)
}

// assertCBLatchDuration checks that the latch set by CheckRisk expires ~want
// after the call started (CheckRisk stamps its own time.Now, so allow slop for
// scheduling between `before` and the stamp).
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

// #1273: the #1048 suppression warning resolves the loss-streak threshold
// through the same accessor as the firing arm — a tuned threshold moves both
// in lockstep so the warning can neither under- nor over-report.
func TestRecordCircuitBreakerSuppression_ThresholdMatchesFiringArm(t *testing.T) {
	off := false
	intp := func(v int) *int { return &v }
	run := func(id string, losses int, threshold *int) (bool, string) {
		circuitBreakerSuppressedWarned.Delete(id)
		var buf bytes.Buffer
		logger := &StrategyLogger{stratID: id, writer: &buf}
		s := &StrategyState{
			ID: id, Type: "spot", Cash: 10000,
			RiskState: RiskState{
				PeakValue:         10000,
				MaxDrawdownPct:    50,
				ConsecutiveLosses: losses,
				DailyPnLDate:      todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
		sc := &StrategyConfig{
			ID: id, Type: "spot", MaxDrawdownPct: 50,
			CircuitBreaker: &off, CBLossStreakThreshold: threshold,
		}
		allowed, _ := CheckRisk(sc, s, PortfolioValue(s, nil), nil, logger, nil)
		return allowed, buf.String()
	}

	// Disabled CB + threshold 3 + exactly 3 losses: the arm WOULD fire, so the
	// suppression warning must trip at the same streak length.
	allowed, out := run("cb-suppress-th3", 3, intp(3))
	if !allowed {
		t.Fatal("disabled CB must never halt")
	}
	if !strings.Contains(out, "DISABLED") || !strings.Contains(out, "3 consecutive losses") {
		t.Fatalf("threshold-3 suppression warning missing from: %s", out)
	}

	// Disabled CB + threshold 8 + 5 losses (the historical default): the arm
	// would NOT fire, so no warning either.
	allowed, out = run("cb-suppress-th8", 5, intp(8))
	if !allowed {
		t.Fatal("disabled CB must never halt")
	}
	if strings.Contains(out, "DISABLED") {
		t.Fatalf("no suppression warning expected below a threshold-8 streak, got: %s", out)
	}
}

// #1273: loss-streak reason strings carry the fired count/threshold after the
// RiskReasonConsecutiveLosses prefix; every classifier matches on the prefix,
// so routing survives non-default thresholds.
func TestLossStreakReasonClassification_NonDefaultThreshold(t *testing.T) {
	reason := fmt.Sprintf("%s (3 in a row, threshold 3)", RiskReasonConsecutiveLosses)
	if !isFreshPerStrategyCircuitBreaker(reason) {
		t.Fatalf("fresh loss-streak reason %q must classify as a fresh fire", reason)
	}
	if isFreshPerStrategyCircuitBreaker(RiskReasonCircuitBreakerActive) {
		t.Fatal("latched reason must not classify as fresh")
	}
	if got := circuitBreakerTriggerLine(reason); got != reason {
		t.Fatalf("trigger line = %q, want the reason passed through verbatim %q", got, reason)
	}
	if rec := circuitBreakerRecommendation(reason); !strings.Contains(rec, "cooldown") {
		t.Fatalf("loss-streak recommendation lost: %q", rec)
	}
}

// #1273: the alert block's "Consecutive loss run" counter reads the strategy's
// configured threshold, not a hardcoded /5.
func TestFormatPerStrategyCircuitBreakerBlock_LossRunUsesConfiguredThreshold(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	th := 3
	sc := StrategyConfig{
		ID: "hl-btc-sma-30", Type: "perps", Platform: "hyperliquid",
		Args:                  []string{"sma_cross", "BTC", "30m", "--mode=live"},
		CBLossStreakThreshold: &th,
	}
	state := &StrategyState{
		ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 4200,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		RiskState: RiskState{
			PeakValue:           4500,
			ConsecutiveLosses:   3,
			CircuitBreaker:      true,
			CircuitBreakerUntil: now.Add(30 * time.Minute),
		},
	}
	snap := snapshotPerStrategyCircuitBreaker(state, map[string]float64{"BTC": 67800})
	snap.Now = now
	msg := formatPerStrategyCircuitBreakerBlock(perStrategyCircuitBreakerFormatInput{
		Strategy: sc,
		Snapshot: snap,
		Reason:   fmt.Sprintf("%s (3 in a row, threshold 3)", RiskReasonConsecutiveLosses),
		RecentTrades: []Trade{
			{Timestamp: now.Add(-5 * time.Minute), StrategyID: sc.ID, Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 67800, IsClose: true, RealizedPnL: -20},
		},
	})
	if !strings.Contains(msg, "Consecutive loss run: 3/3") {
		t.Fatalf("expected the configured threshold in the loss-run line, got:\n%s", msg)
	}
}
