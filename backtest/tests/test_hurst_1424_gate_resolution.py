"""#1424: pure helpers of the Hurst gate RESOLUTION study.

Covers the three routes that lower the detection limit and the machinery that
decides what the result MEANS: the single-hypothesis primary family and its
Benjamini-Hochberg denominator, the venue-qualified dataset identity and the
window-ownership matrix that stops a calendar span being counted twice, the
signed fixed-horizon efficiency target, the 3-of-4 protocol rule, the validity
gate, and the report/contract-path protections. The EMPIRICAL result of the
study is never asserted here — only the machinery that turns numbers into a
verdict.

Imported the same way the #1410 and #1422 test modules import theirs (explicit
research/ on sys.path, unambiguous module name — safe under the #1304
`-n auto` parallel run).
"""
import json
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import hurst_1424_gate_resolution as study  # noqa: E402
import hurst_1422_gate_power as study1422  # noqa: E402
import hurst_1410_gate_calibration as study1410  # noqa: E402

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


# ---------------------------------------------------------------------------
# Route 1 — the single pre-registered primary hypothesis.
# ---------------------------------------------------------------------------

def test_primary_hypothesis_is_the_committed_1410_argmin():
    # The pre-registration is only meaningful while the pinned id and the
    # committed evidence agree. A regenerated #1410 JSON that moves the argmin
    # must fail loud, never swap the confirmatory hypothesis silently.
    assert study.resolve_primary_config_id(study._JSON_1410) == \
        study.PRIMARY_CONFIG_ID


def test_primary_pick_never_reads_the_1422_outcomes(tmp_path):
    # Selection hygiene: the pick must come from #1410's evidence alone,
    # because the primary cohort here INCLUDES cells #1422 scored.
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
    # Route 1's whole dividend: the rank-1 bar is alpha, not alpha/4.
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
    # Route 1's stated cost, asserted rather than trusted to prose.
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
    # alpha/1 = 0.05 clears 0.04; alpha/30 = 0.00167 does not.
    assert cfgs[0]["bh_reject"] is True
    assert cfgs[1]["bh_reject"] is False


def test_bh_reject_is_reset_not_merely_set():
    cfgs = [{"cohort": study.COHORT_PRIMARY, "p_cluster": 0.9, "bh_reject": True}]
    study.apply_bh_by_cohort(cfgs)
    assert cfgs[0]["bh_reject"] is False


# ---------------------------------------------------------------------------
# Route 2 — venue identity, window ownership, correlation.
# ---------------------------------------------------------------------------

def test_binanceus_symbols_keep_their_plain_identity():
    # Every inherited comparison (AUDIT_SYMBOLS, D_1410, #1422's cohort rule)
    # keys on the plain symbol, so qualifying it would silently reclassify the
    # whole inherited pool.
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
    # Import already asserted this; re-assert on the built matrix so a future
    # dataset addition cannot pass by editing the table and the assert together.
    seen = {}
    for (exchange_id, symbol, _tf), windows in study.DATASET_WINDOWS.items():
        for window in windows:
            key = f"{study.base_asset(symbol)}|{window}"
            seen.setdefault(key, set()).add(exchange_id)
    assert all(len(v) == 1 for v in seen.values()), \
        {k: v for k, v in seen.items() if len(v) > 1}


def test_window_ownership_collision_raises():
    clashing = dict(study.DATASET_WINDOWS)
    clashing[("bitstamp", "BTC/USD", "1h")] = ("2013", "2021")   # binanceus owns 2021
    with pytest.raises(AssertionError) as exc:
        study._assert_window_ownership(clashing)
    assert "ownership collision" in str(exc.value)


def test_every_new_venue_window_is_pre_2020h2():
    # This is what makes "new venue implies primary cohort" sound.
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
    # BTC 2h over a #1410 window is the same tape at a different granularity.
    assert study.cell_cohort("binanceus", "BTC/USDT", "2h", "is") == \
        study.COHORT_EXPLORATORY
    # A symbol #1410 never scored is primary in the same window.
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
    # Two DIFFERENT assets get their measured correlation, not a forced 1.0.
    measured = rho.get(("BTC/USDT", "ETH/USDT"))
    assert measured is not None and measured != 1.0


