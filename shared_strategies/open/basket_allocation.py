import math
from fractions import Fraction
from typing import Any, Dict, List, Mapping, Tuple

SIDE_WEIGHT = 0.5
SIDE_TOLERANCE = 1e-9
COORDINATOR_RELATIVE_TOLERANCE = 1e-9
WEIGHT_GRID_BITS = 52
WEIGHT_GRID_SCALE = float(2 ** WEIGHT_GRID_BITS)
WEIGHT_GRID_UNIT = 2.0 ** -WEIGHT_GRID_BITS
LONG_TAIL_TOP = "top"
LONG_TAIL_BOTTOM = "bottom"
LONG_TAILS = (LONG_TAIL_TOP, LONG_TAIL_BOTTOM)
RETURN_RANK_MAX_LOOKBACK = 4999
RETURN_RANK_PRICE_COLUMNS = ("open", "high", "low", "close")


class BasketAllocationHold(Exception):
    pass


def rank_by_score(scores: Mapping[str, float]) -> List[str]:
    return sorted(scores, key=lambda sym: (-scores[sym], sym))


def tail_size(quantile: float, population: int) -> int:
    if population < 2:
        raise BasketAllocationHold(f"the eligible population {population} cannot fill two disjoint tails")
    exact = Fraction(repr(float(quantile))) * population
    size = max(1, math.floor(exact))
    if 2 * size > population:
        raise BasketAllocationHold(f"tails of {size} members each do not fit disjointly in {population} members")
    return size


def select_tails(scores: Mapping[str, float], quantile: float) -> Tuple[List[str], List[str]]:
    order = rank_by_score(scores)
    size = tail_size(quantile, len(order))
    return order[:size], order[-size:]


def inverse_volatility_ratios(vols: Mapping[str, float]) -> Dict[str, float]:
    floor_vol = min(vols.values())
    return {sym: floor_vol / vol for sym, vol in vols.items()}


def cap_fraction_for(ctx: Mapping[str, float]) -> float:
    return float(ctx["max_coin_notional_usd"]) / float(ctx["gross_budget_usd"])


def capped_side_weights(ratios: Mapping[str, float], cap_fraction: float, side: str) -> Dict[str, float]:
    members = sorted(ratios)
    if len(members) * cap_fraction < SIDE_WEIGHT - SIDE_TOLERANCE:
        raise BasketAllocationHold(
            f"the {side} side has {len(members)} members and a per-coin cap of {cap_fraction:.9g} of the gross budget, "
            f"so it cannot reach {SIDE_WEIGHT} of the gross budget")
    weights: Dict[str, float] = {}
    free = list(members)
    budget = SIDE_WEIGHT
    while free:
        total = math.fsum(ratios[sym] for sym in free)
        share = {sym: budget * ratios[sym] / total for sym in free}
        over = [sym for sym in free if share[sym] > cap_fraction]
        if not over:
            weights.update(share)
            break
        for sym in over:
            weights[sym] = cap_fraction
        budget = max(0.0, budget - cap_fraction * len(over))
        free = [sym for sym in free if sym not in over]
    return weights


def _grid_units(weights: Mapping[str, float], cap_units: int, side_units: int) -> Dict[str, int]:
    exact = {sym: weights[sym] * WEIGHT_GRID_SCALE for sym in weights}
    units = {sym: min(cap_units, max(0, math.floor(exact[sym]))) for sym in weights}
    excess = sum(units.values()) - side_units
    for sym in sorted(units, key=lambda s: (-units[s], s)):
        if excess <= 0:
            break
        cut = min(excess, units[sym])
        units[sym] -= cut
        excess -= cut
    order = sorted(units, key=lambda s: (-(exact[s] - math.floor(exact[s])), s))
    remaining = side_units - sum(units.values())
    for sym in order:
        if remaining <= 0:
            break
        if units[sym] < cap_units:
            units[sym] += 1
            remaining -= 1
    for sym in order:
        if remaining <= 0:
            break
        add = min(cap_units - units[sym], remaining)
        if add > 0:
            units[sym] += add
            remaining -= add
    return units


