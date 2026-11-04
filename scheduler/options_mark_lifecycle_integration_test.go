package main

import (
	"errors"
	"fmt"
	"math"
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
		if a, c, l := lcRows(t, env, sc.ID); a != 1 || c != 1 || l != 0 || s.Cash != 200000-470*200 || s.Positions["SPY"] == nil || s.Positions["SPY"].Quantity != 200 || s.Positions["SPY"].AvgCost != 460 {
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

const esExpiry = "2026-06-26"

var esAt = time.Date(2026, 6, 26, 23, 0, 0, 0, time.UTC)

func esSettle(platform string, price float64) lcPricer {
	return lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
		cutoff, ready, ok, _ := optionGoExpirySchedule(platform, req.Expiry)
		if !ok {
			day, _ := time.Parse("2006-01-02", req.Expiry)
			cutoff, ready = day.Add(20*time.Hour), day.Add(21*time.Hour)
		}
		st := &optionLegSettlement{Price: price, Date: req.Expiry}
		switch platform {
		case "robinhood":
			st.Basis, st.Source = optionSettlementBasisRawClose, "yahoo_daily_raw_close"
		case "ibkr":
			st.Basis, st.Source = optionSettlementBasisIBKR, ibkrSettlementSource
		case "okx":
			st.Basis, st.Source = optionSettlementBasisDelivery, okxDeribitProxySource
		default:
			st.Basis, st.Source = optionSettlementBasisDelivery, deribitSettlementSource
		}
		return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Settlement: st}
	}}
}

func esLeg(s *StrategyState, underlying, optionType, action string, strike, qty, mult, premium float64) string {
	key := fmt.Sprintf("%s-%s-%s-%.0f-%s", underlying, optionType, action, strike, esExpiry)
	value := premium
	if action == "sell" {
		value = -premium
	}
	s.OptionPositions[key] = &OptionPosition{ID: key, TradePositionID: "tp-" + key, Underlying: underlying, OptionType: optionType,
		Strike: strike, Expiry: esExpiry, Action: action, Quantity: qty, ContractMultiplier: mult, EntryPremiumUSD: premium,
		CurrentValueUSD: value, OpenedAt: time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)}
	return key
}

func esRun(env *entryTestEnv, sc StrategyConfig, pricer OptionPricer) (optionLegApplyResult, string) {
	logger, buf := ocLogger(sc.ID)
	res := runOptionMarkRefresh(optionMarkRefresh{SC: sc, State: env.strat(sc.ID), Pricer: pricer, Mu: &env.mu, Logger: logger, RefreshAt: esAt})
	return res, buf.String()
}

type esTrade struct {
	symbol, positionID, side, tradeType, details, regime, ts string
	qty, price, value, fee, pnl                              float64
	isClose, gross                                           bool
}

