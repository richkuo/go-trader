
import contextlib
import fcntl
import hashlib
import io
import json
import os
import pickle
import secrets
import shutil
import signal
import stat
import sys
import math
import re
import tempfile
import threading
import time
import types
import uuid
from collections import namedtuple
from datetime import datetime, timezone, timedelta
from decimal import Context, Decimal, DivisionByZero, Inexact, InvalidOperation, Overflow, localcontext
from typing import Tuple
from urllib.parse import urlsplit

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import egress_policy

YAHOO_CRYPTO_MAP = {
    "BTC": "BTC-USD",
    "ETH": "ETH-USD",
    "SOL": "SOL-USD",
    "DOGE": "DOGE-USD",
    "AVAX": "AVAX-USD",
    "LINK": "LINK-USD",
    "ADA": "ADA-USD",
    "DOT": "DOT-USD",
    "MATIC": "MATIC-USD",
    "SHIB": "SHIB-USD",
}

_HOUR_MS = 3_600_000
_DAY_MS = 86_400_000


RobinhoodTimeframe = namedtuple("RobinhoodTimeframe", ("yahoo_interval", "resample_hours", "session_interval"))

ROBINHOOD_TIMEFRAMES = {
    "1m": RobinhoodTimeframe("1m", 0, "1m"),
    "2m": RobinhoodTimeframe("2m", 0, "2m"),
    "5m": RobinhoodTimeframe("5m", 0, "5m"),
    "15m": RobinhoodTimeframe("15m", 0, "15m"),
    "30m": RobinhoodTimeframe("30m", 0, "30m"),
    "1h": RobinhoodTimeframe("1h", 0, "1h"),
    "60m": RobinhoodTimeframe("1h", 0, "1h"),
    "90m": RobinhoodTimeframe("90m", 0, "90m"),
    "2h": RobinhoodTimeframe("1h", 2, ""),
    "4h": RobinhoodTimeframe("1h", 4, "4h"),
    "6h": RobinhoodTimeframe("1h", 6, ""),
    "8h": RobinhoodTimeframe("1h", 8, ""),
    "12h": RobinhoodTimeframe("1h", 12, ""),
    "1d": RobinhoodTimeframe("1d", 0, "1d"),
    "1w": RobinhoodTimeframe("1wk", 0, "1wk"),
    "1M": RobinhoodTimeframe("1mo", 0, "1mo"),
    "1mo": RobinhoodTimeframe("1mo", 0, "1mo"),
}

ROBINHOOD_CLOSED_BAR_REFUSED = {
    "1m": "Yahoo's newest closed 1m row is a flat zero-volume placeholder (open = high = low = close) until the next minute's row appears, between 10 and 20 seconds after the close in the sampled BTC-USD and ETH-USD minutes (#1822), so the settle hold would cover a third of every interval",
}

_YAHOO_INTERVAL_MS = {
    "1m": 60_000,
    "2m": 120_000,
    "5m": 300_000,
    "15m": 900_000,
    "30m": 1_800_000,
    "90m": 5_400_000,
    "1h": _HOUR_MS,
    "4h": 4 * _HOUR_MS,
    "1d": _DAY_MS,
    "1wk": 7 * _DAY_MS,
    "1mo": 31 * _DAY_MS,
}

_YAHOO_GRID_INTERVALS = ("1m", "2m", "5m", "15m", "30m", "90m", "1h", "1d")

_YAHOO_PERIODS = (
    ("1d", 1),
    ("5d", 5),
    ("1mo", 28),
    ("3mo", 89),
    ("6mo", 181),
    ("1y", 365),
    ("2y", 730),
    ("5y", 1826),
    ("10y", 3652),
    ("max", None),
)

_YAHOO_PERIOD_CAP = {
    "1m": "5d",
    "2m": "1mo",
    "5m": "1mo",
    "15m": "1mo",
    "30m": "1mo",
    "90m": "1mo",
    "1h": "1y",
    "4h": "1y",
}

_YAHOO_RANGE_MARGIN = 1.25
_YAHOO_SESSION_DAY_FACTOR = 1.6


class UnsupportedRobinhoodTimeframe(ValueError):
    pass


class RobinhoodCandleError(ValueError):
    pass


def robinhood_supported_timeframes():
    return sorted(ROBINHOOD_TIMEFRAMES)


def robinhood_timeframe(name):
    entry = ROBINHOOD_TIMEFRAMES.get(name) if isinstance(name, str) else None
    if entry is None:
        raise UnsupportedRobinhoodTimeframe(
            f"unsupported Robinhood timeframe {name!r}: Yahoo candles serve only "
            f"{', '.join(robinhood_supported_timeframes())}")
    return entry


def _is_yahoo_crypto_pair(yahoo_symbol):
    return yahoo_symbol.endswith("-USD")


def _yahoo_period_for(yahoo_interval, bars, crypto):
    span_ms = _YAHOO_INTERVAL_MS[yahoo_interval] * max(int(bars), 1)
    cap = _YAHOO_PERIOD_CAP.get(yahoo_interval)
    if not crypto and cap is not None:
        return cap
    days = span_ms / _DAY_MS
    if not crypto:
        days *= _YAHOO_SESSION_DAY_FACTOR
    days = days * _YAHOO_RANGE_MARGIN + 1
    names = [name for name, _ in _YAHOO_PERIODS]
    chosen = "max"
    for name, span_days in _YAHOO_PERIODS:
        if span_days is not None and span_days >= days:
            chosen = name
            break
    if cap is not None and names.index(chosen) > names.index(cap):
        chosen = cap
    return chosen


def _finite_row(row):
    return all(math.isfinite(v) for v in row[1:6])


def _align_to_grid(rows, interval_ms, symbol, interval, now_ms):
    if not rows:
        return rows
    out = [list(r) for r in rows]
    last = out[-1]
    if last[0] % interval_ms:
        snapped = last[0] - last[0] % interval_ms
        prev_open = out[-2][0] if len(out) > 1 else None
        if (prev_open is None or snapped > prev_open) and snapped <= now_ms < snapped + interval_ms:
            last[0] = snapped
        else:
            raise RobinhoodCandleError(
                f"Yahoo {symbol} {interval} final row opens at {last[0]}, off the {interval} grid and not inside the current interval")
    for i, row in enumerate(out):
        if row[0] % interval_ms:
            raise RobinhoodCandleError(
                f"Yahoo {symbol} {interval} row {i} opens at {row[0]}, off the {interval} grid")
        if i and row[0] <= out[i - 1][0]:
            raise RobinhoodCandleError(
                f"Yahoo {symbol} {interval} row {i} opens at {row[0]}, not after row {i - 1} ({out[i - 1][0]})")
    return out


def resample_hourly(rows, hours, now_ms, symbol="", timeframe=""):
    span = int(hours) * _HOUR_MS
    current_hour = int(now_ms) - int(now_ms) % _HOUR_MS
    buckets = []
    for row in rows:
        start = row[0] - row[0] % span
        if not buckets or buckets[-1][0] != start:
            buckets.append((start, []))
        buckets[-1][1].append(row)
    out = []
    dropped = 0
    for idx, (start, members) in enumerate(buckets):
        expected = [start + k * _HOUR_MS for k in range(int(hours))]
        good = [r for r in members if _finite_row(r)]
        opens = [r[0] for r in good]
        if start + span <= now_ms:
            complete = opens == expected
        elif start <= now_ms:
            complete = bool(opens) and opens == expected[:len(opens)] and opens[-1] <= current_hour
        else:
            complete = False
        if not complete:
            if idx:
                dropped += 1
            continue
        out.append([
            start,
            good[0][1],
            max(r[2] for r in good),
            min(r[3] for r in good),
            good[-1][4],
            sum(r[5] for r in good),
        ])
    if dropped:
        print(f"[robinhood] dropped {dropped} incomplete {timeframe} buckets for {symbol} (missing Yahoo 1h rows)", file=sys.stderr)
    return out

STRIKE_INTERVALS = [
    (100, 1),
    (500, 5),
    (float('inf'), 10),
]

_HOLDINGS_ERROR_PREFIX = "Robinhood holdings read failed:"
_HOLDINGS_CONNECT_TIMEOUT_S = 5.0
_HOLDINGS_READ_TIMEOUT_S = 10.0
_HOLDINGS_TOTAL_DEADLINE_S = 15.0
_HOLDINGS_PAGE_CAP = 50

_ORDER_CONNECT_TIMEOUT_S = 5.0
_ORDER_READ_TIMEOUT_S = 10.0
_LOGIN_WATCHDOG_CAP_S = 15.0
_CANCEL_RESERVE_S = 12.0
_CANCEL_RESERVE_SHARE = 0.4
_FINAL_MARGIN_S = 1.0
_POLL_INTERVAL_START_S = 0.5
_POLL_INTERVAL_MAX_S = 2.0
_POLL_INTERVAL_GROWTH = 1.5
_ORDER_PAGE_CAP = 20
_SEND_ATTEMPTS = 3
_QTY_TOLERANCE = 1e-9
_SETTLE_WINDOW_MS = 10 * 60 * 1000

_DECIMAL_MIN_EXPONENT = -30
_DECIMAL_MAX_ADJUSTED = 15
_SIZING_PRECISION = 100
_FLOOR_QUOTIENT_CAP = Decimal(10) ** 36
CLOSE_KIND_WHOLE = "whole"
CLOSE_KIND_PARTIAL = "partial"
CLOSE_KIND_EMERGENCY = "emergency"
CLOSE_KINDS = (CLOSE_KIND_WHOLE, CLOSE_KIND_PARTIAL)
SELL_POLICY_KINDS = (CLOSE_KIND_WHOLE, CLOSE_KIND_PARTIAL, CLOSE_KIND_EMERGENCY)

_FILLED_STATES = frozenset({"filled"})
_CANCELED_STATES = frozenset({"canceled", "cancelled"})
_REFUSED_STATES = frozenset({"rejected", "failed"})
_REFUSAL_STATUSES = frozenset({400, 401, 403, 404, 422})
_REFUSAL_KEYS = ("detail", "non_field_errors")

OUTCOME_FILLED = "filled"
OUTCOME_PARTIALLY_FILLED = "partially_filled"
OUTCOME_NO_FILL = "no_fill"
OUTCOME_REJECTED = "rejected"
OUTCOME_NOT_SENT = "not_sent"
OUTCOME_NOT_FOUND = "not_found"
OUTCOME_OPEN = "open"
OUTCOME_UNKNOWN = "unknown"
OUTCOME_INCONSISTENT = "inconsistent"
CONFIRMED_FILL_OUTCOMES = frozenset({OUTCOME_FILLED, OUTCOME_PARTIALLY_FILLED})

_EVIDENCE_KEYS = (
    "id", "ref_id", "state", "side", "currency_pair_id", "account_id",
    "quantity", "price", "cumulative_quantity", "average_price", "created_at",
)

