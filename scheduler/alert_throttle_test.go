package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseAlertThrottleInterval(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty defaults to six hours", raw: "", want: DefaultAlertThrottleInterval},
		{name: "go duration", raw: "30m", want: 30 * time.Minute},
		{name: "invalid rejected", raw: "nonsense", wantErr: true},
		{name: "zero rejected", raw: "0", wantErr: true},
		{name: "negative rejected", raw: "-1h", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseAlertThrottleInterval(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d != tc.want {
				t.Fatalf("got %s, want %s", d, tc.want)
			}
		})
	}
}

func withAlertThrottleInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := alertThrottleInterval
	alertThrottleInterval = d
	t.Cleanup(func() { alertThrottleInterval = prev })
}

func TestLoadConfig_AppliesAlertThrottleInterval(t *testing.T) {
	prev := alertThrottleInterval
	t.Cleanup(func() { alertThrottleInterval = prev })

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"alert_throttle_interval": "45m",
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
	if cfg.AlertThrottleInterval != "45m" {
		t.Fatalf("AlertThrottleInterval = %q, want 45m", cfg.AlertThrottleInterval)
	}
	if err := applyAlertThrottleFromConfig(cfg); err != nil {
		t.Fatalf("applyAlertThrottleFromConfig: %v", err)
	}
	if effectiveAlertThrottleInterval() != 45*time.Minute {
		t.Fatalf("runtime interval = %s, want 45m", effectiveAlertThrottleInterval())
	}
}

func TestApplyHotReloadConfig_UpdatesAlertThrottleInterval(t *testing.T) {
	prev := alertThrottleInterval
	t.Cleanup(func() { alertThrottleInterval = prev })
	applyAlertThrottleInterval(time.Hour)

	cfg := minimalReloadConfig(nil)
	next := minimalReloadConfig(nil)
	next.AlertThrottleInterval = "2h"

	changes, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "alert_throttle_interval") {
		t.Fatalf("expected alert_throttle_interval change, got %v", changes)
	}
	if effectiveAlertThrottleInterval() != 2*time.Hour {
		t.Fatalf("runtime interval = %s, want 2h", effectiveAlertThrottleInterval())
	}
}

func TestLoadConfigForProbe_DoesNotMutateAdoptedAlertThrottleInterval(t *testing.T) {
	prev := alertThrottleInterval
	t.Cleanup(func() { alertThrottleInterval = prev })
	applyAlertThrottleInterval(90 * time.Minute)

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"alert_throttle_interval": "15m",
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
	if effectiveAlertThrottleInterval() != 90*time.Minute {
		t.Fatalf("probe load mutated runtime interval to %s, want 90m", effectiveAlertThrottleInterval())
	}
}

func TestAlertThrottleInterval_ConcurrentProbeDoesNotRace(t *testing.T) {
	prev := alertThrottleInterval
	t.Cleanup(func() { alertThrottleInterval = prev })
	applyAlertThrottleInterval(time.Hour)

	dir := t.TempDir()
	path := dir + "/config.json"
	cfgJSON := `{
		"alert_throttle_interval": "30m",
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
			_ = effectiveAlertThrottleInterval()
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := LoadConfigForProbe(path); err != nil {
			t.Fatalf("LoadConfigForProbe: %v", err)
		}
	}
	<-done
	if effectiveAlertThrottleInterval() != time.Hour {
		t.Fatalf("runtime interval = %s, want 1h", effectiveAlertThrottleInterval())
	}
}
