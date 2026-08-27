import json
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import hurst_1424_gate_resolution as study
import hurst_1422_gate_power as study1422
import hurst_1410_gate_calibration as study1410

DAY_NS = 86_400_000_000_000


def _trade(symbol="BTC/USDT", timeframe="1h", window="2021", day=0, pnl=1.0,
           eff=0.1, hold_days=1, h=None, adx=None, cohort=None,
           exchange="binanceus", armed=None):
    entry = pd.Timestamp("2021-01-01") + pd.Timedelta(days=day)
    return {
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
        "h": {512: h, 128: h},
        "armed": dict(armed or {}),
    }


def test_primary_hypothesis_is_the_committed_1410_argmin():
    assert study.resolve_primary_config_id(study._JSON_1410) == \
        study.PRIMARY_CONFIG_ID


def test_primary_pick_never_reads_the_1422_outcomes(tmp_path):
    stub = tmp_path / "stub_1410.json"
    stub.write_text(json.dumps({"configs": [
        {"config_id": "mean_reversion/gate/W128/arm0.4/dis0.48", "p_raw": 0.9},
        {"config_id": "momentum/size/W256/gain5", "p_raw": 0.002},
        {"config_id": "momentum/gate/W512/arm0.52/dis0.48", "p_raw": 0.5},
        {"config_id": "momentum/gate/W128/arm0.55/dis0.5", "p_raw": None},
    ]}))
    assert study.resolve_primary_config_id(str(stub)) == "momentum/size/W256/gain5"


def test_primary_family_size_is_one_and_moves_the_rank1_bar():
    assert study.PRIMARY_FAMILY_SIZE == 1
    assert len(study.PRIMARY_CONFIG_IDS) == 1
    assert study._rank1_threshold(study.PRIMARY_FAMILY_SIZE) == pytest.approx(study.ALPHA)
    assert study._rank1_threshold(4) == pytest.approx(study.ALPHA / 4.0)


def test_sweep_grid_primary_is_exactly_the_pinned_hypothesis():
    grid = study._sweep_grid(study.COHORT_PRIMARY, study.HURST_WINDOWS)
    ids = []
    for family, mode, hw, arm, disarm, gain in grid:
        ids.append(study.gate_config_id(family, hw, arm, disarm) if mode == "gate"
                   else study.size_config_id(family, hw, gain))
    assert ids == [study.PRIMARY_CONFIG_ID]


def test_mean_reversion_is_exploratory_only():
    assert study.PRIMARY_FAMILY == "momentum"
    grid = study._sweep_grid(study.COHORT_EXPLORATORY, study.HURST_WINDOWS)
    families = {f for f, *_ in grid}
    assert families == set(study.FAMILIES)
    primary_families = {f for f, *_ in
                        study._sweep_grid(study.COHORT_PRIMARY, study.HURST_WINDOWS)}
    assert primary_families == {"momentum"}


def test_bh_denominator_is_per_cohort_and_primary_is_one():
    cfgs = [
        {"cohort": study.COHORT_PRIMARY, "p_cluster": 0.04},
        {"cohort": study.COHORT_EXPLORATORY, "p_cluster": 0.04},
    ] + [{"cohort": study.COHORT_EXPLORATORY, "p_cluster": 0.9} for _ in range(29)]
    study.apply_bh_by_cohort(cfgs)
    assert cfgs[0]["bh_reject"] is True
    assert cfgs[1]["bh_reject"] is False


def test_bh_reject_is_reset_not_merely_set():
    cfgs = [{"cohort": study.COHORT_PRIMARY, "p_cluster": 0.9, "bh_reject": True}]
    study.apply_bh_by_cohort(cfgs)
    assert cfgs[0]["bh_reject"] is False


def test_binanceus_symbols_keep_their_plain_identity():
    assert study.qualified_symbol("binanceus", "BTC/USDT") == "BTC/USDT"
    assert study.qualified_symbol("bitstamp", "BTC/USD") == "BTC/USD@bitstamp"
    assert study.qualified_symbol("coinbaseexchange", "ETH/USD") == \
        "ETH/USD@coinbaseexchange"


def test_two_venues_listing_one_pair_are_distinct_datasets():
    a = study.dataset_key(study.qualified_symbol("bitstamp", "BTC/USD"), "1h")
    b = study.dataset_key(study.qualified_symbol("coinbaseexchange", "BTC/USD"), "1h")
    assert a != b


def test_base_asset_ignores_quote_and_venue():
    for symbol in ("BTC/USDT", "BTC/USD", "BTC/USD@bitstamp",
                   "btc/usd@coinbaseexchange"):
        assert study.base_asset(symbol) == "BTC"


def test_exactly_one_venue_owns_each_asset_window_cell():
    seen = {}
    for (exchange_id, symbol, _tf), windows in study.DATASET_WINDOWS.items():
        for window in windows:
            key = f"{study.base_asset(symbol)}|{window}"
            seen.setdefault(key, set()).add(exchange_id)
    assert all(len(v) == 1 for v in seen.values()), \
        {k: v for k, v in seen.items() if len(v) > 1}


def test_window_ownership_collision_raises():
    clashing = dict(study.DATASET_WINDOWS)
    clashing[("bitstamp", "BTC/USD", "1h")] = ("2013", "2021")
    with pytest.raises(AssertionError) as exc:
        study._assert_window_ownership(clashing)
    assert "ownership collision" in str(exc.value)


def test_every_new_venue_window_is_pre_2020h2():
    floor = pd.Timestamp("2020-07-01")
    for window in set(study.BITSTAMP_WINDOWS) | set(study.COINBASE_WINDOWS):
        assert pd.Timestamp(study.WINDOWS[window][1]) <= floor


