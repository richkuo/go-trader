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
    declare_frame_cadence,
    fetch_rows_or_hold,
    hold_metadata,
    htf_closed_fetcher,
    select_opening_time,
    timings_timestamp_kind,
    unsupported_open_strategy_reason,
)
from htf_filter import (
    HTF_EMA_PERIOD,
    HTF_EXCLUDED_DEFAULT,
    get_default_htf,
    htf_not_evaluated_block,
    htf_skipped_block,
    htf_unavailable_block,
    run_htf_filter,
)
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime

TEXT_INDICATOR_COLUMNS = ("eom_reason",)

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

        df = declare_frame_cadence(_make_dataframe(candles), timeframe, getattr(adapter, "OHLCV_TIMESTAMP_KIND", None))
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
        htf_applies = htf_filter_enabled and htf_strategy_name not in HTF_EXCLUDED_DEFAULT
        htf_block = None
        if htf_filter_enabled and not htf_applies:
            htf_block = htf_skipped_block(timeframe, htf_strategy_name)
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
                            timeframe=regime_tf, cutoff_ms=selection.boundary_ms, keep=ohlcv_limit, min_rows=1).rows
                    decision_regime_payload, _decision_live, decision_strategy_regime = prepare_check_regime(
                        _make_dataframe(regime_rows),
                        regime_enabled=True,
                        windows_spec=regime_windows_spec,
                        injected_payload_json=None,
                    )
                    decision_params["regime"] = decision_strategy_regime
                else:
                    decision_params["regime"] = strategy_regime
                if htf_applies:
                    htf_tf = get_default_htf(timeframe)
                    try:
                        htf_frame = _make_dataframe(select_opening_time(
                            fetch_rows_or_hold(lambda: adapter.fetch_candles(symbol, htf_tf, 61, inst_type=inst_type), symbol, htf_tf),
                            timeframe=htf_tf, cutoff_ms=selection.boundary_ms, keep=60, min_rows=HTF_EMA_PERIOD).rows)
                    except ClosedBarHold as e:
                        htf_block = htf_unavailable_block(symbol, htf_tf, e.reason)
                        raise
                decision_df = declare_frame_cadence(_make_dataframe(selection.rows), timeframe,
                                                    timings_timestamp_kind(selection.rows, selection.timings))
                closed_meta = selection.metadata()
            except ClosedBarHold as e:
                hold = e.reason
            protection_kwargs = {"protection_df": df, "protection_params": strategy_params}
            if hold:
                closed_meta = hold_metadata(hold, cutoff_ms)
                decision_regime_payload = None
                decision_df = None
                print(f"Closed-bar decision held for {symbol} {timeframe}: {hold}", file=sys.stderr)
                if htf_applies and htf_block is None:
                    htf_block = htf_not_evaluated_block(
                        timeframe, f"the closed-bar decision held before the higher-timeframe filter ran: {hold}")
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

        htf_indicators = {}
        if decision_df is not None and htf_applies and htf_block is None:
            if closed_bar:
                _fetch_htf = htf_closed_fetcher(htf_frame)
            else:
                def _fetch_htf(sym, tf, limit):
                    if inst_type == "swap":
                        candles = adapter.get_perp_ohlcv(sym, interval=tf, limit=limit)
                    else:
                        candles = adapter.get_ohlcv(sym, interval=tf, limit=limit)
                    return _make_dataframe(candles) if candles else None

            signal, htf_block, htf_indicators = run_htf_filter(signal, symbol, timeframe, _fetch_htf)
        if htf_filter_enabled and htf_block is None:
            htf_block = htf_not_evaluated_block(timeframe, "the check produced no decision frame for the higher-timeframe filter")

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
        indicator_labels = {}
        skip_cols = {
            "open", "high", "low", "close", "volume",
            "timestamp", "signal", "position", "datetime",
        }
        for col in (result_df.columns if last is not None else []):
            if col in skip_cols:
                continue
            val = last.get(col)
            if col in TEXT_INDICATOR_COLUMNS:
                if isinstance(val, str):
                    indicator_labels[col] = val
                continue
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
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if indicator_labels:
            output["indicator_labels"] = indicator_labels
        if decision:
            output.update(decision)
        if htf_block is not None:
            output["htf_filter"] = htf_block
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


PAIR_MANAGEMENT_INPUT_VERSION = 1


def parse_pair_management_input(raw):
    payload = json.loads(raw)
    if not isinstance(payload, dict):
        raise ValueError("the frozen management input must be a JSON object")
    if payload.get("v") != PAIR_MANAGEMENT_INPUT_VERSION:
        raise ValueError(f"the frozen management input needs version {PAIR_MANAGEMENT_INPUT_VERSION}, got {payload.get('v')!r}")
    candles = payload.get("candles")
    if not isinstance(candles, list) or len(candles) < 30:
        raise ValueError(f"the frozen management input needs at least 30 candles, got {len(candles) if isinstance(candles, list) else 0}")
    rows = []
    for i, row in enumerate(candles):
        if not isinstance(row, list) or len(row) != 6:
            raise ValueError(f"candle {i} must be [open_ms, open, high, low, close, volume]")
        try:
            vals = [float(v) for v in row]
        except (TypeError, ValueError) as exc:
            raise ValueError(f"candle {i} is not numeric: {exc}") from exc
        if not all(math.isfinite(v) for v in vals):
            raise ValueError(f"candle {i} is not finite")
        rows.append([int(row[0])] + vals[1:])
    for a, b in zip(rows, rows[1:]):
        if b[0] <= a[0]:
            raise ValueError("frozen candles must be strictly increasing by open time")
    mark = payload.get("mark_price")
    if not isinstance(mark, (int, float)) or isinstance(mark, bool) or not math.isfinite(float(mark)) or float(mark) <= 0:
        raise ValueError("the frozen management input needs a finite positive mark_price")
    return {"candles": rows, "mark_price": float(mark)}


