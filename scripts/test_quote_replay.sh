#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/quote-replay-harness.XXXXXX")" && pwd -P)
trap 'rm -rf "$OUT"' EXIT
cd "$ROOT"

GO_TRADER_QUOTE_REPLAY_HARNESS_OUT="$OUT/rec" go -C "$ROOT/scheduler" test -run '^TestQuoteReplayRecordingHarness$' -count=1 .
GO_TRADER_QUOTE_REPLAY_HARNESS_OUT="$OUT/depth" GO_TRADER_QUOTE_REPLAY_DEPTH_ONLY=1 go -C "$ROOT/scheduler" test -run '^TestQuoteReplayRecordingHarness$' -count=1 .
GO_TRADER_QUOTE_REPLAY_HARNESS_OUT="$OUT/gap" GO_TRADER_QUOTE_REPLAY_TRADE_GAP=1 go -C "$ROOT/scheduler" test -run '^TestQuoteReplayRecordingHarness$' -count=1 .
go -C "$ROOT/scheduler" build -o "$OUT/go-trader" .
for f in rec/run.manifest.json depth/run.manifest.json gap/run.manifest.json rec-harness.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

uv run --no-sync python backtest/quote_replay.py discover --binary "$OUT/go-trader" > "$OUT/discover.json"

uv run --no-sync python - "$OUT" <<'PY'
import json, os, subprocess, sys
sys.path.insert(0, "backtest")
import quote_replay as q
out = sys.argv[1]
proto = json.load(open("backtest/candidates/inventory_market_making_1636/protocol-lock.json"))
h = json.load(open(f"{out}/rec-harness.json"))
base = h["base_ms"]
recs = [{"time": t, "fundingRate": "0.0000125"} for t in range(base - 3_600_000, h["end_ms"] + 3_600_000, 3_600_000)]
json.dump({"coins": {"BTC": recs}}, open(f"{out}/funding.json", "w"))
funding = q.funding_records(f"{out}/funding.json", "BTC")
window = {"first_deadline_ms": base + 300_000, "last_deadline_ms": base + 715_000, "label": "harness"}


def run(plan, name, *extra):
    path = f"{out}/{name}.plan.json"
    json.dump(plan, open(path, "w"))
    code, res, err = q.run_plan(f"{out}/go-trader", path, extra)
    return code, res, err


rec = q.load_recording(f"{out}/rec", "BTC")
if rec["refusals"] or not rec["books"] or not rec["trades"] or not rec["trade_coverage"]:
    raise SystemExit(f"FAIL: the recorder output did not load cleanly: {rec['refusals']}")
plan = q.build_plan(rec, proto, window, {}, "harness", f"{out}/state-a", funding)
code, res, err = run(plan, "a")
if code != 0 or res.get("status") != "complete" or not res.get("scored") or not res.get("fills"):
    raise SystemExit(f"FAIL: the recorded replay did not complete scored with fills: {code} {res.get('status')} {res.get('unscored_reasons')} {err[-800:]}")
rep = q.verify(plan, res)
if not rep["ok"]:
    raise SystemExit(f"FAIL: the independent ledger disagrees with the Go settlement: {rep['problems'][:5]}")
print(f"ok: recorded replay {len(res['decisions'])} decisions, {len(res['fills'])} fills, {len(rep['round_trips'])} round trips, independent ledger equal")

part = q.build_plan(rec, proto, window, {}, "harness", f"{out}/state-b", funding)
code, first, err = run(part, "b1", "--max-deadlines", "33")
if code != 0 or first.get("status") != "stopped":
    raise SystemExit(f"FAIL: the bounded slice did not stop resumably: {code} {first.get('status')} {err[-400:]}")
part["mode"] = "resume"
code, resumed, err = run(part, "b2")
if code != 0 or resumed.get("status") != "complete" or not resumed.get("scored"):
    raise SystemExit(f"FAIL: the resumed run did not complete scored: {code} {resumed.get('status')} {err[-400:]}")


def comparable(r):
    decs = []
    for d in r["decisions"]:
        d = json.loads(json.dumps(d))
        d["fingerprints"].pop("config", None)
        d["verdict"].pop("config_fingerprint", None)
        decs.append(d)
    return json.dumps([decs, r["fills"], r["settlements"], r["venue_events"], {k: v for k, v in r["book"].items() if k != "position_id"}], sort_keys=True)


if comparable(res) != comparable(resumed):
    raise SystemExit("FAIL: the resumed run's decisions, fills, settlements, venue events or book differ from the uninterrupted run")
if not q.verify(part, resumed)["ok"]:
    raise SystemExit("FAIL: the resumed run's ledger disagrees with the independent ledger")
print("ok: a run resumed at a completed checkpoint reproduces the uninterrupted records")

