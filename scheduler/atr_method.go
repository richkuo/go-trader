package main

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ATRMethodSimple = "simple"
	ATRMethodWilder = "wilder"
)

func normalizeATRMethod(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validATRMethodValue(raw string) bool {
	switch normalizeATRMethod(raw) {
	case "", ATRMethodSimple, ATRMethodWilder:
		return true
	}
	return false
}

func resolveATRMethod(sc StrategyConfig, cfg *Config) string {
	if m := normalizeATRMethod(sc.ATRMethod); m != "" {
		return m
	}
	if cfg != nil {
		if m := normalizeATRMethod(cfg.ATRMethod); m != "" {
			return m
		}
	}
	return ATRMethodSimple
}

func appendATRMethodArg(args []string, method string) []string {
	return append(args, "--atr-method="+method)
}

func stampATRMethodAtOpenIfOpened(s *StrategyState, symbol string, opened bool, sc StrategyConfig, cfg *Config) {
	if s == nil || !opened {
		return
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return
	}
	pos.ATRMethodAtOpen = resolveATRMethod(sc, cfg)
}

func checkATRMethodDriftAtStartup(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	var warnings []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Type == "options" {
			continue
		}
		s, ok := state.Strategies[sc.ID]
		if !ok {
			continue
		}
		resolved := resolveATRMethod(*sc, cfg)
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			if pos.ATRMethodAtOpen == "" || pos.ATRMethodAtOpen == resolved {
				continue
			}
			msg := fmt.Sprintf("atr_method drift: strategy %s %s opened under atr_method=%q but now resolves %q — config was edited and the process restarted (not SIGHUP'd) while the position stayed open. Live-recomputed close evaluators (tiered_tp_atr_live, atr_stop/avwap_stop with atr_source=live) are re-based to the new smoothing for this position; the frozen entry-ATR and on-chain protection stay under the original math. Flatten and reopen to fully re-baseline, or revert atr_method to %q for this strategy.",
				sc.ID, sym, pos.ATRMethodAtOpen, resolved, pos.ATRMethodAtOpen)
			fmt.Printf("[WARN] %s\n", msg)
			warnings = append(warnings, msg)
		}
	}
	return warnings
}