func esTrades(t *testing.T, env *entryTestEnv, id string) []esTrade {
	t.Helper()
	db := env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT symbol, position_id, side, trade_type, details, regime, timestamp, quantity, price, value, exchange_fee, realized_pnl, is_close, pnl_gross
		FROM trades WHERE strategy_id = ? ORDER BY rowid`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []esTrade
	for rows.Next() {
		var r esTrade
		if err := rows.Scan(&r.symbol, &r.positionID, &r.side, &r.tradeType, &r.details, &r.regime, &r.ts, &r.qty, &r.price, &r.value, &r.fee, &r.pnl, &r.isClose, &r.gross); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

type esClosedOption struct {
	positionID, reason, closedAt string
	closePrice, pnl              float64
}

func esClosedOptions(t *testing.T, env *entryTestEnv, id string) []esClosedOption {
	t.Helper()
	db := env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT position_id, close_reason, closed_at, close_price_usd, realized_pnl FROM closed_option_positions WHERE strategy_id = ? ORDER BY id`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []esClosedOption
	for rows.Next() {
		var r esClosedOption
		if err := rows.Scan(&r.positionID, &r.reason, &r.closedAt, &r.closePrice, &r.pnl); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func esOptionRows(trades []esTrade) (opt, assign []esTrade) {
	for _, tr := range trades {
		switch tr.tradeType {
		case "options":
			opt = append(opt, tr)
		case "assignment":
			assign = append(assign, tr)
		}
	}
	return opt, assign
}

func esCheckOptionClose(t *testing.T, tr esTrade, key string, qty, intrinsic, pnl float64, side, label string) {
	t.Helper()
	if tr.symbol != key || tr.positionID != "tp-"+key || tr.side != side || tr.qty != qty || !ocNear(tr.price, intrinsic) || !ocNear(tr.value, intrinsic) ||
		!ocNear(tr.pnl, pnl) || !tr.isClose || !tr.gross || tr.fee != 0 || tr.regime != "ranging" || tr.ts != formatTime(esAt) ||
		!strings.Contains(tr.details, "[settlement "+label+"]") {
		t.Fatalf("option close row = %+v, want %s qty=%v side=%s value=%v pnl=%v label %q at %s", tr, key, qty, side, intrinsic, pnl, label, formatTime(esAt))
	}
}

func esStrategy(t *testing.T, id, platform, underlying string, cash float64) (*entryTestEnv, StrategyConfig, *StrategyState) {
	t.Helper()
	sc := ocSC(id, platform, underlying)
	env := ocEnv(t, sc)
	s := env.strat(sc.ID)
	s.Cash = cash
	s.Regime = "ranging"
	s.RiskState.DailyPnLDate = esAt.Format("2006-01-02")
	elBindClock(t, esAt)
	return env, sc, s
}

func TestOptionExpirySettlementIntegration(t *testing.T) {
	t.Run("deribit_bought_call_in_the_money_credits_cash_and_records_a_win", func(t *testing.T) {
		env, sc, s := esStrategy(t, "deribit-es-long-call", "deribit", "BTC", 10000)
		key := esLeg(s, "BTC", "call", "buy", 60000, 1, 1, 1000)
		s.OptionPositions[key].CurrentValueUSD = 5000
		s.RiskState.ConsecutiveLosses = 2
		env.saveAll()
		pvBefore := PortfolioValue(s, nil)
		if res, _ := esRun(env, sc, esSettle("deribit", 65000)); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		if !ocNear(s.Cash, 15000) || !ocNear(PortfolioValue(s, nil), pvBefore) || len(s.Positions) != 0 {
			t.Fatalf("cash=%v pv=%v (before %v) positions=%v", s.Cash, PortfolioValue(s, nil), pvBefore, s.Positions)
		}
		if !ocNear(s.RiskState.DailyPnL, 4000) || s.RiskState.ConsecutiveLosses != 0 {
			t.Fatalf("risk = %+v, want daily +4000 and a reset streak", s.RiskState)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		if len(opt) != 1 || len(assign) != 0 {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
		esCheckOptionClose(t, opt[0], key, 1, 5000, 4000, "sell", "deribit_delivery_price delivery_price 2026-06-26 $65000.00")
		closed := esClosedOptions(t, env, sc.ID)
		if len(closed) != 1 || closed[0].reason != "expired_itm" || !ocNear(closed[0].pnl, 4000) || closed[0].positionID != key || closed[0].closedAt != formatTime(esAt) {
			t.Fatalf("closed = %+v", closed)
		}
	})

	t.Run("deribit_bought_put_out_of_the_money_records_a_loss", func(t *testing.T) {
		env, sc, s := esStrategy(t, "deribit-es-long-put", "deribit", "BTC", 10000)
		key := esLeg(s, "BTC", "put", "buy", 50000, 1, 1, 300)
		s.RiskState.ConsecutiveLosses = 1
		if res, _ := esRun(env, sc, esSettle("deribit", 60000)); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		if s.Cash != 10000 || s.RiskState.ConsecutiveLosses != 2 || !ocNear(s.RiskState.DailyPnL, -300) {
			t.Fatalf("cash=%v risk=%+v", s.Cash, s.RiskState)
		}
		env.saveAll()
		opt, _ := esOptionRows(esTrades(t, env, sc.ID))
		closed := esClosedOptions(t, env, sc.ID)
		if len(opt) != 1 || len(closed) != 1 || closed[0].reason != "expired_worthless" || !ocNear(closed[0].pnl, -300) {
			t.Fatalf("rows: options=%+v closed=%+v", opt, closed)
		}
		esCheckOptionClose(t, opt[0], key, 1, 0, -300, "sell", "deribit_delivery_price delivery_price 2026-06-26 $60000.00")
	})

	t.Run("deribit_sold_call_settles_cash_and_never_touches_btc", func(t *testing.T) {
		for _, held := range []float64{0, 0.5} {
			env, sc, s := esStrategy(t, fmt.Sprintf("deribit-es-call-%g", held), "deribit", "BTC", 10000)
			key := esLeg(s, "BTC", "call", "sell", 60000, 1, 1, 500)
			if held > 0 {
				s.Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: held, AvgCost: 40000, TradePositionID: "btc-1", OwnerStrategyID: sc.ID}
			}
			_, log := esRun(env, sc, esSettle("deribit", 66000))
			if !ocNear(s.Cash, 4000) || !ocNear(s.RiskState.DailyPnL, -5500) {
				t.Fatalf("held=%v cash=%v daily=%v", held, s.Cash, s.RiskState.DailyPnL)
			}
			if p := s.Positions["BTC"]; held > 0 && (p == nil || p.Quantity != held || p.AvgCost != 40000) || held == 0 && p != nil {
				t.Fatalf("held=%v BTC = %+v", held, p)
			}
			if strings.Contains(log, "UNCOVERED CALL ASSIGNMENT") {
				t.Fatalf("a cash platform logged a delivery warning:\n%s", log)
			}
			env.saveAll()
			opt, assign := esOptionRows(esTrades(t, env, sc.ID))
			if len(opt) != 1 || len(assign) != 0 {
				t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
			}
			esCheckOptionClose(t, opt[0], key, 1, 6000, -5500, "buy", "deribit_delivery_price delivery_price 2026-06-26 $66000.00")
			unbindTradingClock("expired-leg-test-" + t.Name())
		}
	})

	t.Run("sold_put_on_every_cash_platform_delivers_nothing", func(t *testing.T) {
		for _, tc := range []struct{ platform, label string }{
			{"deribit", "deribit_delivery_price delivery_price 2026-06-26 $54000.00"},
			{"okx", "deribit_proxy delivery_price 2026-06-26 $54000.00"},
			{"ibkr", "ibkr_cycle_price_legacy_model cycle_price_legacy_model 2026-06-26 $54000.00"},
		} {
			t.Run(tc.platform, func(t *testing.T) {
				env, sc, s := esStrategy(t, tc.platform+"-es-put", tc.platform, "BTC", 100000)
				key := esLeg(s, "BTC", "put", "sell", 60000, 1, 1, 500)
				if res, _ := esRun(env, sc, esSettle(tc.platform, 54000)); res.Settled != 1 {
					t.Fatalf("settle: %+v", res)
				}
				if !ocNear(s.Cash, 94000) || len(s.Positions) != 0 || !ocNear(s.RiskState.DailyPnL, -5500) || s.RiskState.ConsecutiveLosses != 1 {
					t.Fatalf("cash=%v positions=%v risk=%+v", s.Cash, s.Positions, s.RiskState)
				}
				env.saveAll()
				opt, assign := esOptionRows(esTrades(t, env, sc.ID))
				if len(opt) != 1 || len(assign) != 0 {
					t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
				}
				esCheckOptionClose(t, opt[0], key, 1, 6000, -5500, "buy", tc.label)
			})
		}
	})

	t.Run("robinhood_sold_put_delivers_shares_at_settlement_cost", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-put", "robinhood", "SPY", 50000)
		key := esLeg(s, "SPY", "put", "sell", 100, 1, 100, 300)
		if res, _ := esRun(env, sc, esSettle("robinhood", 90)); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		p := s.Positions["SPY"]
		if !ocNear(s.Cash, 40000) || p == nil || p.Side != "long" || p.Quantity != 100 || p.AvgCost != 90 || !p.OpenedAt.Equal(esAt) {
			t.Fatalf("cash=%v SPY=%+v, want cash 40000 and 100 shares at 90", s.Cash, p)
		}
		if !ocNear(s.RiskState.DailyPnL, -700) || s.RiskState.ConsecutiveLosses != 1 {
			t.Fatalf("risk = %+v, want only the option result -700", s.RiskState)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		label := "yahoo_daily_raw_close raw_close 2026-06-26 $90.00"
		if len(opt) != 1 || len(assign) != 1 {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
		esCheckOptionClose(t, opt[0], key, 1, 1000, -700, "buy", label)
		a := assign[0]
		if a.side != "buy" || a.qty != 100 || a.price != 90 || a.value != 9000 || a.isClose || a.pnl != 0 || a.positionID != p.TradePositionID ||
			a.ts != formatTime(esAt) || a.regime != "ranging" || !strings.Contains(a.details, "[settlement "+label+"]") {
			t.Fatalf("assignment row = %+v", a)
		}

		fire := sc
		fire.MaxDrawdownPct = 5
		s.RiskState.MaxDrawdownPct = 5
		s.RiskState.PeakValue = 100000
		if ok, reason := CheckRisk(&fire, s, PortfolioValue(s, map[string]float64{"SPY": 90}), map[string]float64{"SPY": 90}, nil, nil); ok || !strings.Contains(reason, RiskReasonMaxDrawdownExceeded) {
			t.Fatalf("breaker did not fire: ok=%v reason=%q", ok, reason)
		}
		if s.Positions["SPY"] != nil || !ocNear(s.Cash, 49000) {
			t.Fatalf("breaker sale: positions=%v cash=%v", s.Positions, s.Cash)
		}
		env.saveAll()
		trades := esTrades(t, env, sc.ID)
		last := trades[len(trades)-1]
		if len(trades) != 3 || last.symbol != "SPY" || !last.isClose || last.pnl != 0 || last.price != 90 || last.qty != 100 {
			t.Fatalf("breaker sale row = %+v of %d rows, want SPY close at 90 with zero PnL", last, len(trades))
		}
	})

	t.Run("robinhood_sold_put_keeps_a_short_and_settles_cash_only", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-put-short", "robinhood", "SPY", 50000)
		key := esLeg(s, "SPY", "put", "sell", 100, 1, 100, 300)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "short", Quantity: 20, AvgCost: 95, TradePositionID: "spy-short", OwnerStrategyID: sc.ID}
		_, log := esRun(env, sc, esSettle("robinhood", 90))
		p := s.Positions["SPY"]
		if !ocNear(s.Cash, 49000) || p == nil || p.Side != "short" || p.Quantity != 20 || p.AvgCost != 95 {
			t.Fatalf("cash=%v SPY=%+v", s.Cash, p)
		}
		if !strings.Contains(log, "PUT ASSIGNMENT NOT DELIVERED: sold put "+key) || !strings.Contains(log, "short position qty=20") {
			t.Fatalf("missing conflict WARN:\n%s", log)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		if len(opt) != 1 || len(assign) != 0 {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
	})

	t.Run("robinhood_covered_call_closes_shares_at_settlement", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-cc", "robinhood", "SPY", 50000)
		key := esLeg(s, "SPY", "call", "sell", 100, 1, 100, 300)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, InitialQuantity: 100, AvgCost: 80, TradePositionID: "spy-lot-1",
			OwnerStrategyID: sc.ID, OpenedAt: time.Date(2026, 5, 1, 15, 0, 0, 0, time.UTC), EntryATR: 2.5}
		if res, _ := esRun(env, sc, esSettle("robinhood", 110)); res.Settled != 1 {
			t.Fatalf("settle: %+v", res)
		}
		if !ocNear(s.Cash, 60000) || s.Positions["SPY"] != nil {
			t.Fatalf("cash=%v SPY=%+v, want 60000 and no shares", s.Cash, s.Positions["SPY"])
		}
		if !ocNear(s.RiskState.DailyPnL, 2300) || s.RiskState.ConsecutiveLosses != 0 {
			t.Fatalf("risk = %+v: the option loss then the share win must leave daily 2300 and streak 0", s.RiskState)
		}
		if len(s.ClosedPositions) != 1 || !s.ClosedPositions[0].ClosedAt.Equal(esAt) || s.ClosedPositions[0].RealizedPnL != 3000 || s.ClosedPositions[0].ClosePrice != 110 {
			t.Fatalf("closed positions = %+v", s.ClosedPositions)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		if len(opt) != 1 || len(assign) != 1 {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
		esCheckOptionClose(t, opt[0], key, 1, 1000, -700, "buy", "yahoo_daily_raw_close raw_close 2026-06-26 $110.00")
		a := assign[0]
		if a.side != "sell" || a.qty != 100 || a.price != 110 || a.value != 11000 || !a.isClose || !a.gross || a.pnl != 3000 || a.positionID != "spy-lot-1" || a.ts != formatTime(esAt) {
			t.Fatalf("call-away row = %+v", a)
		}
		if combined := opt[0].pnl + a.pnl; !ocNear(combined, 2300) {
			t.Fatalf("combined realized PnL = %v, want 2300", combined)
		}
	})

	t.Run("robinhood_call_with_missing_shares_delivers_only_held_units", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-legacy-call", "robinhood", "SPY", 50000)
		key := esLeg(s, "SPY", "call", "sell", 100, 2, 100, 600)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 150, AvgCost: 80, TradePositionID: "spy-lot-2", OwnerStrategyID: sc.ID}
		_, log := esRun(env, sc, esSettle("robinhood", 110))
		if want := 50000 + 110*150 - 10*200.0; !ocNear(s.Cash, want) || s.Positions["SPY"] != nil {
			t.Fatalf("cash=%v SPY=%+v, want %v and no shares", s.Cash, s.Positions["SPY"], want)
		}
		if !strings.Contains(log, "UNCOVERED CALL ASSIGNMENT: sold call "+key) || strings.Count(log, "UNCOVERED CALL ASSIGNMENT") != 1 {
			t.Fatalf("want one uncovered WARN naming %s:\n%s", key, log)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		if len(opt) != 1 || len(assign) != 1 || assign[0].qty != 150 || assign[0].value != 16500 || !ocNear(assign[0].pnl, 4500) {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
		esCheckOptionClose(t, opt[0], key, 2, 2000, -1400, "buy", "yahoo_daily_raw_close raw_close 2026-06-26 $110.00")
	})

	t.Run("two_calls_share_held_units_once_and_a_bought_call_takes_cash", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-two-calls", "robinhood", "SPY", 50000)
		first := esLeg(s, "SPY", "call", "sell", 100, 1, 100, 300)
		second := esLeg(s, "SPY", "call", "sell", 105, 1, 100, 200)
		bought := esLeg(s, "SPY", "call", "buy", 95, 1, 100, 400)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 130, AvgCost: 80, TradePositionID: "spy-lot-3", OwnerStrategyID: sc.ID}
		if res, _ := esRun(env, sc, esSettle("robinhood", 110)); res.Settled != 3 {
			t.Fatalf("settle: %+v", res)
		}
		want := 50000 + (110*100 - 1000) + (110*30 - 500) + 1500.0
		p := s.Positions["SPY"]
		if !ocNear(s.Cash, want) || p != nil {
			t.Fatalf("cash=%v SPY=%+v, want %v and no shares", s.Cash, p, want)
		}
		env.saveAll()
		opt, assign := esOptionRows(esTrades(t, env, sc.ID))
		if len(opt) != 3 || len(assign) != 2 || assign[0].qty != 100 || assign[1].qty != 30 || !strings.Contains(assign[0].details, first) || !strings.Contains(assign[1].details, second) {
			t.Fatalf("rows: options=%+v assignments=%+v", opt, assign)
		}
		if opt[0].symbol != bought || opt[0].side != "sell" || opt[0].value != 1500 || !ocNear(opt[0].pnl, 1100) {
			t.Fatalf("bought call row = %+v", opt[0])
		}
		if len(esClosedOptions(t, env, sc.ID)) != 3 {
			t.Fatalf("want one closed option record per leg")
		}
	})

	t.Run("partial_call_away_keeps_the_rest_of_the_lot", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-partial", "robinhood", "SPY", 50000)
		esLeg(s, "SPY", "call", "sell", 100, 1, 100, 300)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 250, AvgCost: 80, TradePositionID: "spy-lot-4", OwnerStrategyID: sc.ID}
		esRun(env, sc, esSettle("robinhood", 110))
		p := s.Positions["SPY"]
		if p == nil || p.Quantity != 150 || p.AvgCost != 80 || p.TradePositionID != "spy-lot-4" || len(s.ClosedPositions) != 0 || !ocNear(s.Cash, 60000) {
			t.Fatalf("SPY=%+v closed=%d cash=%v", p, len(s.ClosedPositions), s.Cash)
		}
		env.saveAll()
		env = env.restart()
		if p := env.strat(sc.ID).Positions["SPY"]; p == nil || p.Quantity != 150 {
			t.Fatalf("reload SPY = %+v", p)
		}
	})

	t.Run("sold_put_adds_to_held_shares_at_weighted_settlement_cost", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-put-add", "robinhood", "SPY", 50000)
		esLeg(s, "SPY", "put", "sell", 100, 1, 100, 300)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, AvgCost: 70, TradePositionID: "spy-lot-5", OwnerStrategyID: sc.ID}
		esRun(env, sc, esSettle("robinhood", 90))
		p := s.Positions["SPY"]
		if p == nil || p.Quantity != 200 || !ocNear(p.AvgCost, 80) || p.TradePositionID != "spy-lot-5" || !ocNear(s.Cash, 40000) {
			t.Fatalf("SPY=%+v cash=%v", p, s.Cash)
		}
	})

	t.Run("corrupt_held_long_is_not_cover_and_stays", func(t *testing.T) {
		env, sc, s := esStrategy(t, "rh-es-corrupt", "robinhood", "SPY", 50000)
		key := esLeg(s, "SPY", "call", "sell", 100, 1, 100, 300)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, AvgCost: 0, TradePositionID: "spy-corrupt", OwnerStrategyID: sc.ID}
		res, log := esRun(env, sc, esSettle("robinhood", 110))
		p := s.Positions["SPY"]
		if res.Settled != 1 || p == nil || p.Quantity != 100 || p.AvgCost != 0 || !ocNear(s.Cash, 49000) {
			t.Fatalf("res=%+v SPY=%+v cash=%v", res, p, s.Cash)
		}
		if !strings.Contains(log, "CORRUPT CALL COVER: sold call "+key) {
			t.Fatalf("missing corrupt-cover WARN:\n%s", log)
		}
	})
}

