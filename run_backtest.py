#!/usr/bin/env python3
"""
Run backtests with different strategies and parameters.
Main entry point for Phase 1 trading bot.
"""

import sys
import argparse

from data_fetcher import fetch_full_history, load_cached_data
from indicators import sma_crossover, rsi, bollinger_bands
from backtester import Backtester, format_results


STRATEGIES = {
    "sma_crossover": {
        "fn": sma_crossover,
        "default_params": {"fast_period": 20, "slow_period": 50},
        "description": "SMA Crossover — buy when fast SMA crosses above slow SMA",
    },
    "rsi": {
        "fn": rsi,
        "default_params": {"period": 14, "overbought": 70, "oversold": 30},
        "description": "RSI — buy at oversold, sell at overbought",
    },
    "bollinger_bands": {
        "fn": bollinger_bands,
        "default_params": {"period": 20, "num_std": 2.0},
        "description": "Bollinger Bands — mean reversion at band touches",
    },
}


def run_single_backtest(
    strategy_name: str = "sma_crossover",
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    params: dict = None,
):
    """Run a single backtest and print results."""
    if strategy_name not in STRATEGIES:
        print(f"Unknown strategy: {strategy_name}")
        print(f"Available: {', '.join(STRATEGIES.keys())}")
        return None

    strategy = STRATEGIES[strategy_name]
    strat_params = params or strategy["default_params"]

    print(f"\n▶ Strategy: {strategy['description']}")
    print(f"  Params: {strat_params}")
    print(f"  Symbol: {symbol} | Timeframe: {timeframe} | Since: {since}")
    print(f"  Capital: ${capital:,.2f}")

    # Fetch data
    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None

    print(f"  Data: {len(df)} candles from {df.index[0]} to {df.index[-1]}")

    # Apply indicator/strategy
    df_signals = strategy["fn"](df, **strat_params)

    # Run backtest
    bt = Backtester(initial_capital=capital)
    results = bt.run(
        df_signals,
        strategy_name=strategy_name,
        symbol=symbol,
        timeframe=timeframe,
        params=strat_params,
    )

    print(format_results(results))

    # Print individual trades
    if results["trades"]:
        print(f"\n  TRADE LOG ({len(results['trades'])} trades):")
        print(f"  {'Entry Date':<22} {'Exit Date':<22} {'Entry $':>10} {'Exit $':>10} {'PnL %':>8}")
        print(f"  {'─'*74}")
        for t in results["trades"]:
            print(f"  {t['entry_date'][:19]:<22} {t['exit_date'][:19]:<22} "
                  f"{t['entry_price']:>10,.2f} {t['exit_price']:>10,.2f} {t['pnl_pct']:>+7.2f}%")

    return results


def run_all_strategies(symbol="BTC/USDT", timeframe="1d", since="2022-01-01", capital=1000.0):
    """Run all available strategies and compare."""
    print(f"\n{'#'*60}")
    print(f"  RUNNING ALL STRATEGIES")
    print(f"  {symbol} | {timeframe} | since {since} | ${capital:,.0f}")
    print(f"{'#'*60}")

    all_results = []
    for name in STRATEGIES:
        result = run_single_backtest(name, symbol, timeframe, since, capital)
        if result:
            all_results.append(result)

    if all_results:
        print(f"\n\n{'='*60}")
        print(f"  STRATEGY COMPARISON")
        print(f"{'='*60}")
        print(f"  {'Strategy':<20} {'Return':>8} {'Sharpe':>8} {'MaxDD':>8} {'WinRate':>8} {'Trades':>7}")
        print(f"  {'─'*60}")
        for r in sorted(all_results, key=lambda x: x["total_return_pct"], reverse=True):
            print(f"  {r['strategy_name']:<20} {r['total_return_pct']:>+7.1f}% "
                  f"{r['sharpe_ratio']:>7.2f} {r['max_drawdown_pct']:>+7.1f}% "
                  f"{r['win_rate']:>6.1f}% {r['total_trades']:>6}")

    return all_results


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Crypto Trading Bot - Backtester")
    parser.add_argument("--strategy", "-s", default="all", choices=list(STRATEGIES.keys()) + ["all"])
    parser.add_argument("--symbol", default="BTC/USDT")
    parser.add_argument("--timeframe", "-tf", default="1d")
    parser.add_argument("--since", default="2022-01-01")
    parser.add_argument("--capital", type=float, default=1000.0)
    args = parser.parse_args()

    if args.strategy == "all":
        run_all_strategies(args.symbol, args.timeframe, args.since, args.capital)
    else:
        run_single_backtest(args.strategy, args.symbol, args.timeframe, args.since, args.capital)
