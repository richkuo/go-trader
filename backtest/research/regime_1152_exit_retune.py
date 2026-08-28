#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_THIS_DIR, "..", ".."))
_HARNESS = os.path.join(_REPO, "backtest", "exit_policy_ab.py")
_CONFIG_RATCHET = os.path.join(_THIS_DIR, "regime_1152_config_ratchet.json")
_CONFIG_B2 = os.path.join(_THIS_DIR, "regime_1152_config_b2.json")
_AGGREGATE_OUT = os.path.join(_THIS_DIR, "regime_1152_exit_retune.json")

OPENS = {
    "sq": {
        "open": "squeeze_momentum",
        "ratchet": {"config": _CONFIG_RATCHET, "strategy": "m6-1152-ratchet"},
        "b2": {"config": _CONFIG_B2, "strategy": "m6-1152-b2"},
    },
    "mr": {
        "open": "mean_reversion",
        "ratchet": {"config": os.path.join(_THIS_DIR, "regime_1152_config_ratchet_mr.json"),
                    "strategy": "m6-1152-ratchet-mr"},
        "b2": {"config": os.path.join(_THIS_DIR, "regime_1152_config_b2_mr.json"),
               "strategy": "m6-1152-b2-mr"},
    },
}


def _rung(mult: float, frac: float, trail: float) -> dict:
    return {"atr_multiple": mult, "close_fraction": frac, "trailing_mult_after": trail}


def _tp(mult: float, frac: float) -> dict:
    return {"atr_multiple": mult, "close_fraction": frac}


def _ratchet_candidate(labels: list[str], ladder: list[dict]) -> str:
    return json.dumps([{
        "name": "trailing_tp_ratchet_regime",
        "params": {"tp_tiers": {lab: ladder for lab in labels}},
    }])


def _b2_candidate(ladder: list[dict]) -> str:
    return json.dumps([{"name": "tiered_tp_atr", "params": {"tp_tiers": ladder}}])


_DIRECTIONAL_LABELS = [
    "ranging_directional", "ranging_directional_up", "ranging_directional_down",
]

INCUMBENTS = {
    "ratchet.ranging_quiet": [
        _rung(0.75, 0.40, 1.0), _rung(1.5, 0.80, 0.75), _rung(2.0, 1.00, 0.75)],
    "ratchet.ranging_volatile": [
        _rung(1.0, 0.40, 1.0), _rung(2.0, 0.80, 0.75), _rung(3.0, 1.00, 0.75)],
    "ratchet.ranging_directional": [
        _rung(1.0, 0.25, 1.0), _rung(2.0, 0.50, 1.0),
        _rung(3.0, 0.75, 0.8), _rung(4.5, 0.75, 0.6)],
    "b2.ranging (collapsed)": [_tp(0.5, 0.50), _tp(1.0, 1.00)],
}

