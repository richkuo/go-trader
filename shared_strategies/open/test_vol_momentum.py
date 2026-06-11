"""Tests for vol_momentum.py — volatility-targeted time-series momentum (#959)."""

import numpy as np
import pandas as pd

from vol_momentum import vol_momentum_core


def make_ohlcv(closes, noise=0.5, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, volume),
    }, index=pd.date_range("2026-01-01", periods=n, freq="1h"))


def make_uptrend_with_plateau():
    """Quiet base → clean ramp → plateau; momentum builds, then decays."""
    return np.concatenate([
        100 + np.random.RandomState(1).randn(40) * 0.3,
        np.linspace(100, 180, 100),
        180 + np.random.RandomState(3).randn(40) * 0.3,
    ])


def make_downtrend_with_plateau():
    return np.concatenate([
        100 + np.random.RandomState(2).randn(40) * 0.3,
        np.linspace(100, 50, 100),
        50 + np.random.RandomState(4).randn(40) * 0.3,
    ])


def make_round_trip_chop():
    """Perfectly periodic oscillation (period 8 divides mom_window 24):
    the 24-bar net move is exactly zero → vol_mom and efficiency are 0."""
    return 100 + np.tile([0, 2, 4, 2, 0, -2, -4, -2], 38)[:300] * 1.5


def test_columns_present():
    out = vol_momentum_core(make_ohlcv(make_uptrend_with_plateau()))
    for col in ("signal", "position", "atr", "vol_mom", "efficiency"):
        assert col in out.columns


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = vol_momentum_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_warmup_returns_no_signal():
    out = vol_momentum_core(make_ohlcv([100.0] * 30))
    assert (out["signal"] == 0).all()
    assert (out["position"] == 0).all()


def test_uptrend_exact_entry_and_decay_exit():
    """One long entry once ATR-normalized momentum and efficiency confirm the
    ramp; one exit when momentum decays on the plateau."""
    out = vol_momentum_core(make_ohlcv(make_uptrend_with_plateau(), noise=0.4))
    assert np.where(out["signal"].values == 1)[0].tolist() == [50]
    assert np.where(out["signal"].values == -1)[0].tolist() == [163]
    assert int((out["position"] == 1).sum()) == 113
    assert out["vol_mom"].iloc[50] > 0.30
    assert out["efficiency"].iloc[50] >= 0.35


def test_downtrend_long_only_stays_flat():
    out = vol_momentum_core(make_ohlcv(make_downtrend_with_plateau(), noise=0.4))
    assert (out["position"] == 0).all()


def test_downtrend_allow_short_enters_short():
    out = vol_momentum_core(
        make_ohlcv(make_downtrend_with_plateau(), noise=0.4), allow_short=True)
    shorts = np.where(out["signal"].values == -1)[0]
    assert shorts.tolist()[0] == 53
    assert int((out["position"] == -1).sum()) == 108
    assert out["vol_mom"].iloc[53] < -0.30


def test_round_trip_chop_never_enters():
    """Zero net move over the window → vol_mom == efficiency == 0 → flat,
    even with shorts allowed. This is the whole point of the normalization."""
    out = vol_momentum_core(make_ohlcv(make_round_trip_chop()), allow_short=True)
    assert (out["position"] == 0).all()
    assert float(out["vol_mom"].abs().max()) == 0.0


def test_impossible_entry_threshold_blocks_entries():
    out = vol_momentum_core(
        make_ohlcv(make_uptrend_with_plateau(), noise=0.4), entry_threshold=10.0)
    assert (out["position"] == 0).all()


def test_impossible_efficiency_gate_blocks_entries():
    out = vol_momentum_core(
        make_ohlcv(make_uptrend_with_plateau(), noise=0.4), eff_entry=1.01)
    assert (out["position"] == 0).all()


def test_efficiency_collapse_exits_position():
    """With the momentum-decay exit disabled (exit_threshold pushed far below
    zero), the plateau's efficiency collapse must still flatten the long."""
    out = vol_momentum_core(
        make_ohlcv(make_uptrend_with_plateau(), noise=0.4),
        exit_threshold=-10.0, eff_exit=0.15)
    assert int(out["position"].iloc[-1]) == 0
    assert (out["position"] == 1).any()


def test_no_lookahead_future_perturbation():
    df = make_ohlcv(make_uptrend_with_plateau(), noise=0.4)
    full = vol_momentum_core(df, allow_short=True)
    cut = 110
    pert = df.copy()
    pert.iloc[cut:, :] = pert.iloc[cut:, :] * 1.7
    out = vol_momentum_core(pert, allow_short=True)
    assert (full["signal"].iloc[:cut].values == out["signal"].iloc[:cut].values).all()
    assert (full["position"].iloc[:cut].values == out["position"].iloc[:cut].values).all()