_FEE_CONTAINER_KEYS = (
    "fee",
    "fees",
    "commission",
    "commission_paid",
    "total_fee",
    "totalFee",
    "estimated_fee",
    "regulatory_fee",
)

_FEE_VALUE_KEYS = ("cost", "amount", "total", "value")


class _HoldingsReadError(RuntimeError):
    pass


class _BudgetExpired(BaseException):
    pass


class _OrderPrepError(RuntimeError):
    pass


class _OrderEvidenceError(RuntimeError):
    pass


class _SellSizingRefusal(_OrderPrepError):
    pass


@contextlib.contextmanager
def _watchdog(seconds, what):
    if seconds <= 0:
        raise _BudgetExpired(f"{what}: the time budget is exhausted")

    def _expire(signum, frame):
        raise _BudgetExpired(f"{what}: the time budget of {seconds:.1f}s expired")

    previous = signal.signal(signal.SIGALRM, _expire)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def _norm_state(value):
    if not isinstance(value, str):
        return ""
    return value.strip().lower()


def _evidence_value(value):
    if value is None:
        return None
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, (int, float)):
        if isinstance(value, float) and not math.isfinite(value):
            return None
        return str(value)
    if isinstance(value, str):
        return value
    return None


def _order_evidence(order):
    if not isinstance(order, dict):
        return {}
    return {key: _evidence_value(order.get(key)) for key in _EVIDENCE_KEYS if key in order}


def _float_or_none(value):
    if value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _extract_fee_value(value, fee_context=False):
    if isinstance(value, dict):
        keys = _FEE_VALUE_KEYS if fee_context else _FEE_CONTAINER_KEYS
        for key in keys:
            fee = _extract_fee_value(value.get(key), fee_context=True)
            if fee is not None:
                return fee
        return None
    if isinstance(value, list):
        total = 0.0
        found = False
        for item in value:
            fee = _extract_fee_value(item, fee_context=fee_context)
            if fee is not None:
                total += fee
                found = True
        return total if found else None
    if fee_context:
        return _float_or_none(value)
    return None


def extract_order_fee(order):
    if not isinstance(order, dict):
        return None
    for key in _FEE_CONTAINER_KEYS:
        fee = _extract_fee_value(order.get(key), fee_context=True)
        if fee is not None:
            return fee if math.isfinite(fee) and fee >= 0 else None
    fee = _extract_fee_value(order.get("executions"))
    if fee is None or not math.isfinite(fee) or fee < 0:
        return None
    return fee


def _is_pre_send_failure(exc):
    import requests
    from urllib3.exceptions import NewConnectionError

    if isinstance(exc, requests.exceptions.ConnectTimeout):
        return True
    if not isinstance(exc, requests.exceptions.ConnectionError):
        return False
    seen = set()
    stack = [exc]
    while stack:
        item = stack.pop()
        if item is None or id(item) in seen:
            continue
        seen.add(id(item))
        if isinstance(item, NewConnectionError):
            return True
        if isinstance(item, BaseException):
            stack.extend(item.args)
            stack.append(item.__cause__)
            stack.append(item.__context__)
            stack.append(getattr(item, "reason", None))
    return False


def _parse_created_at_ms(value):
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z") or text.endswith("z"):
        text = text[:-1] + "+00:00"
    try:
        stamp = datetime.fromisoformat(text)
    except ValueError:
        return None
    if stamp.tzinfo is None:
        return None
    return int(stamp.timestamp() * 1000)


def _relative_close(a, b):
    scale = max(abs(a), abs(b), 1e-18)
    return abs(a - b) / scale <= _QTY_TOLERANCE


def _evaluate_terminal(order, requested_quantity):
    state = _norm_state(order.get("state"))
    cumulative = _parse_decimal(order.get("cumulative_quantity"))
    raw_average = order.get("average_price")
    average = None if raw_average is None else _parse_decimal(raw_average)
    terminal = state in _FILLED_STATES or state in _CANCELED_STATES or state in _REFUSED_STATES
    if not terminal:
        return None, ""
    if cumulative is None or cumulative < 0:
        return OUTCOME_UNKNOWN, f"order {state} with an unreadable cumulative_quantity {order.get('cumulative_quantity')!r}"
    if raw_average is not None and average is None:
        return OUTCOME_UNKNOWN, f"order {state} with an unreadable average_price {raw_average!r}"
    if requested_quantity is not None and requested_quantity > 0 and cumulative > requested_quantity * (1 + _QTY_TOLERANCE):
        return OUTCOME_INCONSISTENT, f"order {state} reports cumulative_quantity {cumulative} above the requested {requested_quantity}"
    if state in _FILLED_STATES:
        if cumulative > 0 and average is not None and average > 0:
            return OUTCOME_FILLED, ""
        return OUTCOME_UNKNOWN, f"order filled without a positive cumulative_quantity and average_price ({cumulative}, {raw_average!r})"
    if state in _CANCELED_STATES:
        if cumulative == 0:
            return OUTCOME_NO_FILL, "order canceled with no fill"
        if average is not None and average > 0:
            return OUTCOME_PARTIALLY_FILLED, f"order canceled after a partial fill of {cumulative}"
        return OUTCOME_UNKNOWN, f"order canceled after a fill of {cumulative} with no usable average_price"
    if cumulative == 0:
        return OUTCOME_REJECTED, f"order {state} with no fill"
    return OUTCOME_UNKNOWN, f"order {state} after a fill of {cumulative}"


_DECIMAL_TEXT = re.compile(r"[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?")


def _parse_decimal(value):
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        number = float(value)
    elif isinstance(value, str):
        text = value.strip()
        if not _DECIMAL_TEXT.fullmatch(text):
            return None
        number = float(text)
    else:
        return None
    if not math.isfinite(number):
        return None
    return number


def _parse_exact_decimal(value):
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        number = Decimal(value)
    elif isinstance(value, float):
        if not math.isfinite(value):
            return None
        number = Decimal(repr(value))
    elif isinstance(value, str):
        text = value.strip()
        if not _DECIMAL_TEXT.fullmatch(text):
            return None
        try:
            number = Decimal(text)
        except InvalidOperation:
            return None
    else:
        return None
    if not number.is_finite():
        return None
    digits = len(number.as_tuple().digits)
    try:
        number = number.normalize(Context(prec=max(digits, 1), traps=[InvalidOperation, Inexact, Overflow]))
    except (InvalidOperation, Inexact, Overflow):
        return None
    if number == 0:
        return Decimal(0)
    if number.as_tuple().exponent < _DECIMAL_MIN_EXPONENT or number.adjusted() > _DECIMAL_MAX_ADJUSTED:
        return None
    return number


def _pair_quantity_rules(row, symbol):
    want = symbol.strip().upper()
    if not isinstance(row, dict):
        raise _OrderPrepError(f"currency pairs: the {want} pair row is not an object")
    rules = {}
    for key, name, required in (
        ("min_order_size", "min", True),
        ("min_order_quantity_increment", "increment", True),
        ("max_order_size", "max", False),
    ):
        raw = row.get(key)
        if raw is None:
            if required:
                raise _OrderPrepError(f"currency pairs: the {want} pair has no {key}")
            rules[name] = None
            continue
        value = _parse_exact_decimal(raw)
        if value is None:
            raise _OrderPrepError(f"currency pairs: the {want} pair {key} {raw!r} is not a bounded finite decimal")
        if name == "min" and value < 0:
            raise _OrderPrepError(f"currency pairs: the {want} pair {key} {raw!r} is negative")
        if name in ("increment", "max") and value <= 0:
            raise _OrderPrepError(f"currency pairs: the {want} pair {key} {raw!r} is not positive")
        rules[name] = value
    return rules


def _decimal_text(value):
    return "none" if value is None else format(value, "f")


def plan_sized_sell(asked, available, rules, close_kind, sole_owner):
    if close_kind not in SELL_POLICY_KINDS:
        raise _SellSizingRefusal(f"close kind {close_kind!r} is not whole, partial or emergency")
    if not isinstance(asked, Decimal) or not asked.is_finite() or asked <= 0:
        raise _SellSizingRefusal(f"requested quantity {asked!r} is not a finite positive decimal")
    if not isinstance(available, Decimal) or not available.is_finite() or available < 0:
        raise _SellSizingRefusal(f"available quantity {available!r} is not a finite non-negative decimal")
    minimum = rules.get("min")
    increment = rules.get("increment")
    maximum = rules.get("max")
    if not isinstance(minimum, Decimal) or minimum < 0 or not isinstance(increment, Decimal) or increment <= 0:
        raise _SellSizingRefusal("the pair quantity rules are missing or invalid")
    sole_owner = sole_owner is True

    def refuse(reason, sent=None):
        raise _SellSizingRefusal(
            f"sell refused before sending: {reason} (requested R={_decimal_text(asked)}, "
            f"available A={_decimal_text(available)}, min_order_size M={_decimal_text(minimum)}, "
            f"increment I={_decimal_text(increment)}, max_order_size={_decimal_text(maximum)}, "
            f"computed S={_decimal_text(sent)})"
        )

    try:
        with localcontext() as ctx:
            ctx.prec = _SIZING_PRECISION
            ctx.traps[InvalidOperation] = True
            ctx.traps[DivisionByZero] = True
            ctx.traps[Overflow] = True
            ctx.traps[Inexact] = False
            sweep_room = available - asked
            sweep = close_kind == CLOSE_KIND_WHOLE and sole_owner and sweep_room > 0 and sweep_room < minimum
            target = available if sweep else min(asked, available)
            if target <= 0:
                refuse("the account has no sellable quantity", target)
            ctx.traps[Inexact] = True
            quotient = target // increment
            if quotient > _FLOOR_QUOTIENT_CAP:
                refuse("the quantity is too many increments to floor exactly", target)
            sent = quotient * increment
            ctx.traps[Inexact] = False
            residue = available - sent
    except (InvalidOperation, DivisionByZero, Overflow, Inexact) as e:
        refuse(f"exact decimal sizing failed ({type(e).__name__})")
    if sent <= 0:
        refuse("the quantity floors to zero at the pair increment", sent)
    if sent < minimum:
        refuse("the quantity is below the venue minimum", sent)
    if maximum is not None and sent > maximum:
        refuse("the quantity is above the venue maximum", sent)
    wire = float(sent)
    if not math.isfinite(wire) or Decimal(repr(wire)) != sent:
        refuse("the floored quantity has no exact JSON number form", sent)
    return {
        "asked_quantity": float(asked),
        "account_available": float(available),
        "sent_quantity": wire,
        "min_order_size": float(minimum),
        "quantity_increment": float(increment),
        "max_order_size": None if maximum is None else float(maximum),
        "close_kind": close_kind,
        "sole_owner": sole_owner,
        "swept": bool(sweep and sent > asked),
        "capped": sent != asked,
        "account_short": available < asked,
        "residue_quantity": float(residue),
    }


