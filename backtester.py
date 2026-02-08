"""
Backtesting engine — simulates strategy execution on historical data.
Calculates comprehensive performance metrics.
"""

import json
import math
from datetime import datetime
from typing import Callable, Optional

import numpy as np
import pandas as pd

from storage import store_backtest_result


class Trade:
    """Represents a single round-trip trade."""
    def __init__(self, entry_date, entry_price, side="long"):
        self.entry_date = entry_date
        self.entry_price = entry_price
        self.side = side
        self.exit_date = None
        self.exit_price = None
        self.pnl = 0.0
        self.pnl_pct = 0.0
        self.shares = 0.0

    def close(self, exit_date, exit_price):
        self.exit_date = exit_date
        self.exit_price = exit_price
        if self.side == "long":
            self.pnl_pct = (exit_price - self.entry_price) / self.entry_price
        else:
            self.pnl_pct = (self.entry_price - exit_price) / self.entry_price
        self.pnl = self.shares * self.entry_price * self.pnl_pct

    def to_dict(self):
        return {
            "entry_date": str(self.entry_date),
            "exit_date": str(self.exit_date),
            "entry_price": self.entry_price,
            "exit_price": self.exit_price,
            "side": self.side,
            "shares": self.shares,
            "pnl": round(self.pnl, 2),
            "pnl_pct": round(self.pnl_pct * 100, 2),
        }


class Backtester:
    """
    Event-driven backtesting engine.

    Usage:
        bt = Backtester(initial_capital=1000)
        results = bt.run(df_with_signals, strategy_name="SMA Crossover")
    """

    def __init__(self, initial_capital: float = 1000.0, commission_pct: float = 0.001,
                 slippage_pct: float = 0.0005):
        """
        Args:
            initial_capital: Starting portfolio value
            commission_pct: Commission per trade as fraction (0.001 = 0.1%)
            slippage_pct: Slippage per trade as fraction (0.0005 = 0.05%)
        """
        self.initial_capital = initial_capital
        self.commission_pct = commission_pct
        self.slippage_pct = slippage_pct

    def run(self, df: pd.DataFrame, strategy_name: str = "Unknown",
            symbol: str = "BTC/USDT", timeframe: str = "1d",
            params: Optional[dict] = None, save: bool = True) -> dict:
        """
        Run backtest on a DataFrame that already has a 'signal' column.
        signal: 1 = buy, -1 = sell, 0 = hold

        Returns dict with all performance metrics.
        """
        if "signal" not in df.columns:
            raise ValueError("DataFrame must have a 'signal' column")

        cash = self.initial_capital
        position = 0.0  # shares held
        trades = []
        current_trade = None
        equity_curve = []

        for i, (idx, row) in enumerate(df.iterrows()):
            price = row["close"]
            signal = row["signal"]

            # Calculate current equity
            equity = cash + position * price
            equity_curve.append({"date": idx, "equity": equity})

            # Process signals
            if signal == 1 and position == 0:
                # BUY — go long with all available cash
                effective_price = price * (1 + self.slippage_pct)
                commission = cash * self.commission_pct
                available = cash - commission
                shares = available / effective_price
                position = shares
                cash = 0.0

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares

            elif signal == -1 and position > 0:
                # SELL — close long position
                effective_price = price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash = proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    trades.append(current_trade)
                    current_trade = None

        # Close any open position at the end
        if position > 0:
            final_price = df["close"].iloc[-1] * (1 - self.slippage_pct)
            proceeds = position * final_price
            commission = proceeds * self.commission_pct
            cash = proceeds - commission
            position = 0.0

            if current_trade:
                current_trade.close(df.index[-1], final_price)
                trades.append(current_trade)

        final_equity = cash
        equity_df = pd.DataFrame(equity_curve).set_index("date")

        # Calculate metrics
        metrics = self._calculate_metrics(equity_df, trades, df)
        metrics.update({
            "strategy_name": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "start_date": str(df.index[0]),
            "end_date": str(df.index[-1]),
            "initial_capital": self.initial_capital,
            "final_capital": round(final_equity, 2),
            "params": params or {},
            "trades": [t.to_dict() for t in trades],
        })

        if save:
            store_backtest_result(metrics)

        return metrics

    def _calculate_metrics(self, equity_df: pd.DataFrame, trades: list,
                           df: pd.DataFrame) -> dict:
        """Calculate comprehensive performance metrics."""
        equity = equity_df["equity"]

        # Returns
        total_return = (equity.iloc[-1] - equity.iloc[0]) / equity.iloc[0]

        # Annualized return
        days = (df.index[-1] - df.index[0]).days
        years = max(days / 365.25, 0.01)
        annual_return = (1 + total_return) ** (1 / years) - 1 if total_return > -1 else -1

        # Daily returns for ratio calculations
        daily_returns = equity.pct_change().dropna()

        # Sharpe Ratio (annualized, assuming 365 trading days for crypto)
        if len(daily_returns) > 1 and daily_returns.std() > 0:
            sharpe = (daily_returns.mean() / daily_returns.std()) * np.sqrt(365)
        else:
            sharpe = 0.0

        # Sortino Ratio (only downside deviation)
        downside = daily_returns[daily_returns < 0]
        if len(downside) > 1 and downside.std() > 0:
            sortino = (daily_returns.mean() / downside.std()) * np.sqrt(365)
        else:
            sortino = 0.0

        # Max Drawdown
        cummax = equity.cummax()
        drawdown = (equity - cummax) / cummax
        max_drawdown = drawdown.min()

        # Trade statistics
        total_trades = len(trades)
        if total_trades > 0:
            winning = [t for t in trades if t.pnl > 0]
            losing = [t for t in trades if t.pnl <= 0]
            win_rate = len(winning) / total_trades

            gross_profit = sum(t.pnl for t in winning) if winning else 0
            gross_loss = abs(sum(t.pnl for t in losing)) if losing else 0
            profit_factor = gross_profit / gross_loss if gross_loss > 0 else float("inf")

            avg_win = np.mean([t.pnl_pct for t in winning]) if winning else 0
            avg_loss = np.mean([t.pnl_pct for t in losing]) if losing else 0
        else:
            win_rate = 0
            profit_factor = 0
            avg_win = 0
            avg_loss = 0

        # Volatility (annualized)
        volatility = daily_returns.std() * np.sqrt(365) if len(daily_returns) > 1 else 0

        # Calmar ratio
        calmar = annual_return / abs(max_drawdown) if max_drawdown != 0 else 0

        return {
            "total_return_pct": round(total_return * 100, 2),
            "annual_return_pct": round(annual_return * 100, 2),
            "sharpe_ratio": round(sharpe, 3),
            "sortino_ratio": round(sortino, 3),
            "max_drawdown_pct": round(max_drawdown * 100, 2),
            "calmar_ratio": round(calmar, 3),
            "volatility_pct": round(volatility * 100, 2),
            "win_rate": round(win_rate * 100, 2),
            "profit_factor": round(profit_factor, 3),
            "total_trades": total_trades,
            "avg_win_pct": round(avg_win * 100, 2),
            "avg_loss_pct": round(avg_loss * 100, 2),
        }


