"""
Vol Momentum — volatility-targeted time-series momentum (#959).

The naive ``momentum`` strategy keys off a raw percent rate-of-change, so the
same threshold means wildly different things in quiet vs violent markets and
it happily chases choppy moves. Vol Momentum normalizes the N-bar net move by
the straight-line ATR travel over the same window and only enters when the
Kaufman efficiency ratio confirms the move was a clean trend, not chop.

Signal definition
-----------------
``vol_mom[t]   = (close[t] - close[t-N]) / (ATR[t] * N)``
    Net move in ATR-units per bar of straight-line ATR travel — the same
    normalization as ``return_eff`` in ``shared_tools/regime.py``
    (``_composite_efficiency_metrics``). Roughly within [-1, 1].

``efficiency[t] = |close[t] - close[t-N]| / sum(|close.diff()|, N bars)``
    Kaufman efficiency ratio in [0, 1]; 1 = perfectly straight move,
    near 0 = round-trip chop. Same convention as ``regime.py``.

ATR is the simple rolling mean of True Range with the ``standard_atr``
integer-rounding rule (values >= 100 round to integers) so stamped ATRs match
``shared_tools/atr.py`` / ``breakout_strategy`` / ``atr_breakout_strategy``.

Rules (long; short is the mirror, gated by ``allow_short``)
-----------------------------------------------------------
Enter long  when ``vol_mom >  entry_threshold`` AND ``efficiency >= eff_entry``.
Exit  long  when ``vol_mom <  exit_threshold``  OR  ``efficiency <  eff_exit``
            (hysteresis: the hold band [exit_threshold, entry_threshold]
            avoids churning around a single cutoff).
Enter short when ``vol_mom < -entry_threshold`` AND ``efficiency >= eff_entry``.
Exit  short when ``vol_mom > -exit_threshold``  OR  ``efficiency <  eff_exit``.

Anti-look-ahead: every input at bar t is a rolling window over closed bars
up to and including t; the position recursion only reads bar t and the bar
t-1 state. A signal emitted at bar N fills at bar N+1's open (engine
contract, ``backtest/tests/test_backtester_lookahead.py``).

Emits ``signal`` in {-1, 0, 1} as position *transitions* (a direct
long→short flip is clamped to a single step, like ``tema_cross_bd``).
"""

import numpy as np
import pandas as pd


def vol_momentum_core(
    df: pd.DataFrame,
    mom_window: int = 24,
    atr_period: int = 14,
    entry_threshold: float = 0.30,
    exit_threshold: float = 0.05,
    eff_entry: float = 0.35,
    eff_exit: float = 0.15,
    allow_short: bool = False,
) -> pd.DataFrame:
    """Generate volatility-targeted momentum signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    mom_window : N — bars for the net-move / efficiency window
    atr_period : rolling True-Range mean window
    entry_threshold : enter when |vol_mom| exceeds this (ATR-travel units)
    exit_threshold : exit when the held side's vol_mom decays below this;
        must be < entry_threshold for the hysteresis band to exist
    eff_entry : minimum Kaufman efficiency ratio to allow an entry
    eff_exit : efficiency floor — a held position exits when efficiency
        collapses below this
    allow_short : mirror entries on the short side (futures variant)

    Returns
    -------
    DataFrame with added columns:
        signal     : 1 (open/flip toward long), -1 (close long / open short), 0
        position   : held state in {-1, 0, 1} after this bar's transition
        atr        : rolling-mean True Range (standard_atr rounding rule)
        vol_mom    : ATR-normalized N-bar net move
        efficiency : Kaufman efficiency ratio over the same window
    """
    result = df.copy()
    n = len(result)

    close = result["close"].astype(float)
    high = result["high"].astype(float)
    low = result["low"].astype(float)
    prev_close = close.shift(1)

    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    atr = tr.rolling(window=atr_period).mean()
    # standard_atr convention: integer-round only when ATR >= 100.
    result["atr"] = atr.where(atr < 100, atr.round(0))

    net = close - close.shift(mom_window)
    denom = result["atr"] * float(mom_window)
    # Warmup / degenerate denominators resolve to 0.0: no entry can fire and
    # a held position exits — the conservative side of every gate.
    vol_mom = (net / denom).where(denom > 0, 0.0).fillna(0.0)
    path = close.diff().abs().rolling(window=mom_window).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0).fillna(0.0)

    result["vol_mom"] = vol_mom
    result["efficiency"] = efficiency

    m = vol_mom.to_numpy()
    e = efficiency.to_numpy()
    pos = np.zeros(n, dtype=np.int64)
    for i in range(1, n):
        cur = pos[i - 1]
        # Exits first: momentum decay below the held side's floor, or the
        # move's efficiency collapsing into chop.
        if cur == 1 and (m[i] < exit_threshold or e[i] < eff_exit):
            cur = 0
        elif cur == -1 and (m[i] > -exit_threshold or e[i] < eff_exit):
            cur = 0
        # Entries (an opposite-side entry on the same bar is a direct flip).
        if e[i] >= eff_entry:
            if m[i] > entry_threshold:
                cur = 1
            elif allow_short and m[i] < -entry_threshold:
                cur = -1
        pos[i] = cur

    result["position"] = pos
    # A direct long→short flip yields diff == -2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = (
        pd.Series(pos, index=result.index).diff().fillna(0).clip(-1, 1).astype(int)
    )
    return result
