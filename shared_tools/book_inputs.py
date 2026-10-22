import re
from fractions import Fraction

KIND = "l2_book"
CONTRACT = {
    "source": "hyperliquid_ws_l2Book",
    "contract": "hyperliquid-l2book-native-slow/1635-v1",
    "time_semantics": "source_event_ms+first_receipt_ms/1635-v1",
    "identity_semantics": "sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1",
    "max_future_skew_ms": 250,
    "max_lag_ms": 5000,
    "max_gap_ms": 10000,
}
ARCHIVE_TIME_SEMANTICS = "archiver_receipt_ns_floor_ms/1635-v1"
MAX_LEVELS = 20
MAX_ORDERS = 4_294_967_295
MARKER_REASONS = frozenset({"epoch_start", "subscribed", "subscription_failed", "disconnected", "invalid_frame", "conflict",
                            "out_of_order", "clock_regression", "silence", "evidence_loss", "depth_raised"})
BOOK_INPUT_STRATEGIES = frozenset({"order_book_imbalance"})
_DECIMAL_RE = re.compile(r"^(0|[1-9][0-9]*)(\.[0-9]*[1-9])?$")
_HEX64_RE = re.compile(r"^[0-9a-f]{64}$")


class BookInputError(ValueError):
    pass


def refuse_book_input_strategy(name, consumer: str) -> None:
    if str(name or "").strip() in BOOK_INPUT_STRATEGIES:
        raise ValueError(
            f"{str(name).strip()} decides from supplied l2Book depth observations at one-second deadlines; {consumer} "
            "cannot supply them, so it is refused before any data is read or scored. Use the dedicated book-event "
            "driver (backtest/book_event_simulator.py) on a verified archive or recording")


def is_book_input_strategy(name) -> bool:
    return str(name or "").strip() in BOOK_INPUT_STRATEGIES


def _int(v, what: str, lo: int = 0, hi: int = (1 << 63) - 1) -> int:
    if isinstance(v, bool) or not isinstance(v, int) or not lo <= v <= hi:
        raise BookInputError(f"{what} {v!r} is not an integer in [{lo}, {hi}]")
    return v


def canonical_decimal(text, what: str) -> str:
    if not isinstance(text, str) or len(text) > 32 or not _DECIMAL_RE.match(text) or text == "0":
        raise BookInputError(f"{what} {text!r} is not a canonical positive decimal")
    if "." in text and len(text.split(".", 1)[1]) > 18:
        raise BookInputError(f"{what} {text!r} has more than 18 fractional digits")
    return text


def _levels(raw, what: str) -> list:
    if not isinstance(raw, list) or len(raw) > MAX_LEVELS:
        raise BookInputError(f"{what} is not a list of at most {MAX_LEVELS} levels")
    out = []
    for i, lv in enumerate(raw):
        if not isinstance(lv, list) or len(lv) != 3:
            raise BookInputError(f"{what} level {i} is not [px, sz, n]")
        px = canonical_decimal(lv[0], f"{what} level {i} px")
        sz = canonical_decimal(lv[1], f"{what} level {i} sz")
        n = _int(lv[2], f"{what} level {i} n", 1, MAX_ORDERS)
        out.append((px, sz, n))
    return out


def _check_sides(bids: list, asks: list, what: str) -> None:
    for i in range(1, len(bids)):
        if Fraction(bids[i][0]) >= Fraction(bids[i - 1][0]):
            raise BookInputError(f"{what} bids are not strictly descending at {i}")
    for i in range(1, len(asks)):
        if Fraction(asks[i][0]) <= Fraction(asks[i - 1][0]):
            raise BookInputError(f"{what} asks are not strictly ascending at {i}")
    if bids and asks and Fraction(bids[0][0]) >= Fraction(asks[0][0]):
        raise BookInputError(f"{what} is crossed or locked")


def parse_observation(o, what: str) -> dict:
    if not isinstance(o, dict):
        raise BookInputError(f"{what} is not an object")
    bids = _levels(o.get("bids"), f"{what} bids")
    asks = _levels(o.get("asks"), f"{what} asks")
    bid_depth = _int(o.get("bid_depth"), f"{what} bid_depth", 1, MAX_LEVELS)
    ask_depth = _int(o.get("ask_depth"), f"{what} ask_depth", 1, MAX_LEVELS)
    if len(bids) > bid_depth or len(asks) > ask_depth or not bids or not asks:
        raise BookInputError(f"{what} carries more levels than its declared depth or an empty side")
    _check_sides(bids, asks, what)
    identity = o.get("identity")
    if not isinstance(identity, str) or not _HEX64_RE.match(identity):
        raise BookInputError(f"{what} identity is not 64 lowercase hex characters")
    return {"seq": _int(o.get("seq"), f"{what} seq", 1), "event_ms": _int(o.get("event_ms"), f"{what} event_ms", 1),
            "recv_ms": _int(o.get("recv_ms"), f"{what} recv_ms", 1), "session": _int(o.get("session"), f"{what} session"),
            "identity": identity, "bid_depth": bid_depth, "ask_depth": ask_depth, "bids": bids, "asks": asks}


