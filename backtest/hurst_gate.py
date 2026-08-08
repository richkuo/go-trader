"""#1411 Hurst entry gate — backtest-side parity implementation.

This module mirrors ``scheduler/hurst_gate.go`` bar-for-bar so a config that
gates or scales live does the same thing in backtest. Everything here is a pure
function of the config plus a rolling Hurst series; the Backtester owns the
per-bar loop.

PARITY CONTRACT (each item is asserted by tests)

  1. Estimator. H comes from ``hurst_exponent`` in
     ``shared_strategies/open/indicators_core.py`` — the #1409 single source of
     truth. It is never reimplemented here.

  2. Frame length. Live computes H over the FULL fetched regime frame, whose
     depth is ``max(200, 2*max_period - 1 + 10)`` (``regimeOhlcvBaseLimit`` /
     ``regimeOhlcvMargin`` in scheduler/regime_multi_window.go). The rolling
     window here uses the identical formula so both sides see the same number
     of closes.

  3. Look-ahead. The rolling value at bar i uses closes ``[i-W+1, i]``. The
     DECISION series is that series shifted one bar, so a signal evaluated at
     bar N reads H computed through bar N-1 — the same one-bar lag the live
     label gate carries (the live regime column is shifted identically), and
     the fill still lands at N+1's open.

  4. NaN policy. A NaN reading is UNKNOWN, never 0.5. It neither arms nor
     disarms; the state simply holds. ``on_failure`` decides whether a fresh
     open is admitted while H is unknown, and that arm is FLAT-ONLY.

  5. Formula. mode=size uses the ISSUE's ``clamp(|H-0.5|/0.15, floor, 1.0)``.
     The #1410 calibration study swept a different form
     (``clamp(1 + gain*e, 0, 1.5)``, which can exceed 1.0); that study shipped
     no recommendation, so the issue's formula governs on both sides.

  6. Range. The CONFIG bounds are validated to (0, 1) exclusive, but the
     RUNTIME metric is not bounded above — DFA reads ~2.0 on a near-smooth
     series. Both comparators stay correct for any finite H.
"""

from __future__ import annotations

import math
import os
import sys
from typing import Optional

import numpy as np
import pandas as pd

_REPO_ROOT = os.path.join(os.path.dirname(__file__), "..")
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

# Mirrors scheduler/hurst_gate.go.
HURST_GATE_MODE_GATE = "gate"
HURST_GATE_MODE_SIZE = "size"
HURST_ON_FAILURE_OPEN = "open"
HURST_ON_FAILURE_CLOSED = "closed"

HURST_STATE_UNKNOWN = ""
HURST_STATE_ARMED = "armed"
HURST_STATE_DISARMED = "disarmed"

# scheduler/hurst_gate.go: hurstSizeSpan / hurstDefaultSizeFloor.
HURST_SIZE_SPAN = 0.15
HURST_DEFAULT_SIZE_FLOOR = 0.25

# scheduler/regime_multi_window.go: regimeOhlcvBaseLimit / regimeOhlcvMargin.
REGIME_OHLCV_BASE_LIMIT = 200
REGIME_OHLCV_MARGIN = 10

_hurst_exponent_fn = None


def _hurst_exponent(close: pd.Series) -> float:
    """#1409 SSoT estimator, imported lazily so the module stays cheap."""
    global _hurst_exponent_fn
    if _hurst_exponent_fn is None:
        from shared_strategies.open.indicators_core import hurst_exponent

        _hurst_exponent_fn = hurst_exponent
    return _hurst_exponent_fn(close)


def hurst_live_frame_bars(windows_spec: Optional[dict], regime_period: int = 14) -> int:
    """Reproduce the live regime OHLCV fetch depth (see PARITY CONTRACT item 2)."""
    max_period = int(regime_period or 14)
    for spec in (windows_spec or {}).values():
        try:
            period = int((spec or {}).get("period") or 0)
        except (AttributeError, TypeError, ValueError):
            period = 0
        max_period = max(max_period, period)
    return max(REGIME_OHLCV_BASE_LIMIT, 2 * max_period - 1 + REGIME_OHLCV_MARGIN)


