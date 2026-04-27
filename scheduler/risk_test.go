package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// yesterday returns the UTC date string for one day before today.
func yesterday() string {
	return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
}

// today returns the current UTC date string.
func todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

// newRiskState returns a minimal RiskState for testing.
func newRiskState(date string, dailyPnL float64) RiskState {
	return RiskState{
		DailyPnLDate: date,
		DailyPnL:     dailyPnL,
	}
}

// TestRolloverDailyPnL_SameDay verifies that PnL and date are unchanged when
// DailyPnLDate already equals today.
func TestRolloverDailyPnL_SameDay(t *testing.T) {
	r := newRiskState(todayUTC(), 123.45)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 123.45 {
		t.Errorf("expected DailyPnL=123.45 unchanged; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRolloverDailyPnL_NewDay verifies that DailyPnL is zeroed and DailyPnLDate
// is updated when the stored date is stale (e.g. yesterday).
func TestRolloverDailyPnL_NewDay(t *testing.T) {
	r := newRiskState(yesterday(), 99.99)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRolloverDailyPnL_EmptyDate verifies that an empty DailyPnLDate (e.g. freshly
// initialized state) is treated as stale and the day is properly initialized.
func TestRolloverDailyPnL_EmptyDate(t *testing.T) {
	r := newRiskState("", 50.0)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 on empty date; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRecordTradeResult_MidnightCrossing is the core issue-27 regression test.
// It simulates a scenario where a trade is recorded without a prior CheckRisk
// call after midnight: DailyPnLDate is yesterday, so RecordTradeResult must
// roll over the day before accumulating the new trade's PnL.
func TestRecordTradeResult_MidnightCrossing(t *testing.T) {
	r := newRiskState(yesterday(), 200.0) // stale — prior day PnL should be discarded

	RecordTradeResult(&r, 50.0)

	if r.DailyPnL != 50.0 {
		t.Errorf("expected DailyPnL=50 after midnight crossing; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
	if r.TotalTrades != 1 {
		t.Errorf("expected TotalTrades=1; got %d", r.TotalTrades)
	}
}

// TestRecordTradeResult_SameDayAccumulation verifies that multiple trades on the
// same day correctly accumulate DailyPnL without any spurious resets.
func TestRecordTradeResult_SameDayAccumulation(t *testing.T) {
	r := newRiskState(todayUTC(), 100.0)

	RecordTradeResult(&r, 30.0)
	RecordTradeResult(&r, -10.0)

	if r.DailyPnL != 120.0 {
		t.Errorf("expected DailyPnL=120 after two trades; got %.2f", r.DailyPnL)
	}
	if r.TotalTrades != 2 {
		t.Errorf("expected TotalTrades=2; got %d", r.TotalTrades)
	}
}

// TestCheckRisk_RollsOverDailyPnL verifies that CheckRisk itself also triggers
// day rollover so the risk check always operates on the correct day's budget.
func TestCheckRisk_RollsOverDailyPnL(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(yesterday(), 500.0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 50.0

	CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if s.RiskState.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 by CheckRisk; got %.2f", s.RiskState.DailyPnL)
	}
	if s.RiskState.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), s.RiskState.DailyPnLDate)
	}
}

// TestCheckRisk_ForceCloseOnDrawdown verifies that positions are liquidated when
// the max drawdown circuit breaker fires.
func TestCheckRisk_ForceCloseOnDrawdown(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:      10000.0,
			MaxDrawdownPct: 20.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		InitialCapital: 10000.0,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-60000-2026-03-01": {
				ID:              "BTC-call-60000-2026-03-01",
				Action:          "buy",
				Quantity:        1,
				EntryPremiumUSD: 1000.0,
				CurrentValueUSD: 500.0,
			},
			"BTC-put-50000-2026-03-01": {
				ID:              "BTC-put-50000-2026-03-01",
				Action:          "sell",
				Quantity:        1,
				EntryPremiumUSD: 600.0,
				CurrentValueUSD: -800.0,
			},
		},
		TradeHistory: []Trade{},
	}

	// BTC at $45000 → portfolio ≈ $5000 + 0.1*45000 + 500 + (-800) = $5000+4500+500-800 = $9200
	// drawdown = (10000-9200)/10000 = 8% → below 20% threshold
	// We need drawdown > 20%, so use BTC=$30000:
	// portfolio = $5000 + 0.1*30000 + 500 + (-800) = $5000+3000+500-800 = $7700
	// drawdown = (10000-7700)/10000 = 23% > 20% ✓
	prices := map[string]float64{"BTC": 30000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(nil, s, pv, prices, nil, nil)

	if allowed {
		t.Error("expected CheckRisk to return false on drawdown breach")
	}
	if len(reason) == 0 {
		t.Error("expected non-empty reason")
	}

	// All positions should be closed
	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.OptionPositions) != 0 {
		t.Errorf("expected OptionPositions empty after force-close; got %d entries", len(s.OptionPositions))
	}

	// 3 trades recorded (1 spot + 2 options)
	if len(s.TradeHistory) != 3 {
		t.Errorf("expected 3 trades in history; got %d", len(s.TradeHistory))
	}

	// RiskState.TotalTrades incremented by 3 (was 1, now 4)
	if s.RiskState.TotalTrades != 4 {
		t.Errorf("expected TotalTrades=4; got %d", s.RiskState.TotalTrades)
	}

	// Cash: started $5000
	// + long BTC close: 0.1 * 30000 = $3000 → pnl = 3000 - 0.1*50000 = -$2000
	// + bought call close: +$500 → pnl = 500 - 1000 = -$500
	// + sold put close: buyback = 800 → cash -= 800 → pnl = 600 - 800 = -$200
	// expected Cash = 5000 + 3000 + 500 - 800 = $7700
	expectedCash := 7700.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}

// TestCheckPortfolioRisk_DrawdownKillSwitch verifies the kill switch fires at the
// drawdown threshold and latches on subsequent calls.
func TestCheckPortfolioRisk_DrawdownKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 0, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Just under threshold — should be allowed.
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 7600.0, 0, 0, 0)
	if !allowed {
		t.Errorf("expected allowed below threshold; got reason=%s", reason)
	}
	if nb {
		t.Error("expected notionalBlocked=false")
	}

	// Peak should not change (value dropped).
	if prs.PeakValue != 10000.0 {
		t.Errorf("expected peak=10000; got %.2f", prs.PeakValue)
	}

	// Drawdown = (10000-7400)/10000 = 26% > 25% — kill switch fires.
	allowed, nb, _, reason = CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to fire at 26% drawdown")
	}
	if nb {
		t.Error("expected notionalBlocked=false when kill switch fires")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true after firing")
	}
	if prs.KillSwitchAt.IsZero() {
		t.Error("expected KillSwitchAt to be set")
	}

	// Subsequent call — still latched even with recovered value.
	allowed, _, _, _ = CheckPortfolioRisk(prs, cfg, 10000.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to remain latched on subsequent call")
	}
}

// TestCheckPortfolioRisk_NotionalCap verifies the notional cap blocks new trades
// without triggering the kill switch.
func TestCheckPortfolioRisk_NotionalCap(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 50000, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Under cap — allowed, not notional-blocked.
	allowed, nb, _, _ := CheckPortfolioRisk(prs, cfg, 10000.0, 30000.0, 0, 0)
	if !allowed {
		t.Error("expected allowed under notional cap")
	}
	if nb {
		t.Error("expected notionalBlocked=false under cap")
	}

	// Over cap — allowed=true, notionalBlocked=true, kill switch NOT active.
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0, 0, 0)
	if !allowed {
		t.Error("expected allowed=true (notional cap doesn't kill switch)")
	}
	if !nb {
		t.Errorf("expected notionalBlocked=true over cap; reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected kill switch NOT fired for notional cap breach")
	}
}

// TestCheckPortfolioRisk_PeakTracking verifies the peak high-water mark only
// ratchets upward, never down.
func TestCheckPortfolioRisk_PeakTracking(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 50, MaxNotionalUSD: 0, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 5000.0}

	// Value rises — peak should update.
	CheckPortfolioRisk(prs, cfg, 8000.0, 0, 0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 after rise; got %.2f", prs.PeakValue)
	}

	// Value drops — peak should NOT update.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0, 0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 unchanged after drop; got %.2f", prs.PeakValue)
	}

	// Value rises again — peak updates.
	CheckPortfolioRisk(prs, cfg, 9000.0, 0, 0, 0)
	if prs.PeakValue != 9000.0 {
		t.Errorf("expected peak=9000 after new high; got %.2f", prs.PeakValue)
	}

	// Drawdown tracked correctly: (9000-6000)/9000 ≈ 33.3%.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0, 0, 0)
	expectedDD := (9000.0 - 6000.0) / 9000.0 * 100
	if prs.CurrentDrawdownPct < expectedDD-0.01 || prs.CurrentDrawdownPct > expectedDD+0.01 {
		t.Errorf("expected drawdown≈%.2f%%; got %.2f%%", expectedDD, prs.CurrentDrawdownPct)
	}
}

// TestPortfolioNotional verifies notional computation for spot + sold options +
// bought options.
func TestPortfolioNotional(t *testing.T) {
	strategies := map[string]*StrategyState{
		"spot-strat": {
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000.0, Side: "long"},
				"ETH": {Symbol: "ETH", Quantity: 10.0, AvgCost: 3000.0, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"options-strat": {
			Positions: make(map[string]*Position),
			OptionPositions: map[string]*OptionPosition{
				"BTC-put-40000-sell": {
					Action:          "sell",
					Strike:          40000.0,
					Quantity:        2.0,
					CurrentValueUSD: -500.0,
				},
				"BTC-call-50000-buy": {
					Action:          "buy",
					Strike:          50000.0,
					Quantity:        1.0,
					CurrentValueUSD: 800.0,
				},
			},
		},
	}

	prices := map[string]float64{
		"BTC": 50000.0,
		"ETH": 3500.0,
	}

	notional := PortfolioNotional(strategies, prices)

	// Spot: 0.5*50000 + 10*3500 = 25000 + 35000 = 60000
	// Sold put: 40000 * 2 = 80000
	// Bought call: CurrentValueUSD = 800 (positive)
	// Total = 60000 + 80000 + 800 = 140800
	expected := 140800.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected notional=%.2f; got %.2f", expected, notional)
	}
}

