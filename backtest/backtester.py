"""
Backtesting engine — simulates strategy execution on historical data.
Calculates comprehensive performance metrics.
"""

import sys
import os
import json
import math
from datetime import datetime
from typing import Callable, Optional

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

import numpy as np
import pandas as pd

from storage import store_backtest_result

# Close-registry import is deferred until needed so backtests with no
# close_strategies don't pay the import cost. Uses ``close_registry_loader``
# to avoid the bare ``import registry`` collision with the open registry's
# module of the same name.
_close_registry = None


def _load_close_registry():
    global _close_registry
    if _close_registry is None:
        from close_registry_loader import evaluate as _evaluate, list_strategies as _list
        _close_registry = (_evaluate, _list)
    return _close_registry


# Equity-curve points per year per timeframe — used to derive the Sharpe
# annualization factor. Crypto trades 24/7, so a 1d run has ~365 points/yr,
# a 4h run has ~365*6, etc. Hardcoding sqrt(365) overstated Sharpe by
# sqrt(periods_per_day) for any sub-daily timeframe (issue #304 M3).
TIMEFRAME_PERIODS_PER_YEAR = {
    "1m":  365 * 24 * 60,
    "5m":  365 * 24 * 12,
    "15m": 365 * 24 * 4,
    "30m": 365 * 24 * 2,
    "1h":  365 * 24,
    "2h":  365 * 12,
    "4h":  365 * 6,
    "6h":  365 * 4,
    "8h":  365 * 3,
    "12h": 365 * 2,
    "1d":  365,
    "1w":  52,
    "1M":  12,
}


def periods_per_year(timeframe: str) -> int:
    """Equity-curve samples per year for ``timeframe``; defaults to daily."""
    return TIMEFRAME_PERIODS_PER_YEAR.get(timeframe, 365)


# Taker fee rates per platform — mirrors scheduler/fees.go:CalculatePlatformSpotFee
# and related constants. test_platform_fees.py scrapes fees.go to enforce parity.
PLATFORM_FEE_PCT = {
    "binanceus":   0.001,    # BinanceSpotFeePct
    "hyperliquid": 0.00035,  # HyperliquidTakerFeePct
    "robinhood":   0.0,      # RobinhoodCryptoFeePct (no commission)
    "luno":        0.01,     # LunoTakerFeePct
    "okx":         0.001,    # OKXSpotTakerFeePct
    "okx-perps":   0.0005,   # OKXPerpsTakerFeePct
}


def fee_pct_for_platform(platform: str) -> float:
    """Return taker fee rate for ``platform``; defaults to BinanceUS spot rate
    (0.1%) to match ``scheduler/fees.go:CalculateSpotFee``."""
    return PLATFORM_FEE_PCT.get(platform, PLATFORM_FEE_PCT["binanceus"])


def _open_action_from_signal(signal: int) -> str:
    if signal > 0:
        return "long"
    if signal < 0:
        return "short"
    return "none"


def _normalize_open_action(value) -> str:
    action = str(value or "none").strip().lower()
    if action not in {"long", "short", "none"}:
        raise ValueError(
            "open_action column must contain only 'long', 'short', or 'none' "
            f"(got {value!r})"
        )
    return action


def _close_fraction_columns(df: pd.DataFrame) -> list[str]:
    return [
        c for c in df.columns
        if c == "close_fraction" or str(c).startswith("close_fraction:")
    ]