def normalize_hurst_mode(value) -> str:
    """Validate a hurst_gate.mode value. Empty defaults to "gate"."""
    raw = str(value or "").strip().lower()
    if raw == "":
        return HURST_GATE_MODE_GATE
    if raw not in (HURST_GATE_MODE_GATE, HURST_GATE_MODE_SIZE):
        raise ValueError(
            f'hurst_gate.mode must be "{HURST_GATE_MODE_GATE}" or '
            f'"{HURST_GATE_MODE_SIZE}", got {value!r}'
        )
    return raw


def normalize_hurst_on_failure(value) -> str:
    """Validate a hurst on_failure value. Empty defaults to "open"."""
    raw = str(value or "").strip().lower()
    if raw == "":
        return HURST_ON_FAILURE_OPEN
    if raw not in (HURST_ON_FAILURE_OPEN, HURST_ON_FAILURE_CLOSED):
        raise ValueError(
            f'hurst_gate_on_failure must be "{HURST_ON_FAILURE_OPEN}" or '
            f'"{HURST_ON_FAILURE_CLOSED}", got {value!r}'
        )
    return raw


def rolling_hurst(close: pd.Series, window: int) -> pd.Series:
    """Rolling H over a trailing ``window`` of closes.

    Bars with fewer than ``window`` prior observations are NaN — genuinely
    unknown, exactly as a live process is at start-up before its fetch depth is
    covered. Never back-filled and never defaulted to 0.5.

    Rounded to 4 decimals to match ``shared_tools/regime.py``, so a bound
    comparison lands on the same side on both engines.
    """
    if window < 2:
        raise ValueError(f"hurst window must be >= 2, got {window}")
    values = np.full(len(close), np.nan, dtype=float)
    prices = close.astype(float)
    for i in range(window - 1, len(prices)):
        h = _hurst_exponent(prices.iloc[i - window + 1 : i + 1])
        if h == h and math.isfinite(h):
            values[i] = round(float(h), 4)
    return pd.Series(values, index=close.index)


class HurstGate:
    """Per-bar Hurst gate state machine, mirroring the Go implementation."""

    def __init__(self, cfg: dict):
        cfg = dict(cfg or {})
        self.enabled = bool(cfg.get("enabled"))
        self.mode = normalize_hurst_mode(cfg.get("mode"))
        self.on_failure = normalize_hurst_on_failure(cfg.get("on_failure"))
        self.min = _opt_float(cfg.get("min"))
        self.max = _opt_float(cfg.get("max"))
        self.disarm_min = _opt_float(cfg.get("disarm_min"))
        self.disarm_max = _opt_float(cfg.get("disarm_max"))
        floor = _opt_float(cfg.get("size_floor"))
        self.size_floor = (
            floor if (floor is not None and 0 < floor <= 1.0) else HURST_DEFAULT_SIZE_FLOOR
        )
        # Initial state is UNKNOWN, matching a live process with no persisted
        # latch. It resolves on the first valid reading.
        self.state = HURST_STATE_UNKNOWN

    # -- state machine ----------------------------------------------------

    def _in_arm_band(self, h: float) -> bool:
        if self.min is not None and h < self.min:
            return False
        if self.max is not None and h > self.max:
            return False
        return True

    def _crossed_disarm(self, h: float) -> bool:
        lo = self.disarm_min if self.disarm_min is not None else self.min
        hi = self.disarm_max if self.disarm_max is not None else self.max
        if lo is not None and h < lo:
            return True
        if hi is not None and h > hi:
            return True
        return False

    def advance(self, h) -> str:
        """Apply one observation. NaN/None holds the state (PARITY item 4)."""
        if h is None or not _is_finite(h):
            return self.state
        h = float(h)
        if self.state == HURST_STATE_ARMED:
            if self._crossed_disarm(h):
                self.state = HURST_STATE_DISARMED
        elif self.state == HURST_STATE_DISARMED:
            if self._in_arm_band(h):
                self.state = HURST_STATE_ARMED
        else:
            self.state = (
                HURST_STATE_ARMED if self._in_arm_band(h) else HURST_STATE_DISARMED
            )
        return self.state

    # -- decision ---------------------------------------------------------

    def size_multiplier(self, h) -> float:
        """clamp(|H-0.5|/0.15, size_floor, 1.0); 1.0 for an unknown reading."""
        if h is None or not _is_finite(h):
            return 1.0
        m = abs(float(h) - 0.5) / HURST_SIZE_SPAN
        return min(1.0, max(self.size_floor, m))

    def step(self, h, flat: bool) -> tuple[bool, float]:
        """Advance one bar and return ``(blocks_entry, size_multiplier)``.

        ``flat`` scopes the fail-closed arm: an unknown reading under
        ``on_failure="closed"`` holds only a FRESH open, never management of an
        open position (the #1278 ``regimeBlocksOpen`` shape).
        """
        if not self.enabled:
            return False, 1.0
        known = h is not None and _is_finite(h)
        self.advance(h)
        fail_closed = self.on_failure == HURST_ON_FAILURE_CLOSED

        if self.mode == HURST_GATE_MODE_SIZE:
            if known:
                return False, self.size_multiplier(h)
            return (fail_closed and flat), 1.0

        if self.state == HURST_STATE_DISARMED:
            return True, 1.0
        if self.state == HURST_STATE_UNKNOWN and fail_closed and flat:
            return True, 1.0
        return False, 1.0


