//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func stopPreviewCandles() []UICandle {
	candles := make([]UICandle, 0, 80)
	for i := 0; i < 80; i++ {
		px := 100 + float64(i%17) - float64(i%7)
		candles = append(candles, UICandle{Time: 1767225600 + int64(i)*3600, Open: px, High: px + 1, Low: px - 1, Close: px + 0.5, Volume: 10})
	}
	return candles
}

func stopPreviewStrategy() StrategyConfig {
	return StrategyConfig{
		ID:             "hl-stop-preview",
		Type:           "perps",
		Platform:       "hyperliquid",
		Args:           []string{"sma_crossover", "BTC", "1h", "--mode=paper"},
		OpenStrategy:   StrategyRef{Name: "sma_crossover"},
		Leverage:       5,
		MaxDrawdownPct: 50,
	}
}

func loadStopPreviewStrategy(t *testing.T, config, strategy map[string]interface{}) (StrategyConfig, *RegimeConfig) {
	t.Helper()
	f, _ := loadStopGeometryFixture(t)
	cfg, err := LoadConfigReadOnly(writeTestConfig(t, t.TempDir(), stopGeometryConfigJSON(t, f, false, false, config, strategy)))
	if err != nil {
		t.Fatalf("preview strategy must load: %v", err)
	}
	return cfg.Strategies[0], cfg.Regime
}

func TestTunerPreviewStopPayloadContract(t *testing.T) {
	explicitMargin, _ := loadStopPreviewStrategy(t, nil, map[string]interface{}{"stop_loss_margin_pct": 20, "leverage": 5})
	defaultedMargin, _ := loadStopPreviewStrategy(t, nil, map[string]interface{}{"stop_loss_margin_pct": 20})
	regimeWindows := map[string]interface{}{"regime": map[string]interface{}{
		"enabled": true, "period": 14, "adx_threshold": 20,
		"windows": map[string]interface{}{"medium": 14, "long": 50},
	}}
	regimeTrail := func(window string) map[string]interface{} {
		return map[string]interface{}{
			"trailing_stop_atr_mult_regime": map[string]interface{}{"use_defaults": true},
			"regime_atr_window":             window,
		}
	}
	primaryWindow, primaryRegime := loadStopPreviewStrategy(t, regimeWindows, regimeTrail("medium"))
	otherWindow, otherRegime := loadStopPreviewStrategy(t, regimeWindows, regimeTrail("long"))
	t.Chdir("..")
	candles := stopPreviewCandles()

	pct := 2.0
	sc := stopPreviewStrategy()
	sc.StopLossPct = &pct
	if _, err := runStrategySimulate(candles, map[string]map[string]interface{}{"live": simulateConfigPayload(sc, nil)}); err != nil {
		t.Fatalf("percent stop preview refused: %v", err)
	}

	unmarked := simulateConfigPayload(sc, nil)
	delete(unmarked, "stop_units")
	if _, err := runStrategySimulate(candles, map[string]map[string]interface{}{"live": unmarked}); err == nil || !strings.Contains(err.Error(), "stop_units") {
		t.Fatalf("preview without the unit marker must be refused, got %v", err)
	}

	if explicitMargin.leverageDefaulted {
		t.Fatalf("explicit config leverage must not be marked as a loader default")
	}
	preview, err := runTunerPreviewSimulate(candles, simulateConfigPayload(explicitMargin, nil), simulateConfigPayload(explicitMargin, nil))
	if err != nil || preview.LiveRefusal != "" {
		t.Fatalf("margin stop with explicit config leverage must preview both arms, got refusal %q err %v", preview.LiveRefusal, err)
	}
	if _, ok := preview.Markers["live"]; !ok {
		t.Fatalf("explicit-leverage margin preview has no live arm: %v", preview.Markers)
	}
	if _, ok := preview.Markers["simulated"]; !ok {
		t.Fatalf("explicit-leverage margin preview has no simulated arm: %v", preview.Markers)
	}

	if !defaultedMargin.leverageDefaulted || defaultedMargin.Leverage != 1 {
		t.Fatalf("loader must default leverage to 1 and record it, got %v defaulted=%v", defaultedMargin.Leverage, defaultedMargin.leverageDefaulted)
	}
	overridden, err := mergeStrategyTunerOverrides(defaultedMargin, map[string]json.RawMessage{"leverage": json.RawMessage("5")})
	if err != nil {
		t.Fatalf("leverage override: %v", err)
	}
	overriddenPayload := simulateConfigPayload(overridden, nil)
	overriddenPayload["leverage_source"] = "tuner_override"
	preview, err = runTunerPreviewSimulate(candles, simulateConfigPayload(defaultedMargin, nil), overriddenPayload)
	if err != nil {
		t.Fatalf("simulated arm with an operator leverage override must render, got %v", err)
	}
	if !strings.Contains(preview.LiveRefusal, "UNVERIFIED_MARGIN_LEVERAGE") {
		t.Fatalf("live arm with defaulted leverage must be refused as unverified, got %q", preview.LiveRefusal)
	}
	if _, ok := preview.Markers["live"]; ok {
		t.Fatalf("refused live arm must not render markers: %v", preview.Markers)
	}
	if _, ok := preview.Markers["simulated"]; !ok {
		t.Fatalf("simulated arm missing after a live refusal: %v", preview.Markers)
	}

	primaryPayload := simulateConfigPayload(primaryWindow, primaryRegime)
	if preview, err := runTunerPreviewSimulate(candles, primaryPayload, primaryPayload); err != nil || preview.LiveRefusal != "" || preview.SimulatedRefusal != "" {
		t.Fatalf("regime owner on the primary window must preview, got %+v err %v", preview, err)
	}
	otherPayload := simulateConfigPayload(otherWindow, otherRegime)
	if preview, err := runTunerPreviewSimulate(candles, otherPayload, otherPayload); err != nil || preview.LiveRefusal != "" || preview.SimulatedRefusal != "" {
		t.Fatalf("prepared non-primary ATR window must preview, got %+v err %v", preview, err)
	}
	otherRegime.Timeframe = "4h"
	shiftedLive := simulateConfigPayload(otherWindow, otherRegime)
	shiftedSim := simulateConfigPayload(otherWindow, otherRegime)
	preview, err = runTunerPreviewSimulate(candles, shiftedLive, shiftedSim)
	if err != nil {
		t.Fatalf("a regime timeframe the preview does not fetch must refuse per arm, not fail: %v", err)
	}
	if !strings.Contains(preview.LiveRefusal, "regime.timeframe") || !strings.Contains(preview.SimulatedRefusal, "regime.timeframe") {
		t.Fatalf("both arms built from the 4h regime must report the timeframe refusal, got %+v", preview)
	}
	if len(preview.Markers) != 0 {
		t.Fatalf("refused arms must not render markers: %v", preview.Markers)
	}
	primaryRegime.Timeframe = "4h"
	primaryShifted := simulateConfigPayload(primaryWindow, primaryRegime)
	preview, err = runTunerPreviewSimulate(candles, primaryShifted, primaryShifted)
	if err != nil || !strings.Contains(preview.LiveRefusal, "regime.timeframe") {
		t.Fatalf("a primary-window regime stop owner reads labels and must refuse a regime timeframe the preview does not fetch, got %+v err %v", preview, err)
	}
}

