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

func TestFetchOKXPerpsMids_Basic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/market/tickers" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("instType") != "SWAP" {
			http.Error(w, "wrong instType", http.StatusBadRequest)
			return
		}
		w.Write(okxTickersResponse(map[string]string{ //nolint:errcheck
			"BTC-USDT-SWAP": "67500.50",
			"ETH-USDT-SWAP": "3200.10",
			"SOL-USDT-SWAP": "150.00",
			"DOGE-USDT":     "0.10", // spot, not SWAP — should be ignored even if coin matches
		}))
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	marks, err := fetchOKXPerpsMids([]string{"BTC", "ETH"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

func TestFetchOKXPerpsMids_CoinMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(okxTickersResponse(map[string]string{ //nolint:errcheck
			"BTC-USDT-SWAP": "67500.50",
		}))
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	marks, err := fetchOKXPerpsMids([]string{"BTC", "OBSCURE"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(marks["BTC"]-67500.50) > 1e-6 {
		t.Errorf("BTC = %v, want 67500.50", marks["BTC"])
	}
	if _, ok := marks["OBSCURE"]; ok {
		t.Errorf("OBSCURE should be absent (not in tickers response)")
	}
}

func TestFetchOKXPerpsMids_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	_, err := fetchOKXPerpsMids([]string{"BTC"})
	if err == nil {
		t.Error("expected error on HTTP 500, got nil")
	}
}

func TestFetchOKXPerpsMids_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json")) //nolint:errcheck
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	_, err := fetchOKXPerpsMids([]string{"BTC"})
	if err == nil {
		t.Error("expected error on invalid JSON, got nil")
	}
}

func TestFetchOKXPerpsMids_APIErrorCode(t *testing.T) {
	// OKX returns HTTP 200 with non-"0" code on logical errors — must surface
	// as an error, not a silent empty map (would look like "no coins listed"
	// and hide auth/rate-limit issues).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"code": "50011",
			"msg":  "Rate limit exceeded",
			"data": []any{},
		})
		w.Write(body) //nolint:errcheck
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	_, err := fetchOKXPerpsMids([]string{"BTC"})
	if err == nil {
		t.Error("expected error on OKX code!=0, got nil")
	}
}

func TestFetchOKXPerpsMids_ZeroPriceOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(okxTickersResponse(map[string]string{ //nolint:errcheck
			"BTC-USDT-SWAP": "67500.50",
			"ETH-USDT-SWAP": "0",
			"SOL-USDT-SWAP": "bad",
		}))
	}))
	defer srv.Close()

	orig := okxMainnetURL
	okxMainnetURL = srv.URL
	defer func() { okxMainnetURL = orig }()

	marks, err := fetchOKXPerpsMids([]string{"BTC", "ETH", "SOL"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(marks["BTC"]-67500.50) > 1e-6 {
		t.Errorf("BTC = %v, want 67500.50", marks["BTC"])
	}
	if _, ok := marks["ETH"]; ok {
		t.Errorf("ETH should be omitted (price=0)")
	}
	if _, ok := marks["SOL"]; ok {
		t.Errorf("SOL should be omitted (invalid price string)")
	}
}
