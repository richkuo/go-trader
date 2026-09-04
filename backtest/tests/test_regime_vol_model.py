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
from regime_hmm import forward_filter_labels


def test_standardize_masks_nan_rows_and_floors_zero_std():
    feats = np.array([[1.0, 10.0, 0.5, 20.0],
                      [3.0, 10.0, 0.5, 30.0],
                      [np.nan, 10.0, 0.5, 40.0]], dtype=float)
    mean, std, mask = rvm.standardize(feats)
    assert mask.tolist() == [True, True, False]
    assert mean[0] == pytest.approx(2.0)
    assert std[1] == 1.0


def test_empirical_transition_skips_pairs_spanning_a_dropped_bar():
    valid_mask = np.array([True, True, False, True])
    assignments_valid = np.array([0, 1, 0])
    A = rvm.empirical_transition(assignments_valid, valid_mask, k=2, laplace=1.0)
    assert A.shape == (2, 2)
    assert np.allclose(A.sum(1), 1.0)
    assert np.allclose(A, [[1.0 / 3, 2.0 / 3], [0.5, 0.5]])


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


@pytest.mark.parametrize("fitter,seed", [("kmeans", 0), ("gmm", 1)])
def test_fitters_recover_three_blobs(fitter, seed):
    z, truth = _three_blobs(seed=seed)
    assign, em_mean, em_var, counts = rvm.FITTERS[fitter](z, 3, seed=0)
    assert em_mean.shape == (3, 4) and em_var.shape == (3, 4)
    assert (em_var > 0).all()
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


def test_fit_kmeans_k1_returns_global_mean_not_random_init():
    rng = np.random.default_rng(3)
    z = rng.normal([5.0, -2.0, 0.0, 1.0], 0.5, size=(300, 4))
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 1, seed=0)
    assert counts[0] == 300
    assert np.allclose(em_mean[0], z.mean(0), atol=1e-9)


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
    assert prov["overlap_resolvable"] is True
    assert prov["in_sample"] is False


@pytest.mark.parametrize("thr_kw,stream_fn,expected", [
    pytest.param(
        dict(min_active_labels=2, max_occupancy=0.8, min_transition_rate=0.05),
        lambda: np.array(["ranging_quiet"] * 500, dtype=object),
        {"ok": False, "active_labels": 1, "reason": "min_active_labels"},
        id="constant_stream"),
    pytest.param(
        dict(min_active_labels=2, max_occupancy=0.9, min_transition_rate=0.01),
        lambda: np.random.default_rng(0).choice(
            ["ranging_quiet", "trending_up_clean", "ranging_volatile"],
            size=600).astype(object),
        {"ok": True, "reason": None},
        id="healthy_stream"),
    pytest.param(
        dict(min_active_labels=2, max_occupancy=0.8, min_transition_rate=0.0),
        lambda: np.array((["a"] * 9 + ["b"]) * 50, dtype=object),
        {"ok": False, "active_labels": 2, "reason": "max_occupancy"},
        id="high_occupancy"),
    pytest.param(
        dict(min_active_labels=2, max_occupancy=1.0, min_transition_rate=0.5),
        lambda: np.array(["a"] * 300 + ["b"] * 300, dtype=object),
        {"ok": False, "reason": "min_transition_rate"},
        id="low_transition_rate"),
])
def test_non_degeneracy(thr_kw, stream_fn, expected):
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    rep = non_degeneracy(stream_fn(), NonDegeneracyThresholds(**thr_kw))
    assert rep["ok"] is expected["ok"]
    if "active_labels" in expected:
        assert rep["active_labels"] == expected["active_labels"]
    if expected["reason"] is None:
        assert rep["reasons"] == []
    else:
        assert expected["reason"] in " ".join(rep["reasons"])


