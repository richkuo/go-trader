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
from closed_bar import (
    RULE_HYPERLIQUID_NATIVE,
    ClosedBarHold,
    filter_records_to_boundary,
    hold_metadata,
    htf_closed_fetcher,
    hyperliquid_rows_and_timings,
    rows_sha256,
    sealed_frame_timings,
    select_closed,
    unsupported_open_strategy_reason,
)
from hl_user_fills import apply_user_fills_lookup
from market_payload import (
    MarketPayloadError as MarketPayloadBaseError,
    market_decision_cutoff_ms,
    market_frame_rows,
    market_frame_rows_with_timing,
    market_funding_records,
    market_funding_scalar,
    market_mid,
    market_observation,
    validate_market_payload,
)
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
        ("position_risk_anchor_price", "risk_anchor_price"),
    ):
        value = getattr(args, attr, None)
        if value is not None:
            ctx[key] = value
    regime = (getattr(args, "position_regime", "") or "").strip()
    if regime:
        ctx["regime"] = regime
    return ctx


TP_MODEL_RESTING_LIMIT = "resting_limit"

BATCH_PROTOCOL_VERSION = 1

BATCH_PROTOCOL_VERSIONS = frozenset({1, 2})

BATCH_PROTOCOL_VERSION_MARKET = 2


class SharedSignalStateError(Exception):
    pass


class MarketPayloadError(MarketPayloadBaseError, SharedSignalStateError):
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
        parse_invert_open_signal,
        admit_configured_strategies,
        parse_allow_no_edge_value,
        parse_raw_gate_mode,
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
        parse_invert_open_signal=parse_invert_open_signal,
        normalize_signal=normalize_signal,
        parse_close_strategies=parse_close_strategies,
        admit_configured_strategies=admit_configured_strategies,
        parse_allow_no_edge_value=parse_allow_no_edge_value,
        parse_raw_gate_mode=parse_raw_gate_mode,
    )


def _offline_sz_decimals(symbol):
    from adapter import sz_decimals_from_meta_cache

    return sz_decimals_from_meta_cache(symbol)


def _venue_min_order_notional_usd():
    try:
        from adapter import MIN_ORDER_NOTIONAL_USD

        return float(MIN_ORDER_NOTIONAL_USD)
    except Exception:
        return 10.0


def _venue_min_order_notional_margin():
    try:
        from adapter import MIN_ORDER_NOTIONAL_SAFETY_MARGIN

        return max(float(MIN_ORDER_NOTIONAL_SAFETY_MARGIN), 0.0)
    except Exception:
        return 0.03


def resolve_venue_lot_decimals(shared, symbol):
    try:
        adapter = shared.get("adapter")
        if adapter is not None:
            value = adapter.lot_size_decimals(symbol)
        else:
            value = _offline_sz_decimals(symbol)
    except Exception as exc:
        print(f"[WARN] venue lot size unresolved for {symbol}: {exc}", file=sys.stderr)
        return None
    if value is None or isinstance(value, bool):
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def apply_venue_close_gate(decision, position_ctx, price, lot_decimals, min_notional_usd, position_side="",
                           min_notional_margin=0.0):
    if not decision or lot_decimals is None:
        return decision
    try:
        close_fraction = float(decision.get("close_fraction", 0.0) or 0.0)
    except (TypeError, ValueError):
        return decision
    if close_fraction <= 0 or close_fraction >= 1:
        return decision
    try:
        current_qty = float((position_ctx or {}).get("current_quantity", 0.0) or 0.0)
    except (TypeError, ValueError):
        return decision
    if current_qty <= 0:
        return decision
    from adapter import floor_lot_size

    requested_qty = current_qty * close_fraction
    floored_qty = floor_lot_size(requested_qty, lot_decimals)
    try:
        px = float(price or 0.0)
    except (TypeError, ValueError):
        px = 0.0
    notional = floored_qty * px if px > 0 else None
    gate_threshold = float(min_notional_usd) * (1.0 + max(float(min_notional_margin or 0.0), 0.0))
    below_lot = floored_qty <= 0
    below_value = notional is not None and notional < gate_threshold
    if not below_lot and not below_value:
        return decision
    from strategy_composition import compose_signal

    gated = dict(decision)
    gated["close_fraction"] = 0.0
    gated["signal"] = compose_signal(decision.get("open_action", "none"), 0.0, position_side)
    gated["close_gate"] = "below_venue_minimum"
    gated["close_gate_detail"] = {
        "requested_qty": requested_qty,
        "floored_qty": floored_qty,
        "lot_decimals": int(lot_decimals),
        "notional_usd": notional,
        "min_notional_usd": float(min_notional_usd),
        "gate_threshold_usd": gate_threshold,
    }
    return gated


def slot_admission(slot, deps):
    return (
        deps.parse_raw_gate_mode(slot.get("mode_args")),
        deps.parse_allow_no_edge_value(slot),
    )


def _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies, admission):
    gate_mode, acknowledgement = admission
    deps.admit_configured_strategies(
        strategy_name,
        open_strategy,
        close_strategies,
        gate_mode,
        acknowledgement,
        deps.get_strategy,
        deps.get_close_strategy,
        deps.list_strategies,
        deps.list_close_strategies,
    )


def build_shared_signal_state(symbol, timeframe, *, adapter=None, df=None,
                              ohlcv_limit=200, atr_method="simple", mark_price=0.0,
                              regime_enabled=False, regime_windows_spec=None,
                              regime_payload_json=None, mode="paper",
                              regime_period=14, regime_adx_threshold=20.0,
                              market=None, closed_bar=False, decision_regime_timeframe="",
                              timed_rows=False):
    timed = bool(closed_bar or timed_rows)
    closed = {
        "requested": bool(closed_bar),
        "timed": timed,
        "cutoff_ms": 0,
        "rows": None,
        "timings": None,
        "hold": "",
    }
    if market is not None:
        validate_market_payload(market, MarketPayloadError)
        if timed:
            all_rows, timing = market_frame_rows_with_timing(
                market, symbol, timeframe, MarketPayloadError, limit=ohlcv_limit + 1)
            rows = all_rows[-ohlcv_limit:]
            closed["cutoff_ms"] = market_decision_cutoff_ms(market, MarketPayloadError)
            try:
                closed["timings"] = sealed_frame_timings(all_rows, timing, timeframe)
                closed["rows"] = all_rows
            except ClosedBarHold as e:
                closed["hold"] = e.reason
        else:
            rows = market_frame_rows(market, symbol, timeframe, MarketPayloadError, limit=ohlcv_limit)
        if len(rows) < 30:
            raise InsufficientCandlesError(len(rows))
        df = _make_dataframe(rows)
        adapter = None
    elif timed and df is not None:
        closed["hold"] = "a prebuilt candle frame carries no bar timing"

    if df is None:
        if adapter is None:
            raise SharedSignalStateError("no adapter and no prebuilt DataFrame")
        print(f"Fetching {symbol} {timeframe} from Hyperliquid ({mode})...", file=sys.stderr)
        if timed:
            closed["cutoff_ms"] = int(time.time() * 1000)
            raw_candles = adapter.get_ohlcv_candles(symbol, interval=timeframe, limit=ohlcv_limit + 1)
            try:
                all_rows, timings = hyperliquid_rows_and_timings(raw_candles or [])
            except ClosedBarHold as e:
                raise SharedSignalStateError(f"Hyperliquid candles are malformed: {e.reason}")
            closed["rows"], closed["timings"] = all_rows, timings
            candles = all_rows[-ohlcv_limit:]
        else:
            candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=ohlcv_limit)
        if not candles or len(candles) < 30:
            raise InsufficientCandlesError(len(candles) if candles else 0)
        df = _make_dataframe(candles)

    price_override = 0.0
    if mark_price and mark_price > 0:
        price_override = float(mark_price)
    elif market is not None:
        payload_mid = market_mid(market, symbol, MarketPayloadError)
        if payload_mid:
            price_override = float(payload_mid)
    elif adapter is not None:
        try:
            mid = adapter.get_spot_price(symbol)
            if mid > 0:
                price_override = float(mid)
        except Exception:
            pass

    return {
        "adapter": adapter,
        "market": market,
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
        "ohlcv_limit": int(ohlcv_limit),
        "closed": closed,
        "decision_regime_timeframe": (decision_regime_timeframe or "").strip() or timeframe,
        "decision_cache": {},
    }


def _shared_funding_scalar(shared, symbol):
    if shared.get("funding_scalar") is not None:
        return shared["funding_scalar"]
    market = shared.get("market")
    if market is not None:
        params = market_funding_scalar(market, symbol, MarketPayloadError)
        shared["funding_scalar"] = params
        return params
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
    market = shared.get("market")
    if market is not None:
        start_ms = int(shared["df"]["timestamp"].iloc[0])
        records = market_funding_records(market, symbol, start_ms, MarketPayloadError)
        shared["funding_records"] = records
        return records
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


