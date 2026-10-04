import hashlib
import json

import numpy as np
import pandas as pd
import pytest

import eval_windows
import observation_replay as orp
import offline_manifest as om
import run_backtest
from atr import ensure_atr_indicator
from backtester import Backtester
from registry_loader import load_registry

NAME = "open_interest_breakout"
HOUR = 3_600_000
MINUTE = 60_000
T0 = 1_767_225_600_000
PARAMS = {"price_lookback": 3, "oi_lookback": 2, "oi_change_threshold": 0.01}


def _sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _series(coin, start_ms, end_ms, value_at, gaps=()):
    samples = []
    seq = 0
    for t in range(start_ms + MINUTE // 2, end_ms, MINUTE):
        seq += 1
        samples.append({"recv_ms": t, "event_ms": None, "value": float(value_at(t)), "session": 1, "seq": seq})
    return orp.build_series(coin, MINUTE, samples, [dict(g) for g in gaps], {"run_id": "fixture", "segments": []})


def _candles(n, breakouts=()):
    close = np.full(n, 100.0)
    for i, c in breakouts:
        close[i] = c
    open_ = np.concatenate([[close[0]], close[:-1]])
    ts = T0 + HOUR * np.arange(n)
    return pd.DataFrame({"timestamp": ts, "open": open_, "high": np.maximum(open_, close) + 0.5,
                         "low": np.minimum(open_, close) - 0.5, "close": close, "volume": np.ones(n)})


def _build(tmp_path, schema=None, with_oi=True, extra_key=None, n=200, oi_start=None):
    data = tmp_path / "data"
    data.mkdir()
    candles = _candles(n, breakouts=[(150, 110.0), (151, 111.0), (170, 90.0), (171, 89.0)])
    om._write_csv_gz(candles, str(data / "BTC.csv.gz"))
    (data / "meta.json").write_text(json.dumps({"universe": [{"name": "BTC", "szDecimals": 5}]}))
    start = T0 + HOUR * (oi_start if oi_start is not None else 0)
    series = _series("BTC", start, T0 + HOUR * n, lambda t: 1000.0 * 1.01 ** ((t - T0) / HOUR))
    orp.write_series(series, str(data / "BTC_open_interest.json.gz"))
    quote = "Order MinTradeNtl Order must have minimum value of $10."
    ds = {"coin": "BTC", "size_decimals": 5, "half_spread_bps": 0.5,
          "candles": {"path": "data/BTC.csv.gz", "sha256": _sha(data / "BTC.csv.gz")}}
    if with_oi:
        ds["open_interest"] = {"path": "data/BTC_open_interest.json.gz",
                               "sha256": _sha(data / "BTC_open_interest.json.gz")}
    if extra_key:
        ds[extra_key] = 1
    manifest = {
        "schema": schema or om.SCHEMA_V2, "study": "fixture", "venue": "hyperliquid",
        "provenance": {"kind": "venue", "label": "fixture candles"},
        "interval": "1h", "warmup_bars": 40,
        "windows": {"train": {"start": pd.Timestamp(T0 + HOUR * 60, unit="ms").isoformat(),
                              "end": pd.Timestamp(T0 + HOUR * 130, unit="ms").isoformat()},
                    "test": {"start": pd.Timestamp(T0 + HOUR * 130, unit="ms").isoformat(),
                             "end": pd.Timestamp(T0 + HOUR * 200, unit="ms").isoformat()}},
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0,
                  "min_notional_usd": 10.0, "min_notional_margin": 0.03},
        "venue_reference": {
            "meta": {"path": "data/meta.json", "sha256": _sha(data / "meta.json")},
            "documents": [{"url": "https://example.invalid", "quote": quote,
                           "quote_sha256": hashlib.sha256(quote.encode()).hexdigest()}]},
        "datasets": [ds],
    }
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(manifest))
    return path


def test_v1_manifest_refuses_an_open_interest_key_and_v2_refuses_unknown_keys(tmp_path):
    v1 = tmp_path / "v1"
    v1.mkdir()
    with pytest.raises(om.ManifestError, match="open_interest needs schema"):
        om.load_manifest(str(_build(v1, schema=om.SCHEMA)))
    v2 = tmp_path / "v2"
    v2.mkdir()
    with pytest.raises(om.ManifestError, match="unknown keys"):
        om.load_manifest(str(_build(v2, extra_key="open_interest_usd")))


