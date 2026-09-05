MARKET_PAYLOAD_VERSION = 1


class MarketPayloadError(Exception):
    pass


def frame_key(symbol: str, timeframe: str) -> str:
    return f"{symbol}|{timeframe}"


def _fail(error_cls, message: str):
    raise (error_cls or MarketPayloadError)(message)


def validate_market_payload(market, error_cls=None) -> dict:
    if not isinstance(market, dict):
        _fail(error_cls, "market payload is missing or is not a JSON object")
    version = market.get("version")
    if int(version or 0) != MARKET_PAYLOAD_VERSION:
        _fail(error_cls, f"unsupported market payload version {version!r}")
    if not market.get("snapshot_id"):
        _fail(error_cls, "market payload has no snapshot_id")
    if not isinstance(market.get("frames"), dict):
        _fail(error_cls, "market payload has no frames object")
    return market


def market_frame(market, symbol: str, timeframe: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = frame_key(symbol, timeframe)
    frame = (payload.get("frames") or {}).get(key)
    if not isinstance(frame, dict):
        _fail(error_cls, f"market payload has no frame for {key}")
    if not frame.get("ready"):
        _fail(error_cls, f"market frame {key} is not ready")
    return frame


def market_frame_rows(market, symbol: str, timeframe: str, error_cls=None, limit: int = 0) -> list:
    frame = market_frame(market, symbol, timeframe, error_cls)
    rows = frame.get("rows")
    if not isinstance(rows, list) or not rows:
        _fail(error_cls, f"market frame {frame_key(symbol, timeframe)} carries no rows")
    for i, row in enumerate(rows):
        if not isinstance(row, list) or len(row) != 6:
            _fail(error_cls, f"market frame {frame_key(symbol, timeframe)} row {i} is malformed")
    if limit and len(rows) > limit:
        rows = rows[-limit:]
    return [[int(r[0]), float(r[1]), float(r[2]), float(r[3]), float(r[4]), float(r[5])] for r in rows]


def market_mid(market, coin: str, error_cls=None):
    payload = validate_market_payload(market, error_cls)
    entry = (payload.get("mids") or {}).get(coin)
    if not isinstance(entry, dict):
        return None
    if entry.get("stale"):
        return None
    px = entry.get("px")
    try:
        px = float(px)
    except (TypeError, ValueError):
        return None
    return px if px > 0 else None


def market_funding(market, coin: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    entry = (payload.get("funding") or {}).get(coin)
    if not isinstance(entry, dict):
        _fail(error_cls, f"market payload carries no funding for {coin}")
    if entry.get("error"):
        _fail(error_cls, f"market payload funding for {coin} failed: {entry['error']}")
    return entry


def market_funding_scalar(market, coin: str, error_cls=None) -> dict:
    entry = market_funding(market, coin, error_cls)
    if not entry.get("has_scalar"):
        _fail(error_cls, f"market payload carries no current funding rate for {coin}")
    return {
        "current_funding_rate": float(entry.get("current") or 0.0),
        "avg_funding_rate_7d": float(entry.get("avg_7d") or 0.0),
    }


def market_funding_records(market, coin: str, start_ms: int, error_cls=None) -> list:
    entry = market_funding(market, coin, error_cls)
    if not entry.get("has_records"):
        _fail(error_cls, f"market payload carries no funding history for {coin}")
    out = []
    for record in entry.get("records") or []:
        if not isinstance(record, dict):
            _fail(error_cls, f"market payload funding record for {coin} is malformed")
        ts = int(record.get("time") or 0)
        if ts < int(start_ms):
            continue
        out.append({"rate": float(record.get("rate") or 0.0), "time": ts})
    return out