def test_derive_thresholds_is_looser_than_incumbent_worst_window():
    from regime_vol_model import derive_thresholds, non_degeneracy
    a = np.array((["x", "y", "z"] * 200), dtype=object)
    b = np.array((["x"] * 150 + ["y"] * 50) * 2, dtype=object)
    thr = derive_thresholds([a, b])
    assert non_degeneracy(a, thr)["ok"] is True
    assert non_degeneracy(b, thr)["ok"] is True
    occ_b = max(c["pct"] for c in __import__("regime_diagnostics").coverage(b).values())
    assert thr.max_occupancy >= occ_b


def _load_research_module(tag):
    import importlib.util
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "research", "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location(f"regime_1080_{tag}", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_select_winner_prefers_eligible_high_separation():
    mod = _load_research_module("rank1")
    cands = [
        {"family": "hmm", "k": 3, "verdict": {"ship": True}, "model_p_value": 0.001,
         "non_degenerate_all": True, "model_kruskal_h": 90.0, "stability_gain": 0.05},
        {"family": "gmm", "k": 4, "verdict": {"ship": True}, "model_p_value": 0.001,
         "non_degenerate_all": True, "model_kruskal_h": 120.0, "stability_gain": 0.03},
        {"family": "kmeans", "k": 5, "verdict": {"ship": False}, "model_p_value": 0.001,
         "non_degenerate_all": True, "model_kruskal_h": 999.0, "stability_gain": 0.9},
        {"family": "hmm", "k": 7, "verdict": {"ship": True}, "model_p_value": 0.001,
         "non_degenerate_all": False, "model_kruskal_h": 999.0, "stability_gain": 0.9},
    ]
    win = mod.select_winner(cands)
    assert win["family"] == "gmm" and win["k"] == 4


def test_select_winner_returns_none_when_no_eligible():
    mod = _load_research_module("rank2")
    assert mod.select_winner([{"family": "hmm", "k": 3, "verdict": {"ship": False},
                               "model_p_value": 0.001, "non_degenerate_all": True,
                               "model_kruskal_h": 1.0, "stability_gain": 0.0}]) is None


def test_select_winner_applies_bonferroni_correction():
    mod = _load_research_module("bonf")
    lucky = {"family": "kmeans", "k": 4, "verdict": {"ship": True}, "model_p_value": 0.03,
             "non_degenerate_all": True, "model_kruskal_h": 999.0, "stability_gain": 0.9}
    fillers = [{"family": "hmm", "k": 2, "verdict": {"ship": False}, "model_p_value": 0.9,
                "non_degenerate_all": False, "model_kruskal_h": 1.0, "stability_gain": 0.0}
               for _ in range(19)]
    assert mod.select_winner([lucky] + fillers) is None
    assert mod.bonferroni_alpha(20) == pytest.approx(0.05 / 20)
    lucky_real = dict(lucky, model_p_value=0.0001)
    win = mod.select_winner([lucky_real] + fillers)
    assert win["family"] == "kmeans" and win["k"] == 4


def test_fit_kmeans_reseeds_empty_clusters_no_ghost_states():
    z, _ = _three_blobs(seed=0)
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 7, seed=0)
    assert counts.min() >= 1
    assert counts.sum() == len(z)
    assert (em_var > 0).all()
    assert np.isfinite(em_mean).all()
    for j in range(7):
        assert (assign == j).any()


def test_fit_kmeans_reseed_survives_duplicate_rows():
    z = np.tile(np.array([0.3, 0.4, 0.5, 12.0]), (50, 1))
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 4, seed=0)
    assert counts.min() >= 1
    assert counts.sum() == 50
    assert (em_var > 0).all() and np.isfinite(em_mean).all()


def test_fit_unsupervised_high_k_stores_no_zero_n_emission():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = _feature_blob_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=7, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert all(e["n"] >= 1 for e in model["emissions"])
    assert sum(e["n"] for e in model["emissions"]) == int((~np.isnan(feats).any(1)).sum())


