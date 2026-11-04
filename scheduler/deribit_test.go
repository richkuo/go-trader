package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		{"BTC", "call", "not-a-date", 50000, ""},
	}

	for _, tc := range cases {
		t.Run(tc.expiry, func(t *testing.T) {
			got := d.formatInstrument(tc.underlying, tc.optionType, tc.strike, tc.expiry)
			if got != tc.want {
				t.Errorf("formatInstrument(%q, %q, %g, %q) = %q, want %q",
					tc.underlying, tc.optionType, tc.strike, tc.expiry, got, tc.want)
			}
		})
	}
}

func TestDeribitMarkPricing(t *testing.T) {
	refreshAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t.Run("CollectMarkRequests", func(t *testing.T) {
		s := &StrategyState{
			Platform: "deribit",
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
					Expiry:     "2020-01-01",
					Action:     "sell",
					Quantity:   2,
				},
				"BAD-expiry": {
					ID:     "BAD-expiry",
					Expiry: "not-a-date",
				},
			},
		}

		reqs := collectOptionLegRequests(s, "deribit", refreshAt)

		if len(reqs) != 3 {
			t.Fatalf("len(reqs) = %d, want 3 (every leg yields a request)", len(reqs))
		}

		found := false
		for _, r := range reqs {
			switch r.Key {
			case "ETH-put-sell-3000-2020-01-01":
				found = true
				if r.Op != optionLegOpSettle {
					t.Errorf("ETH position op = %q, want settle", r.Op)
				}
				if want := time.Date(2020, 1, 1, 8, 0, 0, 0, time.UTC); !r.KnownCutoff.Equal(want) {
					t.Errorf("ETH cutoff = %s, want %s", r.KnownCutoff, want)
				}
			case "BTC-call-buy-60000-2027-12-31":
				if r.Op != optionLegOpMark || r.PreError != "" {
					t.Errorf("BTC position op=%q preErr=%q, want mark", r.Op, r.PreError)
				}
			case "BAD-expiry":
				if r.PreError == "" || r.Op != optionLegOpResolve {
					t.Errorf("BAD-expiry must carry a pre-error and stay unresolved: %+v", r)
				}
			}
		}
		if !found {
			t.Error("ETH expired position not found in requests")
		}
	})
	t.Run("ApplyMarkResults", func(t *testing.T) {
		s := &StrategyState{
			Cash:     1000,
			Platform: "deribit",
			OptionPositions: map[string]*OptionPosition{
				"pos1": {
					ID:              "pos1",
					Underlying:      "BTC",
					OptionType:      "call",
					Expiry:          "2026-07-01",
					Action:          "buy",
					Quantity:        1,
					CurrentValueUSD: 100,
				},
				"pos2": {
					ID:              "pos2",
					Underlying:      "BTC",
					OptionType:      "call",
					Strike:          50000,
					Expiry:          "2026-05-01",
					Action:          "buy",
					Quantity:        1,
					CurrentValueUSD: 200,
				},
			},
			Positions:    make(map[string]*Position),
			TradeHistory: []Trade{},
		}

		lm, _ := NewLogManager("")
		logger, _ := lm.GetStrategyLogger("test")
		defer logger.Close()

		reqs := collectOptionLegRequests(s, "deribit", refreshAt)
		replies := []optionLegReply{
			{Key: "pos1", Mark: &optionLegMark{PerUnitUSD: 150, Spot: 60000, Greeks: OptGreeks{Delta: 0.5}, Source: deribitMarkSource}},
			{Key: "pos2", Settlement: &optionLegSettlement{Price: 40000, Basis: optionSettlementBasisDelivery, Date: "2026-05-01", Source: deribitSettlementSource}},
		}

		applyOptionLegReplies(s, "deribit", reqs, replies, refreshAt, logger)

		if pos, ok := s.OptionPositions["pos1"]; ok {
			if pos.CurrentValueUSD != 150 {
				t.Errorf("pos1 CurrentValueUSD = %g, want 150", pos.CurrentValueUSD)
			}
			if pos.Greeks.Delta != 0.5 {
				t.Errorf("pos1 Delta = %g, want 0.5", pos.Greeks.Delta)
			}
			if want := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC).Sub(refreshAt).Hours() / 24; pos.DTE != want {
				t.Errorf("pos1 DTE = %g, want %g", pos.DTE, want)
			}
		} else {
			t.Error("pos1 should still exist")
		}

		if _, ok := s.OptionPositions["pos2"]; ok {
			t.Error("pos2 should be removed (expired OTM)")
		}
	})
	t.Run("FetchMarkPricesExpiredOTMAndITMPut", func(t *testing.T) {
		cases := []struct {
			name         string
			spot         float64
			id           string
			wantAssigned bool
			wantCash     float64
		}{
			{name: "expired OTM sold put", spot: 60000, id: "expired-otm-put", wantAssigned: false, wantCash: 100000},
			{name: "expired ITM sold put settles in cash", spot: 45000, id: "expired-itm-put", wantAssigned: false, wantCash: 100000 - 5000},
			{name: "expired sold put with no spot is held", spot: 0, id: "expired-held-put", wantCash: 100000},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				pricer := &IBKROptionPricer{script: optionMarksScript, run: ibkrFixtureMarksRunner(tc.spot)}

				lm, _ := NewLogManager("")
				logger, _ := lm.GetStrategyLogger("test")
				defer logger.Close()

				s := &StrategyState{ID: "ibkr-opt", Platform: "ibkr", Cash: 100000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
					tc.id: {ID: tc.id, Underlying: "BTC", OptionType: "put", Strike: 50000, Expiry: "2020-01-01", Action: "sell", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 100},
				}}
				reqs := collectOptionLegRequests(s, "ibkr", refreshAt)
				applyOptionLegReplies(s, "ibkr", reqs, refreshOptionLegs(pricer, reqs, refreshAt), refreshAt, logger)

				_, open := s.OptionPositions[tc.id]
				if open != (tc.spot == 0) {
					t.Fatalf("%s: leg open = %t", tc.name, open)
				}
				assigned := s.Positions["BTC"] != nil
				if assigned != tc.wantAssigned {
					t.Errorf("%s: assigned = %v, want %v", tc.name, assigned, tc.wantAssigned)
				}
				if s.Cash != tc.wantCash {
					t.Errorf("%s: cash = %g, want %g", tc.name, s.Cash, tc.wantCash)
				}
				if tc.spot == 0 && !strings.HasPrefix(s.OptionPositions[tc.id].MarkError, optionSettlementPendingPrefix) {
					t.Errorf("%s: held leg mark error = %q", tc.name, s.OptionPositions[tc.id].MarkError)
				}
			})
		}
	})
}

