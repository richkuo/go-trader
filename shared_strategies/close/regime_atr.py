"""Regime-aware ATR multiplier resolver (#733).

Pure-Python mirror of scheduler/regime_atr.go. Parses the `trend_regime`
block that powers `tiered_tp_atr_regime`, `tiered_tp_atr_live_regime`,
`stop_loss_atr_regime`, and `trailing_stop_atr_regime`.

The Go file is the source of truth for behavior; keep this in sync.
"""

from __future__ import annotations

import sys
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

CANONICAL_TREND_REGIME_LABELS: Tuple[str, ...] = (
    "trending_up",
    "trending_down",
    "ranging",
)

_deprecated_keys_warned: set = set()


def _warn_deprecated_key(old: str, canonical: str) -> None:
    """One-shot stderr deprecation notice for a legacy regime-block key (#841)."""
    if old in _deprecated_keys_warned:
        return
    _deprecated_keys_warned.add(old)
    print(
        f"[DEPRECATED] config key {old!r} is deprecated; use {canonical!r} (#841)",
        file=sys.stderr,
    )


def _regime_entry_atr_raw(entry_raw: dict):
    """Read the canonical 'atr_multiple' trigger from a per-regime entry.
    Setting both atr_multiple and legacy 'atr' is rejected (#841).
    Returns (raw, present, error_msg)."""
    has_canon = "atr_multiple" in entry_raw
    has_legacy = "atr" in entry_raw
    if has_canon and has_legacy:
        return None, False, (
            "set only one of 'atr_multiple' or 'atr' "
            "('atr' is the deprecated alias)"
        )
    if has_canon:
        return entry_raw.get("atr_multiple"), True, None
    return None, False, None


def close_params_are_unified_regime(params) -> bool:
    """Report whether a close ref's params use the #841 unified per-regime block
    (top-level ``trend_regime``) vs the legacy tier-keyed shape."""
    return isinstance(params, dict) and REGIME_CLASSIFIER_KEY in params


def unified_regime_scalar_params(params: dict, regime: str):
    """Select the scalar tiered-close plan for ``regime`` from a unified block.
    Returns (scalar_params, stop_loss_atr) where scalar_params is a plain scalar
    tiered_tp_atr config ({"tp_tiers": [...], "atr_source": ...}), or (None, 0.0)
    when the label is absent/malformed (caller falls back). Mirrors the Go
    unifiedRegimeScalarParams. #841 2b."""
    trend = params.get(REGIME_CLASSIFIER_KEY)
    if not isinstance(trend, dict):
        return None, 0.0
    r = (regime or "").strip()
    label = trend.get(r)
    if not isinstance(label, dict):
        # #1124: a ranging_directional_up/_down stamp falls back to the bare
        # ranging_directional entry (exact match wins first). The unified close
        # is the sole SL owner, so without this fallback a bare-only block
        # yields no SL *and* no TPs for a sub-label stamp. Mirrors
        # unifiedRegimeScalarParams in scheduler/regime_unified.go.
        if r in ("ranging_directional_up", "ranging_directional_down"):
            label = trend.get("ranging_directional")
    if not isinstance(label, dict) or "tp_tiers" not in label:
        return None, 0.0
    scalar = {"tp_tiers": label["tp_tiers"]}
    if "atr_source" in params:
        scalar["atr_source"] = params["atr_source"]
    sl = 0.0
    try:
        sl = float(label.get("stop_loss_atr", 0) or 0)
    except (TypeError, ValueError):
        sl = 0.0
    return scalar, sl

REGIME_CLASSIFIER_KEY = "trend_regime"

# regimeATRSurface equivalents — kept as string constants so the parser's
# error messages match Go's surface-specific allowlists.
SURFACE_STOP_LOSS = "stop_loss"
SURFACE_TRAILING = "trailing"
SURFACE_TP_TIER_ATR_ONLY = "tp_tier_atr_only"
SURFACE_TP_TIER_WITH_FRAC = "tp_tier_with_frac"
SURFACE_SL_AFTER = "sl_after"  # atr_offset variant — signed atr legal (#736)
SURFACE_SL_AFTER_TRAIL = "sl_after_trail"  # trail_from_here variant — strictly positive atr (#736)


@dataclass(frozen=True)
class RegimeATREntry:
    atr: float = 0.0
    close_fraction: float = 0.0
    has_close_frac: bool = False


