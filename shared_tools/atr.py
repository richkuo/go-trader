
from __future__ import annotations

import importlib.util
import os

import pandas as pd

_INDICATORS_CORE_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "shared_strategies", "open", "indicators_core.py",
)


def _load_indicators_core():
    spec = importlib.util.spec_from_file_location(
        "_go_trader_indicators_core", _INDICATORS_CORE_PATH
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_core = _load_indicators_core()


def normalize_atr_method(method: str | None) -> str:
    return _core.normalize_atr_method(method)


def standard_atr(df: pd.DataFrame, period: int = 14, method: str = "simple") -> pd.Series:
    return _core.atr_sma(df, period, method=method)


def ensure_atr_indicator(df: pd.DataFrame, period: int = 14, method: str = "simple") -> pd.DataFrame:
    if "atr" not in df.columns:
        df["atr"] = standard_atr(df, period, method=method)
    return df


def latest_atr(df: pd.DataFrame, period: int = 14, method: str = "simple") -> float:
    series = standard_atr(df, period, method=method)
    if series.empty:
        return 0.0
    value = series.iloc[-1]
    try:
        value = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not (value > 0):
        return 0.0
    return value