def _holding_avg_price(row, quantity):
    cost_bases = row.get("cost_bases")
    if not isinstance(cost_bases, list) or not cost_bases:
        return 0.0
    first = cost_bases[0]
    if not isinstance(first, dict):
        return 0.0
    basis = _parse_decimal(first.get("direct_cost_basis"))
    if basis is None or basis < 0:
        return 0.0
    avg_price = basis / quantity
    if not math.isfinite(avg_price):
        return 0.0
    return avg_price


def _parse_holding_row(row, page, index):
    where = f"page {page} row {index}"
    if not isinstance(row, dict):
        raise _HoldingsReadError(f"{where}: row is not an object")
    if "quantity" not in row:
        raise _HoldingsReadError(f"{where}: quantity is missing")
    quantity = _parse_decimal(row["quantity"])
    if quantity is None:
        raise _HoldingsReadError(f"{where}: quantity is not a finite number")
    if quantity < 0:
        raise _HoldingsReadError(f"{where}: quantity is negative")
    if quantity == 0:
        return None
    currency = row.get("currency")
    code = currency.get("code") if isinstance(currency, dict) else None
    if not isinstance(code, str) or not code.strip():
        raise _HoldingsReadError(f"{where}: held row has no currency code")
    return {
        "symbol": code.strip().upper(),
        "quantity": quantity,
        "avg_price": _holding_avg_price(row, quantity),
        "quantity_raw": row["quantity"],
        "quantity_available_raw": row.get("quantity_available"),
        "has_quantity_available": "quantity_available" in row,
    }


def _get_strike_interval(price: float) -> float:
    for threshold, interval in STRIKE_INTERVALS:
        if price < threshold:
            return interval
    return 10


@contextlib.contextmanager
def robinhood_library():
    with contextlib.redirect_stdout(sys.stderr):
        import robin_stocks.robinhood as rh
        egress_policy.pin_robin_stocks()
        rh.helper.set_output(sys.stderr)
        yield rh


class RobinhoodLoginError(RuntimeError):
    pass


_LOGIN_ERROR_PREFIX = "Robinhood login failed:"
_LOGIN_LINE_PREFIX = "[robinhood] robin_stocks: "
_WITHHELD = "(detail withheld)"
_LOGIN_FIXED_LINES = frozenset({
    "Starting login process...",
    "ERROR: There was an issue loading pickle file. Authentication may be expired - logging in normally.",
    "Verification required, handling challenge...",
    "Starting verification process...",
    "Error: No response from Robinhood API. Retrying...",
    "Check robinhood app for device approvals method...",
    "Verification successful!",
    "Retrying workflow status check...",
    "Workflow status approved! Proceeding with login...",
    "Still waiting for Robinhood to finalize login approval...",
    "Login failed. Check credentials and try again.",
})
_LOGIN_VERIFICATION_KEYS = frozenset({
    "access_token",
    "token_type",
    "refresh_token",
    "verification_workflow",
    "id",
    "type_context",
    "result",
    "challenge_status",
})
_LOGIN_VERIFICATION_TEXTS = frozenset({
    "Error: No verification ID returned in user-machine response",
    "Max retries reached. Assuming login approved and proceeding.",
    "Timeout reached. Assuming login is approved and proceeding.",
    "EOF when reading a line",
    "go-trader refused the device verification prompt",
    "go-trader never answers a login prompt",
})
_REQUEST_POST_PREFIX = "Error in request_post: "
_VERIFICATION_PREFIX = "Error during login verification: "
_API_FAILED_PREFIX = "API request failed: "
_REQUEST_POST_STATUS = re.compile(r"Received \d{3}")
_ERRNO_TAIL = re.compile(r"\[Errno (\d{1,4})\] ")
_LOGIN_PROMPT = re.compile(r"Enter the (?:sms|email) verification code sent to your device: ")


def _safe_verification_tail(tail):
    if tail.startswith("'") and tail.endswith("'") and tail[1:-1] in _LOGIN_VERIFICATION_KEYS:
        return tail
    if tail in _LOGIN_VERIFICATION_TEXTS:
        return tail
    match = _ERRNO_TAIL.match(tail)
    if match:
        number = int(match.group(1))
        return f"[Errno {number}] {os.strerror(number)} (path withheld)"
    return None


class _LoginOutputFilter:

    def __init__(self, sink):
        self._sink = sink
        self._buffer = ""
        self.detail = None

    def isatty(self):
        return False

    def writable(self):
        return True

    def write(self, text):
        try:
            if not isinstance(text, str):
                self._forward(_WITHHELD)
                return 0
            self._buffer += text
            while "\n" in self._buffer:
                line, self._buffer = self._buffer.split("\n", 1)
                self._forward(self._map_line(line))
            return len(text)
        except Exception:
            self._buffer = ""
            self._forward(_WITHHELD)
            return 0

    def flush(self):
        try:
            if self._buffer and _LOGIN_PROMPT.fullmatch(self._buffer):
                self._forward(self._buffer, newline=False)
                self._buffer = ""
            self._sink.flush()
        except Exception:
            pass

    def drain(self):
        try:
            if self._buffer:
                line = self._buffer
                self._buffer = ""
                self._forward(self._map_line(line))
            self._sink.flush()
        except Exception:
            pass

    def _forward(self, text, newline=True):
        try:
            self._sink.write(_LOGIN_LINE_PREFIX + text + ("\n" if newline else ""))
        except Exception:
            pass

    def _map_line(self, line):
        line = line.rstrip("\r")
        if line in _LOGIN_FIXED_LINES:
            return line
        if line.startswith(_REQUEST_POST_PREFIX):
            tail = line[len(_REQUEST_POST_PREFIX):]
            if _REQUEST_POST_STATUS.fullmatch(tail):
                return line
            return _REQUEST_POST_PREFIX + _WITHHELD
        if line.startswith(_VERIFICATION_PREFIX):
            safe = _safe_verification_tail(line[len(_VERIFICATION_PREFIX):])
            self.detail = safe
            return _VERIFICATION_PREFIX + (safe if safe is not None else _WITHHELD)
        if line.startswith(_API_FAILED_PREFIX):
            return _API_FAILED_PREFIX + _WITHHELD
        return "login line withheld"


@contextlib.contextmanager
def _filtered_login_output(rh):
    previous = rh.helper.get_output()
    output_filter = _LoginOutputFilter(sys.stderr)
    rh.helper.set_output(output_filter)
    try:
        with contextlib.redirect_stdout(output_filter):
            yield output_filter
    finally:
        output_filter.drain()
        rh.helper.set_output(previous)


def _nonempty_text(value):
    return isinstance(value, str) and bool(value.strip())


def _login_failure_reason(totp_error, login_error, login_os_error, result, logged_in, authorization, detail):
    if totp_error is not None:
        return f"could not make a TOTP code from ROBINHOOD_TOTP_SECRET ({totp_error})"
    if login_error is not None:
        reason = f"robin_stocks login raised {login_error}"
        if login_os_error:
            reason += f"; check that the session stage folder under {SESSION_DIR_ENV} can be written"
        return reason
    if result is None:
        if logged_in is True:
            reason = (
                "robin_stocks set its login state but returned no session; "
                "the session write to the stage folder may have failed "
                "or the login response may have had no refresh token"
            )
        else:
            reason = "robin_stocks reported a failed login (credentials, MFA code, device verification, or transport)"
        if detail is not None:
            reason += f"; robin_stocks reported {detail}"
        return reason
    if not isinstance(result, dict):
        return f"robin_stocks login returned {type(result).__name__}, expected a token object"
    access_token = result.get("access_token")
    token_type = result.get("token_type")
    if not _nonempty_text(access_token):
        return "login response has no access token"
    if not _nonempty_text(token_type):
        return "login response has no token type"
    if logged_in is not True:
        return "robin_stocks login state is not set after login"
    if not isinstance(authorization, str) or authorization != f"{token_type} {access_token}":
        return "robin_stocks session Authorization header does not match the returned token"
    return None


def _attempt_robinhood_login(username, password, totp_secret, stage, pickle_name, device_token, interactive, outcome):
    totp_error = None
    code = None
    try:
        import pyotp

        code = pyotp.TOTP(totp_secret).now()
    except Exception as e:
        totp_error = type(e).__name__

    login_error = None
    login_os_error = False
    result = None
    detail = None
    with robinhood_library() as rh:
        if totp_error is None:
            with _login_guards(rh, device_token, interactive, outcome):
                with _filtered_login_output(rh) as output_filter:
                    try:
                        result = rh.login(
                            username,
                            password,
                            mfa_code=code,
                            pickle_path=stage,
                            pickle_name=pickle_name,
                            store_session=True,
                        )
                    except Exception as e:
                        login_error = type(e).__name__
                        login_os_error = isinstance(e, OSError) and type(e).__module__ == "builtins"
                detail = output_filter.detail
        try:
            reason = _login_failure_reason(
                totp_error,
                login_error,
                login_os_error,
                result,
                rh.helper.LOGGED_IN,
                rh.helper.SESSION.headers.get("Authorization"),
                detail,
            )
        except Exception as e:
            reason = f"login validation raised {type(e).__name__}"
        if reason is not None or outcome.get("verification"):
            reason = _reset_library_session(reason)
    return reason


def _reset_library_session(reason=None):
    try:
        with robinhood_library() as rh:
            rh.helper.set_login_state(False)
            rh.helper.update_session("Authorization", None)
    except Exception as e:
        suffix = f"library session reset raised {type(e).__name__}"
        reason = suffix if reason is None else f"{reason}; {suffix}"
    return reason


SESSION_DIR_ENV = "GO_TRADER_ROBINHOOD_SESSION_DIR"
BOOTSTRAP_COMMAND = "go-trader robinhood-login --config <config.json>"
_BOOTSTRAP_TIMEOUT_S = 600.0
_SESSION_FILE_MAX_BYTES = 65536
_LATCH_FILE_MAX_BYTES = 4096
_LOCK_MARGIN_S = 0.5
_LOCK_POLL_START_S = 0.05
_LOCK_POLL_MAX_S = 0.25
_ACCOUNT_KEY_PREFIX = "go-trader/robinhood-username/v1:"
_SESSION_KEYS = ("access_token", "device_token", "refresh_token", "token_type")
_DEVICE_TOKEN = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_SESSION_VALUE = re.compile(r"[\x21-\x7e]+")
_LATCH_SINCE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z")
_LATCH_IDENTITY = re.compile(r"absent|[0-9a-f]{64}")
_GUARDED_NAMES = frozenset({"_validate_sherrif_id", "generate_device_token", "input", "getpass"})
_VERIFICATION_REFUSAL = "go-trader refused the device verification prompt"
_PROMPT_REFUSAL = "go-trader never answers a login prompt"
_VERIFICATION_MESSAGE = (
    "Robinhood asks to verify this device, and go-trader never answers a verification prompt. "
    f"Run once as the service user: {BOOTSTRAP_COMMAND} (SKILL.md, Robinhood session)."
)


