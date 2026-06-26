"""Market regime detection for go-trader check scripts.

Supports per-window classifiers (#795):
  adx       — 3-state Wilder ADX + DI (default)
  composite — 7-state return/ADX/range tuple

Usage in check scripts (after data fetch and before apply_strategy):

    from regime import prepare_check_regime, parse_regime_windows_spec_json
"""

from __future__ import annotations

import json
from typing import Any

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

CLASSIFIER_ADX = "adx"
CLASSIFIER_COMPOSITE = "composite"

# Preferred multi-window name for strategy_params primary snapshot (#792).
# When absent, the lexicographically first window name is used.
REGIME_PRIMARY_WINDOW_KEY = "medium"

# ADX persistence in composite uses a capped lookback; return/range use the full window period.
COMPOSITE_ADX_PERIOD_CAP = 14

VALID_LABELS_ADX = frozenset({"trending_up", "trending_down", "ranging"})
VALID_LABELS_COMPOSITE = frozenset({
    "trending_up_clean",
    "trending_up_choppy",
    "trending_down_clean",
    "trending_down_choppy",
    "ranging_quiet",
    "ranging_volatile",
    "ranging_directional",
    "ranging_directional_up",
    "ranging_directional_down",
})
# Back-compat alias for ADX-only callers
_VALID_LABELS = VALID_LABELS_ADX

# #1124: the bare `ranging_directional` label is the parent of the directional
# family — the producer emits the bare label only at exactly return_eff == 0
# (rare on real price data) and the `_up`/`_down` sub-labels for any non-zero
# drift. The family rule is one-directional for entry gates and exhaustiveness:
# a bare `ranging_directional` covers its `_up`/`_down` sub-labels, never the
# reverse. Mirrors the Go `regimeLabelFamilyCovered` / `regimeDirectionalSubs`.
RANGING_DIRECTIONAL_BARE = "ranging_directional"
RANGING_DIRECTIONAL_SUBS = frozenset({"ranging_directional_up", "ranging_directional_down"})


def regime_label_allows_entry(allowed, current: str) -> bool:
    """#1124 entry-gate family match.

    True when ``current`` is explicitly in ``allowed`` OR when ``current`` is a
    directional sub-label (``_up``/``_down``) and the bare ``ranging_directional``
    parent is in ``allowed``. Expansion is one-directional (bare→subs), so an
    operator listing an explicit ``_up`` still gates out ``_down``. Empty
    ``allowed`` (no gate) or empty ``current`` (no regime available) allow entry,
    matching the Go ``regimeAllowsEntry`` contract for parity.
    """
    if not allowed or not current:
        return True
    if current in allowed:
        return True
    if current in RANGING_DIRECTIONAL_SUBS and RANGING_DIRECTIONAL_BARE in allowed:
        return True
    return False

_DEFAULT_COMPOSITE_THRESHOLDS = {
    "return_pct": 0.05,
    "range_pct": 0.03,
    "adx": 25.0,
    "efficiency": 0.5,
}

_REGIME_COLUMNS = ("regime", "regime_score", "adx", "plus_di", "minus_di")
_DEFAULT_METRICS: dict = {"adx": 0.0, "plus_di": 0.0, "minus_di": 0.0, "atr_pct": 0.0}
_DEFAULT_RESULT: dict = {"regime": "ranging", "score": 0.0, "metrics": dict(_DEFAULT_METRICS)}


def valid_labels_for_classifier(classifier: str) -> frozenset[str]:
    if str(classifier or "").strip().lower() == CLASSIFIER_COMPOSITE:
        return VALID_LABELS_COMPOSITE
    return VALID_LABELS_ADX


