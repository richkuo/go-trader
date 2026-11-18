package main

import (
	"strings"
	"testing"
)

func chartPatternGateHL(id, mode string, params map[string]interface{}, args ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{chartPatternStrategyName, "BTC", "1h", "--mode=" + mode}, args...)...)
	sc["open_strategy"] = map[string]interface{}{"name": chartPatternStrategyName, "params": params}
	return sc
}

func chartPatternGateSpot(id string, params map[string]interface{}, args ...string) map[string]interface{} {
	return map[string]interface{}{
		"id":               id,
		"type":             "spot",
		"platform":         "binanceus",
		"script":           "shared_scripts/check_strategy.py",
		"args":             append([]string{chartPatternStrategyName, "BTC/USDT", "1h"}, args...),
		"capital":          1000.0,
		"max_drawdown_pct": 50.0,
		"open_strategy":    map[string]interface{}{"name": chartPatternStrategyName, "params": params},
	}
}

func chartPatternGateLoad(t *testing.T, root map[string]interface{}, sc map[string]interface{}) (*Config, error) {
	t.Helper()
	return calendarLoadRoot(t, root, sc)
}

var chartPatternGateRegimeOn = map[string]interface{}{"regime": map[string]interface{}{"enabled": true}}

func TestChartPatternHTFGateAcceptsWarmFramesAndInactiveFactors(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, params := range []map[string]interface{}{
		nil,
		{"htf_gate_factor": 0},
		{"htf_gate_factor": 1},
		{"htf_gate_factor": 1, "htf_gate_ema_slow": 5000},
		{"htf_gate_factor": 4},
		{"htf_gate_factor": 4, "htf_gate_mode": "align"},
		{"htf_gate_factor": 4, "htf_gate_ema_slow": 49},
	} {
		for _, mode := range []string{"live", "paper"} {
			if _, err := chartPatternGateLoad(t, nil, chartPatternGateHL("hl-cpat-ok", mode, params)); err != nil {
				t.Fatalf("%s %v must keep loading: %v", mode, params, err)
			}
		}
	}
	if _, err := chartPatternGateLoad(t, nil, chartPatternGateHL("hl-cpat-ema50", "paper", map[string]interface{}{"htf_gate_factor": 4, "htf_gate_ema_slow": 50})); err == nil ||
		!strings.Contains(err.Error(), "htf_gate_factor=4 (htf_gate_mode=\"veto\", explicit htf_gate_ema_slow=50) cannot warm in the default check frame of 200 candles") ||
		!strings.Contains(err.Error(), "at least 204 candles") {
		t.Fatalf("factor 4 with htf_gate_ema_slow 50 on 200 candles must be refused, got %v", err)
	}
	if _, err := chartPatternGateLoad(t, nil, chartPatternGateHL("hl-cpat-frac", "paper", map[string]interface{}{"htf_gate_factor": 4.5})); err == nil ||
		!strings.Contains(err.Error(), "htf_gate_factor must be an integer") {
		t.Fatalf("a fractional factor must be refused, got %v", err)
	}
}

