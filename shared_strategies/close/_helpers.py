
from __future__ import annotations

import math
import sys
from collections import namedtuple
from decimal import Decimal
from typing import Optional

_deprecated_close_keys_warned: set[str] = set()

FLOAT_NOISE_RELATIVE_TOLERANCE = 1e-9


def warn_deprecated_close_key(old: str, canonical: str) -> None:
    token = f"{old}->{canonical}"
    if token in _deprecated_close_keys_warned:
        return
    _deprecated_close_keys_warned.add(token)
    print(
        f"[DEPRECATED] close config key {old!r} is deprecated; use {canonical!r} (#841)",
        file=sys.stderr,
    )


def tier_list_from_params(params: dict):
    if not isinstance(params, dict):
        return None
    return params.get("tp_tiers")


def float_from(mapping: dict, key: str) -> float:
    try:
        return float(mapping.get(key, 0) or 0)
    except (TypeError, ValueError):
        return 0.0


def clamp_fraction(value) -> float:
    try:
        fraction = float(value)
    except (TypeError, ValueError):
        return 0.0
    return min(max(fraction, 0.0), 1.0)


def current_close_fraction(position: dict, target_closed_fraction: float) -> float:
    current_qty = float_from(position, "current_quantity")
    initial_qty = float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed_qty = max(initial_qty - current_qty, 0.0)
    target_closed_qty = initial_qty * clamp_fraction(target_closed_fraction)
    qty_to_close = min(max(target_closed_qty - already_closed_qty, 0.0), current_qty)
    if qty_to_close <= initial_qty * FLOAT_NOISE_RELATIVE_TOLERANCE:
        return 0.0
    if qty_to_close >= current_qty * (1.0 - FLOAT_NOISE_RELATIVE_TOLERANCE):
        return 1.0
    return clamp_fraction(qty_to_close / current_qty)


TP_MODEL_RESTING_LIMIT = "resting_limit"


def resting_limit_model(position: dict) -> bool:
    if not isinstance(position, dict):
        return False
    return str(position.get("tp_model", "") or "").strip().lower() == TP_MODEL_RESTING_LIMIT


def geometry_anchor(position: dict) -> float:
    anchor = float_from(position, "risk_anchor_price")
    if anchor > 0:
        return anchor
    return float_from(position, "avg_cost")


def tier_number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def parse_resting_tp_tiers(raw) -> list[tuple[float, float]]:
    if not isinstance(raw, (list, tuple)):
        return []
    parsed = []
    for tier in raw:
        if not isinstance(tier, dict):
            continue
        multiple = tier_number(tier.get("atr_multiple"))
        fraction = tier_number(tier.get("close_fraction"))
        if multiple is None or fraction is None:
            continue
        if multiple <= 0 or fraction <= 0:
            continue
        parsed.append((multiple, min(fraction, 1.0)))
    parsed.sort(key=lambda item: item[0])
    return parsed


def finalize_tp_tiers(pairs):
    tiers = [(float(multiple), float(fraction)) for multiple, fraction in pairs]
    if len(tiers) < 2:
        return tiers
    previous = 0.0
    for multiple, fraction in tiers:
        if multiple <= 0 or fraction <= previous:
            return None
        previous = fraction
    tiers[-1] = (tiers[-1][0], 1.0)
    return tiers


TierGeometry = namedtuple(
    "TierGeometry",
    ("anchor", "atr", "atr_label", "regime", "regime_label", "resting_limit"),
)


def _normalized_atr_source(params: dict) -> str:
    source = str((params or {}).get("atr_source", "live") or "live").strip().lower()
    return source if source in ("live", "entry") else "live"


def _market_atr(market: dict) -> float:
    live_atr = float_from(market, "atr")
    if live_atr <= 0:
        live_atr = float_from(market, "live_atr")
    return live_atr


def _resolve_tier_atr(position: dict, market: dict, params: dict, live_atr: bool, resting: bool) -> tuple[float, str]:
    entry_atr = float_from(position, "entry_atr")
    if not live_atr:
        return entry_atr, "entry"
    atr_source = _normalized_atr_source(params)
    if resting:
        if entry_atr > 0:
            return entry_atr, "entry"
        if atr_source == "entry":
            return 0.0, "entry"
        market_atr = _market_atr(market)
        if market_atr > 0:
            return market_atr, "live"
        return 0.0, "missing"
    if atr_source == "entry":
        return entry_atr, "entry"
    market_atr = _market_atr(market)
    if market_atr > 0:
        return market_atr, "live"
    if entry_atr > 0:
        return entry_atr, "entry_fallback"
    return 0.0, "missing"


