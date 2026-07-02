"""Tests for analog_retrieval.py — backtest-only k-NN analog research strategy.

The leakage prefix-consistency test here is #1138's dedicated regression test
(the strategy-internal analog of ``backtest/tests/test_backtester_lookahead.py``):
the walk-forward analog index must never contain bars whose forward-return
window extends past the current bar, so every output row must be identical
whether the series ends at that row or continues arbitrarily far beyond it.
"""

import numpy as np
import pandas as pd
import pytest

from analog_retrieval import (
    FEATURE_COLUMNS,
    analog_retrieval_core,
    encode_features,
    forward_returns,
    retrieve_neighbors,
)


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
    """Structured series: long +1%/bar ramps, short -1%/bar dips, light noise.

    Momentum-up states are overwhelmingly followed by further up moves, so an
    analog retriever with loose gates must find positive-mean neighborhoods.
    """
    rng = np.random.RandomState(seed)
    closes = [base]
    while len(closes) < n:
        for _ in range(up_bars):
            closes.append(closes[-1] * (1 + step_pct + rng.randn() * 0.001))
        for _ in range(down_bars):
            closes.append(closes[-1] * (1 - step_pct + rng.randn() * 0.001))
    return _ohlcv(closes[:n])


# Small params so a few hundred bars produce a working index; gates loosened
# so signals actually fire (the leakage test needs live signal rows to bite).
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


# ── encoder ──────────────────────────────────────────────────────────────


def test_encode_features_columns_and_warmup():
    df = _sawtooth_df(120)
    feats = encode_features(df, feat_window=8, atr_period=5, vol_baseline=20)
    assert tuple(feats.columns) == FEATURE_COLUMNS
    assert len(feats) == len(df)
    # Warmup rows are NaN (vol_baseline is the longest lookback)…
    assert feats.iloc[:5].isna().any(axis=1).all()
    # …and past warmup every feature is finite.
    tail = feats.iloc[30:]
    assert np.isfinite(tail.to_numpy()).all()
    # Return efficiency is a path ratio — bounded in [-1, 1].
    assert (tail["ret_eff"].abs() <= 1.0 + 1e-9).all()


def test_encode_features_is_prefix_stable():
    df = _sawtooth_df(150)
    full = encode_features(df, feat_window=8, atr_period=5, vol_baseline=20)
    cut = encode_features(df.iloc[:90], feat_window=8, atr_period=5, vol_baseline=20)
    pd.testing.assert_frame_equal(full.iloc[:90], cut)


# ── forward returns ──────────────────────────────────────────────────────


def test_forward_returns_arithmetic_and_nan_tail():
    close = pd.Series([100.0, 110.0, 121.0, 133.1], index=_hourly_index(4))
    fwd = forward_returns(close, horizon=2)
    assert fwd.iloc[0] == pytest.approx(0.21)
    assert fwd.iloc[1] == pytest.approx(0.21)
    assert fwd.iloc[2:].isna().all()


# ── retrieval ────────────────────────────────────────────────────────────


def test_retrieve_neighbors_orders_by_distance_and_caps_k():
    index = np.array([[0.0, 0.0], [1.0, 1.0], [0.1, 0.1], [5.0, 5.0]])
    query = np.zeros(2)
    nbr = retrieve_neighbors(index, query, k=2)
    assert list(nbr) == [0, 2]
    assert len(retrieve_neighbors(index, query, k=10)) == 4
    assert len(retrieve_neighbors(np.empty((0, 2)), query, k=3)) == 0
    assert len(retrieve_neighbors(index, query, k=0)) == 0


def test_retrieve_neighbors_breaks_ties_by_row_order():
    index = np.array([[1.0, 0.0], [1.0, 0.0], [1.0, 0.0]])
    nbr = retrieve_neighbors(index, np.zeros(2), k=2)
    assert list(nbr) == [0, 1]


# ── core signal behavior ─────────────────────────────────────────────────


def test_core_handles_empty_single_row_and_short_frames():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = analog_retrieval_core(empty, **_FAST)
    assert "signal" in out.columns and len(out) == 0

    single = _ohlcv([100.0])
    out = analog_retrieval_core(single, **_FAST)
    assert list(out["signal"]) == [0]

    short = _sawtooth_df(40)
    out = analog_retrieval_core(short, **_FAST)
    assert (out["signal"] == 0).all()


def test_core_fires_and_signals_agree_with_retrieved_mean():
    df = _sawtooth_df(700)
    out = analog_retrieval_core(df, **_FAST)
    fired = out[out["signal"] != 0]
    assert len(fired) > 0, "structured series with loose gates must fire"
    assert set(out["signal"].unique()) <= {-1, 0, 1}
    # Every fired direction is the sign of the retrieved forward-return mean.
    assert (np.sign(fired["analog_mean_fwd"]) == fired["signal"]).all()
    # Retrieval never exceeds k and diagnostics land where signals fired.
    assert (fired["analog_k"] <= _FAST["k_neighbors"]).all()
    assert fired["analog_t_stat"].abs().min() >= _FAST["min_t_stat"]


def test_core_gates_suppress_all_signals_when_impossible():
    df = _sawtooth_df(700)
    hard_t = analog_retrieval_core(df, **{**_FAST, "min_t_stat": 1e9})
    assert (hard_t["signal"] == 0).all()
    hard_edge = analog_retrieval_core(df, **{**_FAST, "min_edge_atr": 1e9})
    assert (hard_edge["signal"] == 0).all()


def test_core_respects_min_index_before_firing():
    df = _sawtooth_df(700)
    out = analog_retrieval_core(df, **_FAST)
    fired_pos = np.flatnonzero(out["signal"].to_numpy() != 0)
    # A signal at t needs min_index eligible bars at or before t - horizon.
    assert fired_pos.min() >= _FAST["min_index"] + _FAST["horizon"]


def test_core_max_index_caps_the_searched_window():
    df = _sawtooth_df(700)
    capped = analog_retrieval_core(df, **{**_FAST, "max_index": 40})
    # Still functional under the cap…
    assert (capped["analog_k"] <= _FAST["k_neighbors"]).all()
    assert (capped[capped["signal"] != 0].shape[0]) > 0
    # …and the cap changes retrieval vs the wide index (different analog set).
    wide = analog_retrieval_core(df, **{**_FAST, "max_index": 0})
    assert not capped["analog_mean_fwd"].equals(wide["analog_mean_fwd"])


# ── THE leakage regression (#1138) ───────────────────────────────────────


def test_leakage_prefix_consistency_regression():
    """Walk-forward guard: row t's outputs may depend only on bars <= t.

    Truncating the series at any cut must reproduce the full run's rows
    exactly. A leaky index (bars with unrealized forward returns, or
    normalization stats from the full series) breaks this immediately —
    verified red→green by widening the eligibility cut during development.
    """
    df = _sawtooth_df(700)
    full = analog_retrieval_core(df, **_FAST)
    checked_cols = ["signal", "analog_mean_fwd", "analog_t_stat", "analog_k"]
    fired_before_cut = 0
    for cut in (320, 450, 555, 699):
        prefix = analog_retrieval_core(df.iloc[:cut], **_FAST)
        pd.testing.assert_frame_equal(
            full.iloc[:cut][checked_cols], prefix[checked_cols]
        )
        fired_before_cut += int((prefix["signal"] != 0).sum())
    # The comparison must exercise live signal rows, not an all-zero frame.
    assert fired_before_cut > 0
