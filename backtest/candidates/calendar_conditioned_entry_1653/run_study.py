#!/usr/bin/env python3

import argparse
import contextlib
import copy
import csv
import gzip
import hashlib
import itertools
import json
import math
import os
import statistics
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
VENUE_MANIFEST_PATH = os.path.abspath(os.path.join(HERE, "..", "calendar_conditioned_entry_1653_manifest.json"))
VENUE_SOURCE_MANIFEST = "backtest/candidates/dual_thrust_1642/venue_manifest.json"
RESULTS_PATH = os.path.join(HERE, "results.json")
PARITY_PATH = os.path.join(HERE, "parity_results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
EVIDENCE_DIR = os.path.join(HERE, "evidence")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
for _p in (os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
UNCONDITIONAL = "unconditional_entry_1653"
RANDOMIZED = "randomized_entry_1653"
SOURCE_FILES = (
    "shared_strategies/open/calendar_conditioned_entry.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/time_stop.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/closed_bar.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/data_fetcher.py",
    "shared_scripts/check_hyperliquid.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/calendar_conditioned_entry_1653/run_study.py",
    "backtest/candidates/calendar_conditioned_entry_1653/protocol.json",
    "backtest/candidates/calendar_conditioned_entry_1653/study_manifest.json",
    "backtest/candidates/calendar_conditioned_entry_1653_manifest.json",
)
PARITY_SINCE = "2025-10-01"
SYNTHETIC_SEED = 1653
SYNTHETIC_BARS = 5300
SYNTHETIC_START = "2025-01-06"
PARITY_COMPARE_COLUMNS = ("signal", "cce_valid", "cce_reason", "cce_refit_boundary_ms", "cce_gross_mean",
                          "cce_long_lower_bound", "cce_short_lower_bound", "cce_effective_obs", "cce_std_error",
                          "cce_next_cell")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_json(path: str):
    with open(path) as fh:
        return json.load(fh)


def write_json(path: str, obj) -> None:
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True, default=str)
        fh.write("\n")


def write_json_gz(path: str, obj) -> str:
    data = json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str).encode()
    with open(path, "wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as fh:
            fh.write(data)
    return sha256_bytes(data)


def read_gz_csv(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        rows = list(csv.reader(fh))
    return rows[1:]


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def protocol_sha256() -> str:
    return sha256_file(PROTOCOL_PATH)


def lock() -> None:
    sha = protocol_sha256()
    if os.path.exists(LOCK_PATH):
        held = load_json(LOCK_PATH)
        if held["protocol_sha256"] != sha:
            raise SystemExit(f"protocol.json changed after the lock ({held['protocol_sha256']} -> {sha}); "
                             "a frozen protocol cannot be amended after scoring starts")
        print(f"protocol already locked at {held['locked_at_utc']}: {sha}")
        return
    if os.path.exists(RESULTS_PATH) or os.path.exists(PARITY_PATH):
        raise SystemExit("results exist; the protocol must be locked before any score is written")
    write_json(LOCK_PATH, {
        "protocol_sha256": sha,
        "locked_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": git_head(),
        "note": "written by run_study.py lock before any window was scored; run and parity refuse a different "
                "protocol.json",
    })
    print(f"locked protocol.json {sha}")


def require_lock() -> dict:
    if not os.path.exists(LOCK_PATH):
        raise SystemExit("protocol_lock.json is missing; run `run_study.py lock` before scoring")
    held = load_json(LOCK_PATH)
    if held["protocol_sha256"] != protocol_sha256():
        raise SystemExit(f"protocol.json sha256 {protocol_sha256()} differs from the lock {held['protocol_sha256']}")
    return held


def check_protocol_against_code(proto: dict) -> dict:
    from calendar_conditioned_entry import DEFAULT_PARAMS, required_history_bars
    from optimizer import DEFAULT_PARAM_RANGES
    from registry_loader import load_registry
    params = proto["parameters"]
    reg = load_registry(proto["registry"])
    registry_defaults = dict(reg.STRATEGY_REGISTRY[proto["candidate"]]["default_params"])
    if params["proxy_1h_primary"] != registry_defaults or registry_defaults != DEFAULT_PARAMS:
        raise SystemExit("protocol proxy_1h_primary differs from the registry default parameters")
    grid = {k: v for k, v in params["proxy_1h_variant_grid"].items() if k != "expected_interval_ms"}
    if grid != DEFAULT_PARAM_RANGES[proto["candidate"]]:
        raise SystemExit("protocol proxy_1h_variant_grid differs from optimizer DEFAULT_PARAM_RANGES")
    proxy_need = max(required_history_bars(p) for p in variant_params(proto))
    venue_need = required_history_bars(params["venue_4h_primary"])
    if proxy_need != proto["warmup"]["proxy_1h_bars"] or venue_need != proto["warmup"]["venue_4h_bars"]:
        raise SystemExit(f"warmup bars disagree with required_history_bars: proxy {proxy_need}, venue {venue_need}")
    return {"proxy_1h_required_history_bars": proxy_need, "venue_4h_required_history_bars": venue_need}


def variant_params(proto: dict) -> list:
    grid = proto["parameters"]["proxy_1h_variant_grid"]
    keys = sorted(grid)
    return [dict(zip(keys, combo)) for combo in itertools.product(*(grid[k] for k in keys))]


def variant_label(p: dict) -> str:
    return f"T{p['training_bars']}_H{p['holding_bars']}"


def proxy_data_root(manifest: dict) -> str:
    return os.path.join(REPO, manifest["data_root"])


def verify_frozen(proto: dict) -> dict:
    manifest = load_json(MANIFEST_PATH)
    source_path = os.path.join(REPO, manifest["reused_from"]["manifest"])
    if sha256_file(source_path) != manifest["reused_from"]["sha256"]:
        raise SystemExit(f"{manifest['reused_from']['manifest']} sha256 differs from the pinned value")
    source = load_json(source_path)["files"]
    root = proxy_data_root(manifest)
    for name, meta in manifest["files"].items():
        got = sha256_file(os.path.join(root, name))
        if got != meta["sha256"] or source.get(name, {}).get("sha256") != meta["sha256"]:
            raise SystemExit(f"frozen proxy input {name} sha256 {got} != manifest {meta['sha256']}")
    import offline_manifest as om
    venue = om.load_manifest(VENUE_MANIFEST_PATH, verify=True)
    if venue["warmup_bars"] != proto["warmup"]["venue_4h_bars"]:
        raise SystemExit("venue manifest warmup_bars differs from the protocol")
    vsrc = load_json(os.path.join(REPO, VENUE_SOURCE_MANIFEST))
    want = {d["coin"]: (d["candles"]["sha256"], d["funding"]["sha256"]) for d in vsrc["datasets"]}
    for d in venue["datasets"]:
        if want.get(d["coin"]) != (d["candles"]["sha256"], d["funding"]["sha256"]):
            raise SystemExit(f"venue {d['coin']} inputs no longer match {VENUE_SOURCE_MANIFEST}")
    for key in ("meta", "l2_snapshot"):
        if vsrc["venue_reference"][key]["sha256"] != venue["venue_reference"][key]["sha256"]:
            raise SystemExit(f"venue {key} no longer matches {VENUE_SOURCE_MANIFEST}")
    copied = {"proxy": f"every proxy file equals {manifest['reused_from']['manifest']} (read in place)",
              "venue": f"every venue file equals {VENUE_SOURCE_MANIFEST} (read in place)"}
    return {"proxy": manifest, "venue": venue, "copied_from_check": copied}


def build_cache(cache_db: str, manifest: dict, aliases: bool = False) -> None:
    if os.path.exists(cache_db):
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; the run rebuilds it from the frozen files")
    os.environ[CACHE_ENV] = os.path.abspath(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    root = proxy_data_root(manifest)
    for name, meta in manifest["files"].items():
        rows = read_gz_csv(os.path.join(root, name))
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float,
                            "close": float, "volume": float})
            storage.store_ohlcv(df, meta["exchange"], meta["symbol"], meta["timeframe"], db_path=cache_db)
            if aliases:
                storage.store_ohlcv(df, meta["exchange"], meta["symbol"].split("/")[0], meta["timeframe"],
                                    db_path=cache_db)
        else:
            records = [{"time": int(t), "rate": float(r)} for t, r in rows]
            absent = [h for h, _ in meta["venue_absent_hours"]]
            observed = max((o for _, o in meta["venue_absent_hours"]), default=None)
            storage.store_funding_fetch(meta["exchange"], meta["coin"], records, absent,
                                        observed_through_ms=observed, db_path=cache_db)
            for s, e in meta["coverage"]:
                storage.store_funding_coverage(meta["exchange"], meta["coin"], s, e, db_path=cache_db)


def forbid_network() -> None:
    import data_fetcher
    import funding_fetcher

    def refuse(*_a, **_k):
        raise RuntimeError("frozen study run: network acquisition is disabled")

    data_fetcher.fetch_full_history = refuse
    data_fetcher.fetch_ohlcv = refuse
    funding_fetcher._hl_adapter = refuse


def pooled_entry(df, **params):
    import numpy as np
    from statistics import NormalDist
    import calendar_conditioned_entry as cce
    p = cce.validate_calendar_params({k: v for k, v in params.items() if k in cce.DEFAULT_PARAMS})
    out = cce.calendar_conditioned_entry_core(df, **p)
    valid = out["cce_valid"].to_numpy() == 1.0
    signal = np.zeros(len(out), dtype=np.int64)
    reason = np.full(len(out), "", dtype=object)
    reason[~valid] = out["cce_reason"].to_numpy()[~valid]
    rows = np.flatnonzero(valid)
    mean = np.full(len(out), np.nan)
    lower = np.full(len(out), np.nan)
    if len(rows):
        frame, _ = cce._prepare(out, p)
        frame.cell = np.zeros_like(frame.cell)
        alpha = (1.0 - p["confidence"]) / 2.0
        z = NormalDist().inv_cdf(1.0 - alpha)
        lag = cce.newey_west_lag(p["holding_bars"], p["expected_interval_ms"])
        interval = p["expected_interval_ms"]
        boundary = out["cce_refit_boundary_ms"].to_numpy()[rows].astype(np.int64)
        d = (frame.open_ms[rows] + interval - boundary) // interval
        refit = rows - d
        tables = {int(r): cce._fit_table(frame, int(r), p, z, lag) for r in np.unique(refit)}
        for row, r in zip(rows, refit):
            t = tables[int(r)]
            signal[row] = int(t["decision"][0])
            reason[row] = cce.CELL_REASONS[int(t["reason"][0])]
            mean[row] = t["mean"][0]
            lower[row] = max(t["long_lb"][0], t["short_lb"][0])
    out["pooled_reason"] = reason
    out["pooled_gross_mean"] = mean
    out["pooled_best_lower_bound"] = lower
    out["signal"] = signal
    return out


RANDOMIZED_SEED_DEPARTURE = (
    "Departure from the frozen protocol: the randomized control that produced every published randomized number "
    "seeds numpy default_rng([1653, seed, refit_boundary_ms // 1000, salt]), where salt = round(open of the "
    "block's first valid decision row x 1e6) mod 2^32 (`randomized_entry` in `run_study.py`). The locked "
    "`protocol.json` text above omits the salt; the protocol file is left unchanged, so its lock still holds. "
    "With the salt, blocks of different markets that share a refit boundary and a row count get different "
    "permutations. The verdict does not depend on it: both groups stop at the independent-cluster sample floor "
    "before any randomized comparison can decide."
)


def randomized_entry(df, random_seed: int = 0, **params):
    import numpy as np
    import calendar_conditioned_entry as cce
    p = cce.validate_calendar_params({k: v for k, v in params.items() if k in cce.DEFAULT_PARAMS})
    out = cce.calendar_conditioned_entry_core(df, **p)
    cand = out["signal"].to_numpy().astype(np.int64)
    valid = out["cce_valid"].to_numpy() == 1.0
    blocks = out["cce_refit_boundary_ms"].to_numpy()
    opens = out["open"].to_numpy(dtype="float64")
    signal = np.zeros(len(out), dtype=np.int64)
    for boundary in np.unique(blocks[valid]):
        rows = np.flatnonzero(valid & (blocks == boundary))
        n_long = int((cand[rows] == 1).sum())
        n_short = int((cand[rows] == -1).sum())
        if n_long + n_short == 0:
            continue
        salt = int(round(float(opens[rows[0]]) * 1e6)) % (2 ** 32)
        rng = np.random.default_rng([1653, int(random_seed), int(boundary) // 1000, salt])
        order = rows[rng.permutation(len(rows))]
        signal[order[:n_long]] = 1
        signal[order[n_long:n_long + n_short]] = -1
    out["random_seed"] = float(random_seed)
    out["candidate_signal"] = cand
    out["signal"] = signal
    return out


@contextlib.contextmanager
def installed_baselines(reg):
    from calendar_conditioned_entry import DEFAULT_PARAMS
    for name in (UNCONDITIONAL, RANDOMIZED):
        if name in reg.STRATEGY_REGISTRY:
            raise SystemExit(f"{name} is already present in the research registry")
    reg.STRATEGY_REGISTRY[UNCONDITIONAL] = {
        "fn": pooled_entry, "description": "study-local pooled (unconditional) entry baseline (#1653)",
        "default_params": dict(DEFAULT_PARAMS), "constraints": (), "edge_status": None, "edge_source": None,
        "edge_ref": None, "param_validator": None}
    reg.STRATEGY_REGISTRY[RANDOMIZED] = {
        "fn": randomized_entry, "description": "study-local time-matched randomized entry baseline (#1653)",
        "default_params": {**DEFAULT_PARAMS, "random_seed": 0}, "constraints": (), "edge_status": None,
        "edge_source": None, "edge_ref": None, "param_validator": None}
    try:
        yield
    finally:
        reg.STRATEGY_REGISTRY.pop(UNCONDITIONAL, None)
        reg.STRATEGY_REGISTRY.pop(RANDOMIZED, None)


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


def corrected_lower_bound(proto: dict, positions: list, clusters: int):
    from statistics import NormalDist
    rets = [p["net_pnl"] / p["entry_notional"] for p in positions if p["entry_notional"] > 0]
    corr = proto["economic_correction"]
    z = NormalDist().inv_cdf(1.0 - corr["alpha"] / corr["family"])
    if len(rets) < 2 or clusters < 2:
        return {"n_positions": len(rets), "clusters": clusters, "mean": statistics.mean(rets) if rets else None,
                "sd": None, "z": z, "lower_bound": None}
    mean = statistics.mean(rets)
    sd = statistics.stdev(rets)
    return {"n_positions": len(rets), "clusters": clusters, "mean": mean, "sd": sd, "z": z,
            "lower_bound": mean - z * sd / math.sqrt(clusters)}


def funding_brief(leg: dict) -> dict:
    block = leg.get("funding") or {}
    if not block and leg.get("manifest"):
        cov = leg["manifest"]["funding_coverage"]
        return {"mode": "manifest attachment", "source": "manifest", "complete": cov.get("complete"),
                "sha256": leg["manifest"].get("funding_sha256"), "expected_hours": cov.get("expected_hours"),
                "present_hours": cov.get("present_hours"), "missing_hours": cov.get("missing_hours"),
                "venue_absent_hours": None, "unpriced_held_hours": None, "fetch_error": None}
    cov = block.get("coverage") or {}
    return {"mode": block.get("mode"), "source": block.get("source"), "complete": block.get("complete"),
            "sha256": block.get("sha256"), "expected_hours": cov.get("expected_hours"),
            "present_hours": cov.get("present_hours"), "missing_hours": cov.get("missing_hours"),
            "venue_absent_hours": cov.get("venue_absent_hours"),
            "unpriced_held_hours": block.get("unpriced_held_hours"),
            "fetch_error": block.get("fetch_error") or block.get("error")}


def calendar_shares(positions: list) -> dict:
    import pandas as pd
    if not positions:
        return {"top_cell": None, "top_cell_share": None, "top_weekday": None, "top_weekday_share": None,
                "distinct_cells": 0}
    cells, days = {}, {}
    for p in positions:
        ts = pd.Timestamp(p["entry_date"])
        cell = f"{ts.dayofweek}-{ts.hour:02d}"
        cells[cell] = cells.get(cell, 0) + 1
        days[ts.dayofweek] = days.get(ts.dayofweek, 0) + 1
    top_cell = max(sorted(cells), key=lambda k: cells[k])
    top_day = max(sorted(days), key=lambda k: days[k])
    return {"top_cell": top_cell, "top_cell_share": round(cells[top_cell] / len(positions), 4),
            "top_weekday": int(top_day), "top_weekday_share": round(days[top_day] / len(positions), 4),
            "distinct_cells": len(cells)}


def summarize(proto: dict, score: dict, venue: bool) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    bars = score.get("bars") or {}
    out = {"datasets": len(rows), "m1_verdict": score.get("verdict"), "m1_mean_sharpe": score.get("mean_sharpe"),
           "m1_bar_sharpe": score.get("mean_bar_sharpe"), "m1_mean_ddadj": score.get("mean_ddadj"),
           "m1_bar_ddadj": score.get("mean_bar_ddadj"),
           "incumbent_funding_incomplete": sum(1 for b in bars.values() if b and b.get("funding_incomplete")),
           "incumbent_missing": sum(1 for b in bars.values() if b is None)}
    if not rows:
        out.update({"positions": 0, "clusters": 0, "funding_incomplete_legs": 0, "liquidated_legs": 0,
                    "funding_fetched_legs": 0, "coverage_complete": False if venue else None,
                    "net_pnl_usd": 0.0, "mean_sharpe": None, "mean_ddadj": None, "mean_return_pct": None,
                    "per_dataset": {}, "positions_list": [], "close_validation_codes": [],
                    "lower_bound": corrected_lower_bound(proto, [], 0)})
        return out
    legs = [r["leg"] for r in rows]
    positions, per_dataset, codes = [], {}, set()
    for r in rows:
        leg, ex = r["leg"], r["leg"]["execution"]
        plist = ex["position_list"]
        validation = leg.get("close_validation") or {}
        for rec in validation.get("approximations") or []:
            codes.add(rec.get("reason_code"))
        item = {
            "return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"], "trades": leg["trades"], "bh_return_pct": leg.get("bh_return_pct"),
            "net_pnl_usd": ex["net_pnl_usd"], "fees_usd": ex["fees_usd"], "funding_pnl_usd": ex["funding_pnl_usd"],
            "turnover": ex["turnover"], "traded_notional_usd": ex["traded_notional_usd"],
            "positions": ex["positions"], "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"], "bars_in_market": ex["bars_in_market"],
            "liquidated": bool(leg.get("liquidated")), "funding_incomplete": bool(leg.get("funding_incomplete")),
            "funding": funding_brief(leg), "beats_incumbent_sharpe": r["beats_sharpe"],
            "beats_incumbent_ddadj": r["beats_ddadj"], "warmup": leg.get("warmup"),
            "close_validation_mode": validation.get("mode"),
            "incomplete_close_parity": validation.get("incomplete_parity"),
            "exit_reasons": sorted({x.split(":")[0] for p in plist for x in p.get("exit_reasons") or [] if x}),
            "long_net_pnl_usd": round(sum(p["net_pnl"] for p in plist if p["side"] == "long"), 6),
            "short_net_pnl_usd": round(sum(p["net_pnl"] for p in plist if p["side"] == "short"), 6),
        }
        if venue:
            item["candle_coverage"] = leg["manifest"]["candle_coverage"]
            item["funding_coverage"] = leg["manifest"]["funding_coverage"]
            item["rejected_entries"] = ex.get("rejected_entries", 0)
            item["execution_spec"] = leg["manifest"]["execution_spec"]
        per_dataset[r["dataset"]] = item
        for p in plist:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    clusters = position_clusters(positions)
    out.update({
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "long_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "long"), 4),
        "short_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "short"), 4),
        "clusters": clusters,
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in per_dataset.values() if v["funding"]["source"] == "fetched"),
        "funding_unpriced_held_hours": sum(int(v["funding"]["unpriced_held_hours"] or 0)
                                           for v in per_dataset.values()),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
        "top5_position_abs_pnl_share": round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None,
        "calendar_concentration": calendar_shares(positions),
        "coverage_complete": (all(v["candle_coverage"]["complete"] and v["funding_coverage"]["complete"]
                                  for v in per_dataset.values()) if venue else None),
        "rejected_entries": sum(int(v.get("rejected_entries") or 0) for v in per_dataset.values()) if venue else None,
        "close_validation_codes": sorted(c for c in codes if c),
        "lower_bound": corrected_lower_bound(proto, positions, clusters),
        "per_dataset": per_dataset,
        "positions_list": positions,
    })
    return out


