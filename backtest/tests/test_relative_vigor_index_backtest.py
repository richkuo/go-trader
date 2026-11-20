import numpy as np
import pandas as pd

from atr import ensure_atr_indicator
from backtester import Backtester
from registry_loader import load_registry

NAME = "relative_vigor_index"
PARAMS = {"period": 3, "zero_line_filter": True}
TIME_STOP = [{"name": "time_stop", "params": {"max_bars": 3}}]


def _frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _doji(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px)] * n


def _bar(o, c):
    return (o, max(o, c) + 1.0, min(o, c) - 1.0, c)


def _signals(df, params=PARAMS):
    out = load_registry("futures").apply_strategy(NAME, df, params)
    return ensure_atr_indicator(out)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=TIME_STOP, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def _market(n=420, seed=1666):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 40 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 4.0))
    open_ = np.concatenate([[close[0]], close[:-1]]) + np.round(rng.randn(n) * 2.0)
    span = np.round(np.abs(rng.randn(n)) * 5.0) + 1.0
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": 1.0})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df
