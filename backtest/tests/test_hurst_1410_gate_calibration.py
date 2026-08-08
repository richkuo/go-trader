"""#1410: pure helpers of the Hurst gate-calibration study.

Covers the bucketing, hysteresis, entry masking, look-ahead alignment, entry
dedup, compounding, chop-loss, permutation inference, and the recommendation
renderer. The EMPIRICAL result of the study is never asserted here — only the
machinery that turns numbers into a verdict.

Imported the same way test_regime_1076_certify.py imports its research module
(explicit research/ on sys.path, unambiguous module name — safe under the
#1304 `-n auto` parallel run).
"""
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import hurst_1410_gate_calibration as study  # noqa: E402

_UNSET = object()


# ---------------------------------------------------------------------------
# bucket_label
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("value,expected", [
    (0.0, "<0.45"),
    (0.4499999, "<0.45"),
    (0.45, "0.45-0.50"),
    (0.4999999, "0.45-0.50"),
    (0.50, "0.50-0.55"),
    (0.5499999, "0.50-0.55"),
    (0.55, ">=0.55"),
    (1.2, ">=0.55"),
])
def test_bucket_label_boundaries_are_half_open_upward(value, expected):
    assert study.bucket_label(value) == expected


def test_nan_is_its_own_bucket_and_never_becomes_a_half():
    for missing in (float("nan"), np.nan, None, float("inf"), float("-inf")):
        assert study.bucket_label(missing) == study.BUCKET_NAN
    # The NaN bucket must not collide with the bucket a literal 0.5 lands in.
    assert study.bucket_label(0.5) == "0.50-0.55"
    assert study.BUCKET_NAN != study.bucket_label(0.5)


def test_every_bucket_label_is_declared():
    for value in (0.1, 0.46, 0.51, 0.9, float("nan")):
        assert study.bucket_label(value) in study.BUCKETS


# ---------------------------------------------------------------------------
# hysteresis_mask
# ---------------------------------------------------------------------------

def test_hysteresis_high_sense_arms_and_disarms():
    # arm 0.55 / disarm 0.50: 0.52 is inside the band and holds the state.
    values = [0.40, 0.52, 0.56, 0.52, 0.49, 0.52]
    mask = study.hysteresis_mask(values, 0.55, 0.50, study.SENSE_HIGH)
    # starts armed -> 0.40 disarms -> 0.52 holds -> 0.56 arms -> 0.52 holds
    # -> 0.49 disarms -> 0.52 holds
    assert mask.tolist() == [False, False, True, True, False, False]


def test_hysteresis_low_sense_is_mirrored():
    # arm 0.45 / disarm 0.50: arms on LOW H, disarms on HIGH H.
    values = [0.60, 0.48, 0.44, 0.48, 0.55, 0.48]
    mask = study.hysteresis_mask(values, 0.45, 0.50, study.SENSE_LOW)
    assert mask.tolist() == [False, False, True, True, False, False]


def test_hysteresis_starts_armed():
    assert study.GATE_INITIAL_ARMED is True
    mask = study.hysteresis_mask([float("nan")], 0.55, 0.50, study.SENSE_HIGH)
    assert mask.tolist() == [True]
    # And a first bar inside the hold band also keeps the initial state.
    mask = study.hysteresis_mask([0.52], 0.55, 0.50, study.SENSE_HIGH)
    assert mask.tolist() == [True]


def test_nan_holds_gate_state_in_both_senses():
    high = study.hysteresis_mask(
        [0.40, float("nan"), float("nan"), 0.56, float("nan")],
        0.55, 0.50, study.SENSE_HIGH)
    assert high.tolist() == [False, False, False, True, True]
    low = study.hysteresis_mask(
        [0.60, None, 0.44, float("nan")], 0.45, 0.50, study.SENSE_LOW)
    assert low.tolist() == [False, False, True, True]


def test_pre_registered_pairs_are_valid_in_their_family_sense():
    for family, pairs in study.GATE_PAIRS.items():
        sense = study.FAMILY_SENSE[family]
        for arm, disarm in pairs:
            study.validate_gate_pair(arm, disarm, sense)


@pytest.mark.parametrize("arm,disarm,sense", [
    (0.50, 0.55, study.SENSE_HIGH),   # not hysteresis in the high sense
    (0.55, 0.50, study.SENSE_LOW),    # not hysteresis in the low sense
    (0.55, 0.55, study.SENSE_HIGH),
])
def test_invalid_gate_pairs_are_rejected(arm, disarm, sense):
    with pytest.raises(ValueError):
        study.hysteresis_mask([0.5], arm, disarm, sense)


def test_unknown_sense_is_rejected():
    with pytest.raises(ValueError):
        study.hysteresis_mask([0.5], 0.55, 0.50, "sideways")


# ---------------------------------------------------------------------------
# mask_entry_signals
# ---------------------------------------------------------------------------

def test_only_entries_are_suppressed_and_only_while_disarmed():
    signal = [1, 1, -1, -1, 0, 1]
    armed = [True, False, True, False, False, True]
    out = study.mask_entry_signals(signal, armed)
    assert out.tolist() == [1, 0, -1, -1, 0, 1]


def test_close_signals_always_survive_a_fully_disarmed_gate():
    signal = [-1] * 5
    armed = [False] * 5
    assert study.mask_entry_signals(signal, armed).tolist() == [-1] * 5


def test_fully_armed_gate_is_a_no_op():
    signal = [1, -1, 0, 1, -1]
    out = study.mask_entry_signals(signal, [True] * 5)
    assert out.tolist() == signal


def test_mask_length_mismatch_is_rejected():
    with pytest.raises(ValueError):
        study.mask_entry_signals([1, 1, 1], [True, False])


