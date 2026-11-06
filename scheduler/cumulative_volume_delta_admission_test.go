package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const cvdReportRef = "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md"

func noEdgeCaseCumulativeVolumeDeltaRegistrationTables(t *testing.T) {
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
	for _, name := range refused {
		t.Run(name, func(t *testing.T) { productionRefusedEveryModeAndReference(t, name) })
	}
}

var researchRefusedModes = map[string][]string{
	"explicit live":       {"--mode=live"},
	"split live":          {"--mode", "live"},
	"missing mode":        nil,
	"invalid mode":        {"--mode=bogus"},
	"dangling mode":       {"--mode"},
	"duplicate paper":     {"--mode=paper", "--mode=paper"},
	"paper and live":      {"--mode=paper", "--mode=live"},
	"split duplicate":     {"--mode", "paper", "--mode=paper"},
	"empty mode value":    {"--mode="},
	"uppercase paper":     {"--mode=PAPER"},
	"live then paper":     {"--mode=live", "--mode", "paper"},
	"dangling after live": {"--mode=live", "--mode"},
}

var researchPaperModes = map[string][]string{
	"explicit paper": {"--mode=paper"},
	"split paper":    {"--mode", "paper"},
}

func researchQuoteOwnerFixture(args ...string) map[string]interface{} {
	return map[string]interface{}{
		"id": "hl-invmm-btc", "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args":           append([]string{inventoryMarketMakingStrategyName, "BTC", "1m"}, args...),
		"open_strategy":  map[string]interface{}{"name": inventoryMarketMakingStrategyName},
		"close_strategy": map[string]interface{}{"name": inventoryQuoteExitCloseName},
		"capital":        1000.0, "leverage": 1.0, "margin_mode": "isolated", "stop_loss_pct": 2.0, "interval_seconds": 5,
		"quote": map[string]interface{}{"coin": "BTC", "max_inventory_usd": 1000.0, "max_order_qty": 0.01, "request_expiry_ms": 2000,
			"fee_reserve_rate": 0.00045, "margin_price_buffer_pct": 1.0},
	}
}

func researchFixtureAsset(name string) string {
	if name == bitcoinLeadLagStrategyName {
		return "ETH"
	}
	return "BTC"
}

func researchScalarFixture(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	var params map[string]interface{}
	if len(addStrategyResearchCandidates[name].RequiredParams) > 0 {
		params = map[string]interface{}{"calendar_path": "/var/lib/go-trader/macro/calendar.json", "calendar_sha256": strings.Repeat("0", 64),
			"quote_path": "/var/lib/go-trader/macro/quotes.json", "quote_sha256": strings.Repeat("2", 64)}
	}
	_, raw, err := buildAddStrategyEntryWithParams(name, "hyperliquid", researchFixtureAsset(name), params)
	if err != nil {
		t.Fatal(err)
	}
	var sc map[string]interface{}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func researchFixtureArgs(sc map[string]interface{}, positional string, mode []string) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range sc {
		out[k] = v
	}
	args := sc["args"].([]interface{})
	out["args"] = append([]string{positional, args[1].(string), args[2].(string)}, mode...)
	return out
}

func researchLoadAll(t *testing.T, root map[string]interface{}, sc map[string]interface{}) []error {
	t.Helper()
	path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, root, sc)
	_, loadErr := LoadConfig(path)
	_, probeErr := LoadConfigForProbe(path)
	_, readErr := LoadConfigReadOnly(path)
	return []error{loadErr, probeErr, readErr}
}

