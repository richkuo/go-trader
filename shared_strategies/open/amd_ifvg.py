import numpy as np
import pandas as pd

def _session_local(index: pd.DatetimeIndex, session_tz: str) -> pd.DatetimeIndex:
    if index.tz is None:
        local = index.tz_localize('UTC').tz_convert(session_tz)
    else:
        local = index.tz_convert(session_tz)
    return local.tz_localize(None)

def _hours_in_window(hours: np.ndarray, start_hour: int, end_hour: int) -> np.ndarray:
    end_eff = 24 if end_hour == 0 else end_hour
    start_n = start_hour % 24
    if start_n < end_eff:
        return (hours >= start_n) & (hours < end_eff)
    return (hours >= start_n) | (hours < end_eff % 24)

def amd_ifvg_core(df: pd.DataFrame, asian_start_hour: int=20, asian_end_hour: int=0, london_start_hour: int=2, london_end_hour: int=5, min_ifvg_pct: float=0.05, sweep_threshold_pct: float=0.01, session_tz: str='America/New_York') -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result['signal'] = 0
    result['asian_high'] = np.nan
    result['asian_low'] = np.nan
    result['ifvg_high'] = np.nan
    result['ifvg_low'] = np.nan
    result['sweep_dir'] = 0
    if n < 3:
        return result
    local = _session_local(result.index, session_tz)
    hours = np.asarray(local.hour)
    session_day = np.asarray((local - pd.Timedelta(hours=asian_start_hour % 24)).floor('D'))
    asian_hour_mask = _hours_in_window(hours, asian_start_hour, asian_end_hour)
    london_hour_mask = _hours_in_window(hours, london_start_hour, london_end_hour)
    for day in pd.unique(session_day):
        day_mask = session_day == day
        asian_mask = day_mask & asian_hour_mask
        asian_candles = result.loc[asian_mask]
        if len(asian_candles) < 2:
            continue
        asian_high = asian_candles['high'].max()
        asian_low = asian_candles['low'].min()
        asian_range = asian_high - asian_low
        if asian_range <= 0:
            continue
        result.loc[day_mask, 'asian_high'] = asian_high
        result.loc[day_mask, 'asian_low'] = asian_low
        london_mask = day_mask & london_hour_mask
        london_candles = result.loc[london_mask]
        if len(london_candles) < 3:
            continue
        sweep_threshold = asian_range * sweep_threshold_pct
        swept_below_idx = None
        swept_above_idx = None
        for idx in london_candles.index:
            row = london_candles.loc[idx]
            if swept_below_idx is None and row['low'] < asian_low - sweep_threshold:
                swept_below_idx = idx
            if swept_above_idx is None and row['high'] > asian_high + sweep_threshold:
                swept_above_idx = idx
        if swept_below_idx is not None and swept_above_idx is not None:
            if swept_below_idx <= swept_above_idx:
                bias = -1
            else:
                bias = 1
        elif swept_below_idx is not None:
            bias = -1
        elif swept_above_idx is not None:
            bias = 1
        else:
            continue
        result.loc[london_mask, 'sweep_dir'] = bias
        sweep_idx = swept_below_idx if bias == -1 else swept_above_idx
        post_sweep_mask = day_mask & (result.index >= sweep_idx)
        post_sweep = result.loc[post_sweep_mask]
        if len(post_sweep) < 3:
            continue
        ps_indices = post_sweep.index.tolist()
        ifvg_candidates = []
        for i in range(2, len(ps_indices)):
            c0 = post_sweep.loc[ps_indices[i - 2]]
            c2 = post_sweep.loc[ps_indices[i]]
            if bias == -1:
                if c0['high'] >= c2['low']:
                    continue
                gap_low, gap_high = (c0['high'], c2['low'])
            else:
                if c0['low'] <= c2['high']:
                    continue
                gap_low, gap_high = (c2['high'], c0['low'])
            gap_size = gap_high - gap_low
            mid_price = (gap_high + gap_low) / 2
            if mid_price <= 0:
                continue
            if gap_size / mid_price * 100 < min_ifvg_pct:
                continue
            ifvg_candidates.append((gap_low, gap_high, i))
        if not ifvg_candidates:
            continue
        signal_fired = False
        chosen_ifvg = None
        chosen_entry_idx = None
        for k in range(1, len(ps_indices)):
            available = [c for c in ifvg_candidates if c[2] < k]
            if not available:
                continue
            bar_idx = ps_indices[k]
            bar_close = result.loc[bar_idx, 'close']
            bar_low = result.loc[bar_idx, 'low']
            bar_high = result.loc[bar_idx, 'high']
            best = min(available, key=lambda c: abs(bar_close - (c[0] + c[1]) / 2))
            ifvg_low, ifvg_high = (best[0], best[1])
            if bias == -1:
                if bar_low <= ifvg_high and bar_close >= ifvg_low:
                    result.loc[bar_idx, 'signal'] = 1
                    signal_fired = True
            elif bar_high >= ifvg_low and bar_close <= ifvg_high:
                result.loc[bar_idx, 'signal'] = -1
                signal_fired = True
            if signal_fired:
                chosen_ifvg = (ifvg_low, ifvg_high)
                chosen_entry_idx = bar_idx
                break
        if chosen_ifvg is not None and chosen_entry_idx is not None:
            viz_mask = day_mask & (result.index >= chosen_entry_idx)
            result.loc[viz_mask, 'ifvg_low'] = chosen_ifvg[0]
            result.loc[viz_mask, 'ifvg_high'] = chosen_ifvg[1]
    return result
