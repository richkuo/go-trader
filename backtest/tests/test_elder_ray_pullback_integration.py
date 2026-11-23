import contextlib
import gzip
import importlib.util
import json
import os
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import offline_manifest as om
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "elder_ray_pullback"
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "elder_ray_pullback_1648")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "elder_ray_pullback_1648_manifest.json")
SEED = os.path.join(STUDY_DIR, "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
HORIZON = 64
DEFAULTS = {"ema_period": 13, "slope_span": 3, "pullback_window": 8}
DOMAIN_ENDPOINTS = [
    {"ema_period": e, "slope_span": s, "pullback_window": w}
    for e in (2, 32) for s in (1, 8) for w in (1, 16)
]
DECISION_COLUMNS = [
    "erp_ema", "erp_ema_prior", "erp_slope", "erp_bull_power", "erp_bear_power", "erp_setup_side",
    "erp_setup_age", "erp_ema_valid", "erp_slope_valid", "erp_valid", "erp_reason", "erp_reason_code", "signal",
]


def _reg():
    return load_registry("futures")


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(params):
    return HORIZON + params["slope_span"] + params["pullback_window"] + 1


def _walk(n, seed, start=100.0, freq="4h", vol=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, vol, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return df


def _frame(close, low, high, open_=None):
    close = np.asarray(close, dtype=float)
    df = pd.DataFrame({"open": close if open_ is None else np.asarray(open_, dtype=float),
                       "high": np.asarray(high, dtype=float), "low": np.asarray(low, dtype=float),
                       "close": close, "volume": np.full(len(close), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(close), freq="4h")
    return df


def _trend_frame(n, pullbacks, *, rising=True, deep=10.0, shallow=1.0):
    close = np.array([100.0 + i if rising else 1000.0 - i for i in range(n)])
    if rising:
        low = close - shallow
        high = close + 1.0
        for start, length in pullbacks:
            low[start:start + length] = close[start:start + length] - deep
    else:
        high = close + shallow
        low = close - 1.0
        for start, length in pullbacks:
            high[start:start + length] = close[start:start + length] + deep
    return _frame(close, low, high)


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["erp_valid"].iloc[-1] == 0.0
    assert out["erp_reason"].iloc[-1] == reason, out["erp_reason"].iloc[-1]
    numeric = out.drop(columns=["erp_reason"]).filter(like="erp_")
    assert np.isfinite(numeric.to_numpy(dtype=float)).all()
    return out


P = {"ema_period": 8, "slope_span": 2, "pullback_window": 4}


def _shift_index(df, pos, delta):
    idx = list(df.index)
    idx[pos] = idx[pos] + delta
    return df.set_axis(pd.DatetimeIndex(idx))


INVALID_ENTRY_DATA = [
    (lambda df: df.drop(columns=["volume"]), "missing_columns"),
    (lambda df: df.drop(columns=["open", "high"]), "missing_columns"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [pd.NaT])), "missing_timestamps"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], np.nan]), "nonfinite_input"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(close=df["close"].astype(object).where(df.index != df.index[-1], "x")), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-2]])), "timestamp_order"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-3]])), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "irregular_cadence"),
    (lambda df: _shift_index(df, len(df) - 30, pd.Timedelta(hours=1)), "irregular_cadence"),
    (lambda df: df.iloc[-(_need(P) - 1):], "insufficient_history"),
    (lambda df: df.iloc[:0], None),
]


def _a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(400, 5)
    bad = 200
    need = _need(P)
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, P)
    clean = _apply(df, P)
    assert (out["erp_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["erp_valid"].iloc[bad:bad + need] == 0.0).all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[:bad], clean[DECISION_COLUMNS].iloc[:bad])
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[bad + need:], clean[DECISION_COLUMNS].iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, P)
    assert (out["erp_reason"].iloc[bad:bad + need - 1] == "irregular_cadence").all()
    assert out["erp_valid"].iloc[bad + need - 1] == 1.0


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(dict(DEFAULTS), specs)] + DOMAIN_ENDPOINTS


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


def _load_study():
    path = os.path.join(STUDY_DIR, "run_study.py")
    spec = importlib.util.spec_from_file_location("_erp_run_study", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _rewrite_gz_csv(src, dst, keep):
    with gzip.open(src, "rt") as fh:
        lines = fh.read().splitlines()
    kept = [lines[0]] + [line for i, line in enumerate(lines[1:]) if keep(i, line)]
    with open(dst, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
            fh.write(("\n".join(kept) + "\n").encode("utf-8"))


def test_frozen_manifest_gate_is_complete_and_missing_funding_refuses_it_with_an_inconclusive_verdict(
        tmp_path, subtests):
    with subtests.test(case="frozen_manifest_complete"):
        _frozen_manifest_gate_is_complete_and_hashes_verify()
    with subtests.test(case="missing_funding"):
        _missing_funding_refuses_the_coverage_gate_and_gives_an_inconclusive_verdict(tmp_path)


def _missing_funding_refuses_the_coverage_gate_and_gives_an_inconclusive_verdict(tmp_path):
    study = _load_study()
    with open(MANIFEST) as fh:
        raw = json.load(fh)
    base = os.path.dirname(MANIFEST)
    refs = [raw["venue_reference"]["meta"], raw["venue_reference"]["l2_snapshot"]]
    refs += [ds[key] for ds in raw["datasets"] for key in ("candles", "funding")]
    for ref in refs:
        dst = tmp_path / ref["path"]
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_bytes(open(os.path.join(base, ref["path"]), "rb").read())
    eth = next(d for d in raw["datasets"] if d["coin"] == "ETH")
    test_start = int(pd.Timestamp(raw["windows"]["test"]["start"], tz="UTC").timestamp() * 1000)
    gapped = tmp_path / eth["funding"]["path"]

    def keep(_, line):
        ts = int(float(line.split(",")[0]))
        return not (test_start + 10 * 3_600_000 <= ts < test_start + 13 * 3_600_000)

    _rewrite_gz_csv(os.path.join(base, eth["funding"]["path"]), str(gapped), keep)
    eth["funding"]["sha256"] = om.sha256_file(str(gapped))
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(raw))
    manifest = om.load_manifest(str(path))
    gate = study.preflight(manifest)
    assert gate["complete"] is False
    refused = {(r["dataset"], r["window"]) for r in gate["refused"]}
    assert refused == {("ETH 4h", "test")}
    assert any("funding" in p for r in gate["refused"] for p in r["problems"])
    result = study.run(str(path))
    assert result["verdict"]["outcome"] == "inconclusive"
    assert "coverage gate refused ETH 4h test" in result["verdict"]["reason"]
    assert "arms" not in result


def _frozen_manifest_gate_is_complete_and_hashes_verify():
    study = _load_study()
    manifest = om.load_manifest(MANIFEST)
    gate = study.preflight(manifest)
    assert gate["complete"], gate["refused"]
    assert len(gate["windows"]) == 6
