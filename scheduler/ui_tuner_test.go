package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestMergeStrategyTunerOverrides(t *testing.T) {
	t.Run("MergeStrategyTunerOverrides", func(t *testing.T) {
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
	})
	t.Run("MergeStrategyTunerOverridesClearsSiblingStop", func(t *testing.T) {
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
	})
}

func TestTunerStrategyConfigPatch(t *testing.T) {
	t.Run("BuildUIStrategyConfigFields", func(t *testing.T) {
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
	})
	t.Run("TunerApplyRequiresRestart", func(t *testing.T) {
		if !tunerApplyRequiresRestart(map[string]json.RawMessage{"htf_filter": json.RawMessage(`true`)}, false) {
			t.Fatal("htf_filter override should require restart")
		}
		if tunerApplyRequiresRestart(map[string]json.RawMessage{"interval_seconds": json.RawMessage(`60`)}, false) {
			t.Fatal("interval_seconds alone should not require restart")
		}
		if !tunerApplyRequiresRestart(map[string]json.RawMessage{"leverage": json.RawMessage(`5`)}, true) {
			t.Fatal("leverage override with open position should require restart")
		}
	})
	t.Run("ApplyStrategyConfigPatch", func(t *testing.T) {
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
	})
	t.Run("PatchStrategyJSONParamsPreserved", func(t *testing.T) {
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
	})
	t.Run("PatchStrategyJSONSkipsUntouched", func(t *testing.T) {
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
	})
	t.Run("ApplyRefusesADifferentOpenStrategyName", func(t *testing.T) {
		ss, path := newTunerApplyServer(t, tunerApplyNamedConfig)
		for _, name := range []string{"rsi", "SMA_CROSSOVER", "sma crossover"} {
			body := `{"overrides":{"open_strategy":{"name":"` + name + `","params":{"fast_period":7}}}}`
			before := snapshotTunerApplyState(t, ss, path)
			w := postTunerApply(ss, "spot-btc", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("name %q: status = %d, want 400 (body %s)", name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), name) || !strings.Contains(w.Body.String(), "sma_crossover") {
				t.Fatalf("name %q: refusal must name both strategies, got %s", name, w.Body.String())
			}
			before.assertUnchanged(t, ss, path)
		}
	})
	t.Run("ApplyAcceptsTheCurrentNameAndParameterOnlyEdits", func(t *testing.T) {
		cases := []struct {
			name  string
			body  string
			key   string
			value float64
		}{
			{"nested current name", `{"overrides":{"open_strategy":{"name":"sma_crossover","params":{"fast_period":11}}}}`, "fast_period", 11},
			{"current name with surrounding spaces", `{"overrides":{"open_strategy":{"name":"  sma_crossover ","params":{"fast_period":12}}}}`, "fast_period", 12},
			{"nested parameter only", `{"overrides":{"open_strategy":{"params":{"slow_period":61}}}}`, "slow_period", 61},
			{"blank name", `{"overrides":{"open_strategy":{"name":"   ","params":{"slow_period":62}}}}`, "slow_period", 62},
			{"dotted", `{"overrides":{"open_strategy.params.slow_period":63}}`, "slow_period", 63},
		}
		for _, tc := range cases {
			ss, path := newTunerApplyServer(t, tunerApplyNamedConfig)
			loaded := marshalLoadedTunerParams(t, ss)
			w := postTunerApply(ss, "spot-btc", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 (body %s)", tc.name, w.Code, w.Body.String())
			}
			open := readTunerApplyOpenStrategy(t, path)
			if open.Name != "sma_crossover" {
				t.Fatalf("%s: saved name = %q, want sma_crossover", tc.name, open.Name)
			}
			if open.Params[tc.key] != tc.value {
				t.Fatalf("%s: %s = %v, want %v", tc.name, tc.key, open.Params[tc.key], tc.value)
			}
			if got := marshalLoadedTunerParams(t, ss); got != loaded {
				t.Fatalf("%s: loaded params changed before a reload: %s -> %s", tc.name, loaded, got)
			}
		}
	})
	t.Run("ApplyKeepsEveryEditBeforeAReload", func(t *testing.T) {
		ss, path := newTunerApplyServer(t, tunerApplyNamedConfig)
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy.params.fast_period":9}}`); w.Code != http.StatusOK {
			t.Fatalf("first edit status = %d (body %s)", w.Code, w.Body.String())
		}
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy":{"name":"sma_crossover","params":{"slow_period":70}}}}`); w.Code != http.StatusOK {
			t.Fatalf("second edit status = %d (body %s)", w.Code, w.Body.String())
		}
		open := readTunerApplyOpenStrategy(t, path)
		if open.Name != "sma_crossover" {
			t.Fatalf("saved name = %q, want sma_crossover", open.Name)
		}
		if open.Params["fast_period"] != float64(9) {
			t.Fatalf("fast_period = %v, want 9 kept from the first edit", open.Params["fast_period"])
		}
		if open.Params["slow_period"] != float64(70) {
			t.Fatalf("slow_period = %v, want 70", open.Params["slow_period"])
		}
	})
	t.Run("ApplyNameFromScriptArgs", func(t *testing.T) {
		ss, path := newTunerApplyServer(t, tunerApplyArgsOnlyConfig)
		w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy":{"name":"rsi","params":{"fast_period":7}}}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("different name status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy.params.fast_period":7}}`); w.Code != http.StatusOK {
			t.Fatalf("dotted edit status = %d (body %s)", w.Code, w.Body.String())
		}
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy":{"params":{"slow_period":40}}}}`); w.Code != http.StatusOK {
			t.Fatalf("nested edit status = %d (body %s)", w.Code, w.Body.String())
		}
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy":{"name":"sma_crossover","params":{"fast_period":8}}}}`); w.Code != http.StatusOK {
			t.Fatalf("named edit status = %d (body %s)", w.Code, w.Body.String())
		}
		open := readTunerApplyOpenStrategy(t, path)
		if open.Name != "sma_crossover" {
			t.Fatalf("saved name = %q, want sma_crossover from args[0]", open.Name)
		}
		if open.Params["fast_period"] != float64(8) {
			t.Fatalf("fast_period = %v, want 8", open.Params["fast_period"])
		}
		if open.Params["slow_period"] != float64(40) {
			t.Fatalf("slow_period = %v, want 40 kept", open.Params["slow_period"])
		}
		args, _ := readConfigStrategy(t, path)["args"].([]interface{})
		if len(args) != 3 || args[0] != "sma_crossover" {
			t.Fatalf("args = %v, want unchanged", args)
		}
		cfg, err := LoadConfigReadOnly(path)
		if err != nil {
			t.Fatalf("patched config must load: %v", err)
		}
		if got := effectiveOpenStrategy(cfg.Strategies[0]); got != "sma_crossover" {
			t.Fatalf("effective name after edits = %q, want sma_crossover", got)
		}
	})
	t.Run("ApplyRefusesAStaleSnapshot", func(t *testing.T) {
		ss, path := newTunerApplyServer(t, tunerApplyNamedConfig)
		stale := strings.Replace(tunerApplyNamedConfig, `"name": "sma_crossover"`, `"name": "ema_crossover"`, 1)
		if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
			t.Fatalf("rewrite config: %v", err)
		}
		for _, body := range []string{
			`{"overrides":{"open_strategy":{"name":"sma_crossover","params":{"fast_period":9}}}}`,
			`{"overrides":{"open_strategy":{"params":{"fast_period":9}}}}`,
			`{"overrides":{"open_strategy.params.fast_period":9}}`,
		} {
			before := snapshotTunerApplyState(t, ss, path)
			w := postTunerApply(ss, "spot-btc", body)
			if w.Code != http.StatusConflict {
				t.Fatalf("%s: status = %d, want 409 (body %s)", body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "sma_crossover") || !strings.Contains(w.Body.String(), "ema_crossover") {
				t.Fatalf("%s: stale refusal must name both strategies, got %s", body, w.Body.String())
			}
			before.assertUnchanged(t, ss, path)
		}
		before := snapshotTunerApplyState(t, ss, path)
		w := postTunerApply(ss, "spot-btc", `{"overrides":{"open_strategy":{"name":"ema_crossover","params":{"fast_period":9}}}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("file name request: status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		before.assertUnchanged(t, ss, path)
		if w := postTunerApply(ss, "spot-btc", `{"overrides":{"interval_seconds":120}}`); w.Code != http.StatusOK {
			t.Fatalf("runtime-only edit on a stale file: status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
	})
	t.Run("ApplyConcurrentEditsKeepEveryParameter", func(t *testing.T) {
		ss, path := newTunerApplyServer(t, tunerApplyNamedConfig)
		var wg sync.WaitGroup
		codes := make([]int, 8)
		for i := 0; i < len(codes); i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := "fast_period"
				if i%2 == 1 {
					key = "slow_period"
				}
				body := `{"overrides":{"open_strategy.params.` + key + `":` + strconv.Itoa(100+i) + `}}`
				codes[i] = postTunerApply(ss, "spot-btc", body).Code
			}(i)
		}
		wg.Wait()
		for i, code := range codes {
			if code != http.StatusOK {
				t.Fatalf("request %d status = %d, want 200", i, code)
			}
		}
		if _, err := LoadConfigReadOnly(path); err != nil {
			t.Fatalf("config must load after concurrent edits: %v", err)
		}
		open := readTunerApplyOpenStrategy(t, path)
		if open.Name != "sma_crossover" {
			t.Fatalf("saved name = %q, want sma_crossover", open.Name)
		}
		fast, _ := open.Params["fast_period"].(float64)
		slow, _ := open.Params["slow_period"].(float64)
		if fast < 100 || slow < 100 {
			t.Fatalf("a concurrent edit was lost: fast_period %v slow_period %v", open.Params["fast_period"], open.Params["slow_period"])
		}
	})
}

const tunerApplyNamedConfig = `{
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
}
`

const tunerApplyArgsOnlyConfig = `{
  "config_version": 14,
  "interval_seconds": 60,
  "strategies": [
    {
      "id": "spot-btc",
      "type": "spot",
      "platform": "binanceus",
      "script": "shared_scripts/check_strategy.py",
      "args": ["sma_crossover", "BTC/USDT", "1h"],
      "capital": 1000
    }
  ]
}
`

func newTunerApplyServer(t *testing.T, body string) (*StatusServer, string) {
	t.Helper()
	path := writeTestConfig(t, t.TempDir(), body)
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", cfg.Strategies, nil)
	ss.SetConfigContext(path, cfg)
	return ss, path
}

func postTunerApply(ss *StatusServer, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+id+"/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ss.handleAPIStrategy(w, req)
	return w
}

func marshalLoadedTunerParams(t *testing.T, ss *StatusServer) string {
	t.Helper()
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	raw, err := json.Marshal(ss.strategies[0].OpenStrategy)
	if err != nil {
		t.Fatalf("marshal loaded open strategy: %v", err)
	}
	return string(raw)
}

func readTunerApplyOpenStrategy(t *testing.T, path string) StrategyRef {
	t.Helper()
	raw, err := json.Marshal(readConfigStrategy(t, path)["open_strategy"])
	if err != nil {
		t.Fatalf("marshal open_strategy: %v", err)
	}
	var ref StrategyRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("decode open_strategy: %v", err)
	}
	return ref
}

type tunerApplyState struct {
	data   []byte
	info   os.FileInfo
	loaded string
}

func snapshotTunerApplyState(t *testing.T, ss *StatusServer, path string) tunerApplyState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	return tunerApplyState{data: data, info: info, loaded: marshalLoadedTunerParams(t, ss)}
}

func (s tunerApplyState) assertUnchanged(t *testing.T, ss *StatusServer, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(data, s.data) {
		t.Fatalf("config bytes changed on a refusal:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !os.SameFile(info, s.info) {
		t.Fatal("config file was replaced on a refusal")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("config dir holds more than config.json after a refusal: %v", entries)
	}
	if got := marshalLoadedTunerParams(t, ss); got != s.loaded {
		t.Fatalf("loaded open strategy changed on a refusal: %s -> %s", s.loaded, got)
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
	sc.RegimeGateWindow = "fast"
	sc.RegimeDirectionalWindow = "slow"
	sc.RegimeGateOnFailure = "closed"
	payload = simulateConfigPayload(sc, &RegimeConfig{Enabled: true, Timeframe: "4h", GateOnFailure: "open"})
	if payload["regime_gate_window"] != "fast" || payload["regime_directional_window"] != "slow" || payload["regime_gate_on_failure"] != "closed" {
		t.Fatalf("window selectors missing: %#v", payload)
	}
	regimePayload := payload["regime"].(map[string]interface{})
	if regimePayload["timeframe"] != "4h" || regimePayload["gate_on_failure"] != "open" {
		t.Fatalf("regime payload = %#v", regimePayload)
	}
}

func TestFetchStrategyDefaultParamsCachesSchemaLoad(t *testing.T) {
	clearStrategySchemaCacheForTest()
	t.Cleanup(clearStrategySchemaCacheForTest)

	loads := 0
	load := func(sc StrategyConfig) (map[string]interface{}, string, error) {
		loads++
		return map[string]interface{}{
			"fast": float64(10),
			"id":   sc.ID,
		}, "desc-" + effectiveOpenStrategy(sc), nil
	}

	scA := StrategyConfig{ID: "a", Type: "perps", OpenStrategy: StrategyRef{Name: "momentum"}}
	scB := StrategyConfig{ID: "b", Type: "perps", OpenStrategy: StrategyRef{Name: "momentum"}}
	scC := StrategyConfig{ID: "c", Type: "perps", OpenStrategy: StrategyRef{Name: "sma"}}

	first, desc, err := fetchStrategyDefaultParamsWith(scA, load)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if loads != 1 || desc != "desc-momentum" || first["fast"] != float64(10) {
		t.Fatalf("first load unexpected: loads=%d desc=%q first=%v", loads, desc, first)
	}

	second, desc2, err := fetchStrategyDefaultParamsWith(scB, load)
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if loads != 1 {
		t.Fatalf("cached load still invoked schema loader: loads=%d", loads)
	}
	if desc2 != "desc-momentum" || second["fast"] != float64(10) {
		t.Fatalf("cached values = desc=%q params=%v", desc2, second)
	}

	second["fast"] = float64(99)
	third, _, err := fetchStrategyDefaultParamsWith(scA, load)
	if err != nil {
		t.Fatalf("post-mutation load: %v", err)
	}
	if loads != 1 || third["fast"] != float64(10) {
		t.Fatalf("cache mutated via caller map: loads=%d third=%v", loads, third)
	}

	other, otherDesc, err := fetchStrategyDefaultParamsWith(scC, load)
	if err != nil {
		t.Fatalf("other strategy load: %v", err)
	}
	if loads != 2 || otherDesc != "desc-sma" {
		t.Fatalf("other strategy cache miss failed: loads=%d desc=%q", loads, otherDesc)
	}
	if other["fast"] != float64(10) {
		t.Fatalf("other strategy params = %v", other)
	}

	empty, emptyDesc, err := fetchStrategyDefaultParamsWith(StrategyConfig{Type: "spot"}, load)
	if err != nil {
		t.Fatalf("empty open name: %v", err)
	}
	if loads != 2 || emptyDesc != "" || len(empty) != 0 {
		t.Fatalf("empty open name should skip loader: loads=%d desc=%q empty=%v", loads, emptyDesc, empty)
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
