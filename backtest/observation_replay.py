#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import json
import math
import os
import sys
from typing import Dict, List, Optional

from observation_kinds import v3_kinds_problem, v3_other_kind_records

RUN_SCHEMA = "go_trader_observation_run/v1"
RUN_SCHEMA_V2 = "go_trader_observation_run/v2"
SEGMENT_SCHEMA_V2 = "go_trader_observation_segment/v2"
RECORDING_SCHEMA_V2 = "go_trader_observation_recording/v2"
RUN_SCHEMA_V3 = "go_trader_observation_run/v3"
SEGMENT_SCHEMA_V3 = "go_trader_observation_segment/v3"
RECORDING_SCHEMA_V3 = "go_trader_observation_recording/v3"
SEGMENT_SCHEMA = "go_trader_observation_segment/v1"
RECORDING_SCHEMA = "go_trader_observation_recording/v1"
SERIES_SCHEMA = "go_trader_observation_series/v1"
ACCEPTED_SOURCE = "hyperliquid_ws_activeAssetCtx"
ACCEPTED_UNITS = "base"
ACCEPTED_TIME_BASIS = "receipt"
OBSERVATION_KIND = "open_interest"


TRADE_RECORD_KINDS = ("trades", "sub", "cover", "interval", "invalidate", "tick")


class RecordingError(ValueError):
    pass


def _v2_open_interest_contract(run_dir: str, run: dict) -> dict:
    segments = run.get("segments") or []
    if not segments:
        return {}
    blob = _read_segment_bytes(run_dir, segments[0]["file"])
    header = json.loads(blob.split(b"\n", 1)[0])
    contract = header.get("open_interest_contract") or {}
    return {k: contract.get(k) for k in ("units", "time_basis", "cadence_ms")}


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
        for gap in reversed(self.gaps):
            if gap[1] is None:
                return gap
        return None


def load_recording(run_dir: str) -> dict:
    run_dir = os.path.abspath(run_dir)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    v3 = run.get("schema") == RUN_SCHEMA_V3
    v2 = run.get("schema") == RUN_SCHEMA_V2 or v3
    if v3 and v3_kinds_problem(run):
        raise RecordingError(f"run manifest {v3_kinds_problem(run)}")
    other_kinds = v3_other_kind_records(run.get("kinds") or []) if v3 else frozenset()
    if v2:
        if OBSERVATION_KIND not in (run.get("kinds") or []):
            raise RecordingError(f"run manifest schema {run.get('schema')!r} did not record {OBSERVATION_KIND}; "
                                 "use trade_flow_replay.load_trade_recording for trade flow or book_replay.load_book_recording for l2_book")
        contract = _v2_open_interest_contract(run_dir, run)
        run = {**run, "source": run.get("sources", {}).get(OBSERVATION_KIND), **contract}
    elif run.get("schema") != RUN_SCHEMA:
        raise RecordingError(f"run manifest schema must be {RUN_SCHEMA!r}, {RUN_SCHEMA_V2!r} or {RUN_SCHEMA_V3!r}")
    segment_schema = SEGMENT_SCHEMA_V3 if v3 else (SEGMENT_SCHEMA_V2 if v2 else SEGMENT_SCHEMA)
    recording_schema = RECORDING_SCHEMA_V3 if v3 else (RECORDING_SCHEMA_V2 if v2 else RECORDING_SCHEMA)
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
        if seg.get("schema") != segment_schema or seg.get("index") != expected_index:
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
        if header.get("k") != "header" or header.get("schema") != recording_schema or header.get("run_id") != run.get("run_id"):
            raise RecordingError(f"segment {seg['file']} header does not match the run")
        if v3 and (v3_kinds_problem(header) or header.get("kinds") != run.get("kinds")):
            raise RecordingError(f"segment {seg['file']} header {v3_kinds_problem(header) or 'lists different kinds than the run'}")
        if v2:
            header = {**header, **(header.get("open_interest_contract") or {})}
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
                            start = min(st.last_recv, recv)
                            st.gaps.append([start, None, "disconnected", max(recv, start)])
                else:
                    raise RecordingError(f"{seg['file']}:{line_no} has unknown connection state {rec.get('state')!r}")
                continue
            if v2 and kind in TRADE_RECORD_KINDS:
                continue
            if kind in other_kinds:
                continue
            if kind == "drop":
                report["dropped"] += int(rec.get("count") or 0)
                start, stop = rec.get("from_ms"), rec.get("to_ms")
                for st in states.values():
                    st.gaps.append([int(start), int(max(stop, recv)), "recorder_overflow", int(start)])
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
                                 [{"start_ms": g[0], "end_ms": g[1], "detected_ms": g[3], "reason": g[2]}
                                  for g in st.gaps],
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
            "oib_coverage", "oib_max_gap_ms", "oib_window_sha256", "oib_carried", "signal"]
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
    replay_obs["gaps"] = [dict(g, end_ms=None if g["end_ms"] is not None and g["end_ms"] > cutoff else g["end_ms"])
                          for g in replay_obs["gaps"]
                          if g["start_ms"] <= cutoff and g["detected_ms"] <= cutoff
                          and (g["end_ms"] is None or g["end_ms"] >= cutoff - window)]
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


