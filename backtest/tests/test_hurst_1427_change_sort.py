import json
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import hurst_1427_change_sort as study
import hurst_1426_two_sided_sort as study1426
import hurst_1424_gate_resolution as study1424
import hurst_1422_gate_power as study1422
import hurst_1410_gate_calibration as study1410

_MR = "mean_reversion"
CONTRACT = os.path.join(os.path.dirname(study._DEFAULT_REPORT_OUT),
                        "hurst_gate_calibration.md")


def _trade(symbol="BTC/USDT", timeframe="1h", window="2021", day=0, pnl=1.0,
           eff=0.1, hold_days=1, dh=None, adx=None, cohort=None,
           exchange="binanceus", armed=None, level_h=None):
    entry = pd.Timestamp("2021-01-01") + pd.Timedelta(days=day)
    row = {
        "strategy": "momentum",
        "exchange": exchange,
        "symbol": symbol,
        "base_symbol": symbol.split("@", 1)[0],
        "timeframe": timeframe,
        "window": window,
        "cohort": cohort or study.cell_cohort(exchange, symbol.split("@", 1)[0],
                                              timeframe, window),
        "entry_date": str(entry),
        "entry_ns": int(entry.value),
        "exit_ns": int((entry + pd.Timedelta(days=hold_days)).value),
        "pnl_pct_net": float(pnl),
        "efficiency": None if eff is None else float(eff),
        "adx": adx,
        "dh": {512: dh, 128: dh},
        "armed": dict(armed or {}),
    }
    if level_h is not None:
        row["h"] = {512: level_h, 128: level_h}
    return row


def _rotatable_pool(n=60):
    return ([_trade(day=i * 5) for i in range(n)]
            + [_trade(symbol="ETH/USDT", day=i * 5) for i in range(n)])


def _random_mask(n, seed=7):
    return list(np.random.default_rng(seed).random(n) < 0.5)


# --- the re-centring rule is derived from the committed level study --------


def test_the_delta_edges_are_1410s_level_edges_minus_the_level_origin():
    assert study.LEVEL_ORIGIN == 0.5
    assert study.DELTA_ORIGIN == 0.0
    assert study.LEVEL_EDGES == (0.45, 0.50, 0.55)
    assert study.DELTA_EDGES == tuple(round(e - 0.5, 6)
                                      for e in study.LEVEL_EDGES)
    assert study.DELTA_EDGES == (-0.05, 0.0, 0.05)


def test_the_level_edges_this_study_re_centres_are_still_1410s_own_cuts():
    probes = [0.40] + list(study.LEVEL_EDGES)
    assert tuple(study1410.bucket_label(p) for p in probes) == \
        tuple(study.BUCKETS[:-1])


def test_a_drifted_level_bucketing_fails_the_import_time_assertion(monkeypatch):
    monkeypatch.setattr(study, "bucket_label", lambda h: "moved")
    with pytest.raises(AssertionError) as exc:
        study._assert_level_landmarks_are_1410s()
    assert "no longer cuts" in str(exc.value)


def test_the_delta_buckets_mirror_the_level_buckets_one_for_one():
    assert len(study.DELTA_BUCKETS) == len(study.BUCKETS)
    assert study.DELTA_BUCKETS[-1] == study.BUCKET_NAN == study.BUCKETS[-1]
    assert study.DELTA_BUCKETS == ("<-0.05", "-0.05..+0.00", "+0.00..+0.05",
                                   ">=+0.05", "NaN")


def test_every_delta_bucket_is_its_level_bucket_re_centred():
    for level in (0.30, 0.449, 0.45, 0.499, 0.50, 0.549, 0.55, 0.90):
        delta = round(level - study.LEVEL_ORIGIN, 6)
        expected = study.DELTA_BUCKETS[
            study.BUCKETS.index(study1410.bucket_label(level))]
        assert study.delta_bucket_label(delta) == expected


def test_the_delta_bucket_edges_are_half_open_exactly_like_the_level_ones():
    assert study.delta_bucket_label(-0.0500001) == "<-0.05"
    assert study.delta_bucket_label(-0.05) == "-0.05..+0.00"
    assert study.delta_bucket_label(-1e-9) == "-0.05..+0.00"
    assert study.delta_bucket_label(0.0) == "+0.00..+0.05"
    assert study.delta_bucket_label(0.0499999) == "+0.00..+0.05"
    assert study.delta_bucket_label(0.05) == ">=+0.05"


def test_the_gate_pairs_are_1410s_pairs_re_centred_and_still_valid():
    for family in study.FAMILIES:
        assert study.DELTA_GATE_PAIRS[family] == tuple(
            (round(a - 0.5, 6), round(d - 0.5, 6))
            for a, d in study1410.GATE_PAIRS[family])
        for arm, disarm in study.DELTA_GATE_PAIRS[family]:
            study.validate_gate_pair(arm, disarm, study.FAMILY_SENSE[family])
    assert study.DELTA_GATE_PAIRS["momentum"] == ((0.05, 0.0), (0.1, 0.02),
                                                  (0.02, -0.02))


def test_the_pin_is_1424s_pin_re_centred_and_nothing_else():
    assert study.LEVEL_PRIMARY_CONFIG_ID == study1424.PRIMARY_CONFIG_ID
    assert study.PRIMARY_CONFIG_ID == \
        study._recentre_gate_config_id(study1424.PRIMARY_CONFIG_ID)
    assert study.PRIMARY_CONFIG_ID == "momentum/gate/W512/arm0.02/dis-0.02"
    assert study.PRIMARY_CONFIG_IDS == (study.PRIMARY_CONFIG_ID,)
    assert study.PRIMARY_FAMILY_SIZE == 1


def test_the_pin_is_one_of_the_swept_pairs():
    study._assert_pin_is_recentred_1424()
    assert (study.PRIMARY_ARM, study.PRIMARY_DISARM) in \
        study.DELTA_GATE_PAIRS[study.PRIMARY_FAMILY]


def test_a_pin_outside_the_swept_pairs_fails_loud(monkeypatch):
    monkeypatch.setattr(study, "PRIMARY_ARM", 0.33)
    with pytest.raises(AssertionError) as exc:
        study._assert_pin_is_recentred_1424()
    assert "not one of this study's swept pairs" in str(exc.value)


def test_the_committed_1410_argmin_still_backs_1424s_pin():
    assert study.resolve_primary_config_id(study._JSON_1410) == \
        study.LEVEL_PRIMARY_CONFIG_ID


def test_the_size_multiplier_is_1410s_own_curve_re_centred():
    for dh in (-0.4, -0.05, -0.001, 0.0, 0.001, 0.05, 0.4):
        for sense in (study.SENSE_HIGH, study.SENSE_LOW):
            for gain in study.SIZING_GAINS:
                assert study.delta_size_multiplier(dh, sense, gain) == \
                    study1410.size_multiplier(dh + 0.5, sense, gain)
    assert study.delta_size_multiplier(0.0, study.SENSE_HIGH, 5.0) == 1.0


def test_the_anti_signal_side_is_1422s_own_split_re_centred():
    assert study.delta_anti_signal_side(0.01, study.SENSE_HIGH) is False
    assert study.delta_anti_signal_side(-0.01, study.SENSE_HIGH) is True
    assert study.delta_anti_signal_side(0.0, study.SENSE_HIGH) is False
    assert study.delta_anti_signal_side(0.01, study.SENSE_LOW) is True
    for dh in (-0.4, -0.05, -0.001, 0.0, 0.001, 0.05, 0.4):
        for sense in (study.SENSE_HIGH, study.SENSE_LOW):
            assert study.delta_anti_signal_side(dh, sense) == \
                study1422.anti_signal_side(dh + 0.5, sense)


# --- the lookback is one rule, fixed in advance, never swept ---------------


def test_the_lookback_is_half_the_hurst_window():
    assert study.DELTA_LOOKBACK_DENOMINATOR == 2
    for hw in study.HURST_WINDOWS:
        assert study.delta_lookback_bars(hw) == hw // 2
    assert study.PRIMARY_LOOKBACK_BARS == study.PRIMARY_HURST_WINDOW // 2


def test_the_lookback_never_falls_below_one_bar():
    assert study.delta_lookback_bars(1) == 1
    assert study.delta_lookback_bars(0) == 1