def format_results(results: dict) -> str:
    """Pretty-print backtest results."""
    lines = [
        f"\n{'='*60}",
        f"  BACKTEST RESULTS: {results['strategy_name']}",
        f"{'='*60}",
        f"  Symbol:          {results['symbol']}",
        f"  Timeframe:       {results['timeframe']}",
        f"  Period:          {results['start_date'][:10]} → {results['end_date'][:10]}",
        f"  Initial Capital: ${results['initial_capital']:,.2f}",
        f"  Final Capital:   ${results['final_capital']:,.2f}",
        f"{'─'*60}",
        f"  RETURNS",
        f"    Total Return:    {results['total_return_pct']:+.2f}%",
        f"    Annual Return:   {results['annual_return_pct']:+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*60}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results['sharpe_ratio']:.3f}",
        f"    Sortino Ratio:   {results['sortino_ratio']:.3f}",
        f"    Max Drawdown:    {results['max_drawdown_pct']:.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*60}",
        f"  TRADE STATS",
        f"    Total Trades:    {results['total_trades']}",
        f"    Win Rate:        {results['win_rate']:.1f}%",
        f"    Profit Factor:   {results['profit_factor']:.3f}",
        f"    Avg Win:         {results.get('avg_win_pct', 0):+.2f}%",
        f"    Avg Loss:        {results.get('avg_loss_pct', 0):+.2f}%",
        f"{'='*60}",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    # Quick test with synthetic data
    np.random.seed(42)
    dates = pd.date_range("2023-01-01", periods=200, freq="D")
    prices = 100 + np.cumsum(np.random.randn(200) * 2)
    df = pd.DataFrame({
        "close": prices,
    }, index=dates)

    # Add simple alternating signals for testing
    df["signal"] = 0
    df.iloc[10, df.columns.get_loc("signal")] = 1  # buy
    df.iloc[30, df.columns.get_loc("signal")] = -1  # sell
    df.iloc[50, df.columns.get_loc("signal")] = 1  # buy
    df.iloc[80, df.columns.get_loc("signal")] = -1  # sell

    bt = Backtester(initial_capital=1000)
    results = bt.run(df, strategy_name="Test", save=False)
    print(format_results(results))
