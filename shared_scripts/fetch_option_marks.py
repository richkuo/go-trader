#!/usr/bin/env python3

import importlib.util
import json
import math
import os
import sys
import traceback
from datetime import datetime, timezone

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _REPO_ROOT)
sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

from stdout_contract import json_stdout

CONTRACT = "option_marks_v1"
PREMIUM_UNIT = "underlying_unit"
SUPPORTED_PLATFORMS = ("robinhood",)
OPERATIONS = ("mark", "settle", "resolve")
SETTLEMENT_SOURCE = "yahoo_daily_raw_close"


class BatchError(Exception):

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _parse_args(argv):
    platform = ""
    probe = False
    invalid = []
    for arg in argv:
        if arg.startswith("--platform="):
            platform = arg.split("=", 1)[1]
        elif arg == "--probe-only":
            probe = True
        else:
            invalid.append(arg)
    return platform, probe, invalid


def _doc(platform, **fields):
    doc = {"contract": CONTRACT, "platform": platform, "premium_unit": PREMIUM_UNIT, "legs": []}
    doc.update(fields)
    return doc


def _load_adapter(platform):
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        raise ImportError(f"No adapter found for platform '{platform}' at {adapter_path}")
    platform_dir = os.path.dirname(adapter_path)
    if platform_dir not in sys.path:
        sys.path.insert(0, platform_dir)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", adapter_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    kwargs = {"asset_class": module.ASSET_CLASS_STOCK} if platform == "robinhood" else {}
    for name in dir(module):
        if name.endswith("ExchangeAdapter"):
            return getattr(module, name)(**kwargs)
    raise AttributeError(f"No ExchangeAdapter class found in {adapter_path}")


def _reject_constant(name):
    raise ValueError(f"non-finite JSON constant {name}")


def _finite_float(text):
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"non-finite JSON number {text}")
    return value