@dataclass
class RegimeATRBlock:
    use_defaults: bool = False
    trend_regime: Dict[str, RegimeATREntry] = field(default_factory=dict)

    def is_zero(self) -> bool:
        return not self.use_defaults and not self.trend_regime

    def resolve(self, regime: str) -> Optional[RegimeATREntry]:
        if not self.trend_regime:
            return None
        r = (regime or "").strip()
        entry = self.trend_regime.get(r)
        if entry is not None:
            return entry
        # #1124: a ranging_directional_up/_down stamp falls back to the bare
        # ranging_directional entry when no explicit sub-label key is present
        # (the back-compat shape — bare label covers the whole family). Exact
        # match wins first, so an explicit sub key still overrides bare. Mirrors
        # RegimeATRBlock.Resolve in scheduler/regime_atr.go.
        if r in ("ranging_directional_up", "ranging_directional_down"):
            return self.trend_regime.get("ranging_directional")
        return None


# Mirrors regimeATRDefaults in scheduler/regime_atr.go — keep values in sync.
REGIME_ATR_DEFAULTS_STOP_LOSS: Dict[str, RegimeATREntry] = {
    "trending_up": RegimeATREntry(atr=2.0),
    "trending_down": RegimeATREntry(atr=2.0),
    "ranging": RegimeATREntry(atr=1.5),
}

REGIME_ATR_DEFAULTS_TRAILING: Dict[str, RegimeATREntry] = {
    "trending_up": RegimeATREntry(atr=2.5),
    "trending_down": RegimeATREntry(atr=2.5),
    "ranging": RegimeATREntry(atr=2.0),
    # #870 / #1120: composite group opening trails (clean=2.5, choppy=2.25,
    # ranging_quiet=1.0, ranging_volatile=1.25, ranging_directional*=1.5).
    # ADX labels keep their pre-#870 values; resolve() exact-matches keys so the
    # extra composite entries are inert for an ADX strategy.
    "trending_up_clean": RegimeATREntry(atr=2.5),
    "trending_down_clean": RegimeATREntry(atr=2.5),
    "trending_up_choppy": RegimeATREntry(atr=2.25),
    "trending_down_choppy": RegimeATREntry(atr=2.25),
    "ranging_quiet": RegimeATREntry(atr=1.0),
    "ranging_volatile": RegimeATREntry(atr=1.25),
    "ranging_directional": RegimeATREntry(atr=1.5),
    # #1124: directional-drift substates inherit the ranging_directional
    # opening trail (1.5). Explicit entries are required because resolve() is a
    # strict key lookup — without them a use_defaults block would leave these
    # labels unresolved.
    "ranging_directional_up": RegimeATREntry(atr=1.5),
    "ranging_directional_down": RegimeATREntry(atr=1.5),
}

# #870: per-quality-group default ATR take-profit ladders. Mirrors
# regimeTPTierGroupDefaults in scheduler/regime_atr.go. (atr_multiple,
# cumulative_close_fraction); the final rung is coerced to 1.0 by the consumer.
# Tier counts are ragged by design: clean lets trends run (4 rungs), choppy
# mirrors the scalar default (3), ranging scales out fast (2).
# #1152 evaluated a per-substate split of the collapsed ranging group with M6
# entry-locked replay and kept the collapse — see
# docs/research/1152-ranging-exit-geometry-m6.md.
REGIME_TP_TIER_GROUP_DEFAULTS: Dict[str, List[Tuple[float, float]]] = {
    "clean": [(2.5, 0.25), (4.0, 0.50), (5.5, 0.75), (7.0, 1.00)],
    "choppy": [(1.5, 0.40), (3.0, 0.80), (5.0, 1.00)],
    "ranging": [(0.5, 0.50), (1.0, 1.00)],
}


def regime_close_default_group(label: str) -> Optional[str]:
    """Classify a classifier label into one of three default-ladder quality
    groups (#870). Mirrors regimeCloseDefaultGroup in
    scheduler/regime_atr.go: composite quality suffixes win, bare ADX trends
    fall to choppy, the ranging-family maps to ranging."""
    label = (label or "").strip()
    if not label:
        return None
    if label.endswith("_clean"):
        return "clean"
    if label.endswith("_choppy"):
        return "choppy"
    if label.startswith("ranging"):
        return "ranging"
    if label.startswith("trending_up") or label.startswith("trending_down"):
        return "choppy"
    return None


