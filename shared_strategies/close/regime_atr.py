"""Regime-aware ATR multiplier resolver (#733).

Pure-Python mirror of scheduler/regime_atr.go. Parses the `trend_regime`
block that powers `tiered_tp_atr_regime`, `tiered_tp_atr_live_regime`,
`stop_loss_atr_regime`, and `trailing_stop_atr_regime`.

The Go file is the source of truth for behavior; keep this in sync.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

CANONICAL_TREND_REGIME_LABELS: Tuple[str, ...] = (
    "trending_up",
    "trending_down",
    "ranging",
)

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
        return self.trend_regime.get((regime or "").strip())


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
}

# Tier defaults: positional list. Each entry is one tier's regime block with
# per-regime close_fraction. Final tier close_fraction is coerced to 1.0 by
# downstream consumers.
REGIME_ATR_DEFAULTS_TP_TIERS: List[RegimeATRBlock] = [
    RegimeATRBlock(
        trend_regime={
            "trending_up": RegimeATREntry(atr=2.0, close_fraction=0.5, has_close_frac=True),
            "trending_down": RegimeATREntry(atr=2.0, close_fraction=0.5, has_close_frac=True),
            "ranging": RegimeATREntry(atr=1.5, close_fraction=0.5, has_close_frac=True),
        }
    ),
    RegimeATRBlock(
        trend_regime={
            "trending_up": RegimeATREntry(atr=4.0, close_fraction=1.0, has_close_frac=True),
            "trending_down": RegimeATREntry(atr=4.0, close_fraction=1.0, has_close_frac=True),
            "ranging": RegimeATREntry(atr=2.5, close_fraction=1.0, has_close_frac=True),
        }
    ),
]


def _default_block_for_surface(surface: str) -> Optional[Dict[str, RegimeATREntry]]:
    if surface == SURFACE_STOP_LOSS:
        return dict(REGIME_ATR_DEFAULTS_STOP_LOSS)
    if surface == SURFACE_TRAILING:
        return dict(REGIME_ATR_DEFAULTS_TRAILING)
    return None


def _default_entry_for_label(
    baseline: Dict[str, RegimeATREntry],
    label: str,
) -> Optional[RegimeATREntry]:
    if label in baseline:
        return baseline[label]
    if label.startswith("trending_up"):
        return baseline.get("trending_up")
    if label.startswith("trending_down"):
        return baseline.get("trending_down")
    if label.startswith("ranging"):
        return baseline.get("ranging")
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

    missing_labels = [l for l in labels if l not in trend_raw]
    if missing_labels:
        errs.append(
            f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: missing required regime labels: "
            f"{', '.join(missing_labels)} (must be exhaustive — no silent fallback)"
        )

    result: Dict[str, RegimeATREntry] = {}
    allow_frac = surface == SURFACE_TP_TIER_WITH_FRAC
    allowed_entry_keys = {"atr"} | ({"close_fraction"} if allow_frac else set())

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
                    "for SL/trailing/sl_after surfaces, only atr is accepted"
                )
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: unknown key {k!r}{hint}"
            )

        atr_raw = entry_raw.get("atr")
        if atr_raw is None:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: missing required 'atr'"
            )
            continue
        try:
            atr = float(atr_raw)
        except (TypeError, ValueError):
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr: expected number, "
                f"got {atr_raw!r}"
            )
            continue
        # sl_after atr_offset accepts signed atr (zero = breakeven, negative
        # = SL behind entry). Every other surface requires strictly positive.
        # See #736.
        if surface != SURFACE_SL_AFTER and atr <= 0:
            errs.append(
                f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr: must be > 0, "
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
        # Use the default tier list. Each default tier carries per-regime
        # close_fraction (HasCloseFrac=True).
        out: List[RegimeTierSpec] = []
        for default_block in REGIME_ATR_DEFAULTS_TP_TIERS:
            # Deep copy and expand composite labels onto their ADX-family
            # defaults so Python mirrors Go's defaultRegimeTPTiersForRegime.
            expanded: Dict[str, RegimeATREntry] = {}
            for label in labels:
                entry = _default_entry_for_label(default_block.trend_regime, label)
                if entry is not None:
                    expanded[label] = entry
            block_copy = RegimeATRBlock(
                use_defaults=True,
                trend_regime=expanded,
            )
            out.append(RegimeTierSpec(block=block_copy, tier_close_fraction=None))
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
        # block shape.
        tier_subset = {k: v for k, v in item.items() if k != "close_fraction"}
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
