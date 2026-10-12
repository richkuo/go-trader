#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import io
import itertools
import json
import os
import statistics
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
DATA = os.path.join(HERE, "data")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
VENUE_MANIFEST_PATH = os.path.join(HERE, "venue_manifest.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
PARITY_PATH = os.path.join(HERE, "parity_results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
HOUR_MS = 3_600_000
for _p in (os.path.join(REPO, "shared_tools"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
COPIED_FROM = {
    "proxy": "backtest/candidates/chande_momentum_1661/study_manifest.json",
    "venue": "backtest/candidates/aroon_trend_1643/study_manifest.json",
}
SOURCE_FILES = (
    "shared_strategies/open/dual_thrust.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
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
    "backtest/candidates/dual_thrust_1642/run_study.py",
    "backtest/candidates/dual_thrust_1642/protocol.json",
)
PARITY_SPANS = {"1h": ("2026-07-01", "2026-10-01"), "4h": ("2025-10-01", "2026-10-01")}
PARITY_WINDOWS = (200, None)


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


def set_paths(cache_db: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(cache_db)
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)


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


def verify_frozen() -> dict:
    manifest = load_json(MANIFEST_PATH)
    for name, meta in manifest["files"].items():
        got = sha256_file(os.path.join(DATA, name))
        if got != meta["sha256"]:
            raise SystemExit(f"frozen input data/{name} sha256 {got} != manifest {meta['sha256']}")
    source = os.path.join(REPO, COPIED_FROM["proxy"])
    copied = {"proxy": "source manifest absent; local hashes verified only",
              "venue": "source manifest absent; local hashes verified only"}
    if os.path.exists(source):
        src = load_json(source)["files"]
        for name, meta in manifest["files"].items():
            if src.get(name.split("/", 1)[1], {}).get("sha256") != meta["sha256"]:
                raise SystemExit(f"data/{name} no longer matches its copied-from manifest {COPIED_FROM['proxy']}")
        copied["proxy"] = f"every proxy file equals {COPIED_FROM['proxy']}"
    import offline_manifest as om
    venue = om.load_manifest(VENUE_MANIFEST_PATH, verify=True)
    vsource = os.path.join(REPO, COPIED_FROM["venue"])
    if os.path.exists(vsource):
        src = load_json(vsource)
        want = {d["coin"]: (d["candles"]["sha256"], d["funding"]["sha256"]) for d in src["datasets"]}
        for d in venue["datasets"]:
            if want.get(d["coin"]) != (d["candles"]["sha256"], d["funding"]["sha256"]):
                raise SystemExit(f"venue {d['coin']} inputs no longer match {COPIED_FROM['venue']}")
        for key in ("meta", "l2_snapshot"):
            if src["venue_reference"][key]["sha256"] != venue["venue_reference"][key]["sha256"]:
                raise SystemExit(f"venue {key} no longer matches {COPIED_FROM['venue']}")
        copied["venue"] = f"every venue file equals {COPIED_FROM['venue']}"
    return {"proxy": manifest, "venue": venue, "copied_from_check": copied}


def build_cache(cache_db: str, manifest: dict) -> None:
    if os.path.exists(cache_db):
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; the run rebuilds it from data/")
    set_paths(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    for name, meta in manifest["files"].items():
        rows = read_gz_csv(os.path.join(DATA, name))
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float,
                            "close": float, "volume": float})
            storage.store_ohlcv(df, meta["exchange"], meta["symbol"], meta["timeframe"], db_path=cache_db)
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


def grid_points(grid: dict) -> list:
    keys = list(grid)
    return [dict(zip(keys, vals)) for vals in itertools.product(*(grid[k] for k in keys))]


def label_of(params: dict) -> str:
    return (f"N={params['lookback_sessions']} kL={params['k_long']:g} kS={params['k_short']:g} "
            f"h={params['session_start_hour']}")


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


def summarize(score: dict, venue: bool) -> dict:
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
                    "per_dataset": {}, "positions_list": []})
        return out
    legs = [r["leg"] for r in rows]
    positions, per_dataset = [], {}
    for r in rows:
        leg, ex = r["leg"], r["leg"]["execution"]
        item = {
            "return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"], "trades": leg["trades"], "bh_return_pct": leg.get("bh_return_pct"),
            "net_pnl_usd": ex["net_pnl_usd"], "fees_usd": ex["fees_usd"], "funding_pnl_usd": ex["funding_pnl_usd"],
            "turnover": ex["turnover"], "traded_notional_usd": ex["traded_notional_usd"],
            "positions": ex["positions"], "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"], "bars_in_market": ex["bars_in_market"],
            "liquidated": bool(leg.get("liquidated")), "funding_incomplete": bool(leg.get("funding_incomplete")),
            "funding": funding_brief(leg), "beats_incumbent_sharpe": r["beats_sharpe"],
            "beats_incumbent_ddadj": r["beats_ddadj"],
            "long_net_pnl_usd": round(sum(p["net_pnl"] for p in ex["position_list"] if p["side"] == "long"), 6),
            "short_net_pnl_usd": round(sum(p["net_pnl"] for p in ex["position_list"] if p["side"] == "short"), 6),
            "min_entry_notional_usd": (round(min(p["entry_notional"] for p in ex["position_list"]), 4)
                                       if ex["position_list"] else None),
            "max_entry_notional_usd": (round(max(p["entry_notional"] for p in ex["position_list"]), 4)
                                       if ex["position_list"] else None),
        }
        if venue:
            item["candle_coverage"] = leg["manifest"]["candle_coverage"]
            item["funding_coverage"] = leg["manifest"]["funding_coverage"]
            item["rejected_entries"] = ex.get("rejected_entries", 0)
            item["execution_spec"] = leg["manifest"]["execution_spec"]
        per_dataset[r["dataset"]] = item
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
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
        "clusters": position_clusters(positions),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in per_dataset.values() if v["funding"]["source"] == "fetched"),
        "funding_unpriced_held_hours": sum(int(v["funding"]["unpriced_held_hours"] or 0)
                                           for v in per_dataset.values()),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
        "top5_position_abs_pnl_share": round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None,
        "min_entry_notional_usd": min((v["min_entry_notional_usd"] for v in per_dataset.values()
                                       if v["min_entry_notional_usd"] is not None), default=None),
        "coverage_complete": (all(v["candle_coverage"]["complete"] and v["funding_coverage"]["complete"]
                                  for v in per_dataset.values()) if venue else None),
        "per_dataset": per_dataset,
        "positions_list": positions,
    })
    return out


