#!/usr/bin/env python3
"""
TopStep futures strategy check script.
Fetches OHLCV from TopStepX API, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_topstep.py <strategy> <symbol> <timeframe> [--mode=paper|live]

Execution mode (live only, called by Go as phase 2):
    check_topstep.py --execute --symbol=ES --side=buy|sell --contracts=2 [--mode=live]
"""

import sys
import os
import json
import math
import traceback
from datetime import datetime, timezone

# Add paths for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'topstep'))
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


def _float_or_none(value):
    if value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


_FEE_CONTAINER_KEYS = ("fee", "fees", "commission", "commission_paid", "totalFee", "totalFees", "commissionAndFees")
_FEE_VALUE_KEYS = ("cost", "amount", "total", "value")


def _extract_fee_value(value, fee_context=False):
    if isinstance(value, dict):
        keys = _FEE_VALUE_KEYS if fee_context else _FEE_CONTAINER_KEYS
        for key in keys:
            fee = _extract_fee_value(value.get(key), fee_context=True)
            if fee is not None:
                return fee
        return None
    if isinstance(value, list):
        total = 0.0
        found = False
        for item in value:
            fee = _extract_fee_value(item, fee_context=fee_context)
            if fee is not None:
                total += fee
                found = True
        return total if found else None
    if fee_context:
        return _float_or_none(value)
    return None


def _extract_fee(response):
    """Best-effort TopStep fill fee extraction; absent fees fall back in Go."""
    if not isinstance(response, dict):
        return None
    for key in _FEE_CONTAINER_KEYS:
        fee = _extract_fee_value(response.get(key), fee_context=True)
        if fee is not None:
            return fee
    return None


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None):
    """Run strategy signal check using TopStep market data."""
    try:
        from adapter import TopStepExchangeAdapter
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

        adapter = TopStepExchangeAdapter(mode=mode)

        # Check market hours
        market_open = adapter.is_market_open()
        if not market_open:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "contract_spec": adapter.get_contract_spec(symbol),
                "market_open": False,
                "indicators": {},
                "mode": mode,
                "platform": "topstep",
                "timestamp": datetime.now(timezone.utc).isoformat(),
            }))
            return

        print(f"Fetching {symbol} {timeframe} from TopStepX ({mode})...", file=sys.stderr)
        candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=200)

        if not candles or len(candles) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "contract_spec": adapter.get_contract_spec(symbol),
                "market_open": market_open,
                "indicators": {},
                "mode": mode,
                "platform": "topstep",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(candles) if candles else 0} candles",
            }))
            sys.exit(1)

        df = _make_dataframe(candles)
        regime_payload = latest_regime(df)
        strategy_params = (strategy_params or {})
        strategy_params["regime"] = regime_payload
        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            atr_now = latest_atr(df)
            if atr_now > 0:
                market_ctx["atr"] = atr_now
            evaluation = evaluate_open_close(
                apply_strategy,
                get_strategy,
                df,
                strategy_name,
                open_strategy,
                parse_close_strategies(close_strategies),
                position_side,
                strategy_params,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        else:
            result_df = apply_strategy(strategy_name, df, strategy_params)
            signal = normalize_signal(result_df.iloc[-1].get("signal", 0))

        ensure_atr_indicator(result_df)
        last = result_df.iloc[-1]
        price = float(last["close"])

        # Apply HTF trend filter if enabled (skip for funding-rate strategies — #103)
        htf_info = {}
        htf_strategy_name = open_strategy or strategy_name
        if htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter

            def _fetch_htf(sym, tf, limit):
                candles = adapter.get_ohlcv(sym, interval=tf, limit=limit)
                return _make_dataframe(candles) if candles else None

            htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
            original_signal = signal
            signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
            if signal != original_signal:
                print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

        if open_close_enabled:
            decision = finalize_decision(evaluation, position_side, signal)
            signal = decision["signal"]

        # Freshen price with live quote if available
        try:
            live = adapter.get_price(symbol)
            if live > 0:
                price = live
        except Exception:
            pass

        indicators = {}
        skip_cols = {
            "open", "high", "low", "close", "volume",
            "timestamp", "signal", "position", "datetime",
        }
        for col in result_df.columns:
            if col in skip_cols:
                continue
            val = last.get(col)
            if val is not None:
                try:
                    fval = float(val)
                    if math.isfinite(fval):
                        indicators[col] = round(fval, 6)
                except (ValueError, TypeError):
                    pass

        # Merge HTF indicators
        if htf_info:
            for k, v in htf_info.items():
                if isinstance(v, (int, float)):
                    indicators[k] = v

        output = {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "contract_spec": adapter.get_contract_spec(symbol),
            "market_open": market_open,
            "indicators": indicators,
            "regime": regime_payload["regime"],
            "mode": mode,
            "platform": "topstep",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if decision:
            output.update(decision)
        print(json.dumps(output))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": 0,
            "price": 0,
            "contract_spec": {},
            "market_open": False,
            "indicators": {},
            "regime": None,
            "mode": mode,
            "platform": "topstep",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def run_execute(symbol, side, contracts, mode):
    """Place a live market order on TopStep."""
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)

    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode="live")

        is_buy = side.lower() == "buy"
        result = adapter.market_open(symbol, is_buy, contracts)

        # Extract fill info from API response
        fill = {}
        try:
            fill = {
                "avg_px": float(result.get("avgPrice", 0) or 0),
                "total_contracts": int(result.get("filledQuantity", contracts) or contracts),
            }
            fee = _extract_fee(result)
            if fee is not None:
                fill["fee"] = fee
            oid = result.get("orderId")
            if oid is None:
                oid = result.get("id")
            if oid:
                fill["oid"] = str(oid)
        except Exception as e:
            print(f"[topstep] fill parse error: {e}", file=sys.stderr)

        print(json.dumps({
            "execution": {
                "action": "buy" if is_buy else "sell",
                "symbol": symbol,
                "contracts": contracts,
                "fill": fill,
            },
            "platform": "topstep",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "execution": None,
            "platform": "topstep",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def main():
    if "--execute" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--contracts", type=int, required=True)
        parser.add_argument("--mode", default="live")
        args = parser.parse_args()
        run_execute(args.symbol, args.side, args.contracts, args.mode)
    else:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        parser.add_argument("--htf-filter", action="store_true", default=False)
        parser.add_argument("--params", default=None)
        parser.add_argument("--open-strategy", default=None)
        parser.add_argument("--close-strategies", default=None)
        parser.add_argument("--position-side", default="")
        parser.add_argument("--position-avg-cost", type=float, default=None)
        parser.add_argument("--position-qty", type=float, default=None)
        parser.add_argument("--position-initial-qty", type=float, default=None)
        parser.add_argument("--position-entry-atr", type=float, default=None)
        args = parser.parse_args()
        params_parsed = json.loads(args.params) if args.params else None
        position_ctx = _position_ctx_from_args(args)
        run_signal_check(
            args.strategy, args.symbol, args.timeframe, args.mode,
            args.htf_filter, params_parsed, args.open_strategy,
            args.close_strategies,
            args.position_side, position_ctx,
        )


if __name__ == "__main__":
    main()
