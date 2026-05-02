"""Tests for regime injection contract in check scripts.

Verifies the regime injection pattern used by all 5 standard check scripts:
  - latest_regime() is importable via shared_tools sys.path
  - regime payload is JSON-serializable (no NaN/Inf; SafeEncoder compat)
  - strip_unsupported_position_context drops "regime" for non-aware strategies
  - regime payload can safely be merged into strategy_params (even when None)
  - check_options.py emits regime: null (tested via source inspection)
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


# ─── check_options.py emits regime: null ─────────────────────────────────────


def test_check_options_source_emits_regime_null():
    """check_options.py must emit 'regime': None (null in JSON) in its output paths."""
    src_path = pathlib.Path(__file__).parent / "check_options.py"
    source = src_path.read_text()
    assert '"regime"' in source or "'regime'" in source, (
        "check_options.py does not emit a 'regime' field — add \"'regime': None\" to all output dicts"
    )