def test_bakeoff_smoke_on_cached_data_if_available():
    try:
        mod = _load_research_module("smoke")
        report = mod.run_bakeoff("BTC/USDT", "1h", families=("kmeans",),
                                 k_range=range(3, 4), eval_windows=("is", "oos"),
                                 n_perm=200)
    except Exception as e:
        pytest.skip(f"no cached OHLCV / data path unavailable: {e}")
    assert "candidates" in report and len(report["candidates"]) == 1
    assert "non_degeneracy_thresholds" in report
    assert "handrule_held_out" in report and "abstained" in report["handrule_held_out"]
    assert report["n_perm"] == 200
    assert report["min_achievable_p_value"] == pytest.approx(1.0 / 201.0)
    assert "bonferroni_denominator" in report and "bonferroni_denominator_policy" in report
    assert "structurally_ineligible" in report
    assert "permutation_steps_to_alpha" in report["handrule_held_out"]
    assert "knife_edge" in report["handrule_held_out"]
    assert report["min_achievable_p_value"] <= report["bonferroni_alpha"]


def test_resolve_bakeoff_n_perm_default_achieves_corrected_alpha_with_headroom():
    mod = _load_research_module("nperm_default")
    n = mod.resolve_bakeoff_n_perm(18)
    assert n >= mod.DEFAULT_BAKEOFF_MIN_N_PERM
    assert 1.0 / (n + 1) <= mod.bonferroni_alpha(18) / 2.0


def test_resolve_bakeoff_n_perm_rejects_underpowered_explicit_request():
    mod = _load_research_module("nperm_reject")
    with pytest.raises(ValueError, match="cannot satisfy the Bonferroni-corrected alpha"):
        mod.resolve_bakeoff_n_perm(18, requested=200)


def test_resolve_bakeoff_n_perm_narrowed_sweep_accepts_200():
    mod = _load_research_module("nperm_narrow")
    assert mod.resolve_bakeoff_n_perm(6, requested=200) == 200


def test_real_pipeline_p_value_can_be_crowned_at_default_sweep_resolution():
    from regime_diagnostics import block_shuffle_pvalue
    mod = _load_research_module("crown")
    n_perm = mod.resolve_bakeoff_n_perm(18)
    rng = np.random.default_rng(0)
    labels = np.array((["low"] * 8 + ["high"] * 8) * 25, dtype=object)
    fwd = np.where(labels == "high", 1.0, 0.001) + rng.normal(0, 1e-4, len(labels))
    sig = block_shuffle_pvalue(labels, fwd, block_len=8, n_perm=n_perm, seed=0)
    alpha = mod.bonferroni_alpha(18)
    assert sig["p_value"] <= alpha
    strong = {"family": "kmeans", "k": 4, "verdict": {"ship": True},
              "model_p_value": sig["p_value"], "non_degenerate_all": True,
              "model_kruskal_h": 999.0, "stability_gain": 0.9}
    fillers = [{"family": "hmm", "k": 5, "verdict": {"ship": False}, "model_p_value": 0.9,
                "non_degenerate_all": False, "model_kruskal_h": 1.0, "stability_gain": 0.0}
               for _ in range(17)]
    win = mod.select_winner([strong] + fillers)
    assert win is not None and win["family"] == "kmeans" and win["k"] == 4


def test_structurally_ineligible_reason_keys_off_min_active_labels():
    mod = _load_research_module("inelig_reason")
    thr = rvm.NonDegeneracyThresholds(min_active_labels=4, max_occupancy=0.9,
                                      min_transition_rate=0.0)
    assert mod.structurally_ineligible_reason(2, thr) is not None
    assert mod.structurally_ineligible_reason(3, thr) is not None
    assert mod.structurally_ineligible_reason(4, thr) is None
    assert mod.structurally_ineligible_reason(7, thr) is None


def test_bonferroni_denominator_excludes_structurally_ineligible():
    mod = _load_research_module("denom")
    cands = ([{"structurally_ineligible": True} for _ in range(6)]
             + [{} for _ in range(12)])
    assert mod.bonferroni_denominator(cands) == 12
    assert mod.bonferroni_alpha(mod.bonferroni_denominator(cands)) == pytest.approx(0.05 / 12)


