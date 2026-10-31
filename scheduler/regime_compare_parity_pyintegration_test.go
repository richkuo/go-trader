//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// regimeCompareParityDriver checks the comparison resolvers against the live
// Go resolvers. The comparison must not grow a second definition of window
// selection, lookback, or certification identity.
const regimeCompareParityDriver = `import json
import sys

root = sys.argv[1]
sys.path.insert(0, root + "/backtest")
sys.path.insert(0, root + "/shared_tools")

from directional_certification import directional_cert_identity
from regime import (
    classifier_for_window,
    required_ohlcv_limit,
    required_regime_bars,
    resolve_strategy_regime_window,
    valid_labels_for_classifier,
)

req = json.load(sys.stdin)
out = []
fields = ("gate", "directional", "atr")
for case in req["cases"]:
    regime = case["regime"]
    strategy = case["strategy"]
    windows = {field: resolve_strategy_regime_window(strategy, field, regime) for field in fields}
    vocabularies = {
        field: sorted(valid_labels_for_classifier(classifier_for_window(regime, windows[field])))
        for field in fields
    }
    out.append({
        "window": resolve_strategy_regime_window(strategy, case["field"], regime),
        "limit": required_ohlcv_limit(int(regime.get("period") or 14), regime.get("windows") or None),
        "identity": directional_cert_identity(strategy, regime),
        "needs": {name: {"adx": required_regime_bars(spec if isinstance(spec, int) else spec["period"], "adx"),
                          "composite": required_regime_bars(spec if isinstance(spec, int) else spec["period"], "composite")}
                  for name, spec in (regime.get("windows") or {}).items()},
        "windows": windows,
        "vocabularies": vocabularies,
    })
json.dump(out, sys.stdout)
`

type regimeParityCase struct {
	Field    string
	Strategy StrategyConfig
	Regime   *RegimeConfig
	Python   map[string]any
}

func TestRegimeCompareResolversMatchLive(t *testing.T) {
	windows := RegimeWindowsMap{
		"short":  {Classifier: "adx", Period: 8, ADXThreshold: 20},
		"medium": {Classifier: "composite", Period: 28},
	}
	multi := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, Windows: windows}
	off := &RegimeConfig{Enabled: false, Period: 14}
	longPeriod := &RegimeConfig{Enabled: true, Period: 120}
	longComposite := &RegimeConfig{Enabled: true, Period: 14, Windows: RegimeWindowsMap{"medium": {Classifier: "composite", Period: 120}}}
	override := &RegimeConfig{Enabled: true, Period: 14, Timeframe: "4H", Windows: windows}
	cases := []regimeParityCase{
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: multi},
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeGateWindow: "Short"}, Regime: multi},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeDirectionalWindow: "short"}, Regime: off},
		{Field: "atr", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: off},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC/USDT", "1h"}}, Regime: multi},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeDirectionalWindow: "short"}, Regime: override},
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: longPeriod},
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: longComposite},
	}
	payload := map[string]any{"cases": []map[string]any{}}
	for _, c := range cases {
		body, err := json.Marshal(c.Regime)
		if err != nil {
			t.Fatal(err)
		}
		var regime map[string]any
		if err := json.Unmarshal(body, &regime); err != nil {
			t.Fatal(err)
		}
		strategy := map[string]any{
			"args":                      c.Strategy.Args,
			"regime_gate_window":        c.Strategy.RegimeGateWindow,
			"regime_atr_window":         c.Strategy.RegimeATRWindow,
			"regime_directional_window": c.Strategy.RegimeDirectionalWindow,
		}
		payload["cases"] = append(payload["cases"].([]map[string]any), map[string]any{
			"field": c.Field, "strategy": strategy, "regime": regime,
		})
	}
	got := runRegimeCompareParityDriver(t, payload)
	if len(got) != len(cases) {
		t.Fatalf("driver returned %d cases, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		window := resolveStrategyRegimeWindow(c.Strategy, c.Field, c.Regime)
		if got[i].Window != window {
			t.Fatalf("case %d window: python %q go %q", i, got[i].Window, window)
		}
		limit := regimeRequiredOhlcvLimit(c.Regime)
		if got[i].Limit != limit {
			t.Fatalf("case %d lookback: python %d go %d", i, got[i].Limit, limit)
		}
		for name, spec := range c.Regime.Windows {
			for _, classifier := range []string{"adx", "composite"} {
				if got[i].Needs[name][classifier] != regimeRequiredBars(spec.Period, classifier) {
					t.Fatalf("%s %s need differs", name, classifier)
				}
			}
		}
		asset, tf, classifier, ok := directionalCertIdentity(c.Strategy, c.Regime)
		if !ok {
			t.Fatalf("case %d: Go identity did not resolve", i)
		}
		if got[i].Identity.Asset != asset || got[i].Identity.Timeframe != tf || got[i].Identity.Classifier != classifier {
			t.Fatalf("case %d identity: python %+v go %s %s %s", i, got[i].Identity, asset, tf, classifier)
		}
		for _, field := range []string{"gate", "directional", "atr"} {
			wantWindow := resolveStrategyRegimeWindow(c.Strategy, field, c.Regime)
			if got[i].Windows[field] != wantWindow {
				t.Fatalf("case %d %s window: python %q go %q", i, field, got[i].Windows[field], wantWindow)
			}
			wantLabels := append([]string(nil), regimeLabelsForStrategyWindow(c.Strategy, c.Regime, field)...)
			sort.Strings(wantLabels)
			if strings.Join(got[i].Vocabularies[field], ",") != strings.Join(wantLabels, ",") {
				t.Fatalf("case %d %s vocabulary: python %v go %v", i, field, got[i].Vocabularies[field], wantLabels)
			}
		}
	}
}

type regimeParityIdentity struct {
	Asset      string `json:"asset"`
	Timeframe  string `json:"timeframe"`
	Classifier string `json:"classifier"`
}

type regimeParityResult struct {
	Needs        map[string]map[string]int `json:"needs"`
	Window       string                    `json:"window"`
	Limit        int                       `json:"limit"`
	Identity     regimeParityIdentity      `json:"identity"`
	Windows      map[string]string         `json:"windows"`
	Vocabularies map[string][]string       `json:"vocabularies"`
}

func runRegimeCompareParityDriver(t *testing.T, req any) []regimeParityResult {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "regime_compare_parity_driver.py")
	if err := os.WriteFile(driver, []byte(regimeCompareParityDriver), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	cmd := exec.Command(python, "-I", driver, root)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("parity driver: %v\n%s", err, stderr.String())
	}
	var out []regimeParityResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse driver output: %v\n%s", err, stdout.String())
	}
	return out
}