def run_pair_member_check(strategy_name, symbol, timeframe, mode, inst_type, pair_member,
                          open_strategy=None, close_strategies=None, close_params_by_name=None,
                          position_side="", position_ctx=None, ohlcv_limit=200, atr_method="simple",
                          htf_filter_enabled=False, invert_present=False, frozen_input=None):
    try:
        from close_registry_loader import evaluate as close_evaluate, get_strategy as get_close_strategy
        from strategy_composition import (
            evaluate_open_close,
            finalize_decision,
            parse_close_strategies,
            parse_pair_member_request,
            resolve_pair_member_close_references,
        )
        request = parse_pair_member_request(pair_member, "okx")
        if inst_type != "swap":
            raise ValueError("a pair member check runs only on the OKX swap instrument type")
        if htf_filter_enabled:
            raise ValueError("a pair member check has no higher-timeframe entry filter")
        if invert_present:
            raise ValueError("a pair member check has no open signal to invert")
        resolve_pair_member_close_references(strategy_name, open_strategy, close_strategies, None, get_close_strategy)
        if frozen_input is not None:
            candles = frozen_input["candles"][-ohlcv_limit:]
            mark = frozen_input["mark_price"]
        else:
            from adapter import OKXExchangeAdapter
            adapter = OKXExchangeAdapter()
            candles = adapter.get_perp_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)
            mark = 0.0
            try:
                mark = float(adapter.get_perp_price(symbol))
            except Exception:
                mark = 0.0
        if not candles or len(candles) < 30:
            raise ValueError(f"Insufficient data: {len(candles) if candles else 0} candles")
        df = _make_dataframe(candles)
        if not mark or mark <= 0:
            mark = float(df["close"].iloc[-1])
        market_ctx = {"mark_price": mark}
        atr_now = latest_atr(df, method=atr_method)
        if atr_now > 0:
            market_ctx["atr"] = atr_now

        def _no_open(*_args, **_kwargs):
            raise ValueError("a pair member runs no entry evaluator")

        evaluation = evaluate_open_close(
            _no_open,
            _no_open,
            df,
            strategy_name,
            open_strategy,
            parse_close_strategies(close_strategies),
            position_side,
            None,
            position_ctx,
            close_evaluate=close_evaluate,
            market_ctx=market_ctx,
            close_params_by_name=close_params_by_name,
            management_only=True,
        )
        decision = finalize_decision(evaluation, position_side, 0)
        result_df = df.copy()
        ensure_atr_indicator(result_df, method=atr_method)
        indicators = {}
        last = result_df.iloc[-1]
        for col in result_df.columns:
            if col in ("open", "high", "low", "close", "volume", "timestamp", "datetime"):
                continue
            try:
                fval = float(last.get(col))
            except (TypeError, ValueError):
                continue
            if math.isfinite(fval):
                indicators[col] = round(fval, 6)
        output = {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": decision["signal"],
            "price": mark,
            "indicators": indicators,
            "regime": None,
            "mode": mode,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "pair_member": dict(request),
        }
        output.update(decision)
        output["signal"] = decision["signal"]
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


OKX_ENTRY_TERMINAL_STATUSES = ("closed", "canceled", "cancelled", "expired", "rejected")


def _okx_order_float(order, key):
    value = _float_or_none(order.get(key)) if isinstance(order, dict) else None
    if value is None or not math.isfinite(value):
        return None
    return value


def okx_entry_order_evidence(order):
    if not isinstance(order, dict):
        raise ValueError(f"order lookup returned no object: {order!r}")
    status = str(order.get("status") or "").lower()
    filled = _okx_order_float(order, "filled")
    average = _okx_order_float(order, "average")
    fee = _extract_fee(order)
    if fee is not None and not math.isfinite(fee):
        fee = None
    return {
        "found": True,
        "status": status,
        "order_id": str(order.get("id") or ""),
        "terminal": status in OKX_ENTRY_TERMINAL_STATUSES,
        "executed_sz": filled if filled is not None and filled > 0 else 0.0,
        "avg_px": average if average is not None and average > 0 else 0.0,
        "fee": fee if fee is not None else 0.0,
        "fee_known": fee is not None,
        "filled_known": filled is not None,
    }


def okx_entry_outcome(evidence):
    if not evidence.get("found") or not evidence.get("filled_known"):
        return "unknown"
    if not evidence.get("terminal"):
        return "unknown"
    if evidence.get("executed_sz", 0.0) > 0:
        if evidence.get("avg_px", 0.0) > 0 and evidence.get("fee_known"):
            return "filled"
        return "unknown"
    return "rejected"


def _okx_entry_create_error_outcome(exc):
    try:
        import ccxt
    except ImportError:
        return "unknown"
    if isinstance(exc, ccxt.NetworkError):
        return "unknown"
    if isinstance(exc, (ccxt.DuplicateOrderId, ccxt.OrderNotFound)):
        return "confirm"
    if isinstance(exc, (ccxt.InsufficientFunds, ccxt.BadRequest, ccxt.AuthenticationError,
                        ccxt.ArgumentsRequired, ccxt.InvalidOrder)):
        return "rejected"
    if isinstance(exc, ccxt.ExchangeError):
        return "confirm"
    return "unknown"


