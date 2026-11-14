import copy
import json
import math
import os
import shutil
import time

import run_study as rs

LOCKED_TOP = ("protocol.json", "protocol_lock.json", "study_manifest.json", "screen.json")


def make_copy(paths: rs.Paths, proto: dict, work: str, name: str) -> rs.Paths:
    root = os.path.join(work, name)
    if os.path.exists(root):
        raise SystemExit(f"{root} exists; verify needs a fresh work directory")
    os.makedirs(root)
    for f in LOCKED_TOP:
        shutil.copy(os.path.join(paths.root, f), os.path.join(root, f))
    for pop in rs.POPULATIONS:
        mp = paths.manifest(proto, pop)
        if os.path.exists(mp):
            shutil.copy(mp, os.path.join(root, os.path.basename(mp)))
    shutil.copytree(paths.data, os.path.join(root, "data"))
    return rs.Paths(root)


def relock_copy(cp: rs.Paths) -> None:
    if os.path.realpath(cp.root) == os.path.realpath(rs.HERE):
        raise SystemExit("relock_copy refuses the canonical study directory")
    proto = rs.protocol(cp)
    inv = rs.load_json(cp.inventory)
    for pop, m in inv["manifests"].items():
        m["sha256"] = rs.sha256_file(os.path.join(cp.root, m["path"]))
    for rel, meta in inv["files"].items():
        if os.path.exists(os.path.join(cp.root, rel)):
            meta["sha256"] = rs.sha256_file(os.path.join(cp.root, rel))
    inv["screen"]["sha256"] = rs.sha256_file(cp.screen)
    rs.write_json(cp.inventory, inv)
    if os.path.exists(cp.lock):
        os.remove(cp.lock)
    rs.write_json(cp.lock, rs.lock_record(cp, proto))


def rewrite_gz(path: str, mutate) -> None:
    header, rows = rs.read_gz_rows(path)
    rs.write_gz_csv(path, header, mutate(rows))


def set_manifest_hash(cp: rs.Paths, proto: dict, pop: str, rel: str) -> None:
    mp = cp.manifest(proto, pop)
    m = rs.load_json(mp)
    for d in m["datasets"]:
        for k in ("candles", "funding"):
            if d.get(k) and d[k]["path"] == rel:
                d[k]["sha256"] = rs.sha256_file(os.path.join(cp.root, rel))
    rs.write_json(mp, m)


def expect_refusal(name: str, fn, needle: str) -> dict:
    try:
        fn()
    except SystemExit as exc:
        msg = str(exc)
        return {"name": name, "kind": "refusal", "expected": f"refused ({needle})", "observed": msg[:300],
                "ok": needle in msg}
    return {"name": name, "kind": "refusal", "expected": f"refused ({needle})", "observed": "accepted", "ok": False}


def scenario_run(name: str, cp: rs.Paths, work: str, needle: str, workers: int) -> dict:
    out = rs.run(cp, os.path.join(work, "caches", name, "cache.db"), os.path.join(cp.root, "results.json"),
                 populations=[rs.PRIMARY], workers=workers, git_check=False, score_invalid=False)
    v = out["verdict"]
    return {"name": name, "kind": "real runner on a disposable copy", "expected": f"inconclusive ({needle})",
            "observed": f"{v['outcome']}: {v['reason'][:300]}",
            "ok": v["outcome"] == "inconclusive" and needle in v["reason"]}


def verdict_case(name: str, proto: dict, pop: dict, expected: str, needle: str = "", kind: str = "saved evidence") -> dict:
    v = rs.primary_verdict(proto, pop)
    return {"name": name, "kind": kind, "expected": expected + (f" ({needle})" if needle else ""),
            "observed": f"{v['outcome']}: {v['reason'][:300]}",
            "ok": v["outcome"] == expected and needle in v["reason"]}


def first_ds(pop: dict) -> str:
    return sorted(pop["legs"]["base"]["candidate"])[0]