# ---------------------------------------------------------------------------
# Look-ahead alignment
# ---------------------------------------------------------------------------

def _synthetic_walk(n=260, seed=7):
    rng = np.random.default_rng(seed)
    prices = 100.0 * np.exp(np.cumsum(rng.normal(0, 0.01, n)))
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    return pd.Series(prices, index=idx, name="close")


def test_rolling_hurst_value_uses_only_its_own_trailing_window():
    close = _synthetic_walk()
    window = 128
    rolling = study.rolling_hurst(close, window)
    target = 200
    # Perturbing a bar AFTER `target` cannot change H at `target`.
    mutated = close.copy()
    mutated.iloc[target + 1] *= 1.25
    assert study.rolling_hurst(mutated, window).iloc[target] == pytest.approx(
        rolling.iloc[target], abs=1e-12)


def test_decision_value_at_bar_n_is_invariant_to_bar_n_close():
    """The core look-ahead guard: a bar-N decision may not read bar N's close."""
    close = _synthetic_walk()
    window = 128
    target = 200
    base = study.decision_series(study.rolling_hurst(close, window))
    mutated = close.copy()
    mutated.iloc[target] *= 1.35  # a large, unmistakable move on the decision bar
    after = study.decision_series(study.rolling_hurst(mutated, window))
    assert math.isfinite(base.iloc[target])
    assert after.iloc[target] == pytest.approx(base.iloc[target], abs=1e-12)
    # Sanity: the BAR-CLOSE series at that bar DID move, so the invariance
    # above comes from the shift, not from an inert perturbation.
    assert (study.rolling_hurst(mutated, window).iloc[target]
            != pytest.approx(study.rolling_hurst(close, window).iloc[target],
                             abs=1e-9))


def test_entry_stamp_is_the_signal_bar_decision_value():
    """A trade fills at bar N+1; it must be stamped with bar N's decision."""
    rolling = pd.Series([0.1, 0.2, 0.3, 0.4, 0.5])
    decision = study.decision_series(rolling)
    stamp = study.entry_stamp_series(rolling)
    fill_bar = 3
    signal_bar = fill_bar - 1
    assert stamp.iloc[fill_bar] == decision.iloc[signal_bar]
    assert stamp.iloc[fill_bar] == rolling.iloc[fill_bar - 2]
    assert math.isnan(stamp.iloc[0]) and math.isnan(stamp.iloc[1])


def test_rolling_hurst_is_nan_before_the_window_fills():
    close = _synthetic_walk(n=160)
    rolling = study.rolling_hurst(close, 128)
    assert rolling.iloc[:127].isna().all()


def test_first_needed_trim_never_changes_a_value_a_caller_reads():
    close = _synthetic_walk()
    window = 128
    full = study.rolling_hurst(close, window)
    trimmed = study.rolling_hurst(close, window, first_needed=close.index[220])
    for i in range(220, len(close)):
        assert trimmed.iloc[i] == pytest.approx(full.iloc[i], abs=1e-12)


def test_rolling_hurst_rejects_a_degenerate_window():
    with pytest.raises(ValueError):
        study.rolling_hurst(_synthetic_walk(n=10), 1)


# ---------------------------------------------------------------------------
# slice_window
# ---------------------------------------------------------------------------

def _frame(n=40, start="2024-12-20"):
    idx = pd.date_range(start, periods=n, freq="1D")
    return pd.DataFrame({"close": np.arange(n, dtype=float)}, index=idx)


def test_window_end_is_exclusive_so_adjacent_windows_never_overlap():
    df = _frame()
    left = study.slice_window(df, ("2024-12-20", "2025-01-01"))
    right = study.slice_window(df, ("2025-01-01", "2025-01-10"))
    assert pd.Timestamp("2025-01-01") not in left.index
    assert right.index[0] == pd.Timestamp("2025-01-01")
    assert not set(left.index) & set(right.index)


def test_open_ended_window_keeps_every_later_bar():
    df = _frame()
    out = study.slice_window(df, ("2025-01-01", None))
    assert out.index[-1] == df.index[-1]


# ---------------------------------------------------------------------------
# dedup_entries
# ---------------------------------------------------------------------------

def _entry(window, entry_date, strategy="momentum", symbol="BTC/USDT",
           timeframe="1h", pnl=1.0):
    return {"window": window, "entry_date": entry_date, "strategy": strategy,
            "symbol": symbol, "timeframe": timeframe, "pnl_pct_net": pnl}


def test_is_and_2025h1_overlap_collapses_to_the_earlier_window():
    """The concrete overlap: 2025H1 is 2025-01-01..2025-07-01, `is` starts
    2025-06-10, so mid-June entries appear in both."""
    shared = "2025-06-20 04:00:00"
    rows = [_entry("is", shared, pnl=5.0), _entry("2025H1", shared, pnl=5.0)]
    out = study.dedup_entries(rows)
    assert len(out) == 1
    assert out[0]["window"] == "2025H1"  # chronologically first window wins


def test_dedup_order_is_independent_of_input_order():
    shared = "2025-06-20 04:00:00"
    a = study.dedup_entries([_entry("is", shared), _entry("2025H1", shared)])
    b = study.dedup_entries([_entry("2025H1", shared), _entry("is", shared)])
    assert a == b


def test_distinct_physical_entries_are_never_collapsed():
    ts = "2025-06-20 04:00:00"
    rows = [
        _entry("is", ts, strategy="momentum"),
        _entry("is", ts, strategy="vol_momentum"),
        _entry("is", ts, symbol="ETH/USDT"),
        _entry("is", ts, timeframe="4h"),
        _entry("is", "2025-06-21 04:00:00"),
    ]
    assert len(study.dedup_entries(rows)) == 5


