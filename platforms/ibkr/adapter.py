
import sys
import os as _os
import math
import json
from datetime import date, datetime, timedelta
from typing import Optional, Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import egress_policy
import option_data
from pricing import bs_price_and_greeks

CME_SPECS = {
    "BTC": {"interval": 1000},
    "ETH": {"interval": 50},
}
DEFAULT_SPECS = {"interval": 100}

CALENDAR_CONTRACT = "option_calendar_v1"
MARK_SOURCE = "binanceus_black_scholes_hv14"
SETTLEMENT_SOURCE = "binanceus_spot_vwap_1530_1600_london"
SETTLEMENT_BASIS = "spot_candle_close_vwap"
SETTLEMENT_WINDOW_MINUTES = 30
SETTLEMENT_SYMBOL_SUFFIXES = ("/USDT", "/USD")

WEEKLY_WEEKDAYS = (0, 1, 2, 3)
FRIDAY = 4
FRIDAY_WEEKLY_SERIES = 4
CONSECUTIVE_MONTHS = 6
EXTRA_QUARTERLY_MONTHS = 4
QUARTERLY_MONTHS = (3, 6, 9, 12)
ROLLOVER_BUSINESS_DAYS = 2

_MINUTE_MS = 60_000


class CalendarContextError(ValueError):

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _exchange():
    import ccxt
    return egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))


def _get_spot_price(underlying: str) -> float:
    exchange = _exchange()
    for suffix in ("/USDT", "/USD"):
        try:
            ticker = exchange.fetch_ticker(underlying + suffix)
            price = ticker.get("last") or 0
            if price and price > 0:
                return float(price)
        except Exception:
            continue
    return 0.0


def _calc_vol_and_iv_rank(underlying: str) -> Tuple[float, float]:
    try:
        ohlcv = _exchange().fetch_ohlcv(underlying + "/USDT", "1d", limit=90)
    except Exception as e:
        raise option_data.unavailable("volatility unavailable: daily history read failed", e) from e
    return option_data.realized_vol_metrics(option_data.daily_closes(ohlcv), 365)


def _int_field(doc, name, low, high):
    value = doc.get(name)
    if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
        raise CalendarContextError(f"option calendar context {name} must be an integer from {low} to {high}")
    return value


def parse_calendar_context(raw, platform, underlying):
    if not isinstance(raw, str) or raw.strip() == "":
        raise CalendarContextError("option calendar context is empty")
    try:
        doc = json.loads(raw)
    except (ValueError, RecursionError):
        raise CalendarContextError("option calendar context is not valid JSON") from None
    if not isinstance(doc, dict):
        raise CalendarContextError("option calendar context must be a JSON object")
    if doc.get("contract") != CALENDAR_CONTRACT:
        raise CalendarContextError(f"option calendar context contract is not {CALENDAR_CONTRACT}")
    if doc.get("platform") != platform:
        raise CalendarContextError(f"option calendar context platform {doc.get('platform')!r} does not match {platform!r}")
    if not isinstance(doc.get("underlying"), str) or doc["underlying"].upper() != (underlying or "").upper():
        raise CalendarContextError(f"option calendar context underlying {doc.get('underlying')!r} does not match {underlying!r}")
    text = doc.get("evaluated_at")
    if not isinstance(text, str) or text.strip() == "":
        raise CalendarContextError("option calendar context evaluated_at is missing")
    try:
        evaluated_at = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        raise CalendarContextError(f"option calendar context evaluated_at {text!r} is not an ISO timestamp") from None
    if evaluated_at.tzinfo is None:
        raise CalendarContextError("option calendar context evaluated_at has no time zone")
    zone_name = doc.get("cutoff_zone")
    if not isinstance(zone_name, str) or zone_name.strip() == "":
        raise CalendarContextError("option calendar context cutoff_zone is missing")
    try:
        from zoneinfo import ZoneInfo
        zone = ZoneInfo(zone_name)
    except Exception:
        raise CalendarContextError(f"option calendar context cutoff_zone {zone_name!r} is not a known time zone") from None
    hour = _int_field(doc, "cutoff_hour", 0, 23)
    minute = _int_field(doc, "cutoff_minute", 0, 59)
    return {"platform": platform, "underlying": underlying.upper(), "evaluated_at": evaluated_at,
            "zone": zone, "hour": hour, "minute": minute}


