"""Unit tests for the M6 exit-policy A/B pure core (#1066).

Everything under test here is pure (operates on lists of plain dicts / floats),
so no data access, registry, or backtester is needed — same architecture as
test_eval_windows / test_exit_diagnostics.
"""

import math

import pandas as pd
import pytest

import exit_policy_ab as m


# --------------------------------------------------------------------------
# sign_test
# --------------------------------------------------------------------------

def test_sign_test_all_positive_is_significant():
    r = m.sign_test([0.5, 1.0, 2.0, 0.1, 0.3])
    assert r["n_pos"] == 5 and r["n_neg"] == 0 and r["n_zero"] == 0
    assert r["n"] == 5
    # 2 * (0.5^5) = 0.0625
    assert r["p_value"] == pytest.approx(0.0625, abs=1e-6)


def test_sign_test_balanced_is_not_significant():
    r = m.sign_test([1.0, -1.0, 2.0, -2.0])
    assert r["n_pos"] == 2 and r["n_neg"] == 2
    assert r["p_value"] == pytest.approx(1.0)


def test_sign_test_drops_zeros_not_splits():
    r = m.sign_test([0.0, 0.0, 1.0, 2.0])
    assert r["n_zero"] == 2 and r["n"] == 2 and r["n_pos"] == 2
    assert r["p_value"] == pytest.approx(0.5)  # 2 * 0.25


def test_sign_test_empty():
    r = m.sign_test([])
    assert r["n"] == 0 and r["p_value"] == 1.0


# --------------------------------------------------------------------------
# wilcoxon_signed_rank
# --------------------------------------------------------------------------

def test_signed_rank_all_positive_low_p():
    r = m.wilcoxon_signed_rank([1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0])
    assert r["n"] == 8
    # W+ is the full rank sum n(n+1)/2 = 36; should be a strong, low-p signal.
    assert r["w"] == pytest.approx(36.0)
    assert r["z"] > 0
    assert r["p_value"] < 0.05


def test_signed_rank_symmetric_high_p():
    r = m.wilcoxon_signed_rank([1.0, -1.0, 2.0, -2.0, 3.0, -3.0])
    # Perfectly symmetric magnitudes/signs → W+ at the mean → p ~ 1.
    assert r["p_value"] == pytest.approx(1.0, abs=0.05)


def test_signed_rank_drops_zeros():
    a = m.wilcoxon_signed_rank([0.0, 0.0, 1.0, 2.0, 3.0])
    b = m.wilcoxon_signed_rank([1.0, 2.0, 3.0])
    assert a["n"] == b["n"] == 3


def test_signed_rank_handles_ties():
    # Tie group on |d|=1 must not blow up the variance term.
    r = m.wilcoxon_signed_rank([1.0, 1.0, 1.0, -1.0, 2.0])
    assert r["n"] == 5
    assert 0.0 <= r["p_value"] <= 1.0


def test_signed_rank_empty_is_undefined():
    r = m.wilcoxon_signed_rank([])
    assert r["n"] == 0 and r["p_value"] == 1.0


# --------------------------------------------------------------------------
# bootstrap_ci / unpaired_diff_ci
# --------------------------------------------------------------------------

def test_bootstrap_point_is_mean_and_deterministic():
    xs = [1.0, 2.0, 3.0, 4.0, 5.0]
    a = m.bootstrap_ci(xs, n_resamples=500, seed=7)
    b = m.bootstrap_ci(xs, n_resamples=500, seed=7)
    assert a == b  # deterministic given the seed
    assert a["point"] == pytest.approx(3.0)
    assert a["lo"] <= a["point"] <= a["hi"]


