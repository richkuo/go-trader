#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
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
VENUE_MANIFEST_PATH = os.path.abspath(os.path.join(HERE, "..", "mass_index_reversal_1670_manifest.json"))
VENUE_SOURCE_MANIFEST = "backtest/candidates/dual_thrust_1642/venue_manifest.json"
RESULTS_PATH = os.path.join(HERE, "results.json")
PARITY_PATH = os.path.join(HERE, "parity_results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
for _p in (os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
FILTER_OFF = "mass_index_reversal_filter_off"
MRP = "mean_reversion_pro"
ARMS = ("candidate", FILTER_OFF, MRP)
COMPARATOR_CHECK_KEYS = {FILTER_OFF: "filter_off", MRP: "mean_reversion_pro"}
SOURCE_FILES = (
    "shared_strategies/open/mass_index_reversal.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/open/mean_reversion_pro.py",
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
    "backtest/candidates/mass_index_reversal_1670/run_study.py",
    "backtest/candidates/mass_index_reversal_1670/protocol.json",
    "backtest/candidates/mass_index_reversal_1670/study_manifest.json",
    "backtest/candidates/mass_index_reversal_1670_manifest.json",
)
PARITY_BARS = 2200
PARITY_WINDOWS = (200, None)
RAW_TAILS = ("required", 199, 200)
RAW_COLUMNS = ("signal", "mir_valid", "mir_reason", "mir_eval_ts_ms", "mir_range", "mir_ema1", "mir_ema2", "mir_ratio",
               "mir_mass_index", "mir_prev_mass_index", "mir_trend_sma", "mir_trend_slope", "mir_upper_cross",
               "mir_lower_cross", "mir_bulge_armed_ts_ms", "mir_bulge_age", "mir_bulge_slope", "mir_bulge_direction",
               "mir_bulge_completed", "mir_tested_side", "mir_confirmed", "mir_filtered", "mir_close",
               "mir_confirm_high", "mir_confirm_low")


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
    check_grid(load_json(PROTOCOL_PATH))
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


def grid() -> dict:
    from optimizer import DEFAULT_PARAM_RANGES
    return copy.deepcopy(DEFAULT_PARAM_RANGES["mass_index_reversal"])


def check_grid(proto: dict) -> dict:
    from optimizer import generate_param_grid
    from mass_index_reversal import mir_param_validator, required_history_bars, DEFAULT_PARAMS
    seed = proto["seed_params"]
    if seed != DEFAULT_PARAMS:
        raise SystemExit(f"protocol seed {seed} differs from the registry defaults {DEFAULT_PARAMS}")
    g = grid()
    for key, value in seed.items():
        if value not in g[key]:
            raise SystemExit(f"seed {key}={value} is not in the optimizer grid {g[key]}")
    if g["bulge_filter"] != [1]:
        raise SystemExit("the optimizer grid must fix bulge_filter at 1")
    combos = generate_param_grid(g)
    worst = 0
    for combo in combos:
        mir_param_validator(combo)
        worst = max(worst, required_history_bars(combo["sum_period"], combo["trend_period"], combo["slope_span"],
                                                 combo["confirm_lookback"], combo["max_bulge_age"],
                                                 combo["recursion_length"]))
    need = required_history_bars(seed["sum_period"], seed["trend_period"], seed["slope_span"],
                                 seed["confirm_lookback"], seed["max_bulge_age"], seed["recursion_length"])
    if need != proto["required_history_bars"]:
        raise SystemExit(f"seed required history {need} != protocol {proto['required_history_bars']}")
    return {"combinations": len(combos), "all_valid": True, "worst_required_history": worst,
            "seed_required_history": need}


def neighbors(seed: dict) -> list:
    from mass_index_reversal import mir_param_validator
    out = []
    for key, values in grid().items():
        ordered = sorted(values)
        pos = ordered.index(seed[key])
        for j in (pos - 1, pos + 1):
            if 0 <= j < len(ordered):
                params = dict(seed, **{key: ordered[j]})
                mir_param_validator(params)
                out.append({"param": key, "value": ordered[j], "params": params})
    return out


def proxy_data_root(manifest: dict) -> str:
    return os.path.join(REPO, manifest["data_root"])


def verify_frozen() -> dict:
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


def build_cache(cache_db: str, manifest: dict) -> None:
    if os.path.exists(cache_db):
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; the run rebuilds it from the frozen files")
    set_paths(cache_db)
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


def cluster_pnls(positions: list) -> list:
    import pandas as pd
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"]), float(p["net_pnl"]))
                   for p in positions)
    out, end = [], None
    for s, e, pnl in spans:
        if end is None or s > end:
            out.append(pnl)
            end = e
        else:
            out[-1] += pnl
            end = max(end, e)
    return [round(x, 6) for x in out]


def concentration(clusters: list) -> dict:
    positive = [x for x in clusters if x > 0]
    total = sum(positive)
    share = (max(positive) / total) if total > 0 else None
    return {"largest_positive_cluster_usd": round(max(positive), 6) if positive else None,
            "positive_cluster_sum_usd": round(total, 6), "largest_positive_cluster_share":
                round(share, 6) if share is not None else None,
            "ok": bool(positive) and share <= 0.5}


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
        out.update({"positions": 0, "clusters": 0, "cluster_pnls": [], "concentration": concentration([]),
                    "funding_incomplete_legs": 0, "liquidated_legs": 0, "funding_fetched_legs": 0,
                    "coverage_complete": False if venue else None, "net_pnl_usd": 0.0, "mean_sharpe": None,
                    "mean_ddadj": None, "mean_return_pct": None, "per_dataset": {}, "positions_list": []})
        return out
    legs = [r["leg"] for r in rows]
    positions, per_dataset = [], {}
    for r in rows:
        leg, ex = r["leg"], r["leg"]["execution"]
        plist = ex["position_list"]
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
            "long_net_pnl_usd": round(sum(p["net_pnl"] for p in plist if p["side"] == "long"), 6),
            "short_net_pnl_usd": round(sum(p["net_pnl"] for p in plist if p["side"] == "short"), 6),
            "min_entry_notional_usd": round(min(p["entry_notional"] for p in plist), 4) if plist else None,
            "max_entry_notional_usd": round(max(p["entry_notional"] for p in plist), 4) if plist else None,
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
    clusters = cluster_pnls(positions)
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
        "clusters": len(clusters),
        "cluster_pnls": clusters,
        "concentration": concentration(clusters),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in per_dataset.values() if v["funding"]["source"] == "fetched"),
        "funding_unpriced_held_hours": sum(int(v["funding"]["unpriced_held_hours"] or 0)
                                           for v in per_dataset.values()),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
        "min_entry_notional_usd": min((v["min_entry_notional_usd"] for v in per_dataset.values()
                                       if v["min_entry_notional_usd"] is not None), default=None),
        "max_entry_notional_usd": max((v["max_entry_notional_usd"] for v in per_dataset.values()
                                       if v["max_entry_notional_usd"] is not None), default=None),
        "coverage_complete": (all(v["candle_coverage"]["complete"] and v["funding_coverage"]["complete"]
                                  for v in per_dataset.values()) if venue else None),
        "rejected_entries": sum(int(v.get("rejected_entries") or 0) for v in per_dataset.values()) if venue else None,
        "per_dataset": per_dataset,
        "positions_list": positions,
    })
    return out


def brief(summary: dict) -> dict:
    return {k: v for k, v in summary.items() if k not in ("per_dataset", "positions_list", "cluster_pnls")}