func tunerHandlerCandles() []UICandle {
	candles := make([]UICandle, 0, 600)
	for i := 0; i < 600; i++ {
		px := 5000.0 - 4.0*float64(i) + 40*math.Sin(float64(i)/6.0)
		candles = append(candles, UICandle{Time: 1700000000 + int64(i)*3600, Open: px, High: px + 4, Low: px - 4, Close: px + 1, Volume: 1})
	}
	return candles
}

func tunerHandlerStrategy(mutate func(*StrategyConfig)) StrategyConfig {
	pct := 50.0
	sc := StrategyConfig{
		ID:           "okx-preview",
		Type:         "perps",
		Platform:     "okx",
		Args:         []string{"sma_crossover", "BTC", "1h"},
		OpenStrategy: StrategyRef{Name: "sma_crossover", Params: map[string]interface{}{"fast_period": 5, "slow_period": 20}},
		StopLossPct:  &pct,
		Leverage:     1,
	}
	if mutate != nil {
		mutate(&sc)
	}
	return sc
}

func postTunerSimulate(t *testing.T, sc StrategyConfig, regime *RegimeConfig, overrides string) (int, UISimulateResponse) {
	t.Helper()
	var mu sync.RWMutex
	ss := NewStatusServer(NewAppState(), &mu, "", []StrategyConfig{sc}, nil)
	ss.regime = regime
	candles := tunerHandlerCandles()
	ss.candleFetcher = func(req UICandleRequest) ([]UICandle, string, error) {
		return candles, "test", nil
	}
	body := `{"limit":1000}`
	if overrides != "" {
		body = `{"limit":1000,"overrides":` + overrides + `}`
	}
	req := httptest.NewRequest(http.MethodPost, "/api/strategies/"+sc.ID+"/simulate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ss.handleAPIStrategy(w, req)
	var resp UISimulateResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode simulate response: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, resp
}

func entrySides(markers []UITradeMarker) (long, short int) {
	for _, m := range markers {
		if m.IsClose {
			continue
		}
		switch m.Side {
		case "buy":
			long++
		case "sell":
			short++
		}
	}
	return long, short
}

func TestTunerPreviewHandlerRegimeTimeframeRefusesPerArm(t *testing.T) {
	t.Chdir("..")
	fourHour := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, Timeframe: "4h"}
	gated := tunerHandlerStrategy(func(sc *StrategyConfig) { sc.AllowedRegimes = []string{"trending_up"} })

	code, resp := postTunerSimulate(t, gated, fourHour, "")
	if code != http.StatusOK {
		t.Fatalf("gated 1h strategy on a 4h regime: status %d, want 200", code)
	}
	if !strings.Contains(resp.LiveRefusal, "regime.timeframe") {
		t.Fatalf("live_refusal must name regime.timeframe, got %q", resp.LiveRefusal)
	}
	if !strings.Contains(resp.PreviewNote, resp.LiveRefusal) {
		t.Fatalf("preview note must quote the refusal, got %q", resp.PreviewNote)
	}

	code, resp = postTunerSimulate(t, gated, fourHour, `{"stop_loss_pct":10}`)
	if code != http.StatusOK {
		t.Fatalf("gated strategy with a stop override: status %d, want 200", code)
	}
	if !strings.Contains(resp.SimulatedRefusal, "regime.timeframe") || len(resp.SimulatedMarkers) != 0 {
		t.Fatalf("simulated arm must report its own refusal and no markers, got refusal %q markers %d", resp.SimulatedRefusal, len(resp.SimulatedMarkers))
	}
	if !strings.Contains(resp.PreviewNote, resp.SimulatedRefusal) {
		t.Fatalf("preview note must quote the simulated refusal, got %q", resp.PreviewNote)
	}

	plain := tunerHandlerStrategy(nil)
	code, resp = postTunerSimulate(t, plain, fourHour, `{"stop_loss_pct":10}`)
	if code != http.StatusOK || resp.LiveRefusal != "" || resp.SimulatedRefusal != "" {
		t.Fatalf("a strategy that reads no regime label must render both arms, got status %d %+v", code, resp)
	}
	if len(resp.LiveMarkers) == 0 || len(resp.SimulatedMarkers) == 0 {
		t.Fatalf("both arms must render markers, got live %d simulated %d", len(resp.LiveMarkers), len(resp.SimulatedMarkers))
	}
}