def _trim_units(units: Dict[str, int], target: int) -> None:
    excess = sum(units.values()) - target
    for sym in sorted(units, key=lambda s: (-units[s], s)):
        if excess <= 0:
            return
        cut = min(excess, units[sym])
        units[sym] -= cut
        excess -= cut


def emit_signed_weights(long_weights: Mapping[str, float], short_weights: Mapping[str, float],
                        cap_fraction: float) -> Dict[str, float]:
    cap_units = min(int(WEIGHT_GRID_SCALE), math.floor(cap_fraction * WEIGHT_GRID_SCALE))
    side_units = int(SIDE_WEIGHT * WEIGHT_GRID_SCALE)
    tolerance_units = math.floor(SIDE_TOLERANCE * WEIGHT_GRID_SCALE)
    long_units = _grid_units(long_weights, cap_units, side_units)
    short_units = _grid_units(short_weights, cap_units, side_units)
    common = min(sum(long_units.values()), sum(short_units.values()))
    if side_units - common > tolerance_units:
        raise BasketAllocationHold(
            f"the capped side weights reach {common * WEIGHT_GRID_UNIT:.12g} of the gross budget, "
            f"short of {SIDE_WEIGHT} by more than the tolerance {SIDE_TOLERANCE}")
    _trim_units(long_units, common)
    _trim_units(short_units, common)
    out = {sym: u * WEIGHT_GRID_UNIT for sym, u in long_units.items()}
    out.update({sym: -(u * WEIGHT_GRID_UNIT) for sym, u in short_units.items()})
    return out


def check_coordinator_bounds(weights: Mapping[str, float], ctx: Mapping[str, float]) -> None:
    gross = float(ctx["gross_budget_usd"])
    cap = float(ctx["max_coin_notional_usd"])
    net_bound = float(ctx["max_transition_net_usd"])
    for sym in sorted(weights):
        w = weights[sym]
        if not math.isfinite(w):
            raise BasketAllocationHold(f"the {sym} weight is not finite")
        if abs(w) * gross > cap * (1 + COORDINATOR_RELATIVE_TOLERANCE):
            raise BasketAllocationHold(
                f"the {sym} weight {w:.12g} allocates ${abs(w) * gross:.2f}, above the ${cap:.2f} per-coin cap")
    absolute = math.fsum(abs(w) for w in weights.values())
    if absolute > 1 + COORDINATOR_RELATIVE_TOLERANCE:
        raise BasketAllocationHold(f"absolute target weights sum to {absolute:.12g}, above 1")
    net = math.fsum(weights.values())
    if abs(net) * gross > net_bound * (1 + COORDINATOR_RELATIVE_TOLERANCE):
        raise BasketAllocationHold(
            f"the target net exposure ${abs(net) * gross:.6g} exceeds max_transition_net_usd ${net_bound:.6g}")


def allocate_long_short(scores: Mapping[str, float], vols: Mapping[str, float], quantile: float,
                        ctx: Mapping[str, float], *, long_tail: str = LONG_TAIL_TOP) -> Dict[str, float]:
    if long_tail not in LONG_TAILS:
        raise ValueError(f"long_tail must be one of {LONG_TAILS!r}, got {long_tail!r}")
    first, last = select_tails(scores, quantile)
    longs, shorts = (first, last) if long_tail == LONG_TAIL_TOP else (last, first)
    cap_fraction = cap_fraction_for(ctx)
    long_weights = capped_side_weights(inverse_volatility_ratios({s: vols[s] for s in longs}), cap_fraction, "long")
    short_weights = capped_side_weights(inverse_volatility_ratios({s: vols[s] for s in shorts}), cap_fraction, "short")
    signed = emit_signed_weights(long_weights, short_weights, cap_fraction)
    weights = {sym: 0.0 for sym in scores}
    weights.update(signed)
    check_coordinator_bounds(weights, ctx)
    return weights


