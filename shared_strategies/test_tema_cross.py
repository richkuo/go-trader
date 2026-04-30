"""Tests for tema_cross and tema_cross_bd strategies in registry.py."""

import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))


def make_ohlcv(closes, volume=None, noise=0.5):
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.array(volume, dtype=float),
    })


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
    """Linear uptrend with sinusoidal oscillation so ema_short crosses ema_mid repeatedly."""
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


# ─── tema_cross (long-only) ─────────────────────────────────────────────


def test_tema_cross_emits_buy_during_uptrend(registry):
    """A run-up after a downtrend should fire a buy on the bullish cross."""
    prices = _oscillating_uptrend(200, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_strategy(df)
    assert (result["signal"] == 1).any(), "Expected a buy signal on bullish cross during uptrend"


def test_tema_cross_position_persists_between_crosses(registry):
    """Position must hold at 1 across silent bars between bullish and bearish cross."""
    prices = _trend_up_then_down(150, 100, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_strategy(df)
    assert (result["signal"] == 1).any(), "Expected an entry"
    # After the first +1 signal and before any -1, position should equal 1 on multiple bars.
    entry_idx = result.index[result["signal"] == 1][0]
    after_entry = result.loc[entry_idx:]
    in_position = after_entry["position"] == 1
    # At least 5 consecutive in-position bars proves persistence vs single-bar bug.
    assert in_position.sum() > 5, (
        f"Position only held for {in_position.sum()} bars after entry — should persist until bearish cross"
    )


def test_tema_cross_exits_on_bearish_cross(registry):
    """An uptrend followed by a sustained downtrend should produce a -1 exit signal."""
    prices = _trend_up_then_down(120, 120, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_strategy(df)
    assert (result["signal"] == 1).any()
    assert (result["signal"] == -1).any(), "Expected an exit on the bearish cross"


def test_tema_cross_no_short_entries(registry):
    """Long-only strategy must never go to position == -1."""
    prices = _trend_up_then_down(120, 120, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_strategy(df)
    assert (result["position"] >= 0).all(), "tema_cross must be long-only"


def test_tema_cross_flat_market_no_signals(registry):
    """A flat market should produce no entry signals."""
    df = make_ohlcv(np.full(200, 100.0), noise=0)
    result = registry.tema_cross_strategy(df)
    # No bullish cross can fire when EMAs converge to the same flat value.
    assert not (result["signal"] == 1).any()


# ─── tema_cross_bd (bidirectional) ───────────────────────────────────────


def test_tema_cross_bd_emits_long_in_uptrend(registry):
    prices = _oscillating_uptrend(200, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_bd_strategy(df)
    assert (result["signal"] == 1).any()


def test_tema_cross_bd_emits_short_in_downtrend(registry):
    # Prime EMAs with an uptrend, then a sharp dump to push ema_mid below ema_long
    # (confirmed downtrend), then a small oscillating drift down so bearish crosses
    # keep firing while the downtrend remains confirmed.
    up = _oscillating_uptrend(120, start=100.0, drift=0.6, amp=2.0)
    dump = np.linspace(up[-1], up[-1] - 60.0, 80)
    rng = np.random.RandomState(0)
    t = np.arange(400)
    grind = dump[-1] - 0.05 * t + 1.5 * np.sin(2 * np.pi * t / 25) + rng.randn(400) * 0.05
    prices = np.concatenate([up, dump, grind])
    df = make_ohlcv(prices)
    result = registry.tema_cross_bd_strategy(df)
    assert (result["signal"] == -1).any(), "Expected a short entry on bearish cross during downtrend"
    assert (result["position"] == -1).any(), "Expected position to take -1 (short)"


def test_tema_cross_bd_position_persists(registry):
    """Position must hold across silent bars between confirmed crosses."""
    prices = _trend_up_then_down(150, 150, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_bd_strategy(df)
    long_bars = (result["position"] == 1).sum()
    assert long_bars > 5, f"Long position only held {long_bars} bars — should persist between crosses"


def test_tema_cross_bd_signal_bounded(registry):
    """Signal must be in {-1, 0, 1} even on direct long→short flips."""
    prices = _trend_up_then_down(100, 100, start=100.0)
    df = make_ohlcv(prices)
    result = registry.tema_cross_bd_strategy(df)
    assert result["signal"].isin([-1, 0, 1]).all(), "Signal values must be clamped to {-1, 0, 1}"
