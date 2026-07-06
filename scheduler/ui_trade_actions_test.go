package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTradeActionTestServer builds a StatusServer over a real temp SQLite
// state DB, one type=manual strategy and one live HL perps strategy, each
// with an open long position in the in-memory AppState.
func newTradeActionTestServer(t *testing.T) (*StatusServer, *StateDB, *Config) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &Config{
		DBFile: dbPath,
		Strategies: []StrategyConfig{
			{
				ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid",
				Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py",
				Args: []string{"hold", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2,
			},
			{
				ID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid",
				Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py",
				Args: []string{"tcross", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2,
			},
		},
	}

	state := NewAppState()
	for _, id := range []string{"hl-manual-eth", "hl-perps-eth"} {
		typ := "manual"
		if id == "hl-perps-eth" {
			typ = "perps"
		}
		state.Strategies[id] = &StrategyState{
			ID: id, Type: typ, Platform: "hyperliquid",
			Cash: 1000, InitialCapital: 1000,
			Positions: map[string]*Position{"ETH": {
				Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000,
				EntryATR: 50, Side: "long", Multiplier: 1, Leverage: 2,
				OwnerStrategyID: id, StopLossOID: 111, StopLossTriggerPx: 1900,
				OpenedAt: time.Now().UTC().Add(-time.Hour),
			}},
		}
	}

	ss := NewStatusServer(state, &sync.RWMutex{}, "", cfg.Strategies, db)
	ss.SetConfigContext("", cfg)
	return ss, db, cfg
}

// tradeActionPost drives the real /api/strategies/ prefix router so path
// dispatch is covered too.
func tradeActionPost(ss *StatusServer, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	if path == "/api/confirm" {
		ss.handleAPIConfirm(w, req)
	} else {
		ss.handleAPIStrategy(w, req)
	}
	return w
}

// confirmNonceFor round-trips POST /api/confirm and returns the nonce.
func confirmNonceFor(t *testing.T, ss *StatusServer, action, id, params string) string {
	t.Helper()
	body := fmt.Sprintf(`{"action":%q,"strategy_id":%q,"params":%s}`, action, id, params)
	w := tradeActionPost(ss, "/api/confirm", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, body %s", w.Code, w.Body.String())
	}
	var resp uiConfirmResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse confirm: %v", err)
	}
	if resp.Nonce == "" || resp.ConfirmPhrase != id {
		t.Fatalf("confirm resp = %+v", resp)
	}
	return resp.Nonce
}

// tradeStubs lets a test override individual on-chain seams; anything left
// nil fails loudly if hit.
type tradeStubs struct {
	updateSL    func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error)
	execute     func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
	closer      HyperliquidLiveCloser
	cancelOrder func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)
}

// stubTradeDeps replaces every on-chain seam with fail-loud stubs; tests set
// fields on the returned tradeStubs to allow specific calls.
func stubTradeDeps(t *testing.T, ss *StatusServer) *tradeStubs {
	t.Helper()
	stubs := &tradeStubs{}
	ss.tradeDepsHook = func(d *manualCoreDeps) {
		d.fetchMids = func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		}
		if stubs.cancelOrder != nil {
			d.cancelOrder = stubs.cancelOrder
		} else {
			d.cancelOrder = func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
				t.Error("cancelOrder must not be called")
				return nil, "", fmt.Errorf("stub")
			}
		}
		if stubs.updateSL != nil {
			d.updateSL = stubs.updateSL
		} else {
			d.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				t.Error("updateSL must not be called")
				return nil, "", fmt.Errorf("stub")
			}
		}
		if stubs.execute != nil {
			d.execute = stubs.execute
		} else {
			d.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				t.Error("execute must not be called")
				return nil, "", fmt.Errorf("stub")
			}
		}
		if stubs.closer != nil {
			d.closer = stubs.closer
		} else {
			d.closer = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
				t.Error("closer must not be called")
				return nil, fmt.Errorf("stub")
			}
		}
	}
	return stubs
}