def test_bootstrap_point_is_seed_independent_and_brackets():
    # The point estimate is the sample mean regardless of seed; both seeds'
    # CIs must bracket it (the CI width can coincide on tiny samples, so we
    # assert the genuine invariants, not that two seeds differ).
    xs = [1.0, -2.0, 3.0, -4.0, 5.0, 0.5]
    a = m.bootstrap_ci(xs, n_resamples=500, seed=1)
    b = m.bootstrap_ci(xs, n_resamples=500, seed=2)
    assert a["point"] == b["point"]
    assert a["lo"] <= a["point"] <= a["hi"]
    assert b["lo"] <= b["point"] <= b["hi"]


def test_bootstrap_single_sample_collapses():
    r = m.bootstrap_ci([2.5], n_resamples=100)
    assert r["point"] == r["lo"] == r["hi"] == 2.5
    assert r["n_resamples"] == 0


def test_bootstrap_empty():
    r = m.bootstrap_ci([], n_resamples=100)
    assert r["point"] is None and r["lo"] is None


def test_unpaired_diff_point_is_difference_of_means():
    control = [1.0, 1.0, 1.0, 1.0]
    candidate = [3.0, 3.0, 3.0, 3.0]
    r = m.unpaired_diff_ci(control, candidate, n_resamples=300, seed=3)
    assert r["point"] == pytest.approx(2.0)
    # Both arms constant → the CI collapses on 2.0.
    assert r["lo"] == pytest.approx(2.0) and r["hi"] == pytest.approx(2.0)


def test_unpaired_diff_one_empty_arm():
    r = m.unpaired_diff_ci([], [2.0, 4.0], n_resamples=10)
    assert r["point"] == pytest.approx(3.0)
    assert r["lo"] is None


# --------------------------------------------------------------------------
# collapse_entry / group_entries / free_arm_entries
# --------------------------------------------------------------------------

def _leg(entry_date="2025-01-01", side="long", pnl_pct=2.0, shares=1.0,
         entry_price=100.0, exit_price=102.0, entry_fee=0.1, exit_fee=0.1,
         mfe_pct=3.0, mae_pct=-1.0, bars_held=5, exit_reason="tp"):
    return {
        "entry_date": entry_date, "side": side, "pnl_pct": pnl_pct,
        "shares": shares, "entry_price": entry_price, "exit_price": exit_price,
        "entry_fee": entry_fee, "exit_fee": exit_fee, "mfe_pct": mfe_pct,
        "mae_pct": mae_pct, "bars_held": bars_held, "exit_reason": exit_reason,
    }


def test_collapse_single_leg_matches_trade_metrics_net():
    leg = _leg()
    rec = m.collapse_entry([leg])
    tm = m.trade_metrics(leg)
    assert rec["net_pct"] == pytest.approx(tm["net_pct"])
    assert rec["side"] == "long" and rec["n_legs"] == 1
    assert rec["mfe_pct"] == 3.0 and rec["mae_pct"] == -1.0


def test_collapse_multi_leg_notional_weighted():
    # Two partial closes of one entry: 3 shares @100 then 1 share @100.
    leg_a = _leg(shares=3.0, pnl_pct=1.0, mfe_pct=2.0, mae_pct=-0.5, bars_held=4,
                 entry_fee=0.0, exit_fee=0.0, exit_price=101.0)
    leg_b = _leg(shares=1.0, pnl_pct=5.0, mfe_pct=6.0, mae_pct=-2.0, bars_held=9,
                 entry_fee=0.0, exit_fee=0.0, exit_price=105.0)
    rec = m.collapse_entry([leg_a, leg_b])
    # Notional-weighted net%: (1.0*300 + 5.0*100) / 400 = 2.0 (fees zero here).
    assert rec["net_pct"] == pytest.approx(2.0, abs=1e-6)
    assert rec["mfe_pct"] == 6.0          # max favourable across legs
    assert rec["mae_pct"] == -2.0         # most adverse across legs
    assert rec["bars_held"] == 9          # longest hold
    assert rec["n_legs"] == 2


def test_collapse_empty_returns_none():
    assert m.collapse_entry([]) is None
    assert m.collapse_entry([None]) is None


