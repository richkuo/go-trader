import gzip
import hashlib
import json
import os
import sys

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
import data_fetcher
import eval_windows
import hurst_gate
import run_backtest
from backtester import Backtester
from registry_loader import load_registry

HOUR = 3_600_000
T0 = 1_767_225_600_000


def _spec(**kw):
    base = {"taker_fee_pct": 0.0, "maker_fee_pct": 0.0, "half_spread_pct": 0.0,
            "slippage_pct": 0.0, "size_decimals": 2, "min_notional_usd": 10.0,
            "min_notional_margin": 0.0}
    base.update(kw)
    return base


def _engine_df(prices, actions=None, fractions=None):
    n = len(prices)
    px = np.asarray(prices, dtype=float)
    df = pd.DataFrame({"open": px, "high": px, "low": px, "close": px,
                       "volume": np.ones(n)},
                      index=pd.date_range("2026-01-01", periods=n, freq="1h"))
    df["open_action"] = list(actions) if actions is not None else ["none"] * n
    if fractions is not None:
        df["close_fraction"] = list(fractions)
    return df


def _run(df, spec, capital=1000.0, **kw):
    bt = Backtester(initial_capital=capital, platform="hyperliquid",
                    execution_spec=spec, **kw)
    return bt.run(df, strategy_name="engine", symbol="BTC/USDT", timeframe="1h", save=False)


def test_exact_lot_entry_and_fee_accounting():
    df = _engine_df([100.0] * 5, ["long"] + ["none"] * 4)
    res = _run(df, _spec())
    assert res["trades"][0]["shares"] == 10.0
    assert res["final_capital"] == 1000.0
    assert res["execution"]["rejected_entry_count"] == 0

    taker = 0.00045
    res = _run(df, _spec(taker_fee_pct=taker))
    t = res["trades"][0]
    assert t["shares"] == 9.99
    assert t["entry_fee"] == pytest.approx(9.99 * 100 * taker, abs=1e-6)
    assert t["exit_fee"] == pytest.approx(9.99 * 100 * taker, abs=1e-6)
    assert res["final_capital"] == pytest.approx(1000.0 - 2 * 9.99 * 100 * taker, abs=0.01)
    assert res["execution"]["entry_lot_residual_qty"] == pytest.approx(1000 / (1 + taker) / 100 - 9.99)


def test_short_entry_fee_and_cash_are_consistent():
    df = _engine_df([100.0, 100.0, 90.0, 90.0], ["short", "none", "none", "none"])
    res = _run(df, _spec(taker_fee_pct=0.001))
    t = res["trades"][0]
    assert t["side"] == "short"
    qty = t["shares"]
    assert qty == 9.99
    expected = 1000.0 + qty * (100.0 - 90.0) - qty * 100.0 * 0.001 - qty * 90.0 * 0.001
    assert res["final_capital"] == pytest.approx(expected, abs=0.01)


def test_below_lot_and_min_notional_entries_rejected_and_recorded():
    df = _engine_df([100.0] * 4, ["long", "none", "none", "none"])
    res = _run(df, _spec(size_decimals=0), capital=50.0)
    assert res["trades"] == []
    assert res["execution"]["rejected_entries"][0]["reason"] == "below_lot"

    margin = {"size_decimals": 4, "min_notional_margin": 0.03}
    below = _run(df, _spec(**margin), capital=10.29)
    assert below["trades"] == []
    assert below["execution"]["rejected_entries"][0]["reason"] == "below_min_notional"
    assert below["execution"]["rejected_entries"][0]["threshold_usd"] == pytest.approx(10.3)
    above = _run(df, _spec(**margin), capital=10.31)
    assert above["trades"][0]["shares"] == 0.1031


def test_partial_close_lot_floor_min_gate_and_full_close_below_minimum():
    prices = [100.0] * 6 + [1.5] * 5
    fractions = [0, 0.05, 0, 0.55, 0, 0, 0.5, 0, 1.0, 0, 0]
    df = _engine_df(prices, ["long"] + ["none"] * 10, fractions)
    res = _run(df, _spec(size_decimals=0, min_notional_margin=0.03))
    ex = res["execution"]
    gates = [(s["gate"], s["requested_qty"], s["floored_qty"]) for s in ex["skipped_partial_closes"]]
    assert gates[0] == ("below_lot", pytest.approx(0.5), 0.0)
    assert gates[1][0] == "below_min_notional" and gates[1][2] == 2.0
    assert ex["close_residuals"][0]["residual_qty"] == pytest.approx(0.5)
    shares = [t["shares"] for t in res["trades"]]
    assert shares == [5.0, 5.0]
    assert res["trades"][1]["exit_price"] == 1.5
    assert res["trades"][1]["exit_reason"] != "end_of_data"


