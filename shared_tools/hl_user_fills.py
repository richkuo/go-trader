import math
import sys
from collections.abc import Mapping

def _finite_number(value):
    if isinstance(value, bool):
        return None
    if not isinstance(value, (int, float, str)):
        return None
    try:
        numeric = float(value)
    except (TypeError, ValueError):
        return None
    return numeric if math.isfinite(numeric) else None

def apply_user_fills_lookup(fill, lookup):
    if not isinstance(lookup, Mapping):
        return False
    fee = _finite_number(lookup.get('fee'))
    if fee is None:
        return False
    fill['fee'] = fee
    if 'closed_pnl' in lookup:
        closed_pnl = _finite_number(lookup.get('closed_pnl'))
        if closed_pnl is not None:
            fill['closed_pnl'] = closed_pnl
        else:
            print(f"[WARN] userFills lookup: closed_pnl present but malformed ({lookup.get('closed_pnl')!r}), dropping", file=sys.stderr)
    return True
