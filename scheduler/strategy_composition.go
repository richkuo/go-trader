package main

import (
	"fmt"
	"strings"
)

// StrategyDecisionFields is the optional open/close decision metadata emitted
// by check scripts when a strategy opts into issue #480's split entry/exit
// model. The legacy signal field remains authoritative for execution.
type StrategyDecisionFields struct {
	OpenStrategy    string   `json:"open_strategy,omitempty"`
	CloseStrategies []string `json:"close_strategies,omitempty"`
	OpenAction      string   `json:"open_action,omitempty"`
	CloseFraction   float64  `json:"close_fraction"`
	CloseStrategy   string   `json:"close_strategy,omitempty"`
}

func usesOpenCloseConfig(sc StrategyConfig) bool {
	return strings.TrimSpace(sc.OpenStrategy) != "" || len(sc.CloseStrategies) > 0 || sc.DisableImplicitClose
}

func strategyNameFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.TrimSpace(args[0])
}

func effectiveOpenStrategy(sc StrategyConfig) string {
	if name := strings.TrimSpace(sc.OpenStrategy); name != "" {
		return name
	}
	return strategyNameFromArgs(sc.Args)
}

func effectiveCloseStrategies(sc StrategyConfig) []string {
	if len(sc.CloseStrategies) > 0 {
		out := make([]string, 0, len(sc.CloseStrategies))
		for _, name := range sc.CloseStrategies {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	if sc.DisableImplicitClose {
		return nil
	}
	if name := effectiveOpenStrategy(sc); name != "" {
		return []string{name}
	}
	return nil
}

func validateStrategyConceptName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(trimmed, " \t\r\n,") {
		return fmt.Errorf("must be a single strategy id, got %q", name)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("must not start with '-'")
	}
	return nil
}

func appendOpenCloseArgs(args []string, sc StrategyConfig, positionSide string) []string {
	if !usesOpenCloseConfig(sc) {
		return args
	}
	out := append([]string{}, args...)
	if name := strings.TrimSpace(sc.OpenStrategy); name != "" {
		out = append(out, "--open-strategy", name)
	}
	if closeStrategies := explicitCloseStrategies(sc); len(closeStrategies) > 0 {
		out = append(out, "--close-strategies", strings.Join(closeStrategies, ","))
	}
	if sc.DisableImplicitClose {
		out = append(out, "--disable-implicit-close")
	}
	if side := strings.TrimSpace(positionSide); side != "" {
		out = append(out, "--position-side", side)
	}
	return out
}

func explicitCloseStrategies(sc StrategyConfig) []string {
	if len(sc.CloseStrategies) == 0 {
		return nil
	}
	out := make([]string, 0, len(sc.CloseStrategies))
	for _, name := range sc.CloseStrategies {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func maxCloseFraction(fractions []float64) float64 {
	max := 0.0
	for _, f := range fractions {
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		if f > max {
			max = f
		}
	}
	return max
}

func composeOpenCloseSignal(openAction string, closeFraction float64, positionSide string) int {
	if closeFraction < 0 {
		closeFraction = 0
	}
	if closeFraction > 1 {
		closeFraction = 1
	}
	if closeFraction > 0 {
		switch positionSide {
		case "long":
			return -1
		case "short":
			return 1
		default:
			return 0
		}
	}
	if positionSide != "" {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(openAction)) {
	case "long":
		return 1
	case "short":
		return -1
	default:
		return 0
	}
}
