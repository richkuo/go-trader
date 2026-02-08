"""
Multi-asset portfolio optimization and strategy analytics.
- Mean-variance optimization (Markowitz)
- Strategy correlation analysis
- Performance attribution
"""

import numpy as np
import pandas as pd
from typing import Dict, List, Optional, Tuple


def calculate_returns(prices_dict: Dict[str, pd.Series]) -> pd.DataFrame:
    """Calculate daily returns from price series dict."""
    prices_df = pd.DataFrame(prices_dict).dropna()
    return prices_df.pct_change().dropna()


def mean_variance_optimize(
    returns: pd.DataFrame,
    risk_free_rate: float = 0.02,
    n_portfolios: int = 5000,
    target_return: Optional[float] = None,
) -> dict:
    """
    Mean-variance portfolio optimization via Monte Carlo simulation.
    Lightweight alternative to scipy.optimize for 2GB RAM constraint.

    Args:
        returns: DataFrame of asset returns (each column = asset)
        risk_free_rate: Annual risk-free rate
        n_portfolios: Number of random portfolios to simulate
        target_return: Optional target annual return

    Returns:
        Dict with optimal weights and metrics
    """
    n_assets = len(returns.columns)
    assets = list(returns.columns)
    mean_returns = returns.mean() * 365  # Annualize
    cov_matrix = returns.cov() * 365

    results = np.zeros((n_portfolios, 3 + n_assets))
    np.random.seed(42)

    for i in range(n_portfolios):
        weights = np.random.dirichlet(np.ones(n_assets))

        portfolio_return = np.dot(weights, mean_returns)
        portfolio_vol = np.sqrt(np.dot(weights, np.dot(cov_matrix, weights)))
        sharpe = (portfolio_return - risk_free_rate) / portfolio_vol if portfolio_vol > 0 else 0

        results[i, 0] = portfolio_return
        results[i, 1] = portfolio_vol
        results[i, 2] = sharpe
        results[i, 3:] = weights

    # Find optimal portfolios
    max_sharpe_idx = results[:, 2].argmax()
    min_vol_idx = results[:, 1].argmin()

    max_sharpe_weights = dict(zip(assets, results[max_sharpe_idx, 3:]))
    min_vol_weights = dict(zip(assets, results[min_vol_idx, 3:]))

    # If target return specified, find closest with min volatility
    target_weights = None
    if target_return is not None:
        close_to_target = np.abs(results[:, 0] - target_return) < 0.05
        if close_to_target.any():
            target_results = results[close_to_target]
            best_idx = target_results[:, 1].argmin()
            target_weights = dict(zip(assets, target_results[best_idx, 3:]))

    return {
        "max_sharpe": {
            "weights": {k: round(v, 4) for k, v in max_sharpe_weights.items()},
            "return": round(results[max_sharpe_idx, 0] * 100, 2),
            "volatility": round(results[max_sharpe_idx, 1] * 100, 2),
            "sharpe": round(results[max_sharpe_idx, 2], 3),
        },
        "min_volatility": {
            "weights": {k: round(v, 4) for k, v in min_vol_weights.items()},
            "return": round(results[min_vol_idx, 0] * 100, 2),
            "volatility": round(results[min_vol_idx, 1] * 100, 2),
            "sharpe": round(results[min_vol_idx, 2], 3),
        },
        "target_return": {
            "weights": {k: round(v, 4) for k, v in target_weights.items()} if target_weights else None,
        } if target_return else None,
        "assets": assets,
        "individual_returns": {a: round(r * 100, 2) for a, r in mean_returns.items()},
        "individual_volatility": {a: round(np.sqrt(cov_matrix.loc[a, a]) * 100, 2) for a in assets},
    }


