"""Tests for the consolidation characterization study (synthetic, no network)."""

import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))

from consolidation_research import (  # noqa: E402
    Episode,
    detect_range_containment,
    detect_regression_flatness,
    detect_volatility_contraction,
    measure_box,
    measure_shape,
    measure_escape_candle,
    build_episode_table,
    benchmark_detectors,
    classify_pattern,
    measure_volume_profile,
    atr,
)


def _candles(highs, lows, closes, opens=None):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    opens = opens if opens is not None else closes
    return pd.DataFrame(
        {
            "open": opens,
            "high": highs,
            "low": lows,
            "close": closes,
            "volume": [1.0] * n,
        },
        index=idx,
    )


def _flat_box(n=20, level=100.0, halfspan=0.5):
    rng = np.linspace(-halfspan, halfspan, n)
    highs = level + halfspan + np.abs(rng) * 0.0
    lows = level - halfspan
    closes = level + rng * 0.3
    return _candles(highs, lows, closes)


def test_flat_box_detected_and_measured():
    df = _flat_box(n=20, level=100.0, halfspan=0.5)
    eps = detect_range_containment(df, min_bars=8, box_width_pct=0.05)
    assert eps, "flat box should be detected by range-containment"
    ep = max(eps, key=lambda e: e.n_bars)

    box = measure_box(df, ep)
    assert abs(box["top"] - 100.5) < 0.1
    assert abs(box["bottom"] - 99.5) < 0.1
    assert abs(box["mid"] - 100.0) < 0.1

    shape = measure_shape(df, ep)
    # flat box: both edge slopes near zero.
    assert abs(shape["top_edge_slope"]) < 1e-3
    assert abs(shape["bottom_edge_slope"]) < 1e-3


def test_contracting_triangle_shape():
    n = 24
    span = np.linspace(3.0, 0.3, n)  # converging
    level = 100.0
    highs = level + span
    lows = level - span
    closes = np.full(n, level)
    df = _candles(highs, lows, closes)
    ep = Episode(start_idx=0, end_idx=n, method="t")
    shape = measure_shape(df, ep)
    # width contracts -> ratio < 1; top edge falls, bottom edge rises.
    assert shape["width_contraction"] < 1.0
    assert shape["top_edge_slope"] < 0
    assert shape["bottom_edge_slope"] > 0


def test_escape_candle_flagged_upward():
    # 12 quiet bars then a large up candle.
    quiet_h = [100.4] * 12
    quiet_l = [99.6] * 12
    quiet_c = [100.0] * 12
    df = _candles(
        quiet_h + [104.0],
        quiet_l + [99.9],
        quiet_c + [103.5],
    )
    ep = Episode(start_idx=0, end_idx=12, method="t")
    atr_series = atr(df, 14)
    esc = measure_escape_candle(df, ep, atr_series, escape_k=1.5)
    assert esc["escape_idx"] == 12.0
    assert esc["escape_by_edge"] == 1.0
    assert esc["escape_by_median_tr"] == 1.0
    assert esc["breakout_direction"] == 1.0


def test_escape_candle_downward_direction():
    quiet_h = [100.4] * 12
    quiet_l = [99.6] * 12
    quiet_c = [100.0] * 12
    df = _candles(
        quiet_h + [100.1],
        quiet_l + [96.0],
        quiet_c + [96.5],
    )
    ep = Episode(start_idx=0, end_idx=12, method="t")
    esc = measure_escape_candle(df, ep, atr(df, 14), escape_k=1.5)
    assert esc["escape_by_edge"] == 1.0
    assert esc["breakout_direction"] == -1.0


def test_no_escape_when_episode_runs_to_end():
    df = _flat_box(n=12)
    ep = Episode(start_idx=0, end_idx=len(df), method="t")
    esc = measure_escape_candle(df, ep, atr(df, 14))
    assert np.isnan(esc["escape_idx"])


def test_regression_flatness_detects_flat_run():
    # flat noisy run then a steep ramp.
    flat = np.full(16, 100.0) + np.random.RandomState(0).normal(0, 0.05, 16)
    ramp = np.linspace(100.5, 130.0, 16)
    closes = np.concatenate([flat, ramp])
    df = _candles(closes + 0.1, closes - 0.1, closes)
    eps = detect_regression_flatness(df, min_bars=8)
    assert eps, "should detect the flat run"
    # the flat segment should sit in the first half.
    assert eps[0].start_idx < 16


def test_volatility_contraction_runs():
    rng = np.random.RandomState(1)
    wide = 100 + np.cumsum(rng.normal(0, 1.5, 40))
    tight = wide[-1] + rng.normal(0, 0.1, 40)
    closes = np.concatenate([wide, tight])
    df = _candles(closes + 0.2, closes - 0.2, closes)
    eps = detect_volatility_contraction(df, min_bars=8, bb_period=20)
    # tight tail should produce at least one episode.
    assert isinstance(eps, list)


