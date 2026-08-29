#!/usr/bin/env python3

import sys
import os
import argparse
import json
from copy import deepcopy
from typing import List, Optional

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, normalize_atr_method
from data_fetcher import load_cached_data
from directional_certification import (
    config_directional_classifier,
    load_certifications,
    is_directional_certified,
    certified_states,
    backtest_classifier,
)

FUNDING_COLUMN_STRATEGIES = {"funding_skew", "delta_neutral_funding"}

FUNDING_ACCRUAL_STRATEGIES = {"delta_neutral_funding"}


def _attach_funding_if_needed(df, strategy_name, symbol, since):
    if strategy_name not in FUNDING_COLUMN_STRATEGIES or df.empty:
        return df
    from funding_fetcher import (attach_funding_accrual_column,
                                 attach_funding_column, load_cached_funding)
    coin = symbol.split("/")[0]
    try:
        funding = load_cached_funding(coin, since, end_date=df.index[-1])
    except Exception as e:
        print(f"[WARN] funding history fetch failed for {coin}: {e} — "
              f"'{strategy_name}' will produce zero entries.")
        funding = None
    out = attach_funding_column(df, funding)
    have = int(out["funding_rate"].notna().sum())
    if have == 0:
        print(f"[WARN] no funding data attached for {coin} since {since} — "
              f"'{strategy_name}' will produce zero entries.")
    else:
        print(f"  Funding: {have}/{len(out)} bars covered (HL hourly, coin={coin})")
    if strategy_name in FUNDING_ACCRUAL_STRATEGIES:
        out = attach_funding_accrual_column(out, funding)
    return out
from htf_filter import get_default_htf, apply_htf_filter
from registry_loader import load_registry
from backtester import Backtester, format_results
from optimizer import (walk_forward_optimize, DEFAULT_PARAM_RANGES,
                       DEFAULT_CLOSE_STACK_SPECS, generate_close_stack_grid)
from reporter import (
    format_single_report, format_comparison_report,
    format_multi_asset_report, format_walk_forward_report,
    generate_full_report,
)
from regime import (
    compute_regime,
    compute_regime_composite,
    ensure_regime_columns,
    normalize_regime_gate_on_failure,
    parse_regime_windows_spec_json,
    valid_labels_for_classifier,
    CLASSIFIER_ADX,
    CLASSIFIER_COMPOSITE,
    REGIME_PRIMARY_WINDOW_KEY,
    VALID_LABELS_COMPOSITE,
)

_REGIME_COLUMNS = ("regime", "regime_score", "adx", "plus_di", "minus_di")


def _normalize_regime_window_spec(spec) -> dict:
    if isinstance(spec, (int, float)) and not isinstance(spec, bool):
        return {"classifier": "adx", "period": int(spec)}
    spec = dict(spec or {})
    classifier = str(spec.get("classifier") or "adx").strip().lower() or "adx"
    out = {"classifier": classifier, "period": int(spec.get("period") or 0)}
    if classifier == "adx":
        out["adx_threshold"] = float(spec.get("adx_threshold") or 20.0)
    else:
        out["thresholds"] = dict(spec.get("thresholds") or {})
    return out


def _resolve_regime_windows_spec(regime_cfg: dict) -> Optional[dict]:
    if not regime_cfg or not regime_cfg.get("enabled"):
        return None
    windows = regime_cfg.get("windows") or {}
    if not windows:
        return None
    top_period = int(regime_cfg.get("period", 14) or 14)
    top_adx = float(regime_cfg.get("adx_threshold", 20.0) or 20.0)
    raw: dict = {}
    for name, spec in windows.items():
        if isinstance(spec, (int, float)) and not isinstance(spec, bool):
            entry: dict = {"classifier": "adx", "period": int(spec)}
        else:
            entry = dict(spec or {})
        classifier = str(entry.get("classifier") or "adx").strip().lower() or "adx"
        period = int(entry.get("period") or 0)
        if period <= 0:
            period = top_period
        out_entry: dict = {"classifier": classifier, "period": period}
        if classifier == "adx":
            adx_th = float(entry.get("adx_threshold") or 0.0)
            if adx_th <= 0:
                adx_th = top_adx if top_adx > 0 else 20.0
            out_entry["adx_threshold"] = adx_th
        else:
            th = dict(entry.get("thresholds") or {})
            if th:
                out_entry["thresholds"] = th
        raw[str(name)] = out_entry
    import json as _json
    return parse_regime_windows_spec_json(_json.dumps(raw))


def _primary_window_classifier(spec: Optional[dict]) -> str:
    if not spec:
        return CLASSIFIER_ADX
    primary_key = (
        REGIME_PRIMARY_WINDOW_KEY
        if REGIME_PRIMARY_WINDOW_KEY in spec
        else sorted(spec.keys())[0]
    )
    return str(spec[primary_key].get("classifier") or CLASSIFIER_ADX).strip().lower()


def _resolve_backtestable_hurst_gate(
    hurst_cfg: dict,
    sc: dict,
    regime_cfg: dict,
    strategy_id: str,
    config_path: str,
) -> dict:
    from hurst_gate import validate_hurst_gate_config

    prefix = f"{config_path}: strategy {strategy_id!r}"

    try:
        validate_hurst_gate_config(hurst_cfg)
    except ValueError as exc:
        raise ValueError(f"{prefix} {exc}") from exc

    strategy_type = str(sc.get("type") or "").strip().lower()
    if strategy_type in ("options", "manual"):
        raise ValueError(
            f"{prefix} configures a hurst_gate on type={strategy_type!r}, which the "
            f"live daemon rejects at config load — the gate wires into the "
            f"spot/perps/futures signal dispatch only (#1411)."
        )

    if not regime_cfg.get("enabled"):
        raise ValueError(
            f"{prefix} configures a hurst_gate but regime.enabled=false. The hurst "
            f"metric is produced by the regime bundle, so the live daemon rejects "
            f"this at config load (#1411)."
        )

    windows = regime_cfg.get("windows") or {}
    if not isinstance(windows, dict) or not windows:
        raise ValueError(
            f"{prefix} configures a hurst_gate but regime.windows is empty. Only the "
            f"composite classifier emits a hurst metric; the legacy single-lookback "
            f"regime path never does (#1411)."
        )

    explicit = str(hurst_cfg.get("window_key") or "").strip().lower()
    gate_window = str(sc.get("regime_gate_window") or "").strip().lower()
    key = explicit if explicit not in ("", "default") else gate_window
    normalized = {str(k).strip().lower(): (k, v) for k, v in windows.items()}
    primary_key = "medium" if "medium" in normalized else sorted(normalized)[0]
    if key in ("", "default"):
        key = primary_key
    if key not in normalized:
        raise ValueError(
            f"{prefix} configures a hurst_gate on window {key!r}, which is not in "
            f"regime.windows (valid: {', '.join(sorted(normalized))}) (#1411)."
        )

    if key != primary_key:
        raise ValueError(
            f"{prefix} gates hurst on window {key!r}, but the backtester classifies "
            f"only the PRIMARY window ({primary_key!r}) — a named non-primary window "
            f"has no bar-level parity path. Point hurst_gate.window_key at the "
            f"primary window, or drop the gate for backtesting (#1411)."
        )

    spec = normalized[key][1]
    classifier = ""
    if isinstance(spec, dict):
        classifier = str(spec.get("classifier") or "").strip().lower()
    classifier = classifier or "adx"
    if classifier != "composite":
        raise ValueError(
            f"{prefix} gates hurst on window {key!r}, whose classifier is "
            f"{classifier!r}. The hurst metric is emitted ONLY by the "
            f'"composite" classifier (shared_tools/regime.py '
            f"latest_regime_composite), so the live daemon rejects this at config "
            f"load (#1411)."
        )

    resolved = dict(hurst_cfg)
    from hurst_gate import normalize_hurst_on_failure

    per_raw = str(hurst_cfg.get("on_failure") or "").strip().lower()
    try:
        global_raw = normalize_hurst_on_failure(regime_cfg.get("hurst_gate_on_failure"))
    except ValueError as exc:
        raise ValueError(f"{config_path}: regime.{exc}") from exc
    resolved["on_failure"] = (
        normalize_hurst_on_failure(per_raw) if per_raw else global_raw
    )
    resolved["window_key"] = key
    return resolved


