#!/usr/bin/env python3
"""
Full 15-minute strategy comparison for BTC/USDT on binanceus.
"""

import sys
import os
sys.path.insert(0, os.path.dirname(__file__))

from data_fetcher import fetch_full_history, load_cached_data
from strategies import apply_strategy, list_strategies, STRATEGY_REGISTRY
from backtester import Backtester

SYMBOL = "BTC/USDT"
TIMEFRAME = "15m"
EXCHANGE = "binanceus"
CAPITAL = 1000.0
# Fetch from as early as possible
SINCE = "2020-01-01"

def main():
    # Step 1: Fetch data
    print(f"Fetching {SYMBOL} {TIMEFRAME} data from {EXCHANGE} since {SINCE}...")
    df = fetch_full_history(SYMBOL, TIMEFRAME, since=SINCE, exchange_id=EXCHANGE, store=True)
    
    if df.empty:
        print("ERROR: No data fetched!")
        sys.exit(1)
    
    print(f"\n{'='*80}")
    print(f"  DATA SUMMARY")
    print(f"  Candles: {len(df)}")
    print(f"  From:    {df.index[0]}")
    print(f"  To:      {df.index[-1]}")
    print(f"{'='*80}\n")
    
    # Step 2: Run all strategies
    strategies = list_strategies()
    print(f"Running {len(strategies)} strategies: {strategies}\n")
    
    all_results = []
    for name in strategies:
        strat = STRATEGY_REGISTRY[name]
        params = strat["default_params"]
        print(f"  Running {name}...", end=" ", flush=True)
        try:
            df_signals = apply_strategy(name, df, params)
            bt = Backtester(initial_capital=CAPITAL)
            result = bt.run(df_signals, strategy_name=name, symbol=SYMBOL,
                          timeframe=TIMEFRAME, params=params, save=False)
            all_results.append(result)
            print(f"Return: {result['total_return_pct']:+.2f}%, "
                  f"Sharpe: {result['sharpe_ratio']:.3f}, "
                  f"Trades: {result['total_trades']}")
        except Exception as e:
            print(f"ERROR: {e}")
    
    # Step 3: Rank and display
    print(f"\n{'='*100}")
    print(f"  15-MINUTE STRATEGY COMPARISON — {SYMBOL} on {EXCHANGE}")
    print(f"  Capital: ${CAPITAL:,.0f} | Candles: {len(df)} | Period: {df.index[0].date()} → {df.index[-1].date()}")
    print(f"{'='*100}")
    
    # Sort by Sharpe ratio
    ranked = sorted(all_results, key=lambda x: x.get("sharpe_ratio", 0), reverse=True)
    
    print(f"\n  {'Rank':<5} {'Strategy':<20} {'Return %':>10} {'Sharpe':>8} {'MaxDD %':>10} "
          f"{'WinRate %':>10} {'Trades':>8}")
    print(f"  {'─'*75}")
    
    for i, r in enumerate(ranked, 1):
        marker = " ★" if i <= 3 else ""
        print(f"  {i:<5} {r['strategy_name']:<20} "
              f"{r['total_return_pct']:>+9.2f}% "
              f"{r['sharpe_ratio']:>7.3f} "
              f"{r['max_drawdown_pct']:>+9.2f}% "
              f"{r['win_rate']:>8.1f}% "
              f"{r['total_trades']:>7}{marker}")
    
    print(f"\n  {'='*75}")
    print(f"  TOP 3 STRATEGIES FOR 15-MINUTE TIMEFRAME:")
    print(f"  {'─'*75}")
    for i, r in enumerate(ranked[:3], 1):
        print(f"  #{i}  {r['strategy_name']}")
        print(f"       Return: {r['total_return_pct']:+.2f}% | Sharpe: {r['sharpe_ratio']:.3f} | "
              f"MaxDD: {r['max_drawdown_pct']:.2f}% | Win Rate: {r['win_rate']:.1f}% | "
              f"Trades: {r['total_trades']}")
        print(f"       Final Capital: ${r['final_capital']:,.2f}")
    print(f"  {'='*75}")

if __name__ == "__main__":
    main()
