"""Trailing-stop tier ratchet close evaluators (#844).

Each tier tightens the trailing ATR distance and may scale out (``close_fraction``
may be 0 for trail-only rungs). Regime is frozen at open via ``position["regime"]``.
"""

from __future__ import annotations

from typing import List, Optional, Tuple

from _helpers import (
    clamp_fraction,
    current_close_fraction,
    float_from,
    tier_list_from_params,
)
from regime_atr import CANONICAL_TREND_REGIME_LABELS, regime_close_default_group

# DEFAULT_RATCHET_TIERS is the conservative fallback ladder (#866) used when the
# SCALAR trailing_tp_ratchet omits tp_tiers (or sets use_defaults:true). It
# mirrors the Go single source of truth defaultTrailingRatchetTiers() in
# scheduler/trailing_tp_ratchet.go — keep the two in sync. Precondition: the
# first rung tightens to 1.5xATR, so a strategy using this default must set
# trailing_stop_atr_mult >= 1.5 (else the Go loader rejects it via
# validateTrailingRatchetInitialTrail).
DEFAULT_RATCHET_TIERS = [
    {"atr_multiple": 2.0, "trailing_mult_after": 1.5, "close_fraction": 0.0},
    {"atr_multiple": 2.5, "trailing_mult_after": 1.0, "close_fraction": 0.0},
    {"atr_multiple": 3.0, "trailing_mult_after": 0.8, "close_fraction": 0.0},
]

