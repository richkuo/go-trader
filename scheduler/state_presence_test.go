package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasLiveStrategy(t *testing.T) {
	cases := []struct {
		name string
		in   []StrategyConfig
		want bool
	}{
		{"empty", nil, false},
		{"paper only", []StrategyConfig{{ID: "a", Args: []string{"--mode=paper"}}}, false},
		{"no mode arg", []StrategyConfig{{ID: "a", Args: []string{"--symbol=BTC"}}}, false},
		{"one live among many", []StrategyConfig{
			{ID: "a", Args: []string{"--mode=paper"}},
			{ID: "b", Args: []string{"foo", "--mode=live"}},
		}, true},
		{"mode=live suffix only does not match", []StrategyConfig{
			{ID: "a", Args: []string{"mode=live"}},
		}, false},
		{"two-token --mode live form", []StrategyConfig{
			{ID: "a", Args: []string{"--mode", "live"}},
		}, true},
		{"two-token --mode paper form", []StrategyConfig{
			{ID: "a", Args: []string{"--mode", "paper"}},
		}, false},
		{"trailing --mode with no value", []StrategyConfig{
			{ID: "a", Args: []string{"--mode"}},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasLiveStrategy(c.in); got != c.want {
				t.Fatalf("HasLiveStrategy=%v want %v", got, c.want)
			}
		})
	}
}

func TestCheckStatePresence(t *testing.T) {
	dir := t.TempDir()
	liveCfg := []StrategyConfig{{ID: "hl-x", Args: []string{"--mode=live"}}}
	paperCfg := []StrategyConfig{{ID: "hl-x", Args: []string{"--mode=paper"}}}

	// Missing DB + live → warns.
	missing := filepath.Join(dir, "missing.db")
	if got := CheckStatePresence(missing, liveCfg); !strings.Contains(got, "CRITICAL") {
		t.Fatalf("expected CRITICAL warning for missing live DB, got %q", got)
	}

	// Missing DB + paper-only → no warning.
	if got := CheckStatePresence(missing, paperCfg); got != "" {
		t.Fatalf("expected no warning for paper-only, got %q", got)
	}

	// Existing DB + live → no warning.
	existing := filepath.Join(dir, "state.db")
	if err := os.WriteFile(existing, nil, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := CheckStatePresence(existing, liveCfg); got != "" {
		t.Fatalf("expected no warning for existing DB, got %q", got)
	}
}

func TestAllowMissingState(t *testing.T) {
	cases := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{"unset", "", false, false},
		{"empty", "", true, false},
		{"one", "1", true, true},
		{"true string not accepted", "true", true, false},
		{"yes string not accepted", "yes", true, false},
		{"zero", "0", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("GO_TRADER_ALLOW_MISSING_STATE", c.val)
			} else {
				os.Unsetenv("GO_TRADER_ALLOW_MISSING_STATE")
			}
			if got := AllowMissingState(); got != c.want {
				t.Fatalf("AllowMissingState=%v want %v", got, c.want)
			}
		})
	}
}
