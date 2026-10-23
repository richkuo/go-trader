//go:build pyintegration

package main

func okxGoldenDispatch(h *okxGoldenHarness, prices map[string]float64, in okxGoldenInputs, persistenceHold bool) (int, string) {
	sc := h.sc
	stratState := h.env.state.Strategies[sc.ID]
	stratDB := h.env.fileFor(sc.ID)
	scope := partitionFor(sc).Scope
	if sc.Type == "spot" {
		return okxSpotDispatch(sc, nil, prices, in.PosCtx, h.env.cfg, h.notifier, h.logger, in.PosQty, in.PosSide, stratState, &h.env.mu, h.sr, persistenceHold, scope, h.env.store, in.AvgCost, in.Cash, in.PoolBalanceKnown, in.CashReconcile, in.Leverage, stratDB, h.env.state)
	}
	return okxPerpsDispatch(sc, nil, prices, in.PosCtx, h.env.cfg, h.notifier, h.logger, in.PosQty, in.PosSide, stratState, &h.env.mu, h.sr, persistenceHold, scope, h.env.store, in.AvgCost, in.Cash, in.PoolBalanceKnown, in.CashReconcile, in.Leverage, stratDB, h.env.state)
}

var okxGoldenBoth = []string{"spot", "perps"}

func okxGoldenBuy(px float64) okxGoldenCheck  { return okxGoldenCheck{Signal: 1, Price: px} }
func okxGoldenSell(px float64) okxGoldenCheck { return okxGoldenCheck{Signal: -1, Price: px} }

func okxGoldenPx(px float64) map[string]float64 { return map[string]float64{"BTC": px} }

