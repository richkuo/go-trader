package main

import (
	"strings"
	"testing"
	"time"
)

func elBindClock(t *testing.T, at time.Time) {
	t.Helper()
	owner := "expired-leg-test-" + t.Name()
	if err := bindTradingClock(owner, func() time.Time { return at }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock(owner) })
}

func elFireDrawdown(t *testing.T, sc StrategyConfig, s *StrategyState) {
	t.Helper()
	if ok, reason := CheckRisk(&sc, s, 200000, nil, nil, nil); !ok {
		t.Fatalf("peak check blocked: %s", reason)
	}
	if ok, reason := CheckRisk(&sc, s, 20000, nil, nil, nil); ok || !strings.Contains(reason, RiskReasonMaxDrawdownExceeded) {
		t.Fatalf("expected a drawdown fire, got allowed=%v reason=%q", ok, reason)
	}
}

func elClosedRows(t *testing.T, env *entryTestEnv, id string) (rows int, reason string, pnl float64) {
	t.Helper()
	db := env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ?`, sid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 1 {
		if err := db.db.QueryRow(`SELECT close_reason, realized_pnl FROM closed_option_positions WHERE strategy_id = ?`, sid).Scan(&reason, &pnl); err != nil {
			t.Fatal(err)
		}
	}
	return rows, reason, pnl
}

func TestOptionExpiredLegLeavesOnlyThroughSettlementIntegration(t *testing.T) {
	const pendingKey = "SPY-put-sell-800-2026-10-30"
	cutoff := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	afterReady := cutoff.Add(2 * time.Hour)
	pending := lcPricer{reply: lcRobinhoodSchedule(cutoff)}
	settle := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
		if req.Expiry != "2026-10-30" {
			return optionLegReply{CutoffAt: time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC), ReadyAt: time.Date(2026, 12, 18, 22, 0, 0, 0, time.UTC),
				Mark: &optionLegMark{PerUnitUSD: 5, Spot: 760, Source: "test"}}
		}
		return optionLegReply{CutoffAt: cutoff, ReadyAt: cutoff.Add(time.Hour),
			Settlement: &optionLegSettlement{Price: 755.72, Basis: optionSettlementBasisRawClose, Date: "2026-10-30", Source: "yahoo_daily_raw_close"}}
	}}
	seedPending := func(s *StrategyState) {
		s.OptionPositions[pendingKey] = &OptionPosition{ID: pendingKey, TradePositionID: "pos-800", Underlying: "SPY", OptionType: "put",
			Strike: 800, Expiry: "2026-10-30", Action: "sell", Quantity: 1, ContractMultiplier: 100,
			EntryPremiumUSD: 300, CurrentValueUSD: -4000, OpenedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}
	}

	t.Run("circuit_breaker_keeps_a_pending_robinhood_put_until_settlement", func(t *testing.T) {
		sc := lcSC("rh-el-cb", "")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 200000
		seedPending(s)
		env.saveAll()
		notifier, _ := lcNotifier(env.cfg)
		throttle := newOptionSettlementAlertThrottle()
		if res := lcRefresh(env, sc, pending, notifier, throttle, afterReady); res.Settled != 0 {
			t.Fatalf("pending refresh settled: %+v", res)
		}
		if leg := s.OptionPositions[pendingKey]; leg == nil || !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) {
			t.Fatalf("leg must be pending: %+v", leg)
		}
		elBindClock(t, afterReady)
		trades := len(s.TradeHistory)
		elFireDrawdown(t, sc, s)
		logger, _ := ocLogger(sc.ID)
		action := OptionsAction{Action: "close", OptionType: "put", Strike: 800, Expiry: "2026-10-30"}
		if closed, err := executeOptionClose(s, ocResult("robinhood", "SPY", 760, action), &action, logger); err != nil || closed != 0 {
			t.Fatalf("a signal close removed a pending leg: closed=%d err=%v", closed, err)
		}
		if s.OptionPositions[pendingKey] == nil || s.Cash != 200000 || len(s.TradeHistory) != trades || len(s.ClosedOptionPositions) != 0 {
			t.Fatalf("circuit breaker booked a pending leg: legs=%d cash=%v trades=%d closed=%d", len(s.OptionPositions), s.Cash, len(s.TradeHistory), len(s.ClosedOptionPositions))
		}
		s.OptionPositions[pendingKey].markVerifiedAt = afterReady
		harvest := &ThetaHarvestConfig{Enabled: true, ProfitTargetPct: 1, StopLossPct: 1, MinDTEClose: 30}
		if n, _ := CheckThetaHarvest(s, harvest, optionHarvestClock{Now: afterReady, MaxMarkAge: time.Hour}, logger); n != 0 || s.OptionPositions[pendingKey] == nil || s.Cash != 200000 || len(s.TradeHistory) != trades {
			t.Fatalf("theta harvest closed a pending leg: trades=%d cash=%v", n, s.Cash)
		}
		s.OptionPositions[pendingKey].MarkError = ""
		if n, _ := CheckThetaHarvest(s, harvest, optionHarvestClock{Now: afterReady, MaxMarkAge: time.Hour}, logger); n != 0 || s.OptionPositions[pendingKey] == nil || s.RiskState.ConsecutiveLosses != 0 {
			t.Fatalf("theta harvest closed a leg past its cutoff: trades=%d", n)
		}
		s.OptionPositions[pendingKey].MarkError = optionSettlementPendingPrefix + "test"
		s.OptionPositions[pendingKey].markVerifiedAt = time.Time{}
		env.saveAll()
		if a, c, l := lcRows(t, env, sc.ID); a != 0 || c != 0 || l != 1 {
			t.Fatalf("after the trip: assignments=%d closed=%d legs=%d", a, c, l)
		}
		if n := lcCount(t, env, sc.ID, `SELECT COUNT(*) FROM trades WHERE strategy_id = ?`); n != 0 {
			t.Fatalf("the trip wrote %d trade rows", n)
		}
		if res := lcRefresh(env, sc, settle, notifier, throttle, afterReady.Add(time.Hour)); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		env.saveAll()
		rows, reason, pnl := elClosedRows(t, env, sc.ID)
		if a, _, l := lcRows(t, env, sc.ID); a != 1 || rows != 1 || reason != "expired_itm" || l != 0 || s.Cash != 200000-800*100 || !ocNear(pnl, 300-(800-755.72)*100) {
			t.Fatalf("settlement: assignments=%d closed=%d reason=%q pnl=%v legs=%d cash=%v", a, rows, reason, pnl, l, s.Cash)
		}
		if p := s.Positions["SPY"]; p == nil || p.Quantity != 100 || !ocNear(p.AvgCost, 755.72) {
			t.Fatalf("assigned shares = %+v, want 100 at the 755.72 settlement price", p)
		}
		if n := lcCount(t, env, sc.ID, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND trade_type = 'options' AND is_close = 1`); n != 1 {
			t.Fatalf("option close rows = %d, want 1", n)
		}
	})

	t.Run("paper_kill_switch_closes_the_live_leg_and_keeps_the_pending_leg", func(t *testing.T) {
		sc := lcSC("rh-el-ks", "")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 200000
		seedPending(s)
		s.OptionPositions["SPY-call-buy-900-2026-12-18"] = &OptionPosition{ID: "SPY-call-buy-900-2026-12-18", TradePositionID: "pos-900",
			Underlying: "SPY", OptionType: "call", Strike: 900, Expiry: "2026-12-18", Action: "buy", Quantity: 1, ContractMultiplier: 100,
			EntryPremiumUSD: 400, CurrentValueUSD: 500, ExpiryCutoffAt: time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC),
			OpenedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}
		notifier, _ := lcNotifier(env.cfg)
		mixed := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			if req.Expiry == "2026-10-30" {
				return pending.reply(req, at)
			}
			return optionLegReply{CutoffAt: time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC), ReadyAt: time.Date(2026, 12, 18, 22, 0, 0, 0, time.UTC),
				Mark: &optionLegMark{PerUnitUSD: 5, Spot: 760, Source: "test"}}
		}}
		lcRefresh(env, sc, mixed, notifier, newOptionSettlementAlertThrottle(), afterReady)
		if leg := s.OptionPositions[pendingKey]; leg == nil || !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) {
			t.Fatalf("leg must be pending: %+v", leg)
		}
		elBindClock(t, afterReady)
		part := partitionFor(sc)
		if closed := forceClosePaperScopePositions(env.state, env.cfg, part, nil); len(closed) != 1 {
			t.Fatalf("kill switch closed %v", closed)
		}
		if s.OptionPositions[pendingKey] == nil || s.OptionPositions["SPY-call-buy-900-2026-12-18"] != nil || len(s.OptionPositions) != 1 {
			t.Fatalf("kill switch legs left = %v", sortedOptionLegKeys(s.OptionPositions))
		}
		if !ocNear(s.Cash, 200500) || len(s.ClosedOptionPositions) != 1 || s.ClosedOptionPositions[0].CloseReason != "circuit_breaker" {
			t.Fatalf("live leg close: cash=%v closed=%+v", s.Cash, s.ClosedOptionPositions)
		}
		trades := len(s.TradeHistory)
		forceClosePaperScopePositions(env.state, env.cfg, part, nil)
		if s.OptionPositions[pendingKey] == nil || len(s.TradeHistory) != trades || !ocNear(s.Cash, 200500) || len(s.ClosedOptionPositions) != 1 {
			t.Fatalf("a later force close touched the pending leg: legs=%d trades=%d cash=%v", len(s.OptionPositions), len(s.TradeHistory), s.Cash)
		}
		env.saveAll()
		if _, c, l := lcRows(t, env, sc.ID); c != 1 || l != 1 {
			t.Fatalf("after save: closed=%d legs=%d", c, l)
		}
	})

	t.Run("ibkr_leg_at_1502_before_any_refresh_since_the_cutoff", func(t *testing.T) {
		ibkrCutoff := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
		for _, known := range []bool{true, false} {
			sc := ocSC("ibkr-el-cb", "ibkr", "BTC")
			env := ocEnv(t, sc)
			s := env.strat(sc.ID)
			s.Cash = 100000
			leg := &OptionPosition{ID: "BTC-put-sell-60000-2026-06-26", TradePositionID: "pos-btc", Underlying: "BTC", OptionType: "put",
				Strike: 60000, Expiry: "2026-06-26", Action: "sell", Quantity: 1, EntryPremiumUSD: 900, CurrentValueUSD: -1200,
				MarkUpdatedAt: ibkrCutoff.Add(-10 * time.Minute), MarkSource: ibkrMarkSource}
			if known {
				leg.ExpiryCutoffAt = ibkrCutoff
				leg.SettlementReadyAt = ibkrCutoff.Add(5 * time.Minute)
			}
			s.OptionPositions[leg.ID] = leg
			elBindClock(t, ibkrCutoff.Add(2*time.Minute))
			elFireDrawdown(t, sc, s)
			if s.OptionPositions[leg.ID] == nil || s.Cash != 100000 || len(s.ClosedOptionPositions) != 0 || leg.MarkError != "" {
				t.Fatalf("known cutoff=%v: circuit breaker closed the leg: legs=%d cash=%v", known, len(s.OptionPositions), s.Cash)
			}
			unbindTradingClock("expired-leg-test-" + t.Name())
		}
	})

	t.Run("paper_kill_switch_message_names_every_leg_it_keeps", func(t *testing.T) {
		fire := func(env *entryTestEnv, sc StrategyConfig, hasOwner bool) paperKillSwitchOutcome {
			part := partitionFor(sc)
			env.state.partitionRisk(part).KillSwitchActive = true
			return applyPaperKillSwitchCycle(env.state, env.cfg, nil, &scopeCycleRisk{
				Partition: part, KillSwitchFired: true, Reason: "paper drawdown 90.0% exceeds limit 25.0%", TotalPV: 20000, PeakRebaselineAvailable: true,
			}, hasOwner)
		}
		keptLine := "Option legs at or past their expiry cutoff stay open until settlement books them: "

		sc := lcSC("rh-el-only", "")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 200000
		seedPending(s)
		notifier, _ := lcNotifier(env.cfg)
		lcRefresh(env, sc, pending, notifier, newOptionSettlementAlertThrottle(), afterReady)
		elBindClock(t, afterReady)
		out := fire(env, sc, true)
		if len(out.Closed) != 0 || s.OptionPositions[pendingKey] == nil || strings.Contains(out.Message, "force-closed at mark:") ||
			!strings.Contains(out.Message, "No paper book was force-closed. "+keptLine+"rh-el-only "+pendingKey+".") ||
			strings.Join(out.Kept, ",") != "rh-el-only "+pendingKey {
			t.Fatalf("pending-only book: closed=%v kept=%v message=%q", out.Closed, out.Kept, out.Message)
		}

		sc = lcSC("rh-el-mixed", "")
		env = ocEnv(t, sc)
		s = env.strat(sc.ID)
		s.Cash = 200000
		seedPending(s)
		s.OptionPositions["SPY-call-buy-900-2026-12-18"] = &OptionPosition{ID: "SPY-call-buy-900-2026-12-18", TradePositionID: "pos-900",
			Underlying: "SPY", OptionType: "call", Strike: 900, Expiry: "2026-12-18", Action: "buy", Quantity: 1, ContractMultiplier: 100,
			EntryPremiumUSD: 400, CurrentValueUSD: 500, ExpiryCutoffAt: time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC),
			OpenedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}
		s.OptionPositions[pendingKey].MarkError = optionSettlementPendingPrefix + " test"
		out = fire(env, sc, true)
		if strings.Join(out.Closed, ",") != "rh-el-mixed" || s.OptionPositions[pendingKey] == nil || len(s.OptionPositions) != 1 ||
			!strings.Contains(out.Message, "Paper books force-closed at mark: rh-el-mixed "+keptLine+"rh-el-mixed "+pendingKey+".") {
			t.Fatalf("mixed book: closed=%v legs=%v message=%q", out.Closed, sortedOptionLegKeys(s.OptionPositions), out.Message)
		}

		ibkrCutoff := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
		dsc := ocSC("ibkr-el-ks", "ibkr", "BTC")
		env = ocEnv(t, dsc)
		s = env.strat(dsc.ID)
		s.Cash = 100000
		legID := "BTC-put-sell-60000-2026-06-26"
		s.OptionPositions[legID] = &OptionPosition{ID: legID, TradePositionID: "pos-btc", Underlying: "BTC", OptionType: "put",
			Strike: 60000, Expiry: "2026-06-26", Action: "sell", Quantity: 1, EntryPremiumUSD: 900, CurrentValueUSD: -1200,
			MarkUpdatedAt: ibkrCutoff.Add(-10 * time.Minute), MarkSource: ibkrMarkSource}
		unbindTradingClock("expired-leg-test-" + t.Name())
		elBindClock(t, ibkrCutoff.Add(2*time.Minute))
		out = fire(env, dsc, false)
		if !out.AutoReset || len(out.Closed) != 0 || s.OptionPositions[legID] == nil ||
			!strings.Contains(out.Message, keptLine+"ibkr-el-ks "+legID+".") || !strings.Contains(out.Message, paperKillSwitchAutoResetLine) {
			t.Fatalf("ibkr auto-reset: reset=%v closed=%v message=%q", out.AutoReset, out.Closed, out.Message)
		}
	})

	t.Run("bought_leg_in_the_money_credits_its_intrinsic_to_cash", func(t *testing.T) {
		sc := lcSC("rh-el-long", "")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 50000
		key := "SPY-call-buy-700-2026-10-30"
		s.OptionPositions[key] = &OptionPosition{ID: key, TradePositionID: "pos-700", Underlying: "SPY", OptionType: "call",
			Strike: 700, Expiry: "2026-10-30", Action: "buy", Quantity: 2, ContractMultiplier: 100,
			EntryPremiumUSD: 1000, CurrentValueUSD: 11000, OpenedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}
		env.saveAll()
		notifier, _ := lcNotifier(env.cfg)
		if res := lcRefresh(env, sc, settle, notifier, newOptionSettlementAlertThrottle(), afterReady); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		want := 50000 + (755.72-700)*100*2
		if !ocNear(s.Cash, want) || len(s.Positions) != 0 {
			t.Fatalf("cash=%v positions=%v, want cash %v and no delivery", s.Cash, s.Positions, want)
		}
		env = env.restart()
		if s := env.strat(sc.ID); s.Cash != 50000 || len(s.OptionPositions) != 1 {
			t.Fatalf("a restart before the save must reload the open leg: cash=%v legs=%d", s.Cash, len(s.OptionPositions))
		}
		useEntryTradeRecorder(t, env.store)
		s = env.strat(sc.ID)
		lcRefresh(env, sc, settle, notifier, newOptionSettlementAlertThrottle(), afterReady.Add(time.Hour))
		env.saveAll()
		env = env.restart()
		s = env.strat(sc.ID)
		rows, _, pnl := elClosedRows(t, env, sc.ID)
		if !ocNear(s.Cash, want) || rows != 1 || !ocNear(pnl, (755.72-700)*100*2-1000) || len(s.OptionPositions) != 0 {
			t.Fatalf("after save: cash=%v closed=%d pnl=%v legs=%d", s.Cash, rows, pnl, len(s.OptionPositions))
		}
	})
}

