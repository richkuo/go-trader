import hashlib
import re
from fractions import Fraction

KIND = "volume_profile"
SCHEMA = "go_trader_volume_profile/v1"
CONTRACT = {
    "schema": SCHEMA,
    "source": "hyperliquid_ws_trades",
    "units": "base",
    "identity": "venue+namespace+coin+time+tid",
    "time_basis": "event_time_receipt_cutoff",
    "session_ms": 86_400_000,
    "interval_ms": 60_000,
    "reorder_grace_ms": 5_000,
    "decimal_scale": 8,
    "max_entries": 16_384,
    "max_price_chars": 24,
    "max_pair_bytes": 1 << 20,
}
PROXY_SOURCE = "candle_typical_price_proxy/1665"
SOURCES = (CONTRACT["source"], PROXY_SOURCE)
SESSION_MS = CONTRACT["session_ms"]
MINUTES = SESSION_MS // CONTRACT["interval_ms"]
WINDOW_MS = 2 * SESSION_MS
DUAL_WINDOW_MS = 3_600_000
SCALE = 10 ** CONTRACT["decimal_scale"]
INT64_MAX = (1 << 63) - 1
MAX_REASON_CHARS = 240

STATUS_AVAILABLE = "available"
STATUS_INVALIDATED = "invalidated"
STATUS_PENDING = "pending"
STATUS_UNAVAILABLE = "unavailable"
STATUSES = (STATUS_AVAILABLE, STATUS_INVALIDATED, STATUS_PENDING, STATUS_UNAVAILABLE)

_HEX64 = re.compile(r"^[0-9a-f]{64}$")
ARTIFACT_KEYS = ("session_start_ms", "session_end_ms", "trades", "volume", "trades_hash", "first_event_ms",
                 "last_event_ms", "complete_minutes", "available_ms", "sha256", "prices")
SESSION_KEYS = ("session_start_ms", "status", "reason_code", "reason", "invalid_ms", "artifact")


class ProfileInputError(ValueError):
    pass


def session_start(ms: int) -> int:
    return ms - ((ms % SESSION_MS) + SESSION_MS) % SESSION_MS


def target_session(cutoff_ms: int) -> int:
    return session_start(cutoff_ms - 1) - SESSION_MS


def sealed_sessions(cutoff_ms: int) -> list:
    target = target_session(cutoff_ms)
    if cutoff_ms - session_start(cutoff_ms - 1) < DUAL_WINDOW_MS:
        return [target - SESSION_MS, target]
    return [target]


def is_canonical_decimal(text) -> bool:
    if not isinstance(text, str) or not text or not text.isascii():
        return False
    whole, dot, frac = text.partition(".")
    if not whole.isdigit() or (len(whole) > 1 and whole.startswith("0")):
        return False
    if dot and (not frac or not frac.isdigit() or frac.endswith("0")):
        return False
    return True


def compare_decimal(a: str, b: str) -> int:
    ai, _, af = a.partition(".")
    bi, _, bf = b.partition(".")
    if len(ai) != len(bi):
        return -1 if len(ai) < len(bi) else 1
    if ai != bi:
        return -1 if ai < bi else 1
    n = max(len(af), len(bf))
    af, bf = af.ljust(n, "0"), bf.ljust(n, "0")
    if af == bf:
        return 0
    return -1 if af < bf else 1


def units_of(text: str) -> int:
    if not is_canonical_decimal(text):
        raise ProfileInputError(f"{text!r} is not a canonical nonnegative decimal")
    whole, _, frac = text.partition(".")
    if len(frac) > CONTRACT["decimal_scale"] or len(whole) > 12:
        raise ProfileInputError(f"{text!r} does not fit the {CONTRACT['decimal_scale']}-decimal accumulator")
    value = int(whole) * SCALE + int(frac.ljust(CONTRACT["decimal_scale"], "0") or "0")
    if value > INT64_MAX:
        raise ProfileInputError(f"{text!r} overflows the volume accumulator")
    return value


