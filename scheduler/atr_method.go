package main

import (
	"fmt"
	"sort"
	"strings"
)

// ATR smoothing methods (#1277). "simple" is the frozen legacy rolling mean
// of True Range (with the #887 >=100 integer-rounding convention); "wilder"
// is the published Wilder RMA (ewm(alpha=1/period, adjust=False), never
// rounded). The vocabulary must stay in lockstep with
// shared_strategies/open/indicators_core.py (ATR_METHODS).
const (
	ATRMethodSimple = "simple"
	ATRMethodWilder = "wilder"
)

// normalizeATRMethod canonicalizes a raw atr_method config value. Empty stays
// empty (= inherit); validateConfig rejects anything else outside the
// vocabulary, so downstream consumers only ever see "", "simple", or "wilder".
func normalizeATRMethod(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validATRMethodValue reports whether a raw config value is acceptable:
// "" (inherit/default) or one of the canonical methods.
func validATRMethodValue(raw string) bool {
	switch normalizeATRMethod(raw) {
	case "", ATRMethodSimple, ATRMethodWilder:
		return true
	}
	return false
}

// resolveATRMethod resolves the effective ATR smoothing method for a strategy
// (#1277): per-strategy atr_method > global atr_method > "simple". Read via
// this accessor, never the fields directly — the wilder cutover must resolve
// identically at every consumer (check-script argv, manual fetch-atr, tuner
// simulate payload, hot-reload guard), or two surfaces would stamp EntryATR
// under different math for the same strategy.
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

// appendATRMethodArg appends the resolved --atr-method flag to a check-script
// signal-check argv (#1277). Always appended, even for the default — the argv
// contract is enforced by the startup probe (probeArgv carries the flag), and
// an unconditional flag keeps the runtime argv shape uniform across methods.
func appendATRMethodArg(args []string, method string) []string {
	return append(args, "--atr-method="+method)
}

// stampATRMethodAtOpenIfOpened freezes the resolved atr_method (#1277) on a
// FRESH open only (never a scale-in add — mirrors the RiskAnchorPrice/
// EntryATR freeze-at-entry convention) so checkATRMethodDriftAtStartup can
// later detect a config edit + restart that changed the effective method
// while this position stayed open — a gap the SIGHUP hot-reload guard
// (config_reload.go validateHotReloadStateCompatible) cannot see, since a
// fresh process has no "old" resolved value to diff against. Options are
// never dispatched here (no ATR surface; atr_method is rejected on them at
// load), matching the other four stamp-at-open call sites.
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

// checkATRMethodDriftAtStartup detects the one restart-time gap the SIGHUP
// hot-reload guard can't cover (#1277 optional hardening, follow-up to the
// v17 migration): an operator edits atr_method on disk and restarts the
// process (rather than SIGHUP) while a strategy still holds a position opened
// under a different resolved method. validateHotReloadStateCompatible only
// runs on the SIGHUP path — a fresh process just loads the new config and
// forwards the new --atr-method every cycle, so any live-recomputed close
// evaluator (tiered_tp_atr_live, atr_stop/avwap_stop with atr_source=live)
// silently re-bases that position's stop/TP distance under the new smoothing
// mid-flight. This scans once per boot, at the same coarse per-strategy
// granularity as the SIGHUP guard (not per-close-evaluator — mirroring
// config_reload.go:603 keeps the two checks consistent and avoids duplicating
// each close strategy's atr_source default in Go). Mirrors
// ValidatePerpsDirectionConfig's shape: collect warnings before the notifier
// is wired, forward to the owner DM once it is.
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
