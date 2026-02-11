#!/usr/bin/env python3
"""
Full 1-hour strategy comparison across 4 assets.
Fetches max data from binanceus, runs all 11 strategies, outputs ranked results.
"""

import sys
import os
sys.path.insert(0, os.path.dirname(__file__))

import json
import pandas as pd
import numpy as np
from data_fetcher import fetch_full_history, load_cached_data
from strategies import apply_strategy, list_strategies, STRATEGY_REGISTRY
from backtester import Backtester

SYMBOLS = ["BTC/USDT", "ETH/USDT", "SOL/USDT", "BNB/USDT"]
TIMEFRAME = "1h"
CAPITAL = 1000.0
EXCHANGE = "binanceus"
# Go as far back as possible
SINCE = "2017-01-01"

def fetch_all_data():
    """Fetch 1h data for all symbols, return dict of DataFrames."""
    data = {}
    for symbol in SYMBOLS:
        print(f"\n{'='*60}")
        print(f"Fetching {symbol} {TIMEFRAME} data...")
        print(f"{'='*60}")
        try:
            df = fetch_full_history(symbol, TIMEFRAME, since=SINCE, exchange_id=EXCHANGE, store=True)
            if df.empty:
                print(f"  WARNING: No data for {symbol}")
            else:
                print(f"  Got {len(df)} candles: {df.index[0]} to {df.index[-1]}")
            data[symbol] = df
        except Exception as e:
            print(f"  ERROR fetching {symbol}: {e}")
            # Try loading cached
            try:
                df = load_cached_data(symbol, TIMEFRAME, exchange_id=EXCHANGE, start_date=SINCE)
                if not df.empty:
                    print(f"  Loaded {len(df)} cached candles")
                    data[symbol] = df
                else:
                    data[symbol] = pd.DataFrame()
            except:
                data[symbol] = pd.DataFrame()
    return data

def run_comparison(data):
    """Run all strategies on all assets, return structured results."""
    strategies = list_strategies()
    print(f"\nStrategies to test ({len(strategies)}): {strategies}")
    
    all_results = []
    results_by_asset = {}
    
    for symbol in SYMBOLS:
        df = data.get(symbol)
        if df is None or df.empty:
            print(f"\nSkipping {symbol} — no data")
            continue
        
        print(f"\n{'#'*70}")
        print(f"  {symbol} — {len(df)} candles")
        print(f"  {df.index[0]} to {df.index[-1]}")
        print(f"{'#'*70}")
        
        results_by_asset[symbol] = []
        
        for strat_name in strategies:
            strat = STRATEGY_REGISTRY[strat_name]
            params = strat["default_params"]
            
            try:
                df_signals = apply_strategy(strat_name, df, params)
                bt = Backtester(initial_capital=CAPITAL)
                result = bt.run(
                    df_signals,
                    strategy_name=strat_name,
                    symbol=symbol,
                    timeframe=TIMEFRAME,
                    params=params,
                    save=False,
                )
                results_by_asset[symbol].append(result)
                all_results.append(result)
                
                ret = result['total_return_pct']
                sharpe = result['sharpe_ratio']
                trades = result['total_trades']
                print(f"  {strat_name:<20} Return: {ret:>+8.2f}%  Sharpe: {sharpe:>7.3f}  Trades: {trades:>4}")
                
            except Exception as e:
                print(f"  {strat_name:<20} ERROR: {e}")
    
    return all_results, results_by_asset

