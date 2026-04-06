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
import traceback
from datetime import datetime, timezone

# Add paths: platforms/okx/ for adapter, shared_strategies/spot/ for apply_strategy,
# shared_tools/ for utilities.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'okx'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))


def _make_dataframe(candles):
    """Convert raw OHLCV list to pandas DataFrame compatible with strategy functions."""
    import pandas as pd
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False, inst_type="swap", strategy_params_override=None):
    """Run strategy signal check using OKX OHLCV data."""
    try:
        from adapter import OKXExchangeAdapter
        from strategies import apply_strategy, get_strategy

        # Verify strategy exists early
        get_strategy(strategy_name)

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
        if strategy_params_override:
            merged = {**strategy_params_override, **strategy_params}
            strategy_params = merged
        result_df = apply_strategy(strategy_name, df, strategy_params or None)

        last = result_df.iloc[-1]
        signal = int(last.get("signal", 0))
        if signal > 0:
            signal = 1
        elif signal < 0:
            signal = -1
        else:
            signal = 0

        price = float(last["close"])

        # Apply HTF trend filter if enabled (skip for funding-rate strategies — #103)
        htf_info = {}
        if htf_filter_enabled and strategy_name != "delta_neutral_funding":
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

        # Freshen price with live mid if available
        try:
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
                    indicators[col] = round(float(val), 6)
                except (ValueError, TypeError):
                    pass

        # Merge HTF indicators
        if htf_info:
            for k, v in htf_info.items():
                if isinstance(v, (int, float)):
                    indicators[k] = v

        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "indicators": indicators,
            "mode": mode,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
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
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--params", default=None)
        args = parser.parse_args()
        params_override = json.loads(args.params) if args.params else None
        run_signal_check(args.strategy, args.symbol, args.timeframe, args.mode, args.htf_filter, args.inst_type, params_override)


if __name__ == "__main__":
    main()
