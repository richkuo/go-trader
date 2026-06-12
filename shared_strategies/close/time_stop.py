"""Time-stop close evaluator (#997 M3 — late-giveback bleed mode).

Default-off holding-time cap: closes the whole position once it has been held
for ``max_bars`` evaluation bars. Targets strategies whose winners give back
their open profit late in the hold (the diagnostic's ``late_giveback`` mode).

``max_bars == 0`` (the registry default) is an explicit no-op, so merely
registering the evaluator — or referencing it with no params — changes nothing
(M1 step 4: every mechanism is an independent default-off knob).

``bars_held`` is supplied by the backtester per evaluation bar (a position
filled at bar N's open carries ``bars_held == 1`` at bar N's close). It is NOT
supplied by the live check scripts yet — live wiring is deferred (see #997).
Until wired, a live config referencing ``time_stop`` fails safe with a
``noop:missing_bars_held`` close_fraction of 0 rather than acting on stale data.
"""

from __future__ import annotations


def evaluate(position: dict, market: dict, params: dict) -> dict:
    try:
        max_bars = int(params.get("max_bars", 0) or 0)
    except (TypeError, ValueError):
        max_bars = 0
    if max_bars <= 0:
        return {"close_fraction": 0.0, "reason": "noop:disabled"}

    raw = position.get("bars_held")
    if raw is None:
        return {"close_fraction": 0.0, "reason": "noop:missing_bars_held"}
    try:
        bars_held = int(raw)
    except (TypeError, ValueError):
        return {"close_fraction": 0.0, "reason": "noop:missing_bars_held"}

    if bars_held >= max_bars:
        return {"close_fraction": 1.0, "reason": f"time_stop:{max_bars}"}
    return {"close_fraction": 0.0, "reason": "noop:within_window"}
