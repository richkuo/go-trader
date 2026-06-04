#!/usr/bin/env python3
"""
Hyperliquid perps strategy check script.
Fetches OHLCV from Hyperliquid, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_hyperliquid.py <strategy> <symbol> <timeframe> [--mode=paper|live]

Execution mode (live only, called by Go as phase 2):
    check_hyperliquid.py --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live]
        [--stop-loss-pct=3.0]         # optional: place a reduce-only SL trigger after fill (#412)
        [--cancel-stop-loss-oid=OID]  # optional: cancel this trigger OID before the order
        [--prev-pos-qty=0.5]          # optional: existing position qty being flipped, so the SL
                                      # is sized against the *new* net position (total_sz - prev) (#421)
        [--margin-mode=isolated|cross] # optional: enforce margin mode via update_leverage before the
        [--leverage=N]                #   order (only on a fresh open from flat — HL rejects mode
                                      #   changes on an open position) (#486)

Trailing stop update mode (live only):
    check_hyperliquid.py --update-stop-loss --symbol=BTC --side=long|short --size=0.01 \
        --trigger-px=62000 --cancel-stop-loss-oid=OID [--mode=live]

Fetch ATR mode (read-only, used by manual-open when --atr is omitted):
    check_hyperliquid.py --fetch-atr --symbol=BTC --timeframe=1h [--period=14]
"""

import sys
import os
import json
import math
import time
import traceback
from datetime import datetime, timezone


class SafeEncoder(json.JSONEncoder):
    """JSON encoder that converts NaN/Inf to null (Python None)."""

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

# Add paths: platforms/hyperliquid/ directly (avoids naming conflict with hyperliquid SDK),
# shared_strategies/open/futures/ for apply_strategy (Hyperliquid is a perps exchange), shared_tools/ for utilities.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'hyperliquid'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'futures'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from hl_user_fills import apply_user_fills_lookup
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime


def _make_dataframe(candles):
    """Convert raw OHLCV list to pandas DataFrame compatible with strategy functions."""
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


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params_override=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None,
                     regime_enabled=False, regime_windows_spec=None, ohlcv_limit=200, regime_atr_window="",
                     close_params_by_name=None,
                     mark_price=0.0):
    """Run strategy signal check using Hyperliquid OHLCV data."""
    try:
        from adapter import HyperliquidExchangeAdapter
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
            validate_close_strategy_names,
        )

        open_close_enabled = bool(open_strategy or close_strategies)
        configured_names = [open_strategy or strategy_name]
        for name in configured_names:
            get_strategy(name)
        validate_close_strategy_names(
            parse_close_strategies(close_strategies),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        adapter = HyperliquidExchangeAdapter()

        # Fetch funding rate data for delta-neutral strategy
        strategy_params = {}
        if strategy_name == "delta_neutral_funding":
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

        print(f"Fetching {symbol} {timeframe} from Hyperliquid ({mode})...", file=sys.stderr)
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
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(candles) if candles else 0} candles",
            }, cls=SafeEncoder))
            sys.exit(1)

        df = _make_dataframe(candles)
        stdout_regime, live_regime, strategy_regime = prepare_check_regime(
            df,
            regime_enabled=regime_enabled,
            windows_spec=regime_windows_spec,
            atr_window=regime_atr_window,
        )
        strategy_params["regime"] = strategy_regime
        if strategy_params_override:
            merged = {**strategy_params_override, **strategy_params}
            strategy_params = merged
        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            atr_now = latest_atr(df)
            if atr_now > 0:
                market_ctx["atr"] = atr_now
            # #733: live regime label for tiered_tp_atr_live_regime evaluator.
            # Falls back to the position's frozen regime via the evaluator if
            # this is empty (e.g. regime detection disabled mid-position).
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
                strategy_params or None,
                position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
                close_params_by_name=close_params_by_name,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        else:
            result_df = apply_strategy(strategy_name, df, strategy_params or None)
            signal = normalize_signal(result_df.iloc[-1].get("signal", 0))

        ensure_atr_indicator(result_df)
        last = result_df.iloc[-1]
        price = float(last["close"])

        # Apply HTF trend filter if enabled (skip for funding-rate strategies — #103)
        htf_info = {}
        htf_strategy_name = open_strategy or strategy_name
        if htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter

            def _fetch_htf(sym, tf, limit):
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

        # Freshen price with live mid if available. Go fetches /info allMids
        # once per cycle (fetchHyperliquidMids) and forwards the mid via
        # --mark-price so this subprocess can skip its own /info call (#768
        # fix #3). Zero staleness risk: same source, seconds old, used only
        # to freshen the display price in the output JSON. Fall back to
        # adapter.get_spot_price when the flag is absent.
        if mark_price and mark_price > 0:
            price = mark_price
        else:
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
            "regime": stdout_regime,
            "mode": mode,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if decision:
            output.update(decision)
        print(json.dumps(output, cls=SafeEncoder))

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


