"""
Hyperliquid Perpetuals Exchange Adapter.

Supports paper (simulated fills using live prices, no credentials) and
live (real orders on Hyperliquid mainnet, wallet credentials required) modes.

Environment variables:
    HYPERLIQUID_SECRET_KEY       — private key for live trading (hex string)
    HYPERLIQUID_ACCOUNT_ADDRESS  — account address (inferred from key if omitted)
    HYPERLIQUID_TESTNET=1        — use testnet instead of mainnet
    GO_TRADER_HL_OHLCV_CACHE=0   — disable the per-cycle OHLCV /info cache (#839)
"""

import json
import math
import os
import sys
import tempfile
import time
from decimal import Decimal, ROUND_DOWN

MAINNET_URL = "https://api.hyperliquid.xyz"
TESTNET_URL = "https://api.hyperliquid-testnet.xyz"

# /info `spotMeta` + `meta` rarely change (HL lists new coins on the order of
# weeks). The SDK's Info constructor re-fetches both on every instantiation,
# which costs 4 /info calls per scheduler cycle (signal-check + sync-protection
# each spawn a subprocess). Cache the raw responses to disk and pass them into
# Info(..., meta=..., spot_meta=...) on cache hit so the SDK skips the network
# call entirely (#768).
META_CACHE_PATH = "/tmp/hl_meta.json"
META_CACHE_TTL_S = 3600  # 60 minutes

# OHLCV candles are re-fetched from /info by every strategy subprocess. With
# ~20 strategies per instance sharing a handful of asset+timeframe combos, the
# identical candle request is fired dozens of times per cycle from one IP,
# which trips HL's 429 rate limit and cascades into script-failure alerts
# (#839). Cache the transformed candles to disk keyed by (symbol, interval,
# limit) with a short TTL so the first strategy of a cycle fetches and the rest
# read from cache. /tmp is shared across an instance's subprocesses but
# isolated per systemd instance (PrivateTmp=true), so this dedups within an
# instance — exactly the per-IP burst we need to flatten. Disable via
# GO_TRADER_HL_OHLCV_CACHE=0. Mirrors the meta-cache pattern above.
OHLCV_CACHE_DIR = "/tmp"
OHLCV_CACHE_PREFIX = "hl_ohlcv_"
OHLCV_CACHE_TTL_S = 60
# Extra intervals requested beyond `limit` so sparse candleSnapshot gaps (#937)
# still yield at least `limit` rows before trimming to the most recent `limit`.
OHLCV_GAP_MARGIN = 50
# Hyperliquid's candleSnapshot caps the RETURNED COUNT (most-recent ~5000
# candles within the range), NOT the request span. The extend-until-limit loop
# (#947) treats this as the ceiling on obtainable rows: once the returned count
# reaches it, a wider window can't yield more, so we stop.
OHLCV_MAX_CANDLES = 5000
# Backstop on extend passes so a pathological dribble of new candles per widen
# can't loop unbounded; the count-plateau check is the normal terminator.
OHLCV_MAX_EXTEND_PASSES = 12

# SDK imports: defer to avoid module-level ImportError when SDK not installed.
# adapter.py is loaded with platforms/hyperliquid/ directly in sys.path (not
# platforms/), so `from hyperliquid.info import Info` resolves to the SDK
# package (site-packages), not this local directory.
#
# Info + Exchange are load-bearing — if either fails to import the adapter is
# unusable. API + ClientError power the cache-miss fetch and 429 short-circuit
# respectively; we degrade them gracefully in their own try blocks so a future
# SDK reshuffle of those submodules doesn't take the whole adapter dark when
# the core Info/Exchange surface is still intact (PR #769 review point 3).
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
    _HLAPI = None  # _build_info falls back to letting Info's constructor fetch

try:
    from hyperliquid.utils.error import ClientError as _HLClientError
