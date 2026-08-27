from __future__ import annotations
import os
import sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, '..')), os.path.abspath(os.path.join(_THIS_DIR, '..', 'shared_tools')), os.path.abspath(os.path.join(_THIS_DIR, '..', 'shared_strategies', 'open'))):
    if _p not in sys.path:
        sys.path.insert(0, _p)
import numpy as np
import pandas as pd
from regime import composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS
from indicators_core import hurst_exponent, HURST_DFA_MIN_POINTS
CANONICAL_COLUMNS = ['return_eff', 'range_eff', 'efficiency', 'adx']
ENRICHED_EXTRA_COLUMNS = ['funding_rate', 'volume_z', 'htf_range_eff', 'hurst']
ENRICHED_COLUMNS = CANONICAL_COLUMNS + ENRICHED_EXTRA_COLUMNS
CANONICAL_INDICES = (0, 1, 2, 3)
HURST_WINDOW_DEFAULT = HURST_DFA_MIN_POINTS
LIVE_WIRING_DELTA = 'Enriched models decode from ENRICHED_COLUMNS; live check_regime.py builds only CANONICAL_COLUMNS. Live wiring (#1074) must feed funding_rate + volume_z + htf_range_eff + hurst on-cycle in this exact order, or forward_filter_labels reads garbage.'

def _infer_base_delta(index: pd.DatetimeIndex) -> pd.Timedelta:
    if len(index) < 2:
        raise ValueError('need >= 2 bars to infer the base timeframe')
    diffs = pd.Series(index).diff().dropna()
    med = diffs.median()
    if not (pd.notna(med) and med > pd.Timedelta(0)):
        raise ValueError('could not infer a positive base timeframe from the index')
    return med

def _volume_z_column(df: pd.DataFrame, window: int) -> np.ndarray:
    vol = df['volume'].astype(float)
    roll = vol.rolling(window=window, min_periods=window)
    mean = roll.mean()
    std = roll.std(ddof=0)
    z = (vol - mean) / std
    z = z.where(std > 1e-12, 0.0)
    z = z.where(mean.notna(), np.nan)
    return z.to_numpy(dtype=float)

def _htf_range_eff_column(df: pd.DataFrame, period: int, thresholds: dict, htf_multiple: int) -> np.ndarray:
    if htf_multiple < 2:
        raise ValueError('htf_multiple must be >= 2 (a strictly coarser timeframe)')
    if not isinstance(df.index, pd.DatetimeIndex):
        raise TypeError('htf_range_eff needs a DatetimeIndex (timestamped bars)')
    base_delta = _infer_base_delta(df.index)
    htf_delta = base_delta * htf_multiple
    agg = {'open': 'first', 'high': 'max', 'low': 'min', 'close': 'last', 'volume': 'sum'}
    htf = df.resample(htf_delta, closed='left', label='left').agg(agg).dropna(subset=['close'])
    if len(htf) <= period:
        return np.full(len(df), np.nan, dtype=float)
    htf_feat = composite_feature_matrix(htf, period, thresholds)['range_eff']
    right = pd.DataFrame({'ts': htf_feat.index + htf_delta, 'htf_range_eff': htf_feat.to_numpy(dtype=float)}).dropna(subset=['htf_range_eff']).sort_values('ts').reset_index(drop=True)
    if right.empty:
        return np.full(len(df), np.nan, dtype=float)
    left = pd.DataFrame({'ts': pd.DatetimeIndex(df.index)}).reset_index(drop=True)
    merged = pd.merge_asof(left, right, on='ts', direction='backward')
    return merged['htf_range_eff'].to_numpy(dtype=float)

def _hurst_column(df: pd.DataFrame, window: int) -> np.ndarray:
    if window < 2:
        raise ValueError('hurst_window must be >= 2')
    closes = df['close'].astype(float).to_numpy()
    n = len(closes)
    out = np.full(n, np.nan, dtype=float)
    for i in range(window, n):
        seg = pd.Series(closes[i - window:i + 1])
        out[i] = hurst_exponent(seg, min_points=window)
    return out

def enriched_feature_matrix(df: pd.DataFrame, period: int, thresholds: dict | None=None, *, funding: pd.DataFrame | None=None, vol_window: int | None=None, htf_multiple: int=4, hurst_window: int | None=None, columns: list[str] | None=None) -> pd.DataFrame:
    if thresholds is None:
        thresholds = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    if columns is None:
        columns = list(ENRICHED_COLUMNS)
    unknown = [c for c in columns if c not in ENRICHED_COLUMNS]
    if unknown:
        raise ValueError(f'unknown enriched columns {unknown}; known: {ENRICHED_COLUMNS}')
    columns = [c for c in ENRICHED_COLUMNS if c in columns]
    vol_window = int(vol_window) if vol_window else int(period)
    hurst_window = int(hurst_window) if hurst_window else HURST_WINDOW_DEFAULT
    out = pd.DataFrame(index=df.index)
    canon = composite_feature_matrix(df, period, thresholds)
    for col in CANONICAL_COLUMNS:
        out[col] = canon[col].to_numpy(dtype=float)
    if 'funding_rate' in columns:
        from funding_fetcher import attach_funding_column
        out['funding_rate'] = attach_funding_column(df, funding)['funding_rate'].to_numpy(dtype=float)
    if 'volume_z' in columns:
        out['volume_z'] = _volume_z_column(df, vol_window)
    if 'htf_range_eff' in columns:
        out['htf_range_eff'] = _htf_range_eff_column(df, period, thresholds, htf_multiple)
    if 'hurst' in columns:
        out['hurst'] = _hurst_column(df, hurst_window)
    return out[columns]

def canonical_indices_for(columns: list[str]) -> tuple[int, int, int, int]:
    cols = list(columns)
    missing = [c for c in CANONICAL_COLUMNS if c not in cols]
    if missing:
        raise ValueError(f'naming needs all canonical columns; missing {missing}')
    return tuple((cols.index(c) for c in CANONICAL_COLUMNS))

def assert_feature_contract(model: dict, columns: list[str]) -> None:
    fit_cols = list(model.get('features', []))
    if list(columns) != fit_cols:
        raise ValueError(f'feature-order contract violated: model fit on {fit_cols} but decode matrix has {list(columns)} — forward_filter_labels needs identical column order')

def decode_with_model(matrix: pd.DataFrame, model: dict):
    from regime_hmm import forward_filter_labels
    assert_feature_contract(model, list(matrix.columns))
    return forward_filter_labels(matrix.to_numpy(dtype=float), model)
