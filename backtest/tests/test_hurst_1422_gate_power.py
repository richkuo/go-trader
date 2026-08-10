"""#1422: pure helpers of the Hurst gate POWER study.

Covers the cohort split that keeps hypothesis selection from contaminating the
primary test, the effective-N estimator, the cluster-rotation null, the
minimum-detectable-effect search, the joint ADX x Hurst bucketing and its Stage 0
verdict, the coverage density floor, the shared look-ahead rule for H AND ADX,
and the report's purity. The EMPIRICAL result of the study is never asserted
here — only the machinery that turns numbers into a verdict.

Imported the same way test_hurst_1410_gate_calibration.py imports its research
module (explicit research/ on sys.path, unambiguous module name — safe under the
#1304 `-n auto` parallel run).
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

import hurst_1422_gate_power as study  # noqa: E402
import hurst_1410_gate_calibration as study1410  # noqa: E402

DAY_NS = 86_400_000_000_000


def _trade(symbol="BTC/USDT", timeframe="1h", window="2021", day=0, pnl=1.0,
           hold_days=1, h=None, adx=None, cohort=None):
    entry = pd.Timestamp("2021-01-01") + pd.Timedelta(days=day)
    return {
        "strategy": "momentum",
        "symbol": symbol,
        "timeframe": timeframe,
        "window": window,
        "cohort": cohort or study.cell_cohort(symbol, timeframe, window),
        "entry_date": str(entry),
        "entry_ns": int(entry.value),
        "exit_ns": int((entry + pd.Timedelta(days=hold_days)).value),
        "pnl_pct_net": float(pnl),
        "adx": adx,
        "h": {512: h, 128: h},
        "armed": {},
    }


# ---------------------------------------------------------------------------
# Cohort split — the defence against selection contamination.
# ---------------------------------------------------------------------------

def test_every_1410_cell_is_exploratory():
    for key, window in study.D_1410:
        symbol, timeframe = key.rsplit(" ", 1)
        assert study.cell_cohort(symbol, timeframe, window) == study.COHORT_EXPLORATORY


def test_pre_2023_windows_are_primary_for_audit_symbols():
    for window in study.NEW_WINDOWS:
        for symbol in study.AUDIT_SYMBOLS:
            for tf in ("1h", "4h"):
                assert study.cell_cohort(symbol, tf, window) == study.COHORT_PRIMARY


def test_new_symbols_are_primary_on_every_window():
    for window in study.WINDOWS:
        assert study.cell_cohort("DOGE/USDT", "1h", window) == study.COHORT_PRIMARY


def test_new_timeframe_on_audit_symbol_over_1410_window_is_exploratory():
    # The same TAPE, resampled — #1410 already mined this period on this symbol,
    # so a hypothesis chosen from #1410's p-values may not be scored here.
    for window in study1410.WINDOWS:
        for tf in ("2h", "15m", "30m"):
            assert study.cell_cohort("BTC/USDT", tf, window) == \
                study.COHORT_EXPLORATORY


def test_new_timeframe_on_audit_symbol_over_new_window_is_primary():
    assert study.cell_cohort("BTC/USDT", "2h", "2021") == study.COHORT_PRIMARY


def test_cell_cohort_rejects_unknown_window():
    with pytest.raises(ValueError):
        study.cell_cohort("BTC/USDT", "1h", "not-a-window")


def test_d_1410_matches_eval_windows_grid():
    from eval_windows import DATASETS, WINDOWS as EW
    assert len(study.D_1410) == len(DATASETS) * len(EW) == 30


def test_new_windows_do_not_collide_with_eval_windows():
    assert not set(study.NEW_WINDOWS) & set(study1410.WINDOWS)
    for name in study1410.WINDOWS:
        assert study.WINDOWS[name] == study1410.WINDOWS[name]


def test_primary_grid_is_exactly_the_pinned_hypotheses():
    grid = study._sweep_grid(study.COHORT_PRIMARY, study.HURST_WINDOWS)
    ids = set()
    for family, mode, hw, arm, disarm, gain in grid:
        ids.add(study.gate_config_id(family, hw, arm, disarm) if mode == "gate"
                else study.size_config_id(family, hw, gain))
    assert ids == set(study.PRIMARY_CONFIG_IDS)


def test_exploratory_grid_is_the_full_1410_sweep():
    grid = study._sweep_grid(study.COHORT_EXPLORATORY, study.HURST_WINDOWS)
    assert len(grid) == 30


def test_primary_ids_match_the_committed_1410_argmin():
    path = os.path.join(os.path.dirname(study.__file__),
                        "hurst_1410_gate_calibration.json")
    assert study.resolve_primary_config_ids(path) == tuple(
        sorted(study.PRIMARY_CONFIG_IDS))


# ---------------------------------------------------------------------------
# Effective N.
# ---------------------------------------------------------------------------

def test_overlap_counter_matches_brute_force():
    rng = np.random.default_rng(0)
    a_s = rng.integers(0, 100, 40)
    a_e = a_s + rng.integers(1, 20, 40)
    b_s = rng.integers(0, 100, 30)
    b_e = b_s + rng.integers(1, 20, 30)
    brute = sum(1 for i in range(len(a_s)) for j in range(len(b_s))
                if a_s[i] < b_e[j] and b_s[j] < a_e[i])
    assert study.count_overlapping_pairs(a_s, a_e, b_s, b_e) == brute


def test_overlap_counter_handles_empty_sides():
    assert study.count_overlapping_pairs([], [], [1], [2]) == 0
    assert study.count_overlapping_pairs([1], [2], [], []) == 0


def test_effective_n_equals_n_when_nothing_overlaps():
    trades = [_trade(day=10 * i, hold_days=1) for i in range(8)]
    assert study.effective_n(trades, {}) == pytest.approx(len(trades))


def test_effective_n_collapses_for_concurrent_same_symbol_trades():
    # Same tape, all coexisting: eight rows carry about one trade of
    # independent information.
    trades = [_trade(day=0, hold_days=30) for _ in range(8)]
    assert study.effective_n(trades, {}) == pytest.approx(1.0, abs=0.01)


def test_effective_n_collapses_for_correlated_concurrent_symbols():
    rho = {("BTC/USDT", "ETH/USDT"): 1.0}
    trades = ([_trade(symbol="BTC/USDT", day=0, hold_days=30) for _ in range(4)]
              + [_trade(symbol="ETH/USDT", day=0, hold_days=30) for _ in range(4)])
    assert study.effective_n(trades, rho) == pytest.approx(1.0, abs=0.01)


def test_effective_n_credits_uncorrelated_concurrent_symbols():
    rho = {("BTC/USDT", "ETH/USDT"): 0.0}
    trades = ([_trade(symbol="BTC/USDT", day=0, hold_days=30) for _ in range(4)]
              + [_trade(symbol="ETH/USDT", day=0, hold_days=30) for _ in range(4)])
    # Each symbol's own four rows still collapse; the two groups stay separate.
    assert study.effective_n(trades, rho) == pytest.approx(2.0, abs=0.05)


def test_effective_n_never_credits_anti_correlation():
    neg = {("BTC/USDT", "ETH/USDT"): -1.0}
    zero = {("BTC/USDT", "ETH/USDT"): 0.0}
    trades = ([_trade(symbol="BTC/USDT", day=0, hold_days=30) for _ in range(4)]
              + [_trade(symbol="ETH/USDT", day=0, hold_days=30) for _ in range(4)])
    assert study.effective_n(trades, neg) == study.effective_n(trades, zero)


def test_effective_n_is_bounded_by_n():
    rng = np.random.default_rng(3)
    trades = [_trade(day=int(d), hold_days=int(h))
              for d, h in zip(rng.integers(0, 200, 25), rng.integers(1, 40, 25))]
    n_eff = study.effective_n(trades, {})
    assert 1.0 <= n_eff <= len(trades)


def test_effective_n_of_empty_pool_is_zero():
    assert study.effective_n([], {}) == 0.0


def test_pairwise_rho_same_symbol_is_one_across_timeframes():
    assert study.pairwise_trade_rho({}, "BTC/USDT", "BTC/USDT") == 1.0


def test_pairwise_rho_unknown_pair_is_conservative():
    # Unknown correlation must never be a free grant of independence.
    assert study.pairwise_trade_rho({}, "BTC/USDT", "DOGE/USDT") == 1.0


def test_pairwise_rho_is_symmetric_and_clipped():
    rho = {("A", "B"): 0.4}
    assert study.pairwise_trade_rho(rho, "B", "A") == 0.4
    assert study.pairwise_trade_rho({("A", "B"): 5.0}, "A", "B") == 1.0
    assert study.pairwise_trade_rho({("A", "B"): float("nan")}, "A", "B") == 1.0


def _close_frame(log_returns, *, intraday=False):
    closes = np.exp(np.r_[0.0, np.cumsum(np.asarray(log_returns, dtype=float))])
    days = pd.date_range("2021-01-01", periods=len(closes), freq="1D")
    if not intraday:
        return pd.DataFrame({"close": closes}, index=days)
    index, values = [], []
    for i, (day, close) in enumerate(zip(days, closes)):
        # The first observation is deliberately unrelated noise. A correct
        # daily resample reads the LAST close and recovers `closes` exactly.
        index.extend((day, day + pd.Timedelta(hours=23)))
        values.extend((close * (1.7 if i % 2 else 0.4), close))
    return pd.DataFrame({"close": values}, index=pd.DatetimeIndex(index))


def test_symbol_return_correlations_uses_daily_last_from_finest_timeframe():
    returns = np.linspace(-0.04, 0.04, 40)
    frames = {
        # Lexical ordering puts "1d" before "1h"; resolution ordering must
        # still select the 1h tape, whose daily LAST closes match BBB.
        ("AAA/USDT", "1d"): _close_frame(-returns),
        ("AAA/USDT", "1h"): _close_frame(returns, intraday=True),
        ("BBB/USDT", "4h"): _close_frame(returns),
        ("CCC/USDT", "1h"): _close_frame(np.zeros_like(returns)),
        ("DDD/USDT", "2h"): _close_frame(-returns),
    }
    rho = study.symbol_return_correlations(frames)
    assert rho[("AAA/USDT", "BBB/USDT")] == pytest.approx(1.0)
    # Preserve a legitimate negative value here; pairwise_trade_rho is the
    # downstream layer that conservatively clips it to zero.
    assert rho[("BBB/USDT", "DDD/USDT")] == pytest.approx(-1.0)
    # A constant series produces NaN Pearson correlations, which must not leak
    # into the effective-N input.
    assert not any("CCC/USDT" in pair for pair in rho)
    assert list(rho) == sorted(rho)


def test_symbol_return_correlations_omits_short_overlap():
    # Thirty closes yield only 29 daily returns, below the 30-return floor.
    returns = np.linspace(-0.03, 0.03, 29)
    frames = {
        ("AAA/USDT", "1h"): _close_frame(returns),
        ("BBB/USDT", "1h"): _close_frame(returns),
    }
    assert study.symbol_return_correlations(frames) == {}


# ---------------------------------------------------------------------------
# Cluster rotation null.
# ---------------------------------------------------------------------------

def _spread_trades(symbol, n, pnl_fn, label_fn, start_day=0, step_days=3):
    out = []
    for i in range(n):
        t = _trade(symbol=symbol, day=start_day + i * step_days,
                   pnl=pnl_fn(i), hold_days=1)
        t["_lab"] = label_fn(i)
        out.append(t)
    return out


def test_rotation_shift_counts_share_one_calendar_offset():
    a = _spread_trades("BTC/USDT", 60, lambda i: 0.0, lambda i: False, step_days=2)
    b = _spread_trades("ETH/USDT", 30, lambda i: 0.0, lambda i: False, step_days=4)
    clusters = study.cluster_rotation_offsets(a + b)
    shifts = study.rotation_shift_counts(clusters, 40)
    # 40 days at one trade every 2 days is 20 trades; every 4 days is 10.
    assert shifts["BTC/USDT 1h"] == 20
    assert shifts["ETH/USDT 1h"] == 10


def test_rotation_preserves_label_counts():
    trades = _spread_trades("BTC/USDT", 80, lambda i: 0.0, lambda i: i < 40)
    clusters = study.cluster_rotation_offsets(trades)
    labels = np.array([t["_lab"] for t in trades], dtype=bool)
    shifts = study.rotation_shift_counts(clusters, 55)
    rotated = study._rotate_values(labels, clusters, shifts)
    assert int(rotated.sum()) == int(labels.sum())


def test_cluster_p_is_seeded_and_reproducible():
    trades = _spread_trades("BTC/USDT", 120, lambda i: (-2.0 if i < 60 else 2.0),
                            lambda i: i < 60)
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    a = study.cluster_permutation_pvalue_group_diff(trades, vals, sup, n_perm=200,
                                                    seed=7)
    b = study.cluster_permutation_pvalue_group_diff(trades, vals, sup, n_perm=200,
                                                    seed=7)
    assert a["p"] == b["p"]


def _blocky(i, run=17):
    """Run-structured label, the shape a hysteresis gate actually produces."""
    return bool((i // run) % 2)


def test_cluster_p_detects_a_real_time_aligned_separation():
    # A gate that genuinely suppresses the bad stretches. Labels come in RUNS,
    # which is what hysteresis produces and what a rotation has to break.
    trades = _spread_trades("BTC/USDT", 300,
                            lambda i: (-5.0 if _blocky(i) else 5.0), _blocky)
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=500, seed=1)
    assert res["p"] is not None and res["p"] < 0.05


def test_cluster_p_has_little_power_against_a_periodic_pattern():
    # An honest limitation, asserted so it stays known: labels alternating
    # every trade are reproduced exactly by any even rotation, so the null
    # contains the observed statistic about half the time. Real gate labels are
    # runs, not alternations — but a reader must not mistake this for a bug.
    trades = _spread_trades("BTC/USDT", 300, lambda i: (-5.0 if i % 2 else 5.0),
                            lambda i: bool(i % 2))
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=500, seed=1)
    assert res["p"] is not None and res["p"] > 0.05


def test_cluster_p_is_unimpressed_by_noise():
    rng = np.random.default_rng(11)
    trades = _spread_trades("BTC/USDT", 200, lambda i: 0.0, lambda i: bool(i % 2))
    vals = list(rng.normal(0, 1, len(trades)))
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=500, seed=2)
    assert res["p"] is not None and res["p"] > 0.05


def test_cluster_p_moves_concurrent_datasets_together():
    # Two datasets holding identical, perfectly aligned tape must produce the
    # same null as one of them alone — that is what "clusters move together"
    # means, and what the free shuffle gets wrong.
    one = _spread_trades("BTC/USDT", 150, lambda i: (-3.0 if i % 2 else 3.0),
                         lambda i: bool(i % 2))
    two = one + [dict(t, symbol="ETH/USDT") for t in one]
    v1 = [t["pnl_pct_net"] for t in one]
    s1 = [t["_lab"] for t in one]
    v2 = [t["pnl_pct_net"] for t in two]
    s2 = [t["_lab"] for t in two]
    r1 = study.cluster_permutation_pvalue_group_diff(one, v1, s1, n_perm=300, seed=5)
    r2 = study.cluster_permutation_pvalue_group_diff(two, v2, s2, n_perm=300, seed=5)
    assert r1["p"] == r2["p"]


def test_cluster_p_excludes_short_span_datasets():
    short = _spread_trades("DOGE/USDT", 10, lambda i: 1.0, lambda i: bool(i % 2),
                           step_days=1)
    long = _spread_trades("BTC/USDT", 150, lambda i: 1.0, lambda i: bool(i % 2))
    trades = short + long
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=50, seed=3)
    assert "DOGE/USDT 1h" in res["excluded_datasets"]
    # "Excluded" must mean the rows LEFT, not merely that the name was printed.
    assert res["n_scored"] == len(long)
    assert res["n_excluded_trades"] == len(short)


def test_excluded_dataset_leaves_the_observed_statistic():
    # The excluded dataset carries nearly all the suppressed rows and a huge
    # separation. If exclusion were a label only, dropping it would leave the
    # p-value unchanged.
    long = _spread_trades("BTC/USDT", 150, lambda i: 1.0, lambda i: bool(i % 2))
    short = _spread_trades("DOGE/USDT", 40, lambda i: -50.0, lambda i: True,
                           step_days=1)
    with_short = long + short
    vals = [t["pnl_pct_net"] for t in with_short]
    sup = [t["_lab"] for t in with_short]
    res_all = study.cluster_permutation_pvalue_group_diff(with_short, vals, sup,
                                                          n_perm=200, seed=3)
    res_long = study.cluster_permutation_pvalue_group_diff(
        long, [t["pnl_pct_net"] for t in long], [t["_lab"] for t in long],
        n_perm=200, seed=3)
    assert res_all["n_scored"] == len(long)
    assert res_all["p"] == res_long["p"]


def test_excluded_rows_leave_the_config_counts_and_effective_n():
    # A dataset the null cannot rotate must not help a config clear a volume
    # floor it never contributed evidence to.
    long = _spread_trades("BTC/USDT", 150, lambda i: 1.0, lambda i: bool(i % 2))
    short = _spread_trades("DOGE/USDT", 40, lambda i: 1.0, lambda i: bool(i % 2),
                           step_days=1)
    idx, excluded = study.usable_cluster_rows(long + short)
    assert excluded == ["DOGE/USDT 1h"]
    assert len(idx) == len(long)
    assert all(i < len(long) for i in idx)


def test_cluster_p_is_none_when_no_dataset_can_rotate():
    trades = _spread_trades("BTC/USDT", 5, lambda i: 1.0, lambda i: bool(i % 2),
                            step_days=1)
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=50, seed=3)
    assert res["p"] is None and res["reason"]
    assert res["n_scored"] == 0 and res["n_excluded_trades"] == len(trades)


def test_cluster_weighted_p_is_none_when_every_dataset_is_short():
    trades = _spread_trades("BTC/USDT", 6, lambda i: 1.0, lambda i: False,
                            step_days=1)
    rets = [t["pnl_pct_net"] for t in trades]
    res = study.cluster_permutation_pvalue_weighted(
        trades, rets, [1.5 if i % 2 else 0.5 for i in range(len(trades))],
        n_perm=50, seed=3)
    assert res["p"] is None and res["n_scored"] == 0


# --- the offset fold: no dataset may sit out a draw ------------------------

def test_effective_offset_is_the_identity_on_the_longest_span():
    # The draw range is [MIN_OFFSET_DAYS, span - MIN_OFFSET_DAYS], so a dataset
    # holding the pool's longest span must fold to itself — that is what keeps
    # an even-span pool byte-identical to the unfolded behaviour.
    span = 900
    for off in range(study.MIN_OFFSET_DAYS, span - study.MIN_OFFSET_DAYS + 1):
        assert study.effective_offset_days(off, span) == off


def test_effective_offset_folds_a_long_offset_into_a_short_span():
    eff = study.effective_offset_days(1500, 913)
    assert study.MIN_OFFSET_DAYS <= eff <= 913 - study.MIN_OFFSET_DAYS


def test_effective_offset_never_returns_a_near_identity_shift():
    for span in (91, 200, 913, 2198):
        for off in (30, 31, span - 1, span, span + 1, 5000):
            eff = study.effective_offset_days(off, span)
            assert eff >= 1
            assert eff <= span - study.MIN_OFFSET_DAYS or span <= 2 * study.MIN_OFFSET_DAYS


def test_short_span_dataset_still_rotates_under_a_long_offset():
    # The regression: a 900-day dataset beside a 2,200-day one, drawn at an
    # offset above its own span. Before the fold, `searchsorted` ran off the end
    # and the modulo made the shift exactly 0 — the dataset kept its observed
    # label ordering inside the "null".
    short = _spread_trades("ETH/USDT", 300, lambda i: 0.0, lambda i: False,
                           step_days=3)                      # ~900 days
    long = _spread_trades("BTC/USDT", 300, lambda i: 0.0, lambda i: False,
                          step_days=7)                       # ~2,100 days
    clusters = study.cluster_rotation_offsets(short + long)
    for offset in (1000, 1500, 2000):
        shifts = study.rotation_shift_counts(clusters, offset)
        assert shifts["ETH/USDT 1h"] != 0
        assert shifts["BTC/USDT 1h"] != 0


def test_rotation_shift_is_never_the_identity_for_any_offset():
    short = _spread_trades("ETH/USDT", 120, lambda i: 0.0, lambda i: False,
                           step_days=3)
    long = _spread_trades("BTC/USDT", 400, lambda i: 0.0, lambda i: False,
                          step_days=7)
    clusters = study.cluster_rotation_offsets(short + long)
    for offset in range(study.MIN_OFFSET_DAYS, 2600, 37):
        for key, shift in study.rotation_shift_counts(clusters, offset).items():
            assert 1 <= shift <= len(clusters[key]["order"]) - 1


def test_offset_range_is_capped_by_the_shortest_span():
    # Co-movement is the null's whole purpose: every retained dataset must be
    # able to host every drawn offset, so no draw shifts two concurrent datasets
    # by different calendar amounts.
    short = _spread_trades("ETH/USDT", 200, lambda i: 0.0, lambda i: False,
                           step_days=3)                      # ~597 days
    long = _spread_trades("BTC/USDT", 200, lambda i: 0.0, lambda i: False,
                          step_days=9)                       # ~1,791 days
    clusters = study.cluster_rotation_offsets(short + long)
    lo, hi = study._admissible_offsets(clusters)
    spans = [v["span_days"] for v in clusters.values()]
    assert lo == study.MIN_OFFSET_DAYS
    assert hi == min(spans) - study.MIN_OFFSET_DAYS
    assert hi < max(spans) - study.MIN_OFFSET_DAYS


def test_capped_range_makes_the_wrap_guard_dormant():
    # Inside the capped range the fold must be the identity for every dataset,
    # so the shared calendar offset really is shared.
    short = _spread_trades("ETH/USDT", 200, lambda i: 0.0, lambda i: False,
                           step_days=3)
    long = _spread_trades("BTC/USDT", 200, lambda i: 0.0, lambda i: False,
                          step_days=9)
    clusters = study.cluster_rotation_offsets(short + long)
    lo, hi = study._admissible_offsets(clusters)
    for off in range(lo, hi + 1, 17):
        for info in clusters.values():
            assert study.effective_offset_days(off, info["span_days"]) == off


def test_ragged_pool_null_no_longer_inherits_the_observed_alignment():
    # Both datasets carry the same real, run-structured separation. When the
    # short one is never rotated, its observed contrast rides into every null
    # draw and inflates p. With the fold it is rotated like the long one.
    short = _spread_trades("ETH/USDT", 200,
                           lambda i: (-5.0 if _blocky(i) else 5.0), _blocky,
                           step_days=3)
    long = _spread_trades("BTC/USDT", 200,
                          lambda i: (-5.0 if _blocky(i) else 5.0), _blocky,
                          step_days=9)
    trades = short + long
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=500, seed=1)
    assert res["p"] is not None and res["p"] < 0.05


def test_equal_span_pool_is_unaffected_by_the_fold():
    # Scoping guarantee: when every dataset shares the pool's longest span the
    # fold is the identity, so the p-value is exactly what the unfolded code
    # produced. The fix touches the ragged case only.
    a = _spread_trades("BTC/USDT", 200, lambda i: (-4.0 if _blocky(i) else 4.0),
                       _blocky, step_days=3)
    b = [dict(t, symbol="ETH/USDT") for t in a]
    trades = a + b
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    clusters = study.cluster_rotation_offsets(trades)
    span = max(v["span_days"] for v in clusters.values())
    for off in (study.MIN_OFFSET_DAYS, span // 2, span - study.MIN_OFFSET_DAYS):
        shifts = study.rotation_shift_counts(clusters, off)
        ns = np.asarray(clusters["BTC/USDT 1h"]["ns"], dtype=np.int64)
        unfolded = int(np.searchsorted(ns, ns[0] + off * DAY_NS, side="left"))
        assert shifts["BTC/USDT 1h"] == unfolded % len(ns)
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=300, seed=1)
    assert res["p"] is not None


def test_cluster_p_is_none_without_a_contrast():
    trades = _spread_trades("BTC/USDT", 100, lambda i: 1.0, lambda i: False)
    vals = [t["pnl_pct_net"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(
        trades, vals, [False] * len(trades), n_perm=50, seed=3)
    assert res["p"] is None


def test_cluster_p_uses_the_add_one_convention():
    trades = _spread_trades("BTC/USDT", 200, lambda i: (-9.0 if i % 2 else 9.0),
                            lambda i: bool(i % 2))
    vals = [t["pnl_pct_net"] for t in trades]
    sup = [t["_lab"] for t in trades]
    res = study.cluster_permutation_pvalue_group_diff(trades, vals, sup,
                                                      n_perm=100, seed=4)
    assert res["p"] > 0.0


def test_cluster_weighted_p_needs_multiplier_variation():
    trades = _spread_trades("BTC/USDT", 100, lambda i: 1.0, lambda i: False)
    rets = [t["pnl_pct_net"] for t in trades]
    res = study.cluster_permutation_pvalue_weighted(trades, rets,
                                                    [1.0] * len(trades),
                                                    n_perm=50, seed=1)
    assert res["p"] is None


def test_cluster_weighted_p_is_seeded():
    trades = _spread_trades("BTC/USDT", 150, lambda i: (i % 5) - 2.0,
                            lambda i: False)
    rets = [t["pnl_pct_net"] for t in trades]
    mults = [1.5 if r > 0 else 0.5 for r in rets]
    a = study.cluster_permutation_pvalue_weighted(trades, rets, mults,
                                                  n_perm=200, seed=9)
    b = study.cluster_permutation_pvalue_weighted(trades, rets, mults,
                                                  n_perm=200, seed=9)
    assert a["p"] == b["p"]


def test_cluster_p_rejects_length_mismatch():
    trades = _spread_trades("BTC/USDT", 10, lambda i: 1.0, lambda i: False)
    with pytest.raises(ValueError):
        study.cluster_permutation_pvalue_group_diff(trades, [1.0], [True])


# ---------------------------------------------------------------------------
# Minimum detectable effect.
# ---------------------------------------------------------------------------

def test_rank1_threshold_is_the_hardest_bh_bar():
    assert study._rank1_threshold(30) == pytest.approx(0.05 / 30)
    assert study._rank1_threshold(4) == pytest.approx(0.05 / 4)
    assert study._rank1_threshold(0) == pytest.approx(0.05)


def test_mde_returns_none_when_the_design_cannot_detect_anything():
    # Two trades per side cannot clear a 0.05/30 bar at any injected effect.
    trades = _spread_trades("BTC/USDT", 4, lambda i: 0.0, lambda i: bool(i % 2))
    vals = [0.0] * 4
    sup = [t["_lab"] for t in trades]
    assert study.min_detectable_effect(trades, vals, sup, 30, cluster=False,
                                       n_perm=study.N_PERM_MDE, seed=1) is None


def test_mde_injection_of_the_returned_effect_reaches_the_bar():
    rng = np.random.default_rng(21)
    trades = _spread_trades("BTC/USDT", 200, lambda i: 0.0, lambda i: bool(i % 2))
    vals = list(rng.normal(0, 1.0, len(trades)))
    sup = [t["_lab"] for t in trades]
    d = study.min_detectable_effect(trades, vals, sup, 4, cluster=False,
                                    n_perm=400, seed=1)
    assert d is not None
    shifted = np.asarray(vals) - np.where(np.asarray(sup, dtype=bool), d, 0.0)
    p = study.permutation_pvalue_group_diff(shifted, sup, n_perm=400, seed=1)
    assert p <= study._rank1_threshold(4)


def test_mde_is_deterministic_under_the_seed():
    rng = np.random.default_rng(22)
    trades = _spread_trades("BTC/USDT", 150, lambda i: 0.0, lambda i: bool(i % 2))
    vals = list(rng.normal(0, 1.0, len(trades)))
    sup = [t["_lab"] for t in trades]
    kw = dict(cluster=False, n_perm=200, seed=3)
    assert (study.min_detectable_effect(trades, vals, sup, 4, **kw)
            == study.min_detectable_effect(trades, vals, sup, 4, **kw))


def test_mde_is_harder_under_a_bigger_family():
    rng = np.random.default_rng(23)
    trades = _spread_trades("BTC/USDT", 200, lambda i: 0.0, lambda i: bool(i % 2))
    vals = list(rng.normal(0, 1.0, len(trades)))
    sup = [t["_lab"] for t in trades]
    small = study.min_detectable_effect(trades, vals, sup, 4, cluster=False,
                                        n_perm=4000, seed=1)
    big = study.min_detectable_effect(trades, vals, sup, 30, cluster=False,
                                      n_perm=4000, seed=1)
    assert small is not None and big is not None and big >= small


def test_mde_refuses_a_permutation_count_that_cannot_resolve_the_bar():
    # 400 draws floor p at 1/401 = 0.0025, above the 0.05/30 rank-1 bar. Left
    # unguarded this would report "no detectable effect" when the real cause is
    # too few permutations — a false claim about power, in the very number the
    # report leans on.
    trades = _spread_trades("BTC/USDT", 50, lambda i: 0.0, lambda i: bool(i % 2))
    vals = [0.0] * len(trades)
    sup = [t["_lab"] for t in trades]
    with pytest.raises(ValueError, match="cannot resolve"):
        study.min_detectable_effect(trades, vals, sup, 30, cluster=False,
                                    n_perm=400, seed=1)


def test_default_mde_permutation_count_resolves_every_family_size():
    for family_size in (len(study.PRIMARY_CONFIG_IDS), 30):
        bar = study._rank1_threshold(family_size)
        assert 1.0 / (study.N_PERM_MDE + 1.0) <= bar


def test_mde_of_a_degenerate_split_is_none():
    trades = _spread_trades("BTC/USDT", 20, lambda i: 1.0, lambda i: False)
    assert study.min_detectable_effect(trades, [1.0] * 20, [False] * 20, 4,
                                       cluster=False, n_perm=study.N_PERM_MDE,
                                       seed=1) is None


# ---------------------------------------------------------------------------
# Joint ADX x Hurst.
# ---------------------------------------------------------------------------

def test_joint_h_bucket_routes_nan_to_its_own_bucket():
    assert study.joint_h_bucket(None) == study.BUCKET_NAN
    assert study.joint_h_bucket(float("nan")) == study.BUCKET_NAN


@pytest.mark.parametrize("value,expected", [
    (0.20, "<0.45"), (0.4499, "<0.45"), (0.45, "0.45-0.55"),
    (0.50, "0.45-0.55"), (0.55, "0.45-0.55"), (0.5501, ">0.55"), (2.0, ">0.55"),
])
def test_joint_h_bucket_boundaries(value, expected):
    assert study.joint_h_bucket(value) == expected


@pytest.mark.parametrize("value,expected", [
    (0.0, "<25"), (24.999, "<25"), (25.0, ">=25"), (80.0, ">=25"),
    (None, study.BUCKET_NAN), (float("nan"), study.BUCKET_NAN),
])
def test_joint_adx_bucket_boundaries(value, expected):
    assert study.joint_adx_bucket(value) == expected


def test_joint_table_covers_every_cell_and_counts_correctly():
    trades = [_trade(h=0.3, adx=30.0), _trade(h=0.7, adx=30.0),
              _trade(h=0.5, adx=10.0), _trade(h=None, adx=None)]
    table = study.joint_adx_hurst_table(trades, 512)
    assert len(table) == len(study.JOINT_ADX_BUCKETS) * len(study.JOINT_H_BUCKETS)
    assert table[">=25|<0.45"]["trades"] == 1
    assert table[">=25|>0.55"]["trades"] == 1
    assert table["<25|0.45-0.55"]["trades"] == 1
    assert table[f"{study.BUCKET_NAN}|{study.BUCKET_NAN}"]["trades"] == 1
    assert sum(v["trades"] for v in table.values()) == len(trades)


def test_joint_verdict_says_no_separation_on_a_null_pool():
    trades = []
    for i in range(200):
        t = _trade(day=i * 3, pnl=1.0 if i % 2 else -1.0,
                   h=0.3 if i % 2 else 0.7, adx=40.0)
        trades.append(t)
    v = study.joint_separation_verdict(trades, 512, n_perm=200, seed=1)
    assert v["separated"] is False


def test_joint_verdict_measures_its_limit_on_its_own_rows():
    # The limit must come from the rows this contrast scores, at the bar this
    # contrast is corrected by — never borrowed from a differently sized pool.
    small = [_trade(day=i * 3, pnl=-5.0 if _blocky(i) else 5.0,
                    h=0.3 if _blocky(i) else 0.7, adx=40.0)
             for i in range(80)]
    large = [_trade(day=i * 3, pnl=-5.0 if _blocky(i) else 5.0,
                    h=0.3 if _blocky(i) else 0.7, adx=40.0)
             for i in range(400)]
    v_small = study.joint_separation_verdict(small, 512, n_perm=200, seed=1)
    v_large = study.joint_separation_verdict(large, 512, n_perm=200, seed=1)
    assert v_small["mde_pp"] is not None and v_large["mde_pp"] is not None
    # More independent rows resolve a smaller effect.
    assert v_large["mde_pp"] <= v_small["mde_pp"]
    assert v_small["n_scored"] == len(small)


def test_joint_verdict_separates_on_a_real_significant_effect():
    strong = [_trade(day=i * 3, pnl=-5.0 if _blocky(i) else 5.0,
                     h=0.3 if _blocky(i) else 0.7, adx=40.0)
              for i in range(300)]
    v = study.joint_separation_verdict(strong, 512, n_perm=300, seed=1)
    assert v["separated"] is True
    assert abs(v["delta_mean_pp"]) >= v["mde_pp"]


def test_joint_verdict_fails_on_a_noisy_pool_before_it_reaches_the_limit():
    rng = np.random.default_rng(3)
    noisy = [_trade(day=i * 3, pnl=float(rng.normal(0, 5)),
                    h=0.3 if _blocky(i) else 0.7, adx=40.0)
             for i in range(300)]
    v = study.joint_separation_verdict(noisy, 512, n_perm=300, seed=1)
    assert v["separated"] is False
    assert "Bonferroni bar" in v["reason"]


def test_same_pool_limit_can_only_bind_when_the_contrast_is_untestable():
    # A consequence of measuring the limit on the rows it gates, asserted so a
    # reader does not mistake it for the check being dropped: a contrast whose
    # UNINJECTED p already clears the bar has a measured limit of 0.0, so the
    # materiality condition is a floor, not a second hurdle. It still fails
    # closed when the limit is unreachable or no rotation exists.
    strong = [_trade(day=i * 3, pnl=-5.0 if _blocky(i) else 5.0,
                     h=0.3 if _blocky(i) else 0.7, adx=40.0)
              for i in range(300)]
    v = study.joint_separation_verdict(strong, 512, n_perm=300, seed=1)
    assert v["p_cluster"] <= study.JOINT_ALPHA
    assert v["mde_pp"] == 0.0


def test_joint_verdict_uses_one_permutation_resolution_at_the_boundary(monkeypatch):
    trades = [_trade(day=i * 3, pnl=-1.0 if i % 2 else 1.0,
                     h=0.3 if i % 2 else 0.7, adx=40.0)
              for i in range(100)]
    seen = {}

    def fake_cluster(*args, n_perm, seed):
        seen["p"] = (n_perm, seed)
        return {"p": study.JOINT_ALPHA - 0.000001}

    def fake_mde(*args, n_perm, seed, **kwargs):
        seen["mde"] = (n_perm, seed)
        return 0.0

    monkeypatch.setattr(study, "cluster_permutation_pvalue_group_diff",
                        fake_cluster)
    monkeypatch.setattr(study, "min_detectable_effect", fake_mde)
    verdict = study.joint_separation_verdict(trades, 512, n_perm=137, seed=9)
    assert seen == {"p": (137, 9), "mde": (137, 9)}
    assert verdict["separated"] is True


def test_joint_verdict_handles_an_empty_side():
    trades = [_trade(h=0.7, adx=40.0) for _ in range(10)]
    v = study.joint_separation_verdict(trades, 512, n_perm=50, seed=1)
    assert v["separated"] is False and "empty" in v["reason"]
    assert v["mde_pp"] is None


def test_joint_verdict_never_separates_on_an_unrotatable_pool():
    # No admissible rotation means no evidence — never an unbounded pass.
    trades = [_trade(day=i, pnl=-5.0 if i % 2 else 5.0,
                     h=0.3 if i % 2 else 0.7, adx=40.0) for i in range(40)]
    v = study.joint_separation_verdict(trades, 512, n_perm=300, seed=1)
    assert v["separated"] is False
    assert v["p_cluster"] is None and v["mde_pp"] is None


# ---------------------------------------------------------------------------
# Coverage audit.
# ---------------------------------------------------------------------------

def _frame(start, end, timeframe="1h"):
    idx = pd.date_range(start, end, freq=pd.Timedelta(
        minutes=study.timeframe_minutes(timeframe)), inclusive="left")
    return pd.DataFrame({"open": 1.0, "high": 1.0, "low": 1.0, "close": 1.0,
                         "volume": 1.0}, index=idx)


@pytest.mark.parametrize("tf,minutes", [("15m", 15), ("30m", 30), ("1h", 60),
                                        ("2h", 120), ("4h", 240), ("1d", 1440)])
def test_timeframe_minutes(tf, minutes):
    assert study.timeframe_minutes(tf) == minutes


def test_timeframe_minutes_rejects_unknown_unit():
    with pytest.raises(ValueError):
        study.timeframe_minutes("3y")


def test_coverage_keeps_a_complete_cell():
    frames = {("BTC/USDT", "4h"): _frame("2019-01-01", "2026-01-01", "4h")}
    cov = study.coverage_audit(frames, ["2021"], [512])
    assert cov["cells"]["BTC/USDT 4h|2021"] is True
    assert cov["n_dropped"] == 0


def test_coverage_drops_a_cell_without_enough_lead():
    frames = {("BTC/USDT", "4h"): _frame("2020-12-01", "2026-01-01", "4h")}
    cov = study.coverage_audit(frames, ["2021"], [512])
    assert cov["cells"]["BTC/USDT 4h|2021"] is False
    assert "lead" in cov["dropped"][0]["reason"]


def test_coverage_drops_a_delisting_gap():
    # A year present as a handful of bars is not a year.
    full = _frame("2019-01-01", "2021-01-01", "4h")
    sparse = _frame("2021-01-01", "2021-01-20", "4h")
    frames = {("XRP/USDT", "4h"): pd.concat([full, sparse])}
    cov = study.coverage_audit(frames, ["2021"], [512])
    assert cov["cells"]["XRP/USDT 4h|2021"] is False
    assert "data gap" in cov["dropped"][0]["reason"]


def test_coverage_drops_an_empty_frame():
    frames = {("NEW/USDT", "1h"): pd.DataFrame()}
    cov = study.coverage_audit(frames, ["2021"], [512])
    assert cov["cells"]["NEW/USDT 1h|2021"] is False


def test_expected_bars_closes_an_open_window_at_the_reference_bar():
    last = pd.Timestamp("2021-01-11")
    assert study.expected_bars(("2021-01-01", None), "1h", last) == 240


def test_open_window_denominator_is_shared_across_datasets():
    # A dataset that stops early inside the open-ended window must be measured
    # against the run's latest bar, not its own — otherwise it scores 100%
    # dense on a fraction of the period and is silently kept.
    frames = {
        ("BTC/USDT", "4h"): _frame("2024-01-01", "2026-06-01", "4h"),
        ("STOP/USDT", "4h"): _frame("2024-01-01", "2026-01-20", "4h"),
    }
    cov = study.coverage_audit(frames, ["oos"], [512])
    assert cov["cells"]["BTC/USDT 4h|oos"] is True
    assert cov["cells"]["STOP/USDT 4h|oos"] is False
    assert "data gap" in [d["reason"] for d in cov["dropped"]][0]
    assert cov["reference_last_bar"] == str(frames[("BTC/USDT", "4h")].index[-1])


def test_open_window_keeps_every_cell_when_all_datasets_end_together():
    frames = {
        ("BTC/USDT", "4h"): _frame("2024-01-01", "2026-06-01", "4h"),
        ("ETH/USDT", "4h"): _frame("2024-01-01", "2026-06-01", "4h"),
    }
    cov = study.coverage_audit(frames, ["oos"], [512])
    assert cov["cells"]["BTC/USDT 4h|oos"] is True
    assert cov["cells"]["ETH/USDT 4h|oos"] is True
    assert cov["n_dropped"] == 0


def test_open_window_catches_a_mid_window_gap_plus_an_early_end():
    head = _frame("2024-01-01", "2026-02-01", "4h")
    gapped = _frame("2026-04-01", "2026-04-20", "4h")
    frames = {
        ("BTC/USDT", "4h"): _frame("2024-01-01", "2026-06-01", "4h"),
        ("GAPPY/USDT", "4h"): pd.concat([head, gapped]),
    }
    cov = study.coverage_audit(frames, ["oos"], [512])
    assert cov["cells"]["GAPPY/USDT 4h|oos"] is False


# ---------------------------------------------------------------------------
# Look-ahead: H and ADX share one rule.
# ---------------------------------------------------------------------------

def _ohlc(n=400, seed=5):
    rng = np.random.default_rng(seed)
    close = 100 + np.cumsum(rng.normal(0, 1, n))
    idx = pd.date_range("2021-01-01", periods=n, freq="1h")
    return pd.DataFrame({
        "open": close, "high": close + 1.0, "low": close - 1.0,
        "close": close, "volume": 1.0}, index=idx)


def test_adx_warmup_bars_are_nan_never_zero():
    df = _ohlc(120)
    s = study.adx_series(df, period=14)
    assert s.isna().iloc[0]
    # compute_regime would have written 0.0 here; a 0.0 would silently read as
    # "low ADX" instead of "unknown".
    assert not (s.fillna(-1) == 0.0).any()


def test_adx_stamp_does_not_see_its_own_bar():
    df = _ohlc(300)
    bar = 250
    stamp = study.adx_entry_stamp(df)
    raw = study.adx_series(df)
    mutated = df.copy()
    for col in ("open", "high", "low", "close"):
        mutated.iat[bar, mutated.columns.get_loc(col)] *= 1.25
    stamp_m = study.adx_entry_stamp(mutated)
    raw_m = study.adx_series(mutated)
    assert stamp.iloc[bar] == pytest.approx(stamp_m.iloc[bar], nan_ok=True)
    # Paired assertion: the UNSHIFTED series at that bar really did move, so
    # the test above is proving a shift, not an inert series.
    assert not np.isclose(raw.iloc[bar], raw_m.iloc[bar])


def test_hurst_decision_does_not_see_its_own_bar():
    df = _ohlc(300)
    bar = 250
    rolling = study.rolling_hurst(df["close"], 128)
    decision = study.decision_series(rolling)
    mutated = df.copy()
    mutated.iat[bar, mutated.columns.get_loc("close")] *= 1.25
    rolling_m = study.rolling_hurst(mutated["close"], 128)
    decision_m = study.decision_series(rolling_m)
    assert decision.iloc[bar] == pytest.approx(decision_m.iloc[bar], nan_ok=True)
    assert not np.isclose(rolling.iloc[bar], rolling_m.iloc[bar])


def test_adx_and_hurst_stamps_use_the_same_shift():
    df = _ohlc(300)
    raw_adx = study.adx_series(df)
    stamped = study.adx_entry_stamp(df)
    rolling = study.rolling_hurst(df["close"], 128)
    h_stamp = study.entry_stamp_series(rolling)
    # Both are the bar-close series shifted twice.
    assert stamped.iloc[200] == pytest.approx(raw_adx.iloc[198], nan_ok=True)
    assert h_stamp.iloc[200] == pytest.approx(rolling.iloc[198], nan_ok=True)


# ---------------------------------------------------------------------------
# Acceptance rule and recommendation.
# ---------------------------------------------------------------------------

def _cfg(**kw):
    base = {
        "config_id": "momentum/gate/W512/arm0.52/dis0.48",
        "cohort": study.COHORT_PRIMARY,
        "family": "momentum",
        "mode": "gate",
        "sense": study.FAMILY_SENSE["momentum"],
        "hurst_window": 512,
        "arm": 0.52, "disarm": 0.48, "gain": None,
        "protocol_windows": list(study.PRIMARY_PROTOCOL_WINDOWS),
        "held_out_windows": list(study.PRIMARY_HELD_OUT_WINDOWS),
        "p_raw": 0.001, "p_cluster": 0.001, "bh_reject": True,
        "cluster_reason": None,
        "n_pooled_trades": 400, "n_suppressed": 200, "n_kept": 200,
        "n_pooled_effective": 200.0,
        "n_suppressed_effective": 60.0, "n_kept_effective": 90.0,
        "windows": {},
    }
    good = {"n_legs": 3, "dd_delta": -2.0, "chop_delta": -3.0,
            "ret_gated": 5.0, "ret_ungated": 5.0,
            "trades_gated": 10, "trades_ungated": 20}
    windows = {w: dict(good) for w in study.PRIMARY_PROTOCOL_WINDOWS}
    windows.update({w: dict(good) for w in study.PRIMARY_HELD_OUT_WINDOWS})
    base["windows"] = windows
    base.update(kw)
    return base


def test_verdict_passes_a_fully_compliant_config():
    ok, reasons = study.config_verdict(_cfg())
    assert ok, reasons


def test_verdict_reads_the_cluster_p_not_the_free_p():
    # Significant on the free shuffle, untestable on the cluster null: the
    # #1410 number must not be able to carry a config through.
    cfg = _cfg(p_raw=0.0001, p_cluster=None, bh_reject=False,
               cluster_reason="no draws")
    ok, reasons = study.config_verdict(cfg)
    assert not ok
    assert any("cluster permutation p is untestable" in r for r in reasons)


def test_verdict_fails_closed_on_an_untestable_cluster_p():
    cfg = _cfg(p_cluster=None, bh_reject=True)
    assert not study.config_verdict(cfg)[0]


def test_effective_volume_floors_block_a_correlated_pool():
    cfg = _cfg(n_suppressed=5000, n_kept=5000,
               n_suppressed_effective=3.0, n_kept_effective=4.0)
    ok, reasons = study.config_verdict(cfg)
    assert not ok
    assert any("effective suppressed" in r for r in reasons)
    assert any("effective kept" in r for r in reasons)


def test_verdict_requires_drawdown_and_chop_to_improve():
    cfg = _cfg()
    cfg["windows"]["2021"] = dict(cfg["windows"]["2021"], dd_delta=1.0)
    ok, reasons = study.config_verdict(cfg)
    assert not ok and any("drawdown not reduced" in r for r in reasons)


def test_verdict_enforces_the_return_give_up_tolerance():
    cfg = _cfg()
    cfg["windows"]["2022"] = dict(cfg["windows"]["2022"], ret_gated=-50.0,
                                  ret_ungated=5.0)
    ok, reasons = study.config_verdict(cfg)
    assert not ok and any("return give-up" in r for r in reasons)


def test_held_out_rule_matches_1410_on_three_windows():
    windows = {"a": {"n_legs": 1, "dd_delta": -1.0},
               "b": {"n_legs": 1, "dd_delta": -1.0},
               "c": {"n_legs": 1, "dd_delta": 1.0}}
    ok, non_deg, n_with = study.held_out_verdict(windows, ["a", "b", "c"])
    assert ok and non_deg == 2 and n_with == 3
    windows["b"] = {"n_legs": 1, "dd_delta": 1.0}
    assert not study.held_out_verdict(windows, ["a", "b", "c"])[0]


def test_held_out_rule_fails_closed_with_too_few_windows():
    windows = {"a": {"n_legs": 1, "dd_delta": -1.0},
               "b": {"n_legs": 1, "dd_delta": -1.0}}
    assert not study.held_out_verdict(windows, ["a", "b"])[0]


def test_only_primary_configs_can_win():
    winner = _cfg(cohort=study.COHORT_EXPLORATORY)
    decision = study.decide_recommendation([winner], {})
    assert decision["verdict"] == "inconclusive"
    assert all(v["winner"] is None for v in decision["families"].values())


def test_a_passing_primary_config_wins():
    decision = study.decide_recommendation([_cfg()], {})
    assert decision["verdict"] == "config"
    assert decision["families"]["momentum"]["winner"]["config_id"] == _cfg()["config_id"]


def test_inconclusive_justification_carries_the_measured_power():
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False)],
        {"pooled_1410_cluster": 3.25, "pooled_primary_cluster": 1.5,
         "observed_separation_pp_by_pool": {"primary": {"momentum|512": 0.2}}})
    assert decision["verdict"] == "inconclusive"
    assert "3.25" in decision["justification"]
    assert "1.50" in decision["justification"]


def test_inconclusive_justification_states_an_unreachable_limit():
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False)],
        {"pooled_1410_cluster": None, "pooled_primary_cluster": None,
         "observed_separation_pp_by_pool": {"primary": {}}})
    assert "nothing below" in decision["justification"]


def test_a_sub_limit_separation_is_reported_as_unresolvable_not_as_absent():
    # A separation UNDER the design's own detection limit is invisible to the
    # design. Reading that null as "no edge exists" inverts the inference: it
    # turns a power failure into a claim about the market, which is the one
    # error that would make this report worse than silence.
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False)],
        {"pooled_1410_cluster": 3.0, "pooled_primary_cluster": 2.0,
         "observed_separation_pp_by_pool": {"primary": {"momentum|512": 0.5}}})
    text = decision["justification"]
    assert "BELOW" in text
    assert "INVISIBLE" in text
    assert "Power is the binding constraint" in text
    assert "no edge" not in text
    assert "ABOVE" not in text


def test_an_unreachable_limit_draws_no_conclusion_about_an_edge():
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False)],
        {"pooled_1410_cluster": 3.0, "pooled_primary_cluster": None,
         "observed_separation_pp_by_pool": {"primary": {"momentum|512": 0.5}}})
    text = decision["justification"]
    assert "resolves no edge of any size" in text
    assert "Nothing about the presence or absence of an edge follows" in text


def test_the_limit_is_compared_against_its_own_pool_s_separation():
    # The limit is measured on the PRIMARY cohort, so it may only be read
    # against the separation on that same cohort. A large separation elsewhere
    # in the study is a different sample and must not flip this branch.
    mde = {"pooled_1410_cluster": 3.0, "pooled_primary_cluster": 2.0,
           "observed_separation_pp": {"momentum|512": 9.0},
           "observed_separation_pp_by_pool": {
               "primary": {"momentum|512": 0.5},
               "exploratory": {"momentum|512": 9.0}}}
    text = study.decide_recommendation([_cfg(bh_reject=False)],
                                       mde)["justification"]
    assert "0.50" in text
    assert "9.00" not in text
    assert "change the RULE" not in text


def test_justification_says_the_rule_failed_when_separation_exceeds_the_limit():
    # The inverse of the case above, and the one that would be most damaging to
    # get wrong: claiming "no edge" when the design could see one and the raw
    # split shows it. The verdict must blame the RULE, not the evidence.
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False)],
        {"pooled_1410_cluster": 0.2, "pooled_primary_cluster": 0.1,
         "pooled_primary_cluster_p0": 0.03,
         "observed_separation_pp_by_pool": {"primary": {"momentum|512": 0.45}}})
    text = decision["justification"]
    assert "ABOVE" in text
    assert "change the RULE" in text
    assert "INVISIBLE" not in text


def test_justification_reports_the_strongest_primary_hypothesis():
    decision = study.decide_recommendation(
        [_cfg(bh_reject=False, p_cluster=0.061),
         _cfg(config_id="other", bh_reject=False, p_cluster=0.4)],
        {"observed_separation_pp_by_pool": {"primary": {}}})
    assert "0.0610" in decision["justification"]


# ---------------------------------------------------------------------------
# Warm-up audit scoping.
# ---------------------------------------------------------------------------

def test_warmup_leads_use_each_dataset_s_own_earliest_scored_window():
    # SOL listed in Sept 2020, so its 2020H2 cell is dropped. Auditing it
    # against 2020-07-01 anyway would report a shortfall for a window it never
    # scores, and the report would claim a populated NaN bucket over an empty
    # table.
    frames = {("SOL/USDT", "4h"): _frame("2020-09-18", "2026-01-01", "4h")}
    coverage = {"cells": {"SOL/USDT 4h|2020H2": False,
                          "SOL/USDT 4h|2021": True}}
    leads = study.scored_warmup_leads(frames, coverage, ["2020H2", "2021"])
    # Lead is measured to 2021-01-01, which SOL genuinely has bars before.
    assert leads["SOL/USDT 4h"] > 0
    audit = study.warmup_audit(leads, [128])
    assert audit["sufficient"]


def test_warmup_leads_skip_a_dataset_with_no_scored_cell():
    frames = {("LINK/USDT", "4h"): _frame("2022-01-14", "2026-01-01", "4h")}
    coverage = {"cells": {"LINK/USDT 4h|2021": False}}
    assert study.scored_warmup_leads(frames, coverage, ["2021"]) == {}


def test_warmup_leads_still_flag_a_genuine_shortfall():
    frames = {("BTC/USDT", "4h"): _frame("2020-12-25", "2026-01-01", "4h")}
    coverage = {"cells": {"BTC/USDT 4h|2021": True}}
    leads = study.scored_warmup_leads(frames, coverage, ["2021"])
    assert not study.warmup_audit(leads, [512])["sufficient"]


# ---------------------------------------------------------------------------
# Benjamini-Hochberg cohort isolation.
# ---------------------------------------------------------------------------

def test_bh_families_never_share_a_denominator():
    # ONE small p among otherwise-null hypotheses: 0.008 clears the rank-1 bar
    # 0.05/4 in a 4-hypothesis family but not 0.05/30 in a 30-hypothesis one.
    # Pooling the cohorts would let the exploratory grid borrow the primary
    # family's protection, or bury the primary result under 30 denominators.
    primary = ([_cfg(config_id="p0", cohort=study.COHORT_PRIMARY, p_cluster=0.008)]
               + [_cfg(config_id=f"p{i}", cohort=study.COHORT_PRIMARY,
                       p_cluster=0.9) for i in range(1, 4)])
    expl = ([_cfg(config_id="e0", cohort=study.COHORT_EXPLORATORY, p_cluster=0.008)]
            + [_cfg(config_id=f"e{i}", cohort=study.COHORT_EXPLORATORY,
                    p_cluster=0.9) for i in range(1, 30)])
    study.apply_bh_by_cohort(primary + expl)
    assert primary[0]["bh_reject"]
    assert not any(c["bh_reject"] for c in expl)


def test_bh_counts_untestable_configs_in_the_denominator():
    testable = [_cfg(config_id="a", p_cluster=0.02, bh_reject=False)]
    untestable = [_cfg(config_id=f"u{i}", p_cluster=None, bh_reject=False)
                  for i in range(20)]
    study.apply_bh_by_cohort(testable + untestable)
    # 0.02 clears 0.05/1 but not 0.05/21 — dropping the untestable configs must
    # never make the correction more permissive.
    assert not testable[0]["bh_reject"]
    assert not any(c["bh_reject"] for c in untestable)


def test_bh_marks_every_config_even_with_nothing_testable():
    cfgs = [_cfg(config_id=f"u{i}", p_cluster=None) for i in range(3)]
    study.apply_bh_by_cohort(cfgs)
    assert all(c["bh_reject"] is False for c in cfgs)


# ---------------------------------------------------------------------------
# Dedup, rendering, and the #1410 contract-path regression.
# ---------------------------------------------------------------------------

def test_dedup_is_independent_of_scheduling_order():
    rows = [
        {"strategy": "m", "symbol": "BTC/USDT", "timeframe": "1h",
         "window": "2021", "entry_date": "2021-05-01", "tag": "new"},
        {"strategy": "m", "symbol": "BTC/USDT", "timeframe": "1h",
         "window": "is", "entry_date": "2021-05-01", "tag": "old"},
    ]
    order = study.WINDOW_ORDER
    a = study.dedup_entries(rows, order)
    b = study.dedup_entries(list(reversed(rows)), order)
    assert len(a) == len(b) == 1
    assert a[0]["tag"] == b[0]["tag"] == "new"  # chronologically first window wins


def test_dedup_keeps_distinct_datasets_that_share_a_timestamp():
    rows = [
        {"strategy": "m", "symbol": "BTC/USDT", "timeframe": "1h",
         "window": "2021", "entry_date": "2021-05-01"},
        {"strategy": "m", "symbol": "ETH/USDT", "timeframe": "1h",
         "window": "2021", "entry_date": "2021-05-01"},
    ]
    assert len(study.dedup_entries(rows, study.WINDOW_ORDER)) == 2


def test_report_is_a_pure_function_of_the_payload():
    payload = _render_payload()
    assert study.report_from_payload(payload) == study.report_from_payload(payload)


def test_recommendation_is_the_final_section():
    text = study.report_from_payload(_render_payload())
    assert text.rstrip().split("## ")[-1].startswith("Recommendation")


def test_inconclusive_report_blames_its_own_power_when_the_edge_is_unresolvable():
    text = study.report_from_payload(_render_payload()).replace("\n", " ")
    assert "INCONCLUSIVE" in text
    assert "not about the market" in text
    assert "Do not read it as evidence that no edge exists" in text
    assert "change the RULE" not in text


def test_inconclusive_report_blames_the_rule_when_the_edge_is_resolvable():
    # The closing guidance must follow the same branch as the justification. A
    # fixed "no edge exists" sign-off over a run that measured a resolvable
    # separation would tell the reader the opposite of the report's own table.
    payload = _render_payload()
    payload["mde"] = dict(
        payload["mde"], pooled_primary_cluster=0.08,
        pooled_primary_cluster_p0=0.021,
        observed_separation_pp_by_pool={"primary": {"momentum|512": 0.45}})
    payload["decision"] = study.decide_recommendation(
        payload["configs"], payload["mde"])
    text = study.report_from_payload(payload)
    assert "change the RULE" in text
    assert "not about the market" not in text
    assert "re-register its hypotheses" in text


def test_report_reads_a_resolvable_pool_s_uninjected_p_as_exploratory():
    # A better-powered pool outside the confirmatory cohort carries real
    # information the primary test cannot supply. Reporting it is right;
    # letting it read as confirmatory would defeat the cohort split.
    payload = _render_payload()
    payload["mde"] = dict(
        payload["mde"], pooled_exploratory_cluster=0.4,
        pooled_exploratory_cluster_p0=0.49,
        observed_separation_pp_by_pool={"primary": {"momentum|512": 0.1},
                                        "exploratory": {"momentum|512": 0.99}})
    payload["decision"] = study.decide_recommendation(
        payload["configs"], payload["mde"])
    text = study.report_from_payload(payload).replace("\n", " ")
    assert "EXPLORATORY" in text
    assert "0.4900" in text
    assert "licenses no gate" in text


def test_report_omits_the_exploratory_reading_when_no_pool_resolves():
    payload = _render_payload()
    payload["mde"] = dict(
        payload["mde"], pooled_1410_cluster=9.0, pooled_primary_cluster=9.0,
        pooled_exploratory_cluster=9.0,
        observed_separation_pp_by_pool={"1410": {"momentum|512": 0.1},
                                        "primary": {"momentum|512": 0.1},
                                        "exploratory": {"momentum|512": 0.1}})
    payload["decision"] = study.decide_recommendation(
        payload["configs"], payload["mde"])
    text = study.report_from_payload(payload)
    assert "EXPLORATORY" not in text
    assert "stays DEFAULT-OFF" in text


def test_inconclusive_report_never_licenses_shipping_a_threshold():
    for payload in (_render_payload(),):
        assert "stays DEFAULT-OFF" in study.report_from_payload(payload)


def test_report_prints_the_uninjected_p_beside_the_detection_limit():
    text = study.report_from_payload(_render_payload())
    assert "p at zero injection" in text


def test_report_prints_effective_n_beside_nominal():
    text = study.report_from_payload(_render_payload())
    assert "Pooled N (eff)" in text
    assert "sup/kept eff" in text


def test_report_prints_both_p_values():
    text = study.report_from_payload(_render_payload())
    assert "free p" in text and "cluster p" in text


def test_report_states_whether_any_rows_left_the_cluster_contrast():
    clean = _render_payload()
    assert "no rows were dropped from any cluster contrast" in \
        study.report_from_payload(clean)
    dirty = _render_payload()
    for cfg in dirty["configs"]:
        cfg["cluster_excluded_datasets"] = ["DOGE/USDT 1h"]
        cfg["cluster_excluded_trades"] = 42
    text = study.report_from_payload(dirty)
    assert "`DOGE/USDT 1h`" in text and "42 rows" in text


def test_report_prints_the_no_joint_separation_token():
    text = study.report_from_payload(_render_payload())
    assert study.NO_JOINT_SEPARATION in text


def test_no_superseded_study_defaults_to_the_contract_path():
    # The regression this guards: a --render-only run of a superseded study
    # silently reverting the live evidence file to its old verdict. #1424 owns
    # the contract path now, so BOTH #1410 and #1422 must point elsewhere.
    assert os.path.basename(study1410._DEFAULT_REPORT_OUT) == \
        "hurst_1410_gate_calibration.md"
    assert os.path.basename(study._DEFAULT_REPORT_OUT) == \
        "hurst_1422_gate_power.md"
    assert os.path.basename(study1410._DEFAULT_REPORT_OUT) != \
        os.path.basename(study._DEFAULT_REPORT_OUT)
    for module in (study1410, study):
        assert os.path.basename(module._DEFAULT_REPORT_OUT) != \
            "hurst_gate_calibration.md"


def test_1422_refuses_to_write_the_contract_path_even_when_asked(tmp_path):
    # Changing the default alone leaves `--report-out <contract path>` as a
    # one-flag revert of the live evidence. The refusal must fire before any
    # scoping check, so a COMPLETE run is refused too.
    contract = os.path.join(os.path.dirname(study._DEFAULT_REPORT_OUT),
                            "hurst_gate_calibration.md")
    with pytest.raises(SystemExit) as exc:
        study.main(["--report-out", contract,
                    "--json-out", str(tmp_path / "scoped.json")])
    assert "SUPERSEDED" in str(exc.value)


def test_1422_render_marks_itself_superseded():
    text = study.report_from_payload(_render_payload())
    assert "SUPERSEDED by the #1424 resolution study" in text
    assert "hurst_1424_gate_resolution.py" in text


# ---------------------------------------------------------------------------
# The committed JSON and the contract report belong to the full design.
# ---------------------------------------------------------------------------

def test_scoped_run_may_not_overwrite_the_committed_json():
    with pytest.raises(SystemExit) as exc:
        study.main(["--only", "momentum"])
    assert "committed aggregate" in str(exc.value)


@pytest.mark.parametrize("flag,value", [("--only", "momentum"),
                                        ("--datasets", "BTC/USDT:1h"),
                                        ("--windows", "2021"),
                                        ("--hurst-windows", "128")])
def test_every_scoping_flag_protects_the_contract_report(tmp_path, flag, value):
    with pytest.raises(SystemExit) as exc:
        study.main([flag, value, "--json-out", str(tmp_path / "scoped.json")])
    assert "contract path" in str(exc.value)


def test_scoped_run_is_allowed_on_explicit_paths(tmp_path, monkeypatch):
    # The guard must protect the two committed paths, never block scoped work.
    # A sentinel raised at the first step past the guard proves it let the run
    # through, without starting a real scoring run.
    class _Reached(Exception):
        pass

    def _boom(raw):
        raise _Reached()

    monkeypatch.setattr(study, "_parse_datasets", _boom)
    with pytest.raises(_Reached):
        study.main(["--only", "momentum",
                    "--json-out", str(tmp_path / "scoped.json"),
                    "--report-out", str(tmp_path / "scoped.md")])


def test_render_only_refuses_an_unstamped_payload_on_the_contract_path(tmp_path):
    payload = _render_payload()
    payload["run_summary"].pop("scope", None)
    src = tmp_path / "unstamped.json"
    src.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--write-report", "--json-out", str(src)])
    assert "not stamped as a complete run" in str(exc.value)


def test_render_only_refuses_a_scoped_payload_on_the_contract_path(tmp_path):
    payload = _render_payload()
    payload["run_summary"]["scope"] = {"complete": False, "only": "momentum"}
    src = tmp_path / "scoped.json"
    src.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--write-report", "--json-out", str(src)])
    assert "not stamped as a complete run" in str(exc.value)


def test_render_only_needs_write_report_for_the_contract_path(tmp_path):
    payload = _render_payload()
    payload["run_summary"]["scope"] = {"complete": True}
    src = tmp_path / "complete.json"
    src.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(["--render-only", "--json-out", str(src)])
    assert "--write-report" in str(exc.value)


def test_render_only_writes_a_non_contract_path_freely(tmp_path):
    payload = _render_payload()
    payload["run_summary"]["scope"] = {"complete": False, "only": "momentum"}
    src = tmp_path / "scoped.json"
    src.write_text(json.dumps(payload))
    out = tmp_path / "scoped.md"
    assert study.main(["--render-only", "--json-out", str(src),
                       "--report-out", str(out)]) == 0
    assert out.read_text().startswith("# Hurst gate power study")


# ---------------------------------------------------------------------------
# Per-family anti-signal orientation.
# ---------------------------------------------------------------------------

def test_anti_signal_side_follows_the_family_sense():
    # momentum arms on high H, so its gate suppresses LOW H; mean_reversion is
    # the mirror. A single split for both would inject the edge backwards for
    # one of them.
    assert study.anti_signal_side(0.3, study.SENSE_HIGH) is True
    assert study.anti_signal_side(0.7, study.SENSE_HIGH) is False
    assert study.anti_signal_side(0.3, study.SENSE_LOW) is False
    assert study.anti_signal_side(0.7, study.SENSE_LOW) is True
    assert study.anti_signal_side(0.5, study.SENSE_HIGH) is False
    assert study.anti_signal_side(0.5, study.SENSE_LOW) is True


def test_anti_signal_side_covers_every_declared_family():
    for family in study.FAMILIES:
        sense = study.FAMILY_SENSE[family]
        assert study.anti_signal_side(0.2, sense) != study.anti_signal_side(0.8, sense)


def test_anti_signal_side_rejects_an_unknown_sense():
    # A silent default would invert an injected contrast without a trace.
    with pytest.raises(ValueError):
        study.anti_signal_side(0.4, "arms_on_tuesdays")


def test_separation_is_kept_minus_suppressed():
    # Same orientation as the injected edge in `min_detectable_effect`, so a
    # separation and a limit measured on the same rows are comparable at all.
    sep = study._separation([10.0, 10.0, 4.0, 4.0], [False, False, True, True])
    assert sep == 6.0


def test_separation_is_none_when_the_split_is_one_sided():
    assert study._separation([1.0, 2.0], [False, False]) is None
    assert study._separation([1.0, 2.0], [True, True]) is None
    assert study._separation([], []) is None


def test_every_pool_reports_the_separation_measured_on_its_own_rows():
    # The primary and exploratory cohorts are DISJOINT samples, so each one's
    # detection limit may only be read against its own separation. Publishing a
    # single whole-study separation beside three per-pool limits invites exactly
    # the mismatched comparison this field exists to prevent.
    pooled = {"momentum": [], "mean_reversion": []}
    for i in range(240):
        bad = _blocky(i)
        pooled["mean_reversion"].append(
            _trade(day=i * 3, pnl=-4.0 if bad else 4.0,
                   h=0.7 if bad else 0.3, adx=40.0))
    out = study._measure_detection_limits(pooled, [512], "x.json", 800, 1)
    by_pool = out["observed_separation_pp_by_pool"]
    assert set(by_pool) == {"1410", "primary", "exploratory"}
    for label in by_pool:
        assert f"pooled_{label}_cluster" in out


def test_detection_limit_pool_drops_short_rows_from_every_compared_number(
        monkeypatch):
    long = [
        _trade(symbol="BTC/USDT", day=i * 3,
               pnl=-2.0 if i % 2 else 2.0,
               h=0.3 if i % 2 else 0.7,
               cohort=study.COHORT_PRIMARY)
        for i in range(120)
    ]
    short = [
        _trade(symbol="DOGE/USDT", day=i, pnl=-50.0, h=0.3,
               cohort=study.COHORT_PRIMARY)
        for i in range(30)
    ]
    calls = []

    def fake_mde(trades, values, suppressed, family_size, *, cluster,
                 n_perm, seed, **kwargs):
        calls.append(("cluster" if cluster else "free", list(trades),
                      list(values), list(suppressed)))
        return 0.5 if trades and any(suppressed) and not all(suppressed) else None

    def fake_cluster(trades, values, suppressed, *, n_perm, seed):
        calls.append(("cluster_p0", list(trades), list(values),
                      list(suppressed)))
        return {"p": 0.2}

    def fake_free(values, suppressed, *, n_perm, seed):
        calls.append(("free_p0", [], list(values), list(suppressed)))
        return 0.3

    monkeypatch.setattr(study, "min_detectable_effect", fake_mde)
    monkeypatch.setattr(study, "cluster_permutation_pvalue_group_diff",
                        fake_cluster)
    monkeypatch.setattr(study, "permutation_pvalue_group_diff", fake_free)

    with_short = study._measure_detection_limits(
        {"momentum": long + short, "mean_reversion": []}, [512], "x.json",
        800, 1)
    without_short = study._measure_detection_limits(
        {"momentum": long, "mean_reversion": []}, [512], "x.json", 800, 1)

    assert with_short["observed_separation_pp_by_pool"]["primary"] == \
        without_short["observed_separation_pp_by_pool"]["primary"] == \
        {"momentum|512": 4.0}
    for key in ("pooled_primary_cluster", "pooled_primary_free",
                "pooled_primary_cluster_p0", "pooled_primary_free_p0"):
        assert with_short[key] == without_short[key]
    # Every non-empty statistical call sees the same usable BTC-only pool;
    # DOGE cannot survive in a free limit or p0 after the cluster null drops it.
    assert calls
    assert all(t["symbol"] != "DOGE/USDT"
               for _, trades, _, _ in calls for t in trades)
    assert all(-50.0 not in values for _, _, values, _ in calls if values)


def test_detection_limit_separation_is_absent_when_exclusion_removes_a_side(
        monkeypatch):
    kept = [
        _trade(symbol="BTC/USDT", day=i * 3, pnl=2.0, h=0.7,
               cohort=study.COHORT_PRIMARY)
        for i in range(120)
    ]
    dropped_suppressed = [
        _trade(symbol="DOGE/USDT", day=i, pnl=-50.0, h=0.3,
               cohort=study.COHORT_PRIMARY)
        for i in range(30)
    ]
    monkeypatch.setattr(study, "min_detectable_effect",
                        lambda *args, **kwargs: None)
    out = study._measure_detection_limits(
        {"momentum": kept + dropped_suppressed, "mean_reversion": []},
        [512], "x.json", 800, 1)
    assert "momentum|512" not in \
        out["observed_separation_pp_by_pool"]["primary"]


def test_report_marks_a_sub_limit_pool_as_unresolvable():
    payload = _render_payload()
    payload["mde"] = dict(
        payload["mde"], pooled_primary_cluster=2.0,
        observed_separation_pp_by_pool={"1410": {"momentum|512": 0.1},
                                        "primary": {"momentum|512": 0.1},
                                        "exploratory": {"momentum|512": 0.1}})
    text = study.report_from_payload(payload)
    assert "Largest separation ON THAT POOL" in text
    assert "| NO |" in text
    assert "the design cannot see an effect that small" in text


def test_report_marks_a_resolvable_pool_as_resolvable():
    payload = _render_payload()
    payload["mde"] = dict(
        payload["mde"], pooled_primary_cluster=0.05,
        observed_separation_pp_by_pool={"1410": {"momentum|512": 9.0},
                                        "primary": {"momentum|512": 9.0},
                                        "exploratory": {"momentum|512": 9.0}})
    text = study.report_from_payload(payload)
    assert "| yes |" in text


def test_report_states_the_joint_p_and_limit_share_one_resolution():
    text = study.report_from_payload(_render_payload())
    assert ("significance p and contrast-local detection limit both use the "
            "inference resolution of 100 draws and seed 1422") in text
    assert "lower 50-draw general MDE resolution" in text


def test_detection_limit_split_orients_each_family_separately():
    # mean_reversion alone: its suppressed side is H >= 0.5, so a pool where the
    # HIGH-H trades are the bad ones must show a POSITIVE observed separation.
    pooled = {"momentum": [], "mean_reversion": []}
    for i in range(240):
        bad = _blocky(i)
        pooled["mean_reversion"].append(
            _trade(day=i * 3, pnl=-4.0 if bad else 4.0,
                   h=0.7 if bad else 0.3, adx=40.0))
    out = study._measure_detection_limits(pooled, [512], "x.json", 800, 1)
    assert out["observed_separation_pp"]["mean_reversion|512"] > 0


def _render_payload() -> dict:
    cfg_p = _cfg(bh_reject=False, p_cluster=0.4)
    cfg_e = _cfg(config_id="momentum/gate/W128/arm0.55/dis0.5",
                 cohort=study.COHORT_EXPLORATORY, bh_reject=False,
                 p_cluster=0.5,
                 protocol_windows=list(study.EXPLORATORY_PROTOCOL_WINDOWS),
                 held_out_windows=list(study.EXPLORATORY_HELD_OUT_WINDOWS))
    decision = study.decide_recommendation(
        [cfg_p, cfg_e], {"pooled_1410_cluster": 3.0, "pooled_primary_cluster": 2.0,
                         "observed_separation_pp_by_pool": {
                             "primary": {"momentum|512": 0.1}}})
    empty_bucket = {b: {"trades": 0, "win_rate_pct": None,
                        "mean_pnl_pct_net": None, "median_pnl_pct_net": None,
                        "compounded_return_pct": 0.0, "trade_seq_max_dd_pct": 0.0,
                        "chop_loss_pct": 0.0} for b in study.BUCKETS}
    return {
        "schema_version": study.SCHEMA_VERSION,
        "issue": study.ISSUE,
        "pre_registered": {
            "hurst_windows": [512],
            "history_since": study.HISTORY_SINCE,
            "platform": study.PLATFORM, "fee_platform": study.FEE_PLATFORM,
            "datasets": ["BTC/USDT 1h"],
            "windows": {"2021": ["2021-01-01", "2022-01-01"]},
            "n_perm": 100, "n_perm_mde": 50, "seed": study.SEED,
        },
        "run_summary": {
            "legs": 2, "gated_arms": 18, "mirror_verified_legs": 2,
            "raw_trades": {f: 0 for f in study.FAMILIES},
            "pooled_trades": {f: 0 for f in study.FAMILIES},
            "pooled_primary": {f: 0 for f in study.FAMILIES},
            "pooled_exploratory": {f: 0 for f in study.FAMILIES},
            "n_primary_configs": 1, "n_exploratory_configs": 1,
            "n_primary_significant": 0, "n_exploratory_significant": 0,
            "warmup": {"required_bars": 514, "min_lead_bars": 5000,
                       "sufficient": True, "insufficient_datasets": [],
                       "lead_bars": {}},
            "coverage": {"n_cells": 1, "n_kept": 1, "n_dropped": 0,
                         "required_lead_bars": 514,
                         "min_window_bar_fraction": study.MIN_WINDOW_BAR_FRACTION,
                         "cells": {}, "dropped": []},
            "symbol_correlations": {"BTC/USDT|ETH/USDT": 0.8},
            "elapsed_sec": 1.0,
        },
        "mde": {"pooled_1410_cluster": 3.0, "pooled_1410_free": 2.5,
                "pooled_1410_cluster_p0": 0.3, "pooled_1410_free_p0": 0.2,
                "pooled_primary_cluster": 2.0, "pooled_primary_free": 1.8,
                "pooled_primary_cluster_p0": 0.4, "pooled_primary_free_p0": 0.3,
                "pooled_exploratory_cluster": None,
                "pooled_exploratory_free": None,
                "pooled_exploratory_cluster_p0": None,
                "pooled_exploratory_free_p0": None,
                "by_family_cluster": {f: 2.0 for f in study.FAMILIES},
                "observed_separation_pp": {"momentum|512": 0.1},
                "observed_separation_pp_by_pool": {
                    "1410": {"momentum|512": 0.1},
                    "primary": {"momentum|512": 0.1},
                    "exploratory": {"momentum|512": 0.1}}},
        "buckets": {f: {"512": dict(empty_bucket)} for f in study.FAMILIES},
        "joint": {f: {"table": study.joint_adx_hurst_table([], 512),
                      "verdict": {"separated": False, "reason": "no contrast",
                                  "p_cluster": None, "delta_mean_pp": None,
                                  "mde_pp": 2.0}}
                  for f in study.FAMILIES},
        "configs": [cfg_p, cfg_e],
        "legs": [],
        "decision": decision,
    }