func TestConfirmNonceLifecycle(t *testing.T) {
	ss, _, _ := newTradeActionTestServer(t)
	now := time.Now()

	binding, err := canonicalConfirmBinding("close", "hl-manual-eth", json.RawMessage(`{"qty":0.1}`))
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	// Key-order-insensitive canonicalization.
	binding2, _ := canonicalConfirmBinding("close", "hl-manual-eth", json.RawMessage(`{ "qty": 0.1 }`))
	if binding != binding2 {
		t.Fatalf("binding not canonical: %q vs %q", binding, binding2)
	}

	nonce, err := ss.issueConfirmNonce(binding, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := ss.consumeConfirmNonce(nonce, binding, now); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// Reuse rejected (single-use).
	if err := ss.consumeConfirmNonce(nonce, binding, now); err == nil {
		t.Fatal("reused nonce must be rejected")
	}

	// Expired rejected.
	nonce, _ = ss.issueConfirmNonce(binding, now)
	if err := ss.consumeConfirmNonce(nonce, binding, now.Add(confirmNonceTTL+time.Second)); err == nil {
		t.Fatal("expired nonce must be rejected")
	}
	// ... and burned even though it failed.
	if err := ss.consumeConfirmNonce(nonce, binding, now); err == nil {
		t.Fatal("expired nonce must stay burned")
	}

	// Wrong action binding rejected (and burned).
	nonce, _ = ss.issueConfirmNonce(binding, now)
	other, _ := canonicalConfirmBinding("force-close", "hl-manual-eth", json.RawMessage(`{"qty":0.1}`))
	if err := ss.consumeConfirmNonce(nonce, other, now); err == nil {
		t.Fatal("wrong-action nonce must be rejected")
	}
	if err := ss.consumeConfirmNonce(nonce, binding, now); err == nil {
		t.Fatal("mismatched nonce must be burned on failure")
	}

	// Wrong strategy binding rejected.
	nonce, _ = ss.issueConfirmNonce(binding, now)
	otherStrat, _ := canonicalConfirmBinding("close", "hl-perps-eth", json.RawMessage(`{"qty":0.1}`))
	if err := ss.consumeConfirmNonce(nonce, otherStrat, now); err == nil {
		t.Fatal("wrong-strategy nonce must be rejected")
	}

	// Wrong params binding rejected.
	nonce, _ = ss.issueConfirmNonce(binding, now)
	otherQty, _ := canonicalConfirmBinding("close", "hl-manual-eth", json.RawMessage(`{"qty":0.2}`))
	if err := ss.consumeConfirmNonce(nonce, otherQty, now); err == nil {
		t.Fatal("wrong-params nonce must be rejected")
	}
}

func TestTradeActionNonceBindingOverHTTP(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubTradeDeps(t, ss)

	// Nonce confirmed for close must not authorize force-close.
	nonce := confirmNonceFor(t, ss, "close", "hl-manual-eth", `{"qty":0.1}`)
	w := tradeActionPost(ss, "/api/strategies/hl-perps-eth/force-close",
		fmt.Sprintf(`{"nonce":%q,"params":{"qty":0.1}}`, nonce), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("rebound nonce status = %d, body %s", w.Code, w.Body.String())
	}
	// The nonce is burned: the originally-confirmed action now fails too.
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/close",
		fmt.Sprintf(`{"nonce":%q,"params":{"qty":0.1}}`, nonce), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("burned nonce status = %d, body %s", w.Code, w.Body.String())
	}
	// Missing nonce → 400.
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/close", `{"params":{"qty":0.1}}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing nonce status = %d", w.Code)
	}
	actions, err := db.LoadPendingManualActions()
	if err != nil || len(actions) != 0 {
		t.Fatalf("no action may be queued (err=%v, n=%d)", err, len(actions))
	}
}

func TestTradeActionEndpointsRejectCrossOrigin(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubTradeDeps(t, ss)
	hdr := map[string]string{"Origin": "http://evil.example"}

	paths := []string{"/api/confirm"}
	for _, action := range sortedUITradeActions() {
		paths = append(paths, "/api/strategies/hl-manual-eth/"+action)
	}
	for _, p := range paths {
		w := tradeActionPost(ss, p, `{"action":"close","strategy_id":"hl-manual-eth","nonce":"x","params":{}}`, hdr)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s cross-origin status = %d, want 403", p, w.Code)
		}
	}
	if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
		t.Fatalf("cross-origin request queued %d actions", len(actions))
	}
}

