"""Tests for the #1198 squeeze_momentum regime-gate driver plumbing (pure
helpers, no data access).

Direct analog of test_breakout_1165_drivers.py. The load-bearing property:
every #1198 candidate carries regime state, and ``candidate_leg_kwargs`` is the
single place it is threaded into run_leg — a driver that dropped
``allowed_regimes`` / ``regime_windows_spec`` / ``profile_allocation`` would
silently score the UNGATED entry on the continuous audit window (a plausible
wrong number, not an error). Also covers the strategy-specific M4 param sets
(the "off"/"selective" sets are the only squeeze-specific part) and the inlined
``summarize_fee_drag`` (inlined here rather than imported from a twin, so it is
unit-tested alongside its own drivers).
"""

import importlib.util
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..",
                                "shared_tools"))

from eval_windows import validate_candidate  # noqa: E402

_COMMON_PATH = os.path.join(
    os.path.dirname(__file__), "..", "candidates", "squeeze_momentum_1198",
    "driver_common.py")
_spec = importlib.util.spec_from_file_location(
    "squeeze_momentum_1198_driver_common", _COMMON_PATH)
dc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(dc)


def test_candidate_leg_kwargs_threads_all_regime_state():
    candidate = {
        "name": "squeeze_momentum",
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
    # squeeze's raw signal=-1 (downside squeeze releases) — the default must be
    # the pinned long leg, matching the #983 close-stack sweep.
    assert dc.candidate_leg_kwargs(
        {"name": "squeeze_momentum"})["direction"] == "long"


def test_gate_grid_labels_unique_and_specs_validate():
    grid = dc.build_gate_grid() + dc.build_profile_grid() + \
        dc.build_gate_threshold_plateau(["trending_up", "ranging"])
    labels = [c["label"] for c in grid]
    assert len(labels) == len(set(labels))
    for c in grid:
        spec = {k: v for k, v in c.items() if k != "label"}
        spec["name"] = "squeeze_momentum"
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


def test_off_set_zeroes_entries_via_wide_keltner():
    # squeeze_momentum fires on the Bollinger band popping back OUTSIDE the
    # Keltner channel (the squeeze releasing). kc_mult=100 makes the Keltner
    # channel so wide the Bollinger band is always inside — the squeeze never
    # turns off, so no release ever fires (verified 0 trades in the sweep). The
    # analog of breakout's unreachable expansion multiple.
    assert dc.PARAM_SET_OFF["kc_mult"] >= 100.0
    # It overrides only the Keltner multiple — nothing else.
    assert set(dc.PARAM_SET_OFF) == {"kc_mult"}


def test_selective_set_tightens_without_zeroing():
    # "selective" raises the bar (tighter coil + longer momentum window)
    # instead of zeroing it, so it must stay a strict middle ground: a Keltner
    # multiple BELOW the 1.5 default (tighter squeeze) but well above the
    # kc_mult=1.0 that collapses to near-off, and a momentum lookback ABOVE the
    # 12 default.
    assert 1.0 < dc.PARAM_SET_SELECTIVE["kc_mult"] < 1.5
    assert dc.PARAM_SET_SELECTIVE["mom_lookback"] > 12


def test_summarize_fee_drag_pairs_and_annualizes():
    # Pure aggregation: mean gross/net, drag = gross-net, summed trades, and
    # trades/yr over the summed calendar span; None legs drop pairwise.
    gross = [{"return_pct": 10.0, "trades": 5, "span_days": 365.25},
             {"return_pct": 20.0, "trades": 5, "span_days": 365.25},
             None]
    net = [{"return_pct": 6.0, "trades": 5, "span_days": 365.25},
           {"return_pct": 12.0, "trades": 5, "span_days": 365.25},
           {"return_pct": 0.0, "trades": 9, "span_days": 365.25}]
    s = dc.summarize_fee_drag(gross, net)
    assert s["legs"] == 2                       # the None gross drops its pair
    assert s["mean_gross_return_pct"] == 15.0
    assert s["mean_net_return_pct"] == 9.0
    assert s["drag_pp"] == 6.0
    assert s["trades"] == 10                     # 5 + 5, dropped pair excluded
    assert s["trades_per_year"] == 5.0           # 10 trades / (730.5/365.25) yr


def test_summarize_fee_drag_empty_is_none():
    assert dc.summarize_fee_drag([None], [None]) is None
    assert dc.summarize_fee_drag([], []) is None
