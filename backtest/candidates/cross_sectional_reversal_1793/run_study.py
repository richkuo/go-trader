#!/usr/bin/env python3

import argparse
import copy
import csv
import datetime
import gzip
import hashlib
import importlib.util
import json
import math
import os
import re
import statistics
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from types import MappingProxyType

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
BACKTEST = os.path.join(REPO, "backtest")
REPLAY_DRIVER = os.path.join(BACKTEST, "basket_replay.py")
RANKER_RUNNER = os.path.join(REPO, "shared_scripts", "run_basket_ranker.py")
HOUR_MS = 3_600_000
INTERVAL_MS = {"1h": 3_600_000, "4h": 14_400_000, "1d": 86_400_000}
RESULT_SCHEMA = "go-trader.xsrev-basket-study-result"
RESULT_VERSION = 1
LOCK_SCHEMA = "go-trader.xsrev-basket-study-lock"
DATA_REF_SCHEMA = "go-trader.xsrev-basket-study-data-reference"
BASELINE_ORDER = ("cross_sectional_momentum", "mean_reversion_pro")
HYPOTHESES = ("net_positive", "beats_cross_sectional_momentum", "beats_mean_reversion_pro", "stress_net_positive")
FUNDING_DETAIL = re.compile(r"qty (-?[0-9.eE+-]+) x mark ([0-9.eE+-]+) .* x rate (-?[0-9.eE+-]+) at")


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
        self.data_ref_path = os.path.join(self.dir, "data_reference.json")
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
    return "xr-" + coin.lower()


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


def data_dir(proto):
    ref = proto["data"]["reference_dir"]
    return ref if os.path.isabs(ref) else os.path.normpath(os.path.join(REPO, ref))


def data_rel(study, proto, name):
    ref = proto["data"]["reference_dir"]
    if os.path.isabs(ref):
        return os.path.join(ref, name)
    return os.path.relpath(os.path.join(data_dir(proto), name), study.dir)


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


def cmd_reference(study):
    if os.path.exists(study.data_ref_path):
        raise SystemExit(f"{study.data_ref_path} exists; a data reference is never replaced")
    proto = load_json(study.protocol_path)
    ddir = data_dir(proto)
    manifest = os.path.join(ddir, "data_manifest.json")
    dm = load_json(manifest)
    got = sha256_file(manifest)
    want = proto["data"].get("reference_manifest_sha256")
    if want and got != want:
        raise SystemExit(f"the referenced data manifest has sha256 {got}, the protocol names {want}")
    files = {}
    for name, info in sorted(dm["files"].items()):
        path = os.path.join(ddir, name)
        h = sha256_file(path)
        if h != info["sha256"]:
            raise SystemExit(f"{name} has sha256 {h}, its own manifest freezes {info['sha256']}")
        files[name] = {"sha256": h, "kind": info["kind"], "coin": info["coin"]}
    lots = os.path.join(ddir, dm["lot_metadata"]["path"])
    ref = {
        "schema": DATA_REF_SCHEMA, "version": 1, "study": proto["study"],
        "note": "this study copies no data; it reads the referenced frozen files in place and refuses any hash change",
        "source_study": dm.get("study"), "reference_dir": proto["data"]["reference_dir"],
        "source_manifest": {"path": "data_manifest.json", "sha256": got},
        "files": files,
        "lot_metadata": {"path": dm["lot_metadata"]["path"], "sha256": sha256_file(lots)},
        "universe_members": list(dm["universe"]["members"]),
    }
    write_new(study.data_ref_path, ref)
    print(f"wrote {study.data_ref_path}: {len(files)} files from {ddir}")


def cmd_lock(study):
    if os.path.exists(study.lock_path):
        raise SystemExit(f"{study.lock_path} exists; a lock is never replaced (a new experiment needs a new study "
                         "directory)")
    proto = load_json(study.protocol_path)
    spec_path = os.path.join(REPO, proto["signal_spec"]["path"])
    lock = {"schema": LOCK_SCHEMA, "version": 1, "locked_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "protocol_sha256": sha256_file(study.protocol_path), "signal_spec_sha256": sha256_file(spec_path),
            "data_reference_sha256": sha256_file(study.data_ref_path)}
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
    if not os.path.isfile(study.data_ref_path):
        problems.append("data_reference.json is missing")
    got = {"protocol_sha256": sha256_file(study.protocol_path),
           "signal_spec_sha256": sha256_file(spec_path) if os.path.isfile(spec_path) else None,
           "data_reference_sha256": sha256_file(study.data_ref_path) if not problems else None}
    for key, val in sorted(got.items()):
        if lock.get(key) != val:
            problems.append(f"{key} is {val}, the lock freezes {lock.get(key)}")
    if got["signal_spec_sha256"] != proto["signal_spec"]["sha256"]:
        problems.append("the frozen signal specification changed")
    if problems:
        raise StudyRefusal("lock", "lock_mismatch", problems)
    return proto, lock


def load_registry_module():
    if BACKTEST not in sys.path:
        sys.path.insert(0, BACKTEST)
    from registry_loader import load_registry
    return load_registry("futures")


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
    driver = import_path("xsrev_study_replay_driver", REPLAY_DRIVER)
    for key, have in (("replay_plan", [driver.PLAN_SCHEMA, driver.PLAN_VERSION]),
                      ("replay_result", [driver.RESULT_SCHEMA, driver.RESULT_VERSION]),
                      ("adapter_protocol", [driver.ADAPTER_PROTOCOL, driver.ADAPTER_VERSION])):
        if list(contracts[key]) != have:
            problems.append(f"{key} is {have}, the protocol checked {contracts[key]}")
        checked["contracts"][key] = have
    ranker = import_path("xsrev_study_ranker_runner", RANKER_RUNNER)
    if getattr(ranker, "REQUEST_VERSION", None) != contracts["ranker_request_version"]:
        problems.append(f"ranker request version is {getattr(ranker, 'REQUEST_VERSION', None)}")
    checked["contracts"]["ranker_request_version"] = getattr(ranker, "REQUEST_VERSION", None)
    reg = load_registry_module()
    rankers = reg._registry.basket_rankers()
    registry = {}
    for name in (proto["candidate"], "cross_sectional_momentum"):
        entry = reg._registry.STRATEGIES.get(name) or {}
        if name not in rankers:
            problems.append(f"{name} is not a registered basket ranker")
        if entry.get("edge_status") != "no_edge":
            problems.append(f"{name} edge_status is {entry.get('edge_status')!r}")
        registry[name] = {"edge_status": entry.get("edge_status"), "edge_source": entry.get("edge_source"),
                          "edge_ref": entry.get("edge_ref"), "default_params": entry.get("default_params")}
    mrp = reg._registry.STRATEGIES.get("mean_reversion_pro") or {}
    frozen = proto["baselines"]["mean_reversion_pro"]["params"]
    if dict(mrp.get("default_params") or {}) != frozen:
        problems.append(f"mean_reversion_pro registered defaults {mrp.get('default_params')} differ from the frozen "
                        f"baseline parameters {frozen}")
    registry["mean_reversion_pro"] = {"default_params": mrp.get("default_params"),
                                      "short_entries": mrp.get("short_entries")}
    checked["contracts"]["registry"] = registry
    spec_src = open(os.path.join(REPO, "scheduler", "basket_replay_spec.go")).read()
    if 'basketReplayTargetsBaseline = "baseline"' not in spec_src:
        problems.append("the adapter source declares no baseline target input")
    cost_src = open(os.path.join(REPO, "scheduler", "fill_cost_policy.go")).read()
    if "fillCostRoleTakerAll" not in cost_src or "fillCostSpreadInSlippage" not in cost_src:
        problems.append("the cost policy source declares no taker_all and inside_slippage policy")
    cfg_src = open(os.path.join(REPO, "scheduler", "basket_config.go")).read()
    if 'json:"holding_horizon_seconds,omitempty"' not in cfg_src:
        problems.append("the basket configuration source declares no holding_horizon_seconds lifecycle field")
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


