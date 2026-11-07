package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const trSID = "hl-eth"

type trEnv struct {
	t       *testing.T
	ss      *StatusServer
	store   *StateStore
	state   *AppState
	mu      *sync.RWMutex
	base    time.Time
	fetchMu sync.Mutex
	fetches []UICandleRequest
	source  string
	fail    error
	mutate  func([]UICandle) []UICandle
	hook    func()
}

func trConfig() StrategyConfig {
	return StrategyConfig{ID: trSID, Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h",
		Args: []string{"momentum", "ETH", "1h", "--mode=paper"}}
}

func newTREnvWith(t *testing.T, token string, store *StateStore, cfgs []StrategyConfig) *trEnv {
	t.Helper()
	state := NewAppState()
	for _, sc := range cfgs {
		state.Strategies[sc.ID] = &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 1000, InitialCapital: 1000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	}
	mu := &sync.RWMutex{}
	e := &trEnv{t: t, store: store, state: state, mu: mu, source: "hyperliquid:adapter:range",
		base: time.Now().UTC().Truncate(time.Hour).Add(-100 * time.Hour)}
	e.ss = NewStatusServer(state, mu, token, cfgs, store)
	e.ss.candleCache = nil
	e.ss.candleFetcher = e.fetch
	return e
}

func newTREnv(t *testing.T, token string) *trEnv {
	t.Helper()
	return newTREnvWith(t, token, singleFileStore(openTestDB(t)), []StrategyConfig{trConfig()})
}

func (e *trEnv) fetch(req UICandleRequest) ([]UICandle, string, error) {
	e.fetchMu.Lock()
	e.fetches = append(e.fetches, req)
	hook, fail, mutate, source := e.hook, e.fail, e.mutate, e.source
	e.fetchMu.Unlock()
	if hook != nil {
		hook()
	}
	if fail != nil {
		return nil, "", fail
	}
	bar := int64(3600)
	if d, ok := diagTimeframeDuration(req.Strategy.Timeframe); ok {
		bar = int64(d / time.Second)
	}
	var out []UICandle
	for t := req.From.Unix(); t <= req.To.Unix(); t += bar {
		out = append(out, UICandle{Time: t, Open: 100, High: 120, Low: 90, Close: 105, Volume: 1})
	}
	if mutate != nil {
		out = mutate(out)
	}
	return out, source, nil
}

func (e *trEnv) at(hours float64) time.Time {
	return e.base.Add(time.Duration(hours * float64(time.Hour)))
}

func (e *trEnv) insertTrade(sid string, tr Trade) {
	e.t.Helper()
	if tr.StrategyID == "" {
		tr.StrategyID = sid
	}
	if err := e.store.InsertTrade(sid, tr); err != nil {
		e.t.Fatalf("InsertTrade: %v", err)
	}
}

func (e *trEnv) insertDiag(row TradeDiagnosticsRow) {
	e.t.Helper()
	if err := e.store.InsertTradeDiagnostics(&row); err != nil {
		e.t.Fatalf("InsertTradeDiagnostics: %v", err)
	}
}

