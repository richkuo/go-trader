import contextlib
import math
import os
import sqlite3
import sys

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from closed_bar import FRAME_INTERVAL_ATTR, FRAME_TIMESTAMP_KIND_ATTR, declare_frame_cadence
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "ease_of_movement_breakout"
PARAMS = {"eom_period": 3, "breakout_window": 4}
NEED = 6
DIAGNOSTICS = ("eom_eval_ts_ms", "eom_interval_ms", "eom_mid_change", "eom_raw", "eom_smoothed",
               "eom_prior_high", "eom_prior_low", "eom_setup")
REG = load_registry("futures")


def _frame(rows, freq="1h", kind="open", declare=True):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq=freq)
    if declare:
        declare_frame_cadence(df, freq, kind)
    return df


def _flat(n, px=100.0, volume=1000.0):
    return [(px, px + 1.0, px - 1.0, px, volume)] * n


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _apply(df, params=None):
    return REG.apply_strategy(NAME, df, params or PARAMS)


def _last(df, params=None):
    return _apply(df, params).iloc[-1]


def _breakout_after(prefix_rows):
    return _apply(_frame(prefix_rows + _flat(8) + [(100.0, 104.0, 100.0, 103.5, 1000.0)]))


BAD_ROWS = [
    ((100.0, 101.0, 99.0, 100.0, 0.0), "invalid_volume"),
    ((100.0, 101.0, 99.0, 100.0, -5.0), "invalid_volume"),
    ((100.0, 101.0, 99.0, 100.0, float("inf")), "nonfinite_input"),
    ((100.0, float("nan"), 99.0, 100.0, 1000.0), "nonfinite_input"),
    ((100.0, 101.0, float("-inf"), 100.0, 1000.0), "nonfinite_input"),
    ((100.0, 98.0, 99.0, 98.5, 1000.0), "inconsistent_range"),
    ((100.0, 101.0, 99.0, 101.5, 1000.0), "inconsistent_range"),
    ((1e308, 1.7e308, 1e308, 1.2e308, 1000.0), "nonfinite_derived"),
]


def test_native_close_timestamps_normalize_to_bar_opening_time():
    rows = _flat(10) + [(100.0, 104.0, 100.0, 103.5, 1000.0)]
    df = _frame(rows, kind="native_close_inclusive")
    df.index = df.index + pd.Timedelta(milliseconds=3_600_000 - 1)
    row = _last(df)
    assert row["signal"] == 1
    assert row["eom_eval_ts_ms"] == pd.Timestamp("2026-01-01 10:00").value // 1_000_000
    df.attrs.pop(FRAME_TIMESTAMP_KIND_ATTR)
    unknown = _last(df)
    assert unknown["signal"] == 1 and math.isnan(unknown["eom_eval_ts_ms"])


def _walk(n=320, seed=5):
    rng = np.random.default_rng(seed)
    close = 100.0 + np.cumsum(rng.normal(0, 1.0, n))
    high = close + np.abs(rng.normal(0, 0.8, n))
    low = close - np.abs(rng.normal(0, 0.8, n))
    vol = rng.lognormal(7.0, 0.6, n)
    df = _frame(list(zip(close, high, low, close, vol)))
    return df


def _same(a, b):
    for col in DIAGNOSTICS:
        x, y = a[col], b[col]
        if not ((isinstance(x, float) and math.isnan(x) and math.isnan(y)) or x == y):
            return False
    return (a["signal"] == b["signal"] and a["eom_reason"] == b["eom_reason"]
            and bool(a["eom_valid"]) == bool(b["eom_valid"]))


PREFIX_PARAMS = [{"eom_period": 14, "breakout_window": 20}, {"eom_period": 28, "breakout_window": 60},
                 {"eom_period": 2, "breakout_window": 2}]


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[{"name": "tiered_tp_atr", "params": {}}],
                stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_entry_fills_next_bar_open_one_close_owner_exits_and_an_open_position_never_reverses_or_adds(subtests):
    with subtests.test(case="entry_fill_and_close_owner"):
        _entry_fills_next_bar_open_and_one_close_owner_exits()
    with subtests.test(case="opposite_entry_while_open"):
        _opposite_entry_while_open_never_reverses_or_adds()


def _entry_fills_next_bar_open_and_one_close_owner_exits():
    rows = _flat(20) + [(100.0, 104.0, 100.0, 103.5, 1000.0)]
    rows += [(104.0, 112.0, 103.8, 111.0, 1000.0)] * 6
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df))
    assert sig["signal"].iloc[20] == 1
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    trades = res["trades"]
    assert trades and {t["side"] for t in trades} == {"long"}
    assert {t["entry_date"] for t in trades} == {str(df.index[21])}
    assert trades[0]["entry_price"] == pytest.approx(df["open"].iloc[21] * 1.0005)
    reasons = [t["exit_reason"] for t in trades]
    assert reasons[0].startswith("tiered_tp_atr:")
    assert all(r.startswith("tiered_tp_atr:") or r == "end_of_data" for r in reasons)
    assert res["close_validation"]["mode"] == "strict"


