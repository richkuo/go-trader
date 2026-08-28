#!/usr/bin/env python3

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone


class SafeEncoder(json.JSONEncoder):

    def default(self, obj):
        return super().default(obj)

    def encode(self, o):
        return super().encode(self._sanitize(o))

    def _sanitize(self, obj):
        if isinstance(obj, float):
            if math.isnan(obj) or math.isinf(obj):
                return None
            return obj
        if isinstance(obj, dict):
            return {k: self._sanitize(v) for k, v in obj.items()}
        if isinstance(obj, (list, tuple)):
            return [self._sanitize(v) for v in obj]
        return obj

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'hyperliquid'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'futures'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from hl_user_fills import apply_user_fills_lookup
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime


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
    ):
        value = getattr(args, attr, None)
        if value is not None:
            ctx[key] = value
    regime = (getattr(args, "position_regime", "") or "").strip()
    if regime:
        ctx["regime"] = regime
    return ctx


BATCH_PROTOCOL_VERSION = 1


class SharedSignalStateError(Exception):
    pass


class InsufficientCandlesError(SharedSignalStateError):

    def __init__(self, count):
        self.count = int(count)
        super().__init__(f"Insufficient data: {self.count} candles")


FUTURES_STRATEGIES_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "shared_strategies", "open", "futures", "strategies.py")


def _futures_strategies_module():
    try:
        import strategies as mod
        if os.path.realpath(getattr(mod, "__file__", "") or "") == os.path.realpath(FUTURES_STRATEGIES_PATH):
            return mod
    except ImportError:
        pass
    import importlib.util
    spec = importlib.util.spec_from_file_location(
        "_check_hyperliquid_futures_strategies", FUTURES_STRATEGIES_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _signal_check_deps():
    from types import SimpleNamespace

    _strategies = _futures_strategies_module()
    apply_strategy = _strategies.apply_strategy
    get_strategy = _strategies.get_strategy
    list_strategies = _strategies.list_strategies
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
        reject_backtest_only_strategies,
        validate_close_strategy_names,
    )

    return SimpleNamespace(
        apply_strategy=apply_strategy,
        get_strategy=get_strategy,
        list_strategies=list_strategies,
        close_evaluate=close_evaluate,
        get_close_strategy=get_close_strategy,
        list_close_strategies=list_close_strategies,
        evaluate_open_close=evaluate_open_close,
        finalize_decision=finalize_decision,
        normalize_signal=normalize_signal,
        parse_close_strategies=parse_close_strategies,
        reject_backtest_only_strategies=reject_backtest_only_strategies,
        validate_close_strategy_names=validate_close_strategy_names,
    )


def _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies):
    configured_names = [open_strategy or strategy_name]
    deps.reject_backtest_only_strategies(configured_names, deps.get_strategy)
    deps.validate_close_strategy_names(
        deps.parse_close_strategies(close_strategies),
        deps.get_strategy,
        deps.get_close_strategy,
        deps.list_strategies,
        deps.list_close_strategies,
    )


def build_shared_signal_state(symbol, timeframe, *, adapter=None, df=None,
                              ohlcv_limit=200, atr_method="simple", mark_price=0.0,
                              regime_enabled=False, regime_windows_spec=None,
                              regime_payload_json=None, mode="paper",
                              regime_period=14, regime_adx_threshold=20.0):
    if df is None:
        if adapter is None:
            raise SharedSignalStateError("no adapter and no prebuilt DataFrame")
        print(f"Fetching {symbol} {timeframe} from Hyperliquid ({mode})...", file=sys.stderr)
        candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)
        if not candles or len(candles) < 30:
            raise InsufficientCandlesError(len(candles) if candles else 0)
        df = _make_dataframe(candles)

    price_override = 0.0
    if mark_price and mark_price > 0:
        price_override = float(mark_price)
    elif adapter is not None:
        try:
            mid = adapter.get_spot_price(symbol)
            if mid > 0:
                price_override = float(mid)
        except Exception:
            pass

    return {
        "adapter": adapter,
        "symbol": symbol,
        "timeframe": timeframe,
        "mode": mode,
        "df": df,
        "atr_method": atr_method,
        "atr": latest_atr(df, method=atr_method),
        "price_override": price_override,
        "regime_enabled": regime_enabled,
        "regime_windows_spec": regime_windows_spec,
        "regime_payload_json": regime_payload_json,
        "regime_period": regime_period,
        "regime_adx_threshold": regime_adx_threshold,
        "htf_cache": {},
        "funding_scalar": None,
        "funding_records": None,
    }


def _shared_funding_scalar(shared, symbol):
    if shared.get("funding_scalar") is not None:
        return shared["funding_scalar"]
    adapter = shared.get("adapter")
    params = {}
    if adapter is not None:
        try:
            current_rate = adapter.get_funding_rate(symbol)
            history = adapter.get_funding_history(symbol, days=7)
            avg_rate = (sum(r["rate"] for r in history) / len(history)) if history else 0.0
            params = {
                "current_funding_rate": current_rate,
                "avg_funding_rate_7d": avg_rate,
            }
            print(f"Funding rate {symbol}: current={current_rate:.6f} avg7d={avg_rate:.6f}", file=sys.stderr)
        except Exception as e:
            print(f"Warning: failed to fetch funding rate: {e}", file=sys.stderr)
    shared["funding_scalar"] = params
    return params


def _shared_funding_records(shared, symbol):
    if shared.get("funding_records") is not None:
        return shared["funding_records"]
    adapter = shared.get("adapter")
    records = None
    if adapter is not None:
        try:
            start_ms = int(shared["df"]["timestamp"].iloc[0])
            records = adapter.get_funding_history_range(symbol, start_ms)
            print(f"Funding history {symbol}: {len(records)} records since bar0",
                  file=sys.stderr)
        except Exception as e:
            print(f"Warning: failed to fetch funding history: {e}", file=sys.stderr)
    shared["funding_records"] = records if records is not None else []
    return shared["funding_records"]


def _shared_htf_frame(shared, sym, tf, limit):
    cache = shared["htf_cache"]
    key = (sym, tf, limit)
    if key not in cache:
        adapter = shared.get("adapter")
        candles = adapter.get_ohlcv(sym, interval=tf, limit=limit) if adapter is not None else None
        cache[key] = _make_dataframe(candles) if candles else None
    frame = cache[key]
    return frame.copy() if frame is not None else None


