package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quoteHTTP(t *testing.T, ss *StatusServer, h http.HandlerFunc, method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestQuoteDiagnosticsAndControlsArePartitionOwnedAndDurable(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{paperSrc: true, extra: []map[string]any{{
		"id": "hl-plain-eth", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
		"args": []string{"sma_crossover", "ETH", "1h", "--mode=paper"}, "capital": 1000, "leverage": 1, "stop_loss_pct": 5,
	}}})
	ss := NewStatusServer(e.adm.state, e.adm.mu, "", e.cfg.Strategies, e.store)

	code, body := quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=paper:lab", "")
	if code != http.StatusOK || body["owner_count"] != float64(1) {
		t.Fatalf("owned diagnostics read: %d %v", code, body)
	}
	owners := body["owners"].([]any)
	snap := owners[0].(map[string]any)
	if snap["owner_id"] != quoteTestOwner || snap["revision"] == float64(0) || snap["stop"] == nil || snap["inventory"] == "0" {
		t.Fatalf("the snapshot must carry the owner, revision, stop and inventory: %v", snap)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=paper:", ""); code != http.StatusBadRequest {
		t.Fatalf("a malformed partition must be 400, got %d", code)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=paper:other", ""); code != http.StatusNotFound {
		t.Fatalf("an unowned partition must be 404, got %d", code)
	}
	if code, body := quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=paper", ""); code != http.StatusOK || body["owner_count"] != float64(0) || body["held_count"] != float64(0) {
		t.Fatalf("the default paper partition must see no quote owner from the lab file: %d %v", code, body)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteHold, http.MethodPost, "/api/quotes/hold?partition=paper", `{"strategy_id":"hl-quote-btc","hold":true}`); code != http.StatusNotFound {
		t.Fatalf("a control naming another partition must be 404, got %d", code)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=live", ""); code != http.StatusNotFound {
		t.Fatalf("the live partition is not owned here, got %d", code)
	}

	gen := int64(snap["generation"].(float64))
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteCancel, http.MethodPost, "/api/quotes/cancel", `{"strategy_id":"hl-quote-btc","generation":1}`); code != http.StatusBadRequest {
		t.Fatalf("a control with no explicit partition must be 400, got %d", code)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteCancel, http.MethodPost, "/api/quotes/cancel?partition=paper:lab", `{"strategy_id":"hl-quote-btc","generation":99}`); code != http.StatusConflict {
		t.Fatalf("a stale or foreign generation must be refused, got %d", code)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteCancel, http.MethodPost, "/api/quotes/cancel?partition=paper:lab", `{"strategy_id":"someone-else","generation":1}`); code != http.StatusNotFound {
		t.Fatalf("a strategy outside the partition must be 404, got %d", code)
	}
	code, body = quoteHTTP(t, ss, ss.handleAPIQuoteCancel, http.MethodPost, "/api/quotes/cancel?partition=paper:lab", `{"strategy_id":"hl-quote-btc","generation":`+jsonInt(gen)+`}`)
	if code != http.StatusOK || body["cancel_requested"].(float64) < 1 {
		t.Fatalf("a durable cancel request: %d %v", code, body)
	}
	if code, body = quoteHTTP(t, ss, ss.handleAPIQuoteCancel, http.MethodPost, "/api/quotes/cancel?partition=paper:lab", `{"strategy_id":"hl-quote-btc","generation":`+jsonInt(gen)+`}`); code != http.StatusOK || body["cancel_requested"] != float64(0) {
		t.Fatalf("a repeated cancel request is idempotent: %d %v", code, body)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteHold, http.MethodPost, "/api/quotes/hold?partition=paper:lab", `{"strategy_id":"hl-quote-btc","hold":true,"reason":"maintenance"}`); code != http.StatusOK {
		t.Fatalf("a durable hold request: %d", code)
	}
	if code, _ := quoteHTTP(t, ss, ss.handleAPIQuoteHold, http.MethodPost, "/api/quotes/hold?partition=paper:lab", `{"strategy_id":"hl-quote-btc","hold":true,"waive_coverage":true}`); code != http.StatusBadRequest {
		t.Fatalf("a control cannot carry unknown waivers, got %d", code)
	}
	for _, st := range e.stops() {
		if st.CancelState != quoteCancelNone {
			t.Fatalf("a control must never cancel the required stop on nonzero inventory: %+v", st)
		}
	}

	e.reopen()
	if c := e.contract(); !c.OperatorHold || c.OperatorHoldReason != "maintenance" {
		t.Fatalf("the hold must survive a reload: %+v", c)
	}
	requested := 0
	for _, o := range e.sides(gen) {
		if o.CancelState == quoteCancelRequested {
			requested++
		}
	}
	if requested == 0 {
		t.Fatal("the cancel request must survive a reload")
	}
	ss = NewStatusServer(e.adm.state, e.adm.mu, "", e.cfg.Strategies, e.store)
	res := e.mustRun(3, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	if len(res.HoldReasons) == 0 || len(e.sides(res.Generation+1)) != 0 {
		t.Fatalf("the operator hold must hold increases: %+v", res)
	}
	code, body = quoteHTTP(t, ss, ss.handleAPIQuotes, http.MethodGet, "/api/quotes?partition=paper:lab", "")
	if code != http.StatusOK || body["owners"].([]any)[0].(map[string]any)["operator_hold"] != true {
		t.Fatalf("diagnostics must show the hold: %v", body)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