func TestChartPatternHTFGateResolvesTheFrameEachCheckPathRequests(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	five := map[string]interface{}{"htf_gate_factor": 5}
	if _, err := chartPatternGateLoad(t, nil, chartPatternGateHL("hl-cpat-args", "paper", five, "--ohlcv-limit", "300")); err != nil {
		t.Fatalf("an individual-path args frame of 300 candles with regime off must load: %v", err)
	}
	if _, err := chartPatternGateLoad(t, nil, chartPatternGateHL("hl-cpat-args", "paper", five, "--ohlcv-limit=300", "--ohlcv-limit", "100")); err == nil ||
		!strings.Contains(err.Error(), "cannot warm in the args --ohlcv-limit check frame of 100 candles") {
		t.Fatalf("argparse keeps the last args frame, got %v", err)
	}
	_, err := chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateHL("hl-cpat-args", "paper", five, "--ohlcv-limit", "300"))
	if err == nil || !strings.Contains(err.Error(), "cannot warm in the regime-enabled check frame (the scheduler appends its --ohlcv-limit after args) of 200 candles") {
		t.Fatalf("the appended regime frame of 200 wins over args 300, got %v", err)
	}
	feed := map[string]interface{}{"market_feed": "websocket"}
	if _, err := chartPatternGateLoad(t, feed, chartPatternGateHL("hl-cpat-args", "paper", five, "--ohlcv-limit", "300")); err == nil ||
		!strings.Contains(err.Error(), "cannot warm in the websocket market feed signal frame (the sealed snapshot holds 200 candles for this slot) of 200 candles") {
		t.Fatalf("the sealed feed frame caps an args frame, got %v", err)
	}
	if _, err := chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateSpot("bus-cpat-btc", five, "--ohlcv-limit", "300")); err != nil {
		t.Fatalf("check_strategy.py takes the first --ohlcv-limit, so args 300 win over the appended regime frame: %v", err)
	}
	if _, err := chartPatternGateLoad(t, nil, chartPatternGateSpot("bus-cpat-btc", five)); err == nil ||
		!strings.Contains(err.Error(), "strategy bus-cpat-btc: open strategy chart_pattern htf_gate_factor=5") {
		t.Fatalf("a spot slot on the default frame must be refused, got %v", err)
	}
	cfg, err := chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateHL("hl-cpat-batch", "paper", map[string]interface{}{"htf_gate_factor": 4}))
	if err != nil {
		t.Fatal(err)
	}
	key, ok := hlBatchKeyForStrategy(cfg.Strategies[0], cfg)
	frame, _, frameErr := chartPatternCheckFrame(cfg.Strategies[0], cfg)
	if !ok || frameErr != nil || int64(key.OhlcvLimit) != frame {
		t.Fatalf("a batch-eligible slot must resolve the batch key frame: key=%+v ok=%v frame=%d err=%v", key, ok, frame, frameErr)
	}
}

func chartPatternGateProfileHL(id string, base, trend, rng map[string]interface{}) map[string]interface{} {
	sc := chartPatternGateHL(id, "paper", base)
	sc["regime_profile_allocation"] = map[string]interface{}{
		"window":          "default",
		"profiles":        map[string]interface{}{"trending_up": "trend", "trending_down": "trend", "ranging": "range"},
		"param_sets":      map[string]interface{}{"trend": trend, "range": rng},
		"confirm_bars":    12,
		"initial_profile": "range",
	}
	return sc
}

func TestChartPatternHTFGateChecksEveryRegimeProfileParamSet(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	_, err := chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateProfileHL("hl-cpat-prof-trend",
		map[string]interface{}{}, map[string]interface{}{"htf_gate_factor": 5}, map[string]interface{}{}))
	if err == nil ||
		!strings.Contains(err.Error(), "strategy hl-cpat-prof-trend: open strategy chart_pattern with regime_profile_allocation.param_sets[\"trend\"] merged over its params htf_gate_factor=5") ||
		!strings.Contains(err.Error(), "at least 205 candles") ||
		strings.Contains(err.Error(), "param_sets[\"range\"]") {
		t.Fatalf("a profile that sets factor 5 on the 200-candle regime frame must be refused and named, got %v", err)
	}
	_, err = chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateProfileHL("hl-cpat-prof-merge",
		map[string]interface{}{"htf_gate_factor": 4}, map[string]interface{}{"htf_gate_ema_slow": 50}, map[string]interface{}{"htf_gate_ema_slow": 40}))
	if err == nil ||
		!strings.Contains(err.Error(), "param_sets[\"trend\"] merged over its params htf_gate_factor=4 (htf_gate_mode=\"veto\", explicit htf_gate_ema_slow=50)") ||
		!strings.Contains(err.Error(), "at least 204 candles") {
		t.Fatalf("the base factor 4 merged with the profile htf_gate_ema_slow 50 must be refused, got %v", err)
	}
	if _, err := chartPatternGateLoad(t, chartPatternGateRegimeOn, chartPatternGateProfileHL("hl-cpat-prof-ok",
		map[string]interface{}{"htf_gate_factor": 4}, map[string]interface{}{"htf_gate_ema_slow": 40}, map[string]interface{}{"htf_gate_factor": 0})); err != nil {
		t.Fatalf("profiles whose merged params warm on 200 candles must load: %v", err)
	}
}
