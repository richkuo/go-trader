
import importlib.util
import os

import numpy as np
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv

_HERE = os.path.dirname(os.path.abspath(__file__))


def _load_registry():
    spec = importlib.util.spec_from_file_location(
        "_registry_tema_cross", os.path.join(_HERE, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_registry()


def _oscillating_uptrend(n=200, start=100.0, drift=0.4, amp=3.0, period=20, seed=0):
    t = np.arange(n)
    rng = np.random.RandomState(seed)
    return start + t * drift + amp * np.sin(2 * np.pi * t / period) + rng.randn(n) * 0.05


def _oscillating_downtrend(n=200, start=200.0, drift=0.4, amp=3.0, period=20, seed=0):
    t = np.arange(n)
    rng = np.random.RandomState(seed)
    return start - t * drift + amp * np.sin(2 * np.pi * t / period) + rng.randn(n) * 0.05


def _trend_up_then_down(up_n=200, down_n=200, start=100.0, amp=3.0, period=20):
    up = _oscillating_uptrend(up_n, start=start, amp=amp, period=period)
    down = _oscillating_downtrend(down_n, start=up[-1], amp=amp, period=period)
    return np.concatenate([up, down])


def test_tema_cross_bd_emits_long_in_uptrend(registry):
    prices = _oscillating_uptrend(200, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_bd_strategy(df)
    assert (result["signal"] == 1).any()


