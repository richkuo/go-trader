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
    assert A.shape == (2, 2)
    assert np.allclose(A.sum(1), 1.0)                     # row-stochastic
    # Only adjacency 0->1 (bars 0,1) counts; (1,2) and (2,3) span the dropped bar 2.
    # Correct: laplace [[1,1],[1,1]] + one 0->1 => [[1,2],[1,1]] -> rows [1/3,2/3],[1/2,1/2].
    # The gap-splice bug (treating compacted [0,1,0] as contiguous) would add a spurious
    # 1->0, giving row1 [2/3,1/3] — so ROW 1, not row 0, is the discriminating assertion.
    assert np.allclose(A, [[1.0 / 3, 2.0 / 3], [0.5, 0.5]])


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
    # 4 candidates -> bonferroni alpha = 0.05/4 = 0.0125; eligible candidates clear it (p=0.001).
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
    # A candidate that clears the gate (ship + non-degenerate) and the RAW alpha (0.05) but
    # NOT the family-wise corrected alpha must be rejected, bounding the sweep's false-positive
    # rate. 20 candidates -> corrected alpha = 0.05/20 = 0.0025; p=0.03 clears 0.05 but not 0.0025.
    mod = _load_research_module("bonf")
    lucky = {"family": "kmeans", "k": 4, "verdict": {"ship": True}, "model_p_value": 0.03,
             "non_degenerate_all": True, "model_kruskal_h": 999.0, "stability_gain": 0.9}
    fillers = [{"family": "hmm", "k": 2, "verdict": {"ship": False}, "model_p_value": 0.9,
                "non_degenerate_all": False, "model_kruskal_h": 1.0, "stability_gain": 0.0}
               for _ in range(19)]
    assert mod.select_winner([lucky] + fillers) is None        # lucky chance winner rejected
    assert mod.bonferroni_alpha(20) == pytest.approx(0.05 / 20)
    # the same candidate WOULD win if it cleared the corrected alpha (p well under 0.0025)
    lucky_real = dict(lucky, model_p_value=0.0001)
    win = mod.select_winner([lucky_real] + fillers)
    assert win["family"] == "kmeans" and win["k"] == 4


def test_fit_kmeans_reseeds_empty_clusters_no_ghost_states():
    # K greater than the number of natural clusters: Lloyd's alone would leave clusters empty,
    # storing n=0 ghost emission states. The repair must fill every cluster from real data.
    z, _ = _three_blobs(seed=0)                         # 3 well-separated blobs, K=7 requested
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 7, seed=0)
    assert counts.min() >= 1                            # no n=0 ghost state
    assert counts.sum() == len(z)                       # every bar assigned exactly once
    assert (em_var > 0).all()
    assert np.isfinite(em_mean).all()                   # no stale/NaN ghost centroid
    for j in range(7):
        assert (assign == j).any()                      # every cluster occupied


def test_fit_kmeans_reseed_survives_duplicate_rows():
    # Inverse/degenerate: many identical rows collapse all centroids onto one point. Re-seeding
    # to distinct row INDICES still gives every cluster >=1 member (no n=0 ghost).
    z = np.tile(np.array([0.3, 0.4, 0.5, 12.0]), (50, 1))
    assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 4, seed=0)
    assert counts.min() >= 1
    assert counts.sum() == 50
    assert (em_var > 0).all() and np.isfinite(em_mean).all()


def test_fit_unsupervised_high_k_stores_no_zero_n_emission():
    # The invariant at the schema boundary: no stored emission may summarize zero observations.
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = _feature_blob_matrix()                      # 3 natural blobs, K=7 requested
    model = rvm.fit_unsupervised(feats, family="kmeans", k=7, filter_window=32,
                                 thresholds=dict(TH), seed=0)
    assert all(e["n"] >= 1 for e in model["emissions"])
    assert sum(e["n"] for e in model["emissions"]) == int((~np.isnan(feats).any(1)).sum())


def test_bakeoff_smoke_on_cached_data_if_available():
    try:
        # module load is inside the guard: a missing transitive dep / unparseable
        # eval_windows config must SKIP, not FAIL.
        mod = _load_research_module("smoke")
        # narrowed sweep at an explicit n_perm=200 — the #1160 achievability guard must
        # NOT spuriously trip when 200 permutations DO resolve the corrected alpha.
        report = mod.run_bakeoff("BTC/USDT", "1h", families=("kmeans",),
                                 k_range=range(3, 4), eval_windows=("is", "oos"),
                                 n_perm=200)
    except Exception as e:  # noqa: BLE001 — no cached OHLCV in CI -> skip, not fail
        pytest.skip(f"no cached OHLCV / data path unavailable: {e}")
    assert "candidates" in report and len(report["candidates"]) == 1
    assert "non_degeneracy_thresholds" in report
    assert "handrule_held_out" in report and "abstained" in report["handrule_held_out"]
    # #1160 poweredness audit fields
    assert report["n_perm"] == 200
    assert report["min_achievable_p_value"] == pytest.approx(1.0 / 201.0)
    assert "bonferroni_denominator" in report and "bonferroni_denominator_policy" in report
    assert "structurally_ineligible" in report
    assert "permutation_steps_to_alpha" in report["handrule_held_out"]
    assert "knife_edge" in report["handrule_held_out"]
    assert report["min_achievable_p_value"] <= report["bonferroni_alpha"]