class Study:
    def __init__(self, proto: dict, proxy_manifest: dict, venue_manifest: dict):
        import eval_windows as ew
        from registry_loader import load_registry
        self.ew = ew
        self.proto = proto
        self.reg = load_registry(proto["registry"])
        self.capital = proto["capital_usd"]
        self.contract = copy.deepcopy(proto["contract"])
        self.cand = proto["candidate"]
        self.seed = dict(proto["seed_params"])
        self.windows = {k: tuple(v) for k, v in proto["proxy"]["windows"].items()}
        self.symbols = proto["proxy"]["symbols"]
        self.memo = {}
        self.proxy_manifest = proxy_manifest
        self.venue_manifest = venue_manifest
        self.venue_datasets = ew.manifest_datasets(venue_manifest)
        self.venue_memo = {}
        self.comparator_params = {MRP: dict(self.reg.STRATEGY_REGISTRY[MRP]["default_params"]),
                                  FILTER_OFF: dict(self.seed, bulge_filter=0)}

    def spec_for(self, name: str, params: dict) -> dict:
        spec = {"name": name, "type": self.proto["strategy_type"], "params": dict(params)}
        spec.update(copy.deepcopy(self.contract))
        return spec

    def arm_spec(self, arm: str) -> dict:
        if arm == "candidate":
            return self.spec_for(self.cand, self.seed)
        if arm == FILTER_OFF:
            return self.spec_for(self.cand, self.comparator_params[FILTER_OFF])
        return self.spec_for(MRP, self.comparator_params[MRP])

    def warmup_for(self, tf: str, wname: str) -> int:
        import pandas as pd
        from closed_bar import FIXED_DURATION_MS
        want = int(self.proto["warmup"]["bars"])
        start_ms = int(pd.Timestamp(self.windows[wname][0], tz="UTC").value // 1_000_000)
        first = min(meta["first_ms"] for meta in self.proxy_manifest["files"].values()
                    if meta["kind"] == "candles" and meta["timeframe"] == tf)
        return want if start_ms - want * FIXED_DURATION_MS[tf] >= first else 0

    def proxy(self, spec: dict, tf: str, wname: str) -> dict:
        ew = self.ew
        window = self.windows[wname]
        warmup = self.warmup_for(tf, wname)
        datasets = [(s, tf) for s in self.symbols]
        key = (tf, wname)
        if key not in self.memo:
            self.memo[key] = ew.incumbent_bars(ew.compute_incumbent_legs(
                self.reg, datasets, window, self.capital, funding_mode=self.proto["proxy"]["funding_mode"],
                contract=self.contract, warmup_bars=warmup))
        ew.validate_candidate(spec)
        legs = {}
        for symbol, dtf in datasets:
            legs[ew.dataset_key(symbol, dtf)] = ew.run_candidate_leg(
                self.reg, spec, symbol, dtf, window, self.capital, keep_trades=True,
                funding_mode=self.proto["proxy"]["funding_mode"], warmup_bars=warmup)
        score = ew.score_candidate(legs, self.memo[key])
        score["bars"] = self.memo[key]
        out = summarize(score, venue=False)
        out["warmup_bars"] = warmup
        return out

    def venue(self, spec: dict, wname: str, cost: float) -> dict:
        score = self.ew.evaluate_window(self.reg, spec, self.venue_datasets, wname, self.capital, self.venue_memo,
                                        manifest=self.venue_manifest, cost_multiplier=cost,
                                        incumbent_contract=self.contract)
        out = summarize(score, venue=True)
        out["warmup_bars"] = self.venue_manifest["warmup_bars"]
        return out


def leg_unscorable(name: str, s: dict) -> list:
    bad = []
    if s["datasets"] != 3:
        bad.append(f"{name}: {s['datasets']} datasets scored")
    if s["funding_incomplete_legs"]:
        bad.append(f"{name}: {s['funding_incomplete_legs']} funding_incomplete legs")
    if s.get("funding_fetched_legs"):
        bad.append(f"{name}: funding fetched while scoring")
    if s.get("coverage_complete") is False:
        bad.append(f"{name}: candle or funding coverage incomplete")
    return bad


def window_unscorable(arms: dict) -> list:
    bad = []
    for arm, s in sorted(arms.items()):
        bad.extend(leg_unscorable(arm, s))
        if s.get("incumbent_funding_incomplete"):
            bad.append(f"{arm}: {s['incumbent_funding_incomplete']} incumbent datasets funding_incomplete")
        if s.get("incumbent_missing"):
            bad.append(f"{arm}: {s['incumbent_missing']} incumbent datasets missing")
    return bad


def stability_gate(rows: list) -> dict:
    unscorable = [r for r in rows if r["unscorable"]]
    positive = sum(1 for r in rows if not r["unscorable"] and r["net_pnl_usd"] > 0)
    need = math.ceil(2 * len(rows) / 3)
    if not rows:
        ok = False
    elif unscorable:
        ok = True if positive >= need else (False if positive + len(unscorable) < need else None)
    else:
        ok = positive >= need
    return {"neighbors": len(rows), "positive": positive, "required_positive": need,
            "unscorable": len(unscorable), "ok": ok}


def _beats(cand: dict, other: dict) -> bool:
    return (cand.get("mean_sharpe") or 0.0) > (other.get("mean_sharpe") or 0.0)


def _outcome(known: bool, unknown_reason: str, clusters: int, floor: int, checks: dict) -> tuple:
    false = sorted(k for k, v in checks.items() if v is False)
    unknown = sorted(k for k, v in checks.items() if v is None)
    if not known:
        return "inconclusive", unknown_reason
    if clusters < floor:
        return "inconclusive", f"held-out independent position clusters {clusters} < {floor}"
    if false:
        return "fail", "false: " + ", ".join(false)
    if not unknown:
        return "pass", "every frozen check is true"
    return "inconclusive", "unknown: " + ", ".join(unknown)


def proxy_verdict(proto: dict, held: dict, stability: dict) -> dict:
    floor = proto["sample_rules"]["min_held_out_clusters"]
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
        "training_stability": stability["ok"],
        "m1_oos_pass": tri(cand["m1_verdict"] == "pass"),
        "m1_secondary_passes": True if passes >= 2 else (False if passes + unknown_secondary < 2 else None),
        "oos_net_positive": tri((cand.get("net_pnl_usd") or 0.0) > 0),
    }
    for comp, label in COMPARATOR_CHECK_KEYS.items():
        checks[f"oos_beats_{label}_sharpe"] = tri(_beats(cand, oos[comp]))
    checks["oos_concentration"] = tri(cand["concentration"]["ok"])
    outcome, reason = _outcome(known, "oos is unscorable: " + "; ".join(unscorable[primary]), cand["clusters"],
                               floor, checks)
    return {"outcome": outcome, "reason": reason, "checks": checks, "secondary_m1_passes": passes,
            "unscorable_windows": {w: v for w, v in unscorable.items() if v}, "oos_clusters": cand["clusters"]}


