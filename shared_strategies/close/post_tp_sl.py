"""Post-TP stop-loss adjustment helpers (`sl_after` rules).

Pure-Python mirror of scheduler/post_tp_sl.go. Used by the backtester (#709)
to simulate the same SL bumps the live HL/manual paths do after a tiered TP
fills. The Go file is the source of truth for behavior; keep this in sync.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, List, Optional, Tuple

# Absolute import (not relative) so this module loads cleanly under
# importlib.util.spec_from_file_location — the backtester tests use that
# loader to sidestep the open/close registry.py name collision, and
# relative imports require a parent-package context that the loader
# doesn't set up.
from shared_strategies.close.regime_atr import (
    CANONICAL_TREND_REGIME_LABELS,
    REGIME_CLASSIFIER_KEY,
    SURFACE_SL_AFTER,
    SURFACE_SL_AFTER_TRAIL,
    SURFACE_STOP_LOSS,
    RegimeATRBlock,
    parse_regime_atr_block,
    parse_regime_tp_tiers,
    resolve_regime_tier,
)
from shared_strategies.close._helpers import tier_list_from_params


_TIERED_TP_NAMES = (
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "tiered_tp_atr_regime",
    "tiered_tp_atr_live_regime",
)

# Canonical fallback tier ladder when a tiered_tp_atr* close ref omits explicit
# tiers. MUST stay in sync with the Go source of truth
# `defaultHLProtectionTiers()` in scheduler/hyperliquid_protection.go
# (#870: 1.5×/3×/5× @ 40%/80%/100%); tp_atr_fraction derives its firing-tier
# multiple from it.
_DEFAULT_SCALAR_TP_TIERS: Tuple[Tuple[float, float], ...] = (
    (1.5, 0.40),
    (3.0, 0.80),
    (5.0, 1.00),
)


@dataclass(frozen=True)
class RegimeFloatBlock:
    trend_regime: Dict[str, float] = field(default_factory=dict)

    def resolve(self, regime: str) -> Optional[float]:
        r = (regime or "").strip()
        v = self.trend_regime.get(r)
        if v is not None:
            return v
        # #1124: sub-label stamp falls back to the bare ranging_directional entry.
        if r in ("ranging_directional_up", "ranging_directional_down"):
            return self.trend_regime.get("ranging_directional")
        return None


@dataclass(frozen=True)
class SLAfterRule:
    """Typed sl_after rule. ``kind=""`` means the empty / no-op rule.

    ``atr_regime`` / ``trail_atr_regime`` are set instead of the scalar
    multiplier when the operator wrote a ``trend_regime`` block (#736);
    the backtester resolves them per regime at fire time (live behavior
    lives in scheduler/post_tp_sl.go).
    """

    kind: str = ""
    atr_mult: float = 0.0  # signed; +N moves toward profit (long: above avg)
    trail_atr_mult: float = 0.0  # > 0 for trail_from_here
    atr_regime: Optional[RegimeATRBlock] = None
    trail_atr_regime: Optional[RegimeATRBlock] = None
    tp_atr_fraction: float = 0.0
    tp_atr_fraction_regime: Optional[RegimeFloatBlock] = None

    def is_empty(self) -> bool:
        return self.kind == ""

    def has_regime(self) -> bool:
        return (
            self.atr_regime is not None
            or self.trail_atr_regime is not None
            or self.tp_atr_fraction_regime is not None
        )

    def resolve_for_regime(
        self, regime: str, tier_multiple: float = 0.0,
    ) -> Optional["SLAfterRule"]:
        """Collapse a regime-aware rule to its scalar form for the given
        regime label. Returns None when the rule is regime-aware but the
        label is missing (caller should defer). Scalar rules pass through
        unchanged. Mirrors Go's SLAfterRule.resolveForRegime.
        """
        if self.kind == "atr_offset" and self.atr_regime is not None:
            entry = self.atr_regime.resolve(regime)
            if entry is None:
                return None
            return SLAfterRule(kind="atr_offset", atr_mult=entry.atr)
        if self.kind == "trail_from_here" and self.trail_atr_regime is not None:
            entry = self.trail_atr_regime.resolve(regime)
            if entry is None or entry.atr <= 0:
                return None
            return SLAfterRule(kind="trail_from_here", trail_atr_mult=entry.atr)
        if self.kind == "trail_from_here" and self.tp_atr_fraction_regime is not None:
            frac = self.tp_atr_fraction_regime.resolve(regime)
            if frac is None or frac <= 0 or tier_multiple <= 0:
                return None
            return SLAfterRule(
                kind="trail_from_here",
                trail_atr_mult=frac * tier_multiple,
            )
        if self.kind == "trail_from_here" and self.tp_atr_fraction > 0:
            if tier_multiple <= 0:
                return None
            return SLAfterRule(
                kind="trail_from_here",
                trail_atr_mult=self.tp_atr_fraction * tier_multiple,
            )
        return self


@dataclass
class TierSLAfterRules:
    """Strategy-level default + per-tier overrides, aligned with the parsed
    tiers (ascending by ``atr_multiple``)."""

    default: SLAfterRule = field(default_factory=SLAfterRule)
    per_tier: List[SLAfterRule] = field(default_factory=list)
    multiples: List[float] = field(default_factory=list)

    def for_tier(self, idx: int) -> SLAfterRule:
        if 0 <= idx < len(self.per_tier) and not self.per_tier[idx].is_empty():
            return self.per_tier[idx]
        return self.default

    def tier_multiple(self, idx: int) -> float:
        if 0 <= idx < len(self.multiples):
            return self.multiples[idx]
        return 0.0

    def has_any(self) -> bool:
        if not self.default.is_empty():
            return True
        return any(not r.is_empty() for r in self.per_tier)


def _float_or_raise(raw: Any, label: str) -> float:
    if raw is None:
        raise ValueError(f"{label}: missing required numeric value")
    try:
        return float(raw)
    except (TypeError, ValueError):
        raise ValueError(f"{label}: expected number, got {raw!r}")


def _first_present(d: dict, *keys: str) -> Any:
    for k in keys:
        if k in d:
            return d[k]
    return None


def _first_non_nil(d: dict, *keys: str) -> bool:
    for k in keys:
        if d.get(k) is not None:
            return True
    return False


def parse_sl_after_rule(
    raw: Any,
    labels: Optional[Iterable[str]] = CANONICAL_TREND_REGIME_LABELS,
) -> SLAfterRule:
    """Parse the raw value found at ``params["sl_after"]`` (or inside a tier).

    Mirrors ``parseSLAfterRule`` in scheduler/post_tp_sl.go. Accepts:

      * ``None`` / ``""``                                  → empty rule
      * ``"breakeven"``                                    → breakeven
      * ``{"atr_mult": 0.25}``                             → atr_offset scalar
      * ``{"trend_regime": {<labels>}}``                   → atr_offset regime (#736)
      * ``{"trail_from_here": {"atr_mult": 1.0}}``         → trail_from_here scalar
      * ``{"trail_from_here": {"trend_regime": {...}}}``   → trail_from_here regime (#736)
      * ``{"kind": "atr_offset", "atr_mult": ...}``
      * ``{"kind": "atr_offset", "trend_regime": {...}}``  → atr_offset regime, explicit kind
      * ``{"kind": "trail_from_here", "atr_mult": ...}``
      * ``{"kind": "trail_from_here", "trend_regime": {...}}``

    Raises ``ValueError`` on malformed shapes.
    """
    if raw is None:
        return SLAfterRule()
    if isinstance(raw, str):
        kind = raw.strip().lower()
        if kind == "":
            return SLAfterRule()
        if kind == "breakeven":
            return SLAfterRule(kind="breakeven")
        raise ValueError(
            f'sl_after string {raw!r} is not recognized (expected "breakeven")'
        )
    if isinstance(raw, dict):
        if "kind" in raw:
            kind_raw = raw["kind"]
            if not isinstance(kind_raw, str):
                raise ValueError(
                    f"sl_after.kind must be a string, got {type(kind_raw).__name__}"
                )
            kind = kind_raw.strip().lower()
            if kind == "breakeven":
                return SLAfterRule(kind="breakeven")
            if kind == "atr_offset":
                return _parse_sl_after_atr_offset(raw, "sl_after kind=atr_offset", labels)
            if kind == "trail_from_here":
                return _parse_sl_after_trail_from_here(raw, "sl_after kind=trail_from_here", labels)
            raise ValueError(f"sl_after kind {kind!r} is not recognized")
        if "trail_from_here" in raw:
            trail_raw = raw["trail_from_here"]
            if not isinstance(trail_raw, dict):
                raise ValueError(
                    f"sl_after.trail_from_here must be an object, got "
                    f"{type(trail_raw).__name__}"
                )
            return _parse_sl_after_trail_from_here(
                trail_raw, "sl_after.trail_from_here", labels,
            )
        if REGIME_CLASSIFIER_KEY in raw:
            return _parse_sl_after_atr_offset(raw, "sl_after", labels)
        if _first_non_nil(raw, "atr_mult", "atr_offset"):
            return _parse_sl_after_atr_offset(raw, "sl_after atr_mult", labels)
        raise ValueError(
            'sl_after object must contain "kind", "atr_mult", "trail_from_here", '
            'or "trend_regime"'
        )
    raise ValueError(
        f"sl_after must be a string or object, got {type(raw).__name__}"
    )


# Scalar-form keys that conflict with a regime block on the atr_offset
# variant. trail_atr_mult here is a misplaced field, surfaced loudly.
_SCALAR_MULT_KEYS_ATR_OFFSET = ("atr_mult", "atr_offset", "trail_atr_mult")
# Scalar-form keys that conflict with a regime block on the trail_from_here
# variant. atr_offset here is a misplaced field.
_SCALAR_MULT_KEYS_TRAIL = ("atr_mult", "trail_atr_mult", "atr_offset")


def _parse_sl_after_atr_offset(
    m: dict,
    ctx_label: str,
    labels: Optional[Iterable[str]] = None,
) -> SLAfterRule:
    """Parse the atr_offset variant — scalar (atr_mult/atr_offset) or regime
    (trend_regime/use_defaults). Multi-label regime errors join with '; '
    so callers that surface a single error per field stay compatible."""
    has_trend = REGIME_CLASSIFIER_KEY in m
    has_use_defaults = "use_defaults" in m
    if has_trend or has_use_defaults:
        if _first_non_nil(m, *_SCALAR_MULT_KEYS_ATR_OFFSET):
            raise ValueError(
                f"{ctx_label}: cannot combine scalar "
                "atr_mult/atr_offset/trail_atr_mult with "
                "trend_regime/use_defaults — pick one shape"
            )
        regime_raw: dict = {}
        if has_trend:
            regime_raw[REGIME_CLASSIFIER_KEY] = m[REGIME_CLASSIFIER_KEY]
        if has_use_defaults:
            regime_raw["use_defaults"] = m["use_defaults"]
        block, errs = parse_regime_atr_block(
            regime_raw, ctx_label, SURFACE_SL_AFTER,
            labels=_labels_for_regime_raw(regime_raw, labels),
        )
        if errs:
            raise ValueError("; ".join(errs))
        rule = SLAfterRule(kind="atr_offset", atr_regime=block)
        validate_sl_after_rule(rule)
        return rule
    mult = _float_or_raise(
        _first_present(m, "atr_mult", "atr_offset"),
        ctx_label,
    )
    rule = SLAfterRule(kind="atr_offset", atr_mult=mult)
    validate_sl_after_rule(rule)
    return rule


def _parse_sl_after_trail_from_here(
    m: dict,
    ctx_label: str,
    labels: Optional[Iterable[str]] = None,
) -> SLAfterRule:
    """Parse the trail_from_here variant — scalar (atr_mult/trail_atr_mult)
    or regime (trend_regime/use_defaults). Regime form uses
    SURFACE_SL_AFTER_TRAIL so per-label atr must be strictly positive."""
    has_trend = REGIME_CLASSIFIER_KEY in m
    has_use_defaults = "use_defaults" in m
    if "tp_atr_fraction" in m:
        if has_trend or has_use_defaults:
            raise ValueError(
                f"{ctx_label}: cannot combine tp_atr_fraction with "
                "trend_regime/use_defaults — pick one trail_from_here shape"
            )
        if _first_non_nil(m, *_SCALAR_MULT_KEYS_TRAIL):
            raise ValueError(
                f"{ctx_label}: cannot combine tp_atr_fraction with "
                "atr_mult/trail_atr_mult/atr_offset — pick one shape"
            )
        rule = _parse_tp_atr_fraction(
            m["tp_atr_fraction"],
            f"{ctx_label}.tp_atr_fraction",
            labels,
        )
        validate_sl_after_rule(rule)
        return rule
    if has_trend or has_use_defaults:
        if _first_non_nil(m, *_SCALAR_MULT_KEYS_TRAIL):
            raise ValueError(
                f"{ctx_label}: cannot combine scalar "
                "atr_mult/trail_atr_mult/atr_offset with "
                "trend_regime/use_defaults — pick one shape"
            )
        regime_raw: dict = {}
        if has_trend:
            regime_raw[REGIME_CLASSIFIER_KEY] = m[REGIME_CLASSIFIER_KEY]
        if has_use_defaults:
            regime_raw["use_defaults"] = m["use_defaults"]
        block, errs = parse_regime_atr_block(
            regime_raw, ctx_label, SURFACE_SL_AFTER_TRAIL,
            labels=_labels_for_regime_raw(regime_raw, labels),
        )
        if errs:
            raise ValueError("; ".join(errs))
        rule = SLAfterRule(kind="trail_from_here", trail_atr_regime=block)
        validate_sl_after_rule(rule)
        return rule
    mult = _float_or_raise(
        _first_present(m, "atr_mult", "trail_atr_mult"),
        ctx_label,
    )
    rule = SLAfterRule(kind="trail_from_here", trail_atr_mult=mult)
    validate_sl_after_rule(rule)
    return rule


def _parse_tp_atr_fraction(
    raw: Any,
    ctx_label: str,
    labels: Optional[Iterable[str]] = None,
) -> SLAfterRule:
    if isinstance(raw, dict):
        block, errs = _parse_regime_float_block(
            raw, ctx_label, _labels_for_regime_raw(raw, labels),
        )
        if errs:
            raise ValueError("; ".join(errs))
        return SLAfterRule(
            kind="trail_from_here",
            tp_atr_fraction_regime=block,
        )
    frac = _float_or_raise(raw, ctx_label)
    if frac <= 0:
        raise ValueError(f"{ctx_label}: must be > 0, got {frac:g}")
    return SLAfterRule(kind="trail_from_here", tp_atr_fraction=frac)


def _parse_regime_float_block(
    raw: dict,
    ctx_label: str,
    labels: Iterable[str],
) -> Tuple[RegimeFloatBlock, List[str]]:
    labels = tuple(labels)
    errs: List[str] = []
    for key in raw:
        if key != REGIME_CLASSIFIER_KEY:
            errs.append(f"{ctx_label}: unknown key {key!r} (expected {REGIME_CLASSIFIER_KEY!r})")
    trend_raw = raw.get(REGIME_CLASSIFIER_KEY)
    if not isinstance(trend_raw, dict):
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: must be an object, "
            f"got {type(trend_raw).__name__}"
        )
        return RegimeFloatBlock(), errs
    valid = set(labels)
    unknown = sorted(k for k in trend_raw if k not in valid)
    for label in unknown:
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: unknown regime label {label!r} "
            f"(expected one of: {', '.join(labels)})"
        )
    missing = [
        label for label in labels
        if label not in trend_raw
        and not (
            label in ("ranging_directional_up", "ranging_directional_down")
            and "ranging_directional" in trend_raw
        )
    ]
    if missing:
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: missing required regime labels: "
            f"{', '.join(missing)} (must be exhaustive — no silent fallback)"
        )
    out: Dict[str, float] = {}
    for label in labels:
        if label not in trend_raw:
            continue
        try:
            frac = float(trend_raw[label])
        except (TypeError, ValueError):
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: expected number, "
                f"got {trend_raw[label]!r}"
            )
            continue
        if frac <= 0:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: must be > 0, got {frac:g}"
            )
            continue
        out[label] = frac
    if errs:
        return RegimeFloatBlock(), errs
    return RegimeFloatBlock(trend_regime=out), []


def _labels_for_regime_raw(
    raw: Any,
    labels: Optional[Iterable[str]],
) -> Tuple[str, ...]:
    if labels is not None:
        return tuple(labels)
    if isinstance(raw, dict) and isinstance(raw.get(REGIME_CLASSIFIER_KEY), dict):
        inferred = tuple(sorted(raw[REGIME_CLASSIFIER_KEY].keys()))
        if inferred:
            return inferred
    return tuple(CANONICAL_TREND_REGIME_LABELS)


def _labels_for_regime_tiers(
    raw_tiers: Any,
    labels: Optional[Iterable[str]],
) -> Tuple[str, ...]:
    if labels is not None:
        return tuple(labels)
    if isinstance(raw_tiers, list):
        found = set()
        for item in raw_tiers:
            if isinstance(item, dict) and isinstance(item.get(REGIME_CLASSIFIER_KEY), dict):
                found.update(item[REGIME_CLASSIFIER_KEY].keys())
        if found:
            return tuple(sorted(found))
    return tuple(CANONICAL_TREND_REGIME_LABELS)


def validate_sl_after_rule(rule: SLAfterRule) -> None:
    """Sanity-check a parsed rule. Raises ``ValueError`` on bad shapes; the
    empty rule passes silently."""
    if rule.kind == "":
        return
    if rule.kind == "breakeven":
        if (
            rule.atr_regime is not None
            or rule.trail_atr_regime is not None
            or rule.tp_atr_fraction_regime is not None
            or rule.tp_atr_fraction != 0
        ):
            raise ValueError("sl_after breakeven does not accept trend_regime or tp_atr_fraction")
        return
    if rule.kind == "atr_offset":
        if (
            rule.trail_atr_regime is not None
            or rule.tp_atr_fraction_regime is not None
            or rule.tp_atr_fraction != 0
        ):
            raise ValueError(
                "sl_after atr_offset accepts trend_regime under atr, not "
                "trail_from_here trail fields"
            )
        return
    if rule.kind == "trail_from_here":
        if rule.atr_regime is not None:
            raise ValueError(
                "sl_after trail_from_here accepts trend_regime under "
                "trail_from_here.atr, not at the top level"
            )
        forms = sum(
            [
                rule.trail_atr_mult > 0,
                rule.trail_atr_regime is not None,
                rule.tp_atr_fraction > 0,
                rule.tp_atr_fraction_regime is not None,
            ]
        )
        if forms != 1:
            raise ValueError(
                "sl_after trail_from_here requires exactly one of atr_mult, "
                "trend_regime, or tp_atr_fraction"
            )
        return
    raise ValueError(
        f"sl_after kind {rule.kind!r} is not recognized "
        "(expected breakeven|atr_offset|trail_from_here)"
    )


def _format_atr_offset_mode(mult: float) -> str:
    """Mirror Go ``formatATROffsetMode`` so logs/audits read identically.
    Preserves operator intent: ``{atr_mult: 0}`` renders ``atr+0``, never
    collapses to ``breakeven`` (that string is reserved for explicit
    ``kind="breakeven"``)."""
    sign = "+"
    abs_m = mult
    if mult < 0:
        sign = "-"
        abs_m = -mult
    return f"atr{sign}{_format_g(abs_m)}"


def _format_g(value: float) -> str:
    """Mimic Go's ``%g`` for a non-negative float — strips trailing zeros."""
    text = f"{value:g}"
    return text


def compute_post_tp_stop_loss_trigger(
    rule: SLAfterRule,
    side: str,
    avg_cost: float,
    entry_atr: float,
    current_mark: float,
) -> Tuple[float, str, bool]:
    """Return ``(trigger_px, mode, ok)`` for a post-TP SL bump.

    ``ok=False`` when inputs are insufficient (rule needs ATR but it's
    missing, unknown side, etc.). The caller is responsible for the
    "never worse than current SL" clamp; this returns the rule's natural
    target. For ``trail_from_here`` the returned price is the initial
    trailing trigger seeded at ``current_mark``; subsequent walking is the
    walker's job.
    """
    side_lower = (side or "").strip().lower()
    if side_lower not in ("long", "short"):
        return 0.0, "", False
    if avg_cost <= 0:
        return 0.0, "", False
    if rule.kind == "":
        return 0.0, "", False
    if rule.kind == "breakeven":
        return avg_cost, "breakeven", True
    if rule.kind == "atr_offset":
        if entry_atr <= 0:
            return 0.0, "", False
        if side_lower == "long":
            px = avg_cost + rule.atr_mult * entry_atr
        else:
            px = avg_cost - rule.atr_mult * entry_atr
        if px <= 0:
            return 0.0, "", False
        return px, _format_atr_offset_mode(rule.atr_mult), True
    if rule.kind == "trail_from_here":
        if entry_atr <= 0 or current_mark <= 0 or rule.trail_atr_mult <= 0:
            return 0.0, "", False
        if side_lower == "long":
            px = current_mark - rule.trail_atr_mult * entry_atr
        else:
            px = current_mark + rule.trail_atr_mult * entry_atr
        if px <= 0:
            return 0.0, "", False
        return px, f"trail {_format_g(rule.trail_atr_mult)}×ATR", True
    return 0.0, "", False


def _strategy_uses_tiered_tp_atr_close(close_refs: Iterable[dict]) -> bool:
    for ref in close_refs:
        if (ref.get("name") or "").strip().lower() in _TIERED_TP_NAMES:
            return True
    return False


def parse_strategy_tp_sl_after_rules(
    close_refs: Iterable[dict],
    regime: Optional[str] = None,
    labels: Optional[Iterable[str]] = None,
) -> Tuple[TierSLAfterRules, List[str]]:
    """Walk the strategy's close refs and extract the strategy-level default
    and per-tier sl_after rules from the first ``tiered_tp_atr*`` entry.

    Returns ``(rules, errs)``. Errors describe individual malformed fields;
    the parser still returns whatever it could so the caller can surface the
    problems at config-load time without losing the rest of the config.

    When the first tiered ref is ``tiered_tp_atr_regime`` /
    ``tiered_tp_atr_live_regime``, per-tier ``sl_after`` alignment uses the
    tier's ATR multiple **resolved for the given regime label** (same order as
    ``parse_tp_tier_close_fractions``). Pass ``regime=None`` at static load
    time to skip per-tier extraction (defaults still parse).
    """
    rules = TierSLAfterRules()
    errs: List[str] = []
    if not _strategy_uses_tiered_tp_atr_close(close_refs):
        return rules, errs
    default_raw: Any = None
    tiers_raw: Any = None
    tiered_name = ""
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name not in _TIERED_TP_NAMES:
            continue
        tiered_name = name
        params = ref.get("params") or {}
        if "sl_after" in params:
            default_raw = params["sl_after"]
        _tiers = tier_list_from_params(params)
        if _tiers is not None:
            tiers_raw = _tiers
        break
    if default_raw is not None:
        try:
            r = parse_sl_after_rule(default_raw, labels=labels)
            validate_sl_after_rule(r)
            rules.default = r
        except ValueError as e:
            errs.append(f"sl_after (strategy-level): {e}")
    if tiered_name in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
        reg = (regime or "").strip()
        if not reg:
            if isinstance(tiers_raw, list):
                for idx, item in enumerate(tiers_raw):
                    if not isinstance(item, dict):
                        continue
                    rule = SLAfterRule()
                    if item.get("sl_after") is not None:
                        try:
                            parsed = parse_sl_after_rule(item["sl_after"], labels=labels)
                            validate_sl_after_rule(parsed)
                            rule = parsed
                        except ValueError as e:
                            errs.append(f"sl_after (tier[{idx}]): {e}")
                    rules.per_tier.append(rule)
            return rules, errs
        ref_params: dict = {}
        for ref in close_refs:
            if (ref.get("name") or "").strip().lower() == tiered_name:
                ref_params = dict(ref.get("params") or {})
                break
        use_defaults = bool(ref_params.get("use_defaults"))
        specs, terr = parse_regime_tp_tiers(
            tiers_raw,
            f"{tiered_name}.tiers",
            use_defaults,
            labels=(
                tuple(labels)
                if labels is not None
                else ((reg,) if use_defaults else _labels_for_regime_tiers(tiers_raw, labels))
            ),
        )
        errs.extend(terr)
        if terr:
            return rules, errs
        pairs: List[Tuple[float, SLAfterRule]] = []
        items_list = tiers_raw if isinstance(tiers_raw, list) else []
        for idx, spec in enumerate(specs):
            pair = resolve_regime_tier(spec, reg)
            if pair is None:
                errs.append(
                    f"{tiered_name}.tiers[{idx}]: regime {reg!r} resolved to no "
                    "atr/close_fraction for sl_after tier alignment"
                )
                continue
            mult, _ = pair
            item: dict = {}
            if idx < len(items_list) and isinstance(items_list[idx], dict):
                item = items_list[idx]
            rule = SLAfterRule()
            if item.get("sl_after") is not None:
                try:
                    parsed = parse_sl_after_rule(item["sl_after"], labels=labels)
                    validate_sl_after_rule(parsed)
                    rule = parsed
                except ValueError as e:
                    errs.append(f"sl_after (tier[{idx}]): {e}")
            pairs.append((mult, rule))
        pairs.sort(key=lambda p: p[0])
        rules.per_tier = [p[1] for p in pairs]
        rules.multiples = [p[0] for p in pairs]
        return rules, errs

    if not isinstance(tiers_raw, list) or len(tiers_raw) == 0:
        if rules.has_any():
            rules.multiples = [p[0] for p in _DEFAULT_SCALAR_TP_TIERS]
        return rules, errs
    pairs: List[Tuple[float, SLAfterRule]] = []
    for idx, item in enumerate(tiers_raw):
        if not isinstance(item, dict):
            continue
        mult_raw = item.get("atr_multiple")
        try:
            mult = float(mult_raw) if mult_raw is not None else 0.0
        except (TypeError, ValueError):
            continue
        if mult <= 0:
            continue
        rule = SLAfterRule()
        if item.get("sl_after") is not None:
            try:
                parsed = parse_sl_after_rule(item["sl_after"], labels=labels)
                validate_sl_after_rule(parsed)
                rule = parsed
            except ValueError as e:
                errs.append(f"sl_after (tier[{idx}]): {e}")
        pairs.append((mult, rule))
    pairs.sort(key=lambda p: p[0])
    rules.per_tier = [p[1] for p in pairs]
    rules.multiples = [p[0] for p in pairs]
    return rules, errs


def validate_post_tp_stop_loss_rules(
    close_refs: Iterable[dict],
    *,
    stop_loss_atr_mult: Optional[float] = None,
    stop_loss_pct: Optional[float] = None,
    stop_loss_margin_pct: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    trailing_stop_pct: Optional[float] = None,
    stop_loss_atr_regime: Optional[Any] = None,
    strategy_type: str = "perps",
    labels: Optional[Iterable[str]] = None,
) -> List[str]:
    """Mirror ``validatePostTPStopLossRulesWithLabels`` in scheduler/post_tp_sl.go.

    Conditions enforced:
      - shape/field-level errors from parsing
      - reject sl_after on non-tiered_tp_atr* close refs (silent no-op in
        live, so we fail loud at load)
      - reject combination with a strategy-level trailing stop
      - require a fixed stop-loss to adjust
      - reject trail_from_here on manual strategies (perps-only in v1)

    ``labels`` is the regime vocabulary the strategy's primary/ATR window
    classifier emits (#1058): live threads ``regimeLabelsForStrategyWindow`` here
    so a composite-keyed ``stop_loss_atr_regime`` / ``sl_after`` block validates
    against the 7-state substates instead of the 3 ADX labels. ``None`` keeps the
    legacy ADX behavior byte-identical (``parse_regime_atr_block`` defaults to the
    canonical ADX labels; ``parse_strategy_tp_sl_after_rules`` infers from keys).
    """
    close_refs = list(close_refs)
    rules, parse_errs = parse_strategy_tp_sl_after_rules(close_refs, labels=labels)
    out: List[str] = list(parse_errs)
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name in _TIERED_TP_NAMES:
            continue
        params = ref.get("params") or {}
        if "sl_after" in params:
            out.append(
                f"sl_after is only honored on tiered_tp_atr* close refs; "
                f"found on {ref.get('name')!r}"
            )
        tiers_raw = tier_list_from_params(params)
        if isinstance(tiers_raw, list):
            for i, item in enumerate(tiers_raw):
                if isinstance(item, dict) and "sl_after" in item:
                    out.append(
                        f"sl_after on tier[{i}] of {ref.get('name')!r} has "
                        "no effect; only honored on tiered_tp_atr* close refs"
                    )
    if not rules.has_any():
        return out
    if (trailing_stop_atr_mult is not None and trailing_stop_atr_mult > 0) or (
        trailing_stop_pct is not None and trailing_stop_pct > 0
    ):
        out.append(
            "sl_after cannot be combined with trailing_stop_atr_mult or "
            "trailing_stop_pct — trailing already walks the SL continuously"
        )
    blk_sl_regime, sl_regime_errs = parse_regime_atr_block(
        stop_loss_atr_regime, "stop_loss_atr_regime", SURFACE_STOP_LOSS,
        labels=tuple(labels) if labels is not None else None,
    )
    if stop_loss_atr_regime is not None and sl_regime_errs:
        out.extend(sl_regime_errs)
    has_regime_sl = (
        stop_loss_atr_regime is not None
        and not blk_sl_regime.is_zero()
        and not sl_regime_errs
    )
    has_fixed_sl = (
        (stop_loss_atr_mult is not None and stop_loss_atr_mult > 0)
        or (stop_loss_pct is not None and stop_loss_pct > 0)
        or (stop_loss_margin_pct is not None and stop_loss_margin_pct > 0)
        or has_regime_sl
    )
    if not has_fixed_sl:
        out.append(
            "sl_after requires a fixed stop-loss to adjust (set "
            "stop_loss_atr_mult, stop_loss_atr_regime, stop_loss_pct, "
            "or stop_loss_margin_pct)"
        )
    if (strategy_type or "").strip().lower() == "manual":
        if rules.default.kind == "trail_from_here":
            out.append(
                "sl_after: trail_from_here is not supported on manual "
                "strategies (perps only in v1) — use breakeven or "
                "atr_mult instead"
            )
        for i, r in enumerate(rules.per_tier):
            if r.kind == "trail_from_here":
                out.append(
                    f"sl_after (tier[{i}]): trail_from_here is not supported "
                    "on manual strategies (perps only in v1) — use "
                    "breakeven or atr_mult instead"
                )
    return out


def validate_regime_tiered_tp_labels(
    close_refs: Iterable[dict],
    labels: Optional[Iterable[str]] = None,
) -> List[str]:
    """Validate ``tiered_tp_atr_regime`` / ``tiered_tp_atr_live_regime`` tier-key
    vocabularies against the strategy's primary-window classifier ``labels`` (#1058).

    Mirrors the intent of the live config-load check in scheduler/regime_atr.go
    (``parseRegimeTPTiers`` keyed by ``regimeLabelsForStrategyWindow``): a tier's
    ``trend_regime`` block keyed by labels the primary window's classifier can
    never emit — an ADX-keyed tier under a composite primary window, or the
    inverse — is rejected loudly at load. Without it the backtester's
    ``parse_tp_tier_close_fractions`` infers labels from the tier keys and then
    ``resolve_regime_tier`` silently misses on every stamped label, disabling all
    take-profit tiers (a 0-TP run that reads as "never hit TP", not "bad config").

    Guards the tier-key VOCABULARY only — by inspecting each tier's
    ``trend_regime`` keys, never re-parsing tier shape/count/sibling keys
    (``sl_after``, ``tp_atr_fraction``, scalar ``close_fraction``), which the
    backtester's existing machinery and the HL-live-only guards already handle.
    Each per-regime tier must be EXHAUSTIVE over the expected vocabulary: a key
    the classifier can never emit is flagged as unknown, and an omitted label is
    flagged as missing — mirroring live ``parseRegimeATRBlock`` (regime_atr.go),
    which rejects a non-exhaustive ``trend_regime`` block per tier ("must be
    exhaustive — no silent fallback"). Without the missing-label arm a
    composite-primary config with a partially-keyed tier passes the backtester
    but is rejected live, and ``resolve_regime_tier`` silently no-ops TP on every
    un-keyed substate. A tier-level ``use_defaults`` expands to the full
    vocabulary at the resolver, so it is exempt (like live's early-return).
    ``labels=None`` resolves to the canonical 3 ADX labels, so the ADX/legacy
    path is byte-identical AND an ADX-primary strategy still rejects
    composite-keyed tiers (must-survive (b)). Returns error strings (empty=valid).
    """
    expected = set(labels) if labels is not None else set(CANONICAL_TREND_REGIME_LABELS)
    errs: List[str] = []
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            continue
        params = ref.get("params") or {}
        if bool(params.get("use_defaults")):
            # Baseline ladder carries no operator-supplied keys to mis-vocabulary;
            # validated at the default-tier resolver, like live.
            continue
        tiers_raw = tier_list_from_params(params)
        if not isinstance(tiers_raw, list):
            continue
        for i, tier in enumerate(tiers_raw):
            if not isinstance(tier, dict):
                continue
            if bool(tier.get("use_defaults")):
                # Tier-level use_defaults expands to the full vocabulary at the
                # resolver (mirrors live parseRegimeATRBlock early-return) — no
                # operator-supplied keys to mis-vocabulary or omit.
                continue
            block = tier.get(REGIME_CLASSIFIER_KEY)
            if not isinstance(block, dict):
                continue
            for key in sorted(k for k in block.keys() if k not in expected):
                errs.append(
                    f"{name}.tiers[{i}].{REGIME_CLASSIFIER_KEY}: unknown regime "
                    f"label {key!r} (expected one of: {', '.join(sorted(expected))})"
                )
            # #1124: a present bare `ranging_directional` covers its _up/_down
            # sub-labels for exhaustiveness (back-compat — the bare label
            # resolves the whole family at runtime, including the return_eff==0
            # neutral case the producer still emits). Providing only the
            # sub-labels without the bare label is NOT exhaustive.
            bare_directional_present = "ranging_directional" in block
            missing = [
                label for label in sorted(expected)
                if label not in block
                and not (
                    label in ("ranging_directional_up", "ranging_directional_down")
                    and bare_directional_present
                )
            ]
            if missing:
                errs.append(
                    f"{name}.tiers[{i}].{REGIME_CLASSIFIER_KEY}: missing required "
                    f"regime labels: {', '.join(missing)} "
                    f"(must be exhaustive — no silent fallback)"
                )
    return errs


def parse_tp_tier_close_fractions(
    close_refs: Iterable[dict],
    regime: Optional[str] = None,
) -> List[float]:
    """Return cumulative ``close_fraction`` values for the strategy's
    ``tiered_tp_atr*`` close ref (sorted by ascending ``atr_multiple``).

    Used by the backtester (#709, #737) to detect which tier just fired by
    comparing the post-bar ``closed_qty / initial_qty`` ratio against the
    cumulative thresholds. Returns an empty list when no tiered ATR ref is
    configured. Final-tier close_fraction is coerced to 1.0 to match the
    live ``strategyTPTiers`` behavior.

    For ``tiered_tp_atr_regime`` / ``tiered_tp_atr_live_regime``, pass the
    stamped position regime label so per-regime ATR multiples resolve to the
    same tier ordering used by the close evaluators. With ``regime=None`` or
    an empty label, regime-aware refs return ``[]`` (caller re-parses at
    open with a concrete label).
    """
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name not in _TIERED_TP_NAMES:
            continue
        params = ref.get("params") or {}
        if name in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            reg = (regime or "").strip()
            if not reg:
                return []
            use_defaults = bool(params.get("use_defaults"))
            tiers_raw = tier_list_from_params(params)
            specs, terr = parse_regime_tp_tiers(
                tiers_raw,
                f"{name}.tiers",
                use_defaults,
                labels=((reg,) if use_defaults else _labels_for_regime_tiers(tiers_raw, None)),
            )
            if terr:
                return []
            pairs: List[Tuple[float, float]] = []
            for spec in specs:
                pair = resolve_regime_tier(spec, reg)
                if pair is None:
                    return []
                mult, frac = pair
                pairs.append((mult, max(min(frac, 1.0), 0.0)))
            if not pairs:
                return []
            pairs.sort(key=lambda p: p[0])
            out = [p[1] for p in pairs]
            if out:
                out[-1] = 1.0
            return out

        tiers_raw = tier_list_from_params(params)
        if not isinstance(tiers_raw, list) or len(tiers_raw) == 0:
            return [p[1] for p in _DEFAULT_SCALAR_TP_TIERS]
        pairs = []
        for item in tiers_raw:
            if not isinstance(item, dict):
                continue
            mult_raw = item.get("atr_multiple")
            frac_raw = item.get("close_fraction")
            try:
                mult = float(mult_raw) if mult_raw is not None else 0.0
                frac = float(frac_raw) if frac_raw is not None else 0.0
            except (TypeError, ValueError):
                continue
            if mult <= 0 or frac <= 0:
                continue
            pairs.append((mult, max(min(frac, 1.0), 0.0)))
        if not pairs:
            return []
        pairs.sort(key=lambda p: p[0])
        out = [p[1] for p in pairs]
        # Coerce final tier to 1.0 so detection matches live behavior.
        if out:
            out[-1] = 1.0
        return out
    return []


def find_highest_cleared_tier(
    cumulative_thresholds: List[float],
    closed_ratio: float,
    from_idx: int = 0,
    epsilon: float = 1e-9,
) -> int:
    """Return the highest tier index ``i >= from_idx`` whose cumulative
    threshold has been satisfied by ``closed_ratio``. Returns ``-1`` when
    no tier has cleared in that range.

    Mirrors ``findHighestClearedTier`` in scheduler/post_tp_sl.go but
    operates on cumulative close-fractions instead of OID slots — the
    backtester doesn't have OIDs, so we infer "tier filled" from the
    fraction of initial quantity that's been closed so far.
    """
    if from_idx < 0:
        from_idx = 0
    highest = -1
    for i in range(from_idx, len(cumulative_thresholds)):
        if closed_ratio + epsilon >= cumulative_thresholds[i]:
            highest = i
    return highest
