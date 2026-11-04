import math
import re
import sys

VOL_WINDOW = 14
MODEL_NEEDS_VOLATILITY = "volatility unavailable: model pricing needs measured volatility"

_STATUS_TEXT_RE = re.compile(
    r"(?<![\w.])(429|5\d\d)\s+(?:Client Error|Server Error|Too Many Requests|Internal Server Error"
    r"|Bad Gateway|Service Unavailable|Gateway Time-?out)",
    re.IGNORECASE,
)
_RATE_LIMIT_CLASSES = frozenset({"RateLimitExceeded", "DDoSProtection", "YFRateLimitError"})


class OptionDataUnavailable(Exception):

    def __init__(self, reason, vol_annual=None):
        super().__init__(reason)
        self.reason = reason
        self.vol_annual = vol_annual


def _status_code(value):
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    if value == 429 or 500 <= value <= 599:
        return value
    return None


def upstream_status_from_text(text):
    match = _STATUS_TEXT_RE.search(text or "")
    return int(match.group(1)) if match else None


def upstream_status(exc):
    seen = set()
    while exc is not None and id(exc) not in seen:
        seen.add(id(exc))
        code = _status_code(getattr(getattr(exc, "response", None), "status_code", None))
        if code is None:
            code = _status_code(getattr(exc, "status_code", None))
        if code is not None:
            return code
        if type(exc).__name__ in _RATE_LIMIT_CLASSES:
            return 429
        code = upstream_status_from_text(str(exc))
        if code is not None:
            return code
        exc = exc.__cause__ or exc.__context__
    return None


def unavailable(reason, exc=None, text=None, vol_annual=None):
    if isinstance(exc, OptionDataUnavailable):
        return exc
    code = upstream_status(exc) if exc is not None else None
    if code is None and text:
        code = upstream_status_from_text(text)
    if code is not None:
        reason = f"{reason} (upstream status {code})"
    return OptionDataUnavailable(reason, vol_annual=vol_annual)


def is_finite_positive(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(value) and value > 0


def require_finite_positive(value, reason):
    if isinstance(value, bool):
        raise OptionDataUnavailable(reason)
    try:
        number = float(value)
    except (TypeError, ValueError, OverflowError):
        raise OptionDataUnavailable(reason) from None
    if not math.isfinite(number) or number <= 0:
        raise OptionDataUnavailable(reason)
    return number


def daily_closes(rows):
    try:
        return [row[4] for row in rows]
    except (TypeError, IndexError, KeyError):
        raise OptionDataUnavailable("volatility unavailable: malformed daily bar") from None


def realized_vol_metrics(closes, periods_per_year):
    if not closes or len(closes) < VOL_WINDOW + 1:
        raise OptionDataUnavailable(f"volatility unavailable: fewer than {VOL_WINDOW + 1} daily closes")
    values = [require_finite_positive(c, "volatility unavailable: invalid daily close") for c in closes]
    returns = [math.log(values[i] / values[i - 1]) for i in range(1, len(values))]
    w = VOL_WINDOW
    mean = sum(returns[-w:]) / w
    variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
    vol = math.sqrt(variance) * math.sqrt(periods_per_year)
    vol_annual = round(vol, 4)
    if not math.isfinite(vol_annual) or vol_annual <= 0:
        raise OptionDataUnavailable("volatility unavailable: realized volatility is not positive")
    hvs = []
    for i in range(len(returns) - w + 1):
        chunk = returns[i:i + w]
        m = sum(chunk) / w
        v = sum((r - m) ** 2 for r in chunk) / w
        hvs.append(math.sqrt(v) * math.sqrt(periods_per_year) * 100)
    current_hv = vol * 100
    hv_min, hv_max = min(hvs), max(hvs)
    if not hv_max > hv_min:
        raise OptionDataUnavailable("iv rank unavailable: flat volatility range", vol_annual=vol_annual)
    iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100
    iv_rank = round(min(max(iv_rank, 0.0), 100.0), 1)
    if not math.isfinite(iv_rank):
        raise OptionDataUnavailable("iv rank unavailable: rank is not finite", vol_annual=vol_annual)
    return vol_annual, iv_rank


def require_model_inputs(spot, strike, dte, vol):
    vol = require_finite_positive(vol, MODEL_NEEDS_VOLATILITY)
    spot = require_finite_positive(spot, "option model unavailable: spot is not positive")
    strike = require_finite_positive(strike, "option model unavailable: strike is not positive")
    dte = require_finite_positive(dte, "option model unavailable: days to expiry are not positive")
    return spot, strike, dte, vol


def measured_vol_metrics(adapter, underlying):
    try:
        vol_annual, iv_rank = adapter.get_vol_metrics(underlying)
    except OptionDataUnavailable as e:
        print(f"volatility metrics unavailable for {underlying}: {e.reason}", file=sys.stderr)
        vol = e.vol_annual if is_finite_positive(e.vol_annual) else None
        return vol, None, e.reason
    if not is_finite_positive(vol_annual):
        return None, None, "volatility unavailable: adapter returned an invalid volatility"
    if isinstance(iv_rank, bool) or not isinstance(iv_rank, (int, float)) or not math.isfinite(iv_rank) or not 0 <= iv_rank <= 100:
        return vol_annual, None, "iv rank unavailable: adapter returned an invalid rank"
    return vol_annual, iv_rank, None
