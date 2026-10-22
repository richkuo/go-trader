#!/usr/bin/env python3

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone

_STARTED = time.monotonic()

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'robinhood'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from htf_filter import (
    HTF_EXCLUDED_DEFAULT,
    htf_skipped_block,
    run_htf_filter,
)
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime
from stdout_contract import json_stdout


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


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_windows_spec=None, ohlcv_limit=200, regime_atr_window="",
                     regime_payload_json=None,
                     close_params_by_name=None,
                     atr_method="simple", gate_mode=None, acknowledgement=None):
    with json_stdout() as emit:
        try:
            from adapter import RobinhoodExchangeAdapter
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

            adapter = RobinhoodExchangeAdapter(mode=mode)

            print(f"Fetching {symbol} {timeframe} from Robinhood/yfinance ({mode})...", file=sys.stderr)
            candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)

            if not candles or len(candles) < 30:
                emit({
                    "strategy": strategy_name,
                    "symbol": symbol,
                    "timeframe": timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "mode": mode,
                    "platform": "robinhood",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"Insufficient data: {len(candles) if candles else 0} candles",
                })
                sys.exit(1)

            df = _make_dataframe(candles)
            stdout_regime, live_regime, strategy_regime = prepare_check_regime(
                df,
                regime_enabled=regime_enabled,
                windows_spec=regime_windows_spec,
                atr_window=regime_atr_window,
                injected_payload_json=regime_payload_json,
            )
            strategy_params = (strategy_params or {})
            strategy_params["regime"] = strategy_regime
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
                    df,
                    strategy_name,
                    open_strategy,
                    parse_close_strategies(close_strategies),
                    position_side,
                    strategy_params,
                    position_ctx,
                    close_evaluate=close_evaluate,
                    market_ctx=market_ctx,
                    close_params_by_name=close_params_by_name,
                )
                result_df = evaluation.open_result_df
                signal = evaluation.open_signal
            else:
                result_df = apply_strategy(strategy_name, df, strategy_params)
                signal = normalize_signal(result_df.iloc[-1].get("signal", 0))

            ensure_atr_indicator(result_df, method=atr_method)
            last = result_df.iloc[-1]
            price = float(last["close"])

            htf_indicators = {}
            htf_block = None
            htf_strategy_name = open_strategy or strategy_name
            if htf_filter_enabled and htf_strategy_name in HTF_EXCLUDED_DEFAULT:
                htf_block = htf_skipped_block(timeframe, htf_strategy_name)
            elif htf_filter_enabled:
                def _fetch_htf(sym, tf, limit):
                    candles = adapter.get_ohlcv(sym, interval=tf, limit=limit)
                    return _make_dataframe(candles) if candles else None

                signal, htf_block, htf_indicators = run_htf_filter(signal, symbol, timeframe, _fetch_htf)

            if open_close_enabled:
                decision = finalize_decision(evaluation, position_side, signal)
                signal = decision["signal"]

            try:
                live_price = adapter.get_price(symbol)
                if live_price > 0:
                    price = live_price
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

            indicators.update(htf_indicators)

            output = {
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": signal,
                "price": price,
                "indicators": indicators,
                "regime": stdout_regime,
                "mode": mode,
                "platform": "robinhood",
                "timestamp": datetime.now(timezone.utc).isoformat(),
            }
            if decision:
                output.update(decision)
            if htf_block is not None:
                output["htf_filter"] = htf_block
            emit(output)

        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            emit({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "regime": None,
                "mode": mode,
                "platform": "robinhood",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": str(e),
            })
            sys.exit(1)


_BUDGET_MIN_MS = 2000
_BUDGET_MAX_MS = 120000
_CONFIRMED_FILL_OUTCOMES = ("filled", "partially_filled")