def test_full_correlation_survives_into_effective_n():
    # The point of the rho override: BTC on two venues must never read as two
    # independent markets when their trades overlap.
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
    assert cov["n_unowned"] == 1                       # bitstamp does not own 2021
    assert all(d["window"] == "2013" for d in cov["dropped"])
    assert "BTC/USD@bitstamp 1h|2021" not in cov["cells"]


def test_fetch_page_limits_match_the_recorded_probes():
    # Coinbase rejects anything above 300 per granularity request; passing 500
    # there truncates a backfill silently.
    assert study.FETCH_PAGE_LIMIT["coinbaseexchange"] == 300
    assert study.FETCH_PAGE_LIMIT["bitstamp"] == 1000
    assert study.FETCH_PAGE_LIMIT["binanceus"] == 500


def test_each_dataset_carries_its_own_measured_history_floor():
    # A venue-wide floor earlier than the latest-listed pair backfills NOTHING
    # for that pair: the fetch stops on the first empty page. The floors are
    # per dataset for exactly that reason.
    assert study.history_since_for(("coinbaseexchange", "ETH/USD", "1h")) == \
        "2016-06-01"
    assert study.history_since_for(("coinbaseexchange", "LTC/USD", "1h")) == \
        "2016-09-01"
    # A dataset with no override falls back to its venue default.
    assert study.history_since_for(("bitstamp", "BTC/USD", "4h")) == \
        study.HISTORY_SINCE["bitstamp"]
    assert study.history_since_for(("binanceus", "BTC/USDT", "1h")) == \
        study1422.HISTORY_SINCE


def test_every_new_venue_floor_precedes_its_earliest_owned_window():
    # A floor at or after the first owned window start leaves no warm-up lead,
    # so that cell could only ever be dropped.
    for dataset, windows in study.DATASET_WINDOWS.items():
        if dataset[0] == study.PLATFORM:
            continue
        first = min(pd.Timestamp(study.WINDOWS[w][0]) for w in windows)
        floor = pd.Timestamp(study.history_since_for(dataset))
        # ETH and LTC list mid-2016 and lose their `2016` cell by design; every
        # dataset must still reach SOME owned window with lead to spare.
        latest = max(pd.Timestamp(study.WINDOWS[w][0]) for w in windows)
        assert floor < latest, (dataset, floor, first)


def test_empty_backfill_is_reported_as_not_ok(monkeypatch, capsys):
    import data_fetcher  # noqa: WPS433 - resolved through the study's sys.path

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


# ---------------------------------------------------------------------------
# Route 3 — the signed fixed-horizon efficiency target.
# ---------------------------------------------------------------------------

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
    # Exactly enough bars is fine; one short is not.
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
    # A dataset whose only long-span rows are horizon-truncated must not be
    # judged rotatable on rows the contrast never scores.
    rows = [_trade(symbol="XRP/USDT", day=0, eff=0.1),
            _trade(symbol="XRP/USDT", day=400, eff=None)]
    kept, _ = study._target_rows(rows)
    idx, excluded = study.usable_cluster_rows(kept)
    assert excluded == ["XRP/USDT 1h"]      # 0-day span once truncation is applied
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


# ---------------------------------------------------------------------------
# The detection-limit estimator on a caller-supplied grid.
# ---------------------------------------------------------------------------

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
    # Route 1 has to actually pay: the same rows must resolve a SMALLER effect
    # under a denominator of 1 than under one of 30.
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


# ---------------------------------------------------------------------------
# Economics — the 3-of-4 protocol rule and the unchanged held-out rule.
# ---------------------------------------------------------------------------

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
    assert ok and holding == 3     # the give-up window is the one that fails


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


# ---------------------------------------------------------------------------
# config_verdict — significance on the primary target, economics on net return.
# ---------------------------------------------------------------------------

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
    # A config significant on net return but NOT on the primary target must
    # fail — the verdict's statistic is pre-registered and is the bounded one.
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
    # Route 3 buys significance on a bounded statistic; it never buys the money.
    windows = {w: _win() for w in study.PRIMARY_PROTOCOL_WINDOWS}
    windows["2017"] = _win(dd=+5.0)
    windows["2018"] = _win(dd=+5.0)
    windows.update({w: _win() for w in study.PRIMARY_HELD_OUT_WINDOWS[:3]})
    ok, reasons = study.config_verdict(_cfg(windows=windows))
    assert not ok
    assert any("drawdown not reduced" in r for r in reasons)


# ---------------------------------------------------------------------------
# The validity gate — the mechanism that decides what a null MEANS.
# ---------------------------------------------------------------------------

