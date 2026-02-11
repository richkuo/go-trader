#!/usr/bin/env python3
"""
Options strategy backtester.
Replays vol_mean_reversion (and other options strategies) against historical spot data.
Uses Black-Scholes for premium estimation and simulates expiry settlement.

Usage: python3 backtest_options.py [--strategy vol_mean_reversion] [--underlying BTC] [--since 2023-01-01] [--capital 1000]
"""

import sys
import math
import argparse
from datetime import datetime, timedelta
from typing import List, Dict, Optional, Tuple


def fetch_historical_data(underlying: str, since: str, timeframe: str = "1d") -> list:
    """Fetch historical OHLCV data from Binance US."""
    import ccxt
    exchange = ccxt.binanceus({"enableRateLimit": True})
    symbol = f"{underlying}/USDT"
    
    since_ts = exchange.parse8601(f"{since}T00:00:00Z")
    all_candles = []
    
    print(f"Fetching {symbol} {timeframe} data from {since}...")
    while True:
        candles = exchange.fetch_ohlcv(symbol, timeframe, since=since_ts, limit=1000)
        if not candles:
            break
        all_candles.extend(candles)
        since_ts = candles[-1][0] + 1
        if len(candles) < 1000:
            break
    
    print(f"  Fetched {len(all_candles)} candles")
    return all_candles


def black_scholes_price(spot: float, strike: float, dte_days: float, vol: float, 
                         risk_free: float = 0.05, option_type: str = "call") -> float:
    """Black-Scholes option price."""
    if dte_days <= 0 or vol <= 0 or spot <= 0:
        # At expiry: intrinsic value
        if option_type == "call":
            return max(spot - strike, 0)
        else:
            return max(strike - spot, 0)
    
    t = dte_days / 365.0
    d1 = (math.log(spot / strike) + (risk_free + 0.5 * vol**2) * t) / (vol * math.sqrt(t))
    d2 = d1 - vol * math.sqrt(t)
    
    # Standard normal CDF approximation
    def norm_cdf(x):
        return 0.5 * (1 + math.erf(x / math.sqrt(2)))
    
    if option_type == "call":
        price = spot * norm_cdf(d1) - strike * math.exp(-risk_free * t) * norm_cdf(d2)
    else:
        price = strike * math.exp(-risk_free * t) * norm_cdf(-d2) - spot * norm_cdf(-d1)
    
    return max(price, 0)


def calc_historical_vol(closes: list, window: int = 14) -> float:
    """Calculate annualized historical volatility from daily closes."""
    if len(closes) < window + 1:
        return 0.5  # default 50%
    
    returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(-window, 0)]
    variance = sum(r**2 for r in returns) / len(returns)
    return math.sqrt(variance * 365)


def calc_iv_rank(closes: list, recent_window: int = 14) -> float:
    """Calculate IV rank (simplified: recent vol vs historical vol)."""
    if len(closes) < 30:
        return 50.0
    
    returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(1, len(closes))]
    recent_vol = math.sqrt(sum(r**2 for r in returns[-recent_window:]) / recent_window) * math.sqrt(365) * 100
    hist_vol = math.sqrt(sum(r**2 for r in returns) / len(returns)) * math.sqrt(365) * 100
    
    return min(max((recent_vol / max(hist_vol, 0.001)) * 50, 0), 100)


class OptionPosition:
    def __init__(self, option_type: str, action: str, strike: float, expiry_idx: int,
                 premium: float, premium_usd: float, opened_idx: int):
        self.option_type = option_type  # "call" or "put"
        self.action = action            # "buy" or "sell"
        self.strike = strike
        self.expiry_idx = expiry_idx    # index in candle array when this expires
        self.premium = premium          # as fraction of spot
        self.premium_usd = premium_usd
        self.opened_idx = opened_idx
    
    def settlement_pnl(self, spot_at_expiry: float) -> float:
        """Calculate P&L at expiry."""
        if self.option_type == "call":
            intrinsic = max(spot_at_expiry - self.strike, 0)
        else:
            intrinsic = max(self.strike - spot_at_expiry, 0)
        
        if self.action == "sell":
            # Sold option: collected premium, owe intrinsic
            return self.premium_usd - intrinsic
        else:
            # Bought option: paid premium, receive intrinsic
            return intrinsic - self.premium_usd


