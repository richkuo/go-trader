"""
Exchange adapter — unified interface to Binance US via ccxt.
Supports market, limit, and stop-loss orders.
Paper trading mode with real market data.
"""

import time
import json
import os
from typing import Optional, Dict, List, Any
from datetime import datetime
from enum import Enum

import ccxt
import pandas as pd

from storage import DB_PATH


class OrderSide(str, Enum):
    BUY = "buy"
    SELL = "sell"


class OrderType(str, Enum):
    MARKET = "market"
    LIMIT = "limit"
    STOP_LOSS = "stop_loss"
    STOP_LIMIT = "stop_limit"


class OrderStatus(str, Enum):
    PENDING = "pending"
    OPEN = "open"
    FILLED = "filled"
    CANCELLED = "cancelled"
    FAILED = "failed"


class Order:
    """Represents a trading order."""
    _id_counter = 0

    def __init__(self, symbol: str, side: OrderSide, order_type: OrderType,
                 quantity: float, price: Optional[float] = None,
                 stop_price: Optional[float] = None):
        Order._id_counter += 1
        self.id = f"order_{Order._id_counter}"
        self.symbol = symbol
        self.side = side
        self.order_type = order_type
        self.quantity = quantity
        self.price = price
        self.stop_price = stop_price
        self.status = OrderStatus.PENDING
        self.filled_price = None
        self.filled_quantity = 0.0
        self.commission = 0.0
        self.created_at = datetime.utcnow()
        self.filled_at = None
        self.exchange_order_id = None

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "symbol": self.symbol,
            "side": self.side.value,
            "type": self.order_type.value,
            "quantity": self.quantity,
            "price": self.price,
            "stop_price": self.stop_price,
            "status": self.status.value,
            "filled_price": self.filled_price,
            "filled_quantity": self.filled_quantity,
            "commission": self.commission,
            "created_at": str(self.created_at),
            "filled_at": str(self.filled_at) if self.filled_at else None,
            "exchange_order_id": self.exchange_order_id,
        }


