#!/usr/bin/env python3

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'okx'))
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
        ("position_risk_anchor_price", "risk_anchor_price"),
    ):
        value = getattr(args, attr, None)
        if value is not None:
            ctx[key] = value
    regime = (getattr(args, "position_regime", "") or "").strip()
    if regime:
        ctx["regime"] = regime
    return ctx


def _float_or_none(value):
    if value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _extract_fee(response):
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
                     regime_enabled=False, regime_windows_spec=None, ohlcv_limit=200, regime_atr_window="",
                     regime_payload_json=None,
                     close_params_by_name=None,
                     atr_method="simple", gate_mode=None, acknowledgement=None,
                     closed_bar=False, decision_regime_timeframe=""):
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
            admit_configured_strategies,
            GateMode,
            Acknowledgement,
            GATE_MODE_MISSING,
        )

        open_close_enabled = bool(open_strategy or close_strategies)
        admit_configured_strategies(
            strategy_name,
            open_strategy,
            close_strategies,
            gate_mode if gate_mode is not None else GateMode(GATE_MODE_MISSING),
            acknowledgement if acknowledgement is not None else Acknowledgement(False),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        adapter = OKXExchangeAdapter()

        if closed_bar:
            for name in (strategy_name, open_strategy or strategy_name):
                why = unsupported_open_strategy_reason(name)
                if why:
                    raise ValueError(f"closed_bar_decisions does not support {name}: {why}")
            if (open_strategy or strategy_name) == "funding_skew":
                raise ValueError("closed_bar_decisions does not support funding_skew on OKX: this check has no timestamped funding history")
            if getattr(adapter, "OHLCV_TIMESTAMP_KIND", "") != "open":
                raise ValueError("closed_bar_decisions needs opening-time candle timestamps from the OKX adapter")

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
        cutoff_ms = 0
        raw_rows = None
        if closed_bar:
            cutoff_ms = int(time.time() * 1000)
            raw_rows = adapter.fetch_candles(symbol, timeframe, ohlcv_limit + 1, inst_type=inst_type)
            candles = raw_rows[-ohlcv_limit:]
        elif inst_type == "swap":
            candles = adapter.get_perp_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)
        else:
            candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)

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
        stdout_regime, live_regime, strategy_regime = prepare_check_regime(
            df,
            regime_enabled=regime_enabled,
            windows_spec=regime_windows_spec,
            atr_window=regime_atr_window,
            injected_payload_json=regime_payload_json,
        )
        current_params = dict(strategy_params)
        current_params["regime"] = strategy_regime
        if strategy_params_override:
            current_params = {**strategy_params_override, **current_params}
        strategy_params = current_params

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
                decision_params = dict(strategy_params_override or {})
                if regime_enabled:
                    regime_tf = decision_regime_timeframe or timeframe
                    if regime_tf == timeframe:
                        regime_rows = selection.rows
                    else:
                        regime_rows = select_opening_time(
                            fetch_rows_or_hold(lambda: adapter.fetch_candles(symbol, regime_tf, ohlcv_limit + 1, inst_type=inst_type), symbol, regime_tf),
                            timeframe=regime_tf, cutoff_ms=selection.boundary_ms, keep=ohlcv_limit).rows
                    decision_regime_payload, _decision_live, decision_strategy_regime = prepare_check_regime(
                        _make_dataframe(regime_rows),
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
                    htf_frame = _make_dataframe(select_opening_time(
                        fetch_rows_or_hold(lambda: adapter.fetch_candles(symbol, htf_tf, 61, inst_type=inst_type), symbol, htf_tf),
                        timeframe=htf_tf, cutoff_ms=selection.boundary_ms, keep=60, min_rows=50).rows)
                decision_df = _make_dataframe(selection.rows)
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
                parse_close_strategies(close_strategies),
                position_side,
                strategy_params or None,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
                close_params_by_name=close_params_by_name,
                **protection_kwargs,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        elif decision_df is not None:
            result_df = apply_strategy(strategy_name, decision_df, strategy_params or None)
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
        for col in (result_df.columns if last is not None else []):
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
            "mode": mode,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
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
            "mode": mode,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def run_execute(symbol, side, size, mode, inst_type="swap"):
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)

    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()

        is_buy = side.lower() == "buy"
        result = adapter.market_open(symbol, is_buy, size, inst_type=inst_type)

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
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        parser.add_argument("--htf-filter", action="store_true", default=False)
        parser.add_argument("--regime-enabled", action="store_true", default=False)
        parser.add_argument("--regime-windows-spec-json", default="")
        parser.add_argument("--ohlcv-limit", type=int, default=200)
        parser.add_argument("--regime-atr-window", default="")
        parser.add_argument("--regime-payload-json", default=None)
        parser.add_argument("--atr-method", default="simple", choices=["simple", "wilder"])
        parser.add_argument("--regime-directional-window", default="")
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--params", default=None)
        parser.add_argument("--open-strategy", default=None)
        parser.add_argument("--close-strategies", default=None)
        parser.add_argument("--strategy-refs", default=None)
        parser.add_argument("--position-side", default="")
        parser.add_argument("--position-avg-cost", type=float, default=None)
        parser.add_argument("--position-qty", type=float, default=None)
        parser.add_argument("--position-initial-qty", type=float, default=None)
        parser.add_argument("--position-entry-atr", type=float, default=None)
        parser.add_argument("--position-regime", default="")
        parser.add_argument("--position-risk-anchor-price", type=float, default=None)
        parser.add_argument("--mark-price", type=float, default=0.0, help="Accepted for argv-shape compatibility with check_hyperliquid.py (#768); ignored on this platform.")
        parser.add_argument("--allow-no-edge", nargs="?", const=True, default=None)
        parser.add_argument("--closed-bar-decisions", action="store_true", default=False,
            help="#1712: decide signals and entry ATR on the last closed bar; protection keeps current inputs.")
        parser.add_argument("--decision-regime-timeframe", default="",
            help="#1712: regime timeframe for the closed-bar decision view; defaults to the strategy timeframe.")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#645): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        from strategy_composition import parse_allow_no_edge_tokens, parse_raw_gate_mode, parse_strategy_refs_arg
        refs = parse_strategy_refs_arg(args.strategy_refs)
        open_strategy_name = refs["open_name"] if refs else args.open_strategy
        close_strategies_arg = refs["close_csv"] if refs else args.close_strategies
        params_override = refs["open_params"] if refs else (json.loads(args.params) if args.params else None)
        close_params_by_name = refs["close_params_by_name"] if refs else None
        position_ctx = _position_ctx_from_args(args)
        regime_windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        run_signal_check(
            args.strategy, args.symbol, args.timeframe, args.mode,
            args.htf_filter, args.inst_type, params_override,
            open_strategy_name, close_strategies_arg,
            args.position_side, position_ctx,
            regime_enabled=args.regime_enabled,
            regime_windows_spec=regime_windows_spec,
            ohlcv_limit=args.ohlcv_limit,
            regime_atr_window=args.regime_atr_window,
            regime_payload_json=args.regime_payload_json,
            close_params_by_name=close_params_by_name,
            atr_method=args.atr_method,
            gate_mode=parse_raw_gate_mode(sys.argv[1:]),
            acknowledgement=parse_allow_no_edge_tokens(sys.argv[1:]),
            closed_bar=args.closed_bar_decisions,
            decision_regime_timeframe=args.decision_regime_timeframe,
        )


if __name__ == "__main__":
    main()
