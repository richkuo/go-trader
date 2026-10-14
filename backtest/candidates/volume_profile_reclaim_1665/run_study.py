#!/usr/bin/env python3

import argparse
import copy
import gzip
import hashlib
import io
import itertools
import json
import os
import subprocess
import sys
from decimal import ROUND_HALF_EVEN, Decimal

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
SPEC_PATH = os.path.join(HERE, "candidate_spec.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
RECORDING_DIR = os.path.join(REPO, "backtest", "candidates", "cumulative_volume_delta_divergence_1638", "data", "recording")
EXACT_MANIFEST = os.path.join(REPO, "backtest", "candidates", "volume_profile_reclaim_1665_exact_manifest.json")
PROXY_MANIFEST = os.path.join(REPO, "backtest", "candidates", "volume_profile_reclaim_1665_proxy_manifest.json")
PROFILE_DIR = os.path.join(HERE, "data", "profile")
PROXY_DIR = os.path.join(HERE, "data", "proxy")
COINS = ("BTC", "ETH")
ALLOCATION_USD = 1000.0
POOL_USD = ALLOCATION_USD * len(COINS)
SOURCE_FILES = (
    "shared_strategies/open/volume_profile_reclaim.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_atr.py",
    "shared_tools/volume_profile_inputs.py",
    "backtest/volume_profile_replay.py",
    "backtest/trade_flow_replay.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "scheduler/market_feed_profile.go",
    "scheduler/market_feed_trades.go",
    "backtest/candidates/volume_profile_reclaim_1665/candidate_spec.json",
    "backtest/candidates/volume_profile_reclaim_1665/run_study.py",
)
STUDY_ORDER = ("primary", "proxy")
ARM_ORDER = ("candidate", "session_bounds_ablation", "anchored_vwap_reversion", "mean_reversion_pro")
COST_ORDER = ("base", "stress")


def load_spec() -> dict:
    with open(SPEC_PATH) as fh:
        return json.load(fh)


SPEC = load_spec()


def paths_on_sys():
    for p in (BACKTEST, os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open")):
        if p not in sys.path:
            sys.path.insert(0, p)


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def _rel(path: str) -> str:
    return os.path.relpath(path, os.path.join(REPO, "backtest", "candidates"))


def _proxy_price(tp: float) -> str:
    d = Decimal(repr(tp))
    exp = d.adjusted() - 4
    q = d.quantize(Decimal(1).scaleb(exp), rounding=ROUND_HALF_EVEN)
    text = format(q.normalize(), "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text


def _units(volume: float) -> int:
    return int((Decimal(repr(volume)) * Decimal(10 ** 8)).to_integral_value(rounding=ROUND_HALF_EVEN))


def build_proxy_series(coin: str, candles) -> dict:
    paths_on_sys()
    import volume_profile_inputs as vpi
    from volume_profile_replay import PAYLOAD_KEYS, SERIES_SCHEMA, sessions_sha256
    step = 900_000
    per_session = vpi.SESSION_MS // step
    by_session = {}
    for row in candles.itertuples():
        open_ms = int(row.timestamp)
        by_session.setdefault(vpi.session_start(open_ms), []).append(row)
    sessions = []
    for start in sorted(by_session):
        rows = by_session[start]
        if len(rows) != per_session:
            sessions.append({"session_start_ms": start, "status": "unavailable", "reason_code": "proxy_incomplete_candles",
                             "reason": f"{len(rows)} of {per_session} candles", "unavailable_ms": start + vpi.SESSION_MS,
                             "invalid_ms": None, "complete_minutes": 0, "artifact": None})
            continue
        prices = {}
        used = 0
        for r in rows:
            units = _units(float(r.volume))
            if units <= 0:
                continue
            px = _proxy_price((float(r.high) + float(r.low) + float(r.close)) / 3.0)
            prices[px] = prices.get(px, 0) + units
            used += 1
        if len(prices) < 2:
            sessions.append({"session_start_ms": start, "status": "unavailable", "reason_code": "zero_price_range",
                             "reason": "proxy prices collapse to one price", "unavailable_ms": start + vpi.SESSION_MS,
                             "invalid_ms": None, "complete_minutes": 0, "artifact": None})
            continue
        ordered = sorted(prices.items(), key=lambda kv: Decimal(kv[0]))
        rows_text = "\n".join(f"{int(r.timestamp)},{r.open!r},{r.high!r},{r.low!r},{r.close!r},{r.volume!r}" for r in rows)
        art = {"session_start_ms": start, "session_end_ms": start + vpi.SESSION_MS, "trades": used,
               "volume": vpi.format_units(sum(u for _, u in ordered)),
               "trades_hash": hashlib.sha256(rows_text.encode("utf-8")).hexdigest(),
               "first_event_ms": int(rows[0].timestamp), "last_event_ms": int(rows[-1].timestamp) + step - 1,
               "complete_minutes": vpi.MINUTES, "available_ms": start + vpi.SESSION_MS + vpi.CONTRACT["reorder_grace_ms"],
               "prices": [[px, vpi.format_units(u)] for px, u in ordered]}
        art["sha256"] = vpi.artifact_sha256(coin, art)
        vpi.validate_artifact(coin, art, start)
        sessions.append({"session_start_ms": start, "status": "final", "reason_code": "", "reason": "", "unavailable_ms": None,
                         "invalid_ms": None, "complete_minutes": vpi.MINUTES, "artifact": art})
    contract = {k: vpi.CONTRACT[k] for k in PAYLOAD_KEYS}
    contract["source"] = vpi.PROXY_SOURCE
    return {"series_schema": SERIES_SCHEMA, "kind": vpi.KIND, "coin": coin, **contract, "sessions": sessions,
            "sessions_sha256": sessions_sha256(sessions),
            "complete_sessions": sum(1 for s in sessions if s["status"] == "final"),
            "economic_evidence": True, "refusals": [],
            "proxy": {"label": SPEC["data_plan"]["proxy_sensitivity"]["label"],
                      "method": SPEC["data_plan"]["proxy_sensitivity"]["method"]},
            "stats": {}, "provenance": {"candles": "hyperliquid candleSnapshot 15m via the proxy manifest"}}


def prepare() -> dict:
    paths_on_sys()
    import offline_manifest as om
    from volume_profile_replay import coverage_report, load_profile_recording, write_profile_series
    os.makedirs(PROFILE_DIR, exist_ok=True)
    os.makedirs(PROXY_DIR, exist_ok=True)
    loaded = load_profile_recording(RECORDING_DIR)
    report = {"recording": {k: loaded["report"][k] for k in ("run_id", "recorder_version", "frames", "malformed_frames",
                                                              "dropped", "events_compared", "economic_evidence",
                                                              "refusals", "started_ms", "closed_ms")},
              "exact": {}, "proxy": {}}
    exact_refs, proxy_refs = {}, {}
    for coin in COINS:
        series = loaded["series"][coin]
        path = os.path.join(PROFILE_DIR, f"{coin}_volume_profile.json.gz")
        sha = write_profile_series(series, path)
        exact_refs[coin] = {"path": _rel(path), "sha256": sha}
        report["exact"][coin] = coverage_report(series)
    proxy = om.load_manifest(PROXY_MANIFEST, verify=True)
    for ds in proxy["datasets"]:
        series = build_proxy_series(ds["coin"], om.load_candles(ds))
        path = os.path.join(PROXY_DIR, f"{ds['coin']}_proxy_profile.json.gz")
        sha = write_profile_series(series, path)
        proxy_refs[ds["coin"]] = {"path": _rel(path), "sha256": sha}
        report["proxy"][ds["coin"]] = coverage_report(series)
    with open(PROXY_MANIFEST) as fh:
        raw = json.load(fh)
    for ds in raw["datasets"]:
        ds["volume_profile"] = proxy_refs[ds["coin"]]
    with open(PROXY_MANIFEST, "w") as fh:
        json.dump(raw, fh, indent=2)
        fh.write("\n")
    exact = copy.deepcopy(raw)
    exact["provenance"]["label"] = ("Hyperliquid mainnet perpetual 15m candles from the public info API (candleSnapshot, closed bars "
                                    "only); the attached volume_profile series are exact executed-trade profiles replayed from the "
                                    "committed public trades WebSocket recording of issue 1638")
    exact["windows"] = {name: dict(spec) for name, spec in SPEC["data_plan"]["primary"]["windows"].items()}
    for ds in exact["datasets"]:
        ds["volume_profile"] = exact_refs[ds["coin"]]
    with open(EXACT_MANIFEST, "w") as fh:
        json.dump(exact, fh, indent=2)
        fh.write("\n")
    om.load_manifest(EXACT_MANIFEST, verify=True)
    om.load_manifest(PROXY_MANIFEST, verify=True)
    with open(os.path.join(HERE, "data", "import_report.json"), "w") as fh:
        json.dump(report, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return report


class Capture:
    def __init__(self):
        self.equity = []
        self.results = []

    def install(self):
        import backtester
        base = backtester.Backtester
        sink = self

        class CapturingBacktester(base):
            def _calculate_metrics(self, equity_df, *a, **k):
                sink.equity.append(equity_df["equity"].copy())
                return super()._calculate_metrics(equity_df, *a, **k)

            def run(self, *a, **k):
                out = super().run(*a, **k)
                sink.results.append(out)
                return out

        backtester.Backtester = CapturingBacktester

    def clear(self):
        self.equity.clear()
        self.results.clear()

    def take(self):
        curve = self.equity[-1] if self.equity else None
        results = self.results[-1] if self.results else None
        self.clear()
        return curve, results


def grid() -> list:
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[SPEC["name"]]
    if ranges != SPEC["parameters"]["optimizer_grid"]:
        raise SystemExit("the optimizer grid differs from the frozen specification")
    keys = list(ranges)
    return [dict(zip(keys, vals)) for vals in itertools.product(*(ranges[k] for k in keys))]


def arm_candidate(arm: str, base: dict) -> dict:
    risk = SPEC["exit_and_risk"]
    close = [copy.deepcopy(risk["close_strategy"])]
    if arm in ("candidate", "session_bounds_ablation"):
        name = SPEC["name"]
        params = dict(SPEC["parameters"]["seed"], **base)
        if arm == "session_bounds_ablation":
            params["session_bounds"] = True
    else:
        name, params = arm, None
    return {"name": name, "type": SPEC["strategy_type"], "params": params, "direction": risk["direction"],
            "close_strategies": close, "stop_loss_atr_mult": 1.0, "comparison_mode": risk["comparison_mode"]}


def max_drawdown_pct(values) -> float:
    peak, worst = None, 0.0
    for v in values:
        peak = v if peak is None or v > peak else peak
        if peak and peak > 0:
            worst = min(worst, (v - peak) / peak * 100.0)
    return round(worst, 6)


def decision_audit(manifest, dataset, window_name, params) -> dict:
    import offline_manifest as om
    from registry_loader import load_registry
    reg = load_registry(SPEC["registry"])
    frame, win, _ = om.window_frame(manifest, dataset, window_name)
    profile, cov = om.attach_volume_profile(manifest, dataset, win)
    out = reg.apply_strategy(SPEC["name"], frame, {**params, "volume_profile_observations": profile})
    rows = out[(out["timestamp"] >= win["start_ms"]) & (out["timestamp"] < win["end_ms"])]
    reasons = rows["vpr_reason"].value_counts().to_dict()
    usable = rows[rows["vpr_profile_sha256"] != ""]
    eligible_sessions = sorted({int(s) for s in usable["vpr_session_ms"].dropna()})
    entries = rows[rows["signal"] != 0]
    return {
        "scored_bars": int(len(rows)), "profile_usable_bars": int(len(usable)),
        "eligible_sessions": len(eligible_sessions), "eligible_session_starts": eligible_sessions,
        "reasons": {k: int(v) for k, v in sorted(reasons.items())},
        "coverage": cov,
        "entries": [{"deadline_ms": int(r.vpr_deadline_ms), "side": r.vpr_side, "profile_session_ms": int(r.vpr_profile_session_ms),
                     "profile_sha256": r.vpr_profile_sha256, "profile_source": r.vpr_profile_source, "val": r.vpr_val,
                     "vah": r.vpr_vah, "prev_close": float(r.vpr_prev_close), "close": float(r.vpr_close)}
                    for r in entries.itertuples()],
    }


class Runner:
    def __init__(self):
        import eval_windows as ew
        from registry_loader import load_registry
        self.ew = ew
        self.reg = load_registry(SPEC["registry"])
        self.capture = Capture()
        self.capture.install()

    def pool(self, manifest, arm: str, base: dict, window: str, cost: float) -> dict:
        import pandas as pd
        from eval_windows import execution_metrics
        cand = arm_candidate(arm, base)
        self.ew.validate_candidate(cand)
        legs, curves = {}, {}
        for ds in manifest["datasets"]:
            self.capture.clear()
            ctx = {"manifest": manifest, "window": window, "cost_multiplier": cost}
            leg = self.ew.run_candidate_leg(self.reg, cand, ds["coin"], manifest["interval"], (None, None),
                                            capital=ALLOCATION_USD, manifest_ctx=ctx)
            curve, results = self.capture.take()
            ex = execution_metrics(results, ALLOCATION_USD) if results is not None else {}
            pos = ex.get("position_list") or []
            block = (leg or {}).get("funding") or {}
            legs[ds["coin"]] = {
                "return_pct": leg["return_pct"] if leg else None, "max_dd_pct": leg["max_dd_pct"] if leg else None,
                "positions": ex.get("positions", 0), "long_positions": ex.get("long_positions", 0),
                "short_positions": ex.get("short_positions", 0), "net_pnl_usd": ex.get("net_pnl_usd", 0.0),
                "fees_usd": ex.get("fees_usd", 0.0), "funding_pnl_usd": ex.get("funding_pnl_usd", 0.0),
                "turnover": ex.get("turnover"), "funding_incomplete": bool((leg or {}).get("funding_incomplete")),
                "funding_complete": block.get("complete"), "liquidated": bool((leg or {}).get("liquidated")),
                "positions_detail": [{"entry": p["entry_date"], "exit": p["exit_date"], "side": p["side"],
                                      "net_pnl": round(p["net_pnl"], 6)} for p in pos],
            }
            curves[ds["coin"]] = curve
        aligned = None
        if all(c is not None for c in curves.values()):
            frame = pd.concat({k: v for k, v in curves.items()}, axis=1).sort_index().ffill()
            for coin in curves:
                frame[coin] = frame[coin].fillna(ALLOCATION_USD)
            aligned = frame.sum(axis=1)
        net = [l["net_pnl_usd"] for l in legs.values()]
        total = sum(net)
        gross = sum(abs(x) for x in net)
        pool_ret = round(total / POOL_USD * 100.0, 6)
        pool_dd = max_drawdown_pct(list(aligned.values)) if aligned is not None else None
        return {
            "arm": arm, "window": window, "cost_multiplier": cost, "params": cand["params"], "legs": legs,
            "pool": {"capital_usd": POOL_USD, "allocation_usd": ALLOCATION_USD, "net_pnl_usd": round(total, 6),
                     "return_pct": pool_ret, "max_dd_pct": pool_dd,
                     "ddadj": round(pool_ret / abs(pool_dd), 6) if pool_dd else 0.0,
                     "positions": sum(l["positions"] for l in legs.values()),
                     "long_positions": sum(l["long_positions"] for l in legs.values()),
                     "short_positions": sum(l["short_positions"] for l in legs.values()),
                     "fees_usd": round(sum(l["fees_usd"] for l in legs.values()), 6),
                     "funding_pnl_usd": round(sum(l["funding_pnl_usd"] for l in legs.values()), 6),
                     "turnover": round(sum(l["turnover"] or 0 for l in legs.values()), 6),
                     "largest_coin_share": round(max(abs(x) for x in net) / gross, 6) if gross else None,
                     "funding_complete": all(not l["funding_incomplete"] and l["funding_complete"] is not False for l in legs.values()),
                     "liquidated": any(l["liquidated"] for l in legs.values())},
        }


def select(rows: list) -> dict:
    eligible = [r for r in rows if r["pool"]["funding_complete"] and not r["pool"]["liquidated"] and r["pool"]["positions"] >= 10]
    if not eligible:
        return None
    return sorted(eligible, key=lambda r: (-r["pool"]["ddadj"], r["grid_index"]))[0]


def study(runner: Runner, manifest_path: str, label: str) -> dict:
    import offline_manifest as om
    manifest = om.load_manifest(manifest_path, verify=True)
    audits = {w: {ds["coin"]: decision_audit(manifest, ds, w, dict(SPEC["parameters"]["seed"])) for ds in manifest["datasets"]}
              for w in manifest["windows"]}
    eligible = {w: {c: a["eligible_sessions"] for c, a in by.items()} for w, by in audits.items()}
    train_rows = []
    if any(eligible["train"].values()):
        for i, base in enumerate(grid()):
            r = runner.pool(manifest, "candidate", base, "train", 1.0)
            r["grid_index"] = i
            r["base"] = base
            train_rows.append(r)
    chosen = select(train_rows)
    base = chosen["base"] if chosen else {k: SPEC["parameters"]["seed"][k] for k in SPEC["parameters"]["optimizer_grid"]}
    stability = (sum(1 for r in train_rows if r["pool"]["net_pnl_usd"] > 0) / len(train_rows)) if train_rows else None
    held = {}
    complete_bars = all(a["profile_usable_bars"] == a["scored_bars"] for a in audits["test"].values())
    for arm in ARM_ORDER:
        for cost_name, mult in (("base", 1.0), ("stress", 2.0)):
            key = f"{arm}@{cost_name}"
            if not any(eligible["test"].values()):
                held[key] = {"arm": arm, "cost_multiplier": mult, "scored": False,
                             "reason": "no decision bar in the test window had a usable prior-session profile, so no arm is scored on an eligible session"}
                continue
            if arm not in ("candidate", "session_bounds_ablation") and not complete_bars:
                held[key] = {"arm": arm, "cost_multiplier": mult, "scored": False,
                             "reason": "the baseline cannot be restricted to the candidate's eligible bars because some test bars lack a usable profile"}
                continue
            out = runner.pool(manifest, arm, base, "test", mult)
            out["scored"] = True
            held[key] = out
    seed_audit_test = audits["test"]
    chosen_audit = {ds["coin"]: decision_audit(manifest, ds, "test", dict(SPEC["parameters"]["seed"], **base))
                    for ds in manifest["datasets"]} if any(eligible["test"].values()) else seed_audit_test
    return {
        "label": label, "manifest": _rel(manifest_path), "manifest_sha256": sha256_file(manifest_path),
        "provenance": manifest["provenance"], "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                                                          for k, v in manifest["windows"].items()},
        "datasets": {ds["coin"]: {"candles_sha256": ds["candles"]["sha256"], "funding_sha256": ds["funding"]["sha256"],
                                  "volume_profile_sha256": ds["volume_profile"]["sha256"],
                                  "half_spread_bps": ds["half_spread_bps"]} for ds in manifest["datasets"]},
        "seed_audit": audits, "eligible_sessions": eligible,
        "selection": {"outcome": "selected" if chosen else ("no_eligible_combo" if train_rows else "no_train_eligible_session"),
                      "params": base, "grid_points": len(train_rows), "positive_train_share": stability,
                      "train": [{"base": r["base"], "pool": r["pool"]} for r in train_rows]},
        "held_out": held, "chosen_audit": chosen_audit,
    }


def verdict(primary: dict) -> dict:
    reasons = []
    rec = primary.get("recording") or {}
    if not rec.get("economic_evidence"):
        reasons.append("the primary trade recording is incomplete or damaged: " + "; ".join(rec.get("refusals") or []))
    sessions = sum(primary["eligible_sessions"]["test"].values())
    floor = 20
    if sessions < floor:
        reasons.append(f"the primary test window has {sessions} eligible complete prior sessions across coins, below {floor}: "
                       "the recording holds no complete UTC session")
    cand = primary["held_out"].get("candidate@base") or {}
    positions = cand.get("pool", {}).get("positions", 0) if cand.get("scored") else 0
    if positions < 30:
        reasons.append(f"the candidate made {positions} primary test positions, below 30")
    if reasons:
        return {"verdict": "inconclusive", "reasons": reasons}
    pool = cand["pool"]
    stress = primary["held_out"]["candidate@stress"]["pool"]
    checks = [("pool net PnL at cost x1 is not positive", pool["net_pnl_usd"] > 0),
              ("pool net PnL at cost x2 is not positive", stress["net_pnl_usd"] > 0),
              ("one coin carries more than 60% of the absolute PnL", (pool["largest_coin_share"] or 0) <= 0.6)]
    for arm in ARM_ORDER[1:]:
        other = primary["held_out"][f"{arm}@base"]
        checks.append((f"pool DD-adjusted return does not beat {arm}", other.get("scored") and pool["ddadj"] > other["pool"]["ddadj"]))
    missed = [n for n, ok in checks if not ok]
    return {"verdict": "fail" if missed else "pass", "reasons": missed}


def run() -> dict:
    paths_on_sys()
    runner = Runner()
    with open(os.path.join(HERE, "data", "import_report.json")) as fh:
        imported = json.load(fh)
    from volume_profile_replay import load_profile_recording
    replay = load_profile_recording(RECORDING_DIR)
    for coin in COINS:
        rebuilt = replay["series"][coin]["sessions_sha256"]
        if rebuilt != imported["exact"][coin]["sessions_sha256"]:
            raise SystemExit(f"{coin}: the committed profile series differs from a fresh replay of the recording")
    primary = study(runner, EXACT_MANIFEST, SPEC["data_plan"]["primary"]["label"])
    primary["recording"] = imported["recording"]
    primary["import"] = imported["exact"]
    proxy = study(runner, PROXY_MANIFEST, SPEC["data_plan"]["proxy_sensitivity"]["label"])
    proxy["import"] = imported["proxy"]
    return {
        "schema": "go_trader_study_results/v1", "issue": SPEC["issue"], "candidate": SPEC["name"],
        "source_commit": git_head(),
        "spec_sha256": sha256_file(SPEC_PATH),
        "source_sha256": {p: sha256_file(os.path.join(REPO, p)) for p in SOURCE_FILES},
        "studies": {"primary": primary, "proxy": proxy},
        "verdict": verdict(primary),
    }


def _fmt(v, nd=4):
    if v is None:
        return "n/a"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def _ts(ms):
    import pandas as pd
    return str(pd.to_datetime(int(ms), unit="ms")) + "Z"


def render(res: dict) -> str:
    v = res["verdict"]
    lines = [f"# Volume-profile value-area reclaim: recorded study (#{res['issue']})", "",
             f"Verdict: **{v['verdict'].upper()}**. " + " ".join(r.rstrip(".") + "." for r in v["reasons"]) +
             " production_refused regardless of outcome; promotion needs a separate reviewed decision. The earlier CUT label "
             "for the volume-profile/liquidity family in the candidate report is unchanged.", "",
             "Generated by `run_study.py` from `results.json`; do not edit by hand.", "",
             "## Inputs", "",
             f"- Specification `candidate_spec.json` sha256 `{res['spec_sha256']}`",
             f"- Source commit `{res['source_commit']}`"]
    for name in STUDY_ORDER:
        st = res["studies"][name]
        lines.append(f"- {name}: manifest `{st['manifest']}` sha256 `{st['manifest_sha256']}`; {st['label']}")
        for wname, w in st["windows"].items():
            lines.append(f"  - window `{wname}` [{w['start']}, {w['end']}): {w['role']}")
        for coin, ds in sorted(st["datasets"].items()):
            lines.append(f"  - {coin} 15m: candles `{ds['candles_sha256'][:16]}`, funding `{ds['funding_sha256'][:16]}`, "
                         f"volume profile `{ds['volume_profile_sha256'][:16]}`, half spread {ds['half_spread_bps']} bps")
    p = res["studies"]["primary"]
    rec = p["recording"]
    lines += ["", "## Primary: exact executed-trade profiles", "",
              f"- Recording `{rec['run_id']}` (recorder `{rec['recorder_version']}`), {_ts(rec['started_ms'])} to {_ts(rec['closed_ms'])}: "
              f"{rec['frames']} trade frames, {rec['malformed_frames']} malformed, {rec['dropped']} dropped records. The independent replay "
              f"reproduced all {rec['events_compared']} recorder events; economic evidence: {'yes' if rec['economic_evidence'] else 'no'}.",
              "", "| Coin | Sessions seen | Complete sessions | Unusable sessions (reason code: reason) |", "|---|---|---|---|"]
    for coin in COINS:
        cov = p["import"][coin]
        un = "; ".join(f"{_ts(u['session_start_ms'])[:10]} {u['reason_code']}: {u['reason']} ({u['complete_minutes']} complete minutes)"
                       for u in cov["unavailable"]) or "none"
        lines.append(f"| {coin} | {cov['sessions_seen']} | {cov['complete_sessions']} | {un} |")
    for name in STUDY_ORDER:
        st = res["studies"][name]
        title = "Primary" if name == "primary" else "PROXY sensitivity (candle-distributed volume; not executed-trade volume)"
        lines += ["", f"## {title}: decision coverage at the seed", "",
                  "| Window | Coin | Scored bars | Profile-usable bars | Eligible sessions | Reasons |", "|---|---|---|---|---|---|"]
        for w in ("train", "test"):
            for coin in COINS:
                a = st["seed_audit"][w][coin]
                reasons = ", ".join(f"{k} {n}" for k, n in a["reasons"].items())
                lines.append(f"| {w} | {coin} | {a['scored_bars']} | {a['profile_usable_bars']} | {a['eligible_sessions']} | {reasons} |")
        sel = st["selection"]
        lines += ["", f"Selection: {sel['outcome']} over {sel['grid_points']} grid points; scored parameters "
                      f"`{json.dumps(sel['params'], sort_keys=True)}`; positive train share {_fmt(sel['positive_train_share'])}."]
        if sel["train"]:
            lines += ["", "| Grid point | Pool net PnL $ | Pool return % | Pool max DD % | DD-adjusted | Positions |", "|---|---|---|---|---|---|"]
            for r in sel["train"]:
                pl = r["pool"]
                lines.append(f"| `{json.dumps(r['base'], sort_keys=True)}` | {_fmt(pl['net_pnl_usd'], 2)} | {_fmt(pl['return_pct'])} | "
                             f"{_fmt(pl['max_dd_pct'])} | {_fmt(pl['ddadj'])} | {pl['positions']} |")
        lines += ["", "| Arm | Cost | Scored | Pool net PnL $ | Return % | Max DD % | DD-adjusted | Positions (L/S) | Fees $ | Funding $ | Funding complete | Turnover | Largest coin share |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for arm in ARM_ORDER:
            for cost in COST_ORDER:
                h = st["held_out"][f"{arm}@{cost}"]
                if not h.get("scored"):
                    lines.append(f"| {arm} | {cost} | no: {h['reason']} | | | | | | | | | | |")
                    continue
                pl = h["pool"]
                lines.append(f"| {arm} | {cost} | yes | {_fmt(pl['net_pnl_usd'], 2)} | {_fmt(pl['return_pct'])} | {_fmt(pl['max_dd_pct'])} | "
                             f"{_fmt(pl['ddadj'])} | {pl['positions']} ({pl['long_positions']}/{pl['short_positions']}) | "
                             f"{_fmt(pl['fees_usd'], 2)} | {_fmt(pl['funding_pnl_usd'], 2)} | {'yes' if pl['funding_complete'] else 'no'} | "
                             f"{_fmt(pl['turnover'], 2)} | "
                             f"{_fmt(pl['largest_coin_share'])} |")
    lines += ["", "## Per-coin held-out legs", "",
              "| Data | Arm | Cost | Coin | Net PnL $ | Return % | Max DD % | Positions (L/S) |", "|---|---|---|---|---|---|---|---|"]
    for name in STUDY_ORDER:
        for arm in ARM_ORDER:
            for cost in COST_ORDER:
                h = res["studies"][name]["held_out"][f"{arm}@{cost}"]
                if not h.get("scored"):
                    continue
                for coin in COINS:
                    leg = h["legs"][coin]
                    lines.append(f"| {name} | {arm} | {cost} | {coin} | {_fmt(leg['net_pnl_usd'], 2)} | {_fmt(leg['return_pct'])} | "
                                 f"{_fmt(leg['max_dd_pct'])} | {leg['positions']} ({leg['long_positions']}/{leg['short_positions']}) |")
    lines += ["", "## Evidence kinds", "",
              "- Economic evidence: only the primary data set can supply it, and it holds no complete UTC session, so the result is inconclusive "
              "and no arm is scored on it. The proxy rows are a sensitivity on candle-distributed volume and are not economic evidence for "
              "the executed-trade signal.",
              "- Fixture evidence (not economic): `scripts/test_volume_profile_replay.sh` drives hand-calculated trades through the real feed "
              "owner, recorder, sealer, shared socket and client and checks the sealed sessions, value areas and decisions against the "
              "independent Python replay; `backtest/tests/test_volume_profile_reclaim_integration.py` covers the evaluator rules."]
    lines += ["", "## Protocol", "",
              f"- Every arm: direction `both`, close `tiered_tp_atr` (one tier, 2.0 ATR, full close), entry-ATR stop 1.0, strict close comparison, "
              f"next-open fills, funding charged on every leg, one ${POOL_USD:.0f} pool with ${ALLOCATION_USD:.0f} per coin and no cross-coin borrowing; "
              "the pool result sums the synchronized marked equity of both coins.",
              "- Baselines `anchored_vwap_reversion` and `mean_reversion_pro` run at their registry defaults on the candidate's eligible sessions; "
              "they are not scored where the candidate had no eligible session.",
              "- The proxy is a labelled sensitivity: each 15m candle's whole volume sits at its typical price. It does not model the intra-candle "
              "price distribution, executed-trade identity, duplicates, revocations, socket coverage or late events, and it never changes the verdict.",
              f"- Selection: {SPEC['selection_rule']}"]
    return "\n".join(lines) + "\n"


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="volume_profile_reclaim study (#1665)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("prepare", help="Replay the committed trade recording and build the proxy series; write both manifests")
    r = sub.add_parser("run", help="Score the frozen study and write results.json and REPORT.md")
    r.add_argument("--render-only", action="store_true")
    args = p.parse_args(argv)
    if args.cmd == "prepare":
        print(json.dumps(prepare(), indent=2, sort_keys=True, default=str))
        return 0
    if args.render_only:
        with open(RESULTS_PATH) as fh:
            res = json.load(fh)
    else:
        res = run()
        with open(RESULTS_PATH, "w") as fh:
            json.dump(res, fh, indent=1, sort_keys=True, default=str)
            fh.write("\n")
    with open(REPORT_PATH, "w") as fh:
        fh.write(render(res))
    print(json.dumps(res["verdict"], indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