def test_the_lookback_adds_no_swept_dimension():
    grid = study._sweep_grid(study.COHORT_EXPLORATORY, study.HURST_WINDOWS)
    assert len(grid) == 30
    lookbacks_per_window = {}
    for _family, _mode, hw, _arm, _dis, _gain in grid:
        lookbacks_per_window.setdefault(hw, set()).add(
            study.delta_lookback_bars(hw))
    assert all(len(v) == 1 for v in lookbacks_per_window.values())
    assert study._sweep_grid(study.COHORT_PRIMARY, study.HURST_WINDOWS) == [
        (study.PRIMARY_FAMILY, "gate", study.PRIMARY_HURST_WINDOW,
         study.PRIMARY_ARM, study.PRIMARY_DISARM, None)]


def test_the_change_series_refuses_a_lookback_below_one_bar():
    series = pd.Series(np.arange(10.0))
    with pytest.raises(ValueError):
        study.delta_hurst_series(series, 0)
    with pytest.raises(ValueError):
        study.rolling_hurst_for_delta(series, 4, 0,
                                      first_needed=pd.Timestamp("2021-01-01"))


# --- an undefined change is never zero and never moves a bucket -----------


def test_an_undefined_endpoint_makes_the_change_undefined_and_never_zero():
    idx = pd.date_range("2021-01-01", periods=12, freq="1h")
    rolling = pd.Series([np.nan, np.nan, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5,
                         0.5, 0.5, 0.5], index=idx)
    delta = study.delta_hurst_series(rolling, 3)
    assert [bool(pd.isna(v)) for v in delta] == \
        [True] * 5 + [False] * 7
    assert all(not math.isclose(float(v), 0.0)
               for v in delta[:5].fillna(-999.0))


def test_an_undefined_change_is_its_own_bucket_and_never_the_zero_bucket():
    assert study.delta_bucket_label(None) == study.BUCKET_NAN
    assert study.delta_bucket_label(float("nan")) == study.BUCKET_NAN
    assert study.delta_bucket_label(0.0) != study.BUCKET_NAN
    assert study.delta_bucket_label(float("nan")) != \
        study.delta_bucket_label(0.0)
    assert study.joint_delta_bucket(None) == study.BUCKET_NAN
    assert study.joint_delta_bucket(float("nan")) != \
        study.joint_delta_bucket(0.0)


def test_an_undefined_change_never_transitions_the_gate():
    arm, disarm = study.DELTA_GATE_PAIRS["momentum"][-1]
    values = [arm, np.nan, np.nan, disarm - 0.01, np.nan, np.nan, arm]
    mask = study.hysteresis_mask(values, arm, disarm, study.SENSE_HIGH)
    assert list(mask) == [True, True, True, False, False, False, True]


def test_an_undefined_change_has_no_gate_side():
    for sense in (study.SENSE_HIGH, study.SENSE_LOW):
        assert study.delta_anti_signal_side(None, sense) is None
        assert study.delta_anti_signal_side(float("nan"), sense) is None
        assert study.delta_anti_signal_side(float("inf"), sense) is None
        assert study.delta_anti_signal_side(0.0, sense) is not None


def test_an_undefined_gate_side_is_never_the_aligned_side():
    for sense in (study.SENSE_HIGH, study.SENSE_LOW):
        assert study.delta_anti_signal_side(None, sense) is not False
        assert study.delta_anti_signal_side(float("nan"), sense) is not False


def test_the_gate_side_still_rejects_an_unknown_sense_on_an_undefined_change():
    for dh in (None, float("nan"), 0.01):
        with pytest.raises(ValueError):
            study.delta_anti_signal_side(dh, "sideways")


def test_an_undefined_change_sizes_at_exactly_one():
    for sense in (study.SENSE_HIGH, study.SENSE_LOW):
        for gain in study.SIZING_GAINS:
            assert study.delta_size_multiplier(None, sense, gain) == \
                study.SIZING_NAN_MULTIPLIER == 1.0
            assert study.delta_size_multiplier(float("nan"), sense,
                                               gain) == 1.0


def test_an_undefined_change_keeps_its_own_bucket_row_in_the_tables():
    rows = [_trade(day=0, dh=None), _trade(day=5, dh=0.0)]
    table = study.bucket_tables(rows, 512)
    assert table[study.BUCKET_NAN]["trades"] == 1
    assert table["+0.00..+0.05"]["trades"] == 1


# --- look-ahead, on the derived series, end to end ------------------------


def test_the_shifts_are_the_inherited_ones():
    series = pd.Series(np.arange(10.0))
    assert study.decision_series(series).equals(series.shift(1))
    assert study.entry_stamp_series(series).equals(series.shift(2))
    assert study.decision_series is study1410.decision_series
    assert study.entry_stamp_series is study1410.entry_stamp_series


_W = 128
_L = 64


def _walk(n, seed=1427):
    idx = pd.date_range("2021-01-01", periods=n, freq="1h")
    rng = np.random.default_rng(seed)
    return pd.Series(100.0 + np.cumsum(rng.normal(0, 0.5, n)), index=idx)


def test_the_change_at_a_bar_reads_no_bar_after_it():
    close = _walk(600)
    idx = close.index
    tamper_at = 450
    base = study.delta_hurst_series(
        study.rolling_hurst_for_delta(close, _W, _L, first_needed=idx[400]),
        _L)
    assert base.iloc[:tamper_at].notna().any()
    tampered = close.copy()
    tampered.iloc[tamper_at:] = tampered.iloc[tamper_at:] * 3.0
    after = study.delta_hurst_series(
        study.rolling_hurst_for_delta(tampered, _W, _L,
                                      first_needed=idx[400]), _L)
    assert np.allclose(base.iloc[:tamper_at].to_numpy(dtype=float),
                       after.iloc[:tamper_at].to_numpy(dtype=float),
                       equal_nan=True)
    assert not np.allclose(base.iloc[tamper_at:].to_numpy(dtype=float),
                           after.iloc[tamper_at:].to_numpy(dtype=float),
                           equal_nan=True)


def test_the_change_is_the_difference_of_the_same_rolling_hurst():
    close = _walk(500, seed=11)
    rolling = study.rolling_hurst_for_delta(close, _W, _L,
                                            first_needed=close.index[400])
    delta = study.delta_hurst_series(rolling, _L)
    assert rolling.notna().any()
    assert delta.notna().any()
    for i in range(len(close)):
        expected = (rolling.iloc[i] - rolling.iloc[i - _L]) if i >= _L \
            else np.nan
        got = delta.iloc[i]
        assert (bool(pd.isna(got)) and bool(pd.isna(expected))) or \
            got == pytest.approx(expected)


def _leg_frame(n=400):
    idx = pd.date_range("2021-02-01", periods=n, freq="1h")
    rng = np.random.default_rng(5)
    close = 100.0 + np.cumsum(rng.normal(0, 0.5, n))
    return pd.DataFrame({"open": close, "high": close + 1.0,
                         "low": close - 1.0, "close": close,
                         "volume": np.ones(n)}, index=idx)


def test_build_leg_stamps_the_change_two_bars_before_the_fill(monkeypatch):
    frame = _leg_frame()
    entry_pos = 200
    entry = frame.index[entry_pos]
    exit_ts = frame.index[entry_pos + 5]

    def _fake_arm(_reg, _name, _symbol, _tf, df, armed, _over):
        return {
            "return_pct": 1.0, "max_dd_pct": -1.0, "trades": 1,
            "trade_samples": [{"entry_date": str(entry),
                               "exit_date": str(exit_ts),
                               "pnl_pct_net": 1.5, "side": "long"}],
        }

    monkeypatch.setattr(study, "build_leg_run_arm", _fake_arm)
    delta = pd.Series(np.arange(len(frame), dtype=float) / 1000.0,
                      index=frame.index)
    leg = study.build_leg(None, "momentum", "momentum",
                          ("binanceus", "BTC/USDT", "1h"), "2021", frame,
                          {512: delta}, pd.Series(np.nan, index=frame.index),
                          verify_mirror=False)
    row = leg["trades"][0]
    assert "h" not in row
    assert row["dh"][512] == pytest.approx(float(delta.iloc[entry_pos - 2]))