TRADE_INTERVAL_FIELDS = ("start_ms", "end_ms", "buy", "sell", "total", "trades", "records_hash", "session",
                         "finalized_ms", "status", "reason", "invalid_ms")
TRADE_DECISION_COLUMNS = ["timestamp", "cvd_reason", "cvd_flow_available", "cvd_setup_id", "cvd_ratio",
                          "cvd_flow_sha256", "cvd_deadline_ms", "cvd_carried", "signal"]


def _cmd_import_trades(args) -> int:
    from trade_flow_replay import load_trade_recording, write_trade_series
    loaded = load_trade_recording(args.run_dir)
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "frames", "malformed_frames", "dropped",
                                  "events_compared", "economic_evidence", "refusals", "max_frame_silence_ms")}
    out["sessions"] = report["sessions"]
    out["segments"] = report["segments"]
    out["series"] = {}
    for coin, series in loaded["series"].items():
        entry = {"intervals": len(series["intervals"]), "stats": series["stats"], "latency_ms": series["latency_ms"],
                 "intervals_sha256": series["intervals_sha256"]}
        if args.out_dir:
            path = os.path.join(args.out_dir, f"{coin}_trade_flow.json.gz")
            entry["file"] = path
            entry["file_sha256"] = write_trade_series(series, path)
        out["series"][coin] = entry
    print(json.dumps(out, indent=2, sort_keys=True))
    return 0


def _trade_decisions(df, flow: dict, params: dict) -> list:
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_strategies", "open", "futures"))
    import strategies as futures_registry
    out = futures_registry.apply_strategy("cumulative_volume_delta_divergence", df,
                                          {**params, "trade_flow_observations": flow})
    return json.loads(out[TRADE_DECISION_COLUMNS].to_json(orient="values", double_precision=15))


def _norm_intervals(intervals: list) -> list:
    return [{k: (iv.get(k) if iv.get(k) is not None or k != "reason" else "") for k in TRADE_INTERVAL_FIELDS}
            for iv in intervals]


def _trade_harness_case(directory: str, harness: dict, case: dict, loaded: dict) -> dict:
    import pandas as pd
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_tools"))
    from market_payload import market_frame_rows, market_trade_flow, validate_market_payload
    from trade_flow_replay import payload_from_series
    with open(os.path.join(directory, case["payload"])) as fh:
        payload = json.load(fh)
    validate_market_payload(payload)
    coin = case["coin"]
    rows = market_frame_rows(payload, coin, harness["timeframe"])
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    live = market_trade_flow(payload, coin)
    params = harness.get("params") or {}
    replay = payload_from_series(loaded["series"][coin], live["bar_interval_ms"], live["cutoff_ms"], live["window_ms"])
    live_a = _trade_decisions(df, live, params)
    live_b = _trade_decisions(df, market_trade_flow(payload, coin), params)
    replayed = _trade_decisions(df, replay, params)
    checks = {
        "payload_trade_flow_available": bool(live.get("available")),
        "repeat_evaluation_identical": live_a == live_b,
        "recording_intervals_match_seal": _norm_intervals(live["intervals"]) == _norm_intervals(replay["intervals"]),
        "recording_decisions_match_seal": live_a == replayed,
    }
    failures = [f"{k}: false" for k, v in checks.items() if not v]
    reasons = {}
    for r in live_a:
        reasons[r[1]] = reasons.get(r[1], 0) + 1
    return {"payload": case["payload"], "coin": coin, "cutoff_ms": live.get("cutoff_ms"),
            "intervals": len(live["intervals"]), "invalid_intervals": sum(1 for iv in live["intervals"] if iv["status"] == "invalid"),
            "checks": checks, "reasons": reasons, "failures": failures}


