
import json
import math
import os
import re
import sys
import tempfile
import threading
import time
from decimal import Decimal, ROUND_DOWN

MAINNET_URL = "https://api.hyperliquid.xyz"
TESTNET_URL = "https://api.hyperliquid-testnet.xyz"

META_CACHE_PATH = "/tmp/hl_meta.json"
META_CACHE_TTL_S = 3600
MIN_ORDER_NOTIONAL_USD = 10.0
MIN_ORDER_NOTIONAL_SAFETY_MARGIN = 0.03

_EXCHANGE_INIT_BACKOFF_S = 30

OHLCV_CACHE_DIR = "/tmp"
OHLCV_CACHE_PREFIX = "hl_ohlcv_"
OHLCV_CACHE_TTL_S = 60
OHLCV_GAP_MARGIN = 50
OHLCV_MAX_CANDLES = 5000
OHLCV_MAX_EXTEND_PASSES = 12

try:
    from hyperliquid.info import Info as _HLInfo
    from hyperliquid.exchange import Exchange as _HLExchange
    _SDK_AVAILABLE = True
except ImportError:
    _HLInfo = None
    _HLExchange = None
    _SDK_AVAILABLE = False

try:
    from hyperliquid.api import API as _HLAPI
except ImportError:
    _HLAPI = None

try:
    from hyperliquid.utils.error import ClientError as _HLClientError
except ImportError:
    class _HLClientError(Exception):
        pass

try:
    from hyperliquid.utils.types import Cloid as _HLCloid
except ImportError:
    _HLCloid = None

_SHARED_TOOLS = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools')
if _SHARED_TOOLS not in sys.path:
    sys.path.append(_SHARED_TOOLS)

import egress_policy

egress_policy.pin_hyperliquid_sdk()


def _to_cloid(cloid):
    if not cloid:
        return None
    if _HLCloid is None:
        raise RuntimeError("hyperliquid-python-sdk has no Cloid type; cannot tag a client order id")
    return _HLCloid.from_str(str(cloid))


def _safe_float(v) -> float:
    if v is None:
        return 0.0
    try:
        return float(v)
    except (TypeError, ValueError):
        return 0.0


def _safe_int(v) -> int:
    if v is None:
        return 0
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def _perps_px_decimals(px: float, sz_decimals: int) -> int:
    px_decimals = max(0, 6 - sz_decimals)
    log = math.floor(math.log10(abs(px)))
    sig_decimals = max(0, 5 - 1 - int(log))
    return min(px_decimals, sig_decimals)


def _round_perps_px(px: float, sz_decimals: int) -> float:
    if px <= 0:
        return px
    return round(px, _perps_px_decimals(px, sz_decimals))


def quote_request_unchanged(symbol: str, size, px, sz_decimals: int) -> None:
    sz_dec = Decimal(str(size))
    px_dec = Decimal(str(px))
    if sz_dec <= 0 or px_dec <= 0:
        raise ValueError(f"quote request for {symbol} needs a positive size and price")
    if sz_dec != sz_dec.quantize(Decimal("1").scaleb(-max(int(sz_decimals), 0)), rounding=ROUND_DOWN):
        raise ValueError(f"quote size {size} for {symbol} is not on the {sz_decimals}-decimal lot grid; the saved request would change")
    decimals = _perps_px_decimals(float(px_dec), sz_decimals)
    if px_dec != px_dec.quantize(Decimal("1").scaleb(-decimals), rounding=ROUND_DOWN):
        raise ValueError(f"quote price {px} for {symbol} is not on the {decimals}-decimal price grid; the saved request would change")


def floor_lot_size(sz: float, sz_decimals: int) -> float:
    if sz <= 0:
        return 0.0
    decimals = max(int(sz_decimals), 0)
    quant = Decimal("1").scaleb(-decimals)
    slack = Decimal("1e-9").scaleb(-decimals)
    return float((Decimal(str(sz)) + slack).quantize(quant, rounding=ROUND_DOWN))