def test_new_venue_cells_are_primary_and_1410_cells_stay_exploratory():
    assert study.cell_cohort("bitstamp", "BTC/USD", "1h", "2013") == \
        study.COHORT_PRIMARY
    assert study.cell_cohort("coinbaseexchange", "ETH/USD", "1h", "2018") == \
        study.COHORT_PRIMARY
    for key, window in study.D_1410:
        symbol, timeframe = key.rsplit(" ", 1)
        assert study.cell_cohort("binanceus", symbol, timeframe, window) == \
            study.COHORT_EXPLORATORY


def test_a_resampled_audit_tape_stays_exploratory():
    assert study.cell_cohort("binanceus", "BTC/USDT", "2h", "is") == \
        study.COHORT_EXPLORATORY
    assert study.cell_cohort("binanceus", "BNB/USDT", "1h", "is") == \
        study.COHORT_PRIMARY


def test_cell_cohort_rejects_an_unknown_window():
    with pytest.raises(ValueError):
        study.cell_cohort("binanceus", "BTC/USDT", "1h", "1999")


def test_same_base_asset_is_credited_full_correlation():
    idx = pd.date_range("2021-01-01", periods=3000, freq="1h")
    rng = np.random.default_rng(0)
    frames = {}
    for ds in (("bitstamp", "BTC/USD", "1h"),
               ("binanceus", "BTC/USDT", "1h"),
               ("binanceus", "ETH/USDT", "1h")):
        close = pd.Series(100 + np.cumsum(rng.normal(size=len(idx))), index=idx)
        frames[ds] = pd.DataFrame({"close": close}, index=idx)
    rho = study.symbol_return_correlations(frames)
    assert rho[("BTC/USD@bitstamp", "BTC/USDT")] == 1.0
    measured = rho.get(("BTC/USDT", "ETH/USDT"))
    assert measured is not None and measured != 1.0


def test_full_correlation_survives_into_effective_n():
    rho = {("BTC/USD@bitstamp", "BTC/USDT"): 1.0}
    overlapping = [
        _trade(symbol="BTC/USD@bitstamp", exchange="bitstamp", window="2013", day=0,
               hold_days=10, cohort=study.COHORT_PRIMARY),
        _trade(symbol="BTC/USDT", window="2021", day=0, hold_days=10),
    ]
    assert study.effective_n(overlapping, rho) == pytest.approx(1.0, abs=1e-6)


def test_coverage_audit_never_reports_an_unowned_cell_as_dropped():
    idx = pd.date_range("2013-01-01", periods=50, freq="1h")
    frames = {("bitstamp", "BTC/USD", "1h"):
              pd.DataFrame({"close": np.arange(50.0)}, index=idx)}
    cov = study.coverage_audit(frames, ["2013", "2021"], [128])
    assert cov["n_unowned"] == 1
    assert all(d["window"] == "2013" for d in cov["dropped"])
    assert "BTC/USD@bitstamp 1h|2021" not in cov["cells"]


def test_fetch_page_limits_match_the_recorded_probes():
    assert study.FETCH_PAGE_LIMIT["coinbaseexchange"] == 300
    assert study.FETCH_PAGE_LIMIT["bitstamp"] == 1000
    assert study.FETCH_PAGE_LIMIT["binanceus"] == 500


def test_each_dataset_carries_its_own_measured_history_floor():
    assert study.history_since_for(("coinbaseexchange", "ETH/USD", "1h")) == \
        "2016-06-01"
    assert study.history_since_for(("coinbaseexchange", "LTC/USD", "1h")) == \
        "2016-09-01"
    assert study.history_since_for(("bitstamp", "BTC/USD", "4h")) == \
        study.HISTORY_SINCE["bitstamp"]
    assert study.history_since_for(("binanceus", "BTC/USDT", "1h")) == \
        study1422.HISTORY_SINCE


def test_every_new_venue_floor_precedes_its_earliest_owned_window():
    for dataset, windows in study.DATASET_WINDOWS.items():
        if dataset[0] == study.PLATFORM:
            continue
        first = min(pd.Timestamp(study.WINDOWS[w][0]) for w in windows)
        floor = pd.Timestamp(study.history_since_for(dataset))
        latest = max(pd.Timestamp(study.WINDOWS[w][0]) for w in windows)
        assert floor < latest, (dataset, floor, first)


def test_empty_backfill_is_reported_as_not_ok(monkeypatch, capsys):
    import data_fetcher

    monkeypatch.setattr(data_fetcher, "fetch_full_history",
                        lambda *a, **kw: pd.DataFrame())
    report = study.ensure_min_history([("coinbaseexchange", "ETH/USD", "1h")])
    entry = report["ETH/USD@coinbaseexchange 1h"]
    assert entry["ok"] is False and entry["bars"] == 0
    assert "NO BARS" in capsys.readouterr().out


def test_infeasible_routes_are_recorded_not_omitted():
    sources = {p["source"]: p["verdict"] for p in study.FEASIBILITY_PROBES}
    assert any("topstep" in s.lower() for s in sources)
    assert any("ibkr" in s.lower() for s in sources)
    assert {v for v in sources.values()} == {"USED", "INFEASIBLE"}


def test_horizon_is_fixed_in_calendar_time_across_timeframes():
    assert study.horizon_bars("1h") * 60 == study.HORIZON_HOURS * 60
    assert study.horizon_bars("4h") * 240 == study.HORIZON_HOURS * 60
    assert study.horizon_bars("2h") * 120 == study.HORIZON_HOURS * 60


