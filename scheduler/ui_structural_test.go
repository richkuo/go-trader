package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestGetRequest(url string) *http.Request {
	return httptest.NewRequest(http.MethodGet, url, nil)
}

func newTestRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// waitForRestarts polls for the async fire-and-forget restart goroutine.
func waitForRestarts(t *testing.T, restarts *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if restarts.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("restarts = %d, want %d", restarts.Load(), want)
}

// newStructuralTestServer builds a StatusServer wired to a real temp config
// file (the shared minimalConfigJSON fixture: one binanceus spot + one paper
// HL perps strategy) with the restart trigger stubbed to a counter.
func newStructuralTestServer(t *testing.T) (*StatusServer, string, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	strategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
		{ID: "hl-momentum-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}},
	}
	ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", strategies, nil)
	ss.SetConfigContext(path, &Config{IntervalSeconds: 300})
	restarts := &atomic.Int32{}
	ss.restartFn = func() error {
		restarts.Add(1)
		return nil
	}
	return ss, path, restarts
}

// structuralConfirm runs POST /api/confirm for a structural action and
// returns the decoded response. Fails the test on a non-200 unless wantErr.
func structuralConfirm(t *testing.T, ss *StatusServer, action, strategyID, params string) uiConfirmResponse {
	t.Helper()
	body := fmt.Sprintf(`{"action":%q,"strategy_id":%q,"params":%s}`, action, strategyID, params)
	w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm %s status = %d, body %s", action, w.Code, w.Body.String())
	}
	var resp uiConfirmResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse confirm response: %v", err)
	}
	return resp
}

func configStrategyIDs(t *testing.T, path string) []string {
	t.Helper()
	root := readConfigRoot(t, path)
	list, err := configStrategies(root)
	if err != nil {
		t.Fatalf("configStrategies: %v", err)
	}
	ids := make([]string, 0, len(list))
	for _, raw := range list {
		ids = append(ids, strategyRawID(raw))
	}
	return ids
}

func TestUIStructuralEndpointsRejectCrossOriginAndNonPOST(t *testing.T) {
	ss, _, _ := newStructuralTestServer(t)
	endpoints := map[string]func(http.ResponseWriter, *http.Request){
		"/api/config/add-strategy": ss.handleAPIAddStrategy,
		"/api/strategies/hl-momentum-eth/remove-strategy": func(w http.ResponseWriter, r *http.Request) {
			ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "remove-strategy")
		},
		"/api/strategies/hl-momentum-eth/paper-to-live": func(w http.ResponseWriter, r *http.Request) {
			ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "paper-to-live")
		},
		"/api/strategies/hl-momentum-eth/apply-regime-gate": func(w http.ResponseWriter, r *http.Request) {
			ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "apply-regime-gate")
		},
	}
	for url, h := range endpoints {
		// Cross-origin POST refused.
		w := mutationPost(ss, h, url, `{"nonce":"x","params":{}}`, map[string]string{"Origin": "http://evil.example"})
		if w.Code != http.StatusForbidden {
			t.Errorf("%s cross-origin status = %d, want 403", url, w.Code)
		}
		// GET refused.
		req := newTestGetRequest(url)
		rec := newTestRecorder()
		h(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status = %d, want 405", url, rec.Code)
		}
		// Missing nonce refused before any config touch.
		w = mutationPost(ss, h, url, `{"params":{}}`, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "nonce is required") {
			t.Errorf("%s no-nonce status = %d body %s, want 400 nonce-required", url, w.Code, w.Body.String())
		}
		// Bogus nonce refused.
		w = mutationPost(ss, h, url, `{"nonce":"deadbeef","params":{}}`, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s bogus-nonce status = %d, want 403", url, w.Code)
		}
	}
}

