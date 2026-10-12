#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import io
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
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
EVIDENCE_DIR = os.path.join(HERE, "evidence")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
TRIX_EVIDENCE_COLUMNS = ["trix_close", "trix_prev_close", "trix_seed_close", "trix_prev_seed_close", "trix_ema3",
                         "trix_ema3_base", "trix_prev_ema3", "trix_prev_ema3_base", "trix_value", "trix_prev_value",
                         "trix_signal_line", "trix_prev_signal_line", "trix_diff", "trix_prev_diff",
                         "trix_required_history"]
EDGE_SOURCE = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_protocol() -> dict:
    with open(PROTOCOL_PATH) as fh:
        return json.load(fh)


def read_gz_csv(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        rows = list(csv.reader(fh))
    return rows[1:]


def write_gz_csv(path: str, header: list, rows: list) -> None:
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(header)
    w.writerows(rows)
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(buf.getvalue().encode())


def write_json(path: str, obj) -> None:
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True, default=str)
        fh.write("\n")


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def source_hashes() -> dict:
    paths = ["shared_strategies/open/trix_momentum.py", "shared_strategies/open/registry.py",
             "backtest/eval_windows.py", "backtest/backtester.py", "backtest/parity_diff.py",
             "shared_strategies/close/tiered_tp_pct.py", "backtest/candidates/trix_momentum_1659/run_study.py"]
    return {p: sha256_file(os.path.join(REPO, p)) for p in paths}


def source_dir(proto: dict) -> str:
    return os.path.join(REPO, proto["data"]["source_dir"])


def verify_inputs(proto: dict) -> dict:
    data = proto["data"]
    src_manifest = os.path.join(REPO, data["source_manifest"])
    got = sha256_file(src_manifest)
    if got != data["source_manifest_sha256"]:
        raise SystemExit(f"source manifest {src_manifest} sha256 {got} != pinned {data['source_manifest_sha256']}")
    with open(src_manifest) as fh:
        source = json.load(fh)
    if sorted(source["files"]) != sorted(data["files"]):
        raise SystemExit("source manifest file set differs from the protocol pins")
    for name, pinned in sorted(data["files"].items()):
        path = os.path.join(source_dir(proto), name)
        got = sha256_file(path)
        if got != pinned or source["files"][name]["sha256"] != pinned:
            raise SystemExit(f"frozen input {name} sha256 {got} != pinned {pinned}")
    return source


def freeze() -> None:
    proto = load_protocol()
    source = verify_inputs(proto)
    manifest = {
        "study": proto["study"],
        "frozen_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": git_head(),
        "protocol_sha256": sha256_file(PROTOCOL_PATH),
        "data_reuse": "no new acquisition: the inputs are the hash-pinned frozen files of study "
                      f"{proto['data']['source_study']}, read in place; no copies",
        "source_manifest": {
            "path": proto["data"]["source_manifest"],
            "sha256": proto["data"]["source_manifest_sha256"],
            "acquired_at_utc": source.get("acquired_at_utc"),
            "source_commit": source.get("source_commit"),
            "candles": source.get("candles"),
            "funding": source.get("funding"),
            "acquisition_start": source.get("acquisition_start"),
            "pinned_end": source.get("pinned_end"),
        },
        "files": {name: {k: v for k, v in meta.items() if k != "venue_absent_hours"}
                  | {"venue_absent_hour_count": len(meta.get("venue_absent_hours") or [])}
                  for name, meta in sorted(source["files"].items())},
    }
    write_json(MANIFEST_PATH, manifest)
    print(f"verified {len(manifest['files'])} pinned inputs; wrote {MANIFEST_PATH}")


def verify_frozen(proto: dict) -> tuple:
    with open(MANIFEST_PATH) as fh:
        manifest = json.load(fh)
    if manifest["protocol_sha256"] != sha256_file(PROTOCOL_PATH):
        raise SystemExit("protocol.json changed after freeze; the frozen protocol is the contract")
    source = verify_inputs(proto)
    return manifest, source


def set_cache(path: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(path)
    for p in (BACKTEST, os.path.join(REPO, "shared_tools")):
        if p not in sys.path:
            sys.path.insert(0, p)


def build_cache(cache_db: str, proto: dict, source: dict, drop_funding=None) -> None:
    if os.path.exists(cache_db):
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; the run rebuilds it from the frozen files")
    set_cache(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    for name, meta in sorted(source["files"].items()):
        rows = read_gz_csv(os.path.join(source_dir(proto), name))
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp"] + INPUT_COLUMNS)
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float,
                            "close": float, "volume": float})
            storage.store_ohlcv(df, meta["exchange"], meta["symbol"], meta["timeframe"], db_path=cache_db)
        else:
            records = [{"time": int(t), "rate": float(r)} for t, r in rows]
            if drop_funding and meta["coin"] in drop_funding:
                lo, hi = drop_funding[meta["coin"]]
                records = [r for r in records if not (lo <= r["time"] < hi)]
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
    funding_fetcher._hl_adapter = refuse


class Capture:
    def __init__(self):
        self.runs = []

    def install(self):
        import backtester
        base = backtester.Backtester
        if getattr(base, "_trix_capture", False):
            base = base.__mro__[1]
        sink = self

        class CapturingBacktester(base):
            _trix_capture = True

            def run(self, df, *a, **k):
                out = super().run(df, *a, **k)
                sink.runs.append({"results": out, "signals": df, "indicator_frame": k.get("indicator_frame"),
                                  "slippage_pct": self.slippage_pct})
                return out

        backtester.Backtester = CapturingBacktester

    def take(self):
        if not self.runs:
            return None
        out = self.runs[-1]
        self.runs.clear()
        return out


def grid_combos(grid: dict) -> list:
    keys = list(grid)
    return [dict(zip(keys, vals)) for vals in itertools.product(*(grid[k] for k in keys))]


