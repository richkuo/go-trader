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


def test_subset_selection_preserves_canonical_first_order(subtests):
    with subtests.test("test_subset_selection_preserves_canonical_first_order"):
        df = _synthetic_ohlcv()
        fund = _synthetic_funding(df.index)
        mat = ref.enriched_feature_matrix(df, period=48, funding=fund,
                                          columns=["htf_range_eff", "adx", "return_eff",
                                                   "funding_rate", "range_eff", "efficiency"])
        assert list(mat.columns) == ["return_eff", "range_eff", "efficiency", "adx",
                                     "funding_rate", "htf_range_eff"]
    with subtests.test("test_builder_rejects_unknown_column"):
        df = _synthetic_ohlcv()
        with pytest.raises(ValueError, match="unknown enriched columns"):
            ref.enriched_feature_matrix(df, period=48, columns=["return_eff", "rsi_14"])


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


def test_full_enriched_matrix_builds_in_order_and_fitted_models_hold_the_feature_contract():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    from regime_hmm import forward_filter_labels
    df = _synthetic_ohlcv()
    fund = _synthetic_funding(df.index)
    cols = ref.ENRICHED_COLUMNS
    mat = ref.enriched_feature_matrix(df, period=48, funding=fund, columns=cols)
    default_mat = ref.enriched_feature_matrix(df, period=48, funding=fund)

    case = "test_builder_full_matrix_columns_and_order"
    assert list(default_mat.columns) == ref.ENRICHED_COLUMNS, case
    assert len(default_mat) == len(df), case

    case = "test_fit_unsupervised_enriched_schema_and_decode"
    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="hmm", k=4, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=ref.canonical_indices_for(cols))
    assert model["features"] == list(cols), case
    assert model["canonical_indices"] == [0, 1, 2, 3], case
    assert len(model["feature_means"]) == len(cols), case
    assert all(len(e["mean"]) == len(cols) for e in model["emissions"]), case
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"]), case
    labels, conf = ref.decode_with_model(mat, model)
    assert len(labels) == len(mat), case
    valid = ~np.isnan(mat.to_numpy(dtype=float)).any(1)
    assert set(np.asarray(labels, dtype=object)[valid]).issubset(VALID_LABELS_COMPOSITE), case

    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="kmeans", k=4, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=(0, 1, 2, 3))
    reordered = mat[list(reversed(cols))]
    with pytest.raises(ValueError, match="feature-order contract violated"):
        ref.decode_with_model(reordered, model)
    with pytest.raises(ValueError, match="feature-order contract violated"):
        ref.assert_feature_contract(model, list(reversed(cols)))

    case = "test_column_order_is_load_bearing_for_labels"
    model = rvm.fit_unsupervised(mat.to_numpy(dtype=float), family="hmm", k=5, filter_window=32,
                                 thresholds=dict(TH), seed=0, feature_names=cols,
                                 canonical_indices=(0, 1, 2, 3))
    correct, _ = forward_filter_labels(mat.to_numpy(dtype=float), model)
    swapped = mat[["adx", "range_eff", "efficiency", "return_eff", "funding_rate",
                   "volume_z", "htf_range_eff", "hurst"]]
    wrong, _ = forward_filter_labels(swapped.to_numpy(dtype=float), model)
    assert list(np.asarray(correct)[valid]) != list(np.asarray(wrong)[valid]), case


def test_fit_unsupervised_rejects_feature_name_count_mismatch():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = np.random.default_rng(0).normal(size=(300, 5))
    with pytest.raises(ValueError, match="columns but feature_names lists"):
        rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=16, thresholds=dict(TH),
                             feature_names=["a", "b", "c"])


def test_naming_uses_canonical_indices_ignoring_extra_dims_and_column_position():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(6); std = np.ones(6)
    em_a = np.array([[0.0, 0.02, 0.1, 8.0, 0.0, 0.0],
                     [0.4, 0.5, 0.9, 40.0, 0.0, 0.0]], dtype=float)
    em_b = em_a.copy(); em_b[:, 4:] = 99.0
    names_a, _ = rvm.map_latent_to_names(em_a, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    names_b, _ = rvm.map_latent_to_names(em_b, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    assert names_a == names_b, "test_naming_ignores_extra_dims_given_same_canonical_centroids"

    mean = np.zeros(5); std = np.ones(5)
    em = np.array([[0.0, 0.0, 0.02, 0.1, 8.0],
                   [0.0, 0.4, 0.5, 0.9, 40.0]], dtype=float)
    names, _ = rvm.map_latent_to_names(em, mean, std, dict(TH), canonical_indices=(1, 2, 3, 4))
    assert names == ["ranging_quiet", "trending_up_clean"], "test_naming_with_canonical_columns_not_first"


def _bakeoff_ohlcv(n, seed):
    rng = np.random.default_rng(seed)
    idx = pd.date_range("2022-01-01", periods=n, freq="1h")
    drift = np.where((np.arange(n) // 50) % 2 == 0, 0.002, 0.0)
    vol = np.where((np.arange(n) // 120) % 2 == 0, 0.004, 0.02)
    close = 100 * np.cumprod(1 + drift + rng.normal(0, 1, size=n) * vol)
    high = close * (1 + np.abs(rng.normal(0, 0.004, size=n)))
    low = close * (1 - np.abs(rng.normal(0, 0.004, size=n)))
    openp = np.concatenate([[close[0]], close[:-1]])
    volume = np.abs(rng.normal(1000, 200, size=n)) + (np.arange(n) // 50 % 2) * 500
    return pd.DataFrame({"open": openp, "high": np.maximum(high, np.maximum(openp, close)),
                         "low": np.minimum(low, np.minimum(openp, close)), "close": close,
                         "volume": volume}, index=idx)