def venue_verdict(proto: dict, held: dict, provenance_kind: str, stability: dict) -> dict:
    floor = proto["sample_rules"]["min_held_out_clusters"]
    base = {k: v["base"] for k, v in held.items()}
    stress = {k: v["stress"] for k, v in held.items()}
    evidence = (provenance_kind == "venue"
                and all(not leg_unscorable(arm, s) and not s.get("incumbent_funding_incomplete")
                        and not s.get("incumbent_missing") and s["coverage_complete"]
                        for arm, legs in held.items() for s in legs.values()))
    cand = base["candidate"]
    checks = {
        "training_stability": stability["ok"],
        "net_positive_x1": (cand.get("net_pnl_usd") or 0.0) > 0,
        "net_positive_x2": (stress["candidate"].get("net_pnl_usd") or 0.0) > 0,
        "m1_test_pass_x1": cand["m1_verdict"] == "pass",
    }
    for comp, label in COMPARATOR_CHECK_KEYS.items():
        checks[f"beats_{label}_sharpe_x1"] = _beats(cand, base[comp])
    checks["concentration_x1"] = cand["concentration"]["ok"]
    outcome, reason = _outcome(evidence, "venue provenance, candle, funding or incumbent evidence is incomplete",
                               cand["clusters"], floor, checks)
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


def verdict_guards(proto: dict, proxy_held: dict, venue_held: dict, stability: dict) -> dict:
    out = {}
    floor = proto["sample_rules"]["min_held_out_clusters"]
    passing = {"ok": True}
    if proxy_held:
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["funding_incomplete_legs"] = 1
        out["proxy_oos_funding_incomplete"] = proxy_verdict(proto, g, passing)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"][FILTER_OFF]["datasets"] = 2
        out["proxy_oos_missing_comparator_dataset"] = proxy_verdict(proto, g, passing)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["clusters"] = floor - 1
        out["proxy_below_sample_floor"] = proxy_verdict(proto, g, passing)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"][MRP]["incumbent_funding_incomplete"] = 1
        out["proxy_incumbent_funding_incomplete"] = proxy_verdict(proto, g, passing)["outcome"]
        out["proxy_isolated_seed"] = proxy_verdict(proto, proxy_held, {"ok": False})["outcome"]
        out["proxy_unknown_stability"] = proxy_verdict(proto, proxy_held, {"ok": None})["outcome"]
    if venue_held:
        g = copy.deepcopy(venue_held)
        g["candidate"]["stress"]["funding_incomplete_legs"] = 1
        out["venue_funding_incomplete"] = venue_verdict(proto, g, "venue", passing)["outcome"]
        g = copy.deepcopy(venue_held)
        g[FILTER_OFF]["base"]["coverage_complete"] = False
        out["venue_coverage_incomplete"] = venue_verdict(proto, g, "venue", passing)["outcome"]
        out["venue_proxy_provenance"] = venue_verdict(proto, venue_held, "proxy", passing)["outcome"]
        g = copy.deepcopy(venue_held)
        g["candidate"]["base"]["clusters"] = floor - 1
        out["venue_below_sample_floor"] = venue_verdict(proto, g, "venue", passing)["outcome"]
        out["venue_isolated_seed"] = venue_verdict(proto, venue_held, "venue", {"ok": False})["outcome"]
    if any(v == "pass" for v in out.values()):
        raise SystemExit(f"verdict guard failed: an incomplete, undersized or isolated group passed: {out}")
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
    out = df[df.index < pd.Timestamp(end)]
    out.attrs = dict(df.attrs)
    return out


def reference_decision(frame, i: int, params: dict) -> dict:
    high = [float(x) for x in frame["high"]]
    low = [float(x) for x in frame["low"]]
    close = [float(x) for x in frame["close"]]
    e, p, length = params["ema_period"], params["sum_period"], params["recursion_length"]
    trend, span, look, age_max = (params["trend_period"], params["slope_span"], params["confirm_lookback"],
                                  params["max_bulge_age"])
    upper, lower = params["upper_threshold"], params["lower_threshold"]
    alpha = 2.0 / (e + 1.0)
    keep = 1.0 - alpha
    ratios = {}

    def ratio(x):
        if x not in ratios:
            e1 = high[x - length] - low[x - length]
            e2 = e1
            for k in range(1, length + 1):
                e1 = alpha * (high[x - length + k] - low[x - length + k]) + keep * e1
                e2 = alpha * e1 + keep * e2
            ratios[x] = e1 / e2
        return ratios[x]

    def mass(x):
        total = ratio(x - p + 1)
        for j in range(1, p):
            total = total + ratio(x - p + 1 + j)
        return total

    def sma(x):
        total = close[x - trend + 1]
        for j in range(1, trend):
            total = total + close[x - trend + 1 + j]
        return total / float(trend)

    def slope(x):
        return sma(x) - sma(x - span)

    def up(x):
        return mass(x - 1) <= upper < mass(x)

    def down(x):
        return mass(x - 1) >= lower > mass(x)

    arm = None
    for s in range(i - 1, i - age_max - 1, -1):
        if up(s):
            arm = s
            break
    consumed = arm is not None and any(down(x) for x in range(arm + 1, i))
    direction = 0
    if arm is not None and not consumed and slope(arm) != 0.0:
        direction = 1 if slope(arm) < 0 else -1
    completed = direction != 0 and down(i)
    prior_high = max(high[i - look:i])
    prior_low = min(low[i - look:i])
    signal = 0
    if completed and direction == 1 and close[i] > prior_high:
        signal = 1
    if completed and direction == -1 and close[i] < prior_low:
        signal = -1
    return {"signal": signal, "mass_index": mass(i), "prev_mass_index": mass(i - 1), "arm": arm,
            "arm_slope": slope(arm) if arm is not None else None, "completed": completed,
            "confirm_high": prior_high, "confirm_low": prior_low}


def _close(a: float, b: float) -> bool:
    return abs(a - b) <= 1e-9 * max(1.0, abs(a), abs(b))


def entry_audit(reg, cand: str, frames: dict, params: dict, positions: list) -> list:
    import pandas as pd
    outs = {k: reg.apply_strategy(cand, f, dict(params)) for k, f in frames.items()}
    rows = []
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]], outs[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig = fill - 1
        rec = out.iloc[sig]
        want = 1 if p["side"] == "long" else -1
        if int(rec["signal"]) != want or rec["mir_valid"] != 1.0 or rec["mir_bulge_completed"] != 1.0:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} has no valid completed {p['side']} "
                             "signal on the prior bar")
        ref = reference_decision(frame, sig, params)
        arm_ts = float(rec["mir_bulge_armed_ts_ms"])
        ref_arm_ts = float(frame.index[ref["arm"]].value // 1_000_000) if ref["arm"] is not None else None
        if (ref["signal"] != want or not ref["completed"] or ref_arm_ts != arm_ts
                or not _close(ref["mass_index"], float(rec["mir_mass_index"]))
                or not _close(ref["prev_mass_index"], float(rec["mir_prev_mass_index"]))
                or not _close(ref["arm_slope"], float(rec["mir_bulge_slope"]))):
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} disagrees with the independent "
                             "Mass Index recomputation")
        need = int(rec["mir_required_history"])
        span = frame.iloc[sig - need + 1:sig + 1]
        rows.append({"dataset": p["dataset"], "side": p["side"], "signal_bar": str(out.index[sig]),
                     "fill_bar": str(out.index[fill]), "exit_bar": p["exit_date"],
                     "exit_reasons": sorted({r.split(":")[0] for r in p.get("exit_reasons") or [] if r}),
                     "net_pnl_usd": p["net_pnl"], "entry_notional_usd": round(p["entry_notional"], 6),
                     "fill_after_signal": out.index[fill] > out.index[sig],
                     "reason": rec["mir_reason"], "input_rows": int(len(span)), "input_sha256": frame_sha256(span),
                     "independent_recomputation_match": True,
                     "mass_index": float(rec["mir_mass_index"]), "prev_mass_index": float(rec["mir_prev_mass_index"]),
                     "armed_at": str(frame.index[ref["arm"]]), "bulge_age": int(rec["mir_bulge_age"]),
                     "bulge_slope": float(rec["mir_bulge_slope"]), "close": float(rec["mir_close"]),
                     "confirm_high": float(rec["mir_confirm_high"]), "confirm_low": float(rec["mir_confirm_low"])})
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
        counts = scored["mir_reason"].value_counts().to_dict()
        valid = scored["mir_valid"].to_numpy() == 1.0
        first_valid = scored.index[valid].min() if valid.any() else None
        out[key] = {"bars": int(len(scored)), "valid_bars": int(valid.sum()),
                    "reasons": {k: int(v) for k, v in sorted(counts.items())},
                    "upper_crossings": int((scored["mir_upper_cross"] == 1.0).sum()),
                    "completed_bulges": int((scored["mir_bulge_completed"] == 1.0).sum()),
                    "filtered_signals": int(((scored["mir_bulge_completed"] == 1.0)
                                             & (scored["mir_confirmed"] == 0.0)).sum()),
                    "long_signals": int((scored["signal"] == 1).sum()),
                    "short_signals": int((scored["signal"] == -1).sum()),
                    "first_valid_bar": str(first_valid) if first_valid is not None else None,
                    "loaded_bars": int(len(frame)), "frame_sha256": frame_sha256(frame),
                    "first_loaded_bar": str(frame.index[0]), "last_bar": str(frame.index[-1])}
    return out