def _cmd_harness_trades(args) -> int:
    from trade_flow_replay import load_trade_recording
    with open(os.path.join(args.dir, "harness.json")) as fh:
        harness = json.load(fh)
    loaded = load_trade_recording(os.path.join(args.dir, "recording"))
    oi = load_recording(os.path.join(args.dir, "recording"))
    results = [_trade_harness_case(args.dir, harness, case, loaded) for case in harness["cases"]]
    failures = [f"{r['payload']}: {f}" for r in results for f in r["failures"]]
    for coin, want in (harness.get("expected_rejected") or {}).items():
        got = loaded["series"][coin]["stats"]["rejected"]
        if got != want:
            failures.append(f"{coin} rejected trades {got}, want {want}")
    if not loaded["report"]["economic_evidence"]:
        failures.append(f"the harness recording is refused as evidence: {loaded['report']['refusals']}")
    print(json.dumps({"recording": {k: loaded["report"][k] for k in ("frames", "events_compared", "sessions", "economic_evidence")},
                      "open_interest_samples": {c: len(s["samples"]) for c, s in oi["series"].items()},
                      "cases": results, "failures": failures}, indent=2, sort_keys=True, default=str))
    return 1 if failures else 0


def _cmd_import_books(args) -> int:
    from book_replay import load_book_recording, write_book_series
    loaded = load_book_recording(args.run_dir)
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "frames", "malformed_frames", "markers_compared", "dropped",
                                  "mismatch_count", "economic_evidence", "refusals", "sessions", "segments")}
    out["series"] = {}
    for coin, series in loaded["series"].items():
        entry = {"observations": len(series["observations"]), "markers": len(series["markers"]), "stats": series["stats"],
                 "latency_ms": series["latency_ms"], "max_accept_gap_ms": series["max_accept_gap_ms"],
                 "series_sha256": series["series_sha256"]}
        if args.out_dir:
            path = os.path.join(args.out_dir, f"{coin}_l2_book.json.gz")
            entry["file"] = path
            entry["file_sha256"] = write_book_series(series, path)
        out["series"][coin] = entry
    print(json.dumps(out, indent=2, sort_keys=True))
    return 0


def _book_harness_case(directory: str, case: dict, loaded: dict) -> dict:
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_tools"))
    from market_payload import market_l2_book, validate_market_payload
    from book_replay import book_identity, comparable_payload, payload_from_series
    with open(os.path.join(directory, case["payload"])) as fh:
        payload = json.load(fh)
    validate_market_payload(payload)
    coin = case["coin"]
    live = market_l2_book(payload, coin)
    again = market_l2_book(payload, coin)
    replay = payload_from_series(loaded["series"][coin], live.get("cutoff_ms"), case["need"], case["as_of_ms"],
                                 payload.get("snapshot_id", ""))
    full_identities = [r for r in live.get("observations") or [] if r["bid_depth"] == len(r["bids"]) and r["ask_depth"] == len(r["asks"])]
    checks = {
        "payload_book_available": bool(live.get("available")),
        "payload_need_matches_case": [live.get(k) for k in ("window_ms", "capacity", "depth")] == [case["need"][k] for k in ("window_ms", "capacity", "depth")],
        "prefix_complete_matches_case": live.get("prefix_complete") is case["prefix_complete"],
        "repeat_read_identical": live == again,
        "recording_matches_seal": comparable_payload(live) == comparable_payload(replay),
        "full_depth_identities_recompute": all(
            book_identity(coin, r["event_ms"], r["bids"], r["asks"]) == r["identity"] for r in full_identities),
    }
    sys.path.insert(0, os.path.join(_here, "..", "shared_strategies", "open"))
    import order_book_imbalance as obi
    decisions = {}
    for label, sec in (("seal", live), ("replay", replay)):
        p = {"depth_levels": min(5, int(live.get("depth") or 5)), "entry_threshold": 0.2, "confirmation_observations": 2}
        decisions[label] = [(d["signal"], d["reason"], d["entry_id"]) for d in (
            obi.evaluate_section(sec, coin, p, deadline) for deadline in range(
                int(live.get("cutoff_ms") or 0) - 20_000, int(live.get("cutoff_ms") or 0) + 1, 1000))]
    checks["evaluator_decisions_match_seal"] = decisions["seal"] == decisions["replay"]
    failures = [f"{k}: false" for k, v in checks.items() if not v]
    if not checks["recording_matches_seal"]:
        a, b = comparable_payload(live), comparable_payload(replay)
        failures.append("first differing fields: " + ", ".join(k for k in a if a[k] != b[k]))
    return {"payload": case["payload"], "coin": coin, "cutoff_ms": live.get("cutoff_ms"),
            "observations": len(live.get("observations") or []), "markers": [m["reason"] for m in live.get("markers") or []],
            "prefix_complete": live.get("prefix_complete"), "retained_from_ms": live.get("retained_from_ms"),
            "checks": checks, "failures": failures}


