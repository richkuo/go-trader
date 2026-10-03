package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchHyperliquidMids(t *testing.T) {
	allMidsHandler := func(body map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/info" {
				http.NotFound(w, r)
				return
			}
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			if req["type"] != "allMids" {
				http.Error(w, "wrong type", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(body)
		}
	}

	cases := []struct {
		name       string
		handler    http.HandlerFunc
		noServer   bool
		coins      []string
		wantErr    bool
		wantMarks  map[string]float64
		wantAbsent []string
		wantLen    int
	}{
		{
			name: "returns only the requested coins",
			handler: allMidsHandler(map[string]string{
				"BTC": "67500.50", "ETH": "3200.10", "HYPE": "12.50", "SOL": "150.00",
			}),
			coins:      []string{"BTC", "ETH"},
			wantMarks:  map[string]float64{"BTC": 67500.50, "ETH": 3200.10},
			wantAbsent: []string{"SOL"},
			wantLen:    2,
		},
		{
			name:     "no coins requested makes no call",
			noServer: true,
			coins:    nil,
			wantLen:  0,
		},
		{
			name:       "a coin absent from allMids is absent from the result",
			handler:    allMidsHandler(map[string]string{"BTC": "67500.50"}),
			coins:      []string{"BTC", "PURR"},
			wantMarks:  map[string]float64{"BTC": 67500.50},
			wantAbsent: []string{"PURR"},
		},
		{
			name: "HTTP 500 is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "internal error", http.StatusInternalServerError)
			},
			coins:   []string{"BTC"},
			wantErr: true,
		},
		{
			name: "invalid JSON is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not-json"))
			},
			coins:   []string{"BTC"},
			wantErr: true,
		},
		{
			name: "zero and unparseable prices are omitted",
			handler: allMidsHandler(map[string]string{
				"BTC": "67500.50", "ETH": "0", "SOL": "bad",
			}),
			coins:      []string{"BTC", "ETH", "SOL"},
			wantMarks:  map[string]float64{"BTC": 67500.50},
			wantAbsent: []string{"ETH", "SOL"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.noServer {
				srv := httptest.NewServer(tc.handler)
				defer srv.Close()
				orig := hlMainnetURL
				hlMainnetURL = srv.URL
				defer func() { hlMainnetURL = orig }()
			}

			marks, err := fetchHyperliquidMids(tc.coins)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for coin, want := range tc.wantMarks {
				if math.Abs(marks[coin]-want) > 1e-6 {
					t.Errorf("%s = %v, want %v", coin, marks[coin], want)
				}
			}
			for _, coin := range tc.wantAbsent {
				if _, ok := marks[coin]; ok {
					t.Errorf("%s should be absent from the returned marks", coin)
				}
			}
			if tc.wantLen != 0 || tc.coins == nil {
				if len(marks) != tc.wantLen {
					t.Errorf("len(marks) = %d, want %d", len(marks), tc.wantLen)
				}
			}
		})
	}
}
