#!/usr/bin/env python3
"""
Backtest theta harvesting strategies.
Compares: conservative (60% target) vs aggressive (40% target) vs no theta harvest (hold to expiry).

Usage: python3 backtest_theta.py [--underlying BTC] [--since 2023-01-01] [--capital 1000]
"""

import sys
import math
import argparse
from datetime import datetime
from typing import List, Tuple
from backtest_options import (
    fetch_historical_data, black_scholes_price, calc_historical_vol,
    calc_iv_rank, OptionPosition
)


class ThetaHarvestBacktester:
    def __init__(self, initial_capital: float = 1000.0, max_positions: int = 2,
                 profit_target_pct: float = 0, stop_loss_pct: float = 0,
                 min_dte_close: float = 0, label: str = ""):
        self.initial_capital = initial_capital
        self.max_positions = max_positions
        self.profit_target_pct = profit_target_pct  # 0 = disabled (hold to expiry)
        self.stop_loss_pct = stop_loss_pct
        self.min_dte_close = min_dte_close
        self.label = label
        self.cash = initial_capital
        self.positions: List[OptionPosition] = []
        self.trade_log: List[dict] = []
        self.equity_curve: List[Tuple[str, float]] = []
        self.total_trades = 0
        self.winning_trades = 0
        self.losing_trades = 0
        self.total_premium_collected = 0.0
        self.total_premium_paid = 0.0
        self.early_closes = 0
        self.stop_losses = 0
        self.dte_closes = 0

    def _check_early_exit(self, pos: OptionPosition, spot: float, current_idx: int,
                          hist_vol: float, date: str) -> bool:
        """Check if a position should be closed early. Returns True if closed."""
        if pos.action != "sell":
            return False

        days_left = max(pos.expiry_idx - current_idx, 0)
        
        # Current option price (what it would cost to buy back)
        current_price = black_scholes_price(spot, pos.strike, days_left, hist_vol,
                                            option_type=pos.option_type)
        
        entry_premium = pos.premium_usd
        if entry_premium <= 0:
            return False

        # Profit captured = premium collected - cost to buy back
        profit_usd = entry_premium - current_price
        profit_pct = (profit_usd / entry_premium) * 100

        # Profit target
        if self.profit_target_pct > 0 and profit_pct >= self.profit_target_pct:
            self.cash -= current_price  # buy back
            pnl = profit_usd
            self.total_trades += 1
            self.early_closes += 1
            if pnl >= 0:
                self.winning_trades += 1
            else:
                self.losing_trades += 1
            self.trade_log.append({
                "date": date, "event": "theta_harvest",
                "type": pos.option_type, "action": "close_sell",
                "strike": pos.strike, "spot": spot,
                "buyback_cost": round(current_price, 2),
                "entry_premium": round(entry_premium, 2),
                "profit_pct": round(profit_pct, 1),
                "pnl": round(pnl, 2),
                "cash_after": round(self.cash, 2),
                "reason": f"profit_target ({self.profit_target_pct}%)",
            })
            return True

        # Stop loss
        if self.stop_loss_pct > 0 and profit_pct < 0:
            loss_pct = -profit_pct
            if loss_pct >= self.stop_loss_pct:
                self.cash -= current_price
                pnl = profit_usd
                self.total_trades += 1
                self.stop_losses += 1
                self.losing_trades += 1
                self.trade_log.append({
                    "date": date, "event": "stop_loss",
                    "type": pos.option_type, "action": "close_sell",
                    "strike": pos.strike, "spot": spot,
                    "buyback_cost": round(current_price, 2),
                    "entry_premium": round(entry_premium, 2),
                    "loss_pct": round(loss_pct, 1),
                    "pnl": round(pnl, 2),
                    "cash_after": round(self.cash, 2),
                    "reason": f"stop_loss ({self.stop_loss_pct}%)",
                })
                return True

        # DTE floor
        if self.min_dte_close > 0 and days_left <= self.min_dte_close:
            self.cash -= current_price
            pnl = entry_premium - current_price
            self.total_trades += 1
            self.dte_closes += 1
            if pnl >= 0:
                self.winning_trades += 1
            else:
                self.losing_trades += 1
            self.trade_log.append({
                "date": date, "event": "dte_close",
                "type": pos.option_type, "action": "close_sell",
                "strike": pos.strike, "spot": spot,
                "buyback_cost": round(current_price, 2),
                "pnl": round(pnl, 2),
                "days_left": days_left,
                "cash_after": round(self.cash, 2),
                "reason": f"dte_floor ({self.min_dte_close}d)",
            })
            return True

        return False

    def run(self, candles: list, underlying: str) -> dict:
        """Backtest vol_mean_reversion with theta harvesting."""
        closes = [c[4] for c in candles]
        dates = [datetime.utcfromtimestamp(c[0] / 1000).strftime("%Y-%m-%d") for c in candles]
        
        lookback = 90

        for i in range(lookback, len(candles)):
            spot = closes[i]
            date = dates[i]
            hist_closes = closes[max(0, i-90):i+1]
            hist_vol = calc_historical_vol(hist_closes)

            # Check early exits first
            remaining = []
            for pos in self.positions:
                if pos.expiry_idx <= i:
                    # Expired
                    spot_at_expiry = closes[min(pos.expiry_idx, len(closes)-1)]
                    pnl = pos.settlement_pnl(spot_at_expiry)
                    self.cash += pnl
                    self.total_trades += 1
                    if pnl >= 0:
                        self.winning_trades += 1
                    else:
                        self.losing_trades += 1
                    self.trade_log.append({
                        "date": date, "event": "expiry",
                        "type": pos.option_type, "action": pos.action,
                        "strike": pos.strike, "pnl": round(pnl, 2),
                        "cash_after": round(self.cash, 2),
                    })
                elif self._check_early_exit(pos, spot, i, hist_vol, date):
                    pass  # already handled in _check_early_exit
                else:
                    remaining.append(pos)
            self.positions = remaining

            # Open new positions (same logic as vol_mean_reversion)
            iv_rank = calc_iv_rank(hist_closes)
            
            if len(self.positions) < self.max_positions and iv_rank > 75:
                call_strike = round(spot * 1.10, -2)
                put_strike = round(spot * 0.90, -2)
                dte = 30
                expiry_idx = min(i + dte, len(candles) - 1)

                call_premium = black_scholes_price(spot, call_strike, dte, hist_vol, option_type="call")
                put_premium = black_scholes_price(spot, put_strike, dte, hist_vol, option_type="put")

                if len(self.positions) < self.max_positions:
                    pos_call = OptionPosition("call", "sell", call_strike, expiry_idx,
                                              call_premium / spot, call_premium, i)
                    self.positions.append(pos_call)
                    self.cash += call_premium
                    self.total_premium_collected += call_premium
                    self.trade_log.append({
                        "date": date, "event": "open", "type": "call", "action": "sell",
                        "strike": call_strike, "spot": spot, "premium": round(call_premium, 2),
                        "iv_rank": round(iv_rank, 1), "dte": dte,
                    })

                if len(self.positions) < self.max_positions:
                    pos_put = OptionPosition("put", "sell", put_strike, expiry_idx,
                                             put_premium / spot, put_premium, i)
                    self.positions.append(pos_put)
                    self.cash += put_premium
                    self.total_premium_collected += put_premium
                    self.trade_log.append({
                        "date": date, "event": "open", "type": "put", "action": "sell",
                        "strike": put_strike, "spot": spot, "premium": round(put_premium, 2),
                        "iv_rank": round(iv_rank, 1), "dte": dte,
                    })

            elif len(self.positions) < self.max_positions and iv_rank < 25:
                strike = round(spot, -2)
                dte = 30
                expiry_idx = min(i + dte, len(candles) - 1)
                call_premium = black_scholes_price(spot, strike, dte, hist_vol, option_type="call")
                put_premium = black_scholes_price(spot, strike, dte, hist_vol, option_type="put")
                total_cost = call_premium + put_premium

                if total_cost <= self.cash * 0.5:
                    if len(self.positions) < self.max_positions:
                        pos_call = OptionPosition("call", "buy", strike, expiry_idx,
                                                   call_premium / spot, call_premium, i)
                        self.positions.append(pos_call)
                        self.cash -= call_premium
                        self.total_premium_paid += call_premium

                    if len(self.positions) < self.max_positions:
                        pos_put = OptionPosition("put", "buy", strike, expiry_idx,
                                                  put_premium / spot, put_premium, i)
                        self.positions.append(pos_put)
                        self.cash -= put_premium
                        self.total_premium_paid += put_premium

            # Mark-to-market
            mtm = self.cash
            for pos in self.positions:
                days_left = max(pos.expiry_idx - i, 0)
                current_price = black_scholes_price(spot, pos.strike, days_left, hist_vol,
                                                     option_type=pos.option_type)
                if pos.action == "sell":
                    mtm -= current_price
                else:
                    mtm += current_price
            self.equity_curve.append((date, round(mtm, 2)))

        # Force-close remaining
        final_spot = closes[-1]
        final_date = dates[-1]
        for pos in self.positions:
            pnl = pos.settlement_pnl(final_spot)
            self.cash += pnl
            self.total_trades += 1
            if pnl >= 0:
                self.winning_trades += 1
            else:
                self.losing_trades += 1
        self.positions = []

        return self._report(underlying, dates[lookback], dates[-1], closes[lookback], closes[-1])

    def _report(self, underlying, start_date, end_date, start_price, end_price) -> dict:
        final_value = self.cash
        total_return = (final_value - self.initial_capital) / self.initial_capital * 100
        buy_hold_return = (end_price - start_price) / start_price * 100

        peak = self.initial_capital
        max_dd = 0
        for _, eq in self.equity_curve:
            if eq > peak:
                peak = eq
            dd = (peak - eq) / peak * 100
            if dd > max_dd:
                max_dd = dd

        win_rate = (self.winning_trades / self.total_trades * 100) if self.total_trades > 0 else 0

        days = len(self.equity_curve)
        years = days / 365
        ann_return = ((final_value / self.initial_capital) ** (1 / years) - 1) * 100 if years > 0 and final_value > 0 else 0

        # Sharpe
        sharpe = 0
        if len(self.equity_curve) > 1:
            rets = []
            for j in range(1, len(self.equity_curve)):
                prev = self.equity_curve[j-1][1]
                curr = self.equity_curve[j][1]
                if prev > 0:
                    rets.append((curr - prev) / prev)
            if rets:
                avg = sum(rets) / len(rets)
                std = math.sqrt(sum((r - avg)**2 for r in rets) / len(rets))
                sharpe = (avg / std * math.sqrt(365)) if std > 0 else 0

        return {
            "label": self.label,
            "underlying": underlying,
            "period": f"{start_date} to {end_date}",
            "days": days,
            "initial_capital": self.initial_capital,
            "final_value": round(final_value, 2),
            "total_return_pct": round(total_return, 2),
            "annualized_return_pct": round(ann_return, 2),
            "buy_hold_return_pct": round(buy_hold_return, 2),
            "max_drawdown_pct": round(max_dd, 2),
            "sharpe_ratio": round(sharpe, 2),
            "total_trades": self.total_trades,
            "winning_trades": self.winning_trades,
            "losing_trades": self.losing_trades,
            "win_rate_pct": round(win_rate, 1),
            "total_premium_collected": round(self.total_premium_collected, 2),
            "early_closes": self.early_closes,
            "stop_losses": self.stop_losses,
            "dte_closes": self.dte_closes,
            "profit_target_pct": self.profit_target_pct,
            "stop_loss_pct": self.stop_loss_pct,
            "min_dte_close": self.min_dte_close,
        }