# #870: per-group default ratchet ladders for the REGIME variant
# (trailing_tp_ratchet_regime) when tp_tiers is omitted / use_defaults:true.
# Mirrors ratchetTierGroupDefaults in scheduler/trailing_tp_ratchet.go. Trend
# groups (clean/choppy) are pure let-it-ride (close_fraction 0). #1059 split the
# single ranging ladder into three composite substate ladders keyed by
# ratchet_close_default_group (NOT the shared regime_close_default_group, which
# still collapses ranging* → "ranging" for the B2 ATR-TP path):
#   - ranging_quiet keeps the pre-#1059 ranging geometry and is also the target
#     for bare ADX "ranging", so ADX behavior is unchanged.
#   - ranging_volatile widens the triggers (avoid scaling out on wide-range
#     noise); close fractions unchanged.
#   - ranging_directional scales out lighter early (25/50/75) and adds a 4th
#     let-ride rung that only tightens the trail (no extra close) so a nascent
#     breakout's runner survives.
# Each group's first rung couples to that group's opening trail in
# REGIME_ATR_DEFAULTS_TRAILING (#1120: clean 2.5 / choppy 2.25 / ranging_quiet 1.0
# / ranging_volatile 1.25 / ranging_directional* 1.5).
# #1152 validated the ranging split with M6 entry-locked replay (see
# docs/research/1152-ranging-exit-geometry-m6.md): volatile + directional
# incumbents stand (directional's let-ride runner survived its inverse check);
# ranging_quiet is unevaluable on the audit data (label ≈0.2–0.9% of bars) and
# keeps its pre-#1059 geometry on that documented evidence gap.
DEFAULT_RATCHET_TIERS_BY_GROUP = {
    "clean": [
        {"atr_multiple": 3.0, "trailing_mult_after": 1.5, "close_fraction": 0.0},
        {"atr_multiple": 4.5, "trailing_mult_after": 1.0, "close_fraction": 0.0},
        {"atr_multiple": 6.0, "trailing_mult_after": 0.8, "close_fraction": 0.0},
    ],
    "choppy": [
        {"atr_multiple": 2.0, "trailing_mult_after": 1.5, "close_fraction": 0.0},
        {"atr_multiple": 2.5, "trailing_mult_after": 1.0, "close_fraction": 0.0},
        {"atr_multiple": 3.0, "trailing_mult_after": 0.8, "close_fraction": 0.0},
    ],
    "ranging_quiet": [
        {"atr_multiple": 0.75, "trailing_mult_after": 1.0, "close_fraction": 0.4},
        {"atr_multiple": 1.5, "trailing_mult_after": 0.75, "close_fraction": 0.8},
        {"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 1.0},
    ],
    "ranging_volatile": [
        {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.4},
        {"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 0.8},
        {"atr_multiple": 3.0, "trailing_mult_after": 0.75, "close_fraction": 1.0},
    ],
    "ranging_directional": [
        {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.25},
        {"atr_multiple": 2.0, "trailing_mult_after": 1.0, "close_fraction": 0.50},
        {"atr_multiple": 3.0, "trailing_mult_after": 0.8, "close_fraction": 0.75},
        {"atr_multiple": 4.5, "trailing_mult_after": 0.6, "close_fraction": 0.75},
    ],
}


def ratchet_close_default_group(label: str) -> Optional[str]:
    """Resolve a classifier label to a ratchet default-ladder group key (#1059).

    Unlike the shared regime_close_default_group (which collapses every ranging*
    → "ranging" for the B2 ATR-TP path), the ratchet ladder differentiates the
    three composite ranging substates. Bare ADX "ranging" maps to the quiet
    ladder (pre-#1059 behavior). clean/choppy and ADX-trend labels delegate
    unchanged to regime_close_default_group. Mirrors ratchetCloseDefaultGroup in
    scheduler/trailing_tp_ratchet.go."""
    l = (label or "").strip()
    if l in ("ranging_quiet", "ranging_volatile", "ranging_directional"):
        return l
    # #1124: the directional-drift substates share the ranging_directional
    # scale-out ladder (the geometry is direction-agnostic — the SL side carries
    # direction, the TP scale-out does not), so map them to that group rather
    # than fall through to regime_close_default_group's "ranging" (which has no
    # ratchet ladder → silent never-arm of the protective exit).
    if l in ("ranging_directional_up", "ranging_directional_down"):
        return "ranging_directional"
    if l == "ranging":
        return "ranging_quiet"
    return regime_close_default_group(l)


def _first_present(d: dict, *keys: str):
    for k in keys:
        if k in d:
            return d[k]
    return None


def resolve_trailing_mult_after(tier: dict, firing_multiple: float) -> Optional[float]:
    """Return the new trail distance in ATR units for a firing tier."""
    has_abs = "trailing_mult_after" in tier
    has_frac = "tp_atr_fraction" in tier
    if has_abs and has_frac:
        return None
    if has_abs:
        try:
            mult = float(tier["trailing_mult_after"])
        except (TypeError, ValueError):
            return None
        return mult if mult > 0 else None
    if has_frac:
        try:
            frac = float(tier["tp_atr_fraction"])
        except (TypeError, ValueError):
            return None
        if frac <= 0 or firing_multiple <= 0:
            return None
        return frac * firing_multiple
    return None


def _parse_tier_dict(tier: dict) -> Optional[Tuple[float, float, Optional[float]]]:
    """Return (atr_multiple, close_fraction, trailing_mult) or None when invalid."""
    if not isinstance(tier, dict):
        return None
    mult_raw = _first_present(tier, "atr_multiple", "multiple", "atr")
    frac_raw = _first_present(tier, "close_fraction", "fraction")
    try:
        mult = float(mult_raw)
        frac = float(frac_raw if frac_raw is not None else 0.0)
    except (TypeError, ValueError):
        return None
    if mult <= 0 or frac < 0 or frac > 1:
        return None
    trail = resolve_trailing_mult_after(tier, mult)
    if trail is None or trail <= 0:
        return None
    return mult, clamp_fraction(frac), trail


def _parse_scalar_tiers(raw) -> Tuple[List[Tuple[float, float, float]], List[str]]:
    if not isinstance(raw, list):
        return [], ["tp_tiers must be a list for trailing_tp_ratchet"]
    parsed: List[Tuple[float, float, float]] = []
    for idx, item in enumerate(raw):
        row = _parse_tier_dict(item) if isinstance(item, dict) else None
        if row is None:
            continue
        parsed.append(row)
    parsed.sort(key=lambda p: p[0])
    if not parsed:
        return [], ["tp_tiers must contain at least one valid tier"]
    errs = _validate_monotonic_tiers(parsed)
    if errs:
        return [], errs
    return parsed, []


def _validate_monotonic_tiers(tiers: List[Tuple[float, float, float]]) -> List[str]:
    errs: List[str] = []
    for idx in range(1, len(tiers)):
        prev_mult, prev_frac, prev_trail = tiers[idx - 1]
        _cur_mult, cur_frac, cur_trail = tiers[idx]
        if cur_trail > prev_trail + 1e-12:
            errs.append(
                f"tp_tiers[{idx}].trailing distance {cur_trail:g} must be "
                f"<= tier[{idx - 1}] ({prev_trail:g})"
            )
        if cur_frac + 1e-12 < prev_frac:
            errs.append(
                f"tp_tiers[{idx}].close_fraction {cur_frac:g} must be "
                f">= tier[{idx - 1}] close_fraction {prev_frac:g}"
            )
    return errs


def _parse_regime_table(raw, labels) -> Tuple[List[Tuple[float, float, float]], List[str]]:
    if not isinstance(raw, dict):
        return [], ["tp_tiers must be a regime-keyed object for trailing_tp_ratchet_regime"]
    label_set = set(labels or CANONICAL_TREND_REGIME_LABELS)
    errs: List[str] = []
    for key in raw:
        if key not in label_set:
            errs.append(f"tp_tiers: unknown regime key {key!r}")
    if errs:
        return [], errs
    # Validation-only path may pass regime="" — caller supplies table per open.
    return [], []


def resolve_tiers_for_regime(
    params: dict, regime: str, *, regime_table: bool
) -> Tuple[List[Tuple[float, float, float]], List[str]]:
    """Resolve concrete tiers for the stamped regime label."""
    raw = tier_list_from_params(params)
    if raw is None:
        # Omitted tp_tiers (or use_defaults:true) resolves to the system default
        # ladder. #870: the regime variant resolves the per-quality-group ladder
        # for the stamped regime; the scalar variant uses the single #866 default.
        if regime_table:
            group = ratchet_close_default_group(regime)
            ladder = DEFAULT_RATCHET_TIERS_BY_GROUP.get(group) if group else None
            if not ladder:
                return [], []
            return _parse_scalar_tiers([dict(t) for t in ladder])
        return _parse_scalar_tiers([dict(t) for t in DEFAULT_RATCHET_TIERS])
    if regime_table:
        if not isinstance(raw, dict):
            return [], ["tp_tiers must be a regime-keyed object"]
        label = (regime or "").strip()
        if not label:
            return [], ["noop:missing_position_regime"]
        block = raw.get(label)
        if block is None:
            return [], [f"tp_tiers: missing regime key {label!r}"]
        tiers, terr = _parse_scalar_tiers(block)
        return tiers, terr
    if isinstance(raw, dict):
        block = raw.get("default", raw.get("ranging"))
        if block is None:
            return [], ["tp_tiers: expected list or object with 'default' key"]
        tiers, terr = _parse_scalar_tiers(block)
        return tiers, terr
    return _parse_scalar_tiers(raw)


def evaluate_scalar(position: dict, market: dict, params: dict) -> dict:
    return _evaluate(position, market, params, regime_table=False)


def evaluate_regime(position: dict, market: dict, params: dict) -> dict:
    return _evaluate(position, market, params, regime_table=True)


def _evaluate(position: dict, market: dict, params: dict, *, regime_table: bool) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    entry_atr = float_from(position, "entry_atr")
    side = str(position.get("side", "") or "").strip().lower()
    regime = str(position.get("regime", "") or "").strip()
    mark_price = float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if entry_atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
    if regime_table and not regime:
        return {"close_fraction": 0.0, "reason": "noop:missing_position_regime"}

    tiers, errs = resolve_tiers_for_regime(params, regime, regime_table=regime_table)
    if errs or not tiers:
        return {"close_fraction": 0.0, "reason": "noop:tier_resolution_failed"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    hit = [(m, f, t) for m, f, t in tiers if atr_profit >= m]
    if not hit:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction, _trail = hit[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    tag = "trailing_tp_ratchet_regime" if regime_table else "trailing_tp_ratchet"
    suffix = f":{regime}" if regime_table else ""
    return {
        "close_fraction": close_fraction,
        "reason": f"{tag}{suffix}:{multiple:g}",
    }


def maybe_apply_mark_ratchet(
    tiers: List[Tuple[float, float, float]],
    *,
    watermark: int,
    mark_price: float,
    avg_cost: float,
    entry_atr: float,
    side: str,
    post_tp_trail_mult: Optional[float],
    trailing_stop_atr_mult: float,
) -> tuple[int, Optional[float]]:
    """Return (new_watermark, new_post_tp_trail_mult) after mark-based tier clears."""
    if not tiers or mark_price <= 0 or avg_cost <= 0 or entry_atr <= 0:
        return watermark, post_tp_trail_mult
    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    hit_idx = -1
    for i in range(watermark, len(tiers)):
        if atr_profit + 1e-12 >= tiers[i][0]:
            hit_idx = i
    if hit_idx < 0:
        return watermark, post_tp_trail_mult
    new_mult = tiers[hit_idx][2]
    current = post_tp_trail_mult if post_tp_trail_mult and post_tp_trail_mult > 0 else trailing_stop_atr_mult
    out_mult = post_tp_trail_mult
    if new_mult < current - 1e-12:
        out_mult = new_mult
    return hit_idx + 1, out_mult