class OptionsBacktester:
    def __init__(self, initial_capital: float = 1000.0, max_positions: int = 2,
                 check_interval: int = 1):
        self.initial_capital = initial_capital
        self.max_positions = max_positions
        self.check_interval = check_interval  # days between checks
        self.cash = initial_capital
        self.positions: List[OptionPosition] = []
        self.trade_log: List[dict] = []
        self.equity_curve: List[Tuple[str, float]] = []
        self.total_trades = 0
        self.winning_trades = 0
        self.losing_trades = 0
        self.total_premium_collected = 0
        self.total_premium_paid = 0
        self.total_settlement_loss = 0
    
    def run_vol_mean_reversion(self, candles: list, underlying: str) -> dict:
        """Backtest vol_mean_reversion strategy on historical data."""
        closes = [c[4] for c in candles]
        dates = [datetime.utcfromtimestamp(c[0] / 1000).strftime("%Y-%m-%d") for c in candles]
        
        print(f"\nBacktesting vol_mean_reversion on {underlying}")
        print(f"  Period: {dates[0]} to {dates[-1]} ({len(candles)} days)")
        print(f"  Capital: ${self.initial_capital:,.0f}")
        print(f"  Max positions: {self.max_positions}")
        print(f"  Check interval: every {self.check_interval} day(s)")
        print()
        
        lookback = 90  # need 90 days of history for vol calc
        
        for i in range(lookback, len(candles), self.check_interval):
            spot = closes[i]
            date = dates[i]
            hist_closes = closes[max(0, i-90):i+1]
            
            # Check for expired positions
            expired = [p for p in self.positions if p.expiry_idx <= i]
            for pos in expired:
                spot_at_expiry = closes[min(pos.expiry_idx, len(closes)-1)]
                pnl = pos.settlement_pnl(spot_at_expiry)
                self.cash += pnl
                self.total_trades += 1
                
                if pnl >= 0:
                    self.winning_trades += 1
                else:
                    self.losing_trades += 1
                
                if pos.action == "sell":
                    settlement_cost = max(0, -pnl + pos.premium_usd)
                    if settlement_cost > pos.premium_usd:
                        self.total_settlement_loss += (settlement_cost - pos.premium_usd)
                
                self.trade_log.append({
                    "date": date,
                    "event": "expiry",
                    "type": pos.option_type,
                    "action": pos.action,
                    "strike": pos.strike,
                    "spot_at_expiry": spot_at_expiry,
                    "premium_collected": pos.premium_usd if pos.action == "sell" else 0,
                    "pnl": round(pnl, 2),
                    "cash_after": round(self.cash, 2),
                })
            
            self.positions = [p for p in self.positions if p.expiry_idx > i]
            
            # Calculate IV rank
            iv_rank = calc_iv_rank(hist_closes)
            hist_vol = calc_historical_vol(hist_closes)
            
            # Strategy logic
            if len(self.positions) < self.max_positions:
                if iv_rank > 75:
                    # High IV → sell strangle
                    call_strike = round(spot * 1.10, -2)
                    put_strike = round(spot * 0.90, -2)
                    dte = 30
                    expiry_idx = min(i + dte, len(candles) - 1)
                    
                    # Price with Black-Scholes using current vol
                    call_premium = black_scholes_price(spot, call_strike, dte, hist_vol, option_type="call")
                    put_premium = black_scholes_price(spot, put_strike, dte, hist_vol, option_type="put")
                    
                    # Sell call
                    if len(self.positions) < self.max_positions:
                        pos_call = OptionPosition("call", "sell", call_strike, expiry_idx,
                                                   call_premium / spot, call_premium, i)
                        self.positions.append(pos_call)
                        self.cash += call_premium  # collect premium upfront
                        self.total_premium_collected += call_premium
                        self.trade_log.append({
                            "date": date,
                            "event": "open",
                            "type": "call",
                            "action": "sell",
                            "strike": call_strike,
                            "spot": spot,
                            "premium": round(call_premium, 2),
                            "iv_rank": round(iv_rank, 1),
                            "vol": round(hist_vol * 100, 1),
                            "dte": dte,
                        })
                    
                    # Sell put
                    if len(self.positions) < self.max_positions:
                        pos_put = OptionPosition("put", "sell", put_strike, expiry_idx,
                                                  put_premium / spot, put_premium, i)
                        self.positions.append(pos_put)
                        self.cash += put_premium
                        self.total_premium_collected += put_premium
                        self.trade_log.append({
                            "date": date,
                            "event": "open",
                            "type": "put",
                            "action": "sell",
                            "strike": put_strike,
                            "spot": spot,
                            "premium": round(put_premium, 2),
                            "iv_rank": round(iv_rank, 1),
                            "vol": round(hist_vol * 100, 1),
                            "dte": dte,
                        })
                
                elif iv_rank < 25:
                    # Low IV → buy straddle
                    strike = round(spot, -2)
                    dte = 30
                    expiry_idx = min(i + dte, len(candles) - 1)
                    
                    call_premium = black_scholes_price(spot, strike, dte, hist_vol, option_type="call")
                    put_premium = black_scholes_price(spot, strike, dte, hist_vol, option_type="put")
                    
                    total_cost = call_premium + put_premium
                    if total_cost <= self.cash * 0.5:  # don't spend more than 50% on one trade
                        if len(self.positions) < self.max_positions:
                            pos_call = OptionPosition("call", "buy", strike, expiry_idx,
                                                       call_premium / spot, call_premium, i)
                            self.positions.append(pos_call)
                            self.cash -= call_premium
                            self.total_premium_paid += call_premium
                            self.trade_log.append({
                                "date": date,
                                "event": "open",
                                "type": "call",
                                "action": "buy",
                                "strike": strike,
                                "spot": spot,
                                "premium": round(call_premium, 2),
                                "iv_rank": round(iv_rank, 1),
                                "vol": round(hist_vol * 100, 1),
                                "dte": dte,
                            })
                        
                        if len(self.positions) < self.max_positions:
                            pos_put = OptionPosition("put", "buy", strike, expiry_idx,
                                                      put_premium / spot, put_premium, i)
                            self.positions.append(pos_put)
                            self.cash -= put_premium
                            self.total_premium_paid += put_premium
                            self.trade_log.append({
                                "date": date,
                                "event": "open",
                                "type": "put",
                                "action": "buy",
                                "strike": strike,
                                "spot": spot,
                                "premium": round(put_premium, 2),
                                "iv_rank": round(iv_rank, 1),
                                "vol": round(hist_vol * 100, 1),
                                "dte": dte,
                            })
            
            # Mark-to-market for equity curve
            mtm = self.cash
            for pos in self.positions:
                days_left = max(pos.expiry_idx - i, 0)
                current_price = black_scholes_price(spot, pos.strike, days_left, hist_vol, 
                                                     option_type=pos.option_type)
                if pos.action == "sell":
                    mtm -= current_price  # liability
                else:
                    mtm += current_price  # asset
            
            self.equity_curve.append((date, round(mtm, 2)))
        
        # Force-expire remaining positions at last price
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
            self.trade_log.append({
                "date": final_date,
                "event": "force_close",
                "type": pos.option_type,
                "action": pos.action,
                "strike": pos.strike,
                "spot_at_expiry": final_spot,
                "pnl": round(pnl, 2),
                "cash_after": round(self.cash, 2),
            })
        self.positions = []
        
        return self._generate_report(underlying, dates[0], dates[-1], closes[0], closes[-1])
    
    def _generate_report(self, underlying: str, start_date: str, end_date: str,
                          start_price: float, end_price: float) -> dict:
        """Generate backtest results report."""
        final_value = self.cash
        total_return = (final_value - self.initial_capital) / self.initial_capital * 100
        
        # Buy and hold comparison
        buy_hold_return = (end_price - start_price) / start_price * 100
        
        # Drawdown
        peak = self.initial_capital
        max_dd = 0
        for _, equity in self.equity_curve:
            if equity > peak:
                peak = equity
            dd = (peak - equity) / peak * 100
            if dd > max_dd:
                max_dd = dd
        
        # Win rate
        win_rate = (self.winning_trades / self.total_trades * 100) if self.total_trades > 0 else 0
        
        # Annualized return
        days = len(self.equity_curve)
        years = days / 365
        if years > 0 and final_value > 0:
            ann_return = ((final_value / self.initial_capital) ** (1 / years) - 1) * 100
        else:
            ann_return = 0
        
        # Sharpe ratio (simplified)
        if len(self.equity_curve) > 1:
            daily_returns = []
            for i in range(1, len(self.equity_curve)):
                prev_eq = self.equity_curve[i-1][1]
                curr_eq = self.equity_curve[i][1]
                if prev_eq > 0:
                    daily_returns.append((curr_eq - prev_eq) / prev_eq)
            if daily_returns:
                avg_ret = sum(daily_returns) / len(daily_returns)
                std_ret = math.sqrt(sum((r - avg_ret)**2 for r in daily_returns) / len(daily_returns))
                sharpe = (avg_ret / std_ret * math.sqrt(365)) if std_ret > 0 else 0
            else:
                sharpe = 0
        else:
            sharpe = 0
        
        report = {
            "underlying": underlying,
            "strategy": "vol_mean_reversion",
            "period": f"{start_date} to {end_date}",
            "days": days,
            "initial_capital": self.initial_capital,
            "final_value": round(final_value, 2),
            "total_return_pct": round(total_return, 2),
            "annualized_return_pct": round(ann_return, 2),
            "buy_hold_return_pct": round(buy_hold_return, 2),
            "max_drawdown_pct": round(max_dd, 2),
            "total_trades": self.total_trades,
            "winning_trades": self.winning_trades,
            "losing_trades": self.losing_trades,
            "win_rate_pct": round(win_rate, 1),
            "total_premium_collected": round(self.total_premium_collected, 2),
            "total_premium_paid": round(self.total_premium_paid, 2),
            "total_settlement_losses": round(self.total_settlement_loss, 2),
            "sharpe_ratio": round(sharpe, 2),
            "price_start": round(start_price, 2),
            "price_end": round(end_price, 2),
        }
        
        return report


