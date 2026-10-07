"""Per-feature regime labels for a live-config backtest.

Window selection is `resolve_strategy_regime_window`. Bar labels are
`bounded_window_labels`, which does not shift. Callers that align a higher
timeframe shift once before the forward fill. The Backtester owns the one
closed-bar decision shift.
"""

from __future__ import annotations

from typing import Optional

_FIELDS = (
    ("gate", "regime_gate_window"),
    ("directional", "regime_directional_window"),
    ("atr", "regime_atr_window"),
)


def _named(value) -> bool:
    return str(value or "").strip().lower() not in ("", "default")


def spec_window_key(windows_spec: dict | None, key: str) -> str:
    """Return the configured window name so payload lookup matches its key."""
    wanted = str(key or "").strip().lower()
    for name in windows_spec or {}:
        if str(name).strip().lower() == wanted:
            return str(name)
    return str(key or "")


def column_name(window_key: str) -> str:
    slug = str(window_key or "default").strip().lower() or "default"
    return "regime_w_" + "".join(ch if ch.isalnum() else "_" for ch in slug)


def _window_period(regime: dict, key: str, windows: dict, fallback: int) -> int:
    wanted = str(key or "").strip().lower()
    for name, spec in windows.items():
        if str(name).strip().lower() != wanted:
            continue
        if isinstance(spec, dict):
            try:
                return int(spec.get("period") or fallback)
            except (TypeError, ValueError):
                return fallback
        if isinstance(spec, int) and not isinstance(spec, bool):
            return spec
    return fallback


def resolve_feature_windows(strategy: dict | None, regime: dict | None, *,
                            label: str = "") -> dict:
    """Resolve gate, directional and atr with the merged helper.

    A named selector that is not in `regime.windows` raises ValueError and
    the message names the field.
    """
    from regime import (
        CLASSIFIER_ADX,
        classifier_for_window,
        primary_regime_window_key,
        required_ohlcv_limit,
        resolve_strategy_regime_window,
        valid_labels_for_classifier,
    )

    strategy = strategy or {}
    regime = regime if isinstance(regime, dict) else {}
    windows = regime.get("windows") or {}
    if not isinstance(windows, dict):
        windows = {}
    enabled = bool(regime.get("enabled"))
    multi = enabled and len(windows) > 0
    known = {str(name).strip().lower() for name in windows}
    prefix = f"{label}: " if label else ""
    resolved, named, classifiers, lookbacks, vocabs, parser = {}, {}, {}, {}, {}, {}
    for field, raw_name in _FIELDS:
        raw = strategy.get(raw_name)
        key = resolve_strategy_regime_window(strategy, field, regime)
        is_named = _named(raw)
        if is_named and not multi:
            raise ValueError(
                f"{prefix}{raw_name}={raw!r} requires regime.windows to be configured")
        if is_named and key not in known:
            raise ValueError(
                f"{prefix}{raw_name}={raw!r} not found in regime.windows "
                f"(valid: {sorted(known)})")
        resolved[field] = key
        named[field] = is_named
        clf = classifier_for_window(regime if multi else {}, key)
        classifiers[field] = clf
        labels = sorted(valid_labels_for_classifier(clf))
        vocabs[field] = labels
        parser[field] = tuple(labels) if clf != CLASSIFIER_ADX else None
        lookbacks[field] = _window_period(
            regime, key, windows, int(regime.get("period") or 14))
    period = int(regime.get("period") or 14)
    return {
        "windows": resolved,
        "named": named,
        "classifiers": classifiers,
        "lookbacks": lookbacks,
        "vocabularies": vocabs,
        "parser_labels": parser,
        "primary": primary_regime_window_key(windows) if multi else "default",
        "windows_spec": windows or None,
        "period": period,
        "adx_threshold": float(regime.get("adx_threshold") or 20.0),
        "limit": required_ohlcv_limit(period, windows or None),
        "source": "bounded_window_labels",
        "timeframe": str(regime.get("timeframe") or "").strip().lower(),
    }