const regimeReadinessIntegrationDriver = `import json, sys, subprocess
import numpy as np
import pandas as pd
root = sys.argv[1]
sys.path[:0] = [root, root + "/shared_tools", root + "/backtest"]
from regime import classify_window, compute_regime, compute_regime_composite, required_regime_bars, required_ohlcv_limit, prepare_check_regime, ensure_regime_columns
from backtest.regime_label_columns import resolve_feature_windows, attach_regime_label_columns, overlay_regime_columns
from backtest.regime_hmm import fit_label_anchored_hmm
from backtest.regime_diagnostics import score_labels
x = pd.Series(np.arange(1, 150, dtype=float))
df = pd.DataFrame(dict(open=x, high=x+1, low=x-1, close=x, volume=1000))
for classifier, periods in (("adx", (14,50)), ("composite", (14,21,30,60))):
    for period in periods:
        need = required_regime_bars(period, classifier)
        series = compute_regime(df, period) if classifier == "adx" else compute_regime_composite(df, period)
        for i in range(len(df)):
            snap = classify_window(df.iloc[:i+1], dict(classifier=classifier, period=period))
            assert snap["ready"] == (i+1 >= need)
            assert snap["regime"] == series.iloc[i]["regime"]
            assert snap["ready"] == bool(series.iloc[i]["regime_ready"])
        assert not classify_window(df.iloc[:need-1], dict(classifier=classifier,period=period))["regime"]
        assert classify_window(df.iloc[:need], dict(classifier=classifier,period=period))["regime"]
assert required_ohlcv_limit(14, {"long": {"classifier":"composite", "period":120}}) >= 251
standalone = ensure_regime_columns(df.copy(), classifier="composite", period=30)
assert standalone.iloc[-1]["regime"].startswith("trending_up_")
spec = {"medium":{"classifier":"composite","period":30}, "long":{"classifier":"adx","period":50}}
plan = resolve_feature_windows({"regime_atr_window":"long"}, {"enabled":True,"windows":spec})
prepared, columns = attach_regime_label_columns(df.copy(), plan)
overlay = overlay_regime_columns(prepared, df.copy())
assert prepared["regime_w_long_ready"].dtype == bool
assert prepared["regime_w_long_bars"].dtype.kind in "iu"
assert not prepared.iloc[99]["regime_w_long_ready"] and prepared.iloc[100]["regime_w_long_ready"]
assert overlay["regime_w_long_ready"].equals(prepared["regime_w_long_ready"])
report = score_labels(np.arange(1,11,dtype=float), np.array([""]*10), np.full((10,4), np.nan), n_perm=2)
assert report["coverage"] == {} and report["stability"]["flips"] == 0
try:
    fit_label_anchored_hmm(np.full((10,4), np.nan), [""]*10, ["trending_up_clean"], filter_window=3)
except ValueError:
    pass
else:
    raise AssertionError("cold model fitting was accepted")
rows = [[1700000000000+i*3600000, float(i+1), float(i+2), float(i), float(i+1),1000] for i in range(101)]
out = []
for n in (1,30,100,101):
    frame = {"rows": rows[:n], "required":251, "bars":n, "ready":True, "source":"ws", "forming_bar_included":True}
    payload = {"v":2,"market":{"version":4,"snapshot_id":"readiness", "frames":{"BTC|1h":frame},"mids":{},"feed_complete":True}}
    proc = subprocess.run([sys.executable,root+"/shared_scripts/check_regime.py", "--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h", "--market-stdin", "--min-bars=1000", "--regime-windows-spec-json", json.dumps(spec)], input=json.dumps(payload), capture_output=True,text=True,timeout=30)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    bundle = json.loads(proc.stdout)
    assert bundle["regime"]["long"]["ready"] == (n>=101)
    assert bundle["views"]["medium"]["adx3"]["ready"] == (n>=61)
    assert bundle["views"]["medium"]["composite7"]["ready"] == (n>=30)
    injected, live, strategy = prepare_check_regime(df,regime_enabled=True,windows_spec=spec,atr_window="long",injected_payload_json=json.dumps(bundle["regime"]))
    assert live == bundle["regime"]["long"]["regime"]
    out.append(bundle)
for edit in ("need", "label", "missing", "null"):
    injected = json.loads(json.dumps(bundle["regime"]))
    if edit == "need": injected["medium"]["need"] = 29
    if edit == "label": injected["medium"]["regime"] = "trending_up"
    if edit == "missing": del injected["medium"]
    if edit == "null": injected["medium"] = None
    normalized, live, strategy = prepare_check_regime(df,regime_enabled=True,windows_spec=spec,injected_payload_json=json.dumps(injected))
    assert not normalized["medium"].get("ready",False) and not strategy["regime"]
    assert normalized["long"]["ready"]
_, live, strategy = prepare_check_regime(df,regime_enabled=True,period=50,injected_payload_json=json.dumps(dict(regime="trending_up",ready=True,bars=101,need=29)))
assert not live and not strategy["regime"]
json.dump(out, sys.stdout, allow_nan=False)
`

