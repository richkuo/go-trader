package main

import (
	"strings"
	"testing"
	"time"
)

func TestOptionMarkQuantityAndMultiplierParity(t *testing.T) {
	at := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		platform, underlying, action string
		perUnit, want                float64
	}{
		{"robinhood", "SPY", "buy", 3.5, 3.5 * 100 * 2},
		{"robinhood", "SPY", "sell", 3.5, -3.5 * 100 * 2},
		{"ibkr", "BTC", "buy", 1500, 1500 * 2},
		{"ibkr", "BTC", "sell", 1500, -1500 * 2},
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
}

func TestOptionMarkFixedClockCutoffs(t *testing.T) {
	winterClose := time.Date(2026, 1, 16, 21, 0, 0, 0, time.UTC)
	afterReady := winterClose.Add(90 * time.Minute)

	ibkrCutoff := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
	ibkrReady := ibkrCutoff.Add(5 * time.Minute)
	mkIBKR := func() *StrategyState {
		return &StrategyState{ID: "ibkr-clock", Platform: "ibkr", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
			"leg": {ID: "leg", TradePositionID: "p1", Underlying: "BTC", OptionType: "call", Strike: 70000, Expiry: "2026-06-26", Action: "sell", Quantity: 1, EntryPremiumUSD: 100, CurrentValueUSD: -100}}}
	}
	both := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
		cutoff, ready, _, _ := optionGoExpirySchedule("ibkr", req.Expiry)
		if at.Before(cutoff) {
			return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Mark: &optionLegMark{PerUnitUSD: 50, Spot: 60000, Source: ibkrMarkSource}}
		}
		return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Settlement: &optionLegSettlement{Price: 60000, Basis: optionSettlementBasisVWAP, Date: req.Expiry, Source: ibkrSettlementSource}}
	}}
	for _, tc := range []struct {
		name                     string
		at                       time.Time
		marked, waiting, settled bool
	}{
		{"one_nanosecond_before_cutoff_marks", ibkrCutoff.Add(-time.Nanosecond), true, false, false},
		{"cutoff_equality_stops_marking_and_waits", ibkrCutoff, false, true, false},
		{"read_boundary_equality_settles", ibkrReady, false, false, true},
	} {
		s := mkIBKR()
		reqs := collectOptionLegRequests(s, "ibkr", tc.at)
		logger, _ := ocLogger(s.ID)
		res := applyOptionLegReplies(s, "ibkr", reqs, refreshOptionLegs(both, reqs, tc.at), tc.at, logger)
		if (res.Marked == 1) != tc.marked || (res.Waiting == 1) != tc.waiting || (res.Settled == 1) != tc.settled {
			t.Fatalf("%s: res=%+v", tc.name, res)
		}
	}
	t.Run("cutoff_equality_settles_without_a_read_boundary", func(t *testing.T) {
		s := &StrategyState{ID: "rh-cutoff-equality", Platform: "robinhood", Cash: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
			"leg": {ID: "leg", TradePositionID: "p1", Underlying: "SPY", OptionType: "call", Strike: 700, Expiry: "2026-01-16", Action: "sell", Quantity: 1,
				ContractMultiplier: 100, EntryPremiumUSD: 100, CurrentValueUSD: -100, ExpiryCutoffAt: winterClose, SettlementReadyAt: winterClose.Add(time.Hour)}}}
		pricer := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			return optionLegReply{CutoffAt: winterClose, ReadyAt: winterClose.Add(time.Hour), Settlement: &optionLegSettlement{Price: 600, Basis: optionSettlementBasisRawClose, Date: "2026-01-16", Source: "clock"}}
		}}
		reqs := collectOptionLegRequests(s, "robinhood", winterClose)
		logger, _ := ocLogger(s.ID)
		if res := applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(pricer, reqs, winterClose), winterClose, logger); res.Settled != 1 || res.Marked != 0 || len(s.OptionPositions) != 0 || s.Cash != 1000 {
			t.Fatalf("cutoff equality: res=%+v legs=%d cash=%v", res, len(s.OptionPositions), s.Cash)
		}
	})

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
		s := mkIBKR()
		published := false
		pricer := lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
			cutoff, ready, _, _ := optionGoExpirySchedule("ibkr", req.Expiry)
			if !published {
				return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Pending: ibkrSettlementSource + " for BTC 2026-06-26 is not published yet"}
			}
			return optionLegReply{CutoffAt: cutoff, ReadyAt: ready, Settlement: &optionLegSettlement{Price: 60000, Basis: optionSettlementBasisVWAP, Date: req.Expiry, Source: ibkrSettlementSource}}
		}}
		logger, _ := ocLogger(s.ID)
		step := func(at time.Time) optionLegApplyResult {
			reqs := collectOptionLegRequests(s, "ibkr", at)
			return applyOptionLegReplies(s, "ibkr", reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
		}
		if res := step(ibkrCutoff.Add(2 * time.Minute)); res.Waiting != 1 || len(res.Alerts) != 0 || len(res.Failures) != 0 {
			t.Fatalf("before the read boundary: %+v", res)
		}
		if res := step(ibkrCutoff.Add(2 * time.Hour)); len(res.Alerts) != 1 || len(s.OptionPositions) != 1 {
			t.Fatalf("after the read boundary: %+v", res)
		}
		published = true
		if res := step(ibkrCutoff.Add(3 * time.Hour)); res.Settled != 1 || len(s.OptionPositions) != 0 || s.Cash != 1000 {
			t.Fatalf("late publication: %+v cash=%v", res, s.Cash)
		}
	})
}
