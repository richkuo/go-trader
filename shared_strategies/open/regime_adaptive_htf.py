"""
Regime-Adaptive HTF — selective mean-reversion fades gated by composite
regime labels classified on a higher timeframe with confirmation hysteresis
(#973).

#967's ``regime_adaptive`` classifies regimes on the same timeframe it trades;
its labels flip late and noisily, and it trades 50-80 times per 5-month
window on 1h frames — at ~0.3% round-trip cost the churn alone explains most
of its OOS bleed (mean Sharpe -1.63 vs the median incumbent bar). The #973
redesign evaluated three mechanisms for making classification slower and more
deliberate than execution; what survived benchmarking is **selectivity**:

1. **HTF classification window** (``htf_factor``, SHIPPED default 6):
   composite metrics (``return_eff``, ``range_eff``, Kaufman ``efficiency`` +
   Wilder ADX) are computed on epoch-aligned higher-timeframe buckets
   resampled in-process from the single native frame (same ``_resample_htf``
   / ``_project_to_native`` machinery as ``mtf_confluence`` #963, including
   its anti-look-ahead contract: a bucket is only readable from the native
   bar at which it has fully closed). ``htf_factor=1`` degenerates to
   native-bar classification.

2. **Confirmation hysteresis** (``confirm_buckets``, SHIPPED default 2): the
   *effective* label only switches after N consecutive HTF buckets carry the
   same raw label (the backtest analogue of live ``regime_confirm_cycles``),
   suppressing label flicker at regime boundaries. Worth +0.3-0.8 OOS Sharpe
   across the htf_factor grid. ``confirm_buckets=1`` disables.

3. **Transition-as-signal** (``trend_entry="transition"``, REJECTED as
   default): treating the confirmed label flip into a clean trend as the
   entry trigger benchmarked worse than the #967 baseline on every dataset
   combination tried — by the time a slow, confirmed label flips, the move
   is exhausted; entering on the flip buys tops. Kept as an opt-in mode for
   the optimizer to revisit on other windows.

The shipped default is therefore **fade-only in confirmed true ranges**
(``trend_entry="off"``, ``fade_labels="ranging"``): a z-score recovery fade
fires only when the confirmed HTF label is ``ranging_quiet`` or
``ranging_volatile``. Clean-trend entries (breakout / pullback / transition
variants all benchmarked below the bar) stay available behind ``trend_entry``
but default off. Crucially, HTF labels make the ranging-only gate *workable*:
#967 had to map choppy-trend labels to fades because a same-timeframe
big-move label almost always covers the z-recovery bar — but a 6x-slower
bucket label does not flip on one native recovery bar, so fades in true
ranges stay reachable without that hack (``fade_labels="all_mr"`` restores
the #967 mapping for comparison).

``ranging_directional`` (high-ADX grind without a decisive move) stays
entry-free — fading a directional grind is the falling-knife case the #956
audit showed unfiltered mean reversion dying on. Fades exit at the mean
(``mr_exit_z``) or when a confirmed *clean* trend ignites against them; the
slow-trend drift veto from #967 (ATR-normalized net move over
``slow_trend_lookback`` native bars opposing the fade side) is retained — it
is orthogonal to the classification redesign and improved OOS materially.

Anti-look-ahead: native triggers are trailing rolling windows / ``shift(1)``;
HTF labels are projected to native bars at bucket close only (the in-progress
bucket is never read); confirmation runs on closed buckets. The signal at bar
N uses bars <= N only and fills at bar N+1 open per the engine contract.

The base registration is long-only (shorts only ever flatten); the futures
variant sets ``allow_short=True`` for bidirectional perps.
"""

import numpy as np
import pandas as pd

from adx_trend import _compute_adx_components
from mtf_confluence import _project_to_native, _resample_htf

# Mirrors regime.COMPOSITE_ADX_PERIOD_CAP (same convention as regime_adaptive).
_COMPOSITE_ADX_PERIOD_CAP = 14

# Int-coded labels; names mirror the 7-label composite vocabulary in
# shared_tools/regime.py (codes match regime_adaptive for inspectability).
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