func TestOptionExpirySettlementHoldsIntegration(t *testing.T) {
	type snap struct {
		cash, daily float64
		losses      int
		trades      int
		closed      int
		positions   int
	}
	take := func(s *StrategyState) snap {
		return snap{s.Cash, s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses, len(s.TradeHistory), len(s.ClosedOptionPositions), len(s.Positions)}
	}
	deribitCutoff := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
	reply := func(fn func(st *optionLegSettlement) optionLegReply) lcPricer {
		return lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			st := &optionLegSettlement{Price: 54000, Basis: optionSettlementBasisDelivery, Date: req.Expiry, Source: deribitSettlementSource}
			r := fn(st)
			r.CutoffAt, r.ReadyAt = deribitCutoff, deribitCutoff.Add(time.Hour)
			return r
		}}
	}
	for _, tc := range []struct {
		name   string
		pricer lcPricer
		style  *string
		mutate func(leg *OptionPosition)
		prefix string
	}{
		{name: "price_error", pricer: reply(func(*optionLegSettlement) optionLegReply {
			return optionLegReply{Err: "deribit delivery read failed (upstream status 503)"}
		}), prefix: optionSettlementPendingPrefix},
		{name: "missing_settlement", pricer: reply(func(*optionLegSettlement) optionLegReply { return optionLegReply{} }), prefix: optionSettlementPendingPrefix},
		{name: "invalid_settlement", pricer: reply(func(st *optionLegSettlement) optionLegReply {
			st.Basis = optionSettlementBasisRawClose
			return optionLegReply{Settlement: st}
		}), prefix: optionSettlementPendingPrefix},
		{name: "absent_style", pricer: reply(func(st *optionLegSettlement) optionLegReply { return optionLegReply{Settlement: st} }), style: new(string), prefix: optionSettlementPendingPrefix},
		{name: "invalid_style", pricer: reply(func(st *optionLegSettlement) optionLegReply { return optionLegReply{Settlement: st} }), style: func() *string { v := "delivery"; return &v }(), prefix: optionSettlementPendingPrefix},
		{name: "non_finite_premium", pricer: reply(func(st *optionLegSettlement) optionLegReply { return optionLegReply{Settlement: st} }),
			mutate: func(leg *OptionPosition) { leg.EntryPremiumUSD = math.Inf(1) }, prefix: optionSettlementPendingPrefix},
		{name: "overflowing_quantity", pricer: reply(func(st *optionLegSettlement) optionLegReply { return optionLegReply{Settlement: st} }),
			mutate: func(leg *OptionPosition) { leg.Quantity = math.MaxFloat64 }, prefix: optionSettlementPendingPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.style != nil {
				prev := optionContractSpecs["deribit"]
				next := prev
				next.Settlement = *tc.style
				optionContractSpecs["deribit"] = next
				t.Cleanup(func() { optionContractSpecs["deribit"] = prev })
			}
			env, sc, s := esStrategy(t, "deribit-es-hold-"+strings.ReplaceAll(tc.name, "_", "-"), "deribit", "BTC", 100000)
			key := esLeg(s, "BTC", "put", "sell", 60000, 1, 1, 500)
			if tc.mutate != nil {
				tc.mutate(s.OptionPositions[key])
			}
			s.RiskState.ConsecutiveLosses = 1
			before := take(s)
			env.saveAll()
			res, _ := esRun(env, sc, tc.pricer)
			leg := s.OptionPositions[key]
			if res.Settled != 0 || leg == nil || leg.DTE != 0 || !strings.HasPrefix(leg.MarkError, tc.prefix) || take(s) != before {
				t.Fatalf("res=%+v leg=%+v snap=%+v (before %+v)", res, leg, take(s), before)
			}
			env.saveAll()
			if len(esTrades(t, env, sc.ID)) != 0 || len(esClosedOptions(t, env, sc.ID)) != 0 || lcCount(t, env, sc.ID, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`) != 1 {
				t.Fatalf("a held settlement wrote rows")
			}
		})
	}

	t.Run("unknown_platform_holds_on_its_existing_branch", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			stored bool
			prefix string
		}{
			{"no_stored_cutoff", false, optionCutoffUnresolvedPrefix},
			{"stored_cutoff", true, optionSettlementPendingPrefix},
		} {
			env, sc, s := esStrategy(t, "kraken-es-"+tc.name, "deribit", "BTC", 100000)
			sc.Platform = "kraken"
			key := esLeg(s, "BTC", "put", "sell", 60000, 1, 1, 500)
			if tc.stored {
				s.OptionPositions[key].ExpiryCutoffAt = deribitCutoff
				s.OptionPositions[key].SettlementReadyAt = deribitCutoff.Add(time.Hour)
			}
			before := take(s)
			res, _ := esRun(env, sc, esSettle("deribit", 54000))
			leg := s.OptionPositions[key]
			if res.Settled != 0 || leg == nil || !strings.HasPrefix(leg.MarkError, tc.prefix) || take(s) != before {
				t.Fatalf("%s: res=%+v leg=%+v snap=%+v (before %+v)", tc.name, res, leg, take(s), before)
			}
			unbindTradingClock("expired-leg-test-" + t.Name())
		}
	})
}

func TestOptionExpirySettlementStorageIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, platform, underlying string
		strike, settle, mult       float64
		wantCash                   float64
		wantAssign                 int
		named                      bool
	}{
		{"deribit_cash_default", "deribit", "BTC", 60000, 54000, 1, 100000 - 6000, 0, false},
		{"deribit_cash_named_source", "deribit", "BTC", 60000, 54000, 1, 100000 - 6000, 0, true},
		{"robinhood_physical_default", "robinhood", "SPY", 100, 90, 100, 100000 - 10000, 1, false},
		{"robinhood_physical_named_source", "robinhood", "SPY", 100, 90, 100, 100000 - 10000, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			other := ocSC("es-store-other", "deribit", "BTC")
			sc := ocSC("es-store-"+tc.platform, tc.platform, tc.underlying)
			cfg := &Config{DBFile: filepath.Join(dir, "state.db"), IntervalSeconds: 600, Strategies: []StrategyConfig{other, sc}}
			if tc.named {
				sc.PaperSource = "src"
				sc.StorageStrategyID = "es-store-alias"
				cfg.PaperSources = []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}}
				cfg.Strategies[1] = sc
			}
			env := openEntryTestEnv(t, cfg)
			useEntryTradeRecorder(t, env.store)
			elBindClock(t, esAt)
			seed := func(env *entryTestEnv) *StrategyState {
				s := env.strat(sc.ID)
				s.Regime = "ranging"
				return s
			}
			s := seed(env)
			s.Cash = 100000
			s.RiskState.DailyPnLDate = esAt.Format("2006-01-02")
			s.RiskState.ConsecutiveLosses = 1
			env.strat(other.ID).Cash = 5000
			key := esLeg(s, tc.underlying, "put", "sell", tc.strike, 1, tc.mult, 500)
			env.saveAll()

			if res, _ := esRun(env, sc, esSettle(tc.platform, tc.settle)); res.Settled != 1 || !ocNear(s.Cash, tc.wantCash) {
				t.Fatalf("settle: %+v cash=%v", res, s.Cash)
			}
			env = env.restart()
			useEntryTradeRecorder(t, env.store)
			s = seed(env)
			if s.Cash != 100000 || s.OptionPositions[key] == nil || s.RiskState.ConsecutiveLosses != 1 || s.RiskState.DailyPnL != 0 ||
				len(esTrades(t, env, sc.ID)) != 0 || len(esClosedOptions(t, env, sc.ID)) != 0 {
				t.Fatalf("restart before the save: cash=%v leg=%v risk=%+v", s.Cash, s.OptionPositions[key], s.RiskState)
			}

			esRun(env, sc, esSettle(tc.platform, tc.settle))
			prevHook := storeCommitHook
			storeCommitHook = func(storageRole) error { return errors.New("injected settlement save failure") }
			failed := 0
			for _, err := range env.store.SaveAll(env.state) {
				if err != nil {
					failed++
				}
			}
			storeCommitHook = prevHook
			if failed == 0 || len(esTrades(t, env, sc.ID)) != 0 || len(esClosedOptions(t, env, sc.ID)) != 0 ||
				lcCount(t, env, sc.ID, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`) != 1 {
				t.Fatalf("a failed save changed the file (failed partitions=%d)", failed)
			}
			env.saveAll()
			esRun(env, sc, esSettle(tc.platform, tc.settle))
			env.saveAll()
			env = env.restart()
			s = env.strat(sc.ID)
			opt, assign := esOptionRows(esTrades(t, env, sc.ID))
			if len(opt) != 1 || len(assign) != tc.wantAssign || len(esClosedOptions(t, env, sc.ID)) != 1 || len(s.OptionPositions) != 0 ||
				!ocNear(s.Cash, tc.wantCash) || s.RiskState.ConsecutiveLosses != 2 || !ocNear(s.RiskState.DailyPnL, 500-(tc.strike-tc.settle)*tc.mult) {
				t.Fatalf("after save and restart: options=%d assignments=%d cash=%v legs=%d risk=%+v", len(opt), len(assign), s.Cash, len(s.OptionPositions), s.RiskState)
			}
			if tc.wantAssign == 1 && (s.Positions["SPY"] == nil || s.Positions["SPY"].AvgCost != 90 || s.Positions["SPY"].Quantity != 100) {
				t.Fatalf("reloaded SPY = %+v", s.Positions["SPY"])
			}
			if o := env.strat(other.ID); o.Cash != 5000 || len(esTrades(t, env, other.ID)) != 0 {
				t.Fatalf("the other strategy changed: cash=%v", o.Cash)
			}
			if tc.named {
				var n int
				if err := env.fileFor(other.ID).db.QueryRow(`SELECT COUNT(*) FROM trades WHERE strategy_id IN (?, ?)`, sc.ID, sc.StorageStrategyID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("the default file holds %d named-source rows (err %v)", n, err)
				}
				var stored string
				if err := env.fileFor(sc.ID).db.QueryRow(`SELECT DISTINCT strategy_id FROM trades`).Scan(&stored); err != nil || stored != sc.StorageStrategyID {
					t.Fatalf("named-source rows stored under %q (err %v), want %q", stored, err, sc.StorageStrategyID)
				}
			}
		})
	}
}