def format_units(units: int) -> str:
    whole, part = divmod(units, SCALE)
    if part == 0:
        return str(whole)
    return f"{whole}." + f"{part:0{CONTRACT['decimal_scale']}d}".rstrip("0")


def pair_bytes(px: str, sz: str) -> int:
    return len(px) + len(sz) + 8


def canonical_text(coin: str, art: dict) -> str:
    lines = [SCHEMA, f"coin={coin}", f"session_start_ms={art['session_start_ms']}",
             f"session_end_ms={art['session_end_ms']}", f"trades={art['trades']}", f"volume={art['volume']}",
             f"trades_hash={art['trades_hash']}", f"first_event_ms={art['first_event_ms']}",
             f"last_event_ms={art['last_event_ms']}"]
    body = "\n".join(lines) + "\n"
    return body + "".join(f"{px} {sz}\n" for px, sz in art["prices"])


def artifact_sha256(coin: str, art: dict) -> str:
    return hashlib.sha256(canonical_text(coin, art).encode("utf-8")).hexdigest()


def _int(value):
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    return value


def validate_artifact(coin: str, art, session_start_ms: int, cutoff_ms=None) -> None:
    if not isinstance(art, dict) or sorted(art) != sorted(ARTIFACT_KEYS):
        raise ProfileInputError(f"artifact keys must be exactly {sorted(ARTIFACT_KEYS)}")
    for key in ARTIFACT_KEYS:
        if key not in ("volume", "trades_hash", "sha256", "prices") and _int(art[key]) is None:
            raise ProfileInputError(f"artifact {key} {art[key]!r} is not an integer")
    if art["session_start_ms"] != session_start_ms or art["session_end_ms"] != session_start_ms + SESSION_MS:
        raise ProfileInputError(f"artifact session [{art['session_start_ms']}, {art['session_end_ms']}) is not session {session_start_ms}")
    if art["complete_minutes"] != MINUTES:
        raise ProfileInputError(f"artifact holds {art['complete_minutes']} complete minutes, want {MINUTES}")
    earliest = art["session_end_ms"] + CONTRACT["reorder_grace_ms"]
    if art["available_ms"] < earliest or (cutoff_ms is not None and art["available_ms"] > cutoff_ms):
        raise ProfileInputError(f"artifact became available at {art['available_ms']}, before {earliest} or after the cutoff {cutoff_ms}")
    if not (art["session_start_ms"] <= art["first_event_ms"] <= art["last_event_ms"] < art["session_end_ms"]):
        raise ProfileInputError("artifact event bounds are outside its session")
    if not isinstance(art["trades_hash"], str) or not _HEX64.match(art["trades_hash"]):
        raise ProfileInputError("artifact trades_hash is malformed")
    if not isinstance(art["sha256"], str) or not _HEX64.match(art["sha256"]):
        raise ProfileInputError("artifact sha256 is malformed")
    prices = art["prices"]
    if not isinstance(prices, list) or not 2 <= len(prices) <= CONTRACT["max_entries"]:
        raise ProfileInputError(f"artifact must hold 2 to {CONTRACT['max_entries']} prices")
    if art["trades"] < len(prices):
        raise ProfileInputError(f"artifact counts {art['trades']} trades for {len(prices)} distinct prices")
    volume = units_of(art["volume"])
    if volume <= 0:
        raise ProfileInputError("artifact volume is not positive")
    total = 0
    size = 0
    prev = None
    for i, pair in enumerate(prices):
        if not isinstance(pair, list) or len(pair) != 2 or not all(isinstance(x, str) for x in pair):
            raise ProfileInputError(f"artifact price {i} is not a [price, size] string pair")
        px, sz = pair
        if not is_canonical_decimal(px) or px == "0" or len(px) > CONTRACT["max_price_chars"]:
            raise ProfileInputError(f"artifact price {i} {px!r} is not a positive canonical decimal of at most {CONTRACT['max_price_chars']} characters")
        if prev is not None and compare_decimal(prev, px) >= 0:
            raise ProfileInputError(f"artifact prices are not strictly increasing at {i}")
        prev = px
        units = units_of(sz)
        if units <= 0:
            raise ProfileInputError(f"artifact size {i} is not positive")
        total += units
        if total > INT64_MAX:
            raise ProfileInputError("artifact sizes overflow")
        size += pair_bytes(px, sz)
    if total != volume:
        raise ProfileInputError(f"artifact sizes sum to {format_units(total)}, volume is {art['volume']}")
    if size > CONTRACT["max_pair_bytes"]:
        raise ProfileInputError(f"artifact pairs serialize to {size} bytes, over the {CONTRACT['max_pair_bytes']}-byte cap")
    want = artifact_sha256(coin, art)
    if want != art["sha256"]:
        raise ProfileInputError(f"artifact sha256 {art['sha256']} does not match its content ({want})")


