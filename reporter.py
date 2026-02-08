"""
Performance reporting — generates text-based reports with comprehensive metrics.
Supports single-strategy reports, comparisons, and multi-asset analysis.
"""

import json
from typing import List, Dict, Optional
from datetime import datetime

import numpy as np
import pandas as pd

from storage import get_backtest_results


def format_single_report(results: dict) -> str:
    """Format a single backtest result into a detailed text report."""
    lines = [
        f"\n{'='*70}",
        f"  BACKTEST REPORT: {results.get('strategy_name', 'Unknown')}",
        f"{'='*70}",
        f"  Symbol:          {results.get('symbol', 'N/A')}",
        f"  Timeframe:       {results.get('timeframe', 'N/A')}",
        f"  Period:          {str(results.get('start_date', ''))[:10]} → {str(results.get('end_date', ''))[:10]}",
        f"  Initial Capital: ${results.get('initial_capital', 0):,.2f}",
        f"  Final Capital:   ${results.get('final_capital', 0):,.2f}",
        f"  Parameters:      {results.get('params', {})}",
        f"{'─'*70}",
        f"  RETURNS",
        f"    Total Return:    {results.get('total_return_pct', 0):+.2f}%",
        f"    Annual Return:   {results.get('annual_return_pct', 0):+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*70}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results.get('sharpe_ratio', 0):.3f}",
        f"    Sortino Ratio:   {results.get('sortino_ratio', 0):.3f}",
        f"    Max Drawdown:    {results.get('max_drawdown_pct', 0):.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*70}",
        f"  TRADE STATS",
        f"    Total Trades:    {results.get('total_trades', 0)}",
        f"    Win Rate:        {results.get('win_rate', 0):.1f}%",
        f"    Profit Factor:   {results.get('profit_factor', 0):.3f}",
        f"    Avg Win:         {results.get('avg_win_pct', 0):+.2f}%",
        f"    Avg Loss:        {results.get('avg_loss_pct', 0):+.2f}%",
    ]

    # Trade log
    trades = results.get("trades", [])
    if trades:
        lines.append(f"{'─'*70}")
        lines.append(f"  TRADE LOG ({len(trades)} trades)")
        lines.append(f"  {'Entry Date':<22} {'Exit Date':<22} {'Entry $':>10} {'Exit $':>10} {'PnL %':>8}")
        lines.append(f"  {'─'*74}")
        for t in trades[:20]:  # Show first 20
            lines.append(
                f"  {str(t.get('entry_date',''))[:19]:<22} "
                f"{str(t.get('exit_date',''))[:19]:<22} "
                f"{t.get('entry_price',0):>10,.2f} "
                f"{t.get('exit_price',0):>10,.2f} "
                f"{t.get('pnl_pct',0):>+7.2f}%"
            )
        if len(trades) > 20:
            lines.append(f"  ... and {len(trades)-20} more trades")

    lines.append(f"{'='*70}")
    return "\n".join(lines)


def format_comparison_report(results_list: List[dict], title: str = "STRATEGY COMPARISON") -> str:
    """Format multiple backtest results into a comparison table."""
    if not results_list:
        return "No results to compare."

    lines = [
        f"\n{'='*90}",
        f"  {title}",
        f"{'='*90}",
        f"  {'Strategy':<20} {'Symbol':<10} {'Return':>8} {'Sharpe':>8} {'Sortino':>8} "
        f"{'MaxDD':>8} {'WinRate':>8} {'PF':>6} {'Trades':>7}",
        f"  {'─'*88}",
    ]

    sorted_results = sorted(results_list, key=lambda x: x.get("sharpe_ratio", 0), reverse=True)
    for r in sorted_results:
        lines.append(
            f"  {r.get('strategy_name','?'):<20} "
            f"{r.get('symbol','?'):<10} "
            f"{r.get('total_return_pct',0):>+7.1f}% "
            f"{r.get('sharpe_ratio',0):>7.2f} "
            f"{r.get('sortino_ratio',0):>7.2f} "
            f"{r.get('max_drawdown_pct',0):>+7.1f}% "
            f"{r.get('win_rate',0):>6.1f}% "
            f"{r.get('profit_factor',0):>5.2f} "
            f"{r.get('total_trades',0):>6}"
        )

    # Summary stats
    returns = [r.get("total_return_pct", 0) for r in sorted_results]
    sharpes = [r.get("sharpe_ratio", 0) for r in sorted_results]
    lines.extend([
        f"  {'─'*88}",
        f"  Best Return: {max(returns):+.2f}% | Best Sharpe: {max(sharpes):.3f}",
        f"  Mean Return: {np.mean(returns):+.2f}% | Mean Sharpe: {np.mean(sharpes):.3f}",
        f"{'='*90}",
    ])
    return "\n".join(lines)


