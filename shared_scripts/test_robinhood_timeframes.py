import json
import os
import subprocess
import sys
import time

import pytest


_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
_OFFLINE = os.path.join(_ROOT, "shared_scripts", "testdata", "htf_offline")
H = 3_600_000
FOUR_H = 4 * H

EXPECTED_YAHOO_REQUEST = {
    "1m": ("1m", "5d"),
    "2m": ("2m", "5d"),
    "5m": ("5m", "5d"),
    "15m": ("15m", "5d"),
    "30m": ("30m", "1mo"),
    "1h": ("1h", "1mo"),
    "60m": ("1h", "1mo"),
    "90m": ("90m", "1mo"),
    "2h": ("1h", "1mo"),
    "4h": ("1h", "3mo"),
    "6h": ("1h", "3mo"),
    "8h": ("1h", "3mo"),
    "12h": ("1h", "6mo"),
    "1d": ("1d", "1y"),
    "1w": ("1wk", "5y"),
    "1M": ("1mo", "max"),
    "1mo": ("1mo", "max"),
}


def _now_hour():
    now = int(time.time() * 1000)
    return now - now % H


def _hourly(start, end, *, skip=(), base=100.0):
    rows = []
    t = start
    i = 0
    while t <= end:
        if t not in skip:
            p = base + (i % 7) * 0.5 - (i % 3) * 0.25
            rows.append([t, p - 0.1, p + 0.6, p - 0.7, p, 10.0 + i])
        t += H
        i += 1
    return rows


def _write_fixture(tmp_path, yahoo, name="fixture"):
    log = tmp_path / f"{name}-yahoo.log"
    path = tmp_path / f"{name}.json"
    path.write_text(json.dumps({"yahoo": yahoo, "yahoo_price": {"BTC-USD": 100.0}, "yahoo_log": str(log)}))
    return str(path), log


def _run(script, argv, fixture):
    env = dict(os.environ)
    env.update({"PYTHONPATH": _OFFLINE, "HTF_OFFLINE_FIXTURE": fixture, "ROBINHOOD_USERNAME": ""})
    proc = subprocess.run([sys.executable, os.path.join(_ROOT, "shared_scripts", script)] + argv,
                          capture_output=True, text=True, cwd=_ROOT, env=env, timeout=240)
    assert "network blocked by the htf offline fixture" not in proc.stderr, proc.stderr
    return proc, json.loads(proc.stdout)


def _requests(log):
    if not log.exists():
        return []
    return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]


def _candles(tmp_path, symbol, timeframe, yahoo, limit=200):
    fixture, log = _write_fixture(tmp_path, yahoo)
    proc, out = _run("fetch_candles.py", ["--platform", "robinhood", "--symbol", symbol,
                                          "--timeframe", timeframe, "--limit", str(limit)], fixture)
    return proc, out, _requests(log)


@pytest.mark.parametrize("timeframe", sorted(EXPECTED_YAHOO_REQUEST))
def test_every_supported_timeframe_requests_its_mapped_yahoo_interval_and_standard_period(tmp_path, timeframe):
    proc, out, requests = _candles(tmp_path, "BTC", timeframe, {})
    assert proc.returncode == 0, proc.stdout + proc.stderr
    interval, period = EXPECTED_YAHOO_REQUEST[timeframe]
    assert requests == [["BTC-USD", interval, period]]
    assert out["candles"] == []


@pytest.mark.parametrize("timeframe", ["3m", "4h ", " 1h", "1wk", "3d", "1H", "5d", "3mo"])
def test_an_unsupported_timeframe_errors_before_any_yahoo_request(tmp_path, timeframe):
    proc, out, requests = _candles(tmp_path, "BTC", timeframe, {})
    assert proc.returncode == 1
    assert f"unsupported Robinhood timeframe {timeframe!r}" in out["error"]
    assert "Yahoo candles serve only 12h, 15m, 1M, 1d, 1h, 1m, 1mo, 1w, 2h, 2m, 30m, 4h, 5m, 60m, 6h, 8h, 90m" in out["error"]
    assert requests == []


def test_an_unsupported_primary_timeframe_is_a_check_error(tmp_path):
    fixture, log = _write_fixture(tmp_path, {})
    proc, out = _run("check_robinhood.py", ["atr_breakout", "BTC", "3m", "--mode=paper"], fixture)
    assert proc.returncode == 1
    assert "unsupported Robinhood timeframe '3m'" in out["error"]
    assert _requests(log) == []


def test_four_hour_bars_are_complete_utc_buckets_built_from_yahoo_hourly_rows(tmp_path):
    now = _now_hour()
    forming = now - now % FOUR_H
    first_full = forming - 70 * FOUR_H
    start = first_full - 2 * H
    gap_bucket = forming - 10 * FOUR_H
    hourly = _hourly(start, now, skip={gap_bucket + H})
    proc, out, requests = _candles(tmp_path, "BTC", "4h", {"BTC-USD|1h": hourly})
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert requests == [["BTC-USD", "1h", "3mo"]]
    assert "dropped 1 incomplete 4h buckets for BTC-USD" in proc.stderr
    times = [c["time"] * 1000 for c in out["candles"]]
    assert all(t % FOUR_H == 0 for t in times)
    assert times[0] == first_full
    assert gap_bucket not in times
    assert times[-1] == forming
    assert len(times) == 70
    bucket = [r for r in hourly if first_full <= r[0] < first_full + FOUR_H]
    first = out["candles"][0]
    assert len(bucket) == 4
    assert first["open"] == pytest.approx(bucket[0][1])
    assert first["high"] == pytest.approx(max(r[2] for r in bucket))
    assert first["low"] == pytest.approx(min(r[3] for r in bucket))
    assert first["close"] == pytest.approx(bucket[-1][4])
    assert first["volume"] == pytest.approx(sum(r[5] for r in bucket))
    last = out["candles"][-1]
    assert last["close"] == pytest.approx(hourly[-1][4])