RUNS: list[dict] = [
    {
        "key": "rq_volatile_geometry",
        "experiment": "ratchet", "substate": "ranging_quiet",
        "gate": ["ranging_quiet"],
        "candidate_close": _ratchet_candidate(["ranging_quiet"], [
            _rung(1.0, 0.40, 1.0), _rung(2.0, 0.80, 0.75), _rung(3.0, 1.00, 0.75)]),
        "hypothesis": "quiet's tighter triggers over-harvest; the volatile "
                      "geometry (wider rungs) does better even in quiet ranges",
    },
    {
        "key": "rq_lighter_early",
        "experiment": "ratchet", "substate": "ranging_quiet",
        "gate": ["ranging_quiet"],
        "candidate_close": _ratchet_candidate(["ranging_quiet"], [
            _rung(0.75, 0.25, 1.0), _rung(1.5, 0.50, 0.75), _rung(2.0, 1.00, 0.75)]),
        "hypothesis": "same triggers, lighter early scale-out (25/50/100) lets "
                      "quiet-range winners contribute more",
    },
    {
        "key": "rv_wider",
        "experiment": "ratchet", "substate": "ranging_volatile",
        "gate": ["ranging_volatile"],
        "candidate_close": _ratchet_candidate(["ranging_volatile"], [
            _rung(1.5, 0.40, 1.0), _rung(3.0, 0.80, 0.75), _rung(4.5, 1.00, 0.75)]),
        "hypothesis": "volatile ranges still scale out on noise at 1.0/2.0/3.0; "
                      "widen further (1.5/3.0/4.5)",
    },
    {
        "key": "rv_quiet_geometry",
        "experiment": "ratchet", "substate": "ranging_volatile",
        "gate": ["ranging_volatile"],
        "candidate_close": _ratchet_candidate(["ranging_volatile"], [
            _rung(0.75, 0.40, 1.0), _rung(1.5, 0.80, 0.75), _rung(2.0, 1.00, 0.75)]),
        "hypothesis": "inverse check — if the quiet geometry wins here too, the "
                      "#1059 volatile widening never earned its keep",
    },
    {
        "key": "rd_full_scaleout",
        "experiment": "ratchet", "substate": "ranging_directional",
        "gate": ["ranging_directional"],
        "candidate_close": _ratchet_candidate(_DIRECTIONAL_LABELS, [
            _rung(1.0, 0.40, 1.0), _rung(2.0, 0.80, 0.75), _rung(3.0, 1.00, 0.75)]),
        "hypothesis": "inverse check — does the directional let-ride runner "
                      "(25/50/75 + 4th rung) beat a plain full scale-out?",
    },
    {
        "key": "rd_lighter",
        "experiment": "ratchet", "substate": "ranging_directional",
        "gate": ["ranging_directional"],
        "candidate_close": _ratchet_candidate(_DIRECTIONAL_LABELS, [
            _rung(1.0, 0.15, 1.0), _rung(2.0, 0.35, 1.0),
            _rung(3.0, 0.60, 0.8), _rung(4.5, 0.60, 0.6)]),
        "hypothesis": "directional drift rewards an even lighter early "
                      "scale-out (15/35/60) — more runner",
    },
    {
        "key": "b2_rq_wider",
        "experiment": "b2", "substate": "ranging_quiet",
        "gate": ["ranging_quiet"],
        "candidate_close": _b2_candidate([_tp(0.75, 0.50), _tp(1.5, 1.00)]),
        "hypothesis": "quiet: is the collapsed fast exit (0.5/1.0) already "
                      "right, or does a wider 2-rung do better?",
    },
    {
        "key": "b2_rv_wider",
        "experiment": "b2", "substate": "ranging_volatile",
        "gate": ["ranging_volatile"],
        "candidate_close": _b2_candidate([_tp(0.75, 0.50), _tp(1.5, 1.00)]),
        "hypothesis": "volatile: widen the 2 rungs so wide-range noise does not "
                      "cash the whole position at 0.5 ATR",
    },
    {
        "key": "b2_rv_patient3",
        "experiment": "b2", "substate": "ranging_volatile",
        "gate": ["ranging_volatile"],
        "candidate_close": _b2_candidate([
            _tp(1.0, 0.40), _tp(2.0, 0.80), _tp(3.0, 1.00)]),
        "hypothesis": "volatile: a patient 3-rung ladder (ratchet-split "
                      "analogue) beats the collapsed fast exit",
    },
    {
        "key": "b2_rd_patient3",
        "experiment": "b2", "substate": "ranging_directional",
        "gate": ["ranging_directional"],
        "candidate_close": _b2_candidate([
            _tp(1.0, 0.40), _tp(2.0, 0.80), _tp(3.0, 1.00)]),
        "hypothesis": "directional: drift rewards patience — 3 rungs mirroring "
                      "the ratchet's directional split",
    },
    {
        "key": "b2_rd_wider2",
        "experiment": "b2", "substate": "ranging_directional",
        "gate": ["ranging_directional"],
        "candidate_close": _b2_candidate([_tp(1.0, 0.50), _tp(2.0, 1.00)]),
        "hypothesis": "directional: keep 2 rungs but double the distances",
    },
]


def _run_one(open_key: str, run: dict, out_dir: str, windows: str,
             datasets: str | None, resamples: int,
             intrabar_resolution: str = "ohlc_walk") -> dict:
    spec = OPENS[open_key][run["experiment"]]
    full_key = f"{open_key}.{run['key']}"
    json_path = os.path.join(out_dir, f"{full_key}.json")
    argv = [
        sys.executable, _HARNESS,
        "--baseline-config", spec["config"],
        "--strategy", spec["strategy"],
        "--registry", "futures",
        "--candidate-close", run["candidate_close"],
        "--windows", windows,
        "--bootstrap-resamples", str(resamples),
        "--intrabar-resolution", intrabar_resolution,
        "--json", json_path,
    ]
    for label in run["gate"]:
        argv += ["--allowed-regimes", label]
    if datasets:
        argv += ["--datasets", datasets]
    proc = subprocess.run(argv, capture_output=True, text=True)
    status = "ok" if proc.returncode == 0 and os.path.exists(json_path) else "failed"
    if status == "failed":
        sys.stderr.write(f"[{full_key}] FAILED rc={proc.returncode}\n"
                         f"{proc.stdout[-2000:]}\n{proc.stderr[-2000:]}\n")
    return {"key": full_key, "status": status, "json": json_path,
            "report": proc.stdout}


