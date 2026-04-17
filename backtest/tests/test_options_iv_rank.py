"""
Regression tests for #302 C3: calc_iv_rank must be a percentile rank of
current realised vol inside a trailing lookback — matching the shape of
adapter.get_iv_rank() in the live VolMeanReversionStrategy — not the
``(recent / hist) * 50`` ratio the old implementation returned.
"""
import math

import numpy as np
import pytest

from backtest_options import calc_iv_rank


RECENT = 14
LOOKBACK = 60


def _path_from_vol_schedule(vol_per_day: list, seed: int = 0) -> list:
    """Build a close-price path where each day's log return has the given
    (day-specific) volatility. Used to control where realised vol lands in
    the lookback distribution.
    """
    rng = np.random.default_rng(seed)
    closes = [100.0]
    for sigma in vol_per_day:
        r = rng.normal(loc=0.0, scale=sigma)
        closes.append(closes[-1] * math.exp(r))
    return closes


def test_returns_default_when_history_too_short():
    closes = [100.0 + i for i in range(50)]  # only 50 bars
    assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0


def test_flat_lookback_returns_neutral_50():
    """If every day is dead flat vol is 0 everywhere → degenerate range → 50."""
    closes = [100.0] * (RECENT + LOOKBACK + 5)
    assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0


def test_recent_spike_ranks_near_100():
    """Vol is low for the lookback then spikes in the most recent window.
    Current realised vol should sit at the top of the distribution → rank ≈ 100.
    """
    # 90 total bars: first 76 at low vol, last 14 at high vol.
    sched = [0.005] * 76 + [0.08] * 14
    closes = _path_from_vol_schedule(sched, seed=1)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert rank > 90, f"expected near-100, got {rank}"


def test_recent_calm_ranks_near_0():
    """Inverse: lookback is volatile then the recent window calms down → rank ≈ 0."""
    sched = [0.08] * 76 + [0.001] * 14
    closes = _path_from_vol_schedule(sched, seed=2)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert rank < 10, f"expected near-0, got {rank}"


def test_rank_is_clamped_to_0_100():
    """Values outside the historical window should clamp, not go negative."""
    # 2× ratio between recent and historical that used to produce rank=100
    # in the old formula — verify the new one returns a legal percentile.
    sched = [0.02] * 76 + [0.04] * 14
    closes = _path_from_vol_schedule(sched, seed=3)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert 0.0 <= rank <= 100.0


def test_middle_of_distribution_ranks_around_50():
    """Current realised vol sitting mid-range of the lookback → rank near 50."""
    # Deterministic schedule: vol linearly walks up then current hits the midpoint.
    # First 76 bars span 0.01 → 0.05 linearly; last 14 bars are ~0.03.
    lookback_vols = np.linspace(0.01, 0.05, 76).tolist()
    recent_vols = [0.03] * 14
    closes = _path_from_vol_schedule(lookback_vols + recent_vols, seed=4)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    # Wide tolerance — exact rank depends on the realised path. We only assert
    # we're nowhere near the extremes the old broken formula hit.
    assert 20 < rank < 80, f"expected a middle value, got {rank}"
