#!/usr/bin/env python3
"""
Run backtests with multiple strategies across multiple assets and timeframes.
Main entry point for strategy evaluation.
"""

import sys
import argparse
from typing import List, Optional

from data_fetcher import fetch_full_history, load_cached_data
from strategies import apply_strategy, list_strategies, STRATEGY_REGISTRY
from backtester import Backtester, format_results
from optimizer import walk_forward_optimize, DEFAULT_PARAM_RANGES
from reporter import (
    format_single_report, format_comparison_report,
    format_multi_asset_report, format_walk_forward_report,
    generate_full_report,
)


DEFAULT_SYMBOLS = ["BTC/USDT", "ETH/USDT", "SOL/USDT", "BNB/USDT"]
DEFAULT_TIMEFRAMES = ["4h", "1d"]


def run_single_backtest(
    strategy_name: str = "sma_crossover",
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    params: dict = None,
) -> Optional[dict]:
    """Run a single backtest and print results."""
    strat = STRATEGY_REGISTRY.get(strategy_name)
    if not strat:
        print(f"Unknown strategy: {strategy_name}")
        print(f"Available: {list_strategies()}")
        return None

    strat_params = params or strat["default_params"]
    print(f"\n▶ Strategy: {strat['description']}")
    print(f"  Params: {strat_params}")
    print(f"  Symbol: {symbol} | Timeframe: {timeframe} | Since: {since}")

    # Fetch data
    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None

    print(f"  Data: {len(df)} candles from {df.index[0]} to {df.index[-1]}")

    # Apply strategy
    df_signals = apply_strategy(strategy_name, df, strat_params)

    # Run backtest
    bt = Backtester(initial_capital=capital)
    results = bt.run(
        df_signals,
        strategy_name=strategy_name,
        symbol=symbol,
        timeframe=timeframe,
        params=strat_params,
    )

    print(format_single_report(results))
    return results


def run_all_strategies(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    strategies: Optional[List[str]] = None,
) -> list:
    """Run multiple strategies on one asset and compare."""
    strat_list = strategies or list_strategies()
    print(f"\n{'#'*60}")
    print(f"  RUNNING {len(strat_list)} STRATEGIES")
    print(f"  {symbol} | {timeframe} | since {since} | ${capital:,.0f}")
    print(f"{'#'*60}")

    all_results = []
    for name in strat_list:
        result = run_single_backtest(name, symbol, timeframe, since, capital)
        if result:
            all_results.append(result)

    if all_results:
        print(format_comparison_report(all_results))

    return all_results


def run_multi_asset(
    strategies: Optional[List[str]] = None,
    symbols: Optional[List[str]] = None,
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
) -> dict:
    """Run strategies across multiple assets."""
    strat_list = strategies or list_strategies()
    sym_list = symbols or DEFAULT_SYMBOLS

    print(f"\n{'#'*60}")
    print(f"  MULTI-ASSET BACKTEST")
    print(f"  Strategies: {len(strat_list)} | Assets: {len(sym_list)}")
    print(f"  Timeframe: {timeframe} | Since: {since}")
    print(f"{'#'*60}")

    results_by_asset = {}
    for symbol in sym_list:
        print(f"\n{'─'*40}")
        print(f"  Asset: {symbol}")
        print(f"{'─'*40}")
        results_by_asset[symbol] = []
        for strat_name in strat_list:
            result = run_single_backtest(strat_name, symbol, timeframe, since, capital)
            if result:
                results_by_asset[symbol].append(result)

    print(format_multi_asset_report(results_by_asset))
    return results_by_asset


def run_walk_forward(
    strategy_name: str,
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2020-01-01",
    n_splits: int = 5,
    capital: float = 1000.0,
) -> Optional[dict]:
    """Run walk-forward optimization for a strategy."""
    param_ranges = DEFAULT_PARAM_RANGES.get(strategy_name)
    if not param_ranges:
        print(f"No default param ranges for {strategy_name}")
        return None

    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None

    result = walk_forward_optimize(
        df, strategy_name, param_ranges,
        n_splits=n_splits,
        initial_capital=capital,
        symbol=symbol,
        timeframe=timeframe,
        verbose=True,
    )

    print(format_walk_forward_report(result))
    return result


def main():
    parser = argparse.ArgumentParser(description="Crypto Trading Bot — Backtester")
    parser.add_argument("--strategy", "-s", default="all",
                        help=f"Strategy name or 'all'. Available: {list_strategies()}")
    parser.add_argument("--symbol", default="BTC/USDT",
                        help="Trading pair")
    parser.add_argument("--symbols", nargs="+", default=None,
                        help="Multiple trading pairs for multi-asset mode")
    parser.add_argument("--timeframe", "-tf", default="1d",
                        help="Candle timeframe (1h, 4h, 1d)")
    parser.add_argument("--since", default="2022-01-01",
                        help="Start date")
    parser.add_argument("--capital", type=float, default=1000.0,
                        help="Starting capital")
    parser.add_argument("--mode", choices=["single", "compare", "multi", "optimize"],
                        default="compare",
                        help="Run mode: single/compare/multi/optimize")
    parser.add_argument("--splits", type=int, default=5,
                        help="Walk-forward splits (optimize mode)")
    args = parser.parse_args()

    if args.mode == "single":
        if args.strategy == "all":
            print("Specify a strategy for single mode: --strategy <name>")
            sys.exit(1)
        run_single_backtest(args.strategy, args.symbol, args.timeframe, args.since, args.capital)

    elif args.mode == "compare":
        strategies = None if args.strategy == "all" else [args.strategy]
        run_all_strategies(args.symbol, args.timeframe, args.since, args.capital, strategies)

    elif args.mode == "multi":
        strategies = None if args.strategy == "all" else [args.strategy]
        symbols = args.symbols or DEFAULT_SYMBOLS
        run_multi_asset(strategies, symbols, args.timeframe, args.since, args.capital)

    elif args.mode == "optimize":
        if args.strategy == "all":
            # Optimize all strategies
            for strat in list_strategies():
                if strat in DEFAULT_PARAM_RANGES:
                    run_walk_forward(strat, args.symbol, args.timeframe, args.since, args.splits, args.capital)
        else:
            run_walk_forward(args.strategy, args.symbol, args.timeframe, args.since, args.splits, args.capital)


if __name__ == "__main__":
    main()