def test_resting_take_profit_uses_maker_fee():
    prices = [100.0, 100.0, 100.0, 103.0, 103.0]
    df = _engine_df(prices, ["long"] + ["none"] * 4)
    df["atr"] = 1.0
    res = _run(df, _spec(taker_fee_pct=0.0005, maker_fee_pct=0.0001),
               close_strategies=[{"name": "tiered_tp_atr",
                                  "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}])
    t = res["trades"][0]
    assert t["exit_price"] == pytest.approx(102.0)
    assert t["exit_fee"] == pytest.approx(t["shares"] * 102.0 * 0.0001)
    assert t["entry_fee"] == pytest.approx(t["shares"] * 100.0 * 0.0005)


def test_spec_guards():
    df = _engine_df([100.0] * 3, ["long", "none", "none"])
    with pytest.raises(ValueError, match="charged twice"):
        Backtester(execution_spec=_spec(), commission_pct=0.001)
    with pytest.raises(ValueError, match="charged twice"):
        Backtester(execution_spec=_spec(), slippage_pct=0.001)
    with pytest.raises(ValueError, match="keys must be exactly"):
        Backtester(execution_spec={**_spec(), "lot": 1})
    with pytest.raises(ValueError, match="integer"):
        Backtester(execution_spec=_spec(size_decimals=1.5))
    plain = df.drop(columns=["open_action"]).assign(signal=[1, 0, 0])
    with pytest.raises(ValueError, match="open/close engine"):
        _run(plain, _spec())
    with pytest.raises(ValueError, match="scale-in"):
        _run(df, _spec(), allow_scale_in=True)


def _sha(path):
    return om.sha256_file(path)


def _build_manifest(tmp_path, n_bars=400, funding_drop=(), funding=True, meta_dec=5,
                    windows=None):
    data = tmp_path / "data"
    data.mkdir()
    rng = np.random.RandomState(3)
    close = 100 + np.cumsum(rng.randn(n_bars) * 0.8)
    open_ = np.concatenate([[close[0]], close[:-1]])
    high = np.maximum(open_, close) + 0.5
    low = np.minimum(open_, close) - 0.5
    ts = T0 + HOUR * np.arange(n_bars)
    candles = pd.DataFrame({"timestamp": ts, "open": open_, "high": high, "low": low,
                            "close": close, "volume": 10 + rng.rand(n_bars)})
    om._write_csv_gz(candles, str(data / "BTC.csv.gz"))
    rates = pd.DataFrame({"timestamp": ts + 1, "rate": np.full(n_bars, 0.0001)})
    rates = rates.drop(index=list(funding_drop)).reset_index(drop=True)
    om._write_csv_gz(rates, str(data / "BTC_f.csv.gz"))
    (data / "meta.json").write_text(json.dumps({"universe": [{"name": "BTC", "szDecimals": meta_dec}]}))
    quote = "Order MinTradeNtl Order must have minimum value of $10."
    windows = windows or {
        "train": {"start": pd.Timestamp(T0 + HOUR * 100, unit="ms").isoformat(),
                  "end": pd.Timestamp(T0 + HOUR * 250, unit="ms").isoformat()},
        "test": {"start": pd.Timestamp(T0 + HOUR * 250, unit="ms").isoformat(),
                 "end": pd.Timestamp(T0 + HOUR * 400, unit="ms").isoformat()},
    }
    manifest = {
        "schema": om.SCHEMA, "study": "fixture", "venue": "hyperliquid",
        "provenance": {"kind": "venue", "label": "fixture candles"},
        "interval": "1h", "warmup_bars": 60, "windows": windows,
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0,
                  "min_notional_usd": 10.0, "min_notional_margin": 0.03},
        "venue_reference": {
            "meta": {"path": "data/meta.json", "sha256": _sha(data / "meta.json")},
            "documents": [{"url": "https://example.invalid", "quote": quote,
                           "quote_sha256": hashlib.sha256(quote.encode()).hexdigest()}],
        },
        "datasets": [{"coin": "BTC", "size_decimals": 5, "half_spread_bps": 0.5,
                      "candles": {"path": "data/BTC.csv.gz", "sha256": _sha(data / "BTC.csv.gz")},
                      "funding": ({"path": "data/BTC_f.csv.gz", "sha256": _sha(data / "BTC_f.csv.gz")}
                                  if funding else None)}],
    }
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(manifest))
    return path


def test_manifest_loads_and_reports_complete_coverage(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    report = om.verify_report(m)
    win = report["datasets"]["BTC 1h"]["windows"]["train"]
    assert win["candles"]["complete"] and win["candles"]["present_bars"] == 150
    assert win["funding"]["complete"] and win["funding"]["present_hours"] == 150


@pytest.mark.parametrize("damage,match", [
    ("tamper", "sha256 mismatch"),
    ("missing", "is missing"),
    ("escape", "escapes"),
    ("quote", "quote hash mismatch"),
])
def test_manifest_fails_closed_on_bad_inputs(tmp_path, damage, match):
    path = _build_manifest(tmp_path)
    raw = json.loads(path.read_text())
    if damage == "tamper":
        with open(tmp_path / "data" / "BTC.csv.gz", "ab") as fh:
            fh.write(b"x")
    elif damage == "missing":
        os.remove(tmp_path / "data" / "BTC_f.csv.gz")
    elif damage == "escape":
        raw["datasets"][0]["candles"]["path"] = "../outside.csv.gz"
    else:
        raw["venue_reference"]["documents"][0]["quote"] += " edited"
    path.write_text(json.dumps(raw))
    with pytest.raises(om.ManifestError, match=match):
        om.load_manifest(str(path))


def test_manifest_lot_size_must_match_pinned_meta(tmp_path):
    with pytest.raises(om.ManifestError, match="pinned venue meta"):
        om.load_manifest(str(_build_manifest(tmp_path, meta_dec=4)))


def test_insufficient_warmup_fails_closed(tmp_path):
    windows = {"early": {"start": pd.Timestamp(T0 + HOUR * 30, unit="ms").isoformat(),
                         "end": pd.Timestamp(T0 + HOUR * 200, unit="ms").isoformat()}}
    m = om.load_manifest(str(_build_manifest(tmp_path, windows=windows)))
    with pytest.raises(om.ManifestError, match="warm-up"):
        om.window_frame(m, m["datasets"][0], "early")


def test_funding_gaps_and_absence_are_never_verified_zero(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path, funding_drop=(120, 121, 122))))
    ds = m["datasets"][0]
    frame, win, _ = om.window_frame(m, ds, "train")
    _, cov = om.attach_funding_cost(frame, ds, win)
    assert not cov["complete"] and cov["missing_hours"] == 3 and cov["max_gap_hours"] == 3

    other = tmp_path / "nofunding"
    other.mkdir()
    m2 = om.load_manifest(str(_build_manifest(other, funding=False)))
    ds2 = m2["datasets"][0]
    frame2, win2, _ = om.window_frame(m2, ds2, "train")
    out, cov2 = om.attach_funding_cost(frame2, ds2, win2)
    assert cov2["available"] is False and cov2["complete"] is False
    assert "funding_accrual" not in out.columns