OPEN_INTEREST_STRATEGY = "open_interest_breakout"


def _shared_open_interest(shared, symbol):
    market = shared.get("market")
    if market is None:
        return {"available": False, "kind": "open_interest", "coin": symbol,
                "reason": "open_interest_breakout reads open interest only from the sealed market "
                          "feed payload; this check has none and never fetches it"}
    return market_observation(market, symbol, "open_interest", MarketPayloadError)


def _shared_htf_frame(shared, sym, tf, limit):
    cache = shared["htf_cache"]
    key = (sym, tf, limit)
    if key not in cache:
        market = shared.get("market")
        if market is not None:
            rows = market_frame_rows(market, sym, tf, MarketPayloadError, limit=limit)
            cache[key] = _make_dataframe(rows)
        else:
            adapter = shared.get("adapter")
            candles = adapter.get_ohlcv(sym, interval=tf, limit=limit) if adapter is not None else None
            cache[key] = _make_dataframe(candles) if candles else None
    frame = cache[key]
    return frame.copy() if frame is not None else None


def _closed_frame_selection(shared, sym, tf, limit, cutoff_ms, min_rows):
    market = shared.get("market")
    if market is not None:
        try:
            rows, timing = market_frame_rows_with_timing(market, sym, tf, MarketPayloadError, limit=limit + 1)
        except MarketPayloadError as e:
            raise ClosedBarHold(str(e))
        timings = sealed_frame_timings(rows, timing, tf)
    else:
        adapter = shared.get("adapter")
        if adapter is None:
            raise ClosedBarHold(f"no candle source for {sym} {tf}")
        try:
            raw_candles = adapter.get_ohlcv_candles(sym, interval=tf, limit=limit + 1)
        except Exception as e:
            raise ClosedBarHold(f"fetching {sym} {tf} candles failed: {e}")
        rows, timings = hyperliquid_rows_and_timings(raw_candles or [])
    return select_closed(rows, timings, timeframe=tf, cutoff_ms=cutoff_ms,
                         rule=RULE_HYPERLIQUID_NATIVE, keep=limit, min_rows=min_rows)


def _shared_closed_decision(shared):
    cache = shared["decision_cache"]
    if "primary" in cache:
        return cache["primary"]
    closed = shared["closed"]
    out = {"selection": None, "hold": ""}
    if closed["hold"]:
        out["hold"] = closed["hold"]
    elif closed["rows"] is None or closed["timings"] is None:
        out["hold"] = "no timed candle frame was acquired for this evaluation"
    else:
        try:
            out["selection"] = select_closed(
                closed["rows"], closed["timings"],
                timeframe=shared["timeframe"],
                cutoff_ms=closed["cutoff_ms"],
                rule=RULE_HYPERLIQUID_NATIVE,
                keep=shared["ohlcv_limit"],
            )
        except ClosedBarHold as e:
            out["hold"] = e.reason
    cache["primary"] = out
    return out


def _shared_decision_regime(shared, selection):
    cache = shared["decision_cache"]
    if "regime" in cache:
        return cache["regime"]
    out = {"payload": None, "strategy": None, "hold": ""}
    if not shared["regime_enabled"]:
        cache["regime"] = out
        return out
    tf = shared["decision_regime_timeframe"]
    try:
        if tf == shared["timeframe"]:
            regime_df = _make_dataframe(selection.rows)
        else:
            regime_sel = _closed_frame_selection(
                shared, shared["symbol"], tf, shared["ohlcv_limit"], selection.boundary_ms, 30)
            regime_df = _make_dataframe(regime_sel.rows)
        payload, _live, strategy_payload = prepare_check_regime(
            regime_df,
            regime_enabled=True,
            period=shared.get("regime_period", 14),
            adx_threshold=shared.get("regime_adx_threshold", 20.0),
            windows_spec=shared["regime_windows_spec"],
            injected_payload_json=None,
        )
        out["payload"], out["strategy"] = payload, strategy_payload
    except ClosedBarHold as e:
        out["hold"] = f"decision regime {tf}: {e.reason}"
    cache["regime"] = out
    return out


def _shared_decision_htf(shared, sym, tf, limit, boundary_ms):
    cache = shared["decision_cache"]
    key = ("htf", sym, tf, limit, boundary_ms)
    if key not in cache:
        try:
            sel = _closed_frame_selection(shared, sym, tf, limit, boundary_ms, 50)
            cache[key] = {"frame": _make_dataframe(sel.rows), "hold": ""}
        except ClosedBarHold as e:
            cache[key] = {"frame": None, "hold": f"higher timeframe {tf}: {e.reason}"}
    return cache[key]


def _shared_decision_funding_records(shared, symbol, start_ms, boundary_ms):
    cache = shared["decision_cache"]
    key = ("funding", symbol, int(start_ms), int(boundary_ms))
    if key in cache:
        return cache[key]
    out = {"records": None, "hold": ""}
    market = shared.get("market")
    try:
        if market is not None:
            try:
                records = market_funding_records(market, symbol, start_ms, MarketPayloadError)
            except MarketPayloadError as e:
                raise ClosedBarHold(str(e))
        else:
            adapter = shared.get("adapter")
            if adapter is None:
                raise ClosedBarHold("no funding source")
            try:
                records = adapter.get_funding_history_range(symbol, start_ms)
            except Exception as e:
                raise ClosedBarHold(f"funding history fetch failed: {e}")
        out["records"] = filter_records_to_boundary(records, boundary_ms)
    except ClosedBarHold as e:
        out["hold"] = f"funding history: {e.reason}"
    cache[key] = out
    return out


RESTING_RULE_REQUEST_KEYS = frozenset({"v", "k_ticks", "sz_decimals", "entry_time_ms", "stop_trigger_px", "hold_reason",
                                       "scanned_through_ms", "prior_reach_px"})


def _request_int(raw, field, minimum=None, maximum=None, allow_none=False):
    if raw is None and allow_none:
        return None
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise ValueError(f"resting_tp_rule.{field} must be an integer, got {raw!r}")
    if (minimum is not None and raw < minimum) or (maximum is not None and raw > maximum):
        raise ValueError(f"resting_tp_rule.{field} is out of range: {raw!r}")
    return raw


def parse_resting_rule_request(raw):
    if raw is None:
        return None
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError as e:
            raise ValueError(f"resting_tp_rule is not valid JSON: {e}")
    if not isinstance(raw, dict):
        raise ValueError(f"resting_tp_rule must be a JSON object, got {type(raw).__name__}")
    unknown = sorted(set(raw) - RESTING_RULE_REQUEST_KEYS)
    missing = sorted(RESTING_RULE_REQUEST_KEYS - set(raw))
    if unknown or missing:
        raise ValueError(f"resting_tp_rule keys must be exactly {sorted(RESTING_RULE_REQUEST_KEYS)}; unknown={unknown} missing={missing}")
    from close_registry_loader import resting_rule_helpers
    helpers = resting_rule_helpers()
    if _request_int(raw["v"], "v") != helpers.RESTING_TP_RULE_VERSION:
        raise ValueError(f"resting_tp_rule.v {raw['v']!r} is not {helpers.RESTING_TP_RULE_VERSION}")
    if _request_int(raw["k_ticks"], "k_ticks", minimum=1) != helpers.RESTING_TP_TRADE_THROUGH_TICKS:
        raise ValueError(f"resting_tp_rule.k_ticks {raw['k_ticks']!r} is not the shared constant {helpers.RESTING_TP_TRADE_THROUGH_TICKS}")
    stop = raw["stop_trigger_px"]
    if isinstance(stop, bool) or not isinstance(stop, (int, float)) or not math.isfinite(float(stop)) or float(stop) < 0:
        raise ValueError(f"resting_tp_rule.stop_trigger_px must be a finite number >= 0, got {stop!r}")
    if not isinstance(raw["hold_reason"], str):
        raise ValueError(f"resting_tp_rule.hold_reason must be a string, got {raw['hold_reason']!r}")
    scanned_through = _request_int(raw["scanned_through_ms"], "scanned_through_ms", minimum=0)
    prior = raw["prior_reach_px"]
    if prior is not None and (isinstance(prior, bool) or not isinstance(prior, (int, float))
                              or not math.isfinite(float(prior)) or float(prior) <= 0):
        raise ValueError(f"resting_tp_rule.prior_reach_px must be null or a finite number > 0, got {prior!r}")
    if prior is not None and scanned_through == 0:
        raise ValueError("resting_tp_rule.prior_reach_px needs a scanned_through_ms watermark")
    return {
        "scanned_through_ms": scanned_through,
        "prior_reach_px": prior,
        "v": raw["v"],
        "k_ticks": raw["k_ticks"],
        "sz_decimals": _request_int(raw["sz_decimals"], "sz_decimals", minimum=0, maximum=12, allow_none=True),
        "entry_time_ms": _request_int(raw["entry_time_ms"], "entry_time_ms", minimum=0),
        "stop_trigger_px": stop,
        "hold_reason": raw["hold_reason"].strip(),
    }


