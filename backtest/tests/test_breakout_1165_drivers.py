"""Tests for the #1165 regime-gate driver plumbing (pure helpers, no data
access).

The load-bearing property: every #1165 candidate carries regime state, and
``candidate_leg_kwargs`` is the single place it is threaded into run_leg — a
driver that dropped ``allowed_regimes`` / ``regime_windows_spec`` /
``profile_allocation`` would silently score the UNGATED entry on the
continuous audit window (a plausible wrong number, not an error).
"""

import importlib.util
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..",
                                "shared_tools"))

from eval_windows import validate_candidate  # noqa: E402

_COMMON_PATH = os.path.join(
    os.path.dirname(__file__), "..", "candidates", "breakout_1165",
    "driver_common.py")
_spec = importlib.util.spec_from_file_location("breakout_1165_driver_common",
                                               _COMMON_PATH)
dc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(dc)


def test_candidate_leg_kwargs_threads_all_regime_state():
    candidate = {
        "name": "breakout",
        "direction": "long",
        "allowed_regimes": ["trending_up"],
        "regime_windows_spec": {"medium": {"classifier": "adx", "period": 14,
                                           "adx_threshold": 25.0}},
        "profile_allocation": {"window_spec": {"classifier": "adx",
                                               "period": 14}},
    }
    kw = dc.candidate_leg_kwargs(candidate)
    assert kw["allowed_regimes"] == ["trending_up"]
    assert kw["regime_windows_spec"]["medium"]["adx_threshold"] == 25.0
    assert kw["profile_allocation"]["window_spec"]["period"] == 14
    assert kw["direction"] == "long"
    # Frozen close stack: nothing may inject a stop the shortlist doesn't own.
    assert kw["close_strategies"] is None
    assert kw["stop_loss_atr_mult"] is None
    assert kw["trailing_stop_atr_mult"] is None


def test_candidate_leg_kwargs_defaults_direction_long():
    # #996: with direction unset the engine path would open shorts on the
    # breakout's raw signal=-1 — the default must be the pinned long leg.
    assert dc.candidate_leg_kwargs({"name": "breakout"})["direction"] == "long"


def test_gate_grid_labels_unique_and_specs_validate():
    grid = dc.build_gate_grid() + dc.build_profile_grid() + \
        dc.build_gate_threshold_plateau(["trending_up", "ranging"])
    labels = [c["label"] for c in grid]
    assert len(labels) == len(set(labels))
    for c in grid:
        spec = {k: v for k, v in c.items() if k != "label"}
        spec["name"] = "breakout"
        validate_candidate(dict(spec))  # raises on malformed candidates


def test_not_down_sets_use_bare_ranging_directional():
    # #1124 bare-covers-subs: the bare label gates both _up and _down; listing
    # the subs alongside it would be redundant, listing only one sub would
    # silently gate out the other.
    assert "ranging_directional" in dc.COMP_NOT_DOWN
    assert "ranging_directional_up" not in dc.COMP_NOT_DOWN
    assert "ranging_directional_down" not in dc.COMP_NOT_DOWN


def test_profile_grid_maps_every_composite_label():
    # _ProfileSwitcher holds the active profile on unknown labels (fail-open),
    # so the composite M4 candidate must map all nine labels explicitly.
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..",
                                    "shared_tools"))
    from regime import VALID_LABELS_COMPOSITE
    comp = next(c for c in dc.build_profile_grid()
                if c["label"] == "m4_comp_bear_off")
    profiles = comp["profile_allocation"]["profiles"]
    assert set(profiles) == set(VALID_LABELS_COMPOSITE)
    assert profiles["trending_down_clean"] == "off"
    assert profiles["trending_down_choppy"] == "off"


def test_profile_off_set_emits_no_entries():
    # The "off" profile zeroes the entry list via an unreachable expansion
    # multiple; the frozen exit survives because the switch is flat-only.
    assert dc.PARAM_SET_OFF["atr_multiplier"] >= 100.0
    # "selective" raises the bar without zeroing it.
    assert dc.PARAM_SET_SELECTIVE["atr_multiplier"] > 1.5
    assert dc.PARAM_SET_SELECTIVE["lookback"] > 20