def next_bar_fill_check(run: dict) -> dict:
    import pandas as pd
    sig = run["signals"]
    trades = run["results"].get("trades") or []
    slip = float(run["slippage_pct"] or 0.0)
    checked = 0
    bad = []
    seen = set()
    for t in trades:
        key = (str(t["entry_date"]), t.get("side"))
        if key in seen:
            continue
        seen.add(key)
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        want = 1 if t.get("side") == "long" else -1
        opened = float(sig["open"].iloc[pos])
        filled = float(t["entry_price"])
        expect = opened * (1.0 + slip) if want == 1 else opened * (1.0 - slip)
        ok = pos > 0 and int(sig["signal"].iloc[pos - 1]) == want and abs(filled - expect) <= 1e-9 * max(1.0, expect)
        checked += 1
        if not ok:
            bad.append(key)
    return {"positions_checked": checked, "mismatches": len(bad), "first_mismatches": bad[:5],
            "first_scored_bar": str(sig.index[0]) if len(sig) else None}


def funding_coverage_counts(cov: dict) -> dict:
    present = cov.get("present_hours")
    absent = cov.get("venue_absent_hours")
    priced = present - absent if present is not None and absent is not None else None
    return {"expected_hours": cov.get("expected_hours"), "present_hours": present, "priced_hours": priced,
            "missing_hours": cov.get("missing_hours"), "venue_absent_hours": absent}


def leg_summary(leg, run, capital: float) -> dict:
    from eval_windows import execution_metrics
    if leg is None:
        return None
    results = run["results"]
    ex = execution_metrics(results, capital)
    positions = ex.get("position_list") or []
    block = leg.get("funding") or {}
    cov = block.get("coverage") or {}
    return {
        "sharpe": leg["sharpe"], "return_pct": leg["return_pct"], "max_dd_pct": leg["max_dd_pct"],
        "ddadj": leg["ddadj"], "trades": leg["trades"], "bh_return_pct": leg["bh_return_pct"],
        "liquidated": leg["liquidated"], "funding_incomplete": bool(leg.get("funding_incomplete")),
        "warmup": leg.get("warmup"),
        "positions": ex.get("positions", 0), "long_positions": ex.get("long_positions", 0),
        "short_positions": ex.get("short_positions", 0), "fees_usd": ex.get("fees_usd", 0.0),
        "funding_pnl_usd": ex.get("funding_pnl_usd", 0.0), "net_pnl_usd": ex.get("net_pnl_usd", 0.0),
        "traded_notional_usd": ex.get("traded_notional_usd", 0.0), "turnover": ex.get("turnover"),
        "bars_in_market": ex.get("bars_in_market", 0),
        "intervals": [[p["entry_date"], p["exit_date"]] for p in positions],
        "position_net_pnl": [round(p["net_pnl"], 6) for p in positions],
        "exit_reasons": sorted({r.split(":")[0] for p in positions for r in p["exit_reasons"] if r}),
        "partial_close_positions": sum(1 for p in positions if p["closes"] > 1),
        "next_bar_fills": next_bar_fill_check(run),
        "funding": {
            "mode": block.get("mode"), "source": block.get("source"), "complete": block.get("complete"),
            "sha256": block.get("sha256"), "row_count": block.get("row_count"),
            **funding_coverage_counts(cov), "coverage_complete": cov.get("complete"),
            "unpriced_held_hours": block.get("unpriced_held_hours"),
            "fetch_error": block.get("fetch_error"), "error": block.get("error"),
        },
    }


def continuous_trix(close, period: int, signal_period: int) -> tuple:
    import numpy as np
    n = len(close)
    osc = np.full(n, np.nan)
    line = np.full(n, np.nan)
    if n < 2:
        return osc, line
    a = 2.0 / (period + 1.0)
    b = 2.0 / (signal_period + 1.0)
    e1 = e2 = e3 = float(close[0])
    for j in range(1, n):
        base = e3
        e1 = e1 + a * (float(close[j]) - e1)
        e2 = e2 + a * (e1 - e2)
        e3 = e3 + a * (e2 - e3)
        osc[j] = 100.0 * (e3 - base) / base
        line[j] = osc[j] if j == 1 else line[j - 1] + b * (osc[j] - line[j - 1])
    return osc, line


def decisions(osc, line):
    import numpy as np
    diff = osc - line
    prev = np.concatenate([[np.nan], diff[:-1]])
    out = np.zeros(len(osc), dtype=int)
    with np.errstate(invalid="ignore"):
        out[(diff > 0) & (prev <= 0) & (osc > 0)] = 1
        out[(diff < 0) & (prev >= 0) & (osc < 0)] = -1
    return out


def continuous_reference(run: dict, params: dict) -> dict:
    import numpy as np
    sig = run["signals"]
    hist = run["indicator_frame"] if run["indicator_frame"] is not None else sig
    osc, line = continuous_trix(hist["close"].to_numpy(dtype=float), params["period"], params["signal_period"])
    pos = hist.index.get_indexer(sig.index)
    c_osc, c_line = osc[pos], line[pos]
    c_dec = decisions(osc, line)[pos]
    valid = sig["trix_valid"].to_numpy() == 1.0
    w_osc = sig["trix_value"].to_numpy(dtype=float)
    w_line = sig["trix_signal_line"].to_numpy(dtype=float)
    d_osc = np.abs(w_osc[valid] - c_osc[valid])
    d_line = np.abs(w_line[valid] - c_line[valid])
    w_dec = sig["signal"].to_numpy()[valid]
    return {"bars": int(valid.sum()),
            "max_abs_oscillator_diff": float(d_osc.max()) if len(d_osc) else None,
            "median_abs_oscillator_diff": float(np.median(d_osc)) if len(d_osc) else None,
            "max_abs_signal_line_diff": float(d_line.max()) if len(d_line) else None,
            "median_abs_signal_line_diff": float(np.median(d_line)) if len(d_line) else None,
            "decision_differences": int((w_dec != c_dec[valid]).sum()),
            "fixed_window_entries": int((w_dec != 0).sum()), "continuous_entries": int((c_dec[valid] != 0).sum())}