// TestPortfolioNotional_IncludesPerps verifies that perps positions (keyed
// by base asset, e.g. "BTC" for Hyperliquid/OKX) are included in notional
// exposure once their fetch price has been mirrored into the position key.
// Regression test for issue #245: before the fix, perps notional was
// frozen at pos.AvgCost because the symbolSet builder only picked up spot
// strategies, so prices[sym] missed for perps and the function fell back
// to entry cost.
func TestPortfolioNotional_IncludesPerps(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-momentum-btc": {
			Type: "perps",
			Positions: map[string]*Position{
				// Hyperliquid perps store positions under the base asset.
				"BTC": {Symbol: "BTC", Quantity: 0.4, AvgCost: 40000.0, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"spot-btc": {
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, AvgCost: 45000.0, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}

	// Simulate the mirrored prices map after collectPriceSymbols +
	// mirrorPerpsPrices: "BTC/USDT" is the fetch key, "BTC" the alias.
	prices := map[string]float64{
		"BTC/USDT": 50000.0,
		"BTC":      50000.0,
	}

	notional := PortfolioNotional(strategies, prices)

	// Perps: 0.4 * 50000 = 20000
	// Spot:  0.1 * 50000 =  5000
	// Total: 25000
	expected := 25000.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected notional=%.2f; got %.2f", expected, notional)
	}
}

// TestPortfolioNotional_IncludesFutures verifies that TopStep/CME futures
// positions (Type="futures", Multiplier > 0, keyed under the bare contract
// symbol like "ES") are revalued in notional at the live mark rather than
// frozen at pos.AvgCost. Regression test for issue #261: before the fix,
// collectPriceSymbols handled only spot + perps, so futures positions had
// no entry in the prices map and PortfolioNotional fell back to AvgCost —
// after a rally this understated exposure, after a drawdown it overstated
// it, breaking the portfolio-notional kill switch for TopStep strategies.
func TestPortfolioNotional_IncludesFutures(t *testing.T) {
	strategies := map[string]*StrategyState{
		"ts-trend-es": {
			Type: "futures",
			Positions: map[string]*Position{
				// TopStep futures: 2 ES contracts long, entry 5000, multiplier 50.
				"ES": {Symbol: "ES", Quantity: 2, AvgCost: 5000.0, Side: "long", Multiplier: 50},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"ts-mr-nq": {
			Type: "futures",
			Positions: map[string]*Position{
				// 1 NQ contract short, entry 18000, multiplier 20.
				"NQ": {Symbol: "NQ", Quantity: 1, AvgCost: 18000.0, Side: "short", Multiplier: 20},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}

	// Simulate the prices map after fetch_futures_marks.py has merged
	// live TopStep adapter quotes. Both marks diverge from entry — that
	// is exactly what the fix unlocks for the notional computation.
	prices := map[string]float64{
		"ES": 5100.0,
		"NQ": 18500.0,
	}

	notional := PortfolioNotional(strategies, prices)

	// ES long: 2 * 50 * 5100 = 510000
	// NQ short: 1 * 20 * 18500 = 370000 (absolute notional, sign-agnostic)
	// Total:    880000
	expected := 880000.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected futures notional at live mark=%.2f; got %.2f", expected, notional)
	}

	// Guard the regression: the buggy pre-fix computation would have used
	// pos.AvgCost, so assert the result is NOT equal to the frozen-entry
	// notional (2*50*5000 + 1*20*18000 = 500000 + 360000 = 860000).
	frozen := 860000.0
	if notional == frozen {
		t.Errorf("notional equals frozen-entry value %.2f — mark price was not applied", frozen)
	}
}

// TestPortfolioNotional_FuturesMarkMiss verifies graceful degradation
// when fetch_futures_marks.py returns no price for a symbol: the function
// must fall back to pos.AvgCost (pre-fix behavior) rather than double-
// counting or crashing. This is the acceptance-criteria fallback path —
// the kill switch degrades toward stale exposure, not a cycle skip.
func TestPortfolioNotional_FuturesMarkMiss(t *testing.T) {
	strategies := map[string]*StrategyState{
		"ts-trend-cl": {
			Type: "futures",
			Positions: map[string]*Position{
				"CL": {Symbol: "CL", Quantity: 1, AvgCost: 80.0, Side: "long", Multiplier: 1000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	// Empty prices map — simulates fetch_futures_marks.py failing or
	// omitting this symbol.
	notional := PortfolioNotional(strategies, map[string]float64{})

	// Fallback: 1 * 1000 * 80 (entry) = 80000
	expected := 80000.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected fallback notional=%.2f; got %.2f", expected, notional)
	}
}

// TestCollectFuturesMarkSymbols verifies that only futures strategies
// contribute to the CME mark fetch list and that duplicate symbols are
// deduplicated. Spot/perps/options must NOT appear — they live on the
// check_price.py rail, not fetch_futures_marks.py.
func TestCollectFuturesMarkSymbols(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "ts-trend-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
		{ID: "ts-mr-es", Type: "futures", Platform: "topstep", Args: []string{"mean_rev", "ES", "15m"}}, // dup symbol
		{ID: "ts-trend-nq", Type: "futures", Platform: "topstep", Args: []string{"trend", "NQ", "1h"}},
		{ID: "ts-trend-mes", Type: "futures", Platform: "topstep", Args: []string{"trend", "MES", "1h"}},
		// Non-futures strategies must be ignored.
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
		{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
		{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
		// Short-arg futures strategy should be ignored (early return
		// guard at risk.go len(sc.Args) < 2).
		{ID: "ts-short", Type: "futures", Platform: "topstep", Args: []string{"trend"}},
		// Empty-symbol futures strategy should be ignored (early return
		// guard at risk.go sym == "") — explicit coverage of that branch
		// which the short-arg case above cannot reach.
		{ID: "ts-empty-sym", Type: "futures", Platform: "topstep", Args: []string{"trend", "", "1h"}},
		// Non-topstep futures platform must be filtered out:
		// fetch_futures_marks.py hardcodes TopStepExchangeAdapter, so
		// routing a hypothetical IBKR futures symbol through it would
		// either fail outright or resolve against the wrong contract.
		// Use a symbol distinct from the topstep entries so a filter
		// bypass would leak "CL" into the result and fail this test.
		{ID: "ibkr-trend-cl", Type: "futures", Platform: "ibkr", Args: []string{"trend", "CL", "1h"}},
	}

	got := collectFuturesMarkSymbols(strategies)
	want := []string{"ES", "MES", "NQ"} // sorted
	if len(got) != len(want) {
		t.Fatalf("got %d symbols %v, want %d %v", len(got), got, len(want), want)
	}
	for i, sym := range want {
		if got[i] != sym {
			t.Errorf("got[%d]=%q, want %q (full: %v)", i, got[i], sym, got)
		}
	}
}

// TestMergeFuturesMarks verifies that mergeFuturesMarks copies non-zero
// marks into the shared prices map, preserves existing entries (so a
// live mark already published during the cycle wins over a fetcher
// fallback), and skips zero/negative values.
func TestMergeFuturesMarks(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 50000.0, // unrelated spot, must be untouched
		"ES":       5120.5,  // strategy already published live mark — must win
	}
	marks := map[string]float64{
		"ES":  5100.0, // stale, must not overwrite
		"NQ":  18500.0,
		"MES": 0.0, // missing/failed — must be skipped
		"CL":  -1,  // bogus — must be skipped
	}

	mergeFuturesMarks(prices, marks)

	if prices["BTC/USDT"] != 50000.0 {
		t.Errorf("prices[BTC/USDT] = %v, want 50000 (unrelated entry mutated)", prices["BTC/USDT"])
	}
	if prices["ES"] != 5120.5 {
		t.Errorf("prices[ES] = %v, want 5120.5 (existing live mark must win)", prices["ES"])
	}
	if prices["NQ"] != 18500.0 {
		t.Errorf("prices[NQ] = %v, want 18500 (new mark must be merged)", prices["NQ"])
	}
	if _, ok := prices["MES"]; ok {
		t.Errorf("prices[MES] should not be set when mark is zero (got %v)", prices["MES"])
	}
	if _, ok := prices["CL"]; ok {
		t.Errorf("prices[CL] should not be set when mark is negative (got %v)", prices["CL"])
	}
}

// TestPortfolioNotional_IncludesPerpsShort verifies that a perps short
// also contributes positive exposure to notional (absolute-value
// interpretation) and is revalued at the live mark rather than frozen at
// entry cost. HL shorts are stored with positive Quantity + Side:"short"
// (see hyperliquid_balance.go syncs the on-chain |Size|), so the
// pre-fix fallback to AvgCost would have understated notional after a
// price rally and overstated it after a drawdown — this pins the fix
// against the sign path, not just longs.
func TestPortfolioNotional_IncludesPerpsShort(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-mean-rev-eth": {
			Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 2.0, AvgCost: 3000.0, Side: "short"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	// Live mark diverges from entry — this is what the fix unlocks.
	prices := map[string]float64{
		"ETH/USDT": 3200.0,
		"ETH":      3200.0,
	}

	notional := PortfolioNotional(strategies, prices)

	// Short notional at live mark: 2.0 * 3200 = 6400 (not 2.0 * 3000 = 6000).
	expected := 6400.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected short notional at live mark=%.2f; got %.2f", expected, notional)
	}
}

// TestCollectPriceSymbols verifies that only spot strategies contribute to the
// BinanceUS fetch list (#263). Perps strategies must NOT appear — they are
// sourced from venue-native marks via collectPerpsMarkSymbols. Options and
// short-arg strategies are also excluded.
func TestCollectPriceSymbols(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
		{ID: "sma-eth", Type: "spot", Platform: "binanceus", Args: []string{"sma", "ETH/USDT", "1h"}},
		// Perps must NOT appear in the BinanceUS fetch list — venue-native marks only.
		{ID: "hl-momentum-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "okx-ema-sol-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "SOL", "1h"}},
		// Options must be ignored.
		{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
		// Short-arg strategies must be ignored.
		{ID: "short", Type: "spot", Args: []string{"sma"}},
	}

	symbols := collectPriceSymbols(strategies)

	got := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		got[s] = true
	}

	// Only spot symbols should appear.
	wantSymbols := []string{"BTC/USDT", "ETH/USDT"}
	for _, sym := range wantSymbols {
		if !got[sym] {
			t.Errorf("symbols missing %q; got %v", sym, symbols)
		}
	}
	if len(symbols) != len(wantSymbols) {
		t.Errorf("symbols len = %d (%v), want %d (%v)", len(symbols), symbols, len(wantSymbols), wantSymbols)
	}

	// Perps base coins must NOT appear in the spot fetch list.
	for _, notWanted := range []string{"BTC", "SOL", "BTC/USDT:USDT", "SOL/USDT"} {
		if got[notWanted] {
			t.Errorf("symbol %q should not be in the BinanceUS fetch list (perps now venue-native)", notWanted)
		}
	}
}

// TestCollectPerpsMarkSymbols verifies that collectPerpsMarkSymbols splits
// HL and OKX perps into separate slices, deduplicates symbols, sorts them,
// and ignores spot/options/futures/short-arg strategies.
func TestCollectPerpsMarkSymbols(t *testing.T) {
	strategies := []StrategyConfig{
		// HL perps — two strategies on the same coin to test dedup.
		{ID: "hl-momentum-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-mr-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"mean_rev", "BTC", "15m"}},
		{ID: "hl-trend-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "ETH", "1h"}},
		// OKX perps.
		{ID: "okx-ema-sol-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "SOL", "1h"}},
		{ID: "okx-ema-btc-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "BTC", "1h"}},
		// Non-perps — all must be ignored.
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
		{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
		{ID: "ts-trend-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
		// Short-arg perps must be ignored.
		{ID: "hl-short", Type: "perps", Platform: "hyperliquid", Args: []string{"trend"}},
		// Empty-symbol perps must be ignored.
		{ID: "hl-empty", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "", "1h"}},
	}

	hlCoins, okxCoins := collectPerpsMarkSymbols(strategies)

	// HL: BTC (dedup'd) + ETH, sorted.
	wantHL := []string{"BTC", "ETH"}
	if len(hlCoins) != len(wantHL) {
		t.Fatalf("hlCoins = %v, want %v", hlCoins, wantHL)
	}
	for i, c := range wantHL {
		if hlCoins[i] != c {
			t.Errorf("hlCoins[%d] = %q, want %q", i, hlCoins[i], c)
		}
	}

	// OKX: BTC + SOL, sorted.
	wantOKX := []string{"BTC", "SOL"}
	if len(okxCoins) != len(wantOKX) {
		t.Fatalf("okxCoins = %v, want %v", okxCoins, wantOKX)
	}
	for i, c := range wantOKX {
		if okxCoins[i] != c {
			t.Errorf("okxCoins[%d] = %q, want %q", i, okxCoins[i], c)
		}
	}
}

// TestCollectPerpsMarkSymbols_Empty verifies that collectPerpsMarkSymbols
// returns nil slices (no allocation) when no perps strategies are configured.
func TestCollectPerpsMarkSymbols_Empty(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
	}
	hlCoins, okxCoins := collectPerpsMarkSymbols(strategies)
	if len(hlCoins) != 0 {
		t.Errorf("hlCoins = %v, want empty", hlCoins)
	}
	if len(okxCoins) != 0 {
		t.Errorf("okxCoins = %v, want empty", okxCoins)
	}
}

// TestMergePerpsMarks verifies that mergePerpsMarks copies non-zero marks
// into the shared prices map, preserves existing entries (strategy-published
// mark wins over a fetcher snapshot), and skips zero/negative values.
func TestMergePerpsMarks(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 50000.0, // unrelated spot — must be untouched
		"ETH":      3199.5,  // strategy already published live mark — must win
	}
	marks := map[string]float64{
		"ETH":  3200.1, // stale — must not overwrite the existing live mark
		"BTC":  67500.5,
		"SOL":  0,  // zero — must be skipped
		"DOGE": -1, // negative — must be skipped
	}

	mergePerpsMarks(prices, marks)

	if prices["BTC/USDT"] != 50000.0 {
		t.Errorf("prices[BTC/USDT] = %v, want 50000 (unrelated entry mutated)", prices["BTC/USDT"])
	}
	if prices["ETH"] != 3199.5 {
		t.Errorf("prices[ETH] = %v, want 3199.5 (existing live mark must win)", prices["ETH"])
	}
	if prices["BTC"] != 67500.5 {
		t.Errorf("prices[BTC] = %v, want 67500.5 (new mark must be merged)", prices["BTC"])
	}
	if _, ok := prices["SOL"]; ok {
		t.Errorf("prices[SOL] should not be set when mark is zero (got %v)", prices["SOL"])
	}
	if _, ok := prices["DOGE"]; ok {
		t.Errorf("prices[DOGE] should not be set when mark is negative (got %v)", prices["DOGE"])
	}
}

// TestCheckRisk_ConsecutiveLossesForceClose verifies that the consecutive-losses
// circuit breaker force-closes all open positions.
func TestCheckRisk_ConsecutiveLossesForceClose(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:         10000.0,
			MaxDrawdownPct:    50.0,
			TotalTrades:       5,
			ConsecutiveLosses: 5,
			DailyPnLDate:      todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	prices := map[string]float64{"BTC": 50000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(nil, s, pv, prices, nil, nil)

	if allowed {
		t.Errorf("expected circuit breaker to fire; reason=%s", reason)
	}

	// Positions must be force-closed
	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded for force-close; got %d", len(s.TradeHistory))
	}
	// BTC long: proceeds = 0.1 * 50000 = 5000, cash = 5000 + 5000 = 10000
	expectedCash := 10000.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}

// TestCheckPortfolioRisk_WarningFires verifies that drawdown at 80% of limit
// triggers a warning on every call while the portfolio remains in the warning
// band.
func TestCheckPortfolioRisk_WarningFires(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Warn threshold = 25 * 80/100 = 20%. Drawdown = (10000-7900)/10000 = 21% > 20%.
	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true at 21% drawdown (warn threshold=20%)")
	}
	if reason == "" {
		t.Error("expected non-empty reason for warning")
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent=true after warning fires")
	}

	// Second call at same drawdown — warning should fire again so operators get
	// a reminder each cycle while the account remains in the warning band.
	_, _, warning, reason = CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true on second call while still in warning band")
	}
	if reason == "" {
		t.Error("expected non-empty reason for repeated warning")
	}
}

// TestCheckPortfolioRisk_WarningRepeatsAcrossCycles verifies that warning
// fires on every cycle while drawdown remains in the warn band, even with no
// recovery in between.
func TestCheckPortfolioRisk_WarningRepeatsAcrossCycles(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Warn threshold = 20%. Hold portfolio at 21% drawdown across many cycles.
	for i := 0; i < 5; i++ {
		_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
		if !warning {
			t.Errorf("cycle %d: expected warning=true while in warn band", i)
		}
		if reason == "" {
			t.Errorf("cycle %d: expected non-empty reason", i)
		}
		if !prs.WarningSent {
			t.Errorf("cycle %d: expected WarningSent=true while in warn band", i)
		}
	}
}

// TestCheckPortfolioRisk_WarnBandEnteredTransition verifies that the
// prevWarningSent snapshot pattern used by main.go correctly identifies only
// the first cycle as a warn-band entry. This prevents the kill-switch event
// log from being flooded by repeat "warning" entries while drawdown stays in
// the warn band.
func TestCheckPortfolioRisk_WarnBandEnteredTransition(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	for i := 0; i < 5; i++ {
		prevWarningSent := prs.WarningSent
		_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
		enteredWarnBand := warning && !prevWarningSent
		if i == 0 {
			if !enteredWarnBand {
				t.Error("cycle 0: expected enteredWarnBand=true on first entry")
			}
		} else {
			if enteredWarnBand {
				t.Errorf("cycle %d: expected enteredWarnBand=false while already in warn band", i)
			}
		}
	}

	// After recovery, re-entering the band should produce enteredWarnBand=true again.
	CheckPortfolioRisk(prs, cfg, 8500.0, 0, 0, 0) // recover below warn threshold
	prevWarningSent := prs.WarningSent
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true after re-crossing warn threshold")
	}
	if !warning || prevWarningSent {
		t.Error("expected enteredWarnBand=true on re-entry after recovery")
	}
}

// TestCheckPortfolioRisk_WarningResetOnRecovery verifies that recovery below
// the warning threshold resets WarningSent.
func TestCheckPortfolioRisk_WarningResetOnRecovery(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Trigger warning at 21% drawdown.
	CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !prs.WarningSent {
		t.Fatal("expected WarningSent=true after first warning")
	}

	// Recover to 15% drawdown (below 20% warn threshold).
	CheckPortfolioRisk(prs, cfg, 8500.0, 0, 0, 0)
	if prs.WarningSent {
		t.Error("expected WarningSent=false after recovery below warn threshold")
	}

	// Cross warning threshold again — should warn again.
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true after recovery and re-crossing threshold")
	}
}

// TestCheckPortfolioRisk_WarningNotAfterKillSwitch verifies that past the kill
// threshold the kill switch fires and no warning is returned.
func TestCheckPortfolioRisk_WarningNotAfterKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// 26% drawdown > 25% kill switch threshold.
	allowed, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to fire")
	}
	if warning {
		t.Error("expected warning=false when kill switch fires (kill takes precedence)")
	}
}

// TestAddKillSwitchEvent_MaxCap verifies that events are capped at maxKillSwitchEvents.
func TestAddKillSwitchEvent_MaxCap(t *testing.T) {
	prs := &PortfolioRiskState{}

	for i := 0; i < 60; i++ {
		addKillSwitchEvent(prs, "warning", "equity", float64(i), 1000, 2000, "test")
	}

	if len(prs.Events) != maxKillSwitchEvents {
		t.Errorf("expected %d events; got %d", maxKillSwitchEvents, len(prs.Events))
	}
	// Oldest event should be the 11th one added (index 10).
	if prs.Events[0].DrawdownPct != 10 {
		t.Errorf("expected oldest event drawdown=10; got %.0f", prs.Events[0].DrawdownPct)
	}
}

// TestCheckPortfolioRisk_EventLoggedOnTrigger verifies that a "triggered" event
// is appended when the kill switch fires.
func TestCheckPortfolioRisk_EventLoggedOnTrigger(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)

	if len(prs.Events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(prs.Events))
	}
	if prs.Events[0].Type != "triggered" {
		t.Errorf("expected event type='triggered'; got %q", prs.Events[0].Type)
	}
	if prs.Events[0].PortfolioValue != 7400.0 {
		t.Errorf("expected portfolio_value=7400; got %.2f", prs.Events[0].PortfolioValue)
	}
}