def test_open_interest_series_is_hash_pinned(tmp_path):
    path = _build(tmp_path)
    m = om.load_manifest(str(path))
    assert m["schema"] == om.SCHEMA_V2 and m["datasets"][0]["open_interest"] is not None
    with open(tmp_path / "data" / "BTC_open_interest.json.gz", "ab") as fh:
        fh.write(b"x")
    with pytest.raises(om.ManifestError, match="sha256 mismatch"):
        om.load_manifest(str(path))


def test_series_sample_hash_detects_edited_samples(tmp_path):
    series = _series("BTC", T0, T0 + 3 * HOUR, lambda t: 1000.0)
    series["samples"][5]["value"] = 999.0
    target = tmp_path / "edited.json.gz"
    orp.write_series(series, str(target))
    with pytest.raises(orp.RecordingError, match="samples_sha256"):
        orp.read_series(str(target))


def _obs_record(recv, session, seq, value, refreshed=False):
    raw = json.dumps({"channel": "activeAssetCtx",
                      "data": {"coin": "BTC", "ctx": {"openInterest": repr(value)}}})
    return {"k": "obs", "coin": "BTC", "kind": "open_interest", "recv_ms": recv, "session": session,
            "raw": raw, "accepted": True, "value": value, "seq": seq, "refreshed": refreshed, "event_ms": None}


def _write_run(tmp_path, records, closed=True):
    lines = [{"k": "header", "schema": orp.RECORDING_SCHEMA, "run_id": "r", "index": 1, "recv_ms": records[0]["recv_ms"],
              "source": orp.ACCEPTED_SOURCE, "units": "base", "time_basis": "receipt", "cadence_ms": MINUTE,
              "coins": ["BTC"]}]
    lines += records
    lines.append({"k": "end", "recv_ms": records[-1]["recv_ms"], "records": len(records), "dropped": 0})
    blob = "".join(json.dumps(x, sort_keys=True) + "\n" for x in lines).encode()
    (tmp_path / "segment-00001.jsonl").write_bytes(blob)
    seg = {"schema": orp.SEGMENT_SCHEMA, "run_id": "r", "index": 1, "file": "segment-00001.jsonl",
           "sha256": hashlib.sha256(blob).hexdigest(), "records": len(records)}
    (tmp_path / "segment-00001.jsonl.manifest.json").write_text(json.dumps(seg))
    run = {"schema": orp.RUN_SCHEMA, "run_id": "r", "closed": closed, "source": orp.ACCEPTED_SOURCE,
           "units": "base", "time_basis": "receipt", "cadence_ms": MINUTE, "coins": ["BTC"], "segments": [seg]}
    (tmp_path / "run.manifest.json").write_text(json.dumps(run))
    return tmp_path


def test_importer_closes_a_disconnect_gap_even_after_an_overflow_marker(tmp_path):
    t = T0
    records = [{"k": "conn", "state": "connected", "recv_ms": t, "session": 1},
               _obs_record(t + 1_000, 1, 1, 100.0),
               _obs_record(t + 61_000, 1, 2, 101.0),
               {"k": "conn", "state": "disconnected", "recv_ms": t + 90_000, "session": 1},
               {"k": "drop", "count": 3, "from_ms": t + 91_000, "to_ms": t + 92_000, "recv_ms": t + 93_000, "session": 1},
               {"k": "conn", "state": "connected", "recv_ms": t + 95_000, "session": 2},
               _obs_record(t + 125_000, 2, 1, 102.0)]
    series = orp.load_recording(str(_write_run(tmp_path, records)))["series"]["BTC"]
    gaps = {g["reason"]: g for g in series["gaps"]}
    assert gaps["disconnected"]["start_ms"] == t + 61_000
    assert gaps["disconnected"]["detected_ms"] == t + 90_000
    assert gaps["disconnected"]["end_ms"] == t + 125_000
    assert gaps["recorder_overflow"]["start_ms"] == gaps["recorder_overflow"]["detected_ms"] == t + 91_000
    assert [s["value"] for s in series["samples"]] == [100.0, 101.0, 102.0]


def test_importer_refuses_an_unclosed_run_and_a_wrong_refresh_flag(tmp_path):
    t = T0
    base = [{"k": "conn", "state": "connected", "recv_ms": t, "session": 1}, _obs_record(t + 1_000, 1, 1, 100.0)]
    (tmp_path / "a").mkdir()
    (tmp_path / "b").mkdir()
    with pytest.raises(orp.RecordingError, match="not closed"):
        orp.load_recording(str(_write_run(tmp_path / "a", base, closed=False)))
    flagged = base + [_obs_record(t + 2_000, 1, 2, 100.5, refreshed=False)]
    with pytest.raises(orp.RecordingError, match="refresh flag"):
        orp.load_recording(str(_write_run(tmp_path / "b", flagged)))


