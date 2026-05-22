#!/usr/bin/env python3
"""Two-leg pairs backtester.

Simulates a beta-hedged long/short pair (e.g. long ETH + short BTC) driven by a
rolling z-score of the price spread. Each leg is accounted independently:
per-fill fees, hourly funding accrual, mark-to-market P&L, and per-leg
liquidation. Designed for research on whether a spread mean-reversion edge
survives realistic HL costs — not for live execution.

Look-ahead contract mirrors the main backtester (#730/#731): a signal at bar N
fills at bar N+1 open. Z-score uses closed-bar values only.

Not for live execution:
- Quantities are not rounded to exchange lot/tick size, so equity curves here
  will diverge slightly from what HL would actually fill.
- Slippage beyond the next-bar-open assumption is not modeled.
"""

from __future__ import annotations

import argparse
import math
import os
import sys
from dataclasses import dataclass, field
from typing import Optional

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))


SIDE_LONG_A = +1   # long A, short B
SIDE_SHORT_A = -1  # short A, long B
SIDE_FLAT = 0


@dataclass
class PairsTrade:
    entry_bar: int
    entry_time: pd.Timestamp
    side_a: int
    entry_price_a: float
    entry_price_b: float
    qty_a: float
    qty_b: float
    notional_a: float
    notional_b: float
    margin_a: float
    margin_b: float
    exit_bar: Optional[int] = None
    exit_time: Optional[pd.Timestamp] = None
    exit_price_a: Optional[float] = None
    exit_price_b: Optional[float] = None
    pnl_a: float = 0.0
    pnl_b: float = 0.0
    fees: float = 0.0
    funding: float = 0.0
    exit_reason: str = ""

    @property
    def net_pnl(self) -> float:
        return self.pnl_a + self.pnl_b - self.fees + self.funding


@dataclass
class PairsResults:
    trades: list[PairsTrade] = field(default_factory=list)
    equity_curve: pd.Series = field(default_factory=lambda: pd.Series(dtype=float))
    initial_capital: float = 0.0
    final_equity: float = 0.0
    total_return_pct: float = 0.0
    sharpe: float = 0.0
    max_drawdown_pct: float = 0.0
    win_rate: float = 0.0
    avg_hold_bars: float = 0.0
    total_fees: float = 0.0
    total_funding: float = 0.0
    liquidations: int = 0


def _compute_spread(close_a: pd.Series, close_b: pd.Series) -> pd.Series:
    """Log spread = ln(A) - ln(B). Log scale makes the z-score symmetric
    across price levels (a 5% move in either direction shows up the same)."""
    return np.log(close_a) - np.log(close_b)


def _rolling_zscore(series: pd.Series, lookback: int) -> pd.Series:
    mean = series.rolling(window=lookback).mean()
    std = series.rolling(window=lookback).std()
    return (series - mean) / std


def _liquidation_loss(notional: float, leverage: float,
                      maintenance_margin: float) -> float:
    """Loss in dollars on a single leg that triggers liquidation.

    Initial margin = notional / leverage. Liquidation fires when equity drops
    to maintenance_margin × notional. So liquidation loss in $ is:
        (1/leverage − maintenance_margin) × notional
    For 20× isolated with 2% MMR on $1000 notional: (0.05 − 0.02) × 1000 = $30.
    """
    return notional * (1.0 / leverage - maintenance_margin)