def _bulge_totals(inv: dict) -> dict:
    keys = ("upper_crossings", "completed_bulges", "filtered_signals", "long_signals", "short_signals")
    return {k: sum(v[k] for v in inv.values()) for k in keys}


def proxy_stability(study: Study, tf: str) -> dict:
    w = study.proto["proxy"]["training_window"]
    rows = []
    for nb in neighbors(study.seed):
        s = study.proxy(study.spec_for(study.cand, nb["params"]), tf, w)
        rows.append({"param": nb["param"], "value": nb["value"], "net_pnl_usd": s["net_pnl_usd"],
                     "mean_sharpe": s["mean_sharpe"], "positions": s["positions"], "clusters": s["clusters"],
                     "m1_verdict": s["m1_verdict"], "unscorable": leg_unscorable("neighbor", s)})
    seed = study.proxy(study.arm_spec("candidate"), tf, w)
    gate = stability_gate(rows)
    gate.update({"window": w, "rows": rows, "seed_training": brief(seed)})
    print(f"[proxy {tf}] stability {gate['positive']}/{gate['neighbors']} positive -> {gate['ok']}", flush=True)
    return gate


def venue_stability(study: Study) -> dict:
    w = study.proto["venue"]["training_window"]
    rows = []
    for nb in neighbors(study.seed):
        s = study.venue(study.spec_for(study.cand, nb["params"]), w, 1.0)
        rows.append({"param": nb["param"], "value": nb["value"], "net_pnl_usd": s["net_pnl_usd"],
                     "mean_sharpe": s["mean_sharpe"], "positions": s["positions"], "clusters": s["clusters"],
                     "m1_verdict": s["m1_verdict"], "unscorable": leg_unscorable("neighbor", s)})
    gate = stability_gate(rows)
    gate.update({"window": w, "rows": rows})
    print(f"[venue] stability {gate['positive']}/{gate['neighbors']} positive -> {gate['ok']}", flush=True)
    return gate


def run_proxy_group(study: Study, tf: str) -> dict:
    proto = study.proto
    from closed_bar import FIXED_DURATION_MS
    stability = proxy_stability(study, tf)
    arms = {a: study.arm_spec(a) for a in ARMS}
    primary = proto["proxy"]["primary_window"]
    windows = [primary] + proto["proxy"]["secondary_windows"] + [proto["proxy"]["training_window"]]
    full, held = {}, {}
    for w in windows:
        full[w] = {k: study.proxy(a, tf, w) for k, a in arms.items()}
        held[w] = {k: brief(s) for k, s in full[w].items()}
        for k in arms:
            held[w][k]["per_dataset"] = full[w][k]["per_dataset"]
            held[w][k]["cluster_pnls"] = full[w][k]["cluster_pnls"]
        print(f"[proxy {tf}] {w}: candidate {held[w]['candidate']['m1_verdict']} "
              f"net {held[w]['candidate']['net_pnl_usd']}", flush=True)
    scored = {w: held[w] for w in [primary] + proto["proxy"]["secondary_windows"]}
    verdict = proxy_verdict(proto, scored, stability)
    frames = {}
    for symbol in study.symbols:
        key = study.ew.dataset_key(symbol, tf)
        frames[key] = proxy_frame(symbol, tf, study.windows[primary], study.warmup_for(tf, primary))
    audit = entry_audit(study.reg, study.cand, frames, study.seed, full[primary]["candidate"]["positions_list"])
    holds, filter_off_holds = {}, {}
    for w in windows:
        wf, starts = {}, {}
        for s in study.symbols:
            key = study.ew.dataset_key(s, tf)
            wf[key] = proxy_frame(s, tf, study.windows[w], study.warmup_for(tf, w))
            starts[key] = study.windows[w][0]
        holds[w] = hold_inventory(study.reg, study.cand, wf, study.seed, starts)
        filter_off_holds[w] = _bulge_totals(hold_inventory(study.reg, study.cand, wf,
                                                           study.comparator_params[FILTER_OFF], starts))
    return {"expected_interval_ms": FIXED_DURATION_MS[tf], "params": study.seed,
            "comparator_params": study.comparator_params, "stability": stability, "held_out": held,
            "verdict": verdict, "entry_audit": audit, "hold_inventory": holds,
            "bulge_totals": {w: _bulge_totals(v) for w, v in holds.items()},
            "filter_off_signal_totals": filter_off_holds,
            "warmup_by_window": {w: study.warmup_for(tf, w) for w in windows}}


