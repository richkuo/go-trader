package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestM5DeprecatedRosterMatchesPythonRegistry enforces the cross-language
// mirror (#1275): the Go m5DeprecatedEdgeStrategies set must stay identical
// to M5_DEPRECATED_EDGE_STRATEGIES in shared_strategies/open/registry.py.
// Go CI must not spawn Python, so this parses the Python source directly.
func TestM5DeprecatedRosterMatchesPythonRegistry(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "shared_strategies", "open", "registry.py"))
	if err != nil {
		t.Fatalf("read registry.py: %v", err)
	}
	start := strings.Index(string(src), "M5_DEPRECATED_EDGE_STRATEGIES = frozenset({")
	if start < 0 {
		t.Fatal("M5_DEPRECATED_EDGE_STRATEGIES block not found in registry.py")
	}
	rest := string(src)[start:]
	end := strings.Index(rest, "})")
	if end < 0 {
		t.Fatal("M5_DEPRECATED_EDGE_STRATEGIES block not terminated")
	}
	block := rest[:end]
	pyNames := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllStringSubmatch(block, -1) {
		pyNames[m[1]] = struct{}{}
	}
	for name := range pyNames {
		if _, ok := m5DeprecatedEdgeStrategies[name]; !ok {
			t.Errorf("registry.py quarantines %q but Go m5DeprecatedEdgeStrategies is missing it", name)
		}
	}
	for name := range m5DeprecatedEdgeStrategies {
		if _, ok := pyNames[name]; !ok {
			t.Errorf("Go m5DeprecatedEdgeStrategies has %q but registry.py does not quarantine it", name)
		}
	}
	if len(pyNames) != 26 {
		t.Errorf("expected 26 quarantined names in registry.py, parsed %d", len(pyNames))
	}
}

func TestStrategyEdgeDeprecatedResolution(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
		want bool
	}{
		{"open ref hit", StrategyConfig{Type: "spot", OpenStrategy: StrategyRef{Name: "macd"}}, true},
		{"args0 hit", StrategyConfig{Type: "perps", Args: []string{"rsi", "BTC", "1h"}}, true},
		{"open ref wins over args0", StrategyConfig{Type: "spot", OpenStrategy: StrategyRef{Name: "tema_cross"}, Args: []string{"macd", "BTC", "1h"}}, false},
		{"clean strategy", StrategyConfig{Type: "spot", OpenStrategy: StrategyRef{Name: "regime_adaptive"}}, false},
		{"manual hold", StrategyConfig{Type: "manual", Args: []string{"hold"}}, false},
		{"options registry excluded", StrategyConfig{Type: "options", Args: []string{"momentum"}}, false},
		{"empty", StrategyConfig{Type: "spot"}, false},
	}
	for _, tc := range cases {
		if got := strategyEdgeDeprecated(tc.sc); got != tc.want {
			t.Errorf("%s: strategyEdgeDeprecated=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeprecatedEdgeStartupWarnings(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "s-macd", Type: "spot", OpenStrategy: StrategyRef{Name: "macd"}},
		{ID: "s-ack", Type: "perps", OpenStrategy: StrategyRef{Name: "rsi"}, AllowDeprecated: true},
		{ID: "s-clean", Type: "spot", OpenStrategy: StrategyRef{Name: "regime_adaptive"}},
	}
	warnings := deprecatedEdgeStartupWarnings(strategies)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "s-macd") || !strings.Contains(warnings[0], "open=macd") {
		t.Errorf("warning should name the strategy and open leg: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "allow_deprecated") {
		t.Errorf("warning should tell the operator how to acknowledge: %s", warnings[0])
	}
}

func TestEdgeStatusSummaryTagNeverHiddenByAck(t *testing.T) {
	dep := StrategyConfig{ID: "s", Type: "spot", OpenStrategy: StrategyRef{Name: "macd"}}
	if got := edgeStatusSummaryTag(dep); got != "edge=deprecated_m5" {
		t.Errorf("tag = %q, want edge=deprecated_m5", got)
	}
	dep.AllowDeprecated = true
	if got := edgeStatusSummaryTag(dep); got != "edge=deprecated_m5(ack)" {
		t.Errorf("acknowledged tag = %q, want edge=deprecated_m5(ack)", got)
	}
	clean := StrategyConfig{ID: "s", Type: "spot", OpenStrategy: StrategyRef{Name: "tema_cross"}}
	if got := edgeStatusSummaryTag(clean); got != "" {
		t.Errorf("clean strategy tag = %q, want empty", got)
	}
	// The startup summary line carries the tag (both raw and acknowledged).
	line := formatStrategySummaryLine(dep, nil)
	if !strings.Contains(line, "edge=deprecated_m5(ack)") {
		t.Errorf("summary line missing edge tag: %s", line)
	}
}