def test_horizon_rejects_a_timeframe_coarser_than_the_horizon():
    with pytest.raises(ValueError):
        study.horizon_bars("1w")


def test_efficiency_is_one_on_a_monotone_advance_and_signed_by_direction():
    closes = np.array([100.0, 101.0, 102.0, 103.0, 104.0])
    assert study.signed_efficiency(closes, 0, 4, 1) == pytest.approx(1.0, abs=1e-9)
    assert study.signed_efficiency(closes, 0, 4, -1) == pytest.approx(-1.0, abs=1e-9)


def test_efficiency_is_zero_on_a_round_trip():
    closes = np.array([100.0, 105.0, 100.0, 105.0, 100.0])
    assert study.signed_efficiency(closes, 0, 4, 1) == pytest.approx(0.0, abs=1e-9)


def test_efficiency_is_bounded_by_plus_minus_one():
    rng = np.random.default_rng(1424)
    closes = 100 + np.cumsum(rng.normal(size=500))
    for pos in range(0, 400, 17):
        for direction in (1, -1):
            value = study.signed_efficiency(closes, pos, 96, direction)
            if value is not None:
                assert -1.0 <= value <= 1.0


def test_efficiency_returns_none_past_the_slice_end_rather_than_truncating():
    closes = np.array([100.0, 101.0, 102.0])
    assert study.signed_efficiency(closes, 1, 4, 1) is None
    assert study.signed_efficiency(closes, 0, 2, 1) is not None
    assert study.signed_efficiency(closes, 1, 2, 1) is None


def test_efficiency_survives_a_dead_flat_span_without_dividing_by_zero():
    closes = np.zeros(10) + 42.0
    assert study.signed_efficiency(closes, 0, 5, 1) == 0.0


def test_efficiency_rejects_a_non_positive_horizon():
    with pytest.raises(ValueError):
        study.signed_efficiency(np.arange(10.0), 0, 0, 1)


def test_trade_direction_raises_on_an_unknown_side():
    assert study.trade_direction("long") == 1
    assert study.trade_direction("SHORT") == -1
    for bad in (None, "", "flat", 0):
        with pytest.raises(ValueError):
            study.trade_direction(bad)


def test_target_rows_drop_horizon_truncated_trades_and_count_them():
    rows = [_trade(day=0, eff=0.2), _trade(day=1, eff=None), _trade(day=2, eff=-0.1)]
    kept, missing = study._target_rows(rows)
    assert missing == 1
    assert [r["efficiency"] for r in kept] == [0.2, -0.1]


def test_target_filter_runs_before_the_cluster_usability_filter():
    rows = [_trade(symbol="XRP/USDT", day=0, eff=0.1),
            _trade(symbol="XRP/USDT", day=400, eff=None)]
    kept, _ = study._target_rows(rows)
    idx, excluded = study.usable_cluster_rows(kept)
    assert excluded == ["XRP/USDT 1h"]
    assert idx == []


def test_bucket_table_reports_the_primary_target_beside_net_return():
    trades = [_trade(day=i, pnl=1.0, eff=0.4, h=0.7) for i in range(3)]
    trades += [_trade(day=10 + i, pnl=-1.0, eff=-0.2, h=0.3, ) for i in range(2)]
    table = study.bucket_tables(trades, 512)
    assert table[">=0.55"]["mean_efficiency"] == pytest.approx(0.4)
    assert table[">=0.55"]["efficiency_trades"] == 3
    assert table["<0.45"]["mean_efficiency"] == pytest.approx(-0.2)


def test_bucket_table_counts_target_rows_separately_from_trades():
    trades = [_trade(day=0, h=0.7, eff=0.4), _trade(day=1, h=0.7, eff=None)]
    table = study.bucket_tables(trades, 512)
    assert table[">=0.55"]["trades"] == 2
    assert table[">=0.55"]["efficiency_trades"] == 1
    assert table[">=0.55"]["mean_efficiency"] == pytest.approx(0.4)


def test_nan_h_still_gets_its_own_bucket():
    trades = [_trade(day=0, h=None, eff=0.3)]
    table = study.bucket_tables(trades, 512)
    assert table[study.BUCKET_NAN]["trades"] == 1
    assert table["0.50-0.55"]["trades"] == 0


def _spread_trades(n=60, symbol="BTC/USDT"):
    return [_trade(symbol=symbol, day=i * 5, hold_days=1) for i in range(n)]


def test_mde_raises_when_the_permutation_count_cannot_resolve_the_bar():
    trades = _spread_trades(10)
    values = [0.1] * 10
    mask = [True] * 5 + [False] * 5
    with pytest.raises(ValueError) as exc:
        study.min_detectable_effect_on_grid(
            trades, values, mask, family_size=1, grid_step=0.02, grid_max=0.5,
            refine_step=0.001, cluster=False, n_perm=5)
    assert "cannot resolve" in str(exc.value)


def test_mde_returns_none_when_the_largest_grid_point_cannot_clear_the_bar():
    trades = _spread_trades(8)
    values = [0.0] * 8
    mask = [True] * 4 + [False] * 4
    out = study.min_detectable_effect_on_grid(
        trades, values, mask, family_size=1, grid_step=0.5, grid_max=0.5,
        refine_step=0.5, cluster=False, n_perm=200)
    assert out is None or out <= 0.5


