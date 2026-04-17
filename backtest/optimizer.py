"""
Walk-forward optimization framework to prevent overfitting.
Splits data into rolling in-sample/out-of-sample windows,
optimizes parameters on in-sample, validates on out-of-sample.
"""

import sys
import os
import itertools
from typing import Dict, List, Optional, Any, Tuple
from datetime import timedelta

import numpy as np
import pandas as pd

from registry_loader import load_registry
from backtester import Backtester


_EXPECTED_FOLD_ERRORS = (KeyError, ValueError, TypeError, IndexError, ZeroDivisionError)


def generate_param_grid(param_ranges: Dict[str, list]) -> List[dict]:
    """Generate all parameter combinations from ranges."""
    keys = list(param_ranges.keys())
    values = list(param_ranges.values())
    combos = list(itertools.product(*values))
    return [dict(zip(keys, combo)) for combo in combos]


def warmup_exit_long_entry(warmup_with_signal: pd.DataFrame,
                             slippage_pct: float) -> Optional[dict]:
    """Walk through warmup signals to find whether the strategy ends the
    warmup period already long, and if so at what effective entry price.

    Mirrors Backtester's execution model: ``signal`` is shifted by one bar
    (signal at bar t fills at bar t+1's open), slippage is added on entry.

    Returns ``{"entry_price": float, "entry_date": idx}`` when the warmup
    ends long, or ``None`` when flat. The caller passes the dict to
    ``Backtester.run(starting_long=...)`` so a SELL in the train window
    actually closes the warmup position rather than being silently dropped.
    """
    if len(warmup_with_signal) == 0 or "signal" not in warmup_with_signal.columns:
        return None

    shifted = warmup_with_signal.copy()
    shifted["signal"] = shifted["signal"].shift(1).fillna(0)
    has_open = "open" in shifted.columns

    in_position = False
    entry_price = None
    entry_date = None
    for idx, row in shifted.iterrows():
        fill_price = row["open"] if has_open else row["close"]
        sig = row["signal"]
        if sig == 1 and not in_position:
            entry_price = fill_price * (1 + slippage_pct)
            entry_date = idx
            in_position = True
        elif sig == -1 and in_position:
            in_position = False
            entry_price = None
            entry_date = None

    if in_position and entry_price is not None:
        return {"entry_price": entry_price, "entry_date": entry_date}
    return None


def max_indicator_lookback(param_ranges: Dict[str, list]) -> int:
    """Heuristic warmup size for walk-forward folds.

    Scans the integer values in the parameter grid and returns the largest.
    Period/lookback-style params are ints (``fast_period``, ``slow_period``,
    ``atr_period``, ``swing_lookback``, etc.) and dominate warmup cost;
    non-integer params (multipliers, thresholds) are ignored since they
    don't add history requirements.

    Returns 0 if no integer params found — matches the pre-warmup behavior
    for grids like ``vwap_reversion`` that only tune std-dev thresholds.
    """
    max_lb = 0
    for values in param_ranges.values():
        for v in values:
            if isinstance(v, int) and v > max_lb:
                max_lb = v
    return max_lb


