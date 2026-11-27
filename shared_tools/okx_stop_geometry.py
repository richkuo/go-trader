from decimal import Decimal, ROUND_CEILING, ROUND_FLOOR, InvalidOperation

LIQ_BUFFER_NUM = Decimal("1005")
LIQ_BUFFER_DEN = Decimal("1000")
SHORT_LIQ_BUFFER_NUM = Decimal("995")


def _decimal(value, label):
    if isinstance(value, bool) or value is None:
        raise ValueError(f"{label} is missing")
    try:
        number = Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise ValueError(f"{label} is not a number") from exc
    if not number.is_finite():
        raise ValueError(f"{label} is not finite")
    return number


def _positive(value, label):
    number = _decimal(value, label)
    if number <= 0:
        raise ValueError(f"{label} must be positive")
    return number


def _tick_decimals(tick_text):
    text = str(tick_text)
    if "e" in text.lower() or text.startswith("+"):
        raise ValueError("tick must be a plain decimal string")
    if "." not in text:
        return 0
    return len(text.split(".", 1)[1])


def _format_tick(value, tick_text):
    places = _tick_decimals(tick_text)
    quant = Decimal("1") if places == 0 else Decimal("1").scaleb(-places)
    return format(value.quantize(quant), "f")


def round_toward_protection(side, trigger, tick_text):
    px = _positive(trigger, "trigger")
    tick = _positive(tick_text, "tick")
    steps = px / tick
    rounding = ROUND_CEILING if side == "long" else ROUND_FLOOR
    if side not in ("long", "short"):
        raise ValueError("side must be long or short")
    rounded = steps.to_integral_value(rounding=rounding) * tick
    if rounded <= 0:
        raise ValueError("rounded trigger is not positive")
    return rounded


def cross_liquidation_price(side, equity, contracts, ct_val, entry, mmr, fee):
    margin = _positive(equity, "equity")
    count = _positive(contracts, "contracts")
    face = _positive(ct_val, "ctVal")
    avg = _positive(entry, "entry")
    maintenance = _positive(mmr, "mmr")
    fee_rate = _decimal(fee, "fee")
    if maintenance >= 1 or fee_rate < 0 or fee_rate >= 1:
        raise ValueError("mmr and fee are not a usable OKX margin ratio")
    if side == "long":
        numerator = margin - face * count * avg
        denominator = face * count * (maintenance + fee_rate - 1)
    elif side == "short":
        numerator = margin + face * count * avg
        denominator = face * count * (maintenance + fee_rate + 1)
    else:
        raise ValueError("side must be long or short")
    if denominator == 0:
        raise ValueError("liquidation denominator is zero")
    return numerator / denominator


def _tighter_liquidation(side, formula, venue):
    bounds = []
    if formula is not None and formula > 0:
        bounds.append(formula)
    if venue is not None and venue > 0:
        bounds.append(venue)
    if not bounds:
        return None
    if side == "long":
        return max(bounds)
    return min(bounds)


def _clamp_inside(side, trigger, liquidation):
    if liquidation is None or liquidation <= 0:
        return trigger, False
    if side == "long" and trigger <= liquidation:
        return liquidation * LIQ_BUFFER_NUM / LIQ_BUFFER_DEN, True
    if side == "short" and trigger >= liquidation:
        return liquidation * SHORT_LIQ_BUFFER_NUM / LIQ_BUFFER_DEN, True
    return trigger, False


def _on_mark_side(side, trigger, mark):
    if side == "long":
        return trigger < mark
    return trigger > mark


def _min_move_reached(new_trigger, previous, min_move_pct):
    if previous is None or previous <= 0:
        return True
    if min_move_pct < 0:
        raise ValueError("min move must be >= 0")
    move = abs(new_trigger - previous) / previous * Decimal("100")
    return move >= min_move_pct