def _finite_or_none(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    value = float(value)
    return value if math.isfinite(value) else None


def _budget_deadline(budget_ms):
    if not isinstance(budget_ms, int) or budget_ms < _BUDGET_MIN_MS or budget_ms > _BUDGET_MAX_MS:
        raise ValueError(f"--budget-ms must be an integer from {_BUDGET_MIN_MS} to {_BUDGET_MAX_MS}, got {budget_ms!r}")
    return _STARTED + budget_ms / 1000.0


_SIZING_NUMBER_KEYS = (
    "asked_quantity", "account_available", "sent_quantity", "min_order_size",
    "quantity_increment", "max_order_size", "residue_quantity",
)
_SIZING_BOOL_KEYS = ("sole_owner", "swept", "capped", "account_short")


def _clean_sizing(sizing):
    if not isinstance(sizing, dict):
        return None
    clean = {key: _finite_or_none(sizing.get(key)) for key in _SIZING_NUMBER_KEYS}
    for key in _SIZING_BOOL_KEYS:
        clean[key] = sizing.get(key) is True
    kind = sizing.get("close_kind")
    clean["close_kind"] = kind if kind in ("whole", "partial") else None
    return clean


def _order_envelope(order_result, action, symbol, amount_usd=None, quantity=None):
    outcome = order_result.get("outcome") or "unknown"
    execution = {"action": action, "symbol": symbol}
    if action == "buy" and amount_usd is not None:
        execution["amount_usd"] = _finite_or_none(amount_usd)
    if action == "sell" and quantity is not None:
        execution["quantity"] = _finite_or_none(quantity)
    fill = order_result.get("fill")
    sizing = _clean_sizing(order_result.get("sizing"))
    payload = {
        "execution": execution,
        "order_outcome": outcome,
        "order": order_result.get("order") or {},
        "ref_id": order_result.get("ref_id"),
        "requested_quantity": _finite_or_none(order_result.get("requested_quantity")),
        "account_id": order_result.get("account_id"),
        "cancel": order_result.get("cancel") or {"requested": False, "acknowledged": False},
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if sizing is not None:
        payload["sizing"] = sizing
    if outcome in _CONFIRMED_FILL_OUTCOMES and isinstance(fill, dict):
        clean = {
            "avg_px": _finite_or_none(fill.get("avg_px")),
            "quantity": _finite_or_none(fill.get("quantity")),
            "oid": fill.get("oid"),
        }
        fee = _finite_or_none(fill.get("fee"))
        if fee is not None:
            clean["fee"] = fee
        execution["fill"] = clean
    else:
        payload["error"] = order_result.get("detail") or f"Robinhood order outcome {outcome}"
    return payload


def _not_sent(detail, ref_id=None):
    return {"outcome": "not_sent", "detail": detail, "order": {}, "ref_id": ref_id,
            "cancel": {"requested": False, "acknowledged": False}}


def _live_adapter(deadline):
    from adapter import RobinhoodExchangeAdapter

    adapter = RobinhoodExchangeAdapter(mode="live", login_deadline=deadline)
    if not adapter.is_live:
        raise RuntimeError("Robinhood adapter not live — set ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET")
    return adapter


def _emit_order(emit, payload):
    emit(payload)
    if payload.get("order_outcome") not in _CONFIRMED_FILL_OUTCOMES:
        sys.exit(1)


def _parse_sell_ask(quantity_text):
    from adapter import _parse_exact_decimal

    asked = _parse_exact_decimal(quantity_text)
    if asked is None or asked <= 0:
        raise ValueError(f"--quantity {quantity_text!r} is not a bounded finite positive decimal")
    return asked


def _float_ask(quantity_text):
    try:
        value = float(quantity_text)
    except (TypeError, ValueError):
        return None
    return value if math.isfinite(value) else None


def run_execute(symbol, side, amount_usd, quantity_text, mode, ref_id=None, budget_ms=None, close_kind="partial", sole_owner=False):
    with json_stdout() as emit:
        action = (side or "").lower()
        quantity = _float_ask(quantity_text) if action == "sell" else None
        asked = None
        if action == "sell":
            try:
                asked = _parse_sell_ask(quantity_text)
            except Exception as e:
                _emit_order(emit, _order_envelope(_not_sent(str(e), ref_id), action, symbol, amount_usd, quantity))
                return
        if mode != "live":
            _emit_order(emit, _order_envelope(_not_sent("--execute requires --mode=live", ref_id), action, symbol, amount_usd, quantity))
            return
        try:
            deadline = _budget_deadline(budget_ms)
            if not ref_id:
                raise ValueError("--ref-id is required")
            adapter = _live_adapter(deadline)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            _emit_order(emit, _order_envelope(_not_sent(str(e), ref_id), action, symbol, amount_usd, quantity))
            return
        try:
            if action == "buy":
                result = adapter.place_market_order(symbol, "buy", ref_id=ref_id, deadline=deadline, amount_usd=amount_usd)
            else:
                result = adapter.place_market_order(
                    symbol, "sell", ref_id=ref_id, deadline=deadline, quantity=float(asked),
                    sell_policy={"asked": asked, "close_kind": close_kind, "sole_owner": sole_owner is True},
                )
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            result = _not_sent(str(e), ref_id)
        if result.get("detail"):
            print(f"[robinhood] order {result.get('outcome')}: {result['detail']}", file=sys.stderr)
        _emit_order(emit, _order_envelope(result, action, symbol, amount_usd, quantity))


def run_resolve_order(args):
    with json_stdout() as emit:
        action = (args.side or "").lower()
        if args.mode != "live":
            _emit_order(emit, _order_envelope({"outcome": "unknown", "detail": "--resolve-order requires --mode=live", "ref_id": args.ref_id}, action, args.symbol))
            return
        try:
            deadline = _budget_deadline(args.budget_ms)
            adapter = _live_adapter(deadline)
            result = adapter.resolve_order(
                args.symbol, action,
                ref_id=args.ref_id,
                deadline=deadline,
                submitted_after_ms=args.submitted_after_ms,
                submitted_before_ms=args.submitted_before_ms,
                order_id=args.order_id or None,
                requested_quantity=args.requested_quantity if args.requested_quantity and args.requested_quantity > 0 else None,
                expected_account_id=args.expected_account_id or None,
                cancel_open=args.cancel_open,
                verify_fields=args.verify_order_fields,
            )
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            result = {"outcome": "unknown", "detail": f"resolution failed: {e}", "ref_id": args.ref_id}
        if result.get("detail"):
            print(f"[robinhood] resolution {result.get('outcome')}: {result['detail']}", file=sys.stderr)
        _emit_order(emit, _order_envelope(result, action, args.symbol))


def main():
    with json_stdout() as emit:
        if "--resolve-order" in sys.argv:
            import argparse
            parser = argparse.ArgumentParser()
            parser.add_argument("--resolve-order", action="store_true")
            parser.add_argument("--symbol", required=True)
            parser.add_argument("--side", required=True, choices=["buy", "sell"])
            parser.add_argument("--ref-id", required=True)
            parser.add_argument("--budget-ms", type=int, required=True)
            parser.add_argument("--submitted-after-ms", type=int, required=True)
            parser.add_argument("--submitted-before-ms", type=int, required=True)
            parser.add_argument("--order-id", default="")
            parser.add_argument("--requested-quantity", type=float, default=0)
            parser.add_argument("--expected-account-id", default="")
            parser.add_argument("--cancel-open", action="store_true")
            parser.add_argument("--verify-order-fields", action="store_true")
            parser.add_argument("--mode", default="live")
            parser.add_argument("--probe-only", action="store_true")
            args = parser.parse_args()
            if args.probe_only:
                sys.exit(0)
            run_resolve_order(args)
        elif "--execute" in sys.argv:
            import argparse
            parser = argparse.ArgumentParser()
            parser.add_argument("--execute", action="store_true")
            parser.add_argument("--symbol", required=True)
            parser.add_argument("--side", required=True, choices=["buy", "sell"])
            parser.add_argument("--amount_usd", type=float, default=0)
            parser.add_argument("--quantity", type=str, default="0")
            parser.add_argument("--close-kind", choices=["whole", "partial"], default="partial")
            parser.add_argument("--sole-owner", action="store_true")
            parser.add_argument("--mode", default="live")
            parser.add_argument("--ref-id", required=True)
            parser.add_argument("--budget-ms", type=int, required=True)
            parser.add_argument("--probe-only", action="store_true")
            args = parser.parse_args()
            if args.probe_only:
                sys.exit(0)
            run_execute(args.symbol, args.side, args.amount_usd, args.quantity, args.mode, args.ref_id, args.budget_ms,
                        close_kind=args.close_kind, sole_owner=args.sole_owner)
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
                help="#1712: accepted for argv-shape compatibility; this platform refuses closed-bar decisions.")
            parser.add_argument("--decision-regime-timeframe", default="",
                help="#1712: accepted for argv-shape compatibility; this platform refuses closed-bar decisions.")
            parser.add_argument("--basket-member", action="store_true", default=False,
                help="#1781: accepted for argv-shape compatibility with the startup probe; basket members run only on Hyperliquid, so this platform refuses it.")
            parser.add_argument("--probe-only", action="store_true",
                help="Startup compatibility probe (#645): validate argv shape and exit 0.")
            args = parser.parse_args()
            if args.probe_only:
                sys.exit(0)
            if args.basket_member:
                emit({
                    "strategy": args.strategy,
                    "symbol": args.symbol,
                    "timeframe": args.timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "regime": None,
                    "mode": args.mode,
                    "platform": "robinhood",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": "basket members are not supported on robinhood: the basket coordinator runs only Hyperliquid members",
                })
                sys.exit(1)
            if args.closed_bar_decisions:
                emit({
                    "strategy": args.strategy,
                    "symbol": args.symbol,
                    "timeframe": args.timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "regime": None,
                    "mode": args.mode,
                    "platform": "robinhood",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": "closed_bar_decisions is not supported on robinhood: Robinhood candles carry no verified closure contract that accounts for trading sessions",
                })
                sys.exit(1)
            from strategy_composition import parse_allow_no_edge_tokens, parse_raw_gate_mode, parse_strategy_refs_arg
            refs = parse_strategy_refs_arg(args.strategy_refs)
            open_strategy_name = refs["open_name"] if refs else args.open_strategy
            close_strategies_arg = refs["close_csv"] if refs else args.close_strategies
            params_parsed = refs["open_params"] if refs else (json.loads(args.params) if args.params else None)
            close_params_by_name = refs["close_params_by_name"] if refs else None
            position_ctx = _position_ctx_from_args(args)
            regime_windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
            run_signal_check(
                args.strategy, args.symbol, args.timeframe, args.mode,
                args.htf_filter, params_parsed, open_strategy_name,
                close_strategies_arg,
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
            )


if __name__ == "__main__":
    main()
