#!/usr/bin/env python3
import argparse
import json
import math
import os
import sys
import time
from types import SimpleNamespace

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "shared_scripts"))

import candle_archive

TOOL = "scripts/candle-archive-publish.py"
VERSION = "1"
HL_ROWS_PER_PAGE = 5000
CCXT_ROWS_PER_PAGE = 500
DEFAULT_MAX_REQUESTS = 20

USAGE = """Publish operator-provisioned candle history for the dashboard chart and trade review.

This tool runs outside the scheduler and outside the dashboard. Nothing in the
scheduler, the service units or update.sh starts it. Run it from a host or
cron job whose venue traffic is accounted apart from the trading feed, and name
that egress path with --egress-id.

Stage 1, fetch (network): read one verified opening-time range through the same
range readers shared_scripts/fetch_candles.py uses for --from, under a request
cap, and write the rows to a file:

    uv run --no-sync python scripts/candle-archive-publish.py fetch \\
        --platform hyperliquid --type perps --symbol ETH --timeframe 1h \\
        --from 2026-01-01T00:00:00Z --to 2026-03-01T00:00:00Z \\
        --egress-id research-host --out /var/tmp/eth-1h.json

Hyperliquid perps and CCXT spot markets are supported. OKX, TopStep and
Robinhood serve only a latest window and are refused.

Stage 2, build (offline): validate the row files, drop bars younger than the
finalization delay, write a new immutable generation, and switch CURRENT to it
by rename:

    uv run --no-sync python scripts/candle-archive-publish.py build \\
        --archive-dir /var/lib/go-trader-candles --publisher-id ops-host \\
        --expires-hours 48 --min-close-age-seconds 300 --keep 3 \\
        /var/tmp/eth-1h.json /var/tmp/btc-1h.json

Each build publishes a whole generation; a series missing from the row files is
missing from the new generation. Mount the archive directory read-only into
the scheduler and set dashboard_candles.archive_dir to it.
"""


def _parse_bound(raw, name):
    from fetch_candles import _parse_bound as parse

    return int(round(parse(raw, name).timestamp() * 1000))


def _identity(platform, ctype, symbol, timeframe):
    if platform in candle_archive.REFUSED_VENUES:
        raise candle_archive.ArchiveError(f"platform {platform} has no verified opening-time range read; format v1 refuses it")
    if platform == "manual":
        raise candle_archive.ArchiveError("platform manual has no historical candle source")
    if platform == "hyperliquid":
        if ctype not in ("perps", "manual"):
            raise candle_archive.ArchiveError(f"hyperliquid type {ctype!r} has no chart identity")
        network = "testnet" if os.environ.get("HYPERLIQUID_TESTNET") == "1" else "mainnet"
        return {"venue": "hyperliquid", "network": network, "instrument_class": "perps", "symbol": symbol, "timeframe": timeframe}
    exchange = platform or "binanceus"
    if "/" not in symbol and ctype in ("perps", "manual"):
        symbol = f"{symbol}/USDT"
    return {"venue": exchange, "network": "live", "instrument_class": "spot", "symbol": symbol, "timeframe": timeframe}


