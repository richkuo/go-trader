#!/usr/bin/env python3

import sys
import os
import hashlib
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
        if indicator_labels:
            output["indicator_labels"] = indicator_labels
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
    api_key = os.environ.get("OKX_API_KEY", "").strip()
    if not api_key:
        return ""
    network = "sandbox" if os.environ.get("OKX_SANDBOX", "") == "1" else "live"
    fingerprint = hashlib.sha256(("go-trader/okx-api-key/v1:" + api_key).encode()).hexdigest()[:16]
    return f"okx:{inst_type}:{network}:key-{fingerprint}"


def scan_okx_client_fills(adapter, inst_type, client_order_id, since_ms, now_ms):
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
    notional = 0.0
    fee = 0.0
    after = ""
    while True:
        if coverage["pages"] >= OKX_FILLS_MAX_PAGES:
            coverage["reason"] = f"the fill history needs more than {OKX_FILLS_MAX_PAGES} pages"
            return coverage, notional, fee
        page = adapter.fetch_fills_page(inst_type, since_ms, now_ms, OKX_FILLS_PAGE_LIMIT, after=after, history=history)
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
            notional += sz * px
            row_fee = _okx_order_float(row, "fee")
            if row_fee is not None:
                fee += -row_fee
        if len(page) < OKX_FILLS_PAGE_LIMIT:
            coverage["complete"] = True
            coverage["through_ms"] = int(now_ms)
            return coverage, notional, fee
        after = str(page[-1].get("billId") or "")
        if not after:
            coverage["reason"] = "a full fill page carries no bill id, so the next page cannot be read"
            return coverage, notional, fee


def run_recover_entry(symbol, inst_type, client_order_id, mode, since_ms=0):
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
        coverage, scan_notional, scan_fee = scan_okx_client_fills(adapter, inst_type, client_order_id, int(since_ms or 0), now_ms)
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


def main():
    if "--recover-entry" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--recover-entry", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--inst-type", default="swap", choices=["spot", "swap"])
        parser.add_argument("--client-order-id", required=True)
        parser.add_argument("--since-ms", type=int, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#1769): validate argv shape and exit 0 without reading the venue.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_recover_entry(args.symbol, args.inst_type, args.client_order_id, args.mode, since_ms=args.since_ms)
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