def _classify_sl_response(sdk_response: dict):
    """Classify a trigger-order SDK response into one of:

      ("resting", oid)        — order is now resting on the book (happy path)
      ("filled",  oid_or_0)   — order filled at submit (price was already through the trigger)
      ("error",   reason_str) — SDK reported an error in the status payload
      ("missing", None)       — couldn't find a status entry (malformed response)

    HL responses look like:
      {"status":"ok","response":{"type":"order","data":{"statuses":[ <status> ]}}}

    where <status> is one of `{"resting":{"oid":N}}`, `{"filled":{...,"oid":N}}`,
    or `{"error":"..."}`. Distinguishing these matters because an instant-fill
    means the position is already closed on-chain — surfacing it as "no resting
    OID" the way the previous _extract_resting_oid did made the scheduler log
    a placement error and leave virtual state thinking the position is open. (#421)
    """
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


def _oid_is_open(open_oids: set[int] | None, oid: int) -> bool:
    return oid > 0 and open_oids is not None and int(oid) in open_oids


def _oid_filled_externally(adapter, oid: int, since_ms: int, fill_hints=None) -> dict:
    """Check whether ``oid`` has filled on-chain by querying userFills.

    When ``fill_hints`` is provided (oid → hint dict from the Go reconciler's
    same-cycle prefetch, #759), only a **confirmed fill** (``filled: true``)
    short-circuits ``lookup_fill_fee_by_oid``. A ``filled: false`` hint does
    not — Go's prefetch can miss on transient indexer errors, so Python keeps
    an independent userFills attempt with its own retry budget.

    Returns a dict with at minimum ``{"filled": bool}``. When filled, also
    includes ``size`` (summed across partial fills) and the ``fee`` /
    ``closed_pnl`` fields surfaced by ``lookup_fill_fee_by_oid``. Failure to
    query is non-fatal: the caller treats {"filled": False} as "we don't
    know" and proceeds with re-placement only when we have positive evidence
    the order was cancelled (open-orders fetch succeeded and OID absent).

    Used by run_sync_protection to avoid the over-close hazard where a TP
    OID that has actually filled (shrinking the on-chain position) is
    re-placed at the same price sized against stale virtual qty (#604 review #1).
    """
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
    """Return canonical cumulative TP tiers as (atr_multiple, close_fraction)."""
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

    # Match Go: the last on-chain TP order always covers everything remaining,
    # preserving the old TP2 behavior for two-tier configs ending below 100%.
    tiers[-1] = (tiers[-1][0], 1.0)
    return tiers


