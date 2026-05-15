#!/usr/bin/env python3
"""
Phemex perps strategy check script.
Fetches OHLCV from Phemex, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_phemex.py <strategy> <symbol> <timeframe> [--mode=paper|live]

Execution mode (live only, called by Go as phase 2):
    check_phemex.py --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live]
        [--stop-loss-pct=3.0]         # optional: place a reduce-only SL trigger after fill
        [--cancel-stop-loss-oid=OID]  # optional: cancel this trigger OID before the order
        [--prev-pos-qty=0.5]          # optional: existing position qty being flipped
        [--margin-mode=isolated|cross] # optional: enforce margin mode
        [--leverage=N]                # optional: leverage setting
"""

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone


class SafeEncoder(json.JSONEncoder):
    """JSON encoder that converts NaN/Inf to null (Python None)."""

    def default(self, obj):
        return super().default(obj)

    def encode(self, o):
        return super().encode(self._sanitize(o))

    def _sanitize(self, obj):
        if isinstance(obj, float):
            if math.isnan(obj) or math.isinf(obj):
                return None
            return obj
        if isinstance(obj, dict):
            return {k: self._sanitize(v) for k, v in obj.items()}
        if isinstance(obj, (list, tuple)):
            return [self._sanitize(v) for v in obj]
        return obj


# Add paths: platforms/phemex/, shared_strategies/open/futures/, shared_tools/
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'phemex'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'futures'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from regime import latest_regime


