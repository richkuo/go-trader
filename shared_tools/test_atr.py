
import numpy as np

from shared_tools.conftest import load_module, make_ohlcv

_atr_mod = load_module("_atr_test", __file__.replace("test_atr.py", "atr.py"))
ensure_atr_indicator = _atr_mod.ensure_atr_indicator


def _make_close(n: int = 30, seed: int = 42, start: float = 100.0, scale: float = 1.0) -> np.ndarray:
    rng = np.random.default_rng(seed)
    return start + np.cumsum(rng.normal(0, scale, n))


def test_ensure_atr_indicator_threads_method():
    big = make_ohlcv(_make_close(60, seed=7, start=50_000, scale=300), volume=1.0, noise=200)
    simple = ensure_atr_indicator(big.copy(), period=14)["atr"].dropna()
    wilder = ensure_atr_indicator(big.copy(), period=14, method="wilder")["atr"].dropna()
    assert (simple == simple.round(0)).all()
    assert (wilder != wilder.round(0)).any()
