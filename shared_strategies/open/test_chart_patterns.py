
import numpy as np
import pandas as pd
import pytest

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


class TestDoubleBottom:
    def test_detects_double_bottom(self):
        prices = (
            list(np.linspace(100, 80, 20)) +
            list(np.linspace(80, 90, 15)) +
            list(np.linspace(90, 81, 15)) +
            list(np.linspace(81, 100, 20)) +
            [100] * 30
        )
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df["high"], df["low"], lookback=3)
        matches = detect_double_bottom(
            df["high"], df["low"], df["close"], sh, sl, tolerance=0.03
        )
        assert len(matches) >= 1
        assert matches[0].signal == 1


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