def _resting_rule_refusal(mode, close_names):
    if mode == "live":
        return "resting_tp_rule is paper-only: the venue decides live take-profit fills"
    from close_registry_loader import resting_rule_helpers
    helpers = resting_rule_helpers()
    for name in close_names or []:
        if name in helpers.RESTING_TP_UNSUPPORTED_CLOSES:
            return f"resting_tp_rule does not support {name}"
    return ""


def _resting_rule_observation(shared, request, side):
    from close_registry_loader import resting_rule_helpers
    helpers = resting_rule_helpers()
    obs = {
        "hold_reason": request["hold_reason"],
        "cutoff_ms": int(shared["closed"]["cutoff_ms"] or 0),
        "entry_bar_open_ms": 0,
        "coverage": "complete",
        "rows": [],
        "timings": [],
        "bars": [],
    }
    if not obs["hold_reason"] and request["entry_time_ms"] <= 0:
        obs["hold_reason"] = "entry_time_unknown"
    if not obs["hold_reason"] and side not in ("long", "short"):
        obs["hold_reason"] = "position_missing"
    if not obs["hold_reason"]:
        primary = _shared_closed_decision(shared)
        if primary["hold"]:
            obs["hold_reason"] = "closed_history_unavailable: " + primary["hold"]
        else:
            selection = primary["selection"]
            entry_ms = request["entry_time_ms"]
            scanned_through = request["scanned_through_ms"]
            interval = int(selection.interval_ms)
            start = None
            entry_idx = None
            for i, timing in enumerate(selection.timings):
                if timing.open_ms <= entry_ms < timing.open_ms + interval:
                    obs["entry_bar_open_ms"] = int(timing.open_ms)
                    if not scanned_through:
                        entry_idx = i
                        start = i
                        break
                if scanned_through and timing.open_ms > scanned_through:
                    start = i
                    break
                if not scanned_through and timing.open_ms > entry_ms:
                    start = i
                    break
            if start is not None:
                if scanned_through:
                    if int(selection.timings[start].open_ms) > scanned_through + interval:
                        obs["coverage"] = "frame_truncated"
                elif entry_idx is None:
                    obs["coverage"] = "frame_truncated"
                obs["rows"] = selection.rows[start:]
                obs["timings"] = selection.timings[start:]
                obs["bars"] = [
                    helpers.resting_rule_bar(
                        timing.open_ms, row[2], row[3], row[4], side,
                        entry_bar=(start + i) == entry_idx, walk_mode=True,
                    )
                    for i, (row, timing) in enumerate(zip(obs["rows"], obs["timings"]))
                ]
    obs["rule"] = helpers.build_resting_rule(
        sz_decimals=request["sz_decimals"], bars=obs["bars"],
        stop_trigger_px=request["stop_trigger_px"], coverage=obs["coverage"],
        prior_reach_px=request["prior_reach_px"],
        held=bool(obs["hold_reason"]), hold_reason=obs["hold_reason"],
    )
    obs["next_scanned_through_ms"] = request["scanned_through_ms"]
    obs["next_reach_px"] = request["prior_reach_px"]
    obs["stop_reached_bar_open_ms"] = 0
    if not obs["hold_reason"]:
        scan = helpers.resting_rule_scan(helpers.resting_rule_input({helpers.RESTING_TP_RULE_KEY: obs["rule"]}), side)
        if scan["last_scanned_open_ms"]:
            obs["next_scanned_through_ms"] = int(scan["last_scanned_open_ms"])
        if scan["best"] is not None:
            obs["next_reach_px"] = float(scan["best"])
        obs["stop_reached_bar_open_ms"] = int(scan["stop_bar"] or 0)
    return obs


def _resting_rule_echo(request, obs, decision):
    evidence = (decision or {}).get("close_resting_fill") or {}
    timings = obs["timings"]
    return {
        "v": request["v"],
        "enabled": True,
        "held": bool(obs["hold_reason"]),
        "hold_reason": obs["hold_reason"],
        "k_ticks": request["k_ticks"],
        "sz_decimals": request["sz_decimals"],
        "cutoff_ms": obs["cutoff_ms"],
        "entry_time_ms": request["entry_time_ms"],
        "entry_bar_open_ms": obs["entry_bar_open_ms"],
        "stop_trigger_px": request["stop_trigger_px"],
        "coverage": obs["coverage"],
        "observed_bars": len(obs["bars"]),
        "first_observed_open_ms": int(timings[0].open_ms) if timings else 0,
        "last_observed_open_ms": int(timings[-1].open_ms) if timings else 0,
        "observed_rows_sha256": rows_sha256(obs["rows"], timings) if timings else "",
        "scanned_through_ms": request["scanned_through_ms"],
        "prior_reach_px": request["prior_reach_px"],
        "next_scanned_through_ms": obs["next_scanned_through_ms"],
        "next_reach_px": obs["next_reach_px"],
        "reach_bar_open_ms": int(evidence.get("reach_bar_open_ms") or 0),
        "stop_reached_bar_open_ms": obs["stop_reached_bar_open_ms"],
        "verdict": str(evidence.get("verdict") or ""),
    }


