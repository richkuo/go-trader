import gzip
import hashlib
import importlib.util
import io
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BASE = os.path.join(os.path.dirname(HERE), "market", "generate_market.py")
RAISED_HIGHS = {
    1767610800000: ("62124.2", "62125.0"),
    1767625200000: ("61624.5", "62127.0"),
}


def _base_module():
    spec = importlib.util.spec_from_file_location("ledger_fixture_base_market", BASE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _gzip_bytes(text: str) -> bytes:
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0, filename="") as fh:
        fh.write(text.encode("utf-8"))
    return buf.getvalue()


def _raise_highs(candles: str) -> str:
    lines = candles.splitlines()
    seen = set()
    out = [lines[0]]
    for line in lines[1:]:
        ts, open_, high, low, close, volume = line.split(",")
        key = int(ts)
        if key in RAISED_HIGHS:
            old, new = RAISED_HIGHS[key]
            if high != old:
                raise SystemExit(f"bar {key} high is {high}, expected {old}; market/ changed")
            if float(new) < max(float(open_), float(close)):
                raise SystemExit(f"bar {key} raised high {new} is below its open or close")
            high = new
            seen.add(key)
        out.append(",".join((ts, open_, high, low, close, volume)))
    if seen != set(RAISED_HIGHS):
        raise SystemExit(f"bars {sorted(set(RAISED_HIGHS) - seen)} are missing from market/")
    return "\n".join(out) + "\n"


def write(path: str, data: bytes) -> str:
    with open(path, "wb") as fh:
        fh.write(data)
    return hashlib.sha256(data).hexdigest()


def main() -> int:
    base = _base_module()
    candles, funding = base.candles_and_funding()
    with open(os.path.join(os.path.dirname(BASE), "venue_meta.json"), "rb") as fh:
        meta = fh.read()
    candle_sha = write(os.path.join(HERE, "BTC_1h_candles.csv.gz"), _gzip_bytes(_raise_highs(candles)))
    funding_sha = write(os.path.join(HERE, "BTC_funding.csv.gz"), _gzip_bytes(funding))
    meta_sha = write(os.path.join(HERE, "venue_meta.json"), meta)
    manifest = {
        "schema": "offline_candle_manifest/v1",
        "study": "ledger_compare_resting_fixture_1727",
        "venue": "hyperliquid",
        "provenance": {
            "kind": "proxy",
            "label": "derived from market/ with two highs raised; not market data",
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
            "note": "market/generate_market.py candles and funding with the 2026-01-05T11:00Z high raised from "
                    "62124.2 to 62125.0 (a touch of a whole-dollar limit) and the 2026-01-05T15:00Z high raised "
                    "from 61624.5 to 62127.0 (a two-tick wick with the close back inside); gzip mtime 0; rerunning "
                    "the generator reproduces every byte.",
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
