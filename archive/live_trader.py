"""
Live/Paper trading engine.
Runs strategies against real-time market data with full risk management.
Defaults to paper mode — live mode requires explicit --live flag.
"""

import time
import json
import signal
import sys
import argparse
from typing import Optional, Dict, List
from datetime import datetime, timedelta

import pandas as pd

from exchange_adapter import ExchangeAdapter, OrderSide, OrderType
from risk_manager import RiskManager, RiskConfig
from strategies import apply_strategy, get_strategy, list_strategies
from data_fetcher import fetch_ohlcv, get_exchange
from alerts import AlertManager


class LiveTrader:
    """
    Live/Paper trading engine that runs strategies on real market data.
    """

    def __init__(
        self,
        strategy_name: str,
        symbols: List[str],
        timeframe: str = "4h",
        paper_mode: bool = True,
        initial_capital: float = 10000.0,
        strategy_params: Optional[dict] = None,
        risk_config: Optional[RiskConfig] = None,
        api_key: Optional[str] = None,
        api_secret: Optional[str] = None,
    ):
        self.strategy_name = strategy_name
        self.symbols = symbols
        self.timeframe = timeframe
        self.paper_mode = paper_mode
        self.strategy_params = strategy_params
        self.running = False

        # Components
        self.adapter = ExchangeAdapter(
            api_key=api_key,
            api_secret=api_secret,
            paper_mode=paper_mode,
            initial_balance=initial_capital,
        )
        self.risk_manager = RiskManager(risk_config)
        self.risk_manager.peak_portfolio_value = initial_capital
        self.risk_manager.daily_start_value = initial_capital
        self.alerts = AlertManager()

        # State tracking
        self.last_signals: Dict[str, int] = {}
        self.positions: Dict[str, float] = {}
        self.daily_trades: List[dict] = []
        self.start_time = None
        self.iteration = 0

    def start(self, max_iterations: int = 0, sleep_seconds: float = 60.0):
        """
        Start the trading loop.

        Args:
            max_iterations: 0 for infinite, >0 to stop after N iterations
            sleep_seconds: Seconds between each check
        """
        self.running = True
        self.start_time = datetime.utcnow()
        mode = "PAPER" if self.paper_mode else "🔴 LIVE"

        self.alerts.send(
            f"🤖 Trading Bot Started [{mode}]",
            f"Strategy: {self.strategy_name}\n"
            f"Symbols: {', '.join(self.symbols)}\n"
            f"Timeframe: {self.timeframe}\n"
            f"Capital: ${self.adapter.initial_balance:,.2f}",
            level="info"
        )

        # Register signal handlers for graceful shutdown
        signal.signal(signal.SIGINT, self._handle_shutdown)
        signal.signal(signal.SIGTERM, self._handle_shutdown)

        print(f"\n{'='*60}")
        print(f"  TRADING BOT — {mode} MODE")
        print(f"  Strategy: {self.strategy_name}")
        print(f"  Symbols: {', '.join(self.symbols)}")
        print(f"  Timeframe: {self.timeframe}")
        print(f"  Check interval: {sleep_seconds}s")
        print(f"{'='*60}\n")

        try:
            while self.running:
                self.iteration += 1
                self._trading_tick()

                if max_iterations > 0 and self.iteration >= max_iterations:
                    print(f"\nMax iterations ({max_iterations}) reached. Stopping.")
                    break

                if self.running:
                    time.sleep(sleep_seconds)
        except Exception as e:
            self.alerts.send("❌ Bot Error", str(e), level="error")
            raise
        finally:
            self._shutdown()

    def _trading_tick(self):
        """Single iteration of the trading loop."""
        now = datetime.utcnow()
        portfolio_value = self.adapter.get_portfolio_value()
        self.risk_manager.update_peak(portfolio_value)
        self.risk_manager.reset_daily(portfolio_value)

        if self.iteration % 10 == 1:
            print(f"\n[{now.strftime('%Y-%m-%d %H:%M UTC')}] "
                  f"Iteration {self.iteration} | "
                  f"Portfolio: ${portfolio_value:,.2f} | "
                  f"Positions: {self.adapter.get_positions()}")

        for symbol in self.symbols:
            try:
                self._check_symbol(symbol, portfolio_value)
            except Exception as e:
                print(f"  Error checking {symbol}: {e}")

    def _check_symbol(self, symbol: str, portfolio_value: float):
        """Check a single symbol for signals and manage positions."""
        # Fetch recent candles for signal generation
        try:
            df = fetch_ohlcv(symbol, self.timeframe, limit=100, store=False)
        except Exception as e:
            print(f"  Failed to fetch data for {symbol}: {e}")
            return

        if df.empty or len(df) < 30:
            return

        # Apply strategy
        df_signals = apply_strategy(self.strategy_name, df, self.strategy_params)
        latest_signal = df_signals["signal"].iloc[-1]

        # Check for signal change
        prev_signal = self.last_signals.get(symbol, 0)
        self.last_signals[symbol] = latest_signal

        if latest_signal == 0:
            return

        base_currency = symbol.split("/")[0]
        current_price = df["close"].iloc[-1]

        if latest_signal == 1 and base_currency not in self.adapter.get_positions():
            # BUY signal
            risk_check = self.risk_manager.check_can_trade(
                portfolio_value,
                proposed_trade_usd=portfolio_value * 0.15,
                symbol=symbol,
                current_positions=self._get_position_values(),
            )

            if not risk_check["allowed"]:
                print(f"  ⚠️  {symbol} BUY blocked: {risk_check['reason']}")
                return

            # Calculate position size
            stop_loss = self.risk_manager.get_stop_loss_price(current_price, "long")
            position_usd = self.risk_manager.calculate_position_size(
                portfolio_value, current_price, stop_loss
            )
            quantity = position_usd / current_price

            order = self.adapter.place_order(
                symbol, OrderSide.BUY, OrderType.MARKET, quantity
            )

            if order.status.value == "filled":
                self.positions[base_currency] = order.filled_quantity
                msg = (f"🟢 BUY {symbol}: {quantity:.6f} @ ${order.filled_price:,.2f} "
                       f"(${position_usd:,.2f})")
                print(f"  {msg}")
                self.alerts.send(f"Trade: {symbol}", msg, level="trade")
            else:
                print(f"  ❌ {symbol} BUY order failed: {order.status.value}")

        elif latest_signal == -1 and base_currency in self.adapter.get_positions():
            # SELL signal
            qty = self.adapter.get_positions().get(base_currency, 0)
            if qty <= 0:
                return

            order = self.adapter.place_order(
                symbol, OrderSide.SELL, OrderType.MARKET, qty
            )

            if order.status.value == "filled":
                # Calculate PnL
                entry_cost = qty * (current_price * 1.001)  # approximate
                exit_proceeds = qty * order.filled_price
                pnl = exit_proceeds - entry_cost
                self.risk_manager.record_trade_result(pnl)
                self.positions.pop(base_currency, None)

                msg = (f"🔴 SELL {symbol}: {qty:.6f} @ ${order.filled_price:,.2f} "
                       f"PnL: ${pnl:+,.2f}")
                print(f"  {msg}")
                self.alerts.send(f"Trade: {symbol}", msg, level="trade")
            else:
                print(f"  ❌ {symbol} SELL order failed: {order.status.value}")

    def _get_position_values(self) -> Dict[str, float]:
        """Get current positions valued in USDT."""
        result = {}
        for asset, qty in self.adapter.get_positions().items():
            try:
                price = self.adapter.get_price(f"{asset}/USDT")
                result[asset] = qty * price
            except Exception:
                result[asset] = 0
        return result

    def get_daily_report(self) -> str:
        """Generate daily PnL report."""
        portfolio_value = self.adapter.get_portfolio_value()
        daily_pnl = portfolio_value - self.risk_manager.daily_start_value
        daily_pnl_pct = (daily_pnl / self.risk_manager.daily_start_value * 100) if self.risk_manager.daily_start_value > 0 else 0
        total_pnl = portfolio_value - self.adapter.initial_balance
        total_pnl_pct = (total_pnl / self.adapter.initial_balance * 100)

        lines = [
            f"\n{'='*50}",
            f"  DAILY REPORT — {datetime.utcnow().strftime('%Y-%m-%d')}",
            f"{'='*50}",
            f"  Mode:           {'PAPER' if self.paper_mode else 'LIVE'}",
            f"  Strategy:       {self.strategy_name}",
            f"  Portfolio:      ${portfolio_value:,.2f}",
            f"  Daily PnL:      ${daily_pnl:+,.2f} ({daily_pnl_pct:+.2f}%)",
            f"  Total PnL:      ${total_pnl:+,.2f} ({total_pnl_pct:+.2f}%)",
            f"  Positions:      {self.adapter.get_positions()}",
            f"  Trades Today:   {len(self.adapter.get_trade_history())}",
            f"{'='*50}",
        ]
        return "\n".join(lines)

    def _handle_shutdown(self, signum, frame):
        """Handle graceful shutdown on SIGINT/SIGTERM."""
        print("\n\n⚠️  Shutdown signal received...")
        self.running = False

    def _shutdown(self):
        """Clean shutdown — report final state."""
        report = self.get_daily_report()
        print(report)
        self.alerts.send("🛑 Bot Stopped", report, level="info")
        print(self.risk_manager.format_status())


