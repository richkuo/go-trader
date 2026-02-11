"""
Options Paper Trading Engine — runs options strategies against real Deribit market data.
Defaults to sandbox mode with paper trading.
"""

import time
import signal
import sys
import argparse
import logging
import os
from typing import Optional, Dict, List
from datetime import datetime

from options_adapter import (
    DeribitOptionsAdapter, OptionContract, OptionPosition,
    OptionType, OptionSide
)
from options_risk import OptionsRiskManager, OptionsRiskConfig
from options_strategies import (
    create_options_strategy, list_options_strategies, get_options_strategy,
    BaseOptionsStrategy
)


# ─────────────────────────────────────────────
# Logging setup
# ─────────────────────────────────────────────

def setup_logging(strategy_name: str, underlying: str) -> logging.Logger:
    """Set up logging to file and console."""
    os.makedirs("logs", exist_ok=True)
    timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
    log_file = f"logs/options_{strategy_name}_{underlying}_{timestamp}.log"

    logger = logging.getLogger(f"options_{strategy_name}")
    logger.setLevel(logging.DEBUG)

    # File handler
    fh = logging.FileHandler(log_file)
    fh.setLevel(logging.DEBUG)
    fh.setFormatter(logging.Formatter(
        "%(asctime)s | %(levelname)-7s | %(message)s", datefmt="%Y-%m-%d %H:%M:%S"
    ))
    logger.addHandler(fh)

    # Console handler (INFO only)
    ch = logging.StreamHandler()
    ch.setLevel(logging.INFO)
    ch.setFormatter(logging.Formatter("%(message)s"))
    logger.addHandler(ch)

    logger.info(f"Logging to {log_file}")
    return logger


# ─────────────────────────────────────────────
# Options Trader
# ─────────────────────────────────────────────