class RobinhoodSessionError(RobinhoodLoginError):
    pass


class RobinhoodVerificationRequired(RobinhoodLoginError):
    pass


def robinhood_account_key(username):
    user = username.strip().lower()
    return hashlib.sha256((_ACCOUNT_KEY_PREFIX + user).encode("utf-8")).hexdigest()[:16]


def _session_refusal(text):
    return RobinhoodSessionError(f"{_LOGIN_ERROR_PREFIX} {text}")


def _folder_refusal(folder, check):
    return _session_refusal(f"the Robinhood session folder {SESSION_DIR_ENV}={folder!r} {check}")


def _errno_text(e):
    number = getattr(e, "errno", None)
    if isinstance(number, int):
        return f"[Errno {number}] {os.strerror(number)}"
    return type(e).__name__


def _warn(text):
    try:
        sys.stderr.write(f"[robinhood] {text}\n")
        sys.stderr.flush()
    except Exception:
        pass


def _fsync_folder(folder):
    fd = os.open(folder, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _session_folder():
    folder = os.environ.get(SESSION_DIR_ENV)
    if folder is None or folder == "":
        raise _session_refusal(
            f"{SESSION_DIR_ENV} is not set; go-trader sets it at startup to <config folder>/robinhood-session, "
            "so run Robinhood scripts from the go-trader binary, which never falls back to the home folder"
        )
    if "\x00" in folder or not os.path.isabs(folder):
        raise _folder_refusal(folder, "is not an absolute path")
    if os.path.normpath(folder) != folder or not os.path.basename(folder):
        raise _folder_refusal(folder, "is not a normalized path")
    parent = os.path.dirname(folder)
    try:
        parent_stat = os.stat(parent)
    except OSError as e:
        raise _folder_refusal(folder, f"has no parent folder ({_errno_text(e)})") from None
    if not stat.S_ISDIR(parent_stat.st_mode):
        raise _folder_refusal(folder, "has a parent that is not a folder")
    created = False
    try:
        os.mkdir(folder, 0o700)
        created = True
    except FileExistsError:
        pass
    except OSError as e:
        raise _folder_refusal(folder, f"cannot be created ({_errno_text(e)})") from None
    try:
        fd = os.open(folder, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    except OSError as e:
        raise _folder_refusal(folder, f"is a symlink or not a folder ({_errno_text(e)})") from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISDIR(info.st_mode):
            raise _folder_refusal(folder, "is not a folder")
        if info.st_uid != os.geteuid():
            raise _folder_refusal(folder, f"is owned by uid {info.st_uid}, not the running uid {os.geteuid()}")
        mode = stat.S_IMODE(info.st_mode)
        if created or mode & 0o077:
            try:
                os.fchmod(fd, 0o700)
            except OSError as e:
                raise _folder_refusal(folder, f"mode cannot be set to 0700 ({_errno_text(e)})") from None
            if mode & 0o077 and not created:
                _warn(f"session folder {folder} had mode {mode:04o}; set it to 0700")
    finally:
        os.close(fd)
    if os.path.realpath(folder) != folder:
        raise _folder_refusal(folder, f"is not canonical (it resolves to {os.path.realpath(folder)!r})")
    return folder


def _open_session_lock(folder, key):
    path = os.path.join(folder, f"robinhood-{key}.lock")
    try:
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, 0o600)
    except OSError as e:
        raise _folder_refusal(folder, f"is not writable: the session lock cannot be opened ({_errno_text(e)})") from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise _folder_refusal(folder, f"holds a session lock robinhood-{key}.lock that is not a regular file")
        if info.st_uid != os.geteuid():
            raise _folder_refusal(folder, f"holds a session lock robinhood-{key}.lock owned by uid {info.st_uid}")
        if stat.S_IMODE(info.st_mode) != 0o600:
            os.fchmod(fd, 0o600)
    except BaseException:
        os.close(fd)
        raise
    return fd


def _take_session_lock(fd, key, wait_until):
    pause = _LOCK_POLL_START_S
    started = time.monotonic()
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return
        except BlockingIOError:
            pass
        now = time.monotonic()
        if now >= wait_until:
            raise _session_refusal(
                f"session lock busy: another Robinhood login held robinhood-{key}.lock for {now - started:.1f}s; "
                "this login sent no request"
            )
        time.sleep(min(pause, max(wait_until - now, 0.0)))
        pause = min(pause * 2, _LOCK_POLL_MAX_S)


def _remove_stale_stages(folder, key):
    prefix = f".stage-{key}-"
    for name in os.listdir(folder):
        if not name.startswith(prefix):
            continue
        path = os.path.join(folder, name)
        info = os.lstat(path)
        if info.st_uid != os.geteuid():
            raise _folder_refusal(folder, f"holds a stale stage {name} owned by uid {info.st_uid}")
        if stat.S_ISDIR(info.st_mode):
            shutil.rmtree(path)
        else:
            os.unlink(path)


class _SessionUnpickler(pickle.Unpickler):

    def find_class(self, module, name):
        raise pickle.UnpicklingError("the stored session refers to a global")

    def persistent_load(self, pid):
        raise pickle.UnpicklingError("the stored session refers to a persistent id")


def _read_private_file(path, limit, fix_mode=False):
    name = os.path.basename(path)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    except OSError as e:
        raise ValueError(f"{name} is a symlink or cannot be opened ({_errno_text(e)})") from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise ValueError(f"{name} is not a regular file")
        if info.st_uid != os.geteuid():
            raise ValueError(f"{name} is owned by uid {info.st_uid}, not the running uid {os.geteuid()}")
        if info.st_nlink != 1:
            raise ValueError(f"{name} has {info.st_nlink} hard links, want 1")
        if fix_mode and stat.S_IMODE(info.st_mode) != 0o600:
            os.fchmod(fd, 0o600)
            info = os.fstat(fd)
        if stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError(f"{name} has mode {stat.S_IMODE(info.st_mode):04o}, want 0600")
        if info.st_size > limit:
            raise ValueError(f"{name} is {info.st_size} bytes, above the {limit} byte limit")
        chunks = []
        total = 0
        while True:
            chunk = os.read(fd, limit + 1 - total)
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > limit:
                raise ValueError(f"{name} grew above the {limit} byte limit")
        if fix_mode:
            os.fsync(fd)
        return b"".join(chunks)
    finally:
        os.close(fd)


def _decode_session(data, name):
    try:
        session = _SessionUnpickler(io.BytesIO(data)).load()
    except Exception as e:
        raise ValueError(f"{name} is not a plain session record ({type(e).__name__})") from None
    if not isinstance(session, dict) or set(session) != set(_SESSION_KEYS):
        raise ValueError(f"{name} does not hold exactly the keys {', '.join(_SESSION_KEYS)}")
    for field in _SESSION_KEYS:
        value = session[field]
        if not isinstance(value, str) or not _SESSION_VALUE.fullmatch(value):
            raise ValueError(f"{name} field {field} is not a non-empty printable string")
    if not _DEVICE_TOKEN.fullmatch(session["device_token"]):
        raise ValueError(f"{name} field device_token is not a device token")
    return session


def _write_private_file(folder, name, data):
    final = os.path.join(folder, name)
    tmp = os.path.join(folder, f".{name}.{secrets.token_hex(8)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    try:
        os.fchmod(fd, 0o600)
        view = memoryview(data)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        os.fsync(fd)
    except BaseException:
        os.close(fd)
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    os.close(fd)
    os.replace(tmp, final)
    _fsync_folder(folder)


def _remove_file(folder, name):
    try:
        os.unlink(os.path.join(folder, name))
    except FileNotFoundError:
        return
    _fsync_folder(folder)


def _verification_error(since=None):
    message = f"{_LOGIN_ERROR_PREFIX} {_VERIFICATION_MESSAGE}"
    if since is not None:
        message += f" No login request was sent; verification is pending since {since}."
    return RobinhoodVerificationRequired(message)


def _read_latch(folder, name):
    try:
        data = _read_private_file(os.path.join(folder, name), _LATCH_FILE_MAX_BYTES)
    except ValueError:
        return {"identity": None, "since": "an unknown time (the latch file is unreadable)"}
    if data is None:
        return None
    try:
        latch = json.loads(data.decode("utf-8"))
    except Exception:
        return {"identity": None, "since": "an unknown time (the latch file is unreadable)"}
    if not isinstance(latch, dict):
        return {"identity": None, "since": "an unknown time (the latch file is unreadable)"}
    identity = latch.get("session_sha256")
    since = latch.get("since")
    if not isinstance(identity, str) or not _LATCH_IDENTITY.fullmatch(identity):
        return {"identity": None, "since": "an unknown time (the latch file is unreadable)"}
    if not isinstance(since, str) or not _LATCH_SINCE.fullmatch(since):
        since = "an unknown time"
    return {"identity": identity, "since": since}


def _utc_now_text():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@contextlib.contextmanager
def _login_guards(rh, device_token, interactive, outcome):
    from robin_stocks.robinhood import authentication as auth

    login = getattr(rh, "login", None)
    code = getattr(login, "__code__", None)
    if (
        login is not getattr(auth, "login", None)
        or getattr(login, "__globals__", None) is not vars(auth)
        or code is None
        or not _GUARDED_NAMES <= set(code.co_names)
        or not callable(getattr(auth, "_validate_sherrif_id", None))
        or not callable(getattr(auth, "generate_device_token", None))
        or not callable(getattr(getattr(auth, "getpass", None), "getpass", None))
    ):
        raise _session_refusal(
            "the installed robin_stocks no longer matches the session guard, so go-trader cannot stop a "
            "verification prompt; no login request was sent"
        )

    def refuse_verification(*args, **kwargs):
        outcome["verification"] = True
        raise RuntimeError(_VERIFICATION_REFUSAL)

    def refuse_prompt(*args, **kwargs):
        raise RuntimeError(_PROMPT_REFUSAL)

    patches = {"generate_device_token": lambda: device_token}
    if not interactive:
        patches["_validate_sherrif_id"] = refuse_verification
        patches["input"] = refuse_prompt
        patches["getpass"] = types.SimpleNamespace(getpass=refuse_prompt)
    namespace = vars(auth)
    missing = object()
    saved = {name: namespace.get(name, missing) for name in patches}
    try:
        for name, value in patches.items():
            namespace[name] = value
        yield
    finally:
        for name, value in saved.items():
            if value is missing:
                namespace.pop(name, None)
            else:
                namespace[name] = value


def _login_attempt(folder, key, username, password, totp_secret, interactive, lock_wait_until, progress):
    lock_fd = _open_session_lock(folder, key)
    stage = None
    try:
        _take_session_lock(lock_fd, key, lock_wait_until)
        progress["locked"] = True
        _remove_stale_stages(folder, key)

        session_name = f"robinhood-{key}.pickle"
        device_name = f"robinhood-{key}.device"
        latch_name = f"robinhood-{key}.verification"
        bootstrap = f"run the bootstrap ({BOOTSTRAP_COMMAND}) after fixing or removing it"

        pre_bytes = None
        session = None
        try:
            pre_bytes = _read_private_file(os.path.join(folder, session_name), _SESSION_FILE_MAX_BYTES)
            if pre_bytes is not None:
                session = _decode_session(pre_bytes, session_name)
        except ValueError as e:
            if not interactive:
                raise _session_refusal(f"the stored Robinhood session in {folder} is refused: {e}; {bootstrap}") from None
            _warn(f"ignoring the stored session: {e}; a successful login replaces it")
            pre_bytes = None
            session = None
        identity = hashlib.sha256(pre_bytes).hexdigest() if pre_bytes is not None else "absent"

        if not interactive:
            latch = _read_latch(folder, latch_name)
            if latch is not None:
                if latch["identity"] is None or latch["identity"] == identity:
                    raise _verification_error(latch["since"])
                _remove_file(folder, latch_name)

        device_token = session["device_token"] if session is not None else None
        stored_device = None
        try:
            device_bytes = _read_private_file(os.path.join(folder, device_name), 256)
            if device_bytes is not None:
                stored_device = device_bytes.decode("ascii").strip()
                if not _DEVICE_TOKEN.fullmatch(stored_device):
                    raise ValueError(f"{device_name} does not hold a device token")
        except (ValueError, UnicodeDecodeError) as e:
            if not interactive:
                raise _session_refusal(f"the Robinhood device file in {folder} is refused: {e}; {bootstrap}") from None
            _warn(f"ignoring the device file: {e}; a successful login replaces it")
            stored_device = None
        if device_token is None:
            device_token = stored_device
        if device_token is None:
            with robinhood_library():
                from robin_stocks.robinhood import authentication as auth

                device_token = auth.generate_device_token()
            if not _DEVICE_TOKEN.fullmatch(device_token):
                raise _session_refusal("robin_stocks made a device token in an unknown form; no login request was sent")
        if device_token != stored_device:
            _write_private_file(folder, device_name, (device_token + "\n").encode("ascii"))

        stage = tempfile.mkdtemp(prefix=f".stage-{key}-", dir=folder)
        os.chmod(stage, 0o700)
        if pre_bytes is not None:
            _write_private_file(stage, session_name, pre_bytes)

        outcome = {"verification": False}
        previous_umask = os.umask(0o077)
        try:
            reason = _attempt_robinhood_login(
                username, password, totp_secret, stage, f"-{key}", device_token, interactive, outcome,
            )
        finally:
            os.umask(previous_umask)
        return {
            "reason": reason,
            "verification": outcome["verification"],
            "identity": identity,
            "pre_bytes": pre_bytes,
            "stage": stage,
            "session_name": session_name,
            "device_name": device_name,
            "latch_name": latch_name,
            "stored_device": device_token,
            "lock_fd": lock_fd,
        }
    except BaseException:
        if stage is not None:
            shutil.rmtree(stage, ignore_errors=True)
        os.close(lock_fd)
        raise


def _finish_login(folder, attempt):
    try:
        if attempt["verification"]:
            _write_private_file(
                folder,
                attempt["latch_name"],
                (json.dumps({"session_sha256": attempt["identity"], "since": _utc_now_text()}, sort_keys=True) + "\n").encode("utf-8"),
            )
            raise _verification_error()
        if attempt["reason"] is not None:
            raise RobinhoodLoginError(f"{_LOGIN_ERROR_PREFIX} {attempt['reason']}")
        session_name = attempt["session_name"]
        staged_path = os.path.join(attempt["stage"], session_name)
        try:
            staged = _read_private_file(staged_path, _SESSION_FILE_MAX_BYTES, fix_mode=True)
            if staged is None:
                raise ValueError(f"{session_name} was not written to the stage folder")
            session = _decode_session(staged, session_name)
        except (ValueError, OSError) as e:
            _reset_library_session()
            raise _session_refusal(f"robin_stocks logged in but the session was not stored: {e}") from None
        _remove_file(folder, attempt["latch_name"])
        if staged == attempt["pre_bytes"]:
            return "reused"
        os.replace(staged_path, os.path.join(folder, session_name))
        _fsync_folder(folder)
        if session["device_token"] != attempt["stored_device"]:
            _write_private_file(folder, attempt["device_name"], (session["device_token"] + "\n").encode("ascii"))
        return "stored"
    finally:
        shutil.rmtree(attempt["stage"], ignore_errors=True)
        os.close(attempt["lock_fd"])


def _robinhood_login(username, password, totp_secret, deadline, interactive):
    if not (_nonempty_text(username) and _nonempty_text(password) and _nonempty_text(totp_secret)):
        raise RobinhoodLoginError(f"{_LOGIN_ERROR_PREFIX} credentials are missing")
    if threading.current_thread() is not threading.main_thread():
        raise _session_refusal("a Robinhood login must run on the main thread, which owns the login watchdog")
    try:
        folder = _session_folder()
        key = robinhood_account_key(username)
        cap = _BOOTSTRAP_TIMEOUT_S if interactive else _LOGIN_WATCHDOG_CAP_S
        budget = cap if deadline is None else min(cap, deadline - time.monotonic())
        lock_wait_until = time.monotonic() + budget - _LOCK_MARGIN_S
        progress = {"locked": False}
        attempt = None
        try:
            with _watchdog(budget, "Robinhood login"):
                attempt = _login_attempt(folder, key, username, password, totp_secret, interactive, lock_wait_until, progress)
        except _BudgetExpired as e:
            if attempt is not None:
                shutil.rmtree(attempt["stage"], ignore_errors=True)
                os.close(attempt["lock_fd"])
            message = f"{_LOGIN_ERROR_PREFIX} {e}"
            if not progress["locked"]:
                message += " while waiting for the session lock"
            raise RobinhoodLoginError(_reset_library_session(message)) from None
        return _finish_login(folder, attempt)
    except OSError as e:
        message = f"{_LOGIN_ERROR_PREFIX} a Robinhood session file operation failed ({_errno_text(e)})"
        raise RobinhoodSessionError(_reset_library_session(message)) from None


def robinhood_login(username, password, totp_secret, deadline=None, interactive=False):
    try:
        return _robinhood_login(username, password, totp_secret, deadline, interactive)
    except RobinhoodLoginError as e:
        _warn(str(e))
        raise


class RobinhoodExchangeAdapter:

    OHLCV_TIMESTAMP_KIND = "open"

    def __init__(self, mode="paper", login_deadline=None):
        self._mode = mode
        self._logged_in = False
        self._login_deadline = login_deadline

        if mode == "live":
            self._login()
        else:
            try:
                self._login()
            except Exception:
                pass

    def _login(self):
        username = os.environ.get("ROBINHOOD_USERNAME", "")
        password = os.environ.get("ROBINHOOD_PASSWORD", "")
        totp_secret = os.environ.get("ROBINHOOD_TOTP_SECRET", "")

        if not username or not password or not totp_secret:
            if self._mode == "live":
                raise RuntimeError(
                    "Live mode requires ROBINHOOD_USERNAME, ROBINHOOD_PASSWORD, "
                    "and ROBINHOOD_TOTP_SECRET environment variables"
                )
            return

        try:
            robinhood_login(username, password, totp_secret, deadline=self._login_deadline)
        except RobinhoodLoginError:
            if self._mode == "live":
                raise
            print("[robinhood] paper mode continues logged out on public data", file=sys.stderr)
            return
        self._logged_in = True

    @property
    def is_live(self) -> bool:
        return self._mode == "live" and self._logged_in

    @property
    def mode(self) -> str:
        return self._mode

    @property
    def name(self) -> str:
        return "robinhood"


    def _resolve_yahoo_symbol(self, symbol: str) -> str:
        return YAHOO_CRYPTO_MAP.get(symbol.upper(), symbol.upper())

    def get_price(self, symbol: str) -> float:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    if symbol.upper() in YAHOO_CRYPTO_MAP:
                        quote = rh.crypto.get_crypto_quote(symbol)
                        if quote and quote.get("mark_price"):
                            return float(quote["mark_price"])
                    else:
                        prices = rh.stocks.get_latest_price(symbol)
                        if prices and prices[0]:
                            return float(prices[0])
            except Exception as e:
                print(f"[robinhood] robin_stocks price error for {symbol}: {e}", file=sys.stderr)
        return self._get_yahoo_price(symbol)

    def get_spot_price(self, symbol: str) -> float:
        return self.get_price(symbol)

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        return self._get_yahoo_ohlcv(symbol, interval, limit)

    def get_ohlcv_closes(self, symbol: str, timeframe: str = "4h", limit: int = 100, min_len: int = 30) -> list:
        candles = self._get_yahoo_ohlcv(symbol, timeframe, limit)
        if not candles or len(candles) < min_len:
            return None
        return [c[4] for c in candles]


    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        try:
            import yfinance as yf
            egress_policy.pin_yfinance()
            yahoo_sym = self._resolve_yahoo_symbol(underlying)
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period="90d", interval="1d")
            if hist.empty or len(hist) < 15:
                return 0.30, 50.0
            closes = hist["Close"].tolist()
            returns = [math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes)) if closes[i - 1] > 0]
            if len(returns) < 14:
                return 0.30, 50.0

            w = 14
            mean = sum(returns[-w:]) / w
            variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
            vol = math.sqrt(variance) * math.sqrt(252)

            hvs = []
            for i in range(len(returns) - w + 1):
                chunk = returns[i:i + w]
                m = sum(chunk) / w
                v = sum((r - m) ** 2 for r in chunk) / w
                hvs.append(math.sqrt(v) * math.sqrt(252) * 100)
            current_hv = vol * 100
            hv_min, hv_max = min(hvs), max(hvs)
            if hv_max > hv_min:
                iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100
                iv_rank = round(min(max(iv_rank, 0.0), 100.0), 1)
            else:
                iv_rank = 50.0

            return round(vol, 4), iv_rank
        except Exception as e:
            print(f"[robinhood] vol_metrics error for {underlying}: {e}", file=sys.stderr)
            return 0.30, 50.0

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    chain = rh.options.get_chains(underlying)
                if chain and chain.get("expiration_dates"):
                    today = datetime.now(timezone.utc).date()
                    target_date = today + timedelta(days=target_dte)
                    best_expiry = None
                    best_diff = float('inf')
                    for exp_str in chain["expiration_dates"]:
                        exp_date = datetime.strptime(exp_str, "%Y-%m-%d").date()
                        diff = abs((exp_date - target_date).days)
                        if diff < best_diff:
                            best_diff = diff
                            best_expiry = exp_str
                    if best_expiry:
                        actual_dte = (datetime.strptime(best_expiry, "%Y-%m-%d").date() - today).days
                        return best_expiry, max(actual_dte, 1)
            except Exception as e:
                print(f"[robinhood] get_real_expiry error: {e}", file=sys.stderr)

        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    options = rh.options.find_tradable_options(
                        underlying, expirationDate=expiry,
                        optionType=option_type
                    )
                if options:
                    best_strike = None
                    best_diff = float('inf')
                    for opt in options:
                        strike = float(opt.get("strike_price", 0))
                        if strike > 0:
                            diff = abs(strike - target_strike)
                            if diff < best_diff:
                                best_diff = diff
                                best_strike = strike
                    if best_strike is not None:
                        return best_strike
            except Exception as e:
                print(f"[robinhood] get_real_strike error: {e}", file=sys.stderr)

        interval = _get_strike_interval(target_strike)
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    options = rh.options.find_tradable_options(
                        underlying, expirationDate=expiry,
                        strikePrice=str(strike), optionType=option_type
                    )
                if options:
                    opt_id = options[0].get("id") or options[0].get("url", "").split("/")[-2]
                    if opt_id:
                        with robinhood_library() as rh:
                            market_data = rh.options.get_option_market_data_by_id(opt_id)
                        if market_data and isinstance(market_data, list) and market_data[0]:
                            md = market_data[0]
                            mark = float(md.get("adjusted_mark_price", 0) or 0)
                            greeks = {
                                "delta": float(md.get("delta", 0) or 0),
                                "gamma": float(md.get("gamma", 0) or 0),
                                "theta": float(md.get("theta", 0) or 0),
                                "vega": float(md.get("vega", 0) or 0),
                            }
                            premium_usd = mark * 100
                            mark_pct = (mark / spot) if spot > 0 else 0.0
                            return round(mark_pct, 6), round(premium_usd, 2), greeks
            except Exception as e:
                print(f"[robinhood] get_premium_and_greeks error: {e}", file=sys.stderr)

        from pricing import bs_price_and_greeks
        if vol <= 0:
            vol = 0.30
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        premium_usd = price_usd * 100
        mark_pct = (price_usd / spot) if spot > 0 else 0.0
        return round(mark_pct, 6), round(premium_usd, 2), greeks


    def _get_yahoo_price(self, symbol: str) -> float:
        yahoo_sym = self._resolve_yahoo_symbol(symbol)
        try:
            import yfinance as yf
            egress_policy.pin_yfinance()
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period="1d")
            if hist.empty:
                return 0.0
            return float(hist["Close"].iloc[-1])
        except ImportError:
            print("[robinhood] yfinance not installed. Run: uv add yfinance", file=sys.stderr)
            return 0.0
        except Exception as e:
            print(f"[robinhood] yahoo price error for {symbol}: {e}", file=sys.stderr)
            return 0.0

    def _get_yahoo_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        entry = robinhood_timeframe(interval)
        yahoo_sym = self._resolve_yahoo_symbol(symbol)
        crypto = _is_yahoo_crypto_pair(yahoo_sym)
        hours = entry.resample_hours if crypto else 0
        yf_interval = entry.yahoo_interval if crypto else entry.session_interval
        if not yf_interval:
            raise UnsupportedRobinhoodTimeframe(
                f"Robinhood timeframe {interval!r} is resampled from Yahoo 1h rows only for crypto pairs; "
                f"{yahoo_sym} is not a Yahoo crypto pair")
        bars = int(limit) + 2
        if hours:
            bars = (bars + 1) * hours
        period = _yahoo_period_for(yf_interval, bars, crypto)
        try:
            import yfinance as yf
            egress_policy.pin_yfinance()
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period=period, interval=yf_interval)
            now_ms = int(time.time() * 1000)
            if hist.empty:
                return []
            result = []
            for idx, row in hist.iterrows():
                ts_ms = int(idx.timestamp() * 1000)
                result.append([
                    ts_ms,
                    float(row["Open"]),
                    float(row["High"]),
                    float(row["Low"]),
                    float(row["Close"]),
                    float(row.get("Volume", 0)),
                ])
        except ImportError:
            print("[robinhood] yfinance not installed. Run: uv add yfinance", file=sys.stderr)
            return []
        except Exception as e:
            print(f"[robinhood] yahoo ohlcv error for {symbol} {interval} (Yahoo {yf_interval}, period {period}): {e}", file=sys.stderr)
            return []
        if crypto and yf_interval in _YAHOO_GRID_INTERVALS:
            result = _align_to_grid(result, _YAHOO_INTERVAL_MS[yf_interval], yahoo_sym, yf_interval, now_ms)
        if hours:
            result = resample_hourly(result, hours, now_ms, yahoo_sym, interval)
        return result[-limit:]

    def _logged_in_session(self, error_cls, not_logged_in="Robinhood adapter not logged in"):
        if not self._logged_in:
            raise error_cls(not_logged_in)
        with robinhood_library() as rh:
            logged_in = rh.helper.LOGGED_IN
            session = rh.helper.SESSION
            urls = rh.urls
        if logged_in is not True:
            raise error_cls("robin_stocks session not logged in")
        authorization = session.headers.get("Authorization")
        if not isinstance(authorization, str) or not authorization.strip():
            raise error_cls("robin_stocks session has no Authorization header")
        return session, urls

    def _order_request(self, session, method, url, deadline, json_body=None):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise _BudgetExpired(f"{method} {url}: the time budget is exhausted")
        kwargs = {
            "timeout": (min(_ORDER_CONNECT_TIMEOUT_S, remaining), min(_ORDER_READ_TIMEOUT_S, remaining)),
            "allow_redirects": False,
        }
        if json_body is not None:
            kwargs["json"] = json_body
            kwargs["headers"] = {"Content-Type": "application/json"}
        return session.request(method, url, **kwargs)

    def _read_json_dict(self, session, url, deadline, what):
        import requests

        try:
            res = self._order_request(session, "GET", url, deadline)
        except requests.exceptions.RequestException as e:
            raise _OrderPrepError(f"{what}: request failed ({type(e).__name__})") from e
        if res.status_code != 200:
            raise _OrderPrepError(f"{what}: HTTP status {res.status_code}")
        try:
            body = res.json()
        except ValueError as e:
            raise _OrderPrepError(f"{what}: response is not valid JSON") from e
        if not isinstance(body, dict):
            raise _OrderPrepError(f"{what}: response is not a JSON object")
        return body

    def _read_pages(self, session, first_url, deadline, what):
        host = urlsplit(first_url).netloc
        requested = set()
        url = first_url
        page = 0
        while url:
            page += 1
            if page > _ORDER_PAGE_CAP:
                raise _OrderPrepError(f"{what}: more than {_ORDER_PAGE_CAP} pages")
            if url in requested:
                raise _OrderPrepError(f"{what}: page {page} repeats a next link")
            requested.add(url)
            body = self._read_json_dict(session, url, deadline, f"{what} page {page}")
            rows = body.get("results")
            if not isinstance(rows, list):
                raise _OrderPrepError(f"{what} page {page}: response has no results list")
            if "next" not in body:
                raise _OrderPrepError(f"{what} page {page}: response has no next key")
            next_url = body["next"]
            if next_url is None or next_url == "":
                next_url = None
            elif not isinstance(next_url, str):
                raise _OrderPrepError(f"{what} page {page}: next link is not a string")
            else:
                parts = urlsplit(next_url)
                if parts.scheme != "https" or parts.netloc != host:
                    raise _OrderPrepError(f"{what} page {page}: next link is not https on {host}")
            yield page, rows, next_url
            url = next_url

    def _currency_pair_id(self, session, urls, symbol, deadline):
        return self._currency_pair_row(session, urls, symbol, deadline)[0]

    def _currency_pair_row(self, session, urls, symbol, deadline):
        want = symbol.strip().upper()
        matches = []
        for _, rows, _ in self._read_pages(session, urls.crypto_currency_pairs_url(), deadline, "currency pairs"):
            for row in rows:
                if not isinstance(row, dict):
                    continue
                asset = row.get("asset_currency")
                code = asset.get("code") if isinstance(asset, dict) else None
                if isinstance(code, str) and code.strip().upper() == want:
                    matches.append(row)
        if len(matches) > 1:
            usd = []
            for row in matches:
                quote = row.get("quote_currency")
                code = quote.get("code") if isinstance(quote, dict) else None
                if isinstance(code, str) and code.strip().upper() == "USD":
                    usd.append(row)
            matches = usd
        if len(matches) != 1:
            raise _OrderPrepError(f"currency pairs: {len(matches)} tradable pairs match {want}, expected exactly one")
        pair_id = matches[0].get("id")
        if not isinstance(pair_id, str) or not pair_id.strip():
            raise _OrderPrepError(f"currency pairs: the {want} pair has no id")
        return pair_id.strip(), matches[0]

    def holding_quantities(self, symbol, deadline):
        want = symbol.strip().upper()
        try:
            rows = self._read_crypto_holdings(deadline)
        except _HoldingsReadError as e:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} {e}") from e
        except Exception as e:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} unexpected {type(e).__name__}") from e
        held = [row for row in rows if row.get("symbol") == want]
        if not held:
            return {"quantity": Decimal(0), "available": Decimal(0)}
        row = held[0]
        if not row.get("has_quantity_available"):
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} the {want} holding has no quantity_available")
        raw_available = row.get("quantity_available_raw")
        available = _parse_exact_decimal(raw_available)
        if available is None:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} the {want} quantity_available {raw_available!r} is not a bounded finite decimal")
        if available < 0:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} the {want} quantity_available {raw_available!r} is negative")
        quantity = _parse_exact_decimal(row.get("quantity_raw"))
        if quantity is None:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} the {want} quantity {row.get('quantity_raw')!r} is not a bounded finite decimal")
        if available > quantity:
            raise _OrderPrepError(f"{_HOLDINGS_ERROR_PREFIX} the {want} quantity_available {raw_available!r} is above the quantity {row.get('quantity_raw')!r}")
        return {"quantity": quantity, "available": available}

    def _crypto_account_id(self, session, urls, deadline, expected_account_id):
        body = self._read_json_dict(session, urls.crypto_account_url(), deadline, "crypto account")
        rows = body.get("results")
        if not isinstance(rows, list):
            raise _OrderPrepError("crypto account: response has no results list")
        ids = []
        for row in rows:
            if isinstance(row, dict) and isinstance(row.get("id"), str) and row["id"].strip():
                ids.append(row["id"].strip())
        if len(ids) != 1 or len(rows) != 1:
            raise _OrderPrepError(f"crypto account: {len(rows)} account rows, expected exactly one with an id")
        if expected_account_id and ids[0] != expected_account_id:
            raise _OrderPrepError("crypto account: the account id differs from the attempt's account")
        return ids[0]

    def _quote_price(self, session, urls, pair_id, side, deadline):
        body = self._read_json_dict(session, urls.crypto_quote_url(pair_id), deadline, "crypto quote")
        key = "ask_price" if side == "buy" else "bid_price"
        price = _parse_decimal(body.get(key))
        if price is None or price <= 0:
            raise _OrderPrepError(f"crypto quote: {key} {body.get(key)!r} is not a finite positive number")
        return price

    def _poll_order(self, session, urls, order_id, ref_id, side, deadline):
        import requests

        try:
            res = self._order_request(session, "GET", urls.crypto_orders_url(order_id), deadline)
        except requests.exceptions.RequestException as e:
            return None, f"order read failed ({type(e).__name__})"
        if res.status_code != 200:
            return None, f"order read returned HTTP status {res.status_code}"
        try:
            body = res.json()
        except ValueError:
            return None, "order read returned invalid JSON"
        if not isinstance(body, dict):
            return None, "order read did not return a JSON object"
        if body.get("id") != order_id:
            raise _OrderEvidenceError(f"order read returned id {body.get('id')!r}, expected {order_id!r}")
        if "ref_id" in body and body.get("ref_id") != ref_id:
            raise _OrderEvidenceError(f"order read returned ref_id {body.get('ref_id')!r}, expected {ref_id!r}")
        if "side" in body and _norm_state(body.get("side")) != side:
            raise _OrderEvidenceError(f"order read returned side {body.get('side')!r}, expected {side!r}")
        return body, ""

    def _follow_order(self, session, urls, order_id, ref_id, side, deadline, requested_quantity, result, cancel_open=True, first=None):
        last = first
        last_error = ""
        if first is not None:
            outcome, detail = _evaluate_terminal(first, requested_quantity)
            if outcome is not None:
                return self._finish(result, outcome, detail, first)
        now = time.monotonic()
        reserve = min(_CANCEL_RESERVE_S, max(deadline - now, 0.0) * _CANCEL_RESERVE_SHARE) if cancel_open else _FINAL_MARGIN_S
        poll_deadline = deadline - reserve
        interval = _POLL_INTERVAL_START_S
        while time.monotonic() < poll_deadline:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, poll_deadline)
            if body is None:
                last_error = error
            else:
                last, last_error = body, ""
                result["order"] = _order_evidence(body)
                outcome, detail = _evaluate_terminal(body, requested_quantity)
                if outcome is not None:
                    return self._finish(result, outcome, detail, body)
            if time.monotonic() + interval >= poll_deadline:
                break
            time.sleep(interval)
            interval = min(interval * _POLL_INTERVAL_GROWTH, _POLL_INTERVAL_MAX_S)
        if not cancel_open:
            if last is None or last_error:
                return self._finish(result, OUTCOME_UNKNOWN, f"order {order_id} state is unreadable: {last_error}", last)
            return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(last.get('state')) or 'in an unknown state'}", last)
        result["cancel"]["requested"] = True
        try:
            res = self._order_request(session, "POST", urls.crypto_cancel_url(order_id), deadline - _FINAL_MARGIN_S)
            result["cancel"]["acknowledged"] = 200 <= res.status_code < 300
            if not result["cancel"]["acknowledged"]:
                result["cancel"]["detail"] = f"HTTP status {res.status_code}"
        except Exception as e:
            result["cancel"]["detail"] = f"cancel request failed ({type(e).__name__})"
        final_deadline = deadline - _FINAL_MARGIN_S
        interval = _POLL_INTERVAL_START_S
        while time.monotonic() < final_deadline:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, final_deadline)
            if body is None:
                last_error = error
            else:
                last, last_error = body, ""
                result["order"] = _order_evidence(body)
                outcome, detail = _evaluate_terminal(body, requested_quantity)
                if outcome is not None:
                    return self._finish(result, outcome, detail, body)
            if time.monotonic() + interval >= final_deadline:
                break
            time.sleep(interval)
            interval = min(interval * _POLL_INTERVAL_GROWTH, _POLL_INTERVAL_MAX_S)
        if last is None or last_error:
            return self._finish(result, OUTCOME_UNKNOWN, f"order {order_id} state is unreadable after the cancel request: {last_error}", last)
        return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(last.get('state')) or 'in an unknown state'} after the cancel request; cancel completion is unconfirmed", last)

    def _finish(self, result, outcome, detail, order):
        result["outcome"] = outcome
        result["detail"] = detail
        if isinstance(order, dict):
            result["order"] = _order_evidence(order)
        if outcome in CONFIRMED_FILL_OUTCOMES and isinstance(order, dict):
            fill = {
                "avg_px": _parse_decimal(order.get("average_price")),
                "quantity": _parse_decimal(order.get("cumulative_quantity")),
                "oid": str(order.get("id")),
            }
            fee = extract_order_fee(order)
            if fee is not None:
                fill["fee"] = fee
            result["fill"] = fill
        return result

    def _new_result(self, ref_id):
        return {
            "outcome": OUTCOME_UNKNOWN,
            "detail": "",
            "order": {},
            "ref_id": ref_id,
            "requested_quantity": None,
            "account_id": None,
            "currency_pair_id": None,
            "cancel": {"requested": False, "acknowledged": False},
            "post_attempts": 0,
        }

    def place_market_order(self, symbol, side, *, ref_id, deadline, quantity=None, amount_usd=None, expected_account_id=None, sell_policy=None):
        side = (side or "").strip().lower()
        if side not in ("buy", "sell"):
            raise ValueError(f"side must be buy or sell, got {side!r}")
        if sell_policy is not None:
            if side != "sell":
                raise ValueError("a sell policy applies only to a sell")
            asked = sell_policy.get("asked") if isinstance(sell_policy, dict) else None
            if not isinstance(asked, Decimal) or not asked.is_finite() or asked <= 0:
                raise ValueError(f"sell policy ask {asked!r} is not a finite positive decimal")
            if sell_policy.get("close_kind") not in SELL_POLICY_KINDS:
                raise ValueError(f"sell policy close kind {sell_policy.get('close_kind')!r} is not whole, partial or emergency")
        try:
            ref_id = str(uuid.UUID(str(ref_id)))
        except ValueError:
            raise ValueError(f"ref_id {ref_id!r} is not a UUID") from None
        size = quantity if side == "sell" else amount_usd
        if isinstance(size, bool) or not isinstance(size, (int, float)) or not math.isfinite(size) or size <= 0:
            raise ValueError(f"order size {size!r} is not a finite positive number")
        if not self.is_live:
            raise RuntimeError("place_market_order requires live mode")
        result = self._new_result(ref_id)
        result["_send_started"] = False
        try:
            with _watchdog(deadline - time.monotonic() - 0.25, "Robinhood order"):
                self._place(symbol, side, ref_id, deadline, size, expected_account_id, result, sell_policy)
        except _BudgetExpired as e:
            if result["_send_started"]:
                self._finish_clean(result, OUTCOME_UNKNOWN, f"{e}; the order was sent and its outcome is unknown")
            else:
                self._finish_clean(result, OUTCOME_NOT_SENT, f"{e}; no order was sent")
        except _OrderEvidenceError as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, str(e))
        except Exception as e:
            if result["_send_started"]:
                self._finish_clean(result, OUTCOME_UNKNOWN, f"unexpected {type(e).__name__} after the order was sent: {e}")
            else:
                self._finish_clean(result, OUTCOME_NOT_SENT, str(e))
        result.pop("_send_started", None)
        return result

    def _finish_clean(self, result, outcome, detail):
        result["outcome"] = outcome
        result["detail"] = detail
        result.pop("fill", None)
        return result

    def _place(self, symbol, side, ref_id, deadline, size, expected_account_id, result, sell_policy=None):
        import requests

        session, urls = self._logged_in_session(_OrderPrepError)
        pair_id, pair_row = self._currency_pair_row(session, urls, symbol, deadline)
        result["currency_pair_id"] = pair_id
        account_id = self._crypto_account_id(session, urls, deadline, expected_account_id)
        result["account_id"] = account_id
        with robinhood_library() as rh:
            round_price = rh.helper.round_price
        price = round_price(self._quote_price(session, urls, pair_id, side, deadline))
        sizing = None
        if sell_policy is not None:
            rules = _pair_quantity_rules(pair_row, symbol)
            holding = self.holding_quantities(symbol, deadline)
            sizing = plan_sized_sell(sell_policy["asked"], holding["available"], rules,
                                     sell_policy["close_kind"], sell_policy.get("sole_owner") is True)
            sizing["account_quantity"] = float(holding["quantity"])
            remaining = deadline - time.monotonic()
            reserve = min(_CANCEL_RESERVE_S, max(remaining, 0.0) * _CANCEL_RESERVE_SHARE)
            if remaining - reserve < _POLL_INTERVAL_START_S:
                raise _OrderPrepError(f"too little of the time budget is left after the account reads ({remaining:.2f}s); no order was sent")
            result["sizing"] = sizing
            order_quantity = sizing["sent_quantity"]
        elif side == "sell":
            order_quantity = size
        else:
            order_quantity = round_price(size / price)
        if not math.isfinite(order_quantity) or order_quantity <= 0:
            raise _OrderPrepError(f"order quantity {order_quantity!r} is not a finite positive number")
        result["requested_quantity"] = order_quantity
        payload = {
            "account_id": account_id,
            "currency_pair_id": pair_id,
            "price": price,
            "quantity": order_quantity,
            "ref_id": ref_id,
            "side": side,
            "time_in_force": "gtc",
            "type": "market",
        }
        response = None
        last_pre_send = None
        while result["post_attempts"] < _SEND_ATTEMPTS:
            if deadline - time.monotonic() <= 0:
                break
            result["post_attempts"] += 1
            result["_send_started"] = True
            try:
                response = self._order_request(session, "POST", urls.order_crypto_url(), deadline, json_body=payload)
                break
            except requests.exceptions.RequestException as e:
                if _is_pre_send_failure(e):
                    last_pre_send = e
                    result["_send_started"] = False
                    continue
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit failed after the request was sent ({type(e).__name__}); the outcome is unknown")
        if response is None:
            detail = f"every order submit failed before the request was sent ({type(last_pre_send).__name__ if last_pre_send else 'no budget'})"
            return self._finish_clean(result, OUTCOME_NOT_SENT, detail)
        try:
            body = response.json()
        except ValueError:
            body = None
        status = response.status_code
        if 200 <= status < 300:
            if not isinstance(body, dict):
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status} without a JSON object; the outcome is unknown")
            order_id = body.get("id")
            if not isinstance(order_id, str) or not order_id.strip():
                result["order"] = _order_evidence(body)
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status} with no order id; the outcome is unknown")
            for key, want in (("ref_id", ref_id), ("currency_pair_id", pair_id)):
                if key in body and body.get(key) != want:
                    result["order"] = _order_evidence(body)
                    return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned {key} {body.get(key)!r}, expected {want!r}")
            if "side" in body and _norm_state(body.get("side")) != side:
                result["order"] = _order_evidence(body)
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned side {body.get('side')!r}, expected {side!r}")
            result["order"] = _order_evidence(body)
            return self._follow_order(session, urls, order_id, ref_id, side, deadline, order_quantity, result, cancel_open=True, first=body)
        if status in _REFUSAL_STATUSES and isinstance(body, dict) and not body.get("id") and any(body.get(k) for k in _REFUSAL_KEYS):
            reason = "; ".join(f"{k}={body[k]!r}" for k in _REFUSAL_KEYS if body.get(k))
            result["order"] = _order_evidence(body)
            return self._finish_clean(result, OUTCOME_REJECTED, f"Robinhood refused the order (HTTP {status}): {reason}")
        return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status}; the outcome is unknown")

    def resolve_order(self, symbol, side, *, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                      order_id=None, requested_quantity=None, expected_account_id=None, cancel_open=False, now_ms=None,
                      verify_fields=False):
        side = (side or "").strip().lower()
        if side not in ("buy", "sell"):
            raise ValueError(f"side must be buy or sell, got {side!r}")
        try:
            ref_id = str(uuid.UUID(str(ref_id)))
        except ValueError:
            raise ValueError(f"ref_id {ref_id!r} is not a UUID") from None
        if requested_quantity is not None and (not math.isfinite(requested_quantity) or requested_quantity <= 0):
            raise ValueError(f"requested quantity {requested_quantity!r} is not a finite positive number")
        if not self.is_live:
            raise RuntimeError("resolve_order requires live mode")
        result = self._new_result(ref_id)
        result["requested_quantity"] = requested_quantity
        try:
            with _watchdog(deadline - time.monotonic() - 0.25, "Robinhood order resolution"):
                self._resolve(symbol, side, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                              order_id, requested_quantity, expected_account_id, cancel_open, now_ms, result,
                              verify_fields)
        except _BudgetExpired as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, f"{e}; the order outcome is still unknown")
        except (_OrderPrepError, _OrderEvidenceError) as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, str(e))
        except Exception as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, f"unexpected {type(e).__name__}: {e}")
        return result

    def _resolve(self, symbol, side, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                 order_id, requested_quantity, expected_account_id, cancel_open, now_ms, result, verify_fields=False):
        session, urls = self._logged_in_session(_OrderPrepError)
        account_id = self._crypto_account_id(session, urls, deadline, expected_account_id)
        result["account_id"] = account_id
        if order_id:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, deadline)
            if body is None:
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order {order_id}: {error}")
            if verify_fields and body.get("ref_id") != ref_id:
                pair_id = self._currency_pair_id(session, urls, symbol, deadline)
                result["currency_pair_id"] = pair_id
                self._verify_order_fields(body, pair_id, side, requested_quantity, submitted_after_ms, submitted_before_ms)
            return self._resolve_found(session, urls, order_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result)
        pair_id = self._currency_pair_id(session, urls, symbol, deadline)
        result["currency_pair_id"] = pair_id
        window_start = submitted_after_ms - _SETTLE_WINDOW_MS
        window_end = submitted_before_ms + _SETTLE_WINDOW_MS
        by_ref = []
        by_fields = []
        complete = False
        window_rows_complete = True
        try:
            for _, rows, next_url in self._read_pages(session, urls.crypto_orders_url(), deadline, "crypto orders"):
                for row in rows:
                    if not isinstance(row, dict):
                        window_rows_complete = False
                        continue
                    created = _parse_created_at_ms(row.get("created_at"))
                    if created is None:
                        window_rows_complete = False
                    if row.get("ref_id") == ref_id:
                        by_ref.append(row)
                        continue
                    if "ref_id" in row and row.get("ref_id"):
                        continue
                    if created is None or created < window_start or created > window_end:
                        continue
                    row_account = row.get("account_id")
                    if row_account is not None and row_account != account_id:
                        continue
                    if row.get("currency_pair_id") is None or "side" not in row or row.get("quantity") is None:
                        window_rows_complete = False
                        continue
                    if row.get("currency_pair_id") != pair_id or _norm_state(row.get("side")) != side:
                        continue
                    qty = _parse_decimal(row.get("quantity"))
                    if qty is None or requested_quantity is None:
                        window_rows_complete = False
                        continue
                    if _relative_close(qty, requested_quantity):
                        by_fields.append(row)
                if next_url is None:
                    complete = True
                    break
        except _OrderPrepError as e:
            return self._finish_clean(result, OUTCOME_UNKNOWN, f"order list read failed: {e}")
        matches = by_ref if by_ref else by_fields
        if len(matches) > 1:
            return self._finish_clean(result, OUTCOME_UNKNOWN, f"{len(matches)} orders match the attempt; the order is ambiguous")
        if len(matches) == 1:
            match = matches[0]
            found_id = match.get("id")
            if not isinstance(found_id, str) or not found_id.strip():
                return self._finish_clean(result, OUTCOME_UNKNOWN, "the matching order has no id")
            body, error = self._poll_order(session, urls, found_id, ref_id, side, deadline)
            if body is None:
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order {found_id}: {error}")
            return self._resolve_found(session, urls, found_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result)
        current_ms = now_ms if now_ms is not None else int(time.time() * 1000)
        if not complete:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order on the pages read, and the order list was not read to its end")
        if not window_rows_complete:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order, but some order rows in the settle window are unreadable")
        if current_ms < submitted_before_ms + _SETTLE_WINDOW_MS:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order yet; the settle window has not passed")
        return self._finish_clean(result, OUTCOME_NOT_FOUND, "no order with this ref_id or matching fields exists after the settle window")

    def _resolve_found(self, session, urls, order_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result):
        self._check_account(body, account_id)
        result["order"] = _order_evidence(body)
        outcome, detail = _evaluate_terminal(body, requested_quantity)
        if outcome is not None:
            return self._finish(result, outcome, detail, body)
        if not cancel_open:
            return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(body.get('state')) or 'in an unknown state'}", body)
        return self._follow_order(session, urls, order_id, ref_id, side, deadline, requested_quantity, result,
                                  cancel_open=True, first=body)

    def _verify_order_fields(self, body, pair_id, side, requested_quantity, submitted_after_ms, submitted_before_ms):
        if body.get("currency_pair_id") != pair_id:
            raise _OrderEvidenceError(f"order pair {body.get('currency_pair_id')!r} is not {pair_id!r}")
        if _norm_state(body.get("side")) != side:
            raise _OrderEvidenceError(f"order side {body.get('side')!r} is not {side!r}")
        qty = _parse_decimal(body.get("quantity"))
        if requested_quantity is None or qty is None or not _relative_close(qty, requested_quantity):
            raise _OrderEvidenceError(f"order quantity {body.get('quantity')!r} does not match the attempt's {requested_quantity!r}")
        created = _parse_created_at_ms(body.get("created_at"))
        if created is None or created < submitted_after_ms - _SETTLE_WINDOW_MS or created > submitted_before_ms + _SETTLE_WINDOW_MS:
            raise _OrderEvidenceError(f"order created_at {body.get('created_at')!r} is outside the attempt's submit window")

    def _check_account(self, body, account_id):
        row_account = body.get("account_id")
        if row_account is not None and row_account != account_id:
            raise _OrderEvidenceError(f"order belongs to account {row_account!r}, not the logged-in crypto account")

    def get_crypto_positions_strict(self, deadline=None) -> list:
        try:
            return self._read_crypto_holdings(deadline)
        except _HoldingsReadError as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} {e}") from e
        except Exception as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} unexpected {type(e).__name__}") from e

    def _read_crypto_holdings(self, deadline=None) -> list:
        import requests

        session, urls = self._logged_in_session(
            _HoldingsReadError, "Robinhood adapter not logged in — cannot fetch crypto positions")
        first_url = urls.crypto_holdings_url()

        holdings_host = urlsplit(first_url).netloc
        own_deadline = time.monotonic() + _HOLDINGS_TOTAL_DEADLINE_S
        deadline = own_deadline if deadline is None else min(own_deadline, deadline)
        requested = set()
        result = []
        seen_symbols = set()
        url = first_url
        page = 0
        while url:
            page += 1
            if page > _HOLDINGS_PAGE_CAP:
                raise _HoldingsReadError(f"more than {_HOLDINGS_PAGE_CAP} holdings pages")
            if url in requested:
                raise _HoldingsReadError(f"page {page}: repeated next link")
            requested.add(url)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise _HoldingsReadError(f"page {page}: read deadline of {_HOLDINGS_TOTAL_DEADLINE_S}s exceeded")
            timeout = (
                min(_HOLDINGS_CONNECT_TIMEOUT_S, remaining),
                min(_HOLDINGS_READ_TIMEOUT_S, remaining),
            )
            try:
                res = session.get(url, timeout=timeout, allow_redirects=False)
            except requests.exceptions.RequestException as e:
                raise _HoldingsReadError(f"page {page}: request failed ({type(e).__name__})") from e
            if res.status_code != 200:
                raise _HoldingsReadError(f"page {page}: HTTP status {res.status_code}")
            try:
                body = res.json()
            except ValueError as e:
                raise _HoldingsReadError(f"page {page}: response is not valid JSON") from e
            if not isinstance(body, dict):
                raise _HoldingsReadError(f"page {page}: response is not a JSON object")
            rows = body.get("results")
            if not isinstance(rows, list):
                raise _HoldingsReadError(f"page {page}: response has no results list")
            if "next" not in body:
                raise _HoldingsReadError(f"page {page}: response has no next key")
            next_url = body["next"]
            if next_url is None or next_url == "":
                next_url = None
            elif not isinstance(next_url, str):
                raise _HoldingsReadError(f"page {page}: next link is not a string")
            else:
                parts = urlsplit(next_url)
                if parts.scheme != "https" or parts.netloc != holdings_host:
                    raise _HoldingsReadError(f"page {page}: next link is not https on {holdings_host}")
            for index, row in enumerate(rows):
                parsed = _parse_holding_row(row, page, index)
                if parsed is None:
                    continue
                if parsed["symbol"] in seen_symbols:
                    raise _HoldingsReadError(
                        f"page {page} row {index}: duplicate holding for {parsed['symbol']}"
                    )
                seen_symbols.add(parsed["symbol"])
                result.append(parsed)
            url = next_url
        return result
