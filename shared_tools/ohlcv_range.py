import math


_UNIT_MS = {"m": 60_000, "h": 3_600_000, "d": 86_400_000, "w": 604_800_000}

RANGE_MAX_REQUESTS = 64


def timeframe_ms(timeframe: str) -> int:
    tf = str(timeframe or "").strip()
    if len(tf) < 2 or tf[-1] not in _UNIT_MS or not tf[:-1].isdigit():
        raise ValueError(f"unsupported candle timeframe {timeframe!r}")
    n = int(tf[:-1])
    if n <= 0:
        raise ValueError(f"unsupported candle timeframe {timeframe!r}")
    return n * _UNIT_MS[tf[-1]]


def check_range_bounds(start_ms, end_ms):
    for name, value in (("start", start_ms), ("end", end_ms)):
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError(f"candle range {name} must be integer milliseconds, got {value!r}")
    if start_ms < 0 or end_ms < start_ms:
        raise ValueError(f"candle range [{start_ms}, {end_ms}] is empty or negative")


def range_request_budget(start_ms: int, end_ms: int, interval_ms: int, per_request: int) -> int:
    bars = (end_ms - start_ms) // interval_ms + 1
    return max(1, min(RANGE_MAX_REQUESTS, math.ceil(bars / max(1, per_request)) + 4))


def page_ccxt_ohlcv(exchange, pair: str, timeframe: str, start_ms: int, end_ms: int,
                    page_limit: int = 300, max_requests: int = None) -> dict:
    check_range_bounds(start_ms, end_ms)
    interval_ms = timeframe_ms(timeframe)
    if max_requests is None:
        max_requests = range_request_budget(start_ms, end_ms, interval_ms, page_limit)
    rows = []
    cursor = start_ms
    requests = 0
    truncated = False
    stalled = False
    while cursor <= end_ms:
        if requests >= max_requests:
            truncated = True
            break
        requests += 1
        page = exchange.fetch_ohlcv(pair, timeframe, since=cursor, limit=page_limit)
        if not page:
            break
        page_last = None
        for raw in page:
            row = list(raw)
            t = int(row[0])
            if page_last is None or t > page_last:
                page_last = t
            if cursor <= t <= end_ms:
                rows.append(row)
        if page_last is None or page_last < cursor:
            stalled = True
            break
        cursor = page_last + interval_ms
    return {"rows": rows, "requests": requests, "truncated": truncated, "stalled": stalled}
