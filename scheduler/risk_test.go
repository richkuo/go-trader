package main

import (
	"fmt"
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

	CheckRisk(s, 1000.0, nil, nil)

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

	allowed, reason := CheckRisk(s, pv, prices, nil)

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
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 7600.0, 0)
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
	allowed, nb, _, reason = CheckPortfolioRisk(prs, cfg, 7400.0, 0)
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
	allowed, _, _, _ = CheckPortfolioRisk(prs, cfg, 10000.0, 0)
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
	allowed, nb, _, _ := CheckPortfolioRisk(prs, cfg, 10000.0, 30000.0)
	if !allowed {
		t.Error("expected allowed under notional cap")
	}
	if nb {
		t.Error("expected notionalBlocked=false under cap")
	}

	// Over cap — allowed=true, notionalBlocked=true, kill switch NOT active.
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0)
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
	CheckPortfolioRisk(prs, cfg, 8000.0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 after rise; got %.2f", prs.PeakValue)
	}

	// Value drops — peak should NOT update.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 unchanged after drop; got %.2f", prs.PeakValue)
	}

	// Value rises again — peak updates.
	CheckPortfolioRisk(prs, cfg, 9000.0, 0)
	if prs.PeakValue != 9000.0 {
		t.Errorf("expected peak=9000 after new high; got %.2f", prs.PeakValue)
	}

	// Drawdown tracked correctly: (9000-6000)/9000 ≈ 33.3%.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0)
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
		"SOL":  0,   // zero — must be skipped
		"DOGE": -1,  // negative — must be skipped
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

	allowed, reason := CheckRisk(s, pv, prices, nil)

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
// triggers a warning once but not again on second call.
func TestCheckPortfolioRisk_WarningFires(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Warn threshold = 25 * 80/100 = 20%. Drawdown = (10000-7900)/10000 = 21% > 20%.
	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if !warning {
		t.Error("expected warning=true at 21% drawdown (warn threshold=20%)")
	}
	if reason == "" {
		t.Error("expected non-empty reason for warning")
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent=true after warning fires")
	}

	// Second call at same drawdown — warning should NOT fire again.
	_, _, warning, _ = CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if warning {
		t.Error("expected warning=false on second call (already sent)")
	}
}

// TestCheckPortfolioRisk_WarningResetOnRecovery verifies that recovery below
// the warning threshold resets WarningSent so it can fire again.
func TestCheckPortfolioRisk_WarningResetOnRecovery(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Trigger warning at 21% drawdown.
	CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if !prs.WarningSent {
		t.Fatal("expected WarningSent=true after first warning")
	}

	// Recover to 15% drawdown (below 20% warn threshold).
	CheckPortfolioRisk(prs, cfg, 8500.0, 0)
	if prs.WarningSent {
		t.Error("expected WarningSent=false after recovery below warn threshold")
	}

	// Cross warning threshold again — should warn again.
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0)
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
	allowed, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7400.0, 0)
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
		addKillSwitchEvent(prs, "warning", float64(i), 1000, 2000, "test")
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

	CheckPortfolioRisk(prs, cfg, 7400.0, 0)

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
	allowed, _, _, reason := CheckPortfolioRisk(&state.PortfolioRisk, cfg, 5000, 0)
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