def _load_meta_cache(path: str = META_CACHE_PATH, ttl_s: int = META_CACHE_TTL_S, now: float = None):
    try:
        with open(path, "r") as f:
            data = json.load(f)
    except (OSError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    ts = data.get("ts")
    try:
        ts_f = float(ts) if ts is not None else 0.0
    except (TypeError, ValueError):
        return None
    cur = now if now is not None else time.time()
    if cur - ts_f > ttl_s:
        return None
    spot_meta = data.get("spot_meta")
    meta = data.get("meta")
    if not isinstance(spot_meta, dict) or not isinstance(meta, dict):
        return None
    if not spot_meta.get("universe") or not meta.get("universe"):
        return None
    return spot_meta, meta


def sz_decimals_from_meta_cache(symbol: str, path: str = META_CACHE_PATH):
    cached = _load_meta_cache(path, ttl_s=float("inf"))
    if cached is None:
        return None
    _spot_meta, meta = cached
    universe = meta.get("universe")
    if not isinstance(universe, list):
        return None
    for entry in universe:
        if not isinstance(entry, dict) or entry.get("name") != symbol:
            continue
        value = entry.get("szDecimals")
        if isinstance(value, bool):
            return None
        try:
            return int(value)
        except (TypeError, ValueError):
            return None
    return None


def _save_meta_cache(spot_meta, meta, path: str = META_CACHE_PATH) -> None:
    payload = {"ts": time.time(), "spot_meta": spot_meta, "meta": meta}
    dir_ = os.path.dirname(path) or "."
    fd = None
    tmp_path = None
    try:
        fd, tmp_path = tempfile.mkstemp(prefix=".hl_meta_", suffix=".json", dir=dir_)
        with os.fdopen(fd, "w") as f:
            fd = None
            json.dump(payload, f)
        os.replace(tmp_path, path)
        tmp_path = None
    except (OSError, TypeError, ValueError) as exc:
        print(f"[WARN] hl meta cache save failed: {exc}", file=sys.stderr)
    finally:
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                pass
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


def _ohlcv_cache_enabled() -> bool:
    return os.environ.get("GO_TRADER_HL_OHLCV_CACHE", "1") != "0"


def _ohlcv_cache_ttl(interval_ms: int) -> int:
    half_bar_s = max(1, interval_ms // 2000)
    return min(OHLCV_CACHE_TTL_S, half_bar_s)


def _ohlcv_cache_path(symbol: str, interval: str, limit: int,
                      cache_dir: str = None) -> str:
    if cache_dir is None:
        cache_dir = OHLCV_CACHE_DIR
    safe_sym = "".join(c if c.isalnum() else "_" for c in str(symbol))
    safe_int = "".join(c if c.isalnum() else "_" for c in str(interval))
    return os.path.join(cache_dir, f"{OHLCV_CACHE_PREFIX}{safe_sym}_{safe_int}_{limit}.json")


def _load_ohlcv_cache(path: str, ttl_s: int = OHLCV_CACHE_TTL_S, now: float = None):
    try:
        with open(path, "r") as f:
            data = json.load(f)
    except (OSError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    ts = data.get("ts")
    try:
        ts_f = float(ts) if ts is not None else 0.0
    except (TypeError, ValueError):
        return None
    cur = now if now is not None else time.time()
    if cur - ts_f > ttl_s:
        return None
    candles = data.get("candles")
    if not isinstance(candles, list) or not candles:
        return None
    return candles


def _save_ohlcv_cache(candles, path: str) -> None:
    payload = {"ts": time.time(), "candles": candles}
    dir_ = os.path.dirname(path) or "."
    fd = None
    tmp_path = None
    try:
        fd, tmp_path = tempfile.mkstemp(prefix=".hl_ohlcv_", suffix=".json", dir=dir_)
        with os.fdopen(fd, "w") as f:
            fd = None
            json.dump(payload, f)
        os.replace(tmp_path, path)
        tmp_path = None
    except (OSError, TypeError, ValueError) as exc:
        print(f"[WARN] hl ohlcv cache save failed: {exc}", file=sys.stderr)
    finally:
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                pass
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


def _normalize_spot_meta(spot_meta):
    if not isinstance(spot_meta, dict):
        return spot_meta
    tokens = spot_meta.get("tokens")
    universe = spot_meta.get("universe")
    if not isinstance(tokens, list) or not isinstance(universe, list):
        return spot_meta

    by_index = {}
    max_index = -1
    for tok in tokens:
        if isinstance(tok, dict) and isinstance(tok.get("index"), int):
            idx = tok["index"]
            by_index[idx] = tok
            if idx > max_index:
                max_index = idx

    clean_universe = []
    dropped = []
    for entry in universe:
        pair = entry.get("tokens") if isinstance(entry, dict) else None
        if (
            isinstance(pair, list)
            and len(pair) == 2
            and all(isinstance(x, int) and x in by_index for x in pair)
        ):
            clean_universe.append(entry)
        else:
            dropped.append(entry.get("name") if isinstance(entry, dict) else None)

    aligned = max_index + 1 == len(tokens) and all(
        isinstance(t, dict) and t.get("index") == i for i, t in enumerate(tokens)
    )
    if aligned and not dropped:
        return spot_meta

    placeholder = {"name": "", "szDecimals": 0, "index": -1}
    dense_tokens = [by_index.get(i, placeholder) for i in range(max_index + 1)]

    if dropped:
        print(
            f"[WARN] hl spotMeta: dropped {len(dropped)} unresolvable spot "
            f"pair(s): {dropped[:5]}",
            file=sys.stderr,
        )

    normalized = dict(spot_meta)
    normalized["tokens"] = dense_tokens
    normalized["universe"] = clean_universe
    return normalized


def _fetch_raw_meta(base_url: str):
    if _HLAPI is None:
        raise RuntimeError("hyperliquid.api.API unavailable; cannot prefetch meta")
    api = _HLAPI(base_url=base_url)
    spot_meta = api.post("/info", {"type": "spotMeta"})
    meta = api.post("/info", {"type": "meta", "dex": ""})
    return spot_meta, meta


class HyperliquidExchangeAdapter:

    def __init__(self, public_only: bool = False):
        if not _SDK_AVAILABLE:
            raise ImportError(
                "hyperliquid-python-sdk not installed. Run: uv sync --no-dev (service host) or uv sync (development)"
            )

        self._public_only = bool(public_only)
        secret = "" if self._public_only else os.environ.get("HYPERLIQUID_SECRET_KEY", "")
        addr = "" if self._public_only else os.environ.get("HYPERLIQUID_ACCOUNT_ADDRESS", "")
        testnet = os.environ.get("HYPERLIQUID_TESTNET", "") == "1"
        base_url = TESTNET_URL if testnet else MAINNET_URL
        self._base_url = base_url

        self._wallet = None
        self._exchange = None
        self._exchange_lock = threading.Lock()
        self._exchange_init_error = None
        self._exchange_backoff_until = 0.0
        self._cached_spot_meta = None
        self._cached_meta = None
        self._sz_decimals_misses: set[str] = set()

        self._info = self._build_info(base_url, allow_cache=not self._public_only)
        self._account_address = addr

        if secret:
            import eth_account
            wallet = eth_account.Account.from_key(secret)
            self._wallet = wallet
            self._account_address = addr or wallet.address

    def _build_info(self, base_url: str, allow_cache: bool):
        cached = _load_meta_cache() if allow_cache else None
        if cached is not None:
            spot_meta, meta = cached
            self._cached_spot_meta, self._cached_meta = spot_meta, meta
            return _HLInfo(base_url=base_url, skip_ws=True, meta=meta,
                           spot_meta=_normalize_spot_meta(spot_meta))
        try:
            spot_meta, meta = _fetch_raw_meta(base_url)
            if not self._public_only:
                _save_meta_cache(spot_meta, meta)
            self._cached_spot_meta, self._cached_meta = spot_meta, meta
            return _HLInfo(base_url=base_url, skip_ws=True, meta=meta,
                           spot_meta=_normalize_spot_meta(spot_meta))
        except Exception as exc:
            self._cached_spot_meta, self._cached_meta = None, None
            if self._public_only:
                raise
            print(f"[WARN] hl meta fetch failed ({exc}); falling back to SDK init", file=sys.stderr)
            return _HLInfo(base_url=base_url, skip_ws=True)

    def _ensure_exchange(self):
        if self._exchange is not None:
            return self._exchange
        if self._wallet is None:
            return None
        now = time.time()
        with self._exchange_lock:
            if self._exchange is not None:
                return self._exchange
            if (self._exchange_init_error is not None
                    and now < self._exchange_backoff_until):
                raise RuntimeError(
                    "Failed to initialize Hyperliquid Exchange client: "
                    f"{self._exchange_init_error}"
                )
            try:
                kwargs = {
                    "base_url": self._base_url,
                    "account_address": self._account_address,
                }
                if self._cached_meta is not None and self._cached_spot_meta is not None:
                    kwargs["meta"] = self._cached_meta
                    kwargs["spot_meta"] = _normalize_spot_meta(self._cached_spot_meta)
                self._exchange = _HLExchange(self._wallet, **kwargs)
                self._exchange_init_error = None
                self._exchange_backoff_until = 0.0
                return self._exchange
            except Exception as e:
                self._exchange_init_error = e
                self._exchange_backoff_until = time.time() + _EXCHANGE_INIT_BACKOFF_S
                raise RuntimeError(
                    f"Failed to initialize Hyperliquid Exchange client: {e}"
                ) from e

    def _require_exchange(self, caller: str = "live trading"):
        if self._wallet is None:
            raise RuntimeError(
                f"{caller} requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        return self._ensure_exchange()

    def _sz_decimals(self, symbol: str) -> int:
        resolved = self.lot_size_decimals(symbol)
        if resolved is not None:
            return resolved
        print(f"[WARN] sz_decimals unresolved for {symbol}, defaulting to 3", file=sys.stderr)
        return 3

    def lot_size_decimals(self, symbol: str):
        info = self._info
        resolved = self._resolve_sz_decimals_any(info, symbol)
        if resolved is not None:
            return resolved
        if symbol in self._sz_decimals_misses:
            return None
        try:
            self._info = self._build_info(self._base_url, allow_cache=False)
        except Exception as exc:
            print(f"[WARN] hl meta refresh failed for {symbol}: {exc}", file=sys.stderr)
            self._sz_decimals_misses.add(symbol)
            return None
        resolved = self._resolve_sz_decimals_any(self._info, symbol)
        if resolved is not None:
            return resolved
        print(f"[WARN] sz_decimals not found for {symbol} after refresh; lot size unresolved", file=sys.stderr)
        self._sz_decimals_misses.add(symbol)
        return None

    @classmethod
    def _resolve_sz_decimals_any(cls, info, symbol: str):
        if info is None:
            return None
        resolved = cls._resolve_sz_decimals(info, symbol)
        if resolved is not None:
            return resolved
        sz_by_asset = getattr(info, "asset_to_sz_decimals", None)
        if isinstance(sz_by_asset, dict) and symbol in sz_by_asset:
            return sz_by_asset[symbol]
        return None

    @staticmethod
    def _resolve_sz_decimals(info, symbol: str):
        sz_by_asset = getattr(info, "asset_to_sz_decimals", None)
        if not isinstance(sz_by_asset, dict):
            return None
        name_to_asset = getattr(info, "name_to_asset", None)
        if callable(name_to_asset):
            try:
                asset = name_to_asset(symbol)
            except Exception:
                asset = None
            if isinstance(asset, int) and asset in sz_by_asset:
                return sz_by_asset[asset]
        coin_to_asset = getattr(info, "coin_to_asset", None)
        if isinstance(coin_to_asset, dict) and symbol in coin_to_asset:
            asset = coin_to_asset[symbol]
            if isinstance(asset, int) and asset in sz_by_asset:
                return sz_by_asset[asset]
        return None

    def public_instrument_rules(self, symbol: str) -> dict:
        if not self._public_only:
            raise RuntimeError("public instrument rules need the public-only reader")
        meta = self._cached_meta
        universe = meta.get("universe") if isinstance(meta, dict) else None
        if not isinstance(universe, list):
            raise RuntimeError("Hyperliquid meta has no perpetual universe")
        try:
            asset = self._info.name_to_asset(symbol)
        except Exception as exc:
            raise RuntimeError(f"Hyperliquid meta does not resolve {symbol}: {exc!r}") from exc
        if isinstance(asset, bool) or not isinstance(asset, int) or not 0 <= asset < len(universe):
            raise RuntimeError(f"{symbol} resolves to asset {asset!r}, which is not a main-universe perpetual")
        entry = universe[asset]
        if not isinstance(entry, dict) or entry.get("name") != symbol:
            raise RuntimeError(f"Hyperliquid meta entry {asset} does not name {symbol}")
        sz = entry.get("szDecimals")
        if isinstance(sz, bool) or not isinstance(sz, int):
            raise RuntimeError(f"Hyperliquid szDecimals for {symbol} is not an integer: {sz!r}")
        if self._info.asset_to_sz_decimals.get(asset) != sz:
            raise RuntimeError(f"Hyperliquid szDecimals for {symbol} disagrees with the resolved asset")
        lev = entry.get("maxLeverage")
        if isinstance(lev, bool) or not isinstance(lev, int) or lev <= 0:
            raise RuntimeError(f"Hyperliquid maxLeverage for {symbol} is not a positive integer: {lev!r}")
        delisted = entry.get("isDelisted", False)
        if not isinstance(delisted, bool):
            raise RuntimeError(f"Hyperliquid isDelisted for {symbol} is not a boolean: {delisted!r}")
        return {"name": symbol, "asset": asset, "sz_decimals": sz, "max_leverage": lev, "is_delisted": delisted}

    def public_host(self) -> str:
        return self._base_url

    def public_l2_snapshot(self, symbol: str, depth: int) -> dict:
        if not self._public_only:
            raise RuntimeError("public depth reads need the public-only reader")
        if isinstance(depth, bool) or not isinstance(depth, int) or not 1 <= depth <= 20:
            raise RuntimeError(f"Hyperliquid l2 depth {depth!r} is outside [1, 20]")
        rules = self.public_instrument_rules(symbol)
        if rules["is_delisted"]:
            raise RuntimeError(f"{symbol} is delisted")
        resp = self._info.l2_snapshot(symbol)
        if not isinstance(resp, dict):
            raise RuntimeError(f"Hyperliquid l2Book for {symbol} is not an object")
        if resp.get("coin") != symbol:
            raise RuntimeError(f"Hyperliquid l2Book answered for {resp.get('coin')!r}, not {symbol}")
        ts = resp.get("time")
        if isinstance(ts, bool) or not isinstance(ts, int) or ts <= 0:
            raise RuntimeError(f"Hyperliquid l2Book time {ts!r} is not a positive integer")
        levels = resp.get("levels")
        if not isinstance(levels, list) or len(levels) != 2:
            raise RuntimeError("Hyperliquid l2Book levels are not two sides")
        sides = []
        for side in levels:
            if not isinstance(side, list):
                raise RuntimeError("Hyperliquid l2Book side is not a list")
            out = []
            for level in side[:depth]:
                if not isinstance(level, dict):
                    raise RuntimeError("Hyperliquid l2Book level is not an object")
                px, sz, n = level.get("px"), level.get("sz"), level.get("n")
                if not isinstance(px, str) or not isinstance(sz, str) or isinstance(n, bool) or not isinstance(n, int):
                    raise RuntimeError(f"Hyperliquid l2Book level {level!r} is not px/sz text with an integer order count")
                out.append({"n": n, "px": px, "sz": sz})
            sides.append(out)
        return {"request": {"coin": symbol, "type": "l2Book"}, "response": {"coin": symbol, "levels": sides, "time": ts}}

    PUBLIC_CANDLE_INTERVALS = ("1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "8h", "12h", "1d")
    PUBLIC_DECIMAL = re.compile(r"^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]{1,4})?$")

    def public_candles(self, symbol: str, interval: str, limit: int, now_ms: int) -> dict:
        if not self._public_only:
            raise RuntimeError("public candle reads need the public-only reader")
        if interval not in self.PUBLIC_CANDLE_INTERVALS:
            raise RuntimeError(f"Hyperliquid candle interval {interval!r} is not supported")
        if isinstance(limit, bool) or not isinstance(limit, int) or not 1 <= limit <= 5000:
            raise RuntimeError(f"Hyperliquid candle limit {limit!r} is outside [1, 5000]")
        rules = self.public_instrument_rules(symbol)
        if rules["is_delisted"]:
            raise RuntimeError(f"{symbol} is delisted")
        ivl = self._OHLCV_INTERVAL_MS[interval]
        start = now_ms - ivl * (limit + 2)
        rows = self._info.candles_snapshot(symbol, interval, start, now_ms)
        if not isinstance(rows, list):
            raise RuntimeError(f"Hyperliquid candleSnapshot for {symbol} is not a list")
        closed, forming = [], []
        prev = None
        for row in rows:
            if not isinstance(row, dict):
                raise RuntimeError("Hyperliquid candle row is not an object")
            t, close_t = row.get("t"), row.get("T")
            if isinstance(t, bool) or not isinstance(t, int) or isinstance(close_t, bool) or not isinstance(close_t, int) or t % ivl != 0:
                raise RuntimeError(f"Hyperliquid candle row {row!r} has no aligned integer open and close time")
            if row.get("s") not in (None, symbol) or row.get("i") not in (None, interval):
                raise RuntimeError(f"Hyperliquid candle row is for {row.get('s')!r} {row.get('i')!r}")
            if prev is not None and t <= prev:
                raise RuntimeError("Hyperliquid candle rows are not strictly ascending")
            prev = t
            vals = []
            for name in ("o", "h", "l", "c", "v"):
                raw = row.get(name)
                if not isinstance(raw, str) or not self.PUBLIC_DECIMAL.match(raw):
                    raise RuntimeError(f"Hyperliquid candle {name} {raw!r} is not decimal text")
                vals.append(float(raw))
            if t + ivl <= now_ms:
                closed.append([t] + vals)
            elif t <= now_ms:
                forming = [t, vals[0]]
        return {"bars": closed[-limit:], "forming": forming}

    def public_funding_window(self, symbol: str, start_ms: int, end_ms: int, max_pages: int = 64) -> dict:
        if not self._public_only:
            raise RuntimeError("public funding reads need the public-only reader")
        for name, value in (("start_ms", start_ms), ("end_ms", end_ms), ("max_pages", max_pages)):
            if isinstance(value, bool) or not isinstance(value, int):
                raise RuntimeError(f"Hyperliquid funding window {name} must be an integer, got {value!r}")
        if start_ms < 1 or end_ms < start_ms:
            raise RuntimeError(f"Hyperliquid funding window [{start_ms}, {end_ms}] is empty or negative")
        self.public_instrument_rules(symbol)
        out = []
        cursor = start_ms
        pages = 0
        while cursor <= end_ms:
            if pages >= max_pages:
                raise RuntimeError(f"Hyperliquid funding window needed more than {max_pages} pages")
            pages += 1
            page = self._info.funding_history(symbol, cursor, end_ms)
            if not isinstance(page, list):
                raise RuntimeError(f"Hyperliquid fundingHistory for {symbol} at {cursor} is not a list")
            if not page:
                break
            prev = None
            for r in page:
                if not isinstance(r, dict):
                    raise RuntimeError("Hyperliquid funding record is not an object")
                t, rate = r.get("time"), r.get("fundingRate")
                if isinstance(t, bool) or not isinstance(t, int) or not isinstance(rate, str) or not self.PUBLIC_DECIMAL.match(rate):
                    raise RuntimeError(f"Hyperliquid funding record {r!r} has no integer time and decimal rate text")
                if r.get("coin") not in (None, symbol):
                    raise RuntimeError(f"Hyperliquid funding record is for {r.get('coin')!r}, not {symbol}")
                if t < cursor or (prev is not None and t <= prev):
                    raise RuntimeError(f"Hyperliquid funding history for {symbol} is not strictly ascending from {cursor}")
                prev = t
                if t <= end_ms:
                    out.append({"time": t, "rate": rate})
            if prev > end_ms:
                break
            cursor = prev + 1
        return {"records": out, "pages": pages}

    @property
    def is_live(self) -> bool:
        return self._wallet is not None

    @property
    def mode(self) -> str:
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "hyperliquid"


    def get_spot_price(self, symbol: str) -> float:
        mids = self._info.all_mids()
        raw = mids.get(symbol, mids.get(symbol + "-PERP", "0"))
        return float(raw or 0)

    _OHLCV_INTERVAL_MS = {
        "1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000,
        "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000,
        "4h": 14_400_000, "8h": 28_800_000, "12h": 43_200_000,
        "1d": 86_400_000, "3d": 259_200_000, "1w": 604_800_000,
    }

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        interval_ms = self._OHLCV_INTERVAL_MS.get(interval, 3_600_000)

        cache_enabled = _ohlcv_cache_enabled()
        cache_path = None
        if cache_enabled:
            cache_path = _ohlcv_cache_path(symbol, interval, limit)
            cached = _load_ohlcv_cache(cache_path, ttl_s=_ohlcv_cache_ttl(interval_ms))
            if cached is not None:
                return cached

        result = []
        for c in self._candle_window(symbol, interval, limit):
            result.append([
                int(c.get("T", c.get("t", 0))),
                float(c["o"]),
                float(c["h"]),
                float(c["l"]),
                float(c["c"]),
                float(c["v"]),
            ])
        if cache_enabled and result:
            _save_ohlcv_cache(result, cache_path)
        return result

    def get_ohlcv_candles(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        if interval not in self._OHLCV_INTERVAL_MS:
            raise ValueError(f"unsupported Hyperliquid candle interval {interval!r}")
        return [dict(c) for c in self._candle_window(symbol, interval, limit)]

    def _candle_window(self, symbol: str, interval: str, limit: int) -> list:
        interval_ms = self._OHLCV_INTERVAL_MS.get(interval, 3_600_000)
        end_ms = int(time.time() * 1000)
        requested = limit + OHLCV_GAP_MARGIN
        result = []
        prev_count = -1
        stale_widens = 0
        for _ in range(OHLCV_MAX_EXTEND_PASSES):
            start_ms = end_ms - interval_ms * requested
            candles = self._info.candles_snapshot(symbol, interval, start_ms, end_ms)
            result = list(candles)
            if (not result
                    or len(result) >= limit
                    or len(result) >= OHLCV_MAX_CANDLES):
                break
            if len(result) > prev_count:
                stale_widens = 0
            else:
                stale_widens += 1
                if stale_widens >= 2:
                    break
            prev_count = len(result)
            requested *= 2

        if len(result) > limit:
            result = result[-limit:]
        elif result and len(result) < limit:
            print(
                f"[WARN] hl ohlcv shortfall for {symbol} {interval}: got "
                f"{len(result)} of {limit} requested after extending the window "
                f"to the symbol's available history",
                file=sys.stderr,
            )
        return result

    def get_funding_rate(self, symbol: str) -> float:
        try:
            data = self._info.meta_and_asset_ctxs()
            universe = data[0]["universe"]
            asset_ctxs = data[1]
            for i, asset in enumerate(universe):
                if asset["name"] == symbol:
                    return float(asset_ctxs[i].get("funding", 0))
            return 0.0
        except Exception:
            return 0.0

    def get_funding_history(self, symbol: str, days: int = 7) -> list:
        try:
            start_time = int(time.time() * 1000) - days * 86400 * 1000
            records = self._info.funding_history(symbol, start_time)
            return [
                {"rate": float(r["fundingRate"]), "time": int(r["time"])}
                for r in records
            ]
        except Exception:
            return []

    def get_funding_history_range(self, symbol: str, start_ms: int,
                                  end_ms: int = None) -> list:
        if end_ms is None:
            end_ms = int(time.time() * 1000)
        out = []
        seen = set()
        cursor = int(start_ms)
        try:
            while cursor < end_ms:
                records = self._info.funding_history(symbol, cursor)
                if not records:
                    break
                progressed = False
                for r in records:
                    t = int(r["time"])
                    if t > end_ms:
                        continue
                    if t not in seen:
                        seen.add(t)
                        out.append({"rate": float(r["fundingRate"]), "time": t})
                        progressed = True
                last_t = int(records[-1]["time"])
                if last_t <= cursor or not progressed:
                    break
                cursor = last_t + 1
        except Exception:
            return []
        out.sort(key=lambda r: r["time"])
        return out

    def get_funding_history_window(self, symbol: str, start_ms: int,
                                   end_ms: int) -> dict:
        start_ms = int(start_ms)
        end_ms = int(end_ms)
        if end_ms < start_ms:
            raise ValueError(f"funding window end {end_ms} is before start {start_ms}")
        out = []
        observed = None
        cursor = start_ms
        while True:
            page = self._info.funding_history(symbol, cursor)
            if not isinstance(page, list):
                raise ValueError(
                    f"funding_history {symbol} at {cursor} returned {type(page).__name__}, not a list")
            if not page:
                break
            prev = None
            for r in page:
                t = int(r["time"])
                rate = float(r["fundingRate"])
                if not math.isfinite(rate):
                    raise ValueError(f"funding_history {symbol} print {t} has non-finite rate")
                if t < cursor:
                    raise ValueError(
                        f"funding_history {symbol} print {t} is before cursor {cursor}")
                if prev is not None and t <= prev:
                    raise ValueError(
                        f"funding_history {symbol} page is not ascending at {t}")
                prev = t
                if t <= end_ms:
                    out.append({"rate": rate, "time": t})
            observed = prev if observed is None else max(observed, prev)
            if prev > end_ms:
                break
            cursor = prev + 1
        return {"records": out, "observed_through_ms": observed}


    def get_open_positions(self) -> list:
        if not self._account_address:
            return []
        try:
            user_state = self._info.user_state(self._account_address)
            positions = []
            for asset_pos in user_state.get("assetPositions", []):
                pos = asset_pos.get("position", {})
                szi = float(pos.get("szi", 0))
                if szi == 0:
                    continue
                positions.append({
                    "coin": pos.get("coin", ""),
                    "size": szi,
                    "entry_price": float(pos.get("entryPx", 0) or 0),
                    "unrealized_pnl": float(pos.get("unrealizedPnl", 0) or 0),
                })
            return positions
        except Exception:
            return []


    def market_open(self, symbol: str, is_buy: bool, size: float, cloid: str | None = None) -> dict:
        exchange = self._require_exchange("market_open")
        sz_decimals = self._sz_decimals(symbol)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        if cloid:
            return exchange.market_open(symbol, is_buy, size, None, 0.01, cloid=_to_cloid(cloid))
        return exchange.market_open(symbol, is_buy, size, None, 0.01)

    def set_order_expiry(self, expires_after_ms: int | None) -> None:
        exchange = self._require_exchange("set_order_expiry")
        exchange.set_expires_after(int(expires_after_ms) if expires_after_ms else None)

    def query_order_by_cloid(self, cloid: str) -> dict:
        if not self._account_address:
            raise RuntimeError("query_order_by_cloid requires HYPERLIQUID_ACCOUNT_ADDRESS")
        return self._info.query_order_by_cloid(self._account_address, _to_cloid(cloid))

    def query_order_by_oid(self, oid: int) -> dict:
        if not self._account_address:
            raise RuntimeError("query_order_by_oid requires HYPERLIQUID_ACCOUNT_ADDRESS")
        return self._info.query_order_by_oid(self._account_address, int(oid))

    def user_fills_since(self, since_ms: int) -> list:
        if not self._account_address:
            raise RuntimeError("user_fills_since requires HYPERLIQUID_ACCOUNT_ADDRESS")
        return self._info.user_fills_by_time(self._account_address, int(since_ms))

    @property
    def account_address(self) -> str:
        return str(self._account_address or "")

    def limit_open(
        self,
        symbol: str,
        is_buy: bool,
        size: float,
        limit_px: float,
        tif: str = "Alo",
        cloid: str | None = None,
        reduce_only: bool = False,
        require_exact: bool = False,
    ) -> dict:
        exchange = self._require_exchange("limit_open")
        sz_decimals = self._sz_decimals(symbol)
        if require_exact:
            quote_request_unchanged(symbol, size, limit_px, sz_decimals)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        if limit_px <= 0:
            raise ValueError(f"limit_px must be > 0, got {limit_px}")
        if tif not in ("Alo", "Gtc", "Ioc"):
            raise ValueError(f"unsupported tif {tif!r}, expected 'Alo', 'Gtc' or 'Ioc'")
        limit_px = _round_perps_px(limit_px, sz_decimals)
        order_type = {"limit": {"tif": tif}}
        if cloid:
            return exchange.order(
                symbol, is_buy, size, limit_px, order_type, reduce_only=bool(reduce_only), cloid=_to_cloid(cloid)
            )
        return exchange.order(
            symbol, is_buy, size, limit_px, order_type, reduce_only=bool(reduce_only)
        )

    def check_quote_request(self, symbol: str, size, px) -> None:
        sz_decimals = self.lot_size_decimals(symbol)
        if sz_decimals is None:
            raise ValueError(f"lot metadata for {symbol} is unknown, so the saved quote request cannot be checked")
        quote_request_unchanged(symbol, size, px, sz_decimals)

    def cancel_by_cloid(self, symbol: str, cloid: str) -> dict:
        exchange = self._require_exchange("cancel_by_cloid")
        return exchange.cancel_by_cloid(symbol, _to_cloid(cloid))

    def strict_position(self, symbol: str) -> dict:
        if not self._account_address:
            raise RuntimeError("strict_position requires HYPERLIQUID_ACCOUNT_ADDRESS")
        user_state = self._info.user_state(self._account_address)
        if not isinstance(user_state, dict) or not isinstance(user_state.get("assetPositions"), list):
            raise ValueError(f"user_state returned no position list: {user_state!r}")
        size = "0"
        liq = 0.0
        for asset_pos in user_state["assetPositions"]:
            pos = asset_pos.get("position") if isinstance(asset_pos, dict) else None
            if not isinstance(pos, dict) or pos.get("coin") != symbol:
                continue
            raw = pos.get("szi")
            if raw in (None, ""):
                raise ValueError(f"position for {symbol} carries no size")
            Decimal(str(raw))
            size = str(raw)
            liq_raw = pos.get("liquidationPx")
            if liq_raw not in (None, ""):
                liq = float(liq_raw)
                if not math.isfinite(liq) or liq < 0:
                    raise ValueError(f"position for {symbol} carries liquidation price {liq_raw!r}")
        free = user_state.get("withdrawable")
        return {"size": size, "liquidation_px": liq, "free_margin": "" if free in (None, "") else str(free)}

    def strict_pair_position(self, symbol: str) -> dict:
        if not self._account_address:
            raise RuntimeError("strict_pair_position requires HYPERLIQUID_ACCOUNT_ADDRESS")
        user_state = self._info.user_state(self._account_address)
        if not isinstance(user_state, dict) or not isinstance(user_state.get("assetPositions"), list):
            raise ValueError(f"user_state returned no position list: {user_state!r}")
        found = None
        for i, asset_pos in enumerate(user_state["assetPositions"]):
            if not isinstance(asset_pos, dict) or not isinstance(asset_pos.get("position"), dict):
                raise ValueError(f"position row {i} is malformed, so no coin can be proven flat: {asset_pos!r}")
            pos = asset_pos["position"]
            coin = pos.get("coin")
            if not isinstance(coin, str) or not coin:
                raise ValueError(f"position row {i} names no coin, so no coin can be proven flat")
            if coin != symbol:
                continue
            if found is not None:
                raise ValueError(f"the position list holds {symbol} twice")
            raw = pos.get("szi")
            if not isinstance(raw, str) or not raw.strip():
                raise ValueError(f"position for {symbol} carries no size text: {raw!r}")
            size = Decimal(raw.strip())
            if not size.is_finite():
                raise ValueError(f"position for {symbol} carries size {raw!r}")
            row_type = asset_pos.get("type")
            if row_type != "oneWay":
                raise ValueError(f"position for {symbol} has position mode {row_type!r}, not oneWay")
            lev = pos.get("leverage")
            margin = lev.get("type") if isinstance(lev, dict) else None
            if size != 0 and margin not in ("cross", "isolated"):
                raise ValueError(f"position for {symbol} carries margin mode {margin!r}")
            found = {"szi": raw.strip(), "margin_mode": margin or "", "position_mode": "one_way", "found": True}
        if found is None:
            return {"szi": "0", "margin_mode": "", "position_mode": "one_way", "found": False}
        return found

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
        exchange = self._require_exchange("market_close")
        if sz is not None:
            sz_decimals = self._sz_decimals(symbol)
            sz = round(sz, sz_decimals)
            if sz <= 0:
                raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return exchange.market_close(symbol, sz)

    def sized_close_price(self, symbol: str, is_buy: bool) -> float:
        sz_decimals = self._sz_decimals(symbol)
        mid = _safe_float(self._info.all_mids().get(symbol))
        if mid <= 0 or not math.isfinite(mid):
            raise ValueError(f"no usable mid price for {symbol}")
        return _round_perps_px(mid * (1.01 if is_buy else 0.99), sz_decimals)

    def market_close_sized(self, symbol: str, is_buy: bool, size: float, px: float, reduce_only: bool,
                           cloid: str | None = None) -> dict:
        exchange = self._require_exchange("market_close_sized")
        sz_decimals = self._sz_decimals(symbol)
        size = floor_lot_size(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size floored to zero for {symbol} (sz_decimals={sz_decimals})")
        px = _safe_float(px)
        if px <= 0 or not math.isfinite(px):
            raise ValueError(f"no usable limit price for {symbol}")
        order_type = {"limit": {"tif": "Ioc"}}
        if cloid:
            return exchange.order(symbol, is_buy, size, px, order_type, reduce_only=bool(reduce_only), cloid=_to_cloid(cloid))
        return exchange.order(symbol, is_buy, size, px, order_type, reduce_only=bool(reduce_only))

    def lookup_fill_fee_by_oid(
        self,
        oid: int,
        since_ms: int,
        max_retries: int = 4,
        retry_delay_s: float = 0.5,
    ) -> dict:
        if not self._account_address:
            return {}
        attempt = 0
        while attempt < max_retries:
            try:
                fills = self._info.user_fills_by_time(self._account_address, since_ms)
            except _HLClientError as exc:
                if getattr(exc, "status_code", None) == 429:
                    print(
                        f"[WARN] userFills lookup got HTTP 429 for oid={oid}; not retrying",
                        file=sys.stderr,
                    )
                    return {}
                fills = None
            except Exception:
                fills = None
            if isinstance(fills, list):
                matched = [f for f in fills if isinstance(f, dict) and _safe_int(f.get("oid")) == int(oid)]
                if matched:
                    fee_total = 0.0
                    pnl_total = 0.0
                    for f in matched:
                        fee_total += _safe_float(f.get("fee"))
                        pnl_total += _safe_float(f.get("closedPnl"))
                    return {
                        "fee": fee_total,
                        "closed_pnl": pnl_total,
                        "count": len(matched),
                        "fills": matched,
                    }
            attempt += 1
            if attempt < max_retries:
                time.sleep(retry_delay_s)
        return {}

    def fills_summary_by_oid(
        self,
        oid: int,
        since_ms: int,
        max_retries: int = 4,
        retry_delay_s: float = 0.5,
    ) -> dict:
        if oid <= 0 or not self._account_address:
            return {}
        attempt = 0
        while attempt < max_retries:
            try:
                fills = self._info.user_fills_by_time(self._account_address, since_ms)
            except _HLClientError as exc:
                if getattr(exc, "status_code", None) == 429:
                    print(
                        f"[WARN] fills_summary lookup got HTTP 429 for oid={oid}; not retrying",
                        file=sys.stderr,
                    )
                    return {}
                fills = None
            except Exception:
                fills = None
            if isinstance(fills, list):
                matched = [
                    f for f in fills
                    if isinstance(f, dict) and _safe_int(f.get("oid")) == int(oid)
                ]
                if matched:
                    size_total = 0.0
                    notional_total = 0.0
                    fee_total = 0.0
                    for f in matched:
                        sz = _safe_float(f.get("sz"))
                        px = _safe_float(f.get("px"))
                        size_total += sz
                        notional_total += sz * px
                        fee_total += _safe_float(f.get("fee"))
                    avg_px = (notional_total / size_total) if size_total > 0 else 0.0
                    return {
                        "filled_size": size_total,
                        "avg_px": avg_px,
                        "fee": fee_total,
                        "count": len(matched),
                    }
            attempt += 1
            if attempt < max_retries:
                time.sleep(retry_delay_s)
        return {}

    def round_perps_trigger_px(self, symbol: str, px: float) -> float:
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return _round_perps_px(px, sz_decimals)

    def perps_trigger_px_tick(self, symbol: str, px: float) -> float:
        if px <= 0:
            return 0.0
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return 10.0 ** -_perps_px_decimals(px, sz_decimals)

    def place_stop_loss(
        self,
        symbol: str,
        sz: float,
        trigger_px: float,
        is_buy: bool,
        limit_slippage_pct: float = 5.0,
        cloid: str | None = None,
        require_exact: bool = False,
    ) -> dict:
        exchange = self._require_exchange("place_stop_loss")
        sz_decimals = self._sz_decimals(symbol)
        if require_exact:
            quote_request_unchanged(symbol, sz, trigger_px, sz_decimals)
        sz = floor_lot_size(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size floored to zero for {symbol} (sz_decimals={sz_decimals})")
        if trigger_px <= 0:
            raise ValueError(f"trigger_px must be > 0, got {trigger_px}")

        slip = max(limit_slippage_pct, 0.0) / 100.0
        if is_buy:
            limit_px = trigger_px * (1.0 + slip)
        else:
            limit_px = trigger_px * (1.0 - slip)
        limit_px = _round_perps_px(limit_px, sz_decimals)
        trigger_px = _round_perps_px(trigger_px, sz_decimals)

        order_type = {"trigger": {"triggerPx": trigger_px, "isMarket": True, "tpsl": "sl"}}
        if cloid:
            return exchange.order(
                symbol, is_buy, sz, limit_px, order_type, reduce_only=True, cloid=_to_cloid(cloid)
            )
        return exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def modify_stop_loss(
        self,
        symbol: str,
        oid: int,
        sz: float,
        trigger_px: float,
        is_buy: bool,
        limit_slippage_pct: float = 5.0,
    ) -> dict:
        exchange = self._require_exchange("modify_stop_loss")
        sz_decimals = self._sz_decimals(symbol)
        sz = floor_lot_size(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size floored to zero for {symbol} (sz_decimals={sz_decimals})")
        if trigger_px <= 0:
            raise ValueError(f"trigger_px must be > 0, got {trigger_px}")
        if oid <= 0:
            raise ValueError(f"oid must be > 0, got {oid}")
        slip = max(limit_slippage_pct, 0.0) / 100.0
        if is_buy:
            limit_px = trigger_px * (1.0 + slip)
        else:
            limit_px = trigger_px * (1.0 - slip)
        limit_px = _round_perps_px(limit_px, sz_decimals)
        trigger_px = _round_perps_px(trigger_px, sz_decimals)
        order_type = {"trigger": {"triggerPx": trigger_px, "isMarket": True, "tpsl": "sl"}}
        return exchange.modify_order(
            int(oid), symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def place_take_profit_limit(
        self,
        symbol: str,
        sz: float,
        limit_px: float,
        is_buy: bool,
    ) -> dict:
        exchange = self._require_exchange("place_take_profit_limit")
        sz_decimals = self._sz_decimals(symbol)
        sz = floor_lot_size(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size floored to zero for {symbol} (sz_decimals={sz_decimals})")
        if limit_px <= 0:
            raise ValueError(f"limit_px must be > 0, got {limit_px}")
        limit_px = _round_perps_px(limit_px, sz_decimals)
        order_type = {"limit": {"tif": "Gtc"}}
        return exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def floor_size(self, symbol: str, sz: float) -> float:
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return floor_lot_size(sz, sz_decimals)

    def round_size(self, symbol: str, sz: float) -> float:
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return round(sz, sz_decimals)

    def frontend_open_orders(self, symbol: str | None = None) -> list:
        if not self._account_address:
            return []
        orders = self._info.frontend_open_orders(self._account_address)
        out = []
        for order in orders or []:
            if not isinstance(order, dict):
                continue
            if symbol and order.get("coin") != symbol:
                continue
            out.append(order)
            for child in order.get("children") or []:
                if isinstance(child, dict):
                    out.append(child)
        return out

    def open_order_oids(self, symbol: str | None = None) -> set[int]:
        if not self._account_address:
            return set()
        orders = self._info.open_orders(self._account_address)
        out: set[int] = set()
        for order in orders or []:
            if not isinstance(order, dict):
                continue
            if symbol and order.get("coin") != symbol:
                continue
            oid = _safe_int(order.get("oid"))
            if oid:
                out.add(oid)
        return out

    def cancel_order_by_oid(self, symbol: str, oid: int) -> dict:
        exchange = self._require_exchange("cancel_order_by_oid")
        return exchange.cancel(symbol, int(oid))

    def cancel_trigger_order(self, symbol: str, oid: int) -> dict:
        return self.cancel_order_by_oid(symbol, oid)

    def update_leverage(self, leverage: int, symbol: str, is_cross: bool) -> dict:
        exchange = self._require_exchange("update_leverage")
        if leverage < 1:
            raise ValueError(f"leverage must be >= 1, got {leverage}")
        return exchange.update_leverage(int(leverage), symbol, bool(is_cross))

    def get_position_leverage(self, symbol: str) -> dict | None:
        if not self._account_address:
            return None
        try:
            user_state = self._info.user_state(self._account_address)
        except Exception as exc:
            print(
                f"[WARN] HL get_position_leverage({symbol}) user_state failed: {exc}",
                file=sys.stderr,
            )
            return None
        for asset_pos in user_state.get("assetPositions", []):
            pos = asset_pos.get("position", {}) or {}
            if pos.get("coin") != symbol:
                continue
            try:
                szi = float(pos.get("szi", 0) or 0)
            except (TypeError, ValueError):
                szi = 0.0
            if szi == 0:
                continue
            lev = pos.get("leverage", {}) or {}
            mode = lev.get("type")
            if mode not in ("isolated", "cross"):
                return None
            try:
                value = int(lev.get("value", 0) or 0)
            except (TypeError, ValueError):
                return None
            if value < 1:
                return None
            return {"margin_mode": mode, "leverage": value}
        return None
