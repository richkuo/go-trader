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
import traceback
from datetime import datetime, timezone

# Add paths for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'topstep'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'futures'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))


def _make_dataframe(candles):
    """Convert raw OHLCV list to pandas DataFrame compatible with strategy functions."""
    import pandas as pd
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False, strategy_params=None):
    """Run strategy signal check using TopStep market data."""
    try:
        from adapter import TopStepExchangeAdapter
        from strategies import apply_strategy, get_strategy

        # Verify strategy exists early
        get_strategy(strategy_name)

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
        result_df = apply_strategy(strategy_name, df, strategy_params)

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
                candles = adapter.get_ohlcv(sym, interval=tf, limit=limit)
                return _make_dataframe(candles) if candles else None

            htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
            original_signal = signal
            signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
            if signal != original_signal:
                print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

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
            "contract_spec": adapter.get_contract_spec(symbol),
            "market_open": market_open,
            "indicators": indicators,
            "mode": mode,
            "platform": "topstep",
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
            "contract_spec": {},
            "market_open": False,
            "indicators": {},
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
        args = parser.parse_args()
        params_parsed = json.loads(args.params) if args.params else None
        run_signal_check(args.strategy, args.symbol, args.timeframe, args.mode, args.htf_filter, params_parsed)


if __name__ == "__main__":
    main()
