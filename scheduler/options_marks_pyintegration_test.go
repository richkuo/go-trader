//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const omFakeScript = `import json, os, sys
mode = os.environ["OM_MODE"]
req = json.loads(sys.stdin.read())
ids = [leg["id"] for leg in req["legs"]]
def doc(**kw):
    d = {"contract": "option_marks_v1", "platform": "robinhood", "premium_unit": "underlying_unit", "legs": []}
    d.update(kw)
    return d
def mark(leg_id):
    return {"id": leg_id, "status": "mark", "cutoff_utc": "2099-01-16T21:00:00Z", "settlement_ready_utc": "2099-01-16T22:00:00Z",
            "session_date": "2099-01-16", "mark": {"value_per_unit": 9.0, "spot": 450.0, "greeks": {"delta": 0.1, "gamma": 0.0, "theta": 0.0, "vega": 0.0}, "source": "fake"}}
good = [mark(i) for i in ids]
if mode == "good":
    print(json.dumps(doc(legs=good)))
elif mode == "bad_contract":
    print(json.dumps(dict(doc(legs=good), contract="option_marks_v0")))
elif mode == "omitted_leg":
    print(json.dumps(doc(legs=good[1:])))
`

func omSeed(s *StrategyState) {
	s.Cash = 100000
	s.OptionPositions["SPY-put-sell-450-2099-01-16"] = &OptionPosition{ID: "SPY-put-sell-450-2099-01-16", Underlying: "SPY", OptionType: "put", Strike: 450,
		Expiry: "2099-01-16", Action: "sell", Quantity: 2, ContractMultiplier: 100, EntryPremiumUSD: 600, CurrentValueUSD: -555,
		MarkUpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), MarkSource: "robinhood_quote",
		ExpiryCutoffAt: time.Date(2099, 1, 16, 21, 0, 0, 0, time.UTC), SettlementReadyAt: time.Date(2099, 1, 16, 22, 0, 0, 0, time.UTC)}
	s.OptionPositions["SPY-call-sell-500-2099-01-16"] = &OptionPosition{ID: "SPY-call-sell-500-2099-01-16", Underlying: "SPY", OptionType: "call", Strike: 500,
		Expiry: "2099-01-16", Action: "sell", Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 300, CurrentValueUSD: -222}
}

func TestRobinhoodOptionMarksPyIntegration(t *testing.T) {
	root := qpRoot(t)
	refreshAt := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	t.Run("real_script_marks_and_holds_through_the_real_pricer", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_QUOTE": "450", "QP_CLOSE": "450", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily)})
		sc := ocSC("rh-om-real", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 100000
		s.OptionPositions["live"] = &OptionPosition{ID: "live", TradePositionID: "p-live", Underlying: "SPY", OptionType: "put", Strike: 440,
			Expiry: "2026-06-18", Action: "sell", Quantity: 2, ContractMultiplier: 100, EntryPremiumUSD: 600, CurrentValueUSD: -600}
		s.OptionPositions["expired"] = &OptionPosition{ID: "expired", TradePositionID: "p-exp", Underlying: "SPY", OptionType: "put", Strike: 470,
			Expiry: "2026-05-15", Action: "sell", Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 300, CurrentValueUSD: -300}
		res := lcRefresh(env, sc, NewRobinhoodOptionPricer(), nil, newOptionSettlementAlertThrottle(), refreshAt)
		live, expired := s.OptionPositions["live"], s.OptionPositions["expired"]
		if res.Marked != 1 || live == nil || live.MarkSource != "robinhood_black_scholes" || live.MarkError != "" || live.CurrentValueUSD >= 0 {
			t.Fatalf("live mark: res=%+v leg=%+v", res, live)
		}
		if want := time.Date(2026, 6, 18, 20, 0, 0, 0, time.UTC); !live.ExpiryCutoffAt.Equal(want) || !ocNear(live.DTE, want.Sub(refreshAt).Hours()/24) {
			t.Fatalf("live cutoff=%s dte=%v, want the XNYS close %s", live.ExpiryCutoffAt, live.DTE, want)
		}
		if expired == nil || !strings.HasPrefix(expired.MarkError, "settlement pending: no daily bar for SPY on expected session 2026-05-15") || len(res.Alerts) != 1 {
			t.Fatalf("expired leg must hold with an alert: res=%+v leg=%+v", res, expired)
		}
		if s.Cash != 100000 || len(s.ClosedOptionPositions) != 0 {
			t.Fatalf("a pending settlement changed the book: cash=%v closed=%v", s.Cash, s.ClosedOptionPositions)
		}

		bars, _ := json.Marshal([]map[string]interface{}{{"date": "2026-05-14", "close": 455.0}, {"date": "2026-05-15", "close": 460.0}, {"date": "2026-05-18", "close": 461.0}})
		t.Setenv("QP_DAILY_BARS", string(bars))
		res = lcRefresh(env, sc, NewRobinhoodOptionPricer(), nil, newOptionSettlementAlertThrottle(), refreshAt.Add(time.Hour))
		if res.Settled != 1 || s.OptionPositions["expired"] != nil || !ocNear(s.Cash, 100000-470*100) || s.Positions["SPY"] == nil || s.Positions["SPY"].Quantity != 100 {
			t.Fatalf("settle through the real script: res=%+v cash=%v positions=%+v", res, s.Cash, s.Positions)
		}
	})

	t.Run("reply_contract_failures", func(t *testing.T) {
		script := filepath.Join(t.TempDir(), "fake_option_marks.py")
		if err := os.WriteFile(script, []byte(omFakeScript), 0o644); err != nil {
			t.Fatal(err)
		}
		const whole, omitted = "whole", "omitted"
		cases := []struct {
			mode, scope, reason string
		}{
			{"bad_contract", whole, "contract is not option_marks_v1"},
			{"omitted_leg", omitted, "robinhood option marks reply omitted this leg"},
		}
		for _, tc := range cases {
			t.Run(tc.mode, func(t *testing.T) {
				t.Setenv("OM_MODE", tc.mode)
				pricer := &RobinhoodOptionPricer{script: script, run: func(script string, args []string, stdin []byte) ([]byte, []byte, error) {
					return runPythonWithTimeout(shutdownReadOnlyCtx, script, args, stdin, 2*time.Second)
				}}
				s := &StrategyState{ID: "rh-om-fake", Platform: "robinhood", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
				omSeed(s)
				reqs := collectOptionLegRequests(s, "robinhood", refreshAt)
				logger, _ := ocLogger(s.ID)
				res := applyOptionLegReplies(s, "robinhood", reqs, refreshOptionLegs(pricer, reqs, refreshAt), refreshAt, logger)
				put, call := s.OptionPositions["SPY-put-sell-450-2099-01-16"], s.OptionPositions["SPY-call-sell-500-2099-01-16"]
				failedPut := tc.scope == whole
				if failedPut {
					if put.CurrentValueUSD != -555 || !strings.Contains(put.MarkError, tc.reason) || !put.markVerifiedAt.IsZero() {
						t.Fatalf("put must keep its last value and fail with %q: %+v", tc.reason, put)
					}
					if want := time.Date(2099, 1, 16, 21, 0, 0, 0, time.UTC).Sub(refreshAt).Hours() / 24; !ocNear(put.DTE, want) {
						t.Fatalf("put DTE must advance from its stored cutoff: %v want %v", put.DTE, want)
					}
					if call.CurrentValueUSD != -222 || !strings.Contains(call.MarkError, tc.reason) {
						t.Fatalf("call must fail with the batch: %+v", call)
					}
					if len(res.Failures) != 2 {
						t.Fatalf("failures = %v", res.Failures)
					}
					return
				}
				if call.CurrentValueUSD != -222 || call.MarkError != optionCutoffUnresolvedPrefix+tc.reason || !call.markVerifiedAt.IsZero() {
					t.Fatalf("the omitted call must fail alone: %+v", call)
				}
				if put.CurrentValueUSD != -1800 || put.MarkError != "" || put.markVerifiedAt.IsZero() {
					t.Fatalf("the returned put must mark: %+v", put)
				}
			})
		}
	})

	t.Run("startup_probe", func(t *testing.T) {
		robinhoodStdoutOfflineEnv(t, root, "routes")
		routes := filepath.Join(t.TempDir(), "routes.json")
		if err := os.WriteFile(routes, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		requests := filepath.Join(t.TempDir(), "requests.jsonl")
		t.Setenv("RH_OFFLINE_STUB_ROUTES", routes)
		t.Setenv("RH_OFFLINE_STUB_LOG", requests)
		cfg := &Config{Strategies: []StrategyConfig{ocSC("rh-om-probe", "robinhood", "SPY")}}
		if err := probeCheckScripts(cfg); err != nil {
			t.Fatalf("probe with the real mark script: %v", err)
		}
		if raw, err := os.ReadFile(requests); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			t.Fatalf("--probe-only made network requests:\n%s", raw)
		}
		deribitOnly := &Config{Strategies: []StrategyConfig{ocSC("deribit-om-probe", "deribit", "BTC")}}
		if anyRobinhoodOptions(deribitOnly) || !anyRobinhoodOptions(cfg) {
			t.Fatal("the mark probe must run only for robinhood options")
		}
	})
}

const ibFakeScript = `import json, os, sys
mode = os.environ["OM_MODE"]
req = json.loads(sys.stdin.read())
def doc(**kw):
    d = {"contract": "option_marks_v1", "platform": "ibkr", "premium_unit": "underlying_unit", "legs": []}
    d.update(kw)
    return d