def test_select_winner_ignores_structurally_ineligible_candidates():
    mod = _load_research_module("inelig_select")
    eligible = {"family": "gmm", "k": 4, "verdict": {"ship": True}, "model_p_value": 0.03,
                "non_degenerate_all": True, "model_kruskal_h": 10.0, "stability_gain": 0.1}
    ineligible = [{"family": "kmeans", "k": 2, "verdict": {"ship": True},
                   "model_p_value": 0.0001, "non_degenerate_all": True,
                   "model_kruskal_h": 999.0, "stability_gain": 0.9,
                   "structurally_ineligible": True}
                  for _ in range(19)]
    win = mod.select_winner([eligible] + ineligible)
    assert win is not None and win["family"] == "gmm" and win["k"] == 4


def test_permutation_steps_to_alpha_and_knife_edge_are_symmetric():
    mod = _load_research_module("steps")
    assert mod.permutation_steps_to_alpha(10.0 / 201.0, 200) == 0
    assert mod.permutation_steps_to_alpha(15.0 / 201.0, 200) < 0
    assert mod.permutation_steps_to_alpha(2.0 / 201.0, 200) > 0
    assert mod.permutation_steps_to_alpha(11.0 / 201.0, 200) == -1
    assert mod.verdict_knife_edge(0) is True
    assert mod.verdict_knife_edge(-1) is True
    assert mod.verdict_knife_edge(1) is True
    assert mod.verdict_knife_edge(-2) is False
    assert mod.verdict_knife_edge(2) is False


def test_score_labels_default_n_perm_is_byte_identical():
    import json
    from regime_diagnostics import score_labels
    feats = _feature_blob_matrix()
    rng = np.random.default_rng(1)
    labels = rng.choice(["a", "b", "c"], size=len(feats)).astype(object)
    close = 100.0 + np.cumsum(rng.normal(0.0, 0.5, len(feats)))
    base = score_labels(close, labels, feats, target="volatility")
    explicit = score_labels(close, labels, feats, target="volatility", n_perm=200)
    assert (json.dumps(base, default=float, sort_keys=True)
            == json.dumps(explicit, default=float, sort_keys=True))


def _decoder_model(em_mean, em_var, states, *, filter_window=8):
    k = len(states)
    return {"states": list(states),
            "feature_means": [0.0, 0.0, 0.0, 0.0], "feature_stds": [1.0, 1.0, 1.0, 1.0],
            "emissions": [{"mean": list(m), "var": list(v), "n": 0}
                          for m, v in zip(em_mean, em_var)],
            "init": [1.0 / k] * k, "transition": [[1.0 / k] * k for _ in range(k)],
            "filter_window": int(filter_window)}


def test_neutralize_dead_components_classifies_by_soft_mass_not_geometry():
    mu = np.array([[0.0, 0.0, 0.0, 0.0],
                   [0.0, 0.0, 0.0, 0.0],
                   [3.0, 3.0, 3.0, 3.0]], dtype=float)
    var = np.array([[1e-3, 1e-3, 1e-3, 1e-3],
                    [1e-3, 1e-3, 1e-3, 1e-3],
                    [0.4, 0.4, 0.4, 0.4]], dtype=float)
    dead = rvm._neutralize_dead_components(mu, var, np.array([1e-9, 250.0, 250.0]),
                                           min_soft_mass=1.0)
    assert dead.tolist() == [True, False, False]
    assert np.array_equal(mu[0], [0.0, 0.0, 0.0, 0.0]) and np.array_equal(var[0], [1.0, 1.0, 1.0, 1.0])
    assert np.array_equal(var[1], [1e-3, 1e-3, 1e-3, 1e-3])
    assert np.array_equal(mu[2], [3.0, 3.0, 3.0, 3.0]) and np.array_equal(var[2], [0.4, 0.4, 0.4, 0.4])


def test_neutralize_dead_components_is_noop_when_all_components_alive():
    mu = np.array([[1.0, 2.0, 3.0, 4.0], [-1.0, -2.0, -3.0, -4.0]], dtype=float)
    var = np.array([[0.5, 0.6, 0.7, 0.8], [0.9, 1.1, 1.2, 1.3]], dtype=float)
    mu0, var0 = mu.copy(), var.copy()
    dead = rvm._neutralize_dead_components(mu, var, np.array([500.0, 500.0]), min_soft_mass=1.0)
    assert not dead.any()
    assert np.array_equal(mu, mu0) and np.array_equal(var, var0)