def test_manifest_funding_charges_long_and_credits_short(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    ds = m["datasets"][0]
    frame, win, _ = om.window_frame(m, ds, "train")
    frame, _ = om.attach_funding_cost(frame, ds, win)
    scored = om.slice_window(frame, win)
    assert np.allclose(scored["funding_accrual"].to_numpy(), 0.0001, rtol=1e-9, atol=0.0)
    for side, sign in (("long", -1), ("short", 1)):
        df = scored.copy()
        df["open_action"] = [side] + ["none"] * (len(df) - 1)
        res = _run(df, _spec(size_decimals=5))
        qty = res["trades"][0]["shares"]
        expected = qty * float((df["close"] * df["funding_accrual"]).iloc[2:].sum())
        assert np.sign(res["total_funding_pnl"]) == sign
        assert abs(res["total_funding_pnl"]) == pytest.approx(expected, abs=1e-4)


def test_eval_windows_manifest_leg_is_offline_and_repeatable(tmp_path, monkeypatch):
    def _no_loader(*a, **k):
        raise AssertionError("manifest path must not call the network loader")
    monkeypatch.setattr(data_fetcher, "load_cached_data", _no_loader)
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    reg = load_registry("futures")
    ctx = {"manifest": m, "window": "test", "cost_multiplier": 1.0}
    close = [{"name": "time_stop", "params": {"max_bars": 5}}]
    leg = eval_windows.run_leg(reg, "breakout", None, "BTC", "1h", (None, None),
                               close_strategies=close, direction="both",
                               stop_loss_atr_mult=1.0, manifest_ctx=ctx,
                               comparison_mode="approximate")
    again = eval_windows.run_leg(reg, "breakout", None, "BTC", "1h", (None, None),
                                 close_strategies=close, direction="both",
                                 stop_loss_atr_mult=1.0, manifest_ctx=ctx,
                                 comparison_mode="approximate")
    assert leg == again
    assert leg["manifest"]["cost_model"] == "execution_spec"
    assert leg["manifest"]["candle_coverage"]["present_bars"] == 150
    assert leg["span_days"] == pytest.approx(149 / 24, abs=1e-3)
    plain = eval_windows.run_leg(reg, "sma_crossover", None, "BTC", "1h", (None, None),
                                 manifest_ctx=ctx)
    assert plain["manifest"]["cost_model"] == "legacy_flat"
    with pytest.raises(ValueError, match="funding as an entry input"):
        eval_windows.run_leg(reg, "funding_skew", None, "BTC", "1h", (None, None),
                             manifest_ctx=ctx)


def _warmup_frames(tmp_path, warmup_bars=240):
    path = _build_manifest(tmp_path)
    raw = json.loads(path.read_text())
    raw["warmup_bars"] = warmup_bars
    path.write_text(json.dumps(raw))
    m = om.load_manifest(str(path))
    full, win, _ = om.window_frame(m, m["datasets"][0], "test")
    full["open_action"] = "none"
    full.loc[om.slice_window(full, win).index[0], "open_action"] = "long"
    return full, om.slice_window(full, win)


def _bt(**kw):
    return Backtester(initial_capital=1000.0, platform="hyperliquid", **kw)


def test_indicator_frame_gives_entry_atr_on_scored_bar_two(tmp_path):
    full, scored = _warmup_frames(tmp_path)
    cut = _bt(stop_loss_atr_mult=1.0).run(scored, save=False)
    warm = _bt(stop_loss_atr_mult=1.0).run(scored, save=False, indicator_frame=full)
    assert cut["trades"] == [] and cut["stop_warmup_skipped_entries"] == 1
    assert warm["trades"][0]["entry_date"] == str(scored.index[1])
    assert warm["trades"][0]["entry_atr"] > 0.0
    assert "stop_warmup_skipped_entries" not in warm


def test_indicator_frame_gives_zscore_and_hurst_on_scored_bar_one(tmp_path, monkeypatch):
    full, scored = _warmup_frames(tmp_path)
    seen = {}
    real_eval = Backtester._evaluate_close_strategies
    real_step = hurst_gate.HurstGate.step

    def spy_eval(self, *a, **k):
        seen.setdefault("z", k.get("zscore_series"))
        return real_eval(self, *a, **k)

    def spy_step(self, h, flat):
        seen.setdefault("h", h)
        return real_step(self, h, flat)

    monkeypatch.setattr(Backtester, "_evaluate_close_strategies", spy_eval)
    monkeypatch.setattr(hurst_gate.HurstGate, "step", spy_step)
    kw = dict(close_strategies=[{"name": "zscore_target", "params": {"lookback": 20}}],
              hurst_gate={"enabled": True, "min": 0.0, "max": 1.0},
              comparison_mode="approximate")
    for ctx, finite in ((None, False), (full, True)):
        seen.clear()
        _bt(**kw).run(scored, save=False, indicator_frame=ctx)
        assert bool(np.isfinite(seen["z"].iloc[0])) is finite
        assert bool(np.isfinite(seen["h"])) is finite


def test_indicator_frame_equal_to_scored_frame_changes_nothing(tmp_path):
    _, scored = _warmup_frames(tmp_path)
    kw = dict(stop_loss_pct=0.05,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 5}}],
              comparison_mode="approximate")
    base = _bt(**kw).run(scored, save=False)
    same = _bt(**kw).run(scored, save=False, indicator_frame=scored)
    assert base["trades"] and base["trades"] == same["trades"]
    assert base["final_capital"] == same["final_capital"]


def test_indicator_frame_must_cover_and_match_the_scored_bars(tmp_path):
    full, scored = _warmup_frames(tmp_path)
    with pytest.raises(ValueError, match="contains every bar"):
        _bt(stop_loss_atr_mult=1.0).run(scored, save=False,
                                         indicator_frame=full.drop(index=scored.index[3]))
    altered = full.copy()
    altered.loc[scored.index[3], "close"] += 1.0
    with pytest.raises(ValueError, match="must match"):
        _bt(stop_loss_atr_mult=1.0).run(scored, save=False, indicator_frame=altered)


def _fake_info(fail_on=None):
    def post(payload, retries=5):
        kind = payload["type"]
        if kind == fail_on:
            raise om.ManifestError(f"fake {kind} failure")
        if kind == "meta":
            return {"universe": [{"name": "BTC", "szDecimals": 5}]}
        req = payload["req"]
        now_ms = int(om.time.time() * 1000)
        out = []
        t = int(req["startTime"])
        while t <= min(int(req["endTime"]), now_ms):
            px = 100.0 + (t - T0) / HOUR * 0.01
            out.append({"t": t, "o": str(px), "h": str(px + 0.5), "l": str(px - 0.5),
                        "c": str(px), "v": "10.0"})
            t += HOUR
        return out
    return post