def _validate_allowed_regimes_vocabulary(
    allowed_regimes: Optional[List[str]],
    windows_spec: Optional[dict],
) -> None:
    if not allowed_regimes:
        return
    classifier = _primary_window_classifier(windows_spec)
    valid = valid_labels_for_classifier(classifier)
    invalid = [lab for lab in allowed_regimes if lab not in valid]
    if not invalid:
        return
    msg = (
        f"--allowed-regimes {invalid!r}: not valid label(s) for the primary "
        f"regime window's {classifier!r} classifier. Valid: "
        f"{', '.join(sorted(valid))}."
    )
    if classifier == CLASSIFIER_ADX and any(lab in VALID_LABELS_COMPOSITE for lab in invalid):
        msg += (
            " (Composite 9-state labels require a composite primary window — "
            "supply --regime-windows-spec-json with a composite classifier, or "
            "use --config.)"
        )
    print(msg)
    sys.exit(1)


def _build_profile_label_series(df: pd.DataFrame, window_spec: dict) -> pd.Series:
    classifier = window_spec.get("classifier", "adx")
    period = int(window_spec.get("period") or 14)
    if classifier == "composite":
        reg = compute_regime_composite(df, period=period,
                                       thresholds=window_spec.get("thresholds") or None)
    else:
        reg = compute_regime(df, period=period,
                             adx_threshold=float(window_spec.get("adx_threshold") or 20.0))
    return reg["regime"].astype(str)


def _aligned_regime_columns(
    symbol: str,
    trade_index: pd.Index,
    regime_timeframe: str,
    since: str,
    *,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    regime_windows_spec: Optional[dict] = None,
) -> Optional[pd.DataFrame]:
    regime_df = load_cached_data(symbol, regime_timeframe, start_date=since)
    if regime_df.empty:
        print(f"No regime data available for {symbol} {regime_timeframe}")
        return None
    source = regime_df.copy().sort_index()
    ensure_regime_columns(
        source,
        period=regime_period,
        adx_threshold=regime_adx_threshold,
        windows_spec=regime_windows_spec,
    )
    cols = source.loc[:, list(_REGIME_COLUMNS)].shift(1)
    aligned = cols.reindex(trade_index, method="ffill")
    aligned["regime"] = aligned["regime"].fillna("").astype(str)
    for col in _REGIME_COLUMNS:
        if col != "regime":
            aligned[col] = aligned[col].fillna(0.0)
    return aligned


def _apply_regime_timeframe_override(
    df: pd.DataFrame,
    symbol: str,
    trade_timeframe: str,
    regime_timeframe: Optional[str],
    since: str,
    *,
    regime_period: int,
    regime_adx_threshold: float,
    regime_windows_spec: Optional[dict],
) -> Optional[pd.DataFrame]:
    tf = str(regime_timeframe or "").strip().lower()
    trade_tf = str(trade_timeframe or "").strip().lower()
    if not tf or tf == trade_tf:
        return df
    aligned = _aligned_regime_columns(
        symbol,
        df.index,
        tf,
        since,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        regime_windows_spec=regime_windows_spec,
    )
    if aligned is None:
        return None
    out = df.copy()
    for col in _REGIME_COLUMNS:
        out[col] = aligned[col].values
    return out


def _profile_label_series(
    df: pd.DataFrame,
    symbol: str,
    trade_timeframe: str,
    regime_timeframe: Optional[str],
    since: str,
    window_spec: dict,
) -> Optional[pd.Series]:
    tf = str(regime_timeframe or "").strip().lower()
    trade_tf = str(trade_timeframe or "").strip().lower()
    if not tf or tf == trade_tf:
        return _build_profile_label_series(df, window_spec)
    regime_df = load_cached_data(symbol, tf, start_date=since)
    if regime_df.empty:
        print(f"No regime profile data available for {symbol} {tf}")
        return None
    labels = _build_profile_label_series(regime_df.copy().sort_index(), window_spec).shift(1)
    return labels.reindex(df.index, method="ffill").fillna("").astype(str)


def _htf_trend_series(symbol: str, timeframe: str, ltf_index: pd.Index,
                      ema_period: int = 50) -> pd.Series:
    htf = get_default_htf(timeframe)
    htf_df = load_cached_data(symbol, htf)
    if htf_df.empty or len(htf_df) < ema_period:
        return pd.Series(0, index=ltf_index, dtype=int)

    closes = htf_df["close"].astype(float)
    ema = closes.ewm(span=ema_period, adjust=False).mean()
    trend = pd.Series(
        np.where(closes > ema, 1, np.where(closes < ema, -1, 0)),
        index=htf_df.index,
        dtype=int,
    )
    return trend.shift(1).reindex(ltf_index, method="ffill").fillna(0).astype(int)


def _apply_htf_filter_to_df(df: pd.DataFrame, symbol: str,
                            timeframe: str) -> pd.DataFrame:
    if "signal" not in df.columns:
        return df
    trend = _htf_trend_series(symbol, timeframe, df.index)
    df = df.copy()
    df["signal"] = [
        apply_htf_filter(int(s), int(t))
        for s, t in zip(df["signal"].fillna(0).astype(int), trend)
    ]
    return df


DEFAULT_SYMBOLS = ["BTC/USDT", "ETH/USDT", "SOL/USDT", "BNB/USDT"]
DEFAULT_TIMEFRAMES = ["4h", "1d"]


_USER_CLOSE_DEFAULTS_SUPPORTED = {
    "tiered_tp_pct",
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "trailing_tp_ratchet",
    "trailing_tp_ratchet_regime",
}
_USER_CLOSE_DEFAULT_REGIME_ATR_KEY = "regime_atr"

_LEGACY_V17_TRAIL_STOP_KEY = "trailing_stop_atr_regime"
_LEGACY_V18_TRAIL_STOP_KEY = "trail_stop_atr_regime"
_LEGACY_V18_STOP_LOSS_KEY = "stop_loss_atr_regime"
_V19_STOP_LOSS_KEY = "stop_loss_atr_mult_regime"
_V19_TRAIL_STOP_KEY = "trailing_stop_atr_mult_regime"

_ATR_REGIME_KEY_RENAMES = (
    (_LEGACY_V17_TRAIL_STOP_KEY, _V19_TRAIL_STOP_KEY),
    (_LEGACY_V18_TRAIL_STOP_KEY, _V19_TRAIL_STOP_KEY),
    (_LEGACY_V18_STOP_LOSS_KEY, _V19_STOP_LOSS_KEY),
)


def _normalize_atr_regime_keys(node):
    if isinstance(node, dict):
        out = {}
        for key, value in node.items():
            normalized = _normalize_atr_regime_keys(value)
            rewritten = False
            for legacy, canon in _ATR_REGIME_KEY_RENAMES:
                if key == legacy:
                    if canon in node:
                        rewritten = True
                        break
                    out[canon] = normalized
                    rewritten = True
                    break
            if rewritten:
                continue
            out[key] = normalized
        return out
    if isinstance(node, list):
        return [_normalize_atr_regime_keys(v) for v in node]
    return node


_STOP_OWNER_KEYS = (
    "stop_loss_atr_mult",
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_atr_mult",
    "trailing_stop_pct",
    _V19_STOP_LOSS_KEY,
    _V19_TRAIL_STOP_KEY,
)


def _user_close_default_entry(user_defaults: Optional[dict], name: str) -> Optional[dict]:
    if not isinstance(user_defaults, dict):
        return None
    want = str(name or "").strip().lower()
    entry = user_defaults.get(want)
    if isinstance(entry, dict):
        return entry
    for key in sorted(user_defaults):
        if str(key or "").strip().lower() == want:
            entry = user_defaults.get(key)
            return entry if isinstance(entry, dict) else None
    return None


def _json_equivalent(a, b) -> bool:
    return json.dumps(a, sort_keys=True, separators=(",", ":")) == json.dumps(
        b, sort_keys=True, separators=(",", ":")
    )


def _split_legacy_user_close_defaults(legacy: Optional[dict]) -> tuple[dict, object, bool]:
    if legacy is None:
        return {}, None, False
    if not isinstance(legacy, dict):
        raise ValueError("user_close_defaults: must be an object")
    close_defaults: dict = {}
    regime_atr = None
    regime_present = False
    for key in sorted(legacy):
        norm = str(key or "").strip().lower()
        value = legacy[key]
        if norm == _USER_CLOSE_DEFAULT_REGIME_ATR_KEY:
            if regime_present and not _json_equivalent(regime_atr, value):
                raise ValueError("user_close_defaults contains conflicting regime_atr entries")
            regime_atr = value
            regime_present = True
            continue
        if norm in close_defaults and not _json_equivalent(close_defaults[norm], value):
            raise ValueError(f"user_close_defaults contains conflicting {norm!r} entries")
        close_defaults[norm] = value
    return close_defaults, regime_atr, regime_present