func TestRegimeReadinessContractIntegration(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-c", regimeReadinessIntegrationDriver, root)
	raw, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			t.Fatalf("classifier integration: %v: %s", err, e.Stderr)
		}
		t.Fatal(err)
	}
	var outputs []json.RawMessage
	if err := json.Unmarshal(raw, &outputs); err != nil {
		t.Fatal(err)
	}
	spec := RegimeWindowsMap{"medium": {Classifier: "composite", Period: 30}, "long": {Classifier: "adx", Period: 50}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: string(specJSON)}
	rc := &RegimeConfig{Enabled: true, Windows: spec}
	var bundles []*RegimeBundle
	for _, raw := range outputs {
		bundle, err := parseRegimeBundleOutput(key, raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, bundle)
	}
	if len(bundles) != 4 {
		t.Fatal("missing script bundles")
	}
	sc := StrategyConfig{ID: "readiness", Platform: "hyperliquid", Type: "perps", Symbol: "BTC", AllowedRegimes: []string{"trending_up_clean"}, RegimeGateWindow: "medium"}
	for _, policy := range []string{"open", "closed"} {
		sc.RegimeGateOnFailure = policy
		if regimeDependencyHold(sc, bundles[1].Payload, bundles[1].Payload, rc, false) != "" {
			t.Fatal("unrelated cold long window held ready gate")
		}
		sc.RegimeGateWindow = "long"
		if regimeDependencyHold(sc, bundles[2].Payload, bundles[2].Payload, rc, false) == "" {
			t.Fatal("cold gate admitted increase")
		}
		if regimeDependencyHold(sc, bundles[3].Payload, bundles[2].Payload, rc, false) == "" {
			t.Fatal("current ready snapshot rescued a cold decision snapshot")
		}
		sc.RegimeGateWindow = "medium"
	}
	singleRC := &RegimeConfig{Enabled: true, Period: 50}
	singleKey := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: `{"default":{"classifier":"adx","period":50}}`}
	for i, original := range []*RegimeBundle{bundles[2], bundles[3]} {
		raw, err := json.Marshal(map[string]any{"regime": map[string]RegimeSnapshot{"default": original.Payload.Windows["long"]}})
		if err != nil {
			t.Fatal(err)
		}
		single, err := parseRegimeBundleOutput(singleKey, raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		singleStrategy := StrategyConfig{AllowedRegimes: []string{"trending_up"}}
		hold := regimeDependencyHold(singleStrategy, single.Payload, single.Payload, singleRC, false)
		if (hold == "") != (i == 1) {
			t.Fatalf("single-window readiness: %q", hold)
		}
		captured := entryRegimeSnapshots(single.Payload, singleRC)
		if captured[regimeWindowReservedName].Ready != (i == 1) {
			t.Fatal("single-window entry snapshot lost readiness")
		}
	}

	collisionRC := &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{"medium": spec["medium"], "default": spec["long"]}}
	collision := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{"medium": bundles[3].Payload.Windows["medium"], "default": bundles[2].Payload.Windows["long"]}}
	captured := entryRegimeSnapshots(collision, collisionRC)
	if !reflect.DeepEqual(captured["default"], collision.Windows["default"]) || positionATRRegimeLabel(&Position{RegimeSnapshots: captured}, StrategyConfig{}) != collision.Windows["medium"].Regime {
		t.Fatal("entry primary alias overwrote a configured default window")
	}
	metricSource := RegimePayload{MultiMode: true, Windows: cloneRegimeSnapshots(bundles[3].Payload.Windows)}
	metricCapture := entryRegimeSnapshots(metricSource, rc)
	metricBefore := cloneRegimeSnapshots(metricCapture)
	metricSource.Windows["medium"].Metrics["adx"] = -1
	if !reflect.DeepEqual(metricBefore, metricCapture) {
		t.Fatal("current metric mutation changed captured entry evidence")
	}
	regimeReadinessOnceIntegration(t)
	regimeReadinessAdmissionIntegration(t, bundles, rc)
	regimeReadinessStopBackfillIntegration(t, bundles, rc)
	regimeReadinessClassifierCaseIntegration(t, root)
	state := &StrategyState{Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 100}}, Regime: "stale"}
	syncStrategyRegimeState(state, bundles[3].Payload, rc)
	syncStrategyRegimeState(state, bundles[0].Payload, rc)
	if state.Regime != "" || state.RegimeWindows["long"] != "" {
		t.Fatal("cold result retained current label")
	}
	stampPositionRegimeFromPayload(state, "BTC", bundles[0].Payload, sc, rc)
	before := cloneRegimeSnapshots(state.Positions["BTC"].RegimeSnapshots)
	stampPositionRegimeFromPayload(state, "BTC", bundles[3].Payload, sc, rc)
	if !reflect.DeepEqual(before, state.Positions["BTC"].RegimeSnapshots) || state.Positions["BTC"].Regime != "" {
		t.Fatal("entry evidence changed")
	}
	db := openTestDB(t)
	action := PendingManualAction{StrategyID: sc.ID, Action: "open", Symbol: "BTC", Side: "long", Quantity: 1, FillPrice: 100, CreatedAt: time.Now(), RegimeSnapshots: before}
	if err := db.InsertPendingManualAction(action); err != nil {
		t.Fatal(err)
	}
	actions, err := db.LoadPendingManualActions()
	if err != nil || len(actions) != 1 || !reflect.DeepEqual(actions[0].RegimeSnapshots, before) {
		t.Fatalf("queued snapshots: %v %+v", err, actions)
	}
	for _, invalid := range []string{`null`, `{"regime":"trending_up"}`, `{"regime":"trending_up","ready":"true","bars":100,"need":101}`, `{"regime":"trending_up","ready":true,"bars":100,"need":101}`, `{"regime":"trending_up","ready":true,"bars":101,"need":29}`, `{"regime":"","ready":false,"bars":101,"need":101}`} {
		var out map[string]json.RawMessage
		if err := json.Unmarshal(outputs[3], &out); err != nil {
			t.Fatal(err)
		}
		var windows map[string]json.RawMessage
		if err := json.Unmarshal(out["regime"], &windows); err != nil {
			t.Fatal(err)
		}
		windows["long"] = json.RawMessage(invalid)
		out["regime"], _ = json.Marshal(windows)
		changed, _ := json.Marshal(out)
		bundle, err := parseRegimeBundleOutput(key, changed, time.Now())
		if err != nil || bundle.Payload.Windows["long"].Available || !bundle.Payload.Windows["medium"].Ready {
			t.Fatalf("invalid window escaped isolation: %v %+v", err, bundle)
		}
		var normalized RegimePayload
		if err := json.Unmarshal([]byte(bundle.RawRegimeJSON), &normalized); err != nil || normalized.Windows["long"].Regime != "" {
			t.Fatal("rejected label returned through injection")
		}
	}
}

