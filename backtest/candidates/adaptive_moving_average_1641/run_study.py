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
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
PARITY_PATH = os.path.join(HERE, "parity_results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
EVIDENCE_DIR = os.path.join(HERE, "evidence")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
MAX_TOP_POSITION_SHARE = 0.25
AMA_EVIDENCE_COLUMNS = ["ama_close", "ama_prev_close", "ama_seed_close", "ama_efficiency", "ama_smoothing",
                        "ama_value", "ama_prev_value", "ama_slope_ref", "ama_slope", "ama_required_history"]


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
        if getattr(base, "_ama_capture", False):
            base = base.__mro__[1]
        sink = self

        class CapturingBacktester(base):
            _ama_capture = True

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
        "entries": [p["entry_date"] for p in positions],
        "position_net_pnl": [round(p["net_pnl"], 6) for p in positions],
        "exit_reasons": sorted({r.split(":")[0] for p in positions for r in p["exit_reasons"] if r}),
        "next_bar_fills": next_bar_fill_check(run),
        "funding": {
            "mode": block.get("mode"), "source": block.get("source"), "complete": block.get("complete"),
            "sha256": block.get("sha256"), "row_count": block.get("row_count"),
            "expected_hours": cov.get("expected_hours"), "priced_hours": cov.get("priced_hours"),
            "missing_hours": cov.get("missing_hours"), "venue_absent_hours": cov.get("venue_absent_hours"),
            "unpriced_held_hours": block.get("unpriced_held_hours"),
            "fetch_error": block.get("fetch_error"), "error": block.get("error"),
        },
    }


def entry_evidence(run: dict, reg, name: str, params: dict) -> list:
    sig = run["signals"]
    hist = run["indicator_frame"] if run["indicator_frame"] is not None else sig
    out = []
    for ts in sig.index[sig["signal"].to_numpy() != 0]:
        pos = hist.index.get_loc(ts)
        need = int(sig.loc[ts, "ama_required_history"])
        window = hist.iloc[pos - need + 1:pos + 1][INPUT_COLUMNS]
        body = window.to_csv(float_format="%r").encode()
        replay = reg.apply_strategy(name, window.copy(), params).iloc[-1]
        row = {"bar_open": str(ts), "signal": int(sig.loc[ts, "signal"]), "reason": sig.loc[ts, "ama_reason"],
               "input_first_bar": str(window.index[0]), "input_rows": len(window), "input_sha256": sha256_bytes(body),
               "exact_span_replay_matches": bool(int(replay["signal"]) == int(sig.loc[ts, "signal"])
                                                 and float(replay["ama_value"]) == float(sig.loc[ts, "ama_value"])
                                                 and float(replay["ama_slope"]) == float(sig.loc[ts, "ama_slope"]))}
        for col in AMA_EVIDENCE_COLUMNS:
            row[col] = float(sig.loc[ts, col])
        out.append(row)
    return out


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

    def params_for(self, name: str, params: dict) -> dict:
        return {**self.proto.get("fixed_params", {}).get(name, {}), **params}

    def leg(self, name: str, params: dict, symbol: str, group: str, window: str, slippage_mult: float = 1.0,
            evidence: bool = False):
        kwargs = {}
        if slippage_mult != 1.0:
            kwargs["slippage_pct"] = self.base_slippage * slippage_mult
        self.capture.runs.clear()
        leg = self.ew.run_leg(
            self.reg, name, self.params_for(name, params), symbol, group, self.window(window), capital=self.capital,
            close_strategies=copy.deepcopy(self.proto["close_strategies"]),
            direction=self.proto["directions"][name],
            stop_loss_atr_mult=self.proto["stop_loss_atr_mult"],
            intrabar_resolution=self.proto["intrabar_resolution"],
            funding_mode=self.proto["funding_mode"],
            warmup_bars=self.proto["warmup_bars"], **kwargs)
        run = self.capture.take()
        summary = leg_summary(leg, run, self.capital)
        if evidence and summary is not None:
            summary["entry_evidence"] = entry_evidence(run, self.reg, name, self.params_for(name, params))
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

    def evaluate(self, name: str, params: dict, group: str, window: str, slippage_mult: float = 1.0,
                 evidence: bool = False) -> dict:
        legs = {}
        for symbol in self.proto["symbols"]:
            legs[self.ew.dataset_key(symbol, group)] = self.leg(name, params, symbol, group, window,
                                                                slippage_mult, evidence)
        bars = self.incumbent_bars(group, window)
        raw = {ds: ({k: l[k] for k in ("sharpe", "return_pct", "max_dd_pct", "ddadj", "trades", "bh_return_pct",
                                       "liquidated", "funding_incomplete")} if l else None)
               for ds, l in legs.items()}
        score = self.ew.score_candidate(raw, bars)
        return {"name": name, "params": dict(params), "group": group, "window": window,
                "window_range": list(self.window(window)), "slippage_multiplier": slippage_mult,
                "verdict": score["verdict"], "mean_sharpe": score.get("mean_sharpe"),
                "mean_ddadj": score.get("mean_ddadj"), "mean_bar_sharpe": score.get("mean_bar_sharpe"),
                "mean_bar_ddadj": score.get("mean_bar_ddadj"), "beats_sharpe_count": score.get("beats_sharpe_count"),
                "beats_ddadj_count": score.get("beats_ddadj_count"), "degenerate": score.get("degenerate"),
                "bars": bars, "legs": legs}