def parse_marker(m, what: str) -> dict:
    if not isinstance(m, dict):
        raise BookInputError(f"{what} is not an object")
    reason = m.get("reason")
    if reason not in MARKER_REASONS:
        raise BookInputError(f"{what} reason {reason!r} is not a contract marker")
    detail = m.get("detail", "")
    if not isinstance(detail, str):
        raise BookInputError(f"{what} detail is not a string")
    return {"seq": _int(m.get("seq"), f"{what} seq", 1), "at_ms": _int(m.get("at_ms"), f"{what} at_ms", 1),
            "reason": reason, "session": _int(m.get("session"), f"{what} session"), "detail": detail}


def unavailable(coin: str, reason: str, detail: str = "") -> dict:
    return {"available": False, "coin": coin, "reason": reason, "detail": detail}


def parse_book_input(section, coin: str, accept_time_semantics=(CONTRACT["time_semantics"],)) -> dict:
    if section is None:
        return unavailable(coin, "book_input_missing", "no l2_book section was supplied")
    if not isinstance(section, dict):
        return unavailable(coin, "book_malformed", "the l2_book section is not an object")
    if not section.get("available"):
        return unavailable(coin, "book_unavailable", str(section.get("reason") or "the feed marked the section unavailable"))
    try:
        if section.get("kind") != KIND or section.get("coin") != coin:
            raise BookInputError(f"section is labelled {section.get('coin')!r}|{section.get('kind')!r}")
        for k, v in CONTRACT.items():
            if k == "time_semantics":
                if section.get(k) not in accept_time_semantics:
                    raise BookInputError(f"time_semantics {section.get(k)!r} is not accepted")
            elif section.get(k) != v:
                raise BookInputError(f"{k} {section.get(k)!r} is outside the accepted contract")
        cutoff = _int(section.get("cutoff_ms"), "cutoff_ms", 1)
        window = _int(section.get("window_ms"), "window_ms", 1, 900_000)
        depth = _int(section.get("depth"), "depth", 1, MAX_LEVELS)
        capacity = _int(section.get("capacity"), "capacity", 1, 4096)
        retained = _int(section.get("retained_from_ms"), "retained_from_ms")
        prefix = section.get("prefix_complete")
        if not isinstance(prefix, bool):
            raise BookInputError("prefix_complete is not a boolean")
        raw_obs = section.get("observations")
        raw_marks = section.get("markers")
        if not isinstance(raw_obs, list) or not isinstance(raw_marks, list) or len(raw_obs) > capacity:
            raise BookInputError("observations and markers must be lists within capacity")
        obs = [parse_observation(o, f"observation {i}") for i, o in enumerate(raw_obs)]
        marks = [parse_marker(m, f"marker {i}") for i, m in enumerate(raw_marks)]
        start = cutoff - window
        for i, o in enumerate(obs):
            if not start <= o["recv_ms"] <= cutoff:
                raise BookInputError(f"observation {i} receipt {o['recv_ms']} lies outside the window")
            if o["recv_ms"] - o["event_ms"] > CONTRACT["max_lag_ms"] or o["event_ms"] - o["recv_ms"] > CONTRACT["max_future_skew_ms"]:
                raise BookInputError(f"observation {i} breaks the time bounds")
            if len(o["bids"]) > depth or len(o["asks"]) > depth:
                raise BookInputError(f"observation {i} carries more levels than the section depth")
            if i and not (o["seq"] > obs[i - 1]["seq"] and o["event_ms"] > obs[i - 1]["event_ms"] and o["recv_ms"] >= obs[i - 1]["recv_ms"]):
                raise BookInputError(f"observation {i} is out of sequence, event or receipt order")
        if len({o["identity"] for o in obs}) != len(obs):
            raise BookInputError("observations repeat a source identity")
        for i, m in enumerate(marks):
            if not start <= m["at_ms"] <= cutoff:
                raise BookInputError(f"marker {i} lies outside the window")
            if i and m["seq"] <= marks[i - 1]["seq"]:
                raise BookInputError(f"marker {i} is out of sequence")
    except BookInputError as exc:
        return unavailable(coin, "book_malformed", str(exc))
    return {"available": True, "coin": coin, "cutoff_ms": cutoff, "window_ms": window, "depth": depth, "capacity": capacity,
            "retained_from_ms": retained, "prefix_complete": prefix, "instance": str(section.get("owner_instance") or ""),
            "time_semantics": section.get("time_semantics"), "observations": obs, "markers": marks}


def side_notional(levels: list, depth: int) -> Fraction:
    total = Fraction(0)
    for px, sz, _ in levels[:depth]:
        total += Fraction(px) * Fraction(sz)
    return total


def book_features(obs: dict, depth: int):
    if len(obs["bids"]) < depth or len(obs["asks"]) < depth:
        return None
    b = side_notional(obs["bids"], depth)
    a = side_notional(obs["asks"], depth)
    bid, ask = Fraction(obs["bids"][0][0]), Fraction(obs["asks"][0][0])
    total = b + a
    return {"B": b, "A": a, "total": total, "imbalance": (b - a) / total if total > 0 else None,
            "spread_bps": Fraction(10000) * (ask - bid) / ((ask + bid) / 2), "best_bid": bid, "best_ask": ask}
