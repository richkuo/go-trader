package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeStrategyTunerOverrides(t *testing.T) {
	base := StrategyConfig{
		ID:              "spot-btc",
		Type:            "spot",
		IntervalSeconds: 3600,
		OpenStrategy: StrategyRef{
			Name:   "sma",
			Params: map[string]interface{}{"period": 20},
		},
	}
	overrides := map[string]json.RawMessage{
		"interval_seconds":            json.RawMessage(`7200`),
		"open_strategy.params.period": json.RawMessage(`10`),
	}
	merged, err := mergeStrategyTunerOverrides(base, overrides)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.IntervalSeconds != 7200 {
		t.Fatalf("interval_seconds = %d, want 7200", merged.IntervalSeconds)
	}
	if merged.OpenStrategy.Params["period"] != float64(10) && merged.OpenStrategy.Params["period"] != 10 {
		t.Fatalf("period = %v, want 10", merged.OpenStrategy.Params["period"])
	}
}

func TestMergeStrategyTunerOverridesClearsSiblingStop(t *testing.T) {
	stop := 1.5
	base := StrategyConfig{
		ID:              "hl-btc",
		Type:            "perps",
		StopLossATRMult: &stop,
	}
	overrides := map[string]json.RawMessage{
		"stop_loss_pct": json.RawMessage(`2`),
	}
	merged, err := mergeStrategyTunerOverrides(base, overrides)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.StopLossPct == nil || *merged.StopLossPct != 2 {
		t.Fatalf("stop_loss_pct = %v, want 2", merged.StopLossPct)
	}
	if merged.StopLossATRMult != nil {
		t.Fatalf("stop_loss_atr_mult = %v, want nil", merged.StopLossATRMult)
	}
}

func TestBuildUIStrategyConfigFields(t *testing.T) {
	stop := 2.5
	sc := StrategyConfig{
		ID:              "hl-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Args:            []string{"triple_ema", "BTC", "1h"},
		IntervalSeconds: 3600,
		Direction:       DirectionLong,
		Leverage:        5,
		StopLossATRMult: &stop,
		OpenStrategy:    StrategyRef{Name: "triple_ema", Params: map[string]interface{}{"fast_period": 8}},
	}
	defaults := map[string]interface{}{"fast_period": 12, "slow_period": 26}
	resp := buildUIStrategyConfig(sc, defaults, "", false)
	if resp.OpenStrategy.Params["fast_period"] != 8 {
		t.Fatalf("merged fast_period = %v, want 8", resp.OpenStrategy.Params["fast_period"])
	}
	if resp.ApplyRequiresRestart {
		t.Fatal("expected apply_requires_restart=false with no overrides")
	}
	foundRuntime := false
	foundParam := false
	for _, field := range resp.EditableFields {
		if field.Key == "leverage" {
			foundRuntime = true
		}
		if field.Key == "open_strategy.params.fast_period" {
			foundParam = true
		}
	}
	if !foundRuntime || !foundParam {
		t.Fatalf("editable fields missing runtime/param: %+v", resp.EditableFields)
	}
}

func TestTunerApplyRequiresRestart(t *testing.T) {
	if !tunerApplyRequiresRestart(map[string]json.RawMessage{"htf_filter": json.RawMessage(`true`)}, false) {
		t.Fatal("htf_filter override should require restart")
	}
	if tunerApplyRequiresRestart(map[string]json.RawMessage{"interval_seconds": json.RawMessage(`60`)}, false) {
		t.Fatal("interval_seconds alone should not require restart")
	}
	if !tunerApplyRequiresRestart(map[string]json.RawMessage{"leverage": json.RawMessage(`5`)}, true) {
		t.Fatal("leverage override with open position should require restart")
	}
}

func TestApplyStrategyConfigPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "config_version": 14,
  "interval_seconds": 60,
  "strategies": [
    {
      "id": "spot-btc",
      "type": "spot",
      "platform": "binanceus",
      "script": "shared_scripts/check_strategy.py",
      "args": ["sma", "BTC/USDT", "1h"],
      "capital": 1000,
      "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 20, "slow_period": 50}}
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	merged := StrategyConfig{
		ID:              "spot-btc",
		Type:            "spot",
		IntervalSeconds: 7200,
		OpenStrategy: StrategyRef{
			Name:   "sma_crossover",
			Params: map[string]interface{}{"fast_period": 10, "slow_period": 50},
		},
	}
	overrides := map[string]json.RawMessage{
		"interval_seconds":                 json.RawMessage(`7200`),
		"open_strategy.params.fast_period": json.RawMessage(`10`),
	}
	restartRequired, err := applyStrategyConfigPatch(path, "spot-btc", merged, overrides, false)
	if err != nil {
		t.Fatalf("applyStrategyConfigPatch: %v", err)
	}
	if restartRequired {
		t.Fatal("expected restartRequired=false for interval/param overrides")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root struct {
		Strategies []map[string]interface{} `json:"strategies"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(root.Strategies) != 1 {
		t.Fatalf("strategies len = %d, want 1", len(root.Strategies))
	}
	if root.Strategies[0]["interval_seconds"] != float64(7200) {
		t.Fatalf("interval_seconds = %v, want 7200", root.Strategies[0]["interval_seconds"])
	}
	rawOpen, ok := root.Strategies[0]["open_strategy"].(map[string]interface{})
	if !ok {
		t.Fatalf("open_strategy = %v, want object", root.Strategies[0]["open_strategy"])
	}
	params, ok := rawOpen["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("open_strategy.params = %v, want object", rawOpen["params"])
	}
	if params["fast_period"] != float64(10) {
		t.Fatalf("fast_period = %v, want 10", params["fast_period"])
	}
}

func TestPatchStrategyJSONParamsPreserved(t *testing.T) {
	item := map[string]json.RawMessage{
		"open_strategy": mustRawJSON(t, map[string]interface{}{
			"name":   "sma_crossover",
			"params": map[string]interface{}{"fast_period": 20.0, "slow_period": 50.0},
		}),
	}
	merged := StrategyConfig{
		Type: "spot",
		OpenStrategy: StrategyRef{
			Name:   "sma_crossover",
			Params: map[string]interface{}{"fast_period": 10, "slow_period": 50},
		},
	}
	overrides := map[string]json.RawMessage{
		"open_strategy.params.fast_period": json.RawMessage(`10`),
	}
	patched, err := patchStrategyJSON(item, merged, overrides)
	if err != nil {
		t.Fatalf("patchStrategyJSON: %v", err)
	}
	var ref StrategyRef
	if err := json.Unmarshal(patched["open_strategy"], &ref); err != nil {
		t.Fatalf("unmarshal open_strategy: %v", err)
	}
	if ref.Params["fast_period"] != float64(10) {
		t.Fatalf("fast_period = %v, want 10", ref.Params["fast_period"])
	}
	if ref.Params["slow_period"] != float64(50) {
		t.Fatalf("slow_period = %v, want 50 preserved", ref.Params["slow_period"])
	}
}

func TestPatchStrategyJSONSkipsUntouched(t *testing.T) {
	item := map[string]json.RawMessage{
		"invert_signal": mustRawJSON(t, true),
		"htf_filter":    mustRawJSON(t, false),
	}
	merged := StrategyConfig{
		Type:         "perps",
		InvertSignal: false,
		HTFFilter:    true,
		Leverage:     3,
	}
	overrides := map[string]json.RawMessage{
		"leverage": json.RawMessage(`3`),
	}
	patched, err := patchStrategyJSON(item, merged, overrides)
	if err != nil {
		t.Fatalf("patchStrategyJSON: %v", err)
	}
	var invert bool
	if err := json.Unmarshal(patched["invert_signal"], &invert); err != nil || !invert {
		t.Fatalf("invert_signal = %v, want true preserved", invert)
	}
	if _, ok := patched["htf_filter"]; !ok {
		t.Fatal("htf_filter should remain untouched")
	}
	var lev float64
	if err := json.Unmarshal(patched["leverage"], &lev); err != nil || lev != 3 {
		t.Fatalf("leverage = %v, want 3", lev)
	}
}

func TestRequireMutatingAPIAuth(t *testing.T) {
	// #1256 (#1229 security model): unset status_token no longer blocks
	// mutations — the dashboard is loopback-only and unauthenticated by design.
	ss := NewStatusServer(NewAppState(), nil, "", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/x/config", nil)
	w := httptest.NewRecorder()
	if !ss.requireMutatingAPIAuth(w, req) {
		t.Fatal("expected auth success when status_token unset")
	}

	// A configured token is still enforced.
	ss = NewStatusServer(NewAppState(), nil, "secret", nil, nil)
	w = httptest.NewRecorder()
	if ss.requireMutatingAPIAuth(w, req) {
		t.Fatal("expected auth failure without bearer token when configured")
	}
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	if !ss.requireMutatingAPIAuth(w, req) {
		t.Fatal("expected auth success with matching token")
	}
}

func TestSimulateConfigPayloadOpenFallback(t *testing.T) {
	sc := StrategyConfig{
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h"},
	}
	payload := simulateConfigPayload(sc, nil)
	if payload["strategy"] != "sma" {
		t.Fatalf("strategy = %v, want sma", payload["strategy"])
	}
	openRef := payload["open_strategy"].(StrategyRef)
	if openRef.Name != "sma" {
		t.Fatalf("open_strategy.name = %q, want sma", openRef.Name)
	}
}

func mustRawJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