def test_truly_dead_component_cannot_win_near_mean_bars_after_parking():
    em_mean = [[0.3, 0.3, 0.3, 0.3], [3.0, 3.0, 3.0, 3.0], [0.0, 0.0, 0.0, 0.0]]
    em_var = [[0.5, 0.5, 0.5, 0.5], [1.0, 1.0, 1.0, 1.0], [1e-3, 1e-3, 1e-3, 1e-3]]
    states = ["A", "B", "C_ghost"]
    near_mean = np.full((8, 4), 0.01)

    labels_bug, _ = forward_filter_labels(near_mean, _decoder_model(em_mean, em_var, states))
    assert set(labels_bug) == {"C_ghost"}

    mu = np.array(em_mean, dtype=float); var = np.array(em_var, dtype=float)
    dead = rvm._neutralize_dead_components(mu, var, np.array([500.0, 500.0, 1e-9]), min_soft_mass=1.0)
    assert dead.tolist() == [False, False, True]
    labels_fixed, _ = forward_filter_labels(near_mean, _decoder_model(mu, var, states))
    assert set(labels_fixed) == {"A"}


def test_benign_zero_hard_count_component_decodes_unchanged():
    em_mean = [[2.0, 2.0, 2.0, 2.0], [-2.0, -2.0, -2.0, -2.0]]
    em_var = [[1.0, 1.0, 1.0, 1.0], [0.8, 0.8, 0.8, 0.8]]
    states = ["X", "Y_benign"]
    in_Y = np.full((6, 4), -2.0)

    before, _ = forward_filter_labels(in_Y, _decoder_model(em_mean, em_var, states))
    assert set(before) == {"Y_benign"}

    mu = np.array(em_mean, dtype=float); var = np.array(em_var, dtype=float)
    dead = rvm._neutralize_dead_components(mu, var, np.array([300.0, 300.0]), min_soft_mass=1.0)
    assert not dead.any()
    after, _ = forward_filter_labels(in_Y, _decoder_model(mu, var, states))
    assert list(after) == list(before)


@pytest.mark.parametrize("fitter", ["gmm", "hmm"])
def test_fitters_pass_valid_soft_mass_to_neutralizer(fitter, monkeypatch):
    z, _ = _three_blobs(seed=0)
    captured = {}
    orig = rvm._neutralize_dead_components

    def spy(mu, var, soft_mass, **kw):
        captured["soft_mass"] = np.asarray(soft_mass, dtype=float).copy()
        captured["calls"] = captured.get("calls", 0) + 1
        return orig(mu, var, soft_mass, **kw)

    monkeypatch.setattr(rvm, "_neutralize_dead_components", spy)
    (rvm.fit_gmm if fitter == "gmm" else rvm.fit_hmm)(z, 3, seed=0)
    assert captured.get("calls") == 1
    sm = captured["soft_mass"]
    assert sm.shape == (3,) and np.isfinite(sm).all() and (sm >= 0).all()
    assert sm.sum() == pytest.approx(len(z), rel=1e-6)


@pytest.mark.parametrize("fitter", ["gmm", "hmm"])
def test_healthy_fit_parks_nothing(fitter):
    z, _ = _three_blobs(seed=2)
    _, mu, var, _ = (rvm.fit_gmm if fitter == "gmm" else rvm.fit_hmm)(z, 3, seed=0)
    for j in range(3):
        parked = np.allclose(mu[j], 0.0, atol=1e-6) and np.allclose(var[j], 1.0, atol=1e-9)
        assert not parked, f"component {j} was spuriously parked on healthy data"


