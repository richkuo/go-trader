package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// parsePythonFrozensetLiteral extracts the string members of a named
// `NAME = frozenset({...})` literal from shared_strategies/open/registry.py.
// Go CI must not spawn Python, so tests parse the Python source directly.
func parsePythonFrozensetLiteral(t *testing.T, name string) map[string]struct{} {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "shared_strategies", "open", "registry.py"))
	if err != nil {
		t.Fatalf("read registry.py: %v", err)
	}
	start := strings.Index(string(src), name+" = frozenset({")
	if start < 0 {
		t.Fatalf("%s block not found in registry.py", name)
	}
	rest := string(src)[start:]
	end := strings.Index(rest, "})")
	if end < 0 {
		t.Fatalf("%s block not terminated", name)
	}
	block := rest[:end]
	names := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllStringSubmatch(block, -1) {
		names[m[1]] = struct{}{}
	}
	return names
}

// pythonDiscoveryHiddenStrategies returns the effective DISCOVERY_HIDDEN_STRATEGIES
// set: its literal members unioned with M5_DEPRECATED_EDGE_STRATEGIES, mirroring
// the `frozenset({...}) | M5_DEPRECATED_EDGE_STRATEGIES` expression in registry.py.
func pythonDiscoveryHiddenStrategies(t *testing.T) map[string]struct{} {
	t.Helper()
	hidden := parsePythonFrozensetLiteral(t, "DISCOVERY_HIDDEN_STRATEGIES")
	for name := range parsePythonFrozensetLiteral(t, "M5_DEPRECATED_EDGE_STRATEGIES") {
		hidden[name] = struct{}{}
	}
	return hidden
}

// TestM5DeprecatedRosterMatchesPythonRegistry enforces the cross-language
// mirror (#1275): the Go m5DeprecatedEdgeStrategies set must stay identical
// to M5_DEPRECATED_EDGE_STRATEGIES in shared_strategies/open/registry.py.
func TestM5DeprecatedRosterMatchesPythonRegistry(t *testing.T) {
	pyNames := parsePythonFrozensetLiteral(t, "M5_DEPRECATED_EDGE_STRATEGIES")
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
	if len(pyNames) != 32 {
		t.Errorf("expected 32 quarantined names in registry.py, parsed %d", len(pyNames))
	}
}