def brief(summary: dict) -> dict:
    return {k: v for k, v in summary.items() if k not in ("per_dataset", "positions_list")}


def held_brief(summary: dict) -> dict:
    out = brief(summary)
    out["per_dataset"] = summary.get("per_dataset", {})
    return out


class Study:
    def __init__(self, proto: dict, venue_manifest: dict):
        import eval_windows as ew
        from registry_loader import load_registry
        self.ew = ew
        self.proto = proto
        self.reg = load_registry(proto["registry"])
        self.capital = proto["capital_usd"]
        self.contract = copy.deepcopy(proto["contract"])
        self.cand = proto["candidate"]
        self.windows = {k: tuple(v) for k, v in proto["proxy"]["windows"].items()}
        self.symbols = proto["proxy"]["symbols"]
        self.memo = {}
        self.venue_manifest = venue_manifest
        self.venue_datasets = ew.manifest_datasets(venue_manifest)
        self.venue_memo = {}
        self.grid = proto["parameters"]["grid"]
        self.comparator_params = {c: dict(self.reg.STRATEGY_REGISTRY[c]["default_params"])
                                  for c in proto["comparators"]}

    def interval(self, group: str) -> int:
        return self.proto["parameters"]["groups"][group]["expected_interval_ms"]

    def spec(self, name: str, params: dict, interval=None) -> dict:
        p = dict(params)
        if name == self.cand:
            p["expected_interval_ms"] = interval
        spec = {"name": name, "type": self.proto["strategy_type"], "params": p}
        spec.update(copy.deepcopy(self.contract))
        return spec

    def exclusions(self, interval: int) -> tuple:
        kept, excluded = [], []
        for p in grid_points(self.grid):
            try:
                self.reg.validate_params(self.cand, dict(p, expected_interval_ms=interval))
                kept.append(p)
            except ValueError as exc:
                excluded.append({"params": p, "reason": str(exc)})
        return kept, excluded

    def proxy(self, spec: dict, group: str, wname: str) -> dict:
        ew = self.ew
        window = self.windows[wname]
        datasets = [(s, group) for s in self.symbols]
        key = (group, wname)
        if key not in self.memo:
            self.memo[key] = ew.incumbent_bars(ew.compute_incumbent_legs(
                self.reg, datasets, window, self.capital, funding_mode=self.proto["proxy"]["funding_mode"],
                contract=self.contract))
        ew.validate_candidate(spec)
        legs = {}
        for symbol, tf in datasets:
            legs[ew.dataset_key(symbol, tf)] = ew.run_candidate_leg(
                self.reg, spec, symbol, tf, window, self.capital, keep_trades=True,
                funding_mode=self.proto["proxy"]["funding_mode"])
        score = ew.score_candidate(legs, self.memo[key])
        score["bars"] = self.memo[key]
        return summarize(score, venue=False)

    def venue(self, spec: dict, wname: str, cost: float) -> dict:
        score = self.ew.evaluate_window(self.reg, spec, self.venue_datasets, wname, self.capital, self.venue_memo,
                                        manifest=self.venue_manifest, cost_multiplier=cost,
                                        incumbent_contract=self.contract)
        return summarize(score, venue=True)


def eligible(summary: dict, expected: int, rule: dict, venue: bool) -> bool:
    ok = (summary["datasets"] == expected and summary["funding_incomplete_legs"] == 0
          and summary["liquidated_legs"] == 0 and summary["funding_fetched_legs"] == 0
          and summary["clusters"] >= rule["min_training_clusters"])
    if venue:
        ok = ok and bool(summary["coverage_complete"])
    return ok


def select(rows: list, expected: int, rule: dict, venue: bool) -> dict:
    ok = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"], expected, rule, venue)]
    if not ok:
        return {"outcome": "no_eligible_combination", "params": None, "label": None, "eligible": 0}
    best_i, best = max(ok, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))
    return {"outcome": "selected", "params": dict(best["params"]), "label": best["label"], "eligible": len(ok),
            "grid_index": best_i, "mean_sharpe": best["summary"]["mean_sharpe"],
            "mean_ddadj": best["summary"]["mean_ddadj"], "clusters": best["summary"]["clusters"]}


def neighbors(params: dict, rows: list, grid: dict) -> dict:
    near = []
    for r in rows:
        steps = sum(abs(grid[k].index(params[k]) - grid[k].index(r["params"][k])) for k in grid)
        if steps == 1:
            near.append(r["summary"]["mean_sharpe"])
    return {"neighbor_count": len(near),
            "neighbor_median_sharpe": round(statistics.median(near), 4) if near else None,
            "neighbor_min_sharpe": round(min(near), 4) if near else None,
            "neighbor_positive_sharpe": sum(1 for v in near if v is not None and v > 0)}


def _rank(values: list) -> list:
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        for k in range(i, j + 1):
            ranks[order[k]] = (i + j) / 2.0
        i = j + 1
    return ranks


def spearman(a: list, b: list):
    pairs = [(x, y) for x, y in zip(a, b) if x is not None and y is not None]
    if len(pairs) < 3:
        return None
    ra, rb = _rank([p[0] for p in pairs]), _rank([p[1] for p in pairs])
    ma, mb = statistics.mean(ra), statistics.mean(rb)
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    den = (sum((x - ma) ** 2 for x in ra) * sum((y - mb) ** 2 for y in rb)) ** 0.5
    return round(num / den, 4) if den else None


def window_unscorable(arms: dict) -> list:
    bad = []
    for arm, s in sorted(arms.items()):
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


