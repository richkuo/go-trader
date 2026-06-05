"""#879: parity between the live regime subprocess (fetch_regime.compute_payload)
and the backtest regime path (ensure_regime_columns / compute_regime_composite)
for the same candles. Backtest code is unchanged by #879; this guards that the
relocated live computation stays label-identical to the backtest.
"""
import importlib.util
import pathlib
import sys

import numpy as np
import pandas as pd

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "shared_tools"))
sys.path.insert(0, str(ROOT))

from regime import ensure_regime_columns  # noqa: E402


def _load_fetch_regime():
    spec = importlib.util.spec_from_file_location(
        "fetch_regime", ROOT / "shared_scripts" / "fetch_regime.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _df(n=160, slope=0.7):
    close = np.linspace(100.0, 100.0 + slope * n, n)
    return pd.DataFrame({
        "timestamp": range(n), "open": close - 0.2, "high": close + 0.6,
        "low": close - 0.6, "close": close, "volume": np.ones(n),
    })


def _last_label(df, **kwargs):
    out = ensure_regime_columns(df.copy(), **kwargs)
    return str(out["regime"].iloc[-1])


def test_adx_bundle_matches_backtest_period14():
    fr = _load_fetch_regime()
    df = _df()
    spec = {"w": {"classifier": "adx", "period": 14, "adx_threshold": 20}}
    bundle = fr.compute_payload(df, spec)["w"]["regime"]
    assert bundle == _last_label(df, period=14, adx_threshold=20)


def test_adx_bundle_matches_backtest_full_period_over_14():
    """period>14 must use the full ADX period in both paths (no COMPOSITE cap)."""
    fr = _load_fetch_regime()
    df = _df(200)
    spec = {"w": {"classifier": "adx", "period": 30, "adx_threshold": 20}}
    bundle = fr.compute_payload(df, spec)["w"]["regime"]
    assert bundle == _last_label(df, period=30, adx_threshold=20)


def test_composite_bundle_matches_backtest():
    fr = _load_fetch_regime()
    df = _df(180)
    spec = {"w": {"classifier": "composite", "period": 30}}
    bundle = fr.compute_payload(df, spec)["w"]["regime"]
    backtest_label = _last_label(
        df, windows_spec={"w": {"classifier": "composite", "period": 30}}, gate_window="w")
    assert bundle == backtest_label
