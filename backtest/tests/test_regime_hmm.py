import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_hmm import fit_label_anchored_hmm

STATES = ["s0", "s1"]


def test_forward_filter_recovers_regime():
    from regime_hmm import forward_filter_labels
    rng = np.random.default_rng(1)
    feats = np.vstack([rng.normal(0, 1, (60, 4)), rng.normal(4, 1, (60, 4))])
    labels = np.array(["s0"] * 60 + ["s1"] * 60, dtype=object)
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=8)
    lab, conf = forward_filter_labels(feats, m)
    assert (lab[20:55] == "s0").mean() > 0.8 and (lab[80:115] == "s1").mean() > 0.8
    assert (conf >= 0).all() and (conf <= 1.0 + 1e-9).all()


def _noisy_fixture(seed=3, n=60, sep=0.7):
    rng = np.random.default_rng(seed)
    feats = np.vstack([rng.normal(0, 1, (n, 4)), rng.normal(sep, 1, (n, 4))])
    labels = np.array(["s0"] * n + ["s1"] * n, dtype=object)
    feats[17] = np.nan
    return feats, labels


def test_decode_options_reject_negative_values():
    import pytest
    from regime_hmm import forward_filter_labels
    feats, labels = _noisy_fixture()
    m = fit_label_anchored_hmm(feats, labels, STATES, filter_window=4)
    with pytest.raises(ValueError):
        forward_filter_labels(feats, {**m, "decode_min_dwell": -1})
    with pytest.raises(ValueError):
        forward_filter_labels(feats, {**m, "decode_stickiness": -0.5})