def test_build_leg_arms_the_fill_bar_from_the_bar_before_the_signal(monkeypatch):
    frame = _leg_frame()
    entry_pos = 250
    entry = frame.index[entry_pos]

    def _fake_arm(_reg, _name, _symbol, _tf, df, armed, _over):
        return {
            "return_pct": 0.0, "max_dd_pct": 0.0, "trades": 1,
            "trade_samples": [{"entry_date": str(entry),
                               "exit_date": str(frame.index[entry_pos + 3]),
                               "pnl_pct_net": 1.0, "side": "long"}],
        }

    monkeypatch.setattr(study, "build_leg_run_arm", _fake_arm)
    arm, disarm = study.DELTA_GATE_PAIRS["momentum"][-1]
    values = np.full(len(frame), disarm - 0.01)
    values[:entry_pos - 3] = arm + 0.01
    delta = pd.Series(values, index=frame.index)
    leg = study.build_leg(None, "momentum", "momentum",
                          ("binanceus", "BTC/USDT", "1h"), "2021", frame,
                          {512: delta}, pd.Series(np.nan, index=frame.index),
                          verify_mirror=False)
    cid = study.gate_config_id("momentum", 512, arm, disarm)
    decision = study.decision_series(delta).to_numpy(dtype=float)
    expected = study.hysteresis_mask(decision, arm, disarm, study.SENSE_HIGH)
    assert leg["trades"][0]["armed"][cid] == bool(expected[entry_pos - 1])


def test_the_rolling_hurst_is_led_far_enough_for_both_shifts_and_the_lookback():
    close = _walk(600, seed=3)
    first_needed = close.index[400]
    stamp = study.entry_stamp_series(study.delta_hurst_series(
        study.rolling_hurst_for_delta(close, _W, _L,
                                      first_needed=first_needed), _L))
    assert not bool(pd.isna(stamp.loc[first_needed]))


def test_the_level_studys_own_lead_would_leave_the_stamp_undefined():
    close = _walk(600, seed=3)
    first_needed = close.index[400]
    level_lead = study1410.rolling_hurst(close, _W, first_needed=first_needed)
    stamp = study.entry_stamp_series(study.delta_hurst_series(level_lead, _L))
    assert bool(pd.isna(stamp.loc[first_needed]))


def test_the_lead_stamp_walks_back_exactly_the_lookback():
    idx = pd.date_range("2021-01-01", periods=100, freq="1h")
    assert study.delta_first_needed(idx, idx[50], 10) == idx[40]
    assert study.delta_first_needed(idx, idx[3], 10) == idx[0]
    assert study.delta_first_needed(pd.DatetimeIndex([]), idx[3], 10) == idx[3]


# --- warm-up accounts for the lookback on top of the Hurst window ---------


def test_the_required_lead_is_the_hurst_lead_plus_the_lookback():
    for hw in study.HURST_WINDOWS:
        assert study.delta_required_lead_bars(hw) == \
            study1410.required_lead_bars(hw) + study.delta_lookback_bars(hw)
    assert study.delta_required_lead_bars(512) == 512 + 2 + 256


def test_the_warmup_audit_reports_both_halves_of_the_requirement():
    audit = study.delta_warmup_audit({"BTC/USDT 1h": 900}, [512])
    assert audit["required_bars"] == 770
    assert audit["hurst_only_required_bars"] == 514
    assert audit["components"]["512"] == {"hurst_window": 512,
                                          "lookback_bars": 256,
                                          "margin_bars": 2,
                                          "required_bars": 770}
    assert audit["sufficient"] is True


def test_a_lead_that_suits_a_level_study_can_still_be_short_for_a_change_study():
    leads = {"BTC/USDT 1h": 600}
    audit = study.delta_warmup_audit(leads, [512])
    assert audit["sufficient"] is False
    assert audit["insufficient_datasets"] == ["BTC/USDT 1h"]
    assert audit["hurst_only_sufficient"] is True
    assert audit["hurst_only_insufficient_datasets"] == []


def _coverage_frames(bars_before, window="2021"):
    start = pd.Timestamp(study.WINDOWS[window][0])
    idx = pd.date_range(start - pd.Timedelta(hours=bars_before), periods=
                        bars_before + 24 * 365, freq="1h")
    close = np.linspace(100.0, 200.0, len(idx))
    return {("binanceus", "BTC/USDT", "1h"): pd.DataFrame(
        {"open": close, "high": close, "low": close, "close": close,
         "volume": np.ones(len(idx))}, index=idx)}


def test_coverage_drops_a_cell_that_only_meets_the_level_studys_lead():
    frames = _coverage_frames(600)
    cov = study.coverage_audit(frames, ["2021"], [512])
    assert cov["required_lead_bars"] == 770
    assert cov["hurst_only_required_lead_bars"] == 514
    assert cov["lookback_lead_bars"] == 256
    assert cov["n_dropped"] == 1
    reason = cov["dropped"][0]["reason"]
    assert "required 770" in reason
    assert "Hurst window 514" in reason and "lookback 256" in reason


def test_coverage_keeps_a_cell_that_meets_the_deeper_lead():
    cov = study.coverage_audit(_coverage_frames(900), ["2021"], [512])
    assert cov["n_dropped"] == 0
    assert cov["n_kept"] == 1


def test_every_dropped_cell_carries_a_reason():
    cov = study.coverage_audit(_coverage_frames(10), ["2021"], [512])
    assert cov["dropped"]
    for row in cov["dropped"]:
        assert row["dataset"] and row["window"] and row["reason"]


# --- the inference is two-sided everywhere on the confirmatory path -------


_ONE_SIDED = (
    (study1410, "permutation_pvalue_group_diff"),
    (study1410, "permutation_pvalue_weighted"),
    (study1422, "cluster_permutation_pvalue_group_diff"),
    (study1422, "cluster_permutation_pvalue_weighted"),
    (study1424, "permutation_pvalue_group_diff"),
    (study1424, "permutation_pvalue_weighted"),
    (study1424, "cluster_permutation_pvalue_group_diff"),
    (study1424, "cluster_permutation_pvalue_weighted"),
    (study1424, "min_detectable_effect_on_grid"),
    (study1424, "min_detectable_effect_eff"),
    (study1424, "min_detectable_effect_pp"),
)


@pytest.fixture()
def one_sided_is_a_landmine(monkeypatch):
    def _boom(*_a, **_kw):
        raise AssertionError("a one-sided p-value function was reached from "
                             "the two-sided confirmatory path")

    for module, name in _ONE_SIDED:
        monkeypatch.setattr(module, name, _boom, raising=False)
    return _boom


_SWEEP_N_PERM = int(np.ceil(2.0 / (study.ALPHA / 30.0)))


def _pooled_for_sweep():
    cid = study.PRIMARY_CONFIG_ID
    rows = []
    mask = _random_mask(80, seed=11)
    for i in range(40):
        rows.append(_trade(day=i * 5, dh=0.1 if mask[i] else -0.1,
                           eff=1.0 if mask[i] else -1.0,
                           armed={cid: bool(mask[i])}))
        rows.append(_trade(symbol="ETH/USDT", day=i * 5,
                           dh=0.1 if mask[40 + i] else -0.1,
                           eff=1.0 if mask[40 + i] else -1.0,
                           armed={cid: bool(mask[40 + i])}))
    return {study.PRIMARY_FAMILY: rows, _MR: []}


def test_build_configs_never_reaches_a_one_sided_p(one_sided_is_a_landmine):
    cfgs = study.build_configs([], _pooled_for_sweep(), [512], {}, n_perm=50,
                               seed=study.SEED)
    assert cfgs
    assert any(c["p_cluster"] is not None or c["p_raw"] is not None
               for c in cfgs)


def test_measure_detection_limits_never_reaches_a_one_sided_p(
        one_sided_is_a_landmine):
    study.measure_detection_limits(_pooled_for_sweep(), [512],
                                   n_perm=_SWEEP_N_PERM, seed=study.SEED)


def test_the_two_sided_functions_are_1426s_objects():
    assert study.two_sided_cluster_permutation_pvalue_group_diff is \
        study1426.two_sided_cluster_permutation_pvalue_group_diff
    assert study.two_sided_min_detectable_effect_eff is \
        study1426.two_sided_min_detectable_effect_eff
    assert study.doubled_tail_p is study1426.doubled_tail_p
    assert study.TWO_SIDED is True
    assert study.INFERENCE_DIRECTION == "two_sided"