def _opt_float(v) -> Optional[float]:
    if v is None:
        return None
    return float(v)


def _is_finite(v) -> bool:
    try:
        f = float(v)
    except (TypeError, ValueError):
        return False
    return f == f and math.isfinite(f)


def validate_hurst_gate_config(cfg: dict, prefix: str = "hurst_gate") -> None:
    """Reject an unusable block, mirroring Go ``validateHurstGateBounds``.

    Runs even when ``enabled`` is false so a parked-but-broken block fails at
    edit time rather than the first time it is switched on.
    """
    cfg = dict(cfg or {})
    mode = normalize_hurst_mode(cfg.get("mode"))
    normalize_hurst_on_failure(cfg.get("on_failure"))

    bounds = {k: _opt_float(cfg.get(k)) for k in ("min", "max", "disarm_min", "disarm_max")}
    for name, value in bounds.items():
        if value is not None and not (0.0 < value < 1.0):
            raise ValueError(f"{prefix}.{name} must be in (0, 1) exclusive, got {value}")

    floor = _opt_float(cfg.get("size_floor"))
    if floor is not None:
        if mode != HURST_GATE_MODE_SIZE:
            raise ValueError(
                f'{prefix}.size_floor only applies with mode="{HURST_GATE_MODE_SIZE}", '
                f"got mode={mode!r}"
            )
        if not (0.0 < floor <= 1.0):
            raise ValueError(f"{prefix}.size_floor must be in (0, 1], got {floor}")

    if mode == HURST_GATE_MODE_SIZE:
        for name in ("min", "max", "disarm_min", "disarm_max"):
            if bounds[name] is not None:
                raise ValueError(
                    f'{prefix}.{name} has no meaning with mode="{HURST_GATE_MODE_SIZE}" '
                    f"(the band gates entries; size scales them) — remove it or "
                    f'switch to mode="{HURST_GATE_MODE_GATE}"'
                )
        return

    # Dependency rules first: they name a more specific cause than the generic
    # "no band configured" message below, which would otherwise mask them.
    if bounds["disarm_min"] is not None:
        if bounds["min"] is None:
            raise ValueError(
                f"{prefix}.disarm_min requires {prefix}.min — it is the hysteresis "
                f"exit for the min bound"
            )
        if bounds["disarm_min"] > bounds["min"]:
            raise ValueError(
                f"{prefix}.disarm_min ({bounds['disarm_min']}) must be <= {prefix}.min "
                f"({bounds['min']}) — a disarm bound tighter than the arm bound "
                f"inverts hysteresis into a flapping gate"
            )
    if bounds["disarm_max"] is not None:
        if bounds["max"] is None:
            raise ValueError(
                f"{prefix}.disarm_max requires {prefix}.max — it is the hysteresis "
                f"exit for the max bound"
            )
        if bounds["disarm_max"] < bounds["max"]:
            raise ValueError(
                f"{prefix}.disarm_max ({bounds['disarm_max']}) must be >= {prefix}.max "
                f"({bounds['max']}) — a disarm bound tighter than the arm bound "
                f"inverts hysteresis into a flapping gate"
            )
    if bounds["min"] is None and bounds["max"] is None:
        raise ValueError(
            f'{prefix} with mode="{HURST_GATE_MODE_GATE}" requires at least one of '
            f"min/max — without a band the gate can never disarm"
        )
    if bounds["min"] is not None and bounds["max"] is not None and bounds["min"] >= bounds["max"]:
        raise ValueError(
            f"{prefix}.min ({bounds['min']}) must be < {prefix}.max ({bounds['max']})"
        )