def source_hashes(proto):
    rels = list(proto["prerequisites"]["files"]) + [os.path.relpath(os.path.abspath(__file__), REPO),
                                                    proto["signal_spec"]["path"]]
    out = {}
    for rel in sorted(set(rels)):
        path = os.path.join(REPO, rel)
        out[rel] = sha256_file(path) if os.path.isfile(path) else None
    return out


def load_frozen_data(study, proto):
    ref = load_json(study.data_ref_path)
    ddir = data_dir(proto)
    manifest = os.path.join(ddir, ref["source_manifest"]["path"])
    problems = []
    if not os.path.isfile(manifest):
        raise StudyRefusal("data", "data_hash_mismatch", [f"the referenced data manifest {manifest} is missing"])
    got = sha256_file(manifest)
    if got != ref["source_manifest"]["sha256"]:
        problems.append(f"the referenced data manifest has sha256 {got}, the data reference freezes "
                        f"{ref['source_manifest']['sha256']}")
    if proto["data"].get("reference_manifest_sha256") and got != proto["data"]["reference_manifest_sha256"]:
        problems.append("the referenced data manifest differs from the hash the protocol names")
    dm = load_json(manifest)
    if sorted(dm["files"]) != sorted(ref["files"]):
        problems.append("the referenced data manifest lists other files than the data reference")
    for name, info in sorted(ref["files"].items()):
        path = os.path.join(ddir, name)
        if not os.path.isfile(path):
            problems.append(f"{name} is missing")
            continue
        h = sha256_file(path)
        if h != info["sha256"] or h != (dm["files"].get(name) or {}).get("sha256"):
            problems.append(f"{name} has sha256 {h}, the data reference freezes {info['sha256']}")
    lots_path = os.path.join(ddir, ref["lot_metadata"]["path"])
    if not os.path.isfile(lots_path) or sha256_file(lots_path) != ref["lot_metadata"]["sha256"]:
        problems.append("lot_metadata.json is missing or changed")
    if problems:
        raise StudyRefusal("data", "data_hash_mismatch", problems)
    members = list(dm["universe"]["members"])
    again, excluded = apply_universe_rule(proto, dm["universe"]["listing"])
    if again != members or members != ref["universe_members"]:
        raise StudyRefusal("data", "universe_mismatch",
                           [f"the frozen universe rule gives {again}, the referenced manifest lists {members}"])
    tf = proto["data"]["timeframe"]
    candles, funding = {}, {}
    for coin in members:
        candles[coin] = [[int(r["timestamp"]), float(r["open"]), float(r["high"]), float(r["low"]), float(r["close"]),
                          float(r["volume"])] for r in read_gz_csv(os.path.join(ddir, candle_name(coin, tf)))]
        funding[coin] = [(int(r["timestamp"]), float(r["rate"]))
                         for r in read_gz_csv(os.path.join(ddir, funding_name(coin)))]
    lots = load_json(lots_path)
    return dm, ref, members, excluded, candles, funding, lots


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
                            f"{iso(venue_absent[0])}; this study refuses them and never synthesizes a zero rate")
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
                          "funding_zero": sum(1 for r in signs if r == 0),
                          "funding_positive": sum(1 for r in signs if r > 0), "funding_negative": sum(1 for r in signs if r < 0),
                          "lot_sz_decimals": lots.get(coin)}
    return problems, coverage