def _resolve_tier_regime(position: dict, market: dict, regime_source: str, resting: bool) -> tuple[str, str]:
    position_regime = str((position or {}).get("regime", "") or "").strip()
    if regime_source == "position":
        return position_regime, "frozen" if position_regime else ""
    if regime_source != "live":
        return "", ""
    market_regime = str((market or {}).get("regime", "") or "").strip()
    if resting:
        if position_regime:
            return position_regime, "frozen"
        return market_regime, "live" if market_regime else ""
    if market_regime:
        return market_regime, "live"
    return position_regime, "frozen" if position_regime else ""


def resolve_tp_tier_geometry(position: dict, market: dict, params: dict, *, live_atr: bool, regime_source: str = "") -> TierGeometry:
    resting = resting_limit_model(position)
    atr, atr_label = _resolve_tier_atr(position, market, params, live_atr, resting)
    regime, regime_label = _resolve_tier_regime(position, market, regime_source, resting)
    return TierGeometry(
        anchor=geometry_anchor(position),
        atr=atr,
        atr_label=atr_label,
        regime=regime,
        regime_label=regime_label,
        resting_limit=resting,
    )


def tier_price(side: str, anchor: float, atr: float, multiple: float) -> float:
    offset = multiple * atr
    return anchor - offset if side == "short" else anchor + offset


def resting_tier_fill_price(position: dict, hit_tiers, side: str, anchor: float, atr: float) -> float:
    if not hit_tiers or anchor <= 0 or atr <= 0:
        return 0.0
    current_qty = float_from(position, "current_quantity")
    initial_qty = float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed = max(initial_qty - current_qty, 0.0)
    weighted = 0.0
    filled = 0.0
    previous_target = 0.0
    for multiple, cumulative in hit_tiers:
        low = max(previous_target * initial_qty, already_closed)
        high = min(clamp_fraction(cumulative) * initial_qty, already_closed + current_qty)
        previous_target = clamp_fraction(cumulative)
        if high <= low:
            continue
        weighted += (high - low) * tier_price(side, anchor, atr, multiple)
        filled += high - low
    if filled <= 0:
        multiple = hit_tiers[-1][0]
        return tier_price(side, anchor, atr, multiple)
    return weighted / filled


def with_tier_fill_price(result: dict, position: dict, geometry: TierGeometry, hit_tiers, side: str) -> dict:
    if not geometry.resting_limit or result.get("close_fraction", 0.0) <= 0:
        return result
    price = resting_tier_fill_price(position, hit_tiers, side, geometry.anchor, geometry.atr)
    if price > 0 and math.isfinite(price):
        result["tier_fill_price"] = price
    return result


RESTING_TP_TRADE_THROUGH_TICKS = 1
RESTING_TP_RULE_KEY = "resting_tp_rule"
RESTING_TP_RULE_VERSION = 1
RESTING_TP_TIER_CLOSES = (
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "tiered_tp_atr_regime",
    "tiered_tp_atr_live_regime",
)
RESTING_TP_UNSUPPORTED_CLOSES = ("tiered_tp_atr_live_regime_dynamic",)
RESTING_TP_COVERAGES = ("complete", "frame_truncated")
_RESTING_TICK_CAP = 100000


class RestingRuleInvalid(ValueError):

    def __init__(self, field: str):
        self.field = field
        super().__init__(field)


def hl_perps_px_decimals(px: float, sz_decimals: int) -> int:
    px_decimals = max(0, 6 - sz_decimals)
    log = math.floor(math.log10(abs(px)))
    sig_decimals = max(0, 5 - 1 - int(log))
    return min(px_decimals, sig_decimals)


def hl_round_limit(px: float, sz_decimals: int) -> Decimal:
    return Decimal(repr(round(px, hl_perps_px_decimals(px, sz_decimals))))


def _decimal_floor_log10(px: Decimal) -> int:
    _sign, digits, exp = px.normalize().as_tuple()
    return exp + len(digits) - 1


def _grid_step(px: Decimal, sz_decimals: int) -> Decimal:
    places = min(max(0, 6 - sz_decimals), max(0, 4 - _decimal_floor_log10(px)))
    return Decimal(1).scaleb(-places)


def _grid_next(cursor: Decimal, side: str, sz_decimals: int) -> Decimal:
    if side == "long":
        return cursor + _grid_step(cursor, sz_decimals)
    nxt = cursor - _grid_step(cursor - _grid_step(cursor, sz_decimals), sz_decimals)
    if nxt >= cursor:
        nxt = cursor - _grid_step(cursor, sz_decimals)
    return nxt


