package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPortfolioWarningMessageLiveLikeSample is an end-to-end format check that
// mirrors the live scope state when margin DD crosses the warn threshold: a
// paused strategy with a frozen -$220 P&L must NOT be named as the lead
// contributor and the warning must show the "(N paused strateg...)" footnote.
func TestPortfolioWarningMessageLiveLikeSample(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "hl-vwap-eth-60", Type: "perps", Args: []string{"--mode=live"}, MarginPerTradeUSD: ptrF(50)},
		{ID: "hl-rmc-eth-live", Type: "perps", Args: []string{"--mode=live"}, MarginPerTradeUSD: ptrF(50), Paused: true},
		{ID: "hl-tcross-eth-live", Type: "perps", Args: []string{"--mode=live"}, MarginPerTradeUSD: ptrF(50), Paused: true},
		{ID: "manual-eth", Type: "manual", Args: []string{"--mode=live"}, InitialCapital: 100, Capital: 100},
	}
	state := NewAppState()
	state.Strategies["hl-vwap-eth-60"] = &StrategyState{
		ID: "hl-vwap-eth-60", Platform: "hyperliquid", InitialCapital: 50,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 28.75, DailyPnL: -15},
	}
	state.Strategies["hl-rmc-eth-live"] = &StrategyState{
		ID: "hl-rmc-eth-live", Platform: "hyperliquid", InitialCapital: 220,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 0, DailyPnL: -220.28},
	}
	state.Strategies["hl-tcross-eth-live"] = &StrategyState{
		ID: "hl-tcross-eth-live", Platform: "hyperliquid", InitialCapital: 50,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 0, DailyPnL: -8.12},
	}
	state.Strategies["manual-eth"] = &StrategyState{
		ID: "manual-eth", Platform: "hyperliquid", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 0, DailyPnL: -39.36},
	}
	// Realistic live-equity state from the recent /status snapshot
	state.PortfolioRisk = map[RiskPartition]*PortfolioRiskState{
		livePartition: {
			PeakValue:                1014.25,
			CurrentDrawdownPct:       9.85,
			CurrentMarginDrawdownPct: 30.5,
			WarningSent:              true,
			WarnBandEnteredAt:        time.Now().UTC().Add(-30 * time.Minute),
			LastWarningMarginDDPct:   30.4,
			WarningMarginDeltaPct:    0.1,
		},
	}

	prices := map[string]float64{"ETH": 2700}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Reason:        "portfolio perps margin drawdown 30.5% exceeds limit 30.0%",
		Config:        &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 100},
		State:         state,
		Partition:     livePartition,
		CfgStrategies: cfgStrategies,
		Prices:        prices,
		TotalValue:    913.7,
		PerpsMargin:   49.24,
		PerpsLoss:     15.02,
		Recent: []Trade{
			{StrategyID: "hl-vwap-eth-60", Side: "buy", Quantity: 0.361, Price: 2769.3, Timestamp: time.Now().Add(-10 * time.Minute), Details: "limit fill"},
		},
		Now:              time.Now().UTC(),
		EquityGuardArmed: true,
	})

	fmt.Println("===== BEGIN WARNING DM =====")
	fmt.Println(msg)
	fmt.Println("===== END WARNING DM =====")

	// Hard assertions on what must NOT and MUST be in the message.
	mustNotContain := []string{"hl-rmc-eth-live", "hl-tcross-eth-live"}
	for _, s := range mustNotContain {
		// find line(s) containing the strategy id in the contributors block.
		// We tolerate mentions outside Top contributors (e.g., Recent activity).
		// Filter out the recent activity block where it's allowed.
		withinContribs := false
		for _, line := range strings.Split(msg, "\n") {
			// Lines inside the ``` block following "Top contributors:" are the rule.
			if strings.HasPrefix(strings.TrimSpace(line), "Top contributors:") {
				withinContribs = true
				continue
			}
			if withinContribs && strings.HasPrefix(strings.TrimSpace(line), "```") {
				withinContribs = false
				continue
			}
			if withinContribs && strings.Contains(line, s) {
				t.Fatalf("paused strategy %q leaked into Top contributors block: %q", s, line)
			}
		}
	}

	if !strings.Contains(msg, "excluded from contributors") {
		t.Fatalf("expected '(N paused strateg...) excluded from contributors' footnote in warning, got:\n%s", msg)
	}
	if !strings.Contains(msg, "include_paused_in_warning=true") {
		t.Fatalf("expected footnote to mention the opt-in config key, got:\n%s", msg)
	}

	// Lead line must not name a paused strategy as "leading portfolio drawdown".
	if strings.Contains(msg, "hl-rmc-eth-live (dd=") || strings.Contains(msg, "hl-tcross-eth-live (dd=") {
		t.Fatalf("lead attribution named a paused strategy: %s", msg)
	}
}

func ptrF(v float64) *float64 { return &v }