func regimeReadinessAdmissionIntegration(t *testing.T, bundles []*RegimeBundle, rc *RegimeConfig) {
	t.Helper()
	sc := StrategyConfig{ID: "readiness-manual", Platform: "hyperliquid", Type: "manual", Symbol: "BTC", Args: []string{"hold", "BTC", "1h", "--mode=paper"}, Direction: DirectionBoth, Capital: 1000, Leverage: 1, RegimeATRWindow: "long", StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending_up": {ATR: 2}, "trending_down": {ATR: 2}, "ranging": {ATR: 2}}}}
	cfg := &Config{Regime: rc, Strategies: []StrategyConfig{sc}}
	orders, cancels := 0, 0
	recorder := func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		orders++
		if cancelStopLossOID != 0 || len(extraCancelOIDs) > 0 {
			cancels++
		}
		return nil, "", fmt.Errorf("offline recorder reached")
	}
	previousExecute := runHyperliquidExecuteFn
	runHyperliquidExecuteFn = recorder
	t.Cleanup(func() { runHyperliquidExecuteFn = previousExecute })
	d := manualCoreDeps{cfg: cfg, resolveRegime: func(StrategyConfig) (RegimePayload, error) { return bundles[2].Payload, nil }, execute: recorder, loadState: func(string, string) (manualStateView, error) {
		return manualStateView{HasStrategy: true, Pos: &Position{Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long"}}, nil
	}, cancelOrder: func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
		cancels++
		return nil, "", fmt.Errorf("offline cancellation reached")
	}}
	for _, dry := range []bool{false, true} {
		if _, err := manualOpenCore(d, sc, manualOpenInputs{StrategyID: sc.ID, Side: "long", Size: 1, DryRun: dry}); err == nil || !strings.Contains(err.Error(), "warming") {
			t.Fatalf("cold manual open: %v", err)
		}
		if _, err := manualAddCore(d, sc, manualAddInputs{StrategyID: sc.ID, Size: 1, DryRun: dry}); err == nil || !strings.Contains(err.Error(), "warming") {
			t.Fatalf("cold manual add: %v", err)
		}
	}
	logger := silentStrategyLogger(sc.ID)
	hold := regimeDependencyHold(sc, bundles[2].Payload, bundles[2].Payload, rc, true)
	if hold == "" {
		t.Fatal("cold stop was admitted")
	}
	for _, side := range []string{"long", "short"} {
		for _, signal := range []int{-1, 1} {
			result := &HyperliquidResult{Symbol: "BTC", Signal: signal, StrategyDecisionFields: StrategyDecisionFields{RegimeReadinessHold: hold}}
			if _, sent := runHyperliquidExecuteOrder(sc, result, 100, 1000, true, 0, side, 100, 1, 77, []int64{88}, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, logger); sent {
				t.Fatal("cold open sent")
			}
			if _, sent := runHyperliquidExecuteOrder(sc, result, 100, 1000, true, 1, side, 100, 1, 77, []int64{88}, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, logger); sent {
				t.Fatal("cold increase sent")
			}
			if _, sent := runHyperliquidScaleInOrder(sc, result, 1, hlExecuteSnapshot{}, nil, logger); sent {
				t.Fatal("cold add sent")
			}
		}
	}
	if orders != 0 || cancels != 0 {
		t.Fatalf("cold actions sent %d orders and %d cancellations", orders, cancels)
	}
	basketCfg := loadBasketTestConfig(t, nil)
	basketCfg.Regime = rc
	for i := range basketCfg.Strategies {
		basketCfg.Strategies[i].StopLossATRMult = nil
		basketCfg.Strategies[i].StopLossATRMultRegime = sc.StopLossATRMultRegime
		basketCfg.Strategies[i].RegimeATRWindow = "long"
	}
	basketEnv := newBasketTestEnv(t, basketCfg, nil, nil)
	now := time.Now().UTC().Truncate(time.Hour)
	member := basketEnv.bc.members["hl-bsk-btc"]
	observation := basketTestObservation(now, 300)
	observation.Regime = &bundles[2].Payload
	basketEnv.bc.observe(member, observation, 60000)
	memberState := basketEnv.state.Strategies[member.ID]
	for _, quantity := range []float64{0, 1} {
		if quantity > 0 {
			memberState.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: quantity, AvgCost: 60000, Side: "long"}
		}
		if _, why := basketEnv.bc.admitIncrease(basketEnv.rt(), member, memberState, basketCandidate{Coin: "BTC", PositionSide: "long"}, now); !strings.Contains(why, "warming") {
			t.Fatalf("basket cold stop: %s", why)
		}
	}
	if basketEnv.dbCount("SELECT COUNT(*) FROM basket_member_actions") != 0 {
		t.Fatal("cold basket reserved an action")
	}
	pairEnv := newPairTestEnv(t, nil)
	pairEnv.cfg.Regime = rc
	pairSC := pairEnv.bc.members[pairTestHL]
	pairSC.StopLossATRMult = nil
	pairSC.StopLossATRMultRegime = sc.StopLossATRMultRegime
	pairSC.RegimeATRWindow = "long"
	pairEnv.bc.members[pairTestHL] = pairSC
	pairEnv.observe("")
	pairObservation := pairEnv.rt().observations[pairTestHL]
	pairObservation.Regime = bundles[2].Payload
	pairEnv.rt().observations[pairTestHL] = pairObservation
	for _, quantity := range []float64{0, 1} {
		if quantity > 0 {
			pairEnv.state.Strategies[pairTestHL].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: quantity, AvgCost: 60000, Side: "long"}
		}
		if why := pairEnv.bc.pairIncreaseHold(pairEnv.rt(), pairEnv.inputs(pairEnv.now)); !strings.Contains(why, "warming") {
			t.Fatalf("pair cold stop: %s", why)
		}
	}

	pos := &Position{Symbol: "BTC", Quantity: 1, InitialQuantity: 1, AvgCost: 100, Side: "long", Regime: "trending_up_clean", RegimeWindows: map[string]string{"medium": "trending_up_clean", "long": ""}, RegimeSnapshots: bundles[2].Payload.Snapshots()}
	if positionCtxForCheck(sc, pos, rc).Regime != "" || positionATRRegimeLabel(hyperliquidProtectionPositionSnapshot(pos), sc) != "" {
		t.Fatal("cold named stop used primary label")
	}
	db := openTestDB(t)
	store := openTestStore(t, db)
	state := NewAppState()
	state.Strategies[sc.ID] = NewStrategyState(sc)
	state.Strategies[sc.ID].Positions["BTC"] = pos
	if err := db.migrateSchema(); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateSchema(); err != nil {
		t.Fatal(err)
	}
	for _, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Strategies[sc.ID].Positions["BTC"].RegimeSnapshots, pos.RegimeSnapshots) {
		t.Fatal("saved position snapshots changed")
	}
	if err := db.SaveStrategyBook(state.Strategies[sc.ID]); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.LoadState()
	if err != nil || !reflect.DeepEqual(loaded.Strategies[sc.ID].Positions["BTC"].RegimeWindows, pos.RegimeWindows) {
		t.Fatal("book write lost empty window")
	}
	pos.Regime, pos.RegimeWindows, pos.RegimeSnapshots = "", nil, entryRegimeSnapshots(RegimePayload{}, rc)
	if err := db.SaveStrategyBook(state.Strategies[sc.ID]); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	stampPositionRegimeFromPayload(loaded.Strategies[sc.ID], "BTC", bundles[3].Payload, sc, rc)
	emptyEntry := loaded.Strategies[sc.ID].Positions["BTC"]
	if emptyEntry.RegimeSnapshots == nil || len(emptyEntry.RegimeSnapshots) != 0 || emptyEntry.Regime != "" {
		t.Fatal("empty entry evidence was populated after restart")
	}
	storeRegime := &RegimeStore{}
	gen := storeRegime.resetForCycle(time.Now())
	storeRegime.set(bundles[0], gen)
	rc.Transitions = &RegimeTransitionAlertsConfig{Enabled: true}
	processRegimeTransitionAlerts(db, storeRegime, rc, nil, time.Now())
	var history int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM regime_window_history").Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 0 {
		t.Fatal("cold history was persisted")
	}
	entries, _ := uiRegimeEntries(storeRegime)
	if len(entries) != 1 || entries[0].Windows["long"].Status != "warming (1/101)" {
		t.Fatalf("cold UI state: %+v", entries)
	}
	regimeReadinessRecoveryIntegration(t, bundles, rc)
}