var _ OptionPricer = (*DeribitPricer)(nil)

var _ OptionPricer = (*IBKROptionPricer)(nil)

func ibkrFixtureMarksRunner(settlement float64) optionMarksRunner {
	return func(script string, args []string, stdin []byte) ([]byte, []byte, error) {
		var req rhOptionMarksRequest
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, nil, err
		}
		legs := make([]map[string]interface{}, 0, len(req.Legs))
		for _, leg := range req.Legs {
			out := map[string]interface{}{"id": leg.ID, "cutoff_utc": leg.CutoffUTC, "settlement_ready_utc": leg.SettlementReadyUTC}
			if settlement > 0 {
				out["status"] = "settled"
				out["settlement"] = map[string]interface{}{"price": settlement, "basis": optionSettlementBasisVWAP, "date": leg.Expiry, "source": ibkrSettlementSource}
			} else {
				out["status"] = "pending"
				out["reason"] = "no complete settlement window"
			}
			legs = append(legs, out)
		}
		doc, err := json.Marshal(map[string]interface{}{"contract": optionMarksContract, "platform": "ibkr", "premium_unit": optionPremiumUnitUnderlying, "legs": legs})
		return doc, nil, err
	}
}

var _ OptionPricer = (*RobinhoodOptionPricer)(nil)

func TestDeribitFetchTicker(t *testing.T) {
	t.Run("Mocked", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"result":{"instrument_name":"BTC-PERPETUAL","mark_price":0.05,"underlying_price":60000}}`))
		}))
		defer server.Close()

		d := &DeribitPricer{client: server.Client(), baseURL: server.URL}

		ticker, err := d.fetchTickerFull("BTC-PERPETUAL")
		if err != nil {
			t.Fatalf("fetchTickerFull failed: %v", err)
		}
		if *ticker.MarkPrice != 0.05 {
			t.Errorf("mark = %g, want 0.05", *ticker.MarkPrice)
		}
		if *ticker.UnderlyingPrice != 60000 {
			t.Errorf("spot = %g, want 60000", *ticker.UnderlyingPrice)
		}
	})
	t.Run("Error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": "not found"}`))
		}))
		defer server.Close()

		d := &DeribitPricer{client: server.Client(), baseURL: server.URL}
		_, err := d.fetchTickerFull("BTC-PERPETUAL")
		if err == nil {
			t.Error("expected error for 404 response")
		}
	})
}