// --- ClearLatchedKillSwitchSharedWallet (#244) ---

// latchedSharedWalletState builds an AppState with a latched kill switch and
// shared-wallet strategies for use in #244 regression tests.
func latchedSharedWalletState() *AppState {
	return &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: PortfolioRiskState{
			PeakValue:          10000,
			CurrentDrawdownPct: 50,
			KillSwitchActive:   true,
			KillSwitchAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		},
	}
}

func sharedHLStrategies() []StrategyConfig {
	return []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
	}
}

// TestClearLatchedKillSwitchSharedWallet_Success verifies the kill switch is
// cleared when a shared wallet's real balance is fetched successfully, and
// that PeakValue is re-baselined so the next CheckPortfolioRisk call does
// not immediately re-latch the switch (#244 regression).
func TestClearLatchedKillSwitchSharedWallet_Success(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies()

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		if platform != "hyperliquid" {
			t.Errorf("expected fetcher called for hyperliquid; got %q", platform)
		}
		return 4500, nil
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if !cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return true")
	}
	if calls != 1 {
		t.Errorf("expected 1 fetcher call; got %d", calls)
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false after clear")
	}
	if !state.PortfolioRisk.KillSwitchAt.IsZero() {
		t.Errorf("expected KillSwitchAt zeroed; got %v", state.PortfolioRisk.KillSwitchAt)
	}
	if state.PortfolioRisk.WarningSent {
		t.Error("expected WarningSent reset to false")
	}
	// Peak should be re-baselined from the fetched balance (was 10000, now 4500).
	if state.PortfolioRisk.PeakValue != 4500 {
		t.Errorf("expected PeakValue re-baselined to 4500; got %.2f", state.PortfolioRisk.PeakValue)
	}
	if state.PortfolioRisk.CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct reset to 0; got %.2f", state.PortfolioRisk.CurrentDrawdownPct)
	}
	if len(state.PortfolioRisk.Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.PortfolioRisk.Events))
	}
	evt := state.PortfolioRisk.Events[0]
	if evt.Type != "auto_reset" {
		t.Errorf("expected event type=auto_reset; got %q", evt.Type)
	}
	if evt.PortfolioValue != 4500 {
		t.Errorf("expected event portfolio_value=4500 (fetched balance); got %.2f", evt.PortfolioValue)
	}
	if evt.PeakValue != 4500 {
		t.Errorf("expected event peak_value=4500 (re-baselined); got %.2f", evt.PeakValue)
	}
}

