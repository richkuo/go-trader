import contextlib
import json
import math
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(_REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from parity_diff import _ENGINE_SLIPPAGE_PCT, ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "chande_momentum"
H = 3_600_000
PARAMS = {"period": 5, "threshold": 25.0, "expected_interval_ms": H}
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
COLS = ["signal", "cmo_reason", "cmo_valid", "cmo", "cmo_prev", "cmo_gain_sum", "cmo_loss_sum",
        "cmo_close", "cmo_eval_ts_ms"]


def _reg():
    return load_registry("futures")


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


def _apply(df, params=PARAMS):
    return _reg().apply_strategy(NAME, df, dict(params))


def _frame(closes, freq="1h", start="2026-01-01", index=None):
    closes = np.asarray(closes, dtype=float)
    opens = np.concatenate([[closes[0]], closes[:-1]])
    with np.errstate(invalid="ignore"):
        high = np.fmax(opens, closes) + 0.5
        low = np.fmin(opens, closes) - 0.5
    df = pd.DataFrame({"open": opens, "high": high, "low": low, "close": closes, "volume": 1.0})
    df.index = index if index is not None else pd.date_range(start, periods=len(df), freq=freq)
    return df


def _walk(n, seed=1661, base=100.0):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    return np.round(base + 6.0 * np.sin(t / 9.0) + np.cumsum(rng.randn(n) * 0.8), 2)


def _ref(closes, period, i):
    ups = sum(max(closes[j] - closes[j - 1], 0.0) for j in range(i - period + 1, i + 1))
    downs = sum(max(closes[j - 1] - closes[j], 0.0) for j in range(i - period + 1, i + 1))
    osc = 0.0 if ups + downs == 0 else 100.0 * (ups - downs) / (ups + downs)
    return ups, downs, osc


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and isinstance(y, float) and math.isnan(x) and math.isnan(y):
            continue
        assert x == y, (col, x, y)


DEFECTS = {
    "nonfinite_close": ("nonfinite_close", 2),
    "infinite_close": ("nonfinite_close", 2),
    "nonpositive_close": ("nonpositive_close", 2),
    "negative_close": ("nonpositive_close", 2),
    "missing_timestamp": ("missing_timestamp", 2),
    "duplicate_timestamp": ("duplicate_timestamp", 2),
    "descending_timestamp": ("descending_timestamp", 2),
    "missing_bar": ("cadence_gap", 1),
}


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[TP], stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _run_bt(df, **kw):
    sig = ensure_atr_indicator(_apply(df))
    before = sig.copy()
    return before, _bt(**kw).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)


def _check_long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit():
    closes = [100.0] * 30 + [100.5, 101.0, 101.4, 101.8, 102.6, 103.4, 104.6, 105.5, 106.0]
    df = _frame(closes)
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == 1)[0]) == 30
    t = res["trades"][0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[31])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[31] * (1 + _ENGINE_SLIPPAGE_PCT))
    assert t["exit_reason"].startswith("tiered_tp_pct") or "tp" in t["exit_reason"]


def _check_short_entry_fills_at_the_next_bar_open():
    closes = [100.0] * 30 + [99.5, 99.0, 98.6, 98.2, 97.4, 96.6, 95.4, 94.5, 94.0]
    df = _frame(closes)
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == -1)[0]) == 30
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[31])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[31] * (1 - _ENGINE_SLIPPAGE_PCT))


