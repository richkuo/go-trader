MARKET_PAYLOAD_VERSION = 4


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


def market_frame_rows_with_timing(market, symbol: str, timeframe: str, error_cls=None, limit: int = 0):
    frame = market_frame(market, symbol, timeframe, error_cls)
    rows = market_frame_rows(market, symbol, timeframe, error_cls)
    timing = frame.get("timing")
    if isinstance(timing, dict) and isinstance(timing.get("bars"), list):
        bars = timing["bars"]
        if len(bars) == len(rows) and limit and len(rows) > limit:
            timing = {**timing, "bars": bars[-limit:]}
    if limit and len(rows) > limit:
        rows = rows[-limit:]
    return rows, timing


def market_decision_cutoff_ms(market, error_cls=None) -> int:
    payload = validate_market_payload(market, error_cls)
    try:
        return int(payload.get("decision_cutoff_ms") or 0)
    except (TypeError, ValueError):
        return 0


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


def market_observation(market, coin: str, kind: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|{kind}"
    observations = payload.get("observations")
    if not isinstance(observations, dict) or key not in observations:
        return {"available": False, "kind": kind, "coin": coin,
                "reason": f"market payload carries no {key} observations"}
    entry = observations[key]
    if not isinstance(entry, dict):
        return {"available": False, "kind": kind, "coin": coin,
                "reason": f"market payload {key} observations are not an object"}
    if entry.get("kind") != kind or entry.get("coin") != coin:
        return {"available": False, "kind": kind, "coin": coin,
                "reason": f"market payload {key} observations are labelled "
                          f"{entry.get('coin')!r}|{entry.get('kind')!r}"}
    return dict(entry)


def market_trade_flow(market, coin: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|trade_flow"
    flows = payload.get("trade_flows")
    if not isinstance(flows, dict) or key not in flows:
        return {"available": False, "kind": "trade_flow", "coin": coin,
                "reason": f"market payload carries no {key} aggregates"}
    entry = flows[key]
    if not isinstance(entry, dict):
        return {"available": False, "kind": "trade_flow", "coin": coin,
                "reason": f"market payload {key} aggregates are not an object"}
    if entry.get("kind") != "trade_flow" or entry.get("coin") != coin:
        return {"available": False, "kind": "trade_flow", "coin": coin,
                "reason": f"market payload {key} aggregates are labelled "
                          f"{entry.get('coin')!r}|{entry.get('kind')!r}"}
    return dict(entry)


def market_volume_profile(market, coin: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|volume_profile"
    profiles = payload.get("volume_profiles")
    if not isinstance(profiles, dict) or key not in profiles:
        return {"available": False, "kind": "volume_profile", "coin": coin,
                "reason": f"market payload carries no {key} entry"}
    entry = profiles[key]
    if not isinstance(entry, dict):
        return {"available": False, "kind": "volume_profile", "coin": coin,
                "reason": f"market payload {key} entry is not an object"}
    if entry.get("kind") != "volume_profile" or entry.get("coin") != coin:
        return {"available": False, "kind": "volume_profile", "coin": coin,
                "reason": f"market payload {key} entry is labelled "
                          f"{entry.get('coin')!r}|{entry.get('kind')!r}"}
    return dict(entry)


L2_BOOK_CONTRACT = {
    "source": "hyperliquid_ws_l2Book",
    "contract": "hyperliquid-l2book-native-slow/1635-v1",
    "time_semantics": "source_event_ms+first_receipt_ms/1635-v1",
    "identity_semantics": "sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1",
    "max_future_skew_ms": 250,
    "max_lag_ms": 5000,
    "max_gap_ms": 10000,
}


def market_l2_book(market, coin: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|l2_book"
    refused = payload.get("l2_books_refused")
    if refused:
        return {"available": False, "kind": "l2_book", "coin": coin, "reason": str(refused)}
    books = payload.get("l2_books")
    if not isinstance(books, dict) or key not in books:
        return {"available": False, "kind": "l2_book", "coin": coin,
                "reason": f"market payload carries no {key} records"}
    entry = books[key]
    if not isinstance(entry, dict):
        return {"available": False, "kind": "l2_book", "coin": coin,
                "reason": f"market payload {key} records are not an object"}
    if entry.get("kind") != "l2_book" or entry.get("coin") != coin:
        return {"available": False, "kind": "l2_book", "coin": coin,
                "reason": f"market payload {key} records are labelled "
                          f"{entry.get('coin')!r}|{entry.get('kind')!r}"}
    drift = sorted(k for k, v in L2_BOOK_CONTRACT.items() if entry.get(k) != v)
    if drift:
        return {"available": False, "kind": "l2_book", "coin": coin,
                "reason": f"market payload {key} declares {', '.join(drift)} outside the accepted l2_book contract"}
    if not isinstance(entry.get("observations"), list) or not isinstance(entry.get("markers"), list):
        return {"available": False, "kind": "l2_book", "coin": coin,
                "reason": f"market payload {key} carries no observation and marker lists"}
    return dict(entry)


def market_macro_events(market, calendar_sha256: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    section = payload.get("macro_events")
    if not isinstance(section, dict) or calendar_sha256 not in section:
        return {"kind": "macro_calendar", "available": False, "calendar_sha256": calendar_sha256,
                "reason": f"market payload carries no macro_events entry for calendar {calendar_sha256 or '(none)'}"}
    entry = section[calendar_sha256]
    if not isinstance(entry, dict):
        return {"kind": "macro_calendar", "available": False, "calendar_sha256": calendar_sha256,
                "reason": f"market payload macro_events entry {calendar_sha256} is not an object"}
    if entry.get("kind") != "macro_calendar" or entry.get("calendar_sha256") != calendar_sha256:
        return {"kind": "macro_calendar", "available": False, "calendar_sha256": calendar_sha256,
                "reason": f"market payload macro_events entry {calendar_sha256} is labelled "
                          f"{entry.get('kind')!r}/{entry.get('calendar_sha256')!r}"}
    return dict(entry)


def market_macro_quotes(market, coin: str, quote_sha256: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|{quote_sha256}"
    section = payload.get("macro_quotes")
    if not isinstance(section, dict) or key not in section:
        return {"kind": "macro_quotes", "available": False, "coin": coin, "quote_sha256": quote_sha256,
                "reason": f"market payload carries no macro_quotes entry for {key}"}
    entry = section[key]
    if not isinstance(entry, dict):
        return {"kind": "macro_quotes", "available": False, "coin": coin, "quote_sha256": quote_sha256,
                "reason": f"market payload macro_quotes entry {key} is not an object"}
    if entry.get("kind") != "macro_quotes" or entry.get("coin") != coin or entry.get("quote_sha256") != quote_sha256:
        return {"kind": "macro_quotes", "available": False, "coin": coin, "quote_sha256": quote_sha256,
                "reason": f"market payload macro_quotes entry {key} is labelled "
                          f"{entry.get('kind')!r}/{entry.get('coin')!r}/{entry.get('quote_sha256')!r}"}
    return dict(entry)


def market_premium(market, coin: str, error_cls=None) -> dict:
    payload = validate_market_payload(market, error_cls)
    key = f"{coin}|perp_premium"
    section = payload.get("perp_premiums")
    if not isinstance(section, dict) or key not in section:
        return {"available": False, "kind": "perp_premium", "coin": coin,
                "reason": f"market payload carries no {key} records"}
    entry = section[key]
    if not isinstance(entry, dict):
        return {"available": False, "kind": "perp_premium", "coin": coin,
                "reason": f"market payload {key} records are not an object"}
    if entry.get("kind") != "perp_premium" or entry.get("coin") != coin:
        return {"available": False, "kind": "perp_premium", "coin": coin,
                "reason": f"market payload {key} records are labelled "
                          f"{entry.get('coin')!r}|{entry.get('kind')!r}"}
    return dict(entry)