def _max_close_fraction_series(df: pd.DataFrame) -> pd.Series:
    cols = _close_fraction_columns(df)
    if not cols:
        return pd.Series(0.0, index=df.index)
    fractions = df[cols].fillna(0).astype(float)
    bad = (fractions < 0) | (fractions > 1)
    if bad.any().any():
        values = sorted(set(fractions[bad].stack().tolist()))
        raise ValueError(f"close_fraction values must be in [0, 1] — got {values}")
    return fractions.max(axis=1)


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

    def __init__(self, initial_capital: float = 1000.0,
                 commission_pct: Optional[float] = None,
                 slippage_pct: float = 0.0005,
                 platform: str = "binanceus",
                 close_strategies: Optional[list[str]] = None,
                 close_params: Optional[dict[str, dict]] = None):
        """
        Args:
            initial_capital: Starting portfolio value.
            commission_pct: Commission per trade as fraction. If ``None``,
                derived from ``platform`` using ``PLATFORM_FEE_PCT`` (which
                mirrors ``scheduler/fees.go``). Pass an explicit value to
                override (e.g. in tests).
            slippage_pct: Slippage per trade as fraction (0.0005 = 5 bps).
            platform: Exchange fee model — one of ``PLATFORM_FEE_PCT`` keys.
                Unknown platforms fall back to BinanceUS (0.1%) with no
                warning, matching the Go dispatch default.
            close_strategies: Ordered list of close-evaluator names from
                ``shared_strategies/close/registry.py``. When provided, each
                evaluator runs at the end of every bar against the simulated
                open position; the max ``close_fraction`` across evaluators
                is applied at the next bar's open. Combined with any
                column-based ``close_fraction`` on the input DataFrame using
                max-wins (mirrors the live composition contract).
            close_params: Per-evaluator params dict (keyed by strategy name).
                Merged over the registry's ``default_params`` for that
                strategy. Unknown keys are forwarded as-is to the evaluator.
        """
        self.initial_capital = initial_capital
        self.platform = platform
        self.commission_pct = (
            commission_pct if commission_pct is not None
            else fee_pct_for_platform(platform)
        )
        self.slippage_pct = slippage_pct
        self.close_strategies = list(close_strategies or [])
        self.close_params = dict(close_params or {})
        if self.close_strategies:
            _evaluate, list_strategies = _load_close_registry()
            available = set(list_strategies())
            for name in self.close_strategies:
                if name not in available:
                    raise ValueError(
                        f"Unknown close strategy: {name}. "
                        f"Available: {sorted(available)}"
                    )

    def run(self, df: pd.DataFrame, strategy_name: str = "Unknown",
            symbol: str = "BTC/USDT", timeframe: str = "1d",
            params: Optional[dict] = None, save: bool = True,
            starting_long: Optional[dict] = None) -> dict:
        """
        Run backtest on a DataFrame that already has a 'signal' column.
        signal: 1 = buy, -1 = sell, 0 = hold

        Execution model matches the live scheduler: a signal produced by the
        close of bar t is read after the bar finishes and filled at bar t+1's
        open (no look-ahead bias). Falls back to close when an ``open`` column
        is not present.

        starting_long: optional dict with keys ``entry_price`` (float, USD)
            and ``entry_date`` (index value, defaults to df.index[0]).
            When provided, the run begins already-long: the full
            ``initial_capital`` is treated as committed at ``entry_price``
            (minus one commission for the implicit buy). Use for carrying
            walk-forward position state across a fold boundary so SELL
            signals in the first train bars actually close the warmup
            position instead of being dropped as "sell while flat".
            Note: ``equity[0]`` for a seeded run reflects the starting
            position's mark-to-market (``shares * close[0]``), not
            ``initial_capital``. ``_calculate_metrics`` anchors
            ``total_return_pct`` and ``max_drawdown_pct`` at
            ``self.initial_capital`` so the baseline is consistent with
            unseeded runs, while ``sharpe`` and ``volatility`` are
            computed from ``pct_change()`` and are unaffected.

        Returns dict with all performance metrics.
        """
        uses_open_close = (
            "open_action" in df.columns
            or bool(_close_fraction_columns(df))
            or bool(self.close_strategies)
        )
        if "signal" not in df.columns and not uses_open_close:
            raise ValueError("DataFrame must have a 'signal' column or open_action/close_fraction columns")

        df = df.copy()
        if "signal" in df.columns:
            # Contract: signal ∈ {-1, 0, 1}. position.diff() emits ±1.0 floats
            # and some strategies emit ints; coerce NaN → 0, reject non-integral
            # floats before casting, and then reject any out-of-domain integer.
            sig_raw = df["signal"].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"non-integral values {sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"unexpected values {sorted(bad.unique().tolist())}"
                )
            signal_for_open = sig_int
            df["signal"] = sig_int.shift(1).fillna(0).astype(int)
        else:
            signal_for_open = pd.Series(0, index=df.index)
            df["signal"] = 0

        if uses_open_close:
            if "open_action" in df.columns:
                open_actions = df["open_action"].map(_normalize_open_action)
            else:
                open_actions = signal_for_open.map(_open_action_from_signal)
            df["_open_action"] = open_actions.shift(1).fillna("none")
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)

        has_open = "open" in df.columns

        cash = self.initial_capital
        position = 0.0  # shares held
        trades = []
        current_trade = None
        equity_curve = []

        # Position context for close-strategy evaluators. Stamped at open,
        # cleared at full close. ``initial_quantity`` is preserved across
        # partial closes so tiered evaluators can compute incremental
        # ``close_fraction`` correctly (mirrors live ``Position.InitialQuantity``).
        avg_cost = 0.0
        initial_quantity = 0.0
        entry_atr_value = 0.0
        pending_close_fraction = 0.0

        atr_series = df["atr"] if "atr" in df.columns else None

        if starting_long:
            effective_entry = starting_long["entry_price"]
            entry_commission = self.initial_capital * self.commission_pct
            available = self.initial_capital - entry_commission
            position = available / effective_entry
            cash = 0.0
            current_trade = Trade(
                starting_long.get("entry_date", df.index[0]),
                effective_entry, "long",
            )
            current_trade.shares = position
            avg_cost = effective_entry
            initial_quantity = position

        for i, (idx, row) in enumerate(df.iterrows()):
            fill_price = row["open"] if has_open else row["close"]
            mark_price = row["close"]
            signal = row["signal"]

            equity = cash + position * mark_price
            equity_curve.append({"date": idx, "equity": equity})

            if uses_open_close:
                col_close_fraction = float(row.get("_close_fraction", 0.0))
                close_fraction = max(col_close_fraction, pending_close_fraction)
                pending_close_fraction = 0.0
                open_action = row.get("_open_action", "none")

                if close_fraction > 0 and position != 0:
                    qty_to_close = abs(position) * min(close_fraction, 1.0)
                    if position > 0:
                        effective_price = fill_price * (1 - self.slippage_pct)
                        proceeds = qty_to_close * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                        position -= qty_to_close
                    else:
                        effective_price = fill_price * (1 + self.slippage_pct)
                        cost = qty_to_close * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                        position += qty_to_close

                    if current_trade:
                        closed = Trade(current_trade.entry_date, current_trade.entry_price, current_trade.side)
                        closed.shares = qty_to_close
                        closed.close(idx, effective_price)
                        closed.pnl -= commission
                        trades.append(closed)
                        current_trade.shares -= qty_to_close
                        if current_trade.shares <= 1e-12:
                            current_trade = None

                    if abs(position) <= 1e-12:
                        position = 0.0
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0

                if open_action == "long" and position == 0:
                    effective_price = fill_price * (1 + self.slippage_pct)
                    commission = cash * self.commission_pct
                    available = cash - commission
                    shares = available / effective_price
                    position = shares
                    cash = 0.0

                    current_trade = Trade(idx, effective_price, "long")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                elif open_action == "short" and position == 0:
                    effective_price = fill_price * (1 - self.slippage_pct)
                    commission = cash * self.commission_pct
                    notional = cash - commission
                    shares = notional / effective_price
                    cash = 2 * notional  # pay commission, receive short-sale proceeds
                    position = -shares

                    current_trade = Trade(idx, effective_price, "short")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)

                # End-of-bar: evaluate close strategies against the now-current
                # position using this bar's close as the mark. The result is
                # applied at the NEXT bar's open (mirrors live: eval at end of
                # bar, fill at next open).
                if self.close_strategies and position != 0 and avg_cost > 0:
                    pending_close_fraction = self._evaluate_close_strategies(
                        position, avg_cost, initial_quantity, entry_atr_value,
                        mark_price, atr_series, idx,
                    )
                continue

            if signal == 1 and position == 0:
                # BUY — go long with all available cash
                effective_price = fill_price * (1 + self.slippage_pct)
                commission = cash * self.commission_pct
                available = cash - commission
                shares = available / effective_price
                position = shares
                cash = 0.0

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares

            elif signal == -1 and position > 0:
                # SELL — close long position
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash = proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    trades.append(current_trade)
                    current_trade = None

        # Close any open position at the end
        if position != 0:
            if position > 0:
                final_price = df["close"].iloc[-1] * (1 - self.slippage_pct)
                proceeds = position * final_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
            else:
                final_price = df["close"].iloc[-1] * (1 + self.slippage_pct)
                cost = abs(position) * final_price
                commission = cost * self.commission_pct
                cash -= cost + commission
            position = 0.0

            if current_trade:
                current_trade.close(df.index[-1], final_price)
                trades.append(current_trade)

        final_equity = cash
        equity_df = pd.DataFrame(equity_curve).set_index("date")

        # Calculate metrics
        metrics = self._calculate_metrics(equity_df, trades, df, timeframe)
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

    def _stamp_entry_atr(self, atr_series: Optional[pd.Series], idx,
                         entry_price: float) -> float:
        """Return the ATR at ``idx`` for stamping ``Position.EntryATR``.

        Mirrors ``stampEntryATRIfOpened`` in scheduler/main.go: rejects NaN
        and any value greater than 50% of the entry price as a plausibility
        guard. Returns 0.0 when no usable ATR is available — close evaluators
        that require ATR (``tiered_tp_atr``) then fall through with a no-op
        until a position with a valid ATR is opened.
        """
        if atr_series is None or entry_price <= 0:
            return 0.0
        try:
            value = float(atr_series.loc[idx])
        except (KeyError, TypeError, ValueError):
            return 0.0
        if not (value > 0):  # rejects NaN, 0, negative
            return 0.0
        if value > 0.5 * entry_price:
            return 0.0
        return value

    def _evaluate_close_strategies(self, position: float, avg_cost: float,
                                   initial_quantity: float,
                                   entry_atr_value: float,
                                   mark_price: float,
                                   atr_series: Optional[pd.Series],
                                   idx) -> float:
        """Run every configured close evaluator against the simulated position
        and return the max ``close_fraction``. Same max-wins resolution as the
        live composition flow in shared_tools/strategy_composition.py.
        """
        evaluate, _list_strategies = _load_close_registry()
        side = "long" if position > 0 else "short"
        position_dict = {
            "side": side,
            "avg_cost": float(avg_cost),
            "current_quantity": float(abs(position)),
            "initial_quantity": float(initial_quantity or abs(position)),
            "entry_atr": float(entry_atr_value),
        }
        market_dict = {"mark_price": float(mark_price)}
        if atr_series is not None:
            try:
                live_atr = float(atr_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                live_atr = 0.0
            if live_atr > 0:
                market_dict["atr"] = live_atr

        best = 0.0
        for name in self.close_strategies:
            params = self.close_params.get(name)
            result = evaluate(name, position_dict, market_dict, params)
            fraction = float(result.get("close_fraction", 0.0) or 0.0)
            if fraction > best:
                best = fraction
        return min(max(best, 0.0), 1.0)

    def _calculate_metrics(self, equity_df: pd.DataFrame, trades: list,
                           df: pd.DataFrame, timeframe: str = "1d") -> dict:
        """Calculate comprehensive performance metrics."""
        equity = equity_df["equity"]
        ann_factor = math.sqrt(periods_per_year(timeframe))

        # Anchor return + drawdown at initial_capital so seeded runs (where
        # equity[0] reflects the starting_long mark-to-market, not the true
        # pre-trade balance) don't distort the baseline. For non-seeded runs
        # this is a no-op because equity[0] == initial_capital.
        total_return = (equity.iloc[-1] - self.initial_capital) / self.initial_capital

        # Annualized return
        days = (df.index[-1] - df.index[0]).days
        years = max(days / 365.25, 0.01)
        annual_return = (1 + total_return) ** (1 / years) - 1 if total_return > -1 else -1

        # Daily returns for ratio calculations
        daily_returns = equity.pct_change().dropna()

        # Sharpe Ratio — annualized using the timeframe's periods-per-year
        # (sqrt(365*6) for 4h, sqrt(365*24) for 1h, etc.) so sub-daily
        # timeframes don't get inflated by a factor of sqrt(periods_per_day).
        if len(daily_returns) > 1 and daily_returns.std() > 0:
            sharpe = (daily_returns.mean() / daily_returns.std()) * ann_factor
        else:
            sharpe = 0.0

        # Sortino Ratio (only downside deviation)
        downside = daily_returns[daily_returns < 0]
        if len(downside) > 1 and downside.std() > 0:
            sortino = (daily_returns.mean() / downside.std()) * ann_factor
        else:
            sortino = 0.0

        # Max Drawdown — floor the running peak at initial_capital so the
        # baseline is always the true starting balance, not a seeded
        # mark-to-market that may already be below initial_capital.
        cummax_raw = equity.cummax()
        cummax = cummax_raw.where(cummax_raw >= self.initial_capital, self.initial_capital)
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

        # Volatility (annualized) — same timeframe-aware factor as Sharpe.
        volatility = daily_returns.std() * ann_factor if len(daily_returns) > 1 else 0

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