def test_there_is_no_reversed_mode_in_a_symmetric_study():
    assert not hasattr(study, "MODE_REVERSED")
    source = open(study.__file__).read()
    assert "MODE_REVERSED" not in source


# --- Stage 0 is deliberately not re-answered ------------------------------


def test_no_stage_0_verdict_is_reachable_from_this_module(monkeypatch):
    def _boom(*_a, **_kw):
        raise AssertionError("Stage 0 was reached from #1427")

    monkeypatch.setattr(study1422, "joint_separation_verdict", _boom)
    monkeypatch.setattr(study1424, "joint_separation_verdict", _boom)
    pooled = _pooled_for_sweep()
    cfgs = study.build_configs([], pooled, [512], {}, n_perm=50,
                               seed=study.SEED)
    mde = study.measure_detection_limits(pooled, [512], n_perm=_SWEEP_N_PERM,
                                         seed=study.SEED)
    study.validity_gate(mde)
    study.decide_recommendation(cfgs, mde)
    study.joint_adx_delta_table(pooled[study.PRIMARY_FAMILY], 512)
    assert not hasattr(study, "joint_separation_verdict")


def test_the_joint_table_carries_no_verdict():
    table = study.joint_adx_delta_table(
        [_trade(dh=0.1, adx=30.0), _trade(day=5, dh=-0.1, adx=10.0)], 512)
    assert set(table) == {f"{a}|{d}" for a in study.JOINT_ADX_BUCKETS
                          for d in study.JOINT_DELTA_BUCKETS}
    assert all("separated" not in row for row in table.values())


# --- the confirmatory path reads the change, never the level --------------


def test_the_detection_limits_ignore_a_level_column_entirely():
    cid = study.PRIMARY_CONFIG_ID
    mask = _random_mask(60, seed=3)
    plain, poisoned = [], []
    for i in range(60):
        kw = dict(day=i * 5, dh=0.1 if mask[i] else -0.1,
                  eff=1.0 if mask[i] else -1.0, armed={cid: bool(mask[i])})
        plain.append(_trade(**kw))
        poisoned.append(_trade(level_h=0.9 if not mask[i] else 0.1, **kw))
    a = study.measure_detection_limits({study.PRIMARY_FAMILY: plain, _MR: []},
                                       [512], n_perm=_SWEEP_N_PERM,
                                       seed=study.SEED)
    b = study.measure_detection_limits({study.PRIMARY_FAMILY: poisoned,
                                        _MR: []}, [512], n_perm=_SWEEP_N_PERM,
                                       seed=study.SEED)
    assert a["by_family_separation"] == b["by_family_separation"]
    assert a["by_family_cluster"] == b["by_family_cluster"]
    assert a["by_family_cluster_p0"] == b["by_family_cluster_p0"]


def test_build_configs_sizes_from_the_change_and_ignores_a_level_column():
    mask = _random_mask(60, seed=9)
    plain, poisoned = [], []
    for i in range(60):
        kw = dict(day=i * 5, dh=0.1 if mask[i] else -0.1,
                  eff=1.0 if mask[i] else -1.0)
        plain.append(_trade(**kw))
        poisoned.append(_trade(level_h=0.9 if not mask[i] else 0.1, **kw))
    a = study.build_configs([], {study.PRIMARY_FAMILY: plain, _MR: []}, [512],
                            {}, n_perm=50, seed=study.SEED)
    b = study.build_configs([], {study.PRIMARY_FAMILY: poisoned, _MR: []},
                            [512], {}, n_perm=50, seed=study.SEED)
    keys = ("config_id", "separation", "p_cluster", "n_suppressed", "n_kept")
    assert [{k: c[k] for k in keys} for c in a] == \
        [{k: c[k] for k in keys} for c in b]


def test_the_bucket_tables_read_the_change():
    rows = [_trade(day=0, dh=0.2, level_h=0.1),
            _trade(day=5, dh=-0.2, level_h=0.9)]
    table = study.bucket_tables(rows, 512)
    assert table[">=+0.05"]["trades"] == 1
    assert table["<-0.05"]["trades"] == 1


# --- the validity gate ----------------------------------------------------


def _mde(mom_limit=0.05, mom_sep=0.09, mr_limit=0.05, mr_sep=0.02, p0=0.4,
         mom_n=8000, **extra):
    out = {
        "by_family_cluster": {study.PRIMARY_FAMILY: mom_limit,
                              _MR: mr_limit},
        "by_family_separation": {study.PRIMARY_FAMILY: mom_sep, _MR: mr_sep},
        "by_family_n": {study.PRIMARY_FAMILY: mom_n, _MR: 20000},
        "by_family_cluster_p0": {study.PRIMARY_FAMILY: p0, _MR: 0.8},
        "observed_separation_by_pool": {
            "primary": {f"{study.PRIMARY_FAMILY}|512": mom_sep,
                        f"{_MR}|512": mr_sep}},
        "pooled_primary_cluster": 0.01,
        "pooled_primary_free": 0.009,
        "pooled_primary_n": 28000,
        "pooled_primary_cluster_p0": 0.5,
        "pooled_primary_free_p0": 0.5,
        "pooled_primary_cluster_return": 1.0,
        "pooled_primary_cluster_return_p0": 0.6,
    }
    out.update(extra)
    return out


def test_the_gate_passes_on_a_positive_separation_above_the_limit():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.09))
    assert gate["passed"] is True
    assert gate["mode"] == study.MODE_OK
    assert gate["two_sided"] is True


def test_the_gate_passes_on_a_negative_separation_above_the_limit():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=-0.09))
    assert gate["passed"] is True
    assert gate["largest_separation"] == -0.09


def test_the_gate_fails_below_the_limit_in_either_direction():
    assert study.validity_gate(_mde(mom_limit=0.05,
                                    mom_sep=0.01))["passed"] is False
    assert study.validity_gate(_mde(mom_limit=0.05,
                                    mom_sep=-0.01))["passed"] is False
    assert study.validity_gate(_mde(mom_limit=0.05,
                                    mom_sep=-0.01))["mode"] == \
        study.MODE_BELOW_LIMIT


def test_the_gate_fails_closed_on_an_unreachable_limit():
    gate = study.validity_gate(_mde(mom_limit=None, mom_sep=0.4))
    assert gate["passed"] is False
    assert gate["mode"] == study.MODE_UNRESOLVABLE


def test_the_gate_fails_closed_with_no_separation_at_all():
    gate = study.validity_gate(_mde(mom_sep=None))
    assert gate["passed"] is False
    assert gate["mode"] == study.MODE_NO_SEPARATION


def test_the_gate_reads_the_confirmatory_familys_own_rows_not_the_pool():
    mde = _mde(mom_limit=0.05, mom_sep=0.01, pooled_primary_cluster=0.001)
    gate = study.validity_gate(mde)
    assert gate["passed"] is False
    assert gate["limit"] == 0.05
    assert gate["n_rows"] == mde["by_family_n"][study.PRIMARY_FAMILY]


def test_the_gate_never_borrows_the_other_familys_numbers():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.01,
                                    mr_limit=0.001, mr_sep=0.9))
    assert gate["family"] == study.PRIMARY_FAMILY
    assert gate["passed"] is False
    assert gate["largest_separation"] == 0.01


# --- nothing is ever promoted --------------------------------------------


def _passing_cfg(**over):
    windows = {w: {"n_legs": 1, "dd_delta": -1.0, "chop_delta": -1.0,
                   "ret_gated": 10.0, "ret_ungated": 10.0}
               for w in study.WINDOW_ORDER}
    cfg = {
        "config_id": study.PRIMARY_CONFIG_ID,
        "cohort": study.COHORT_PRIMARY,
        "family": study.PRIMARY_FAMILY,
        "mode": "gate",
        "hurst_window": 512,
        "lookback_bars": 256,
        "arm": study.PRIMARY_ARM,
        "disarm": study.PRIMARY_DISARM,
        "gain": None,
        "protocol_windows": list(study.PRIMARY_PROTOCOL_WINDOWS),
        "protocol_min_windows": study.PRIMARY_PROTOCOL_MIN_WINDOWS,
        "held_out_windows": list(study.PRIMARY_HELD_OUT_WINDOWS),
        "windows": windows,
        "p_raw": 0.001, "p_cluster": 0.001, "p_cluster_return": 0.001,
        "separation": 0.09, "separation_return": 1.0,
        "bh_reject": True,
        "n_pooled_trades": 500, "n_missing_target": 0,
        "n_suppressed": 100, "n_kept": 400,
        "n_pooled_effective": 200.0, "n_suppressed_effective": 60.0,
        "n_kept_effective": 140.0,
        "cluster_excluded_datasets": [], "cluster_excluded_trades": 0,
        "cluster_offset_range": [30, 300], "cluster_distinct_offsets": 271,
        "cluster_reason": None,
    }
    cfg.update(over)
    return cfg


