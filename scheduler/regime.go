package main

import (
	"fmt"
	"strings"
)

const (
	RegimeGateOnFailureOpen   = "open"
	RegimeGateOnFailureClosed = "closed"
)

func normalizeRegimeGateOnFailure(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func parseRegimeGateOnFailure(v string) (string, error) {
	n := normalizeRegimeGateOnFailure(v)
	switch n {
	case "", RegimeGateOnFailureOpen, RegimeGateOnFailureClosed:
		return n, nil
	}
	return "", fmt.Errorf("regime_gate_on_failure must be %q or %q, got %q", RegimeGateOnFailureOpen, RegimeGateOnFailureClosed, v)
}

func resolveRegimeGateOnFailure(sc StrategyConfig, rc *RegimeConfig) string {
	if v := normalizeRegimeGateOnFailure(sc.RegimeGateOnFailure); v != "" {
		return v
	}
	if rc != nil {
		if v := normalizeRegimeGateOnFailure(rc.GateOnFailure); v != "" {
			return v
		}
	}
	return RegimeGateOnFailureOpen
}

func regimeAllowsEntry(allowed []string, current string) bool {
	if len(allowed) == 0 || current == "" {
		return true
	}
	cur := strings.TrimSpace(current)
	for _, label := range allowed {
		if label == cur {
			return true
		}
	}

	if regimeDirectionalSubs[cur] {
		for _, label := range allowed {
			if label == regimeDirectionalBare {
				return true
			}
		}
	}
	return false
}

func regimeBlocksOpen(allowed []string, current string, posQty float64, failClosed bool) bool {
	if posQty > 0 {
		return false
	}
	if failClosed && len(allowed) > 0 && strings.TrimSpace(current) == "" {
		return true
	}
	return !regimeAllowsEntry(allowed, current)
}

func regimeGateBlockDetail(gateLabel string) string {
	if strings.TrimSpace(gateLabel) == "" {
		return "regime unknown, fail-closed"
	}
	return "regime=" + gateLabel
}
