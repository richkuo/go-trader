#!/usr/bin/env python3

import sys
import os
import json
import math
import traceback
import importlib.util
from datetime import datetime, timezone

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _REPO_ROOT)
sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

import egress_policy
import option_data
from option_data import OptionDataUnavailable
from regime import latest_regime, regime_from_injected_payload, required_regime_bars
from stdout_contract import json_stdout

MAX_POSITIONS_PER_STRATEGY = 4
MIN_SCORE_THRESHOLD = 0.3
REGIME_TIMEFRAME = "4h"
REGIME_LIMIT = 100
REGIME_MIN_BARS = required_regime_bars(14)
SUPPORTED_PLATFORMS = ("deribit", "ibkr", "okx", "robinhood")
RANK_STRATEGIES = ("butterfly", "vol_mean_reversion")


class CheckError(Exception):

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _load_adapter(platform: str):
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        raise ImportError(f"No adapter found for platform '{platform}' at {adapter_path}")
    platform_dir = os.path.dirname(adapter_path)
    if platform_dir not in sys.path:
        sys.path.insert(0, platform_dir)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", adapter_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in dir(module):
        if name.endswith("ExchangeAdapter"):
            return getattr(module, name)()
    raise AttributeError(f"No ExchangeAdapter class found in {adapter_path}")


def _fetch_ohlcv_df(underlying, timeframe, limit, min_len, adapter=None, errors=None):
    rows = None
    if adapter is not None:
        ohlcv_fn = getattr(adapter, "get_ohlcv", None)
        if ohlcv_fn is not None:
            try:
                rows = ohlcv_fn(underlying, timeframe, limit)
            except Exception as e:
                print(f"adapter.get_ohlcv failed: {e}", file=sys.stderr)
                if errors is not None:
                    errors.append(e)
                rows = None
    if not rows:
        try:
            import ccxt
            exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))
            rows = exchange.fetch_ohlcv(f"{underlying}/USDT", timeframe, limit=limit)
        except Exception as e:
            print(f"BinanceUS OHLCV fetch failed: {e}", file=sys.stderr)
            if errors is not None:
                errors.append(e)
            return None
    if not rows or len(rows) < min_len:
        return None
    try:
        import pandas as pd
        df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
        return df
    except Exception as e:
        print(f"OHLCV DataFrame construction failed: {e}", file=sys.stderr)
        return None


def _regime_label_from_df(df):
    if df is None or len(df) < REGIME_MIN_BARS:
        return None
    try:
        return latest_regime(df)["regime"]
    except Exception as e:
        print(f"Regime detection failed: {e}", file=sys.stderr)
        return None


def _platform_extra(adapter, underlying: str) -> dict:
    multiplier_fn = getattr(adapter, 'get_multiplier', None)
    if multiplier_fn is not None:
        return {'multiplier': multiplier_fn(underlying), 'contract_spec': 'CME_MICRO'}
    return {}


def _build_action(action, option_type, strike, expiry_str, dte,
                  premium_pct, premium_usd, greeks, **extra) -> dict:
    d = {
        "action": action,
        "option_type": option_type,
        "strike": strike,
        "expiry": expiry_str,
        "dte": dte,
        "premium": premium_pct,
        "premium_usd": premium_usd,
        "greeks": greeks,
    }
    d.update(extra)
    return d


def _priced_leg(adapter, underlying, option_type, strike, expiry_str, dte, spot, vol):
    prem_pct, prem_usd, greeks = adapter.get_premium_and_greeks(
        underlying, option_type, strike, expiry_str, dte, spot, vol
    )
    if not (_is_finite_number(prem_pct) and _is_finite_number(prem_usd)):
        raise OptionDataUnavailable("option premium unavailable: entry premium is not finite")
    amount = prem_usd if prem_usd > 0 else prem_pct * spot
    if not math.isfinite(amount) or amount <= 0:
        raise OptionDataUnavailable("option premium unavailable: entry premium is not positive")
    return prem_pct, prem_usd, greeks