def test_this_module_defines_no_configuration_verdict_of_its_own():
    assert study.config_verdict is study1424.config_verdict
    source = open(study.__file__).read()
    assert "def config_verdict" not in source


def test_a_config_that_passes_1424s_rule_still_wins_nothing():
    cfg = _passing_cfg()
    assert study1424.config_verdict(cfg)[0] is True
    decision = study.decide_recommendation([cfg], _mde())
    assert decision["families"][study.PRIMARY_FAMILY]["n_passing"] == 1
    assert decision["families"][study.PRIMARY_FAMILY]["winner"] is None


@pytest.mark.parametrize("mde_kw", [
    {"mom_limit": 0.05, "mom_sep": 0.09, "p0": 0.0001},
    {"mom_limit": 0.05, "mom_sep": 0.01, "p0": 0.9},
    {"mom_limit": None, "mom_sep": 0.4, "p0": 0.5},
    {"mom_sep": None, "p0": None},
])
def test_no_verdict_can_name_a_winner_whatever_the_inputs(mde_kw):
    decision = study.decide_recommendation([_passing_cfg()], _mde(**mde_kw))
    assert all(v["winner"] is None for v in decision["families"].values())
    assert study.decision_payload(decision)["families"][
        study.PRIMARY_FAMILY]["winner"] is None


def test_every_verdict_carries_the_no_promotion_sentence():
    for mde_kw in ({"mom_sep": 0.09, "p0": 0.0001},
                   {"mom_sep": 0.09, "p0": 0.9},
                   {"mom_sep": 0.001, "p0": 0.9}):
        decision = study.decide_recommendation([_passing_cfg()],
                                               _mde(**mde_kw))
        assert study.NO_PROMOTION_SENTENCE in decision["justification"]


def test_a_significant_contrast_is_a_finding_about_the_change():
    decision = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.05, mom_sep=0.09, p0=0.0001))
    assert decision["verdict"] == study.VERDICT_CHANGE_SORTS
    assert decision["significant"] is True
    assert "CHANGE in the Hurst exponent" in decision["justification"]


def test_a_significant_contrast_below_the_limit_stays_a_power_statement():
    decision = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.5, mom_sep=0.01, p0=0.0001))
    assert decision["verdict"] == study.VERDICT_CHANGE_SORTS
    assert decision["key_risk_held"] is False
    assert "POWER statement" in decision["justification"]


def test_a_bound_is_claimed_only_when_the_gate_passed():
    passed = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.05, mom_sep=0.09, p0=0.9))
    assert passed["verdict"] == study.VERDICT_RESOLVED_NULL
    assert "EITHER DIRECTION" in passed["justification"]
    failed = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.5, mom_sep=0.01, p0=0.9))
    assert failed["verdict"] == study.VERDICT_INCONCLUSIVE
    assert "POWER statement" in failed["justification"]


def test_the_verdict_only_speaks_about_the_market_when_the_limit_clears():
    for mde_kw in ({"mom_limit": 0.5, "mom_sep": 0.01, "p0": 0.9},
                   {"mom_limit": None, "mom_sep": 0.4, "p0": 0.9},
                   {"mom_sep": None, "p0": None}):
        decision = study.decide_recommendation([_passing_cfg()],
                                               _mde(**mde_kw))
        assert decision["verdict"] == study.VERDICT_INCONCLUSIVE
        assert "POWER statement" in decision["justification"]
        assert "statement about the market" not in \
            decision["justification"].replace(
                "not a statement about the market", "")


def test_a_zero_limit_is_flagged_as_degenerate_everywhere():
    assert study.limit_is_degenerate(0.0) is True
    assert study.limit_is_degenerate(0.001) is False
    assert study.limit_is_degenerate(None) is False
    for mde_kw in ({"mom_limit": 0.0, "mom_sep": 0.01},
                   {"mom_limit": 0.0, "mom_sep": None},
                   {"mom_limit": 0.05, "mom_sep": 0.09},
                   {"mom_limit": None, "mom_sep": 0.09}):
        gate = study.validity_gate(_mde(**mde_kw))
        assert "limit_is_degenerate" in gate
        assert gate["limit_is_degenerate"] is (mde_kw["mom_limit"] == 0.0)


def test_a_degenerate_limit_passes_the_gate_but_corroborates_nothing():
    gate = study.validity_gate(_mde(mom_limit=0.0, mom_sep=0.0099))
    assert gate["passed"] is True
    assert gate["limit_is_degenerate"] is True
    decision = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.0, mom_sep=0.0099, p0=0.011))
    assert decision["verdict"] == study.VERDICT_CHANGE_SORTS
    text = decision["justification"]
    assert "PASSES TRIVIALLY" in text
    assert "corroborates NOTHING" in text
    assert "effect SIZE is therefore unestimated" in text
    assert "rather than one it merely reached significance on" not in text


def test_a_non_degenerate_pass_still_claims_what_it_earned():
    decision = study.decide_recommendation(
        [_passing_cfg()], _mde(mom_limit=0.005, mom_sep=0.0099, p0=0.011))
    text = decision["justification"]
    assert "PASSED on a NON-degenerate limit" in text
    assert "PASSES TRIVIALLY" not in text


def test_the_gate_sentence_names_a_degenerate_limit():
    degenerate = study._render_gate_sentence(
        study.validity_gate(_mde(mom_limit=0.0, mom_sep=0.0099)))
    assert "DEGENERATE" in degenerate
    real = study._render_gate_sentence(
        study.validity_gate(_mde(mom_limit=0.005, mom_sep=0.0099)))
    assert "DEGENERATE" not in real


def test_every_verdict_reports_the_continuity_target_beside_the_primary():
    for mde_kw in ({"mom_limit": 0.0, "mom_sep": 0.0099, "p0": 0.011},
                   {"mom_limit": 0.05, "mom_sep": 0.09, "p0": 0.9},
                   {"mom_limit": 0.5, "mom_sep": 0.01, "p0": 0.9}):
        decision = study.decide_recommendation([_passing_cfg()],
                                               _mde(**mde_kw))
        assert "CONTINUITY target" in decision["justification"]


def test_the_continuity_clause_says_when_the_economics_are_unestimated():
    below = study.continuity_clause(_mde(
        by_family_separation_return={study.PRIMARY_FAMILY: 0.55, _MR: 0.0},
        by_family_cluster_return={study.PRIMARY_FAMILY: 1.1, _MR: 0.4},
        by_family_cluster_return_p0={study.PRIMARY_FAMILY: 0.81, _MR: 0.3}))
    assert "BELOW that limit" in below and "UNESTIMATED" in below
    assert "never on its own a licence to ship a gate" in below
    clears = study.continuity_clause(_mde(
        by_family_separation_return={study.PRIMARY_FAMILY: 2.0, _MR: 0.0},
        by_family_cluster_return={study.PRIMARY_FAMILY: 1.1, _MR: 0.4},
        by_family_cluster_return_p0={study.PRIMARY_FAMILY: 0.01, _MR: 0.3}))
    assert "clears" in clears and "UNESTIMATED" not in clears
    unresolvable = study.continuity_clause(_mde(
        by_family_separation_return={study.PRIMARY_FAMILY: 0.1, _MR: 0.0},
        by_family_cluster_return={study.PRIMARY_FAMILY: None, _MR: None},
        by_family_cluster_return_p0={study.PRIMARY_FAMILY: 0.9, _MR: 0.3}))
    assert "UNESTIMATED" in unresolvable
    silent = study.continuity_clause(_mde(
        by_family_separation_return={study.PRIMARY_FAMILY: None, _MR: None},
        by_family_cluster_return={study.PRIMARY_FAMILY: 1.1, _MR: 0.4},
        by_family_cluster_return_p0={study.PRIMARY_FAMILY: None, _MR: None}))
    assert "nothing at all about the economics" in silent


