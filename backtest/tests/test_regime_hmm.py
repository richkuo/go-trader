import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_hmm import fit_label_anchored_hmm, stationary_distribution

STATES = ["s0", "s1"]


def test_stationary_distribution_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = stationary_distribution(A)
    assert abs(pi.sum() - 1.0) < 1e-9 and (pi > 0).all()
    np.testing.assert_allclose(pi @ A, pi, atol=1e-9)


def test_fit_shapes_and_determinism():
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (80, 4)), rng.normal(4, 1, (80, 4))])
    labels = np.array(["s0"] * 80 + ["s1"] * 80, dtype=object)
    m1 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    m2 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    assert m1["states"] == STATES
    assert len(m1["emissions"]) == 2
    assert np.array(m1["transition"]).shape == (2, 2)
    assert m1["emissions"][0]["mean"] == m2["emissions"][0]["mean"]
    assert m1["emissions"][0]["mean"][0] < m1["emissions"][1]["mean"][0]


def test_fit_drops_nan_rows():
    feats = np.array([[np.nan, np.nan, np.nan, np.nan], [0.0, 0, 0, 0], [1.0, 1, 1, 1]])
    labels = np.array(["s0", "s0", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    assert m["emissions"][0]["n"] == 1


def test_fit_no_spurious_transition_across_midseries_nan():
    feats = np.array([[0.0], [0.0], [0.0], [np.nan], [1.0], [1.0], [1.0]])
    labels = np.array(["s0", "s0", "s0", "s0", "s1", "s1", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4, laplace=1.0)
    A = np.array(m["transition"])
    assert abs(A[0][1] - 0.25) < 1e-9
    assert abs(A[0][0] - 0.75) < 1e-9


def test_fit_counts_genuine_adjacent_transition():
    feats = np.array([[0.0], [0.0], [0.0], [1.0], [1.0], [1.0]])
    labels = np.array(["s0", "s0", "s0", "s1", "s1", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4, laplace=1.0)
    A = np.array(m["transition"])
    assert abs(A[0][1] - 0.4) < 1e-9


def test_forward_filter_look_ahead_safe():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab_a, _ = forward_filter_labels(feats, m)
    perturbed = feats.copy()
    perturbed[71:] += 100.0
    lab_b, _ = forward_filter_labels(perturbed, m)
    assert list(lab_a[:71]) == list(lab_b[:71])


def test_forward_filter_recovers_regime():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(1)
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab, conf = forward_filter_labels(feats, m)
    assert (lab[20:55] == "s0").mean() > 0.8 and (lab[80:115] == "s1").mean() > 0.8
    assert (conf >= 0).all() and (conf <= 1.0 + 1e-9).all()


def test_forward_filter_nan_carry():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(2)
    feats = np.vstack([rng.normal(0, 1, (40, 4)), rng.normal(4, 1, (40, 4))])
    labels = np.array(["s0"] * 40 + ["s1"] * 40, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=6)
    feats[50] = np.nan
    lab, _ = forward_filter_labels(feats, m)
    assert lab[50] in STATES


def _reference_forward_filter(features, model):
    from regime_hmm import _logsumexp
    features = np.asarray(features, dtype=float)
    n = len(features)
    states = list(model["states"])
    k = len(states)
    mean = np.asarray(model["feature_means"], dtype=float)
    std = np.asarray(model["feature_stds"], dtype=float)
    em_mean = np.array([e["mean"] for e in model["emissions"]], dtype=float)
    em_var = np.array([e["var"] for e in model["emissions"]], dtype=float)
    log_init = np.log(np.asarray(model["init"], dtype=float) + 1e-300)
    log_A = np.log(np.asarray(model["transition"], dtype=float) + 1e-300)
    w = int(model["filter_window"])
    default_label = states[int(np.argmax(model["init"]))]
    labels = np.array([default_label] * n, dtype=object)
    conf = np.zeros(n, dtype=float)
    for i in range(n):
        lo = max(0, i - w + 1)
        alpha = log_init.copy()
        seen = False
        for t in range(lo, i + 1):
            x = features[t]
            pred = np.array([_logsumexp(alpha + log_A[:, j]) for j in range(k)])
            if np.isnan(x).any():
                alpha = pred
                continue
            z = (x - mean) / std
            log_emit = -0.5 * (np.log(2 * np.pi * em_var) + (z - em_mean) ** 2 / em_var).sum(1)
            alpha = pred + log_emit
            alpha -= _logsumexp(alpha)
            seen = True
        if seen:
            j = int(np.argmax(alpha))
            labels[i] = states[j]
            conf[i] = float(np.exp(alpha[j] - _logsumexp(alpha)))
    return labels, conf


def _noisy_fixture(seed=3, n=60, sep=0.7):
    rng = np.random.default_rng(seed)
    feats = np.vstack([rng.normal(0, 1, (n, 4)), rng.normal(sep, 1, (n, 4))])
    labels = np.array(["s0"] * n + ["s1"] * n, dtype=object)
    feats[17] = np.nan
    return feats, labels


def _transition_rate(labels):
    labels = np.asarray(labels, dtype=object)
    return float((labels[1:] != labels[:-1]).sum() / (len(labels) - 1))


def test_forward_filter_without_decode_options_matches_reference_decoder():
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    ref_lab, ref_conf = _reference_forward_filter(feats, m)
    for model in (m, {**m, "decode_min_dwell": 0, "decode_stickiness": 0.0},
                  {**m, "decode_min_dwell": None, "decode_stickiness": None},
                  {**m, "decode_min_dwell": 1}):
        lab, conf = forward_filter_labels(feats, model)
        assert lab.tolist() == ref_lab.tolist()
        assert conf.tobytes() == ref_conf.tobytes()


def test_decode_min_dwell_holds_label_until_run_persists():
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    base, _ = forward_filter_labels(feats, m)
    assert _transition_rate(base) > 0.05
    prev = _transition_rate(base)
    for dwell in (2, 4, 8):
        lab, conf = forward_filter_labels(feats, {**m, "decode_min_dwell": dwell})
        assert set(lab.tolist()) <= set(STATES)
        assert (conf >= 0).all() and (conf <= 1.0 + 1e-9).all()
        rate = _transition_rate(lab)
        assert rate <= prev
        prev = rate
    held, _ = forward_filter_labels(feats, {**m, "decode_min_dwell": 3})
    assert (held[80:115] == "s1").mean() > 0.8


def test_decode_min_dwell_is_look_ahead_safe():
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = {**fit_label_anchored_hmm(feats, labels, STATES, filter_window=8), "decode_min_dwell": 3}
    lab_a, _ = forward_filter_labels(feats, m)
    perturbed = feats.copy()
    perturbed[71:] += 100.0
    lab_b, _ = forward_filter_labels(perturbed, m)
    assert list(lab_a[:71]) == list(lab_b[:71])


def test_decode_stickiness_reduces_churn_without_mutating_the_model():
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    before = [row[:] for row in m["transition"]]
    base, _ = forward_filter_labels(feats, m)
    sticky, conf = forward_filter_labels(feats, {**m, "decode_stickiness": 5.0})
    assert _transition_rate(sticky) < _transition_rate(base)
    assert set(sticky.tolist()) <= set(STATES)
    assert (conf >= 0).all() and (conf <= 1.0 + 1e-9).all()
    assert m["transition"] == before


def test_decode_options_reject_negative_values():
    import pytest
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4)
    with pytest.raises(ValueError):
        forward_filter_labels(feats, {**m, "decode_min_dwell": -1})
    with pytest.raises(ValueError):
        forward_filter_labels(feats, {**m, "decode_stickiness": -0.5})