except ImportError:
    # ClientError is what we match for HTTP 429 in lookup_fill_fee_by_oid.
    # When absent, fall back to a sentinel that never matches a real HL
    # error, so the 429 short-circuit silently degrades to the original
    # retry path rather than catching unrelated exceptions.
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
    """Round a perps price to HL's tick rules.

    Two constraints apply simultaneously:
      - At most (MAX_DECIMALS - sz_decimals) decimal places, where
        MAX_DECIMALS=6 for perps. Higher-priced assets (BTC sz_decimals=5)
        therefore allow only 1 decimal of price precision.
      - At most 5 significant figures.

    The earlier fixed-6-decimal round was fine for tiny-priced coins but
    routinely produced HL "price has too many decimals" rejections on
    majors, leaving the position unprotected with the SL slot consumed
    (#421 review point 5).
    """
    if px <= 0:
        return px
    px_decimals = max(0, 6 - sz_decimals)
    log = math.floor(math.log10(abs(px)))
    sig_decimals = max(0, 5 - 1 - int(log))
    decimals = min(px_decimals, sig_decimals)
    return round(px, decimals)


def _floor_size(sz: float, sz_decimals: int) -> float:
    """Floor order size to HL asset precision so reduce-only TPs never oversize."""
    if sz <= 0:
        return sz
    quant = Decimal("1").scaleb(-max(sz_decimals, 0))
    return float(Decimal(str(sz)).quantize(quant, rounding=ROUND_DOWN))


def _load_meta_cache(path: str = META_CACHE_PATH, ttl_s: int = META_CACHE_TTL_S, now: float = None):
    """Return (spot_meta, meta) from on-disk cache if fresh, else None.

    Returns None on any read/parse failure so callers fall through to a fresh
    fetch. Empty {} payloads are treated as cache misses — the SDK rejects them
    on parse, and we want a fresh fetch in that case.
    """
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
    """Persist (spot_meta, meta) atomically.

    Uses a temp file in the same directory + os.replace so concurrent writers
    (multiple go-trader instances on one host share /tmp/hl_meta.json) never
    leave a torn file. Failures are logged and swallowed — caching is an
    optimization, not a correctness requirement.
    """
    payload = {"ts": time.time(), "spot_meta": spot_meta, "meta": meta}
    dir_ = os.path.dirname(path) or "."
    fd = None
    tmp_path = None
    try:
        fd, tmp_path = tempfile.mkstemp(prefix=".hl_meta_", suffix=".json", dir=dir_)
        with os.fdopen(fd, "w") as f:
            fd = None  # ownership transferred to file object
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
    """OHLCV caching is on by default; GO_TRADER_HL_OHLCV_CACHE=0 disables it.

    Read at call time so tests (and an operator) can toggle it without
    reloading the module.
    """
    return os.environ.get("GO_TRADER_HL_OHLCV_CACHE", "1") != "0"