// TestClearLatchedKillSwitchSharedWallet_NoRelatchOnNextTick is the core
// #244 regression test: after an auto-clear, the very next CheckPortfolioRisk
// call must NOT re-latch the kill switch using the stale inflated PeakValue.
// This reproduces the exact scenario from the issue — a $20K peak from
// shared-wallet double-counting against a real $5K balance.
func TestClearLatchedKillSwitchSharedWallet_NoRelatchOnNextTick(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: PortfolioRiskState{
			PeakValue:          20000, // inflated (double-counted)
			CurrentDrawdownPct: 75,
			KillSwitchActive:   true,
			KillSwitchAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	strategies := sharedHLStrategies()

	// Real balance is $5K — well below the stale $20K peak.
	fetcher := func(platform string) (float64, error) {
		return 5000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected auto-clear to succeed")
	}

	// First tick after restart: CheckPortfolioRisk with real balance ~= $5K.
	// With a properly re-baselined peak, drawdown is 0% and the kill switch
	// stays cleared. With the old buggy behavior (peak still $20K), drawdown
	// would be 75% and the kill switch would re-latch immediately.
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	allowed, _, _, reason := CheckPortfolioRisk(&state.PortfolioRisk, cfg, 5000, 0, 0, 0)
	if !allowed {
		t.Fatalf("expected kill switch to stay cleared after auto-clear; got reason=%s", reason)
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false after first post-clear tick — stale peak re-latched the switch")
	}
}

// TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch verifies
// that a network/config failure on the balance fetch leaves the kill switch
// latched (acceptance criterion #2).
func TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies()
	originalLatchedAt := state.PortfolioRisk.KillSwitchAt

	fetcher := func(platform string) (float64, error) {
		return 0, fmt.Errorf("simulated network failure")
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return false on fetch failure")
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true after fetch failure")
	}
	if !state.PortfolioRisk.KillSwitchAt.Equal(originalLatchedAt) {
		t.Errorf("expected KillSwitchAt unchanged; got %v", state.PortfolioRisk.KillSwitchAt)
	}
	if len(state.PortfolioRisk.Events) != 0 {
		t.Errorf("expected no audit event on failure; got %d", len(state.PortfolioRisk.Events))
	}
}

// TestClearLatchedKillSwitchSharedWallet_NoSharedWalletNoOp verifies that
// non-shared-wallet setups are unaffected (acceptance criterion #3).
func TestClearLatchedKillSwitchSharedWallet_NoSharedWalletNoOp(t *testing.T) {
	state := latchedSharedWalletState()
	// Strategies without capital_pct (or only one strategy on a wallet) are
	// not "shared" — there is no double-counting risk to recover from.
	strategies := []StrategyConfig{
		{ID: "spot-a", Platform: "binanceus", Capital: 1000},
		{ID: "spot-b", Platform: "binanceus", Capital: 1000},
		{ID: "hl-solo", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
	}

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		return 5000, nil
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Error("expected no clear when no shared wallet detected")
	}
	if calls != 0 {
		t.Errorf("expected fetcher NOT called for non-shared wallets; got %d calls", calls)
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true")
	}
}

// TestClearLatchedKillSwitchSharedWallet_InactiveSwitchNoOp verifies the
// helper is a no-op (and skips the network fetch entirely) when the kill
// switch is not active.
func TestClearLatchedKillSwitchSharedWallet_InactiveSwitchNoOp(t *testing.T) {
	state := &AppState{
		PortfolioRisk: PortfolioRiskState{PeakValue: 10000, KillSwitchActive: false},
	}
	strategies := sharedHLStrategies()

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		return 5000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); cleared {
		t.Error("expected no clear when switch already inactive")
	}
	if calls != 0 {
		t.Errorf("expected fetcher NOT called when switch inactive; got %d calls", calls)
	}
}

// TestClearLatchedKillSwitchSharedWallet_MultiPlatformAllSuccess verifies
// that when multiple shared-wallet platforms are configured, the kill
// switch is cleared and PeakValue is re-baselined to the SUM of all
// fetched balances (not just the first).
func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAllSuccess(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "okx-a", Platform: "okx", CapitalPct: 0.3, Capital: 300},
		{ID: "okx-b", Platform: "okx", CapitalPct: 0.7, Capital: 700},
	}

	fetcher := func(platform string) (float64, error) {
		switch platform {
		case "hyperliquid":
			return 3000, nil
		case "okx":
			return 2000, nil
		}
		return 0, fmt.Errorf("unexpected platform %q", platform)
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected kill switch to clear when all platforms fetch successfully")
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false")
	}
	// PeakValue must be re-baselined to the SUM (3000 + 2000 = 5000), not
	// just the first platform's balance.
	if state.PortfolioRisk.PeakValue != 5000 {
		t.Errorf("expected PeakValue=5000 (sum of hyperliquid+okx); got %.2f", state.PortfolioRisk.PeakValue)
	}
	if len(state.PortfolioRisk.Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.PortfolioRisk.Events))
	}
	if state.PortfolioRisk.Events[0].PortfolioValue != 5000 {
		t.Errorf("expected audit event portfolio_value=5000 (total); got %.2f",
			state.PortfolioRisk.Events[0].PortfolioValue)
	}
}

// TestClearLatchedKillSwitchSharedWallet_MultiPlatformAnyFailPreservesLatch
// verifies that if ANY shared-wallet platform fails to fetch, the kill
// switch is preserved. We require the full portfolio-wide truth before
// re-baselining peak — a partial slice would under-baseline and still be
// unsafe.
func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAnyFailPreservesLatch(t *testing.T) {
	state := latchedSharedWalletState()
	originalLatchedAt := state.PortfolioRisk.KillSwitchAt
	originalPeak := state.PortfolioRisk.PeakValue
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "okx-a", Platform: "okx", CapitalPct: 0.3, Capital: 300},
		{ID: "okx-b", Platform: "okx", CapitalPct: 0.7, Capital: 700},
	}

	// hyperliquid fails; okx would succeed — but we should NOT partially
	// clear because the re-baselined peak would miss hyperliquid capital.
	fetcher := func(platform string) (float64, error) {
		if platform == "hyperliquid" {
			return 0, fmt.Errorf("hyperliquid unreachable")
		}
		return 2000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); cleared {
		t.Fatal("expected kill switch to remain latched when any platform fails")
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true")
	}
	if !state.PortfolioRisk.KillSwitchAt.Equal(originalLatchedAt) {
		t.Error("expected KillSwitchAt unchanged")
	}
	if state.PortfolioRisk.PeakValue != originalPeak {
		t.Errorf("expected PeakValue unchanged; got %.2f", state.PortfolioRisk.PeakValue)
	}
	if len(state.PortfolioRisk.Events) != 0 {
		t.Errorf("expected no audit event on partial failure; got %d", len(state.PortfolioRisk.Events))
	}
}