def _is_finite_number(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def _option_leg(underlying, action, option_type, target_strike,
                expiry_str, dte, fallback_pct, spot, vol, adapter, **extra) -> dict:
    strike = adapter.get_real_strike(underlying, expiry_str, option_type, target_strike)
    prem_pct, prem_usd, greeks = _priced_leg(adapter, underlying, option_type, strike, expiry_str, dte, spot, vol)
    platform_fields = _platform_extra(adapter, underlying)
    return _build_action(action, option_type, strike, expiry_str, dte,
                         prem_pct, prem_usd, greeks, **platform_fields, **extra)


def parse_positions_context(raw_positions):
    option_positions, spot_positions = [], []
    for p in (raw_positions or []):
        if isinstance(p, dict) and p.get("position_type") == "spot":
            spot_positions.append(p)
        elif isinstance(p, dict):
            option_positions.append(p)
    return option_positions, spot_positions


def score_new_trade(proposed_action, existing_positions, spot_price):
    if not existing_positions:
        return 1.0, "first position"

    score = 0.5
    reasons = []

    p_strike = proposed_action.get("strike", 0)
    p_expiry = proposed_action.get("expiry", "")
    p_type = proposed_action.get("option_type", "")
    p_delta = proposed_action.get("greeks", {}).get("delta", 0)

    same_type = [p for p in existing_positions if p.get("option_type") == p_type]
    if same_type and spot_price > 0:
        min_dist = min(abs(p_strike - p["strike"]) / spot_price for p in same_type)
        if min_dist > 0.10:
            score += 0.4
            reasons.append(f"strike distance {min_dist:.1%}")
        elif min_dist > 0.05:
            score += 0.2
            reasons.append(f"moderate strike distance {min_dist:.1%}")
        else:
            score -= 0.3
            reasons.append(f"overlapping strikes {min_dist:.1%}")

    existing_expiries = set(p.get("expiry", "") for p in existing_positions)
    if p_expiry not in existing_expiries:
        score += 0.3
        reasons.append("different expiry")
    else:
        score -= 0.1
        reasons.append("same expiry")

    net_delta = sum(p.get("delta", 0) for p in existing_positions)
    new_net = net_delta + p_delta
    if abs(new_net) > abs(net_delta) and abs(new_net) > 0.5:
        score -= 0.3
        reasons.append(f"delta concentration {new_net:+.2f}")
    elif abs(new_net) < abs(net_delta):
        score += 0.2
        reasons.append(f"delta balancing {new_net:+.2f}")

    if proposed_action.get("action") == "sell":
        sell_positions = [p for p in existing_positions if p.get("action") == "sell"]
        if sell_positions:
            avg_premium = sum(p.get("entry_premium_usd", 0) for p in sell_positions) / len(sell_positions)
            if proposed_action.get("premium_usd", 0) > avg_premium * 1.1:
                score += 0.1
                reasons.append("better premium")

    return round(score, 2), "; ".join(reasons) if reasons else "default"


def evaluate_momentum_options(underlying, spot_price, vol_annual, iv_rank,
                               existing_positions, spot_positions, adapter):
    fetch_errors = []
    df = _fetch_ohlcv_df(underlying, "4h", 100, 30, adapter=adapter, errors=fetch_errors)
    if df is None:
        cause = next((e for e in reversed(fetch_errors) if option_data.upstream_status(e) is not None), None)
        raise option_data.unavailable("momentum bars unavailable", cause)
    closes = df["close"].tolist()

    roc_period = 14
    threshold = 5.0
    current_roc = (closes[-1] - closes[-1 - roc_period]) / closes[-1 - roc_period] * 100
    prev_roc = (closes[-2] - closes[-2 - roc_period]) / closes[-2 - roc_period] * 100

    signal = 0
    if current_roc > threshold and prev_roc <= threshold:
        signal = 1
    elif current_roc < -threshold and prev_roc >= -threshold:
        signal = -1

    actions = []
    if signal != 0:
        expiry_str, dte = adapter.get_real_expiry(underlying, 37)
        if signal == 1:
            actions.append(_option_leg(
                underlying, "buy", "call", spot_price * 1.02,
                expiry_str, dte, 0.045, spot_price, vol_annual, adapter,
            ))
        else:
            actions.append(_option_leg(
                underlying, "buy", "put", spot_price * 0.98,
                expiry_str, dte, 0.040, spot_price, vol_annual, adapter,
            ))

    return signal, actions, iv_rank


def evaluate_vol_mean_reversion(underlying, spot_price, vol_annual, iv_rank,
                                 existing_positions, spot_positions, adapter):
    signal = 0
    actions = []
    expiry_str, dte = adapter.get_real_expiry(underlying, 30)
    pf = _platform_extra(adapter, underlying)

    if iv_rank > 75:
        signal = -1
        actions = [
            _option_leg(underlying, "sell", "call", spot_price * 1.10,
                        expiry_str, dte, 0.025, spot_price, vol_annual, adapter),
            _option_leg(underlying, "sell", "put", spot_price * 0.90,
                        expiry_str, dte, 0.020, spot_price, vol_annual, adapter),
        ]
    elif iv_rank < 25:
        signal = 1
        atm_strike = adapter.get_real_strike(underlying, expiry_str, "call", spot_price)
        call_pct, call_usd, call_greeks = _priced_leg(
            adapter, underlying, "call", atm_strike, expiry_str, dte, spot_price, vol_annual
        )
        put_pct, put_usd, put_greeks = _priced_leg(
            adapter, underlying, "put", atm_strike, expiry_str, dte, spot_price, vol_annual
        )
        actions = [
            _build_action("buy", "call", atm_strike, expiry_str, dte,
                          call_pct, call_usd, call_greeks, **pf),
            _build_action("buy", "put", atm_strike, expiry_str, dte,
                          put_pct, put_usd, put_greeks, **pf),
        ]

    return signal, actions, iv_rank


def evaluate_protective_puts(underlying, spot_price, vol_annual, iv_rank,
                              existing_positions, spot_positions, adapter):
    has_protective_put = any(
        p.get("option_type") == "put" and p.get("action") == "buy"
        for p in existing_positions
    )
    if has_protective_put:
        return 0, [], iv_rank

    expiry_str, dte = adapter.get_real_expiry(underlying, 45)
    actions = [
        _option_leg(underlying, "buy", "put", spot_price * 0.88,
                    expiry_str, dte, 0.015, spot_price, vol_annual, adapter),
    ]
    return 1, actions, iv_rank


def evaluate_covered_calls(underlying, spot_price, vol_annual, iv_rank,
                            existing_positions, spot_positions, adapter):
    has_covered_call = any(
        p.get("option_type") == "call" and p.get("action") == "sell"
        for p in existing_positions
    )
    if has_covered_call:
        return 0, [], iv_rank

    expiry_str, dte = adapter.get_real_expiry(underlying, 21)
    actions = [
        _option_leg(underlying, "sell", "call", spot_price * 1.12,
                    expiry_str, dte, 0.020, spot_price, vol_annual, adapter),
    ]
    return -1, actions, iv_rank


def evaluate_wheel(underlying, spot_price, vol_annual, iv_rank,
                   existing_positions, spot_positions, adapter):
    has_assigned_spot = any(
        p.get("symbol", "").upper() == underlying.upper()
        and p.get("side") == "long"
        and p.get("quantity", 0) > 0
        for p in spot_positions
    )

    if has_assigned_spot:
        has_active_call = any(
            p.get("option_type") == "call" and p.get("action") == "sell"
            for p in existing_positions
        )
        if has_active_call:
            return 0, [], iv_rank
        expiry_str, dte = adapter.get_real_expiry(underlying, 21)
        actions = [
            _option_leg(underlying, "sell", "call", spot_price * 1.10,
                        expiry_str, dte, 0.020, spot_price, vol_annual, adapter,
                        wheel_phase=2),
        ]
        return -1, actions, iv_rank
    else:
        has_wheel_put = any(
            p.get("option_type") == "put" and p.get("action") == "sell"
            for p in existing_positions
        )
        if has_wheel_put:
            return 0, [], iv_rank
        expiry_str, dte = adapter.get_real_expiry(underlying, 37)
        actions = [
            _option_leg(underlying, "sell", "put", spot_price * 0.94,
                        expiry_str, dte, 0.020, spot_price, vol_annual, adapter,
                        wheel_phase=1),
        ]
        return -1, actions, iv_rank


def evaluate_butterfly(underlying, spot_price, vol_annual, iv_rank,
                        existing_positions, spot_positions, adapter):
    if iv_rank < 30 or iv_rank > 70:
        return 0, [], iv_rank

    expiry_str, dte = adapter.get_real_expiry(underlying, 30)
    actions = [
        _option_leg(underlying, "buy", "call", spot_price * 0.95,
                    expiry_str, dte, 0.055, spot_price, vol_annual, adapter),
        _option_leg(underlying, "sell", "call", spot_price,
                    expiry_str, dte, 0.035, spot_price, vol_annual, adapter,
                    quantity=2),
        _option_leg(underlying, "buy", "call", spot_price * 1.05,
                    expiry_str, dte, 0.015, spot_price, vol_annual, adapter),
    ]
    return 1, actions, iv_rank


STRATEGY_MAP = {
    "momentum_options": evaluate_momentum_options,
    "vol_mean_reversion": evaluate_vol_mean_reversion,
    "protective_puts": evaluate_protective_puts,
    "covered_calls": evaluate_covered_calls,
    "wheel": evaluate_wheel,
    "butterfly": evaluate_butterfly,
}


def _is_live_args(args):
    for i, arg in enumerate(args):
        if arg == "--mode=live":
            return True
        if arg == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False


def _options_live_mode_error(platform):
    return (
        "options strategies run as paper only: no venue order path exists for options on "
        f"{platform}; remove the strategy, or replace --mode=live (or --mode live) with --mode=paper under a new id"
    )


def _usage():
    return f"Usage: {sys.argv[0]} <strategy> <underlying> [positions_json] [--platform={'|'.join(SUPPORTED_PLATFORMS)}]"


def _parse_args(args):
    platform = "deribit"
    regime_payload_json = None
    remaining = []
    invalid = False
    i = 0
    while i < len(args):
        arg = args[i]
        if arg.startswith("--mode="):
            pass
        elif arg == "--mode":
            if i + 1 < len(args) and not args[i + 1].startswith("--"):
                i += 1
        elif arg.startswith("--platform="):
            platform = arg.split("=", 1)[1]
        elif arg.startswith("--platform"):
            invalid = True
        elif arg.startswith("--regime-payload-json="):
            regime_payload_json = arg.split("=", 1)[1]
        elif arg.startswith("--regime-payload-json"):
            regime_payload_json = ""
        else:
            remaining.append(arg)
        i += 1
    return platform, regime_payload_json, remaining, invalid


def _reject_constant(name):
    raise ValueError(f"non-finite JSON constant {name}")


def _finite_float(text):
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"non-finite JSON number {text}")
    return value


