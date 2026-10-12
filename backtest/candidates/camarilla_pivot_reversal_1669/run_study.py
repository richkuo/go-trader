#!/usr/bin/env python3

import argparse
import contextlib
import copy
import csv
import gzip
import hashlib
import json
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
VENUE_MANIFEST_PATH = os.path.abspath(os.path.join(HERE, "..", "camarilla_pivot_reversal_1669_manifest.json"))
VENUE_SOURCE_MANIFEST = "backtest/candidates/dual_thrust_1642/venue_manifest.json"
RESULTS_PATH = os.path.join(HERE, "results.json")
PARITY_PATH = os.path.join(HERE, "parity_results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
DAY_MS = 86_400_000
for _p in (os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
LOCAL_COMPARATOR = "prior_day_reclaim_1669"
COMPARATOR_CHECK_KEYS = {
    "mean_reversion_pro": "mean_reversion_pro",
    "anchored_vwap_reversion": "anchored_vwap_reversion",
    LOCAL_COMPARATOR: "prior_day_reclaim",
}
SOURCE_FILES = (
    "shared_strategies/open/camarilla_pivot_reversal.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/open/mean_reversion_pro.py",
    "shared_strategies/open/anchored_vwap_reversion.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/closed_bar.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/data_fetcher.py",
    "shared_scripts/check_hyperliquid.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/parity_diff.py",
    "backtest/candidates/camarilla_pivot_reversal_1669/run_study.py",
    "backtest/candidates/camarilla_pivot_reversal_1669/protocol.json",
    "backtest/candidates/camarilla_pivot_reversal_1669/study_manifest.json",
    "backtest/candidates/camarilla_pivot_reversal_1669_manifest.json",
)
PARITY_INTERVALS = ("15m", "30m", "1h", "2h", "4h", "8h", "12h")
PARITY_BARS = 2200
PARITY_WINDOWS = (200, None)
SYNTHETIC_SEED = 1669
SYNTHETIC_START = "2026-01-05"


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


def prior_day_reclaim(df, expected_interval_ms: int = 3_600_000):
    import numpy as np
    from camarilla_pivot_reversal import camarilla_pivot_reversal_core
    out = camarilla_pivot_reversal_core(df, expected_interval_ms=expected_interval_ms)
    valid = out["cpr_valid"].to_numpy() == 1.0
    hi = out["cpr_prior_high"].to_numpy()
    lo = out["cpr_prior_low"].to_numpy()
    s4 = out["cpr_s4"].to_numpy()
    r4 = out["cpr_r4"].to_numpy()
    prev_c = out["cpr_prev_close"].to_numpy()
    c = out["cpr_close"].to_numpy()
    with np.errstate(invalid="ignore"):
        long_ = (valid & (prev_c <= lo) & (c > lo) & (c < hi)
                 & (out["cpr_prev_low"].to_numpy() > s4) & (out["cpr_low"].to_numpy() > s4))
        short = (valid & (prev_c >= hi) & (c < hi) & (c > lo)
                 & (out["cpr_prev_high"].to_numpy() < r4) & (out["cpr_high"].to_numpy() < r4))
    signal = np.zeros(len(out), dtype=np.int64)
    signal[long_] = 1
    signal[short] = -1
    out["pdr_signal"] = signal
    out["signal"] = signal
    return out


@contextlib.contextmanager
def installed_comparator(reg):
    if LOCAL_COMPARATOR in reg.STRATEGY_REGISTRY:
        raise SystemExit(f"{LOCAL_COMPARATOR} is already present in the research registry")
    reg.STRATEGY_REGISTRY[LOCAL_COMPARATOR] = {
        "fn": prior_day_reclaim,
        "description": "study-local prior-day high/low reclaim comparator (#1669); never registered for production",
        "default_params": {"expected_interval_ms": 3_600_000},
        "constraints": (),
        "edge_status": None,
        "edge_source": None,
        "edge_ref": None,
        "param_validator": None,
    }
    try:
        yield
    finally:
        reg.STRATEGY_REGISTRY.pop(LOCAL_COMPARATOR, None)


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
    return {k: v for k, v in summary.items() if k not in ("per_dataset", "positions_list")}


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
        self.windows = {k: tuple(v) for k, v in proto["proxy"]["windows"].items()}
        self.symbols = proto["proxy"]["symbols"]
        self.memo = {}
        self.proxy_manifest = proxy_manifest
        self.venue_manifest = venue_manifest
        self.venue_datasets = ew.manifest_datasets(venue_manifest)
        self.venue_memo = {}
        self.comparator_params = {c: dict(self.reg.STRATEGY_REGISTRY[c]["default_params"])
                                  for c in proto["comparators"] if c != LOCAL_COMPARATOR}

    def spec(self, name: str, interval: int) -> dict:
        if name in (self.cand, LOCAL_COMPARATOR):
            params = {"expected_interval_ms": interval}
        else:
            params = dict(self.comparator_params[name])
        spec = {"name": name, "type": self.proto["strategy_type"], "params": params}
        spec.update(copy.deepcopy(self.contract))
        return spec

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


def _beats(cand: dict, other: dict) -> bool:
    return (cand.get("mean_sharpe") or 0.0) > (other.get("mean_sharpe") or 0.0)


def proxy_verdict(proto: dict, held: dict) -> dict:
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
    }
    for comp, label in COMPARATOR_CHECK_KEYS.items():
        checks[f"oos_beats_{label}_sharpe"] = tri(_beats(cand, oos[comp]))
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
    return {"outcome": outcome, "reason": reason, "checks": checks, "secondary_m1_passes": passes,
            "unscorable_windows": {w: v for w, v in unscorable.items() if v},
            "oos_clusters": cand["clusters"]}


def venue_verdict(proto: dict, held: dict, provenance_kind: str) -> dict:
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
    }
    for comp, label in COMPARATOR_CHECK_KEYS.items():
        checks[f"beats_{label}_sharpe_x1"] = _beats(cand, base[comp])
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
    if proxy_held:
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["funding_incomplete_legs"] = 1
        out["proxy_oos_funding_incomplete"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"][LOCAL_COMPARATOR]["datasets"] = 2
        out["proxy_oos_missing_comparator_dataset"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"]["candidate"]["clusters"] = floor - 1
        out["proxy_below_sample_floor"] = proxy_verdict(proto, g)["outcome"]
        g = copy.deepcopy(proxy_held)
        g["oos"]["mean_reversion_pro"]["incumbent_funding_incomplete"] = 1
        out["proxy_incumbent_funding_incomplete"] = proxy_verdict(proto, g)["outcome"]
    if venue_held:
        g = copy.deepcopy(venue_held)
        g["candidate"]["stress"]["funding_incomplete_legs"] = 1
        out["venue_funding_incomplete"] = venue_verdict(proto, g, "venue")["outcome"]
        g = copy.deepcopy(venue_held)
        g[LOCAL_COMPARATOR]["base"]["coverage_complete"] = False
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


DIAG = ("cpr_eval_ts_ms", "cpr_prior_day_open_ms", "cpr_prior_high", "cpr_prior_low", "cpr_prior_close",
        "cpr_range", "cpr_s3", "cpr_r3", "cpr_s4", "cpr_r4", "cpr_prev_close", "cpr_close", "cpr_prev_low",
        "cpr_low", "cpr_prev_high", "cpr_high")


def entry_audit(reg, cand: str, frames: dict, params_by_ds: dict, positions: list) -> list:
    import pandas as pd
    from camarilla_pivot_reversal import camarilla_levels
    outs = {k: reg.apply_strategy(cand, f, dict(params_by_ds[k])) for k, f in frames.items()}
    rows = []
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]], outs[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig = fill - 1
        rec = out.iloc[sig]
        want = 1 if p["side"] == "long" else -1
        if int(rec["signal"]) != want or rec["cpr_valid"] != 1.0:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} has no valid {p['side']} signal on the "
                             "prior bar")
        day0 = pd.Timestamp(int(rec["cpr_prior_day_open_ms"]), unit="ms", tz=frame.index.tz)
        prior = frame[(frame.index >= day0) & (frame.index < day0 + pd.Timedelta(days=1))]
        lv = camarilla_levels(float(prior["high"].max()), float(prior["low"].min()), float(prior["close"].iloc[-1]))
        for key in ("s3", "r3", "s4", "r4"):
            if abs(lv[key] - float(rec["cpr_" + key])) > 1e-9 * max(1.0, abs(lv[key])):
                raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} {key} disagrees with the "
                                 "independent prior-day recomputation")
        span = pd.concat([prior, frame.iloc[sig - 1:sig + 1]])
        item = {"dataset": p["dataset"], "side": p["side"], "signal_bar": str(out.index[sig]),
                "fill_bar": str(out.index[fill]), "exit_bar": p["exit_date"],
                "exit_reasons": sorted({r.split(":")[0] for r in p.get("exit_reasons") or [] if r}),
                "net_pnl_usd": p["net_pnl"], "entry_notional_usd": round(p["entry_notional"], 6),
                "reason": rec["cpr_reason"], "prior_day_bars": int(len(prior)),
                "input_sha256": frame_sha256(span), "independent_levels_match": True}
        for col in DIAG:
            item[col[4:]] = float(rec[col])
        rows.append(item)
    return rows