def entry_evidence(run: dict, reg, name: str, params: dict) -> list:
    sig = run["signals"]
    hist = run["indicator_frame"] if run["indicator_frame"] is not None else sig
    out = []
    for ts in sig.index[sig["signal"].to_numpy() != 0]:
        pos = hist.index.get_loc(ts)
        need = int(sig.loc[ts, "trix_required_history"])
        window = hist.iloc[pos - need + 1:pos + 1][INPUT_COLUMNS]
        body = window.to_csv(float_format="%r").encode()
        replay = reg.apply_strategy(name, window.copy(), params).iloc[-1]
        short = reg.apply_strategy(name, window.iloc[1:].copy(), params).iloc[-1]
        row = {"bar_open": str(ts), "signal": int(sig.loc[ts, "signal"]), "reason": sig.loc[ts, "trix_reason"],
               "input_first_bar": str(window.index[0]), "input_rows": len(window), "input_sha256": sha256_bytes(body),
               "exact_span_replay_matches": bool(
                   int(replay["signal"]) == int(sig.loc[ts, "signal"])
                   and float(replay["trix_value"]) == float(sig.loc[ts, "trix_value"])
                   and float(replay["trix_signal_line"]) == float(sig.loc[ts, "trix_signal_line"])
                   and float(replay["trix_prev_diff"]) == float(sig.loc[ts, "trix_prev_diff"])
                   and short["trix_reason"] == "insufficient_history")}
        for col in TRIX_EVIDENCE_COLUMNS:
            row[col] = float(sig.loc[ts, col])
        out.append(row)
    return out


def clusters(intervals: list) -> int:
    count = 0
    latest = None
    for entry, exit_ in sorted((str(a), str(b)) for a, b in intervals):
        if latest is None or entry > latest:
            count += 1
            latest = exit_
        elif exit_ > latest:
            latest = exit_
    return count


def row_intervals(rows: list) -> list:
    return [iv for r in rows for l in r["legs"].values() if l for iv in l["intervals"]]


class Runner:
    def __init__(self, proto: dict):
        import eval_windows as ew
        from registry_loader import load_registry
        self.proto = proto
        self.ew = ew
        self.capital = proto["capital_usd"]
        self.reg = load_registry(proto["registry"])
        self.capture = Capture()
        self.capture.install()
        self.bars = {}
        self.base_slippage = proto["cost_model"]["slippage_pct"]

    def window(self, name: str) -> tuple:
        s, e = self.proto["windows"][name]
        return (s, e)

    def leg(self, arm: str, params: dict, symbol: str, group: str, window: str, slippage_mult: float = 1.0,
            evidence: bool = False, reference: bool = False):
        spec = self.proto["arms"][arm]
        kwargs = {}
        if slippage_mult != 1.0:
            kwargs["slippage_pct"] = self.base_slippage * slippage_mult
        self.capture.runs.clear()
        leg = self.ew.run_leg(
            self.reg, spec["name"], dict(params), symbol, group, self.window(window), capital=self.capital,
            close_strategies=copy.deepcopy(self.proto["close_strategies"]),
            direction=spec["direction"],
            stop_loss_atr_mult=self.proto["stop_loss_atr_mult"],
            intrabar_resolution=self.proto["intrabar_resolution"],
            funding_mode=self.proto["funding_mode"],
            warmup_bars=self.proto["warmup_bars"], **kwargs)
        run = self.capture.take()
        summary = leg_summary(leg, run, self.capital)
        if summary is not None:
            if evidence:
                summary["entry_evidence"] = entry_evidence(run, self.reg, spec["name"], params)
            if reference:
                summary["continuous_reference"] = continuous_reference(run, params)
        return summary

    def incumbent_bars(self, group: str, window: str) -> dict:
        key = (group, window)
        if key not in self.bars:
            datasets = [(s, group) for s in self.proto["symbols"]]
            legs = self.ew.compute_incumbent_legs(
                self.reg, datasets, self.window(window), self.capital,
                intrabar_resolution=self.proto["intrabar_resolution"],
                funding_mode=self.proto["funding_mode"], warmup_bars=self.proto["warmup_bars"])
            self.capture.runs.clear()
            self.bars[key] = self.ew.incumbent_bars(legs)
        return self.bars[key]

    def evaluate(self, arm: str, params: dict, group: str, window: str, slippage_mult: float = 1.0,
                 evidence: bool = False, reference: bool = False) -> dict:
        legs = {}
        for symbol in self.proto["symbols"]:
            legs[self.ew.dataset_key(symbol, group)] = self.leg(arm, params, symbol, group, window, slippage_mult,
                                                                evidence, reference)
        bars = self.incumbent_bars(group, window)
        raw = {ds: ({k: l[k] for k in ("sharpe", "return_pct", "max_dd_pct", "ddadj", "trades", "bh_return_pct",
                                       "liquidated", "funding_incomplete")} if l else None)
               for ds, l in legs.items()}
        score = self.ew.score_candidate(raw, bars)
        present = [l for l in legs.values() if l is not None]
        return {"arm": arm, "params": dict(params), "group": group, "window": window,
                "window_range": list(self.window(window)), "slippage_multiplier": slippage_mult,
                "m1_verdict": score["verdict"],
                "mean_sharpe": statistics.mean(l["sharpe"] for l in present) if present else None,
                "mean_ddadj": statistics.mean(l["ddadj"] for l in present) if present else None,
                "mean_bar_sharpe": score.get("mean_bar_sharpe"), "mean_bar_ddadj": score.get("mean_bar_ddadj"),
                "clusters": clusters(row_intervals([{"legs": legs}])),
                "bars": bars, "legs": legs}


def select(rows: list, rule: dict, grid_keys: list, fallback: dict) -> dict:
    eligible = []
    for r in rows:
        legs = list(r["legs"].values())
        if any(l is None or l["funding_incomplete"] for l in legs):
            continue
        if r["clusters"] < rule["min_training_clusters"]:
            continue
        eligible.append(r)
    if not eligible:
        return {"params": dict(fallback), "how": "fallback", "eligible": 0}
    key = lambda r: (-r["mean_ddadj"], -r["mean_sharpe"]) + tuple(r["params"][k] for k in grid_keys)
    ranked = sorted(eligible, key=key)
    best = ranked[0]
    return {"params": {k: best["params"][k] for k in grid_keys}, "how": "selected", "eligible": len(eligible),
            "mean_ddadj": best["mean_ddadj"], "mean_sharpe": best["mean_sharpe"], "clusters": best["clusters"],
            "rank_of_seed": next((i + 1 for i, r in enumerate(ranked) if r.get("is_seed")), None)}


