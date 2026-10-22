#!/usr/bin/env python3

import argparse
import calendar
import json
import os
import subprocess
import sys
import time
import urllib.request

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))
sys.path.insert(0, os.path.join(_ROOT, "backtest"))

import book_archive as ba  # noqa: E402
import numpy as np  # noqa: E402

SPEC_PATH = os.path.join(_HERE, "candidate_spec.json")
DATA = os.path.join(_HERE, "data")
ARCHIVE = os.path.join(DATA, "archive")
RAW = os.path.join(ARCHIVE, "raw")
SERIES = os.path.join(ARCHIVE, "series")
MANIFEST = os.path.join(DATA, "archive_manifest.json")
FUNDING = os.path.join(DATA, "funding.json")
VENUE = os.path.join(DATA, "venue_meta.json")
INFO_URL = "https://api.hyperliquid.xyz/info"
MANIFEST_SCHEMA = "go_trader_obi_1635_archive_manifest/v1"


def spec() -> dict:
    with open(SPEC_PATH) as fh:
        return json.load(fh)


def _day_list(first: str, last: str) -> list:
    out = []
    t = ba._day_ms(first.replace("-", ""))
    end = ba._day_ms(last.replace("-", ""))
    while t <= end:
        out.append(time.strftime("%Y%m%d", time.gmtime(t / 1000)))
        t += ba.DAY_MS
    return out


def study_days(s: dict) -> tuple:
    w = s["data_plan"]["window"]
    days = _day_list(w["first_day"], w["last_day"])
    lookback = time.strftime("%Y%m%d", time.gmtime((ba._day_ms(days[0]) - ba.DAY_MS) / 1000))
    return lookback, days


def expected_keys(s: dict) -> list:
    lookback, days = study_days(s)
    keys = []
    for day in [lookback] + days:
        for coin in s["data_plan"]["coins"]:
            for hour in range(24):
                keys.append(ba.book_key(day, hour, coin))
        keys.append(ba.asset_ctx_key(day))
    return keys


def _aws(args: list) -> subprocess.CompletedProcess:
    return subprocess.run(["aws"] + args + ["--request-payer", "requester"], capture_output=True, text=True)


def fetch() -> dict:
    s = spec()
    lookback, days = study_days(s)
    os.makedirs(RAW, exist_ok=True)
    for day in [lookback] + days:
        args = ["s3", "sync", f"s3://{ba.ARCHIVE_BUCKET}/market_data/{day}/", os.path.join(RAW, "market_data", day),
                "--exclude", "*", "--only-show-errors"]
        for coin in s["data_plan"]["coins"]:
            args += ["--include", f"*/l2Book/{coin}.lz4"]
        r = _aws(args)
        if r.returncode != 0:
            raise SystemExit(f"sync market_data/{day} failed: {r.stderr.strip()}")
        key = ba.asset_ctx_key(day)
        target = os.path.join(RAW, key)
        if not os.path.exists(target):
            r = _aws(["s3", "cp", f"s3://{ba.ARCHIVE_BUCKET}/{key}", target, "--only-show-errors"])
            if r.returncode != 0 and "404" not in r.stderr and "Not Found" not in r.stderr:
                raise SystemExit(f"cp {key} failed: {r.stderr.strip()}")
    files = []
    total = 0
    for key in expected_keys(s):
        path = os.path.join(RAW, key)
        if os.path.exists(path):
            size = os.path.getsize(path)
            total += size
            files.append({"key": key, "status": "present", "size": size, "sha256": ba.sha256_file(path)})
            continue
        r = _aws(["s3api", "head-object", "--bucket", ba.ARCHIVE_BUCKET, "--key", key])
        if r.returncode == 0:
            raise SystemExit(f"{key} exists in the archive but was not downloaded")
        if "404" not in r.stderr and "Not Found" not in r.stderr:
            raise SystemExit(f"head-object {key} failed: {r.stderr.strip()}")
        files.append({"key": key, "status": "absent"})
    manifest = {"schema": MANIFEST_SCHEMA, "bucket": ba.ARCHIVE_BUCKET, "request_payer": "requester",
                "fetched_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "spec_window": s["data_plan"]["window"],
                "present": sum(1 for f in files if f["status"] == "present"), "absent": sum(1 for f in files if f["status"] == "absent"),
                "bytes": total, "files": files}
    with open(MANIFEST, "w") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return {k: manifest[k] for k in ("present", "absent", "bytes")}