def hold_inventory(reg, cand: str, frames: dict, params_by_ds: dict, starts: dict) -> dict:
    import pandas as pd
    out = {}
    for key, frame in sorted(frames.items()):
        res = reg.apply_strategy(cand, frame, dict(params_by_ds[key]))
        start = pd.Timestamp(starts[key])
        if res.index.tz is not None and start.tzinfo is None:
            start = start.tz_localize(res.index.tz)
        scored = res[res.index >= start]
        counts = scored["cpr_reason"].value_counts().to_dict()
        valid = scored["cpr_valid"].to_numpy() == 1.0
        first_valid = scored.index[valid].min() if valid.any() else None
        out[key] = {"bars": int(len(scored)), "reasons": {k: int(v) for k, v in sorted(counts.items())},
                    "first_valid_bar": str(first_valid) if first_valid is not None else None,
                    "loaded_bars": int(len(frame)), "frame_sha256": frame_sha256(frame),
                    "first_loaded_bar": str(frame.index[0]), "last_bar": str(frame.index[-1])}
    return out


def _arms(study: Study, interval: int) -> dict:
    arms = {"candidate": study.spec(study.cand, interval)}
    arms.update({c: study.spec(c, interval) for c in study.proto["comparators"]})
    return arms


def run_proxy_group(study: Study, tf: str) -> dict:
    proto = study.proto
    from closed_bar import FIXED_DURATION_MS
    interval = FIXED_DURATION_MS[tf]
    arms = _arms(study, interval)
    windows = ([proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"]
               + proto["proxy"]["diagnostic_windows"])
    full, held = {}, {}
    for w in windows:
        full[w] = {k: study.proxy(a, tf, w) for k, a in arms.items()}
        held[w] = {k: brief(s) for k, s in full[w].items()}
        for k in arms:
            held[w][k]["per_dataset"] = full[w][k]["per_dataset"]
        print(f"[proxy {tf}] {w}: candidate {held[w]['candidate']['m1_verdict']} "
              f"net {held[w]['candidate']['net_pnl_usd']}", flush=True)
    scored = {w: held[w] for w in [proto["proxy"]["primary_window"]] + proto["proxy"]["secondary_windows"]}
    verdict = proxy_verdict(proto, scored)
    primary = proto["proxy"]["primary_window"]
    params = {"expected_interval_ms": interval}
    frames, params_by_ds = {}, {}
    for symbol in study.symbols:
        key = study.ew.dataset_key(symbol, tf)
        frames[key] = proxy_frame(symbol, tf, study.windows[primary], study.warmup_for(tf, primary))
        params_by_ds[key] = dict(params)
    audit = entry_audit(study.reg, study.cand, frames, params_by_ds, full[primary]["candidate"]["positions_list"])
    holds = {}
    for w in windows:
        wf, starts = {}, {}
        for s in study.symbols:
            key = study.ew.dataset_key(s, tf)
            wf[key] = proxy_frame(s, tf, study.windows[w], study.warmup_for(tf, w))
            starts[key] = study.windows[w][0]
        holds[w] = hold_inventory(study.reg, study.cand, wf, params_by_ds, starts)
    return {"expected_interval_ms": interval, "params": params, "comparator_params": study.comparator_params,
            "held_out": held, "verdict": verdict, "entry_audit": audit, "hold_inventory": holds,
            "warmup_by_window": {w: study.warmup_for(tf, w) for w in windows}}


def run_venue_group(study: Study) -> dict:
    proto = study.proto
    from closed_bar import FIXED_DURATION_MS
    tf = proto["venue"]["interval"]
    interval = FIXED_DURATION_MS[tf]
    arms = _arms(study, interval)
    costs = proto["venue"]["cost_multipliers"]
    held_window, second = proto["venue"]["held_out_window"], proto["venue"]["secondary_window"]
    out_windows = {}
    full_test = None
    for w in (held_window, second):
        full = {k: {label: study.venue(a, w, mult) for label, mult in costs.items()} for k, a in arms.items()}
        held = {k: {label: brief(s) for label, s in v.items()} for k, v in full.items()}
        for k in held:
            held[k]["base"]["per_dataset"] = full[k]["base"]["per_dataset"]
        out_windows[w] = held
        if w == held_window:
            full_test = full
        print(f"[venue] {w}: candidate {held['candidate']['base']['m1_verdict']} "
              f"net {held['candidate']['base']['net_pnl_usd']}", flush=True)
    verdict = venue_verdict(proto, out_windows[held_window], study.venue_manifest["provenance"]["kind"])
    secondary_verdict = venue_verdict(proto, out_windows[second], study.venue_manifest["provenance"]["kind"])
    import offline_manifest as om
    params = {"expected_interval_ms": interval}
    frames, params_by_ds = {}, {}
    for ds in study.venue_manifest["datasets"]:
        frame, _, _ = om.window_frame(study.venue_manifest, ds, held_window)
        frames[ds["key"]] = frame
        params_by_ds[ds["key"]] = dict(params)
    audit = entry_audit(study.reg, study.cand, frames, params_by_ds, full_test["candidate"]["base"]["positions_list"])
    holds = {}
    for w in (held_window, second):
        wf, starts = {}, {}
        for ds in study.venue_manifest["datasets"]:
            frame, _, _ = om.window_frame(study.venue_manifest, ds, w)
            wf[ds["key"]] = frame
            starts[ds["key"]] = study.venue_manifest["windows"][w]["start"]
        holds[w] = hold_inventory(study.reg, study.cand, wf, params_by_ds, starts)
    return {"expected_interval_ms": interval, "params": params, "comparator_params": study.comparator_params,
            "held_out": out_windows[held_window], "secondary": out_windows[second], "verdict": verdict,
            "secondary_diagnostic_verdict": secondary_verdict, "entry_audit": audit, "hold_inventory": holds}


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


def interval_stability(groups: dict) -> list:
    rows = []
    for gname, g in groups.items():
        windows = {"test": g["held_out"], "train": g.get("secondary")} if gname.startswith("venue") else g["held_out"]
        for w, arms in windows.items():
            if arms is None:
                continue
            cand = arms["candidate"]["base"] if gname.startswith("venue") else arms["candidate"]
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
    study = Study(proto, frozen["proxy"], frozen["venue"])
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
    with installed_comparator(study.reg):
        for g in wanted:
            if g.startswith("proxy_"):
                out["groups"][g] = run_proxy_group(study, g.split("_", 1)[1])
            else:
                out["groups"][g] = run_venue_group(study)
            print(f"[{g}] verdict {out['groups'][g]['verdict']['outcome']}: {out['groups'][g]['verdict']['reason']}",
                  flush=True)
    out["local_comparator_removed"] = LOCAL_COMPARATOR not in study.reg.STRATEGY_REGISTRY
    if not out["local_comparator_removed"]:
        raise SystemExit(f"{LOCAL_COMPARATOR} is still installed in the research registry")
    out["verdict"] = overall_verdict({g: v["verdict"] for g, v in out["groups"].items()})
    out["verdict_guards"] = {}
    for g, v in out["groups"].items():
        if g.startswith("proxy_"):
            primary = proto["proxy"]["primary_window"]
            scored = {w: v["held_out"][w] for w in [primary] + proto["proxy"]["secondary_windows"]}
            out["verdict_guards"][g] = verdict_guards(proto, scored, {})
        else:
            out["verdict_guards"][g] = verdict_guards(proto, {}, v["held_out"])
    out["interval_stability"] = interval_stability(out["groups"])
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']['outcome']}; wrote {RESULTS_PATH}")


def synthetic_frame(tf: str):
    import numpy as np
    import pandas as pd
    from closed_bar import FIXED_DURATION_MS, TIMESTAMP_KIND_OPEN, declare_frame_cadence
    rng = np.random.default_rng(SYNTHETIC_SEED + FIXED_DURATION_MS[tf] // 60_000)
    n = PARITY_BARS
    step = FIXED_DURATION_MS[tf]
    vol = 0.004 * (step / 3_600_000) ** 0.5
    close = 30_000.0 * np.exp(np.cumsum(rng.normal(0.0, vol, n)))
    open_ = np.concatenate([[30_000.0], close[:-1]])
    wick = np.abs(rng.normal(0.0, vol, n))
    high = np.maximum(open_, close) * (1.0 + wick)
    low = np.minimum(open_, close) * (1.0 - wick)
    idx = pd.date_range(SYNTHETIC_START, periods=n, freq=pd.Timedelta(milliseconds=step), tz="UTC")
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close,
                       "volume": 1000.0 + rng.random(n) * 100.0}, index=idx)
    df.index.name = "datetime"
    return declare_frame_cadence(df, tf, TIMESTAMP_KIND_OPEN)