def evaluate_signal_slot(shared, slot, deps=None, admission=None):
    if deps is None:
        deps = _signal_check_deps()
    if admission is None:
        admission = slot_admission(slot, deps)

    strategy_name = slot["strategy"]
    mode = slot.get("mode") or shared.get("mode") or "paper"
    open_strategy = slot.get("open_strategy") or None
    close_strategies = slot.get("close_strategies") or None
    close_params_by_name = slot.get("close_params_by_name") or None
    close_owner = slot.get("close_owner") or None
    strategy_params_override = slot.get("params") or None
    position_side = slot.get("position_side") or ""
    position_ctx = slot.get("position_ctx") or None
    if position_ctx:
        position_ctx = {**position_ctx, "tp_model": TP_MODEL_RESTING_LIMIT}
    htf_filter_enabled = bool(slot.get("htf_filter"))
    regime_atr_window = slot.get("regime_atr_window") or ""
    invert_present = "invert_open_signal" in slot
    invert_open_signal = deps.parse_invert_open_signal(slot.get("invert_open_signal")) if invert_present else False

    _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies, admission)
    resting_request = parse_resting_rule_request(slot.get("resting_tp_rule"))
    if resting_request is not None:
        refusal = _resting_rule_refusal(mode, deps.parse_close_strategies(close_strategies))
        if refusal:
            raise ValueError(refusal)
        if not shared["closed"].get("timed"):
            raise ValueError("resting_tp_rule slot reached a shared state built without bar timing")

    symbol = shared["symbol"]
    timeframe = shared["timeframe"]
    atr_method = shared["atr_method"]
    df = shared["df"].copy()

    open_close_enabled = bool(open_strategy or close_strategies or close_owner or invert_present)
    funding_aware_name = open_strategy or strategy_name
    closed_bar = slot.get("closed_bar_decisions") is True
    if closed_bar:
        for name in (strategy_name, funding_aware_name):
            why = unsupported_open_strategy_reason(name)
            if why:
                raise ValueError(f"closed_bar_decisions does not support {name}: {why}")
        if not shared["closed"]["requested"]:
            raise ValueError("closed_bar_decisions slot reached a shared state built without bar timing")

    stdout_regime, live_regime, strategy_regime = prepare_check_regime(
        df,
        regime_enabled=shared["regime_enabled"],
        period=shared.get("regime_period", 14),
        adx_threshold=shared.get("regime_adx_threshold", 20.0),
        windows_spec=shared["regime_windows_spec"],
        atr_window=regime_atr_window,
        injected_payload_json=shared["regime_payload_json"],
    )

    closed_meta = None
    decision_regime_payload = None
    decision_df = df
    protection_params = None
    htf_closed = None
    if closed_bar:
        primary = _shared_closed_decision(shared)
        hold = primary["hold"]
        selection = primary["selection"]
        decision_params = {}
        if not hold:
            boundary = selection.boundary_ms
            if funding_aware_name == "funding_skew":
                funding = _shared_decision_funding_records(
                    shared, symbol, int(selection.rows[0][0]), boundary)
                hold = funding["hold"]
                if not hold and funding["records"]:
                    decision_params["funding_records"] = funding["records"]
        if not hold:
            regime_view = _shared_decision_regime(shared, selection)
            hold = regime_view["hold"]
            decision_regime_payload = regime_view["payload"]
            decision_params["regime"] = regime_view["strategy"] if shared["regime_enabled"] else strategy_regime
        if not hold and htf_filter_enabled and funding_aware_name not in ("delta_neutral_funding", "funding_skew"):
            from htf_filter import get_default_htf
            htf_closed = _shared_decision_htf(shared, symbol, get_default_htf(timeframe), 60, selection.boundary_ms)
            hold = htf_closed["hold"]
        protection_params = {"regime": strategy_regime}
        if strategy_params_override:
            protection_params = {**strategy_params_override, **protection_params}
        if hold:
            closed_meta = hold_metadata(hold, shared["closed"]["cutoff_ms"])
            decision_regime_payload = None
            decision_df = None
            strategy_params = {}
            print(f"Closed-bar decision held for {symbol} {timeframe}: {hold}", file=sys.stderr)
        else:
            closed_meta = selection.metadata()
            decision_df = _make_dataframe(selection.rows)
            strategy_params = decision_params
    else:
        strategy_params = {}
        if strategy_name == "delta_neutral_funding":
            strategy_params.update(_shared_funding_scalar(shared, symbol))
        if funding_aware_name == "funding_skew":
            records = _shared_funding_records(shared, symbol)
            if records:
                strategy_params["funding_records"] = records
        if funding_aware_name == OPEN_INTEREST_STRATEGY:
            strategy_params["open_interest_observations"] = _shared_open_interest(shared, symbol)
        strategy_params["regime"] = strategy_regime
    if strategy_params_override:
        merged = {**strategy_params_override, **strategy_params}
        strategy_params = merged
    decision = None
    resting_obs = None
    if resting_request is not None:
        resting_obs = _resting_rule_observation(
            shared, resting_request, str((position_ctx or {}).get("side") or position_side or "").strip().lower())
        if resting_obs["hold_reason"]:
            print(f"Resting take-profit rule held for {symbol} {timeframe}: {resting_obs['hold_reason']}", file=sys.stderr)
    if open_close_enabled:
        market_ctx = {"mark_price": float(df["close"].iloc[-1])}
        atr_now = shared["atr"]
        if atr_now > 0:
            market_ctx["atr"] = atr_now
        if live_regime:
            market_ctx["regime"] = live_regime
        if resting_obs is not None:
            market_ctx["resting_tp_rule"] = resting_obs["rule"]
        evaluation = deps.evaluate_open_close(
            deps.apply_strategy,
            deps.get_strategy,
            decision_df,
            strategy_name,
            open_strategy,
            deps.parse_close_strategies(close_strategies),
            position_side,
            strategy_params or None,
            position_ctx,
            close_evaluate=deps.close_evaluate,
            market_ctx=market_ctx,
            close_params_by_name=close_params_by_name,
            close_owner=close_owner,
            invert_open_signal=invert_open_signal,
            **({"protection_df": df, "protection_params": protection_params} if closed_bar else {}),
        )
        result_df = evaluation.open_result_df
        signal = evaluation.open_signal
    elif decision_df is not None:
        result_df = deps.apply_strategy(strategy_name, decision_df, strategy_params or None)
        signal = deps.normalize_signal(result_df.iloc[-1].get("signal", 0))
    else:
        deps.get_strategy(strategy_name)
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
    htf_strategy_name = open_strategy or strategy_name
    if decision_df is not None and htf_filter_enabled and htf_strategy_name not in ("delta_neutral_funding", "funding_skew"):
        from htf_filter import htf_trend_filter, apply_htf_filter

        if closed_bar:
            _fetch_htf = htf_closed_fetcher(htf_closed["frame"])
        else:
            def _fetch_htf(sym, tf, limit):
                return _shared_htf_frame(shared, sym, tf, limit)

        htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
        original_signal = signal
        signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
        if signal != original_signal:
            print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

    if shared["price_override"] > 0:
        price = shared["price_override"]

    if open_close_enabled:
        decision = deps.finalize_decision(evaluation, position_side, signal, invert_open_signal)
        if mode == "live" and 0 < float(decision.get("close_fraction", 0.0) or 0.0) < 1:
            gated = apply_venue_close_gate(
                decision, position_ctx, price,
                resolve_venue_lot_decimals(shared, symbol),
                _venue_min_order_notional_usd(),
                position_side,
                min_notional_margin=_venue_min_order_notional_margin(),
            )
            if gated is not decision:
                detail = gated["close_gate_detail"]
                print(
                    f"Venue close gate: {symbol} close_fraction {decision['close_fraction']:.6g} -> 0 "
                    f"(requested {detail['requested_qty']:.10g}, floored {detail['floored_qty']:.10g} "
                    f"at {detail['lot_decimals']} decimals, notional {detail['notional_usd']}, "
                    f"min {detail['min_notional_usd']}, gate {detail['gate_threshold_usd']:.4g})",
                    file=sys.stderr,
                )
                decision = gated
        signal = decision["signal"]

    indicators = {}
    skip_cols = {
        "open", "high", "low", "close", "volume",
        "timestamp", "signal", "position", "datetime",
    }
    if last is not None:
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
        "price": price,
        "indicators": indicators,
        "regime": stdout_regime,
        "mode": mode,
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if decision:
        output.update(decision)
    if resting_request is not None:
        output["resting_tp_rule"] = _resting_rule_echo(resting_request, resting_obs, decision)
    if closed_meta is not None:
        output["closed_bar_decision"] = closed_meta
        if decision_regime_payload is not None:
            output["decision_regime"] = decision_regime_payload
    return output


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params_override=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_windows_spec=None, ohlcv_limit=200, regime_atr_window="",
                     regime_payload_json=None,
                     close_params_by_name=None,
                     atr_method="simple",
                     mark_price=0.0,
                     market=None,
                     close_owner=None,
                     invert_open_signal=None,
                     mode_args=None,
                     closed_bar=False,
                     decision_regime_timeframe="",
                     resting_tp_rule=None):
    try:
        deps = _signal_check_deps()
        from strategy_composition import parse_allow_no_edge_tokens
        admission = (deps.parse_raw_gate_mode(mode_args), parse_allow_no_edge_tokens(mode_args))
        _validate_slot_strategy_names(deps, strategy_name, open_strategy, close_strategies, admission)

        adapter = None
        if market is None:
            from adapter import HyperliquidExchangeAdapter

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
            market=market,
            closed_bar=closed_bar,
            decision_regime_timeframe=decision_regime_timeframe,
            timed_rows=resting_tp_rule is not None,
        )
        slot = {
            "id": strategy_name,
            "strategy": strategy_name,
            "mode": mode,
            "htf_filter": htf_filter_enabled,
            "params": strategy_params_override,
            "open_strategy": open_strategy,
            "close_strategies": close_strategies,
            "close_params_by_name": close_params_by_name,
            "close_owner": close_owner,
            "position_side": position_side,
            "position_ctx": position_ctx,
            "regime_atr_window": regime_atr_window,
        }
        if invert_open_signal is not None:
            slot["invert_open_signal"] = invert_open_signal
        if closed_bar:
            slot["closed_bar_decisions"] = True
        if resting_tp_rule is not None:
            slot["resting_tp_rule"] = resting_tp_rule
        output = evaluate_signal_slot(shared, slot, deps=deps, admission=admission)
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
    slots, _market = parse_batch_request(raw_stdin)
    return slots


def parse_market_stdin(raw_stdin):
    payload = json.loads(raw_stdin)
    if not isinstance(payload, dict):
        raise ValueError("market stdin payload must be a JSON object")
    version = int(payload.get("v", BATCH_PROTOCOL_VERSION_MARKET))
    if version != BATCH_PROTOCOL_VERSION_MARKET:
        raise ValueError(f"--market-stdin requires envelope version {BATCH_PROTOCOL_VERSION_MARKET}, got {version}")
    market = payload.get("market")
    validate_market_payload(market, MarketPayloadError)
    return market


