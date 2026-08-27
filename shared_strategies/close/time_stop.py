from __future__ import annotations

def evaluate(position: dict, market: dict, params: dict) -> dict:
    try:
        max_bars = int(params.get('max_bars', 0) or 0)
    except (TypeError, ValueError):
        max_bars = 0
    if max_bars <= 0:
        return {'close_fraction': 0.0, 'reason': 'noop:disabled'}
    raw = position.get('bars_held')
    if raw is None:
        return {'close_fraction': 0.0, 'reason': 'noop:missing_bars_held'}
    try:
        bars_held = int(raw)
    except (TypeError, ValueError):
        return {'close_fraction': 0.0, 'reason': 'noop:missing_bars_held'}
    if bars_held >= max_bars:
        return {'close_fraction': 1.0, 'reason': f'time_stop:{max_bars}'}
    return {'close_fraction': 0.0, 'reason': 'noop:within_window'}
