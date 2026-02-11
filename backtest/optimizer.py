"""
Walk-forward optimization framework to prevent overfitting.
Splits data into rolling in-sample/out-of-sample windows,
optimizes parameters on in-sample, validates on out-of-sample.
"""

import itertools
from typing import Dict, List, Optional, Any, Tuple
from datetime import timedelta

import numpy as np
import pandas as pd

from strategies import apply_strategy, get_strategy
from backtester import Backtester


def generate_param_grid(param_ranges: Dict[str, list]) -> List[dict]:
    """Generate all parameter combinations from ranges."""
    keys = list(param_ranges.keys())
    values = list(param_ranges.values())
    combos = list(itertools.product(*values))
    return [dict(zip(keys, combo)) for combo in combos]


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

    param_grid = generate_param_grid(param_ranges)
    if verbose:
        print(f"\nWalk-Forward Optimization: {strategy_name}")
        print(f"  Data: {len(df)} candles | Splits: {n_splits} | Train: {train_pct:.0%}")
        print(f"  Parameter combinations: {len(param_grid)}")
        print(f"  Optimizing: {optimize_metric}")

    bt = Backtester(initial_capital=initial_capital)
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

        if verbose:
            print(f"\n  Fold {fold+1}/{n_splits}: "
                  f"Train {train_df.index[0].strftime('%Y-%m-%d')}→{train_df.index[-1].strftime('%Y-%m-%d')} "
                  f"| Test {test_df.index[0].strftime('%Y-%m-%d')}→{test_df.index[-1].strftime('%Y-%m-%d')}")

        # Optimize on training data
        best_metric = -np.inf
        best_params = None
        for params in param_grid:
            try:
                signals_df = apply_strategy(strategy_name, train_df, params)
                result = bt.run(signals_df, strategy_name=strategy_name,
                              symbol=symbol, timeframe=timeframe,
                              params=params, save=False)
                metric_val = result.get(optimize_metric, 0)
                if isinstance(metric_val, (int, float)) and metric_val > best_metric:
                    best_metric = metric_val
                    best_params = params
            except Exception:
                continue

        if best_params is None:
            continue

        # Validate on test data with best params
        try:
            test_signals = apply_strategy(strategy_name, test_df, best_params)
            test_result = bt.run(test_signals, strategy_name=strategy_name,
                               symbol=symbol, timeframe=timeframe,
                               params=best_params, save=False)
        except Exception:
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


# Predefined parameter ranges for optimization
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