func TestTradeActionsEnforceConfiguredToken(t *testing.T) {
	ss, _, _ := newTradeActionTestServer(t)
	stubTradeDeps(t, ss)
	ss.statusToken = "secret"

	w := tradeActionPost(ss, "/api/confirm", `{"action":"close","strategy_id":"hl-manual-eth","params":{}}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token confirm status = %d, want 401", w.Code)
	}
	w = tradeActionPost(ss, "/api/confirm", `{"action":"close","strategy_id":"hl-manual-eth","params":{}}`,
		map[string]string{"Authorization": "Bearer secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("token confirm status = %d, body %s", w.Code, w.Body.String())
	}
}

// TestUIUpdateSLQueuesPendingActionLikeCLI pins the zero-bypass invariant:
// the UI SL edit produces a PendingManualAction identical to the CLI core's
// (same core), and never touches the position directly.
func TestUIUpdateSLQueuesPendingActionLikeCLI(t *testing.T) {
	ss, db, cfg := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	stubs.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		if cancelOID != 111 {
			t.Errorf("cancelOID = %d, want 111 (cancel-then-queue)", cancelOID)
		}
		return &HyperliquidStopLossUpdateResult{StopLossOID: 555, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: true}, "", nil
	}

	// CLI-path reference row: same core, CLI-style deps over the same DB.
	if err := db.SaveState(ss.state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	cliDeps := newCLIManualCoreDeps(cfg, db, nil)
	cliDeps.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossOID: 555, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: true}, "", nil
	}
	cliDeps.fetchMids = func(coins []string) (map[string]float64, error) { return map[string]float64{"ETH": 2000}, nil }
	sc, err := lookupManualStrategy(cfg, "hl-manual-eth")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := manualUpdateSLCore(cliDeps, sc, manualSLInputs{StrategyID: "hl-manual-eth", Trigger: 1850}); err != nil {
		t.Fatalf("CLI core update-sl: %v", err)
	}
	cliRows, err := db.LoadPendingManualActions()
	if err != nil || len(cliRows) != 1 {
		t.Fatalf("CLI rows = %d (err=%v), want 1", len(cliRows), err)
	}
	cliRow := cliRows[0]
	if err := db.DeletePendingManualActionsThrough(cliRow.ID); err != nil {
		t.Fatalf("clear queue: %v", err)
	}

	// UI path.
	nonce := confirmNonceFor(t, ss, "update-sl", "hl-manual-eth", `{"trigger":1850}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/update-sl",
		fmt.Sprintf(`{"nonce":%q,"params":{"trigger":1850}}`, nonce), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("update-sl status = %d, body %s", w.Code, w.Body.String())
	}
	var resp uiTradeActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.OK || !resp.Queued {
		t.Fatalf("resp = %+v (err=%v), want ok+queued", resp, err)
	}
	if !strings.Contains(resp.Message, "Queued") {
		t.Fatalf("message %q must report the queued outcome", resp.Message)
	}

	uiRows, err := db.LoadPendingManualActions()
	if err != nil || len(uiRows) != 1 {
		t.Fatalf("UI rows = %d (err=%v), want 1", len(uiRows), err)
	}
	uiRow := uiRows[0]
	// Identical rows modulo ID/CreatedAt.
	cliRow.ID, uiRow.ID = 0, 0
	cliRow.CreatedAt, uiRow.CreatedAt = time.Time{}, time.Time{}
	if fmt.Sprintf("%+v", cliRow) != fmt.Sprintf("%+v", uiRow) {
		t.Fatalf("UI row differs from CLI row:\ncli=%+v\nui =%+v", cliRow, uiRow)
	}
	if uiRow.Action != "update-sl" || uiRow.StopLossOID != 555 || uiRow.StopLossTriggerPx != 1850 {
		t.Fatalf("queued row = %+v", uiRow)
	}

	// Regression: no direct position mutation — the daemon adopts on drain.
	pos := ss.state.Strategies["hl-manual-eth"].Positions["ETH"]
	if pos.StopLossOID != 111 || pos.StopLossTriggerPx != 1900 {
		t.Fatalf("position mutated directly: OID=%d trigger=%.2f", pos.StopLossOID, pos.StopLossTriggerPx)
	}
}