// TestPerpsMarginDrawdownInputs_OnlyPerpsCount verifies that spot and futures
// positions are excluded from margin deployed — only positions with
// Multiplier > 0 contribute when configLeverage > 0. Prevents the #292
// denominator from picking up unleveraged spot/options exposure mixed into a
// perps strategy state.
func TestPerpsMarginDrawdownInputs_OnlyPerpsCount(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			// Perp: notional 0.2 * $3000 = $600, margin @ configLev=20 = $30
			// PnL: 0.2 * 1 * (3000 - 2000) = $200 gain → clamps to 0 loss
			"ETH": {Symbol: "ETH", Quantity: 0.2, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 20},
			// Spot — Multiplier=0, must be ignored
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.05, AvgCost: 50000, Side: "long"},
		},
	}
	prices := map[string]float64{"ETH": 3000, "BTC/USDT": 60000, "ES": 4500}

	loss, margin := perpsMarginDrawdownInputs(s, 20, prices)
	if margin < 29.999 || margin > 30.001 {
		t.Errorf("margin = %.4f; want 30.0 (only perps count)", margin)
	}
	if loss != 0 {
		t.Errorf("loss = %.4f; want 0 (ETH has unrealized gain, not loss)", loss)
	}
}

// TestPerpsMarginDrawdownInputs_UnrealizedLoss verifies the unrealized-loss
// numerator tracks negative PnL on open perps positions — the key change in
// the #292 review: numerator is tied to currently-open positions, not to
// cumulative loss from peak.
func TestPerpsMarginDrawdownInputs_UnrealizedLoss(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			// Long ETH down 10%: PnL = 1 * 1 * (2700 - 3000) = -$300
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 10},
			// Short BTC down 5% (gain for short): PnL = 0.1 * 1 * (50000 - 47500) = +$250 → clamp
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, Leverage: 10},
		},
	}
	prices := map[string]float64{"ETH": 2700, "BTC": 47500}
	loss, margin := perpsMarginDrawdownInputs(s, 10, prices)
	// margin = (1 * 2700 / 10) + (0.1 * 47500 / 10) = 270 + 475 = 745
	if margin < 744.999 || margin > 745.001 {
		t.Errorf("margin = %.4f; want 745", margin)
	}
	if loss < 299.999 || loss > 300.001 {
		t.Errorf("loss = %.4f; want 300 (only ETH is underwater)", loss)
	}
}

// TestPerpsMarginDrawdownInputs_FallbackToAvgCost verifies that margin uses
// AvgCost when no mark price is available — matches the valuation fallback
// in PortfolioValue so margin and PnL use a consistent basis.
func TestPerpsMarginDrawdownInputs_FallbackToAvgCost(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			"HYPE": {Symbol: "HYPE", Quantity: 100, AvgCost: 20, Side: "long", Multiplier: 1, Leverage: 10},
		},
	}
	// Prices map is empty — should fall back to AvgCost ($20).
	// PnL at entry == mark → 0 loss.
	_, margin := perpsMarginDrawdownInputs(s, 10, map[string]float64{})
	want := 100.0 * 20.0 / 10.0 // $200
	if margin < want-0.001 || margin > want+0.001 {
		t.Errorf("margin with missing price = %.4f; want %.4f", margin, want)
	}

	// Zero/negative mark price must also fall back to AvgCost.
	_, margin = perpsMarginDrawdownInputs(s, 10, map[string]float64{"HYPE": 0})
	if margin < want-0.001 || margin > want+0.001 {
		t.Errorf("margin with zero price = %.4f; want %.4f", margin, want)
	}
}

// TestPerpsMarginDrawdownInputs_NoPositions verifies zero return when strategy
// has no positions — the caller uses this signal to fall back to peak-relative
// drawdown.
func TestPerpsMarginDrawdownInputs_NoPositions(t *testing.T) {
	s := &StrategyState{Positions: map[string]*Position{}}
	loss, margin := perpsMarginDrawdownInputs(s, 10, nil)
	if loss != 0 || margin != 0 {
		t.Errorf("perpsMarginDrawdownInputs with no positions = (%.4f, %.4f); want (0, 0)", loss, margin)
	}
}

// #418: config leverage (sc.Leverage) is the source of truth for the
// margin-drawdown denominator, NOT pos.Leverage. This regression test fails
// before the fix: pos.Leverage = 20 (on-chain margin tier overwrite from
// reconcileHyperliquidPositions) would inflate the drawdown ratio 10x against
// a config Leverage of 2.
func TestPerpsMarginDrawdownInputs_UsesConfigLeverageNotPosLeverage(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			// pos.Leverage = 20 simulates the corrupted state that
			// reconcileHyperliquidPositions writes when on-chain margin tier
			// (HL exchange max leverage) differs from trader's intent.
			"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
		},
	}
	prices := map[string]float64{"ETH": 2900}

	// configLeverage = 2 — what the trader actually configured. Margin
	// denominator MUST use this, not the corrupted pos.Leverage.
	loss, margin := perpsMarginDrawdownInputs(s, 2, prices)

	// notional = 1 * 2900 = 2900; margin @ configLev=2 = 1450 (not 145 @ 20x)
	wantMargin := 1450.0
	if math.Abs(margin-wantMargin) > 1e-6 {
		t.Errorf("margin = %.4f; want %.4f (must use configLeverage=2, NOT pos.Leverage=20)", margin, wantMargin)
	}
	// PnL: 1 * (2900 - 3000) = -100 → loss = 100
	if math.Abs(loss-100) > 1e-6 {
		t.Errorf("loss = %.4f; want 100", loss)
	}
}

// #418: configLeverage <= 0 → (0, 0) so caller falls back to peak-relative.
func TestPerpsMarginDrawdownInputs_ZeroConfigLeverageReturnsZero(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
		},
	}
	loss, margin := perpsMarginDrawdownInputs(s, 0, map[string]float64{"ETH": 2900})
	if loss != 0 || margin != 0 {
		t.Errorf("zero configLeverage must return (0, 0); got (%.4f, %.4f)", loss, margin)
	}
}

// #418: AggregatePerpsMarginInputs portfolio-kill-switch variant must also
// source leverage from configs, not from pos.Leverage. Two strategies, one
// with corrupted pos.Leverage from on-chain overwrite — the aggregate must
// still compute against config values.
func TestAggregatePerpsMarginInputs_UsesConfigLeverage(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-eth": {
			Type: "perps",
			Positions: map[string]*Position{
				// pos.Leverage = 20 (corrupted by hl-sync overwrite).
				"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
			},
		},
	}
	configs := []StrategyConfig{
		{ID: "hl-eth", Leverage: 2}, // trader's intent
	}
	prices := map[string]float64{"ETH": 2900}
	loss, margin := AggregatePerpsMarginInputs(strategies, configs, prices)

	// Margin = notional / configLev = 2900 / 2 = 1450 (NOT 145 @ 20x).
	if math.Abs(margin-1450) > 1e-6 {
		t.Errorf("margin = %.4f; want 1450 (config leverage, not pos.Leverage)", margin)
	}
	if math.Abs(loss-100) > 1e-6 {
		t.Errorf("loss = %.4f; want 100", loss)
	}
}

// #418: a perps strategy whose config is missing from the configs slice (or
// has Leverage=0) must contribute 0 to the aggregate so the kill switch
// falls back to equity drawdown for it rather than dividing by a corrupted
// on-chain value.
func TestAggregatePerpsMarginInputs_MissingConfigSkipsStrategy(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-orphan": {
			Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
			},
		},
	}
	loss, margin := AggregatePerpsMarginInputs(strategies, nil, map[string]float64{"ETH": 2900})
	if loss != 0 || margin != 0 {
		t.Errorf("orphan strategy without config must contribute 0; got (%.4f, %.4f)", loss, margin)
	}
}

// TestCheckRisk_PerpsMarginDrawdown_FiresEarly is the core #292 regression.
// Reproduces the issue scenario: a 20x ETH long where margin is tiny
// relative to cash. An adverse ETH move that wipes a large fraction of
// margin fires the circuit breaker where the old portfolio-relative
// calculation would have shown only a few-percent drawdown and allowed the
// position to continue decaying toward liquidation.
func TestCheckRisk_PerpsMarginDrawdown_FiresEarly(t *testing.T) {
	// Strategy: $584 cash, 0.236 ETH long @ $2357 (20x cross).
	// After -2.1% ETH move to $2307.5:
	//   unrealized PnL = 0.236 * 1 * (2307.5 - 2357) = -$11.68
	//   margin at mark   = 0.236 * 2307.5 / 20 = $27.22
	//   margin-based drawdown = 11.68 / 27.22 * 100 ≈ 42.9%  ← fires @ 25%
	//   portfolio-based drawdown would be ≈ 2% and would NOT fire
	s := &StrategyState{
		ID:   "hl-test",
		Type: "perps",
		Cash: 584.0,
		RiskState: RiskState{
			PeakValue:      589.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			"ETH": {
				Symbol:     "ETH",
				Quantity:   0.236,
				AvgCost:    2357.0,
				Side:       "long",
				Multiplier: 1,
				Leverage:   20,
			},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	prices := map[string]float64{"ETH": 2307.5}
	pv := PortfolioValue(s, prices)

	// sc.Leverage is now load-bearing for the margin-drawdown calc (#418):
	// without a config leverage, perpsMarginDrawdownInputs returns (0, 0)
	// and the path falls back to peak-relative drawdown.
	sc := &StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Leverage: 20}
	allowed, reason := CheckRisk(sc, s, pv, prices, nil, nil)

	if allowed {
		t.Errorf("expected circuit breaker to fire on margin-based drawdown; reason=%s", reason)
	}
	if s.RiskState.CurrentDrawdownPct < 25.0 {
		t.Errorf("expected CurrentDrawdownPct > 25 on margin basis; got %.2f", s.RiskState.CurrentDrawdownPct)
	}
	if s.RiskState.CurrentDrawdownPct < 40 {
		t.Errorf("expected margin-based drawdown well above threshold; got %.2f", s.RiskState.CurrentDrawdownPct)
	}
	// Positions liquidated on circuit-breaker fire.
	if len(s.Positions) != 0 {
		t.Errorf("expected positions force-closed; got %d", len(s.Positions))
	}
}

// TestCheckRisk_LiveHLSharedCoin_SetsPendingPartialClose verifies #356: a live
// HL strategy that shares a coin with another configured live HL strategy gets
// a proportional pending on-chain close (capital_pct weights), not the full
// wallet szi.
func TestCheckRisk_LiveHLSharedCoin_SetsPendingPartialClose(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-tema", Platform: "hyperliquid", Type: "perps",
		CapitalPct: 0.5, Capital: 500, Leverage: 20,
		Args: []string{"triple_ema", "ETH", "1h", "--mode=live"},
	}
	hlLiveAll := []StrategyConfig{
		sc,
		{ID: "hl-rmc", Platform: "hyperliquid", Type: "perps",
			CapitalPct: 0.5, Capital: 500, Leverage: 20,
			Args: []string{"rsi_macd", "ETH", "1h", "--mode=live"}},
	}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}},
		HLLiveAll:   hlLiveAll,
	}

	s := &StrategyState{
		ID:       sc.ID,
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     584.0,
		RiskState: RiskState{
			PeakValue:      589.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.236, AvgCost: 2357.0, Side: "long", Multiplier: 1, Leverage: 20},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	prices := map[string]float64{"ETH": 2307.5}
	pv := PortfolioValue(s, prices)

	_, _ = CheckRisk(&sc, s, pv, prices, nil, assist)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("expected PendingCircuitCloses[hyperliquid] after CB fire")
	}
	if len(p.Symbols) != 1 {
		t.Fatalf("expected 1 pending symbol, got %d", len(p.Symbols))
	}
	c0 := p.Symbols[0]
	if c0.Symbol != "ETH" {
		t.Errorf("symbol=%q want ETH", c0.Symbol)
	}
	want := 0.517 * 0.5
	if math.Abs(c0.Size-want) > 1e-6 {
		t.Errorf("pending size=%.6f want %.6f (half of shared on-chain 0.517)", c0.Size, want)
	}
}

