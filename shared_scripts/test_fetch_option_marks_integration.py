import json
import math
import os
import subprocess
import sys
from datetime import date, datetime, timedelta, timezone

import exchange_calendars
import pandas as pd

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
QP_STUB = os.path.join(ROOT, "shared_scripts", "testdata", "quote_precision")
RH_FIXTURE = os.path.join(ROOT, "shared_scripts", "testdata", "robinhood_offline")
SCRIPT = os.path.join(ROOT, "shared_scripts", "fetch_option_marks.py")
CREDENTIALS = ("ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET")
XNYS = exchange_calendars.get_calendar("XNYS")


def _baseline_daily():
    return json.dumps([round(100 * math.exp(0.03 * math.sin(i * 1.7) + 0.02 * math.cos(i * 0.37) + 0.001 * i), 6) for i in range(90)])


def _flat_rank_daily():
    return json.dumps([round(100 * (1.01 if i % 2 else 1.0), 6) for i in range(90)])


def _iso(dt):
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _leg(leg_id, expiry, underlying="SPY", option_type="put", strike=450.0, operation="mark"):
    return {"id": leg_id, "underlying": underlying, "option_type": option_type, "strike": strike, "expiry": expiry,
            "action": "sell", "dte": 5.0, "operation": operation}


def _request(refresh_at, legs, **override):
    body = {"contract": "option_marks_v1", "platform": "robinhood", "refresh_at": _iso(refresh_at), "legs": legs}
    body.update(override)
    return json.dumps(body)


def _clean_env(tmp_path):
    env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE", "QP_"))}
    env["HOME"] = str(tmp_path)
    return env


def _run_qp(tmp_path, stdin, extra=None, argv=("--platform=robinhood",)):
    env = _clean_env(tmp_path)
    log_path = tmp_path / "qp.log"
    env.update({"QP_STUB": "1", "QP_LOG": str(log_path), "PYTHONPATH": QP_STUB, "QP_QUOTE": "450", "QP_CLOSE": "450",
                "QP_DAILY": _baseline_daily()})
    env.update({k: str(v) for k, v in (extra or {}).items()})
    proc = subprocess.run([sys.executable, SCRIPT, *argv], input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180)
    records = [json.loads(ln) for ln in log_path.read_text().splitlines() if ln.strip()] if log_path.exists() else []
    return proc, records


