#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import json
import math
import os
import sys
from typing import Dict, List, Optional

RUN_SCHEMA = "go_trader_observation_run/v1"
SEGMENT_SCHEMA = "go_trader_observation_segment/v1"
RECORDING_SCHEMA = "go_trader_observation_recording/v1"
SERIES_SCHEMA = "go_trader_observation_series/v1"
ACCEPTED_SOURCE = "hyperliquid_ws_activeAssetCtx"
ACCEPTED_UNITS = "base"
ACCEPTED_TIME_BASIS = "receipt"
OBSERVATION_KIND = "open_interest"


class RecordingError(ValueError):
    pass


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def _read_segment_bytes(run_dir: str, name: str) -> bytes:
    plain = os.path.join(run_dir, name)
    packed = plain + ".gz"
    if os.path.isfile(plain):
        with open(plain, "rb") as fh:
            return fh.read()
    if os.path.isfile(packed):
        with gzip.open(packed, "rb") as fh:
            return fh.read()
    raise RecordingError(f"segment {name} is missing (looked for {name} and {name}.gz)")


def _bucket(recv_ms: int, cadence_ms: int) -> int:
    return -(-recv_ms // cadence_ms) * cadence_ms


def _parse_open_interest(raw: str) -> Optional[float]:
    try:
        env = json.loads(raw)
        text = env["data"]["ctx"]["openInterest"]
    except (ValueError, KeyError, TypeError):
        return None
    if not isinstance(text, str):
        return None
    try:
        return float(text.strip())
    except ValueError:
        return None


class _CoinState:
    def __init__(self):
        self.samples: List[dict] = []
        self.gaps: List[list] = []
        self.last_recv = 0
        self.seen = {}

    def open_gap(self):
        if self.gaps and self.gaps[-1][1] is None:
            return self.gaps[-1]
        return None


def load_recording(run_dir: str) -> dict:
    run_dir = os.path.abspath(run_dir)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    if run.get("schema") != RUN_SCHEMA:
        raise RecordingError(f"run manifest schema must be {RUN_SCHEMA!r}")
    if not run.get("closed"):
        raise RecordingError("the recording run was not closed; its last segment may be truncated")
    if (run.get("source"), run.get("units"), run.get("time_basis")) != (ACCEPTED_SOURCE, ACCEPTED_UNITS, ACCEPTED_TIME_BASIS):
        raise RecordingError(
            f"run declares source {run.get('source')!r}, units {run.get('units')!r}, time basis "
            f"{run.get('time_basis')!r}; only {ACCEPTED_SOURCE!r}/{ACCEPTED_UNITS!r}/{ACCEPTED_TIME_BASIS!r} is accepted")
    cadence = run.get("cadence_ms")
    if not isinstance(cadence, int) or cadence <= 0:
        raise RecordingError(f"cadence_ms {cadence!r} is not a positive integer")
    coins = list(run.get("coins") or [])
    states: Dict[str, _CoinState] = {c: _CoinState() for c in coins}
    segments = run.get("segments") or []
    if not segments:
        raise RecordingError("the run has no closed segment")
    report = {"run_id": run.get("run_id"), "segments": [], "dropped": 0, "rejected": 0,
              "refreshed": 0, "accepted": 0, "sessions": set(), "coins": coins,
              "started_ms": run.get("started_ms"), "closed_ms": run.get("closed_ms")}
    session = None
    for expected_index, seg in enumerate(segments, start=1):
        if seg.get("schema") != SEGMENT_SCHEMA or seg.get("index") != expected_index:
            raise RecordingError(f"segment manifest {expected_index} is missing or out of order")
        with open(os.path.join(run_dir, seg["file"] + ".manifest.json")) as fh:
            standalone = json.load(fh)
        if standalone != seg:
            raise RecordingError(f"segment {seg['file']}: its manifest file disagrees with the run manifest")
        blob = _read_segment_bytes(run_dir, seg["file"])
        digest = sha256_bytes(blob)
        if digest != seg.get("sha256"):
            raise RecordingError(f"segment {seg['file']}: sha256 {digest} does not match the manifest {seg.get('sha256')}")
        if not blob.endswith(b"\n"):
            raise RecordingError(f"segment {seg['file']} is truncated (no final newline)")
        lines = blob.decode("utf-8").split("\n")[:-1]
        if len(lines) < 2:
            raise RecordingError(f"segment {seg['file']} has no header and end record")
        header = json.loads(lines[0])
        end = json.loads(lines[-1])
        if header.get("k") != "header" or header.get("schema") != RECORDING_SCHEMA or header.get("run_id") != run.get("run_id"):
            raise RecordingError(f"segment {seg['file']} header does not match the run")
        if header.get("cadence_ms") != cadence or header.get("units") != ACCEPTED_UNITS or header.get("source") != ACCEPTED_SOURCE:
            raise RecordingError(f"segment {seg['file']} header declares a different source, units or cadence")
        if end.get("k") != "end" or end.get("records") != len(lines) - 2:
            raise RecordingError(f"segment {seg['file']} end record is missing or miscounts its records")
        for line_no, line in enumerate(lines[1:-1], start=2):
            rec = json.loads(line)
            kind = rec.get("k")
            recv = rec.get("recv_ms")
            if not isinstance(recv, int):
                raise RecordingError(f"{seg['file']}:{line_no} has no integer recv_ms")
            report["sessions"].add(rec.get("session"))
            if kind == "conn":
                if rec.get("state") == "connected":
                    session = rec.get("session")
                elif rec.get("state") == "disconnected":
                    for st in states.values():
                        if st.last_recv and st.open_gap() is None:
                            st.gaps.append([min(st.last_recv, recv), None, "disconnected"])
                else:
                    raise RecordingError(f"{seg['file']}:{line_no} has unknown connection state {rec.get('state')!r}")
                continue
            if kind == "drop":
                report["dropped"] += int(rec.get("count") or 0)
                start, stop = rec.get("from_ms"), rec.get("to_ms")
                for st in states.values():
                    st.gaps.append([int(start), int(max(stop, recv)), "recorder_overflow"])
                continue
            if kind != "obs":
                raise RecordingError(f"{seg['file']}:{line_no} has unknown record kind {kind!r}")
            coin = rec.get("coin")
            if coin not in states or rec.get("kind") != OBSERVATION_KIND:
                raise RecordingError(f"{seg['file']}:{line_no} is for an unrecorded coin or kind")
            if rec.get("session") != session:
                raise RecordingError(f"{seg['file']}:{line_no} session {rec.get('session')} is not the connected session {session}")
            st = states[coin]
            if not rec.get("accepted"):
                report["rejected"] += 1
                continue
            value = rec.get("value")
            parsed = _parse_open_interest(rec.get("raw") or "")
            if parsed is None or not isinstance(value, (int, float)) or float(value) != parsed:
                raise RecordingError(f"{seg['file']}:{line_no} value {value!r} does not match its raw record ({parsed!r})")
            if not math.isfinite(float(value)) or float(value) < 0:
                raise RecordingError(f"{seg['file']}:{line_no} accepted a negative or non-finite value")
            if recv < st.last_recv:
                raise RecordingError(f"{seg['file']}:{line_no} receipt time moves backwards for {coin}")
            ident = (rec.get("session"), rec.get("seq"))
            if ident in st.seen:
                if st.seen[ident] != (recv, float(value)):
                    raise RecordingError(f"{seg['file']}:{line_no} conflicting records for {coin} session/seq {ident}")
                raise RecordingError(f"{seg['file']}:{line_no} duplicate record for {coin} session/seq {ident}")
            st.seen[ident] = (recv, float(value))
            gap = st.open_gap()
            if gap is not None:
                gap[1] = recv
            sample = {"recv_ms": recv, "event_ms": None, "value": float(value),
                      "session": rec.get("session"), "seq": rec.get("seq")}
            same_bucket = bool(st.samples) and _bucket(st.samples[-1]["recv_ms"], cadence) == _bucket(recv, cadence)
            if same_bucket != bool(rec.get("refreshed")):
                raise RecordingError(f"{seg['file']}:{line_no} refresh flag {rec.get('refreshed')} disagrees with the "
                                     f"cadence bucket rule ({same_bucket})")
            if same_bucket:
                st.samples[-1] = sample
                report["refreshed"] += 1
            else:
                st.samples.append(sample)
                report["accepted"] += 1
            st.last_recv = recv
        report["segments"].append({"file": seg["file"], "sha256": digest, "records": seg.get("records"),
                                   "complete": seg.get("complete")})
    report["sessions"] = sorted(s for s in report["sessions"] if s is not None)
    out = {}
    for coin, st in states.items():
        out[coin] = build_series(coin, cadence, st.samples,
                                 [{"start_ms": g[0], "end_ms": g[1], "reason": g[2]} for g in st.gaps],
                                 {"run_id": run.get("run_id"), "segments": report["segments"]})
    return {"series": out, "report": report}


def series_samples_sha256(samples: list, gaps: list) -> str:
    blob = json.dumps({"samples": samples, "gaps": gaps}, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return sha256_bytes(blob)


def build_series(coin: str, cadence: int, samples: list, gaps: list, provenance: dict) -> dict:
    return {
        "schema": SERIES_SCHEMA,
        "available": True,
        "kind": OBSERVATION_KIND,
        "coin": coin,
        "source": ACCEPTED_SOURCE,
        "units": ACCEPTED_UNITS,
        "time_basis": ACCEPTED_TIME_BASIS,
        "cadence_ms": cadence,
        "samples": samples,
        "gaps": gaps,
        "samples_sha256": series_samples_sha256(samples, gaps),
        "provenance": provenance,
    }


def for_bars(series: dict, bar_interval_ms: int, bar_endpoint_offset_ms: int,
             cutoff_ms: Optional[int] = None) -> dict:
    out = dict(series)
    out["bar_interval_ms"] = int(bar_interval_ms)
    out["bar_endpoint_offset_ms"] = int(bar_endpoint_offset_ms)
    if cutoff_ms is not None:
        out["cutoff_ms"] = int(cutoff_ms)
    return out


def write_series(series: dict, path: str) -> str:
    text = json.dumps(series, sort_keys=True, separators=(",", ":")).encode("utf-8")
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    import io
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text)
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())
    return sha256_bytes(buf.getvalue())