def brief(summary: dict) -> dict:
    return {k: v for k, v in summary.items() if k not in ("per_dataset", "positions_list")}


def contract_for(proto: dict, holding: int) -> dict:
    c = proto["contract"]
    return {"direction": c["direction"],
            "close_strategies": [{"name": "time_stop", "params": {"max_bars": int(holding)}}],
            "stop_loss_atr_mult": c["stop_loss_atr_mult"], "comparison_mode": c["comparison_mode"]}


class Study:
    def __init__(self, proto: dict, proxy_manifest: dict, venue_manifest: dict):
        import eval_windows as ew
        from registry_loader import load_registry
        self.ew = ew
        self.proto = proto
        self.reg = load_registry(proto["registry"])
        self.capital = proto["capital_usd"]
        self.cand = proto["candidate"]
        self.windows = {k: tuple(v) for k, v in proto["proxy"]["windows"].items()}
        self.symbols = proto["proxy"]["symbols"]
        self.tf = proto["proxy"]["timeframe"]
        self.warmup = int(proto["warmup"]["proxy_1h_bars"])
        self.memo = {}
        self.proxy_manifest = proxy_manifest
        self.venue_manifest = venue_manifest
        self.venue_datasets = ew.manifest_datasets(venue_manifest)
        self.venue_memo = {}

    def spec(self, name: str, params: dict) -> dict:
        spec = {"name": name, "type": self.proto["strategy_type"], "params": dict(params)}
        spec.update(copy.deepcopy(contract_for(self.proto, params["holding_bars"])))
        return spec

    def incumbents(self, wname: str, holding: int):
        ew = self.ew
        key = (wname, holding)
        if key not in self.memo:
            datasets = [(s, self.tf) for s in self.symbols]
            try:
                self.memo[key] = ew.incumbent_bars(ew.compute_incumbent_legs(
                    self.reg, datasets, self.windows[wname], self.capital,
                    funding_mode=self.proto["proxy"]["funding_mode"],
                    contract=contract_for(self.proto, holding), warmup_bars=self.warmup))
            except ValueError as exc:
                self.memo[key] = {"refused": str(exc)}
        return self.memo[key]

    def proxy(self, spec: dict, wname: str, with_incumbents: bool) -> dict:
        ew = self.ew
        window = self.windows[wname]
        ew.validate_candidate(spec)
        bars = self.incumbents(wname, spec["params"]["holding_bars"]) if with_incumbents else {}
        if "refused" in bars:
            return {"refused": bars["refused"], "datasets": 0}
        legs = {}
        try:
            for symbol in self.symbols:
                legs[ew.dataset_key(symbol, self.tf)] = ew.run_candidate_leg(
                    self.reg, spec, symbol, self.tf, window, self.capital, keep_trades=True,
                    funding_mode=self.proto["proxy"]["funding_mode"], warmup_bars=self.warmup)
        except ValueError as exc:
            return {"refused": str(exc), "datasets": 0}
        score = ew.score_candidate(legs, bars) if with_incumbents else {
            "rows": [{"dataset": k, "leg": v, "bar": None, "beats_sharpe": None, "beats_ddadj": None}
                     for k, v in legs.items()], "verdict": None}
        score["bars"] = bars
        out = summarize(self.proto, score, venue=False)
        out["warmup_bars"] = self.warmup
        return out

    def venue(self, spec: dict, cost: float) -> dict:
        score = self.ew.evaluate_window(self.reg, spec, self.venue_datasets, self.proto["venue"]["held_out_window"],
                                        self.capital, self.venue_memo, manifest=self.venue_manifest,
                                        cost_multiplier=cost,
                                        incumbent_contract=contract_for(self.proto, spec["params"]["holding_bars"]))
        out = summarize(self.proto, score, venue=True)
        out["warmup_bars"] = self.venue_manifest["warmup_bars"]
        return out