def _acquire_manifest(tmp_path, monkeypatch, fail_on=None):
    path = _build_manifest(tmp_path, funding=False)
    raw = json.loads(path.read_text())
    raw["acquisition"] = {"start": pd.Timestamp(T0, unit="ms").isoformat(),
                          "end": pd.Timestamp(T0 + HOUR * 400, unit="ms").isoformat()}
    path.write_text(json.dumps(raw))
    monkeypatch.delenv("HYPERLIQUID_SECRET_KEY", raising=False)
    monkeypatch.setattr(om.time, "sleep", lambda s: None)
    monkeypatch.setattr(om, "_post_info", _fake_info(fail_on))
    return path


def _snapshot(root):
    return {str(p.relative_to(root)): p.read_bytes()
            for p in sorted(root.rglob("*")) if p.is_file()}


def test_acquire_refuses_existing_inputs_without_overwrite(tmp_path, monkeypatch):
    path = _acquire_manifest(tmp_path, monkeypatch)
    monkeypatch.setattr(om, "_post_info", lambda *a, **k: pytest.fail("acquire used the network"))
    before = _snapshot(tmp_path)
    with pytest.raises(om.ManifestError, match="without --overwrite"):
        om.acquire(str(path))
    assert _snapshot(tmp_path) == before


@pytest.mark.parametrize("stage", ["fetch", "move"])
def test_failed_acquire_leaves_frozen_inputs_and_hashes_untouched(tmp_path, monkeypatch, stage):
    path = _acquire_manifest(tmp_path, monkeypatch,
                             fail_on="candleSnapshot" if stage == "fetch" else None)
    if stage == "move":
        real_replace = os.replace
        calls = []

        def flaky_replace(src, dst):
            calls.append(dst)
            if len(calls) == 4:
                raise OSError("simulated failure while moving staged files")
            return real_replace(src, dst)

        monkeypatch.setattr(om.os, "replace", flaky_replace)
    before = _snapshot(tmp_path)
    with pytest.raises((om.ManifestError, OSError)):
        om.acquire(str(path), overwrite=True)
    assert _snapshot(tmp_path) == before
    assert not list(tmp_path.glob(".acquire-staging-*"))


def test_acquire_overwrite_replaces_inputs_and_hashes_together(tmp_path, monkeypatch):
    path = _acquire_manifest(tmp_path, monkeypatch)
    before = _snapshot(tmp_path)
    om.acquire(str(path), overwrite=True)
    after = _snapshot(tmp_path)
    assert after["data/BTC.csv.gz"] != before["data/BTC.csv.gz"]
    m = om.load_manifest(str(path))
    assert len(om.load_candles(m["datasets"][0])) == 400
    assert not list(tmp_path.glob(".acquire-staging-*"))


@pytest.mark.parametrize("now_ms,last_kept", [
    (T0 + HOUR * 10 - 1, T0 + HOUR * 8),
    (T0 + HOUR * 10, T0 + HOUR * 9),
    (T0 + HOUR * 10 + 1, T0 + HOUR * 9),
])
def test_fetch_keeps_closed_candles_only(monkeypatch, now_ms, last_kept):
    monkeypatch.setattr(om.time, "sleep", lambda s: None)
    monkeypatch.setattr(om.time, "time", lambda: now_ms / 1000)
    monkeypatch.setattr(om, "_post_info", _fake_info())
    df = om._fetch_hl_candles("BTC", "1h", T0, T0 + HOUR * 100)
    assert int(df["timestamp"].iloc[0]) == T0
    assert int(df["timestamp"].iloc[-1]) == last_kept


@pytest.mark.parametrize("argv", [
    ["--mode", "compare", "--manifest", "manifest.json"],
    ["--mode", "multi", "--manifest-window", "test"],
    ["--mode", "optimize", "--cost-multiplier", "2"],
    ["--mode", "single", "--manifest-window", "test"],
    ["--mode", "single", "--manifest-dataset", "BTC 4h"],
])
def test_manifest_flags_are_refused_before_any_data_load(monkeypatch, argv):
    monkeypatch.setattr(run_backtest, "load_cached_data",
                        lambda *a, **k: pytest.fail("data loaded despite a refused flag"))
    monkeypatch.setattr(sys, "argv", ["run_backtest.py", "--strategy", "sma_crossover", *argv])
    with pytest.raises(SystemExit) as exc:
        run_backtest.main()
    assert exc.value.code not in (0, None)


DAY_MS = 1_704_067_200_000
FIVE_MIN = 300_000
TEN_DAYS = 10 * 86_400_000
ONE_DAY = 86_400_000


def _avail(value):
    return {"provenance": [], "reason": None, "status": "available", "value": value}


class _InfoResp:
    def __init__(self, payload):
        self.status = 200
        self._raw = json.dumps(payload).encode()

    def read(self):
        return self._raw

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


def _ms(minute, second=0, millis=0):
    return DAY_MS + minute * 60_000 + second * 1000 + millis


def _flat_bars(start, end, step, high=99, low=98, close=99):
    return [
        {"t": open_ms, "o": "99", "h": str(high), "l": str(low), "c": str(close), "v": "1"}
        for open_ms in range(int(start), int(end), int(step))
    ]


def _frame_from_candles(candles, high=None, low=None, close=None):
    return pd.DataFrame({
        "timestamp": [int(row["t"]) for row in candles],
        "high": [float(high if high is not None else row["h"]) for row in candles],
        "low": [float(low if low is not None else row["l"]) for row in candles],
        "close": [float(close if close is not None else row["c"]) for row in candles],
    })


def _order_status(order):
    body = {
        "coin": "ETH",
        "limitPx": order["limit"],
        "oid": order["oid"],
        "origSz": order["orig"],
        "reduceOnly": True,
        "side": "A",
        "tif": "Gtc",
        "timestamp": order["placement"],
    }
    if order.get("sz") is not None:
        body["sz"] = order["sz"]
    return {
        "status": "order",
        "order": {
            "order": body,
            "status": order["status"],
            "statusTimestamp": order["status_time"],
        },
    }


