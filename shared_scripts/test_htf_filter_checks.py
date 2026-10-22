import json
import os
import subprocess
import sys
import time

import pytest


_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
_OFFLINE = os.path.join(_ROOT, "shared_scripts", "testdata", "htf_offline")
H = 3_600_000

SCRIPTS = {
    "binanceus": ("check_strategy.py", ["atr_breakout", "BTC/USDT", "1h", "--mode=paper"], "BTC/USDT"),
    "okx-spot": ("check_okx.py", ["atr_breakout", "BTC", "1h", "--mode=paper", "--inst-type=spot"], "BTC"),
    "okx-swap": ("check_okx.py", ["breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap"], "BTC"),
    "robinhood": ("check_robinhood.py", ["atr_breakout", "BTC", "30m", "--mode=paper"], "BTC"),
    "topstep": ("check_topstep.py", ["breakout", "ES", "1h", "--mode=paper"], "ES"),
}

SWALLOWING_ADAPTERS = ("okx-spot", "okx-swap", "robinhood", "topstep")


def _now_hour():
    now = int(time.time() * 1000)
    return now - now % H


def _bars(n, step, end, last=None):
    start = end - (n - 1) * step
    out = []
    p = 100.0
    for i in range(n):
        p += 0.4 if i % 2 == 0 else -0.35
        o, h, low, c = p - 0.1, p + 0.5, p - 0.5, p
        if i == n - 1 and last == "up":
            h, c = p + 12.5, p + 12
        out.append([start + i * step, o, h, low, c, 1000.0 + i])
    return out


def _htf(mode, now):
    step = 4 * H
    if mode == "missing":
        return []
    if mode == "short":
        return _bars(49, step, now)
    if mode == "error":
        return "network_error"
    if mode == "nan":
        return [r[:4] + ["nan"] + r[5:] for r in _bars(60, step, now)]
    if mode == "neutral":
        return [[now - (59 - i) * step, 100.0, 100.0, 100.0, 100.0, 1.0] for i in range(60)]
    raise ValueError(mode)


def _hourly_htf(mode, now):
    step = 4 * H
    if mode == "missing":
        return []
    if mode == "error":
        return "network_error"
    forming = now - now % step
    buckets = 49 if mode == "short" else 60
    hours = (now - (forming - (buckets - 1) * step)) // H + 1
    rows = _bars(hours, H, now)
    if mode == "nan":
        return [r[:4] + ["nan"] + r[5:] for r in rows]
    if mode == "neutral":
        return [[r[0], 100.0, 100.0, 100.0, 100.0, 1.0] for r in rows]
    return rows


def _fixture(tmp_path, htf_mode, *, primary=True, market_open=True):
    now = _now_hour()
    rows = _bars(200, H, now, "up") if primary else []
    htf = _htf(htf_mode, now)
    last = _bars(200, H, now, "up")[-1][4]
    doc = {
        "ccxt": {"BTC/USDT|1h": rows, "BTC/USDT|4h": htf, "BTC/USDT:USDT|1h": rows, "BTC/USDT:USDT|4h": htf},
        "tickers": {"BTC/USDT": last, "BTC/USDT:USDT": last},
        "yahoo": {"BTC-USD|30m": rows, "BTC-USD|1h": _hourly_htf(htf_mode, now), "ES=F|1h": rows, "ES=F|4h": htf},
        "yahoo_price": {"BTC-USD": last, "ES=F": last},
        "topstep_market_open": market_open,
    }
    path = tmp_path / f"fixture-{htf_mode}-{int(primary)}-{int(market_open)}.json"
    path.write_text(json.dumps(doc))
    return str(path)


def _run(script, argv, fixture):
    env = dict(os.environ)
    env.update({"PYTHONPATH": _OFFLINE, "HTF_OFFLINE_FIXTURE": fixture, "OKX_API_KEY": "",
                "ROBINHOOD_USERNAME": "", "TOPSTEP_API_KEY": ""})
    proc = subprocess.run([sys.executable, os.path.join(_ROOT, "shared_scripts", script)] + argv,
                          capture_output=True, text=True, cwd=_ROOT, env=env, timeout=240)
    assert "network blocked by the htf offline fixture" not in proc.stderr, proc.stderr
    return proc


def _check(tmp_path, name, htf_mode, *, htf=True, extra=(), **fixture_kw):
    script, argv, _symbol = SCRIPTS[name]
    argv = list(argv) + list(extra)
    if htf:
        argv.append("--htf-filter")
    proc = _run(script, argv, _fixture(tmp_path, htf_mode, **fixture_kw))
    return proc, json.loads(proc.stdout)


UNAVAILABLE_REASONS = {
    "missing": ("0 4h candles available for {sym} (need 50)", "no 4h candle frame was returned for {sym}"),
    "short": ("49 4h candles available for {sym} (need 50)", "49 4h candles available for {sym} (need 50)"),
    "error": ("fetching {sym} 4h candles failed: NetworkError", "no 4h candle frame was returned for {sym}"),
    "nan": ("60 of 60 4h close values for {sym} are not finite", "60 of 60 4h close values for {sym} are not finite"),
}

