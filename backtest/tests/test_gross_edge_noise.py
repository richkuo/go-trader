"""Unit tests for the M1 step-2 gross-edge noise adjudicator (#1054).

The statistics layer is pure (lists of floats / plain dicts, stdlib-only,
seeded) so it is tested without data access; the run_leg keep_trades plumbing
is exercised end-to-end on a synthetic frame with the cache monkeypatched —
same architecture as test_eval_windows / test_exit_policy_ab.
"""

import numpy as np
import pandas as pd
import pytest

import eval_windows as ew
import gross_edge_noise as gen


# ---------------------------------------------------------------------------
# summarize_returns
# ---------------------------------------------------------------------------

def test_summarize_returns_empty():
    s = gen.summarize_returns([])
    assert s["n"] == 0 and s["mean"] is None and s["median"] is None
    assert s["n_pos"] == 0 and s["n_neg"] == 0 and s["n_zero"] == 0


def test_summarize_returns_mixed():
    s = gen.summarize_returns([1.0, -2.0, 0.0, 3.0])
    assert s["n"] == 4
    assert s["mean"] == pytest.approx(0.5)
    assert s["median"] == pytest.approx(0.5)
    assert s["min"] == -2.0 and s["max"] == 3.0
    assert s["n_pos"] == 2 and s["n_neg"] == 1 and s["n_zero"] == 1


# ---------------------------------------------------------------------------
# sign_flip_permutation
# ---------------------------------------------------------------------------

def test_permutation_empty_sample_is_p_one():
    r = gen.sign_flip_permutation([])
    assert r["p_value"] == 1.0 and r["n"] == 0 and r["mean"] is None


def test_permutation_strong_positive_sample_is_significant():
    # 20 identical positive returns: only the all-heads flip (p=0.5^20)
    # reaches the observed mean — permutation p must be tiny.
    r = gen.sign_flip_permutation([1.0] * 20, n_resamples=2000, seed=7)
    assert r["mean"] == pytest.approx(1.0)
    assert r["p_value"] < 0.01


def test_permutation_symmetric_sample_is_not_significant():
    # Mean 0 sample: every flip is >= observed mean about half the time.
    vals = [1.0, -1.0, 2.0, -2.0, 0.5, -0.5]
    r = gen.sign_flip_permutation(vals, n_resamples=2000, seed=7)
    assert r["mean"] == pytest.approx(0.0)
    assert r["p_value"] > 0.2


def test_permutation_deterministic_under_seed():
    vals = [0.4, -0.2, 1.1, -0.7, 0.3]
    a = gen.sign_flip_permutation(vals, n_resamples=500, seed=1066)
    b = gen.sign_flip_permutation(vals, n_resamples=500, seed=1066)
    assert a == b
    c = gen.sign_flip_permutation(vals, n_resamples=500, seed=2077)
    assert c["n_resamples"] == a["n_resamples"]  # different seed still runs


def test_permutation_p_never_zero():
    # Add-one smoothing: even a sample no flip can match keeps p > 0.
    r = gen.sign_flip_permutation([5.0] * 10, n_resamples=100, seed=1)
    assert 0.0 < r["p_value"] <= 1.0


# ---------------------------------------------------------------------------
# bootstrap_p_mean_le_zero
# ---------------------------------------------------------------------------

def test_bootstrap_p_le_zero_empty_is_none():
    assert gen.bootstrap_p_mean_le_zero([]) is None


def test_bootstrap_p_le_zero_single_value_uses_point():
    assert gen.bootstrap_p_mean_le_zero([2.0]) == 0.0
    assert gen.bootstrap_p_mean_le_zero([-2.0]) == 1.0
    assert gen.bootstrap_p_mean_le_zero([0.0]) == 1.0


def test_bootstrap_p_le_zero_all_positive_is_zero():
    assert gen.bootstrap_p_mean_le_zero([1.0, 2.0, 3.0],
                                        n_resamples=500, seed=3) == 0.0