class PairsBacktester:
    def __init__(
        self,
        initial_capital: float = 1000.0,
        base_notional: float = 1000.0,
        beta: float = 1.0,
        leverage: float = 20.0,
        maintenance_margin: float = 0.02,
        lookback: int = 30,
        entry_z: float = 2.0,
        exit_z: float = 0.5,
        taker_fee_pct: float = 0.000432,
        maker_fee_pct: float = 0.000144,
        use_maker: bool = False,
        funding_a_per_hour: float = 0.0,
        funding_b_per_hour: float = 0.0,
        bar_hours: float = 1.0,
        bars_per_year: Optional[int] = None,
    ):
        if initial_capital <= 0:
            raise ValueError("initial_capital must be positive")
        if base_notional <= 0:
            raise ValueError("base_notional must be positive")
        if bar_hours <= 0:
            raise ValueError("bar_hours must be positive")
        if entry_z <= exit_z:
            raise ValueError("entry_z must exceed exit_z")
        if leverage <= 0:
            raise ValueError("leverage must be positive")
        if maintenance_margin < 0 or maintenance_margin >= 1.0 / leverage:
            raise ValueError("maintenance_margin must be in [0, 1/leverage)")
        self.initial_capital = float(initial_capital)
        self.base_notional = float(base_notional)
        self.beta = float(beta)
        self.leverage = float(leverage)
        self.maintenance_margin = float(maintenance_margin)
        self.lookback = int(lookback)
        self.entry_z = float(entry_z)
        self.exit_z = float(exit_z)
        self.fee_pct = float(maker_fee_pct if use_maker else taker_fee_pct)
        self.funding_a_per_hour = float(funding_a_per_hour)
        self.funding_b_per_hour = float(funding_b_per_hour)
        self.bar_hours = float(bar_hours)
        # Derive bars_per_year from bar_hours when not explicitly set so Sharpe
        # annualizes correctly on 4h/1d data. 24*365/bar_hours = bars per year.
        if bars_per_year is None:
            self.bars_per_year = int(round(24 * 365 / self.bar_hours))
        else:
            self.bars_per_year = int(bars_per_year)

        # Sanity: total margin shouldn't exceed available capital. Warn but
        # don't reject — caller may be intentionally over-leveraging for stress
        # tests against the liquidation logic.
        total_margin = (self.base_notional + self.base_notional * abs(self.beta)) / self.leverage
        if total_margin > self.initial_capital:
            print(
                f"warning: total margin ${total_margin:.2f} exceeds initial capital "
                f"${self.initial_capital:.2f} — runs as if margin were free",
                file=sys.stderr,
            )

    def run(self, df_a: pd.DataFrame, df_b: pd.DataFrame) -> PairsResults:
        if not df_a.index.equals(df_b.index):
            raise ValueError("df_a and df_b must share an index")
        for col in ("open", "close"):
            if col not in df_a.columns or col not in df_b.columns:
                raise ValueError(f"Both inputs need '{col}' column")

        spread = _compute_spread(df_a["close"], df_b["close"])
        z = _rolling_zscore(spread, self.lookback)

        n = len(df_a)
        equity = self.initial_capital
        equity_curve = np.full(n, np.nan)
        trades: list[PairsTrade] = []
        position: Optional[PairsTrade] = None
        liquidations = 0

        for i in range(n):
            # Mark-to-market + funding accrual on any open position
            if position is not None:
                mark_a = df_a["close"].iat[i]
                mark_b = df_b["close"].iat[i]
                position.pnl_a = position.side_a * position.qty_a * (mark_a - position.entry_price_a)
                position.pnl_b = (-position.side_a) * position.qty_b * (mark_b - position.entry_price_b)

                funding_a = (-position.side_a) * position.notional_a * self.funding_a_per_hour * self.bar_hours
                funding_b = (position.side_a) * position.notional_b * self.funding_b_per_hour * self.bar_hours
                position.funding += funding_a + funding_b

                liq_loss_a = _liquidation_loss(position.notional_a, self.leverage, self.maintenance_margin)
                liq_loss_b = _liquidation_loss(position.notional_b, self.leverage, self.maintenance_margin)
                if (-position.pnl_a) >= liq_loss_a or (-position.pnl_b) >= liq_loss_b:
                    # Cap each leg's loss at the margin posted (HL isolated
                    # mode: insurance fund absorbs anything beyond). On a gap
                    # bar the unconstrained mark-to-market loss can exceed the
                    # isolated margin slot, so without this cap the backtester
                    # overstates drawdown vs. live.
                    if position.pnl_a < 0:
                        position.pnl_a = -min(-position.pnl_a, position.margin_a)
                    if position.pnl_b < 0:
                        position.pnl_b = -min(-position.pnl_b, position.margin_b)
                    position.exit_bar = i
                    position.exit_time = df_a.index[i]
                    position.exit_price_a = mark_a
                    position.exit_price_b = mark_b
                    position.fees += (position.notional_a + position.notional_b) * self.fee_pct
                    position.exit_reason = "liquidation"
                    liquidations += 1
                    equity += position.net_pnl
                    trades.append(position)
                    position = None

            equity_curve[i] = equity + (position.net_pnl if position is not None else 0.0)

            # Signal decision uses closed bar i; fill on bar i+1 open
            if i + 1 >= n:
                continue
            z_now = z.iat[i]
            if math.isnan(z_now):
                continue

            if position is None:
                if z_now >= self.entry_z:
                    position = self._open_position(i + 1, df_a, df_b, SIDE_SHORT_A)
                elif z_now <= -self.entry_z:
                    position = self._open_position(i + 1, df_a, df_b, SIDE_LONG_A)
            else:
                if abs(z_now) <= self.exit_z:
                    self._close_position(position, i + 1, df_a, df_b, "exit_signal")
                    equity += position.net_pnl
                    trades.append(position)
                    position = None

        if position is not None:
            self._close_position(position, n - 1, df_a, df_b, "end_of_data")
            equity += position.net_pnl
            trades.append(position)

        return self._summarize(trades, equity_curve, df_a.index, liquidations)

    def _open_position(self, fill_bar: int, df_a: pd.DataFrame, df_b: pd.DataFrame,
                       side_a: int) -> PairsTrade:
        entry_a = df_a["open"].iat[fill_bar]
        entry_b = df_b["open"].iat[fill_bar]
        notional_a = self.base_notional
        notional_b = self.base_notional * self.beta
        qty_a = notional_a / entry_a
        qty_b = notional_b / entry_b
        margin_a = notional_a / self.leverage
        margin_b = notional_b / self.leverage
        trade = PairsTrade(
            entry_bar=fill_bar,
            entry_time=df_a.index[fill_bar],
            side_a=side_a,
            entry_price_a=entry_a,
            entry_price_b=entry_b,
            qty_a=qty_a,
            qty_b=qty_b,
            notional_a=notional_a,
            notional_b=notional_b,
            margin_a=margin_a,
            margin_b=margin_b,
        )
        trade.fees = (notional_a + notional_b) * self.fee_pct
        return trade

    def _close_position(self, trade: PairsTrade, fill_bar: int,
                        df_a: pd.DataFrame, df_b: pd.DataFrame, reason: str) -> None:
        exit_a = df_a["open"].iat[fill_bar]
        exit_b = df_b["open"].iat[fill_bar]
        trade.exit_bar = fill_bar
        trade.exit_time = df_a.index[fill_bar]
        trade.exit_price_a = exit_a
        trade.exit_price_b = exit_b
        trade.pnl_a = trade.side_a * trade.qty_a * (exit_a - trade.entry_price_a)
        trade.pnl_b = (-trade.side_a) * trade.qty_b * (exit_b - trade.entry_price_b)
        trade.fees += (trade.notional_a + trade.notional_b) * self.fee_pct
        trade.exit_reason = reason

    def _summarize(self, trades: list[PairsTrade], equity_curve: np.ndarray,
                   index: pd.Index, liquidations: int) -> PairsResults:
        eq_series = pd.Series(equity_curve, index=index).ffill().fillna(self.initial_capital)
        returns = eq_series.pct_change().dropna()
        if len(returns) > 1 and returns.std() > 0:
            sharpe = float(returns.mean() / returns.std() * math.sqrt(self.bars_per_year))
        else:
            sharpe = 0.0
        cummax = eq_series.cummax()
        dd = (eq_series - cummax) / cummax
        max_dd = float(dd.min()) if len(dd) else 0.0
        wins = sum(1 for t in trades if t.net_pnl > 0)
        win_rate = (wins / len(trades)) if trades else 0.0
        avg_hold = (
            float(np.mean([(t.exit_bar - t.entry_bar) for t in trades])) if trades else 0.0
        )
        total_fees = sum(t.fees for t in trades)
        total_funding = sum(t.funding for t in trades)
        final_equity = float(eq_series.iloc[-1]) if len(eq_series) else self.initial_capital
        return PairsResults(
            trades=trades,
            equity_curve=eq_series,
            initial_capital=self.initial_capital,
            final_equity=final_equity,
            total_return_pct=(final_equity / self.initial_capital - 1.0) * 100.0,
            sharpe=sharpe,
            max_drawdown_pct=max_dd * 100.0,
            win_rate=win_rate,
            avg_hold_bars=avg_hold,
            total_fees=total_fees,
            total_funding=total_funding,
            liquidations=liquidations,
        )