func regimeReadinessOnceIntegration(t *testing.T) {
	t.Helper()
	root := htfITRoot(t)
	dir := t.TempDir()
	entries := []map[string]any{
		{"id": "ready-bus", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py", "args": []string{"atr_breakout", "BTC/USDT", "1h"}, "capital": 1000},
		{"id": "ready-okx-spot", "type": "spot", "platform": "okx", "script": "shared_scripts/check_okx.py", "args": []string{"atr_breakout", "BTC", "1h", "--mode=paper", "--inst-type=spot"}, "capital": 1000},
		{"id": "ready-okx-perps", "type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py", "args": []string{"breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap"}, "capital": 1000, "leverage": 1, "sizing_leverage": 1, "close_strategy": map[string]any{"name": "tiered_tp_pct"}},
		{"id": "ready-rh", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py", "args": []string{"atr_breakout", "BTC", "30m", "--mode=paper"}, "capital": 1000},
		{"id": "ready-ts", "type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py", "args": []string{"breakout", "ES", "1h", "--mode=paper"}, "capital": 50000, "close_strategy": map[string]any{"name": "tiered_tp_pct"}},
	}
	for _, e := range entries {
		e["max_drawdown_pct"] = 90
		e["interval_seconds"] = 60
		e["allowed_regimes"] = []string{"trending_up", "trending_down", "ranging"}
	}
	cfgPath := filepath.Join(dir, "config.json")
	blob, err := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "interval_seconds": 60, "db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "regime": map[string]any{"enabled": true, "period": 50, "timeframe": "4h"}, "strategies": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := pyintegrationBinary(t)
	for _, stage := range []string{"cold-entry", "ready-entry", "cold-close"} {
		spot, fut := "up", "up"
		if stage == "cold-close" {
			spot, fut = "down", "tp"
		}
		fixture := htfITWriteFixture(t, filepath.Join(dir, stage+".json"), htfITFixture{HTF: "short", Spot: spot, Fut: fut})
		if stage == "ready-entry" {
			var doc map[string]any
			raw, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			now := htfITNow()
			for key := range doc["ccxt"].(map[string]any) {
				if strings.HasSuffix(key, "|4h") {
					doc["ccxt"].(map[string]any)[key] = htfITBars(104, 4*htfITHour, now, "")
				}
			}
			doc["yahoo"].(map[string]any)["ES=F|4h"] = htfITBars(104, 4*htfITHour, now, "")
			doc["yahoo"].(map[string]any)["BTC-USD|1h"] = htfITBars(416, htfITHour, now, "")
			raw, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bin, "--config", cfgPath, "--once", "--status-port", fmt.Sprint(htfITFreePort(t)))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"), "HTF_OFFLINE_FIXTURE="+fixture, "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
		raw, err := cmd.CombinedOutput()
		log := string(raw)
		if err != nil || strings.Contains(log, "network blocked by the htf offline fixture") {
			t.Fatalf("%s: offline scheduler failed: %v\n%s", stage, err, log)
		}
		for _, e := range entries {
			id := e["id"].(string)
			want := 0
			if stage == "ready-entry" {
				want = 1
			}
			if got := htfITPositions(t, log, id); got != want {
				t.Fatalf("%s: %s positions=%d want=%d\n%s", stage, id, got, want, log)
			}
			if stage == "cold-entry" && !strings.Contains(log, "["+id+"] [INFO] regime gate window") {
				t.Fatalf("%s: missing readiness hold\n%s", id, log)
			}
			if stage == "cold-close" && !strings.Contains(log, "["+id+"] [INFO] SELL ") {
				t.Fatalf("%s: cold management close was suppressed\n%s", id, log)
			}
		}
		t.Logf("%s: five real check scripts and scheduler paths passed", stage)
	}
}

func regimeReadinessRecoveryIntegration(t *testing.T, bundles []*RegimeBundle, rc *RegimeConfig) {
	t.Helper()
	sc := StrategyConfig{ID: "readiness-recovery", Platform: "hyperliquid", Type: "manual", Symbol: "BTC", Args: []string{"hold", "BTC", "1h", "--mode=paper"}, Direction: DirectionBoth, Capital: 1000, Leverage: 1, RegimeATRWindow: "long", StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending_up": {ATR: 2}, "trending_down": {ATR: 2}, "ranging": {ATR: 2}}}}
	cfg := &Config{Regime: rc, MarketFeed: "shared", IntervalSeconds: 60, Strategies: []StrategyConfig{sc}}
	db := openTestDB(t)
	store := openTestStore(t, db)
	state := NewAppState()
	state.Strategies[sc.ID] = NewStrategyState(sc)
	state.Strategies[sc.ID].Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 1, AvgCost: 100, StopLossOID: 77, TPOIDs: []int64{88}}
	for _, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatal(err)
		}
	}
	previousStore := globalRegimeStore
	globalRegimeStore = &RegimeStore{}
	t.Cleanup(func() { globalRegimeStore = previousStore })
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		t.Fatal("manual production bundle request missing")
	}
	fresh := *bundles[2]
	fresh.Key, fresh.At = req.Key, time.Now()
	gen := globalRegimeStore.resetForCycle(fresh.At)
	globalRegimeStore.set(&fresh, gen)
	server := &StatusServer{state: state, stateDB: store, mu: &sync.RWMutex{}}
	for _, deps := range []manualCoreDeps{newCLIManualCoreDeps(cfg, store, nil), server.daemonManualCoreDeps(cfg)} {
		for _, dry := range []bool{false, true} {
			if _, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: sc.ID, Side: "long", Size: 1, DryRun: dry}); err == nil || !strings.Contains(err.Error(), "warming") {
				t.Fatalf("production manual open: %v", err)
			}
			if _, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: sc.ID, Size: 1, DryRun: dry}); err == nil || !strings.Contains(err.Error(), "warming") {
				t.Fatalf("production manual add: %v", err)
			}
		}
	}
	fallback := sc
	fallback.CloseStrategy = &StrategyRef{Name: trailingTPRatchetCloseName}
	fallback.TrailingStopATRMultRegime = sc.StopLossATRMultRegime
	deps := manualCoreDeps{cfg: cfg, resolveRegime: func(StrategyConfig) (RegimePayload, error) {
		return RegimePayload{}, fmt.Errorf("offline classification failure")
	}}
	result := &manualCoreResult{}
	if _, why := manualRegimeAdmission(deps, fallback, result); why != "" || len(result.lines) == 0 || !strings.Contains(result.lines[0].text, "offline classification failure") {
		t.Fatalf("genuine failure fallback: %q %+v", why, result.lines)
	}
	deps.resolveRegime = func(StrategyConfig) (RegimePayload, error) { return RegimePayload{Snapshot: &RegimeSnapshot{}}, nil }
	if _, why := manualRegimeAdmission(deps, fallback, &manualCoreResult{}); !strings.Contains(why, "unavailable") {
		t.Fatal("malformed metadata used the failure fallback")
	}
	adopted := sc
	adopted.ID += "-adopted"
	captured := sc
	captured.ID += "-captured"
	cfg.Strategies = append(cfg.Strategies, adopted, captured)
	state.Strategies[adopted.ID] = NewStrategyState(adopted)
	state.Strategies[captured.ID] = NewStrategyState(captured)
	if _, err := manualOpenCore(newCLIManualCoreDeps(cfg, store, nil), adopted, manualOpenInputs{StrategyID: adopted.ID, Side: "long", Size: 1, FillPrice: 100, RecordOnly: true}); err != nil {
		t.Fatalf("cold record-only adoption: %v", err)
	}
	evidence := entryRegimeSnapshots(bundles[2].Payload, rc)
	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: captured.ID, Action: "open", Symbol: "BTC", Side: "long", Quantity: 1, FillPrice: 100, CreatedAt: time.Now(), RegimeSnapshots: evidence}); err != nil {
		t.Fatal(err)
	}
	alerts, criticals := drainPendingManualActions(state, cfg, store)
	if len(alerts) != 2 || len(criticals) != 0 {
		t.Fatalf("queued adoption: alerts=%d criticals=%v", len(alerts), criticals)
	}
	queued, err := db.LoadPendingManualActions()
	if err != nil || len(queued) != 0 {
		t.Fatalf("queued rows were not acknowledged: %v %+v", err, queued)
	}
	loaded, err := db.LoadState()
	if err != nil || !reflect.DeepEqual(loaded.Strategies[captured.ID].Positions["BTC"].RegimeSnapshots, evidence) {
		t.Fatalf("queued evidence did not survive restart: %v", err)
	}
	if _, exists := loaded.Strategies[captured.ID].Positions["BTC"].RegimeWindows[regimeWindowReservedName]; exists {
		t.Fatal("entry primary reference leaked into the window label projection")
	}
	readyLabel := bundles[3].Payload.Label("long", rc)
	coldState := state.Strategies[sc.ID]
	syncStrategyRegimeState(coldState, bundles[2].Payload, rc)
	dynamic := sc
	dynamic.CloseStrategy = &StrategyRef{Name: dynamicCloseStrategyName, Params: unifiedBlock()}
	dynamic.CloseStrategy.Params[dynamicCloseParamConfirmCycles] = 2
	position := coldState.Positions["BTC"]
	position.RegimeAppliedLabel, position.RegimePendingLabel, position.RegimePendingCount = "ranging", readyLabel, 1
	if advanceDynamicCloseRegime(position, coldState, dynamic) || position.RegimePendingCount != 0 || position.RegimePendingLabel != "" || position.RegimeAppliedLabel != "ranging" || position.StopLossOID != 77 {
		t.Fatal("cold dynamic state changed applied protection or retained confirmation")
	}
	syncStrategyRegimeState(coldState, bundles[3].Payload, rc)
	if advanceDynamicCloseRegime(position, coldState, dynamic) || !advanceDynamicCloseRegime(position, coldState, dynamic) || position.RegimeAppliedLabel != readyLabel {
		t.Fatal("dynamic recovery bypassed fresh confirmation")
	}
	allocation := &RegimeProfileAllocation{Window: "long", InitialProfile: "fade", ConfirmBars: 12, Profiles: map[string]string{readyLabel: "trend", "ranging": "fade"}}
	profile := RegimeProfileState{ActiveProfile: "fade", PendingProfile: "trend", PendingBarsSeen: 11}
	_, profile = resolveRegimeProfile(allocation, "", "cold", &profile, 0, "")
	if profile.PendingProfile != "" || profile.PendingBarsSeen != 0 || profile.ActiveProfile != "fade" {
		t.Fatal("cold profile state retained confirmation")
	}
	for i := 0; i < 11; i++ {
		_, profile = resolveRegimeProfile(allocation, readyLabel, fmt.Sprint(i), &profile, 0, "")
		if profile.ActiveProfile != "fade" {
			t.Fatal("profile recovery bypassed fresh confirmation")
		}
	}
	_, profile = resolveRegimeProfile(allocation, readyLabel, "confirmed", &profile, 0, "")
	if profile.ActiveProfile != "trend" {
		t.Fatal("profile did not recover after fresh confirmation")
	}
	snaps := bundles[2].Payload.Snapshots()
	snaps["fast"] = RegimeSnapshot{Regime: "trending_down"}
	if _, ok := classifyRegimeReversal(snaps, map[string]int{"fast": 14, "medium": 30, "long": 50}, 1); ok {
		t.Fatal("cold longest reversal window used a shorter reference")
	}
	rec := &okxGoldenRecNotifier{}
	notifier := NewMultiNotifier(notifierBackend{notifier: rec, ownerID: "offline-owner"})
	coldStore := &RegimeStore{}
	gen = coldStore.resetForCycle(time.Now())
	coldStore.set(bundles[0], gen)
	processRegimeTransitionAlerts(db, coldStore, rc, notifier, time.Now())
	if len(rec.drain()) != 0 {
		t.Fatal("cold observation sent a transition alert")
	}
	regimeReadinessLimitIntegration(t, bundles, rc)
}