def _raw_trigger(spec):
    side = spec["side"]
    owner = spec["owner"]
    entry = _positive(spec["entry"], "entry")
    if owner == "stop_loss_pct":
        pct = _positive(spec["pct"], "pct")
        if pct > 50:
            raise ValueError("pct must be <= 50")
        return entry * (1 - pct / Decimal("100")) if side == "long" else entry * (1 + pct / Decimal("100"))
    if owner == "stop_loss_atr_mult":
        mult = _positive(spec["atr_mult"], "atr_mult")
        atr = _positive(spec["entry_atr"], "entry_atr")
        return entry - mult * atr if side == "long" else entry + mult * atr
    if owner == "trailing_stop_pct":
        pct = _positive(spec["pct"], "pct")
        if pct > 50:
            raise ValueError("pct must be <= 50")
        mark = _positive(spec["mark"], "mark")
        high = _decimal(spec.get("high_water") or spec["mark"], "high_water")
        if high <= 0:
            high = mark
        if side == "long":
            high = max(high, mark)
        elif side == "short":
            high = min(high, mark) if high > 0 else mark
        else:
            raise ValueError("side must be long or short")
        raw = high * (1 - pct / Decimal("100")) if side == "long" else high * (1 + pct / Decimal("100"))
        return raw, high
    raise ValueError("owner is not an OKX stop")


def finalize_stop(spec):
    side = spec.get("side")
    if side not in ("long", "short"):
        raise ValueError("side must be long or short")
    mark = _positive(spec["mark"], "mark")
    tick = str(spec.get("tick") or "")
    _positive(tick, "tick")
    owner = spec.get("owner")
    high = None
    if owner == "trailing_stop_pct":
        raw, high = _raw_trigger(spec)
    else:
        raw = _raw_trigger(spec)
    if raw <= 0:
        raise ValueError("raw trigger is not positive")
    rounded = round_toward_protection(side, raw, tick)
    previous_text = spec.get("previous_trigger") or ""
    previous = _positive(previous_text, "previous_trigger") if previous_text else None
    min_move = _decimal(spec.get("min_move_pct") or "0", "min_move_pct")
    formula = None
    if spec.get("equity") and spec.get("contracts") and spec.get("ct_val") and spec.get("mmr") and spec.get("fee") is not None and spec.get("entry"):
        formula = cross_liquidation_price(side, spec["equity"], spec["contracts"], spec["ct_val"], spec["entry"], spec["mmr"], spec["fee"])
    venue = None
    if spec.get("require_venue_liq") and not spec.get("venue_liq_known"):
        raise ValueError("position liquidation evidence is missing")
    if spec.get("venue_liq"):
        venue = _positive(spec["venue_liq"], "venue_liq")
    if spec.get("require_venue_liq") and venue is None and formula is not None and formula > 0:
        raise ValueError("venue liquidation price is missing while the cross bound is positive")
    bound = _tighter_liquidation(side, formula if formula is not None and formula > 0 else None, venue)
    clamped_px, did_clamp = _clamp_inside(side, rounded, bound)
    if did_clamp:
        clamped_px = round_toward_protection(side, clamped_px, tick)
        bound_again = _tighter_liquidation(side, formula if formula is not None and formula > 0 else None, venue)
        if bound_again is not None:
            if side == "long" and clamped_px <= bound_again:
                raise ValueError("stop cannot sit inside the liquidation bound")
            if side == "short" and clamped_px >= bound_again:
                raise ValueError("stop cannot sit inside the liquidation bound")
    moved = True
    if previous is not None:
        moved = (side == "long" and clamped_px > previous) or (side == "short" and clamped_px < previous)
        if owner == "trailing_stop_pct" and moved and not _min_move_reached(clamped_px, previous, min_move):
            _, moved = _clamp_inside(side, previous, bound)
        if not moved:
            clamped_px = previous
    if not _on_mark_side(side, clamped_px, mark):
        raise ValueError("stop is not on the executable side of the mark")
    liq_text = ""
    if bound is not None and bound > 0:
        liq_text = format(bound, "f")
    return {
        "trigger": _format_tick(clamped_px, tick),
        "high_water": _format_tick(high, tick) if high is not None else "",
        "moved": moved,
        "liq_bound": liq_text,
        "clamped": did_clamp,
    }


def stop_fill_price(side, mark, trigger):
    mark_px = _positive(mark, "mark")
    trigger_px = _positive(trigger, "trigger")
    if side == "long":
        return mark_px if mark_px <= trigger_px else trigger_px
    if side == "short":
        return mark_px if mark_px >= trigger_px else trigger_px
    raise ValueError("side must be long or short")


def contracts_from_coin(coin_qty, ct_val, lot):
    qty = _positive(coin_qty, "coin_qty")
    face = _positive(ct_val, "ctVal")
    step = _positive(lot, "lot")
    contracts = qty / face
    steps = contracts / step
    if steps != steps.to_integral_value():
        raise ValueError("size is not an exact contract lot")
    if contracts < step:
        raise ValueError("size is below the minimum lot")
    return format(contracts, "f")