def _ohlcv_cache_ttl(interval_ms: int) -> int:
    """Cache freshness bound for a given candle interval.

    Capped at ``OHLCV_CACHE_TTL_S`` but never longer than half the bar, so a
    fast-interval strategy (e.g. 1m) can't read a snapshot more than half a
    bar stale even when ``effectiveStrategyIntervalSeconds`` starts a new
    cycle inside the TTL window. 1m → 30s, 3m+ → 60s, sub-minute scales down.
    """
    half_bar_s = max(1, interval_ms // 2000)
    return min(OHLCV_CACHE_TTL_S, half_bar_s)


def _ohlcv_cache_path(symbol: str, interval: str, limit: int,
                      cache_dir: str = None) -> str:
    """Per-(symbol, interval, limit) cache file path.

    Both ``symbol`` and ``interval`` are sanitized to alphanumerics +
    underscore so neither (spot symbols like ``PURR/USDC``/``@367``, or a
    stray ``/``/``..`` in an interval) can escape the cache directory or
    collide. ``cache_dir`` resolves to ``OHLCV_CACHE_DIR`` at call time (not
    bound as a default) so tests can repoint it via monkeypatch.
    """
    if cache_dir is None:
        cache_dir = OHLCV_CACHE_DIR
    safe_sym = "".join(c if c.isalnum() else "_" for c in str(symbol))
    safe_int = "".join(c if c.isalnum() else "_" for c in str(interval))
    return os.path.join(cache_dir, f"{OHLCV_CACHE_PREFIX}{safe_sym}_{safe_int}_{limit}.json")


def _load_ohlcv_cache(path: str, ttl_s: int = OHLCV_CACHE_TTL_S, now: float = None):
    """Return cached candles (list of [ts, o, h, l, c, v]) if fresh, else None.

    None on any read/parse/TTL failure so callers fall through to a live
    fetch. An empty candle list is never cached (insufficient-data results
    must not pin every strategy to the error path for the TTL window), so a
    present-but-empty payload is treated as a miss.
    """
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
    """Persist transformed candles atomically (temp file + os.replace).

    Mirrors _save_meta_cache: concurrent subprocess writers never observe a
    torn file, and failures are logged + swallowed since caching is an
    optimization, not a correctness requirement.
    """
    payload = {"ts": time.time(), "candles": candles}
    dir_ = os.path.dirname(path) or "."
    fd = None
    tmp_path = None
    try:
        fd, tmp_path = tempfile.mkstemp(prefix=".hl_ohlcv_", suffix=".json", dir=dir_)
        with os.fdopen(fd, "w") as f:
            fd = None  # ownership transferred to file object
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
    """Make ``spot_meta`` safe for the SDK's positional ``tokens[idx]`` lookup.

    The SDK's ``Info`` constructor resolves each spot pair's base/quote tokens
    via ``spot_meta["tokens"][idx]`` (info.py:47-49), treating the token's
    ``index`` as a LIST POSITION. Hyperliquid's token indices used to be dense
    (position == index) but have become sparse — e.g. token index 479 ("WARS")
    now lives at list position 459 — so the SDK's ``tokens[479]`` raises
    ``IndexError`` and kills ``HLInfo`` init for every strategy (#831). The
    token data is all present; only the positional assumption is wrong.

    We rebuild ``tokens`` into an index-aligned (dense) list so positional
    lookup resolves the correct token for every referenced index, and drop any
    spot pair whose token indices can't be resolved at all (defends against a
    genuinely truncated payload). Returns a shallow copy; the original is left
    untouched. Non-dict/list inputs pass through unchanged so a malformed
    payload still reaches the SDK's own validation.
    """
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

    # Drop spot pairs whose base/quote indices aren't resolvable.
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

    # Fast path: already dense + index-aligned (position == index) and nothing
    # to drop → return the original untouched so the cache round-trip is exact.
    aligned = max_index + 1 == len(tokens) and all(
        isinstance(t, dict) and t.get("index") == i for i, t in enumerate(tokens)
    )
    if aligned and not dropped:
        return spot_meta

    # Dense, index-aligned token list. Gap slots get a harmless placeholder
    # that no surviving pair references (unresolvable pairs were dropped above).
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
    """POST /info {type:spotMeta} + {type:meta} via the SDK's API base class.

    Returns (spot_meta, meta) — same raw shape the SDK's Info constructor
    consumes when passed via meta=/spot_meta= kwargs. Errors bubble. Raises
    RuntimeError when the SDK's API class isn't importable so callers fall
    back to letting the SDK's Info constructor do the fetch (preserves the
    pre-#768 path).
    """
    if _HLAPI is None:
        raise RuntimeError("hyperliquid.api.API unavailable; cannot prefetch meta")
    api = _HLAPI(base_url=base_url)
    spot_meta = api.post("/info", {"type": "spotMeta"})
    meta = api.post("/info", {"type": "meta", "dex": ""})
    return spot_meta, meta


class HyperliquidExchangeAdapter:
    """
    Exchange adapter for Hyperliquid perpetual futures.

    Paper mode:  no credentials needed; uses live Hyperliquid prices for simulation.
    Live mode:   requires HYPERLIQUID_SECRET_KEY; places real market orders.
    """

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

        self._info = self._build_info(base_url, allow_cache=True)
        self._account_address = addr
        self._exchange = None
        # Symbols we've already refreshed meta for and still couldn't find —
        # capped at one /info refresh per missing symbol per subprocess
        # lifetime, otherwise a typo or delisted asset would re-fetch meta
        # on every order operation. (PR #769 review point 2.)
        self._sz_decimals_misses: set[str] = set()

        if secret:
            try:
                import eth_account
                wallet = eth_account.Account.from_key(secret)
                account_addr = addr or wallet.address
                self._account_address = account_addr
                self._exchange = _HLExchange(
                    wallet, base_url=base_url, account_address=account_addr
                )
            except Exception as e:
                raise RuntimeError(
                    f"Failed to initialize Hyperliquid Exchange client: {e}"
                )

    def _build_info(self, base_url: str, allow_cache: bool):
        """Construct an SDK Info instance, using the /tmp/hl_meta.json cache
        when fresh so the SDK skips its two-call init storm (#768).

        On cache miss we POST /info {spotMeta, meta} ourselves (2 calls) and
        pass the raw responses to Info via meta=/spot_meta= kwargs so its
        constructor performs zero network calls. Net: 2 /info on miss, 0 on
        hit (vs. 2 every construction in the unpatched path).

        allow_cache=False forces a fresh fetch (used by _refresh_meta when a
        symbol miss indicates the cached universe is stale).
        """
        cached = _load_meta_cache() if allow_cache else None
        if cached is not None:
            spot_meta, meta = cached
            return _HLInfo(base_url=base_url, skip_ws=True, meta=meta,
                           spot_meta=_normalize_spot_meta(spot_meta))
        try:
            spot_meta, meta = _fetch_raw_meta(base_url)
            _save_meta_cache(spot_meta, meta)
            return _HLInfo(base_url=base_url, skip_ws=True, meta=meta,
                           spot_meta=_normalize_spot_meta(spot_meta))
        except Exception as exc:
            # Last-resort fallback: let the SDK's constructor fetch fresh.
            # Costs the same 2 /info as before this change; cache write failed
            # but trading must continue.
            print(f"[WARN] hl meta fetch failed ({exc}); falling back to SDK init", file=sys.stderr)
            return _HLInfo(base_url=base_url, skip_ws=True)

    def _sz_decimals(self, symbol: str) -> int:
        """Look up sz_decimals for ``symbol``, force-refreshing the cached
        meta if the symbol is missing.

        Silent fall-through to ``3`` on a missing symbol produced HL "price
        has too many decimals" rejections on high-priced assets like BTC
        (sz_decimals=5; allowed price decimals = 6-5 = 1). The guardrail
        (#768 fix #1): on cache hit, if the configured symbol isn't in
        ``asset_to_sz_decimals``, force a meta refresh once before falling
        back. A still-missing symbol after refresh logs a warning and uses
        the legacy default 3.
        """
        if self._info is not None and symbol in self._info.asset_to_sz_decimals:
            return self._info.asset_to_sz_decimals[symbol]
        # Already tried to refresh for this symbol earlier in this subprocess
        # and still couldn't find it — typo or genuinely unlisted asset; the
        # cached universe will not save us. Skip the redundant /info calls.
        if symbol in self._sz_decimals_misses:
            return 3
        # Symbol missing — could be a stale cached universe. Refresh once.
        try:
            self._info = self._build_info(self._base_url, allow_cache=False)
        except Exception as exc:
            print(f"[WARN] hl meta refresh failed for {symbol}: {exc}", file=sys.stderr)
            self._sz_decimals_misses.add(symbol)
            return 3
        if self._info is not None and symbol in self._info.asset_to_sz_decimals:
            return self._info.asset_to_sz_decimals[symbol]
        print(f"[WARN] sz_decimals not found for {symbol} after refresh, defaulting to 3", file=sys.stderr)
        self._sz_decimals_misses.add(symbol)
        return 3

    @property
    def is_live(self) -> bool:
        """True if Exchange client is initialized (live mode)."""
        return self._exchange is not None

    @property
    def mode(self) -> str:
        """'live' or 'paper'."""
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "hyperliquid"

    # ─────────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────────

    def get_spot_price(self, symbol: str) -> float:
        """Get current mid price for a coin (e.g. 'BTC')."""
        mids = self._info.all_mids()
        raw = mids.get(symbol, mids.get(symbol + "-PERP", "0"))
        return float(raw or 0)

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles from Hyperliquid.

        interval: "1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "8h", "12h", "1d"
        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        interval_ms_map = {
            "1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000,
            "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000,
            "4h": 14_400_000, "8h": 28_800_000, "12h": 43_200_000,
            "1d": 86_400_000, "3d": 259_200_000, "1w": 604_800_000,
        }
        interval_ms = interval_ms_map.get(interval, 3_600_000)
        end_ms = int(time.time() * 1000)

        # Cycle-scoped dedup: reuse a fresh on-disk snapshot so peer strategies
        # sharing this (symbol, interval, limit) don't each hit /info (#839).
        cache_enabled = _ohlcv_cache_enabled()
        cache_path = None
        if cache_enabled:
            cache_path = _ohlcv_cache_path(symbol, interval, limit)
            cached = _load_ohlcv_cache(cache_path, ttl_s=_ohlcv_cache_ttl(interval_ms))
            if cached is not None:
                return cached

        # Extend-until-limit (#947): start at `limit + OHLCV_GAP_MARGIN`
        # intervals and, if sparse candleSnapshot gaps still leave fewer than
        # `limit` rows, double the window and refetch. HL caps the *returned
        # count* (not the request span), so we widen the span until one of:
        #   - we have >= limit rows (done);
        #   - the returned count plateaus across two consecutive widens (the
        #     symbol's full history is in-window; a wider span can't add more);
        #   - the count hits OHLCV_MAX_CANDLES (HL's return cap — can't get more
        #     from a single call);
        #   - the window is empty (halted/untraded symbol won't populate by
        #     widening; empty is the caller's error path); or
        #   - the pass backstop trips (pathological dribble guard).
        # The plateau needs TWO consecutive zero-growth widens, not one: because
        # candleSnapshot omits no-trade bars, a single doubling can land entirely
        # inside an interior no-trade gap (e.g. an overnight stretch on a
        # session-traded illiquid symbol) and add nothing, while older bars sit
        # just one more doubling back. A 2-widen confirmation clears that single
        # dead window at the cost of ~1 extra /info call; a genuinely exhausted
        # or leading-gap symbol still terminates in a small bounded number of
        # passes (it never walks the full backstop ladder).
        # We deliberately do NOT persist a "short symbol" marker: a brand-new
        # listing accumulates candles over time and must be re-attempted on each
        # cache miss, not pinned short. Extra /info calls are bounded per call
        # and only on the cache-miss shortfall path.
        requested = limit + OHLCV_GAP_MARGIN
        result = []
        prev_count = -1
        stale_widens = 0
        for _ in range(OHLCV_MAX_EXTEND_PASSES):
            start_ms = end_ms - interval_ms * requested
            candles = self._info.candles_snapshot(symbol, interval, start_ms, end_ms)
            result = []
            for c in candles:
                # T = close time, t = open time; use T as the candle timestamp
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
            # Track consecutive zero-growth widens; one dead window (interior
            # gap) is not enough to declare history exhausted — require two.
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
            # The symbol/interval has fewer traded bars than the requested
            # lookback even after widening to its full available history.
            # Callers continue while len >= 30 (check_hyperliquid.py), so flag
            # the shortfall to stderr rather than letting indicators silently
            # warm up on fewer bars than requested (#937/#947).
            print(
                f"[WARN] hl ohlcv shortfall for {symbol} {interval}: got "
                f"{len(result)} of {limit} requested after extending the window "
                f"to the symbol's available history",
                file=sys.stderr,
            )
        # Never cache an empty result — insufficient-data fetches must keep
        # retrying live rather than pinning every peer to the error path.
        if cache_enabled and result:
            _save_ohlcv_cache(result, cache_path)
        return result

    def get_funding_rate(self, symbol: str) -> float:
        """Get current predicted funding rate for a coin (e.g. 'BTC').

        Returns the raw rate as a float (e.g. 0.0001 = 0.01% per 8h).
        Returns 0.0 if the symbol is not found or on error.
        """
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
        """Get historical funding rate snapshots for a coin.

        Args:
            symbol: Coin name (e.g. 'BTC').
            days: Number of days of history to fetch (default 7).

        Returns list of {"rate": float, "time": int} dicts, newest last.
        """
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
        """Get historical funding snapshots over an arbitrary range, paginated.

        ``funding_history`` returns at most ~500 records (oldest-first) per
        call — about 20 days of hourly funding — so ``get_funding_history``
        silently truncates longer windows. This walks forward page by page
        until ``end_ms`` (default: now) is covered or the API stops making
        progress.

        Args:
            symbol: Coin name (e.g. 'BTC').
            start_ms: Range start, Unix ms.
            end_ms: Range end, Unix ms (default now).

        Returns list of {"rate": float, "time": int} dicts, oldest first,
        de-duplicated on time. Empty list on any API failure.
        """
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

    # ─────────────────────────────────────────────
    # Account data (requires HYPERLIQUID_ACCOUNT_ADDRESS)
    # ─────────────────────────────────────────────

    def get_open_positions(self) -> list:
        """
        Get open perp positions for the configured account.
        Returns list of dicts: {coin, size, entry_price, unrealized_pnl}.
        """
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

    # ─────────────────────────────────────────────
    # Order execution (live mode only)
    # ─────────────────────────────────────────────

    def market_open(self, symbol: str, is_buy: bool, size: float) -> dict:
        """
        Place a market order to open/add to a position.
        Only available in live mode; raises RuntimeError in paper mode.
        Returns raw SDK response dict.
        """
        if not self._exchange:
            raise RuntimeError(
                "market_open requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        # Round to asset's tick precision to avoid float_to_wire rounding error
        sz_decimals = self._sz_decimals(symbol)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return self._exchange.market_open(symbol, is_buy, size, None, 0.01)

    def limit_open(
        self,
        symbol: str,
        is_buy: bool,
        size: float,
        limit_px: float,
        tif: str = "Alo",
    ) -> dict:
        """Place a NON-reduce-only resting limit order to open a position (#883).

        Unlike ``market_open`` (immediate taker fill) this rests on the book
        until the price reaches ``limit_px``. ``tif`` defaults to ``Alo``
        (add-liquidity-only / post-only) so the order is guaranteed to rest as a
        maker and never crosses into an accidental taker fill — HL rejects an
        ``Alo`` order whose price is already marketable, which surfaces a
        mis-priced entry to the operator instead of silently filling. Pass
        ``tif="Gtc"`` to allow an immediately-marketable price to fill.

        ``reduce_only=False`` is the net-new piece: every other ``order`` call in
        this adapter (stop-loss, take-profit) is reduce-only. Only available in
        live mode; raises RuntimeError in paper mode.
        Returns the raw SDK response dict.
        """
        if not self._exchange:
            raise RuntimeError(
                "limit_open requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
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
        return self._exchange.order(
            symbol, is_buy, size, limit_px, order_type, reduce_only=False
        )

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
        """
        Close an open perp position for a symbol (reduce-only).

        When ``sz`` is None, closes the full on-chain position (SDK default).
        When ``sz`` is set, submits a reduce-only market order for that coin
        quantity only — used for shared-wallet per-strategy circuit breakers
        (#356).

        Only available in live mode; raises RuntimeError in paper mode.
        Returns raw SDK response dict.
        """
        if not self._exchange:
            raise RuntimeError(
                "market_close requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        if sz is not None:
            # Round to asset's tick precision to avoid float_to_wire rounding error (#425)
            sz_decimals = self._sz_decimals(symbol)
            sz = round(sz, sz_decimals)
            if sz <= 0:
                raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return self._exchange.market_close(symbol, sz)

    def lookup_fill_fee_by_oid(
        self,
        oid: int,
        since_ms: int,
        max_retries: int = 4,
        retry_delay_s: float = 0.5,
    ) -> dict:
        """Look up the actual exchange fee for a filled order via the userFills API.

        HL's order placement response (`market_open` / `market_close`) does not
        include the `fee` field — the modeled fee in the trade record drifts
        from the on-chain balance over many trades (#585). This helper queries
        the indexer-backed `userFills` endpoint to retrieve the real fee.

        Returns a dict with summed `fee` and `closed_pnl` across all fills
        sharing the OID (a single market order can fragment into multiple
        partial fills at different price levels). Empty dict when no fills
        match within the retry budget.

        Indexer lag: fills can take several hundred ms to surface after the
        order is placed. We retry up to `max_retries` times with
        `retry_delay_s` between attempts. Total worst-case delay is
        ~max_retries * retry_delay_s.
        """
        if not self._account_address:
            return {}
        attempt = 0
        while attempt < max_retries:
            try:
                fills = self._info.user_fills_by_time(self._account_address, since_ms)
            except _HLClientError as exc:
                # 429 is a multi-second cool-down, not the sub-second indexer
                # lag the retry budget exists for. Hammering through 4 retries
                # at 0.5s intervals just deepens the rate-limit hole and turns
                # one cycle's burst into a sustained outage (#768 fix #2). Bail
                # immediately; callers fall through to modeled-fee / reconcile
                # defaults. The over-close safety net is preserved because
                # `_oid_filled_externally` treats {} as "no fill observed",
                # not "OID confirmed unfilled".
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
        """Summarise the cumulative on-chain fills for a resting order (#883).

        ``lookup_fill_fee_by_oid`` returns only fee/closed_pnl/count, which is
        enough for the market-order post-fill path but NOT for tracking a
        resting limit order that fills incrementally — the scheduler needs the
        cumulative filled **size** and a size-weighted average **price** to grow
        the tracked position and re-size its protection each cycle.

        Returns ``{"filled_size", "avg_px", "fee", "count"}`` summed across all
        partial fills sharing ``oid``. Empty dict when no fills match within the
        retry budget (caller treats {} as "no fill observed yet", never as
        "confirmed zero" — the over-adopt hazard is avoided by combining this
        with an ``open_order_oids`` check). ``avg_px`` is size-weighted across
        legs; 0 when the summed size is 0.
        """
        if oid <= 0 or not self._account_address:
            return {}
        attempt = 0
        while attempt < max_retries:
            try:
                fills = self._info.user_fills_by_time(self._account_address, since_ms)
            except _HLClientError as exc:
                # Mirror lookup_fill_fee_by_oid: a 429 is a multi-second
                # cooldown, not the sub-second indexer lag the retry budget
                # targets — bail immediately so we don't deepen the rate limit.
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
        """Public wrapper around HL's per-asset price-tick rounding.

        Callers that need to record the post-rounding trigger price (for PnL
        bookkeeping when the SL fills) can pre-round before calling
        ``place_stop_loss``; rounding is idempotent.
        """
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
        """Place a reduce-only stop-loss trigger order (#412).

        ``is_buy`` is the direction of the triggered order itself — a long
        position's stop-loss is a SELL (is_buy=False); a short's is a BUY
        (is_buy=True). HL requires a ``limit_px`` as the worst acceptable
        fill price; a market-trigger uses a wide band off the trigger
        (default 5%) so slippage around the stop doesn't reject the fill.

        HL's open-order limit is 1000 per account (scales to 5000 with volume).
        When ≥1000 orders are open, new trigger / reduce-only orders are rejected.
        The scheduler detects this via isHLOpenOrderCapRejection and escalates
        to CRITICAL + notifier — no proactive client-side counter is required.
        """
        if not self._exchange:
            raise RuntimeError(
                "place_stop_loss requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
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
        # HL perps: prices use at most (MAX_DECIMALS - sz_decimals) decimals
        # AND at most 5 significant figures. Fixed-6-decimal rounding here
        # was rejected by HL on high-priced assets like BTC (sz_decimals=5,
        # so px_decimals=1) — the trigger sat resting only because the order
        # got rejected (#421 review point 5).
        limit_px = _round_perps_px(limit_px, sz_decimals)
        trigger_px = _round_perps_px(trigger_px, sz_decimals)

        order_type = {"trigger": {"triggerPx": trigger_px, "isMarket": True, "tpsl": "sl"}}
        return self._exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def place_take_profit_limit(
        self,
        symbol: str,
        sz: float,
        limit_px: float,
        is_buy: bool,
    ) -> dict:
        """Place a reduce-only take-profit limit order (#601)."""
        if not self._exchange:
            raise RuntimeError(
                "place_take_profit_limit requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        sz_decimals = self._sz_decimals(symbol)
        sz = _floor_size(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size floored to zero for {symbol} (sz_decimals={sz_decimals})")
        if limit_px <= 0:
            raise ValueError(f"limit_px must be > 0, got {limit_px}")
        limit_px = _round_perps_px(limit_px, sz_decimals)
        order_type = {"limit": {"tif": "Gtc"}}
        return self._exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def floor_size(self, symbol: str, sz: float) -> float:
        """Exposes the same lot-precision flooring `place_take_profit_limit`
        applies internally, so callers can pre-compute the on-chain size each
        tier will occupy and absorb the remainder into a final tier."""
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return _floor_size(sz, sz_decimals)

    def round_size(self, symbol: str, sz: float) -> float:
        """Round sz to the asset's lot precision (nearest, not floor).

        Use this to normalize a virtual qty that may have drifted just below a
        lot boundary due to float64 subtraction in Go (e.g. 0.011 - 0.010 =
        0.0009999...).  place_stop_loss already uses round(); this makes TP
        tier sizing consistent.
        """
        sz_decimals = self._sz_decimals(symbol) if self._info else 3
        return round(sz, sz_decimals)

    def open_order_oids(self, symbol: str | None = None) -> set[int]:
        """Return currently open order OIDs, optionally filtered by coin (#601).

        Raises whatever the underlying SDK raises — callers in
        check_hyperliquid.py treat a raise as "open-orders fetch failed,
        defer placement decisions" so wrapping it in try/except here would
        silently coerce uncertainty into "no open orders" and produce the
        over-place hazard we're trying to avoid.
        """
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
        """Cancel any resting order (trigger or limit) by OID.

        HL's cancel endpoint is order-type-agnostic — it accepts the OID and
        figures out the underlying order kind from the book. Trigger orders
        (stop-loss, take-profit-trigger) and limit orders (reduce-only TP
        limits placed via place_take_profit_limit) are both cancellable
        through this single primitive (#604 review #4).
        """
        if not self._exchange:
            raise RuntimeError(
                "cancel_order_by_oid requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        return self._exchange.cancel(symbol, int(oid))

    # Backwards-compatible alias. The original name implied only trigger
    # orders were supported; in practice HL's cancel works for any order
    # type. New code should call cancel_order_by_oid; existing callers can
    # keep using cancel_trigger_order without a rename churn.
    def cancel_trigger_order(self, symbol: str, oid: int) -> dict:
        return self.cancel_order_by_oid(symbol, oid)

    def update_leverage(self, leverage: int, symbol: str, is_cross: bool) -> dict:
        """Set leverage and margin mode (cross/isolated) for ``symbol`` (#486).

        HL's SDK takes both fields in one call, so callers always pass both.
        Fails closed: HL rejects this when there is an open position on the
        coin, so the scheduler must only invoke it when opening from flat —
        or when ``get_position_leverage`` confirms the on-chain state already
        matches the desired (mode, leverage) pair (#491).
        """
        if not self._exchange:
            raise RuntimeError(
                "update_leverage requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        if leverage < 1:
            raise ValueError(f"leverage must be >= 1, got {leverage}")
        return self._exchange.update_leverage(int(leverage), symbol, bool(is_cross))

    def get_position_leverage(self, symbol: str) -> dict | None:
        """Return ``{"margin_mode": "isolated"|"cross", "leverage": int}`` for the
        on-chain position on ``symbol`` if one exists, else ``None`` (#491).

        HL aggregates positions per coin per account, so two go-trader
        strategies sharing a coin land on the same on-chain position. When
        strategy A has already pinned (mode, leverage) on the coin, strategy
        B can use this to detect the existing state and skip a redundant
        ``update_leverage`` call — HL rejects mode/leverage changes while a
        position is open, so the redundant call would fail-closed and abort
        B's order. ``None`` means HL has no open position for ``symbol``;
        ``update_leverage`` is then safe to call.
        """
        if not self._account_address:
            return None
        try:
            user_state = self._info.user_state(self._account_address)
        except Exception as exc:
            # Don't swallow silently: a transient `info` failure is
            # indistinguishable from "no position" to the caller, which would
            # then call update_leverage and HL would reject it. Surface the
            # cause to stderr so operators can see *why* the fallback ran.
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
