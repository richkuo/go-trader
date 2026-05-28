"""Market regime detection for go-trader check scripts.

Computes a 3-state regime label per (symbol, timeframe) from OHLCV data using
Wilder's ADX + directional indicator (+DI/-DI):

  trending_up   — ADX >= threshold AND +DI > -DI
  trending_down — ADX >= threshold AND -DI > +DI
  ranging       — ADX < threshold  (weak or absent trend)

Bars during the ADX warmup window (first 2*period - 1 bars) default to
"ranging" because there is insufficient history for a valid ADX value.

Usage in check scripts (after data fetch and before apply_strategy):

    from regime import latest_regime
    regime_payload = latest_regime(df, period=14, adx_threshold=20.0)
    strategy_params["regime"] = regime_payload
"""

from __future__ import annotations

import pandas as pd

try:
    from .atr import standard_atr
except ImportError:  # pragma: no cover - exercised by check-script style imports
    from atr import standard_atr

try:
    from shared_strategies.open.adx_trend import _compute_adx_components
except ImportError:  # pragma: no cover - supports direct shared_tools/regime.py imports
    import importlib.util
    from pathlib import Path

    _ADX_TREND_PATH = (
        Path(__file__).resolve().parents[1] / "shared_strategies" / "open" / "adx_trend.py"
    )
    _ADX_SPEC = importlib.util.spec_from_file_location("_regime_adx_trend", _ADX_TREND_PATH)
    if _ADX_SPEC is None or _ADX_SPEC.loader is None:
        raise
    _ADX_MODULE = importlib.util.module_from_spec(_ADX_SPEC)
    _ADX_SPEC.loader.exec_module(_ADX_MODULE)
    _compute_adx_components = _ADX_MODULE._compute_adx_components

_VALID_LABELS = frozenset({"trending_up", "trending_down", "ranging"})
_REGIME_COLUMNS = ("regime", "regime_score", "adx", "plus_di", "minus_di")
_DEFAULT_METRICS: dict = {"adx": 0.0, "plus_di": 0.0, "minus_di": 0.0, "atr_pct": 0.0}
_DEFAULT_RESULT: dict = {"regime": "ranging", "score": 0.0, "metrics": _DEFAULT_METRICS}


