#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import itertools
import json
import os
import statistics
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
SPEC_PATH = os.path.join(HERE, "candidate_spec.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
HOUR_MS = 3_600_000
SOURCE_FILES = (
    "shared_strategies/open/macro_event_breakout.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/time_stop.py",
    "shared_tools/macro_event_inputs.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/run_backtest.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/candidates/macro_event_breakout_1654/calendar_sources.py",
    "backtest/candidates/macro_event_breakout_1654/candidate_spec.json",
    "backtest/candidates/macro_event_breakout_1654/run_study.py",
)
QUOTE_REASONS = ("quotes_unavailable", "quotes_malformed", "quote_missing", "quote_conflict", "spread_too_wide")


def load_spec() -> dict:
    with open(SPEC_PATH) as fh:
        return json.load(fh)


SPEC = load_spec()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def protocol_sha256() -> str:
    return hashlib.sha256(json.dumps(SPEC, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def paths_on_sys():
    for p in (BACKTEST, os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open")):
        if p not in sys.path:
            sys.path.insert(0, p)


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def verify_inputs() -> dict:
    paths_on_sys()
    import macro_event_inputs as mi
    import offline_manifest as om
    out = {"calendars": {}, "proxy": {}, "venue": {}}
    for role in ("official", "control"):
        ref = SPEC["calendar"][role]
        doc, sha = mi.load_calendar_artifact(os.path.join(REPO, ref["path"]), ref["sha256"])
        out["calendars"][role] = {"path": ref["path"], "sha256": sha, "records": len(doc["records"])}
    proxy_dir = os.path.join(REPO, SPEC["proxy"]["files_from"])
    for name, sha in sorted(SPEC["proxy"]["sha256"].items()):
        got = sha256_file(os.path.join(proxy_dir, name))
        if got != sha:
            raise SystemExit(f"proxy input {name} sha256 {got} != spec {sha}")
        out["proxy"][name] = sha
    for rel in SPEC["venue"]["manifests"]:
        m = om.load_manifest(os.path.join(REPO, rel))
        out["venue"][rel] = {"sha256": sha256_file(os.path.join(REPO, rel)),
                             "datasets": {d["key"]: {"candles": d["candles"]["sha256"],
                                                     "funding": d["funding"]["sha256"],
                                                     "macro_calendar": d["macro_calendar"]["sha256"]}
                                          for d in m["datasets"]}}
    return out


def build_cache(cache_db: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(cache_db)
    paths_on_sys()
    import pandas as pd
    import storage
    with open(os.path.join(REPO, SPEC["proxy"]["manifest"])) as fh:
        meta_all = json.load(fh)["files"]
    data_dir = os.path.join(REPO, SPEC["proxy"]["files_from"])
    storage.init_db(cache_db)
    for name in sorted(SPEC["proxy"]["sha256"]):
        meta = meta_all[name]
        with gzip.open(os.path.join(data_dir, name), "rt") as fh:
            rows = list(csv.reader(fh))[1:]
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float, "close": float,
                            "volume": float})
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
    funding_fetcher._hl_adapter = refuse


class Capture:
    def __init__(self):
        self.results = []

    def install(self):
        import backtester
        base = backtester.Backtester
        sink = self

        class CapturingBacktester(base):
            def run(self, *a, **k):
                out = super().run(*a, **k)
                sink.results.append(out)
                return out

        backtester.Backtester = CapturingBacktester

    def take(self):
        out = self.results[-1] if self.results else None
        self.results.clear()
        return out


def grid() -> list:
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[SPEC["name"]]
    keys = list(ranges)
    return [dict(zip(keys, vals)) for vals in itertools.product(*(ranges[k] for k in keys))]


def arm_params(arm: str, base: dict) -> dict:
    a = SPEC["arms"][arm]
    ref = SPEC["calendar"][a["calendar"]]
    params = dict(SPEC["params"], **base)
    params["event_type"] = SPEC["event_type"] if a["calendar"] == "official" else SPEC["control_event_type"]
    params["spread_gate"] = bool(a["spread_gate"])
    params["calendar_path"] = ref["path"]
    params["calendar_sha256"] = ref["sha256"]
    return params


def candidate(arm: str, base: dict) -> dict:
    params = arm_params(arm, base)
    close = copy.deepcopy(SPEC["close_strategies"])
    if close[0]["name"] != "time_stop" or close[0]["params"]["max_bars"] != params["holding_bars"]:
        raise SystemExit("the time_stop max_bars must equal holding_bars")
    return {"name": SPEC["name"], "type": SPEC["strategy_type"], "params": params, "direction": SPEC["direction"],
            "close_strategies": close, "stop_loss_atr_mult": SPEC["stop_loss_atr_mult"],
            "comparison_mode": SPEC["comparison_mode"]}


def decision_audit(frame, params: dict, coin: str, manifest=None, dataset=None, window=None) -> dict:
    from registry_loader import load_registry
    from run_backtest import attach_macro_event_inputs
    reg = load_registry(SPEC["registry"])
    inputs, _ = attach_macro_event_inputs(params, f"{coin}/USDT", manifest, dataset, window)
    out = reg.apply_strategy(SPEC["name"], frame, {**params, **inputs})
    relevant = out[out["meb_event_id"] != ""]
    reasons = relevant["meb_reason"].value_counts().to_dict()
    window_rows = relevant[~relevant["meb_reason"].isin(("event_not_known_before_release", "calendar_not_covered"))]
    quote_ok = int(window_rows["meb_spread_frac"].notna().sum()) if params["spread_gate"] else 0
    entries = out[out["signal"] != 0]
    return {
        "event_window_decisions": int(len(window_rows)),
        "quote_usable_decisions": quote_ok,
        "quote_coverage": (round(quote_ok / len(window_rows), 6) if len(window_rows) else None)
        if params["spread_gate"] else None,
        "reasons": {k: int(v) for k, v in sorted(reasons.items())},
        "events_seen": sorted(set(relevant["meb_event_id"])),
        "entries": [{"bar_ms": int(r.meb_deadline_ms - params["expected_interval_ms"]), "side": r.meb_side,
                     "event_id": r.meb_event_id, "event_record_sha256": r.meb_event_record_sha256,
                     "release_ms": int(r.meb_release_ms), "range_high": r.meb_range_high,
                     "range_low": r.meb_range_low, "range_start_ms": int(r.meb_range_start_ms),
                     "range_end_ms": int(r.meb_range_end_ms), "close": float(r.close),
                     "bid": r.meb_bid, "ask": r.meb_ask, "params": r.meb_params}
                    for r in entries.itertuples()],
    }


def leg_summary(leg, results, capital: float) -> dict:
    from eval_windows import execution_metrics
    if leg is None:
        return None
    ex = execution_metrics(results, capital) if results is not None else {}
    positions = ex.get("position_list") or []
    pnl = [p["net_pnl"] for p in positions]
    gross = sum(abs(x) for x in pnl)
    block = leg.get("funding") or {}
    cov = block.get("coverage") or {}
    return {
        "sharpe": leg["sharpe"], "return_pct": leg["return_pct"], "max_dd_pct": leg["max_dd_pct"],
        "ddadj": leg["ddadj"], "trades": leg["trades"], "liquidated": leg["liquidated"],
        "funding_incomplete": bool(leg.get("funding_incomplete")),
        "positions": ex.get("positions", 0), "long_positions": ex.get("long_positions", 0),
        "short_positions": ex.get("short_positions", 0), "fees_usd": ex.get("fees_usd", 0.0),
        "funding_pnl_usd": ex.get("funding_pnl_usd", 0.0), "net_pnl_usd": ex.get("net_pnl_usd", 0.0),
        "turnover": ex.get("turnover"), "bars_in_market": ex.get("bars_in_market", 0),
        "largest_position_share": round(max(abs(x) for x in pnl) / gross, 6) if gross else None,
        "positions_detail": [{"entry": p["entry_date"], "exit": p["exit_date"], "side": p["side"],
                              "net_pnl": round(p["net_pnl"], 6), "exit_reasons": sorted(set(p["exit_reasons"]))}
                             for p in positions],
        "funding": {"mode": block.get("mode"), "complete": block.get("complete"),
                    "missing_hours": cov.get("missing_hours"), "expected_hours": cov.get("expected_hours"),
                    "unpriced_held_hours": block.get("unpriced_held_hours"), "error": block.get("error")},
        "manifest": leg.get("manifest"),
    }


class Runner:
    def __init__(self):
        import eval_windows as ew
        from registry_loader import load_registry
        self.ew = ew
        self.reg = load_registry(SPEC["registry"])
        self.capital = SPEC["capital_usd"]
        self.capture = Capture()
        self.capture.install()
        for name, (s, e) in SPEC["windows"].items():
            ew.WINDOWS[name] = (s, e)

    def proxy(self, arm: str, base: dict, window: str) -> dict:
        ew = self.ew
        cand = candidate(arm, base)
        ew.validate_candidate(cand)
        start, end = SPEC["windows"][window]
        legs, audits = {}, {}
        for symbol in SPEC["proxy"]["symbols"]:
            self.capture.results.clear()
            leg = ew.run_candidate_leg(self.reg, cand, symbol, SPEC["interval"], (start, end), capital=self.capital,
                                       funding_mode=SPEC["proxy"]["funding_mode"], warmup_bars=SPEC["warmup_bars"])
            legs[symbol] = leg_summary(leg, self.capture.take(), self.capital)
            audits[symbol] = self.proxy_audit(cand["params"], symbol, start, end)
        return {"arm": arm, "data": "proxy", "window": window, "params": cand["params"], "legs": legs,
                "audit": audits}

    def proxy_audit(self, params, symbol, start, end):
        import pandas as pd
        from data_fetcher import load_cached_data
        from eval_windows import warmup_load_start
        df = load_cached_data(symbol, SPEC["interval"], start_date=warmup_load_start(start, SPEC["interval"],
                                                                                     SPEC["warmup_bars"]),
                              end_date=end)
        df = df[df.index < pd.Timestamp(end)]
        audit = decision_audit(df, params, symbol.split("/")[0])
        lo, hi = to_ms(start), to_ms(end)
        audit["entries"] = [e for e in audit["entries"] if lo <= e["bar_ms"] < hi]
        return audit

    def venue(self, arm: str, base: dict, cost: float) -> dict:
        import offline_manifest as om
        ew = self.ew
        cand = candidate(arm, base)
        ew.validate_candidate(cand)
        rel = SPEC["venue"]["manifests"][0 if SPEC["arms"][arm]["calendar"] == "official" else 1]
        manifest = om.load_manifest(os.path.join(REPO, rel))
        wname = SPEC["venue"]["window"]
        legs, audits = {}, {}
        for ds in manifest["datasets"]:
            self.capture.results.clear()
            ctx = {"manifest": manifest, "window": wname, "cost_multiplier": cost}
            leg = ew.run_candidate_leg(self.reg, cand, ds["coin"], manifest["interval"], (None, None),
                                       capital=self.capital, manifest_ctx=ctx)
            legs[ds["symbol"]] = leg_summary(leg, self.capture.take(), self.capital)
            frame, win, _ = om.window_frame(manifest, ds, wname)
            audit = decision_audit(frame, cand["params"], ds["coin"], manifest, ds, win)
            audit["entries"] = [e for e in audit["entries"] if win["start_ms"] <= e["bar_ms"] < win["end_ms"]]
            audits[ds["symbol"]] = audit
        return {"arm": arm, "data": "venue", "window": wname, "cost_multiplier": cost, "manifest": rel,
                "params": cand["params"], "legs": legs, "audit": audits}


def venue_costs() -> dict:
    with open(os.path.join(REPO, SPEC["venue"]["manifests"][0])) as fh:
        raw = json.load(fh)
    return {"venue_costs": raw["costs"], "half_spread_bps": {d["coin"]: d["half_spread_bps"] for d in raw["datasets"]}}


def mean(values):
    vals = [v for v in values if v is not None]
    return round(statistics.mean(vals), 6) if vals else None


def aggregate(run: dict) -> dict:
    legs = [l for l in run["legs"].values() if l is not None]
    events = sorted({e["event_id"] for a in run["audit"].values() for e in a["entries"]})
    decisions = sum(a["event_window_decisions"] for a in run["audit"].values())
    usable = sum(a["quote_usable_decisions"] for a in run["audit"].values())
    gated = bool(run["params"]["spread_gate"])
    return {
        "datasets": len(run["legs"]), "scored": len(legs),
        "mean_return_pct": mean(l["return_pct"] for l in legs), "mean_sharpe": mean(l["sharpe"] for l in legs),
        "mean_ddadj": mean(l["ddadj"] for l in legs),
        "worst_max_dd_pct": min((l["max_dd_pct"] for l in legs), default=None),
        "net_pnl_usd": round(sum(l["net_pnl_usd"] for l in legs), 6),
        "positions": sum(l["positions"] for l in legs), "long_positions": sum(l["long_positions"] for l in legs),
        "short_positions": sum(l["short_positions"] for l in legs),
        "fees_usd": round(sum(l["fees_usd"] for l in legs), 6),
        "funding_pnl_usd": round(sum(l["funding_pnl_usd"] for l in legs), 6),
        "turnover": round(sum(l["turnover"] or 0 for l in legs), 6),
        "funding_complete": all(not l["funding_incomplete"] and l["funding"]["complete"] is not False for l in legs),
        "liquidated": any(l["liquidated"] for l in legs),
        "independent_events_with_entries": len(events), "events_with_entries": events,
        "event_window_decisions": decisions, "quote_usable_decisions": usable,
        "quote_coverage": (round(usable / decisions, 6) if decisions else None) if gated else None,
        "largest_position_share": max((l["largest_position_share"] or 0 for l in legs), default=None),
    }


def select(rows: list) -> dict:
    eligible = [r for r in rows if r["aggregate"]["scored"] == r["aggregate"]["datasets"]
                and r["aggregate"]["funding_complete"] and not r["aggregate"]["liquidated"]]
    if not eligible:
        return None
    order = {json.dumps(r["base"], sort_keys=True): i for i, r in enumerate(rows)}
    return sorted(eligible, key=lambda r: (-(r["aggregate"]["mean_return_pct"] or -1e18),
                                           -(r["aggregate"]["mean_ddadj"] or -1e18),
                                           order[json.dumps(r["base"], sort_keys=True)]))[0]


def verdict(held: dict, venue: dict) -> dict:
    reasons = []
    full = held["candidate_full"]["aggregate"]
    if (full["quote_coverage"] or 0) < SPEC["min_quote_coverage"]:
        reasons.append(f"candidate_full quote coverage {full['quote_coverage']} is below "
                       f"{SPEC['min_quote_coverage']}: no historical bid/ask record backs the spread gate")
    diag = held["price_only_event"]["aggregate"]
    n = max(full["independent_events_with_entries"], diag["independent_events_with_entries"])
    if n < SPEC["min_independent_events"]:
        reasons.append(f"at most {n} independent official events produced an entry in held_out, below "
                       f"{SPEC['min_independent_events']}")
    for name, block in list(held.items()) + [("venue:" + k, v) for k, v in venue.items()]:
        if not block["aggregate"]["funding_complete"]:
            reasons.append(f"{name} has a funding gap")
    if reasons:
        return {"verdict": "inconclusive", "reasons": reasons}
    passed = (full["mean_return_pct"] > 0 and venue["candidate_full@stress"]["aggregate"]["mean_return_pct"] > 0
              and full["mean_return_pct"] > held["price_only_control"]["aggregate"]["mean_return_pct"])
    return {"verdict": "pass" if passed else "fail", "reasons": []}


def run(skip_stability: bool) -> dict:
    inputs = verify_inputs()
    with tempfile.TemporaryDirectory() as tmp:
        cache_db = os.path.join(tmp, "cache.db")
        build_cache(cache_db)
        forbid_network()
        runner = Runner()
        train = []
        for base in grid():
            r = runner.proxy("price_only_event", base, "train")
            train.append({"base": base, "aggregate": aggregate(r), "run": r})
        chosen = select(train)
        if chosen is None:
            raise SystemExit("no eligible training variant; the study is inconclusive by its selection rule")
        base = chosen["base"]
        held, train_arms = {}, {}
        for arm in SPEC["arms"]:
            r = runner.proxy(arm, base, "held_out")
            held[arm] = {"aggregate": aggregate(r), "run": r}
            t = runner.proxy(arm, base, "train")
            train_arms[arm] = {"aggregate": aggregate(t), "run": t}
        venue = {}
        for arm in SPEC["arms"]:
            for label, cost in SPEC["venue"]["cost_runs"].items():
                r = runner.venue(arm, base, cost)
                venue[f"{arm}@{label}"] = {"aggregate": aggregate(r), "run": r}
        stability = []
        if not skip_stability:
            for variant in grid():
                r = runner.proxy("price_only_event", variant, "held_out")
                stability.append({"base": variant, "aggregate": aggregate(r)})
    result = {
        "study": "macro_event_breakout_1654",
        "protocol_sha256": protocol_sha256(),
        "source_commit": git_head(),
        "sources_sha256": {p: sha256_file(os.path.join(REPO, p)) for p in SOURCE_FILES},
        "inputs": inputs,
        "costs": venue_costs(),
        "invocation": "uv run --no-sync python backtest/candidates/macro_event_breakout_1654/run_study.py run",
        "selection": {"rule": SPEC["selection"], "selected": base,
                      "train_variants": [{"base": r["base"], "aggregate": r["aggregate"]} for r in train]},
        "train": {k: v["aggregate"] for k, v in train_arms.items()},
        "held_out": {k: v["aggregate"] for k, v in held.items()},
        "venue": {k: v["aggregate"] for k, v in venue.items()},
        "stability_exploratory": stability,
        "detail": {"train": {k: v["run"] for k, v in train_arms.items()},
                   "held_out": {k: v["run"] for k, v in held.items()},
                   "venue": {k: v["run"] for k, v in venue.items()}},
    }
    result["verdict"] = verdict(held, venue)
    result["verdict"]["live_status"] = SPEC["verdict"]["live_status"]
    with open(RESULTS_PATH, "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    return result


def fmt(v, nd=3):
    if v is None:
        return "n/a"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def render() -> str:
    with open(RESULTS_PATH) as fh:
        r = json.load(fh)
    cal = r["inputs"]["calendars"]
    lines = [
        "# macro_event_breakout study (#1654)",
        "",
        f"Verdict: **{r['verdict']['verdict'].upper()}**. Reasons: " + "; ".join(r["verdict"]["reasons"])
        + ". Live status: " + r["verdict"]["live_status"] + ".",
        "",
        "## Plain simple English",
        "",
        "The strategy enters after a scheduled US rate statement when price breaks the range set before it. "
        "The study has no historical bid and ask quotes, so the full rule, which needs a spread check, never "
        "trades. The sample of statements is also too small. The result is inconclusive, and the strategy stays "
        "refused in production.",
        "",
        "## Inputs",
        "",
        f"- Protocol sha256 `{r['protocol_sha256']}`; source commit `{r['source_commit']}`.",
        f"- Official calendar `{cal['official']['path']}` sha256 `{cal['official']['sha256']}` "
        f"({cal['official']['records']} records).",
        f"- Matched control calendar `{cal['control']['path']}` sha256 `{cal['control']['sha256']}` "
        f"({cal['control']['records']} records; research controls, not official releases).",
        "- Quotes: none. No historical Hyperliquid bid/ask archive with per-decision receipt times was available. "
        "The l2Book snapshot in the venue manifest is a cost assumption only.",
        "- Proxy: Binance US spot 1h candles and Hyperliquid funding from `backtest/candidates/chande_momentum_1661/data` "
        "(hash-pinned in `candidate_spec.json`), `--funding charge` on every leg.",
        "- Venue: Hyperliquid 1h candles, funding, meta and one l2Book snapshot in "
        "`backtest/candidates/macro_event_breakout_1654_venue_manifest.json` (schema `offline_candle_manifest/v4`).",
        f"- Invocation: `{r['invocation']}`.",
        "",
        "## Selection (train, proxy, price_only_event)",
        "",
        f"Selected parameters: `{json.dumps(r['selection']['selected'], sort_keys=True)}`.",
        "",
        "| variant | mean return % | mean ddadj | positions | events with entries | funding complete |",
        "|---|---|---|---|---|---|",
    ]
    for v in r["selection"]["train_variants"]:
        a = v["aggregate"]
        lines.append(f"| `{json.dumps(v['base'], sort_keys=True)}` | {fmt(a['mean_return_pct'])} | {fmt(a['mean_ddadj'])} "
                     f"| {a['positions']} | {a['independent_events_with_entries']} | {a['funding_complete']} |")
    for title, block in (("Train window, selected parameters (proxy)", r["train"]),
                         ("Held-out window (proxy)", r["held_out"]),
                         ("Venue window 2026-04-01 to 2026-10-01 (Hyperliquid, manifest costs)", r["venue"])):
        lines += ["", f"## {title}", "",
                  "| arm | mean return % | mean Sharpe | worst max DD % | net PnL $ | positions (L/S) | fees $ | "
                  "funding $ | turnover | largest position share | events with entries | quote coverage | "
                  "funding complete |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for arm, a in block.items():
            lines.append(f"| {arm} | {fmt(a['mean_return_pct'])} | {fmt(a['mean_sharpe'])} | {fmt(a['worst_max_dd_pct'])} "
                         f"| {fmt(a['net_pnl_usd'], 2)} | {a['positions']} ({a['long_positions']}/{a['short_positions']}) "
                         f"| {fmt(a['fees_usd'], 2)} | {fmt(a['funding_pnl_usd'], 2)} | {fmt(a['turnover'])} "
                         f"| {fmt(a['largest_position_share'])} | {a['independent_events_with_entries']} "
                         f"| {fmt(a['quote_coverage']) if a['quote_coverage'] is not None else 'gate off'} "
                         f"| {a['funding_complete']} |")
    if r["stability_exploratory"]:
        lines += ["", "## Parameter stability (exploratory, held-out, price_only_event, after the freeze)", "",
                  "| variant | mean return % | positions |", "|---|---|---|"]
        for v in r["stability_exploratory"]:
            lines.append(f"| `{json.dumps(v['base'], sort_keys=True)}` | {fmt(v['aggregate']['mean_return_pct'])} "
                         f"| {v['aggregate']['positions']} |")
    lines += [
        "",
        "## Costs and their provenance",
        "",
        f"- Venue leg: `{r['costs']['venue_costs']}` from the manifest, half spread per coin "
        f"`{r['costs']['half_spread_bps']}` bps from one l2Book snapshot, szDecimals lot flooring from the pinned "
        "meta, hourly Hyperliquid funding; stress doubles half spread and slippage, fees unchanged.",
        "- Proxy leg: the cached-path platform fee model for Hyperliquid perps and hourly Hyperliquid funding "
        "(`--funding charge`); no spread, lot or minimum-size model.",
        "- Unmodeled: per-decision spread (no quotes), queue position, partial fills, latency after the bar close, "
        "and event-time liquidity changes beyond the stress multiplier.",
        "",
        "## Limits and unmodeled costs",
        "",
        "- The full contract needs a supplied bid/ask quote at each decision. None exists for this window, so "
        "`candidate_full` holds every event-window decision with `quotes_unavailable`. A fixed spread assumption "
        "or the current l2Book snapshot cannot replace that input.",
        "- The 2023 and 2024 tentative schedules name meeting dates but not the 2 p.m. ET statement time. Their "
        "records carry no publication time and are never eligible, so only 2025 and 2026 statements count.",
        "- The proxy leg prices Binance US spot candles; it is not Hyperliquid execution. Its fees come from the "
        "cached-path platform model; spread, lot and minimum-size rules apply only on the venue leg.",
        "- `price_only_event` and `price_only_control` drop the spread gate. They are diagnostics with identical "
        "calendar eligibility, exits, sizing and costs; they cannot produce a pass.",
        "- `time_stop` needs `bars_held`, which the research simulator supplies. Live and paper checks do not "
        "supply it, so a runtime position would close only through its stop. The candidate is production-refused, "
        "so no runtime position exists.",
        "- Unscheduled FOMC actions (the 2025-08-22 notation vote) are not scheduled releases and are excluded.",
        "",
        "---",
        "Created with LLM: Opus 5.5 | high | Harness: Claude Code",
        "",
    ]
    text = "\n".join(lines)
    with open(REPORT_PATH, "w") as fh:
        fh.write(text)
    return text


def main() -> int:
    ap = argparse.ArgumentParser(description="Frozen macro_event_breakout study (#1654); offline, sends no order")
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--skip-stability", action="store_true")
    sub.add_parser("render")
    sub.add_parser("verify")
    sub.add_parser("protocol-hash")
    args = ap.parse_args()
    if args.cmd == "run":
        out = run(args.skip_stability)
        render()
        print(json.dumps(out["verdict"], indent=1))
    elif args.cmd == "render":
        render()
    elif args.cmd == "verify":
        print(json.dumps(verify_inputs(), indent=1, sort_keys=True))
    else:
        print(protocol_sha256())
    return 0


if __name__ == "__main__":
    sys.exit(main())
