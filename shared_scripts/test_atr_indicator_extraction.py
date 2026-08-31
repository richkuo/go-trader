import sys
import os

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

from shared_tools.conftest import load_module, make_ohlcv

_ATR = load_module("_atr_indicator_extraction_test", os.path.join(os.path.dirname(__file__), "..", "shared_tools", "atr.py"))
ensure_atr_indicator = _ATR.ensure_atr_indicator


def _make_df(n=20):
    return make_ohlcv([1.0] * n, volume=1.0, noise=0.1)


def test_stale_last_missing_atr():
    df = _make_df()
    stale_last = df.iloc[-1]
    ensure_atr_indicator(df)
    assert "atr" not in stale_last.index


def test_fresh_last_has_atr():
    df = _make_df()
    ensure_atr_indicator(df)
    fresh_last = df.iloc[-1]
    assert "atr" in fresh_last.index
    import math
    assert math.isfinite(float(fresh_last["atr"]))


def test_noop_when_atr_already_present():
    df = _make_df()
    df["atr"] = 0.05
    original_atr = df.iloc[-1]["atr"]
    ensure_atr_indicator(df)
    fresh_last = df.iloc[-1]
    assert fresh_last["atr"] == original_atr
