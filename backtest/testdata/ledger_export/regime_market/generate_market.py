"""Deterministic market for regime-label comparison cases.

300 warm-up bars, then the same comparison window as market/ (2026-01-05T00:00Z
to 2026-01-06T20:00Z). The live regime lookback is at least 200 bars, so a
modeled gate needs this warm-up. The shared 60-bar market stays the booked
ledger. Not market data.
"""

import gzip
import hashlib
import io
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
HOUR_MS = 3_600_000
WINDOW_START_MS = 1767571200000
WARMUP = 300
WINDOW_BARS = 44
BARS = WARMUP + WINDOW_BARS
START_MS = WINDOW_START_MS - WARMUP * HOUR_MS
SEED = 1730


def _lcg(seed):
    state = seed
    while True:
        state = (6364136223846793005 * state + 1442695040888963407) % (1 << 64)
        yield (state >> 11) / float(1 << 53)


def _gzip_bytes(text: str) -> bytes:
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0, filename="") as fh:
        fh.write(text.encode("utf-8"))
    return buf.getvalue()


def _close(i, _rnd):
    # Climb, then drop from the first scored bar. The short ADX period turns
    # down while the longer period is still up, and the fast SMA crosses there.
    if i < 300:
        return 40000.0 + i * 20.0
    return 40000.0 + 300 * 20.0 - (i - 300) * 80.0


def candles_and_funding():
    rnd = _lcg(SEED)
    rows = ["timestamp,open,high,low,close,volume"]
    funding = ["timestamp,rate"]
    prev_close = round(_close(0, rnd), 1)
    for i in range(BARS):
        ts = START_MS + i * HOUR_MS
        close = round(_close(i, rnd), 1)
        open_ = prev_close
        high = round(max(open_, close) + 40.0 + 30.0 * next(rnd), 1)
        low = round(min(open_, close) - 40.0 - 30.0 * next(rnd), 1)
        volume = round(100.0 + 20.0 * next(rnd), 3)
        rows.append(f"{ts},{open_:.1f},{high:.1f},{low:.1f},{close:.1f},{volume:.3f}")
        rate = 0.0000125 + 0.000005 * (next(rnd) - 0.5)
        funding.append(f"{ts},{rate:.10f}")
        prev_close = close
    return "\n".join(rows) + "\n", "\n".join(funding) + "\n"


def write(path: str, data: bytes) -> str:
    with open(path, "wb") as fh:
        fh.write(data)
    return hashlib.sha256(data).hexdigest()


def main() -> int:
    candles, funding = candles_and_funding()
    meta = json.dumps({"universe": [{"name": "BTC", "szDecimals": 5}]}, indent=1, sort_keys=True) + "\n"
    candle_sha = write(os.path.join(HERE, "BTC_1h_candles.csv.gz"), _gzip_bytes(candles))
    funding_sha = write(os.path.join(HERE, "BTC_funding.csv.gz"), _gzip_bytes(funding))
    meta_sha = write(os.path.join(HERE, "venue_meta.json"), meta.encode("utf-8"))
    manifest = {
        "schema": "offline_candle_manifest/v1",
        "study": "ledger_compare_regime_fixture_1730",
        "venue": "hyperliquid",
        "provenance": {
            "kind": "proxy",
            "label": "synthetic deterministic test candles and hourly funding written by generate_market.py; not market data",
        },
        "interval": "1h",
        "warmup_bars": WARMUP,
        "windows": {
            "comparison": {
                "start": "2026-01-05T00:00:00Z",
                "end": "2026-01-06T20:00:00Z",
                "role": "comparison interval; start inclusive, end exclusive; warm-up covers the live regime lookback",
            },
        },
        "acquisition": {
            "source": "generate_market.py (no network access)",
            "note": "Deterministic series seeded with 1730; gzip mtime 0; rerunning the generator reproduces every byte.",
        },
        "costs": {
            "taker_fee_pct": 0.00045,
            "maker_fee_pct": 0.00015,
            "slippage_bps": 1.0,
            "min_notional_usd": 10.0,
            "min_notional_margin": 0.03,
        },
        "venue_reference": {
            "meta": {"path": "venue_meta.json", "sha256": meta_sha},
        },
        "datasets": [
            {
                "coin": "BTC",
                "symbol": "BTC/USDT",
                "size_decimals": 5,
                "half_spread_bps": 0.5,
                "candles": {"path": "BTC_1h_candles.csv.gz", "sha256": candle_sha},
                "funding": {"path": "BTC_funding.csv.gz", "sha256": funding_sha},
            },
        ],
    }
    with open(os.path.join(HERE, "manifest.json"), "w") as fh:
        fh.write(json.dumps(manifest, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
