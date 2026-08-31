import os
import sys

import numpy as np
import pandas as pd
import pytest

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import regime_enriched_features as ref
import regime_vol_model as rvm


def _synthetic_ohlcv(n=600, seed=0):
    rng = np.random.default_rng(seed)
    idx = pd.date_range("2022-01-01", periods=n, freq="1h")
    drift = np.where((np.arange(n) // 50) % 2 == 0, 0.002, 0.0)
    rets = drift + rng.normal(0, 0.01, size=n)
    close = 100 * np.cumprod(1 + rets)
    high = close * (1 + np.abs(rng.normal(0, 0.004, size=n)))
    low = close * (1 - np.abs(rng.normal(0, 0.004, size=n)))
    openp = np.concatenate([[close[0]], close[:-1]])
    volume = np.abs(rng.normal(1000, 200, size=n)) + (np.arange(n) // 50 % 2) * 500
    return pd.DataFrame({"open": openp, "high": np.maximum(high, np.maximum(openp, close)),
                         "low": np.minimum(low, np.minimum(openp, close)), "close": close,
                         "volume": volume}, index=idx)


def _synthetic_funding(idx, seed=1):
    rng = np.random.default_rng(seed)
    ts = (idx.as_unit("ns").asi8 // 1_000_000).astype("int64")
    rate = rng.normal(0.0001, 0.00005, size=len(idx))
    return pd.DataFrame({"timestamp": ts, "rate": rate})


def test_canonical_columns_lead_enriched_set():
    assert ref.ENRICHED_COLUMNS[:4] == ref.CANONICAL_COLUMNS
    assert ref.CANONICAL_INDICES == (0, 1, 2, 3)
    assert set(ref.ENRICHED_EXTRA_COLUMNS).isdisjoint(ref.CANONICAL_COLUMNS)


def test_builder_full_matrix_columns_and_order():
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund)
    assert list(mat.columns) == ref.ENRICHED_COLUMNS
    assert len(mat) == len(df)


def test_subset_selection_preserves_canonical_first_order():
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund,
                                      columns=["htf_range_eff", "adx", "return_eff",
                                               "funding_rate", "range_eff", "efficiency"])
    assert list(mat.columns) == ["return_eff", "range_eff", "efficiency", "adx",
                                 "funding_rate", "htf_range_eff"]


def test_builder_rejects_unknown_column():
    df = _synthetic_ohlcv()
    with pytest.raises(ValueError, match="unknown enriched columns"):
        ref.enriched_feature_matrix(df, period=48, columns=["return_eff", "rsi_14"])


def test_canonical_indices_for():
    assert ref.canonical_indices_for(ref.ENRICHED_COLUMNS) == (0, 1, 2, 3)
    cols = ["funding_rate", "return_eff", "range_eff", "efficiency", "adx"]
    assert ref.canonical_indices_for(cols) == (1, 2, 3, 4)
    with pytest.raises(ValueError, match="missing"):
        ref.canonical_indices_for(["return_eff", "range_eff", "adx"])


def test_enriched_matrix_is_causal_future_bars_do_not_change_past_rows():
    df = _synthetic_ohlcv(n=600)
    fund = _synthetic_funding(df.index)
    base = ref.enriched_feature_matrix(df, period=48, funding=fund, htf_multiple=4)
    t = 400
    mutated = df.copy()
    mutated.iloc[t + 1:, mutated.columns.get_loc("close")] *= 3.0
    mutated.iloc[t + 1:, mutated.columns.get_loc("high")] *= 3.0
    mutated.iloc[t + 1:, mutated.columns.get_loc("low")] *= 3.0
    mutated.iloc[t + 1:, mutated.columns.get_loc("open")] *= 3.0
    mutated.iloc[t + 1:, mutated.columns.get_loc("volume")] *= 9.0
    after = ref.enriched_feature_matrix(mutated, period=48, funding=fund, htf_multiple=4)
    a = base.iloc[: t + 1].to_numpy(dtype=float)
    b = after.iloc[: t + 1].to_numpy(dtype=float)
    assert np.array_equal(np.nan_to_num(a, nan=-7.0), np.nan_to_num(b, nan=-7.0))


def test_funding_column_is_backward_only():
    df = _synthetic_ohlcv(n=120)
    idx = df.index
    ts = (idx.as_unit("ns").asi8 // 1_000_000).astype("int64")
    fund = pd.DataFrame({"timestamp": [ts[0], ts[60]], "rate": [0.001, 0.005]})
    mat = ref.enriched_feature_matrix(df, period=24, funding=fund, columns=ref.CANONICAL_COLUMNS + ["funding_rate"])
    fr = mat["funding_rate"].to_numpy()
    assert fr[59] == pytest.approx(0.001)
    assert fr[60] == pytest.approx(0.005)
    assert fr[100] == pytest.approx(0.005)


def test_htf_feature_does_not_leak_in_progress_bar():
    df = _synthetic_ohlcv(n=600)
    mat = ref.enriched_feature_matrix(df, period=20, columns=ref.CANONICAL_COLUMNS + ["htf_range_eff"],
                                      htf_multiple=4)
    htf = mat["htf_range_eff"].to_numpy()
    mutated = df.copy()
    mutated.iloc[-1, mutated.columns.get_loc("high")] *= 5.0
    htf2 = ref.enriched_feature_matrix(mutated, period=20,
                                       columns=ref.CANONICAL_COLUMNS + ["htf_range_eff"],
                                       htf_multiple=4)["htf_range_eff"].to_numpy()
    assert np.array_equal(np.nan_to_num(htf[:-1], nan=-1.0), np.nan_to_num(htf2[:-1], nan=-1.0))


def test_volume_z_is_trailing_and_warmup_nan():
    df = _synthetic_ohlcv(n=200)
    mat = ref.enriched_feature_matrix(df, period=48, columns=ref.CANONICAL_COLUMNS + ["volume_z"],
                                      vol_window=24)
    vz = mat["volume_z"].to_numpy()
    assert np.isnan(vz[:23]).all()
    assert np.isfinite(vz[23])
    vol = df["volume"].to_numpy()
    i = 100
    w = vol[i - 23: i + 1]
    expected = (vol[i] - w.mean()) / w.std()
    assert vz[i] == pytest.approx(expected, rel=1e-9)


def test_hurst_column_is_trailing_and_warmup_nan():
    df = _synthetic_ohlcv(n=300)
    mat = ref.enriched_feature_matrix(df, period=48, columns=ref.CANONICAL_COLUMNS + ["hurst"],
                                      hurst_window=100)
    h = mat["hurst"].to_numpy()
    assert np.isnan(h[:100]).all()
    assert np.isfinite(h[100])
    from indicators_core import hurst_exponent
    close = df["close"].to_numpy()
    i = 200
    expected = hurst_exponent(pd.Series(close[i - 100: i + 1]), min_points=100)
    assert h[i] == pytest.approx(expected, rel=1e-12)


def test_hurst_column_uses_own_window_independent_of_period():
    df = _synthetic_ohlcv(n=300)
    mat_a = ref.enriched_feature_matrix(df, period=20, columns=ref.CANONICAL_COLUMNS + ["hurst"],
                                        hurst_window=100)
    mat_b = ref.enriched_feature_matrix(df, period=48, columns=ref.CANONICAL_COLUMNS + ["hurst"],
                                        hurst_window=100)
    pd.testing.assert_series_equal(mat_a["hurst"], mat_b["hurst"], check_exact=True)


def test_hurst_column_is_causal_future_bars_do_not_change_past_rows():
    df = _synthetic_ohlcv(n=300)
    base = ref.enriched_feature_matrix(df, period=48, columns=ref.CANONICAL_COLUMNS + ["hurst"])
    t = 200
    mutated = df.copy()
    mutated.iloc[t + 1:, mutated.columns.get_loc("close")] *= 3.0
    after = ref.enriched_feature_matrix(mutated, period=48, columns=ref.CANONICAL_COLUMNS + ["hurst"])
    a = base["hurst"].iloc[: t + 1].to_numpy()
    b = after["hurst"].iloc[: t + 1].to_numpy()
    assert np.array_equal(np.nan_to_num(a, nan=-7.0), np.nan_to_num(b, nan=-7.0))


def test_hurst_extra_column_appended_after_canonical_block():
    assert ref.ENRICHED_COLUMNS[-1] == "hurst"
    assert ref.ENRICHED_COLUMNS[:4] == ref.CANONICAL_COLUMNS
    assert "hurst" in ref.ENRICHED_EXTRA_COLUMNS


def test_fit_unsupervised_enriched_schema_and_decode():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    cols = ref.ENRICHED_COLUMNS
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund, columns=cols)
    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="hmm", k=4, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=ref.canonical_indices_for(cols))
    assert model["features"] == list(cols)
    assert model["canonical_indices"] == [0, 1, 2, 3]
    assert len(model["feature_means"]) == len(cols)
    assert all(len(e["mean"]) == len(cols) for e in model["emissions"])
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"])
    labels, conf = ref.decode_with_model(mat, model)
    assert len(labels) == len(mat)
    valid = ~np.isnan(mat.to_numpy(dtype=float)).any(1)
    assert set(np.asarray(labels, dtype=object)[valid]).issubset(VALID_LABELS_COMPOSITE)


def test_fit_unsupervised_rejects_feature_name_count_mismatch():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = np.random.default_rng(0).normal(size=(300, 5))
    with pytest.raises(ValueError, match="columns but feature_names lists"):
        rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=16, thresholds=dict(TH),
                             feature_names=["a", "b", "c"])