depth = q.load_recording(f"{out}/depth", "BTC")
dplan = q.build_plan(depth, proto, window, {}, "depth", f"{out}/state-c", funding)
if dplan["scoring"]["scored"] or not any("depth only" in l for l in dplan["scoring"]["limits"]):
    raise SystemExit(f"FAIL: a depth-only recording was planned as scored: {dplan['scoring']}")
code, dres, err = run(dplan, "c")
if code != 0 or dres.get("scored") or dres.get("fills"):
    raise SystemExit(f"FAIL: a depth-only replay scored or filled: {code} {dres.get('scored')} {len(dres.get('fills') or [])}")
print("ok: depth-only input is refused for scoring and supplies no fills")

gap = q.load_recording(f"{out}/gap", "BTC")
gh = json.load(open(f"{out}/gap-harness.json"))
gwin = {"first_deadline_ms": gh["base_ms"] + 300_000, "last_deadline_ms": gh["base_ms"] + 715_000}
gplan = q.build_plan(gap, proto, gwin, {}, "gap", f"{out}/state-d", funding)
code, gres, err = run(gplan, "d")
if code != 0 or gres.get("scored"):
    raise SystemExit(f"FAIL: a raw-trade gap inside the window did not leave the replay unscored: {code} {gres.get('status')}")
print(f"ok: a raw-trade coverage gap leaves the replay unscored ({gres.get('status')}: {gres.get('stopped') or gres.get('unscored_reasons')[:1]})")

bad = json.loads(json.dumps(plan))
bad["mode"] = "resume"
bad["trades"][7]["sz"] = "9.9"
code, refused, err = run(bad, "tamper")
if code != 3 or (refused.get("refusal") or {}).get("reason") != "resume_refused":
    raise SystemExit(f"FAIL: a resume over altered trade evidence was not refused: {code} {refused.get('refusal')}")
print("ok: a resume over altered evidence is refused by its data fingerprint")

from inventory_market_making import validate_run_config
cases = {
    "unknown parameter": ({"edge_boost": 1}, {}),
    "boolean parameter": ({"risk_aversion": True}, {}),
    "out of range": ({"risk_aversion": 4.5}, {}),
    "integer violation": ({"volatility_window_seconds": 60.5}, {}),
    "minimum above maximum": ({"min_spread_bps": 50, "max_spread_bps": 20}, {}),
    "notional above a quarter of the cap": ({}, {"max_inventory_usd": 300}),
    "target above half the cap": ({"target_inventory_usd": 600}, {}),
    "lifetime below the cycle": ({"quote_lifetime_seconds": 5}, {"interval_seconds": 10}),
    "cycle out of range": ({}, {"interval_seconds": 61}),
    "stop at zero": ({}, {"stop_loss_pct": 0}),
}
for name, (pover, cover) in sorted(cases.items()):
    full = dict(plan["params"])
    full.update(pover)
    cfg = dict(plan["config"])
    cfg.update(cover)
    try:
        validate_run_config(full, cfg)
    except ValueError:
        pass
    else:
        raise SystemExit(f"FAIL: the Python run validator accepted {name}")
    bad = json.loads(json.dumps(plan))
    bad["params"], bad["config"] = full, cfg
    bad["run"]["state_dir"] = f"{out}/state-param-{name.replace(' ', '-')}"
    code, refused, err = run(bad, "param-" + name.replace(" ", "-"))
    if code != 3 or os.path.exists(bad["run"]["state_dir"]):
        raise SystemExit(f"FAIL: the Go runner did not refuse {name} before state: {code} {refused.get('refusal')}")
print(f"ok: the Python and Go run validators refuse the same {len(cases)} parameter and configuration cases before any state")

env = dict(q.clean_env(), HYPERLIQUID_SECRET_KEY="0xnotarealkey")
p = subprocess.run([f"{out}/go-trader", "quote-replay", "--spec", f"{out}/a.plan.json"], capture_output=True, text=True, env=env)
if p.returncode != 3 or "credentials_refused" not in p.stdout or "notarealkey" in p.stdout + p.stderr:
    raise SystemExit(f"FAIL: venue credentials were not refused before state: {p.returncode} {p.stdout[-300:]}")
print("ok: venue credentials are refused and never printed")
PY

cp -R "$OUT/rec" "$OUT/rec-clean"
SEG=$(ls "$OUT"/rec/segment-*.jsonl | head -1)
printf '{"k":"book"}\n' >> "$SEG"
if uv run --no-sync python backtest/quote_replay.py plan --recording "$OUT/rec" --coin BTC \
    --protocol backtest/candidates/inventory_market_making_1636/protocol-lock.json --funding "$OUT/funding.json" \
    --run-id tamper --state-dir "$OUT/state-e" --out "$OUT/tamper.json" > /dev/null 2> "$OUT/tamper.err"; then
    echo "FAIL: the quote planner accepted a segment whose bytes no longer match its manifest hash" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.err" || { echo "FAIL: the tamper refusal did not name the hash mismatch" >&2; cat "$OUT/tamper.err" >&2; exit 1; }
