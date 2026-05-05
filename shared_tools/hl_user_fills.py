"""
Shared helpers for applying Hyperliquid userFills lookup results to fill dicts.

Used by shared_scripts/check_hyperliquid.py and shared_scripts/close_hyperliquid_position.py
to keep the shape-validation and numeric-guard logic in one place (#598).
"""

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
    """Apply fee and closed_pnl from a userFills lookup result to *fill* in-place.

    Returns True when fee was successfully applied, False when lookup is not a
    Mapping or fee is malformed (caller should warn). A present-but-malformed
    closed_pnl emits a [WARN] to stderr and is silently dropped so the fee
    (the primary goal of #585/#587) is still recorded.
    """
    if not isinstance(lookup, Mapping):
        return False
    fee = _finite_number(lookup.get("fee"))
    if fee is None:
        return False
    fill["fee"] = fee
    if "closed_pnl" in lookup:
        closed_pnl = _finite_number(lookup.get("closed_pnl"))
        if closed_pnl is not None:
            fill["closed_pnl"] = closed_pnl
        else:
            print(
                f"[WARN] userFills lookup: closed_pnl present but malformed"
                f" ({lookup.get('closed_pnl')!r}), dropping",
                file=sys.stderr,
            )
    return True
