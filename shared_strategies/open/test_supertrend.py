
import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv

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


def _three_leg_trend_df():
    closes = np.concatenate([
        np.linspace(100, 200, 100),
        np.linspace(200, 120, 100),
        np.linspace(120, 220, 100),
    ])
    idx = pd.date_range("2025-01-01", periods=len(closes), freq="1h")
    return make_ohlcv(closes, index=idx)


def test_supertrend_emits_signals_on_trending_data(registry):
    res = registry.supertrend_strategy(_three_leg_trend_df())
    signals = res["signal"].to_numpy()
    assert (signals == 1).sum() > 0, "no buy signals on trending data"
    assert (signals == -1).sum() > 0, "no sell signals on trending data"


def test_supertrend_exact_signal_values_and_positions(registry):
    res = registry.supertrend_strategy(_three_leg_trend_df())
    signals = res["signal"].to_numpy()
    buy_idx = list(np.where(signals == 1)[0])
    sell_idx = list(np.where(signals == -1)[0])
    assert buy_idx == [14, 204]
    assert sell_idx == [106]
    assert res["signal"].iloc[14] == 1
    assert res["signal"].iloc[106] == -1
    assert res["signal"].iloc[204] == 1
    assert res["st_direction"].iloc[50] == 1
    assert res["st_direction"].iloc[150] == -1
    assert res["st_direction"].iloc[280] == 1


def test_supertrend_bands_escape_nan_warmup(registry):
    res = registry.supertrend_strategy(_three_leg_trend_df())
    st = res["supertrend"]
    assert int(st.isna().sum()) == 9
    assert st.iloc[9:].notna().all()


def test_supertrend_all_nan_atr_returns_no_signals(registry):
    df = make_ohlcv(np.linspace(100, 110, 5))
    res = registry.supertrend_strategy(df)
    assert (res["signal"] == 0).all()
    assert res["supertrend"].isna().all()
