package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

func tryClaimKillSwitchResetPrompt(running *atomic.Bool) bool {
	return running.CompareAndSwap(false, true)
}

func releaseKillSwitchResetPrompt(running *atomic.Bool) {
	running.Store(false)
}

const DefaultKillSwitchResetDMTimeout = 6 * time.Hour

var killSwitchResetDMTimeout = DefaultKillSwitchResetDMTimeout

func effectiveKillSwitchResetDMTimeout() time.Duration {
	return killSwitchResetDMTimeout
}

func applyKillSwitchResetDMTimeout(d time.Duration) {
	killSwitchResetDMTimeout = d
}

func applyKillSwitchResetDMTimeoutFromConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	d, err := ParseKillSwitchResetDMTimeout(cfg.KillSwitchResetDMTimeout)
	if err != nil {
		return err
	}
	applyKillSwitchResetDMTimeout(d)
	return nil
}

func ParseKillSwitchResetDMTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultKillSwitchResetDMTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid kill_switch_reset_dm_timeout %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("kill_switch_reset_dm_timeout must be > 0, got %s", s)
	}
	return d, nil
}