def hl_grid_step_beyond(limit: Decimal, side: str, sz_decimals: int, k: int) -> Optional[Decimal]:
    cursor = limit
    for _ in range(int(k)):
        cursor = _grid_next(cursor, side, sz_decimals)
        if cursor <= 0:
            return None
    return cursor


def _reaches(price: Decimal, target: Decimal, side: str) -> bool:
    return price >= target if side == "long" else price <= target


def hl_ticks_through(reach: Decimal, limit: Decimal, side: str, sz_decimals: int) -> int:
    if not _reaches(reach, limit, side):
        return 0
    cursor = limit
    ticks = 0
    while ticks < _RESTING_TICK_CAP:
        nxt = _grid_next(cursor, side, sz_decimals)
        if nxt <= 0 or not _reaches(reach, nxt, side):
            break
        cursor = nxt
        ticks += 1
    return ticks


def _rule_price(value) -> Decimal:
    return Decimal(repr(float(value)))


def _rule_int(raw, field: str, minimum=None, maximum=None) -> int:
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise RestingRuleInvalid(field)
    if minimum is not None and raw < minimum:
        raise RestingRuleInvalid(field)
    if maximum is not None and raw > maximum:
        raise RestingRuleInvalid(field)
    return raw


def _rule_float(raw, field: str, *, positive: bool) -> float:
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise RestingRuleInvalid(field)
    value = float(raw)
    if not math.isfinite(value) or value < 0 or (positive and value <= 0):
        raise RestingRuleInvalid(field)
    return value


def resting_rule_input(market: dict):
    if not isinstance(market, dict) or RESTING_TP_RULE_KEY not in market:
        return None
    raw = market[RESTING_TP_RULE_KEY]
    if not isinstance(raw, dict):
        raise RestingRuleInvalid("rule")
    if _rule_int(raw.get("v"), "v") != RESTING_TP_RULE_VERSION:
        raise RestingRuleInvalid("v")
    if _rule_int(raw.get("k_ticks"), "k_ticks", minimum=1) != RESTING_TP_TRADE_THROUGH_TICKS:
        raise RestingRuleInvalid("k_ticks")
    if "sz_decimals" not in raw:
        raise RestingRuleInvalid("sz_decimals")
    sz_decimals = raw["sz_decimals"]
    if sz_decimals is not None:
        sz_decimals = _rule_int(sz_decimals, "sz_decimals", minimum=0, maximum=12)
    prior = raw.get("prior_reach_px")
    if prior is not None:
        prior = _rule_float(prior, "prior_reach_px", positive=True)
    bars_raw = raw.get("bars")
    if not isinstance(bars_raw, list):
        raise RestingRuleInvalid("bars")
    bars = []
    last_open = None
    for bar in bars_raw:
        if not isinstance(bar, dict):
            raise RestingRuleInvalid("bars")
        open_ms = _rule_int(bar.get("open_ms"), "bars.open_ms", minimum=0)
        if last_open is not None and open_ms <= last_open:
            raise RestingRuleInvalid("bars.open_ms")
        last_open = open_ms
        entry_bar = bar.get("entry_bar")
        if not isinstance(entry_bar, bool):
            raise RestingRuleInvalid("bars.entry_bar")
        favorable = _rule_float(bar.get("favorable_px"), "bars.favorable_px", positive=True)
        adverse = _rule_float(bar.get("adverse_px"), "bars.adverse_px", positive=True)
        if entry_bar and (bars or prior is not None or favorable != adverse):
            raise RestingRuleInvalid("bars.entry_bar")
        bars.append({
            "open_ms": open_ms,
            "favorable_px": favorable,
            "adverse_px": adverse,
            "entry_bar": entry_bar,
        })
    stop = _rule_float(raw.get("stop_trigger_px"), "stop_trigger_px", positive=False)
    held = raw.get("held")
    if not isinstance(held, bool):
        raise RestingRuleInvalid("held")
    hold_reason = raw.get("hold_reason", "")
    if not isinstance(hold_reason, str):
        raise RestingRuleInvalid("hold_reason")
    if held and not hold_reason.strip():
        raise RestingRuleInvalid("hold_reason")
    coverage = raw.get("coverage")
    if coverage not in RESTING_TP_COVERAGES:
        raise RestingRuleInvalid("coverage")
    return {
        "k_ticks": RESTING_TP_TRADE_THROUGH_TICKS,
        "sz_decimals": sz_decimals,
        "prior_reach_px": prior,
        "bars": bars,
        "stop_trigger_px": stop,
        "held": held,
        "hold_reason": hold_reason.strip(),
        "coverage": coverage,
    }


