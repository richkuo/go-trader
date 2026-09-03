
import json
import math
import os
import sys
import tempfile
import threading
import time
from decimal import Decimal, ROUND_DOWN

MAINNET_URL = "https://api.hyperliquid.xyz"
TESTNET_URL = "https://api.hyperliquid-testnet.xyz"

META_CACHE_PATH = "/tmp/hl_meta.json"
META_CACHE_TTL_S = 3600

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


def _round_perps_px(px: float, sz_decimals: int) -> float:
    if px <= 0:
        return px
    px_decimals = max(0, 6 - sz_decimals)
    log = math.floor(math.log10(abs(px)))
    sig_decimals = max(0, 5 - 1 - int(log))
    decimals = min(px_decimals, sig_decimals)
    return round(px, decimals)


def _floor_size(sz: float, sz_decimals: int) -> float:
    if sz <= 0:
        return sz
    quant = Decimal("1").scaleb(-max(sz_decimals, 0))
    return float(Decimal(str(sz)).quantize(quant, rounding=ROUND_DOWN))


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

    def __init__(self):
        if not _SDK_AVAILABLE:
            raise ImportError(
                "hyperliquid-python-sdk not installed. Run: uv sync"
            )

        secret = os.environ.get("HYPERLIQUID_SECRET_KEY", "")
        addr = os.environ.get("HYPERLIQUID_ACCOUNT_ADDRESS", "")
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

        self._info = self._build_info(base_url, allow_cache=True)
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
            _save_meta_cache(spot_meta, meta)
            self._cached_spot_meta, self._cached_meta = spot_meta, meta
            return _HLInfo(base_url=base_url, skip_ws=True, meta=meta,
                           spot_meta=_normalize_spot_meta(spot_meta))
        except Exception as exc:
            self._cached_spot_meta, self._cached_meta = None, None
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
        info = self._info
        resolved = self._resolve_sz_decimals(info, symbol) if info is not None else None
        if resolved is not None:
            return resolved
        if info is not None and isinstance(getattr(info, "asset_to_sz_decimals", None), dict) \
                and symbol in info.asset_to_sz_decimals:
            return info.asset_to_sz_decimals[symbol]
        if symbol in self._sz_decimals_misses:
            return 3
        try:
            self._info = self._build_info(self._base_url, allow_cache=False)
        except Exception as exc:
            print(f"[WARN] hl meta refresh failed for {symbol}: {exc}", file=sys.stderr)
            self._sz_decimals_misses.add(symbol)
            return 3
        info = self._info
        resolved = self._resolve_sz_decimals(info, symbol) if info is not None else None
        if resolved is not None:
            return resolved
        if info is not None and isinstance(getattr(info, "asset_to_sz_decimals", None), dict) \
                and symbol in info.asset_to_sz_decimals:
            return info.asset_to_sz_decimals[symbol]
        print(f"[WARN] sz_decimals not found for {symbol} after refresh, defaulting to 3", file=sys.stderr)
        self._sz_decimals_misses.add(symbol)
        return 3

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

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        interval_ms_map = {
            "1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000,
            "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000,
            "4h": 14_400_000, "8h": 28_800_000, "12h": 43_200_000,
            "1d": 86_400_000, "3d": 259_200_000, "1w": 604_800_000,
        }
        interval_ms = interval_ms_map.get(interval, 3_600_000)
        end_ms = int(time.time() * 1000)

        cache_enabled = _ohlcv_cache_enabled()
        cache_path = None
        if cache_enabled:
            cache_path = _ohlcv_cache_path(symbol, interval, limit)
            cached = _load_ohlcv_cache(cache_path, ttl_s=_ohlcv_cache_ttl(interval_ms))
            if cached is not None:
                return cached

        requested = limit + OHLCV_GAP_MARGIN
        result = []
        prev_count = -1
        stale_widens = 0
        for _ in range(OHLCV_MAX_EXTEND_PASSES):
            start_ms = end_ms - interval_ms * requested
            candles = self._info.candles_snapshot(symbol, interval, start_ms, end_ms)
            result = []
            for c in candles:
                result.append([
                    int(c.get("T", c.get("t", 0))),
                    float(c["o"]),
                    float(c["h"]),
                    float(c["l"]),
                    float(c["c"]),
                    float(c["v"]),
                ])
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
        if cache_enabled and result:
            _save_ohlcv_cache(result, cache_path)
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


    def market_open(self, symbol: str, is_buy: bool, size: float) -> dict:
        exchange = self._require_exchange("market_open")
        sz_decimals = self._sz_decimals(symbol)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return exchange.market_open(symbol, is_buy, size, None, 0.01)

    def limit_open(
        self,
        symbol: str,
        is_buy: bool,
        size: float,
        limit_px: float,
        tif: str = "Alo",
    ) -> dict:
        exchange = self._require_exchange("limit_open")
        sz_decimals = self._sz_decimals(symbol)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        if limit_px <= 0:
            raise ValueError(f"limit_px must be > 0, got {limit_px}")
        if tif not in ("Alo", "Gtc", "Ioc"):
            raise ValueError(f"unsupported tif {tif!r}, expected 'Alo', 'Gtc' or 'Ioc'")
        limit_px = _round_perps_px(limit_px, sz_decimals)
        order_type = {"limit": {"tif": tif}}
        return exchange.order(
            symbol, is_buy, size, limit_px, order_type, reduce_only=False
        )

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
        exchange = self._require_exchange("market_close")
        if sz is not None:
            sz_decimals = self._sz_decimals(symbol)
            sz = round(sz, sz_decimals)
            if sz <= 0:
                raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return exchange.market_close(symbol, sz)

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

    def place_stop_loss(
        self,
        symbol: str,
        sz: float,
        trigger_px: float,
        is_buy: bool,
        limit_slippage_pct: float = 5.0,
    ) -> dict:
        exchange = self._require_exchange("place_stop_loss")
        sz_decimals = self._sz_decimals(symbol)
        sz = round(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
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
        return exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
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
        sz = _floor_size(sz, sz_decimals)
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
        return _floor_size(sz, sz_decimals)

    def round_size(self, symbol: str, sz: float) -> float:
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return round(sz, sz_decimals)

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
