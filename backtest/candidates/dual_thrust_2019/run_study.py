#!/usr/bin/env python3

import argparse
import concurrent.futures
import copy
import gzip
import hashlib
import io
import json
import math
import os
import shutil
import socket
import statistics
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
REL = os.path.relpath(HERE, REPO)
for _p in (os.path.join(REPO, "shared_tools"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)

CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
HOUR_MS = 3_600_000
INTERVAL_MS = {"1h": 3_600_000, "4h": 14_400_000}
POPULATIONS = ("primary_4h", "forward_4h", "descriptive_1h")
PRIMARY = "primary_4h"
COSTS = ("base", "stress")
ORIGINAL_COINS = ("BTC", "ETH", "SOL")
CANDLE_HEADER = ["timestamp", "open", "high", "low", "close", "volume"]
FUNDING_HEADER = ["timestamp", "rate"]
PROVENANCE_LABEL = "Hyperliquid mainnet perpetual candles from the public info API (candleSnapshot); closed bars only"
SOURCE_ROOTS = ("shared_strategies/open", "shared_strategies/close", "shared_tools")
SOURCE_EXTRA = (
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/registry_loader.py",
    "backtest/run_backtest.py",
    "backtest/candidates/dual_thrust_2019/run_study.py",
)
METRIC_KEYS = ("sharpe", "return_pct", "max_dd_pct", "ddadj")
EXEC_METRIC_KEYS = ("net_pnl_usd", "fees_usd", "funding_pnl_usd", "traded_notional_usd")


class Paths:
    def __init__(self, root: str = HERE):
        self.root = os.path.abspath(root)
        self.protocol = os.path.join(self.root, "protocol.json")
        self.lock = os.path.join(self.root, "protocol_lock.json")
        self.inventory = os.path.join(self.root, "study_manifest.json")
        self.screen = os.path.join(self.root, "screen.json")
        self.results = os.path.join(self.root, "results.json")
        self.verification = os.path.join(self.root, "verification.json")
        self.data = os.path.join(self.root, "data")

    def rel(self, path: str) -> str:
        return os.path.relpath(path, self.root)

    def manifest(self, proto: dict, pop: str) -> str:
        return os.path.join(self.root, proto["populations"][pop]["manifest"])


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_json(path: str):
    with open(path) as fh:
        return json.load(fh)


def write_json(path: str, obj) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True, default=str)
        fh.write("\n")


def write_gz_csv(path: str, header: list, rows: list) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    text = ",".join(header) + "\n" + "".join(",".join(str(v) for v in r) + "\n" for r in rows)
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text.encode("utf-8"))
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())


def read_gz_rows(path: str) -> tuple:
    with gzip.open(path, "rt") as fh:
        lines = fh.read().splitlines()
    return lines[0].split(","), [ln.split(",") for ln in lines[1:] if ln]


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").value // 1_000_000)


def ms_str(ms) -> str:
    import pandas as pd
    return str(pd.to_datetime(int(ms), unit="ms"))


def git(*args) -> str:
    return subprocess.check_output(["git", "-C", REPO] + list(args), text=True, stderr=subprocess.DEVNULL).strip()


def git_head() -> str:
    try:
        head = git("rev-parse", "HEAD")
        dirty = git("status", "--porcelain", "--untracked-files=no")
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def source_files() -> list:
    out = list(SOURCE_EXTRA)
    for root in SOURCE_ROOTS:
        for dirpath, dirnames, filenames in os.walk(os.path.join(REPO, root)):
            dirnames[:] = sorted(d for d in dirnames if d != "__pycache__" and not d.startswith("."))
            for f in sorted(filenames):
                if f.endswith(".py") and not f.startswith("test_") and f != "conftest.py":
                    out.append(os.path.relpath(os.path.join(dirpath, f), REPO))
    return sorted(set(out))


def source_sha256() -> dict:
    return {p: sha256_file(os.path.join(REPO, p)) for p in source_files()}


def protocol(paths: Paths) -> dict:
    return load_json(paths.protocol)


def candle_checks(rows: list, interval: str, start_ms: int, end_ms: int, warmup: int) -> dict:
    step = INTERVAL_MS[interval]
    reasons = []
    ts = []
    for r in rows:
        try:
            ts.append(int(r[0]))
        except (ValueError, IndexError):
            reasons.append(f"non-integer timestamp {r[:1]!r}")
    if any(b <= a for a, b in zip(ts, ts[1:])):
        reasons.append("timestamps are not strictly increasing")
    off_grid = sum(1 for t in ts if t % step)
    if off_grid:
        reasons.append(f"{off_grid} bars off the UTC {interval} grid")
    by_ts = {}
    for t, r in zip(ts, rows):
        by_ts[t] = r
    want_window = list(range(start_ms, end_ms, step))
    want_warm = list(range(start_ms - warmup * step, start_ms, step))
    missing_window = [t for t in want_window if t not in by_ts]
    missing_warm = [t for t in want_warm if t not in by_ts]
    extra = sorted(t for t in ts if start_ms <= t < end_ms and (t - start_ms) % step)
    if missing_window:
        reasons.append(f"{len(missing_window)} window bars missing (first {ms_str(missing_window[0])})")
    if missing_warm:
        reasons.append(f"{len(missing_warm)} warm-up bars missing (first {ms_str(missing_warm[0])})")
    if extra:
        reasons.append(f"{len(extra)} unexpected bars inside the window")
    invalid = []
    for t in want_warm + want_window:
        r = by_ts.get(t)
        if r is None:
            continue
        try:
            o, h, l, c, v = (float(x) for x in r[1:6])
        except (ValueError, TypeError):
            invalid.append(t)
            continue
        if not all(math.isfinite(x) for x in (o, h, l, c, v)) or min(o, h, l, c) <= 0 or v < 0 \
                or h < max(o, c) or l > min(o, c) or h < l:
            invalid.append(t)
    if invalid:
        reasons.append(f"{len(invalid)} invalid OHLCV bars (first {ms_str(invalid[0])})")
    return {
        "interval": interval,
        "rows": len(rows),
        "first_bar": ms_str(ts[0]) if ts else None,
        "last_bar": ms_str(ts[-1]) if ts else None,
        "window": [ms_str(start_ms), ms_str(end_ms)],
        "expected_window_bars": len(want_window),
        "present_window_bars": len(want_window) - len(missing_window),
        "warmup_bars": warmup,
        "warmup_range": [ms_str(want_warm[0]), ms_str(want_warm[-1])] if want_warm else None,
        "present_warmup_bars": len(want_warm) - len(missing_warm),
        "invalid_bars": len(invalid),
        "complete": not reasons,
        "reasons": reasons,
    }