def base(leg):
    return {"id": leg["id"], "cutoff_utc": leg["cutoff_utc"], "settlement_ready_utc": leg["settlement_ready_utc"]}
def mark(leg, source="binanceus_black_scholes_hv14"):
    out = base(leg)
    out.update(status="mark", mark={"value_per_unit": 900.0, "spot": 60000.0, "greeks": {"delta": -0.3, "gamma": 0.0, "theta": -5.0, "vega": 1.0}, "source": source})
    return out
def settled(leg):
    out = base(leg)
    out.update(status="settled", settlement={"price": 58000.0, "basis": "spot_candle_close_vwap", "date": leg["expiry"], "source": "binanceus_spot_vwap_1530_1600_london"})
    return out
legs = req["legs"]
if mode == "good":
    print(json.dumps(doc(legs=[mark(l) for l in legs])))
elif mode == "wrong_source":
    print(json.dumps(doc(legs=[mark(legs[0], "fixed_vol_model")] + [mark(l) for l in legs[1:]])))
elif mode == "omitted_leg":
    print(json.dumps(doc(legs=[mark(l) for l in legs[1:]])))
elif mode == "early_settle":
    print(json.dumps(doc(legs=[settled(l) for l in legs])))
`

func ibPyLeg(id, optionType, action, expiry string, strike, mult, value float64) *OptionPosition {
	return &OptionPosition{ID: id, TradePositionID: "tp-" + id, Underlying: "BTC", OptionType: optionType, Strike: strike, Expiry: expiry,
		Action: action, Quantity: 1, ContractMultiplier: mult, EntryPremiumUSD: math.Abs(value), CurrentValueUSD: value,
		OpenedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func ibMinuteWindow(expiry string, close float64) []interface{} {
	cutoff, _, _, _ := optionGoExpirySchedule("ibkr", expiry)
	start := cutoff.Add(-30 * time.Minute).UnixMilli()
	rows := make([]interface{}, 0, 30)
	for i := int64(0); i < 30; i++ {
		rows = append(rows, []interface{}{start + i*60000, close, close, close, close, 1.0 + float64(i%3)})
	}
	return rows
}

func ibMinuteReads(t *testing.T, logPath string) []int64 {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []int64
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Call      string `json:"call"`
			Timeframe string `json:"timeframe"`
			Since     *int64 `json:"since"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("request log line %q: %v", line, err)
		}
		if rec.Call == "fetch_ohlcv" && rec.Timeframe == "1m" && rec.Since != nil {
			out = append(out, *rec.Since)
		}
	}
	return out
}