def _make_dataframe(candles):
    """Convert raw OHLCV list to pandas DataFrame compatible with strategy functions."""
    import pandas as pd
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def _position_ctx_from_args(args):
    ctx = {}
    side = (args.position_side or "").lower()
    if side:
        ctx["side"] = side
    for attr, key in (
        ("position_avg_cost", "avg_cost"),
        ("position_qty", "current_quantity"),
        ("position_initial_qty", "initial_quantity"),
        ("position_entry_atr", "entry_atr"),
    ):
        value = getattr(args, attr, None)
        if value is not None:
            ctx[key] = value
    return ctx


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params_override=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_period=14, regime_adx_threshold=20.0,
                     close_params_by_name=None):
    """Run strategy signal check using Phemex OHLCV data."""
    try:
        from adapter import PhemexExchangeAdapter
        from strategies import apply_strategy, get_strategy, list_strategies
        from close_registry_loader import (
            evaluate as close_evaluate,
            get_strategy as get_close_strategy,
            list_strategies as list_close_strategies,
        )
        from strategy_composition import (
            evaluate_open_close,
            finalize_decision,
            normalize_signal,
            parse_close_strategies,
            validate_close_strategy_names,
        )

        open_close_enabled = bool(open_strategy or close_strategies)
        configured_names = [open_strategy or strategy_name]
        for name in configured_names:
            get_strategy(name)
        validate_close_strategy_names(
            parse_close_strategies(close_strategies),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        adapter = PhemexExchangeAdapter()

        # Fetch funding rate data for delta-neutral strategy
        strategy_params = {}
        if strategy_name == "delta_neutral_funding":
            try:
                current_rate = adapter.get_funding_rate(symbol)
                history = adapter.get_funding_history(symbol, days=7)
                avg_rate = (sum(r["rate"] for r in history) / len(history)) if history else 0.0
                strategy_params = {
                    "current_funding_rate": current_rate,
                    "avg_funding_rate_7d": avg_rate,
                }
                print(f"Funding rate {symbol}: current={current_rate:.6f} avg7d={avg_rate:.6f}", file=sys.stderr)
            except Exception as e:
                print(f"Warning: failed to fetch funding rate: {e}", file=sys.stderr)

        print(f"Fetching {symbol} {timeframe} from Phemex ({mode})...", file=sys.stderr)
        candles = adapter.get_perp_ohlcv(symbol, interval=timeframe, limit=200)

        if not candles or len(candles) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "error": f"Insufficient data: got {len(candles) if candles else 0} candles",
                "signal": 0,
                "action": "hold",
            }), file=sys.stderr)
            return {
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "error": f"Insufficient data: got {len(candles) if candles else 0} candles",
                "signal": 0,
                "action": "hold",
            }

        df = _make_dataframe(candles)
        market_ctx = {
            "symbol": symbol,
            "timeframe": timeframe,
            "price": float(candles[-1][4]),
            "timestamp": candles[-1][0],
        }

        # Ensure ATR indicator
        ensure_atr_indicator(df)
        market_ctx["atr"] = latest_atr(df)

        # Regime detection
        regime_label = ""
        if regime_enabled:
            regime_label = latest_regime(df, period=regime_period, adx_threshold=regime_adx_threshold)
            market_ctx["regime"] = regime_label

        # HTF filter
        if htf_filter_enabled:
            from htf_filter import check_htf_trend
            htf_tf = "4h" if timeframe == "1h" else "1d"
            htf_closes = adapter.get_ohlcv_closes(symbol, interval=htf_tf, limit=50)
            if htf_closes:
                htf_trend = check_htf_trend(htf_closes)
                market_ctx["htf_trend"] = htf_trend

        # Position context
        pos_ctx = position_ctx or {}
        if position_side:
            pos_ctx["side"] = position_side.lower()

        # Run strategy
        if open_close_enabled:
            result = evaluate_open_close(
                open_strategy=open_strategy or strategy_name,
                df=df,
                market_ctx=market_ctx,
                position_ctx=pos_ctx,
                open_params=strategy_params_override or {},
                close_params_by_name=close_params_by_name or {},
            )
        else:
            result = apply_strategy(strategy_name, df, strategy_params_override or {})

        signal = normalize_signal(result.get("signal", 0))
        action = "hold"
        if signal > 0:
            action = "buy"
        elif signal < 0:
            action = "sell"

        output = {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "action": action,
            "price": market_ctx["price"],
            "timestamp": market_ctx["timestamp"],
        }

        if "atr" in market_ctx:
            output["atr"] = market_ctx["atr"]
        if regime_label:
            output["regime"] = regime_label
        if "htf_trend" in market_ctx:
            output["htf_trend"] = market_ctx["htf_trend"]
        if "close_fraction" in result:
            output["close_fraction"] = result["close_fraction"]
        if "close_strategy" in result:
            output["close_strategy"] = result["close_strategy"]

        return output

    except Exception as e:
        return {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "error": str(e),
            "traceback": traceback.format_exc(),
            "signal": 0,
            "action": "hold",
        }


def run_execute(symbol, side, size, mode, stop_loss_pct=None, cancel_oid=None,
                prev_pos_qty=None, margin_mode=None, leverage=None):
    """Execute a live order on Phemex."""
    try:
        from adapter import PhemexExchangeAdapter

        adapter = PhemexExchangeAdapter()

        if not adapter.is_live:
            return {
                "error": "Live mode required. Set PHEMEX_API_KEY and PHEMEX_API_SECRET",
                "success": False,
            }

        is_buy = side.lower() == "buy"
        print(f"Executing {side} {size} {symbol} on Phemex...", file=sys.stderr)

        order = adapter.market_open(symbol, is_buy, size, inst_type="swap")

        fill_price = float(order.get("price") or order.get("avgPrice") or 0)
        fill_qty = float(order.get("execQty") or order.get("quantity") or size)
        fill_oid = order.get("orderID") or order.get("id") or ""
        fill_fee = float(order.get("fee") or 0)

        result = {
            "success": True,
            "symbol": symbol,
            "side": side,
            "fill_price": fill_price,
            "fill_qty": fill_qty,
            "fill_oid": fill_oid,
            "fill_fee": fill_fee,
            "order": order,
        }

        print(f"Filled {side} {fill_qty} {symbol} @ {fill_price}", file=sys.stderr)
        return result

    except Exception as e:
        return {
            "error": str(e),
            "traceback": traceback.format_exc(),
            "success": False,
        }


