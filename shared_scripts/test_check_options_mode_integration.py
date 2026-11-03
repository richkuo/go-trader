import json
import math
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STUB_DIR = os.path.join(ROOT, "shared_scripts", "testdata", "quote_precision")
CHECK_OPTIONS = os.path.join(ROOT, "shared_scripts", "check_options.py")
CREDENTIALS = (
    "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX",
    "ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET",
)
FOUR_POSITIONS = json.dumps([
    {"option_type": "call", "strike": 500, "expiry": "2026-12-18"},
    {"option_type": "call", "strike": 510, "expiry": "2026-12-18"},
    {"option_type": "put", "strike": 450, "expiry": "2026-12-18"},
    {"option_type": "put", "strike": 440, "expiry": "2026-12-18"},
])
CAP_REASON = "Max positions reached (4/4)"
QP_KEYS = (
    "QP_CLOSE", "QP_QUOTE", "QP_SHAPE", "QP_NOW", "QP_TICKERS", "QP_CLOSES", "QP_CANDLES", "QP_OKX_FILL",
    "QP_DAILY", "QP_OPTION_QUOTE", "QP_OPTION_GREEKS", "QP_OPTION_QUOTE_FAIL_AFTER", "QP_OKX_OPTIONS",
    "QP_STDOUT_NOISE", "QP_ALL_CLOSED",
)


def _expected_error(platform):
    return (
        "options strategies run as paper only: no venue order path exists for options on "
        f"{platform}; remove the strategy, or replace --mode=live (or --mode live) with --mode=paper under a new id"
    )


def _run(argv, tmp_path, stdin, extra=None, script=None):
    env = dict(os.environ)
    for key in CREDENTIALS:
        env.pop(key, None)
    for key in QP_KEYS:
        env.pop(key, None)
    log_path = tmp_path / "qp.log"
    env["QP_STUB"] = "1"
    env["QP_LOG"] = str(log_path)
    env["PYTHONPATH"] = STUB_DIR
    env["HOME"] = str(tmp_path)
    env.update({k: str(v) for k, v in (extra or {}).items()})
    proc = subprocess.run(
        [sys.executable, script or CHECK_OPTIONS] + argv,
        input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180,
    )
    records = []
    if log_path.exists():
        records = [json.loads(ln) for ln in log_path.read_text().splitlines() if ln.strip()]
    return proc, records


