package main

import (
	"strings"
	"testing"
)

func TestEvaluateNotionalCapHold(t *testing.T) {
	states := map[string]*StrategyState{
		"a": {ID: "a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 60000, Side: "long"},
		}},
	}
	if held, _ := evaluateNotionalCapHold(nil, states, nil); held {
		t.Fatal("nil portfolio risk must not hold")
	}
	if held, _ := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 0}, states, nil); held {
		t.Fatal("disabled notional cap must not hold")
	}
	if held, _ := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 100000}, states, nil); held {
		t.Fatal("under-cap book must not hold")
	}
	held, detail := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 50000}, states, nil)
	if !held {
		t.Fatal("over-cap book must hold")
	}
	if !strings.Contains(detail, "new opens blocked, exits continue") {
		t.Fatalf("detail=%q, want exits-continue audit wording", detail)
	}
	if !strings.Contains(detail, "60000.00") || !strings.Contains(detail, "50000.00") {
		t.Fatalf("detail=%q, want notional and cap amounts", detail)
	}
}
