# backtest/tests/test_regime_diagnostics.py
import os, sys
import numpy as np
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_diagnostics import (
    forward_returns, forward_realized_vol, coverage, separation, stability,
)


def test_forward_returns_horizon():
    close = np.array([100.0, 110, 121])
    fr = forward_returns(close, 1)
    np.testing.assert_allclose(fr[:2], [0.10, 0.10])
    assert np.isnan(fr[-1])


def test_forward_realized_vol_horizon1():
    # log prices [0, 0.1, 0.3, 0.6] -> 1-bar log returns [0, 0.1, 0.2, 0.3].
    # forward realized vol at h=1 is |next log return|: [0.1, 0.2, 0.3, NaN].
    close = np.exp(np.array([0.0, 0.1, 0.3, 0.6]))
    fv = forward_realized_vol(close, 1)
    np.testing.assert_allclose(fv[:3], [0.1, 0.2, 0.3], atol=1e-12)
    assert np.isnan(fv[-1])


def test_forward_realized_vol_horizon2_sums_squared_log_returns():
    close = np.exp(np.array([0.0, 0.1, 0.3, 0.6]))
    fv = forward_realized_vol(close, 2)
    # out[0] = sqrt(0.1^2 + 0.2^2) = sqrt(0.05); out[1] = sqrt(0.2^2 + 0.3^2) = sqrt(0.13)
    np.testing.assert_allclose(fv[:2], [np.sqrt(0.05), np.sqrt(0.13)], atol=1e-12)
    assert np.isnan(fv[2]) and np.isnan(fv[3])  # last `horizon` bars have no full window


def test_forward_realized_vol_separates_quiet_from_volatile():
    # A quiet leg (tiny moves) then a volatile leg (large moves): forward vol must rank
    # the volatile-state bars far above the quiet-state bars (the regime's real signal).
    rng = np.random.default_rng(0)
    quiet = rng.normal(0, 0.001, 200)
    loud = rng.normal(0, 0.02, 200)
    close = 100 * np.exp(np.cumsum(np.concatenate([quiet, loud])))
    labels = np.array(["quiet"] * 200 + ["loud"] * 200, dtype=object)
    fv = forward_realized_vol(close, 4)
    sep = separation(labels, fv)
    assert sep["per_state"]["loud"]["mean"] > sep["per_state"]["quiet"]["mean"]
    assert sep["kruskal_h"] > 5.0


def test_coverage_counts():
    labels = np.array(["a", "a", "b", "a"])
    cov = coverage(labels)
    assert cov["a"]["count"] == 3 and abs(cov["a"]["pct"] - 0.75) < 1e-9


def test_separation_directional():
    labels = np.array(["up"] * 50 + ["down"] * 50)
    fwd = np.concatenate([np.full(50, 0.02), np.full(50, -0.02)])
    sep = separation(labels, fwd)
    assert sep["per_state"]["up"]["mean"] > 0 > sep["per_state"]["down"]["mean"]
    assert sep["kruskal_h"] > 5.0


def test_stability_transition_rate():
    labels = np.array(["a", "a", "b", "b", "a"])
    st = stability(labels)
    assert abs(st["transition_rate"] - 0.5) < 1e-9  # 2 flips over 4 transitions
    assert st["flips"] == 2


def test_block_shuffle_pvalue_separated_is_significant():
    from regime_diagnostics import block_shuffle_pvalue
    labels = np.array(["up"] * 60 + ["down"] * 60)
    fwd = np.concatenate([np.full(60, 0.02), np.full(60, -0.02)])
    out = block_shuffle_pvalue(labels, fwd, block_len=5, n_perm=100, seed=0)
    assert out["p_value"] < 0.05 and out["kruskal_h"] > 5.0


def test_block_shuffle_pvalue_noise_not_significant():
    from regime_diagnostics import block_shuffle_pvalue
    rng = np.random.default_rng(0)
    labels = np.array(["a", "b"])[rng.integers(0, 2, 200)]
    fwd = rng.normal(0, 0.01, 200)
    out = block_shuffle_pvalue(labels, fwd, block_len=10, n_perm=100, seed=0)
    assert out["p_value"] > 0.05


def test_kmeans_yardstick_runs():
    from regime_diagnostics import kmeans_yardstick
    rng = np.random.default_rng(0)
    feats = np.vstack([rng.normal(0, 1, (100, 4)), rng.normal(5, 1, (100, 4))])
    fwd = np.concatenate([np.full(100, 0.02), np.full(100, -0.02)])
    out = kmeans_yardstick(feats, fwd, k_range=(2, 3), seed=0)
    assert out[2]["kruskal_h"] > 5.0


