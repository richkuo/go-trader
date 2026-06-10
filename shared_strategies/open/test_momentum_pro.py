"""Tests for momentum_pro.py — trend-pullback momentum strategy."""

import numpy as np
import pandas as pd

from momentum_pro import momentum_pro_core


def make_ohlcv(opens, highs, lows, closes, volume):
    return pd.DataFrame({
        "open": np.asarray(opens, dtype=float),
        "high": np.asarray(highs, dtype=float),
        "low": np.asarray(lows, dtype=float),
        "close": np.asarray(closes, dtype=float),
        "volume": np.asarray(volume, dtype=float),
    })


def build_uptrend_with_pullback():
    """Stacked-EMA uptrend, a pullback that tags EMA(fast), then a resumption
    bar that breaks the prior high on high volume."""
    n = 260
    # Steady uptrend to stack the EMAs and build ADX.
    closes = list(np.linspace(100, 200, n - 6))
    # Pullback: three down bars dipping toward the fast EMA.
    base = closes[-1]
    closes += [base - 4, base - 7, base - 9]
    # Resumption: strong up bar that breaks the prior bar's high.
    closes += [base - 4, base + 6, base + 12]
    closes = np.array(closes, dtype=float)
    n = len(closes)
    highs = closes + 1.0
    lows = closes - 1.0
    opens = closes - 0.3
    # Make the pullback lows actually reach down (so low <= ema_fast can hold).
    vol = np.full(n, 100.0)
    vol[-1] = 100.0
    vol[-2] = 500.0  # volume spike on the resumption bar
    return make_ohlcv(opens, highs, lows, closes, vol)


def test_columns_present():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df)
    for col in ("signal", "ema_fast", "ema_mid", "ema_long", "adx", "vol_sma"):
        assert col in out.columns


def test_warmup_returns_no_signal():
    df = make_ohlcv(
        opens=[100] * 30, highs=[101] * 30, lows=[99] * 30,
        closes=[100] * 30, volume=[100] * 30,
    )
    out = momentum_pro_core(df)
    assert (out["signal"] == 0).all()


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = momentum_pro_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_uptrend_pullback_fires_long():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df, vol_mult=1.2)
    assert (out["signal"] == 1).any(), "expected a long entry on the resumption bar"


def test_volume_gate_blocks_when_unmet():
    """A volume multiplier no bar can satisfy must suppress all entries."""
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df, vol_mult=1e6)
    assert (out["signal"] == 0).all()


def test_flat_market_no_signal():
    """No trend (flat) → ADX gate keeps it out."""
    n = 260
    closes = np.full(n, 100.0) + np.random.RandomState(0).randn(n) * 0.05
    df = make_ohlcv(closes - 0.3, closes + 0.5, closes - 0.5, closes, np.full(n, 100.0))
    out = momentum_pro_core(df)
    assert (out["signal"] == 0).all()


def test_downtrend_pullback_fires_short():
    """Mirror image: stacked bearish EMAs, a rally to EMA(fast), then a
    breakdown through the prior low."""
    n = 260
    closes = list(np.linspace(200, 100, n - 6))
    base = closes[-1]
    closes += [base + 4, base + 7, base + 9]      # rally up into resistance
    closes += [base + 4, base - 6, base - 12]     # breakdown
    closes = np.array(closes, dtype=float)
    n = len(closes)
    highs = closes + 1.0
    lows = closes - 1.0
    opens = closes + 0.3
    vol = np.full(n, 100.0)
    vol[-2] = 500.0
    df = make_ohlcv(opens, highs, lows, closes, vol)
    out = momentum_pro_core(df, vol_mult=1.2)
    assert (out["signal"] == -1).any(), "expected a short entry on the breakdown bar"