def _effective_user_close_defaults(cfg: dict) -> Optional[dict]:
    user_defaults = cfg.get("user_defaults")
    if user_defaults is None:
        user_defaults = {}
    if not isinstance(user_defaults, dict):
        raise ValueError("user_defaults: must be an object")

    close_present = "close" in user_defaults and user_defaults.get("close") is not None
    close_defaults = user_defaults.get("close") if close_present else {}
    if not isinstance(close_defaults, dict):
        raise ValueError("user_defaults.close: must be an object")
    for key in close_defaults:
        if str(key or "").strip().lower() == _USER_CLOSE_DEFAULT_REGIME_ATR_KEY:
            raise ValueError('user_defaults.close["regime_atr"]: regime_atr moved to user_defaults.regime_atr')

    regime_present = "regime_atr" in user_defaults and user_defaults.get("regime_atr") is not None
    regime_atr = user_defaults.get("regime_atr") if regime_present else None

    legacy_present = "user_close_defaults" in cfg and cfg.get("user_close_defaults") is not None
    legacy_close, legacy_regime, legacy_regime_present = _split_legacy_user_close_defaults(
        cfg.get("user_close_defaults") if legacy_present else None
    )
    if legacy_close:
        if close_present and not _json_equivalent(close_defaults, legacy_close):
            raise ValueError("user_defaults.close conflicts with deprecated user_close_defaults")
        if not close_present:
            close_defaults = legacy_close
            close_present = True
    if legacy_regime_present:
        if regime_present and not _json_equivalent(regime_atr, legacy_regime):
            raise ValueError("user_defaults.regime_atr conflicts with deprecated user_close_defaults.regime_atr")
        if not regime_present:
            regime_atr = legacy_regime
            regime_present = True
    if regime_present and not isinstance(regime_atr, dict):
        raise ValueError("user_defaults.regime_atr: must be an object")

    combined = {}
    if close_present:
        combined.update(close_defaults)
    if regime_present:
        combined[_USER_CLOSE_DEFAULT_REGIME_ATR_KEY] = regime_atr
    return combined or None


def _uses_trailing_tp_ratchet_regime(close_refs: list) -> bool:
    return any(
        str(ref.get("name", "")).strip().lower() == "trailing_tp_ratchet_regime"
        for ref in close_refs
        if isinstance(ref, dict)
    )


def _has_explicit_stop_owner(sc: dict) -> bool:
    return any(sc.get(k) is not None for k in _STOP_OWNER_KEYS)


def _regime_atr_block_is_use_defaults_only(raw) -> bool:
    if not isinstance(raw, dict):
        return False
    if raw.get("trend_regime") is not None:
        return False
    return raw.get("use_defaults") is True


def _validate_user_close_defaults_regime_atr(user_defaults: Optional[dict]) -> None:
    entry = _user_close_default_entry(user_defaults, _USER_CLOSE_DEFAULT_REGIME_ATR_KEY)
    if entry is None:
        return
    section = "user_defaults.regime_atr"
    if not isinstance(entry, dict):
        raise ValueError(f"{section}: must be an object")
    if not entry:
        raise ValueError(f"{section}: must not be empty")
    allowed = {_V19_STOP_LOSS_KEY, _V19_TRAIL_STOP_KEY}
    for key in entry:
        if key not in allowed:
            raise ValueError(
                f'{section}: unknown key {key!r} '
                f"(only {_V19_STOP_LOSS_KEY} and {_V19_TRAIL_STOP_KEY} are allowed)"
            )
    _close_dir = os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close"
    )
    if _close_dir not in sys.path:
        sys.path.insert(0, _close_dir)
    from regime_atr import (
        CANONICAL_TREND_REGIME_LABELS,
        REGIME_CLASSIFIER_KEY,
        SURFACE_STOP_LOSS,
        SURFACE_TRAILING,
        parse_regime_atr_block,
    )

    def _validate_sub(sub_key: str, surface: str) -> None:
        raw = entry.get(sub_key)
        if raw is None:
            return
        ctx = f'{section}.{sub_key}'
        if not isinstance(raw, dict) or not raw:
            raise ValueError(f"{ctx}: must be a non-empty object")
        labels = list(CANONICAL_TREND_REGIME_LABELS)
        trend = raw.get(REGIME_CLASSIFIER_KEY)
        if isinstance(trend, dict) and trend:
            labels = sorted(trend.keys())
        _, errs = parse_regime_atr_block(raw, ctx, surface, labels)
        if errs:
            raise ValueError(errs[0])

    _validate_sub(_V19_STOP_LOSS_KEY, SURFACE_STOP_LOSS)
    _validate_sub(_V19_TRAIL_STOP_KEY, SURFACE_TRAILING)


def _apply_user_close_defaults(close_refs: list, user_defaults: Optional[dict],
                               sc: Optional[dict] = None) -> None:
    if not user_defaults:
        return
    for ref in close_refs:
        name = str(ref.get("name", "")).strip().lower()
        if name not in _USER_CLOSE_DEFAULTS_SUPPORTED:
            continue
        params = ref.setdefault("params", {})
        if params.get("tp_tiers") is not None:
            continue
        entry = _user_close_default_entry(user_defaults, name)
        if entry is None:
            continue
        tp = entry.get("tp_tiers")
        if (isinstance(tp, list) or isinstance(tp, dict)) and tp:
            params["tp_tiers"] = tp
    if sc is not None and _uses_trailing_tp_ratchet_regime(close_refs):
        if not _has_explicit_stop_owner(sc):
            entry = _user_close_default_entry(user_defaults, "trailing_tp_ratchet_regime")
            if entry is not None:
                trail = entry.get(_V19_TRAIL_STOP_KEY)
                if isinstance(trail, dict) and trail:
                    sc[_V19_TRAIL_STOP_KEY] = deepcopy(trail)
        return
    if sc is None:
        return
    regime_entry = _user_close_default_entry(user_defaults, _USER_CLOSE_DEFAULT_REGIME_ATR_KEY)
    if regime_entry is None:
        return
    sl_raw = regime_entry.get(_V19_STOP_LOSS_KEY)
    if (
        isinstance(sl_raw, dict)
        and sl_raw
        and _regime_atr_block_is_use_defaults_only(sc.get(_V19_STOP_LOSS_KEY))
    ):
        sc[_V19_STOP_LOSS_KEY] = deepcopy(sl_raw)
    trail_raw = regime_entry.get(_V19_TRAIL_STOP_KEY)
    if (
        isinstance(trail_raw, dict)
        and trail_raw
        and _regime_atr_block_is_use_defaults_only(sc.get(_V19_TRAIL_STOP_KEY))
    ):
        sc[_V19_TRAIL_STOP_KEY] = deepcopy(trail_raw)


def _effective_direction(sc: dict) -> str:
    if str(sc.get("type") or "perps") not in ("perps", "manual"):
        return "long"
    d = str(sc.get("direction") or "").strip().lower()
    if d in ("long", "short", "both"):
        return d
    return "both" if sc.get("allow_shorts") else "long"


def _capture_promotion_baseline(cfg: dict, sc: dict) -> dict:
    open_present = "open_strategy" in sc
    ud_present = "user_defaults" in cfg
    ucd_present = "user_close_defaults" in cfg
    return {
        "open_strategy": deepcopy(sc["open_strategy"]) if open_present else None,
        "open_strategy_present": open_present,
        "user_defaults": deepcopy(cfg["user_defaults"]) if ud_present else None,
        "user_defaults_present": ud_present,
        "user_close_defaults": (
            deepcopy(cfg["user_close_defaults"]) if ucd_present else None
        ),
        "user_close_defaults_present": ucd_present,
    }