// TestCheckRisk_LiveTopStepCB_SetsPendingFullFlatten verifies #362: a live
// TopStep futures strategy with a sole-peer contract gets a full-flatten
// pending close enqueued when its per-strategy circuit breaker fires.
func TestCheckRisk_LiveTopStepCB_SetsPendingFullFlatten(t *testing.T) {
	sc := StrategyConfig{
		ID: "ts-es", Platform: "topstep", Type: "futures",
		Capital: 5000,
		Args:    []string{"sma", "ES", "15m", "--mode=live"},
	}
	tsLiveAll := []StrategyConfig{sc}
	assist := &PlatformRiskAssist{
		TSPositions: []TopStepPosition{{Coin: "ES", Size: 3, Side: "long"}},
		TSLiveAll:   tsLiveAll,
	}

	// Rig a max-drawdown breach so CheckRisk fires the CB.
	s := &StrategyState{
		ID:   sc.ID,
		Type: "futures",
		Cash: 3000.0,
		RiskState: RiskState{
			PeakValue:      5000.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			// Futures position with Multiplier > 0; no Leverage (TS isn't perps).
			"ES": {Symbol: "ES", Quantity: 3, AvgCost: 5000, Side: "long", Multiplier: 50},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"ES": 4995}
	pv := PortfolioValue(s, prices)

	allowed, _ := CheckRisk(&sc, s, pv, prices, nil, assist)
	if allowed {
		t.Fatal("expected CB fire (drawdown exceeds 25%)")
	}

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
	if p == nil {
		t.Fatal("expected PendingCircuitCloses[topstep] after CB fire")
	}
	if len(p.Symbols) != 1 {
		t.Fatalf("expected 1 pending symbol, got %d", len(p.Symbols))
	}
	c0 := p.Symbols[0]
	if c0.Symbol != "ES" {
		t.Errorf("symbol=%q want ES", c0.Symbol)
	}
	if c0.Size != 3 {
		t.Errorf("pending size=%.0f want 3 (full flatten for sole peer)", c0.Size)
	}
}

// Multi-peer: CheckRisk still fires CB and force-closes virtual state, but
// setTopStepCircuitBreakerPending does NOT enqueue because market_close has
// no partial-size variant — operator handles the shared contract manually.
func TestCheckRisk_LiveTopStepCB_MultiPeerNoPending(t *testing.T) {
	sc := StrategyConfig{
		ID: "ts-a", Platform: "topstep", Type: "futures",
		Capital: 5000,
		Args:    []string{"sma", "ES", "15m", "--mode=live"},
	}
	tsLiveAll := []StrategyConfig{
		sc,
		{ID: "ts-b", Platform: "topstep", Type: "futures",
			Capital: 5000,
			Args:    []string{"rsi", "ES", "15m", "--mode=live"}},
	}
	assist := &PlatformRiskAssist{
		TSPositions: []TopStepPosition{{Coin: "ES", Size: 5, Side: "long"}},
		TSLiveAll:   tsLiveAll,
	}

	s := &StrategyState{
		ID:   sc.ID,
		Type: "futures",
		Cash: 3000.0,
		RiskState: RiskState{
			PeakValue:      5000.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			"ES": {Symbol: "ES", Quantity: 2, AvgCost: 5000, Side: "long", Multiplier: 50},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"ES": 4995}
	pv := PortfolioValue(s, prices)

	allowed, _ := CheckRisk(&sc, s, pv, prices, nil, assist)
	if allowed {
		t.Fatal("expected CB fire")
	}

	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected no pending TS entry for multi-peer contract")
	}
}

// TestCheckRisk_PerpsMarginDrawdown_BelowThreshold verifies the perps
// strategy is allowed to continue when margin-based drawdown is under the
// circuit-breaker limit.
func TestCheckRisk_PerpsMarginDrawdown_BelowThreshold(t *testing.T) {
	// Same 0.236 ETH @ $2357 20x setup.
	// At price 2355: PnL = 0.236 * (2355 - 2357) = -$0.47;
	//                margin = 0.236 * 2355 / 20 = $27.78;
	//                drawdown ≈ 1.7% — well under 25%.
	s := &StrategyState{
		Type: "perps",
		Cash: 584.0,
		RiskState: RiskState{
			PeakValue:      589.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.236, AvgCost: 2357.0, Side: "long", Multiplier: 1, Leverage: 20},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"ETH": 2355.0}
	pv := PortfolioValue(s, prices)
	sc := &StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Leverage: 20}
	allowed, reason := CheckRisk(sc, s, pv, prices, nil, nil)
	if !allowed {
		t.Errorf("expected allowed below margin drawdown threshold; reason=%s dd=%.2f",
			reason, s.RiskState.CurrentDrawdownPct)
	}
	if s.RiskState.CurrentDrawdownPct >= 25 {
		t.Errorf("expected drawdown < 25%%; got %.2f", s.RiskState.CurrentDrawdownPct)
	}
}

