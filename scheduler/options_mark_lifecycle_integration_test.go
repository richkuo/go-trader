package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lcPricer struct {
	reply func(req optionLegRequest, refreshAt time.Time) optionLegReply
}

func (p lcPricer) Name() string { return "lifecycle-test" }

func (p lcPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	var out []optionLegReply
	for _, req := range reqs {
		r := p.reply(req, refreshAt)
		r.Key = req.Key
		out = append(out, r)
	}
	return out
}

func lcRobinhoodSchedule(cutoff time.Time) func(req optionLegRequest, refreshAt time.Time) optionLegReply {
	return func(req optionLegRequest, refreshAt time.Time) optionLegReply {
		return optionLegReply{CutoffAt: cutoff, ReadyAt: cutoff.Add(time.Hour), Pending: "no daily bar for SPY on expected session"}
	}
}

func lcSC(id, source string) StrategyConfig {
	sc := ocSC(id, "robinhood", "SPY")
	sc.PaperSource = source
	return sc
}

func lcCount(t *testing.T, env *entryTestEnv, id, query string) int {
	t.Helper()
	db := env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(query, sid).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func lcRows(t *testing.T, env *entryTestEnv, id string) (assignments, closed, legs int) {
	t.Helper()
	return lcCount(t, env, id, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND trade_type = 'assignment'`),
		lcCount(t, env, id, `SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ?`),
		lcCount(t, env, id, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`)
}

func lcSeedPut(s *StrategyState, expiry string, qty float64) {
	s.OptionPositions["SPY-put-sell-470-"+expiry] = &OptionPosition{ID: "SPY-put-sell-470-" + expiry, TradePositionID: "pos-put-" + expiry,
		Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: expiry, Action: "sell", Quantity: qty, ContractMultiplier: 100,
		EntryPremiumUSD: 300, CurrentValueUSD: -300, OpenedAt: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)}
}

func lcRefresh(env *entryTestEnv, sc StrategyConfig, pricer OptionPricer, notifier *MultiNotifier, throttle *optionSettlementAlertThrottle, at time.Time) optionLegApplyResult {
	logger, _ := ocLogger(sc.ID)
	return runOptionMarkRefresh(optionMarkRefresh{SC: sc, State: env.strat(sc.ID), Pricer: pricer, Mu: &env.mu, Notifier: notifier,
		Throttle: throttle, Logger: logger, RefreshAt: at})
}

func lcNotifier(cfg *Config) (*MultiNotifier, *mockNotifier) {
	backend := &mockNotifier{}
	cfg.Discord.Channels = map[string]string{"robinhood": "live-ch", "robinhood-paper": "paper-ch", "robinhood-paper:src": "src-ch"}
	mn := NewMultiNotifier(notifierBackend{notifier: backend, ownerID: "owner", channels: cfg.Discord.Channels})
	mn.ReloadConfig(cfg)
	return mn, backend
}

func lcSettlementAlerts(m *mockNotifier) (channels []string, dms int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages {
		if strings.Contains(msg.content, "OPTION SETTLEMENT PENDING") {
			channels = append(channels, msg.channelID)
		}
	}
	for _, dm := range m.dms {
		if strings.Contains(dm.content, "OPTION SETTLEMENT PENDING") {
			dms++
		}
	}
	return channels, dms
}

