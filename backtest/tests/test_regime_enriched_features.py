"""Tests for the #1095 enriched feature matrix and the fit/decode column-order contract.

Covers: canonical-first column order, causal joins (look-ahead regression mirroring
test_backtester_lookahead.py — future bars never change a past feature row), funding backward-only,
HTF causal alignment, the fit<->decode column-order contract, and the decouple of fit-features from
naming-features (states named from the four canonical columns regardless of extra dims or position).
"""
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
    """Hourly OHLCV with a DatetimeIndex, alternating trending/ranging segments so the canonical
    composite features are non-degenerate, plus a volume series with regime-correlated bursts."""
    rng = np.random.default_rng(seed)
    idx = pd.date_range("2022-01-01", periods=n, freq="1h")
    drift = np.where((np.arange(n) // 50) % 2 == 0, 0.002, 0.0)   # alternating trend/range blocks
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
    """Hourly funding (timestamp ms, rate) covering [first_bar, last_bar], first snapshot exactly on
    the first bar so the column is not all-NaN."""
    rng = np.random.default_rng(seed)
    ts = (idx.as_unit("ns").asi8 // 1_000_000).astype("int64")   # -> epoch ms
    rate = rng.normal(0.0001, 0.00005, size=len(idx))
    return pd.DataFrame({"timestamp": ts, "rate": rate})


# ---------------------------------------------------------------- column contract / ordering

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
    # request out of order; builder must re-order to canonical-first global order
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
    # canonical not first -> indices track their real positions
    cols = ["funding_rate", "return_eff", "range_eff", "efficiency", "adx"]
    assert ref.canonical_indices_for(cols) == (1, 2, 3, 4)
    with pytest.raises(ValueError, match="missing"):
        ref.canonical_indices_for(["return_eff", "range_eff", "adx"])  # efficiency missing


# ---------------------------------------------------------------- look-ahead invariants

def test_enriched_matrix_is_causal_future_bars_do_not_change_past_rows():
    df = _synthetic_ohlcv(n=600)
    fund = _synthetic_funding(df.index)
    base = ref.enriched_feature_matrix(df, period=48, funding=fund, htf_multiple=4)
    t = 400
    mutated = df.copy()
    # blow up every bar AFTER t (prices and volume); past rows must be byte-identical
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
    # one funding snapshot at bar 60, value 0.005; bars < 60 see the prior (none -> NaN/earlier),
    # bars >= 60 see 0.005 until any later snapshot. Build a sparse funding series.
    idx = df.index
    ts = (idx.as_unit("ns").asi8 // 1_000_000).astype("int64")
    fund = pd.DataFrame({"timestamp": [ts[0], ts[60]], "rate": [0.001, 0.005]})
    mat = ref.enriched_feature_matrix(df, period=24, funding=fund, columns=ref.CANONICAL_COLUMNS + ["funding_rate"])
    fr = mat["funding_rate"].to_numpy()
    assert fr[59] == pytest.approx(0.001)    # before the bar-60 snapshot -> still the bar-0 value
    assert fr[60] == pytest.approx(0.005)    # at/after the snapshot
    assert fr[100] == pytest.approx(0.005)


def test_htf_feature_does_not_leak_in_progress_bar():
    df = _synthetic_ohlcv(n=600)
    mat = ref.enriched_feature_matrix(df, period=20, columns=ref.CANONICAL_COLUMNS + ["htf_range_eff"],
                                      htf_multiple=4)
    htf = mat["htf_range_eff"].to_numpy()
    # mutate only the LAST base bar's high (still inside the final, not-yet-closed HTF bucket);
    # no earlier base row's htf value may move.
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
    assert np.isnan(vz[:23]).all()            # fewer than `vol_window` observations -> NaN
    assert np.isfinite(vz[23])                # first full trailing window defined
    # recompute the z-score at one bar by hand from the trailing window
    vol = df["volume"].to_numpy()
    i = 100
    w = vol[i - 23: i + 1]
    expected = (vol[i] - w.mean()) / w.std()
    assert vz[i] == pytest.approx(expected, rel=1e-9)


# ---------------------------------------------------------------- fit / decode contract

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
    assert len(model["feature_means"]) == len(cols)             # ALL dims carried
    assert all(len(e["mean"]) == len(cols) for e in model["emissions"])
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"])  # named from canonical only
    labels, conf = ref.decode_with_model(mat, model)
    assert len(labels) == len(mat)
    valid = ~np.isnan(mat.to_numpy(dtype=float)).any(1)
    assert set(np.asarray(labels, dtype=object)[valid]).issubset(VALID_LABELS_COMPOSITE)


def test_fit_unsupervised_rejects_feature_name_count_mismatch():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = np.random.default_rng(0).normal(size=(300, 5))
    with pytest.raises(ValueError, match="columns but feature_names lists"):
        rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=16, thresholds=dict(TH),
                             feature_names=["a", "b", "c"])  # 5 cols vs 3 names


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
    # Decoding the SAME bars in a different column order changes labels — proves the contract
    # guards a real correctness hazard (forward_filter_labels is positional).
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
                   "volume_z", "htf_range_eff"]]  # return_eff <-> adx swapped (very different scale)
    wrong, _ = forward_filter_labels(swapped.to_numpy(dtype=float), model)
    valid = ~np.isnan(mat.to_numpy(dtype=float)).any(1)
    assert list(np.asarray(correct)[valid]) != list(np.asarray(wrong)[valid])


# ---------------------------------------------------------------- naming decoupled from fit dims

def test_naming_ignores_extra_dims_given_same_canonical_centroids():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(6); std = np.ones(6)
    # two states: identical in canonical cols 0..3, differ only in the extra cols 4,5
    em_a = np.array([[0.0, 0.02, 0.1, 8.0, 0.0, 0.0],
                     [0.4, 0.5, 0.9, 40.0, 0.0, 0.0]], dtype=float)
    em_b = em_a.copy(); em_b[:, 4:] = 99.0     # extra dims wildly different
    names_a, _ = rvm.map_latent_to_names(em_a, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    names_b, _ = rvm.map_latent_to_names(em_b, mean, std, dict(TH), canonical_indices=(0, 1, 2, 3))
    assert names_a == names_b                  # extra signals shape geometry, not the label


def test_naming_with_canonical_columns_not_first():
    # canonical block placed AFTER an extra column; correct canonical_indices still names right.
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(5); std = np.ones(5)
    # layout: [funding, return_eff, range_eff, efficiency, adx]
    em = np.array([[0.0, 0.0, 0.02, 0.1, 8.0],
                   [0.0, 0.4, 0.5, 0.9, 40.0]], dtype=float)
    names, _ = rvm.map_latent_to_names(em, mean, std, dict(TH), canonical_indices=(1, 2, 3, 4))
    assert names == ["ranging_quiet", "trending_up_clean"]


def test_map_latent_to_names_default_indices_unchanged_for_canonical_only():
    # regression: the 4-column legacy path (default canonical_indices) is byte-identical.
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    em = np.array([[0.0, 0.0, 0.1, 0.0], [0.5, 0.5, 0.9, 40.0]], dtype=float)
    names, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names == ["ranging_quiet", "trending_up_clean"]
    assert mapping["1"]["centroid_raw"] == [0.5, 0.5, 0.9, 40.0]


# ---------------------------------------------------------------- harness smoke (skips without data)

def test_enriched_bakeoff_smoke_if_data_available():
    import importlib.util
    path = os.path.join(_BACKTEST, "research", "regime_1095_enriched_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1095_smoke", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
        report = mod.run_bakeoff("BTC/USDT", "1h", families=("kmeans",), k_range=range(3, 4),
                                 subsets={"canonical": list(ref.CANONICAL_COLUMNS),
                                          "volume": ref.CANONICAL_COLUMNS + ["volume_z"]},
                                 eval_windows=("is", "oos"))
    except Exception as e:  # noqa: BLE001 — no cached OHLCV in CI -> skip, not fail
        pytest.skip(f"no cached OHLCV / data path unavailable: {e}")
    assert "ablation" in report and "candidates" in report
    assert report["ablation"]["canonical"]["status"] in ("ok", "unavailable")
    assert "live_wiring_delta" in report
