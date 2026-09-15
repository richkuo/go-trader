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

// parseKillSwitchResetReply resolves the owner's reply to exactly one latched
// partition. A bare "reset" is accepted only when one partition is latched, so
// an ambiguous reply never clears the wrong source's latch.
func parseKillSwitchResetReply(resp string, latched []RiskPartition) (RiskPartition, error) {
	reply := strings.ToLower(strings.TrimSpace(resp))
	if len(latched) == 0 {
		return unassignedPartition, fmt.Errorf("no portfolio scope is latched; nothing to reset")
	}
	if reply == "reset" {
		if len(latched) == 1 {
			return latched[0], nil
		}
		return unassignedPartition, fmt.Errorf("%d portfolio scopes are latched (%s); reply %s to name the one to clear",
			len(latched), joinScopeLabels(latched), killSwitchResetReplyOptions(latched))
	}
	named, ok := strings.CutPrefix(reply, "reset ")
	if !ok {
		return unassignedPartition, fmt.Errorf("unexpected reply %q; reply 'reset' when one scope is latched, or %s when several are", resp, killSwitchResetReplyOptions(latched))
	}
	part, err := parseRiskPartition(strings.TrimSpace(named))
	if err != nil {
		return unassignedPartition, fmt.Errorf("unexpected reply %q; reply 'reset' when one scope is latched, or %s when several are", resp, killSwitchResetReplyOptions(latched))
	}
	if !partitionInList(part, latched) {
		return unassignedPartition, fmt.Errorf("the %s scope is not latched; latched scope(s): %s", partitionLabel(part), joinScopeLabels(latched))
	}
	return part, nil
}

// killSwitchResetReplyOptions lists the exact replies that clear each latched
// partition, so the owner never has to guess a source id.
func killSwitchResetReplyOptions(latched []RiskPartition) string {
	opts := make([]string, 0, len(latched))
	for _, p := range latched {
		opts = append(opts, fmt.Sprintf("'reset %s'", p.String()))
	}
	return strings.Join(opts, " / ")
}

func joinScopeLabels(parts []RiskPartition) string {
	labels := make([]string, 0, len(parts))
	for _, p := range parts {
		labels = append(labels, partitionLabel(p))
	}
	return strings.Join(labels, ", ")
}