def _confirm_okx_entry(adapter, symbol, inst_type, client_order_id, attempts=4, delay_s=0.5):
    last_err = ""
    for attempt in range(attempts):
        try:
            evidence = okx_entry_order_evidence(adapter.fetch_order_by_client_id(symbol, inst_type, client_order_id))
            if okx_entry_outcome(evidence) != "unknown":
                return evidence, ""
            last_err = f"order {evidence.get('order_id')} is {evidence.get('status')!r}"
        except Exception as e:
            last_err = str(e)
        if attempt + 1 < attempts:
            time.sleep(delay_s)
    return None, last_err


def okx_entry_deadline_error(client_order_id, exp_time_ms, now_ms):
    if not client_order_id:
        if exp_time_ms:
            return "--exp-time-ms requires --client-order-id"
        return ""
    try:
        deadline = int(exp_time_ms or 0)
    except (TypeError, ValueError):
        return f"--exp-time-ms must be an integer, got {exp_time_ms!r}"
    if deadline <= 0:
        return "--client-order-id requires --exp-time-ms so an unreachable order can be proven never accepted"
    if deadline <= now_ms:
        return f"the order deadline {deadline} has already passed (now {now_ms})"
    return ""


def _okx_entry_fill_output(is_buy, symbol, size, client_order_id, evidence):
    outcome = okx_entry_outcome(evidence)
    out = {
        "execution": {
            "action": "buy" if is_buy else "sell",
            "symbol": symbol,
            "size": size,
            "fill": {
                "avg_px": evidence["avg_px"],
                "total_sz": evidence["executed_sz"],
                "fee": evidence["fee"],
                "oid": evidence["order_id"],
            },
        },
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "order_outcome": outcome,
        "order_status": evidence["status"],
        "client_order_id": client_order_id,
    }
    if outcome != "filled":
        out["execution"] = None
        out["error"] = f"order {evidence['order_id']} ended {evidence['status']!r} with no fill"
    return out


def run_execute(symbol, side, size, mode, inst_type="swap", client_order_id="", reduce_only=False, exp_time_ms=0):
    if mode != "live":
        out = {"error": "--execute requires --mode=live"}
        if client_order_id:
            out["order_outcome"] = "not_sent"
            out["client_order_id"] = client_order_id
        print(json.dumps(out))
        sys.exit(1)
    if reduce_only and inst_type != "swap":
        print(json.dumps({"error": "--reduce-only applies to swap closes only", "order_outcome": "not_sent"}))
        sys.exit(1)
    if client_order_id:
        cid = str(client_order_id)
        if not (1 <= len(cid) <= 32) or not cid.isalnum() or cid.lower() != cid:
            print(json.dumps({"error": f"--client-order-id must be 1-32 lowercase letters or digits, got {cid!r}",
                              "order_outcome": "not_sent", "client_order_id": cid}))
            sys.exit(1)
        if reduce_only:
            print(json.dumps({"error": "--client-order-id tags a guarded entry and cannot be reduce-only",
                              "order_outcome": "not_sent", "client_order_id": cid}))
            sys.exit(1)
    deadline_err = okx_entry_deadline_error(client_order_id, exp_time_ms, int(time.time() * 1000))
    if deadline_err:
        out = {"error": deadline_err, "order_outcome": "not_sent"}
        if client_order_id:
            out["client_order_id"] = str(client_order_id)
        print(json.dumps(out))
        sys.exit(1)

    adapter = None
    sent = False
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()

        is_buy = side.lower() == "buy"
        if client_order_id:
            try:
                sent = True
                result = adapter.market_open(symbol, is_buy, size, inst_type=inst_type,
                                             client_order_id=client_order_id, exp_time_ms=int(exp_time_ms))
            except Exception as ce:
                traceback.print_exc(file=sys.stderr)
                create_outcome = _okx_entry_create_error_outcome(ce)
                if create_outcome == "confirm":
                    evidence, confirm_err = _confirm_okx_entry(adapter, symbol, inst_type, client_order_id)
                    if evidence is not None:
                        out = _okx_entry_fill_output(is_buy, symbol, size, client_order_id, evidence)
                        out["create_error"] = str(ce)
                        print(json.dumps(out))
                        sys.exit(0 if out["order_outcome"] == "filled" else 1)
                    print(json.dumps({
                        "execution": None,
                        "platform": "okx",
                        "timestamp": datetime.now(timezone.utc).isoformat(),
                        "error": f"order placement failed ({ce}) and the order read-back by client id gave no outcome: {confirm_err}",
                        "order_outcome": "unknown",
                        "client_order_id": client_order_id,
                    }))
                    sys.exit(1)
                print(json.dumps({
                    "execution": None,
                    "platform": "okx",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"order placement failed: {ce}",
                    "order_outcome": create_outcome,
                    "client_order_id": client_order_id,
                }))
                sys.exit(1)
            evidence, confirm_err = _confirm_okx_entry(adapter, symbol, inst_type, client_order_id)
            if evidence is None:
                print(json.dumps({
                    "execution": None,
                    "platform": "okx",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"order {result.get('id') if isinstance(result, dict) else ''} has no confirmed outcome: {confirm_err}",
                    "order_outcome": "unknown",
                    "client_order_id": client_order_id,
                }))
                sys.exit(1)
            out = _okx_entry_fill_output(is_buy, symbol, size, client_order_id, evidence)
            print(json.dumps(out))
            if out["order_outcome"] != "filled":
                sys.exit(1)
            return

        if reduce_only:
            result = adapter.market_open(symbol, is_buy, size, inst_type=inst_type, reduce_only=True)
        else:
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

    except SystemExit:
        raise
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        payload = {
            "execution": None,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }
        if client_order_id:
            payload["order_outcome"] = "unknown" if sent else "not_sent"
            payload["client_order_id"] = client_order_id
        print(json.dumps(payload))
        sys.exit(1)


