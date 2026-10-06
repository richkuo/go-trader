package main

import "testing"

func closedBarTestResult(signal int, atr float64, held bool) *HyperliquidResult {
	result := &HyperliquidResult{Symbol: "BTC", Signal: signal, Price: 40000, Indicators: map[string]interface{}{"atr": atr}}
	result.ClosedBar = &ClosedBarDecision{Enabled: true, Held: held}
	return result
}

func TestClosedBarEntryATRStampsLateWhenOpenHadNoClosedATR(t *testing.T) {
	sc := StrategyConfig{ID: "hl-closed-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"breakout", "BTC", "1h", "--mode=paper"}, Capital: 10000, ClosedBarDecisions: true}
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	s := NewStrategyState(sc)
	logger := silentStrategyLogger(sc.ID)
	defer logger.Close()

	if trades, _ := executeHyperliquidResult(sc, s, closedBarTestResult(1, 0, false), nil, "BUY", 40000, nil, &Config{}, HurstGateDecision{}, logger); trades != 1 {
		t.Fatalf("open cycle trades = %d, want 1", trades)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.Quantity <= 0 || pos.EntryATR != 0 {
		t.Fatalf("after open = %+v, want an open position with EntryATR 0", pos)
	}

	executeHyperliquidResult(sc, s, closedBarTestResult(0, 1584, true), nil, "HOLD", 40000, nil, &Config{}, HurstGateDecision{}, logger)
	if got := s.Positions["BTC"].EntryATR; got != 0 {
		t.Fatalf("EntryATR after a held cycle = %v, want 0 (a held decision has no closed bar to stamp from)", got)
	}

	executeHyperliquidResult(sc, s, closedBarTestResult(0, 1584, false), nil, "HOLD", 40000, nil, &Config{}, HurstGateDecision{}, logger)
	if got := s.Positions["BTC"].EntryATR; got != 1584 {
		t.Fatalf("EntryATR after the next closed-bar cycle = %v, want 1584", got)
	}

	executeHyperliquidResult(sc, s, closedBarTestResult(0, 1700, false), nil, "HOLD", 40000, nil, &Config{}, HurstGateDecision{}, logger)
	if got := s.Positions["BTC"].EntryATR; got != 1584 {
		t.Fatalf("EntryATR after a later cycle = %v, want the first stamp 1584 kept", got)
	}
}