def _opposite_entry_while_open_never_reverses_or_adds():
    rows = _flat(20) + [(100.0, 100.0, 96.0, 96.5, 1000.0)]
    rows += [(96.4, 96.8, 96.0, 96.4, 1000.0)] * 5
    rows += [(96.4, 99.0, 96.2, 98.9, 1000.0)]
    rows += [(98.9, 99.2, 98.6, 99.0, 1000.0)] * 4
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df))
    assert sig["signal"].iloc[20] == -1
    assert sig["signal"].iloc[26] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    entries = {(t["entry_date"], t["side"]) for t in res["trades"]}
    assert entries == {(str(df.index[21]), "short")}

    deps = REG
    evaluation = evaluate_open_close(
        deps.apply_strategy, deps.get_strategy, df.iloc[:27], NAME, None, ["tiered_tp_atr"], "short",
        dict(PARAMS), {"side": "short", "avg_cost": 96.4, "current_quantity": 1.0, "initial_quantity": 1.0,
                       "entry_atr": 1.0},
        close_evaluate=_close_evaluate(), market_ctx={"mark_price": 98.9, "atr": 1.0})
    decision = finalize_decision(evaluation, "short")
    assert evaluation.open_signal == 1
    assert decision["open_action"] == "long" and decision["signal"] == 0
    assert decision["close_strategies"] == ["tiered_tp_atr"]


def _close_evaluate():
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "shared_strategies", "close"))
    from close_registry_loader import evaluate
    return evaluate


def test_invalid_entry_input_keeps_the_stop_working():
    rows = _flat(20) + [(100.0, 104.0, 100.0, 103.5, 1000.0)]
    rows += [(103.0, 103.5, 80.0, 81.0, 0.0)] * 4
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df))
    assert (sig["eom_reason"].iloc[21:] == "invalid_volume").all()
    res = _bt(close_strategies=[{"name": "tiered_tp_atr", "params": {}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    assert res["trades"][0]["exit_reason"] == "sl"


_M1_LEG = """
import json, os, sys
repo = sys.argv[1]
sys.path.insert(0, os.path.join(repo, "backtest"))
sys.path.insert(0, os.path.join(repo, "shared_tools"))
import data_fetcher, funding_fetcher, eval_windows
from registry_loader import load_registry
def refuse(*_a, **_k):
    raise RuntimeError("network disabled in this test")
data_fetcher.fetch_full_history = refuse
funding_fetcher._hl_adapter = refuse
frame = data_fetcher.load_cached_data("BTC/USDT", "1h", start_date="2026-01-01")
leg = eval_windows.run_leg(load_registry("futures"), "ease_of_movement_breakout", None, "BTC/USDT", "1h",
                           ("2026-01-01", "2026-01-17"), close_strategies=[{"name": "tiered_tp_atr"}],
                           direction="both", stop_loss_atr_mult=1.0, keep_positions=True, funding_mode="charge")
print(json.dumps({"attrs": frame.attrs, "leg": leg}, default=str))
"""


def _load_run_study(repo):
    import importlib.util
    path = os.path.join(repo, "backtest", "candidates", "ease_of_movement_1671", "run_study.py")
    spec = importlib.util.spec_from_file_location("eom1671_run_study", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_cached_loader_declares_cadence_and_m1_marks_unpriced_funding_incomplete(tmp_path):
    import subprocess
    import json
    repo = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    sys.path.insert(0, os.path.join(repo, "shared_tools"))
    from storage import init_db
    path = str(tmp_path / "cache.db")
    init_db(path)
    start = int(pd.Timestamp("2026-01-01").value // 1_000_000)
    rng = np.random.default_rng(3)
    rows = []
    px = 100.0
    for i in range(400):
        px = max(px + rng.normal(0, 1.0), 5.0)
        rows.append(("binanceus", "BTC/USDT", "1h", start + i * 3_600_000, px, px + 1.0, px - 1.0, px,
                     float(rng.uniform(500, 1500))))
    conn = sqlite3.connect(path)
    conn.executemany("INSERT INTO ohlcv (exchange, symbol, timeframe, timestamp, open, high, low, close, volume) "
                     "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", rows)
    conn.commit()
    conn.close()
    env = {**os.environ, "GO_TRADER_OHLCV_CACHE_DB": path}
    proc = subprocess.run([sys.executable, "-c", _M1_LEG, repo], capture_output=True, text=True,
                          env=env, cwd=repo, timeout=240)
    assert proc.returncode == 0, proc.stderr[-3000:]
    out = json.loads(proc.stdout.strip().splitlines()[-1])
    assert out["attrs"] == {FRAME_INTERVAL_ATTR: 3_600_000, FRAME_TIMESTAMP_KIND_ATTR: "open"}
    leg = out["leg"]
    assert leg["trades"] > 0 and leg["positions"]
    assert leg["funding_incomplete"] is True

    run_study = _load_run_study(repo)
    agg = run_study._aggregate([run_study._summ_leg(leg)])
    assert agg["funding_incomplete_legs"] == 1
    base = {"scored": agg}
    summary = {
        "candidate_P3_B4": base,
        f"candidate_P3_B4_slip{run_study.SLIPPAGE_STRESS[0]}": base,
        "donchian_breakout_B4": base,
        "chaikin_money_flow_breakout_B4": base,
        "volume_weighted": base,
    }
    verdict = run_study.verdict(summary, "P3_B4", {"eom_period": 3, "breakout_window": 4})
    assert verdict["verdict"] == "inconclusive"
    assert verdict["checks"][0]["ok"] is False
