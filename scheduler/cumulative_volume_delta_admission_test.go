package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

const cvdReportRef = "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md"

func TestCumulativeVolumeDeltaRegistrationTables(t *testing.T) {
	name := cumulativeVolumeDeltaStrategyName
	if knownShortNames[name] != "cvddiv" || !isBidirectionalPerpsStrategy(name) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[name], isBidirectionalPerpsStrategy(name))
	}
	var owners []string
	for other, short := range knownShortNames {
		if short == "cvddiv" {
			owners = append(owners, other)
		}
	}
	if !reflect.DeepEqual(owners, []string{name}) {
		t.Fatalf("short name cvddiv owners = %v", owners)
	}
	if !openStrategyRegisteredFor(name, "futures") || openStrategyRegisteredFor(name, "spot") {
		t.Fatal("cumulative_volume_delta_divergence must be registered for futures only")
	}
	if ev, ok := noEdgeEvidenceFor(name); !ok || ev.Ref != cvdReportRef {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if productionRefusedStrategies[name] != cvdReportRef {
		t.Fatalf("production-refused table = %v", productionRefusedStrategies)
	}
	if _, frozen := v20StampEligibleStrategies[name]; frozen {
		t.Fatal("the candidate must not join the frozen v20 auto-acknowledgement set")
	}
	if fixedIntervalStrategyDefaults[name] != 300_000 {
		t.Fatalf("fixed interval default = %d", fixedIntervalStrategyDefaults[name])
	}
}

func cvdLoadAll(t *testing.T, sc map[string]interface{}) []error {
	t.Helper()
	path := writeNoEdgeConfig(t, CurrentConfigVersion, sc)
	_, loadErr := LoadConfig(path)
	_, probeErr := LoadConfigForProbe(path)
	_, readErr := LoadConfigReadOnly(path)
	return []error{loadErr, probeErr, readErr}
}

func TestCumulativeVolumeDeltaIsRefusedInEveryModeAndReference(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	refused := make([]string, 0, len(productionRefusedStrategies))
	for name := range productionRefusedStrategies {
		refused = append(refused, name)
	}
	sort.Strings(refused)
	fixtures := map[string]struct{ id, coin, tf string }{
		cumulativeVolumeDeltaStrategyName: {"hl-cvddiv-btc", "BTC", "5m"},
		bitcoinLeadLagStrategyName:        {"hl-btclag-eth", "ETH", "1h"},
	}
	for _, name := range refused {
		fx, ok := fixtures[name]
		if !ok {
			t.Fatalf("production-refused %s has no admission fixture", name)
		}
		t.Run(name, func(t *testing.T) { productionRefusedEveryModeAndReference(t, name, fx.id, fx.coin, fx.tf) })
	}
}

func productionRefusedEveryModeAndReference(t *testing.T, name, id, coin, tf string) {
	positional := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy(id, append([]string{name, coin, tf}, args...)...)
	}
	effective := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-btc", append([]string{"breakout", "BTC", tf}, args...)...)
		sc["open_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	overridden := func(args ...string) map[string]interface{} {
		sc := positional(args...)
		sc["open_strategy"] = map[string]interface{}{"name": "breakout"}
		return sc
	}
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-btc", append([]string{"breakout", "BTC", tf}, args...)...)
		sc["close_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	acked := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	cases := map[string]struct {
		sc   map[string]interface{}
		role string
	}{
		"positional paper":              {positional("--mode=paper"), "open"},
		"positional split paper":        {positional("--mode", "paper"), "open"},
		"positional live acknowledged":  {acked(positional("--mode=live")), "open"},
		"positional missing mode":       {positional(), "open"},
		"positional missing mode acked": {acked(positional()), "open"},
		"effective open paper":          {effective("--mode=paper"), "open"},
		"effective open live acked":     {acked(effective("--mode=live")), "open"},
		"positional under an override":  {overridden("--mode=paper"), "open"},
		"close fallback paper":          {fallback("--mode=paper"), "close"},
		"close fallback live acked":     {acked(fallback("--mode=live")), "close"},
	}
	names := make([]string, 0, len(cases))
	for k := range cases {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, label := range names {
		tc := cases[label]
		for i, err := range cvdLoadAll(t, tc.sc) {
			want := tc.role + ` strategy "` + name + `" is production_refused`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: loader %d must refuse with %q, got %v", label, i, want, err)
			}
		}
	}

	mass := noEdgeHLStrategy("hl-massrev-eth", "mass_index_reversal", "ETH", "1h", "--mode=paper")
	for i, err := range cvdLoadAll(t, mass) {
		if err != nil {
			t.Fatalf("an existing no-edge candidate must keep its explicit-paper admission (loader %d): %v", i, err)
		}
	}
	if _, _, err := buildAddStrategyEntry(name, "hyperliquid", "btc"); err == nil || !strings.Contains(err.Error(), "production_refused") {
		t.Fatalf("the add-strategy command must refuse the candidate, got %v", err)
	}
	if id, _, err := buildAddStrategyEntry("mass_index_reversal", "hyperliquid", "sol"); err != nil || id != "hl-massrev-sol" {
		t.Fatalf("the add-strategy command must keep existing candidates: %q, %v", id, err)
	}
}
