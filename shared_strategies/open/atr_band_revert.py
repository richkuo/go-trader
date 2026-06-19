"""
ATR Band Reversion — entry side.

Ranging-market mean reversion on ATR-scaled bands around a simple moving
average. With ``mid = SMA(close, period)`` and ``ATR`` the average true range:

    band_lower = mid - k_entry * ATR
    band_upper = mid + k_entry * ATR

Entries fade the stretch back toward the mean:
  * long  when ``close <= band_lower`` (price sank a chunk below average),
  * short when ``close >= band_upper`` (price stretched a chunk above) —
    only when ``allow_short`` (spot can't short, so it defaults off; the
    futures variant turns it on).

This module emits ENTRIES ONLY, exactly like ``consolidation_range``. The exit
is owned by the close+stop machinery and is configuration, not code — at entry
``mid ≈ entry ± k_entry*ATR``, so the take-profit targets map onto the existing
ATR close evaluators (no new close code):

  * Take profit AT MID  → ``tiered_tp_atr`` tier at ``atr_multiple ≈ k_entry``.
  * Take profit AT THE OPPOSITE BAND → leave the close strategy nil
    (open-as-close): when price reaches the far band the entry signal flips and
    closes the position; equivalently a ``tiered_tp_atr`` tier at ``2*k_entry``.
  * SPLIT (half at mid, runner to the opposite band) → a two-tier
    ``tiered_tp_atr`` (e.g. 50% at ``k_entry``, remainder at ``2*k_entry``).
  * The range-break STOP is the framework ATR stop
    (``stop_loss_atr_mult ≈ k_entry + k_stop``) — if price keeps going past the
    band the range broke and the position is cut.

RANGING GATE: restrict entries to ranging conditions with config
``allowed_regimes`` (e.g. ``["ranging"]`` for the adx classifier, or
``["ranging_quiet","ranging_volatile"]`` for the composite classifier — exclude
``ranging_directional``, the danger zone where a range is about to break). The
strategy core stays regime-agnostic; the Go regime gate enforces it.

STATUS: a tunable mean-reversion baseline. Mean reversion's failure mode is a
range that breaks into a trend (worst fill right before the stop) — keep the
stop tight and the regime gate honest before any live use.

Defaults: period=20 (mid/SMA), atr_period=14, k_entry=1.5.
"""

import numpy as np
import pandas as pd


def atr_band_revert_core(
    df: pd.DataFrame,
    period: int = 20,
    atr_period: int = 14,
    k_entry: float = 1.5,
    allow_short: bool = False,
) -> pd.DataFrame:
    result = df.copy()

    mid = result["close"].rolling(window=period).mean()

    # True range -> ATR (rolling mean), matching the repo's ATR convention:
    # keep full precision below 100, round to int at/above 100 (atr.py).
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    _atr = tr.rolling(window=atr_period).mean()
    atr = _atr.where(_atr < 100, _atr.round(0))

    result["atr"] = atr
    result["band_mid"] = mid
    result["band_lower"] = mid - k_entry * atr
    result["band_upper"] = mid + k_entry * atr

    result["signal"] = 0
    long_entry = result["close"] <= result["band_lower"]
    result.loc[long_entry.fillna(False), "signal"] = 1
    if allow_short:
        short_entry = result["close"] >= result["band_upper"]
        result.loc[short_entry.fillna(False), "signal"] = -1
    return result