def resting_rule_bar(open_ms, high, low, close, side: str, *, entry_bar: bool, walk_mode: bool) -> dict:
    if entry_bar or not walk_mode:
        favorable = adverse = float(close)
    elif side == "long":
        favorable, adverse = float(high), float(low)
    else:
        favorable, adverse = float(low), float(high)
    return {
        "open_ms": int(open_ms),
        "favorable_px": favorable,
        "adverse_px": adverse,
        "entry_bar": bool(entry_bar),
    }


def build_resting_rule(*, sz_decimals, bars, stop_trigger_px, coverage: str = "complete",
                       prior_reach_px=None, held: bool = False, hold_reason: str = "") -> dict:
    return {
        "v": RESTING_TP_RULE_VERSION,
        "k_ticks": RESTING_TP_TRADE_THROUGH_TICKS,
        "sz_decimals": sz_decimals,
        "prior_reach_px": prior_reach_px,
        "bars": list(bars),
        "stop_trigger_px": float(stop_trigger_px or 0.0),
        "held": bool(held),
        "hold_reason": str(hold_reason or ""),
        "coverage": coverage,
    }


def resting_rule_geometry(position: dict, regime_keyed: bool):
    entry_atr = float_from(position, "entry_atr")
    if entry_atr <= 0:
        return None, "entry_atr_missing"
    regime = ""
    regime_label = ""
    if regime_keyed:
        regime = str((position or {}).get("regime", "") or "").strip()
        if not regime:
            return None, "position_regime_missing"
        regime_label = "frozen"
    return TierGeometry(
        anchor=geometry_anchor(position),
        atr=entry_atr,
        atr_label="entry",
        regime=regime,
        regime_label=regime_label,
        resting_limit=resting_limit_model(position),
    ), ""


def _bps_through(reach: Decimal, limit: Decimal, side: str) -> float:
    if limit <= 0:
        return 0.0
    raw = (reach - limit) / limit if side == "long" else (limit - reach) / limit
    return max(float(raw * Decimal(10000)), 0.0)


def resting_rule_scan(rule: dict, side: str) -> dict:
    stop = _rule_price(rule["stop_trigger_px"]) if rule["stop_trigger_px"] > 0 else None
    scanned = []
    stop_bar = 0
    last_scanned = 0
    if rule["prior_reach_px"] is not None:
        scanned.append((None, _rule_price(rule["prior_reach_px"])))
    for bar in rule["bars"]:
        if stop is not None and _reaches(_rule_price(bar["adverse_px"]), stop, "short" if side == "long" else "long"):
            stop_bar = bar["open_ms"]
            break
        scanned.append((bar["open_ms"], _rule_price(bar["favorable_px"])))
        last_scanned = bar["open_ms"]
    best = None
    best_bar = None
    for open_ms, reach in scanned:
        if best is None or _reaches(reach, best, side) and reach != best:
            best, best_bar = reach, open_ms
    return {
        "scanned": scanned,
        "best": best,
        "best_bar": best_bar,
        "stop_bar": stop_bar,
        "last_scanned_open_ms": last_scanned,
    }


def resting_rule_hit_tiers(tiers, side: str, geometry: TierGeometry, rule: dict):
    sz_decimals = rule["sz_decimals"]
    scan = resting_rule_scan(rule, side)
    scanned, best, best_bar, stop_bar = scan["scanned"], scan["best"], scan["best_bar"], scan["stop_bar"]
    evidence = []
    hit = []
    limits = {}
    for multiple, fraction in tiers:
        raw_px = tier_price(side, geometry.anchor, geometry.atr, multiple)
        entry = {"atr_multiple": multiple, "hit": False}
        if not (raw_px > 0) or not math.isfinite(raw_px):
            entry["limit_px"] = None
            evidence.append(entry)
            continue
        limit = hl_round_limit(raw_px, sz_decimals)
        required = hl_grid_step_beyond(limit, side, sz_decimals, rule["k_ticks"])
        entry["limit_px"] = float(limit)
        entry["required_px"] = float(required) if required is not None else None
        cross = None
        if required is not None:
            for open_ms, reach in scanned:
                if _reaches(reach, required, side):
                    cross = open_ms
                    entry["hit"] = True
                    break
        if entry["hit"]:
            entry["cross_bar_open_ms"] = cross if cross is not None else 0
            entry["cross_source"] = "prior_reach" if cross is None else "bar"
        if best is not None:
            entry["ticks_through"] = hl_ticks_through(best, limit, side, sz_decimals)
            entry["bps_through"] = _bps_through(best, limit, side)
        evidence.append(entry)
        if entry["hit"]:
            hit.append((multiple, fraction))
            limits[multiple] = limit
    return {
        "hit_tiers": hit,
        "limits": limits,
        "tiers": evidence,
        "reach_px": float(best) if best is not None else None,
        "reach_bar_open_ms": best_bar if best_bar is not None else 0,
        "stop_reached_bar_open_ms": stop_bar,
        "scanned_bars": sum(1 for open_ms, _ in scanned if open_ms is not None),
    }


