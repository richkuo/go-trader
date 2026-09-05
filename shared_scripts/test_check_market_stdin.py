import importlib.util
import json
import os
import subprocess
import sys

import pytest


_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(_REPO_ROOT, "shared_scripts", "check_hyperliquid.py")
CHECK_REGIME = os.path.join(_REPO_ROOT, "shared_scripts", "check_regime.py")


def _candles(n=160, start_ms=1_700_000_000_000, step_ms=3_600_000):
    out = []
    price = 100.0
    for i in range(n):
        drift = 0.35 if i < n // 2 else -0.2
        price = price + drift + (0.6 if i % 7 == 0 else -0.25)
        out.append([start_ms + i * step_ms, price - 0.3, price + 1.2, price - 1.1, price, 1000.0 + i])
    return out


def _frame(rows, required=200):
    return {
        "rows": rows,
        "required": required,
        "bars": len(rows),
        "coverage_short": False,
        "first_open_ms": rows[0][0],
        "last_open_ms": rows[-1][0],
        "last_close_ms": rows[-1][0],
        "last_recv_at_ms": 1_700_000_000_000,
        "source": "ws",
        "ready": True,
        "forming_bar_included": True,
    }


def _envelope(frames, mid=25_000.0):
    return {
        "v": 2,
        "market": {
            "version": 1,
            "snapshot_id": "300s/1700000000",
            "generation": 4,
            "sealed_at_ms": 1_700_000_000_000,
            "frames": frames,
            "mids": {"BTC": {"px": mid, "recv_at_ms": 1_700_000_000_000,
                             "source": "ws", "age_ms": 0, "stale": False, "confirmed": True}},
            "feed_complete": True,
        },
    }


def _run(script, argv, stdin_payload, env_extra=None):
    env = dict(os.environ)
    env["HYPERLIQUID_SECRET_KEY"] = ""
    env["HYPERLIQUID_ACCOUNT_ADDRESS"] = ""
    if env_extra:
        env.update(env_extra)
    proc = subprocess.run(
        [sys.executable, script] + argv,
        input=json.dumps(stdin_payload),
        capture_output=True, text=True, cwd=_REPO_ROOT, env=env, timeout=180,
    )
    return proc


def test_market_stdin_single_check_uses_the_sealed_frame():
    rows = _candles()
    proc = _run(CHECK_HL, [
        "breakout", "BTC", "1h", "--mode=paper", "--market-stdin",
    ], _envelope({"BTC|1h": _frame(rows)}))
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert out.get("error") is None or out.get("error") == ""
    assert out["symbol"] == "BTC"
    assert out["timeframe"] == "1h"
    assert out["price"] == round(25_000.0, 2)
    assert "Fetching" not in proc.stderr


def test_market_stdin_single_check_refuses_a_missing_frame():
    proc = _run(CHECK_HL, [
        "breakout", "BTC", "1h", "--mode=paper", "--market-stdin",
    ], _envelope({"ETH|1h": _frame(_candles())}))
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    assert "market payload" in out["error"]


def test_market_stdin_single_check_refuses_a_v1_envelope():
    proc = _run(CHECK_HL, [
        "breakout", "BTC", "1h", "--mode=paper", "--market-stdin",
    ], {"v": 1, "market": _envelope({"BTC|1h": _frame(_candles())})["market"]})
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    assert "envelope version" in out["error"]


def test_check_regime_market_stdin_uses_the_sealed_frame():
    rows = _candles()
    proc = _run(CHECK_REGIME, [
        "--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h",
        "--regime-windows-spec-json", '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}',
        "--ohlcv-limit", "200", "--min-bars", "30", "--market-stdin",
    ], _envelope({"BTC|1h": _frame(rows)}))
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert out["symbol"] == "BTC"
    assert out["timeframe"] == "1h"
    assert out["regime"]["default"]["regime"]


def test_check_regime_market_stdin_refuses_a_missing_frame():
    proc = _run(CHECK_REGIME, [
        "--platform=hyperliquid", "--symbol=BTC", "--timeframe=4h",
        "--regime-windows-spec-json", '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}',
        "--ohlcv-limit", "200", "--min-bars", "30", "--market-stdin",
    ], _envelope({"BTC|1h": _frame(_candles())}))
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    assert "market payload" in out["error"]


def test_check_regime_probe_validates_the_market_flag():
    proc = subprocess.run(
        [sys.executable, CHECK_REGIME,
         "--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h",
         "--regime-windows-spec-json", '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}',
         "--ohlcv-limit", "200", "--min-bars", "30", "--market-stdin", "--probe-only"],
        capture_output=True, text=True, cwd=_REPO_ROOT, timeout=60,
    )
    assert proc.returncode == 0, proc.stderr
    rejected = subprocess.run(
        [sys.executable, CHECK_REGIME,
         "--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h",
         "--regime-windows-spec-json", "{}",
         "--not-a-real-flag", "--probe-only"],
        capture_output=True, text=True, cwd=_REPO_ROOT, timeout=60,
    )
    assert rejected.returncode != 0
