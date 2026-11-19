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


_FITTERS_RECOVER_THREE_BLOBS_CASES = [("kmeans", 0), ("gmm", 1)]


def test_fitters_recover_three_blobs(subtests):
    for i, (fitter, seed) in enumerate(_FITTERS_RECOVER_THREE_BLOBS_CASES):
        with subtests.test(case=i, fitter=fitter, seed=seed):
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


def test_map_latent_to_names_uses_canonical_boundaries_and_is_deterministic(subtests):
    with subtests.test(case="test_map_latent_to_names_uses_canonical_boundaries_and_is_deterministic"):
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

    with subtests.test(case="test_volatility_rank_orders_by_range_eff"):
        from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
        mean = np.zeros(4); std = np.ones(4)
        em = np.array([[0.0, 0.8, 0.1, 10.0],
                       [0.0, 0.01, 0.1, 10.0],
                       [0.0, 0.3, 0.1, 10.0]], dtype=float)
        _, mapping = rvm.map_latent_to_names(em, mean, std, dict(TH))
        ranks = {int(i): m["volatility_rank"] for i, m in mapping.items()}
        assert ranks[1] < ranks[2] < ranks[0]


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


def test_fit_kmeans_reseeds_empty_clusters_no_ghost_states(subtests):
    with subtests.test(case="test_fit_kmeans_reseeds_empty_clusters_no_ghost_states"):
        z, _ = _three_blobs(seed=0)
        assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 7, seed=0)
        assert counts.min() >= 1
        assert counts.sum() == len(z)
        assert (em_var > 0).all()
        assert np.isfinite(em_mean).all()
        for j in range(7):
            assert (assign == j).any()

    with subtests.test(case="test_fit_kmeans_reseed_survives_duplicate_rows"):
        z = np.tile(np.array([0.3, 0.4, 0.5, 12.0]), (50, 1))
        assign, em_mean, em_var, counts = rvm.fit_kmeans(z, 4, seed=0)
        assert counts.min() >= 1
        assert counts.sum() == 50
        assert (em_var > 0).all() and np.isfinite(em_mean).all()


def _bakeoff_ohlcv(n, seed):
    import pandas as pd
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


def test_neutralize_dead_components_parks_by_soft_mass_and_is_noop_when_all_alive():
    case = "test_neutralize_dead_components_classifies_by_soft_mass_not_geometry"
    mu = np.array([[0.0, 0.0, 0.0, 0.0],
                   [0.0, 0.0, 0.0, 0.0],
                   [3.0, 3.0, 3.0, 3.0]], dtype=float)
    var = np.array([[1e-3, 1e-3, 1e-3, 1e-3],
                    [1e-3, 1e-3, 1e-3, 1e-3],
                    [0.4, 0.4, 0.4, 0.4]], dtype=float)
    dead = rvm._neutralize_dead_components(mu, var, np.array([1e-9, 250.0, 250.0]),
                                           min_soft_mass=1.0)
    assert dead.tolist() == [True, False, False], case
    assert np.array_equal(mu[0], [0.0, 0.0, 0.0, 0.0]) and np.array_equal(var[0], [1.0, 1.0, 1.0, 1.0]), case
    assert np.array_equal(var[1], [1e-3, 1e-3, 1e-3, 1e-3]), case
    assert np.array_equal(mu[2], [3.0, 3.0, 3.0, 3.0]) and np.array_equal(var[2], [0.4, 0.4, 0.4, 0.4]), case

    case = "test_neutralize_dead_components_is_noop_when_all_components_alive"
    mu = np.array([[1.0, 2.0, 3.0, 4.0], [-1.0, -2.0, -3.0, -4.0]], dtype=float)
    var = np.array([[0.5, 0.6, 0.7, 0.8], [0.9, 1.1, 1.2, 1.3]], dtype=float)
    mu0, var0 = mu.copy(), var.copy()
    dead = rvm._neutralize_dead_components(mu, var, np.array([500.0, 500.0]), min_soft_mass=1.0)
    assert not dead.any(), case
    assert np.array_equal(mu, mu0) and np.array_equal(var, var0), case