type ksrRun struct {
	t     *testing.T
	env   *entryTestEnv
	sc    StrategyConfig
	part  RiskPartition
	clock time.Time
}

func ksrStart(t *testing.T, id string, at time.Time) *ksrRun {
	t.Helper()
	sc := lcSC(id, "")
	env := ocEnv(t, sc)
	env.cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 25}
	r := &ksrRun{t: t, env: env, sc: sc, part: partitionFor(sc), clock: at}
	owner := "kept-leg-reset-test-" + t.Name()
	if err := bindTradingClock(owner, func() time.Time { return r.clock }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock(owner) })
	s := env.strat(sc.ID)
	s.Cash = 7300
	s.OptionPositions["SPY-put-sell-800-2026-10-30"] = &OptionPosition{ID: "SPY-put-sell-800-2026-10-30", TradePositionID: "pos-800",
		Underlying: "SPY", OptionType: "put", Strike: 800, Expiry: "2026-10-30", Action: "sell", Quantity: 1, ContractMultiplier: 100,
		EntryPremiumUSD: 300, CurrentValueUSD: -300, ExpiryCutoffAt: time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC),
		OpenedAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}
	env.state.partitionRisk(r.part).PeakValue = 10000
	env.saveAll()
	return r
}

func (r *ksrRun) save(p RiskPartition) error { return r.env.store.SavePartition(r.env.state, p) }