def _default_block_for_surface(surface: str) -> Optional[Dict[str, RegimeATREntry]]:
    if surface == SURFACE_STOP_LOSS:
        return dict(REGIME_ATR_DEFAULTS_STOP_LOSS)
    if surface == SURFACE_TRAILING:
        return dict(REGIME_ATR_DEFAULTS_TRAILING)
    return None


def parse_regime_atr_block(
    raw: Any, ctx_label: str, surface: str, labels: Optional[Tuple[str, ...]] = None
) -> Tuple[RegimeATRBlock, List[str]]:
    """Validate + parse the `trend_regime` shape. Returns (block, errors).

    Mirrors parseRegimeATRBlock in scheduler/regime_atr.go. Accepts either
    {"use_defaults": True} or {"trend_regime": {...}}, never both.

    surface controls which baseline expansion applies for use_defaults and
    whether close_fraction is allowed inside per-regime entries.
    """
    errs: List[str] = []
    labels = tuple(labels or CANONICAL_TREND_REGIME_LABELS)
    if raw is None:
        return RegimeATRBlock(), errs
    if not isinstance(raw, dict):
        errs.append(f"{ctx_label}: must be an object, got {type(raw).__name__}")
        return RegimeATRBlock(), errs

    allowed_top = {"use_defaults", REGIME_CLASSIFIER_KEY}
    for k in raw.keys():
        if k not in allowed_top:
            errs.append(
                f"{ctx_label}: unknown key {k!r} (expected 'use_defaults' or "
                f"{REGIME_CLASSIFIER_KEY!r})"
            )

    use_defaults_raw = raw.get("use_defaults")
    trend_raw = raw.get(REGIME_CLASSIFIER_KEY)
    has_use_defaults = "use_defaults" in raw
    has_trend = REGIME_CLASSIFIER_KEY in raw

    use_defaults = False
    if has_use_defaults:
        if not isinstance(use_defaults_raw, bool):
            errs.append(
                f"{ctx_label}: use_defaults must be a boolean, got "
                f"{type(use_defaults_raw).__name__}"
            )
        else:
            use_defaults = use_defaults_raw

    if use_defaults and has_trend:
        errs.append(
            f"{ctx_label}: cannot combine use_defaults:true with explicit "
            f"{REGIME_CLASSIFIER_KEY} (use_defaults is all-or-nothing)"
        )

    if use_defaults:
        baseline = _default_block_for_surface(surface)
        if baseline is None:
            errs.append(
                f"{ctx_label}: use_defaults not supported on this surface "
                "(tier-level use_defaults is handled by the close evaluator parser)"
            )
            return RegimeATRBlock(), errs
        return RegimeATRBlock(use_defaults=True, trend_regime=baseline), errs

    if not has_trend:
        errs.append(
            f"{ctx_label}: missing {REGIME_CLASSIFIER_KEY!r} (either set "
            "use_defaults:true or supply a trend_regime block)"
        )
        return RegimeATRBlock(), errs

    if not isinstance(trend_raw, dict):
        errs.append(
            f"{ctx_label}: {REGIME_CLASSIFIER_KEY} must be an object, got "
            f"{type(trend_raw).__name__}"
        )
        return RegimeATRBlock(), errs

    valid_labels = set(labels)
    unknown_labels = sorted([k for k in trend_raw.keys() if k not in valid_labels])
    for k in unknown_labels:
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: unknown regime label {k!r} "
            f"(expected one of: {', '.join(labels)})"
        )

    # #1124: a present bare `ranging_directional` covers its _up/_down sub-labels
    # for exhaustiveness (back-compat — resolve() resolves the whole family at
    # runtime via its bare fallback, including the return_eff==0 neutral case the
    # producer still emits). Providing only the sub-labels without the bare label
    # is NOT exhaustive (the neutral case would resolve to None → silent
    # never-arm of an auto-protective exit).
    bare_directional_present = "ranging_directional" in trend_raw
    missing_labels = [
        l for l in labels
        if l not in trend_raw
        and not (
            l in ("ranging_directional_up", "ranging_directional_down")
            and bare_directional_present
        )
    ]
    if missing_labels:
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: missing required regime labels: "
            f"{', '.join(missing_labels)} (must be exhaustive — no silent fallback)"
        )

    result: Dict[str, RegimeATREntry] = {}
    allow_frac = surface == SURFACE_TP_TIER_WITH_FRAC
    allowed_entry_keys = {"atr_multiple"} | (
        {"close_fraction"} if allow_frac else set()
    )

    for label in labels:
        entry_raw = trend_raw.get(label)
        if entry_raw is None:
            continue
        if not isinstance(entry_raw, dict):
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: must be an object, "
                f"got {type(entry_raw).__name__}"
            )
            continue

        entry_unknown = sorted(
            [k for k in entry_raw.keys() if k not in allowed_entry_keys]
        )
        for k in entry_unknown:
            hint = ""
            if k == "close_fraction":
                hint = (
                    " — close_fraction is only allowed inside close-evaluator tiers; "
                    "for SL/trailing/sl_after surfaces, only atr_multiple is accepted"
                )
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: unknown key {k!r}{hint}"
            )

        atr_raw, atr_present, atr_err = _regime_entry_atr_raw(entry_raw)
        if atr_err:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: {atr_err}"
            )
            continue
        if not atr_present:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: missing required 'atr_multiple'"
            )
            continue
        try:
            atr = float(atr_raw)
        except (TypeError, ValueError):
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr_multiple: expected number, "
                f"got {atr_raw!r}"
            )
            continue
        # sl_after atr_offset accepts signed atr (zero = breakeven, negative
        # = SL behind entry). Every other surface requires strictly positive.
        # See #736.
        if surface != SURFACE_SL_AFTER and atr <= 0:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr_multiple: must be > 0, "
                f"got {atr}"
            )
            continue

        entry = RegimeATREntry(atr=atr)
        if allow_frac and "close_fraction" in entry_raw:
            frac_raw = entry_raw["close_fraction"]
            try:
                frac = float(frac_raw)
            except (TypeError, ValueError):
                errs.append(
                    f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.close_fraction: "
                    f"expected number, got {frac_raw!r}"
                )
                continue
            if frac <= 0 or frac > 1:
                errs.append(
                    f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.close_fraction: "
                    f"must be in (0, 1], got {frac}"
                )
                continue
            entry = RegimeATREntry(atr=atr, close_fraction=frac, has_close_frac=True)
        result[label] = entry

    return RegimeATRBlock(trend_regime=result), errs