def strategy_correlation_analysis(strategy_returns: Dict[str, pd.Series]) -> dict:
    """
    Analyze correlations between strategy returns.
    Low correlation = good diversification potential.
    """
    returns_df = pd.DataFrame(strategy_returns).dropna()
    if returns_df.empty or len(returns_df.columns) < 2:
        return {"error": "Need at least 2 strategies for correlation analysis"}

    corr_matrix = returns_df.corr()
    n = len(corr_matrix)
    pairs = []
    for i in range(n):
        for j in range(i+1, n):
            s1, s2 = corr_matrix.columns[i], corr_matrix.columns[j]
            pairs.append({
                "strategy_1": s1,
                "strategy_2": s2,
                "correlation": round(corr_matrix.iloc[i, j], 4),
            })

    pairs.sort(key=lambda x: abs(x["correlation"]))

    avg_corr = np.mean([abs(p["correlation"]) for p in pairs])

    return {
        "correlation_matrix": corr_matrix.round(4).to_dict(),
        "pairs": pairs,
        "avg_abs_correlation": round(avg_corr, 4),
        "most_diversified_pair": pairs[0] if pairs else None,
        "most_correlated_pair": pairs[-1] if pairs else None,
        "diversification_score": round(1 - avg_corr, 4),  # Higher = more diversified
    }


def performance_attribution(
    strategy_returns: Dict[str, pd.Series],
    weights: Dict[str, float],
    benchmark_returns: Optional[pd.Series] = None,
) -> dict:
    """
    Performance attribution analysis.
    Breaks down portfolio return by strategy contribution.
    """
    returns_df = pd.DataFrame(strategy_returns).dropna()
    if returns_df.empty:
        return {"error": "No returns data"}

    # Weighted returns
    portfolio_return = 0
    contributions = {}
    for strat, weight in weights.items():
        if strat in returns_df.columns:
            strat_return = returns_df[strat].sum()
            contribution = strat_return * weight
            portfolio_return += contribution
            contributions[strat] = {
                "weight": round(weight, 4),
                "strategy_return": round(strat_return * 100, 2),
                "contribution": round(contribution * 100, 2),
                "volatility": round(returns_df[strat].std() * np.sqrt(365) * 100, 2),
            }

    # Benchmark comparison
    benchmark_info = None
    if benchmark_returns is not None:
        aligned = benchmark_returns.reindex(returns_df.index).dropna()
        if len(aligned) > 0:
            bench_return = aligned.sum()
            excess_return = portfolio_return - bench_return
            bench_vol = aligned.std() * np.sqrt(365)
            benchmark_info = {
                "benchmark_return": round(bench_return * 100, 2),
                "excess_return": round(excess_return * 100, 2),
                "benchmark_volatility": round(bench_vol * 100, 2),
                "information_ratio": round(excess_return / (returns_df.mean(axis=1) - aligned).std() / np.sqrt(365), 3) if (returns_df.mean(axis=1) - aligned).std() > 0 else 0,
            }

    return {
        "portfolio_return": round(portfolio_return * 100, 2),
        "contributions": contributions,
        "benchmark": benchmark_info,
    }


def format_portfolio_report(opt_result: dict) -> str:
    """Format portfolio optimization results."""
    lines = [
        f"\n{'='*70}",
        f"  PORTFOLIO OPTIMIZATION REPORT",
        f"{'='*70}",
    ]

    ms = opt_result["max_sharpe"]
    lines.extend([
        f"\n  ▸ MAX SHARPE PORTFOLIO",
        f"    Expected Return: {ms['return']:+.2f}%",
        f"    Volatility:      {ms['volatility']:.2f}%",
        f"    Sharpe Ratio:    {ms['sharpe']:.3f}",
        f"    Weights:",
    ])
    for asset, w in sorted(ms["weights"].items(), key=lambda x: -x[1]):
        lines.append(f"      {asset:<12} {w:.1%}")

    mv = opt_result["min_volatility"]
    lines.extend([
        f"\n  ▸ MIN VOLATILITY PORTFOLIO",
        f"    Expected Return: {mv['return']:+.2f}%",
        f"    Volatility:      {mv['volatility']:.2f}%",
        f"    Sharpe Ratio:    {mv['sharpe']:.3f}",
        f"    Weights:",
    ])
    for asset, w in sorted(mv["weights"].items(), key=lambda x: -x[1]):
        lines.append(f"      {asset:<12} {w:.1%}")

    # Individual assets
    lines.extend([f"\n  ▸ INDIVIDUAL ASSETS"])
    for asset in opt_result.get("assets", []):
        r = opt_result.get("individual_returns", {}).get(asset, 0)
        v = opt_result.get("individual_volatility", {}).get(asset, 0)
        lines.append(f"    {asset:<12} Return: {r:+.2f}%  Vol: {v:.2f}%")

    lines.append(f"{'='*70}")
    return "\n".join(lines)