def test_dedup_of_an_empty_pool_is_empty():
    assert study.dedup_entries([]) == []


def test_window_order_is_chronological():
    starts = [study.WINDOWS[w][0] for w in study.WINDOW_ORDER]
    assert starts == sorted(starts)
    assert set(study.WINDOW_ORDER) == set(study.WINDOWS)


# ---------------------------------------------------------------------------
# compound_equity / chop_loss / size_multiplier
# ---------------------------------------------------------------------------

def test_compound_equity_matches_a_hand_computed_sequence():
    ret, dd = study.compound_equity([10.0, -10.0])
    # 1.10 * 0.90 = 0.99 -> -1%; peak 1.10 -> trough 0.99 -> -10%
    assert ret == pytest.approx(-1.0, abs=1e-9)
    assert dd == pytest.approx(-10.0, abs=1e-9)


def test_compound_equity_of_no_trades_is_flat():
    assert study.compound_equity([]) == (0.0, 0.0)


def test_multipliers_scale_each_trade():
    ret_full, _ = study.compound_equity([10.0, 10.0])
    ret_half, _ = study.compound_equity([10.0, 10.0], [0.5, 0.5])
    assert ret_full == pytest.approx(21.0, abs=1e-9)
    assert ret_half == pytest.approx(10.25, abs=1e-9)


def test_a_zero_multiplier_removes_a_trade_entirely():
    ret, dd = study.compound_equity([-50.0, 20.0], [0.0, 1.0])
    assert ret == pytest.approx(20.0, abs=1e-9)
    assert dd == pytest.approx(0.0, abs=1e-9)


def test_equity_is_floored_at_zero_and_stays_there():
    ret, dd = study.compound_equity([-90.0, -90.0, 500.0], [1.5, 1.5, 1.5])
    assert ret == pytest.approx(-100.0, abs=1e-9)
    assert dd == pytest.approx(-100.0, abs=1e-9)


def test_compound_equity_rejects_a_multiplier_count_mismatch():
    with pytest.raises(ValueError):
        study.compound_equity([1.0, 2.0], [1.0])


def test_chop_loss_sums_only_losing_trade_magnitudes():
    assert study.chop_loss([5.0, -2.0, 0.0, -3.5, 1.0]) == pytest.approx(5.5)
    assert study.chop_loss([]) == 0.0
    assert study.chop_loss([1.0, 2.0]) == 0.0


def test_size_multiplier_is_one_at_a_random_walk_reading():
    for sense in (study.SENSE_HIGH, study.SENSE_LOW):
        assert study.size_multiplier(0.5, sense, 5.0) == pytest.approx(1.0)


def test_size_multiplier_senses_are_mirrored():
    assert study.size_multiplier(0.6, study.SENSE_HIGH, 2.5) == pytest.approx(1.25)
    assert study.size_multiplier(0.6, study.SENSE_LOW, 2.5) == pytest.approx(0.75)
    assert study.size_multiplier(0.4, study.SENSE_LOW, 2.5) == pytest.approx(1.25)


def test_size_multiplier_clamps_both_ends():
    assert study.size_multiplier(0.99, study.SENSE_HIGH, 5.0) == pytest.approx(
        study.SIZING_CLAMP_HI)
    assert study.size_multiplier(0.01, study.SENSE_HIGH, 5.0) == pytest.approx(
        study.SIZING_CLAMP_LO)


def test_nan_size_multiplier_is_exactly_one_never_a_half_edge():
    for missing in (float("nan"), None, float("inf")):
        assert study.size_multiplier(missing, study.SENSE_HIGH, 5.0) == 1.0
    # A NaN reading must NOT be equivalent to feeding H=0.5 by accident of the
    # arithmetic — assert the policy explicitly, not the coincidence.
    assert study.SIZING_NAN_MULTIPLIER == 1.0


def test_size_multiplier_rejects_an_unknown_sense():
    with pytest.raises(ValueError):
        study.size_multiplier(0.6, "sideways", 2.5)


# ---------------------------------------------------------------------------
# Permutation inference
# ---------------------------------------------------------------------------

def test_group_permutation_is_seeded_and_reproducible():
    values = [1.0, -1.0] * 40
    suppressed = [True, False] * 40
    a = study.permutation_pvalue_group_diff(values, suppressed, n_perm=500, seed=1410)
    b = study.permutation_pvalue_group_diff(values, suppressed, n_perm=500, seed=1410)
    assert a == b


def test_group_permutation_detects_a_real_separation():
    rng = np.random.default_rng(3)
    good = list(rng.normal(4.0, 0.5, 60))
    bad = list(rng.normal(-4.0, 0.5, 60))
    values = good + bad
    suppressed = [False] * 60 + [True] * 60
    p = study.permutation_pvalue_group_diff(values, suppressed, n_perm=500, seed=1410)
    assert p < 0.01


def test_group_permutation_is_unimpressed_by_noise():
    rng = np.random.default_rng(11)
    values = list(rng.normal(0.0, 1.0, 200))
    suppressed = [i % 2 == 0 for i in range(200)]
    p = study.permutation_pvalue_group_diff(values, suppressed, n_perm=500, seed=1410)
    assert p > 0.05


def test_group_permutation_is_one_sided_against_the_wrong_direction():
    """When the SUPPRESSED side is the better one, p must be large."""
    values = [-5.0] * 40 + [5.0] * 40
    suppressed = [False] * 40 + [True] * 40  # suppressed group is the winner
    p = study.permutation_pvalue_group_diff(values, suppressed, n_perm=500, seed=1410)
    assert p > 0.9


def test_group_permutation_needs_both_groups():
    assert study.permutation_pvalue_group_diff([1.0, 2.0], [False, False]) is None
    assert study.permutation_pvalue_group_diff([1.0, 2.0], [True, True]) is None
    assert study.permutation_pvalue_group_diff([], []) is None