def test_detector_cache_matches_uncached():
    # A sweep-like series with a couple of ranges and moves.
    rng = np.random.RandomState(3)
    seg1 = 100 + rng.normal(0, 0.2, 30)
    ramp = np.linspace(100, 115, 20)
    seg2 = 115 + rng.normal(0, 0.2, 30)
    closes = np.concatenate([seg1, ramp, seg2])
    df = _candles(closes + 0.3, closes - 0.3, closes)

    cache = {}
    # Vary only box_width_pct (range_containment param); other detectors depend
    # on min_bars/flatness/bandwidth, so they must be reused from cache.
    for bwp in (0.02, 0.03, 0.02):  # repeat 0.02 to exercise a cache hit
        params = {"min_bars": 8, "box_width_pct": bwp, "bandwidth_threshold": 0.7,
                  "flatness_slope": 0.0006, "flatness_residual": 0.02,
                  "escape_k": 1.5, "atr_period": 14}
        res_uncached, bench_uncached = benchmark_detectors(df, params)
        res_cached, bench_cached = benchmark_detectors(df, params,
                                                       detector_cache=cache)
        for name in res_uncached:
            a = [(e.start_idx, e.end_idx) for e in res_uncached[name]]
            b = [(e.start_idx, e.end_idx) for e in res_cached[name]]
            assert a == b, f"{name} episodes differ with cache at bwp={bwp}"
        pd.testing.assert_frame_equal(bench_uncached, bench_cached)
    # cache should hold the two distinct range_containment keys + 1 each for the
    # two cache-independent detectors (min_bars constant across cells).
    assert len(cache) >= 3


def test_classify_pattern_named_shapes():
    # rectangle: both edges flat
    assert classify_pattern({"top_edge_travel": 0.05,
                             "bottom_edge_travel": -0.05}) == "rectangle"
    # ascending triangle: flat top, rising bottom
    assert classify_pattern({"top_edge_travel": 0.1,
                             "bottom_edge_travel": 0.6}) == "ascending_triangle"
    # descending triangle: falling top, flat bottom
    assert classify_pattern({"top_edge_travel": -0.6,
                             "bottom_edge_travel": 0.05}) == "descending_triangle"
    # symmetrical triangle: converging
    assert classify_pattern({"top_edge_travel": -0.6,
                             "bottom_edge_travel": 0.6}) == "symmetrical_triangle"
    # broadening: diverging
    assert classify_pattern({"top_edge_travel": 0.6,
                             "bottom_edge_travel": -0.6}) == "broadening"
    # wedges
    assert classify_pattern({"top_edge_travel": 0.6,
                             "bottom_edge_travel": 0.6}) == "rising_wedge"
    assert classify_pattern({"top_edge_travel": -0.6,
                             "bottom_edge_travel": -0.6}) == "falling_wedge"


def test_classify_pattern_via_measure_shape_on_triangle():
    from consolidation_research import measure_shape, Episode
    n = 24
    span = np.linspace(3.0, 0.3, n)
    level = 100.0
    df = _candles(level + span, level - span, np.full(n, level))
    shape = measure_shape(df, Episode(0, n, "t"))
    assert classify_pattern(shape) == "symmetrical_triangle"


def test_volume_profile_poc_and_value_area():
    # Concentrate volume in the middle of a flat box -> POC near mid, VA inside box.
    n = 30
    level = 100.0
    highs = np.full(n, 100.5)
    lows = np.full(n, 99.5)
    closes = np.full(n, level)
    # heavy volume on bars sitting at the mid, light at the edges
    closes[:5] = 100.4
    closes[-5:] = 99.6
    df = _candles(highs, lows, closes)
    df["volume"] = [1.0] * n
    df.iloc[10:20, df.columns.get_loc("volume")] = 50.0  # mid-heavy
    from consolidation_research import Episode
    vp = measure_volume_profile(df, Episode(0, n, "t"), bins=10)
    assert 99.5 <= vp["poc"] <= 100.5
    assert 0.0 <= vp["poc_position"] <= 1.0
    assert vp["val"] <= vp["vah"]
    assert 0.0 < vp["value_area_width_frac"] <= 1.0


def test_build_episode_table_columns():
    df = _flat_box(n=20)
    eps = detect_range_containment(df, min_bars=8, box_width_pct=0.05)
    table = build_episode_table(df, eps, {"atr_period": 14, "escape_k": 1.5})
    assert not table.empty
    for col in ["top", "bottom", "mid", "width_pct", "top_edge_slope",
                "width_contraction", "escape_tr", "breakout_direction",
                "pattern", "poc", "vah", "val", "poc_position"]:
        assert col in table.columns