def _check_opposite_and_repeated_signals_never_reverse_or_add_while_open():
    closes = ([100.0] * 30 + [100.4, 100.8, 101.0, 101.1, 101.2] + [100.9, 100.6, 100.3, 100.0, 99.8]
              + [100.2, 100.6, 101.0, 101.2] + [101.0] * 30)
    df = _frame(closes)
    sig, res = _run_bt(df, stop_loss_atr_mult=None, stop_loss_pct=50.0,
                       close_strategies=[{"name": "tiered_tp_pct",
                                          "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}])
    s = sig["signal"].to_numpy()
    assert (s == 1).sum() >= 2 and (s == -1).sum() >= 1
    trades = res["trades"]
    assert len(trades) == 1
    assert trades[0]["side"] == "long"
    assert trades[0]["shares"] == pytest.approx(trades[0].get("initial_shares", trades[0]["shares"]))


def test_simulator_fills_next_open_and_never_reverses_or_adds(subtests):
    for label, check in (
        ("long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit", _check_long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit),
        ("short_entry_fills_at_the_next_bar_open", _check_short_entry_fills_at_the_next_bar_open),
        ("opposite_and_repeated_signals_never_reverse_or_add_while_open", _check_opposite_and_repeated_signals_never_reverse_or_add_while_open),
    ):
        with subtests.test(case=label):
            check()


def test_composer_suppresses_entries_while_open_and_keeps_close_on_invalid_entry_data():
    fut = _reg()
    closes = [100.0] * 10 + [100.5, 101.0]
    crossing = _frame(closes)
    pos = {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    open_eval = evaluate_open_close(fut.apply_strategy, fut.get_strategy, crossing.iloc[:11], NAME, None,
                                    ["tiered_tp_pct"], "long", dict(PARAMS), pos, close_evaluate=close_evaluate,
                                    market_ctx={"mark_price": 100.5},
                                    close_params_by_name={"tiered_tp_pct": TP["params"]})
    assert open_eval.open_signal == 1
    decided = finalize_decision(open_eval, "long")
    assert decided["signal"] == 0 and decided["close_fraction"] == 0.0

    gapped = crossing.drop(crossing.index[9])
    held = evaluate_open_close(fut.apply_strategy, fut.get_strategy, gapped, NAME, None, ["tiered_tp_pct"],
                               "long", dict(PARAMS), pos, close_evaluate=close_evaluate,
                               market_ctx={"mark_price": 104.0},
                               close_params_by_name={"tiered_tp_pct": TP["params"]})
    assert held.open_signal == 0
    assert held.open_result_df["cmo_reason"].iloc[-1] == "cadence_gap"
    out = finalize_decision(held, "long")
    assert out["close_fraction"] == 1.0 and out["close_strategy"] == "tiered_tp_pct" and out["signal"] == -1

    flat = finalize_decision(evaluate_open_close(fut.apply_strategy, fut.get_strategy, crossing.iloc[:11], NAME,
                                                 None, ["tiered_tp_pct"], "", dict(PARAMS), {},
                                                 close_evaluate=close_evaluate, market_ctx={"mark_price": 100.5},
                                                 close_params_by_name={"tiered_tp_pct": TP["params"]}), "")
    assert flat["open_action"] == "long" and flat["signal"] == 1


def _market(n=420, seed=16610):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 25 * np.sin(t / 11.0) + np.cumsum(rng.randn(n) * 3.0), 2)
    df = _frame(close, start="2025-03-01")
    df["high"] = df["high"] + np.round(np.abs(rng.randn(n)) * 2.0, 2)
    df["low"] = df["low"] - np.round(np.abs(rng.randn(n)) * 2.0, 2)
    return df


def _parity_cfg(**kw):
    base = dict(strategy_name=NAME, params={"period": 10, "threshold": 25.0, "expected_interval_ms": H},
                registry="futures", platform="hyperliquid", symbol="BTC", timeframe="1h",
                close_refs=[TP], direction="both")
    base.update(kw)
    return ParityConfig(**base)


def test_period_198_parity_at_the_window_boundary():
    cfg = _parity_cfg(params={"period": 198, "threshold": 10.0, "expected_interval_ms": H})
    frame = compute_parity_frame(_market(260), cfg=cfg, window=200)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0 and s["clean"] is True


RUN_BACKTEST = os.path.join(_REPO, "backtest", "run_backtest.py")
_TIMEFRAME_FREQ = {"1h": "1h", "4h": "4h", "1w": "7D"}


@pytest.fixture(scope="module")
def cached_candles(tmp_path_factory):
    import storage
    db = str(tmp_path_factory.mktemp("cmo_interval") / "ohlcv.db")
    storage.init_db(db)
    for timeframe, freq in _TIMEFRAME_FREQ.items():
        df = _frame(_walk(600), freq=freq, start="2025-01-01")
        rows = df.reset_index(drop=True)
        rows.insert(0, "timestamp", (df.index.astype("int64") // 10**6).astype("int64"))
        storage.store_ohlcv(rows, "binanceus", "BTC/USDT", timeframe, db_path=db)
    env = dict(os.environ)
    env["GO_TRADER_OHLCV_CACHE_DB"] = db
    return env


def _run_backtest_cli(env, strategy, timeframe):
    cmd = [sys.executable, RUN_BACKTEST, "--mode", "single", "--strategy", strategy, "--registry", "futures",
           "--platform", "hyperliquid", "--symbol", "BTC/USDT", "--since", "2025-01-01", "--timeframe", timeframe,
           "--funding", "off", "--direction", "both", "--close-strategy", json.dumps(TP),
           "--stop-loss-atr-mult", "1.0"]
    return subprocess.run(cmd, cwd=_REPO, env=env, capture_output=True, text=True, timeout=600)


def _printed_params(stdout):
    line = next(l for l in stdout.splitlines() if l.strip().startswith("Params:"))
    return line.split("Params:", 1)[1].strip()


def _check_run_backtest_binds_the_interval_from_a_non_hourly_timeframe(cached_candles):
    proc = _run_backtest_cli(cached_candles, NAME, "4h")
    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert "'expected_interval_ms': 14400000" in _printed_params(proc.stdout)
    trades = [l for l in proc.stdout.splitlines() if "Total Trades:" in l]
    assert trades and int(trades[0].split(":")[1]) > 0


def _check_run_backtest_refuses_an_explicit_interval_that_disagrees_with_the_timeframe(cached_candles):
    snippet = (
        "import sys; sys.path[:0] = ['backtest', 'shared_tools']\n"
        "from run_backtest import run_single_backtest\n"
        "run_single_backtest('chande_momentum', 'BTC/USDT', '4h', '2025-01-01', "
        "params={'expected_interval_ms': 3600000}, registry='futures', platform='hyperliquid', "
        "funding_mode='off')\n"
    )
    proc = subprocess.run([sys.executable, "-c", snippet], cwd=_REPO, env=cached_candles,
                          capture_output=True, text=True, timeout=600)
    assert proc.returncode != 0
    assert "chande_momentum: expected_interval_ms=3600000 does not match timeframe '4h' (14400000 ms)" in proc.stderr


def _check_strategies_without_the_interval_parameter_run_unchanged_on_any_timeframe(cached_candles):
    proc = _run_backtest_cli(cached_candles, "sma_crossover", "1w")
    assert proc.returncode == 0, proc.stderr + proc.stdout
    defaults = load_registry("futures").STRATEGY_REGISTRY["sma_crossover"]["default_params"]
    assert _printed_params(proc.stdout) == str(defaults)
    assert "expected_interval_ms" not in proc.stdout


def test_run_backtest_binds_or_refuses_the_interval_from_the_timeframe(subtests, cached_candles):
    for label, check in (
        ("run_backtest_binds_the_interval_from_a_non_hourly_timeframe", lambda: _check_run_backtest_binds_the_interval_from_a_non_hourly_timeframe(cached_candles)),
        ("run_backtest_refuses_an_explicit_interval_that_disagrees_with_the_timeframe", lambda: _check_run_backtest_refuses_an_explicit_interval_that_disagrees_with_the_timeframe(cached_candles)),
        ("strategies_without_the_interval_parameter_run_unchanged_on_any_timeframe", lambda: _check_strategies_without_the_interval_parameter_run_unchanged_on_any_timeframe(cached_candles)),
    ):
        with subtests.test(case=label):
            check()


def _walk_forward(df, grid, timeframe, capsys):
    from optimizer import walk_forward_optimize
    result = walk_forward_optimize(df, NAME, grid, n_splits=3, timeframe=timeframe, registry="futures",
                                   platform="hyperliquid", verbose=True, close_strategies=[TP],
                                   stop_loss_atr_mult=1.0, direction="long")
    warmup = [l for l in capsys.readouterr().out.splitlines() if "Warmup bars per fold:" in l]
    return result, warmup


def test_walk_forward_warmup_ignores_the_interval_and_binds_it_from_the_timeframe(capsys):
    hourly = _frame(_walk(900), freq="1h", start="2025-01-01")
    base = {"period": [20], "threshold": [25.0]}
    _, without = _walk_forward(hourly, base, "1h", capsys)
    _, with_interval = _walk_forward(hourly, dict(base, expected_interval_ms=[H]), "1h", capsys)
    assert without == with_interval == ["  Warmup bars per fold: 20"]

    from optimizer import DEFAULT_PARAM_RANGES
    four_hour = _frame(_walk(900), freq="4h", start="2025-01-01")
    result, _ = _walk_forward(four_hour, DEFAULT_PARAM_RANGES[NAME], "4h", capsys)
    assert result["n_valid_folds"] == 3
    assert all(w["best_params"]["expected_interval_ms"] == 4 * H for w in result["window_results"])
