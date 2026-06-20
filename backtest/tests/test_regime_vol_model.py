import os, sys
import numpy as np
import pytest

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import regime_vol_model as rvm


def test_standardize_masks_nan_rows_and_floors_zero_std():
    feats = np.array([[1.0, 10.0, 0.5, 20.0],
                      [3.0, 10.0, 0.5, 30.0],
                      [np.nan, 10.0, 0.5, 40.0]], dtype=float)
    mean, std, mask = rvm.standardize(feats)
    assert mask.tolist() == [True, True, False]          # NaN row dropped
    assert mean[0] == pytest.approx(2.0)                  # mean over valid rows only
    assert std[1] == 1.0                                  # zero-variance col floored to 1.0


def test_empirical_transition_skips_pairs_spanning_a_dropped_bar():
    # bars: 0->valid(0), 1->valid(1), 2->NaN(dropped), 3->valid(0)
    valid_mask = np.array([True, True, False, True])
    assignments_valid = np.array([0, 1, 0])              # only for the 3 valid bars
    A = rvm.empirical_transition(assignments_valid, valid_mask, k=2, laplace=1.0)
    # only adjacency 0->1 counts (bars 0,1). Pair (1,2)&(2,3) span the dropped bar.
    assert A.shape == (2, 2)
    assert np.allclose(A.sum(1), 1.0)                     # row-stochastic
    # 0->1 got the lone real count; with laplace=1 row0 = [1,2]/3
    assert A[0, 1] > A[0, 0]


def test_init_distribution_is_stationary_and_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = rvm.init_distribution(A)
    assert pi.sum() == pytest.approx(1.0)
    assert np.allclose(pi @ A, pi, atol=1e-8)            # stationary


def test_logsumexp_matches_naive():
    v = np.array([-1.0, -2.0, -3.0])
    assert rvm._logsumexp(v) == pytest.approx(np.log(np.exp(v).sum()))
    m = np.array([[-1.0, -2.0], [0.0, -5.0]])
    assert np.allclose(rvm._logsumexp_rows(m),
                       np.log(np.exp(m).sum(1)))


def _three_blobs(seed=0, per=200):
    rng = np.random.default_rng(seed)
    centers = np.array([[-3, -3, -3, -3], [0, 0, 0, 0], [3, 3, 3, 3]], dtype=float)
    pts, truth = [], []
    for c_idx, c in enumerate(centers):
        pts.append(rng.normal(c, 0.25, size=(per, 4)))
        truth += [c_idx] * per
    return np.vstack(pts), np.array(truth)


def _purity(assign, truth, k):
    correct = 0
    for j in range(k):
        members = truth[assign == j]
        if len(members):
            maj = np.bincount(members).argmax()
            correct += int((members == maj).sum())
    return correct / len(truth)


def test_fit_kmeans_recovers_three_blobs():
    z, truth = _three_blobs()
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and em_var.shape == (3, 4)
    assert counts.sum() == len(z)
    assert (em_var > 0).all()
    assert _purity(assign, truth, 3) > 0.95


def test_fit_gmm_recovers_three_blobs():
    z, truth = _three_blobs(seed=1)
    assign, em_mean, em_var, counts = rvm.fit_gmm(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and em_var.shape == (3, 4) and (em_var > 0).all()
    assert counts.sum() == len(z)
    assert _purity(assign, truth, 3) > 0.95


def _markov_sequence(seed=0, n=1500):
    rng = np.random.default_rng(seed)
    A = np.array([[0.95, 0.04, 0.01], [0.03, 0.94, 0.03], [0.01, 0.04, 0.95]])
    centers = np.array([[-3, -3, -3, -3], [0, 0, 0, 0], [3, 3, 3, 3]], dtype=float)
    s = 0; states = []
    for _ in range(n):
        states.append(s)
        s = rng.choice(3, p=A[s])
    states = np.array(states)
    z = np.array([rng.normal(centers[s], 0.4) for s in states])
    return z, states


def test_fit_hmm_recovers_markov_states():
    z, truth = _markov_sequence()
    assign, em_mean, em_var, counts = rvm.fit_hmm(z, 3, seed=0)
    assert em_mean.shape == (3, 4) and (em_var > 0).all()
    assert counts.sum() == len(z)
    assert _purity(assign, truth, 3) > 0.9


def test_fitters_registry_complete():
    assert set(rvm.FITTERS) == {"kmeans", "gmm", "hmm"}
    assert rvm.FITTERS["hmm"] is rvm.fit_hmm


def test_fit_kmeans_k1_returns_global_mean_not_random_init():
    # regression: k=1's first assignment trivially equals the sentinel; the loop must NOT
    # early-break before computing the single cluster's mean.
    rng = np.random.default_rng(3)
    z = rng.normal([5.0, -2.0, 0.0, 1.0], 0.5, size=(300, 4))
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 1, seed=0)
    assert counts[0] == 300
    assert np.allclose(em_mean[0], z.mean(0), atol=1e-9)   # the true global mean, not the rng pick


def test_map_latent_to_names_uses_canonical_boundaries_and_is_deterministic():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    em = np.array([[0.0, 0.0, 0.1, 0.0],
                   [0.5, 0.5, 0.9, 40.0]], dtype=float)
    names, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names == ["ranging_quiet", "trending_up_clean"]
    from regime import VALID_LABELS_COMPOSITE
    assert all(nm in VALID_LABELS_COMPOSITE for nm in names)
    names2, _ = rvm.map_latent_to_names(em, mean, std, dict(TH))
    assert names2 == names
    assert mapping["1"]["centroid_raw"] == [0.5, 0.5, 0.9, 40.0]