def evaluate_signal_slot(shared, slot, deps=None):
    if deps is None:
        deps = _signal_check_deps()

    strategy_name = slot["strategy"]
    mode = slot.get("mode") or shared.get("mode") or "paper"
    open_strategy = slot.get("open_strategy") or None
    close_strategies = slot.get("close_strategies") or None
    close_params_by_name = slot.get("close_params_by_name") or None
    strategy_params_override = slot.get("params") or None
    position_side = slot.get("position_side") or ""
    position_ctx = slot.get("position_ctx") or None
    htf_filter_enabled = bool(slot.get("htf_filter"))
    regime_atr_window = slot.get("regime_atr_window") or ""

    _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies)

    symbol = shared["symbol"]
    timeframe = shared["timeframe"]
    atr_method = shared["atr_method"]
    df = shared["df"].copy()

    open_close_enabled = bool(open_strategy or close_strategies)
    funding_aware_name = open_strategy or strategy_name

    strategy_params = {}
    if strategy_name == "delta_neutral_funding":
        strategy_params.update(_shared_funding_scalar(shared, symbol))
    if funding_aware_name == "funding_skew":
        records = _shared_funding_records(shared, symbol)
        if records:
            strategy_params["funding_records"] = records

    stdout_regime, live_regime, strategy_regime = prepare_check_regime(
        df,
        regime_enabled=shared["regime_enabled"],
        period=shared.get("regime_period", 14),
        adx_threshold=shared.get("regime_adx_threshold", 20.0),
        windows_spec=shared["regime_windows_spec"],
        atr_window=regime_atr_window,
        injected_payload_json=shared["regime_payload_json"],
    )
    strategy_params["regime"] = strategy_regime
    if strategy_params_override:
        merged = {**strategy_params_override, **strategy_params}
        strategy_params = merged
    decision = None
    if open_close_enabled:
        market_ctx = {"mark_price": float(df["close"].iloc[-1])}
        atr_now = shared["atr"]
        if atr_now > 0:
            market_ctx["atr"] = atr_now
        if live_regime:
            market_ctx["regime"] = live_regime
        evaluation = deps.evaluate_open_close(
            deps.apply_strategy,
            deps.get_strategy,
            df,
            strategy_name,
            open_strategy,
            deps.parse_close_strategies(close_strategies),
            position_side,
            strategy_params or None,
            position_ctx,
            close_evaluate=deps.close_evaluate,
            market_ctx=market_ctx,
            close_params_by_name=close_params_by_name,
        )
        result_df = evaluation.open_result_df
        signal = evaluation.open_signal
    else:
        result_df = deps.apply_strategy(strategy_name, df, strategy_params or None)
        signal = deps.normalize_signal(result_df.iloc[-1].get("signal", 0))

    ensure_atr_indicator(result_df, method=atr_method)
    last = result_df.iloc[-1]
    price = float(last["close"])

    htf_info = {}
    htf_strategy_name = open_strategy or strategy_name
    if htf_filter_enabled and htf_strategy_name not in ("delta_neutral_funding", "funding_skew"):
        from htf_filter import htf_trend_filter, apply_htf_filter

        def _fetch_htf(sym, tf, limit):
            return _shared_htf_frame(shared, sym, tf, limit)

        htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
        original_signal = signal
        signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
        if signal != original_signal:
            print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

    if open_close_enabled:
        decision = deps.finalize_decision(evaluation, position_side, signal)
        signal = decision["signal"]

    if shared["price_override"] > 0:
        price = shared["price_override"]

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
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if decision:
        output.update(decision)
    return output


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params_override=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_windows_spec=None, ohlcv_limit=200, regime_atr_window="",
                     regime_payload_json=None,
                     close_params_by_name=None,
                     atr_method="simple",
                     mark_price=0.0):
    try:
        from adapter import HyperliquidExchangeAdapter

        deps = _signal_check_deps()
        _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies)

        adapter = HyperliquidExchangeAdapter()

        shared = build_shared_signal_state(
            symbol, timeframe,
            adapter=adapter,
            ohlcv_limit=ohlcv_limit,
            atr_method=atr_method,
            mark_price=mark_price,
            regime_enabled=regime_enabled,
            regime_windows_spec=regime_windows_spec,
            regime_payload_json=regime_payload_json,
            mode=mode,
        )
        output = evaluate_signal_slot(shared, {
            "id": strategy_name,
            "strategy": strategy_name,
            "mode": mode,
            "htf_filter": htf_filter_enabled,
            "params": strategy_params_override,
            "open_strategy": open_strategy,
            "close_strategies": close_strategies,
            "close_params_by_name": close_params_by_name,
            "position_side": position_side,
            "position_ctx": position_ctx,
            "regime_atr_window": regime_atr_window,
        }, deps=deps)
        print(json.dumps(output, cls=SafeEncoder))

    except InsufficientCandlesError as e:
        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": 0,
            "price": 0,
            "indicators": {},
            "mode": mode,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }, cls=SafeEncoder))
        sys.exit(1)
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
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }, cls=SafeEncoder))
        sys.exit(1)


def parse_batch_slots(raw_stdin):
    payload = json.loads(raw_stdin)
    if not isinstance(payload, dict):
        raise ValueError("batch payload must be a JSON object")
    version = payload.get("v", BATCH_PROTOCOL_VERSION)
    if int(version) != BATCH_PROTOCOL_VERSION:
        raise ValueError(f"unsupported batch protocol version {version}")
    slots = payload.get("slots")
    if not isinstance(slots, list) or not slots:
        raise ValueError("batch payload must carry a non-empty 'slots' array")
    seen = set()
    out = []
    for idx, slot in enumerate(slots):
        if not isinstance(slot, dict):
            raise ValueError(f"slot {idx} must be a JSON object")
        slot_id = str(slot.get("id") or "").strip()
        if not slot_id:
            raise ValueError(f"slot {idx} is missing 'id'")
        if slot_id in seen:
            raise ValueError(f"duplicate slot id {slot_id!r}")
        seen.add(slot_id)
        refs = slot.get("strategy_refs")
        if refs:
            from strategy_composition import parse_strategy_refs_arg
            parsed = parse_strategy_refs_arg(refs if isinstance(refs, str) else json.dumps(refs))
            if parsed:
                slot = dict(slot)
                slot["open_strategy"] = parsed["open_name"]
                slot["close_strategies"] = parsed["close_csv"]
                slot["params"] = parsed["open_params"]
                slot["close_params_by_name"] = parsed["close_params_by_name"]
        if not str(slot.get("strategy") or "").strip():
            raise ValueError(f"slot {slot_id!r} is missing 'strategy'")
        out.append(slot)
    return out


def _batch_slot_error(slot, symbol, timeframe, message):
    return {
        "id": slot.get("id", ""),
        "strategy": slot.get("strategy", ""),
        "symbol": symbol,
        "timeframe": timeframe,
        "signal": 0,
        "price": 0,
        "indicators": {},
        "regime": None,
        "mode": slot.get("mode") or "paper",
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }


def run_batch_signal_check(symbol, timeframe, slots, *, ohlcv_limit=200, atr_method="simple",
                           mark_price=0.0, regime_enabled=False, regime_windows_spec=None,
                           regime_payload_json=None, adapter=None, df=None):
    envelope = {
        "platform": "hyperliquid",
        "symbol": symbol,
        "timeframe": timeframe,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": "",
        "error_scope": "",
        "results": [],
    }
    try:
        deps = _signal_check_deps()
        if adapter is None and df is None:
            from adapter import HyperliquidExchangeAdapter
            adapter = HyperliquidExchangeAdapter()
        shared = build_shared_signal_state(
            symbol, timeframe,
            adapter=adapter,
            df=df,
            ohlcv_limit=ohlcv_limit,
            atr_method=atr_method,
            mark_price=mark_price,
            regime_enabled=regime_enabled,
            regime_windows_spec=regime_windows_spec,
            regime_payload_json=regime_payload_json,
        )
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        envelope["error"] = str(e)
        envelope["error_scope"] = "shared_state"
        return envelope, 1

    failed = False
    for slot in slots:
        try:
            output = evaluate_signal_slot(shared, slot, deps=deps)
            output["id"] = slot.get("id", "")
            envelope["results"].append(output)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            failed = True
            envelope["results"].append(
                _batch_slot_error(slot, symbol, timeframe, str(e)))
    return envelope, (1 if failed else 0)


def _classify_sl_response(sdk_response: dict):
    try:
        statuses = sdk_response.get("response", {}).get("data", {}).get("statuses", [])
        if not statuses:
            return ("missing", None)
        status = statuses[0] if isinstance(statuses[0], dict) else {}
        if "resting" in status and isinstance(status["resting"], dict):
            oid = status["resting"].get("oid")
            return ("resting", int(oid) if oid is not None else 0)
        if "filled" in status and isinstance(status["filled"], dict):
            oid = status["filled"].get("oid")
            return ("filled", int(oid) if oid is not None else 0)
        if "error" in status:
            return ("error", str(status["error"]))
    except Exception as e:
        return ("error", f"_classify_sl_response: {e}")
    return ("missing", None)