func okxGoldenScenarios() []okxGoldenScenario {
	closedBar := func(sc *StrategyConfig) { sc.ClosedBarDecisions = true }
	return []okxGoldenScenario{
		{Name: "paper_open_close", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "buy opens", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
				{Name: "hold", Check: okxGoldenCheck{Price: 104}, Prices: okxGoldenPx(104)},
				{Name: "sell closes", Check: okxGoldenSell(110), Prices: okxGoldenPx(110)},
			}},
		{Name: "paper_partial_close", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "buy opens", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
				{Name: "half close", Check: okxGoldenCheck{Signal: -1, Price: 105, CloseFraction: 0.5}, Prices: okxGoldenPx(105)},
				{Name: "buy adds to the open long", Check: okxGoldenBuy(106), Prices: okxGoldenPx(106)},
			}},
		{Name: "paper_direction_both", Types: okxGoldenBoth, Mode: "paper", Config: func(sc *StrategyConfig) { sc.Direction = DirectionBoth },
			Steps: []okxGoldenStep{
				{Name: "sell from flat", Check: okxGoldenSell(100), Prices: okxGoldenPx(100)},
				{Name: "buy against the book", Check: okxGoldenBuy(95), Prices: okxGoldenPx(95)},
				{Name: "sell again", Check: okxGoldenSell(97), Prices: okxGoldenPx(97)},
			}},
		{Name: "gate_regime", Types: okxGoldenBoth, Mode: "paper",
			Config: func(sc *StrategyConfig) {
				sc.AllowedRegimes = []string{"trending_up"}
				sc.RegimeGateOnFailure = RegimeGateOnFailureClosed
			},
			Steps:  []okxGoldenStep{{Name: "buy from flat", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)}},
			Expect: map[string][]string{"*": {"Regime gate: open signal blocked"}}},
		{Name: "gate_closed_bar_held", Types: okxGoldenBoth, Mode: "paper", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "held decision", Check: okxGoldenCheck{Signal: 1, Price: 100, ClosedBar: "held"}, Prices: okxGoldenPx(100)},
				{Name: "verified decision", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
			},
			Expect: map[string][]string{"*": {"Closed-bar decision: BUY signal suppressed"}}},
		{Name: "gate_htf_unavailable", Types: okxGoldenBoth, Mode: "paper", Config: func(sc *StrategyConfig) { sc.HTFFilter = true },
			Steps: []okxGoldenStep{
				{Name: "unavailable 1", Check: okxGoldenCheck{Signal: 1, Price: 100, HTF: "unavailable"}, Prices: okxGoldenPx(100)},
				{Name: "unavailable 2", Check: okxGoldenCheck{Signal: 1, Price: 100, HTF: "unavailable"}, Prices: okxGoldenPx(100)},
				{Name: "unavailable 3 alerts", Check: okxGoldenCheck{Signal: 1, Price: 100, HTF: "unavailable"}, Prices: okxGoldenPx(100)},
				{Name: "recovered opens", Check: okxGoldenCheck{Signal: 1, Price: 101, HTF: "ok"}, Prices: okxGoldenPx(101)},
				{Name: "unavailable keeps the close", Check: okxGoldenCheck{Signal: -1, Price: 103, HTF: "unavailable"}, Prices: okxGoldenPx(103)},
			},
			Expect: map[string][]string{"*": {"HTF filter: BUY signal suppressed"}}},
		{Name: "gate_hurst", Types: okxGoldenBoth, Mode: "paper",
			Config: func(sc *StrategyConfig) {
				sc.HurstGate = &HurstGateConfig{Enabled: true, Min: floatPtr(0.55), OnFailure: HurstGateOnFailureClosed}
			},
			Steps:  []okxGoldenStep{{Name: "buy from flat", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)}},
			Expect: map[string][]string{"*": {"Hurst gate: BUY signal suppressed"}}},
		{Name: "gate_paused", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "paused buy", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) { h.sc.Paused = true }},
				{Name: "unpaused buy opens", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) { h.sc.Paused = false }},
				{Name: "paused add held", Check: okxGoldenBuy(101), Prices: okxGoldenPx(101), Before: func(h *okxGoldenHarness) { h.sc.Paused = true }},
				{Name: "paused sell closes", Check: okxGoldenSell(102), Prices: okxGoldenPx(102)},
			},
			Expect: map[string][]string{"*": {"Paused: BUY signal suppressed"}}},
		{Name: "gate_daily_loss", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "held buy", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) { h.sr.DailyLossEntriesHeld = true }},
			},
			Expect: map[string][]string{"*": {"Daily loss limit: BUY signal suppressed"}}},
		{Name: "gate_notional", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "held buy", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) { h.sr.NotionalBlocked = true }},
			},
			Expect: map[string][]string{"*": {"Notional cap: BUY signal suppressed"}}},
		{Name: "gate_persistence", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "buy opens and the cycle save fails", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), FailSave: true},
				{Name: "add held by the persistence hold", Check: okxGoldenBuy(101), Prices: okxGoldenPx(101), FailSave: true},
				{Name: "sell closes under the hold", Check: okxGoldenSell(102), Prices: okxGoldenPx(102)},
				{Name: "buy after the save recovered", Check: okxGoldenBuy(103), Prices: okxGoldenPx(103)},
			},
			Expect: map[string][]string{"*": {"Persistence hold: BUY signal suppressed"}}},
		{Name: "gate_exposure_cap", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "held buy", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) {
					h.sr.ExposureCapStatus = ExposureCapStatus{Configured: true, CapUSD: 500, LongUSD: 600, LongBlocked: true}
				}},
			},
			Expect: map[string][]string{"*": {"Exposure cap: BUY signal suppressed"}}},
		{Name: "gate_compound_keeps_reductions", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "buy opens", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
				{Name: "every hold on an add", Check: okxGoldenBuy(101), Prices: okxGoldenPx(101), Before: okxGoldenAllHolds},
				{Name: "every hold on a close", Check: okxGoldenSell(102), Prices: okxGoldenPx(102), Before: okxGoldenAllHolds},
			}},
		{Name: "closed_bar_same_candle", Types: okxGoldenBoth, Mode: "paper", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "buy admitted", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
				{Name: "sell closes", Check: okxGoldenSell(101), Prices: okxGoldenPx(101)},
				{Name: "same candle re-entry held", Check: okxGoldenBuy(102), Prices: okxGoldenPx(102)},
			},
			Expect: map[string][]string{"*": {"Closed-bar entry: BUY open held"}}},
		{Name: "check_crash", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "crash 1", Check: okxGoldenCheck{Raw: "Traceback: fixture crash", Exit: 1, Stderr: "fixture stderr line"}, Prices: okxGoldenPx(100)},
				{Name: "crash 2", Check: okxGoldenCheck{Raw: "Traceback: fixture crash", Exit: 1}, Prices: okxGoldenPx(100)},
				{Name: "crash 3 alerts", Check: okxGoldenCheck{Raw: "Traceback: fixture crash", Exit: 1}, Prices: okxGoldenPx(100)},
				{Name: "recovered", Check: okxGoldenCheck{Price: 100, Stderr: "fixture recovered stderr"}, Prices: okxGoldenPx(100)},
			},
			Expect: map[string][]string{"*": {"Script failed:"}}},
		{Name: "check_reported_error", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{
				{Name: "error with failing exit", Check: okxGoldenCheck{Signal: 1, Price: 100, Error: "fixture strategy error", Exit: 1}, Prices: okxGoldenPx(100)},
				{Name: "error with clean exit", Check: okxGoldenCheck{Signal: 1, Price: 100, Error: "fixture strategy error"}, Prices: okxGoldenPx(100)},
			},
			Expect: map[string][]string{"*": {"Script returned error: fixture strategy error"}}},
		{Name: "check_parse_error", Types: okxGoldenBoth, Mode: "paper",
			Steps:  []okxGoldenStep{{Name: "garbage stdout", Check: okxGoldenCheck{Raw: "not json"}, Prices: okxGoldenPx(100)}},
			Expect: map[string][]string{"*": {"parse output"}}},
		{Name: "check_closed_bar_contract", Types: okxGoldenBoth, Mode: "paper", Config: closedBar,
			Steps:  []okxGoldenStep{{Name: "missing decision block", Check: okxGoldenCheck{Signal: 1, Price: 100, ClosedBar: "none"}, Prices: okxGoldenPx(100)}},
			Expect: map[string][]string{"*": {"closed-bar decision contract mismatch"}}},
		{Name: "check_htf_contract", Types: okxGoldenBoth, Mode: "paper", Config: func(sc *StrategyConfig) { sc.HTFFilter = true },
			Steps:  []okxGoldenStep{{Name: "missing filter block", Check: okxGoldenCheck{Signal: 1, Price: 100, HTF: "none"}, Prices: okxGoldenPx(100)}},
			Expect: map[string][]string{"*": {"higher-timeframe filter contract mismatch"}}},
		{Name: "check_no_price", Types: okxGoldenBoth, Mode: "paper",
			Steps:  []okxGoldenStep{{Name: "no price anywhere", Check: okxGoldenCheck{Signal: 1}, Prices: map[string]float64{}}},
			Expect: map[string][]string{"*": {"No price available for BTC"}}},
		{Name: "check_price_fallback", Types: okxGoldenBoth, Mode: "paper",
			Steps: []okxGoldenStep{{Name: "cached price opens", Check: okxGoldenCheck{Signal: 1}, Prices: okxGoldenPx(123)}}},
		{Name: "live_open_close_plain", Types: okxGoldenBoth, Mode: "live",
			Steps: []okxGoldenStep{
				{Name: "buy fills", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFill(1)}},
				{Name: "sell fills", Check: okxGoldenSell(101), Prices: okxGoldenPx(101), Venue: []okxGoldenVenue{okxGoldenFill(1)}},
			}},
		{Name: "live_closed_bar_filled", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "buy fills", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFill(1)}},
				{Name: "same candle add held", Check: okxGoldenBuy(101), Prices: okxGoldenPx(101)},
			}},
		{Name: "live_unknown_fill", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "unknown outcome", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenOutcome("unknown", "order has no confirmed outcome")}},
				{Name: "held without resend", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
			},
			Expect: map[string][]string{"*": {"has no resolved venue outcome"}}},
		{Name: "live_partial_fill", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "partial fill books the confirmed quantity", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFill(0.4)}},
			}},
		{Name: "live_rejected", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "venue rejects", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenOutcome("rejected", "fixture venue rejected the order")}},
			},
			Expect: map[string][]string{"*": {"Live execute returned error: fixture venue rejected the order"}}},
		{Name: "live_transport_error", Types: okxGoldenBoth, Mode: "live",
			Steps: []okxGoldenStep{
				{Name: "plain send fails", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenTransportError("fixture connection reset")}},
			},
			Expect: map[string][]string{"*": {"Live execute failed: fixture connection reset"}}},
		{Name: "live_reservation_failure", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "reservation write fails", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) {
					h.execSQL(`CREATE TRIGGER okx_fixture_reserve_fail BEFORE INSERT ON entry_order_intents BEGIN SELECT RAISE(ABORT, 'injected reservation write failure'); END`)
				}},
			},
			Expect: map[string][]string{"*": {"the live attempt could not be saved before sending"}}},
		{Name: "live_submit_gate_failure", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "submission record fails", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Before: func(h *okxGoldenHarness) {
					h.execSQL(`CREATE TRIGGER okx_fixture_submit_fail BEFORE UPDATE ON entry_order_intents BEGIN SELECT RAISE(ABORT, 'injected submission write failure'); END`)
				}},
			},
			Expect: map[string][]string{"*": {"the durable submission record failed"}}},
		{Name: "live_spot_cash_reconcile", Types: []string{"spot"}, Mode: "live",
			Steps: []okxGoldenStep{
				{Name: "over-budget fill raises the cash alert", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFillAt(100, 20)}},
				{Name: "sell still closes", Check: okxGoldenSell(101), Prices: okxGoldenPx(101), Venue: []okxGoldenVenue{okxGoldenFillAt(101, 20)}},
				{Name: "buy from flat held for reconcile", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100)},
			},
			Expect: map[string][]string{"spot": {"cash reconcile required"}}},
		{Name: "live_perps_reversal_reduce_only", Types: []string{"perps"}, Mode: "live",
			Config: func(sc *StrategyConfig) { sc.ClosedBarDecisions = true; sc.Direction = DirectionBoth },
			Steps: []okxGoldenStep{
				{Name: "buy fills", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFill(1)}},
				{Name: "reversal closes reduce-only", Check: okxGoldenSell(99), Prices: okxGoldenPx(99), Venue: []okxGoldenVenue{okxGoldenFill(1)}},
			},
			Expect: map[string][]string{"perps": {"reversal closes the open position this check"}}},
		{Name: "live_persistence_failure", Types: okxGoldenBoth, Mode: "live", Config: closedBar,
			Steps: []okxGoldenStep{
				{Name: "buy fills and the cycle save fails", Check: okxGoldenBuy(100), Prices: okxGoldenPx(100), Venue: []okxGoldenVenue{okxGoldenFill(1)}, FailSave: true},
				{Name: "add held, nothing resent", Check: okxGoldenBuy(101), Prices: okxGoldenPx(101)},
			},
			Expect: map[string][]string{"*": {"Persistence hold: BUY signal suppressed"}}},
	}
}

func okxGoldenAllHolds(h *okxGoldenHarness) {
	h.sc.Paused = true
	h.sr.DailyLossEntriesHeld = true
	h.sr.NotionalBlocked = true
	h.sr.ExposureCapStatus = ExposureCapStatus{Configured: true, CapUSD: 500, LongUSD: 600, LongBlocked: true}
}
