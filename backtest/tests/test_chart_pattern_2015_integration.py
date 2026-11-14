import json
import os
import subprocess
import sys

import pytest

_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
_DRIVER = os.path.join(_ROOT, "backtest", "candidates", "chart_pattern_2015", "run_study.py")

_SCRIPT = r"""
import copy, gzip, importlib.util, json, os, shutil, sys
root, tmp = sys.argv[1], sys.argv[2]
spec = importlib.util.spec_from_file_location("chart_pattern_2015_driver", sys.argv[3])
rs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rs)
study = os.path.join(tmp, "study")
os.makedirs(study)
for name in ("protocol.json", "research_config.json"):
    shutil.copyfile(os.path.join(os.path.dirname(sys.argv[3]), name), os.path.join(study, name))
rs.set_study_root(study)
os.environ[rs.CACHE_ENV] = os.path.join(tmp, "cache.db")
import numpy as np
import pandas as pd
import storage
storage.init_db(os.environ[rs.CACHE_ENV])
rs.block_network()
rs.install_equity_capture()
out = {}
proto = rs.protocol()
params = proto["signal_params"]
L = proto["frame"]["length_bars"]

report = rs.build_arms(check=False)
rs.build_arms(check=True)
out["arms"] = {"contracts": {k: {"direction": v["resolved"]["direction"],
                                 "close": [c["name"] for c in v["resolved"]["close"] or []],
                                 "identical": v["per_dataset_identical"]}
                             for k, v in report["contracts"].items()},
               "refused": sorted(report["refused"]), "frame": report["frame_length_bars"]}
arm = rs.contract_arm_path("ratchet_long", 4)
doc = json.load(open(arm))
doc["strategies"][0]["direction"] = "short"
open(arm, "w").write(json.dumps(doc))
try:
    rs.build_arms(check=True)
    out["tampered_arm_refused"] = False
except SystemExit:
    out["tampered_arm_refused"] = True
rs.build_arms(check=False)

step = 3_600_000
start = int(pd.Timestamp("2024-01-01").timestamp() * 1000)
n = 1500
rng = np.random.default_rng(2015)
drift = 0.004 * np.sin(np.arange(n) / 37.0) + 0.002 * np.sin(np.arange(n) / 151.0)
close = 100 * np.exp(np.cumsum(rng.normal(0, 0.006, n) + drift))
ts = start + step * np.arange(n)
opens = np.r_[close[0], close[:-1]]
wick = np.abs(rng.normal(0, 0.004, n))
candles = pd.DataFrame({"timestamp": ts, "open": opens, "high": np.maximum(opens, close) * (1 + wick),
                        "low": np.minimum(opens, close) * (1 - wick), "close": close,
                        "volume": rng.uniform(50, 300, n) * (1 + 3 * (rng.uniform(0, 1, n) > 0.93))})
src_path = os.path.join(tmp, "BTC_1h_candles.csv.gz")
with gzip.open(src_path, "wt") as fh:
    fh.write("timestamp,open,high,low,close,volume\n")
    for r in candles.itertuples():
        fh.write(f"{int(r.timestamp)},{r.open!r},{r.high!r},{r.low!r},{r.close!r},{r.volume!r}\n")
src = rs.candle_frame(src_path)
storage.store_ohlcv(src.reset_index(drop=True)[["timestamp", "open", "high", "low", "close", "volume"]],
                    "binanceus", "BTC/USDT", "1h", db_path=os.environ[rs.CACHE_ENV])
rows = []
for p in range(L - 1, n):
    d = rs.decide(src.iloc[p - L + 1:p + 1], params)
    rows.append([int(src["timestamp"].iat[p]), float(src["close"].iat[p]), d["raw"]]
                + [d[f"t{f}"] for f in rs.FINITE_FACTORS])
table_doc = {"fingerprint": "fixture", "source": src_path, "source_sha256": rs.sha256_file(src_path), "length": L,
             "params": params, "columns": rs.table_columns(), "rows": rows}
table_file = os.path.join(tmp, "table.json.gz")
rs.write_gz_json(table_file, table_doc)
table = rs.SignalTable(table_doc)
out["table"] = {"rows": len(rows), "raw_nonzero": sum(1 for r in rows if r[2] != 0),
                "t4_nonzero": sum(1 for r in rows if r[5] != 0),
                "t5_nonzero": sum(1 for r in rows if r[6] != 0), "t6_nonzero": sum(1 for r in rows if r[7] != 0)}

replay_proto = copy.deepcopy(proto)
replay_proto["replay_check"]["sample_per_dataset"] = 16
sha, _, checks = rs._replay_source((replay_proto, table_doc["source_sha256"],
                                    {"path": os.path.relpath(src_path, rs.REPO), "start_ms": 0, "kind": "proxy",
                                     "dataset": "BTC 1h"}, table_file))
out["replay"] = {k: {"compared": len(v), "ok": sum(1 for r in v if r["ok"])} for k, v in checks.items()}

late = pd.Timestamp(int(ts[1200]), unit="ms")
bad = src.copy()
bad.loc[bad.index > late, "close"] = bad.loc[bad.index > late, "close"] * 3.0
p_late = int(src.index.get_loc(late))
out["future_invariance"] = rs.decide(src.iloc[p_late - L + 1:p_late + 1], params) == \
    rs.decide(bad.iloc[p_late - L + 1:p_late + 1], params)

import eval_windows as ew
from registry_loader import load_registry
reg = load_registry("futures")
win = ("2024-02-01", "2024-03-03")
hours = list(range(int(pd.Timestamp("2023-12-01").timestamp() * 1000), int(ts[-1]) + step, step))
records = [{"time": h + 48, "rate": 1.25e-05} for h in hours]

def run(contract, factor):
    cand = rs.arm_candidate(proto, contract, factor, "BTC", "1h")
    ff = rs.FiniteFrameRegistry(reg, table, rs.to_ms(win[0]))
    try:
        leg = ew.run_candidate_leg(ff, cand, "BTC/USDT", "1h", win, proto["capital_usd"], keep_trades=True,
                                   funding_mode="charge", warmup_bars=proto["frame"]["warmup_bars"])
    except Exception as exc:
        return {"refused": type(exc).__name__}
    return {"leg": leg, "calls": ff.calls, "equity": rs.daily_equity(rs._EQUITY.get("last"))}

unfunded = run("ref_long", 4)
out["unfunded"] = {"refused": unfunded.get("refused"),
                   "funding_incomplete": bool((unfunded.get("leg") or {}).get("funding_incomplete"))}
storage.store_funding_fetch("hyperliquid", "BTC", records, [], observed_through_ms=None,
                            db_path=os.environ[rs.CACHE_ENV])
storage.store_funding_coverage("hyperliquid", "BTC", hours[0], hours[-1] + step, db_path=os.environ[rs.CACHE_ENV])
legs = {}
for contract in ("ref_long", "ratchet_both"):
    for f in (0, 4):
        legs[(contract, f)] = run(contract, f)
fills = []
idx = {t: i for i, t in enumerate(src.index)}
for (contract, f), r in legs.items():
    leg = r["leg"]
    for p in leg["execution"]["position_list"]:
        i = idx[pd.Timestamp(p["entry_date"])]
        prev = table.signal(int(src["timestamp"].iat[i - 1]), f, "veto")[0]
        fills.append({"contract": contract, "f": f, "side": p["side"], "prev_signal": prev,
                      "closes": p["closes"]})
both = legs[("ratchet_both", 4)]["leg"]["execution"]
out["legs"] = {f"{c}|f{f}": {"positions": len(r["leg"]["execution"]["position_list"]),
                             "records": r["leg"]["execution"]["trade_records"],
                             "closes": sum(p["closes"] for p in r["leg"]["execution"]["position_list"]),
                             "calls": r["calls"], "funding_incomplete": bool(r["leg"].get("funding_incomplete")),
                             "equity_days": len(r["equity"])}
               for (c, f), r in legs.items()}
out["fills"] = fills
out["both_sides"] = sorted({p["side"] for p in both["position_list"]})
try:
    ew.validate_candidate({"name": "chart_pattern", "type": "perps", "direction": "both"})
    out["both_open_as_close_refused"] = False
except ValueError:
    out["both_open_as_close_refused"] = True

def row(section, contract, group, window, arm, coin, leg_obj, equity, positions):
    return ({"section": section, "contract": contract, "memo": None, "group": group, "kind": "proxy", "coin": coin,
             "interval": "1h", "window": window, "arm": arm, "leg": rs.compact_leg(leg_obj)},
            {"positions": positions, "daily_equity": equity})

legs_out, samples = [], {}
for w in proto["perps"]["judged_windows"]:
    for (c, f), r in legs.items():
        if c != "ref_long":
            continue
        for coin in ("BNB", "BTC", "ETH"):
            rr, ss = row("perps", c, "proxy_1h", w, f"f{f}", coin, r["leg"], r["equity"], [])
            legs_out.append(rr)
            samples[rs.leg_key(rr)] = ss
results = {"legs": legs_out, "samples_sha256": None, "source_commit": "fixture", "network_calls": 0}
rs.write_gz_json(rs.SAMPLES_PATH, samples)
results["samples_sha256"] = rs.sha256_file(rs.SAMPLES_PATH)
rs.write_json(rs.RESULTS_PATH, results)
sel = {"selection": {f"{c}|{g}": {"selected_additional": None} for c in rs.contracts(proto)
                     for g in ("proxy_30m", "proxy_1h")}}
val_ok = {"replay": {k: {"passed": True} for k in ("decomposition", "signal_check_path", "future_invariance",
                                                    "htf_visibility", "next_bar_fill", "full_frame_causality")}}
val_bad = copy.deepcopy(val_ok)
val_bad["replay"]["signal_check_path"]["passed"] = False
proto_small = copy.deepcopy(proto)
for memo in proto_small["audit"]["memos"].values():
    memo["windows"] = []
res_doc, samp = rs.load_results()
v_ok = rs.verdict_from(proto_small, res_doc, samp, sel, val_ok, {})
v_bad = rs.verdict_from(proto_small, res_doc, samp, sel, val_bad, {})
m = v_ok["members"]["ref_long|proxy_1h|f4"]
out["verdict"] = {"ok_outcome": m["outcome"], "ok_blocks": m["paired"]["blocks"], "ok_positions": m["positions"],
                  "ok_reasons": len(m["reasons"]),
                  "empty_group": v_ok["members"]["ratchet_short|proxy_30m|f4"]["outcome"],
                  "empty_group_usable": len(v_ok["members"]["ratchet_short|proxy_30m|f4"]["usable_members"]),
                  "bad_replay": sorted({x["outcome"] for x in v_bad["members"].values()}),
                  "family": len(v_ok["members"]), "suggested": len(v_ok["suggested_lines"])}
inc = copy.deepcopy(res_doc)
for r in inc["legs"]:
    r["leg"]["funding_incomplete"] = True
v_inc = rs.verdict_from(proto_small, inc, samp, sel, val_ok, {})
out["verdict"]["funding_incomplete_outcome"] = v_inc["members"]["ref_long|proxy_1h|f4"]["outcome"]

rs.write_json(rs.VERDICT_PATH, v_ok)
rs.write_json(rs.SELECTION_PATH, {"selection": {}, "rule": "fixture"})
spec2 = importlib.util.spec_from_file_location("render2015", os.path.join(os.path.dirname(sys.argv[3]),
                                                                         "render_report.py"))
render = importlib.util.module_from_spec(spec2)
spec2.loader.exec_module(render)
out["render_identical"] = render.render(study) == render.render(study)

with open(rs.SAMPLES_PATH, "ab") as fh:
    fh.write(b"0")
try:
    rs.load_results()
    out["tampered_samples_refused"] = False
except SystemExit:
    out["tampered_samples_refused"] = True

os.remove(rs.SELECTION_PATH)
try:
    rs.require_selection()
    out["missing_selection_refused"] = False
except SystemExit:
    out["missing_selection_refused"] = True

fake_input = os.path.join(tmp, "input.csv.gz")
shutil.copyfile(src_path, fake_input)
rs.write_json(rs.STUDY_MANIFEST_PATH, {"sources": {}, "manifests": {},
                                       "files": {"x": {"path": os.path.relpath(fake_input, rs.REPO),
                                                       "sha256": rs.sha256_file(fake_input)}}})
rs.verify_inputs()
with open(fake_input, "ab") as fh:
    fh.write(b"0")
try:
    rs.verify_inputs()
    out["changed_input_refused"] = False
except SystemExit:
    out["changed_input_refused"] = True

ck = os.path.join(tmp, "ck.json")
rs.write_json(ck, {"fingerprint": "a", "leg": {}})
out["checkpoint"] = {"same": rs.checkpoint_fresh(ck, "a") is not None, "other": rs.checkpoint_fresh(ck, "b") is None}
held = {"protocol_sha256": rs.protocol_sha256()}
pdoc = json.load(open(rs.PROTOCOL_PATH))
pdoc["capital_usd"] = 2000.0
open(rs.PROTOCOL_PATH, "w").write(json.dumps(pdoc))
out["protocol_change_detected"] = held["protocol_sha256"] != rs.protocol_sha256()
out["network_calls"] = len(rs.NETWORK_CALLS)
print("RESULT " + json.dumps(out, sort_keys=True, default=str))
"""