def test_bootstrap_p_le_zero_deterministic():
    vals = [0.4, -0.2, 1.1, -0.7, 0.3]
    a = gen.bootstrap_p_mean_le_zero(vals, n_resamples=500, seed=1066)
    b = gen.bootstrap_p_mean_le_zero(vals, n_resamples=500, seed=1066)
    assert a == b


# ---------------------------------------------------------------------------
# dedupe_samples
# ---------------------------------------------------------------------------

def test_dedupe_drops_same_dataset_same_entry():
    samples = [
        {"dataset": "BTC/USDT 1h", "entry_date": "2025-06-15", "pnl_pct": 1.0},
        {"dataset": "BTC/USDT 1h", "entry_date": "2025-06-15", "pnl_pct": 1.0},
        {"dataset": "ETH/USDT 1h", "entry_date": "2025-06-15", "pnl_pct": 2.0},
    ]
    out, dropped = gen.dedupe_samples(samples)
    assert dropped == 1
    assert len(out) == 2
    # First occurrence wins; distinct datasets sharing a timestamp both stay.
    assert out[0]["dataset"] == "BTC/USDT 1h"
    assert out[1]["dataset"] == "ETH/USDT 1h"


def test_dedupe_keeps_distinct_entries():
    samples = [
        {"dataset": "BTC/USDT 1h", "entry_date": "2025-06-15", "pnl_pct": 1.0},
        {"dataset": "BTC/USDT 1h", "entry_date": "2025-06-16", "pnl_pct": -1.0},
    ]
    out, dropped = gen.dedupe_samples(samples)
    assert dropped == 0 and len(out) == 2


# ---------------------------------------------------------------------------
# noise_verdict
# ---------------------------------------------------------------------------

def test_verdict_non_positive_mean_is_no_edge():
    assert gen.noise_verdict(0.0, 0.001) == gen.VERDICT_NO_EDGE
    assert gen.noise_verdict(-0.5, 0.001) == gen.VERDICT_NO_EDGE
    assert gen.noise_verdict(None, 0.001) == gen.VERDICT_NO_EDGE


def test_verdict_significant_positive_is_distinguishable():
    assert gen.noise_verdict(0.3, 0.01) == gen.VERDICT_DISTINGUISHABLE


def test_verdict_insignificant_positive_is_indistinguishable():
    assert gen.noise_verdict(0.3, 0.39) == gen.VERDICT_INDISTINGUISHABLE
    # Boundary: p == alpha is NOT below alpha.
    assert gen.noise_verdict(0.3, 0.05) == gen.VERDICT_INDISTINGUISHABLE


# ---------------------------------------------------------------------------
# analyze_sample
# ---------------------------------------------------------------------------

def test_analyze_sample_verdict_matches_primary_test():
    strong = gen.analyze_sample([1.0] * 20, n_resamples=1000, seed=5)
    assert strong["verdict"] == gen.VERDICT_DISTINGUISHABLE
    assert strong["summary"]["n"] == 20
    noisy = gen.analyze_sample([1.0, -1.0, 0.5, -0.5, 0.2],
                               n_resamples=1000, seed=5)
    assert noisy["verdict"] in (gen.VERDICT_INDISTINGUISHABLE,
                                gen.VERDICT_NO_EDGE)
    for key in ("summary", "permutation", "bootstrap",
                "bootstrap_p_mean_le_zero", "sign_test", "wilcoxon",
                "alpha", "verdict"):
        assert key in strong


# ---------------------------------------------------------------------------
# _entry_in_range
# ---------------------------------------------------------------------------

def test_entry_in_range_bare_date_bounds_vs_timestamps():
    rng = ("2025-06-10", "2025-07-01")
    # Bare-date start precedes the day's first timestamp; end is exclusive.
    assert gen._entry_in_range("2025-06-10 00:00:00", rng)
    assert gen._entry_in_range("2025-06-30 23:00:00", rng)
    assert not gen._entry_in_range("2025-07-01 00:00:00", rng)
    assert not gen._entry_in_range("2025-06-09 23:00:00", rng)


def test_entry_in_range_open_ended():
    assert gen._entry_in_range("2099-01-01 00:00:00", ("2026-01-01", None))
    assert not gen._entry_in_range("2025-12-31 23:00:00", ("2026-01-01", None))


