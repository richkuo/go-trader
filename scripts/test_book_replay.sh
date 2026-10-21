#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(mktemp -d "${TMPDIR:-/tmp}/book-harness.XXXXXX")
trap 'rm -rf "$OUT"' EXIT

GO_TRADER_L2BOOK_HARNESS_OUT="$OUT/pipe" GO_TRADER_L2BOOK_MIXED_OUT="$OUT/mixed" \
    go -C "$ROOT/scheduler" test -run '^(TestL2BookObservationPipeline|TestL2BookMixedRecordingKeepsEveryImporter)$' -count=1 .

for f in pipe/harness.json pipe/payload_1.json pipe/payload_2.json pipe/payload_eth.json pipe/payload_batch.json pipe/payload_wide.json \
    pipe/recording/run.manifest.json mixed/run.manifest.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

cd "$ROOT"
uv run --no-sync python - "$OUT/pipe/recording" <<'PY'
import hashlib, json, re, sys
sys.path.insert(0, "backtest")
import book_replay
doc = "docs/research/1635-l2-book-contract.md"
path = "docs/research/1635-l2-book-contract-v1.json"
with open(path, "rb") as fh:
    blob = fh.read()
digest = hashlib.sha256(blob).hexdigest()
with open(doc) as fh:
    pinned = re.findall(r"sha256 `([0-9a-f]{64})`", fh.read())
if pinned != [digest]:
    raise SystemExit(f"FAIL: {doc} pins {pinned}, the contract file hashes to {digest}")
contract = json.loads(blob)
with open(sys.argv[1] + "/run.manifest.json") as fh:
    recorded = json.load(fh)["book_contract"]
if contract != book_replay.CONTRACT or recorded != contract:
    raise SystemExit("FAIL: the pinned contract, the replay contract and the recorder contract differ")
PY

uv run --no-sync python backtest/observation_replay.py import-books --run-dir "$OUT/pipe/recording" --out-dir "$OUT" > "$OUT/import.json"
uv run --no-sync python backtest/observation_replay.py harness-books --dir "$OUT/pipe"
uv run --no-sync python - "$OUT" <<'PY'
import json, sys
sys.path.insert(0, "backtest")
import book_replay
with open(sys.argv[1] + "/import.json") as fh:
    out = json.load(fh)
if not out["economic_evidence"] or out["mismatch_count"] or out["markers_compared"] < 20:
    raise SystemExit(f"FAIL: the owner recording did not replay cleanly: {out['refusals']}")
for coin, entry in out["series"].items():
    series = book_replay.read_book_series(entry["file"])
    if series["series_sha256"] != entry["series_sha256"]:
        raise SystemExit(f"FAIL: {coin} series hash changed through its written file")
PY

REC="$OUT/pipe/recording"
cp -R "$REC" "$OUT/pristine"
reset_recording() {
    rm -rf "$REC"
    cp -R "$OUT/pristine" "$REC"
}

SEG=$(ls "$REC"/segment-*.jsonl | head -1)
printf '{"k":"book"}\n' >> "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-books --run-dir "$REC" > /dev/null 2> "$OUT/tamper.err"; then
    echo "FAIL: the book importer accepted a segment whose bytes no longer match its manifest hash" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.err" || { echo "FAIL: tamper refusal did not name the hash mismatch" >&2; cat "$OUT/tamper.err" >&2; exit 1; }
reset_recording

head -c 300 "$OUT/pristine/$(basename "$SEG")" > "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-books --run-dir "$REC" > /dev/null 2>&1; then
    echo "FAIL: the book importer accepted a truncated segment" >&2
    exit 1
fi
reset_recording

uv run --no-sync python - "$REC" "$OUT/pristine" "$OUT/mixed" <<'PY'
import copy, hashlib, json, os, shutil, sys
sys.path.insert(0, "backtest")
import book_replay
import observation_replay
import trade_flow_replay
import volume_profile_replay

run_dir, pristine, mixed = sys.argv[1], sys.argv[2], sys.argv[3]


