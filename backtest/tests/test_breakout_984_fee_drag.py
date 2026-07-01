"""Tests for the #984 fee-drag aggregation (pure helper, no data access)."""

import importlib.util
import os

import pytest

_FEE_DRAG_PATH = os.path.join(
    os.path.dirname(__file__), "..", "candidates", "breakout_984", "fee_drag.py")
_spec = importlib.util.spec_from_file_location("breakout_984_fee_drag",
                                               _FEE_DRAG_PATH)
fee_drag = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fee_drag)


def _leg(ret, trades=10, span_days=365.25 / 2):
    return {"return_pct": ret, "trades": trades, "span_days": span_days}


def test_summarize_pairs_gross_net_and_drag():
    gross = [_leg(5.0), _leg(7.0)]
    net = [_leg(2.0), _leg(3.0)]
    s = fee_drag.summarize_fee_drag(gross, net)
    assert s["legs"] == 2
    assert s["mean_gross_return_pct"] == pytest.approx(6.0)
    assert s["mean_net_return_pct"] == pytest.approx(2.5)
    assert s["drag_pp"] == pytest.approx(3.5)
    # trades/span come from the NET legs: 20 trades over one summed year.
    assert s["trades"] == 20
    assert s["trades_per_year"] == pytest.approx(20.0)


def test_summarize_drops_none_legs_pairwise():
    gross = [_leg(5.0), None, _leg(9.0)]
    net = [_leg(2.0), _leg(4.0), None]
    s = fee_drag.summarize_fee_drag(gross, net)
    # Only the first pair survives: a None on EITHER side drops the dataset.
    assert s["legs"] == 1
    assert s["mean_gross_return_pct"] == pytest.approx(5.0)
    assert s["mean_net_return_pct"] == pytest.approx(2.0)
    assert s["drag_pp"] == pytest.approx(3.0)


def test_summarize_all_none_returns_none():
    assert fee_drag.summarize_fee_drag([None], [None]) is None
    assert fee_drag.summarize_fee_drag([], []) is None


def test_summarize_zero_span_yields_no_annualization():
    s = fee_drag.summarize_fee_drag([_leg(1.0, span_days=0.0)],
                                    [_leg(0.5, span_days=0.0)])
    assert s["trades_per_year"] is None


def test_summarize_missing_span_key_treated_as_zero():
    gross = [{"return_pct": 4.0, "trades": 3}]
    net = [{"return_pct": 1.0, "trades": 3}]
    s = fee_drag.summarize_fee_drag(gross, net)
    assert s["drag_pp"] == pytest.approx(3.0)
    assert s["trades_per_year"] is None