# ---------------------------------------------------------------------------
# pool_trade_samples — exact-key AND calendar-coverage guards
# ---------------------------------------------------------------------------

def test_pool_exact_duplicate_still_dropped():
    # Must-survive (b): warmup converged, overlap entries byte-identical.
    legs = [
        {"dataset": "BTC/USDT 1h", "window": "is",
         "trade_samples": [{"entry_date": "2025-08-15 04:00:00", "pnl_pct": 1.0}]},
        {"dataset": "BTC/USDT 1h", "window": "is",
         "trade_samples": [{"entry_date": "2025-08-15 04:00:00", "pnl_pct": 1.0}]},
        {"dataset": "ETH/USDT 1h", "window": "is", "trade_samples": []},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs)
    assert n_exact == 1 and n_overlap == 0
    assert len(samples) == 1
    assert samples[0]["window"] == "is"


def test_pool_overlap_nonidentical_entries_dropped():
    """The reviewed defect (#1172): per-window warmup divergence makes overlap
    entries NON-identical, so the exact key never fires — the calendar-coverage
    guard must drop the later window's entry inside the earlier window's range."""
    legs = [
        # is = 2025-06-10 → 2026-01-01 claims BTC 1h coverage first.
        {"dataset": "BTC/USDT 1h", "window": "is",
         "trade_samples": [{"entry_date": "2025-06-17 19:00:00", "pnl_pct": 1.37}]},
        # 2025H1 = 2025-01-01 → 2025-07-01: its 2025-06-18 entry sits inside
        # is's claimed range at a timestamp is never produced.
        {"dataset": "BTC/USDT 1h", "window": "2025H1",
         "trade_samples": [{"entry_date": "2025-06-18 21:00:00", "pnl_pct": 0.22},
                           {"entry_date": "2025-03-01 00:00:00", "pnl_pct": -0.5}]},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs)
    assert n_exact == 0 and n_overlap == 1
    assert [s["entry_date"] for s in samples] == [
        "2025-06-17 19:00:00", "2025-03-01 00:00:00"]


