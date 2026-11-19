import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))


def test_block_shuffle_pvalue_separated_is_significant_and_noise_is_not():
    from regime_diagnostics import block_shuffle_pvalue
    labels = np.array(["up"] * 60 + ["down"] * 60)
    fwd = np.concatenate([np.full(60, 0.02), np.full(60, -0.02)])
    out = block_shuffle_pvalue(labels, fwd, block_len=5, n_perm=100, seed=0)
    assert out["p_value"] < 0.05 and out["kruskal_h"] > 5.0, (
        "test_block_shuffle_pvalue_separated_is_significant")

    rng = np.random.default_rng(0)
    labels = np.array(["a", "b"])[rng.integers(0, 2, 200)]
    fwd = rng.normal(0, 0.01, 200)
    out = block_shuffle_pvalue(labels, fwd, block_len=10, n_perm=100, seed=0)
    assert out["p_value"] > 0.05, "test_block_shuffle_pvalue_noise_not_significant"


def test_per_state_significance():
    from regime_diagnostics import per_state_significance

    labels_sep = np.array(["up"] * 100 + ["down"] * 100)
    fwd_sep = np.concatenate([np.full(100, 0.02), np.full(100, -0.02)])
    result = per_state_significance(labels_sep, fwd_sep, block_len=5, n_perm=200, seed=0)
    assert result["up"]["fdr_reject"] is True
    assert result["down"]["fdr_reject"] is True
    assert result["up"]["gap"] > 0
    assert result["down"]["gap"] < 0

    rng = np.random.default_rng(7)
    labels_noise = np.array(["a", "b"])[rng.integers(0, 2, 300)]
    fwd_noise = rng.normal(0, 0.01, 300)
    result_noise = per_state_significance(labels_noise, fwd_noise, block_len=10, n_perm=200, seed=0)
    n_reject = sum(1 for v in result_noise.values() if v["fdr_reject"])
    assert n_reject <= 1