def _read_positions(remaining):
    if len(remaining) > 2:
        raw = remaining[2]
    elif sys.stdin is None or sys.stdin.isatty():
        return []
    else:
        raw = sys.stdin.read()
    if not raw.strip():
        return []
    try:
        positions = json.loads(raw, parse_constant=_reject_constant, parse_float=_finite_float)
    except (ValueError, RecursionError) as e:
        print(f"positions JSON rejected: {e}", file=sys.stderr)
        raise CheckError("invalid positions JSON") from None
    if not isinstance(positions, list):
        raise CheckError("invalid positions payload: expected a JSON list")
    if any(not isinstance(p, dict) for p in positions):
        raise CheckError("invalid positions payload: every entry must be a JSON object")
    return positions


def _measured_vol_metrics(adapter, underlying):
    try:
        vol_annual, iv_rank = adapter.get_vol_metrics(underlying)
    except OptionDataUnavailable as e:
        print(f"volatility metrics unavailable for {underlying}: {e.reason}", file=sys.stderr)
        vol = e.vol_annual if option_data.is_finite_positive(e.vol_annual) else None
        return vol, None, e.reason
    if not option_data.is_finite_positive(vol_annual):
        return None, None, "volatility unavailable: adapter returned an invalid volatility"
    if not _is_finite_number(iv_rank) or not 0 <= iv_rank <= 100:
        return vol_annual, None, "iv rank unavailable: adapter returned an invalid rank"
    return vol_annual, iv_rank, None


