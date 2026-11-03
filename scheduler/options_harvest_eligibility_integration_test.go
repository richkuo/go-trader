package main

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var heT0 = time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)

type heLeg struct {
	key     string
	value   float64
	cutoff  time.Time
	closeBy string
}

func heLegs() []heLeg {
	return []heLeg{
		{key: "SPY-put-sell-400-2026-03-20", value: 30, cutoff: time.Date(2026, 3, 20, 20, 0, 0, 0, time.UTC), closeBy: "Theta harvest:"},
		{key: "SPY-put-sell-410-2026-03-20", value: 350, cutoff: time.Date(2026, 3, 20, 20, 0, 0, 0, time.UTC), closeBy: "Stop loss:"},
		{key: "SPY-put-sell-420-2026-03-04", value: 90, cutoff: time.Date(2026, 3, 4, 21, 0, 0, 0, time.UTC), closeBy: "DTE exit:"},
	}
}

func heSC(id string) StrategyConfig {
	sc := ocSC(id, "robinhood", "SPY")
	sc.ThetaHarvest = &ThetaHarvestConfig{Enabled: true, ProfitTargetPct: 60, StopLossPct: 200, MinDTEClose: 3}
	return sc
}

func heSeed(s *StrategyState) {
	s.Cash = 100000
	for i, leg := range heLegs() {
		s.OptionPositions[leg.key] = &OptionPosition{ID: leg.key, TradePositionID: "pos-" + leg.key, Underlying: "SPY", OptionType: "put",
			Strike: float64(400 + 10*i), Expiry: leg.cutoff.Format("2006-01-02"), DTE: leg.cutoff.Sub(heT0).Hours() / 24, Action: "sell",
			Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 100, CurrentValueUSD: -100, OpenedAt: heT0.Add(-time.Hour)}
	}
}

func heValuePricer() lcPricer {
	byKey := map[string]heLeg{}
	for _, leg := range heLegs() {
		byKey[leg.key] = leg
	}
	return lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
		leg := byKey[req.Key]
		return optionLegReply{CutoffAt: leg.cutoff, ReadyAt: leg.cutoff.Add(time.Hour), Mark: &optionLegMark{PerUnitUSD: leg.value / 100, Spot: 450, Source: "harvest-test"}}
	}}
}

func heFailingPricer() lcPricer {
	return lcPricer{reply: func(req optionLegRequest, at time.Time) optionLegReply {
		return optionLegReply{Err: "option quote unavailable: robinhood option market data missing (upstream status 503)"}
	}}
}

func heMark(t *testing.T, env *entryTestEnv, sc StrategyConfig, pricer OptionPricer, at time.Time) optionLegApplyResult {
	t.Helper()
	return lcRefresh(env, sc, pricer, nil, nil, at)
}

func heHarvestLocked(env *entryTestEnv, sc StrategyConfig, result *OptionsResult, at time.Time) (int, []string) {
	env.mu.Lock()
	defer env.mu.Unlock()
	logger, _ := ocLogger(sc.ID)
	return executeOptionsCycleLocked(sc, env.strat(sc.ID), result, "", newOptionHarvestClock(sc, env.cfg.IntervalSeconds, at), logger)
}

func heDispatch(env *entryTestEnv, sc StrategyConfig, posErr error, at time.Time) (int, []string) {
	logger, _ := ocLogger(sc.ID)
	return runOptionsDispatch(optionsDispatch{SC: sc, GlobalIntervalSeconds: env.cfg.IntervalSeconds, Now: at, State: env.strat(sc.ID),
		PositionsErr: posErr, Risk: &scopeCycleRisk{Partition: partitionFor(sc)}, Scope: partitionFor(sc).Scope, Mu: &env.mu, Logger: logger})
}

