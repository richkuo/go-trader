import os
import sys

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from simulate_strategy import _resolve_atr_regime_stop

SL_CANON = "stop_loss_atr_mult_regime"
SL_LEGACY = "stop_loss_atr_regime"
TRAIL_CANON = "trailing_stop_atr_mult_regime"
TRAIL_V18 = "trail_stop_atr_regime"
TRAIL_V17 = "trailing_stop_atr_regime"

A = {"trending_up": {"atr_multiple": 2.0}}
B = {"trending_up": {"atr_multiple": 4.0}}


def test_missing_field_resolves_to_none():
    assert _resolve_atr_regime_stop({}, SL_CANON) is None
    assert _resolve_atr_regime_stop({}, TRAIL_CANON) is None


@pytest.mark.parametrize("key", [SL_CANON, SL_LEGACY])
def test_stop_loss_single_spelling_resolves(key):
    assert _resolve_atr_regime_stop({key: A}, SL_CANON) == A


@pytest.mark.parametrize("key", [TRAIL_CANON, TRAIL_V18, TRAIL_V17])
def test_trailing_single_spelling_resolves(key):
    assert _resolve_atr_regime_stop({key: A}, TRAIL_CANON) == A


@pytest.mark.parametrize("cfg", [
    {SL_LEGACY: A, SL_CANON: A},
    {SL_CANON: A, SL_LEGACY: A},
])
def test_stop_loss_equivalent_spellings_merge_regardless_of_order(cfg):
    assert _resolve_atr_regime_stop(cfg, SL_CANON) == A


@pytest.mark.parametrize("cfg", [
    {SL_LEGACY: B, SL_CANON: A},
    {SL_CANON: A, SL_LEGACY: B},
])
def test_stop_loss_conflicting_spellings_raise(cfg):
    with pytest.raises(ValueError):
        _resolve_atr_regime_stop(cfg, SL_CANON)


def test_trailing_all_three_spellings_agreeing_merge():
    cfg = {TRAIL_CANON: A, TRAIL_V18: A, TRAIL_V17: A}
    assert _resolve_atr_regime_stop(cfg, TRAIL_CANON) == A


@pytest.mark.parametrize("cfg", [
    {TRAIL_CANON: A, TRAIL_V18: A, TRAIL_V17: B},
    {TRAIL_CANON: A, TRAIL_V18: B, TRAIL_V17: A},
    {TRAIL_V17: A, TRAIL_V18: B},
    {TRAIL_V18: B, TRAIL_V17: A},
])
def test_trailing_conflicting_spellings_raise(cfg):
    with pytest.raises(ValueError):
        _resolve_atr_regime_stop(cfg, TRAIL_CANON)


def test_two_legacy_spellings_agreeing_with_no_canonical_merge():
    assert _resolve_atr_regime_stop({TRAIL_V17: A, TRAIL_V18: A}, TRAIL_CANON) == A


def test_resolvers_are_independent_per_field():
    cfg = {SL_LEGACY: A, TRAIL_V18: B}
    assert _resolve_atr_regime_stop(cfg, SL_CANON) == A
    assert _resolve_atr_regime_stop(cfg, TRAIL_CANON) == B