def print_report(report: dict, trade_log: list, equity_curve: list, verbose: bool = False):
    """Pretty-print backtest results."""
    print("\n" + "=" * 60)
    print(f"  OPTIONS BACKTEST: {report['strategy']} on {report['underlying']}")
    print("=" * 60)
    print(f"  Period:        {report['period']} ({report['days']} days)")
    print(f"  {report['underlying']} price:   ${report['price_start']:,.0f} → ${report['price_end']:,.0f}")
    print()
    print(f"  Initial:       ${report['initial_capital']:,.0f}")
    print(f"  Final:         ${report['final_value']:,.2f}")
    print(f"  Return:        {report['total_return_pct']:+.2f}%")
    print(f"  Annualized:    {report['annualized_return_pct']:+.2f}%")
    print(f"  Buy & Hold:    {report['buy_hold_return_pct']:+.2f}%")
    print(f"  Max Drawdown:  {report['max_drawdown_pct']:.2f}%")
    print(f"  Sharpe Ratio:  {report['sharpe_ratio']:.2f}")
    print()
    print(f"  Trades:        {report['total_trades']} ({report['winning_trades']}W / {report['losing_trades']}L)")
    print(f"  Win Rate:      {report['win_rate_pct']:.1f}%")
    print(f"  Premium In:    ${report['total_premium_collected']:,.2f}")
    print(f"  Premium Out:   ${report['total_premium_paid']:,.2f}")
    print(f"  Settlement ↓:  ${report['total_settlement_losses']:,.2f}")
    print("=" * 60)
    
    if verbose:
        print("\n--- Trade Log ---")
        for t in trade_log:
            if t["event"] == "open":
                print(f"  [{t['date']}] {t['action'].upper()} {t['type']} @ strike ${t['strike']:,.0f} "
                      f"(spot ${t['spot']:,.0f}) premium=${t['premium']:.2f} IV={t['iv_rank']:.0f} vol={t['vol']:.0f}%")
            elif t["event"] in ("expiry", "force_close"):
                tag = "EXPIRY" if t["event"] == "expiry" else "CLOSE"
                print(f"  [{t['date']}] {tag} {t['action']} {t['type']} strike=${t['strike']:,.0f} "
                      f"spot=${t['spot_at_expiry']:,.0f} P&L=${t['pnl']:+,.2f}")
    
    # Mini equity curve (sample 20 points)
    if equity_curve:
        print("\n--- Equity Curve (sampled) ---")
        step = max(1, len(equity_curve) // 20)
        for i in range(0, len(equity_curve), step):
            date, eq = equity_curve[i]
            bar_len = max(0, int((eq / report['initial_capital'] - 0.5) * 40))
            bar = "█" * min(bar_len, 50)
            print(f"  {date}  ${eq:>10,.2f}  {bar}")
        # Always show last
        if len(equity_curve) > 1:
            date, eq = equity_curve[-1]
            bar_len = max(0, int((eq / report['initial_capital'] - 0.5) * 40))
            bar = "█" * min(bar_len, 50)
            print(f"  {date}  ${eq:>10,.2f}  {bar}")


def main():
    parser = argparse.ArgumentParser(description="Options Strategy Backtester")
    parser.add_argument("--strategy", "-s", default="vol_mean_reversion",
                        choices=["vol_mean_reversion"],
                        help="Options strategy to backtest")
    parser.add_argument("--underlying", "-u", default="BTC",
                        help="Underlying asset (BTC, ETH)")
    parser.add_argument("--since", default="2023-01-01",
                        help="Start date (YYYY-MM-DD)")
    parser.add_argument("--capital", type=float, default=1000.0,
                        help="Starting capital")
    parser.add_argument("--max-positions", type=int, default=2,
                        help="Max concurrent positions per strategy")
    parser.add_argument("--check-interval", type=int, default=1,
                        help="Days between strategy checks")
    parser.add_argument("--verbose", "-v", action="store_true",
                        help="Show individual trades")
    args = parser.parse_args()
    
    candles = fetch_historical_data(args.underlying, args.since)
    if not candles or len(candles) < 100:
        print("Not enough data for backtest")
        sys.exit(1)
    
    bt = OptionsBacktester(
        initial_capital=args.capital,
        max_positions=args.max_positions,
        check_interval=args.check_interval,
    )
    
    report = bt.run_vol_mean_reversion(candles, args.underlying)
    print_report(report, bt.trade_log, bt.equity_curve, verbose=args.verbose)
    
    return report


if __name__ == "__main__":
    main()