OKX_FILLS_RECENT_MS = 3 * 24 * 3600 * 1000
OKX_FILLS_HISTORY_MS = 90 * 24 * 3600 * 1000
OKX_FILLS_MARGIN_MS = 3600 * 1000
OKX_FILLS_PAGE_LIMIT = 100
OKX_FILLS_MAX_PAGES = 50


def okx_account_key(inst_type):
    return _pair_orders().okx_account_key(inst_type)


def scan_okx_client_fills(adapter, inst_type, client_order_id, since_ms, now_ms, until_ms=0, rows=None):
    coverage = {
        "complete": False,
        "reason": "",
        "from_ms": int(since_ms),
        "through_ms": 0,
        "pages": 0,
        "rows": 0,
        "matched_count": 0,
        "matched_qty": 0.0,
        "source": "",
    }
    if since_ms <= 0:
        coverage["reason"] = "the attempt window has no start"
        return coverage, 0.0, 0.0
    if since_ms < now_ms - OKX_FILLS_HISTORY_MS + OKX_FILLS_MARGIN_MS:
        coverage["reason"] = "the attempt window starts before the 3-month fill history the venue retains"
        return coverage, 0.0, 0.0
    history = since_ms < now_ms - OKX_FILLS_RECENT_MS + OKX_FILLS_MARGIN_MS
    coverage["source"] = "trade/fills-history" if history else "trade/fills"
    end_ms = min(int(until_ms), int(now_ms)) if until_ms and until_ms > 0 else int(now_ms)
    notional = 0.0
    fee = 0.0
    try:
        return _scan_okx_client_fill_pages(adapter, inst_type, client_order_id, since_ms, end_ms, history, coverage, rows)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        coverage["complete"] = False
        coverage["through_ms"] = 0
        coverage["reason"] = f"the fill history query failed: {e}"
        coverage["query_error"] = str(e)
        return coverage, notional, fee


def _scan_okx_client_fill_pages(adapter, inst_type, client_order_id, since_ms, end_ms, history, coverage, rows=None):
    notional = 0.0
    fee = 0.0
    after = ""
    while True:
        if coverage["pages"] >= OKX_FILLS_MAX_PAGES:
            coverage["reason"] = f"the fill history needs more than {OKX_FILLS_MAX_PAGES} pages"
            return coverage, notional, fee
        page = adapter.fetch_fills_page(inst_type, since_ms, end_ms, OKX_FILLS_PAGE_LIMIT, after=after, history=history)
        coverage["pages"] += 1
        if not isinstance(page, list):
            raise ValueError(f"fills query returned no list: {page!r}")
        for row in page:
            if not isinstance(row, dict):
                raise ValueError(f"fills query returned a non-object row: {row!r}")
            if "clOrdId" not in row:
                coverage["reason"] = "a fill row carries no client order id field"
                return coverage, notional, fee
            coverage["rows"] += 1
            if str(row.get("clOrdId") or "") != client_order_id:
                continue
            sz = _okx_order_float(row, "fillSz")
            px = _okx_order_float(row, "fillPx")
            if sz is None or px is None or sz <= 0 or px <= 0:
                raise ValueError(f"a matching fill carries no usable size or price: {row!r}")
            coverage["matched_count"] += 1
            coverage["matched_qty"] += sz
            if rows is not None:
                rows.append(dict(row))
            notional += sz * px
            row_fee = _okx_order_float(row, "fee")
            if row_fee is not None:
                fee += -row_fee
        if len(page) < OKX_FILLS_PAGE_LIMIT:
            coverage["complete"] = True
            coverage["through_ms"] = int(end_ms)
            return coverage, notional, fee
        after = str(page[-1].get("billId") or "")
        if not after:
            coverage["reason"] = "a full fill page carries no bill id, so the next page cannot be read"
            return coverage, notional, fee


def run_recover_entry(symbol, inst_type, client_order_id, mode, since_ms=0, until_ms=0):
    out = {
        "platform": "okx",
        "symbol": symbol,
        "client_order_id": client_order_id,
        "checked_at_ms": int(time.time() * 1000),
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if mode != "live":
        out["error"] = "--recover-entry requires --mode=live"
        print(json.dumps(out))
        sys.exit(1)
    try:
        import ccxt
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        out["account"] = okx_account_key(inst_type)
        now_ms = int(time.time() * 1000)
        coverage, scan_notional, scan_fee = scan_okx_client_fills(adapter, inst_type, client_order_id, int(since_ms or 0), now_ms, int(until_ms or 0))
        try:
            order = adapter.fetch_order_by_client_id(symbol, inst_type, client_order_id)
        except ccxt.OrderNotFound:
            order = None
        if order is None:
            qty = coverage["matched_qty"]
            evidence = {
                "found": False,
                "executed_sz": 0.0,
                "fill_qty": qty,
                "fill_notional": scan_notional,
                "fill_fee": scan_fee,
                "fill_count": coverage["matched_count"],
                "fill_avg_px": (scan_notional / qty) if qty > 0 else 0.0,
                "fills_complete": False,
                "coverage": coverage,
            }
            out["order"] = evidence
            out["checked_at_ms"] = int(time.time() * 1000)
            print(json.dumps(out))
            return
        evidence = okx_entry_order_evidence(order)
        qty = notional = fee = 0.0
        count = 0
        if evidence["order_id"] and evidence["executed_sz"] > 0:
            for t in adapter.fetch_order_trades(symbol, inst_type, evidence["order_id"]) or []:
                amount = _okx_order_float(t, "amount")
                price = _okx_order_float(t, "price")
                if amount is None or price is None or amount <= 0 or price <= 0:
                    raise ValueError(f"order trade carries no usable amount or price: {t!r}")
                qty += amount
                notional += amount * price
                trade_fee = _extract_fee(t)
                if trade_fee is not None and math.isfinite(trade_fee):
                    fee += trade_fee
                count += 1
        evidence["fill_qty"] = qty
        evidence["fill_notional"] = notional
        evidence["fill_fee"] = fee
        evidence["fill_count"] = count
        evidence["fill_avg_px"] = (notional / qty) if qty > 0 else 0.0
        tolerance = max(1e-9, abs(evidence["executed_sz"]) * 1e-6)
        evidence["fills_complete"] = abs(qty - evidence["executed_sz"]) <= tolerance
        evidence["coverage"] = coverage
        out["order"] = evidence
        out["checked_at_ms"] = int(time.time() * 1000)
        print(json.dumps(out))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        out["error"] = f"entry recovery query failed: {e}"
        print(json.dumps(out))
        sys.exit(1)


PAIR_FILL_MARGIN_MS = 60000
PAIR_CONFIRM_ATTEMPTS = 3
PAIR_CONFIRM_DELAY_S = 0.25
PAIR_OKX_LIVE_STATES = ("live", "partially_filled")
PAIR_OKX_TERMINAL_STATES = ("filled", "canceled", "mmp_canceled")


def _pair_orders():
    import importlib.util
    name = "go_trader_pair_orders"
    mod = sys.modules.get(name)
    if mod is None:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "shared_tools", "pair_orders.py")
        spec = importlib.util.spec_from_file_location(name, path)
        mod = importlib.util.module_from_spec(spec)
        sys.modules[name] = mod
        spec.loader.exec_module(mod)
    return mod