def check_grid(proto, n_members):
    b = proto["basket"]
    lookback = int(b["lookback_bars"])
    cap = float(b["max_coin_notional_usd"]) / float(b["gross_budget_usd"])
    problems = []
    for r in proto["grid"]["return_lookback_bars"]:
        for v in proto["grid"]["volatility_lookback_bars"]:
            if lookback < max(r, v) + 1:
                problems.append(f"lookback_bars {lookback} is below max(R={r}, V={v}) + 1")
            for q in proto["grid"]["quantile"]:
                if not 0 < q < 0.5:
                    problems.append(f"quantile {q} is outside (0, 0.5)")
    for h in proto["grid"]["holding_horizon_seconds"]:
        if not isinstance(h, int) or h <= 0 or h % (INTERVAL_MS[proto["data"]["timeframe"]] // 1000):
            problems.append(f"holding_horizon_seconds {h} is not a positive whole number of bars")
    if int(proto["history_bars"]) < lookback + 1:
        problems.append("history_bars is below the ranker frame need")
    for q in proto["grid"]["quantile"]:
        k = max(1, int(math.floor(q * n_members)))
        if 2 * k > n_members or k * cap < 0.5 - 1e-9:
            problems.append(f"quantile {q} with {n_members} members gives {k} member(s) per side, which the per-coin "
                            "cap or the population cannot hold")
    return problems


def deadlines(proto, window):
    step = int(proto["basket"]["rebalance_interval_seconds"]) * 1000
    start, end = utc_ms(window[0]), utc_ms(window[1])
    first = -(-start // step) * step
    return list(range(first, end, step))


def closed_rows(rows, deadline_ms, ivl, keep):
    cut = [r for r in rows if r[0] + ivl <= deadline_ms]
    return cut[-keep:] if keep else cut


def frame_of(rows):
    import pandas as pd
    df = pd.DataFrame([[int(r[0])] + [float(x) for x in r[1:6]] for r in rows],
                      columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df


def deadline_prefix(rows, d, ivl, keep, coin):
    pre = closed_rows(rows, d, ivl, keep)
    if len(pre) < keep:
        return None, f"{coin}: {len(pre)} closed bars at the deadline, the frozen conversion needs {keep}"
    if pre[-1][0] + ivl != d:
        return None, f"{coin}: the last closed bar does not close at the deadline {iso(d)}"
    for a, b in zip(pre, pre[1:]):
        if b[0] - a[0] != ivl:
            return None, f"{coin}: the closed prefix has a gap before {iso(b[0])}"
    return pre, None


def allocation_context(proto):
    b = proto["basket"]
    ranker = import_path("xsrev_study_ranker_context", RANKER_RUNNER)
    return ranker.parse_allocation_context({
        "gross_budget_usd": b["gross_budget_usd"], "max_coin_notional_usd": b["max_coin_notional_usd"],
        "max_transition_net_usd": b["max_transition_net_usd"], "lookback_bars": b["lookback_bars"],
        "min_history_bars": b["min_history_bars"]})


def momentum_baseline(proto, members, candles, window, selected):
    spec = proto["baselines"]["cross_sectional_momentum"]
    ivl = INTERVAL_MS[proto["data"]["timeframe"]]
    keep = int(proto["basket"]["lookback_bars"])
    reg = load_registry_module()
    entry = reg._registry.get_basket_ranker(spec["strategy"])
    params = {k: selected[k] for k in spec["matched_params"]}
    reg._registry.validate_params(spec["strategy"], params)
    effective = {**dict(entry["default_params"]), **params}
    ctx = allocation_context(proto)
    if entry.get("basket_preflight") is not None:
        entry["basket_preflight"](MappingProxyType(dict(effective)), ctx)
    weights, holds, trace = {}, {}, {}
    for d in deadlines(proto, window):
        at = iso(d)
        frames, why = {}, None
        for coin in sorted(members):
            pre, why = deadline_prefix(candles[coin], d, ivl, keep, coin)
            if why:
                break
            frames[coin] = frame_of(pre)
        if why:
            holds[at] = why
            continue
        eligibility = {coin: {"symbol": coin, "status": "eligible", "closed_bars": keep} for coin in frames}
        res = entry["basket_fn"](frames, eligibility, ctx, **effective)
        if res.get("hold"):
            holds[at] = f"the ranker held: {res['hold']}"
            continue
        w = {coin: float(v) for coin, v in res["weights"].items()}
        if not any(v > 0 for v in w.values()):
            holds[at] = "the long side is empty"
            continue
        if not any(v < 0 for v in w.values()):
            holds[at] = "the short side is empty"
            continue
        weights[at] = w
        trace[at] = {"input_sha256": sha256_bytes(json.dumps({c: closed_rows(candles[c], d, ivl, keep) for c in sorted(frames)},
                                                             sort_keys=True).encode()),
                     "net_usd": math.fsum(w.values()) * float(proto["basket"]["gross_budget_usd"])}
    return weights, holds, {"params": effective, "deadlines": trace}


def mean_reversion_baseline(proto, members, candles, window):
    spec = proto["baselines"]["mean_reversion_pro"]
    ivl = INTERVAL_MS[proto["data"]["timeframe"]]
    keep = int(spec["input_prefix_bars"])
    reg = load_registry_module()
    strat = reg.get_strategy(spec["strategy"])
    params = dict(spec["params"])
    n = len(members)
    weights, holds, trace = {}, {}, {}
    for d in deadlines(proto, window):
        at = iso(d)
        sig, why = {}, None
        for coin in sorted(members):
            pre, why = deadline_prefix(candles[coin], d, ivl, keep, coin)
            if why:
                break
            res = strat["fn"](frame_of(pre), **params)
            last = res.iloc[-1]
            for col in spec["indicator_columns"]:
                v = last.get(col)
                if v is None or not math.isfinite(float(v)):
                    why = f"{coin}: the {col} indicator is not finite on the deadline bar"
                    break
            if why:
                break
            s = float(last["signal"])
            if s not in (-1.0, 0.0, 1.0):
                why = f"{coin}: the entry signal {s!r} is not -1, 0 or 1"
                break
            sig[coin] = int(s)
        if why:
            holds[at] = why
            continue
        weights[at] = {coin: s / n for coin, s in sig.items()}
        trace[at] = {"signals": {c: s for c, s in sorted(sig.items()) if s},
                     "net_usd": math.fsum(weights[at].values()) * float(proto["basket"]["gross_budget_usd"])}
    return weights, holds, {"params": params, "deadlines": trace}


def horizon_label(seconds):
    return f"{seconds // 86400}d" if seconds % 86400 == 0 else f"{seconds // 3600}h"


def run_id_for(params, horizon_s):
    q = str(params["quantile"]).replace(".", "p")
    return f"r{params['return_lookback_bars']}-q{q}-v{params['volatility_lookback_bars']}-h{horizon_label(horizon_s)}"


def grid_variants(proto):
    g = proto["grid"]
    out = []
    for r in g["return_lookback_bars"]:
        for q in g["quantile"]:
            for v in g["volatility_lookback_bars"]:
                for h in g["holding_horizon_seconds"]:
                    out.append(({"return_lookback_bars": r, "quantile": q, "volatility_lookback_bars": v}, h))
    return out


def run_specs(proto, selected=None):
    if selected is None:
        return [{"run_id": "train-" + run_id_for(p, h), "window": "train", "params": p, "horizon": h,
                 "target": "ranker", "cost": "primary"} for p, h in grid_variants(proto)]
    p, h = selected["params"], selected["holding_horizon_seconds"]
    out = [{"run_id": "heldout-candidate", "window": "held_out", "params": p, "horizon": h, "target": "ranker",
            "cost": "primary"},
           {"run_id": "heldout-candidate-stress", "window": "held_out", "params": p, "horizon": h, "target": "ranker",
            "cost": "stress"}]
    for name in BASELINE_ORDER:
        out.append({"run_id": f"heldout-baseline-{name.replace('_', '-')}", "window": "held_out", "params": None,
                    "horizon": h, "target": name, "cost": "primary"})
    return out


def portable_plan(study, proto, dm, lock, members, lots, run, weights):
    tf = proto["data"]["timeframe"]
    window = proto["windows"][run["window"]]
    b = proto["basket"]
    listings = {}
    for coin in members:
        first = iso(dm["universe"]["listing"][coin]["first_traded_ms"])
        listings[coin] = {"listed_at": first, "known_at": first}
    basket = {"id": b["id"], "ranker": proto["candidate"], "params": dict(run["params"] or {}),
              "members": [member_id(c) for c in members], "rebalance_interval_seconds": b["rebalance_interval_seconds"],
              "evaluation_window_seconds": b["evaluation_window_seconds"],
              "ranker_timeout_seconds": b["ranker_timeout_seconds"], "lookback_bars": b["lookback_bars"],
              "gross_budget_usd": b["gross_budget_usd"], "max_transition_net_usd": b["max_transition_net_usd"],
              "max_coin_notional_usd": b["max_coin_notional_usd"], "min_eligible_members": b["min_eligible_members"],
              "holding_horizon_seconds": run["horizon"],
              "eligibility": {"source": b["eligibility_source"], "version": b["eligibility_version"],
                              "min_history_bars": b["min_history_bars"], "listings": listings}}
    if run["target"] != "ranker":
        basket["params"] = {}
    plan = {
        "plan": "go-trader.basket-replay-plan", "version": 1, "run_id": run["run_id"], "state_dir": "<work>/state/" + run["run_id"],
        "source": {"study": proto["study"], "protocol_sha256": lock["protocol_sha256"],
                   "data_reference_sha256": lock["data_reference_sha256"], "candles": proto["data"]["candles"],
                   "funding": proto["data"]["funding"], "lot_metadata": proto["data"]["lot_metadata"]},
        "frames": {f"{c}|{tf}": {"path": data_rel(study, proto, candle_name(c, tf)),
                                 "sha256": dm["files"][candle_name(c, tf)]["sha256"]} for c in members},
        "funding": {c: {"path": data_rel(study, proto, funding_name(c)), "sha256": dm["files"][funding_name(c)]["sha256"],
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
    script = {at: copy.deepcopy(v) for at, v in sorted((proto.get("venue_script") or {}).items())
              if utc_ms(window[0]) <= utc_ms(at) < utc_ms(window[1])}
    if script:
        plan["venue_script"] = script
    return plan


def materialize(study, plan, work):
    out = copy.deepcopy(plan)
    out["state_dir"] = os.path.join(work, "state", plan["run_id"])
    for spec in list(out["frames"].values()) + list(out["funding"].values()):
        if not os.path.isabs(spec["path"]):
            spec["path"] = os.path.normpath(os.path.join(study.dir, spec["path"]))
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


def validity_problems(proto, run, ao, steps, lots_by_coin, baseline_weights, baseline_holds, members):
    problems = []
    save_failures = [f"step {s['index']} ({s['at']}): {e}" for s in steps for e in (s.get("save_errors") or [])]
    if save_failures:
        problems.append(f"{len(save_failures)} recorded state save failure(s); this study never scores a run with a "
                        f"save failure, even one a later durable save recovered: {save_failures[:5]}")
    blocked = [s["at"] for s in steps if s.get("save_blocked")]
    if blocked:
        problems.append(f"{len(blocked)} save-blocked step(s), first {blocked[0]}")
    want = proto["costs"][run["cost"]]
    applied = ((ao.get("cost") or {}).get("applied") or {})
    for key in ("taker_fee_pct", "slippage_pct", "fill_role", "spread"):
        if applied.get(key) != want.get(key):
            problems.append(f"the applied cost policy {key} is {applied.get(key)!r}, the protocol freezes {want.get(key)!r}")
    trades = ao.get("trades") or []
    off_grid, bad_sign, unparsed = [], 0, 0
    for t in trades:
        if t.get("trade_type") == "funding":
            m = FUNDING_DETAIL.search(t.get("details") or "")
            if not m:
                unparsed += 1
                continue
            qty, rate = float(m.group(1)), float(m.group(3))
            pnl = float(t.get("realized_pnl") or 0.0)
            want_sign = -math.copysign(1.0, qty * rate) if qty * rate else 0.0
            if pnl and want_sign and math.copysign(1.0, pnl) != want_sign:
                bad_sign += 1
            continue
        dec = lots_by_coin.get(t.get("symbol"))
        units = float(t.get("quantity") or 0.0) * (10 ** int(dec)) if dec is not None else None
        if units is None or abs(units - round(units)) > 1e-6:
            off_grid.append(f"{t.get('symbol')} {t.get('quantity')}")
    if off_grid:
        problems.append(f"{len(off_grid)} fill(s) are off the frozen lot grid: {off_grid[:5]}")
    if bad_sign:
        problems.append(f"{bad_sign} funding row(s) book the wrong sign for their position and rate")
    if unparsed:
        problems.append(f"{unparsed} funding row(s) carry no readable quantity and rate evidence")
    if baseline_weights is not None:
        by_deadline = {}
        for s in steps:
            for r in s.get("rebalances") or []:
                cur = by_deadline.setdefault(r["deadline_ms"], {"status": None, "targets": None, "hold_reason": None})
                cur["status"] = r.get("status")
                if r.get("targets"):
                    cur["targets"] = r["targets"]
                if r.get("hold_reason"):
                    cur["hold_reason"] = r["hold_reason"]
        coin_of = {member_id(c): c for c in members}
        for at, w in sorted(baseline_weights.items()):
            rb = by_deadline.get(utc_ms(at))
            if rb is None:
                problems.append(f"baseline deadline {at} has no rebalance record")
                continue
            if rb["status"] == "held":
                continue
            got = {coin_of[sid]: float(t.get("weight") or 0.0) for sid, t in (rb["targets"] or {}).items()}
            if got != {c: float(w.get(c, 0.0)) for c in members}:
                problems.append(f"baseline deadline {at}: the adapter froze weights {got}, the conversion froze {w}")
        for at in sorted(baseline_holds):
            rb = by_deadline.get(utc_ms(at))
            if rb is not None and rb["status"] != "held":
                problems.append(f"baseline deadline {at} carries no frozen weights but the adapter did not hold it")
    return problems


def summarize(proto, run, rep, members, lots, baseline=None):
    rec = rep["record"]
    gross = float(proto["basket"]["gross_budget_usd"])
    out = {"run_id": run["run_id"], "window": proto["windows"][run["window"]], "target": run["target"],
           "params": run["params"], "holding_horizon_seconds": run["horizon"], "cost": proto["costs"][run["cost"]],
           "replay_status": rec.get("status"), "replay_scored": bool(rec.get("scored")),
           "replay_promotion_eligible": bool(rec.get("promotion_eligible")),
           "adapter_exit_code": rec.get("adapter_exit_code"), "driver_exit_code": rep["driver_exit_code"],
           "seconds": rep["seconds"], "raw_result_sha256": rep.get("result_sha256"),
           "adapter_log_sha256": rep.get("log_sha256"), "complete": False, "valid": False}
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
    steps = ao.get("steps") or []
    out["save_errors"] = [{"index": s["index"], "at": s["at"], "errors": s["save_errors"]} for s in steps
                          if s.get("save_errors")]
    complete = (rec.get("status") == "complete" and rec.get("scored") is True and ao.get("status") == "complete"
                and (ao.get("coverage") or {}).get("complete") is True and rec.get("adapter_exit_code") == 0)
    if not complete:
        return out
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
    residual = (eq[-1] - initial) - (realized + funding_usd - fees + unrealized)
    out["accounting"] = {
        "fees_usd": fees, "funding_usd": funding_usd, "realized_close_pnl_pre_fee_usd": realized,
        "endpoint_unrealized_usd": unrealized, "equity_change_usd": eq[-1] - initial, "residual_usd": residual,
        "tolerance_usd": float(proto["accounting"]["tolerance_usd"]),
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
    kinds, expiry = {}, {"committed": 0, "actions": 0}
    seen_actions = {}
    for s in steps:
        for a in s.get("actions") or []:
            seen_actions[a["action_id"]] = a
    for a in seen_actions.values():
        if a.get("purpose") == "expiry":
            expiry["actions"] += 1
            if a.get("status") == "committed":
                expiry["committed"] += 1
    out["expiry_actions"] = expiry
    for t in fills:
        key = f"{t.get('trade_type') or ''}/{'close' if t.get('is_close') else 'open'}"
        kinds[key] = kinds.get(key, 0) + 1
    out["fill_kinds"] = dict(sorted(kinds.items()))
    out["mean_gross_usd"] = statistics.fmean(s["exposure"]["gross_usd"] for s in steps)
    out["mean_abs_net_usd"] = statistics.fmean(abs(s["exposure"]["net_usd"]) for s in steps)
    lots_by_coin = {c: int(lots[c]) for c in members}
    bw, bh = (baseline or (None, None))
    problems = validity_problems(proto, run, ao, steps, lots_by_coin, bw, bh, members)
    if abs(residual) > float(proto["accounting"]["tolerance_usd"]):
        problems.append(f"equity does not reconcile with booked fills, fees, funding and the endpoint book: residual "
                        f"${residual:.6f} exceeds the ${proto['accounting']['tolerance_usd']} tolerance")
    out["validity_problems"] = problems
    out["valid"] = not problems
    return out


def test_series(proto, xs):
    st = proto["statistics"]
    h = hac(xs, int(st["lag_weeks"]))
    from statistics import NormalDist
    z = NormalDist().inv_cdf(1 - float(st["alpha_one_sided"]) / int(st["family_size"]))
    if h is None:
        return {"n": len(xs), "mean": statistics.fmean(xs) if xs else None, "defined": False, "z": z,
                "sufficient": False, "condition": None}
    lower = h["mean"] - z * h["se"]
    suff = h["n"] >= int(st["min_clusters"]) and h["n_eff"] >= float(st["min_effective_clusters"])
    if lower > 0:
        cond = True
    elif h["mean"] <= 0:
        cond = False
    else:
        cond = None
    return dict(h, defined=True, z=z, lower_bound=lower, condition=cond, sufficient=suff)


def stability(proto, selected, train):
    g = proto["grid"]
    axes = (("return_lookback_bars", g["return_lookback_bars"]), ("quantile", g["quantile"]),
            ("volatility_lookback_bars", g["volatility_lookback_bars"]))
    by_key = {}
    for r in train:
        by_key[(r["params"]["return_lookback_bars"], r["params"]["quantile"], r["params"]["volatility_lookback_bars"],
                r["holding_horizon_seconds"])] = r
    p, h = selected["params"], selected["holding_horizon_seconds"]
    base = [p["return_lookback_bars"], p["quantile"], p["volatility_lookback_bars"], h]
    neighbors = []
    for ai, (name, values) in enumerate(list(axes) + [("holding_horizon_seconds", g["holding_horizon_seconds"])]):
        i = values.index(base[ai])
        for j in (i - 1, i + 1):
            if 0 <= j < len(values):
                key = list(base)
                key[ai] = values[j]
                r = by_key.get(tuple(key))
                mean = r.get("train_objective") if r and r.get("valid") else None
                neighbors.append({"axis": name, "value": values[j], "run_id": r["run_id"] if r else None,
                                  "valid": bool(r and r.get("valid")), "train_mean": mean})
    if any(n["valid"] and n["train_mean"] is not None and n["train_mean"] <= 0 for n in neighbors):
        cond = False
    elif all(n["valid"] and n["train_mean"] is not None and n["train_mean"] > 0 for n in neighbors):
        cond = True
    else:
        cond = None
    return {"neighbors": neighbors, "condition": cond}


def verdict(proto, held, stable):
    cand = held.get("heldout-candidate")
    stress = held.get("heldout-candidate-stress")
    base = {name: held.get(f"heldout-baseline-{name.replace('_', '-')}") for name in BASELINE_ORDER}
    runs = [cand, stress] + list(base.values())
    if not all(r and r.get("complete") and r.get("valid") for r in runs):
        detail = []
        for r in runs:
            if r is None:
                detail.append("a held-out replay is missing")
            elif not r.get("complete"):
                detail.append(f"{r['run_id']} replay is not complete ({r.get('replay_status')}; "
                              f"{r.get('accounting_problem') or (r.get('refusal') or {}).get('reason')})")
            elif not r.get("valid"):
                detail.append(f"{r['run_id']} failed the study validity gates: {r.get('validity_problems')}")
        return {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                "promotion_eligible": False, "reason": "accounting_invalid", "detail": detail,
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
    if [c["to"] for c in stress["clusters"]] != [c["to"] for c in cand["clusters"]]:
        return {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                "promotion_eligible": False, "reason": "clusters_misaligned", "detail": ["stress"],
                "edge_source": "study_inconclusive"}
    tests["stress_net_positive"] = test_series(proto, [c["return_on_gross"] for c in stress["clusters"]])
    sufficient = all(t["sufficient"] and t["defined"] for t in tests.values())
    conds = {k: tests[k].get("condition") for k in HYPOTHESES}
    conds["stable"] = stable["condition"]
    share = cand.get("largest_positive_member_share")
    conds["not_concentrated"] = share is not None and share <= float(proto["concentration"]["max_largest_member_share"])
    out = {"tests": tests, "conditions": conds, "sufficient": sufficient, "stability": stable}
    if not sufficient:
        why = [f"{k}: n={t['n']} n_eff={round(t.get('n_eff', 0) or 0, 2)} defined={t['defined']}"
               for k, t in sorted(tests.items()) if not (t["sufficient"] and t["defined"])]
        out.update(verdict="inconclusive", scored=False, valid_economic_result=False, promotion_eligible=False,
                   reason="insufficient_independent_evidence", detail=why, edge_source="study_inconclusive")
        return out
    if all(v is True for v in conds.values()):
        out.update(verdict="pass", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="every condition true", detail=[], edge_source="unvalidated")
    elif any(conds[k] is False for k in HYPOTHESES):
        out.update(verdict="fail", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="a required net, excess or stressed mean is at or below zero",
                   detail=sorted(k for k in HYPOTHESES if conds[k] is False), edge_source="study_fail")
    else:
        out.update(verdict="inconclusive", scored=True, valid_economic_result=True, promotion_eligible=False,
                   reason="no required mean is at or below zero and a confidence, stability or concentration "
                          "condition is unresolved or false",
                   detail=sorted(k for k, v in conds.items() if v is not True), edge_source="study_inconclusive")
    return out


def select(proto, train):
    eligible = [r for r in train if r.get("complete") and r.get("valid") and r.get("clusters")]
    for r in train:
        if r.get("complete") and r.get("valid") and r.get("clusters"):
            r["train_objective"] = statistics.fmean(c["return_on_gross"] for c in r["clusters"])
    if not eligible:
        return None, []
    ranked = sorted(eligible, key=lambda r: (-r["train_objective"], r["params"]["return_lookback_bars"],
                                             r["params"]["quantile"], r["params"]["volatility_lookback_bars"],
                                             r["holding_horizon_seconds"]))
    best = ranked[0]
    return {"params": dict(best["params"]), "holding_horizon_seconds": best["holding_horizon_seconds"],
            "run_id": best["run_id"]}, [r["run_id"] for r in ranked]


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


def execute(binary, work, jobs, timeout, plans, runs, final_marks):
    def go(run):
        rep = run_replay(binary, plans[run["run_id"]], work, run["run_id"], timeout)
        rep["final_marks"] = final_marks[run["window"]]
        return run, rep
    with ThreadPoolExecutor(max_workers=max(1, jobs)) as pool:
        return list(pool.map(go, runs))


def check_plan_files(plans_dir, plans, require=False):
    problems = []
    for run_id, plan in sorted(plans.items()):
        path = os.path.join(plans_dir, run_id + ".json")
        if not os.path.exists(path):
            if require:
                problems.append(f"plans/{run_id}.json is missing")
        else:
            with open(path, "rb") as fh:
                if fh.read() != dump_json(plan).encode():
                    problems.append(f"plans/{run_id}.json differs from the plan this run replays")
    if problems:
        raise StudyRefusal("plans", "plan_file_mismatch", problems)


def keep(study, keep_raw, run_id, rep):
    if not keep_raw:
        return
    os.makedirs(study.raw_dir, exist_ok=True)
    write_gz_json(os.path.join(study.raw_dir, run_id + ".json.gz"), rep["record"])


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
        dm, ref, members, excluded, candles, funding, lots = load_frozen_data(study, proto)
        grid_problems = check_grid(proto, len(members))
        if grid_problems:
            raise StudyRefusal("protocol", "grid_infeasible", grid_problems)
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
    for wname in ("train", "held_out"):
        last = utc_ms(proto["windows"][wname][1]) - tf_ms
        final_marks[wname] = {c: next((r[1] for r in candles[c] if r[0] == last), None) for c in members}
    os.makedirs(study.plans_dir, exist_ok=True)
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    attempts, portable = [], {}

    def prepare_runs(runs, weights_by):
        plans = {run["run_id"]: portable_plan(study, proto, dm, lock, members, lots, run, weights_by.get(run["target"]))
                 for run in runs}
        check_plan_files(study.plans_dir, plans)
        paths = {}
        for run_id, plan in plans.items():
            portable[run_id] = plan
            ppath = os.path.join(study.plans_dir, run_id + ".json")
            if not os.path.exists(ppath):
                with open(ppath, "w") as fh:
                    fh.write(dump_json(plan))
            paths[run_id] = materialize(study, plan, work)
        return paths

    def refuse(exc):
        write_new(out_path, refusal_record(proto_name, exc, {"attempts": attempts}))
        print(f"study refused: {exc}", file=sys.stderr)
        return 3

    def record(run, rep, baseline=None):
        s = summarize(proto, run, rep, members, lots, baseline)
        s["plan_sha256"] = sha256_file(os.path.join(work, "plans", run["run_id"] + ".json"))
        s["portable_plan_sha256"] = sha256_bytes(dump_json(portable[run["run_id"]]).encode())
        attempts.append({"run_id": run["run_id"], "status": s["replay_status"], "complete": s["complete"],
                         "valid": s["valid"]})
        keep(study, keep_raw, run["run_id"], rep)
        return s

    train_runs = run_specs(proto)
    try:
        train_paths = prepare_runs(train_runs, {})
    except StudyRefusal as exc:
        return refuse(exc)
    train_out = [record(run, rep) for run, rep in
                 execute(binary, work, jobs, timeout, train_paths, train_runs, final_marks)]
    selected, ranking = select(proto, train_out)
    held_out, weights_meta, verdict_rec, stable = [], {}, None, None
    if selected is None:
        verdict_rec = {"verdict": "inconclusive", "scored": False, "valid_economic_result": False,
                       "promotion_eligible": False, "reason": "no_valid_training_variant",
                       "detail": sorted(r["run_id"] for r in train_out), "edge_source": "study_inconclusive"}
    else:
        stable = stability(proto, selected, train_out)
        held_window = proto["windows"]["held_out"]
        conversions = {
            "cross_sectional_momentum": momentum_baseline(proto, members, candles, held_window, selected["params"]),
            "mean_reversion_pro": mean_reversion_baseline(proto, members, candles, held_window),
        }
        weights_by = {name: conv[0] for name, conv in conversions.items()}
        for name, (w, holds, meta) in conversions.items():
            weights_meta[name] = dict(meta, policy=proto["baselines"][name], weighted_deadlines=len(w),
                                      nonzero_target_deadlines=sum(1 for x in w.values() if any(x.values())),
                                      held_deadlines=holds, weights=w)
        held_runs = run_specs(proto, selected)
        try:
            held_paths = prepare_runs(held_runs, weights_by)
        except StudyRefusal as exc:
            return refuse(exc)
        for run, rep in execute(binary, work, jobs, timeout, held_paths, held_runs, final_marks):
            conv = conversions.get(run["target"])
            held_out.append(record(run, rep, (conv[0], conv[1]) if conv else None))
        verdict_rec = verdict(proto, {s["run_id"]: s for s in held_out}, stable)
    result = {
        "schema": RESULT_SCHEMA, "version": RESULT_VERSION, "study": proto["study"], "candidate": proto["candidate"],
        "started_at_utc": started, "finished_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "lock": lock, "source": src, "prerequisites": checked, "source_hashes": source_hashes(proto),
        "data_reference": {"sha256": lock["data_reference_sha256"], "source_manifest": ref["source_manifest"],
                           "reference_dir": ref["reference_dir"], "source_study": ref["source_study"]},
        "binary": {"sha256": sha256_file(binary)}, "tools": tool_versions(binary),
        "universe": {"members": members, "excluded": excluded}, "coverage": coverage,
        "selected": selected, "training_ranking": ranking,
        "training": [compact(s) for s in train_out], "held_out": [compact(s) for s in held_out],
        "baseline_targets": weights_meta, "attempts": attempts,
        "study_verdict": verdict_rec, "live_use": "refused",
        "invocation": {"argv": sys.argv, "jobs": jobs, "timeout": timeout},
    }
    try:
        check_plan_files(study.plans_dir, portable, require=True)
    except StudyRefusal as exc:
        return refuse(exc)
    write_new(out_path, result)
    manifest = {"study": proto["study"], "results": os.path.basename(out_path),
                "results_sha256": sha256_file(out_path), "protocol_sha256": lock["protocol_sha256"],
                "data_reference_sha256": lock["data_reference_sha256"], "signal_spec_sha256": lock["signal_spec_sha256"],
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


def pct(x, digits=2):
    return "n/a" if x is None else f"{x * 100:.{digits}f}%"


def num(x, digits=4):
    return "n/a" if x is None else f"{x:.{digits}f}"


def cmd_render(study):
    res = load_json(study.results_path)
    proto = load_json(study.protocol_path)
    ref = load_json(study.data_ref_path)
    dm = load_json(os.path.join(data_dir(proto), ref["source_manifest"]["path"]))
    v = res["study_verdict"]
    rel = os.path.relpath(study.dir, REPO)
    L = []
    L.append(f"# Cross-sectional reversal basket study (issue {proto['issue']})")
    L.append("")
    L.append(f"Verdict: **{v['verdict']}** (study scored={str(v['scored']).lower()}, valid economic result="
             f"{str(v['valid_economic_result']).lower()}). Reason: {v['reason']}.")
    L.append(f"Edge metadata: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`. Live basket use stays refused.")
    L.append("")
    L.append("This report is generated by `run_study.py render` from `results.json`, `protocol.json`, "
             "`data_reference.json` and the referenced data manifest. It endorses no trading setting. A negative "
             "result is a valid outcome.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append(f"uv run --no-sync python {rel}/run_study.py reference")
    L.append(f"uv run --no-sync python {rel}/run_study.py lock")
    L.append("go -C scheduler build -o <bin>/go-trader .")
    L.append(f"uv run --no-sync python {rel}/run_study.py run --binary <bin>/go-trader --work-dir <new dir> --jobs N")
    L.append(f"uv run --no-sync python {rel}/run_study.py render")
    L.append("```")
    L.append("")
    L.append(f"- Study run argv: `{' '.join(res['invocation']['argv'][1:])}` (jobs {res['invocation']['jobs']}).")
    L.append(f"- Source commit `{res['source']['commit']}` (dirty={str(res['source']['dirty']).lower()}); binary sha256 "
             f"`{res['binary']['sha256']}`; tools: {res['tools'].get('go')}; {res['tools'].get('go_trader')}; "
             f"python {res['tools'].get('python')}.")
    L.append(f"- Protocol sha256 `{res['lock']['protocol_sha256']}`, signal specification sha256 "
             f"`{res['lock']['signal_spec_sha256']}` (unchanged), data reference sha256 "
             f"`{res['lock']['data_reference_sha256']}` (referenced manifest `{ref['reference_dir']}/data_manifest.json` "
             f"sha256 `{ref['source_manifest']['sha256']}`).")
    L.append(f"- Prerequisites checked: {', '.join(f'{k} = `{s[:12]}`' for k, s in sorted(res['prerequisites']['commits'].items()))}.")
    c = res["prerequisites"]["contracts"]
    L.append(f"- Contracts checked: plan {c['replay_plan']}, result {c['replay_result']}, adapter {c['adapter_protocol']} "
             f"(binary probe answered {c['adapter_probe']['protocol']} v{c['adapter_probe']['version']}), ranker request "
             f"v{c['ranker_request_version']}, baseline target input, the taker_all / inside_slippage cost path and the "
             "holding_horizon_seconds lifecycle field present.")
    L.append("- Every replay ran through `backtest/basket_replay.py` and the one `go-trader basket-replay` adapter: the "
             "real paper basket coordinator, holding-horizon lifecycle, member management, ranker runner, risk, funding "
             "and booking owners on a simulated clock, each in its own new temporary state directory, with no live route.")
    L.append("")
    L.append("## Data, universe and reuse")
    L.append("")
    L.append(f"- {proto['data']['reuse']}")
    L.append(f"- Candles: {proto['data']['candles']}.")
    L.append(f"- Funding: {proto['data']['funding']}.")
    L.append(f"- Lot metadata: {proto['data']['lot_metadata']} (prepared {dm['prepared_at_utc']}).")
    L.append(f"- Members ({len(res['universe']['members'])}): {', '.join(res['universe']['members'])}.")
    for coin, why in sorted(res["universe"]["excluded"].items()):
        L.append(f"- Excluded {coin}: {why}.")
    L.append(f"- {proto['universe']['survivorship']}.")
    L.append(f"- {proto['window_disclosure']}")
    L.append("")
    L.append("| member | listed (first traded bar) | training funding hours (+/0/-) | held-out funding hours (+/0/-) | szDecimals |")
    L.append("|---|---|---|---|---|")
    for coin in res["universe"]["members"]:
        lst = dm["universe"]["listing"][coin]
        tr, cov = res["coverage"]["train"][coin], res["coverage"]["held_out"][coin]
        L.append(f"| {coin} | {iso(lst['first_traded_ms'])[:10]} | {tr['funding_hours']} ({tr['funding_positive']}/"
                 f"{tr['funding_zero']}/{tr['funding_negative']}) | {cov['funding_hours']} ({cov['funding_positive']}/"
                 f"{cov['funding_zero']}/{cov['funding_negative']}) | {cov['lot_sz_decimals']} |")
    L.append("")
    L.append("## Frozen setup")
    L.append("")
    b = proto["basket"]
    L.append(f"- Windows: training {proto['windows']['train'][0]} to {proto['windows']['train'][1]}, held-out "
             f"{proto['windows']['held_out'][0]} to {proto['windows']['held_out'][1]}; daily steps; weekly rebalance "
             f"deadlines (Thursday 00:00 UTC); {proto['history_bars']} warm-up bars.")
    L.append(f"- Basket: gross budget ${b['gross_budget_usd']}, per-coin cap ${b['max_coin_notional_usd']}, net bound "
             f"${b['max_transition_net_usd']} for every arm, lookback {b['lookback_bars']} bars, minimum history "
             f"{b['min_history_bars']} bars; members ${proto['member_config']['capital']} paper capital each at leverage "
             f"{proto['member_config']['leverage']} (per-member cash, no pooled cash or margin).")
    L.append(f"- Holding horizon: {proto['holding_horizon']}")
    L.append(f"- Member close policy: {proto['member_close_policy']}.")
    L.append(f"- Portfolio risk: {proto['portfolio_risk']}.")
    L.append(f"- Costs: primary {proto['costs']['primary']}; stress {proto['costs']['stress']}. {proto['costs']['note']}.")
    L.append(f"- Grid: {proto['grid']} (all {len(res['training'])} variants listed below).")
    st = proto["statistics"]
    L.append(f"- Statistics: {st['cluster']}; {st['inference']} (lag {st['lag_weeks']} weeks); Bonferroni family "
             f"{st['family_size']} ({st['family_reason']}) at one-sided alpha {st['alpha_one_sided']}; sufficiency "
             f"{st['min_clusters']} clusters and n_eff >= {st['min_effective_clusters']} for every tested series.")
    L.append(f"- Validity gates: {' '.join(proto['accounting']['rules'])}")
    L.append("")
    L.append("## Training variants (every variant tried)")
    L.append("")
    L.append("| run | R | q | V | horizon | replay | valid | weeks | mean weekly (gross) | total (gross) | max DD (gross) | turnover/week | seconds |")
    L.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for s in res["training"]:
        p = s["params"]
        weeks = len(s.get("clusters") or [])
        mean = statistics.fmean(c["return_on_gross"] for c in s["clusters"]) if weeks else None
        L.append(f"| {s['run_id']} | {p['return_lookback_bars']} | {p['quantile']} | {p['volatility_lookback_bars']} | "
                 f"{horizon_label(s['holding_horizon_seconds'])} | {s['replay_status']} | {str(s.get('valid')).lower()} | "
                 f"{weeks} | {pct(mean, 3)} | {pct(s.get('total_return_on_gross'))} | "
                 f"{pct(s.get('max_drawdown_on_gross'))} | {pct(s.get('turnover_on_gross_per_cluster'))} | {s['seconds']} |")
    L.append("")
    for s in res["training"]:
        if s.get("validity_problems") or not s.get("complete"):
            L.append(f"- {s['run_id']}: complete={str(s.get('complete')).lower()}; "
                     f"{s.get('validity_problems') or s.get('accounting_problem') or s.get('refusal')}.")
    sel = res.get("selected")
    L.append(f"Selected on training only: `{sel}`; ranking {res['training_ranking']}.")
    L.append("")
    if v.get("stability"):
        L.append(f"Training-neighbor stability (frozen before selection): condition {v['stability']['condition']}; "
                 + "; ".join(f"{n['axis']}={n['value']}: {pct(n['train_mean'], 3)} (valid {str(n['valid']).lower()})"
                             for n in v["stability"]["neighbors"]) + ".")
        L.append("")
    L.append("## Held-out results")
    L.append("")
    L.append("| arm | replay | valid | total (gross) | max DD (gross) | turnover/week | mean gross $ | mean abs net $ | fees $ | funding $ | positions | expiry closes | largest member share |")
    L.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for s in res["held_out"]:
        a = s.get("accounting") or {}
        L.append(f"| {s['run_id']} | {s['replay_status']} | {str(s.get('valid')).lower()} | "
                 f"{pct(s.get('total_return_on_gross'))} | {pct(s.get('max_drawdown_on_gross'))} | "
                 f"{pct(s.get('turnover_on_gross_per_cluster'))} | {num(s.get('mean_gross_usd'), 0)} | "
                 f"{num(s.get('mean_abs_net_usd'), 0)} | {num(a.get('fees_usd'), 2)} | {num(a.get('funding_usd'), 2)} | "
                 f"{s.get('positions_opened', 'n/a')} | {(s.get('expiry_actions') or {}).get('committed', 'n/a')} | "
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
    L.append("| arm | equity change $ | realized close (pre-fee) $ | funding $ | fees $ | endpoint unrealized $ | residual $ | funding rows +/- | save errors | rebalances | holds | protective exceptions |")
    L.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
    for s in res["training"] + res["held_out"]:
        a = s.get("accounting") or {}
        r = s.get("rebalances") or {}
        L.append(f"| {s['run_id']} | {num(a.get('equity_change_usd'), 2)} | {num(a.get('realized_close_pnl_pre_fee_usd'), 2)} | "
                 f"{num(a.get('funding_usd'), 2)} | {num(a.get('fees_usd'), 2)} | {num(a.get('endpoint_unrealized_usd'), 2)} | "
                 f"{num(a.get('residual_usd'), 4)} | {a.get('funding_rows_positive')}/{a.get('funding_rows_negative')} | "
                 f"{len(s.get('save_errors') or [])} | {r.get('by_status')} | {r.get('holds')} | {r.get('protective_exceptions')} |")
    L.append("")
    L.append("## Baselines")
    L.append("")
    for name in BASELINE_ORDER:
        meta = res["baseline_targets"].get(name)
        if not meta:
            continue
        pol = meta["policy"]
        L.append(f"- `{name}`: {pol['rule']} Parameters {meta['params']}. {meta['weighted_deadlines']} deadlines carry "
                 f"frozen weights, {meta['nonzero_target_deadlines']} of them a nonzero target; "
                 f"{len(meta['held_deadlines'])} hold the whole set ({sorted(set(meta['held_deadlines'].values()))[:3]}).")
    L.append(f"- {proto['baselines']['common']}")
    L.append("")
    L.append("## Limits")
    L.append("")
    for line in proto["limits"]:
        L.append(f"- {line}")
    L.append("")
    with open(study.report_path, "w") as fh:
        fh.write("\n".join(L))
    print(f"wrote {study.report_path}")


def main(argv=None):
    ap = argparse.ArgumentParser(description="Frozen cross-sectional reversal basket study (issue 1790). "
                                             "Research-only and suggest-only: it never edits registry metadata, live "
                                             "defaults or deployed configuration.")
    ap.add_argument("--study-dir", default=HERE)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("reference")
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
    if args.cmd == "reference":
        cmd_reference(study)
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