class ExchangeAdapter:
    """
    Unified exchange adapter supporting both paper and live trading.
    Uses Binance US via ccxt.
    """

    def __init__(self, api_key: Optional[str] = None, api_secret: Optional[str] = None,
                 paper_mode: bool = True, exchange_id: str = "binanceus",
                 initial_balance: float = 10000.0):
        self.exchange_id = exchange_id
        self.paper_mode = paper_mode
        self.initial_balance = initial_balance

        # Paper trading state
        self._paper_balance = {"USDT": initial_balance}
        self._paper_positions: Dict[str, float] = {}
        self._paper_orders: List[Order] = []
        self._paper_trades: List[dict] = []

        # Real exchange connection
        exchange_class = getattr(ccxt, exchange_id)
        config = {"enableRateLimit": True}
        if api_key and api_secret and not paper_mode:
            config["apiKey"] = api_key
            config["secret"] = api_secret
        self.exchange = exchange_class(config)

    @property
    def mode_str(self) -> str:
        return "PAPER" if self.paper_mode else "LIVE"

    def get_ticker(self, symbol: str) -> dict:
        """Get current ticker (bid/ask/last) from exchange."""
        return self.exchange.fetch_ticker(symbol)

    def get_price(self, symbol: str) -> float:
        """Get current last price."""
        ticker = self.get_ticker(symbol)
        return ticker["last"]

    def get_orderbook(self, symbol: str, limit: int = 10) -> dict:
        """Get order book."""
        return self.exchange.fetch_order_book(symbol, limit)

    def get_balance(self) -> Dict[str, float]:
        """Get account balances."""
        if self.paper_mode:
            return dict(self._paper_balance)
        balance = self.exchange.fetch_balance()
        return {k: v for k, v in balance["free"].items() if v > 0}

    def get_positions(self) -> Dict[str, float]:
        """Get open positions."""
        if self.paper_mode:
            return {k: v for k, v in self._paper_positions.items() if v > 0}
        # For spot, positions are just non-zero balances
        balance = self.exchange.fetch_balance()
        return {k: v for k, v in balance["free"].items()
                if v > 0 and k != "USDT"}

    def place_order(self, symbol: str, side: OrderSide, order_type: OrderType,
                    quantity: float, price: Optional[float] = None,
                    stop_price: Optional[float] = None) -> Order:
        """
        Place an order (paper or live).

        Args:
            symbol: Trading pair (e.g., 'BTC/USDT')
            side: buy or sell
            order_type: market, limit, stop_loss, stop_limit
            quantity: Amount of base currency
            price: Limit price (required for limit/stop_limit)
            stop_price: Stop trigger price (required for stop_loss/stop_limit)

        Returns:
            Order object
        """
        order = Order(symbol, side, order_type, quantity, price, stop_price)

        if self.paper_mode:
            return self._execute_paper_order(order)
        else:
            return self._execute_live_order(order)

    def _execute_paper_order(self, order: Order) -> Order:
        """Simulate order execution with current market prices."""
        try:
            current_price = self.get_price(order.symbol)
        except Exception as e:
            order.status = OrderStatus.FAILED
            return order

        # Determine fill price based on order type
        if order.order_type == OrderType.MARKET:
            slippage = 0.0005  # 0.05% slippage
            if order.side == OrderSide.BUY:
                fill_price = current_price * (1 + slippage)
            else:
                fill_price = current_price * (1 - slippage)
        elif order.order_type == OrderType.LIMIT:
            if order.price is None:
                order.status = OrderStatus.FAILED
                return order
            # In paper mode, assume limit fills if price is favorable
            if order.side == OrderSide.BUY and order.price >= current_price:
                fill_price = min(order.price, current_price)
            elif order.side == OrderSide.SELL and order.price <= current_price:
                fill_price = max(order.price, current_price)
            else:
                # Price not yet reached — keep as open order
                order.status = OrderStatus.OPEN
                self._paper_orders.append(order)
                return order
        elif order.order_type in (OrderType.STOP_LOSS, OrderType.STOP_LIMIT):
            # Stop orders are pending until stop price is hit
            order.status = OrderStatus.OPEN
            self._paper_orders.append(order)
            return order
        else:
            fill_price = current_price

        # Execute the fill
        base_currency = order.symbol.split("/")[0]
        quote_currency = order.symbol.split("/")[1]
        commission_rate = 0.001  # 0.1%

        if order.side == OrderSide.BUY:
            cost = order.quantity * fill_price
            commission = cost * commission_rate
            total_cost = cost + commission
            if self._paper_balance.get(quote_currency, 0) < total_cost:
                order.status = OrderStatus.FAILED
                return order
            self._paper_balance[quote_currency] = self._paper_balance.get(quote_currency, 0) - total_cost
            self._paper_positions[base_currency] = self._paper_positions.get(base_currency, 0) + order.quantity
        else:
            if self._paper_positions.get(base_currency, 0) < order.quantity:
                order.status = OrderStatus.FAILED
                return order
            proceeds = order.quantity * fill_price
            commission = proceeds * commission_rate
            self._paper_positions[base_currency] = self._paper_positions.get(base_currency, 0) - order.quantity
            self._paper_balance[quote_currency] = self._paper_balance.get(quote_currency, 0) + proceeds - commission

        order.filled_price = fill_price
        order.filled_quantity = order.quantity
        order.commission = commission_rate * order.quantity * fill_price
        order.status = OrderStatus.FILLED
        order.filled_at = datetime.utcnow()

        self._paper_trades.append(order.to_dict())
        return order

    def _execute_live_order(self, order: Order) -> Order:
        """Execute order on real exchange."""
        if self.paper_mode:
            raise RuntimeError("Cannot execute live order in paper mode")

        try:
            if order.order_type == OrderType.MARKET:
                result = self.exchange.create_order(
                    order.symbol, "market", order.side.value, order.quantity
                )
            elif order.order_type == OrderType.LIMIT:
                result = self.exchange.create_order(
                    order.symbol, "limit", order.side.value, order.quantity, order.price
                )
            elif order.order_type == OrderType.STOP_LOSS:
                result = self.exchange.create_order(
                    order.symbol, "stop_loss", order.side.value, order.quantity,
                    None, {"stopPrice": order.stop_price}
                )
            elif order.order_type == OrderType.STOP_LIMIT:
                result = self.exchange.create_order(
                    order.symbol, "stop_limit", order.side.value, order.quantity,
                    order.price, {"stopPrice": order.stop_price}
                )
            else:
                order.status = OrderStatus.FAILED
                return order

            order.exchange_order_id = result.get("id")
            order.status = OrderStatus.FILLED if result.get("status") == "closed" else OrderStatus.OPEN
            order.filled_price = result.get("average") or result.get("price")
            order.filled_quantity = result.get("filled", 0)
            order.commission = result.get("fee", {}).get("cost", 0)
            if order.status == OrderStatus.FILLED:
                order.filled_at = datetime.utcnow()

        except Exception as e:
            order.status = OrderStatus.FAILED
            print(f"[{self.mode_str}] Order failed: {e}")

        return order

    def cancel_order(self, order_id: str, symbol: str = "") -> bool:
        """Cancel an open order."""
        if self.paper_mode:
            for o in self._paper_orders:
                if o.id == order_id and o.status == OrderStatus.OPEN:
                    o.status = OrderStatus.CANCELLED
                    return True
            return False
        else:
            try:
                self.exchange.cancel_order(order_id, symbol)
                return True
            except Exception:
                return False

    def get_open_orders(self, symbol: Optional[str] = None) -> List[dict]:
        """Get all open orders."""
        if self.paper_mode:
            orders = [o for o in self._paper_orders if o.status == OrderStatus.OPEN]
            if symbol:
                orders = [o for o in orders if o.symbol == symbol]
            return [o.to_dict() for o in orders]
        else:
            return self.exchange.fetch_open_orders(symbol)

    def get_trade_history(self) -> List[dict]:
        """Get trade history."""
        if self.paper_mode:
            return list(self._paper_trades)
        # For live, would fetch from exchange
        return []

    def get_portfolio_value(self, quote_currency: str = "USDT") -> float:
        """Calculate total portfolio value in quote currency."""
        if self.paper_mode:
            total = self._paper_balance.get(quote_currency, 0)
            for asset, qty in self._paper_positions.items():
                if qty > 0:
                    try:
                        price = self.get_price(f"{asset}/{quote_currency}")
                        total += qty * price
                    except Exception:
                        pass
            return total
        else:
            balance = self.exchange.fetch_balance()
            return balance.get("total", {}).get(quote_currency, 0)

    def check_pending_stops(self, symbol: str, current_price: float):
        """Check and execute any triggered stop orders (paper mode)."""
        if not self.paper_mode:
            return
        for order in self._paper_orders:
            if order.status != OrderStatus.OPEN or order.symbol != symbol:
                continue
            if order.order_type == OrderType.STOP_LOSS:
                if order.side == OrderSide.SELL and current_price <= order.stop_price:
                    order.order_type = OrderType.MARKET
                    self._execute_paper_order(order)
                elif order.side == OrderSide.BUY and current_price >= order.stop_price:
                    order.order_type = OrderType.MARKET
                    self._execute_paper_order(order)

    def stream_prices(self, symbol: str, callback, interval_sec: float = 5.0, max_updates: int = 0):
        """
        Stream live prices by polling. Calls callback(symbol, price, timestamp) on each update.
        Set max_updates=0 for infinite streaming.
        """
        count = 0
        while True:
            try:
                price = self.get_price(symbol)
                callback(symbol, price, datetime.utcnow())
                count += 1
                if max_updates > 0 and count >= max_updates:
                    break
            except Exception as e:
                print(f"Stream error: {e}")
            time.sleep(interval_sec)


if __name__ == "__main__":
    # Test paper trading
    adapter = ExchangeAdapter(paper_mode=True, initial_balance=10000)
    print(f"Mode: {adapter.mode_str}")
    print(f"Balance: {adapter.get_balance()}")

    # Get a current price
    try:
        price = adapter.get_price("BTC/USDT")
        print(f"BTC/USDT price: ${price:,.2f}")

        # Place a paper buy
        qty = 0.01
        order = adapter.place_order("BTC/USDT", OrderSide.BUY, OrderType.MARKET, qty)
        print(f"Order: {order.to_dict()}")
        print(f"Balance after buy: {adapter.get_balance()}")
        print(f"Positions: {adapter.get_positions()}")
        print(f"Portfolio value: ${adapter.get_portfolio_value():,.2f}")
    except Exception as e:
        print(f"Exchange test skipped (no connection): {e}")