@pytest.fixture(scope="module")
def study(tmp_path_factory):
    tmp = str(tmp_path_factory.mktemp("chart_pattern_2015"))
    env = dict(os.environ, OMP_NUM_THREADS="1", OPENBLAS_NUM_THREADS="1")
    env.pop("HYPERLIQUID_SECRET_KEY", None)
    proc = subprocess.run([sys.executable, "-c", _SCRIPT, _ROOT, tmp, _DRIVER], capture_output=True, text=True,
                          env=env, timeout=1800)
    line = next((ln for ln in proc.stdout.splitlines() if ln.startswith("RESULT ")), None)
    assert proc.returncode == 0 and line, proc.stdout[-3000:] + proc.stderr[-6000:]
    return json.loads(line[len("RESULT "):])


def test_arm_construction_and_refusals(study):
    arms = study["arms"]
    assert arms["frame"] == 200
    assert arms["contracts"]["ratchet_both"] == {"direction": "both", "close": ["trailing_tp_ratchet_regime"],
                                                 "identical": True}
    assert arms["contracts"]["ref_long"]["close"] == [] and arms["contracts"]["ref_short"]["direction"] == "short"
    assert arms["refused"] == ["ref_both"]
    assert study["tampered_arm_refused"] is True
    assert study["both_open_as_close_refused"] is True