def test_the_predecessor_reference_matches_the_committed_predecessors():
    ref = study.predecessor_reference()
    with open(study1424._DEFAULT_JSON_OUT) as fh:
        m1424 = json.load(fh)["mde"]
    with open(study1426._DEFAULT_JSON_OUT) as fh:
        m1426 = json.load(fh)["mde"]
    assert ref["1424_family_separation"] == \
        m1424["by_family_separation"][study.PRIMARY_FAMILY]
    assert ref["1426_family_limit"] == \
        m1426["by_family_cluster"][study.PRIMARY_FAMILY]
    assert ref["1426_pooled_primary_limit"] == m1426["pooled_primary_cluster"]


def test_the_prediction_audit_refuses_to_read_a_degenerate_limit():
    ref = {"1426_family_limit": 0.013, "1426_pooled_primary_limit": 0.009}
    text = study.prediction_audit(_mde(mom_limit=0.0, mom_sep=0.0099), ref)
    assert "DEGENERATE" in text
    assert "cannot be read off it" in text


def test_the_prediction_audit_calls_its_own_mechanism_wrong_when_it_is():
    ref = {"1426_family_limit": 0.013, "1426_pooled_primary_limit": 0.009}
    wrong = study.prediction_audit(
        _mde(mom_limit=0.0, mom_sep=0.0099, pooled_primary_cluster=0.007), ref)
    assert "was WRONG" in wrong
    assert "changes the SPLIT" in wrong
    held = study.prediction_audit(
        _mde(mom_limit=0.02, mom_sep=0.03, pooled_primary_cluster=0.011), ref)
    assert "was WRONG" not in held
    assert "AT OR ABOVE, as predicted." in held


def test_an_untestable_confirmatory_p_is_not_significance():
    decision = study.decide_recommendation([_passing_cfg()], _mde(p0=None))
    assert decision["significant"] is False
    assert decision["verdict"] != study.VERDICT_CHANGE_SORTS


def test_the_confirmatory_bar_is_alpha_for_a_family_of_one():
    decision = study.decide_recommendation([_passing_cfg()], _mde())
    assert decision["confirmatory_bar"] == study.ALPHA
    assert study.PRIMARY_FAMILY_SIZE == 1


def test_the_confirmatory_p_is_the_row_matched_one_not_the_pool():
    mde = _mde(p0=0.4, pooled_primary_cluster_p0=0.0001)
    assert study.confirmatory_p(mde) == 0.4
    assert study.decide_recommendation([_passing_cfg()],
                                       mde)["confirmatory_p"] == 0.4


# --- the report -----------------------------------------------------------


def _render_payload(decision=None, mde=None, configs=None, dropped=None):
    mde = dict(mde or _mde(mom_limit=0.02, mom_sep=-0.005, p0=0.71))
    mde.setdefault("observed_separation_pp_by_pool",
                   {"primary": {f"{study.PRIMARY_FAMILY}|512": -0.12}})
    mde.setdefault("by_family_cluster_return",
                   {study.PRIMARY_FAMILY: 1.4, _MR: 0.9})
    mde.setdefault("by_family_separation_return",
                   {study.PRIMARY_FAMILY: -0.12, _MR: -0.23})
    mde.setdefault("by_family_cluster_return_p0",
                   {study.PRIMARY_FAMILY: 0.85, _MR: 0.9})
    cfgs = list(configs if configs is not None
                else [_passing_cfg(bh_reject=False, p_cluster=0.71,
                                   p_raw=0.7, p_cluster_return=0.85)])
    decision = decision or study.decide_recommendation(cfgs, mde)
    warm = study.delta_warmup_audit({"BTC/USDT 1h": 900}, [512])
    return {
        "schema_version": study.SCHEMA_VERSION,
        "issue": study.ISSUE,
        "pre_registered": {
            "hurst_windows": [512],
            "lookback_bars": {"512": 256},
            "primary_lookback_bars": 256,
            "level_edges": list(study.LEVEL_EDGES),
            "delta_edges": list(study.DELTA_EDGES),
            "delta_buckets": list(study.DELTA_BUCKETS),
            "windows": {w: list(study.WINDOWS[w]) for w in study.WINDOW_ORDER},
            "datasets": ["BTC/USDT 1h"],
            "fee_platform": study.FEE_PLATFORM,
            "n_perm": study.N_PERM,
            "n_perm_mde": study.N_PERM_MDE,
            "seed": study.SEED,
            "feasibility_probes": [dict(p) for p in study.FEASIBILITY_PROBES],
        },
        "run_summary": {
            "scope": {"complete": True, "pre_registered_inference": True},
            "legs": 1, "gated_arms": 9, "mirror_verified_legs": 1,
            "pooled_trades": {f: 1 for f in study.FAMILIES},
            "pooled_primary": {f: 1 for f in study.FAMILIES},
            "pooled_exploratory": {f: 0 for f in study.FAMILIES},
            "n_primary_configs": 1, "n_exploratory_configs": 30,
            "n_primary_significant": 0, "n_exploratory_significant": 0,
            "warmup": warm,
            "coverage": {"n_kept": 1, "n_cells": 2, "n_dropped": len(
                dropped or []), "n_unowned": 3, "required_lead_bars": 770,
                "hurst_only_required_lead_bars": 514,
                "lookback_lead_bars": 256,
                "min_window_bar_fraction": 0.8,
                "reference_last_bar": "2026-01-01",
                "dropped": list(dropped or [])},
            "symbol_correlations": {},
            "elapsed_sec": 1.0,
        },
        "mde": mde,
        "buckets": {f: {"512": study.bucket_tables([], 512)}
                    for f in study.FAMILIES},
        "joint": {f: {"table": study.joint_adx_delta_table([], 512)}
                  for f in study.FAMILIES},
        "configs": cfgs,
        "legs": [],
        "decision": decision,
    }


def test_report_renders_and_ends_with_the_recommendation():
    text = study.report_from_payload(_render_payload())
    body, _, tail = text.rpartition("## Recommendation")
    assert body and "## Recommendation" not in tail


def test_report_states_all_three_pre_registered_decisions_verbatim():
    text = study.report_from_payload(_render_payload())
    assert study.RECENTRING_RULE in text
    assert study.DELTA_LOOKBACK_RATIONALE in text
    assert study.INFERENCE_DIRECTION_RATIONALE in text
    assert study.PRIOR_EXPOSURE_DISCLOSURE in text
    assert study.PRIMARY_HYPOTHESIS_STATEMENT in text
    assert study.KEY_RISK_PREDICTION in text


def test_report_names_the_contract_path_deferral_and_the_sibling():
    text = study.report_from_payload(_render_payload())
    assert study.CONTRACT_PATH_STATEMENT in text
    assert "#1428" in text
    assert study.NO_PROMOTION_SENTENCE in text


def test_report_states_the_limit_and_the_signed_separation_together():
    text = study.report_from_payload(_render_payload())
    assert "Measured two-sided detection limit on those same rows" in text
    assert "-0.0050" in text
    assert "Separations carry their SIGN" in text


def test_report_says_the_verdict_is_a_power_statement_when_the_gate_fails():
    text = study.report_from_payload(_render_payload())
    assert "**Outcome: FAILED**" in text
    assert "POWER statement" in text


def test_report_prints_the_warmup_split_and_every_dropped_cell():
    dropped = [{"dataset": "BTC/USD@bitstamp 1h", "window": "2013",
                "reason": "lead 300 bars before 2013-01-01 < required 770 "
                          "(Hurst window 514 + lookback 256)"}]
    text = study.report_from_payload(_render_payload(dropped=dropped))
    assert "| Hurst window | Lookback | Margin | Required lead |" in text
    assert "| 512 | 256 | 2 | 770 |" in text
    assert "BTC/USD@bitstamp 1h" in text
    assert "lookback 256" in text
    assert "The lead requirement is where this study pays for its predictor." \
        in text


def test_report_prints_the_recentring_table():
    text = study.report_from_payload(_render_payload())
    assert "| Bucket edges | 0.45, 0.5, 0.55 | -0.05, +0.00, +0.05 |" in text
    assert f"| Pinned hypothesis | `{study.LEVEL_PRIMARY_CONFIG_ID}` | " \
           f"`{study.PRIMARY_CONFIG_ID}` |" in text