def test_group_permutation_p_is_never_zero():
    values = [100.0] * 30 + [-100.0] * 30
    suppressed = [False] * 30 + [True] * 30
    p = study.permutation_pvalue_group_diff(values, suppressed, n_perm=200, seed=1410)
    assert p > 0.0


def test_weighted_permutation_is_seeded_and_reproducible():
    rets = [1.0, -1.0, 2.0, -2.0] * 10
    mults = [1.4, 0.6] * 20
    a = study.permutation_pvalue_weighted(rets, mults, n_perm=500, seed=1410)
    b = study.permutation_pvalue_weighted(rets, mults, n_perm=500, seed=1410)
    assert a == b


def test_weighted_permutation_rewards_a_correct_pairing():
    rets = [5.0] * 40 + [-5.0] * 40
    mults = [1.5] * 40 + [0.5] * 40  # up-weights winners, down-weights losers
    p = study.permutation_pvalue_weighted(rets, mults, n_perm=500, seed=1410)
    assert p < 0.01


def test_weighted_permutation_punishes_an_inverted_pairing():
    rets = [5.0] * 40 + [-5.0] * 40
    mults = [0.5] * 40 + [1.5] * 40
    p = study.permutation_pvalue_weighted(rets, mults, n_perm=500, seed=1410)
    assert p > 0.9


def test_weighted_permutation_needs_multiplier_variation():
    assert study.permutation_pvalue_weighted([1.0, 2.0], [1.0, 1.0]) is None
    assert study.permutation_pvalue_weighted([], []) is None


def test_permutation_helpers_reject_length_mismatches():
    with pytest.raises(ValueError):
        study.permutation_pvalue_group_diff([1.0, 2.0], [True])
    with pytest.raises(ValueError):
        study.permutation_pvalue_weighted([1.0, 2.0], [1.0])


# ---------------------------------------------------------------------------
# Bucket tables
# ---------------------------------------------------------------------------

def test_bucket_tables_route_every_trade_including_nan():
    trades = [
        {"pnl_pct_net": 3.0, "h": {128: 0.60}},
        {"pnl_pct_net": -1.0, "h": {128: 0.60}},
        {"pnl_pct_net": 2.0, "h": {128: 0.40}},
        {"pnl_pct_net": -4.0, "h": {128: None}},
    ]
    table = study.bucket_tables(trades, 128)
    assert table[">=0.55"]["trades"] == 2
    assert table["<0.45"]["trades"] == 1
    assert table[study.BUCKET_NAN]["trades"] == 1
    assert table[study.BUCKET_NAN]["mean_pnl_pct_net"] == pytest.approx(-4.0)
    assert table["0.50-0.55"]["trades"] == 0
    assert sum(table[b]["trades"] for b in study.BUCKETS) == len(trades)
    assert table[">=0.55"]["win_rate_pct"] == pytest.approx(50.0)


# ---------------------------------------------------------------------------
# Acceptance rule + recommendation
# ---------------------------------------------------------------------------

def _window_row(dd=-3.0, chop=-2.0, ret_gated=10.0, ret_ungated=9.0, n_legs=6):
    return {"n_legs": n_legs, "dd_delta": dd, "chop_delta": chop,
            "ret_gated": ret_gated, "ret_ungated": ret_ungated,
            "trades_gated": 50, "trades_ungated": 90}


def _cfg(family=study.FAMILY_MOMENTUM, mode="gate", **over):
    cfg = {
        "config_id": over.pop("config_id", f"{family}/{mode}/W256/arm0.55/dis0.5"),
        "family": family,
        "mode": mode,
        "sense": study.FAMILY_SENSE[family],
        "hurst_window": 256,
        "arm": 0.55, "disarm": 0.50, "gain": None,
        "n_pooled_trades": 400,
        "n_suppressed": 120,
        "n_kept": 280,
        "p_raw": 0.0005,
        "bh_reject": True,
        "windows": {w: _window_row() for w in study.WINDOWS},
    }
    cfg.update(over)
    return cfg


def test_a_config_meeting_every_rule_passes():
    ok, reasons = study.config_verdict(_cfg())
    assert ok and reasons == []


def test_volume_floors_block_a_trade_nothing_config():
    ok, reasons = study.config_verdict(_cfg(n_suppressed=5, n_kept=2))
    assert not ok
    assert any("suppressed" in r for r in reasons)
    assert any("kept" in r for r in reasons)


def test_bh_insignificance_blocks_a_config():
    ok, reasons = study.config_verdict(_cfg(bh_reject=False))
    assert not ok and any("Benjamini-Hochberg" in r for r in reasons)


def test_a_protocol_window_that_worsens_drawdown_blocks_a_config():
    windows = {w: _window_row() for w in study.WINDOWS}
    windows["oos"] = _window_row(dd=+1.5)
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok and any("oos: drawdown not reduced" in r for r in reasons)


def test_a_protocol_window_that_worsens_chop_blocks_a_config():
    windows = {w: _window_row() for w in study.WINDOWS}
    windows["is"] = _window_row(chop=+0.5)
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok and any("is: chop loss not reduced" in r for r in reasons)


def test_return_give_up_is_tolerated_up_to_the_pre_registered_band():
    windows = {w: _window_row() for w in study.WINDOWS}
    windows["is"] = _window_row(ret_gated=99.1, ret_ungated=100.0)  # 0.9 pp of 100
    assert study.config_verdict(_cfg(windows=windows))[0]
    windows["is"] = _window_row(ret_gated=88.0, ret_ungated=100.0)  # 12 pp > 10 pp
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok and any("return give-up" in r for r in reasons)


