"""
Chart Pattern Recognition — algorithmic detection of classic chart patterns.

Detects reversal patterns (Double Top/Bottom, Head & Shoulders, Triple Top/Bottom)
and continuation patterns (Bull/Bear Flag, Triangles, Cup & Handle) using swing
high/low pivot detection with volume confirmation.

Each detector returns a list of PatternMatch instances. The chart_pattern_core()
orchestrator runs all detectors, filters by volume confirmation, and produces a
signal column (1 = buy, -1 = sell, 0 = hold).
"""

from dataclasses import dataclass
import numpy as np
import pandas as pd


@dataclass
class PatternMatch:
    pattern: str       # e.g. "double_top", "head_shoulders"
    signal: int        # +1 or -1
    bar_index: int     # index where pattern completes (breakout bar)
    neckline: float    # neckline / breakout level
    confidence: float  # 0-1 score


# ─────────────────────────────────────────────
# Foundation: Swing Point Detection
# ─────────────────────────────────────────────

def find_swing_points(
    highs: pd.Series,
    lows: pd.Series,
    lookback: int = 5,
) -> tuple:
    """
    ZigZag-style swing high/low detection.

    A swing high at bar i: high[i] is the max of highs[i-lookback : i+lookback+1].
    A swing low at bar i:  low[i]  is the min of lows[i-lookback : i+lookback+1].

    Returns (swing_highs, swing_lows) as Series with NaN where no swing point
    exists and the price level where one does.
    """
    window = 2 * lookback + 1
    roll_max = highs.rolling(window=window, center=True).max()
    roll_min = lows.rolling(window=window, center=True).min()

    swing_highs = highs.where(highs == roll_max, other=np.nan)
    swing_lows = lows.where(lows == roll_min, other=np.nan)

    # Remove flat-top duplicates: keep only the first occurrence in each plateau.
    sh_mask = swing_highs.notna()
    sh_groups = (~sh_mask).cumsum()
    first_in_group = sh_mask & (~sh_mask.shift(1, fill_value=True))
    swing_highs = swing_highs.where(first_in_group, other=np.nan)

    sl_mask = swing_lows.notna()
    sl_groups = (~sl_mask).cumsum()
    first_in_group_low = sl_mask & (~sl_mask.shift(1, fill_value=True))
    swing_lows = swing_lows.where(first_in_group_low, other=np.nan)

    return swing_highs, swing_lows


def _get_swing_indices(swing_series: pd.Series) -> list:
    """Return sorted list of integer indices where swing points exist."""
    return sorted(swing_series.dropna().index.tolist())


# ─────────────────────────────────────────────
# Volume Confirmation
# ─────────────────────────────────────────────

def volume_confirmed(
    volume: pd.Series,
    index: int,
    vol_period: int = 20,
    vol_multiplier: float = 1.5,
) -> bool:
    """True if volume at `index` exceeds vol_multiplier * avg over preceding vol_period bars."""
    if index < vol_period:
        return True  # not enough history — allow
    avg_vol = volume.iloc[index - vol_period:index].mean()
    if avg_vol <= 0:
        return True
    return volume.iloc[index] > vol_multiplier * avg_vol


# ─────────────────────────────────────────────
# Pattern Detectors — Reversal
# ─────────────────────────────────────────────

