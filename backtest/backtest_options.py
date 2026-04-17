#!/usr/bin/env python3
"""
Options strategy backtester.
Replays vol_mean_reversion (and other options strategies) against historical spot data.
Uses Black-Scholes for premium estimation and simulates expiry settlement.

Usage: python3 backtest_options.py [--strategy vol_mean_reversion] [--underlying BTC] [--since 2023-01-01] [--capital 1000]
"""

import os
import sys
import math
import argparse
from datetime import datetime, timedelta
from typing import List, Dict, Optional, Tuple

# Use the same BS pricing used by live adapters (shared_tools/pricing.py) so
# backtest premium ≡ live adapter fallback premium on identical inputs.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
from pricing import bs_price, bs_price_and_greeks  # type: ignore


# Deribit strike-grid granularity per underlying. Matches the fallback in
# platforms/deribit/adapter.py:get_real_strike — BTC rounds to the nearest
# $1000; everything else (ETH, SOL, DOGE, ...) rounds to the nearest $50.
ADAPTER_STRIKE_STEP = {
    "BTC": 1000.0,
}
DEFAULT_STRIKE_STEP = 50.0


def adapter_strike(underlying: str, target_strike: float) -> float:
    """Round ``target_strike`` to the nearest listed strike for ``underlying``.

    Unknown underlyings use the $50 default — same behavior as the live
    Deribit adapter, which only special-cases BTC.
    """
    step = ADAPTER_STRIKE_STEP.get(underlying.upper(), DEFAULT_STRIKE_STEP)
    return round(target_strike / step) * step


SUPPORTED_UNDERLYING_EXCHANGES = ("binanceus", "binance", "okx", "kraken", "coinbase")


def fetch_historical_data(underlying: str, since: str, timeframe: str = "1d",
                          exchange_name: str = "binanceus") -> list:
    """Fetch historical OHLCV data from a CCXT exchange (default BinanceUS).

    Options live on Deribit / OKX / IBKR / Robinhood, but we use a spot
    exchange here only to fetch the *underlying* price series for premium
    pricing. ``exchange_name`` lets callers pick a non-BinanceUS source
    when BinanceUS is geo-blocked or missing the symbol; unknown exchanges
    fall back to BinanceUS with a warning (issue #304 L2).
    """
    import ccxt
    if exchange_name not in SUPPORTED_UNDERLYING_EXCHANGES:
        print(f"[warn] unknown --exchange '{exchange_name}', falling back to binanceus. "
              f"Supported: {SUPPORTED_UNDERLYING_EXCHANGES}")
        exchange_name = "binanceus"
    exchange_cls = getattr(ccxt, exchange_name)
    exchange = exchange_cls({"enableRateLimit": True})
    symbol = f"{underlying}/USDT"

    since_ts = exchange.parse8601(f"{since}T00:00:00Z")
    all_candles = []

    print(f"Fetching {symbol} {timeframe} data from {since} ({exchange_name})...")
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
    """Back-compat wrapper; new code should call ``bs_price_and_greeks``."""
    return bs_price(spot, strike, dte_days, vol, risk_free, option_type)


def calc_historical_vol(closes: list, window: int = 14) -> float:
    """Annualized historical volatility from daily closes.

    Uses log returns (correct for multiplicative price processes) and
    population variance around the sample mean. The previous implementation
    used simple returns with ``sum(r**2) / n`` — the latter is the mean of
    squared returns, which equals variance only when the mean return is
    exactly zero. For crypto over short windows that assumption is false,
    inflating vol and overpricing every Black-Scholes premium.
    """
    if len(closes) < window + 1:
        return 0.5  # default 50%

    log_returns = [
        math.log(closes[i] / closes[i - 1]) for i in range(-window, 0)
    ]
    mean = sum(log_returns) / len(log_returns)
    variance = sum((r - mean) ** 2 for r in log_returns) / len(log_returns)
    return math.sqrt(variance * 365)


def calc_iv_rank(closes: list, recent_window: int = 14,
                  lookback_days: int = 60) -> float:
    """IV rank — percentile of current realised vol within a trailing window.

    Defined as ``(current - min) / (max - min) * 100`` over the past
    ``lookback_days`` days of rolling ``recent_window``-day realised vol,
    matching the shape of ``adapter.get_iv_rank()`` in the live
    ``VolMeanReversionStrategy``.

    The previous implementation returned ``(recent / hist) * 50`` — an IV
    *ratio* halved and clipped, which reached 100 whenever recent vol was
    merely 2× historical vol rather than at a true lookback high. That
    triggered strangles at entirely different moments than live.
    """
    if len(closes) < recent_window + lookback_days + 1:
        return 50.0

    log_returns = [
        math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes))
    ]

    def _rolling_vol(end_idx: int) -> float:
        window = log_returns[end_idx - recent_window + 1: end_idx + 1]
        mean = sum(window) / len(window)
        variance = sum((r - mean) ** 2 for r in window) / len(window)
        return math.sqrt(variance * 365)

    current = _rolling_vol(len(log_returns) - 1)
    history = [
        _rolling_vol(end)
        for end in range(
            len(log_returns) - 1 - lookback_days,
            len(log_returns),
        )
    ]

    lo, hi = min(history), max(history)
    if hi - lo <= 1e-12:
        # Degenerate range — rank is ill-defined, return neutral.
        return 50.0

    rank = (current - lo) / (hi - lo) * 100.0
    return min(max(rank, 0.0), 100.0)