def _pair_okx_fills(po, rows, order_id):
    by_bill = {}
    order = []
    for row in rows:
        if order_id and str(row.get("ordId") or "") != order_id:
            continue
        bill = row.get("billId")
        if bill in (None, ""):
            raise ValueError("a matching fill row carries no bill id")
        key = str(bill)
        body = json.dumps(row, sort_keys=True, separators=(",", ":"), default=str)
        if key in by_bill:
            if by_bill[key][1] != body:
                raise ValueError(f"fill {key} was returned twice with different bodies")
            continue
        by_bill[key] = (row, body)
        order.append(key)
    out = []
    for key in order:
        row = by_bill[key][0]
        raw_fee = row.get("fee")
        fee_ccy = str(row.get("feeCcy") or "").strip()
        fee_known = raw_fee not in (None, "") and bool(fee_ccy)
        fee = ""
        if fee_known:
            fee = po.decimal_text(-po.exact_decimal(raw_fee, f"fill {key} fee", allow_negative=True))
        when = row.get("fillTime") or row.get("ts")
        out.append(po.fill_record(key, when, row.get("fillSz"), row.get("fillPx"), fee, fee_ccy, fee_known,
                                  order_id=row.get("ordId"), side=row.get("side")))
    out.sort(key=lambda r: (r["venue_time_ms"], r["fill_id"]))
    return out


def pair_okx_outcome(adapter, po, req, mode, account, since_ms, inst=None, refusal=""):
    import ccxt
    inst_id, cid = req["instrument"], req["client_order_id"]
    venue = {"account_key": account, "price_currency": "USDT", "native_unit": "contract"}
    try:
        if inst is None:
            inst = adapter.pair_instrument(inst_id)
        venue["contract_size"] = po.decimal_text(po.exact_decimal(inst["ctVal"], "ctVal", positive=True))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        return po.outcome(mode, "unknown", f"the instrument read failed: {e}", req, venue=venue)
    try:
        order = adapter.fetch_pair_order(inst_id, cid)
    except ccxt.OrderNotFound:
        order = None
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        return po.outcome(mode, "unknown", f"the order read failed: {e}", req, venue=venue)
    rows = []
    now = po.now_ms()
    coverage, _, _ = scan_okx_client_fills(adapter, "swap", cid, int(since_ms), now, 0, rows=rows)
    cov = {"complete": bool(coverage["complete"]), "reason": coverage["reason"], "from_ms": int(coverage["from_ms"]),
           "through_ms": int(coverage["through_ms"] if coverage["complete"] else 0), "pages": int(coverage["pages"])}
    try:
        if order is None:
            if rows:
                return po.outcome(mode, "unknown", "fills name this client order id, but the venue holds no order record for it", req,
                                  venue=venue, fill_coverage=cov, contrary_evidence=True)
            if mode == "order":
                if refusal:
                    return po.outcome(mode, "rejected", f"the venue refused the order with no fill: {refusal}", req, venue=venue, fill_coverage=cov, terminal=True)
                return po.outcome(mode, "unknown", "the venue returned no order record after the placement", req, venue=venue, fill_coverage=cov)
            return po.outcome(mode, "absent", "the venue holds no order record for this client order id", req, venue=venue, fill_coverage=cov)
        state = str(order.get("state") or "")
        oid = str(order.get("ordId") or "")
        size = po.exact_decimal(order.get("sz"), "sz", positive=True)
        executed = po.exact_decimal(order.get("accFillSz") or "0", "accFillSz")
        venue.update({
            "instrument": str(order.get("instId") or ""),
            "client_order_id": str(order.get("clOrdId") or ""),
            "venue_order_id": oid,
            "side": str(order.get("side") or ""),
            "native_qty": po.decimal_text(size),
            "price": po.decimal_text(po.exact_decimal(order.get("px"), "px", positive=True)),
            "tif": str(order.get("ordType") or ""),
            "status": state,
        })
        ro = order.get("reduceOnly")
        if ro in ("true", "false"):
            venue["reduce_only"] = ro == "true"
        accepted = int(order.get("cTime") or 0)
        fills = _pair_okx_fills(po, rows, oid)
    except (ValueError, TypeError, po.PairOrderError) as e:
        traceback.print_exc(file=sys.stderr)
        return po.outcome(mode, "unknown", f"the venue evidence is malformed: {e}", req, venue=venue, fill_coverage=cov)
    qty, notional = po.sum_fills(fills)
    base = {"venue": venue, "fill_coverage": cov, "accepted_at_ms": accepted, "fills": fills}
    if qty > executed:
        return po.outcome(mode, "unknown", f"the fills sum to {qty}, above the executed size {executed}", req, **base)
    cum = {"cum_native_qty": po.decimal_text(qty), "cum_native_notional": po.decimal_text(notional)}
    if state in PAIR_OKX_LIVE_STATES:
        return po.outcome(mode, "open", f"the order is {state}", req, **base, **cum)
    if state not in PAIR_OKX_TERMINAL_STATES:
        return po.outcome(mode, "unknown", f"unfamiliar order state {state!r}", req, **base)
    if executed == 0:
        if qty > 0:
            return po.outcome(mode, "unknown", "fills exist for an order the venue reports unfilled", req, **base)
        return po.outcome(mode, "rejected", f"the order ended {state} with no fill", req, terminal=True, **base)
    if not (cov["complete"] and qty == executed):
        return po.outcome(mode, "partial" if qty > 0 else "unknown", f"the order ended {state}; {qty} of {executed} executed are visible", req, **base, **cum)
    return po.outcome(mode, "filled" if executed == size else "partial", f"the order ended {state} with {executed} of {size} executed", req, terminal=True, **base, **cum)


