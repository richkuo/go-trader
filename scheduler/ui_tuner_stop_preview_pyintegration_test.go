//go:build pyintegration

package main

import (
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

func TestTunerPreviewStopPayloadContract(t *testing.T) {
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

	margin := 20.0
	msc := stopPreviewStrategy()
	msc.StopLossMarginPct = &margin
	loaded := simulateConfigPayload(msc, nil)
	if _, err := runStrategySimulate(candles, map[string]map[string]interface{}{"live": loaded}); err == nil || !strings.Contains(err.Error(), "UNVERIFIED_MARGIN_LEVERAGE") {
		t.Fatalf("margin stop with loaded leverage must be refused as unverified, got %v", err)
	}
	overridden := simulateConfigPayload(msc, nil)
	overridden["leverage_source"] = "tuner_override"
	if _, err := runStrategySimulate(candles, map[string]map[string]interface{}{"simulated": overridden}); err != nil {
		t.Fatalf("margin stop with an operator leverage override refused: %v", err)
	}
}