def test_the_absolute_tolerance_floor_applies_to_a_tiny_baseline():
    windows = {w: _window_row() for w in study.WINDOWS}
    windows["oos"] = _window_row(ret_gated=-0.5, ret_ungated=0.2)  # give-up 0.7 pp
    assert study.config_verdict(_cfg(windows=windows))[0]
    windows["oos"] = _window_row(ret_gated=-2.0, ret_ungated=0.2)  # give-up 2.2 pp
    assert not study.config_verdict(_cfg(windows=windows))[0]


def test_held_out_degradation_blocks_a_config():
    windows = {w: _window_row() for w in study.WINDOWS}
    for w in study.HELD_OUT_WINDOWS[:2]:
        windows[w] = _window_row(dd=+2.0)
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok and any("held-out" in r for r in reasons)


def test_a_missing_protocol_window_blocks_a_config():
    windows = {w: _window_row() for w in study.WINDOWS}
    windows["is"] = {"n_legs": 0}
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok and any("is: no legs" in r for r in reasons)


def test_protocol_dd_reduction_is_positive_when_drawdown_falls():
    assert study.protocol_dd_reduction(_cfg()) == pytest.approx(6.0)


# --- decide_recommendation -------------------------------------------------

def test_no_winner_anywhere_yields_the_inconclusive_verdict():
    configs = [_cfg(family=f, bh_reject=False,
                    config_id=f"{f}/gate/W256/arm0.55/dis0.5")
               for f in study.FAMILIES]
    decision = study.decide_recommendation(configs)
    assert decision["verdict"] == "inconclusive"
    assert all(v["winner"] is None for v in decision["families"].values())
    assert decision["justification"]


def test_the_inconclusive_justification_counts_the_failure_modes():
    """The justification is derived, so it can never contradict the tables."""
    economics_ok = _cfg(family=study.FAMILY_MOMENTUM, bh_reject=False,
                        config_id="momentum/gate/W256/arm0.55/dis0.5")
    economics_bad = _cfg(family=study.FAMILY_MEAN_REVERSION, bh_reject=False,
                         config_id="mean_reversion/gate/W256/arm0.45/dis0.5",
                         windows={w: _window_row(dd=+3.0) for w in study.WINDOWS})
    decision = study.decide_recommendation([economics_ok, economics_bad])
    assert decision["verdict"] == "inconclusive"
    assert "0 reached Benjamini-Hochberg significance" in decision["justification"]
    assert "1 met every economic condition" in decision["justification"]


def test_a_winner_in_one_family_yields_a_config_verdict():
    configs = [
        _cfg(family=study.FAMILY_MOMENTUM,
             config_id="momentum/gate/W256/arm0.55/dis0.5"),
        _cfg(family=study.FAMILY_MEAN_REVERSION, bh_reject=False,
             config_id="mean_reversion/gate/W256/arm0.45/dis0.5"),
    ]
    decision = study.decide_recommendation(configs)
    assert decision["verdict"] == "config"
    assert decision["families"][study.FAMILY_MOMENTUM]["winner"] is not None
    assert decision["families"][study.FAMILY_MEAN_REVERSION]["winner"] is None


def test_the_tie_break_picks_the_largest_protocol_drawdown_reduction():
    small = _cfg(config_id="momentum/gate/W128/arm0.52/dis0.48",
                 windows={w: _window_row(dd=-1.0) for w in study.WINDOWS})
    big = _cfg(config_id="momentum/gate/W512/arm0.6/dis0.52",
               windows={w: _window_row(dd=-9.0) for w in study.WINDOWS})
    decision = study.decide_recommendation([small, big])
    winner = decision["families"][study.FAMILY_MOMENTUM]["winner"]
    assert winner["config_id"] == big["config_id"]


def test_the_tie_break_is_deterministic_for_equal_reductions():
    a = _cfg(config_id="momentum/gate/W128/arm0.52/dis0.48")
    b = _cfg(config_id="momentum/gate/W512/arm0.6/dis0.52")
    first = study.decide_recommendation([a, b])
    second = study.decide_recommendation([b, a])
    assert (first["families"][study.FAMILY_MOMENTUM]["winner"]["config_id"]
            == second["families"][study.FAMILY_MOMENTUM]["winner"]["config_id"])


# --- renderer --------------------------------------------------------------

def test_inconclusive_recommendation_prints_the_bare_token():
    configs = [_cfg(family=f, bh_reject=False, config_id=f"{f}/gate/W256/a/d")
               for f in study.FAMILIES]
    text = study.render_recommendation(
        study.decide_recommendation(configs), configs)
    assert text.startswith("## Recommendation")
    assert any(line.strip() == "INCONCLUSIVE" for line in text.splitlines())
    # An inconclusive verdict never smuggles a configuration in beside it.
    assert "Mode:" not in text


def test_a_winning_recommendation_states_the_full_configuration():
    configs = [
        _cfg(family=study.FAMILY_MOMENTUM,
             config_id="momentum/gate/W256/arm0.55/dis0.5"),
        _cfg(family=study.FAMILY_MEAN_REVERSION, bh_reject=False,
             config_id="mean_reversion/gate/W256/arm0.45/dis0.5"),
    ]
    text = study.render_recommendation(
        study.decide_recommendation(configs), configs)
    assert "INCONCLUSIVE" not in text
    assert "Mode: **gate**" in text
    assert "0.55 / 0.5" in text
    assert "256 bars" in text
    # The losing family gets an explicit no-gate sentence, never silence.
    assert "Do not gate or size the mean_reversion family" in text


