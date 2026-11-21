package main

import (
	"crypto/rand"
	"encoding/hex"
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

type killSwitchLatchInstance struct {
	Partition RiskPartition
	LatchedAt time.Time
}

type killSwitchResetRequest struct {
	Code     string
	Prompted []killSwitchLatchInstance
	Reply    string
}

type killSwitchResetResult struct {
	Target    RiskPartition
	PeakValue float64
	Remaining []RiskPartition
	Err       error
	SaveErr   error
}

func newKillSwitchResetCode() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

func captureKillSwitchLatchInstances(state *AppState, parts []RiskPartition) []killSwitchLatchInstance {
	out := make([]killSwitchLatchInstance, 0, len(parts))
	for _, p := range parts {
		prs := state.partitionRiskIfPresent(p)
		if prs == nil || !prs.KillSwitchActive {
			continue
		}
		out = append(out, killSwitchLatchInstance{Partition: p, LatchedAt: prs.KillSwitchAt})
	}
	return out
}

func killSwitchInstancePartitions(instances []killSwitchLatchInstance) []RiskPartition {
	out := make([]RiskPartition, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.Partition)
	}
	return out
}

// parseKillSwitchResetReply resolves the owner's reply to exactly one latched
// partition. The reply must carry the prompt's code, so a delayed reply to an
// older prompt never authorizes a newer one. A bare "reset <code>" is accepted
// only when one partition is latched, so an ambiguous reply never clears the
// wrong source's latch.
func parseKillSwitchResetReply(resp, code string, latched []RiskPartition) (RiskPartition, error) {
	reply := strings.ToLower(strings.TrimSpace(resp))
	if len(latched) == 0 {
		return unassignedPartition, fmt.Errorf("no portfolio scope is latched; nothing to reset")
	}
	fields := strings.Fields(reply)
	usage := fmt.Sprintf("reply %s", killSwitchResetReplyOptionsWithCode(latched, code))
	if len(fields) < 2 || len(fields) > 3 || fields[0] != "reset" {
		return unassignedPartition, fmt.Errorf("unexpected reply %q; %s", resp, usage)
	}
	if fields[len(fields)-1] != strings.ToLower(code) {
		return unassignedPartition, fmt.Errorf("reply %q does not carry the code of the current kill switch prompt (%s); no latch was changed. %s", resp, code, usage)
	}
	if len(fields) == 2 {
		if len(latched) == 1 {
			return latched[0], nil
		}
		return unassignedPartition, fmt.Errorf("%d portfolio scopes are latched (%s); %s to name the one to clear",
			len(latched), joinScopeLabels(latched), usage)
	}
	part, err := parseRiskPartition(fields[1])
	if err != nil {
		return unassignedPartition, fmt.Errorf("unexpected reply %q; %s", resp, usage)
	}
	if !partitionInList(part, latched) {
		return unassignedPartition, fmt.Errorf("the %s scope is not latched in this prompt; latched scope(s): %s", partitionLabel(part), joinScopeLabels(latched))
	}
	return part, nil
}

func resolveKillSwitchResetTarget(state *AppState, req killSwitchResetRequest) (RiskPartition, error) {
	prompted := killSwitchInstancePartitions(req.Prompted)
	target, err := parseKillSwitchResetReply(req.Reply, req.Code, prompted)
	if err != nil {
		return unassignedPartition, err
	}
	current := state.latchedPartitions()
	if len(strings.Fields(req.Reply)) == 2 && (len(current) != 1 || current[0] != target) {
		return unassignedPartition, fmt.Errorf("the latched scopes changed since the prompt (now: %s); no latch was changed. Wait for the next prompt", joinScopeLabels(current))
	}
	for _, inst := range req.Prompted {
		if inst.Partition != target {
			continue
		}
		prs := state.partitionRiskIfPresent(target)
		if prs == nil || !prs.KillSwitchActive || !prs.KillSwitchAt.Equal(inst.LatchedAt) {
			return unassignedPartition, fmt.Errorf("the %s latch in this prompt was cleared or replaced since the prompt was sent; no latch was changed. Wait for the next prompt", partitionLabel(target))
		}
		return target, nil
	}
	return unassignedPartition, fmt.Errorf("the %s scope is not latched in this prompt", partitionLabel(target))
}

// applyKillSwitchResetReply runs at a cycle boundary under the state lock. It
// commits the reset, the pending peak change and the reset event for the one
// selected partition, or restores the latched state when the save fails.
func applyKillSwitchResetReply(state *AppState, req killSwitchResetRequest, save func(RiskPartition) error) killSwitchResetResult {
	target, err := resolveKillSwitchResetTarget(state, req)
	if err != nil {
		return killSwitchResetResult{Err: err}
	}
	prs := state.partitionRisk(target)
	before := clonePortfolioRiskState(prs)
	resetDrawdownPct := ResetPortfolioKillSwitchManual(prs)
	addKillSwitchEvent(prs, "reset", "", resetDrawdownPct, 0, prs.PeakValue,
		fmt.Sprintf("manual reset via DM (%s scope); peak $%.2f kept until the first eligible equity reading after this reset", partitionLabel(target), prs.PeakValue))
	prs.Events[len(prs.Events)-1].Partition = target
	saveErr := fmt.Errorf("state store unavailable")
	if save != nil {
		saveErr = save(target)
	}
	if saveErr != nil {
		*prs = before
		return killSwitchResetResult{Target: target, SaveErr: saveErr,
			Err: fmt.Errorf("the %s kill switch reset was NOT applied: the state save failed (%v). The latch stays in force; a new prompt follows", partitionLabel(target), saveErr)}
	}
	return killSwitchResetResult{Target: target, PeakValue: prs.PeakValue, Remaining: state.latchedPartitions()}
}

func formatKillSwitchResetAck(res killSwitchResetResult) string {
	ack := fmt.Sprintf("Kill switch reset (%s scope). Trading can resume next cycle. The drawdown peak stays at $%.2f until the first eligible equity reading after this reset; that reading then becomes the new peak and you get a follow-up message. This reset does not close or protect any position: verify open positions and their stop-loss protection.",
		partitionLabel(res.Target), res.PeakValue)
	if len(res.Remaining) > 0 {
		ack += fmt.Sprintf(" The %s scope stays latched; a new prompt follows next cycle.", joinScopeLabels(res.Remaining))
	}
	return ack
}

func killSwitchResetReplyOptionsWithCode(latched []RiskPartition, code string) string {
	if len(latched) == 1 {
		return fmt.Sprintf("'reset %s'", code)
	}
	opts := make([]string, 0, len(latched))
	for _, p := range latched {
		opts = append(opts, fmt.Sprintf("'reset %s %s'", p.String(), code))
	}
	return strings.Join(opts, " / ")
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