func TestUIAddStrategyEndToEnd(t *testing.T) {
	ss, path, restarts := newStructuralTestServer(t)
	params := `{"name":"momentum","platform":"hyperliquid","asset":"SOL"}`
	confirm := structuralConfirm(t, ss, "add-strategy", "", params)
	if confirm.ConfirmPhrase == "" || !strings.HasPrefix(confirm.ConfirmPhrase, "hl-") {
		t.Fatalf("confirm phrase should be the generated hl- id, got %q", confirm.ConfirmPhrase)
	}
	if !strings.Contains(confirm.Description, "PAPER mode") {
		t.Errorf("description should state paper mode: %s", confirm.Description)
	}
	if !strings.Contains(confirm.Description, "restart-required") {
		t.Errorf("description should state the restart requirement: %s", confirm.Description)
	}

	body := fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params)
	w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("add-strategy status = %d, body %s", w.Code, w.Body.String())
	}
	ids := configStrategyIDs(t, path)
	if len(ids) != 3 || ids[2] != confirm.ConfirmPhrase {
		t.Fatalf("config strategies after add = %v, want 3rd = %s", ids, confirm.ConfirmPhrase)
	}
	// New entry is paper mode.
	if !strings.Contains(string(mustReadFile(t, path)), "--mode=paper") {
		t.Error("new strategy must be created in paper mode")
	}
	// restart not requested → not fired.
	if restarts.Load() != 0 {
		t.Fatalf("restarts = %d, want 0 (restart not requested)", restarts.Load())
	}
	// The written config must still pass the real validator.
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("config after add-strategy fails validation: %v", err)
	}

	// Nonce is single-use: replay refused, config unchanged.
	w = mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("nonce replay status = %d, want 403", w.Code)
	}
	if got := configStrategyIDs(t, path); len(got) != 3 {
		t.Fatalf("replay must not mutate config; strategies = %v", got)
	}
}

func TestUIAddStrategyRestartParamFiresRestart(t *testing.T) {
	ss, _, restarts := newStructuralTestServer(t)
	params := `{"name":"momentum","platform":"hyperliquid","asset":"SOL","restart":true}`
	confirm := structuralConfirm(t, ss, "add-strategy", "", params)
	body := fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params)
	w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("add-strategy status = %d, body %s", w.Code, w.Body.String())
	}
	waitForRestarts(t, restarts, 1)
}

// The nonce binding covers the exact params: a nonce confirmed for one param
// set must not authorize an execute with different params.
func TestUIStructuralNonceBindingCoversParams(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	confirm := structuralConfirm(t, ss, "add-strategy", "", `{"name":"momentum","platform":"hyperliquid","asset":"SOL"}`)
	// Execute with a DIFFERENT asset.
	body := fmt.Sprintf(`{"nonce":%q,"params":{"name":"momentum","platform":"hyperliquid","asset":"DOGE"}}`, confirm.Nonce)
	w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("param-mismatch status = %d, want 403; body %s", w.Code, w.Body.String())
	}
	if got := configStrategyIDs(t, path); len(got) != 2 {
		t.Fatalf("mismatched execute must not mutate config; strategies = %v", got)
	}
}

func TestUIRemoveStrategyEndToEnd(t *testing.T) {
	ss, path, restarts := newStructuralTestServer(t)
	confirm := structuralConfirm(t, ss, "remove-strategy", "sma-btc", `{}`)
	if confirm.ConfirmPhrase != "sma-btc" {
		t.Fatalf("confirm phrase = %q, want sma-btc", confirm.ConfirmPhrase)
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "sma-btc", "remove-strategy")
	}
	body := fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w := mutationPost(ss, h, "/api/strategies/sma-btc/remove-strategy", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("remove-strategy status = %d, body %s", w.Code, w.Body.String())
	}
	if ids := configStrategyIDs(t, path); len(ids) != 1 || ids[0] != "hl-momentum-eth" {
		t.Fatalf("strategies after remove = %v, want [hl-momentum-eth]", ids)
	}
	if restarts.Load() != 0 {
		t.Fatalf("restarts = %d, want 0", restarts.Load())
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("config after remove fails validation: %v", err)
	}

	// Removing the last remaining strategy is refused at execute.
	ss.UpdateStrategies([]StrategyConfig{{ID: "hl-momentum-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}}})
	confirm = structuralConfirm(t, ss, "remove-strategy", "hl-momentum-eth", `{}`)
	h = func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "remove-strategy")
	}
	body = fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w = mutationPost(ss, h, "/api/strategies/hl-momentum-eth/remove-strategy", body, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "only strategy") {
		t.Fatalf("only-strategy removal status = %d body %s, want 409 refusal", w.Code, w.Body.String())
	}
	if ids := configStrategyIDs(t, path); len(ids) != 1 {
		t.Fatalf("refused removal must not mutate config; strategies = %v", ids)
	}
}

