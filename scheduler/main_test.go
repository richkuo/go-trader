package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestExecuteHyperliquidResult_StampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-test-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "hl-test-btc", Type: "perps", Platform: "hyperliquid"}
	result := &HyperliquidResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &HyperliquidExecuteResult{
		Execution: &HyperliquidExecution{
			Action: "buy", Symbol: "BTC", Size: 0.015,
			Fill: &HyperliquidFill{AvgPx: 50000.5, TotalSz: 0.015, OID: 1234567890, Fee: 1.75},
		},
		Platform: "hyperliquid",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeHyperliquidResult(sc, s, result, execResult, "BUY", 50000, nil, nil, HurstGateDecision{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}

	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "1234567890" {
		t.Errorf("ExchangeOrderID = %q, want %q", tr.ExchangeOrderID, "1234567890")
	}
	if tr.ExchangeFee != 1.75 {
		t.Errorf("ExchangeFee = %g, want 1.75", tr.ExchangeFee)
	}
}

func TestExecuteHyperliquidResult_PaperModeNoExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-paper-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "hl-paper-btc", Type: "perps", Platform: "hyperliquid"}
	result := &HyperliquidResult{Signal: 1, Symbol: "BTC", Price: 50000}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeHyperliquidResult(sc, s, result, nil, "BUY", 50000, nil, nil, HurstGateDecision{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}

	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("ExchangeOrderID should be empty in paper mode, got %q", tr.ExchangeOrderID)
	}
	wantFee := CalculatePlatformSpotFee("hyperliquid", tr.Value)
	if math.Abs(tr.ExchangeFee-wantFee) > 1e-9 || tr.FeeSource != FeeSourceModeled {
		t.Errorf("paper open fee = %g (src %q), want modeled %g", tr.ExchangeFee, tr.FeeSource, wantFee)
	}
}

func TestExecuteHyperliquidResult_PaperTierFillBooksAtTierPrice(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		side      string
		blob      string
		mid       float64
		exec      *HyperliquidExecuteResult
		wantPrice float64
		wantTol   float64
	}{
		{"paper long books the tier price below the mid", []string{"sma", "BTC", "1h", "--mode=paper"}, "long",
			`{"signal":-1,"symbol":"BTC","close_fraction":0.5,"close_tier_fill_price":104}`, 106, nil, 104, 1e-9},
		{"paper short books the tier price above the mid", []string{"sma", "BTC", "1h", "--mode=paper"}, "short",
			`{"signal":1,"symbol":"BTC","close_fraction":0.5,"close_tier_fill_price":96}`, 94, nil, 96, 1e-9},
		{"paper close without a tier price books the mid", []string{"sma", "BTC", "1h", "--mode=paper"}, "long",
			`{"signal":-1,"symbol":"BTC","close_fraction":0.5}`, 106, nil, 106, 106 * SlippagePct},
		{"live fill ignores the tier price", []string{"sma", "BTC", "1h", "--mode=live"}, "long",
			`{"signal":-1,"symbol":"BTC","close_fraction":0.5,"close_tier_fill_price":104}`, 106,
			&HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "sell", Symbol: "BTC", Size: 0.5,
				Fill: &HyperliquidFill{AvgPx: 105.5, TotalSz: 0.5, OID: 7}}, Platform: "hyperliquid"}, 105.5, 1e-9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StrategyState{
				ID: "hl-tp", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000,
				Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Side: tc.side, Quantity: 1, InitialQuantity: 1, AvgCost: 100, EntryATR: 2}},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
				RiskState:       RiskState{PeakValue: 1000},
			}
			sc := StrategyConfig{ID: "hl-tp", Type: "perps", Platform: "hyperliquid", Args: tc.args}
			var result HyperliquidResult
			if err := json.Unmarshal([]byte(tc.blob), &result); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			lm, _ := NewLogManager("")
			logger, _ := lm.GetStrategyLogger("test")
			defer logger.Close()

			trades, _ := executeHyperliquidResult(sc, s, &result, tc.exec, "CLOSE", tc.mid, nil, nil, HurstGateDecision{}, logger)
			if trades != 1 || len(s.TradeHistory) != 1 {
				t.Fatalf("trades = %d history = %d, want one close", trades, len(s.TradeHistory))
			}
			if got := s.TradeHistory[0].Price; math.Abs(got-tc.wantPrice) > tc.wantTol {
				t.Fatalf("close booked at %g, want %g", got, tc.wantPrice)
			}
			if pos := s.Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.5) > 1e-9 {
				t.Fatalf("remaining position = %+v, want 0.5 left", pos)
			}
		})
	}
}

func TestExecuteOKXResult_SpotLiveFillCashOverBudget(t *testing.T) {
	s := &StrategyState{
		ID:              "okx-spot-btc",
		Type:            "spot",
		Platform:        "okx",
		Cash:            0.40,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "okx-spot-btc", Type: "spot", Platform: "okx"}
	result := &OKXResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &OKXExecuteResult{
		Execution: &OKXExecution{
			Action: "buy", Symbol: "BTC", Size: 0.01,
			Fill: &OKXFill{AvgPx: 50000, TotalSz: 0.01, OID: "okx-over-oid", Fee: 0.50},
		},
		Platform: "okx",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _, cashAlert := executeOKXResult(sc, s, nil, result, execResult, "BUY", 50000, nil, nil, HurstGateDecision{}, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 — sub-dollar cash live fill must book (#1394)", trades)
	}
	if cashAlert == "" || !strings.Contains(cashAlert, "CRITICAL: LIVE SPOT CASH OVER BUDGET") {
		t.Fatalf("cashAlert = %q, want CRITICAL over-budget alert", cashAlert)
	}
	if !s.CashReconcileRequired {
		t.Fatal("CashReconcileRequired must latch on OKX over-budget book")
	}
}