def reset():
    shutil.rmtree(run_dir)
    shutil.copytree(pristine, run_dir)


def manifest(d=run_dir):
    with open(os.path.join(d, "run.manifest.json")) as fh:
        return json.load(fh)


def write_manifest(run, d=run_dir):
    with open(os.path.join(d, "run.manifest.json"), "w") as fh:
        json.dump(run, fh)


def rewrite(edit, counts=None):
    run = manifest()
    for idx, seg in enumerate(run["segments"]):
        path = os.path.join(run_dir, seg["file"])
        with open(path) as fh:
            lines = fh.read().split("\n")[:-1]
        body = [json.loads(l) for l in lines[1:-1]]
        new = edit(seg["file"], body)
        if new is None:
            continue
        end = json.loads(lines[-1])
        end["records"] = len(new)
        out = [lines[0]] + [json.dumps(r, separators=(",", ":"), sort_keys=True) for r in new] + [json.dumps(end, separators=(",", ":"), sort_keys=True)]
        blob = ("\n".join(out) + "\n").encode("utf-8")
        with open(path, "wb") as fh:
            fh.write(blob)
        seg["sha256"] = hashlib.sha256(blob).hexdigest()
        seg["bytes"] = len(blob)
        seg["records"] = len(new)
        if counts:
            counts(seg, new)
        with open(path + ".manifest.json", "w") as fh:
            json.dump(seg, fh)
        run["segments"][idx] = seg
        write_manifest(run)
        return
    raise SystemExit("FAIL: no segment held the record to alter")


def refused(label, needle):
    try:
        loaded = book_replay.load_book_recording(run_dir)
    except book_replay.BookRecordingError as exc:
        if needle not in str(exc):
            raise SystemExit(f"FAIL: {label}: wrong refusal {exc}")
        reset()
        return
    rep = loaded["report"]
    if rep["economic_evidence"]:
        raise SystemExit(f"FAIL: {label}: the importer accepted the recording as evidence")
    if not any(needle in r for r in rep["refusals"]):
        raise SystemExit(f"FAIL: {label}: refusals {rep['refusals']} do not name {needle!r}")
    reset()


def alter(kind, pred, change):
    def edit(name, body):
        for r in body:
            if r.get("k") == kind and pred(r):
                change(r)
                return body
        return None
    return edit


run = manifest()
run["closed"] = False
write_manifest(run)
refused("unclosed run", "was not closed")