def compute_tp_tier_sizes(size, tiers, floor_size_fn):
    """Compute per-tier reduce-only sizes that cover the full lot-aligned position.

    Non-final tiers are pre-floored so each on-chain order is lot-aligned;
    the final tier absorbs the remainder via integer-lot subtraction
    (`floor_size(size) - sum(non-final floors)`) so per-tier truncation
    cannot strand a permanent residual (#628).

    `tiers` is the normalized output of `_normalize_tp_tiers`: a list of
    (atr_multiple, cumulative_fraction) with the final fraction == 1.0.

    Returns a list of float sizes the same length as `tiers`. Returns all
    zeros when `size <= 0` or `tiers` is empty.
    """
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
    """Verify/re-place per-strategy reduce-only SL/TP orders (#601)."""
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
    # Shared-coin dust can drive size to 0 while surplus TP OIDs still need cancel (#843).
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

        # Wide window for the userFills "did this OID fill?" lookup. We don't
        # know how long the prior OID was outstanding, so look back 7 days —
        # any fill older than that is irrelevant (the OID would have been
        # rotated long since). Bounding at 7d keeps the indexer scan cheap
        # but still catches fills that occurred during a multi-day outage.
        fill_check_since_ms = int(time.time() * 1000) - 7 * 24 * 3600 * 1000

        def _resolve_missing_oid(prev_oid: int):
            """Decide what to do with a previously-recorded OID that is no
            longer in open_orders. Returns one of:
                ("place",   None)  — OID never existed or was cancelled; place new
                ("filled",  fill)  — OID actually filled on-chain; do NOT re-place
                ("unknown", None)  — open_orders fetch failed; defer
            (#604 review #1)
            """
            if prev_oid <= 0:
                return ("place", None)
            if open_oids is None:
                # We couldn't fetch open_orders — don't re-place a TP/SL
                # without knowing whether the prior one is still resting.
                # Re-placement here is what would over-close: better to
                # surface the failure and try again next cycle.
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
            out["stop_loss_trigger_px"] = sl_px
            if _oid_is_open(open_oids, stop_loss_oid) and not force_sl_replace:
                out["stop_loss_oid"] = int(stop_loss_oid)
            elif _oid_is_open(open_oids, stop_loss_oid) and force_sl_replace:
                if size <= 0:
                    out["stop_loss_oid"] = int(stop_loss_oid)
                else:
                    try:
                        adapter.cancel_order_by_oid(symbol, int(stop_loss_oid))
                    except Exception as ce:
                        out["stop_loss_error"] = f"force replace cancel: {ce}"
                    try:
                        resp = adapter.place_stop_loss(symbol, size, sl_px, close_is_buy)
                        kind, payload = _classify_sl_response(resp)
                        if kind == "resting":
                            out["stop_loss_oid"] = payload
                        elif kind == "filled":
                            out["stop_loss_filled_immediately"] = True
                        elif kind == "error":
                            out["stop_loss_error"] = f"place_stop_loss SDK error: {payload}"
                        else:
                            out["stop_loss_error"] = f"place_stop_loss returned no usable status: {resp}"
                    except Exception as se:
                        out["stop_loss_error"] = str(se)
            else:
                action, fill = _resolve_missing_oid(stop_loss_oid)
                if action == "filled":
                    out["stop_loss_filled_externally"] = True
                    out["stop_loss_fill"] = fill
                    print(f"[WARN] stop-loss OID={stop_loss_oid} already filled on-chain; not re-placing — reconciler will book the close", file=sys.stderr)
                elif action == "place" and size > 0:
                    try:
                        resp = adapter.place_stop_loss(symbol, size, sl_px, close_is_buy)
                        kind, payload = _classify_sl_response(resp)
                        if kind == "resting":
                            out["stop_loss_oid"] = payload
                        elif kind == "filled":
                            out["stop_loss_filled_immediately"] = True
                        elif kind == "error":
                            out["stop_loss_error"] = f"place_stop_loss SDK error: {payload}"
                        else:
                            out["stop_loss_error"] = f"place_stop_loss returned no usable status: {resp}"
                    except Exception as se:
                        out["stop_loss_error"] = str(se)
                # action=="unknown" → leave SL OID untouched, retry next cycle

        tiers = _normalize_tp_tiers(tp_tiers, tp1_atr_mult, tp1_fraction, tp2_atr_mult)
        if tiers:
            existing_tp_oids = list(tp_oids or [])
            if not existing_tp_oids and (tp1_oid > 0 or tp2_oid > 0):
                existing_tp_oids = [tp1_oid, tp2_oid]
            if len(existing_tp_oids) < len(tiers):
                existing_tp_oids.extend([0] * (len(tiers) - len(existing_tp_oids)))

            # Normalize to lot precision before computing tier sizes.  Go's
            # float64 arithmetic (pos.Quantity -= closeQty) can drift just below
            # a lot boundary (e.g. 0.011 - 0.010 = 0.000999...) even though the
            # true virtual qty is exactly one lot.  round() matches what
            # place_stop_loss already does for SL size.
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

                    # #749: OID 0 means "no resting order" both before first placement
                    # and after a tier filled (Go zeros the slot; TPArmedTiers marks
                    # the tier as armed). Only the latter must skip re-placement —
                    # otherwise cumulative fractions are re-applied to the reduced
                    # size and tier 1 comes back as a "new TP1".
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
                    # action=="unknown" → echo previous OID, retry next cycle

                out["tp_oids"] = tp_oids_out
                out["tp_pxs"] = tp_pxs
                if any(tp_errors):
                    out["tp_errors"] = tp_errors
                if any(tp_filled_externally):
                    out["tp_filled_externally"] = tp_filled_externally
                    out["tp_fills"] = tp_fills
                if any(tp_filled_immediately):
                    out["tp_filled_immediately"] = tp_filled_immediately

                # Legacy fields stay populated for older callers/tests during the
                # migration from fixed TP1/TP2 fields to the N-tier slice (#612).
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
    """Place a live market order on Hyperliquid, optionally wrapping it with
    a stop-loss trigger (open) or cancelling a stale SL trigger (close).

    When ``close_full_position`` is True the call uses ``adapter.market_close(sz=None)``
    instead of ``market_open``, which closes the entire on-chain residual without
    a sized order. This eliminates dust on final tiered-TP legs (#592).

    ``prev_pos_qty`` is the absolute quantity of any existing position being
    flipped through (e.g. long→short). On a flip, total_sz from the fill is
    closeQty + newQty, so the SL must be sized against ``total_sz - prev_pos_qty``
    to avoid placing an oversized reduce-only trigger that HL may reject (#421).
    For pure opens from flat (no flip), pass 0 — full total_sz is the new
    position size."""
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)

    # Track cancel state outside the main try/except so the scheduler still
    # learns whether the stale SL was freed even if the subsequent market_open
    # raises. Otherwise pos.StopLossOID points at a dead OID for another cycle
    # and the next signal tries to cancel a non-existent order. (#421)
    cancel_err = ""
    cancel_oids = cancel_oid if isinstance(cancel_oid, list) else [cancel_oid]
    cancel_oids = [int(oid) for oid in cancel_oids if int(oid or 0) > 0]
    cancel_attempted = len(cancel_oids) > 0
    cancel_succeeded = False

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        is_buy = side.lower() == "buy"

        # Enforce margin mode + leverage before placing the order (#486).
        # Fail closed: if HL rejects this we abort the order rather than
        # silently opening into the wrong margin mode. When a peer strategy
        # has already opened the same coin (#491), HL has the desired state
        # pinned and would reject a fresh update_leverage call — so skip the
        # call when get_position_leverage confirms the on-chain state already
        # matches. LoadConfig validates that all peers share margin_mode and
        # leverage, so a match here is the expected case.
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
            # Go pulls clearinghouseState once per cycle (fetchHyperliquidState)
            # and forwards the per-coin leverage + margin mode via
            # --account-leverage / --account-margin-mode (#768 fix #4). When
            # provided, skip get_position_leverage entirely — the snapshot is
            # the same /info endpoint Python would call. Zero staleness risk:
            # this subprocess runs in the same cycle Go produced the snapshot,
            # and update_leverage failures still trip the original fail-loud
            # safety path below.
            if account_leverage and account_margin_mode in ("isolated", "cross"):
                current = {"margin_mode": account_margin_mode, "leverage": int(account_leverage)}
            else:
                try:
                    current = adapter.get_position_leverage(symbol)
                except Exception as ce:
                    # Don't fail-closed on a state-fetch hiccup — the
                    # update_leverage call below will fail loudly if the on-chain
                    # state actually disagrees, preserving the original safety
                    # behavior. We still log so the cause is debuggable.
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

        # Cancel stale SL first: we want to free the trigger slot before
        # possibly spending another one on the new entry. A cancel failure is
        # non-fatal (SL may have already triggered on-chain, in which case the
        # position sync will detect the close on the next cycle) but is
        # surfaced in the JSON so the scheduler can log it.
        if cancel_attempted:
            cancel_errors = []
            try:
                for oid in cancel_oids:
                    try:
                        adapter.cancel_trigger_order(symbol, oid)
                        cancel_succeeded = True
                    except Exception as ce:
                        cancel_errors.append(f"{oid}: {ce}")
                        print(f"[WARN] cancel_trigger_order({symbol}, {oid}) failed: {ce}", file=sys.stderr)
            finally:
                if cancel_errors:
                    cancel_err = "; ".join(cancel_errors)

        # Bound the userFills lookup window to "shortly before submit" so the
        # post-fill query (#585) doesn't have to scan unrelated history.
        # 10s buffer absorbs local-vs-indexer clock skew.
        fills_since_ms = int(time.time() * 1000) - 10_000

        if close_full_position:
            # Final-tier TP close (#592): close the entire on-chain residual
            # without specifying a size so rounding drift never leaves dust.
            result = adapter.market_close(symbol, sz=None)
        else:
            result = adapter.market_open(symbol, is_buy, size)

        # Extract fill info from SDK response structure:
        # {"status": "ok", "response": {"type": "order", "data": {"statuses": [...]}}}
        fill = {}
        try:
            statuses = result.get("response", {}).get("data", {}).get("statuses", [])
            if statuses:
                filled = statuses[0].get("filled", {})
                fill = {
                    "avg_px": float(filled.get("avgPx", 0) or 0),
                    "total_sz": float(filled.get("totalSz", 0) or 0),
                }
                # Extract exchange order ID if present
                oid = filled.get("oid")
                if oid is not None:
                    fill["oid"] = int(oid)
                # Extract fee if present in response (HL placeOrder response
                # currently omits this — keep the read for forward compat).
                fee = filled.get("fee")
                if fee is not None:
                    fill["fee"] = float(fee)
        except Exception:
            pass

        # The HL placeOrder response does not include `fee`; the real fee is
        # only available via the userFills indexer endpoint (#585). Query it
        # by OID so partial fills across multiple price levels aggregate
        # correctly. Failures here fall back to the modeled fee on the Go
        # side — non-fatal.
        if fill.get("oid"):
            try:
                lookup = adapter.lookup_fill_fee_by_oid(fill["oid"], fills_since_ms)
                if not lookup:
                    print(f"[WARN] userFills lookup returned no fills for oid={fill['oid']}", file=sys.stderr)
                elif not apply_user_fills_lookup(fill, lookup):
                    print(f"[WARN] userFills lookup returned malformed fill data for oid={fill['oid']}", file=sys.stderr)
            except Exception as fe:
                print(f"[WARN] userFills lookup failed for oid={fill['oid']}: {fe}", file=sys.stderr)

        # Place the stop-loss trigger on successful opens only. We only try to
        # place an SL when the main order actually filled; a zero-size fill
        # usually means the order was rejected and there's nothing to protect.
        sl_err = ""
        sl_filled_immediately = False
        # Net new-position size: on a flip (long→short or vice versa) total_sz
        # is closeQty + newQty, but reduce-only triggers must be sized against
        # the resulting net position (#421).
        net_new_sz = max(fill.get("total_sz", 0) - max(prev_pos_qty, 0.0), 0.0)
        if stop_loss_pct > 0 and fill.get("avg_px", 0) > 0 and net_new_sz > 0:
            entry_px = fill["avg_px"]
            sl_size = net_new_sz
            # Stop-loss fires against the opposite direction of the open:
            # long open (is_buy=True)  → SL sells when price drops below entry*(1-pct).
            # short open (is_buy=False) → SL buys when price rises above entry*(1+pct).
            if is_buy:
                trigger_px = entry_px * (1.0 - stop_loss_pct / 100.0)
                sl_is_buy = False
            else:
                trigger_px = entry_px * (1.0 + stop_loss_pct / 100.0)
                sl_is_buy = True
            # Pre-round to HL's per-asset px tick so the recorded value matches
            # the price the order actually rests at — the scheduler books PnL
            # off this field on StopLossFilledImmediately (#421 review).
            trigger_px = adapter.round_perps_trigger_px(symbol, trigger_px)
            try:
                sl_resp = adapter.place_stop_loss(symbol, sl_size, trigger_px, sl_is_buy)
                kind, payload = _classify_sl_response(sl_resp)
                if kind == "resting":
                    fill["stop_loss_oid"] = payload
                    fill["stop_loss_trigger_px"] = trigger_px
                elif kind == "filled":
                    # Price was already through the trigger — the SL filled at
                    # submit time, so the position just got stopped out. No OID
                    # to track. Surface as a distinct field so the scheduler
                    # can reconcile virtual state instead of treating it as a
                    # placement error and leaving the position recorded as open.
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
        # Always surface cancel state on failure paths too so the scheduler
        # can clear pos.StopLossOID even when the subsequent open raises (#421).
        if cancel_err:
            err_payload["cancel_stop_loss_error"] = cancel_err
        if cancel_succeeded:
            err_payload["cancel_stop_loss_succeeded"] = True
        print(json.dumps(err_payload, cls=SafeEncoder))
        sys.exit(1)