def select(rows: list, rule: dict, grid_keys: list, fallback: dict) -> dict:
    eligible = []
    for r in rows:
        legs = list(r["legs"].values())
        if any(l is None or l["funding_incomplete"] for l in legs):
            continue
        if any(l["positions"] < rule["min_training_positions"] for l in legs):
            continue
        eligible.append(r)
    if not eligible:
        return {"params": dict(fallback), "how": "fallback", "eligible": 0}
    key = lambda r: (-r["mean_ddadj"], -r["mean_sharpe"]) + tuple(r["params"][k] for k in grid_keys)
    best = sorted(eligible, key=key)[0]
    ranked = sorted(eligible, key=key)
    return {"params": {k: best["params"][k] for k in grid_keys}, "how": "selected", "eligible": len(eligible),
            "mean_ddadj": best["mean_ddadj"], "mean_sharpe": best["mean_sharpe"],
            "rank_of_seed": next((i + 1 for i, r in enumerate(ranked) if r.get("is_seed")), None)}


def neighbors(params: dict, grid: dict) -> list:
    out = []
    for k, values in grid.items():
        i = values.index(params[k])
        for j in (i - 1, i + 1):
            if 0 <= j < len(values):
                out.append(dict(params, **{k: values[j]}))
    return out


def entry_clusters(rows: list) -> int:
    return len({e for r in rows for l in r["legs"].values() if l for e in l["entries"]})


def window_unscorable(window: str, cand: dict, stress: dict, comps: dict) -> list:
    bad = []
    for label, row in [("candidate", cand), ("candidate x2 slippage", stress)] + \
            [(c, rows[window]) for c, rows in comps.items()]:
        for ds, l in row["legs"].items():
            if l is None or l["funding_incomplete"]:
                bad.append(f"{label} {ds}")
    for ds, bar in (cand["bars"] or {}).items():
        if bar is None or bar.get("funding_incomplete"):
            bad.append(f"incumbent median {ds}")
    return bad


def group_verdict(proto: dict, held: dict, stress: dict, comps: dict) -> dict:
    rules = proto["verdict_rules"]
    windows = proto["held_out_windows"]
    unscorable = {w: window_unscorable(w, held[w], stress[w], comps) for w in windows}
    scorable = [w for w in windows if not unscorable[w]]
    any_unscorable = len(scorable) < len(windows)
    cand_legs = [l for w in scorable for l in held[w]["legs"].values()]
    stress_legs = [l for w in scorable for l in stress[w]["legs"].values()]
    clusters = entry_clusters([held[w] for w in scorable])
    net = sum(l["net_pnl_usd"] for l in cand_legs)
    net_stress = sum(l["net_pnl_usd"] for l in stress_legs)
    pos_pnl = [x for l in cand_legs for x in l["position_net_pnl"]]
    positive = sum(x for x in pos_pnl if x > 0)
    top_share = (max(pos_pnl) / positive) if pos_pnl and positive > 0 else None
    mean_ret = statistics.mean(l["return_pct"] for l in cand_legs) if cand_legs else None
    comp_ret = {c: statistics.mean(l["return_pct"] for w in scorable for l in rows[w]["legs"].values())
                for c, rows in comps.items()} if scorable else {}
    m1_pass = sum(1 for w in scorable if held[w]["verdict"] == "pass")
    m1_unknown = len(windows) - len(scorable)
    need = rules["pass_conditions"]
    min_pass = 2

    def tri(value, known):
        return bool(value) if known else None

    conditions = {
        "net_positive_base": tri(net > 0, not any_unscorable),
        "net_positive_stress": tri(net_stress > 0, not any_unscorable),
        "m1_passes": True if m1_pass >= min_pass else (False if m1_pass + m1_unknown < min_pass else None),
        "beats_comparators": tri(mean_ret is not None and all(mean_ret > v for v in comp_ret.values()),
                                 not any_unscorable),
        "not_concentrated": tri(top_share is not None and top_share <= MAX_TOP_POSITION_SHARE,
                                not any_unscorable),
    }
    assert set(conditions) == set(need)
    checks = {
        "scorable_windows": scorable,
        "unscorable_windows": {w: v for w, v in unscorable.items() if v},
        "conditions": conditions,
        "entry_clusters": clusters,
        "positions": sum(l["positions"] for l in cand_legs),
        "long_positions": sum(l["long_positions"] for l in cand_legs),
        "short_positions": sum(l["short_positions"] for l in cand_legs),
        "m1_passes": m1_pass,
        "net_pnl_usd": round(net, 4),
        "net_pnl_usd_x2_slippage": round(net_stress, 4),
        "fees_usd": round(sum(l["fees_usd"] for l in cand_legs), 4),
        "funding_pnl_usd": round(sum(l["funding_pnl_usd"] for l in cand_legs), 4),
        "mean_return_pct": round(mean_ret, 4) if mean_ret is not None else None,
        "comparator_mean_return_pct": {c: round(v, 4) for c, v in comp_ret.items()},
        "top_position_share": round(top_share, 4) if top_share is not None else None,
        "turnover": round(sum(l["turnover"] or 0.0 for l in cand_legs), 4),
    }
    false = sorted(k for k, v in conditions.items() if v is False)
    unknown = sorted(k for k, v in conditions.items() if v is None)
    if clusters < rules["min_held_out_entry_clusters"]:
        verdict, why = "inconclusive", f"{clusters} entry clusters < {rules['min_held_out_entry_clusters']}"
    elif false:
        verdict, why = "fail", "false: " + ", ".join(false)
    elif not unknown:
        verdict, why = "pass", "every condition true"
    else:
        verdict, why = "inconclusive", "unknown: " + ", ".join(unknown)
    return {"verdict": verdict, "reason": why, "checks": checks}