def _write_export(path, oids):
    doc = {
        "schema": "go-trader.booked-ledger",
        "events": [
            {
                "process_strategy_id": "strat",
                "position_id": _avail("p1"),
                "is_close": _avail(False),
                "side": _avail("buy"),
                "symbol": _avail("ETH"),
                "timestamp": "2024-01-01T00:00:00Z",
                "tp_oids_json": _avail(json.dumps(oids)),
            },
            {
                "process_strategy_id": "strat",
                "position_id": _avail("p1"),
                "is_close": _avail(True),
                "side": _avail("sell"),
                "symbol": _avail("ETH"),
                "timestamp": "2024-01-01T00:30:00Z",
            },
        ],
    }
    path.write_text(json.dumps(doc))


def _study_orders(tmp_path, orders, candles):
    import resting_tp_capture as cap
    import resting_tp_fill_study as study

    export = tmp_path / "export.json"
    _write_export(export, [order["oid"] for order in orders])
    by_oid = {order["oid"]: _order_status(order) for order in orders}
    fills = []
    for order in orders:
        for fill in order.get("fills") or []:
            row = dict(fill)
            row["oid"] = order["oid"]
            row["coin"] = "ETH"
            fills.append(row)
    since = DAY_MS
    end = _ms(30)

    def opener(req, timeout=None):
        body = json.loads(req.data)
        kind = body["type"]
        if kind == "userFillsByTime":
            return _InfoResp(fills if body["startTime"] == since else [])
        if kind == "historicalOrders":
            return _InfoResp([])
        if kind == "orderStatus":
            return _InfoResp(by_oid[body["oid"]])
        if kind == "recentTrades":
            return _InfoResp([])
        if kind == "candleSnapshot":
            return _InfoResp(candles)
        raise AssertionError(kind)

    out = tmp_path / "capture"
    cap._prepare_out(str(out), os.path.abspath(os.path.join(os.path.dirname(cap.__file__), "..")))
    cap.capture(
        [str(export)], "0x" + "11" * 20, since, end, str(out),
        5, 1, "5m", opener=opener, clock_ms=end)
    manifest = tmp_path / "manifest.json"
    candle_path = tmp_path / "ETH.csv.gz"
    frame = pd.DataFrame({
        "timestamp": [row["t"] for row in candles],
        "open": [float(row["o"]) for row in candles],
        "high": [float(row["h"]) for row in candles],
        "low": [float(row["l"]) for row in candles],
        "close": [float(row["c"]) for row in candles],
        "volume": [1.0] * len(candles),
    })
    with gzip.open(candle_path, "wt") as fh:
        frame.to_csv(fh, index=False)
    meta_path = tmp_path / "meta.json"
    meta_path.write_text(json.dumps({"universe": [{"name": "ETH", "szDecimals": 2}]}))
    manifest_doc = {
        "costs": {
            "maker_fee_pct": 0.00015,
            "min_notional_margin": 0.03,
            "min_notional_usd": 10.0,
            "slippage_bps": 1.0,
            "taker_fee_pct": 0.00045,
        },
        "datasets": [{
            "candles": {"path": "ETH.csv.gz", "sha256": hashlib.sha256(candle_path.read_bytes()).hexdigest()},
            "coin": "ETH",
            "half_spread_bps": 0.5,
            "size_decimals": 2,
            "symbol": "ETH/USDT",
        }],
        "interval": "5m",
        "provenance": {"kind": "proxy", "label": "synthetic bars for the resting take-profit cases"},
        "schema": "offline_candle_manifest/v1",
        "study": "resting_tp_case",
        "venue": "hyperliquid",
        "venue_reference": {
            "meta": {"path": "meta.json", "sha256": hashlib.sha256(meta_path.read_bytes()).hexdigest()},
        },
        "warmup_bars": 0,
        "windows": {"study": {"start": "2024-01-01T00:00:00Z", "end": "2024-01-01T00:30:00Z", "role": "case"}},
    }
    manifest.write_text(json.dumps(manifest_doc))
    digest = hashlib.sha256(manifest.read_bytes()).hexdigest()
    return study.study([str(export)], str(out), str(manifest), digest, "study", "pending")


def _example_candles():
    candles = _flat_bars(DAY_MS, _ms(30), FIVE_MIN)
    for row in candles:
        if row["t"] == _ms(20):
            row["h"] = "100.60"
            row["c"] = "100.55"
    return candles


def test_resting_tp_two_tiers_in_one_bar_agree_on_touch(tmp_path):
    orders = [
        {
            "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "filled",
            "status_time": _ms(22, 30), "orig": "1", "sz": "0",
            "fills": [{"time": _ms(22, 30), "sz": "1", "px": "100.00", "tid": 1, "fee": "0.015"}],
        },
        {
            "oid": 2, "limit": "100.50", "placement": _ms(10), "status": "filled",
            "status_time": _ms(23, 10), "orig": "1", "sz": "0",
            "fills": [{"time": _ms(23, 10), "sz": "1", "px": "100.50", "tid": 2, "fee": "0.015"}],
        },
    ]
    report = _study_orders(tmp_path, orders, _example_candles())
    assert [row["outcome"] for row in report["orders"]] == ["full_fill", "full_fill"]
    touch = report["rules"]["touch"]["long"]
    assert touch["missed_fills"] == 0
    assert touch["agreements"] == 2
    assert touch["false_fills"] == 0


def test_resting_tp_later_sibling_cancel_keeps_the_fill_bar(tmp_path):
    orders = [
        {
            "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "filled",
            "status_time": _ms(22, 30), "orig": "1", "sz": "0",
            "fills": [{"time": _ms(22, 30), "sz": "1", "px": "100.00", "tid": 1}],
        },
        {
            "oid": 2, "limit": "100.50", "placement": _ms(10), "status": "reduceOnlyCanceled",
            "status_time": _ms(22, 30, 50), "orig": "1", "sz": "1", "fills": [],
        },
    ]
    report = _study_orders(tmp_path, orders, _example_candles())
    by_outcome = sorted(row["outcome"] for row in report["orders"])
    assert "full_fill" in by_outcome
    assert report["rules"]["touch"]["long"]["missed_fills"] == 0


