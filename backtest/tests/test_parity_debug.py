import os
import sys

import pandas as pd

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

import parity_debug


class _FakeRegistry:
    STRATEGY_REGISTRY = {
        "toy": {
            "description": "Toy strategy",
            "default_params": {},
        },
    }

    @staticmethod
    def list_strategies():
        return ["toy"]

    @staticmethod
    def apply_strategy(_name, df, _params):
        out = df.copy()
        out["signal"] = [0, 1, 0, -1, 0]
        return out


def _candles():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    return pd.DataFrame(
        {
            "open": [100.0, 100.0, 101.0, 102.0, 103.0],
            "high": [101.0, 101.0, 102.0, 103.0, 104.0],
            "low": [99.0, 99.0, 100.0, 101.0, 102.0],
            "close": [100.0, 101.0, 102.0, 103.0, 104.0],
            "volume": [1.0] * 5,
        },
        index=idx,
    )


def test_build_backtest_trace_emits_shifted_decision_rows(monkeypatch):
    monkeypatch.setattr(parity_debug, "load_registry", lambda _name: _FakeRegistry)
    monkeypatch.setattr(parity_debug, "load_cached_data", lambda *a, **kw: _candles())

    trace = parity_debug.build_backtest_trace(
        strategy="toy",
        symbol="BTC/USDT",
        timeframe="1d",
        since="2024-01-01",
        capital=1000.0,
        registry="spot",
        platform="binanceus",
    )

    assert list(trace.columns) == parity_debug.TRACE_COLUMNS
    assert len(trace) == 5
    # Signal emitted on bar 1 fills at bar 2's open.
    row = trace.iloc[2]
    assert row["signal"] == 1
    assert row["open_action"] == "long"
    assert "signal_open_long" in row["event"]
    assert row["fill_px"] == 101.0505
    # Signal -1 emitted on bar 3 is shifted to bar 4 and closes there.
    assert "signal_close_long" in trace.iloc[4]["event"]


def test_compare_traces_returns_empty_for_matching_rows():
    left = pd.DataFrame(
        [{"date": "2024-01-01", "signal": 1, "regime": "ranging", "fee": 0.1}]
    )
    right = pd.DataFrame(
        [{"date": "2024-01-01", "signal": 1, "regime": "ranging", "fee": 0.100000001}]
    )

    diff = parity_debug.compare_traces(left, right, tolerance=1e-6)

    assert diff.empty


def test_compare_traces_reports_numeric_and_string_mismatches():
    left = pd.DataFrame(
        [{"date": "2024-01-01", "signal": 1, "regime": "ranging", "fee": 0.1}]
    )
    right = pd.DataFrame(
        [{"date": "2024-01-01", "signal": 0, "regime": "trending_up", "fee": 0.2}]
    )

    diff = parity_debug.compare_traces(left, right, tolerance=1e-8)

    assert set(diff["column"]) == {"signal", "regime", "fee"}


def test_compare_traces_reports_missing_rows():
    left = pd.DataFrame([{"date": "2024-01-01", "signal": 1}])
    right = pd.DataFrame([{"date": "2024-01-02", "signal": 1}])

    diff = parity_debug.compare_traces(left, right)

    assert list(diff["column"]) == ["_row", "_row"]