def test_efficiency_and_pp_wrappers_use_their_own_grids():
    calls = []

    def _spy(*args, **kwargs):
        calls.append((kwargs["grid_step"], kwargs["grid_max"],
                      kwargs["refine_step"]))
        return 0.0

    original = study.min_detectable_effect_on_grid
    study.min_detectable_effect_on_grid = _spy
    try:
        study.min_detectable_effect_eff([], [], [], 1)
        study.min_detectable_effect_pp([], [], [], 1)
    finally:
        study.min_detectable_effect_on_grid = original
    assert calls == [
        (study.MDE_EFF_GRID_STEP, study.MDE_EFF_GRID_MAX, study.MDE_EFF_REFINE_STEP),
        (study.MDE_PP_GRID_STEP, study.MDE_PP_GRID_MAX, study.MDE_PP_REFINE_STEP),
    ]


def test_pp_grid_is_1422s_verbatim_so_the_two_studies_stay_comparable():
    assert study.MDE_PP_GRID_STEP == study1422.MDE_GRID_STEP
    assert study.MDE_PP_GRID_MAX == study1422.MDE_GRID_MAX
    assert study.MDE_PP_REFINE_STEP == study1422.MDE_REFINE_STEP


def test_mde_shrinks_as_the_family_denominator_shrinks():
    rng = np.random.default_rng(1424)
    trades = _spread_trades(120)
    values = list(rng.normal(0.0, 0.3, size=120))
    mask = [i % 2 == 0 for i in range(120)]
    one = study.min_detectable_effect_on_grid(
        trades, values, mask, family_size=1, grid_step=0.05, grid_max=1.0,
        refine_step=0.05, cluster=False, n_perm=1000)
    thirty = study.min_detectable_effect_on_grid(
        trades, values, mask, family_size=30, grid_step=0.05, grid_max=1.0,
        refine_step=0.05, cluster=False, n_perm=1000)
    assert one is not None and thirty is not None
    assert one <= thirty


def _win(dd=-1.0, chop=-1.0, ret_g=10.0, ret_u=10.0, legs=1):
    return {"n_legs": legs, "dd_delta": dd, "chop_delta": chop,
            "ret_gated": ret_g, "ret_ungated": ret_u}


def test_protocol_rule_passes_on_three_of_four():
    windows = {"2017": _win(), "2018": _win(), "2021": _win(),
               "2022": _win(dd=+1.0)}
    ok, holding, with_legs, reasons = study.protocol_verdict(
        windows, study.PRIMARY_PROTOCOL_WINDOWS, study.PRIMARY_PROTOCOL_MIN_WINDOWS)
    assert ok and holding == 3 and with_legs == 4 and reasons == []


def test_protocol_rule_fails_on_two_of_four():
    windows = {"2017": _win(), "2018": _win(), "2021": _win(dd=+1.0),
               "2022": _win(chop=+1.0)}
    ok, holding, _with, reasons = study.protocol_verdict(
        windows, study.PRIMARY_PROTOCOL_WINDOWS, study.PRIMARY_PROTOCOL_MIN_WINDOWS)
    assert not ok and holding == 2
    assert any("holds on only" in r for r in reasons)


def test_protocol_rule_fails_closed_when_too_few_windows_carry_legs():
    windows = {"2017": _win(), "2018": _win(legs=0), "2021": _win(legs=0),
               "2022": _win(legs=0)}
    ok, _holding, with_legs, reasons = study.protocol_verdict(
        windows, study.PRIMARY_PROTOCOL_WINDOWS, study.PRIMARY_PROTOCOL_MIN_WINDOWS)
    assert not ok and with_legs == 1
    assert any("carry legs" in r for r in reasons)


def test_protocol_rule_enforces_the_return_give_up_tolerance():
    windows = {"2017": _win(), "2018": _win(), "2021": _win(),
               "2022": _win(ret_g=-100.0, ret_u=100.0)}
    ok, holding, _w, _r = study.protocol_verdict(
        windows, study.PRIMARY_PROTOCOL_WINDOWS, study.PRIMARY_PROTOCOL_MIN_WINDOWS)
    assert ok and holding == 3


def test_exploratory_protocol_rule_still_requires_every_window():
    assert study.EXPLORATORY_PROTOCOL_MIN_WINDOWS == \
        len(study.EXPLORATORY_PROTOCOL_WINDOWS)
    windows = {"is": _win(), "oos": _win(dd=+1.0)}
    ok, _h, _w, _r = study.protocol_verdict(
        windows, study.EXPLORATORY_PROTOCOL_WINDOWS,
        study.EXPLORATORY_PROTOCOL_MIN_WINDOWS)
    assert not ok


def test_primary_protocol_and_held_out_windows_partition_the_window_set():
    assert set(study.PRIMARY_PROTOCOL_WINDOWS).isdisjoint(
        study.PRIMARY_HELD_OUT_WINDOWS)
    assert set(study.PRIMARY_PROTOCOL_WINDOWS) | set(study.PRIMARY_HELD_OUT_WINDOWS) \
        == set(study.WINDOW_ORDER)


def test_primary_protocol_spans_two_bull_and_two_bear_years():
    assert study.PRIMARY_PROTOCOL_WINDOWS == ("2017", "2018", "2021", "2022")
    assert study.PRIMARY_PROTOCOL_MIN_WINDOWS == 3