def _add_months(year, month, n):
    index = year * 12 + (month - 1) + n
    return index // 12, index % 12 + 1


def _last_friday(year, month):
    ny, nm = _add_months(year, month, 1)
    day = date(ny, nm, 1) - timedelta(days=1)
    return day - timedelta(days=(day.weekday() - FRIDAY) % 7)


def _business_days_before(day, n):
    out = day
    while n > 0:
        out -= timedelta(days=1)
        if out.weekday() < 5:
            n -= 1
    return out


def _next_weekday_on_or_after(day, weekday):
    return day + timedelta(days=(weekday - day.weekday()) % 7)


def candidate_expiries(ctx):
    zone, now = ctx["zone"], ctx["evaluated_at"]
    today = now.astimezone(zone).date()

    def live(day):
        return datetime(day.year, day.month, day.day, ctx["hour"], ctx["minute"], tzinfo=zone) > now

    year, month = today.year, today.month
    if not live(_last_friday(year, month)):
        year, month = _add_months(year, month, 1)
    months = [_add_months(year, month, i) for i in range(CONSECUTIVE_MONTHS)]
    ny, nm = months[-1]
    while len(months) < CONSECUTIVE_MONTHS + EXTRA_QUARTERLY_MONTHS:
        ny, nm = _add_months(ny, nm, 1)
        if nm in QUARTERLY_MONTHS:
            months.append((ny, nm))
    decembers = [ym for ym in months if ym[1] == 12]
    if len(decembers) == 1:
        months.append((decembers[0][0] + 1, 12))
    monthly = {_last_friday(y, m) for (y, m) in months}

    weekly = set()
    for weekday in WEEKLY_WEEKDAYS:
        first = _next_weekday_on_or_after(today, weekday)
        if not live(first):
            first += timedelta(days=7)
        weekly.add(first)
        if today >= _business_days_before(first, ROLLOVER_BUSINESS_DAYS):
            weekly.add(first + timedelta(days=7))

    fridays = []
    friday = _next_weekday_on_or_after(today, FRIDAY)
    if not live(friday):
        friday += timedelta(days=7)
    while len(fridays) < FRIDAY_WEEKLY_SERIES + 1:
        if friday not in monthly:
            fridays.append(friday)
        friday += timedelta(days=7)
    if today < _business_days_before(fridays[0], ROLLOVER_BUSINESS_DAYS):
        fridays = fridays[:FRIDAY_WEEKLY_SERIES]
    weekly.update(fridays)

    out = sorted(d for d in monthly | weekly if live(d) and d.weekday() < 5)
    return today, out


def select_expiry(ctx, target_dte):
    today, candidates = candidate_expiries(ctx)
    if not candidates:
        raise option_data.OptionDataUnavailable("expiry calendar unavailable: no listed candidate expiry")
    best = min(candidates, key=lambda d: (abs((d - today).days - target_dte), -d.toordinal()))
    return best.isoformat(), (best - today).days


