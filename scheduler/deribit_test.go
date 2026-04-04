package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFormatInstrument(t *testing.T) {
	d := NewDeribitPricer()

	cases := []struct {
		underlying, optionType, expiry string
		strike                         float64
		want                           string
	}{
		{"BTC", "call", "2026-03-13", 75000, "BTC-13MAR26-75000-C"},
		{"ETH", "put", "2026-06-25", 3000, "ETH-25JUN26-3000-P"},
		{"BTC", "call", "2027-12-31", 100000, "BTC-31DEC27-100000-C"},
		{"btc", "PUT", "2026-01-15", 50000, "BTC-15JAN26-50000-P"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := d.formatInstrument(tc.underlying, tc.optionType, tc.strike, tc.expiry)
			if got != tc.want {
				t.Errorf("formatInstrument(%q, %q, %g, %q) = %q, want %q",
					tc.underlying, tc.optionType, tc.strike, tc.expiry, got, tc.want)
			}
		})
	}
}

func TestFormatInstrumentInvalidExpiry(t *testing.T) {
	d := NewDeribitPricer()
	got := d.formatInstrument("BTC", "call", 50000, "not-a-date")
	if got != "" {
		t.Errorf("expected empty string for invalid expiry, got %q", got)
	}
}

func TestDeribitPricerName(t *testing.T) {
	d := NewDeribitPricer()
	if d.Name() != "deribit" {
		t.Errorf("Name() = %q, want %q", d.Name(), "deribit")
	}
}


func TestCollectMarkRequests(t *testing.T) {
	s := &StrategyState{
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-buy-60000-2027-12-31": {
				ID:         "BTC-call-buy-60000-2027-12-31",
				Underlying: "BTC",
				OptionType: "call",
				Strike:     60000,
				Expiry:     "2027-12-31",
				Action:     "buy",
				Quantity:   1,
			},
			"ETH-put-sell-3000-2020-01-01": {
				ID:         "ETH-put-sell-3000-2020-01-01",
				Underlying: "ETH",
				OptionType: "put",
				Strike:     3000,
				Expiry:     "2020-01-01", // expired
				Action:     "sell",
				Quantity:   2,
			},
			"BAD-expiry": {
				ID:     "BAD-expiry",
				Expiry: "not-a-date", // invalid, should be skipped
			},
		},
	}

	reqs := collectMarkRequests(s)

	// Should have 2 valid requests (BAD-expiry skipped)
	if len(reqs) != 2 {
		t.Fatalf("len(reqs) = %d, want 2", len(reqs))
	}

	// Find the expired one
	found := false
	for _, r := range reqs {
		if r.ID == "ETH-put-sell-3000-2020-01-01" {
			found = true
			if !r.Expired {
				t.Error("ETH position should be marked as expired")
			}
			if r.DTE >= 0 {
				t.Errorf("DTE should be negative for expired, got %g", r.DTE)
			}
		}
		if r.ID == "BTC-call-buy-60000-2027-12-31" {
			if r.Expired {
				t.Error("BTC position should not be expired")
			}
		}
	}
	if !found {
		t.Error("ETH expired position not found in requests")
	}
}