def _cmd_harness_books(args) -> int:
    from book_replay import load_book_recording
    with open(os.path.join(args.dir, "harness.json")) as fh:
        harness = json.load(fh)
    loaded = load_book_recording(os.path.join(args.dir, "recording"))
    results = [_book_harness_case(args.dir, case, loaded) for case in sorted(harness["cases"], key=lambda c: c["payload"])]
    failures = [f"{r['payload']}: {f}" for r in results for f in r["failures"]]
    for coin, want in sorted((harness.get("expected_stats") or {}).items()):
        got = loaded["series"][coin]["stats"]
        if got != want:
            failures.append(f"{coin} replayed stats {got}, the owner reported {want}")
    if not loaded["report"]["economic_evidence"]:
        failures.append(f"the harness recording is refused as evidence: {loaded['report']['refusals']}")
    if len(results) != 5:
        failures.append(f"the harness holds {len(results)} cases, want 5")
    print(json.dumps({"recording": {k: loaded["report"][k] for k in ("frames", "malformed_frames", "markers_compared",
                                                                       "mismatch_count", "economic_evidence")},
                      "stats": {c: s["stats"] for c, s in loaded["series"].items()},
                      "cases": results, "failures": failures}, indent=2, sort_keys=True, default=str))
    return 1 if failures else 0


PROFILE_DECISION_COLUMNS = ["timestamp", "vpr_reason", "vpr_profile_sha256", "vpr_val", "vpr_vah", "vpr_deadline_ms",
                            "vpr_carried", "signal"]
PROFILE_SESSION_FIELDS = ("session_start_ms", "status", "reason_code", "invalid_ms", "artifact")


def _cmd_import_profiles(args) -> int:
    from volume_profile_replay import coverage_report, load_profile_recording, write_profile_series
    loaded = load_profile_recording(args.run_dir)
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "frames", "malformed_frames", "dropped",
                                  "events_compared", "economic_evidence", "refusals")}
    out["series"] = {}
    for coin, series in loaded["series"].items():
        entry = {"coverage": coverage_report(series), "stats": series["stats"]}
        if args.out_dir:
            path = os.path.join(args.out_dir, f"{coin}_volume_profile.json.gz")
            entry["file"] = path
            entry["file_sha256"] = write_profile_series(series, path)
        out["series"][coin] = entry
    print(json.dumps(out, indent=2, sort_keys=True))
    return 0


def _profile_frame(payload: dict, coin: str, timeframe: str, cutoff_ms: int):
    import pandas as pd
    from market_payload import market_frame_rows
    rows = market_frame_rows(payload, coin, timeframe)
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df.attrs["decision_time"] = {"cutoff_ms": int(cutoff_ms)}
    return df


def _profile_decisions(df, profile: dict, params: dict):
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_strategies", "open", "futures"))
    import strategies as futures_registry
    out = futures_registry.apply_strategy("volume_profile_reclaim", df, {**params, "volume_profile_observations": profile})
    return out, json.loads(out[PROFILE_DECISION_COLUMNS].to_json(orient="values", double_precision=15))


def _last_closed(out):
    closed = out[out["vpr_reason"] != "bar_not_closed"]
    return closed.iloc[-1]


def _norm_sessions(sessions: list) -> list:
    return [{k: s.get(k) for k in PROFILE_SESSION_FIELDS} for s in sessions]


def _profile_expectation(harness: dict, coin: str, key: int) -> str:
    keys = harness["keys"]
    if key == keys["key0"]:
        return "profile_unavailable"
    if coin == "ETH":
        return "outside_value_area" if key == keys["key3"] else "profile_unavailable"
    return {keys["key0"]: "profile_pending", keys["key1"]: "entry_long", keys["key2"]: "profile_invalidated",
            keys["key3"]: "profile_unavailable"}[key]