class ProfileInput:
    def __init__(self, error=None, detail=""):
        self.error = error
        self.detail = detail
        self.coin = ""
        self.cutoff = None
        self.bar_interval = None
        self.source = ""
        self.sessions = {}

    def lookup(self, start: int):
        return self.sessions.get(start)


def parse_profile_input(obs) -> ProfileInput:
    if obs is None:
        return ProfileInput("profile_input_missing", "no volume profile input was supplied")
    if not isinstance(obs, dict):
        return ProfileInput("unsupported_profile", "the volume profile input must be an object")
    if not obs.get("available", False):
        return ProfileInput("profile_input_missing", str(obs.get("reason") or "the volume profile input is marked unavailable"))
    if obs.get("kind") != KIND:
        return ProfileInput("unsupported_profile", f"kind {obs.get('kind')!r} is not {KIND!r}")
    for key, want in CONTRACT.items():
        if key in ("interval_ms", "source"):
            continue
        if obs.get(key) != want:
            return ProfileInput("unsupported_profile", f"{key} {obs.get(key)!r} is not the frozen contract value {want!r}")
    if obs.get("interval_ms") != CONTRACT["interval_ms"]:
        return ProfileInput("unsupported_profile", f"interval_ms {obs.get('interval_ms')!r} is not {CONTRACT['interval_ms']}")
    coin = obs.get("coin")
    if not isinstance(coin, str) or not coin:
        return ProfileInput("unsupported_profile", "the volume profile input names no coin")
    if obs.get("source") not in SOURCES:
        return ProfileInput("unsupported_profile", f"source {obs.get('source')!r} is not one of {SOURCES}")
    out = ProfileInput()
    out.coin = coin
    out.source = obs["source"]
    if obs.get("cutoff_ms") is not None:
        out.cutoff = _int(obs.get("cutoff_ms"))
        if out.cutoff is None or out.cutoff <= 0:
            return ProfileInput("unsupported_profile", f"cutoff_ms {obs.get('cutoff_ms')!r} is not a positive integer")
    if obs.get("bar_interval_ms") is not None:
        out.bar_interval = _int(obs.get("bar_interval_ms"))
    sessions = obs.get("sessions")
    if not isinstance(sessions, list):
        return ProfileInput("unsupported_profile", "sessions must be a list")
    if out.cutoff is not None:
        starts = [s.get("session_start_ms") if isinstance(s, dict) else None for s in sessions]
        if starts != sealed_sessions(out.cutoff):
            return ProfileInput("profile_malformed", f"sessions {starts} are not the sessions {sealed_sessions(out.cutoff)} sealed at the cutoff {out.cutoff}")
    for i, sess in enumerate(sessions):
        if not isinstance(sess, dict) or sorted(sess) != sorted(SESSION_KEYS):
            return ProfileInput("profile_malformed", f"session {i} keys must be exactly {sorted(SESSION_KEYS)}")
        start = _int(sess["session_start_ms"])
        if start is None or start % SESSION_MS:
            return ProfileInput("profile_malformed", f"session {i} start {sess['session_start_ms']!r} is not a UTC session boundary")
        if start in out.sessions:
            return ProfileInput("profile_malformed", f"session {start} appears twice")
        status = sess["status"]
        code, reason = sess["reason_code"], sess["reason"]
        if status not in STATUSES or not isinstance(code, str) or not isinstance(reason, str) or len(reason) > MAX_REASON_CHARS:
            return ProfileInput("profile_malformed", f"session {start} status {status!r} or its reason is malformed")
        invalid = sess["invalid_ms"]
        if invalid is not None and (_int(invalid) is None or invalid <= 0):
            return ProfileInput("profile_malformed", f"session {start} invalid_ms {invalid!r} is not a positive integer")
        art = sess["artifact"]
        entry = {"start": start, "status": status, "code": code, "reason": reason, "invalid_ms": invalid, "artifact": None}
        if status in (STATUS_AVAILABLE, STATUS_INVALIDATED):
            if art is None:
                return ProfileInput("profile_malformed", f"session {start} is {status} without an artifact")
            if (status == STATUS_AVAILABLE) != (invalid is None) or (status == STATUS_AVAILABLE and code):
                return ProfileInput("profile_malformed", f"session {start} status {status} disagrees with its invalidation")
            try:
                validate_artifact(coin, art, start, out.cutoff)
            except ProfileInputError as exc:
                return ProfileInput("profile_malformed", f"session {start}: {exc}")
            if invalid is not None and (invalid < art["available_ms"] or (out.cutoff is not None and invalid > out.cutoff)):
                return ProfileInput("profile_malformed", f"session {start} invalidation {invalid} is outside its availability")
            entry["artifact"] = art
        else:
            if art is not None or invalid is not None or not code:
                return ProfileInput("profile_malformed", f"session {start} is {status} but carries an artifact or no reason")
        out.sessions[start] = entry
    return out


