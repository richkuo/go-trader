
import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_ANALOG_RETRIEVAL = load_module("_analog_retrieval_test", __file__.replace("test_analog_retrieval.py", "analog_retrieval.py"))
FEATURE_COLUMNS = _ANALOG_RETRIEVAL.FEATURE_COLUMNS
analog_retrieval_core = _ANALOG_RETRIEVAL.analog_retrieval_core
encode_features = _ANALOG_RETRIEVAL.encode_features
forward_returns = _ANALOG_RETRIEVAL.forward_returns
retrieve_neighbors = _ANALOG_RETRIEVAL.retrieve_neighbors


def _hourly_index(n, start="2026-01-01 00:00:00"):
    return pd.date_range(start, periods=n, freq="1h")


def _ohlcv(closes, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame(
        {
            "open": closes,
            "high": closes + 0.5,
            "low": closes - 0.5,
            "close": closes,
            "volume": np.full(n, float(volume)),
        },
        index=_hourly_index(n),
    )


def _sawtooth_df(n=700, base=100.0, up_bars=40, down_bars=10, step_pct=0.01, seed=7):
    rng = np.random.RandomState(seed)
    closes = [base]
    while len(closes) < n:
        for _ in range(up_bars):
            closes.append(closes[-1] * (1 + step_pct + rng.randn() * 0.001))
        for _ in range(down_bars):
            closes.append(closes[-1] * (1 - step_pct + rng.randn() * 0.001))
    return _ohlcv(closes[:n])


_FAST = dict(
    feat_window=8,
    atr_period=5,
    vol_baseline=20,
    horizon=5,
    k_neighbors=10,
    min_index=30,
    max_index=200,
    min_t_stat=0.5,
    min_edge_atr=0.0,
)


def test_core_fires_and_signals_agree_with_retrieved_mean():
    df = _sawtooth_df(700)
    out = analog_retrieval_core(df, **_FAST)
    fired = out[out["signal"] != 0]
    assert len(fired) > 0, "structured series with loose gates must fire"
    assert set(out["signal"].unique()) <= {-1, 0, 1}
    assert (np.sign(fired["analog_mean_fwd"]) == fired["signal"]).all()
    assert (fired["analog_k"] <= _FAST["k_neighbors"]).all()
    assert fired["analog_t_stat"].abs().min() >= _FAST["min_t_stat"]