def _strict_int(name: str, params: Mapping[str, Any], key: str, low: int, high: int) -> int:
    value = params.get(key)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{name}: {key} must be an integer, got {value!r}")
    if not low <= value <= high:
        raise ValueError(f"{name}: {key} must be in [{low}, {high}], got {value}")
    return value


def validate_return_rank_params(name: str, params: Mapping[str, Any]) -> None:
    _strict_int(name, params, "return_lookback_bars", 1, RETURN_RANK_MAX_LOOKBACK)
    _strict_int(name, params, "volatility_lookback_bars", 2, RETURN_RANK_MAX_LOOKBACK)
    q = params.get("quantile")
    if isinstance(q, bool) or not isinstance(q, (int, float)) or not math.isfinite(float(q)):
        raise ValueError(f"{name}: quantile must be a finite number, got {q!r}")
    if not 0 < float(q) < 0.5:
        raise ValueError(f"{name}: quantile must be above 0 and below 0.5, got {q!r}")


def return_rank_required_closes(params: Mapping[str, Any]) -> int:
    return max(int(params["return_lookback_bars"]), int(params["volatility_lookback_bars"])) + 1


def return_rank_preflight(name: str, params: Mapping[str, Any], allocation_context: Mapping[str, Any]) -> None:
    validate_return_rank_params(name, params)
    need = return_rank_required_closes(params)
    lookback = allocation_context["lookback_bars"]
    if lookback < need:
        raise ValueError(
            f"lookback_bars {lookback} cannot supply return_lookback_bars {params['return_lookback_bars']} and "
            f"volatility_lookback_bars {params['volatility_lookback_bars']}: the ranker needs {need} closed bars")


def trailing_return_and_volatility(name: str, sym: str, frame, return_bars: int, vol_bars: int,
                                   need: int) -> Tuple[float, float]:
    prices = frame[list(RETURN_RANK_PRICE_COLUMNS)].to_numpy(dtype=float)
    if len(prices) < need:
        raise BasketAllocationHold(f"{sym}: the frame carries {len(prices)} closed bars, {name} needs {need}")
    for row in prices:
        for px in row:
            if not math.isfinite(px):
                raise BasketAllocationHold(f"{sym}: the frame carries a non-finite price")
            if px <= 0:
                raise BasketAllocationHold(f"{sym}: the frame carries a non-positive price")
    closes = [float(c) for c in prices[:, 3]]
    score = closes[-1] / closes[-1 - return_bars] - 1.0
    if not math.isfinite(score):
        raise BasketAllocationHold(f"{sym}: the trailing return is not finite")
    logs = [math.log(c) for c in closes[-vol_bars - 1:]]
    returns = [logs[i] - logs[i - 1] for i in range(1, len(logs))]
    mean = math.fsum(returns) / vol_bars
    variance = math.fsum((r - mean) ** 2 for r in returns) / vol_bars
    vol = math.sqrt(variance)
    if not math.isfinite(vol) or vol <= 0:
        raise BasketAllocationHold(f"{sym}: the trailing volatility {vol!r} is zero or not finite")
    return score, vol


def trailing_return_rank_targets(name: str, frames: Mapping[str, Any], allocation_context: Mapping[str, Any],
                                 return_lookback_bars: int, volatility_lookback_bars: int, quantile: float,
                                 long_tail: str) -> Dict[str, Any]:
    params = {"return_lookback_bars": return_lookback_bars, "volatility_lookback_bars": volatility_lookback_bars,
              "quantile": quantile}
    validate_return_rank_params(name, params)
    need = return_rank_required_closes(params)
    try:
        scores: Dict[str, float] = {}
        vols: Dict[str, float] = {}
        for sym in sorted(frames):
            scores[sym], vols[sym] = trailing_return_and_volatility(name, sym, frames[sym], return_lookback_bars,
                                                                    volatility_lookback_bars, need)
        weights = allocate_long_short(scores, vols, quantile, allocation_context, long_tail=long_tail)
    except BasketAllocationHold as e:
        return {"hold": str(e)}
    return {"scores": scores, "weights": weights, "exclusions": {}}