def print_comparison(reports: list):
    """Print side-by-side comparison."""
    print("\n" + "=" * 80)
    print("  THETA HARVESTING BACKTEST COMPARISON")
    print("=" * 80)
    print(f"  {reports[0]['underlying']} | {reports[0]['period']} ({reports[0]['days']} days)")
    print(f"  Buy & Hold: {reports[0]['buy_hold_return_pct']:+.2f}%")
    print()

    # Header
    labels = [r['label'] for r in reports]
    header = f"{'Metric':<28}" + "".join(f"{l:>17}" for l in labels)
    print(header)
    print("-" * len(header))

    rows = [
        ("Final Value", lambda r: f"${r['final_value']:,.2f}"),
        ("Total Return", lambda r: f"{r['total_return_pct']:+.2f}%"),
        ("Annualized Return", lambda r: f"{r['annualized_return_pct']:+.2f}%"),
        ("Max Drawdown", lambda r: f"{r['max_drawdown_pct']:.2f}%"),
        ("Sharpe Ratio", lambda r: f"{r['sharpe_ratio']:.2f}"),
        ("Total Trades", lambda r: f"{r['total_trades']}"),
        ("Win Rate", lambda r: f"{r['win_rate_pct']:.1f}%"),
        ("Premium Collected", lambda r: f"${r['total_premium_collected']:,.2f}"),
        ("Early Closes", lambda r: f"{r['early_closes']}"),
        ("Stop Losses", lambda r: f"{r['stop_losses']}"),
        ("DTE Closes", lambda r: f"{r['dte_closes']}"),
        ("Profit Target", lambda r: f"{r['profit_target_pct']:.0f}%" if r['profit_target_pct'] else "none"),
        ("Stop Loss", lambda r: f"{r['stop_loss_pct']:.0f}%" if r['stop_loss_pct'] else "none"),
        ("Min DTE Close", lambda r: f"{r['min_dte_close']:.0f}d" if r['min_dte_close'] else "none"),
    ]

    for name, fn in rows:
        line = f"  {name:<26}" + "".join(f"{fn(r):>17}" for r in reports)
        print(line)

    print("=" * 80)

    # Winner
    best = max(reports, key=lambda r: r['sharpe_ratio'])
    print(f"\n  🏆 Best risk-adjusted: {best['label']} (Sharpe {best['sharpe_ratio']:.2f})")
    best_return = max(reports, key=lambda r: r['total_return_pct'])
    print(f"  💰 Best return: {best_return['label']} ({best_return['total_return_pct']:+.2f}%)")
    lowest_dd = min(reports, key=lambda r: r['max_drawdown_pct'])
    print(f"  🛡️  Lowest drawdown: {lowest_dd['label']} ({lowest_dd['max_drawdown_pct']:.2f}%)")