def test_label_anchored_hmm_degenerate_anchor_is_unchanged():
    from regime_hmm import fit_label_anchored_hmm
    rng = np.random.default_rng(0)
    feats = rng.normal([1.0, 1.0, 1.0, 1.0], 0.2, size=(60, 4))
    labels = np.array(["ranging_quiet"] * 60, dtype=object)
    model = fit_label_anchored_hmm(feats, labels, ["ranging_quiet", "trending_up_clean"],
                                   filter_window=16)
    degenerate = model["emissions"][1]
    assert degenerate["mean"] == [0.0, 0.0, 0.0, 0.0]
    assert degenerate["var"] == [1.0, 1.0, 1.0, 1.0]
    assert degenerate["n"] == 0
    labels_out, _ = forward_filter_labels(feats, model)
    assert len(labels_out) == len(feats)


def test_kmeans_path_unaffected_by_neutralizer():
    import inspect
    assert "min_soft_mass" not in inspect.signature(rvm.fit_kmeans).parameters
    z, _ = _three_blobs(seed=0)
    _, mu, var, counts = rvm.fit_kmeans(z, 3, seed=0)
    assert counts.sum() == len(z)
    for j in range(3):
        assert not (np.allclose(mu[j], 0.0, atol=1e-6) and np.allclose(var[j], 1.0, atol=1e-9))


def _colliding_feature_matrix(seed=0):
    rng = np.random.default_rng(seed)
    centers = np.array([[0.0, 0.02, 0.1, 8.0],
                        [0.07, 0.14, 0.15, 20.0],
                        [0.15, 0.20, 0.30, 22.0]], dtype=float)
    rows = [rng.normal(c, [0.005, 0.01, 0.02, 1.0], size=(150, 4)) for c in centers]
    return np.vstack([np.full((5, 4), np.nan), *rows])


def test_colliding_centroids_share_a_handrule_cell():
    from regime import map_composite_label, _DEFAULT_COMPOSITE_THRESHOLDS as TH
    assert map_composite_label(0.07, 20.0, 0.14, 0.15, dict(TH)) == "trending_up_choppy"
    assert map_composite_label(0.15, 22.0, 0.20, 0.30, dict(TH)) == "trending_up_choppy"


@pytest.mark.parametrize("family", ["kmeans", "gmm", "hmm"])
def test_fitted_model_with_colliding_centroids_keeps_every_latent_state_distinct(family):
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    feats = _colliding_feature_matrix()
    model = rvm.fit_unsupervised(feats, family=family, k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert model["latent_count"] == 3
    assert len(set(model["states"])) == 3, model["states"]
    assert all(s in VALID_LABELS_COMPOSITE for s in model["states"])
    labels, _ = forward_filter_labels(feats, model)
    valid = ~np.isnan(feats).any(1)
    assert len(set(labels[valid].tolist())) == 3


def _artifact_candidate(path_tail, subset, family, k):
    import json
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "..", "docs", "research", "1218-artifacts", path_tail)
    with open(path) as fh:
        report = json.load(fh)
    for c in report["candidates"]:
        if c.get("subset", "canonical") == subset and c["family"] == family and c["k"] == k:
            return c
    raise AssertionError(f"{subset}:{family}:k={k} missing from {path_tail}")


@pytest.mark.parametrize("path_tail,subset,family,k", [
    ("regime_1095_btc.json", "htf", "hmm", 6),
    ("regime_1095_eth.json", "canonical", "kmeans", 7),
    ("regime_1095_eth.json", "funding", "kmeans", 6),
    ("regime_1095_eth.json", "all_enriched", "kmeans", 6),
])
def test_1218_near_miss_centroids_map_to_distinct_names(path_tail, subset, family, k):
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH, VALID_LABELS_COMPOSITE
    from regime_enriched_features import canonical_indices_for
    cand = _artifact_candidate(path_tail, subset, family, k)
    stored = cand["states"]
    assert len(set(stored)) < len(stored)
    raw = np.array([cand["mapping"][str(i)]["centroid_raw"] for i in range(k)], dtype=float)
    d = raw.shape[1]
    cidx = canonical_indices_for(cand["columns"])
    names, mapping = rvm.map_latent_to_names(raw, np.zeros(d), np.ones(d), dict(TH),
                                             canonical_indices=cidx)
    assert len(set(names)) == k, names
    assert all(nm in VALID_LABELS_COMPOSITE for nm in names)
    handrule = [mapping[str(i)]["handrule_name"] for i in range(k)]
    assert handrule == stored
    for i in range(k):
        if stored.count(stored[i]) == 1:
            assert names[i] == stored[i]