def test_pool_shared_start_windows_keep_distinct_entries():
    # Must-survive (a): shared start, different ends — entries beyond the
    # first window's range are genuinely distinct and must survive.
    windows = {"a": ("2023-01-01", "2024-01-01"),
               "b": ("2023-01-01", "2025-01-01")}
    legs = [
        {"dataset": "BTC/USDT 1h", "window": "a",
         "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 1.0}]},
        {"dataset": "BTC/USDT 1h", "window": "b",
         "trade_samples": [{"entry_date": "2023-06-01 00:00:00", "pnl_pct": 2.0},
                           {"entry_date": "2024-06-01 00:00:00", "pnl_pct": 3.0}]},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
    assert n_exact == 0 and n_overlap == 1  # b's 2023 entry covered by a
    assert [s["pnl_pct"] for s in samples] == [1.0, 3.0]


def test_pool_zero_trade_leg_still_claims_coverage():
    # A leg that sampled its period and chose not to trade still represents
    # it — a later overlapping window must not re-sample that period.
    windows = {"a": ("2023-01-01", "2024-01-01"),
               "b": ("2023-06-01", "2024-06-01")}
    legs = [
        {"dataset": "BTC/USDT 1h", "window": "a", "trade_samples": []},
        {"dataset": "BTC/USDT 1h", "window": "b",
         "trade_samples": [{"entry_date": "2023-08-01 00:00:00", "pnl_pct": 1.0},
                           {"entry_date": "2024-03-01 00:00:00", "pnl_pct": 2.0}]},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
    assert n_overlap == 1
    assert [s["pnl_pct"] for s in samples] == [2.0]


def test_pool_coverage_is_per_dataset():
    # Window a pooled only for BTC must not block ETH's window-b entries.
    windows = {"a": ("2023-01-01", "2024-01-01"),
               "b": ("2023-01-01", "2024-01-01")}
    legs = [
        {"dataset": "BTC/USDT 1h", "window": "a",
         "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 1.0}]},
        {"dataset": "ETH/USDT 1h", "window": "b",
         "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 2.0}]},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
    assert n_exact == 0 and n_overlap == 0
    assert len(samples) == 2


def test_pool_open_ended_window_claims_to_infinity():
    windows = {"oos": ("2026-01-01", None),
               "late": ("2026-06-01", "2027-01-01")}
    legs = [
        {"dataset": "BTC/USDT 1h", "window": "oos",
         "trade_samples": [{"entry_date": "2026-02-01 00:00:00", "pnl_pct": 1.0}]},
        {"dataset": "BTC/USDT 1h", "window": "late",
         "trade_samples": [{"entry_date": "2026-08-01 00:00:00", "pnl_pct": 2.0}]},
    ]
    samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
    assert n_overlap == 1
    assert [s["pnl_pct"] for s in samples] == [1.0]


# ---------------------------------------------------------------------------
# window_overlaps — leg-level disclosure
# ---------------------------------------------------------------------------

def test_window_overlaps_is_2025h1():
    out = gen.window_overlaps(["is", "oos", "2023", "2024", "2025H1"])
    assert len(out) == 1
    o = out[0]
    assert set(o["windows"]) == {"is", "2025H1"}
    assert o["start"] == "2025-06-10" and o["end"] == "2025-07-01"
    assert o["days"] == pytest.approx(21.0)


def test_window_overlaps_disjoint_windows_empty():
    assert gen.window_overlaps(["2023", "2024", "oos"]) == []
    assert gen.window_overlaps(["is", "oos"]) == []


# ---------------------------------------------------------------------------
# trade_samples_from_results / run_leg keep_trades plumbing
# ---------------------------------------------------------------------------

def test_trade_samples_from_results_extracts_entry_and_pnl():
    results = {"trades": [
        {"entry_date": "2025-06-15 04:00:00", "pnl_pct": 1.25, "pnl": 12.5},
        {"entry_date": "2025-06-20 09:00:00", "pnl_pct": -0.4, "pnl": -4.0},
    ]}
    out = ew.trade_samples_from_results(results)
    # pnl_pct_net (#1274) is additive; without notional it mirrors the gross.
    assert out == [
        {"entry_date": "2025-06-15 04:00:00", "pnl_pct": 1.25,
         "pnl_pct_net": 1.25},
        {"entry_date": "2025-06-20 09:00:00", "pnl_pct": -0.4,
         "pnl_pct_net": -0.4},
    ]
    assert ew.trade_samples_from_results({}) == []
    assert ew.trade_samples_from_results({"trades": None}) == []


class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {"period": 2},
                                        "description": "test"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1   # buy
        sig[20::20] = -1  # sell
        out["signal"] = sig
        return out


def _synthetic_df(n=120):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_run_leg_keep_trades_attaches_samples(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None), keep_trades=True)
    assert leg is not None and leg["trades"] > 0
    samples = leg["trade_samples"]
    assert len(samples) == leg["trades"]
    for s in samples:
        assert isinstance(s["entry_date"], str) and s["entry_date"]
        assert isinstance(s["pnl_pct"], float)


def test_run_leg_default_omits_trade_samples(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None))
    assert leg is not None
    assert "trade_samples" not in leg


def test_collect_gross_legs_zeroes_friction(monkeypatch):
    """The collected legs must be the fee audit's GROSS runs: on a synthetic
    frame the same signals must yield a higher (or equal) return than the
    fee-model run, and identical trade counts (all-in/all-out contract)."""
    df = _synthetic_df(n=240)
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    legs = gen.collect_gross_legs(_FakeRegistry(), "alternator", None,
                                  [("BTC/USDT", "1h")], ["oos"])
    assert len(legs) == 1
    gross = legs[0]
    net = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ew.WINDOWS["oos"])
    assert gross["trades"] == net["trades"]
    assert gross["return_pct"] >= net["return_pct"]
    assert gross["window"] == "oos"
    assert gross["dataset"] == "BTC/USDT 1h"
