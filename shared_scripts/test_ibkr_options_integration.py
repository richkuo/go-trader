import json
import math
import os
import subprocess
import sys
from datetime import datetime, timedelta, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
QP_STUB = os.path.join(ROOT, "shared_scripts", "testdata", "quote_precision")
MARKS = os.path.join(ROOT, "shared_scripts", "fetch_option_marks.py")
CHECK_OPTIONS = os.path.join(ROOT, "shared_scripts", "check_options.py")
DAILY = [round(100 * math.exp(0.03 * math.sin(i * 1.7) + 0.02 * math.cos(i * 0.37) + 0.001 * i), 6) for i in range(90)]
FLAT_RANK_DAILY = [100.0, 200.0] * 20
SIZE_FIELDS = ("multiplier", "contract_spec", "contract_type")
SETTLEMENT_SOURCE = "binanceus_spot_vwap_1530_1600_london"
SETTLEMENT_BASIS = "spot_candle_close_vwap"
MARK_SOURCE = "binanceus_black_scholes_hv14"
MINUTE_MS = 60_000

DRIVER = """
import json, os, sys
sys.path.insert(0, os.path.join(os.getcwd(), "platforms", "ibkr"))
sys.path.insert(0, os.path.join(os.getcwd(), "shared_tools"))
import adapter
ctx = adapter.parse_calendar_context(sys.argv[1], "ibkr", "BTC")
today, cands = adapter.candidate_expiries(ctx)
out = {"today": today.isoformat(), "candidates": [d.isoformat() for d in cands],
       "select": {str(t): list(adapter.select_expiry(ctx, t)) for t in json.loads(sys.argv[2])}}
print(json.dumps(out))
"""


def _calendar(evaluated_at, platform="ibkr", underlying="BTC", **override):
    doc = {"contract": "option_calendar_v1", "platform": platform, "underlying": underlying, "evaluated_at": evaluated_at,
           "cutoff_zone": "Europe/London", "cutoff_hour": 16, "cutoff_minute": 0}
    doc.update(override)
    return json.dumps(doc)


def _env(tmp_path, extra=None):
    env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE", "QP_"))}
    log_path = tmp_path / "qp.log"
    env.update({"HOME": str(tmp_path), "QP_STUB": "1", "QP_LOG": str(log_path), "PYTHONPATH": QP_STUB,
                "QP_QUOTE": "60000", "QP_CLOSE": "60000", "QP_DAILY": json.dumps(DAILY)})
    env.update({k: str(v) for k, v in (extra or {}).items()})
    return env, log_path


def _run(tmp_path, argv, stdin="", extra=None):
    env, log_path = _env(tmp_path, extra)
    proc = subprocess.run([sys.executable, *argv], input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180)
    records = [json.loads(ln) for ln in log_path.read_text().splitlines() if ln.strip()] if log_path.exists() else []
    return proc, records