def proxy_verdict(proto: dict, held: dict, selection: dict) -> dict:
    rules = proto["sample_rules"]
    primary = proto["proxy"]["primary_window"]
    secondary = proto["proxy"]["secondary_windows"]
    unscorable = {w: window_unscorable(arms) for w, arms in held.items()}
    scorable = {w: not v for w, v in unscorable.items()}
    oos = held[primary]
    cand = oos["candidate"]
    passes = sum(1 for w in secondary if scorable[w] and held[w]["candidate"]["m1_verdict"] == "pass")
    unknown_secondary = sum(1 for w in secondary if not scorable[w])
    known = scorable[primary]

    def tri(value):
        return value if known else None

    checks = {
        "m1_oos_pass": tri(cand["m1_verdict"] == "pass"),
        "m1_secondary_passes": True if passes >= 2 else (False if passes + unknown_secondary < 2 else None),
        "oos_net_positive": tri((cand.get("net_pnl_usd") or 0.0) > 0),
        "oos_beats_breakout_sharpe": tri((cand.get("mean_sharpe") or 0.0) > (oos["breakout"].get("mean_sharpe") or 0.0)),
        "oos_beats_session_breakout_sharpe": tri(
            (cand.get("mean_sharpe") or 0.0) > (oos["session_breakout"].get("mean_sharpe") or 0.0)),
    }
    false = sorted(k for k, v in checks.items() if v is False)
    unknown = sorted(k for k, v in checks.items() if v is None)
    if selection["outcome"] != "selected":
        outcome, reason = "inconclusive", "no is combination met the frozen eligibility rule"
    elif not known:
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
    return {"outcome": outcome, "reason": reason, "checks": checks, "secondary_m1_passes": passes,
            "unscorable_windows": {w: v for w, v in unscorable.items() if v},
            "oos_clusters": cand["clusters"]}


def venue_verdict(proto: dict, held: dict, selection: dict, provenance_kind: str) -> dict:
    rules = proto["sample_rules"]
    base = {k: v["base"] for k, v in held.items()}
    stress = {k: v["stress"] for k, v in held.items()}
    evidence = (provenance_kind == "venue"
                and all(s["datasets"] == 3 and s["coverage_complete"] and s["funding_incomplete_legs"] == 0
                        and not s.get("incumbent_funding_incomplete") and not s.get("incumbent_missing")
                        for arm in held.values() for s in arm.values()))
    cand = base["candidate"]
    checks = {
        "net_positive_x1": (cand.get("net_pnl_usd") or 0.0) > 0,
        "net_positive_x2": (stress["candidate"].get("net_pnl_usd") or 0.0) > 0,
        "m1_test_pass_x1": cand["m1_verdict"] == "pass",
        "beats_breakout_sharpe_x1": (cand.get("mean_sharpe") or 0.0) > (base["breakout"].get("mean_sharpe") or 0.0),
        "beats_session_breakout_sharpe_x1": (cand.get("mean_sharpe") or 0.0) > (
            base["session_breakout"].get("mean_sharpe") or 0.0),
    }
    false = sorted(k for k, v in checks.items() if not v)
    if not evidence:
        outcome, reason = "inconclusive", "venue provenance, candle, funding or incumbent evidence is incomplete"
    elif selection["outcome"] != "selected":
        outcome, reason = "inconclusive", "no train combination met the frozen eligibility rule"
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


def verdict_guards(proto: dict, proxy_held: dict, proxy_sel: dict, venue_held: dict, venue_sel: dict) -> dict:
    out = {}
    if proxy_held:
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["funding_incomplete_legs"] = 1
        out["proxy_oos_funding_incomplete"] = proxy_verdict(proto, g, proxy_sel)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"]["breakout"]["datasets"] = 2
        out["proxy_oos_missing_comparator_dataset"] = proxy_verdict(proto, g, proxy_sel)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["clusters"] = proto["sample_rules"]["min_held_out_clusters"] - 1
        out["proxy_below_sample_floor"] = proxy_verdict(proto, g, proxy_sel)["outcome"]
        out["proxy_no_selection"] = proxy_verdict(proto, proxy_held, {"outcome": "no_eligible_combination"})["outcome"]
    if venue_held:
        g = copy.deepcopy(venue_held)
        g["candidate"]["stress"]["funding_incomplete_legs"] = 1
        out["venue_funding_incomplete"] = venue_verdict(proto, g, venue_sel, "venue")["outcome"]
        g = copy.deepcopy(venue_held)
        g["breakout"]["base"]["coverage_complete"] = False
        out["venue_coverage_incomplete"] = venue_verdict(proto, g, venue_sel, "venue")["outcome"]
        out["venue_proxy_provenance"] = venue_verdict(proto, venue_held, venue_sel, "proxy")["outcome"]
        g = copy.deepcopy(venue_held)
        g["candidate"]["base"]["clusters"] = proto["sample_rules"]["min_held_out_clusters"] - 1
        out["venue_below_sample_floor"] = venue_verdict(proto, g, venue_sel, "venue")["outcome"]
    if any(v == "pass" for v in out.values()):
        raise SystemExit(f"verdict guard failed: an incomplete or undersized group passed: {out}")
    return out


def frame_sha256(frame) -> str:
    body = frame[["open", "high", "low", "close", "volume"]].copy()
    body.insert(0, "ts", [str(t) for t in frame.index])
    return sha256_bytes(body.to_csv(index=False).encode())


def proxy_frame(symbol: str, tf: str, window: tuple):
    import pandas as pd
    from data_fetcher import load_cached_data
    start, end = window
    df = load_cached_data(symbol, tf, start_date=start, end_date=end)
    return df[df.index < pd.Timestamp(end)]


DIAG = ("dt_session_open_ms", "dt_bar_in_session", "dt_open", "dt_high", "dt_low", "dt_close", "dt_hh", "dt_lc",
        "dt_hc", "dt_ll", "dt_range", "dt_anchor", "dt_upper", "dt_lower")


def entry_audit(reg, cand: str, frames: dict, params_by_ds: dict, positions: list, slicer=None) -> list:
    import pandas as pd
    outs = {k: reg.apply_strategy(cand, f, dict(params_by_ds[k])) for k, f in frames.items()}
    rows = []
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]], outs[p["dataset"]]
        params = params_by_ds[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig = fill - 1
        rec = out.iloc[sig]
        want = 1 if p["side"] == "long" else -1
        if int(rec["signal"]) != want or not bool(rec["dt_valid"]):
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} has no valid {p['side']} signal on the "
                             "prior bar")
        per = 86_400_000 // int(params["expected_interval_ms"])
        first = sig - int(rec["dt_bar_in_session"]) - int(params["lookback_sessions"]) * per
        window = frame.iloc[first:sig + 1]
        item = {"dataset": p["dataset"], "side": p["side"], "signal_bar": str(out.index[sig]),
                "fill_bar": str(out.index[fill]), "exit_bar": p["exit_date"],
                "exit_reasons": sorted({r.split(":")[0] for r in p.get("exit_reasons") or [] if r}),
                "net_pnl_usd": p["net_pnl"], "entry_notional_usd": round(p["entry_notional"], 6),
                "reason": rec["dt_reason"], "input_first_bar": str(window.index[0]), "input_bars": int(len(window)),
                "input_sha256": frame_sha256(window)}
        for col in DIAG:
            item[col[3:]] = float(rec[col])
        rows.append(item)
    return rows


