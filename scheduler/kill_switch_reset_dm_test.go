package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeKillSwitchResetDMConfig(t *testing.T, timeoutField string) string {
	t.Helper()
	path := t.TempDir() + "/config.json"
	cfgJSON := `{
		` + timeoutField + `
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
	return path
}

func TestParseKillSwitchResetDMTimeout(t *testing.T) {
	if DefaultKillSwitchResetDMTimeout != 6*time.Hour {
		t.Fatalf("default = %s, want 6h", DefaultKillSwitchResetDMTimeout)
	}
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: DefaultKillSwitchResetDMTimeout},
		{raw: "30m", want: 30 * time.Minute},
		{raw: "nonsense", wantErr: true},
		{raw: "0", wantErr: true},
		{raw: "-1h", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			d, err := ParseKillSwitchResetDMTimeout(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %s", tc.raw, d)
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

func TestLoadConfig_KillSwitchResetDMTimeout(t *testing.T) {
	prev := killSwitchResetDMTimeout
	t.Cleanup(func() { killSwitchResetDMTimeout = prev })

	t.Run("applies configured value", func(t *testing.T) {
		cfg, err := LoadConfigForProbe(writeKillSwitchResetDMConfig(t, `"kill_switch_reset_dm_timeout": "45m",`))
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
	})
	t.Run("omitted defaults to six hours", func(t *testing.T) {
		if _, err := LoadConfigForProbe(writeKillSwitchResetDMConfig(t, "")); err != nil {
			t.Fatalf("LoadConfigForProbe: %v", err)
		}
		if err := applyKillSwitchResetDMTimeoutFromConfig(&Config{}); err != nil {
			t.Fatalf("applyKillSwitchResetDMTimeoutFromConfig: %v", err)
		}
		if effectiveKillSwitchResetDMTimeout() != DefaultKillSwitchResetDMTimeout {
			t.Fatalf("runtime timeout = %s, want %s", effectiveKillSwitchResetDMTimeout(), DefaultKillSwitchResetDMTimeout)
		}
	})
	t.Run("rejects invalid value", func(t *testing.T) {
		if _, err := LoadConfigForProbe(writeKillSwitchResetDMConfig(t, `"kill_switch_reset_dm_timeout": "nonsense",`)); err == nil {
			t.Fatal("expected LoadConfigForProbe to reject invalid kill_switch_reset_dm_timeout")
		}
	})
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

	path := writeKillSwitchResetDMConfig(t, `"kill_switch_reset_dm_timeout": "15m",`)
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("LoadConfigForProbe: %v", err)
	}
	if effectiveKillSwitchResetDMTimeout() != 90*time.Minute {
		t.Fatalf("probe load mutated runtime timeout to %s, want 90m", effectiveKillSwitchResetDMTimeout())
	}
}

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

	path := writeKillSwitchResetDMConfig(t, `"kill_switch_reset_dm_timeout": "30m",`)

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

func TestTryClaimKillSwitchResetPrompt_SingleOwner(t *testing.T) {
	var running atomic.Bool
	if !tryClaimKillSwitchResetPrompt(&running) {
		t.Fatal("first claim should succeed")
	}
	if tryClaimKillSwitchResetPrompt(&running) {
		t.Fatal("second claim must fail while held")
	}
	releaseKillSwitchResetPrompt(&running)
	if running.Load() {
		t.Fatal("flag must be clear after release")
	}
	if !tryClaimKillSwitchResetPrompt(&running) {
		t.Fatal("claim after release should succeed")
	}
	releaseKillSwitchResetPrompt(&running)
}

func TestTryClaimKillSwitchResetPrompt_ConcurrentClaimRelease(t *testing.T) {
	var running atomic.Bool
	const goroutines = 64
	const rounds = 400
	var claims atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if tryClaimKillSwitchResetPrompt(&running) {
					claims.Add(1)
					releaseKillSwitchResetPrompt(&running)
				}
			}
		}()
	}
	wg.Wait()
	if claims.Load() == 0 {
		t.Fatal("expected at least one successful claim")
	}
	if running.Load() {
		t.Fatal("flag still set after all releases")
	}
}