func regimeReadinessStopBackfillIntegration(t *testing.T, bundles []*RegimeBundle, rc *RegimeConfig) {
	t.Helper()
	stop := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending_up": {ATR: 2.5}, "trending_down": {ATR: 9}, "ranging": {ATR: 9}}}
	base := StrategyConfig{Platform: "hyperliquid", Type: "manual", Symbol: "BTC", Args: []string{"hold", "BTC", "1h", "--mode=paper"}, Direction: DirectionBoth, Capital: 1000, Leverage: 1, RegimeATRWindow: "long", StopLossATRMultRegime: stop}
	ids := []string{"backfill-ready", "backfill-warming", "backfill-limit"}
	cfg := &Config{Regime: rc, MarketFeed: "shared", IntervalSeconds: 60}
	for _, id := range ids {
		sc := base
		sc.ID = id
		cfg.Strategies = append(cfg.Strategies, sc)
	}
	db := openTestDB(t)
	store := openTestStore(t, db)
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	previousStore := globalRegimeStore
	globalRegimeStore = &RegimeStore{}
	t.Cleanup(func() { globalRegimeStore = previousStore })
	req, ok := strategyRegimeBundleRequest(cfg.Strategies[0], rc)
	if !ok {
		t.Fatal("backfill bundle request missing")
	}
	setBundle := func(original *RegimeBundle) RegimePayload {
		fresh := *original
		fresh.Key, fresh.At = req.Key, time.Now()
		gen := globalRegimeStore.resetForCycle(fresh.At)
		globalRegimeStore.set(&fresh, gen)
		return globalRegimeStore.PayloadForStrategy(cfg.Strategies[0], rc)
	}
	stopMult := func(st *AppState, sc StrategyConfig) float64 {
		plan, ok := buildHyperliquidProtectionPlan(sc, st.Strategies[sc.ID].Positions["BTC"], 0)
		if !ok {
			return 0
		}
		return plan.StopLossATRMult
	}
	cycle := func(st *AppState, payload RegimePayload, strategies ...StrategyConfig) {
		for _, sc := range strategies {
			syncStrategyRegimeState(st.Strategies[sc.ID], payload, rc)
			backfillPositionStopRegimeEvidence(st.Strategies[sc.ID], sc, payload, rc)
		}
	}
	ready, warming, limit := cfg.Strategies[0], cfg.Strategies[1], cfg.Strategies[2]
	readyPayload := setBundle(bundles[3])
	if _, err := manualOpenCore(newCLIManualCoreDeps(cfg, store, nil), ready, manualOpenInputs{StrategyID: ready.ID, Side: "long", Size: 1, FillPrice: 100, ATR: 1, RecordOnly: true}); err != nil {
		t.Fatalf("ready record-only adoption: %v", err)
	}
	coldPayload := setBundle(bundles[2])
	if _, err := manualOpenCore(newCLIManualCoreDeps(cfg, store, nil), warming, manualOpenInputs{StrategyID: warming.ID, Side: "long", Size: 1, FillPrice: 100, ATR: 1, RecordOnly: true}); err != nil {
		t.Fatalf("warming record-only adoption: %v", err)
	}
	if _, criticals := drainPendingManualActions(state, cfg, store); len(criticals) != 0 {
		t.Fatalf("record-only adoption criticals: %v", criticals)
	}
	if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: limit.ID, Symbol: "BTC", Side: "long", OrderOID: 4242, LimitPrice: 100, OrderSize: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE pending_limit_orders SET regime_snapshots_json = '' WHERE order_oid = 4242"); err != nil {
		t.Fatal(err)
	}
	orders, err := db.LoadPendingLimitOrders()
	if err != nil || len(orders) != 1 || orders[0].RegimeSnapshots != nil {
		t.Fatalf("pre-migration limit row: %v %+v", err, orders)
	}
	if _, err := applyLimitFillProgress(state, limit, orders[0], 1, 100, 0, 1, ATRMethodSimple, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, sc := range cfg.Strategies {
		if got := stopMult(state, sc); got != 0 {
			t.Fatalf("%s armed a stop before backfill: %g", sc.ID, got)
		}
	}
	cycle(state, readyPayload, ready)
	if got := stopMult(state, ready); got != 2.5 {
		t.Fatalf("ready record-only adoption: next cycle stop multiplier %g, want 2.5", got)
	}
	t.Logf("must survive: record-only adoption while the ATR window is ready arms stop multiplier %g on the next cycle", stopMult(state, ready))
	cycle(state, coldPayload, warming, limit)
	if stopMult(state, warming) != 0 || stopMult(state, limit) != 0 {
		t.Fatal("warming stop window armed a stop")
	}
	for _, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if stopMult(restarted, warming) != 0 || stopMult(restarted, limit) != 0 || stopMult(restarted, ready) != 2.5 {
		t.Fatal("restart changed stop evidence")
	}
	readyEvidence := cloneRegimeSnapshots(restarted.Strategies[ready.ID].Positions["BTC"].RegimeSnapshots)
	cycle(restarted, setBundle(bundles[3]), cfg.Strategies...)
	if got := stopMult(restarted, warming); got != 2.5 {
		t.Fatalf("warming record-only adoption after restart: first ready cycle multiplier %g, want 2.5", got)
	}
	t.Logf("must survive: warming record-only adoption held no stop while warming and armed multiplier %g on the first ready cycle after a restart", stopMult(restarted, warming))
	if got := stopMult(restarted, limit); got != 2.5 {
		t.Fatalf("pre-migration limit fill: first ready cycle multiplier %g, want 2.5", got)
	}
	t.Logf("must survive: a pending limit row with empty regime_snapshots_json armed multiplier %g on the first ready cycle", stopMult(restarted, limit))
	if !reflect.DeepEqual(restarted.Strategies[ready.ID].Positions["BTC"].RegimeSnapshots, readyEvidence) {
		t.Fatal("backfill replaced captured stop evidence")
	}
	other := setBundle(bundles[3])
	changed := other.Windows["long"]
	changed.Regime = "trending_down"
	other.Windows["long"] = changed
	if backfillPositionStopRegimeEvidence(restarted.Strategies[warming.ID], warming, other, rc) || stopMult(restarted, warming) != 2.5 {
		t.Fatal("backfill overwrote a captured stop label")
	}
}

