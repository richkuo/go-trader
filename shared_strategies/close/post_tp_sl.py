"""Post-TP stop-loss adjustment helpers (`sl_after` rules).

Pure-Python mirror of scheduler/post_tp_sl.go. Used by the backtester (#709)
to simulate the same SL bumps the live HL/manual paths do after a tiered TP
fills. The Go file is the source of truth for behavior; keep this in sync.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable, List, Optional, Tuple

# Absolute import (not relative) so this module loads cleanly under
# importlib.util.spec_from_file_location — the backtester tests use that
# loader to sidestep the open/close registry.py name collision, and
# relative imports require a parent-package context that the loader
# doesn't set up.
from shared_strategies.close.regime_atr import (
    REGIME_CLASSIFIER_KEY,
    SURFACE_SL_AFTER,
    SURFACE_SL_AFTER_TRAIL,
    RegimeATRBlock,
    parse_regime_atr_block,
)


_TIERED_TP_NAMES = ("tiered_tp_atr", "tiered_tp_atr_live")


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

    def is_empty(self) -> bool:
        return self.kind == ""

    def has_regime(self) -> bool:
        return self.atr_regime is not None or self.trail_atr_regime is not None

    def resolve_for_regime(self, regime: str) -> Optional["SLAfterRule"]:
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
        return self


@dataclass
class TierSLAfterRules:
    """Strategy-level default + per-tier overrides, aligned with the parsed
    tiers (ascending by ``atr_multiple``)."""

    default: SLAfterRule = field(default_factory=SLAfterRule)
    per_tier: List[SLAfterRule] = field(default_factory=list)

    def for_tier(self, idx: int) -> SLAfterRule:
        if 0 <= idx < len(self.per_tier) and not self.per_tier[idx].is_empty():
            return self.per_tier[idx]
        return self.default

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


def parse_sl_after_rule(raw: Any) -> SLAfterRule:
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
                return _parse_sl_after_atr_offset(raw, "sl_after kind=atr_offset")
            if kind == "trail_from_here":
                return _parse_sl_after_trail_from_here(raw, "sl_after kind=trail_from_here")
            raise ValueError(f"sl_after kind {kind!r} is not recognized")
        if "trail_from_here" in raw:
            trail_raw = raw["trail_from_here"]
            if not isinstance(trail_raw, dict):
                raise ValueError(
                    f"sl_after.trail_from_here must be an object, got "
                    f"{type(trail_raw).__name__}"
                )
            return _parse_sl_after_trail_from_here(trail_raw, "sl_after.trail_from_here")
        if REGIME_CLASSIFIER_KEY in raw:
            return _parse_sl_after_atr_offset(raw, "sl_after")
        if _first_non_nil(raw, "atr_mult", "atr_offset"):
            return _parse_sl_after_atr_offset(raw, "sl_after atr_mult")
        raise ValueError(
            'sl_after object must contain "kind", "atr_mult", "trail_from_here", '
            'or "trend_regime"'
        )
    raise ValueError(
        f"sl_after must be a string or object, got {type(raw).__name__}"
    )


def _parse_sl_after_atr_offset(m: dict, ctx_label: str) -> SLAfterRule:
    """Parse the atr_offset variant — scalar (atr_mult/atr_offset) or regime
    (trend_regime/use_defaults). Multi-label regime errors join with '; '
    so callers that surface a single error per field stay compatible."""
    has_trend = REGIME_CLASSIFIER_KEY in m
    has_use_defaults = "use_defaults" in m
    if has_trend or has_use_defaults:
        if _first_non_nil(m, "atr_mult", "atr_offset"):
            raise ValueError(
                f"{ctx_label}: cannot combine scalar atr_mult with "
                "trend_regime/use_defaults — pick one shape"
            )
        regime_raw: dict = {}
        if has_trend:
            regime_raw[REGIME_CLASSIFIER_KEY] = m[REGIME_CLASSIFIER_KEY]
        if has_use_defaults:
            regime_raw["use_defaults"] = m["use_defaults"]
        block, errs = parse_regime_atr_block(regime_raw, ctx_label, SURFACE_SL_AFTER)
        if errs:
            raise ValueError("; ".join(errs))
        return SLAfterRule(kind="atr_offset", atr_regime=block)
    mult = _float_or_raise(
        _first_present(m, "atr_mult", "atr_offset"),
        ctx_label,
    )
    return SLAfterRule(kind="atr_offset", atr_mult=mult)


def _parse_sl_after_trail_from_here(m: dict, ctx_label: str) -> SLAfterRule:
    """Parse the trail_from_here variant — scalar (atr_mult/trail_atr_mult)
    or regime (trend_regime/use_defaults). Regime form uses
    SURFACE_SL_AFTER_TRAIL so per-label atr must be strictly positive."""
    has_trend = REGIME_CLASSIFIER_KEY in m
    has_use_defaults = "use_defaults" in m
    if has_trend or has_use_defaults:
        if _first_non_nil(m, "atr_mult", "trail_atr_mult"):
            raise ValueError(
                f"{ctx_label}: cannot combine scalar atr_mult with "
                "trend_regime/use_defaults — pick one shape"
            )
        regime_raw: dict = {}
        if has_trend:
            regime_raw[REGIME_CLASSIFIER_KEY] = m[REGIME_CLASSIFIER_KEY]
        if has_use_defaults:
            regime_raw["use_defaults"] = m["use_defaults"]
        block, errs = parse_regime_atr_block(
            regime_raw, ctx_label, SURFACE_SL_AFTER_TRAIL
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


def validate_sl_after_rule(rule: SLAfterRule) -> None:
    """Sanity-check a parsed rule. Raises ``ValueError`` on bad shapes; the
    empty rule passes silently."""
    if rule.kind == "":
        return
    if rule.kind == "breakeven":
        if rule.atr_regime is not None or rule.trail_atr_regime is not None:
            raise ValueError("sl_after breakeven does not accept a trend_regime block")
        return
    if rule.kind == "atr_offset":
        if rule.trail_atr_regime is not None:
            raise ValueError(
                "sl_after atr_offset accepts trend_regime under atr, not "
                "trail_from_here.atr"
            )
        return
    if rule.kind == "trail_from_here":
        if rule.atr_regime is not None:
            raise ValueError(
                "sl_after trail_from_here accepts trend_regime under "
                "trail_from_here.atr, not at the top level"
            )
        if rule.trail_atr_regime is None and rule.trail_atr_mult <= 0:
            raise ValueError("sl_after trail_from_here requires atr_mult > 0")
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
) -> Tuple[TierSLAfterRules, List[str]]:
    """Walk the strategy's close refs and extract the strategy-level default
    and per-tier sl_after rules from the first ``tiered_tp_atr*`` entry.

    Returns ``(rules, errs)``. Errors describe individual malformed fields;
    the parser still returns whatever it could so the caller can surface the
    problems at config-load time without losing the rest of the config.
    """
    rules = TierSLAfterRules()
    errs: List[str] = []
    if not _strategy_uses_tiered_tp_atr_close(close_refs):
        return rules, errs
    default_raw: Any = None
    tiers_raw: Any = None
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name not in _TIERED_TP_NAMES:
            continue
        params = ref.get("params") or {}
        if "sl_after" in params:
            default_raw = params["sl_after"]
        if "tiers" in params:
            tiers_raw = params["tiers"]
        break
    if default_raw is not None:
        try:
            r = parse_sl_after_rule(default_raw)
            validate_sl_after_rule(r)
            rules.default = r
        except ValueError as e:
            errs.append(f"sl_after (strategy-level): {e}")
    if not isinstance(tiers_raw, list):
        return rules, errs
    pairs: List[Tuple[float, SLAfterRule]] = []
    for idx, item in enumerate(tiers_raw):
        if not isinstance(item, dict):
            continue
        mult_raw = _first_present(item, "atr_multiple", "multiple")
        try:
            mult = float(mult_raw) if mult_raw is not None else 0.0
        except (TypeError, ValueError):
            continue
        if mult <= 0:
            continue
        rule = SLAfterRule()
        if item.get("sl_after") is not None:
            try:
                parsed = parse_sl_after_rule(item["sl_after"])
                validate_sl_after_rule(parsed)
                rule = parsed
            except ValueError as e:
                errs.append(f"sl_after (tier[{idx}]): {e}")
        pairs.append((mult, rule))
    pairs.sort(key=lambda p: p[0])
    rules.per_tier = [p[1] for p in pairs]
    return rules, errs


def validate_post_tp_stop_loss_rules(
    close_refs: Iterable[dict],
    *,
    stop_loss_atr_mult: Optional[float] = None,
    stop_loss_pct: Optional[float] = None,
    stop_loss_margin_pct: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    trailing_stop_pct: Optional[float] = None,
    strategy_type: str = "perps",
) -> List[str]:
    """Mirror ``validatePostTPStopLossRules`` in scheduler/post_tp_sl.go.

    Conditions enforced:
      - shape/field-level errors from parsing
      - reject sl_after on non-tiered_tp_atr* close refs (silent no-op in
        live, so we fail loud at load)
      - reject combination with a strategy-level trailing stop
      - require a fixed stop-loss to adjust
      - reject trail_from_here on manual strategies (perps-only in v1)
    """
    close_refs = list(close_refs)
    rules, parse_errs = parse_strategy_tp_sl_after_rules(close_refs)
    out: List[str] = list(parse_errs)
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name in _TIERED_TP_NAMES:
            continue
        params = ref.get("params") or {}
        if "sl_after" in params:
            out.append(
                f"sl_after is only honored on tiered_tp_atr / "
                f"tiered_tp_atr_live close refs; found on {ref.get('name')!r}"
            )
        tiers_raw = params.get("tiers")
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
    has_fixed_sl = (
        (stop_loss_atr_mult is not None and stop_loss_atr_mult > 0)
        or (stop_loss_pct is not None and stop_loss_pct > 0)
        or (stop_loss_margin_pct is not None and stop_loss_margin_pct > 0)
    )
    if not has_fixed_sl:
        out.append(
            "sl_after requires a fixed stop-loss to adjust (set "
            "stop_loss_atr_mult, stop_loss_pct, or stop_loss_margin_pct)"
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


def parse_tp_tier_close_fractions(close_refs: Iterable[dict]) -> List[float]:
    """Return cumulative ``close_fraction`` values for the strategy's
    ``tiered_tp_atr*`` close ref (sorted by ascending ``atr_multiple``).

    Used by the backtester (#709) to detect which tier just fired by
    comparing the post-bar ``closed_qty / initial_qty`` ratio against the
    cumulative thresholds. Returns an empty list when no tiered ATR ref is
    configured. Final-tier close_fraction is coerced to 1.0 to match the
    live ``strategyTPTiers`` behavior.
    """
    for ref in close_refs:
        name = (ref.get("name") or "").strip().lower()
        if name not in _TIERED_TP_NAMES:
            continue
        tiers_raw = (ref.get("params") or {}).get("tiers")
        if not isinstance(tiers_raw, list):
            return []
        pairs: List[Tuple[float, float]] = []
        for item in tiers_raw:
            if not isinstance(item, dict):
                continue
            mult_raw = _first_present(item, "atr_multiple", "multiple")
            frac_raw = _first_present(item, "close_fraction", "fraction")
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
