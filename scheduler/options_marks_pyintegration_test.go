//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const omFakeScript = `import json, os, sys, time
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
elif mode == "bad_unit":
    print(json.dumps(dict(doc(legs=good), premium_unit="contract")))
elif mode == "bad_platform":
    print(json.dumps(dict(doc(legs=good), platform="deribit")))
elif mode == "duplicate_id":
    print(json.dumps(doc(legs=good + [mark(ids[0])])))
elif mode == "unknown_id":
    print(json.dumps(doc(legs=good + [mark("ghost")])))
elif mode == "omitted_leg":
    print(json.dumps(doc(legs=good[1:])))
elif mode == "extra_document":
    print(json.dumps(doc(legs=good)))
    print(json.dumps(doc(legs=good)))
elif mode == "string_number":
    bad = mark(ids[0]); bad["mark"]["value_per_unit"] = "9.0"
    print(json.dumps(doc(legs=[bad] + good[1:])))
elif mode == "exit_with_error_doc":
    print(json.dumps(doc(error="robinhood adapter unavailable: RuntimeError")))
    sys.exit(1)
elif mode == "exit_without_doc":
    sys.exit(1)
elif mode == "timeout":
    time.sleep(10)
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
			{"bad_unit", whole, "premium_unit is not underlying_unit"},
			{"bad_platform", whole, "platform is not robinhood"},
			{"duplicate_id", whole, "duplicate leg id"},
			{"unknown_id", whole, `unknown leg id "ghost"`},
			{"extra_document", whole, "stdout holds more than one JSON document"},
			{"string_number", whole, "invalid JSON document"},
			{"exit_with_error_doc", whole, "script error: robinhood adapter unavailable: RuntimeError"},
			{"exit_without_doc", whole, "script failed with no output"},
			{"timeout", whole, "script timed out after 2s"},
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
		if err := probeOneScriptContract("shared_scripts/missing_option_marks.py", robinhoodOptionMarksProbeArgv, optionMarksContract); err == nil || !strings.Contains(err.Error(), "missing from deploy tree") {
			t.Fatalf("missing script probe = %v", err)
		}
		other := filepath.Join(t.TempDir(), "other_contract.py")
		if err := os.WriteFile(other, []byte(`import json; print(json.dumps({"contract": "option_marks_v0", "probe": True}))`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := probeOneScriptContract(other, robinhoodOptionMarksProbeArgv, optionMarksContract); err == nil || !strings.Contains(err.Error(), `reports contract "option_marks_v0"`) {
			t.Fatalf("incompatible contract probe = %v", err)
		}
		shadow := t.TempDir()
		if err := os.WriteFile(filepath.Join(shadow, "exchange_calendars.py"), []byte(`raise ImportError("exchange_calendars is not installed")`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PYTHONPATH", shadow)
		if err := probeOneScriptContract(robinhoodOptionMarksScript, robinhoodOptionMarksProbeArgv, optionMarksContract); err == nil || !strings.Contains(err.Error(), "exchange_calendars is not installed") {
			t.Fatalf("a missing calendar dependency must fail the probe: %v", err)
		}
		deribitOnly := &Config{Strategies: []StrategyConfig{ocSC("deribit-om-probe", "deribit", "BTC")}}
		if anyRobinhoodOptions(deribitOnly) || !anyRobinhoodOptions(cfg) {
			t.Fatal("the mark probe must run only for robinhood options")
		}
	})
}