def add_drop(name, body):
    at = body[len(body) // 2]["recv_ms"]
    body.insert(len(body) // 2, {"k": "drop", "count": 3, "from_ms": at, "to_ms": at, "recv_ms": at, "session": body[len(body) // 2].get("session", 0)})
    return body
rewrite(add_drop)
refused("dropped records", "dropped")

rewrite(alter("book", lambda r: r.get("outcome") == "accepted", lambda r: r.update(outcome="duplicate")))
refused("rehashed fabricated outcome", "disagree with the recorder")

rewrite(alter("book", lambda r: r.get("outcome") == "accepted", lambda r: r.update(identity="0" * 64)))
refused("rehashed fabricated identity", "disagree with the recorder")

def drop_marker(name, body):
    for i, r in enumerate(body):
        if r.get("k") == "book_marker" and r.get("reason") == "conflict":
            del body[i]
            return body
    return None
rewrite(drop_marker, counts=lambda seg, new: seg.update(book_markers=sum(1 for r in new if r.get("k") == "book_marker")))
refused("missing conflict marker", "disagree with the recorder")

def add_marker(name, body):
    for i, r in enumerate(body):
        if r.get("k") == "book" and r.get("outcome") == "accepted":
            fake = {"k": "book_marker", "coin": r["coin"], "kind": "l2_book", "seq": r["seq"] + 1, "at_ms": r["recv_ms"],
                    "recv_ms": r["recv_ms"], "reason": "silence", "marker_session": r["session"], "detail": "", "session": r["session"]}
            body.insert(i + 1, fake)
            return body
    return None
rewrite(add_marker, counts=lambda seg, new: seg.update(book_markers=sum(1 for r in new if r.get("k") == "book_marker")))
refused("fabricated silence marker", "disagree with the recorder")

rewrite(alter("book", lambda r: r.get("outcome") == "accepted" and r.get("session", 0) > 0, lambda r: r.update(session=r["session"] + 7)))
refused("conflicting session", "disagree with the recorder")

def swap_raw(name, body):
    for r in body:
        if r.get("k") == "book" and r.get("outcome") == "accepted":
            env = json.loads(r["raw"])
            env["data"]["levels"][0][0]["sz"] = "999"
            r["raw"] = json.dumps(env, separators=(",", ":"))
            r["bytes"] = len(r["raw"].encode())
            return body
    return None
rewrite(swap_raw)
refused("raw frame contradicting its recorded identity", "disagree with the recorder")

run = manifest()
run["book_contract"] = {**run["book_contract"], "source": "hyperliquid_ws_l2Book_fast"}
write_manifest(run)
refused("wrong source", "book contract")

run = manifest()
run["sources"] = {**run["sources"], "l2_book": "hyperliquid_ws_activeAssetCtx"}
write_manifest(run)
refused("wrong declared source", "different l2_book source")

for label, fn, exc in (("open interest", observation_replay.load_recording, observation_replay.RecordingError),
                       ("trade flow", trade_flow_replay.load_trade_recording, trade_flow_replay.TradeRecordingError),
                       ("volume profile", volume_profile_replay.load_profile_recording, trade_flow_replay.TradeRecordingError)):
    try:
        fn(run_dir)
    except exc as e:
        if "did not record" not in str(e):
            raise SystemExit(f"FAIL: the {label} importer refused a book-only run for the wrong reason: {e}")
    else:
        raise SystemExit(f"FAIL: the {label} importer accepted a book-only run")

oi = observation_replay.load_recording(mixed)
if not all(len(s["samples"]) > 0 for s in oi["series"].values()):
    raise SystemExit("FAIL: the open-interest importer lost samples in a mixed v3 run")
tf = trade_flow_replay.load_trade_recording(mixed)
if not tf["report"]["economic_evidence"] or tf["report"]["outcome_mismatches"] or tf["report"]["event_mismatches"]:
    raise SystemExit(f"FAIL: the trade-flow importer refused a mixed v3 run: {tf['report']['refusals']}")
vp = volume_profile_replay.load_profile_recording(mixed)
if not vp["report"]["economic_evidence"]:
    raise SystemExit(f"FAIL: the volume-profile importer refused a mixed v3 run: {vp['report']['refusals']}")
bk = book_replay.load_book_recording(mixed)
if not bk["report"]["economic_evidence"] or bk["report"]["mismatch_count"]:
    raise SystemExit(f"FAIL: the book importer refused a mixed v3 run: {bk['report']['refusals']}")

future = mixed + ".v4"
shutil.copytree(mixed, future)
run = manifest(future)
run["schema"] = "go_trader_observation_run/v4"
write_manifest(run, future)
for label, fn, exc in (("open interest", observation_replay.load_recording, observation_replay.RecordingError),
                       ("trade flow", trade_flow_replay.load_trade_recording, trade_flow_replay.TradeRecordingError),
                       ("volume profile", volume_profile_replay.load_profile_recording, trade_flow_replay.TradeRecordingError),
                       ("book", book_replay.load_book_recording, book_replay.BookRecordingError)):
    try:
        fn(future)
    except exc as e:
        if "schema" not in str(e):
            raise SystemExit(f"FAIL: the {label} importer refused an unknown schema for the wrong reason: {e}")
    else:
        raise SystemExit(f"FAIL: the {label} importer accepted an unknown run schema")
PY

echo "PASS: l2_book replay harness (sealed book payloads match the independent recording replay; mixed v3 runs keep every importer; tampered, truncated, unclosed, dropped, mislabeled and contradicted recordings refused)"