def _profile_harness_case(directory: str, harness: dict, case: dict, loaded: dict) -> dict:
    from fractions import Fraction
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_tools"))
    from market_payload import market_volume_profile, validate_market_payload
    from volume_profile_inputs import value_area
    from volume_profile_replay import payload_from_profile_series
    with open(os.path.join(directory, case["payload"])) as fh:
        payload = json.load(fh)
    validate_market_payload(payload)
    coin = case["coin"]
    live = market_volume_profile(payload, coin)
    cutoff = int(live["cutoff_ms"])
    key = cutoff // 1000
    series = loaded["series"][coin]
    replay = payload_from_profile_series(series, live["bar_interval_ms"], cutoff)
    full = payload_from_profile_series(series, live["bar_interval_ms"])
    params = harness.get("params") or {}
    df = _profile_frame(payload, coin, harness["timeframe"], cutoff)
    live_out, live_a = _profile_decisions(df, live, params)
    _, live_b = _profile_decisions(df, market_volume_profile(payload, coin), params)
    replay_out, replayed = _profile_decisions(df, replay, params)
    full_out, full_rows = _profile_decisions(df, full, params)
    last_live, last_full = _last_closed(live_out), _last_closed(full_out)
    sealed = {float(s["session_start_ms"]) for s in live["sessions"]}
    want = _profile_expectation(harness, coin, key)
    checks = {
        "payload_profile_available": bool(live.get("available")),
        "repeat_evaluation_identical": live_a == live_b,
        "recording_sessions_match_seal": _norm_sessions(live["sessions"]) == _norm_sessions(replay["sessions"]),
        "recording_decisions_match_seal": live_a == replayed,
        "full_series_signals_match_seal": [r[-1] for r, prior in zip(live_a, live_out["vpr_profile_session_ms"]) if prior in sealed]
                                          == [r[-1] for r, prior in zip(full_rows, live_out["vpr_profile_session_ms"]) if prior in sealed],
        "full_series_last_decision_matches": [last_live[c] for c in PROFILE_DECISION_COLUMNS] == [last_full[c] for c in PROFILE_DECISION_COLUMNS]
                                             or (last_live["vpr_reason"] in ("profile_pending", "profile_unavailable")
                                                 and last_full["vpr_reason"] in ("profile_pending", "profile_unavailable")
                                                 and int(last_live["signal"]) == int(last_full["signal"]) == 0),
        "expected_last_reason": last_live["vpr_reason"] == want,
    }
    hand = harness.get("hand") or {}
    if coin == "BTC" and key == harness["keys"]["key1"]:
        art = live["sessions"][0]["artifact"]
        base = value_area(art, 4, Fraction(str(params["value_area_fraction"])), False)
        wide = value_area(art, 4, Fraction(str(hand["wide_fraction"])), False)
        bounds = value_area(art, 4, Fraction(str(params["value_area_fraction"])), True)
        checks["hand_value_area"] = (str(base["val"]), str(base["vah"]), str(base["poc_low"]), str(base["poc_high"])) == (
            hand["val"], hand["vah"], hand["poc_low"], hand["poc_high"])
        checks["hand_lower_side_tie"] = (str(wide["val"]), str(wide["vah"])) == (hand["wide_val"], hand["wide_vah"])
        checks["hand_session_bounds"] = (str(bounds["val"]), str(bounds["vah"])) == (hand["bounds_val"], hand["bounds_vah"])
        checks["hand_total_volume"] = base["total"] == round(hand["total_volume"] * 10 ** 8)
        checks["hand_entry_long"] = int(last_live["signal"]) == 1 and last_live["vpr_val"] == hand["val"]
    failures = [f"{k}: false" for k, v in checks.items() if not v]
    return {"payload": case["payload"], "coin": coin, "cutoff_ms": cutoff, "last_reason": last_live["vpr_reason"],
            "statuses": [s["status"] for s in live["sessions"]], "checks": checks, "failures": failures}