def _colliding_feature_matrix(seed=0):
    rng = np.random.default_rng(seed)
    centers = np.array([[0.0, 0.02, 0.1, 8.0],
                        [0.07, 0.14, 0.15, 20.0],
                        [0.15, 0.20, 0.30, 22.0]], dtype=float)
    rows = [rng.normal(c, [0.005, 0.01, 0.02, 1.0], size=(150, 4)) for c in centers]
    return np.vstack([np.full((5, 4), np.nan), *rows])


_FITTED_MODEL_WITH_COLLIDING_CENTROIDS_KEEPS_EVERY_LATENT_STATE_DISTINCT_CASES = ["kmeans", "gmm", "hmm"]


def test_fitted_model_with_colliding_centroids_keeps_every_latent_state_distinct(subtests):
    for i, family in enumerate(_FITTED_MODEL_WITH_COLLIDING_CENTROIDS_KEEPS_EVERY_LATENT_STATE_DISTINCT_CASES):
        with subtests.test(case=i, family=family):
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


def test_min_cost_assignment_matches_brute_force_and_rejects_more_rows_than_columns():
    import itertools
    rng = np.random.default_rng(11)
    for n, m in [(2, 2), (3, 5), (4, 4), (5, 9)]:
        for _ in range(20):
            cost = rng.uniform(0, 10, size=(n, m))
            assign = rvm._min_cost_assignment(cost)
            case = f"test_min_cost_assignment_matches_brute_force n={n} m={m}"
            assert sorted(assign) == sorted(set(assign)) and len(assign) == n, case
            got = sum(cost[i, assign[i]] for i in range(n))
            best = min(sum(cost[i, p[i]] for i in range(n))
                       for p in itertools.permutations(range(m), n))
            assert got == pytest.approx(best), case

    with pytest.raises(ValueError):
        rvm._min_cost_assignment(np.zeros((3, 2)))


def test_uncontested_states_keep_their_handrule_name(subtests):
    with subtests.test(case="test_uncontested_states_keep_their_handrule_name"):
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

    with subtests.test(case="test_collision_moves_the_state_nearest_to_a_free_cell_in_z_units"):
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

    with subtests.test(case="test_ranging_collision_relabels_by_range_toward_volatile"):
        from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
        th = dict(TH)
        std = np.ones(4)
        raw = np.array([[0.0, 0.005, 0.1, 10.0],
                        [0.0, 0.025, 0.1, 10.0]], dtype=float)
        names, _ = rvm.assign_distinct_names(raw, std, ["ranging_quiet", "ranging_quiet"], th)
        assert names == ["ranging_quiet", "ranging_volatile"]

    with subtests.test(case="test_assignment_is_deterministic_across_calls_and_row_order"):
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

    with subtests.test(case="test_more_states_than_vocabulary_falls_back_to_handrule_names_and_records_duplicates"):
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


def test_kmeans_colliding_fit_records_naming_summary_and_decodes_distinct_states():
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS as TH
    feats = _colliding_feature_matrix()
    model = rvm.fit_unsupervised(feats, family="kmeans", k=3, filter_window=32,
                                 thresholds=dict(TH), seed=0)

    case = "test_fit_unsupervised_records_naming_summary_and_bumps_version"
    assert model["version"] == 2, case
    naming = model["naming"]
    assert naming["rule"] == "distinct_nearest_cell", case
    assert naming["distinct"] is True and naming["duplicate_names"] == [], case
    assert len(naming["relabeled"]) == 1, case
    (idx, rec), = naming["relabeled"].items()
    assert model["mapping"][idx]["relabeled"] is True, case
    assert rec["from"] == "trending_up_choppy" and rec["to"] == model["states"][int(idx)], case
    assert rec["to"] != "trending_up_choppy", case
    untouched = [i for i in range(3) if str(i) != idx]
    for i in untouched:
        assert model["mapping"][str(i)]["relabeled"] is False, case
        assert model["states"][i] == model["mapping"][str(i)]["handrule_name"], case

    case = "test_non_degeneracy_counts_distinct_decoded_states_after_relabel"
    labels, _ = forward_filter_labels(feats, model)
    valid = ~np.isnan(feats).any(1)
    thr = rvm.NonDegeneracyThresholds(min_active_labels=3, max_occupancy=1.0,
                                      min_transition_rate=0.0)
    rep = rvm.non_degeneracy(labels[valid], thr)
    assert rep["active_labels"] == 3 and rep["ok"] is True, case