def lift_to_floor(proto: dict, pop: dict) -> tuple:
    floor = proto["sample_rules"]["min_primary_clusters"]
    out = copy.deepcopy(pop)
    ds = first_ds(out)
    import pandas as pd
    have = rs.position_clusters([p for d in out["legs"]["base"]["candidate"].values()
                                 for p in d["execution"]["position_list"]])
    added = 0
    if have >= floor:
        return out, 0
    start = pd.Timestamp(proto["populations"][rs.PRIMARY]["window"][0])
    all_pos = [p for d in out["legs"]["base"]["candidate"].values() for p in d["execution"]["position_list"]]
    spans = [(pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in all_pos]
    t = start
    plist = out["legs"]["base"]["candidate"][ds]["execution"]["position_list"]
    while have < floor:
        s, e = t, t + pd.Timedelta(hours=4)
        if not any(not (e < a or s > b) for a, b in spans):
            plist.append({"entry_date": str(s), "exit_date": str(s), "side": "long", "net_pnl": 0.0, "fees": 0.0,
                          "entry_notional": 0.0, "exit_notional": 0.0, "bars_held": 0, "closes": 1,
                          "exit_reasons": ["fixture"]})
            spans.append((s, s))
            added += 1
            have = rs.position_clusters([p for d in out["legs"]["base"]["candidate"].values()
                                         for p in d["execution"]["position_list"]])
        t = t + pd.Timedelta(hours=8)
    return out, added


def force_all_true(pop: dict) -> dict:
    out = copy.deepcopy(pop)
    for cost in rs.COSTS:
        for ds, leg in out["legs"][cost]["candidate"].items():
            leg["sharpe"] = 50.0
            leg["ddadj"] = 500.0
            leg["trades"] = max(1, leg["trades"])
            leg["liquidated"] = False
            leg["execution"]["net_pnl_usd"] = abs(leg["execution"]["net_pnl_usd"]) + 1.0
    return out


def screen_replay(paths: rs.Paths, proto: dict) -> dict:
    scr = rs.load_json(paths.screen)
    meta, ctxs = rs.load_json(os.path.join(paths.root, scr["liquidity_response"]["path"]))
    excluded = set(proto["screen"]["excluded_coins"])
    ranked = []
    for e, c in zip(meta["universe"], ctxs):
        try:
            vol = float(c.get("dayNtlVlm"))
        except (TypeError, ValueError):
            continue
        if e.get("isDelisted") or e["name"] in excluded or not math.isfinite(vol):
            continue
        ranked.append((-vol, e["name"]))
    ranked = [n for _, n in sorted(ranked)]
    order_ok = [s["coin"] for s in scr["screened"]] == ranked[:len(scr["screened"])]
    selected = [s["coin"] for s in scr["screened"] if s["candles"]["complete"] and s["funding"]["complete"]]
    stop_ok = len(selected) == proto["screen"]["max_coins"] or len(scr["screened"]) == len(ranked)
    recheck = {}
    win = proto["populations"][rs.PRIMARY]["window"]
    s, e = rs.to_ms(win[0]), rs.to_ms(win[1])
    for s_ in scr["screened"]:
        if not s_["selected"]:
            continue
        coin = s_["coin"]
        _, crow = rs.read_gz_rows(os.path.join(paths.root, f"data/venue/{coin}_4h_candles.csv.gz"))
        _, frow = rs.read_gz_rows(os.path.join(paths.root, f"data/venue/{coin}_funding.csv.gz"))
        cc = rs.candle_checks(crow, "4h", s, e, proto["warmup_bars"])
        fc = rs.funding_checks(frow, s, e)
        csha = rs.sha256_bytes(json.dumps([[int(r[0])] + r[1:] for r in crow]).encode())
        fsha = rs.sha256_bytes(json.dumps([[int(r[0]), r[1]] for r in frow]).encode())
        recheck[coin] = {"complete": cc["complete"] and fc["complete"],
                         "fetched_hashes_match": csha == s_["fetched_candles_sha256"]
                         and fsha == s_["fetched_funding_sha256"]}
    ok = (order_ok and stop_ok and selected == scr["selected"] == scr["rosters"][rs.PRIMARY]
          and all(v["complete"] and v["fetched_hashes_match"] for v in recheck.values()))
    return {"name": "screen replay from the frozen liquidity response and coverage records",
            "kind": "frozen inputs", "expected": "ranking order, stop rule and selection reproduce",
            "observed": json.dumps({"order_ok": order_ok, "stop_ok": stop_ok, "selected": selected,
                                    "recheck": recheck}, sort_keys=True)[:600], "ok": ok}


def comparable(res: dict) -> dict:
    return {k: res.get(k) for k in ("populations", "verdict", "primary_reduced", "descriptive_reduced", "rosters",
                                    "protocol_sha256")}


def main(paths: rs.Paths, work_dir: str, workers: int, skip_rerun: bool) -> None:
    work = os.path.abspath(work_dir)
    if os.path.exists(work) and os.listdir(work):
        raise SystemExit(f"{work} is not empty; pass a fresh work directory")
    if os.path.commonpath([work, paths.root]) == paths.root:
        raise SystemExit("the work directory must be outside the study directory")
    os.makedirs(work, exist_ok=True)
    if not os.path.exists(paths.results):
        raise SystemExit("results.json is missing; verify runs after the canonical scored run")
    proto = rs.protocol(paths)
    canonical = rs.load_json(paths.results)
    prim = canonical["populations"][rs.PRIMARY]
    cases = []
    t0 = time.time()

    held = rs.require_lock(paths, git_check=True)
    cases.append({"name": "canonical lock order in branch history", "kind": "git",
                  "expected": "lock committed once, before results, sources clean",
                  "observed": json.dumps(held["git"]), "ok": True})
    cases.append(screen_replay(paths, proto))

    def mutate_copy(name, fn):
        cp = make_copy(paths, proto, work, name)
        fn(cp)
        return cp

    def edit_protocol(cp):
        d = rs.load_json(cp.protocol)
        d["capital_usd"] = 2000.0
        rs.write_json(cp.protocol, d)
    cp = mutate_copy("protocol_changed", edit_protocol)
    cases.append(expect_refusal("changed protocol hash", lambda: rs.require_lock(cp, git_check=False),
                                "protocol.json differs from the lock"))

    def edit_inventory(cp):
        d = rs.load_json(cp.inventory)
        d["note"] = "edited"
        rs.write_json(cp.inventory, d)
    cp2 = mutate_copy("inventory_changed", edit_inventory)
    cases.append(expect_refusal("changed input inventory hash", lambda: rs.require_lock(cp2, git_check=False),
                                "differs from the lock"))

    coin0 = prim["roster"][0]
    fund0 = f"data/venue/{coin0}_funding.csv.gz"

    def edit_data(cp):
        rewrite_gz(os.path.join(cp.root, fund0), lambda rows: [[r[0], "0.0"] if i == 10 else r
                                                               for i, r in enumerate(rows)])
    cp3 = mutate_copy("input_changed", edit_data)
    cases.append(expect_refusal("changed input file hash", lambda: rs.require_lock(cp3, git_check=False),
                                f"registered input {fund0}"))

    def add_file(cp):
        with open(os.path.join(cp.root, "data", "venue", "EXTRA_4h_candles.csv.gz"), "wb") as fh:
            fh.write(b"x")
    cp4 = mutate_copy("unregistered_input", add_file)
    cases.append(expect_refusal("unregistered input file", lambda: rs.require_lock(cp4, git_check=False),
                                "unregistered files under data/"))

    cp5 = mutate_copy("lock_missing", lambda cp: os.remove(cp.lock))
    cases.append(expect_refusal("missing lock", lambda: rs.require_lock(cp5, git_check=False),
                                "protocol_lock.json is missing"))

    def edit_source(cp):
        d = rs.load_json(cp.lock)
        k = "backtest/eval_windows.py"
        d["source_sha256"][k] = "0" * 64
        rs.write_json(cp.lock, d)
    cp6 = mutate_copy("source_changed", edit_source)
    cases.append(expect_refusal("changed scoring source hash", lambda: rs.require_lock(cp6, git_check=False),
                                "scoring sources differ from the lock"))

    def edit_manifest(cp):
        mp = cp.manifest(proto, rs.PRIMARY)
        d = rs.load_json(mp)
        d["datasets"][0]["half_spread_bps"] = 0.0
        rs.write_json(mp, d)
    cp7 = mutate_copy("manifest_changed", edit_manifest)
    cases.append(expect_refusal("changed manifest", lambda: rs.require_lock(cp7, git_check=False),
                                "differs from the inventory"))

    def replace_lock(cp):
        shutil.copy(paths.results, cp.results)
        os.remove(cp.lock)
    cp8 = mutate_copy("lock_after_scores", replace_lock)
    cases.append(expect_refusal("lock written after scores", lambda: rs.lock(cp8),
                                "results exist; the lock must precede every score"))

    cp9 = mutate_copy("cache_reuse", lambda cp: None)
    used = os.path.join(work, "caches", "reused.db")
    os.makedirs(os.path.dirname(used), exist_ok=True)
    open(used, "w").close()
    cases.append(expect_refusal("existing research cache", lambda: rs.run(
        cp9, used, os.path.join(cp9.root, "results.json"), populations=[rs.PRIMARY], workers=workers,
        git_check=False), "refusing to reuse an existing research cache"))

    def drop_dataset(cp):
        mp = cp.manifest(proto, rs.PRIMARY)
        d = rs.load_json(mp)
        d["datasets"] = d["datasets"][:-1]
        rs.write_json(mp, d)
        relock_copy(cp)
    cases.append(scenario_run("missing registered dataset", mutate_copy("missing_dataset", drop_dataset), work,
                              "dataset roster differs", workers))

    def wrong_coin(cp):
        mp = cp.manifest(proto, rs.PRIMARY)
        d = rs.load_json(mp)
        fwd = rs.load_json(cp.manifest(proto, "forward_4h"))
        d["datasets"][-1] = copy.deepcopy(fwd["datasets"][0])
        rs.write_json(mp, d)
        relock_copy(cp)
    cases.append(scenario_run("same-size roster with a wrong coin", mutate_copy("wrong_coin", wrong_coin), work,
                              "dataset roster differs", workers))

    def no_funding(cp):
        mp = cp.manifest(proto, rs.PRIMARY)
        d = rs.load_json(mp)
        d["datasets"][0].pop("funding")
        rs.write_json(mp, d)
        relock_copy(cp)
    cases.append(scenario_run("missing funding series", mutate_copy("missing_funding", no_funding), work,
                              "no funding series attached", workers))

    win = proto["populations"][rs.PRIMARY]["window"]
    ws, we = rs.to_ms(win[0]), rs.to_ms(win[1])
    mid = ws + (we - ws) // 2

    def funding_gap(cp):
        rewrite_gz(os.path.join(cp.root, fund0),
                   lambda rows: [r for r in rows if not (mid <= int(r[0]) < mid + rs.HOUR_MS)])
        set_manifest_hash(cp, proto, rs.PRIMARY, fund0)
        relock_copy(cp)
    cases.append(scenario_run("interior funding gap", mutate_copy("funding_gap", funding_gap), work,
                              "funding hours missing", workers))

    cand0 = f"data/venue/{coin0}_4h_candles.csv.gz"

    def bad_candle(cp):
        def mut(rows):
            out = []
            for r in rows:
                if int(r[0]) == ws + 40 * rs.INTERVAL_MS["4h"]:
                    r = [r[0], r[1], str(float(r[4]) * 0.5), r[3], r[4], r[5]]
                out.append(r)
            return out
        rewrite_gz(os.path.join(cp.root, cand0), mut)
        set_manifest_hash(cp, proto, rs.PRIMARY, cand0)
        relock_copy(cp)
    cases.append(scenario_run("malformed candle", mutate_copy("bad_candle", bad_candle), work,
                              "invalid OHLCV bars", workers))

    def warm_gap(cp):
        gap = ws - 50 * rs.INTERVAL_MS["4h"]
        rewrite_gz(os.path.join(cp.root, cand0), lambda rows: [r for r in rows if int(r[0]) != gap])
        set_manifest_hash(cp, proto, rs.PRIMARY, cand0)
        relock_copy(cp)
    cases.append(scenario_run("warm-up gap", mutate_copy("warmup_gap", warm_gap), work,
                              "warm-up bars missing", workers))

    v = rs.primary_verdict(proto, prim)
    same = {k: v[k] for k in ("outcome", "reason", "pooled_clusters", "checks")} == \
        {k: canonical["verdict"][k] for k in ("outcome", "reason", "pooled_clusters", "checks")}
    cases.append({"name": "verdict recomputed from saved leg evidence", "kind": "saved evidence",
                  "expected": canonical["verdict"]["outcome"], "observed": f"{v['outcome']}: {v['reason'][:200]}",
                  "ok": same})

    ds = first_ds(prim)
    for cost in rs.COSTS:
        p = copy.deepcopy(prim)
        del p["legs"][cost]["momentum_pro"][ds]
        cases.append(verdict_case(f"missing incumbent leg at {cost} cost", proto, p, "inconclusive",
                                  f"{cost} momentum_pro {ds}: leg missing"))
    p = copy.deepcopy(prim)
    p["legs"]["base"]["candidate"][ds] = {"error": "TimeoutError: fixture"}
    cases.append(verdict_case("errored candidate leg", proto, p, "inconclusive", "TimeoutError"))
    p = copy.deepcopy(prim)
    p["legs"]["stress"]["candidate"][ds]["sharpe"] = float("nan")
    cases.append(verdict_case("non-finite candidate metric", proto, p, "inconclusive", "non-finite ['sharpe']"))
    p = copy.deepcopy(prim)
    p["legs"]["base"]["breakout"]["ZZZ 4h"] = copy.deepcopy(p["legs"]["base"]["breakout"][ds])
    cases.append(verdict_case("unexpected comparator dataset", proto, p, "inconclusive", "!= frozen"))
    p = copy.deepcopy(prim)
    p["legs"]["base"]["sma_crossover"][ds]["manifest"]["funding_coverage"]["complete"] = False
    cases.append(verdict_case("incumbent funding coverage incomplete", proto, p, "inconclusive",
                              "funding coverage incomplete"))
    p = copy.deepcopy(prim)
    p["entry_audit"] = {"ok": False, "failures": [{"problem": "fixture"}], "entries": 0, "rows": []}
    cases.append(verdict_case("entry audit failure", proto, p, "inconclusive", "entry audit"))
    p = copy.deepcopy(prim)
    p["legs"]["base"].pop("chart_pattern")
    cases.append(verdict_case("incumbent arm absent", proto, p, "inconclusive", "chart_pattern: no legs"))

    floor = proto["sample_rules"]["min_primary_clusters"]
    lifted, added = lift_to_floor(proto, prim)
    if added == 0:
        p = copy.deepcopy(prim)
        keep = []
        for d in sorted(p["legs"]["base"]["candidate"]):
            plist = p["legs"]["base"]["candidate"][d]["execution"]["position_list"]
            keep_here = []
            for pos in plist:
                trial = keep + keep_here + [pos]
                if rs.position_clusters(trial) <= floor - 1:
                    keep_here.append(pos)
            p["legs"]["base"]["candidate"][d]["execution"]["position_list"] = keep_here
            keep += keep_here
        cases.append(verdict_case("valid sample below the floor (positions removed)", proto, p, "inconclusive",
                                  f"< {floor}"))
    else:
        cases.append(verdict_case("valid sample below the floor (the scored primary sample)", proto, prim,
                                  "inconclusive", f"< {floor}"))
    fixture_kind = "saved evidence lifted to the floor with zero-PnL fixture positions" if added else "saved evidence"
    allp = force_all_true(lifted)
    cases.append(verdict_case("all frozen checks true", proto, allp, "pass", "every frozen check is true",
                              kind="isolated verdict fixture (not market evidence)"))
    flips = {
        "net_positive_base": lambda q: [l["execution"].__setitem__("net_pnl_usd", -abs(l["execution"]["net_pnl_usd"]) - 1e3)
                                        for l in q["legs"]["base"]["candidate"].values()],
        "net_positive_stress": lambda q: [l["execution"].__setitem__("net_pnl_usd", -abs(l["execution"]["net_pnl_usd"]) - 1e3)
                                          for l in q["legs"]["stress"]["candidate"].values()],
        "m1_pass_base": lambda q: [l.__setitem__("ddadj", -1e6) for l in q["legs"]["base"]["candidate"].values()],
        "beats_breakout_sharpe_base": lambda q: [l.__setitem__("sharpe", 1e6)
                                                 for l in q["legs"]["base"]["breakout"].values()],
        "beats_session_breakout_sharpe_base": lambda q: [l.__setitem__("sharpe", 1e6)
                                                         for l in q["legs"]["base"]["session_breakout"].values()],
    }
    for check, fn in flips.items():
        q = copy.deepcopy(allp)
        fn(q)
        cases.append(verdict_case(f"performance failure: {check}", proto, q, "fail", check,
                                  kind="isolated verdict fixture (not market evidence)"))
    q = copy.deepcopy(allp)
    del q["legs"]["stress"]["range_scalper"][ds]
    cases.append(verdict_case("all checks true but a stress incumbent leg missing", proto, q, "inconclusive",
                              "leg missing", kind="isolated verdict fixture (not market evidence)"))
    q = copy.deepcopy(allp)
    q["inputs"]["reasons"] = [f"{ds} funding: 1 funding hours missing (fixture)"]
    cases.append(verdict_case("all checks true but an input check failed", proto, q, "inconclusive",
                              "funding hours missing", kind="isolated verdict fixture (not market evidence)"))
    cases.append({"name": "fixture provenance", "kind": "note", "expected": "-",
                  "observed": f"{fixture_kind}; {added} fixture positions added", "ok": True})

    a = {"entry_date": "2026-01-01 00:00:00", "exit_date": "2026-01-01 08:00:00"}
    b = {"entry_date": "2026-01-01 04:00:00", "exit_date": "2026-01-01 12:00:00"}
    c = {"entry_date": "2026-01-01 12:00:00", "exit_date": "2026-01-01 16:00:00"}
    per = [rs.position_clusters([a]), rs.position_clusters([b]), rs.position_clusters([c])]
    pooled = rs.position_clusters([a, b, c])
    cases.append({"name": "span bridge: pooled clusters differ from summed per-coin clusters", "kind": "synthetic spans",
                  "expected": "per-coin 1+1+1=3, pooled 1 (overlap and touching spans merge)",
                  "observed": f"per-coin {per} sum {sum(per)}, pooled {pooled}", "ok": sum(per) == 3 and pooled == 1})
    red = canonical["primary_reduced"]["arms"]["base"]["candidate"]
    cases.append({"name": "scored primary: pooled vs summed per-coin clusters", "kind": "saved evidence",
                  "expected": "pooled <= summed, recomputed independently",
                  "observed": f"pooled {red.get('pooled_clusters')}, summed {red.get('summed_per_coin_clusters')}",
                  "ok": red.get("pooled_clusters") is not None
                  and red["pooled_clusters"] <= red["summed_per_coin_clusters"]
                  and red["pooled_clusters"] == rs.position_clusters(
                      [pp for d in prim["legs"]["base"]["candidate"].values()
                       for pp in d["execution"]["position_list"]])})

    stripped = copy.deepcopy(canonical)
    for pop in list(stripped["populations"]):
        if pop != rs.PRIMARY:
            stripped["populations"][pop] = {"garbage": True}
    v2 = rs.primary_verdict(proto, stripped["populations"][rs.PRIMARY])
    cases.append({"name": "descriptive outputs replaced: primary verdict unchanged", "kind": "saved evidence",
                  "expected": canonical["verdict"]["outcome"], "observed": v2["outcome"],
                  "ok": {k: v2[k] for k in ("outcome", "reason", "checks")}
                  == {k: canonical["verdict"][k] for k in ("outcome", "reason", "checks")}})

    def alter_forward(cp):
        mp = cp.manifest(proto, "forward_4h")
        d = rs.load_json(mp)
        d["datasets"] = d["datasets"][:1]
        rs.write_json(mp, d)
        relock_copy(cp)
    cpf = mutate_copy("descriptive_changed", alter_forward)
    outf = rs.run(cpf, os.path.join(work, "caches", "descriptive_changed", "cache.db"),
                  os.path.join(cpf.root, "results.json"), populations=[rs.PRIMARY, "forward_4h"],
                  workers=workers, git_check=False)
    same_primary = json.dumps(outf["populations"][rs.PRIMARY], sort_keys=True, default=str) == \
        json.dumps(canonical["populations"][rs.PRIMARY], sort_keys=True, default=str)
    cases.append({"name": "forward population roster changed: primary evidence and verdict unchanged",
                  "kind": "real runner on a disposable copy",
                  "expected": "identical primary legs and verdict; forward evidence incomplete",
                  "observed": f"primary identical {same_primary}; verdict {outf['verdict']['outcome']}; forward "
                              f"evidence complete {outf['descriptive_reduced']['forward_4h']['evidence_complete']}",
                  "ok": same_primary and outf["verdict"]["outcome"] == canonical["verdict"]["outcome"]
                  and not outf["descriptive_reduced"]["forward_4h"]["evidence_complete"]})

    if not skip_rerun:
        rerun = rs.run(paths, os.path.join(work, "caches", "rerun", "cache.db"), os.path.join(work, "rerun.json"),
                       workers=workers, git_check=True)
        a1 = json.dumps(comparable(rerun), sort_keys=True, default=str)
        a2 = json.dumps(comparable(canonical), sort_keys=True, default=str)
        cases.append({"name": "full reproduction with a separate fresh research cache",
                      "kind": "real runner on the canonical study",
                      "expected": "identical populations, legs, reductions and verdict (timing excluded)",
                      "observed": f"identical {a1 == a2}; rerun verdict {rerun['verdict']['outcome']}",
                      "ok": a1 == a2})
    else:
        cases.append({"name": "full reproduction with a separate fresh research cache", "kind": "skipped",
                      "expected": "-", "observed": "skipped by --skip-rerun", "ok": True})

    passed = all(c["ok"] for c in cases)
    out = {"study": proto["study"], "results_sha256": rs.sha256_file(paths.results), "source_commit": rs.git_head(),
           "all_ok": passed, "cases": cases, "runtime_seconds": round(time.time() - t0, 1)}
    rs.write_json(paths.verification, out)
    for c in cases:
        print(f"[{'ok' if c['ok'] else 'FAIL'}] {c['name']}: {c['observed'][:160]}")
    if not passed:
        raise SystemExit("verification failed")
    print(f"verification ok; wrote {paths.verification}")