def _doc(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    return json.loads(lines[0])


def _drive(tmp_path, evaluated_at, targets=(21, 30, 45)):
    proc, _ = _run(tmp_path, ["-c", DRIVER, _calendar(evaluated_at), json.dumps(list(targets))])
    assert proc.returncode == 0, proc.stderr
    return _doc(proc)


def _subdir(tmp_path, name):
    d = tmp_path / name
    d.mkdir()
    return d


def test_calendar_selects_listed_weekday_candidates(tmp_path, subtests):
    with subtests.test(case="targets_21_30_45"):
        out = _drive(_subdir(tmp_path, "targets"), "2026-11-04T12:00:00Z")
        assert out["today"] == "2026-11-04"
        assert out["select"] == {"21": ["2026-11-27", 23], "30": ["2026-12-04", 30], "45": ["2026-12-25", 51]}
        for day in out["candidates"]:
            assert datetime.fromisoformat(day).weekday() < 5, day
        weekdays = {datetime.fromisoformat(d).weekday() for d in out["candidates"][:10]}
        assert {1, 3} <= weekdays, out["candidates"]
    with subtests.test(case="tie_uses_the_later_date"):
        out = _drive(_subdir(tmp_path, "tie"), "2026-11-04T12:00:00Z", targets=(44,))
        assert out["select"] == {"44": ["2026-12-25", 51]}
    with subtests.test(case="winter_same_day_cutoff"):
        before = _drive(_subdir(tmp_path, "winter-before"), "2026-11-20T15:59:59Z")["candidates"]
        at = _drive(_subdir(tmp_path, "winter-at"), "2026-11-20T16:00:00Z")["candidates"]
        assert before[0] == "2026-11-20" and "2026-11-20" not in at
    with subtests.test(case="summer_same_day_cutoff"):
        before = _drive(_subdir(tmp_path, "summer-before"), "2026-06-19T14:59:59Z")["candidates"]
        at = _drive(_subdir(tmp_path, "summer-at"), "2026-06-19T15:00:00Z")["candidates"]
        assert before[0] == "2026-06-19" and "2026-06-19" not in at
    with subtests.test(case="weekday_rollover_boundary"):
        before = _drive(_subdir(tmp_path, "tue-before"), "2026-11-05T09:00:00Z")["candidates"]
        on = _drive(_subdir(tmp_path, "tue-on"), "2026-11-06T09:00:00Z")["candidates"]
        assert "2026-11-10" in before and "2026-11-17" not in before
        assert "2026-11-10" in on and "2026-11-17" in on
    with subtests.test(case="friday_rollover_and_monthly_friday"):
        before = [d for d in _drive(_subdir(tmp_path, "fri-before"), "2026-11-03T09:00:00Z")["candidates"]
                  if datetime.fromisoformat(d).weekday() == 4 and d < "2026-12-20"]
        on = [d for d in _drive(_subdir(tmp_path, "fri-on"), "2026-11-04T09:00:00Z")["candidates"]
              if datetime.fromisoformat(d).weekday() == 4 and d < "2026-12-20"]
        assert before == ["2026-11-06", "2026-11-13", "2026-11-20", "2026-11-27", "2026-12-04"]
        assert on == ["2026-11-06", "2026-11-13", "2026-11-20", "2026-11-27", "2026-12-04", "2026-12-11"]
        assert on.count("2026-11-27") == 1
    with subtests.test(case="second_december_listed"):
        out = _drive(_subdir(tmp_path, "december"), "2027-01-04T09:00:00Z")
        assert [d for d in out["candidates"] if d.endswith("-12-31") or d.endswith("-12-29")] == ["2027-12-31", "2028-12-29"]


def _check(tmp_path, argv, extra=None, stdin="[]"):
    return _run(tmp_path, [CHECK_OPTIONS, *argv], stdin=stdin, extra=extra)


def test_entry_uses_the_scheduler_calendar_and_emits_no_size_fields(tmp_path, subtests):
    with subtests.test(case="covered_call_entry"):
        proc, _ = _check(_subdir(tmp_path, "entry"), ["covered_calls", "BTC", "--platform=ibkr", "--mode=paper",
                                                      "--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z")])
        assert proc.returncode == 0, proc.stderr
        out = _doc(proc)
        assert out["platform"] == "ibkr" and out["premium_unit"] == "underlying_unit"
        assert out["structure_contract"] == "options_structures_v1"
        assert len(out["actions"]) == 1, out
        leg = out["actions"][0]
        assert leg["expiry"] == "2026-11-27" and leg["dte"] == 23, leg
        assert leg["structure_id"] == "covered_calls-1" and leg["structure_legs"] == 1
        for field in SIZE_FIELDS:
            assert field not in out and field not in leg, out
    failures = {
        "missing_context": ([], "expiry calendar unavailable: no option calendar context from the scheduler (--option-calendar-json)"),
        "malformed_context": (["--option-calendar-json={not json"], "invalid option calendar context: option calendar context is not valid JSON"),
        "wrong_platform": (["--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z", platform="deribit")],
                           "invalid option calendar context: option calendar context platform 'deribit' does not match 'ibkr'"),
        "wrong_underlying": (["--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z", underlying="ETH")],
                             "invalid option calendar context: option calendar context underlying 'ETH' does not match 'BTC'"),
        "naive_clock": (["--option-calendar-json=" + _calendar("2026-11-04T12:00:00")],
                        "invalid option calendar context: option calendar context evaluated_at has no time zone"),
        "bad_zone": (["--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z", cutoff_zone="Mars/Olympus")],
                     "invalid option calendar context: option calendar context cutoff_zone 'Mars/Olympus' is not a known time zone"),
        "flag_without_value": (["--option-calendar-json"], None),
    }
    for case in sorted(failures):
        extra_args, want = failures[case]
        with subtests.test(case=case):
            proc, _ = _check(_subdir(tmp_path, case), ["covered_calls", "BTC", "--platform=ibkr", *extra_args])
            assert proc.returncode == 1, proc.stderr
            out = _doc(proc)
            if want is None:
                assert out["error"].startswith("Usage: "), out
            else:
                assert out["error"] == want, out
            assert out["actions"] == []
    with subtests.test(case="deribit_refuses_a_calendar_context"):
        proc, _ = _check(_subdir(tmp_path, "deribit"), ["covered_calls", "BTC", "--platform=deribit",
                                                        "--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z", platform="deribit")])
        assert proc.returncode == 1 and _doc(proc)["error"] == "deribit options take no --option-calendar-json context"
    with subtests.test(case="live_mode_refusal_kept"):
        proc, _ = _check(_subdir(tmp_path, "live"), ["covered_calls", "BTC", "--platform=ibkr", "--mode=live",
                                                     "--option-calendar-json=" + _calendar("2026-11-04T12:00:00Z")])
        assert proc.returncode == 1 and _doc(proc)["error"].startswith("options strategies run as paper only")
    with subtests.test(case="early_cap_hold_kept"):
        four = json.dumps([{"option_type": "call", "strike": 70000 + i, "expiry": "2026-12-18"} for i in range(4)])
        proc, _ = _check(_subdir(tmp_path, "cap"), ["covered_calls", "BTC", "--platform=ibkr"], stdin=four)
        assert proc.returncode == 0 and _doc(proc)["skip_reason"] == "Max positions reached (4/4)"