class OptionPosition:
    def __init__(self, option_type: str, action: str, strike: float, expiry_idx: int,
                 premium: float, premium_usd: float, opened_idx: int,
                 greeks: Optional[dict] = None):
        self.option_type = option_type  # "call" or "put"
        self.action = action            # "buy" or "sell"
        self.strike = strike
        self.expiry_idx = expiry_idx    # index in candle array when this expires
        self.premium = premium          # as fraction of spot
        self.premium_usd = premium_usd
        self.opened_idx = opened_idx
        self.greeks = greeks or {"delta": 0.0, "gamma": 0.0, "theta": 0.0, "vega": 0.0}
    
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
        if max_positions < 2:
            # Strangles and straddles open two legs simultaneously; with only
            # one slot the second leg is silently skipped, leaving a naked
            # call/put. Reject upfront rather than producing wrong results
            # (issue #304 M4).
            raise ValueError(
                f"max_positions must be >= 2 for two-legged options "
                f"strategies (strangle/straddle); got {max_positions}"
            )
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
                    # High IV → sell strangle on the adapter's listed-strike grid.
                    call_strike = adapter_strike(underlying, spot * 1.10)
                    put_strike = adapter_strike(underlying, spot * 0.90)
                    dte = 30
                    expiry_idx = min(i + dte, len(candles) - 1)

                    call_premium, call_greeks = bs_price_and_greeks(
                        spot, call_strike, dte, hist_vol, option_type="call"
                    )
                    put_premium, put_greeks = bs_price_and_greeks(
                        spot, put_strike, dte, hist_vol, option_type="put"
                    )

                    if len(self.positions) < self.max_positions:
                        pos_call = OptionPosition(
                            "call", "sell", call_strike, expiry_idx,
                            call_premium / spot, call_premium, i, greeks=call_greeks,
                        )
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
                            "delta": call_greeks["delta"],
                        })

                    if len(self.positions) < self.max_positions:
                        pos_put = OptionPosition(
                            "put", "sell", put_strike, expiry_idx,
                            put_premium / spot, put_premium, i, greeks=put_greeks,
                        )
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
                            "delta": put_greeks["delta"],
                        })

                elif iv_rank < 25:
                    # Low IV → buy straddle at the ATM listed strike.
                    strike = adapter_strike(underlying, spot)
                    dte = 30
                    expiry_idx = min(i + dte, len(candles) - 1)

                    call_premium, call_greeks = bs_price_and_greeks(
                        spot, strike, dte, hist_vol, option_type="call"
                    )
                    put_premium, put_greeks = bs_price_and_greeks(
                        spot, strike, dte, hist_vol, option_type="put"
                    )

                    total_cost = call_premium + put_premium
                    if total_cost <= self.cash * 0.5:  # don't spend more than 50% on one trade
                        if len(self.positions) < self.max_positions:
                            pos_call = OptionPosition(
                                "call", "buy", strike, expiry_idx,
                                call_premium / spot, call_premium, i, greeks=call_greeks,
                            )
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
                                "delta": call_greeks["delta"],
                            })

                        if len(self.positions) < self.max_positions:
                            pos_put = OptionPosition(
                                "put", "buy", strike, expiry_idx,
                                put_premium / spot, put_premium, i, greeks=put_greeks,
                            )
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
                                "delta": put_greeks["delta"],
                            })
            
            # Mark-to-market for equity curve
            mtm = self.cash
            for pos in self.positions:
                days_left = max(pos.expiry_idx - i, 0)
                current_price = bs_price(
                    spot, pos.strike, days_left, hist_vol,
                    option_type=pos.option_type,
                )
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
    
    def _elapsed_days(self) -> int:
        """Calendar days between first and last equity-curve timestamps."""
        if len(self.equity_curve) < 2:
            return 0
        first = datetime.strptime(self.equity_curve[0][0], "%Y-%m-%d")
        last = datetime.strptime(self.equity_curve[-1][0], "%Y-%m-%d")
        return max((last - first).days, 0)

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
        
        # Annualized return — use elapsed calendar days between the first
        # and last equity-curve dates, NOT len(equity_curve). With
        # check_interval=7 the curve only samples weekly, so a 1-year run
        # would report days=52 → years=0.14 → wildly inflated annualized
        # return (issue #304 M5).
        days = self._elapsed_days()
        years = days / 365.0
        if years > 0 and final_value > 0:
            ann_return = ((final_value / self.initial_capital) ** (1 / years) - 1) * 100
        else:
            ann_return = 0

        # Sharpe ratio — annualized using the actual periods-per-year of the
        # equity-curve sampling rate (1 / check_interval per day), not the
        # hardcoded 365 that assumes daily samples (issue #304 M3).
        sample_periods_per_year = 365.0 / max(self.check_interval, 1)
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
                sharpe = (avg_ret / std_ret * math.sqrt(sample_periods_per_year)) if std_ret > 0 else 0
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
    parser.add_argument("--exchange", default="binanceus",
                        choices=SUPPORTED_UNDERLYING_EXCHANGES,
                        help="CCXT exchange for the underlying spot series "
                             "(default binanceus)")
    parser.add_argument("--verbose", "-v", action="store_true",
                        help="Show individual trades")
    args = parser.parse_args()

    candles = fetch_historical_data(args.underlying, args.since,
                                    exchange_name=args.exchange)
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