def test_score_labels_bundle():
    from regime_diagnostics import score_labels
    rng = np.random.default_rng(0)
    close = 100 * np.cumprod(1 + rng.normal(0, 0.005, 300))
    labels = np.array(["up" if r >= 0 else "down" for r in np.diff(close, prepend=close[0])], dtype=object)
    feats = rng.normal(0, 1, (300, 4))
    out = score_labels(close, labels, feats, horizons=(1, 4), block_mult=3, seed=0)
    assert "h4" in out and "kruskal_h" in out["h4"]["separation"]
    assert "coverage" in out and "stability" in out
    assert "p_value" in out["h4"]["significance"]


def test_score_labels_masks_warmup():
    """NaN feature rows are excluded: labeling them 'ranging_quiet' vs 'default_label' is irrelevant."""
    from regime_diagnostics import score_labels
    rng = np.random.default_rng(42)
    n = 120
    close = 100 * np.cumprod(1 + rng.normal(0, 0.005, n))
    base_labels = np.array(
        ["up" if r >= 0 else "down" for r in np.diff(close, prepend=close[0])], dtype=object
    )
    feats = rng.normal(0, 1, (n, 4))
    feats[:10] = np.nan  # warmup bars have NaN features

    labels_a = base_labels.copy()
    labels_a[:10] = "ranging_quiet"
    labels_b = base_labels.copy()
    labels_b[:10] = "default_label"

    out_a = score_labels(close, labels_a, feats, horizons=(4,), seed=0)
    out_b = score_labels(close, labels_b, feats, horizons=(4,), seed=0)

    # warmup-label variants must not appear in coverage
    assert "ranging_quiet" not in out_a["coverage"]
    assert "default_label" not in out_b["coverage"]
    # scoring is identical regardless of which label the warmup bars carry
    assert out_a["coverage"] == out_b["coverage"]
    assert out_a["stability"] == out_b["stability"]
    assert out_a["h4"]["significance"]["p_value"] == out_b["h4"]["significance"]["p_value"]


def test_score_labels_target_volatility_stamped_and_distinct():
    from regime_diagnostics import score_labels
    rng = np.random.default_rng(1)
    # Quiet then volatile leg so the vol target genuinely separates the two label groups.
    steps = np.concatenate([rng.normal(0, 0.001, 150), rng.normal(0, 0.02, 150)])
    close = 100 * np.exp(np.cumsum(steps))
    labels = np.array(["quiet"] * 150 + ["loud"] * 150, dtype=object)
    feats = rng.normal(0, 1, (300, 4))

    vol = score_labels(close, labels, feats, horizons=(4,), seed=0, target="volatility")
    ret = score_labels(close, labels, feats, horizons=(4,), seed=0, target="returns")
    assert vol["target"] == "volatility" and ret["target"] == "returns"
    # Different forward variable -> different separation statistic on the same labels.
    assert vol["h4"]["separation"]["kruskal_h"] != ret["h4"]["separation"]["kruskal_h"]
    # Vol target strongly separates quiet vs loud; the gate reads this KW-H.
    assert vol["h4"]["separation"]["kruskal_h"] > 5.0


def test_score_labels_rejects_unknown_target():
    from regime_diagnostics import score_labels
    import pytest
    close = 100 * np.cumprod(1 + np.zeros(50))
    labels = np.array(["a"] * 50, dtype=object)
    feats = np.zeros((50, 4))
    with pytest.raises(ValueError):
        score_labels(close, labels, feats, horizons=(4,), target="sharpe")


def test_per_state_significance():
    from regime_diagnostics import per_state_significance

    # Clearly separated returns → both states should reject at FDR level
    labels_sep = np.array(["up"] * 100 + ["down"] * 100)
    fwd_sep = np.concatenate([np.full(100, 0.02), np.full(100, -0.02)])
    result = per_state_significance(labels_sep, fwd_sep, block_len=5, n_perm=200, seed=0)
    assert result["up"]["fdr_reject"] is True
    assert result["down"]["fdr_reject"] is True
    assert result["up"]["gap"] > 0  # up mean > global mean
    assert result["down"]["gap"] < 0

    # All-noise: random labels, random returns → few/no rejects expected
    rng = np.random.default_rng(7)
    labels_noise = np.array(["a", "b"])[rng.integers(0, 2, 300)]
    fwd_noise = rng.normal(0, 0.01, 300)
    result_noise = per_state_significance(labels_noise, fwd_noise, block_len=10, n_perm=200, seed=0)
    n_reject = sum(1 for v in result_noise.values() if v["fdr_reject"])
    assert n_reject <= 1  # at most 1 false positive under noise
