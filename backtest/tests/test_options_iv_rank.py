import math
import os
import sys
import numpy as np
import pytest
from backtest_options import calc_iv_rank
RECENT = 14
LOOKBACK = 60

def _path_from_vol_schedule(vol_per_day: list, seed: int=0) -> list:
    rng = np.random.default_rng(seed)
    closes = [100.0]
    for sigma in vol_per_day:
        r = rng.normal(loc=0.0, scale=sigma)
        closes.append(closes[-1] * math.exp(r))
    return closes

def test_returns_default_when_history_too_short():
    closes = [100.0 + i for i in range(50)]
    assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0

def test_boundary_one_below_minimum_returns_default():
    closes = [100.0 + i for i in range(74)]
    assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0

def test_boundary_exact_minimum_computes_rank():
    closes = _path_from_vol_schedule([0.02] * 74, seed=11)
    assert len(closes) == 75
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert 0.0 <= rank <= 100.0
    assert rank != 50.0

def test_flat_lookback_returns_neutral_50():
    closes = [100.0] * (RECENT + LOOKBACK + 5)
    assert calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK) == 50.0

def test_recent_spike_ranks_near_100():
    sched = [0.005] * 76 + [0.08] * 14
    closes = _path_from_vol_schedule(sched, seed=1)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert rank > 90, f'expected near-100, got {rank}'

def test_recent_calm_ranks_near_0():
    sched = [0.08] * 76 + [0.001] * 14
    closes = _path_from_vol_schedule(sched, seed=2)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert rank < 10, f'expected near-0, got {rank}'

def test_rank_is_clamped_to_0_100():
    sched = [0.02] * 76 + [0.04] * 14
    closes = _path_from_vol_schedule(sched, seed=3)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert 0.0 <= rank <= 100.0

def test_middle_of_distribution_ranks_around_50():
    lookback_vols = np.linspace(0.01, 0.05, 76).tolist()
    recent_vols = [0.03] * 14
    closes = _path_from_vol_schedule(lookback_vols + recent_vols, seed=4)
    rank = calc_iv_rank(closes, recent_window=RECENT, lookback_days=LOOKBACK)
    assert 20 < rank < 80, f'expected a middle value, got {rank}'

def _live_okx_iv_rank(closes: list, window: int=14) -> float:
    if len(closes) < window + 1:
        return 50.0
    returns = [math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes))]
    if len(returns) < window:
        return 50.0
    mean = sum(returns[-window:]) / window
    variance = sum(((r - mean) ** 2 for r in returns[-window:])) / window
    current = math.sqrt(variance * 365) * 100
    hvs = []
    for i in range(len(returns) - window + 1):
        chunk = returns[i:i + window]
        m = sum(chunk) / window
        v = sum(((r - m) ** 2 for r in chunk)) / window
        hvs.append(math.sqrt(v * 365) * 100)
    lo, hi = (min(hvs), max(hvs))
    if hi <= lo:
        return 50.0
    rank = (current - lo) / (hi - lo) * 100
    return min(max(rank, 0.0), 100.0)

def test_matches_live_okx_adapter_shape():
    rng = np.random.default_rng(99)
    log_returns = rng.normal(loc=0.0, scale=0.03, size=120)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * math.exp(r))
    total_returns = len(closes) - 1
    lookback = total_returns - RECENT
    got = calc_iv_rank(closes, recent_window=RECENT, lookback_days=lookback)
    expected = _live_okx_iv_rank(closes, window=RECENT)
    assert got == pytest.approx(expected, abs=1e-09), f'backtest calc_iv_rank must produce the same percentile as the live OKX adapter on identical inputs — got {got}, expected {expected}'
