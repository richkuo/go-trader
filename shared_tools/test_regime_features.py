import numpy as np
import pandas as pd
from shared_tools.conftest import load_module

_REGIME = load_module("_regime_features_test", __file__.replace("test_regime_features.py", "regime.py"))
composite_feature_matrix = _REGIME.composite_feature_matrix
compute_regime_composite = _REGIME.compute_regime_composite
map_composite_label = _REGIME.map_composite_label
_DEFAULT_COMPOSITE_THRESHOLDS = _REGIME._DEFAULT_COMPOSITE_THRESHOLDS


def _synth(n=300, seed=1):
    rng = np.random.default_rng(seed)
    steps = rng.normal(0, 1, n).cumsum()
    close = 100 + steps
    high = close + np.abs(rng.normal(0, 0.5, n))
    low = close - np.abs(rng.normal(0, 0.5, n))
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    return pd.DataFrame({"open": close, "high": high, "low": low, "close": close}, index=idx)


def test_feature_matrix_reproduces_handrule_labels():
    df = _synth()
    period = 48
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th)
    labels = compute_regime_composite(df, period=period, thresholds=th)["regime"]
    assert list(feats.columns) == ["return_eff", "range_eff", "efficiency", "adx"]
    assert feats.iloc[:period].isna().all().all()
    for i in range(period, len(df)):
        row = feats.iloc[i]
        if row.isna().any():
            continue
        got = map_composite_label(row["return_eff"], row["adx"], row["range_eff"], row["efficiency"], th)
        assert got == labels.iloc[i], f"bar {i}: {got} != {labels.iloc[i]}"
