import json
import os
import sqlite3
import subprocess
import sys

import pytest


_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
_OFFLINE = os.path.join(_ROOT, "shared_scripts", "testdata", "htf_offline")
_DAY_MS = 86_400_000
_START_MS = 1_672_531_200_000

_HOUR_MS = 3_600_000

_SEED = """
import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "shared_tools"))
import pandas as pd
from storage import store_ohlcv

def rows(n, step, start, drift, flat=False):
    out = []
    p = 100.0
    for i in range(n):
        if not flat:
            p += drift + (0.5 if i % 3 else -0.4)
        out.append({"timestamp": start + i * step, "open": p - 0.2, "high": p + 1, "low": p - 1, "close": p, "volume": 10.0})
    return pd.DataFrame(out)

for spec in json.loads(sys.argv[2]):
    store_ohlcv(rows(spec["n"], spec["step"], spec["start"], spec["drift"], spec["flat"]),
                "binanceus", "BTC/USDT", spec["tf"])
"""


def _series(tf, n, step, start, drift, flat=False):
    return {"tf": tf, "n": n, "step": step, "start": start, "drift": drift, "flat": flat}


def _env(tmp_path, exchange_rows=None):
    fixture = tmp_path / "offline.json"
    fixture.write_text(json.dumps({"ccxt": exchange_rows or {}}))
    env = dict(os.environ)
    env.update({"GO_TRADER_OHLCV_CACHE_DB": str(tmp_path / "cache.db"),
                "PYTHONPATH": _OFFLINE, "HTF_OFFLINE_FIXTURE": str(fixture)})
    return env


def _seed_series(tmp_path, series, exchange_rows=None):
    script = tmp_path / "seed.py"
    script.write_text(_SEED)
    env = _env(tmp_path)
    env.pop("PYTHONPATH")
    subprocess.run([sys.executable, str(script), _ROOT, json.dumps(series)],
                   check=True, env=env, cwd=str(tmp_path), timeout=300)
    return _env(tmp_path, exchange_rows)


def _exchange_rows(n, step, start, drift):
    out = []
    p = 100.0
    for i in range(n):
        p += drift + (0.5 if i % 3 else -0.4)
        out.append([start + i * step, p - 0.2, p + 1, p - 1, p, 10.0])
    return out


def _last_cached_ms(tmp_path, timeframe):
    conn = sqlite3.connect(str(tmp_path / "cache.db"))
    try:
        return conn.execute("SELECT MAX(timestamp) FROM ohlcv WHERE timeframe = ?", (timeframe,)).fetchone()[0]
    finally:
        conn.close()


def _seed(tmp_path, htf_rows, flat=False):
    series = [_series("1d", 400, _DAY_MS, _START_MS, 0.3)]
    if htf_rows:
        series.append(_series("1w", htf_rows, 7 * _DAY_MS, _START_MS - 7 * _DAY_MS * max(0, htf_rows - 57),
                              1.0, flat))
    return _seed_series(tmp_path, series)


def _saved_results(tmp_path):
    conn = sqlite3.connect(str(tmp_path / "cache.db"))
    try:
        return conn.execute("SELECT COUNT(*) FROM backtest_results").fetchone()[0]
    except sqlite3.OperationalError:
        return 0
    finally:
        conn.close()


def _backtest(tmp_path, env, timeframe="1d"):
    proc = subprocess.run(
        [sys.executable, os.path.join(_ROOT, "backtest", "run_backtest.py"), "--mode", "single",
         "--strategy", "sma_crossover", "--symbol", "BTC/USDT", "--timeframe", timeframe, "--since", "2023-01-01",
         "--htf-filter"],
        capture_output=True, text=True, cwd=_ROOT, env=env, timeout=600)
    assert "network blocked by the htf offline fixture" not in proc.stdout + proc.stderr
    return proc


def _candles():
    out = []
    p = 100.0
    for i in range(400):
        p += 0.3 + (0.5 if i % 3 else -0.4)
        out.append({"time": _START_MS // 1000 + i * 86_400, "open": p - 0.2, "high": p + 1, "low": p - 1,
                    "close": p, "volume": 10})
    return out


def _simulate(env, htf_filter, candles=None, timeframe="1d"):
    payload = {"candles": candles or _candles(), "configs": [{"label": "htf", "config": {
        "type": "spot", "strategy": "sma_crossover", "symbol": "BTC/USDT", "timeframe": timeframe,
        "htf_filter": htf_filter}}]}
    proc = subprocess.run([sys.executable, os.path.join(_ROOT, "shared_scripts", "simulate_strategy.py")],
                          input=json.dumps(payload), capture_output=True, text=True, cwd=_ROOT, env=env, timeout=600)
    return proc, json.loads(proc.stdout.strip().splitlines()[-1])


@pytest.mark.parametrize("htf_rows,want", [
    (0, "no cached 1w candles for BTC/USDT (need at least 50)"),
    (49, "49 cached 1w candles for BTC/USDT (need at least 50)"),
])
def test_backtest_refuses_an_empty_or_short_higher_timeframe_cache(tmp_path, htf_rows, want):
    env = _seed(tmp_path, htf_rows)
    proc = _backtest(tmp_path, env)
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "HTF FILTER REFUSED: sma_crossover BTC/USDT" in proc.stdout
    assert want in proc.stdout
    assert _saved_results(tmp_path) == 0


