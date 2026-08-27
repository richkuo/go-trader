from __future__ import annotations
from _helpers import float_from

def evaluate(position: dict, market: dict, params: dict) -> dict:
    try:
        atr_mult = float(params.get('atr_mult', 0.0) or 0.0)
    except (TypeError, ValueError):
        atr_mult = 0.0
    if atr_mult <= 0:
        return {'close_fraction': 0.0, 'reason': 'noop:disabled'}
    avg_cost = float_from(position, 'avg_cost')
    current_quantity = float_from(position, 'current_quantity')
    side = str(position.get('side', '') or '').strip().lower()
    mark_price = float_from(market, 'mark_price')
    if mark_price <= 0:
        return {'close_fraction': 0.0, 'reason': 'noop:missing_mark_price'}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ('long', 'short'):
        return {'close_fraction': 0.0, 'reason': 'noop:missing_position'}
    atr_source = str(params.get('atr_source', 'entry') or 'entry').strip().lower()
    if atr_source == 'live':
        atr_ref = float_from(market, 'atr')
        missing_reason = 'noop:missing_live_atr'
    else:
        atr_ref = float_from(position, 'entry_atr')
        missing_reason = 'noop:missing_entry_atr'
    if atr_ref <= 0:
        return {'close_fraction': 0.0, 'reason': missing_reason}
    distance = atr_mult * atr_ref
    if side == 'long':
        hit = mark_price <= avg_cost - distance
    else:
        hit = mark_price >= avg_cost + distance
    if hit:
        return {'close_fraction': 1.0, 'reason': f'atr_stop:{atr_mult:g}'}
    return {'close_fraction': 0.0, 'reason': 'noop:not_hit'}