func (e *trEnv) seedClosedLong(sid, pid, symbol string, openAt, closeAt time.Time) {
	e.t.Helper()
	e.insertTrade(sid, Trade{Timestamp: openAt, Symbol: symbol, PositionID: pid, Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps", ExchangeFee: 0.05})
	e.insertTrade(sid, Trade{Timestamp: closeAt, Symbol: symbol, PositionID: pid, Side: "sell", Quantity: 1, Price: 110, Value: 110, TradeType: "perps", IsClose: true, RealizedPnL: 10, PnLGross: true, ExchangeFee: 0.05})
	e.insertDiag(TradeDiagnosticsRow{StrategyID: sid, PositionID: pid, Symbol: symbol, Side: "long", CloseReason: "signal",
		EntryPrice: 100, ExitPrice: 110, Quantity: 1, RealizedPnL: 9.95, EntryATR: 2, OpenedAt: openAt, ClosedAt: closeAt, MetricsStatus: diagMetricsPending})
}

func (e *trEnv) seedDefault() {
	e.seedClosedLong(trSID, "pos-a", "ETH", e.at(2.2), e.at(6.1))
}

func (e *trEnv) do(method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ss.handleAPIStrategy(rec, req)
	return rec
}

func (e *trEnv) review(sid, pid string) (*httptest.ResponseRecorder, tradeReviewResponse) {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/strategies/"+url.PathEscape(sid)+"/trade-review?position_id="+url.QueryEscape(pid), nil, nil)
	var resp tradeReviewResponse
	if rec.Code == http.StatusOK || rec.Code == http.StatusBadGateway {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			e.t.Fatalf("decode review: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, resp
}

func trBody(pid string, entry, exit int64, entryPx, exitPx float64, note string) []byte {
	b, _ := json.Marshal(tradeFeedbackRequest{PositionID: pid, EntryTime: entry, ExitTime: exit, EntryPrice: entryPx, ExitPrice: exitPx, Note: note})
	return b
}

func (e *trEnv) post(sid string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	if hdr == nil {
		hdr = map[string]string{"Content-Type": "application/json"}
	}
	return e.do(http.MethodPost, "/api/strategies/"+url.PathEscape(sid)+"/trade-feedback", body, hdr)
}

func (e *trEnv) validBody(pid, note string) []byte {
	return trBody(pid, e.at(2).Unix(), e.at(6).Unix(), 95, 115, note)
}

func trFeedbackRows(t *testing.T, db *StateDB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM trade_feedback`).Scan(&n); err != nil {
		t.Fatalf("count trade_feedback: %v", err)
	}
	return n
}

func TestTradeReviewPositionIdentityAndCompleteHistory(t *testing.T) {
	e := newTREnv(t, "")
	start := e.at(-60)
	e.insertTrade(trSID, Trade{Timestamp: start, Symbol: "ETH", PositionID: "pos-old", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	e.insertTrade(trSID, Trade{Timestamp: start.Add(time.Hour), Symbol: "ETH", PositionID: "pos-old", Side: "sell", Quantity: 1, Price: 101, Value: 101, TradeType: "perps", IsClose: true, RealizedPnL: 1, PnLGross: true})
	big := e.at(1)
	for i := 0; i < 520; i++ {
		e.insertTrade(trSID, Trade{Timestamp: big.Add(time.Duration(i) * time.Second), Symbol: "ETH", PositionID: "pos-big", Side: "buy", Quantity: 0.01, Price: 100, Value: 1, TradeType: "perps"})
	}
	e.insertTrade(trSID, Trade{Timestamp: e.at(5), Symbol: "ETH", PositionID: "pos-big", Side: "sell", Quantity: 5.2, Price: 104, Value: 540.8, TradeType: "perps", IsClose: true, RealizedPnL: 20.8, PnLGross: true})
	e.insertTrade(trSID, Trade{Timestamp: e.at(7), Symbol: "BTC", Side: "buy", Quantity: 1, Price: 50, Value: 50, TradeType: "perps", Details: "legacy"})

	rec := e.do(http.MethodGet, "/api/strategies/"+trSID+"/trades?limit=400", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trades status = %d", rec.Code)
	}
	var tr struct {
		Markers []map[string]interface{} `json:"markers"`
		Trades  []map[string]interface{} `json:"trades"`
		Total   int                      `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode trades: %v", err)
	}
	if tr.Total != 524 || len(tr.Trades) != 400 {
		t.Fatalf("total=%d rows=%d, want 524/400", tr.Total, len(tr.Trades))
	}
	wantKeys := []string{"time", "position", "color", "shape", "text", "side", "is_close", "price", "quantity", "position_id"}
	for _, k := range wantKeys {
		if _, ok := tr.Trades[0][k]; !ok {
			t.Errorf("table row lacks %q: %v", k, tr.Trades[0])
		}
	}
	last := tr.Trades[len(tr.Trades)-1]
	if last["position_id"] != "" || last["text"] != "BUY" || last["price"].(float64) != 50 {
		t.Errorf("newest legacy row = %v, want empty position_id BUY @50", last)
	}
	pids := map[string]bool{}
	for _, m := range tr.Trades {
		pids[m["position_id"].(string)] = true
	}
	if pids["pos-old"] {
		t.Errorf("latest 400 rows unexpectedly include the old position")
	}

	_, old := e.review(trSID, "pos-old")
	if len(old.Fills) != 2 || old.Fills[0].Role != "open" || old.Fills[1].Role != "close" {
		t.Fatalf("old position fills = %+v, want open+close", old.Fills)
	}
	_, bigResp := e.review(trSID, "pos-big")
	if len(bigResp.Fills) != 521 {
		t.Fatalf("big position fills = %d, want 521", len(bigResp.Fills))
	}
	if math.Abs(bigResp.Actual.OpenedQuantity-5.2) > 1e-9 || bigResp.Actual.RemainingQuantity != 0 {
		t.Errorf("big position quantities = %+v", bigResp.Actual)
	}
	for i := 1; i < len(bigResp.Fills); i++ {
		if bigResp.Fills[i].RowID <= bigResp.Fills[i-1].RowID {
			t.Fatalf("fills not in deterministic row order at %d", i)
		}
	}
}

func TestTradeReviewReadContract(t *testing.T) {
	e := newTREnv(t, "")
	e.seedDefault()
	e.seedClosedLong(trSID, "pos-b", "ETH", e.at(10), e.at(12))

	rec, resp := e.review(trSID, "pos-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !resp.Closed || !resp.AnnotationEligible || !resp.SaveAvailable || resp.Coverage.Status != tradeReviewCoverageFull {
		t.Fatalf("review = closed %v eligible %v save %v coverage %+v reason %q", resp.Closed, resp.AnnotationEligible, resp.SaveAvailable, resp.Coverage, resp.Reason)
	}
	if len(resp.Diagnostics) != 1 || resp.Diagnostics[0].ExitPrice != 110 || resp.Actual.CloseReason != "signal" {
		t.Errorf("diagnostics = %+v", resp.Diagnostics)
	}
	if resp.Window.EffectiveFrom != e.at(-18).Unix() || resp.Window.EffectiveTo != e.at(26).Unix() || resp.Window.ExpectedBars != 45 {
		t.Errorf("window = %+v", resp.Window)
	}
	if len(resp.Fills) != 2 {
		t.Errorf("fills = %d, want only pos-a's 2", len(resp.Fills))
	}

	if rec := e.do(http.MethodGet, "/api/strategies/"+trSID+"/trade-review", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("missing position_id = %d, want 400", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/api/strategies/"+trSID+"/trade-review?position_id=", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("empty position_id = %d, want 400", rec.Code)
	}
	if rec, _ := e.review("nope", "pos-a"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown strategy = %d, want 404", rec.Code)
	}
	if rec, _ := e.review(trSID, "pos-zzz"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown position = %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/strategies/"+trSID+"/trade-review?position_id=pos-a", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST review = %d, want 405", rec.Code)
	}

	authed := newTREnv(t, "s3cret")
	authed.seedDefault()
	if rec, _ := authed.review(trSID, "pos-a"); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token = %d, want 401", rec.Code)
	}
	bad := authed.do(http.MethodGet, "/api/strategies/"+trSID+"/trade-review?position_id=pos-a", nil, map[string]string{"Authorization": "Bearer nope"})
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", bad.Code)
	}
	good := authed.do(http.MethodGet, "/api/strategies/"+trSID+"/trade-review?position_id=pos-a", nil, map[string]string{"Authorization": "Bearer s3cret"})
	if good.Code != http.StatusOK {
		t.Errorf("right token = %d, want 200", good.Code)
	}

	nodb := newTREnvWith(t, "", nil, []StrategyConfig{trConfig()})
	if rec, _ := nodb.review(trSID, "pos-a"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no storage = %d, want 503", rec.Code)
	}
	if rec := nodb.post(trSID, nodb.validBody("pos-a", ""), nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no storage POST = %d, want 503", rec.Code)
	}
}

func TestTradeReviewCoverage(t *testing.T) {
	drop := func(times ...int64) func([]UICandle) []UICandle {
		return func(in []UICandle) []UICandle {
			var out []UICandle
			for _, c := range in {
				skip := false
				for _, t := range times {
					if c.Time == t {
						skip = true
					}
				}
				if !skip {
					out = append(out, c)
				}
			}
			return out
		}
	}
	type tc struct {
		name   string
		setup  func(e *trEnv)
		status string
		code   int
		save   bool
	}
	cases := []tc{
		{name: "full", setup: func(e *trEnv) {}, status: tradeReviewCoverageFull, code: 200, save: true},
		{name: "missing_first_bound", setup: func(e *trEnv) { e.mutate = drop(e.at(-18).Unix()) }, status: tradeReviewCoveragePartial, code: 200},
		{name: "missing_last_bound", setup: func(e *trEnv) { e.mutate = drop(e.at(26).Unix()) }, status: tradeReviewCoveragePartial, code: 200},
		{name: "interior_gap", setup: func(e *trEnv) { e.mutate = drop(e.at(4).Unix()) }, status: tradeReviewCoveragePartial, code: 200},
		{name: "none", setup: func(e *trEnv) { e.mutate = func([]UICandle) []UICandle { return nil } }, status: tradeReviewCoverageNone, code: 200},
		{name: "duplicate", setup: func(e *trEnv) {
			e.mutate = func(in []UICandle) []UICandle { return append(in, in[5]) }
		}, status: tradeReviewCoveragePartial, code: 200},
		{name: "invalid_price", setup: func(e *trEnv) {
			e.mutate = func(in []UICandle) []UICandle { in[7].High = 80; return in }
		}, status: tradeReviewCoveragePartial, code: 200},
		{name: "nan_price", setup: func(e *trEnv) {
			e.mutate = func(in []UICandle) []UICandle { in[8].Low = math.NaN(); return in }
		}, status: tradeReviewCoveragePartial, code: 200},
		{name: "off_grid", setup: func(e *trEnv) {
			e.mutate = func(in []UICandle) []UICandle { in[9].Time += 60; return in }
		}, status: tradeReviewCoveragePartial, code: 200},
		{name: "latest_window_source", setup: func(e *trEnv) { e.source = "robinhood:adapter:latest" }, status: tradeReviewCoveragePartial, code: 200},
		{name: "fetch_failure", setup: func(e *trEnv) { e.fail = fmt.Errorf("venue timeout") }, status: tradeReviewCoverageNone, code: 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTREnv(t, "")
			e.seedDefault()
			c.setup(e)
			rec, resp := e.review(trSID, "pos-a")
			if rec.Code != c.code {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.code, rec.Body.String())
			}
			if resp.Coverage.Status != c.status || resp.SaveAvailable != c.save {
				t.Fatalf("coverage = %+v save=%v reason=%q, want %s save=%v", resp.Coverage, resp.SaveAvailable, resp.Reason, c.status, c.save)
			}
			if c.name == "fetch_failure" && len(resp.Candles) != 0 {
				t.Fatalf("fetch failure returned %d candles; no synthetic fallback is allowed", len(resp.Candles))
			}
			post := e.post(trSID, e.validBody("pos-a", ""), nil)
			wantPost := http.StatusOK
			if !c.save {
				wantPost = http.StatusUnprocessableEntity
				if c.code == http.StatusBadGateway {
					wantPost = http.StatusBadGateway
				}
			}
			if post.Code != wantPost {
				t.Fatalf("POST = %d, want %d: %s", post.Code, wantPost, post.Body.String())
			}
			want := 0
			if c.save {
				want = 1
			}
			if n := trFeedbackRows(t, e.store.primary()); n != want {
				t.Fatalf("rows = %d, want %d", n, want)
			}
		})
	}

	t.Run("too_long", func(t *testing.T) {
		e := newTREnv(t, "")
		e.base = time.Now().UTC().Truncate(time.Hour).Add(-3000 * time.Hour)
		e.seedClosedLong(trSID, "pos-long", "ETH", e.at(1), e.at(1500))
		rec, resp := e.review(trSID, "pos-long")
		if rec.Code != 200 || resp.Coverage.Status != tradeReviewCoverageTooLong || len(resp.Candles) != 0 || len(e.fetches) != 0 {
			t.Fatalf("too long: code=%d coverage=%+v candles=%d fetches=%d", rec.Code, resp.Coverage, len(resp.Candles), len(e.fetches))
		}
		if post := e.post(trSID, trBody("pos-long", e.at(1).Unix(), e.at(2).Unix(), 95, 115, ""), nil); post.Code != http.StatusUnprocessableEntity {
			t.Fatalf("too long POST = %d", post.Code)
		}
	})

	t.Run("forming_close_bar_and_future_padding", func(t *testing.T) {
		e := newTREnv(t, "")
		now := time.Now().UTC()
		e.seedClosedLong(trSID, "pos-now", "ETH", now.Add(-5*time.Hour), now)
		_, resp := e.review(trSID, "pos-now")
		if resp.Window == nil || resp.Window.CloseBarComplete || !resp.Window.FuturePaddingShortened || resp.SaveAvailable {
			t.Fatalf("forming close bar window = %+v save=%v", resp.Window, resp.SaveAvailable)
		}
		if resp.Window.EffectiveTo >= now.Unix() || resp.Window.EffectiveTo >= resp.Window.RequestedTo {
			t.Fatalf("effective end %d is not a completed bar before %d", resp.Window.EffectiveTo, resp.Window.RequestedTo)
		}
		start := now.Truncate(time.Hour).Add(-5 * time.Hour).Unix()
		if post := e.post(trSID, trBody("pos-now", start, start+3600, 95, 115, ""), nil); post.Code != http.StatusUnprocessableEntity {
			t.Fatalf("forming bar POST = %d: %s", post.Code, post.Body.String())
		}

		recent := newTREnv(t, "")
		recent.base = now.Truncate(time.Hour).Add(-10 * time.Hour)
		recent.seedClosedLong(trSID, "pos-recent", "ETH", recent.at(1), recent.at(3))
		_, r2 := recent.review(trSID, "pos-recent")
		if !r2.Window.FuturePaddingShortened || !r2.Window.CloseBarComplete || !r2.SaveAvailable {
			t.Fatalf("recent window = %+v save=%v reason=%q", r2.Window, r2.SaveAvailable, r2.Reason)
		}
	})

	t.Run("position_symbol_differs_from_config", func(t *testing.T) {
		e := newTREnv(t, "")
		e.seedClosedLong(trSID, "pos-sol", "SOL", e.at(2), e.at(6))
		_, resp := e.review(trSID, "pos-sol")
		if resp.Symbol != "SOL" || len(e.fetches) != 1 || strategyDisplaySymbol(e.fetches[0].Strategy) != "SOL" || e.fetches[0].Strategy.Timeframe != "1h" {
			t.Fatalf("fetch used %+v for symbol %s", e.fetches, resp.Symbol)
		}
		if e.ss.strategies[0].Symbol != "ETH" {
			t.Fatalf("roster symbol mutated to %s", e.ss.strategies[0].Symbol)
		}
	})
}