def detect_double_top(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Two consecutive swing highs within tolerance; break below intervening low."""
    matches = []
    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)
    if len(sh_idx) < 2:
        return matches

    for i in range(len(sh_idx) - 1):
        h1_bar, h2_bar = sh_idx[i], sh_idx[i + 1]
        h1, h2 = swing_highs.iloc[h1_bar], swing_highs.iloc[h2_bar]

        # Peaks within tolerance
        if abs(h1 - h2) / max(h1, 1e-9) > tolerance:
            continue

        # Find intervening swing low (neckline)
        between_lows = [s for s in sl_idx if h1_bar < s < h2_bar]
        if not between_lows:
            continue
        neckline_bar = min(between_lows, key=lambda s: swing_lows.iloc[s])
        neckline = swing_lows.iloc[neckline_bar]

        # Find breakout: first close below neckline after second peak
        for j in range(h2_bar + 1, len(close)):
            if close.iloc[j] < neckline:
                matches.append(PatternMatch(
                    pattern="double_top", signal=-1,
                    bar_index=j, neckline=neckline, confidence=0.7,
                ))
                break

    return matches


def detect_double_bottom(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Two consecutive swing lows within tolerance; break above intervening high."""
    matches = []
    sl_idx = _get_swing_indices(swing_lows)
    sh_idx = _get_swing_indices(swing_highs)
    if len(sl_idx) < 2:
        return matches

    for i in range(len(sl_idx) - 1):
        l1_bar, l2_bar = sl_idx[i], sl_idx[i + 1]
        l1, l2 = swing_lows.iloc[l1_bar], swing_lows.iloc[l2_bar]

        if abs(l1 - l2) / max(l1, 1e-9) > tolerance:
            continue

        between_highs = [s for s in sh_idx if l1_bar < s < l2_bar]
        if not between_highs:
            continue
        neckline_bar = max(between_highs, key=lambda s: swing_highs.iloc[s])
        neckline = swing_highs.iloc[neckline_bar]

        for j in range(l2_bar + 1, len(close)):
            if close.iloc[j] > neckline:
                matches.append(PatternMatch(
                    pattern="double_bottom", signal=1,
                    bar_index=j, neckline=neckline, confidence=0.7,
                ))
                break

    return matches


def detect_triple_top(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Three consecutive swing highs within tolerance; break below lowest intervening low."""
    matches = []
    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)
    if len(sh_idx) < 3:
        return matches

    for i in range(len(sh_idx) - 2):
        h1_bar, h2_bar, h3_bar = sh_idx[i], sh_idx[i + 1], sh_idx[i + 2]
        h1 = swing_highs.iloc[h1_bar]
        h2 = swing_highs.iloc[h2_bar]
        h3 = swing_highs.iloc[h3_bar]

        avg_h = (h1 + h2 + h3) / 3
        if any(abs(h - avg_h) / max(avg_h, 1e-9) > tolerance for h in (h1, h2, h3)):
            continue

        between_lows = [s for s in sl_idx if h1_bar < s < h3_bar]
        if not between_lows:
            continue
        neckline = min(swing_lows.iloc[s] for s in between_lows)

        for j in range(h3_bar + 1, len(close)):
            if close.iloc[j] < neckline:
                matches.append(PatternMatch(
                    pattern="triple_top", signal=-1,
                    bar_index=j, neckline=neckline, confidence=0.8,
                ))
                break

    return matches


def detect_triple_bottom(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Three consecutive swing lows within tolerance; break above highest intervening high."""
    matches = []
    sl_idx = _get_swing_indices(swing_lows)
    sh_idx = _get_swing_indices(swing_highs)
    if len(sl_idx) < 3:
        return matches

    for i in range(len(sl_idx) - 2):
        l1_bar, l2_bar, l3_bar = sl_idx[i], sl_idx[i + 1], sl_idx[i + 2]
        l1 = swing_lows.iloc[l1_bar]
        l2 = swing_lows.iloc[l2_bar]
        l3 = swing_lows.iloc[l3_bar]

        avg_l = (l1 + l2 + l3) / 3
        if any(abs(l - avg_l) / max(avg_l, 1e-9) > tolerance for l in (l1, l2, l3)):
            continue

        between_highs = [s for s in sh_idx if l1_bar < s < l3_bar]
        if not between_highs:
            continue
        neckline = max(swing_highs.iloc[s] for s in between_highs)

        for j in range(l3_bar + 1, len(close)):
            if close.iloc[j] > neckline:
                matches.append(PatternMatch(
                    pattern="triple_bottom", signal=1,
                    bar_index=j, neckline=neckline, confidence=0.8,
                ))
                break

    return matches


def detect_head_and_shoulders(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Three swing highs where middle is highest and shoulders are within tolerance."""
    matches = []
    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)
    if len(sh_idx) < 3:
        return matches

    for i in range(len(sh_idx) - 2):
        ls_bar, head_bar, rs_bar = sh_idx[i], sh_idx[i + 1], sh_idx[i + 2]
        ls = swing_highs.iloc[ls_bar]
        head = swing_highs.iloc[head_bar]
        rs = swing_highs.iloc[rs_bar]

        # Head must be highest
        if head <= ls or head <= rs:
            continue
        # Shoulders approximately equal
        if abs(ls - rs) / max(ls, 1e-9) > tolerance:
            continue

        # Neckline: average of the two troughs between the three peaks
        between_lows = [s for s in sl_idx if ls_bar < s < rs_bar]
        if not between_lows:
            continue
        neckline = np.mean([swing_lows.iloc[s] for s in between_lows])

        for j in range(rs_bar + 1, len(close)):
            if close.iloc[j] < neckline:
                matches.append(PatternMatch(
                    pattern="head_shoulders", signal=-1,
                    bar_index=j, neckline=neckline, confidence=0.85,
                ))
                break

    return matches


def detect_inverse_head_and_shoulders(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """Three swing lows where middle is lowest and shoulders are within tolerance."""
    matches = []
    sl_idx = _get_swing_indices(swing_lows)
    sh_idx = _get_swing_indices(swing_highs)
    if len(sl_idx) < 3:
        return matches

    for i in range(len(sl_idx) - 2):
        ls_bar, head_bar, rs_bar = sl_idx[i], sl_idx[i + 1], sl_idx[i + 2]
        ls = swing_lows.iloc[ls_bar]
        head = swing_lows.iloc[head_bar]
        rs = swing_lows.iloc[rs_bar]

        if head >= ls or head >= rs:
            continue
        if abs(ls - rs) / max(ls, 1e-9) > tolerance:
            continue

        between_highs = [s for s in sh_idx if ls_bar < s < rs_bar]
        if not between_highs:
            continue
        neckline = np.mean([swing_highs.iloc[s] for s in between_highs])

        for j in range(rs_bar + 1, len(close)):
            if close.iloc[j] > neckline:
                matches.append(PatternMatch(
                    pattern="inv_head_shoulders", signal=1,
                    bar_index=j, neckline=neckline, confidence=0.85,
                ))
                break

    return matches


# ─────────────────────────────────────────────
# Pattern Detectors — Continuation (Flags)
# ─────────────────────────────────────────────

def _detect_flag(
    highs: pd.Series, lows: pd.Series, close: pd.Series, volume: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    direction: int,  # +1 for bull flag, -1 for bear flag
    pole_min_bars: int = 5,
    pole_max_bars: int = 20,
    flag_min_bars: int = 5,
    flag_max_bars: int = 30,
    pole_atr_mult: float = 2.0,
) -> list:
    """
    Detect flag pattern in the given direction.

    Bull flag (+1): strong rally (pole) followed by a slight downward consolidation
    (flag), then breakout above the flag's upper boundary.

    Bear flag (-1): strong drop (pole) followed by a slight upward consolidation
    (flag), then breakdown below the flag's lower boundary.
    """
    matches = []
    n = len(close)
    if n < pole_min_bars + flag_min_bars + 10:
        return matches

    # Compute ATR for pole strength measurement
    tr = pd.concat([
        highs - lows,
        (highs - close.shift(1)).abs(),
        (lows - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    atr = tr.rolling(window=14, min_periods=1).mean()

    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)

    if direction == 1:
        # Bull flag: look for swing highs as potential pole tops
        for peak_bar in sh_idx:
            if peak_bar < pole_min_bars or peak_bar >= n - flag_min_bars:
                continue
            peak_price = swing_highs.iloc[peak_bar]

            # Find pole start: the low before the peak
            pole_start = max(0, peak_bar - pole_max_bars)
            pole_low_bar = pole_start + close.iloc[pole_start:peak_bar].idxmin()
            if not (pole_min_bars <= peak_bar - pole_low_bar <= pole_max_bars):
                # Try using the actual iloc index
                segment = close.iloc[pole_start:peak_bar]
                pole_low_bar = segment.values.argmin() + pole_start
                if not (pole_min_bars <= peak_bar - pole_low_bar <= pole_max_bars):
                    continue
            pole_move = peak_price - close.iloc[pole_low_bar]
            if atr.iloc[peak_bar] > 0 and pole_move < pole_atr_mult * atr.iloc[peak_bar]:
                continue

            # Find flag: consolidation after the peak
            flag_end = min(peak_bar + flag_max_bars, n - 1)
            flag_segment = close.iloc[peak_bar:flag_end + 1]
            if len(flag_segment) < flag_min_bars:
                continue

            # Flag should slope downward or sideways (not continuing up)
            flag_highs = highs.iloc[peak_bar:flag_end + 1]
            flag_lows = lows.iloc[peak_bar:flag_end + 1]
            flag_high_level = flag_highs.max()
            flag_low_level = flag_lows.min()

            # Flag range should be less than half the pole
            if (flag_high_level - flag_low_level) > 0.5 * pole_move:
                continue

            # Breakout: close above the flag high
            for j in range(peak_bar + flag_min_bars, min(flag_end + 1, n)):
                if close.iloc[j] > flag_high_level:
                    matches.append(PatternMatch(
                        pattern="bull_flag", signal=1,
                        bar_index=j, neckline=flag_high_level, confidence=0.75,
                    ))
                    break

    else:
        # Bear flag: look for swing lows as potential pole bottoms
        for trough_bar in sl_idx:
            if trough_bar < pole_min_bars or trough_bar >= n - flag_min_bars:
                continue
            trough_price = swing_lows.iloc[trough_bar]

            pole_start = max(0, trough_bar - pole_max_bars)
            segment = close.iloc[pole_start:trough_bar]
            if len(segment) == 0:
                continue
            pole_high_bar = segment.values.argmax() + pole_start
            if not (pole_min_bars <= trough_bar - pole_high_bar <= pole_max_bars):
                continue
            pole_move = close.iloc[pole_high_bar] - trough_price
            if atr.iloc[trough_bar] > 0 and pole_move < pole_atr_mult * atr.iloc[trough_bar]:
                continue

            flag_end = min(trough_bar + flag_max_bars, n - 1)
            flag_segment = close.iloc[trough_bar:flag_end + 1]
            if len(flag_segment) < flag_min_bars:
                continue

            flag_highs = highs.iloc[trough_bar:flag_end + 1]
            flag_lows = lows.iloc[trough_bar:flag_end + 1]
            flag_high_level = flag_highs.max()
            flag_low_level = flag_lows.min()

            if (flag_high_level - flag_low_level) > 0.5 * pole_move:
                continue

            for j in range(trough_bar + flag_min_bars, min(flag_end + 1, n)):
                if close.iloc[j] < flag_low_level:
                    matches.append(PatternMatch(
                        pattern="bear_flag", signal=-1,
                        bar_index=j, neckline=flag_low_level, confidence=0.75,
                    ))
                    break

    return matches


def detect_bull_flag(
    highs: pd.Series, lows: pd.Series, close: pd.Series, volume: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    return _detect_flag(highs, lows, close, volume, swing_highs, swing_lows, direction=1)


def detect_bear_flag(
    highs: pd.Series, lows: pd.Series, close: pd.Series, volume: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    return _detect_flag(highs, lows, close, volume, swing_highs, swing_lows, direction=-1)


# ─────────────────────────────────────────────
# Pattern Detectors — Triangles
# ─────────────────────────────────────────────

def _fit_slope(indices: list, values: list) -> float:
    """Simple linear regression slope. Returns 0 if fewer than 2 points."""
    if len(indices) < 2:
        return 0.0
    x = np.array(indices, dtype=float)
    y = np.array(values, dtype=float)
    x_mean = x.mean()
    y_mean = y.mean()
    denom = ((x - x_mean) ** 2).sum()
    if denom == 0:
        return 0.0
    return ((x - x_mean) * (y - y_mean)).sum() / denom


def _detect_triangle(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    pattern_type: str,  # "ascending", "descending", "symmetrical"
    tolerance: float = 0.03,
    min_points: int = 4,
) -> list:
    """
    Detect triangle patterns using linear regression on swing points.

    Ascending: flat resistance (slope ~0), rising support (slope > 0). Signal +1.
    Descending: flat support (slope ~0), declining resistance (slope < 0). Signal -1.
    Symmetrical: converging lines (resistance slope < 0, support slope > 0). Signal = breakout direction.
    """
    matches = []
    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)

    if len(sh_idx) < 2 or len(sl_idx) < 2:
        return matches
    if len(sh_idx) + len(sl_idx) < min_points:
        return matches

    # Use a sliding window of the last N swing points
    window_sizes = [4, 6, 8]
    for ws in window_sizes:
        if len(sh_idx) < ws // 2 or len(sl_idx) < ws // 2:
            continue

        # Take the most recent swing points
        recent_sh = sh_idx[-(ws // 2):]
        recent_sl = sl_idx[-(ws // 2):]

        sh_values = [swing_highs.iloc[s] for s in recent_sh]
        sl_values = [swing_lows.iloc[s] for s in recent_sl]

        resistance_slope = _fit_slope(recent_sh, sh_values)
        support_slope = _fit_slope(recent_sl, sl_values)

        # Normalize slopes by average price level
        avg_price = (np.mean(sh_values) + np.mean(sl_values)) / 2
        if avg_price <= 0:
            continue
        norm_r_slope = resistance_slope / avg_price
        norm_s_slope = support_slope / avg_price

        flat_threshold = 0.0001  # slope per bar, normalized

        triangle_end = max(recent_sh[-1], recent_sl[-1])

        if pattern_type == "ascending":
            if abs(norm_r_slope) > flat_threshold:
                continue
            if norm_s_slope <= flat_threshold:
                continue
            # Breakout above resistance
            resistance_level = np.mean(sh_values)
            for j in range(triangle_end + 1, min(triangle_end + 20, len(close))):
                if close.iloc[j] > resistance_level:
                    matches.append(PatternMatch(
                        pattern="ascending_triangle", signal=1,
                        bar_index=j, neckline=resistance_level, confidence=0.7,
                    ))
                    break

        elif pattern_type == "descending":
            if abs(norm_s_slope) > flat_threshold:
                continue
            if norm_r_slope >= -flat_threshold:
                continue
            support_level = np.mean(sl_values)
            for j in range(triangle_end + 1, min(triangle_end + 20, len(close))):
                if close.iloc[j] < support_level:
                    matches.append(PatternMatch(
                        pattern="descending_triangle", signal=-1,
                        bar_index=j, neckline=support_level, confidence=0.7,
                    ))
                    break

        elif pattern_type == "symmetrical":
            if norm_r_slope >= -flat_threshold or norm_s_slope <= flat_threshold:
                continue
            # Lines must be converging
            mid_level = avg_price
            for j in range(triangle_end + 1, min(triangle_end + 20, len(close))):
                upper = np.mean(sh_values) + resistance_slope * (j - np.mean(recent_sh))
                lower = np.mean(sl_values) + support_slope * (j - np.mean(recent_sl))
                if close.iloc[j] > upper:
                    matches.append(PatternMatch(
                        pattern="symmetrical_triangle", signal=1,
                        bar_index=j, neckline=upper, confidence=0.65,
                    ))
                    break
                elif close.iloc[j] < lower:
                    matches.append(PatternMatch(
                        pattern="symmetrical_triangle", signal=-1,
                        bar_index=j, neckline=lower, confidence=0.65,
                    ))
                    break

    return matches


def detect_ascending_triangle(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    return _detect_triangle(highs, lows, close, swing_highs, swing_lows, "ascending", tolerance)


def detect_descending_triangle(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    return _detect_triangle(highs, lows, close, swing_highs, swing_lows, "descending", tolerance)


def detect_symmetrical_triangle(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    return _detect_triangle(highs, lows, close, swing_highs, swing_lows, "symmetrical", tolerance)


# ─────────────────────────────────────────────
# Pattern Detectors — Cup & Handle
# ─────────────────────────────────────────────

def detect_cup_and_handle(
    highs: pd.Series, lows: pd.Series, close: pd.Series,
    swing_highs: pd.Series, swing_lows: pd.Series,
    tolerance: float = 0.03,
) -> list:
    """
    Cup & Handle: U-shaped bottom with left/right rims at similar height,
    followed by a small pullback (handle), then breakout above the rim.
    """
    matches = []
    sh_idx = _get_swing_indices(swing_highs)
    sl_idx = _get_swing_indices(swing_lows)
    if len(sh_idx) < 3 or len(sl_idx) < 1:
        return matches

    for i in range(len(sh_idx) - 2):
        left_rim_bar = sh_idx[i]
        left_rim = swing_highs.iloc[left_rim_bar]

        # Find a swing low between left rim and a later swing high (cup bottom)
        for j_idx in range(i + 1, len(sh_idx)):
            right_rim_bar = sh_idx[j_idx]
            right_rim = swing_highs.iloc[right_rim_bar]

            # Rims should be approximately equal
            if abs(left_rim - right_rim) / max(left_rim, 1e-9) > tolerance:
                continue

            # Find the deepest swing low between the rims (cup bottom)
            cup_lows = [s for s in sl_idx if left_rim_bar < s < right_rim_bar]
            if not cup_lows:
                continue
            cup_bottom_bar = min(cup_lows, key=lambda s: swing_lows.iloc[s])
            cup_bottom = swing_lows.iloc[cup_bottom_bar]

            # Cup should have meaningful depth (at least 10% of rim height)
            rim_avg = (left_rim + right_rim) / 2
            cup_depth = rim_avg - cup_bottom
            if cup_depth / max(rim_avg, 1e-9) < 0.10:
                continue

            # Look for handle: a small pullback after right rim
            handle_end = min(right_rim_bar + 20, len(close))
            if handle_end <= right_rim_bar + 2:
                continue
            handle_segment = close.iloc[right_rim_bar:handle_end]
            handle_low = handle_segment.min()

            # Handle should not drop below half the cup depth
            if (rim_avg - handle_low) > 0.5 * cup_depth:
                continue

            # Breakout above rim
            breakout_level = max(left_rim, right_rim)
            for k in range(right_rim_bar + 2, min(handle_end + 10, len(close))):
                if close.iloc[k] > breakout_level:
                    matches.append(PatternMatch(
                        pattern="cup_and_handle", signal=1,
                        bar_index=k, neckline=breakout_level, confidence=0.8,
                    ))
                    break

            if matches and matches[-1].pattern == "cup_and_handle":
                break  # found one cup & handle from this left rim

    return matches


# ─────────────────────────────────────────────
# Orchestrator
# ─────────────────────────────────────────────

# Detectors that need volume as an extra arg
_VOLUME_DETECTORS = {"bull_flag", "bear_flag"}

_ALL_DETECTORS = {
    "double_top": detect_double_top,
    "double_bottom": detect_double_bottom,
    "triple_top": detect_triple_top,
    "triple_bottom": detect_triple_bottom,
    "head_shoulders": detect_head_and_shoulders,
    "inv_head_shoulders": detect_inverse_head_and_shoulders,
    "bull_flag": detect_bull_flag,
    "bear_flag": detect_bear_flag,
    "ascending_triangle": detect_ascending_triangle,
    "descending_triangle": detect_descending_triangle,
    "symmetrical_triangle": detect_symmetrical_triangle,
    "cup_and_handle": detect_cup_and_handle,
}


def chart_pattern_core(
    df: pd.DataFrame,
    pivot_lookback: int = 5,
    tolerance: float = 0.03,
    vol_multiplier: float = 1.5,
    vol_period: int = 20,
) -> pd.DataFrame:
    """
    Run all pattern detectors on OHLCV data. Produces a 'signal' column:
    1 = bullish pattern confirmed, -1 = bearish, 0 = no pattern.
    """
    result = df.copy()
    result["signal"] = 0
    n = len(result)

    if n < 2 * pivot_lookback + 5:
        result["swing_high"] = np.nan
        result["swing_low"] = np.nan
        return result

    swing_highs, swing_lows = find_swing_points(
        result["high"], result["low"], pivot_lookback
    )

    all_matches = []
    for name, detector in _ALL_DETECTORS.items():
        if name in _VOLUME_DETECTORS:
            matches = detector(
                result["high"], result["low"], result["close"], result["volume"],
                swing_highs, swing_lows, tolerance,
            )
        else:
            matches = detector(
                result["high"], result["low"], result["close"],
                swing_highs, swing_lows, tolerance,
            )
        all_matches.extend(matches)

    # Apply volume confirmation and write signals
    for match in all_matches:
        idx = match.bar_index
        if 0 <= idx < n and volume_confirmed(result["volume"], idx, vol_period, vol_multiplier):
            result.iloc[idx, result.columns.get_loc("signal")] = match.signal

    result["swing_high"] = swing_highs
    result["swing_low"] = swing_lows

    return result