def format_correlation_report(corr_result: dict) -> str:
    """Format correlation analysis results."""
    lines = [
        f"\n{'='*70}",
        f"  STRATEGY CORRELATION ANALYSIS",
        f"{'='*70}",
        f"  Diversification Score: {corr_result.get('diversification_score', 0):.4f} (1.0 = perfectly uncorrelated)",
        f"  Avg Absolute Correlation: {corr_result.get('avg_abs_correlation', 0):.4f}",
    ]

    best = corr_result.get("most_diversified_pair")
    worst = corr_result.get("most_correlated_pair")
    if best:
        lines.append(f"  Most Diversified: {best['strategy_1']} ↔ {best['strategy_2']} ({best['correlation']:.4f})")
    if worst:
        lines.append(f"  Most Correlated:  {worst['strategy_1']} ↔ {worst['strategy_2']} ({worst['correlation']:.4f})")

    lines.extend([f"\n  CORRELATION PAIRS:"])
    for p in corr_result.get("pairs", []):
        bar = "█" * int(abs(p["correlation"]) * 20)
        lines.append(f"    {p['strategy_1']:<15} ↔ {p['strategy_2']:<15} {p['correlation']:+.4f} {bar}")

    lines.append(f"{'='*70}")
    return "\n".join(lines)


def format_attribution_report(attr_result: dict) -> str:
    """Format performance attribution results."""
    lines = [
        f"\n{'='*70}",
        f"  PERFORMANCE ATTRIBUTION",
        f"{'='*70}",
        f"  Portfolio Return: {attr_result.get('portfolio_return', 0):+.2f}%",
        f"\n  {'Strategy':<20} {'Weight':>8} {'Return':>8} {'Contribution':>14} {'Volatility':>12}",
        f"  {'─'*64}",
    ]
    for strat, data in attr_result.get("contributions", {}).items():
        lines.append(
            f"  {strat:<20} {data['weight']:>7.1%} {data['strategy_return']:>+7.2f}% "
            f"{data['contribution']:>+13.2f}% {data['volatility']:>10.2f}%"
        )

    bench = attr_result.get("benchmark")
    if bench:
        lines.extend([
            f"\n  Benchmark Return:  {bench['benchmark_return']:+.2f}%",
            f"  Excess Return:     {bench['excess_return']:+.2f}%",
        ])

    lines.append(f"{'='*70}")
    return "\n".join(lines)


if __name__ == "__main__":
    # Test with synthetic data
    np.random.seed(42)
    n = 365
    dates = pd.date_range("2023-01-01", periods=n, freq="D")

    returns_data = {
        "BTC": pd.Series(np.random.randn(n) * 0.03 + 0.001, index=dates),
        "ETH": pd.Series(np.random.randn(n) * 0.04 + 0.0015, index=dates),
        "SOL": pd.Series(np.random.randn(n) * 0.05 + 0.002, index=dates),
    }

    # Portfolio optimization
    returns_df = pd.DataFrame(returns_data)
    opt = mean_variance_optimize(returns_df)
    print(format_portfolio_report(opt))

    # Strategy correlation
    strat_returns = {
        "sma_crossover": pd.Series(np.random.randn(n) * 0.02, index=dates),
        "macd": pd.Series(np.random.randn(n) * 0.025, index=dates),
        "rsi": pd.Series(np.random.randn(n) * 0.015 + 0.0005, index=dates),
        "momentum": pd.Series(np.random.randn(n) * 0.03, index=dates),
    }
    corr = strategy_correlation_analysis(strat_returns)
    print(format_correlation_report(corr))

    # Attribution
    attr = performance_attribution(strat_returns, {"sma_crossover": 0.3, "macd": 0.3, "rsi": 0.2, "momentum": 0.2})
    print(format_attribution_report(attr))
