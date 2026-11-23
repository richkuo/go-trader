package main

import (
	"encoding/json"
	"testing"
	"time"
)

type paperFundingTestClock struct{ ms int64 }

func (c *paperFundingTestClock) set(ms int64)   { c.ms = ms }
func (c *paperFundingTestClock) now() time.Time { return time.UnixMilli(c.ms).UTC() }

func withPaperFundingClock(t *testing.T, startMs int64) *paperFundingTestClock {
	t.Helper()
	c := &paperFundingTestClock{ms: startMs}
	prev := paperFundingClock
	paperFundingClock = c.now
	t.Cleanup(func() { paperFundingClock = prev })
	return c
}

func paperFundingStateJSON(t *testing.T, s *StrategyState) string {
	t.Helper()
	raw, err := marshalPaperFundingState(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPaperFundingHelperTracksEveryBookChangeAndFlagsBypass(t *testing.T) {
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	sc := StrategyConfig{ID: "hl-p", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 10000,
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	state := NewAppState()
	s := NewStrategyState(sc)
	state.Strategies[sc.ID] = s
	syncPaperFundingEligibility(state, []StrategyConfig{sc}, clock.now())
	logger := silentStrategyLogger(sc.ID)

	clock.set(1_800_000_100_000)
	if n, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "", 0, DirectionBoth, 0, logger); err != nil || n != 1 {
		t.Fatalf("open: n=%d err=%v", n, err)
	}
	pid := s.Positions["ETH"].TradePositionID
	clock.set(1_800_000_200_000)
	if n, _ := applyPerpsScaleIn(s, sc, "ETH", 2010, 0.25, 0, "", false, logger); n != 1 {
		t.Fatal("scale-in did not book")
	}
	clock.set(1_800_000_300_000)
	applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionOpen, Side: "sell", HedgeSide: "short", Qty: 0.01, NewBasis: 0.75}, 0.01, 60000, 0, false, "", logger)
	clock.set(1_800_000_400_000)
	if n, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2020, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 0, logger); err != nil || n == 0 {
		t.Fatalf("flip: n=%d err=%v", n, err)
	}
	eth := s.PaperFunding.Coins["ETH"]
	if len(eth.Segments) != 2 || eth.Segments[0].SignedQty != 0.5 || eth.Segments[0].ToMs != 1_800_000_200_000 ||
		eth.Segments[1].SignedQty != 0.75 || eth.Segments[1].PositionID != pid || eth.Segments[1].ToMs != 1_800_000_400_000 {
		t.Fatalf("ETH segments must record 0.5 then 0.75 on the first position: %+v", eth.Segments)
	}
	if eth.Anchor.SignedQty >= 0 || eth.Anchor.PositionID == pid || eth.Anchor.SinceMs != 1_800_000_400_000 {
		t.Fatalf("a flip must anchor the new short position at the flip time: %+v", eth.Anchor)
	}
	if btc := s.PaperFunding.Coins["BTC"]; btc == nil || btc.Anchor.SignedQty != -0.01 || btc.Anchor.SinceMs != 1_800_000_300_000 {
		t.Fatalf("the hedge leg must be anchored on its own coin: %+v", btc)
	}
	if alerts := s.PaperFunding.drainAlerts(); len(alerts) != 0 {
		t.Fatalf("hooked changes must raise no alert: %+v", alerts)
	}

	clock.set(1_800_000_500_000)
	s.Positions["ETH"].Quantity *= 2
	clock.set(1_800_000_600_000)
	observePaperFunding(s, clock.ms)
	if len(eth.Unknown) != 1 || eth.Unknown[0].FromMs != 1_800_000_400_000 || eth.Unknown[0].ToMs != 1_800_000_600_000 {
		t.Fatalf("a direct book write must become an explicit unknown window from the last check: %+v", eth.Unknown)
	}
	alerts := s.PaperFunding.drainAlerts()
	if len(alerts) != 1 || !alerts[0].Critical || alerts[0].Kind != "helper_bypass" {
		t.Fatalf("a bypass must raise one CRITICAL alert: %+v", alerts)
	}

	live := hlLivePerps("hl-l", "ETH", 1000)
	ls := NewStrategyState(live)
	state.Strategies[live.ID] = ls
	syncPaperFundingEligibility(state, []StrategyConfig{sc, live}, clock.now())
	if _, err := ExecutePerpsSignalWithLeverage(ls, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.1, "", 0, DirectionBoth, 0, silentStrategyLogger(live.ID)); err != nil {
		t.Fatal(err)
	}
	if ls.PaperFunding != nil {
		t.Fatal("a live strategy must never gain paper funding state through the helper")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(paperFundingStateJSON(t, s)), &probe); err != nil {
		t.Fatalf("state must marshal to valid JSON: %v", err)
	}
}
