#!/usr/bin/env python3
"""
Stateless spot strategy check script.
Fetches data, runs strategy, outputs JSON to stdout, exits.

Usage: python3 check_strategy.py <strategy> <symbol> <timeframe> [symbol_b]

  symbol_b  Optional second asset symbol for pairs_spread (e.g. ETH/USDT).
            When provided, close prices of symbol_b are merged into the
            dataframe as the 'close_b' column so the strategy runs proper
            stat-arb.  Without it, pairs_spread degrades to self-mean-reversion.
"""

import sys
import os
import json
import math
import traceback
from datetime import datetime, timezone

# Add parent dirs to path so we can import from strategies/ and core/
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator


def _arg_value(flag, default=None):
    prefix = flag + "="
    for arg in sys.argv:
        if arg.startswith(prefix):
            return arg.split("=", 1)[1]
    if flag not in sys.argv:
        return default
    idx = sys.argv.index(flag)
    if idx + 1 >= len(sys.argv):
        return default
    return sys.argv[idx + 1]


def _arg_float(flag):
    raw = _arg_value(flag)
    if raw in (None, ""):
        return None
    try:
        return float(raw)
    except (TypeError, ValueError):
        return None


def _position_ctx(position_side):
    ctx = {}
    if position_side:
        ctx["side"] = position_side
    for flag, key in (
        ("--position-avg-cost", "avg_cost"),
        ("--position-qty", "current_quantity"),
        ("--position-initial-qty", "initial_quantity"),
        ("--position-entry-atr", "entry_atr"),
    ):
        value = _arg_float(flag)
        if value is not None:
            ctx[key] = value
    return ctx


def main():
    # Parse optional flags from argv before positional args
    htf_filter_enabled = "--htf-filter" in sys.argv
    open_strategy = _arg_value("--open-strategy")
    close_strategies_raw = _arg_value("--close-strategies")
    position_side = (_arg_value("--position-side", "") or "").lower()
    position_ctx = _position_ctx(position_side)
    open_close_enabled = bool(open_strategy or close_strategies_raw)
    strategy_params = None
    if "--params" in sys.argv:
        idx = sys.argv.index("--params")
        if idx + 1 < len(sys.argv):
            strategy_params = json.loads(sys.argv[idx + 1])
    # Filter out --flag and --params <value> (skip the arg after --params)
    filtered = []
    skip_next = False
    for a in sys.argv[1:]:
        if skip_next:
            skip_next = False
            continue
        if a in (
            "--params", "--open-strategy", "--close-strategies", "--position-side",
            "--position-avg-cost", "--position-qty", "--position-initial-qty",
            "--position-entry-atr",
        ):
            skip_next = True
            continue
        if a.startswith("--"):
            continue
        filtered.append(a)
    positional_args = filtered

    if len(positional_args) < 3:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <symbol> <timeframe> [symbol_b] [--htf-filter]"
        }))
        sys.exit(1)

    strategy_name = positional_args[0]
    symbol = positional_args[1]
    timeframe = positional_args[2]
    symbol_b = positional_args[3] if len(positional_args) >= 4 else None

    try:
        from strategies import apply_strategy, get_strategy, list_strategies
        from close_registry_loader import (
            evaluate as close_evaluate,
            get_strategy as get_close_strategy,
            list_strategies as list_close_strategies,
        )
        from data_fetcher import fetch_ohlcv
        from strategy_composition import (
            evaluate_open_close,
            finalize_decision,
            normalize_signal,
            parse_close_strategies,
            validate_close_strategy_names,
        )

        configured_names = [open_strategy or strategy_name]
        for name in configured_names:
            get_strategy(name)
        validate_close_strategy_names(
            parse_close_strategies(close_strategies_raw),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        # Warn when pairs_spread will degrade due to missing secondary symbol
        needs_pair = "pairs_spread" in configured_names
        if needs_pair and not symbol_b:
            print(
                "Warning: pairs_spread requires a secondary symbol (symbol_b); "
                "degrading to self-mean-reversion. Pass a 4th argument to enable "
                "proper stat-arb (e.g. ETH/USDT for a BTC/USDT primary).",
                file=sys.stderr,
            )

        # Fetch primary data
        print(f"Fetching {symbol} {timeframe}...", file=sys.stderr)
        df = fetch_ohlcv(symbol=symbol, timeframe=timeframe, limit=200, store=False)

        # Fetch and merge secondary data for pairs strategies
        if needs_pair and symbol_b:
            print(f"Fetching secondary {symbol_b} {timeframe}...", file=sys.stderr)
            df_b = fetch_ohlcv(symbol=symbol_b, timeframe=timeframe, limit=200, store=False)
            if df_b.empty:
                print(json.dumps({
                    "strategy": strategy_name,
                    "symbol": symbol,
                    "timeframe": timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"No data returned for secondary symbol {symbol_b}",
                }))
                sys.exit(1)
            # Inner join on datetime index so both assets have the same timestamps
            df = df.join(df_b[["close"]].rename(columns={"close": "close_b"}), how="inner")
            print(f"Merged pair: {len(df)} aligned candles ({symbol} / {symbol_b})", file=sys.stderr)

        if df.empty or len(df) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(df)} candles"
            }))
            return

        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            evaluation = evaluate_open_close(
                apply_strategy,
                get_strategy,
                df,
                strategy_name,
                open_strategy,
                parse_close_strategies(close_strategies_raw),
                position_side,
                strategy_params,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        else:
            # Run the strategy
            result_df = apply_strategy(strategy_name, df, strategy_params)
            signal = normalize_signal(result_df.iloc[-1].get("signal", 0))

        # Get the last row's signal
        last = result_df.iloc[-1]
        price = float(last["close"])

        # Apply HTF trend filter if enabled (skip for funding-rate strategies — #103)
        htf_info = {}
        htf_strategy_name = open_strategy or strategy_name
        if htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter

            def _fetch_htf(sym, tf, limit):
                return fetch_ohlcv(symbol=sym, timeframe=tf, limit=limit, store=False)

            htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
            original_signal = signal
            signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
            if signal != original_signal:
                print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

        if open_close_enabled:
            decision = finalize_decision(evaluation, position_side, signal)
            signal = decision["signal"]

        # Collect relevant indicators
        ensure_atr_indicator(result_df)
        indicators = {}
        indicator_cols = [c for c in result_df.columns
                         if c not in ("open", "high", "low", "close", "close_b", "volume",
                                      "timestamp", "signal", "position", "datetime")]
        for col in indicator_cols:
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
            "timestamp": datetime.now(timezone.utc).isoformat()
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
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e)
        }))
        sys.exit(1)  # Exit 1; Go will still parse the JSON error field


if __name__ == "__main__":
    main()
