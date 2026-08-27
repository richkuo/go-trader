package main

import (
	"fmt"
	"os"
)

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

func HasLiveStrategy(strategies []StrategyConfig) bool {
	for _, sc := range strategies {
		if isLiveArgs(sc.Args) {
			return true
		}
	}
	return false
}

func CheckStatePresence(dbPath string, strategies []StrategyConfig) string {
	if !HasLiveStrategy(strategies) {
		return ""
	}
	if _, err := os.Stat(dbPath); err == nil {
		return ""
	} else if !os.IsNotExist(err) {
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

func AllowMissingState() bool {
	return os.Getenv("GO_TRADER_ALLOW_MISSING_STATE") == "1"
}
