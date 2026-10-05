import os

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator
from eval_windows import run_leg
from parity_diff import compute_parity_frame, summarize

NAME = "vortex_trend"
PARAMS = {"period": 2, "min_separation": 0.0}
TIME_STOP = [{"name": "time_stop", "params": {"max_bars": 3}}]
_MANIFEST = os.path.abspath(os.path.join(
    os.path.dirname(__file__), "..", "candidates", "vortex_trend_1647", "study_manifest.json"))


def _frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _flat(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px, 1.0)] * n


def _signals(df, params=PARAMS):
    reg = load_registry("futures")
    return ensure_atr_indicator(reg.apply_strategy(NAME, df, params))


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=TIME_STOP, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def test_long_entry_fills_next_bar_open_and_time_stop_owns_exit():
    rows = _flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    rows += [(102.5, 103.5, 102.0, 103.0, 1.0)] * 8
    df = _frame(rows)
    sig = _signals(df)
    assert sig["signal"].iloc[20] == 1
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert len(trades) == 1
    t = trades[0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[21])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[21] * 1.0005)
    assert t["exit_reason"].startswith("time_stop:")


def test_short_mirror_and_no_reversal_while_open():
    rows = _flat(20) + [(100.0, 100.0, 97.0, 98.0, 1.0)]
    rows += [(97.5, 98.0, 96.0, 97.0, 1.0)] * 2
    rows += [(97.0, 112.0, 96.5, 111.0, 1.0)]
    rows += [(111.0, 112.0, 110.0, 111.5, 1.0)] * 6
    df = _frame(rows)
    sig = _signals(df)
    assert sig["signal"].iloc[20] == -1
    assert sig["signal"].iloc[23] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=0.5,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 6}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[21])
    assert t["exit_reason"].startswith("time_stop:")
    assert t["exit_date"] == str(df.index[27])


def test_invalid_entry_data_keeps_close_and_stop_processing():
    rows = _flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    rows += [(102.5, 103.5, 102.0, float("nan"), 1.0)] * 6
    df = _frame(rows)
    sig = _signals(df)
    assert (sig["vortex_reason"].iloc[21:] == "nonfinite_input").all()
    assert (sig["signal"].iloc[21:] == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("time_stop:")

    crash = _flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    crash += [(101.0, 101.5, 80.0, 81.0, 1.0)]
    crash += [(81.0, 81.5, 79.0, float("nan"), 1.0)] * 4
    sig2 = _signals(_frame(crash))
    res2 = _bt(close_strategies=[{"name": "time_stop", "params": {"max_bars": 50}}]).run(
        sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


def _market(n=420, seed=1647):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 8 * np.sin(t / 30.0) + np.cumsum(rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.8 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


@pytest.mark.parametrize("params", [None, {"period": 28, "min_separation": 0.05}])
def test_non_batched_parity_frame_matches_on_the_bounded_check_window(params):
    df = _market()
    frame = compute_parity_frame(
        df, NAME, params=params, registry="futures",
        close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
        comparison_mode="approximate")
    assert len(frame) == len(df) - 199
    assert bool(frame["match"].all())
    assert summarize(frame)["close_parity"] == "incomplete" and not summarize(frame)["clean"]
    assert (frame["bt_signal"] != 0).sum() > 0
    assert set(frame["live_open_action"]) >= {"long", "short"}


def test_manifest_leg_books_funding_and_uses_the_execution_spec():
    import offline_manifest as om
    manifest = om.load_manifest(_MANIFEST)
    reg = load_registry("futures")
    leg = run_leg(reg, NAME, {"period": 14, "min_separation": 0.0}, "BTC", "4h",
                  (None, None), capital=1000.0,
                  close_strategies=[{"name": "time_stop", "params": {"max_bars": 20}}],
                  direction="both", stop_loss_atr_mult=1.0,
                  manifest_ctx={"manifest": manifest, "window": "test", "cost_multiplier": 1.0},
                  comparison_mode="approximate")
    info = leg["manifest"]
    assert info["cost_model"] == "execution_spec"
    assert info["candle_coverage"]["complete"] is True
    assert info["funding_coverage"]["complete"] is True
    assert info["execution_spec"]["size_decimals"] == 5
    ex = leg["execution"]
    assert ex["positions"] > 0
    assert ex["long_positions"] > 0 and ex["short_positions"] > 0
    assert ex["funding_pnl_usd"] != 0.0
    assert ex["fees_usd"] > 0.0