def format_multi_asset_report(results_by_asset: Dict[str, List[dict]]) -> str:
    """Format results across multiple assets."""
    lines = [
        f"\n{'#'*90}",
        f"  MULTI-ASSET ANALYSIS",
        f"{'#'*90}",
    ]

    for symbol, results in results_by_asset.items():
        lines.append(f"\n  ▸ {symbol}")
        lines.append(format_comparison_report(results, title=f"{symbol} Results"))

    # Cross-asset summary
    all_results = []
    for results in results_by_asset.values():
        all_results.extend(results)

    if all_results:
        lines.append(f"\n{'='*90}")
        lines.append(f"  CROSS-ASSET TOP PERFORMERS (by Sharpe)")
        lines.append(f"{'='*90}")
        top = sorted(all_results, key=lambda x: x.get("sharpe_ratio", 0), reverse=True)[:10]
        lines.append(f"  {'Rank':<5} {'Strategy':<20} {'Symbol':<10} {'Return':>8} {'Sharpe':>8} {'MaxDD':>8}")
        lines.append(f"  {'─'*65}")
        for i, r in enumerate(top, 1):
            lines.append(
                f"  {i:<5} {r.get('strategy_name','?'):<20} {r.get('symbol','?'):<10} "
                f"{r.get('total_return_pct',0):>+7.1f}% {r.get('sharpe_ratio',0):>7.2f} "
                f"{r.get('max_drawdown_pct',0):>+7.1f}%"
            )

    return "\n".join(lines)


def format_walk_forward_report(wf_result: dict) -> str:
    """Format walk-forward optimization results."""
    lines = [
        f"\n{'='*70}",
        f"  WALK-FORWARD OPTIMIZATION: {wf_result.get('strategy', 'Unknown')}",
        f"{'='*70}",
        f"  Folds:             {wf_result.get('n_valid_folds', 0)}/{wf_result.get('n_splits', 0)}",
        f"  Param Combos:      {wf_result.get('param_grid_size', 0)}",
        f"  Optimize Metric:   {wf_result.get('optimize_metric', 'sharpe_ratio')}",
        f"{'─'*70}",
        f"  OUT-OF-SAMPLE PERFORMANCE",
        f"    Mean Return:     {wf_result.get('oos_mean_return', 0):+.2f}%",
        f"    Median Return:   {wf_result.get('oos_median_return', 0):+.2f}%",
        f"    Return StdDev:   {wf_result.get('oos_std_return', 0):.2f}%",
        f"    Mean Sharpe:     {wf_result.get('oos_mean_sharpe', 0):.3f}",
        f"    Mean MaxDD:      {wf_result.get('oos_mean_drawdown', 0):.2f}%",
        f"    Worst MaxDD:     {wf_result.get('oos_worst_drawdown', 0):.2f}%",
        f"{'─'*70}",
        f"  STABILITY",
        f"    Most Stable Params: {wf_result.get('most_common_best_params', 'N/A')}",
    ]

    # Per-fold details
    windows = wf_result.get("window_results", [])
    if windows:
        lines.append(f"{'─'*70}")
        lines.append(f"  PER-FOLD BREAKDOWN")
        lines.append(f"  {'Fold':<6} {'Test Period':<25} {'Return':>8} {'Sharpe':>8} {'Params'}")
        lines.append(f"  {'─'*68}")
        for w in windows:
            tr = w.get("test_result", {})
            lines.append(
                f"  {w.get('fold',0):<6} "
                f"{w.get('test_period','?'):<25} "
                f"{tr.get('total_return_pct',0):>+7.1f}% "
                f"{tr.get('sharpe_ratio',0):>7.2f} "
                f"{w.get('best_params', {})}"
            )

    lines.append(f"{'='*70}")
    return "\n".join(lines)


def generate_full_report(
    results_list: List[dict],
    wf_results: Optional[List[dict]] = None,
    title: str = "TRADING BOT ANALYSIS REPORT"
) -> str:
    """Generate a comprehensive report combining all analyses."""
    now = datetime.utcnow().strftime("%Y-%m-%d %H:%M UTC")
    lines = [
        f"\n{'#'*90}",
        f"  {title}",
        f"  Generated: {now}",
        f"{'#'*90}",
    ]

    # Group by symbol
    by_symbol = {}
    for r in results_list:
        sym = r.get("symbol", "Unknown")
        by_symbol.setdefault(sym, []).append(r)

    if len(by_symbol) > 1:
        lines.append(format_multi_asset_report(by_symbol))
    else:
        lines.append(format_comparison_report(results_list))

    # Walk-forward results
    if wf_results:
        lines.append(f"\n{'#'*90}")
        lines.append(f"  WALK-FORWARD OPTIMIZATION RESULTS")
        lines.append(f"{'#'*90}")
        for wf in wf_results:
            lines.append(format_walk_forward_report(wf))

    return "\n".join(lines)


if __name__ == "__main__":
    # Test with dummy data
    dummy = [
        {"strategy_name": "sma_crossover", "symbol": "BTC/USDT", "total_return_pct": 15.2,
         "sharpe_ratio": 1.2, "sortino_ratio": 1.8, "max_drawdown_pct": -12.5,
         "win_rate": 55.0, "profit_factor": 1.4, "total_trades": 20},
        {"strategy_name": "rsi", "symbol": "BTC/USDT", "total_return_pct": 8.5,
         "sharpe_ratio": 0.9, "sortino_ratio": 1.1, "max_drawdown_pct": -18.3,
         "win_rate": 48.0, "profit_factor": 1.1, "total_trades": 35},
    ]
    print(format_comparison_report(dummy))