def test_group_and_free_arm_entries_orders_by_entry():
    trades = [
        _leg(entry_date="2025-01-01"),
        _leg(entry_date="2025-01-05"),
        _leg(entry_date="2025-01-01"),  # second partial of the first entry
    ]
    groups = m.group_entries(trades)
    assert list(groups.keys()) == ["2025-01-01", "2025-01-05"]
    assert len(groups["2025-01-01"]) == 2
    entries = m.free_arm_entries(trades)
    assert [e["entry_date"] for e in entries] == ["2025-01-01", "2025-01-05"]


# --------------------------------------------------------------------------
# build_paired_rows
# --------------------------------------------------------------------------

def test_build_paired_rows_pairs_and_counts_unmatched():
    control = [
        {"entry_date": "d1", "side": "long", "net_pct": 1.0, "mfe_pct": 2.0,
         "mae_pct": -1.0, "bars_held": 3},
        {"entry_date": "d2", "side": "long", "net_pct": -1.0, "mfe_pct": 0.5,
         "mae_pct": -2.0, "bars_held": 4},
    ]
    candidate_by_date = {
        "d1": {"net_pct": 2.5, "mfe_pct": 3.0, "mae_pct": -0.5, "bars_held": 6},
        "d2": None,  # replay produced no trade → unmatched
    }
    regime_by_date = {"d1": "ranging_quiet"}
    rows, diag = m.build_paired_rows(control, candidate_by_date, regime_by_date)
    assert diag == {"schedule_entries": 2, "paired": 1, "unmatched": 1}
    assert len(rows) == 1
    assert rows[0]["regime"] == "ranging_quiet"
    assert rows[0]["delta_net_pct"] == pytest.approx(1.5)


def test_build_paired_rows_unknown_regime_label():
    control = [{"entry_date": "d1", "side": "long", "net_pct": 1.0, "mfe_pct": 1.0,
                "mae_pct": -1.0, "bars_held": 2}]
    rows, _ = m.build_paired_rows(
        control, {"d1": {"net_pct": 1.0, "mfe_pct": 1.0, "mae_pct": -1.0,
                         "bars_held": 2}}, {})
    assert rows[0]["regime"] == m.UNKNOWN_REGIME


# --------------------------------------------------------------------------
# per_regime_table
# --------------------------------------------------------------------------

def _row(regime, ctrl, cand, mfe=2.0, mae=-1.0):
    return {"entry_date": "d", "regime": regime, "side": "long",
            "control_net_pct": ctrl, "candidate_net_pct": cand,
            "delta_net_pct": cand - ctrl, "control_mfe_pct": mfe,
            "candidate_mfe_pct": mfe, "control_mae_pct": mae,
            "candidate_mae_pct": mae, "control_bars_held": 3,
            "candidate_bars_held": 3}


def test_per_regime_table_buckets_and_all_and_sorted():
    rows = [
        _row("trending", 1.0, 2.0),
        _row("ranging", 0.0, -1.0),
        _row("trending", 2.0, 4.0),
    ]
    table = m.per_regime_table(rows, n_resamples=200, seed=5)
    assert list(table["by_regime"].keys()) == ["ranging", "trending"]  # sorted
    assert table["by_regime"]["trending"]["n"] == 2
    assert table["all"]["n"] == 3
    assert table["by_regime"]["trending"]["candidate_mean_net_pct"] == pytest.approx(3.0)
    # Δ mean over all three: (1 + -1 + 2)/3
    assert table["all"]["paired_delta"]["mean"] == pytest.approx(2.0 / 3.0, abs=1e-6)


def test_per_regime_win_rate_delta():
    rows = [_row("r", -1.0, 1.0), _row("r", -2.0, 2.0)]
    blk = m.per_regime_table(rows)["by_regime"]["r"]
    assert blk["control_win_rate"] == 0.0
    assert blk["candidate_win_rate"] == 1.0
    assert blk["delta_win_rate"] == pytest.approx(1.0)