def neighbors(params: dict, grid: dict) -> list:
    out = []
    for k, values in grid.items():
        i = values.index(params[k])
        for j in (i - 1, i + 1):
            if 0 <= j < len(values):
                out.append(dict(params, **{k: values[j]}))
    return out


def window_unscorable(rows: dict) -> list:
    bad = []
    for label, row in rows.items():
        for ds, l in row["legs"].items():
            if l is None or l["funding_incomplete"]:
                bad.append(f"{label} {ds}")
    any_row = next(iter(rows.values()))
    for ds, bar in (any_row["bars"] or {}).items():
        if bar is None or bar.get("funding_incomplete"):
            bad.append(f"incumbent median {ds}")
    return bad


def _mean(values):
    values = list(values)
    return statistics.mean(values) if values else None


def group_verdict(proto: dict, g: dict, parity_ok, close_ok) -> dict:
    rules = proto["verdict_rules"]
    windows = proto["held_out_windows"]
    held = g["held_out"]
    stress = g["held_out_x2_slippage"]
    unscorable = {}
    for w in windows:
        rows = {arm: held[arm][w] for arm in held}
        rows.update({f"{arm} x2 slippage": stress[arm][w] for arm in stress})
        unscorable[w] = window_unscorable(rows)
    legs = {arm: [l for w in windows for l in held[arm][w]["legs"].values() if l is not None] for arm in held}
    stress_legs = {arm: [l for w in windows for l in stress[arm][w]["legs"].values() if l is not None]
                   for arm in stress}
    arm_clusters = {arm: sum(held[arm][w]["clusters"] for w in windows) for arm in held}
    both = proto["candidate_arms"][0]
    long_arm = proto["matched_long_arm"]
    gates = {
        "all_windows_scorable": not any(unscorable.values()),
        "selections_not_fallback": all(s["how"] == "selected" for s in g["selections"].values()),
        "candidate_clusters": all(arm_clusters[a] >= rules["min_held_out_clusters"] for a in proto["candidate_arms"]),
        "both_sides": (sum(l["long_positions"] for l in legs[both]) > 0
                       and sum(l["short_positions"] for l in legs[both]) > 0),
        "integration_parity": bool(parity_ok),
        "close_capability": bool(close_ok),
    }
    net = {arm: round(sum(l["net_pnl_usd"] for l in legs[arm]), 4) for arm in legs}
    net_stress = {arm: round(sum(l["net_pnl_usd"] for l in stress_legs[arm]), 4) for arm in stress_legs}
    sharpe = {arm: _mean(l["sharpe"] for l in legs[arm]) for arm in legs}
    ddadj = {arm: _mean(l["ddadj"] for l in legs[arm]) for arm in legs}
    conditions = {}
    for arm in proto["candidate_arms"]:
        conditions[f"{arm}_net_positive_base"] = net[arm] > 0
        conditions[f"{arm}_net_positive_stress"] = net_stress[arm] > 0
    for comp in proto["comparator_arms"]:
        conditions[f"{long_arm}_beats_{comp}_sharpe"] = (sharpe[long_arm] is not None and sharpe[comp] is not None
                                                         and sharpe[long_arm] > sharpe[comp])
        conditions[f"{long_arm}_beats_{comp}_ddadj"] = (ddadj[long_arm] is not None and ddadj[comp] is not None
                                                        and ddadj[long_arm] > ddadj[comp])
    assert set(conditions) == set(rules["pass_conditions"]), sorted(conditions)
    pos_pnl = [x for l in legs[both] for x in l["position_net_pnl"]]
    positive = sum(x for x in pos_pnl if x > 0)
    checks = {
        "unscorable_windows": {w: v for w, v in unscorable.items() if v},
        "clusters": arm_clusters,
        "positions": {arm: sum(l["positions"] for l in legs[arm]) for arm in legs},
        "long_positions": {arm: sum(l["long_positions"] for l in legs[arm]) for arm in legs},
        "short_positions": {arm: sum(l["short_positions"] for l in legs[arm]) for arm in legs},
        "net_pnl_usd": net, "net_pnl_usd_x2_slippage": net_stress,
        "mean_sharpe": {a: (round(v, 4) if v is not None else None) for a, v in sharpe.items()},
        "mean_ddadj": {a: (round(v, 4) if v is not None else None) for a, v in ddadj.items()},
        "fees_usd": {arm: round(sum(l["fees_usd"] for l in legs[arm]), 4) for arm in legs},
        "funding_pnl_usd": {arm: round(sum(l["funding_pnl_usd"] for l in legs[arm]), 4) for arm in legs},
        "turnover": {arm: round(sum(l["turnover"] or 0.0 for l in legs[arm]), 4) for arm in legs},
        "max_dd_pct": {arm: (round(max(abs(l["max_dd_pct"]) for l in legs[arm]), 4) if legs[arm] else None)
                       for arm in legs},
        "top_position_share_candidate_both": round(max(pos_pnl) / positive, 4) if pos_pnl and positive > 0 else None,
        "m1_passes_candidate_both": sum(1 for w in windows if held[both][w]["m1_verdict"] == "pass"),
    }
    failed_gates = sorted(k for k, v in gates.items() if not v)
    false = sorted(k for k, v in conditions.items() if not v)
    if failed_gates:
        verdict, why = "inconclusive", "evidence gate false: " + ", ".join(failed_gates)
    elif false:
        verdict, why = "fail", "pass condition false: " + ", ".join(false)
    else:
        verdict, why = "pass", "every evidence gate and pass condition true"
    return {"verdict": verdict, "reason": why, "gates": gates, "conditions": conditions, "checks": checks}