def test_manifest_leg_runs_the_candidate_with_attached_open_interest(tmp_path):
    m = om.load_manifest(str(_build(tmp_path)))
    reg = load_registry("futures")
    ctx = {"manifest": m, "window": "test", "cost_multiplier": 1.0}
    close = [{"name": "time_stop", "params": {"max_bars": 3}}]
    leg = eval_windows.run_leg(reg, NAME, PARAMS, "BTC", "1h", (None, None), close_strategies=close,
                               direction="both", stop_loss_atr_mult=1.0, manifest_ctx=ctx, keep_trades=True)
    again = eval_windows.run_leg(reg, NAME, PARAMS, "BTC", "1h", (None, None), close_strategies=close,
                                 direction="both", stop_loss_atr_mult=1.0, manifest_ctx=ctx, keep_trades=True)
    assert leg == again
    cov = leg["manifest"]["open_interest_coverage"]
    assert cov["available"] and cov["bucket_coverage"] == 1.0
    assert leg["manifest"]["open_interest_sha256"] == m["datasets"][0]["open_interest"]["sha256"]
    assert leg["execution"]["long_positions"] == 1 and leg["execution"]["short_positions"] == 1


def test_manifest_leg_without_coverage_holds_and_non_manifest_paths_refuse(tmp_path):
    late = tmp_path / "late"
    late.mkdir()
    m = om.load_manifest(str(_build(late, oi_start=190)))
    reg = load_registry("futures")
    ctx = {"manifest": m, "window": "test", "cost_multiplier": 1.0}
    close = [{"name": "time_stop", "params": {"max_bars": 3}}]
    leg = eval_windows.run_leg(reg, NAME, PARAMS, "BTC", "1h", (None, None), close_strategies=close,
                               direction="both", stop_loss_atr_mult=1.0, manifest_ctx=ctx)
    assert leg["execution"]["positions"] == 0
    assert leg["manifest"]["open_interest_coverage"]["bucket_coverage"] < 0.2

    bare = tmp_path / "bare"
    bare.mkdir()
    m2 = om.load_manifest(str(_build(bare, with_oi=False)))
    with pytest.raises(ValueError, match="attaches no open_interest"):
        eval_windows.run_leg(reg, NAME, PARAMS, "BTC", "1h", (None, None), close_strategies=close,
                             direction="both", manifest_ctx={"manifest": m2, "window": "test"})
    with pytest.raises(ValueError, match="only the --manifest path"):
        eval_windows.run_leg(reg, NAME, PARAMS, "BTC/USDT", "1h", ("2026-01-01", None))
    with pytest.raises(SystemExit, match="only with --manifest"):
        run_backtest.run_single_backtest(strategy_name=NAME, registry="futures")
    with pytest.raises(SystemExit, match="attaches no open_interest"):
        run_backtest.run_single_backtest(strategy_name=NAME, registry="futures",
                                         close_strategies=close, manifest_path=str(bare / "manifest.json"),
                                         manifest_dataset="BTC 1h", manifest_window="test")


def test_entry_fills_next_bar_open_and_close_survives_missing_open_interest():
    candles = _candles(30, breakouts=[(20, 110.0)])
    candles["open"] = candles["close"].shift(1).fillna(100.0)
    candles.loc[21:, ["open", "close"]] = 111.0
    candles["high"] = candles[["open", "close"]].max(axis=1) + 0.5
    candles["low"] = candles[["open", "close"]].min(axis=1) - 0.5
    df = candles.set_index(pd.to_datetime(candles["timestamp"], unit="ms"))
    obs = orp.for_bars(_series("BTC", T0 - HOUR, T0 + 23 * HOUR,
                               lambda t: 1000.0 * 1.01 ** ((t - T0) / HOUR)), HOUR, HOUR)
    reg = load_registry("futures")
    signals = ensure_atr_indicator(reg.apply_strategy(NAME, df, {**PARAMS, "open_interest_observations": obs}))
    assert signals["signal"].iloc[20] == 1
    assert not signals["oib_oi_valid"].iloc[23:].any()
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": PARAMS},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                    stop_loss_atr_mult=1.0, direction="both")
    res = bt.run(signals, strategy_name=NAME, symbol="BTC", timeframe="1h", params=PARAMS, save=False)
    trade = res["trades"][0]
    assert pd.Timestamp(trade["entry_date"]) == df.index[21]
    assert trade["entry_price"] == pytest.approx(111.0, rel=0.01)
    assert pd.Timestamp(trade["exit_date"]) == df.index[24]
    assert len(res["trades"]) == 1