def run_update_stop_loss(symbol, side, size, trigger_px, mode, cancel_oid=0):
    """Cancel the old resting SL trigger and place a replacement for an open
    position. ``side`` is the current position side, not the trigger order side.
    Margin mode / leverage flags are intentionally absent: HL rejects changes
    on an open position, and this mode only updates protection for an open leg.
    """
    if mode != "live":
        print(json.dumps({"error": "--update-stop-loss requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)

    cancel_err = ""
    cancel_attempted = cancel_oid > 0
    cancel_succeeded = False
    sl_err = ""
    sl_filled_immediately = False
    resting_oid = 0

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

        if cancel_attempted:
            try:
                adapter.cancel_trigger_order(symbol, cancel_oid)
                cancel_succeeded = True
            except Exception as ce:
                cancel_err = str(ce)
                print(f"[WARN] cancel_trigger_order({symbol}, {cancel_oid}) failed: {ce}", file=sys.stderr)

        sl_is_buy = side == "short"
        trigger_px = adapter.round_perps_trigger_px(symbol, trigger_px)
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
        except Exception as se:
            sl_err = str(se)
            print(f"[WARN] place_stop_loss({symbol}, {size}, {trigger_px}) failed: {se}", file=sys.stderr)

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
        if sl_err:
            out["stop_loss_error"] = sl_err
        if sl_filled_immediately:
            out["stop_loss_filled_immediately"] = True
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


def run_fetch_atr(symbol: str, timeframe: str, period: int):
    """Fetch OHLCV from Hyperliquid and emit latest ATR as JSON.

    Used by manual-open when --atr is omitted so manual positions get the
    same ATR baseline strategy opens compute via ensure_atr_indicator (#689).
    Emits {"atr": <float>, "candles": <int>} on success; {"error": "..."} on
    failure (still exits 0 so Go can parse the JSON and decide whether to
    fall back to computeFallbackATR).
    """
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
        atr = latest_atr(df, period=period)
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
    """Place a resting NON-reduce-only limit order to open a position (#883).

    Unlike --execute (a market order that must fill immediately) this places a
    maker limit order that rests until ``limit_px`` is reached, then exits with
    the resting OID. NO stop-loss / take-profit is armed here — there is no fill
    at placement, so the scheduler arms protection post-fill via the existing
    per-cycle manual protection sync (#883 design point 4).

    Margin mode / leverage are enforced before placement exactly like --execute,
    so the resting order carries the operator's intended leverage when it fills.
    """
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

        # Enforce margin mode + leverage from flat before resting the order
        # (mirrors run_execute). The position is flat at placement (limit hasn't
        # filled), so HL accepts the update. Reuse Go's clearinghouse snapshot
        # when forwarded to skip a duplicate /info call.
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
            # Gtc price was already marketable and filled at submit. The order
            # is not resting; the scheduler reconcile will detect the fill by
            # OID on its next poll exactly as it would for a delayed fill.
            out["order_oid"] = int(payload)
            out["status"] = "filled"
            print(f"[WARN] limit order filled immediately at submit (price already marketable)", file=sys.stderr)
        elif kind == "error":
            # Alo rejection of a marketable price lands here — surface so the
            # operator re-prices instead of silently degrading to a taker fill.
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
    """Report resting/fill status for one or more limit-order OIDs (#883).

    For each OID emit ``{oid, resting, filled_size, avg_px, fee, count}`` where
    ``resting`` reflects HL's open-orders book and the fill fields are the
    cumulative on-chain fills summed across partial legs. The scheduler combines
    these: ``resting=false`` + ``filled_size>=order_size`` ⇒ fully filled;
    ``resting=false`` + ``filled_size<order_size`` ⇒ cancelled/expired with a
    (possibly zero) partial fill; ``resting=true`` ⇒ still working, adopt any
    incremental fill and keep polling.

    open_orders fetch failure is reported as ``open_orders_error`` and
    ``resting`` is left null so the caller defers the cancelled/expired verdict
    (never books a phantom cancellation on a transient indexer error).
    """
    if mode != "live":
        print(json.dumps({"error": "--limit-status requires --mode=live"}, cls=SafeEncoder))
        sys.exit(1)
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        if since_ms <= 0:
            # Default lookback: 7 days. Resting orders can sit far longer than a
            # market fill's 10s window, so the userFills scan must reach back to
            # at least the order's placement.
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
    """Cancel a resting order by OID (#883). Idempotent: a "not found" cancel
    (order already filled or already cancelled) is reported as a non-fatal
    warning, not an error, so the scheduler's cancel+finalize path is safe to
    retry across cycles."""
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
            # Treat as non-fatal: the order may have already filled or been
            # cancelled. The caller re-polls fill status to reconcile truth.
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
    if "--fetch-atr" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--fetch-atr", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--timeframe", required=True)
        parser.add_argument("--period", type=int, default=14)
        parser.add_argument("--probe-only", action="store_true",
            help="Startup compatibility probe: validate argv shape and exit 0.")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)
        run_fetch_atr(args.symbol, args.timeframe, args.period)
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
        # Execute mode: --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live]
        # Or for final-tier TP closes: --execute --symbol=ETH --side=sell --close-full-position
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        # --size is required unless --close-full-position is set (#592)
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
        # Resting limit-order open: --limit-open --symbol=BTC --side=buy
        #   --size=0.01 --limit-price=58000 [--tif=Alo] [--mode=live] (#883)
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
        # Resting limit-order fill poll: --limit-status --symbol=BTC
        #   --oids-json='[123,456]' [--since-ms=N] [--mode=live] (#883)
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
        # Cancel a resting order by OID: --cancel-order --symbol=BTC --oid=123 (#883)
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
        # Signal check mode: <strategy> <symbol> <timeframe> [--mode=paper|live] [--htf-filter]
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
            close_params_by_name=close_params_by_name,
            mark_price=args.mark_price,
        )


if __name__ == "__main__":
    main()
