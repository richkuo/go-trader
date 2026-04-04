package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodePositionsJSONEmpty(t *testing.T) {
	got := EncodePositionsJSON(nil)
	if got != "[]" {
		t.Errorf("nil positions = %q, want %q", got, "[]")
	}

	got = EncodePositionsJSON(map[string]*OptionPosition{})
	if got != "[]" {
		t.Errorf("empty positions = %q, want %q", got, "[]")
	}
}

func TestEncodePositionsJSON(t *testing.T) {
	positions := map[string]*OptionPosition{
		"pos1": {
			OptionType:      "call",
			Strike:          60000,
			Expiry:          "2026-12-31",
			DTE:             30,
			Action:          "buy",
			EntryPremiumUSD: 500,
			Greeks:          OptGreeks{Delta: 0.55, Gamma: 0.01, Theta: -5, Vega: 100},
		},
	}

	got := EncodePositionsJSON(positions)

	// Should be valid JSON
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, got)
	}
	if len(parsed) != 1 {
		t.Fatalf("len = %d, want 1", len(parsed))
	}
	if parsed[0]["option_type"] != "call" {
		t.Errorf("option_type = %v, want %q", parsed[0]["option_type"], "call")
	}
	if parsed[0]["strike"].(float64) != 60000 {
		t.Errorf("strike = %v, want 60000", parsed[0]["strike"])
	}
}

func TestEncodeAllPositionsJSONEmpty(t *testing.T) {
	got := EncodeAllPositionsJSON(nil, nil)
	if got != "[]" {
		t.Errorf("nil positions = %q, want %q", got, "[]")
	}
}

func TestEncodeAllPositionsJSON(t *testing.T) {
	optPos := map[string]*OptionPosition{
		"opt1": {
			OptionType: "put",
			Strike:     55000,
			Expiry:     "2026-06-30",
			Action:     "sell",
		},
	}
	spotPos := map[string]*Position{
		"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.5, AvgCost: 50000, Side: "long"},
	}

	got := EncodeAllPositionsJSON(optPos, spotPos)

	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("len = %d, want 2", len(parsed))
	}

	// Find spot entry
	foundSpot := false
	for _, entry := range parsed {
		if entry["position_type"] == "spot" {
			foundSpot = true
			if entry["symbol"] != "BTC/USDT" {
				t.Errorf("symbol = %v, want %q", entry["symbol"], "BTC/USDT")
			}
		}
	}
	if !foundSpot {
		t.Error("should contain spot position entry")
	}
}

func TestExecuteOptionsSignalNoSignal(t *testing.T) {
	s := &StrategyState{
		Cash:            10000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	result := &OptionsResult{Signal: 0}
	trades, err := ExecuteOptionsSignal(s, result, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 for no signal", trades)
	}
}

func TestExecuteOptionsSignalBuy(t *testing.T) {
	s := &StrategyState{
		ID:              "test",
		Cash:            10000,
		Platform:        "deribit",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	result := &OptionsResult{
		Signal:     1,
		Underlying: "BTC",
		SpotPrice:  60000,
		Actions: []OptionsAction{
			{
				Action:     "buy",
				OptionType: "call",
				Strike:     65000,
				Expiry:     "2026-12-31",
				DTE:        30,
				PremiumUSD: 500,
				Quantity:   1,
			},
		},
	}

	trades, err := ExecuteOptionsSignal(s, result, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if s.Cash >= 10000 {
		t.Error("cash should decrease after buying option")
	}
	if len(s.OptionPositions) != 1 {
		t.Errorf("should have 1 option position, got %d", len(s.OptionPositions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("should have 1 trade, got %d", len(s.TradeHistory))
	}
}

func TestExecuteOptionsSignalSell(t *testing.T) {
	s := &StrategyState{
		ID:              "test",
		Cash:            10000,
		Platform:        "deribit",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	result := &OptionsResult{
		Signal:     -1,
		Underlying: "BTC",
		SpotPrice:  60000,
		Actions: []OptionsAction{
			{
				Action:     "sell",
				OptionType: "put",
				Strike:     55000,
				Expiry:     "2026-12-31",
				DTE:        30,
				PremiumUSD: 300,
				Quantity:   1,
			},
		},
	}

	trades, err := ExecuteOptionsSignal(s, result, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if s.Cash <= 10000 {
		t.Error("cash should increase after selling option (premium received)")
	}
}

func TestCheckThetaHarvestDisabled(t *testing.T) {
	s := &StrategyState{
		OptionPositions: map[string]*OptionPosition{
			"pos1": {Action: "sell", EntryPremiumUSD: 100, CurrentValueUSD: -20},
		},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := CheckThetaHarvest(s, nil, logger)
	if trades != 0 {
		t.Error("should not harvest when config is nil")
	}

	trades, _ = CheckThetaHarvest(s, &ThetaHarvestConfig{Enabled: false}, logger)
	if trades != 0 {
		t.Error("should not harvest when disabled")
	}
}

func TestCheckThetaHarvestProfitTarget(t *testing.T) {
	s := &StrategyState{
		ID:   "test",
		Cash: 5000,
		OptionPositions: map[string]*OptionPosition{
			"pos1": {
				ID:              "pos1",
				Action:          "sell",
				EntryPremiumUSD: 100,
				CurrentValueUSD: -30, // cost to buy back = 30, profit = 70%
				Quantity:        1,
			},
		},
		Positions:    make(map[string]*Position),
		TradeHistory: []Trade{},
		RiskState:    RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	cfg := &ThetaHarvestConfig{
		Enabled:         true,
		ProfitTargetPct: 60,
		StopLossPct:     200,
		MinDTEClose:     3,
	}

	trades, details := CheckThetaHarvest(s, cfg, logger)
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if !strings.Contains(details[0], "Theta harvest") {
		t.Errorf("detail should mention theta harvest: %q", details[0])
	}

	// Position should be removed
	if _, ok := s.OptionPositions["pos1"]; ok {
		t.Error("position should be removed after harvest")
	}
}

func TestCheckThetaHarvestDTEExit(t *testing.T) {
	s := &StrategyState{
		ID:   "test",
		Cash: 5000,
		OptionPositions: map[string]*OptionPosition{
			"pos1": {
				ID:              "pos1",
				Action:          "sell",
				EntryPremiumUSD: 100,
				CurrentValueUSD: -90, // barely any profit
				DTE:             2,   // below min
				Quantity:        1,
			},
		},
		Positions:    make(map[string]*Position),
		TradeHistory: []Trade{},
		RiskState:    RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	cfg := &ThetaHarvestConfig{
		Enabled:         true,
		ProfitTargetPct: 60,
		StopLossPct:     200,
		MinDTEClose:     3,
	}

	trades, details := CheckThetaHarvest(s, cfg, logger)
	if trades != 1 {
		t.Errorf("trades = %d, want 1 (DTE exit)", trades)
	}
	if len(details) > 0 && !strings.Contains(details[0], "DTE exit") {
		t.Errorf("detail should mention DTE: %q", details[0])
	}
}

func TestCheckThetaHarvestSkipsBuyPositions(t *testing.T) {
	s := &StrategyState{
		OptionPositions: map[string]*OptionPosition{
			"buy-pos": {
				Action:          "buy",
				EntryPremiumUSD: 100,
				CurrentValueUSD: 150,
				DTE:             2,
			},
		},
		TradeHistory: []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	cfg := &ThetaHarvestConfig{Enabled: true, ProfitTargetPct: 60, MinDTEClose: 3}
	trades, _ := CheckThetaHarvest(s, cfg, logger)
	if trades != 0 {
		t.Error("should not harvest buy positions")
	}
}