def _finite(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def settlement_window_vwap(rows, start_ms, minutes=SETTLEMENT_WINDOW_MINUTES):
    end_ms = start_ms + minutes * _MINUTE_MS
    if not isinstance(rows, (list, tuple)):
        return None, "settlement window unavailable: candle reply is not a list"
    window = {}
    for row in rows:
        if not isinstance(row, (list, tuple)) or len(row) < 6:
            return None, "settlement window unavailable: malformed candle"
        ts = row[0]
        if isinstance(ts, bool) or not isinstance(ts, (int, float)) or not math.isfinite(ts):
            return None, "settlement window unavailable: malformed candle timestamp"
        if ts < start_ms or ts >= end_ms:
            continue
        if ts != int(ts) or (int(ts) - start_ms) % _MINUTE_MS != 0:
            return None, f"settlement window unavailable: candle timestamp {ts} is not on the minute grid"
        ts = int(ts)
        if ts in window:
            return None, f"settlement window unavailable: duplicate candle at {ts}"
        window[ts] = row
    expected = [start_ms + i * _MINUTE_MS for i in range(minutes)]
    missing = [ts for ts in expected if ts not in window]
    if missing:
        return None, f"settlement window unavailable: {len(missing)} of {minutes} minute candles missing"
    weighted = 0.0
    volume = 0.0
    for ts in expected:
        close, vol = window[ts][4], window[ts][5]
        if not _finite(close) or close <= 0:
            return None, f"settlement window unavailable: candle close at {ts} is not a finite positive price"
        if not _finite(vol) or vol < 0:
            return None, f"settlement window unavailable: candle volume at {ts} is not a finite non-negative amount"
        weighted += float(close) * float(vol)
        volume += float(vol)
    if not math.isfinite(volume) or volume <= 0:
        return None, "settlement window unavailable: total window volume is zero"
    price = weighted / volume
    if not math.isfinite(price) or price <= 0:
        return None, "settlement window unavailable: weighted close is not a finite positive price"
    return price, ""


class IBKRExchangeAdapter:

    mark_source = MARK_SOURCE
    settlement_source = SETTLEMENT_SOURCE
    settlement_basis = SETTLEMENT_BASIS

    def __init__(self, calendar=None):
        self._calendar = calendar

    @property
    def name(self) -> str:
        return "ibkr"

    def get_spot_price(self, underlying: str) -> float:
        return _get_spot_price(underlying)

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        return _calc_vol_and_iv_rank(underlying)

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        ctx = self._calendar
        if ctx is None:
            raise option_data.OptionDataUnavailable(
                "expiry calendar unavailable: no option calendar context from the scheduler (--option-calendar-json)")
        if ctx["underlying"] != underlying.upper():
            raise option_data.OptionDataUnavailable(
                f"expiry calendar unavailable: calendar context is for {ctx['underlying']}, not {underlying.upper()}")
        return select_expiry(ctx, target_dte)

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        specs = CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)
        interval = specs["interval"]
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: Optional[float] = None) -> Tuple[float, float, dict]:
        spot, strike, dte, vol = option_data.require_model_inputs(spot, strike, dte, vol)
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        mark_pct = price_usd / spot
        return round(mark_pct, 6), round(price_usd, 2), greeks

    def get_option_mark(self, underlying: str, option_type: str, strike: float, expiry: str,
                        dte: float, spot: float, vol: Optional[float] = None) -> Tuple[float, dict, str]:
        spot, strike, dte, vol = option_data.require_model_inputs(spot, strike, dte, vol)
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        if not _finite(price_usd) or price_usd < 0:
            raise option_data.OptionDataUnavailable("option model unavailable: black-scholes price is not a finite non-negative amount")
        try:
            greeks = {k: float(greeks[k]) for k in ("delta", "gamma", "theta", "vega")}
        except (KeyError, TypeError, ValueError):
            raise option_data.OptionDataUnavailable("option model unavailable: black-scholes greeks are missing") from None
        if not all(math.isfinite(v) for v in greeks.values()):
            raise option_data.OptionDataUnavailable("option model unavailable: black-scholes greeks are not finite")
        return float(price_usd), greeks, MARK_SOURCE

    def get_settlement_price(self, underlying: str, expiry: str, cutoff: datetime) -> Tuple[Optional[float], str]:
        end_ms = int(cutoff.timestamp() * 1000)
        start_ms = end_ms - SETTLEMENT_WINDOW_MINUTES * _MINUTE_MS
        if start_ms % _MINUTE_MS != 0:
            return None, f"settlement window unavailable: cutoff {cutoff.isoformat()} is not on a whole minute"
        reasons = []
        exchange = None
        for suffix in SETTLEMENT_SYMBOL_SUFFIXES:
            symbol = underlying.upper() + suffix
            try:
                if exchange is None:
                    exchange = _exchange()
                rows = exchange.fetch_ohlcv(symbol, "1m", since=start_ms, limit=SETTLEMENT_WINDOW_MINUTES)
            except Exception as e:
                code = option_data.upstream_status(e)
                why = f"{symbol} minute candle read failed: {type(e).__name__}"
                reasons.append(f"{why} (upstream status {code})" if code is not None else why)
                continue
            price, why = settlement_window_vwap(rows, start_ms)
            if price is not None:
                return price, ""
            reasons.append(f"{symbol} {why}")
        return None, (f"no complete {SETTLEMENT_SOURCE} window for {underlying.upper()} expiry {expiry}: "
                      + "; ".join(reasons))

    def get_strike_interval(self, underlying: str) -> float:
        return CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)["interval"]