def parse_batch_request(raw_stdin):
    payload = json.loads(raw_stdin)
    if not isinstance(payload, dict):
        raise ValueError("batch payload must be a JSON object")
    version = int(payload.get("v", BATCH_PROTOCOL_VERSION))
    if version not in BATCH_PROTOCOL_VERSIONS:
        raise ValueError(f"unsupported batch protocol version {version}")
    market = payload.get("market")
    if version >= BATCH_PROTOCOL_VERSION_MARKET:
        if not isinstance(market, dict):
            raise ValueError(
                f"batch protocol version {version} requires a 'market' object")
    elif market is not None:
        raise ValueError(
            f"batch protocol version {version} must not carry a 'market' object")
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
        if "mode_args" in slot:
            mode_args = slot["mode_args"]
            if not isinstance(mode_args, list) or not all(isinstance(t, str) for t in mode_args):
                raise ValueError(f"slot {slot_id!r} mode_args must be a list of strings, got {mode_args!r}")
        if "allow_no_edge" in slot and not isinstance(slot["allow_no_edge"], bool):
            raise ValueError(f"slot {slot_id!r} allow_no_edge must be a JSON boolean, got {slot['allow_no_edge']!r}")
        if "closed_bar_decisions" in slot and not isinstance(slot["closed_bar_decisions"], bool):
            raise ValueError(f"slot {slot_id!r} closed_bar_decisions must be a JSON boolean, got {slot['closed_bar_decisions']!r}")
        if "resting_tp_rule" in slot and not isinstance(slot["resting_tp_rule"], dict):
            raise ValueError(f"slot {slot_id!r} resting_tp_rule must be a JSON object, got {slot['resting_tp_rule']!r}")
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
                slot["close_owner"] = parsed["close_owner"]
                if "invert_open_signal" in parsed:
                    slot["invert_open_signal"] = parsed["invert_open_signal"]
        if not str(slot.get("strategy") or "").strip():
            raise ValueError(f"slot {slot_id!r} is missing 'strategy'")
        out.append(slot)
    return out, market


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
                           regime_payload_json=None, adapter=None, df=None, market=None,
                           decision_regime_timeframe=""):
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
        if market is not None:
            adapter = None
            df = None
        elif adapter is None and df is None:
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
            market=market,
            closed_bar=any(slot.get("closed_bar_decisions") is True for slot in slots),
            decision_regime_timeframe=decision_regime_timeframe,
            timed_rows=any("resting_tp_rule" in slot for slot in slots),
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


def _resolve_placement_by_book_diff(adapter, symbol, pre_oids, label="SL"):
    if pre_oids is None:
        return ("unknown", None)
    try:
        now_oids = adapter.open_order_oids(symbol)
    except Exception as oe:
        print(f"[WARN] outcome-unknown {label} placement: open_order_oids({symbol}) re-read failed: {oe}", file=sys.stderr)
        return ("unknown", None)
    if now_oids is None:
        return ("unknown", None)
    fresh = [int(o) for o in now_oids if int(o) not in pre_oids]
    if len(fresh) == 1:
        print(f"[WARN] unreadable {label} placement response resolved to resting oid={fresh[0]}", file=sys.stderr)
        return ("resting", fresh[0])
    if not fresh:
        return ("none", None)
    return ("unknown", None)


def _resolve_sl_placement_by_book_diff(adapter, symbol, pre_oids):
    return _resolve_placement_by_book_diff(adapter, symbol, pre_oids, label="SL")


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


def _extract_execute_fill(sdk_response):
    if not isinstance(sdk_response, dict):
        return None, f"exchange returned no usable order response: {sdk_response!r}", "unknown"
    if sdk_response.get("status") != "ok":
        return None, f"exchange rejected order: {sdk_response}", "rejected"

    response = sdk_response.get("response")
    data = response.get("data") if isinstance(response, dict) else None
    statuses = data.get("statuses") if isinstance(data, dict) else None
    if not isinstance(statuses, list) or not statuses:
        return None, "exchange returned no order status", "unknown"

    status = statuses[0]
    if not isinstance(status, dict):
        return None, "exchange returned a malformed order status", "unknown"
    if "error" in status:
        return None, f"exchange rejected order: {status['error']}", "rejected"
    if "filled" not in status or not isinstance(status["filled"], dict):
        return None, "exchange returned no filled status", "unknown"

    filled = status["filled"]
    raw_avg_px = filled.get("avgPx")
    raw_total_sz = filled.get("totalSz")
    try:
        avg_px = float(raw_avg_px)
        total_sz = float(raw_total_sz)
    except (TypeError, ValueError):
        return None, f"exchange returned malformed fill values (avgPx={raw_avg_px!r}, totalSz={raw_total_sz!r})", "unknown"
    if not math.isfinite(avg_px) or not math.isfinite(total_sz):
        return None, f"exchange returned malformed fill values (avgPx={raw_avg_px!r}, totalSz={raw_total_sz!r})", "unknown"
    if total_sz == 0:
        return None, f"exchange returned no confirmed fill (sz={total_sz:.8f} px={avg_px:.8f})", "rejected"
    if avg_px <= 0 or total_sz < 0:
        return None, f"exchange returned no confirmed fill (sz={total_sz:.8f} px={avg_px:.8f})", "unknown"

    fill = {"avg_px": avg_px, "total_sz": total_sz}
    oid = filled.get("oid")
    if oid is not None:
        try:
            fill["oid"] = int(oid)
        except (TypeError, ValueError):
            print(f"[WARN] ignoring malformed fill oid={oid!r}", file=sys.stderr)
    fee = filled.get("fee")
    if fee is not None:
        try:
            parsed_fee = float(fee)
            if math.isfinite(parsed_fee):
                fill["fee"] = parsed_fee
        except (TypeError, ValueError):
            print(f"[WARN] ignoring malformed fill fee={fee!r}", file=sys.stderr)
    return fill, "", "filled"


def _add_execute_cancel_metadata(payload, cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids):
    if cancel_err:
        payload["cancel_stop_loss_error"] = cancel_err
    if cancel_succeeded:
        payload["cancel_stop_loss_succeeded"] = True
    if cancel_succeeded_oids:
        payload["cancel_stop_loss_succeeded_oids"] = cancel_succeeded_oids
    if cancel_failed_oids:
        payload["cancel_stop_loss_failed_oids"] = cancel_failed_oids


def _oid_is_open(open_oids: set[int] | None, oid: int) -> bool:
    return oid > 0 and open_oids is not None and int(oid) in open_oids


def _stop_trigger_looser(side: str, candidate: float, requested: float) -> bool:
    eps = abs(requested) * 1e-9
    if side == "long":
        return candidate < requested - eps
    return candidate > requested + eps