def walk_forward_optimize(
    df: pd.DataFrame,
    strategy_name: str,
    param_ranges: Dict[str, list],
    n_splits: int = 5,
    train_pct: float = 0.7,
    optimize_metric: str = "sharpe_ratio",
    initial_capital: float = 1000.0,
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    registry: str = "spot",
    platform: str = "binanceus",
    verbose: bool = True,
) -> dict:
    """
    Walk-forward optimization.

    1. Split data into n_splits rolling windows
    2. For each window: optimize on train portion, validate on test portion
    3. Report aggregated out-of-sample performance

    Args:
        df: OHLCV DataFrame with datetime index
        strategy_name: Name of registered strategy
        param_ranges: Dict of param_name -> [values to test]
        n_splits: Number of walk-forward windows
        train_pct: Fraction of each window used for training
        optimize_metric: Metric to maximize during optimization
        initial_capital: Starting capital per window
        symbol: Trading pair
        timeframe: Candle timeframe

    Returns:
        Dict with optimization results and best parameters per window
    """
    total_len = len(df)
    window_size = total_len // n_splits
    if window_size < 50:
        raise ValueError(f"Not enough data: {total_len} rows / {n_splits} splits = {window_size} rows per window. Need >= 50.")

    reg = load_registry(registry)
    apply_strategy = reg.apply_strategy

    param_grid = generate_param_grid(param_ranges)
    warmup = max_indicator_lookback(param_ranges)
    if verbose:
        print(f"\nWalk-Forward Optimization: {strategy_name}")
        print(f"  Data: {len(df)} candles | Splits: {n_splits} | Train: {train_pct:.0%}")
        print(f"  Parameter combinations: {len(param_grid)}")
        print(f"  Warmup bars per fold: {warmup}")
        print(f"  Optimizing: {optimize_metric}")

    bt = Backtester(initial_capital=initial_capital, platform=platform)
    window_results = []

    for fold in range(n_splits):
        start_idx = fold * window_size
        end_idx = min(start_idx + window_size, total_len) if fold < n_splits - 1 else total_len
        window_df = df.iloc[start_idx:end_idx]

        train_size = int(len(window_df) * train_pct)
        train_df = window_df.iloc[:train_size]
        test_df = window_df.iloc[train_size:]

        if len(train_df) < 30 or len(test_df) < 10:
            continue

        # Pre-roll indicator state with ``warmup`` bars of preceding history
        # so long-lookback indicators prime before the first signal bar.
        train_boundary = start_idx + train_size
        train_start_ext = max(0, start_idx - warmup)
        test_start_ext = max(0, train_boundary - warmup)
        train_ext_df = df.iloc[train_start_ext:train_boundary]
        test_ext_df = df.iloc[test_start_ext:end_idx]
        train_trim = start_idx - train_start_ext
        test_trim = train_boundary - test_start_ext

        if verbose:
            print(f"\n  Fold {fold+1}/{n_splits}: "
                  f"Train {train_df.index[0].strftime('%Y-%m-%d')}→{train_df.index[-1].strftime('%Y-%m-%d')} "
                  f"| Test {test_df.index[0].strftime('%Y-%m-%d')}→{test_df.index[-1].strftime('%Y-%m-%d')}")

        # Optimize on training data
        best_metric = -np.inf
        best_params = None
        for params in param_grid:
            try:
                signals_ext = apply_strategy(strategy_name, train_ext_df, params)
                signals_df = signals_ext.iloc[train_trim:]
                # Carry warmup position state so SELL signals in the first
                # train bars close a real warmup entry instead of being
                # dropped as "sell while flat".
                train_seed = warmup_exit_long_entry(
                    signals_ext.iloc[:train_trim], bt.slippage_pct,
                ) if train_trim else None
                result = bt.run(signals_df, strategy_name=strategy_name,
                              symbol=symbol, timeframe=timeframe,
                              params=params, save=False,
                              starting_long=train_seed)
                metric_val = result.get(optimize_metric, 0)
                if isinstance(metric_val, (int, float)) and metric_val > best_metric:
                    best_metric = metric_val
                    best_params = params
            except _EXPECTED_FOLD_ERRORS as e:
                if verbose:
                    print(f"    [skip] fold {fold+1} {strategy_name} {params}: {type(e).__name__}: {e}")
                continue

        if best_params is None:
            continue

        # Validate on test data with best params
        try:
            test_signals_ext = apply_strategy(strategy_name, test_ext_df, best_params)
            test_signals = test_signals_ext.iloc[test_trim:]
            test_seed = warmup_exit_long_entry(
                test_signals_ext.iloc[:test_trim], bt.slippage_pct,
            ) if test_trim else None
            test_result = bt.run(test_signals, strategy_name=strategy_name,
                               symbol=symbol, timeframe=timeframe,
                               params=best_params, save=False,
                               starting_long=test_seed)
        except _EXPECTED_FOLD_ERRORS as e:
            if verbose:
                print(f"    [skip] fold {fold+1} validation {strategy_name}: {type(e).__name__}: {e}")
            continue

        window_results.append({
            "fold": fold + 1,
            "best_params": best_params,
            "train_metric": best_metric,
            "test_result": test_result,
            "train_period": f"{train_df.index[0].strftime('%Y-%m-%d')} to {train_df.index[-1].strftime('%Y-%m-%d')}",
            "test_period": f"{test_df.index[0].strftime('%Y-%m-%d')} to {test_df.index[-1].strftime('%Y-%m-%d')}",
        })

        if verbose:
            print(f"    Best params: {best_params}")
            print(f"    Train {optimize_metric}: {best_metric:.3f}")
            print(f"    Test return: {test_result['total_return_pct']:+.2f}% | "
                  f"Sharpe: {test_result['sharpe_ratio']:.3f} | "
                  f"MaxDD: {test_result['max_drawdown_pct']:.2f}%")

    if not window_results:
        return {"error": "No valid optimization windows", "strategy": strategy_name}

    # Aggregate out-of-sample results
    oos_returns = [w["test_result"]["total_return_pct"] for w in window_results]
    oos_sharpes = [w["test_result"]["sharpe_ratio"] for w in window_results]
    oos_drawdowns = [w["test_result"]["max_drawdown_pct"] for w in window_results]

    # Find most common best params
    all_params = [str(w["best_params"]) for w in window_results]
    from collections import Counter
    most_common_params = Counter(all_params).most_common(1)[0][0]

    summary = {
        "strategy": strategy_name,
        "n_splits": n_splits,
        "n_valid_folds": len(window_results),
        "param_grid_size": len(param_grid),
        "optimize_metric": optimize_metric,
        "oos_mean_return": round(np.mean(oos_returns), 2),
        "oos_median_return": round(np.median(oos_returns), 2),
        "oos_std_return": round(np.std(oos_returns), 2),
        "oos_mean_sharpe": round(np.mean(oos_sharpes), 3),
        "oos_mean_drawdown": round(np.mean(oos_drawdowns), 2),
        "oos_worst_drawdown": round(min(oos_drawdowns), 2),
        "most_common_best_params": most_common_params,
        "window_results": window_results,
    }

    if verbose:
        print(f"\n{'='*60}")
        print(f"  WALK-FORWARD SUMMARY: {strategy_name}")
        print(f"{'='*60}")
        print(f"  Valid folds: {summary['n_valid_folds']}/{n_splits}")
        print(f"  OOS Mean Return: {summary['oos_mean_return']:+.2f}%")
        print(f"  OOS Median Return: {summary['oos_median_return']:+.2f}%")
        print(f"  OOS Return StdDev: {summary['oos_std_return']:.2f}%")
        print(f"  OOS Mean Sharpe: {summary['oos_mean_sharpe']:.3f}")
        print(f"  OOS Mean MaxDD: {summary['oos_mean_drawdown']:.2f}%")
        print(f"  OOS Worst MaxDD: {summary['oos_worst_drawdown']:.2f}%")
        print(f"  Most Stable Params: {summary['most_common_best_params']}")

    return summary


