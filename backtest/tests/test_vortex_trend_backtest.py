import os

import numpy as np
import pandas as pd

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator
from eval_windows import run_leg

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