def test_a_winning_sizing_recommendation_states_its_gain():
    cfg = _cfg(family=study.FAMILY_MOMENTUM, mode="size", arm=None, disarm=None,
               gain=2.5, config_id="momentum/size/W256/gain2.5")
    text = study.render_recommendation(
        study.decide_recommendation([cfg]), [cfg])
    assert "Mode: **size**" in text
    assert "Gain: **2.5**" in text


def test_recommendation_is_the_final_section_of_the_report():
    payload = _report_payload()
    report = study.render_report(payload)
    lines = report.splitlines()
    last_h2 = max(i for i, line in enumerate(lines) if line.startswith("## "))
    assert lines[last_h2] == "## Recommendation"
    assert any(line.strip() == "INCONCLUSIVE" for line in lines[last_h2:])


def _report_payload(verdict_configs=None, warmup=_UNSET):
    configs = verdict_configs or [
        _cfg(family=f, bh_reject=False, config_id=f"{f}/gate/W256/arm0.55/dis0.5")
        for f in study.FAMILIES
    ]
    for cfg in configs:
        cfg.setdefault("bh_reject", False)
    decision = study.decide_recommendation(configs)
    pooled = {f: [] for f in study.FAMILIES}
    return {
        "schema_version": study.SCHEMA_VERSION,
        "pre_registered": {
            "families": {f: list(study.FAMILY_EXEMPLARS[f]) for f in study.FAMILIES},
            "family_sense": dict(study.FAMILY_SENSE),
            "exemplar_close_overrides": study.EXEMPLAR_CLOSE_OVERRIDES,
            "buckets": list(study.BUCKETS),
            "hurst_windows": [256],
            "gate_pairs": {f: [list(p) for p in study.GATE_PAIRS[f]]
                           for f in study.FAMILIES},
            "gate_initial_armed": study.GATE_INITIAL_ARMED,
            "sizing": {"gains": list(study.SIZING_GAINS),
                       "clamp_lo": study.SIZING_CLAMP_LO,
                       "clamp_hi": study.SIZING_CLAMP_HI,
                       "nan_multiplier": study.SIZING_NAN_MULTIPLIER},
            "min_suppressed": study.MIN_SUPPRESSED_TRADES,
            "min_kept": study.MIN_KEPT_TRADES,
            "return_tolerance_pp": study.RETURN_TOLERANCE_PP,
            "return_tolerance_frac": study.RETURN_TOLERANCE_FRAC,
            "held_out_min_non_degrading": study.HELD_OUT_MIN_NON_DEGRADING,
            "alpha": study.ALPHA,
            "n_perm": study.N_PERM,
            "seed": study.SEED,
            "windows": {k: list(v) for k, v in study.WINDOWS.items()},
            "protocol_windows": list(study.PROTOCOL_WINDOWS),
            "held_out_windows": list(study.HELD_OUT_WINDOWS),
            "datasets": ["BTC/USDT 1h"],
            "platform": study.PLATFORM,
            "fee_platform": study.FEE_PLATFORM,
            "capital": study.DEFAULT_CAPITAL,
        },
        "run_summary": {
            "legs": 1, "gated_arms": 3, "mirror_verified_legs": 1,
            "raw_trades": {f: 0 for f in study.FAMILIES},
            "pooled_trades": {f: 0 for f in study.FAMILIES},
            "warmup": (study.warmup_audit({"BTC/USDT 1h": 5000}, [256])
                       if warmup is _UNSET else warmup),
            "elapsed_sec": 1.0,
        },
        "buckets": {f: {"256": study.bucket_tables(pooled[f], 256)}
                    for f in study.FAMILIES},
        "configs": configs,
        "legs": [],
        "decision": decision,
    }


def test_report_renders_and_carries_the_pre_registered_constants():
    report = study.render_report(_report_payload())
    assert "# Hurst gate calibration study (#1410)" in report
    assert "## Pre-registered design" in report
    assert "shared_strategies/open/indicators_core.py" in report
    assert "benjamini_hochberg" in report
    assert study.PLATFORM in report and study.FEE_PLATFORM in report
    assert "NaN is its OWN bucket" in report
    for family in study.FAMILIES:
        for exemplar in study.FAMILY_EXEMPLARS[family]:
            assert exemplar in report


def test_report_names_the_data_and_fee_axes_separately():
    report = study.render_report(_report_payload())
    assert "Data source exchange: `binanceus`" in report
    assert "Fee model: `hyperliquid`" in report
    assert study.PLATFORM != study.FEE_PLATFORM


def test_report_recommendation_section_survives_a_winner():
    winner = _cfg(family=study.FAMILY_MOMENTUM,
                  config_id="momentum/gate/W256/arm0.55/dis0.5")
    loser = _cfg(family=study.FAMILY_MEAN_REVERSION, bh_reject=False,
                 config_id="mean_reversion/gate/W256/arm0.45/dis0.5")
    report = study.render_report(_report_payload([winner, loser]))
    lines = report.splitlines()
    last_h2 = max(i for i, line in enumerate(lines) if line.startswith("## "))
    assert lines[last_h2] == "## Recommendation"
    assert "INCONCLUSIVE" not in report


def test_report_is_a_pure_function_of_the_committed_payload():
    """`--render-only` must reproduce the same report from the same JSON."""
    payload = _report_payload()
    stored = dict(payload)
    # The committed JSON stores the reduced decision shape, not winner objects.
    stored["decision"] = {
        "verdict": payload["decision"]["verdict"],
        "justification": payload["decision"]["justification"],
        "families": {f: {"n_tested": d["n_tested"], "n_passing": d["n_passing"],
                         "winner": (d["winner"] or {}).get("config_id")}
                     for f, d in payload["decision"]["families"].items()},
    }
    assert study.report_from_payload(stored) == study.render_report(payload)