_UP_FAMILY = (_TREND_UP_CLEAN, _TREND_UP_CHOPPY)
_DOWN_FAMILY = (_TREND_DOWN_CLEAN, _TREND_DOWN_CHOPPY)
# Choppy "trends" are swings inside a range (big move, low efficiency) —
# exactly what the z-recovery fade targets (#967 docstring).
_MR_FAMILY = (_RANGING_QUIET, _RANGING_VOLATILE, _TREND_UP_CHOPPY, _TREND_DOWN_CHOPPY)


def _classify_buckets(
    htf: pd.DataFrame,
    period: int,
    adx_threshold: float,
    return_eff_threshold: float,
    range_eff_threshold: float,
    efficiency_threshold: float,
) -> np.ndarray:
    """Composite-regime label per closed HTF bucket (int codes).

    Rolling equivalents of ``regime._composite_efficiency_metrics`` computed
    on the HTF frame, exactly as ``regime_adaptive`` computes them on the
    native frame: a window of ``period`` buckets ending at bucket t spans
    period-1 bucket-to-bucket moves.
    """
    close = htf["close"]
    high = htf["high"]
    low = htf["low"]

    # ATR mirrors the in-repo convention (shared_tools/atr.standard_atr):
    # SMA of true range, integer-rounded only when >= 100.
    tr = pd.concat([
        high - low,
        (high - close.shift(1)).abs(),
        (low - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    _atr = tr.rolling(window=period).mean()
    atr = _atr.where(_atr < 100, _atr.round(0))

    denom = atr * period
    net = close - close.shift(period - 1)
    return_eff = (net / denom).where(denom > 0, 0.0)
    range_eff = (
        (high.rolling(window=period).max() - low.rolling(window=period).min()) / denom
    ).where(denom > 0, 0.0)
    path = close.diff().abs().rolling(window=period - 1).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0)

    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    adx = pd.Series(comps["adx"], index=htf.index)

    warmup = net.isna() | atr.isna()
    big_move = return_eff.abs() >= return_eff_threshold
    up = return_eff > 0
    high_adx = adx >= adx_threshold
    wide = range_eff >= range_eff_threshold
    clean = (efficiency >= efficiency_threshold) & high_adx
    return np.select(
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


def _confirm_labels(raw: np.ndarray, confirm_buckets: int) -> np.ndarray:
    """Hysteresis over closed buckets: the effective label switches only after
    ``confirm_buckets`` consecutive buckets carry the same raw label
    (the backtest analogue of live ``regime_confirm_cycles``).

    Warmup buckets (code 0) neither confirm a new label nor disturb the
    streak-in-progress semantics: they reset the streak (a gap in agreement)
    and hold the previously confirmed label.
    """
    n = len(raw)
    confirmed = np.zeros(n, dtype=int)
    if confirm_buckets <= 1:
        return raw.copy()
    current = _WARMUP
    streak_label = _WARMUP
    streak = 0
    for i in range(n):
        lab = raw[i]
        if lab == _WARMUP:
            streak_label = _WARMUP
            streak = 0
        elif lab == streak_label:
            streak += 1
        else:
            streak_label = lab
            streak = 1
        if streak_label != _WARMUP and streak >= confirm_buckets and streak_label != current:
            current = streak_label
        confirmed[i] = current
    return confirmed


def regime_adaptive_htf_core(
    df: pd.DataFrame,
    htf_factor: int = 6,
    period: int = 14,
    adx_threshold: float = 20.0,
    return_eff_threshold: float = 0.05,
    range_eff_threshold: float = 0.03,
    efficiency_threshold: float = 0.5,
    confirm_buckets: int = 2,
    trend_entry: str = "off",
    transition_window: int = 6,
    pullback_z: float = 1.0,
    fade_labels: str = "ranging",
    breakout_lookback: int = 10,
    mr_lookback: int = 20,
    mr_entry_z: float = 2.0,
    mr_exit_z: float = 0.0,
    slow_trend_lookback: int = 100,
    slow_veto_threshold: float = 0.05,
    allow_short: bool = False,
) -> pd.DataFrame:
    """Generate HTF-regime-switched breakout / mean-reversion signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    htf_factor : native bars per classification bucket (e.g. 4 turns 1h into
        4h labels); 1 = classify on the native frame
    period : composite metric window in HTF buckets
    adx_threshold / return_eff_threshold / range_eff_threshold /
        efficiency_threshold : label-mapping thresholds (regime_adaptive
        semantics, applied to HTF-bucket metrics)
    confirm_buckets : consecutive HTF buckets required before the effective
        label switches; 1 disables hysteresis
    trend_entry : clean-trend entry mode — "off" (default; fade-only),
        "breakout" (prior N-bar extreme), "pullback" (shallow z-dip recovery
        at ``pullback_z``), or "transition" (confirmed label flip itself,
        within ``transition_window`` native bars)
    transition_window : native bars after a confirmed label flip during which
        ``trend_entry="transition"`` may enter
    pullback_z : shallow recovery band (std devs) for ``trend_entry="pullback"``
    fade_labels : "ranging" (default; fade only in ``ranging_quiet`` /
        ``ranging_volatile``) or "all_mr" (#967 mapping: also fade choppy
        trends)
    breakout_lookback : prior-bar extreme window (native bars) for
        ``trend_entry="breakout"``
    mr_lookback : z-score window (native bars) for the ranging-mode fade
    mr_entry_z : band (in std devs) whose recovery-cross triggers a fade entry
    mr_exit_z : z level at which a mean-reversion position takes the mean
    slow_trend_lookback : window (native bars) for the slow-drift fade veto;
        0 disables
    slow_veto_threshold : |slow_eff| at which an opposing drift blocks a fade
    allow_short : open shorts (futures variant); False = long-only base

    Returns
    -------
    DataFrame with added columns:
        signal         : 1 / -1 / 0 (position transitions, clamped)
        position       : held state per bar (1 long, -1 short, 0 flat)
        rah_label      : confirmed HTF regime label visible at each native bar
        rah_raw_label  : unconfirmed HTF label visible at each native bar
        rah_z          : z-score of close vs SMA(mr_lookback)
        rah_slow_eff   : ATR-normalized slow drift backing the fade veto
    """
    htf_factor = max(int(htf_factor), 1)
    result = df.copy()
    n = len(result)

    result["signal"] = 0
    result["position"] = 0
    result["rah_label"] = ""
    result["rah_raw_label"] = ""
    result["rah_z"] = np.nan
    result["rah_slow_eff"] = np.nan
    if n == 0:
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    # ── HTF classification (closed buckets only) ────────────────────────────
    htf, visible_at = _resample_htf(result, htf_factor)
    # _compute_adx_components indexes smooth_tr[adx_period] unguarded — any
    # frame with n <= adx_period raises IndexError (same guard as
    # regime_adaptive: too short to prime ⇒ all-flat frame, not a failure).
    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    if len(htf) <= adx_period:
        return result

    raw_labels = _classify_buckets(
        htf, period, adx_threshold, return_eff_threshold,
        range_eff_threshold, efficiency_threshold,
    )
    conf_labels = _confirm_labels(raw_labels, confirm_buckets)

    conf_native = (
        _project_to_native(pd.Series(conf_labels, index=htf.index), visible_at, result.index)
        .fillna(0).astype(int).to_numpy()
    )
    raw_native = (
        _project_to_native(pd.Series(raw_labels, index=htf.index), visible_at, result.index)
        .fillna(0).astype(int).to_numpy()
    )

    # ── Native-frame entry triggers (all trailing) ──────────────────────────
    breakout_up = (close > high.rolling(window=breakout_lookback).max().shift(1)).values
    breakout_down = (close < low.rolling(window=breakout_lookback).min().shift(1)).values
    z = (close - close.rolling(window=mr_lookback).mean()) / close.rolling(window=mr_lookback).std()
    mr_long_trig = ((z > -mr_entry_z) & (z.shift(1) <= -mr_entry_z)).values
    mr_short_trig = ((z < mr_entry_z) & (z.shift(1) >= mr_entry_z)).values
    # Pullback-resume: a shallow dip against the confirmed trend recovers
    # (same recovery-cross family as the fade, at a shallower band).
    pb_long_trig = ((z > -pullback_z) & (z.shift(1) <= -pullback_z)).values
    pb_short_trig = ((z < pullback_z) & (z.shift(1) >= pullback_z)).values
    z_vals = z.values

    fade_family = _MR_FAMILY if fade_labels == "all_mr" else (_RANGING_QUIET, _RANGING_VOLATILE)

    # Slow-trend fade veto (native bars, #967 semantics): ATR-normalized drift.
    # NaN warmup compares False on both sides — no veto until primed. The ATR
    # window reuses mr_lookback (the fade's own scale; both default 20).
    if slow_trend_lookback > 0:
        tr_native = pd.concat([
            high - low,
            (high - close.shift(1)).abs(),
            (low - close.shift(1)).abs(),
        ], axis=1).max(axis=1)
        _atr_native = tr_native.rolling(window=mr_lookback).mean()
        atr_native = _atr_native.where(_atr_native < 100, _atr_native.round(0))
        slow_denom = atr_native * slow_trend_lookback
        slow_eff = ((close - close.shift(slow_trend_lookback)) / slow_denom).where(
            slow_denom > 0, 0.0
        )
        veto_long_fade = (slow_eff <= -slow_veto_threshold).values
        veto_short_fade = (slow_eff >= slow_veto_threshold).values
    else:
        slow_eff = pd.Series(np.nan, index=result.index)
        veto_long_fade = np.zeros(n, dtype=bool)
        veto_short_fade = np.zeros(n, dtype=bool)

    # ── Forward-only state loop ─────────────────────────────────────────────
    # pos encodes side and logic: 1 trend-long, -1 trend-short, 2 mr-long,
    # -2 mr-short, 0 flat.
    pos = 0
    positions = np.zeros(n, dtype=int)
    last_flip_i = -1  # native bar where the confirmed label last changed
    prev_lab = _WARMUP
    for i in range(n):
        lab = conf_native[i]
        if lab != prev_lab and lab != _WARMUP:
            last_flip_i = i
        prev_lab = lab

        if lab == _WARMUP:
            pos = 0
            continue

        # Exits: the confirmed regime flipped against the held logic.
        if pos == 1 and lab not in _UP_FAMILY:
            pos = 0
        elif pos == -1 and lab not in _DOWN_FAMILY:
            pos = 0
        elif pos == 2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] >= mr_exit_z) or lab == _TREND_DOWN_CLEAN
        ):
            pos = 0
        elif pos == -2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] <= -mr_exit_z) or lab == _TREND_UP_CLEAN
        ):
            pos = 0

        # Entries (evaluated when flat, including the bar that just exited).
        if pos == 0:
            if trend_entry == "breakout":
                trend_long_trig = breakout_up[i]
                trend_short_trig = breakout_down[i]
            elif trend_entry == "pullback":
                trend_long_trig = pb_long_trig[i]
                trend_short_trig = pb_short_trig[i]
            elif trend_entry == "transition":
                trend_long_trig = i - last_flip_i < transition_window
                trend_short_trig = trend_long_trig
            else:  # "off" — fade-only
                trend_long_trig = False
                trend_short_trig = False
            if lab == _TREND_UP_CLEAN and trend_long_trig:
                pos = 1
            elif allow_short and lab == _TREND_DOWN_CLEAN and trend_short_trig:
                pos = -1
            elif lab in fade_family:
                if mr_long_trig[i] and not veto_long_fade[i]:
                    pos = 2
                elif allow_short and mr_short_trig[i] and not veto_short_fade[i]:
                    pos = -2

        positions[i] = 1 if pos > 0 else (-1 if pos < 0 else 0)

    pos_series = pd.Series(positions, index=result.index)
    result["position"] = pos_series
    # A direct long→short handoff yields diff == -2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = pos_series.diff().fillna(0).clip(-1, 1).astype(int)
    result["rah_label"] = [_LABEL_NAMES[int(code)] for code in conf_native]
    result["rah_raw_label"] = [_LABEL_NAMES[int(code)] for code in raw_native]
    result["rah_z"] = z
    result["rah_slow_eff"] = slow_eff
    return result