echo "ok: a tampered recording segment is refused before planning"

uv run --no-sync python - "$OUT" <<'PY'
import json, os, sys
sys.path.insert(0, "backtest")
import archive_cache
import quote_replay as q
out = sys.argv[1]
root = os.path.join(out, "archive")
key = "market_data/20261001/0/l2Book/BTC.lz4"
manifest = {"files": [{"key": key, "size": 5, "sha256": "0" * 64, "status": "present"}]}
os.makedirs(os.path.join(root, "market_data/20261001/0/l2Book"), exist_ok=True)
json.dump(manifest, open(os.path.join(out, "archive-manifest.json"), "w"))
for label, setup, want in (("missing", lambda: None, "is missing"),
                           ("corrupt", lambda: open(os.path.join(root, key), "wb").write(b"abcde"), "does not match")):
    setup()
    try:
        q.load_archive_books(root, os.path.join(out, "archive-manifest.json"), "BTC", ["20261001"], os.path.join(out, "archive-work-" + label))
    except archive_cache.ArchiveError as exc:
        if want not in str(exc):
            raise SystemExit(f"FAIL: the {label} archive object was refused for the wrong reason: {exc}")
    else:
        raise SystemExit(f"FAIL: a {label} archive object was accepted")
print("ok: missing and corrupt archive cache objects are refused without download")
PY

uv run --no-sync python - "$OUT" <<'PY'
import hashlib, json, subprocess, sys
out = sys.argv[1]
p = json.load(open("backtest/candidates/inventory_market_making_1636/protocol-lock.json"))
p["base_params"].update({"volatility_window_seconds": 60, "quote_lifetime_seconds": 20})
p["eligibility"].update({"min_window_minutes": 2, "max_deadlines_per_run": 24})
p["grid"] = {"risk_aversion": [0.5, 1], "min_spread_bps": [6, 10]}
p["baseline"]["min_spread_bps_grid"] = [6, 10]
p["data_manifest"] = f"{out}/smoke-manifest.json"
json.dump(p, open(f"{out}/smoke-protocol.json", "w"))
sha = lambda path: hashlib.sha256(open(path, "rb").read()).hexdigest()
json.dump({"recordings": [{"path": f"{out}/rec-clean", "coin": "BTC", "manifest_sha256": sha(f"{out}/rec-clean/run.manifest.json")}],
           "funding": {"path": f"{out}/funding.json", "sha256": sha(f"{out}/funding.json")}}, open(f"{out}/smoke-manifest.json", "w"))
proc = subprocess.run(["uv", "run", "--no-sync", "python", "backtest/candidates/inventory_market_making_1636/run_study.py", "--protocol",
                       f"{out}/smoke-protocol.json", "--binary", f"{out}/go-trader", "--out-dir", f"{out}/study"], capture_output=True, text=True)
if proc.returncode != 0:
    raise SystemExit(f"FAIL: the study driver failed: {proc.stderr[-800:]}")
r = json.load(open(f"{out}/study/results.json"))
held = r["held_out"]["candidate"] if r["held_out"] else None
if (r["verdict"] != "INCONCLUSIVE" or len(r["variants"]) != 4 or len(r["baselines"]) != 2 or held is None
        or not all(v["train"]["scored"] for v in r["variants"]) or not held["scored"] or not r["held_out"]["baseline"]["scored"]
        or not r["stress"]["scored"] or not any("round trips <" in x for x in r["reasons"])):
    raise SystemExit(f"FAIL: the study smoke did not score every split and stay INCONCLUSIVE on sample size and evidence: {r['reasons'][:8]}")
if "## Reproduce" not in open(f"{out}/study/REPORT.md").read():
    raise SystemExit("FAIL: the study report has no reproduction section")
print(f"ok: the study driver scores {len(r['variants'])} variants and the fixed-spread baseline on train, then validation, held-out and stress; verdict {r['verdict']}")
PY

for platform in spot futures; do
    uv run --no-sync python "shared_strategies/open/$platform/strategies.py" --list-json > "$OUT/$platform.json"
    if ! cmp -s "$OUT/$platform.json" "backtest/candidates/inventory_market_making_1636/discovery_before_$platform.json"; then
        echo "FAIL: ordinary $platform discovery changed" >&2
        exit 1
    fi
done
echo "ok: ordinary spot and futures discovery are unchanged"

echo "PASS: quote replay harness (recorder output replays through the real quote coordinator with an equal independent ledger; resume reproduces records; the frozen study scores every split; depth-only, raw-trade-gap, tampered, altered, credentialed and archive-cache inputs refused or unscored; ordinary discovery unchanged)"