def _cfg(**over):
    cfg = {
        "config_id": study.PRIMARY_CONFIG_ID,
        "cohort": study.COHORT_PRIMARY,
        "family": "momentum",
        "mode": "gate",
        "sense": study.SENSE_HIGH,
        "hurst_window": 512,
        "arm": 0.52,
        "disarm": 0.48,
        "gain": None,
        "p_raw": 0.01,
        "p_raw_return": 0.01,
        "p_cluster": 0.01,
        "p_cluster_return": 0.01,
        "bh_reject": True,
        "n_pooled_trades": 200,
        "n_pooled_effective": 100.0,
        "n_missing_target": 2,
        "n_suppressed": 80,
        "n_kept": 120,
        "n_suppressed_effective": 50.0,
        "n_kept_effective": 50.0,
        "protocol_windows": list(study.PRIMARY_PROTOCOL_WINDOWS),
        "protocol_min_windows": study.PRIMARY_PROTOCOL_MIN_WINDOWS,
        "held_out_windows": list(study.PRIMARY_HELD_OUT_WINDOWS),
        "windows": {w: _win() for w in study.PRIMARY_PROTOCOL_WINDOWS},
    }
    cfg["windows"].update({w: _win() for w in study.PRIMARY_HELD_OUT_WINDOWS[:3]})
    cfg.update(over)
    return cfg


def test_config_passes_when_every_pre_registered_condition_holds():
    ok, reasons = study.config_verdict(_cfg())
    assert ok, reasons


def test_untestable_cluster_p_fails_closed():
    ok, reasons = study.config_verdict(
        _cfg(p_cluster=None, cluster_reason="no draws"))
    assert not ok
    assert any("untestable" in r for r in reasons)


def test_significance_reads_the_primary_target_not_net_return():
    ok, reasons = study.config_verdict(
        _cfg(bh_reject=False, p_cluster=0.4, p_cluster_return=0.0001))
    assert not ok
    assert any("primary target" in r for r in reasons)


def test_effective_volume_floors_bind_not_nominal_counts():
    ok, reasons = study.config_verdict(
        _cfg(n_suppressed_effective=5.0, n_suppressed=5000))
    assert not ok
    assert any("effective suppressed" in r for r in reasons)


def test_economics_still_read_net_return():
    windows = {w: _win() for w in study.PRIMARY_PROTOCOL_WINDOWS}
    windows["2017"] = _win(dd=+5.0)
    windows["2018"] = _win(dd=+5.0)
    windows.update({w: _win() for w in study.PRIMARY_HELD_OUT_WINDOWS[:3]})
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok
    assert any("drawdown not reduced" in r for r in reasons)


_MR = "mean_reversion"


def _mde(mom_limit=0.05, mom_sep=0.09, mr_limit=0.05, mr_sep=0.02,
         pooled=None, **extra):
    out = {
        "by_family_cluster": {study.PRIMARY_FAMILY: mom_limit, _MR: mr_limit},
        "by_family_separation": {study.PRIMARY_FAMILY: mom_sep, _MR: mr_sep},
        "by_family_n": {study.PRIMARY_FAMILY: 100, _MR: 100},
        "pooled_primary_cluster": (0.001 if pooled is None else pooled),
        "observed_separation_by_pool": {
            "primary": {f"{study.PRIMARY_FAMILY}|512": mom_sep,
                        f"{_MR}|512": mr_sep}},
    }
    out.update(extra)
    return out


def test_validity_gate_passes_when_the_limit_sits_below_the_separation():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.09))
    assert gate["passed"] is True
    assert gate["limit"] == pytest.approx(0.05)
    assert gate["largest_separation"] == pytest.approx(0.09)
    assert gate["family"] == study.PRIMARY_FAMILY


def test_validity_gate_fails_when_the_separation_sits_under_the_limit():
    assert study.validity_gate(_mde(mom_limit=0.20, mom_sep=0.09))["passed"] \
        is False


def test_validity_gate_reads_the_confirmatory_familys_own_rows_not_the_pool():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.009,
                                    mr_limit=0.05, mr_sep=0.008, pooled=0.004))
    assert gate["passed"] is False
    assert gate["limit"] == pytest.approx(0.05)


def test_validity_gate_is_unchanged_when_the_pool_holds_one_family():
    both = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.09))
    alone = study.validity_gate({
        "by_family_cluster": {study.PRIMARY_FAMILY: 0.05},
        "by_family_separation": {study.PRIMARY_FAMILY: 0.09},
        "by_family_n": {study.PRIMARY_FAMILY: 100},
        "pooled_primary_cluster": 0.05,
        "observed_separation_by_pool": {
            "primary": {f"{study.PRIMARY_FAMILY}|512": 0.09}},
    })
    assert alone["passed"] == both["passed"] is True
    assert alone["limit"] == both["limit"]
    assert alone["largest_separation"] == both["largest_separation"]


def test_validity_gate_never_borrows_the_other_familys_limit():
    gate = study.validity_gate(_mde(mom_limit=None, mom_sep=0.09,
                                    mr_limit=0.001, mr_sep=0.5))
    assert gate["passed"] is False
    assert gate["limit"] is None
    assert "detection limit above" in gate["reason"]


def test_validity_gate_refuses_a_separation_pointing_the_untested_way():
    gate = study.validity_gate(_mde(mom_limit=0.01, mom_sep=-0.30))
    assert gate["passed"] is False
    assert gate["largest_separation"] == pytest.approx(-0.30)
    assert "OPPOSITE" in gate["reason"]


def test_validity_gate_refuses_when_every_family_points_the_untested_way():
    gate = study.validity_gate(_mde(mom_limit=0.005, mom_sep=-0.05,
                                    mr_limit=0.005, mr_sep=-0.08))
    assert gate["passed"] is False
    assert "OPPOSITE" in gate["reason"]


def test_validity_gate_decides_on_direction_not_the_larger_magnitude():
    gate = study.validity_gate(_mde(mom_limit=0.02, mom_sep=0.05,
                                    mr_limit=0.02, mr_sep=-0.40))
    assert gate["passed"] is True
    assert gate["largest_separation"] == pytest.approx(0.05)