def test_resting_tp_sibling_placement_inside_the_fill_bar_stays_boundary():
    import resting_tp_fill_study as study

    filled = study.order_record(1, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 1, "origSz": "1", "sz": "0",
            "side": "A", "timestamp": _ms(10),
        },
        "status": "filled",
        "status_time": _ms(22, 30),
    }, [{"oid": 1, "sz": "1", "px": "100.00", "time": _ms(22, 30), "tid": 1}])
    placed = study.order_record(2, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 2, "origSz": "1",
            "side": "A", "timestamp": _ms(21),
        },
        "status": "open",
    }, [])
    candles = _example_candles()
    frame = _frame_from_candles(candles)
    times = study.sibling_boundary_times(filled, [filled, placed])
    interior, boundary, missing = study.bars_for(
        frame, FIVE_MIN, filled["placement_ms"], study.score_end_ms(filled), times,
        include_end_bar=True)
    assert not missing
    assert _ms(20) in [bar["open_ms"] for bar in boundary]
    assert _ms(20) not in [bar["open_ms"] for bar in interior]


def _classify(status, orig, sz, fills, high=99):
    import resting_tp_fill_study as study

    start = DAY_MS
    end = DAY_MS + 2 * FIVE_MIN
    rec = study.order_record(1, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 1, "origSz": orig, "sz": sz,
            "side": "A", "timestamp": start,
        },
        "status": status,
        "status_time": end,
    }, fills)
    frame = pd.DataFrame({
        "timestamp": [start, start + FIVE_MIN],
        "high": [high, high],
        "low": [98, 98],
        "close": [99, 99],
    })
    windows = [(start, end + FIVE_MIN), (start, end + FIVE_MIN)]
    return study.classify_order(rec, {"side": "long"}, frame, FIVE_MIN, None, 2, [], windows)


def test_resting_tp_canceled_partial_missing_from_capture_is_incomplete():
    import resting_tp_fill_study as study

    assert _classify("reduceOnlyCanceled", "1.0", "0.6", []) == "acquisition_incomplete"
    bare = study.order_record(9, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 9, "sz": "0.4",
            "side": "A", "timestamp": DAY_MS,
        },
        "status": "canceled",
        "status_time": DAY_MS + FIVE_MIN,
    }, [])
    assert bare["orig"] is None
    assert "requested_quantity" in bare["missing"]


def test_resting_tp_canceled_partial_with_captured_fills_stays_partial():
    fills = [{"oid": 1, "sz": "0.4", "px": "100.00", "time": DAY_MS + 1000, "tid": 1}]
    assert _classify("canceled", "1.0", "0.6", fills) == "partial_fill"


def test_resting_tp_canceled_unfilled_order_stays_unreached():
    assert _classify("canceled", "1.0", "1.0", [], high=99) == "not_reached"


def _basis(candles, frame, since, end, candle_rows, snapshot_name="5m"):
    import resting_tp_fill_study as study

    return study.candle_basis(
        {"ETH": {"frame": frame}}, {"ETH": candles}, [], since, end, FIVE_MIN,
        "5m", snapshot_name, candle_rows)[0]


def test_resting_tp_five_minute_snapshot_confirms_a_ten_day_window(tmp_path):
    import resting_tp_capture as cap

    assert set(cap.INTERVAL_MS) == set(om.INTERVAL_MS)
    clock = DAY_MS + 20 * ONE_DAY
    end = clock - ONE_DAY
    since = end - TEN_DAYS
    candles = _flat_bars(since, end, FIVE_MIN, high=100, low=99, close=100)
    export = tmp_path / "export.json"
    _write_export(export, [])

    def opener(req, timeout=None):
        body = json.loads(req.data)
        kind = body["type"]
        if kind == "candleSnapshot":
            return _InfoResp(candles)
        if kind == "userFillsByTime":
            return _InfoResp([])
        return _InfoResp([])

    out = tmp_path / "capture"
    cap._prepare_out(str(out), os.path.abspath(os.path.join(os.path.dirname(cap.__file__), "..")))
    cap.capture(
        [str(export)], "0x" + "22" * 20, since, end, str(out),
        5, 1, "5m", opener=opener, clock_ms=clock)
    bundle = json.loads((out / "bundle.json").read_text())
    row = bundle["completeness"]["candleSnapshot"][0]
    assert bundle["inputs"]["interval"] == "5m"
    assert row["complete"] is True
    assert "reason" not in row
    frame = _frame_from_candles(candles, high=100, low=99, close=100)
    assert _basis(candles, frame, since, end, bundle["completeness"]["candleSnapshot"]) == "confirmed"


def test_resting_tp_snapshot_older_than_five_thousand_bars_is_unconfirmed(tmp_path):
    import resting_tp_capture as cap

    clock = DAY_MS + 20 * ONE_DAY
    since = clock - (cap.CANDLE_HISTORY_BARS + 1) * FIVE_MIN
    end = since + 2 * FIVE_MIN
    export = tmp_path / "export.json"
    _write_export(export, [])
    candles = _flat_bars(since, end, FIVE_MIN, high=100, low=99, close=100)

    def opener(req, timeout=None):
        body = json.loads(req.data)
        if body["type"] == "candleSnapshot":
            return _InfoResp(candles)
        return _InfoResp([])

    out = tmp_path / "capture"
    cap._prepare_out(str(out), os.path.abspath(os.path.join(os.path.dirname(cap.__file__), "..")))
    with pytest.raises(cap.CaptureError):
        cap.capture(
            [str(export)], "0x" + "33" * 20, since, end, str(out),
            5, 1, "5m", opener=opener, clock_ms=clock)
    bundle = json.loads((out / "bundle.json").read_text())
    row = bundle["completeness"]["candleSnapshot"][0]
    assert row["complete"] is False
    assert "5000" in row["reason"]
    frame = _frame_from_candles(candles, high=100, low=99, close=100)
    assert _basis(candles, frame, since, end, bundle["completeness"]["candleSnapshot"]) == "unconfirmed"


def test_resting_tp_snapshot_high_mismatch_is_unconfirmed():
    candles = [{"t": DAY_MS, "h": "100.03", "l": "99", "c": "100", "o": "99"}]
    frame = pd.DataFrame({"timestamp": [DAY_MS], "high": [100.05], "low": [99.0], "close": [100.0]})
    assert _basis(candles, frame, DAY_MS, DAY_MS + FIVE_MIN, [{"complete": True, "interval": "5m"}]) == "unconfirmed"