// TestCheckRisk_PerpsPriorRealizedLossesDoNotInflateDrawdown is the review
// regression for the "stale peak meets fresh margin" concern. A strategy
// that took realized losses in the past ($1000 peak → $900 cash) then opens
// a fresh untouched small position must NOT fire the circuit breaker on the
// very first tick: cumulative peak-relative loss ($100) against tiny
// new-position margin ($0.15) would otherwise blow past any threshold even
// though the open position itself is flat. (#292 code review)
func TestCheckRisk_PerpsPriorRealizedLossesDoNotInflateDrawdown(t *testing.T) {
	s := &StrategyState{
		Type: "perps",
		Cash: 900.0, // prior realized losses brought cash from $1000 → $900
		RiskState: RiskState{
			PeakValue:      1000.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    5,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			// Fresh tiny position, mark == entry → 0 unrealized PnL
			"ETH": {Symbol: "ETH", Quantity: 0.001, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"ETH": 3000}
	pv := PortfolioValue(s, prices)
	sc := &StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Leverage: 20}
	allowed, reason := CheckRisk(sc, s, pv, prices, nil, nil)
	if !allowed {
		t.Errorf("expected fresh position with no unrealized PnL to NOT fire; reason=%s dd=%.2f",
			reason, s.RiskState.CurrentDrawdownPct)
	}
	if s.RiskState.CurrentDrawdownPct > 0.001 {
		t.Errorf("expected drawdown ≈ 0 (no unrealized loss on open position); got %.2f",
			s.RiskState.CurrentDrawdownPct)
	}
	if len(s.Positions) != 1 {
		t.Errorf("expected position to survive; got %d", len(s.Positions))
	}
}

// TestCheckRisk_PerpsNoOpenPositions_FallsBackToPeak verifies that a perps
// strategy with no open positions (e.g. after all were closed) uses the
// peak-relative drawdown formula — otherwise the denominator would be zero
// and drawdown semantics would be undefined.
func TestCheckRisk_PerpsNoOpenPositions_FallsBackToPeak(t *testing.T) {
	s := &StrategyState{
		Type: "perps",
		Cash: 700.0, // realized losses brought cash down from $1000
		RiskState: RiskState{
			PeakValue:      1000.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    3,
			DailyPnLDate:   todayUTC(),
		},
		Positions:       map[string]*Position{},
		OptionPositions: make(map[string]*OptionPosition),
	}
	// Portfolio = cash only = $700. Peak-relative drawdown = 30% → fires.
	pv := PortfolioValue(s, nil)
	allowed, _ := CheckRisk(nil, s, pv, nil, nil, nil)
	if allowed {
		t.Error("expected peak-relative drawdown to fire when no perps margin deployed")
	}
	if s.RiskState.CurrentDrawdownPct < 29 || s.RiskState.CurrentDrawdownPct > 31 {
		t.Errorf("expected peak-relative drawdown ≈ 30%%; got %.2f", s.RiskState.CurrentDrawdownPct)
	}
}

// TestCheckRisk_SpotUnchanged verifies that spot strategies continue to use
// peak-relative drawdown regardless of position state — the #292 change is
// scoped to perps.
func TestCheckRisk_SpotUnchanged(t *testing.T) {
	s := &StrategyState{
		Type: "spot",
		Cash: 500.0,
		RiskState: RiskState{
			PeakValue:      1000.0,
			MaxDrawdownPct: 25.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	// BTC dropped from $50k to $30k: position value 0.01*30000 = $300.
	// Portfolio = 500 + 300 = $800. Peak drawdown = 20% < 25% → allowed.
	prices := map[string]float64{"BTC/USDT": 30000}
	pv := PortfolioValue(s, prices)
	allowed, _ := CheckRisk(nil, s, pv, prices, nil, nil)
	if !allowed {
		t.Errorf("expected spot strategy to stay within 25%% peak drawdown; dd=%.2f",
			s.RiskState.CurrentDrawdownPct)
	}
	if s.RiskState.CurrentDrawdownPct < 19.5 || s.RiskState.CurrentDrawdownPct > 20.5 {
		t.Errorf("expected spot drawdown ≈ 20%% (peak-relative); got %.2f",
			s.RiskState.CurrentDrawdownPct)
	}
}

// TestDetectSharedWalletPlatforms verifies the shared-wallet detector picks
// out platforms with > 1 capital_pct strategy and ignores everything else.
func TestDetectSharedWalletPlatforms(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5},
		{ID: "okx-solo", Platform: "okx", CapitalPct: 0.5},   // only one — not shared
		{ID: "spot-a", Platform: "binanceus", Capital: 1000}, // no capital_pct
		{ID: "spot-b", Platform: "binanceus", Capital: 1000},
	}

	got := detectSharedWalletPlatforms(strategies)
	if len(got) != 1 || got[0] != "hyperliquid" {
		t.Errorf("expected [hyperliquid]; got %v", got)
	}
}

// --- #296: portfolio-level perps margin drawdown ---

// TestCheckPortfolioRisk_AllPerps_MarginDrawdownFires is the core acceptance-
// criteria test for issue #296. An all-perps portfolio where deployed margin
// has lost 50% of its value must fire the kill switch at the configured
// drawdown limit, even though the equity-based drawdown looks small because
// leveraged PnL is only a small fraction of total account value.
//
// Scenario: $10K equity, $1K of margin deployed on a 10x leveraged position
// (notional ~$10K). A 5% adverse price move = $500 unrealized loss = 50% of
// deployed margin, but only 5% of total equity. Pre-#296 the portfolio kill
// switch would not fire until equity drawdown breached 25%, long after the
// position would have been liquidated. Post-#296 the margin signal trips at
// 25%.
func TestCheckPortfolioRisk_AllPerps_MarginDrawdownFires(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity has barely moved — 5% nominal drawdown — so the pre-#296
	// equity-only check would allow. Margin drawdown is 50%, well above
	// the 25% limit, so the kill switch must fire.
	totalValue := 9500.0
	perpsLoss := 500.0
	perpsMargin := 1000.0

	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, totalValue, 0, perpsLoss, perpsMargin)
	if allowed {
		t.Errorf("expected kill switch to fire on 50%% perps margin drawdown; got allowed=true, reason=%s", reason)
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true")
	}
	if reason == "" {
		t.Error("expected non-empty reason for kill switch")
	}
	// Reason should name the margin signal, not equity.
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to reference perps margin drawdown; got %q", reason)
	}
	// Equity drawdown was only 5% — field stays on the equity signal.
	if prs.CurrentDrawdownPct < 4.9 || prs.CurrentDrawdownPct > 5.1 {
		t.Errorf("expected CurrentDrawdownPct (equity)≈5%%; got %.2f", prs.CurrentDrawdownPct)
	}
	// Margin drawdown is 50% — recorded on the dedicated field so persistence
	// stays arithmetically consistent (peak_value / current_drawdown_pct).
	if prs.CurrentMarginDrawdownPct < 49.9 || prs.CurrentMarginDrawdownPct > 50.1 {
		t.Errorf("expected CurrentMarginDrawdownPct≈50%%; got %.2f", prs.CurrentMarginDrawdownPct)
	}
	// Event must be recorded with source="margin" so auditors can tell which
	// signal drove the fire without re-parsing the reason string.
	if len(prs.Events) != 1 {
		t.Fatalf("expected exactly one event; got %d", len(prs.Events))
	}
	evt := prs.Events[0]
	if evt.Type != "triggered" {
		t.Errorf("expected event Type=triggered; got %q", evt.Type)
	}
	if evt.Source != "margin" {
		t.Errorf("expected event Source=margin; got %q", evt.Source)
	}
	// Event's DrawdownPct records the signal value (margin=50%), not a
	// mixed "worse of" number.
	if evt.DrawdownPct < 49.9 || evt.DrawdownPct > 50.1 {
		t.Errorf("expected event DrawdownPct≈50%% (margin signal); got %.2f", evt.DrawdownPct)
	}
}

// TestCheckPortfolioRisk_MixedAccount_SpotEquityStillHonored verifies that a
// mixed spot+perps portfolio does not regress on the equity signal when perps
// margin is healthy. Acceptance criterion 2: "Mixed spot+perps portfolios
// don't regress (spot equity drawdown still honored)."
func TestCheckPortfolioRisk_MixedAccount_SpotEquityStillHonored(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity drawdown 30% (spot leg tanked). Perps has margin deployed but
	// no unrealized loss — margin signal does not fire.
	totalValue := 7000.0
	perpsLoss := 0.0
	perpsMargin := 500.0

	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, totalValue, 0, perpsLoss, perpsMargin)
	if allowed {
		t.Errorf("expected equity-drawdown kill switch to fire at 30%%; got allowed=true")
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true")
	}
	// Reason should NOT reference margin — this was an equity event.
	if strings.Contains(reason, "margin") {
		t.Errorf("expected reason to reference equity drawdown, not margin; got %q", reason)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "equity" {
		t.Errorf("expected one triggered event with Source=equity; got %+v", prs.Events)
	}
}

// TestCheckPortfolioRisk_MixedAccount_MarginFiresFirst verifies that when both
// equity and margin drawdowns breach the limit simultaneously, the reason
// names the larger (margin) signal — so operators see the headline number.
func TestCheckPortfolioRisk_MixedAccount_MarginFiresFirst(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity: 30% drawdown (> 25%). Margin: 60% drawdown (way bigger).
	totalValue := 7000.0
	perpsLoss := 600.0
	perpsMargin := 1000.0

	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, totalValue, 0, perpsLoss, perpsMargin)
	if allowed {
		t.Error("expected kill switch to fire")
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to reference margin (worse signal); got %q", reason)
	}
	// Equity and margin are persisted separately: equity=30%, margin=60%.
	if prs.CurrentDrawdownPct < 29.9 || prs.CurrentDrawdownPct > 30.1 {
		t.Errorf("expected CurrentDrawdownPct (equity)≈30%%; got %.2f", prs.CurrentDrawdownPct)
	}
	if prs.CurrentMarginDrawdownPct < 59.9 || prs.CurrentMarginDrawdownPct > 60.1 {
		t.Errorf("expected CurrentMarginDrawdownPct≈60%%; got %.2f", prs.CurrentMarginDrawdownPct)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "margin" {
		t.Errorf("expected one triggered event with Source=margin; got %+v", prs.Events)
	}
}

// TestCheckPortfolioRisk_NoPerps_EquityBehaviorUnchanged verifies that
// passing zero perps inputs reproduces the pre-#296 equity-only behavior
// exactly. Guard against regressions for all-spot/all-options portfolios.
func TestCheckPortfolioRisk_NoPerps_EquityBehaviorUnchanged(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// 20% equity drawdown, no perps — below the 25% limit, should allow.
	allowed, _, _, _ := CheckPortfolioRisk(prs, cfg, 8000, 0, 0, 0)
	if !allowed {
		t.Error("expected allowed=true at 20%% equity drawdown with no perps")
	}

	// 26% equity drawdown — fires.
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 7400, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch at 26%% equity drawdown")
	}
	if strings.Contains(reason, "margin") {
		t.Errorf("expected equity-drawdown reason (no perps deployed); got %q", reason)
	}
}

// TestCheckPortfolioRisk_MarginWarning verifies the warning signal also
// respects the perps margin drawdown, not just equity — so a leveraged
// position approaching the kill switch threshold alerts operators early.
func TestCheckPortfolioRisk_MarginWarning(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity flat. Margin drawdown 21% — between warn threshold (20%) and
	// kill switch (25%). Warning must fire.
	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 10000, 0, 210, 1000)
	if !warning {
		t.Errorf("expected warning=true at 21%% margin drawdown; reason=%q", reason)
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected warning reason to reference margin; got %q", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected kill switch NOT active (warning, not fire)")
	}

	_, _, warning, reason = CheckPortfolioRisk(prs, cfg, 10000, 0, 210, 1000)
	if !warning {
		t.Errorf("expected repeated warning=true while margin drawdown remains above threshold; reason=%q", reason)
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected repeated warning reason to reference margin; got %q", reason)
	}
}

// TestAggregatePerpsMarginInputs verifies the helper sums across multiple
// perps strategies and ignores non-perps (spot/options/futures). This is the
// inputs side of the #296 portfolio kill switch — a regression here would
// silently under-count deployed margin and hide leveraged losses.
func TestAggregatePerpsMarginInputs(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-btc": {
			Type: "perps",
			Positions: map[string]*Position{
				// 1 BTC short @ 40K, now 42K, 10x leverage, multiplier 1.
				// notional = 1 * 42000 = 42000, margin = 42000/10 = 4200.
				// pnl = 1 * 1 * (40000 - 42000) = -2000 (short loses when price rises).
				"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 40000, Side: "short", Multiplier: 1, Leverage: 10},
			},
		},
		"hl-eth": {
			Type: "perps",
			Positions: map[string]*Position{
				// 10 ETH long @ 3000, now 3100, 5x leverage.
				// notional = 10 * 3100 = 31000, margin = 31000/5 = 6200.
				// pnl = 10 * 1 * (3100 - 3000) = +1000 (winner, clamps to 0 loss).
				"ETH": {Symbol: "ETH", Quantity: 10, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 5},
			},
		},
		"spot-sol": {
			Type: "spot",
			Positions: map[string]*Position{
				// Spot position must be ignored — no leverage, no margin.
				"SOL/USDT": {Symbol: "SOL/USDT", Quantity: 100, AvgCost: 150, Side: "long"},
			},
		},
		"ts-es": {
			Type: "futures",
			Positions: map[string]*Position{
				// Futures position must be ignored — Type != perps.
				"ES": {Symbol: "ES", Quantity: 1, AvgCost: 5000, Side: "long", Multiplier: 50},
			},
		},
	}
	prices := map[string]float64{
		"BTC":      42000,
		"ETH":      3100,
		"SOL/USDT": 200,
		"ES":       5100,
	}
	configs := []StrategyConfig{
		{ID: "hl-btc", Leverage: 10},
		{ID: "hl-eth", Leverage: 5},
	}

	loss, margin := AggregatePerpsMarginInputs(strategies, configs, prices)

	// Only the losing BTC short contributes to loss: 2000.
	// Margin includes both perps positions: 4200 + 6200 = 10400.
	expectedLoss := 2000.0
	expectedMargin := 10400.0
	if loss < expectedLoss-0.01 || loss > expectedLoss+0.01 {
		t.Errorf("expected loss=%.2f; got %.2f", expectedLoss, loss)
	}
	if margin < expectedMargin-0.01 || margin > expectedMargin+0.01 {
		t.Errorf("expected margin=%.2f; got %.2f", expectedMargin, margin)
	}
}

