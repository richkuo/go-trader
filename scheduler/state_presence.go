package main

import (
	"fmt"
	"os"
)

// isLiveArgs reports whether a check-script arg list selects live mode. It
// recognizes both the joined form (--mode=live) and the split form
// (--mode live). Canonical predicate shared by HasLiveStrategy and every
// per-platform <plat>IsLive helper so walletKeyFor, startup state-presence
// checks, and live-execution guards agree on what "live" means (#364).
func isLiveArgs(args []string) bool {
	for i, arg := range args {
		if arg == "--mode=live" {
			return true
		}
		if arg == "--mode" && i+1 < len(args) && args[i+1] == "live" {
			return true
		}
	}
	return false
}

// HasLiveStrategy reports whether any configured strategy passes --mode=live
// to its check script. Paper-only deployments don't hold persistent exchange
// state, so a missing state.db on first startup is expected for them — the
// wipe-on-update concern from issue #339 only matters when live positions
// exist.
func HasLiveStrategy(strategies []StrategyConfig) bool {
	for _, sc := range strategies {
		if isLiveArgs(sc.Args) {
			return true
		}
	}
	return false
}

// CheckStatePresence returns a CRITICAL warning when a live deployment is
// starting without its persisted state DB — typically a sign that the update
// process wiped the repo directory instead of running `git pull` in place
// (issue #339). Returns empty string when no warning is needed.
//
// The check runs BEFORE OpenStateDB because that call creates the file if it
// doesn't exist, erasing the signal.
func CheckStatePresence(dbPath string, strategies []StrategyConfig) string {
	if !HasLiveStrategy(strategies) {
		return ""
	}
	if _, err := os.Stat(dbPath); err == nil {
		return ""
	} else if !os.IsNotExist(err) {
		// Non-IsNotExist errors (permission denied, transient I/O) are
		// deliberately ignored — the #339 concern is specifically the
		// wiped-directory case, and warning on transient filesystem hiccups
		// would generate false positives.
		return ""
	}
	return fmt.Sprintf(
		"CRITICAL: state DB %q is missing but live strategies are configured. "+
			"If you just updated the trader, the directory may have been wiped instead of "+
			"`git pull`ed in place — open positions and trade history will not be reconciled "+
			"on this cycle. See issue #339. Set GO_TRADER_ALLOW_MISSING_STATE=1 to silence "+
			"this warning (e.g. genuine first-run deployments).",
		dbPath,
	)
}

// AllowMissingState returns true when the operator has explicitly opted out
// of the missing-state warning — used by genuine first-run deployments where
// no DB is expected yet.
func AllowMissingState() bool {
	return os.Getenv("GO_TRADER_ALLOW_MISSING_STATE") == "1"
}
