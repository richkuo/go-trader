"""Regression tests for the #1076 regime-timing economic isolation (backtest/research).

The load-bearing invariant is the look-ahead-safe alignment in ``_book``: the side decided
at the close of bar t is held over the NEXT move (t -> t+1), never the move into bar t. A
draft that used ``decision_side[t]`` for the t-1 -> t move produced impossible Sharpe ~9;
these tests pin the corrected alignment so it cannot silently regress.
"""
import os
import sys

import numpy as np
import pytest

_RESEARCH = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "research"))
if _RESEARCH not in sys.path:
    sys.path.insert(0, _RESEARCH)

from regime_1076_economic_sim import (  # noqa: E402
    _book, _block_shuffle, _mean_dwell, _policy_side,
)


def test_buyhold_book_equals_asset_return():
    # An always-long book must reproduce the asset's buy-and-hold return exactly — proves
    # the equity/alignment math has no off-by-one.
    close = np.array([100.0, 110.0, 99.0, 108.0, 120.0])
    bh = _book(close, np.ones(5), fee_rate=0.0)
    assert bh["total_return_pct"] == pytest.approx((120.0 / 100.0 - 1) * 100)


def test_no_lookahead_long_decided_after_jump_earns_nothing():
    # close jumps 100 -> 200 over the bar1 -> bar2 move. A long decided AT bar 2 (the bar
    # whose close already printed 200) must NOT capture that move: side at bar 2 governs the
    # 2 -> 3 move (200 -> 200 = flat). Capturing it would be look-ahead.
    close = np.array([100.0, 100.0, 200.0, 200.0, 200.0])
    side_at_jump = np.array([0.0, 0.0, 1.0, 0.0, 0.0])
    assert _book(close, side_at_jump, fee_rate=0.0)["total_return_pct"] == pytest.approx(0.0)


def test_long_decided_before_jump_earns_it():
    # The mirror: a long decided at bar 1 (prior to the jump) governs the 1 -> 2 move and
    # earns the full +100%.
    close = np.array([100.0, 100.0, 200.0, 200.0, 200.0])
    side_prior = np.array([0.0, 1.0, 0.0, 0.0, 0.0])
    assert _book(close, side_prior, fee_rate=0.0)["total_return_pct"] == pytest.approx(100.0)


def test_short_earns_downmove_with_prior_decision():
    # A short decided at bar 1 governs the 1 -> 2 down-move (-50%) and earns +50%.
    close = np.array([100.0, 100.0, 50.0, 50.0])
    side = np.array([0.0, -1.0, 0.0, 0.0])
    assert _book(close, side, fee_rate=0.0)["total_return_pct"] == pytest.approx(50.0)


def test_fee_charged_on_turnover():
    # Flat market; flip flat->long then long->flat. Two unit turnovers at 1% each compound
    # the equity to 0.99 * 0.99, independent of price (which never moves).
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
    assert _mean_dwell(np.ones(10)) == pytest.approx(10.0)        # never changes -> one run
    assert _mean_dwell(np.array([1.0, -1.0, 1.0, -1.0])) == pytest.approx(1.0)  # flips every bar


def test_policy_side_mapping():
    assert _policy_side("trending_up_clean", "flat") == 1
    assert _policy_side("trending_down_choppy", "flat") == -1
    assert _policy_side("ranging_quiet", "flat") == 0
    assert _policy_side("ranging_quiet", "long") == 1