def test_backtest_with_a_valid_higher_timeframe_cache_saves_its_result(tmp_path):
    env = _seed(tmp_path, 60)
    proc = _backtest(tmp_path, env)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "HTF filter: applied (HTF=1w)" in proc.stdout
    assert "Fetching BTC/USDT 1w" not in proc.stdout + proc.stderr
    assert _saved_results(tmp_path) == 1


@pytest.mark.parametrize("htf_rows,want", [
    (0, "no cached 1w candles for BTC/USDT (need at least 50)"),
    (49, "49 cached 1w candles for BTC/USDT (need at least 50)"),
])
def test_simulator_refuses_an_empty_or_short_higher_timeframe_cache(tmp_path, htf_rows, want):
    env = _seed(tmp_path, htf_rows)
    proc, out = _simulate(env, True)
    assert proc.returncode == 1
    assert want in out["error"]
    assert out["markers"] == {}
    assert len(proc.stdout.strip().splitlines()) == 1


def test_simulator_neutral_higher_timeframe_trend_keeps_every_marker(tmp_path):
    env = _seed(tmp_path, 60, flat=True)
    proc_on, on = _simulate(env, True)
    proc_off, off = _simulate(env, False)
    assert proc_on.returncode == 0, proc_on.stdout + proc_on.stderr
    assert proc_off.returncode == 0, proc_off.stdout + proc_off.stderr
    assert on["markers"]["htf"]
    assert on["markers"] == off["markers"]


def test_backtest_refuses_a_higher_timeframe_cache_that_starts_after_the_requested_range(tmp_path):
    htf_start = 1_748_736_000_000
    ltf_rows = (htf_start - _START_MS) // _HOUR_MS + 240
    env = _seed_series(tmp_path, [
        _series("1h", ltf_rows, _HOUR_MS, _START_MS, 0.01),
        _series("4h", 3000, 4 * _HOUR_MS, htf_start, 0.2),
    ], {"BTC/USDT|4h": _exchange_rows(3000, 4 * _HOUR_MS, htf_start, 0.2)})
    proc = _backtest(tmp_path, env, timeframe="1h")
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "HTF FILTER REFUSED: sma_crossover BTC/USDT" in proc.stdout
    assert "cached 4h candles for BTC/USDT span 2025-06-01 00:00:00" in proc.stdout
    assert "does not cover the requested range 2023-01-01 00:00:00" in proc.stdout
    assert "this run fetched 4h candles since 2022-12-23 into the cache" in proc.stdout
    assert "Fetched 3000 candles" in proc.stderr
    assert _saved_results(tmp_path) == 0


def test_simulator_refuses_a_higher_timeframe_cache_that_ends_before_the_candles(tmp_path):
    first_ms = _START_MS + 1000 * _DAY_MS
    htf_last = first_ms + 30 * _DAY_MS - 90 * _DAY_MS
    env = _seed_series(tmp_path, [
        _series("4h", 3000, 4 * _HOUR_MS, htf_last - 2999 * 4 * _HOUR_MS, 0.2),
    ])
    candles = []
    p = 100.0
    for i in range(30 * 24):
        p += 0.05 + (0.5 if i % 3 else -0.4)
        candles.append({"time": first_ms // 1000 + i * 3600, "open": p - 0.2, "high": p + 1, "low": p - 1,
                        "close": p, "volume": 10})
    proc, out = _simulate(env, True, candles=candles, timeframe="1h")
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "does not cover the requested range" in out["error"]
    assert "this run fetched 4h candles since" in out["error"]
    assert "rerun when the exchange serves BTC/USDT 4h candles" in out["error"]
    assert out["markers"] == {}
    assert len(proc.stdout.strip().splitlines()) == 1


def test_simulator_tops_up_a_stale_higher_timeframe_cache_and_runs_filtered(tmp_path):
    first_ms = _START_MS + 1000 * _DAY_MS
    last_ms = first_ms + (30 * 24 - 1) * _HOUR_MS
    htf_last = first_ms + 30 * _DAY_MS - 90 * _DAY_MS
    htf_start = htf_last - 2999 * 4 * _HOUR_MS
    fresh = _exchange_rows(3000 + 90 * 6, 4 * _HOUR_MS, htf_start, 0.2)
    env = _seed_series(tmp_path, [
        _series("4h", 3000, 4 * _HOUR_MS, htf_start, 0.2),
    ], {"BTC/USDT|4h": fresh})
    candles = []
    p = 100.0
    for i in range(30 * 24):
        p += 0.05 + (0.5 if i % 3 else -0.4)
        candles.append({"time": first_ms // 1000 + i * 3600, "open": p - 0.2, "high": p + 1, "low": p - 1,
                        "close": p, "volume": 10})
    proc, out = _simulate(env, True, candles=candles, timeframe="1h")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "error" not in out
    assert "htf" in out["markers"]
    assert len(proc.stdout.strip().splitlines()) == 1
    assert _last_cached_ms(tmp_path, "4h") >= last_ms - 4 * _HOUR_MS