# --------------------------------------------------------------------------
# arm_summary
# --------------------------------------------------------------------------

def test_arm_summary_passes_max_dd_and_computes_winrate():
    results = {
        "total_trades": 2, "total_return_pct": 5.0, "max_drawdown_pct": -3.0,
        "sharpe_ratio": 1.2, "liquidated": False,
        "trades": [_leg(entry_date="d1", pnl_pct=2.0),
                   _leg(entry_date="d2", pnl_pct=-1.0, exit_price=99.0)],
    }
    s = m.arm_summary(results)
    assert s["entries"] == 2
    assert s["max_drawdown_pct"] == -3.0
    assert 0.0 <= s["win_rate"] <= 1.0


def test_arm_summary_none_results():
    s = m.arm_summary(None)
    assert s["entries"] == 0 and s["win_rate"] is None and s["max_drawdown_pct"] is None


# --------------------------------------------------------------------------
# candidate_is_replayable
# --------------------------------------------------------------------------

def test_replayable_true_for_rule_based_exits():
    assert m.candidate_is_replayable([{"name": "atr_stop", "params": {}}])
    assert m.candidate_is_replayable(
        [{"name": "tiered_tp_atr", "params": {}},
         {"name": "trailing_stop_atr_mult", "params": {"atr_mult": 3}}])


def test_replayable_false_for_open_as_close_and_unknown():
    assert not m.candidate_is_replayable(None)
    assert not m.candidate_is_replayable([])
    assert not m.candidate_is_replayable([{"name": "some_signal_reversal_close"}])


