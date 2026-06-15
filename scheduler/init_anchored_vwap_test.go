package main

import "testing"

func TestAnchoredVWAPWiring(t *testing.T) {
	if got := deriveShortName("anchored_vwap"); got != "avwap" {
		t.Fatalf("deriveShortName(anchored_vwap) = %q, want avwap", got)
	}
	if !isBidirectionalPerpsStrategy("anchored_vwap") {
		t.Fatal("anchored_vwap must be a bidirectional perps strategy")
	}
	lists := map[string][]stratDef{
		"spot":    defaultSpotStrategies,
		"perps":   defaultPerpsStrategies,
		"futures": defaultFuturesStrategies,
	}
	for name, list := range lists {
		found := false
		for _, s := range list {
			if s.ID == "anchored_vwap" {
				found = true
				if s.ShortName != "avwap" {
					t.Fatalf("%s list: anchored_vwap short name = %q, want avwap", name, s.ShortName)
				}
			}
		}
		if !found {
			t.Fatalf("anchored_vwap missing from default %s list", name)
		}
	}
}