def _doc(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    doc = json.loads(lines[0])
    assert doc["contract"] == "option_marks_v1" and doc["platform"] == "robinhood" and doc["premium_unit"] == "underlying_unit", doc
    return doc


def _only_leg(proc):
    doc = _doc(proc)
    assert proc.returncode == 0, proc.stderr
    assert len(doc["legs"]) == 1, doc
    return doc["legs"][0]


def _sessions(start, end):
    return [s.date() for s in XNYS.sessions_in_range(pd.Timestamp(start), pd.Timestamp(end))]


def _session_close(day):
    return XNYS.session_close(pd.Timestamp(day)).tz_convert("UTC").to_pydatetime()


def _future_session(min_days=20):
    today = datetime.now(timezone.utc).date()
    return next(d for d in _sessions(today + timedelta(days=min_days), today + timedelta(days=min_days + 14)))


def _past_session(days_back=30):
    today = datetime.now(timezone.utc).date()
    return next(d for d in _sessions(today - timedelta(days=days_back), today - timedelta(days=days_back - 14)))


def test_logged_out_marks_and_failures(tmp_path, subtests):
    expiry = _future_session()
    refresh = _session_close(expiry) - timedelta(days=10)
    cases = {
        "model_mark": ({}, _leg("leg", expiry.isoformat()), "mark", None),
        "rank_unavailable_uses_measured_volatility": ({"QP_DAILY": _flat_rank_daily()}, _leg("leg", expiry.isoformat()), "mark", None),
        "missing_spot_data": ({"QP_QUOTE": ""}, _leg("leg", expiry.isoformat()), "error", "spot unavailable: SPY spot price is not positive"),
        "zero_spot": ({"QP_QUOTE": "0"}, _leg("leg", expiry.isoformat()), "error", "spot unavailable: SPY spot price is not positive"),
        "volatility_read_fails": ({"QP_DAILY": "error"}, _leg("leg", expiry.isoformat()), "error",
                                  "volatility unavailable: model pricing needs measured volatility: volatility unavailable: daily history read failed"),
        "too_few_closes": ({"QP_DAILY": "[100, 101]"}, _leg("leg", expiry.isoformat()), "error",
                           "volatility unavailable: model pricing needs measured volatility: volatility unavailable: fewer than 15 daily closes"),
        "model_underflow_is_not_a_mark": ({}, _leg("leg", expiry.isoformat(), option_type="call", strike=1e9), "error",
                                          "option model unavailable: black-scholes price is not positive"),
        "crypto_underlying_is_refused": ({}, _leg("leg", expiry.isoformat(), underlying="BTC"), "error", "unsupported basis: BTC resolves to a Yahoo crypto pair"),
    }
    for name in sorted(cases):
        extra, leg, status, reason = cases[name]
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            proc, _ = _run_qp(run_dir, _request(refresh, [leg]), extra)
            out = _only_leg(proc)
            assert out["status"] == status, out
            assert out["cutoff_utc"] == _iso(_session_close(expiry)), out
            if status == "mark":
                mark = out["mark"]
                assert mark["source"] == "robinhood_black_scholes" and mark["value_per_unit"] > 0 and mark["spot"] == 450, out
                assert all(math.isfinite(v) for v in mark["greeks"].values()), out
            else:
                assert out["reason"].startswith(reason), out
                assert "mark" not in out and "settlement" not in out, out


def test_batch_errors_fail_the_whole_document(tmp_path, subtests):
    expiry = _future_session().isoformat()
    now = datetime.now(timezone.utc)
    cases = {
        "bad_contract": (_request(now, [_leg("a", expiry)], contract="option_marks_v0"), "request contract is not option_marks_v1"),
        "bad_platform": (_request(now, [_leg("a", expiry)], platform="deribit"), "request platform 'deribit' does not match --platform=robinhood"),
        "duplicate_id": (_request(now, [_leg("a", expiry), _leg("a", expiry)]), "request leg id 'a' is missing or duplicated"),
        "non_finite_number": ('{"contract":"option_marks_v1","platform":"robinhood","refresh_at":"2026-01-01T00:00:00Z","legs":[{"strike":NaN}]}',
                              "invalid option marks request JSON"),
        "bad_operation": (_request(now, [dict(_leg("a", expiry), operation="close")]), "request leg a has an invalid operation"),
    }
    for name in sorted(cases):
        stdin, reason = cases[name]
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            proc, records = _run_qp(run_dir, stdin)
            assert proc.returncode == 1, proc.stderr
            doc = _doc(proc)
            assert doc["error"] == reason and doc["legs"] == [], doc
            assert not any(r["call"].startswith("yahoo") for r in records), records


def test_session_calendar_cutoffs_come_from_the_library(tmp_path, subtests):
    year = datetime.now(timezone.utc).year
    summer = next(d for d in _sessions(date(year, 7, 6), date(year, 7, 31)) if _session_close(d).hour == 20)
    winter = next(d for d in _sessions(date(year, 1, 5), date(year, 1, 31)) if _session_close(d).hour == 21)
    early = [d.date() for d in XNYS.early_closes if XNYS.first_session <= d <= XNYS.last_session and d.year == year][0]
    saturday = next(d for d in (date(year, 3, 1) + timedelta(days=i) for i in range(60))
                    if d.weekday() == 5 and XNYS.is_session(pd.Timestamp(d - timedelta(days=1))))
    out_of_bounds = (XNYS.last_session + pd.Timedelta(days=40)).date()
    refresh = datetime(year - 1, 12, 1, tzinfo=timezone.utc)
    legs = [_leg("summer", summer.isoformat()), _leg("winter", winter.isoformat()), _leg("early", early.isoformat()),
            _leg("weekend", saturday.isoformat()), _leg("bounds", out_of_bounds.isoformat()), _leg("invalid", "2026-02-30")]
    proc, _ = _run_qp(tmp_path, _request(refresh, legs))
    doc = _doc(proc)
    got = {leg["id"]: leg for leg in doc["legs"]}
    with subtests.test(case="summer_regular_close"):
        assert got["summer"]["cutoff_utc"].endswith("T20:00:00Z") and got["summer"]["cutoff_utc"] == _iso(_session_close(summer)), got["summer"]
        assert got["summer"]["settlement_ready_utc"] == _iso(_session_close(summer) + timedelta(minutes=60))
    with subtests.test(case="winter_regular_close"):
        assert got["winter"]["cutoff_utc"].endswith("T21:00:00Z") and got["winter"]["cutoff_utc"] == _iso(_session_close(winter)), got["winter"]
    with subtests.test(case="early_close"):
        assert got["early"]["cutoff_utc"] == _iso(_session_close(early)), got["early"]
        assert _session_close(early) < XNYS.session_open(pd.Timestamp(early)).tz_convert("UTC").to_pydatetime() + timedelta(hours=6, minutes=30)
    with subtests.test(case="weekend_maps_to_prior_session"):
        friday = saturday - timedelta(days=1)
        assert got["weekend"]["session_date"] == friday.isoformat() and got["weekend"]["non_session_mapped"] is True, got["weekend"]
        assert got["weekend"]["cutoff_utc"] == _iso(_session_close(friday)), got["weekend"]
    with subtests.test(case="out_of_bounds_is_unresolved"):
        assert got["bounds"]["status"] == "error" and got["bounds"]["reason"].startswith("session unresolved:"), got["bounds"]
        assert "cutoff_utc" not in got["bounds"]
    with subtests.test(case="invalid_date_is_unresolved"):
        assert got["invalid"]["status"] == "error" and "is not a YYYY-MM-DD date" in got["invalid"]["reason"], got["invalid"]


def _bars(days, close_for=None, split_on=None):
    out = []
    for d in days:
        bar = {"date": d.isoformat(), "close": (close_for or {}).get(d, 400.0)}
        if split_on and d in split_on:
            bar["split"] = split_on[d]
        out.append(bar)
    return json.dumps(out)


def test_settlement_daily_bars(tmp_path, subtests):
    session = _past_session(40)
    nearby = _sessions(session - timedelta(days=10), session + timedelta(days=150))
    after = [d for d in nearby if d > session]
    ready = _session_close(session) + timedelta(minutes=60)
    saturday = next(session + timedelta(days=i) for i in range(1, 7) if (session + timedelta(days=i)).weekday() == 5)
    friday = saturday - timedelta(days=1)
    weekend_days = _sessions(friday - timedelta(days=10), friday + timedelta(days=10))
    cases = {
        "settles_on_the_raw_session_close": (ready + timedelta(hours=1), session, _bars(nearby, {session: 437.25}), "settled", 437.25, None),
        "bar_not_final_before_the_read_boundary": (ready - timedelta(minutes=1), session, _bars(nearby), "pending", None,
                                                   "settlement close not final before"),
        "missing_expected_session": (ready + timedelta(days=1), session, _bars([d for d in nearby if d < session]), "pending", None,
                                     f"no daily bar for SPY on expected session {session.isoformat()}"),
        "later_bar_never_proves_a_holiday": (ready + timedelta(days=3), session, _bars([d for d in nearby if d != session]), "pending", None,
                                             f"no daily bar for SPY on expected session {session.isoformat()}"),
        "split_after_expiry_holds": (ready + timedelta(days=5), session, _bars(nearby, split_on={after[1]: 2.0}), "pending", None,
                                     "split after expiry: SPY split 2"),
        "recovers_after_a_long_outage": (ready + timedelta(days=120), session, _bars(nearby, {session: 421.5}), "settled", 421.5, None),
        "confirmed_non_session_expiry_uses_the_prior_session": (_session_close(friday) + timedelta(hours=3), saturday,
                                                                _bars(weekend_days, {friday: 433.0}), "settled", 433.0, None),
        "history_read_failure_is_an_error": (ready + timedelta(hours=1), session, "error", "error", None,
                                             "settlement close unavailable: daily history read failed (upstream status 503)"),
    }
    for name in sorted(cases):
        refresh, expiry, bars, status, price, reason = cases[name]
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            proc, records = _run_qp(run_dir, _request(refresh, [_leg("leg", expiry.isoformat(), operation="settle")]), {"QP_DAILY_BARS": bars})
            out = _only_leg(proc)
            assert out["status"] == status, out
            if status == "settled":
                want_session = friday if expiry == saturday else session
                assert out["settlement"] == {"price": price, "basis": "raw_close", "date": want_session.isoformat(), "source": "yahoo_daily_raw_close"}, out
                assert out["session_date"] == want_session.isoformat() and out["non_session_mapped"] is (expiry == saturday), out
            else:
                assert out["reason"].startswith(reason), out
            windows = [r for r in records if r["call"] == "yahoo_history_window"]
            if name == "bar_not_final_before_the_read_boundary":
                assert windows == [], records
                continue
            assert len(windows) == 1, records
            w = windows[0]
            want_session = friday if expiry == saturday else session
            assert w["symbol"] == "SPY" and w["interval"] == "1d", w
            assert w["auto_adjust"] is False and w["back_adjust"] is False and w["actions"] is True, w
            assert w["start"] == (want_session - timedelta(days=7)).isoformat(), w
            assert w["end"] == (max(refresh.date(), want_session) + timedelta(days=1)).isoformat(), w
            assert not any(r["call"] == "yahoo_history" and r.get("period") not in (None, "1d", "90d") for r in records), records


RH_OPTION_URL = "https://api.robinhood.com/options/instruments/opt-1/"


def _rh_routes(market):
    return {
        "GET https://api.robinhood.com/instruments/?symbol=SPY": [{"status": 200, "json": {"results": [{"tradable_chain_id": "chain-spy"}]}}],
        "GET https://api.robinhood.com/options/instruments/?*": [{"status": 200, "json": {"next": None, "results": [{"id": "opt-1", "url": RH_OPTION_URL}]}}],
        "GET " + RH_OPTION_URL: [{"status": 200, "json": {"id": "opt-1", "url": RH_OPTION_URL}}],
        "GET https://api.robinhood.com/marketdata/options/?*": [market],
    }


def _rh_market(**fields):
    row = {"delta": "-0.2", "gamma": "0.01", "theta": "-0.05", "vega": "0.3"}
    row.update(fields)
    return {"status": 200, "json": {"results": [row]}}


def _run_logged_in(tmp_path, routes, stdin, argv=("--platform=robinhood",)):
    routes_path = tmp_path / "routes.json"
    routes_path.write_text(json.dumps(routes))
    log_path = tmp_path / "requests.jsonl"
    env = _clean_env(tmp_path)
    env.update({
        "PYTHONPATH": RH_FIXTURE,
        "RH_OFFLINE_STUB_MODE": "routes",
        "RH_OFFLINE_STUB_ROUTES": str(routes_path),
        "RH_OFFLINE_STUB_LOG": str(log_path),
        "ROBINHOOD_USERNAME": "offline-user",
        "ROBINHOOD_PASSWORD": "offline-pass",
        "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
        "GO_TRADER_ROBINHOOD_SESSION_DIR": str(tmp_path / "robinhood-session"),
    })
    proc = subprocess.run([sys.executable, SCRIPT, *argv], input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180)
    made = [json.loads(ln) for ln in log_path.read_text().splitlines() if ln.strip()] if log_path.exists() else []
    return proc, made


def test_logged_in_marks_come_only_from_the_venue_quote(tmp_path, subtests):
    expiry = _future_session()
    refresh = _session_close(expiry) - timedelta(days=10)
    cases = {
        "venue_quote": (_rh_market(adjusted_mark_price="1.37"), "mark", 1.37, None),
        "explicit_zero_venue_mark": (_rh_market(adjusted_mark_price="0.0000"), "mark", 0.0, None),
        "missing_mark_field": (_rh_market(), "error", None, "option quote unavailable: robinhood mark field missing"),
        "empty_mark_field": (_rh_market(adjusted_mark_price=""), "error", None, "option quote unavailable: robinhood mark field missing"),
        "nan_mark": (_rh_market(adjusted_mark_price="NaN"), "error", None, "option quote unavailable: robinhood mark is not a finite non-negative price"),
        "infinite_greek": (_rh_market(adjusted_mark_price="1.0", delta="inf"), "error", None, "option quote unavailable: robinhood greeks are not finite"),
        "missing_quote_has_no_model_fallback": ({"status": 503, "json": {"detail": "busy"}}, "error", None,
                                                "option quote unavailable: robinhood option market data missing (upstream status 503)"),
    }
    for name in sorted(cases):
        market, status, value, reason = cases[name]
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            proc, _ = _run_logged_in(run_dir, _rh_routes(market), _request(refresh, [_leg("leg", expiry.isoformat())]))
            assert "Starting login process..." in proc.stderr, proc.stderr
            out = _only_leg(proc)
            assert out["status"] == status, out
            if status == "mark":
                assert out["mark"]["source"] == "robinhood_quote" and out["mark"]["value_per_unit"] == value, out
                assert out["mark"]["greeks"] == {"delta": -0.2, "gamma": 0.01, "theta": -0.05, "vega": 0.3}, out
            else:
                assert out["reason"] == reason, out


def test_probe_only_needs_no_login_or_market_data(tmp_path):
    proc, made = _run_logged_in(tmp_path, {}, "", argv=("--platform=robinhood", "--probe-only"))
    assert proc.returncode == 0, proc.stderr
    assert json.loads(proc.stdout) == {"contract": "option_marks_v1", "platform": "robinhood", "probe": True}
    assert made == [], made
    assert "Starting login process..." not in proc.stderr
    assert not (tmp_path / "robinhood-session").exists()