def test_finite_frame_signal_replay_and_htf_visibility(study):
    assert study["table"]["raw_nonzero"] > 0
    assert study["table"]["t5_nonzero"] == 0 and study["table"]["t6_nonzero"] == 0
    assert study["table"]["t4_nonzero"] > 0
    for name, r in study["replay"].items():
        assert r["compared"] > 0 and r["ok"] == r["compared"], (name, r)
    assert study["future_invariance"] is True


def test_real_backtester_legs_fill_next_bar_and_aggregate_closes(study):
    for key, leg in study["legs"].items():
        assert leg["calls"] == 1, key
        assert leg["closes"] == leg["records"], key
        assert leg["funding_incomplete"] is False, key
        assert leg["equity_days"] > 25, key
    assert study["fills"]
    for f in study["fills"]:
        assert f["prev_signal"] == (1 if f["side"] == "long" else -1), f
    assert study["legs"]["ratchet_both|f4"]["positions"] > 0
    assert set(study["both_sides"]) <= {"long", "short"}


def test_funding_refusal(study):
    u = study["unfunded"]
    assert u["refused"] is not None or u["funding_incomplete"] is True
    assert study["verdict"]["funding_incomplete_outcome"] == "inconclusive"


def test_verdict_rules_and_report_regeneration(study):
    v = study["verdict"]
    assert v["family"] == 10
    assert v["ok_outcome"] in ("inconclusive", "negative")
    assert v["empty_group"] == "inconclusive" and v["empty_group_usable"] == 0
    assert v["bad_replay"] == ["inconclusive"]
    assert v["suggested"] == 0
    assert study["render_identical"] is True


def test_changed_inputs_and_incompatible_artifacts_refused(study):
    assert study["tampered_samples_refused"] is True
    assert study["missing_selection_refused"] is True
    assert study["changed_input_refused"] is True
    assert study["checkpoint"] == {"same": True, "other": True}
    assert study["protocol_change_detected"] is True