func regimeReadinessClassifierCaseIntegration(t *testing.T, root string) {
	t.Helper()
	cases := []struct {
		name, classifier string
		period, bars     int
		hold             string
	}{
		{"capital", "Composite", 20, 200, ""},
		{"capital-warming", "Composite", 20, 10, "regime stop window w warming (10/29)"},
		{"spaced", " composite ", 60, 200, ""},
		{"spaced-warming", " composite ", 60, 59, "regime stop window w warming (59/60)"},
	}
	for _, tc := range cases {
		spec := RegimeWindowsMap{"w": {Classifier: tc.classifier, Period: tc.period}}
		rc := &RegimeConfig{Enabled: true, Windows: spec}
		if errs := validateRegimeWindowSpec("w", spec["w"], rc); len(errs) != 0 {
			t.Fatalf("%s: validator rejected %q: %v", tc.name, tc.classifier, errs)
		}
		emitted := regimeWindowsSpecJSON(rc)
		rows := make([][]float64, tc.bars)
		for i := range rows {
			rows[i] = []float64{float64(1700000000000 + i*3600000), float64(i + 1), float64(i + 2), float64(i), float64(i + 1), 1000}
		}
		frame := map[string]any{"rows": rows, "required": 251, "bars": tc.bars, "ready": true, "source": "ws", "forming_bar_included": true}
		input, err := json.Marshal(map[string]any{"v": 2, "market": map[string]any{"version": 4, "snapshot_id": "classifier-case", "frames": map[string]any{"BTC|1h": frame}, "mids": map[string]any{}, "feed_complete": true}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), filepath.Join(root, "shared_scripts", "check_regime.py"), "--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h", "--market-stdin", "--regime-windows-spec-json", emitted)
		cmd.Stdin = strings.NewReader(string(input))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: check_regime: %v", tc.name, err)
		}
		bundle, err := parseRegimeBundleOutput(regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: emitted}, out, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var raw struct {
			Regime map[string]struct {
				Need int `json:"need"`
			} `json:"regime"`
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		goNeed := regimeRequiredBars(spec["w"].Period, spec["w"].Classifier)
		if raw.Regime["w"].Need != goNeed {
			t.Fatalf("%s: python need %d, go need %d", tc.name, raw.Regime["w"].Need, goNeed)
		}
		sc := StrategyConfig{ID: "classifier-case", Platform: "hyperliquid", Type: "perps", Symbol: "BTC", RegimeATRWindow: "w", StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending_up": {ATR: 2}}}}
		hold := regimeDependencyHold(sc, bundle.Payload, bundle.Payload, rc, false)
		if hold != tc.hold {
			t.Fatalf("%s: hold %q, want %q", tc.name, hold, tc.hold)
		}
		t.Logf("must survive: classifier %q period %d with %d bars: python need %d, go need %d, hold %q", tc.classifier, tc.period, tc.bars, raw.Regime["w"].Need, goNeed, hold)
	}
}

func regimeReadinessLimitIntegration(t *testing.T, bundles []*RegimeBundle, rc *RegimeConfig) {
	t.Helper()
	sc := StrategyConfig{ID: "readiness-limit-live", Platform: "hyperliquid", Type: "manual", Symbol: "BTC", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Direction: DirectionBoth, Capital: 1000, Leverage: 1, RegimeATRWindow: "long", StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending_up": {ATR: 2}, "trending_down": {ATR: 2}, "ranging": {ATR: 2}}}}
	cfg := &Config{Regime: rc, MarketFeed: "shared", IntervalSeconds: 60, Strategies: []StrategyConfig{sc}}
	db := openTestDB(t)
	store := openTestStore(t, db)
	state := NewAppState()
	state.Strategies[sc.ID] = NewStrategyState(sc)
	for _, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec("ALTER TABLE pending_limit_orders DROP COLUMN regime_snapshots_json"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadPendingLimitOrders(); err != nil {
		t.Fatalf("legacy pending limit read: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := db.migrateSchema(); err != nil {
			t.Fatal(err)
		}
	}
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		t.Fatal("limit bundle request missing")
	}
	setBundle := func(original *RegimeBundle) {
		fresh := *original
		fresh.Key, fresh.At = req.Key, time.Now()
		gen := globalRegimeStore.resetForCycle(fresh.At)
		globalRegimeStore.set(&fresh, gen)
	}
	calls := 0
	withStubbedLimitOpen(t, func(string, string, string, float64, float64, string, string, float64, hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
		calls++
		return &HyperliquidLimitOpenResult{Status: "resting", OrderOID: 999}, "", nil
	})
	setBundle(bundles[2])
	input := manualLimitOpenInputs{strategyID: sc.ID, side: "long", openSide: "buy", size: 1, limitPrice: 100, tif: "Alo"}
	for _, dry := range []bool{false, true} {
		for _, side := range []string{"long", "short"} {
			cold := input
			cold.dryRun, cold.side = dry, side
			if side == "short" {
				cold.openSide = "sell"
			}
			if runManualLimitOpen(cfg, sc, store, cold) != 1 || calls != 0 {
				t.Fatal("cold limit placement or dry-run passed")
			}
		}
	}
	setBundle(bundles[3])
	dry := input
	dry.dryRun = true
	if runManualLimitOpen(cfg, sc, store, dry) != 0 || calls != 0 {
		t.Fatal("ready limit dry-run sent an order")
	}
	if runManualLimitOpen(cfg, sc, store, input) != 0 || calls != 1 {
		t.Fatal("ready limit placement failed")
	}
	orders, err := db.LoadPendingLimitOrders()
	if err != nil || len(orders) != 1 {
		t.Fatalf("pending limit evidence: %v %+v", err, orders)
	}
	evidence := entryRegimeSnapshots(bundles[3].Payload, rc)
	if !reflect.DeepEqual(orders[0].RegimeSnapshots, evidence) {
		t.Fatal("pending limit did not retain placement evidence")
	}
	setBundle(bundles[2])
	if _, err := applyLimitFillProgress(state, sc, orders[0], 0.5, 100, 0, 1, ATRMethodSimple, time.Now()); err != nil {
		t.Fatal(err)
	}
	orders[0].FilledSize = 0.5
	if _, err := applyLimitFillProgress(state, sc, orders[0], 1, 100, 0, 2, ATRMethodSimple, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStrategyBook(state.Strategies[sc.ID]); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	position := loaded.Strategies[sc.ID].Positions["BTC"]
	if !reflect.DeepEqual(position.RegimeSnapshots, evidence) || positionATRRegimeLabel(position, sc) != bundles[3].Payload.Label("long", rc) || position.EntryATR != 1 || position.Quantity != 1 {
		t.Fatal("cold later limit fill lost placement protection or changed entry evidence")
	}
}
