
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_MTF_CONFLUENCE = load_module("_mtf_confluence_test", __file__.replace("test_mtf_confluence.py", "mtf_confluence.py"))
mtf_confluence_core = _MTF_CONFLUENCE.mtf_confluence_core
_resample_htf = _MTF_CONFLUENCE._resample_htf


def build_uptrend_with_pullback(n_trend=900, dip=10.0):
    closes = list(np.linspace(100, 400, n_trend))
    base = closes[-1]
    closes += [base - 2, base - 5, base - dip]
    closes += [base - 4, base + 4, base + 10]
    return make_ohlcv(closes)


def build_downtrend_with_rally(n_trend=900, pop=10.0):
    closes = list(np.linspace(400, 100, n_trend))
    base = closes[-1]
    closes += [base + 2, base + 5, base + pop]
    closes += [base + 4, base - 4, base - 10]
    return make_ohlcv(closes)


def test_range_index_fallback_is_safe():
    df = make_ohlcv(np.linspace(100, 400, 903)).reset_index(drop=True)
    out = mtf_confluence_core(df)
    assert "signal" in out.columns
    assert out["htf_trend"].iloc[-1] == 1