def aggregated_frame(base, tf: str):
    import pandas as pd
    from closed_bar import FIXED_DURATION_MS, TIMESTAMP_KIND_OPEN, declare_frame_cadence
    step = FIXED_DURATION_MS[tf]
    per = step // FIXED_DURATION_MS["1h"]
    key = base.index.floor(pd.Timedelta(milliseconds=step))
    grouped = base.groupby(key)
    agg = grouped.agg({"open": "first", "high": "max", "low": "min", "close": "last", "volume": "sum"})
    agg = agg[grouped.size() == per]
    agg.index.name = "datetime"
    return declare_frame_cadence(agg.tail(PARITY_BARS).copy(), tf, TIMESTAMP_KIND_OPEN)


def parity_sources(frozen: dict) -> list:
    import pandas as pd
    import offline_manifest as om
    from closed_bar import FIXED_DURATION_MS, TIMESTAMP_KIND_OPEN, declare_frame_cadence
    end = "2026-10-01"
    sources = []
    for tf in PARITY_INTERVALS:
        step = FIXED_DURATION_MS[tf]
        if tf in ("15m", "30m"):
            sources.append((tf, f"synthetic seeded walk {tf} ({PARITY_BARS} bars, seed {SYNTHETIC_SEED})",
                            synthetic_frame(tf)))
            continue
        if tf in ("1h", "4h"):
            start = pd.Timestamp(end) - pd.Timedelta(milliseconds=step * PARITY_BARS)
            df = proxy_frame("BTC/USDT", tf, (str(start.date()), end))
            df = declare_frame_cadence(df.copy(), tf, TIMESTAMP_KIND_OPEN)
            sources.append((tf, f"proxy BTC/USDT {tf} {df.index[0].date()}..{end}", df))
            continue
        per = step // FIXED_DURATION_MS["1h"]
        start = pd.Timestamp(end) - pd.Timedelta(milliseconds=step * (PARITY_BARS + 2))
        base = proxy_frame("BTC/USDT", "1h", (str(start.date()), end))
        df = aggregated_frame(base, tf)
        sources.append((tf, f"proxy BTC/USDT 1h aggregated to {tf} (complete groups of {per}) "
                            f"{df.index[0].date()}..{end}", df))
    vds = om.dataset_by_key(frozen["venue"], "BTC 4h")
    vframe, _, _ = om.window_frame(frozen["venue"], vds, "test")
    sources.append(("4h", "venue BTC 4h test window with warmup", vframe))
    return sources