func TestApplyMarkResults(t *testing.T) {
	s := &StrategyState{
		Cash: 1000,
		OptionPositions: map[string]*OptionPosition{
			"pos1": {
				ID:              "pos1",
				Action:          "buy",
				CurrentValueUSD: 100,
			},
			"pos2": {
				ID:              "pos2",
				Action:          "buy",
				CurrentValueUSD: 200,
			},
		},
		Positions:    make(map[string]*Position),
		TradeHistory: []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	results := []markResult{
		{ID: "pos1", DTE: 30, CurrentValueUSD: 150, Fetched: true, Greeks: OptGreeks{Delta: 0.5}},
		{ID: "pos2", DTE: 0, CurrentValueUSD: 0, Expired: true},
	}

	applyMarkResults(s, results, logger)

	// pos1 should be updated
	if pos, ok := s.OptionPositions["pos1"]; ok {
		if pos.CurrentValueUSD != 150 {
			t.Errorf("pos1 CurrentValueUSD = %g, want 150", pos.CurrentValueUSD)
		}
		if pos.Greeks.Delta != 0.5 {
			t.Errorf("pos1 Delta = %g, want 0.5", pos.Greeks.Delta)
		}
		if pos.DTE != 30 {
			t.Errorf("pos1 DTE = %g, want 30", pos.DTE)
		}
	} else {
		t.Error("pos1 should still exist")
	}

	// pos2 should be removed (expired, no assignment)
	if _, ok := s.OptionPositions["pos2"]; ok {
		t.Error("pos2 should be removed (expired OTM)")
	}
}

func TestApplyAssignmentPut(t *testing.T) {
	s := &StrategyState{
		ID:              "test",
		Cash:            10000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	r := markResult{
		Assigned:         true,
		AssignUnderlying: "BTC",
		AssignOptionType: "put",
		AssignStrike:     50000,
		AssignSpotPrice:  48000,
		AssignQuantity:   0.1,
	}

	applyAssignment(s, r, logger)

	// Cash should decrease by strike * quantity
	expectedCash := 10000 - 50000*0.1
	if s.Cash != expectedCash {
		t.Errorf("Cash = %g, want %g", s.Cash, expectedCash)
	}

	// Should have a long position
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("should have BTC long position")
	}
	if pos.Side != "long" {
		t.Errorf("Side = %q, want %q", pos.Side, "long")
	}
	if pos.Quantity != 0.1 {
		t.Errorf("Quantity = %g, want 0.1", pos.Quantity)
	}
	if pos.AvgCost != 50000 {
		t.Errorf("AvgCost = %g, want 50000", pos.AvgCost)
	}

	// Should have trade history
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "assignment" {
		t.Error("trade type should be 'assignment'")
	}
}

func TestApplyAssignmentCall(t *testing.T) {
	s := &StrategyState{
		ID:   "test",
		Cash: 5000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.2, AvgCost: 45000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	r := markResult{
		Assigned:         true,
		AssignUnderlying: "BTC",
		AssignOptionType: "call",
		AssignStrike:     55000,
		AssignSpotPrice:  56000,
		AssignQuantity:   0.1,
	}

	applyAssignment(s, r, logger)

	// Cash should increase by strike * quantity
	expectedCash := 5000 + 55000*0.1
	if s.Cash != expectedCash {
		t.Errorf("Cash = %g, want %g", s.Cash, expectedCash)
	}

	// Position quantity should decrease
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("BTC position should still exist")
	}
	if pos.Quantity != 0.1 {
		t.Errorf("Quantity = %g, want 0.1 (0.2 - 0.1)", pos.Quantity)
	}
}

func TestFetchMarkPricesExpiredOTM(t *testing.T) {
	// Mock pricer that returns spot price
	prices := map[string]float64{"BTC/USDT": 60000}
	pricer := NewIBKRPricer(prices)

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	requests := []markRequest{
		{
			ID:         "expired-otm-put",
			Underlying: "BTC",
			OptionType: "put",
			Strike:     50000, // OTM: strike < spot
			Action:     "sell",
			Quantity:   1,
			DTE:        -1,
			Expired:    true,
		},
	}

	results := fetchMarkPrices(requests, pricer, logger)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	r := results[0]
	if !r.Expired {
		t.Error("should be expired")
	}
	if r.Assigned {
		t.Error("OTM put should not be assigned")
	}
	if r.CurrentValueUSD != 0 {
		t.Errorf("OTM expired value should be 0, got %g", r.CurrentValueUSD)
	}
}

func TestFetchMarkPricesExpiredITMPut(t *testing.T) {
	prices := map[string]float64{"BTC/USDT": 45000}
	pricer := NewIBKRPricer(prices)

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	requests := []markRequest{
		{
			ID:         "expired-itm-put",
			Underlying: "BTC",
			OptionType: "put",
			Strike:     50000, // ITM: strike > spot for put
			Action:     "sell",
			Quantity:   1,
			DTE:        -1,
			Expired:    true,
		},
	}

	results := fetchMarkPrices(requests, pricer, logger)
	r := results[0]

	if !r.Assigned {
		t.Error("ITM sold put should be assigned")
	}
	if r.AssignOptionType != "put" {
		t.Error("should be put assignment")
	}
	// Intrinsic = (strike - spot) * qty = 5000
	expectedValue := -5000.0 // negative for sold option
	if r.CurrentValueUSD != expectedValue {
		t.Errorf("CurrentValueUSD = %g, want %g", r.CurrentValueUSD, expectedValue)
	}
}

// Verify DeribitPricer implements OptionPricer at compile time.
var _ OptionPricer = (*DeribitPricer)(nil)

func TestDeribitFetchTickerMocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DeribitTickerResponse{}
		resp.Result.MarkPrice = 0.05
		resp.Result.UnderlyingPrice = 60000
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	d := &DeribitPricer{client: server.Client()}

	// fetchTicker directly (need to construct URL manually for test)
	mark, spot, err := d.fetchTicker(server.URL)
	if err != nil {
		t.Fatalf("fetchTicker failed: %v", err)
	}
	if mark != 0.05 {
		t.Errorf("mark = %g, want 0.05", mark)
	}
	if spot != 60000 {
		t.Errorf("spot = %g, want 60000", spot)
	}
}

func TestDeribitFetchTickerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "not found"}`))
	}))
	defer server.Close()

	d := &DeribitPricer{client: server.Client()}
	_, _, err := d.fetchTicker(server.URL)
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestDeribitGetOptionPriceFullInvalidInstrument(t *testing.T) {
	d := NewDeribitPricer()
	_, _, _, err := d.GetOptionPriceFull("BTC", "call", 60000, "invalid-date")
	if err == nil {
		t.Error("expected error for invalid expiry")
	}
}

