
import sys
import os
import copy
import json
import math
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from types import MappingProxyType
from typing import Any, Callable, Optional, Tuple

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

import numpy as np
import pandas as pd

from storage import store_backtest_result
from atr import standard_atr

_close_registry = None

_ensure_regime_fn = None
_regime_allows_entry_fn = None

_post_tp_sl_module = None
_trailing_ratchet_module = None


def _load_regime():
    global _ensure_regime_fn, _regime_allows_entry_fn
    if _ensure_regime_fn is None:
        from regime import ensure_regime_columns as _ensure_regime_columns
        from regime import regime_label_allows_entry as _allows_entry
        _ensure_regime_fn = _ensure_regime_columns
        _regime_allows_entry_fn = _allows_entry
    return _ensure_regime_fn


def _regime_allows_entry(allowed, bar_regime: str, on_failure: str = "open") -> bool:
    if _regime_allows_entry_fn is None:
        _load_regime()
    if _regime_allows_entry_fn is None:
        if not allowed:
            return True
        if not bar_regime:
            return on_failure != "closed"
        return bar_regime in allowed
    return _regime_allows_entry_fn(allowed, bar_regime, on_failure)


def _regime_primary_labels(spec: Optional[dict]) -> Optional[tuple]:
    if not spec:
        return None
    from regime import (
        valid_labels_for_classifier,
        REGIME_PRIMARY_WINDOW_KEY,
        CLASSIFIER_ADX,
    )
    primary_key = (
        REGIME_PRIMARY_WINDOW_KEY
        if REGIME_PRIMARY_WINDOW_KEY in spec
        else sorted(spec.keys())[0]
    )
    classifier = str(spec[primary_key].get("classifier") or CLASSIFIER_ADX).strip().lower()
    if classifier == CLASSIFIER_ADX:
        return None
    return tuple(sorted(valid_labels_for_classifier(classifier)))


def _normalize_regime_directional_policy(policy: Optional[dict]) -> Optional[dict]:
    if not policy:
        return None
    if not isinstance(policy, dict):
        raise ValueError("regime_directional_policy must be an object")
    raw = policy.get("trend_regime")
    if not isinstance(raw, dict):
        raise ValueError(
            "regime_directional_policy must contain a trend_regime object"
        )
    parsed: dict[str, dict[str, object]] = {}
    for label, entry in raw.items():
        if not isinstance(entry, dict):
            raise ValueError(
                f"regime_directional_policy.{label}: must be an object"
            )
        direction = entry.get("direction")
        if not isinstance(direction, str):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be a string"
            )
        if direction not in ("long", "short", "both"):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be "
                f"'long', 'short', or 'both'"
            )
        invert = entry.get("invert_signal", False)
        if not isinstance(invert, bool):
            raise ValueError(
                f"regime_directional_policy.{label}.invert_signal: "
                f"must be a boolean"
            )
        for key in entry:
            if key not in ("direction", "invert_signal"):
                raise ValueError(
                    f"regime_directional_policy.{label}: unknown key {key!r}"
                )
        parsed[str(label).strip()] = {
            "direction": direction,
            "invert_signal": invert,
        }
    return parsed or None


def _gate_directional_policy_by_states(
    policy: Optional[dict], cert_states: Optional[dict],
) -> Optional[dict]:
    if not policy:
        return policy
    from regime import RANGING_DIRECTIONAL_BARE, RANGING_DIRECTIONAL_SUBS
    bare_entry = policy.get(RANGING_DIRECTIONAL_BARE)
    if isinstance(bare_entry, dict):
        expanded = dict(policy)
        for sub in sorted(RANGING_DIRECTIONAL_SUBS):
            expanded.setdefault(sub, bare_entry)
        policy = expanded
    if cert_states is None:
        return policy
    gated = {}
    for label, entry in policy.items():
        cert_dir = str(cert_states.get(label) or "").strip().lower()
        if not cert_dir:
            continue
        direction = str((entry or {}).get("direction") or "").strip().lower()
        if direction != "both" and direction != cert_dir:
            continue
        gated[label] = entry
    return gated


def _resolve_regime_directional_entry(
    policy: Optional[dict],
    current_regime: str,
    position_regime: str = "",
    position_qty: float = 0.0,
) -> Optional[dict]:
    if not policy:
        return None
    regime = str(current_regime or "").strip()
    if position_qty > 0 and str(position_regime or "").strip():
        regime = str(position_regime or "").strip()
    entry = policy.get(regime)
    return dict(entry) if isinstance(entry, dict) else None


def _apply_direction_invert_value(
    signal: int,
    uses_open_close: bool,
    direction: Optional[str],
    invert_signal: bool,
) -> int:
    sig = int(signal)
    if invert_signal and sig != 0:
        sig = -sig
    d = (direction or "").strip().lower()
    if uses_open_close and d in ("long", "short"):
        if d == "long" and sig < 0:
            return 0
        if d == "short" and sig > 0:
            return 0
    return sig


def _signal_from_open_action(action: str) -> int:
    action = str(action or "").strip().lower()
    if action == "long":
        return 1
    if action == "short":
        return -1
    return 0


def _load_post_tp_sl():
    global _post_tp_sl_module
    if _post_tp_sl_module is not None:
        return _post_tp_sl_module
    import importlib.util
    name = "_go_trader_post_tp_sl"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "post_tp_sl.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _post_tp_sl_module = mod
    return mod


def _load_trailing_ratchet():
    global _trailing_ratchet_module
    if _trailing_ratchet_module is not None:
        return _trailing_ratchet_module
    _ensure_close_strategies_path()
    import importlib.util
    name = "_go_trader_trailing_ratchet"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "trailing_tp_ratchet.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _trailing_ratchet_module = mod
    return mod


def _load_close_registry():
    global _close_registry
    if _close_registry is None:
        from close_registry_loader import evaluate as _evaluate, list_strategies as _list
        _close_registry = (_evaluate, _list)
    return _close_registry


_CLOSE_STRATEGIES_DIR = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "shared_strategies", "close")
)


def _ensure_close_strategies_path() -> None:
    if _CLOSE_STRATEGIES_DIR not in sys.path:
        sys.path.insert(0, _CLOSE_STRATEGIES_DIR)


def _rewrite_deprecated_close_ref(name: str, params: dict) -> tuple[str, dict]:
    if name != "tp_at_pct":
        return name, dict(params or {})
    pct = 0.03
    if params and params.get("pct") is not None:
        try:
            pct = max(float(params.get("pct", 0.03)), 0.0)
        except (TypeError, ValueError):
            pct = 0.03
    out = {
        "tp_tiers": [{"profit_pct": pct, "close_fraction": 1.0}],
    }
    if params and "sl_after" in params:
        out["sl_after"] = params["sl_after"]
    return "tiered_tp_pct", out


COMPARISON_MODE_STRICT = "strict"
COMPARISON_MODE_APPROXIMATE = "approximate"
COMPARISON_MODES = (COMPARISON_MODE_STRICT, COMPARISON_MODE_APPROXIMATE)
CLOSE_VALIDATION_SCHEMA_VERSION = 1
CLOSE_VALIDATION_FIELDS = (
    "schema_version",
    "mode",
    "close_eligibility",
    "approximations",
    "incomplete_parity",
    "parity_status",
    "refusals",
)
CAPABILITY_CONSUMERS = ("engine", "decision_parity", "entry_replay")
CAPABILITY_PHASES = ("construction", "preflight", "runtime")
_STATIC_CAPABILITY_PHASES = ("construction", "preflight")
CAPABILITY_INPUT_STATUSES = ("verified", "missing", "unverified", "invalid")

CLOSE_CAPABILITY_REFUSAL_CODES = (
    "INVALID_COMPARISON_MODE",
    "INVALID_CLOSE_REFERENCE",
    "UNKNOWN_CLOSE_STRATEGY",
    "UNCLASSIFIED_CLOSE_CAPABILITY",
    "LIVE_ONLY_CLOSE",
    "UNSUPPORTED_LIVE_CONTEXT",
    "UNSUPPORTED_REPLAY_CAPABILITY",
    "INVALID_CAPABILITY_CONTEXT",
    "MISSING_STOP_INPUT",
    "UNSUPPORTED_STOP_OWNER",
    "UNVERIFIED_MARGIN_LEVERAGE",
)

STOP_SCALAR_FIELD_KEYS = (
    "stop_loss_atr_mult",
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_atr_mult",
    "trailing_stop_pct",
)
STOP_REGIME_FIELD_KEYS = ("stop_loss_atr_mult_regime", "trailing_stop_atr_mult_regime")
STOP_FIELD_KEYS = STOP_SCALAR_FIELD_KEYS + STOP_REGIME_FIELD_KEYS
STOP_GEOMETRY_INPUT_KEYS = ("leverage", "max_drawdown_pct", "trailing_stop_min_move_pct")
STOP_PERCENT_FIELD_KEYS = (
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_pct",
    "max_drawdown_pct",
    "trailing_stop_min_move_pct",
)
STOP_UNITS_DIRECT = "direct_fraction"
STOP_UNITS_LIVE_CONFIG = "live_config"
STOP_UNITS_PREVIEW = "preview_payload"
STOP_UNIT_SOURCES = (STOP_UNITS_DIRECT, STOP_UNITS_LIVE_CONFIG, STOP_UNITS_PREVIEW)
MAX_AUTO_STOP_LOSS_FRACTION = 0.5
DEFAULT_TRAILING_STOP_MIN_MOVE_FRACTION = 0.005
STOP_OWNERS_NEEDING_ATR = (
    "trailing_atr", "trailing_atr_regime", "fixed_atr", "fixed_atr_regime", "unified_regime",
)
STOP_OWNERS_NEEDING_LABEL = ("trailing_atr_regime", "fixed_atr_regime", "unified_regime")
CLOSE_CAPABILITY_APPROXIMATION_CODES = ("RESEARCH_ONLY_CLOSE_CONTEXT",)

CLOSE_LIVE_SUPPORTED = "supported"
CLOSE_LIVE_RESEARCH_CONTEXT = "research_context"
CLOSE_LIVE_ONLY = "live_only"


@dataclass(frozen=True)
class CloseCapability:
    live: str
    research_inputs: Tuple[str, ...] = ()
    replayable: bool = False


CLOSE_CAPABILITIES = MappingProxyType({
    "tiered_tp_pct": CloseCapability(CLOSE_LIVE_SUPPORTED),
    "tiered_tp_atr": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "tiered_tp_atr_live": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "tiered_tp_atr_regime": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "tiered_tp_atr_live_regime": CloseCapability(CLOSE_LIVE_SUPPORTED),
    "tiered_tp_atr_live_regime_dynamic": CloseCapability(CLOSE_LIVE_ONLY),
    "trailing_tp_ratchet": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "trailing_tp_ratchet_regime": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "time_stop": CloseCapability(
        CLOSE_LIVE_RESEARCH_CONTEXT, research_inputs=("bars_held",), replayable=True),
    "atr_stop": CloseCapability(CLOSE_LIVE_SUPPORTED, replayable=True),
    "zscore_target": CloseCapability(
        CLOSE_LIVE_RESEARCH_CONTEXT, research_inputs=("zscore",), replayable=True),
    "avwap_stop": CloseCapability(CLOSE_LIVE_SUPPORTED),
})


def _is_json_value(value) -> bool:
    if value is None or isinstance(value, (bool, str)):
        return True
    if isinstance(value, int):
        return True
    if isinstance(value, float):
        return math.isfinite(value)
    if isinstance(value, Mapping):
        return all(isinstance(k, str) and _is_json_value(v) for k, v in value.items())
    if isinstance(value, (list, tuple)):
        return all(_is_json_value(v) for v in value)
    return False


def _freeze_json(value):
    if isinstance(value, Mapping):
        return MappingProxyType({k: _freeze_json(v) for k, v in value.items()})
    if isinstance(value, (list, tuple)):
        return tuple(_freeze_json(v) for v in value)
    return value


def _thaw_json(value):
    if isinstance(value, Mapping):
        return {k: _thaw_json(v) for k, v in value.items()}
    if isinstance(value, tuple):
        return [_thaw_json(v) for v in value]
    return value


@dataclass(frozen=True)
class CapabilityContext:
    raw_fields: Mapping = field(default_factory=dict)
    resolved_stop_owner: Optional[Mapping] = None
    input_evidence: Mapping = field(default_factory=dict)
    errors: Tuple[str, ...] = field(default=(), init=False, compare=False)

    def __post_init__(self):
        errors = []
        raw = self.raw_fields
        if not isinstance(raw, Mapping):
            errors.append("raw_fields must be a mapping")
            raw = {}
        for name, entry in raw.items():
            if not isinstance(name, str) or not name:
                errors.append("raw_fields keys must be non-empty strings")
                continue
            if (not isinstance(entry, Mapping) or set(entry) != {"present", "value"}
                    or not isinstance(entry.get("present"), bool)
                    or not _is_json_value(entry.get("value"))):
                errors.append(
                    f"raw_fields[{name!r}] must be {{present: bool, value: JSON value}}")
            elif not entry["present"] and entry["value"] is not None:
                errors.append(f"raw_fields[{name!r}] is absent but carries a value")
        owner = self.resolved_stop_owner
        if owner is not None:
            if (not isinstance(owner, Mapping) or set(owner) != {"name", "parameters"}
                    or not isinstance(owner.get("name"), str) or not owner.get("name")
                    or not isinstance(owner.get("parameters"), Mapping)
                    or not _is_json_value(owner.get("parameters"))):
                errors.append(
                    "resolved_stop_owner must be None or {name: str, parameters: mapping}")
        evidence = self.input_evidence
        if not isinstance(evidence, Mapping):
            errors.append("input_evidence must be a mapping")
            evidence = {}
        for name, entry in evidence.items():
            if not isinstance(name, str) or not name:
                errors.append("input_evidence keys must be non-empty strings")
                continue
            if (not isinstance(entry, Mapping)
                    or set(entry) != {"status", "source", "value"}
                    or entry.get("status") not in CAPABILITY_INPUT_STATUSES
                    or not (entry.get("source") is None or isinstance(entry.get("source"), str))
                    or not _is_json_value(entry.get("value"))):
                errors.append(
                    f"input_evidence[{name!r}] must be {{status: one of "
                    f"{list(CAPABILITY_INPUT_STATUSES)}, source: str or null, value: JSON value}}")
        object.__setattr__(self, "errors", tuple(errors))
        if not errors:
            object.__setattr__(self, "raw_fields", _freeze_json(raw))
            object.__setattr__(self, "resolved_stop_owner",
                               None if owner is None else _freeze_json(owner))
            object.__setattr__(self, "input_evidence", _freeze_json(evidence))

    def to_dict(self) -> dict:
        return {
            "raw_fields": _thaw_json(self.raw_fields),
            "resolved_stop_owner": _thaw_json(self.resolved_stop_owner),
            "input_evidence": _thaw_json(self.input_evidence),
        }

    def __reduce__(self):
        return (CapabilityContext, (_thaw_json(self.raw_fields),
                                    _thaw_json(self.resolved_stop_owner),
                                    _thaw_json(self.input_evidence)))


@dataclass(frozen=True)
class CapabilityRecord:
    kind: str
    reason_code: str
    feature: str
    close_ref_index: Optional[int] = None
    required_inputs: Tuple[str, ...] = ()
    details: Mapping = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "reason_code": self.reason_code,
            "feature": self.feature,
            "close_ref_index": self.close_ref_index,
            "required_inputs": sorted(self.required_inputs),
            "details": _thaw_json(self.details),
        }


def capability_refusal(reason_code: str, feature: str, *,
                       close_ref_index: Optional[int] = None,
                       required_inputs=(), details: Optional[dict] = None) -> CapabilityRecord:
    return CapabilityRecord("refusal", reason_code, feature, close_ref_index,
                            tuple(sorted(required_inputs)), _freeze_json(dict(details or {})))


def capability_approximation(reason_code: str, feature: str, *,
                             close_ref_index: Optional[int] = None,
                             required_inputs=(), details: Optional[dict] = None) -> CapabilityRecord:
    return CapabilityRecord("approximation", reason_code, feature, close_ref_index,
                            tuple(sorted(required_inputs)), _freeze_json(dict(details or {})))


@dataclass(frozen=True)
class CloseCapabilityRequest:
    close_refs: Tuple[Mapping, ...]
    mode: str
    platform: Optional[str]
    strategy_type: Optional[str]
    consumer: str
    phase: str
    capability_context: Optional[CapabilityContext]
    registered_closes: frozenset


@dataclass(frozen=True)
class CloseCapabilityCheck:
    check_id: str
    phases: Tuple[str, ...]
    reason_codes: Tuple[str, ...]
    callback: Callable[[CloseCapabilityRequest], Sequence[CapabilityRecord]]


def _check_registry_membership(request: CloseCapabilityRequest) -> list:
    out = []
    for idx, ref in enumerate(request.close_refs):
        if ref["name"] not in request.registered_closes:
            available = sorted(request.registered_closes)
            out.append(capability_refusal(
                "UNKNOWN_CLOSE_STRATEGY", ref["name"], close_ref_index=idx,
                details={"message": f"Unknown close strategy: {ref['name']}. "
                                    f"Available: {available}",
                         "available": available}))
    return out


def _check_capability_declared(request: CloseCapabilityRequest) -> list:
    out = []
    for idx, ref in enumerate(request.close_refs):
        name = ref["name"]
        if name in request.registered_closes and name not in CLOSE_CAPABILITIES:
            out.append(capability_refusal(
                "UNCLASSIFIED_CLOSE_CAPABILITY", name, close_ref_index=idx,
                details={"message": f"close strategy {name!r} is registered but has no "
                                    "central capability declaration in "
                                    "backtester.CLOSE_CAPABILITIES"}))
    return out


def _check_live_only_close(request: CloseCapabilityRequest) -> list:
    out = []
    for idx, ref in enumerate(request.close_refs):
        cap = CLOSE_CAPABILITIES.get(ref["name"])
        if ref["name"] in request.registered_closes and cap is not None \
                and cap.live == CLOSE_LIVE_ONLY:
            out.append(capability_refusal(
                "LIVE_ONLY_CLOSE", ref["name"], close_ref_index=idx,
                details={"message": f"{ref['name']} is HL-live-only: the common "
                                    "backtest engine has no parity path for it in "
                                    "any comparison mode",
                         "mode": request.mode}))
    return out


def _check_live_close_context(request: CloseCapabilityRequest) -> list:
    out = []
    for idx, ref in enumerate(request.close_refs):
        cap = CLOSE_CAPABILITIES.get(ref["name"])
        if ref["name"] not in request.registered_closes or cap is None \
                or cap.live != CLOSE_LIVE_RESEARCH_CONTEXT:
            continue
        details = {"mode": request.mode, "platform": request.platform,
                   "strategy_type": request.strategy_type}
        if request.mode == COMPARISON_MODE_STRICT:
            details["message"] = (
                f"{ref['name']} needs {', '.join(cap.research_inputs)}, which no live "
                "close context supplies on any platform; strict comparison refuses it. "
                "Pass comparison_mode='approximate' (--comparison-mode approximate) "
                "for research that accepts incomplete parity")
            out.append(capability_refusal(
                "UNSUPPORTED_LIVE_CONTEXT", ref["name"], close_ref_index=idx,
                required_inputs=cap.research_inputs, details=details))
        else:
            details["message"] = (
                f"{ref['name']} reads simulator-only {', '.join(cap.research_inputs)}; "
                "the result is research evidence with incomplete parity")
            out.append(capability_approximation(
                "RESEARCH_ONLY_CLOSE_CONTEXT", ref["name"], close_ref_index=idx,
                required_inputs=cap.research_inputs, details=details))
    return out


def _check_entry_replay(request: CloseCapabilityRequest) -> list:
    if request.consumer != "entry_replay":
        return []
    out = []
    for idx, ref in enumerate(request.close_refs):
        cap = CLOSE_CAPABILITIES.get(ref["name"])
        if ref["name"] in request.registered_closes and cap is not None \
                and not cap.replayable:
            out.append(capability_refusal(
                "UNSUPPORTED_REPLAY_CAPABILITY", ref["name"], close_ref_index=idx,
                details={"message": f"{ref['name']} has no per-entry rule the "
                                    "entry-locked replay can isolate"}))
    return out


def uses_hyperliquid_stop_geometry(platform, strategy_type) -> bool:
    return (str(platform or "").strip().lower() == "hyperliquid"
            and str(strategy_type or "").strip().lower() == "perps")


def _finite_number(value) -> Optional[float]:
    if value is None or isinstance(value, bool):
        return None
    try:
        out = float(value)
    except (TypeError, ValueError):
        return None
    return out if math.isfinite(out) else None


def _positive(value) -> bool:
    v = _finite_number(value)
    return v is not None and v > 0


def _unified_close_params(close_refs) -> Optional[dict]:
    for ref in close_refs or ():
        name = str(ref.get("name") or "").strip().lower()
        if name not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            continue
        params = _thaw_json(ref.get("params") or {})
        if isinstance(params, dict) and "trend_regime" in params:
            return params
        return None
    return None


def _parse_stop_regime_blocks(fields: Mapping, labels, live: bool) -> Tuple[dict, list]:
    _ensure_close_strategies_path()
    from regime_atr import SURFACE_STOP_LOSS, SURFACE_TRAILING, parse_regime_atr_block
    blocks: dict = {}
    errs: list = []
    for key, surface in (("stop_loss_atr_mult_regime", SURFACE_STOP_LOSS),
                         ("trailing_stop_atr_mult_regime", SURFACE_TRAILING)):
        raw = fields.get(key)
        if raw is None or (not live and not raw):
            blocks[key] = None
            continue
        blk, block_errs = parse_regime_atr_block(
            _thaw_json(raw), key, surface, labels=tuple(labels) if labels else None)
        errs.extend(block_errs)
        blocks[key] = blk
    return blocks, errs


def _regime_block_active(block) -> bool:
    return block is not None and not block.is_zero()