def test_validity_gate_fails_closed_on_an_unreachable_limit():
    gate = study.validity_gate(_mde(mom_limit=None, mom_sep=0.09))
    assert gate["passed"] is False
    assert "detection limit above" in gate["reason"]


def test_validity_gate_fails_closed_with_no_separation_at_all():
    gate = study.validity_gate({"by_family_cluster": {study.PRIMARY_FAMILY: 0.05},
                                "by_family_separation": {}})
    assert gate["passed"] is False
    assert "no measurable separation" in gate["reason"]


def test_gate_is_never_read_against_a_mismatched_pool():
    mde = _mde(mom_limit=0.20, mom_sep=0.09)
    mde["observed_separation_by_pool"]["exploratory"] = {
        f"{study.PRIMARY_FAMILY}|512": 0.90}
    assert study.validity_gate(mde)["passed"] is False


def test_a_reversed_separation_never_closes_the_question():
    decision = study.decide_recommendation(
        [], _mde(mom_limit=0.001, mom_sep=-0.40))
    assert decision["verdict"] == study.VERDICT_INCONCLUSIVE
    assert decision["key_risk_held"] is False
    assert "closes the question" not in decision["justification"]
    assert "OPPOSITE" in decision["justification"]


_PASSING_MDE = _mde(mom_limit=0.05, mom_sep=0.09)
_FAILING_MDE = _mde(mom_limit=0.20, mom_sep=0.09)


def test_a_resolved_null_is_named_differently_from_an_underpowered_one():
    resolved = study.decide_recommendation([], _PASSING_MDE)
    under = study.decide_recommendation([], _FAILING_MDE)
    assert resolved["verdict"] == study.VERDICT_RESOLVED_NULL
    assert under["verdict"] == study.VERDICT_INCONCLUSIVE
    assert resolved["key_risk_held"] is True
    assert under["key_risk_held"] is False


def test_a_resolved_null_says_it_closes_the_question():
    decision = study.decide_recommendation([], _PASSING_MDE)
    assert "NO usable Hurst edge at or above this limit" in decision["justification"]


def test_an_underpowered_null_stays_a_power_statement():
    decision = study.decide_recommendation([], _FAILING_MDE)
    assert "stays a POWER statement" in decision["justification"]
    assert "no edge exists" in decision["justification"]


def test_only_a_primary_config_can_win():
    exploratory = _cfg(cohort=study.COHORT_EXPLORATORY)
    decision = study.decide_recommendation([exploratory], _PASSING_MDE)
    assert decision["verdict"] != study.VERDICT_CONFIG
    assert all(v["winner"] is None for v in decision["families"].values())


def test_a_passing_primary_config_produces_a_recommendation():
    decision = study.decide_recommendation([_cfg()], _PASSING_MDE)
    assert decision["verdict"] == study.VERDICT_CONFIG
    assert decision["families"]["momentum"]["winner"]["config_id"] == \
        study.PRIMARY_CONFIG_ID
    assert decision["families"]["mean_reversion"]["n_tested"] == 0


def _render_payload(decision=None, mde=None, configs=None):
    mde = dict(mde or _FAILING_MDE)
    mde.setdefault("observed_separation_pp_by_pool",
                   {"primary": {"momentum|512": 0.4}})
    mde.setdefault("by_family_cluster_return",
                   {study.PRIMARY_FAMILY: 0.9, _MR: 0.5})
    mde.setdefault("by_family_separation_return",
                   {study.PRIMARY_FAMILY: 0.4, _MR: -0.2})
    cfgs = list(configs if configs is not None else [_cfg(bh_reject=False,
                                                          p_cluster=0.4)])
    decision = decision or study.decide_recommendation(cfgs, mde)
    return {
        "schema_version": study.SCHEMA_VERSION,
        "issue": study.ISSUE,
        "pre_registered": {
            "hurst_windows": [512],
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
            "warmup": {"required_bars": 522, "min_lead_bars": 900,
                       "sufficient": True, "insufficient_datasets": [],
                       "lead_bars": {}},
            "coverage": {"n_kept": 1, "n_cells": 1, "n_dropped": 0,
                         "n_unowned": 3, "required_lead_bars": 522,
                         "min_window_bar_fraction": 0.8,
                         "reference_last_bar": "2026-01-01", "dropped": []},
            "symbol_correlations": {},
            "elapsed_sec": 1.0,
        },
        "mde": mde,
        "buckets": {f: {"512": study.bucket_tables([], 512)}
                    for f in study.FAMILIES},
        "joint": {f: {"table": {}, "verdict": {"separated": False,
                                               "reason": "test"}}
                  for f in study.FAMILIES},
        "configs": cfgs,
        "legs": [],
        "decision": decision,
    }


def test_report_renders_and_ends_with_the_recommendation():
    text = study.report_from_payload(_render_payload())
    body, _, tail = text.rpartition("## Recommendation")
    assert body and "## Recommendation" not in tail


def test_report_states_the_interim_look_and_the_key_risk_verbatim():
    text = study.report_from_payload(_render_payload())
    assert study.INTERIM_LOOK_DISCLOSURE in text
    assert study.KEY_RISK_PREDICTION in text


def test_report_prints_the_validity_gate_outcome_both_ways():
    failing = study.report_from_payload(_render_payload())
    assert "Validity gate: **FAILED**" in failing
    assert "INCONCLUSIVE" in failing
    passing = study.report_from_payload(_render_payload(mde=_PASSING_MDE))
    assert "Validity gate: **PASSED**" in passing
    assert "NO USABLE HURST EDGE AT OR ABOVE THE MEASURED LIMIT" in passing


