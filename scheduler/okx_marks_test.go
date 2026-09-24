package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okxTickersResponse(tickers map[string]string) []byte {
	type row struct {
		InstID string `json:"instId"`
		Last   string `json:"last"`
	}
	data := make([]row, 0, len(tickers))
	for instID, last := range tickers {
		data = append(data, row{InstID: instID, Last: last})
	}
	body, _ := json.Marshal(map[string]any{
		"code": "0",
		"msg":  "",
		"data": data,
	})
	return body
}

func TestFetchOKXPerpsMids(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		coins   []string
		wantErr bool
		check   func(t *testing.T, marks map[string]float64)
	}{
		{
			name: "basic returns only requested coins",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v5/market/tickers" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("instType") != "SWAP" {
					http.Error(w, "wrong instType", http.StatusBadRequest)
					return
				}
				w.Write(okxTickersResponse(map[string]string{
					"BTC-USDT-SWAP": "67500.50",
					"ETH-USDT-SWAP": "3200.10",
					"SOL-USDT-SWAP": "150.00",
					"DOGE-USDT":     "0.10",
				}))
			},
			coins: []string{"BTC", "ETH"},
			check: func(t *testing.T, marks map[string]float64) {
				if math.Abs(marks["BTC"]-67500.50) > 1e-6 {
					t.Errorf("BTC = %v, want 67500.50", marks["BTC"])
				}
				if math.Abs(marks["ETH"]-3200.10) > 1e-6 {
					t.Errorf("ETH = %v, want 3200.10", marks["ETH"])
				}
				if _, ok := marks["SOL"]; ok {
					t.Errorf("SOL should not be in returned marks (not requested)")
				}
				if len(marks) != 2 {
					t.Errorf("len(marks) = %d, want 2", len(marks))
				}
			},
		},
		{
			name: "coin missing from response is absent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write(okxTickersResponse(map[string]string{"BTC-USDT-SWAP": "67500.50"}))
			},
			coins: []string{"BTC", "OBSCURE"},
			check: func(t *testing.T, marks map[string]float64) {
				if math.Abs(marks["BTC"]-67500.50) > 1e-6 {
					t.Errorf("BTC = %v, want 67500.50", marks["BTC"])
				}
				if _, ok := marks["OBSCURE"]; ok {
					t.Errorf("OBSCURE should be absent (not in tickers response)")
				}
			},
		},
		{
			name: "http 500 errors",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "internal error", http.StatusInternalServerError)
			},
			coins:   []string{"BTC"},
			wantErr: true,
		},
		{
			name: "invalid json errors",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not-json"))
			},
			coins:   []string{"BTC"},
			wantErr: true,
		},
		{
			name: "okx error code errors",
			handler: func(w http.ResponseWriter, r *http.Request) {
				body, _ := json.Marshal(map[string]any{
					"code": "50011",
					"msg":  "Rate limit exceeded",
					"data": []any{},
				})
				w.Write(body)
			},
			coins:   []string{"BTC"},
			wantErr: true,
		},
		{
			name: "zero and unparseable prices omitted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write(okxTickersResponse(map[string]string{
					"BTC-USDT-SWAP": "67500.50",
					"ETH-USDT-SWAP": "0",
					"SOL-USDT-SWAP": "bad",
				}))
			},
			coins: []string{"BTC", "ETH", "SOL"},
			check: func(t *testing.T, marks map[string]float64) {
				if math.Abs(marks["BTC"]-67500.50) > 1e-6 {
					t.Errorf("BTC = %v, want 67500.50", marks["BTC"])
				}
				if _, ok := marks["ETH"]; ok {
					t.Errorf("ETH should be omitted (price=0)")
				}
				if _, ok := marks["SOL"]; ok {
					t.Errorf("SOL should be omitted (invalid price string)")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			orig := okxMainnetURL
			okxMainnetURL = srv.URL
			defer func() { okxMainnetURL = orig }()

			marks, err := fetchOKXPerpsMids(tc.coins)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, marks)
		})
	}
}

func TestFetchOKXPerpsMids_EmptyCoins(t *testing.T) {
	marks, err := fetchOKXPerpsMids(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(marks) != 0 {
		t.Errorf("expected empty map, got %v", marks)
	}
}