def test_calendar_probe(tmp_path, subtests):
    with subtests.test(case="valid"):
        proc, records = _check(_subdir(tmp_path, "valid"), ["wheel", "BTC", "--platform=ibkr",
                                                            "--option-calendar-json=" + _calendar("2026-01-02T12:00:00Z"), "--probe-only"])
        assert proc.returncode == 0, proc.stderr
        assert _doc(proc) == {"contract": "option_calendar_v1", "platform": "ibkr", "probe": True}
        assert records == []
    with subtests.test(case="invalid"):
        proc, _ = _check(_subdir(tmp_path, "invalid"), ["wheel", "BTC", "--platform=ibkr",
                                                        "--option-calendar-json=" + _calendar("2026-01-02T12:00:00Z", cutoff_hour=24), "--probe-only"])
        assert proc.returncode == 2 and proc.stdout.strip() == ""
        assert "cutoff_hour must be an integer from 0 to 23" in proc.stderr
    with subtests.test(case="generic_probe_unchanged"):
        proc, _ = _check(_subdir(tmp_path, "generic"), ["probe", "BTC", "1h", "--mark-price=0", "--probe-only"])
        assert proc.returncode == 0 and proc.stdout == ""


def _iso(dt):
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _schedule(expiry):
    from zoneinfo import ZoneInfo
    day = datetime.fromisoformat(expiry)
    cutoff = datetime(day.year, day.month, day.day, 16, 0, tzinfo=ZoneInfo("Europe/London")).astimezone(timezone.utc)
    return cutoff, cutoff + timedelta(minutes=5)


def _leg(leg_id, expiry, option_type="put", strike=60000.0, underlying="BTC", **override):
    cutoff, ready = _schedule(expiry)
    leg = {"id": leg_id, "underlying": underlying, "option_type": option_type, "strike": strike, "expiry": expiry,
           "action": "sell", "dte": 5.0, "operation": "mark", "cutoff_utc": _iso(cutoff), "settlement_ready_utc": _iso(ready)}
    leg.update(override)
    return leg