def _normalize_spec(spec: dict[str, Any], *, default_adx_threshold: float = 20.0) -> dict[str, Any]:
    out = dict(spec)
    out["classifier"] = str(out.get("classifier") or CLASSIFIER_ADX).strip().lower()
    if out["classifier"] not in (CLASSIFIER_ADX, CLASSIFIER_COMPOSITE):
        raise ValueError(f"unsupported classifier {out['classifier']!r}")
    period = out.get("period")
    if not isinstance(period, int) or isinstance(period, bool) or period < 2:
        raise ValueError(f"period must be an int >= 2, got {period!r}")
    out["period"] = period
    if out["classifier"] == CLASSIFIER_ADX:
        th = float(out.get("adx_threshold") or default_adx_threshold)
        out["adx_threshold"] = th
    else:
        raw_th = out.get("thresholds") or {}
        th = {**_DEFAULT_COMPOSITE_THRESHOLDS, **raw_th}
        out["thresholds"] = th
    return out


def _window_slice(df: pd.DataFrame, period: int) -> pd.DataFrame:
    if len(df) <= period:
        return df
    return df.iloc[-period:]


def _atr_at_end(df: pd.DataFrame, period: int) -> float:
    atr_series = standard_atr(df, period=period)
    atr_val = atr_series.iloc[-1] if not atr_series.empty else float("nan")
    try:
        atr_val = float(atr_val)
    except (TypeError, ValueError):
        atr_val = 0.0
    return atr_val if atr_val > 0 else 0.0


def _composite_efficiency_metrics(window: pd.DataFrame, atr_val: float, period: int) -> dict:
    """ATR-efficiency metrics for one window (shared by live + backtest paths).

    `atr_val` is the per-bar ATR; the window spans `period` bars, so the
    straight-line ATR travel denominator is atr_val * period.
    """
    denom = atr_val * period
    close_end = float(window["close"].iloc[-1])
    close_start = float(window["close"].iloc[0])
    net = close_end - close_start
    hi = float(window["high"].max())
    lo = float(window["low"].min())
    # Kaufman efficiency ratio: net travel / summed bar-to-bar travel ∈ [0, 1].
    path = float(window["close"].diff().abs().sum())
    return {
        "return_eff": net / denom if denom > 0 else 0.0,
        "range_eff": (hi - lo) / denom if denom > 0 else 0.0,
        "efficiency": abs(net) / path if path > 0 else 0.0,
        "close_end": close_end,
    }


def map_composite_label(
    return_eff: float,
    adx_val: float,
    range_eff: float,
    efficiency: float,
    thresholds: dict[str, float],
) -> str:
    """Map the composite metric tuple to one of nine labels (#795, #1124).

    Inputs are ATR-efficiency normalized so the thresholds are unit-consistent:
      return_eff — window net move / (per-bar ATR * period), signed, ~[-1, 1]
      range_eff  — window high-low / (per-bar ATR * period), ~[0, 1]
      efficiency — Kaufman efficiency ratio |net move| / summed bar-to-bar
                   travel, ∈ [0, 1]; high = clean directional move, low = chop.
    `adx_val` corroborates the efficiency-based clean/choppy split.

    #1124: the ranging-directional drift sign is baked into the label —
    `ranging_directional_up` when return_eff > 0, `ranging_directional_down`
    when return_eff < 0, and the bare `ranging_directional` when return_eff
    is exactly zero. The bare label fires only at exact zero, which is rare
    on real price data, so configs keyed on bare `ranging_directional` rely on
    the one-directional family rule (bare covers its `_up`/`_down` sub-labels
    for exhaustiveness, runtime resolution, and entry gating) rather than on
    the bare label being frequently emitted. Existing configs keyed on bare
    `ranging_directional` keep working via that rule.
    """
    ret_th = float(thresholds.get("return_pct", _DEFAULT_COMPOSITE_THRESHOLDS["return_pct"]))
    range_th = float(thresholds.get("range_pct", _DEFAULT_COMPOSITE_THRESHOLDS["range_pct"]))
    adx_th = float(thresholds.get("adx", _DEFAULT_COMPOSITE_THRESHOLDS["adx"]))
    eff_th = float(thresholds.get("efficiency", _DEFAULT_COMPOSITE_THRESHOLDS["efficiency"]))

    big_move = abs(return_eff) >= ret_th
    up = return_eff > 0
    high_adx = adx_val >= adx_th
    wide = range_eff >= range_th
    clean = efficiency >= eff_th and high_adx

    if big_move:
        if up:
            return "trending_up_clean" if clean else "trending_up_choppy"
        return "trending_down_clean" if clean else "trending_down_choppy"
    # No decisive net move → ranging family.
    if high_adx:
        # Directional pressure without net follow-through. Sign of the
        # drift is baked into the label (#1124); exact-zero stays bare so
        # the neutral/back-compat fallback is owned by the producer.
        if return_eff > 0:
            return "ranging_directional_up"
        if return_eff < 0:
            return "ranging_directional_down"
        return "ranging_directional"
    if wide:
        return "ranging_volatile"
    return "ranging_quiet"