class OptionsTrader:
    """
    Options paper trading engine that runs strategies on real Deribit market data.
    """

    def __init__(
        self,
        strategy_name: str,
        underlyings: List[str],
        initial_capital: float = 10000.0,
        strategy_params: Optional[dict] = None,
        risk_config: Optional[OptionsRiskConfig] = None,
        api_key: Optional[str] = None,
        api_secret: Optional[str] = None,
    ):
        self.strategy_name = strategy_name
        self.underlyings = underlyings
        self.running = False
        self.iteration = 0
        self.start_time = None

        # Set up logger
        self.logger = setup_logging(strategy_name, "_".join(underlyings))

        # Exchange adapter (always sandbox for now)
        self.adapter = DeribitOptionsAdapter(
            api_key=api_key,
            api_secret=api_secret,
            sandbox=True,
            initial_balance_usd=initial_capital,
        )

        # Risk manager
        self.risk = OptionsRiskManager(risk_config)
        self.risk.peak_portfolio_value = initial_capital
        self.risk.daily_start_value = initial_capital

        # Strategy instance
        self.strategy = create_options_strategy(
            strategy_name, self.adapter, self.risk, strategy_params
        )

    def start(self, max_iterations: int = 0, sleep_seconds: float = 600.0):
        """
        Start the trading loop.

        Args:
            max_iterations: 0 for infinite, >0 to stop after N iterations
            sleep_seconds: Seconds between each check
        """
        self.running = True
        self.start_time = datetime.utcnow()

        self.logger.info(f"\n{'='*60}")
        self.logger.info(f"  OPTIONS TRADING BOT — SANDBOX PAPER MODE")
        self.logger.info(f"  Strategy:    {self.strategy_name}")
        self.logger.info(f"  Underlyings: {', '.join(self.underlyings)}")
        self.logger.info(f"  Capital:     ${self.adapter.initial_balance_usd:,.2f}")
        self.logger.info(f"  Interval:    {sleep_seconds}s")
        self.logger.info(f"{'='*60}\n")

        # Register signal handlers
        signal.signal(signal.SIGINT, self._handle_shutdown)
        signal.signal(signal.SIGTERM, self._handle_shutdown)

        try:
            while self.running:
                self.iteration += 1
                self._trading_tick()

                if max_iterations > 0 and self.iteration >= max_iterations:
                    self.logger.info(f"\nMax iterations ({max_iterations}) reached. Stopping.")
                    break

                if self.running:
                    time.sleep(sleep_seconds)
        except Exception as e:
            self.logger.error(f"❌ Fatal error: {e}", exc_info=True)
            raise
        finally:
            self._shutdown()

    def _trading_tick(self):
        """Single iteration of the trading loop."""
        now = datetime.utcnow()
        self.logger.debug(f"--- Iteration {self.iteration} @ {now.strftime('%Y-%m-%d %H:%M UTC')} ---")

        # Handle expired options
        self.adapter.handle_expiries()

        # Update position prices
        self.adapter.update_positions()

        # Update risk tracking
        portfolio_value = self.adapter.get_portfolio_value()
        self.risk.update_peak(portfolio_value)
        self.risk.reset_daily(portfolio_value)

        for underlying in self.underlyings:
            try:
                self._process_underlying(underlying)
            except Exception as e:
                self.logger.error(f"  Error processing {underlying}: {e}")

        # Status line
        self._print_status()

    def _process_underlying(self, underlying: str):
        """Process a single underlying: manage positions then evaluate new trades."""
        # 1. Manage existing positions
        mgmt_actions = self.strategy.manage_positions(underlying)
        for action in mgmt_actions:
            self._execute_action(action, underlying)

        # 2. Evaluate new trades
        trade_actions = self.strategy.evaluate(underlying)
        for action in trade_actions:
            self._execute_action(action, underlying)

    def _execute_action(self, action: dict, underlying: str):
        """Execute a strategy action."""
        action_type = action.get("type", "none")

        if action_type == "none":
            self.logger.debug(f"  [{underlying}] {action.get('reason', 'No action')}")
            return

        self.logger.info(f"  [{underlying}] → {action.get('reason', action_type)}")

        try:
            if action_type == "buy_call":
                pos = self.adapter.buy_option(
                    action["contract"], action.get("quantity", 1.0)
                )
                if pos:
                    self.logger.info(f"    ✅ Bought call: {pos.symbol} @ ${pos.entry_price_usd:.2f}")
                    if action.get("is_hedge"):
                        self.risk.record_hedge_spend(pos.entry_price_usd * pos.quantity)
                else:
                    self.logger.warning(f"    ❌ Failed to buy call")

            elif action_type == "buy_put":
                pos = self.adapter.buy_option(
                    action["contract"], action.get("quantity", 1.0)
                )
                if pos:
                    self.logger.info(f"    ✅ Bought put: {pos.symbol} @ ${pos.entry_price_usd:.2f}")
                    if action.get("is_hedge"):
                        self.risk.record_hedge_spend(pos.entry_price_usd * pos.quantity)
                else:
                    self.logger.warning(f"    ❌ Failed to buy put")

            elif action_type == "sell_call":
                pos = self.adapter.sell_option(
                    action["contract"], action.get("quantity", 1.0)
                )
                if pos:
                    self.logger.info(f"    ✅ Sold call: {pos.symbol} @ ${pos.entry_price_usd:.2f}")
                else:
                    self.logger.warning(f"    ❌ Failed to sell call")

            elif action_type == "sell_put":
                pos = self.adapter.sell_option(
                    action["contract"], action.get("quantity", 1.0)
                )
                if pos:
                    self.logger.info(f"    ✅ Sold put: {pos.symbol} @ ${pos.entry_price_usd:.2f}")
                else:
                    self.logger.warning(f"    ❌ Failed to sell put")

            elif action_type == "close":
                result = self.adapter.close_position(action["position_id"])
                if result:
                    self.risk.record_trade_result(result.get("pnl_usd", 0))
                    self.logger.info(f"    ✅ Closed position: PnL ${result.get('pnl_usd', 0):+,.2f}")
                else:
                    self.logger.warning(f"    ❌ Failed to close position")

            elif action_type == "close_group":
                results = self.adapter.close_leg_group(action["leg_group"])
                total_pnl = sum(r.get("pnl_usd", 0) for r in results)
                self.risk.record_trade_result(total_pnl)
                self.logger.info(f"    ✅ Closed leg group: PnL ${total_pnl:+,.2f}")

            elif action_type == "buy_straddle":
                group = self.adapter.open_straddle(
                    action["underlying"],
                    dte_target=action.get("target_dte", 30),
                    side=OptionSide.BUY,
                    quantity=action.get("quantity", 1.0),
                )
                if group:
                    self.logger.info(f"    ✅ Opened long straddle: {group}")
                else:
                    self.logger.warning(f"    ❌ Failed to open straddle")

            elif action_type == "sell_strangle":
                group = self.adapter.open_strangle(
                    action["underlying"],
                    dte_target=action.get("target_dte", 30),
                    side=OptionSide.SELL,
                    quantity=action.get("quantity", 1.0),
                )
                if group:
                    self.logger.info(f"    ✅ Opened short strangle: {group}")
                else:
                    self.logger.warning(f"    ❌ Failed to open strangle")

            elif action_type == "roll":
                # Roll = close old + open new
                pid = action["position_id"]
                pos = self.adapter.get_positions().get(pid)
                if pos:
                    result = self.adapter.close_position(pid)
                    if result:
                        self.risk.record_trade_result(result.get("pnl_usd", 0))
                        self.logger.info(f"    ✅ Closed for roll: PnL ${result.get('pnl_usd', 0):+,.2f}")
                        # Re-evaluate will open new position next iteration

        except Exception as e:
            self.logger.error(f"    ❌ Action failed: {e}")

    def _print_status(self):
        """Print iteration status line."""
        portfolio_value = self.adapter.get_portfolio_value()
        cash = self.adapter.get_cash()
        greeks = self.adapter.get_portfolio_greeks()
        positions = self.adapter.get_open_position_count()
        pnl = portfolio_value - self.adapter.initial_balance_usd
        pnl_pct = (pnl / self.adapter.initial_balance_usd * 100) if self.adapter.initial_balance_usd > 0 else 0

        status = (
            f"[Iter {self.iteration}] "
            f"Portfolio: ${portfolio_value:,.2f} ({pnl:+,.2f} / {pnl_pct:+.2f}%) | "
            f"Cash: ${cash:,.2f} | "
            f"Positions: {positions} | "
            f"Δ: {greeks.delta:+.3f} | "
            f"Θ: ${greeks.theta * portfolio_value / 100:+.2f}/day"
        )
        self.logger.info(status)

        # Log positions detail
        for pid, pos in self.adapter.get_positions().items():
            self.logger.debug(
                f"  {pos.side.value.upper()} {pos.option_type.value} "
                f"{pos.underlying} {pos.strike:.0f} "
                f"({pos.dte:.0f}d) PnL: ${pos.pnl_usd:+,.2f} ({pos.pnl_pct:+.1f}%)"
            )

    def _handle_shutdown(self, signum, frame):
        """Handle graceful shutdown."""
        self.logger.info("\n⚠️  Shutdown signal received...")
        self.running = False

    def _shutdown(self):
        """Clean shutdown — report final state."""
        self.logger.info(self._get_final_report())
        self.logger.info(self.risk.format_status(self.adapter))

    def _get_final_report(self) -> str:
        """Generate final report."""
        portfolio_value = self.adapter.get_portfolio_value()
        pnl = portfolio_value - self.adapter.initial_balance_usd
        pnl_pct = (pnl / self.adapter.initial_balance_usd * 100) if self.adapter.initial_balance_usd > 0 else 0
        trades = self.adapter.get_trade_history()

        lines = [
            f"\n{'='*55}",
            f"  OPTIONS TRADING — FINAL REPORT",
            f"{'='*55}",
            f"  Strategy:      {self.strategy_name}",
            f"  Underlyings:   {', '.join(self.underlyings)}",
            f"  Runtime:       {self.iteration} iterations",
            f"  Portfolio:     ${portfolio_value:,.2f}",
            f"  PnL:           ${pnl:+,.2f} ({pnl_pct:+.2f}%)",
            f"  Cash:          ${self.adapter.get_cash():,.2f}",
            f"  Positions:     {self.adapter.get_open_position_count()}",
            f"  Total Trades:  {len(trades)}",
            f"{'='*55}",
        ]
        return "\n".join(lines)


