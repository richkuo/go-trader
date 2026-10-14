#!/usr/bin/env python3

import argparse
import copy
import csv
import datetime
import gzip
import hashlib
import importlib.util
import io
import json
import math
import os
import sqlite3
import statistics
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
BACKTEST = os.path.join(REPO, "backtest")
REPLAY_DRIVER = os.path.join(BACKTEST, "basket_replay.py")
HOUR_MS = 3_600_000
INTERVAL_MS = {"1h": 3_600_000, "4h": 14_400_000, "1d": 86_400_000}
RESULT_SCHEMA = "go-trader.xsmom-basket-study-result"
RESULT_VERSION = 1
DATA_SCHEMA = "go-trader.xsmom-basket-study-data"
LOCK_SCHEMA = "go-trader.xsmom-basket-study-lock"
CANDLE_HEADER = ["timestamp", "open", "high", "low", "close", "volume", "trades"]
BASELINE_ORDER = ("equal_weight_hold", "momentum", "vol_momentum")


class StudyRefusal(Exception):
    def __init__(self, stage, reason, detail):
        super().__init__(f"{stage}: {reason}: {detail}")
        self.stage = stage
        self.reason = reason
        self.detail = detail if isinstance(detail, list) else [detail]


class Study:
    def __init__(self, study_dir):
        self.dir = os.path.realpath(study_dir)
        self.protocol_path = os.path.join(self.dir, "protocol.json")
        self.lock_path = os.path.join(self.dir, "protocol.lock.json")
        self.data_dir = os.path.join(self.dir, "data")
        self.data_manifest_path = os.path.join(self.data_dir, "data_manifest.json")
        self.plans_dir = os.path.join(self.dir, "plans")
        self.raw_dir = os.path.join(self.dir, "raw")
        self.results_path = os.path.join(self.dir, "results.json")
        self.manifest_path = os.path.join(self.dir, "study_manifest.json")
        self.report_path = os.path.join(self.dir, "REPORT.md")


def sha256_bytes(blob):
    return hashlib.sha256(blob).hexdigest()


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def utc_ms(text):
    ts = datetime.datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
    return int(ts.timestamp() * 1000)


def iso(ms):
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def load_json(path):
    with open(path) as fh:
        return json.load(fh)


def dump_json(obj):
    return json.dumps(obj, indent=2, sort_keys=True) + "\n"


def write_new(path, obj):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    with os.fdopen(fd, "w") as fh:
        fh.write(dump_json(obj))


def write_gz_csv(path, header, rows):
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(header)
    w.writerows(rows)
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(buf.getvalue().encode())


def write_gz_json(path, obj):
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(json.dumps(obj, sort_keys=True).encode())


