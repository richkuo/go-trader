#!/usr/bin/env python3

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from closed_bar import (
    ClosedBarHold,
    fetch_rows_or_hold,
    hold_metadata,
    htf_closed_fetcher,
    select_opening_time,
    unsupported_open_strategy_reason,
)
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime


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
        ("--position-risk-anchor-price", "risk_anchor_price"),
    ):
        value = _arg_float(flag)
        if value is not None:
            ctx[key] = value
    regime = (_arg_value("--position-regime", "") or "").strip()
    if regime:
        ctx["regime"] = regime
    return ctx


def main():
    if "--probe-only" in sys.argv:
        sys.exit(0)
    htf_filter_enabled = "--htf-filter" in sys.argv
    regime_enabled = "--regime-enabled" in sys.argv
    regime_windows_spec = parse_regime_windows_spec_json(_arg_value("--regime-windows-spec-json"))
    ohlcv_limit = int(_arg_value("--ohlcv-limit") or 200)
    regime_atr_window = (_arg_value("--regime-atr-window") or "").strip()
    regime_payload_json = _arg_value("--regime-payload-json")
    atr_method = (_arg_value("--atr-method") or "simple").strip().lower()
    closed_bar = "--closed-bar-decisions" in sys.argv
    decision_regime_timeframe = (_arg_value("--decision-regime-timeframe") or "").strip()
    if atr_method not in ("simple", "wilder"):
        print(json.dumps({
            "error": f"--atr-method must be 'simple' or 'wilder', got {atr_method!r}",
        }))
        sys.exit(1)
    open_strategy = _arg_value("--open-strategy")
    close_strategies_raw = _arg_value("--close-strategies")
    position_side = (_arg_value("--position-side", "") or "").lower()
    position_ctx = _position_ctx(position_side)
    strategy_params = None
    if "--params" in sys.argv:
        idx = sys.argv.index("--params")
        if idx + 1 < len(sys.argv):
            strategy_params = json.loads(sys.argv[idx + 1])
    close_params_by_name = None
    strategy_refs_raw = _arg_value("--strategy-refs")
    if strategy_refs_raw:
        sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
        from strategy_composition import parse_strategy_refs_arg
        refs = parse_strategy_refs_arg(strategy_refs_raw)
        if refs:
            open_strategy = refs["open_name"]
            close_strategies_raw = refs["close_csv"]
            strategy_params = refs["open_params"]
            close_params_by_name = refs["close_params_by_name"]
    open_close_enabled = bool(open_strategy or close_strategies_raw)
    filtered = []
    skip_next = False
    for a in sys.argv[1:]:
        if skip_next:
            skip_next = False
            continue
        if a in (
            "--params", "--open-strategy", "--close-strategies", "--strategy-refs",
            "--position-side", "--position-avg-cost", "--position-qty",
            "--position-initial-qty", "--position-entry-atr",
            "--position-regime", "--position-risk-anchor-price",
            "--regime-windows-spec-json", "--ohlcv-limit",
            "--regime-atr-window", "--regime-directional-window",
            "--regime-payload-json", "--atr-method", "--mode",
            "--decision-regime-timeframe",
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
            admit_configured_strategies,
            evaluate_open_close,
            finalize_decision,
            normalize_signal,
            parse_allow_no_edge_tokens,
            parse_close_strategies,
            parse_raw_gate_mode,
        )

        configured_names = [open_strategy or strategy_name]
        admit_configured_strategies(
            strategy_name,
            open_strategy,
            close_strategies_raw,
            parse_raw_gate_mode(sys.argv[1:]),
            parse_allow_no_edge_tokens(sys.argv[1:]),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        needs_pair = "pairs_spread" in configured_names
        if needs_pair and not symbol_b:
            print(
                "Warning: pairs_spread requires a secondary symbol (symbol_b); "
                "degrading to self-mean-reversion. Pass a 4th argument to enable "
                "proper stat-arb (e.g. ETH/USDT for a BTC/USDT primary).",
                file=sys.stderr,
            )

        if closed_bar:
            for name in (strategy_name, open_strategy or strategy_name):
                why = unsupported_open_strategy_reason(name)
                if why:
                    raise ValueError(f"closed_bar_decisions does not support {name}: {why}")
            from data_fetcher import OHLCV_TIMESTAMP_KIND, fetch_ohlcv_rows, ohlcv_rows_frame
            if OHLCV_TIMESTAMP_KIND != "open":
                raise ValueError("closed_bar_decisions needs opening-time candle timestamps from data_fetcher")

        cutoff_ms = 0
        raw_rows = None
        raw_rows_b = None
        print(f"Fetching {symbol} {timeframe}...", file=sys.stderr)
        if closed_bar:
            cutoff_ms = int(time.time() * 1000)
            raw_rows = fetch_ohlcv_rows(symbol, timeframe, ohlcv_limit + 1)
            df = ohlcv_rows_frame(raw_rows[-ohlcv_limit:])
        else:
            df = fetch_ohlcv(symbol=symbol, timeframe=timeframe, limit=ohlcv_limit, store=False)

        if needs_pair and symbol_b:
            print(f"Fetching secondary {symbol_b} {timeframe}...", file=sys.stderr)
            if closed_bar:
                raw_rows_b = fetch_ohlcv_rows(symbol_b, timeframe, ohlcv_limit + 1)
                df_b = ohlcv_rows_frame(raw_rows_b[-ohlcv_limit:])
            else:
                df_b = fetch_ohlcv(symbol=symbol_b, timeframe=timeframe, limit=ohlcv_limit, store=False)
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
                "regime": None,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(df)} candles"
            }))
            return

        stdout_regime, live_regime, strategy_regime = prepare_check_regime(
            df,
            regime_enabled=regime_enabled,
            windows_spec=regime_windows_spec,
            atr_window=regime_atr_window,
            injected_payload_json=regime_payload_json,
        )
        override_params = dict(strategy_params or {})
        strategy_params = dict(override_params)
        strategy_params["regime"] = strategy_regime

        closed_meta = None
        decision_regime_payload = None
        decision_df = df
        protection_kwargs = {}
        htf_frame = None
        htf_strategy_name = open_strategy or strategy_name
        if closed_bar:
            hold = ""
            try:
                selection = select_opening_time(
                    raw_rows, timeframe=timeframe, cutoff_ms=cutoff_ms, keep=ohlcv_limit)
                decision_df = ohlcv_rows_frame(selection.rows)
                if raw_rows_b is not None:
                    selection_b = select_opening_time(
                        raw_rows_b, timeframe=timeframe, cutoff_ms=cutoff_ms, keep=ohlcv_limit)
                    if selection_b.boundary_ms != selection.boundary_ms:
                        raise ClosedBarHold(
                            f"paired frame {symbol_b} closes at {selection_b.boundary_ms}, "
                            f"primary closes at {selection.boundary_ms}")
                    df_b_closed = ohlcv_rows_frame(selection_b.rows)
                    decision_df = decision_df.join(
                        df_b_closed[["close"]].rename(columns={"close": "close_b"}), how="inner")
                    if len(decision_df) < 30 or int(decision_df["timestamp"].iloc[-1]) != selection.bar_open_ms:
                        raise ClosedBarHold("paired frames do not align on the selected closed bar")
                decision_params = dict(override_params)
                if regime_enabled:
                    regime_tf = decision_regime_timeframe or timeframe
                    if regime_tf == timeframe:
                        regime_rows = selection.rows
                    else:
                        regime_rows = select_opening_time(
                            fetch_rows_or_hold(lambda: fetch_ohlcv_rows(symbol, regime_tf, ohlcv_limit + 1), symbol, regime_tf),
                            timeframe=regime_tf, cutoff_ms=selection.boundary_ms, keep=ohlcv_limit).rows
                    decision_regime_payload, _decision_live, decision_strategy_regime = prepare_check_regime(
                        ohlcv_rows_frame(regime_rows),
                        regime_enabled=True,
                        windows_spec=regime_windows_spec,
                        injected_payload_json=None,
                    )
                    decision_params["regime"] = decision_strategy_regime
                else:
                    decision_params["regime"] = strategy_regime
                if htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
                    from htf_filter import get_default_htf
                    htf_tf = get_default_htf(timeframe)
                    htf_frame = ohlcv_rows_frame(select_opening_time(
                        fetch_rows_or_hold(lambda: fetch_ohlcv_rows(symbol, htf_tf, 61), symbol, htf_tf),
                        timeframe=htf_tf, cutoff_ms=selection.boundary_ms, keep=60, min_rows=50).rows)
                closed_meta = selection.metadata()
            except ClosedBarHold as e:
                hold = e.reason
            protection_kwargs = {"protection_df": df, "protection_params": strategy_params}
            if hold:
                closed_meta = hold_metadata(hold, cutoff_ms)
                decision_regime_payload = None
                decision_df = None
                print(f"Closed-bar decision held for {symbol} {timeframe}: {hold}", file=sys.stderr)
            else:
                strategy_params = decision_params

        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            atr_now = latest_atr(df, method=atr_method)
            if atr_now > 0:
                market_ctx["atr"] = atr_now
            if live_regime:
                market_ctx["regime"] = live_regime
            evaluation = evaluate_open_close(
                apply_strategy,
                get_strategy,
                decision_df,
                strategy_name,
                open_strategy,
                parse_close_strategies(close_strategies_raw),
                position_side,
                strategy_params,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
                close_params_by_name=close_params_by_name,
                **protection_kwargs,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        elif decision_df is not None:
            result_df = apply_strategy(strategy_name, decision_df, strategy_params)
            signal = normalize_signal(result_df.iloc[-1].get("signal", 0))
        else:
            get_strategy(strategy_name)
            result_df = None
            signal = 0

        last = None
        if result_df is not None and not result_df.empty:
            ensure_atr_indicator(result_df, method=atr_method)
            last = result_df.iloc[-1]
        if closed_bar:
            price = float(df["close"].iloc[-1])
        else:
            price = float(last["close"])

        htf_info = {}
        if decision_df is not None and htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter

            if closed_bar:
                _fetch_htf = htf_closed_fetcher(htf_frame)
            else:
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

        indicators = {}
        indicator_cols = []
        if last is not None:
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
            "regime": stdout_regime,
            "timestamp": datetime.now(timezone.utc).isoformat()
        }
        if decision:
            output.update(decision)
        if closed_meta is not None:
            output["closed_bar_decision"] = closed_meta
            if decision_regime_payload is not None:
                output["decision_regime"] = decision_regime_payload
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
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e)
        }))
        sys.exit(1)


if __name__ == "__main__":
    main()