def test_validity_gate_passes_when_the_limit_sits_below_the_separation():
    gate = study.validity_gate({
        "pooled_primary_cluster": 0.05,
        "observed_separation_by_pool": {"primary": {"momentum|512": 0.09}},
    })
    assert gate["passed"] is True
    assert gate["limit"] == pytest.approx(0.05)
    assert gate["largest_separation"] == pytest.approx(0.09)


def test_validity_gate_fails_when_the_separation_sits_under_the_limit():
    gate = study.validity_gate({
        "pooled_primary_cluster": 0.20,
        "observed_separation_by_pool": {"primary": {"momentum|512": 0.09}},
    })
    assert gate["passed"] is False


def test_validity_gate_reads_the_magnitude_of_a_negative_separation():
    # `mean_reversion` separates in the opposite direction by construction;
    # its magnitude is what the limit has to beat.
    gate = study.validity_gate({
        "pooled_primary_cluster": 0.05,
        "observed_separation_by_pool": {
            "primary": {"momentum|512": 0.01, "mean_reversion|512": -0.30}},
    })
    assert gate["passed"] is True
    assert gate["largest_separation"] == pytest.approx(0.30)


def test_validity_gate_fails_closed_on_an_unreachable_limit():
    gate = study.validity_gate({
        "pooled_primary_cluster": None,
        "observed_separation_by_pool": {"primary": {"momentum|512": 0.09}},
    })
    assert gate["passed"] is False
    assert "detection limit is above" in gate["reason"]


def test_validity_gate_fails_closed_with_no_separation_at_all():
    gate = study.validity_gate({"pooled_primary_cluster": 0.05,
                                "observed_separation_by_pool": {}})
    assert gate["passed"] is False


def test_gate_is_never_read_against_a_mismatched_pool():
    # Reading the exploratory pool's larger separation against the primary
    # pool's limit is the exact mistake the pool-matched table exists to stop.
    mde = {
        "pooled_primary_cluster": 0.20,
        "observed_separation_by_pool": {
            "primary": {"momentum|512": 0.09},
            "exploratory": {"momentum|512": 0.90},
        },
    }
    assert study.validity_gate(mde)["passed"] is False


# ---------------------------------------------------------------------------
# decide_recommendation — the verdict word is itself mechanical.
# ---------------------------------------------------------------------------

_PASSING_MDE = {
    "pooled_primary_cluster": 0.05,
    "observed_separation_by_pool": {"primary": {"momentum|512": 0.09}},
}
_FAILING_MDE = {
    "pooled_primary_cluster": 0.20,
    "observed_separation_by_pool": {"primary": {"momentum|512": 0.09}},
}


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


# ---------------------------------------------------------------------------
# Report rendering and the contract-path protections.
# ---------------------------------------------------------------------------

def _render_payload(decision=None, mde=None, configs=None):
    mde = dict(mde or _FAILING_MDE)
    mde.setdefault("observed_separation_pp_by_pool",
                   {"primary": {"momentum|512": 0.4}})
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
            "scope": {"complete": True},
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
    # The committed payload stores only the winner's id; the recommendation
    # branch reads that winner's own numbers, so --render-only must resolve it.
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


# ---------------------------------------------------------------------------
# The committed JSON and the contract report belong to the full design.
# ---------------------------------------------------------------------------

def test_1424_owns_the_contract_path():
    assert os.path.basename(study._DEFAULT_REPORT_OUT) == \
        "hurst_gate_calibration.md"
    assert os.path.basename(study._DEFAULT_JSON_OUT) == \
        "hurst_1424_gate_resolution.json"


def test_no_predecessor_study_still_defaults_to_the_contract_path():
    # The regression this guards: a --render-only of a superseded study
    # silently reverting the live evidence to its old verdict.
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


# ---------------------------------------------------------------------------
# Inherited invariants that must not drift.
# ---------------------------------------------------------------------------

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


def test_stage_0_is_scored_on_net_return_so_it_stays_comparable():
    assert study.joint_separation_verdict.__doc__
    assert "NET RETURN" in study.joint_separation_verdict.__doc__


def test_look_ahead_shifts_are_the_inherited_ones():
    series = pd.Series(np.arange(10.0))
    assert study.decision_series(series).equals(series.shift(1))
    assert study.entry_stamp_series(series).equals(series.shift(2))