def resolve_static_stop_owner(fields: Mapping, blocks: Mapping, unified: bool,
                              hyperliquid: bool) -> str:
    if not hyperliquid:
        return "legacy"
    tsp = _finite_number(fields.get("trailing_stop_pct"))
    tsp_present = fields.get("trailing_stop_pct") is not None
    sl_regime = _regime_block_active(blocks.get("stop_loss_atr_mult_regime"))
    trail_regime = _regime_block_active(blocks.get("trailing_stop_atr_mult_regime"))
    if tsp_present:
        if tsp is not None and tsp > 0:
            return "trailing_pct"
    elif _positive(fields.get("trailing_stop_atr_mult")):
        return "trailing_atr"
    elif trail_regime:
        return "trailing_atr_regime"
    if unified:
        return "unified_regime"
    if _positive(fields.get("stop_loss_atr_mult")):
        return "fixed_atr"
    if sl_regime:
        return "fixed_atr_regime"
    if _positive(fields.get("trailing_stop_atr_mult")) or trail_regime or tsp_present:
        return "none"
    if fields.get("stop_loss_pct") is not None:
        return "fixed_pct" if _positive(fields.get("stop_loss_pct")) else "none"
    if fields.get("stop_loss_margin_pct") is not None:
        return "margin_pct" if _positive(fields.get("stop_loss_margin_pct")) else "none"
    if _positive(fields.get("max_drawdown_pct")):
        return "drawdown_fallback"
    return "none"


def resolve_risk_stop_owner(fields: Mapping, unified: bool,
                            live: bool) -> Tuple[Optional[str], Optional[str], str]:
    if unified:
        return None, None, (
            "risk_per_trade_pct cannot size from the unified per-regime close block — "
            "its SL resolves per-regime after open, so the stop distance is unknowable "
            "at sizing time (#1268; live rejects this at config load)")
    if _positive(fields.get("trailing_stop_atr_mult")):
        return "atr", "trailing_stop_atr_mult", ""
    if _positive(fields.get("stop_loss_atr_mult")):
        return "atr", "stop_loss_atr_mult", ""
    if fields.get("stop_loss_atr_mult_regime") or fields.get("trailing_stop_atr_mult_regime"):
        return None, None, (
            "risk_per_trade_pct cannot size from a regime-resolved stop owner "
            "(stop_loss_atr_mult_regime / trailing_stop_atr_mult_regime) — the SL "
            "resolves from the regime stamped after open (#1268; live rejects this at "
            "config load)")
    for key in ("trailing_stop_pct", "stop_loss_pct"):
        if live and fields.get(key) is not None:
            if _positive(fields.get(key)):
                return "pct", key, ""
            return None, None, (
                f"risk_per_trade_pct requires a stop owner whose distance is resolvable "
                f"at sizing time — {key}=0 explicitly disables the stop")
        if not live and _positive(fields.get(key)):
            return "pct", key, ""
    if _positive(fields.get("stop_loss_margin_pct")):
        return None, None, (
            "risk_per_trade_pct cannot size from a stop_loss_margin_pct-only stop in "
            "backtests — margin-percent risk sizing has no proven parity path. Use "
            "stop_loss_atr_mult, trailing_stop_atr_mult, stop_loss_pct, or "
            "trailing_stop_pct.")
    return None, None, (
        "risk_per_trade_pct requires an explicit stop owner (stop_loss_atr_mult, "
        "trailing_stop_atr_mult, stop_loss_pct, or trailing_stop_pct) to derive the "
        "stop distance from (#1268); no stop owner is configured, and the "
        "max_drawdown_pct fallback is an account backstop, not a per-trade stop")


def _stop_context_parameters(request: CloseCapabilityRequest) -> Optional[Mapping]:
    ctx = request.capability_context
    if not isinstance(ctx, CapabilityContext) or ctx.resolved_stop_owner is None:
        return None
    params = ctx.resolved_stop_owner.get("parameters")
    if not isinstance(params, Mapping) or "fields" not in params:
        return None
    return params


def _stop_evidence(request: CloseCapabilityRequest, name: str) -> Optional[Mapping]:
    ctx = request.capability_context
    if not isinstance(ctx, CapabilityContext):
        return None
    entry = ctx.input_evidence.get(name)
    return entry if isinstance(entry, Mapping) else None


def _stop_refusal(code: str, feature: str, message: str, *, required_inputs=(),
                  details: Optional[dict] = None) -> CapabilityRecord:
    payload = {"message": message}
    payload.update(details or {})
    return capability_refusal(code, feature, required_inputs=required_inputs, details=payload)