def load_strategy_config(config_path: str, strategy_id: str,
                         inject_user_defaults: bool = False,
                         include_promotion_baseline: bool = False) -> dict:
    import json as _json
    with open(config_path) as fh:
        cfg = _normalize_atr_regime_keys(_json.load(fh))
    user_defaults = _effective_user_close_defaults(cfg)
    _validate_user_close_defaults_regime_atr(user_defaults)
    version = int(cfg.get("config_version", 0) or 0)
    if version < 15:
        raise ValueError(
            f"{config_path}: config_version={version} predates the v15 "
            f"close-param canonicalization (tiers->tp_tiers, atr/multiple/"
            f"fraction->atr_multiple/close_fraction). The backtest close "
            f"evaluators read only the canonical runtime keys, so a pre-v15 "
            f"config's legacy close params would silently no-op (diverging from "
            f"live, which canonicalizes on read). Run go-trader once against "
            f"this file to migrate it, then retry."
        )
    for sc in cfg.get("strategies", []) or []:
        if sc.get("id") != strategy_id:
            continue
        promotion_baseline = (
            _capture_promotion_baseline(cfg, sc)
            if include_promotion_baseline else None
        )
        open_ref = sc.get("open_strategy")
        if not isinstance(open_ref, dict):
            open_ref = {}
        open_name = str(open_ref.get("name") or "").strip()
        if not open_name:
            args_list = sc.get("args")
            if isinstance(args_list, list) and args_list:
                open_name = str(args_list[0] or "").strip()
        if not open_name:
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} has neither "
                f"open_strategy.name nor a positional args[0] strategy arg to "
                f"resolve the open strategy from."
            )
        regime_cfg = cfg.get("regime") or {}
        if not isinstance(regime_cfg, dict):
            regime_cfg = {}
        regime_timeframe = str(regime_cfg.get("timeframe") or "").strip().lower() or None
        regime_directional_policy = sc.get("regime_directional_policy")
        if regime_directional_policy and not regime_cfg.get("enabled"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} uses "
                f"regime_directional_policy, which requires regime.enabled=true "
                f"for backtest/live parity."
            )
        if sc.get("regime_window_divergence"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} uses "
                f"regime_window_divergence, which is HL-live-only in this "
                f"release (backtester parity deferred — see #907). Use the "
                f"static `direction` / `invert_signal` fields for backtesting."
            )
        hedge_cfg = sc.get("hedge")
        if isinstance(hedge_cfg, dict) and hedge_cfg.get("enabled"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} configures a correlated "
                f"hedge leg (hedge block on {hedge_cfg.get('symbol')!r}), which is "
                f"HL-live-only in phase 1 (#1159). The backtester models a single "
                f"instrument and would silently drop the hedge's PnL, fees and "
                f"slippage — reporting the unhedged primary as if it were the "
                f"strategy. Set hedge.enabled=false (or remove the block) to "
                f"backtest the primary leg alone."
            )
        close_refs = []
        single = sc.get("close_strategy")
        if isinstance(single, dict) and single.get("name"):
            close_refs.append({"name": single["name"], "params": dict(single.get("params") or {})})
        else:
            legacy = sc.get("close_strategies", []) or []
            if len(legacy) > 1:
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} has "
                    f"{len(legacy)} close_strategies; the array model was "
                    f"collapsed to a single close_strategy (#842). Keep one "
                    f"profit-taking close and move risk backstops to "
                    f"strategy-level stop fields."
                )
            for ref in legacy:
                if isinstance(ref, dict) and ref.get("name"):
                    close_refs.append({"name": ref["name"], "params": dict(ref.get("params") or {})})
        for ref in close_refs:
            if ref.get("name") == "tiered_tp_atr_live_regime_dynamic":
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} uses "
                    f"tiered_tp_atr_live_regime_dynamic, which is HL-live-only "
                    f"in this release (backtester parity deferred — see #843)."
                )
        if inject_user_defaults:
            _apply_user_close_defaults(close_refs, user_defaults, sc)
        direction = _effective_direction(sc)
        invert_signal = bool(sc.get("invert_signal"))
        strategy_type = str(sc.get("type") or "perps")
        if invert_signal and strategy_type not in ("perps", "manual"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} sets invert_signal "
                f"on type={strategy_type!r}, but invert_signal is HL-perps/"
                f"manual-only (the live daemon rejects this config at startup — "
                f"config.go). Remove invert_signal or backtest a perps/manual "
                f"strategy."
            )
        if not close_refs and direction == "both":
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} has "
                f"direction='both' but no close_strategy. The backtester's "
                f"plain signal path runs one leg at a time (long/flat, or "
                f"short/flat under direction='short'), so the short side of a "
                f"'both' config would be silently dropped. Add a "
                f"close_strategy (the open/close engine models both sides) or "
                f"backtest each leg separately."
            )
        profile_allocation = None
        pal = sc.get("regime_profile_allocation")
        if pal:
            window = str(pal.get("window") or "").strip()
            regime_cfg = cfg.get("regime") or {}
            windows = regime_cfg.get("windows") or {}
            spec = windows.get(window)
            if not regime_cfg.get("enabled"):
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} uses "
                    f"regime_profile_allocation but regime.enabled is not true."
                )
            if spec is None:
                raise ValueError(
                    f"{config_path}: regime_profile_allocation.window={window!r} "
                    f"not found in regime.windows (have: {sorted(windows)})."
                )
            profile_allocation = {
                "window": window,
                "window_spec": _normalize_regime_window_spec(spec),
                "profiles": dict(pal.get("profiles") or {}),
                "param_sets": {
                    k: dict(v or {}) for k, v in (pal.get("param_sets") or {}).items()
                },
                "confirm_bars": int(pal.get("confirm_bars") or 0),
                "initial_profile": str(pal.get("initial_profile") or "").strip(),
            }
        allowed_regimes = sc.get("allowed_regimes") or None
        _per_raw = str(sc.get("regime_gate_on_failure") or "").strip().lower()
        try:
            _global_gate = normalize_regime_gate_on_failure(
                regime_cfg.get("gate_on_failure")
            )
        except ValueError as exc:
            raise ValueError(f"{config_path}: {exc}") from exc
        try:
            regime_gate_on_failure = (
                normalize_regime_gate_on_failure(_per_raw)
                if _per_raw
                else _global_gate
            )
        except ValueError as exc:
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} {exc}"
            ) from exc
        gate_window = str(sc.get("regime_gate_window") or "").strip().lower()
        if (
            allowed_regimes
            and regime_cfg.get("enabled")
            and gate_window not in ("", "default")
        ):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} gates allowed_regimes "
                f"on regime_gate_window={gate_window!r}, but the backtester models "
                f"only the legacy single-lookback regime (regime.period / "
                f"regime.adx_threshold) — a named gate window has no bar-level "
                f"parity path. Gate on the default lookback (remove "
                f"regime_gate_window) or drop allowed_regimes for backtesting."
            )
        hurst_gate_cfg = sc.get("hurst_gate")
        if isinstance(hurst_gate_cfg, dict) and hurst_gate_cfg.get("enabled"):
            hurst_gate_cfg = _resolve_backtestable_hurst_gate(
                hurst_gate_cfg, sc, regime_cfg, strategy_id, config_path
            )
        else:
            hurst_gate_cfg = None
        try:
            _global_atr = normalize_atr_method(cfg.get("atr_method"))
        except ValueError as exc:
            raise ValueError(f"{config_path}: {exc}") from exc
        _per_atr_raw = str(sc.get("atr_method") or "").strip().lower()
        try:
            atr_method = (
                normalize_atr_method(_per_atr_raw) if _per_atr_raw else _global_atr
            )
        except ValueError as exc:
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} {exc}"
            ) from exc
        risk_per_trade_pct = sc.get("risk_per_trade_pct")
        if risk_per_trade_pct is not None:
            if sc.get("sizing_leverage"):
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} combines "
                    f"risk_per_trade_pct with sizing_leverage — mutually "
                    f"exclusive sizing modes (the live daemon rejects this "
                    f"config at startup; #1268)."
                )
            if sc.get("margin_per_trade_usd") is not None:
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} combines "
                    f"risk_per_trade_pct with margin_per_trade_usd — mutually "
                    f"exclusive sizing modes (the live daemon rejects this "
                    f"config at startup; #1268)."
                )
            if sc.get("allow_scale_in"):
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} combines "
                    f"risk_per_trade_pct with allow_scale_in — the live "
                    f"daemon rejects this config at startup (#1268: add legs "
                    f"re-size off frozen SL geometry, breaking the "
                    f"constant-dollar-risk invariant)."
                )
            for _pk in ("stop_loss_pct", "trailing_stop_pct", "stop_loss_margin_pct"):
                if (sc.get(_pk) or 0) > 0:
                    raise ValueError(
                        f"{config_path}: strategy {strategy_id!r} sizes "
                        f"risk_per_trade_pct from {_pk}, but the backtester's "
                        f"pct-stop fields are fraction-denominated (live is "
                        f"percent), so the risk formula would skew 100×. Use "
                        f"an ATR-mult stop owner (stop_loss_atr_mult / "
                        f"trailing_stop_atr_mult) for risk-sizing backtests."
                    )
            if not any(sc.get(k) is not None for k in _STOP_OWNER_KEYS):
                _default_mult = cfg.get("default_stop_loss_atr_mult")
                if _default_mult is None:
                    _default_mult = 1.0
                _default_mult = float(_default_mult or 0)
                if _default_mult <= 0:
                    raise ValueError(
                        f"{config_path}: strategy {strategy_id!r} sets "
                        f"risk_per_trade_pct with no stop owner and "
                        f"default_stop_loss_atr_mult=0 (auto-default opted "
                        f"out) — no stop distance to size risk from (the "
                        f"live daemon rejects this config at startup; #1268)."
                    )
                sc["stop_loss_atr_mult"] = _default_mult
        cfg_args = sc.get("args") or []
        allow_scale_in = bool(sc.get("allow_scale_in"))
        scale_in_cfg = sc.get("scale_in")
        if allow_scale_in:
            platform_cfg = str(sc.get("platform") or "").strip().lower()
            if strategy_type not in ("perps", "manual"):
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} sets "
                    f"allow_scale_in on type={strategy_type!r}, but scale-in "
                    f"is perps/manual-only (the live daemon rejects this "
                    f"config at startup; #873)."
                )
            if platform_cfg != "hyperliquid":
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} sets "
                    f"allow_scale_in on platform={platform_cfg!r}, but "
                    f"scale-in is hyperliquid-only (the live daemon rejects "
                    f"this config at startup; #873)."
                )
            _is_live_args = False
            for _ai, _arg in enumerate(cfg_args):
                if _arg == "--mode=live" or (
                    _arg == "--mode"
                    and _ai + 1 < len(cfg_args)
                    and cfg_args[_ai + 1] == "live"
                ):
                    _is_live_args = True
                    break
            if strategy_type == "perps" and _is_live_args:
                _has_trailing = (
                    (sc.get("trailing_stop_pct") or 0) > 0
                    or (sc.get("trailing_stop_atr_mult") or 0) > 0
                    or bool(sc.get(_V19_TRAIL_STOP_KEY))
                    or (str((sc.get("close_strategy") or {}).get("name") or "")
                        .strip().lower()
                        in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime"))
                )
                _has_static_scalar_sl = (
                    (sc.get("stop_loss_pct") or 0) > 0
                    or (sc.get("stop_loss_margin_pct") or 0) > 0
                )
                if not _has_trailing and _has_static_scalar_sl:
                    raise ValueError(
                        f"{config_path}: strategy {strategy_id!r} sets "
                        f"allow_scale_in on live perps with a static scalar "
                        f"stop (stop_loss_pct/stop_loss_margin_pct), which "
                        f"the live scale-in resize path cannot grow — the "
                        f"live daemon rejects this config at startup (#873). "
                        f"Use stop_loss_atr_mult, {_V19_STOP_LOSS_KEY}, or a "
                        f"trailing stop."
                    )
        elif scale_in_cfg:
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} has a scale_in "
                f"block but allow_scale_in is false — enable allow_scale_in "
                f"or remove the block (the live daemon rejects this config "
                f"at startup; #873)."
            )
        cert_symbol = str(cfg_args[1]) if len(cfg_args) > 1 else ""
        cert_timeframe = regime_timeframe or (str(cfg_args[2]) if len(cfg_args) > 2 else "")
        regime_directional_certified = False
        regime_directional_certified_states = None
        if regime_directional_policy and cert_symbol and cert_timeframe:
            _certs = load_certifications()
            _clf = config_directional_classifier(regime_cfg, sc)
            regime_directional_certified_states = certified_states(
                _certs, cert_symbol, cert_timeframe, _clf,
            )
            regime_directional_certified = regime_directional_certified_states is not None
        out = {
            "open_strategy": {
                "name": open_name,
                "params": dict(open_ref.get("params") or {}),
            },
            "close_strategies": close_refs,
            "stop_loss_atr_mult": sc.get("stop_loss_atr_mult"),
            "stop_loss_pct": sc.get("stop_loss_pct"),
            "stop_loss_margin_pct": sc.get("stop_loss_margin_pct"),
            "trailing_stop_atr_mult": sc.get("trailing_stop_atr_mult"),
            "trailing_stop_pct": sc.get("trailing_stop_pct"),
            "stop_loss_atr_mult_regime": sc.get(_V19_STOP_LOSS_KEY),
            "trailing_stop_atr_mult_regime": sc.get(_V19_TRAIL_STOP_KEY),
            "strategy_type": strategy_type,
            "direction": direction,
            "invert_signal": invert_signal,
            "regime_directional_policy": regime_directional_policy,
            "regime_directional_certified": regime_directional_certified,
            "regime_directional_certified_states": regime_directional_certified_states,
            "regime_enabled": bool(regime_cfg.get("enabled")),
            "regime_period": int(regime_cfg.get("period", 14) or 14),
            "regime_adx_threshold": float(regime_cfg.get("adx_threshold", 20.0) or 20.0),
            "regime_timeframe": regime_timeframe,
            "regime_windows_spec": _resolve_regime_windows_spec(regime_cfg),
            "allowed_regimes": allowed_regimes,
            "regime_gate_on_failure": regime_gate_on_failure,
            "hurst_gate": hurst_gate_cfg,
            "profile_allocation": profile_allocation,
            "risk_per_trade_pct": risk_per_trade_pct,
            "allow_scale_in": allow_scale_in,
            "scale_in": dict(scale_in_cfg) if scale_in_cfg else None,
            "atr_method": atr_method,
        }
        if include_promotion_baseline:
            out["promotion_baseline"] = promotion_baseline
        return out
    raise ValueError(
        f"{config_path}: no strategy with id={strategy_id!r}. "
        f"Available: {[s.get('id') for s in cfg.get('strategies', []) or []]}"
    )


