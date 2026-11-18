
import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import make_flat, make_ohlcv

_HERE = os.path.dirname(os.path.abspath(__file__))


def _load_registry():
    spec = importlib.util.spec_from_file_location(
        "_registry_core_signals", os.path.join(_HERE, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_registry()


_FN = {"bollinger_bands": "bollinger_strategy"}


def _apply(registry, name, df, **params):
    fn = getattr(registry, _FN.get(name, name + "_strategy"))
    return fn(df, **params)


def _flat_df(n=200, price=100.0):
    return make_ohlcv(make_flat(n, price), noise=0)


def _dip_and_recover(n_flat=30, depth=20.0, dip_len=20, recover_len=30, start=100.0):
    flat = [start] * n_flat
    dip = list(np.linspace(start, start - depth, dip_len))
    recover = list(np.linspace(start - depth, start + depth / 2, recover_len))
    return np.array(flat + dip + recover)


def _rally_and_fade(n_flat=30, height=20.0, rally_len=20, fade_len=30, start=100.0):
    flat = [start] * n_flat
    rally = list(np.linspace(start, start + height, rally_len))
    fade = list(np.linspace(start + height, start - height / 2, fade_len))
    return np.array(flat + rally + fade)


def _down_then_up(down_n=60, up_n=80, start=150.0, low=100.0, high=200.0):
    return np.concatenate([np.linspace(start, low, down_n), np.linspace(low, high, up_n)])


def _up_then_down(up_n=60, down_n=80, start=100.0, high=150.0, low=60.0):
    return np.concatenate([np.linspace(start, high, up_n), np.linspace(high, low, down_n)])


def test_ema_crossover_sell_on_bearish_cross(registry):
    df = make_ohlcv(_up_then_down())
    out = _apply(registry, "ema_crossover", df)
    assert (out["signal"] == -1).any(), "Expected a sell when fast EMA crosses below slow EMA"


def _stoch_rsi_cycle_df():
    t = np.arange(250)
    return make_ohlcv(100 + 4 * np.sin(2 * np.pi * t / 20), noise=0.05)


def _squeeze_then_breakout(direction=1):
    rng = np.random.RandomState(7)
    coil = 100.0 + rng.randn(80) * 0.05
    burst = 100.0 + direction * np.linspace(0.5, 25.0, 40)
    closes = np.concatenate([coil, burst])
    n = len(closes)
    noise = np.concatenate([np.full(80, 2.0), np.full(40, 2.0)])
    return pd.DataFrame({
        "open": closes,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, 100.0),
    })