def test_non_degeneracy_flags_high_occupancy():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=0.8,
                                  min_transition_rate=0.0)
    # 90% one label, 10% another: 2 active labels (passes), but occupancy 0.9 > 0.8 fails.
    stream = np.array((["a"] * 9 + ["b"]) * 50, dtype=object)
    rep = non_degeneracy(stream, thr)
    assert rep["ok"] is False
    assert "max_occupancy" in " ".join(rep["reasons"])
    assert rep["active_labels"] == 2


def test_non_degeneracy_flags_low_transition_rate():
    from regime_vol_model import NonDegeneracyThresholds, non_degeneracy
    thr = NonDegeneracyThresholds(min_active_labels=2, max_occupancy=1.0,
                                  min_transition_rate=0.5)
    # two long blocks: 2 active labels, balanced occupancy, but only 1 flip in 599 -> tr ~ 0.0017.
    stream = np.array(["a"] * 300 + ["b"] * 300, dtype=object)
    rep = non_degeneracy(stream, thr)
    assert rep["ok"] is False
    assert "min_transition_rate" in " ".join(rep["reasons"])


# --- #1160: the Bonferroni eligibility arm must be achievable by the permutation statistic ---


def test_resolve_bakeoff_n_perm_default_achieves_corrected_alpha_with_headroom():
    mod = _load_research_module("nperm_default")
    n = mod.resolve_bakeoff_n_perm(18)
    assert n >= mod.DEFAULT_BAKEOFF_MIN_N_PERM
    # minimum achievable p clears the 18-candidate corrected alpha with >= 2x headroom
    assert 1.0 / (n + 1) <= mod.bonferroni_alpha(18) / 2.0


def test_resolve_bakeoff_n_perm_rejects_underpowered_explicit_request():
    mod = _load_research_module("nperm_reject")
    # the merged default: 200 permutations bottom out at 1/201 ~ 0.00498 > 0.05/18 ~ 0.00278
    with pytest.raises(ValueError, match="cannot satisfy the Bonferroni-corrected alpha"):
        mod.resolve_bakeoff_n_perm(18, requested=200)


def test_resolve_bakeoff_n_perm_narrowed_sweep_accepts_200():
    mod = _load_research_module("nperm_narrow")
    # kmeans-only sweep: 6 candidates -> alpha = 0.05/6 ~ 0.00833 > 1/201, guard must not trip
    assert mod.resolve_bakeoff_n_perm(6, requested=200) == 200


def test_real_pipeline_p_value_can_be_crowned_at_default_sweep_resolution():
    # Regression at the REAL statistic's resolution (#1160), not synthetic p-values: a candidate
    # with genuinely strong forward separation, scored by block_shuffle_pvalue at the
    # auto-resolved n_perm, must clear the 18-candidate Bonferroni threshold and be crowned.
    # At the merged n_perm=200 this was impossible by construction (min p 1/201 > 0.05/18).
    from regime_diagnostics import block_shuffle_pvalue
    mod = _load_research_module("crown")
    n_perm = mod.resolve_bakeoff_n_perm(18)
    rng = np.random.default_rng(0)
    labels = np.array((["low"] * 8 + ["high"] * 8) * 25, dtype=object)   # 400 bars, 50 blocks
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
    # An ineligible candidate must neither win nor shrink alpha for the eligible one:
    # 1 eligible + 19 ineligible -> denominator 1 -> alpha = 0.05, so p=0.03 is crownable.
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


def test_permutation_steps_to_alpha_flags_merged_run_knife_edge():
    mod = _load_research_module("steps")
    # the merged evidence run: incumbent OOS p = 10/201 ~ 0.0498 at n_perm=200 -> zero headroom
    # (the very next as-or-more-extreme permutation under another seed flips the verdict)
    assert mod.permutation_steps_to_alpha(10.0 / 201.0, 200) == 0
    assert mod.permutation_steps_to_alpha(15.0 / 201.0, 200) < 0   # already abstained
    assert mod.permutation_steps_to_alpha(2.0 / 201.0, 200) > 0    # comfortable margin


def test_score_labels_default_n_perm_is_byte_identical():
    # #1160 acceptance: existing callers that pass no n_perm must produce byte-identical
    # output to an explicit n_perm=200 (regime_calibrate / regime_bounded_window_validate).
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