def overall_verdict(groups: dict) -> str:
    vs = set(groups.values())
    if vs == {"pass"}:
        return "pass"
    if vs == {"fail"}:
        return "fail"
    return "inconclusive"


def close_capability(proto: dict) -> dict:
    from backtester import validate_close_capabilities
    report = validate_close_capabilities(close_refs=copy.deepcopy(proto["close_strategies"]), comparison_mode=None,
                                         platform=proto["fee_platform"], strategy_type=proto["strategy_type"],
                                         phase="preflight").to_dict()
    ok = report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]
    return {"ok": ok, "report": report}


def parity_runs(proto: dict, selected: dict) -> dict:
    import pandas as pd
    from data_fetcher import load_cached_data
    from parity_diff import compute_parity_frame, config_from_live_config, summarize
    out = {"runs": []}
    for tf in proto["groups"]:
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        with open(path) as fh:
            sid = json.load(fh)["strategies"][0]["id"]
        start, end = proto["parity"]["spans"][tf]
        df = load_cached_data("BTC/USDT", tf, start_date=start, end_date=end)
        df = df[df.index < pd.Timestamp(end)]
        param_sets = {"seed": None, "selected": selected[tf]}
        for label, override in param_sets.items():
            for batched in proto["parity"]["batched"]:
                for window in proto["parity"]["windows"]:
                    cfg = config_from_live_config(path, sid)
                    cfg.batched = batched
                    cfg.funding_mode = proto["funding_mode"]
                    if override is not None:
                        cfg.params = dict(override)
                    t0 = time.time()
                    frame = compute_parity_frame(df, cfg=cfg, window=window or None)
                    summ = summarize(frame)
                    row = {
                        "config": os.path.basename(path), "strategy_id": sid, "params_label": label,
                        "params": cfg.params, "close_refs": cfg.close_refs, "direction": cfg.direction,
                        "batched": batched, "window": window, "span": [start, end], "candles": int(len(df)),
                        "bars_compared": summ["bars_compared"], "mismatches": summ["mismatches"],
                        "batch_mismatches": summ.get("batch_mismatches"),
                        "decision_agreement": summ["decision_agreement"], "close_parity": summ["close_parity"],
                        "clean": summ["clean"],
                        "live_long_entries": int((frame["live_open_action"] == "long").sum()),
                        "live_short_entries": int((frame["live_open_action"] == "short").sum()),
                        "long_position_closes": int(((frame["live_close_fraction"] > 0)
                                                     & (frame["live_signal"] == -1)).sum()),
                        "short_position_closes": int(((frame["live_close_fraction"] > 0)
                                                      & (frame["live_signal"] == 1)).sum()),
                        "seconds": round(time.time() - t0, 1),
                    }
                    out["runs"].append(row)
                    print(json.dumps({k: row[k] for k in ("config", "params_label", "batched", "window",
                                                          "bars_compared", "mismatches", "batch_mismatches",
                                                          "clean")}), flush=True)
    out["clean"] = bool(out["runs"]) and all(
        r["clean"] and r["bars_compared"] > 0 and not r["batch_mismatches"] for r in out["runs"])
    return out


def strip_leg_detail(rows: list) -> None:
    for row in rows:
        for leg in row["legs"].values():
            if leg is not None:
                leg.pop("intervals", None)
                leg.pop("position_net_pnl", None)