func (r *ksrRun) cycle(hasOwner bool) (*scopeCycleRisk, paperKillSwitchOutcome) {
	r.t.Helper()
	r.clock = r.clock.Add(10 * time.Minute)
	env := r.env
	inputsFrom := tradingNow().UTC()
	sr := measureScopeCycleRisk(r.part, partitionRiskConfig(env.cfg, r.part), env.cfg.Strategies, env.state, nil, nil, nil, true, false, tradingNow().UTC())
	prs := env.state.partitionRisk(r.part)
	measureScopeRebaselineValuation(sr, prs, env.cfg.Strategies, env.state, nil, cyclePriceSources{}, nil, nil, env.cfg.IntervalSeconds, inputsFrom, tradingNow().UTC())
	applyScopeCycleRiskCommitted(sr, prs, r.save)
	if sr.RebaselineSaveErr != nil {
		r.t.Fatalf("rebaseline save: %v", sr.RebaselineSaveErr)
	}
	var out paperKillSwitchOutcome
	if sr.KillSwitchFired {
		out = applyPaperKillSwitchCycle(env.state, env.cfg, nil, sr, hasOwner)
		if err := commitPaperKillSwitchAutoReset(env.state, r.part, &out, r.save); err != nil {
			r.t.Fatalf("auto-reset commit: %v", err)
		}
	}
	return sr, out
}

