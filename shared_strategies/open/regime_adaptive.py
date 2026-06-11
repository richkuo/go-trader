"""
Regime-Adaptive Entry — breakout vs mean-reversion switch from composite
regime metrics (#958).

Instead of gating one static entry logic via ``allowed_regimes``, this
strategy classifies every bar itself and *switches* entry logic:

    clean trend   → breakout/momentum entry in the trend direction
    ranging       → mean-reversion fade of band extremes back to the mean
    choppy trend / directional chop → flat, no entries

Per-bar classification reuses the composite regime metric definitions from
``shared_tools/regime.py`` (``_composite_efficiency_metrics`` +
``map_composite_label``), computed here as rolling (trailing-window)
equivalents so the strategy stays a self-contained registry entry reading
only its own DataFrame — no regime subprocess, no injected payload:

    return_eff = (close_t - close_{t-period+1}) / (ATR_t * period)   signed, ~[-1, 1]
    range_eff  = (max high - min low over window)  / (ATR_t * period)
    efficiency = |net move| / Σ|bar-to-bar close move|               Kaufman ER ∈ [0, 1]

plus Wilder ADX from the same ``_compute_adx_components`` the live composite
classifier uses (ADX period capped at 14, mirroring
``regime.COMPOSITE_ADX_PERIOD_CAP``). The label mapping mirrors
``regime.map_composite_label``:

    big_move = |return_eff| >= return_eff_threshold
    clean    = efficiency >= efficiency_threshold AND adx >= adx_threshold
    big_move & clean        → trending_{up,down}_clean   (breakout mode)
    big_move & !clean       → trending_{up,down}_choppy  (mean-reversion mode)
    !big_move & high adx    → ranging_directional        (no entries)
    !big_move & wide range  → ranging_volatile           (mean-reversion mode)
    otherwise               → ranging_quiet              (mean-reversion mode)

The choppy-trend labels take mean-reversion mode because a swing inside a
broader range *is* a big low-efficiency net move: the z-recovery bar that
confirms a fade almost definitionally carries a large trailing return, so
restricting fades to the ``ranging_*`` labels would make them unreachable.
Only ``ranging_directional`` (high-ADX grind without a decisive move) stays
entry-free — fading a directional grind is the falling-knife case the #956
audit showed unfiltered mean reversion dying on.

Entries
-------
Breakout (clean trend): close breaks the prior ``breakout_lookback``-bar
extreme in the trend direction. Mean reversion (ranging): the z-score of
close vs its rolling mean crosses back inside the ±``mr_entry_z`` band
(recovery confirmation — same trigger family as ``mean_reversion_pro``, which
the #956 audit ranked top-tier, vs the dead-last unfiltered band-touch fade).

Exits — "regime flips against the position logic"
-------------------------------------------------
Trend positions exit when the label leaves the held side's trend family
(the choppy variant of the same direction is hold-only hysteresis).
Mean-reversion positions exit at the mean (z reaches ``mr_exit_z``) or when a
*clean* trend ignites against the fade (the choppy label opposing the fade is
expected at entry — the dip being faded — and must not insta-exit it).

Slow-trend veto (#967 follow-up): a counter-trend fade is blocked when the
*slow drift* opposes it — ``slow_eff``, the ATR-normalized net move over
``slow_trend_lookback`` bars (same construction as ``return_eff`` on a longer
window), at or beyond ``slow_veto_threshold`` against the fade side. The gate
is drift-based, not price-vs-mean: a fade buys below its rolling mean by
construction, so a price-level gate would make fades unreachable. In a flat
range slow_eff ~ 0 and the veto never fires; in a persistent bear it blocks
the long fades that bled in the #967 OOS window. ``slow_trend_lookback=0``
disables the veto.

Anti-look-ahead: every input is a trailing rolling window / shift(1) /
diff — no centered windows, no future rows. The signal at bar N uses bars
<= N only and fills at bar N+1 open per the engine contract.

The base registration is long-only (shorts only ever flatten); the futures
variant sets ``allow_short=True`` for bidirectional perps.
"""

import numpy as np
import pandas as pd

from adx_trend import _compute_adx_components

# Mirrors regime.COMPOSITE_ADX_PERIOD_CAP — ADX persistence uses a capped
# lookback while return/range/ATR normalization use the full window period.
_COMPOSITE_ADX_PERIOD_CAP = 14

# Int-coded labels for the state loop; names mirror the 7-label composite
# vocabulary in shared_tools/regime.py.
_WARMUP = 0
_TREND_UP_CLEAN = 1
_TREND_UP_CHOPPY = 2
_TREND_DOWN_CLEAN = -1
_TREND_DOWN_CHOPPY = -2
_RANGING_DIRECTIONAL = 3
_RANGING_VOLATILE = 4
_RANGING_QUIET = 5

