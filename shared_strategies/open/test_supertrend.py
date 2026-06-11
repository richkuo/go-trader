"""Regression tests for supertrend_strategy in registry.py.

Issue #961: the supertrend band recursion seeded from the NaN rows of the
rolling ATR warmup; NaN comparisons are always False, so the bands carried
NaN forward forever and the strategy emitted zero signals on any data.
These tests pin the fixed behavior: the recursion seeds from the first
non-NaN ATR row and emits direction-flip signals on trending data.
"""

import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))


def _load_registry():
    spec = importlib.util.spec_from_file_location(
        "_registry_supertrend", os.path.join(_HERE, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_registry()


def make_ohlcv(closes, index=None, noise=0.5):
    closes = np.array(closes, dtype=float)
    n = len(closes)
    df = pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, 100.0),
    })
    if index is not None:
        df.index = index
    return df


def _three_leg_trend_df():
    """Deterministic up -> down -> up trend with a DatetimeIndex (300 bars)."""
    closes = np.concatenate([
        np.linspace(100, 200, 100),
        np.linspace(200, 120, 100),
        np.linspace(120, 220, 100),
    ])
    idx = pd.date_range("2025-01-01", periods=len(closes), freq="1h")
    return make_ohlcv(closes, index=idx)


def test_supertrend_emits_signals_on_trending_data(registry):
    """Regression for #961: must emit nonzero buy AND sell signals."""
    res = registry.supertrend_strategy(_three_leg_trend_df())
    signals = res["signal"].to_numpy()
    assert (signals == 1).sum() > 0, "no buy signals on trending data"
    assert (signals == -1).sum() > 0, "no sell signals on trending data"


def test_supertrend_exact_signal_values_and_positions(registry):
    """Assert the actual signal values: one flip per trend leg, at known bars."""
    res = registry.supertrend_strategy(_three_leg_trend_df())
    signals = res["signal"].to_numpy()
    buy_idx = list(np.where(signals == 1)[0])
    sell_idx = list(np.where(signals == -1)[0])
    # Uptrend confirms after the 10-bar ATR warmup, the downtrend leg flips
    # short, the second uptrend flips long again.
    assert buy_idx == [14, 204]
    assert sell_idx == [106]
    assert res["signal"].iloc[14] == 1
    assert res["signal"].iloc[106] == -1
    assert res["signal"].iloc[204] == 1
    # Direction tracks each leg between flips.
    assert res["st_direction"].iloc[50] == 1
    assert res["st_direction"].iloc[150] == -1
    assert res["st_direction"].iloc[280] == 1


def test_supertrend_bands_escape_nan_warmup(registry):
    """The supertrend line must be NaN only during the ATR warmup (atr_period-1
    bars), not forever — the pre-fix failure mode was an all-NaN band."""
    res = registry.supertrend_strategy(_three_leg_trend_df())
    st = res["supertrend"]
    assert int(st.isna().sum()) == 9  # atr_period=10 -> 9 warmup bars
    assert st.iloc[9:].notna().all()


def test_supertrend_all_nan_atr_returns_no_signals(registry):
    """Inputs shorter than the ATR window must return cleanly with zero signals."""
    df = make_ohlcv(np.linspace(100, 110, 5))
    res = registry.supertrend_strategy(df)
    assert (res["signal"] == 0).all()
    assert res["supertrend"].isna().all()
