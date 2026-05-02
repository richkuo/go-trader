"""Tests for regime injection contract in check scripts.

Verifies the regime injection pattern used by all 6 standard check scripts:
  - latest_regime() is importable via shared_tools sys.path
  - regime payload is JSON-serializable (no NaN/Inf; SafeEncoder compat)
  - strip_unsupported_position_context drops "regime" for non-aware strategies
  - regime payload can safely be merged into strategy_params (even when None)
  - check_options.py computes a regime label from underlying OHLCV (#544)
"""

import json
import sys
import pathlib
import importlib.util

import numpy as np
import pandas as pd

# Mirror the sys.path setup that check scripts use for shared_tools
_SHARED_TOOLS = pathlib.Path(__file__).parent.parent / "shared_tools"
if str(_SHARED_TOOLS) not in sys.path:
    sys.path.insert(0, str(_SHARED_TOOLS))

from regime import latest_regime

_SHARED_STRATEGIES_TOOLS = str(_SHARED_TOOLS)


# ─── Fixtures ────────────────────────────────────────────────────────────────


def _make_uptrend_df(n: int = 100) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h", tz="UTC")
    return pd.DataFrame(
        {"open": close, "high": close + 0.5, "low": close - 0.5, "close": close, "volume": 1000.0},
        index=idx,
    )


def _make_flat_df(n: int = 100) -> pd.DataFrame:
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h", tz="UTC")
    return pd.DataFrame(
        {"open": close, "high": close + 0.05, "low": close - 0.05, "close": close, "volume": 1000.0},
        index=idx,
    )


# ─── Import path tests ────────────────────────────────────────────────────────


def test_latest_regime_importable_via_check_script_syspath():
    """latest_regime must be importable via the sys.path check scripts set up."""
    assert callable(latest_regime)


# ─── JSON-serializability tests ───────────────────────────────────────────────


def test_latest_regime_output_json_serializable_uptrend():
    """regime payload from an uptrend df must survive json.dumps (no NaN/Inf)."""
    df = _make_uptrend_df()
    payload = latest_regime(df)
    serialized = json.dumps(payload)
    parsed = json.loads(serialized)
    assert parsed["regime"] in ("trending_up", "trending_down", "ranging")
    assert isinstance(parsed["score"], float)
    assert isinstance(parsed["metrics"], dict)


def test_latest_regime_output_json_serializable_flat():
    """regime payload from a flat/ranging df must survive json.dumps."""
    df = _make_flat_df()
    payload = latest_regime(df)
    serialized = json.dumps(payload)
    parsed = json.loads(serialized)
    assert parsed["regime"] == "ranging"


def test_regime_label_string_is_safe_for_output_field():
    """The regime label (just the string) is safe to embed directly in check script output."""
    df = _make_uptrend_df()
    payload = latest_regime(df)
    label = payload["regime"]
    assert isinstance(label, str)
    assert label in ("trending_up", "trending_down", "ranging")
    # Must be embeddable as a JSON string value
    assert json.dumps({"regime": label})


# ─── strategy_params merge tests ─────────────────────────────────────────────


def test_regime_merge_into_none_params():
    """When strategy_params is None, merging regime must not crash."""
    df = _make_uptrend_df()
    payload = latest_regime(df)
    strategy_params = None
    strategy_params = (strategy_params or {})
    strategy_params["regime"] = payload
    assert "regime" in strategy_params
    assert strategy_params["regime"]["regime"] in ("trending_up", "trending_down", "ranging")


def test_regime_merge_preserves_existing_params():
    """Merging regime into existing params must not drop other keys."""
    df = _make_uptrend_df()
    payload = latest_regime(df)
    strategy_params = {"rsi_period": 14, "threshold": 0.6}
    strategy_params["regime"] = payload
    assert strategy_params["rsi_period"] == 14
    assert strategy_params["threshold"] == 0.6
    assert "regime" in strategy_params


# ─── strip_unsupported_position_context tests ─────────────────────────────────


def test_strip_unsupported_drops_regime_for_non_aware_function():
    """strip_unsupported_position_context must drop 'regime' for a strategy that doesn't declare it."""
    from strategy_composition import strip_unsupported_position_context

    def dummy_strategy(df, rsi_period=14):
        return df

    df = _make_uptrend_df()
    params = {"rsi_period": 14, "regime": latest_regime(df)}
    stripped = strip_unsupported_position_context(dummy_strategy, params)
    assert "regime" not in stripped
    assert stripped["rsi_period"] == 14


def test_strip_unsupported_keeps_regime_for_aware_function():
    """strip_unsupported_position_context must keep 'regime' when the strategy declares it."""
    from strategy_composition import strip_unsupported_position_context

    def regime_aware_strategy(df, regime=None, rsi_period=14):
        return df

    df = _make_uptrend_df()
    params = {"rsi_period": 14, "regime": latest_regime(df)}
    stripped = strip_unsupported_position_context(regime_aware_strategy, params)
    assert "regime" in stripped
    assert stripped["rsi_period"] == 14


# ─── check_options.py regime computation (#544) ───────────────────────────────


def _load_check_options_module():
    """Import shared_scripts/check_options.py as a module without executing main()."""
    src_path = pathlib.Path(__file__).parent / "check_options.py"
    spec = importlib.util.spec_from_file_location("_check_options_under_test", src_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_check_options_regime_label_from_uptrend_df():
    """_regime_label_from_df returns a valid label for a sufficient uptrend df."""
    module = _load_check_options_module()
    df = _make_uptrend_df(100)
    label = module._regime_label_from_df(df)
    assert label in ("trending_up", "trending_down", "ranging")


def test_check_options_regime_label_from_short_df_is_none():
    """_regime_label_from_df returns None when the df has fewer than min bars (warmup)."""
    module = _load_check_options_module()
    df = _make_uptrend_df(10)
    assert module._regime_label_from_df(df) is None


def test_check_options_regime_label_from_none_df_is_none():
    """_regime_label_from_df tolerates a None df (fetch failure path)."""
    module = _load_check_options_module()
    assert module._regime_label_from_df(None) is None


def test_check_options_fetch_ohlcv_df_uses_adapter_when_available():
    """_fetch_ohlcv_df prefers adapter.get_ohlcv when present and returns a DataFrame."""
    module = _load_check_options_module()

    class StubAdapter:
        def __init__(self, rows):
            self._rows = rows
            self.calls = []

        def get_ohlcv(self, symbol, timeframe, limit):
            self.calls.append((symbol, timeframe, limit))
            return self._rows

    rows = [
        [i * 1000, 100.0 + i, 101.0 + i, 99.0 + i, 100.5 + i, 1000.0]
        for i in range(50)
    ]
    adapter = StubAdapter(rows)
    df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=adapter)
    assert df is not None
    assert len(df) == 50
    assert {"high", "low", "close"}.issubset(df.columns)
    assert adapter.calls == [("BTC", "4h", 100)]


def test_check_options_fetch_ohlcv_df_short_returns_none():
    """_fetch_ohlcv_df returns None when adapter rows are below min_len (no fallback to ccxt)."""
    module = _load_check_options_module()

    class StubAdapter:
        def get_ohlcv(self, symbol, timeframe, limit):
            return [[i * 1000, 100.0, 101.0, 99.0, 100.5, 1000.0] for i in range(5)]

    # Adapter returned 5 bars; min_len is 30 → expect None (without ccxt fallback,
    # since adapter explicitly returned data — empty/None would fall back).
    # Current behavior: short non-empty rows do NOT trigger fallback; they short-circuit to None.
    df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=StubAdapter())
    assert df is None