// TestAggregatePerpsMarginInputs_NoPerpsReturnsZero verifies the helper
// returns (0, 0) when no perps strategies exist. The caller treats zero
// margin as the signal to fall back to pure equity drawdown.
func TestAggregatePerpsMarginInputs_NoPerpsReturnsZero(t *testing.T) {
	strategies := map[string]*StrategyState{
		"spot-btc": {
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.5, AvgCost: 40000, Side: "long"},
			},
		},
	}
	loss, margin := AggregatePerpsMarginInputs(strategies, nil, map[string]float64{"BTC/USDT": 50000})
	if loss != 0 || margin != 0 {
		t.Errorf("expected (0, 0) for no perps; got (%.2f, %.2f)", loss, margin)
	}
}

// TestCheckPortfolioRisk_PeakZero_MarginCanStillFire guards against the
// subtle gating change introduced in #296: a cold-start account (no prior
// valuation, PeakValue==0) that opens a leveraged perps position and
// immediately blows up its margin must still kill-switch. Pre-#296 the
// entire kill-switch branch sat inside `if prs.PeakValue > 0`, so a fresh
// account firing on bar 1 was impossible; the margin signal has to work
// independent of the equity high-water mark.
func TestCheckPortfolioRisk_PeakZero_MarginCanStillFire(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 0} // cold start: no prior valuation

	// Cold account opens a 10x perps position, immediately down 50% on
	// margin. totalValue is zero (we have no valuation yet) so equityDD is
	// zero; margin signal is 50%, well above the 25% limit.
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 0, 0, 500, 1000)
	if allowed {
		t.Errorf("expected cold-start margin drawdown to fire kill switch; got allowed=true, reason=%s", reason)
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true on cold-start margin blowup")
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected margin-driven reason; got %q", reason)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "margin" {
		t.Errorf("expected one triggered event with Source=margin; got %+v", prs.Events)
	}
}

// TestCheckPortfolioRisk_BothSignalsBreachWarn_ReasonIncludesBoth verifies
// that when both equity and margin cross the warning threshold in the same
// cycle, the reason string surfaces both — so a correlated move is visible
// to the operator at a glance rather than hidden behind the larger signal.
func TestCheckPortfolioRisk_BothSignalsBreachWarn_ReasonIncludesBoth(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity drawdown 22%, margin drawdown 23% — both above the 20% warn
	// threshold, both below the 25% kill switch.
	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7800, 0, 230, 1000)
	if !warning {
		t.Fatalf("expected warning=true; reason=%q", reason)
	}
	if !strings.Contains(reason, "equity=") || !strings.Contains(reason, "margin=") {
		t.Errorf("expected reason to mention both equity= and margin=; got %q", reason)
	}
}

// TestCheckPortfolioRisk_MarginWarning_FieldsPopulated makes sure the
// dedicated CurrentMarginDrawdownPct field is kept current even when the
// warning does not fire (so /status surfaces the live margin signal). This
// mirrors CurrentDrawdownPct's always-updated contract.
func TestCheckPortfolioRisk_MarginWarning_FieldsPopulated(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	// Equity flat. Margin drawdown 10% — below warn. Field still updates.
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 10000, 0, 100, 1000)
	if warning {
		t.Error("expected warning=false at 10%% margin drawdown")
	}
	if prs.CurrentMarginDrawdownPct < 9.9 || prs.CurrentMarginDrawdownPct > 10.1 {
		t.Errorf("expected CurrentMarginDrawdownPct≈10%%; got %.2f", prs.CurrentMarginDrawdownPct)
	}
}

// --- #359 phase 1b: generic PendingCircuitCloses plumbing ---

// TestRiskState_PendingCircuitClose_Marshal_EmptyReturnsBlank verifies that an
// empty or nil pending map serializes to "" so an empty blob never overwrites
// a non-empty column on save.
func TestRiskState_PendingCircuitClose_Marshal_EmptyReturnsBlank(t *testing.T) {
	cases := []struct {
		name string
		r    *RiskState
	}{
		{"nil receiver", nil},
		{"nil map", &RiskState{}},
		{"empty map", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{}}},
		{"entry with nil value", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{"hyperliquid": nil}}},
		{"entry with empty symbols", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
			"hyperliquid": {Symbols: nil},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.r.MarshalPendingCircuitClosesJSON()
			if got != "" {
				t.Errorf("expected empty marshal for %s; got %q", tc.name, got)
			}
		})
	}
}

// TestRiskState_PendingCircuitClose_MarshalUnmarshalRoundTrip locks the
// round-trip contract for the new map-keyed JSON shape.
func TestRiskState_PendingCircuitClose_MarshalUnmarshalRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{
			{Symbol: "ETH", Size: 0.2585},
			{Symbol: "BTC", Size: 0.01},
		}},
	}}
	blob := src.MarshalPendingCircuitClosesJSON()
	if blob == "" {
		t.Fatal("non-empty marshal expected")
	}

	var dst RiskState
	dst.UnmarshalPendingCircuitClosesJSON(blob)

	got := dst.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if got == nil || len(got.Symbols) != 2 {
		t.Fatalf("round-trip missing entries: %+v", got)
	}
	byName := map[string]float64{}
	for _, s := range got.Symbols {
		byName[s.Symbol] = s.Size
	}
	if byName["ETH"] != 0.2585 || byName["BTC"] != 0.01 {
		t.Errorf("round-trip sizes wrong: %+v", byName)
	}
}

// TestRiskState_PendingCircuitClose_UnmarshalLegacyHL verifies the backwards-
// compat path: a pre-#359 {"coins":[{"coin":..., "sz":...}]} payload must
// transparently convert into the new map keyed by "hyperliquid". This is the
// self-healing path for pre-#359 DB rows on first load after upgrade.
func TestRiskState_PendingCircuitClose_UnmarshalLegacyHL(t *testing.T) {
	var r RiskState
	r.UnmarshalPendingCircuitClosesJSON(`{"coins":[{"coin":"ETH","sz":0.2585}]}`)

	p := r.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("legacy JSON did not convert: %+v", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.2585 {
		t.Errorf("legacy conversion wrong: got symbol=%q size=%g", p.Symbols[0].Symbol, p.Symbols[0].Size)
	}
}

// TestRiskState_PendingCircuitClose_UnmarshalEmptyClears verifies that an
// empty string wipes the pending map (matches the prior HL-specific behavior).
func TestRiskState_PendingCircuitClose_UnmarshalEmptyClears(t *testing.T) {
	r := RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}},
	}}
	r.UnmarshalPendingCircuitClosesJSON("")
	if r.PendingCircuitCloses != nil {
		t.Errorf("expected nil map after empty unmarshal; got %+v", r.PendingCircuitCloses)
	}
}

// TestRiskState_PendingCircuitClose_UnmarshalMalformedClears verifies that
// a malformed JSON payload wipes the pending map rather than leaving stale
// data in place.
func TestRiskState_PendingCircuitClose_UnmarshalMalformedClears(t *testing.T) {
	r := RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}},
	}}
	r.UnmarshalPendingCircuitClosesJSON(`not-json{`)
	if r.PendingCircuitCloses != nil {
		t.Errorf("expected nil map after malformed unmarshal; got %+v", r.PendingCircuitCloses)
	}
}

// TestRiskState_PendingCircuitClose_SetClearGet verifies the setter/clearer/
// getter contract: nil map is materialized lazily on set; clear deletes the
// entry and nils the map when empty.
func TestRiskState_PendingCircuitClose_SetClearGet(t *testing.T) {
	var r RiskState

	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Fatal("expected nil for unset key")
	}

	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
	})
	if got := r.getPendingCircuitClose("hyperliquid"); got == nil || got.Symbols[0].Size != 0.5 {
		t.Errorf("setter did not store value: %+v", got)
	}

	// Set with empty symbols should clear the entry.
	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{Symbols: nil})
	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Error("empty-symbols set should have cleared entry")
	}
	if r.PendingCircuitCloses != nil {
		t.Error("map should be nil after last entry cleared")
	}

	// Clear on missing key is a no-op.
	r.clearPendingCircuitClose("hyperliquid")
}

// TestRiskState_PendingCircuitClose_MultiPlatformRoundTrip locks in that the
// generic plumbing is not HL-limited: future phases 2-4 will co-exist in the
// same map.
func TestRiskState_PendingCircuitClose_MultiPlatformRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		"hyperliquid": {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}}},
		"okx":         {Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT-SWAP", Size: 0.01}}},
	}}
	blob := src.MarshalPendingCircuitClosesJSON()
	var dst RiskState
	dst.UnmarshalPendingCircuitClosesJSON(blob)
	if dst.getPendingCircuitClose("hyperliquid") == nil {
		t.Error("hyperliquid entry lost in round-trip")
	}
	if dst.getPendingCircuitClose("okx") == nil {
		t.Error("okx entry lost in round-trip")
	}
}
