package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHTFTableMatchesPython(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "shared_tools", "testdata", "htf_map.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var spec struct {
		Map      map[string]string `json:"map"`
		Default  string            `json:"default"`
		Lookback int               `json:"lookback"`
	}
	if err := json.Unmarshal(blob, &spec); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(spec.Map) != len(hlFeedHTFMap) {
		t.Fatalf("table size: Go has %d entries, the fixture has %d", len(hlFeedHTFMap), len(spec.Map))
	}
	for tf, want := range spec.Map {
		if got := hlFeedHTFTimeframe(tf); got != want {
			t.Fatalf("%s: Go maps to %q, the fixture says %q", tf, got, want)
		}
	}
	if got := hlFeedHTFTimeframe("7m"); got != spec.Default {
		t.Fatalf("default: got %q want %q", got, spec.Default)
	}
	if hlFeedHTFLookback != spec.Lookback {
		t.Fatalf("lookback: Go uses %d, the fixture says %d", hlFeedHTFLookback, spec.Lookback)
	}
}