def resolve_regime_atr(block: RegimeATRBlock, regime: str) -> Optional[float]:
    """Return the ATR multiplier for the given regime label, or None when
    the block is empty / the label is missing."""
    entry = block.resolve(regime)
    if entry is None or entry.atr <= 0:
        return None
    return entry.atr


@dataclass
class RegimeTierSpec:
    """One tier of the regime-aware close evaluator. Resolved per regime
    at runtime by ``resolve_regime_tier``."""

    block: RegimeATRBlock
    # tier_close_fraction is the scalar close_fraction used when the
    # per-regime entries omit close_fraction; None when every per-regime
    # entry carries its own close_fraction.
    tier_close_fraction: Optional[float] = None


def parse_regime_tp_tiers(
    raw_tiers: Any,
    ctx_label: str,
    use_defaults: bool,
    labels: Optional[Tuple[str, ...]] = None,
) -> Tuple[List[RegimeTierSpec], List[str]]:
    """Parse the tier list for tiered_tp_atr_regime / tiered_tp_atr_live_regime.

    Each tier object may take one of two shapes:
      - per-regime close_fraction: {trend_regime: {label: {atr, close_fraction}}}
      - tier-level scalar close_fraction: {trend_regime: {label: {atr}}, close_fraction: X}

    Mixing both shapes within a single tier is rejected. Mixing across tiers
    is fine (one tier per-regime, another tier scalar).
    """
    errs: List[str] = []
    labels = tuple(labels or CANONICAL_TREND_REGIME_LABELS)
    if use_defaults:
        if raw_tiers is not None:
            errs.append(
                f"{ctx_label}: cannot combine use_defaults:true with explicit "
                "tiers (use_defaults is all-or-nothing)"
            )
            return [], errs
        # #870: per-quality-group default ladders (clean 4 / choppy 3 / ranging
        # 2 tiers). Build positional specs as the union across the requested
        # labels — block[i] only carries the labels whose group defines tier i.
        # Callers that resolve a single regime (sl_after) get exactly that
        # group's tier count; resolve_regime_tier skips labels absent from a
        # block. Mirrors Go's defaultRegimeTPTiersForRegime / InspectRegimeTP.
        label_ladders: Dict[str, List[Tuple[float, float]]] = {}
        max_tiers = 0
        for label in labels:
            group = regime_close_default_group(label)
            ladder = REGIME_TP_TIER_GROUP_DEFAULTS.get(group) if group else None
            if not ladder:
                continue
            label_ladders[label] = ladder
            if len(ladder) > max_tiers:
                max_tiers = len(ladder)
        out: List[RegimeTierSpec] = []
        for i in range(max_tiers):
            trend: Dict[str, RegimeATREntry] = {}
            for label, ladder in label_ladders.items():
                if i < len(ladder):
                    mult, frac = ladder[i]
                    trend[label] = RegimeATREntry(
                        atr=mult, close_fraction=frac, has_close_frac=True
                    )
            out.append(
                RegimeTierSpec(
                    block=RegimeATRBlock(use_defaults=True, trend_regime=trend),
                    tier_close_fraction=None,
                )
            )
        return out, errs

    if not isinstance(raw_tiers, list):
        errs.append(
            f"{ctx_label}: tiers must be a list when use_defaults is not set, "
            f"got {type(raw_tiers).__name__}"
        )
        return [], errs

    tiers: List[RegimeTierSpec] = []
    for idx, item in enumerate(raw_tiers):
        if not isinstance(item, dict):
            errs.append(f"{ctx_label}.tiers[{idx}]: must be an object")
            continue

        # Detect shape: does any per-regime entry carry its own close_fraction?
        per_regime_has_frac = False
        trend_block = item.get(REGIME_CLASSIFIER_KEY)
        if isinstance(trend_block, dict):
            for v in trend_block.values():
                if isinstance(v, dict) and "close_fraction" in v:
                    per_regime_has_frac = True
                    break

        tier_level_frac_present = "close_fraction" in item

        if per_regime_has_frac and tier_level_frac_present:
            errs.append(
                f"{ctx_label}.tiers[{idx}]: cannot combine per-regime "
                "close_fraction with tier-level scalar close_fraction "
                "(pick one shape per tier)"
            )
            continue
        if not per_regime_has_frac and not tier_level_frac_present:
            errs.append(
                f"{ctx_label}.tiers[{idx}]: missing close_fraction (either at "
                "tier level or inside every per-regime entry)"
            )
            continue

        surface = (
            SURFACE_TP_TIER_WITH_FRAC if per_regime_has_frac else SURFACE_TP_TIER_ATR_ONLY
        )
        # Strip non-classifier keys we recognize at the tier level before
        # parsing so the inner allowlist check focuses on the trend_regime
        # block shape. Mirrors Go (scheduler/regime_atr.go): close_fraction is
        # handled by the tier-fraction logic below and sl_after by
        # parse_strategy_tp_sl_after_rules — without stripping sl_after, a
        # per-tier sl_after on a regime close failed the parse AND silently
        # never armed at fire time, since the fire path re-parses through here.
        tier_subset = {
            k: v for k, v in item.items() if k not in ("close_fraction", "sl_after")
        }
        sub_label = f"{ctx_label}.tiers[{idx}]"
        block, sub_errs = parse_regime_atr_block(
            tier_subset,
            sub_label,
            surface,
            labels=labels,
        )
        errs.extend(sub_errs)

        tier_frac: Optional[float] = None
        if tier_level_frac_present:
            try:
                tier_frac = float(item["close_fraction"])
            except (TypeError, ValueError):
                errs.append(
                    f"{ctx_label}.tiers[{idx}].close_fraction: expected number, "
                    f"got {item['close_fraction']!r}"
                )
                continue
            if tier_frac <= 0 or tier_frac > 1:
                errs.append(
                    f"{ctx_label}.tiers[{idx}].close_fraction: must be in (0, 1], "
                    f"got {tier_frac}"
                )
                continue

        tiers.append(RegimeTierSpec(block=block, tier_close_fraction=tier_frac))

    return tiers, errs


def resolve_regime_tier(
    spec: RegimeTierSpec, regime: str
) -> Optional[Tuple[float, float]]:
    """Return (atr_multiple, close_fraction) for the given regime, or None
    when the spec / label combination is missing."""
    entry = spec.block.resolve(regime)
    if entry is None or entry.atr <= 0:
        return None
    if spec.tier_close_fraction is not None:
        return entry.atr, spec.tier_close_fraction
    if not entry.has_close_frac or entry.close_fraction <= 0:
        return None
    return entry.atr, entry.close_fraction
