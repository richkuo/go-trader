package main

import (
	"flag"
	"fmt"
	"os"
)

func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	deploymentConfig := fs.String("deployment-config", "", "Persistent config path that names the shared weight owner when --config is a staged copy (identity only; never read)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	identity := *configPath
	if *deploymentConfig != "" {
		identity = *deploymentConfig
	}

	if role, roleErr := peekConfigRole(*configPath); roleErr == nil && role == configRoleFeed {
		return runFeedProbe(*configPath, identity)
	}

	cfg, err := LoadConfigForProbeAdmission(*configPath, regimeSideProbeAdmissionEnforced())
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: failed to load config %s: %v\n", *configPath, err)
		return 1
	}

	if err := probeCheckScripts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		return ExitProbeFailure
	}
	if policy := cfg.MarketFeedWeightBudget.budget().weightPolicy(); policy != nil && cfg.marketFeedWebsocketEnabled() {
		if !printFeedAuthorityForecast(feedAuthorityRoleEmbedded, identity, policy) {
			return ExitProbeFailure
		}
	}

	fmt.Printf("probe: OK (%d unique check scripts, version=%s)\n", len(uniqueCheckScripts(cfg)), Version)
	return 0
}
