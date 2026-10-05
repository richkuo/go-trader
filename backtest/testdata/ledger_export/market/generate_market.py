import gzip
import hashlib
import io
import json
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
HOUR_MS = 3_600_000
START_MS = 1767312000000
BARS = 168
SEED = 1686


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


def candles_and_funding():
    rnd = _lcg(SEED)
    rows = ["timestamp,open,high,low,close,volume"]
    funding = ["timestamp,rate"]
    prev_close = 60000.0
    for i in range(BARS):
        ts = START_MS + i * HOUR_MS
        drift = 1600.0 * math.sin(i / 6.0) + 500.0 * math.sin(i / 2.1)
        target = 60000.0 + drift + 40.0 * (next(rnd) - 0.5)
        open_ = round(prev_close, 1)
        close = round(target, 1)
        high = round(max(open_, close) + 60.0 + 80.0 * next(rnd), 1)
        low = round(min(open_, close) - 60.0 - 80.0 * next(rnd), 1)
        volume = round(100.0 + 50.0 * next(rnd), 3)
        rows.append(f"{ts},{open_:.1f},{high:.1f},{low:.1f},{close:.1f},{volume:.3f}")
        rate = 0.0000125 + 0.00001 * (next(rnd) - 0.5)
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
        "study": "ledger_compare_fixture_1686",
        "venue": "hyperliquid",
        "provenance": {
            "kind": "proxy",
            "label": "synthetic deterministic test candles and hourly funding written by generate_market.py; not market data",
        },
        "interval": "1h",
        "warmup_bars": 60,
        "windows": {
            "comparison": {
                "start": "2026-01-05T00:00:00Z",
                "end": "2026-01-06T20:00:00Z",
                "role": "comparison interval for the booked-ledger fixture; start inclusive, end exclusive",
            },
        },
        "acquisition": {
            "source": "generate_market.py (no network access)",
            "note": "Deterministic 64-bit LCG seeded with 1686; gzip mtime 0; rerunning the generator reproduces every byte.",
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