def test_report_discloses_that_stage_0_is_not_re_answered():
    text = study.report_from_payload(_render_payload())
    assert study.STAGE0_EXCLUSION in text
    assert "NO Stage 0 verdict" in text


def test_report_lists_what_the_study_cannot_say():
    text = study.report_from_payload(_render_payload())
    assert "## What this study cannot say" in text
    assert "It cannot calibrate `scheduler/hurst_gate.go`" in text
    assert "It cannot claim an independent sample" in text
    assert "It cannot size an effect it rejects" in text


def test_report_prints_the_construction_caveat_and_the_degenerate_note():
    text = study.report_from_payload(_render_payload())
    assert study.CONSTRUCTION_CAVEAT in text
    assert study.DEGENERATE_LIMIT_DISCLOSURE in text
    assert "## What a positive result here would and would not mean" in text
    assert "The two targets on the confirmatory family, side by side." in text


def test_report_audits_the_prediction_against_the_predecessor_run():
    payload = _render_payload(
        mde=_mde(mom_limit=0.0, mom_sep=0.0099, p0=0.011,
                 pooled_primary_cluster=0.007))
    payload["pre_registered"]["predecessor_reference"] = {
        "1426_family_limit": 0.013, "1426_pooled_primary_limit": 0.009}
    text = study.report_from_payload(payload)
    assert "was WRONG" in text
    assert "POOLED primary limit" in text


def test_report_prints_the_two_sided_p_definition():
    text = study.report_from_payload(_render_payload())
    assert "p2   = min(1, 2 * min(p_ge, p_le))" in text
    assert "2/(draws+1)" in text


def test_report_never_licenses_a_threshold():
    text = study.report_from_payload(_render_payload())
    assert "DEFAULT-OFF" in text
    assert "No configuration is recommended, and none could be." in text


# --- the contract path is refused unconditionally -------------------------


def test_1427_does_not_default_to_the_contract_path():
    assert os.path.basename(study._DEFAULT_REPORT_OUT) == \
        "hurst_1427_change_sort.md"
    assert os.path.basename(study._DEFAULT_JSON_OUT) == \
        "hurst_1427_change_sort.json"
    assert study.CONTRACT_PATH_CLAIMED is False
    assert study.SIBLING_DEFERRAL == (1428,)
    assert study.DEFERRING_SIBLINGS == (1426,)


def test_1424_still_owns_the_contract_path():
    assert os.path.basename(study1424._DEFAULT_REPORT_OUT) == \
        "hurst_gate_calibration.md"


def test_1427_may_not_write_the_contract_path_even_when_asked(tmp_path):
    with pytest.raises(SystemExit) as exc:
        study.main(["--report-out", CONTRACT,
                    "--json-out", str(tmp_path / "scoped.json")])
    assert "DEFERS" in str(exc.value)
    assert "1424" in str(exc.value)


def test_the_contract_refusal_survives_render_only(tmp_path):
    payload = _render_payload()
    payload["decision"] = study.decision_payload(payload["decision"])
    path = tmp_path / "complete.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path),
                    "--report-out", CONTRACT, "--write-report"])
    assert "DEFERS" in str(exc.value)


def test_the_contract_refusal_is_checked_before_every_other_refusal(tmp_path):
    with pytest.raises(SystemExit) as exc:
        study.main(["--only", "momentum", "--report-out", CONTRACT,
                    "--json-out", str(tmp_path / "scoped.json")])
    assert "DEFERS" in str(exc.value)


# --- scoped and deviating runs cannot touch the committed artifacts -------


def test_scoped_run_may_not_overwrite_the_committed_json():
    with pytest.raises(SystemExit) as exc:
        study.main(["--only", "momentum"])
    assert "committed aggregate" in str(exc.value)


@pytest.mark.parametrize("flag,value", [("--only", "momentum"),
                                        ("--datasets", "BTC/USDT:1h"),
                                        ("--windows", "2017"),
                                        ("--hurst-windows", "128")])
def test_every_scoping_flag_protects_the_committed_report(tmp_path, flag,
                                                          value):
    with pytest.raises(SystemExit) as exc:
        study.main([flag, value, "--json-out", str(tmp_path / "scoped.json")])
    assert "committed report" in str(exc.value)


@pytest.mark.parametrize("argv,needle", [
    (["--n-perm-mde", "1300"], "--n-perm-mde 1300"),
    (["--n-perm", "200"], "--n-perm 200"),
    (["--seed", "7"], "--seed 7"),
    (["--no-mirror-check"], "--no-mirror-check"),
])
def test_a_deviating_run_may_not_write_the_committed_artifacts(tmp_path, argv,
                                                               needle):
    with pytest.raises(SystemExit) as exc:
        study.main(argv)
    assert "committed aggregate" in str(exc.value)
    assert needle in str(exc.value)
    with pytest.raises(SystemExit) as exc:
        study.main(argv + ["--json-out", str(tmp_path / "debug.json")])
    assert "committed report" in str(exc.value)


class _Args:
    def __init__(self, **kw):
        self.n_perm = kw.get("n_perm", study.N_PERM)
        self.n_perm_mde = kw.get("n_perm_mde", study.N_PERM_MDE)
        self.seed = kw.get("seed", study.SEED)
        self.no_mirror_check = kw.get("no_mirror_check", False)


def test_stating_the_pre_registered_settings_explicitly_is_not_a_deviation():
    assert study.inference_deviations(_Args()) == []


@pytest.mark.parametrize("kw,needle", [
    ({"n_perm_mde": study.N_PERM_MDE - 1}, "--n-perm-mde"),
    ({"n_perm": 200}, "--n-perm "),
    ({"seed": study.SEED + 1}, "--seed"),
    ({"no_mirror_check": True}, "--no-mirror-check"),
])
def test_every_inference_deviation_is_named(kw, needle):
    found = study.inference_deviations(_Args(**kw))
    assert len(found) == 1
    assert needle in found[0]


def test_render_only_refuses_an_unstamped_payload(tmp_path):
    payload = _render_payload()
    payload["decision"] = study.decision_payload(payload["decision"])
    payload["run_summary"]["scope"] = {}
    path = tmp_path / "unstamped.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path),
                    "--write-report"])
    assert "not stamped as a complete run" in str(exc.value)


def test_render_only_refuses_a_payload_not_stamped_pre_registered(tmp_path):
    payload = _render_payload()
    payload["decision"] = study.decision_payload(payload["decision"])
    payload["run_summary"]["scope"] = {"complete": True}
    path = tmp_path / "deviating.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path),
                    "--write-report"])
    assert "pre-registered inference" in str(exc.value)


def test_render_only_to_the_committed_report_needs_write_report(tmp_path):
    payload = _render_payload()
    payload["decision"] = study.decision_payload(payload["decision"])
    path = tmp_path / "complete.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path)])
    assert "needs --write-report" in str(exc.value)


def test_render_only_writes_a_non_committed_path_freely(tmp_path):
    payload = _render_payload()
    payload["decision"] = study.decision_payload(payload["decision"])
    path = tmp_path / "p.json"
    path.write_text(json.dumps(payload))
    out = tmp_path / "r.md"
    assert study.main(["--render-only", "--json-out", str(path),
                       "--report-out", str(out)]) == 0
    assert out.exists()


def test_fetch_only_may_be_scoped_to_the_venues_that_need_it(monkeypatch):
    seen = {}

    def _fake(datasets):
        seen["datasets"] = list(datasets)
        return {}

    monkeypatch.setattr(study, "ensure_min_history", _fake)
    assert study.main(["--fetch-only", "--datasets",
                       "bitstamp=BTC/USD:1h"]) == 0
    assert seen["datasets"] == [("bitstamp", "BTC/USD", "1h")]


# --- the rest of the design is inherited, not restated --------------------


def test_the_estimator_is_the_1409_ssot_and_is_never_reimplemented():
    assert study.rolling_hurst is study1410.rolling_hurst
    source = open(study.__file__).read()
    assert "def hurst_exponent" not in source
    assert "def rolling_hurst(" not in source