def _preserved_stop_trigger_px(adapter, symbol: str, side: str, requested: float, preserve_moved_stop: bool):
    rounded = adapter.round_perps_trigger_px(symbol, requested)
    valid = isinstance(rounded, (int, float)) and math.isfinite(rounded) and rounded > 0
    if valid and not (preserve_moved_stop and _stop_trigger_looser(side, rounded, requested)):
        return rounded
    if not preserve_moved_stop:
        return None
    tick = adapter.perps_trigger_px_tick(symbol, requested)
    if not isinstance(tick, (int, float)) or not math.isfinite(tick) or tick <= 0:
        return None
    stepped = requested + tick if side == "long" else requested - tick
    if stepped <= 0:
        return None
    nudged = adapter.round_perps_trigger_px(symbol, stepped)
    if not isinstance(nudged, (int, float)) or not math.isfinite(nudged) or nudged <= 0:
        return None
    if _stop_trigger_looser(side, nudged, requested):
        return None
    return nudged


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
    stop_loss_trigger_px=None,
    preserve_moved_stop=False,
):
    if mode != "live":
        print(json.dumps({"error": "--sync-protection requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)
    if stop_loss_atr_mult > 0 and (preserve_moved_stop or stop_loss_trigger_px is not None):
        px = stop_loss_trigger_px
        if not isinstance(px, (int, float)) or not math.isfinite(px) or px <= 0:
            print(json.dumps({
                "error": f"--stop-loss-trigger-px must be a positive finite price (got {px!r}, preserve_moved_stop={bool(preserve_moved_stop)}); no stop was cancelled or placed",
            }, cls=SafeEncoder))
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
        surplus_cancel_not_open = []
        for surplus_oid in cancel_tp_oids or []:
            oid = int(surplus_oid)
            if oid <= 0:
                continue
            if open_oids is None:
                surplus_cancel_failed.append(oid)
                continue
            if _oid_is_open(open_oids, oid):
                try:
                    kind, payload = _classify_cancel_response(adapter.cancel_order_by_oid(symbol, oid))
                    if kind != "ok":
                        surplus_cancel_failed.append(oid)
                        print(
                            f"[WARN] cancel surplus TP OID={oid} rejected: {payload}",
                            file=sys.stderr,
                        )
                except Exception as ce:
                    surplus_cancel_failed.append(oid)
                    print(
                        f"[WARN] cancel surplus TP OID={oid} failed: {ce}",
                        file=sys.stderr,
                    )
                continue
            fill = _oid_filled_externally(adapter, oid, fill_check_since_ms, fill_hints)
            if fill.get("filled"):
                surplus_cancel_filled.append(oid)
                print(
                    f"[WARN] surplus TP OID={oid} already filled on-chain; not canceling — reconciler will book the close",
                    file=sys.stderr,
                )
                continue
            surplus_cancel_not_open.append(oid)
        if surplus_cancel_failed:
            out["tp_cancel_failed_oids"] = surplus_cancel_failed
        if surplus_cancel_filled:
            out["tp_cancel_filled_oids"] = surplus_cancel_filled
        if surplus_cancel_not_open:
            out["tp_cancel_not_open_oids"] = surplus_cancel_not_open

        if stop_loss_atr_mult > 0:
            if stop_loss_trigger_px is not None:
                sl_px = _preserved_stop_trigger_px(
                    adapter, symbol, side, float(stop_loss_trigger_px), bool(preserve_moved_stop))
            else:
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
                        placed_sz = _placed_stop_size(adapter, symbol, size)
                        if placed_sz > 0:
                            out["stop_loss_size"] = placed_sz
                    elif kind == "error":
                        out["stop_loss_error"] = f"place_stop_loss SDK error: {payload}"
                    else:
                        _resolve_unknown_sl(f"place_stop_loss returned no usable status: {resp}", pre_oids)
                except Exception as se:
                    _resolve_unknown_sl(str(se), pre_oids)

            if sl_px is None:
                out["stop_loss_error"] = (
                    f"the supplied stop trigger {stop_loss_trigger_px!r} cannot be rounded to a venue price "
                    f"without loosening it; no stop was cancelled or placed"
                )
                if _oid_is_open(open_oids, stop_loss_oid):
                    if force_sl_replace and size > 0:
                        out["cancel_stop_loss_succeeded"] = False
                        out["cancel_stop_loss_error"] = (
                            f"the forced replace was refused before the cancel: {out['stop_loss_error']}"
                        )
                    else:
                        out["stop_loss_oid"] = int(stop_loss_oid)
            elif _oid_is_open(open_oids, stop_loss_oid) and not force_sl_replace:
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
                    if not cancel_ok:
                        out["cancel_stop_loss_error"] = out["stop_loss_error"]
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

            size = adapter.floor_size(symbol, size)
            if size <= 0:
                out["tp_size_skipped"] = [True] * len(tiers)
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
                tp_size_skipped = [False] * len(tiers)
                tp_outcome_unknown = [False] * len(tiers)
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

                def _place_tp(idx, tier_size, rounded_px):
                    pre_oids = _snapshot_open_oids(adapter, symbol)

                    def _resolve_unknown_tp(reason):
                        kind, oid = _resolve_placement_by_book_diff(
                            adapter, symbol, pre_oids, label=f"TP{idx + 1}"
                        )
                        if kind == "resting":
                            tp_oids_out[idx] = oid
                            return
                        tp_errors[idx] = reason
                        if kind == "unknown":
                            tp_outcome_unknown[idx] = True

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
                            _resolve_unknown_tp(
                                f"place_take_profit_limit returned no usable status: {resp}"
                            )
                    except Exception as te:
                        _resolve_unknown_tp(str(te))

                for idx, ((atr_mult, _cumulative_fraction), tier_size) in enumerate(
                    zip(tiers, tier_sizes)
                ):
                    raw_px = avg_cost + atr_mult * entry_atr if side == "long" else avg_cost - atr_mult * entry_atr
                    rounded_px = adapter.round_perps_trigger_px(symbol, raw_px)
                    tp_pxs.append(rounded_px)
                    prev_oid = int(existing_tp_oids[idx]) if idx < len(existing_tp_oids) else 0
                    tier_armed = armed[idx] if idx < len(armed) else False

                    if tier_size <= 0:
                        tp_size_skipped[idx] = True
                        continue
                    if _oid_is_open(open_oids, prev_oid) and not (idx < len(force_tp) and force_tp[idx]):
                        tp_oids_out[idx] = prev_oid
                        continue
                    if _oid_is_open(open_oids, prev_oid) and idx < len(force_tp) and force_tp[idx]:
                        try:
                            kind, payload = _classify_cancel_response(
                                adapter.cancel_order_by_oid(symbol, int(prev_oid)))
                            if kind != "ok":
                                tp_errors[idx] = f"force replace cancel rejected: {payload}"
                                continue
                        except Exception as ce:
                            tp_errors[idx] = f"force replace cancel: {ce}"
                            continue
                        _place_tp(idx, tier_size, rounded_px)
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
                        _place_tp(idx, tier_size, rounded_px)

                out["tp_oids"] = tp_oids_out
                out["tp_pxs"] = tp_pxs
                if any(tp_errors):
                    out["tp_errors"] = tp_errors
                if any(tp_filled_externally):
                    out["tp_filled_externally"] = tp_filled_externally
                    out["tp_fills"] = tp_fills
                if any(tp_filled_immediately):
                    out["tp_filled_immediately"] = tp_filled_immediately
                if any(tp_size_skipped):
                    out["tp_size_skipped"] = tp_size_skipped
                if any(tp_outcome_unknown):
                    out["tp_outcome_unknown"] = tp_outcome_unknown

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


EXECUTE_CLOSE_MODES = ("reduce_only", "cross")


def execute_close_mode_error(close_mode, close_full_position, size, stop_loss_pct, prev_pos_qty, margin_mode):
    if not close_mode:
        return ""
    if close_mode not in EXECUTE_CLOSE_MODES:
        return f"invalid --close-mode {close_mode!r}, expected one of {', '.join(EXECUTE_CLOSE_MODES)}"
    if close_full_position:
        return "--close-mode cannot be combined with --close-full-position"
    try:
        size_ok = float(size) > 0 and math.isfinite(float(size))
    except (TypeError, ValueError):
        size_ok = False
    if not size_ok:
        return "--close-mode requires --size > 0"
    if float(stop_loss_pct or 0) > 0:
        return "--close-mode cannot place a stop-loss (--stop-loss-pct must be 0)"
    if float(prev_pos_qty or 0) > 0:
        return "--close-mode cannot be combined with --prev-pos-qty (flip orders are not closes)"
    if margin_mode:
        return "--close-mode cannot be combined with --margin-mode (margin is set on opens only)"
    return ""


def run_execute(symbol, side, size, mode, stop_loss_pct=0.0, cancel_oid=0, prev_pos_qty=0.0, margin_mode="", leverage=0, close_full_position=False, account_leverage=0, account_margin_mode="", close_mode=""):
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live", "order_outcome": "not_sent"}, cls=SafeEncoder))
        sys.exit(1)
    close_mode_err = execute_close_mode_error(close_mode, close_full_position, size, stop_loss_pct, prev_pos_qty, margin_mode)
    if close_mode_err:
        print(json.dumps({
            "execution": None,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": close_mode_err,
            "order_outcome": "not_sent",
        }, cls=SafeEncoder))
        sys.exit(1)

    cancel_err = ""
    cancel_oids = cancel_oid if isinstance(cancel_oid, list) else [cancel_oid]
    cancel_oids = [int(oid) for oid in cancel_oids if int(oid or 0) > 0]
    cancel_attempted = len(cancel_oids) > 0
    cancel_succeeded = False
    cancel_succeeded_oids = []
    cancel_failed_oids = []

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
                    "order_outcome": "not_sent",
                }, cls=SafeEncoder))
                sys.exit(1)
            if leverage < 1:
                print(json.dumps({
                    "execution": None,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"--margin-mode requires --leverage >= 1, got {leverage}",
                    "order_outcome": "not_sent",
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
                        "order_outcome": "not_sent",
                    }, cls=SafeEncoder))
                    sys.exit(1)

        if close_mode and adapter.floor_size(symbol, size) <= 0:
            print(json.dumps({
                "execution": None,
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"sized close {size} for {symbol} floors to zero lots; no order sent and no protection cancelled",
                "order_outcome": "not_sent",
            }, cls=SafeEncoder))
            sys.exit(1)

        sized_close_px = 0.0
        if close_mode:
            try:
                sized_close_px = adapter.sized_close_price(symbol, is_buy)
            except Exception as pe:
                print(json.dumps({
                    "execution": None,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"sized close {symbol} has no usable mid price ({pe}); no order sent and no protection cancelled",
                    "order_outcome": "not_sent",
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
                            cancel_succeeded_oids.append(oid)
                        else:
                            cancel_errors.append(f"{oid}: {payload}")
                            cancel_failed_oids.append(oid)
                            print(f"[WARN] cancel_trigger_order({symbol}, {oid}) rejected: {payload}", file=sys.stderr)
                    except Exception as ce:
                        cancel_errors.append(f"{oid}: {ce}")
                        cancel_failed_oids.append(oid)
                        print(f"[WARN] cancel_trigger_order({symbol}, {oid}) failed: {ce}", file=sys.stderr)
            finally:
                if cancel_errors:
                    cancel_err = "; ".join(cancel_errors)

        fills_since_ms = int(time.time() * 1000) - 10_000

        if close_full_position:
            result = adapter.market_close(symbol, sz=None)
        elif close_mode:
            result = adapter.market_close_sized(symbol, is_buy, size, sized_close_px, reduce_only=(close_mode == "reduce_only"))
        else:
            result = adapter.market_open(symbol, is_buy, size)

        fill, fill_error, order_outcome = _extract_execute_fill(result)
        if fill_error:
            err_payload = {
                "execution": None,
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": fill_error,
                "order_outcome": order_outcome,
            }
            _add_execute_cancel_metadata(
                err_payload,
                cancel_err,
                cancel_succeeded,
                cancel_succeeded_oids,
                cancel_failed_oids,
            )
            print(json.dumps(err_payload, cls=SafeEncoder))
            sys.exit(1)

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
            "order_outcome": "filled",
        }
        _add_execute_cancel_metadata(
            out,
            cancel_err,
            cancel_succeeded,
            cancel_succeeded_oids,
            cancel_failed_oids,
        )
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
            "order_outcome": "unknown",
        }
        _add_execute_cancel_metadata(
            err_payload,
            cancel_err,
            cancel_succeeded,
            cancel_succeeded_oids,
            cancel_failed_oids,
        )
        print(json.dumps(err_payload, cls=SafeEncoder))
        sys.exit(1)


def run_list_open_order_oids(symbol=None):
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        listed = []
        decimals = {}
        for order in adapter.frontend_open_orders(symbol or None):
            try:
                oid = int(order.get("oid") or 0)
            except (TypeError, ValueError):
                oid = 0
            if not oid:
                continue
            try:
                sz = float(order.get("sz") or 0)
            except (TypeError, ValueError):
                sz = 0.0
            try:
                trigger_px = float(order.get("triggerPx") or 0)
            except (TypeError, ValueError):
                trigger_px = 0.0
            coin = str(order.get("coin") or symbol or "")
            if coin and coin not in decimals:
                lot = adapter.lot_size_decimals(coin)
                if isinstance(lot, int):
                    decimals[coin] = lot
            listed.append({
                "oid": oid,
                "coin": coin,
                "side": str(order.get("side") or ""),
                "sz": sz,
                "reduce_only": bool(order.get("reduceOnly")),
                "is_trigger": bool(order.get("isTrigger")),
                "order_type": str(order.get("orderType") or order.get("origType") or ""),
                "trigger_px": trigger_px,
            })
        payload = {
            "platform": "hyperliquid",
            "open_orders": listed,
            "sz_decimals_by_coin": decimals,
        }
        if symbol and symbol in decimals:
            payload["sz_decimals"] = decimals[symbol]
        print(json.dumps(payload, cls=SafeEncoder))
    except Exception as e:
        print(json.dumps({
            "platform": "hyperliquid",
            "open_order_check_error": str(e),
        }, cls=SafeEncoder))


def _run_cancel_only_stop_loss(adapter, symbol, cancel_oid):
    out = {
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "cancel_only": True,
    }
    if cancel_oid <= 0:
        out["error"] = "--size=0 is the cancel-only mode and needs --cancel-stop-loss-oid"
        print(json.dumps(out, cls=SafeEncoder))
        sys.exit(1)
    try:
        open_oids = adapter.open_order_oids(symbol)
    except Exception as oe:
        out["open_order_check_error"] = str(oe)
        out["error"] = f"open orders unreadable, stop-loss OID={cancel_oid} not verified: {oe}"
        print(f"[WARN] open_order_oids({symbol}) failed: {oe}; stop-loss OID={cancel_oid} not verified", file=sys.stderr)
        print(json.dumps(out, cls=SafeEncoder))
        sys.exit(1)
    if _oid_is_open(open_oids, cancel_oid):
        cancel_err = ""
        try:
            kind, payload = _classify_cancel_response(adapter.cancel_trigger_order(symbol, cancel_oid))
            if kind != "ok":
                cancel_err = payload
        except Exception as ce:
            cancel_err = str(ce)
        if cancel_err:
            out["cancel_stop_loss_error"] = cancel_err
            out["error"] = f"cancel of stop-loss OID={cancel_oid} failed: {cancel_err}"
            print(f"[WARN] cancel_trigger_order({symbol}, {cancel_oid}) failed: {cancel_err}", file=sys.stderr)
            print(json.dumps(out, cls=SafeEncoder))
            sys.exit(1)
        out["cancel_stop_loss_succeeded"] = True
        print(json.dumps(out, cls=SafeEncoder))
        return
    since_ms = int(time.time() * 1000) - 7 * 24 * 3600 * 1000
    fill = _oid_filled_externally(adapter, cancel_oid, since_ms, None)
    if fill.get("filled"):
        out["stop_loss_filled_externally"] = True
        print(f"[WARN] stop-loss OID={cancel_oid} already filled on-chain; reconciler will book the close", file=sys.stderr)
    else:
        out["stop_loss_not_open"] = True
    print(json.dumps(out, cls=SafeEncoder))


def _resolve_modify_on_book(adapter, symbol, pre_oids, is_buy, size, trigger_px, cancel_oid=0):
    try:
        orders = adapter.frontend_open_orders(symbol)
    except Exception as oe:
        print(f"[WARN] frontend_open_orders({symbol}) failed after modify: {oe}", file=sys.stderr)
        return "unknown", None
    want_side = "B" if is_buy else "A"
    for order in orders or []:
        if not isinstance(order, dict):
            continue
        try:
            oid = int(order.get("oid") or 0)
            sz = float(order.get("sz") or 0)
            trigger = float(order.get("triggerPx") or 0)
        except (TypeError, ValueError):
            continue
        if oid <= 0 or not order.get("reduceOnly") or not order.get("isTrigger"):
            continue
        kind = str(order.get("orderType") or order.get("origType") or "").lower()
        if "stop" not in kind or "take" in kind or str(order.get("side") or "") != want_side:
            continue
        if abs(sz - size) > 1e-6 and size > 0 and abs(sz - size) / size > 1e-4:
            continue
        # Both sides are rounded to 5 significant figures, so a resting stop at
        # any other tick differs by far more than this; a looser match can adopt
        # the unchanged old stop (or a peer's stop) as the moved one.
        if trigger_px <= 0 or abs(trigger - trigger_px) > trigger_px * 1e-6:
            continue
        if pre_oids is not None and oid in pre_oids:
            if oid == cancel_oid:
                # The same OID at the new trigger means the modify landed in place.
                return "resting", oid
            # Any other pre-existing order proves nothing about the modify.
            continue
        return "resting", oid
    return "unknown", None


def _placed_stop_size(adapter, symbol, size):
    try:
        return float(adapter.floor_size(symbol, size))
    except Exception:
        return 0.0


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

        if size <= 0:
            _run_cancel_only_stop_loss(adapter, symbol, cancel_oid)
            return

        open_oids = None
        if cancel_attempted:
            try:
                open_oids = adapter.open_order_oids(symbol)
            except Exception as oe:
                open_order_check_error = str(oe)
                print(f"[WARN] open_order_oids({symbol}) failed: {oe}; deferring trailing SL replacement", file=sys.stderr)

        fill_check_since_ms = int(time.time() * 1000) - 7 * 24 * 3600 * 1000
        should_place = True
        old_is_open = False
        if cancel_attempted:
            if open_oids is None:
                should_place = False
            elif _oid_is_open(open_oids, cancel_oid):
                # Change the resting stop in place so a second full-size stop is never
                # added beside it.
                old_is_open = True
            else:
                fill = _oid_filled_externally(adapter, cancel_oid, fill_check_since_ms, None)
                if fill.get("filled"):
                    sl_filled_externally = True
                    should_place = False
                    print(f"[WARN] stop-loss OID={cancel_oid} already filled on-chain; not re-placing — reconciler will book the close", file=sys.stderr)

        sl_is_buy = side == "short"
        place_unknown = False
        pre_oids = None
        trigger_px = adapter.round_perps_trigger_px(symbol, trigger_px)
        modified_in_place = False
        if old_is_open and should_place:
            try:
                pre_oids = {int(order.get("oid") or 0) for order in adapter.frontend_open_orders(symbol)}
                pre_oids.discard(0)
            except Exception as oe:
                open_order_check_error = str(oe)
                sl_err = f"open orders unreadable before modify: {oe}"
                print(f"[WARN] {sl_err}", file=sys.stderr)
                should_place = False
            else:
                try:
                    sl_resp = adapter.modify_stop_loss(symbol, cancel_oid, size, trigger_px, sl_is_buy)
                    if isinstance(sl_resp, dict) and str(sl_resp.get("status")) == "err":
                        sl_err = f"modify_stop_loss SDK error: {sl_resp.get('response')}"
                        print(f"[WARN] {sl_err}", file=sys.stderr)
                    else:
                        kind, payload = _classify_sl_response(sl_resp)
                        if kind == "resting":
                            resting_oid = payload or cancel_oid
                            modified_in_place = True
                        elif kind == "filled":
                            sl_filled_immediately = True
                            modified_in_place = True
                            print(f"[WARN] stop-loss filled immediately at submit (price already through {trigger_px})", file=sys.stderr)
                        elif kind == "error":
                            sl_err = f"modify_stop_loss SDK error: {payload}"
                            print(f"[WARN] {sl_err}", file=sys.stderr)
                        else:
                            resolved, oid = _resolve_modify_on_book(adapter, symbol, pre_oids, sl_is_buy, size, trigger_px, cancel_oid)
                            if resolved == "resting":
                                resting_oid = oid
                                modified_in_place = True
                                sl_err = ""
                            else:
                                sl_err = f"modify_stop_loss returned no usable status: {sl_resp}"
                                place_unknown = True
                                print(f"[WARN] {sl_err}", file=sys.stderr)
                except ValueError as ve:
                    sl_err = str(ve)
                    print(f"[WARN] modify_stop_loss rejected before send: {ve}", file=sys.stderr)
                except Exception as se:
                    sl_err = str(se)
                    print(f"[WARN] modify_stop_loss({symbol}, {cancel_oid}) failed: {se}", file=sys.stderr)
                    resolved, oid = _resolve_modify_on_book(adapter, symbol, pre_oids, sl_is_buy, size, trigger_px, cancel_oid)
                    if resolved == "resting":
                        resting_oid = oid
                        modified_in_place = True
                        sl_err = ""
                        place_unknown = False
                    else:
                        place_unknown = True
                should_place = False
        if should_place:
            pre_oids = set(int(o) for o in open_oids) if open_oids is not None else _snapshot_open_oids(adapter, symbol)
            try:
                sl_resp = adapter.place_stop_loss(symbol, size, trigger_px, sl_is_buy)
                if isinstance(sl_resp, dict) and str(sl_resp.get("status")) == "err":
                    sl_err = f"place_stop_loss SDK error: {sl_resp.get('response')}"
                    print(f"[WARN] {sl_err}", file=sys.stderr)
                    kind, payload = ("error", sl_err)
                else:
                    kind, payload = _classify_sl_response(sl_resp)
                if kind == "resting":
                    resting_oid = payload
                elif kind == "filled":
                    sl_filled_immediately = True
                    print(f"[WARN] stop-loss filled immediately at submit (price already through {trigger_px})", file=sys.stderr)
                elif kind == "error":
                    if not sl_err:
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
            placed_sz = _placed_stop_size(adapter, symbol, size)
            if placed_sz > 0:
                out["stop_loss_size"] = placed_sz
        if sl_filled_externally:
            out["stop_loss_filled_externally"] = True
        if place_unknown:
            out["stop_loss_outcome_unknown"] = True
        if old_is_open and not cancel_succeeded:
            out["stop_loss_old_still_open"] = True
        if place_unknown and pre_oids is not None:
            out["pre_place_open_oids"] = sorted(int(o) for o in pre_oids)
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
        parser.add_argument("--market-stdin", action="store_true", default=False,
            help="#1524: the stdin envelope carries a sealed market payload; never fetch candles here.")
        parser.add_argument("--decision-regime-timeframe", default="",
            help="#1712: regime timeframe for closed-bar decision slots; defaults to --timeframe.")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#1442): validate argv shape and exit 0 before reading stdin.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        symbol = args.symbol
        timeframe = args.timeframe
        market = None
        try:
            slots, market = parse_batch_request(sys.stdin.read())
            if args.market_stdin and market is None:
                raise ValueError("--market-stdin was set but the envelope carries no 'market' object")
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
            market=market,
            decision_regime_timeframe=args.decision_regime_timeframe,
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
        parser.add_argument("--stop-loss-trigger-px", type=float, default=None)
        parser.add_argument("--preserve-moved-stop", action="store_true")
        parser.add_argument("--mode", default="live")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe: validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
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
            stop_loss_trigger_px=args.stop_loss_trigger_px,
            preserve_moved_stop=bool(args.preserve_moved_stop),
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
                            help="the resting stop OID to modify in place; a fresh stop is placed only when it is already gone")
        args = parser.parse_args()
        run_update_stop_loss(args.symbol, args.side, args.size, args.trigger_px, args.mode,
                             cancel_oid=args.cancel_stop_loss_oid)
    elif "--list-open-order-oids" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--list-open-order-oids", action="store_true")
        parser.add_argument("--symbol", default="")
        parser.add_argument("--probe-only", action="store_true")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_list_open_order_oids(args.symbol or None)
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
        parser.add_argument("--close-mode", default="", choices=["", "reduce_only", "cross"],
                            help="sized close lane set by Go for a close only: reduce_only sends a reduce-only IOC, cross sends the netted IOC; both floor the size to the lot (#1577)")
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
                    account_margin_mode=args.account_margin_mode,
                    close_mode=args.close_mode)
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
        parser.add_argument("--position-risk-anchor-price", type=float, default=None)
        parser.add_argument("--mark-price", type=float, default=0.0,
            help="Optional mid from Go's fetchHyperliquidMids cycle; when >0 skips adapter.get_spot_price's duplicate /info allMids call (#768).")
        parser.add_argument("--market-stdin", action="store_true", default=False,
            help="#1524: read the sealed market payload from stdin; never fetch candles, higher-timeframe frames or funding here.")
        parser.add_argument("--allow-no-edge", nargs="?", const=True, default=None,
            help="#1681: operator acknowledgement for a no_edge open/close-fallback reference outside explicit paper mode.")
        parser.add_argument("--closed-bar-decisions", action="store_true", default=False,
            help="#1712: decide signals and entry ATR on the last closed bar; protection keeps current inputs.")
        parser.add_argument("--decision-regime-timeframe", default="",
            help="#1712: regime timeframe for the closed-bar decision view; defaults to the strategy timeframe.")
        parser.add_argument("--resting-tp-rule-json", default=None,
            help="#1727: paper-only resting take-profit trade-through rule request from Go; live mode refuses it.")
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe (#645): validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        market = None
        if args.market_stdin:
            try:
                market = parse_market_stdin(sys.stdin.read())
            except Exception as e:
                traceback.print_exc(file=sys.stderr)
                print(json.dumps({
                    "strategy": args.strategy,
                    "symbol": args.symbol,
                    "timeframe": args.timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "regime": None,
                    "mode": args.mode,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"invalid market payload: {e}",
                }, cls=SafeEncoder))
                sys.exit(1)
        from strategy_composition import parse_strategy_refs_arg
        refs = parse_strategy_refs_arg(args.strategy_refs)
        open_strategy_name = refs["open_name"] if refs else args.open_strategy
        close_strategies_arg = refs["close_csv"] if refs else args.close_strategies
        params_override = refs["open_params"] if refs else (json.loads(args.params) if args.params else None)
        close_params_by_name = refs["close_params_by_name"] if refs else None
        position_ctx = _position_ctx_from_args(args)
        regime_windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        resting_rule = None
        if args.resting_tp_rule_json is not None:
            try:
                resting_rule = json.loads(args.resting_tp_rule_json)
                if not isinstance(resting_rule, dict):
                    raise ValueError("--resting-tp-rule-json must be a JSON object")
            except ValueError as e:
                print(json.dumps({
                    "strategy": args.strategy,
                    "symbol": args.symbol,
                    "timeframe": args.timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "regime": None,
                    "mode": args.mode,
                    "platform": "hyperliquid",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"invalid --resting-tp-rule-json: {e}",
                }, cls=SafeEncoder))
                sys.exit(1)
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
            market=market,
            close_owner=refs["close_owner"] if refs else None,
            invert_open_signal=refs.get("invert_open_signal") if refs and "invert_open_signal" in refs else None,
            mode_args=sys.argv[1:],
            closed_bar=args.closed_bar_decisions,
            decision_regime_timeframe=args.decision_regime_timeframe,
            resting_tp_rule=resting_rule,
        )


if __name__ == "__main__":
    main()
