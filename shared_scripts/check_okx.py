#!/usr/bin/env python3
"""
OKX spot/perps strategy check script.
Fetches OHLCV from OKX via CCXT, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_okx.py <strategy> <symbol> <timeframe> [--mode=paper|live] [--htf-filter] [--inst-type=spot|swap]

Execution mode (live only, called by Go as phase 2):
    check_okx.py --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live] [--inst-type=spot|swap]
"""

import sys
import os
import json
import math
import traceback
from datetime import datetime, timezone

# Add paths: platforms/okx/ for adapter, shared_tools/ for utilities.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'okx'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from regime import latest_regime

# Use futures registry for perps (swap), spot registry for spot.
# Default is swap, matching argparse defaults below.
_inst_type = "swap"
for _arg in sys.argv:
    if _arg.startswith("--inst-type="):
        _inst_type = _arg.split("=", 1)[1]
        break
if _inst_type == "spot":
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'spot'))
else:
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'futures'))


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


def _extract_fee(response):
    """Extract ccxt unified order fee.cost when present."""
    if not isinstance(response, dict):
        return None
    fee_info = response.get("fee")
    if isinstance(fee_info, dict):
        return _float_or_none(fee_info.get("cost"))
    return _float_or_none(fee_info)


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     inst_type="swap", strategy_params_override=None,
                     open_strategy=None, close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_period=14, regime_adx_threshold=20.0):
    """Run strategy signal check using OKX OHLCV data."""
    try:
        from adapter import OKXExchangeAdapter
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

        adapter = OKXExchangeAdapter()

        # Fetch funding rate data for delta-neutral strategy (perps only)
        strategy_params = {}
        if strategy_name == "delta_neutral_funding" and inst_type == "swap":
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

        print(f"Fetching {symbol} {timeframe} from OKX ({mode}, {inst_type})...", file=sys.stderr)
        if inst_type == "swap":
            candles = adapter.get_perp_ohlcv(symbol, interval=timeframe, limit=200)
        else:
            candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=200)

        if not candles or len(candles) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "mode": mode,
                "platform": "okx",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(candles) if candles else 0} candles",
            }))
            sys.exit(1)

        df = _make_dataframe(candles)
        if regime_enabled:
            regime_payload = latest_regime(df, period=regime_period, adx_threshold=regime_adx_threshold)
        else:
            regime_payload = {"regime": "", "score": 0.0, "metrics": {}}
        strategy_params["regime"] = regime_payload
        if strategy_params_override:
            merged = {**strategy_params_override, **strategy_params}
            strategy_params = merged
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
                strategy_params or None,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        else:
            result_df = apply_strategy(strategy_name, df, strategy_params or None)
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
                if inst_type == "swap":
                    candles = adapter.get_perp_ohlcv(sym, interval=tf, limit=limit)
                else:
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

        # Freshen price with live mid if available
        try:
            if inst_type == "swap":
                mid = adapter.get_perp_price(symbol)
            else:
                mid = adapter.get_spot_price(symbol)
            if mid > 0:
                price = mid
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
            "indicators": indicators,
            "regime": regime_payload["regime"],
            "mode": mode,
            "platform": "okx",
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
            "indicators": {},
            "regime": None,
            "mode": mode,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def run_execute(symbol, side, size, mode, inst_type="swap"):
    """Place a live market order on OKX."""
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)

    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()

        is_buy = side.lower() == "buy"
        result = adapter.market_open(symbol, is_buy, size, inst_type=inst_type)

        # Extract fill info from ccxt response structure
        fill = {}
        try:
            fill = {
                "avg_px": float(result.get("average", 0) or 0),
                "total_sz": float(result.get("filled", 0) or 0),
            }
            fee = _extract_fee(result)
            if fee is not None:
                fill["fee"] = fee
            oid = result.get("id")
            if oid:
                fill["oid"] = str(oid)
        except Exception:
            pass

        print(json.dumps({
            "execution": {
                "action": "buy" if is_buy else "sell",
                "symbol": symbol,
                "size": size,
                "fill": fill,
            },
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "execution": None,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def main():
    if "--execute" in sys.argv:
        # Execute mode: --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live] [--inst-type=spot|swap]
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        args = parser.parse_args()
        run_execute(args.symbol, args.side, args.size, args.mode, args.inst_type)
    else:
        # Signal check mode: <strategy> <symbol> <timeframe> [--mode=paper|live] [--htf-filter] [--inst-type=spot|swap]
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        parser.add_argument("--htf-filter", action="store_true", default=False)
        parser.add_argument("--regime-enabled", action="store_true", default=False)
        parser.add_argument("--regime-period", type=int, default=14)
        parser.add_argument("--regime-adx-threshold", type=float, default=20.0)
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--params", default=None)
        parser.add_argument("--open-strategy", default=None)
        parser.add_argument("--close-strategies", default=None)
        parser.add_argument("--position-side", default="")
        parser.add_argument("--position-avg-cost", type=float, default=None)
        parser.add_argument("--position-qty", type=float, default=None)
        parser.add_argument("--position-initial-qty", type=float, default=None)
        parser.add_argument("--position-entry-atr", type=float, default=None)
        args = parser.parse_args()
        params_override = json.loads(args.params) if args.params else None
        position_ctx = _position_ctx_from_args(args)
        run_signal_check(
            args.strategy, args.symbol, args.timeframe, args.mode,
            args.htf_filter, args.inst_type, params_override,
            args.open_strategy, args.close_strategies,
            args.position_side, position_ctx,
            regime_enabled=args.regime_enabled,
            regime_period=args.regime_period,
            regime_adx_threshold=args.regime_adx_threshold,
        )


if __name__ == "__main__":
    main()