def _marks(tmp_path, refresh_at, legs, extra=None, **override):
    body = {"contract": "option_marks_v1", "platform": "ibkr", "refresh_at": _iso(refresh_at), "legs": legs}
    body.update(override)
    proc, records = _run(tmp_path, [MARKS, "--platform=ibkr"], stdin=json.dumps(body), extra=extra)
    doc = _doc(proc)
    assert doc["contract"] == "option_marks_v1" and doc["platform"] == "ibkr" and doc["premium_unit"] == "underlying_unit", doc
    return proc, doc, records


def _window(expiry, closes, volumes=None, start_offset=0):
    cutoff, _ = _schedule(expiry)
    start = int(cutoff.timestamp() * 1000) - 30 * MINUTE_MS
    volumes = volumes or [1.0 + (i % 3) for i in range(len(closes))]
    return [[start + (i + start_offset) * MINUTE_MS, c, c, c, c, v] for i, (c, v) in enumerate(zip(closes, volumes))]


def _minute_calls(records):
    return [r for r in records if r.get("call") == "fetch_ohlcv" and r.get("timeframe") == "1m"]


def test_marks_use_measured_volatility_and_the_shared_model(tmp_path, subtests):
    sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
    import option_data
    from pricing import bs_price_and_greeks
    refresh_at = datetime(2026, 11, 4, 12, 0, tzinfo=timezone.utc)
    expiry = "2026-11-27"
    cutoff, _ = _schedule(expiry)
    with subtests.test(case="mark"):
        proc, doc, records = _marks(_subdir(tmp_path, "mark"), refresh_at, [_leg("a", expiry), _leg("b", expiry, option_type="call", strike=65000.0)])
        assert proc.returncode == 0, proc.stderr
        vol, _ = option_data.realized_vol_metrics(DAILY, 365)
        dte = (cutoff - refresh_at).total_seconds() / 86400.0
        for leg, (option_type, strike) in zip(doc["legs"], [("put", 60000.0), ("call", 65000.0)]):
            want, greeks = bs_price_and_greeks(60000.0, strike, dte, vol, option_type=option_type)
            assert leg["status"] == "mark", leg
            assert leg["cutoff_utc"] == "2026-11-27T16:00:00Z" and leg["settlement_ready_utc"] == "2026-11-27T16:05:00Z"
            assert math.isclose(leg["mark"]["value_per_unit"], want, rel_tol=1e-12) and leg["mark"]["spot"] == 60000.0
            assert leg["mark"]["source"] == MARK_SOURCE and leg["mark"]["greeks"] == {k: float(greeks[k]) for k in ("delta", "gamma", "theta", "vega")}
        daily = [r for r in records if r.get("timeframe") == "1d"]
        tickers = [r for r in records if r.get("call") == "fetch_ticker"]
        assert len(daily) == 1 and len(tickers) == 1, records
        assert _minute_calls(records) == []
    with subtests.test(case="rank_unavailable_keeps_volatility"):
        proc, doc, _ = _marks(_subdir(tmp_path, "rank"), refresh_at, [_leg("a", expiry)], extra={"QP_DAILY": json.dumps(FLAT_RANK_DAILY)})
        assert proc.returncode == 0 and doc["legs"][0]["status"] == "mark", doc
    with subtests.test(case="missing_history_is_a_leg_error"):
        proc, doc, _ = _marks(_subdir(tmp_path, "nohist"), refresh_at, [_leg("a", expiry)], extra={"QP_DAILY": "error"})
        assert proc.returncode == 0
        assert doc["legs"][0] == {"id": "a", "cutoff_utc": "2026-11-27T16:00:00Z", "settlement_ready_utc": "2026-11-27T16:05:00Z",
                                  "status": "error", "reason": "volatility unavailable: model pricing needs measured volatility: volatility unavailable: daily history read failed"}
    with subtests.test(case="missing_spot_is_a_leg_error"):
        proc, doc, _ = _marks(_subdir(tmp_path, "nospot"), refresh_at, [_leg("a", expiry)], extra={"QP_QUOTE": "error"})
        assert doc["legs"][0]["status"] == "error" and doc["legs"][0]["reason"] == "spot unavailable: BTC spot price is not positive"