def main():
    parser = argparse.ArgumentParser(description="Crypto Trading Bot — Live/Paper Trading")
    parser.add_argument("--strategy", "-s", default="macd", choices=list_strategies(),
                        help="Strategy to run")
    parser.add_argument("--symbols", nargs="+", default=["BTC/USDT", "ETH/USDT"],
                        help="Trading pairs")
    parser.add_argument("--timeframe", "-tf", default="4h",
                        help="Candle timeframe")
    parser.add_argument("--capital", type=float, default=10000.0,
                        help="Initial capital (paper mode)")
    parser.add_argument("--live", action="store_true",
                        help="⚠️  Enable LIVE trading (requires API keys)")
    parser.add_argument("--api-key", default=None, help="Exchange API key")
    parser.add_argument("--api-secret", default=None, help="Exchange API secret")
    parser.add_argument("--interval", type=float, default=300,
                        help="Check interval in seconds (default: 300 = 5min)")
    parser.add_argument("--max-iterations", type=int, default=0,
                        help="Max iterations (0 = infinite)")
    parser.add_argument("--max-drawdown", type=float, default=15.0,
                        help="Max drawdown %% kill switch")
    parser.add_argument("--daily-loss-limit", type=float, default=5.0,
                        help="Daily loss limit %%")
    args = parser.parse_args()

    if args.live and (not args.api_key or not args.api_secret):
        print("❌ Live mode requires --api-key and --api-secret")
        sys.exit(1)

    if args.live:
        print("\n⚠️  WARNING: LIVE TRADING MODE")
        print("Real money will be used. Press Ctrl+C within 5 seconds to cancel.")
        time.sleep(5)

    risk_config = RiskConfig(
        max_drawdown_pct=args.max_drawdown,
        daily_loss_limit_pct=args.daily_loss_limit,
    )

    trader = LiveTrader(
        strategy_name=args.strategy,
        symbols=args.symbols,
        timeframe=args.timeframe,
        paper_mode=not args.live,
        initial_capital=args.capital,
        risk_config=risk_config,
        api_key=args.api_key,
        api_secret=args.api_secret,
    )

    trader.start(max_iterations=args.max_iterations, sleep_seconds=args.interval)


if __name__ == "__main__":
    main()