func TestOptionExpiryResultsReachRiskGatesIntegration(t *testing.T) {
	sc := ocSC("deribit-es-gates", "deribit", "BTC")
	streak := 2
	sc.CBLossStreakThreshold = &streak
	env := ocEnv(t, sc)
	elBindClock(t, esAt)
	s := env.strat(sc.ID)
	s.Cash = 100000
	s.InitialCapital = 100000
	esLeg(s, "BTC", "put", "sell", 60000, 1, 1, 500)
	esLeg(s, "BTC", "put", "sell", 61000, 1, 1, 500)
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 5000}
	part := partitionFor(sc)
	gate := func(env *entryTestEnv) DailyLossLimitStatus {
		return evaluateDailyLossLimit(pr, filterStatesByPartition(env.state.Strategies, env.cfg.Strategies, part), strategiesInPartition(env.cfg.Strategies, part), esAt)
	}
	if st := gate(env); st.Tripped {
		t.Fatalf("gate tripped before settlement: %+v", st)
	}
	env.saveAll()
	if res, _ := esRun(env, sc, esSettle("deribit", 54000)); res.Settled != 2 {
		t.Fatalf("settle: %+v", res)
	}
	if st := gate(env); !st.Tripped || !ocNear(st.LossUSD, 12000) {
		t.Fatalf("daily-loss gate after settlement: %+v", st)
	}
	env.saveAll()
	env = env.restart()
	s = env.strat(sc.ID)
	if st := gate(env); !st.Tripped || !ocNear(st.LossUSD, 12000) || s.RiskState.ConsecutiveLosses != 2 {
		t.Fatalf("after reload: gate=%+v streak=%d", st, s.RiskState.ConsecutiveLosses)
	}
	if ok, reason := CheckRisk(&sc, s, PortfolioValue(s, nil), nil, nil, nil); ok || !strings.Contains(reason, RiskReasonConsecutiveLosses) || !s.RiskState.CircuitBreaker {
		t.Fatalf("breaker: ok=%v reason=%q", ok, reason)
	}
}