def test_decode_contract_rejects_column_reorder():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    cols = ref.ENRICHED_COLUMNS
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund, columns=cols)
    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="kmeans", k=4, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=(0, 1, 2, 3))
    reordered = mat[list(reversed(cols))]
    with pytest.raises(ValueError, match="feature-order contract violated"):
        ref.decode_with_model(reordered, model)
    with pytest.raises(ValueError, match="feature-order contract violated"):
        ref.assert_feature_contract(model, list(reversed(cols)))


def test_column_order_is_load_bearing_for_labels():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    from regime_hmm import forward_filter_labels
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    cols = ref.ENRICHED_COLUMNS
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund, columns=cols)
    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="hmm", k=5, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=(0, 1, 2, 3))
    correct, _ = forward_filter_labels(mat.to_numpy(dtype=float), model)
    swapped = mat[["adx", "range_eff", "efficiency", "return_eff", "funding_rate",
                   "volume_z", "htf_range_eff", "hurst"]]
    wrong, _ = forward_filter_labels(swapped.to_numpy(dtype=float), model)
    valid = ~np.isnan(mat.to_numpy(dtype=float)).any(1)
    assert list(np.asarray(correct)[valid]) != list(np.asarray(wrong)[valid])


def test_naming_ignores_extra_dims_given_same_canonical_centroids():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(6); std = np.ones(6)
    em_a = np.array([[0.0, 0.02, 0.1, 8.0, 0.0, 0.0],
                     [0.4, 0.5, 0.9, 40.0, 0.0, 0.0]], dtype=float)
    em_b = em_a.copy(); em_b[:, 4:] = 99.0
    names_a, _ = rvm.map_latent_to_names(em_a, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    names_b, _ = rvm.map_latent_to_names(em_b, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    assert names_a == names_b


def test_naming_with_canonical_columns_not_first():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(5); std = np.ones(5)
    em = np.array([[0.0, 0.0, 0.02, 0.1, 8.0],
                   [0.0, 0.4, 0.5, 0.9, 40.0]], dtype=float)
    names, _ = rvm.map_latent_to_names(em, mean, std, dict(TH), canonical_indices=(1, 2, 3, 4))
    assert names == ["ranging_quiet", "trending_up_clean"]


def test_map_latent_to_names_default_indices_unchanged_for_canonical_only():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    em = np.array([[0.0, 0.0, 0.1, 0.0], [0.5, 0.5, 0.9, 40.0]], dtype=float)
    names, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names == ["ranging_quiet", "trending_up_clean"]
    assert mapping["1"]["centroid_raw"] == [0.5, 0.5, 0.9, 40.0]


def _load_research(mod_name, filename):
    import importlib.util
    path = os.path.join(_BACKTEST, "research", filename)
    spec = importlib.util.spec_from_file_location(mod_name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_combined_family_plan_spans_all_subsets():
    m95 = _load_research("regime_1095_plan", "regime_1095_enriched_vol_model.py")
    m80 = _load_research("regime_1080_plan", "regime_1080_unsupervised_vol_model.py")
    th = rvm.NonDegeneracyThresholds(min_active_labels=4, max_occupancy=0.9,
                                     min_transition_rate=0.0)
    subsets = ["canonical", "volume", "htf"]
    plan, reasons, denom = m95.combined_family_plan(
        subsets, ("kmeans", "hmm"), range(3, 6), th,
        ineligible_reason_fn=m80.structurally_ineligible_reason)
    assert len(plan) == 3 * 2 * 3
    assert denom == 3 * 2 * 2
    assert reasons[("volume", "kmeans", 3)]
    assert reasons[("volume", "kmeans", 4)] is None
    per_subset = 2 * 2
    assert m80.bonferroni_alpha(denom) < m80.bonferroni_alpha(per_subset)


def test_combined_family_funds_the_tighter_alpha():
    m80 = _load_research("regime_1080_nperm", "regime_1080_unsupervised_vol_model.py")
    combined = 5 * 3 * 2
    per_subset = 3 * 2
    resolved_combined = m80.resolve_bakeoff_n_perm(combined)
    resolved_single = m80.resolve_bakeoff_n_perm(per_subset)
    assert resolved_combined > resolved_single
    assert 1.0 / (resolved_combined + 1) <= m80.bonferroni_alpha(combined) / 2.0
    assert m80.resolve_bakeoff_n_perm(per_subset, requested=300) == 300
    with pytest.raises(ValueError, match="cannot satisfy the Bonferroni-corrected alpha"):
        m80.resolve_bakeoff_n_perm(combined, requested=300)


def test_enriched_bakeoff_smoke_if_data_available():
    try:
        mod = _load_research("regime_1095_smoke", "regime_1095_enriched_vol_model.py")
        report = mod.run_bakeoff("BTC/USDT", "1h", families=("kmeans",), k_range=range(3, 4),
                                 subsets={"canonical": list(ref.CANONICAL_COLUMNS),
                                          "volume": ref.CANONICAL_COLUMNS + ["volume_z"]},
                                 eval_windows=("is", "oos"), n_perm=200)
    except Exception as e:
        pytest.skip(f"no cached OHLCV / data path unavailable: {e}")
    assert "ablation" in report and "candidates" in report
    status = report["ablation"]["canonical"]["status"]
    assert status == "ok" or status == "unavailable" or status.startswith("unavailable: ")
    assert "live_wiring_delta" in report
    assert report["n_perm"] == 200
    assert report["bonferroni_denominator"] == sum(
        1 for c in report["candidates"] if not c["structurally_ineligible"])
    assert "ONE family" in report["bonferroni_denominator_policy"]
    for hr in report["handrule_held_out"].values():
        assert "permutation_steps_to_alpha" in hr and "knife_edge" in hr
