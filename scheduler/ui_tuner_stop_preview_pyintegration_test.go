//go:build pyintegration

package main

import (
	"encoding/json"
	"strings"
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
	markers, liveRefusal, err := runTunerPreviewSimulate(candles, simulateConfigPayload(explicitMargin, nil), simulateConfigPayload(explicitMargin, nil))
	if err != nil || liveRefusal != "" {
		t.Fatalf("margin stop with explicit config leverage must preview both arms, got refusal %q err %v", liveRefusal, err)
	}
	if _, ok := markers["live"]; !ok {
		t.Fatalf("explicit-leverage margin preview has no live arm: %v", markers)
	}
	if _, ok := markers["simulated"]; !ok {
		t.Fatalf("explicit-leverage margin preview has no simulated arm: %v", markers)
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
	markers, liveRefusal, err = runTunerPreviewSimulate(candles, simulateConfigPayload(defaultedMargin, nil), overriddenPayload)
	if err != nil {
		t.Fatalf("simulated arm with an operator leverage override must render, got %v", err)
	}
	if !strings.Contains(liveRefusal, "UNVERIFIED_MARGIN_LEVERAGE") {
		t.Fatalf("live arm with defaulted leverage must be refused as unverified, got %q", liveRefusal)
	}
	if _, ok := markers["live"]; ok {
		t.Fatalf("refused live arm must not render markers: %v", markers)
	}
	if _, ok := markers["simulated"]; !ok {
		t.Fatalf("simulated arm missing after a live refusal: %v", markers)
	}

	primaryPayload := simulateConfigPayload(primaryWindow, primaryRegime)
	if _, liveRefusal, err := runTunerPreviewSimulate(candles, primaryPayload, primaryPayload); err != nil || liveRefusal != "" {
		t.Fatalf("regime owner on the primary window must preview, got refusal %q err %v", liveRefusal, err)
	}
	otherPayload := simulateConfigPayload(otherWindow, otherRegime)
	if _, liveRefusal, err := runTunerPreviewSimulate(candles, otherPayload, otherPayload); err != nil || liveRefusal != "" {
		t.Fatalf("prepared non-primary ATR window must preview, got refusal %q err %v", liveRefusal, err)
	}
	otherRegime.Timeframe = "4h"
	shifted := simulateConfigPayload(otherWindow, otherRegime)
	if _, liveRefusal, err := runTunerPreviewSimulate(candles, shifted, otherPayload); err != nil || !strings.Contains(liveRefusal, "regime.timeframe") {
		t.Fatalf("a regime timeframe the preview does not fetch must refuse the live arm, got refusal %q err %v", liveRefusal, err)
	}
}
