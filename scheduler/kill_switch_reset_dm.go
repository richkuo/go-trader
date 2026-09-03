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

func parseKillSwitchResetReply(resp string, latched []PortfolioScope) (PortfolioScope, error) {
	reply := strings.ToLower(strings.TrimSpace(resp))
	if len(latched) == 0 {
		return scopeUnassigned, fmt.Errorf("no portfolio scope is latched; nothing to reset")
	}
	switch reply {
	case "reset":
		if len(latched) == 1 {
			return latched[0], nil
		}
		return scopeUnassigned, fmt.Errorf("two portfolio scopes are latched (%s); reply 'reset live' or 'reset paper' to name the one to clear", joinScopeLabels(latched))
	case "reset live":
		if !scopeInList(ScopeLive, latched) {
			return scopeUnassigned, fmt.Errorf("the live scope is not latched; latched scope(s): %s", joinScopeLabels(latched))
		}
		return ScopeLive, nil
	case "reset paper":
		if !scopeInList(ScopePaper, latched) {
			return scopeUnassigned, fmt.Errorf("the paper scope is not latched; latched scope(s): %s", joinScopeLabels(latched))
		}
		return ScopePaper, nil
	}
	return scopeUnassigned, fmt.Errorf("unexpected reply %q; reply 'reset' when one scope is latched, or 'reset live' / 'reset paper' when both are", resp)
}

func scopeInList(scope PortfolioScope, list []PortfolioScope) bool {
	for _, s := range list {
		if s == scope {
			return true
		}
	}
	return false
}

func joinScopeLabels(scopes []PortfolioScope) string {
	labels := make([]string, 0, len(scopes))
	for _, s := range scopes {
		labels = append(labels, scopeLabel(s))
	}
	return strings.Join(labels, ", ")
}