def randomized_summary(arms: dict) -> dict:
    nets = {k: v["net_pnl_usd"] for k, v in arms.items() if k.startswith(RANDOMIZED)}
    pos = {k: v["positions"] for k, v in arms.items() if k.startswith(RANDOMIZED)}
    if not nets:
        return {}
    vals = sorted(nets.values())
    return {"seeds": len(vals), "max_net_pnl_usd": vals[-1], "median_net_pnl_usd": statistics.median(vals),
            "min_net_pnl_usd": vals[0], "positions_min": min(pos.values()), "positions_max": max(pos.values()),
            "per_seed_net_pnl_usd": dict(sorted(nets.items())), "per_seed_positions": dict(sorted(pos.items()))}


def window_unscorable(arms: dict) -> list:
    bad = []
    for arm, s in sorted(arms.items()):
        if s.get("refused"):
            bad.append(f"{arm}: refused ({s['refused']})")
            continue
        if s["datasets"] != 3:
            bad.append(f"{arm}: {s['datasets']} datasets scored")
        if s["funding_incomplete_legs"]:
            bad.append(f"{arm}: {s['funding_incomplete_legs']} funding_incomplete legs")
        if s.get("funding_fetched_legs"):
            bad.append(f"{arm}: funding fetched while scoring")
        if s.get("incumbent_funding_incomplete"):
            bad.append(f"{arm}: {s['incumbent_funding_incomplete']} incumbent datasets funding_incomplete")
        if s.get("incumbent_missing"):
            bad.append(f"{arm}: {s['incumbent_missing']} incumbent datasets missing")
    return bad


def _beats_random(cand: dict, arms: dict):
    rand = [v["net_pnl_usd"] for k, v in arms.items() if k.startswith(RANDOMIZED)]
    if not rand:
        return None
    return (cand.get("net_pnl_usd") or 0.0) > max(rand)


def _lb_positive(cand: dict):
    lb = (cand.get("lower_bound") or {}).get("lower_bound")
    return None if lb is None else lb > 0


def proxy_verdict(proto: dict, held: dict) -> dict:
    rules = proto["sample_rules"]
    primary = proto["proxy"]["primary_window"]
    secondary = proto["proxy"]["secondary_windows"]
    unscorable = {w: window_unscorable(arms) for w, arms in held.items()}
    scorable = {w: not v for w, v in unscorable.items()}
    oos = held[primary]
    cand = oos["candidate"]
    known = scorable[primary]
    sec_known = [w for w in secondary if scorable[w]]
    sec_pass = [w for w in sec_known if held[w]["candidate"]["m1_verdict"] == "pass"]

    def tri(value):
        return value if known else None

    if len(sec_pass) < len(sec_known):
        secondary_check = False
    elif len(sec_known) == len(secondary):
        secondary_check = True
    else:
        secondary_check = None
    lb = _lb_positive(cand)
    checks = {
        "m1_oos_pass": tri(cand.get("m1_verdict") == "pass"),
        "m1_secondary_passes": secondary_check,
        "oos_net_positive": tri((cand.get("net_pnl_usd") or 0.0) > 0),
        "oos_corrected_lower_bound_positive": tri(bool(lb)),
        "oos_beats_unconditional": tri((cand.get("net_pnl_usd") or 0.0) > (oos[UNCONDITIONAL].get("net_pnl_usd") or 0.0)),
        "oos_beats_randomized": tri(_beats_random(cand, oos)),
    }
    false = sorted(k for k, v in checks.items() if v is False)
    unknown = sorted(k for k, v in checks.items() if v is None)
    if not known:
        outcome, reason = "inconclusive", "oos is unscorable: " + "; ".join(unscorable[primary])
    elif cand["clusters"] < rules["min_held_out_clusters"]:
        outcome, reason = "inconclusive", (f"oos independent position clusters {cand['clusters']} < "
                                           f"{rules['min_held_out_clusters']}")
    elif false:
        outcome, reason = "fail", "false: " + ", ".join(false)
    elif not unknown:
        outcome, reason = "pass", "every frozen proxy check is true"
    else:
        outcome, reason = "inconclusive", "unknown: " + ", ".join(unknown)
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "unscorable_windows": {w: v for w, v in unscorable.items() if v}, "oos_clusters": cand.get("clusters")}


def venue_verdict(proto: dict, held: dict, provenance_kind: str) -> dict:
    rules = proto["sample_rules"]
    base = {k: v["base"] for k, v in held.items()}
    stress = {k: v["stress"] for k, v in held.items()}
    evidence = (provenance_kind == "venue"
                and all(s["datasets"] == 3 and s["coverage_complete"] and s["funding_incomplete_legs"] == 0
                        and not s.get("incumbent_funding_incomplete") and not s.get("incumbent_missing")
                        for arm in held.values() for s in arm.values()))
    cand = base["candidate"]
    lb = _lb_positive(cand)
    checks = {
        "net_positive_x1": (cand.get("net_pnl_usd") or 0.0) > 0,
        "net_positive_x2": (stress["candidate"].get("net_pnl_usd") or 0.0) > 0,
        "m1_test_pass_x1": cand["m1_verdict"] == "pass",
        "corrected_lower_bound_positive_x1": bool(lb),
        "beats_unconditional_x1": (cand.get("net_pnl_usd") or 0.0) > (base[UNCONDITIONAL].get("net_pnl_usd") or 0.0),
        "beats_randomized_x1": bool(_beats_random(cand, base)),
    }
    false = sorted(k for k, v in checks.items() if not v)
    if not evidence:
        outcome, reason = "inconclusive", "venue provenance, candle, funding or incumbent evidence is incomplete"
    elif cand["clusters"] < rules["min_held_out_clusters"]:
        outcome, reason = "inconclusive", (f"test independent position clusters {cand['clusters']} < "
                                           f"{rules['min_held_out_clusters']}")
    elif false:
        outcome, reason = "fail", "false: " + ", ".join(false)
    else:
        outcome, reason = "pass", "every frozen venue check is true"
    return {"outcome": outcome, "reason": reason, "checks": checks, "evidence_complete": evidence,
            "test_clusters": cand["clusters"]}


def overall_verdict(groups: dict) -> dict:
    outs = {k: v["outcome"] for k, v in groups.items()}
    vals = set(outs.values())
    if vals == {"pass"}:
        outcome = "pass"
    elif "fail" in vals:
        outcome = "fail"
    else:
        outcome = "inconclusive"
    return {"outcome": outcome, "groups": outs, "edge_source": EDGE_SOURCE_BY_OUTCOME[outcome],
            "availability": "edge_status stays no_edge in every outcome: hidden from discovery, explicit paper mode "
                            "runs it, live use still needs allow_no_edge: true; no result approves deployment"}