def _number(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def _nonempty_text(value):
    return isinstance(value, str) and value.strip() != ""


def _parse_refresh_at(text):
    if not _nonempty_text(text):
        raise BatchError("request refresh_at is missing")
    try:
        when = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        raise BatchError(f"request refresh_at {text!r} is not an ISO timestamp") from None
    if when.tzinfo is None:
        raise BatchError("request refresh_at has no time zone")
    return when.astimezone(timezone.utc)


def _read_request(platform):
    raw = sys.stdin.read() if sys.stdin is not None else ""
    try:
        req = json.loads(raw, parse_constant=_reject_constant, parse_float=_finite_float)
    except (ValueError, RecursionError) as e:
        print(f"option marks request rejected: {e}", file=sys.stderr)
        raise BatchError("invalid option marks request JSON") from None
    if not isinstance(req, dict):
        raise BatchError("option marks request must be a JSON object")
    if req.get("contract") != CONTRACT:
        raise BatchError(f"request contract is not {CONTRACT}")
    if req.get("platform") != platform:
        raise BatchError(f"request platform {req.get('platform')!r} does not match --platform={platform}")
    refresh_at = _parse_refresh_at(req.get("refresh_at"))
    legs = req.get("legs")
    if not isinstance(legs, list):
        raise BatchError("request legs must be a JSON list")
    seen = set()
    for leg in legs:
        if not isinstance(leg, dict):
            raise BatchError("every request leg must be a JSON object")
        leg_id = leg.get("id")
        if not _nonempty_text(leg_id) or leg_id in seen:
            raise BatchError(f"request leg id {leg_id!r} is missing or duplicated")
        seen.add(leg_id)
        if not _nonempty_text(leg.get("underlying")) or not _nonempty_text(leg.get("expiry")):
            raise BatchError(f"request leg {leg_id} needs an underlying and an expiry")
        if leg.get("option_type") not in ("call", "put") or leg.get("action") not in ("buy", "sell"):
            raise BatchError(f"request leg {leg_id} has an invalid option type or action")
        if not _number(leg.get("strike")) or leg["strike"] <= 0 or not _number(leg.get("dte")):
            raise BatchError(f"request leg {leg_id} has an invalid strike or dte")
        if leg.get("operation") not in OPERATIONS:
            raise BatchError(f"request leg {leg_id} has an invalid operation")
    return refresh_at, legs


def _stable_reason(exc, prefix):
    import option_data
    if isinstance(exc, option_data.OptionDataUnavailable):
        return exc.reason
    code = option_data.upstream_status(exc)
    reason = f"{prefix}: {type(exc).__name__}"
    return f"{reason} (upstream status {code})" if code is not None else reason


class _Reader:

    def __init__(self, platform):
        self.platform = platform
        self._adapter = None
        self._spots = {}
        self._vols = {}

    def adapter(self):
        if self._adapter is None:
            self._adapter = _load_adapter(self.platform)
        return self._adapter

    def spot(self, underlying):
        import option_data
        if underlying not in self._spots:
            try:
                value = self.adapter().get_spot_price(underlying)
                self._spots[underlying] = (option_data.require_finite_positive(
                    value, f"spot unavailable: {underlying} spot price is not positive"), None)
            except Exception as e:
                self._spots[underlying] = (None, e)
        value, err = self._spots[underlying]
        if err is not None:
            raise err
        return value

    def vol(self, underlying):
        import option_data
        if underlying not in self._vols:
            vol, _rank, error = option_data.measured_vol_metrics(self.adapter(), underlying)
            self._vols[underlying] = (vol, error)
        vol, error = self._vols[underlying]
        if vol is None:
            raise option_data.OptionDataUnavailable(f"{option_data.MODEL_NEEDS_VOLATILITY}: {error}")
        return vol


def _evaluate_leg(leg, refresh_at, reader):
    import option_data
    import option_sessions
    out = {"id": leg["id"]}
    underlying = leg["underlying"].upper()
    try:
        session = option_sessions.resolve_expiry_session(leg["expiry"])
    except option_sessions.SessionUnresolved as e:
        out.update(status="error", reason=e.reason)
        return out
    out.update({k: session[k] for k in ("session_date", "cutoff_utc", "settlement_ready_utc", "calendar", "non_session_mapped")})
    cutoff, ready = session["cutoff"], session["ready"]
    try:
        reader.adapter().require_stock_underlying(underlying)
        if refresh_at < cutoff:
            adapter = reader.adapter()
            spot = reader.spot(underlying)
            dte = (cutoff - refresh_at).total_seconds() / 86400.0
            vol = None if adapter.quotes_authenticated else reader.vol(underlying)
            value, greeks, source = adapter.get_option_mark(underlying, leg["option_type"], float(leg["strike"]),
                                                            leg["expiry"], dte, spot, vol)
            out.update(status="mark", mark={"value_per_unit": value, "spot": spot, "greeks": greeks, "source": source})
            return out
        if refresh_at < ready:
            out.update(status="pending", reason=f"settlement close not final before {session['settlement_ready_utc']} "
                                                f"(session close + 60 minute read boundary)")
            return out
        close, why = reader.adapter().get_settlement_close(underlying, session["session_date"], refresh_at)
        if close is None:
            out.update(status="pending", reason=why)
            return out
        out.update(status="settled", settlement={"price": close, "basis": "raw_close", "date": session["session_date"],
                                                 "source": SETTLEMENT_SOURCE})
        return out
    except option_data.OptionDataUnavailable as e:
        out.update(status="error", reason=e.reason)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        out.update(status="error", reason=_stable_reason(e, "option read failed"))
    return out


def _run(platform, emit):
    try:
        refresh_at, legs = _read_request(platform)
    except BatchError as e:
        emit(_doc(platform, error=e.reason))
        return 1
    reader = _Reader(platform)
    if legs:
        try:
            reader.adapter()
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            emit(_doc(platform, error=_stable_reason(e, "robinhood adapter unavailable")))
            return 1
    results = [_evaluate_leg(leg, refresh_at, reader) for leg in legs]
    doc = _doc(platform, legs=results, refresh_at=refresh_at.strftime("%Y-%m-%dT%H:%M:%S.%fZ"))
    try:
        json.dumps(doc, allow_nan=False)
    except (TypeError, ValueError) as e:
        print(f"option marks result rejected: {e}", file=sys.stderr)
        emit(_doc(platform, error="option marks result invalid: a value is not finite JSON"))
        return 1
    emit(doc)
    return 0


def main():
    platform, probe, invalid = _parse_args(sys.argv[1:])
    if probe:
        if platform not in SUPPORTED_PLATFORMS or invalid:
            print(f"unsupported option marks probe argv: platform={platform!r} extra={invalid}", file=sys.stderr)
            sys.exit(2)
        import option_sessions
        option_sessions.calendar()
        with json_stdout() as emit:
            emit({"contract": CONTRACT, "platform": platform, "probe": True})
        sys.exit(0)
    with json_stdout() as emit:
        if platform not in SUPPORTED_PLATFORMS or invalid:
            emit(_doc(platform, error=f"usage: {sys.argv[0]} --platform={'|'.join(SUPPORTED_PLATFORMS)} [--probe-only]"))
            sys.exit(1)
        code = _run(platform, emit)
    sys.exit(code)


if __name__ == "__main__":
    main()