func TestTunerPreviewHandlerAppliesCertifiedDirectionalPolicy(t *testing.T) {
	t.Chdir("..")
	prev := getDirectionalCertStore()
	t.Cleanup(func() { setDirectionalCertStore(prev) })
	regime := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	policy := &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_down": {Direction: DirectionShort, InvertSignal: true},
	}}
	withPolicy := tunerHandlerStrategy(func(sc *StrategyConfig) { sc.RegimeDirectionalPolicy = policy })

	setDirectionalCertStore(&DirectionalCertSet{byKey: map[string]DirectionalCertEntry{
		certKey("BTC", "1h", "adx"): {States: map[string]string{"trending_down": DirectionShort}},
	}})
	code, certified := postTunerSimulate(t, withPolicy, regime, `{"stop_loss_pct":40}`)
	if code != http.StatusOK || certified.LiveRefusal != "" {
		t.Fatalf("certified policy preview: status %d refusal %q", code, certified.LiveRefusal)
	}
	if _, short := entrySides(certified.LiveMarkers); short == 0 {
		t.Fatalf("a certified trending_down short policy on a falling series must show short entries, got %+v", certified.LiveMarkers)
	}

	setDirectionalCertStore(emptyDirectionalCertSet())
	code, uncertified := postTunerSimulate(t, withPolicy, regime, `{"stop_loss_pct":40}`)
	if code != http.StatusOK || uncertified.LiveRefusal != "" {
		t.Fatalf("uncertified policy preview: status %d refusal %q", code, uncertified.LiveRefusal)
	}
	long, short := entrySides(uncertified.LiveMarkers)
	if short != 0 || long == 0 {
		t.Fatalf("an uncertified policy must preview the base direction, got long %d short %d", long, short)
	}

	code, noPolicy := postTunerSimulate(t, tunerHandlerStrategy(nil), regime, `{"stop_loss_pct":40}`)
	if code != http.StatusOK {
		t.Fatalf("no-policy preview: status %d", code)
	}
	gotNo, _ := json.Marshal(noPolicy.LiveMarkers)
	gotUncert, _ := json.Marshal(uncertified.LiveMarkers)
	if string(gotNo) != string(gotUncert) {
		t.Fatalf("a strategy with no policy must preview the base direction unchanged:\nno policy   %s\nuncertified %s", gotNo, gotUncert)
	}
}