def run(cache_db: str) -> None:
    proto = load_protocol()
    _, source = verify_frozen(proto)
    build_cache(cache_db, proto, source)
    forbid_network()
    runner = Runner(proto)
    started = time.time()
    cand = proto["candidate"]
    out = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "manifest_sha256": sha256_file(MANIFEST_PATH),
           "source_commit": git_head(), "source_sha256": source_hashes(), "cost_model": proto["cost_model"],
           "warmup_bars": proto["warmup_bars"], "groups": {}}
    cap = close_capability(proto)
    out["close_capability"] = cap
    selected_both = {}
    for group in proto["groups"]:
        g = {"training": {}, "selections": {}}
        for arm, spec in proto["arms"].items():
            grid = proto["grids"][spec["name"]]
            rows = []
            for p in grid_combos(grid):
                row = runner.evaluate(arm, p, group, proto["training_window"])
                if spec["name"] == cand and p == proto["seed_params"]:
                    row["is_seed"] = True
                rows.append(row)
            fallback = proto["seed_params"] if spec["name"] == cand else {
                k: runner.reg.STRATEGY_REGISTRY[spec["name"]]["default_params"][k] for k in grid}
            g["selections"][arm] = select(rows, proto["selection_rule"], list(grid), fallback)
            strip_leg_detail(rows)
            g["training"][arm] = rows
            print(f"[{group}] trained {arm}: {g['selections'][arm]}", flush=True)
        for arm in proto["candidate_arms"]:
            spec = proto["arms"][arm]
            write_json(os.path.join(HERE, f"selected_{group}_{arm}.json"), {
                "name": cand, "type": proto["strategy_type"], "params": g["selections"][arm]["params"],
                "direction": spec["direction"], "close_strategies": proto["close_strategies"],
                "stop_loss_atr_mult": proto["stop_loss_atr_mult"]})
        selected_both[group] = g["selections"][proto["candidate_arms"][0]]["params"]
        held = {arm: {} for arm in proto["arms"]}
        stress = {arm: {} for arm in proto["candidate_arms"]}
        for window in proto["held_out_windows"]:
            for arm in proto["arms"]:
                params = g["selections"][arm]["params"]
                is_both = arm == proto["candidate_arms"][0]
                held[arm][window] = runner.evaluate(arm, params, group, window, evidence=is_both, reference=is_both)
                if arm in stress:
                    stress[arm][window] = runner.evaluate(
                        arm, params, group, window,
                        slippage_mult=proto["cost_model"]["stress"]["slippage_multiplier"])
            print(f"[{group}] held-out {window} done", flush=True)
        evidence_rows = []
        reference = {}
        for window, row in held[proto["candidate_arms"][0]].items():
            for ds, leg in row["legs"].items():
                if leg is None:
                    continue
                reference[f"{window} {ds}"] = leg.pop("continuous_reference", None)
                for e in leg.pop("entry_evidence", []):
                    evidence_rows.append([ds, window] + [e[k] for k in (
                        "bar_open", "signal", "reason", "input_first_bar", "input_rows", "input_sha256",
                        "exact_span_replay_matches")] + [repr(e[c]) for c in TRIX_EVIDENCE_COLUMNS])
        os.makedirs(EVIDENCE_DIR, exist_ok=True)
        ev_path = os.path.join(EVIDENCE_DIR, f"entries_{group}.csv.gz")
        write_gz_csv(ev_path, ["dataset", "window", "bar_open", "signal", "reason", "input_first_bar", "input_rows",
                               "input_sha256", "exact_span_replay_matches"] + TRIX_EVIDENCE_COLUMNS, evidence_rows)
        g["entry_evidence"] = {"path": os.path.relpath(ev_path, HERE), "rows": len(evidence_rows),
                               "exact_span_replay_mismatches": sum(1 for r in evidence_rows if r[8] is not True),
                               "sha256": sha256_file(ev_path)}
        g["continuous_reference"] = reference
        g["held_out"] = held
        g["held_out_x2_slippage"] = stress
        stability = {"train": [], "held_out": {}}
        grid = proto["grids"][cand]
        both_arm = proto["candidate_arms"][0]
        sel = g["selections"][both_arm]["params"]
        train_by_params = {json.dumps(r["params"], sort_keys=True): r for r in g["training"][both_arm]}
        for p in [sel] + neighbors(sel, grid):
            r = train_by_params[json.dumps(p, sort_keys=True)]
            stability["train"].append({"params": p, "mean_ddadj": r["mean_ddadj"], "mean_sharpe": r["mean_sharpe"],
                                       "clusters": r["clusters"]})
        for p in neighbors(sel, grid) + ([proto["seed_params"]] if proto["seed_params"] != sel else []):
            key = json.dumps(p, sort_keys=True)
            stability["held_out"][key] = {}
            for window in proto["held_out_windows"]:
                r = runner.evaluate(both_arm, p, group, window)
                stability["held_out"][key][window] = {
                    "mean_sharpe": r["mean_sharpe"], "mean_ddadj": r["mean_ddadj"],
                    "net_pnl_usd": round(sum(l["net_pnl_usd"] for l in r["legs"].values() if l), 4),
                    "clusters": r["clusters"],
                    "funding_incomplete": any(l is None or l["funding_incomplete"] for l in r["legs"].values())}
        g["stability"] = stability
        out["groups"][group] = g
    out["parity"] = parity_runs(proto, selected_both)
    for group, g in out["groups"].items():
        g["verdict"] = group_verdict(proto, g, out["parity"]["clean"], cap["ok"])
        for arm_rows in list(g["held_out"].values()) + list(g["held_out_x2_slippage"].values()):
            strip_leg_detail(list(arm_rows.values()))
        print(f"[{group}] verdict {g['verdict']['verdict']}: {g['verdict']['reason']}", flush=True)
    out["verdict"] = overall_verdict({k: v["verdict"]["verdict"] for k, v in out["groups"].items()})
    out["edge_source"] = EDGE_SOURCE[out["verdict"]]
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']}; wrote {RESULTS_PATH}")


def _f(v, prec=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float) and not math.isfinite(v):
        return str(v)
    if isinstance(v, (int, float)):
        return f"{v:.{prec}f}"
    return str(v)


def _p(params: dict) -> str:
    return ", ".join(f"{k}={v}" for k, v in params.items())


def _sum(row: dict, key: str) -> float:
    return sum(l[key] or 0.0 for l in row["legs"].values() if l is not None)


