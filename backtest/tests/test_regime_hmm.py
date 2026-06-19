# backtest/tests/test_regime_hmm.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_hmm import fit_label_anchored_hmm, stationary_distribution

STATES = ["s0", "s1"]


def test_stationary_distribution_sums_to_one():
    A = np.array([[0.9, 0.1], [0.2, 0.8]])
    pi = stationary_distribution(A)
    assert abs(pi.sum() - 1.0) < 1e-9 and (pi > 0).all()
    np.testing.assert_allclose(pi @ A, pi, atol=1e-9)  # stationary


def test_fit_shapes_and_determinism():
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (80, 4)), rng.normal(4, 1, (80, 4))])
    labels = np.array(["s0"] * 80 + ["s1"] * 80, dtype=object)
    m1 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    m2 = fit_label_anchored_hmm(feats, labels, STATES, filter_window=16)
    assert m1["states"] == STATES
    assert len(m1["emissions"]) == 2
    assert np.array(m1["transition"]).shape == (2, 2)
    assert m1["emissions"][0]["mean"] == m2["emissions"][0]["mean"]  # deterministic
    # standardized means: s0 cluster ~ negative, s1 ~ positive on each feature
    assert m1["emissions"][0]["mean"][0] < m1["emissions"][1]["mean"][0]


def test_fit_drops_nan_rows():
    feats = np.array([[np.nan, np.nan, np.nan, np.nan], [0.0, 0, 0, 0], [1.0, 1, 1, 1]])
    labels = np.array(["s0", "s0", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=2)
    assert m["emissions"][0]["n"] == 1  # the NaN s0 row was dropped


def test_fit_no_spurious_transition_across_midseries_nan():
    # #1078: a mid-series NaN (low-ATR) bar between a pure s0 run and a pure s1 run must NOT
    # be counted as an s0->s1 transition. Layout: s0 s0 s0 [NaN] s1 s1 s1.
    feats = np.array([[0.0], [0.0], [0.0], [np.nan], [1.0], [1.0], [1.0]])
    labels = np.array(["s0", "s0", "s0", "s0", "s1", "s1", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4, laplace=1.0)
    A = np.array(m["transition"])
    # Raw counts with the gap skipped: s0->s0 = 2, s0->s1 = 0. With laplace=1 the s0 row is
    # [3, 1] -> [0.75, 0.25]. The buggy NaN-dropped zip would have counted one s0->s1 (-> 0.4).
    assert abs(A[0][1] - 0.25) < 1e-9
    assert abs(A[0][0] - 0.75) < 1e-9


def test_fit_counts_genuine_adjacent_transition():
    # Control for the fix above: with NO NaN gap the s0->s1 boundary transition IS counted,
    # so the fix suppresses only spurious NaN-spanned adjacencies, not real ones.
    feats = np.array([[0.0], [0.0], [0.0], [1.0], [1.0], [1.0]])
    labels = np.array(["s0", "s0", "s0", "s1", "s1", "s1"], dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4, laplace=1.0)
    A = np.array(m["transition"])
    # Raw s0->s0 = 2, s0->s1 = 1 -> s0 row [3, 2] -> [0.6, 0.4].
    assert abs(A[0][1] - 0.4) < 1e-9


def test_forward_filter_look_ahead_safe():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(0)
    # 120-bar series; filter_window=8; assert through bar k=70, mutate from k+1=71
    # so even a +1 look-ahead from bar 70 would hit the mutated region.
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab_a, _ = forward_filter_labels(feats, m)
    perturbed = feats.copy()
    perturbed[71:] += 100.0  # mutate from bar 71, immediately after the last asserted bar
    lab_b, _ = forward_filter_labels(perturbed, m)
    assert list(lab_a[:71]) == list(lab_b[:71])  # labels 0..70 unchanged by future


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
    feats[50] = np.nan  # a low-ATR bar inside the s1 stretch
    lab, _ = forward_filter_labels(feats, m)
    assert lab[50] in STATES  # no crash; carried, not undefined