def latest_regime_composite(
    df: pd.DataFrame,
    period: int,
    thresholds: dict[str, float] | None = None,
) -> dict:
    th = {**_DEFAULT_COMPOSITE_THRESHOLDS, **(thresholds or {})}
    if len(df) == 0:
        return {**_DEFAULT_RESULT, "metrics": dict(_DEFAULT_METRICS), "classifier": CLASSIFIER_COMPOSITE}

    window = _window_slice(df, period)
    # Numerators span the full window; ATR-efficiency divides by the window's
    # straight-line ATR travel (per-bar ATR * period) so thresholds are unit-consistent.
    atr_val = _atr_at_end(df, period)
    if atr_val <= 0:
        return {
            "regime": "ranging_quiet",
            "score": 0.0,
            "classifier": CLASSIFIER_COMPOSITE,
            "metrics": dict(_DEFAULT_METRICS),
        }

    eff = _composite_efficiency_metrics(window, atr_val, period)

    adx_period = min(period, COMPOSITE_ADX_PERIOD_CAP)
    reg_df = compute_regime(df, period=adx_period, adx_threshold=th["adx"])
    adx_val = float(reg_df["adx"].iloc[-1]) if len(reg_df) else 0.0

    label = map_composite_label(eff["return_eff"], adx_val, eff["range_eff"], eff["efficiency"], th)
    score = min(adx_val / 100.0, 1.0)
    close_end = eff["close_end"]
    return {
        "regime": label,
        "score": score,
        "classifier": CLASSIFIER_COMPOSITE,
        "metrics": {
            "adx": adx_val,
            "return_eff": round(eff["return_eff"], 4),
            "range_eff": round(eff["range_eff"], 4),
            "efficiency": round(eff["efficiency"], 4),
            "atr_pct": round(atr_val / close_end * 100.0, 4) if close_end else 0.0,
        },
    }


def classify_window(
    df: pd.DataFrame,
    spec: dict[str, Any],
    *,
    default_adx_threshold: float = 20.0,
) -> dict:
    """Run the configured classifier for one window spec."""
    norm = _normalize_spec(spec, default_adx_threshold=default_adx_threshold)
    if norm["classifier"] == CLASSIFIER_COMPOSITE:
        snap = latest_regime_composite(df, norm["period"], norm.get("thresholds"))
    else:
        snap = latest_regime(df, norm["period"], norm["adx_threshold"])
        snap["classifier"] = CLASSIFIER_ADX
    return snap


