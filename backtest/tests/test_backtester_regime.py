import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from regime import compute_regime, latest_regime


def _uptrend_df(n: int = 100) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.5, "low": close - 0.5,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def _ranging_df(n: int = 100) -> pd.DataFrame:
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.05, "low": close - 0.05,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def test_parity_latest_regime_matches_compute_regime_last_bar(subtests):
    with subtests.test(case="parity_latest_regime_matches_compute_regime_last_bar"):
        _assert_parity_latest_regime_matches_compute_regime_last_bar()
    with subtests.test(case="parity_ranging_df"):
        _assert_parity_ranging_df()


def _assert_parity_latest_regime_matches_compute_regime_last_bar():
    df = _uptrend_df(n=100)
    live_result = latest_regime(df, period=14, adx_threshold=20.0)
    backtest_series = compute_regime(df, period=14, adx_threshold=20.0)
    backtest_last = backtest_series.iloc[-1]

    assert live_result["regime"] == backtest_last["regime"], (
        f"Parity violation: live={live_result['regime']}, "
        f"backtest last bar={backtest_last['regime']}"
    )
    assert abs(live_result["score"] - float(backtest_last["regime_score"])) < 1e-9, (
        "Score mismatch between live and backtest"
    )


def _assert_parity_ranging_df():
    df = _ranging_df(n=100)
    live = latest_regime(df, period=14, adx_threshold=20.0)
    bt_last = compute_regime(df, period=14, adx_threshold=20.0).iloc[-1]
    assert live["regime"] == bt_last["regime"]
