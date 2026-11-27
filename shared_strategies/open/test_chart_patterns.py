
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

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


def _gate_frame(n, offset):
    start = pd.Timestamp("2024-01-01", tz="UTC") + pd.Timedelta(hours=offset)
    close = 100 + np.cumsum(np.random.default_rng(offset).normal(0, 1, n))
    return pd.DataFrame(
        {"open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 100.0},
        index=pd.date_range(start, periods=n, freq="1h"),
    )


class TestHTFGateWarmBoundary:
    def test_min_visible_buckets_and_refusal_boundary_over_every_start_offset(self):
        for factor, ema_slow in ((4, 40), (4, 49), (4, 50), (5, 40), (6, 40)):
            boundary = factor * (ema_slow + 1)
            for frame in (150, 200, boundary - 1, boundary):
                visible, warm = [], []
                for offset in range(factor):
                    out = chart_pattern_core(
                        _gate_frame(frame, offset),
                        htf_gate_factor=factor,
                        htf_gate_ema_slow=ema_slow,
                    )
                    last = out.iloc[-1]
                    assert int(last["htf_gate_candles"]) == frame
                    assert int(last["htf_gate_min_candles"]) == boundary
                    assert int(last["htf_gate_warm"]) == int(last["htf_gate_buckets"] > ema_slow)
                    if not last["htf_gate_warm"]:
                        assert (out["htf_gate_trend"].iloc[-factor:] == 0).all()
                    visible.append(int(last["htf_gate_buckets"]))
                    warm.append(bool(last["htf_gate_warm"]))
                assert min(visible) == frame // factor, (factor, ema_slow, frame, visible)
                assert all(warm) == (frame // factor > ema_slow), (factor, ema_slow, frame)
                assert all(warm) == (frame >= boundary), (factor, ema_slow, frame)