def overall_verdict(groups: dict) -> str:
    vs = set(groups.values())
    if vs == {"pass"}:
        return "pass"
    if vs == {"fail"}:
        return "fail"
    return "inconclusive"


def run(cache_db: str) -> None:
    proto = load_protocol()
    _, source = verify_frozen(proto)
    build_cache(cache_db, proto, source)
    forbid_network()
    runner = Runner(proto)
    started = time.time()
    cand = proto["candidate"]
    out = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "manifest_sha256": sha256_file(MANIFEST_PATH),
           "source_commit": git_head(), "cost_model": proto["cost_model"], "warmup_bars": proto["warmup_bars"],
           "groups": {}}
    for group in proto["groups"]:
        g = {"training": {}, "selections": {}}
        for name in [cand] + proto["comparators"]:
            grid = proto["grids"][name]
            rows = []
            for p in grid_combos(grid):
                row = runner.evaluate(name, p, group, proto["training_window"])
                if name == cand and p == proto["seed_params"]:
                    row["is_seed"] = True
                rows.append(row)
            fallback = proto["seed_params"] if name == cand else {
                k: runner.reg.STRATEGY_REGISTRY[name]["default_params"][k] for k in grid}
            g["selections"][name] = select(rows, proto["selection_rule"], list(grid), fallback)
            for row in rows:
                for leg in row["legs"].values():
                    if leg is not None:
                        leg.pop("entries")
                        leg.pop("position_net_pnl")
            g["training"][name] = rows
            print(f"[{group}] trained {name}: {g['selections'][name]}", flush=True)
        selected = g["selections"][cand]["params"]
        write_json(os.path.join(HERE, f"selected_{group}.json"), {
            "name": cand, "type": proto["strategy_type"], "params": selected,
            "direction": proto["directions"][cand], "close_strategies": proto["close_strategies"],
            "stop_loss_atr_mult": proto["stop_loss_atr_mult"]})
        held, stress = {}, {}
        comps = {c: {} for c in proto["comparators"]}
        for window in proto["held_out_windows"]:
            held[window] = runner.evaluate(cand, selected, group, window, evidence=True)
            stress[window] = runner.evaluate(cand, selected, group, window,
                                             slippage_mult=proto["cost_model"]["stress"]["slippage_multiplier"])
            for c in proto["comparators"]:
                comps[c][window] = runner.evaluate(c, g["selections"][c]["params"], group, window)
            print(f"[{group}] held-out {window}: M1 {held[window]['verdict']}", flush=True)
        evidence_rows = []
        for window, row in held.items():
            for ds, leg in row["legs"].items():
                for e in (leg or {}).pop("entry_evidence", []):
                    evidence_rows.append([ds, window] + [e[k] for k in (
                        "bar_open", "signal", "reason", "input_first_bar", "input_rows", "input_sha256",
                        "exact_span_replay_matches")] + [repr(e[c]) for c in AMA_EVIDENCE_COLUMNS])
        os.makedirs(EVIDENCE_DIR, exist_ok=True)
        ev_path = os.path.join(EVIDENCE_DIR, f"entries_{group}.csv.gz")
        write_gz_csv(ev_path, ["dataset", "window", "bar_open", "signal", "reason", "input_first_bar", "input_rows",
                               "input_sha256", "exact_span_replay_matches"] + AMA_EVIDENCE_COLUMNS, evidence_rows)
        g["entry_evidence"] = {"path": os.path.relpath(ev_path, HERE), "rows": len(evidence_rows),
                               "exact_span_replay_mismatches": sum(1 for r in evidence_rows if r[8] is not True),
                               "sha256": sha256_file(ev_path)}
        g["held_out"] = held
        g["held_out_x2_slippage"] = stress
        g["comparators"] = comps
        g["verdict"] = group_verdict(proto, held, stress, comps)
        stability = {"train": [], "held_out": {}}
        grid = proto["grids"][cand]
        train_by_params = {json.dumps(r["params"], sort_keys=True): r for r in g["training"][cand]}
        for p in [selected] + neighbors(selected, grid):
            r = train_by_params[json.dumps(p, sort_keys=True)]
            stability["train"].append({"params": p, "mean_ddadj": r["mean_ddadj"], "mean_sharpe": r["mean_sharpe"],
                                       "positions": sum(l["positions"] for l in r["legs"].values() if l)})
        for p in neighbors(selected, grid) + ([proto["seed_params"]] if proto["seed_params"] != selected else []):
            key = json.dumps(p, sort_keys=True)
            stability["held_out"][key] = {}
            for window in proto["held_out_windows"]:
                r = runner.evaluate(cand, p, group, window)
                stability["held_out"][key][window] = {
                    "verdict": r["verdict"], "mean_sharpe": r["mean_sharpe"], "mean_ddadj": r["mean_ddadj"],
                    "net_pnl_usd": round(sum(l["net_pnl_usd"] for l in r["legs"].values() if l), 4),
                    "positions": sum(l["positions"] for l in r["legs"].values() if l),
                    "funding_incomplete": any(l is None or l["funding_incomplete"] for l in r["legs"].values())}
        g["stability"] = stability
        out["groups"][group] = g
        print(f"[{group}] verdict {g['verdict']['verdict']}: {g['verdict']['reason']}", flush=True)
    out["verdict"] = overall_verdict({k: v["verdict"]["verdict"] for k, v in out["groups"].items()})
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']}; wrote {RESULTS_PATH}")