func heAssertHarvested(t *testing.T, env *entryTestEnv, sc StrategyConfig, trades int, lines []string) {
	t.Helper()
	if trades != 3 || len(lines) != 3 {
		t.Fatalf("trades=%d lines=%q, want three harvest closes", trades, lines)
	}
	for _, leg := range heLegs() {
		n := 0
		for _, line := range lines {
			if strings.Contains(line, "CLOSE "+leg.key+" ") {
				n++
				if !strings.Contains(line, leg.closeBy) {
					t.Fatalf("%s closed by %q, want %q", leg.key, line, leg.closeBy)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s appears %d times in %q", leg.key, n, lines)
		}
	}
	if s := env.strat(sc.ID); len(s.OptionPositions) != 0 || !ocNear(s.Cash, 100000-30-350-90) {
		t.Fatalf("legs=%d cash=%v after harvest", len(s.OptionPositions), s.Cash)
	}
}

func heAssertHeld(t *testing.T, env *entryTestEnv, sc StrategyConfig, trades int, lines []string) {
	t.Helper()
	if trades != 0 || len(lines) != 0 || len(env.strat(sc.ID).OptionPositions) != 3 || env.strat(sc.ID).Cash != 100000 {
		t.Fatalf("ineligible legs must hold: trades=%d lines=%q legs=%d cash=%v", trades, lines, len(env.strat(sc.ID).OptionPositions), env.strat(sc.ID).Cash)
	}
}

func TestOptionHarvestEligibilityIntegration(t *testing.T) {
	newEnv := func(t *testing.T, sc StrategyConfig) *entryTestEnv {
		env := ocEnv(t, sc)
		heSeed(env.strat(sc.ID))
		return env
	}
	t.Run("holds", func(t *testing.T) {
		cases := []struct {
			name string
			prep func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time)
		}{
			{"unknown_mark", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				return env, sc, heT0
			}},
			{"failed_last_refresh", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				heMark(t, env, sc, heValuePricer(), heT0)
				if res := heMark(t, env, sc, heFailingPricer(), heT0.Add(5*time.Minute)); len(res.Failures) != 3 {
					t.Fatalf("failing refresh = %+v", res)
				}
				return env, sc, heT0.Add(10 * time.Minute)
			}},
			{"restart_after_a_good_mark", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				heMark(t, env, sc, heValuePricer(), heT0)
				env.saveAll()
				env = env.restart()
				useEntryTradeRecorder(t, env.store)
				leg := env.strat(sc.ID).OptionPositions[heLegs()[0].key]
				if leg.MarkUpdatedAt.IsZero() || !leg.markVerifiedAt.IsZero() {
					t.Fatalf("restart must keep the display timestamp and drop eligibility: %+v", leg)
				}
				return env, sc, heT0.Add(time.Minute)
			}},
			{"mark_older_than_one_and_a_half_intervals", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				heMark(t, env, sc, heValuePricer(), heT0)
				return env, sc, heT0.Add(901 * time.Second)
			}},
			{"skipped_refresh_cycle", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				heMark(t, env, sc, heValuePricer(), heT0)
				return env, sc, heT0.Add(2 * 600 * time.Second)
			}},
			{"shorter_interval_after_reload", func(t *testing.T, env *entryTestEnv, sc StrategyConfig) (*entryTestEnv, StrategyConfig, time.Time) {
				heMark(t, env, sc, heValuePricer(), heT0)
				sc.IntervalSeconds = 200
				return env, sc, heT0.Add(400 * time.Second)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sc := heSC("rh-harvest-hold")
				env, sc, at := tc.prep(t, newEnv(t, sc), sc)
				trades, lines := heHarvestLocked(env, sc, nil, at)
				heAssertHeld(t, env, sc, trades, lines)
			})
		}
		t.Run("entry_only_value_and_entry_dte", func(t *testing.T) {
			sc := heSC("rh-harvest-entry")
			env := ocEnv(t, sc)
			s := env.strat(sc.ID)
			s.Cash = 100000
			n, err := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 450, OptionsAction{Action: "sell", OptionType: "put", Strike: 430, Expiry: "2026-03-04", DTE: 2, PremiumUSD: 1}), silentStrategyLogger(sc.ID))
			if err != nil || n != 1 {
				t.Fatalf("entry trades=%d err=%v", n, err)
			}
			cash := s.Cash
			trades, lines := heHarvestLocked(env, sc, nil, heT0)
			if trades != 0 || len(lines) != 0 || len(s.OptionPositions) != 1 || s.Cash != cash {
				t.Fatalf("an entry-only value with entry DTE 2 must not harvest: trades=%d lines=%q", trades, lines)
			}
		})
		t.Run("at_the_expiry_cutoff", func(t *testing.T) {
			sc := heSC("rh-harvest-cutoff")
			env := newEnv(t, sc)
			heMark(t, env, sc, heValuePricer(), time.Date(2026, 3, 4, 20, 55, 0, 0, time.UTC))
			trades, lines := heHarvestLocked(env, sc, nil, time.Date(2026, 3, 4, 21, 0, 0, 0, time.UTC))
			for _, line := range lines {
				if strings.Contains(line, heLegs()[2].key) {
					t.Fatalf("the leg at its cutoff harvested: %q", lines)
				}
			}
			if trades != 2 {
				t.Fatalf("the two legs before their cutoff must still harvest: trades=%d lines=%q", trades, lines)
			}
		})
	})

	t.Run("fires_once_after_a_successful_prior_refresh", func(t *testing.T) {
		at := heT0.Add(899 * time.Second)
		cases := []struct {
			name string
			run  func(env *entryTestEnv, sc StrategyConfig) (int, []string)
		}{
			{"entry_check_succeeded", func(env *entryTestEnv, sc StrategyConfig) (int, []string) {
				return heHarvestLocked(env, sc, ocResult("robinhood", "SPY", 450), at)
			}},
			{"entry_check_failed", func(env *entryTestEnv, sc StrategyConfig) (int, []string) {
				return heHarvestLocked(env, sc, nil, at)
			}},
			{"entry_check_not_spawned", func(env *entryTestEnv, sc StrategyConfig) (int, []string) {
				return heDispatch(env, sc, errors.New("options check skipped for the test"), at)
			}},
			{"positions_encoding_failed", func(env *entryTestEnv, sc StrategyConfig) (int, []string) {
				s := env.strat(sc.ID)
				s.OptionPositions[heLegs()[1].key].Greeks.Delta = math.Inf(1)
				_, err := EncodeAllPositionsJSON(s.Platform, s.OptionPositions, s.Positions)
				if err == nil {
					panic("encoding must fail with an infinite delta")
				}
				return heDispatch(env, sc, err, at)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sc := heSC("rh-harvest-fire")
				env := newEnv(t, sc)
				if res := heMark(t, env, sc, heValuePricer(), heT0); res.Marked != 3 {
					t.Fatalf("mark refresh = %+v", res)
				}
				trades, lines := tc.run(env, sc)
				heAssertHarvested(t, env, sc, trades, lines)
				env.saveAll()
				if n := lcCount(t, env, sc.ID, `SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'theta_harvest'`); n != 3 {
					t.Fatalf("closed theta-harvest rows = %d, want 3", n)
				}
			})
		}
	})
}