def test_render_only_rebuilds_a_winning_recommendation_too():
    winner = _cfg(family=study.FAMILY_MOMENTUM,
                  config_id="momentum/gate/W256/arm0.55/dis0.5")
    loser = _cfg(family=study.FAMILY_MEAN_REVERSION, bh_reject=False,
                 config_id="mean_reversion/gate/W256/arm0.45/dis0.5")
    payload = _report_payload([winner, loser])
    payload["decision"] = {"verdict": "config", "justification": "",
                           "families": {}}
    report = study.report_from_payload(payload)
    assert "Mode: **gate**" in report
    assert "INCONCLUSIVE" not in report


def test_config_ids_never_contain_a_markdown_table_separator():
    """Config ids are printed inside table cells; a `|` would split the row."""
    for family in study.FAMILIES:
        for hw in study.HURST_WINDOWS:
            for arm, disarm in study.GATE_PAIRS[family]:
                assert "|" not in study.gate_config_id(family, hw, arm, disarm)
            for gain in study.SIZING_GAINS:
                assert "|" not in study.size_config_id(family, hw, gain)
    assert "|" not in study.CONFIG_ID_SEP


def test_every_rendered_table_row_has_a_consistent_cell_count():
    """A stray `|` inside a cell would silently corrupt the rendered tables."""
    report = study.render_report(_report_payload())
    lines = report.splitlines()
    block = []
    blocks = []
    for line in lines + [""]:
        if line.startswith("|"):
            block.append(line)
        elif block:
            blocks.append(block)
            block = []
    assert blocks, "the report must contain at least one table"
    for tbl in blocks:
        widths = {line.count("|") for line in tbl}
        assert len(widths) == 1, f"ragged table rows: {tbl[:3]}"


def test_report_only_claims_an_empty_nan_bucket_from_a_measured_warmup():
    """The emptiness claim must rest on the RECORDED audit, never on faith."""
    report = study.render_report(_report_payload())
    assert "NaN` bucket is EMPTY here" in report
    # The claim is qualified by the numbers the run measured.
    assert "5000" in report and "258" in report
    assert "never 0.5" in report


def test_report_says_the_nan_bucket_is_populated_when_warmup_is_short():
    short = study.warmup_audit({"BTC/USDT 1h": 10}, [256])
    report = study.render_report(_report_payload(warmup=short))
    assert "NaN` bucket is POPULATED here" in report
    assert "`BTC/USDT 1h`" in report
    assert "NaN` bucket is EMPTY here" not in report
    assert "never 0.5" in report


def test_report_refuses_to_attest_the_nan_bucket_without_an_audit():
    """An older JSON re-rendered with --render-only must not inherit the claim."""
    report = study.render_report(_report_payload(warmup=None))
    assert "NOT attested here" in report
    assert "NaN` bucket is EMPTY here" not in report
    assert "never 0.5" in report


# ---------------------------------------------------------------------------
# Warm-up audit (the NaN-bucket claim's evidence)
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("hw,expected", [(128, 130), (256, 258), (512, 514)])
def test_required_lead_is_the_hurst_window_plus_the_stamp_margin(hw, expected):
    """Entry stamp at fill bar p reads rolling H at p-2, which needs p >= W+1;
    the audit keeps one further bar of margin."""
    assert study.required_lead_bars(hw) == expected
    assert study.required_lead_bars(hw) >= hw + 1


def test_warmup_lead_counts_only_bars_strictly_before_the_window_start():
    idx = pd.date_range("2022-12-30", periods=10, freq="1D")
    # Window starts on the 3rd bar: two bars of lead, and the start bar itself
    # is a SCORED bar, never lead.
    assert study.warmup_lead_bars(idx, "2023-01-01") == 2
    assert study.warmup_lead_bars(idx, "2022-12-30") == 0
    assert study.warmup_lead_bars(idx, "2030-01-01") == len(idx)


def test_warmup_audit_flags_a_cache_that_starts_at_the_window_start():
    """Reviewer case (1): zero lead must be recorded as insufficient."""
    audit = study.warmup_audit({"BTC/USDT 1h": 0, "ETH/USDT 4h": 9000}, [128, 512])
    assert audit["sufficient"] is False
    assert audit["insufficient_datasets"] == ["BTC/USDT 1h"]
    assert audit["required_bars"] == study.required_lead_bars(512)
    assert audit["min_lead_bars"] == 0


def test_warmup_audit_passes_when_every_dataset_clears_the_largest_window():
    """Reviewer case (2): ample lead must not raise a false alarm."""
    leads = {"BTC/USDT 1h": 26000, "SOL/USDT 4h": 5000}
    audit = study.warmup_audit(leads, [128, 256, 512])
    assert audit["sufficient"] is True
    assert audit["insufficient_datasets"] == []
    assert audit["lead_bars"] == dict(sorted(leads.items()))


def test_warmup_audit_boundary_is_inclusive_at_exactly_the_requirement():
    need = study.required_lead_bars(256)
    assert study.warmup_audit({"d": need}, [256])["sufficient"] is True
    assert study.warmup_audit({"d": need - 1}, [256])["sufficient"] is False


def test_warmup_audit_requirement_follows_the_selected_hurst_windows():
    """Reviewer case (3): a run over only W=128 must not be judged against 512."""
    leads = {"d": 200}
    assert study.warmup_audit(leads, [128])["sufficient"] is True
    assert study.warmup_audit(leads, [128, 512])["sufficient"] is False


# ---------------------------------------------------------------------------
# Rolling-Hurst cache identity
# ---------------------------------------------------------------------------

def _idx(n=3000, start="2023-01-01"):
    return pd.date_range(start, periods=n, freq="1h")