def parity(cache_db: str) -> None:
    proto = load_protocol()
    _, source = verify_frozen(proto)
    build_cache(cache_db, proto, source)
    forbid_network()
    import pandas as pd
    from data_fetcher import load_cached_data
    from parity_diff import compute_parity_frame, config_from_live_config, summarize
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "source_commit": git_head(), "runs": []}
    for tf in proto["groups"]:
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        with open(path) as fh:
            sid = json.load(fh)["strategies"][0]["id"]
        start, end = proto["parity"]["spans"][tf]
        df = load_cached_data("BTC/USDT", tf, start_date=start, end_date=end)
        df = df[df.index < pd.Timestamp(end)]
        param_sets = {"seed": None}
        sel = os.path.join(HERE, f"selected_{tf}.json")
        if os.path.exists(sel):
            with open(sel) as fh:
                param_sets["selected"] = json.load(fh)["params"]
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
    out["clean"] = all(r["clean"] and r["bars_compared"] > 0 and not r["batch_mismatches"] for r in out["runs"])
    write_json(PARITY_PATH, out)
    print(f"parity clean: {out['clean']}; wrote {PARITY_PATH}")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1641 adaptive moving average study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("freeze", help="verify the pinned frozen inputs and write study_manifest.json (no network)")
    r = sub.add_parser("run", help="rebuild a fresh cache from the frozen files and run the protocol offline")
    r.add_argument("--cache-db", required=True)
    q = sub.add_parser("parity", help="decision parity of the paper configurations on frozen BTC candles")
    q.add_argument("--cache-db", required=True)
    sub.add_parser("render", help="re-render REPORT.md from results.json and parity_results.json")
    args = p.parse_args(argv)
    if args.cmd == "freeze":
        freeze()
    elif args.cmd == "run":
        run(args.cache_db)
    elif args.cmd == "parity":
        parity(args.cache_db)
    if args.cmd in ("run", "parity", "render") and os.path.exists(RESULTS_PATH):
        from render_report import render
        render(load_protocol(), RESULTS_PATH, MANIFEST_PATH, PARITY_PATH, REPORT_PATH)
    return 0


if __name__ == "__main__":
    sys.exit(main())