def run_venue_group(study: Study) -> dict:
    proto = study.proto
    from closed_bar import FIXED_DURATION_MS
    stability = venue_stability(study)
    arms = {a: study.arm_spec(a) for a in ARMS}
    costs = proto["venue"]["cost_multipliers"]
    held_window, second = proto["venue"]["held_out_window"], proto["venue"]["training_window"]
    out_windows = {}
    full_test = None
    for w in (held_window, second):
        full = {k: {label: study.venue(a, w, mult) for label, mult in costs.items()} for k, a in arms.items()}
        held = {k: {label: brief(s) for label, s in v.items()} for k, v in full.items()}
        for k in held:
            held[k]["base"]["per_dataset"] = full[k]["base"]["per_dataset"]
            held[k]["base"]["cluster_pnls"] = full[k]["base"]["cluster_pnls"]
        out_windows[w] = held
        if w == held_window:
            full_test = full
        print(f"[venue] {w}: candidate {held['candidate']['base']['m1_verdict']} "
              f"net {held['candidate']['base']['net_pnl_usd']}", flush=True)
    kind = study.venue_manifest["provenance"]["kind"]
    verdict = venue_verdict(proto, out_windows[held_window], kind, stability)
    import offline_manifest as om
    frames = {}
    for ds in study.venue_manifest["datasets"]:
        frame, _, _ = om.window_frame(study.venue_manifest, ds, held_window)
        frames[ds["key"]] = frame
    audit = entry_audit(study.reg, study.cand, frames, study.seed, full_test["candidate"]["base"]["positions_list"])
    holds, filter_off_holds = {}, {}
    for w in (held_window, second):
        wf, starts = {}, {}
        for ds in study.venue_manifest["datasets"]:
            frame, _, _ = om.window_frame(study.venue_manifest, ds, w)
            wf[ds["key"]] = frame
            starts[ds["key"]] = study.venue_manifest["windows"][w]["start"]
        holds[w] = hold_inventory(study.reg, study.cand, wf, study.seed, starts)
        filter_off_holds[w] = _bulge_totals(hold_inventory(study.reg, study.cand, wf,
                                                           study.comparator_params[FILTER_OFF], starts))
    return {"expected_interval_ms": FIXED_DURATION_MS[proto["venue"]["interval"]], "params": study.seed,
            "comparator_params": study.comparator_params, "stability": stability,
            "held_out": out_windows[held_window], "training": out_windows[second], "verdict": verdict,
            "entry_audit": audit, "hold_inventory": holds,
            "bulge_totals": {w: _bulge_totals(v) for w, v in holds.items()},
            "filter_off_signal_totals": filter_off_holds}


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


def window_stability(groups: dict) -> list:
    rows = []
    for gname, g in groups.items():
        venue = gname.startswith("venue")
        windows = {"test": g["held_out"], "train": g["training"]} if venue else g["held_out"]
        for w, arms in windows.items():
            cand = arms["candidate"]["base"] if venue else arms["candidate"]
            rows.append({"group": gname, "window": w, "m1_verdict": cand["m1_verdict"],
                         "mean_sharpe": cand["mean_sharpe"], "net_pnl_usd": cand["net_pnl_usd"],
                         "positions": cand["positions"], "clusters": cand["clusters"],
                         "long_net_pnl_usd": cand.get("long_net_pnl_usd"),
                         "short_net_pnl_usd": cand.get("short_net_pnl_usd"),
                         "funding_incomplete_legs": cand["funding_incomplete_legs"]})
    return rows


def run(cache_db: str, groups=None) -> None:
    proto = load_json(PROTOCOL_PATH)
    held_lock = require_lock()
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    grid_check = check_grid(proto)
    study = Study(proto, frozen["proxy"], frozen["venue"])
    started = time.time()
    out = {
        "study": proto["study"], "issue": proto["issue"], "tracking_group": proto["tracking_group"],
        "protocol_sha256": protocol_sha256(), "protocol_lock": held_lock, "grid_check": grid_check,
        "grid": grid(), "neighbors": [{"param": n["param"], "value": n["value"]} for n in neighbors(study.seed)],
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
    out["verdict_guards"] = {}
    for g, v in out["groups"].items():
        if g.startswith("proxy_"):
            primary = proto["proxy"]["primary_window"]
            scored = {w: v["held_out"][w] for w in [primary] + proto["proxy"]["secondary_windows"]}
            out["verdict_guards"][g] = verdict_guards(proto, scored, {}, v["stability"])
        else:
            out["verdict_guards"][g] = verdict_guards(proto, {}, v["held_out"], v["stability"])
    out["window_stability"] = window_stability(out["groups"])
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']['outcome']}; wrote {RESULTS_PATH}")


def parity_sources(frozen: dict) -> list:
    import pandas as pd
    import offline_manifest as om
    from closed_bar import FIXED_DURATION_MS
    end = "2026-10-01"
    sources = []
    for tf in ("1h", "4h"):
        step = FIXED_DURATION_MS[tf]
        start = pd.Timestamp(end) - pd.Timedelta(milliseconds=step * PARITY_BARS)
        df = proxy_frame("BTC/USDT", tf, (str(start.date()), end))
        sources.append((tf, f"proxy BTC/USDT {tf} {df.index[0].date()}..{end}", df))
    vds = om.dataset_by_key(frozen["venue"], "BTC 4h")
    vframe, _, _ = om.window_frame(frozen["venue"], vds, "test")
    sources.append(("4h", "venue BTC 4h test window with warmup", vframe))
    return sources


def _same(a, b) -> bool:
    if isinstance(a, str) or isinstance(b, str):
        return a == b
    a, b = float(a), float(b)
    return (math.isnan(a) and math.isnan(b)) or a == b


def raw_parity(reg, name: str, df, params: dict) -> dict:
    full = reg.apply_strategy(name, df, dict(params))
    need = int(full["mir_required_history"].iloc[0])
    valid = full["mir_valid"].to_numpy() == 1.0
    compared = {"prefix": 0, "required": 0, "199": 0, "200": 0}
    mismatches = {k: 0 for k in compared}
    first = {}
    for i in range(len(df)):
        frames = {"prefix": df.iloc[:i + 1]}
        for tail in RAW_TAILS:
            size = need if tail == "required" else tail
            if i + 1 >= size:
                frames[str(tail)] = df.iloc[i + 1 - size:i + 1]
        want = full.iloc[i]
        for label, frame in frames.items():
            if label != "prefix" and not valid[i]:
                continue
            got = reg.apply_strategy(name, frame.copy(), dict(params)).iloc[-1]
            compared[label] += 1
            bad = [c for c in RAW_COLUMNS if not _same(got[c], want[c])]
            if bad:
                mismatches[label] += 1
                first.setdefault(label, {"bar": str(df.index[i]), "columns": bad})
    return {"rows": int(len(df)), "valid_rows": int(valid.sum()), "required_history": need,
            "compared": compared, "mismatches": mismatches, "first_mismatch": first,
            "long_signals": int((full["signal"] == 1).sum()), "short_signals": int((full["signal"] == -1).sum()),
            "completed_bulges": int((full["mir_bulge_completed"] == 1.0).sum()),
            "clean": all(v == 0 for v in mismatches.values())}


def parity(cache_db: str) -> None:
    proto = load_json(PROTOCOL_PATH)
    require_lock()
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    from parity_diff import compute_parity_frame, config_from_live_config, summarize as parity_summarize
    from registry_loader import load_registry
    reg = load_registry(proto["registry"])
    seed = dict(proto["seed_params"])
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "venue_manifest_sha256": sha256_file(VENUE_MANIFEST_PATH),
           "source_commit": git_head(), "runs": [], "raw": []}
    for tf, label, df in parity_sources(frozen):
        for mode, params in (("filter_on", seed), ("filter_off", dict(seed, bulge_filter=0))):
            t0 = time.time()
            raw = raw_parity(reg, proto["candidate"], df, params)
            raw.update({"source": label, "interval": tf, "mode": mode, "input_sha256": frame_sha256(df),
                        "seconds": round(time.time() - t0, 1)})
            out["raw"].append(raw)
            print(json.dumps({k: raw[k] for k in ("interval", "source", "mode", "compared", "mismatches",
                                                  "long_signals", "short_signals", "clean")}), flush=True)
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        sid = load_json(path)["strategies"][0]["id"]
        for batched in (False, True):
            for window in PARITY_WINDOWS:
                cfg = config_from_live_config(path, sid, comparison_mode=proto["contract"]["comparison_mode"])
                cfg.batched = batched
                cfg.funding_mode = proto["proxy"]["funding_mode"]
                t0 = time.time()
                frame = compute_parity_frame(df, cfg=cfg, window=window)
                summ = parity_summarize(frame)
                validation = summ.get("close_validation") or {}
                row = {"source": label, "interval": tf, "config": os.path.basename(path), "strategy_id": sid,
                       "params": cfg.params, "close_refs": cfg.close_refs, "direction": cfg.direction,
                       "stop_kwargs": cfg.stop_kwargs, "batched": batched,
                       "window": "every prefix" if window is None else window,
                       "input_sha256": frame_sha256(df), "candles": int(len(df)),
                       "bars_compared": summ["bars_compared"], "mismatches": summ["mismatches"],
                       "batch_mismatches": summ.get("batch_mismatches"),
                       "decision_agreement": summ["decision_agreement"], "close_parity": summ["close_parity"],
                       "close_eligibility": validation.get("close_eligibility"),
                       "incomplete_parity": validation.get("incomplete_parity"),
                       "comparison_mode": validation.get("mode"), "clean": summ["clean"],
                       "live_long_signals": int((frame["live_signal"] == 1).sum()) if len(frame) else 0,
                       "live_short_signals": int((frame["live_signal"] == -1).sum()) if len(frame) else 0,
                       "seconds": round(time.time() - t0, 1)}
                out["runs"].append(row)
                print(json.dumps({k: row[k] for k in ("interval", "source", "batched", "window", "bars_compared",
                                                      "mismatches", "batch_mismatches", "live_long_signals",
                                                      "live_short_signals", "clean")}), flush=True)
    out["clean"] = (all(r["clean"] and r["bars_compared"] > 0 and r["mismatches"] == 0
                        and (not r["batched"] or r["batch_mismatches"] == 0)
                        and r["close_eligibility"] == "eligible" and r["incomplete_parity"] is False
                        for r in out["runs"])
                    and all(r["clean"] and r["compared"]["required"] > 0 for r in out["raw"]))
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