def _stable_reason(exc):
    if isinstance(exc, (CheckError, OptionDataUnavailable)):
        return exc.reason
    code = option_data.upstream_status(exc)
    reason = f"options check failed: {type(exc).__name__}"
    return f"{reason} (upstream status {code})" if code is not None else reason


def _result(ctx, **fields):
    doc = {
        "strategy": ctx["strategy"],
        "underlying": ctx["underlying"],
        "signal": 0,
        "spot_price": 0,
        "actions": [],
        "iv_rank": None,
        "regime": None,
        "platform": ctx["platform"],
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    doc.update(fields)
    return doc


def _evaluate(ctx, args):
    platform, regime_payload_json, remaining, invalid_args = _parse_args(args)
    ctx["platform"] = platform
    ctx["strategy"] = remaining[0] if remaining else ""
    ctx["underlying"] = remaining[1].upper() if len(remaining) > 1 else ""

    if _is_live_args(args):
        raise CheckError(_options_live_mode_error(platform))
    if invalid_args or len(remaining) < 2 or len(remaining) > 3:
        raise CheckError(_usage())
    strategy_name = ctx["strategy"]
    underlying = ctx["underlying"]
    if strategy_name not in STRATEGY_MAP:
        raise CheckError(f"Unknown strategy: {strategy_name}. Available: {list(STRATEGY_MAP.keys())}")
    if platform not in SUPPORTED_PLATFORMS:
        raise CheckError(
            f"unsupported options platform: {platform}; options run only on {', '.join(SUPPORTED_PLATFORMS)}"
        )

    existing_positions, spot_positions = parse_positions_context(_read_positions(remaining))

    if len(existing_positions) >= MAX_POSITIONS_PER_STRATEGY:
        return _result(ctx, skip_reason=f"Max positions reached ({len(existing_positions)}/{MAX_POSITIONS_PER_STRATEGY})")

    adapter = _load_adapter(platform)

    spot_price = option_data.require_finite_positive(adapter.get_spot_price(underlying), "Could not fetch spot price")

    vol_annual, iv_rank, vol_metrics_error = _measured_vol_metrics(adapter, underlying)

    if regime_payload_json is not None:
        _, _, regime_snap = regime_from_injected_payload(regime_payload_json, windows_spec={"default": {"classifier": "adx", "period": 14}})
        regime_label = str(regime_snap.get("regime") or "") or None
    else:
        regime_df = _fetch_ohlcv_df(underlying, REGIME_TIMEFRAME, REGIME_LIMIT,
                                    REGIME_MIN_BARS, adapter=adapter)
        regime_label = _regime_label_from_df(regime_df)

    if strategy_name in RANK_STRATEGIES and iv_rank is None:
        raise CheckError(f"{strategy_name} needs iv rank: {vol_metrics_error}")

    evaluate_fn = STRATEGY_MAP[strategy_name]
    try:
        signal, actions, iv_rank = evaluate_fn(
            underlying, spot_price, vol_annual, iv_rank,
            existing_positions, spot_positions, adapter
        )
    except OptionDataUnavailable as e:
        if e.reason == option_data.MODEL_NEEDS_VOLATILITY and vol_metrics_error:
            raise CheckError(f"{e.reason}: {vol_metrics_error}") from e
        raise

    scored_actions = []
    for action in actions:
        score, reason = score_new_trade(action, existing_positions, spot_price)
        action["score"] = score
        action["score_reason"] = reason
        if score >= MIN_SCORE_THRESHOLD:
            scored_actions.append(action)
        else:
            print(f"Skipping {action.get('action')} {action.get('option_type')} "
                  f"strike={action.get('strike')}: score={score} ({reason})",
                  file=sys.stderr)

    if actions and not scored_actions:
        signal = 0

    output = _result(
        ctx,
        signal=signal,
        spot_price=spot_price,
        actions=scored_actions,
        iv_rank=round(iv_rank, 1) if iv_rank is not None else None,
        regime=regime_label,
    )
    if vol_metrics_error:
        output["vol_metrics_error"] = vol_metrics_error
    multiplier_fn = getattr(adapter, 'get_multiplier', None)
    if multiplier_fn is not None:
        output["multiplier"] = multiplier_fn(underlying)
        output["contract_type"] = "CME_MICRO"
    return output


def _run(args, emit):
    ctx = {"strategy": "", "underlying": "", "platform": "deribit"}
    try:
        output = _evaluate(ctx, args)
    except Exception as e:
        if not isinstance(e, CheckError):
            traceback.print_exc(file=sys.stderr)
        emit(_result(ctx, error=_stable_reason(e)))
        return 1
    try:
        json.dumps(output, allow_nan=False)
    except (TypeError, ValueError) as e:
        print(f"options result rejected: {e}", file=sys.stderr)
        emit(_result(ctx, error="options result invalid: a value is not finite JSON"))
        return 1
    emit(output)
    return 0


def main():
    if "--probe-only" in sys.argv:
        sys.exit(0)
    with json_stdout() as emit:
        code = _run(sys.argv[1:], emit)
    sys.exit(code)


if __name__ == "__main__":
    main()