def test_report_pairs_each_pools_limit_with_its_own_separation():
    text = study.report_from_payload(_render_payload())
    assert "Largest separation ON THAT POOL" in text
    assert "Resolvable?" in text


def test_equal_magnitudes_of_opposite_sign_render_differently():
    assert study._fmt_signed(0.005, 3) != study._fmt_signed(-0.005, 3)
    assert study._fmt_signed(0.005, 3) == "+0.005"
    assert study._fmt_signed(-0.005, 3) == "-0.005"


def test_a_missing_separation_renders_as_a_dash_never_as_zero():
    assert study._fmt_signed(None, 3) == "-"
    assert study._fmt_signed(float("nan"), 3) == "-"
    assert study._fmt_signed(0.0, 3) == "+0.000"


def test_largest_signed_prefers_the_tested_direction():
    assert study._largest_signed({"a|512": 0.01, "b|512": -0.30}) == \
        pytest.approx(0.01)
    assert study._largest_signed({"a|512": -0.05, "b|512": -0.30}) == \
        pytest.approx(-0.05)
    assert study._largest_signed({}) is None
    assert study._largest_signed({"a|512": None}) is None


def test_the_pool_tables_print_every_family_with_its_sign():
    mde = _mde(mom_limit=0.20, mom_sep=0.009, mr_limit=0.20, mr_sep=-0.024)
    text = study.report_from_payload(_render_payload(mde=mde))
    assert "By family (signed)" in text
    assert "momentum +0.009" in text
    assert "mean_reversion -0.024" in text


def test_the_pool_tables_flag_a_reversed_pool_rather_than_resolving_it():
    mde = _mde(mom_limit=0.20, mom_sep=-0.05, mr_limit=0.20, mr_sep=-0.08,
               pooled=0.001)
    text = study.report_from_payload(_render_payload(mde=mde))
    assert "n/a (reversed)" in text


def test_the_key_risk_paragraph_matches_the_reason_the_gate_failed():
    reversed_text = study.report_from_payload(
        _render_payload(mde=_mde(mom_limit=0.01, mom_sep=-0.30)))
    assert "points the OTHER WAY" in reversed_text
    assert "would have been caught, and none was" not in reversed_text
    small_text = study.report_from_payload(
        _render_payload(mde=_mde(mom_limit=0.20, mom_sep=0.09)))
    assert "would have been caught, and none was" in small_text
    assert "points the OTHER WAY" not in small_text


def test_the_report_names_the_two_numbers_the_gate_actually_reads():
    text = study.report_from_payload(_render_payload())
    assert "The two numbers the validity gate actually reads" in text
    assert "Reads the gate?" in text
    assert "this study, primary cohort" in text


def test_report_records_the_feasibility_probes():
    text = study.report_from_payload(_render_payload())
    assert "Route 2 — data feasibility, recorded" in text
    for probe in study.FEASIBILITY_PROBES:
        assert probe["source"] in text


def test_report_declares_ownership_of_the_contract_path():
    text = study.report_from_payload(_render_payload())
    assert "LIVE-EVIDENCE CONTRACT PATH" in text
    assert "hurst_1422_gate_power.md" in text


def test_report_never_licenses_a_threshold_on_a_null():
    text = study.report_from_payload(_render_payload())
    assert "stays DEFAULT-OFF with no recommended thresholds" in text


def test_render_only_rehydrates_a_winner_stored_as_an_id():
    winner = _cfg()
    decision = study.decide_recommendation([winner], _PASSING_MDE)
    live = study.render_recommendation(decision, _PASSING_MDE, [winner])
    round_tripped = json.loads(json.dumps({
        "verdict": decision["verdict"],
        "justification": decision["justification"],
        "validity_gate": decision["validity_gate"],
        "key_risk_held": decision["key_risk_held"],
        "families": {f: {"n_tested": d["n_tested"], "n_passing": d["n_passing"],
                         "winner": (d["winner"] or {}).get("config_id")}
                     for f, d in decision["families"].items()},
    }))
    assert study.render_recommendation(round_tripped, _PASSING_MDE, [winner]) == live


def test_render_only_raises_when_a_named_winner_is_absent():
    with pytest.raises(AssertionError):
        study._resolve_winner("momentum/gate/W512/arm0.52/dis0.48", [])


def test_the_committed_decision_is_what_the_current_rule_produces():
    with open(study._DEFAULT_JSON_OUT) as fh:
        payload = json.load(fh)
    fresh = study.decision_payload(
        study.decide_recommendation(payload["configs"], payload["mde"]))
    assert payload["decision"] == fresh


def test_the_committed_run_reports_a_reversed_confirmatory_separation():
    with open(study._DEFAULT_JSON_OUT) as fh:
        payload = json.load(fh)
    gate = payload["decision"]["validity_gate"]
    assert gate["family"] == study.PRIMARY_FAMILY
    assert gate["passed"] is False
    assert gate["largest_separation"] < 0
    assert payload["decision"]["verdict"] == study.VERDICT_INCONCLUSIVE
    assert payload["decision"]["key_risk_held"] is False


def test_1424_owns_the_contract_path():
    assert os.path.basename(study._DEFAULT_REPORT_OUT) == \
        "hurst_gate_calibration.md"
    assert os.path.basename(study._DEFAULT_JSON_OUT) == \
        "hurst_1424_gate_resolution.json"