def run_pair_order(raw, mode, probe_only=False):
    po = _pair_orders()
    try:
        req = po.parse_request(raw, "okx", route="order")
    except po.PairOrderError as e:
        po.finish({"platform": "okx", "error": str(e), "pair_outcome": po.never_sent("order", f"the request was refused before any adapter or venue call: {e}")}, 1)
        return
    if probe_only:
        sys.exit(0)
    if mode != "live":
        po.finish({"platform": "okx", "error": "--pair-order-json requires --mode=live", "pair_outcome": po.never_sent("order", "--pair-order-json requires --mode=live", req)}, 1)
        return
    deadline = req["accept_deadline_ms"]
    if po.now_ms() >= deadline:
        po.finish({"platform": "okx", "pair_outcome": po.never_sent("order", "the acceptance deadline passed before the adapter was built; nothing was sent", req)})
        return
    refusal = po.reference_refusal(req, po.now_ms())
    if refusal:
        po.finish({"platform": "okx", "pair_outcome": po.never_sent("order", refusal + " (checked before the adapter was built)", req)})
        return
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        account = okx_account_key("swap")
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        po.finish({"platform": "okx", "error": str(e), "pair_outcome": po.never_sent("order", f"the adapter could not be built, so nothing was sent: {e}", req)}, 1)
        return
    if account != req["account_key"]:
        po.finish({"platform": "okx", "account": account, "pair_outcome": po.never_sent("order", f"the credentials answer for account {account!r}, the request is bound to {req['account_key']!r}; nothing was sent", req)})
        return
    native_text, bound_text = po.decimal_text(req["native_qty"]), po.decimal_text(req["price_bound"])
    try:
        check = adapter.pair_order_check(req["instrument"], native_text, bound_text)
        if po.exact_decimal(check["contract_size"], "ctVal", positive=True) != req["contract_size"]:
            raise ValueError(f"the venue contract size {check['contract_size']} is not the request's {po.decimal_text(req['contract_size'])}")
        if check["state"] != "live":
            raise ValueError(f"the instrument state is {check['state']!r}")
        lot = po.exact_decimal(check["lot"], "lotSz", positive=True)
        tick = po.exact_decimal(check["tick"], "tickSz", positive=True)
        if req["native_qty"] % lot != 0 or req["native_qty"] < po.exact_decimal(check["min"], "minSz"):
            raise ValueError(f"native quantity {native_text} is not a legal multiple of lotSz {check['lot']} at or above minSz {check['min']}")
        if req["price_bound"] % tick != 0:
            raise ValueError(f"price bound {bound_text} is not on the tickSz {check['tick']} grid")
        if po.exact_decimal(check["amount_text"], "serialized size", positive=True) != req["native_qty"]:
            raise ValueError(f"the SDK would serialize the size {native_text} as {check['amount_text']}")
        if po.exact_decimal(check["price_text"], "serialized price", positive=True) != req["price_bound"]:
            raise ValueError(f"the SDK would serialize the price {bound_text} as {check['price_text']}")
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        po.finish({"platform": "okx", "account": account, "pair_outcome": po.never_sent("order", f"the saved request does not survive the venue precision check, so nothing was sent: {e}", req)})
        return
    if po.now_ms() >= deadline:
        po.finish({"platform": "okx", "account": account, "pair_outcome": po.never_sent("order", "the acceptance deadline passed immediately before the order call; nothing was sent", req)})
        return
    refusal = po.reference_refusal(req, po.now_ms())
    if refusal:
        po.finish({"platform": "okx", "account": account, "pair_outcome": po.never_sent("order", refusal + " (checked immediately before the order call)", req)})
        return
    since = po.now_ms() - PAIR_FILL_MARGIN_MS
    refusal = ""
    try:
        adapter.pair_limit_ioc(req["instrument"], req["order_side"], native_text, bound_text, req["client_order_id"], req["reduce_only"], deadline)
    except Exception as ce:
        traceback.print_exc(file=sys.stderr)
        kind = _okx_entry_create_error_outcome(ce)
        if kind == "rejected":
            refusal = str(ce)
        elif kind == "unknown":
            po.finish({"platform": "okx", "account": account, "pair_outcome": po.outcome("order", "unknown", f"the placement outcome is unknown: {ce}", req,
                                                                                         venue={"account_key": account, "price_currency": "USDT", "native_unit": "contract"})})
            return
    inst = {"ctVal": check["contract_size"]}
    out = None
    for attempt in range(PAIR_CONFIRM_ATTEMPTS):
        out = pair_okx_outcome(adapter, po, req, "order", account, since, inst=inst, refusal=refusal)
        if out["terminal"] and out["status"] != "unknown":
            break
        if attempt + 1 < PAIR_CONFIRM_ATTEMPTS:
            time.sleep(PAIR_CONFIRM_DELAY_S)
    po.finish({"platform": "okx", "account": account, "pair_outcome": out})