def verdict_guards(proto: dict, proxy_held: dict, venue_held: dict) -> dict:
    out = {}
    floor = proto["sample_rules"]["min_held_out_clusters"]
    primary = proto["proxy"]["primary_window"]
    if proxy_held:
        g = copy.deepcopy(proxy_held)
        g[primary]["candidate"]["funding_incomplete_legs"] = 1
        out["proxy_oos_funding_incomplete"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g[primary][UNCONDITIONAL]["datasets"] = 2
        out["proxy_oos_missing_baseline_dataset"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g[primary]["candidate"]["clusters"] = floor - 1
        out["proxy_below_sample_floor"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g[primary]["candidate"]["refused"] = "guard: prefix missing"
        out["proxy_oos_refused_prefix"] = proxy_verdict(proto, g)["outcome"]
    if venue_held:
        g = copy.deepcopy(venue_held)
        g["candidate"]["stress"]["funding_incomplete_legs"] = 1
        out["venue_funding_incomplete"] = venue_verdict(proto, g, "venue")["outcome"]
        g = copy.deepcopy(venue_held)
        g[UNCONDITIONAL]["base"]["coverage_complete"] = False
        out["venue_coverage_incomplete"] = venue_verdict(proto, g, "venue")["outcome"]
        out["venue_proxy_provenance"] = venue_verdict(proto, venue_held, "proxy")["outcome"]
        g = copy.deepcopy(venue_held)
        g["candidate"]["base"]["clusters"] = floor - 1
        out["venue_below_sample_floor"] = venue_verdict(proto, g, "venue")["outcome"]
    if any(v == "pass" for v in out.values()):
        raise SystemExit(f"verdict guard failed: an incomplete or undersized group passed: {out}")
    return out


def frame_sha256(frame) -> str:
    body = frame[["open", "high", "low", "close", "volume"]].copy()
    body.insert(0, "ts", [str(t) for t in frame.index])
    return sha256_bytes(body.to_csv(index=False).encode())


def proxy_frame(symbol: str, tf: str, window: tuple, warmup: int = 0):
    import pandas as pd
    from data_fetcher import load_cached_data
    from closed_bar import FIXED_DURATION_MS
    start, end = window
    load_start = pd.Timestamp(start) - pd.Timedelta(milliseconds=FIXED_DURATION_MS[tf] * warmup)
    df = load_cached_data(symbol, tf, start_date=str(load_start), end_date=end)
    return df[df.index < pd.Timestamp(end)]


def independent_cell(frame, rec, params: dict) -> dict:
    import numpy as np
    from calendar_conditioned_entry import EPOCH_MS, WEEK_MS
    interval = params["expected_interval_ms"]
    H = params["holding_bars"]
    ms = np.asarray([int(t.value // 1_000_000) for t in frame.index], dtype=np.int64)
    first, last = int(rec["cce_training_first_open_ms"]), int(rec["cce_refit_bar_open_ms"])
    pos = {int(t): i for i, t in enumerate(ms)}
    want_cell = int(rec["cce_next_cell"])
    clusters = {}
    for t in range(first, last - H * interval + 1, interval):
        e = pos[t]
        x = pos[t + H * interval]
        rel = t - EPOCH_MS
        cell = ((rel // 86_400_000) % 7) * 24 + (rel % 86_400_000) // 3_600_000
        if cell != want_cell:
            continue
        label = float(frame["open"].iloc[x]) / float(frame["open"].iloc[e]) - 1.0
        clusters.setdefault(rel // WEEK_MS, []).append(label)
    means = [sum(v) / len(v) for _, v in sorted(clusters.items())]
    return {"weekly_obs": len(means), "raw_obs": sum(len(v) for v in clusters.values()),
            "gross_mean": sum(means) / len(means) if means else None,
            "last_label_exit_open_ms": last}


def entry_audit(reg, cand: str, frames: dict, params: dict, positions: list) -> list:
    import pandas as pd
    outs = {k: reg.apply_strategy(cand, f, dict(params)) for k, f in frames.items()}
    rows = []
    interval = params["expected_interval_ms"]
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]], outs[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig = fill - 1
        rec = out.iloc[sig]
        want = 1 if p["side"] == "long" else -1
        if int(rec["signal"]) != want or rec["cce_valid"] != 1.0:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} has no valid {p['side']} signal on the "
                             "prior bar")
        ind = independent_cell(frame, rec, params)
        if ind["weekly_obs"] != int(rec["cce_weekly_obs"]) or ind["raw_obs"] != int(rec["cce_raw_obs"]):
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} cell sample disagrees")
        if abs(ind["gross_mean"] - float(rec["cce_gross_mean"])) > 1e-12:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} cell mean disagrees with the "
                             "independent recomputation")
        if int(rec["cce_refit_bar_open_ms"]) > int(rec["cce_eval_ts_ms"]):
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} table is newer than its decision")
        reasons = sorted({r.split(":")[0] for r in p.get("exit_reasons") or [] if r})
        held_ms = (pd.Timestamp(p["exit_date"]) - pd.Timestamp(p["entry_date"])) / pd.Timedelta(milliseconds=1)
        horizon_ok = reasons != ["time_stop"] or int(held_ms) == params["holding_bars"] * interval
        if not horizon_ok:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} time_stop exit is off the nominal horizon")
        span = frame.iloc[max(0, sig - 2):sig + 1]
        rows.append({
            "dataset": p["dataset"], "side": p["side"], "signal_bar": str(out.index[sig]),
            "fill_bar": str(out.index[fill]), "exit_bar": p["exit_date"], "exit_reasons": reasons,
            "net_pnl_usd": p["net_pnl"], "entry_notional_usd": round(p["entry_notional"], 6),
            "reason": rec["cce_reason"], "next_weekday": int(rec["cce_next_weekday"]),
            "next_hour": int(rec["cce_next_hour"]), "refit_boundary_ms": int(rec["cce_refit_boundary_ms"]),
            "training_first_open_ms": int(rec["cce_training_first_open_ms"]),
            "gross_mean": float(rec["cce_gross_mean"]), "long_lower_bound": float(rec["cce_long_lower_bound"]),
            "short_lower_bound": float(rec["cce_short_lower_bound"]), "weekly_obs": int(rec["cce_weekly_obs"]),
            "effective_obs": float(rec["cce_effective_obs"]), "std_error": float(rec["cce_std_error"]),
            "corrected_z": float(rec["cce_corrected_z"]), "independent_mean_match": True,
            "nominal_horizon_ok": horizon_ok, "input_sha256": frame_sha256(span)})
    return rows


def hold_inventory(reg, cand: str, frames: dict, params: dict, starts: dict) -> dict:
    import pandas as pd
    out = {}
    for key, frame in sorted(frames.items()):
        res = reg.apply_strategy(cand, frame, dict(params))
        start = pd.Timestamp(starts[key])
        if res.index.tz is not None and start.tzinfo is None:
            start = start.tz_localize(res.index.tz)
        scored = res[res.index >= start]
        counts = scored["cce_reason"].value_counts().to_dict()
        valid = scored["cce_valid"].to_numpy() == 1.0
        first_valid = scored.index[valid].min() if valid.any() else None
        out[key] = {"bars": int(len(scored)), "reasons": {k: int(v) for k, v in sorted(counts.items())},
                    "first_valid_bar": str(first_valid) if first_valid is not None else None,
                    "valid_bars": int(valid.sum()), "loaded_bars": int(len(frame)),
                    "frame_sha256": frame_sha256(frame), "first_loaded_bar": str(frame.index[0]),
                    "last_bar": str(frame.index[-1])}
    return out


def refit_evidence(frames: dict, params: dict, starts: dict, path: str) -> dict:
    import pandas as pd
    from calendar_conditioned_entry import calendar_refit_tables
    full, summary = {}, {}
    for key, frame in sorted(frames.items()):
        start_ms = int(pd.Timestamp(starts[key]).value // 1_000_000)
        tables = calendar_refit_tables(frame, **params)
        keep = [t for t in tables if t["refit_boundary_ms"] >= start_ms - params["refit_bars"] * params["expected_interval_ms"]]
        full[key] = keep
        summary[key] = [{k: v for k, v in t.items() if k != "cells"} for t in keep]
    digest = write_json_gz(path, full)
    return {"file": os.path.relpath(path, HERE), "sha256_uncompressed": digest, "tables": summary}


def run_proxy_group(study: Study) -> dict:
    proto = study.proto
    primary = proto["proxy"]["primary_window"]
    scored_windows = [primary] + proto["proxy"]["secondary_windows"] + proto["proxy"]["refused_windows"]
    windows = scored_windows + proto["proxy"]["diagnostic_windows"]
    params = dict(proto["parameters"]["proxy_1h_primary"])
    seeds = int(proto["baselines"]["randomized"]["seeds"])
    held, full = {}, {}
    for w in windows:
        arms = {"candidate": study.spec(study.cand, params), UNCONDITIONAL: study.spec(UNCONDITIONAL, params)}
        if w == primary:
            for s in range(seeds):
                arms[f"{RANDOMIZED}_s{s:02d}"] = study.spec(RANDOMIZED, {**params, "random_seed": s})
        full[w] = {}
        for k, a in arms.items():
            full[w][k] = study.proxy(a, w, with_incumbents=True)
        held[w] = {k: brief(s) for k, s in full[w].items()}
        for k in ("candidate", UNCONDITIONAL):
            held[w][k]["per_dataset"] = full[w][k].get("per_dataset")
        held[w]["randomized_summary"] = randomized_summary(held[w])
        c = held[w]["candidate"]
        print(f"[proxy 1h] {w}: candidate {c.get('m1_verdict')} net {c.get('net_pnl_usd')} "
              f"positions {c.get('positions')} refused {c.get('refused')}", flush=True)
    scored = {w: {k: v for k, v in held[w].items() if k != "randomized_summary"} for w in scored_windows}
    verdict = proxy_verdict(proto, scored)
    variants = {}
    for vp in variant_params(proto):
        label = variant_label(vp)
        variants[label] = {"params": vp, "windows": {}}
        for w in [primary] + proto["proxy"]["secondary_windows"]:
            s = study.proxy(study.spec(study.cand, vp), w, with_incumbents=False)
            variants[label]["windows"][w] = brief(s)
        print(f"[variant {label}] oos net {variants[label]['windows'][primary].get('net_pnl_usd')}", flush=True)
    frames, starts = {}, {}
    for symbol in study.symbols:
        key = study.ew.dataset_key(symbol, study.tf)
        frames[key] = proxy_frame(symbol, study.tf, study.windows[primary], study.warmup)
        starts[key] = study.windows[primary][0]
    audit = entry_audit(study.reg, study.cand, frames, params, full[primary]["candidate"].get("positions_list") or [])
    tables = refit_evidence(frames, params, starts, os.path.join(EVIDENCE_DIR, "refit_tables_proxy_1h_oos.json.gz"))
    holds = {}
    for w in windows:
        wf, ws = {}, {}
        for s in study.symbols:
            key = study.ew.dataset_key(s, study.tf)
            try:
                wf[key] = proxy_frame(s, study.tf, study.windows[w], study.warmup)
            except ValueError:
                continue
            ws[key] = study.windows[w][0]
        holds[w] = hold_inventory(study.reg, study.cand, wf, params, ws)
    return {"params": params, "held_out": held, "verdict": verdict, "variants": variants, "entry_audit": audit,
            "refit_tables": tables, "hold_inventory": holds, "warmup_bars": study.warmup,
            "contract": contract_for(proto, params["holding_bars"])}


def run_venue_group(study: Study) -> dict:
    proto = study.proto
    params = dict(proto["parameters"]["venue_4h_primary"])
    costs = proto["venue"]["cost_multipliers"]
    seeds = int(proto["baselines"]["randomized"]["seeds"])
    arms = {"candidate": study.spec(study.cand, params), UNCONDITIONAL: study.spec(UNCONDITIONAL, params)}
    for s in range(seeds):
        arms[f"{RANDOMIZED}_s{s:02d}"] = study.spec(RANDOMIZED, {**params, "random_seed": s})
    full = {k: {label: study.venue(a, mult) for label, mult in costs.items()} for k, a in arms.items()}
    held = {k: {label: brief(s) for label, s in v.items()} for k, v in full.items()}
    for k in ("candidate", UNCONDITIONAL):
        held[k]["base"]["per_dataset"] = full[k]["base"]["per_dataset"]
    c = held["candidate"]["base"]
    print(f"[venue 4h] test: candidate {c['m1_verdict']} net {c['net_pnl_usd']} positions {c['positions']}",
          flush=True)
    verdict = venue_verdict(proto, held, study.venue_manifest["provenance"]["kind"])
    rand = {label: randomized_summary({k: v[label] for k, v in held.items()}) for label in costs}
    import offline_manifest as om
    wname = proto["venue"]["held_out_window"]
    frames, starts = {}, {}
    for ds in study.venue_manifest["datasets"]:
        frame, _, _ = om.window_frame(study.venue_manifest, ds, wname)
        frames[ds["key"]] = frame
        starts[ds["key"]] = study.venue_manifest["windows"][wname]["start"]
    audit = entry_audit(study.reg, study.cand, frames, params, full["candidate"]["base"]["positions_list"])
    tables = refit_evidence(frames, params, starts, os.path.join(EVIDENCE_DIR, "refit_tables_venue_4h_test.json.gz"))
    holds = {wname: hold_inventory(study.reg, study.cand, frames, params, starts)}
    return {"params": params, "held_out": held, "randomized_summary": rand, "verdict": verdict,
            "entry_audit": audit, "refit_tables": tables, "hold_inventory": holds,
            "warmup_bars": study.venue_manifest["warmup_bars"], "contract": contract_for(proto, params["holding_bars"])}


def engine_assumptions(proto: dict) -> dict:
    import inspect
    import backtester as bt_mod
    probe = bt_mod.Backtester(initial_capital=proto["capital_usd"], platform=proto["fee_platform"],
                              strategy_type=proto["strategy_type"])
    sig = inspect.signature(bt_mod.Backtester.__init__)
    defaults = {k: v.default for k, v in sig.parameters.items()
                if k in ("leverage", "sizing_leverage", "margin_per_trade_usd", "risk_per_trade_pct")
                and v.default is not inspect.Parameter.empty}
    return {"commission_pct": getattr(probe, "commission_pct", None),
            "slippage_pct": getattr(probe, "slippage_pct", None),
            "capital_usd": proto["capital_usd"], "intrabar_resolution": proto["intrabar_resolution"],
            "sizing_defaults": defaults, "leverage": getattr(probe, "leverage", None)}


def run(cache_db: str, groups=None) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(cache_db)
    proto = load_json(PROTOCOL_PATH)
    held_lock = require_lock()
    code_check = check_protocol_against_code(proto)
    frozen = verify_frozen(proto)
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    os.makedirs(EVIDENCE_DIR, exist_ok=True)
    study = Study(proto, frozen["proxy"], frozen["venue"])
    started = time.time()
    out = {
        "study": proto["study"], "issue": proto["issue"], "tracking_group": proto["tracking_group"],
        "protocol_sha256": protocol_sha256(), "protocol_lock": held_lock, "protocol_code_check": code_check,
        "manifest_sha256": sha256_file(MANIFEST_PATH), "venue_manifest_sha256": sha256_file(VENUE_MANIFEST_PATH),
        "copied_from_check": frozen["copied_from_check"], "source_commit": git_head(),
        "source_sha256": {p: sha256_file(os.path.join(REPO, p)) for p in SOURCE_FILES},
        "engine": engine_assumptions(proto), "network_blocked_while_scoring": True,
        "incumbents": list(study.ew.INCUMBENTS),
        "venue_manifest": {
            "provenance": frozen["venue"]["provenance"], "interval": frozen["venue"]["interval"],
            "warmup_bars": frozen["venue"]["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                        for k, v in frozen["venue"]["windows"].items()},
            "costs": frozen["venue"]["costs"],
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"],
                          "half_spread_bps": d["half_spread_bps"], "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                         for d in frozen["venue"]["datasets"]],
        },
        "groups": {},
    }
    wanted = groups or ["proxy_1h", "venue_4h"]
    with installed_baselines(study.reg):
        for g in wanted:
            out["groups"][g] = run_proxy_group(study) if g == "proxy_1h" else run_venue_group(study)
            print(f"[{g}] verdict {out['groups'][g]['verdict']['outcome']}: {out['groups'][g]['verdict']['reason']}",
                  flush=True)
    out["baselines_removed"] = all(n not in study.reg.STRATEGY_REGISTRY for n in (UNCONDITIONAL, RANDOMIZED))
    if not out["baselines_removed"]:
        raise SystemExit("a study-local baseline is still installed in the research registry")
    out["incumbent_refusals"] = {f"{w}/H{h}": v["refused"] for (w, h), v in sorted(study.memo.items())
                                 if "refused" in v}
    out["verdict"] = overall_verdict({g: v["verdict"] for g, v in out["groups"].items()})
    out["verdict_guards"] = {}
    for g, v in out["groups"].items():
        if g == "proxy_1h":
            names = [proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"] + proto["proxy"]["refused_windows"]
            scored = {w: {k: x for k, x in v["held_out"][w].items() if k != "randomized_summary"} for w in names}
            out["verdict_guards"][g] = verdict_guards(proto, scored, {})
        else:
            out["verdict_guards"][g] = verdict_guards(proto, {}, v["held_out"])
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']['outcome']}; wrote {RESULTS_PATH}")


def synthetic_frame():
    import numpy as np
    import pandas as pd
    from closed_bar import TIMESTAMP_KIND_OPEN, declare_frame_cadence
    rng = np.random.default_rng(SYNTHETIC_SEED)
    n = SYNTHETIC_BARS
    idx = pd.date_range(SYNTHETIC_START, periods=n, freq="1h", tz="UTC")
    ret = rng.normal(0.0, 0.003, n)
    cell = idx.dayofweek * 24 + idx.hour
    ret[np.asarray(cell == 14)] += 0.006
    ret[np.asarray(cell == 3 * 24 + 3)] -= 0.006
    close = 30_000.0 * np.exp(np.cumsum(ret))
    open_ = np.concatenate([[30_000.0], close[:-1]])
    wick = np.abs(rng.normal(0.0, 0.001, n))
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) * (1.0 + wick),
                       "low": np.minimum(open_, close) * (1.0 - wick), "close": close,
                       "volume": 1000.0 + rng.random(n) * 100.0}, index=idx)
    df.index.name = "datetime"
    return declare_frame_cadence(df, "1h", TIMESTAMP_KIND_OPEN)


def _same(a, b) -> bool:
    if isinstance(a, float) and isinstance(b, float) and math.isnan(a) and math.isnan(b):
        return True
    return a == b


def prefix_parity(reg, cand: str, df, params: dict) -> dict:
    from calendar_conditioned_entry import calendar_refit_tables
    full = reg.apply_strategy(cand, df, dict(params))
    mismatches = []
    for i in range(len(df)):
        last = reg.apply_strategy(cand, df.iloc[:i + 1], dict(params)).iloc[-1]
        for col in PARITY_COMPARE_COLUMNS:
            if not _same(last[col], full[col].iloc[i]):
                mismatches.append({"bar": str(df.index[i]), "column": col, "prefix": str(last[col]),
                                   "full": str(full[col].iloc[i])})
    tables = calendar_refit_tables(df, **params)
    pos = {int(t.value // 1_000_000): i for i, t in enumerate(df.index)}
    table_mismatches = []
    for t in tables:
        r = pos[t["refit_bar_open_ms"]]
        prefix_tables = calendar_refit_tables(df.iloc[:r + 1], **params)
        if not prefix_tables or prefix_tables[-1] != t:
            table_mismatches.append(t["refit_boundary_ms"])
    shifted = {}
    for k in (1, 25, params["refit_bars"] - 1, params["refit_bars"]):
        out = reg.apply_strategy(cand, df.iloc[k:], dict(params))
        bad = 0
        lost = 0
        for j in range(len(out)):
            i = j + k
            if out["cce_valid"].iloc[j] == 1.0:
                if any(not _same(out[c].iloc[j], full[c].iloc[i]) for c in PARITY_COMPARE_COLUMNS):
                    bad += 1
            elif full["cce_valid"].iloc[i] == 1.0:
                lost += 1
                if out["cce_reason"].iloc[j] != "insufficient_history":
                    bad += 1
        shifted[str(k)] = {"mismatches": bad, "decisions_without_history_after_shift": lost}
    return {"bars": int(len(df)), "mismatches": len(mismatches), "first_mismatches": mismatches[:10],
            "refit_tables": len(tables), "table_mismatches": table_mismatches,
            "shifted_start": shifted,
            "valid_bars": int((full["cce_valid"] == 1.0).sum()),
            "long_signals": int((full["signal"] == 1).sum()), "short_signals": int((full["signal"] == -1).sum()),
            "reasons": {k: int(v) for k, v in sorted(full["cce_reason"].value_counts().to_dict().items())}}


def _parity_row(label: str, tf: str, path: str, sid: str, df, frame, summ: dict, batched: bool, window,
                seconds: float) -> dict:
    validation = summ.get("close_validation") or {}
    return {"source": label, "interval": tf, "config": os.path.basename(path), "strategy_id": sid,
            "batched": batched, "window": "every prefix" if window is None else window,
            "input_sha256": frame_sha256(df), "candles": int(len(df)), "bars_compared": summ["bars_compared"],
            "mismatches": summ["mismatches"], "batch_mismatches": summ.get("batch_mismatches"),
            "first_mismatch": summ.get("first_mismatch"), "last_mismatch": summ.get("last_mismatch"),
            "decision_agreement": summ["decision_agreement"], "close_parity": summ["close_parity"],
            "close_eligibility": validation.get("close_eligibility"),
            "incomplete_parity": validation.get("incomplete_parity"), "comparison_mode": validation.get("mode"),
            "approximation_codes": sorted({a.get("reason_code") for a in validation.get("approximations") or []}),
            "clean": summ["clean"],
            "bt_entries": int((frame["bt_open_action"] != "none").sum()) if len(frame) else 0,
            "live_entries": int((frame["live_open_action"] != "none").sum()) if len(frame) else 0,
            "close_fraction_mismatches": int(((frame["bt_close_fraction"] - frame["live_close_fraction"]).abs()
                                              >= 1e-9).sum()) if len(frame) else 0,
            "open_action_mismatches": int((frame["bt_open_action"] != frame["live_open_action"]).sum())
            if len(frame) else 0, "seconds": round(seconds, 1)}


def parity(cache_db: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(cache_db)
    import pandas as pd
    proto = load_json(PROTOCOL_PATH)
    require_lock()
    frozen = verify_frozen(proto)
    build_cache(cache_db, frozen["proxy"], aliases=True)
    forbid_network()
    import offline_manifest as om
    import parity_diff
    from parity_diff import compute_parity_frame, config_from_live_config, summarize as parity_summarize
    from registry_loader import load_registry
    reg = load_registry(proto["registry"])
    cand = proto["candidate"]
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "venue_manifest_sha256": sha256_file(VENUE_MANIFEST_PATH),
           "source_commit": git_head(), "prefix": {}, "cli": [], "runs": []}
    p1 = dict(proto["parameters"]["proxy_1h_primary"])
    p4 = dict(proto["parameters"]["venue_4h_primary"])
    proxy = proxy_frame("BTC/USDT", "1h", (PARITY_SINCE, "2026-10-01"))
    vds = om.dataset_by_key(frozen["venue"], "BTC 4h")
    venue, _, _ = om.window_frame(frozen["venue"], vds, proto["venue"]["held_out_window"])
    synth = synthetic_frame()
    sources = [("proxy BTC/USDT 1h " + PARITY_SINCE + "..2026-10-01", "1h", proxy, p1),
               (f"synthetic seeded 1h walk with weekly cells ({SYNTHETIC_BARS} bars, seed {SYNTHETIC_SEED})", "1h",
                synth, p1),
               ("venue BTC 4h test window with warmup", "4h", venue, p4)]
    for label, tf, df, params in sources:
        t0 = time.time()
        res = prefix_parity(reg, cand, df, params)
        res["seconds"] = round(time.time() - t0, 1)
        res["input_sha256"] = frame_sha256(df)
        out["prefix"][label] = res
        print(json.dumps({"prefix": label, "mismatches": res["mismatches"], "tables": res["refit_tables"],
                          "table_mismatches": len(res["table_mismatches"]), "long": res["long_signals"],
                          "short": res["short_signals"]}), flush=True)
    path1 = os.path.join(HERE, "paper_parity_1h.json")
    sid1 = load_json(path1)["strategies"][0]["id"]
    for window in ("0", "200"):
        jsonl = os.path.join(os.path.dirname(os.path.abspath(cache_db)), f"cce1653_cli_{window}.jsonl")
        argv = ["--config", path1, "--strategy-id", sid1, "--window", window, "--comparison-mode",
                proto["contract"]["comparison_mode"], "--funding", proto["proxy"]["funding_mode"],
                "--since", PARITY_SINCE, "--jsonl", jsonl]
        t0 = time.time()
        code = parity_diff.main(argv)
        rows = []
        tail = {}
        with open(jsonl) as fh:
            for line in fh:
                rec = json.loads(line)
                if "close_validation" in rec:
                    tail = rec
                elif "fill" not in rec:
                    rows.append(rec)
        mism = [r for r in rows if not r["match"]]
        out["cli"].append({"argv": ["backtest/parity_diff.py"] + [os.path.relpath(a, REPO) if a in (path1,) else a
                                                                   for a in argv if a != jsonl and a != "--jsonl"],
                           "exit_code": code, "bars_compared": len(rows), "mismatches": len(mism),
                           "first_mismatch": mism[0]["ts"] if mism else None,
                           "last_mismatch": mism[-1]["ts"] if mism else None,
                           "bt_entries": sum(1 for r in rows if r["bt_open_action"] != "none"),
                           "live_entries": sum(1 for r in rows if r["live_open_action"] != "none"),
                           "close_parity": tail.get("close_parity"),
                           "incomplete_parity": (tail.get("close_validation") or {}).get("incomplete_parity"),
                           "seconds": round(time.time() - t0, 1)})
        print(json.dumps({k: out["cli"][-1][k] for k in ("exit_code", "bars_compared", "mismatches")}), flush=True)
    for label, tf, df, params in sources:
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        sid = load_json(path)["strategies"][0]["id"]
        for batched in (False, True):
            for window in (200, None):
                cfg = config_from_live_config(path, sid, comparison_mode=proto["contract"]["comparison_mode"])
                cfg.batched = batched
                cfg.funding_mode = proto["proxy"]["funding_mode"]
                t0 = time.time()
                frame = compute_parity_frame(df, cfg=cfg, window=window)
                summ = parity_summarize(frame)
                row = _parity_row(label, tf, path, sid, df, frame, summ, batched, window, time.time() - t0)
                if window == 200:
                    lives = []
                    for i in range(199, len(df), max(1, (len(df) - 199) // 50)):
                        lives.append(reg.apply_strategy(cand, df.iloc[i - 199:i + 1], dict(params))["cce_reason"].iloc[-1])
                    row["bounded_frame_last_bar_reasons_sampled"] = dict(sorted(pd.Series(lives).value_counts().to_dict().items()))
                out["runs"].append(row)
                print(json.dumps({k: row[k] for k in ("interval", "source", "batched", "window", "bars_compared",
                                                      "mismatches", "batch_mismatches", "bt_entries",
                                                      "live_entries", "close_parity")}), flush=True)
    out["prefix_clean"] = all(r["mismatches"] == 0 and not r["table_mismatches"]
                              and all(s["mismatches"] == 0 for s in r["shifted_start"].values())
                              for r in out["prefix"].values())
    out["batch_agreement"] = all(r["batch_mismatches"] == 0 for r in out["runs"] if r["batched"])
    out["expanding_decision_agreement"] = {f"{r['source']} batched={r['batched']}": r["decision_agreement"]
                                           for r in out["runs"] if r["window"] == "every prefix"}
    out["complete_live_parity"] = False
    write_json(PARITY_PATH, out)
    print(f"prefix clean: {out['prefix_clean']}; batch agreement: {out['batch_agreement']}; wrote {PARITY_PATH}")


def fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def tri(v):
    return "unknown" if v is None else ("true" if v else "false")


def _arm_table(arms: dict) -> list:
    lines = ["| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) "
             "| clusters | turnover | fees $ | funding $ | incomplete legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, s in arms.items():
        if s.get("refused"):
            lines.append(f"| {name} | refused: {s['refused'][:120]} | | | | | | | | | | |")
            continue
        lines.append(f"| {name} | {s.get('m1_verdict')} | {fmt(s.get('mean_sharpe'))} | {fmt(s.get('mean_return_pct'))} "
                     f"| {fmt(s.get('net_pnl_usd'))} | {fmt(s.get('worst_max_dd_pct'))} | {s.get('positions')} "
                     f"({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('clusters')} "
                     f"| {fmt(s.get('mean_turnover'))} | {fmt(s.get('fees_usd'))} | {fmt(s.get('funding_pnl_usd'))} "
                     f"| {s.get('funding_incomplete_legs')} |")
    return lines


def _holds_lines(holds: dict) -> list:
    lines = ["| window | dataset | scored bars | valid bars | first valid bar | entries L/S | hold reasons |",
             "|---|---|---|---|---|---|---|"]
    entry = ("entry_long", "entry_short", "below_threshold")
    for w, per in holds.items():
        if not per:
            lines.append(f"| {w} | all | - | - | - | - | prefix refused (no frame) |")
        for ds, h in per.items():
            reasons = ", ".join(f"{k} {v}" for k, v in h["reasons"].items() if k not in entry)
            lines.append(f"| {w} | {ds} | {h['bars']} | {h['valid_bars']} | {h['first_valid_bar']} | "
                         f"{h['reasons'].get('entry_long', 0)}/{h['reasons'].get('entry_short', 0)} "
                         f"| {reasons or '-'} |")
    return lines


def _brief_arms(arms: dict) -> dict:
    return {k: v for k, v in arms.items() if k == "candidate" or k == UNCONDITIONAL}


def _lb_line(cand: dict) -> str:
    lb = cand.get("lower_bound") or {}
    return (f"per-position net return mean {fmt(lb.get('mean'), 5)}, sd {fmt(lb.get('sd'), 5)}, "
            f"{lb.get('clusters')} independent clusters, z {fmt(lb.get('z'), 3)}, corrected lower bound "
            f"{fmt(lb.get('lower_bound'), 5)}")


def render() -> None:
    proto = load_json(PROTOCOL_PATH)
    res = load_json(RESULTS_PATH)
    par = load_json(PARITY_PATH) if os.path.exists(PARITY_PATH) else None
    manifest = load_json(MANIFEST_PATH)
    v = res["verdict"]
    L = []
    L.append("# Calendar-conditioned entry study (issue 1653)")
    L.append("")
    L.append(f"**Verdict: {v['outcome'].upper()}** (proxy 1h: {v['groups'].get('proxy_1h')}, venue 4h: "
             f"{v['groups'].get('venue_4h')}). Registry metadata after this study: `edge_status=\"no_edge\"`, "
             f"`edge_source=\"{v['edge_source']}\"`, `edge_ref` = this file. The candidate stays hidden from "
             "discovery. Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result "
             "here adds one or approves deployment. Every command in this study is suggest-only and sends no order. "
             "The proxy group is proxy evidence (Binance US candles with Hyperliquid fees and funding); the venue "
             "group is Hyperliquid candle evidence. No positive economic outcome was required.")
    L.append("")
    L.append("Group reasons: " + "; ".join(f"{g}: {d['verdict']['outcome']} ({d['verdict']['reason']})"
                                         for g, d in res["groups"].items()) + ".")
    L.append("")
    L.append("## Signal")
    L.append("")
    s = proto["statistics"]
    L.append("`calendar_conditioned_entry` (`shared_strategies/open/calendar_conditioned_entry.py`) is one pure, "
             "entry-only evaluator. " + s["return_unit"][0].upper() + s["return_unit"][1:] + ". " +
             s["label_completion"][0].upper() + s["label_completion"][1:] + ". Cells: " + s["calendar_cell"] +
             ". Refits: " + s["refit_grid"] + ". Training: " + s["training_sample"] + ". Clusters: " + s["cluster"] +
             ". Uncertainty: " + s["uncertainty"] + ". Effective sample: " + s["effective_sample"] + "; " +
             s["undefined_uncertainty"] + ". Edges: " + s["directional_edges"] + ". Threshold: " + s["threshold"] +
             ". Ties: " + s["tie_rule"] + ". " + s["refit_threshold_scope"][0].upper() +
             s["refit_threshold_scope"][1:] + ". Cost hurdle: " + s["cost_hurdle"] + ". Every hold carries a reason "
             "in `cce_reason`, and a data hold returns a zero-signal frame so native close and protective "
             "management continue.")
    L.append("")
    L.append(proto["reference_note"][0].upper() + proto["reference_note"][1:] + ".")
    L.append("")
    L.append("## Protocol")
    L.append("")
    lock_ = res["protocol_lock"]
    L.append(f"`protocol.json` (sha256 `{res['protocol_sha256']}`) was locked by `run_study.py lock` at "
             f"{lock_['locked_at_utc']} (commit `{lock_['source_commit']}`), before any window was scored; `run` "
             "and `parity` refuse a different protocol, and `run` checks that the primary parameters equal the "
             "registry defaults, the variant grid equals optimizer `DEFAULT_PARAM_RANGES` and the warm-up equals "
             f"`required_history_bars` ({json.dumps(res['protocol_code_check'], sort_keys=True)}). "
             + proto["parameters"]["selection"][0].upper() + proto["parameters"]["selection"][1:] + ".")
    L.append("")
    L.append(f"Contract: {proto['contract_note']}")
    L.append("")
    L.append("| group | parameters | close | stop | warm-up bars |")
    L.append("|---|---|---|---|---|")
    for gname, g in res["groups"].items():
        L.append(f"| {gname} | `{json.dumps(g['params'], sort_keys=True)}` | "
                 f"`{json.dumps(g['contract']['close_strategies'])}` | {g['contract']['stop_loss_atr_mult']} x ATR "
                 f"| {g['warmup_bars']} |")
    L.append("")
    b = proto["baselines"]
    L.append(f"Baselines (implemented in `run_study.py`, never registered for production): `{b['unconditional']['name']}`"
             f" — {b['unconditional']['definition']}. `{b['randomized']['name']}` ({b['randomized']['seeds']} seeds) "
             f"— {b['randomized']['definition']}. {b['randomized']['comparison'][0].upper()}"
             f"{b['randomized']['comparison'][1:]}. {b['registration'][0].upper()}{b['registration'][1:]} (removed "
             f"after the run: {fmt(res.get('baselines_removed'))}).")
    L.append("")
    L.append(RANDOMIZED_SEED_DEPARTURE)
    L.append("")
    w = proto["warmup"]
    L.append(f"Warm-up: {w['rule']}. {w['policy'][0].upper()}{w['policy'][1:]}. {w['refusal'][0].upper()}"
             f"{w['refusal'][1:]}.")
    L.append("")
    ec = proto["economic_correction"]
    L.append(f"Economic correction: {ec['statistic']}; {ec['family_note']}.")
    L.append("")
    L.append("Proxy group rule: " + proto["verdict_rules"]["proxy_group"]["order"] + ". Venue group rule: "
             + proto["verdict_rules"]["venue_group"]["order"] + ". Overall: " + proto["verdict_rules"]["overall"] + ".")
    L.append("")
    L.append("| proxy window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    for wn, (st, en) in proto["proxy"]["windows"].items():
        role = ("primary held-out" if wn == proto["proxy"]["primary_window"] else
                "diagnostic" if wn in proto["proxy"]["diagnostic_windows"] else
                "refused (no prefix)" if wn in proto["proxy"]["refused_windows"] else "secondary held-out")
        L.append(f"| {wn} | {st} | {en} | {role} |")
    L.append("")
    L.append("Departures from the default M1 windows: " + "; ".join(proto["proxy"]["departures_from_m1_windows"])
             + ". " + proto["proxy"]["window_note"][0].upper() + proto["proxy"]["window_note"][1:] + ".")
    L.append("")
    vm = res["venue_manifest"]
    L.append("| venue window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    for wn, meta in vm["windows"].items():
        L.append(f"| {wn} | {meta['start']} | {meta['end']} | {meta['role']} |")
    L.append("")
    L.append(proto["venue"]["window_note"][0].upper() + proto["venue"]["window_note"][1:] + ".")
    L.append("")
    L.append("## Inputs, costs and assumptions")
    L.append("")
    eng = res["engine"]
    L.append("- Proxy candles: Binance US spot USDT 1h candles stand in for Hyperliquid perpetual prices, so this is "
             f"proxy evidence and not measured Hyperliquid candle evidence ({res['copied_from_check']['proxy']}).")
    L.append("- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, "
             "`eval_windows.run_leg(funding_mode=\"charge\")`) for every candidate, baseline and incumbent leg; "
             "funding is attached to the scored window only, never to warm-up bars; unavailable funding is never "
             "a silent zero.")
    L.append(f"- Proxy costs: engine Hyperliquid commission `{eng['commission_pct']}` per side and engine slippage "
             f"`{eng['slippage_pct']}` per fill; the slippage term stands in for the spread. This legacy M1 screen "
             "replays no spread, lot rounding or minimum order, so it cannot by itself establish venue execution "
             "parity.")
    L.append(f"- Venue candles: {vm['provenance']['label']} ({res['copied_from_check']['venue']}), interval "
             f"{vm['interval']}, {vm['warmup_bars']} manifest warm-up bars read by the evaluator but never scored or "
             "traded; the cached warm-up is not passed on this path.")
    costs = vm["costs"]
    L.append(f"- Venue costs (manifest execution specification with the explicit close owner): taker fee "
             f"{costs['taker_fee_pct']}, maker fee {costs['maker_fee_pct']}, slippage {costs['slippage_bps']} bps per "
             f"side plus the per-coin half spread from one frozen L2 snapshot, szDecimals lot rounding, minimum "
             f"notional ${costs['min_notional_usd']} with a {costs['min_notional_margin']} margin; cost x2 doubles "
             "spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.")
    L.append(f"- Sizing: capital ${eng['capital_usd']:.0f} per leg, no leverage and no perps sizing block (Backtester "
             f"leverage {eng['leverage']}, sizing defaults {json.dumps(eng['sizing_defaults'], sort_keys=True)}).")
    L.append("")
    L.append("| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |")
    L.append("|---|---|---|---|---|")
    for d in vm["datasets"]:
        L.append(f"| {d['key']} | {d['size_decimals']} | {d['half_spread_bps']} | `{d['candles_sha256']}` "
                 f"| `{d['funding_sha256']}` |")
    L.append("")
    L.append("| proxy file (read in place) | rows | sha256 |")
    L.append("|---|---|---|")
    for name, meta in manifest["files"].items():
        L.append(f"| `{manifest['data_root']}/{name}` | {meta['rows']} | `{meta['sha256']}` |")
    L.append("")
    for gname, g in res["groups"].items():
        venue = gname.startswith("venue")
        L.append(f"## {gname}")
        L.append("")
        ver = g["verdict"]
        L.append(f"**Group verdict: {ver['outcome']}** ({ver['reason']}).")
        L.append("")
        L.append("| check | value |")
        L.append("|---|---|")
        for k, val in ver["checks"].items():
            L.append(f"| {k} | {tri(val)} |")
        L.append("")
        if venue:
            for label in ("base", "stress"):
                L.append(f"### Held-out `test`, cost {label}")
                L.append("")
                L.extend(_arm_table({k: v_[label] for k, v_ in _brief_arms(g["held_out"]).items()}))
                L.append("")
                rs = g["randomized_summary"][label]
                L.append(f"Randomized control ({rs.get('seeds')} seeds): net PnL max ${fmt(rs.get('max_net_pnl_usd'))}, "
                         f"median ${fmt(rs.get('median_net_pnl_usd'))}, min ${fmt(rs.get('min_net_pnl_usd'))}; "
                         f"realized positions {rs.get('positions_min')} to {rs.get('positions_max')} per seed.")
                L.append("")
            cand = g["held_out"]["candidate"]["base"]
            L.append("Candidate economics (test, cost x1): " + _lb_line(cand) + f"; calendar concentration "
                     f"{json.dumps(cand.get('calendar_concentration'), sort_keys=True)}; close validation codes "
                     f"{cand.get('close_validation_codes')}.")
            L.append("")
            L.append("| dataset | candles complete | funding complete | rejected entries | exit reasons |")
            L.append("|---|---|---|---|---|")
            for ds, item in cand["per_dataset"].items():
                L.append(f"| {ds} | {fmt(item['candle_coverage']['complete'])} "
                         f"| {fmt(item['funding_coverage']['complete'])} | {item['rejected_entries']} "
                         f"| {', '.join(item['exit_reasons']) or '-'} |")
            L.append("")
        else:
            order = ([proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"]
                     + proto["proxy"]["refused_windows"] + proto["proxy"]["diagnostic_windows"])
            for wn in order:
                arms = g["held_out"][wn]
                L.append(f"### `{wn}`")
                L.append("")
                L.extend(_arm_table(_brief_arms(arms)))
                L.append("")
                rs = arms.get("randomized_summary") or {}
                if rs:
                    L.append(f"Randomized control ({rs.get('seeds')} seeds): net PnL max "
                             f"${fmt(rs.get('max_net_pnl_usd'))}, median ${fmt(rs.get('median_net_pnl_usd'))}, min "
                             f"${fmt(rs.get('min_net_pnl_usd'))}; realized positions {rs.get('positions_min')} to "
                             f"{rs.get('positions_max')} per seed.")
                    L.append("")
            cand = g["held_out"][proto["proxy"]["primary_window"]]["candidate"]
            L.append("Candidate economics (oos): " + _lb_line(cand) + f"; calendar concentration "
                     f"{json.dumps(cand.get('calendar_concentration'), sort_keys=True)}; close validation codes "
                     f"{cand.get('close_validation_codes')}.")
            L.append("")
            if ver.get("unscorable_windows"):
                L.append("Unscorable windows: " + "; ".join(f"{wn}: {', '.join(r)[:400]}"
                                                           for wn, r in ver["unscorable_windows"].items()) + ".")
                L.append("")
            L.append("### Variant grid (descriptive; no variant changes a verdict)")
            L.append("")
            L.append("| variant | window | net PnL $ | mean Sharpe | positions (L/S) | clusters | corrected lower bound |")
            L.append("|---|---|---|---|---|---|---|")
            for label, vv in g["variants"].items():
                for wn, sm in vv["windows"].items():
                    if sm.get("refused"):
                        L.append(f"| {label} | {wn} | refused | | | | |")
                        continue
                    L.append(f"| {label} | {wn} | {fmt(sm.get('net_pnl_usd'))} | {fmt(sm.get('mean_sharpe'))} "
                             f"| {sm.get('positions')} ({sm.get('long_positions')}/{sm.get('short_positions')}) "
                             f"| {sm.get('clusters')} | {fmt((sm.get('lower_bound') or {}).get('lower_bound'), 5)} |")
            L.append("")
        L.append("Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):")
        L.append("")
        L.extend(_holds_lines(g["hold_inventory"]))
        L.append("")
        tbl = g["refit_tables"]
        n_tables = sum(len(x) for x in tbl["tables"].values())
        elig = sum(t["eligible_long"] + t["eligible_short"] for x in tbl["tables"].values() for t in x)
        best = [max(x for x in (t["max_long_lower_bound"], t["max_short_lower_bound"]) if x is not None)
                for xs in tbl["tables"].values() for t in xs
                if t["max_long_lower_bound"] is not None or t["max_short_lower_bound"] is not None]
        L.append(f"Refit tables: {n_tables} tables across the datasets of the primary window (full per-cell tables in "
                 f"`{tbl['file']}`, uncompressed sha256 `{tbl['sha256_uncompressed']}`); {elig} eligible cell-directions "
                 "in total; the largest directional lower bound of any cell in any of these tables is "
                 f"{fmt(max(best) if best else None, 5)} (a direction needs a value strictly above 0). "
                 "Per refit (first and last per dataset):")
        L.append("")
        L.append("| dataset | refit boundary (UTC) | training first open | labels | eligible L/S | z | best lower bound L/S | reasons |")
        L.append("|---|---|---|---|---|---|---|---|")
        import pandas as pd
        for ds, x in tbl["tables"].items():
            for t in (x[:1] + x[-1:]) if x else []:
                L.append(f"| {ds} | {pd.Timestamp(t['refit_boundary_ms'], unit='ms')} "
                         f"| {pd.Timestamp(t['training_first_open_ms'], unit='ms')} | {t['labels']} "
                         f"| {t['eligible_long']}/{t['eligible_short']} | {fmt(t['corrected_z'], 3)} "
                         f"| {fmt(t['max_long_lower_bound'], 5)}/{fmt(t['max_short_lower_bound'], 5)} "
                         f"| {json.dumps(t['reasons'], sort_keys=True)} |")
        L.append("")
        audit = g["entry_audit"]
        L.append(f"Entry audit: {len(audit)} held-out candidate entries; each fill follows a valid raw signal on the "
                 "prior closed bar, the selected cell's weekly sample and mean equal an independent recomputation "
                 "from the raw opens, the table predates the decision, and every time_stop exit sits on the nominal "
                 "horizon. First entries:")
        L.append("")
        L.append("| dataset | side | signal bar | fill bar | cell (wd-h) | mean | lower bound L/S | weeks | n_eff | net $ |")
        L.append("|---|---|---|---|---|---|---|---|---|---|")
        for a in audit[:10]:
            L.append(f"| {a['dataset']} | {a['side']} | {a['signal_bar']} | {a['fill_bar']} | "
                     f"{a['next_weekday']}-{a['next_hour']:02d} | {a['gross_mean']:.6f} | "
                     f"{a['long_lower_bound']:.6f}/{a['short_lower_bound']:.6f} | {a['weekly_obs']} "
                     f"| {a['effective_obs']:.2f} | {fmt(a['net_pnl_usd'])} |")
        L.append("")
    L.append("Verdict guards (each altered copy must not pass): " + "; ".join(
        f"{g}: " + ", ".join(f"{k} -> {v_}" for k, v_ in guards.items()) for g, guards in res["verdict_guards"].items())
        + ".")
    L.append("")
    if res.get("incumbent_refusals"):
        L.append("Incumbent refusals: " + "; ".join(f"{k}: {v_[:200]}" for k, v_ in res["incumbent_refusals"].items())
                 + ".")
        L.append("")
    L.append("## Parity")
    L.append("")
    if par:
        L.append("Entry parity (pure evaluator through the futures registry; every prefix, every refit table, "
                 f"shifted starts): clean = {fmt(par['prefix_clean'])}.")
        L.append("")
        L.append("| source | bars | valid bars | long/short | prefix mismatches | tables | table mismatches | shifted-start mismatches |")
        L.append("|---|---|---|---|---|---|---|---|")
        for label, r in par["prefix"].items():
            shifted = ", ".join(f"{k}: {x['mismatches']}" for k, x in r["shifted_start"].items())
            L.append(f"| {label} | {r['bars']} | {r['valid_bars']} | {r['long_signals']}/{r['short_signals']} "
                     f"| {r['mismatches']} | {r['refit_tables']} | {len(r['table_mismatches'])} | {shifted} |")
        L.append("")
        L.append("`backtest/parity_diff.py` CLI runs (non-batched, proxy BTC 1h from the frozen cache):")
        L.append("")
        L.append("| argv | exit | bars compared | mismatches | backtest entries | live entries | close parity |")
        L.append("|---|---|---|---|---|---|---|")
        for r in par["cli"]:
            L.append(f"| `{' '.join(r['argv'])}` | {r['exit_code']} | {r['bars_compared']} | {r['mismatches']} "
                     f"| {r['bt_entries']} | {r['live_entries']} | {r['close_parity']} |")
        L.append("")
        L.append("`compute_parity_frame` runs (non-batched and batched solo vs batch slots through the real "
                 "Hyperliquid check evaluator):")
        L.append("")
        L.append("| source | batched | window | bars compared | mismatches | open-action mismatches | close-fraction "
                 "mismatches | batch mismatches | bt/live entries | close parity |")
        L.append("|---|---|---|---|---|---|---|---|---|---|")
        for r in par["runs"]:
            L.append(f"| {r['source']} | {fmt(r['batched'])} | {r['window']} | {r['bars_compared']} | {r['mismatches']} "
                     f"| {r['open_action_mismatches']} | {r['close_fraction_mismatches']} | {fmt(r['batch_mismatches'])} "
                     f"| {r['bt_entries']}/{r['live_entries']} | {r['close_parity']} ({', '.join(r['approximation_codes'])}) |")
        L.append("")
        L.append(proto["parity"]["parity_diff"][0].upper() + proto["parity"]["parity_diff"][1:] + ". "
                 + proto["parity"]["bounded_frame"][0].upper() + proto["parity"]["bounded_frame"][1:] + ". "
                 "Close fractions agree in these runs only because neither side of the decision harness supplies "
                 "`bars_held`: its position walk is stop-free and `time_stop` holds on both sides, so the simulator's "
                 "horizon closes (verified separately against real simulator ledgers in the integration tests) are "
                 "never compared, and the live check has no `bars_held` either. "
                 f"Solo and batched evaluation agree: {fmt(par['batch_agreement'])}. Complete live parity: "
                 f"{fmt(par['complete_live_parity'])}.")
        L.append("")
    else:
        L.append("Parity has not been run for this results file.")
        L.append("")
    L.append("## Promotion boundary")
    L.append("")
    pb = proto["promotion_boundary"]
    L.append(f"Proposed future artifact contract: {pb['proposed_table_contract']}. {pb['scope'][0].upper()}"
             f"{pb['scope'][1:]}. Unresolved production limits:")
    L.append("")
    for item in pb["unresolved_limits"]:
        L.append(f"- {item[0].upper()}{item[1:]}.")
    L.append("")
    L.append("## Limits")
    L.append("")
    L.append("- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not "
             "modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution "
             "claims rest on the venue group only.")
    L.append("- The 2023 proxy window has no 4535-bar prefix and the venue train window has no 1133-bar prefix; both "
             "are refused or unscored, never filled. The 2026-08-31 proxy gap holds every later oos decision.")
    L.append("- The venue window covers 2025-09-01 to 2026-09-01 only and earlier studies already used it, so the "
             "venue result is retrospective. Spread is a per-coin half spread from one frozen L2 snapshot plus flat "
             "slippage; queue position, partial fills and market impact are not modelled.")
    L.append("- Every economic run uses approximate close comparison: `time_stop` reads simulator-only `bars_held`, "
             "so no result here is complete live close parity.")
    L.append("- Every score reads only frozen inputs with network acquisition disabled. A research result alone "
             "cannot approve deployment.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append("uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py lock")
    L.append("uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py run "
             "--cache-db /tmp/cce1653_run.db")
    L.append("uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py parity "
             "--cache-db /tmp/cce1653_parity.db")
    L.append("uv run --no-sync python backtest/candidates/calendar_conditioned_entry_1653/run_study.py render")
    L.append("```")
    L.append("")
    L.append(f"Results: `results.json` (source commit `{res['source_commit']}`, runtime {res['runtime_seconds']} s, "
             f"manifest sha256 `{res['manifest_sha256']}`, venue manifest sha256 `{res['venue_manifest_sha256']}`)"
             + (f", `parity_results.json` (source commit `{par['source_commit']}`)" if par else "") + ".")
    L.append("")
    with open(REPORT_PATH, "w") as fh:
        fh.write("\n".join(L))
    print(f"wrote {REPORT_PATH}")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1653 calendar-conditioned entry study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("lock", help="record protocol.json's sha256 before any score")
    sub.add_parser("hash", help="print protocol.json's sha256")
    r = sub.add_parser("run", help="rebuild a fresh cache from the frozen files and run the protocol offline")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--group", action="append", choices=["proxy_1h", "venue_4h"])
    q = sub.add_parser("parity", help="entry and decision parity on the frozen frames")
    q.add_argument("--cache-db", required=True)
    sub.add_parser("render", help="re-render REPORT.md from results.json and parity_results.json")
    args = p.parse_args(argv)
    if args.cmd == "hash":
        print(protocol_sha256())
        return 0
    if args.cmd == "lock":
        lock()
        return 0
    if args.cmd == "run":
        run(args.cache_db, args.group)
    elif args.cmd == "parity":
        parity(args.cache_db)
    if os.path.exists(RESULTS_PATH):
        render()
    return 0


if __name__ == "__main__":
    sys.exit(main())