def test_the_design_is_inherited_from_1424_rather_than_restated():
    assert study.WINDOWS == study1424.WINDOWS
    assert study.WINDOW_ORDER == study1424.WINDOW_ORDER
    assert study.DATASETS == study1424.DATASETS
    assert study.DATASET_WINDOWS == study1424.DATASET_WINDOWS
    assert study.WINDOW_OWNER == study1424.WINDOW_OWNER
    assert study.PRIMARY_TARGET == study1424.PRIMARY_TARGET
    assert study.HORIZON_HOURS == study1424.HORIZON_HOURS
    assert study.signed_efficiency is study1424.signed_efficiency
    assert study.cell_cohort is study1424.cell_cohort
    assert study.effective_n is study1422.effective_n
    assert study.usable_cluster_rows is study1422.usable_cluster_rows


def test_the_seed_is_the_issue_number():
    assert study.SEED == study.ISSUE == 1427


def test_the_quoted_predecessor_numbers_match_their_source_runs():
    with open(study1424._DEFAULT_JSON_OUT) as fh:
        m1424 = json.load(fh)["mde"]
    with open(study1426._DEFAULT_JSON_OUT) as fh:
        m1426 = json.load(fh)["mde"]
    sep = m1424["by_family_separation"][study.PRIMARY_FAMILY]
    limit = m1426["by_family_cluster"][study.PRIMARY_FAMILY]
    assert f"{sep:.3f}" == "-0.005"
    assert f"{limit:g}" == "0.013"
    assert "-0.005 efficiency units" in study.INFERENCE_DIRECTION_RATIONALE
    assert "the level's -0.005" in study.KEY_RISK_PREDICTION
    assert "0.013 efficiency units #1426 measured" in study.KEY_RISK_PREDICTION
    assert "BELOW 0.013 on these rows" in study.KEY_RISK_PREDICTION


def test_the_report_intro_quotes_the_predecessors_correctly():
    with open(study1424._DEFAULT_JSON_OUT) as fh:
        m1424 = json.load(fh)["mde"]
        fh.seek(0)
    with open(study1424._DEFAULT_JSON_OUT) as fh:
        v1424 = json.load(fh)["decision"]["verdict"]
    with open(study1426._DEFAULT_JSON_OUT) as fh:
        p1426 = json.load(fh)
    text = study.report_from_payload(_render_payload())
    assert f"{m1424['by_family_separation'][study.PRIMARY_FAMILY]:.3f} " \
        f"efficiency units against a limit of " \
        f"{m1424['by_family_cluster'][study.PRIMARY_FAMILY]:g}" in text
    assert v1424 == study.VERDICT_INCONCLUSIVE
    assert p1426["decision"]["verdict"] == study.VERDICT_INCONCLUSIVE
    assert "#1426 re-tested the same contrast two-sided and stayed " \
        "inconclusive" in text


def test_the_registry_row_records_the_predictor_and_the_deferral():
    path = os.path.join(os.path.dirname(study.__file__), "..", "..", "docs",
                        "backtesting-registry.md")
    with open(os.path.abspath(path)) as fh:
        rows = [ln for ln in fh if "hurst_1427_change_sort.py" in ln]
    assert len(rows) == 1
    row = rows[0]
    assert "CHANGE" in row
    assert "DEFERS" in row and "hurst_gate_calibration.md" in row
    assert "#1428" in row


# --- the committed artifacts ----------------------------------------------


def _committed():
    with open(study._DEFAULT_JSON_OUT) as fh:
        return json.load(fh)


def test_the_committed_decision_is_what_the_current_rule_produces():
    payload = _committed()
    fresh = study.decision_payload(
        study.decide_recommendation(payload["configs"], payload["mde"]))
    assert payload["decision"] == fresh


def test_the_committed_report_is_what_the_committed_json_renders():
    payload = _committed()
    with open(study._DEFAULT_REPORT_OUT) as fh:
        assert study.report_from_payload(payload) == fh.read()


def test_the_committed_run_is_complete_and_pre_registered():
    scope = _committed()["run_summary"]["scope"]
    assert scope["complete"] is True
    assert scope["pre_registered_inference"] is True


def test_the_committed_run_declares_its_pre_registration():
    pre = _committed()["pre_registered"]
    assert pre["predictor"] == "delta_hurst"
    assert pre["inference_direction"] == "two_sided"
    assert pre["two_sided"] is True
    assert pre["lookback_denominator"] == 2
    assert pre["level_edges"] == list(study.LEVEL_EDGES)
    assert pre["delta_edges"] == list(study.DELTA_EDGES)
    assert pre["delta_buckets"] == list(study.DELTA_BUCKETS)
    assert pre["primary_config_id"] == study.PRIMARY_CONFIG_ID
    assert pre["level_primary_config_id"] == study.LEVEL_PRIMARY_CONFIG_ID
    assert pre["primary_family_size"] == 1
    assert pre["recentring_rule"] == study.RECENTRING_RULE
    assert pre["lookback_rationale"] == study.DELTA_LOOKBACK_RATIONALE
    assert pre["inference_direction_rationale"] == \
        study.INFERENCE_DIRECTION_RATIONALE
    assert pre["prior_exposure_disclosure"] == study.PRIOR_EXPOSURE_DISCLOSURE


def test_the_committed_run_defers_the_contract_path():
    pre = _committed()["pre_registered"]
    assert pre["contract_path_claimed"] is False
    assert pre["sibling_deferral"] == [1428]
    assert pre["deferring_siblings"] == [1426]
    assert pre["contract_path_statement"] == study.CONTRACT_PATH_STATEMENT


def test_the_committed_run_recommends_nothing():
    decision = _committed()["decision"]
    assert all(v["winner"] is None for v in decision["families"].values())
    assert study.NO_PROMOTION_SENTENCE in decision["justification"]
    assert decision["contract_path_claimed"] is False


def test_the_committed_gate_is_two_sided_and_row_matched():
    payload = _committed()
    gate = payload["decision"]["validity_gate"]
    assert gate["family"] == study.PRIMARY_FAMILY
    assert gate["two_sided"] is True
    assert gate["n_rows"] == payload["mde"]["by_family_n"][study.PRIMARY_FAMILY]
    assert gate["limit"] == payload["mde"]["by_family_cluster"][
        study.PRIMARY_FAMILY]
    assert gate["largest_separation"] == pytest.approx(
        payload["mde"]["by_family_separation"][study.PRIMARY_FAMILY])


def test_the_committed_run_paid_for_the_lookback_in_its_warm_up():
    run = _committed()["run_summary"]
    warm = run["warmup"]
    cov = run["coverage"]
    assert warm["required_bars"] == warm["hurst_only_required_bars"] + \
        max(study.delta_lookback_bars(hw)
            for hw in _committed()["pre_registered"]["hurst_windows"])
    assert cov["required_lead_bars"] > cov["hurst_only_required_lead_bars"]
    assert cov["lookback_lead_bars"] == \
        cov["required_lead_bars"] - cov["hurst_only_required_lead_bars"]
    for row in cov["dropped"]:
        assert row["reason"]


def test_the_committed_predecessor_reference_is_stamped():
    pre = _committed()["pre_registered"]
    assert pre["predecessor_reference"] == study.predecessor_reference()
    assert pre["degenerate_limit_disclosure"] == \
        study.DEGENERATE_LIMIT_DISCLOSURE
    assert pre["construction_caveat"] == study.CONSTRUCTION_CAVEAT


def test_the_committed_run_never_oversells_a_degenerate_limit():
    payload = _committed()
    gate = payload["decision"]["validity_gate"]
    text = payload["decision"]["justification"]
    assert "limit_is_degenerate" in gate
    if gate["limit_is_degenerate"]:
        assert gate["limit"] == 0.0
        assert "PASSES TRIVIALLY" in text
        assert "corroborates NOTHING" in text
        assert "rather than one it merely reached significance on" not in text
    assert "CONTINUITY target" in text


def test_the_committed_report_carries_both_caveats():
    with open(study._DEFAULT_REPORT_OUT) as fh:
        text = fh.read()
    assert study.CONSTRUCTION_CAVEAT in text
    assert study.DEGENERATE_LIMIT_DISCLOSURE in text
    assert study.KEY_RISK_PREDICTION in text


def test_the_committed_report_never_licenses_a_threshold():
    with open(study._DEFAULT_REPORT_OUT) as fh:
        text = fh.read()
    assert "DEFAULT-OFF" in text
    assert "No configuration is recommended, and none could be." in text
    assert study.CONTRACT_PATH_STATEMENT in text
    assert "hurst_gate_calibration.md" in text
