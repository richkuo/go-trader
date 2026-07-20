package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// tryClaimKillSwitchResetPrompt atomically claims the single-flight slot for
// the kill-switch reset AskOwnerDM goroutine (#1396). Returns true iff this
// caller owns the prompt; the owner MUST call releaseKillSwitchResetPrompt
// when the goroutine exits (typically via defer).
func tryClaimKillSwitchResetPrompt(running *atomic.Bool) bool {
	return running.CompareAndSwap(false, true)
}

// releaseKillSwitchResetPrompt clears the single-flight slot so a later cycle
// can prompt again after the AskOwnerDM goroutine finishes.
func releaseKillSwitchResetPrompt(running *atomic.Bool) {
	running.Store(false)
}

// DefaultKillSwitchResetDMTimeout is the AskOwnerDM wait for the portfolio
// kill-switch reset prompt when kill_switch_reset_dm_timeout is omitted
// from config.json (#1368). Quieter than the former hard-coded 30m.
const DefaultKillSwitchResetDMTimeout = 6 * time.Hour

// killSwitchResetDMTimeout is the active AskOwnerDM wait for the kill-switch
// reset prompt. Set from config at load and on SIGHUP hot-reload. Independent
// of alert_throttle_interval (#1266) — that knob is a fire-and-forget re-alert
// back-off, not an interactive reply wait.
var killSwitchResetDMTimeout = DefaultKillSwitchResetDMTimeout

func effectiveKillSwitchResetDMTimeout() time.Duration {
	return killSwitchResetDMTimeout
}

func applyKillSwitchResetDMTimeout(d time.Duration) {
	killSwitchResetDMTimeout = d
}

// applyKillSwitchResetDMTimeoutFromConfig adopts cfg's
// kill_switch_reset_dm_timeout into the live runtime. Call only when a config
// is actually adopted (daemon startup or an accepted SIGHUP reload) — never
// from loadConfig/LoadConfigForProbe.
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

// ParseKillSwitchResetDMTimeout converts kill_switch_reset_dm_timeout config
// text to a duration. Empty/missing → DefaultKillSwitchResetDMTimeout (6h).
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
