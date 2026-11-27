package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPaperManualDashboardRefusesVenueActionsIntegration(t *testing.T) {
	cfg := paperManualGuardConfig(t)
	f := newPCFixture(t, cfg)
	calls := &paperManualVenueCalls{}
	f.ss.tradeDepsHook = calls.hook(t)
	params := map[string]string{
		"open":      `{"side":"long","size":0.01}`,
		"add":       `{"size":0.01}`,
		"close":     `{}`,
		"update-sl": `{"trigger":90}`,
		"cancel-sl": `{}`,
	}
	for _, id := range []string{"pm-default", "pm-args", "pm-nomode", "pm-alt"} {
		wantPart := "the paper partition"
		if id == "pm-alt" {
			wantPart = "the paper:alt partition"
		}
		for _, action := range []string{"open", "add", "close", "update-sl", "cancel-sl"} {
			body := fmt.Sprintf(`{"action":%q,"strategy_id":%q,"params":%s}`, action, id, params[action])
			w := mutationPost(f.ss, f.ss.handleAPIConfirm, "/api/confirm", body, nil)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), wantPart) || !strings.Contains(w.Body.String(), "no order was sent") {
				t.Fatalf("%s %s confirm must refuse the paper venue action: %d %s", id, action, w.Code, w.Body.String())
			}
			binding, err := canonicalConfirmBinding(action, id, json.RawMessage(params[action]))
			if err != nil {
				t.Fatal(err)
			}
			nonce, err := f.ss.issueConfirmNonce(binding, "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			h := func(w http.ResponseWriter, r *http.Request) { f.ss.handleAPIStrategyTradeAction(w, r, id, action) }
			ew := mutationPost(f.ss, h, "/api/strategies/"+id+"/"+action, fmt.Sprintf(`{"nonce":%q,"params":%s}`, nonce, params[action]), nil)
			if ew.Code != http.StatusConflict || !strings.Contains(ew.Body.String(), wantPart) {
				t.Fatalf("%s %s execution with a pre-issued nonce must refuse: %d %s", id, action, ew.Code, ew.Body.String())
			}
		}
	}
	if len(calls.calls) != 0 {
		t.Fatalf("a refused paper venue action reached a protected dependency: %v", calls.calls)
	}
	actions, err := f.env.store.LoadPendingManualActions()
	if err != nil || len(actions) != 0 {
		t.Fatalf("a refused paper venue action must queue nothing: %v %+v", err, actions)
	}
	w := mutationPost(f.ss, f.ss.handleAPIConfirm, "/api/confirm", `{"action":"open","strategy_id":"lm-live","params":{"side":"long","size":0.01}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a live manual strategy keeps its dashboard confirmation: %d %s", w.Code, w.Body.String())
	}
}

func TestPaperManualCycleCloseBooksSimulatedFillsIntegration(t *testing.T) {
	prevLot := paperManualCloseLotLookup
	lotKnown := true
	paperManualCloseLotLookup = func(coin string) hlLotLookup {
		if !lotKnown {
			return hlLotLookup{Coin: coin, Reason: "test lot metadata unavailable"}
		}
		return hlLotLookup{Coin: coin, Known: true, SzDecimals: 3}
	}
	writes := 0
	prevExec, prevSL, prevCancel := runHyperliquidExecuteFn, runHyperliquidUpdateStopLossFunc, runHyperliquidCancelOrderFn
	runHyperliquidExecuteFn = func(_ hlWriteAuth, script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extra ...int64) (*HyperliquidExecuteResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	runHyperliquidUpdateStopLossFunc = func(_ hlWriteAuth, script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	runHyperliquidCancelOrderFn = func(_ hlWriteAuth, script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	t.Cleanup(func() {
		paperManualCloseLotLookup = prevLot
		runHyperliquidExecuteFn, runHyperliquidUpdateStopLossFunc, runHyperliquidCancelOrderFn = prevExec, prevSL, prevCancel
	})

	cfg := paperManualGuardConfig(t)
	env := openEntryTestEnv(t, cfg)
	openedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seed := func(id, side string, qty, avg float64) Position {
		t.Helper()
		pos := &Position{Symbol: "BTC", Side: side, Quantity: qty, InitialQuantity: qty, AvgCost: avg, EntryATR: 1, OwnerStrategyID: id, TradePositionID: id + "-pos", OpenedAt: openedAt}
		if id == "lm-live" {
			pos.StopLossOID, pos.StopLossTriggerPx, pos.TPOIDs = 4242, 80, []int64{4343}
		}
		env.mu.Lock()
		env.strat(id).Positions = map[string]*Position{"BTC": pos}
		env.mu.Unlock()
		env.saveAll()
		return *pos
	}
	run := func(id string, pinned Position, res *HyperliquidResult, price float64) paperManualCycleCloseOutcome {
		t.Helper()
		sc, err := lookupManualStrategy(cfg, id)
		if err != nil {
			t.Fatal(err)
		}
		p := pinned
		return queuePaperManualCycleClose(paperManualCycleCloseInput{SC: sc, State: env.strat(id), Store: env.store, Pinned: &p, Result: res, Price: price, Mu: &env.mu, Logger: silentStrategyLogger(id)})
	}
	pending := func() int {
		t.Helper()
		rows, err := env.store.LoadPendingManualActions()
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	drain := func() {
		t.Helper()
		env.mu.Lock()
		drainPendingManualActions(env.state, cfg, env.store)
		env.mu.Unlock()
		env.saveAll()
	}
	closing := func(signal int, frac float64) *HyperliquidResult {
		return &HyperliquidResult{Symbol: "BTC", Signal: signal, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: frac}}
	}

	live := seed("lm-live", "long", 2, 100)

	long := seed("pm-args", "long", 1, 100)
	if out := run("pm-args", long, closing(-1, 1), 110); !out.Queued || !out.Action.IsFullClose {
		t.Fatalf("a full paper close must queue a simulated full exit: %+v", out)
	}
	if out := run("pm-args", long, closing(-1, 1), 110); out.Queued || !strings.Contains(out.Hold, "already queued") {
		t.Fatalf("a second evaluation before the drain must hold: %+v", out)
	}
	drain()
	if qty, _ := env.position("pm-args", "BTC"); qty != 0 {
		t.Fatalf("the full paper close left %.6f BTC", qty)
	}
	trades := env.strat("pm-args").TradeHistory
	if n := len(trades); n == 0 || !trades[n-1].IsClose || trades[n-1].Price != ApplyAdverseSlippage(110, false) || trades[n-1].FeeSource != FeeSourceModeled {
		t.Fatalf("the full paper close must book one simulated close trade: %+v", trades)
	}

	short := seed("pm-default", "short", 1, 100)
	if out := run("pm-default", short, closing(1, 0.5), 90); !out.Queued || out.Action.Quantity != 0.5 || out.Action.IsFullClose {
		t.Fatalf("a partial short paper close must queue 0.5: %+v", out)
	}
	drain()
	if qty, side := env.position("pm-default", "BTC"); qty != 0.5 || side != "short" {
		t.Fatalf("the partial short close must leave 0.5 short, got %.6f %s", qty, side)
	}

	tier := seed("pm-nomode", "long", 1, 100)
	res := closing(-1, 0.5)
	res.CloseTierFillPrice = 120
	if out := run("pm-nomode", tier, res, 110); !out.Queued || out.Action.FillPrice != 120 {
		t.Fatalf("a tier close must book at the tier price: %+v", out)
	}
	drain()

	held := seed("pm-alt", "long", 1, 100)
	before := pending()
	for name, c := range map[string]struct {
		res   *HyperliquidResult
		price float64
	}{
		"signal opens the held side": {closing(1, 1), 100},
		"no signal":                  {closing(0, 1), 100},
		"fraction above one":         {closing(-1, 1.5), 100},
		"zero fraction":              {closing(-1, 0), 100},
		"no price":                   {closing(-1, 1), 0},
		"other coin":                 {&HyperliquidResult{Symbol: "ETH", Signal: -1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1}}, 100},
		"no result":                  {nil, 100},
	} {
		if out := run("pm-alt", held, c.res, c.price); out.Queued || out.Hold == "" {
			t.Fatalf("%s must hold: %+v", name, out)
		}
	}
	lotKnown = false
	if out := run("pm-alt", held, closing(-1, 0.5), 100); out.Queued || !strings.Contains(out.Hold, hlPaperHoldLotUnknown) {
		t.Fatalf("a partial close with no lot data must hold: %+v", out)
	}
	lotKnown = true
	small := seed("pm-alt", "long", 0.05, 100)
	if out := run("pm-alt", small, closing(-1, 0.5), 100); out.Queued || !strings.Contains(out.Hold, hlPaperHoldBelowMin) {
		t.Fatalf("a partial close below the venue minimum must hold: %+v", out)
	}
	stale := small
	stale.Quantity = 0.04
	if out := run("pm-alt", stale, closing(-1, 1), 100); out.Queued || !strings.Contains(out.Hold, "changed") {
		t.Fatalf("a stale pinned position must hold: %+v", out)
	}
	if n := pending(); n != before {
		t.Fatalf("a held paper close queued %d row(s)", n-before)
	}
	if q, _ := env.position("pm-alt", "BTC"); q != 0.05 {
		t.Fatalf("a held paper close changed the book: %.6f", q)
	}
	lotKnown = false
	if out := run("pm-alt", small, closing(-1, 1), 100); !out.Queued {
		t.Fatalf("a full exit bypasses the partial lot check: %+v", out)
	}
	lotKnown = true
	drain()
	if out := run("lm-live", live, closing(-1, 1), 100); out.Queued || !strings.Contains(out.Hold, "live") {
		t.Fatalf("the paper close path must refuse a live strategy: %+v", out)
	}

	env = env.restart()
	if q, _ := env.position("pm-args", "BTC"); q != 0 {
		t.Fatalf("reloaded pm-args still holds %.6f", q)
	}
	if q, _ := env.position("pm-default", "BTC"); q != 0.5 {
		t.Fatalf("reloaded pm-default holds %.6f, want 0.5", q)
	}
	if q, _ := env.position("pm-nomode", "BTC"); q != 0.5 {
		t.Fatalf("reloaded pm-nomode holds %.6f, want 0.5", q)
	}
	if q, _ := env.position("pm-alt", "BTC"); q != 0 {
		t.Fatalf("reloaded pm-alt holds %.6f, want flat", q)
	}
	var altRows int
	if err := env.fileFor("pm-alt").db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'pm-alt' AND is_close = 1").Scan(&altRows); err != nil || altRows != 1 {
		t.Fatalf("the named paper partition file must hold the pm-alt close: %d %v", altRows, err)
	}
	env.mu.RLock()
	peer := env.strat("lm-live").Positions["BTC"]
	env.mu.RUnlock()
	if peer == nil || peer.Quantity != live.Quantity || peer.StopLossOID != live.StopLossOID || len(peer.TPOIDs) != 1 || peer.TPOIDs[0] != 4343 {
		t.Fatalf("the live peer's book or protection changed: %+v", peer)
	}
	if writes != 0 {
		t.Fatalf("a paper evaluator close reached %d venue write(s)", writes)
	}
}

func TestPaperManualCycleCloseFetchesLotMetadataIntegration(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests[fmt.Sprint(body["type"])]++
		failing := fail
		mu.Unlock()
		if failing {
			http.Error(w, "controlled metadata outage", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"universe":[{"name":"BTC","szDecimals":3},{"name":"ETH","szDecimals":4}]}`))
	}))
	t.Cleanup(srv.Close)
	prevURL, prevCache := hlMainnetURL, hlLotMetadata
	hlMainnetURL = srv.URL
	writes := 0
	prevExec, prevSL, prevCancel := runHyperliquidExecuteFn, runHyperliquidUpdateStopLossFunc, runHyperliquidCancelOrderFn
	runHyperliquidExecuteFn = func(_ hlWriteAuth, script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extra ...int64) (*HyperliquidExecuteResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	runHyperliquidUpdateStopLossFunc = func(_ hlWriteAuth, script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	runHyperliquidCancelOrderFn = func(_ hlWriteAuth, script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		writes++
		return nil, "", fmt.Errorf("no venue write may run")
	}
	t.Cleanup(func() {
		hlMainnetURL, hlLotMetadata = prevURL, prevCache
		runHyperliquidExecuteFn, runHyperliquidUpdateStopLossFunc, runHyperliquidCancelOrderFn = prevExec, prevSL, prevCancel
	})

	dir := t.TempDir()
	cfg := &Config{
		DBFile:          filepath.Join(dir, "live.db"),
		PaperDBFile:     filepath.Join(dir, "paper.db"),
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{ID: "pm-args", Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", Args: []string{"hold", "BTC", "1h", "--mode=paper"}, Leverage: 1, Capital: 1000},
			{ID: "lm-live", Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 1, Capital: 1000},
		},
	}
	env := openEntryTestEnv(t, cfg)
	pos := &Position{Symbol: "BTC", Side: "long", Quantity: 0.667, InitialQuantity: 0.667, AvgCost: 100, EntryATR: 1, OwnerStrategyID: "pm-args", TradePositionID: "pm-args-pos", OpenedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	env.mu.Lock()
	env.strat("pm-args").Positions = map[string]*Position{"BTC": pos}
	env.mu.Unlock()
	env.saveAll()
	sc, err := lookupManualStrategy(cfg, "pm-args")
	if err != nil {
		t.Fatal(err)
	}
	run := func(frac float64) paperManualCycleCloseOutcome {
		t.Helper()
		pinned := *pos
		res := &HyperliquidResult{Symbol: "BTC", Signal: -1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: frac}}
		return queuePaperManualCycleClose(paperManualCycleCloseInput{SC: sc, State: env.strat("pm-args"), Store: env.store, Pinned: &pinned, Result: res, Price: 110, Mu: &env.mu, Logger: silentStrategyLogger("pm-args")})
	}
	queued := func() []PendingManualAction {
		t.Helper()
		rows, err := env.store.LoadPendingManualActions()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	metaRequests := func() int {
		mu.Lock()
		defer mu.Unlock()
		if len(requests) > 1 || (len(requests) == 1 && requests["meta"] == 0) {
			t.Fatalf("only lot metadata reads may reach the venue: %v", requests)
		}
		return requests["meta"]
	}

	hlLotMetadata = newHLLotMetadataCache(nil)
	if out := run(0.5); out.Queued || !strings.Contains(out.Hold, "lot") {
		t.Fatalf("a partial paper close must hold while the lot metadata fetch fails: %+v", out)
	}
	if metaRequests() == 0 {
		t.Fatal("the partial paper close must ask the venue for lot metadata")
	}
	if rows := queued(); len(rows) != 0 {
		t.Fatalf("a held partial close must queue nothing: %+v", rows)
	}

	mu.Lock()
	fail = false
	requests = map[string]int{}
	mu.Unlock()
	hlLotMetadata = newHLLotMetadataCache(nil)
	out := run(0.5)
	if !out.Queued || out.Action.IsFullClose || math.Abs(out.Action.Quantity-0.333) > 1e-12 {
		t.Fatalf("a fresh process must fetch the lot metadata and queue the floored 0.333 BTC partial close: %+v", out)
	}
	if got := metaRequests(); got != 1 {
		t.Fatalf("the partial close must fetch the lot metadata one time, got %d", got)
	}
	if rows := queued(); len(rows) != 1 || rows[0].Action != "close" || math.Abs(rows[0].Quantity-0.333) > 1e-12 {
		t.Fatalf("one floored close row must be queued: %+v", rows)
	}
	env.mu.Lock()
	drainPendingManualActions(env.state, cfg, env.store)
	env.mu.Unlock()
	env.saveAll()

	mu.Lock()
	fail = true
	requests = map[string]int{}
	mu.Unlock()
	hlLotMetadata = newHLLotMetadataCache(nil)
	env.mu.RLock()
	*pos = *env.strat("pm-args").Positions["BTC"]
	env.mu.RUnlock()
	if out := run(1); !out.Queued || !out.Action.IsFullClose {
		t.Fatalf("a full paper close must queue without lot data: %+v", out)
	}
	if got := metaRequests(); got != 0 {
		t.Fatalf("a full close needs no lot metadata read, got %d", got)
	}
	if writes != 0 {
		t.Fatalf("paper cycle closes sent %d venue writes", writes)
	}
}
