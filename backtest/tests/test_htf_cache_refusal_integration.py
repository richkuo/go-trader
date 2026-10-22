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

_SEED = """
import os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "shared_tools"))
import pandas as pd
from storage import store_ohlcv

def rows(n, step, start, drift, flat=False):
    out = []
    p = 100.0
    for i in range(n):
        if not flat:
            p += drift + (0.5 if i %% 3 else -0.4)
        out.append({"timestamp": start + i * step, "open": p - 0.2, "high": p + 1, "low": p - 1, "close": p, "volume": 10.0})
    return pd.DataFrame(out)

day, start = %d, %d
store_ohlcv(rows(400, day, start, 0.3), "binanceus", "BTC/USDT", "1d")
n, flat = int(sys.argv[2]), sys.argv[3] == "flat"
if n:
    store_ohlcv(rows(n, 7 * day, start - 7 * day * max(0, n - 57), 1.0, flat), "binanceus", "BTC/USDT", "1w")
""" % (_DAY_MS, _START_MS)


def _env(tmp_path):
    fixture = tmp_path / "offline.json"
    fixture.write_text("{}")
    env = dict(os.environ)
    env.update({"GO_TRADER_OHLCV_CACHE_DB": str(tmp_path / "cache.db"),
                "PYTHONPATH": _OFFLINE, "HTF_OFFLINE_FIXTURE": str(fixture)})
    return env


def _seed(tmp_path, htf_rows, flat=False):
    script = tmp_path / "seed.py"
    script.write_text(_SEED)
    env = _env(tmp_path)
    env.pop("PYTHONPATH")
    subprocess.run([sys.executable, str(script), _ROOT, str(htf_rows), "flat" if flat else "drift"],
                   check=True, env=env, cwd=str(tmp_path), timeout=120)
    return _env(tmp_path)


def _saved_results(tmp_path):
    conn = sqlite3.connect(str(tmp_path / "cache.db"))
    try:
        return conn.execute("SELECT COUNT(*) FROM backtest_results").fetchone()[0]
    except sqlite3.OperationalError:
        return 0
    finally:
        conn.close()


def _backtest(tmp_path, env):
    proc = subprocess.run(
        [sys.executable, os.path.join(_ROOT, "backtest", "run_backtest.py"), "--mode", "single",
         "--strategy", "sma_crossover", "--symbol", "BTC/USDT", "--timeframe", "1d", "--since", "2023-01-01",
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


def _simulate(env, htf_filter):
    payload = {"candles": _candles(), "configs": [{"label": "htf", "config": {
        "type": "spot", "strategy": "sma_crossover", "symbol": "BTC/USDT", "timeframe": "1d", "htf_filter": htf_filter}}]}
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


def test_simulator_neutral_higher_timeframe_trend_keeps_every_marker(tmp_path):
    env = _seed(tmp_path, 60, flat=True)
    proc_on, on = _simulate(env, True)
    proc_off, off = _simulate(env, False)
    assert proc_on.returncode == 0, proc_on.stdout + proc_on.stderr
    assert proc_off.returncode == 0, proc_off.stdout + proc_off.stderr
    assert on["markers"]["htf"]
    assert on["markers"] == off["markers"]