def print_ranked_results(results_by_asset, all_results):
    """Print clean ranked tables."""
    
    print(f"\n\n{'='*100}")
    print(f"  FULL 1-HOUR STRATEGY COMPARISON RESULTS")
    print(f"  Capital: ${CAPITAL:,.0f} | Exchange: {EXCHANGE} | Timeframe: {TIMEFRAME}")
    print(f"{'='*100}")
    
    # Per-asset rankings
    for symbol, results in results_by_asset.items():
        if not results:
            continue
        
        sorted_r = sorted(results, key=lambda x: x.get('total_return_pct', 0), reverse=True)
        
        candles = "N/A"
        if results:
            candles = f"{results[0].get('start_date', '?')[:10]} to {results[0].get('end_date', '?')[:10]}"
        
        print(f"\n{'─'*100}")
        print(f"  {symbol} — Ranked by Total Return")
        print(f"  Period: {candles}")
        print(f"{'─'*100}")
        print(f"  {'#':<4} {'Strategy':<22} {'Return %':>10} {'Sharpe':>8} {'MaxDD %':>10} {'WinRate %':>10} {'Trades':>8}")
        print(f"  {'─'*96}")
        
        for i, r in enumerate(sorted_r, 1):
            print(
                f"  {i:<4} {r['strategy_name']:<22} "
                f"{r['total_return_pct']:>+9.2f}% "
                f"{r['sharpe_ratio']:>7.3f} "
                f"{r['max_drawdown_pct']:>+9.2f}% "
                f"{r['win_rate']:>8.1f}% "
                f"{r['total_trades']:>8}"
            )
    
    # Overall top 5 by Sharpe
    print(f"\n\n{'='*100}")
    print(f"  OVERALL TOP 5 — Ranked by Sharpe Ratio (across all assets)")
    print(f"{'='*100}")
    top_sharpe = sorted(all_results, key=lambda x: x.get('sharpe_ratio', 0), reverse=True)[:5]
    print(f"  {'#':<4} {'Strategy':<22} {'Asset':<12} {'Return %':>10} {'Sharpe':>8} {'MaxDD %':>10} {'WinRate %':>10} {'Trades':>8}")
    print(f"  {'─'*96}")
    for i, r in enumerate(top_sharpe, 1):
        print(
            f"  {i:<4} {r['strategy_name']:<22} {r['symbol']:<12} "
            f"{r['total_return_pct']:>+9.2f}% "
            f"{r['sharpe_ratio']:>7.3f} "
            f"{r['max_drawdown_pct']:>+9.2f}% "
            f"{r['win_rate']:>8.1f}% "
            f"{r['total_trades']:>8}"
        )
    
    # Overall top 5 by Return
    print(f"\n{'='*100}")
    print(f"  OVERALL TOP 5 — Ranked by Total Return (across all assets)")
    print(f"{'='*100}")
    top_return = sorted(all_results, key=lambda x: x.get('total_return_pct', 0), reverse=True)[:5]
    print(f"  {'#':<4} {'Strategy':<22} {'Asset':<12} {'Return %':>10} {'Sharpe':>8} {'MaxDD %':>10} {'WinRate %':>10} {'Trades':>8}")
    print(f"  {'─'*96}")
    for i, r in enumerate(top_return, 1):
        print(
            f"  {i:<4} {r['strategy_name']:<22} {r['symbol']:<12} "
            f"{r['total_return_pct']:>+9.2f}% "
            f"{r['sharpe_ratio']:>7.3f} "
            f"{r['max_drawdown_pct']:>+9.2f}% "
            f"{r['win_rate']:>8.1f}% "
            f"{r['total_trades']:>8}"
        )
    
    # Strategy average performance across all assets
    print(f"\n{'='*100}")
    print(f"  STRATEGY AVERAGES ACROSS ALL ASSETS")
    print(f"{'='*100}")
    
    strat_avgs = {}
    for r in all_results:
        name = r['strategy_name']
        if name not in strat_avgs:
            strat_avgs[name] = {'returns': [], 'sharpes': [], 'drawdowns': [], 'winrates': [], 'trades': []}
        strat_avgs[name]['returns'].append(r['total_return_pct'])
        strat_avgs[name]['sharpes'].append(r['sharpe_ratio'])
        strat_avgs[name]['drawdowns'].append(r['max_drawdown_pct'])
        strat_avgs[name]['winrates'].append(r['win_rate'])
        strat_avgs[name]['trades'].append(r['total_trades'])
    
    avg_list = []
    for name, vals in strat_avgs.items():
        avg_list.append({
            'name': name,
            'avg_return': np.mean(vals['returns']),
            'avg_sharpe': np.mean(vals['sharpes']),
            'avg_dd': np.mean(vals['drawdowns']),
            'avg_winrate': np.mean(vals['winrates']),
            'avg_trades': np.mean(vals['trades']),
        })
    
    avg_list.sort(key=lambda x: x['avg_sharpe'], reverse=True)
    
    print(f"  {'#':<4} {'Strategy':<22} {'Avg Return %':>12} {'Avg Sharpe':>11} {'Avg MaxDD %':>12} {'Avg WinRate':>11} {'Avg Trades':>11}")
    print(f"  {'─'*96}")
    for i, a in enumerate(avg_list, 1):
        print(
            f"  {i:<4} {a['name']:<22} "
            f"{a['avg_return']:>+10.2f}% "
            f"{a['avg_sharpe']:>10.3f} "
            f"{a['avg_dd']:>+10.2f}% "
            f"{a['avg_winrate']:>9.1f}% "
            f"{a['avg_trades']:>10.1f}"
        )
    
    print(f"\n{'='*100}")
    print(f"  DONE")
    print(f"{'='*100}")


if __name__ == "__main__":
    print("Step 1: Fetching historical 1h data...")
    data = fetch_all_data()
    
    # Print candle counts
    print(f"\n{'='*60}")
    print(f"  DATA SUMMARY")
    print(f"{'='*60}")
    for symbol, df in data.items():
        if not df.empty:
            print(f"  {symbol}: {len(df)} candles ({df.index[0]} to {df.index[-1]})")
        else:
            print(f"  {symbol}: NO DATA")
    
    print("\nStep 2: Running backtests...")
    all_results, results_by_asset = run_comparison(data)
    
    print("\nStep 3: Generating ranked reports...")
    print_ranked_results(results_by_asset, all_results)