def _cmd_harness_profiles(args) -> int:
    from volume_profile_replay import load_profile_recording
    with open(os.path.join(args.dir, "harness.json")) as fh:
        harness = json.load(fh)
    loaded = load_profile_recording(os.path.join(args.dir, "recording"))
    results = [_profile_harness_case(args.dir, harness, case, loaded) for case in sorted(harness["cases"], key=lambda c: c["payload"])]
    failures = [f"{r['payload']}: {f}" for r in results for f in r["failures"]]
    if not loaded["report"]["economic_evidence"]:
        failures.append(f"the harness recording is refused as evidence: {loaded['report']['refusals']}")
    if len(results) != 8:
        failures.append(f"the harness holds {len(results)} cases, want 8")
    print(json.dumps({"recording": {k: loaded["report"][k] for k in ("frames", "events_compared", "economic_evidence")},
                      "complete_sessions": {c: s["complete_sessions"] for c, s in loaded["series"].items()},
                      "cases": results, "failures": failures}, indent=2, sort_keys=True, default=str))
    return 1 if failures else 0


PREMIUM_DECISION_COLUMNS = ["timestamp", "ppr_reason", "ppr_premium", "ppr_mean", "ppr_std", "ppr_z_extreme",
                            "ppr_window_sha256", "ppr_deadline_ms", "ppr_carried", "signal"]


def _cmd_import_premium(args) -> int:
    from premium_replay import load_premium_recording, write_premium_series
    loaded = load_premium_recording(args.run_dir)
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "quote_frames", "context_frames", "malformed_frames",
                                  "dropped", "events_compared", "economic_evidence", "refusals", "connections",
                                  "max_quote_silence_ms", "max_context_silence_ms")}
    out["sessions"] = report["sessions"]
    out["segments"] = report["segments"]
    out["series"] = {}
    for coin, series in loaded["series"].items():
        entry = {"samples": len(series["samples"]), "stats": series["stats"], "quote_latency_ms": series["quote_latency_ms"],
                 "samples_sha256": series["samples_sha256"]}
        if args.out_dir:
            path = os.path.join(args.out_dir, f"{coin}_perp_premium.json.gz")
            entry["file"] = path
            entry["file_sha256"] = write_premium_series(series, path)
        out["series"][coin] = entry
    print(json.dumps(out, indent=2, sort_keys=True))
    return 0


def _premium_decisions(df, obs: dict, params: dict) -> list:
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_strategies", "open", "futures"))
    import strategies as futures_registry
    out = futures_registry.apply_strategy("perpetual_premium_reversion", df, {**params, "premium_observations": obs})
    return json.loads(out[PREMIUM_DECISION_COLUMNS].to_json(orient="values", double_precision=15))


def _premium_harness_case(directory: str, harness: dict, case: dict, loaded: dict) -> dict:
    import pandas as pd
    _here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(_here, "..", "shared_tools"))
    from market_payload import MARKET_PAYLOAD_VERSION, market_frame_rows, market_premium, validate_market_payload
    from premium_replay import payload_from_series
    with open(os.path.join(directory, case["payload"])) as fh:
        payload = json.load(fh)
    validate_market_payload(payload)
    coin = case["coin"]
    rows = market_frame_rows(payload, coin, harness["timeframe"])
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    live = market_premium(payload, coin)
    params = harness.get("params") or {}
    replay = payload_from_series(loaded["series"][coin], live["bar_interval_ms"], live["cutoff_ms"], live["window_ms"])
    live_a = _premium_decisions(df, live, params)
    live_b = _premium_decisions(df, market_premium(payload, coin), params)
    replayed = _premium_decisions(df, replay, params)
    row = next((r for r in live_a if r[7] is not None and int(r[7]) == int(case["decision_ms"]) and not r[8]), None)
    checks = {
        "payload_version_premium": payload.get("version") == MARKET_PAYLOAD_VERSION,
        "payload_premium_available": bool(live.get("available")),
        "oracle_event_time_absent": all((s.get("context") or {}).get("oracle_event_ms") is None for s in live["samples"]),
        "repeat_evaluation_identical": live_a == live_b,
        "recording_samples_match_seal": live["samples"] == replay["samples"],
        "recording_decisions_match_seal": live_a == replayed,
        "decision_row_present": row is not None,
        "decision_signal": row is not None and int(row[-1]) == int(case["signal"]),
        "decision_reason": row is not None and ("reason" not in case or row[1] == case["reason"]),
    }
    failures = [f"{k}: false" for k, v in checks.items() if not v]
    reasons = {}
    for r in live_a:
        reasons[r[1]] = reasons.get(r[1], 0) + 1
    return {"payload": case["payload"], "coin": coin, "cutoff_ms": live.get("cutoff_ms"), "samples": len(live["samples"]),
            "decision": row, "checks": checks, "reasons": reasons, "failures": failures}