func TestUIForceCloseRefusedWhileSLEditQueued(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	closerCalled := false
	stubs.closer = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		closerCalled = true
		return nil, fmt.Errorf("must not run")
	}

	if err := db.InsertPendingManualAction(PendingManualAction{
		StrategyID: "hl-perps-eth", Action: "update-sl", Symbol: "ETH", Side: "long",
		Quantity: 0.4, StopLossOID: 777, StopLossTriggerPx: 1880, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert pending SL edit: %v", err)
	}

	nonce := confirmNonceFor(t, ss, "force-close", "hl-perps-eth", `{}`)
	w := tradeActionPost(ss, "/api/strategies/hl-perps-eth/force-close",
		fmt.Sprintf(`{"nonce":%q,"params":{}}`, nonce), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("force-close status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stop-loss edit") {
		t.Fatalf("body %q must explain the queued SL edit", w.Body.String())
	}
	if closerCalled {
		t.Fatal("closer must not be called while an SL edit is queued")
	}
}

func TestUITradeActionsKillSwitchAndCBGates(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubTradeDeps(t, ss)

	// Kill switch blocks open (and add). Flat first, so the open reaches the
	// core's kill-switch gate instead of the handler's double-open guard.
	delete(ss.state.Strategies["hl-manual-eth"].Positions, "ETH")
	ss.state.PortfolioRisk.KillSwitchActive = true
	nonce := confirmNonceFor(t, ss, "open", "hl-manual-eth", `{"margin":50}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/open",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "kill switch") {
		t.Fatalf("kill-switch open status = %d, body %s", w.Code, w.Body.String())
	}
	ss.state.PortfolioRisk.KillSwitchActive = false

	// Pending circuit-breaker close blocks add.
	ss.state.Strategies["hl-manual-eth"].RiskState.PendingCircuitCloses = map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {},
	}
	nonce = confirmNonceFor(t, ss, "add", "hl-manual-eth", `{"margin":50}`)
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/add",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "circuit-breaker") {
		t.Fatalf("CB add status = %d, body %s", w.Code, w.Body.String())
	}

	if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
		t.Fatalf("gated actions queued %d rows", len(actions))
	}
}

// TestUICloseQueuesFromStubbedFill covers the happy close path end-to-end:
// stubbed venue fill → queued close action → response reports the queue.
func TestUICloseQueuesFromStubbedFill(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	stubs.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		if side != "sell" || size != 0.4 {
			t.Errorf("close exec side=%s size=%.4f, want sell 0.4", side, size)
		}
		if cancelOID != 111 {
			t.Errorf("cancelOID = %d, want 111 (full close cancels SL)", cancelOID)
		}
		return &HyperliquidExecuteResult{
			Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: 0.4, OID: 4242, Fee: 1.5}},
		}, "", nil
	}

	nonce := confirmNonceFor(t, ss, "close", "hl-manual-eth", `{}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/close",
		fmt.Sprintf(`{"nonce":%q,"params":{}}`, nonce), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("close status = %d, body %s", w.Code, w.Body.String())
	}
	rows, err := db.LoadPendingManualActions()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d (err=%v), want 1", len(rows), err)
	}
	row := rows[0]
	if row.Action != "close" || !row.IsFullClose || row.Quantity != 0.4 || row.FillPrice != 2100 {
		t.Fatalf("queued close row = %+v", row)
	}
	// PnL is net of the fee: 0.4*(2100-2000) - 1.5.
	if row.RealizedPnL != 38.5 {
		t.Fatalf("realized pnl = %v, want 38.5", row.RealizedPnL)
	}
	// Position untouched until the scheduler drains.
	if ss.state.Strategies["hl-manual-eth"].Positions["ETH"].Quantity != 0.4 {
		t.Fatal("position mutated before drain")
	}
}