func (r *ksrRun) quietCycles(label string, n int, hasOwner bool, refresh func()) {
	r.t.Helper()
	prs := r.env.state.partitionRisk(r.part)
	for i := 1; i <= n; i++ {
		if refresh != nil {
			refresh()
		}
		sr, out := r.cycle(hasOwner)
		if sr.KillSwitchFired || prs.KillSwitchActive || out.Message != "" || out.CloseApplied || out.AutoReset {
			r.t.Fatalf("%s cycle %d: fired=%v latched=%v message=%q reason=%q gap=%q", label, i, sr.KillSwitchFired, prs.KillSwitchActive, out.Message, sr.Reason, sr.ValuationGap)
		}
		if i == 1 && (!sr.Rebaselined || sr.RebaselineOldPeak != 10000 || sr.RebaselineNewPeak != 7000) {
			r.t.Fatalf("%s cycle 1: rebaselined=%v old=%.2f new=%.2f gap=%q; want 10000 -> 7000", label, sr.Rebaselined, sr.RebaselineOldPeak, sr.RebaselineNewPeak, sr.ValuationGap)
		}
		if i > 1 && sr.Rebaselined {
			r.t.Fatalf("%s cycle %d rebaselined again", label, i)
		}
		if len(captureKillSwitchLatchInstances(r.env.state, []RiskPartition{r.part})) != 0 {
			r.t.Fatalf("%s cycle %d left a latch for a new owner prompt", label, i)
		}
	}
}