# Predefined parameter ranges for optimization.
# Grids are kept small (≤ ~32 combinations) to bound walk-forward runtime;
# each strategy registered in shared_strategies/{spot,futures}/strategies.py
# should have an entry here. Missing entries fall back to the strategy's
# default_params (single-point grid) so optimize mode still runs.
DEFAULT_PARAM_RANGES = {
    "sma_crossover": {
        "fast_period": [10, 15, 20, 25],
        "slow_period": [40, 50, 60, 80],
    },
    "ema_crossover": {
        "fast_period": [8, 12, 16, 20],
        "slow_period": [21, 26, 34, 50],
    },
    "rsi": {
        "period": [10, 14, 21],
        "overbought": [65, 70, 75],
        "oversold": [25, 30, 35],
    },
    "bollinger_bands": {
        "period": [15, 20, 25, 30],
        "num_std": [1.5, 2.0, 2.5],
    },
    "macd": {
        "fast_period": [8, 12, 16],
        "slow_period": [21, 26, 34],
        "signal_period": [7, 9, 12],
    },
    "mean_reversion": {
        "lookback": [20, 30, 40, 50],
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std": [0.0, 0.5, 1.0],
    },
    "momentum": {
        "roc_period": [10, 14, 21, 30],
        "threshold": [3.0, 5.0, 7.0, 10.0],
    },
    "volume_weighted": {
        "sma_period": [15, 20, 30],
        "vol_multiplier": [1.2, 1.5, 2.0],
    },
    "triple_ema": {
        "short_period": [5, 8, 13],
        "mid_period": [15, 21, 30],
        "long_period": [40, 55, 80],
    },
    "rsi_macd_combo": {
        "rsi_period": [10, 14],
        "rsi_oversold": [30, 35, 40],
        "rsi_overbought": [60, 65, 70],
        "macd_fast": [8, 12],
        "macd_slow": [21, 26],
        "macd_signal": [7, 9],
    },
    "stoch_rsi": {
        "rsi_period": [10, 14, 21],
        "stoch_period": [10, 14, 21],
        "overbought": [75, 80, 85],
        "oversold": [15, 20, 25],
    },
    "supertrend": {
        "atr_period": [7, 10, 14],
        "multiplier": [2.0, 3.0, 4.0],
    },
    "ichimoku_cloud": {
        "tenkan_period": [7, 9, 11],
        "kijun_period": [22, 26, 30],
        "senkou_b_period": [44, 52, 60],
    },
    "pairs_spread": {
        "lookback": [20, 30, 40, 50],
        "entry_z": [1.5, 2.0, 2.5],
        "exit_z": [0.0, 0.5, 1.0],
    },
    "squeeze_momentum": {
        "bb_period": [15, 20, 25],
        "kc_period": [15, 20, 25],
        "kc_mult": [1.0, 1.5, 2.0],
        "mom_lookback": [8, 12, 16],
    },
    "atr_breakout": {
        "atr_period": [10, 14, 20],
        "multiplier": [1.0, 1.5, 2.0],
    },
    "amd_ifvg": {
        "min_ifvg_pct": [0.03, 0.05, 0.1],
        "sweep_threshold_pct": [0.005, 0.01, 0.02],
    },
    "heikin_ashi_ema": {
        "ema_period": [13, 21, 34],
        "confirmation": [1, 2, 3],
    },
    "order_blocks": {
        "atr_period": [10, 14, 20],
        "displacement_mult": [1.0, 1.5, 2.0],
        "ob_lookback": [15, 20, 30],
        "max_ob_age": [30, 50, 80],
    },
    "vwap_reversion": {
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std": [0.0, 0.2, 0.5],
    },
    "chart_pattern": {
        "pivot_lookback": [3, 5, 7],
        "tolerance": [0.02, 0.03, 0.05],
        "vol_multiplier": [1.2, 1.5, 2.0],
    },
    "liquidity_sweeps": {
        "swing_lookback": [10, 20, 30],
        "confirmation": [1, 2, 3],
    },
    "parabolic_sar": {
        "iaf": [0.01, 0.02, 0.03],
        "af_step": [0.01, 0.02, 0.03],
        "max_af": [0.1, 0.2, 0.3],
    },
    "range_scalper": {
        "bb_period": [10, 14, 20],
        "bw_threshold": [0.005, 0.008, 0.012],
        "rsi_period": [5, 7, 10],
    },
    "sweep_squeeze_combo": {
        "swing_lookback": [5, 10, 15],
        "min_agree": [2, 3],
    },
    "adx_trend": {
        "adx_period": [10, 14, 20],
        "adx_threshold": [20, 25, 30],
    },
    "donchian_breakout": {
        "entry_period": [10, 20, 30],
        "exit_period": [5, 10, 15],
    },
    # Futures-only
    "breakout": {
        "lookback": [10, 20, 30],
        "atr_period": [10, 14, 20],
        "atr_multiplier": [1.0, 1.5, 2.0],
    },
    "delta_neutral_funding": {
        "entry_threshold": [0.00005, 0.0001, 0.00015],
        "exit_threshold": [0.0, 0.00002, 0.00005],
        "drift_threshold": [1.5, 2.0, 2.5],
    },
}


if __name__ == "__main__":
    # Test with synthetic data
    np.random.seed(42)
    dates = pd.date_range("2020-01-01", periods=500, freq="D")
    prices = 100 + np.cumsum(np.random.randn(500) * 2)
    df = pd.DataFrame({
        "open": prices,
        "high": prices + abs(np.random.randn(500)),
        "low": prices - abs(np.random.randn(500)),
        "close": prices + np.random.randn(500) * 0.5,
        "volume": np.random.randint(1000, 10000, 500).astype(float),
    }, index=dates)

    result = walk_forward_optimize(
        df, "sma_crossover",
        {"fast_period": [10, 20], "slow_period": [40, 50]},
        n_splits=3, verbose=True
    )