def test_an_interior_off_grid_crypto_row_is_an_error(tmp_path):
    now = _now_hour()
    hourly = _hourly(now - 100 * H, now)
    hourly[50][0] += 60_000
    proc, out, _ = _candles(tmp_path, "BTC", "1h", {"BTC-USD|1h": hourly})
    assert proc.returncode == 1
    assert "off the 1h grid" in out["error"]


def test_resampled_timeframes_serve_only_crypto_pairs(tmp_path):
    proc, out, requests = _candles(tmp_path, "SPY", "2h", {})
    assert proc.returncode == 1
    assert "only for crypto pairs" in out["error"]
    assert requests == []
    proc, out, requests = _candles(tmp_path, "SPY", "4h", {})
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert requests == [["SPY", "4h", "1y"]]


def test_one_hour_strategy_gets_a_real_four_hour_trend_filter(tmp_path):
    now = _now_hour()
    fixture, log = _write_fixture(tmp_path, {"BTC-USD|1h": _hourly(now - 400 * H, now)})
    proc, out = _run("check_robinhood.py", ["atr_breakout", "BTC", "1h", "--mode=paper", "--htf-filter"], fixture)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert out["htf_filter"]["status"] == "ok"
    assert out["htf_filter"]["timeframe"] == "4h"
    assert out["indicators"]["htf_ema"] > 0
    assert _requests(log) == [["BTC-USD", "1h", "1mo"], ["BTC-USD", "1h", "1mo"]]


SETTLE_MARGIN_MS = 30_000


def _hour_past_settle_window():
    now = int(time.time() * 1000)
    if now % H < SETTLE_MARGIN_MS:
        time.sleep((SETTLE_MARGIN_MS - now % H) / 1000)
    return _now_hour()


def _closed(tmp_path, timeframe, hourly, *extra):
    fixture, _log = _write_fixture(tmp_path, {"BTC-USD|1h": hourly}, name=f"closed-{timeframe}")
    return _run("check_robinhood.py", ["atr_breakout", "BTC", timeframe, "--mode=paper", "--closed-bar-decisions", *extra], fixture)


def test_paper_closed_bar_decision_drops_the_forming_hour(tmp_path):
    now = _hour_past_settle_window()
    proc, out = _closed(tmp_path, "1h", _hourly(now - 300 * H, now))
    assert proc.returncode == 0, proc.stdout + proc.stderr
    meta = out["closed_bar_decision"]
    assert meta["held"] is False
    assert meta["closure_rule"] == "opening_time_fixed_duration"
    assert meta["interval_ms"] == H
    assert meta["forming_rows_dropped"] == 1
    assert meta["bar_open_ms"] == now - H
    assert meta["decision_boundary_ms"] == now


def test_paper_closed_bar_decision_on_resampled_four_hour_bars(tmp_path):
    now = _hour_past_settle_window()
    forming = now - now % FOUR_H
    proc, out = _closed(tmp_path, "4h", _hourly(forming - 240 * FOUR_H, now))
    assert proc.returncode == 0, proc.stdout + proc.stderr
    meta = out["closed_bar_decision"]
    assert meta["held"] is False
    assert meta["interval_ms"] == FOUR_H
    assert meta["forming_rows_dropped"] == 1
    assert meta["bar_open_ms"] == forming - FOUR_H


def test_paper_closed_bar_decision_holds_when_the_newest_completed_bar_is_missing(tmp_path):
    now = _hour_past_settle_window()
    proc, out = _closed(tmp_path, "1h", _hourly(now - 300 * H, now - 2 * H))
    assert proc.returncode == 0, proc.stdout + proc.stderr
    meta = out["closed_bar_decision"]
    assert meta["held"] is True
    assert "newest completed 1h bar" in meta["hold_reason"]
    assert "missing from the Yahoo rows" in meta["hold_reason"]
    assert out["signal"] == 0


def test_paper_closed_bar_decision_holds_until_yahoo_starts_the_next_bar(tmp_path):
    now = _hour_past_settle_window()
    proc, out = _closed(tmp_path, "1h", _hourly(now - 300 * H, now - H))
    assert proc.returncode == 0, proc.stdout + proc.stderr
    meta = out["closed_bar_decision"]
    assert meta["held"] is True
    assert "Yahoo has not started the 1h bar" in meta["hold_reason"]
    assert out["signal"] == 0


def test_one_minute_closed_bar_decisions_are_refused_with_the_measured_reason(tmp_path):
    fixture, log = _write_fixture(tmp_path, {})
    proc, out = _run("check_robinhood.py", ["atr_breakout", "BTC", "1m", "--mode=paper", "--closed-bar-decisions"], fixture)
    assert proc.returncode == 1
    assert "closed_bar_decisions does not support 1m on robinhood" in out["error"]
    assert "zero-volume placeholder" in out["error"]
    assert _requests(log) == []


def test_live_closed_bar_decisions_are_refused(tmp_path):
    fixture, log = _write_fixture(tmp_path, {})
    proc, out = _run("check_robinhood.py", ["atr_breakout", "BTC", "1h", "--mode=live", "--closed-bar-decisions"], fixture)
    assert proc.returncode == 1
    assert "closed_bar_decisions is not supported on live robinhood" in out["error"]
    assert "no Robinhood provider" in out["error"]
    assert _requests(log) == []
