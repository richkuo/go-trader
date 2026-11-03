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
		rows, reason, _ := elClosedRows(t, env, sc.ID)
		if a, _, l := lcRows(t, env, sc.ID); a != 1 || rows != 1 || reason != "expired_itm" || l != 0 || s.Cash != 200000-800*100 {
			t.Fatalf("settlement: assignments=%d closed=%d reason=%q legs=%d cash=%v", a, rows, reason, l, s.Cash)
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

	t.Run("deribit_leg_at_0805_before_any_refresh_since_the_cutoff", func(t *testing.T) {
		deribitCutoff := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
		for _, known := range []bool{true, false} {
			sc := ocSC("deribit-el-cb", "deribit", "BTC")
			env := ocEnv(t, sc)
			s := env.strat(sc.ID)
			s.Cash = 100000
			leg := &OptionPosition{ID: "BTC-put-sell-60000-2026-06-26", TradePositionID: "pos-btc", Underlying: "BTC", OptionType: "put",
				Strike: 60000, Expiry: "2026-06-26", Action: "sell", Quantity: 1, EntryPremiumUSD: 900, CurrentValueUSD: -1200,
				MarkUpdatedAt: deribitCutoff.Add(-10 * time.Minute), MarkSource: "deribit_mark"}
			if known {
				leg.ExpiryCutoffAt = deribitCutoff
				leg.SettlementReadyAt = deribitCutoff.Add(time.Hour)
			}
			s.OptionPositions[leg.ID] = leg
			elBindClock(t, deribitCutoff.Add(5*time.Minute))
			elFireDrawdown(t, sc, s)
			if s.OptionPositions[leg.ID] == nil || s.Cash != 100000 || len(s.ClosedOptionPositions) != 0 || leg.MarkError != "" {
				t.Fatalf("known cutoff=%v: circuit breaker closed the leg: legs=%d cash=%v", known, len(s.OptionPositions), s.Cash)
			}
			unbindTradingClock("expired-leg-test-" + t.Name())
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