// TestUIOpenGuardsDoubleFire pins the #1260-review double-open guard: a UI
// open is refused while the strategy already holds the symbol or a
// position-increasing action is still queued, and passes again once the
// scheduler drain has applied + deleted the row (simulated by deleting it).
func TestUIOpenGuardsDoubleFire(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)

	// (1) Position already open -> 409, no venue call.
	nonce := confirmNonceFor(t, ss, "open", "hl-manual-eth", `{"margin":50}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/open",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already holds") {
		t.Fatalf("open-with-position status = %d, body %s", w.Code, w.Body.String())
	}

	// (2) Flat, but a queued open (submitted, not yet drained) -> 409.
	delete(ss.state.Strategies["hl-manual-eth"].Positions, "ETH")
	if err := db.InsertPendingManualAction(PendingManualAction{
		StrategyID: "hl-manual-eth", Action: "open", Symbol: "ETH", Side: "long",
		Quantity: 0.05, FillPrice: 2000, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert pending open: %v", err)
	}
	nonce = confirmNonceFor(t, ss, "open", "hl-manual-eth", `{"margin":50}`)
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/open",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already submitted") {
		t.Fatalf("open-with-pending status = %d, body %s", w.Code, w.Body.String())
	}

	// (3) Drain applied (row deleted) -> a legitimate re-open passes.
	rows, _ := db.LoadPendingManualActions()
	if err := db.DeletePendingManualActionsThrough(rows[len(rows)-1].ID); err != nil {
		t.Fatalf("delete pending: %v", err)
	}
	stubs.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		if side != "buy" {
			t.Errorf("open exec side = %s, want buy", side)
		}
		return &HyperliquidExecuteResult{
			Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.05, OID: 555, Fee: 0.5}},
		}, "", nil
	}
	stubs.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: 1900}, "", nil
	}
	nonce = confirmNonceFor(t, ss, "open", "hl-manual-eth", `{"margin":50}`)
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/open",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("re-open status = %d, body %s", w.Code, w.Body.String())
	}
	rows, err := db.LoadPendingManualActions()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d (err=%v), want 1", len(rows), err)
	}
	if rows[0].Action != "open" || rows[0].Quantity != 0.05 || rows[0].FillPrice != 2000 {
		t.Fatalf("queued open row = %+v", rows[0])
	}
	// No in-memory position until the scheduler drains.
	if ss.state.Strategies["hl-manual-eth"].Positions["ETH"] != nil {
		t.Fatal("position created before drain")
	}
	// A peer strategy holding the same coin never blocked any of this: the
	// hl-perps-eth fixture position was present throughout.
	if ss.state.Strategies["hl-perps-eth"].Positions["ETH"] == nil {
		t.Fatal("fixture peer position missing")
	}
}

// TestUIAddQueuesAndGuardsPending: happy add queues an "add" row without
// mutating the in-memory position; a still-queued add blocks a retry.
func TestUIAddQueuesAndGuardsPending(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	stubs.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{
			Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2050, TotalSz: 0.05, OID: 556, Fee: 0.4}},
		}, "", nil
	}

	nonce := confirmNonceFor(t, ss, "add", "hl-manual-eth", `{"margin":50}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/add",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("add status = %d, body %s", w.Code, w.Body.String())
	}
	rows, err := db.LoadPendingManualActions()
	if err != nil || len(rows) != 1 || rows[0].Action != "add" || rows[0].Quantity != 0.05 {
		t.Fatalf("rows = %+v (err=%v)", rows, err)
	}
	if ss.state.Strategies["hl-manual-eth"].Positions["ETH"].Quantity != 0.4 {
		t.Fatal("position mutated before drain")
	}

	// Retry while the add is still queued -> 409, no second venue call.
	stubs.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		t.Error("execute must not be called for a guarded add retry")
		return nil, "", fmt.Errorf("stub")
	}
	nonce = confirmNonceFor(t, ss, "add", "hl-manual-eth", `{"margin":50}`)
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/add",
		fmt.Sprintf(`{"nonce":%q,"params":{"margin":50}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already submitted") {
		t.Fatalf("guarded add status = %d, body %s", w.Code, w.Body.String())
	}
}

// TestUICancelSLQueuesAndErrors: cancel-sl with a resting SL queues the
// removal without touching the in-memory position; with no resting SL it
// errors and queues nothing.
func TestUICancelSLQueuesAndErrors(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	stubs.cancelOrder = func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		if oid != 111 {
			t.Errorf("cancel oid = %d, want 111", oid)
		}
		return &HyperliquidCancelOrderResult{Cancelled: true}, "", nil
	}

	nonce := confirmNonceFor(t, ss, "cancel-sl", "hl-manual-eth", `{}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/cancel-sl",
		fmt.Sprintf(`{"nonce":%q,"params":{}}`, nonce), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel-sl status = %d, body %s", w.Code, w.Body.String())
	}
	rows, err := db.LoadPendingManualActions()
	if err != nil || len(rows) != 1 || rows[0].Action != "cancel-sl" || rows[0].Symbol != "ETH" {
		t.Fatalf("rows = %+v (err=%v)", rows, err)
	}
	if ss.state.Strategies["hl-manual-eth"].Positions["ETH"].StopLossOID != 111 {
		t.Fatal("in-memory SL OID mutated before drain")
	}

	// No resting SL -> error, nothing queued beyond the first row.
	if err := db.DeletePendingManualActionsThrough(rows[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	ss.state.Strategies["hl-manual-eth"].Positions["ETH"].StopLossOID = 0
	stubs.cancelOrder = func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		t.Error("cancelOrder must not be called with no resting SL")
		return nil, "", fmt.Errorf("stub")
	}
	nonce = confirmNonceFor(t, ss, "cancel-sl", "hl-manual-eth", `{}`)
	w = tradeActionPost(ss, "/api/strategies/hl-manual-eth/cancel-sl",
		fmt.Sprintf(`{"nonce":%q,"params":{}}`, nonce), nil)
	if w.Code == http.StatusOK {
		t.Fatalf("cancel-sl with no SL must fail, body %s", w.Body.String())
	}
	if rows, _ := db.LoadPendingManualActions(); len(rows) != 0 {
		t.Fatalf("no-SL cancel queued %d rows", len(rows))
	}
}