def parity(cache_db: str) -> None:
    proto = load_json(PROTOCOL_PATH)
    require_lock()
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    forbid_network()
    from parity_diff import compute_parity_frame, config_from_live_config, summarize as parity_summarize
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "venue_manifest_sha256": sha256_file(VENUE_MANIFEST_PATH),
           "source_commit": git_head(), "runs": []}
    for tf, label, df in parity_sources(frozen):
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
    out["clean"] = all(r["clean"] and r["bars_compared"] > 0 and r["mismatches"] == 0
                       and (not r["batched"] or r["batch_mismatches"] == 0)
                       and r["close_eligibility"] == "eligible" and r["incomplete_parity"] is False
                       for r in out["runs"])
    out["intervals_covered"] = sorted({r["interval"] for r in out["runs"] if r["bars_compared"] > 0},
                                      key=PARITY_INTERVALS.index)
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
             "| clusters | turnover | fees $ | funding $ | incomplete legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, s in arms.items():
        lines.append(f"| {name} | {s.get('m1_verdict')} | {fmt(s.get('mean_sharpe'))} | {fmt(s.get('mean_return_pct'))} "
                     f"| {fmt(s.get('net_pnl_usd'))} | {fmt(s.get('worst_max_dd_pct'))} | {s.get('positions')} "
                     f"({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('clusters')} "
                     f"| {fmt(s.get('mean_turnover'))} | {fmt(s.get('fees_usd'))} | {fmt(s.get('funding_pnl_usd'))} "
                     f"| {s.get('funding_incomplete_legs')} |")
    return lines


