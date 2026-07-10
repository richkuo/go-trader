package main

import "strings"

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