def test_handrule_cells_partition_matches_map_composite_label():
    from regime import map_composite_label, _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    cells = rvm.handrule_cells(th)
    assert set(cells) == {"trending_up_clean", "trending_up_choppy", "trending_down_clean",
                          "trending_down_choppy", "ranging_directional_up",
                          "ranging_directional_down", "ranging_directional",
                          "ranging_volatile", "ranging_quiet"}
    rng = np.random.default_rng(7)
    scale = {"return_eff": 1.0, "range_eff": 1.0, "efficiency": 1.0, "adx": 1.0}
    for _ in range(2000):
        pt = {"return_eff": float(rng.uniform(-0.2, 0.2)), "range_eff": float(rng.uniform(0, 0.1)),
              "efficiency": float(rng.uniform(0, 1)), "adx": float(rng.uniform(0, 60))}
        expected = map_composite_label(pt["return_eff"], pt["adx"], pt["range_eff"],
                                       pt["efficiency"], th)
        zero = [lab for lab, cell in cells.items() if rvm.cell_distance_z(pt, scale, cell) == 0.0]
        assert zero == [expected], (pt, zero, expected)


def test_cell_distance_scales_violation_by_feature_std():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    cells = rvm.handrule_cells(dict(TH))
    pt = {"return_eff": 0.0, "range_eff": 0.0, "efficiency": 0.0, "adx": 15.0}
    unit = rvm.cell_distance_z(pt, {"return_eff": 1, "range_eff": 1, "efficiency": 1, "adx": 1},
                               cells["ranging_directional"])
    wide = rvm.cell_distance_z(pt, {"return_eff": 1, "range_eff": 1, "efficiency": 1, "adx": 10},
                               cells["ranging_directional"])
    assert unit == pytest.approx(10.0)
    assert wide == pytest.approx(1.0)


def test_min_cost_assignment_matches_brute_force():
    import itertools
    rng = np.random.default_rng(11)
    for n, m in [(2, 2), (3, 5), (4, 4), (5, 9)]:
        for _ in range(20):
            cost = rng.uniform(0, 10, size=(n, m))
            assign = rvm._min_cost_assignment(cost)
            assert sorted(assign) == sorted(set(assign)) and len(assign) == n
            got = sum(cost[i, assign[i]] for i in range(n))
            best = min(sum(cost[i, p[i]] for i in range(n))
                       for p in itertools.permutations(range(m), n))
            assert got == pytest.approx(best)


def test_min_cost_assignment_rejects_more_rows_than_columns():
    with pytest.raises(ValueError):
        rvm._min_cost_assignment(np.zeros((3, 2)))


def test_uncontested_states_keep_their_handrule_name():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    std = np.ones(4)
    raw = np.array([[0.0, 0.01, 0.1, 10.0],
                    [0.10, 0.10, 0.2, 20.0],
                    [0.20, 0.20, 0.3, 24.0],
                    [-0.10, 0.10, 0.9, 40.0]], dtype=float)
    handrule = ["ranging_quiet", "trending_up_choppy", "trending_up_choppy", "trending_down_clean"]
    names, disp = rvm.assign_distinct_names(raw, std, handrule, th)
    assert names[0] == "ranging_quiet" and names[3] == "trending_down_clean"
    assert disp[0] == 0.0 and disp[3] == 0.0
    assert {names[1], names[2]} >= {"trending_up_choppy"}
    assert len(set(names)) == 4
    moved = 1 if names[1] != "trending_up_choppy" else 2
    assert disp[moved] > 0.0
    assert disp[3 - moved if moved == 1 else 1] == 0.0