def run_single_backtest(
    strategy_name: str = "sma_crossover",
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    params: dict = None,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    regime_timeframe: Optional[str] = None,
    regime_windows_spec: Optional[dict] = None,
    hurst_gate: Optional[dict] = None,
    allowed_regimes: Optional[List[str]] = None,
    regime_gate_on_failure: str = "open",
    stop_loss_atr_mult: Optional[float] = None,
    stop_loss_pct: Optional[float] = None,
    stop_loss_margin_pct: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    trailing_stop_pct: Optional[float] = None,
    stop_loss_atr_mult_regime: Optional[dict] = None,
    trailing_stop_atr_mult_regime: Optional[dict] = None,
    strategy_type: str = "perps",
    direction: Optional[str] = None,
    invert_signal: bool = False,
    regime_directional_policy: Optional[dict] = None,
    regime_directional_certified: Optional[bool] = None,
    regime_directional_certified_states: Optional[dict] = None,
    directional_cert_path: Optional[str] = None,
    profile_allocation: Optional[dict] = None,
    intrabar_resolution: str = "ohlc_walk",
    risk_per_trade_pct: Optional[float] = None,
    allow_scale_in: bool = False,
    scale_in: Optional[dict] = None,
    atr_method: str = "simple",
) -> Optional[dict]:
    reg = load_registry(registry)
    strat = reg.STRATEGY_REGISTRY.get(strategy_name)
    if not strat:
        print(f"Unknown strategy '{strategy_name}' in '{registry}' registry")
        print(f"Available: {reg.list_strategies()}")
        return None

    strat_params = params or strat["default_params"]
    print(f"\n▶ Strategy: {strat['description']}")
    print(f"  Params: {strat_params}")
    print(f"  Symbol: {symbol} | Timeframe: {timeframe} | Since: {since}")
    if close_strategies:
        print(f"  Close strategies: {[r.get('name') for r in close_strategies]}")

    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None
    df = _attach_funding_if_needed(df, strategy_name, symbol, since)

    print(f"  Data: {len(df)} candles from {df.index[0]} to {df.index[-1]}")

    if profile_allocation:
        param_sets = profile_allocation["param_sets"]
        names = sorted(param_sets)
        df_signals = None
        for p in names:
            p_params = {**(strat_params or {}), **(param_sets[p] or {})}
            res = reg.apply_strategy(strategy_name, df, p_params)
            if df_signals is None:
                df_signals = res.copy()
                df_signals["signal__" + p] = df_signals.pop("signal")
            else:
                df_signals["signal__" + p] = res["signal"].values
        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals, method=atr_method)
        profile_labels = _profile_label_series(
            df_signals,
            symbol,
            timeframe,
            regime_timeframe,
            since,
            profile_allocation["window_spec"],
        )
        if profile_labels is None:
            return None
        df_signals["_profile_label"] = profile_labels.values
        print(f"  Profile allocation: window={profile_allocation['window']} "
              f"profiles={names} confirm_bars={profile_allocation['confirm_bars']}")
    else:
        df_signals = reg.apply_strategy(strategy_name, df, strat_params)

        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals, method=atr_method)

    if htf_filter:
        df_signals = _apply_htf_filter_to_df(df_signals, symbol, timeframe)
        print(f"  HTF filter: applied (HTF={get_default_htf(timeframe)})")

    if regime_enabled:
        df_signals = _apply_regime_timeframe_override(
            df_signals,
            symbol,
            timeframe,
            regime_timeframe,
            since,
            regime_period=regime_period,
            regime_adx_threshold=regime_adx_threshold,
            regime_windows_spec=regime_windows_spec,
        )
        if df_signals is None:
            return None

    if (regime_directional_policy and regime_directional_certified_states is None
            and regime_directional_certified is None):
        certs = load_certifications(directional_cert_path)
        clf = backtest_classifier(regime_windows_spec)
        cert_timeframe = str(regime_timeframe or timeframe).strip().lower()
        regime_directional_certified_states = certified_states(
            certs, symbol, cert_timeframe, clf,
        )
        if regime_directional_certified_states is None:
            print(f"  [#1085] regime_directional_policy default-off: "
                  f"({symbol},{cert_timeframe},{clf}) not certified — base direction "
                  f"(matches live; #1076 negative result).")
    regime_directional_certified = bool(regime_directional_certified)

    bt = Backtester(
        initial_capital=capital, platform=platform,
        open_strategy={"name": strategy_name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        regime_enabled=regime_enabled,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        regime_windows_spec=regime_windows_spec,
        hurst_gate=hurst_gate,
        allowed_regimes=allowed_regimes,
        regime_gate_on_failure=regime_gate_on_failure,
        stop_loss_atr_mult=stop_loss_atr_mult,
        stop_loss_pct=stop_loss_pct,
        stop_loss_margin_pct=stop_loss_margin_pct,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        trailing_stop_pct=trailing_stop_pct,
        stop_loss_atr_mult_regime=stop_loss_atr_mult_regime,
        trailing_stop_atr_mult_regime=trailing_stop_atr_mult_regime,
        strategy_type=strategy_type,
        direction=direction,
        invert_signal=invert_signal,
        regime_directional_policy=regime_directional_policy,
        regime_directional_certified=regime_directional_certified,
        regime_directional_certified_states=regime_directional_certified_states,
        profile_allocation=profile_allocation,
        intrabar_resolution=intrabar_resolution,
        risk_per_trade_pct=risk_per_trade_pct,
        allow_scale_in=allow_scale_in,
        scale_in=scale_in,
        atr_method=atr_method,
    )
    results = bt.run(
        df_signals,
        strategy_name=strategy_name,
        symbol=symbol,
        timeframe=timeframe,
        params=strat_params,
    )

    print(format_single_report(results))
    return results


def run_all_strategies(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    strategies: Optional[List[str]] = None,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
    direction: Optional[str] = None,
    intrabar_resolution: str = "ohlc_walk",
    atr_method: str = "simple",
) -> list:
    reg = load_registry(registry)
    strat_list = strategies or reg.list_strategies()
    print(f"\n{'#'*60}")
    print(f"  RUNNING {len(strat_list)} STRATEGIES ({registry} / {platform})")
    print(f"  {symbol} | {timeframe} | since {since} | ${capital:,.0f}")
    print(f"{'#'*60}")

    all_results = []
    for name in strat_list:
        result = run_single_backtest(
            name, symbol, timeframe, since, capital,
            registry=registry, platform=platform, htf_filter=htf_filter,
            close_strategies=close_strategies,
            regime_enabled=regime_enabled, regime_period=regime_period,
            regime_adx_threshold=regime_adx_threshold,
            allowed_regimes=allowed_regimes,
            direction=direction,
            intrabar_resolution=intrabar_resolution,
            atr_method=atr_method,
        )
        if result:
            all_results.append(result)

    if all_results:
        print(format_comparison_report(all_results))

    return all_results


def run_multi_asset(
    strategies: Optional[List[str]] = None,
    symbols: Optional[List[str]] = None,
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
    direction: Optional[str] = None,
    intrabar_resolution: str = "ohlc_walk",
    atr_method: str = "simple",
) -> dict:
    reg = load_registry(registry)
    strat_list = strategies or reg.list_strategies()
    sym_list = symbols or DEFAULT_SYMBOLS

    print(f"\n{'#'*60}")
    print(f"  MULTI-ASSET BACKTEST ({registry} / {platform})")
    print(f"  Strategies: {len(strat_list)} | Assets: {len(sym_list)}")
    print(f"  Timeframe: {timeframe} | Since: {since}")
    print(f"{'#'*60}")

    results_by_asset = {}
    for symbol in sym_list:
        print(f"\n{'─'*40}")
        print(f"  Asset: {symbol}")
        print(f"{'─'*40}")
        results_by_asset[symbol] = []
        for strat_name in strat_list:
            result = run_single_backtest(
                strat_name, symbol, timeframe, since, capital,
                registry=registry, platform=platform, htf_filter=htf_filter,
                close_strategies=close_strategies,
                regime_enabled=regime_enabled, regime_period=regime_period,
                regime_adx_threshold=regime_adx_threshold,
                allowed_regimes=allowed_regimes,
                direction=direction,
                intrabar_resolution=intrabar_resolution,
                atr_method=atr_method,
            )
            if result:
                results_by_asset[symbol].append(result)

    print(format_multi_asset_report(results_by_asset))
    return results_by_asset


def run_walk_forward(
    strategy_name: str,
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2020-01-01",
    n_splits: int = 5,
    capital: float = 1000.0,
    registry: str = "spot",
    platform: str = "binanceus",
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
    stop_loss_atr_mult: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    close_strategies: Optional[List[dict]] = None,
    close_stack_grid: Optional[List[dict]] = None,
    optimize_metric: str = "sharpe_ratio",
    direction: Optional[str] = None,
) -> Optional[dict]:
    reg = load_registry(registry)
    strat = reg.STRATEGY_REGISTRY.get(strategy_name)
    if not strat:
        print(f"Unknown strategy '{strategy_name}' in '{registry}' registry")
        return None

    param_ranges = DEFAULT_PARAM_RANGES.get(strategy_name)
    if not param_ranges:
        print(f"[warn] No DEFAULT_PARAM_RANGES for '{strategy_name}' — "
              f"using single-point grid from default_params. "
              f"Add a range entry in optimizer.DEFAULT_PARAM_RANGES for "
              f"meaningful walk-forward results.")
        param_ranges = {k: [v] for k, v in strat["default_params"].items()}
        if not param_ranges:
            print(f"[warn] '{strategy_name}' has no default_params either — skipping.")
            return None

    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None
    df = _attach_funding_if_needed(df, strategy_name, symbol, since)

    result = walk_forward_optimize(
        df, strategy_name, param_ranges,
        n_splits=n_splits,
        initial_capital=capital,
        symbol=symbol,
        timeframe=timeframe,
        registry=registry,
        platform=platform,
        verbose=True,
        regime_enabled=regime_enabled,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        allowed_regimes=allowed_regimes,
        stop_loss_atr_mult=stop_loss_atr_mult,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        close_strategies=close_strategies,
        close_stack_grid=close_stack_grid,
        optimize_metric=optimize_metric,
        direction=direction,
    )

    print(format_walk_forward_report(result))
    return result


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Crypto Trading Bot — Backtester")
    parser.add_argument("--strategy", "-s", default="all",
                        help="Strategy name or 'all'")
    parser.add_argument("--registry", choices=["spot", "futures"], default="spot",
                        help="Strategy registry to load (spot or futures)")
    parser.add_argument("--platform",
                        choices=["binanceus", "hyperliquid", "robinhood",
                                 "luno", "okx", "okx-perps"],
                        default="binanceus",
                        help="Exchange fee model (matches fees.go)")
    parser.add_argument("--symbol", default="BTC/USDT",
                        help="Trading pair")
    parser.add_argument("--symbols", nargs="+", default=None,
                        help="Multiple trading pairs for multi-asset mode")
    parser.add_argument("--timeframe", "-tf", default="1d",
                        help="Candle timeframe (1h, 4h, 1d)")
    parser.add_argument("--since", default="2022-01-01",
                        help="Start date")
    parser.add_argument("--capital", type=float, default=1000.0,
                        help="Starting capital")
    parser.add_argument("--mode", choices=["single", "compare", "multi", "optimize"],
                        default="compare",
                        help="Run mode: single/compare/multi/optimize")
    parser.add_argument("--splits", type=int, default=5,
                        help="Walk-forward splits (optimize mode)")
    parser.add_argument("--htf-filter", action="store_true",
                        help="Apply HTF trend filter (matches live "
                             "shared_tools/htf_filter.py); 50-EMA on the "
                             "default HTF for the chosen timeframe.")
    parser.add_argument("--close-strategy", action="append", dest="close_strategies",
                        default=None, metavar="REF",
                        help="Close-evaluator ref. Two accepted shapes (#641):\n"
                             "  - bare name: --close-strategy tp_at_pct\n"
                             "  - JSON ref:  --close-strategy '{\"name\":\"tp_at_pct\",\"params\":{\"pct\":0.03}}'\n"
                             "Repeat for multiple. Each runs per-bar against the simulated position; "
                             "max close_fraction wins. Replaces the pre-#641 --close-strategy NAME + "
                             "--close-params JSON pair.")
    parser.add_argument("--config", default=None,
                        help="Path to a live go-trader config.json. Loads a single strategy by "
                             "--strategy ID and uses its open_strategy/close_strategies refs verbatim "
                             "for the backtest. Lets you backtest a live config without reshaping (#641).")
    parser.add_argument("--defaults", choices=["system", "user"], default=None,
                        help="Which close-default layer to apply when a close ref omits tp_tiers (#866): "
                             "'user' applies the user_defaults block from --config (the default for "
                             "--config, matching live); 'system' uses the built-in defaults (the "
                             "default without --config, or an explicit baseline override). "
                             "Per-strategy tp_tiers always wins.")
    parser.add_argument("--regime-enabled", action="store_true", default=False,
                        help="Enable market regime detection. Injects vectorized regime "
                             "column from shared_tools/regime.py before the per-bar loop, "
                             "matching the live check-script contract (#482).")
    parser.add_argument("--regime-period", type=int, default=14,
                        help="ADX lookback period for regime detection (default: 14).")
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0,
                        help="ADX threshold below which market is 'ranging' (default: 20.0).")
    parser.add_argument("--regime-windows-spec-json", default=None,
                        dest="regime_windows_spec_json", metavar="JSON",
                        help="Composite (9-state) regime windows spec, same shape as the live "
                             "--regime-windows-spec-json arg: a JSON object mapping window name "
                             "-> {classifier,period,...} (bare int = ADX period). The PRIMARY "
                             "window (medium-first) is classified into the per-bar regime label "
                             "the entry gate and close evaluators read (#1058). Mutually exclusive "
                             "with --config (the config's regime.windows owns it). Single mode only.")
    parser.add_argument("--allowed-regimes", action="append", dest="allowed_regimes",
                        default=None, metavar="LABEL",
                        help="Regime label to allow entries for (repeat for multiple). "
                             "Empty = allow all. Validated against the PRIMARY regime "
                             "window's classifier vocabulary (#1058): ADX (default / no "
                             "--regime-windows-spec-json) accepts trending_up, "
                             "trending_down, ranging; a composite primary window accepts "
                             "the 9-state substates (trending_up_clean, ranging_quiet, ...).")
    parser.add_argument("--stop-loss-atr-mult", type=float, default=None,
                        dest="stop_loss_atr_mult", metavar="MULT",
                        help="Fixed ATR-multiple stop loss (e.g. 2.0). Applied in "
                             "single and optimize/walk-forward modes.")
    parser.add_argument("--trailing-stop-atr-mult", type=float, default=None,
                        dest="trailing_stop_atr_mult", metavar="MULT",
                        help="Trailing ATR-multiple stop (e.g. 2.5). Applied in "
                             "optimize/walk-forward mode.")
    parser.add_argument("--sweep-close", action="store_true",
                        help="Optimize mode (#996): sweep the built-in close-stack "
                             "grid (DEFAULT_CLOSE_STACK_SPECS — baseline, ATR "
                             "stops, tiered-TP ladders) jointly with the open-"
                             "param grid.")
    parser.add_argument("--close-stacks-json", default=None, metavar="PATH",
                        help="Optimize mode (#996): JSON file with a list of "
                             "close-stack sweep specs (see optimizer."
                             "generate_close_stack_grid) swept jointly with "
                             "the open-param grid. Overrides --sweep-close.")
    parser.add_argument("--optimize-metric", default="sharpe_ratio",
                        choices=["sharpe_ratio", "total_return_pct",
                                 "dd_adjusted_return"],
                        help="Selection metric for optimize mode (default: "
                             "sharpe_ratio). dd_adjusted_return = return / "
                             "|max DD| (#963 DDadj).")
    parser.add_argument("--direction", default=None,
                        choices=["long", "short", "both"],
                        help="Side the engine may OPEN; forwarded to every "
                             "mode (#989). 'short' on the plain single-leg "
                             "path runs the short/flat mirror (signal=-1 "
                             "opens, +1 closes); 'both' requires a close "
                             "evaluator. In optimize mode defaults to long "
                             "when a close-stack grid is swept so every "
                             "stack scores on the same entry universe.")
    parser.add_argument("--atr-method", dest="atr_method",
                        choices=["simple", "wilder"], default=None,
                        help="ATR smoothing for the injected standard-ATR "
                             "series (#1277). simple (default): frozen legacy "
                             "rolling mean with the >=100 integer rounding — "
                             "byte-identical to documented baselines. wilder: "
                             "published Wilder RMA, never rounded. Not allowed "
                             "alongside --config (the live config's atr_method "
                             "owns it). Regime classification stays pinned to "
                             "simple either way.")
    parser.add_argument("--intrabar-resolution", dest="intrabar_resolution",
                        choices=["ohlc_walk", "bar_close"],
                        default="ohlc_walk",
                        help="SL race resolution (#1271). ohlc_walk "
                             "(default): a bar whose range touches the armed "
                             "stop trigger exits ON that bar at the trigger "
                             "price (or at the open on a gap-through), "
                             "winning adverse-move-first over a same-bar TP. "
                             "bar_close: legacy pre-#1271 semantics (hit "
                             "detected on the close only, filled at the next "
                             "bar's open) for reproducing documented "
                             "baselines. Single mode only.")
    return parser