def usable_artifact(entry, deadline_ms: int):
    if entry is None or entry["artifact"] is None:
        return None
    art = entry["artifact"]
    if art["available_ms"] > deadline_ms:
        return None
    if entry["invalid_ms"] is not None and entry["invalid_ms"] <= deadline_ms:
        return None
    return art


def _scaled_prices(prices):
    digits = max(len(px.partition(".")[2]) for px, _ in prices)
    out = []
    for px, sz in prices:
        whole, _, frac = px.partition(".")
        out.append((int(whole + frac.ljust(digits, "0")), units_of(sz)))
    return out, 10 ** digits


def value_area(art: dict, bins: int, fraction: Fraction, session_bounds: bool) -> dict:
    scaled, denom = _scaled_prices(art["prices"])
    lo, hi = scaled[0][0], scaled[-1][0]
    span = hi - lo
    if span <= 0:
        raise ProfileInputError("the profile has a zero price range")
    volume = [0] * bins
    for p, units in scaled:
        idx = bins - 1 if p == hi else ((p - lo) * bins) // span
        volume[idx] += units
    total = sum(volume)
    poc = 0
    for i in range(1, bins):
        if volume[i] > volume[poc]:
            poc = i
    low_i = high_i = poc
    included = volume[poc]
    num, den = fraction.numerator, fraction.denominator
    while included * den < num * total:
        down = volume[low_i - 1] if low_i > 0 else None
        up = volume[high_i + 1] if high_i < bins - 1 else None
        if up is None or (down is not None and down >= up):
            low_i -= 1
            included += down
        else:
            high_i += 1
            included += up
    width = Fraction(span, denom * bins)
    low = Fraction(lo, denom)
    high = Fraction(hi, denom)
    if session_bounds:
        val, vah = low, high
    else:
        val = low + low_i * width
        vah = low + (high_i + 1) * width
    return {
        "low": low, "high": high, "width": width, "bins": bins, "volume": volume, "total": total, "poc": poc,
        "poc_low": low + poc * width, "poc_high": low + (poc + 1) * width, "va_low_bin": low_i, "va_high_bin": high_i,
        "va_volume": included, "val": val, "vah": vah,
    }
