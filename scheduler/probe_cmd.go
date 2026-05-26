package main

import (
	"flag"
	"fmt"
	"os"
)

// runProbe is the `go-trader probe` subcommand: load the config, invoke each
// configured check script with --probe-only, and exit non-zero on failure.
// scripts/update.sh calls this on a freshly built binary against the freshly
// synced Python *before* swapping the live binary, so a binary/Python argv
// mismatch aborts the update with the live binary still running.
func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := LoadConfigForProbe(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: failed to load config %s: %v\n", *configPath, err)
		return 1
	}

	if err := probeCheckScripts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		return 1
	}

	fmt.Printf("probe: OK (%d unique check scripts, version=%s)\n", len(uniqueCheckScripts(cfg)), Version)
	return 0
}
