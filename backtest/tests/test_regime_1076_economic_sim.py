import os
import sys

import numpy as np
import pytest

_RESEARCH = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "research"))
if _RESEARCH not in sys.path:
    sys.path.insert(0, _RESEARCH)

from regime_1076_economic_sim import (
    _book, _block_shuffle, _mean_dwell, _policy_side,
)


def test_buyhold_book_equals_asset_return():
    close = np.array([100.0, 110.0, 99.0, 108.0, 120.0])
    bh = _book(close, np.ones(5), fee_rate=0.0)
    assert bh["total_return_pct"] == pytest.approx((120.0 / 100.0 - 1) * 100)


def test_no_lookahead_long_decided_after_jump_earns_nothing():
    close = np.array([100.0, 100.0, 200.0, 200.0, 200.0])
    side_at_jump = np.array([0.0, 0.0, 1.0, 0.0, 0.0])
    assert _book(close, side_at_jump, fee_rate=0.0)["total_return_pct"] == pytest.approx(0.0)


def test_long_decided_before_jump_earns_it():
    close = np.array([100.0, 100.0, 200.0, 200.0, 200.0])
    side_prior = np.array([0.0, 1.0, 0.0, 0.0, 0.0])
    assert _book(close, side_prior, fee_rate=0.0)["total_return_pct"] == pytest.approx(100.0)


def test_short_earns_downmove_with_prior_decision():
    close = np.array([100.0, 100.0, 50.0, 50.0])
    side = np.array([0.0, -1.0, 0.0, 0.0])
    assert _book(close, side, fee_rate=0.0)["total_return_pct"] == pytest.approx(50.0)


def test_fee_charged_on_turnover():
    close = np.array([100.0, 100.0, 100.0])
    side = np.array([1.0, 0.0, 0.0])
    got = _book(close, side, fee_rate=0.01)["total_return_pct"]
    assert got == pytest.approx((0.99 * 0.99 - 1) * 100)


def test_block_shuffle_preserves_multiset_and_length():
    rng = np.random.default_rng(0)
    arr = np.array([1.0, 1.0, 0.0, -1.0, -1.0, 0.0, 1.0, 0.0, -1.0, 1.0])
    sh = _block_shuffle(arr, 2, rng)
    assert len(sh) == len(arr)
    assert sorted(sh.tolist()) == sorted(arr.tolist())


def test_mean_dwell_constant_series():
    assert _mean_dwell(np.ones(10)) == pytest.approx(10.0)
    assert _mean_dwell(np.array([1.0, -1.0, 1.0, -1.0])) == pytest.approx(1.0)


def test_policy_side_mapping():
    assert _policy_side("trending_up_clean", "flat") == 1
    assert _policy_side("trending_down_choppy", "flat") == -1
    assert _policy_side("ranging_quiet", "flat") == 0
    assert _policy_side("ranging_quiet", "long") == 1