def funding_checks(rows: list, start_ms: int, end_ms: int) -> dict:
    reasons = []
    hours = set()
    nonfinite = 0
    ts = []
    for r in rows:
        try:
            t, rate = int(r[0]), float(r[1])
        except (ValueError, IndexError):
            nonfinite += 1
            continue
        ts.append(t)
        if not math.isfinite(rate):
            nonfinite += 1
            continue
        if start_ms <= t < end_ms:
            hours.add((t - start_ms) // HOUR_MS)
    if any(b <= a for a, b in zip(ts, ts[1:])):
        reasons.append("funding timestamps are not strictly increasing")
    expected = (end_ms - start_ms) // HOUR_MS
    missing = [h for h in range(expected) if h not in hours]
    if nonfinite:
        reasons.append(f"{nonfinite} non-numeric or non-finite funding rows")
    if missing:
        reasons.append(f"{len(missing)} funding hours missing (first {ms_str(start_ms + missing[0] * HOUR_MS)})")
    return {
        "window": [ms_str(start_ms), ms_str(end_ms)],
        "expected_hours": expected,
        "present_hours": expected - len(missing),
        "missing_hours": len(missing),
        "rows": len(rows),
        "complete": not reasons,
        "reasons": reasons,
    }


def post_info(payload: dict):
    import offline_manifest as om
    return om._post_info(payload)


def fetch_candles(coin: str, interval: str, start: str, end: str) -> list:
    import offline_manifest as om
    df = om._fetch_hl_candles(coin, interval, to_ms(start), to_ms(end))
    return [[int(r.timestamp), r.open, r.high, r.low, r.close, r.volume] for r in df.itertuples(index=False)]


def fetch_funding(coin: str, start: str, end: str, db_path: str) -> list:
    from funding_fetcher import load_cached_funding
    box = {}
    df = load_cached_funding(coin, start, end_date=end, db_path=db_path, source_box=box)
    if box.get("fetch_error"):
        raise SystemExit(f"{coin} funding acquisition failed: {box['fetch_error']}; nothing was written")
    if df is None or df.empty:
        return []
    s, e = to_ms(start), to_ms(end)
    df = df[(df["timestamp"] >= s) & (df["timestamp"] < e)].sort_values("timestamp").drop_duplicates("timestamp")
    return [[int(t), repr(float(r))] for t, r in zip(df["timestamp"], df["rate"])]


def named_candidate(coin: str, screened: list, not_screened: list, excluded: list) -> dict:
    for s in screened:
        if s["coin"] == coin:
            return {"rank": s["rank"], "selected": s["selected"],
                    "reasons": s["candles"]["reasons"] + s["funding"]["reasons"]}
    for n in not_screened:
        if n["coin"] == coin:
            return {"rank": n["rank"], "selected": False, "reasons": ["ranked below the last selection; not screened"]}
    return {"rank": None, "selected": False,
            "reasons": [r["reason"] for r in excluded if r["coin"] == coin] or ["not in the venue universe"]}


def acquire(paths: Paths) -> None:
    if os.environ.get("HYPERLIQUID_SECRET_KEY"):
        raise SystemExit("refusing to acquire research data with HYPERLIQUID_SECRET_KEY set")
    proto = protocol(paths)
    existing = [p for p in (paths.lock, paths.results, paths.inventory, paths.screen, paths.data)
                + tuple(paths.manifest(proto, pop) for pop in POPULATIONS) if os.path.exists(p)]
    if existing:
        raise SystemExit("refusing to acquire over existing study inputs or scores: "
                         + ", ".join(paths.rel(p) for p in existing))
    acq, scr = proto["acquisition"], proto["screen"]
    pwin = proto["populations"][PRIMARY]["window"]
    ps, pe = to_ms(pwin[0]), to_ms(pwin[1])
    warm = proto["warmup_bars"]
    staging = tempfile.mkdtemp(prefix=".acquire-", dir=paths.root)
    files = {}
    try:
        observed_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        meta, ctxs = post_info({"type": "metaAndAssetCtxs"})
        files["data/venue/meta_and_asset_ctxs.json"] = json.dumps([meta, ctxs], sort_keys=True, indent=1) + "\n"
        files["data/venue/venue_meta.json"] = json.dumps(meta, sort_keys=True, indent=1) + "\n"
        universe = meta["universe"]
        if len(universe) != len(ctxs):
            raise SystemExit("metaAndAssetCtxs universe and contexts differ in length")
        excluded, ranked = [], []
        for entry, ctx in zip(universe, ctxs):
            name = entry["name"]
            try:
                vol = float(ctx.get("dayNtlVlm"))
            except (TypeError, ValueError):
                vol = float("nan")
            if entry.get("isDelisted"):
                excluded.append({"coin": name, "reason": "isDelisted", "day_ntl_vlm": ctx.get("dayNtlVlm")})
            elif name in scr["excluded_coins"]:
                excluded.append({"coin": name, "reason": "original study coin", "day_ntl_vlm": ctx.get("dayNtlVlm")})
            elif not math.isfinite(vol):
                excluded.append({"coin": name, "reason": "non-finite dayNtlVlm", "day_ntl_vlm": ctx.get("dayNtlVlm")})
            else:
                ranked.append((vol, name, entry))
        ranked.sort(key=lambda x: (-x[0], x[1]))
        db_path = os.path.join(staging, "funding.db")
        screened, selected = [], []
        cache4, cachef = {}, {}
        for rank, (vol, name, entry) in enumerate(ranked, 1):
            if len(selected) >= scr["max_coins"]:
                break
            print(f"[screen] {rank:3d} {name} dayNtlVlm {vol:,.0f}", flush=True)
            candles = fetch_candles(name, "4h", *acq["candles_4h"])
            funding = fetch_funding(name, *acq["funding"], db_path=db_path)
            cc = candle_checks(candles, "4h", ps, pe, warm)
            fc = funding_checks(funding, ps, pe)
            ok = cc["complete"] and fc["complete"]
            screened.append({"rank": rank, "coin": name, "day_ntl_vlm": vol, "sz_decimals": entry.get("szDecimals"),
                             "candles": cc, "funding": fc, "selected": ok,
                             "fetched_candles_sha256": sha256_bytes(json.dumps(candles).encode()),
                             "fetched_funding_sha256": sha256_bytes(json.dumps(funding).encode())})
            if ok:
                selected.append(name)
                cache4[name], cachef[name] = candles, funding
        not_screened = [{"rank": i + 1, "coin": n, "day_ntl_vlm": v} for i, (v, n, _) in enumerate(ranked)
                        if n not in {s["coin"] for s in screened}]
        for coin in ORIGINAL_COINS:
            print(f"[acquire] {coin} 4h and funding", flush=True)
            cache4[coin] = fetch_candles(coin, "4h", *acq["candles_4h"])
            cachef[coin] = fetch_funding(coin, *acq["funding"], db_path=db_path)
        roster_1h = selected + list(ORIGINAL_COINS)
        cache1 = {}
        for coin in roster_1h:
            print(f"[acquire] {coin} 1h", flush=True)
            cache1[coin] = fetch_candles(coin, "1h", *acq["candles_1h"])
        books = {}
        for coin in roster_1h:
            book = post_info({"type": "l2Book", "coin": coin})
            levels = book.get("levels") or [[], []]
            bid = float(levels[0][0]["px"]) if levels[0] else float("nan")
            ask = float(levels[1][0]["px"]) if levels[1] else float("nan")
            mid = (bid + ask) / 2.0
            books[coin] = {"time": book.get("time"), "best_bid": bid, "best_ask": ask,
                           "half_spread_bps": (ask - bid) / 2.0 / mid * 10_000.0}
        files["data/venue/venue_l2_snapshot.json"] = json.dumps(books, sort_keys=True, indent=1) + "\n"
        sz = {e["name"]: e["szDecimals"] for e in universe}
        acquired_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        rows_by_file = {}
        for coin in sorted(cache4):
            rows_by_file[f"data/venue/{coin}_4h_candles.csv.gz"] = (CANDLE_HEADER, cache4[coin])
            rows_by_file[f"data/venue/{coin}_funding.csv.gz"] = (FUNDING_HEADER, cachef[coin])
        for coin in sorted(cache1):
            rows_by_file[f"data/venue/{coin}_1h_candles.csv.gz"] = (CANDLE_HEADER, cache1[coin])
        for rel, (header, rows) in rows_by_file.items():
            write_gz_csv(os.path.join(staging, rel), header, rows)
        for rel, text in files.items():
            os.makedirs(os.path.dirname(os.path.join(staging, rel)), exist_ok=True)
            with open(os.path.join(staging, rel), "w") as fh:
                fh.write(text)
        screen = {
            "study": proto["study"], "observed_at_utc": observed_at,
            "liquidity_source": scr["liquidity_source"],
            "liquidity_response": {"path": "data/venue/meta_and_asset_ctxs.json",
                                   "sha256": sha256_file(os.path.join(staging, "data/venue/meta_and_asset_ctxs.json"))},
            "rule": scr["rule"], "max_coins": scr["max_coins"], "excluded": sorted(excluded, key=lambda x: x["coin"]),
            "screened": screened, "not_screened": not_screened, "selected": selected,
            "named_candidates": {c: named_candidate(c, screened, not_screened, excluded) for c in ("HYPE", "BNB")},
            "rosters": {"primary_4h": selected, "forward_4h": list(ORIGINAL_COINS), "descriptive_1h": roster_1h},
            "forbidden_inputs": scr["forbidden_inputs"],
        }
        write_json(os.path.join(staging, "screen.json"), screen)
        old_manifest = load_json(os.path.join(REPO, proto["references"]["original_venue_manifest"]["path"]))
        manifests = {}
        for pop in POPULATIONS:
            cfg = proto["populations"][pop]
            interval = cfg["interval"]
            roster = screen["rosters"][pop]
            if not roster:
                continue
            datasets = []
            for coin in roster:
                cpath = f"data/venue/{coin}_{interval}_candles.csv.gz"
                fpath = f"data/venue/{coin}_funding.csv.gz"
                datasets.append({
                    "coin": coin, "symbol": f"{coin}/USDT", "size_decimals": sz[coin],
                    "half_spread_bps": round(books[coin]["half_spread_bps"], 4),
                    "candles": {"path": cpath, "sha256": sha256_file(os.path.join(staging, cpath))},
                    "funding": {"path": fpath, "sha256": sha256_file(os.path.join(staging, fpath))},
                })
            manifests[pop] = {
                "schema": "offline_candle_manifest/v1", "study": proto["study"], "venue": "hyperliquid",
                "provenance": {"kind": "venue", "label": PROVENANCE_LABEL},
                "interval": interval, "warmup_bars": warm,
                "windows": {cfg["window_name"]: {"start": cfg["window"][0], "end": cfg["window"][1],
                                                 "role": cfg["role"]}},
                "acquisition": {"source": acq["source"], "start": acq[f"candles_{interval}"][0],
                                "end": acq[f"candles_{interval}"][1], "acquired_at": acquired_at,
                                "note": "written by run_study.py acquire; the venue serves only the most recent "
                                        "5,000 candles per interval"},
                "costs": dict(proto["costs"]),
                "venue_reference": {
                    "meta": {"path": "data/venue/venue_meta.json",
                             "sha256": sha256_file(os.path.join(staging, "data/venue/venue_meta.json"))},
                    "l2_snapshot": {"path": "data/venue/venue_l2_snapshot.json",
                                    "sha256": sha256_file(os.path.join(staging, "data/venue/venue_l2_snapshot.json"))},
                    "documents": copy.deepcopy(old_manifest["venue_reference"]["documents"]),
                },
                "datasets": datasets,
            }
            write_json(os.path.join(staging, cfg["manifest"]), manifests[pop])
        inv = {"study": proto["study"], "acquired_at_utc": acquired_at, "observed_at_utc": observed_at,
               "source_commit_at_acquisition": git_head(), "files": {}, "manifests": {},
               "screen": {"path": "screen.json", "sha256": sha256_file(os.path.join(staging, "screen.json"))}}
        for pop, m in manifests.items():
            inv["manifests"][pop] = {"path": proto["populations"][pop]["manifest"],
                                     "sha256": sha256_file(os.path.join(staging, proto["populations"][pop]["manifest"]))}
        for rel in sorted(list(rows_by_file) + list(files)):
            full = os.path.join(staging, rel)
            entry = {"sha256": sha256_file(full), "bytes": os.path.getsize(full),
                     "source": acq["source"], "acquired_at_utc": acquired_at}
            base = os.path.basename(rel)
            if rel in rows_by_file:
                header, rows = rows_by_file[rel]
                coin = base.split("_")[0]
                entry.update({"rows": len(rows), "coin": coin,
                              "first_row": ms_str(rows[0][0]) if rows else None,
                              "last_row": ms_str(rows[-1][0]) if rows else None})
                if "_candles" in base:
                    interval = base.split("_")[1]
                    entry["kind"] = "candles"
                    entry["interval"] = interval
                    entry["acquired_range"] = acq[f"candles_{interval}"]
                    entry["populations"] = {}
                    for pop in POPULATIONS:
                        cfg = proto["populations"][pop]
                        if cfg["interval"] == interval and coin in screen["rosters"][pop]:
                            s = to_ms(cfg["window"][0])
                            entry["populations"][pop] = {
                                "scored_range": cfg["window"],
                                "warmup_range": [ms_str(s - warm * INTERVAL_MS[interval]),
                                                 ms_str(s - INTERVAL_MS[interval])]}
                else:
                    entry["kind"] = "funding"
                    entry["acquired_range"] = acq["funding"]
                    entry["populations"] = {pop: {"scored_range": proto["populations"][pop]["window"]}
                                            for pop in POPULATIONS if coin in screen["rosters"][pop]}
            else:
                entry["kind"] = "venue_reference"
                entry["observed_at_utc"] = observed_at if "ctxs" in base else acquired_at
            inv["files"][rel] = entry
        write_json(os.path.join(staging, "study_manifest.json"), inv)
        moves = list(rows_by_file) + list(files) + ["screen.json", "study_manifest.json"] + \
            [proto["populations"][p]["manifest"] for p in manifests]
        for rel in moves:
            dst = os.path.join(paths.root, rel)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            os.replace(os.path.join(staging, rel), dst)
    finally:
        shutil.rmtree(staging, ignore_errors=True)
    print(f"selected primary roster: {selected}")


def registered_files(paths: Paths) -> dict:
    inv = load_json(paths.inventory)
    return inv["files"]


def verify_inputs(paths: Paths, proto: dict) -> dict:
    if not os.path.exists(paths.inventory):
        raise SystemExit("study_manifest.json (the input inventory) is missing")
    inv = load_json(paths.inventory)
    for rel, meta in sorted(inv["files"].items()):
        full = os.path.join(paths.root, rel)
        if not os.path.isfile(full):
            raise SystemExit(f"registered input {rel} is missing")
        got = sha256_file(full)
        if got != meta["sha256"]:
            raise SystemExit(f"registered input {rel} sha256 {got} != inventory {meta['sha256']}")
    present = set()
    for dirpath, dirnames, filenames in os.walk(paths.data):
        dirnames[:] = [d for d in dirnames if d != "__pycache__"]
        for f in filenames:
            present.add(os.path.relpath(os.path.join(dirpath, f), paths.root))
    extra = sorted(present - set(inv["files"]))
    if extra:
        raise SystemExit("unregistered files under data/: " + ", ".join(extra))
    if sha256_file(paths.screen) != inv["screen"]["sha256"]:
        raise SystemExit("screen.json differs from the inventory")
    for pop, m in inv["manifests"].items():
        if m["path"] != proto["populations"][pop]["manifest"]:
            raise SystemExit(f"{pop}: inventory manifest path {m['path']} is not the protocol's")
        if sha256_file(os.path.join(paths.root, m["path"])) != m["sha256"]:
            raise SystemExit(f"{pop}: manifest {m['path']} differs from the inventory")
    for pop in POPULATIONS:
        mp = paths.manifest(proto, pop)
        if os.path.exists(mp) and pop not in inv["manifests"]:
            raise SystemExit(f"{pop}: unregistered manifest {paths.rel(mp)}")
    return inv


def lock_record(paths: Paths, proto: dict) -> dict:
    inv = verify_inputs(paths, proto)
    return {
        "protocol_sha256": sha256_file(paths.protocol),
        "inventory_sha256": sha256_file(paths.inventory),
        "screen_sha256": sha256_file(paths.screen),
        "manifests_sha256": {pop: m["sha256"] for pop, m in sorted(inv["manifests"].items())},
        "inputs_sha256": {rel: m["sha256"] for rel, m in sorted(inv["files"].items())},
        "source_sha256": source_sha256(),
        "locked_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": git_head(),
        "note": "written by run_study.py lock before any leg was scored; run refuses a different protocol, inventory, "
                "screen, manifest, input file or scoring source, and requires this file committed before any score",
    }


def lock(paths: Paths) -> None:
    proto = protocol(paths)
    if os.path.exists(paths.lock):
        held = load_json(paths.lock)
        fresh = lock_record(paths, proto)
        diff = [k for k in ("protocol_sha256", "inventory_sha256", "screen_sha256", "manifests_sha256",
                            "inputs_sha256", "source_sha256") if held.get(k) != fresh[k]]
        if diff:
            raise SystemExit(f"a lock exists and differs in {diff}; a lock is never replaced")
        print(f"already locked at {held['locked_at_utc']}")
        return
    if os.path.exists(paths.results):
        raise SystemExit("results exist; the lock must precede every score")
    write_json(paths.lock, lock_record(paths, proto))
    print(f"locked {paths.rel(paths.lock)}")


def git_lock_order(paths: Paths, locked: list) -> dict:
    rel_root = os.path.relpath(paths.root, REPO)
    lock_rel = os.path.join(rel_root, "protocol_lock.json")
    try:
        commits = git("log", "--format=%H", "--", lock_rel).split()
    except subprocess.CalledProcessError:
        commits = []
    if not commits:
        raise SystemExit("protocol_lock.json is not committed; commit the lock and every locked input before scoring")
    if len(commits) != 1:
        raise SystemExit(f"protocol_lock.json changed in {len(commits)} commits; a lock is never replaced")
    lock_commit = commits[0]
    for name in ("results.json", "REPORT.md", "verification.json"):
        try:
            git("cat-file", "-e", f"{lock_commit}:{os.path.join(rel_root, name)}")
            raise SystemExit(f"{name} already exists in the lock commit {lock_commit}; the lock must precede scores")
        except subprocess.CalledProcessError:
            pass
    rels = [os.path.join(rel_root, os.path.relpath(p, paths.root)) for p in locked]
    tracked = set(git("ls-files", "--", *rels).split("\n")) if rels else set()
    untracked = sorted(set(rels) - tracked)
    if untracked:
        raise SystemExit("locked files are not committed: " + ", ".join(untracked[:5]))
    dirty = git("status", "--porcelain", "--", *rels)
    if dirty:
        raise SystemExit("locked files differ from the committed versions:\n" + dirty)
    src_dirty = git("status", "--porcelain", "--untracked-files=no", "--", *SOURCE_ROOTS, *SOURCE_EXTRA)
    if src_dirty:
        raise SystemExit("scoring sources have uncommitted changes:\n" + src_dirty)
    return {"lock_commit": lock_commit, "head": git("rev-parse", "HEAD")}


def require_lock(paths: Paths, git_check: bool = True) -> dict:
    if not os.path.exists(paths.lock):
        raise SystemExit("protocol_lock.json is missing; run `run_study.py lock` before scoring")
    proto = protocol(paths)
    held = load_json(paths.lock)
    if held.get("protocol_sha256") != sha256_file(paths.protocol):
        raise SystemExit("protocol.json differs from the lock")
    if held.get("inventory_sha256") != sha256_file(paths.inventory):
        raise SystemExit("study_manifest.json (input inventory) differs from the lock")
    if held.get("screen_sha256") != sha256_file(paths.screen):
        raise SystemExit("screen.json differs from the lock")
    inv = verify_inputs(paths, proto)
    if held.get("manifests_sha256") != {pop: m["sha256"] for pop, m in sorted(inv["manifests"].items())}:
        raise SystemExit("manifest hashes differ from the lock")
    if held.get("inputs_sha256") != {rel: m["sha256"] for rel, m in sorted(inv["files"].items())}:
        raise SystemExit("input file hashes differ from the lock")
    now = source_sha256()
    changed = sorted(k for k in set(now) | set(held.get("source_sha256") or {})
                     if now.get(k) != (held.get("source_sha256") or {}).get(k))
    if changed:
        raise SystemExit("scoring sources differ from the lock (check out the lock's source commit to reproduce): "
                         + ", ".join(changed[:8]))
    out = {"lock": held}
    if git_check:
        locked = [paths.protocol, paths.lock, paths.inventory, paths.screen] + \
            [os.path.join(paths.root, m["path"]) for m in inv["manifests"].values()] + \
            [os.path.join(paths.root, rel) for rel in inv["files"]]
        out["git"] = git_lock_order(paths, locked)
    return out


def check_references(proto: dict) -> dict:
    refs = proto["references"]
    out = {}
    for key in ("original_results", "original_protocol", "original_venue_manifest"):
        path = os.path.join(REPO, refs[key]["path"])
        got = sha256_file(path)
        if got != refs[key]["sha256"]:
            raise SystemExit(f"{refs[key]['path']} sha256 {got} != protocol reference {refs[key]['sha256']}")
        out[key] = got
    res = load_json(os.path.join(REPO, refs["original_results"]["path"]))
    venue = res["groups"]["venue_4h"]
    if venue["selected_params"] != proto["candidate_params"]:
        raise SystemExit("candidate_params differ from the original venue selection")
    if venue["comparator_params"] != proto["comparator_params"]:
        raise SystemExit("comparator_params differ from the original study")
    if res["incumbents"] != proto["incumbents"]:
        raise SystemExit("incumbent identities differ from the original study")
    oproto = load_json(os.path.join(REPO, refs["original_protocol"]["path"]))
    if oproto["contract"] != proto["contract"] or oproto["capital_usd"] != proto["capital_usd"] \
            or oproto["intrabar_resolution"] != proto["intrabar_resolution"]:
        raise SystemExit("contract, capital or intrabar resolution differs from the original protocol")
    if oproto["venue"]["cost_multipliers"] != proto["cost_multipliers"]:
        raise SystemExit("cost multipliers differ from the original protocol")
    oman = load_json(os.path.join(REPO, refs["original_venue_manifest"]["path"]))
    if oman["costs"] != proto["costs"]:
        raise SystemExit("costs differ from the original venue manifest")
    src = refs["incumbent_params_source"]
    if res["source_sha256"].get(src["file"]) != src["sha256"]:
        raise SystemExit("the original results do not record the incumbent source hash")
    try:
        blob = subprocess.check_output(["git", "-C", REPO, "show", f"{src['commit']}:{src['file']}"])
        commit_check = "verified" if sha256_bytes(blob) == src["sha256"] else "MISMATCH"
    except (OSError, subprocess.CalledProcessError):
        commit_check = "commit unavailable in this clone; the recorded hash was checked against results.json only"
    if commit_check == "MISMATCH":
        raise SystemExit(f"{src['file']} at {src['commit']} does not match the recorded sha256")
    out["incumbent_source_commit_check"] = commit_check
    from registry_loader import load_registry
    reg = load_registry(proto["registry"])
    drift = {n: reg.STRATEGY_REGISTRY[n]["default_params"] for n in proto["incumbents"]
             if reg.STRATEGY_REGISTRY[n]["default_params"] != proto["incumbent_params"][n]}
    drift.update({n: reg.STRATEGY_REGISTRY[n]["default_params"] for n in proto["comparators"]
                  if reg.STRATEGY_REGISTRY[n]["default_params"] != proto["comparator_params"][n]})
    out["current_default_params_differ"] = drift
    out["evaluator_sources_changed_since_original"] = sorted(
        p for p, h in res["source_sha256"].items()
        if not os.path.exists(os.path.join(REPO, p)) or sha256_file(os.path.join(REPO, p)) != h)
    return out


def refuse_network(*_a, **_k):
    raise RuntimeError("frozen study scoring: network access is blocked")


def block_network() -> None:
    socket.socket.connect = refuse_network
    socket.socket.connect_ex = refuse_network
    socket.create_connection = refuse_network
    socket.getaddrinfo = refuse_network
    import data_fetcher
    import funding_fetcher
    import offline_manifest
    data_fetcher.fetch_full_history = refuse_network
    data_fetcher.fetch_ohlcv = refuse_network
    funding_fetcher._hl_adapter = refuse_network
    offline_manifest._post_info = refuse_network


def fresh_cache(cache_db: str) -> str:
    path = os.path.abspath(cache_db)
    if os.path.exists(path):
        raise SystemExit(f"refusing to reuse an existing research cache {path}; pass a new path")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    os.environ[CACHE_ENV] = path
    import storage
    storage.init_db(path)
    return path


_W = {}


def worker_init(root: str, cache_db: str) -> None:
    os.environ[CACHE_ENV] = cache_db
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()
    import eval_windows as ew
    import offline_manifest as om
    from registry_loader import load_registry
    paths = Paths(root)
    proto = protocol(paths)
    _W.clear()
    _W.update({"ew": ew, "om": om, "proto": proto, "reg": load_registry(proto["registry"]), "manifests": {}})
    for pop in POPULATIONS:
        mp = paths.manifest(proto, pop)
        if os.path.exists(mp):
            _W["manifests"][pop] = om.load_manifest(mp, verify=True)


def arm_names(proto: dict) -> list:
    return ["candidate"] + list(proto["comparators"]) + list(proto["incumbents"])


def trim_leg(leg: dict, keep_positions: bool) -> dict:
    out = {k: leg.get(k) for k in ("sharpe", "return_pct", "max_dd_pct", "ddadj", "trades", "bh_return_pct",
                                   "liquidated", "span_days")}
    out["funding_incomplete"] = bool(leg.get("funding_incomplete"))
    ex = dict(leg.get("execution") or {})
    if not keep_positions:
        ex.pop("position_list", None)
    out["execution"] = ex
    man = leg.get("manifest") or {}
    out["manifest"] = {k: man.get(k) for k in ("provenance", "dataset", "candles_sha256", "funding_sha256",
                                               "candle_coverage", "funding_coverage", "cost_multiplier",
                                               "cost_model", "execution_spec")}
    return out


def run_unit(unit: tuple) -> tuple:
    pop, arm, cost, coin = unit
    ew, proto, reg = _W["ew"], _W["proto"], _W["reg"]
    manifest = _W["manifests"][pop]
    cfg = proto["populations"][pop]
    interval = cfg["interval"]
    ctx = {"manifest": manifest, "window": cfg["window_name"], "cost_multiplier": proto["cost_multipliers"][cost]}
    contract = proto["contract"]
    try:
        if arm == "candidate":
            spec = dict(copy.deepcopy(contract), name=proto["candidate"], type=proto["strategy_type"],
                        params=dict(proto["candidate_params"], expected_interval_ms=proto["expected_interval_ms"][interval]))
            ew.validate_candidate(spec)
            leg = ew.run_candidate_leg(reg, spec, coin, interval, None, proto["capital_usd"], keep_trades=True,
                                       intrabar_resolution=proto["intrabar_resolution"], manifest_ctx=ctx)
        elif arm in proto["comparators"]:
            spec = dict(copy.deepcopy(contract), name=arm, type=proto["strategy_type"],
                        params=dict(proto["comparator_params"][arm]))
            ew.validate_candidate(spec)
            leg = ew.run_candidate_leg(reg, spec, coin, interval, None, proto["capital_usd"], keep_trades=True,
                                       intrabar_resolution=proto["intrabar_resolution"], manifest_ctx=ctx)
        else:
            kw = ew.incumbent_contract_kwargs(contract)
            leg = ew.run_leg(reg, arm, dict(proto["incumbent_params"][arm]), coin, interval, None,
                             capital=proto["capital_usd"], intrabar_resolution=proto["intrabar_resolution"],
                             manifest_ctx=ctx, **kw)
        if leg is None:
            return unit, {"error": "the evaluator returned no leg"}
        return unit, trim_leg(leg, keep_positions=arm == "candidate" or arm in proto["comparators"])
    except Exception as exc:
        return unit, {"error": f"{type(exc).__name__}: {exc}"}


def entry_audit(proto: dict, manifest: dict, pop: str, legs: dict) -> dict:
    import pandas as pd
    import offline_manifest as om
    from registry_loader import load_registry
    reg = load_registry(proto["registry"])
    cfg = proto["populations"][pop]
    interval = cfg["interval"]
    params = dict(proto["candidate_params"], expected_interval_ms=proto["expected_interval_ms"][interval])
    win = manifest["windows"][cfg["window_name"]]
    rows, failures = [], []
    for ds in manifest["datasets"]:
        leg = legs.get(ds["key"])
        if not leg or "error" in leg:
            continue
        frame, _, _ = om.window_frame(manifest, ds, cfg["window_name"])
        out = reg.apply_strategy(proto["candidate"], frame, dict(params))
        for p in leg["execution"].get("position_list") or []:
            entry = pd.Timestamp(p["entry_date"])
            exit_ = pd.Timestamp(p["exit_date"])
            want = 1 if p["side"] == "long" else -1
            item = {"dataset": ds["key"], "side": p["side"], "fill_bar": str(entry), "exit_bar": str(exit_)}
            if entry not in out.index:
                failures.append(dict(item, problem="fill bar not in the evaluated frame"))
                continue
            fill = out.index.get_loc(entry)
            sig = fill - 1
            rec = out.iloc[sig]
            item.update({"signal_bar": str(out.index[sig]), "reason": str(rec.get("dt_reason")),
                         "anchor": float(rec["dt_anchor"]), "upper": float(rec["dt_upper"]),
                         "lower": float(rec["dt_lower"]), "close": float(rec["close"])})
            if int(entry.value // 1_000_000) < win["start_ms"]:
                failures.append(dict(item, problem="entry before the window start (warm-up trade)"))
            elif int(exit_.value // 1_000_000) >= win["end_ms"]:
                failures.append(dict(item, problem="exit at or after the window end"))
            elif int(rec["signal"]) != want or not bool(rec["dt_valid"]):
                failures.append(dict(item, problem=f"no valid {p['side']} signal on the prior closed bar"))
            elif int(out.index[sig].value // 1_000_000) < win["start_ms"] - INTERVAL_MS[interval]:
                failures.append(dict(item, problem="decision bar inside the warm-up"))
            rows.append(item)
    return {"entries": len(rows), "failures": failures, "ok": not failures, "rows": rows}


def input_validation(proto: dict, manifest_path: str, pop: str, roster: list) -> dict:
    import offline_manifest as om
    cfg = proto["populations"][pop]
    reasons, per = [], {}
    if not roster:
        return {"ok": False, "reasons": ["the frozen roster is empty"], "datasets": {}, "manifest": None}
    if not os.path.exists(manifest_path):
        return {"ok": False, "reasons": [f"{os.path.basename(manifest_path)} is missing"], "datasets": {},
                "manifest": None}
    try:
        manifest = om.load_manifest(manifest_path, verify=True)
    except om.ManifestError as exc:
        return {"ok": False, "reasons": [f"manifest refused: {exc}"], "datasets": {}, "manifest": None}
    if manifest["provenance"].get("kind") != "venue":
        reasons.append(f"provenance kind {manifest['provenance'].get('kind')!r} is not venue")
    if manifest["interval"] != cfg["interval"]:
        reasons.append(f"manifest interval {manifest['interval']} != {cfg['interval']}")
    if manifest["warmup_bars"] != proto["warmup_bars"]:
        reasons.append(f"manifest warm-up {manifest['warmup_bars']} != {proto['warmup_bars']}")
    win = manifest["windows"].get(cfg["window_name"])
    if not win or [win["start"], win["end"]] != cfg["window"] or len(manifest["windows"]) != 1:
        reasons.append("manifest windows differ from the protocol window")
    if manifest["costs"] != proto["costs"]:
        reasons.append("manifest costs differ from the protocol")
    coins = [d["coin"] for d in manifest["datasets"]]
    if sorted(coins) != sorted(roster) or len(set(coins)) != len(coins):
        missing = sorted(set(roster) - set(coins))
        unexpected = sorted(set(coins) - set(roster))
        reasons.append(f"dataset roster differs from the frozen roster (missing {missing}, unexpected {unexpected})")
    if win:
        s, e = to_ms(cfg["window"][0]), to_ms(cfg["window"][1])
        for d in manifest["datasets"]:
            _, crow = read_gz_rows(d["candles"]["abs_path"])
            cc = candle_checks(crow, cfg["interval"], s, e, proto["warmup_bars"])
            if d.get("funding") is None:
                fc = {"complete": False, "reasons": ["no funding series attached"]}
            else:
                _, frow = read_gz_rows(d["funding"]["abs_path"])
                fc = funding_checks(frow, s, e)
            per[d["key"]] = {"candles": cc, "funding": fc, "half_spread_bps": d["half_spread_bps"],
                             "size_decimals": d["size_decimals"], "candles_sha256": d["candles"]["sha256"],
                             "funding_sha256": (d.get("funding") or {}).get("sha256")}
            for r in cc["reasons"]:
                reasons.append(f"{d['key']} candles: {r}")
            for r in fc["reasons"]:
                reasons.append(f"{d['key']} funding: {r}")
    return {"ok": not reasons, "reasons": reasons, "datasets": per, "manifest": manifest}


def finite(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def position_clusters(positions: list) -> int:
    import pandas as pd
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count, end = 0, None
    for s, e in spans:
        if end is None or s > end:
            count += 1
            end = e
        else:
            end = max(end, e)
    return count


def evidence_reasons(proto: dict, pop_res: dict) -> list:
    reasons = list(pop_res["inputs"]["reasons"])
    roster = pop_res["roster"]
    interval = proto["populations"][pop_res["population"]]["interval"]
    want = sorted(f"{c} {interval}" for c in roster)
    for cost in COSTS:
        mult = proto["cost_multipliers"][cost]
        by_arm = pop_res["legs"].get(cost) or {}
        for arm in arm_names(proto):
            legs = by_arm.get(arm)
            if legs is None:
                reasons.append(f"{cost} {arm}: no legs")
                continue
            got = sorted(legs)
            if got != want:
                reasons.append(f"{cost} {arm}: datasets {got} != frozen {want}")
            for ds in want:
                leg = legs.get(ds)
                if leg is None:
                    reasons.append(f"{cost} {arm} {ds}: leg missing")
                    continue
                if "error" in leg:
                    reasons.append(f"{cost} {arm} {ds}: {leg['error']}")
                    continue
                bad = [k for k in METRIC_KEYS if not finite(leg.get(k))]
                bad += [k for k in EXEC_METRIC_KEYS if not finite((leg.get("execution") or {}).get(k))]
                if bad:
                    reasons.append(f"{cost} {arm} {ds}: non-finite {bad}")
                man = leg.get("manifest") or {}
                if not (man.get("candle_coverage") or {}).get("complete"):
                    reasons.append(f"{cost} {arm} {ds}: candle coverage incomplete")
                if not (man.get("funding_coverage") or {}).get("complete"):
                    reasons.append(f"{cost} {arm} {ds}: funding coverage incomplete")
                if leg.get("funding_incomplete"):
                    reasons.append(f"{cost} {arm} {ds}: funding_incomplete")
                if man.get("cost_multiplier") != mult:
                    reasons.append(f"{cost} {arm} {ds}: cost multiplier {man.get('cost_multiplier')} != {mult}")
                if (man.get("provenance") or {}).get("kind") != "venue":
                    reasons.append(f"{cost} {arm} {ds}: provenance is not venue")
                inp = (pop_res["inputs"]["datasets"] or {}).get(ds) or {}
                if inp and man.get("candles_sha256") != inp.get("candles_sha256"):
                    reasons.append(f"{cost} {arm} {ds}: candle hash differs from the registered input")
                if inp and man.get("funding_sha256") != inp.get("funding_sha256"):
                    reasons.append(f"{cost} {arm} {ds}: funding hash differs from the registered input")
    extra_costs = sorted(set(pop_res["legs"]) - set(COSTS))
    if extra_costs:
        reasons.append(f"unexpected cost labels {extra_costs}")
    for cost in COSTS:
        extra_arms = sorted(set((pop_res["legs"].get(cost) or {})) - set(arm_names(proto)))
        if extra_arms:
            reasons.append(f"{cost}: unexpected arms {extra_arms}")
    audit = pop_res.get("entry_audit")
    if audit is None:
        reasons.append("candidate entry audit missing")
    elif not audit["ok"]:
        reasons.append(f"candidate entry audit: {len(audit['failures'])} failures")
    return reasons


def arm_summary(legs: dict) -> dict:
    present = {ds: l for ds, l in legs.items() if l and "error" not in l}
    if not present:
        return {"datasets": 0}
    per = {}
    positions = []
    for ds, l in sorted(present.items()):
        ex = l.get("execution") or {}
        plist = ex.get("position_list")
        per[ds] = {"sharpe": l["sharpe"], "return_pct": l["return_pct"], "max_dd_pct": l["max_dd_pct"],
                   "ddadj": l["ddadj"], "trades": l["trades"], "liquidated": l["liquidated"],
                   "net_pnl_usd": ex.get("net_pnl_usd"), "fees_usd": ex.get("fees_usd"),
                   "funding_pnl_usd": ex.get("funding_pnl_usd"), "turnover": ex.get("turnover"),
                   "positions": ex.get("positions"), "long_positions": ex.get("long_positions"),
                   "short_positions": ex.get("short_positions"), "rejected_entries": ex.get("rejected_entries"),
                   "clusters": position_clusters(plist) if plist is not None else None}
        for p in plist or []:
            positions.append(dict(p, dataset=ds))
    vals = list(per.values())

    def mean(k):
        xs = [v[k] for v in vals]
        return statistics.mean(xs) if all(finite(x) for x in xs) else None

    return {
        "datasets": len(per),
        "mean_sharpe": mean("sharpe"),
        "mean_return_pct": mean("return_pct"),
        "mean_ddadj": mean("ddadj"),
        "worst_max_dd_pct": min((v["max_dd_pct"] for v in vals if finite(v["max_dd_pct"])), default=None),
        "net_pnl_usd": sum(v["net_pnl_usd"] for v in vals) if all(finite(v["net_pnl_usd"]) for v in vals) else None,
        "fees_usd": sum(v["fees_usd"] or 0.0 for v in vals),
        "funding_pnl_usd": sum(v["funding_pnl_usd"] or 0.0 for v in vals),
        "positions": sum(v["positions"] or 0 for v in vals),
        "long_positions": sum(v["long_positions"] or 0 for v in vals),
        "short_positions": sum(v["short_positions"] or 0 for v in vals),
        "liquidated_legs": sum(1 for v in vals if v["liquidated"]),
        "pooled_clusters": position_clusters(positions) if all(v["clusters"] is not None for v in vals) else None,
        "per_coin_clusters": {ds: v["clusters"] for ds, v in per.items()},
        "summed_per_coin_clusters": (sum(v["clusters"] for v in vals)
                                     if all(v["clusters"] is not None for v in vals) else None),
        "per_dataset": per,
    }


def m1_score(proto: dict, cand_legs: dict, inc_legs: dict) -> dict:
    import eval_windows as ew
    bars = ew.incumbent_bars({ds: {n: inc_legs[n][ds] for n in proto["incumbents"]} for ds in cand_legs})
    score = ew.score_candidate(dict(cand_legs), bars)
    score.pop("rows", None)
    score["bars"] = bars
    return score


def reduce_population(proto: dict, pop_res: dict) -> dict:
    reasons = evidence_reasons(proto, pop_res)
    out = {"evidence_complete": not reasons, "evidence_reasons": reasons, "arms": {}, "m1": {}}
    for cost in COSTS:
        by_arm = pop_res["legs"].get(cost) or {}
        out["arms"][cost] = {arm: arm_summary(by_arm.get(arm) or {}) for arm in ["candidate"] + proto["comparators"]}
        if not reasons:
            out["m1"][cost] = m1_score(proto, by_arm["candidate"],
                                       {n: by_arm[n] for n in proto["incumbents"]})
        else:
            out["m1"][cost] = None
    return out


def performance_checks(red: dict) -> dict:
    base, stress = red["arms"]["base"], red["arms"]["stress"]
    cand = base["candidate"]
    return {
        "net_positive_base": cand["net_pnl_usd"] > 0,
        "net_positive_stress": stress["candidate"]["net_pnl_usd"] > 0,
        "m1_pass_base": red["m1"]["base"]["verdict"] == "pass",
        "beats_breakout_sharpe_base": cand["mean_sharpe"] > base["breakout"]["mean_sharpe"],
        "beats_session_breakout_sharpe_base": cand["mean_sharpe"] > base["session_breakout"]["mean_sharpe"],
    }


def primary_verdict(proto: dict, pop_res: dict) -> dict:
    red = reduce_population(proto, pop_res)
    floor = proto["sample_rules"]["min_primary_clusters"]
    clusters = red["arms"]["base"]["candidate"].get("pooled_clusters")
    checks, diag = None, None
    if not red["evidence_complete"]:
        outcome = "inconclusive"
        reason = "evidence incomplete: " + "; ".join(red["evidence_reasons"][:12]) + (
            f" (+{len(red['evidence_reasons']) - 12} more)" if len(red["evidence_reasons"]) > 12 else "")
    elif clusters is None or clusters < floor:
        outcome, reason = "inconclusive", f"pooled candidate base-cost clusters {clusters} < {floor}"
        diag = performance_checks(red)
    else:
        checks = performance_checks(red)
        false = sorted(k for k, v in checks.items() if not v)
        outcome, reason = ("fail", "false: " + ", ".join(false)) if false else ("pass", "every frozen check is true")
    return {"outcome": outcome, "reason": reason, "pooled_clusters": clusters, "floor": floor, "checks": checks,
            "checks_not_used_below_floor": diag, "reduced": red}


def run_population(paths: Paths, proto: dict, pop: str, screen: dict, cache_db: str, workers: int,
                   score_invalid: bool = True) -> dict:
    roster = screen["rosters"][pop]
    cfg = proto["populations"][pop]
    val = input_validation(proto, paths.manifest(proto, pop), pop, roster)
    manifest = val.pop("manifest")
    res = {"population": pop, "role": cfg["role"], "interval": cfg["interval"], "window": cfg["window"],
           "roster": roster, "inputs": val, "legs": {c: {} for c in COSTS}, "entry_audit": None,
           "scored": False}
    if manifest is None or (not val["ok"] and not score_invalid):
        return res
    units = [(pop, arm, cost, d["coin"]) for cost in COSTS for arm in arm_names(proto) for d in manifest["datasets"]]
    t0 = time.time()
    with concurrent.futures.ProcessPoolExecutor(max_workers=workers, initializer=worker_init,
                                                initargs=(paths.root, cache_db)) as pool:
        done = 0
        for unit, leg in pool.map(run_unit, units, chunksize=1):
            _, arm, cost, coin = unit
            key = f"{coin} {cfg['interval']}"
            slot = res["legs"][cost].setdefault(arm, {})
            if key in slot:
                slot[key] = {"error": "duplicate leg"}
            else:
                slot[key] = leg
            done += 1
            if done % 25 == 0 or done == len(units):
                print(f"[{pop}] {done}/{len(units)} legs ({time.time() - t0:.0f}s)", flush=True)
    res["scored"] = True
    res["entry_audit"] = entry_audit(proto, manifest, pop, res["legs"]["base"].get("candidate") or {})
    return res


def run(paths: Paths, cache_db: str, out_path: str, populations=None, workers: int = 4, git_check: bool = True,
        score_invalid: bool = True) -> dict:
    proto = protocol(paths)
    held = require_lock(paths, git_check=git_check)
    refs = check_references(proto)
    cache = fresh_cache(cache_db)
    block_network()
    screen = load_json(paths.screen)
    started = time.time()
    out = {
        "study": proto["study"], "issue": proto["issue"], "protocol_sha256": held["lock"]["protocol_sha256"],
        "lock": {k: held["lock"][k] for k in ("protocol_sha256", "inventory_sha256", "screen_sha256",
                                               "manifests_sha256", "locked_at_utc", "source_commit")},
        "lock_git": held.get("git"), "source_commit": git_head(), "references": refs,
        "research_cache": os.path.basename(cache), "network_blocked_while_scoring": True,
        "rosters": screen["rosters"], "populations": {},
    }
    for pop in populations or POPULATIONS:
        print(f"[{pop}] scoring", flush=True)
        out["populations"][pop] = run_population(paths, proto, pop, screen, cache, workers, score_invalid)
    if PRIMARY in out["populations"]:
        v = primary_verdict(proto, out["populations"][PRIMARY])
        out["primary_reduced"] = v.pop("reduced")
        out["verdict"] = v
        print(f"[verdict] {v['outcome']}: {v['reason']}", flush=True)
    out["descriptive_reduced"] = {pop: reduce_population(proto, out["populations"][pop])
                                  for pop in out["populations"] if pop != PRIMARY}
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(out_path, out)
    print(f"wrote {out_path}")
    return out


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#2019 dual_thrust confirmation study (suggest-only, sends no order)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("acquire", help="screen the roster and freeze public venue inputs (network)")
    sub.add_parser("lock", help="record the protocol, inventory, screen, inputs and scoring sources before any score")
    r = sub.add_parser("run", help="score the frozen populations offline from a fresh research cache")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--out", default=None)
    r.add_argument("--population", action="append", choices=list(POPULATIONS))
    r.add_argument("--workers", type=int, default=4)
    v = sub.add_parser("verify", help="integration checks of refusals and verdict precedence on disposable copies")
    v.add_argument("--work-dir", required=True)
    v.add_argument("--workers", type=int, default=4)
    v.add_argument("--skip-rerun", action="store_true")
    sub.add_parser("render", help="write REPORT.md and README.md from results.json and verification.json")
    args = p.parse_args(argv)
    paths = Paths()
    if args.cmd == "acquire":
        acquire(paths)
    elif args.cmd == "lock":
        lock(paths)
    elif args.cmd == "run":
        out = os.path.abspath(args.out) if args.out else paths.results
        if out == paths.results and os.path.exists(out):
            raise SystemExit("results.json exists; pass --out for a reproduction run")
        run(paths, args.cache_db, out, args.population, args.workers)
    elif args.cmd == "verify":
        import verify_study
        verify_study.main(paths, args.work_dir, args.workers, args.skip_rerun)
    elif args.cmd == "render":
        import report
        report.render(paths)
    return 0


if __name__ == "__main__":
    sys.exit(main())