RESAMPLED_REASONS = {"nan": "no 4h candle frame was returned for {sym}"}


@pytest.mark.parametrize("name", sorted(SCRIPTS))
@pytest.mark.parametrize("htf_mode", sorted(UNAVAILABLE_REASONS))
def test_unusable_higher_timeframe_input_reports_unavailable_and_keeps_the_open_signal(tmp_path, name, htf_mode):
    symbol = SCRIPTS[name][2]
    proc, out = _check(tmp_path, name, htf_mode)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    direct, swallowed = UNAVAILABLE_REASONS[htf_mode]
    reason = (swallowed if name in SWALLOWING_ADAPTERS else direct)
    if name == "robinhood":
        reason = RESAMPLED_REASONS.get(htf_mode, reason)
    reason = reason.format(sym=symbol)
    block = out["htf_filter"]
    assert block["status"] == "unavailable"
    assert block["timeframe"] == "4h"
    assert block["trend"] == 0
    assert reason in block["reason"]
    assert out["signal"] == 1
    assert not any(k.startswith("htf_") for k in out["indicators"])
    assert proc.stderr.count(f"HTF filter unavailable for {symbol} 4h: ") == 1


@pytest.mark.parametrize("name", sorted(SCRIPTS))
def test_a_successful_neutral_trend_reports_ok_and_keeps_the_signal(tmp_path, name):
    proc, out = _check(tmp_path, name, "neutral")
    assert proc.returncode == 0, proc.stderr + proc.stdout
    off_proc, off = _check(tmp_path, name, "neutral", htf=False)
    assert off_proc.returncode == 0, off_proc.stderr
    assert out["htf_filter"] == {"status": "ok", "timeframe": "4h", "trend": 0, "reason": ""}
    assert out["signal"] == off["signal"] == 1
    assert out["indicators"]["htf_trend"] == 0
    assert "htf_filter" not in off
    assert "HTF filter unavailable" not in proc.stderr


def test_an_excluded_open_strategy_reports_skipped(tmp_path):
    proc = _run("check_okx.py", ["delta_neutral_funding", "BTC", "1h", "--mode=paper", "--inst-type=swap", "--htf-filter"],
                _fixture(tmp_path, "short"))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    block = json.loads(proc.stdout)["htf_filter"]
    assert block["status"] == "skipped"
    assert "delta_neutral_funding" in block["reason"]
    assert "HTF filter unavailable" not in proc.stderr


def test_topstep_closed_market_return_reports_not_evaluated(tmp_path):
    proc, out = _check(tmp_path, "topstep", "short", market_open=False)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert out["market_open"] is False
    assert out["signal"] == 0
    assert out["htf_filter"]["status"] == "not_evaluated"
    assert "market is closed" in out["htf_filter"]["reason"]
    off_proc, off = _check(tmp_path, "topstep", "short", htf=False, market_open=False)
    assert off_proc.returncode == 0
    assert "htf_filter" not in off


@pytest.mark.parametrize("name", sorted(SCRIPTS))
def test_error_payloads_keep_their_failure_handling(tmp_path, name):
    proc, out = _check(tmp_path, name, "short", primary=False)
    assert out["error"], proc.stderr + proc.stdout
    assert "htf_filter" not in out


def test_closed_bar_higher_timeframe_failure_holds_and_reports_unavailable(tmp_path):
    held_tp = ["--strategy-refs", json.dumps({"open": {"name": "atr_breakout", "params": {}},
                                              "closes": [{"name": "tiered_tp_pct", "params": {}}]}),
               "--position-side", "long", "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1"]
    proc, out = _check(tmp_path, "binanceus", "error", extra=["--closed-bar-decisions", *held_tp])
    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert out["closed_bar_decision"]["held"] is True
    assert out["htf_filter"]["status"] == "unavailable"
    assert "BTC/USDT 4h candles failed" in out["htf_filter"]["reason"]
    assert out["close_fraction"] == 1.0
    assert proc.stderr.count("HTF filter unavailable for BTC/USDT 4h: ") == 1


def test_closed_bar_short_higher_timeframe_frame_holds_and_reports_unavailable(tmp_path):
    proc, out = _check(tmp_path, "okx-swap", "short", extra=["--closed-bar-decisions"])
    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert out["closed_bar_decision"]["held"] is True
    assert out["htf_filter"]["status"] == "unavailable"
    assert "insufficient closed history" in out["htf_filter"]["reason"]


def test_closed_bar_valid_higher_timeframe_reports_ok(tmp_path):
    proc, out = _check(tmp_path, "binanceus", "neutral", extra=["--closed-bar-decisions"])
    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert out["closed_bar_decision"]["held"] is False
    assert out["htf_filter"]["status"] == "ok"
