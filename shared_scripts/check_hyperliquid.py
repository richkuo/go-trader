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
"""

import sys
import os
import json
import math
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
from regime import latest_regime


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
    return ctx


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False,
                     strategy_params_override=None, open_strategy=None,
                     close_strategies=None,
                     position_side="", position_ctx=None):
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
        candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=200)

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
        regime_payload = latest_regime(df)
        strategy_params["regime"] = regime_payload
        if strategy_params_override:
            merged = {**strategy_params_override, **strategy_params}
            strategy_params = merged
        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            atr_now = latest_atr(df)
            if atr_now > 0:
                market_ctx["atr"] = atr_now
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

        # Freshen price with live mid if available
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
            "regime": regime_payload["regime"],
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


def run_execute(symbol, side, size, mode, stop_loss_pct=0.0, cancel_oid=0, prev_pos_qty=0.0, margin_mode="", leverage=0):
    """Place a live market order on Hyperliquid, optionally wrapping it with
    a stop-loss trigger (open) or cancelling a stale SL trigger (close).

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
    cancel_attempted = cancel_oid > 0
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
            try:
                adapter.cancel_trigger_order(symbol, cancel_oid)
                cancel_succeeded = True
            except Exception as ce:
                cancel_err = str(ce)
                print(f"[WARN] cancel_trigger_order({symbol}, {cancel_oid}) failed: {ce}", file=sys.stderr)

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
                # Extract fee if present in response
                fee = filled.get("fee")
                if fee is not None:
                    fill["fee"] = float(fee)
        except Exception:
            pass

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


def main():
    if "--update-stop-loss" in sys.argv:
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
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--stop-loss-pct", type=float, default=0.0,
                            help="place a reduce-only SL trigger this pct away from fill (#412)")
        parser.add_argument("--cancel-stop-loss-oid", type=int, default=0,
                            help="cancel this trigger OID before placing the new order (#412)")
        parser.add_argument("--prev-pos-qty", type=float, default=0.0,
                            help="abs qty of existing position being flipped, so SL is sized against the new net position (#421)")
        parser.add_argument("--margin-mode", default="",
                            help="enforce 'isolated' or 'cross' margin via update_leverage before the order; only safe on a fresh open from flat (#486)")
        parser.add_argument("--leverage", type=float, default=0.0,
                            help="leverage to set alongside --margin-mode (HL update_leverage takes both in one call) (#486)")
        args = parser.parse_args()
        run_execute(args.symbol, args.side, args.size, args.mode,
                    stop_loss_pct=args.stop_loss_pct, cancel_oid=args.cancel_stop_loss_oid,
                    prev_pos_qty=args.prev_pos_qty,
                    margin_mode=args.margin_mode, leverage=args.leverage)
    else:
        # Signal check mode: <strategy> <symbol> <timeframe> [--mode=paper|live] [--htf-filter]
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        parser.add_argument("--htf-filter", action="store_true", default=False)
        parser.add_argument("--params", default=None)
        parser.add_argument("--open-strategy", default=None)
        parser.add_argument("--close-strategies", default=None)
        parser.add_argument("--position-side", default="")
        parser.add_argument("--position-avg-cost", type=float, default=None)
        parser.add_argument("--position-qty", type=float, default=None)
        parser.add_argument("--position-initial-qty", type=float, default=None)
        parser.add_argument("--position-entry-atr", type=float, default=None)
        args = parser.parse_args()
        params_override = json.loads(args.params) if args.params else None
        position_ctx = _position_ctx_from_args(args)
        run_signal_check(
            args.strategy, args.symbol, args.timeframe, args.mode,
            args.htf_filter, params_override, args.open_strategy,
            args.close_strategies,
            args.position_side, position_ctx,
        )


if __name__ == "__main__":
    main()