def test_collision_moves_the_state_nearest_to_a_free_cell_in_z_units():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    std = np.array([0.05, 0.05, 0.2, 10.0])
    raw = np.array([[0.10, 0.10, 0.10, 10.0],
                    [0.10, 0.10, 0.49, 24.9]], dtype=float)
    handrule = ["trending_up_choppy", "trending_up_choppy"]
    names, disp = rvm.assign_distinct_names(raw, std, handrule, th)
    assert names == ["trending_up_choppy", "trending_up_clean"]
    assert disp[0] == 0.0
    assert disp[1] == pytest.approx(np.hypot(0.01 / 0.2, 0.1 / 10.0))
    unit_names, _ = rvm.assign_distinct_names(raw, np.ones(4), handrule, th)
    assert unit_names[0] == "trending_up_choppy" and unit_names[1] == "ranging_volatile"


def test_ranging_collision_relabels_by_range_toward_volatile():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    std = np.ones(4)
    raw = np.array([[0.0, 0.005, 0.1, 10.0],
                    [0.0, 0.025, 0.1, 10.0]], dtype=float)
    names, _ = rvm.assign_distinct_names(raw, std, ["ranging_quiet", "ranging_quiet"], th)
    assert names == ["ranging_quiet", "ranging_volatile"]


def test_assignment_is_deterministic_across_calls_and_row_order():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    std = np.ones(4)
    raw = np.array([[0.07, 0.14, 0.15, 20.0],
                    [0.15, 0.20, 0.30, 22.0],
                    [0.11, 0.18, 0.20, 30.0]], dtype=float)
    hr = ["trending_up_choppy"] * 3
    a, _ = rvm.assign_distinct_names(raw, std, hr, th)
    b, _ = rvm.assign_distinct_names(raw, std, hr, th)
    assert a == b and len(set(a)) == 3
    perm = [2, 0, 1]
    c, _ = rvm.assign_distinct_names(raw[perm], std, hr, th)
    assert [c[perm.index(i)] for i in range(3)] == a


def test_more_states_than_vocabulary_falls_back_to_handrule_names_and_records_duplicates():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    raw = np.tile(np.array([[0.10, 0.10, 0.10, 10.0]]), (10, 1))
    hr = ["trending_up_choppy"] * 10
    names, disp = rvm.assign_distinct_names(raw, np.ones(4), hr, th)
    assert names == hr and disp == [0.0] * 10
    mapping = {str(i): {"name": names[i], "handrule_name": hr[i], "relabeled": False,
                        "displacement_z": 0.0} for i in range(10)}
    summary = rvm.naming_summary(names, mapping)
    assert summary["distinct"] is False
    assert summary["duplicate_names"] == ["trending_up_choppy"]


def test_fit_unsupervised_records_naming_summary_and_bumps_version():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = _colliding_feature_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert model["version"] == 2
    naming = model["naming"]
    assert naming["rule"] == "distinct_nearest_cell"
    assert naming["distinct"] is True and naming["duplicate_names"] == []
    assert len(naming["relabeled"]) == 1
    (idx, rec), = naming["relabeled"].items()
    assert model["mapping"][idx]["relabeled"] is True
    assert rec["from"] == "trending_up_choppy" and rec["to"] == model["states"][int(idx)]
    assert rec["to"] != "trending_up_choppy"
    untouched = [i for i in range(3) if str(i) != idx]
    for i in untouched:
        assert model["mapping"][str(i)]["relabeled"] is False
        assert model["states"][i] == model["mapping"][str(i)]["handrule_name"]


def test_non_degeneracy_counts_distinct_decoded_states_after_relabel():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = _colliding_feature_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    labels, _ = forward_filter_labels(feats, model)
    valid = ~np.isnan(feats).any(1)
    thr = rvm.NonDegeneracyThresholds(min_active_labels=3, max_occupancy=1.0,
                                      min_transition_rate=0.0)
    rep = rvm.non_degeneracy(labels[valid], thr)
    assert rep["active_labels"] == 3 and rep["ok"] is True