// The remove-strategy confirm dialog must warn when the target holds an open
// position (management stops after restart).
func TestUIRemoveStrategyConfirmWarnsOnOpenPosition(t *testing.T) {
	ss, _, _ := newStructuralTestServer(t)
	ss.state.Strategies["sma-btc"] = &StrategyState{
		Positions: map[string]*Position{"BTC/USDT": {Quantity: 0.5, AvgCost: 50000}},
	}
	confirm := structuralConfirm(t, ss, "remove-strategy", "sma-btc", `{}`)
	if !strings.Contains(confirm.Description, "OPEN position") {
		t.Fatalf("confirm description must warn about the open position: %s", confirm.Description)
	}
}

func TestUIPaperToLiveEndToEnd(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	confirm := structuralConfirm(t, ss, "paper-to-live", "hl-momentum-eth", `{}`)
	if !strings.Contains(confirm.Description, "REAL ORDERS WITH REAL FUNDS") {
		t.Fatalf("confirm description must carry the live-funds warning: %s", confirm.Description)
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "paper-to-live")
	}
	body := fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w := mutationPost(ss, h, "/api/strategies/hl-momentum-eth/paper-to-live", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("paper-to-live status = %d, body %s", w.Code, w.Body.String())
	}
	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "--mode=paper") || !strings.Contains(raw, "--mode=live") {
		t.Fatalf("args not flipped to live:\n%s", raw)
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("config after paper-to-live fails validation: %v", err)
	}

	// A second confirm on the now-live strategy fails early: the on-disk args
	// flipped, but the in-memory snapshot only refreshes on reload — mimic it.
	ss.UpdateStrategies([]StrategyConfig{
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
		{ID: "hl-momentum-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h", "--mode=live"}},
	})
	body = `{"action":"paper-to-live","strategy_id":"hl-momentum-eth","params":{}}`
	rec := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already live") {
		t.Fatalf("already-live confirm status = %d body %s, want 400 already-live", rec.Code, rec.Body.String())
	}
}

// Confirming a spot strategy for paper-to-live fails early: no --mode arg.
func TestUIPaperToLiveRejectsModelessStrategy(t *testing.T) {
	ss, _, _ := newStructuralTestServer(t)
	body := `{"action":"paper-to-live","strategy_id":"sma-btc","params":{}}`
	w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "no --mode=paper") {
		t.Fatalf("modeless confirm status = %d body %s, want 400 no-mode", w.Code, w.Body.String())
	}
}

func TestUIApplyRegimeGateEndToEnd(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	confirm := structuralConfirm(t, ss, "apply-regime-gate", "hl-momentum-eth", `{}`)
	if !strings.Contains(confirm.Description, defaultRegimeGatePresetName) {
		t.Fatalf("confirm description should name the preset: %s", confirm.Description)
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "apply-regime-gate")
	}
	body := fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w := mutationPost(ss, h, "/api/strategies/hl-momentum-eth/apply-regime-gate", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("apply-regime-gate status = %d, body %s", w.Code, w.Body.String())
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("config after apply-regime-gate fails validation: %v", err)
	}
	var target *StrategyConfig
	for i := range cfg.Strategies {
		if cfg.Strategies[i].ID == "hl-momentum-eth" {
			target = &cfg.Strategies[i]
		}
	}
	if target == nil || target.RegimeGateWindow != "comp_p21" || len(target.AllowedRegimes) == 0 {
		t.Fatalf("gate wiring missing on target: %+v", target)
	}
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		t.Fatal("regime detection should be enabled after apply")
	}
}