def _arm_table(arms: dict) -> list:
    lines = ["| arm | M1 verdict | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) "
             "| clusters | largest cluster share | turnover | fees $ | funding $ | incomplete legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, s in arms.items():
        conc = s.get("concentration") or {}
        lines.append(f"| {name} | {s.get('m1_verdict')} | {fmt(s.get('mean_sharpe'))} | {fmt(s.get('mean_return_pct'))} "
                     f"| {fmt(s.get('net_pnl_usd'))} | {fmt(s.get('worst_max_dd_pct'))} | {s.get('positions')} "
                     f"({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('clusters')} "
                     f"| {fmt(conc.get('largest_positive_cluster_share'))} "
                     f"| {fmt(s.get('mean_turnover'))} | {fmt(s.get('fees_usd'))} | {fmt(s.get('funding_pnl_usd'))} "
                     f"| {s.get('funding_incomplete_legs')} |")
    return lines


def _concentration_line(s: dict) -> str:
    conc = s.get("concentration") or {}
    return (f"top dataset share of absolute net PnL {fmt(s.get('top_dataset_abs_pnl_share'))}, largest positive "
            f"cluster ${fmt(conc.get('largest_positive_cluster_usd'))} of ${fmt(conc.get('positive_cluster_sum_usd'))} "
            f"positive cluster PnL (share {fmt(conc.get('largest_positive_cluster_share'))}, rule met: "
            f"{fmt(conc.get('ok'))}), long net ${fmt(s.get('long_net_pnl_usd'))}, short net "
            f"${fmt(s.get('short_net_pnl_usd'))}")


DECISION_REASONS = ("no_bulge", "armed", "entry_long", "entry_short", "confirmation_failed", "zero_slope")


def _holds_lines(holds: dict) -> list:
    lines = ["| window | dataset | scored bars | valid bars | first valid bar | upper crossings | completed bulges "
             "| filtered | entries L/S | hold reasons |", "|---|---|---|---|---|---|---|---|---|---|"]
    for w, per in holds.items():
        for ds, h in per.items():
            reasons = ", ".join(f"{k} {v}" for k, v in h["reasons"].items() if k not in DECISION_REASONS)
            lines.append(f"| {w} | {ds} | {h['bars']} | {h['valid_bars']} | {h['first_valid_bar']} "
                         f"| {h['upper_crossings']} | {h['completed_bulges']} | {h['filtered_signals']} "
                         f"| {h['long_signals']}/{h['short_signals']} | {reasons or '-'} |")
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


def _stability_lines(gate: dict) -> list:
    lines = [f"Stability gate on `{gate['window']}` (scored before the held-out windows): {gate['positive']} of "
             f"{gate['neighbors']} one-parameter neighbors had positive summed net PnL; {gate['required_positive']} "
             f"needed; {gate['unscorable']} unscorable; result {tri(gate['ok'])}.", "",
             "| neighbor | net PnL $ | mean Sharpe | positions | clusters | M1 | unscorable |",
             "|---|---|---|---|---|---|---|"]
    for r in gate["rows"]:
        lines.append(f"| {r['param']} = {r['value']} | {fmt(r['net_pnl_usd'])} | {fmt(r['mean_sharpe'])} "
                     f"| {r['positions']} | {r['clusters']} | {r['m1_verdict']} | {'; '.join(r['unscorable']) or '-'} |")
    return lines


def _sentence(text: str) -> str:
    return text[:1].upper() + text[1:]


def render() -> None:
    proto = load_json(PROTOCOL_PATH)
    res = load_json(RESULTS_PATH)
    par = load_json(PARITY_PATH) if os.path.exists(PARITY_PATH) else None
    manifest = load_json(MANIFEST_PATH)
    v = res["verdict"]
    seed = proto["seed_params"]
    L = []
    L.append("# Mass Index reversal study (issue 1670)")
    L.append("")
    L.append(f"**Verdict: {v['outcome'].upper()}** (proxy 1h: {v['groups'].get('proxy_1h')}, proxy 4h: "
             f"{v['groups'].get('proxy_4h')}, venue 4h: {v['groups'].get('venue_4h')}). Registry metadata after this "
             f"study: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`, `edge_ref` = this file. The "
             "candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs "
             "`allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study "
             "is suggest-only and sends no order. The proxy groups are proxy evidence (Binance US candles with "
             "Hyperliquid fees and funding); the venue group is Hyperliquid candle evidence.")
    L.append("")
    L.append("Group reasons: " + "; ".join(f"{g}: {d['verdict']['outcome']} ({d['verdict']['reason']})"
                                         for g, d in res["groups"].items()) + ".")
    L.append("")
    L.append("## Signal")
    L.append("")
    L.append(f"`mass_index_reversal` (`shared_strategies/open/mass_index_reversal.py`, Go short name "
             f"`{proto['short_name']}`) is entry-only and futures-only. " + _sentence(proto["formula"]) + ". "
             + _sentence(proto["event_rules"]) + ". The Mass Index itself has no direction. Seed parameters: "
             + ", ".join(f"`{k}={val}`" for k, val in seed.items())
             + f"; a decision needs {proto['required_history_bars']} clean consecutive rows with declared bar "
             "cadence (at most 199, inside the 200-row check window). Bad candles, timestamp defects, cadence gaps "
             "and a non-positive second average hold the entry with a reason (`mir_reason`); close and stop "
             "management continue.")
    L.append("")
    L.append("## Protocol")
    L.append("")
    lock_ = res["protocol_lock"]
    gc = res["grid_check"]
    L.append(f"`protocol.json` (sha256 `{res['protocol_sha256']}`) was locked by `run_study.py lock` at "
             f"{lock_['locked_at_utc']} (commit `{lock_['source_commit']}`), before any window was scored; `run` "
             "and `parity` refuse a different protocol. It fixes the seed, the matched contract, the windows, the "
             "warm-up policy, the stability, concentration and sample rules and the pass/fail/inconclusive rules. "
             f"The seed is never re-selected. The optimizer grid has {gc['combinations']} combinations, every one "
             f"passes the parameter validator (worst required history {gc['worst_required_history']} rows); the "
             f"stability gate uses its {len(res['neighbors'])} one-parameter neighbors of the seed.")
    L.append("")
    c = proto["contract"]
    first_group = next(iter(res["groups"].values()))
    L.append(f"Contract for the candidate, both comparators and the M1 incumbent median: direction "
             f"`{c['direction']}`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop "
             f"(`stop_loss_atr_mult: {c['stop_loss_atr_mult']}`), `comparison_mode: {c['comparison_mode']}`, "
             f"`intrabar_resolution: {proto['intrabar_resolution']}`, no regime gate, capital "
             f"${proto['capital_usd']:.0f} per leg. Comparators: `{FILTER_OFF}` "
             f"({proto['comparators'][FILTER_OFF]['definition']}) and `{MRP}` with its registry defaults "
             f"{json.dumps(first_group['comparator_params'][MRP], sort_keys=True)}. The M1 incumbent median is its "
             "own reference under the same contract and is not a matched both-side comparison.")
    L.append("")
    L.append("Warm-up: " + proto["warmup"]["policy"] + ". " + _sentence(proto["warmup"]["restriction"]) + ".")
    L.append("")
    L.append("Proxy group rule: " + proto["verdict_rules"]["proxy_group"]["order"] + ". Venue group rule: "
             + proto["verdict_rules"]["venue_group"]["order"] + ". Overall: " + proto["verdict_rules"]["overall"] + ".")
    L.append("")
    L.append("| proxy window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    for w, (s, e) in proto["proxy"]["windows"].items():
        role = ("primary held-out" if w == proto["proxy"]["primary_window"] else
                "training (stability gate, diagnostic)" if w == proto["proxy"]["training_window"]
                else "secondary held-out")
        L.append(f"| {w} | {s} | {e} | {role} |")
    L.append("")
    L.append("Departures from the default M1 windows: " + "; ".join(proto["proxy"]["departures_from_m1_windows"])
             + ". " + _sentence(proto["proxy"]["window_note"]) + ".")
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
             f"proxy evidence and not measured Hyperliquid candle evidence ({res['copied_from_check']['proxy']}).")
    L.append("- Proxy funding: Hyperliquid hourly funding charged on every held hour (`--funding charge`, "
             "`eval_windows.run_leg(funding_mode=\"charge\")`) for every candidate, comparator, neighbor and "
             "incumbent leg.")
    L.append(f"- Proxy costs: engine Hyperliquid commission `{eng['commission_pct']}` per side and engine slippage "
             f"`{eng['slippage_pct']}` per fill; the slippage term stands in for the spread. Lot sizes and venue "
             "minimums are unmodeled, so the proxy result cannot claim Hyperliquid execution parity.")
    L.append(f"- Venue candles: {vm['provenance']['label']} ({res['copied_from_check']['venue']}), interval "
             f"{vm['interval']}, {vm['warmup_bars']} warm-up bars read by the evaluator but never scored or traded.")
    costs = vm["costs"]
    L.append(f"- Venue costs (manifest execution specification with the explicit close owner): taker fee "
             f"{costs['taker_fee_pct']}, maker fee {costs['maker_fee_pct']}, slippage {costs['slippage_bps']} bps per "
             f"side plus the per-coin half spread from one frozen L2 snapshot, szDecimals lot rounding, minimum "
             f"notional ${costs['min_notional_usd']} with a {costs['min_notional_margin']} margin; cost x2 doubles "
             "spread and slippage. Funding uses the manifest's existing attachment and coverage semantics.")
    L.append("")
    L.append("| venue dataset | size decimals | half spread bps | candles sha256 | funding sha256 |")
    L.append("|---|---|---|---|---|")
    for d in vm["datasets"]:
        L.append(f"| {d['key']} | {d['size_decimals']} | {d['half_spread_bps']} | `{d['candles_sha256']}` "
                 f"| `{d['funding_sha256']}` |")
    L.append("")
    venue_g = res["groups"].get("venue_4h")
    rejected = venue_g["held_out"]["candidate"]["base"].get("rejected_entries") if venue_g else None
    L.append(f"- Sizing: capital ${eng['capital_usd']:.0f} per leg, no leverage and no perps sizing block (Backtester "
             f"leverage {eng['leverage']}, sizing defaults {json.dumps(eng['sizing_defaults'], sort_keys=True)}), so "
             "each entry commits the leg's available cash unlevered. Venue lot rounding and the minimum rejected "
             f"{fmt(rejected)} candidate entries in the held-out venue window at cost x1.")
    L.append(f"- Stop: one fixed entry-ATR stop at {c['stop_loss_atr_mult']} x ATR. Close: one `tiered_tp_pct` "
             "tier at +3%.")
    L.append("")
    L.append("| proxy file (read in place) | rows | sha256 |")
    L.append("|---|---|---|")
    for name, meta in manifest["files"].items():
        L.append(f"| `{manifest['data_root']}/{name}` | {meta['rows']} | `{meta['sha256']}` |")
    L.append("")
    for gname, g in res["groups"].items():
        venue = gname.startswith("venue")
        L.append(f"## {gname} (bar interval {g['expected_interval_ms']} ms)")
        L.append("")
        ver = g["verdict"]
        L.append(f"**Group verdict: {ver['outcome']}** ({ver['reason']}).")
        L.append("")
        L.append("| check | value |")
        L.append("|---|---|")
        for k, val in ver["checks"].items():
            L.append(f"| {k} | {tri(val)} |")
        L.append("")
        L.extend(_stability_lines(g["stability"]))
        L.append("")
        if venue:
            for label in ("base", "stress"):
                L.append(f"### Held-out `test`, cost {label}")
                L.append("")
                L.extend(_arm_table({k: v_[label] for k, v_ in g["held_out"].items()}))
                L.append("")
            L.append("### Training `train`, cost base (diagnostic)")
            L.append("")
            L.extend(_arm_table({k: v_["base"] for k, v_ in g["training"].items()}))
            L.append("")
            cand = g["held_out"]["candidate"]["base"]
            L.append("Concentration (candidate, test, cost x1): " + _concentration_line(cand) + ".")
            L.append("")
            L.append("| dataset | candles complete | funding complete | rejected entries | min entry notional $ |")
            L.append("|---|---|---|---|---|")
            for ds, item in cand["per_dataset"].items():
                L.append(f"| {ds} | {fmt(item['candle_coverage']['complete'])} "
                         f"| {fmt(item['funding_coverage']['complete'])} | {item['rejected_entries']} "
                         f"| {fmt(item['min_entry_notional_usd'])} |")
            L.append("")
        else:
            order = ([proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"]
                     + [proto["proxy"]["training_window"]])
            for w in order:
                L.append(f"### `{w}` (warm-up {g['warmup_by_window'][w]} bars)")
                L.append("")
                L.extend(_arm_table(g["held_out"][w]))
                L.append("")
            cand = g["held_out"][proto["proxy"]["primary_window"]]["candidate"]
            L.append("Concentration (candidate, oos): " + _concentration_line(cand) + ".")
            L.append("")
            if ver.get("unscorable_windows"):
                L.append("Unscorable windows: " + "; ".join(f"{w}: {', '.join(r)}"
                                                           for w, r in ver["unscorable_windows"].items()) + ".")
                L.append("")
            L.append("Funding coverage and provenance (candidate legs, oos):")
            L.append("")
            L.extend(_funding_lines(cand["per_dataset"]))
            L.append("")
        L.append("Bulge events and holds per window (scored bars only; warm-up bars are read but not counted). "
                 "Filtered = completed bulges whose price confirmation failed:")
        L.append("")
        L.extend(_holds_lines(g["hold_inventory"]))
        L.append("")
        L.append("Filter-off comparison signal totals per window: " + "; ".join(
            f"{w}: {t['long_signals']} long / {t['short_signals']} short" for w, t in
            g["filter_off_signal_totals"].items()) + ".")
        L.append("")
        audit = g["entry_audit"]
        L.append(f"Entry audit: {len(audit)} held-out candidate entries; each fill is on the bar after a valid "
                 "completed-bulge signal, and the Mass Index, arming bar, frozen slope and confirmation equal an "
                 "independent scalar recomputation from the raw candles; `results.json` stores the input span hash "
                 "and values. First entries:")
        L.append("")
        L.append("| dataset | side | armed at | signal bar | fill bar | age | Mass Index | close | confirm high | "
                 "confirm low | net $ |")
        L.append("|---|---|---|---|---|---|---|---|---|---|---|")
        for a in audit[:10]:
            L.append(f"| {a['dataset']} | {a['side']} | {a['armed_at']} | {a['signal_bar']} | {a['fill_bar']} "
                     f"| {a['bulge_age']} | {a['mass_index']:.6g} | {a['close']:.6g} | {a['confirm_high']:.6g} "
                     f"| {a['confirm_low']:.6g} | {fmt(a['net_pnl_usd'])} |")
        L.append("")
    L.append("## Stability across intervals and windows")
    L.append("")
    L.append("| group | window | M1 | mean Sharpe | net $ | positions | clusters | long net $ | short net $ "
             "| funding-incomplete legs |")
    L.append("|---|---|---|---|---|---|---|---|---|---|")
    for r in res["window_stability"]:
        L.append(f"| {r['group']} | {r['window']} | {r['m1_verdict']} | {fmt(r['mean_sharpe'])} "
                 f"| {fmt(r['net_pnl_usd'])} | {r['positions']} | {r['clusters']} | {fmt(r['long_net_pnl_usd'])} "
                 f"| {fmt(r['short_net_pnl_usd'])} | {r['funding_incomplete_legs']} |")
    L.append("")
    L.append("Searched variants: the seed, the filter-off comparison and the one-parameter neighbors listed in each "
             "group's stability table; no other variant was run.")
    L.append("")
    L.append("Verdict guards (each altered copy must not pass): " + "; ".join(
        f"{g}: " + ", ".join(f"{k} -> {v_}" for k, v_ in guards.items()) for g, guards in res["verdict_guards"].items())
        + ".")
    L.append("")
    L.append("## Decision parity")
    L.append("")
    if par:
        L.append("`run_study.py parity` checks raw indicator and event equality first: for every bar of each source "
                 "the evaluator's last row on the expanding prefix, and on trailing frames of the required history, "
                 "199 and 200 rows wherever the full-series row is valid, must equal the full-series row in every "
                 "`mir_` value, the reason and the signal (filter on and filter off). It then runs the explicit-paper "
                 "configurations `paper_parity_<interval>.json` through `backtest/parity_diff.compute_parity_frame` "
                 "with `--registry futures --platform hyperliquid`, strict close comparison and charged funding: "
                 "non-batched and batched, on the trailing 200-bar window and on every prefix. Overall clean: "
                 f"{fmt(par['clean'])}.")
        L.append("")
        L.append("| interval | source | mode | prefix rows | required-history tails | 199 tails | 200 tails "
                 "| mismatches | completed bulges | long/short | clean |")
        L.append("|---|---|---|---|---|---|---|---|---|---|---|")
        for r in par["raw"]:
            cmp_ = r["compared"]
            L.append(f"| {r['interval']} | {r['source']} | {r['mode']} | {cmp_['prefix']} | {cmp_['required']} "
                     f"| {cmp_['199']} | {cmp_['200']} | {sum(r['mismatches'].values())} | {r['completed_bulges']} "
                     f"| {r['long_signals']}/{r['short_signals']} | {fmt(r['clean'])} |")
        L.append("")
        L.append("| interval | source | batched | window | bars compared | mismatches | batch mismatches | long/short "
                 "signals | close eligibility | clean |")
        L.append("|---|---|---|---|---|---|---|---|---|---|")
        for r in par["runs"]:
            L.append(f"| {r['interval']} | {r['source']} | {fmt(r['batched'])} | {r['window']} "
                     f"| {r['bars_compared']} | {r['mismatches']} | {fmt(r['batch_mismatches'])} "
                     f"| {r['live_long_signals']}/{r['live_short_signals']} | {r['close_eligibility']} "
                     f"(parity {r['close_parity']}) | {fmt(r['clean'])} |")
        L.append("")
        L.append("The parity adapter uses prebuilt frames without closed-bar timing; the closed-bar boundary is "
                 "proved separately by the timed-candle check tests in `shared_scripts/test_mass_index_reversal_"
                 "checks.py`.")
        L.append("")
    else:
        L.append("Parity has not been run for this results file.")
        L.append("")
    L.append("## Limits")
    L.append("")
    L.append("- The Mass Index here is the finite-recursion research variant; it is not an exact copy of the "
             "infinite-history published formula.")
    L.append("- Proxy legs use Binance US spot candles; basis, venue wicks and funding-driven price effects are not "
             "modelled. The legacy M1 screen has no spread, lot rounding or minimum order, so venue execution "
             "claims rest on the venue group only.")
    L.append("- Hyperliquid funding starts in May 2023; a proxy 2023 leg that holds a position before the first "
             "rate is funding_incomplete, and such a window can never support a pass. The 2023 window also has no "
             "prehistory for the warm-up.")
    L.append("- The venue windows cover 2024-09-01 to 2026-09-01 only, and earlier studies already used them, so "
             "the venue result is retrospective. Spread is a per-coin half spread from one frozen L2 snapshot plus "
             "flat slippage; queue position, partial fills and market impact are not modelled.")
    L.append("- Only 1h and 4h are scored. Every score reads only frozen inputs with network acquisition disabled. "
             "A research result alone cannot approve deployment.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append("uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py lock")
    L.append("uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py run "
             "--cache-db /tmp/mir1670_run.db")
    L.append("uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py parity "
             "--cache-db /tmp/mir1670_parity.db")
    L.append("uv run --no-sync python backtest/candidates/mass_index_reversal_1670/run_study.py render")
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
    p = argparse.ArgumentParser(description="#1670 Mass Index reversal study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("lock", help="check the grid and record protocol.json's sha256 before any score")
    sub.add_parser("hash", help="print protocol.json's sha256")
    r = sub.add_parser("run", help="rebuild a fresh cache from the frozen files and run the protocol offline")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--group", action="append", choices=["proxy_1h", "proxy_4h", "venue_4h"])
    q = sub.add_parser("parity", help="raw indicator parity and decision parity of the paper configurations")
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
