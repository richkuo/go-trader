import math
import os
import sys

import numpy as np

from backtest_options import calc_iv_rank


RECENT = 14
LOOKBACK = 60


def _path_from_vol_schedule(vol_per_day: list, seed: int = 0) -> list:
    rng = np.random.default_rng(seed)
    closes = [100.0]
    for sigma in vol_per_day:
        r = rng.normal(loc=0.0, scale=sigma)
        closes.append(closes[-1] * math.exp(r))
    return closes


def test_iv_rank_neutral_on_degenerate_history_and_banded_otherwise(subtests):
    with subtests.test(case="degenerate_history_returns_neutral_50"):
        _assert_degenerate_history_returns_neutral_50()
    with subtests.test(case="boundary_exact_minimum_computes_rank"):
        _assert_boundary_exact_minimum_computes_rank()
    with subtests.test(case="rank_lands_in_expected_band"):
        _assert_rank_lands_in_expected_band()


def _assert_degenerate_history_returns_neutral_50():
    for case_id, closes in [
        ("history_too_short", [100.0 + i for i in range(50)]),
        ("one_below_minimum", [100.0 + i for i in range(74)]),
        ("flat_lookback", [100.0] * (RECENT + LOOKBACK + 5)),
    ]:
        assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0, \
            f"test_degenerate_history_returns_neutral_50[{case_id}]"


def _assert_boundary_exact_minimum_computes_rank():
    closes = _path_from_vol_schedule([0.02] * 74, seed=11)
    assert len(closes) == 75
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert 0.0 <= rank <= 100.0
    assert rank != 50.0


def _assert_rank_lands_in_expected_band():
    for case_id, (schedule, seed, lo, hi, strict_lo, strict_hi) in {
        "recent_spike": ([0.005] * 76 + [0.08] * 14, 1, 90.0, 100.0, True, False),
        "recent_calm": ([0.08] * 76 + [0.001] * 14, 2, 0.0, 10.0, False, True),
        "clamped_to_0_100": ([0.02] * 76 + [0.04] * 14, 3, 0.0, 100.0, False, False),
        "middle_of_distribution": (list(np.linspace(0.01, 0.05, 76)) + [0.03] * 14, 4,
                                   20.0, 80.0, True, True),
    }.items():
        case = f"test_rank_lands_in_expected_band[{case_id}]"
        closes = _path_from_vol_schedule(schedule, seed=seed)
        rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
        assert (rank > lo) if strict_lo else (rank >= lo), f"{case}: got {rank}, low bound {lo}"
        assert (rank < hi) if strict_hi else (rank <= hi), f"{case}: got {rank}, high bound {hi}"