def hold_inventory(reg, cand: str, frames: dict, params_by_ds: dict) -> dict:
    out = {}
    for key, frame in sorted(frames.items()):
        res = reg.apply_strategy(cand, frame, dict(params_by_ds[key]))
        counts = res["dt_reason"].value_counts().to_dict()
        first_valid = res.index[res["dt_valid"].to_numpy()].min() if res["dt_valid"].any() else None
        out[key] = {"bars": int(len(res)), "reasons": {k: int(v) for k, v in sorted(counts.items())},
                    "first_valid_bar": str(first_valid) if first_valid is not None else None,
                    "frame_sha256": frame_sha256(frame), "first_bar": str(frame.index[0]),
                    "last_bar": str(frame.index[-1])}
    return out


def run_proxy_group(study: Study, group: str) -> dict:
    proto = study.proto
    interval = study.interval(group)
    kept, excluded = study.exclusions(interval)
    rule = proto["sample_rules"]
    sel_window = proto["proxy"]["selection_window"]
    training = []
    for p in kept:
        s = study.proxy(study.spec(study.cand, p, interval), group, sel_window)
        training.append({"label": label_of(p), "params": p, "summary": brief(s)})
    print(f"[proxy {group}] trained {len(training)} variants", flush=True)
    selection = select(training, len(study.symbols), rule, venue=False)
    chosen = selection["params"] or dict(proto["parameters"]["seed"])
    arms = {"candidate": study.spec(study.cand, chosen, interval)}
    arms.update({c: study.spec(c, study.comparator_params[c]) for c in proto["comparators"]})
    held_windows = [proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"]
    full, held = {}, {}
    for w in held_windows:
        full[w] = {k: study.proxy(a, group, w) for k, a in arms.items()}
        held[w] = {k: brief(s) for k, s in full[w].items()}
        held[w]["candidate"]["per_dataset"] = full[w]["candidate"]["per_dataset"]
        for c in proto["comparators"]:
            held[w][c]["per_dataset"] = full[w][c]["per_dataset"]
        print(f"[proxy {group}] held-out {w}: candidate {held[w]['candidate']['m1_verdict']}", flush=True)
    verdict = proxy_verdict(proto, {w: {k: v for k, v in arms_.items()} for w, arms_ in held.items()}, selection)
    variants = []
    primary = proto["proxy"]["primary_window"]
    for row in training:
        s = study.proxy(study.spec(study.cand, row["params"], interval), group, primary)
        variants.append({"label": row["label"], "params": row["params"],
                         "train_mean_sharpe": row["summary"]["mean_sharpe"],
                         "train_clusters": row["summary"]["clusters"],
                         "train_eligible": eligible(row["summary"], len(study.symbols), rule, False),
                         "oos_mean_sharpe": s["mean_sharpe"], "oos_mean_return_pct": s["mean_return_pct"],
                         "oos_net_pnl_usd": s["net_pnl_usd"], "oos_clusters": s["clusters"],
                         "oos_m1_verdict": s["m1_verdict"], "oos_funding_incomplete_legs": s["funding_incomplete_legs"]})
    print(f"[proxy {group}] scored {len(variants)} oos diagnostics", flush=True)
    frames, params_by_ds = {}, {}
    for symbol in study.symbols:
        key = study.ew.dataset_key(symbol, group)
        frames[key] = proxy_frame(symbol, group, study.windows[primary])
        params_by_ds[key] = dict(chosen, expected_interval_ms=interval)
    audit = entry_audit(study.reg, study.cand, frames, params_by_ds, full[primary]["candidate"]["positions_list"])
    holds = {}
    for w in held_windows + [sel_window]:
        wf = {study.ew.dataset_key(s, group): proxy_frame(s, group, study.windows[w]) for s in study.symbols}
        holds[w] = hold_inventory(study.reg, study.cand, wf, params_by_ds)
    return {
        "expected_interval_ms": interval,
        "grid_size": len(kept) + len(excluded),
        "excluded": excluded,
        "training": training,
        "selection": selection,
        "selected_params": chosen,
        "comparator_params": study.comparator_params,
        "held_out": held,
        "verdict": verdict,
        "variants_oos": variants,
        "stability": {
            "spearman_train_vs_oos_sharpe": spearman([v["train_mean_sharpe"] for v in variants],
                                                     [v["oos_mean_sharpe"] for v in variants]),
            "oos_net_positive_variants": sum(1 for v in variants if (v["oos_net_pnl_usd"] or 0) > 0),
            "oos_m1_pass_variants": sum(1 for v in variants if v["oos_m1_verdict"] == "pass"),
            "variants": len(variants),
            "selected_neighbors_train": neighbors(chosen, training, study.grid) if selection["params"] else None,
        },
        "entry_audit": audit,
        "hold_inventory": holds,
    }


def run_venue_group(study: Study) -> dict:
    proto = study.proto
    interval = study.interval(proto["venue"]["interval"])
    kept, excluded = study.exclusions(interval)
    rule = proto["sample_rules"]
    costs = proto["venue"]["cost_multipliers"]
    sel_window, held_window = proto["venue"]["selection_window"], proto["venue"]["held_out_window"]
    training = []
    for p in kept:
        s = study.venue(study.spec(study.cand, p, interval), sel_window, costs["base"])
        training.append({"label": label_of(p), "params": p, "summary": brief(s)})
    print(f"[venue] trained {len(training)} variants", flush=True)
    selection = select(training, len(study.venue_datasets), rule, venue=True)
    chosen = selection["params"] or dict(proto["parameters"]["seed"])
    arms = {"candidate": study.spec(study.cand, chosen, interval)}
    arms.update({c: study.spec(c, study.comparator_params[c]) for c in proto["comparators"]})
    full = {k: {label: study.venue(a, held_window, mult) for label, mult in costs.items()} for k, a in arms.items()}
    held = {k: {label: brief(s) for label, s in v.items()} for k, v in full.items()}
    for k in held:
        held[k]["base"]["per_dataset"] = full[k]["base"]["per_dataset"]
    verdict = venue_verdict(proto, held, selection, study.venue_manifest["provenance"]["kind"])
    variants = []
    for row in training:
        s = study.venue(study.spec(study.cand, row["params"], interval), held_window, costs["base"])
        variants.append({"label": row["label"], "params": row["params"],
                         "train_mean_sharpe": row["summary"]["mean_sharpe"],
                         "train_clusters": row["summary"]["clusters"],
                         "train_eligible": eligible(row["summary"], len(study.venue_datasets), rule, True),
                         "test_mean_sharpe": s["mean_sharpe"], "test_mean_return_pct": s["mean_return_pct"],
                         "test_net_pnl_usd": s["net_pnl_usd"], "test_clusters": s["clusters"],
                         "test_m1_verdict": s["m1_verdict"]})
    print(f"[venue] scored {len(variants)} test diagnostics", flush=True)
    import offline_manifest as om
    frames, params_by_ds = {}, {}
    for ds in study.venue_manifest["datasets"]:
        frame, _, _ = om.window_frame(study.venue_manifest, ds, held_window)
        frames[ds["key"]] = frame
        params_by_ds[ds["key"]] = dict(chosen, expected_interval_ms=interval)
    audit = entry_audit(study.reg, study.cand, frames, params_by_ds, full["candidate"]["base"]["positions_list"])
    holds = {}
    for w in (sel_window, held_window):
        wf = {}
        for ds in study.venue_manifest["datasets"]:
            frame, win, _ = om.window_frame(study.venue_manifest, ds, w)
            wf[ds["key"]] = frame
        holds[w] = hold_inventory(study.reg, study.cand, wf, params_by_ds)
    return {
        "expected_interval_ms": interval,
        "grid_size": len(kept) + len(excluded),
        "excluded": excluded,
        "training": training,
        "selection": selection,
        "selected_params": chosen,
        "comparator_params": study.comparator_params,
        "held_out": held,
        "verdict": verdict,
        "variants_test": variants,
        "stability": {
            "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in variants],
                                                      [v["test_mean_sharpe"] for v in variants]),
            "test_net_positive_variants": sum(1 for v in variants if (v["test_net_pnl_usd"] or 0) > 0),
            "test_m1_pass_variants": sum(1 for v in variants if v["test_m1_verdict"] == "pass"),
            "variants": len(variants),
            "selected_neighbors_train": neighbors(chosen, training, study.grid) if selection["params"] else None,
        },
        "entry_audit": audit,
        "hold_inventory": holds,
    }


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
            "capital_usd": proto["capital_usd"],
            "intrabar_resolution": proto["intrabar_resolution"],
            "sizing_defaults": defaults,
            "leverage": getattr(probe, "leverage", None)}