def read_series(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        series = json.loads(fh.read().decode("utf-8"))
    if not isinstance(series, dict) or series.get("schema") != SERIES_SCHEMA:
        raise RecordingError(f"{path}: schema must be {SERIES_SCHEMA!r}")
    if (series.get("source"), series.get("units"), series.get("kind")) != (ACCEPTED_SOURCE, ACCEPTED_UNITS, OBSERVATION_KIND):
        raise RecordingError(f"{path}: unsupported source, units or kind")
    if series.get("samples_sha256") != series_samples_sha256(series.get("samples") or [], series.get("gaps") or []):
        raise RecordingError(f"{path}: samples_sha256 does not match its samples and gaps")
    return series


def coverage_report(series: dict, start_ms: int, end_ms: int) -> dict:
    cadence = int(series["cadence_ms"])
    expected = max(0, (end_ms - start_ms) // cadence)
    buckets = {_bucket(s["recv_ms"], cadence) for s in series["samples"]
               if start_ms < s["recv_ms"] <= end_ms}
    gaps = [g for g in series["gaps"]
            if g["start_ms"] < end_ms and (g["end_ms"] is None or g["end_ms"] > start_ms)]
    inside = [s["recv_ms"] for s in series["samples"] if start_ms <= s["recv_ms"] <= end_ms]
    return {
        "expected_buckets": int(expected),
        "present_buckets": len(buckets),
        "bucket_coverage": round(len(buckets) / expected, 6) if expected else None,
        "first_sample_ms": min(inside) if inside else None,
        "last_sample_ms": max(inside) if inside else None,
        "gap_markers": len(gaps),
        "available": bool(inside),
    }


def _cmd_import(args) -> int:
    loaded = load_recording(args.run_dir)
    report = loaded["report"]
    out = {"run_id": report["run_id"], "accepted": report["accepted"], "refreshed": report["refreshed"],
           "rejected": report["rejected"], "dropped": report["dropped"], "sessions": report["sessions"],
           "segments": report["segments"], "series": {}}
    for coin, series in loaded["series"].items():
        entry = {"samples": len(series["samples"]), "gaps": series["gaps"],
                 "samples_sha256": series["samples_sha256"]}
        if series["samples"]:
            entry["first_ms"] = series["samples"][0]["recv_ms"]
            entry["last_ms"] = series["samples"][-1]["recv_ms"]
        if args.out_dir:
            path = os.path.join(args.out_dir, f"{coin}_open_interest.json.gz")
            entry["file"] = path
            entry["file_sha256"] = write_series(series, path)
        out["series"][coin] = entry
    print(json.dumps(out, indent=2, sort_keys=True))
    return 0


def _decisions(df, observations: dict, params: dict) -> list:
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_strategies", "open", "futures"))
    import strategies as futures_registry
    out = futures_registry.apply_strategy("open_interest_breakout", df,
                                          {**params, "open_interest_observations": observations})
    cols = ["timestamp", "oib_reason", "oib_oi_valid", "oib_oi_current", "oib_oi_prior", "oib_oi_change",
            "oib_coverage", "oib_max_gap_ms", "oib_carried", "signal"]
    return json.loads(out[cols].to_json(orient="values", double_precision=15))


def _harness_case(directory: str, harness: dict, case: dict, loaded: dict) -> dict:
    import pandas as pd
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_tools"))
    from market_payload import market_frame_rows, market_observation, validate_market_payload
    with open(os.path.join(directory, case["payload"])) as fh:
        payload = json.load(fh)
    validate_market_payload(payload)
    coin, timeframe = harness["coin"], harness["timeframe"]
    rows = market_frame_rows(payload, coin, timeframe)
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    live_obs = market_observation(payload, coin, OBSERVATION_KIND)
    params = harness.get("params") or {}
    live_a = _decisions(df, live_obs, params)
    live_b = _decisions(df, market_observation(payload, coin, OBSERVATION_KIND), params)
    series = loaded["series"][coin]
    cutoff, window = live_obs["cutoff_ms"], live_obs["window_ms"]
    replay_obs = for_bars(series, live_obs["bar_interval_ms"], live_obs["bar_endpoint_offset_ms"], cutoff)
    replay_obs["samples"] = [s for s in replay_obs["samples"] if cutoff - window <= s["recv_ms"] <= cutoff]
    replay_obs["gaps"] = [g for g in replay_obs["gaps"]
                          if g["start_ms"] <= cutoff and (g["end_ms"] is None or g["end_ms"] >= cutoff - window)]
    replay = _decisions(df, replay_obs, params)
    live_samples = [(s["recv_ms"], s["value"], s["session"], s["seq"]) for s in live_obs["samples"]]
    replay_samples = [(s["recv_ms"], s["value"], s["session"], s["seq"]) for s in replay_obs["samples"]]
    last = live_a[-1]
    checks = {
        "payload_observations_available": bool(live_obs.get("available")),
        "repeat_evaluation_identical": live_a == live_b,
        "recording_samples_match_seal": live_samples == replay_samples,
        "recording_decisions_match_seal": live_a == replay,
    }
    observed = {"last_signal": last[-1], "last_reason": last[1], "last_carried": last[-2]}
    failures = [f"{k}: false" for k, v in checks.items() if not v]
    for key, want in (case.get("expect") or {}).items():
        if observed.get(key) != want:
            failures.append(f"{key}: got {observed.get(key)!r}, want {want!r}")
    reasons = {}
    for r in live_a:
        reasons[r[1]] = reasons.get(r[1], 0) + 1
    return {"payload": case["payload"], "cutoff_ms": cutoff, "samples": len(live_samples),
            "gaps": live_obs.get("gaps"), "checks": checks, "observed": observed, "reasons": reasons,
            "last_row": last, "failures": failures}


def _cmd_harness(args) -> int:
    with open(os.path.join(args.dir, "harness.json")) as fh:
        harness = json.load(fh)
    loaded = load_recording(os.path.join(args.dir, "recording"))
    results = [_harness_case(args.dir, harness, case, loaded) for case in harness["cases"]]
    failures = [f"{r['payload']}: {f}" for r in results for f in r["failures"]]
    rejected = loaded["report"]["rejected"]
    if rejected != harness.get("expected_rejected_records"):
        failures.append(f"recording rejected records {rejected}, want {harness.get('expected_rejected_records')}")
    print(json.dumps({"recording": {"rejected": rejected, "sessions": loaded["report"]["sessions"],
                                    "accepted": loaded["report"]["accepted"],
                                    "refreshed": loaded["report"]["refreshed"],
                                    "segments": loaded["report"]["segments"]},
                      "cases": results, "failures": failures}, indent=2, sort_keys=True, default=str))
    return 1 if failures else 0


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Verify and import go-trader observation recordings (#1637)")
    sub = p.add_subparsers(dest="cmd", required=True)
    imp = sub.add_parser("import", help="Verify a closed recording run and print (or write) its rebuilt series")
    imp.add_argument("--run-dir", required=True)
    imp.add_argument("--out-dir", default="")
    har = sub.add_parser("harness", help="Compare sealed-payload decisions with recording-replay decisions")
    har.add_argument("--dir", required=True)
    args = p.parse_args(argv)
    try:
        if args.cmd == "import":
            return _cmd_import(args)
        return _cmd_harness(args)
    except RecordingError as exc:
        print(f"recording error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