def _window_rollup(payload: dict) -> dict:
    out = {}
    for wname, results in (payload.get("results") or {}).items():
        deltas, votes_pos, votes_neg, sig_pos, sig_neg, n_paired = [], 0, 0, 0, 0, 0
        for d in results or []:
            t = d.get("per_regime")
            if not t or t["all"]["paired_delta"]["mean"] is None:
                continue
            mean = t["all"]["paired_delta"]["mean"]
            n = t["all"]["n"]
            p = t["all"]["paired_delta"]["signed_rank"]["p_value"]
            deltas.append((mean, n))
            n_paired += n
            if mean > 0:
                votes_pos += 1
                sig_pos += 1 if (p is not None and p < 0.05) else 0
            elif mean < 0:
                votes_neg += 1
                sig_neg += 1 if (p is not None and p < 0.05) else 0
        pooled = (round(sum(m * n for m, n in deltas) / sum(n for _, n in deltas), 4)
                  if deltas and sum(n for _, n in deltas) else None)
        out[wname] = {
            "paired_n": n_paired,
            "pooled_delta_net_pct_per_entry": pooled,
            "datasets_delta_pos": votes_pos,
            "datasets_delta_neg": votes_neg,
            "datasets_sig_pos_p05": sig_pos,
            "datasets_sig_neg_p05": sig_neg,
        }
    return out


def _verdict(rollup: dict) -> str:
    is_w, oos_w = rollup.get("is") or {}, rollup.get("oos") or {}
    pooled_is = is_w.get("pooled_delta_net_pct_per_entry")
    pooled_oos = oos_w.get("pooled_delta_net_pct_per_entry")
    if pooled_is is None or pooled_oos is None:
        return "inconclusive: insufficient paired entries"
    if pooled_is > 0 and pooled_oos > 0:
        sig = (is_w.get("datasets_sig_pos_p05", 0) + oos_w.get("datasets_sig_pos_p05", 0))
        contra = (is_w.get("datasets_sig_neg_p05", 0) + oos_w.get("datasets_sig_neg_p05", 0))
        if sig >= 1 and contra == 0:
            return "candidate_beats_incumbent"
        return "positive_but_not_significant"
    return "incumbent_stands"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--jobs", type=int, default=4)
    ap.add_argument("--out-dir", default=os.path.join(_THIS_DIR, "regime_1152_runs"))
    ap.add_argument("--only", default=None, help="comma list of run keys")
    ap.add_argument("--windows", default="is,oos")
    ap.add_argument("--datasets", default=None,
                    help="comma list SYMBOL:TIMEFRAME (default: audit six)")
    ap.add_argument("--bootstrap-resamples", type=int, default=10000)
    ap.add_argument("--intrabar-resolution", dest="intrabar_resolution",
                    choices=["ohlc_walk", "bar_close"], default="ohlc_walk",
                    help="Same-bar SL/TP race resolution (#1271), forwarded to "
                         "exit_policy_ab.py; bar_close reproduces pre-#1271 runs")
    args = ap.parse_args()

    matrix = [(ok, r) for ok in OPENS for r in RUNS]
    if args.only:
        keys = {k.strip() for k in args.only.split(",") if k.strip()}
        unknown = keys - {f"{ok}.{r['key']}" for ok, r in matrix}
        if unknown:
            raise SystemExit(f"unknown run keys: {sorted(unknown)}")
        matrix = [(ok, r) for ok, r in matrix if f"{ok}.{r['key']}" in keys]

    os.makedirs(args.out_dir, exist_ok=True)
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as ex:
        results = list(ex.map(
            lambda item: _run_one(item[0], item[1], args.out_dir, args.windows,
                                  args.datasets, args.bootstrap_resamples,
                                  args.intrabar_resolution),
            matrix))

    aggregate = {
        "issue": 1152,
        "harness": "backtest/exit_policy_ab.py (M6, #1066) — entry-locked "
                   "per-entry replay, incumbent-relative, regime-gated",
        "open_strategies": {k: v["open"] + " (futures registry, direction both)"
                            for k, v in OPENS.items()},
        "classifier": "composite p20 (regime.windows.medium)",
        "incumbents": INCUMBENTS,
        "runs": [],
    }
    failed = 0
    for (open_key, run), res in zip(matrix, results):
        entry = {
            "key": res["key"], "open": OPENS[open_key]["open"],
            "experiment": run["experiment"],
            "substate": run["substate"], "hypothesis": run["hypothesis"],
            "candidate_close": json.loads(run["candidate_close"]),
            "status": res["status"],
        }
        if res["status"] == "ok":
            with open(res["json"]) as fh:
                payload = json.load(fh)
            entry["windows"] = _window_rollup(payload)
            entry["verdict"] = _verdict(entry["windows"])
        else:
            failed += 1
            entry["verdict"] = "run_failed"
        aggregate["runs"].append(entry)
        print(f"{entry['key']:<22} {entry['verdict']}")

    with open(_AGGREGATE_OUT, "w") as fh:
        json.dump(aggregate, fh, indent=2)
    print(f"\nwrote {_AGGREGATE_OUT}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
