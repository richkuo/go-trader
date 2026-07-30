package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseKillSwitchResetDMTimeout_DefaultsToSixHours(t *testing.T) {
	d, err := ParseKillSwitchResetDMTimeout("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != DefaultKillSwitchResetDMTimeout {
		t.Fatalf("got %s, want %s", d, DefaultKillSwitchResetDMTimeout)
	}
	// Regression: former hard-coded AskOwnerDM wait was 30m (#1368).
	if DefaultKillSwitchResetDMTimeout == 30*time.Minute {
		t.Fatal("default must not be the former hard-coded 30m")
	}
	if DefaultKillSwitchResetDMTimeout != 6*time.Hour {
		t.Fatalf("default = %s, want 6h", DefaultKillSwitchResetDMTimeout)
	}
}

func TestParseKillSwitchResetDMTimeout_ParsesGoDuration(t *testing.T) {
	d, err := ParseKillSwitchResetDMTimeout("30m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 30*time.Minute {
		t.Fatalf("got %s, want 30m", d)
	}
}

func TestParseKillSwitchResetDMTimeout_RejectsInvalid(t *testing.T) {
	if _, err := ParseKillSwitchResetDMTimeout("nonsense"); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestParseKillSwitchResetDMTimeout_RejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0", "-1h"} {
		if _, err := ParseKillSwitchResetDMTimeout(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestLoadConfig_AppliesKillSwitchResetDMTimeout(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"kill_switch_reset_dm_timeout": "45m",
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("LoadConfigForProbe: %v", err)
	}
	if cfg.KillSwitchResetDMTimeout != "45m" {
		t.Fatalf("KillSwitchResetDMTimeout = %q, want 45m", cfg.KillSwitchResetDMTimeout)
	}
	if err := applyKillSwitchResetDMTimeoutFromConfig(cfg); err != nil {
		t.Fatalf("applyKillSwitchResetDMTimeoutFromConfig: %v", err)
	}
	if effectiveKillSwitchResetDMTimeout() != 45*time.Minute {
		t.Fatalf("runtime timeout = %s, want 45m", effectiveKillSwitchResetDMTimeout())
	}
}

func TestLoadConfig_KillSwitchResetDMTimeoutDefaultsToSixHours(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("LoadConfigForProbe: %v", err)
	}
	if err := applyKillSwitchResetDMTimeoutFromConfig(&Config{}); err != nil {
		t.Fatalf("applyKillSwitchResetDMTimeoutFromConfig: %v", err)
	}
	if effectiveKillSwitchResetDMTimeout() != DefaultKillSwitchResetDMTimeout {
		t.Fatalf("runtime timeout = %s, want %s", effectiveKillSwitchResetDMTimeout(), DefaultKillSwitchResetDMTimeout)
	}
}

func TestLoadConfig_RejectsInvalidKillSwitchResetDMTimeout(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"kill_switch_reset_dm_timeout": "nonsense",
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfigForProbe(path); err == nil {
		t.Fatal("expected LoadConfigForProbe to reject invalid kill_switch_reset_dm_timeout")
	}
}

func TestApplyHotReloadConfig_UpdatesKillSwitchResetDMTimeout(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })
	applyKillSwitchResetDMTimeout(time.Hour)

	cfg := minimalReloadConfig(nil)
	next := minimalReloadConfig(nil)
	next.KillSwitchResetDMTimeout = "2h"

	changes, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "kill_switch_reset_dm_timeout") {
		t.Fatalf("expected kill_switch_reset_dm_timeout change, got %v", changes)
	}
	if effectiveKillSwitchResetDMTimeout() != 2*time.Hour {
		t.Fatalf("runtime timeout = %s, want 2h", effectiveKillSwitchResetDMTimeout())
	}
}

func TestLoadConfigForProbe_DoesNotMutateAdoptedKillSwitchResetDMTimeout(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })
	applyKillSwitchResetDMTimeout(90 * time.Minute)

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"kill_switch_reset_dm_timeout": "15m",
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("LoadConfigForProbe: %v", err)
	}
	if effectiveKillSwitchResetDMTimeout() != 90*time.Minute {
		t.Fatalf("probe load mutated runtime timeout to %s, want 90m", effectiveKillSwitchResetDMTimeout())
	}
}

// TestKillSwitchResetAskTimeoutIndependentOfAlertThrottle locks the #1368
// invariant: the KS reset AskOwnerDM wait must not be sourced from
// alert_throttle_interval.
func TestKillSwitchResetAskTimeoutIndependentOfAlertThrottle(t *testing.T) {
	prevKS := killSwitchResetDMTimeout
	prevThrottle := alertThrottleInterval
	t.Cleanup(func() {
		killSwitchResetDMTimeout = prevKS
		alertThrottleInterval = prevThrottle
	})

	applyAlertThrottleInterval(30 * time.Minute)
	applyKillSwitchResetDMTimeout(6 * time.Hour)

	if effectiveKillSwitchResetDMTimeout() != 6*time.Hour {
		t.Fatalf("KS timeout = %s, want 6h", effectiveKillSwitchResetDMTimeout())
	}
	if effectiveAlertThrottleInterval() != 30*time.Minute {
		t.Fatalf("alert throttle = %s, want 30m", effectiveAlertThrottleInterval())
	}
	if effectiveKillSwitchResetDMTimeout() == effectiveAlertThrottleInterval() {
		t.Fatal("KS reset AskDM wait must stay independent of alert_throttle_interval")
	}
}

func TestKillSwitchResetDMTimeout_ConcurrentProbeDoesNotRace(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })
	applyKillSwitchResetDMTimeout(time.Hour)

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"kill_switch_reset_dm_timeout": "30m",
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = effectiveKillSwitchResetDMTimeout()
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := LoadConfigForProbe(path); err != nil {
			t.Fatalf("LoadConfigForProbe: %v", err)
		}
	}
	<-done
	if effectiveKillSwitchResetDMTimeout() != time.Hour {
		t.Fatalf("runtime timeout = %s, want 1h", effectiveKillSwitchResetDMTimeout())
	}
}