def cmd_fetch(args):
    if not args.egress_id.strip():
        raise candle_archive.ArchiveError("--egress-id is required")
    identity = _identity(args.platform, args.type, args.symbol, args.timeframe)
    if args.timeframe not in candle_archive.GRID_SECONDS:
        raise candle_archive.ArchiveError(f"timeframe {args.timeframe!r} has no fixed bar grid")
    start_ms = _parse_bound(args.from_time, "from")
    end_ms = _parse_bound(args.to_time, "to") if args.to_time else int(time.time() * 1000)
    if end_ms < start_ms:
        raise candle_archive.ArchiveError("--to is before --from")
    interval_ms = candle_archive.GRID_SECONDS[args.timeframe] * 1000
    per_page = HL_ROWS_PER_PAGE if args.platform == "hyperliquid" else CCXT_ROWS_PER_PAGE
    estimate = math.ceil(((end_ms - start_ms) // interval_ms + 1) / per_page)
    if estimate > args.max_requests:
        raise candle_archive.ArchiveError(
            f"the range needs about {estimate} requests, above --max-requests {args.max_requests}; split it into smaller ranges")
    from fetch_candles import _fetch_range

    ns = SimpleNamespace(platform=args.platform, type=args.type, symbol=args.symbol, timeframe=args.timeframe,
                         limit=0, mode="paper", from_time=args.from_time, to_time=args.to_time)
    rows, source, meta = _fetch_range(ns, start_ms, end_ms)
    if not meta.get("supported"):
        raise candle_archive.ArchiveError(f"{source} is not a verified opening-time range read")
    if meta.get("truncated") or meta.get("stalled"):
        raise candle_archive.ArchiveError(f"the range read from {source} was truncated or stalled; nothing was written")
    if int(meta.get("requests", 0)) > args.max_requests:
        raise candle_archive.ArchiveError(f"the range read sent {meta['requests']} requests, above --max-requests {args.max_requests}")
    clean = []
    for row in rows:
        open_ms = int(row[0])
        if start_ms <= open_ms <= end_ms:
            clean.append([open_ms] + [float(x) if x is not None else 0.0 for x in row[1:6]])
    observed = int(time.time() * 1000)
    doc = {
        **identity,
        "rows": clean,
        "history_start_known": bool(args.history_start_known),
        "source": {"kind": source, "detail": f"requests={meta.get('requests')} instrument={meta.get('instrument')}", "observed_at_ms": observed},
        "egress_id": args.egress_id,
    }
    tmp = args.out + ".tmp"
    with open(tmp, "w") as fh:
        json.dump(doc, fh, sort_keys=True, separators=(",", ":"))
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, args.out)
    print(json.dumps({"ok": True, "out": args.out, "rows": len(clean), "source": source, "requests": meta.get("requests")}))


def cmd_build(args):
    archive_dir = os.path.abspath(args.archive_dir)
    series = []
    egress = set()
    for path in args.rows:
        with open(path) as fh:
            doc = json.load(fh)
        egress.add(str(doc.get("egress_id") or ""))
        series.append(doc)
    if "" in egress or not egress:
        raise candle_archive.ArchiveError("every row file must carry the egress_id its fetch named")
    now_ms = args.now_ms if args.now_ms else int(time.time() * 1000)
    publisher = {"id": args.publisher_id, "tool": TOOL, "version": VERSION, "egress_id": ",".join(sorted(egress))}
    expires_ms = now_ms + int(args.expires_hours * 3600 * 1000)
    previous = None
    try:
        previous = candle_archive.read_current(archive_dir)
    except (OSError, candle_archive.ArchiveError):
        previous = None
    gen = candle_archive.build_generation(archive_dir, series, publisher, now_ms, expires_ms,
                                          int(args.min_close_age_seconds * 1000), args.chunk_bars)
    if not args.no_switch:
        candle_archive.set_current(archive_dir, gen)
    removed = []
    if args.keep > 0:
        removed = candle_archive.prune_generations(archive_dir, args.keep, protect={gen, previous} - {None})
    print(json.dumps({"ok": True, "generation": gen, "current": not args.no_switch, "previous": previous, "pruned": removed}))


def main():
    parser = argparse.ArgumentParser(description=USAGE, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("--platform", required=True)
    f.add_argument("--type", default="perps")
    f.add_argument("--symbol", required=True)
    f.add_argument("--timeframe", required=True)
    f.add_argument("--from", dest="from_time", required=True)
    f.add_argument("--to", dest="to_time", default="")
    f.add_argument("--egress-id", required=True)
    f.add_argument("--max-requests", type=int, default=DEFAULT_MAX_REQUESTS)
    f.add_argument("--history-start-known", action="store_true")
    f.add_argument("--out", required=True)
    b = sub.add_parser("build")
    b.add_argument("--archive-dir", required=True)
    b.add_argument("--publisher-id", required=True)
    b.add_argument("--expires-hours", type=float, default=48)
    b.add_argument("--min-close-age-seconds", type=float, default=300)
    b.add_argument("--chunk-bars", type=int, default=candle_archive.DEFAULT_CHUNK_BARS)
    b.add_argument("--keep", type=int, default=3)
    b.add_argument("--no-switch", action="store_true")
    b.add_argument("--now-ms", type=int, default=0)
    b.add_argument("rows", nargs="+")
    args = parser.parse_args()
    try:
        if args.cmd == "fetch":
            cmd_fetch(args)
        else:
            cmd_build(args)
    except (candle_archive.ArchiveError, OSError, ValueError) as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
