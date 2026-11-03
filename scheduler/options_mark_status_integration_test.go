package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOptionMarkOperatorOutputIntegration(t *testing.T) {
	sc := ocSC("rh-status", "robinhood", "SPY")
	now := time.Now().UTC()
	ss := &StrategyState{ID: sc.ID, Platform: "robinhood", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
		"a-good":    {ID: "a-good", Underlying: "SPY", CurrentValueUSD: -120, MarkUpdatedAt: now.Add(-25 * time.Minute), MarkSource: "robinhood_quote"},
		"b-unknown": {ID: "b-unknown", Underlying: "SPY", CurrentValueUSD: -300},
		"c-pending": {ID: "c-pending", Underlying: "SPY", CurrentValueUSD: -50, MarkUpdatedAt: now.Add(-50 * time.Hour), MarkSource: "robinhood_black_scholes",
			MarkError: "settlement pending: no daily bar for SPY on expected session 2026-01-16"},
		"d-proxy": {ID: "d-proxy", Underlying: "BTC", CurrentValueUSD: 10, MarkUpdatedAt: now.Add(-3 * time.Minute), MarkSource: okxDeribitProxySource},
	}}
	lines := collectPositions(sc, ss, nil)
	want := []string{
		"rh-status OPT a-good ($-120.00) mark 25m ago (robinhood_quote)",
		"rh-status OPT b-unknown ($-300.00) mark unknown",
		"rh-status OPT c-pending ($-50.00) mark 2d2h ago (robinhood_black_scholes) | settlement pending: no daily bar for SPY on expected session 2026-01-16",
		"rh-status OPT d-proxy ($10.00) mark 3m ago (deribit_proxy)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("position lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	raw, err := json.Marshal(ss.OptionPositions)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["b-unknown"]["mark_updated_at"]; ok {
		t.Fatalf("an unknown mark must not serialize a timestamp: %s", raw)
	}
	if decoded["c-pending"]["mark_error"] == nil || decoded["a-good"]["mark_source"] != "robinhood_quote" {
		t.Fatalf("status JSON lost the mark state: %s", raw)
	}
}
