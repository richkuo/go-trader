package main

import "testing"

func TestDeprecatedStrategiesNotInDefaultFallbackLists(t *testing.T) {
	deprecated := map[string]bool{
		"range_scalper":    true,
		"session_breakout": true,
		"vol_momentum":     true,
	}
	lists := map[string][]stratDef{
		"spot":    defaultSpotStrategies,
		"perps":   defaultPerpsStrategies,
		"futures": defaultFuturesStrategies,
	}
	for listName, list := range lists {
		for _, s := range list {
			if deprecated[s.ID] {
				t.Fatalf("%s fallback list still includes deprecated strategy %q", listName, s.ID)
			}
		}
	}
}