def run_pair_query(raw, since_ms, mode, probe_only=False):
    po = _pair_orders()
    try:
        req = po.parse_request(raw, "okx")
    except po.PairOrderError as e:
        po.finish({"platform": "okx", "error": str(e)}, 1)
        return
    if probe_only:
        sys.exit(0)
    if mode != "live":
        po.finish({"platform": "okx", "error": "--pair-query-json requires --mode=live"}, 1)
        return
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        account = okx_account_key("swap")
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        po.finish({"platform": "okx", "error": f"the adapter could not be built: {e}"}, 1)
        return
    if account != req["account_key"]:
        po.finish({"platform": "okx", "account": account, "error": f"the credentials answer for account {account!r}, the request is bound to {req['account_key']!r}"}, 1)
        return
    po.finish({"platform": "okx", "account": account, "pair_outcome": pair_okx_outcome(adapter, po, req, "query", account, int(since_ms))})


PAIR_OKX_TRADE_PAGE = 100


def pair_okx_native_history(adapter, po, req):
    from decimal import Decimal
    from native_close_evidence import native_history_evidence
    out = po.native_history_output(req)
    match = po.OKX_INST.match(req["instrument"])
    symbol = match.group(1)
    orders = []
    fills = []
    executed = Decimal(0)
    accepted = 0
    for oid in req["venue_order_ids"]:
        try:
            info = adapter.fetch_order_info(symbol, "swap", oid)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            return po.native_history_incomplete(out, f"the order read for {oid} failed: {e}")
        try:
            if str(info.get("ordId") or "") != oid:
                raise ValueError(f"the order read for {oid} returned order {info.get('ordId')!r}")
            state = str(info.get("state") or "")
            ex = po.exact_decimal(info.get("accFillSz") or "0", "accFillSz")
            created = int(info.get("cTime") or 0)
        except (ValueError, TypeError, po.PairOrderError) as e:
            return po.native_history_incomplete(out, f"the order record for {oid} is malformed: {e}")
        orders.append({"order_id": oid, "state": state, "terminal": state in PAIR_OKX_TERMINAL_STATES, "side": str(info.get("side") or ""),
                       "instrument": str(info.get("instId") or ""), "executed_native_qty": po.decimal_text(ex), "accepted_at_ms": created})
        executed += ex
        if created > 0 and (accepted == 0 or created < accepted):
            accepted = created
        try:
            trades = adapter.fetch_order_trades(symbol, "swap", oid)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            out["orders"] = orders
            return po.native_history_incomplete(out, f"the order trade read for {oid} failed: {e}")
        if not isinstance(trades, list):
            out["orders"] = orders
            return po.native_history_incomplete(out, f"the order trade read for {oid} returned no list")
        if len(trades) >= PAIR_OKX_TRADE_PAGE:
            out["orders"] = orders
            return po.native_history_incomplete(out, f"the order trade read for {oid} returned a full page of {len(trades)} rows, so it may be clipped")
        rows = []
        for t in trades:
            row = t.get("info") if isinstance(t, dict) and isinstance(t.get("info"), dict) else t
            if isinstance(row, dict):
                rows.append(row)
        try:
            fills.extend(_pair_okx_fills(po, rows, oid))
        except (ValueError, TypeError, po.PairOrderError) as e:
            out["orders"] = orders
            return po.native_history_incomplete(out, f"the fill records for {oid} are malformed: {e}")
    out["orders"] = orders
    out["coverage"] = {"complete": True, "reason": "", "from_ms": int(req["from_ms"]), "through_ms": po.now_ms(), "pages": len(orders)}
    for o in orders:
        if not o["terminal"]:
            out["coverage"] = {"complete": False, "reason": f"order {o['order_id']} is {o['state']}", "from_ms": int(req["from_ms"]), "through_ms": 0, "pages": len(orders)}
            return po.native_history_incomplete(out, f"order {o['order_id']} is {o['state']!r}, not terminal")
    fills.sort(key=lambda r: (r["venue_time_ms"], r["fill_id"]))
    qty, _ = po.sum_fills(fills)
    if qty != executed:
        out["coverage"]["complete"] = False
        out["coverage"]["through_ms"] = 0
        out["coverage"]["reason"] = f"the fill records cover {qty} of {executed}"
        return po.native_history_incomplete(out, f"the fill records cover {qty} of the {executed} the orders executed")
    out["evidence"] = native_history_evidence(req["venue_order_ids"], accepted, executed, fills)
    out["status"] = "complete"
    out["detail"] = f"{len(orders)} terminal orders executed {executed} with {len(fills)} fill records"
    out["observed_at_ms"] = po.now_ms()
    return out