def run(cache_db: str, groups=None) -> None:
    proto = load_json(PROTOCOL_PATH)
    held_lock = require_lock()
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    study = Study(proto, frozen["venue"])
    started = time.time()
    out = {
        "study": proto["study"], "issue": proto["issue"], "tracking_group": proto["tracking_group"],
        "protocol_sha256": protocol_sha256(), "protocol_lock": held_lock,
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
    wanted = groups or ["proxy_1h", "proxy_4h", "venue_4h"]
    for g in wanted:
        if g.startswith("proxy_"):
            out["groups"][g] = run_proxy_group(study, g.split("_", 1)[1])
        else:
            out["groups"][g] = run_venue_group(study)
        print(f"[{g}] verdict {out['groups'][g]['verdict']['outcome']}: {out['groups'][g]['verdict']['reason']}",
              flush=True)
    out["verdict"] = overall_verdict({g: v["verdict"] for g, v in out["groups"].items()})
    out["verdict_guards"] = {g: verdict_guards(proto, v["held_out"] if g.startswith("proxy_") else {},
                                               v["selection"], v["held_out"] if g.startswith("venue_") else {},
                                               v["selection"])
                             for g, v in out["groups"].items()}
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']['outcome']}; wrote {RESULTS_PATH}")


def parity(cache_db: str) -> None:
    proto = load_json(PROTOCOL_PATH)
    require_lock()
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    import pandas as pd
    import offline_manifest as om
    from parity_diff import compute_parity_frame, config_from_live_config, summarize as parity_summarize
    results = load_json(RESULTS_PATH) if os.path.exists(RESULTS_PATH) else None
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "venue_manifest_sha256": sha256_file(VENUE_MANIFEST_PATH),
           "source_commit": git_head(), "runs": []}
    sources = []
    for tf in ("1h", "4h"):
        start, end = PARITY_SPANS[tf]
        df = proxy_frame("BTC/USDT", tf, (start, end))
        selected = (results or {}).get("groups", {}).get(f"proxy_{tf}", {}).get("selected_params")
        sources.append((tf, f"proxy BTC/USDT {tf} {start}..{end}", df, selected))
    vds = om.dataset_by_key(frozen["venue"], "BTC 4h")
    vframe, _, _ = om.window_frame(frozen["venue"], vds, proto["venue"]["held_out_window"])
    selected = (results or {}).get("groups", {}).get("venue_4h", {}).get("selected_params")
    sources.append(("4h", "venue BTC 4h test window with warmup", vframe, selected))
    for tf, label, df, selected in sources:
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        sid = load_json(path)["strategies"][0]["id"]
        param_sets = {"seed": None}
        if selected:
            param_sets["selected"] = dict(selected, expected_interval_ms=proto["parameters"]["groups"][tf][
                "expected_interval_ms"])
        for plabel, override in param_sets.items():
            for batched in (False, True):
                for window in PARITY_WINDOWS:
                    cfg = config_from_live_config(path, sid, comparison_mode=proto["contract"]["comparison_mode"])
                    cfg.batched = batched
                    cfg.funding_mode = proto["proxy"]["funding_mode"]
                    if override is not None:
                        cfg.params = dict(override)
                    t0 = time.time()
                    frame = compute_parity_frame(df, cfg=cfg, window=window)
                    summ = parity_summarize(frame)
                    row = {"source": label, "config": os.path.basename(path), "strategy_id": sid,
                           "params_label": plabel, "params": cfg.params, "close_refs": cfg.close_refs,
                           "direction": cfg.direction, "stop_kwargs": cfg.stop_kwargs, "batched": batched,
                           "window": "every prefix" if window is None else window,
                           "input_sha256": frame_sha256(df), "candles": int(len(df)),
                           "bars_compared": summ["bars_compared"], "mismatches": summ["mismatches"],
                           "batch_mismatches": summ.get("batch_mismatches"),
                           "decision_agreement": summ["decision_agreement"], "close_parity": summ["close_parity"],
                           "clean": summ["clean"],
                           "live_long_signals": int((frame["live_signal"] == 1).sum()),
                           "live_short_signals": int((frame["live_signal"] == -1).sum()),
                           "seconds": round(time.time() - t0, 1)}
                    out["runs"].append(row)
                    print(json.dumps({k: row[k] for k in ("source", "params_label", "batched", "window",
                                                          "bars_compared", "mismatches", "batch_mismatches",
                                                          "clean")}), flush=True)
    out["clean"] = all(r["clean"] and r["bars_compared"] > 0 and not r["batch_mismatches"] for r in out["runs"])
    write_json(PARITY_PATH, out)
    print(f"parity clean: {out['clean']}; wrote {PARITY_PATH}")


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