def render() -> None:
    proto = load_protocol()
    with open(RESULTS_PATH) as fh:
        res = json.load(fh)
    with open(MANIFEST_PATH) as fh:
        manifest = json.load(fh)
    verdict = res["verdict"]
    L = []
    add = L.append
    add("# trix_momentum study (issue 1659)")
    add("")
    add(f"**Verdict: {verdict.upper()}.** " + "; ".join(
        f"{g}: {gd['verdict']['verdict']} ({gd['verdict']['reason']})" for g, gd in res["groups"].items()) + ".")
    add("")
    add(f"The Python registry and the Go mirror keep `edge_status=\"no_edge\"` with "
        f"`edge_source=\"{EDGE_SOURCE[verdict]}\"` and this file as `edge_ref`. No study result removes the no-edge "
        "label: paper evaluation needs exactly one explicit `--mode=paper`, and live use still needs "
        "`allow_no_edge: true`, which is an operator decision and not evidence of profit. Nothing here edits deployed "
        "configuration, live defaults or acknowledgements; the study is suggest-only.")
    add("")
    add("## Candidate")
    add("")
    add("Entry-only triple-smoothed momentum crossover (`shared_strategies/open/trix_momentum.py`). For each endpoint t "
        "the evaluator takes the L + 1 closes t-L..t, seeds three chained EMAs (weight 2 / (period + 1)) with close "
        "t-L and updates them in order for each later close. The local oscillator is 100 x (third EMA j - third EMA "
        "j-1) / third EMA j-1. The signal line is an EMA (weight 2 / (signal_period + 1)) of the local oscillator, "
        "seeded with its first value. Endpoint t-1 is computed from its own run t-1-L..t-1, so a decision reads L + 2 "
        "bars. Long when oscillator - signal line > 0 at t, <= 0 at t-1, and the oscillator at t > 0; short is the "
        "exact mirror. Bad candles, timestamp defects, missing cadence, cadence gaps, an invalid denominator and "
        "non-finite derived values inside the window hold the entry with a reason; the close owner and stop keep "
        "running.")
    add("")
    add(f"Close owner: `{proto['close_strategies'][0]['name']}` "
        f"(`{json.dumps(proto['close_strategies'][0]['params'])}`); stop owner: "
        f"`stop_loss_atr_mult={proto['stop_loss_atr_mult']}`. Seed: {_p(proto['seed_params'])}. Parameter rules: "
        "integers, period >= 1, signal_period >= 1, signal_period < L, L >= 6 x period, L <= 197.")
    add("")
    add("## Protocol, provenance and invocations")
    add("")
    add(f"- Protocol `protocol.json` sha256 `{res['protocol_sha256']}`, frozen before scoring; `study_manifest.json` "
        f"(sha256 `{res['manifest_sha256']}`) records the same hash at freeze time "
        f"({manifest['frozen_at_utc']}, commit `{manifest['source_commit']}`).")
    add(f"- Run source commit `{res['source_commit']}`; runtime {res['runtime_seconds']} s. Source file sha256:")
    for path, digest in sorted(res["source_sha256"].items()):
        add(f"  - `{path}`: `{digest}`")
    add(f"- Inputs: {manifest['data_reuse']}. Source manifest `{manifest['source_manifest']['path']}` sha256 "
        f"`{manifest['source_manifest']['sha256']}`. Every file below is verified against `protocol.json` before "
        "each run, and network acquisition is disabled while scoring.")
    for name, meta in sorted(manifest["files"].items()):
        add(f"  - `{name}`: `{meta['sha256']}`")
    add("- Invocations, from the repository root:")
    add("  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py freeze`")
    add("  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py run --cache-db <new file>`")
    add("  - `uv run --no-sync python backtest/candidates/trix_momentum_1659/run_study.py render`")
    add(f"- Every leg calls `eval_windows.run_leg` with `registry=futures`, the arm's declared direction, the frozen "
        f"close and stop owner, `funding_mode={proto['funding_mode']}` (the `--funding charge` path), "
        f"`intrabar_resolution={proto['intrabar_resolution']}` and `warmup_bars={proto['warmup_bars']}`.")
    add("")
    add("Windows (finite, disjoint):")
    add("")
    add("| window | start | end | role |")
    add("|---|---|---|---|")
    for w, (s, e) in proto["windows"].items():
        role = "training" if w == proto["training_window"] else "held-out"
        add(f"| {w} | {s} | {e} | {role} |")
    add("")
    add(proto["window_notes"])
    add("")
    add("## Verdict gates")
    add("")
    for group, gd in res["groups"].items():
        v = gd["verdict"]
        add(f"### {group}: {v['verdict']}")
        add("")
        add("| gate | value |")
        add("|---|---|")
        for k, val in v["gates"].items():
            add(f"| {k} | {_f(val)} |")
        for k, val in v["conditions"].items():
            add(f"| {k} | {_f(val)} |")
        add("")
        c = v["checks"]
        add("| arm | clusters | positions | long/short | net PnL USD | net x2 slippage | mean Sharpe | mean ddadj | "
            "worst max DD % | fees USD | funding USD | turnover |")
        add("|---|---|---|---|---|---|---|---|---|---|---|---|")
        for arm in proto["arms"]:
            add(f"| {arm} | {c['clusters'][arm]} | {c['positions'][arm]} | {c['long_positions'][arm]}/"
                f"{c['short_positions'][arm]} | {_f(c['net_pnl_usd'][arm])} | "
                f"{_f(c['net_pnl_usd_x2_slippage'].get(arm))} | {_f(c['mean_sharpe'][arm], 3)} | "
                f"{_f(c['mean_ddadj'][arm], 3)} | {_f(c['max_dd_pct'][arm])} | {_f(c['fees_usd'][arm])} | "
                f"{_f(c['funding_pnl_usd'][arm])} | {_f(c['turnover'][arm])} |")
        add("")
        add(f"Unscorable windows: {json.dumps(c['unscorable_windows']) if c['unscorable_windows'] else 'none'}. "
            f"Largest candidate_both position share of positive position PnL: "
            f"{_f(c['top_position_share_candidate_both'], 4)} (descriptive). M1 incumbent-relative passes for "
            f"candidate_both: {c['m1_passes_candidate_both']} of {len(proto['held_out_windows'])} (descriptive).")
        add("")
    add("## Selections (training window `is`)")
    add("")
    add("| group | arm | how | params | eligible rows | clusters | mean ddadj | mean Sharpe | seed rank |")
    add("|---|---|---|---|---|---|---|---|---|")
    for group, gd in res["groups"].items():
        for arm, s in gd["selections"].items():
            add(f"| {group} | {arm} | {s['how']} | {_p(s['params'])} | {s['eligible']} | {_f(s.get('clusters'))} | "
                f"{_f(s.get('mean_ddadj'), 3)} | {_f(s.get('mean_sharpe'), 3)} | {_f(s.get('rank_of_seed'))} |")
    add("")
    add("## Held-out windows per arm")
    add("")
    add("| group | window | arm | net PnL USD | clusters | positions | long/short | mean Sharpe | mean ddadj | "
        "M1 verdict | funding complete |")
    add("|---|---|---|---|---|---|---|---|---|---|---|")
    for group, gd in res["groups"].items():
        for w in proto["held_out_windows"]:
            for arm in proto["arms"]:
                row = gd["held_out"][arm][w]
                legs = [l for l in row["legs"].values() if l is not None]
                add(f"| {group} | {w} | {arm} | {_f(_sum(row, 'net_pnl_usd'))} | {row['clusters']} | "
                    f"{sum(l['positions'] for l in legs)} | {sum(l['long_positions'] for l in legs)}/"
                    f"{sum(l['short_positions'] for l in legs)} | {_f(row['mean_sharpe'], 3)} | "
                    f"{_f(row['mean_ddadj'], 3)} | {row['m1_verdict']} | "
                    f"{_f(len(legs) == len(row['legs']) and not any(l['funding_incomplete'] for l in legs))} |")
    add("")
    add("## Funding coverage (candidate_both held-out legs)")
    add("")
    add("| group | window | dataset | expected hours | priced hours | unpriced held hours | funding USD |")
    add("|---|---|---|---|---|---|---|")
    for group, gd in res["groups"].items():
        for w in proto["held_out_windows"]:
            for ds, l in gd["held_out"][proto["candidate_arms"][0]][w]["legs"].items():
                if l is None:
                    add(f"| {group} | {w} | {ds} | missing | - | - | - |")
                    continue
                f = l["funding"]
                add(f"| {group} | {w} | {ds} | {f['expected_hours']} | {f['priced_hours']} | "
                    f"{f['unpriced_held_hours']} | {_f(l['funding_pnl_usd'])} |")
    add("")
    add("## Fills, exits and entry evidence")
    add("")
    for group, gd in res["groups"].items():
        held = gd["held_out"][proto["candidate_arms"][0]]
        checked = sum(l["next_bar_fills"]["positions_checked"] for r in held.values() for l in r["legs"].values() if l)
        bad = sum(l["next_bar_fills"]["mismatches"] for r in held.values() for l in r["legs"].values() if l)
        partial = sum(l["partial_close_positions"] for r in held.values() for l in r["legs"].values() if l)
        reasons = sorted({x for r in held.values() for l in r["legs"].values() if l for x in l["exit_reasons"]})
        ev = gd["entry_evidence"]
        add(f"- {group}: {checked} candidate_both held-out positions checked for a bar N signal filled at the bar N+1 "
            f"open, {bad} mismatches; {partial} positions closed in more than one part; exit reasons "
            f"{', '.join(reasons) or 'none'}. `{ev['path']}` holds {ev['rows']} entry rows (sha256 `{ev['sha256']}`); "
            f"{ev['exact_span_replay_mismatches']} rows failed the exact L + 2 row replay (the replay also proves L + 1 "
            "rows hold with insufficient_history).")
    add("")
    add("## Continuous-history reference (descriptive)")
    add("")
    add(proto["continuous_reference"]["label"] + ". The fixed-window values make every decision.")
    add("")
    add("| group | window and dataset | bars | max abs oscillator diff | median | max abs signal-line diff | "
        "decision differences | fixed / continuous entries |")
    add("|---|---|---|---|---|---|---|---|")
    for group, gd in res["groups"].items():
        for key, r in sorted(gd["continuous_reference"].items()):
            if r is None:
                continue
            add(f"| {group} | {key} | {r['bars']} | {_f(r['max_abs_oscillator_diff'], 8)} | "
                f"{_f(r['median_abs_oscillator_diff'], 8)} | {_f(r['max_abs_signal_line_diff'], 8)} | "
                f"{r['decision_differences']} | {r['fixed_window_entries']} / {r['continuous_entries']} |")
    add("")
    add("## Parameter stability and variants (candidate_both)")
    add("")
    for group, gd in res["groups"].items():
        add(f"Training neighbours of the {group} selection:")
        add("")
        add("| params | mean ddadj | mean Sharpe | clusters |")
        add("|---|---|---|---|")
        for r in gd["stability"]["train"]:
            add(f"| {_p(r['params'])} | {_f(r['mean_ddadj'], 3)} | {_f(r['mean_sharpe'], 3)} | {r['clusters']} |")
        add("")
        add("| held-out params | " + " | ".join(proto["held_out_windows"]) + " |")
        add("|---|" + "---|" * len(proto["held_out_windows"]))
        for key, by_w in gd["stability"]["held_out"].items():
            add(f"| {_p(json.loads(key))} | " + " | ".join(
                f"{_f(by_w[w]['net_pnl_usd'])} USD" for w in proto["held_out_windows"]) + " |")
        add("")
        rows = gd["training"][proto["candidate_arms"][0]]
        add(f"All {len(rows)} training variants of the {group} candidate_both arm (unsuccessful variants included):")
        add("")
        add("| params | clusters | mean ddadj | mean Sharpe | net PnL USD |")
        add("|---|---|---|---|---|")
        for r in rows:
            add(f"| {_p(r['params'])} | {r['clusters']} | {_f(r['mean_ddadj'], 3)} | {_f(r['mean_sharpe'], 3)} | "
                f"{_f(_sum(r, 'net_pnl_usd'))} |")
        add("")
    add("## Integration parity")
    add("")
    add(f"Close capability on Hyperliquid perps: {_f(res['close_capability']['ok'])}. `parity_diff` on the "
        f"explicit-paper configurations (`paper_parity_1h.json`, `paper_parity_4h.json`; frozen BTC candles): "
        f"clean = {_f(res['parity']['clean'])}. The generic paper-batch slot does not enable closed-bar decisions; "
        "the timestamped closed-bar check coverage is in `shared_scripts/test_trix_momentum_checks.py`.")
    add("")
    add("| config | params | batched | window | bars | mismatches | batch mismatches | long / short entries | "
        "long / short closes | clean |")
    add("|---|---|---|---|---|---|---|---|---|---|")
    for r in res["parity"]["runs"]:
        add(f"| {r['config']} | {r['params_label']} | {_f(r['batched'])} | {r['window'] or 'full'} | "
            f"{r['bars_compared']} | {r['mismatches']} | {_f(r['batch_mismatches'])} | {r['live_long_entries']} / "
            f"{r['live_short_entries']} | {r['long_position_closes']} / {r['short_position_closes']} | "
            f"{_f(r['clean'])} |")
    add("")
    add("## Limitations")
    add("")
    for item in proto["limitations"]:
        add(f"- {item}")
    add("- A pass would not authorize live use: live use still needs `allow_no_edge: true` and a separate reviewed "
        "edge decision.")
    add("")
    with open(REPORT_PATH, "w") as fh:
        fh.write("\n".join(L))
    print(f"wrote {REPORT_PATH}")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1659 trix_momentum study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("freeze", help="verify the pinned frozen inputs and write study_manifest.json (no network)")
    r = sub.add_parser("run", help="rebuild a fresh cache from the frozen files and run the protocol offline")
    r.add_argument("--cache-db", required=True)
    sub.add_parser("render", help="re-render REPORT.md from results.json")
    args = p.parse_args(argv)
    if args.cmd == "freeze":
        freeze()
    elif args.cmd == "run":
        run(args.cache_db)
        render()
    elif args.cmd == "render":
        render()
    return 0


if __name__ == "__main__":
    sys.exit(main())
