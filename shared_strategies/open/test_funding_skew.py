
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

_FUNDING_SKEW = load_module("_funding_skew_test", __file__.replace("test_funding_skew.py", "funding_skew.py"))
funding_skew_core = _FUNDING_SKEW.funding_skew_core


def make_ohlcv(closes, noise=0.5, tz="UTC"):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, 100.0),
    }, index=pd.date_range("2026-01-01", periods=n, freq="1h", tz=tz))


def make_short_squeeze():
    n = 400
    rng = np.random.RandomState(3)
    closes = 100 + np.cumsum(rng.randn(n)) * 0.05
    funding = rng.randn(n) * 2e-6
    funding[250:271] = -8e-5
    closes[245:300] = closes[245] + np.linspace(0, 6, 55)
    df = make_ohlcv(closes)
    df["funding_rate"] = funding
    return df


def make_long_breakdown():
    n = 400
    rng = np.random.RandomState(9)
    closes = 100 + np.cumsum(rng.randn(n)) * 0.05
    funding = rng.randn(n) * 2e-6
    funding[250:271] = 8e-5
    closes[245:300] = closes[245] - np.linspace(0, 6, 55)
    df = make_ohlcv(closes)
    df["funding_rate"] = funding
    return df


def test_crowded_longs_with_breakdown_goes_short():
    out = funding_skew_core(make_long_breakdown(), funding_window=96, z_entry=1.5, confirm_ema=20)
    shorts = np.where(out["signal"].values == -1)[0]
    assert len(shorts) > 0 and shorts[0] == 250
    assert (out["position"] == -1).any()
    assert out["funding_z"].iloc[250] >= 1.5


def test_records_alignment_is_backward_only():
    df = make_ohlcv([100.0] * 5).drop(columns=[], errors="ignore")
    recs = [{"rate": 5e-5, "time": int(df.index[2].timestamp() * 1000) + 1}]
    out = funding_skew_core(df, funding_records=recs)
    assert np.isnan(out["funding_rate"].iloc[2])
    assert out["funding_rate"].iloc[3] == 5e-5
