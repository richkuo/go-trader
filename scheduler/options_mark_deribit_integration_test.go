package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type deribitFake struct {
	mu       sync.Mutex
	rows     []map[string]interface{}
	status   int
	tickers  map[string]string
	requests []string
}

func (f *deribitFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	if f.status != 0 {
		w.WriteHeader(f.status)
		w.Write([]byte(`{"error":{"message":"fake failure"}}`))
		return
	}
	switch r.URL.Path {
	case "/public/get_delivery_prices":
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		end := offset + count
		if end > len(f.rows) {
			end = len(f.rows)
		}
		page := []map[string]interface{}{}
		if offset < len(f.rows) {
			page = f.rows[offset:end]
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "result": map[string]interface{}{"data": page, "records_total": len(f.rows)}})
	case "/public/ticker":
		body, ok := f.tickers[r.URL.Query().Get("instrument_name")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid params"}}`))
			return
		}
		w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func deribitDescendingRows(newest time.Time, n int, skip string, price func(day string) interface{}) []map[string]interface{} {
	var rows []map[string]interface{}
	for i := 0; i < n; i++ {
		day := newest.AddDate(0, 0, -i).Format("2006-01-02")
		if day == skip {
			continue
		}
		rows = append(rows, map[string]interface{}{"date": day, "delivery_price": price(day)})
	}
	return rows
}

func TestDeribitDeliveryAndMarkSourceIntegration(t *testing.T) {
	newest := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	target := "2026-07-31"
	run := func(t *testing.T, f *deribitFake, platform string, at time.Time, reqs ...optionLegRequest) []optionLegReply {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(f.handler))
		t.Cleanup(srv.Close)
		d := &DeribitPricer{client: srv.Client(), baseURL: srv.URL}
		pricer := optionPricerFor(platform, optionPricers{Deribit: d})
		return pricer.Refresh(reqs, at)
	}
	settleReq := optionLegRequest{Key: "leg", Underlying: "BTC", OptionType: "put", Strike: 60000, Expiry: target, Action: "sell", Quantity: 1, Multiplier: 1}
	afterBoundary := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	t.Run("exact_date_on_a_later_page", func(t *testing.T) {
		f := &deribitFake{rows: deribitDescendingRows(newest.AddDate(0, 2, 0), 300, "", func(day string) interface{} {
			if day == target {
				return 61234.5
			}
			return 70000.0
		})}
		replies := run(t, f, "deribit", afterBoundary, settleReq)
		if len(replies) != 1 || replies[0].Settlement == nil || replies[0].Settlement.Price != 61234.5 || replies[0].Settlement.Source != deribitSettlementSource || replies[0].Settlement.Date != target {
			t.Fatalf("replies = %+v", replies)
		}
		if !strings.Contains(strings.Join(f.requests, " "), "offset=100&count=100") {
			t.Fatalf("the lookup must page past the first 100 rows: %v", f.requests)
		}
		if want := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC); !replies[0].CutoffAt.Equal(want) || !replies[0].ReadyAt.Equal(want.Add(time.Hour)) {
			t.Fatalf("cutoff=%s ready=%s, want 08:00Z and 09:00Z", replies[0].CutoffAt, replies[0].ReadyAt)
		}
	})
	t.Run("missing_date_stays_pending_and_never_takes_the_newest_row", func(t *testing.T) {
		f := &deribitFake{rows: deribitDescendingRows(newest, 300, target, func(string) interface{} { return 70000.0 })}
		replies := run(t, f, "deribit", afterBoundary, settleReq)
		if replies[0].Settlement != nil || !strings.Contains(replies[0].Pending, "not published yet") {
			t.Fatalf("reply = %+v", replies[0])
		}
	})
	t.Run("delayed_publication_is_pending", func(t *testing.T) {
		f := &deribitFake{rows: deribitDescendingRows(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), 10, "", func(string) interface{} { return 70000.0 })}
		replies := run(t, f, "deribit", time.Date(2026, 7, 31, 8, 30, 0, 0, time.UTC), settleReq)
		if replies[0].Pending == "" || replies[0].Settlement != nil {
			t.Fatalf("reply = %+v", replies[0])
		}
	})
	for name, price := range map[string]interface{}{"zero_price": 0.0, "negative_price": -5.0, "missing_price": nil} {
		t.Run(name+"_is_an_error", func(t *testing.T) {
			f := &deribitFake{rows: deribitDescendingRows(newest, 120, "", func(day string) interface{} {
				if day == target {
					return price
				}
				return 70000.0
			})}
			replies := run(t, f, "deribit", afterBoundary, settleReq)
			if replies[0].Err == "" || replies[0].Settlement != nil {
				t.Fatalf("reply = %+v", replies[0])
			}
		})
	}
	t.Run("unsorted_page_is_an_error", func(t *testing.T) {
		rows := deribitDescendingRows(newest, 120, "", func(string) interface{} { return 70000.0 })
		rows[3], rows[4] = rows[4], rows[3]
		replies := run(t, &deribitFake{rows: rows}, "deribit", afterBoundary, settleReq)
		if !strings.Contains(replies[0].Err, "not strictly descending") {
			t.Fatalf("reply = %+v", replies[0])
		}
	})
	t.Run("http_error_is_an_error", func(t *testing.T) {
		replies := run(t, &deribitFake{status: http.StatusInternalServerError}, "deribit", afterBoundary, settleReq)
		if !strings.Contains(replies[0].Err, "status 500") {
			t.Fatalf("reply = %+v", replies[0])
		}
	})
	t.Run("marks_use_the_exact_instrument_only", func(t *testing.T) {
		markAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		f := &deribitFake{tickers: map[string]string{
			"BTC-31JUL26-60000-P": `{"result":{"instrument_name":"BTC-31JUL26-60000-P","mark_price":0.02,"underlying_price":65000,"greeks":{"delta":-0.2,"gamma":0.00001,"theta":-20,"vega":50}}}`,
			"BTC-31JUL26-70000-C": `{"result":{"instrument_name":"BTC-31JUL26-70000-C","underlying_price":65000,"greeks":{"delta":0.2,"gamma":0.00001,"theta":-20,"vega":50}}}`,
		}}
		reqs := []optionLegRequest{
			{Key: "exact", Underlying: "BTC", OptionType: "put", Strike: 60000, Expiry: target, Action: "sell", Quantity: 1, Multiplier: 1},
			{Key: "missing-mark", Underlying: "BTC", OptionType: "call", Strike: 70000, Expiry: target, Action: "buy", Quantity: 1, Multiplier: 1},
			{Key: "unlisted", Underlying: "BTC", OptionType: "call", Strike: 99000, Expiry: target, Action: "buy", Quantity: 1, Multiplier: 1},
		}
		replies := run(t, f, "deribit", markAt, reqs...)
		if replies[0].Mark == nil || !ocNear(replies[0].Mark.PerUnitUSD, 0.02*65000) || replies[0].Mark.Source != deribitMarkSource {
			t.Fatalf("exact mark = %+v", replies[0])
		}
		if !strings.Contains(replies[1].Err, "mark_price missing") {
			t.Fatalf("missing mark_price reply = %+v", replies[1])
		}
		if !strings.Contains(replies[2].Err, "status 400") {
			t.Fatalf("an unlisted instrument must be a mark error with no nearest-expiry substitute: %+v", replies[2])
		}
		for _, r := range f.requests {
			if strings.Contains(r, "get_instruments") {
				t.Fatalf("the nearest-expiry search must not run: %v", f.requests)
			}
		}
	})
	t.Run("ibkr_routing", func(t *testing.T) {
		shared := newOptionPricers()
		if got := optionPricerFor("ibkr", shared); got != shared.IBKR || got.Name() != "ibkr" {
			t.Fatalf("ibkr must route to the shared batch provider, got %#v", got)
		}
		cutoff, ready, ok, err := optionGoExpirySchedule("ibkr", target)
		if err != nil || !ok || !cutoff.Equal(time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC)) || !ready.Equal(time.Date(2026, 7, 31, 15, 5, 0, 0, time.UTC)) {
			t.Fatalf("ibkr schedule = %s %s %v %v, want 16:00 and 16:05 London", cutoff, ready, ok, err)
		}
	})
}