def run_pair_native_history(raw, mode, probe_only=False):
    po = _pair_orders()
    try:
        req = po.parse_native_history_request(raw, "okx")
    except po.PairOrderError as e:
        po.finish({"platform": "okx", "error": str(e)}, 1)
        return
    if probe_only:
        sys.exit(0)
    if mode != "live":
        po.finish({"platform": "okx", "error": "--pair-native-history-json requires --mode=live"}, 1)
        return
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        account = okx_account_key("swap")
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        po.finish({"platform": "okx", "error": f"the adapter could not be built: {e}"}, 1)
        return
    if account != req["account_key"]:
        po.finish({"platform": "okx", "account": account, "error": f"the credentials answer for account {account!r}, the request is bound to {req['account_key']!r}"}, 1)
        return
    po.finish({"platform": "okx", "account": account, "native_history": pair_okx_native_history(adapter, po, req)})


def main():
    if any(a.startswith("--pair-order-json") or a.startswith("--pair-query-json") or a.startswith("--pair-native-history-json") for a in sys.argv[1:]):
        import argparse
        parser = argparse.ArgumentParser()
        group = parser.add_mutually_exclusive_group(required=True)
        group.add_argument("--pair-order-json", default=None)
        group.add_argument("--pair-query-json", default=None)
        group.add_argument("--pair-native-history-json", default=None)
        parser.add_argument("--since-ms", type=int, default=None)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--inst-type", default="swap", choices=["swap"])
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#1877): parse the strict request and exit 0 before any adapter, clock or venue call.")
        args = parser.parse_args()
        if args.pair_query_json is not None:
            if args.since_ms is None or args.since_ms <= 0:
                parser.error("--pair-query-json needs --since-ms above zero")
            run_pair_query(args.pair_query_json, args.since_ms, args.mode, probe_only=args.probe_only)
        elif args.pair_native_history_json is not None:
            if args.since_ms is not None:
                parser.error("--since-ms applies to --pair-query-json only")
            run_pair_native_history(args.pair_native_history_json, args.mode, probe_only=args.probe_only)
        else:
            if args.since_ms is not None:
                parser.error("--since-ms applies to --pair-query-json only")
            run_pair_order(args.pair_order_json, args.mode, probe_only=args.probe_only)
    elif "--recover-entry" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--recover-entry", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--client-order-id", required=True)
        parser.add_argument("--since-ms", type=int, required=True)
        parser.add_argument("--until-ms", type=int, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#1769): validate argv shape and exit 0 without reading the venue.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_recover_entry(args.symbol, args.inst_type, args.client_order_id, args.mode, since_ms=args.since_ms, until_ms=args.until_ms)
    elif "--execute" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--client-order-id", default="",
                            help="#1769: client order id of a guarded closed-bar entry, saved by Go before submission")
        parser.add_argument("--exp-time-ms", type=int, default=0,
                            help="#1769: OKX expTime request deadline (unix ms) of a guarded closed-bar entry, saved by Go before submission")
        parser.add_argument("--reduce-only", action="store_true", default=False,
                            help="#1769: send a swap close as reduce-only")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#1769): validate execute-mode argv shape and exit 0 without trading.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_execute(args.symbol, args.side, args.size, args.mode, args.inst_type,
                    client_order_id=args.client_order_id, reduce_only=args.reduce_only,
                    exp_time_ms=args.exp_time_ms)
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
        parser.add_argument("--basket-member", action="store_true", default=False,
            help="#1781: accepted for argv-shape compatibility with the startup probe; basket members run only on Hyperliquid, so this platform refuses it.")
        parser.add_argument("--pair-member-json", default=None,
            help="#1862: typed management-only pair member request; native closes, indicators and protection run, no entry evaluator runs, and the request is echoed.")
        parser.add_argument("--management-stdin", action="store_true", default=False,
            help="#1862: read frozen pair member management input (candles and mark) from stdin; no exchange client is constructed.")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#645): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        if args.pair_member_json is not None or args.management_stdin:
            from strategy_composition import parse_strategy_refs_arg
            frozen = None
            error = ""
            if args.pair_member_json is None:
                error = "--management-stdin is accepted only with a pair member request"
            elif args.basket_member:
                error = "a check is either a basket member or a pair member, not both"
            elif args.management_stdin:
                try:
                    frozen = parse_pair_management_input(sys.stdin.read())
                except Exception as e:
                    error = f"invalid frozen management input: {e}"
            if error:
                print(json.dumps({
                    "strategy": args.strategy, "symbol": args.symbol, "timeframe": args.timeframe,
                    "signal": 0, "price": 0, "indicators": {}, "regime": None, "mode": args.mode,
                    "platform": "okx", "timestamp": datetime.now(timezone.utc).isoformat(), "error": error,
                }))
                sys.exit(1)
            refs = parse_strategy_refs_arg(args.strategy_refs)
            run_pair_member_check(
                args.strategy, args.symbol, args.timeframe, args.mode, args.inst_type, args.pair_member_json,
                open_strategy=refs["open_name"] if refs else args.open_strategy,
                close_strategies=refs["close_csv"] if refs else args.close_strategies,
                close_params_by_name=refs["close_params_by_name"] if refs else None,
                position_side=args.position_side,
                position_ctx=_position_ctx_from_args(args),
                ohlcv_limit=args.ohlcv_limit,
                atr_method=args.atr_method,
                htf_filter_enabled=args.htf_filter,
                invert_present=bool(refs and "invert_open_signal" in refs),
                frozen_input=frozen,
            )
            return
        if args.basket_member:
            print(json.dumps({
                "strategy": args.strategy,
                "symbol": args.symbol,
                "timeframe": args.timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "regime": None,
                "mode": args.mode,
                "platform": "okx",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": "basket members are not supported on okx: the basket coordinator runs only Hyperliquid members",
            }))
            sys.exit(1)
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