func TestIBKROptionMarksPyIntegration(t *testing.T) {
	root := qpRoot(t)

	t.Run("real_script_marks_with_measured_volatility", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_QUOTE": "60000", "QP_CLOSE": "60000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily)})
		sc := ocSC("ib-om-real", "ibkr", "BTC")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 100000
		s.OptionPositions["live"] = ibPyLeg("live", "put", "sell", "2026-11-27", 60000, 0.1, -123)
		refreshAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
		res := lcRefresh(env, sc, NewIBKROptionPricer(), nil, newOptionSettlementAlertThrottle(), refreshAt)
		leg := s.OptionPositions["live"]
		cutoff := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
		if res.Marked != 1 || leg.MarkSource != ibkrMarkSource || leg.MarkError != "" || !leg.MarkUpdatedAt.Equal(refreshAt) || leg.CurrentValueUSD >= 0 || leg.CurrentValueUSD == -123 {
			t.Fatalf("mark: res=%+v leg=%+v", res, leg)
		}
		if !leg.ExpiryCutoffAt.Equal(cutoff) || !ocNear(leg.DTE, cutoff.Sub(refreshAt).Hours()/24) {
			t.Fatalf("cutoff=%s dte=%v", leg.ExpiryCutoffAt, leg.DTE)
		}
		marked := leg.CurrentValueUSD
		t.Setenv("QP_DAILY", "error")
		later := refreshAt.Add(time.Hour)
		res = lcRefresh(env, sc, NewIBKROptionPricer(), nil, newOptionSettlementAlertThrottle(), later)
		if res.Marked != 0 || len(res.Failures) != 1 || leg.CurrentValueUSD != marked || !strings.Contains(leg.MarkError, "volatility unavailable: model pricing needs measured volatility") || !ocNear(leg.DTE, cutoff.Sub(later).Hours()/24) {
			t.Fatalf("missing history: res=%+v leg=%+v", res, leg)
		}
	})

	t.Run("real_script_settles_the_dated_window_once", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "qp.log")
		bars, _ := json.Marshal(map[string]interface{}{"BTC/USDT": append(ibMinuteWindow("2026-11-13", 62000), ibMinuteWindow("2026-11-20", 58000)...)})
		qpEnv(t, root, map[string]string{"QP_QUOTE": "60000", "QP_CLOSE": "60000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily),
			"QP_MINUTE_BARS": string(bars), "QP_LOG": logPath})
		sc := ocSC("ib-om-settle", "ibkr", "BTC")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 100000
		s.OptionPositions["sold-put"] = ibPyLeg("sold-put", "put", "sell", "2026-11-20", 60000, 0.1, -50)
		s.OptionPositions["bought-call"] = ibPyLeg("bought-call", "call", "buy", "2026-11-13", 60000, 0.1, 40)
		env.saveAll()
		waiting := time.Date(2026, 11, 20, 16, 2, 0, 0, time.UTC)
		res := lcRefresh(env, sc, NewIBKROptionPricer(), nil, newOptionSettlementAlertThrottle(), waiting)
		if res.Settled != 1 || res.Waiting != 1 || len(res.Alerts) != 0 || s.OptionPositions["bought-call"] != nil || s.OptionPositions["sold-put"] == nil || !ocNear(s.Cash, 100200) {
			t.Fatalf("first refresh: res=%+v cash=%v legs=%v", res, s.Cash, ocBookSnapshot(s))
		}
		start13 := time.Date(2026, 11, 13, 15, 30, 0, 0, time.UTC).UnixMilli()
		if reads := ibMinuteReads(t, logPath); len(reads) != 1 || reads[0] != start13 {
			t.Fatalf("minute reads before the 11-20 read boundary = %v, want only the 11-13 window %d", reads, start13)
		}
		ready := time.Date(2026, 11, 20, 16, 5, 0, 0, time.UTC)
		res = lcRefresh(env, sc, NewIBKROptionPricer(), nil, newOptionSettlementAlertThrottle(), ready)
		if res.Settled != 1 || len(s.OptionPositions) != 0 || len(s.Positions) != 0 || !ocNear(s.Cash, 100000) {
			t.Fatalf("second refresh: res=%+v cash=%v positions=%v", res, s.Cash, s.Positions)
		}
		env.saveAll()
		env = env.restart()
		res = lcRefresh(env, sc, NewIBKROptionPricer(), nil, newOptionSettlementAlertThrottle(), ready.Add(time.Hour))
		env.saveAll()
		if _, closed, legs := lcRows(t, env, sc.ID); res.Legs != 0 || closed != 2 || legs != 0 || !ocNear(env.strat(sc.ID).Cash, 100000) {
			t.Fatalf("restart: res=%+v closed=%d legs=%d cash=%v", res, closed, legs, env.strat(sc.ID).Cash)
		}
		trades := esTrades(t, env, sc.ID)
		if len(trades) != 2 || !strings.Contains(trades[0].details+trades[1].details, "binanceus_spot_vwap_1530_1600_london spot_candle_close_vwap 2026-11-20") {
			t.Fatalf("trades = %+v", trades)
		}
	})

	t.Run("reply_contract_failures", func(t *testing.T) {
		script := filepath.Join(t.TempDir(), "fake_ibkr_marks.py")
		if err := os.WriteFile(script, []byte(ibFakeScript), 0o644); err != nil {
			t.Fatal(err)
		}
		markAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
		waitAt := time.Date(2026, 11, 20, 16, 1, 0, 0, time.UTC)
		const first, omitted, early = "first", "omitted", "early"
		for _, tc := range []struct {
			mode, scope, reason string
		}{
			{"wrong_source", first, `ibkr mark source "fixed_vol_model" is not binanceus_black_scholes_hv14`},
			{"omitted_leg", omitted, "ibkr option marks reply omitted this leg"},
			{"early_settle", early, "settlement before the read boundary"},
		} {
			t.Run(tc.mode, func(t *testing.T) {
				t.Setenv("OM_MODE", tc.mode)
				pricer := &IBKROptionPricer{script: script, run: func(script string, args []string, stdin []byte) ([]byte, []byte, error) {
					return runPythonWithTimeout(shutdownReadOnlyCtx, script, args, stdin, 5*time.Second)
				}}
				s := &StrategyState{ID: "ib-om-fake", Platform: "ibkr", Cash: 100000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
				s.OptionPositions["a-put"] = ibPyLeg("a-put", "put", "sell", "2026-11-20", 60000, 0.1, -55)
				s.OptionPositions["b-call"] = ibPyLeg("b-call", "call", "sell", "2026-11-20", 65000, 0.1, -22)
				at := markAt
				if tc.scope == early {
					at = waitAt
				}
				reqs := collectOptionLegRequests(s, "ibkr", at)
				logger, _ := ocLogger(s.ID)
				res := applyOptionLegReplies(s, "ibkr", reqs, refreshOptionLegs(pricer, reqs, at), at, logger)
				put, call := s.OptionPositions["a-put"], s.OptionPositions["b-call"]
				if put == nil || call == nil || s.Cash != 100000 {
					t.Fatalf("an invalid reply changed the book: cash=%v legs=%v", s.Cash, ocBookSnapshot(s))
				}
				cutoff := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
				if !put.ExpiryCutoffAt.Equal(cutoff) || !call.ExpiryCutoffAt.Equal(cutoff) {
					t.Fatalf("schedule must stay the Go cutoff: %s %s", put.ExpiryCutoffAt, call.ExpiryCutoffAt)
				}
				switch tc.scope {
				case first, omitted:
					failed, ok := put, call
					if !strings.Contains(failed.MarkError, tc.reason) || failed.CurrentValueUSD != -55 || ok.MarkError != "" || !ocNear(ok.CurrentValueUSD, -90) || len(res.Failures) != 1 {
						t.Fatalf("one leg: res=%+v failed=%+v ok=%+v", res, failed, ok)
					}
				case early:
					if res.Waiting != 2 || res.Settled != 0 || len(res.Alerts) != 0 || !strings.Contains(put.MarkError, tc.reason) {
						t.Fatalf("early settlement: res=%+v put=%+v", res, put)
					}
				}
			})
		}
	})

	t.Run("options_only_scheduler_once", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "state.db")
		sc := StrategyConfig{ID: "ib-once", Type: "options", Platform: "ibkr", Script: "shared_scripts/check_options.py",
			Args: []string{"wheel", "BTC", "--platform=ibkr", "--mode=paper"}, Capital: 100000, MaxDrawdownPct: 50}
		cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
		if anyRobinhoodOptions(cfg) || len(ibkrOptionsCheckScripts(cfg)) != 1 {
			t.Fatal("an ibkr-only config must run the ibkr probes and no robinhood probe")
		}
		store := openSplitStore(t, cfg)
		state := NewAppState()
		expiry := time.Now().UTC().AddDate(0, 0, 20).Format("2006-01-02")
		state.Strategies[sc.ID] = &StrategyState{ID: sc.ID, Type: "options", Platform: "ibkr", Cash: 100000, InitialCapital: 100000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{
				"legacy-call": {ID: "legacy-call", TradePositionID: "tp-legacy-call", Underlying: "BTC", OptionType: "call", Strike: 65000, Expiry: expiry,
					Action: "buy", Quantity: 1, EntryPremiumUSD: 700, CurrentValueUSD: 700, DTE: 30, OpenedAt: time.Now().UTC().Add(-48 * time.Hour)},
			}}
		if err := SaveStateWithStore(state, store); err != nil {
			t.Fatal(err)
		}
		store.Close()
		blob, err := json.MarshalIndent(map[string]any{
			"config_version": CurrentConfigVersion, "interval_seconds": 3600, "db_file": dbPath, "log_dir": filepath.Join(dir, "logs"),
			"strategies": []StrategyConfig{sc},
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(dir, "qp.log")
		cmd := exec.Command(pyintegrationBinary(t), "--config", cfgPath, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
		cmd.Dir = root
		cmd.Env = append(qpChildEnv(t, root, map[string]string{"QP_QUOTE": "60000", "QP_CLOSE": "60000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily), "QP_LOG": logPath}),
			"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
		started := time.Now().UTC()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("scheduler --once: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "All prices are zero/missing") || strings.Contains(string(out), "network blocked") {
			t.Fatalf("options-only cycle skipped or reached the network:\n%s", out)
		}
		if reads := ibMinuteReads(t, logPath); len(reads) != 0 {
			t.Fatalf("the startup probe or a cycle with no leg near expiry read 1m settlement data: %v", reads)
		}
		db, err := OpenStateDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.db.Query(`SELECT id, expiry, current_value_usd, dte, contract_multiplier, mark_source, mark_error, mark_updated_at, expiry_cutoff_at FROM option_positions WHERE strategy_id = ? ORDER BY id`, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type row struct {
			id, expiry, source, markErr, markedAt, cutoffAt string
			value, dte, mult                                float64
		}
		got := map[string]row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.expiry, &r.value, &r.dte, &r.mult, &r.source, &r.markErr, &r.markedAt, &r.cutoffAt); err != nil {
				t.Fatal(err)
			}
			got[r.id] = r
		}
		legacy, ok := got["legacy-call"]
		if !ok || legacy.mult != 1 || legacy.source != ibkrMarkSource || legacy.markErr != "" {
			t.Fatalf("legacy leg row = %+v; output:\n%s", legacy, out)
		}
		markedAt, err := time.Parse(time.RFC3339Nano, legacy.markedAt)
		if err != nil || markedAt.Before(started) {
			t.Fatalf("mark timestamp %q is not from this run (started %s)", legacy.markedAt, started)
		}
		cutoff, _, _, _ := optionGoExpirySchedule("ibkr", expiry)
		if legacy.cutoffAt != formatTime(cutoff) || !ocNear(legacy.dte, cutoff.Sub(markedAt).Hours()/24) || legacy.dte >= 30 {
			t.Fatalf("legacy leg cutoff=%s dte=%v, want %s", legacy.cutoffAt, legacy.dte, cutoff)
		}
		dte := cutoff.Sub(markedAt).Seconds() / 86400
		expect := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-c", `import json, sys
sys.path.insert(0, "shared_tools")
import option_data
from pricing import bs_price_and_greeks
vol, _ = option_data.realized_vol_metrics(json.loads(sys.argv[1]), 365)
print(json.dumps(bs_price_and_greeks(60000.0, 65000.0, float(sys.argv[2]), vol, option_type="call")[0]))`, qpOptionsDaily(qpOptionsBaselineDaily), strconv.FormatFloat(dte, 'g', -1, 64))
		expect.Dir = root
		raw, err := expect.Output()
		if err != nil {
			t.Fatal(err)
		}
		var perUnit float64
		if err := json.Unmarshal(raw, &perUnit); err != nil {
			t.Fatal(err)
		}
		if want := perUnit * 1; math.Abs(legacy.value-want) > 1e-6*math.Abs(want) {
			t.Fatalf("legacy leg value = %v, want %v from the measured-volatility model at size 1", legacy.value, want)
		}
		entries := 0
		for id, r := range got {
			if id == "legacy-call" {
				continue
			}
			entries++
			day, err := time.Parse("2006-01-02", r.expiry)
			if err != nil || r.mult != 0.1 || day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
				t.Fatalf("new entry %s = %+v, want size 0.1 on a weekday", id, r)
			}
		}
		if entries != 1 {
			t.Fatalf("new wheel entries = %d, want 1; rows=%+v; output:\n%s", entries, got, out)
		}
	})
}