def compute_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Add ADX regime columns to a copy of df."""
    result = df.copy()
    n = len(result)

    result["regime"] = "ranging"
    result["regime_score"] = 0.0
    result["adx"] = 0.0
    result["plus_di"] = 0.0
    result["minus_di"] = 0.0

    if n == 0 or n <= period:
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


def compute_regime_composite(
    df: pd.DataFrame,
    period: int,
    thresholds: dict[str, float] | None = None,
) -> pd.DataFrame:
    """Per-bar composite labels for backtests (#795).

    Uses a rolling Python loop (not vectorized). ADX persistence uses
    COMPOSITE_ADX_PERIOD_CAP; return/range/ATR normalization use the full window period.
    """
    th = {**_DEFAULT_COMPOSITE_THRESHOLDS, **(thresholds or {})}
    result = df.copy()
    n = len(result)
    result["regime"] = "ranging_quiet"
    result["regime_score"] = 0.0
    result["adx"] = 0.0
    result["plus_di"] = 0.0
    result["minus_di"] = 0.0
    if n == 0:
        return result

    adx_period = min(period, COMPOSITE_ADX_PERIOD_CAP)
    adx_df = compute_regime(result, period=adx_period, adx_threshold=th["adx"])
    result["adx"] = adx_df["adx"].values
    result["plus_di"] = adx_df["plus_di"].values
    result["minus_di"] = adx_df["minus_di"].values

    atr_series = standard_atr(result, period=period)
    for i in range(period, n):
        window = result.iloc[i - period + 1 : i + 1]
        atr_val = float(atr_series.iloc[i]) if i < len(atr_series) else 0.0
        if not (atr_val > 0):
            continue
        eff = _composite_efficiency_metrics(window, atr_val, period)
        adx_val = float(result["adx"].iloc[i])
        label = map_composite_label(eff["return_eff"], adx_val, eff["range_eff"], eff["efficiency"], th)
        result.iat[i, result.columns.get_loc("regime")] = label
        result.iat[i, result.columns.get_loc("regime_score")] = min(adx_val / 100.0, 1.0)
    return result


def composite_feature_matrix(
    df: pd.DataFrame,
    period: int,
    thresholds: dict[str, float] | None = None,
) -> pd.DataFrame:
    """Per-bar composite feature tuple (return_eff, range_eff, efficiency, adx).

    Additive, offline-only (#1065 PR1). Mirrors compute_regime_composite's loop
    but emits the features map_composite_label consumes instead of the label, so
    an offline model fits on byte-consistent inputs. Warmup (i < period) and
    atr<=0 bars are NaN.
    """
    th = {**_DEFAULT_COMPOSITE_THRESHOLDS, **(thresholds or {})}
    cols = ["return_eff", "range_eff", "efficiency", "adx"]
    out = pd.DataFrame(float("nan"), index=df.index, columns=cols)
    n = len(df)
    if n == 0:
        return out
    adx_period = min(period, COMPOSITE_ADX_PERIOD_CAP)
    adx_df = compute_regime(df, period=adx_period, adx_threshold=th["adx"])
    atr_series = standard_atr(df, period=period)
    for i in range(period, n):
        window = df.iloc[i - period + 1 : i + 1]
        atr_val = float(atr_series.iloc[i]) if i < len(atr_series) else 0.0
        if not (atr_val > 0):
            continue
        eff = _composite_efficiency_metrics(window, atr_val, period)
        out.iat[i, 0] = eff["return_eff"]
        out.iat[i, 1] = eff["range_eff"]
        out.iat[i, 2] = eff["efficiency"]
        out.iat[i, 3] = float(adx_df["adx"].iloc[i])
    return out


def latest_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> dict:
    if len(df) == 0:
        return {**_DEFAULT_RESULT, "metrics": dict(_DEFAULT_METRICS)}

    reg_df = compute_regime(df, period=period, adx_threshold=adx_threshold)
    last = reg_df.iloc[-1]

    atr_val = _atr_at_end(df, period)
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
    windows: dict[str, Any],
    adx_threshold: float = 20.0,
    *,
    default_adx_threshold: float | None = None,
) -> dict[str, dict]:
    if not windows:
        raise ValueError("windows must be a non-empty dict")
    if default_adx_threshold is None:
        default_adx_threshold = adx_threshold

    out: dict[str, dict] = {}
    for name in sorted(windows.keys()):
        trimmed = str(name).strip()
        if not trimmed:
            raise ValueError("window names must be non-empty strings")
        if trimmed.lower() == "regime":
            raise ValueError(
                "window name 'regime' is reserved (conflicts with legacy regime snapshot JSON)"
            )
        value = windows[name]
        if isinstance(value, int) and not isinstance(value, bool):
            if value < 2:
                raise ValueError(f"window {trimmed!r}: period must be >= 2, got {value}")
            spec = {"classifier": CLASSIFIER_ADX, "period": value, "adx_threshold": default_adx_threshold}
        elif isinstance(value, dict):
            spec = value
        else:
            raise ValueError(f"window {trimmed!r}: must be int or object spec, got {type(value).__name__}")
        out[trimmed] = classify_window(df, spec, default_adx_threshold=default_adx_threshold)
    return out


def regime_payload_for_config(
    df: pd.DataFrame,
    *,
    period: int = 14,
    adx_threshold: float = 20.0,
    windows: dict[str, dict[str, Any]] | None = None,
) -> dict | str:
    if windows:
        return compute_multi_regime(df, windows, default_adx_threshold=adx_threshold)
    return classify_window(
        df,
        {"classifier": CLASSIFIER_ADX, "period": period, "adx_threshold": adx_threshold},
        default_adx_threshold=adx_threshold,
    )


def regime_label_from_payload(payload: dict | str, window_key: str = "") -> str:
    if isinstance(payload, str):
        return payload
    if not isinstance(payload, dict):
        return ""
    if "regime" in payload and isinstance(payload.get("regime"), str):
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
    windows: dict[str, dict[str, Any]] | None = None,
    *,
    base_limit: int = 200,
    margin: int = 10,
) -> int:
    max_period = period
    if windows:
        for spec in windows.values():
            if isinstance(spec, int) and not isinstance(spec, bool):
                p = spec
            else:
                p = int(spec.get("period") or period)
            if p > max_period:
                max_period = p
    warmup = 2 * max(max_period, 14) - 1
    return max(base_limit, warmup + margin)


def regime_from_injected_payload(
    raw: str | None,
    *,
    atr_window: str = "",
) -> tuple[dict | str, str, dict]:
    """Resolve the (stdout_regime, live_regime, strategy_regime) triple from a
    Go-injected precomputed payload (#879) instead of computing inline.

    `raw` is the --regime-payload-json value: the multi-window snapshot map the
    Go scheduler's global regime store holds for this strategy's signature
    (same shape compute_multi_regime returns). Empty/blank/invalid payloads —
    including the deliberate empty injection after a regime-subprocess failure
    — resolve to the disabled triple so every consumer falls back to its
    existing empty-case behavior (entry gate fails open, status shows
    regime=-). There is NO inline recompute fallback by design.
    """
    disabled = {"regime": "", "score": 0.0, "metrics": dict(_DEFAULT_METRICS)}
    text = str(raw or "").strip()
    if not text:
        return "", "", disabled
    try:
        payload = json.loads(text)
    except (ValueError, TypeError):
        return "", "", disabled
    if isinstance(payload, str):
        label = payload.strip()
        snap = dict(disabled, regime=label) if label else disabled
        return label, label, snap
    if not isinstance(payload, dict) or not payload:
        return "", "", disabled

    windows: dict[str, dict] = {}
    for name, snap in payload.items():
        key = str(name).strip()
        if key and isinstance(snap, dict):
            windows[key] = snap
    if not windows:
        return "", "", disabled

    # Primary/atr selection mirrors prepare_check_regime's multi branch: the
    # payload keys are exactly the windows-spec keys the Go side resolved.
    primary_key = (
        REGIME_PRIMARY_WINDOW_KEY
        if REGIME_PRIMARY_WINDOW_KEY in windows
        else sorted(windows.keys())[0]
    )
    strategy_payload = windows.get(primary_key, disabled)
    atr_key = (atr_window or primary_key).strip() or primary_key
    atr_entry = windows.get(atr_key, strategy_payload)
    live_atr = str(atr_entry.get("regime") or "")
    return windows, live_atr, strategy_payload


def prepare_check_regime(
    df: pd.DataFrame,
    *,
    regime_enabled: bool,
    period: int = 14,
    adx_threshold: float = 20.0,
    windows: dict[str, dict[str, Any]] | None = None,
    windows_spec: dict[str, dict[str, Any]] | None = None,
    atr_window: str = "",
    injected_payload_json: str | None = None,
) -> tuple[dict | str, str, dict]:
    disabled = {"regime": "", "score": 0.0, "metrics": dict(_DEFAULT_METRICS)}
    if not regime_enabled:
        return "", "", disabled
    # #879: when the Go scheduler injects the precomputed global-store payload,
    # never recompute inline — even when the injected payload is empty (that is
    # the explicit fail-open signal after a regime-subprocess failure).
    if injected_payload_json is not None:
        return regime_from_injected_payload(injected_payload_json, atr_window=atr_window)

    spec_map = windows_spec if windows_spec is not None else windows
    if spec_map:
        multi = compute_multi_regime(df, spec_map, default_adx_threshold=adx_threshold)
        primary_key = (
            REGIME_PRIMARY_WINDOW_KEY
            if REGIME_PRIMARY_WINDOW_KEY in spec_map
            else sorted(spec_map.keys())[0]
        )
        strategy_payload = multi.get(primary_key, disabled)
        atr_key = (atr_window or primary_key).strip() or primary_key
        atr_entry = multi.get(atr_key, strategy_payload)
        live_atr = str(atr_entry.get("regime") or "")
        return multi, live_atr, strategy_payload

    legacy = classify_window(
        df,
        {"classifier": CLASSIFIER_ADX, "period": period, "adx_threshold": adx_threshold},
        default_adx_threshold=adx_threshold,
    )
    label = str(legacy.get("regime") or "")
    return label, label, legacy


def parse_regime_windows_spec_json(raw: str | None) -> dict[str, dict[str, Any]] | None:
    """Parse --regime-windows-spec-json from Go (#795). Bare ints → ADX specs."""
    if raw is None:
        return None
    text = str(raw).strip()
    if not text:
        return None
    parsed = json.loads(text)
    if not isinstance(parsed, dict) or not parsed:
        raise ValueError("regime windows spec JSON must be a non-empty object")
    out: dict[str, dict[str, Any]] = {}
    for name, value in parsed.items():
        key = str(name).strip()
        if not key:
            raise ValueError("regime window names must be non-empty")
        if key.lower() == "regime":
            raise ValueError(
                "regime window name 'regime' is reserved (conflicts with legacy regime snapshot JSON)"
            )
        if isinstance(value, int) and not isinstance(value, bool):
            out[key] = {"classifier": CLASSIFIER_ADX, "period": value}
        elif isinstance(value, dict):
            out[key] = _normalize_spec(value)
        else:
            raise ValueError(f"regime window {key!r}: must be int or object spec")
    return out


def parse_regime_windows_json(raw: str | None) -> dict[str, int] | None:
    """Legacy int-only windows JSON (#792). Prefer parse_regime_windows_spec_json."""
    spec = parse_regime_windows_spec_json(raw)
    if spec is None:
        return None
    return {name: int(s["period"]) for name, s in spec.items()}


def ensure_regime_columns(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
    *,
    classifier: str = CLASSIFIER_ADX,
    thresholds: dict[str, float] | None = None,
    windows_spec: dict[str, dict[str, Any]] | None = None,
    gate_window: str = "",
) -> pd.DataFrame:
    """Inject regime columns in-place for backtests (#737/#795)."""
    if all(col in df.columns for col in _REGIME_COLUMNS):
        return df

    cls = CLASSIFIER_ADX
    th = thresholds
    p = period
    if windows_spec:
        key = (gate_window or "").strip() or ("medium" if "medium" in windows_spec else sorted(windows_spec.keys())[0])
        spec = windows_spec.get(key) or next(iter(windows_spec.values()))
        cls = str(spec.get("classifier") or CLASSIFIER_ADX).lower()
        p = int(spec["period"])
        if cls == CLASSIFIER_COMPOSITE:
            th = spec.get("thresholds") or _DEFAULT_COMPOSITE_THRESHOLDS
        else:
            adx_threshold = float(spec.get("adx_threshold") or adx_threshold)

    if cls == CLASSIFIER_COMPOSITE:
        reg_df = compute_regime_composite(df, period=p, thresholds=th)
    else:
        reg_df = compute_regime(df, period=p, adx_threshold=adx_threshold)
    for col in _REGIME_COLUMNS:
        df[col] = reg_df[col].values
    return df