func productionRefusedEveryModeAndReference(t *testing.T, name string) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	acks := map[string]interface{}{"absent": nil, "true": true, "false": false}
	withAck := func(sc map[string]interface{}, ack interface{}) map[string]interface{} {
		if ack != nil {
			sc["allow_no_edge"] = ack
		}
		return sc
	}
	type role struct {
		build    func(mode []string) map[string]interface{}
		admitted bool
	}
	roles := map[string]role{}
	if _, quote := quoteCapabilityStrategies[name]; quote {
		roles["quote owner"] = role{build: func(mode []string) map[string]interface{} { return researchQuoteOwnerFixture(mode...) }, admitted: true}
		roles["quote capability on a non-owner"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchQuoteOwnerFixture(mode...)
			delete(sc, "quote")
			return sc
		}}
		roles["quote exit close on a scalar strategy"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchFixtureArgs(researchScalarFixture(t, cumulativeVolumeDeltaStrategyName), cumulativeVolumeDeltaStrategyName, mode)
			sc["close_strategy"] = map[string]interface{}{"name": inventoryQuoteExitCloseName}
			return sc
		}}
	} else {
		base := researchScalarFixture(t, name)
		roles["positional open"] = role{build: func(mode []string) map[string]interface{} { return researchFixtureArgs(base, name, mode) }, admitted: true}
		roles["effective open"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchFixtureArgs(base, "breakout", mode)
			if _, has := sc["open_strategy"]; !has {
				sc["open_strategy"] = map[string]interface{}{"name": name}
			}
			return sc
		}, admitted: true}
		roles["positional under an override"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchFixtureArgs(base, name, mode)
			sc["open_strategy"] = map[string]interface{}{"name": "breakout"}
			return sc
		}, admitted: true}
		roles["same-name fallback close"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchFixtureArgs(base, name, mode)
			sc["close_strategy"] = map[string]interface{}{"name": name}
			return sc
		}, admitted: true}
		roles["fallback close under another open"] = role{build: func(mode []string) map[string]interface{} {
			sc := researchFixtureArgs(base, "breakout", mode)
			sc["open_strategy"] = map[string]interface{}{"name": "breakout"}
			sc["close_strategy"] = map[string]interface{}{"name": name}
			return sc
		}}
	}
	roleNames := make([]string, 0, len(roles))
	for k := range roles {
		roleNames = append(roleNames, k)
	}
	sort.Strings(roleNames)
	ackNames := []string{"absent", "false", "true"}
	for _, rn := range roleNames {
		r := roles[rn]
		for modeName, mode := range researchPaperModes {
			for _, an := range ackNames {
				for i, err := range researchLoadAll(t, websocket, withAck(r.build(mode), acks[an])) {
					if r.admitted && err != nil {
						t.Fatalf("%s %s ack %s: loader %d must admit the explicit-paper reference: %v", rn, modeName, an, i, err)
					}
					if !r.admitted && err == nil {
						t.Fatalf("%s %s ack %s: loader %d must refuse the reference in every mode", rn, modeName, an, i)
					}
				}
			}
		}
		for modeName, mode := range researchRefusedModes {
			for _, an := range ackNames {
				for i, err := range researchLoadAll(t, websocket, withAck(r.build(mode), acks[an])) {
					if err == nil || !strings.Contains(err.Error(), "production_refused") && !strings.Contains(err.Error(), "quote-owned") {
						t.Fatalf("%s %s ack %s: loader %d must refuse every non-paper mode, got %v", rn, modeName, an, i, err)
					}
				}
			}
		}
		if r.admitted {
			for i, err := range researchLoadAll(t, map[string]interface{}{"market_feed": "rest"}, r.build([]string{"--mode=paper"})) {
				if err == nil || !strings.Contains(err.Error(), `"market_feed"`) && !strings.Contains(err.Error(), "market_feed=") {
					t.Fatalf("%s: loader %d must refuse the REST feed and name the setting, got %v", rn, i, err)
				}
			}
		}
	}
	if _, quote := quoteCapabilityStrategies[name]; quote {
		if _, _, err := buildAddStrategyEntry(name, "hyperliquid", "btc"); err == nil || !strings.Contains(err.Error(), "quote-owned") {
			t.Fatalf("the scalar add route must refuse the quote capability, got %v", err)
		}
	}
	mass := noEdgeHLStrategy("hl-massrev-eth", "mass_index_reversal", "ETH", "1h", "--mode=paper")
	for i, err := range researchLoadAll(t, nil, mass) {
		if err != nil {
			t.Fatalf("an existing no-edge candidate must keep its explicit-paper admission (loader %d): %v", i, err)
		}
	}
	missing := noEdgeHLStrategy("hl-massrev-eth", "mass_index_reversal", "ETH", "1h")
	for i, err := range researchLoadAll(t, nil, missing) {
		if err == nil || !strings.Contains(err.Error(), "allow_no_edge") {
			t.Fatalf("an existing no-edge candidate keeps its missing-mode refusal (loader %d): %v", i, err)
		}
	}
	if id, _, err := buildAddStrategyEntry("mass_index_reversal", "hyperliquid", "sol"); err != nil || id != "hl-massrev-sol" {
		t.Fatalf("the add-strategy command must keep existing candidates: %q, %v", id, err)
	}
}