def _concentration(s: dict) -> str:
    return (f"top dataset share of absolute net PnL {fmt(s.get('top_dataset_abs_pnl_share'))}, top five positions' "
            f"share {fmt(s.get('top5_position_abs_pnl_share'))}, long net ${fmt(s.get('long_net_pnl_usd'))}, short "
            f"net ${fmt(s.get('short_net_pnl_usd'))}")


ENTRY_REASONS = ("no_cross", "entry_long", "entry_short", "level_equal", "long_opposite_jump", "short_opposite_jump",
                 "long_outer_breach", "short_outer_breach", "first_bar_of_day")


def _holds_lines(holds: dict) -> list:
    lines = ["| window | dataset | scored bars | first valid bar | entries L/S | hold reasons |",
             "|---|---|---|---|---|---|"]
    for w, per in holds.items():
        for ds, h in per.items():
            reasons = ", ".join(f"{k} {v}" for k, v in h["reasons"].items() if k not in ENTRY_REASONS)
            lines.append(f"| {w} | {ds} | {h['bars']} | {h['first_valid_bar']} | "
                         f"{h['reasons'].get('entry_long', 0)}/{h['reasons'].get('entry_short', 0)} "
                         f"| {reasons or '-'} |")
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
    L.append("# Camarilla pivot reversal study (issue 1669)")
    L.append("")
    L.append(f"**Verdict: {v['outcome'].upper()}** (proxy 1h: {v['groups'].get('proxy_1h')}, proxy 4h: "
             f"{v['groups'].get('proxy_4h')}, venue 4h: {v['groups'].get('venue_4h')}). Registry metadata after this "
             f"study: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`, `edge_ref` = this file. The "
             "candidate stays hidden from discovery. Explicit paper mode is admitted; live use still needs "
             "`allow_no_edge: true`, and no result here adds one or approves deployment. Every command in this study "
             "is suggest-only and sends no order. The proxy groups are proxy evidence (Binance US candles with "
             "Hyperliquid fees and funding); the venue group is Hyperliquid candle evidence.")
    L.append("")
    L.append("## Signal")
    L.append("")
    L.append("`camarilla_pivot_reversal` (`shared_strategies/open/camarilla_pivot_reversal.py`) reads the previous "
             "complete UTC day (00:00 to 24:00, by bar opening time) and freezes its high H, low L and final close C "
             "for the current UTC day: S3 = C - 1.1 x (H - L) / 4, R3 = C + 1.1 x (H - L) / 4, S4 = C - 1.1 x "
             "(H - L) / 2, R4 = C + 1.1 x (H - L) / 2. It enters long when the preceding closed candle closes at or "
             "below S3 and the current closed candle closes strictly above S3 and strictly below R3 with both lows "
             "strictly above S4, and short on the mirror at R3 with both highs strictly below R4. Both candles are "
             "consecutive bars of the current UTC day; a jump across the opposite level holds. Every bar of the prior "
             "day must be present, aligned and well formed; the frame must declare its bar interval and timestamp "
             "kind; a later defect never changes an earlier decision; levels never carry over a missing day. Bad "
             "entry data gives a reasoned hold (`cpr_reason`), and close and stop management continue. The evaluator "
             "tunes nothing.")
    L.append("")
    L.append("## Protocol")
    L.append("")
    lock_ = res["protocol_lock"]
    L.append(f"`protocol.json` (sha256 `{res['protocol_sha256']}`) was locked by `run_study.py lock` at "
             f"{lock_['locked_at_utc']} (commit `{lock_['source_commit']}`), before any window was scored; `run` "
             "and `parity` refuse a different protocol. It fixes the formula, the matched contract, the windows, the "
             "warm-up policy, the sample floor and the pass/fail/inconclusive rules. No parameter is selected.")
    L.append("")
    c = proto["contract"]
    first_group = next(iter(res["groups"].values()))
    L.append(f"Contract for the candidate, the three comparators and the M1 incumbent median: direction "
             f"`{c['direction']}`, one `tiered_tp_pct` close (+3%, full position), one fixed entry-ATR stop "
             f"(`stop_loss_atr_mult: {c['stop_loss_atr_mult']}`), `comparison_mode: {c['comparison_mode']}`, "
             f"`intrabar_resolution: {proto['intrabar_resolution']}`, no regime gate, capital "
             f"${proto['capital_usd']:.0f} per leg. Comparators keep their registry defaults: "
             + "; ".join(f"`{k}` {json.dumps(p, sort_keys=True)}" for k, p in first_group["comparator_params"].items())
             + f". `{LOCAL_COMPARATOR}` is the study-local prior-day reclaim: "
             + proto["study_local_comparator"]["definition"] + ". "
             + proto["study_local_comparator"]["registration"]
             + f" (removed after the run: {fmt(res.get('local_comparator_removed'))}).")
    L.append("")
    L.append("Warm-up: " + proto["warmup"]["policy"] + ". " + proto["warmup"]["restriction"] + ".")
    L.append("")
    L.append("Proxy group rule: " + proto["verdict_rules"]["proxy_group"]["order"] + ". Venue group rule: "
             + proto["verdict_rules"]["venue_group"]["order"] + ". Overall: " + proto["verdict_rules"]["overall"] + ".")
    L.append("")
    L.append("| proxy window | start | end (exclusive) | role |")
    L.append("|---|---|---|---|")
    for w, (s, e) in proto["proxy"]["windows"].items():
        role = ("primary held-out" if w == proto["proxy"]["primary_window"] else
                "diagnostic" if w in proto["proxy"]["diagnostic_windows"] else "secondary held-out")
        L.append(f"| {w} | {s} | {e} | {role} |")
    L.append("")
    L.append("Departures from the default M1 windows: " + "; ".join(proto["proxy"]["departures_from_m1_windows"])
             + ". " + proto["proxy"]["window_note"] + ".")
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
             "`eval_windows.run_leg(funding_mode=\"charge\")`) for every candidate, comparator and incumbent leg.")
    L.append(f"- Proxy costs: engine Hyperliquid commission `{eng['commission_pct']}` per side and engine slippage "
             f"`{eng['slippage_pct']}` per fill; the slippage term stands in for the spread. This legacy M1 screen "
             "replays no spread, lot rounding or minimum order, so it cannot by itself establish venue execution "
             "parity.")
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
    notionals = []
    for gname, g in res["groups"].items():
        arms = ([g["held_out"]["candidate"]["base"]] if gname.startswith("venue")
                else [w["candidate"] for w in g["held_out"].values()])
        for arm in arms:
            for item in arm.get("per_dataset", {}).values():
                for k in ("min_entry_notional_usd", "max_entry_notional_usd"):
                    if item.get(k) is not None:
                        notionals.append(item[k])
    venue_g = res["groups"].get("venue_4h")
    rejected = venue_g["held_out"]["candidate"]["base"].get("rejected_entries") if venue_g else None
    span = f"${min(notionals):.2f} to ${max(notionals):.2f}" if notionals else "none (no entries)"
    L.append(f"- Sizing: capital ${eng['capital_usd']:.0f} per leg, no leverage and no perps sizing block (Backtester "
             f"leverage {eng['leverage']}, sizing defaults {json.dumps(eng['sizing_defaults'], sort_keys=True)}), so "
             "each entry commits the leg's available cash unlevered; candidate entry notionals ranged from "
             f"{span}. Venue lot rounding and the $10 minimum rejected {fmt(rejected)} candidate entries in the held-out "
             "venue window at cost x1, so the minimum does not bind at this size; lot rounding trims each order by at "
             "most one szDecimals step.")
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
        if venue:
            for label in ("base", "stress"):
                L.append(f"### Held-out `test`, cost {label}")
                L.append("")
                L.extend(_arm_table({k: v_[label] for k, v_ in g["held_out"].items()}))
                L.append("")
            L.append("### Secondary `train`, cost base (diagnostic verdict: "
                     f"{g['secondary_diagnostic_verdict']['outcome']}, {g['secondary_diagnostic_verdict']['reason']})")
            L.append("")
            L.extend(_arm_table({k: v_["base"] for k, v_ in g["secondary"].items()}))
            L.append("")
            cand = g["held_out"]["candidate"]["base"]
            L.append("Concentration (candidate, test, cost x1): " + _concentration(cand) + ".")
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
                L.append(f"### `{w}` (warm-up {g['warmup_by_window'][w]} bars)")
                L.append("")
                L.extend(_arm_table(arms))
                L.append("")
            cand = g["held_out"][proto["proxy"]["primary_window"]]["candidate"]
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
        L.append("Start-of-window history and input holds (scored bars only; warm-up bars are read but not counted):")
        L.append("")
        L.extend(_holds_lines(g["hold_inventory"]))
        L.append("")
        audit = g["entry_audit"]
        L.append(f"Entry audit: {len(audit)} held-out candidate entries; each fill follows a valid raw signal on the "
                 "prior closed bar, its S3/R3/S4/R4 equal an independent recomputation from the prior-day candles, "
                 "and `results.json` stores the input span hash and every level. First entries:")
        L.append("")
        L.append("| dataset | side | signal bar | fill bar | prior H | prior L | prior C | S3 | R3 | prev close | close "
                 "| net $ |")
        L.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
        for a in audit[:10]:
            L.append(f"| {a['dataset']} | {a['side']} | {a['signal_bar']} | {a['fill_bar']} | {a['prior_high']:.6g} "
                     f"| {a['prior_low']:.6g} | {a['prior_close']:.6g} | {a['s3']:.6g} | {a['r3']:.6g} "
                     f"| {a['prev_close']:.6g} | {a['close']:.6g} | {fmt(a['net_pnl_usd'])} |")
        L.append("")
    L.append("## Stability across intervals and windows")
    L.append("")
    L.append("| group | window | M1 | mean Sharpe | net $ | positions | clusters | long net $ | short net $ "
             "| funding-incomplete legs |")
    L.append("|---|---|---|---|---|---|---|---|---|---|")
    for r in res["interval_stability"]:
        L.append(f"| {r['group']} | {r['window']} | {r['m1_verdict']} | {fmt(r['mean_sharpe'])} "
                 f"| {fmt(r['net_pnl_usd'])} | {r['positions']} | {r['clusters']} | {fmt(r['long_net_pnl_usd'])} "
                 f"| {fmt(r['short_net_pnl_usd'])} | {r['funding_incomplete_legs']} |")
    L.append("")
    L.append("The regime and close settings were frozen as one configuration (no regime gate, one +3% close, one "
             "1.0 x ATR stop), so the only variation measured is across intervals and windows.")
    L.append("")
    L.append("Verdict guards (each altered copy must not pass): " + "; ".join(
        f"{g}: " + ", ".join(f"{k} -> {v_}" for k, v_ in guards.items()) for g, guards in res["verdict_guards"].items())
        + ".")
    L.append("")
    L.append("## Decision parity")
    L.append("")
    if par:
        L.append("`run_study.py parity` ran the explicit-paper configurations `paper_parity_<interval>.json` through "
                 "`backtest/parity_diff.compute_parity_frame` with `--registry futures --platform hyperliquid`, "
                 "strict close comparison and charged funding: non-batched and batched (solo vs batch slots through "
                 "the real Hyperliquid check evaluator), on the trailing 200-bar window and on every prefix. "
                 f"Intervals covered: {', '.join(par['intervals_covered'])}. Overall clean: {fmt(par['clean'])}.")
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
        L.append("Strict mode with an eligible close set and no approximation is the exact supported comparison mode; "
                 "the runner labels it `unverified` because it never claims more than decision agreement plus an "
                 "eligible close set. The 15m and 30m runs use a seeded synthetic walk and the 2h, 8h and 12h runs "
                 "aggregate frozen 1h proxy candles; they prove decision parity only.")
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
             "rate is funding_incomplete, and such a window can never support a pass. The 2023 window also has no "
             "prehistory for the warm-up.")
    L.append("- The venue windows cover 2024-09-01 to 2026-09-01 only (the venue serves the most recent 5,000 "
             "candles), and earlier studies already used them, so the venue result is retrospective. Spread is a "
             "per-coin half spread from one frozen L2 snapshot plus flat slippage; queue position, partial fills and "
             "market impact are not modelled.")
    L.append("- Only 1h and 4h are scored; the other supported intervals have no frozen candles.")
    L.append("- Every score reads only frozen inputs with network acquisition disabled. A research result alone "
             "cannot approve deployment.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append("uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py lock")
    L.append("uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py run "
             "--cache-db /tmp/cpr1669_run.db")
    L.append("uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py parity "
             "--cache-db /tmp/cpr1669_parity.db")
    L.append("uv run --no-sync python backtest/candidates/camarilla_pivot_reversal_1669/run_study.py render")
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
    p = argparse.ArgumentParser(description="#1669 Camarilla pivot reversal study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("lock", help="record protocol.json's sha256 before any score")
    sub.add_parser("hash", help="print protocol.json's sha256")
    r = sub.add_parser("run", help="rebuild a fresh cache from the frozen files and run the protocol offline")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--group", action="append", choices=["proxy_1h", "proxy_4h", "venue_4h"])
    q = sub.add_parser("parity", help="decision parity of the paper configurations on every supported interval")
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