def format_report(results: PairsResults, symbol_a: str, symbol_b: str,
                  timeframe: str) -> str:
    lines = [
        f"Pairs Backtest: {symbol_a} vs {symbol_b} ({timeframe})",
        "-" * 60,
        f"  Initial capital:   ${results.initial_capital:,.2f}",
        f"  Final equity:      ${results.final_equity:,.2f}",
        f"  Total return:      {results.total_return_pct:+.2f}%",
        f"  Sharpe (ann.):     {results.sharpe:.2f}",
        f"  Max drawdown:      {results.max_drawdown_pct:.2f}%",
        f"  Trades:            {len(results.trades)}",
        f"  Win rate:          {results.win_rate * 100:.1f}%",
        f"  Avg hold (bars):   {results.avg_hold_bars:.1f}",
        f"  Total fees:        ${results.total_fees:,.2f}",
        f"  Total funding:     ${results.total_funding:+,.2f}",
        f"  Liquidations:      {results.liquidations}",
    ]
    return "\n".join(lines)


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description="Two-leg pairs backtester")
    p.add_argument("--symbol-a", default="ETH/USDT")
    p.add_argument("--symbol-b", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--since", default=None, help="ISO date e.g. 2024-11-01")
    p.add_argument("--exchange", default="binanceus")
    p.add_argument("--initial-capital", type=float, default=1000.0)
    p.add_argument("--base-notional", type=float, default=1000.0)
    p.add_argument("--beta", type=float, default=1.2,
                   help="Hedge ratio: notional_B = base_notional × beta")
    p.add_argument("--leverage", type=float, default=20.0)
    p.add_argument("--maintenance-margin", type=float, default=0.02)
    p.add_argument("--lookback", type=int, default=168, help="bars (default 168 = 1 week on 1h)")
    p.add_argument("--entry-z", type=float, default=2.0)
    p.add_argument("--exit-z", type=float, default=0.5)
    p.add_argument("--taker-fee", type=float, default=0.000432)
    p.add_argument("--maker-fee", type=float, default=0.000144)
    p.add_argument("--use-maker", action="store_true")
    p.add_argument("--funding-a-per-hour", type=float, default=0.0)
    p.add_argument("--funding-b-per-hour", type=float, default=0.0)
    p.add_argument("--bar-hours", type=float, default=1.0)
    p.add_argument("--bars-per-year", type=int, default=None,
                   help="override Sharpe annualization (default: 24*365/bar_hours)")
    args = p.parse_args(argv)

    from data_fetcher import fetch_full_history

    since = args.since or "2024-11-01"
    df_a = fetch_full_history(args.symbol_a, args.timeframe, since, args.exchange)
    df_b = fetch_full_history(args.symbol_b, args.timeframe, since, args.exchange)
    common = df_a.index.intersection(df_b.index)
    df_a = df_a.loc[common]
    df_b = df_b.loc[common]
    if df_a.empty:
        print("No overlapping data", file=sys.stderr)
        return 1

    bt = PairsBacktester(
        initial_capital=args.initial_capital,
        base_notional=args.base_notional,
        beta=args.beta,
        leverage=args.leverage,
        maintenance_margin=args.maintenance_margin,
        lookback=args.lookback,
        entry_z=args.entry_z,
        exit_z=args.exit_z,
        taker_fee_pct=args.taker_fee,
        maker_fee_pct=args.maker_fee,
        use_maker=args.use_maker,
        funding_a_per_hour=args.funding_a_per_hour,
        funding_b_per_hour=args.funding_b_per_hour,
        bar_hours=args.bar_hours,
        bars_per_year=args.bars_per_year,
    )
    results = bt.run(df_a, df_b)
    print(format_report(results, args.symbol_a, args.symbol_b, args.timeframe))
    return 0


if __name__ == "__main__":
    sys.exit(main())