// The confirm must fail while the target holds an open position, and the
// execute must re-check flat even when the confirm succeeded (a position can
// open between confirm and execute).
func TestUIApplyRegimeGateFlatChecks(t *testing.T) {
	ss, _, _ := newStructuralTestServer(t)
	openPos := &StrategyState{Positions: map[string]*Position{"ETH": {Quantity: 1, AvgCost: 2000}}}

	// Open at confirm time → confirm refused.
	ss.state.Strategies["hl-momentum-eth"] = openPos
	body := `{"action":"apply-regime-gate","strategy_id":"hl-momentum-eth","params":{}}`
	w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "open position") {
		t.Fatalf("confirm-while-open status = %d body %s, want 400 open-position", w.Code, w.Body.String())
	}

	// Flat at confirm, opens before execute → execute refused.
	delete(ss.state.Strategies, "hl-momentum-eth")
	confirm := structuralConfirm(t, ss, "apply-regime-gate", "hl-momentum-eth", `{}`)
	ss.state.Strategies["hl-momentum-eth"] = openPos
	h := func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "apply-regime-gate")
	}
	execBody := fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w = mutationPost(ss, h, "/api/strategies/hl-momentum-eth/apply-regime-gate", execBody, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "opened a position") {
		t.Fatalf("execute-while-open status = %d body %s, want 409 opened-a-position", w.Code, w.Body.String())
	}
}

// A concurrent config edit between confirm and execute that GROWS the
// regime.enabled blast radius (another strategy's dormant allowed_regimes
// would newly activate) must refuse the write; shrinkage must not.
func TestUIApplyRegimeGateBlastRadiusGrowthRefused(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	confirm := structuralConfirm(t, ss, "apply-regime-gate", "hl-momentum-eth", `{}`)

	// Concurrent edit: give sma-btc a dormant allowed_regimes gate the
	// operator was never shown.
	root := readConfigRoot(t, path)
	list, err := configStrategies(root)
	if err != nil {
		t.Fatalf("configStrategies: %v", err)
	}
	for i, raw := range list {
		if strategyRawID(raw) != "sma-btc" {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("parse sma-btc: %v", err)
		}
		item["allowed_regimes"] = json.RawMessage(`["ranging"]`)
		nb, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal sma-btc: %v", err)
		}
		list[i] = nb
	}
	if err := setConfigStrategies(root, list); err != nil {
		t.Fatalf("setConfigStrategies: %v", err)
	}
	if err := writeValidatedConfigRoot(path, root); err != nil {
		t.Fatalf("write concurrent edit: %v", err)
	}

	h := func(w http.ResponseWriter, r *http.Request) {
		ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "apply-regime-gate")
	}
	body := fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce)
	w := mutationPost(ss, h, "/api/strategies/hl-momentum-eth/apply-regime-gate", body, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "blast radius") {
		t.Fatalf("blast-radius-growth status = %d body %s, want 409 refusal", w.Code, w.Body.String())
	}
	// The gate must NOT have been written.
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("config invalid after refusal: %v", err)
	}
	if cfg.Regime != nil && cfg.Regime.Enabled {
		t.Fatal("refused apply must not enable regime detection")
	}
}

// Ineligible strategy type refused at confirm.
func TestUIApplyRegimeGateRejectsIneligibleType(t *testing.T) {
	ss, _, _ := newStructuralTestServer(t)
	body := `{"action":"apply-regime-gate","strategy_id":"sma-btc","params":{}}`
	w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not eligible") {
		t.Fatalf("ineligible confirm status = %d body %s, want 400 not-eligible", w.Code, w.Body.String())
	}
}

// Structural writes and a concurrent Discord-style config set both serialize
// on configWriteMu without clobbering each other's fields.
func TestUIStructuralInterleavedWrites(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
				_, _, err := applyTopLevelConfigSet(root, "default_stop_loss_atr_mult", "1.5")
				return err
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
				_, _, err := applyTopLevelConfigSet(root, "interval_seconds", "120")
				return err
			})
		}
	}()
	wg.Wait()
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("config invalid after interleaved writes: %v", err)
	}
	if cfg.IntervalSeconds != 120 {
		t.Fatalf("interval_seconds = %d, want 120", cfg.IntervalSeconds)
	}
	if cfg.DefaultStopLossATRMult == nil || *cfg.DefaultStopLossATRMult != 1.5 {
		t.Fatalf("default_stop_loss_atr_mult = %v, want 1.5", cfg.DefaultStopLossATRMult)
	}
}