def regime_label_report(plan: dict) -> dict:
    windows = {}
    for field in ("gate", "directional", "atr"):
        windows[field] = {
            "window": plan["windows"][field],
            "classifier": plan["classifiers"][field],
            "lookback": plan["lookbacks"][field],
            "column": column_name(plan["windows"][field]),
            "named": bool(plan["named"][field]),
        }
    return {
        "source": plan.get("source") or "bounded_window_labels",
        "windows": windows,
        "vocabularies": {k: list(v) for k, v in sorted(plan["vocabularies"].items())},
    }


def engine_label_columns(plan: dict) -> dict:
    windows = plan["windows"]
    out = {
        "gate": column_name(windows["gate"]),
        "directional": column_name(windows["directional"]),
        "directional_named": bool(plan["named"]["directional"]),
        "atr_named": bool(plan["named"]["atr"]),
        "payload_row": "decision_bar",
        "gate_row": "decision_bar",
    }
    if plan["named"]["atr"]:
        out["atr"] = column_name(windows["atr"])
    return out


def feature_label_kwargs(plan: dict) -> dict:
    return {
        "regime_feature_labels": {
            "atr": plan["parser_labels"]["atr"],
            "directional": tuple(plan["vocabularies"]["directional"]),
        },
    }


def prepared_window_keys(plan: dict) -> set:
    return {str(plan["windows"][field]).strip().lower()
            for field in ("gate", "directional", "atr")}


def attach_regime_label_columns(df, plan: dict, *, regime_frame=None):
    """Write one unshifted column per distinct window.

    `regime_frame` is the regime-timeframe candle frame. Labels are built
    there with no shift inside `bounded_window_labels`, then shifted once
    and forward-filled onto `df.index`. That shift is the higher-timeframe
    alignment, not the decision shift.
    """
    from regime import bounded_window_labels

    windows = plan["windows"]
    spec = plan.get("windows_spec") or None
    period = int(plan.get("period") or 14)
    adx = float(plan.get("adx_threshold") or 20.0)
    limit = int(plan["limit"])
    columns = {}
    keys = [windows[field] for field in ("gate", "directional", "atr")]
    keys.append(plan.get("primary") or windows["gate"])
    for key in keys:
        columns[column_name(key)] = spec_window_key(spec, key)
    target = df
    if regime_frame is not None:
        labeled = regime_frame.copy().sort_index()
        bounded_window_labels(
            labeled, period=period, adx_threshold=adx, windows_spec=spec,
            limit=limit, columns=columns)
        aligned = labeled.loc[:, list(columns)].shift(1).reindex(df.index, method="ffill")
        for col in columns:
            df[col] = aligned[col].fillna("").map(lambda v: str(v or "").strip())
    else:
        bounded_window_labels(
            target, period=period, adx_threshold=adx, windows_spec=spec,
            limit=limit, columns=columns)
    primary_col = column_name(plan.get("primary") or windows["gate"])
    df["regime"] = df[primary_col].fillna("").map(lambda v: str(v or "").strip())
    return df, engine_label_columns(plan)


def missing_label_columns(df, plan: dict) -> list:
    needed = [column_name(plan["windows"][field]) for field in ("gate", "directional", "atr")]
    return sorted({name for name in needed if name not in getattr(df, "columns", [])})


def overlay_regime_columns(source, signals):
    """Copy prepared label columns onto a signal frame that shares the index."""
    cols = [c for c in source.columns
            if c == "regime" or str(c).startswith("regime_w_")]
    for col in cols:
        signals[col] = source[col].reindex(signals.index).fillna("").map(
            lambda v: str(v or "").strip()).values
    return signals


def require_label_frame(df, plan: Optional[dict]):
    if not plan:
        return
    missing = missing_label_columns(df, plan)
    if missing:
        raise ValueError(
            "refusing missing regime label column " + ", ".join(missing)
            + "; an absent named column is not recomputed from the primary column")