def test_request_schedule_is_validated(tmp_path, subtests):
    refresh_at = datetime(2026, 11, 4, 12, 0, tzinfo=timezone.utc)
    cases = {
        "missing_cutoff": (_leg("a", "2026-11-27", cutoff_utc=None), "request leg a cutoff_utc is missing"),
        "ready_before_cutoff": (_leg("a", "2026-11-27", settlement_ready_utc="2026-11-27T15:00:00Z"), "request leg a settlement_ready_utc is before its cutoff_utc"),
        "cutoff_on_another_day": (_leg("a", "2026-11-27", cutoff_utc="2026-11-28T16:00:00Z", settlement_ready_utc="2026-11-28T16:05:00Z"),
                                  "request leg a cutoff_utc 2026-11-28T16:00:00Z is not on its expiry date 2026-11-27"),
    }
    for case in sorted(cases):
        leg, want = cases[case]
        with subtests.test(case=case):
            proc, doc, records = _marks(_subdir(tmp_path, case), refresh_at, [leg])
            assert proc.returncode == 1 and doc["error"] == want and doc["legs"] == [], doc
            assert records == []


def test_settlement_reads_one_dated_window(tmp_path, subtests):
    expiry = "2026-11-20"
    cutoff, ready = _schedule(expiry)
    closes = [58000.0 + 10 * i for i in range(30)]
    volumes = [1.0 + (i % 3) for i in range(30)]
    want = sum(c * v for c, v in zip(closes, volumes)) / sum(volumes)

    def run(name, bars, at=ready, legs=None):
        return _marks(_subdir(tmp_path, name), at, legs or [_leg("p", expiry)], extra={"QP_MINUTE_BARS": json.dumps(bars)})

    with subtests.test(case="pending_between_cutoff_and_ready_reads_nothing"):
        proc, doc, records = run("waiting", {"BTC/USDT": _window(expiry, closes)}, at=cutoff + timedelta(minutes=4, seconds=59))
        assert doc["legs"][0]["status"] == "pending" and "2026-11-20T16:05:00Z" in doc["legs"][0]["reason"]
        assert _minute_calls(records) == [] and [r for r in records if r.get("call") == "fetch_ticker"] == []
    with subtests.test(case="exact_window_at_readiness"):
        outside = [[_window(expiry, [1.0])[0][0] - MINUTE_MS, 1.0, 1.0, 1.0, 1.0, 1e9]]
        proc, doc, records = run("exact", {"BTC/USDT": outside + _window(expiry, closes, volumes) + _window(expiry, [1.0], [1e9], start_offset=30)})
        leg = doc["legs"][0]
        assert leg["status"] == "settled", leg
        assert math.isclose(leg["settlement"]["price"], want, rel_tol=1e-12)
        assert leg["settlement"]["basis"] == SETTLEMENT_BASIS and leg["settlement"]["source"] == SETTLEMENT_SOURCE and leg["settlement"]["date"] == expiry
        calls = _minute_calls(records)
        assert [(c["symbol"], c["since"], c["limit"]) for c in calls] == [("BTC/USDT", int(cutoff.timestamp() * 1000) - 30 * MINUTE_MS, 30)]
    with subtests.test(case="one_read_per_underlying_and_expiry"):
        proc, doc, records = run("cached", {"BTC/USDT": _window(expiry, closes, volumes)},
                                 legs=[_leg("p", expiry), _leg("c", expiry, option_type="call", strike=57000.0)])
        assert [l["status"] for l in doc["legs"]] == ["settled", "settled"] and len(_minute_calls(records)) == 1
    bad = {
        "missing_minute": (_window(expiry, closes)[:10] + _window(expiry, closes)[11:] + _window(expiry, [58000.0], [5.0], start_offset=30),
                           "1 of 30 minute candles missing"),
        "duplicate_minute": (_window(expiry, closes)[:4] + [_window(expiry, closes)[3]] + _window(expiry, closes)[4:], "duplicate candle at"),
        "misaligned_minute": ([[r[0] + (1000 if i == 5 else 0)] + r[1:] for i, r in enumerate(_window(expiry, closes))], "is not on the minute grid"),
        "nan_close": ([r[:4] + [float("nan"), r[5]] if i == 7 else r for i, r in enumerate(_window(expiry, closes))], "is not a finite positive price"),
        "zero_close": ([r[:4] + [0.0, r[5]] if i == 7 else r for i, r in enumerate(_window(expiry, closes))], "is not a finite positive price"),
        "negative_volume": ([r[:5] + [-1.0] if i == 7 else r for i, r in enumerate(_window(expiry, closes))], "is not a finite non-negative amount"),
        "zero_total_volume": (_window(expiry, closes, [0.0] * 30), "total window volume is zero"),
    }
    for case in sorted(bad):
        rows, reason = bad[case]
        with subtests.test(case=case):
            proc, doc, records = _marks(_subdir(tmp_path, case), ready, [_leg("p", expiry)],
                                        extra={"QP_MINUTE_BARS": json.dumps({"BTC/USDT": rows}, allow_nan=True)})
            leg = doc["legs"][0]
            assert leg["status"] == "pending" and reason in leg["reason"] and "BTC/USD " not in leg["reason"].split("; ")[0], leg
            assert [c["symbol"] for c in _minute_calls(records)] == ["BTC/USDT", "BTC/USD"]
    with subtests.test(case="second_symbol_replaces_a_failed_first"):
        proc, doc, records = run("fallback", {"BTC/USDT": "error", "BTC/USD": _window(expiry, closes, volumes)})
        assert doc["legs"][0]["status"] == "settled" and math.isclose(doc["legs"][0]["settlement"]["price"], want, rel_tol=1e-12)
        assert [c["symbol"] for c in _minute_calls(records)] == ["BTC/USDT", "BTC/USD"]
    with subtests.test(case="second_symbol_replaces_an_incomplete_first"):
        proc, doc, _ = run("fallback-incomplete", {"BTC/USDT": _window(expiry, closes)[:10], "BTC/USD": _window(expiry, closes, volumes)})
        assert doc["legs"][0]["status"] == "settled"
    with subtests.test(case="mixed_symbol_windows_are_refused"):
        full = _window(expiry, closes, volumes)
        proc, doc, _ = run("mixed", {"BTC/USDT": full[:15], "BTC/USD": full[15:]})
        leg = doc["legs"][0]
        assert leg["status"] == "pending" and leg["reason"].startswith(f"no complete {SETTLEMENT_SOURCE} window for BTC expiry {expiry}: "), leg
    with subtests.test(case="rate_limited_reads_keep_status"):
        proc, doc, _ = run("rate", {"BTC/USDT": "429", "BTC/USD": "429"})
        assert doc["legs"][0]["status"] == "pending" and "(upstream status 429)" in doc["legs"][0]["reason"]
    with subtests.test(case="a_later_refresh_retries"):
        _, first, _ = run("retry-1", {"BTC/USDT": "error", "BTC/USD": "error"})
        _, second, records = run("retry-2", {"BTC/USDT": _window(expiry, closes, volumes)})
        assert first["legs"][0]["status"] == "pending" and second["legs"][0]["status"] == "settled"


def test_probe_validates_the_ibkr_branch(tmp_path, subtests):
    with subtests.test(case="valid"):
        proc, records = _run(_subdir(tmp_path, "valid"), [MARKS, "--platform=ibkr", "--probe-only"])
        assert proc.returncode == 0, proc.stderr
        assert _doc(proc) == {"contract": "option_marks_v1", "platform": "ibkr", "probe": True}
        assert records == []
    with subtests.test(case="missing_dependency"):
        shadow = _subdir(tmp_path, "shadow")
        (shadow / "ccxt.py").write_text('raise ImportError("ccxt is not installed")\n')
        env, _ = _env(tmp_path)
        env["PYTHONPATH"] = str(shadow)
        proc = subprocess.run([sys.executable, MARKS, "--platform=ibkr", "--probe-only"], capture_output=True, text=True, cwd=ROOT, env=env, timeout=60)
        assert proc.returncode != 0 and proc.stdout.strip() == "" and "ccxt is not installed" in proc.stderr