def _resolve_sl_placement_by_book_diff(adapter, symbol, pre_oids):
    if pre_oids is None:
        return ("unknown", None)
    try:
        now_oids = adapter.open_order_oids(symbol)
    except Exception as oe:
        print(f"[WARN] outcome-unknown SL placement: open_order_oids({symbol}) re-read failed: {oe}", file=sys.stderr)
        return ("unknown", None)
    if now_oids is None:
        return ("unknown", None)
    fresh = [int(o) for o in now_oids if int(o) not in pre_oids]
    if len(fresh) == 1:
        print(f"[WARN] unreadable SL placement response resolved to resting oid={fresh[0]}", file=sys.stderr)
        return ("resting", fresh[0])
    if not fresh:
        return ("none", None)
    return ("unknown", None)


def _snapshot_open_oids(adapter, symbol):
    try:
        oids = adapter.open_order_oids(symbol)
    except Exception as oe:
        print(f"[WARN] pre-placement open_order_oids({symbol}) failed: {oe}; an unreadable placement will not be resolvable", file=sys.stderr)
        return None
    if oids is None:
        return None
    return set(int(o) for o in oids)


def _classify_cancel_response(sdk_response):
    try:
        if not isinstance(sdk_response, dict):
            return ("error", f"unexpected cancel response: {sdk_response}")
        if sdk_response.get("status") != "ok":
            return ("error", str(sdk_response))
        data = sdk_response.get("response", {}).get("data", {})
        statuses = data.get("statuses") if isinstance(data, dict) else None
        if not isinstance(statuses, list) or not statuses:
            return ("error", f"cancel returned no per-order status: {sdk_response}")
        for st in statuses:
            if isinstance(st, dict) and "error" in st:
                return ("error", str(st["error"]))
        return ("ok", "")
    except Exception as e:
        return ("error", f"_classify_cancel_response: {e}")

def _oid_is_open(open_oids: set[int] | None, oid: int) -> bool:
    return oid > 0 and open_oids is not None and int(oid) in open_oids


def _oid_filled_externally(adapter, oid: int, since_ms: int, fill_hints=None) -> dict:
    if oid <= 0:
        return {"filled": False}
    if fill_hints is not None:
        hint = fill_hints.get(int(oid))
        if hint is not None and hint.get("filled"):
            return {
                "filled": True,
                "fee": float(hint.get("fee", 0) or 0),
                "closed_pnl": float(hint.get("closed_pnl", 0) or 0),
                "count": int(hint.get("count", 0) or 0),
            }
    try:
        lookup = adapter.lookup_fill_fee_by_oid(int(oid), since_ms)
    except Exception as e:
        print(f"[WARN] userFills lookup({oid}) failed: {e}", file=sys.stderr)
        return {"filled": False, "error": str(e)}
    if not lookup:
        return {"filled": False}
    return {
        "filled": True,
        "fee": float(lookup.get("fee", 0) or 0),
        "closed_pnl": float(lookup.get("closed_pnl", 0) or 0),
        "count": int(lookup.get("count", 0) or 0),
    }


def _normalize_tp_tiers(tp_tiers=None, tp1_atr_mult=0.0, tp1_fraction=0.0, tp2_atr_mult=0.0):
    raw_tiers = tp_tiers
    if raw_tiers is None:
        raw_tiers = []
        if tp1_atr_mult > 0 and tp1_fraction > 0:
            raw_tiers.append({"atr_multiple": tp1_atr_mult, "close_fraction": tp1_fraction})
        if tp2_atr_mult > 0:
            raw_tiers.append({"atr_multiple": tp2_atr_mult, "close_fraction": 1.0})

    tiers = []
    for tier in raw_tiers or []:
        if isinstance(tier, dict):
            multiple = tier.get("atr_multiple", tier.get("multiple", tier.get("Multiple")))
            fraction = tier.get("close_fraction", tier.get("fraction", tier.get("Fraction")))
        else:
            try:
                multiple, fraction = tier
            except (TypeError, ValueError):
                continue
        try:
            multiple = float(multiple)
            fraction = min(max(float(fraction), 0.0), 1.0)
        except (TypeError, ValueError):
            continue
        if multiple > 0 and fraction > 0:
            tiers.append((multiple, fraction))
    tiers.sort(key=lambda item: item[0])

    prev_fraction = 0.0
    for _multiple, fraction in tiers:
        if fraction <= prev_fraction:
            return []
        prev_fraction = fraction
    if len(tiers) < 2:
        return []

    tiers[-1] = (tiers[-1][0], 1.0)
    return tiers


def compute_tp_tier_sizes(size, tiers, floor_size_fn):
    if not tiers or size <= 0:
        return [0.0] * len(tiers)
    floored_total = floor_size_fn(size)
    sizes = []
    placed = 0.0
    prev_fraction = 0.0
    for idx, (_atr_mult, cumulative_fraction) in enumerate(tiers):
        is_final = idx == len(tiers) - 1
        if is_final:
            tier_size = max(floored_total - placed, 0.0)
        else:
            raw = size * max(cumulative_fraction - prev_fraction, 0.0)
            tier_size = floor_size_fn(raw)
            placed += tier_size
        prev_fraction = cumulative_fraction
        sizes.append(tier_size)
    return sizes