def compute_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Add regime columns to a copy of df.

    Parameters
    ----------
    df : DataFrame with high, low, close columns
    period : ADX lookback (Wilder's smoothing)
    adx_threshold : ADX value below which the market is considered ranging

    Returns
    -------
    New DataFrame (input not mutated) with extra columns:
        regime       — "trending_up" | "trending_down" | "ranging"
        regime_score — float in [0, 1]; ADX / 100, clamped
        adx          — raw ADX value
        plus_di      — +DI value
        minus_di     — -DI value
    """
    result = df.copy()
    n = len(result)

    result["regime"] = "ranging"
    result["regime_score"] = 0.0
    result["adx"] = 0.0
    result["plus_di"] = 0.0
    result["minus_di"] = 0.0

    if n == 0:
        return result

    if n <= period:
        return result

    components = _compute_adx_components(
        result["high"].values,
        result["low"].values,
        result["close"].values,
        period,
    )
    plus_di = components["plus_di"]
    minus_di = components["minus_di"]
    adx_arr = components["adx"]
    adx_start = components["adx_start"]

    result["adx"] = adx_arr
    result["plus_di"] = plus_di
    result["minus_di"] = minus_di

    for i in range(adx_start, n):
        adx_val = adx_arr[i]
        score = min(adx_val / 100.0, 1.0)
        result.iat[i, result.columns.get_loc("regime_score")] = score

        if adx_val < adx_threshold:
            label = "ranging"
        elif plus_di[i] > minus_di[i]:
            label = "trending_up"
        elif minus_di[i] > plus_di[i]:
            label = "trending_down"
        else:
            label = "ranging"
        result.iat[i, result.columns.get_loc("regime")] = label

    return result


def latest_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> dict:
    """Return the current regime from the most recent bar.

    Parameters
    ----------
    df : DataFrame with high, low, close columns (at least 2*period bars
         recommended for a reliable ADX reading)
    period : ADX lookback
    adx_threshold : minimum ADX to call a trend

    Returns
    -------
    dict:
        regime  — "trending_up" | "trending_down" | "ranging"
        score   — float in [0, 1]
        metrics — dict with adx, plus_di, minus_di, atr_pct (all floats)
    """
    if len(df) == 0:
        return {**_DEFAULT_RESULT, "metrics": dict(_DEFAULT_METRICS)}

    reg_df = compute_regime(df, period=period, adx_threshold=adx_threshold)
    last = reg_df.iloc[-1]

    atr_series = standard_atr(df, period=period)
    atr_val = atr_series.iloc[-1] if not atr_series.empty else float("nan")
    try:
        atr_val = float(atr_val)
    except (TypeError, ValueError):
        atr_val = 0.0
    if not (atr_val > 0):
        atr_val = 0.0

    close_val = float(df["close"].iloc[-1])
    atr_pct = (atr_val / close_val * 100.0) if close_val != 0 else 0.0

    return {
        "regime": str(last["regime"]),
        "score": float(last["regime_score"]),
        "metrics": {
            "adx": float(last["adx"]),
            "plus_di": float(last["plus_di"]),
            "minus_di": float(last["minus_di"]),
            "atr_pct": round(atr_pct, 4),
        },
    }


def compute_multi_regime(
    df: pd.DataFrame,
    windows: dict[str, int],
    adx_threshold: float = 20.0,
) -> dict[str, dict]:
    """Run ADX regime detection at each named window (ADX period in bars).

    Parameters
    ----------
    df : DataFrame with high, low, close columns
    windows : mapping of window name -> ADX period (bar count), e.g.
        {"short": 168, "medium": 720, "long": 2160}
    adx_threshold : minimum ADX to call a trend

    Returns
    -------
    dict mapping window name -> latest_regime snapshot:
        {"short": {"regime": "...", "score": ..., "metrics": {...}}, ...}

    Raises
    ------
    ValueError
        When windows is empty or contains invalid names/periods.
    """
    if not windows:
        raise ValueError("windows must be a non-empty dict")

    out: dict[str, dict] = {}
    for name in sorted(windows.keys()):
        trimmed = str(name).strip()
        if not trimmed:
            raise ValueError("window names must be non-empty strings")
        period = windows[name]
        if not isinstance(period, int) or isinstance(period, bool):
            raise ValueError(f"window {trimmed!r}: period must be an int, got {type(period).__name__}")
        if period < 2:
            raise ValueError(f"window {trimmed!r}: period must be >= 2, got {period}")
        out[trimmed] = latest_regime(df, period=period, adx_threshold=adx_threshold)
    return out


def regime_payload_for_config(
    df: pd.DataFrame,
    *,
    period: int = 14,
    adx_threshold: float = 20.0,
    windows: dict[str, int] | None = None,
) -> dict | str:
    """Build the regime payload for check-script stdout and strategy_params.

    Legacy mode (windows empty/None): returns a single-window snapshot dict
    (same shape as latest_regime).

    Multi-window mode: returns a dict mapping window name -> snapshot.
    """
    if windows:
        return compute_multi_regime(df, windows, adx_threshold=adx_threshold)
    return latest_regime(df, period=period, adx_threshold=adx_threshold)


def regime_label_from_payload(payload: dict | str, window_key: str = "") -> str:
    """Extract a regime label string from legacy or multi-window payload."""
    if isinstance(payload, str):
        return payload
    if not isinstance(payload, dict):
        return ""
    if "regime" in payload and isinstance(payload.get("regime"), str):
        # Single-window snapshot {"regime": "...", "score": ..., "metrics": {...}}
        if "score" in payload or "metrics" in payload:
            return str(payload["regime"])
    key = (window_key or "").strip()
    if key and key in payload:
        entry = payload[key]
        if isinstance(entry, dict):
            return str(entry.get("regime") or "")
    return ""


def required_ohlcv_limit(
    period: int = 14,
    windows: dict[str, int] | None = None,
    *,
    base_limit: int = 200,
    margin: int = 10,
) -> int:
    """Minimum OHLCV bar count for reliable ADX warmup across all windows."""
    max_period = period
    if windows:
        max_period = max(max(windows.values()), period)
    warmup = 2 * max_period - 1
    return max(base_limit, warmup + margin)


def prepare_check_regime(
    df: pd.DataFrame,
    *,
    regime_enabled: bool,
    period: int = 14,
    adx_threshold: float = 20.0,
    windows: dict[str, int] | None = None,
    atr_window: str = "",
) -> tuple[dict | str, str, dict]:
    """Build regime outputs for check scripts.

    Returns (stdout_regime, market_ctx_regime, strategy_params_regime).
    stdout_regime is a label string (legacy) or multi-window dict (#792).
    """
    disabled = {"regime": "", "score": 0.0, "metrics": dict(_DEFAULT_METRICS)}
    if not regime_enabled:
        return "", "", disabled

    if windows:
        multi = compute_multi_regime(df, windows, adx_threshold=adx_threshold)
        primary_key = "medium" if "medium" in windows else sorted(windows.keys())[0]
        strategy_payload = multi.get(primary_key, disabled)
        atr_key = (atr_window or primary_key).strip() or primary_key
        atr_entry = multi.get(atr_key, strategy_payload)
        live_atr = str(atr_entry.get("regime") or "")
        return multi, live_atr, strategy_payload

    legacy = latest_regime(df, period=period, adx_threshold=adx_threshold)
    label = str(legacy.get("regime") or "")
    return label, label, legacy


def parse_regime_windows_json(raw: str | None) -> dict[str, int] | None:
    """Parse --regime-windows-json from Go; returns None when unset/empty."""
    if raw is None:
        return None
    text = str(raw).strip()
    if not text:
        return None
    import json

    parsed = json.loads(text)
    if not isinstance(parsed, dict) or not parsed:
        raise ValueError("regime windows JSON must be a non-empty object")
    out: dict[str, int] = {}
    for name, value in parsed.items():
        key = str(name).strip()
        if not key:
            raise ValueError("regime window names must be non-empty")
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError(f"regime window {key!r}: bar count must be an int")
        if value < 2:
            raise ValueError(f"regime window {key!r}: bar count must be >= 2")
        out[key] = value
    return out


def ensure_regime_columns(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Inject regime columns into df in-place, no-op if already present.

    Returns the same DataFrame object (mutations are in-place).
    """
    if all(col in df.columns for col in _REGIME_COLUMNS):
        return df

    reg_df = compute_regime(df, period=period, adx_threshold=adx_threshold)
    for col in _REGIME_COLUMNS:
        df[col] = reg_df[col].values
    return df