def test_resting_tp_snapshot_close_mismatch_is_unconfirmed():
    candles = [{"t": DAY_MS, "h": "101", "l": "99", "c": "100.01", "o": "100"}]
    frame = pd.DataFrame({"timestamp": [DAY_MS], "high": [101.0], "low": [99.0], "close": [100.02]})
    assert _basis(candles, frame, DAY_MS, DAY_MS + FIVE_MIN, [{"complete": True, "interval": "5m"}]) == "unconfirmed"


def test_resting_tp_matching_high_low_and_close_confirms():
    candles = [{"t": DAY_MS, "h": "101", "l": "99", "c": "100.02", "o": "100"}]
    frame = pd.DataFrame({"timestamp": [DAY_MS], "high": [101.0], "low": [99.0], "close": [100.02]})
    assert _basis(candles, frame, DAY_MS, DAY_MS + FIVE_MIN, [{"complete": True, "interval": "5m"}]) == "confirmed"


def test_resting_tp_fixture_order_classes_are_pinned():
    import resting_tp_fill_study as study

    root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    base = os.path.join(root, "backtest", "testdata", "resting_tp_study")
    manifest = os.path.join(base, "market", "manifest.json")
    digest = hashlib.sha256(open(manifest, "rb").read()).hexdigest()
    report = study.study(
        [os.path.join(base, "export_a.json"), os.path.join(base, "export_b.json")],
        os.path.join(base, "capture"), manifest, digest, "study", "pending")
    assert [row["outcome"] for row in report["orders"]] == [
        "lifetime_unknown",
        "full_fill",
        "partial_fill",
        "no_fill_touch",
        "no_fill_trade_through",
        "not_reached",
        "boundary_unknown",
        "shared_coin_ambiguous",
        "limit_off_grid",
        "full_fill",
        "acquisition_incomplete",
    ]
    assert report["rules"]["touch"]["long"]["agreements"] == 3
    assert report["rules"]["touch"]["long"]["missed_fills"] == 0
    assert report["candle_price_basis"] == "unconfirmed"


def test_resting_tp_fill_page_overlap_keeps_each_fill_once():
    import resting_tp_capture as cap
    import resting_tp_fill_study as study

    pages = {
        1000: [
            {"tid": 1, "time": 1000, "oid": 1, "sz": "1", "px": "1"},
            {"tid": 2, "time": 2000, "oid": 1, "sz": "1", "px": "1"},
        ],
        2000: [
            {"tid": 2, "time": 2000, "oid": 1, "sz": "1", "px": "1"},
            {"tid": 3, "time": 3000, "oid": 1, "sz": "1", "px": "1"},
        ],
    }
    starts = []

    def opener(req, timeout=None):
        body = json.loads(req.data)
        starts.append(body["startTime"])
        return _InfoResp(pages.get(body["startTime"], []))

    def record(payload, attempt, status, raw):
        return None

    result = cap._page_user_fills(
        "0x" + "ab" * 20, 1000, 10000, 1, 1, record, opener=opener, page_max_rows=10)
    assert starts[1] == 2000
    assert result["complete"] is True
    rows = study.dedupe_fills(pages[1000] + pages[2000])
    assert [row["tid"] for row in rows] == [1, 2, 3]


def _replace_responses(capture_dir, kind, body):
    log = capture_dir / "requests.jsonl"
    rows = []
    replaced = 0
    for line in log.read_text().splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        if row.get("type") == kind:
            target = capture_dir / row["response_file"]
            target.write_bytes(body)
            row["response_sha256"] = hashlib.sha256(body).hexdigest()
            replaced += 1
        rows.append(row)
    assert replaced
    log.write_text("\n".join(json.dumps(row, sort_keys=True) for row in rows) + "\n")


def test_resting_tp_sibling_placed_after_the_fill_keeps_the_fill_bar(tmp_path):
    orders = [
        {
            "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "filled",
            "status_time": _ms(22, 30), "orig": "1", "sz": "0",
            "fills": [{"time": _ms(22, 30), "sz": "1", "px": "100.00", "tid": 1}],
        },
        {
            "oid": 2, "limit": "101.00", "placement": _ms(23), "status": "open",
            "status_time": _ms(23), "orig": "1", "sz": "1", "fills": [],
        },
    ]
    report = _study_orders(tmp_path, orders, _example_candles())
    assert [row["outcome"] for row in report["orders"]] == ["full_fill", "lifetime_unknown"]
    touch = report["rules"]["touch"]["long"]
    assert touch["missed_fills"] == 0
    assert touch["agreements"] == 1
    assert touch["false_fills"] == 0


def test_resting_tp_sibling_placement_keeps_a_no_fill_bar_unknown():
    import resting_tp_fill_study as study

    order = study.order_record(1, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 1, "origSz": "1", "sz": "1",
            "side": "A", "timestamp": _ms(10),
        },
        "status": "expired",
        "status_time": _ms(40),
    }, [])
    sibling = study.order_record(2, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 2, "origSz": "1",
            "side": "A", "timestamp": _ms(23),
        },
        "status": "open",
    }, [])
    candles = _flat_bars(DAY_MS, _ms(45), FIVE_MIN)
    for row in candles:
        if row["t"] == _ms(20):
            row["h"] = "100.60"
    frame = _frame_from_candles(candles)
    windows = [(DAY_MS, _ms(45)), (DAY_MS, _ms(45))]
    name = study.classify_order(
        order, {"side": "long"}, frame, FIVE_MIN, None, 2,
        study.sibling_boundary_times(order, [order, sibling]), windows)
    assert name == "not_reached"
    assert _ms(20) in [bar["open_ms"] for bar in order["boundary"]]
    assert _ms(20) not in [bar["open_ms"] for bar in order["interior"]]