func TestTradeFeedbackRefusalsWriteNothing(t *testing.T) {
	e := newTREnv(t, "")
	e.seedDefault()
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "ETH", PositionID: "pos-open", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	e.state.Strategies[trSID].Positions["ETH"] = &Position{Symbol: "ETH", TradePositionID: "pos-open", Side: "long", Quantity: 1, AvgCost: 100}
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "BTC", PositionID: "pos-partial", Side: "buy", Quantity: 2, Price: 100, Value: 200, TradeType: "perps"})
	e.insertTrade(trSID, Trade{Timestamp: e.at(22), Symbol: "BTC", PositionID: "pos-partial", Side: "sell", Quantity: 1, Price: 105, Value: 105, TradeType: "perps", IsClose: true, RealizedPnL: 5, PnLGross: true})
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "SOL", PositionID: "pos-nodiag", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	e.insertTrade(trSID, Trade{Timestamp: e.at(22), Symbol: "SOL", PositionID: "pos-nodiag", Side: "sell", Quantity: 1, Price: 105, Value: 105, TradeType: "perps", IsClose: true, RealizedPnL: 5, PnLGross: true})
	e.seedClosedLong(trSID, "pos-dup", "XRP", e.at(20), e.at(22))
	e.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: "pos-dup", Symbol: "XRP", Side: "long", CloseReason: "signal", EntryPrice: 100, ExitPrice: 110, Quantity: 1, OpenedAt: e.at(20), ClosedAt: e.at(22)})
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "DOGE", PositionID: "pos-corrupt", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	e.insertTrade(trSID, Trade{Timestamp: e.at(22), Symbol: "DOGE", PositionID: "pos-corrupt", Side: "sell", Quantity: 1, Price: 100, TradeType: "perps", IsClose: true, PnLGross: true})
	e.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: "pos-corrupt", Symbol: "DOGE", Side: "long", CloseReason: "signal_corrupt", EntryPrice: 100, ExitPrice: 100, Quantity: 1, OpenedAt: e.at(20), ClosedAt: e.at(22)})
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "AVAX", PositionID: "pos-hedge", Side: "sell", Quantity: 1, Price: 100, Value: 100, TradeType: hedgeTradeType})
	e.insertTrade(trSID, Trade{Timestamp: e.at(22), Symbol: "AVAX", PositionID: "pos-hedge", Side: "buy", Quantity: 1, Price: 99, Value: 99, TradeType: hedgeTradeType, IsClose: true, RealizedPnL: 1, PnLGross: true})
	e.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: "pos-hedge", Symbol: "AVAX", Side: "short", CloseReason: "signal", EntryPrice: 100, ExitPrice: 99, Quantity: 1, OpenedAt: e.at(20), ClosedAt: e.at(22)})
	e.insertTrade(trSID, Trade{Timestamp: e.at(20), Symbol: "LINK", PositionID: "pos-recon", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	e.insertTrade(trSID, Trade{Timestamp: e.at(22), Symbol: "LINK", PositionID: "pos-recon", Side: "sell", Quantity: 1, Price: 104, Value: 104, TradeType: "perps", IsClose: true, RealizedPnL: 4, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment})
	e.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: "pos-recon", Symbol: "LINK", Side: "long", CloseReason: "signal", EntryPrice: 100, ExitPrice: 104, Quantity: 1, OpenedAt: e.at(20), ClosedAt: e.at(22)})

	for _, pid := range []string{"pos-open", "pos-partial"} {
		_, resp := e.review(trSID, pid)
		if resp.Closed || resp.SaveAvailable || len(resp.Fills) == 0 {
			t.Errorf("%s review = closed %v save %v fills %d, want viewable and unsaveable", pid, resp.Closed, resp.SaveAvailable, len(resp.Fills))
		}
	}

	big := strings.Repeat("x", tradeFeedbackBodyLimit+10)
	okHdr := map[string]string{"Content-Type": "application/json"}
	cases := []struct {
		name string
		body []byte
		hdr  map[string]string
		code int
	}{
		{"open", trBody("pos-open", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"partial", trBody("pos-partial", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"absent_diagnostics", trBody("pos-nodiag", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"duplicate_diagnostics", trBody("pos-dup", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"synthetic_close", trBody("pos-corrupt", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"hedge_record", trBody("pos-hedge", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"reconciliation_close", trBody("pos-recon", e.at(20).Unix(), e.at(21).Unix(), 95, 115, ""), nil, 409},
		{"unknown_position", trBody("pos-zzz", e.at(2).Unix(), e.at(6).Unix(), 95, 115, ""), nil, 404},
		{"zero_price", trBody("pos-a", e.at(2).Unix(), e.at(6).Unix(), 0, 115, ""), nil, 400},
		{"negative_price", trBody("pos-a", e.at(2).Unix(), e.at(6).Unix(), 95, -1, ""), nil, 400},
		{"non_bar_time", trBody("pos-a", e.at(2).Unix()+60, e.at(6).Unix(), 95, 115, ""), nil, 400},
		{"outside_window", trBody("pos-a", e.at(-40).Unix(), e.at(6).Unix(), 95, 115, ""), nil, 400},
		{"reversed_times", trBody("pos-a", e.at(6).Unix(), e.at(2).Unix(), 95, 115, ""), nil, 400},
		{"equal_times", trBody("pos-a", e.at(6).Unix(), e.at(6).Unix(), 95, 115, ""), nil, 400},
		{"price_above_bar", trBody("pos-a", e.at(2).Unix(), e.at(6).Unix(), 95, 121, ""), nil, 422},
		{"price_below_bar", trBody("pos-a", e.at(2).Unix(), e.at(6).Unix(), 89, 115, ""), nil, 422},
		{"note_too_long", e.validBody("pos-a", strings.Repeat("é", tradeFeedbackNoteMaxRunes+1)), nil, 400},
		{"unknown_field", []byte(`{"position_id":"pos-a","entry_time":1,"exit_time":2,"entry_price":1,"exit_price":1,"side":"long"}`), nil, 400},
		{"trailing_data", append(e.validBody("pos-a", ""), []byte(` {}`)...), nil, 400},
		{"invalid_json", []byte(`{"position_id":`), nil, 400},
		{"fractional_time", []byte(`{"position_id":"pos-a","entry_time":1.5,"exit_time":2,"entry_price":1,"exit_price":1}`), nil, 400},
		{"oversized", []byte(`{"position_id":"pos-a","note":"` + big + `"}`), nil, 413},
		{"oversized_whitespace", append(e.validBody("pos-a", ""), bytes.Repeat([]byte(" "), tradeFeedbackBodyLimit)...), nil, 413},
		{"cross_origin", e.validBody("pos-a", ""), map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, 403},
		{"form_content_type", e.validBody("pos-a", ""), map[string]string{"Content-Type": "text/plain"}, 415},
	}
	for _, c := range cases {
		hdr := c.hdr
		if hdr == nil {
			hdr = okHdr
		}
		rec := e.post(trSID, c.body, hdr)
		if rec.Code != c.code {
			t.Errorf("%s: status = %d, want %d: %.300s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}
	if rec := e.do(http.MethodGet, "/api/strategies/"+trSID+"/trade-feedback", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET feedback = %d, want 405", rec.Code)
	}
	if rec := e.post("nope", e.validBody("pos-a", ""), nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown strategy POST = %d, want 404", rec.Code)
	}
	authed := newTREnv(t, "s3cret")
	authed.seedDefault()
	if rec := authed.post(trSID, authed.validBody("pos-a", ""), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token POST = %d, want 401", rec.Code)
	}
	if n := trFeedbackRows(t, authed.store.primary()); n != 0 {
		t.Errorf("authed env rows = %d, want 0", n)
	}
	if n := trFeedbackRows(t, e.store.primary()); n != 0 {
		t.Fatalf("refusals wrote %d rows, want 0", n)
	}

	markup := `<script>alert(1)</script><b>bold</b>`
	ok := e.post(trSID, e.validBody("pos-a", strings.Repeat("é", tradeFeedbackNoteMaxRunes-len([]rune(markup)))+markup), nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("2000-character note = %d: %s", ok.Code, ok.Body.String())
	}
	_, resp := e.review(trSID, "pos-a")
	if len(resp.Annotations) != 1 || !strings.HasSuffix(resp.Annotations[0].Note, markup) || len([]rune(resp.Annotations[0].Note)) != tradeFeedbackNoteMaxRunes {
		t.Fatalf("stored note not preserved verbatim: %+v", resp.Annotations)
	}
}

func TestTradeFeedbackWeightedPricesAndNetResult(t *testing.T) {
	e := newTREnv(t, "")
	pid := "pos-w"
	e.state.Strategies[trSID].Positions["ETH"] = &Position{Symbol: "ETH", TradePositionID: "pos-newer", Side: "long", Quantity: 1, AvgCost: 100}
	e.insertTrade(trSID, Trade{Timestamp: e.at(2.1), Symbol: "ETH", PositionID: pid, Side: "buy", Quantity: 2, Price: 100, Value: 200, TradeType: "perps", ExchangeFee: 0.2, EntryATR: 1.5})
	e.insertTrade(trSID, Trade{Timestamp: e.at(3.1), Symbol: "ETH", PositionID: pid, Side: "buy", Quantity: 1, Price: 103, Value: 103, TradeType: scaleInTradeType, ExchangeFee: 0.1})
	e.insertTrade(trSID, Trade{Timestamp: e.at(4.1), Symbol: "ETH", PositionID: pid, Side: "funding", TradeType: TradeTypeFunding, RealizedPnL: -0.3, PnLGross: true})
	e.insertTrade(trSID, Trade{Timestamp: e.at(5.1), Symbol: "ETH", PositionID: pid, Side: "sell", Quantity: 1, Price: 110, Value: 110, TradeType: "perps", IsClose: true, RealizedPnL: 9, PnLGross: true, ExchangeFee: 0.11})
	e.insertTrade(trSID, Trade{Timestamp: e.at(7.1), Symbol: "ETH", PositionID: pid, Side: "sell", Quantity: 2, Price: 108, Value: 216, TradeType: "perps", IsClose: true, RealizedPnL: 15.5, ExchangeFee: 0.2})
	e.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: pid, Symbol: "ETH", Side: "long", CloseReason: "tp", EntryPrice: 101, ExitPrice: 108, Quantity: 2,
		RealizedPnL: 15.5, OpenedAt: e.at(2.1), ClosedAt: e.at(7.1)})

	_, resp := e.review(trSID, pid)
	if !resp.SaveAvailable {
		t.Fatalf("not saveable: %q", resp.Reason)
	}
	a := resp.Actual
	checks := []struct {
		name      string
		got, want float64
	}{
		{"avg_entry", a.AvgEntryPrice, 101},
		{"avg_exit", a.AvgExitPrice, (110 + 216) / 3.0},
		{"net_close", a.RealizedPnL, 8.89 + 15.5},
		{"opening_fees", a.OpeningFees, 0.3},
		{"funding", a.Funding, -0.3},
		{"opened_qty", a.OpenedQuantity, 3},
		{"closed_qty", a.ClosedQuantity, 3},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %.12g, want %.12g", c.name, c.got, c.want)
		}
	}
	if a.RealizedPnLBasis != tradeReviewRealizedBasis {
		t.Errorf("basis = %q", a.RealizedPnLBasis)
	}

	short := newTREnv(t, "")
	short.insertTrade(trSID, Trade{Timestamp: short.at(2.1), Symbol: "ETH", PositionID: "pos-s", Side: "sell", Quantity: 1, Price: 100, Value: 100, TradeType: "perps"})
	short.insertTrade(trSID, Trade{Timestamp: short.at(2.5), Symbol: "ETH", PositionID: "pos-s", Side: "sell", Quantity: 3, Price: 104, Value: 312, TradeType: "perps"})
	short.insertTrade(trSID, Trade{Timestamp: short.at(5.1), Symbol: "ETH", PositionID: "pos-s", Side: "buy", Quantity: 4, Price: 95, Value: 380, TradeType: "perps", IsClose: true, RealizedPnL: 32, PnLGross: true, ExchangeFee: 0.4})
	short.insertDiag(TradeDiagnosticsRow{StrategyID: trSID, PositionID: "pos-s", Symbol: "ETH", Side: "short", CloseReason: "signal", EntryPrice: 103, ExitPrice: 95, Quantity: 4, OpenedAt: short.at(2.1), ClosedAt: short.at(5.1)})
	_, sresp := short.review(trSID, "pos-s")
	if sresp.Side != "short" || !sresp.SaveAvailable || math.Abs(sresp.Actual.AvgEntryPrice-103) > 1e-9 || math.Abs(sresp.Actual.RealizedPnL-31.6) > 1e-9 {
		t.Fatalf("short review = side %s save %v actual %+v reason %q", sresp.Side, sresp.SaveAvailable, sresp.Actual, sresp.Reason)
	}

	rec := e.post(trSID, trBody(pid, e.at(2).Unix(), e.at(7).Unix(), 90, 120, "weighted"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	var raw string
	var realized float64
	if err := e.store.primary().db.QueryRow(`SELECT evidence_json, actual_realized_pnl FROM trade_feedback`).Scan(&raw, &realized); err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if math.Abs(realized-24.39) > 1e-9 {
		t.Errorf("stored realized = %.10g", realized)
	}
	var ev tradeFeedbackEvidence
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	roles := map[string]int{}
	grossFees := 0.0
	for _, f := range ev.Fills {
		roles[f.Role]++
		if f.PnLGross {
			grossFees += f.ExchangeFee
		}
	}
	if roles["open"] != 2 || roles["close"] != 2 || roles["funding"] != 1 || math.Abs(grossFees-0.11) > 1e-12 {
		t.Errorf("frozen fills roles=%v grossFees=%v", roles, grossFees)
	}
	if ev.Version != tradeFeedbackEvidenceVersion || ev.Candles.TimestampKind != "open" || int64(len(ev.Candles.Bars)) != ev.Window.ExpectedBars || ev.Diagnostics.CloseReason != "tp" {
		t.Errorf("evidence header = version %d kind %s bars %d/%d diag %q", ev.Version, ev.Candles.TimestampKind, len(ev.Candles.Bars), ev.Window.ExpectedBars, ev.Diagnostics.CloseReason)
	}
	if strings.Contains(raw, "s3cret") || strings.Contains(raw, "Bearer") {
		t.Errorf("evidence carries credentials")
	}
}

func TestTradeFeedbackAppendOnlyFrozenAndStale(t *testing.T) {
	e := newTREnv(t, "")
	e.seedDefault()
	db := e.store.primary()
	if rec := e.post(trSID, e.validBody("pos-a", "first"), nil); rec.Code != http.StatusOK {
		t.Fatalf("first save = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.post(trSID, trBody("pos-a", e.at(3).Unix(), e.at(5).Unix(), 91, 119, "second"), nil); rec.Code != http.StatusOK {
		t.Fatalf("second save = %d: %s", rec.Code, rec.Body.String())
	}
	if n := trFeedbackRows(t, db); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	var firstEvidence string
	if err := db.db.QueryRow(`SELECT evidence_json FROM trade_feedback ORDER BY rowid LIMIT 1`).Scan(&firstEvidence); err != nil {
		t.Fatal(err)
	}

	reopened := newTREnvWith(t, "", e.store, []StrategyConfig{trConfig()})
	reopened.base = e.base
	_, resp := reopened.review(trSID, "pos-a")
	if len(resp.Annotations) != 2 || resp.Annotations[0].Note != "second" || resp.Annotations[1].Note != "first" || resp.CurrentAnnotationID != resp.Annotations[0].RowID {
		t.Fatalf("history = %+v current=%d", resp.Annotations, resp.CurrentAnnotationID)
	}

	if _, err := db.db.Exec(`UPDATE trades SET price = 111 WHERE is_close = 1 AND position_id = 'pos-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE trade_diagnostics SET exit_price = 111 WHERE position_id = 'pos-a'`); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.db.QueryRow(`SELECT evidence_json FROM trade_feedback ORDER BY rowid LIMIT 1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != firstEvidence || !strings.Contains(after, `"price":110`) {
		t.Fatalf("frozen evidence changed after a source correction")
	}
	if _, err := db.db.Exec(`UPDATE trade_feedback SET note = 'edited'`); err == nil {
		t.Fatalf("trade_feedback accepted an update")
	}
	if _, err := db.db.Exec(`DELETE FROM trade_feedback`); err == nil {
		t.Fatalf("trade_feedback accepted a delete")
	}

	e.hook = func() {
		e.hook = nil
		if _, err := db.db.Exec(`UPDATE trades SET exchange_fee = 0.07 WHERE is_close = 1 AND position_id = 'pos-a'`); err != nil {
			t.Errorf("correction: %v", err)
		}
	}
	if rec := e.post(trSID, e.validBody("pos-a", "stale"), nil); rec.Code != http.StatusConflict {
		t.Fatalf("correction during validation = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if n := trFeedbackRows(t, db); n != 2 {
		t.Fatalf("rows after stale save = %d, want 2", n)
	}
}

func TestTradeFeedbackSplitOwnership(t *testing.T) {
	dir := t.TempDir()
	mk := func(id, src, mode string) StrategyConfig {
		return StrategyConfig{ID: id, StorageStrategyID: "eth", PaperSource: src, Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h",
			Args: []string{"momentum", "ETH", "1h", "--mode=" + mode}}
	}
	cfg := &Config{
		DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}, {ID: "s2", DBFile: filepath.Join(dir, "s2.db")}},
		Strategies:   []StrategyConfig{mk("live-eth", "", "live"), mk("paper-eth", "", "paper"), mk("s1-eth", "s1", "paper"), mk("s2-eth", "s2", "paper")},
	}
	store := openSplitStore(t, cfg)
	stray := mk("stray-eth", "", "paper")
	stray.StorageStrategyID = ""
	e := newTREnvWith(t, "", store, append(append([]StrategyConfig(nil), cfg.Strategies...), stray))
	owners := map[string]storageRole{"live-eth": storageRolePrimary, "paper-eth": storageRolePaper, "s1-eth": paperSourceRole("s1"), "s2-eth": paperSourceRole("s2")}
	ids := []string{"live-eth", "paper-eth", "s1-eth", "s2-eth"}
	for _, id := range ids {
		e.seedClosedLong(id, "pos-shared", "ETH", e.at(2.2), e.at(6.1))
	}
	for i, id := range ids {
		rec := e.post(id, trBody("pos-shared", e.at(2).Unix(), e.at(6).Unix(), 95+float64(i), 115, id), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s save = %d: %s", id, rec.Code, rec.Body.String())
		}
	}
	for _, id := range ids {
		db := store.file(owners[id])
		rows, err := db.db.Query(`SELECT strategy_id, note FROM trade_feedback`)
		if err != nil {
			t.Fatal(err)
		}
		var got [][2]string
		for rows.Next() {
			var sid, note string
			if err := rows.Scan(&sid, &note); err != nil {
				t.Fatal(err)
			}
			got = append(got, [2]string{sid, note})
		}
		rows.Close()
		if len(got) != 1 || got[0][0] != "eth" || got[0][1] != id {
			t.Errorf("%s file rows = %v, want one stored row eth/%s", owners[id], got, id)
		}
		_, resp := e.review(id, "pos-shared")
		if len(resp.Annotations) != 1 || resp.Annotations[0].Note != id || resp.Annotations[0].StrategyID != id {
			t.Errorf("%s review annotations = %+v", id, resp.Annotations)
		}
	}
	if rec := e.post("stray-eth", trBody("pos-shared", e.at(2).Unix(), e.at(6).Unix(), 95, 115, "stray"), nil); rec.Code == http.StatusOK {
		t.Fatalf("an unmapped owner saved")
	}
	total := 0
	for _, role := range []storageRole{storageRolePrimary, storageRolePaper, paperSourceRole("s1"), paperSourceRole("s2")} {
		total += trFeedbackRows(t, store.file(role))
	}
	if total != 4 {
		t.Fatalf("total rows = %d, want 4", total)
	}
}

func TestTradeFeedbackRetentionSchemaAndCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 2; i++ {
		db, err := OpenStateDB(path)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, obj := range []string{"trade_feedback", "idx_trade_feedback_position", "trade_feedback_immutable", "trade_feedback_retained"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, obj).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s count = %d (%v), want 1", obj, n, err)
		}
	}

	store := singleFileStore(db)
	e := newTREnvWith(t, "", store, []StrategyConfig{trConfig()})
	e.seedDefault()

	digest := func() string {
		conn, err := db.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		d, err := captureTableDigest(context.Background(), conn)
		if err != nil {
			t.Fatal(err)
		}
		return d.Sum
	}
	before := digest()
	if rec := e.post(trSID, e.validBody("pos-a", "keep"), nil); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	if digest() == before {
		t.Fatalf("capture digest did not change after a trade_feedback insert")
	}

	if err := SaveStateWithStore(e.state, store); err != nil {
		t.Fatalf("full save: %v", err)
	}
	if n := trFeedbackRows(t, db); n != 1 {
		t.Fatalf("rows after full save = %d", n)
	}
	empty := NewAppState()
	if err := SaveStateWithStore(empty, store); err != nil {
		t.Fatalf("save without the strategy: %v", err)
	}
	var strategies int
	db.db.QueryRow(`SELECT COUNT(*) FROM strategies`).Scan(&strategies)
	if strategies != 0 {
		t.Fatalf("strategies rows = %d, want the removed book gone", strategies)
	}
	if n := trFeedbackRows(t, db); n != 1 {
		t.Fatalf("rows after removal save = %d, want 1", n)
	}
}

func TestTradeFeedbackRefusesPendingConfigContextChange(t *testing.T) {
	writeCfg := func(t *testing.T, strategies string) string {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"strategies":[`+strategies+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name  string
		entry string
		code  int
	}{
		{"unchanged", `{"id":"hl-eth","type":"perps","platform":"hyperliquid","args":["momentum","ETH","1h","--mode=paper"]}`, 200},
		{"pending_paper_to_live", `{"id":"hl-eth","type":"perps","platform":"hyperliquid","args":["momentum","ETH","1h","--mode=live"]}`, 409},
		{"removed", `{"id":"other","type":"perps","platform":"hyperliquid","args":["momentum","ETH","1h","--mode=paper"]}`, 409},
		{"pending_source", `{"id":"hl-eth","type":"perps","platform":"hyperliquid","paper_source":"s1","args":["momentum","ETH","1h","--mode=paper"]}`, 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTREnv(t, "")
			e.seedDefault()
			e.ss.configPath = writeCfg(t, c.entry)
			rec := e.post(trSID, e.validBody("pos-a", ""), nil)
			if rec.Code != c.code {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.code, rec.Body.String())
			}
			want := 0
			if c.code == 200 {
				want = 1
			}
			if n := trFeedbackRows(t, e.store.primary()); n != want {
				t.Fatalf("rows = %d, want %d", n, want)
			}
		})
	}
	t.Run("removed_in_process", func(t *testing.T) {
		e := newTREnv(t, "")
		e.seedDefault()
		e.ss.removedStrategies = map[string]bool{trSID: true}
		if rec := e.post(trSID, e.validBody("pos-a", ""), nil); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
}

func TestTradeFeedbackManualHyperliquidLoaderDefaultsAreNoPendingChange(t *testing.T) {
	const sid = "hl-manual-btc"
	skeleton := map[string]interface{}{"id": sid, "type": "manual", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h", "leverage": 2, "capital": 1000}
	noPlatform := map[string]interface{}{"id": sid, "type": "manual", "symbol": "BTC", "timeframe": "1h", "leverage": 2, "capital": 1000}
	explicitPaper := map[string]interface{}{"id": sid, "type": "manual", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h", "leverage": 2, "capital": 1000,
		"args": []string{"hold", "BTC", "1h", "--mode=paper"}}
	cases := []struct {
		name   string
		roster map[string]interface{}
		disk   map[string]interface{}
		code   int
	}{
		{"argsless_skeleton_unchanged", skeleton, skeleton, http.StatusOK},
		{"disk_switched_to_paper", skeleton, explicitPaper, http.StatusConflict},
		{"inferred_platform_unchanged", noPlatform, noPlatform, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, c.roster))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			var loaded StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.ID == sid {
					loaded = sc
				}
			}
			if strategyDisplayMode(loaded) != "live" || loaded.Platform != "hyperliquid" {
				t.Fatalf("roster strategy mode=%q platform=%q, want live hyperliquid (args %v)", strategyDisplayMode(loaded), loaded.Platform, loaded.Args)
			}
			e := newTREnvWith(t, "", singleFileStore(openTestDB(t)), []StrategyConfig{loaded})
			e.seedClosedLong(sid, "pos-a", "BTC", e.at(2.2), e.at(6.1))
			e.ss.configPath = writeNoEdgeConfig(t, CurrentConfigVersion, c.disk)
			_, review := e.review(sid, "pos-a")
			if wantAvail := c.code == http.StatusOK; review.SaveAvailable != wantAvail {
				t.Fatalf("review save_available = %v, want %v (reason %q)", review.SaveAvailable, wantAvail, review.Reason)
			}
			rec := e.post(sid, e.validBody("pos-a", ""), nil)
			if rec.Code != c.code {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.code, rec.Body.String())
			}
			want := 0
			if c.code == http.StatusOK {
				want = 1
			}
			if n := trFeedbackRows(t, e.store.primary()); n != want {
				t.Fatalf("rows = %d, want %d", n, want)
			}
		})
	}
}

func TestTradeFeedbackConcurrentSavesReloadsAndConfigWrites(t *testing.T) {
	e := newTREnv(t, "")
	e.seedDefault()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"strategies":[{"id":"hl-eth","type":"perps","platform":"hyperliquid","args":["momentum","ETH","1h","--mode=paper"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ss.configPath = cfgPath
	done := make(chan struct{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.mu.Lock()
			err := SaveStateWithStore(e.state, e.store)
			e.mu.Unlock()
			if err != nil {
				t.Errorf("save: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.mu.RLock()
			e.ss.UpdateStrategies([]StrategyConfig{trConfig()})
			e.mu.RUnlock()
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.ss.configWriteMu.Lock()
			raw, _ := os.ReadFile(cfgPath)
			_ = os.WriteFile(cfgPath, raw, 0o600)
			e.ss.configWriteMu.Unlock()
		}
	}()
	ok := 0
	go func() {
		defer close(done)
		for i := 0; i < 8; i++ {
			rec := e.post(trSID, e.validBody("pos-a", fmt.Sprintf("n%d", i)), nil)
			if rec.Code == http.StatusOK {
				ok++
			} else {
				t.Errorf("concurrent save %d = %d: %s", i, rec.Code, rec.Body.String())
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("saves did not finish; possible lock deadlock")
	}
	close(stop)
	wg.Wait()
	if n := trFeedbackRows(t, e.store.primary()); n != ok || ok != 8 {
		t.Fatalf("rows = %d successes = %d, want 8", n, ok)
	}
}