func TestKillSwitchResetWithLegKeptForSettlementIntegration(t *testing.T) {
	const key = "SPY-put-sell-800-2026-10-30"
	cutoff := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	pending := lcPricer{reply: lcRobinhoodSchedule(cutoff)}

	firstLatch := func(t *testing.T, r *ksrRun, hasOwner bool) {
		t.Helper()
		sr, out := r.cycle(hasOwner)
		prs := r.env.state.partitionRisk(r.part)
		if !sr.KillSwitchFired || !out.CloseApplied || len(out.Closed) != 0 || strings.Join(out.Kept, ",") != r.sc.ID+" "+key || out.Message == "" {
			t.Fatalf("first latch: fired=%v closeApplied=%v closed=%v kept=%v message=%q", sr.KillSwitchFired, out.CloseApplied, out.Closed, out.Kept, out.Message)
		}
		if out.AutoReset == hasOwner || prs.KillSwitchActive != hasOwner || r.env.strat(r.sc.ID).OptionPositions[key] == nil {
			t.Fatalf("first latch: autoReset=%v latched=%v owner=%v", out.AutoReset, prs.KillSwitchActive, hasOwner)
		}
	}
	reloaded := func(t *testing.T, r *ksrRun, peak float64, triggered, autoResets, rebaselines int) {
		t.Helper()
		r.env.saveAll()
		r.env = r.env.restart()
		useEntryTradeRecorder(t, r.env.store)
		prs := r.env.state.partitionRisk(r.part)
		if prs.PeakValue != peak || countEvents(prs, "triggered") != triggered || countEvents(prs, "auto_reset") != autoResets || countEvents(prs, "reset_rebaseline") != rebaselines {
			t.Fatalf("stored risk row: peak=%.2f triggered=%d auto_reset=%d reset_rebaseline=%d; want %.2f, %d, %d, %d", prs.PeakValue,
				countEvents(prs, "triggered"), countEvents(prs, "auto_reset"), countEvents(prs, "reset_rebaseline"), peak, triggered, autoResets, rebaselines)
		}
	}

	t.Run("no_owner_before_the_read_boundary_three_cycles_stay_quiet", func(t *testing.T) {
		r := ksrStart(t, "rh-ksr-wait", cutoff)
		firstLatch(t, r, false)
		notifier, _ := lcNotifier(r.env.cfg)
		throttle := newOptionSettlementAlertThrottle()
		cycles := 0
		r.quietCycles("before the read boundary", 3, false, func() {
			cycles++
			if cycles == 1 {
				return
			}
			res := lcRefresh(r.env, r.sc, pending, notifier, throttle, r.clock)
			if leg := r.env.strat(r.sc.ID).OptionPositions[key]; res.Waiting != 1 || leg == nil || !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) {
				t.Fatalf("the leg must wait for its read boundary: %+v", res)
			}
		})
		if !r.clock.Before(cutoff.Add(time.Hour)) {
			t.Fatalf("the cycles must end before the read boundary; clock %s", r.clock)
		}
		reloaded(t, r, 7000, 1, 1, 1)
	})

	t.Run("no_owner_failed_settlements_stay_quiet_and_a_new_drop_latches", func(t *testing.T) {
		r := ksrStart(t, "rh-ksr-fail", cutoff.Add(2*time.Hour))
		notifier, _ := lcNotifier(r.env.cfg)
		throttle := newOptionSettlementAlertThrottle()
		refresh := func() {
			res := lcRefresh(r.env, r.sc, pending, notifier, throttle, r.clock)
			if leg := r.env.strat(r.sc.ID).OptionPositions[key]; len(res.Failures) != 1 || res.Waiting != 0 || leg == nil || !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) {
				t.Fatalf("the settlement must fail past the read boundary: %+v", res)
			}
		}
		refresh()
		firstLatch(t, r, false)
		r.quietCycles("failed settlements", 3, false, refresh)
		reloaded(t, r, 7000, 1, 1, 1)

		r.env.strat(r.sc.ID).Cash = 5000
		refresh()
		sr, out := r.cycle(false)
		if !sr.KillSwitchFired || !out.CloseApplied || out.Message == "" || !strings.Contains(sr.Reason, "32.9%") {
			t.Fatalf("a drop to 4700 from the post-reset peak 7000 must latch: fired=%v reason=%q", sr.KillSwitchFired, sr.Reason)
		}
		reloaded(t, r, 7000, 2, 2, 1)
	})

	t.Run("owner_reset_with_a_kept_leg_does_not_latch_again", func(t *testing.T) {
		r := ksrStart(t, "rh-ksr-owner", cutoff)
		firstLatch(t, r, true)
		inst := captureKillSwitchLatchInstances(r.env.state, []RiskPartition{r.part})
		code := newKillSwitchResetCode()
		if res := applyKillSwitchResetReply(r.env.state, killSwitchResetRequest{Code: code, Prompted: inst, Reply: "reset " + code}, r.save); res.Err != nil || res.Target != r.part {
			t.Fatalf("owner reset: %+v", res)
		}
		notifier, _ := lcNotifier(r.env.cfg)
		throttle := newOptionSettlementAlertThrottle()
		r.quietCycles("owner reset", 3, true, func() { lcRefresh(r.env, r.sc, pending, notifier, throttle, r.clock) })
		prs := r.env.state.partitionRisk(r.part)
		if countEvents(prs, "reset") != 1 || countEvents(prs, "auto_reset") != 0 {
			t.Fatalf("owner path events: reset=%d auto_reset=%d", countEvents(prs, "reset"), countEvents(prs, "auto_reset"))
		}
		reloaded(t, r, 7000, 1, 0, 1)
	})
}