def _parse_close_strategy_arg(raw: str) -> dict:
    import json as _json
    s = raw.strip()
    if not s.startswith(("{", "[")):
        return {"name": s, "params": {}}
    try:
        ref = _json.loads(s)
    except _json.JSONDecodeError as exc:
        raise SystemExit(f"--close-strategy not valid JSON: {exc}\nGot: {raw}")
    if not isinstance(ref, dict):
        raise SystemExit(f"--close-strategy JSON must be an object, got {type(ref).__name__}")
    name = (ref.get("name") or "").strip()
    if not name:
        raise SystemExit(f"--close-strategy ref missing 'name': {raw}")
    return {"name": name, "params": dict(ref.get("params") or {})}


def _resolve_defaults_mode(args) -> str:
    if args.defaults:
        if args.defaults == "user" and not args.config:
            print("--defaults user requires --config (user_defaults lives in the config); "
                  "falling back to system defaults")
            return "system"
        return args.defaults
    return "user" if args.config else "system"


def main():
    args = _build_parser().parse_args()
    args.defaults = _resolve_defaults_mode(args)

    close_refs = None
    if args.close_strategies:
        close_refs = [_parse_close_strategy_arg(v) for v in args.close_strategies]

    args.regime_windows_spec = None
    if args.regime_windows_spec_json:
        try:
            args.regime_windows_spec = parse_regime_windows_spec_json(
                args.regime_windows_spec_json)
        except (ValueError, TypeError) as exc:
            print(f"--regime-windows-spec-json: {exc}")
            sys.exit(1)
        if args.mode != "single":
            print("--regime-windows-spec-json is only valid with --mode single")
            sys.exit(1)

    if not args.config:
        _validate_allowed_regimes_vocabulary(
            args.allowed_regimes, args.regime_windows_spec)

    open_params: Optional[dict] = None
    live_stop_kwargs: dict = {}
    if args.config:
        if args.mode != "single":
            print("--config is only valid with --mode single (loads one strategy by --strategy <id>)")
            sys.exit(1)
        live_kwargs = load_strategy_config(args.config, args.strategy,
                                           inject_user_defaults=(args.defaults == "user"))
        if close_refs:
            print("--close-strategy is not allowed alongside --config (refs come from the live config)")
            sys.exit(1)
        if args.direction:
            print("--direction is not allowed alongside --config (the live "
                  "config's `direction` field owns the entry transform); "
                  "edit the config or backtest the strategy by name")
            sys.exit(1)
        if args.allowed_regimes:
            print("--allowed-regimes is not allowed alongside --config (the "
                  "live config's `allowed_regimes` field owns the regime gate); "
                  "edit the config or backtest the strategy by name")
            sys.exit(1)
        if args.atr_method:
            print("--atr-method is not allowed alongside --config (the live "
                  "config's `atr_method` field owns the ATR smoothing); "
                  "edit the config or backtest the strategy by name")
            sys.exit(1)
        if args.regime_windows_spec is not None:
            print("--regime-windows-spec-json is not allowed alongside --config "
                  "(the live config's `regime.windows` owns the composite spec); "
                  "edit the config or backtest the strategy by name")
            sys.exit(1)
        close_refs = live_kwargs["close_strategies"]
        args.strategy = live_kwargs["open_strategy"]["name"]
        open_params = dict(live_kwargs["open_strategy"]["params"]) or None
        stop_keys = (
            "stop_loss_atr_mult",
            "stop_loss_pct",
            "stop_loss_margin_pct",
            "trailing_stop_atr_mult",
            "trailing_stop_pct",
            "stop_loss_atr_mult_regime",
            "trailing_stop_atr_mult_regime",
            "strategy_type",
            "direction",
            "invert_signal",
            "regime_directional_policy",
            "regime_directional_certified",
            "regime_directional_certified_states",
            "regime_gate_on_failure",
            "profile_allocation",
            "regime_timeframe",
            "regime_windows_spec",
            "risk_per_trade_pct",
            "allow_scale_in",
            "scale_in",
            "atr_method",
            "hurst_gate",
        )
        live_stop_kwargs = {k: live_kwargs[k] for k in stop_keys if k in live_kwargs}
        args.regime_enabled = live_kwargs.get("regime_enabled", args.regime_enabled)
        args.regime_period = live_kwargs.get("regime_period", args.regime_period)
        args.regime_adx_threshold = live_kwargs.get(
            "regime_adx_threshold", args.regime_adx_threshold,
        )
        args.allowed_regimes = live_kwargs.get(
            "allowed_regimes", args.allowed_regimes,
        )
        _validate_allowed_regimes_vocabulary(
            args.allowed_regimes, live_kwargs.get("regime_windows_spec"))

    if args.stop_loss_atr_mult is not None:
        live_stop_kwargs.setdefault("stop_loss_atr_mult", args.stop_loss_atr_mult)
    if args.trailing_stop_atr_mult is not None:
        live_stop_kwargs.setdefault("trailing_stop_atr_mult", args.trailing_stop_atr_mult)

    if args.regime_windows_spec is not None:
        live_stop_kwargs["regime_windows_spec"] = args.regime_windows_spec

    if args.direction == "both" and not close_refs \
            and not (args.sweep_close or args.close_stacks_json):
        print("--direction both requires a close evaluator (--close-strategy "
              "or a close-stack sweep); backtest each leg separately with "
              "--direction long / --direction short")
        sys.exit(1)
    if args.direction == "short" and args.mode == "optimize":
        print("--direction short is not supported in optimize mode (the "
              "walk-forward warmup seeder is long-only and would carry a "
              "phantom long into the short run); use --mode single "
              "--direction short or eval_windows.py --direction short")
        sys.exit(1)
    if args.direction:
        live_stop_kwargs["direction"] = args.direction

    if args.atr_method and args.mode == "optimize":
        print("--atr-method is not supported in optimize mode (the optimizer's "
              "engines run on the default simple ATR); use --mode single/"
              "compare/multi for wilder runs")
        sys.exit(1)
    if args.atr_method:
        live_stop_kwargs.setdefault("atr_method", args.atr_method)

    live_stop_kwargs["intrabar_resolution"] = args.intrabar_resolution
    if args.intrabar_resolution != "ohlc_walk" and args.mode == "optimize":
        print("--intrabar-resolution bar_close is not supported in optimize "
              "mode (the optimizer's engines run on the default ohlc_walk "
              "semantics); use --mode single, or eval_windows.py, for legacy-"
              "baseline reproduction")
        sys.exit(1)

    reg = load_registry(args.registry)

    if args.mode == "single":
        if args.strategy == "all":
            print("Specify a strategy for single mode: --strategy <name>")
            sys.exit(1)
        run_single_backtest(args.strategy, args.symbol, args.timeframe,
                            args.since, args.capital,
                            params=open_params,
                            registry=args.registry, platform=args.platform,
                            htf_filter=args.htf_filter,
                            close_strategies=close_refs,
                            regime_enabled=args.regime_enabled,
                            regime_period=args.regime_period,
                            regime_adx_threshold=args.regime_adx_threshold,
                            allowed_regimes=args.allowed_regimes,
                            **live_stop_kwargs)

    elif args.mode == "compare":
        strategies = None if args.strategy == "all" else [args.strategy]
        run_all_strategies(args.symbol, args.timeframe, args.since, args.capital,
                           strategies,
                           registry=args.registry, platform=args.platform,
                           htf_filter=args.htf_filter,
                           close_strategies=close_refs,
                           regime_enabled=args.regime_enabled,
                           regime_period=args.regime_period,
                           regime_adx_threshold=args.regime_adx_threshold,
                           allowed_regimes=args.allowed_regimes,
                           direction=args.direction,
                           intrabar_resolution=args.intrabar_resolution,
                           atr_method=args.atr_method or "simple")

    elif args.mode == "multi":
        strategies = None if args.strategy == "all" else [args.strategy]
        symbols = args.symbols or DEFAULT_SYMBOLS
        run_multi_asset(strategies, symbols, args.timeframe, args.since,
                        args.capital,
                        registry=args.registry, platform=args.platform,
                        htf_filter=args.htf_filter,
                        close_strategies=close_refs,
                        regime_enabled=args.regime_enabled,
                        regime_period=args.regime_period,
                        regime_adx_threshold=args.regime_adx_threshold,
                        allowed_regimes=args.allowed_regimes,
                        direction=args.direction,
                        intrabar_resolution=args.intrabar_resolution,
                        atr_method=args.atr_method or "simple")

    elif args.mode == "optimize":
        close_stack_grid = None
        if args.close_stacks_json or args.sweep_close:
            if close_refs or args.stop_loss_atr_mult is not None \
                    or args.trailing_stop_atr_mult is not None:
                print("--sweep-close/--close-stacks-json is mutually exclusive "
                      "with --close-strategy/--stop-loss-atr-mult/"
                      "--trailing-stop-atr-mult (the grid owns the close stack)")
                sys.exit(1)
            if args.close_stacks_json:
                import json as _json
                with open(args.close_stacks_json) as fh:
                    specs = _json.load(fh)
                if not isinstance(specs, list):
                    print(f"{args.close_stacks_json}: expected a JSON list of "
                          f"close-stack specs")
                    sys.exit(1)
            else:
                specs = DEFAULT_CLOSE_STACK_SPECS
            close_stack_grid = generate_close_stack_grid(specs)
            if args.direction == "both" and any(
                    not s.get("close_strategies") for s in close_stack_grid):
                print("--direction both requires a close evaluator on every "
                      "swept close stack, but the grid contains no-close "
                      "baseline stacks (the default --sweep-close grid always "
                      "does); supply --close-stacks-json with close-evaluator "
                      "stacks only, or backtest each leg separately")
                sys.exit(1)

        if args.strategy == "all":
            for strat in reg.list_strategies():
                run_walk_forward(strat, args.symbol, args.timeframe,
                                 args.since, args.splits, args.capital,
                                 registry=args.registry, platform=args.platform,
                                 regime_enabled=args.regime_enabled,
                                 regime_period=args.regime_period,
                                 regime_adx_threshold=args.regime_adx_threshold,
                                 allowed_regimes=args.allowed_regimes,
                                 stop_loss_atr_mult=args.stop_loss_atr_mult,
                                 trailing_stop_atr_mult=args.trailing_stop_atr_mult,
                                 close_strategies=close_refs,
                                 close_stack_grid=close_stack_grid,
                                 optimize_metric=args.optimize_metric,
                                 direction=args.direction)
        else:
            run_walk_forward(args.strategy, args.symbol, args.timeframe,
                             args.since, args.splits, args.capital,
                             registry=args.registry, platform=args.platform,
                             regime_enabled=args.regime_enabled,
                             regime_period=args.regime_period,
                             regime_adx_threshold=args.regime_adx_threshold,
                             allowed_regimes=args.allowed_regimes,
                             stop_loss_atr_mult=args.stop_loss_atr_mult,
                             trailing_stop_atr_mult=args.trailing_stop_atr_mult,
                             close_strategies=close_refs,
                             close_stack_grid=close_stack_grid,
                             optimize_metric=args.optimize_metric,
                             direction=args.direction)


if __name__ == "__main__":
    main()
