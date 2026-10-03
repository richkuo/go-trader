#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import io
import json
import math
import os
import shutil
import sys
import tempfile
import time
import urllib.request
from typing import Optional

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.abspath(os.path.join(_HERE, ".."))
_TOOLS_DIR = os.path.join(_REPO_ROOT, "shared_tools")
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)

SCHEMA = "offline_candle_manifest/v1"
PROVENANCE_KINDS = ("venue", "proxy")
HOUR_MS = 3_600_000
INTERVAL_MS = {
    "15m": 900_000,
    "30m": 1_800_000,
    "1h": 3_600_000,
    "4h": 14_400_000,
    "1d": 86_400_000,
}
CANDLE_COLUMNS = ("timestamp", "open", "high", "low", "close", "volume")
FUNDING_COLUMNS = ("timestamp", "rate")
COST_KEYS = ("taker_fee_pct", "maker_fee_pct", "slippage_bps",
             "min_notional_usd", "min_notional_margin")
HL_INFO_URL = "https://api.hyperliquid.xyz/info"


class ManifestError(ValueError):
    pass


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _ts_ms(value) -> int:
    ts = pd.Timestamp(value)
    ts = ts.tz_localize("UTC") if ts.tz is None else ts.tz_convert("UTC")
    return int(ts.value // 1_000_000)


def _finite_nonneg(name: str, value) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ManifestError(f"{name} must be a number, got {value!r}")
    value = float(value)
    if not math.isfinite(value) or value < 0:
        raise ManifestError(f"{name} must be finite and >= 0, got {value!r}")
    return value


def _resolve(base_dir: str, rel: str) -> str:
    if not isinstance(rel, str) or not rel or os.path.isabs(rel):
        raise ManifestError(f"manifest paths must be relative to the manifest, got {rel!r}")
    path = os.path.normpath(os.path.join(base_dir, rel))
    if not path.startswith(base_dir + os.sep):
        raise ManifestError(f"manifest path escapes the manifest directory: {rel!r}")
    return path


def _file_ref(base_dir: str, ref, label: str, verify: bool) -> dict:
    if not isinstance(ref, dict) or "path" not in ref or "sha256" not in ref:
        raise ManifestError(f"{label} must be {{path, sha256}}, got {ref!r}")
    path = _resolve(base_dir, ref["path"])
    out = {"path": ref["path"], "abs_path": path, "sha256": ref["sha256"]}
    if verify:
        if not os.path.isfile(path):
            raise ManifestError(f"{label}: frozen input {ref['path']} is missing")
        actual = sha256_file(path)
        if actual != ref["sha256"]:
            raise ManifestError(
                f"{label}: sha256 mismatch for {ref['path']} "
                f"(manifest {ref['sha256']}, file {actual})"
            )
    return out


def _meta_size_decimals(meta_path: str) -> dict:
    with open(meta_path) as fh:
        meta = json.load(fh)
    universe = meta.get("universe") if isinstance(meta, dict) else None
    if not isinstance(universe, list):
        raise ManifestError("venue meta snapshot has no universe list")
    out = {}
    for entry in universe:
        if isinstance(entry, dict) and isinstance(entry.get("name"), str):
            value = entry.get("szDecimals")
            if isinstance(value, int) and not isinstance(value, bool):
                out[entry["name"]] = value
    return out


def load_manifest(path: str, verify: bool = True) -> dict:
    path = os.path.abspath(path)
    base_dir = os.path.dirname(path)
    with open(path) as fh:
        raw = json.load(fh)
    if not isinstance(raw, dict) or raw.get("schema") != SCHEMA:
        raise ManifestError(f"{path}: schema must be {SCHEMA!r}")
    for key in ("study", "venue", "provenance", "interval", "warmup_bars",
                "windows", "costs", "datasets", "venue_reference"):
        if key not in raw:
            raise ManifestError(f"{path}: missing required key {key!r}")
    prov = raw["provenance"]
    if not isinstance(prov, dict) or prov.get("kind") not in PROVENANCE_KINDS:
        raise ManifestError(f"provenance.kind must be one of {PROVENANCE_KINDS}")
    if not str(prov.get("label") or "").strip():
        raise ManifestError("provenance.label must describe the candle source")
    interval = raw["interval"]
    if interval not in INTERVAL_MS:
        raise ManifestError(f"interval must be one of {sorted(INTERVAL_MS)}, got {interval!r}")
    warmup = raw["warmup_bars"]
    if isinstance(warmup, bool) or not isinstance(warmup, int) or warmup < 0:
        raise ManifestError(f"warmup_bars must be a non-negative integer, got {warmup!r}")

    windows = {}
    if not isinstance(raw["windows"], dict) or not raw["windows"]:
        raise ManifestError("windows must be a non-empty object")
    for name, spec in raw["windows"].items():
        if not isinstance(spec, dict) or not spec.get("start") or not spec.get("end"):
            raise ManifestError(f"window {name!r} needs finite start and end")
        start_ms, end_ms = _ts_ms(spec["start"]), _ts_ms(spec["end"])
        if start_ms >= end_ms:
            raise ManifestError(f"window {name!r}: start must precede end")
        windows[name] = {"start": spec["start"], "end": spec["end"],
                         "start_ms": start_ms, "end_ms": end_ms,
                         "role": spec.get("role", "")}

    costs_raw = raw["costs"]
    if not isinstance(costs_raw, dict) or sorted(costs_raw) != sorted(COST_KEYS):
        raise ManifestError(f"costs keys must be exactly {sorted(COST_KEYS)}")
    costs = {k: _finite_nonneg(f"costs.{k}", costs_raw[k]) for k in COST_KEYS}

    ref_raw = raw["venue_reference"]
    if not isinstance(ref_raw, dict) or "meta" not in ref_raw:
        raise ManifestError("venue_reference.meta is required")
    meta_ref = _file_ref(base_dir, ref_raw["meta"], "venue_reference.meta", verify)
    venue_reference = {"meta": meta_ref}
    if "l2_snapshot" in ref_raw:
        venue_reference["l2_snapshot"] = _file_ref(
            base_dir, ref_raw["l2_snapshot"], "venue_reference.l2_snapshot", verify)
    docs = ref_raw.get("documents") or []
    for doc in docs:
        quote = str(doc.get("quote") or "")
        digest = hashlib.sha256(quote.encode("utf-8")).hexdigest()
        if not quote or doc.get("quote_sha256") != digest:
            raise ManifestError(f"venue_reference document quote hash mismatch: {doc.get('url')!r}")
    venue_reference["documents"] = docs
    size_table = _meta_size_decimals(meta_ref["abs_path"]) if verify else {}

    datasets = []
    seen = set()
    if not isinstance(raw["datasets"], list) or not raw["datasets"]:
        raise ManifestError("datasets must be a non-empty list")
    for ds in raw["datasets"]:
        coin = str(ds.get("coin") or "").strip()
        if not coin:
            raise ManifestError(f"dataset needs a coin: {ds!r}")
        key = f"{coin} {interval}"
        if key in seen:
            raise ManifestError(f"duplicate dataset {key!r}")
        seen.add(key)
        size_decimals = ds.get("size_decimals")
        if isinstance(size_decimals, bool) or not isinstance(size_decimals, int) or size_decimals < 0:
            raise ManifestError(f"{key}: size_decimals must be a non-negative integer")
        if verify and size_table.get(coin) != size_decimals:
            raise ManifestError(
                f"{key}: size_decimals {size_decimals} disagrees with the pinned venue meta "
                f"({size_table.get(coin)!r})"
            )
        entry = {
            "key": key,
            "coin": coin,
            "symbol": ds.get("symbol") or f"{coin}/USDT",
            "size_decimals": size_decimals,
            "half_spread_bps": _finite_nonneg(f"{key}.half_spread_bps", ds.get("half_spread_bps")),
            "candles": _file_ref(base_dir, ds.get("candles"), f"{key}.candles", verify),
            "funding": (_file_ref(base_dir, ds["funding"], f"{key}.funding", verify)
                        if ds.get("funding") else None),
        }
        datasets.append(entry)

    return {
        "path": path,
        "base_dir": base_dir,
        "study": raw["study"],
        "venue": raw["venue"],
        "provenance": dict(prov),
        "interval": interval,
        "interval_ms": INTERVAL_MS[interval],
        "warmup_bars": warmup,
        "windows": windows,
        "costs": costs,
        "datasets": datasets,
        "venue_reference": venue_reference,
        "acquisition": raw.get("acquisition") or {},
        "raw": raw,
    }


def dataset_by_key(manifest: dict, key: str) -> dict:
    for ds in manifest["datasets"]:
        if ds["key"] == key:
            return ds
    raise ManifestError(f"manifest has no dataset {key!r}; known: {[d['key'] for d in manifest['datasets']]}")


def _read_csv_gz(path: str, columns: tuple) -> pd.DataFrame:
    with gzip.open(path, "rt") as fh:
        df = pd.read_csv(fh)
    if tuple(df.columns) != columns:
        raise ManifestError(f"{path}: columns must be {list(columns)}, got {list(df.columns)}")
    return df


def load_candles(dataset: dict) -> pd.DataFrame:
    df = _read_csv_gz(dataset["candles"]["abs_path"], CANDLE_COLUMNS)
    ts = pd.to_numeric(df["timestamp"], errors="coerce")
    if ts.isna().any():
        raise ManifestError(f"{dataset['key']}: non-numeric candle timestamp")
    ts = ts.astype("int64")
    if not bool((ts.diff().iloc[1:] > 0).all()):
        raise ManifestError(f"{dataset['key']}: candle timestamps must be unique and strictly increasing")
    out = pd.DataFrame({c: pd.to_numeric(df[c], errors="coerce").astype(float)
                        for c in ("open", "high", "low", "close", "volume")})
    out["timestamp"] = ts.to_numpy()
    out.index = pd.to_datetime(ts.to_numpy(), unit="ms")
    out.index.name = "datetime"
    return out


def window_frame(manifest: dict, dataset: dict, window_name: str):
    if window_name not in manifest["windows"]:
        raise ManifestError(f"unknown window {window_name!r}; known: {sorted(manifest['windows'])}")
    win = manifest["windows"][window_name]
    candles = load_candles(dataset)
    ts = candles["timestamp"].to_numpy()
    pre = np.flatnonzero(ts < win["start_ms"])
    warmup = manifest["warmup_bars"]
    if len(pre) < warmup:
        raise ManifestError(
            f"{dataset['key']} window {window_name}: {len(pre)} bars before the window start, "
            f"{warmup} warm-up bars required"
        )
    in_window = (ts >= win["start_ms"]) & (ts < win["end_ms"])
    if not in_window.any():
        raise ManifestError(f"{dataset['key']} window {window_name}: no candles inside the window")
    first = int(pre[-warmup]) if warmup > 0 else int(np.flatnonzero(in_window)[0])
    last = int(np.flatnonzero(in_window)[-1])
    frame = candles.iloc[first:last + 1].copy()
    step = manifest["interval_ms"]
    expected = (win["end_ms"] - win["start_ms"]) // step
    present = int(in_window.sum())
    window_ts = ts[in_window]
    gaps = np.diff(window_ts) // step - 1 if len(window_ts) > 1 else np.array([], dtype=np.int64)
    warm_ts = ts[first:first + warmup]
    warm_gaps = int((np.diff(warm_ts) // step - 1).clip(min=0).sum()) if len(warm_ts) > 1 else 0
    coverage = {
        "expected_bars": int(expected),
        "present_bars": present,
        "missing_bars": int(expected - present),
        "max_gap_bars": int(gaps.max()) if len(gaps) else 0,
        "first_bar": str(pd.to_datetime(int(window_ts[0]), unit="ms")),
        "last_bar": str(pd.to_datetime(int(window_ts[-1]), unit="ms")),
        "warmup_bars": warmup,
        "warmup_missing_bars": warm_gaps,
        "complete": bool(present == expected),
    }
    return frame, win, coverage


def load_funding(dataset: dict) -> Optional[pd.DataFrame]:
    if dataset.get("funding") is None:
        return None
    df = _read_csv_gz(dataset["funding"]["abs_path"], FUNDING_COLUMNS)
    df["timestamp"] = pd.to_numeric(df["timestamp"], errors="coerce")
    df["rate"] = pd.to_numeric(df["rate"], errors="coerce")
    if df.isna().any().any():
        raise ManifestError(f"{dataset['key']}: non-numeric funding row")
    df["timestamp"] = df["timestamp"].astype("int64")
    if not bool((df["timestamp"].diff().iloc[1:] > 0).all()):
        raise ManifestError(f"{dataset['key']}: funding timestamps must be unique and strictly increasing")
    return df


def funding_coverage(funding: Optional[pd.DataFrame], start_ms: int, end_ms: int) -> dict:
    expected = int((end_ms - start_ms) // HOUR_MS)
    if funding is None or funding.empty:
        return {"available": False, "expected_hours": expected, "present_hours": 0,
                "missing_hours": expected, "max_gap_hours": None, "complete": False}
    t = funding["timestamp"].to_numpy()
    inside = t[(t >= start_ms) & (t < end_ms)]
    hours = np.unique((inside - start_ms) // HOUR_MS)
    present = int(len(hours))
    edges = np.concatenate([[-1], hours, [expected]])
    max_gap = int((np.diff(edges) - 1).max()) if len(edges) > 1 else expected
    return {
        "available": True,
        "expected_hours": expected,
        "present_hours": present,
        "missing_hours": int(expected - present),
        "max_gap_hours": max_gap,
        "complete": bool(present == expected),
        "sum_rate": float(funding["rate"].to_numpy()[(t >= start_ms) & (t < end_ms)].sum()),
    }


def attach_funding_cost(frame: pd.DataFrame, dataset: dict, window: dict):
    from funding_fetcher import attach_funding_accrual_column
    funding = load_funding(dataset)
    coverage = funding_coverage(funding, window["start_ms"], window["end_ms"])
    if funding is None or funding.empty:
        return frame.copy(), coverage
    shaped = pd.DataFrame({"timestamp": funding["timestamp"], "rate": funding["rate"]})
    return attach_funding_accrual_column(frame, shaped), coverage


def execution_spec(manifest: dict, dataset: dict, cost_multiplier: float = 1.0) -> dict:
    costs = manifest["costs"]
    mult = _finite_nonneg("cost_multiplier", cost_multiplier)
    return {
        "taker_fee_pct": costs["taker_fee_pct"],
        "maker_fee_pct": costs["maker_fee_pct"],
        "half_spread_pct": dataset["half_spread_bps"] / 10_000.0 * mult,
        "slippage_pct": costs["slippage_bps"] / 10_000.0 * mult,
        "size_decimals": dataset["size_decimals"],
        "min_notional_usd": costs["min_notional_usd"],
        "min_notional_margin": costs["min_notional_margin"],
    }


def slice_window(frame: pd.DataFrame, window: dict) -> pd.DataFrame:
    ts = frame["timestamp"].to_numpy()
    mask = (ts >= window["start_ms"]) & (ts < window["end_ms"])
    return frame.loc[mask].copy()


def _post_info(payload: dict, retries: int = 5) -> object:
    body = json.dumps(payload).encode("utf-8")
    last = None
    for attempt in range(retries):
        req = urllib.request.Request(HL_INFO_URL, data=body,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except Exception as exc:
            last = exc
            time.sleep(2 * (attempt + 1))
    raise ManifestError(f"Hyperliquid info request failed: {last}")


def _fetch_hl_candles(coin: str, interval: str, start_ms: int, end_ms: int) -> pd.DataFrame:
    step = INTERVAL_MS[interval]
    now_ms = int(time.time() * 1000)
    end_ms = min(int(end_ms), now_ms - now_ms % step)
    rows = {}
    cursor = start_ms
    while cursor < end_ms:
        batch = _post_info({"type": "candleSnapshot",
                            "req": {"coin": coin, "interval": interval,
                                    "startTime": int(cursor), "endTime": int(end_ms)}})
        if not batch:
            break
        for c in batch:
            t = int(c["t"])
            if start_ms <= t and t + step <= end_ms:
                rows[t] = (t, c["o"], c["h"], c["l"], c["c"], c["v"])
        last_t = int(batch[-1]["t"])
        if last_t + step <= cursor:
            break
        cursor = last_t + step
        time.sleep(0.25)
    df = pd.DataFrame([rows[t] for t in sorted(rows)], columns=list(CANDLE_COLUMNS))
    return df


def _write_csv_gz(df: pd.DataFrame, path: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    text = df.to_csv(index=False, lineterminator="\n").encode("utf-8")
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text)
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())


def _acquire_targets(manifest: dict) -> dict:
    base = manifest["base_dir"]
    raw = manifest["raw"]
    targets = {"meta": _resolve(base, raw["venue_reference"]["meta"]["path"])}
    if "l2_snapshot" in raw["venue_reference"]:
        targets["l2_snapshot"] = _resolve(base, raw["venue_reference"]["l2_snapshot"]["path"])
    for i, ds_raw in enumerate(raw["datasets"]):
        targets[f"candles:{i}"] = _resolve(base, ds_raw["candles"]["path"])
        if ds_raw.get("funding"):
            targets[f"funding:{i}"] = _resolve(base, ds_raw["funding"]["path"])
    paths = list(targets.values())
    if len(set(paths)) != len(paths) or manifest["path"] in paths:
        raise ManifestError("acquire targets must be distinct files other than the manifest")
    return targets


def _commit_staged(moves: list, staging: str) -> None:
    done = []
    try:
        for i, (staged, target) in enumerate(moves):
            os.makedirs(os.path.dirname(target), exist_ok=True)
            backup = None
            if os.path.exists(target):
                backup = os.path.join(staging, f"backup-{i}")
                os.replace(target, backup)
            done.append((target, backup))
            os.replace(staged, target)
    except BaseException as exc:
        try:
            for target, backup in reversed(done):
                if backup is not None:
                    os.replace(backup, target)
                elif os.path.exists(target):
                    os.remove(target)
        except BaseException as rollback_exc:
            raise ManifestError(
                f"acquire failed while replacing files ({exc}) and the rollback also failed "
                f"({rollback_exc}); the previous files are kept in {staging}") from exc
        raise


def acquire(manifest_path: str, write_hashes: bool = False, overwrite: bool = False) -> dict:
    if os.environ.get("HYPERLIQUID_SECRET_KEY"):
        raise ManifestError("refusing to acquire research data with HYPERLIQUID_SECRET_KEY set")
    manifest = load_manifest(manifest_path, verify=False)
    targets = _acquire_targets(manifest)
    existing = sorted(os.path.relpath(p, manifest["base_dir"])
                      for p in targets.values() if os.path.exists(p))
    if existing and not overwrite:
        raise ManifestError(
            "refusing to replace existing frozen inputs without --overwrite: "
            + ", ".join(existing))
    write_hashes = write_hashes or bool(existing)
    acq = manifest["acquisition"]
    start_ms, end_ms = _ts_ms(acq["start"]), _ts_ms(acq["end"])
    raw = manifest["raw"]
    report = {"acquired_at": pd.Timestamp.now(tz="UTC").isoformat(), "datasets": {},
              "replaced": existing, "hashes_written": write_hashes}

    staging = tempfile.mkdtemp(prefix=".acquire-staging-", dir=manifest["base_dir"])
    keep_staging = False
    try:
        staged = {key: os.path.join(staging, key.replace(":", "-")) for key in targets}

        meta = _post_info({"type": "meta"})
        with open(staged["meta"], "w") as fh:
            json.dump(meta, fh, sort_keys=True, indent=1)
            fh.write("\n")
        if "l2_snapshot" in targets:
            books = {}
            for ds in manifest["datasets"]:
                book = _post_info({"type": "l2Book", "coin": ds["coin"]})
                levels = book.get("levels") or [[], []]
                bid = float(levels[0][0]["px"]) if levels[0] else float("nan")
                ask = float(levels[1][0]["px"]) if levels[1] else float("nan")
                mid = (bid + ask) / 2.0
                books[ds["coin"]] = {"time": book.get("time"), "best_bid": bid, "best_ask": ask,
                                     "half_spread_bps": (ask - bid) / 2.0 / mid * 10_000.0}
            with open(staged["l2_snapshot"], "w") as fh:
                json.dump(books, fh, sort_keys=True, indent=1)
                fh.write("\n")
            report["l2_snapshot"] = books

        from funding_fetcher import load_cached_funding
        from storage import load_funding_coverage
        with tempfile.TemporaryDirectory() as tmp:
            db_path = os.path.join(tmp, "funding.db")
            for i, (ds_raw, ds) in enumerate(zip(raw["datasets"], manifest["datasets"])):
                candles = _fetch_hl_candles(ds["coin"], manifest["interval"], start_ms, end_ms)
                if candles.empty:
                    raise ManifestError(f"{ds['key']}: venue returned no candles")
                _write_csv_gz(candles, staged[f"candles:{i}"])
                entry = {"candles": len(candles),
                         "first": str(pd.to_datetime(int(candles["timestamp"].iloc[0]), unit="ms")),
                         "last": str(pd.to_datetime(int(candles["timestamp"].iloc[-1]), unit="ms"))}
                if ds_raw.get("funding"):
                    funding = None
                    for attempt in range(5):
                        funding = load_cached_funding(ds["coin"], acq["start"], end_date=acq["end"],
                                                      db_path=db_path)
                        if funding is not None and not funding.empty:
                            break
                        time.sleep(15 * (attempt + 1))
                    if funding is None or funding.empty:
                        raise ManifestError(f"{ds['key']}: funding acquisition returned no rows")
                    fdf = funding[["timestamp", "rate"]].copy()
                    fdf = fdf[(fdf["timestamp"] >= start_ms) & (fdf["timestamp"] < end_ms)]
                    fdf = fdf.sort_values("timestamp").drop_duplicates("timestamp")
                    _write_csv_gz(fdf.reset_index(drop=True), staged[f"funding:{i}"])
                    entry["funding_rows"] = int(len(fdf))
                    entry["funding_store_coverage"] = load_funding_coverage(
                        "hyperliquid", ds["coin"], db_path=db_path)
                report["datasets"][ds["key"]] = entry

        moves = [(staged[key], targets[key]) for key in targets]
        if write_hashes:
            raw["venue_reference"]["meta"]["sha256"] = sha256_file(staged["meta"])
            if "l2_snapshot" in targets:
                raw["venue_reference"]["l2_snapshot"]["sha256"] = sha256_file(
                    staged["l2_snapshot"])
            for i, ds_raw in enumerate(raw["datasets"]):
                ds_raw["candles"]["sha256"] = sha256_file(staged[f"candles:{i}"])
                if ds_raw.get("funding"):
                    ds_raw["funding"]["sha256"] = sha256_file(staged[f"funding:{i}"])
            staged_manifest = os.path.join(staging, "manifest.json")
            with open(staged_manifest, "w") as fh:
                json.dump(raw, fh, indent=2)
                fh.write("\n")
            moves.append((staged_manifest, manifest["path"]))
        try:
            _commit_staged(moves, staging)
        except ManifestError:
            keep_staging = True
            raise
    finally:
        if not keep_staging:
            shutil.rmtree(staging, ignore_errors=True)
    return report


def verify_report(manifest: dict) -> dict:
    out = {"study": manifest["study"], "provenance": manifest["provenance"], "datasets": {}}
    for ds in manifest["datasets"]:
        candles = load_candles(ds)
        out["datasets"][ds["key"]] = {
            "candles": len(candles),
            "first": str(candles.index[0]),
            "last": str(candles.index[-1]),
            "windows": {},
        }
        funding = load_funding(ds)
        for name, win in manifest["windows"].items():
            _, _, cov = window_frame(manifest, ds, name)
            out["datasets"][ds["key"]]["windows"][name] = {
                "candles": cov,
                "funding": funding_coverage(funding, win["start_ms"], win["end_ms"]),
            }
    return out


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Frozen offline candle manifest: verify or acquire (#1649)")
    sub = p.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify", help="Verify hashes, warm-up and coverage offline (no network)")
    v.add_argument("--manifest", required=True)
    a = sub.add_parser("acquire", help="Fetch public venue data into the manifest's paths (network)")
    a.add_argument("--manifest", required=True)
    a.add_argument("--write-hashes", action="store_true",
                   help="Record the new files' sha256 values in the manifest")
    a.add_argument("--overwrite", action="store_true",
                   help="Replace existing frozen inputs. Without it, acquire refuses when any "
                        "target file exists. Files are staged and moved into place with their "
                        "hashes only after every fetch succeeds.")
    args = p.parse_args(argv)
    try:
        if args.cmd == "verify":
            print(json.dumps(verify_report(load_manifest(args.manifest)), indent=2, default=str))
        else:
            print(json.dumps(acquire(args.manifest, write_hashes=args.write_hashes,
                                     overwrite=args.overwrite), indent=2, default=str))
    except ManifestError as exc:
        print(f"manifest error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