def _single_document(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    return json.loads(lines[0])


PAPER_ARGVS = {
    "omitted": ["covered_calls", "SPY", "--platform=deribit"],
    "equals_form": ["covered_calls", "SPY", "--platform=deribit", "--mode=paper"],
    "two_token_form": ["covered_calls", "SPY", "--platform=deribit", "--mode", "paper"],
    "two_token_form_first": ["--mode", "paper", "covered_calls", "SPY", "--platform=deribit"],
}


def test_paper_and_omitted_mode_read_the_stdin_positions(tmp_path, subtests):
    shapes = [(shape, PAPER_ARGVS[shape], FOUR_POSITIONS) for shape in sorted(PAPER_ARGVS)]
    shapes.append(("inline_positions_argument_with_two_token_form",
                   ["covered_calls", "SPY", FOUR_POSITIONS, "--platform=deribit", "--mode", "paper"], ""))
    for shape, argv, stdin in shapes:
        with subtests.test(shape=shape):
            run_dir = tmp_path / shape
            run_dir.mkdir()
            proc, records = _run(argv, run_dir, stdin)
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            assert out["skip_reason"] == CAP_REASON
            assert out["strategy"] == "covered_calls"
            assert out["underlying"] == "SPY"
            assert out["platform"] == "deribit"
            assert records == []


def test_paper_control_builds_the_adapter_and_fetches(tmp_path):
    proc, records = _run(["covered_calls", "SPY", "--platform=deribit", "--mode", "paper"], tmp_path, "[]")
    out = _single_document(proc)
    assert out.get("skip_reason") is None
    assert len(records) > 0


LIVE_TAILS = {
    "equals_form": ["--mode=live"],
    "two_token_form": ["--mode", "live"],
}


def test_live_mode_refuses_before_the_cap_and_the_adapter(tmp_path, subtests):
    for tail in sorted(LIVE_TAILS):
        for platform in ["deribit", "robinhood"]:
            with subtests.test(tail=tail, platform=platform):
                run_dir = tmp_path / f"{tail}-{platform}"
                run_dir.mkdir()
                argv = ["covered_calls", "SPY", f"--platform={platform}"] + LIVE_TAILS[tail]
                proc, records = _run(argv, run_dir, FOUR_POSITIONS)
                assert proc.returncode == 1
                out = _single_document(proc)
                assert out["error"] == _expected_error(platform)
                assert out["signal"] == 0
                assert out["actions"] == []
                assert out["platform"] == platform
                assert out.get("skip_reason") is None
                assert records == []


ADAPTER_DRIVER = """
import json, os, sys
sys.path.insert(0, os.path.join(os.getcwd(), "shared_scripts"))
import check_options
import option_data
platform, underlying, call = sys.argv[1:4]
adapter = check_options._load_adapter(platform)
try:
    if call == "vol":
        vol, rank = adapter.get_vol_metrics(underlying)
        out = {"vol": vol, "rank": rank}
    else:
        out = {"premium": list(adapter.get_premium_and_greeks(*json.loads(sys.argv[4])))}
except check_options.OptionDataUnavailable as e:
    out = {"reason": e.reason, "vol_annual": e.vol_annual, "one_identity": type(e) is option_data.OptionDataUnavailable}
sys.__stdout__.write(json.dumps(out) + "\\n")
"""

BASE_CLOSES = [round(100 * math.exp(0.03 * math.sin(i * 1.7) + 0.02 * math.cos(i * 0.37) + 0.001 * i), 6) for i in range(90)]
MID_RANK_CLOSES = [round(100 * math.exp(0.01 * math.sin(i * 1.7) + 0.01 * math.cos(i * 0.37)), 6) for i in range(80)]
LOW_RANK_CLOSES = [round(100 * math.exp((0.05 if i < 70 else 0.005) * math.sin(i * 1.7)), 6) for i in range(90)]
FLAT_RANGE_CLOSES = [100.0, 200.0] * 20
VOL_PLATFORMS = {
    "binanceus": ("BTC", 365, [0.631, 78.8]),
    "deribit": ("BTC", 365, [0.631, 78.8]),
    "ibkr": ("BTC", 365, [0.631, 78.8]),
    "luno": ("BTC", 365, [0.631, 78.8]),
    "okx": ("BTC", 365, [0.631, 78.8]),
    "robinhood": ("SPY", 252, [0.5243, 78.8]),
}
READ_FAILED = "volatility unavailable: daily history read failed"


def _driver(tmp_path, extra, *argv):
    proc, _ = _run([ADAPTER_DRIVER] + [str(a) for a in argv], tmp_path, "", extra=extra, script="-c")
    proc_args = proc.stdout.strip().splitlines()
    assert proc.returncode == 0 and proc_args, proc.stderr
    return json.loads(proc_args[-1])


def _vol_cases(ppy):
    return {
        "baseline_history": (json.dumps(BASE_CLOSES), None),
        "read_error": ("error", {"reason": READ_FAILED, "vol_annual": None}),
        "rate_limited": ("429", {"reason": READ_FAILED + " (upstream status 429)", "vol_annual": None}),
        "server_error": ("503", {"reason": READ_FAILED + " (upstream status 503)", "vol_annual": None}),
        "request_timeout_stays_ordinary": ("timeout", {"reason": READ_FAILED, "vol_annual": None}),
        "short_history": (json.dumps(BASE_CLOSES[:14]), {"reason": "volatility unavailable: fewer than 15 daily closes", "vol_annual": None}),
        "nan_close": (json.dumps(BASE_CLOSES[:40] + [float("nan")] + BASE_CLOSES[41:]), {"reason": "volatility unavailable: invalid daily close", "vol_annual": None}),
        "zero_close": (json.dumps(BASE_CLOSES[:40] + [0.0] + BASE_CLOSES[41:]), {"reason": "volatility unavailable: invalid daily close", "vol_annual": None}),
        "constant_closes": (json.dumps([100.0] * 30), {"reason": "volatility unavailable: realized volatility is not positive", "vol_annual": None}),
        "flat_volatility_range": (json.dumps(FLAT_RANGE_CLOSES), {"reason": "iv rank unavailable: flat volatility range",
                                                                  "vol_annual": round(math.log(2) * math.sqrt(ppy), 4)}),
    }


def test_volatility_adapters_measure_or_refuse_without_substitutes(tmp_path, subtests):
    for platform in sorted(VOL_PLATFORMS):
        underlying, ppy, baseline = VOL_PLATFORMS[platform]
        cases = _vol_cases(ppy)
        for case in sorted(cases):
            daily, want = cases[case]
            with subtests.test(platform=platform, case=case):
                run_dir = tmp_path / f"{platform}-{case}"
                run_dir.mkdir()
                out = _driver(run_dir, {"QP_DAILY": daily}, platform, underlying, "vol")
                if want is None:
                    assert out == {"vol": baseline[0], "rank": baseline[1]}
                    continue
                assert out["one_identity"] is True, out
                assert {k: out[k] for k in want} == want
        with subtests.test(platform=platform, case="fifteen_bars_keep_volatility_without_rank"):
            run_dir = tmp_path / f"{platform}-fifteen"
            run_dir.mkdir()
            out = _driver(run_dir, {"QP_DAILY": json.dumps(BASE_CLOSES[:15])}, platform, underlying, "vol")
            assert out["reason"] == "iv rank unavailable: flat volatility range"
            assert isinstance(out["vol_annual"], float) and out["vol_annual"] > 0


MODEL_CASES = {
    "ibkr": (["BTC", "put", 88000.0, "2026-12-18", 45, 100000.0, 0.5432],
             [0.02531, 2531.03, {"delta": -0.2125, "gamma": 1.5e-05, "theta": -58.24, "vega": 101.89}]),
    "robinhood": (["SPY", "call", 670.0, "2026-12-18", 21, 600.0, 0.2345],
                  [0.000638, 38.28, {"delta": 0.0299, "gamma": 0.002009, "theta": -0.06, "vega": 0.1}]),
}


def test_model_pricing_keeps_its_baseline_and_needs_measured_inputs(tmp_path, subtests):
    for platform in sorted(MODEL_CASES):
        args, baseline = MODEL_CASES[platform]
        variants = {
            "baseline": (args, {"premium": baseline}),
            "no_volatility": (args[:6] + [None], {"reason": "volatility unavailable: model pricing needs measured volatility"}),
            "zero_volatility": (args[:6] + [0.0], {"reason": "volatility unavailable: model pricing needs measured volatility"}),
            "expired": (args[:4] + [0] + args[5:], {"reason": "option model unavailable: days to expiry are not positive"}),
            "zero_spot": (args[:5] + [0.0, args[6]], {"reason": "option model unavailable: spot is not positive"}),
        }
        for name in sorted(variants):
            call_args, want = variants[name]
            with subtests.test(platform=platform, case=name):
                run_dir = tmp_path / f"{platform}-{name}"
                run_dir.mkdir()
                out = _driver(run_dir, {}, platform, args[0], "premium", json.dumps(call_args))
                assert {k: out.get(k) for k in want} == want, out


def _handled_error(proc, want_error=None):
    assert proc.returncode == 1, proc.stderr
    out = _single_document(proc)
    if want_error is not None:
        assert out["error"] == want_error, out
    assert out["signal"] == 0
    assert out["actions"] == []
    assert out.get("skip_reason") is None
    return out


ARG_ERRORS = {
    "missing_underlying": (["covered_calls", "--platform=deribit"], FOUR_POSITIONS, "usage"),
    "extra_positional": (["covered_calls", "SPY", "[]", "extra", "--platform=deribit"], "", "usage"),
    "bare_platform_flag": (["covered_calls", "SPY", "--platform", "deribit"], FOUR_POSITIONS, "usage"),
    "unknown_strategy": (["iron_condor", "SPY", "--platform=deribit"], FOUR_POSITIONS,
                         "Unknown strategy: iron_condor. Available: ['momentum_options', 'vol_mean_reversion', 'protective_puts', 'covered_calls', 'wheel', 'butterfly']"),
    "invalid_positions_json": (["covered_calls", "SPY", "--platform=deribit"], "[{", "invalid positions JSON"),
    "nan_constant_in_positions": (["covered_calls", "SPY", "--platform=deribit"], '[{"strike": NaN}]', "invalid positions JSON"),
    "overflowing_number_in_positions": (["covered_calls", "SPY", "--platform=deribit"], '[{"strike": 1e999}]', "invalid positions JSON"),
    "positions_payload_not_a_list": (["covered_calls", "SPY", "--platform=deribit"], '{"option_type": "call"}', "invalid positions payload: expected a JSON list"),
    "positions_entry_not_an_object": (["covered_calls", "SPY", "--platform=deribit"], '[1, 2, 3, 4]', "invalid positions payload: every entry must be a JSON object"),
}


def test_argument_and_position_errors_fail_before_the_cap_and_the_adapter(tmp_path, subtests):
    for case in sorted(ARG_ERRORS):
        argv, stdin, want = ARG_ERRORS[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            proc, records = _run(argv, run_dir, stdin)
            out = _handled_error(proc)
            if want == "usage":
                assert out["error"].startswith("Usage: ") and "--platform=deribit|ibkr|okx|robinhood" in out["error"], out
            else:
                assert out["error"] == want
            assert records == []


def test_unsupported_platforms_refuse_before_loading_an_adapter(tmp_path, subtests):
    for platform in ["binanceus", "hyperliquid", "luno", "topstep"]:
        with subtests.test(platform=platform):
            run_dir = tmp_path / platform
            run_dir.mkdir()
            proc, records = _run(["covered_calls", "BTC", f"--platform={platform}"], run_dir, FOUR_POSITIONS)
            _handled_error(proc, f"unsupported options platform: {platform}; options run only on deribit, ibkr, okx, robinhood")
            assert records == []
            assert "Traceback" not in proc.stderr


def test_probe_only_and_empty_stdin_keep_their_success(tmp_path):
    proc, records = _run(["covered_calls", "SPY", "--platform=deribit", "--probe-only"], tmp_path, FOUR_POSITIONS)
    assert proc.returncode == 0 and proc.stdout == "" and records == []
    empty = tmp_path / "empty"
    empty.mkdir()
    proc, _ = _run(["covered_calls", "BTC", "--platform=deribit"], empty, "", extra={"QP_QUOTE": 100000, "QP_CLOSE": 100000})
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out["signal"] == -1 and len(out["actions"]) == 1 and not out.get("error")


RUNTIME_ERRORS = {
    "zero_spot": (["protective_puts", "BTC", "--platform=deribit"], {"QP_QUOTE": 0}, "Could not fetch spot price"),
    "nan_spot": (["protective_puts", "BTC", "--platform=deribit"], {"QP_QUOTE": "nan"}, "Could not fetch spot price"),
    "infinite_spot": (["protective_puts", "BTC", "--platform=deribit"], {"QP_QUOTE": "inf"}, "Could not fetch spot price"),
    "missing_momentum_bars": (["momentum_options", "BTC", "--platform=deribit"], {"QP_QUOTE": 100000, "QP_CANDLES": 10}, "momentum bars unavailable"),
    "evaluator_exception": (["covered_calls", "BTC", "--platform=deribit"], {"QP_QUOTE": 100000},
                            "options check failed: KeyError"),
    "rank_strategy_without_rank_vol_mean_reversion": (["vol_mean_reversion", "BTC", "--platform=deribit"], {"QP_QUOTE": 100000, "QP_DAILY": "503"},
                                                       "vol_mean_reversion needs iv rank: " + READ_FAILED + " (upstream status 503)"),
    "rank_strategy_without_rank_butterfly": (["butterfly", "BTC", "--platform=ibkr"], {"QP_QUOTE": 100000, "QP_DAILY": json.dumps(BASE_CLOSES[:15])},
                                             "butterfly needs iv rank: iv rank unavailable: flat volatility range"),
    "ibkr_model_without_volatility": (["protective_puts", "BTC", "--platform=ibkr"], {"QP_QUOTE": 100000, "QP_DAILY": "error"},
                                      "volatility unavailable: model pricing needs measured volatility: " + READ_FAILED),
    "robinhood_logged_out_model_without_volatility": (["covered_calls", "SPY", "--platform=robinhood"], {"QP_QUOTE": 600, "QP_DAILY": "429"},
                                                      "volatility unavailable: model pricing needs measured volatility: " + READ_FAILED + " (upstream status 429)"),
    "non_finite_result_is_refused": (["protective_puts", "BTC", "--platform=deribit"], {"QP_QUOTE": 100000, "QP_OPTION_GREEKS": '{"delta": NaN, "gamma": 0, "theta": 0, "vega": 0}'},
                                     "options result invalid: a value is not finite JSON"),
}
RUNTIME_STDIN = {"evaluator_exception": '[{"option_type": "call", "action": "buy"}]'}


def test_runtime_failures_return_one_error_document_and_exit_1(tmp_path, subtests):
    for case in sorted(RUNTIME_ERRORS):
        argv, extra, want = RUNTIME_ERRORS[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            proc, _ = _run(argv, run_dir, RUNTIME_STDIN.get(case, "[]"), extra=extra)
            _handled_error(proc, want)


def _venue_ok(platform, mark, spot, out):
    assert out["signal"] == 1 and len(out["actions"]) == 1, out
    leg = out["actions"][0]
    assert leg["premium"] == mark
    want_usd = round(mark * spot, 2) if platform == "deribit" else mark * spot
    assert leg["premium_usd"] == want_usd
    assert leg["greeks"] == {"delta": 0.25, "gamma": 0.0001, "theta": -1.5, "vega": 2.5}


VENUES = {
    "deribit": {},
    "okx": {"QP_OKX_OPTIONS": "1"},
}
VENUE_FAILURES = {
    "deribit": {
        "read_error": ("error", "option quote unavailable: deribit ticker read failed"),
        "rate_limited": ("429", "option quote unavailable: deribit ticker read failed (upstream status 429)"),
        "server_error": ("503", "option quote unavailable: deribit ticker read failed (upstream status 503)"),
        "missing_mark": ("missing", "option quote unavailable: deribit mark is not positive"),
        "zero_mark": ("0", "option quote unavailable: deribit mark is not positive"),
        "nan_mark": ("nan", "option quote unavailable: deribit mark is not positive"),
    },
    "okx": {
        "read_error": ("error", "option quote unavailable: okx ticker read failed"),
        "rate_limited": ("429", "option quote unavailable: okx ticker read failed (upstream status 429)"),
        "server_error": ("503", "option quote unavailable: okx ticker read failed (upstream status 503)"),
        "missing_mark": ("missing", "option quote unavailable: okx mark is not positive"),
        "zero_mark": ("0", "option quote unavailable: okx mark is not positive"),
        "nan_mark": ("nan", "option quote unavailable: okx mark is not positive"),
    },
}


def test_venue_quotes_price_without_volatility_and_never_substitute(tmp_path, subtests):
    spot = 100000.0
    for platform in sorted(VENUES):
        base = dict(VENUES[platform], QP_QUOTE=spot, QP_CLOSE=spot)
        with subtests.test(platform=platform, case="usable_quote_without_volatility"):
            run_dir = tmp_path / f"{platform}-novol"
            run_dir.mkdir()
            proc, _ = _run(["protective_puts", "BTC", f"--platform={platform}"], run_dir, "[]",
                           extra=dict(base, QP_DAILY="503", QP_OPTION_QUOTE="0.0123"))
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            assert out["iv_rank"] is None
            assert out["vol_metrics_error"] == READ_FAILED + " (upstream status 503)"
            _venue_ok(platform, 0.0123, spot, out)
        with subtests.test(platform=platform, case="usable_quote_with_measured_metrics"):
            run_dir = tmp_path / f"{platform}-metrics"
            run_dir.mkdir()
            proc, _ = _run(["protective_puts", "BTC", f"--platform={platform}"], run_dir, "[]",
                           extra=dict(base, QP_DAILY=json.dumps(BASE_CLOSES), QP_OPTION_QUOTE="0.0123"))
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            assert out["iv_rank"] == 78.8 and "vol_metrics_error" not in out
            _venue_ok(platform, 0.0123, spot, out)
        for case in sorted(VENUE_FAILURES[platform]):
            quote, want = VENUE_FAILURES[platform][case]
            with subtests.test(platform=platform, case=case):
                run_dir = tmp_path / f"{platform}-{case}"
                run_dir.mkdir()
                proc, _ = _run(["protective_puts", "BTC", f"--platform={platform}"], run_dir, "[]",
                               extra=dict(base, QP_OPTION_QUOTE=quote))
                _handled_error(proc, want)
    with subtests.test(platform="okx", case="no_listed_option"):
        run_dir = tmp_path / "okx-unlisted"
        run_dir.mkdir()
        proc, _ = _run(["protective_puts", "BTC", "--platform=okx"], run_dir, "[]", extra={"QP_QUOTE": spot, "QP_OKX_OPTIONS": "empty"})
        _handled_error(proc, "option quote unavailable: no listed okx option matches")


def test_rank_free_model_entries_price_from_measured_volatility(tmp_path, subtests):
    for strategy in ["covered_calls", "protective_puts"]:
        with subtests.test(strategy=strategy):
            run_dir = tmp_path / strategy
            run_dir.mkdir()
            proc, _ = _run([strategy, "BTC", "--platform=ibkr"], run_dir, "[]",
                           extra={"QP_QUOTE": 100000, "QP_DAILY": json.dumps(FLAT_RANGE_CLOSES)})
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            assert out["iv_rank"] is None
            assert out["vol_metrics_error"] == "iv rank unavailable: flat volatility range"
            assert len(out["actions"]) == 1 and out["actions"][0]["premium_usd"] > 0


def test_a_failed_later_leg_discards_every_earlier_leg(tmp_path, subtests):
    cases = {
        "butterfly_third_leg": (["butterfly", "BTC", "--platform=deribit"], MID_RANK_CLOSES, 2, 3),
        "straddle_put_leg": (["vol_mean_reversion", "BTC", "--platform=deribit"], LOW_RANK_CLOSES, 1, 2),
    }
    for case in sorted(cases):
        argv, closes, served, quotes = cases[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            proc, records = _run(argv, run_dir, "[]", extra={"QP_QUOTE": 100000, "QP_DAILY": json.dumps(closes),
                                                              "QP_OPTION_QUOTE_FAIL_AFTER": served})
            _handled_error(proc, "option quote unavailable: deribit ticker read failed")
            assert [r["mode"] for r in records if r["call"] == "deribit_ticker"] == ["value"] * served + ["error"]
            control = tmp_path / f"{case}-control"
            control.mkdir()
            proc, _ = _run(argv, control, "[]", extra={"QP_QUOTE": 100000, "QP_DAILY": json.dumps(closes)})
            assert proc.returncode == 0, proc.stderr
            assert len(_single_document(proc)["actions"]) == quotes


def test_library_output_reaches_stderr_and_stdout_stays_one_document(tmp_path, subtests):
    cases = {
        "success": ({"QP_QUOTE": 100000}, 0),
        "handled_error": ({"QP_QUOTE": 100000, "QP_OPTION_QUOTE": "error"}, 1),
    }
    for case in sorted(cases):
        extra, code = cases[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            proc, _ = _run(["protective_puts", "BTC", "--platform=deribit"], run_dir, "[]", extra=dict(extra, QP_STDOUT_NOISE="1"))
            assert proc.returncode == code, proc.stderr
            _single_document(proc)
            assert "quote precision stub stdout noise" in proc.stderr
            assert "quote precision stub stdout noise" not in proc.stdout


def test_holds_that_need_no_pricing_succeed_without_metrics_or_quotes(tmp_path, subtests):
    cases = {
        "existing_protective_put": (["protective_puts", "BTC", "--platform=ibkr"], '[{"option_type": "put", "action": "buy", "strike": 80000, "expiry": "2026-12-18"}]'),
        "momentum_with_usable_bars": (["momentum_options", "BTC", "--platform=ibkr"], "[]"),
    }
    for case in sorted(cases):
        argv, stdin = cases[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            proc, _ = _run(argv, run_dir, stdin, extra={"QP_QUOTE": 100000, "QP_DAILY": "error", "QP_SHAPE": "wave"})
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            assert out["signal"] == 0 and out["actions"] == [], out
            assert out["iv_rank"] is None and out["vol_metrics_error"] == READ_FAILED


RH_FIXTURE = os.path.join(ROOT, "shared_scripts", "testdata", "robinhood_offline")
RH_OPTION_URL = "https://api.robinhood.com/options/instruments/opt-1/"


def _rh_routes(instruments, market):
    return {
        "GET https://api.robinhood.com/instruments/?symbol=SPY": [{"status": 200, "json": {"results": [{"tradable_chain_id": "chain-spy"}]}}],
        "GET https://api.robinhood.com/options/instruments/?*": [instruments],
        "GET " + RH_OPTION_URL: [{"status": 200, "json": {"id": "opt-1", "url": RH_OPTION_URL}}],
        "GET https://api.robinhood.com/marketdata/options/?*": [market],
    }


def _rh_market(mark):
    return {"status": 200, "json": {"results": [{"adjusted_mark_price": mark, "delta": "-0.2", "gamma": "0.01", "theta": "-0.05", "vega": "0.3"}]}}


RH_LISTED = {"status": 200, "json": {"next": None, "results": [{"id": "opt-1", "url": RH_OPTION_URL}]}}
RH_QUOTE_CASES = {
    "venue_quote_without_volatility": (_rh_routes(RH_LISTED, _rh_market("1.25")), {"RH_OFFLINE_STUB_DAILY": "error"}, None),
    "venue_quote_with_metrics": (_rh_routes(RH_LISTED, _rh_market("1.25")), {}, None),
    "quote_server_error": (_rh_routes(RH_LISTED, {"status": 503, "json": {"detail": "busy"}}), {},
                           "option quote unavailable: robinhood option market data missing (upstream status 503)"),
    "quote_rate_limited": (_rh_routes(RH_LISTED, {"status": 429, "json": {"detail": "slow down"}}), {},
                           "option quote unavailable: robinhood option market data missing (upstream status 429)"),
    "zero_mark": (_rh_routes(RH_LISTED, _rh_market("0.0000")), {}, "option quote unavailable: robinhood mark is not positive"),
    "nan_mark": (_rh_routes(RH_LISTED, _rh_market("NaN")), {}, "option quote unavailable: robinhood mark is not positive"),
    "no_tradable_option": (_rh_routes({"status": 200, "json": {"next": None, "results": []}}, _rh_market("1.25")), {},
                           "option quote unavailable: no tradable robinhood option matches"),
}


def test_authenticated_robinhood_prices_only_from_a_venue_quote(tmp_path, subtests):
    for case in sorted(RH_QUOTE_CASES):
        routes, extra, want = RH_QUOTE_CASES[case]
        with subtests.test(case=case):
            run_dir = tmp_path / case
            run_dir.mkdir()
            routes_path = run_dir / "routes.json"
            routes_path.write_text(json.dumps(routes))
            env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE", "QP_"))}
            env.update({
                "HOME": str(run_dir),
                "PYTHONPATH": RH_FIXTURE,
                "RH_OFFLINE_STUB_MODE": "routes",
                "RH_OFFLINE_STUB_ROUTES": str(routes_path),
                "ROBINHOOD_USERNAME": "offline-user",
                "ROBINHOOD_PASSWORD": "offline-pass",
                "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
                "GO_TRADER_ROBINHOOD_SESSION_DIR": str(run_dir / "robinhood-session"),
            })
            env.update(extra)
            proc = subprocess.run([sys.executable, CHECK_OPTIONS, "protective_puts", "SPY", "--platform=robinhood"],
                                  input="[]", capture_output=True, text=True, cwd=ROOT, env=env, timeout=180)
            assert "Starting login process..." in proc.stderr, proc.stderr
            if want is not None:
                _handled_error(proc, want)
                continue
            assert proc.returncode == 0, proc.stderr
            out = _single_document(proc)
            leg = out["actions"][0]
            assert leg["premium_usd"] == 125.0
            assert leg["premium"] == round(1.25 / out["spot_price"], 6)
            assert leg["greeks"] == {"delta": -0.2, "gamma": 0.01, "theta": -0.05, "vega": 0.3}
            if extra:
                assert out["iv_rank"] is None and out["vol_metrics_error"] == READ_FAILED
            else:
                assert isinstance(out["iv_rank"], float) and "vol_metrics_error" not in out