def _check_stop_owner_support(request: CloseCapabilityRequest) -> list:
    params = _stop_context_parameters(request)
    if params is None:
        return []
    fields = params["fields"]
    live = params.get("admission") == "live"
    owner = request.capability_context.resolved_stop_owner["name"]
    labels = params.get("regime_labels")
    blocks, _ = _parse_stop_regime_blocks(fields, labels, live)
    refs = [{"name": r["name"], "params": _thaw_json(r["params"])} for r in request.close_refs]
    unified = _unified_close_params(refs) is not None
    out: list = []

    def refuse(feature: str, message: str, **extra) -> None:
        out.append(_stop_refusal("UNSUPPORTED_STOP_OWNER", feature, message, **extra))

    declared = params.get("declared_owner")
    if declared is not None and declared != owner:
        refuse("resolved_stop_owner",
               f"caller-declared stop owner {declared!r} differs from the engine-resolved "
               f"owner {owner!r}")

    ratchet_ref = next((r for r in refs if str(r["name"]).strip().lower()
                        in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime")), None)
    if ratchet_ref is not None:
        if str(ratchet_ref["name"]).strip().lower() == "trailing_tp_ratchet_regime":
            if fields.get("trailing_stop_atr_mult_regime") is None:
                refuse("trailing_tp_ratchet_regime",
                       "trailing_tp_ratchet_regime requires trailing_stop_atr_mult_regime")
        elif not _positive(fields.get("trailing_stop_atr_mult")):
            refuse("trailing_tp_ratchet", "trailing_tp_ratchet requires trailing_stop_atr_mult > 0")
        if _positive(fields.get("trailing_stop_pct")):
            refuse(str(ratchet_ref["name"]),
                   "trailing_tp_ratchet* cannot combine with trailing_stop_pct")

    def set_field(key: str) -> bool:
        return fields.get(key) is not None if live else _positive(fields.get(key))

    if unified:
        for key in STOP_SCALAR_FIELD_KEYS:
            if set_field(key):
                refuse(key, f"{key} is not allowed alongside a unified per-regime close — "
                            "the close owns the SL via per-regime stop_loss_atr")
        regime_conflict = (
            any(_regime_block_active(blocks.get(k)) for k in STOP_REGIME_FIELD_KEYS)
            if live else any(fields.get(k) for k in STOP_REGIME_FIELD_KEYS))
        if regime_conflict:
            refuse("stop_loss_atr_mult_regime",
                   "stop_loss_atr_mult_regime/trailing_stop_atr_mult_regime are not allowed "
                   "alongside a unified per-regime close — the close owns the SL via "
                   "per-regime stop_loss_atr")

    if _regime_block_active(blocks.get("stop_loss_atr_mult_regime")):
        for key in ("stop_loss_atr_mult", "stop_loss_pct", "stop_loss_margin_pct",
                    "trailing_stop_pct", "trailing_stop_atr_mult"):
            if set_field(key):
                refuse("stop_loss_atr_mult_regime",
                       f"stop_loss_atr_mult_regime is mutually exclusive with {key}")
        trail_blk = blocks.get("trailing_stop_atr_mult_regime")
        if (trail_blk is not None) if live else _regime_block_active(trail_blk):
            refuse("stop_loss_atr_mult_regime",
                   "stop_loss_atr_mult_regime is mutually exclusive with "
                   "trailing_stop_atr_mult_regime")
    if _regime_block_active(blocks.get("trailing_stop_atr_mult_regime")):
        for key in ("trailing_stop_atr_mult", "trailing_stop_pct", "stop_loss_pct",
                    "stop_loss_margin_pct", "stop_loss_atr_mult"):
            if set_field(key):
                refuse("trailing_stop_atr_mult_regime",
                       f"trailing_stop_atr_mult_regime is mutually exclusive with {key}")

    sl_mod = _load_post_tp_sl()
    rules, _ = sl_mod.parse_strategy_tp_sl_after_rules(refs, labels=labels)
    if rules.has_any():
        has_atr_sl = (_positive(fields.get("stop_loss_atr_mult"))
                      or _regime_block_active(blocks.get("stop_loss_atr_mult_regime")))
        if _positive(fields.get("stop_loss_margin_pct")) and not (
                has_atr_sl or _positive(fields.get("stop_loss_pct"))):
            refuse("stop_loss_margin_pct",
                   "Invalid sl_after configuration: stop_loss_margin_pct cannot be the sole "
                   "fixed SL in backtests — the post-TP margin-stop bump has no proven "
                   "parity path, so it would diverge from live. Use stop_loss_atr_mult or "
                   "stop_loss_pct.")

    if params.get("risk_per_trade_pct") is not None:
        _, _, reason = resolve_risk_stop_owner(fields, unified, live)
        if reason:
            refuse("risk_per_trade_pct", reason)
    return out


def _check_stop_inputs(request: CloseCapabilityRequest) -> list:
    params = _stop_context_parameters(request)
    if params is None:
        return []
    fields = params["fields"]
    owner = request.capability_context.resolved_stop_owner["name"]
    out: list = []
    if request.phase in _STATIC_CAPABILITY_PHASES:
        _, errs = _parse_stop_regime_blocks(
            fields, params.get("regime_labels"), params.get("admission") == "live")
        if errs:
            out.append(_stop_refusal(
                "MISSING_STOP_INPUT", "regime_atr_stop",
                "Invalid regime ATR stop configuration: " + "; ".join(errs),
                required_inputs=("regime_atr_block",)))
        window = _stop_evidence(request, "atr_regime_window")
        if owner in STOP_OWNERS_NEEDING_LABEL and window is not None \
                and window.get("status") != "verified":
            out.append(_stop_refusal(
                "MISSING_STOP_INPUT", owner,
                f"{owner} resolves from the regime_atr_window {window.get('value')!r} label, "
                "which the backtester does not compute (it stamps only the primary regime "
                "window); refusing instead of resolving the stop from the wrong label",
                required_inputs=("atr_regime_label",),
                details={"window": window.get("value")}))
        return out
    for name, needs in (("entry_atr", owner in STOP_OWNERS_NEEDING_ATR),
                        ("risk_anchor", owner not in ("none", "legacy")),
                        ("atr_regime_label", owner in STOP_OWNERS_NEEDING_LABEL),
                        ("sl_after_entry_atr", True)):
        evidence = _stop_evidence(request, name)
        if not needs or evidence is None or evidence.get("status") == "verified":
            continue
        out.append(_stop_refusal(
            "MISSING_STOP_INPUT", owner,
            f"active stop owner {owner!r} has {evidence.get('status')} {name} "
            f"({evidence.get('source')}: {evidence.get('value')!r}) at "
            f"{params.get('event_date')}; live would leave this position without its "
            "protective stop, so the run is refused instead of simulated unprotected",
            required_inputs=(name,),
            details={"input": name, "status": evidence.get("status"),
                     "event_date": params.get("event_date")}))
    return out


def _check_margin_leverage(request: CloseCapabilityRequest) -> list:
    params = _stop_context_parameters(request)
    if params is None or request.capability_context.resolved_stop_owner["name"] != "margin_pct":
        return []
    evidence = _stop_evidence(request, "leverage")
    status = evidence.get("status") if evidence is not None else "missing"
    if status == "verified" and _positive(params["fields"].get("leverage")):
        return []
    return [_stop_refusal(
        "UNVERIFIED_MARGIN_LEVERAGE", "stop_loss_margin_pct",
        "stop_loss_margin_pct owns the stop, but its price distance needs a verified "
        f"leverage input (leverage evidence is {status}"
        + (f", source {evidence.get('source')}" if evidence is not None else "")
        + "); a defaulted or missing leverage cannot prove the margin-stop geometry",
        required_inputs=("leverage",),
        details={"leverage_status": status,
                 "leverage": params["fields"].get("leverage")})]


CLOSE_CAPABILITY_CHECKS: Tuple[CloseCapabilityCheck, ...] = (
    CloseCapabilityCheck("close_registry_membership", _STATIC_CAPABILITY_PHASES,
                         ("UNKNOWN_CLOSE_STRATEGY",), _check_registry_membership),
    CloseCapabilityCheck("close_capability_declared", _STATIC_CAPABILITY_PHASES,
                         ("UNCLASSIFIED_CLOSE_CAPABILITY",), _check_capability_declared),
    CloseCapabilityCheck("live_only_close", _STATIC_CAPABILITY_PHASES,
                         ("LIVE_ONLY_CLOSE",), _check_live_only_close),
    CloseCapabilityCheck("live_close_context", _STATIC_CAPABILITY_PHASES,
                         ("UNSUPPORTED_LIVE_CONTEXT", "RESEARCH_ONLY_CLOSE_CONTEXT"),
                         _check_live_close_context),
    CloseCapabilityCheck("entry_replay_capability", _STATIC_CAPABILITY_PHASES,
                         ("UNSUPPORTED_REPLAY_CAPABILITY",), _check_entry_replay),
    CloseCapabilityCheck("stop_owner_support", _STATIC_CAPABILITY_PHASES,
                         ("UNSUPPORTED_STOP_OWNER",), _check_stop_owner_support),
    CloseCapabilityCheck("stop_owner_inputs", CAPABILITY_PHASES,
                         ("MISSING_STOP_INPUT",), _check_stop_inputs),
    CloseCapabilityCheck("stop_margin_leverage", _STATIC_CAPABILITY_PHASES,
                         ("UNVERIFIED_MARGIN_LEVERAGE",), _check_margin_leverage),
)


def _validate_capability_checks(checks: Sequence[CloseCapabilityCheck]) -> None:
    registered = set(CLOSE_CAPABILITY_REFUSAL_CODES) | set(CLOSE_CAPABILITY_APPROXIMATION_CODES)
    seen = set()
    for spec in checks:
        if not isinstance(spec, CloseCapabilityCheck):
            raise RuntimeError(f"close capability check {spec!r} is not a CloseCapabilityCheck")
        if not spec.check_id or spec.check_id in seen:
            raise RuntimeError(f"close capability check id {spec.check_id!r} is empty or duplicated")
        seen.add(spec.check_id)
        if not spec.phases or set(spec.phases) - set(CAPABILITY_PHASES):
            raise RuntimeError(f"close capability check {spec.check_id!r} has invalid phases")
        if not spec.reason_codes or set(spec.reason_codes) - registered:
            raise RuntimeError(
                f"close capability check {spec.check_id!r} declares unregistered reason codes")
        if not callable(spec.callback):
            raise RuntimeError(f"close capability check {spec.check_id!r} has no callback")


_validate_capability_checks(CLOSE_CAPABILITY_CHECKS)


def _normalize_close_ref_inputs(close_refs) -> Tuple[list, list]:
    if close_refs is None:
        return [], []
    if isinstance(close_refs, (str, bytes, Mapping)) or not isinstance(close_refs, Sequence):
        return [], [capability_refusal(
            "INVALID_CLOSE_REFERENCE", "close_strategies",
            details={"message": "close_strategies must be a list of "
                                "{'name': str, 'params': dict} refs, got "
                                f"{type(close_refs).__name__}"})]
    refs, findings = [], []
    for idx, ref in enumerate(close_refs):
        if not isinstance(ref, Mapping):
            findings.append(capability_refusal(
                "INVALID_CLOSE_REFERENCE", "close_strategies", close_ref_index=idx,
                details={"message": "close_strategies entries must be dicts of shape "
                                    "{'name': str, 'params': dict}, got "
                                    f"{type(ref).__name__}"}))
            continue
        raw_name = ref.get("name")
        name = raw_name.strip() if isinstance(raw_name, str) else ""
        if not name:
            findings.append(capability_refusal(
                "INVALID_CLOSE_REFERENCE", "close_strategies", close_ref_index=idx,
                details={"message": f"close_strategies ref missing 'name': {dict(ref)}"}))
            continue
        raw_params = ref.get("params")
        if raw_params is not None and not isinstance(raw_params, Mapping):
            findings.append(capability_refusal(
                "INVALID_CLOSE_REFERENCE", name, close_ref_index=idx,
                details={"message": f"close_strategies ref {name!r} params must be a "
                                    f"dict, got {type(raw_params).__name__}"}))
            continue
        name, params = _rewrite_deprecated_close_ref(name, dict(raw_params or {}))
        refs.append({"name": name, "params": params})
    return refs, findings


@dataclass(frozen=True)
class CloseValidation:
    mode: Optional[str]
    consumer: str
    phase: str
    platform: Optional[str]
    strategy_type: Optional[str]
    close_refs: Tuple[Mapping, ...]
    approximations: Tuple[CapabilityRecord, ...]
    refusals: Tuple[CapabilityRecord, ...]
    capability_context: Optional[CapabilityContext] = None
    plain_close_refs: Tuple[dict, ...] = field(default=(), repr=False, compare=False)

    @property
    def accepted(self) -> bool:
        return not self.refusals

    @property
    def close_eligibility(self) -> str:
        if self.refusals:
            return "refused"
        return "approximate" if self.approximations else "eligible"

    @property
    def incomplete_parity(self) -> bool:
        return bool(self.refusals) or self.mode == COMPARISON_MODE_APPROXIMATE

    @property
    def parity_status(self) -> str:
        if self.refusals:
            return "refused"
        return "incomplete" if self.mode == COMPARISON_MODE_APPROXIMATE else "unverified"

    def normalized_close_refs(self) -> list:
        return [copy.deepcopy(r) for r in self.plain_close_refs]

    def to_dict(self) -> dict:
        return {
            "schema_version": CLOSE_VALIDATION_SCHEMA_VERSION,
            "mode": self.mode,
            "close_eligibility": self.close_eligibility,
            "approximations": [r.to_dict() for r in self.approximations],
            "incomplete_parity": self.incomplete_parity,
            "parity_status": self.parity_status,
            "refusals": [r.to_dict() for r in self.refusals],
        }


class CloseCapabilityError(ValueError):

    def __init__(self, validation: CloseValidation, context: str = ""):
        self.validation = validation
        self.context = context
        self.reasons = tuple(r.to_dict() for r in validation.refusals)
        self.reason_code = self.reasons[0]["reason_code"]
        parts = []
        for rec in self.reasons:
            msg = rec["details"].get("message") or rec["feature"]
            parts.append(f"[{rec['reason_code']}] {msg}")
        prefix = f"{context}: " if context else ""
        super().__init__(prefix + "close capability refused: " + "; ".join(parts))

    def with_context(self, context: str) -> "CloseCapabilityError":
        return CloseCapabilityError(self.validation, context=context)

    def to_dict(self) -> dict:
        return {
            "reason_code": self.reason_code,
            "message": str(self),
            "close_validation": self.validation.to_dict(),
        }


def validate_close_capabilities(*, close_refs=None, comparison_mode=None,
                                platform: Optional[str] = None,
                                strategy_type: Optional[str] = None,
                                consumer: str = "engine",
                                phase: str = "construction",
                                capability_context: Optional[CapabilityContext] = None
                                ) -> CloseValidation:
    refusals: list = []
    mode: Optional[str] = None
    if comparison_mode is None:
        mode = COMPARISON_MODE_STRICT
    elif isinstance(comparison_mode, str) and comparison_mode in COMPARISON_MODES:
        mode = comparison_mode
    else:
        refusals.append(capability_refusal(
            "INVALID_COMPARISON_MODE", "comparison_mode",
            details={"message": f"comparison_mode must be omitted (strict) or exactly "
                                f"one of {list(COMPARISON_MODES)}, got {comparison_mode!r}",
                     "received": repr(comparison_mode)}))
    context_errors = []
    if consumer not in CAPABILITY_CONSUMERS:
        context_errors.append(
            f"consumer must be one of {list(CAPABILITY_CONSUMERS)}, got {consumer!r}")
    if phase not in CAPABILITY_PHASES:
        context_errors.append(
            f"phase must be one of {list(CAPABILITY_PHASES)}, got {phase!r}")
    if capability_context is not None:
        if not isinstance(capability_context, CapabilityContext):
            context_errors.append(
                "capability_context must be a CapabilityContext or None, got "
                f"{type(capability_context).__name__}")
        else:
            context_errors.extend(capability_context.errors)
    for err in context_errors:
        refusals.append(capability_refusal(
            "INVALID_CAPABILITY_CONTEXT", "capability_context",
            details={"message": err}))
    refs, ref_findings = _normalize_close_ref_inputs(close_refs)
    refusals.extend(ref_findings)
    approximations: list = []
    frozen_refs = tuple(MappingProxyType({"name": r["name"], "params": _freeze_json(r["params"])})
                        for r in refs)
    if not refusals:
        _evaluate, list_strategies = _load_close_registry()
        request = CloseCapabilityRequest(
            close_refs=frozen_refs,
            mode=mode,
            platform=(str(platform).strip().lower() or None) if platform is not None else None,
            strategy_type=(str(strategy_type).strip().lower() or None)
            if strategy_type is not None else None,
            consumer=consumer,
            phase=phase,
            capability_context=capability_context,
            registered_closes=frozenset(list_strategies()),
        )
        _validate_capability_checks(CLOSE_CAPABILITY_CHECKS)
        for spec in CLOSE_CAPABILITY_CHECKS:
            if phase not in spec.phases:
                continue
            try:
                records = list(spec.callback(request) or [])
            except CloseCapabilityError as exc:
                raise RuntimeError(
                    f"close capability check {spec.check_id!r} raised instead of "
                    "returning findings") from exc
            for rec in records:
                if not isinstance(rec, CapabilityRecord) or rec.reason_code not in spec.reason_codes:
                    raise RuntimeError(
                        f"close capability check {spec.check_id!r} returned an "
                        f"undeclared finding {rec!r}")
                if rec.kind == "refusal" and rec.reason_code in CLOSE_CAPABILITY_REFUSAL_CODES:
                    refusals.append(rec)
                elif rec.kind == "approximation" and \
                        rec.reason_code in CLOSE_CAPABILITY_APPROXIMATION_CODES:
                    approximations.append(rec)
                else:
                    raise RuntimeError(
                        f"close capability check {spec.check_id!r} returned a "
                        f"{rec.kind!r} finding with code {rec.reason_code!r}")
    validation = CloseValidation(
        mode=mode,
        consumer=consumer if consumer in CAPABILITY_CONSUMERS else "engine",
        phase=phase if phase in CAPABILITY_PHASES else "construction",
        platform=platform,
        strategy_type=strategy_type,
        close_refs=frozen_refs,
        approximations=tuple(approximations),
        refusals=tuple(refusals),
        capability_context=capability_context,
        plain_close_refs=tuple(copy.deepcopy(refs)),
    )
    if refusals:
        raise CloseCapabilityError(validation)
    return validation


def _jsonable_stop_value(value):
    if isinstance(value, Mapping):
        return copy.deepcopy(dict(value))
    if value is None or isinstance(value, (bool, str)):
        return value
    number = _finite_number(value)
    return number if number is not None else value


def stop_raw_fields(values: Mapping) -> dict:
    out = {}
    for key in STOP_FIELD_KEYS + STOP_GEOMETRY_INPUT_KEYS:
        value = values.get(key)
        out[key] = {"present": value is not None, "value": _jsonable_stop_value(value)}
    return out


def leverage_evidence(value, source: str, verified: bool) -> dict:
    if value is None:
        return {"status": "missing", "source": source, "value": None}
    lev = _finite_number(value)
    if lev is None or lev <= 0:
        return {"status": "invalid", "source": source, "value": lev}
    return {"status": "verified" if verified else "unverified", "source": source, "value": lev}


def build_stop_capability_context(*, platform, strategy_type, close_refs, fields: Mapping,
                                  regime_windows_spec=None, risk_per_trade_pct=None,
                                  capability_context=None):
    if capability_context is not None and (
            not isinstance(capability_context, CapabilityContext) or capability_context.errors):
        return capability_context
    refs, _ = _normalize_close_ref_inputs(close_refs)
    plain = {k: _jsonable_stop_value(fields.get(k))
             for k in STOP_FIELD_KEYS + STOP_GEOMETRY_INPUT_KEYS}
    if capability_context is None:
        raw_fields = stop_raw_fields(plain)
        evidence = {
            "stop_units": {"status": "verified", "source": STOP_UNITS_DIRECT,
                           "value": "engine_fraction"},
            "leverage": leverage_evidence(plain.get("leverage"), "caller", True),
        }
        declared = None
    else:
        raw_fields = _thaw_json(capability_context.raw_fields)
        evidence = _thaw_json(capability_context.input_evidence)
        owner = capability_context.resolved_stop_owner
        declared = owner["name"] if owner is not None else None
    units = evidence.get("stop_units") if isinstance(evidence.get("stop_units"), dict) else {}
    live = units.get("status") == "verified" and units.get("source") in (
        STOP_UNITS_LIVE_CONFIG, STOP_UNITS_PREVIEW)
    labels = _regime_primary_labels(regime_windows_spec)
    blocks, _ = _parse_stop_regime_blocks(plain, labels, live)
    unified = _unified_close_params(refs) is not None
    owner = resolve_static_stop_owner(
        plain, blocks, unified, uses_hyperliquid_stop_geometry(platform, strategy_type))
    parameters = {
        "fields": plain,
        "admission": "live" if live else "direct",
        "geometry": "legacy" if owner == "legacy" else "hyperliquid",
        "regime_labels": list(labels) if labels else None,
        "risk_per_trade_pct": _jsonable_stop_value(risk_per_trade_pct),
        "unified_close": unified,
        "declared_owner": declared,
    }
    return CapabilityContext(raw_fields=raw_fields,
                             resolved_stop_owner={"name": owner, "parameters": parameters},
                             input_evidence=evidence)


_UNKNOWN_CLOSE_VALIDATION = {
    "schema_version": None,
    "mode": None,
    "close_eligibility": "unknown",
    "approximations": [],
    "incomplete_parity": True,
    "parity_status": "unverified",
    "refusals": [],
}


def _record_is_valid(rec, codes) -> bool:
    if not isinstance(rec, Mapping):
        return False
    if set(rec) != {"reason_code", "feature", "close_ref_index", "required_inputs", "details"}:
        return False
    if rec.get("reason_code") not in codes or not isinstance(rec.get("feature"), str):
        return False
    idx = rec.get("close_ref_index")
    if idx is not None and (isinstance(idx, bool) or not isinstance(idx, int) or idx < 0):
        return False
    inputs = rec.get("required_inputs")
    if not isinstance(inputs, list) or not all(isinstance(x, str) for x in inputs) \
            or inputs != sorted(inputs):
        return False
    return isinstance(rec.get("details"), Mapping)


def decode_close_validation(obj) -> dict:
    def unknown(status: str) -> dict:
        out = copy.deepcopy(_UNKNOWN_CLOSE_VALIDATION)
        if isinstance(obj, Mapping):
            out["schema_version"] = obj.get("schema_version")
        out["decode_status"] = status
        return out

    if obj is None:
        return unknown("missing")
    if not isinstance(obj, Mapping):
        return unknown("inconsistent")
    version = obj.get("schema_version")
    if isinstance(version, bool) or version != CLOSE_VALIDATION_SCHEMA_VERSION:
        return unknown("unknown_schema")
    if obj.get("aggregate") is True:
        return _decode_aggregate(obj, unknown)
    if any(k not in obj for k in CLOSE_VALIDATION_FIELDS):
        return unknown("inconsistent")
    mode = obj["mode"]
    refusals = obj["refusals"]
    approximations = obj["approximations"]
    if not isinstance(refusals, list) or not isinstance(approximations, list):
        return unknown("inconsistent")
    if not all(_record_is_valid(r, CLOSE_CAPABILITY_REFUSAL_CODES) for r in refusals):
        return unknown("inconsistent")
    if not all(_record_is_valid(r, CLOSE_CAPABILITY_APPROXIMATION_CODES) for r in approximations):
        return unknown("inconsistent")
    if not isinstance(obj["incomplete_parity"], bool):
        return unknown("inconsistent")
    if refusals:
        ok = (mode is None or mode in COMPARISON_MODES) \
            and obj["close_eligibility"] == "refused" \
            and obj["parity_status"] == "refused" \
            and obj["incomplete_parity"] is True
    else:
        ok = mode in COMPARISON_MODES \
            and obj["close_eligibility"] == ("approximate" if approximations else "eligible") \
            and not (approximations and mode != COMPARISON_MODE_APPROXIMATE) \
            and obj["incomplete_parity"] is (mode == COMPARISON_MODE_APPROXIMATE) \
            and obj["parity_status"] == (
                "incomplete" if mode == COMPARISON_MODE_APPROXIMATE else "unverified")
    if not ok:
        return unknown("inconsistent")
    out = {k: copy.deepcopy(obj[k]) for k in CLOSE_VALIDATION_FIELDS}
    out["decode_status"] = "ok"
    return out


_AGGREGATE_FIELDS = (
    "schema_version", "aggregate", "children", "modes", "close_eligibility",
    "approximations", "refusals", "unknown_children", "incomplete_parity",
    "parity_status", "requested_set_complete",
)


def _decode_aggregate(obj: Mapping, unknown) -> dict:
    if any(k not in obj for k in _AGGREGATE_FIELDS):
        return unknown("inconsistent")
    if not isinstance(obj["children"], int) or isinstance(obj["children"], bool) \
            or not isinstance(obj["unknown_children"], int) \
            or isinstance(obj["unknown_children"], bool) \
            or obj["unknown_children"] < 0 or obj["children"] < 0 \
            or not isinstance(obj["modes"], list) \
            or not all(m in COMPARISON_MODES for m in obj["modes"]) \
            or not isinstance(obj["approximations"], list) \
            or not isinstance(obj["refusals"], list) \
            or not all(_record_is_valid(r, CLOSE_CAPABILITY_REFUSAL_CODES) for r in obj["refusals"]) \
            or not all(_record_is_valid(r, CLOSE_CAPABILITY_APPROXIMATION_CODES)
                       for r in obj["approximations"]) \
            or not isinstance(obj["incomplete_parity"], bool) \
            or not isinstance(obj["requested_set_complete"], bool):
        return unknown("inconsistent")
    expected = _aggregate_status(
        children=obj["children"], unknown_children=obj["unknown_children"],
        modes=set(obj["modes"]), approximations=obj["approximations"],
        refusals=obj["refusals"])
    for key in ("close_eligibility", "incomplete_parity", "parity_status",
                "requested_set_complete"):
        if obj[key] != expected[key]:
            return unknown("inconsistent")
    out = {k: copy.deepcopy(obj[k]) for k in _AGGREGATE_FIELDS}
    out["decode_status"] = "ok"
    return out


def _aggregate_status(*, children: int, unknown_children: int, modes: set,
                      approximations: list, refusals: list) -> dict:
    incomplete = bool(refusals) or bool(unknown_children) or not children \
        or COMPARISON_MODE_APPROXIMATE in modes
    if refusals:
        eligibility, parity = "refused", "refused"
    elif unknown_children or not children:
        eligibility, parity = "unknown", "incomplete"
    else:
        eligibility = "approximate" if approximations else "eligible"
        parity = "incomplete" if incomplete else "unverified"
    return {
        "close_eligibility": eligibility,
        "incomplete_parity": incomplete,
        "parity_status": parity,
        "requested_set_complete": bool(children) and not unknown_children and not refusals,
    }


def _dedupe_records(records: list) -> list:
    out, seen = [], set()
    for rec in records:
        key = json.dumps(rec, sort_keys=True, default=str)
        if key not in seen:
            seen.add(key)
            out.append(copy.deepcopy(rec))
    return out


def aggregate_close_validations(children) -> dict:
    children = list(children or [])
    modes: set = set()
    approximations: list = []
    refusals: list = []
    unknown_children = 0
    for child in children:
        if isinstance(child, CloseValidation):
            child = child.to_dict()
        if isinstance(child, CloseCapabilityError):
            child = child.validation.to_dict()
        decoded = decode_close_validation(child)
        if decoded["decode_status"] != "ok":
            unknown_children += 1
            continue
        if decoded.get("aggregate"):
            modes.update(decoded["modes"])
            unknown_children += decoded["unknown_children"]
            if not decoded["children"]:
                unknown_children += 1
        elif decoded["mode"] is not None:
            modes.add(decoded["mode"])
        approximations.extend(decoded["approximations"])
        refusals.extend(decoded["refusals"])
    approximations = _dedupe_records(approximations)
    refusals = _dedupe_records(refusals)
    out = {
        "schema_version": CLOSE_VALIDATION_SCHEMA_VERSION,
        "aggregate": True,
        "children": len(children),
        "modes": sorted(modes),
        "approximations": approximations,
        "refusals": refusals,
        "unknown_children": unknown_children,
    }
    out.update(_aggregate_status(children=len(children), unknown_children=unknown_children,
                                 modes=modes, approximations=approximations,
                                 refusals=refusals))
    return {k: out[k] for k in _AGGREGATE_FIELDS}


def format_close_validation(obj) -> str:
    decoded = decode_close_validation(obj)
    if decoded["decode_status"] != "ok":
        return (f"close validation: unknown ({decoded['decode_status']}); "
                "not strict evidence")
    if decoded.get("aggregate"):
        mode = ",".join(decoded["modes"]) or "none"
    else:
        mode = decoded["mode"] or "invalid"
    text = (f"close validation: mode={mode} eligibility={decoded['close_eligibility']} "
            f"parity={decoded['parity_status']}")
    if decoded["approximations"]:
        text += " approximations=" + ",".join(
            f"{r['feature']}({'+'.join(r['required_inputs'])})"
            for r in decoded["approximations"])
    if decoded["refusals"]:
        text += " refusals=" + ",".join(
            f"{r['reason_code']}:{r['feature']}" for r in decoded["refusals"])
    return text


TIMEFRAME_PERIODS_PER_YEAR = {
    "1m":  365 * 24 * 60,
    "5m":  365 * 24 * 12,
    "15m": 365 * 24 * 4,
    "30m": 365 * 24 * 2,
    "1h":  365 * 24,
    "2h":  365 * 12,
    "4h":  365 * 6,
    "6h":  365 * 4,
    "8h":  365 * 3,
    "12h": 365 * 2,
    "1d":  365,
    "1w":  52,
    "1M":  12,
}


def periods_per_year(timeframe: str) -> int:
    return TIMEFRAME_PERIODS_PER_YEAR.get(timeframe, 365)


LIQUIDATED_METRIC_FLOOR = 100.0


PLATFORM_FEE_PCT = {
    "binanceus":   0.001,
    "hyperliquid": 0.00045,
    "robinhood":   0.0,
    "luno":        0.01,
    "okx":         0.001,
    "okx-perps":   0.0005,
}

HYPERLIQUID_MAKER_FEE_PCT = 0.00015


def fee_pct_for_platform(platform: str) -> float:
    return PLATFORM_FEE_PCT.get(platform, PLATFORM_FEE_PCT["binanceus"])


DEFAULT_SLIPPAGE_PCT = 0.0005

EXECUTION_SPEC_KEYS = (
    "taker_fee_pct",
    "maker_fee_pct",
    "half_spread_pct",
    "slippage_pct",
    "size_decimals",
    "min_notional_usd",
    "min_notional_margin",
)

_hl_lot_floor_fn = None


def _hl_floor_lot_size(qty: float, decimals: int) -> float:
    global _hl_lot_floor_fn
    if _hl_lot_floor_fn is None:
        import importlib.util
        path = os.path.join(_REPO_ROOT, "platforms", "hyperliquid", "adapter.py")
        spec = importlib.util.spec_from_file_location("_backtest_hl_adapter_lot", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        _hl_lot_floor_fn = mod.floor_lot_size
    return float(_hl_lot_floor_fn(qty, decimals))


def normalize_execution_spec(spec: Optional[dict]) -> Optional[dict]:
    if spec is None:
        return None
    if not isinstance(spec, dict):
        raise ValueError(f"execution_spec must be a dict, got {type(spec).__name__}")
    unknown = sorted(set(spec) - set(EXECUTION_SPEC_KEYS))
    missing = sorted(set(EXECUTION_SPEC_KEYS) - set(spec))
    if unknown or missing:
        raise ValueError(
            f"execution_spec keys must be exactly {list(EXECUTION_SPEC_KEYS)}; "
            f"unknown={unknown} missing={missing}"
        )
    out: dict = {}
    for key in EXECUTION_SPEC_KEYS:
        value = spec[key]
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise ValueError(f"execution_spec.{key} must be a number, got {value!r}")
        value = float(value)
        if not math.isfinite(value) or value < 0:
            raise ValueError(f"execution_spec.{key} must be finite and >= 0, got {value!r}")
        out[key] = value
    for key in ("taker_fee_pct", "maker_fee_pct", "half_spread_pct", "slippage_pct"):
        if out[key] >= 0.1:
            raise ValueError(
                f"execution_spec.{key} is a fraction (0.00045 = 0.045%); {out[key]!r} is not plausible"
            )
    if not out["size_decimals"].is_integer():
        raise ValueError(
            f"execution_spec.size_decimals must be an integer, got {spec['size_decimals']!r}"
        )
    out["size_decimals"] = int(out["size_decimals"])
    return out


def _open_action_from_signal(signal: int) -> str:
    if signal > 0:
        return "long"
    if signal < 0:
        return "short"
    return "none"


def _parse_profile_allocation(alloc: Optional[dict]) -> Optional[dict]:
    if not alloc:
        return None
    profiles = dict(alloc.get("profiles") or {})
    param_sets = dict(alloc.get("param_sets") or {})
    confirm_bars = int(alloc.get("confirm_bars") or 0)
    initial_profile = str(alloc.get("initial_profile") or "").strip()
    if len(param_sets) != 2:
        raise ValueError(
            f"regime_profile_allocation.param_sets must define exactly 2 "
            f"profiles (the M4 two-profile model), got {len(param_sets)}"
        )
    if confirm_bars < 1:
        raise ValueError("regime_profile_allocation.confirm_bars must be >= 1")
    if initial_profile not in param_sets:
        raise ValueError(
            f"regime_profile_allocation.initial_profile={initial_profile!r} "
            f"is not a param_sets profile {sorted(param_sets)}"
        )
    for lbl, prof in profiles.items():
        if prof not in param_sets:
            raise ValueError(
                f"regime_profile_allocation.profiles[{lbl!r}]={prof!r} is not "
                f"a param_sets profile {sorted(param_sets)}"
            )
    return {
        "profiles": profiles,
        "param_sets": param_sets,
        "confirm_bars": confirm_bars,
        "initial_profile": initial_profile,
        "names": sorted(param_sets),
    }


class _ProfileSwitcher:

    def __init__(self, alloc: dict):
        self._profiles = alloc["profiles"]
        self._confirm_bars = alloc["confirm_bars"]
        self.active = alloc["initial_profile"]
        self._pending = ""
        self._seen = 0

    def step(self, label: str, flat: bool) -> str:
        desired = self._profiles.get((label or "").strip(), "")
        if desired == "":
            return self.active
        if desired == self.active:
            self._pending = ""
            self._seen = 0
            return self.active
        if self._pending == desired:
            self._seen += 1
        else:
            self._pending = desired
            self._seen = 1
        if flat and self._seen >= self._confirm_bars:
            self.active = desired
            self._pending = ""
            self._seen = 0
        return self.active


def _close_refs_use_regime_tiered_tp(refs: list[dict]) -> bool:
    for ref in refs:
        n = (ref.get("name") or "").strip().lower()
        if n in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            return True
    return False


def _normalize_open_action(value) -> str:
    action = str(value or "none").strip().lower()
    if action not in {"long", "short", "none"}:
        raise ValueError(
            "open_action column must contain only 'long', 'short', or 'none' "
            f"(got {value!r})"
        )
    return action


def _close_fraction_columns(df: pd.DataFrame) -> list[str]:
    return [
        c for c in df.columns
        if c == "close_fraction" or str(c).startswith("close_fraction:")
    ]


def _max_close_fraction_series(df: pd.DataFrame) -> pd.Series:
    cols = _close_fraction_columns(df)
    if not cols:
        return pd.Series(0.0, index=df.index)
    fractions = df[cols].fillna(0).astype(float)
    bad = (fractions < 0) | (fractions > 1)
    if bad.any().any():
        values = sorted(set(fractions[bad].stack().tolist()))
        raise ValueError(f"close_fraction values must be in [0, 1] — got {values}")
    return fractions.max(axis=1)


def _validated_entry_fraction_series(df: pd.DataFrame) -> pd.Series:
    vals = df["entry_fraction"].astype(float)
    bad = vals[vals.notna() & ((vals <= 0) | (vals > 1))]
    if not bad.empty:
        values = sorted(set(bad.tolist()))
        raise ValueError(
            f"entry_fraction values must be in (0, 1] — got {values}"
        )
    return vals


class _ScaleInState:

    __slots__ = ("risk_anchor_price", "scale_in_count", "last_add_price",
                 "added_notional_usd", "base_open_notional")

    def __init__(self):
        self.reset()

    def reset(self) -> None:
        self.risk_anchor_price = 0.0
        self.scale_in_count = 0
        self.last_add_price = 0.0
        self.added_notional_usd = 0.0
        self.base_open_notional = 0.0

    def geom_cost(self, avg_cost: float) -> float:
        if self.risk_anchor_price > 0:
            return self.risk_anchor_price
        return avg_cost


_SCALE_IN_CFG_KEYS = (
    "max_adds", "max_added_notional_usd", "add_spacing_atr", "add_notional_usd",
)


def _normalize_scale_in_cfg(scale_in: Optional[dict]) -> dict:
    cfg = {"max_adds": 0, "max_added_notional_usd": 0.0,
           "add_spacing_atr": 0.0, "add_notional_usd": 0.0}
    if not scale_in:
        return cfg
    if not isinstance(scale_in, dict):
        raise ValueError(
            f"scale_in must be a dict of {list(_SCALE_IN_CFG_KEYS)}, "
            f"got {type(scale_in).__name__}"
        )
    unknown = sorted(set(scale_in) - set(_SCALE_IN_CFG_KEYS))
    if unknown:
        raise ValueError(
            f"scale_in has unknown key(s) {unknown}; "
            f"supported: {list(_SCALE_IN_CFG_KEYS)}"
        )
    max_adds = scale_in.get("max_adds", 0) or 0
    if int(max_adds) != max_adds or int(max_adds) < 0:
        raise ValueError(f"scale_in.max_adds must be an int >= 0, got {max_adds!r}")
    cfg["max_adds"] = int(max_adds)
    for key in ("max_added_notional_usd", "add_notional_usd"):
        val = float(scale_in.get(key, 0) or 0)
        if val < 0:
            raise ValueError(f"scale_in.{key} must be >= 0, got {val}")
        cfg[key] = val
    cfg["add_spacing_atr"] = float(scale_in.get("add_spacing_atr", 0) or 0)
    return cfg


def _scale_in_decision(scale_cfg: dict, side: str, quantity: float,
                       avg_cost: float, entry_atr: float, scale_in_count: int,
                       added_notional_usd: float, last_add_price: float,
                       signal: int, price: float,
                       default_open_notional: float) -> Tuple[float, bool, str]:
    if price <= 0:
        return 0.0, False, "no price for scale-in"
    if not ((signal == 1 and side == "long" and quantity > 0)
            or (signal == -1 and side == "short" and quantity > 0)):
        return 0.0, False, "not a same-direction add"

    max_adds = int(scale_cfg.get("max_adds", 0) or 0)
    if max_adds > 0 and scale_in_count >= max_adds:
        return 0.0, False, "scale-in max_adds reached"

    add_notional = default_open_notional
    if (scale_cfg.get("add_notional_usd", 0) or 0) > 0:
        add_notional = float(scale_cfg["add_notional_usd"])
    if add_notional <= 0:
        return 0.0, False, "scale-in add notional resolves to zero"
    max_added = float(scale_cfg.get("max_added_notional_usd", 0) or 0)
    if max_added > 0 and added_notional_usd + add_notional > max_added + 1e-9:
        return 0.0, False, "scale-in max_added_notional_usd reached"

    spacing = float(scale_cfg.get("add_spacing_atr", 0) or 0)
    if spacing != 0:
        if entry_atr <= 0:
            return 0.0, False, "scale-in spacing requires a positive EntryATR"
        last_add = last_add_price
        if last_add <= 0:
            last_add = avg_cost
        direction = -1.0 if side == "short" else 1.0
        favorable_move = (price - last_add) * direction
        needed = spacing * entry_atr
        if spacing > 0:
            if favorable_move + 1e-9 < needed:
                return 0.0, False, "scale-in spacing (add-to-winners) not reached"
        else:
            if -favorable_move + 1e-9 < -needed:
                return 0.0, False, "scale-in spacing (average-down) not reached"

    return add_notional / price, True, ""


def _ungated_leg_notional(leg_notional: float, hurst_size_mult: float) -> float:
    if hurst_size_mult > 0:
        return leg_notional / hurst_size_mult
    return leg_notional


class Trade:
    def __init__(self, entry_date, entry_price, side="long"):
        self.entry_date = entry_date
        self.entry_price = entry_price
        self.side = side
        self.exit_date = None
        self.exit_price = None
        self.pnl = 0.0
        self.pnl_pct = 0.0
        self.shares = 0.0
        self.bars_held = 0
        self.mfe_pct = 0.0
        self.mae_pct = 0.0
        self.bars_to_mfe = 0
        self.bars_to_mae = 0
        self.entry_atr = 0.0
        self.entry_fee = 0.0
        self.exit_fee = 0.0
        self.exit_reason = ""
        self.scale_in_adds = 0

    def close(self, exit_date, exit_price):
        self.exit_date = exit_date
        self.exit_price = exit_price
        if self.side == "long":
            self.pnl_pct = (exit_price - self.entry_price) / self.entry_price
        else:
            self.pnl_pct = (self.entry_price - exit_price) / self.entry_price
        self.pnl = self.shares * self.entry_price * self.pnl_pct

    def to_dict(self):
        return {
            "entry_date": str(self.entry_date),
            "exit_date": str(self.exit_date),
            "entry_price": self.entry_price,
            "exit_price": self.exit_price,
            "side": self.side,
            "shares": self.shares,
            "pnl": round(self.pnl, 2),
            "pnl_pct": round(self.pnl_pct * 100, 2),
            "bars_held": self.bars_held,
            "mfe_pct": round(self.mfe_pct * 100, 4),
            "mae_pct": round(self.mae_pct * 100, 4),
            "bars_to_mfe": self.bars_to_mfe,
            "bars_to_mae": self.bars_to_mae,
            "entry_atr": round(self.entry_atr, 6),
            "entry_fee": round(self.entry_fee, 6),
            "exit_fee": round(self.exit_fee, 6),
            "exit_reason": self.exit_reason,
            "scale_in_adds": self.scale_in_adds,
        }


class _HoldTracker:

    __slots__ = ("bars", "high", "low", "high_bar", "low_bar",
                 "entry_fee", "entry_fee_netted", "entry_price", "side")

    def __init__(self):
        self.open(0.0, "long", 0.0)

    def open(self, entry_price: float, side: str, entry_fee: float) -> None:
        self.bars = 0
        self.high = entry_price
        self.low = entry_price
        self.high_bar = 0
        self.low_bar = 0
        self.entry_fee = entry_fee
        self.entry_fee_netted = 0.0
        self.entry_price = entry_price
        self.side = side

    def step(self, high: float, low: float) -> None:
        self.bars += 1
        if high > self.high:
            self.high = high
            self.high_bar = self.bars
        if low < self.low:
            self.low = low
            self.low_bar = self.bars

    def metrics(self):
        e = self.entry_price
        if e <= 0:
            return 0.0, 0.0, 0, 0
        if self.side == "long":
            return (self.high - e) / e, (self.low - e) / e, self.high_bar, self.low_bar
        return (e - self.low) / e, (e - self.high) / e, self.low_bar, self.high_bar


LEDGER_EVENTS_SCHEMA = "go-trader.backtester-ledger-events"
LEDGER_EVENTS_SCHEMA_VERSION = 1
LEDGER_EVENT_TIMING = {
    "bar_open_fill": "fill at the open of bar_timestamp after a decision on the close of decision_timestamp",
    "intrabar_trigger_fill": "fill inside the bar that opens at bar_timestamp, when a resting trigger or limit price is crossed; no separate decision bar",
    "bar_mark_accrual": "funding events after the previous bar's open and at or before bar_timestamp, valued at the close of the bar that opens at bar_timestamp",
    "seeded_before_first_bar": "inventory assumed open before the first scored bar; not a modeled fill",
    "terminal_mark": "synthetic liquidation at the close of the last scored bar; not a trading decision",
}


def _ledger_ts(value) -> Optional[str]:
    if value is None:
        return None
    ts = pd.Timestamp(value)
    ts = ts.tz_localize("UTC") if ts.tz is None else ts.tz_convert("UTC")
    return ts.isoformat().replace("+00:00", "Z")


class _LedgerEventRecorder:

    def __init__(self, initial_cash: float):
        self.initial_cash = initial_cash
        self.events: list = []
        self.position_seq = 0
        self.position_id: Optional[str] = None
        self.interval_end: Optional[dict] = None

    def record(self, kind: str, *, bar, decision_bar, timing: str, side: str,
               action: str, quantity: float, raw_price: float,
               effective_price: float, fee_rate: float, fee_charged: float,
               reason: str, qty_before: float, qty_after: float,
               avg_cost_before: float, avg_cost_after: float,
               cash_before: float, cash_after: float,
               hold: "_HoldTracker",
               gross_realized: Optional[float] = None,
               entry_fee_allocated: Optional[float] = None,
               funding_cash: Optional[float] = None,
               funding_rate: Optional[float] = None,
               synthetic: bool = False) -> None:
        if kind in ("open", "seed_inventory"):
            self.position_seq += 1
            self.position_id = f"sim-pos-{self.position_seq:04d}"
        seq = len(self.events) + 1
        outstanding = None
        if self.position_id is not None and kind != "funding":
            outstanding = hold.entry_fee - hold.entry_fee_netted
        self.events.append({
            "seq": seq,
            "event_id": f"sim-evt-{seq:06d}",
            "position_local_id": self.position_id,
            "kind": kind,
            "synthetic": synthetic,
            "bar_timestamp": _ledger_ts(bar),
            "decision_timestamp": _ledger_ts(decision_bar),
            "timing": timing,
            "side": side,
            "action": action,
            "quantity": quantity,
            "raw_price": raw_price,
            "effective_price": effective_price,
            "fee_rate": fee_rate,
            "fee_charged": fee_charged,
            "entry_fee_allocated": entry_fee_allocated,
            "entry_fee_outstanding_after": outstanding,
            "gross_realized": gross_realized,
            "funding_cash": funding_cash,
            "funding_rate": funding_rate,
            "reason": reason,
            "qty_before": qty_before,
            "qty_after": qty_after,
            "avg_cost_before": avg_cost_before,
            "avg_cost_after": avg_cost_after,
            "cash_before": cash_before,
            "cash_after": cash_after,
        })
        if kind in ("close", "terminal_liquidation") and qty_after == 0:
            self.position_id = None

    def mark_interval_end(self, *, bar, cash: float, position: float,
                          avg_cost: float, hold: "_HoldTracker") -> None:
        open_position = position != 0
        self.interval_end = {
            "bar_timestamp": _ledger_ts(bar),
            "timing": "after every booking on the last scored bar, before the synthetic terminal liquidation",
            "cash": cash,
            "position_qty": position,
            "side": ("long" if position > 0 else "short") if open_position else None,
            "avg_cost": avg_cost if open_position else None,
            "position_local_id": self.position_id if open_position else None,
            "entry_fee_outstanding": (hold.entry_fee - hold.entry_fee_netted) if open_position else 0.0,
        }

    def envelope(self) -> dict:
        return {
            "schema": LEDGER_EVENTS_SCHEMA,
            "schema_version": LEDGER_EVENTS_SCHEMA_VERSION,
            "time_basis": "UTC; a naive frame index is read as UTC",
            "timing_meanings": dict(LEDGER_EVENT_TIMING),
            "initial_cash": self.initial_cash,
            "events": self.events,
            "interval_end": self.interval_end,
        }


def _stamp_hold(trade, hold: "_HoldTracker", *, entry_atr: float,
                exit_fee: float, reason: str, qty_frac: float = 1.0,
                true_up_entry_fee: bool = False) -> None:
    mfe, mae, b_mfe, b_mae = hold.metrics()
    trade.bars_held = hold.bars
    trade.mfe_pct = mfe
    trade.mae_pct = mae
    trade.bars_to_mfe = b_mfe
    trade.bars_to_mae = b_mae
    trade.entry_atr = entry_atr
    if true_up_entry_fee:
        trade.entry_fee = max(0.0, hold.entry_fee - hold.entry_fee_netted)
    else:
        trade.entry_fee = hold.entry_fee * qty_frac
    hold.entry_fee_netted += trade.entry_fee
    trade.exit_fee = exit_fee
    trade.exit_reason = reason
    trade.pnl -= trade.entry_fee + trade.exit_fee


class Backtester:

    def __init__(self, initial_capital: float = 1000.0,
                 commission_pct: Optional[float] = None,
                 slippage_pct: float = 0.0005,
                 platform: str = "binanceus",
                 open_strategy: Optional[dict] = None,
                 close_strategies: Optional[list[dict]] = None,
                 regime_enabled: bool = False,
                 regime_period: int = 14,
                 regime_adx_threshold: float = 20.0,
                 regime_windows_spec: Optional[dict] = None,
                 hurst_gate: Optional[dict] = None,
                 allowed_regimes: Optional[list[str]] = None,
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
                 regime_directional_certified: bool = False,
                 regime_directional_certified_states: Optional[dict] = None,
                 regime_timeframe: Optional[str] = None,
                 profile_allocation: Optional[dict] = None,
                 intrabar_resolution: str = "ohlc_walk",
                 risk_per_trade_pct: Optional[float] = None,
                 allow_scale_in: bool = False,
                 scale_in: Optional[dict] = None,
                 atr_method: str = "simple",
                 execution_spec: Optional[dict] = None,
                 comparison_mode: Optional[str] = None,
                 leverage: Optional[float] = None,
                 max_drawdown_pct: Optional[float] = None,
                 trailing_stop_min_move_pct: Optional[float] = None,
                 capability_context: Optional[CapabilityContext] = None,
                 stop_platform: Optional[str] = None,
                 regime_label_columns: Optional[dict] = None):
        self.initial_capital = initial_capital
        self._execution = normalize_execution_spec(execution_spec)
        if self._execution is not None and (
            commission_pct is not None or slippage_pct != DEFAULT_SLIPPAGE_PCT
        ):
            raise ValueError(
                "execution_spec owns fees, spread and slippage; do not also pass "
                "commission_pct or a non-default slippage_pct (each cost would be "
                "charged twice)"
            )
        self.platform = platform
        self.intrabar_resolution = str(intrabar_resolution or "").strip().lower()
        if self.intrabar_resolution not in ("ohlc_walk", "bar_close"):
            raise ValueError(
                f"intrabar_resolution must be 'ohlc_walk' or 'bar_close', "
                f"got {intrabar_resolution!r}"
            )
        self.commission_pct = (
            commission_pct if commission_pct is not None
            else fee_pct_for_platform(platform)
        )
        self.slippage_pct = slippage_pct
        self._maker_fee_pct: Optional[float] = None
        if self._execution is not None:
            self.commission_pct = self._execution["taker_fee_pct"]
            self.slippage_pct = (
                self._execution["half_spread_pct"] + self._execution["slippage_pct"]
            )
            self._maker_fee_pct = self._execution["maker_fee_pct"]
        self.open_strategy = dict(open_strategy or {})
        stop_inputs = {
            "stop_loss_atr_mult": stop_loss_atr_mult,
            "stop_loss_pct": stop_loss_pct,
            "stop_loss_margin_pct": stop_loss_margin_pct,
            "trailing_stop_atr_mult": trailing_stop_atr_mult,
            "trailing_stop_pct": trailing_stop_pct,
            "stop_loss_atr_mult_regime": stop_loss_atr_mult_regime,
            "trailing_stop_atr_mult_regime": trailing_stop_atr_mult_regime,
            "leverage": leverage,
            "max_drawdown_pct": max_drawdown_pct,
            "trailing_stop_min_move_pct": trailing_stop_min_move_pct,
        }
        self._stop_context = build_stop_capability_context(
            platform=stop_platform or platform,
            strategy_type=strategy_type,
            close_refs=close_strategies,
            fields=stop_inputs,
            regime_windows_spec=regime_windows_spec,
            risk_per_trade_pct=risk_per_trade_pct,
            capability_context=capability_context,
        )
        self._close_validation = validate_close_capabilities(
            close_refs=close_strategies,
            comparison_mode=comparison_mode,
            platform=platform,
            strategy_type=strategy_type,
            consumer="engine",
            phase="construction",
            capability_context=self._stop_context,
        )
        self._stop_owner = self._stop_context.resolved_stop_owner["name"]
        self._stop_parameters = _thaw_json(self._stop_context.resolved_stop_owner["parameters"])
        self._hl_stop_geometry = self._stop_owner != "legacy"
        self._stop_admission_live = self._stop_parameters["admission"] == "live"
        self.leverage = _finite_number(leverage)
        self.max_drawdown_pct = _finite_number(max_drawdown_pct)
        _min_move = _finite_number(trailing_stop_min_move_pct)
        self.trailing_stop_min_move_pct = (
            _min_move if _min_move is not None and _min_move >= 0
            else DEFAULT_TRAILING_STOP_MIN_MOVE_FRACTION
        )
        self.comparison_mode = self._close_validation.mode
        self._close_refs: list[dict] = self._close_validation.normalized_close_refs()
        self.close_strategies = [r["name"] for r in self._close_refs]
        self.close_params = {r["name"]: r["params"] for r in self._close_refs}
        self._resting_tp_model = str(platform or "").strip().lower() == "hyperliquid"
        self.regime_enabled = regime_enabled
        self._regime_label_columns = dict(regime_label_columns) if regime_label_columns else None
        self._run_position_regime = ""
        self._stamp_gate = ""
        self._stamp_directional = ""
        self._stamp_atr = ""
        self._regime_stamp_trace = []
        self._regime_trace = False
        self.regime_timeframe = str(regime_timeframe or "").strip() or None
        self.regime_period = regime_period
        self.regime_adx_threshold = regime_adx_threshold
        self.regime_windows_spec = dict(regime_windows_spec) if regime_windows_spec else None
        self._regime_primary_labels = _regime_primary_labels(self.regime_windows_spec)
        if self._resting_tp_model:
            _ensure_close_strategies_path()
            from tiered_tp_atr_regime import resting_ladder_errors
            _ladder_errs = []
            for _ref in self._close_refs:
                _ladder_errs.extend(resting_ladder_errors(
                    _ref["name"], _ref["params"], self._regime_primary_labels,
                ))
            if _ladder_errs:
                raise ValueError(
                    "Invalid Hyperliquid take-profit ladder (cumulative close_fraction "
                    "must strictly increase with atr_multiple, as the on-chain tiers "
                    "require): " + "; ".join(_ladder_errs)
                )
        self.hurst_gate = dict(hurst_gate) if hurst_gate else None
        self.allowed_regimes = list(allowed_regimes or [])
        _norm_gate = str(regime_gate_on_failure or "").strip().lower() or "open"
        if _norm_gate not in ("open", "closed"):
            raise ValueError(
                f"regime_gate_on_failure must be 'open' or 'closed', "
                f"got {regime_gate_on_failure!r}"
            )
        self.regime_gate_on_failure = _norm_gate
        _norm_atr_method = str(atr_method or "").strip().lower() or "simple"
        if _norm_atr_method not in ("simple", "wilder"):
            raise ValueError(
                f"atr_method must be 'simple' or 'wilder', got {atr_method!r}"
            )
        self.atr_method = _norm_atr_method
        self.stop_loss_atr_mult = stop_loss_atr_mult
        self.stop_loss_pct = stop_loss_pct
        self.stop_loss_margin_pct = stop_loss_margin_pct
        self.trailing_stop_atr_mult = trailing_stop_atr_mult
        self.trailing_stop_pct = trailing_stop_pct
        self.strategy_type = strategy_type
        self.direction = (str(direction).strip().lower() if direction else None)
        self.invert_signal = bool(invert_signal)
        self.regime_directional_policy = _normalize_regime_directional_policy(
            regime_directional_policy,
        )
        if self.regime_directional_policy is not None:
            if regime_directional_certified_states is not None:
                cert_states = regime_directional_certified_states
            elif bool(regime_directional_certified):
                cert_states = None
            else:
                cert_states = {}
            self.regime_directional_policy = _gate_directional_policy_by_states(
                self.regime_directional_policy, cert_states,
            )
            if not self.regime_directional_policy:
                print("[#1085] regime_directional_policy present but NOT certified "
                      "(or no state survives the per-state sign gate) for this "
                      "(asset,timeframe,classifier) — DEFAULT-OFF in backtest "
                      "(base direction), mirroring live (#1076 negative result).",
                      file=sys.stderr)
                self.regime_directional_policy = None
        if self.regime_directional_policy is not None and not self.regime_enabled:
            raise ValueError(
                "regime_directional_policy requires regime_enabled=True"
            )
        self._profile_alloc = _parse_profile_allocation(profile_allocation)
        self.stop_loss_atr_mult_regime = (
            dict(stop_loss_atr_mult_regime) if stop_loss_atr_mult_regime else None
        )
        self.trailing_stop_atr_mult_regime = (
            dict(trailing_stop_atr_mult_regime) if trailing_stop_atr_mult_regime else None
        )
        self._stop_loss_regime_block = None
        self._trailing_stop_regime_block = None
        self._uses_regime_tiered_close = _close_refs_use_regime_tiered_tp(
            self._close_refs,
        )
        self._unified_close_params: Optional[dict] = None
        self._unified_scalar_params = None
        self._uses_trailing_ratchet_close = any(
            (r.get("name") or "").strip().lower()
            in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime")
            for r in self._close_refs
        )
        _zscore_refs = [
            r for r in self._close_refs
            if (r.get("name") or "").strip().lower() == "zscore_target"
        ]
        if len(_zscore_refs) > 1:
            raise ValueError(
                "duplicate zscore_target close refs are not supported "
                "(close params are keyed by name; the second would silently "
                "override the first's lookback)"
            )
        self._zscore_lookback = 0
        if _zscore_refs:
            try:
                self._zscore_lookback = int(
                    (_zscore_refs[0].get("params") or {}).get("lookback", 0) or 0
                )
            except (TypeError, ValueError):
                self._zscore_lookback = 0
        self._ratchet_mod = None
        self._ratchet_ref: Optional[dict] = None
        self._ratchet_tiers_run: list = []
        if self._uses_trailing_ratchet_close:
            self._ratchet_mod = _load_trailing_ratchet()
            for ref in self._close_refs:
                n = (ref.get("name") or "").strip().lower()
                if n in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime"):
                    self._ratchet_ref = ref
                    break
        _needs_regime_atr = (
            self.stop_loss_atr_mult_regime is not None
            or self.trailing_stop_atr_mult_regime is not None
            or self._uses_regime_tiered_close
        )
        if _needs_regime_atr:
            _ensure_close_strategies_path()
            from regime_atr import (
                close_params_are_unified_regime,
                resolve_regime_atr,
                unified_regime_scalar_params,
                validate_unified_regime_close,
            )

            self._unified_close_params = None
            self._unified_scalar_params = unified_regime_scalar_params
            for _ref in self._close_refs:
                _n = (_ref.get("name") or "").strip().lower()
                if _n not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
                    continue
                _params = _ref.get("params") or {}
                if close_params_are_unified_regime(_params):
                    self._unified_close_params = dict(_params)
                break
            if self._unified_close_params is not None:
                _unified_errs = validate_unified_regime_close(
                    self._unified_close_params,
                    labels=self._regime_primary_labels,
                )
                if _unified_errs:
                    raise ValueError(
                        "Invalid unified per-regime close block: "
                        + "; ".join(_unified_errs)
                    )
            _blocks, _ = _parse_stop_regime_blocks(
                {
                    "stop_loss_atr_mult_regime": self.stop_loss_atr_mult_regime,
                    "trailing_stop_atr_mult_regime": self.trailing_stop_atr_mult_regime,
                },
                self._regime_primary_labels,
                self._stop_admission_live,
            )
            self._stop_loss_regime_block = _blocks["stop_loss_atr_mult_regime"]
            self._trailing_stop_regime_block = _blocks["trailing_stop_atr_mult_regime"]
            self._resolve_regime_atr = resolve_regime_atr
        else:
            self._resolve_regime_atr = None

        self._sl_mod = _load_post_tp_sl()
        _tier_vocab_errs = self._sl_mod.validate_regime_tiered_tp_labels(
            self._close_refs, labels=self._regime_primary_labels,
        )
        if _tier_vocab_errs:
            raise ValueError(
                "Invalid regime tiered-TP configuration: " + "; ".join(_tier_vocab_errs)
            )
        self._sl_after_rules_static, _sl_parse_errs = (
            self._sl_mod.parse_strategy_tp_sl_after_rules(
                self._close_refs, labels=self._regime_primary_labels)
        )
        self._tp_tier_thresholds_static = self._sl_mod.parse_tp_tier_close_fractions(
            self._close_refs,
        )
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._clear_position_stamps()
        any_sl_after_key = False
        for ref in self._close_refs:
            params = ref.get("params") or {}
            if "sl_after" in params:
                any_sl_after_key = True
                break
            tiers_raw = params.get("tp_tiers", params.get("tiers"))
            if isinstance(tiers_raw, list) and any(
                isinstance(t, dict) and "sl_after" in t for t in tiers_raw
            ):
                any_sl_after_key = True
                break
            trend = params.get("trend_regime")
            if isinstance(trend, dict):
                for block in trend.values():
                    if not isinstance(block, dict):
                        continue
                    nested = block.get("tp_tiers")
                    if isinstance(nested, list) and any(
                        isinstance(t, dict) and "sl_after" in t for t in nested
                    ):
                        any_sl_after_key = True
                        break
            if any_sl_after_key:
                break
        self._any_sl_after_key = any_sl_after_key
        self._sl_after_pipeline_enabled = (
            self._sl_after_rules_static.has_any() or any_sl_after_key
        )
        if (
            self._sl_after_rules_static.has_any()
            or _sl_parse_errs
            or any_sl_after_key
        ):
            errs = self._sl_mod.validate_post_tp_stop_loss_rules(
                self._close_refs,
                stop_loss_atr_mult=self.stop_loss_atr_mult,
                stop_loss_pct=self.stop_loss_pct,
                stop_loss_margin_pct=self.stop_loss_margin_pct,
                trailing_stop_atr_mult=self.trailing_stop_atr_mult,
                trailing_stop_pct=self.trailing_stop_pct,
                stop_loss_atr_mult_regime=self.stop_loss_atr_mult_regime,
                strategy_type=self.strategy_type,
                labels=self._regime_primary_labels,
            )
            if errs:
                raise ValueError(
                    "Invalid sl_after configuration: " + "; ".join(errs)
                )
            if self._sl_after_rules_static.has_any():
                regime_rules = []
                if self._sl_after_rules_static.default.has_regime():
                    regime_rules.append("strategy-level default")
                for idx, r in enumerate(self._sl_after_rules_static.per_tier):
                    if r.has_regime():
                        regime_rules.append(f"tier[{idx}]")
                if regime_rules:
                    raise ValueError(
                        "Invalid sl_after configuration: regime-aware "
                        "trend_regime block is HL-live-only in this release "
                        "(backtester parity deferred — see #736). Found on: "
                        + ", ".join(regime_rules)
                        + ". Use the scalar atr_mult / trail_from_here.atr_mult "
                        "form for backtesting."
                    )

        self.risk_per_trade_pct: Optional[float] = None
        if risk_per_trade_pct is not None:
            pct = float(risk_per_trade_pct)
            if not (0 < pct <= 10):
                raise ValueError(
                    f"risk_per_trade_pct must be in (0, 10], got {pct}"
                )
            self.risk_per_trade_pct = pct
        self._risk_owner: Tuple[Optional[str], Optional[str]] = (None, None)
        if self.risk_per_trade_pct is not None:
            _risk_kind, _risk_field, _ = resolve_risk_stop_owner(
                self._stop_parameters["fields"],
                self._unified_close_params is not None,
                self._stop_admission_live,
            )
            self._risk_owner = (_risk_kind, _risk_field)
        self._risk_cap_warned = False
        self._risk_skip_warned = False
        self._stop_observer: Optional[Callable[[dict], None]] = None

        self.allow_scale_in = bool(allow_scale_in)
        if scale_in and not self.allow_scale_in:
            raise ValueError(
                "scale_in block is set but allow_scale_in is false — enable "
                "allow_scale_in or remove the block (the live daemon rejects "
                "this config at startup)"
            )
        self._scale_in_cfg = _normalize_scale_in_cfg(scale_in)
        if self.allow_scale_in and self.risk_per_trade_pct is not None:
            raise ValueError(
                "allow_scale_in is mutually exclusive with risk_per_trade_pct "
                "(#1268: add legs re-size off the frozen SL geometry, breaking "
                "the constant-dollar-risk invariant; the live daemon rejects "
                "this config at startup)"
            )

    @property
    def stop_owner(self) -> str:
        return self._stop_owner

    def _apply_direction_invert(self, sig_int: pd.Series,
                                uses_open_close: bool) -> pd.Series:
        return sig_int.map(
            lambda s: _apply_direction_invert_value(
                int(s), uses_open_close, self.direction, self.invert_signal,
            )
        ).astype(int)

    def _clear_position_stamps(self) -> None:
        had = bool(self._run_position_regime or self._stamp_gate
                   or self._stamp_directional or self._stamp_atr)
        self._run_position_regime = ""
        self._stamp_gate = ""
        self._stamp_directional = ""
        self._stamp_atr = ""
        if had and self._regime_trace:
            self._regime_stamp_trace.append({
                "event": "clear", "gate": "", "directional": "", "atr": "",
            })

    def _note_regime_stamp(self, event: str) -> None:
        if not self._regime_trace:
            return
        self._regime_stamp_trace.append({
            "event": event,
            "gate": self._stamp_gate,
            "directional": self._stamp_directional,
            "atr": self._stamp_atr,
        })

    def _effective_directional_entry(
        self, current_regime: str, position_regime: str, position_qty: float,
    ) -> tuple[str, bool]:
        entry = _resolve_regime_directional_entry(
            self.regime_directional_policy,
            current_regime,
            position_regime,
            abs(position_qty),
        )
        if entry is None:
            return self.direction or "", self.invert_signal
        return str(entry["direction"]), bool(entry["invert_signal"])

    def _normalize_profile_signals(self, df: pd.DataFrame, uses_open_close: bool) -> None:
        for p in self._profile_alloc["names"]:
            col = "signal__" + p
            sig_raw = df[col].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got non-integral values "
                    f"{sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got unexpected values "
                    f"{sorted(bad.unique().tolist())}"
                )
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            if uses_open_close:
                df["_open_action__" + p] = (
                    sig_int.map(_open_action_from_signal).shift(1).fillna("none")
                )
            df[col] = sig_int.shift(1).fillna(0).astype(int)
        df["signal"] = 0
        if uses_open_close:
            df["_open_action"] = "none"
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)
        df["_profile_label"] = df["_profile_label"].shift(1).fillna("")

    def run(self, df: pd.DataFrame, strategy_name: str = "Unknown",
            symbol: str = "BTC/USDT", timeframe: str = "1d",
            params: Optional[dict] = None, save: bool = True,
            starting_long: Optional[dict] = None,
            indicator_frame: Optional[pd.DataFrame] = None,
            record_events: bool = False,
            stop_observer: Optional[Callable[[dict], None]] = None) -> dict:
        self._stop_observer = stop_observer
        uses_open_close = (
            "open_action" in df.columns
            or bool(_close_fraction_columns(df))
            or bool(self.close_strategies)
        )
        if self.direction == "both" and not uses_open_close:
            raise ValueError(
                "direction='both' requires a close evaluator (open/close "
                "engine path) — the plain single-leg path cannot open one "
                "side and close the other, so the run would silently score "
                "long/flat. Backtest each leg separately with "
                "direction='long' / direction='short'."
            )
        if self.regime_directional_policy is not None and not uses_open_close:
            both_labels = sorted(
                label for label, entry in self.regime_directional_policy.items()
                if entry.get("direction") == "both"
            )
            if both_labels:
                raise ValueError(
                    "regime_directional_policy direction='both' requires a "
                    "close evaluator on the plain signal path; labels with "
                    f"both: {both_labels}"
                )
        if self._execution is not None:
            if not uses_open_close:
                raise ValueError(
                    "execution_spec requires the open/close engine path (a close "
                    "strategy or open_action/close_fraction columns); the plain "
                    "signal path has no lot- or minimum-aware fill sites"
                )
            if self.allow_scale_in:
                raise ValueError(
                    "execution_spec does not model scale-in adds; run without "
                    "allow_scale_in"
                )
            if starting_long:
                raise ValueError(
                    "execution_spec cannot seed starting_long: the seeded "
                    "quantity was never lot-floored or minimum-checked"
                )
        plain_short = (not uses_open_close) and self.direction == "short"
        if plain_short and starting_long:
            raise ValueError(
                "starting_long cannot seed a direction='short' plain-path "
                "run — the short/flat path never emits a long close, so the "
                "seeded long would be carried untouched to end-of-data."
            )
        has_profile_alloc = self._profile_alloc is not None
        if has_profile_alloc:
            if "_profile_label" not in df.columns:
                raise ValueError(
                    "regime_profile_allocation backtest requires a '_profile_label' column"
                )
            missing = [
                p for p in self._profile_alloc["names"]
                if ("signal__" + p) not in df.columns
            ]
            if missing:
                raise ValueError(
                    f"regime_profile_allocation backtest is missing signal columns "
                    f"for profiles {missing} (expected 'signal__<profile>')"
                )
        if "signal" not in df.columns and not uses_open_close and not has_profile_alloc:
            raise ValueError("DataFrame must have a 'signal' column or open_action/close_fraction columns")
        if indicator_frame is not None:
            if not indicator_frame.index.is_unique or not df.index.isin(indicator_frame.index).all():
                raise ValueError(
                    "indicator_frame must have a unique index that contains every "
                    "bar of the scored frame"
                )
            shared = [c for c in ("open", "high", "low", "close")
                      if c in df.columns and c in indicator_frame.columns]
            if not np.array_equal(
                indicator_frame.loc[df.index, shared].to_numpy(dtype=float),
                df[shared].to_numpy(dtype=float),
                equal_nan=True,
            ):
                raise ValueError(
                    "indicator_frame bars must match the scored frame's open, high, "
                    "low and close values"
                )
            history = indicator_frame
        else:
            history = df

        df = df.copy()
        if has_profile_alloc:
            self._normalize_profile_signals(df, uses_open_close)
        elif "signal" in df.columns:
            sig_raw = df["signal"].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"non-integral values {sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"unexpected values {sorted(bad.unique().tolist())}"
                )
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            signal_for_open = sig_int
            df["signal"] = sig_int.shift(1).fillna(0).astype(int)
        else:
            signal_for_open = pd.Series(0, index=df.index)
            df["signal"] = 0

        if uses_open_close and not has_profile_alloc:
            if "open_action" in df.columns:
                open_actions = df["open_action"].map(_normalize_open_action)
            else:
                open_actions = signal_for_open.map(_open_action_from_signal)
            df["_open_action"] = open_actions.shift(1).fillna("none")
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)

        if "entry_fraction" in df.columns:
            df["_entry_fraction"] = (
                _validated_entry_fraction_series(df).shift(1).fillna(1.0)
            )

        label_columns = self._regime_label_columns or {}
        if label_columns:
            required = []
            for key in ("gate", "directional", "atr"):
                name = label_columns.get(key)
                if isinstance(name, str) and name.strip():
                    required.append(name.strip())
            missing = [
                name for name in required
                if name not in df.columns and name not in getattr(history, "columns", [])
            ]
            if missing:
                raise ValueError(
                    "refusing missing regime label column "
                    + ", ".join(missing)
                    + "; an absent named column is not recomputed from the primary column"
                )

            def _aligned_label(name: str) -> pd.Series:
                source = history[name] if name in history.columns else df[name]
                return source.reindex(df.index).fillna("").map(lambda v: str(v or "").strip())

            gate_name = str(label_columns.get("gate") or "").strip()
            if gate_name:
                gate_close = _aligned_label(gate_name)
                df["_regime_gate_close"] = gate_close
                df["_regime_gate"] = gate_close.shift(1).fillna("")
            dir_name = str(label_columns.get("directional") or "").strip()
            if dir_name:
                df["_regime_directional_close"] = _aligned_label(dir_name)
            if label_columns.get("atr_named"):
                atr_name = str(label_columns.get("atr") or "").strip()
                if atr_name:
                    atr_close = _aligned_label(atr_name)
                    df["_regime_atr_close"] = atr_close
                    # Decision bar: the named column shifted once, known at the bar-open fill.
                    df["_regime_atr_decision"] = atr_close.shift(1).fillna("")
            self._regime_trace = True
            self._regime_stamp_trace = []
        elif self.regime_enabled and "regime" not in df.columns:
            ensure_regime = _load_regime()
            ensure_regime(
                df,
                period=self.regime_period,
                adx_threshold=self.regime_adx_threshold,
                windows_spec=self.regime_windows_spec,
            )

        if "regime" in df.columns:
            df["_regime_bar_close"] = df["regime"].copy()

        if self.regime_enabled and "regime" in df.columns:
            regime_source = history["regime"] if "regime" in history.columns else df["regime"]
            df["regime"] = regime_source.shift(1).reindex(df.index).fillna("")

        hurst_runner = None
        if self.hurst_gate and self.hurst_gate.get("enabled"):
            from hurst_gate import HurstGate, hurst_live_frame_bars, rolling_hurst

            hurst_runner = HurstGate(self.hurst_gate)
            frame_bars = hurst_live_frame_bars(
                self.regime_windows_spec, self.regime_period
            )
            df["_hurst"] = rolling_hurst(history["close"], frame_bars).shift(1).reindex(df.index)

        has_open = "open" in df.columns

        def _decision_protection_label(row) -> str:
            """Regime label known at a bar-open fill: the previous bar's close.

            `_regime_gate` is the gate column shifted once. The fill bar's own
            close (`_regime_gate_close`) is not known until that bar ends.
            """
            if not self._regime_label_columns:
                return ""
            if self._regime_label_columns.get("atr_named"):
                return str(row.get("_regime_atr_decision", "") or "").strip()
            return str(row.get("_regime_gate", "") or "").strip()

        def _row_close_protection_label(row) -> str:
            """Label written on this row. A seeded position uses it only when
            the recorded entry label is missing."""
            if self._regime_label_columns:
                if self._regime_label_columns.get("atr_named"):
                    return str(row.get("_regime_atr_close", "") or "").strip()
                return str(row.get("_regime_gate_close", "") or "").strip()
            if self.regime_enabled:
                return str(row.get("regime", "") or "").strip()
            return str(row.get("_regime_bar_close", "") or "").strip()

        def _entry_stamp(row) -> str:
            if self._regime_label_columns:
                return _decision_protection_label(row)
            if self.regime_enabled:
                return str(row.get("regime", "") or "").strip()
            return str(row.get("_regime_bar_close", "") or "").strip()

        def _bar_close_regime(row) -> str:
            return str(row.get("_regime_bar_close", "") or "").strip()

        cash = self.initial_capital
        position = 0.0
        trades = []
        current_trade = None
        equity_curve = []
        rec = _LedgerEventRecorder(self.initial_capital) if record_events else None

        avg_cost = 0.0
        initial_quantity = 0.0
        entry_atr_value = 0.0
        pending_close_fraction = 0.0
        pending_close_reason = ""
        hold = _HoldTracker()
        scale = _ScaleInState()
        scale_in_adds_total = 0
        scale_in_added_notional_total = 0.0

        sl_trigger_px = 0.0
        sl_tiers_processed = 0
        post_tp_trail_mult: Optional[float] = None
        sl_high_water_px = 0.0

        pending_signal_sl_close = False
        walk_mode = self.intrabar_resolution == "ohlc_walk"
        sl_pierce_armed = False
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._clear_position_stamps()
        sl_after_active = self._sl_after_pipeline_enabled
        trailing_ratchet_active = self._uses_trailing_ratchet_close

        zscore_series = None
        if self._zscore_lookback > 0 and "close" in history.columns:
            lb = self._zscore_lookback
            closes = history["close"].astype(float)
            roll = closes.rolling(lb)
            std = roll.std(ddof=0)
            zscore_series = (
                (closes - roll.mean()) / std.replace(0.0, float("nan"))
            ).reindex(df.index)

        avwap_series = df["avwap"] if "avwap" in df.columns else None
        if self._close_names_include_avwap_stop():
            avwap_usable = avwap_series is not None and bool(
                (pd.to_numeric(avwap_series, errors="coerce") > 0).any()
            )
            if not avwap_usable:
                from strategy_composition import warn_avwap_stop_missing_context
                warn_avwap_stop_missing_context()

        atr_series = history["atr"] if "atr" in history.columns else None
        if atr_series is None and (
            (self.stop_loss_atr_mult is not None and self.stop_loss_atr_mult > 0)
            or (self.trailing_stop_atr_mult is not None and self.trailing_stop_atr_mult > 0)
            or self._stop_owner in STOP_OWNERS_NEEDING_ATR
        ):
            atr_series = standard_atr(history, method=self.atr_method)

        def _initial_trail_trigger(side: str, mark: float, entry_atr: float,
                                    trail_mult: float) -> float:
            if mark <= 0 or entry_atr <= 0 or trail_mult <= 0:
                return 0.0
            if side == "long":
                return mark - trail_mult * entry_atr
            if side == "short":
                return mark + trail_mult * entry_atr
            return 0.0

        def stamp_open_from_label(stamp: str, row=None, *, keep_recorded: bool = False,
                                  seed_row: bool = False) -> None:
            if self._regime_label_columns and row is not None:
                recorded = (stamp or "").strip() if keep_recorded else ""
                if recorded:
                    gate = directional = atr = recorded
                    stamp = recorded
                else:
                    gate = str(row.get("_regime_gate_close", "") or "").strip()
                    if self._regime_label_columns.get("directional_named"):
                        directional = str(row.get("_regime_directional_close", "") or "").strip()
                    else:
                        directional = gate
                    if self._regime_label_columns.get("atr_named"):
                        atr = str(row.get("_regime_atr_close", "") or "").strip()
                    else:
                        atr = gate
                    # A seed with no recorded label keeps the row's own label.
                    # A modeled directional policy arms from this same closed-candle
                    # row: live writes one result.Regime into pos.Regime, and both
                    # the stop and the open-position policy read that stamp.
                    # With no directional consumer, a bar-open fill arms from the
                    # decision bar. The fill bar's close is still in the future.
                    if seed_row or self.regime_directional_policy is not None:
                        stamp = atr
                    else:
                        stamp = _decision_protection_label(row)
                self._stamp_gate = gate
                self._stamp_directional = directional
                self._stamp_atr = atr
                self._note_regime_stamp("stamp")
            lab = (stamp or "").strip()
            self._run_position_regime = lab
            if self._uses_regime_tiered_close:
                rules_rt, _ = self._sl_mod.parse_strategy_tp_sl_after_rules(
                    self._close_refs, regime=lab,
                    labels=self._regime_primary_labels,
                )
                self._active_sl_after_rules = rules_rt
                self._run_tp_tier_thresholds = self._sl_mod.parse_tp_tier_close_fractions(
                    self._close_refs, regime=lab,
                )
            else:
                self._active_sl_after_rules = self._sl_after_rules_static
                self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)

            self._ratchet_tiers_run = []
            if self._uses_trailing_ratchet_close and self._ratchet_mod and self._ratchet_ref:
                regime_table = (
                    (self._ratchet_ref.get("name") or "").strip().lower()
                    == "trailing_tp_ratchet_regime"
                )
                tiers, terr = self._ratchet_mod.resolve_tiers_for_regime(
                    self._ratchet_ref.get("params") or {},
                    lab,
                    regime_table=regime_table,
                )
                if terr:
                    raise ValueError(
                        "trailing_tp_ratchet tier resolution failed: "
                        + "; ".join(terr)
                    )
                self._ratchet_tiers_run = tiers

            self._run_stop_loss_atr_mult = self._hl_fixed_atr_mult(lab) or None
            self._run_trailing_stop_atr_mult = None
            if _positive(self.trailing_stop_atr_mult):
                self._run_trailing_stop_atr_mult = self.trailing_stop_atr_mult
            elif self._hl_regime_mult(self._trailing_stop_regime_block, lab) > 0:
                self._run_trailing_stop_atr_mult = self._hl_regime_mult(
                    self._trailing_stop_regime_block, lab,
                )

        stop_needs_atr = self._hl_stop_geometry and self._stop_owner in STOP_OWNERS_NEEDING_ATR
        stop_needs_label = (self._hl_stop_geometry
                            and self._stop_owner in STOP_OWNERS_NEEDING_LABEL)
        stop_atr_seen = None
        if stop_needs_atr and atr_series is not None:
            _atr_vals = pd.to_numeric(atr_series, errors="coerce").to_numpy(dtype=float)
            stop_atr_seen = np.logical_or.accumulate(
                np.isfinite(_atr_vals) & (_atr_vals > 0)) if len(_atr_vals) else _atr_vals
        stop_label_seen = False
        stop_warmup_skipped_entries = 0
        stop_seed_dropped = False
        if stop_needs_label and not self.regime_enabled and "regime" not in df.columns:
            self._validate_stop_runtime(
                df.index[0] if len(df) else None,
                {"atr_regime_label": {"status": "missing", "source": "no_regime_label_source",
                                      "value": None}})

        def _stop_inputs_warming(idx) -> bool:
            if stop_needs_label and not stop_label_seen:
                return True
            if not stop_needs_atr:
                return False
            if stop_atr_seen is None:
                return True
            try:
                pos = int(atr_series.index.get_loc(idx))
            except (KeyError, TypeError, ValueError):
                return False
            return pos < 1 or not bool(stop_atr_seen[pos - 1])

        if starting_long and (stop_needs_atr or stop_needs_label):
            try:
                _seed_atr = float(starting_long.get("entry_atr", 0.0) or 0.0)
            except (TypeError, ValueError):
                _seed_atr = 0.0
            _seed_entry = float(starting_long["entry_price"])
            _recorded_seed = str(starting_long.get("entry_regime", "") or "").strip()
            if self._regime_label_columns:
                _seed_label = _recorded_seed or _row_close_protection_label(df.iloc[0])
            else:
                _seed_label = _recorded_seed or _entry_stamp(df.iloc[0])
            if (stop_needs_atr and not (0 < _seed_atr <= 0.5 * _seed_entry)) or (
                    stop_needs_label and not _seed_label):
                stop_seed_dropped = True
                starting_long = None
                print(f"[#1684] seeded position dropped: its {self._stop_owner} stop has no "
                      "entry ATR or regime label yet, so the run starts flat instead of "
                      "carrying an unprotected position.", file=sys.stderr)

        if starting_long:
            effective_entry = starting_long["entry_price"]
            entry_commission = self.initial_capital * self.commission_pct
            available = self.initial_capital - entry_commission
            position = available / effective_entry
            cash = 0.0
            current_trade = Trade(
                starting_long.get("entry_date", df.index[0]),
                effective_entry, "long",
            )
            current_trade.shares = position
            avg_cost = effective_entry
            initial_quantity = position
            scale.reset()
            scale.base_open_notional = position * effective_entry
            hold.open(effective_entry, "long", entry_commission)
            if rec is not None:
                rec.record(
                    "seed_inventory", bar=df.index[0], decision_bar=None,
                    timing="seeded_before_first_bar", side="long", action="buy",
                    quantity=position, raw_price=effective_entry,
                    effective_price=effective_entry, fee_rate=self.commission_pct,
                    fee_charged=entry_commission, reason="starting_long",
                    qty_before=0.0, qty_after=position, avg_cost_before=0.0,
                    avg_cost_after=avg_cost, cash_before=self.initial_capital,
                    cash_after=cash, hold=hold, synthetic=True,
                )
            seed_atr = starting_long.get("entry_atr", 0.0)
            try:
                seed_atr = float(seed_atr or 0.0)
            except (TypeError, ValueError):
                seed_atr = 0.0
            if seed_atr > 0 and seed_atr <= 0.5 * effective_entry:
                entry_atr_value = seed_atr
            recorded_seed = str(starting_long.get("entry_regime", "") or "").strip()
            if self._regime_label_columns and recorded_seed:
                stamp_open_from_label(recorded_seed, df.iloc[0], keep_recorded=True)
            elif self._regime_label_columns:
                stamp_open_from_label("", df.iloc[0], seed_row=True)
            else:
                stamp = recorded_seed or _entry_stamp(df.iloc[0])
                stamp_open_from_label(stamp, df.iloc[0])
            seed_hwm = starting_long.get("high_water", 0.0)
            try:
                seed_hwm = float(seed_hwm or 0.0)
            except (TypeError, ValueError):
                seed_hwm = 0.0
            hwm_anchor = max(effective_entry, seed_hwm)
            if self._hl_stop_geometry:
                sl_trigger_px, sl_high_water_px, _ = self._hl_arm_stop(
                    "long", avg_cost, entry_atr_value, self._run_position_regime,
                    hwm_anchor, high_water=hwm_anchor,
                    event_date=starting_long.get("entry_date", df.index[0]),
                )
            elif sl_after_active and self._run_tp_tier_thresholds:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                sl_high_water_px = 0.0
            elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                sl_trigger_px = _initial_trail_trigger(
                    "long", hwm_anchor, entry_atr_value,
                    self._run_trailing_stop_atr_mult,
                )
                sl_high_water_px = hwm_anchor
            else:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                    sl_trigger_px = _initial_trail_trigger(
                        "long", hwm_anchor, entry_atr_value,
                        self._run_trailing_stop_atr_mult,
                    )
                sl_high_water_px = hwm_anchor
            sl_tiers_processed = 0
            post_tp_trail_mult = None
            sl_pierce_armed = True

        profile_switcher = (
            _ProfileSwitcher(self._profile_alloc) if has_profile_alloc else None
        )
        active_profile = ""

        book_funding = "funding_accrual" in df.columns
        total_funding_pnl = 0.0
        execution_log: dict = {
            "rejected_entries": [],
            "skipped_partial_closes": [],
            "close_residuals": [],
            "entry_lot_residual_qty": 0.0,
        }

        has_entry_fraction = "_entry_fraction" in df.columns

        risk_mode = (self.risk_per_trade_pct or 0) > 0
        if risk_mode and has_entry_fraction:
            raise ValueError(
                "risk_per_trade_pct is mutually exclusive with a "
                "strategy-emitted entry_fraction column (#1268) — the live "
                "sizer has no entry_fraction input, so composing them would "
                "diverge from the live sizing formula"
            )
        risk_skipped_entries = 0

        if self.allow_scale_in and has_entry_fraction:
            raise ValueError(
                "allow_scale_in is mutually exclusive with a strategy-emitted "
                "entry_fraction column (#1276) — the live per-add sizing "
                "(the fresh-open notional) has no entry_fraction input, so "
                "composing them would diverge from the live add-sizing rule"
            )
        prev_close_arr = (
            df["close"].shift(1).to_numpy(dtype=float)
            if self.allow_scale_in else None
        )

        def _try_scale_in_add(i: int, side: str, fill_price: float) -> bool:
            nonlocal position, cash, avg_cost, initial_quantity
            nonlocal scale_in_adds_total, scale_in_added_notional_total
            cash_before, qty_before, avg_before = cash, position, avg_cost
            if hurst_blocked:
                return False
            decision_price = float(prev_close_arr[i])
            if not (decision_price > 0):
                return False
            default_notional = scale.base_open_notional
            add_qty, ok, _reason = _scale_in_decision(
                self._scale_in_cfg, side, abs(position), avg_cost,
                entry_atr_value, scale.scale_in_count,
                scale.added_notional_usd, scale.last_add_price,
                1 if side == "long" else -1, decision_price, default_notional,
            )
            if not ok or add_qty <= 0:
                return False
            add_qty *= hurst_size_mult
            if add_qty <= 0:
                return False
            if side == "long":
                eff = fill_price * (1 + self.slippage_pct)
            else:
                eff = fill_price * (1 - self.slippage_pct)
            notional = add_qty * eff
            commission = notional * self.commission_pct
            if side == "long":
                cash -= notional + commission
                position += add_qty
            else:
                cash += notional - commission
                position -= add_qty
            if scale.risk_anchor_price <= 0:
                scale.risk_anchor_price = avg_cost
            old_qty = abs(position) - add_qty
            new_qty = old_qty + add_qty
            avg_cost = (old_qty * avg_cost + add_qty * eff) / new_qty
            initial_quantity += add_qty
            scale.scale_in_count += 1
            scale.last_add_price = eff
            scale.added_notional_usd += notional
            scale_in_adds_total += 1
            scale_in_added_notional_total += notional
            if current_trade is not None:
                current_trade.entry_price = avg_cost
                current_trade.shares += add_qty
                current_trade.scale_in_adds = scale.scale_in_count
            hold.entry_fee += commission
            if rec is not None:
                rec.record(
                    "scale_in", bar=df.index[i],
                    decision_bar=df.index[i - 1] if i > 0 else None,
                    timing="bar_open_fill", side=side,
                    action="buy" if side == "long" else "sell",
                    quantity=add_qty, raw_price=fill_price, effective_price=eff,
                    fee_rate=self.commission_pct, fee_charged=commission,
                    reason="scale_in", qty_before=qty_before, qty_after=position,
                    avg_cost_before=avg_before, avg_cost_after=avg_cost,
                    cash_before=cash_before, cash_after=cash, hold=hold,
                )
            return True

        def _spec_entry_fill(side: str, raw_fill: float, budget: float, idx):
            spec = self._execution
            if side == "long":
                effective_price = raw_fill * (1 + self.slippage_pct)
            else:
                effective_price = raw_fill * (1 - self.slippage_pct)
            if not (effective_price > 0) or not (budget > 0):
                execution_log["rejected_entries"].append({
                    "date": str(idx), "side": side, "reason": "no_budget_or_price",
                    "requested_qty": 0.0, "floored_qty": 0.0, "notional_usd": 0.0,
                })
                return None
            requested_qty = budget / (1.0 + spec["taker_fee_pct"]) / effective_price
            qty = _hl_floor_lot_size(requested_qty, spec["size_decimals"])
            notional = qty * effective_price
            threshold = spec["min_notional_usd"] * (1.0 + spec["min_notional_margin"])
            reason = ""
            if qty <= 0:
                reason = "below_lot"
            elif notional < threshold:
                reason = "below_min_notional"
            if reason:
                execution_log["rejected_entries"].append({
                    "date": str(idx), "side": side, "reason": reason,
                    "requested_qty": requested_qty, "floored_qty": qty,
                    "notional_usd": notional, "threshold_usd": threshold,
                })
                return None
            execution_log["entry_lot_residual_qty"] += requested_qty - qty
            return effective_price, qty, notional * spec["taker_fee_pct"]

        def _book_close(idx, close_fraction: float, raw_fill: float, slippage: float,
                        reason: str, bar_mark: float, seed_price: float,
                        fee_pct: Optional[float] = None, decision_bar=None,
                        timing: str = "bar_open_fill") -> bool:
            nonlocal position, cash, avg_cost, initial_quantity, entry_atr_value
            nonlocal current_trade, sl_trigger_px, sl_tiers_processed
            nonlocal post_tp_trail_mult, sl_high_water_px
            sl_after_moved = False
            cash_before, qty_before, avg_before = cash, position, avg_cost
            gross_realized = None
            entry_fee_allocated = None
            fee_rate = self.commission_pct if fee_pct is None else fee_pct
            qty_to_close = abs(position) * min(close_fraction, 1.0)
            if self._execution is not None and close_fraction < 1.0:
                spec = self._execution
                floored = _hl_floor_lot_size(qty_to_close, spec["size_decimals"])
                notional = floored * raw_fill
                threshold = spec["min_notional_usd"] * (1.0 + spec["min_notional_margin"])
                if floored <= 0 or notional < threshold:
                    execution_log["skipped_partial_closes"].append({
                        "date": str(idx), "reason": reason or "close_strategy",
                        "gate": "below_lot" if floored <= 0 else "below_min_notional",
                        "requested_qty": qty_to_close, "floored_qty": floored,
                        "notional_usd": notional, "threshold_usd": threshold,
                    })
                    return False
                if floored < qty_to_close:
                    execution_log["close_residuals"].append({
                        "date": str(idx), "requested_qty": qty_to_close,
                        "floored_qty": floored,
                        "residual_qty": qty_to_close - floored,
                    })
                qty_to_close = floored
            if position > 0:
                effective_price = raw_fill * (1 - slippage)
                proceeds = qty_to_close * effective_price
                commission = proceeds * fee_rate
                cash += proceeds - commission
                position -= qty_to_close
            else:
                effective_price = raw_fill * (1 + slippage)
                cost = qty_to_close * effective_price
                commission = cost * fee_rate
                cash -= cost + commission
                position += qty_to_close

            if current_trade:
                closed = Trade(current_trade.entry_date, current_trade.entry_price, current_trade.side)
                closed.shares = qty_to_close
                closed.close(idx, effective_price)
                gross_realized = closed.pnl
                qty_frac = (qty_to_close / initial_quantity) if initial_quantity > 0 else 1.0
                _stamp_hold(closed, hold, entry_atr=entry_atr_value,
                            exit_fee=commission,
                            reason=reason or "close_strategy",
                            qty_frac=qty_frac,
                            true_up_entry_fee=(
                                scale.scale_in_count > 0
                                and abs(position) <= 1e-12
                            ))
                entry_fee_allocated = closed.entry_fee
                closed.scale_in_adds = scale.scale_in_count
                trades.append(closed)
                current_trade.shares -= qty_to_close
                if current_trade.shares <= 1e-12:
                    current_trade = None

            if abs(position) <= 1e-12:
                position = 0.0
                avg_cost = 0.0
                initial_quantity = 0.0
                entry_atr_value = 0.0
                scale.reset()
                sl_trigger_px = 0.0
                sl_tiers_processed = 0
                post_tp_trail_mult = None
                sl_high_water_px = 0.0
                sl_after_moved = False
                self._active_sl_after_rules = self._sl_after_rules_static
                self._run_tp_tier_thresholds = list(
                    self._tp_tier_thresholds_static,
                )
                self._run_stop_loss_atr_mult = None
                self._run_trailing_stop_atr_mult = None
                self._clear_position_stamps()
            elif sl_after_active and self._run_tp_tier_thresholds:
                side_now = "long" if position > 0 else "short"
                prev_trigger = sl_trigger_px
                prev_post_tp_trail = post_tp_trail_mult
                sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, \
                    sl_high_water_px = self._maybe_apply_sl_after(
                        side=side_now,
                        avg_cost=scale.geom_cost(avg_cost),
                        entry_atr=entry_atr_value,
                        position_qty=abs(position),
                        initial_qty=initial_quantity,
                        mark_price=bar_mark,
                        fill_price=seed_price,
                        sl_trigger_px=sl_trigger_px,
                        sl_tiers_processed=sl_tiers_processed,
                        post_tp_trail_mult=post_tp_trail_mult,
                        sl_high_water_px=sl_high_water_px,
                        event_date=idx,
                    )
                if (
                    sl_trigger_px != prev_trigger
                    or post_tp_trail_mult != prev_post_tp_trail
                ):
                    sl_after_moved = True

            if rec is not None:
                rec.record(
                    "close", bar=idx, decision_bar=decision_bar, timing=timing,
                    side="long" if qty_before > 0 else "short",
                    action="sell" if qty_before > 0 else "buy",
                    quantity=qty_to_close, raw_price=raw_fill,
                    effective_price=effective_price, fee_rate=fee_rate,
                    fee_charged=commission, reason=reason or "close_strategy",
                    qty_before=qty_before, qty_after=position,
                    avg_cost_before=avg_before, avg_cost_after=avg_cost,
                    cash_before=cash_before, cash_after=cash, hold=hold,
                    gross_realized=gross_realized,
                    entry_fee_allocated=entry_fee_allocated,
                )
            return sl_after_moved

        for i, (idx, row) in enumerate(df.iterrows()):
            decision_idx = df.index[i - 1] if i > 0 else None
            fill_price = row["open"] if has_open else row["close"]
            mark_price = row["close"]
            signal = row["signal"]
            entry_fraction = (
                float(row["_entry_fraction"]) if has_entry_fraction else 1.0
            )
            risk_entry_blocked = False
            if stop_needs_label and _entry_stamp(row):
                stop_label_seen = True
            if (stop_needs_atr or stop_needs_label) and position == 0 \
                    and _stop_inputs_warming(idx):
                risk_entry_blocked = True
                entry_wanted = (
                    str(row.get("_open_action", "none")) in ("long", "short")
                    if uses_open_close
                    else int(signal) != 0
                )
                if entry_wanted:
                    if not stop_warmup_skipped_entries:
                        print(f"[#1684] entry skipped at {idx}: the {self._stop_owner} stop "
                              "has no entry ATR or regime label history yet (indicator "
                              "warm-up); live would hold that history. Further warm-up "
                              "skips are counted silently.", file=sys.stderr)
                    stop_warmup_skipped_entries += 1
            if risk_mode:
                risk_fraction = self._risk_entry_fraction(
                    atr_series, idx, fill_price,
                )
                if risk_fraction is None:
                    risk_entry_blocked = True
                    entry_wanted = (
                        str(row.get("_open_action", "none")) in ("long", "short")
                        if uses_open_close
                        else int(signal) != 0
                    )
                    if position == 0 and entry_wanted:
                        risk_skipped_entries += 1
                        if not self._risk_skip_warned:
                            self._risk_skip_warned = True
                            print(
                                f"[#1268] risk_per_trade_pct: entry skipped at "
                                f"{idx} — stop distance unresolvable (no usable "
                                f"ATR at the signal bar); fail-closed, matching "
                                f"live. Further skips counted silently.",
                                file=sys.stderr,
                            )
                else:
                    entry_fraction = risk_fraction
            if profile_switcher is not None:
                active_profile = profile_switcher.step(
                    str(row.get("_profile_label", "") or ""), position == 0
                )
                signal = row["signal__" + active_profile]

            bar_regime = str(row.get("regime", "")) if self.regime_enabled else ""
            if self._regime_label_columns:
                if self._regime_label_columns.get("gate_unshifted"):
                    gate_label = str(row.get("_regime_gate_close", "") or "")
                else:
                    gate_label = str(row.get("_regime_gate", "") or "")
                if str(self._regime_label_columns.get("directional") or "").strip():
                    current_directional = str(row.get("_regime_directional_close", "") or "")
                else:
                    current_directional = str(row.get("_regime_gate_close", "") or "")
                position_directional = self._stamp_directional
            else:
                gate_label = bar_regime
                current_directional = bar_regime
                position_directional = self._run_position_regime
            effective_direction = self.direction or ""
            effective_invert = self.invert_signal
            plain_short_for_bar = plain_short
            if self.regime_directional_policy is not None:
                effective_direction, effective_invert = self._effective_directional_entry(
                    current_directional,
                    position_directional,
                    abs(position),
                )
                if not uses_open_close:
                    signal = _apply_direction_invert_value(
                        int(signal),
                        uses_open_close=False,
                        direction=effective_direction,
                        invert_signal=effective_invert,
                    )
                    plain_short_for_bar = effective_direction == "short"

            sl_after_just_applied = False
            ratchet_tightened = False

            if book_funding and position != 0:
                accrual = row.get("funding_accrual", 0.0)
                accrual = float(accrual) if accrual == accrual else 0.0
                if accrual != 0.0:
                    funding_cash = -position * mark_price * accrual
                    cash_before = cash
                    cash += funding_cash
                    total_funding_pnl += funding_cash
                    if rec is not None:
                        rec.record(
                            "funding", bar=idx, decision_bar=None,
                            timing="bar_mark_accrual",
                            side="long" if position > 0 else "short",
                            action="funding", quantity=abs(position),
                            raw_price=mark_price, effective_price=mark_price,
                            fee_rate=0.0, fee_charged=0.0, reason="funding",
                            qty_before=position, qty_after=position,
                            avg_cost_before=avg_cost, avg_cost_after=avg_cost,
                            cash_before=cash_before, cash_after=cash, hold=hold,
                            funding_cash=funding_cash, funding_rate=accrual,
                        )

            equity = cash + position * mark_price
            equity_curve.append({"date": idx, "equity": equity})

            regime_blocked = (
                self.regime_enabled
                and bool(self.allowed_regimes)
                and not _regime_allows_entry(
                    self.allowed_regimes, gate_label, self.regime_gate_on_failure
                )
            )

            hurst_size_mult = 1.0
            hurst_blocked = False
            if hurst_runner is not None:
                hurst_h = row.get("_hurst")
                hurst_blocked, hurst_size_mult = hurst_runner.step(
                    hurst_h, position == 0
                )
                if hurst_blocked:
                    regime_blocked = True
                if hurst_size_mult != 1.0:
                    entry_fraction *= hurst_size_mult

            if uses_open_close:
                col_close_fraction = float(row.get("_close_fraction", 0.0))
                if col_close_fraction >= pending_close_fraction:
                    close_fraction = col_close_fraction
                    close_reason = "column_close_fraction" if col_close_fraction > 0 else ""
                else:
                    close_fraction = pending_close_fraction
                    close_reason = pending_close_reason
                pending_close_fraction = 0.0
                pending_close_reason = ""
                if profile_switcher is not None:
                    open_action = row.get("_open_action__" + active_profile, "none")
                else:
                    open_action = row.get("_open_action", "none")
                raw_open_signal = (
                    _signal_from_open_action(open_action)
                    if self.regime_directional_policy is not None
                    else 0
                )

                if close_fraction > 0 and position != 0:
                    if _book_close(idx, close_fraction, fill_price, self.slippage_pct,
                                   close_reason, mark_price, fill_price,
                                   decision_bar=decision_idx):
                        sl_after_just_applied = True
                if self.regime_directional_policy is not None:
                    entry_direction, entry_invert = self._effective_directional_entry(
                        current_directional,
                        position_directional,
                        abs(position),
                    )
                    open_action = _open_action_from_signal(
                        _apply_direction_invert_value(
                            raw_open_signal,
                            uses_open_close=True,
                            direction=entry_direction,
                            invert_signal=entry_invert,
                        )
                    )

                long_entry_ok = (
                    open_action == "long" and position == 0 and cash > 0
                    and not regime_blocked and not risk_entry_blocked
                )
                short_entry_ok = (
                    open_action == "short" and position == 0 and cash > 0
                    and not regime_blocked and not risk_entry_blocked
                )
                spec_fill = None
                if self._execution is not None and (long_entry_ok or short_entry_ok):
                    spec_fill = _spec_entry_fill(
                        "long" if long_entry_ok else "short",
                        fill_price, cash * entry_fraction, idx,
                    )
                    if spec_fill is None:
                        long_entry_ok = False
                        short_entry_ok = False
                cash_before, qty_before, avg_before = cash, position, avg_cost
                if long_entry_ok:
                    if spec_fill is not None:
                        effective_price, shares, commission = spec_fill
                        position = shares
                        cash -= shares * effective_price + commission
                    else:
                        effective_price = fill_price * (1 + self.slippage_pct)
                        invest = cash * entry_fraction
                        commission = invest * self.commission_pct
                        available = invest - commission
                        shares = available / effective_price
                        position = shares
                        cash -= invest

                    current_trade = Trade(idx, effective_price, "long")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "long", commission)
                    if rec is not None:
                        rec.record(
                            'open', bar=idx, decision_bar=decision_idx,
                            timing='bar_open_fill', side="long", action="buy",
                            quantity=shares, raw_price=fill_price, effective_price=effective_price,
                            fee_rate=self.commission_pct, fee_charged=commission, reason="open_long",
                            qty_before=qty_before, qty_after=position,
                            avg_cost_before=avg_before, avg_cost_after=avg_cost,
                            cash_before=cash_before, cash_after=cash, hold=hold,
                            gross_realized=None, entry_fee_allocated=None,
                        )
                    scale.reset()
                    scale.base_open_notional = _ungated_leg_notional(
                        shares * effective_price, hurst_size_mult,
                    )
                    stamp_open_from_label(_entry_stamp(row), row)
                    if self._hl_stop_geometry:
                        sl_trigger_px, sl_high_water_px, sl_pierce_armed = self._hl_arm_stop(
                            "long", avg_cost, entry_atr_value, self._run_position_regime,
                            mark_price, event_date=idx,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                    elif sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "long", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "long", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif short_entry_ok:
                    if spec_fill is not None:
                        effective_price, shares, commission = spec_fill
                        cash += shares * effective_price - commission
                        position = -shares
                    else:
                        effective_price = fill_price * (1 - self.slippage_pct)
                        margin = cash * entry_fraction
                        commission = margin * self.commission_pct
                        notional = margin - commission
                        shares = notional / effective_price
                        cash += 2 * notional - margin
                        position = -shares

                    current_trade = Trade(idx, effective_price, "short")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "short", commission)
                    if rec is not None:
                        rec.record(
                            'open', bar=idx, decision_bar=decision_idx,
                            timing='bar_open_fill', side="short", action="sell",
                            quantity=shares, raw_price=fill_price, effective_price=effective_price,
                            fee_rate=self.commission_pct, fee_charged=commission, reason="open_short",
                            qty_before=qty_before, qty_after=position,
                            avg_cost_before=avg_before, avg_cost_after=avg_cost,
                            cash_before=cash_before, cash_after=cash, hold=hold,
                            gross_realized=None, entry_fee_allocated=None,
                        )
                    scale.reset()
                    scale.base_open_notional = _ungated_leg_notional(
                        shares * effective_price, hurst_size_mult,
                    )
                    stamp_open_from_label(_entry_stamp(row), row)
                    if self._hl_stop_geometry:
                        sl_trigger_px, sl_high_water_px, sl_pierce_armed = self._hl_arm_stop(
                            "short", avg_cost, entry_atr_value, self._run_position_regime,
                            mark_price, event_date=idx,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                    elif sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "short", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "short", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif (
                    self.allow_scale_in
                    and open_action == "long"
                    and position > 0
                ):
                    _try_scale_in_add(i, "long", fill_price)
                elif (
                    self.allow_scale_in
                    and open_action == "short"
                    and position < 0
                ):
                    _try_scale_in_add(i, "short", fill_price)

                if position != 0:
                    hold.step(
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                    )

                if (
                    walk_mode
                    and position != 0
                    and sl_pierce_armed
                    and sl_trigger_px > 0
                    and not sl_after_just_applied
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    raw_fill = self._intrabar_sl_fill(
                        side_now,
                        float(row["open"]) if has_open else mark_price,
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                        sl_trigger_px,
                    )
                    if raw_fill is not None:
                        cash_before, qty_before, avg_before = cash, position, avg_cost
                        gross_realized = None
                        entry_fee_allocated = None
                        qty_to_close = abs(position)
                        if position > 0:
                            effective_price = raw_fill * (1 - self.slippage_pct)
                            proceeds = qty_to_close * effective_price
                            commission = proceeds * self.commission_pct
                            cash += proceeds - commission
                        else:
                            effective_price = raw_fill * (1 + self.slippage_pct)
                            cost = qty_to_close * effective_price
                            commission = cost * self.commission_pct
                            cash -= cost + commission
                        position = 0.0
                        if current_trade:
                            closed = Trade(
                                current_trade.entry_date,
                                current_trade.entry_price,
                                current_trade.side,
                            )
                            closed.shares = qty_to_close
                            closed.close(idx, effective_price)
                            gross_realized = closed.pnl
                            qty_frac = (
                                qty_to_close / initial_quantity
                                if initial_quantity > 0 else 1.0
                            )
                            _stamp_hold(closed, hold,
                                        entry_atr=entry_atr_value,
                                        exit_fee=commission, reason="sl",
                                        qty_frac=qty_frac,
                                        true_up_entry_fee=(
                                            scale.scale_in_count > 0
                                        ))
                            entry_fee_allocated = closed.entry_fee
                            closed.scale_in_adds = scale.scale_in_count
                            trades.append(closed)
                            current_trade = None
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        scale.reset()
                        sl_trigger_px = 0.0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                        sl_pierce_armed = False
                        self._active_sl_after_rules = self._sl_after_rules_static
                        self._run_tp_tier_thresholds = list(
                            self._tp_tier_thresholds_static,
                        )
                        self._run_stop_loss_atr_mult = None
                        self._run_trailing_stop_atr_mult = None
                        self._clear_position_stamps()
                        if rec is not None:
                            rec.record(
                                'close', bar=idx, decision_bar=None,
                                timing='intrabar_trigger_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                                quantity=qty_to_close, raw_price=raw_fill, effective_price=effective_price,
                                fee_rate=self.commission_pct, fee_charged=commission, reason="sl",
                                qty_before=qty_before, qty_after=position,
                                avg_cost_before=avg_before, avg_cost_after=avg_cost,
                                cash_before=cash_before, cash_after=cash, hold=hold,
                                gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                            )

                if self.close_strategies and position != 0 and avg_cost > 0:
                    pending_close_fraction, pending_close_reason, tier_fill_price = self._evaluate_close_strategies(
                        position, scale.geom_cost(avg_cost), initial_quantity,
                        entry_atr_value,
                        mark_price, atr_series, idx,
                        position_regime=self._run_position_regime,
                        market_regime=_bar_close_regime(row),
                        bars_held=hold.bars,
                        zscore_series=zscore_series,
                        avwap_series=avwap_series,
                    )
                    if (
                        self._resting_tp_model
                        and pending_close_fraction > 0
                        and tier_fill_price > 0
                    ):
                        if _book_close(idx, pending_close_fraction, tier_fill_price, 0.0,
                                       pending_close_reason, mark_price, mark_price,
                                       fee_pct=self._maker_fee_pct,
                                       timing="intrabar_trigger_fill"):
                            sl_after_just_applied = True
                        pending_close_fraction = 0.0
                        pending_close_reason = ""
                    if (
                        trailing_ratchet_active
                        and self._ratchet_mod
                        and self._ratchet_tiers_run
                        and position != 0
                        and entry_atr_value > 0
                    ):
                        side_now = "long" if position > 0 else "short"
                        ratchet_prev_trail = post_tp_trail_mult
                        base_trail = self._run_trailing_stop_atr_mult or 0.0
                        sl_tiers_processed, post_tp_trail_mult = (
                            self._ratchet_mod.maybe_apply_mark_ratchet(
                                self._ratchet_tiers_run,
                                watermark=sl_tiers_processed,
                                mark_price=mark_price,
                                avg_cost=scale.geom_cost(avg_cost),
                                entry_atr=entry_atr_value,
                                side=side_now,
                                post_tp_trail_mult=post_tp_trail_mult,
                                trailing_stop_atr_mult=base_trail,
                            )
                        )
                        ratchet_tightened = post_tp_trail_mult != ratchet_prev_trail

                scalar_stop_active = (
                    (self._run_stop_loss_atr_mult or 0) > 0
                    or (self._run_trailing_stop_atr_mult or 0) > 0
                    or (self.stop_loss_pct or 0) > 0
                )
                if (
                    self._hl_stop_geometry
                    and not sl_after_just_applied
                    and position != 0
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    sl_trigger_px, sl_high_water_px = self._hl_trail_step(
                        side_now, scale.geom_cost(avg_cost), entry_atr_value,
                        self._run_position_regime, mark_price, post_tp_trail_mult,
                        sl_trigger_px, sl_high_water_px, ratchet_tightened,
                        event_date=idx,
                    )
                    if not walk_mode and sl_trigger_px > 0 and self._sl_hit(
                        side_now, mark_price, sl_trigger_px,
                    ):
                        pending_close_fraction = 1.0
                        pending_close_reason = "sl"
                elif (
                    (sl_after_active or trailing_ratchet_active
                     or scalar_stop_active)
                    and not sl_after_just_applied
                    and position != 0
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    trail_mult = post_tp_trail_mult
                    if trail_mult is None or trail_mult <= 0:
                        trail_mult = self._run_trailing_stop_atr_mult
                    if (
                        trail_mult is not None
                        and trail_mult > 0
                        and entry_atr_value > 0
                    ):
                        sl_trigger_px, sl_high_water_px = self._walk_trail(
                            side=side_now,
                            mark_price=mark_price,
                            entry_atr=entry_atr_value,
                            trail_mult=trail_mult,
                            sl_trigger_px=sl_trigger_px,
                            sl_high_water_px=sl_high_water_px,
                        )
                    if not walk_mode and sl_trigger_px > 0 and self._sl_hit(
                        side_now, mark_price, sl_trigger_px,
                    ):
                        pending_close_fraction = 1.0
                        pending_close_reason = "sl"
                if position != 0:
                    sl_pierce_armed = True
                continue

            if pending_signal_sl_close and position > 0:
                cash_before, qty_before, avg_before = cash, position, avg_cost
                gross_realized = None
                entry_fee_allocated = None
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    gross_realized = current_trade.pnl
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    entry_fee_allocated = current_trade.entry_fee
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._clear_position_stamps()
                if rec is not None:
                    rec.record(
                        'close', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                        quantity=abs(qty_before), raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="signal_sl",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                    )
                continue

            if pending_signal_sl_close and position < 0:
                cash_before, qty_before, avg_before = cash, position, avg_cost
                gross_realized = None
                entry_fee_allocated = None
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    gross_realized = current_trade.pnl
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    entry_fee_allocated = current_trade.entry_fee
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._clear_position_stamps()
                if rec is not None:
                    rec.record(
                        'close', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                        quantity=abs(qty_before), raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="signal_sl",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                    )
                continue

            cash_before, qty_before, avg_before = cash, position, avg_cost
            gross_realized = None
            entry_fee_allocated = None
            if plain_short_for_bar and signal == -1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                effective_price = fill_price * (1 - self.slippage_pct)
                margin = cash * entry_fraction
                commission = margin * self.commission_pct
                notional = margin - commission
                shares = notional / effective_price
                cash += 2 * notional - margin
                position = -shares

                current_trade = Trade(idx, effective_price, "short")
                current_trade.shares = shares
                scale.reset()
                scale.base_open_notional = _ungated_leg_notional(
                    shares * effective_price, hurst_size_mult,
                )

                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "short", commission)
                if rec is not None:
                    rec.record(
                        'open', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="short", action="sell",
                        quantity=shares, raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="open_short",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=None, entry_fee_allocated=None,
                    )
                stamp_open_from_label(_entry_stamp(row), row)
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                sl_pierce_armed = False
                if self._hl_stop_geometry:
                    sl_trigger_px, sl_high_water_px, sl_pierce_armed = self._hl_arm_stop(
                        "short", avg_cost, entry_atr_value, self._run_position_regime,
                        mark_price, event_date=idx,
                    )
                elif (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost + self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price + self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 + self.stop_loss_pct)
                    sl_pierce_armed = True

            elif plain_short_for_bar and signal == 1 and position < 0:
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    gross_realized = current_trade.pnl
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    entry_fee_allocated = current_trade.entry_fee
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._clear_position_stamps()
                if rec is not None:
                    rec.record(
                        'close', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                        quantity=abs(qty_before), raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="signal",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                    )

            elif not plain_short_for_bar and signal == 1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                effective_price = fill_price * (1 + self.slippage_pct)
                invest = cash * entry_fraction
                commission = invest * self.commission_pct
                available = invest - commission
                shares = available / effective_price
                position = shares
                cash -= invest

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares
                scale.reset()
                scale.base_open_notional = _ungated_leg_notional(
                    shares * effective_price, hurst_size_mult,
                )

                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "long", commission)
                if rec is not None:
                    rec.record(
                        'open', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="long", action="buy",
                        quantity=shares, raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="open_long",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=None, entry_fee_allocated=None,
                    )
                stamp_open_from_label(_entry_stamp(row), row)
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                sl_pierce_armed = False
                if self._hl_stop_geometry:
                    sl_trigger_px, sl_high_water_px, sl_pierce_armed = self._hl_arm_stop(
                        "long", avg_cost, entry_atr_value, self._run_position_regime,
                        mark_price, event_date=idx,
                    )
                elif (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost - self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price - self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 - self.stop_loss_pct)
                    sl_pierce_armed = True

            elif signal == -1 and position > 0:
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    gross_realized = current_trade.pnl
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    entry_fee_allocated = current_trade.entry_fee
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._clear_position_stamps()
                if rec is not None:
                    rec.record(
                        'close', bar=idx, decision_bar=decision_idx,
                        timing='bar_open_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                        quantity=abs(qty_before), raw_price=fill_price, effective_price=effective_price,
                        fee_rate=self.commission_pct, fee_charged=commission, reason="signal",
                        qty_before=qty_before, qty_after=position,
                        avg_cost_before=avg_before, avg_cost_after=avg_cost,
                        cash_before=cash_before, cash_after=cash, hold=hold,
                        gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                    )

            elif (
                self.allow_scale_in
                and not plain_short_for_bar
                and signal == 1
                and position > 0
            ):
                _try_scale_in_add(i, "long", fill_price)
            elif (
                self.allow_scale_in
                and plain_short_for_bar
                and signal == -1
                and position < 0
            ):
                _try_scale_in_add(i, "short", fill_price)

            if position != 0:
                hold.step(
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                )

            if (
                walk_mode
                and position != 0
                and sl_pierce_armed
                and sl_trigger_px > 0
            ):
                side_now = "long" if position > 0 else "short"
                raw_fill = self._intrabar_sl_fill(
                    side_now,
                    float(row["open"]) if has_open else mark_price,
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                    sl_trigger_px,
                )
                if raw_fill is not None:
                    cash_before, qty_before, avg_before = cash, position, avg_cost
                    gross_realized = None
                    entry_fee_allocated = None
                    if position > 0:
                        effective_price = raw_fill * (1 - self.slippage_pct)
                        proceeds = position * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                    else:
                        effective_price = raw_fill * (1 + self.slippage_pct)
                        cost = abs(position) * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                    position = 0.0
                    if current_trade:
                        current_trade.close(idx, effective_price)
                        gross_realized = current_trade.pnl
                        _stamp_hold(current_trade, hold,
                                    entry_atr=entry_atr_value,
                                    exit_fee=commission, reason="signal_sl")
                        entry_fee_allocated = current_trade.entry_fee
                        current_trade.scale_in_adds = scale.scale_in_count
                        trades.append(current_trade)
                        current_trade = None
                    sl_trigger_px = 0.0
                    avg_cost = 0.0
                    entry_atr_value = 0.0
                    sl_high_water_px = 0.0
                    sl_pierce_armed = False
                    scale.reset()
                    self._clear_position_stamps()
                    if rec is not None:
                        rec.record(
                            'close', bar=idx, decision_bar=None,
                            timing='intrabar_trigger_fill', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                            quantity=abs(qty_before), raw_price=raw_fill, effective_price=effective_price,
                            fee_rate=self.commission_pct, fee_charged=commission, reason="signal_sl",
                            qty_before=qty_before, qty_after=position,
                            avg_cost_before=avg_before, avg_cost_after=avg_cost,
                            cash_before=cash_before, cash_after=cash, hold=hold,
                            gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                        )

            if self._hl_stop_geometry and position != 0 and avg_cost > 0:
                side_now = "long" if position > 0 else "short"
                sl_trigger_px, sl_high_water_px = self._hl_trail_step(
                    side_now, scale.geom_cost(avg_cost), entry_atr_value,
                    self._run_position_regime, mark_price, None,
                    sl_trigger_px, sl_high_water_px, False, event_date=idx,
                )
                if not walk_mode and self._sl_hit(side_now, mark_price, sl_trigger_px):
                    pending_signal_sl_close = True
            elif position > 0 and sl_trigger_px > 0:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price > sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px - self.trailing_stop_atr_mult * entry_atr_value
                    if candidate > sl_trigger_px:
                        sl_trigger_px = candidate
                if not walk_mode and self._sl_hit("long", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True
            elif position < 0 and sl_trigger_px > 0:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price < sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px + self.trailing_stop_atr_mult * entry_atr_value
                    if candidate < sl_trigger_px:
                        sl_trigger_px = candidate
                if not walk_mode and self._sl_hit("short", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True

            if position != 0:
                sl_pierce_armed = True

        if rec is not None:
            rec.mark_interval_end(bar=df.index[-1], cash=cash, position=position,
                                  avg_cost=avg_cost, hold=hold)
        if position != 0:
            cash_before, qty_before, avg_before = cash, position, avg_cost
            gross_realized = None
            entry_fee_allocated = None
            if position > 0:
                final_price = df["close"].iloc[-1] * (1 - self.slippage_pct)
                proceeds = position * final_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
            else:
                final_price = df["close"].iloc[-1] * (1 + self.slippage_pct)
                cost = abs(position) * final_price
                commission = cost * self.commission_pct
                cash -= cost + commission
            position = 0.0

            if current_trade:
                current_trade.close(df.index[-1], final_price)
                gross_realized = current_trade.pnl
                eod_qty_frac = (
                    current_trade.shares / initial_quantity
                    if initial_quantity > 0 else 1.0
                )
                _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                            exit_fee=commission, reason="end_of_data",
                            qty_frac=eod_qty_frac,
                            true_up_entry_fee=scale.scale_in_count > 0)
                entry_fee_allocated = current_trade.entry_fee
                current_trade.scale_in_adds = scale.scale_in_count
                trades.append(current_trade)
            if rec is not None:
                rec.record(
                    'terminal_liquidation', bar=df.index[-1], decision_bar=None,
                    timing='terminal_mark', side="long" if qty_before > 0 else "short", action="sell" if qty_before > 0 else "buy",
                    quantity=abs(qty_before), raw_price=df["close"].iloc[-1], effective_price=final_price,
                    fee_rate=self.commission_pct, fee_charged=commission, reason="end_of_data",
                    qty_before=qty_before, qty_after=position,
                    avg_cost_before=avg_before, avg_cost_after=avg_cost,
                    cash_before=cash_before, cash_after=cash, hold=hold,
                    gross_realized=gross_realized, entry_fee_allocated=entry_fee_allocated,
                    synthetic=True,
                )

        final_equity = cash
        equity_df = pd.DataFrame(equity_curve).set_index("date")

        metrics = self._calculate_metrics(equity_df, trades, df, timeframe)
        open_ref = dict(self.open_strategy) if self.open_strategy else {}
        if not open_ref.get("name") and strategy_name:
            open_ref["name"] = strategy_name
        if "params" not in open_ref and params:
            open_ref["params"] = dict(params)
        metrics.update({
            "strategy_name": open_ref.get("name") or strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "start_date": str(df.index[0]),
            "end_date": str(df.index[-1]),
            "initial_capital": self.initial_capital,
            "final_capital": round(final_equity, 2),
            "total_funding_pnl": round(total_funding_pnl, 4),
            "params": open_ref.get("params") or params or {},
            "open_strategy": open_ref,
            "close_strategies": [dict(r) for r in self._close_refs],
            "close_validation": self._close_validation.to_dict(),
            "trades": [t.to_dict() for t in trades],
        })
        if self._execution is not None:
            metrics["execution"] = {
                "spec": dict(self._execution),
                "combined_adverse_price_pct": self.slippage_pct,
                "rejected_entry_count": len(execution_log["rejected_entries"]),
                "skipped_partial_close_count": len(execution_log["skipped_partial_closes"]),
                "close_residual_qty": sum(
                    r["residual_qty"] for r in execution_log["close_residuals"]
                ),
                "entry_lot_residual_qty": execution_log["entry_lot_residual_qty"],
                "rejected_entries": execution_log["rejected_entries"],
                "skipped_partial_closes": execution_log["skipped_partial_closes"],
                "close_residuals": execution_log["close_residuals"],
            }
        if stop_warmup_skipped_entries:
            metrics["stop_warmup_skipped_entries"] = stop_warmup_skipped_entries
        if stop_seed_dropped:
            metrics["stop_seed_dropped"] = True
        if risk_mode:
            metrics["risk_per_trade_pct"] = self.risk_per_trade_pct
            metrics["risk_sizing_skipped_entries"] = risk_skipped_entries
        if self.allow_scale_in:
            metrics["scale_in_adds"] = scale_in_adds_total
            metrics["scale_in_added_notional_usd"] = round(
                scale_in_added_notional_total, 6,
            )

        if save:
            store_backtest_result(metrics)
        if rec is not None:
            metrics["ledger_events"] = rec.envelope()

        return metrics

    def _risk_entry_fraction(self, atr_series: Optional[pd.Series], idx,
                             price: float) -> Optional[float]:
        pct = float(self.risk_per_trade_pct or 0)
        if pct <= 0 or price <= 0:
            return None
        dist = None
        kind, field_name = self._risk_owner
        if kind == "atr":
            atr = self._stamp_entry_atr(atr_series, idx, price)
            if atr <= 0:
                return None
            dist = float(getattr(self, field_name)) * atr
        elif kind == "pct":
            dist = price * float(getattr(self, field_name))
        if dist is None or dist <= 0:
            return None
        fraction = (pct / 100.0) * price / dist
        if fraction > 1.0:
            if not self._risk_cap_warned:
                self._risk_cap_warned = True
                print(
                    f"[#1268] risk_per_trade_pct: risk-derived notional "
                    f"exceeds available cash at {idx} (fraction "
                    f"{fraction:.2f} capped at 1.0) — the backtester models "
                    f"no leverage, so live would size up to cash × "
                    f"exchange_leverage here. Further caps applied silently.",
                    file=sys.stderr,
                )
            fraction = 1.0
        return fraction

    def _stamp_entry_atr(self, atr_series: Optional[pd.Series], idx,
                         entry_price: float) -> float:
        if atr_series is None or entry_price <= 0:
            return 0.0
        try:
            pos = int(atr_series.index.get_loc(idx))
        except (KeyError, TypeError, ValueError):
            return 0.0
        if pos < 1:
            return 0.0
        try:
            value = float(atr_series.iloc[pos - 1])
        except (TypeError, ValueError):
            return 0.0
        if not (value > 0):
            return 0.0
        if value > 0.5 * entry_price:
            return 0.0
        return value

    def _close_names_include_avwap_stop(self) -> bool:
        from strategy_composition import close_names_include_avwap_stop
        return close_names_include_avwap_stop(self.close_strategies)

    def _evaluate_close_strategies(self, position: float, avg_cost: float,
                                   initial_quantity: float,
                                   entry_atr_value: float,
                                   mark_price: float,
                                   atr_series: Optional[pd.Series],
                                   idx,
                                   *,
                                   position_regime: str = "",
                                   market_regime: str = "",
                                   bars_held: int = 0,
                                   zscore_series: Optional[pd.Series] = None,
                                   avwap_series: Optional[pd.Series] = None
                                   ) -> Tuple[float, str, float]:
        evaluate, _list_strategies = _load_close_registry()
        side = "long" if position > 0 else "short"
        position_dict = {
            "side": side,
            "avg_cost": float(avg_cost),
            "risk_anchor_price": float(avg_cost),
            "current_quantity": float(abs(position)),
            "initial_quantity": float(initial_quantity or abs(position)),
            "entry_atr": float(entry_atr_value),
            "regime": str(position_regime or ""),
            "bars_held": int(bars_held),
        }
        if self._resting_tp_model:
            position_dict["tp_model"] = "resting_limit"
        market_dict = {
            "mark_price": float(mark_price),
            "regime": str(market_regime or ""),
        }
        if atr_series is not None:
            try:
                live_atr = float(atr_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                live_atr = 0.0
            if live_atr > 0:
                market_dict["atr"] = live_atr

        if zscore_series is not None:
            try:
                z = float(zscore_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                z = float("nan")
            if z == z:
                market_dict["zscore"] = z

        if avwap_series is not None:
            try:
                avwap_value = float(avwap_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                avwap_value = float("nan")
            if avwap_value == avwap_value and avwap_value > 0:
                market_dict["avwap"] = avwap_value

        best = 0.0
        best_reason = ""
        best_fill = 0.0
        for name in self.close_strategies:
            params = self.close_params.get(name)
            result = evaluate(name, position_dict, market_dict, params)
            fraction = float(result.get("close_fraction", 0.0) or 0.0)
            if fraction > best:
                best = fraction
                best_reason = str(result.get("reason") or name)
                best_fill = float(result.get("tier_fill_price", 0.0) or 0.0)
                if best >= 1.0:
                    return 1.0, best_reason, best_fill
        return min(max(best, 0.0), 1.0), best_reason, best_fill

    def _initial_sl_trigger(self, side: str, avg_cost: float,
                            entry_atr: float) -> float:
        if avg_cost <= 0 or side not in ("long", "short"):
            return 0.0
        if (
            self._run_stop_loss_atr_mult is not None
            and self._run_stop_loss_atr_mult > 0
            and entry_atr > 0
        ):
            distance = self._run_stop_loss_atr_mult * entry_atr
            return avg_cost - distance if side == "long" else avg_cost + distance
        if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
            return (
                avg_cost * (1 - self.stop_loss_pct)
                if side == "long"
                else avg_cost * (1 + self.stop_loss_pct)
            )
        return 0.0

    def _hl_regime_mult(self, block, label: str) -> float:
        if not _regime_block_active(block) or not label or self._resolve_regime_atr is None:
            return 0.0
        mult = self._resolve_regime_atr(block, label)
        return float(mult) if mult and mult > 0 else 0.0

    def _hl_unified_stop_mult(self, label: str) -> Tuple[bool, float]:
        if self._unified_close_params is None or self._unified_scalar_params is None:
            return False, 0.0
        scalar, sl = self._unified_scalar_params(self._unified_close_params, label or "")
        return scalar is not None, float(sl or 0.0)

    @staticmethod
    def _capped_atr_fraction(mult: float, entry_atr: float, anchor: float) -> float:
        if mult <= 0 or entry_atr <= 0 or anchor <= 0:
            return 0.0
        return min(mult * entry_atr / anchor, MAX_AUTO_STOP_LOSS_FRACTION)

    def _hl_trailing_fraction(self, anchor: float, entry_atr: float, label: str,
                              post_tp_trail_mult: Optional[float]) -> float:
        if post_tp_trail_mult is not None and post_tp_trail_mult > 0:
            return self._capped_atr_fraction(post_tp_trail_mult, entry_atr, anchor)
        if self.trailing_stop_pct is not None:
            return float(self.trailing_stop_pct) if self.trailing_stop_pct > 0 else 0.0
        if _positive(self.trailing_stop_atr_mult):
            return self._capped_atr_fraction(float(self.trailing_stop_atr_mult), entry_atr, anchor)
        mult = self._hl_regime_mult(self._trailing_stop_regime_block, label)
        return self._capped_atr_fraction(mult, entry_atr, anchor)

    def _hl_fixed_atr_mult(self, label: str) -> float:
        _, unified_sl = self._hl_unified_stop_mult(label)
        if unified_sl > 0:
            return unified_sl
        if _positive(self.stop_loss_atr_mult):
            return float(self.stop_loss_atr_mult)
        return self._hl_regime_mult(self._stop_loss_regime_block, label)

    def _hl_percent_fraction(self) -> float:
        if self._unified_close_params is not None:
            return 0.0
        if _positive(self.trailing_stop_atr_mult) or _positive(self.stop_loss_atr_mult):
            return 0.0
        if _regime_block_active(self._stop_loss_regime_block) \
                or _regime_block_active(self._trailing_stop_regime_block):
            return 0.0
        if self.trailing_stop_pct is not None:
            return float(self.trailing_stop_pct) if self.trailing_stop_pct > 0 else 0.0
        if self.stop_loss_pct is not None:
            return float(self.stop_loss_pct) if self.stop_loss_pct > 0 else 0.0
        if self.stop_loss_margin_pct is not None:
            if self.stop_loss_margin_pct > 0 and _positive(self.leverage):
                return float(self.stop_loss_margin_pct) / float(self.leverage)
            return 0.0
        if _positive(self.max_drawdown_pct):
            return min(float(self.max_drawdown_pct), MAX_AUTO_STOP_LOSS_FRACTION)
        return 0.0

    @staticmethod
    def _trailing_stop_update(side: str, mark: float, high_water: float, fraction: float,
                              min_move: float, current_trigger: float,
                              allow_one_shot_widen: bool = False,
                              bypass_min_move: bool = False) -> Tuple[float, float, bool]:
        if mark <= 0 or fraction <= 0:
            return high_water, 0.0, False
        if high_water <= 0:
            high_water = mark
        candidate_hw = high_water
        if side == "long":
            if mark > candidate_hw:
                candidate_hw = mark
        elif side == "short":
            if mark < candidate_hw:
                candidate_hw = mark
        else:
            return high_water, 0.0, False
        if candidate_hw <= 0:
            return high_water, 0.0, False
        if side == "long":
            candidate = candidate_hw * (1.0 - fraction)
        else:
            candidate = candidate_hw * (1.0 + fraction)
        if candidate <= 0:
            return candidate_hw, 0.0, False
        if current_trigger <= 0:
            return candidate_hw, candidate, True
        favorable = (side == "long" and candidate > current_trigger) or (
            side == "short" and candidate < current_trigger)
        if not favorable:
            if allow_one_shot_widen and abs(candidate - current_trigger) > 1e-9:
                return candidate_hw, candidate, True
            return candidate_hw, 0.0, False
        if bypass_min_move and abs(candidate - current_trigger) > 1e-9:
            return candidate_hw, candidate, True
        if abs(candidate - current_trigger) / current_trigger >= min_move:
            return candidate_hw, candidate, True
        return candidate_hw, 0.0, False

    def _hl_label_evidence(self, label: str) -> dict:
        owner = self._stop_owner
        if not label:
            return {"status": "missing", "source": "position_regime", "value": None}
        if owner == "unified_regime":
            resolved, _ = self._hl_unified_stop_mult(label)
        elif owner == "fixed_atr_regime":
            resolved = self._hl_regime_mult(self._stop_loss_regime_block, label) > 0
        else:
            resolved = self._hl_regime_mult(self._trailing_stop_regime_block, label) > 0
        return {"status": "verified" if resolved else "invalid",
                "source": "position_regime", "value": label}

    def _validate_stop_runtime(self, event_date, evidence: dict) -> None:
        if all(e.get("status") == "verified" for e in evidence.values()):
            return
        params = dict(self._stop_parameters)
        params["event_date"] = str(event_date)
        merged = _thaw_json(self._stop_context.input_evidence)
        merged.update(evidence)
        context = CapabilityContext(
            raw_fields=_thaw_json(self._stop_context.raw_fields),
            resolved_stop_owner={"name": self._stop_owner, "parameters": params},
            input_evidence=merged,
        )
        validate_close_capabilities(
            close_refs=self._close_refs,
            comparison_mode=self.comparison_mode,
            platform=self.platform,
            strategy_type=self.strategy_type,
            consumer="engine",
            phase="runtime",
            capability_context=context,
        )

    def _validate_stop_entry_inputs(self, event_date, anchor: float, entry_atr: float,
                                    label: str) -> None:
        owner = self._stop_owner
        evidence = {"risk_anchor": {
            "status": "verified" if _positive(anchor) else "invalid",
            "source": "entry_fill", "value": _finite_number(anchor)}}
        if owner in STOP_OWNERS_NEEDING_ATR:
            evidence["entry_atr"] = {
                "status": "verified" if _positive(entry_atr) else "missing",
                "source": "closed_bar_atr", "value": _finite_number(entry_atr)}
        if owner in STOP_OWNERS_NEEDING_LABEL:
            evidence["atr_regime_label"] = self._hl_label_evidence(label)
        self._validate_stop_runtime(event_date, evidence)

    def _emit_stop_event(self, event: str, **fields) -> None:
        if self._stop_observer is None:
            return
        payload = {"event": event, "owner": self._stop_owner}
        payload.update(fields)
        self._stop_observer(payload)

    def _hl_arm_stop(self, side: str, anchor: float, entry_atr: float, label: str,
                     mark: float, high_water: float = 0.0,
                     event_date=None) -> Tuple[float, float, bool]:
        self._validate_stop_entry_inputs(event_date, anchor, entry_atr, label)
        tf = self._hl_trailing_fraction(anchor, entry_atr, label, None)
        kind, fraction, trigger, hw, pierce = "none", 0.0, 0.0, 0.0, False
        if tf > 0:
            hw, trigger, _ = self._trailing_stop_update(
                side, mark, high_water if high_water > 0 else anchor, tf,
                self.trailing_stop_min_move_pct, 0.0)
            kind, fraction = "trailing", tf
        else:
            ff = self._capped_atr_fraction(self._hl_fixed_atr_mult(label), entry_atr, anchor)
            if ff <= 0:
                ff = self._hl_percent_fraction()
                kind = "percent" if ff > 0 else "none"
            else:
                kind = "fixed_atr"
            if ff > 0 and anchor > 0:
                trigger = anchor * (1.0 - ff) if side == "long" else anchor * (1.0 + ff)
                if trigger <= 0:
                    trigger = 0.0
                fraction = ff
                pierce = trigger > 0
        self._emit_stop_event(
            "arm", date=str(event_date), side=side, geometry=kind, anchor=anchor,
            entry_atr=entry_atr, regime=label, fraction=fraction, mark=mark,
            trigger=trigger, high_water=hw, replaced=trigger > 0)
        return trigger, hw, pierce

    def _hl_trail_step(self, side: str, anchor: float, entry_atr: float, label: str,
                       mark: float, post_tp_trail_mult: Optional[float],
                       trigger: float, high_water: float, bypass_min_move: bool,
                       event_date=None) -> Tuple[float, float]:
        tf = self._hl_trailing_fraction(anchor, entry_atr, label, post_tp_trail_mult)
        if tf <= 0:
            return trigger, high_water
        new_hw, candidate, replaced = self._trailing_stop_update(
            side, mark, high_water if high_water > 0 else anchor, tf,
            self.trailing_stop_min_move_pct, trigger, bypass_min_move=bypass_min_move)
        new_trigger = candidate if replaced else trigger
        self._emit_stop_event(
            "trail", date=str(event_date), side=side, anchor=anchor, entry_atr=entry_atr,
            regime=label, fraction=tf, mark=mark, post_tp_trail_mult=post_tp_trail_mult,
            bypass_min_move=bypass_min_move, trigger=new_trigger, high_water=new_hw,
            replaced=replaced)
        return new_trigger, new_hw

    @staticmethod
    def _intrabar_sl_fill(side: str, open_px: float, high_px: float,
                          low_px: float, trigger_px: float) -> Optional[float]:
        if trigger_px <= 0:
            return None
        if side == "long":
            if open_px > 0 and open_px <= trigger_px:
                return open_px
            if low_px > 0 and low_px <= trigger_px:
                return trigger_px
        elif side == "short":
            if open_px >= trigger_px:
                return open_px
            if high_px >= trigger_px:
                return trigger_px
        return None

    @staticmethod
    def _sl_hit(side: str, mark_price: float, trigger_px: float) -> bool:
        if trigger_px <= 0 or mark_price <= 0:
            return False
        if side == "long":
            return mark_price <= trigger_px
        if side == "short":
            return mark_price >= trigger_px
        return False

    @staticmethod
    def _walk_trail(side: str, mark_price: float, entry_atr: float,
                    trail_mult: float, sl_trigger_px: float,
                    sl_high_water_px: float) -> Tuple[float, float]:
        if mark_price <= 0 or entry_atr <= 0 or trail_mult <= 0:
            return sl_trigger_px, sl_high_water_px
        new_trigger = sl_trigger_px
        new_hwm = sl_high_water_px
        if side == "long":
            if mark_price > new_hwm:
                new_hwm = mark_price
            candidate = new_hwm - trail_mult * entry_atr
            if candidate > new_trigger:
                new_trigger = candidate
        elif side == "short":
            if new_hwm <= 0 or mark_price < new_hwm:
                new_hwm = mark_price
            candidate = new_hwm + trail_mult * entry_atr
            if new_trigger <= 0 or candidate < new_trigger:
                new_trigger = candidate
        return new_trigger, new_hwm

    def _maybe_apply_sl_after(
        self, *, side: str, avg_cost: float, entry_atr: float,
        position_qty: float, initial_qty: float, mark_price: float,
        fill_price: float, sl_trigger_px: float, sl_tiers_processed: int,
        post_tp_trail_mult: Optional[float], sl_high_water_px: float,
        event_date=None,
    ) -> Tuple[float, int, Optional[float], float]:
        if initial_qty <= 0 or position_qty <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        closed_ratio = 1.0 - (position_qty / initial_qty)
        if closed_ratio <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        highest = self._sl_mod.find_highest_cleared_tier(
            self._run_tp_tier_thresholds, closed_ratio, sl_tiers_processed,
        )
        if highest < 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        raw_rule = self._active_sl_after_rules.for_tier(highest)
        if raw_rule.is_empty():
            return sl_trigger_px, highest + 1, post_tp_trail_mult, sl_high_water_px
        if sl_trigger_px <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        tier_multiple = self._active_sl_after_rules.tier_multiple(highest)
        rule = raw_rule.resolve_for_regime(self._run_position_regime, tier_multiple)
        if rule is None:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        seed_mark = fill_price if fill_price > 0 else mark_price
        if self._hl_stop_geometry and rule.kind in ("atr_offset", "trail_from_here"):
            self._validate_stop_runtime(event_date, {"sl_after_entry_atr": {
                "status": "verified" if _positive(entry_atr) else "missing",
                "source": "closed_bar_atr", "value": _finite_number(entry_atr)}})
        new_trigger, _mode, ok = self._sl_mod.compute_post_tp_stop_loss_trigger(
            rule, side, avg_cost, entry_atr, seed_mark,
        )
        if not ok:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        new_post_tp_trail = post_tp_trail_mult
        new_hwm = sl_high_water_px
        if rule.kind == "trail_from_here":
            new_post_tp_trail = rule.trail_atr_mult
            new_hwm = seed_mark
        self._emit_stop_event(
            "sl_after", date=str(event_date), side=side, anchor=avg_cost,
            entry_atr=entry_atr, regime=self._run_position_regime, tier=highest,
            rule=rule.kind, mark=seed_mark, post_tp_trail_mult=new_post_tp_trail,
            trigger=new_trigger, high_water=new_hwm, replaced=True)
        return new_trigger, highest + 1, new_post_tp_trail, new_hwm

    def _calculate_metrics(self, equity_df: pd.DataFrame, trades: list,
                           df: pd.DataFrame, timeframe: str = "1d") -> dict:
        equity = equity_df["equity"]
        ann_factor = math.sqrt(periods_per_year(timeframe))

        liquidated = bool((equity <= 0).any())
        if liquidated:
            bust_pos = int(np.argmax(equity.values <= 0))
            equity = equity.copy()
            equity.iloc[bust_pos:] = 0.0

        total_return = (equity.iloc[-1] - self.initial_capital) / self.initial_capital

        days = (df.index[-1] - df.index[0]).days
        years = max(days / 365.25, 0.01)
        annual_return = (1 + total_return) ** (1 / years) - 1 if total_return > -1 else -1

        daily_returns = equity.pct_change().dropna()

        if len(daily_returns) > 1 and daily_returns.std() > 0:
            sharpe = (daily_returns.mean() / daily_returns.std()) * ann_factor
        else:
            sharpe = 0.0

        if len(daily_returns) > 0:
            neg = daily_returns.clip(upper=0.0)
            downside_dev = float(np.sqrt((neg**2).mean()))
        else:
            downside_dev = 0.0
        if downside_dev > 0:
            sortino = (daily_returns.mean() / downside_dev) * ann_factor
        else:
            sortino = None

        cummax_raw = equity.cummax()
        cummax = cummax_raw.where(cummax_raw >= self.initial_capital, self.initial_capital)
        drawdown = (equity - cummax) / cummax
        max_drawdown = drawdown.min()

        total_trades = len(trades)
        if total_trades > 0:
            winning = [t for t in trades if t.pnl > 0]
            losing = [t for t in trades if t.pnl <= 0]
            win_rate = len(winning) / total_trades

            gross_profit = sum(t.pnl for t in winning) if winning else 0
            gross_loss = abs(sum(t.pnl for t in losing)) if losing else 0
            profit_factor = gross_profit / gross_loss if gross_loss > 0 else None

            def _net_pnl_pct(t):
                notional = t.shares * t.entry_price
                return (t.pnl / notional) if notional > 0 else 0.0
            avg_win = np.mean([_net_pnl_pct(t) for t in winning]) if winning else 0
            avg_loss = np.mean([_net_pnl_pct(t) for t in losing]) if losing else 0
        else:
            win_rate = 0
            profit_factor = 0
            avg_win = 0
            avg_loss = 0

        volatility = daily_returns.std() * ann_factor if len(daily_returns) > 1 else 0

        calmar = annual_return / abs(max_drawdown) if max_drawdown != 0 else 0

        if liquidated:
            sharpe = sortino = -LIQUIDATED_METRIC_FLOOR
            volatility = LIQUIDATED_METRIC_FLOOR

        return {
            "total_return_pct": round(total_return * 100, 2),
            "annual_return_pct": round(annual_return * 100, 2),
            "sharpe_ratio": round(sharpe, 3),
            "sortino_ratio": round(sortino, 3) if sortino is not None else None,
            "max_drawdown_pct": round(max_drawdown * 100, 2),
            "calmar_ratio": round(calmar, 3),
            "volatility_pct": round(volatility * 100, 2),
            "win_rate": round(win_rate * 100, 2),
            "profit_factor": round(profit_factor, 3) if profit_factor is not None else None,
            "total_trades": total_trades,
            "avg_win_pct": round(avg_win * 100, 2),
            "avg_loss_pct": round(avg_loss * 100, 2),
            "liquidated": liquidated,
        }


def _fmt_opt(value, spec: str = ".3f", none_text: str = "n/a") -> str:
    if value is None:
        return none_text
    return format(value, spec)


def format_results(results: dict) -> str:
    lines = [
        f"\n{'='*60}",
        f"  BACKTEST RESULTS: {results['strategy_name']}",
        f"{'='*60}",
        f"  Symbol:          {results['symbol']}",
        f"  Timeframe:       {results['timeframe']}",
        f"  Period:          {results['start_date'][:10]} → {results['end_date'][:10]}",
        f"  Initial Capital: ${results['initial_capital']:,.2f}",
        f"  Final Capital:   ${results['final_capital']:,.2f}",
    ]
    if results.get("liquidated"):
        lines.append(
            "  *** LIQUIDATED: equity hit 0 — metrics floored at the bust bar ***"
        )
    lines += [
        f"{'─'*60}",
        f"  RETURNS",
        f"    Total Return:    {results['total_return_pct']:+.2f}%",
        f"    Annual Return:   {results['annual_return_pct']:+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*60}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results['sharpe_ratio']:.3f}",
        f"    Sortino Ratio:   {_fmt_opt(results['sortino_ratio'])}",
        f"    Max Drawdown:    {results['max_drawdown_pct']:.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*60}",
        f"  TRADE STATS",
        f"    Total Trades:    {results['total_trades']}",
        f"    Win Rate:        {results['win_rate']:.1f}%",
        f"    Profit Factor:   {_fmt_opt(results['profit_factor'])}",
        f"    Avg Win:         {results.get('avg_win_pct', 0):+.2f}%",
        f"    Avg Loss:        {results.get('avg_loss_pct', 0):+.2f}%",
        f"{'='*60}",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    np.random.seed(42)
    dates = pd.date_range("2023-01-01", periods=200, freq="D")
    prices = 100 + np.cumsum(np.random.randn(200) * 2)
    df = pd.DataFrame({
        "close": prices,
    }, index=dates)

    df["signal"] = 0
    df.iloc[10, df.columns.get_loc("signal")] = 1
    df.iloc[30, df.columns.get_loc("signal")] = -1
    df.iloc[50, df.columns.get_loc("signal")] = 1
    df.iloc[80, df.columns.get_loc("signal")] = -1

    bt = Backtester(initial_capital=1000)
    results = bt.run(df, strategy_name="Test", save=False)
    print(format_results(results))
