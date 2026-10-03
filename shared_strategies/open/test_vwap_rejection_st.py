
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_VWAP_REJECTION = load_module("_vwap_rejection_st_test", __file__.replace("test_vwap_rejection_st.py", "vwap_rejection_st.py"))
vwap_rejection_st_core = _VWAP_REJECTION.vwap_rejection_st_core


def _hourly_index(n: int, start: str = "2026-01-01 00:00:00") -> pd.DatetimeIndex:
    return pd.date_range(start, periods=n, freq="1h")


def _bear_setup_with_rally_and_rejection():
    rng = np.random.default_rng(42)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    rally = np.linspace(110.0, 130.0, 5)
    reject_closes = [120.0, 113.0, 109.0, 106.0]
    closes = np.concatenate([down, rally, reject_closes])
    n = len(closes)
    idx = _hourly_index(n)
    df = make_ohlcv(closes, index=idx, noise=0.4)
    for i, close_px in enumerate(reject_closes, start=len(down) + len(rally)):
        prev_close = closes[i - 1]
        df.iat[i, df.columns.get_loc("open")] = prev_close + 0.5
        df.iat[i, df.columns.get_loc("high")] = prev_close + 1.0
        df.iat[i, df.columns.get_loc("low")] = close_px - 1.0
        df.iat[i, df.columns.get_loc("close")] = close_px
    return df


def test_emits_short_on_vwap_rejection_in_bear_trend():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert (result["signal"] == -1).any(), (
        "Expected at least one short signal on rejection bars after a VWAP/EMA rally"
    )
    last_signals = result["signal"].iloc[-5:]
    assert (last_signals == -1).any(), (
        f"Short signal should land in the rejection window, got {last_signals.tolist()}"
    )


