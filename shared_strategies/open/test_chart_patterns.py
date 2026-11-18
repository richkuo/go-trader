
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_CHART_PATTERNS = load_module("_chart_patterns_test", __file__.replace("test_chart_patterns.py", "chart_patterns.py"))
PatternMatch = _CHART_PATTERNS.PatternMatch
find_swing_points = _CHART_PATTERNS.find_swing_points
volume_confirmed = _CHART_PATTERNS.volume_confirmed
detect_double_top = _CHART_PATTERNS.detect_double_top
detect_double_bottom = _CHART_PATTERNS.detect_double_bottom
detect_triple_top = _CHART_PATTERNS.detect_triple_top
detect_triple_bottom = _CHART_PATTERNS.detect_triple_bottom
detect_head_and_shoulders = _CHART_PATTERNS.detect_head_and_shoulders
detect_inverse_head_and_shoulders = _CHART_PATTERNS.detect_inverse_head_and_shoulders
detect_bull_flag = _CHART_PATTERNS.detect_bull_flag
detect_bear_flag = _CHART_PATTERNS.detect_bear_flag
detect_ascending_triangle = _CHART_PATTERNS.detect_ascending_triangle
detect_descending_triangle = _CHART_PATTERNS.detect_descending_triangle
detect_cup_and_handle = _CHART_PATTERNS.detect_cup_and_handle
chart_pattern_core = _CHART_PATTERNS.chart_pattern_core
_get_swing_indices = _CHART_PATTERNS._get_swing_indices
_htf_gate_trend = _CHART_PATTERNS._htf_gate_trend


def _double_top_fixture(prefix=None):
    prices = (
        list(np.linspace(80, 100, 20)) +
        list(np.linspace(100, 90, 15)) +
        list(np.linspace(90, 99, 15)) +
        list(np.linspace(99, 85, 20)) +
        [85] * 30
    )
    if prefix is not None:
        prices = list(prefix) + prices
    vol = [100.0] * len(prices)
    off = len(prefix) if prefix is not None else 0
    for i in range(off + 50, off + 70):
        vol[i] = 200.0
    return make_ohlcv(prices, volume=vol)


class TestHTFGate:
    def test_gated_signal_independent_of_future_bars(self):
        df = _double_top_fixture(prefix=np.linspace(20, 80, 400))
        df.index = pd.date_range("2024-01-01", periods=len(df), freq="1h")
        kwargs = dict(
            pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0,
            htf_gate_factor=4, htf_gate_ema_fast=10, htf_gate_ema_slow=20,
        )
        full = chart_pattern_core(df, **kwargs)
        base = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03,
                                  vol_multiplier=1.0)
        signal_bars = list(np.where(base["signal"].values != 0)[0])
        assert len(signal_bars) >= 1
        for k in signal_bars:
            partial = chart_pattern_core(df.iloc[: k + 1], **kwargs)
            assert partial["signal"].iloc[k] == full["signal"].iloc[k], (
                f"gated signal at bar {k} flipped under truncation"
            )
        k = signal_bars[-1]
        partial = chart_pattern_core(df.iloc[: k + 1], **kwargs)
        assert (
            partial["htf_gate_trend"].values
            == full["htf_gate_trend"].values[: k + 1]
        ).all()


def _gate_frame(n, offset):
    start = pd.Timestamp("2024-01-01", tz="UTC") + pd.Timedelta(hours=offset)
    close = 100 + np.cumsum(np.random.default_rng(offset).normal(0, 1, n))
    return pd.DataFrame(
        {"open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 100.0},
        index=pd.date_range(start, periods=n, freq="1h"),
    )


class TestHTFGateWarmBoundary:

    def test_inactive_gate_adds_no_warmth_columns(self):
        for factor in (0, 1):
            out = chart_pattern_core(_gate_frame(200, 0), htf_gate_factor=factor)
            assert not any(c.startswith("htf_gate_") for c in out.columns)