def _post(body: dict) -> object:
    for i in range(6):
        try:
            req = urllib.request.Request(INFO_URL, data=json.dumps(body).encode("utf-8"), headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.load(resp)
        except Exception as exc:
            last = exc
            time.sleep(2 * (i + 1))
    raise SystemExit(f"{body.get('type')} failed: {last}")


def _iso_ms(text: str) -> int:
    return calendar.timegm(time.strptime(text, "%Y-%m-%dT%H:%M:%SZ")) * 1000


def fetch_funding() -> dict:
    s = spec()
    span = s["data_plan"]["objects"]["funding"]
    start = _iso_ms(span.split(" from ")[1].split(" to ")[0])
    end = _iso_ms(span.split(" to ")[1])
    out = {"schema": "go_trader_obi_1635_funding/v1", "source": "Hyperliquid info fundingHistory", "start_ms": start, "end_ms": end,
           "fetched_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "coins": {}}
    for coin in s["data_plan"]["coins"]:
        rows = {}
        cursor = start
        while cursor < end:
            batch = _post({"type": "fundingHistory", "coin": coin, "startTime": cursor, "endTime": end})
            if not batch:
                break
            for r in batch:
                rows[int(r["time"])] = {"time": int(r["time"]), "fundingRate": r["fundingRate"], "premium": r["premium"]}
            last = max(int(r["time"]) for r in batch)
            if last + 1 <= cursor:
                break
            cursor = last + 1
        out["coins"][coin] = [rows[k] for k in sorted(rows) if start <= k < end]
    with open(FUNDING, "w") as fh:
        json.dump(out, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return {c: len(v) for c, v in out["coins"].items()}


def fetch_venue() -> dict:
    meta = _post({"type": "meta"})
    s = spec()
    assets = {a["name"]: a for a in meta["universe"] if a.get("name") in s["data_plan"]["coins"]}
    tables = {str(tid): t for tid, t in meta.get("marginTables", []) if tid in {a.get("marginTableId") for a in assets.values()}}
    out = {"schema": "go_trader_obi_1635_venue/v1", "source": "Hyperliquid info meta", "fetched_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
           "assets": assets, "margin_tables": tables}
    with open(VENUE, "w") as fh:
        json.dump(out, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return assets


def import_series() -> dict:
    s = spec()
    with open(MANIFEST) as fh:
        manifest = json.load(fh)
    lookback, days = study_days(s)
    result = ba.import_window(RAW, SERIES, s["data_plan"]["coins"], [lookback] + days, manifest)
    with open(os.path.join(DATA, "import_report.json"), "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return result["coins"]



def _load_day(name: str) -> dict:
    return ba.read_day(os.path.join(SERIES, name))


def _verify_series(report: dict) -> None:
    import hashlib
    import gzip
    for name, want in sorted(report["day_files"].items()):
        with gzip.open(os.path.join(SERIES, name), "rb") as fh:
            got = hashlib.sha256(fh.read()).hexdigest()
        if got != want:
            raise SystemExit(f"integrity: series file {name} no longer matches the import report")


def load_market(s: dict) -> dict:
    import book_event_simulator as sim
    with open(os.path.join(DATA, "import_report.json")) as fh:
        report = json.load(fh)
    _verify_series(report)
    with open(VENUE) as fh:
        venue = json.load(fh)
    with open(FUNDING) as fh:
        funding_doc = json.load(fh)
    with open(MANIFEST) as fh:
        manifest = json.load(fh)
    lookback, days = study_days(s)
    first_ms = ba._day_ms(lookback)
    end_ms = ba._day_ms(days[-1]) + ba.DAY_MS
    books, bars, marks, funding = {}, {}, {}, {}
    sz_dec, max_lev = {}, {}
    for coin in s["data_plan"]["coins"]:
        asset = venue["assets"][coin]
        sz_dec[coin] = int(asset["szDecimals"])
        max_lev[coin] = float(asset["maxLeverage"])
        names = [f"{coin}_{d}.json.gz" for d in [lookback] + days] + [f"{coin}_tail.json.gz"]
        markers, chunks = [], []

        def chunk_iter():
            for n in names:
                day = _load_day(n)
                markers.extend(day["markers"])
                yield day["observations"]
        cb = sim.CoinBooks(coin, chunk_iter(), [], 6 - sz_dec[coin], sz_dec[coin])
        markers.sort(key=lambda m: (m["at_ms"], m["seq"]))
        cb.mk_at = np.array([m["at_ms"] for m in markers], dtype=np.int64)
        cb.mk_seq = np.array([m["seq"] for m in markers], dtype=np.int64)
        cb.markers = markers
        books[coin] = cb
        bars[coin] = sim.Bars(cb, first_ms, end_ms + 2 * ba.HOUR_MS)
        funding[coin] = {(int(r["time"]) // ba.HOUR_MS) * ba.HOUR_MS: float(r["fundingRate"]) for r in funding_doc["coins"][coin]}
    rows = {c: {} for c in s["data_plan"]["coins"]}
    import lz4_frame
    import csv
    import io
    for day in [lookback] + days:
        key = ba.asset_ctx_key(day)
        path = os.path.join(RAW, key)
        if not os.path.exists(path):
            continue
        with open(path, "rb") as fh:
            text = lz4_frame.decompress(fh.read()).decode("utf-8")
        for r in csv.DictReader(io.StringIO(text)):
            c = r.get("coin")
            if c in rows:
                t = _iso_ms(r["time"])
                rows[c][t] = (float(r["mark_px"]), float(r["oracle_px"]))
    for coin in rows:
        marks[coin] = sim.Marks(rows[coin])
    present = {f["key"] for f in manifest["files"] if f["status"] == "present"}
    eligible, day_detail = set(), {}
    for day in days:
        d0 = ba._day_ms(day)
        detail = {}
        ok = True
        for coin in s["data_plan"]["coins"]:
            hours = sum(1 for h in range(24) if ba.book_key(day, h, coin) in present)
            nmarks = sum(1 for k in range(1440) if (d0 + k * sim.MINUTE_MS) in rows[coin])
            nfund = sum(1 for h in range(24) if (d0 + h * ba.HOUR_MS) in funding[coin])
            b = bars[coin]
            k0 = b.index(d0)
            complete = int(b.complete[k0:k0 + 1440].sum())
            detail[coin] = {"book_hours": hours, "mark_rows": nmarks, "funding_rows": nfund, "complete_bars": complete}
            ok = ok and hours == 24 and nmarks == 1440 and nfund == 24 and complete >= 0.99 * 1440
        day_detail[day] = {**detail, "eligible": ok}
        if ok:
            eligible.add(d0)
    return {"books": books, "bars": bars, "marks": marks, "funding": funding, "eligible_days": eligible,
            "day_detail": day_detail, "sz_decimals": sz_dec, "max_leverage": max_lev, "import_report": report,
            "manifest_sha256": ba.sha256_file(MANIFEST)}


def splits(s: dict) -> dict:
    out = {}
    for name in ("train", "validation", "held_out"):
        w = s["data_plan"]["splits"][name]
        out[name] = {"start_ms": _iso_ms(w["start"].replace("+00:00", "Z")), "end_ms": _iso_ms(w["end"])}
    out["blocks"] = [{"start_ms": _iso_ms(b["start"]), "end_ms": _iso_ms(b["end"])} for b in s["data_plan"]["splits"]["held_out_blocks"]]
    return out


def opportunity_coverage(market: dict, split: dict) -> dict:
    total = usable = e_total = e_usable = 0
    for coin, b in market["bars"].items():
        k0, k1 = b.index(split["start_ms"]), b.index(split["end_ms"])
        for k in range(k0, k1):
            m = b.first + k * 60_000
            ok = bool(b.complete[k]) and market["marks"][coin].at(m) is not None
            total += 1
            usable += ok
            if (m // ba.DAY_MS) * ba.DAY_MS in market["eligible_days"]:
                e_total += 1
                e_usable += ok
    return {"coin_minutes": total, "usable": usable, "share_all_minutes": usable / total if total else 0.0,
            "eligible_coin_minutes": e_total, "eligible_usable": e_usable,
            "share": e_usable / e_total if e_total else 0.0}


def variant_list(s: dict) -> list:
    import itertools
    grid = s["parameters"]["search"]
    keys = sorted(grid)
    out = []
    for combo in itertools.product(*(grid[k] for k in keys)):
        out.append(dict(zip(keys, combo)))
    return out


def pjson(p: dict) -> str:
    return json.dumps({k: p[k] for k in sorted(p)}, sort_keys=True, separators=(",", ":"))


def candidate_entries_for(market: dict, p: dict, cache: dict) -> list:
    import book_event_simulator as sim
    sys.path.insert(0, os.path.join(_ROOT, "shared_strategies", "open"))
    import order_book_imbalance as obi
    vp = obi.validate_params(p)
    out = []
    for coin, cb in market["books"].items():
        ck = (coin, vp["depth_levels"], vp["entry_threshold"], vp["max_spread_bps"], vp["min_combined_depth_usd"])
        if ck not in cache:
            cache[ck] = cb.classify(*ck[1:])
        for e in sim.candidate_entries(cb, cache[ck], vp["confirmation_observations"], vp["max_snapshot_age_ms"], obi.EVIDENCE_WINDOW_MS):
            _, entry = obi.episode_ids(coin, vp, e["boundary"], e["direction"], cb.identity[e["first"]], cb.identity[e["crossing"]], e["deadline_ms"])
            e["entry_id"] = entry
            out.append(e)
    return out


def comparator_entries(market: dict, s: dict) -> list:
    import pandas as pd
    sys.path.insert(0, os.path.join(_ROOT, "shared_strategies", "open"))
    from donchian_breakout import donchian_breakout_core
    params = s["comparator"]["params"]
    out = []
    for coin, b in market["bars"].items():
        df = pd.DataFrame({"open": b.open, "high": b.high, "low": b.low, "close": b.close})
        sig = donchian_breakout_core(df, **params)["signal"].to_numpy()
        for k in np.nonzero(sig)[0]:
            if not b.complete[k]:
                continue
            m = b.first + int(k) * 60_000
            out.append({"deadline_ms": m + 60_000, "coin": coin, "direction": int(sig[k]), "entry_id": f"{coin}:{m}"})
    return out


def sim_cfg(s: dict, processing_ms=None, slippage_bps=None) -> dict:
    lat = s["execution"]["latency"]
    cap = s["exit_and_risk"]["capital"]
    costs = s["execution"]["costs"]
    return {"settle_ms": lat["shared_feed_settle_ms"], "processing_ms": lat["processing_order_ms"] if processing_ms is None else processing_ms,
            "taker_fee_pct": costs["taker_fee_pct"], "slippage_bps": costs["slippage_bps"] if slippage_bps is None else slippage_bps,
            "margin_per_entry": cap["margin_per_entry"], "leverage": cap["leverage"], "initial_equity": cap["initial_shared_equity"],
            "min_notional_usd": costs["min_notional_usd"], "pre_entry_stop_pct": 1.0, "daily_loss_pct": 2.0, "drawdown_latch_pct": 10.0,
            "time_stop_bars": s["exit_and_risk"]["close_strategy"]["params"]["max_bars"], "purge_ms": 5 * 60_000,
            "exit_tail_ms": 30 * 60_000, "fill_within_ms": 10_000}


def summarize(res: dict) -> dict:
    keep = ("closed", "terminal", "net", "pre_fee", "fees", "funding", "turnover", "max_drawdown", "by_coin", "by_direction",
            "refusals", "held_gaps", "held_complete", "entries_offered")
    return {k: res[k] for k in keep}


def neighbors(p: dict, grid: dict) -> list:
    out = []
    for k in sorted(grid):
        vals = grid[k]
        i = vals.index(p[k])
        for j in (i - 1, i + 1):
            if 0 <= j < len(vals):
                out.append({**p, k: vals[j]})
    return out


def bootstrap_lower(daily_diff: list, seed: int, draws: int) -> float:
    rng = np.random.default_rng(seed)
    arr = np.array(daily_diff, dtype=np.float64)
    if not len(arr):
        return float("nan")
    idx = rng.integers(0, len(arr), size=(draws, len(arr)))
    totals = arr[idx].sum(axis=1)
    return float(np.percentile(totals, 5))


def parity_check(market: dict, p: dict, entries: list, limit: int = 400) -> dict:
    sys.path.insert(0, os.path.join(_ROOT, "shared_strategies", "open"))
    import order_book_imbalance as obi
    import book_event_simulator as sim
    from book_replay import CONTRACT as RC
    contract = {k: RC[k] for k in ("source", "contract", "identity_semantics", "max_future_skew_ms", "max_lag_ms", "max_gap_ms")}
    vp = obi.validate_params(p)
    checked = mismatches = 0
    first = []
    sample = entries[:: max(1, len(entries) // limit)][:limit]
    probes = [(e["coin"], e["deadline_ms"], e) for e in sample] + [(e["coin"], e["deadline_ms"] + 1000, None) for e in sample[:limit // 2]]
    emitted = {(e["coin"], e["deadline_ms"]): e for e in entries}
    rng = np.random.default_rng(1635)
    lo, hi = min(e["deadline_ms"] for e in entries) if entries else 0, max(e["deadline_ms"] for e in entries) if entries else 0
    coins = sorted(market["books"])
    for _ in range(limit if entries else 0):
        D = int(rng.integers(lo // 1000, hi // 1000 + 1)) * 1000
        coin = coins[int(rng.integers(0, len(coins)))]
        probes.append((coin, D, emitted.get((coin, D))))
    for coin, D, e in probes:
        cb = market["books"][coin]
        sec = sim.section_at(cb, D, vp["depth_levels"], obi.EVIDENCE_WINDOW_MS, ba.ARCHIVE_TIME_BASIS, contract)
        d = obi.evaluate_section(sec, coin, vp, D, accept_time_semantics=(ba.ARCHIVE_TIME_BASIS,))
        want_sig = e["direction"] if e else 0
        want_id = e["entry_id"] if e else ""
        checked += 1
        if d["signal"] != want_sig or d["entry_id"] != want_id:
            mismatches += 1
            if len(first) < 5:
                first.append({"coin": coin, "deadline_ms": D, "fast": [want_sig, want_id], "evaluator": [d["signal"], d["entry_id"], d["reason"]]})
    return {"checked": checked, "mismatches": mismatches, "first": first}


def run_study() -> dict:
    import book_event_simulator as sim
    s = spec()
    market = load_market(s)
    sp = splits(s)
    grid = s["parameters"]["search"]
    base = sim_cfg(s)
    cov = {k: opportunity_coverage(market, sp[k]) for k in ("train", "validation", "held_out")}
    cache = {}
    variants = variant_list(s)
    rows = []
    started = time.time()
    for n_done, p in enumerate(variants):
        ents = candidate_entries_for(market, p, cache)
        row = {"params": p, "params_json": pjson(p), "entries": len(ents)}
        for name in ("train", "validation", "held_out"):
            r = sim.simulate(ents, market, base, sp[name])
            row[name] = summarize(r)
            if name == "held_out":
                row["held_out_daily"] = {str(k): v for k, v in sorted(r["daily"].items())}
        rows.append(row)
        if n_done % 50 == 0:
            print(f"[run] {n_done + 1}/{len(variants)} variants in {time.time() - started:.0f} s", file=sys.stderr, flush=True)
    comp_entries = comparator_entries(market, s)
    comp = {}
    comp_daily = {}
    for name in ("train", "validation", "held_out"):
        r = sim.simulate(comp_entries, market, base, sp[name])
        comp[name] = summarize(r)
        if name == "held_out":
            comp_daily = r["daily"]
    gate = 0.99

    def complete(row, name):
        return cov[name]["share"] >= gate and row[name]["held_complete"]

    eligible_rows = [r for r in rows if complete(r, "train")]
    selected = None
    if eligible_rows:
        selected = sorted(eligible_rows, key=lambda r: (-r["train"]["net"], r["params_json"]))[0]
    commit = subprocess.run(["git", "-C", _ROOT, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    dirty = bool(subprocess.run(["git", "-C", _ROOT, "status", "--porcelain", "--untracked-files=no"], capture_output=True, text=True).stdout.strip())
    sources = {rel: ba.sha256_file(os.path.join(_ROOT, rel)) for rel in (
        "backtest/book_event_simulator.py", "backtest/book_archive.py", "backtest/book_replay.py", "backtest/lz4_frame.py",
        "shared_strategies/open/order_book_imbalance.py", "shared_tools/book_inputs.py",
        "backtest/candidates/order_book_imbalance_1635/run_study.py")}
    result = {"schema": "go_trader_obi_1635_results/v1", "spec_sha256": ba.sha256_file(SPEC_PATH),
              "source_commit": commit, "source_tree_dirty": dirty, "source_sha256": sources,
              "manifest_sha256": market["manifest_sha256"], "coverage": cov, "day_detail": market["day_detail"],
              "eligible_days": len(market["eligible_days"]), "import": market["import_report"]["coins"],
              "comparator": comp, "variants": rows, "selected": selected["params_json"] if selected else None}
    verdict = {"reasons": [], "checks": {}}
    if selected is None:
        verdict["outcome"] = "INCONCLUSIVE"
        verdict["reasons"].append("no variant is complete on the training split")
    else:
        p = selected["params"]
        ents = candidate_entries_for(market, p, cache)
        stress = {}
        for proc in s["execution"]["latency"]["processing_stresses_ms"]:
            stress[str(proc)] = summarize(sim.simulate(ents, market, sim_cfg(s, processing_ms=proc), sp["held_out"]))
        slip = summarize(sim.simulate(ents, market, sim_cfg(s, slippage_bps=s["execution"]["costs"]["slippage_stress_bps"]), sp["held_out"]))
        comp_stress = summarize(sim.simulate(comp_entries, market, sim_cfg(s, slippage_bps=s["execution"]["costs"]["slippage_stress_bps"]), sp["held_out"]))
        nb = []
        by_json = {r["params_json"]: r for r in rows}
        for q in neighbors(p, grid):
            r = by_json[pjson(q)]
            nb.append({"params": q, "held_out_net": r["held_out"]["net"]})
        held_days = sorted(d for d in market["eligible_days"] if sp["held_out"]["start_ms"] <= d < sp["held_out"]["end_ms"])
        all_days = list(range(sp["held_out"]["start_ms"], sp["held_out"]["end_ms"], ba.DAY_MS))
        cand_daily = {int(k): v for k, v in selected["held_out_daily"].items()}
        diff = [cand_daily.get(d, 0.0) - comp_daily.get(d, 0.0) for d in all_days]
        lower = bootstrap_lower(diff, s["verdict"]["bootstrap"]["seed"], s["verdict"]["bootstrap"]["draws"])
        blocks = []
        for b in sp["blocks"]:
            c = sum(v for d, v in cand_daily.items() if b["start_ms"] <= d < b["end_ms"])
            k = sum(v for d, v in comp_daily.items() if b["start_ms"] <= d < b["end_ms"])
            blocks.append({"start_ms": b["start_ms"], "candidate": c, "comparator": k, "improvement": c - k})
        parity = {"checked": 0, "mismatches": 0, "first": [], "variants": []}
        seed = {k: next(v for v in grid[k] if v == s["parameters"]["seed"][k]) for k in grid}
        probe_variants = [p, seed]
        probe_variants += [{k: grid[k][0] for k in grid}, {k: grid[k][-1] for k in grid}]
        for q in probe_variants:
            qe = candidate_entries_for(market, {k: q[k] for k in grid}, cache)
            one = parity_check(market, q, [e for e in qe if sp["train"]["start_ms"] <= e["deadline_ms"] < sp["held_out"]["end_ms"]])
            parity["checked"] += one["checked"]
            parity["mismatches"] += one["mismatches"]
            parity["first"] += one["first"][: 5 - len(parity["first"])]
            parity["variants"].append({"params": pjson(q), "checked": one["checked"], "mismatches": one["mismatches"]})
        ho = selected["held_out"]
        enough = {
            "eligible_days_at_least_30": len(market["eligible_days"]) >= s["eligibility"]["minimum_days"],
            "three_held_out_blocks": len([b for b in sp["blocks"] if any(b["start_ms"] <= d < b["end_ms"] for d in held_days)]) >= 3,
            "candidate_held_out_positions_at_least_100": ho["closed"] >= s["eligibility"]["minimum_heldout_positions_per_arm"],
            "comparator_held_out_positions_at_least_100": comp["held_out"]["closed"] >= s["eligibility"]["minimum_heldout_positions_per_arm"],
            "held_out_opportunity_coverage_99": cov["held_out"]["share"] >= gate,
            "candidate_held_coverage_complete": ho["held_complete"],
            "comparator_held_coverage_complete": comp["held_out"]["held_complete"],
        }
        passes = {
            "candidate_held_out_net_positive": ho["net"] > 0,
            "bootstrap_lower_bound_positive": lower > 0,
            "two_of_three_blocks_improve": sum(1 for b in blocks if b["improvement"] > 0) >= 2,
            "drawdown_no_worse_than_comparator": ho["max_drawdown"] <= comp["held_out"]["max_drawdown"],
            "positive_at_each_latency_stress": all(v["net"] > 0 for v in stress.values()),
            "majority_of_neighbors_positive": sum(1 for n in nb if n["held_out_net"] > 0) * 2 > len(nb),
        }
        verdict["checks"] = {"evidence": enough, "pass": passes, "parity": parity}
        verdict.update({"bootstrap_lower_bound": lower, "daily_difference": diff, "blocks": blocks, "latency_stress": stress,
                        "slippage_stress": {"candidate": slip, "comparator": comp_stress}, "neighbors": nb})
        if parity["mismatches"]:
            verdict["outcome"] = "INVALID"
            verdict["reasons"].append(f"{parity['mismatches']} of {parity['checked']} evaluator parity probes disagree with the study path")
        elif not all(enough.values()):
            verdict["outcome"] = "INCONCLUSIVE"
            verdict["reasons"] += [k for k, v in enough.items() if not v]
        elif all(passes.values()):
            verdict["outcome"] = "PASS"
        else:
            verdict["outcome"] = "FAIL"
            verdict["reasons"] += [k for k, v in passes.items() if not v]
    result["verdict"] = verdict
    with open(os.path.join(_HERE, "results.json"), "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True, default=float)
        fh.write("\n")
    return {"outcome": verdict["outcome"], "reasons": verdict["reasons"], "selected": result["selected"]}


def _ts(ms) -> str:
    return time.strftime("%Y-%m-%d %H:%M", time.gmtime(int(ms) / 1000)) + "Z"


def _f(v, nd=2) -> str:
    if v is None:
        return "-"
    if isinstance(v, float) and v != v:
        return "nan"
    return f"{v:,.{nd}f}"


def render() -> dict:
    with open(os.path.join(_HERE, "results.json")) as fh:
        r = json.load(fh)
    s = spec()
    v = r["verdict"]
    lines = ["# order_book_imbalance study (issue 1635, Delivery 2)", "",
             f"**Verdict: {v['outcome']}.** The candidate stays production-refused for every outcome.", ""]
    if v.get("reasons"):
        lines += ["Reasons: " + "; ".join(v["reasons"]) + ".", ""]
    w = s["data_plan"]["window"]
    lines += ["## Inputs", "",
              f"- Specification `candidate_spec.json`, sha256 `{r['spec_sha256']}` (frozen before download).",
              f"- Archive manifest `data/archive_manifest.json`, sha256 `{r['manifest_sha256']}`: Hyperliquid S3 archive books and asset contexts for {w['first_day']} to {w['last_day']} plus the lookback day.",
              f"- Eligible days: {r['eligible_days']} of {w['days']} (minimum {s['eligibility']['minimum_days']}).",
              "", "Opportunity coverage under both readings. The 99% gate uses eligible days only, a clarification committed before any PnL was computed (commit message: \"clarify that the 99% coverage gate counts eligible days only\").", "",
              "| Split | All coin-minutes | Usable | Share (all minutes) | Eligible-day coin-minutes | Usable | Share (gate) |", "|---|---|---|---|---|---|---|"]
    for k in ("train", "validation", "held_out"):
        c = r["coverage"][k]
        lines.append(f"| {k} | {c['coin_minutes']} | {c['usable']} | {c['share_all_minutes']:.4f} | {c['eligible_coin_minutes']} | {c['eligible_usable']} | {c['share']:.4f} |")
    lines += ["", "Ineligible days and their causes (an eligible day needs 24 book hours, 1440 mark rows and 24 funding rows per coin and at least 99% complete derived bars):", ""]
    for d, det in sorted(r["day_detail"].items()):
        if not det["eligible"]:
            causes = []
            for c in sorted(k for k in det if k != "eligible"):
                x = det[c]
                if x["book_hours"] != 24:
                    causes.append(f"{c} book hours {x['book_hours']}/24")
                if x["mark_rows"] != 1440:
                    causes.append(f"{c} mark rows {x['mark_rows']}/1440")
                if x["funding_rows"] != 24:
                    causes.append(f"{c} funding rows {x['funding_rows']}/24")
                if x["complete_bars"] < 0.99 * 1440:
                    causes.append(f"{c} complete bars {x['complete_bars']}/1440")
            lines.append(f"- {d}: " + "; ".join(causes))
    lines += ["", "Archive import (all imported hours):", "", "| Coin | Lines | Accepted | Rejected | Missing hours | Markers | Max accept gap ms |", "|---|---|---|---|---|---|---|"]
    for c, info in sorted(r["import"].items()):
        lines.append(f"| {c} | {info['counts']['lines']} | {info['stats']['accepted']} | {info['stats']['rejected']} | "
                     f"{info['counts']['missing_hours']} | {info['stats']['markers']} | {info['max_accept_gap_ms']} |")
    sel = next((x for x in r["variants"] if x["params_json"] == r["selected"]), None)
    lines += ["", "## Selected variant and comparator", ""]
    if sel is None:
        lines.append("No variant was complete on the training split.")
    else:
        lines += [f"Selected on train: `{sel['params_json']}`.", "",
                  "| Arm | Split | Closed | Terminal | Net | Pre-fee | Fees | Funding | Max DD | Turnover |", "|---|---|---|---|---|---|---|---|---|---|"]
        for arm, src in (("candidate", sel), ("donchian_breakout", r["comparator"])):
            for k in ("train", "validation", "held_out"):
                x = src[k]
                lines.append(f"| {arm} | {k} | {x['closed']} | {x['terminal']} | {_f(x['net'])} | {_f(x['pre_fee'])} | {_f(x['fees'])} | "
                             f"{_f(x['funding'])} | {_f(x['max_drawdown'])} | {_f(x['turnover'], 0)} |")
        if "blocks" in v:
            lines += ["", f"Bootstrap (seed {s['verdict']['bootstrap']['seed']}, {s['verdict']['bootstrap']['draws']} draws) one-sided 95% lower bound of the held-out paired improvement: {_f(v['bootstrap_lower_bound'])}.", "",
                      "| Held-out block start | Candidate | Comparator | Improvement |", "|---|---|---|---|"]
            for b in v["blocks"]:
                lines.append(f"| {_ts(b['start_ms'])} | {_f(b['candidate'])} | {_f(b['comparator'])} | {_f(b['improvement'])} |")
            lines += ["", "Latency stresses (held-out, processing ms): " + ", ".join(f"{k}: net {_f(x['net'])} ({x['closed']} closed)" for k, x in sorted(v["latency_stress"].items())) + ".",
                      f"Slippage stress {s['execution']['costs']['slippage_stress_bps']} bps (held-out): candidate net {_f(v['slippage_stress']['candidate']['net'])}, comparator net {_f(v['slippage_stress']['comparator']['net'])}.", "",
                      "One-axis neighbors (held-out net): " + "; ".join(f"`{pjson(n['params'])}` {_f(n['held_out_net'])}" for n in v["neighbors"]) + ".", ""]
            lines += ["| Check | Result |", "|---|---|"]
            for group in ("evidence", "pass"):
                for k, val in sorted(v["checks"][group].items()):
                    lines.append(f"| {group}: {k} | {'yes' if val else 'no'} |")
            par = v["checks"]["parity"]
            lines.append(f"| parity: evaluator probes | {par['checked']} checked, {par['mismatches']} mismatches |")
            lines += ["", "Refusal counts (held-out): candidate " + json.dumps(sel["held_out"]["refusals"], sort_keys=True) +
                      ", comparator " + json.dumps(r["comparator"]["held_out"]["refusals"], sort_keys=True) + "."]
    lines += ["", "## Every variant", "", "Net in quote units; closed positions in parentheses.", "",
              "| Parameters | Entries | Train | Validation | Held-out |", "|---|---|---|---|---|"]
    for x in sorted(r["variants"], key=lambda x: x["params_json"]):
        lines.append(f"| `{x['params_json']}` | {x['entries']} | {_f(x['train']['net'])} ({x['train']['closed']}) | "
                     f"{_f(x['validation']['net'])} ({x['validation']['closed']}) | {_f(x['held_out']['net'])} ({x['held_out']['closed']}) |")
    lines += ["", "## Reproduce", "", "```bash", "uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch",
              "uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py import",
              "uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py run",
              "uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py render", "```", "",
              "`fetch` downloads with `--request-payer requester` and rewrites `data/archive_manifest.json`; `fetch-funding` and `fetch-venue` refresh the pinned funding and venue files. Research-only, suggest-only; no live default, config or deployment changes.", ""]
    with open(os.path.join(_HERE, "REPORT.md"), "w") as fh:
        fh.write("\n".join(lines))
    return {"outcome": v["outcome"], "lines": len(lines)}


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Issue 1635 order-book imbalance archive study")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("fetch", help="Download the frozen window from the S3 archive (requester pays) and write the manifest")
    sub.add_parser("fetch-funding", help="Fetch and pin hourly funding for the frozen span")
    sub.add_parser("fetch-venue", help="Pin venue size decimals and margin tables")
    sub.add_parser("import", help="Import the archive books into the l2_book contract (writes ignored series and a committed report)")
    sub.add_parser("run", help="Score every variant and the comparator under the frozen rules and write results.json")
    sub.add_parser("render", help="Render REPORT.md from results.json")
    args = p.parse_args(argv)
    fn = {"fetch": fetch, "fetch-funding": fetch_funding, "fetch-venue": fetch_venue, "import": import_series,
          "run": run_study, "render": render}[args.cmd]
    print(json.dumps(fn(), indent=1, sort_keys=True, default=str))
    return 0


if __name__ == "__main__":
    sys.exit(main())