def test_no_predecessor_study_still_defaults_to_the_contract_path():
    for module in (study1410, study1422):
        assert os.path.basename(module._DEFAULT_REPORT_OUT) != \
            "hurst_gate_calibration.md"
    assert os.path.basename(study1422._DEFAULT_REPORT_OUT) == \
        "hurst_1422_gate_power.md"


def test_1422_may_not_write_the_contract_path_even_when_asked(tmp_path):
    contract = os.path.join(os.path.dirname(study._DEFAULT_REPORT_OUT),
                            "hurst_gate_calibration.md")
    with pytest.raises(SystemExit) as exc:
        study1422.main(["--report-out", contract,
                        "--json-out", str(tmp_path / "scoped.json")])
    assert "SUPERSEDED" in str(exc.value)


def test_scoped_run_may_not_overwrite_the_committed_json():
    with pytest.raises(SystemExit) as exc:
        study.main(["--only", "momentum"])
    assert "committed aggregate" in str(exc.value)


@pytest.mark.parametrize("flag,value", [("--only", "momentum"),
                                        ("--datasets", "BTC/USDT:1h"),
                                        ("--windows", "2017"),
                                        ("--hurst-windows", "128")])
def test_every_scoping_flag_protects_the_contract_report(tmp_path, flag, value):
    with pytest.raises(SystemExit) as exc:
        study.main([flag, value, "--json-out", str(tmp_path / "scoped.json")])
    assert "contract path" in str(exc.value)


@pytest.mark.parametrize("argv,needle", [
    (["--n-perm-mde", "200"], "--n-perm-mde 200"),
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
    assert "contract path" in str(exc.value)


class _Args:
    def __init__(self, **kw):
        self.n_perm = kw.get("n_perm", study.N_PERM)
        self.n_perm_mde = kw.get("n_perm_mde", study.N_PERM_MDE)
        self.seed = kw.get("seed", study.SEED)
        self.no_mirror_check = kw.get("no_mirror_check", False)


def test_stating_the_pre_registered_settings_explicitly_is_not_a_deviation():
    assert study.inference_deviations(_Args()) == []
    assert study.inference_deviations(
        _Args(seed=study.SEED, n_perm=study.N_PERM,
              n_perm_mde=study.N_PERM_MDE)) == []


@pytest.mark.parametrize("kw,needle", [
    ({"n_perm_mde": study.N_PERM_MDE - 1}, "--n-perm-mde"),
    ({"n_perm_mde": study.N_PERM_MDE + 1}, "--n-perm-mde"),
    ({"n_perm": 200}, "--n-perm "),
    ({"seed": study.SEED + 1}, "--seed"),
    ({"no_mirror_check": True}, "--no-mirror-check"),
])
def test_every_inference_deviation_is_named(kw, needle):
    found = study.inference_deviations(_Args(**kw))
    assert len(found) == 1
    assert needle in found[0]


def test_render_only_refuses_a_payload_not_stamped_pre_registered(tmp_path):
    payload = _render_payload()
    payload["run_summary"]["scope"] = {"complete": True}
    path = tmp_path / "deviating.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path), "--write-report"])
    assert "pre-registered inference" in str(exc.value)


def test_render_only_refuses_an_unstamped_payload_on_the_contract_path(tmp_path):
    payload = _render_payload()
    payload["run_summary"]["scope"] = {}
    path = tmp_path / "unstamped.json"
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path), "--write-report"])
    assert "not stamped as a complete run" in str(exc.value)


def test_render_only_to_the_contract_path_needs_write_report(tmp_path):
    path = tmp_path / "complete.json"
    path.write_text(json.dumps(_render_payload()))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(path)])
    assert "needs --write-report" in str(exc.value)


def test_fetch_only_may_be_scoped_to_the_venues_that_need_it(monkeypatch):
    seen = {}

    def _fake(datasets):
        seen["datasets"] = list(datasets)
        return {}

    monkeypatch.setattr(study, "ensure_min_history", _fake)
    assert study.main(["--fetch-only", "--datasets", "bitstamp=BTC/USD:1h"]) == 0
    assert seen["datasets"] == [("bitstamp", "BTC/USD", "1h")]


def test_dataset_arg_defaults_to_the_binanceus_venue():
    assert study._parse_datasets("BTC/USDT:1h") == [("binanceus", "BTC/USDT", "1h")]
    assert study._parse_datasets("coinbaseexchange=ETH/USD:1h") == \
        [("coinbaseexchange", "ETH/USD", "1h")]


def test_the_estimator_is_the_1409_ssot_and_is_never_reimplemented():
    assert study.rolling_hurst is study1410.rolling_hurst
    source = open(study.__file__).read()
    assert "def hurst_exponent" not in source


def test_the_cluster_null_and_effective_n_come_from_1422_unchanged():
    assert study.cluster_permutation_pvalue_group_diff is \
        study1422.cluster_permutation_pvalue_group_diff
    assert study.effective_n is study1422.effective_n
    assert study.usable_cluster_rows is study1422.usable_cluster_rows


def test_the_1422_windows_are_reused_rather_than_redefined():
    for name, span in study1422.WINDOWS.items():
        assert study.WINDOWS[name] == span


def test_stage_0_is_scored_on_net_return_so_it_stays_comparable(monkeypatch):
    sentinel = object()
    monkeypatch.setattr(study1422, "joint_separation_verdict", lambda *a, **k: sentinel)
    assert study.joint_separation_verdict([], 512) is sentinel


def test_look_ahead_shifts_are_the_inherited_ones():
    series = pd.Series(np.arange(10.0))
    assert study.decision_series(series).equals(series.shift(1))
    assert study.entry_stamp_series(series).equals(series.shift(2))