def test_replayable_true_for_ratchet_and_frozen_regime_tp():
    # #1152: the ratchet ladders and the frozen-at-open regime TP fire off
    # price/ATR/the regime stamped at open (bar data), never off later signals,
    # so the single-entry replay isolates them faithfully.
    assert m.candidate_is_replayable([{"name": "trailing_tp_ratchet", "params": {}}])
    assert m.candidate_is_replayable(
        [{"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}])
    assert m.candidate_is_replayable([{"name": "tiered_tp_atr_regime", "params": {}}])


def test_replayable_false_for_per_tick_regime_variants():
    # Per-tick re-resolution variants stay out of the set until validated:
    # tiered_tp_atr_live_regime_dynamic is HL-live-only (load_strategy_config
    # rejects it), and tiered_tp_atr_live_regime has no #1152 evidence run.
    assert not m.candidate_is_replayable([{"name": "tiered_tp_atr_live_regime"}])
    assert not m.candidate_is_replayable(
        [{"name": "tiered_tp_atr_live_regime_dynamic"}])


# --------------------------------------------------------------------------
# paired_delta_summary structure
# --------------------------------------------------------------------------

def test_paired_delta_summary_shape():
    s = m.paired_delta_summary([1.0, 2.0, -0.5, 3.0], n_resamples=200, seed=9)
    assert set(s) == {"n", "mean", "median", "sign_test", "signed_rank", "bootstrap"}
    assert s["n"] == 4
    assert s["sign_test"]["n_pos"] == 3 and s["sign_test"]["n_neg"] == 1


def test_paired_delta_summary_empty():
    s = m.paired_delta_summary([])
    assert s["n"] == 0 and s["mean"] is None


# --------------------------------------------------------------------------
# resolve_regime_cfg — classifier/windows_spec consistency (composite kwarg is
# dead in regime.py, so composite MUST carry a non-None windows_spec).
# --------------------------------------------------------------------------

def _regime_args(classifier=None, windows_json=None, period=14, adx=20.0, gate_window=None):
    import types
    return types.SimpleNamespace(
        regime_classifier=classifier, regime_windows_json=windows_json,
        regime_period=period, regime_adx_threshold=adx, gate_window=gate_window)


def test_resolve_regime_adx_default():
    cfg = m.resolve_regime_cfg(_regime_args(), {})
    assert cfg["classifier"] == "adx" and cfg["windows_spec"] is None


def test_resolve_regime_composite_synthesizes_windows_spec():
    cfg = m.resolve_regime_cfg(_regime_args(classifier="composite", period=20), {})
    # The bare classifier= kwarg is ignored downstream; composite must ship a spec.
    assert cfg["classifier"] == "composite"
    assert cfg["windows_spec"] is not None
    spec = next(iter(cfg["windows_spec"].values()))
    assert spec["classifier"] == "composite" and spec["period"] == 20
    assert cfg["gate_window"] == "attribution"


def test_resolve_regime_inherits_config_windows():
    windows = {"medium": {"classifier": "composite", "period": 14}}
    cfg = m.resolve_regime_cfg(_regime_args(), {"windows": windows})
    assert cfg["classifier"] == "composite" and cfg["windows_spec"] == windows


def test_resolve_regime_explicit_windows_json_wins():
    spec = {"fast": {"classifier": "composite", "period": 7}}
    cfg = m.resolve_regime_cfg(_regime_args(windows_json=__import__("json").dumps(spec)), {})
    assert cfg["classifier"] == "composite" and cfg["windows_spec"] == spec


def test_resolve_regime_explicit_adx_overrides_config_windows():
    # An explicit --regime-classifier adx must NOT inherit the config's composite.
    cfg = m.resolve_regime_cfg(_regime_args(classifier="adx"),
                               {"windows": {"medium": {"classifier": "composite", "period": 14}}})
    assert cfg["classifier"] == "adx" and cfg["windows_spec"] is None


# --------------------------------------------------------------------------
# Incumbent stop-field fidelity (#1066 finding-1): the control arm must replay
# the incumbent's strategy-level stops, never a phantom subset.
# --------------------------------------------------------------------------

def test_stops_from_kwargs_collects_all_present_and_drops_none():
    # A load_strategy_config result carries every STOP_FIELD_KEYS entry; keep the
    # present ones (here an ATR stop AND a regime trailing stop) and drop None.
    kwargs = {
        "open_strategy": {"name": "x", "params": {}},
        "close_strategies": [{"name": "tiered_tp_atr", "params": {}}],
        "stop_loss_atr_mult": 1.5,
        "stop_loss_pct": None,
        "stop_loss_margin_pct": None,
        "trailing_stop_atr_mult": None,
        "trailing_stop_pct": None,
        "stop_loss_atr_regime": None,
        "trailing_stop_atr_regime": {"trending_up": 2.0, "ranging_quiet": 3.0},
    }
    stops = m._stops_from_kwargs(kwargs)
    assert stops == {"stop_loss_atr_mult": 1.5,
                     "trailing_stop_atr_regime": {"trending_up": 2.0, "ranging_quiet": 3.0}}
    # No spurious None keys leak through (would override the Backtester defaults).
    assert all(v is not None for v in stops.values())


def test_stops_from_kwargs_empty_when_no_stops():
    assert m._stops_from_kwargs({"open_strategy": {}, "close_strategies": None}) == {}


def test_candidate_stops_inherit_copies_and_drop_clears():
    incumbent = {"stop_loss_atr_mult": 1.5}
    inh = m._candidate_stops("inherit", incumbent)
    assert inh == incumbent and inh is not incumbent  # copy, not alias
    assert m._candidate_stops("drop", incumbent) == {}
    # Defensive: None incumbent never raises.
    assert m._candidate_stops("inherit", None) == {}


def test_backtester_kwargs_threads_present_stops_only():
    kw = m._backtester_kwargs(
        "sqz", {}, [{"name": "tiered_tp_atr", "params": {}}], "long", 10000.0,
        {"allowed_regimes": None}, stops={"stop_loss_atr_mult": 1.5})
    assert kw["stop_loss_atr_mult"] == 1.5
    # Absent stop fields must NOT appear (so the Backtester keeps its None default).
    assert "trailing_stop_atr_mult" not in kw
    assert "stop_loss_pct" not in kw


def test_backtester_kwargs_no_stops_means_no_stop_keys():
    kw = m._backtester_kwargs("sqz", {}, None, "long", 10000.0,
                              {"allowed_regimes": None}, stops=None)
    assert not any(k in kw for k in m.STOP_FIELD_KEYS)


def test_backtester_kwargs_threads_regime_trailing_stop_with_open_as_close():
    # Must-survive (b): incumbent has NO close evaluator but a regime trailing
    # stop — the control arm must still carry that stop.
    regime_trail = {"trending_up": 2.0, "ranging_quiet": 3.0}
    kw = m._backtester_kwargs("sqz", {}, None, "long", 10000.0,
                              {"allowed_regimes": None},
                              stops={"trailing_stop_atr_regime": regime_trail})
    assert kw["close_strategies"] is None
    assert kw["trailing_stop_atr_regime"] == regime_trail


def test_control_keeps_stop_regardless_of_candidate_mode():
    # Must-survive (c): candidate = atr_stop, incumbent's only protection was a
    # strategy-level stop_loss_atr_mult. The control arm must carry that stop in
    # BOTH candidate modes, so the candidate never gets false credit for re-adding
    # protection the control silently lacked.
    incumbent_stops = {"stop_loss_atr_mult": 1.5}
    control_kw = m._backtester_kwargs(
        "sqz", {}, [{"name": "tiered_tp_atr", "params": {}}], "long", 10000.0,
        {"allowed_regimes": None}, stops=incumbent_stops)
    assert control_kw["stop_loss_atr_mult"] == 1.5

    inherit_kw = m._backtester_kwargs(
        "sqz", {}, [{"name": "atr_stop", "params": {}}], "long", 10000.0,
        {"allowed_regimes": None},
        stops=m._candidate_stops("inherit", incumbent_stops))
    drop_kw = m._backtester_kwargs(
        "sqz", {}, [{"name": "atr_stop", "params": {}}], "long", 10000.0,
        {"allowed_regimes": None},
        stops=m._candidate_stops("drop", incumbent_stops))
    assert inherit_kw["stop_loss_atr_mult"] == 1.5      # held fixed
    assert "stop_loss_atr_mult" not in drop_kw          # full replacement


# --------------------------------------------------------------------------
# _resolve_spec wiring (explicit-close path needs no config file / registry).
# --------------------------------------------------------------------------

def _spec_args(extra=None):
    base = [
        "--strategy", "squeeze_momentum",
        "--incumbent-close", "none",
        "--candidate-close", '[{"name":"atr_stop","params":{}}]',
    ]
    return m.build_parser().parse_args(base + (extra or []))


def test_resolve_spec_explicit_path_has_no_stops():
    spec = m._resolve_spec(_spec_args())
    # The explicit --incumbent-close path resolves no strategy-level stops.
    assert spec["control_stops"] == {} and spec["candidate_stops"] == {}
    assert spec["candidate_stops_mode"] == "inherit"


# --------------------------------------------------------------------------
# Gate-window / attribution divergence (#1066 finding-2): a named --gate-window
# on a multi-window spec steers attribution to a window the backtester gate can't
# honor → reject loudly.
# --------------------------------------------------------------------------

_MULTI_WINDOW = ('{"fast":{"classifier":"composite","period":7},'
                 '"slow":{"classifier":"composite","period":21}}')
_SINGLE_WINDOW = '{"slow":{"classifier":"composite","period":21}}'


def test_gate_window_on_multi_window_spec_rejected():
    # Must-survive (a)/(b): naming either window of a multi-window spec must error,
    # because the gate default-picks its primary window irrespective of the name.
    with pytest.raises(SystemExit):
        m._resolve_spec(_spec_args(["--regime-windows-json", _MULTI_WINDOW,
                                    "--gate-window", "slow"]))
    with pytest.raises(SystemExit):
        m._resolve_spec(_spec_args(["--regime-windows-json", _MULTI_WINDOW,
                                    "--gate-window", "fast"]))


def test_multi_window_without_gate_window_is_allowed():
    # Must-survive (c): no --gate-window → gate and attribution both default-pick
    # the same primary window → agree → no rejection.
    spec = m._resolve_spec(_spec_args(["--regime-windows-json", _MULTI_WINDOW]))
    assert len(spec["regime_cfg"]["windows_spec"]) == 2


def test_gate_window_naming_absent_window_rejected():
    with pytest.raises(SystemExit):
        m._resolve_spec(_spec_args(["--regime-windows-json", _SINGLE_WINDOW,
                                    "--gate-window", "nope"]))


def test_gate_window_on_single_window_spec_naming_that_window_ok():
    spec = m._resolve_spec(_spec_args(["--regime-windows-json", _SINGLE_WINDOW,
                                       "--gate-window", "slow"]))
    assert list(spec["regime_cfg"]["windows_spec"].keys()) == ["slow"]


# --------------------------------------------------------------------------
# Unreplayable entry-shaper reject (#1066 re-review finding-1): a baseline
# incumbent using invert_signal / regime_directional_policy / profile_allocation
# cannot be replayed faithfully → refuse, never silently compare a phantom.
# --------------------------------------------------------------------------

def test_reject_invert_signal_incumbent():
    # Must-survive (a): invert_signal=true → the run refuses (control would trade
    # the opposite side from live).
    with pytest.raises(SystemExit):
        m._reject_unreplayable_entry_shapers({"invert_signal": True})


def test_reject_regime_directional_policy_incumbent():
    # Must-survive (b): a regime_directional_policy is flagged, not silently dropped.
    with pytest.raises(SystemExit):
        m._reject_unreplayable_entry_shapers(
            {"regime_directional_policy": {"trending_up": "long"}})


def test_reject_profile_allocation_incumbent():
    with pytest.raises(SystemExit):
        m._reject_unreplayable_entry_shapers(
            {"profile_allocation": {"window": "long", "param_sets": {}}})


def test_reject_names_all_offenders():
    with pytest.raises(SystemExit) as ei:
        m._reject_unreplayable_entry_shapers(
            {"invert_signal": True,
             "regime_directional_policy": {"x": "long"},
             "profile_allocation": {"y": 1}})
    msg = str(ei.value)
    assert "invert_signal" in msg and "regime_directional_policy" in msg \
        and "profile_allocation" in msg


def test_no_reject_when_entry_shapers_absent_or_falsy():
    # Must-survive (c): none set (or explicitly falsy/empty) → runs silently.
    m._reject_unreplayable_entry_shapers({"open_strategy": {"name": "x"}})
    m._reject_unreplayable_entry_shapers(
        {"invert_signal": False, "regime_directional_policy": None,
         "profile_allocation": {}})


# --------------------------------------------------------------------------
# Stop-class candidate stacking warning predicate (#1066 re-review finding-2).
# --------------------------------------------------------------------------

def test_stop_candidate_stacks_under_inherited_stop():
    # Must-survive (a): atr_stop candidate + inherited scalar stop → stacks.
    assert m._candidate_stacks_on_inherited_stop(
        [{"name": "atr_stop", "params": {}}], "inherit", {"stop_loss_atr_mult": 1.5})
    assert m._candidate_stacks_on_inherited_stop(
        [{"name": "trailing_stop_atr_regime", "params": {}}], "inherit",
        {"trailing_stop_atr_mult": 2.0})


def test_tp_ladder_candidate_does_not_stack():
    # Must-survive (b): a take-profit ladder is not a downside stop → no warning.
    assert not m._candidate_stacks_on_inherited_stop(
        [{"name": "tiered_tp_atr", "params": {}}], "inherit", {"stop_loss_atr_mult": 1.5})
    assert not m._candidate_stacks_on_inherited_stop(
        [{"name": "zscore_target", "params": {}}], "inherit", {"stop_loss_atr_mult": 1.5})


def test_no_stack_warning_when_dropped_or_no_inherited_stop():
    # Must-survive (c): drop mode → candidate stop measured alone → no warning.
    assert not m._candidate_stacks_on_inherited_stop(
        [{"name": "atr_stop", "params": {}}], "drop", {"stop_loss_atr_mult": 1.5})
    # No inherited stop at all → nothing to stack under.
    assert not m._candidate_stacks_on_inherited_stop(
        [{"name": "atr_stop", "params": {}}], "inherit", {})
    # Open-as-close candidate → no candidate stop.
    assert not m._candidate_stacks_on_inherited_stop(None, "inherit", {"stop_loss_atr_mult": 1.5})


# --------------------------------------------------------------------------
# Per-entry replay alignment (#1066 re-review finding-3): positions/regime MUST be
# derived from df_signals (the frame the backtester indexes), not the pre-
# apply_strategy df — else a row-dropping strategy silently misaligns every replay.
# --------------------------------------------------------------------------

def test_replay_positions_anchored_on_df_signals_not_df(monkeypatch):
    import data_fetcher

    # df has 10 bars; the open strategy "drops" the first 2 warmup bars, so
    # df_signals has 8 bars on a SHIFTED positional index. The control entry is
    # bar T5 — position 5 in df, position 3 in df_signals.
    idx = pd.date_range("2025-01-01", periods=10, freq="h")
    df = pd.DataFrame({"close": range(10)}, index=idx)
    df_signals = df.iloc[2:].copy()
    df_signals["signal"] = 0
    entry_ts = str(df.index[5])  # == str(df_signals.index[3])

    captured = {}

    def fake_prepare(reg, open_name, params, _df):
        return df_signals

    def fake_regime_series(frame, regime_cfg):
        captured["regime_frame_len"] = len(frame)
        return ["r"] * len(frame)

    def fake_run_free_arm(reg, open_name, params, sig, close_refs, direction,
                          capital, gate, symbol, timeframe, stops=None):
        return {"total_trades": 1, "total_return_pct": 0.0, "max_drawdown_pct": 0.0,
                "sharpe_ratio": 0.0, "liquidated": False,
                "trades": [_leg(entry_date=entry_ts, side="long")]}

    def fake_replay(reg, open_name, params, sig, sig_pos, side_sign, candidate_close,
                    direction, capital, gate, symbol, timeframe, stops=None):
        captured["sig_pos"] = sig_pos
        return {"net_pct": 1.0, "mfe_pct": 1.0, "mae_pct": -1.0, "bars_held": 2}

    monkeypatch.setattr(data_fetcher, "load_cached_data", lambda *a, **k: df)
    monkeypatch.setattr(m, "_prepare_signals", fake_prepare)
    monkeypatch.setattr(m, "_regime_label_series", fake_regime_series)
    monkeypatch.setattr(m, "run_free_arm", fake_run_free_arm)
    monkeypatch.setattr(m, "replay_candidate_for_entry", fake_replay)

    spec = {
        "open_name": "test_open", "params": None, "direction": "long",
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close": [{"name": "atr_stop", "params": {}}],
        "control_stops": {}, "candidate_stops": {}, "replayable": True,
        "gate": {"allowed_regimes": None}, "regime_cfg": {"classifier": "adx"},
        "capital": 10000.0, "n_resamples": 50, "ci": 0.95, "seed": 1,
    }
    res = m.evaluate_dataset_window(object(), spec, "BTC/USDT", "1h",
                                    ("2025-01-01", None))
    # The forced entry must land at df_signals position 3 − 1 = 2; the df-based
    # bug would have produced 5 − 1 = 4.
    assert captured["sig_pos"] == 2
    assert captured["regime_frame_len"] == 8  # df_signals, not df (10)
    assert res is not None and res["paired_diag"]["paired"] == 1