def run_sync_protection(
    symbol,
    side,
    size,
    avg_cost,
    entry_atr,
    mode,
    stop_loss_atr_mult=0.0,
    tp1_atr_mult=0.0,
    tp1_fraction=0.0,
    tp2_atr_mult=0.0,
    stop_loss_oid=0,
    tp1_oid=0,
    tp2_oid=0,
    tp_tiers=None,
    tp_oids=None,
    tp_armed_tiers=None,
    force_sl_replace=False,
    force_tp_replace=None,
    cancel_tp_oids=None,
    reconcile_fill_hints_json="",
):
    if mode != "live":
        print(json.dumps({"error": "--sync-protection requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)
    side = side.lower()
    if side not in ("long", "short"):
        print(json.dumps({"error": f"invalid side {side!r}"}, cls=SafeEncoder))
        sys.exit(1)
    if avg_cost <= 0 or entry_atr <= 0:
        print(json.dumps({"error": "avg-cost and entry-atr must be > 0"}, cls=SafeEncoder))
        sys.exit(1)
    if size <= 0 and not cancel_tp_oids:
        print(json.dumps({"error": "size must be > 0"}, cls=SafeEncoder))
        sys.exit(1)

    out = {
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        fill_hints = None
        if reconcile_fill_hints_json:
            try:
                parsed = json.loads(reconcile_fill_hints_json)
                if isinstance(parsed, list):
                    fill_hints = {}
                    for item in parsed:
                        if isinstance(item, dict) and "oid" in item:
                            fill_hints[int(item["oid"])] = item
            except json.JSONDecodeError as je:
                print(f"[WARN] reconcile_fill_hints_json: {je}", file=sys.stderr)

        open_oids = None
        try:
            open_oids = adapter.open_order_oids(symbol)
        except Exception as oe:
            out["open_order_check_error"] = str(oe)
            print(f"[WARN] open_order_oids({symbol}) failed: {oe}; will place only missing zero-OID protection", file=sys.stderr)

        close_is_buy = side == "short"

        fill_check_since_ms = int(time.time() * 1000) - 7 * 24 * 3600 * 1000

        def _resolve_missing_oid(prev_oid: int):
            if prev_oid <= 0:
                return ("place", None)
            if open_oids is None:
                return ("unknown", None)
            fill = _oid_filled_externally(adapter, prev_oid, fill_check_since_ms, fill_hints)
            if fill.get("filled"):
                return ("filled", fill)
            return ("place", None)

        surplus_cancel_failed = []
        surplus_cancel_filled = []
        for surplus_oid in cancel_tp_oids or []:
            oid = int(surplus_oid)
            if oid <= 0:
                continue
            action, fill = _resolve_missing_oid(oid)
            if action == "filled":
                surplus_cancel_filled.append(oid)
                print(
                    f"[WARN] surplus TP OID={oid} already filled on-chain; not canceling — reconciler will book the close",
                    file=sys.stderr,
                )
                continue
            if action == "unknown":
                surplus_cancel_failed.append(oid)
                continue
            try:
                adapter.cancel_order_by_oid(symbol, oid)
            except Exception as ce:
                surplus_cancel_failed.append(oid)
                print(
                    f"[WARN] cancel surplus TP OID={oid} failed: {ce}",
                    file=sys.stderr,
                )
        if surplus_cancel_failed:
            out["tp_cancel_failed_oids"] = surplus_cancel_failed
        if surplus_cancel_filled:
            out["tp_cancel_filled_oids"] = surplus_cancel_filled

        if stop_loss_atr_mult > 0:
            if side == "long":
                sl_px = avg_cost - stop_loss_atr_mult * entry_atr
            else:
                sl_px = avg_cost + stop_loss_atr_mult * entry_atr
            sl_px = adapter.round_perps_trigger_px(symbol, sl_px)

            def _sl_placed(px):
                out["stop_loss_trigger_px"] = px

            def _resolve_unknown_sl(reason, pre_oids):
                out["stop_loss_error"] = reason
                kind, oid = _resolve_sl_placement_by_book_diff(adapter, symbol, pre_oids)
                if kind == "resting":
                    del out["stop_loss_error"]
                    out["stop_loss_oid"] = oid
                    _sl_placed(sl_px)
                elif kind == "unknown":
                    del out["stop_loss_error"]
                    out["stop_loss_outcome_unknown"] = True

            def _place_sl():
                pre_oids = set(int(o) for o in open_oids) if open_oids is not None else None
                try:
                    resp = adapter.place_stop_loss(symbol, size, sl_px, close_is_buy)
                    kind, payload = _classify_sl_response(resp)
                    if kind == "resting":
                        out["stop_loss_oid"] = payload
                        _sl_placed(sl_px)
                    elif kind == "filled":
                        out["stop_loss_filled_immediately"] = True
                        _sl_placed(sl_px)
                    elif kind == "error":
                        out["stop_loss_error"] = f"place_stop_loss SDK error: {payload}"
                    else:
                        _resolve_unknown_sl(f"place_stop_loss returned no usable status: {resp}", pre_oids)
                except Exception as se:
                    _resolve_unknown_sl(str(se), pre_oids)

            if _oid_is_open(open_oids, stop_loss_oid) and not force_sl_replace:
                out["stop_loss_oid"] = int(stop_loss_oid)
            elif _oid_is_open(open_oids, stop_loss_oid) and force_sl_replace:
                if size <= 0:
                    out["stop_loss_oid"] = int(stop_loss_oid)
                else:
                    cancel_ok = False
                    try:
                        kind, payload = _classify_cancel_response(
                            adapter.cancel_order_by_oid(symbol, int(stop_loss_oid)))
                        if kind == "ok":
                            cancel_ok = True
                        else:
                            out["stop_loss_error"] = f"force replace cancel rejected: {payload}"
                    except Exception as ce:
                        out["stop_loss_error"] = f"force replace cancel: {ce}"
                    out["cancel_stop_loss_succeeded"] = cancel_ok
                    if cancel_ok:
                        _place_sl()
            else:
                action, fill = _resolve_missing_oid(stop_loss_oid)
                if action == "filled":
                    out["stop_loss_filled_externally"] = True
                    out["stop_loss_fill"] = fill
                    print(f"[WARN] stop-loss OID={stop_loss_oid} already filled on-chain; not re-placing — reconciler will book the close", file=sys.stderr)
                elif action == "place" and size > 0:
                    _place_sl()

        tiers = _normalize_tp_tiers(tp_tiers, tp1_atr_mult, tp1_fraction, tp2_atr_mult)
        if out.get("stop_loss_filled_immediately"):
            print(
                f"[WARN] TP protection skipped for {symbol}: SL filled at submit — "
                f"the position is flat on-chain and no TP orders are placed",
                file=sys.stderr,
            )
        elif tiers:
            existing_tp_oids = list(tp_oids or [])
            if not existing_tp_oids and (tp1_oid > 0 or tp2_oid > 0):
                existing_tp_oids = [tp1_oid, tp2_oid]
            if len(existing_tp_oids) < len(tiers):
                existing_tp_oids.extend([0] * (len(tiers) - len(existing_tp_oids)))

            size = adapter.round_size(symbol, size)
            if size <= 0:
                print(
                    f"[INFO] TP protection skipped for {symbol}: virtual qty "
                    f"rounds to zero at lot precision — peer TPs cover the on-chain position",
                    file=sys.stderr,
                )
            else:
                tp_oids_out = list(existing_tp_oids[:len(tiers)])
                tp_pxs = []
                tp_errors = [""] * len(tiers)
                tp_filled_externally = [False] * len(tiers)
                tp_fills = [None] * len(tiers)
                tp_filled_immediately = [False] * len(tiers)
                armed = [bool(x) for x in (tp_armed_tiers or [])]
                if len(armed) < len(tiers):
                    armed.extend([False] * (len(tiers) - len(armed)))
                else:
                    armed = armed[: len(tiers)]
                force_tp = [bool(x) for x in (force_tp_replace or [])]
                if len(force_tp) < len(tiers):
                    force_tp.extend([False] * (len(tiers) - len(force_tp)))
                else:
                    force_tp = force_tp[: len(tiers)]
                tier_sizes = compute_tp_tier_sizes(
                    size, tiers, lambda sz: adapter.floor_size(symbol, sz)
                )

                for idx, ((atr_mult, _cumulative_fraction), tier_size) in enumerate(
                    zip(tiers, tier_sizes)
                ):
                    raw_px = avg_cost + atr_mult * entry_atr if side == "long" else avg_cost - atr_mult * entry_atr
                    rounded_px = adapter.round_perps_trigger_px(symbol, raw_px)
                    tp_pxs.append(rounded_px)
                    prev_oid = int(existing_tp_oids[idx]) if idx < len(existing_tp_oids) else 0
                    tier_armed = armed[idx] if idx < len(armed) else False

                    if tier_size <= 0:
                        continue
                    if _oid_is_open(open_oids, prev_oid) and not (idx < len(force_tp) and force_tp[idx]):
                        tp_oids_out[idx] = prev_oid
                        continue
                    if _oid_is_open(open_oids, prev_oid) and idx < len(force_tp) and force_tp[idx]:
                        try:
                            adapter.cancel_order_by_oid(symbol, int(prev_oid))
                        except Exception as ce:
                            tp_errors[idx] = f"force replace cancel: {ce}"
                            continue
                        try:
                            resp = adapter.place_take_profit_limit(
                                symbol, tier_size, rounded_px, close_is_buy
                            )
                            kind, payload = _classify_sl_response(resp)
                            if kind == "resting":
                                tp_oids_out[idx] = payload
                            elif kind == "filled":
                                tp_filled_immediately[idx] = True
                            elif kind == "error":
                                tp_errors[idx] = (
                                    f"place_take_profit_limit SDK error: {payload}"
                                )
                            else:
                                tp_errors[idx] = (
                                    f"place_take_profit_limit returned no usable status: {resp}"
                                )
                        except Exception as te:
                            tp_errors[idx] = str(te)
                        continue

                    if prev_oid <= 0 and tier_armed:
                        tp_oids_out[idx] = 0
                        continue

                    action, fill = _resolve_missing_oid(prev_oid)
                    if action == "filled":
                        tp_oids_out[idx] = 0
                        tp_filled_externally[idx] = True
                        tp_fills[idx] = fill
                        print(f"[WARN] TP{idx + 1} OID={prev_oid} already filled on-chain; not re-placing — reconciler will book the close", file=sys.stderr)
                    elif action == "place":
                        try:
                            resp = adapter.place_take_profit_limit(symbol, tier_size, rounded_px, close_is_buy)
                            kind, payload = _classify_sl_response(resp)
                            if kind == "resting":
                                tp_oids_out[idx] = payload
                            elif kind == "filled":
                                tp_filled_immediately[idx] = True
                            elif kind == "error":
                                tp_errors[idx] = f"place_take_profit_limit SDK error: {payload}"
                            else:
                                tp_errors[idx] = f"place_take_profit_limit returned no usable status: {resp}"
                        except Exception as te:
                            tp_errors[idx] = str(te)

                out["tp_oids"] = tp_oids_out
                out["tp_pxs"] = tp_pxs
                if any(tp_errors):
                    out["tp_errors"] = tp_errors
                if any(tp_filled_externally):
                    out["tp_filled_externally"] = tp_filled_externally
                    out["tp_fills"] = tp_fills
                if any(tp_filled_immediately):
                    out["tp_filled_immediately"] = tp_filled_immediately

                if len(tp_oids_out) > 0 and tp_oids_out[0] > 0:
                    out["tp1_oid"] = tp_oids_out[0]
                if len(tp_oids_out) > 1 and tp_oids_out[1] > 0:
                    out["tp2_oid"] = tp_oids_out[1]
                if len(tp_pxs) > 0:
                    out["tp1_px"] = tp_pxs[0]
                if len(tp_pxs) > 1:
                    out["tp2_px"] = tp_pxs[1]
                if len(tp_errors) > 0 and tp_errors[0]:
                    out["tp1_error"] = tp_errors[0]
                if len(tp_errors) > 1 and tp_errors[1]:
                    out["tp2_error"] = tp_errors[1]
                if len(tp_filled_externally) > 0 and tp_filled_externally[0]:
                    out["tp1_filled_externally"] = True
                    out["tp1_fill"] = tp_fills[0]
                if len(tp_filled_externally) > 1 and tp_filled_externally[1]:
                    out["tp2_filled_externally"] = True
                    out["tp2_fill"] = tp_fills[1]

        print(json.dumps(out, cls=SafeEncoder))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        out["error"] = str(e)
        print(json.dumps(out, cls=SafeEncoder))
        sys.exit(1)


def run_execute(symbol, side, size, mode, stop_loss_pct=0.0, cancel_oid=0, prev_pos_qty=0.0, margin_mode="", leverage=0, close_full_position=False, account_leverage=0, account_margin_mode=""):
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)

    cancel_err = ""
    cancel_oids = cancel_oid if isinstance(cancel_oid, list) else [cancel_oid]
    cancel_oids = [int(oid) for oid in cancel_oids if int(oid or 0) > 0]
    cancel_attempted = len(cancel_oids) > 0
    cancel_succeeded = False

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        is_buy = side.lower() == "buy"

        if margin_mode:
            if margin_mode not in ("isolated", "cross"):
                print(json.dumps({
                    "execution": None,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"invalid margin_mode {margin_mode!r}, expected 'isolated' or 'cross'",
                }, cls=SafeEncoder))
                sys.exit(1)
            if leverage < 1:
                print(json.dumps({
                    "execution": None,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"--margin-mode requires --leverage >= 1, got {leverage}",
                }, cls=SafeEncoder))
                sys.exit(1)
            current = None
            if account_leverage and account_margin_mode in ("isolated", "cross"):
                current = {"margin_mode": account_margin_mode, "leverage": int(account_leverage)}
            else:
                try:
                    current = adapter.get_position_leverage(symbol)
                except Exception as ce:
                    print(f"[WARN] get_position_leverage({symbol}) failed: {ce}; will call update_leverage", file=sys.stderr)
            if current is not None and current.get("margin_mode") == margin_mode and current.get("leverage") == int(leverage):
                print(f"update_leverage({symbol}, {leverage}x, mode={margin_mode}) SKIPPED (HL state already matches)", file=sys.stderr)
            else:
                try:
                    adapter.update_leverage(int(leverage), symbol, is_cross=(margin_mode == "cross"))
                    print(f"update_leverage({symbol}, {leverage}x, mode={margin_mode}) OK", file=sys.stderr)
                except Exception as ue:
                    traceback.print_exc(file=sys.stderr)
                    print(json.dumps({
                        "execution": None,
                        "platform": "hyperliquid",
                        "timestamp": datetime.now(timezone.utc).isoformat(),
                        "error": f"update_leverage failed (margin_mode={margin_mode}, leverage={leverage}): {ue}",
                    }, cls=SafeEncoder))
                    sys.exit(1)

        if cancel_attempted:
            cancel_errors = []
            try:
                for oid in cancel_oids:
                    try:
                        kind, payload = _classify_cancel_response(
                            adapter.cancel_trigger_order(symbol, oid))
                        if kind == "ok":
                            cancel_succeeded = True
                        else:
                            cancel_errors.append(f"{oid}: {payload}")
                            print(f"[WARN] cancel_trigger_order({symbol}, {oid}) rejected: {payload}", file=sys.stderr)
                    except Exception as ce:
                        cancel_errors.append(f"{oid}: {ce}")
                        print(f"[WARN] cancel_trigger_order({symbol}, {oid}) failed: {ce}", file=sys.stderr)
            finally:
                if cancel_errors:
                    cancel_err = "; ".join(cancel_errors)

        fills_since_ms = int(time.time() * 1000) - 10_000

        if close_full_position:
            result = adapter.market_close(symbol, sz=None)
        else:
            result = adapter.market_open(symbol, is_buy, size)

        fill = {}
        try:
            statuses = result.get("response", {}).get("data", {}).get("statuses", [])
            if statuses:
                filled = statuses[0].get("filled", {})
                fill = {
                    "avg_px": float(filled.get("avgPx", 0) or 0),
                    "total_sz": float(filled.get("totalSz", 0) or 0),
                }
                oid = filled.get("oid")
                if oid is not None:
                    fill["oid"] = int(oid)
                fee = filled.get("fee")
                if fee is not None:
                    fill["fee"] = float(fee)
        except Exception:
            pass

        if fill.get("oid"):
            try:
                lookup = adapter.lookup_fill_fee_by_oid(fill["oid"], fills_since_ms)
                if not lookup:
                    print(f"[WARN] userFills lookup returned no fills for oid={fill['oid']}", file=sys.stderr)
                elif not apply_user_fills_lookup(fill, lookup):
                    print(f"[WARN] userFills lookup returned malformed fill data for oid={fill['oid']}", file=sys.stderr)
            except Exception as fe:
                print(f"[WARN] userFills lookup failed for oid={fill['oid']}: {fe}", file=sys.stderr)

        sl_err = ""
        sl_filled_immediately = False
        net_new_sz = max(fill.get("total_sz", 0) - max(prev_pos_qty, 0.0), 0.0)
        if stop_loss_pct > 0 and fill.get("avg_px", 0) > 0 and net_new_sz > 0:
            entry_px = fill["avg_px"]
            sl_size = net_new_sz
            if is_buy:
                trigger_px = entry_px * (1.0 - stop_loss_pct / 100.0)
                sl_is_buy = False
            else:
                trigger_px = entry_px * (1.0 + stop_loss_pct / 100.0)
                sl_is_buy = True
            trigger_px = adapter.round_perps_trigger_px(symbol, trigger_px)
            try:
                sl_resp = adapter.place_stop_loss(symbol, sl_size, trigger_px, sl_is_buy)
                kind, payload = _classify_sl_response(sl_resp)
                if kind == "resting":
                    fill["stop_loss_oid"] = payload
                    fill["stop_loss_trigger_px"] = trigger_px
                elif kind == "filled":
                    sl_filled_immediately = True
                    fill["stop_loss_trigger_px"] = trigger_px
                    print(f"[WARN] stop-loss filled immediately at submit (price already through {trigger_px})", file=sys.stderr)
                elif kind == "error":
                    sl_err = f"place_stop_loss SDK error: {payload}"
                    print(f"[WARN] {sl_err}", file=sys.stderr)
                else:
                    sl_err = f"place_stop_loss returned no usable status: {sl_resp}"
                    print(f"[WARN] {sl_err}", file=sys.stderr)
            except Exception as se:
                sl_err = str(se)
                print(f"[WARN] place_stop_loss({symbol}, {sl_size}, {trigger_px}) failed: {se}", file=sys.stderr)

        out = {
            "execution": {
                "action": "buy" if is_buy else "sell",
                "symbol": symbol,
                "size": size,
                "fill": fill,
            },
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if cancel_err:
            out["cancel_stop_loss_error"] = cancel_err
        if cancel_succeeded:
            out["cancel_stop_loss_succeeded"] = True
        if sl_err:
            out["stop_loss_error"] = sl_err
        if sl_filled_immediately:
            out["stop_loss_filled_immediately"] = True
        print(json.dumps(out, cls=SafeEncoder))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        err_payload = {
            "execution": None,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }
        if cancel_err:
            err_payload["cancel_stop_loss_error"] = cancel_err
        if cancel_succeeded:
            err_payload["cancel_stop_loss_succeeded"] = True
        print(json.dumps(err_payload, cls=SafeEncoder))
        sys.exit(1)


def run_update_stop_loss(symbol, side, size, trigger_px, mode, cancel_oid=0):
    if mode != "live":
        print(json.dumps({"error": "--update-stop-loss requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)

    cancel_err = ""
    cancel_attempted = cancel_oid > 0
    cancel_succeeded = False
    sl_err = ""
    sl_filled_immediately = False
    sl_filled_externally = False
    resting_oid = 0
    open_order_check_error = ""

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        side = side.lower()
        if side not in ("long", "short"):
            print(json.dumps({
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"invalid side {side!r}, expected 'long' or 'short'",
            }, cls=SafeEncoder))
            sys.exit(1)

        open_oids = None
        if cancel_attempted:
            try:
                open_oids = adapter.open_order_oids(symbol)
            except Exception as oe:
                open_order_check_error = str(oe)
                print(f"[WARN] open_order_oids({symbol}) failed: {oe}; deferring trailing SL replacement", file=sys.stderr)

        fill_check_since_ms = int(time.time() * 1000) - 7 * 24 * 3600 * 1000
        should_place = True
        if cancel_attempted:
            if open_oids is None:
                should_place = False
            elif _oid_is_open(open_oids, cancel_oid):
                try:
                    kind, payload = _classify_cancel_response(
                        adapter.cancel_trigger_order(symbol, cancel_oid))
                    if kind == "ok":
                        cancel_succeeded = True
                    else:
                        cancel_err = payload
                        should_place = False
                        print(f"[WARN] cancel_trigger_order({symbol}, {cancel_oid}) rejected: {payload}; not placing replacement", file=sys.stderr)
                except Exception as ce:
                    cancel_err = str(ce)
                    should_place = False
                    print(f"[WARN] cancel_trigger_order({symbol}, {cancel_oid}) failed: {ce}; not placing replacement", file=sys.stderr)
            else:
                fill = _oid_filled_externally(adapter, cancel_oid, fill_check_since_ms, None)
                if fill.get("filled"):
                    sl_filled_externally = True
                    should_place = False
                    print(f"[WARN] stop-loss OID={cancel_oid} already filled on-chain; not re-placing — reconciler will book the close", file=sys.stderr)

        sl_is_buy = side == "short"
        place_unknown = False
        trigger_px = adapter.round_perps_trigger_px(symbol, trigger_px)
        if should_place:
            pre_oids = set(int(o) for o in open_oids) if open_oids is not None else _snapshot_open_oids(adapter, symbol)
            try:
                sl_resp = adapter.place_stop_loss(symbol, size, trigger_px, sl_is_buy)
                kind, payload = _classify_sl_response(sl_resp)
                if kind == "resting":
                    resting_oid = payload
                elif kind == "filled":
                    sl_filled_immediately = True
                    print(f"[WARN] stop-loss filled immediately at submit (price already through {trigger_px})", file=sys.stderr)
                elif kind == "error":
                    sl_err = f"place_stop_loss SDK error: {payload}"
                    print(f"[WARN] {sl_err}", file=sys.stderr)
                else:
                    sl_err = f"place_stop_loss returned no usable status: {sl_resp}"
                    print(f"[WARN] {sl_err}", file=sys.stderr)
                    resolved, oid = _resolve_sl_placement_by_book_diff(adapter, symbol, pre_oids)
                    if resolved == "resting":
                        resting_oid = oid
                    elif resolved == "unknown":
                        place_unknown = True
            except Exception as se:
                sl_err = str(se)
                print(f"[WARN] place_stop_loss({symbol}, {size}, {trigger_px}) failed: {se}", file=sys.stderr)
                resolved, oid = _resolve_sl_placement_by_book_diff(adapter, symbol, pre_oids)
                if resolved == "resting":
                    resting_oid = oid
                elif resolved == "unknown":
                    place_unknown = True

        out = {
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "stop_loss_trigger_px": trigger_px,
        }
        if resting_oid:
            out["stop_loss_oid"] = resting_oid
        if cancel_err:
            out["cancel_stop_loss_error"] = cancel_err
        if cancel_succeeded:
            out["cancel_stop_loss_succeeded"] = True
        if open_order_check_error:
            out["open_order_check_error"] = open_order_check_error
        if sl_err:
            out["stop_loss_error"] = sl_err
        if sl_filled_immediately:
            out["stop_loss_filled_immediately"] = True
        if sl_filled_externally:
            out["stop_loss_filled_externally"] = True
        if place_unknown:
            out["stop_loss_outcome_unknown"] = True
        print(json.dumps(out, cls=SafeEncoder))

    except SystemExit:
        raise
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        err_payload = {
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }
        if cancel_err:
            err_payload["cancel_stop_loss_error"] = cancel_err
        if cancel_succeeded:
            err_payload["cancel_stop_loss_succeeded"] = True
        print(json.dumps(err_payload, cls=SafeEncoder))
        sys.exit(1)


def run_fetch_atr(symbol: str, timeframe: str, period: int, atr_method: str = "simple"):
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=200)
        if not candles or len(candles) < period + 1:
            print(json.dumps({
                "error": f"insufficient candles: got {len(candles) if candles else 0}, need {period + 1}",
                "candles": len(candles) if candles else 0,
            }, cls=SafeEncoder))
            return
        df = _make_dataframe(candles)
        atr = latest_atr(df, period=period, method=atr_method)
        if not (atr > 0):
            print(json.dumps({
                "error": "latest ATR is not positive",
                "candles": len(candles),
            }, cls=SafeEncoder))
            return
        print(json.dumps({"atr": atr, "candles": len(candles)}, cls=SafeEncoder))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({"error": f"{type(e).__name__}: {e}"}, cls=SafeEncoder))


def run_limit_open(symbol, side, size, limit_px, mode, tif="Alo",
                   margin_mode="", leverage=0, account_leverage=0,
                   account_margin_mode=""):
    if mode != "live":
        print(json.dumps({"error": "--limit-open requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        side = side.lower()
        if side not in ("buy", "sell"):
            print(json.dumps({
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"invalid side {side!r}, expected 'buy' or 'sell'",
            }, cls=SafeEncoder))
            sys.exit(1)
        is_buy = side == "buy"

        if margin_mode:
            if margin_mode not in ("isolated", "cross"):
                print(json.dumps({
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"invalid margin_mode {margin_mode!r}, expected 'isolated' or 'cross'",
                }, cls=SafeEncoder))
                sys.exit(1)
            if leverage < 1:
                print(json.dumps({
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"--margin-mode requires --leverage >= 1, got {leverage}",
                }, cls=SafeEncoder))
                sys.exit(1)
            current = None
            if account_leverage and account_margin_mode in ("isolated", "cross"):
                current = {"margin_mode": account_margin_mode, "leverage": int(account_leverage)}
            else:
                try:
                    current = adapter.get_position_leverage(symbol)
                except Exception as ce:
                    print(f"[WARN] get_position_leverage({symbol}) failed: {ce}; will call update_leverage", file=sys.stderr)
            if current is not None and current.get("margin_mode") == margin_mode and current.get("leverage") == int(leverage):
                print(f"update_leverage({symbol}, {leverage}x, mode={margin_mode}) SKIPPED (HL state already matches)", file=sys.stderr)
            else:
                try:
                    adapter.update_leverage(int(leverage), symbol, is_cross=(margin_mode == "cross"))
                    print(f"update_leverage({symbol}, {leverage}x, mode={margin_mode}) OK", file=sys.stderr)
                except Exception as ue:
                    traceback.print_exc(file=sys.stderr)
                    print(json.dumps({
                        "platform": "hyperliquid",
                        "timestamp": datetime.now(timezone.utc).isoformat(),
                        "error": f"update_leverage failed (margin_mode={margin_mode}, leverage={leverage}): {ue}",
                    }, cls=SafeEncoder))
                    sys.exit(1)

        try:
            resp = adapter.limit_open(symbol, is_buy, size, limit_px, tif=tif)
        except Exception as oe:
            traceback.print_exc(file=sys.stderr)
            print(json.dumps({
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"limit_open failed: {oe}",
            }, cls=SafeEncoder))
            sys.exit(1)

        kind, payload = _classify_sl_response(resp)
        out = {
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "limit_price": limit_px,
            "tif": tif,
        }
        if kind == "resting":
            out["order_oid"] = int(payload)
            out["status"] = "resting"
        elif kind == "filled":
            out["order_oid"] = int(payload)
            out["status"] = "filled"
            print(f"[WARN] limit order filled immediately at submit (price already marketable)", file=sys.stderr)
        elif kind == "error":
            out["status"] = "error"
            out["error"] = f"limit order rejected: {payload}"
        else:
            out["status"] = "error"
            out["error"] = f"limit order returned no usable status: {resp}"
        print(json.dumps(out, cls=SafeEncoder))
        if out["status"] == "error":
            sys.exit(1)

    except SystemExit:
        raise
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }, cls=SafeEncoder))
        sys.exit(1)


def run_limit_status(symbol, oids, mode, since_ms=0):
    if mode != "live":
        print(json.dumps({"error": "--limit-status requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        if since_ms <= 0:
            since_ms = int(time.time() * 1000) - 7 * 24 * 60 * 60 * 1000

        open_oids = None
        open_orders_error = ""
        try:
            open_oids = adapter.open_order_oids(symbol)
        except Exception as oe:
            open_orders_error = str(oe)
            print(f"[WARN] open_order_oids({symbol}) failed: {oe}", file=sys.stderr)

        results = []
        for oid in oids:
            oid = int(oid)
            entry = {"oid": oid}
            if open_oids is not None:
                entry["resting"] = oid in open_oids
            else:
                entry["resting"] = None
            summary = {}
            try:
                summary = adapter.fills_summary_by_oid(oid, since_ms)
            except Exception as fe:
                print(f"[WARN] fills_summary_by_oid({oid}) failed: {fe}", file=sys.stderr)
                entry["fills_error"] = str(fe)
            entry["filled_size"] = float(summary.get("filled_size", 0) or 0)
            entry["avg_px"] = float(summary.get("avg_px", 0) or 0)
            entry["fee"] = float(summary.get("fee", 0) or 0)
            entry["count"] = int(summary.get("count", 0) or 0)
            results.append(entry)

        out = {
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "orders": results,
        }
        if open_orders_error:
            out["open_orders_error"] = open_orders_error
        print(json.dumps(out, cls=SafeEncoder))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }, cls=SafeEncoder))
        sys.exit(1)


def run_cancel_order(symbol, oid, mode):
    if mode != "live":
        print(json.dumps({"error": "--cancel-order requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        out = {
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "oid": int(oid),
        }
        try:
            adapter.cancel_order_by_oid(symbol, int(oid))
            out["cancelled"] = True
        except Exception as ce:
            out["cancelled"] = False
            out["cancel_error"] = str(ce)
            print(f"[WARN] cancel_order_by_oid({symbol}, {oid}) failed: {ce}", file=sys.stderr)
        print(json.dumps(out, cls=SafeEncoder))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }, cls=SafeEncoder))
        sys.exit(1)


def main():
    if "--batch-check" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--batch-check", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--timeframe", required=True)
        parser.add_argument("--ohlcv-limit", type=int, default=200)
        parser.add_argument("--atr-method", default="simple", choices=["simple", "wilder"])
        parser.add_argument("--mark-price", type=float, default=0.0)
        parser.add_argument("--regime-enabled", action="store_true", default=False)
        parser.add_argument("--regime-windows-spec-json", default="")
        parser.add_argument("--regime-payload-json", default=None)
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#1442): validate argv shape and exit 0 before reading stdin.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        symbol = args.symbol
        timeframe = args.timeframe
        try:
            slots = parse_batch_slots(sys.stdin.read())
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            print(json.dumps({
                "platform": "hyperliquid",
                "symbol": symbol,
                "timeframe": timeframe,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"invalid batch payload: {e}",
                "error_scope": "shared_state",
                "results": [],
            }, cls=SafeEncoder))
            sys.exit(1)
        regime_windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        envelope, exit_code = run_batch_signal_check(
            symbol, timeframe, slots,
            ohlcv_limit=args.ohlcv_limit,
            atr_method=args.atr_method,
            mark_price=args.mark_price,
            regime_enabled=args.regime_enabled,
            regime_windows_spec=regime_windows_spec,
            regime_payload_json=args.regime_payload_json,
        )
        print(json.dumps(envelope, cls=SafeEncoder))
        if exit_code:
            sys.exit(exit_code)
        return
    if "--fetch-atr" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--fetch-atr", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--timeframe", required=True)
        parser.add_argument("--period", type=int, default=14)
        parser.add_argument("--atr-method", default="simple", choices=["simple", "wilder"])
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe: validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_fetch_atr(args.symbol, args.timeframe, args.period, args.atr_method)
        return
    if "--sync-protection" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--sync-protection", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["long", "short"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--avg-cost", type=float, required=True)
        parser.add_argument("--entry-atr", type=float, required=True)
        parser.add_argument("--stop-loss-atr-mult", type=float, default=0.0)
        parser.add_argument("--tp1-atr-mult", type=float, default=0.0)
        parser.add_argument("--tp1-fraction", type=float, default=0.0)
        parser.add_argument("--tp2-atr-mult", type=float, default=0.0)
        parser.add_argument("--tp-tiers-json", default="")
        parser.add_argument("--stop-loss-oid", type=int, default=0)
        parser.add_argument("--tp1-oid", type=int, default=0)
        parser.add_argument("--tp2-oid", type=int, default=0)
        parser.add_argument("--tp-oids-json", default="")
        parser.add_argument("--tp-armed-tiers-json", default="")
        parser.add_argument(
            "--reconcile-fill-hints-json",
            default="",
            help="Optional JSON array from Go reconciler prefetch (#759); skips duplicate userFills per OID.",
        )
        parser.add_argument(
            "--force-sl-replace",
            action="store_true",
            help="#843: cancel resting SL and re-place when dynamic regime changes.",
        )
        parser.add_argument(
            "--force-tp-replace-json",
            default="",
            help="#843: JSON bool[] — cancel+replace resting TP tiers when true.",
        )
        parser.add_argument(
            "--cancel-tp-oids-json",
            default="",
            help="#843: JSON int[] — surplus resting TP OIDs to cancel after tier-count shrink.",
        )
        parser.add_argument("--mode", default="live")
        args = parser.parse_args()
        tp_tiers = json.loads(args.tp_tiers_json) if args.tp_tiers_json else None
        tp_oids = json.loads(args.tp_oids_json) if args.tp_oids_json else None
        tp_armed_tiers = (
            json.loads(args.tp_armed_tiers_json) if args.tp_armed_tiers_json else None
        )
        force_tp_replace = (
            json.loads(args.force_tp_replace_json) if args.force_tp_replace_json else None
        )
        cancel_tp_oids = (
            json.loads(args.cancel_tp_oids_json) if args.cancel_tp_oids_json else None
        )
        run_sync_protection(
            args.symbol,
            args.side,
            args.size,
            args.avg_cost,
            args.entry_atr,
            args.mode,
            stop_loss_atr_mult=args.stop_loss_atr_mult,
            tp1_atr_mult=args.tp1_atr_mult,
            tp1_fraction=args.tp1_fraction,
            tp2_atr_mult=args.tp2_atr_mult,
            stop_loss_oid=args.stop_loss_oid,
            tp1_oid=args.tp1_oid,
            tp2_oid=args.tp2_oid,
            tp_tiers=tp_tiers,
            tp_oids=tp_oids,
            tp_armed_tiers=tp_armed_tiers,
            force_sl_replace=bool(args.force_sl_replace),
            force_tp_replace=force_tp_replace,
            cancel_tp_oids=cancel_tp_oids,
            reconcile_fill_hints_json=args.reconcile_fill_hints_json or "",
        )
    elif "--update-stop-loss" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--update-stop-loss", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["long", "short"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--trigger-px", type=float, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--cancel-stop-loss-oid", type=int, default=0,
                            help="cancel this trigger OID before placing the replacement (#501)")
        args = parser.parse_args()
        run_update_stop_loss(args.symbol, args.side, args.size, args.trigger_px, args.mode,
                             cancel_oid=args.cancel_stop_loss_oid)
    elif "--execute" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, default=0.0)
        parser.add_argument("--close-full-position", action="store_true", default=False,
                            help="close entire on-chain residual via market_close(sz=None); mutually exclusive with --size (#592)")
        parser.add_argument("--mode", default="live")
        parser.add_argument("--stop-loss-pct", type=float, default=0.0,
                            help="place a reduce-only SL trigger this pct away from fill (#412)")
        parser.add_argument("--cancel-stop-loss-oid", type=int, action="append", default=[],
                            help="cancel this trigger OID before placing the new order (#412)")
        parser.add_argument("--prev-pos-qty", type=float, default=0.0,
                            help="abs qty of existing position being flipped, so SL is sized against the new net position (#421)")
        parser.add_argument("--margin-mode", default="",
                            help="enforce 'isolated' or 'cross' margin via update_leverage before the order; only safe on a fresh open from flat (#486)")
        parser.add_argument("--leverage", type=float, default=0.0,
                            help="leverage to set alongside --margin-mode (HL update_leverage takes both in one call) (#486)")
        parser.add_argument("--account-leverage", type=int, default=0,
                            help="on-chain leverage observed in Go's clearinghouseState snapshot; when paired with --account-margin-mode lets Python skip the duplicate get_position_leverage /info call (#768)")
        parser.add_argument("--account-margin-mode", default="",
                            help="on-chain margin mode observed in Go's clearinghouseState snapshot; see --account-leverage (#768)")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (PR #769): validate execute-mode argv shape — including --account-leverage / --account-margin-mode — and exit 0 without trading.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        if not args.close_full_position and args.size <= 0:
            print(json.dumps({"error": "--size must be > 0 unless --close-full-position is set"}))
            sys.exit(1)
        run_execute(args.symbol, args.side, args.size, args.mode,
                    stop_loss_pct=args.stop_loss_pct, cancel_oid=args.cancel_stop_loss_oid,
                    prev_pos_qty=args.prev_pos_qty,
                    margin_mode=args.margin_mode, leverage=args.leverage,
                    close_full_position=args.close_full_position,
                    account_leverage=args.account_leverage,
                    account_margin_mode=args.account_margin_mode)
    elif "--limit-open" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--limit-open", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--limit-price", type=float, required=True)
        parser.add_argument("--tif", default="Alo", choices=["Alo", "Gtc", "Ioc"],
                            help="time-in-force: Alo=post-only maker (default), Gtc=allow immediate marketable fill")
        parser.add_argument("--mode", default="live")
        parser.add_argument("--margin-mode", default="",
                            help="enforce 'isolated'/'cross' via update_leverage before resting the order (#486 parity)")
        parser.add_argument("--leverage", type=float, default=0.0)
        parser.add_argument("--account-leverage", type=int, default=0)
        parser.add_argument("--account-margin-mode", default="")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#883): validate argv shape and exit 0 without trading.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        if args.size <= 0:
            print(json.dumps({"error": "--size must be > 0"}))
            sys.exit(1)
        if args.limit_price <= 0:
            print(json.dumps({"error": "--limit-price must be > 0"}))
            sys.exit(1)
        run_limit_open(args.symbol, args.side, args.size, args.limit_price, args.mode,
                       tif=args.tif, margin_mode=args.margin_mode, leverage=args.leverage,
                       account_leverage=args.account_leverage,
                       account_margin_mode=args.account_margin_mode)
    elif "--limit-status" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--limit-status", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--oids-json", required=True,
                            help="JSON array of resting order OIDs to poll")
        parser.add_argument("--since-ms", type=int, default=0,
                            help="userFills lookback floor in epoch ms; 0 = default 7-day window")
        parser.add_argument("--mode", default="live")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#883): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        try:
            oids = json.loads(args.oids_json)
        except Exception as e:
            print(json.dumps({"error": f"invalid --oids-json: {e}"}))
            sys.exit(1)
        if not isinstance(oids, list):
            print(json.dumps({"error": "--oids-json must be a JSON array"}))
            sys.exit(1)
        run_limit_status(args.symbol, oids, args.mode, since_ms=args.since_ms)
    elif "--cancel-order" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--cancel-order", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--oid", type=int, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#883): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        if args.oid <= 0:
            print(json.dumps({"error": "--oid must be > 0"}))
            sys.exit(1)
        run_cancel_order(args.symbol, args.oid, args.mode)
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
        parser.add_argument("--strategy-refs", default=None,
                            help="#640: JSON {'open':{name,params},'closes':[{name,params}...]}; "
                                 "supersedes --params/--open-strategy/--close-strategies when set")
        parser.add_argument("--position-side", default="")
        parser.add_argument("--position-avg-cost", type=float, default=None)
        parser.add_argument("--position-qty", type=float, default=None)
        parser.add_argument("--position-initial-qty", type=float, default=None)
        parser.add_argument("--position-entry-atr", type=float, default=None)
        parser.add_argument("--position-regime", default="")
        parser.add_argument("--mark-price", type=float, default=0.0,
            help="Optional mid from Go's fetchHyperliquidMids cycle; when >0 skips adapter.get_spot_price's duplicate /info allMids call (#768).")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#645): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        from strategy_composition import parse_strategy_refs_arg
        refs = parse_strategy_refs_arg(args.strategy_refs)
        open_strategy_name = refs["open_name"] if refs else args.open_strategy
        close_strategies_arg = refs["close_csv"] if refs else args.close_strategies
        params_override = refs["open_params"] if refs else (json.loads(args.params) if args.params else None)
        close_params_by_name = refs["close_params_by_name"] if refs else None
        position_ctx = _position_ctx_from_args(args)
        regime_windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        run_signal_check(
            args.strategy, args.symbol, args.timeframe, args.mode,
            args.htf_filter, params_override, open_strategy_name,
            close_strategies_arg,
            args.position_side, position_ctx,
            regime_enabled=args.regime_enabled,
            regime_windows_spec=regime_windows_spec,
            ohlcv_limit=args.ohlcv_limit,
            regime_atr_window=args.regime_atr_window,
            regime_payload_json=args.regime_payload_json,
            close_params_by_name=close_params_by_name,
            atr_method=args.atr_method,
            mark_price=args.mark_price,
        )


if __name__ == "__main__":
    main()