def _cmd_harness_premium(args) -> int:
    from premium_replay import load_premium_recording
    with open(os.path.join(args.dir, "harness.json")) as fh:
        harness = json.load(fh)
    loaded = load_premium_recording(os.path.join(args.dir, "recording"))
    results = [_premium_harness_case(args.dir, harness, case, loaded) for case in harness["cases"]]
    failures = [f"{r['payload']}: {f}" for r in results for f in r["failures"]]
    for coin, want in (harness.get("expected_rejected") or {}).items():
        got = loaded["series"][coin]["stats"]["rejected"]
        if got != want:
            failures.append(f"{coin} rejected premium records {got}, want {want}")
    if not loaded["report"]["economic_evidence"]:
        failures.append(f"the harness recording is refused as evidence: {loaded['report']['refusals']}")
    print(json.dumps({"recording": {k: loaded["report"][k] for k in ("quote_frames", "context_frames", "events_compared",
                                                                     "sessions", "economic_evidence", "malformed_frames")},
                      "cases": results, "failures": failures}, indent=2, sort_keys=True, default=str))
    return 1 if failures else 0


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Verify and import go-trader observation recordings (#1635, #1637, #1638, #1639, #1665)")
    sub = p.add_subparsers(dest="cmd", required=True)
    imp = sub.add_parser("import", help="Verify a closed recording run and print (or write) its rebuilt series")
    imp.add_argument("--run-dir", required=True)
    imp.add_argument("--out-dir", default="")
    har = sub.add_parser("harness", help="Compare sealed-payload decisions with recording-replay decisions")
    har.add_argument("--dir", required=True)
    imt = sub.add_parser("import-trades", help="Replay a closed trade-flow recording independently and print (or write) its series")
    imt.add_argument("--run-dir", required=True)
    imt.add_argument("--out-dir", default="")
    hat = sub.add_parser("harness-trades", help="Compare sealed trade-flow payloads and decisions with the recording replay")
    hat.add_argument("--dir", required=True)
    imp2 = sub.add_parser("import-profiles", help="Rebuild daily executed-trade volume profiles from a closed trade recording")
    imp2.add_argument("--run-dir", required=True)
    imp2.add_argument("--out-dir", default="")
    hap = sub.add_parser("harness-profiles", help="Compare sealed volume-profile payloads and decisions with the recording replay")
    hap.add_argument("--dir", required=True)
    imb = sub.add_parser("import-books", help="Replay a closed l2_book recording independently and print (or write) its series")
    imb.add_argument("--run-dir", required=True)
    imb.add_argument("--out-dir", default="")
    hab = sub.add_parser("harness-books", help="Compare sealed l2_book payloads with the independent recording replay")
    hab.add_argument("--dir", required=True)
    imp3 = sub.add_parser("import-premium", help="Replay a closed quote/oracle premium recording independently and print (or write) its series")
    imp3.add_argument("--run-dir", required=True)
    imp3.add_argument("--out-dir", default="")
    hpr = sub.add_parser("harness-premium", help="Compare sealed premium payloads and decisions with the recording replay")
    hpr.add_argument("--dir", required=True)
    args = p.parse_args(argv)
    from trade_flow_replay import TradeRecordingError
    from book_replay import BookRecordingError
    from premium_replay import PremiumRecordingError
    try:
        if args.cmd == "import-premium":
            return _cmd_import_premium(args)
        if args.cmd == "harness-premium":
            return _cmd_harness_premium(args)
        if args.cmd == "import":
            return _cmd_import(args)
        if args.cmd == "import-trades":
            return _cmd_import_trades(args)
        if args.cmd == "harness-trades":
            return _cmd_harness_trades(args)
        if args.cmd == "import-profiles":
            return _cmd_import_profiles(args)
        if args.cmd == "harness-profiles":
            return _cmd_harness_profiles(args)
        if args.cmd == "import-books":
            return _cmd_import_books(args)
        if args.cmd == "harness-books":
            return _cmd_harness_books(args)
        return _cmd_harness(args)
    except (RecordingError, TradeRecordingError, BookRecordingError, PremiumRecordingError) as exc:
        print(f"recording error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