def read_gz_csv(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt", newline="") as fh:
        return list(csv.DictReader(fh))


def candle_name(coin, tf):
    return f"{coin}_{tf}_candles.csv.gz"


def funding_name(coin):
    return f"{coin}_funding.csv.gz"


def member_id(coin):
    return "xs-" + coin.lower()


def git(*args):
    return subprocess.check_output(["git", "-C", REPO, *args], text=True, stderr=subprocess.DEVNULL).strip()


def source_state():
    try:
        head = git("rev-parse", "HEAD")
        dirty = git("status", "--porcelain", "--untracked-files=no")
    except (OSError, subprocess.CalledProcessError):
        return {"commit": None, "dirty": None, "dirty_paths": []}
    paths = sorted(line[3:] for line in dirty.splitlines() if line.strip())
    return {"commit": head, "dirty": bool(paths), "dirty_paths": paths}


def tool_versions(binary):
    out = {"python": sys.version.split()[0]}
    for key, cmd in (("go", ["go", "version"]), ("uv", ["uv", "--version"])):
        try:
            out[key] = subprocess.check_output(cmd, text=True, stderr=subprocess.DEVNULL).strip()
        except (OSError, subprocess.CalledProcessError):
            out[key] = None
    if binary:
        try:
            out["go_trader"] = subprocess.check_output([binary, "version"], text=True, stderr=subprocess.DEVNULL,
                                                       timeout=60).strip()
        except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
            out["go_trader"] = None
    return out


def import_path(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def hl_adapter():
    hl_dir = os.path.join(REPO, "platforms", "hyperliquid")
    if hl_dir not in sys.path:
        sys.path.insert(0, hl_dir)
    import adapter as hl_module
    return hl_module, hl_module.HyperliquidExchangeAdapter()


def hl_meta(base_url):
    import urllib.request
    req = urllib.request.Request(base_url + "/info", data=json.dumps({"type": "meta"}).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.load(resp)


def apply_universe_rule(proto, listing):
    rule = proto["universe"]
    cutoff = utc_ms(rule["listing_cutoff"])
    end = utc_ms(proto["data"]["pinned_end"])
    ivl = INTERVAL_MS[proto["data"]["timeframe"]]
    members, excluded = [], {}
    for coin in rule["candidates"]:
        info = listing.get(coin)
        if info is None:
            excluded[coin] = "no frozen venue history"
            continue
        why = []
        if info.get("delisted"):
            why.append("the venue meta lists it as delisted")
        if info.get("first_traded_ms") is None:
            why.append("no daily bar with a trade")
        elif info["first_traded_ms"] > cutoff:
            why.append(f"first traded bar {iso(info['first_traded_ms'])} is after the listing cutoff")
        if info.get("last_open_ms") is None or info["last_open_ms"] < end - ivl:
            why.append("the frozen history ends before pinned_end")
        if info.get("gaps"):
            why.append(f"{info['gaps']} gap(s) or zero-trade bar(s) after the first traded bar")
        if why:
            excluded[coin] = "; ".join(why)
        else:
            members.append(coin)
    return members, excluded


def missing_funding_hours(cache_db, coin, start_ms, end_ms):
    conn = sqlite3.connect(cache_db)
    try:
        priced = {int(t) // HOUR_MS * HOUR_MS for (t,) in conn.execute(
            "SELECT timestamp FROM funding_rates WHERE exchange='hyperliquid' AND coin=? AND timestamp >= ?"
            " AND timestamp < ?", (coin, start_ms, end_ms))}
        absent = {int(t) for (t,) in conn.execute(
            "SELECT timestamp FROM funding_venue_gaps WHERE exchange='hyperliquid' AND coin=? AND timestamp >= ?"
            " AND timestamp < ?", (coin, start_ms, end_ms))}
    finally:
        conn.close()
    return [h for h in range(start_ms, end_ms, HOUR_MS) if h not in priced and h not in absent], sorted(absent)


def prepare_network(study, proto, cache_db, resume):
    if os.path.exists(cache_db) and not resume:
        raise SystemExit(f"refusing to reuse the existing cache {cache_db}; preparation starts empty "
                         "(--resume continues an interrupted preparation into the same cache)")
    os.environ["GO_TRADER_OHLCV_CACHE_DB"] = os.path.abspath(cache_db)
    tools = os.path.join(REPO, "shared_tools")
    if tools not in sys.path:
        sys.path.insert(0, tools)
    import pandas as pd
    from funding_fetcher import load_cached_funding
    import storage

    tf = proto["data"]["timeframe"]
    ivl = INTERVAL_MS[tf]
    end = utc_ms(proto["data"]["pinned_end"])
    fstart = utc_ms(proto["data"]["funding_start"])
    hl_module, adapter = hl_adapter()
    meta = hl_meta(hl_module.MAINNET_URL)
    delisted = {u.get("name"): bool(u.get("isDelisted")) for u in meta.get("universe", [])}
    os.makedirs(study.data_dir, exist_ok=resume)
    listing, candle_rows = {}, {}
    for coin in proto["universe"]["candidates"]:
        if coin not in delisted:
            listing[coin] = {"delisted": True, "first_traded_ms": None, "last_open_ms": None, "gaps": 0,
                             "note": "absent from the venue meta"}
            continue
        raw = adapter.get_ohlcv_candles(coin, tf, limit=5000)
        bars = sorted((int(c["t"]), c) for c in raw if int(c["t"]) < end)
        traded = [t for t, c in bars if int(c.get("n", 0)) > 0]
        first = traded[0] if traded else None
        kept = [(t, c) for t, c in bars if first is not None and t >= first]
        gaps = sum(1 for (a, _), (b, _) in zip(kept, kept[1:]) if b - a != ivl)
        gaps += sum(1 for _, c in kept if int(c.get("n", 0)) <= 0)
        listing[coin] = {"delisted": delisted[coin], "first_traded_ms": first,
                         "first_bar_ms": bars[0][0] if bars else None,
                         "last_open_ms": kept[-1][0] if kept else None, "gaps": gaps, "bars": len(kept)}
        candle_rows[coin] = [[t, repr(float(c["o"])), repr(float(c["h"])), repr(float(c["l"])), repr(float(c["c"])),
                              repr(float(c["v"])), int(c.get("n", 0))] for t, c in kept]
        time.sleep(0.25)
    members, excluded = apply_universe_rule(proto, listing)
    files = {}
    for coin in members:
        path = os.path.join(study.data_dir, candle_name(coin, tf))
        write_gz_csv(path, CANDLE_HEADER, candle_rows[coin])
        files[candle_name(coin, tf)] = {"kind": "candles", "coin": coin, "timeframe": tf,
                                        "rows": len(candle_rows[coin]), "first_ms": candle_rows[coin][0][0],
                                        "last_ms": candle_rows[coin][-1][0], "sha256": sha256_file(path)}
    absent_hours = {}
    for coin in members:
        cursor = fstart
        while cursor < end:
            stop = min(cursor + 30 * 24 * HOUR_MS, end)
            for attempt in range(8):
                box = {}
                load_cached_funding(coin, pd.Timestamp(cursor, unit="ms", tz="UTC"),
                                    end_date=pd.Timestamp(stop, unit="ms", tz="UTC"), db_path=cache_db,
                                    source_box=box)
                if not box.get("fetch_error"):
                    break
                print(f"{coin} funding {iso(cursor)}..{iso(stop)}: {box['fetch_error'][:160]}; retry {attempt + 1}",
                      flush=True)
                time.sleep(15 * (attempt + 1))
            else:
                raise SystemExit(f"{coin} funding {iso(cursor)}..{iso(stop)} could not be fetched")
            cursor = stop
        missing, absent = missing_funding_hours(cache_db, coin, fstart, end)
        conn = sqlite3.connect(cache_db)
        try:
            rates = conn.execute("SELECT timestamp, rate FROM funding_rates WHERE exchange='hyperliquid' AND coin=?"
                                 " AND timestamp >= ? AND timestamp < ? ORDER BY timestamp",
                                 (coin, fstart, end)).fetchall()
        finally:
            conn.close()
        path = os.path.join(study.data_dir, funding_name(coin))
        write_gz_csv(path, ["timestamp", "rate"], [[int(t), repr(float(r))] for t, r in rates])
        absent_hours[coin] = absent
        files[funding_name(coin)] = {"kind": "funding", "coin": coin, "rows": len(rates),
                                     "first_ms": int(rates[0][0]) if rates else None,
                                     "last_ms": int(rates[-1][0]) if rates else None,
                                     "missing_hours": len(missing), "venue_absent_hours": len(absent),
                                     "sha256": sha256_file(path)}
    lots = {}
    for coin in members:
        dec = adapter.lot_size_decimals(coin)
        if dec is None:
            raise SystemExit(f"{coin} lot size decimals are unavailable")
        lots[coin] = int(dec)
    write_data_manifest(study, proto, members, excluded, listing, files, absent_hours, lots,
                        {"candles": proto["data"]["candles"], "funding": proto["data"]["funding"],
                         "lot_metadata": proto["data"]["lot_metadata"], "network": True})


def prepare_recorded(study, proto, frames_path, lots_path):
    rec = load_json(frames_path)
    lots_in = load_json(lots_path)
    tf = proto["data"]["timeframe"]
    ivl = INTERVAL_MS[tf]
    end = utc_ms(proto["data"]["pinned_end"])
    fstart = utc_ms(proto["data"]["funding_start"])
    os.makedirs(study.data_dir)
    listing, files, absent_hours = {}, {}, {}
    rows_by = {}
    for coin in proto["universe"]["candidates"]:
        rows = sorted((r for r in rec.get("candles", {}).get(f"{coin}|{tf}", []) if int(r[0]) < end),
                      key=lambda r: r[0])
        if not rows:
            listing[coin] = {"delisted": False, "first_traded_ms": None, "last_open_ms": None, "gaps": 0}
            continue
        gaps = sum(1 for a, b in zip(rows, rows[1:]) if int(b[0]) - int(a[0]) != ivl)
        listing[coin] = {"delisted": False, "first_traded_ms": int(rows[0][0]), "first_bar_ms": int(rows[0][0]),
                         "last_open_ms": int(rows[-1][0]), "gaps": gaps, "bars": len(rows)}
        rows_by[coin] = [[int(r[0])] + [repr(float(x)) for x in r[1:6]] + [1] for r in rows]
    members, excluded = apply_universe_rule(proto, listing)
    for coin in members:
        path = os.path.join(study.data_dir, candle_name(coin, tf))
        write_gz_csv(path, CANDLE_HEADER, rows_by[coin])
        files[candle_name(coin, tf)] = {"kind": "candles", "coin": coin, "timeframe": tf, "rows": len(rows_by[coin]),
                                        "first_ms": rows_by[coin][0][0], "last_ms": rows_by[coin][-1][0],
                                        "sha256": sha256_file(path)}
        recs = sorted((int(r[0]), float(r[1])) for r in rec.get("funding", {}).get(coin, [])
                      if fstart <= int(r[0]) < end)
        path = os.path.join(study.data_dir, funding_name(coin))
        write_gz_csv(path, ["timestamp", "rate"], [[t, repr(r)] for t, r in recs])
        absent_hours[coin] = []
        files[funding_name(coin)] = {"kind": "funding", "coin": coin, "rows": len(recs),
                                     "first_ms": recs[0][0] if recs else None, "last_ms": recs[-1][0] if recs else None,
                                     "missing_hours": None, "venue_absent_hours": 0, "sha256": sha256_file(path)}
    lots = {coin: int(lots_in[coin]) for coin in members if coin in lots_in}
    write_data_manifest(study, proto, members, excluded, listing, files, absent_hours, lots,
                        {"candles": f"recorded frames {os.path.basename(frames_path)}",
                         "funding": f"recorded frames {os.path.basename(frames_path)}",
                         "lot_metadata": f"recorded lots {os.path.basename(lots_path)}", "network": False,
                         "frames_sha256": sha256_file(frames_path), "lots_sha256": sha256_file(lots_path)})


def write_data_manifest(study, proto, members, excluded, listing, files, absent_hours, lots, sources):
    lots_path = os.path.join(study.data_dir, "lot_metadata.json")
    with open(lots_path, "w") as fh:
        fh.write(dump_json(lots))
    manifest = {
        "schema": DATA_SCHEMA, "version": 1, "study": proto["study"],
        "prepared_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": source_state()["commit"], "sources": sources,
        "timeframe": proto["data"]["timeframe"], "pinned_end": proto["data"]["pinned_end"],
        "funding_start": proto["data"]["funding_start"],
        "universe": {"members": members, "excluded": excluded, "listing": listing},
        "files": files, "venue_absent_hours": absent_hours,
        "lot_metadata": {"path": "lot_metadata.json", "sha256": sha256_file(lots_path), "values": lots},
    }
    with open(study.data_manifest_path, "w") as fh:
        fh.write(dump_json(manifest))
    print(f"prepared {len(members)} members: {', '.join(members)}; excluded {sorted(excluded)}")


def cmd_lock(study):
    if os.path.exists(study.lock_path):
        raise SystemExit(f"{study.lock_path} exists; a lock is never replaced (a new experiment needs a new study "
                         "directory)")
    proto = load_json(study.protocol_path)
    spec_path = os.path.join(REPO, proto["signal_spec"]["path"])
    lock = {"schema": LOCK_SCHEMA, "version": 1, "locked_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "protocol_sha256": sha256_file(study.protocol_path), "signal_spec_sha256": sha256_file(spec_path),
            "data_manifest_sha256": sha256_file(study.data_manifest_path)}
    if lock["signal_spec_sha256"] != proto["signal_spec"]["sha256"]:
        raise SystemExit("the signal specification differs from the hash the protocol freezes")
    write_new(study.lock_path, lock)
    print(f"locked {study.lock_path}")


def check_lock(study):
    if not os.path.isfile(study.lock_path):
        raise StudyRefusal("lock", "lock_missing", "protocol.lock.json is missing; run lock before run")
    lock = load_json(study.lock_path)
    proto = load_json(study.protocol_path)
    spec_path = os.path.join(REPO, proto["signal_spec"]["path"])
    problems = []
    if not os.path.isfile(study.data_manifest_path):
        problems.append("data/data_manifest.json is missing")
    got = {"protocol_sha256": sha256_file(study.protocol_path),
           "signal_spec_sha256": sha256_file(spec_path) if os.path.isfile(spec_path) else None,
           "data_manifest_sha256": sha256_file(study.data_manifest_path) if not problems else None}
    for key, val in sorted(got.items()):
        if lock.get(key) != val:
            problems.append(f"{key} is {val}, the lock freezes {lock.get(key)}")
    if got["signal_spec_sha256"] != proto["signal_spec"]["sha256"]:
        problems.append("the frozen signal specification changed")
    if problems:
        raise StudyRefusal("lock", "lock_mismatch", problems)
    return proto, lock


def check_prerequisites(proto, binary):
    pre = proto["prerequisites"]
    problems, checked = [], {"commits": {}, "files": {}, "contracts": {}}
    for label, sha in sorted(pre.get("commits", {}).items()):
        try:
            subprocess.check_call(["git", "-C", REPO, "merge-base", "--is-ancestor", sha, "HEAD"],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            checked["commits"][label] = sha
        except (OSError, subprocess.CalledProcessError):
            problems.append(f"prerequisite {label} commit {sha} is not an ancestor of the study source")
    for rel in pre.get("files", []):
        path = os.path.join(REPO, rel)
        if not os.path.isfile(path):
            problems.append(f"prerequisite file {rel} is missing")
        else:
            checked["files"][rel] = sha256_file(path)
    if problems:
        raise StudyRefusal("prerequisites", "prerequisite_missing", problems)
    contracts = pre["contracts"]
    driver = import_path("xsmom_study_replay_driver", REPLAY_DRIVER)
    for key, have in (("replay_plan", [driver.PLAN_SCHEMA, driver.PLAN_VERSION]),
                      ("replay_result", [driver.RESULT_SCHEMA, driver.RESULT_VERSION]),
                      ("adapter_protocol", [driver.ADAPTER_PROTOCOL, driver.ADAPTER_VERSION])):
        if list(contracts[key]) != have:
            problems.append(f"{key} is {have}, the protocol checked {contracts[key]}")
        checked["contracts"][key] = have
    ranker = import_path("xsmom_study_ranker_runner", os.path.join(REPO, "shared_scripts", "run_basket_ranker.py"))
    if getattr(ranker, "REQUEST_VERSION", None) != contracts["ranker_request_version"]:
        problems.append(f"ranker request version is {getattr(ranker, 'REQUEST_VERSION', None)}")
    checked["contracts"]["ranker_request_version"] = getattr(ranker, "REQUEST_VERSION", None)
    reg = load_registry_module()
    entry = reg._registry.STRATEGIES.get(proto["candidate"]) or {}
    if proto["candidate"] not in reg._registry.basket_rankers():
        problems.append(f"{proto['candidate']} is not a registered basket ranker")
    if entry.get("edge_status") != "no_edge":
        problems.append(f"{proto['candidate']} edge_status is {entry.get('edge_status')!r}")
    checked["contracts"]["registry"] = {"edge_status": entry.get("edge_status"), "edge_source": entry.get("edge_source"),
                                        "edge_ref": entry.get("edge_ref"), "default_params": entry.get("default_params")}
    spec_src = open(os.path.join(REPO, "scheduler", "basket_replay_spec.go")).read()
    if 'basketReplayTargetsBaseline = "baseline"' not in spec_src:
        problems.append("the adapter source declares no baseline target input")
    cost_src = open(os.path.join(REPO, "scheduler", "fill_cost_policy.go")).read()
    if "fillCostRoleTakerAll" not in cost_src or "fillCostSpreadInSlippage" not in cost_src:
        problems.append("the cost policy source declares no taker_all and inside_slippage policy")
    probe = adapter_probe(binary)
    if probe.get("protocol") != driver.ADAPTER_PROTOCOL or probe.get("version") != driver.ADAPTER_VERSION:
        problems.append(f"the binary does not answer the replay protocol: {probe}")
    checked["contracts"]["adapter_probe"] = {"protocol": probe.get("protocol"), "version": probe.get("version"),
                                             "status": probe.get("status")}
    if problems:
        raise StudyRefusal("prerequisites", "contract_mismatch", problems)
    return checked


def adapter_probe(binary):
    try:
        proc = subprocess.run([binary, "basket-replay", "--spec", "-"], input=b"{}", stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL, cwd=REPO, timeout=120, check=False)
        return json.loads(proc.stdout.decode() or "{}")
    except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
        return {"error": str(exc)}


def load_registry_module():
    if BACKTEST not in sys.path:
        sys.path.insert(0, BACKTEST)
    from registry_loader import load_registry
    return load_registry("futures")


def source_hashes(proto):
    rels = list(proto["prerequisites"]["files"]) + [os.path.relpath(os.path.abspath(__file__), REPO),
                                                    proto["signal_spec"]["path"]]
    out = {}
    for rel in sorted(set(rels)):
        path = os.path.join(REPO, rel)
        out[rel] = sha256_file(path) if os.path.isfile(path) else None
    return out


def load_frozen_data(study, proto):
    dm = load_json(study.data_manifest_path)
    problems = []
    for name, info in sorted(dm["files"].items()):
        path = os.path.join(study.data_dir, name)
        if not os.path.isfile(path):
            problems.append(f"{name} is missing")
        elif sha256_file(path) != info["sha256"]:
            problems.append(f"{name} has sha256 {sha256_file(path)}, the data manifest freezes {info['sha256']}")
    lots_path = os.path.join(study.data_dir, dm["lot_metadata"]["path"])
    if not os.path.isfile(lots_path) or sha256_file(lots_path) != dm["lot_metadata"]["sha256"]:
        problems.append("lot_metadata.json is missing or changed")
    if problems:
        raise StudyRefusal("data", "data_hash_mismatch", problems)
    members = list(dm["universe"]["members"])
    again, _ = apply_universe_rule(proto, dm["universe"]["listing"])
    if again != members:
        raise StudyRefusal("data", "universe_mismatch", [f"the universe rule gives {again}, the manifest lists {members}"])
    tf = proto["data"]["timeframe"]
    candles, funding = {}, {}
    for coin in members:
        candles[coin] = [[int(r["timestamp"]), float(r["open"]), float(r["high"]), float(r["low"]), float(r["close"]),
                          float(r["volume"])] for r in read_gz_csv(os.path.join(study.data_dir, candle_name(coin, tf)))]
        funding[coin] = [(int(r["timestamp"]), float(r["rate"]))
                         for r in read_gz_csv(os.path.join(study.data_dir, funding_name(coin)))]
    lots = load_json(lots_path)
    return dm, members, candles, funding, lots


def validate_window(proto, dm, members, candles, funding, lots, window):
    tf = proto["data"]["timeframe"]
    ivl = INTERVAL_MS[tf]
    start, end = utc_ms(window[0]), utc_ms(window[1])
    need_from = start - int(proto["history_bars"]) * ivl
    problems, coverage = [], {}
    if len(members) < int(proto["basket"]["min_eligible_members"]):
        problems.append(f"{len(members)} members is below min_eligible_members")
    for coin in members:
        rows = candles[coin]
        by_open = {r[0]: r for r in rows}
        miss = [t for t in range(need_from, end, ivl) if t not in by_open]
        bad = 0
        for r in rows:
            if not all(math.isfinite(v) and v > 0 for v in r[1:5]) or r[2] < max(r[1], r[4]) or r[3] > min(r[1], r[4]):
                bad += 1
        if miss:
            problems.append(f"{coin}: {len(miss)} required {tf} bar(s) are missing in [{iso(need_from)}, {iso(end)}), "
                            f"first {iso(miss[0])}")
        if bad:
            problems.append(f"{coin}: {bad} bar(s) have non-finite, non-positive or unbounded prices")
        seen, dups, nonfinite = set(), 0, 0
        for t, r in funding[coin]:
            b = t // HOUR_MS * HOUR_MS
            if start <= b < end:
                if b in seen:
                    dups += 1
                seen.add(b)
                if not math.isfinite(r):
                    nonfinite += 1
        absent = {int(h) for h in dm.get("venue_absent_hours", {}).get(coin, [])}
        hours = list(range(start, end, HOUR_MS))
        missing = [h for h in hours if h not in seen]
        venue_absent = [h for h in missing if h in absent]
        if venue_absent:
            problems.append(f"{coin}: {len(venue_absent)} venue-absent funding hour(s) in the window, first "
                            f"{iso(venue_absent[0])}; a missing rate is never replaced with zero")
        if len(missing) > len(venue_absent):
            first = [h for h in missing if h not in absent][0]
            problems.append(f"{coin}: {len(missing) - len(venue_absent)} funding hour(s) have no record, first {iso(first)}")
        if dups:
            problems.append(f"{coin}: {dups} duplicate funding hour(s)")
        if nonfinite:
            problems.append(f"{coin}: {nonfinite} non-finite funding rate(s)")
        if coin not in lots:
            problems.append(f"{coin}: no frozen lot metadata")
        signs = [r for t, r in funding[coin] if start <= t < end]
        coverage[coin] = {"bars_required_from": iso(need_from), "bars_missing": len(miss), "funding_hours": len(hours),
                          "funding_missing": len(missing), "funding_venue_absent": len(venue_absent),
                          "funding_positive": sum(1 for r in signs if r > 0), "funding_negative": sum(1 for r in signs if r < 0),
                          "lot_sz_decimals": lots.get(coin)}
    return problems, coverage


def deadlines(proto, window):
    step = int(proto["basket"]["rebalance_interval_seconds"]) * 1000
    start, end = utc_ms(window[0]), utc_ms(window[1])
    first = -(-start // step) * step
    return list(range(first, end, step))


def closed_frame(rows, deadline_ms, ivl):
    import pandas as pd
    cut = [r for r in rows if r[0] + ivl <= deadline_ms]
    df = pd.DataFrame(cut, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df


def baseline_weights(proto, members, candles, window, name):
    spec = proto["baselines"][name]
    ivl = INTERVAL_MS[proto["data"]["timeframe"]]
    n = len(members)
    out, trace = {}, {}
    reg = load_registry_module() if spec["kind"] == "single_asset" else None
    strat = reg.get_strategy(spec["strategy"]) if reg else None
    params = dict(strat["default_params"]) if strat else {}
    for d in deadlines(proto, window):
        w = {}
        for coin in members:
            if spec["kind"] == "equal_weight":
                pos = 1
            else:
                df = closed_frame(candles[coin], d, ivl)
                res = strat["fn"](df, **params)
                if spec["strategy"] == "momentum":
                    pos = 0
                    for sig in res["signal"].tolist():
                        if sig == 1:
                            pos = 1
                        elif sig == -1 and pos == 1:
                            pos = 0
                elif spec["strategy"] == "vol_momentum":
                    pos = int(res["position"].iloc[-1]) if len(res) else 0
                else:
                    raise StudyRefusal("baseline", "baseline_unknown", f"{spec['strategy']} has no frozen conversion")
            w[coin] = pos / n
        out[iso(d)] = w
        trace[iso(d)] = {c: int(round(v * n)) for c, v in w.items()}
    return out, {"params": params, "positions": trace}


def check_weight_bounds(proto, weights, net_usd):
    gross = float(proto["basket"]["gross_budget_usd"])
    cap = float(proto["basket"]["max_coin_notional_usd"])
    problems = []
    for at, w in sorted(weights.items()):
        if sum(abs(v) for v in w.values()) > 1 + 1e-9:
            problems.append(f"{at}: absolute weights sum above 1")
        if any(abs(v) * gross > cap * (1 + 1e-9) for v in w.values()):
            problems.append(f"{at}: a weight is above the per-coin cap")
        if abs(sum(w.values())) * gross > net_usd * (1 + 1e-9):
            problems.append(f"{at}: the target net is above the declared net allowance")
    return problems


def run_specs(proto, selected=None):
    grid = proto["grid"]
    if selected is None:
        out = []
        for r in grid["return_lookback_bars"]:
            for q in grid["quantile"]:
                for v in grid["volatility_lookback_bars"]:
                    out.append({"run_id": f"train-r{r}-q{str(q).replace('.', 'p')}-v{v}", "window": "train",
                                "params": {"return_lookback_bars": r, "quantile": q, "volatility_lookback_bars": v},
                                "target": "ranker", "cost": "primary"})
        return out
    out = [{"run_id": "heldout-candidate", "window": "held_out", "params": selected, "target": "ranker",
            "cost": "primary"},
           {"run_id": "heldout-candidate-stress", "window": "held_out", "params": selected, "target": "ranker",
            "cost": "stress"}]
    for name in BASELINE_ORDER:
        if name in proto["baselines"]:
            out.append({"run_id": f"heldout-baseline-{name.replace('_', '-')}", "window": "held_out", "params": None,
                        "target": name, "cost": "primary"})
    return out


def portable_plan(study, proto, dm, lock, members, lots, run, weights):
    tf = proto["data"]["timeframe"]
    window = proto["windows"][run["window"]]
    b = proto["basket"]
    net = b["max_transition_net_usd"] if run["target"] == "ranker" else proto["baselines"][run["target"]]["net_allowance_usd"]
    listings = {}
    for coin in members:
        first = iso(dm["universe"]["listing"][coin]["first_traded_ms"])
        listings[coin] = {"listed_at": first, "known_at": first}
    basket = {"id": b["id"], "ranker": proto["candidate"], "params": dict(run["params"] or {}),
              "members": [member_id(c) for c in members], "rebalance_interval_seconds": b["rebalance_interval_seconds"],
              "evaluation_window_seconds": b["evaluation_window_seconds"],
              "ranker_timeout_seconds": b["ranker_timeout_seconds"], "lookback_bars": b["lookback_bars"],
              "gross_budget_usd": b["gross_budget_usd"], "max_transition_net_usd": net,
              "max_coin_notional_usd": b["max_coin_notional_usd"], "min_eligible_members": b["min_eligible_members"],
              "eligibility": {"source": b["eligibility_source"], "version": b["eligibility_version"],
                              "min_history_bars": b["min_history_bars"], "listings": listings}}
    if run["target"] != "ranker":
        basket["params"] = {}
    plan = {
        "plan": "go-trader.basket-replay-plan", "version": 1, "run_id": run["run_id"], "state_dir": "<work>/state/" + run["run_id"],
        "source": {"study": proto["study"], "protocol_sha256": lock["protocol_sha256"],
                   "data_manifest_sha256": lock["data_manifest_sha256"], "candles": dm["sources"]["candles"],
                   "funding": dm["sources"]["funding"], "lot_metadata": dm["sources"]["lot_metadata"]},
        "frames": {f"{c}|{tf}": {"path": "data/" + candle_name(c, tf), "sha256": dm["files"][candle_name(c, tf)]["sha256"]}
                   for c in members},
        "funding": {c: {"path": "data/" + funding_name(c), "sha256": dm["files"][funding_name(c)]["sha256"],
                        "from": window[0]} for c in members},
        "window": {"start": window[0], "end": window[1], "step_seconds": INTERVAL_MS[tf] // 1000},
        "history_bars": proto["history_bars"], "basket": basket,
        "members": [{"id": member_id(c), "coin": c, "timeframe": tf, "config": copy.deepcopy(proto["member_config"])}
                    for c in members],
        "target_input": {"mode": "ranker"} if run["target"] == "ranker" else {"mode": "baseline", "weights": weights},
        "cost_policy": dict(proto["costs"][run["cost"]]),
        "lot_metadata": {c: int(lots[c]) for c in members},
        "portfolio_risk": dict(proto["portfolio_risk"]),
    }
    return plan


def materialize(study, plan, work):
    out = copy.deepcopy(plan)
    out["state_dir"] = os.path.join(work, "state", plan["run_id"])
    for spec in list(out["frames"].values()) + list(out["funding"].values()):
        spec["path"] = os.path.join(study.dir, spec["path"])
    path = os.path.join(work, "plans", plan["run_id"] + ".json")
    with open(path, "w") as fh:
        fh.write(dump_json(out))
    return path


def run_replay(binary, plan_path, work, run_id, timeout):
    out = os.path.join(work, "results", run_id + ".json")
    log = os.path.join(work, "logs", run_id + ".stderr.log")
    t0 = time.time()
    proc = subprocess.run([sys.executable, REPLAY_DRIVER, "--plan", plan_path, "--binary", binary, "--out", out,
                           "--stderr", log, "--timeout", str(timeout)], cwd=REPO, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, text=True, check=False)
    rec = load_json(out) if os.path.isfile(out) else {"status": "process_error", "scored": False,
                                                      "promotion_eligible": False,
                                                      "refusal": {"reason": "driver_wrote_no_record",
                                                                  "detail": [proc.stderr[-2000:]]}}
    return {"driver_exit_code": proc.returncode, "record": rec, "seconds": round(time.time() - t0, 1),
            "result_path": out, "log_path": log, "result_sha256": sha256_file(out) if os.path.isfile(out) else None,
            "log_sha256": sha256_file(log) if os.path.isfile(log) else None}


def hac(xs, lag):
    n = len(xs)
    if n < 2:
        return None
    m = statistics.fmean(xs)
    d = [x - m for x in xs]
    g0 = math.fsum(v * v for v in d) / n
    if g0 <= 0:
        return None
    lrv = g0
    for k in range(1, min(lag, n - 1) + 1):
        gk = math.fsum(d[t] * d[t - k] for t in range(k, n)) / n
        lrv += 2 * (1 - k / (lag + 1)) * gk
    if lrv <= 0:
        return None
    return {"n": n, "mean": m, "sample_var": g0, "long_run_var": lrv, "se": math.sqrt(lrv / n),
            "n_eff": min(float(n), n * g0 / lrv)}


def summarize(proto, run, rep, members):
    rec = rep["record"]
    gross = float(proto["basket"]["gross_budget_usd"])
    out = {"run_id": run["run_id"], "window": proto["windows"][run["window"]], "target": run["target"],
           "params": run["params"], "cost": proto["costs"][run["cost"]], "replay_status": rec.get("status"),
           "replay_scored": bool(rec.get("scored")), "replay_promotion_eligible": bool(rec.get("promotion_eligible")),
           "adapter_exit_code": rec.get("adapter_exit_code"), "driver_exit_code": rep["driver_exit_code"],
           "seconds": rep["seconds"], "raw_result_sha256": rep.get("result_sha256"),
           "adapter_log_sha256": rep.get("log_sha256"), "complete": False}
    ao = rec.get("adapter_output") or {}
    if ao.get("coverage") is not None:
        out["coverage"] = {"complete": ao["coverage"].get("complete"), "gaps": (ao["coverage"].get("gaps") or [])[:50],
                           "gap_count": len(ao["coverage"].get("gaps") or [])}
    if rec.get("refusal"):
        out["refusal"] = {"reason": rec["refusal"].get("reason"), "detail": (rec["refusal"].get("detail") or [])[:50]}
    if ao.get("cost"):
        out["cost_identity"] = ao["cost"]
    if ao.get("run"):
        out["fingerprints"] = ao["run"].get("fingerprints")
    complete = (rec.get("status") == "complete" and rec.get("scored") is True and ao.get("status") == "complete"
                and (ao.get("coverage") or {}).get("complete") is True and rec.get("adapter_exit_code") == 0)
    if not complete:
        return out
    steps = ao.get("steps") or []
    step_ms = INTERVAL_MS[proto["data"]["timeframe"]]
    window = proto["windows"][run["window"]]
    expected = list(range(utc_ms(window[0]), utc_ms(window[1]), step_ms))
    got = [utc_ms(s["at"]) for s in steps]
    if got != expected:
        out["accounting_problem"] = "the adapter steps do not match the window steps"
        return out
    booked = sum(int(s.get("funding_rows_booked") or 0) for s in steps)
    exported = sum(1 for t in (ao.get("trades") or []) if t.get("trade_type") == "funding")
    if booked != exported:
        out["accounting_problem"] = (f"the adapter output carries {exported} funding rows while its steps booked "
                                     f"{booked}; the trade export is incomplete")
        return out
    out["complete"] = True
    first = steps[0]
    initial = math.fsum(m["initial_capital"] for m in first["members"])
    eq = [s["exposure"]["basket_equity_usd"] for s in steps]
    out["equity"] = [{"at": steps[i]["at"], "equity_usd": eq[i], "gross_usd": steps[i]["exposure"]["gross_usd"],
                      "net_usd": steps[i]["exposure"]["net_usd"]} for i in range(len(steps))]
    out["initial_equity_usd"] = initial
    out["final_equity_usd"] = eq[-1]
    out["total_return_on_gross"] = (eq[-1] - initial) / gross
    dl = set(deadlines(proto, window))
    idx = [i for i, t in enumerate(got) if t in dl]
    clusters, prev = [], initial
    prev_at = iso(got[0])
    for i in [j for j in idx if j > 0]:
        clusters.append({"from": prev_at, "to": steps[i]["at"], "return_on_gross": (eq[i] - prev) / gross})
        prev, prev_at = eq[i], steps[i]["at"]
    out["clusters"] = clusters
    out["cluster_tail_return_on_gross"] = (eq[-1] - prev) / gross
    peak, mdd = initial, 0.0
    for v in [initial] + eq:
        peak = max(peak, v)
        mdd = max(mdd, peak - v)
    out["max_drawdown_usd"] = mdd
    out["max_drawdown_on_gross"] = mdd / gross
    trades = ao.get("trades") or []
    fills = [t for t in trades if t.get("trade_type") != "funding"]
    fund = [t for t in trades if t.get("trade_type") == "funding"]
    fees = math.fsum(t.get("exchange_fee") or 0.0 for t in trades)
    realized = math.fsum(t.get("realized_pnl") or 0.0 for t in fills if t.get("is_close"))
    funding_usd = math.fsum(t.get("realized_pnl") or 0.0 for t in fund)
    traded = math.fsum(abs(t.get("value") or 0.0) for t in fills)
    weeks = max(1, len(clusters))
    final = steps[-1]["members"]
    plan_marks = rep.get("final_marks") or {}
    unrealized = 0.0
    for m in final:
        q, avg = m.get("signed_qty") or 0.0, m.get("avg_cost") or 0.0
        px = plan_marks.get(m["coin"])
        if q and px is not None:
            unrealized += q * (px - avg)
    out["accounting"] = {
        "fees_usd": fees, "funding_usd": funding_usd, "realized_close_pnl_pre_fee_usd": realized,
        "endpoint_unrealized_usd": unrealized,
        "equity_change_usd": eq[-1] - initial,
        "residual_usd": (eq[-1] - initial) - (realized + funding_usd - fees + unrealized),
        "note": "headline returns come from adapter basket equity, which already carries fees and funding; this "
                "split is a reconciliation and is never subtracted again",
        "fill_rows": len(fills), "funding_rows": len(fund),
        "funding_rows_positive": sum(1 for t in fund if (t.get("realized_pnl") or 0) > 0),
        "funding_rows_negative": sum(1 for t in fund if (t.get("realized_pnl") or 0) < 0),
        "cost_policy_ids": sorted({t.get("cost_policy_id") or "" for t in fills}),
        "cost_model_versions": sorted({t.get("cost_model_version") for t in fills if t.get("cost_model_version") is not None}),
    }
    out["turnover_on_gross"] = traded / gross
    out["turnover_on_gross_per_cluster"] = traded / gross / weeks
    start_vals = {m["coin"]: m["initial_capital"] for m in first["members"]}
    per_coin = {m["coin"]: (m.get("value") or 0.0) - start_vals.get(m["coin"], 0.0) for m in final}
    pos = [v for v in per_coin.values() if v > 0]
    out["member_pnl_usd"] = dict(sorted(per_coin.items()))
    out["largest_positive_member_share"] = (max(pos) / math.fsum(pos)) if pos else None
    rb, positions, exceptions, holds = {}, set(), 0, {}
    for s in steps:
        for r in s.get("rebalances") or []:
            rb[r["deadline_ms"]] = r
    for r in rb.values():
        if r.get("status") == "held":
            holds[r.get("hold_reason") or ""] = holds.get(r.get("hold_reason") or "", 0) + 1
        exceptions += len(r.get("exceptions") or [])
    for t in fills:
        if not t.get("is_close"):
            positions.add((t["strategy_id"], t.get("position_id")))
    statuses = {}
    for r in rb.values():
        statuses[r.get("status")] = statuses.get(r.get("status"), 0) + 1
    out["rebalances"] = {"by_status": dict(sorted(statuses.items())), "holds": dict(sorted(holds.items())),
                         "protective_exceptions": exceptions}
    out["positions_opened"] = len(positions)
    kinds = {}
    for t in fills:
        key = f"{t.get('trade_type') or ''}/{'close' if t.get('is_close') else 'open'}"
        kinds[key] = kinds.get(key, 0) + 1
    out["fill_kinds"] = dict(sorted(kinds.items()))
    gross_avg = statistics.fmean(s["exposure"]["gross_usd"] for s in steps)
    out["mean_gross_usd"] = gross_avg
    out["mean_abs_net_usd"] = statistics.fmean(abs(s["exposure"]["net_usd"]) for s in steps)
    return out


def test_series(proto, xs):
    st = proto["statistics"]
    h = hac(xs, int(st["lag_weeks"]))
    from statistics import NormalDist
    z = NormalDist().inv_cdf(1 - float(st["alpha_one_sided"]) / int(st["family_size"]))
    if h is None:
        return {"n": len(xs), "mean": statistics.fmean(xs) if xs else None, "defined": False, "z": z,
                "sufficient": False}
    lower = h["mean"] - z * h["se"]
    suff = h["n"] >= int(st["min_clusters"]) and h["n_eff"] >= float(st["min_effective_clusters"])
    if lower > 0:
        cond = True
    elif h["mean"] <= 0:
        cond = False
    else:
        cond = None
    return dict(h, defined=True, z=z, lower_bound=lower, condition=cond, sufficient=suff)


def verdict(proto, held):
    cand = held.get("heldout-candidate")
    stress = held.get("heldout-candidate-stress")
    base = {name: held.get(f"heldout-baseline-{name.replace('_', '-')}") for name in BASELINE_ORDER}
    runs = [cand, stress] + list(base.values())
    if not all(r and r.get("complete") for r in runs):
        bad = sorted(r["run_id"] for r in runs if r and not r.get("complete"))
        return {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                "promotion_eligible": False, "reason": "accounting_incomplete",
                "detail": [f"{rid} replay is not complete" for rid in bad] or ["a held-out replay is missing"],
                "edge_source": "study_inconclusive"}
    cx = [c["return_on_gross"] for c in cand["clusters"]]
    tests = {"net_positive": test_series(proto, cx)}
    for name in BASELINE_ORDER:
        bx = [c["return_on_gross"] for c in base[name]["clusters"]]
        if [c["to"] for c in base[name]["clusters"]] != [c["to"] for c in cand["clusters"]]:
            return {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                    "promotion_eligible": False, "reason": "clusters_misaligned", "detail": [name],
                    "edge_source": "study_inconclusive"}
        tests[f"beats_{name}"] = test_series(proto, [a - b for a, b in zip(cx, bx)])
    sufficient = all(t["sufficient"] and t["defined"] for t in tests.values())
    conds = {k: t.get("condition") for k, t in tests.items()}
    conds["stress_net_positive"] = stress["total_return_on_gross"] > 0
    share = cand.get("largest_positive_member_share")
    conds["not_concentrated"] = share is not None and share <= 0.5
    out = {"tests": tests, "conditions": conds, "sufficient": sufficient}
    if not sufficient:
        why = [f"{k}: n={t['n']} n_eff={round(t.get('n_eff', 0) or 0, 2)} defined={t['defined']}"
               for k, t in sorted(tests.items()) if not (t["sufficient"] and t["defined"])]
        out.update(verdict="inconclusive", scored=False, valid_economic_result=False, promotion_eligible=False,
                   reason="insufficient_independent_evidence", detail=why, edge_source="study_inconclusive")
        return out
    hard = ["net_positive"] + [f"beats_{n}" for n in BASELINE_ORDER] + ["stress_net_positive"]
    if all(v is True for v in conds.values()):
        out.update(verdict="pass", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="every condition true", detail=[], edge_source="unvalidated")
    elif any(conds[k] is False for k in hard):
        out.update(verdict="fail", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="a hard condition is false", detail=sorted(k for k in hard if conds[k] is False),
                   edge_source="study_fail")
    else:
        out.update(verdict="inconclusive", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="no hard condition false and at least one condition unknown or concentrated",
                   detail=sorted(k for k, v in conds.items() if v is not True), edge_source="study_inconclusive")
    return out


def select(proto, train):
    eligible = [r for r in train if r.get("complete") and r.get("clusters")]
    if not eligible:
        return None, []
    for r in eligible:
        r["train_objective"] = statistics.fmean(c["return_on_gross"] for c in r["clusters"])
    ranked = sorted(eligible, key=lambda r: (-r["train_objective"], r["params"]["return_lookback_bars"],
                                             r["params"]["quantile"]))
    return ranked[0]["params"], [r["run_id"] for r in ranked]


def refusal_record(proto_name, exc, extra=None):
    rec = {"schema": RESULT_SCHEMA, "version": RESULT_VERSION, "study": proto_name, "verdict": "refused",
           "scored": False, "valid_economic_result": False, "promotion_eligible": False,
           "refusal": {"stage": exc.stage, "reason": exc.reason, "detail": exc.detail},
           "edge_source": None, "live_use": "refused"}
    rec.update(extra or {})
    return rec


def compact(summary):
    out = {k: v for k, v in summary.items() if k != "equity"}
    if summary.get("equity"):
        out["equity_daily"] = [[e["at"], round(e["equity_usd"], 6), round(e["gross_usd"], 4), round(e["net_usd"], 4)]
                               for e in summary["equity"]]
    return out


def execute(binary, study, work, jobs, timeout, plans, runs, final_marks):
    def go(run):
        rep = run_replay(binary, plans[run["run_id"]], work, run["run_id"], timeout)
        rep["final_marks"] = final_marks[run["window"]]
        return run, rep
    with ThreadPoolExecutor(max_workers=max(1, jobs)) as pool:
        return list(pool.map(go, runs))


def cmd_run(study, binary, work, jobs, timeout, out_path, keep_raw):
    out_path = out_path or study.results_path
    if os.path.exists(out_path):
        print(f"{out_path} exists; a study result is never replaced", file=sys.stderr)
        return 2
    binary = os.path.abspath(binary)
    proto_name = None
    try:
        proto_name = load_json(study.protocol_path).get("study")
        proto, lock = check_lock(study)
        src = source_state()
        if proto.get("require_clean_source") and (src["dirty"] or src["commit"] is None):
            raise StudyRefusal("source", "source_not_clean", src["dirty_paths"] or ["no git commit"])
        checked = check_prerequisites(proto, binary)
        dm, members, candles, funding, lots = load_frozen_data(study, proto)
        coverage, problems = {}, []
        for wname in ("train", "held_out"):
            p, cov = validate_window(proto, dm, members, candles, funding, lots, proto["windows"][wname])
            problems += [f"{wname}: {x}" for x in p]
            coverage[wname] = cov
        if problems:
            raise StudyRefusal("data", "coverage_incomplete", problems)
        work = os.path.realpath(work)
        if os.path.exists(work) and os.listdir(work):
            raise StudyRefusal("work", "work_dir_not_empty", f"{work} must be new or empty")
        for sub in ("plans", "results", "logs", "state"):
            os.makedirs(os.path.join(work, sub), exist_ok=True)
    except StudyRefusal as exc:
        write_new(out_path, refusal_record(proto_name, exc))
        print(f"study refused: {exc}", file=sys.stderr)
        return 3
    tf_ms = INTERVAL_MS[proto["data"]["timeframe"]]
    final_marks = {}
    for wname, win in proto["windows"].items():
        if wname not in ("train", "held_out"):
            continue
        last = utc_ms(win[1]) - tf_ms
        final_marks[wname] = {c: next((r[1] for r in candles[c] if r[0] == last), None) for c in members}
    os.makedirs(study.plans_dir, exist_ok=True)
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    attempts, portable = [], {}

    def prepare_runs(runs, weights_by):
        paths = {}
        for run in runs:
            plan = portable_plan(study, proto, dm, lock, members, lots, run, weights_by.get(run["target"]))
            portable[run["run_id"]] = plan
            ppath = os.path.join(study.plans_dir, run["run_id"] + ".json")
            if not os.path.exists(ppath):
                with open(ppath, "w") as fh:
                    fh.write(dump_json(plan))
            paths[run["run_id"]] = materialize(study, plan, work)
        return paths

    train_runs = run_specs(proto)
    train_out = []
    for run, rep in execute(binary, study, work, jobs, timeout, prepare_runs(train_runs, {}), train_runs, final_marks):
        s = summarize(proto, run, rep, members)
        s["plan_sha256"] = sha256_file(os.path.join(work, "plans", run["run_id"] + ".json"))
        s["portable_plan_sha256"] = sha256_bytes(dump_json(portable[run["run_id"]]).encode())
        train_out.append(s)
        attempts.append({"run_id": run["run_id"], "status": s["replay_status"], "complete": s["complete"]})
        keep(study, keep_raw, run["run_id"], rep)
    selected, ranking = select(proto, train_out)
    held_out, weights_meta, verdict_rec = [], {}, None
    if selected is None:
        verdict_rec = {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                       "promotion_eligible": False, "reason": "no_complete_training_variant",
                       "detail": sorted(r["run_id"] for r in train_out), "edge_source": "study_inconclusive"}
    else:
        weights_by = {}
        for name in BASELINE_ORDER:
            if name not in proto["baselines"]:
                continue
            w, meta = baseline_weights(proto, members, candles, proto["windows"]["held_out"], name)
            bounds = check_weight_bounds(proto, w, float(proto["baselines"][name]["net_allowance_usd"]))
            if bounds:
                raise SystemExit(f"baseline {name} weights break their declared bounds: {bounds[:5]}")
            weights_by[name] = w
            weights_meta[name] = dict(meta, policy=proto["baselines"][name], bound_problems=bounds,
                                      net_allowance_usd=proto["baselines"][name]["net_allowance_usd"])
        held_runs = run_specs(proto, selected)
        for run, rep in execute(binary, study, work, jobs, timeout, prepare_runs(held_runs, weights_by), held_runs,
                                final_marks):
            s = summarize(proto, run, rep, members)
            s["plan_sha256"] = sha256_file(os.path.join(work, "plans", run["run_id"] + ".json"))
            s["portable_plan_sha256"] = sha256_bytes(dump_json(portable[run["run_id"]]).encode())
            held_out.append(s)
            attempts.append({"run_id": run["run_id"], "status": s["replay_status"], "complete": s["complete"]})
            keep(study, keep_raw, run["run_id"], rep)
        verdict_rec = verdict(proto, {s["run_id"]: s for s in held_out})
    result = {
        "schema": RESULT_SCHEMA, "version": RESULT_VERSION, "study": proto["study"], "candidate": proto["candidate"],
        "started_at_utc": started, "finished_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "lock": lock, "source": src, "prerequisites": checked, "source_hashes": source_hashes(proto),
        "binary": {"sha256": sha256_file(binary)}, "tools": tool_versions(binary),
        "universe": {"members": members, "excluded": dm["universe"]["excluded"]}, "coverage": coverage,
        "selected_params": selected, "training_ranking": ranking,
        "training": [compact(s) for s in train_out], "held_out": [compact(s) for s in held_out],
        "baseline_targets": weights_meta, "attempts": attempts,
        "study_verdict": verdict_rec, "live_use": "refused",
        "invocation": {"argv": sys.argv, "jobs": jobs, "timeout": timeout},
    }
    write_new(out_path, result)
    manifest = {"study": proto["study"], "results": os.path.basename(out_path),
                "results_sha256": sha256_file(out_path), "protocol_sha256": lock["protocol_sha256"],
                "data_manifest_sha256": lock["data_manifest_sha256"], "signal_spec_sha256": lock["signal_spec_sha256"],
                "source": src, "source_hashes": result["source_hashes"], "binary_sha256": result["binary"]["sha256"],
                "tools": result["tools"],
                "plans": {rid: sha256_file(os.path.join(study.plans_dir, rid + ".json")) for rid in sorted(portable)},
                "raw": sorted(os.listdir(study.raw_dir)) if keep_raw and os.path.isdir(study.raw_dir) else []}
    if out_path == study.results_path:
        with open(study.manifest_path, "w") as fh:
            fh.write(dump_json(manifest))
    v = verdict_rec
    print(f"study verdict={v['verdict']} scored={v['scored']} reason={v['reason']} out={out_path}")
    return 0


def keep(study, keep_raw, run_id, rep):
    if not keep_raw:
        return
    os.makedirs(study.raw_dir, exist_ok=True)
    write_gz_json(os.path.join(study.raw_dir, run_id + ".json.gz"), rep["record"])


def pct(x, digits=2):
    return "n/a" if x is None else f"{x * 100:.{digits}f}%"


def num(x, digits=4):
    return "n/a" if x is None else f"{x:.{digits}f}"


def cmd_render(study):
    res = load_json(study.results_path)
    proto = load_json(study.protocol_path)
    dm = load_json(study.data_manifest_path)
    v = res["study_verdict"]
    L = []
    L.append(f"# Cross-sectional momentum basket study (issue {proto['issue']})")
    L.append("")
    L.append(f"Verdict: **{v['verdict']}** (study scored={str(v['scored']).lower()}, valid economic result="
             f"{str(v['valid_economic_result']).lower()}). Reason: {v['reason']}.")
    L.append(f"Edge metadata: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`. Live basket use stays refused.")
    L.append("")
    L.append("This report is generated by `run_study.py render` from `results.json`, `protocol.json` and "
             "`data/data_manifest.json`. It endorses no trading setting.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append("uv run --no-sync python backtest/candidates/cross_sectional_momentum_1787/run_study.py prepare --cache-db <new.db>")
    L.append("uv run --no-sync python backtest/candidates/cross_sectional_momentum_1787/run_study.py lock")
    L.append("go -C scheduler build -o <bin>/go-trader .")
    L.append("uv run --no-sync python backtest/candidates/cross_sectional_momentum_1787/run_study.py run --binary <bin>/go-trader --work-dir <new dir> --jobs N")
    L.append("uv run --no-sync python backtest/candidates/cross_sectional_momentum_1787/run_study.py render")
    L.append("```")
    L.append("")
    L.append(f"- Study run argv: `{' '.join(res['invocation']['argv'][1:])}` (jobs {res['invocation']['jobs']}).")
    L.append(f"- Source commit `{res['source']['commit']}` (dirty={str(res['source']['dirty']).lower()}); binary sha256 "
             f"`{res['binary']['sha256']}`; tools: {res['tools'].get('go')}; {res['tools'].get('go_trader')}; "
             f"python {res['tools'].get('python')}.")
    L.append(f"- Protocol sha256 `{res['lock']['protocol_sha256']}`, signal specification sha256 "
             f"`{res['lock']['signal_spec_sha256']}` (unchanged), data manifest sha256 `{res['lock']['data_manifest_sha256']}`.")
    L.append(f"- Prerequisites checked: {', '.join(f'{k} = `{s[:12]}`' for k, s in sorted(res['prerequisites']['commits'].items()))}.")
    c = res["prerequisites"]["contracts"]
    L.append(f"- Contracts checked: plan {c['replay_plan']}, result {c['replay_result']}, adapter {c['adapter_protocol']} "
             f"(binary probe answered {c['adapter_probe']['protocol']} v{c['adapter_probe']['version']}), ranker request "
             f"v{c['ranker_request_version']}, registry {c['registry']['edge_status']}/{c['registry']['edge_source']}, "
             "baseline target input and the taker_all / inside_slippage cost path present.")
    L.append("- Every replay ran through `backtest/basket_replay.py` and the one `go-trader basket-replay` adapter: the "
             "real paper basket coordinator, member management, ranker runner, risk, funding and booking owners on a "
             "simulated clock, each in its own new temporary state directory.")
    L.append("")
    L.append("## Data and universe")
    L.append("")
    L.append(f"- Candles: {dm['sources']['candles']}.")
    L.append(f"- Funding: {dm['sources']['funding']}.")
    L.append(f"- Lot metadata: {dm['sources']['lot_metadata']} (prepared {dm['prepared_at_utc']}).")
    L.append(f"- Members ({len(res['universe']['members'])}): {', '.join(res['universe']['members'])}.")
    for coin, why in sorted(res["universe"]["excluded"].items()):
        L.append(f"- Excluded {coin}: {why}.")
    L.append(f"- {proto['universe']['survivorship']}.")
    L.append("")
    L.append("| member | listed (first traded bar) | bars | held-out funding hours (+/-) | szDecimals |")
    L.append("|---|---|---|---|---|")
    for coin in res["universe"]["members"]:
        lst = dm["universe"]["listing"][coin]
        cov = res["coverage"]["held_out"][coin]
        L.append(f"| {coin} | {iso(lst['first_traded_ms'])[:10]} | {lst.get('bars')} | {cov['funding_hours']} "
                 f"({cov['funding_positive']}/{cov['funding_negative']}) | {cov['lot_sz_decimals']} |")
    L.append("")
    L.append("## Frozen setup")
    L.append("")
    b = proto["basket"]
    L.append(f"- Windows: training {proto['windows']['train'][0]} to {proto['windows']['train'][1]}, held-out "
             f"{proto['windows']['held_out'][0]} to {proto['windows']['held_out'][1]}; daily steps; weekly rebalance "
             f"deadlines (Thursday 00:00 UTC); {proto['history_bars']} warm-up bars.")
    L.append(f"- Basket: gross budget ${b['gross_budget_usd']}, per-coin cap ${b['max_coin_notional_usd']}, candidate net "
             f"bound ${b['max_transition_net_usd']}, lookback {b['lookback_bars']} bars, minimum history "
             f"{b['min_history_bars']} bars; members ${proto['member_config']['capital']} paper capital each at leverage "
             f"{proto['member_config']['leverage']} (per-member cash, no pooled cash or margin).")
    L.append(f"- Member close policy: {proto['member_close_policy']}.")
    L.append(f"- Portfolio risk: {proto['portfolio_risk']}.")
    L.append(f"- Costs: primary {proto['costs']['primary']}; stress {proto['costs']['stress']}.")
    L.append(f"- Grid: {proto['grid']} (all {len(res['training'])} variants listed below).")
    L.append(f"- Statistics: {proto['statistics']['cluster']}; {proto['statistics']['inference']} (lag "
             f"{proto['statistics']['lag_weeks']} weeks); Bonferroni family {proto['statistics']['family_size']} at "
             f"one-sided alpha {proto['statistics']['alpha_one_sided']}; sufficiency {proto['statistics']['min_clusters']} "
             f"clusters and n_eff >= {proto['statistics']['min_effective_clusters']}.")
    L.append("")
    L.append("## Training variants (every variant tried)")
    L.append("")
    L.append("| run | R | q | V | replay | weeks | mean weekly (gross) | total (gross) | max DD (gross) | turnover/week | seconds |")
    L.append("|---|---|---|---|---|---|---|---|---|---|---|")
    for s in res["training"]:
        p = s["params"]
        weeks = len(s.get("clusters") or [])
        mean = statistics.fmean(c["return_on_gross"] for c in s["clusters"]) if weeks else None
        L.append(f"| {s['run_id']} | {p['return_lookback_bars']} | {p['quantile']} | {p['volatility_lookback_bars']} | "
                 f"{s['replay_status']} | {weeks} | {pct(mean, 3)} | {pct(s.get('total_return_on_gross'))} | "
                 f"{pct(s.get('max_drawdown_on_gross'))} | {pct(s.get('turnover_on_gross_per_cluster'))} | {s['seconds']} |")
    L.append("")
    L.append(f"Selected on training only: `{res['selected_params']}`; ranking {res['training_ranking']}.")
    L.append("")
    L.append("## Held-out results")
    L.append("")
    L.append("| arm | replay | total (gross) | max DD (gross) | turnover/week | mean gross $ | mean abs net $ | fees $ | funding $ | positions | largest member share |")
    L.append("|---|---|---|---|---|---|---|---|---|---|---|")
    for s in res["held_out"]:
        a = s.get("accounting") or {}
        L.append(f"| {s['run_id']} | {s['replay_status']} | {pct(s.get('total_return_on_gross'))} | "
                 f"{pct(s.get('max_drawdown_on_gross'))} | {pct(s.get('turnover_on_gross_per_cluster'))} | "
                 f"{num(s.get('mean_gross_usd'), 0)} | {num(s.get('mean_abs_net_usd'), 0)} | {num(a.get('fees_usd'), 2)} | "
                 f"{num(a.get('funding_usd'), 2)} | {s.get('positions_opened', 'n/a')} | "
                 f"{pct(s.get('largest_positive_member_share'), 1)} |")
    L.append("")
    if v.get("tests"):
        L.append("| test | n | n_eff | mean weekly | HAC se | corrected lower bound | condition |")
        L.append("|---|---|---|---|---|---|---|")
        for k, t in sorted(v["tests"].items()):
            L.append(f"| {k} | {t['n']} | {num(t.get('n_eff'), 1)} | {pct(t.get('mean'), 3)} | {pct(t.get('se'), 3)} | "
                     f"{pct(t.get('lower_bound'), 3)} | {t.get('condition')} |")
        L.append("")
        L.append(f"Conditions: {v.get('conditions')}. Sufficient: {v.get('sufficient')}.")
        L.append("")
    if v.get("detail"):
        L.append(f"Verdict detail: {v['detail']}.")
        L.append("")
    L.append("## Accounting reconciliation")
    L.append("")
    L.append("| arm | equity change $ | realized close (pre-fee) $ | funding $ | fees $ | endpoint unrealized $ | residual $ | rebalances | holds | protective exceptions |")
    L.append("|---|---|---|---|---|---|---|---|---|---|")
    for s in res["training"] + res["held_out"]:
        a = s.get("accounting") or {}
        r = s.get("rebalances") or {}
        L.append(f"| {s['run_id']} | {num(a.get('equity_change_usd'), 2)} | {num(a.get('realized_close_pnl_pre_fee_usd'), 2)} | "
                 f"{num(a.get('funding_usd'), 2)} | {num(a.get('fees_usd'), 2)} | {num(a.get('endpoint_unrealized_usd'), 2)} | "
                 f"{num(a.get('residual_usd'), 4)} | {r.get('by_status')} | {r.get('holds')} | {r.get('protective_exceptions')} |")
    L.append("")
    L.append("## Baselines")
    L.append("")
    for name in BASELINE_ORDER:
        meta = res["baseline_targets"].get(name)
        if not meta:
            continue
        L.append(f"- `{name}`: {meta['policy']['rule']} Net allowance ${meta['net_allowance_usd']} (candidate "
                 f"${proto['basket']['max_transition_net_usd']}). Parameters {meta['params']}.")
    L.append(f"- {proto['baselines']['common']}.")
    L.append("")
    L.append("## Limits")
    L.append("")
    L.append("- Execution surfaces not modeled: maker fills, a separate spread, order-book depth and market impact, "
             "intrabar execution, venue outages and partial venue fills (every scripted fill is a full taker fill at "
             "the step mark plus slippage), and historical changes of szDecimals or minimum order size (lot metadata "
             "is read once at preparation).")
    L.append("- Marks are the open of the daily Hyperliquid bar at each step; the venue's mark and oracle prices, which "
             "drive funding and liquidation, can differ from candle prices. Funding is charged from the venue's hourly "
             "rates at the sampled step mark.")
    L.append("- Members hold separate paper cash; idle cash earns nothing. Returns are stated on the gross budget, "
             "and the summed member capital is larger than the gross budget.")
    L.append("- The candidate's target net weight is exactly 0, but a dollar-neutral basket is not beta neutral, and "
             "transitions pass through states bounded by the candidate's own net allowance; baselines declare a "
             "larger net allowance for their long or directional holdings.")
    L.append("- The universe is a fixed set of surviving large coins chosen by a frozen rule; the result does not "
             "cover delisted coins, new listings or a larger universe.")
    L.append("- A study pass would grant no live admission; the basket live refusal stays in force for every outcome.")
    L.append("")
    with open(study.report_path, "w") as fh:
        fh.write("\n".join(L))
    print(f"wrote {study.report_path}")


def main(argv=None):
    ap = argparse.ArgumentParser(description="Frozen cross-sectional momentum basket study (issue 1792). "
                                             "Research-only and suggest-only: it never edits registry metadata, live "
                                             "defaults or deployed configuration.")
    ap.add_argument("--study-dir", default=HERE)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("prepare")
    p.add_argument("--cache-db")
    p.add_argument("--resume", action="store_true")
    p.add_argument("--recorded-frames")
    p.add_argument("--recorded-lots")
    sub.add_parser("lock")
    r = sub.add_parser("run")
    r.add_argument("--binary", required=True)
    r.add_argument("--work-dir", required=True)
    r.add_argument("--jobs", type=int, default=1)
    r.add_argument("--timeout", type=int, default=21600)
    r.add_argument("--out")
    r.add_argument("--no-raw", action="store_true")
    sub.add_parser("render")
    args = ap.parse_args(argv)
    study = Study(args.study_dir)
    if args.cmd == "prepare":
        proto = load_json(study.protocol_path)
        if args.recorded_frames:
            prepare_recorded(study, proto, args.recorded_frames, args.recorded_lots)
        else:
            if not args.cache_db:
                raise SystemExit("prepare needs --cache-db for a network preparation")
            prepare_network(study, proto, args.cache_db, args.resume)
        return 0
    if args.cmd == "lock":
        cmd_lock(study)
        return 0
    if args.cmd == "run":
        return cmd_run(study, args.binary, args.work_dir, args.jobs, args.timeout, args.out, not args.no_raw)
    if args.cmd == "render":
        cmd_render(study)
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