def main():
    parser = argparse.ArgumentParser(description="Theta Harvesting Backtest Comparison")
    parser.add_argument("--underlying", "-u", default="BTC", help="BTC or ETH")
    parser.add_argument("--since", default="2023-01-01", help="Start date")
    parser.add_argument("--capital", type=float, default=1000.0, help="Starting capital")
    args = parser.parse_args()

    print(f"Fetching {args.underlying} data...")
    candles = fetch_historical_data(args.underlying, args.since)
    if not candles or len(candles) < 100:
        print("Not enough data")
        sys.exit(1)

    configs = [
        {"label": "No Harvest", "profit_target_pct": 0, "stop_loss_pct": 0, "min_dte_close": 0},
        {"label": "Conservative", "profit_target_pct": 60, "stop_loss_pct": 200, "min_dte_close": 2},
        {"label": "Aggressive", "profit_target_pct": 40, "stop_loss_pct": 150, "min_dte_close": 3},
    ]

    reports = []
    for cfg in configs:
        print(f"\nRunning: {cfg['label']}...")
        bt = ThetaHarvestBacktester(
            initial_capital=args.capital,
            max_positions=2,
            profit_target_pct=cfg["profit_target_pct"],
            stop_loss_pct=cfg["stop_loss_pct"],
            min_dte_close=cfg["min_dte_close"],
            label=cfg["label"],
        )
        report = bt.run(candles, args.underlying)
        reports.append(report)

    print_comparison(reports)


if __name__ == "__main__":
    main()
