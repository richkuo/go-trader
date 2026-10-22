package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrderBookImbalanceBookNeedMirrorsThePythonValidator(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "shared_strategies", "open", "testdata", "order_book_imbalance_param_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Params map[string]interface{} `json:"params"`
		Valid  bool                   `json:"valid"`
	}
	if err := json.Unmarshal(blob, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("only %d shared parameter cases", len(cases))
	}
	for i, c := range cases {
		sc := StrategyConfig{ID: "obi", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args:         []string{orderBookImbalanceStrategyName, "BTC", "1m", "--mode=paper"},
			OpenStrategy: StrategyRef{Name: orderBookImbalanceStrategyName, Params: c.Params}}
		req, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{sc}})
		if c.Valid != (err == nil) {
			t.Fatalf("case %d %v: valid=%v, derive error %v", i, c.Params, c.Valid, err)
		}
		if !c.Valid {
			if !strings.Contains(err.Error(), orderBookImbalanceStrategyName) {
				t.Fatalf("case %d: the refusal does not name the strategy: %v", i, err)
			}
			continue
		}
		depth := 5
		if v, ok := c.Params["depth_levels"].(float64); ok {
			depth = int(v)
		}
		need := req.Books[l2BookKeyFor("BTC")]
		if need != contractBookNeed(orderBookImbalanceWindowMs, orderBookImbalanceCapacity, depth) || req.Strategies["obi"].Book == nil {
			t.Fatalf("case %d: book need %+v", i, need)
		}
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{{ID: "obi", Type: "perps",
		Platform: "hyperliquid", Script: hyperliquidCheckScript, Args: []string{orderBookImbalanceStrategyName, "BTC", "1m", "--mode=paper"},
		OpenStrategy: StrategyRef{Name: orderBookImbalanceStrategyName}}}}); err == nil || !strings.Contains(err.Error(), "l2_book") {
		t.Fatalf("market_feed=rest must refuse the book need: %v", err)
	}
	if reason := closedBarUnsupportedOpenStrategies[orderBookImbalanceStrategyName]; !strings.HasPrefix(reason, "unsupported_cadence") {
		t.Fatalf("closed-bar refusal reason %q", reason)
	}
}