def resting_rule_fill_price(position: dict, hit_tiers, limits) -> float:
    if not hit_tiers:
        return 0.0
    current_qty = float_from(position, "current_quantity")
    initial_qty = float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed = max(initial_qty - current_qty, 0.0)
    weighted = Decimal(0)
    filled = Decimal(0)
    previous_target = 0.0
    for multiple, cumulative in hit_tiers:
        low = max(previous_target * initial_qty, already_closed)
        high = min(clamp_fraction(cumulative) * initial_qty, already_closed + current_qty)
        previous_target = clamp_fraction(cumulative)
        if high <= low:
            continue
        qty = Decimal(repr(high - low))
        weighted += qty * limits[multiple]
        filled += qty
    if filled <= 0:
        return float(limits[hit_tiers[-1][0]])
    return float(weighted / filled)


def _rule_noop(reason: str, evidence=None) -> dict:
    out = {"close_fraction": 0.0, "reason": reason}
    if evidence is not None:
        out["resting_fill"] = evidence
    return out


def evaluate_resting_rule(position: dict, market: dict, *, reason_name: str, regime_keyed: bool,
                          ladder, reason_for) -> dict:
    if reason_name in RESTING_TP_UNSUPPORTED_CLOSES:
        return _rule_noop("noop:resting_rule_unsupported_close")
    try:
        rule = resting_rule_input(market)
    except RestingRuleInvalid as exc:
        return _rule_noop(f"noop:resting_rule_invalid:{exc.field}")
    if not resting_limit_model(position):
        return _rule_noop("noop:resting_rule_without_resting_model")
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return _rule_noop("noop:missing_position")
    base = {
        "rule": "trade_through",
        "k_ticks": rule["k_ticks"],
        "sz_decimals": rule["sz_decimals"],
        "coverage": rule["coverage"],
        "stop_trigger_px": rule["stop_trigger_px"],
    }
    if rule["held"]:
        reason = f"noop:resting_rule_held:{rule['hold_reason']}"
        return _rule_noop(reason, {**base, "verdict": "held", "hold_reason": rule["hold_reason"]})
    geometry, hold = resting_rule_geometry(position, regime_keyed)
    if hold:
        return _rule_noop(f"noop:resting_rule_held:{hold}", {**base, "verdict": "held", "hold_reason": hold})
    if rule["sz_decimals"] is None:
        hold = "sz_decimals_missing"
        return _rule_noop(f"noop:resting_rule_held:{hold}", {**base, "verdict": "held", "hold_reason": hold})
    tiers, ladder_reason = ladder(geometry)
    if tiers is None:
        return _rule_noop(ladder_reason)
    scan = resting_rule_hit_tiers(tiers, side, geometry, rule)
    evidence = {
        **base,
        "anchor_px": geometry.anchor,
        "atr": geometry.atr,
        "regime": geometry.regime,
        "reach_px": scan["reach_px"],
        "reach_bar_open_ms": scan["reach_bar_open_ms"],
        "stop_reached_bar_open_ms": scan["stop_reached_bar_open_ms"],
        "scanned_bars": scan["scanned_bars"],
        "tiers": scan["tiers"],
    }
    hit_tiers = scan["hit_tiers"]
    if not hit_tiers:
        return _rule_noop("noop:resting_not_traded_through", {**evidence, "verdict": "not_traded_through"})
    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, clamp_fraction(cumulative_fraction))
    if close_fraction <= 0:
        return _rule_noop("noop:already_taken", {**evidence, "verdict": "already_taken"})
    price = resting_rule_fill_price(position, hit_tiers, scan["limits"])
    if not (price > 0) or not math.isfinite(price):
        return _rule_noop("noop:resting_rule_invalid:fill_price", {**evidence, "verdict": "invalid"})
    evidence["verdict"] = "traded_through"
    evidence["fill_px"] = price
    return {
        "close_fraction": close_fraction,
        "reason": reason_for(geometry, multiple),
        "tier_fill_price": price,
        "resting_fill": evidence,
    }