def _arm_table(arms: dict, title_col: str) -> list:
    lines = [f"| {title_col} | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) "
             "| clusters | turnover | funding $ | incomplete legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, s in arms.items():
        lines.append(f"| {name} | {s.get('m1_verdict')} | {fmt(s.get('mean_sharpe'))} | {fmt(s.get('mean_return_pct'))} "
                     f"| {fmt(s.get('net_pnl_usd'))} | {fmt(s.get('worst_max_dd_pct'))} | {s.get('positions')} "
                     f"({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('clusters')} "
                     f"| {fmt(s.get('mean_turnover'))} | {fmt(s.get('funding_pnl_usd'))} "
                     f"| {s.get('funding_incomplete_legs')} |")
    return lines


def _variant_table(variants: list, held: str) -> list:
    k = "oos" if held == "oos" else "test"
    lines = [f"| variant | train Sharpe | train clusters | eligible | {k} Sharpe | {k} return % | {k} net $ "
             f"| {k} clusters | {k} M1 |", "|---|---|---|---|---|---|---|---|---|"]
    for v in variants:
        lines.append(f"| {v['label']} | {fmt(v['train_mean_sharpe'])} | {v['train_clusters']} "
                     f"| {fmt(v['train_eligible'])} | {fmt(v[k + '_mean_sharpe'])} | {fmt(v[k + '_mean_return_pct'])} "
                     f"| {fmt(v[k + '_net_pnl_usd'])} | {v[k + '_clusters']} | {v[k + '_m1_verdict']} |")
    return lines


def _concentration(s: dict) -> str:
    return (f"top dataset share of absolute net PnL {fmt(s.get('top_dataset_abs_pnl_share'))}, top five positions' "
            f"share {fmt(s.get('top5_position_abs_pnl_share'))}, long net ${fmt(s.get('long_net_pnl_usd'))}, short "
            f"net ${fmt(s.get('short_net_pnl_usd'))}")


def _holds_lines(holds: dict) -> list:
    lines = ["| window | dataset | bars | first valid bar | hold reasons |", "|---|---|---|---|---|"]
    for w, per in holds.items():
        for ds, h in per.items():
            reasons = ", ".join(f"{k} {v}" for k, v in h["reasons"].items()
                                if k not in ("inside_range", "entry_long", "entry_short", "long_consumed",
                                             "short_consumed", "threshold_equal"))
            lines.append(f"| {w} | {ds} | {h['bars']} | {h['first_valid_bar']} | {reasons or '-'} |")
    return lines


def _funding_lines(per_dataset: dict) -> list:
    lines = ["| dataset | mode | source | complete | expected h | priced h | missing h | venue-absent h "
             "| unpriced held h | funding sha256 |", "|---|---|---|---|---|---|---|---|---|---|"]
    for ds, v in per_dataset.items():
        f = v["funding"]
        lines.append(f"| {ds} | {f['mode']} | {f['source']} | {fmt(f['complete'])} | {f['expected_hours']} "
                     f"| {f['present_hours']} | {f['missing_hours']} | {f['venue_absent_hours']} "
                     f"| {f['unpriced_held_hours']} | `{(f['sha256'] or '-')[:16]}` |")
    return lines


def render() -> None:
    proto = load_json(PROTOCOL_PATH)
    res = load_json(RESULTS_PATH)
    par = load_json(PARITY_PATH) if os.path.exists(PARITY_PATH) else None
    manifest = load_json(MANIFEST_PATH)
    v = res["verdict"]
    L = []
    L.append("# Dual Thrust study (issue 1642)")
    L.append("")
    L.append(f"**Verdict: {v['outcome'].upper()}** (proxy 1h: {v['groups'].get('proxy_1h')}, proxy 4h: "
             f"{v['groups'].get('proxy_4h')}, venue 4h: {v['groups'].get('venue_4h')}). Registry metadata after this "
             f"study: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`, `edge_ref` = this file. The "
             "candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs "
             "`allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study "
             "is suggest-only and sends no order.")
    L.append("")
    L.append("## Signal")
    L.append("")
    L.append("`dual_thrust` (`shared_strategies/open/dual_thrust.py`) works on UTC daily sessions that start at "
             "`session_start_hour`. At each session boundary it freezes HH (highest high), LL (lowest low), HC "
             "(highest daily close) and LC (lowest daily close) over the immediately preceding `lookback_sessions` "
             "complete sessions, sets range = max(HH - LC, HC - LL), and anchors on the current session's opening "
             "candle open. It enters long on the first closed bar of the session whose close is strictly above "
             "anchor + `k_long` x range and short on the first closed bar strictly below anchor - `k_short` x range; "
             "each side fires at most once per session, and a crossing consumes its event even when a position or a "
             "gate suppresses the entry. A close equal to a threshold fires nothing. Every candle of the prior "
             "sessions and of the current session through the decision bar must be present, exactly "
             "`expected_interval_ms` apart, on the UTC grid and well formed; otherwise the entry holds with a "
             "reason (`dt_reason`) and close and stop management continue. The evaluator reads only the supplied "
             "frame and keeps no state, so every decision is reconstructed from its closed-bar prefix.")
    L.append("")
    L.append("## Protocol")
    L.append("")
    lock_ = res["protocol_lock"]
    L.append(f"`protocol.json` (sha256 `{res['protocol_sha256']}`) was locked by `run_study.py lock` at "
             f"{lock_['locked_at_utc']} (commit `{lock_['source_commit']}`), before any window was scored; `run` "
             "and `parity` refuse a different protocol. It fixes the parameter bounds, the 80-point grid, the "
             "matched contract, the selection rule, the sample floors and the pass/fail/inconclusive rules.")
    L.append("")
    c = proto["contract"]
    L.append(f"Contract for the candidate, `breakout`, `session_breakout` and the M1 incumbent median: direction "
             f"`{c['direction']}`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop "
             f"(`stop_loss_atr_mult: {c['stop_loss_atr_mult']}`), `comparison_mode: {c['comparison_mode']}`, "
             f"`intrabar_resolution: {proto['intrabar_resolution']}`, capital ${proto['capital_usd']:.0f} per leg. "
             "Comparators keep their registry defaults: "
             + "; ".join(f"`{k}` {json.dumps(p, sort_keys=True)}" for k, p in
                         res["groups"][next(iter(res["groups"]))]["comparator_params"].items()) + ".")
    L.append("")
    L.append("Selection (per group, candidate only, on the selection window): " + proto["selection_rule"]["eligible"]
             + "; objective " + proto["selection_rule"]["objective"] + ". Proxy group rule: "
             + proto["verdict_rules"]["proxy_group"]["order"] + ". Venue group rule: "
             + proto["verdict_rules"]["venue_group"]["order"] + ". Overall: " + proto["verdict_rules"]["overall"] + ".")
    L.append("")
    L.append("| proxy window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    roles = {"is": "selection", "oos": "primary held-out"}
    for w, (s, e) in proto["proxy"]["windows"].items():
        L.append(f"| {w} | {s} | {e} | {roles.get(w, 'secondary held-out')} |")
    L.append("")
    L.append("Departures from the default M1 windows: " + "; ".join(proto["proxy"]["departures_from_m1_windows"]) + ".")
    L.append("")
    vm = res["venue_manifest"]
    L.append("| venue window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    for w, meta in vm["windows"].items():
        L.append(f"| {w} | {meta['start']} | {meta['end']} | {meta['role']} |")
    L.append("")
    L.append("## Inputs, costs and assumptions")
    L.append("")
    eng = res["engine"]
    L.append("- Proxy candles: Binance US spot USDT candles stand in for Hyperliquid perpetual prices, so this is "
             "proxy evidence and not measured Hyperliquid candle evidence. The inputs are byte-identical copies of "
             f"the `chande_momentum_1661` frozen files ({res['copied_from_check']['proxy']}).")
    L.append(f"- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, "
             f"`eval_windows.run_leg(funding_mode=\"charge\")`) for every candidate, comparator and incumbent leg.")
    L.append(f"- Proxy costs: engine Hyperliquid commission `{eng['commission_pct']}` per side and engine slippage "
             f"`{eng['slippage_pct']}` per fill. This legacy M1 screen replays no spread, lot rounding or minimum "
             "order, so it cannot by itself establish venue execution parity.")
    L.append(f"- Venue candles: {vm['provenance']['label']} ({res['copied_from_check']['venue']}), interval "
             f"{vm['interval']}, {vm['warmup_bars']} warm-up bars read by the evaluator but never scored or traded.")
    costs = vm["costs"]
    L.append(f"- Venue costs (manifest execution specification with the explicit close owner): taker fee "
             f"{costs['taker_fee_pct']}, maker fee {costs['maker_fee_pct']}, slippage {costs['slippage_bps']} bps per "
             f"side plus the per-coin half spread, szDecimals lot rounding, minimum notional "
             f"${costs['min_notional_usd']} with a {costs['min_notional_margin']} margin; cost x2 doubles spread and "
             "slippage. Funding uses the manifest's existing attachment and coverage semantics.")
    L.append("")
    L.append("| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |")
    L.append("|---|---|---|---|---|")
    for d in vm["datasets"]:
        L.append(f"| {d['key']} | {d['size_decimals']} | {d['half_spread_bps']} | `{d['candles_sha256']}` "
                 f"| `{d['funding_sha256']}` |")
    L.append("")
    notionals = [item[k] for gname, g in res["groups"].items()
                 for arms in ([g["held_out"]["candidate"]["base"]] if gname.startswith("venue") else
                              [w["candidate"] for w in g["held_out"].values()])
                 for item in arms.get("per_dataset", {}).values()
                 for k in ("min_entry_notional_usd", "max_entry_notional_usd") if item.get(k) is not None]
    L.append(f"- Sizing: capital ${eng['capital_usd']:.0f} per leg, no leverage and no perps sizing block (Backtester "
             f"leverage {eng['leverage']}, sizing defaults {json.dumps(eng['sizing_defaults'], sort_keys=True)}), so "
             "each entry commits the leg's whole available cash (engine entry fraction 1.0) unlevered; held-out candidate "
             "entry notionals ranged from "
             f"${min(notionals):.2f} to ${max(notionals):.2f}.")
    L.append(f"- Stop: one fixed entry-ATR stop at {c['stop_loss_atr_mult']} x ATR. Close: one `tiered_tp_pct` "
             "tier at +3%.")
    L.append("")
    L.append("| proxy file | rows | sha256 |")
    L.append("|---|---|---|")
    for name, meta in manifest["files"].items():
        L.append(f"| `data/{name}` | {meta['rows']} | `{meta['sha256']}` |")
    L.append("")
    for gname, g in res["groups"].items():
        venue = gname.startswith("venue")
        L.append(f"## {gname} (expected_interval_ms = {g['expected_interval_ms']})")
        L.append("")
        ver = g["verdict"]
        L.append(f"**Group verdict: {ver['outcome']}** ({ver['reason']}).")
        L.append("")
        L.append("| check | value |")
        L.append("|---|---|")
        for k, val in ver["checks"].items():
            L.append(f"| {k} | {tri(val)} |")
        L.append("")
        sel = g["selection"]
        L.append(f"Selection: {sel['outcome']}" + (f", `{sel['label']}` (mean Sharpe {fmt(sel['mean_sharpe'])}, mean "
                                                    f"DDadj {fmt(sel['mean_ddadj'])}, {sel['clusters']} clusters; "
                                                    f"{sel['eligible']} of {len(g['training'])} variants eligible)"
                                                    if sel["params"] else "; the seed is scored for the record only")
                 + f". Grid points: {g['grid_size']}; excluded before scoring: {len(g['excluded'])}"
                 + ("" if not g["excluded"] else " (" + "; ".join(
                     f"{label_of(e['params'])}: {e['reason']}" for e in g["excluded"]) + ")") + ".")
        L.append("")
        if venue:
            for label in ("base", "stress"):
                L.append(f"### Held-out `test`, cost {label}")
                L.append("")
                L.extend(_arm_table({k: v_[label] for k, v_ in g["held_out"].items()}, "arm"))
                L.append("")
            cand = g["held_out"]["candidate"]["base"]
            L.append("Concentration (candidate, cost x1): " + _concentration(cand) + ".")
            L.append("")
            L.append("Coverage and execution (candidate, cost x1):")
            L.append("")
            L.append("| dataset | candles complete | funding complete | rejected entries | min entry notional $ |")
            L.append("|---|---|---|---|---|")
            for ds, item in cand["per_dataset"].items():
                L.append(f"| {ds} | {fmt(item['candle_coverage']['complete'])} "
                         f"| {fmt(item['funding_coverage']['complete'])} | {item['rejected_entries']} "
                         f"| {fmt(item['min_entry_notional_usd'])} |")
            L.append("")
        else:
            for w, arms in g["held_out"].items():
                L.append(f"### Held-out `{w}`")
                L.append("")
                L.extend(_arm_table(arms, "arm"))
                L.append("")
            cand = g["held_out"]["oos"]["candidate"]
            L.append("Concentration (candidate, oos): " + _concentration(cand) + ".")
            L.append("")
            if ver.get("unscorable_windows"):
                L.append("Unscorable windows: " + "; ".join(f"{w}: {', '.join(r)}"
                                                           for w, r in ver["unscorable_windows"].items()) + ".")
                L.append("")
            L.append("Funding coverage and provenance (candidate legs, oos):")
            L.append("")
            L.extend(_funding_lines(cand["per_dataset"]))
            L.append("")
        st = g["stability"]
        nb = st.get("selected_neighbors_train") or {}
        held_key = "test" if venue else "oos"
        L.append(f"Stability: Spearman rank correlation of train vs {held_key} mean Sharpe across the "
                 f"{st['variants']} variants: {fmt(st.get('spearman_train_vs_' + held_key + '_sharpe'))}; variants "
                 f"with positive {held_key} net PnL: {st[held_key + '_net_positive_variants']}; variants with an M1 "
                 f"pass on {held_key}: {st[held_key + '_m1_pass_variants']}; one-step grid neighbours of the "
                 f"selection: {nb.get('neighbor_count', '-')} (median train Sharpe "
                 f"{fmt(nb.get('neighbor_median_sharpe'))}, minimum {fmt(nb.get('neighbor_min_sharpe'))}).")
        L.append("")
        L.append("Start-of-window history and input holds for the selected parameters (proxy windows load no "
                 "earlier bars; venue windows include the manifest warm-up):")
        L.append("")
        L.extend(_holds_lines(g["hold_inventory"]))
        L.append("")
        audit = g["entry_audit"]
        L.append(f"Entry audit: {len(audit)} held-out candidate entries; each fill follows a valid raw signal on the "
                 "prior closed bar, and `results.json` stores its evaluated input span hash and every indicator "
                 "value. First entries:")
        L.append("")
        L.append("| dataset | side | signal bar | fill bar | anchor | range | upper | lower | close | net $ |")
        L.append("|---|---|---|---|---|---|---|---|---|---|")
        for a in audit[:10]:
            L.append(f"| {a['dataset']} | {a['side']} | {a['signal_bar']} | {a['fill_bar']} | {a['anchor']:.6g} "
                     f"| {a['range']:.6g} | {a['upper']:.6g} | {a['lower']:.6g} | {a['close']:.6g} "
                     f"| {fmt(a['net_pnl_usd'])} |")
        L.append("")
        L.append("<details><summary>Every evaluated variant (training window and held-out diagnostic)</summary>")
        L.append("")
        L.extend(_variant_table(g["variants_test"] if venue else g["variants_oos"], held_key))
        L.append("")
        L.append("</details>")
        L.append("")
    L.append("## Decision parity")
    L.append("")
    if par:
        L.append(f"`run_study.py parity` ran the explicit-paper configurations `paper_parity_1h.json` and "
                 f"`paper_parity_4h.json` through `backtest/parity_diff.compute_parity_frame` on frozen BTC frames: "
                 f"non-batched and batched (solo vs batch slots through the real Hyperliquid check evaluator), on the "
                 f"trailing 200-bar window and on every prefix. Overall clean: {fmt(par['clean'])}.")
        L.append("")
        L.append("| source | params | batched | window | bars compared | mismatches | batch mismatches | long/short "
                 "signals | clean |")
        L.append("|---|---|---|---|---|---|---|---|---|")
        for r in par["runs"]:
            L.append(f"| {r['source']} | {r['params_label']} | {fmt(r['batched'])} | {r['window']} "
                     f"| {r['bars_compared']} | {r['mismatches']} | {fmt(r['batch_mismatches'])} "
                     f"| {r['live_long_signals']}/{r['live_short_signals']} | {fmt(r['clean'])} |")
        L.append("")
    else:
        L.append("Parity has not been run for this results file.")
        L.append("")
    L.append("## Limits")
    L.append("")
    L.append("- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not "
             "modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution "
             "claims rest on the venue group only.")
    L.append("- Hyperliquid funding starts in May 2023; a proxy 2023 leg that holds a position before the first "
             "rate is funding_incomplete, and such a window can never support a pass.")
    L.append("- The venue windows cover 2024-09-01 to 2026-09-01 only (the venue serves the most recent 5,000 "
             "candles). Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; queue "
             "position, partial fills and market impact are not modelled.")
    L.append("- The selection and the verdict read only frozen inputs with network acquisition disabled. A "
             "research result alone cannot approve deployment.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append("uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py lock")
    L.append("uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py run --cache-db /tmp/dt1642_run.db")
    L.append("uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py parity --cache-db /tmp/dt1642_parity.db")
    L.append("uv run --no-sync python backtest/candidates/dual_thrust_1642/run_study.py render")
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
    p = argparse.ArgumentParser(description="#1642 Dual Thrust study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("lock", help="record protocol.json's sha256 before any score")
    sub.add_parser("hash", help="print protocol.json's sha256")
    r = sub.add_parser("run", help="rebuild a fresh cache from data/ and run the frozen protocol offline")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--group", action="append", choices=["proxy_1h", "proxy_4h", "venue_4h"])
    q = sub.add_parser("parity", help="decision parity of the paper configurations on frozen BTC frames")
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