def test_volatility_rank_orders_by_range_eff():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    mean = np.zeros(4); std = np.ones(4)
    em = np.array([[0.0, 0.8, 0.1, 10.0],
                   [0.0, 0.01, 0.1, 10.0],
                   [0.0, 0.3, 0.1, 10.0]], dtype=float)
    _, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
    ranks = {int(i): m["volatility_rank"] for i, m in mapping.items()}
    assert ranks[1] < ranks[2] < ranks[0]


def test_map_composite_label_monotone_in_range_within_ranging():
    from regime import map_composite_label, _DEFAULT_COMPOSITE_THRESHOLDS as TH
    quiet = map_composite_label(0.0, 5.0, 0.0, 0.1, dict(TH))
    volatile = map_composite_label(0.0, 5.0, 0.9, 0.1, dict(TH))
    assert quiet == "ranging_quiet"
    assert volatile == "ranging_volatile"


REQUIRED_KEYS = {"type", "version", "fit_method", "features", "feature_means",
                 "feature_stds", "states", "latent_count", "emissions",
                 "transition", "init", "filter_window", "period", "fitted_on", "mapping"}


def _feature_blob_matrix(seed=0):
    rng = np.random.default_rng(seed)
    centers = np.array([[0.0, 0.02, 0.1, 8.0],
                        [0.4, 0.5, 0.9, 40.0],
                        [-0.4, 0.5, 0.9, 40.0]], dtype=float)
    rows = []
    for c in centers:
        rows.append(rng.normal(c, [0.02, 0.02, 0.02, 1.0], size=(150, 4)))
    feats = np.vstack(rows)
    feats = np.vstack([np.full((5, 4), np.nan), feats])
    return feats


@pytest.mark.parametrize("family", ["kmeans", "gmm", "hmm"])
def test_fit_unsupervised_schema_and_decodes(family):
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    from regime_hmm import forward_filter_labels
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family=family, k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0,
                                 fitted_on={"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"})
    assert REQUIRED_KEYS <= set(model)
    assert model["fit_method"] == family and model["latent_count"] == 3
    assert len(model["states"]) == 3 and len(model["emissions"]) == 3
    assert np.allclose(np.sum(model["transition"], axis=1), 1.0)
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"])
    labels, conf = forward_filter_labels(feats, model)
    assert len(labels) == len(feats)
    assert set(labels[~np.isnan(feats).any(1)]).issubset(VALID_LABELS_COMPOSITE)


def test_fit_unsupervised_no_leakage_does_not_call_compute_regime_composite(monkeypatch):
    import regime
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    def _boom(*a, **k):
        raise AssertionError("fit path must not call compute_regime_composite")
    monkeypatch.setattr(regime, "compute_regime_composite", _boom)
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert model["latent_count"] == 3
    # also exercise the thresholds=None branch (lazy-imports defaults) under the same guard
    model2 = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32, seed=0)
    assert model2["latent_count"] == 3


def test_decode_is_causal_future_bars_do_not_change_past_labels():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    from regime_hmm import forward_filter_labels
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="hmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    base, _ = forward_filter_labels(feats, model)
    t = 200
    mutated = feats.copy()
    mutated[t + 1:] = mutated[t + 1:] * 5.0 + 1.0
    after, _ = forward_filter_labels(mutated, model)
    assert list(base[: t + 1]) == list(after[: t + 1])


def test_fitted_model_scores_through_score_labels():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    from regime_hmm import forward_filter_labels
    from regime_diagnostics import score_labels
    feats = _feature_blob_matrix()
    close = np.cumprod(1 + np.zeros(len(feats))) * 100 + np.arange(len(feats))
    model = rvm.fit_unsupervised(feats, family="gmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    labels, _ = forward_filter_labels(feats, model)
    rep = score_labels(close, labels, feats, target="volatility")
    assert "stability" in rep and "coverage" in rep


def test_model_satisfies_bounded_window_and_forward_filter_contract():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    import regime_bounded_window_validate as bw
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="hmm", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0,
                                 fitted_on={"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"})
    for key in ("states", "feature_means", "feature_stds", "emissions",
                "init", "transition", "filter_window", "period", "fitted_on"):
        assert key in model
    prov = bw._provenance_status(model, "BTC/USDT", "1h", "oos")
    assert prov["verified"] is True
    assert prov["overlap_resolvable"] is True   # both windows in WINDOWS -> date-range path engaged
    assert prov["in_sample"] is False           # is/oos disjoint -> held-out, promotable


def test_non_degeneracy_flags_constant_stream():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=0.8,
                                  min_transition_rate=0.05)
    constant = np.array(["ranging_quiet"] * 500, dtype=object)
    rep = non_degeneracy(constant, thr)
    assert rep["ok"] is False
    assert rep["active_labels"] == 1
    assert "min_active_labels" in " ".join(rep["reasons"])


def test_non_degeneracy_passes_healthy_stream():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=0.9,
                                  min_transition_rate=0.01)
    rng = np.random.default_rng(0)
    stream = rng.choice(["ranging_quiet", "trending_up_clean", "ranging_volatile"], size=600)
    rep = non_degeneracy(stream.astype(object), thr)
    assert rep["ok"] is True and rep["reasons"] == []


def test_derive_thresholds_is_looser_than_incumbent_worst_window():
    from regime_vol_model import derive_thresholds, non_degeneracy
    a = np.array((["x", "y", "z"] * 200), dtype=object)
    b = np.array((["x"] * 150 + ["y"] * 50) * 2, dtype=object)
    thr = derive_thresholds([a, b])
    assert non_degeneracy(a, thr)["ok"] is True
    assert non_degeneracy(b, thr)["ok"] is True
    occ_b = max(c["pct"] for c in __import__("regime_diagnostics").coverage(b).values())
    assert thr.max_occupancy >= occ_b