_LABEL_NAMES = {
    _WARMUP: "",
    _TREND_UP_CLEAN: "trending_up_clean",
    _TREND_UP_CHOPPY: "trending_up_choppy",
    _TREND_DOWN_CLEAN: "trending_down_clean",
    _TREND_DOWN_CHOPPY: "trending_down_choppy",
    _RANGING_DIRECTIONAL: "ranging_directional",
    _RANGING_VOLATILE: "ranging_volatile",
    _RANGING_QUIET: "ranging_quiet",
}


def regime_adaptive_core(
    df: pd.DataFrame,
    period: int = 20,
    adx_threshold: float = 20.0,
    return_eff_threshold: float = 0.05,
    range_eff_threshold: float = 0.03,
    efficiency_threshold: float = 0.5,
    breakout_lookback: int = 10,
    mr_lookback: int = 20,
    mr_entry_z: float = 2.0,
    mr_exit_z: float = 0.0,
    slow_trend_lookback: int = 100,
    slow_veto_threshold: float = 0.05,
    allow_short: bool = False,
) -> pd.DataFrame:
    """Generate regime-switched breakout / mean-reversion signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    period : composite metric window (return/range/efficiency + ATR normalization)
    adx_threshold : minimum ADX for the clean-trend / ranging_directional split
        (composite classifier default 25; tuned to 20 by walk-forward stability)
    return_eff_threshold : minimum |return_eff| for a decisive net move
        (mirrors composite ``return_pct``, default 0.05)
    range_eff_threshold : minimum range_eff splitting ranging_volatile from
        ranging_quiet (mirrors composite ``range_pct``, default 0.03)
    efficiency_threshold : minimum Kaufman efficiency for a *clean* trend
        (mirrors composite ``efficiency``, default 0.5)
    breakout_lookback : prior-bar extreme window for the trend-mode entry
    mr_lookback : z-score window for the ranging-mode fade
    mr_entry_z : band (in std devs) whose recovery-cross triggers a fade entry
    mr_exit_z : z level at which a mean-reversion position takes the mean
    slow_trend_lookback : window for the slow-drift fade veto; 0 disables
    slow_veto_threshold : |slow_eff| at which an opposing drift blocks a fade
    allow_short : open shorts (futures variant); False = long-only base

    Returns
    -------
    DataFrame with added columns:
        signal        : 1 / -1 / 0 (position transitions, clamped like tema_cross_bd)
        position      : held state per bar (1 long, -1 short, 0 flat)
        ra_label      : composite regime label string ("" during warmup)
        ra_return_eff : rolling return efficiency
        ra_range_eff  : rolling range efficiency
        ra_efficiency : rolling Kaufman efficiency ratio
        ra_adx        : Wilder ADX (0 during warmup)
        ra_z          : z-score of close vs SMA(mr_lookback)
        ra_slow_eff   : ATR-normalized slow drift backing the fade veto
    """
    result = df.copy()
    n = len(result)

    result["signal"] = 0
    result["position"] = 0
    result["ra_label"] = ""
    result["ra_return_eff"] = np.nan
    result["ra_range_eff"] = np.nan
    result["ra_efficiency"] = np.nan
    result["ra_adx"] = 0.0
    result["ra_z"] = np.nan
    result["ra_slow_eff"] = np.nan
    # _compute_adx_components indexes smooth_tr[adx_period] unguarded — any
    # frame with n <= adx_period raises IndexError. Too short to prime ADX ⇒
    # return the already-initialized all-flat frame (a short backtest window
    # or newly-listed symbol must be a no-signal frame, not a script failure).
    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    if n <= adx_period:
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    # ATR mirrors the in-repo convention (shared_tools/atr.standard_atr and the
    # inline registry sites): SMA of true range, integer-rounded only when >= 100.
    tr = pd.concat([
        high - low,
        (high - close.shift(1)).abs(),
        (low - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    _atr = tr.rolling(window=period).mean()
    atr = _atr.where(_atr < 100, _atr.round(0))

    # Rolling equivalents of regime._composite_efficiency_metrics: a window of
    # `period` bars ending at bar t spans close_{t-period+1}..close_t, i.e.
    # period-1 bar-to-bar moves.
    denom = atr * period
    net = close - close.shift(period - 1)
    return_eff = (net / denom).where(denom > 0, 0.0)
    range_eff = (
        (high.rolling(window=period).max() - low.rolling(window=period).min()) / denom
    ).where(denom > 0, 0.0)
    path = close.diff().abs().rolling(window=period - 1).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0)

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    adx = pd.Series(comps["adx"], index=result.index)

    # Vectorized label mapping (mirrors regime.map_composite_label; ATR <= 0
    # with a primed window resolves to ranging_quiet exactly like
    # compute_regime_composite leaves its default label).
    warmup = net.isna() | atr.isna()
    big_move = return_eff.abs() >= return_eff_threshold
    up = return_eff > 0
    high_adx = adx >= adx_threshold
    wide = range_eff >= range_eff_threshold
    clean = (efficiency >= efficiency_threshold) & high_adx
    labels = np.select(
        [
            warmup,
            big_move & up & clean,
            big_move & up,
            big_move & clean,
            big_move,
            high_adx,
            wide,
        ],
        [
            _WARMUP,
            _TREND_UP_CLEAN,
            _TREND_UP_CHOPPY,
            _TREND_DOWN_CLEAN,
            _TREND_DOWN_CHOPPY,
            _RANGING_DIRECTIONAL,
            _RANGING_VOLATILE,
        ],
        default=_RANGING_QUIET,
    )

    # Entry triggers (all trailing).
    breakout_up = (close > high.rolling(window=breakout_lookback).max().shift(1)).values
    breakout_down = (close < low.rolling(window=breakout_lookback).min().shift(1)).values
    z = (close - close.rolling(window=mr_lookback).mean()) / close.rolling(window=mr_lookback).std()
    mr_long_trig = ((z > -mr_entry_z) & (z.shift(1) <= -mr_entry_z)).values
    mr_short_trig = ((z < mr_entry_z) & (z.shift(1) >= mr_entry_z)).values
    z_vals = z.values

    # Slow-trend fade veto: ATR-normalized drift over a long window (same
    # construction as return_eff). NaN warmup compares False on both sides —
    # no veto until the slow window primes, preserving short-frame behavior.
    if slow_trend_lookback > 0:
        slow_denom = atr * slow_trend_lookback
        slow_eff = ((close - close.shift(slow_trend_lookback)) / slow_denom).where(
            slow_denom > 0, 0.0
        )
        veto_long_fade = (slow_eff <= -slow_veto_threshold).values
        veto_short_fade = (slow_eff >= slow_veto_threshold).values
    else:
        slow_eff = pd.Series(np.nan, index=result.index)
        veto_long_fade = np.zeros(n, dtype=bool)
        veto_short_fade = np.zeros(n, dtype=bool)

    up_family = (_TREND_UP_CLEAN, _TREND_UP_CHOPPY)
    down_family = (_TREND_DOWN_CLEAN, _TREND_DOWN_CHOPPY)
    # Choppy "trends" are swings inside a range (big move, low efficiency) —
    # exactly what the z-recovery fade targets; see module docstring.
    mr_family = (_RANGING_QUIET, _RANGING_VOLATILE, _TREND_UP_CHOPPY, _TREND_DOWN_CHOPPY)

    # Forward-only state loop. pos encodes both side and logic:
    # 1 trend-long, -1 trend-short, 2 mr-long, -2 mr-short, 0 flat.
    pos = 0
    positions = np.zeros(n, dtype=int)
    for i in range(n):
        lab = labels[i]
        if lab == _WARMUP:
            pos = 0
            continue

        # Exits: the regime flipped against the held logic.
        if pos == 1 and lab not in up_family:
            pos = 0
        elif pos == -1 and lab not in down_family:
            pos = 0
        elif pos == 2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] >= mr_exit_z) or lab == _TREND_DOWN_CLEAN
        ):
            pos = 0
        elif pos == -2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] <= -mr_exit_z) or lab == _TREND_UP_CLEAN
        ):
            pos = 0

        # Entries (evaluated when flat, including the bar that just exited —
        # allows a direct fade→breakout handoff; diff is clamped below).
        if pos == 0:
            if lab == _TREND_UP_CLEAN and breakout_up[i]:
                pos = 1
            elif allow_short and lab == _TREND_DOWN_CLEAN and breakout_down[i]:
                pos = -1
            elif lab in mr_family:
                if mr_long_trig[i] and not veto_long_fade[i]:
                    pos = 2
                elif allow_short and mr_short_trig[i] and not veto_short_fade[i]:
                    pos = -2

        positions[i] = 1 if pos > 0 else (-1 if pos < 0 else 0)

    pos_series = pd.Series(positions, index=result.index)
    result["position"] = pos_series
    # A direct long→short handoff yields diff == -2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = pos_series.diff().fillna(0).clip(-1, 1).astype(int)
    result["ra_label"] = [_LABEL_NAMES[int(code)] for code in labels]
    result["ra_return_eff"] = return_eff
    result["ra_range_eff"] = range_eff
    result["ra_efficiency"] = efficiency
    result["ra_adx"] = adx
    result["ra_z"] = z
    result["ra_slow_eff"] = slow_eff
    return result