func TestOptionLegacyCashPlatformLongWarningIntegration(t *testing.T) {
	b := ocSC("deribit-legacy-b", "deribit", "BTC")
	a := ocSC("deribit-legacy-a", "deribit", "BTC")
	okx := ocSC("okx-legacy", "okx", "ETH")
	short := ocSC("deribit-legacy-short", "deribit", "BTC")
	unrelated := ocSC("deribit-legacy-eth", "deribit", "BTC")
	rh := ocSC("rh-legacy", "robinhood", "SPY")
	env := ocEnv(t, b, a, okx, short, unrelated, rh)
	env.strat(b.ID).Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 0.5, AvgCost: 54000, OwnerStrategyID: b.ID}
	env.strat(a.ID).Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 1, AvgCost: 60000, OwnerStrategyID: a.ID}
	esLeg(env.strat(a.ID), "BTC", "call", "sell", 70000, 1, 1, 500)
	env.strat(a.ID).OptionPositions["BTC-call-sell-70000-"+esExpiry].Expiry = "2099-01-01"
	env.strat(okx.ID).Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 2, AvgCost: 3000, OwnerStrategyID: okx.ID}
	env.strat(short.ID).Positions["BTC"] = &Position{Symbol: "BTC", Side: "short", Quantity: 1, AvgCost: 60000, OwnerStrategyID: short.ID}
	env.strat(unrelated.ID).Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 3000, OwnerStrategyID: unrelated.ID}
	env.strat(rh.ID).Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, AvgCost: 90, OwnerStrategyID: rh.ID}
	env.saveAll()
	env = env.restart()
	out := captureORCStdout(t, func() { ValidateState(env.state, env.cfg.Strategies) })
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "settles expiry in cash but holds a legacy long") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 3 || !strings.Contains(warns[0], "strategy deribit-legacy-a on deribit") || !strings.Contains(warns[0], "BTC quantity=1 cost_basis=60000") ||
		!strings.Contains(warns[1], "strategy deribit-legacy-b on deribit") || !strings.Contains(warns[1], "quantity=0.5 cost_basis=54000") ||
		!strings.Contains(warns[2], "strategy okx-legacy on okx") || !strings.Contains(warns[2], "ETH quantity=2") {
		t.Fatalf("legacy-long warnings = %q", warns)
	}
	if p := env.strat(b.ID).Positions["BTC"]; p == nil || p.Quantity != 0.5 || p.AvgCost != 54000 || len(env.strat(b.ID).OptionPositions) != 0 {
		t.Fatalf("validation changed the legacy long: %+v", p)
	}
	env.saveAll()
	env = env.restart()
	if p := env.strat(b.ID).Positions["BTC"]; p == nil || p.Quantity != 0.5 || p.AvgCost != 54000 {
		t.Fatalf("reload changed the legacy long: %+v", p)
	}
}