# ─────────────────────────────────────────────
# CLI
# ─────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="Options Paper Trading Bot — Deribit Sandbox"
    )
    parser.add_argument(
        "--strategy", "-s",
        default="momentum_options",
        choices=list_options_strategies(),
        help="Options strategy to run"
    )
    parser.add_argument(
        "--underlying", "-u",
        default="BTC",
        help="Underlying assets (comma-separated, e.g. BTC,ETH)"
    )
    parser.add_argument(
        "--capital",
        type=float, default=10000.0,
        help="Initial capital in USD"
    )
    parser.add_argument(
        "--interval",
        type=float, default=600,
        help="Check interval in seconds (default: 600 = 10min)"
    )
    parser.add_argument(
        "--max-iterations",
        type=int, default=0,
        help="Max iterations (0 = infinite)"
    )
    parser.add_argument(
        "--max-drawdown",
        type=float, default=20.0,
        help="Max drawdown %% kill switch"
    )
    parser.add_argument(
        "--max-positions",
        type=int, default=10,
        help="Max simultaneous positions"
    )
    parser.add_argument(
        "--max-delta",
        type=float, default=5.0,
        help="Max portfolio delta"
    )
    parser.add_argument(
        "--api-key", default=None,
        help="Deribit API key (optional for sandbox)"
    )
    parser.add_argument(
        "--api-secret", default=None,
        help="Deribit API secret (optional for sandbox)"
    )

    args = parser.parse_args()

    underlyings = [u.strip().upper() for u in args.underlying.split(",")]

    risk_config = OptionsRiskConfig(
        max_drawdown_pct=args.max_drawdown,
        max_positions=args.max_positions,
        max_portfolio_delta=args.max_delta,
        min_portfolio_delta=-args.max_delta,
    )

    trader = OptionsTrader(
        strategy_name=args.strategy,
        underlyings=underlyings,
        initial_capital=args.capital,
        risk_config=risk_config,
        api_key=args.api_key,
        api_secret=args.api_secret,
    )

    trader.start(
        max_iterations=args.max_iterations,
        sleep_seconds=args.interval,
    )


if __name__ == "__main__":
    main()