def test_cache_hit_requires_coverage_at_least_as_deep_as_this_run_needs():
    """A cache written for a LATE window must not serve an EARLIER one.

    The stale array is same-length but nearly all NaN over the earlier span,
    and because a NaN reading HOLDS the gate state every gated arm would
    silently reproduce its ungated arm — a fake "no effect" result.
    """
    idx = _idx()
    late = study.cache_meta(idx, "2026-01-01")
    assert study.cache_entry_is_usable(late, idx, "2023-01-01") is False


def test_cache_hit_allowed_when_the_cached_array_covers_more_than_needed():
    idx = _idx()
    early = study.cache_meta(idx, "2023-01-01")
    assert study.cache_entry_is_usable(early, idx, "2026-01-01") is True


def test_cache_hit_on_an_identical_repeat_run():
    idx = _idx()
    meta = study.cache_meta(idx, "2023-01-01")
    assert study.cache_entry_is_usable(meta, idx, "2023-01-01") is True


def test_cache_miss_when_the_dataset_gains_bars():
    meta = study.cache_meta(_idx(3000), "2023-01-01")
    assert study.cache_entry_is_usable(meta, _idx(3001), "2023-01-01") is False


def test_cache_miss_when_the_series_shifts_without_changing_length():
    """Equal length is not identity — the bars themselves must match."""
    meta = study.cache_meta(_idx(3000, "2023-01-01"), "2023-01-01")
    assert study.cache_entry_is_usable(
        meta, _idx(3000, "2023-06-01"), "2023-01-01") is False


def test_cache_miss_on_a_legacy_entry_with_no_metadata():
    idx = _idx()
    assert study.cache_entry_is_usable(None, idx, "2023-01-01") is False
    assert study.cache_entry_is_usable(
        np.array([1, 2], dtype=np.int64), idx, "2023-01-01") is False


def test_cache_meta_round_trips_through_an_int64_npz_array():
    """np.savez_compressed must be able to store it without pickling."""
    idx = _idx()
    meta = study.cache_meta(idx, "2023-01-01")
    assert meta.dtype == np.int64 and meta.shape == (study.CACHE_META_FIELDS,)
    assert study.cache_entry_is_usable(np.asarray(meta.tolist(), dtype=np.int64),
                                       idx, "2023-01-01") is True


def test_report_states_that_trading_less_is_not_trading_better():
    report = study.render_report(_report_payload())
    assert "trading less, not trading better" in report


# ---------------------------------------------------------------------------
# Benjamini-Hochberg wiring (the correction itself belongs to regime_stats)
# ---------------------------------------------------------------------------

def test_bh_denominator_covers_every_tested_hypothesis():
    """Untestable configs (p=None) must still inflate the BH denominator.

    Dropping them from the family would silently make the correction more
    permissive, which is the opposite of correcting for multiplicity.
    """
    configs = [_cfg(config_id="c0", p_raw=0.001)]
    configs += [_cfg(config_id=f"n{i}", p_raw=None) for i in range(99)]
    for cfg in configs:
        cfg.pop("bh_reject", None)
    study.apply_bh(configs)
    # One p of 0.001 against a family of 100: rank-1 threshold is
    # 0.05/100 = 0.0005, so it must NOT be rejected.
    assert not any(c["bh_reject"] for c in configs)

    # The same p-value alone in a family of one clears rank-1 (0.05),
    # proving the refusal above came from the inflated denominator.
    solo = [_cfg(config_id="c0", p_raw=0.001)]
    solo[0].pop("bh_reject", None)
    study.apply_bh(solo)
    assert solo[0]["bh_reject"]


def test_bh_marks_a_clearly_significant_config():
    configs = [_cfg(config_id="c0", p_raw=1e-6)]
    configs += [_cfg(config_id=f"n{i}", p_raw=0.9) for i in range(9)]
    for cfg in configs:
        cfg.pop("bh_reject", None)
    study.apply_bh(configs)
    assert configs[0]["bh_reject"]
    assert not any(c["bh_reject"] for c in configs[1:])


def test_bh_on_an_all_untestable_sweep_rejects_nothing():
    configs = [_cfg(config_id=f"n{i}", p_raw=None) for i in range(4)]
    for cfg in configs:
        cfg.pop("bh_reject", None)
    study.apply_bh(configs)
    assert not any(c["bh_reject"] for c in configs)


# ---------------------------------------------------------------------------
# Pre-registration integrity
# ---------------------------------------------------------------------------

def test_the_five_named_exemplars_are_the_declared_families():
    assert study.FAMILY_EXEMPLARS[study.FAMILY_MOMENTUM] == (
        "momentum", "vol_momentum", "squeeze_momentum")
    assert study.FAMILY_EXEMPLARS[study.FAMILY_MEAN_REVERSION] == (
        "mean_reversion", "atr_band_revert")


def test_hurst_windows_clear_the_estimator_minimum():
    assert min(study.HURST_WINDOWS) > 100  # HURST_DFA_MIN_POINTS


def test_the_two_platform_axes_stay_uncoupled():
    from eval_windows import FEE_PLATFORM, PLATFORM
    assert study.PLATFORM == PLATFORM == "binanceus"
    assert study.FEE_PLATFORM == FEE_PLATFORM == "hyperliquid"


def test_windows_are_reused_verbatim_from_eval_windows():
    from eval_windows import HELD_OUT_WINDOWS, PROTOCOL_WINDOWS, WINDOWS
    assert study.WINDOWS is WINDOWS
    assert study.PROTOCOL_WINDOWS is PROTOCOL_WINDOWS
    assert study.HELD_OUT_WINDOWS is HELD_OUT_WINDOWS


def test_the_estimator_is_the_1409_ssot_not_a_local_copy():
    src = study.hurst_exponent.__module__
    assert "indicators_core" in src
    assert math.isnan(study.hurst_exponent(pd.Series([1.0, 2.0, 3.0])))