func TestOptionMarkLifecycleIntegration(t *testing.T) {
	const expiry = "2026-01-16"
	winterClose := time.Date(2026, 1, 16, 21, 0, 0, 0, time.UTC)
	afterReady := winterClose.Add(90 * time.Minute)

	t.Run("failed_settlement_holds_then_settles_once_across_restarts", func(t *testing.T) {
		sc := lcSC("rh-lc-settle", "")
		env := ocEnv(t, sc)
		notifier, backend := lcNotifier(env.cfg)
		throttle := newOptionSettlementAlertThrottle()
		s := env.strat(sc.ID)
		s.Cash = 200000
		lcSeedPut(s, expiry, 2)
		env.saveAll()

		res := lcRefresh(env, sc, lcPricer{reply: lcRobinhoodSchedule(winterClose)}, notifier, throttle, afterReady)
		leg := s.OptionPositions["SPY-put-sell-470-"+expiry]
		if leg == nil || res.Settled != 0 || len(res.Alerts) != 1 {
			t.Fatalf("pending settlement must hold the leg and alert once: res=%+v", res)
		}
		if !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) || leg.DTE != 0 || !leg.ExpiryCutoffAt.Equal(winterClose) {
			t.Fatalf("held leg = %+v", leg)
		}
		if s.Cash != 200000 || len(s.Positions) != 0 || len(s.ClosedOptionPositions) != 0 {
			t.Fatalf("a pending settlement changed the book: cash=%v positions=%v closed=%v", s.Cash, s.Positions, s.ClosedOptionPositions)
		}
		if ch, dms := lcSettlementAlerts(backend); len(ch) != 1 || ch[0] != "paper-ch" || dms != 1 {
			t.Fatalf("alert routing = %v dms=%d, want one paper-ch message and one DM", ch, dms)
		}
		lcRefresh(env, sc, lcPricer{reply: lcRobinhoodSchedule(winterClose)}, notifier, throttle, afterReady.Add(time.Minute))
		if ch, dms := lcSettlementAlerts(backend); len(ch) != 1 || dms != 1 {
			t.Fatalf("the throttle must hold a repeat alert: channels=%v dms=%d", ch, dms)
		}
		env.saveAll()
		env = env.restart()
		useEntryTradeRecorder(t, env.store)
		s = env.strat(sc.ID)
		if a, c, l := lcRows(t, env, sc.ID); a != 0 || c != 0 || l != 1 || s.Cash != 200000 {
			t.Fatalf("after a pending save: assignments=%d closed=%d legs=%d cash=%v", a, c, l, s.Cash)
		}
		if leg := s.OptionPositions["SPY-put-sell-470-"+expiry]; leg == nil || !strings.HasPrefix(leg.MarkError, optionSettlementPendingPrefix) || !leg.ExpiryCutoffAt.Equal(winterClose) {
			t.Fatalf("restart lost the pending state: %+v", leg)
		}

		settle := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			return optionLegReply{CutoffAt: winterClose, ReadyAt: winterClose.Add(time.Hour),
				Settlement: &optionLegSettlement{Price: 460, Basis: optionSettlementBasisRawClose, Date: expiry, Source: "yahoo_daily_raw_close"}}
		}}
		res = lcRefresh(env, sc, settle, notifier, throttle, afterReady.Add(time.Hour))
		if res.Settled != 1 || len(s.OptionPositions) != 0 || s.Cash != 200000-470*200 {
			t.Fatalf("settle: res=%+v legs=%d cash=%v", res, len(s.OptionPositions), s.Cash)
		}
		if a, c, l := lcRows(t, env, sc.ID); a != 0 || c != 0 || l != 1 {
			t.Fatalf("settlement rows must wait for the state save: assignments=%d closed=%d legs=%d", a, c, l)
		}
		env = env.restart()
		useEntryTradeRecorder(t, env.store)
		s = env.strat(sc.ID)
		if a, c, l := lcRows(t, env, sc.ID); a != 0 || c != 0 || l != 1 || s.Cash != 200000 || len(s.OptionPositions) != 1 || len(s.Positions) != 0 {
			t.Fatalf("a restart before the save must reload the open leg only: assignments=%d closed=%d legs=%d cash=%v", a, c, l, s.Cash)
		}

		res = lcRefresh(env, sc, settle, notifier, throttle, afterReady.Add(2*time.Hour))
		if res.Settled != 1 {
			t.Fatalf("second settle attempt: %+v", res)
		}
		env.saveAll()
		if a, c, l := lcRows(t, env, sc.ID); a != 1 || c != 1 || l != 0 {
			t.Fatalf("after the save: assignments=%d closed=%d legs=%d, want 1/1/0", a, c, l)
		}
		var details string
		db := env.fileFor(sc.ID)
		sid, _ := db.toStorageID(sc.ID)
		if err := db.db.QueryRow(`SELECT details FROM trades WHERE strategy_id = ? AND trade_type = 'assignment'`, sid).Scan(&details); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(details, "[settlement yahoo_daily_raw_close raw_close 2026-01-16 $460.00]") {
			t.Fatalf("assignment details = %q, want the settlement label", details)
		}
		res = lcRefresh(env, sc, settle, notifier, throttle, afterReady.Add(3*time.Hour))
		env.saveAll()
		env = env.restart()
		s = env.strat(sc.ID)
		if a, c, l := lcRows(t, env, sc.ID); a != 1 || c != 1 || l != 0 || s.Cash != 200000-470*200 || s.Positions["SPY"] == nil || s.Positions["SPY"].Quantity != 200 {
			t.Fatalf("a repeated refresh and restart settled again: assignments=%d closed=%d legs=%d cash=%v positions=%+v", a, c, l, s.Cash, s.Positions)
		}
		throttle.mu.Lock()
		left := len(throttle.last)
		throttle.mu.Unlock()
		if left != 0 {
			t.Fatalf("the throttle kept %d entries after the leg left the book", left)
		}
	})

	t.Run("alerts_reach_the_leg_partition", func(t *testing.T) {
		dir := t.TempDir()
		def := lcSC("rh-lc-default", "")
		src := lcSC("rh-lc-source", "src")
		src.StorageStrategyID = "rh-lc-alias"
		cfg := &Config{DBFile: filepath.Join(dir, "state.db"), IntervalSeconds: 600,
			PaperSources: []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}}, Strategies: []StrategyConfig{def, src}}
		env := openEntryTestEnv(t, cfg)
		useEntryTradeRecorder(t, env.store)
		notifier, backend := lcNotifier(cfg)
		throttle := newOptionSettlementAlertThrottle()
		for _, sc := range []StrategyConfig{def, src} {
			env.strat(sc.ID).Cash = 100000
			lcSeedPut(env.strat(sc.ID), expiry, 1)
			lcRefresh(env, sc, lcPricer{reply: lcRobinhoodSchedule(winterClose)}, notifier, throttle, afterReady)
		}
		ch, dms := lcSettlementAlerts(backend)
		if strings.Join(ch, ",") != "paper-ch,src-ch" || dms != 2 {
			t.Fatalf("alert channels = %v dms=%d, want paper-ch then src-ch and two DMs", ch, dms)
		}
		env.saveAll()
		env = env.restart()
		for _, sc := range []StrategyConfig{def, src} {
			if a, c, l := lcRows(t, env, sc.ID); a != 0 || c != 0 || l != 1 {
				t.Fatalf("%s after save: assignments=%d closed=%d legs=%d", sc.ID, a, c, l)
			}
		}
	})

	t.Run("quantity_and_multiplier_parity", func(t *testing.T) {
		at := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			platform, underlying, action string
			perUnit, want                float64
		}{
			{"robinhood", "SPY", "buy", 3.5, 3.5 * 100 * 2},
			{"robinhood", "SPY", "sell", 3.5, -3.5 * 100 * 2},
			{"deribit", "BTC", "buy", 1500, 1500 * 2},
			{"deribit", "BTC", "sell", 1500, -1500 * 2},
			{"ibkr", "BTC", "sell", 800, -800 * 2},
		} {
			s := &StrategyState{ID: tc.platform + "-parity", Platform: tc.platform, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"leg": {ID: "leg", Underlying: tc.underlying, OptionType: "call", Strike: 100, Expiry: "2026-12-18", Action: tc.action, Quantity: 2}}}
			pricer := lcPricer{reply: func(req optionLegRequest, refreshAt time.Time) optionLegReply {
				cutoff, ready, ok, _ := optionGoExpirySchedule(tc.platform, req.Expiry)
				if !ok {
					cutoff, ready = time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC), time.Date(2026, 12, 18, 22, 0, 0, 0, time.UTC)
				}
				return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Mark: &optionLegMark{PerUnitUSD: tc.perUnit, Spot: 100, Source: "parity"}}
			}}
			reqs := collectOptionLegRequests(s, tc.platform, at)
			logger, _ := ocLogger(s.ID)
			applyOptionLegReplies(s, tc.platform, reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
			if got := s.OptionPositions["leg"].CurrentValueUSD; !ocNear(got, tc.want) {
				t.Fatalf("%s %s value = %v, want %v", tc.platform, tc.action, got, tc.want)
			}
		}
	})

	t.Run("fixed_clock_cutoffs", func(t *testing.T) {
		deribitCutoff := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
		mkDeribit := func() *StrategyState {
			return &StrategyState{ID: "deribit-clock", Platform: "deribit", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"leg": {ID: "leg", TradePositionID: "p1", Underlying: "BTC", OptionType: "call", Strike: 70000, Expiry: "2026-06-26", Action: "sell", Quantity: 1, EntryPremiumUSD: 100, CurrentValueUSD: -100}}}
		}
		both := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			cutoff, ready, _, _ := optionGoExpirySchedule("deribit", req.Expiry)
			if at.Before(cutoff) {
				return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Mark: &optionLegMark{PerUnitUSD: 50, Spot: 60000, Source: "clock"}}
			}
			return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Settlement: &optionLegSettlement{Price: 60000, Basis: optionSettlementBasisDelivery, Date: req.Expiry, Source: "clock"}}
		}}
		for _, tc := range []struct {
			name    string
			at      time.Time
			settled bool
		}{
			{"one_nanosecond_before_cutoff_marks", deribitCutoff.Add(-time.Nanosecond), false},
			{"cutoff_equality_settles", deribitCutoff, true},
		} {
			s := mkDeribit()
			reqs := collectOptionLegRequests(s, "deribit", tc.at)
			logger, _ := ocLogger(s.ID)
			res := applyOptionLegReplies(s, "deribit", reqs, refreshOptionLegs(both, reqs, tc.at), tc.at, logger)
			if (res.Settled == 1) != tc.settled || (res.Marked == 1) == tc.settled {
				t.Fatalf("%s: res=%+v", tc.name, res)
			}
		}

		for _, tc := range []struct {
			name   string
			cutoff time.Time
		}{
			{"summer_new_york_close", time.Date(2026, 7, 17, 20, 0, 0, 0, time.UTC)},
			{"winter_new_york_close", time.Date(2026, 1, 16, 21, 0, 0, 0, time.UTC)},
			{"early_close_session", time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)},
		} {
			at := tc.cutoff.Add(-36 * time.Hour)
			wantDTE := 1.5
			s := &StrategyState{ID: "rh-clock", Platform: "robinhood", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"leg": {ID: "leg", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: tc.cutoff.Format("2006-01-02"), Action: "sell", Quantity: 1, CurrentValueUSD: -100}}}
			pricer := lcPricer{reply: func(req optionLegRequest, refreshAt time.Time) optionLegReply {
				return optionLegReply{CutoffAt: tc.cutoff, ReadyAt: tc.cutoff.Add(time.Hour), Mark: &optionLegMark{PerUnitUSD: 1, Spot: 480, Source: "clock"}}
			}}
			reqs := collectOptionLegRequests(s, "robinhood", at)
			if reqs[0].Op != optionLegOpResolve {
				t.Fatalf("%s: a robinhood leg with no stored cutoff must ask Python to resolve it, got %q", tc.name, reqs[0].Op)
			}
			logger, _ := ocLogger(s.ID)
			applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
			leg := s.OptionPositions["leg"]
			if !leg.ExpiryCutoffAt.Equal(tc.cutoff) || !ocNear(leg.DTE, wantDTE) || leg.markVerifiedAt.IsZero() {
				t.Fatalf("%s: leg cutoff=%s dte=%v verified=%s, want %s and %v", tc.name, leg.ExpiryCutoffAt, leg.DTE, leg.markVerifiedAt, tc.cutoff, wantDTE)
			}
			later := at.Add(12 * time.Hour)
			failing := lcPricer{reply: func(req optionLegRequest, refreshAt time.Time) optionLegReply {
				return optionLegReply{Err: "option quote unavailable: robinhood option market data missing"}
			}}
			reqs = collectOptionLegRequests(s, "robinhood", later)
			applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(failing, reqs, later), later, logger)
			if !ocNear(leg.DTE, wantDTE-0.5) || leg.CurrentValueUSD != -1*100 || leg.MarkError == "" || !leg.markVerifiedAt.IsZero() {
				t.Fatalf("%s: a failed read must keep the last value, advance DTE from the stored cutoff and revoke eligibility: %+v", tc.name, leg)
			}
		}

		t.Run("confirmed_non_session_expiry_settles_on_the_prior_session", func(t *testing.T) {
			s := &StrategyState{ID: "rh-weekend", Platform: "robinhood", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"leg": {ID: "leg", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2026-01-17", Action: "sell", Quantity: 1, EntryPremiumUSD: 100, CurrentValueUSD: -100}}}
			pricer := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
				return optionLegReply{CutoffAt: winterClose, ReadyAt: winterClose.Add(time.Hour), Settlement: &optionLegSettlement{Price: 480, Basis: optionSettlementBasisRawClose, Date: "2026-01-16", Source: "clock"}}
			}}
			reqs := collectOptionLegRequests(s, "robinhood", afterReady)
			logger, _ := ocLogger(s.ID)
			if res := applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(pricer, reqs, afterReady), afterReady, logger); res.Settled != 1 || len(s.OptionPositions) != 0 {
				t.Fatalf("non-session expiry settle: %+v", res)
			}
		})

		t.Run("unresolved_session_and_invalid_date_hold", func(t *testing.T) {
			at := time.Date(2026, 1, 20, 15, 0, 0, 0, time.UTC)
			s := &StrategyState{ID: "rh-unresolved", Platform: "robinhood", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"future":  {ID: "future", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2026-02-20", Action: "sell", Quantity: 1, DTE: 31, CurrentValueUSD: -100},
				"past":    {ID: "past", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2026-01-16", Action: "sell", Quantity: 1, DTE: 1, CurrentValueUSD: -100},
				"invalid": {ID: "invalid", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2026-13-40", Action: "sell", Quantity: 1, DTE: 7, CurrentValueUSD: -100},
			}}
			pricer := lcPricer{reply: func(req optionLegRequest, refreshAt time.Time) optionLegReply {
				return optionLegReply{Err: "session unresolved: expiry is outside the XNYS calendar bounds"}
			}}
			reqs := collectOptionLegRequests(s, "robinhood", at)
			logger, _ := ocLogger(s.ID)
			res := applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
			if len(s.OptionPositions) != 3 || s.Cash != 1000 || len(res.Failures) != 3 {
				t.Fatalf("unresolved legs must stay: legs=%d cash=%v res=%+v", len(s.OptionPositions), s.Cash, res)
			}
			alerted := map[string]bool{}
			for _, a := range res.Alerts {
				alerted[a.Key] = true
			}
			if alerted["future"] || !alerted["past"] || !alerted["invalid"] {
				t.Fatalf("alerts = %+v, want past and invalid only", res.Alerts)
			}
			for key, dte := range map[string]float64{"future": 31, "past": 1, "invalid": 7} {
				if leg := s.OptionPositions[key]; leg.DTE != dte || !strings.HasPrefix(leg.MarkError, optionCutoffUnresolvedPrefix) {
					t.Fatalf("%s: DTE must stay when the cutoff is unknown: %+v", key, leg)
				}
			}
		})

		t.Run("delayed_settlement_availability", func(t *testing.T) {
			s := mkDeribit()
			published := false
			pricer := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
				cutoff, ready, _, _ := optionGoExpirySchedule("deribit", req.Expiry)
				if !published {
					return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Pending: "deribit_delivery_price for btc_usd 2026-06-26 is not published yet"}
				}
				return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Settlement: &optionLegSettlement{Price: 60000, Basis: optionSettlementBasisDelivery, Date: req.Expiry, Source: deribitSettlementSource}}
			}}
			logger, _ := ocLogger(s.ID)
			step := func(at time.Time) optionLegApplyResult {
				reqs := collectOptionLegRequests(s, "deribit", at)
				return applyOptionLegReplies(s, "deribit", reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
			}
			if res := step(deribitCutoff.Add(30 * time.Minute)); res.Waiting != 1 || len(res.Alerts) != 0 || len(res.Failures) != 0 {
				t.Fatalf("before the read boundary: %+v", res)
			}
			if res := step(deribitCutoff.Add(2 * time.Hour)); len(res.Alerts) != 1 || len(s.OptionPositions) != 1 {
				t.Fatalf("after the read boundary: %+v", res)
			}
			published = true
			if res := step(deribitCutoff.Add(3 * time.Hour)); res.Settled != 1 || len(s.OptionPositions) != 0 || s.Cash != 1000 {
				t.Fatalf("late publication: %+v cash=%v", res, s.Cash)
			}
		})
	})
}