def main():
    import argparse
    parser = argparse.ArgumentParser(description="Phemex strategy check script")
    parser.add_argument("strategy", nargs="?", help="Strategy name")
    parser.add_argument("symbol", nargs="?", help="Symbol (e.g. BTC)")
    parser.add_argument("timeframe", nargs="?", help="Timeframe (e.g. 1h)")
    parser.add_argument("--mode", default="paper", choices=["paper", "live"], help="Paper or live mode")
    parser.add_argument("--execute", action="store_true", help="Execute live order")
    parser.add_argument("--side", choices=["buy", "sell"], help="Order side for execution")
    parser.add_argument("--size", type=float, help="Order size for execution")
    parser.add_argument("--stop-loss-pct", type=float, help="Stop loss percentage")
    parser.add_argument("--cancel-stop-loss-oid", help="Cancel this stop loss OID")
    parser.add_argument("--prev-pos-qty", type=float, help="Previous position quantity")
    parser.add_argument("--margin-mode", choices=["isolated", "cross"], help="Margin mode")
    parser.add_argument("--leverage", type=int, help="Leverage")
    parser.add_argument("--htf-filter", action="store_true", help="Enable HTF trend filter")
    parser.add_argument("--params", help="Strategy params as JSON")
    parser.add_argument("--open-strategy", help="Open strategy name (composition)")
    parser.add_argument("--close-strategies", help="Close strategies as JSON list")
    parser.add_argument("--position-side", help="Current position side")
    parser.add_argument("--position-avg-cost", type=float, help="Current position avg cost")
    parser.add_argument("--position-qty", type=float, help="Current position quantity")
    parser.add_argument("--position-initial-qty", type=float, help="Initial position quantity")
    parser.add_argument("--position-entry-atr", type=float, help="Position entry ATR")
    parser.add_argument("--regime-enabled", action="store_true", help="Enable regime filter")
    parser.add_argument("--regime-period", type=int, default=14, help="Regime period")
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0, help="Regime ADX threshold")
    parser.add_argument("--probe-only", action="store_true", help="Probe mode for startup compatibility check")

    args = parser.parse_args()

    if args.probe_only:
        print("Probe OK", file=sys.stderr)
        sys.exit(0)

    if args.execute:
        if not args.symbol or not args.side or args.size is None:
            print("Error: --execute requires --symbol, --side, and --size", file=sys.stderr)
            sys.exit(1)

        result = run_execute(
            symbol=args.symbol,
            side=args.side,
            size=args.size,
            mode=args.mode,
            stop_loss_pct=args.stop_loss_pct,
            cancel_oid=args.cancel_stop_loss_oid,
            prev_pos_qty=args.prev_pos_qty,
            margin_mode=args.margin_mode,
            leverage=args.leverage,
        )

        print(json.dumps(result, cls=SafeEncoder))
        sys.exit(0 if result.get("success") else 1)

    if not args.strategy or not args.symbol or not args.timeframe:
        print("Error: strategy, symbol, and timeframe required", file=sys.stderr)
        sys.exit(1)

    strategy_params = {}
    if args.params:
        try:
            strategy_params = json.loads(args.params)
        except json.JSONDecodeError:
            print(f"Warning: invalid JSON for --params: {args.params}", file=sys.stderr)

    position_ctx = _position_ctx_from_args(args)

    result = run_signal_check(
        strategy_name=args.strategy,
        symbol=args.symbol,
        timeframe=args.timeframe,
        mode=args.mode,
        htf_filter_enabled=args.htf_filter,
        strategy_params_override=strategy_params,
        open_strategy=args.open_strategy,
        close_strategies=args.close_strategies,
        position_side=args.position_side,
        position_ctx=position_ctx,
        regime_enabled=args.regime_enabled,
        regime_period=args.regime_period,
        regime_adx_threshold=args.regime_adx_threshold,
    )

    print(json.dumps(result, cls=SafeEncoder))


if __name__ == "__main__":
    main()