def test_resting_tp_partial_canceled_in_the_fill_bar_is_a_quantity_error(tmp_path):
    orders = [{
        "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "reduceOnlyCanceled",
        "status_time": _ms(23), "orig": "1", "sz": "0.6",
        "fills": [{"time": _ms(22, 30), "sz": "0.4", "px": "100.00", "tid": 1}],
    }]
    report = _study_orders(tmp_path, orders, _example_candles())
    assert [row["outcome"] for row in report["orders"]] == ["partial_fill"]
    touch = report["rules"]["touch"]["long"]
    assert touch["quantity_errors"] == 1
    assert touch["missed_fills"] == 0


def test_resting_tp_cancel_bar_without_a_fill_stays_boundary():
    import resting_tp_fill_study as study

    order = study.order_record(1, {
        "order": {
            "coin": "ETH", "limitPx": "100.00", "oid": 1, "origSz": "1", "sz": "1",
            "side": "A", "timestamp": _ms(10),
        },
        "status": "canceled",
        "status_time": _ms(22, 30),
    }, [])
    frame = _frame_from_candles(_example_candles())
    windows = [(DAY_MS, _ms(30)), (DAY_MS, _ms(30))]
    name = study.classify_order(order, {"side": "long"}, frame, FIVE_MIN, None, 2, [], windows)
    assert name == "not_reached"
    assert _ms(20) in [bar["open_ms"] for bar in order["boundary"]]
    assert _ms(20) not in [bar["open_ms"] for bar in order["interior"]]


def test_resting_tp_html_fill_page_reports_incomplete_capture(tmp_path):
    import resting_tp_fill_study as study

    orders = [{
        "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "canceled",
        "status_time": _ms(22, 30), "orig": "1", "sz": "1", "fills": [],
    }]
    _study_orders(tmp_path, orders, _example_candles())
    capture = tmp_path / "capture"
    _replace_responses(capture, "userFillsByTime", b"<html><body>502 Bad Gateway</body></html>")
    bundle_path = capture / "bundle.json"
    bundle = json.loads(bundle_path.read_text())
    fills = bundle["completeness"]["userFillsByTime"]
    fills["complete"] = False
    fills["uncovered_from_ms"] = DAY_MS
    fills["uncovered_to_ms"] = _ms(30)
    fills["failure"] = "response was not json"
    bundle_path.write_text(json.dumps(bundle))
    manifest = tmp_path / "manifest.json"
    digest = hashlib.sha256(manifest.read_bytes()).hexdigest()
    report = study.study(
        [str(tmp_path / "export.json")], str(capture), str(manifest), digest, "study", "pending")
    assert [row["outcome"] for row in report["orders"]] == ["acquisition_incomplete"]


def test_resting_tp_html_page_on_a_complete_stream_is_an_error(tmp_path):
    import resting_tp_fill_study as study

    orders = [{
        "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "canceled",
        "status_time": _ms(22, 30), "orig": "1", "sz": "1", "fills": [],
    }]
    _study_orders(tmp_path, orders, _example_candles())
    capture = tmp_path / "capture"
    _replace_responses(capture, "userFillsByTime", b"<html><body>502 Bad Gateway</body></html>")
    with pytest.raises(study.StudyError, match="not json"):
        study.load_capture(str(capture))


def test_resting_tp_empty_response_file_has_no_payload(tmp_path):
    import resting_tp_fill_study as study

    orders = [{
        "oid": 1, "limit": "100.00", "placement": _ms(10), "status": "canceled",
        "status_time": _ms(22, 30), "orig": "1", "sz": "1", "fills": [],
    }]
    _study_orders(tmp_path, orders, _example_candles())
    capture = tmp_path / "capture"
    _replace_responses(capture, "recentTrades", b"")
    _bundle, _digest, loaded = study.load_capture(str(capture))
    empty = [item for item in loaded if item["meta"].get("type") == "recentTrades"]
    assert empty
    assert all(item["raw"] == b"" and item["payload"] is None for item in empty)


def test_resting_tp_unaligned_since_is_refused_before_any_request(tmp_path):
    import resting_tp_capture as cap

    export = tmp_path / "export.json"
    _write_export(export, [])
    calls = []

    def opener(req, timeout=None):
        calls.append(req)
        return _InfoResp([])

    out = tmp_path / "capture"
    cap._prepare_out(str(out), os.path.abspath(os.path.join(os.path.dirname(cap.__file__), "..")))
    with pytest.raises(cap.CaptureError, match="boundary"):
        cap.capture(
            [str(export)], "0x" + "44" * 20, _ms(2), _ms(30), str(out),
            5, 1, "5m", opener=opener, clock_ms=_ms(30))
    assert calls == []


def test_resting_tp_aligned_window_missing_one_bar_is_incomplete(tmp_path):
    import resting_tp_capture as cap

    since = DAY_MS
    end = since + 3 * FIVE_MIN
    missing = since + FIVE_MIN
    candles = [row for row in _flat_bars(since, end, FIVE_MIN, high=100, low=99, close=100) if row["t"] != missing]
    export = tmp_path / "export.json"
    _write_export(export, [])

    def opener(req, timeout=None):
        body = json.loads(req.data)
        if body["type"] == "candleSnapshot":
            return _InfoResp(candles)
        return _InfoResp([])

    out = tmp_path / "capture"
    cap._prepare_out(str(out), os.path.abspath(os.path.join(os.path.dirname(cap.__file__), "..")))
    with pytest.raises(cap.CaptureError):
        cap.capture(
            [str(export)], "0x" + "55" * 20, since, end, str(out),
            5, 1, "5m", opener=opener, clock_ms=end + ONE_DAY)
    bundle = json.loads((out / "bundle.json").read_text())
    row = bundle["completeness"]["candleSnapshot"][0]
    assert row["complete"] is False
    assert row["reason"] == "snapshot_incomplete"


def test_resting_tp_full_page_of_one_timestamp_is_incomplete():
    import resting_tp_capture as cap

    page = [
        {"tid": 1, "time": 5000, "oid": 1, "sz": "1", "px": "1"},
        {"tid": 2, "time": 5000, "oid": 1, "sz": "1", "px": "1"},
    ]

    def opener(req, timeout=None):
        return _InfoResp(page)

    result = cap._page_user_fills(
        "0x" + "cd" * 20, 1000, 10000, 1, 1, lambda *args: None,
        opener=opener, page_max_rows=2)
    assert result["complete"] is False
    assert "timestamp" in result["failure"]