// TestConfigGenerationSurfacesExcludeQuarantinedStrategies guards every
// config-generation surface against defaulting to or offering a strategy the
// registry hides (#1275 review): the minimal-starter default, the interactive
// wizard's pre-selection (both use starterSpotStrategyID), and the
// discovery-failure fallback lists. Checked against the full effective
// DISCOVERY_HIDDEN_STRATEGIES set (not just the M5 roster), so a name later
// hidden for any reason cannot silently re-enter a generation surface.
func TestConfigGenerationSurfacesExcludeQuarantinedStrategies(t *testing.T) {
	hidden := pythonDiscoveryHiddenStrategies(t)

	if _, bad := hidden[starterSpotStrategyID]; bad {
		t.Errorf("starterSpotStrategyID %q is discovery-hidden — init would default to a quarantined strategy", starterSpotStrategyID)
	}
	for _, list := range []struct {
		name   string
		strats []stratDef
	}{
		{"defaultSpotStrategies", defaultSpotStrategies},
		{"defaultPerpsStrategies", defaultPerpsStrategies},
		{"defaultFuturesStrategies", defaultFuturesStrategies},
	} {
		for _, s := range list.strats {
			if _, bad := hidden[s.ID]; bad {
				t.Errorf("%s offers %q, which registry.py hides from discovery", list.name, s.ID)
			}
		}
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
		{"open ref wins over args0", StrategyConfig{Type: "spot", OpenStrategy: StrategyRef{Name: "momentum_pro"}, Args: []string{"macd", "BTC", "1h"}}, false},
		{"clean strategy", StrategyConfig{Type: "spot", OpenStrategy: StrategyRef{Name: "chart_pattern"}}, false},
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
		{ID: "s-clean", Type: "spot", OpenStrategy: StrategyRef{Name: "chart_pattern"}},
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
	clean := StrategyConfig{ID: "s", Type: "spot", OpenStrategy: StrategyRef{Name: "chart_pattern"}}
	if got := edgeStatusSummaryTag(clean); got != "" {
		t.Errorf("clean strategy tag = %q, want empty", got)
	}
	// The startup summary line carries the tag (both raw and acknowledged).
	line := formatStrategySummaryLine(dep, nil, nil)
	if !strings.Contains(line, "edge=deprecated_m5(ack)") {
		t.Errorf("summary line missing edge tag: %s", line)
	}
}

// #1275 review: allow_deprecated is advisory-only and must be freely
// hot-reloadable — masked from the restart-shape comparison and applied by
// applyHotReloadConfig, alone, bundled with another reloadable field, and
// while a position is open.
func TestAllowDeprecatedHotReloadable(t *testing.T) {
	a := StrategyConfig{ID: "s", AllowDeprecated: true}
	b := StrategyConfig{ID: "s", AllowDeprecated: false}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("allow_deprecated-only change should not affect restart shape")
	}

	base := func(ack bool, capital float64) []StrategyConfig {
		return []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"macd", "ETH", "1h", "--mode=paper"},
			Capital: capital, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
			AllowDeprecated: ack,
		}}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 10},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
				},
			},
		}}
	}

	// (a) toggle alone while a position is open: accepted, value applied.
	cfg := minimalReloadConfig(base(false, 1000))
	next := minimalReloadConfig(base(true, 1000))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("allow_deprecated toggle while open should be hot-reloadable: %v", err)
	}
	if !cfg.Strategies[0].AllowDeprecated {
		t.Fatal("expected allow_deprecated applied after reload")
	}
	if !strings.Contains(strings.Join(changes, "\n"), "allow_deprecated") {
		t.Fatalf("expected an allow_deprecated change entry, got %v", changes)
	}

	// (b) bundled with another hot-reloadable field (capital): both applied.
	cfg = minimalReloadConfig(base(true, 1000))
	next = minimalReloadConfig(base(false, 2000))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("bundled allow_deprecated + capital reload should succeed: %v", err)
	}
	if cfg.Strategies[0].AllowDeprecated {
		t.Fatal("expected allow_deprecated cleared after reload")
	}
	if cfg.Strategies[0].Capital != 2000 {
		t.Fatalf("expected capital applied alongside, got %v", cfg.Strategies[0].Capital)
	}
}

// #1275 review: a hot reload that moves an open leg onto an M5-deprecated
// name (or drops an allow_deprecated ack) must re-fire the warning, while
// unchanged deprecated strategies and switches AWAY must stay silent.
func TestNewlyDeprecatedEdgeWarnings(t *testing.T) {
	dep := func(id, name string, ack bool) StrategyConfig {
		return StrategyConfig{ID: id, Type: "spot", OpenStrategy: StrategyRef{Name: name}, AllowDeprecated: ack}
	}
	cases := []struct {
		name     string
		old, new []StrategyConfig
		want     int
	}{
		{"switch onto deprecated", []StrategyConfig{dep("s1", "chart_pattern", false)}, []StrategyConfig{dep("s1", "macd", false)}, 1},
		{"unchanged deprecated no respam", []StrategyConfig{dep("s1", "macd", false)}, []StrategyConfig{dep("s1", "macd", false)}, 0},
		{"switch away no warn", []StrategyConfig{dep("s1", "macd", false)}, []StrategyConfig{dep("s1", "chart_pattern", false)}, 0},
		{"ack flipped off re-warns", []StrategyConfig{dep("s1", "macd", true)}, []StrategyConfig{dep("s1", "macd", false)}, 1},
		{"ack flipped on silences", []StrategyConfig{dep("s1", "macd", false)}, []StrategyConfig{dep("s1", "macd", true)}, 0},
		{"deprecated-to-different-deprecated re-warns", []StrategyConfig{dep("s1", "macd", false)}, []StrategyConfig{dep("s1", "rsi", false)}, 1},
	}
	for _, tc := range cases {
		if got := newlyDeprecatedEdgeWarnings(tc.old, tc.new); len(got) != tc.want {
			t.Errorf("%s: got %d warnings (%v), want %d", tc.name, len(got), got, tc.want)
		}
	}
}
